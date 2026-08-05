package main

import (
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
)

// TestExpiredEnvelopeKeepsPrepaidReservationForInFlightWork is the refund-rail
// sibling of TestExpiredEnvelopeStillHoldsFundsForInFlightWork.
//
// The funding rail (evaluateRealtimeBuyerFunding) already falls back to holding
// EXECUTING contracts when their envelope is no longer ACTIVE. The refund rail
// (prepaidOpenReservationMicros) only held ACTIVE envelopes. The moment
// ReleaseExpiredExecutionEnvelopes flipped the envelope off ACTIVE, in-flight
// work stopped counting as committed prepaid cash, so an admin refund of
// "available" could drain the balance settlement still needs to debit.
//
// This drives the real store: create envelope → authorize to EXECUTING → expire
// via the production release path → assert the open-reservation query still
// holds the in-flight spend and that an admin refund cannot take that cash.
// Removing the EXECUTING-without-ACTIVE-envelope term fails this test.
func TestExpiredEnvelopeKeepsPrepaidReservationForInFlightWork(t *testing.T) {
	installSettlementCurrencyForTest(t, "usd")
	t.Setenv("MERC_TOKEN_KEY", "prepaid-envelope-expiry-key-with-at-least-32-bytes!")
	ctx, store, pool := openIsolatedTestStore(t)
	profile, _, _ := realtimeFundingFixture(t, ctx, store, pool)

	maxUSD, estUSD, maxPrompt, maxCompletion := realtimeAuthCeiling(t, profile, 7, 2)
	needNanos := usdToMicros(maxUSD) * NanosPerMicro
	needMicros := usdToMicros(maxUSD)
	// Fund more than one ceiling so that, if the hold drops to zero after
	// expiry, a refund of "available" has cash to drain. With the hold intact,
	// only the unreserved slack is refundable.
	seedMicros := needMicros * 3 / 2
	if seedMicros%10_000 != 0 {
		// Round up to a whole cent so top-up + refund paths accept the amount.
		seedMicros = ((seedMicros / 10_000) + 1) * 10_000
	}
	buyerID := uuid.New()
	if _, err := pool.Exec(ctx, `INSERT INTO buyers (id,email,free_credit_usd) VALUES ($1,$2,0)`,
		buyerID, buyerID.String()+"@prepaid-env-expiry.invalid"); err != nil {
		t.Fatal(err)
	}
	// Production-shaped top-up so BeginPrepaidRefund can trace slices to a
	// collected payment intent (SeedPrepaidBalance would leave refunds unbacked).
	fundPrepaidViaTopup(t, ctx, store, buyerID, seedMicros/10_000)

	env, err := store.CreateExecutionEnvelope(ctx, buyerID, ExecutionEnvelopeCreateRequest{
		RuntimeProfileID:       profile.RuntimeProfileID,
		CapNanos:               needNanos,
		MaxRequests:            5,
		PerRequestCeilingNanos: needNanos,
		TTLSeconds:             600,
	})
	must(t, err)

	contract, replay, err := store.AuthorizeRealtimeContract(ctx, RealtimeContractAuthorization{
		RequestID: "req-prepaid-inflight-" + uuid.NewString(), BuyerID: buyerID, Profile: profile,
		InputCommitment: stringsRepeat("a", 64), RequestSHA256: stringsRepeat("b", 64),
		MaximumPriceUSD: maxUSD, EstimatedPriceUSD: estUSD,
		DeadlineAt:              time.Now().Add(time.Minute),
		IdempotencyKey:          "prepaid-inflight-" + uuid.NewString(),
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

	// While the envelope is ACTIVE the hold is the envelope term; pin that the
	// in-flight ceiling is already committed before we exercise expiry.
	reservedBefore, err := prepaidOpenReservationMicros(ctx, pool, buyerID)
	must(t, err)
	if reservedBefore < needMicros {
		t.Fatalf("before expiry reserved=%d, want at least in-flight %d", reservedBefore, needMicros)
	}

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

	var spendState string
	if err := pool.QueryRow(ctx, `
		SELECT state FROM execution_envelope_spends WHERE contract_id=$1`,
		contract.ID).Scan(&spendState); err != nil {
		t.Fatal(err)
	}
	if spendState != "RESERVED" {
		t.Fatalf("spend bound to EXECUTING contract must stay RESERVED, got %s", spendState)
	}

	// The refund rail must still hold the in-flight spend after the envelope
	// left ACTIVE. Without the EXECUTING fallback term, reserved collapses to 0.
	reservedAfter, err := prepaidOpenReservationMicros(ctx, pool, buyerID)
	must(t, err)
	if reservedAfter < needMicros {
		t.Fatalf("after envelope expiry reserved=%d, want at least in-flight %d: "+
			"prepaidOpenReservationMicros dropped the hold for still-EXECUTING work",
			reservedAfter, needMicros)
	}

	available, err := store.BuyerPrepaidAvailableMicros(ctx, buyerID)
	must(t, err)
	// Slack above the in-flight ceiling may be refundable; the ceiling itself
	// must not be.
	if available > seedMicros-needMicros {
		t.Fatalf("available=%d exceeds seed(%d)-in-flight(%d)=%d: refund rail "+
			"is treating committed envelope spend as free",
			available, seedMicros, needMicros, seedMicros-needMicros)
	}

	actor := insertTestAdminActor(t, pool, ctx)
	plan, err := store.BeginPrepaidRefund(ctx, actor, buyerID,
		"attempt refund while envelope-funded work is still EXECUTING",
		adminTestRef("INC-prepaid-env-expiry"))
	bal, berr := store.BuyerPrepaidBalanceMicros(ctx, buyerID)
	if berr != nil {
		t.Fatal(berr)
	}
	if err != nil {
		// Refused entirely (nothing refundable, or only sub-cent dust left
		// unreserved). Either way the in-flight ceiling must still be on the
		// balance — that is the money the supplier settlement still needs.
		if bal < needMicros {
			t.Fatalf("balance after refused refund=%d, want at least in-flight %d (err=%v)",
				bal, needMicros, err)
		}
		if !errors.Is(err, errInsufficientPrepaid) &&
			!strings.Contains(err.Error(), "below one") {
			t.Fatalf("BeginPrepaidRefund unexpected error: %v", err)
		}
		return
	}
	// A partial refund of true slack is fine; draining past the in-flight hold
	// is the money-loss bug.
	if bal < needMicros {
		t.Fatalf("admin refund of available drained in-flight cash: balance=%d "+
			"refunded_cents=%d reserved_should_have_kept>=%d",
			bal, plan.Cents, needMicros)
	}
}
