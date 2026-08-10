package main

import (
	"errors"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
)

// G070 — internal realtime refund must restore materialised prepaid that
// settle debited. Without restore, buyer_refund zeros spent while prepaidDebited
// still adds D back, so available becomes free+B while balance is only B−D
// (phantom capacity of exactly D). A second authorize then admits; settle hits
// errInsufficientPrepaid after delivery.
//
// Restoring materialisation re-pairs charge and debit: balance returns to B and
// a compensating prepaid_restore nets prepaidDebited so capacity matches cash.

// TestRealtimeRefundRestoresPrepaidMaterialisation pins cases 1 and 2:
// prepaid settle → RefundRealtimeContract → no phantom capacity; second apply
// does not double-credit.
func TestRealtimeRefundRestoresPrepaidMaterialisation(t *testing.T) {
	installSettlementCurrencyForTest(t, "usd")
	t.Setenv("MERC_TOKEN_KEY", "rt-refund-mat-key-with-at-least-32-bytes!!!!!")
	ctx, store, pool := openIsolatedTestStore(t)
	profile, _, _ := realtimeFundingFixture(t, ctx, store, pool)

	// Large enough ceiling that settle charge D is a meaningful positive debit.
	maxUSD, estUSD, maxPrompt, maxCompletion := realtimeAuthCeiling(t, profile, 100_000, 50_000)
	needMicros := usdToMicros(maxUSD)
	if needMicros <= 0 {
		t.Fatalf("ceiling micros=%d", needMicros)
	}
	// Seed exactly one ceiling so after settle (B−D) the pocket cannot fund
	// another full ceiling without restore (or phantom).
	seedMicros := needMicros
	if seedMicros%10_000 != 0 {
		seedMicros = ((seedMicros / 10_000) + 1) * 10_000
	}

	buyerID := uuid.New()
	if _, err := pool.Exec(ctx, `INSERT INTO buyers (id,email,free_credit_usd) VALUES ($1,$2,0)`,
		buyerID, buyerID.String()+"@rt-refund-mat.invalid"); err != nil {
		t.Fatal(err)
	}
	must(t, store.SeedPrepaidBalance(ctx, buyerID, seedMicros, "seed-rt-refund-mat-"+buyerID.String()))

	contract, replay, err := store.AuthorizeRealtimeContract(ctx, RealtimeContractAuthorization{
		RequestID: "req-rt-refund-mat-" + uuid.NewString(), BuyerID: buyerID, Profile: profile,
		InputCommitment: strings.Repeat("a", 64), RequestSHA256: strings.Repeat("b", 64),
		MaximumPriceUSD: maxUSD, EstimatedPriceUSD: estUSD, DeadlineAt: time.Now().Add(time.Minute),
		MaximumPromptTokens: maxPrompt, MaximumCompletionTokens: maxCompletion,
		EstimatedPromptTokens: 100_000, EstimatedCompletionTokens: 50_000,
	})
	if err != nil || replay {
		t.Fatalf("authorize: err=%v replay=%v", err, replay)
	}

	settlement, err := store.FinalizeRealtimeSuccess(ctx, contract.ID, RealtimeExecutionEvidence{
		ID: uuid.New(), HTTPStatus: http.StatusOK, StreamRootSHA256: strings.Repeat("1", 64),
		OutputCommitment: strings.Repeat("2", 64),
		PromptTokens:     100_000, CompletionTokens: 50_000, TotalTokens: 150_000,
	})
	mustf(t, err, "finalize: %v")
	chargeMicros := usdToMicros(settlement.BuyerChargeUSD)
	if chargeMicros <= 0 {
		t.Fatalf("settled charge micros=%d, want positive prepaid debit", chargeMicros)
	}
	balAfterSettle, err := store.BuyerPrepaidBalanceMicros(ctx, buyerID)
	must(t, err)
	if balAfterSettle != seedMicros-chargeMicros {
		t.Fatalf("balance after settle=%d, want %d−%d", balAfterSettle, seedMicros, chargeMicros)
	}
	// Residual after settle cannot fund another full ceiling.
	if balAfterSettle >= needMicros {
		t.Fatalf("precondition: residual %d still covers ceiling %d — raise charge or shrink seed",
			balAfterSettle, needMicros)
	}

	actor := insertTestAdminActor(t, pool, ctx)
	corr := adminTestRef("INC-rt-refund-mat")
	refund, created, err := store.RefundRealtimeContract(ctx, actor, contract.ID,
		"platform fault: restore prepaid materialisation", corr)
	if err != nil || !created {
		t.Fatalf("RefundRealtimeContract: err=%v created=%v refund=%+v", err, created, refund)
	}

	balAfterRefund, err := store.BuyerPrepaidBalanceMicros(ctx, buyerID)
	must(t, err)
	if balAfterRefund != seedMicros {
		t.Fatalf("prepaid after internal refund=%d, want restored seed %d (settle debited %d) — "+
			"RefundRealtimeContract wrote ledger reversals only and left materialised prepaid reduced",
			balAfterRefund, seedMicros, chargeMicros)
	}

	// Case 1: second authorize must not ride phantom capacity. With restore,
	// balance covers the ceiling for real; settle must then debit cleanly.
	// Without restore, balance is B−D < need but the funding formula still
	// admits — that is the P0 defect class.
	contract2, replay2, authErr := store.AuthorizeRealtimeContract(ctx, RealtimeContractAuthorization{
		RequestID: "req-rt-refund-mat-2-" + uuid.NewString(), BuyerID: buyerID, Profile: profile,
		InputCommitment: strings.Repeat("c", 64), RequestSHA256: strings.Repeat("d", 64),
		MaximumPriceUSD: maxUSD, EstimatedPriceUSD: estUSD, DeadlineAt: time.Now().Add(time.Minute),
		MaximumPromptTokens: maxPrompt, MaximumCompletionTokens: maxCompletion,
		EstimatedPromptTokens: 100_000, EstimatedCompletionTokens: 50_000,
	})
	if balAfterRefund < needMicros {
		// Capacity no longer exists in the pocket — admit must refuse.
		if authErr == nil {
			t.Fatalf("second authorize admitted on capacity that no longer exists: "+
				"balance=%d need=%d (phantom of prepaid_debit %d after refund without restore)",
				balAfterRefund, needMicros, chargeMicros)
		}
		if !errors.Is(authErr, errRealtimeInsufficientFunds) &&
			!errors.Is(authErr, errRealtimeTopupRequired) {
			t.Fatalf("second authorize err=%v, want insufficient/topup when balance < ceiling", authErr)
		}
	} else {
		// Materialisation restored — admit is real; settle must not hit
		// errInsufficientPrepaid after delivery.
		if authErr != nil || replay2 {
			t.Fatalf("second authorize after restore: err=%v replay=%v", authErr, replay2)
		}
		_, settleErr := store.FinalizeRealtimeSuccess(ctx, contract2.ID, RealtimeExecutionEvidence{
			ID: uuid.New(), HTTPStatus: http.StatusOK, StreamRootSHA256: strings.Repeat("3", 64),
			OutputCommitment: strings.Repeat("4", 64),
			PromptTokens:     7, CompletionTokens: 2, TotalTokens: 9,
		})
		if settleErr != nil {
			t.Fatalf("settle after restored refund hit %v — capacity was phantom or restore incomplete", settleErr)
		}
	}

	// Case 2: idempotent restore — replaying the same correlation must not
	// credit prepaid twice; a second distinct refund must refuse.
	_, created2, err2 := store.RefundRealtimeContract(ctx, actor, contract.ID,
		"platform fault: restore prepaid materialisation", corr)
	if err2 != nil {
		t.Fatalf("replay RefundRealtimeContract: %v", err2)
	}
	if created2 {
		t.Fatal("replay reported created=true; expected idempotent resume")
	}
	balAfterReplay, err := store.BuyerPrepaidBalanceMicros(ctx, buyerID)
	must(t, err)
	// After second-contract settle above, balance may be seed−charge2; without
	// that settle path, balance stays seed. Either way, replay must not add
	// another +chargeMicros on top of the first restore.
	// Capture balance before the distinct second refund attempt.
	balBeforeSecond := balAfterReplay

	_, _, err3 := store.RefundRealtimeContract(ctx, actor, contract.ID,
		"second distinct correlation must not re-restore", adminTestRef("INC-rt-refund-mat-2"))
	if !errors.Is(err3, errRealtimeNotRefundable) {
		t.Fatalf("second distinct refund err=%v, want errRealtimeNotRefundable", err3)
	}
	balFinal, err := store.BuyerPrepaidBalanceMicros(ctx, buyerID)
	must(t, err)
	if balFinal != balBeforeSecond {
		t.Fatalf("second refund path moved prepaid: %d → %d (double-credit of restore)",
			balBeforeSecond, balFinal)
	}
	// Explicit: never more than one restore credit of the original debit.
	// After optional second settle, balance ≤ seed; double-restore would push
	// balance above seed (or above seed−secondCharge).
	if balFinal > seedMicros {
		t.Fatalf("prepaid double-credited: balance=%d > seed=%d", balFinal, seedMicros)
	}
}

