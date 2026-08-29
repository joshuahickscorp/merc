package main

import (
	"math"
	"testing"
	"time"

	"github.com/google/uuid"
)

// TestBuyerFreeCreditRemainingHoldsActiveEnvelope pins the third committed-money
// rail to the same ACTIVE-envelope residual the funding and prepaid rails use.
//
// BuyerFreeCreditRemaining is consulted by job intake when no payment method is
// on file: a residual that still looks free here becomes MaxUSD and admits work
// against cash already reserved on an envelope. Before this term, a buyer with
// free credit fully committed to an ACTIVE envelope still reported the full
// grant as remaining.
//
// Shape under test:
//  1. ACTIVE envelope residual is held (cap − spent).
//  2. EXECUTING under that ACTIVE envelope is not double-counted.
//  3. After expiry, still-EXECUTING work falls back to the contract term.
func TestBuyerFreeCreditRemainingHoldsActiveEnvelope(t *testing.T) {
	installSettlementCurrencyForTest(t, "usd")
	t.Setenv("MERC_TOKEN_KEY", "free-credit-envelope-rail-key-with-at-least-32-bytes!")
	ctx, store, pool := openIsolatedTestStore(t)
	profile, _, _ := realtimeFundingFixture(t, ctx, store, pool)

	const freeCreditUSD = 5.00
	buyerID := uuid.New()
	if _, err := pool.Exec(ctx,
		`INSERT INTO buyers (id,email,free_credit_usd) VALUES ($1,$2,$3)`,
		buyerID, buyerID.String()+"@free-credit-env.invalid", freeCreditUSD); err != nil {
		t.Fatal(err)
	}

	before, err := store.BuyerFreeCreditRemaining(ctx, buyerID)
	must(t, err)
	if before != freeCreditUSD {
		t.Fatalf("seed free credit remaining=%v, want %v", before, freeCreditUSD)
	}

	// Commit most of the grant to an envelope. $4.00 leaves $1.00 free if the
	// residual term is present; without it remaining stays $5.00.
	const capUSD = 4.00
	capNanos := int64(capUSD * 1e9)
	env, err := store.CreateExecutionEnvelope(ctx, buyerID, ExecutionEnvelopeCreateRequest{
		RuntimeProfileID:       profile.RuntimeProfileID,
		CapNanos:               capNanos,
		MaxRequests:            5,
		PerRequestCeilingNanos: capNanos,
		TTLSeconds:             600,
	})
	mustf(t, err, "create envelope: %v")

	afterCreate, err := store.BuyerFreeCreditRemaining(ctx, buyerID)
	must(t, err)
	wantAfterCreate := freeCreditUSD - capUSD
	if math.Abs(afterCreate-wantAfterCreate) > 1e-9 {
		t.Fatalf("after ACTIVE envelope create remaining=%v, want %v: "+
			"BuyerFreeCreditRemaining does not hold ACTIVE envelope residual",
			afterCreate, wantAfterCreate)
	}

	maxUSD, estUSD, maxPrompt, maxCompletion := realtimeAuthCeiling(t, profile, 7, 2)
	// Ceiling must fit inside the envelope and leave residual for the hold math.
	if maxUSD <= 0 || maxUSD > capUSD {
		t.Fatalf("auth ceiling %v not usable against cap %v", maxUSD, capUSD)
	}

	contract, replay, err := store.AuthorizeRealtimeContract(ctx, RealtimeContractAuthorization{
		RequestID: "req-free-credit-env-" + uuid.NewString(), BuyerID: buyerID, Profile: profile,
		InputCommitment: stringsRepeat("a", 64), RequestSHA256: stringsRepeat("b", 64),
		MaximumPriceUSD: maxUSD, EstimatedPriceUSD: estUSD,
		DeadlineAt:              time.Now().Add(time.Minute),
		IdempotencyKey:          "free-credit-env-" + uuid.NewString(),
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

	// While the envelope is ACTIVE the residual term already covers this
	// in-flight ceiling. Unconditional EXECUTING would subtract maxUSD again
	// and under-report free credit (or floor at zero incorrectly).
	afterAuth, err := store.BuyerFreeCreditRemaining(ctx, buyerID)
	must(t, err)
	// spent_nanos is still 0 until finalize; residual hold stays full cap.
	if math.Abs(afterAuth-wantAfterCreate) > 1e-9 {
		t.Fatalf("after envelope-funded EXECUTING remaining=%v, want %v (no double-hold): got delta would mean EXECUTING is counted on top of ACTIVE residual",
			afterAuth, wantAfterCreate)
	}

	// Expire the envelope while the contract is still EXECUTING. Residual term
	// drops; the conditional EXECUTING term must pick the ceiling up.
	if _, err := pool.Exec(ctx, `
		UPDATE execution_envelopes SET expires_at=now()-interval '1 second' WHERE id=$1`,
		env.ID); err != nil {
		t.Fatal(err)
	}
	if n, err := store.ReleaseExpiredExecutionEnvelopes(ctx, 100); err != nil || n != 1 {
		t.Fatalf("expiry release n=%d err=%v", n, err)
	}

	afterExpiry, err := store.BuyerFreeCreditRemaining(ctx, buyerID)
	must(t, err)
	wantAfterExpiry := freeCreditUSD - maxUSD
	if math.Abs(afterExpiry-wantAfterExpiry) > 1e-6 {
		t.Fatalf("after envelope expiry with EXECUTING still live remaining=%v, want %v: "+
			"in-flight ceiling was not held by the EXECUTING fallback",
			afterExpiry, wantAfterExpiry)
	}
}
