package main

import (
	"testing"
	"time"

	"github.com/google/uuid"
)

// TestExpiredEnvelopeStillHoldsFundsForInFlightWork pins the invariant
// ReleaseExpiredExecutionEnvelopes states in its own doc comment: "In-flight
// RESERVED spends are left alone: they still bind funds until the bound
// contract settles or is recovered."
//
// The funding query held that money in two mutually exclusive places. An
// envelope-funded EXECUTING contract was excluded from the ordinary realtime
// reservation sum, to avoid double-holding, while the envelope's own hold
// summed only ACTIVE envelopes. So the instant expiry flipped an envelope to
// EXPIRED, a contract still executing against it was held by NEITHER term: the
// buyer's available funds jumped by the full in-flight amount while a supplier
// was still doing the work, and the next admission could be granted against
// money already committed.
//
// Orphan recovery does not cover this. It voids spends whose contracts are
// already terminal, or that never bound a contract; a spend bound to a live
// EXECUTING contract is deliberately left alone — the work is real — and so
// the hold is never restored.
//
// The fix makes the exclusion conditional on the envelope still being ACTIVE,
// so an expired envelope's in-flight spend falls back to being held as an
// ordinary realtime reservation. Removing that condition fails this test.
func TestExpiredEnvelopeStillHoldsFundsForInFlightWork(t *testing.T) {
	installSettlementCurrencyForTest(t, "usd")
	t.Setenv("MERC_TOKEN_KEY", "envelope-expiry-hold-key-with-at-least-32-bytes!")
	ctx, store, pool := openIsolatedTestStore(t)
	profile, _, _ := realtimeFundingFixture(t, ctx, store, pool)

	maxUSD, estUSD, maxPrompt, maxCompletion := realtimeAuthCeiling(t, profile, 7, 2)
	needNanos := usdToMicros(maxUSD) * NanosPerMicro

	// Fund the buyer for one such request and a little over — not two. That
	// makes "is the in-flight amount still held?" the only thing deciding
	// whether a second admission succeeds.
	buyerID := envelopeTestBuyer(t, ctx, store, pool, usdToMicros(maxUSD)*3/2)

	env, err := store.CreateExecutionEnvelope(ctx, buyerID, ExecutionEnvelopeCreateRequest{
		RuntimeProfileID:       profile.RuntimeProfileID,
		CapNanos:               needNanos,
		MaxRequests:            5,
		PerRequestCeilingNanos: needNanos,
		TTLSeconds:             600,
	})
	must(t, err)

	// Real in-flight work through the production path: this binds the spend to
	// an EXECUTING contract, which is the case orphan recovery will not touch.
	contract, replay, err := store.AuthorizeRealtimeContract(ctx, RealtimeContractAuthorization{
		RequestID: "req-inflight-" + uuid.NewString(), BuyerID: buyerID, Profile: profile,
		InputCommitment: stringsRepeat("a", 64), RequestSHA256: stringsRepeat("b", 64),
		MaximumPriceUSD: maxUSD, EstimatedPriceUSD: estUSD,
		DeadlineAt:              time.Now().Add(time.Minute),
		IdempotencyKey:          "inflight-" + uuid.NewString(),
		MaximumPromptTokens:     maxPrompt,
		MaximumCompletionTokens: maxCompletion,
		EstimatedPromptTokens:   7, EstimatedCompletionTokens: 2,
		EnvelopeID: env.ID,
	})
	if err != nil || replay {
		t.Fatalf("authorize via envelope: err=%v replay=%v", err, replay)
	}
	if contract.State != "EXECUTING" {
		t.Fatalf("contract state=%s, want EXECUTING", contract.State)
	}

	// Expire the envelope while that contract is still executing.
	if _, err := pool.Exec(ctx, `
		UPDATE execution_envelopes SET expires_at=now()-interval '1 second' WHERE id=$1`,
		env.ID); err != nil {
		t.Fatal(err)
	}
	if n, err := store.ReleaseExpiredExecutionEnvelopes(ctx, 100); err != nil || n != 1 {
		t.Fatalf("expiry release n=%d err=%v", n, err)
	}
	if _, err := store.RecoverOrphanEnvelopeSpends(ctx, 0, 100); err != nil {
		t.Fatal(err)
	}

	// Recovery must not void a hold for work that is still running.
	var spendState string
	if err := pool.QueryRow(ctx, `
		SELECT state FROM execution_envelope_spends WHERE contract_id=$1`,
		contract.ID).Scan(&spendState); err != nil {
		t.Fatal(err)
	}
	if spendState != "RESERVED" {
		t.Fatalf("spend bound to an EXECUTING contract must stay RESERVED, got %s", spendState)
	}

	// The money is still committed, so a second identical request must be
	// refused. Before the fix the funding query saw that money as free and
	// admitted it, committing the same funds twice.
	_, _, err = store.AuthorizeRealtimeContract(ctx, RealtimeContractAuthorization{
		RequestID: "req-second-" + uuid.NewString(), BuyerID: buyerID, Profile: profile,
		InputCommitment: stringsRepeat("c", 64), RequestSHA256: stringsRepeat("d", 64),
		MaximumPriceUSD: maxUSD, EstimatedPriceUSD: estUSD,
		DeadlineAt:              time.Now().Add(time.Minute),
		IdempotencyKey:          "second-" + uuid.NewString(),
		MaximumPromptTokens:     maxPrompt,
		MaximumCompletionTokens: maxCompletion,
		EstimatedPromptTokens:   7, EstimatedCompletionTokens: 2,
	})
	if err == nil {
		t.Fatal("a second request was admitted while the first is still EXECUTING " +
			"against an expired envelope: the buyer has committed the same money " +
			"twice, and one of the two suppliers is owed against funds that are " +
			"already promised")
	}
}