// TestRealtimeRefundIdempotentPrepaidRestore is the narrow double-apply pin:
// applying the refund transaction's restore twice must not credit twice.
// Uses the same correlation replay plus a forced second look at balance.
func TestRealtimeRefundIdempotentPrepaidRestore(t *testing.T) {
	installSettlementCurrencyForTest(t, "usd")
	t.Setenv("MERC_TOKEN_KEY", "rt-refund-idemp-key-with-at-least-32-bytes!!")
	ctx, store, pool := openIsolatedTestStore(t)
	profile, _, _ := realtimeFundingFixture(t, ctx, store, pool)

	maxUSD, estUSD, maxPrompt, maxCompletion := realtimeAuthCeiling(t, profile, 7, 2)
	buyerID := uuid.New()
	if _, err := pool.Exec(ctx, `INSERT INTO buyers (id,email,free_credit_usd) VALUES ($1,$2,0)`,
		buyerID, buyerID.String()+"@rt-refund-idemp.invalid"); err != nil {
		t.Fatal(err)
	}
	const seedMicros int64 = 5_000_000
	must(t, store.SeedPrepaidBalance(ctx, buyerID, seedMicros, "seed-rt-refund-idemp-"+buyerID.String()))

	contract, _, err := store.AuthorizeRealtimeContract(ctx, RealtimeContractAuthorization{
		RequestID: "req-rt-refund-idemp-" + uuid.NewString(), BuyerID: buyerID, Profile: profile,
		InputCommitment: strings.Repeat("e", 64), RequestSHA256: strings.Repeat("f", 64),
		MaximumPriceUSD: maxUSD, EstimatedPriceUSD: estUSD, DeadlineAt: time.Now().Add(time.Minute),
		MaximumPromptTokens: maxPrompt, MaximumCompletionTokens: maxCompletion,
		EstimatedPromptTokens: 7, EstimatedCompletionTokens: 2,
	})
	mustf(t, err, "authorize: %v")
	settlement, err := store.FinalizeRealtimeSuccess(ctx, contract.ID, RealtimeExecutionEvidence{
		ID: uuid.New(), HTTPStatus: http.StatusOK, StreamRootSHA256: strings.Repeat("5", 64),
		OutputCommitment: strings.Repeat("6", 64), PromptTokens: 7, CompletionTokens: 2, TotalTokens: 9,
	})
	mustf(t, err, "finalize: %v")
	chargeMicros := usdToMicros(settlement.BuyerChargeUSD)
	if chargeMicros <= 0 {
		t.Fatalf("charge micros=%d", chargeMicros)
	}

	actor := insertTestAdminActor(t, pool, ctx)
	corr := adminTestRef("INC-rt-refund-idemp")
	if _, _, err := store.RefundRealtimeContract(ctx, actor, contract.ID,
		"idempotent restore", corr); err != nil {
		t.Fatalf("first refund: %v", err)
	}
	bal1, err := store.BuyerPrepaidBalanceMicros(ctx, buyerID)
	must(t, err)
	if bal1 != seedMicros {
		t.Fatalf("after first refund balance=%d, want seed %d (restore of %d)", bal1, seedMicros, chargeMicros)
	}

	// Replay same correlation — must not add another restore credit.
	if _, created, err := store.RefundRealtimeContract(ctx, actor, contract.ID,
		"idempotent restore", corr); err != nil || created {
		t.Fatalf("replay: err=%v created=%v", err, created)
	}
	bal2, err := store.BuyerPrepaidBalanceMicros(ctx, buyerID)
	must(t, err)
	if bal2 != bal1 {
		t.Fatalf("replay double-credited prepaid: %d → %d", bal1, bal2)
	}

	// Distinct correlation refused; balance unchanged.
	if _, _, err := store.RefundRealtimeContract(ctx, actor, contract.ID,
		"second attempt", adminTestRef("INC-rt-refund-idemp-b")); !errors.Is(err, errRealtimeNotRefundable) {
		t.Fatalf("second refund err=%v, want not refundable", err)
	}
	bal3, err := store.BuyerPrepaidBalanceMicros(ctx, buyerID)
	must(t, err)
	if bal3 != bal1 {
		t.Fatalf("refused second refund still moved balance: %d → %d", bal1, bal3)
	}
}
