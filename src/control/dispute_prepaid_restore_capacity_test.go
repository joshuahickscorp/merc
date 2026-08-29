package main

import (
	"errors"
	"testing"
)

// G071 — upheld dispute on prepaid-funded work must restore materialisation
// AND net the prepaid_debit in evaluateRealtimeBuyerFunding. Without a
// prepaid_restore row, buyer_refund zeros spent while prepaidDebited still
// adds D, so available becomes cash+D (phantom capacity). Sequential
// re-dispute then mints further capacity if the credit is not keyed to the
// debit.

func TestDisputeUpheldPrepaidRealtimeCapacityMatchesCash(t *testing.T) {
	installSettlementCurrencyForTest(t, "usd")
	ctx, store, pool := openIsolatedTestStore(t)
	t.Setenv("MERC_CANARY_MODE", "false")
	t.Setenv("MERC_CANARY_DISABLE_DECISION_REF", "test:dispute-prepaid-capacity")

	const seedMicros int64 = 5_000_000 // $5.00 pocket
	f := seedBuyerRefundFixture(t, ctx, pool, buyerRefundOpts{
		withPrepaidDebit: true,
		prepaidBalance:   seedMicros,
	})
	// After fixture: balance = seed − charge, debit D = charge on ledger.
	charge := f.chargeMicros
	wantCashAfter := seedMicros // dispute must restore the debit to cash

	var balBefore int64
	if err := pool.QueryRow(ctx,
		`SELECT balance_micros FROM buyer_prepaid_balances WHERE buyer_id=$1 AND currency='usd'`,
		f.buyerID).Scan(&balBefore); err != nil {
		t.Fatal(err)
	}
	if balBefore != seedMicros-charge {
		t.Fatalf("prepaid before dispute=%d, want seed−charge %d−%d", balBefore, seedMicros, charge)
	}

	disputeID, err := store.RecordDispute(ctx, f.jobID, f.buyerID, "prepaid dispute must net debit for capacity")
	mustf(t, err, "RecordDispute: %v")
	mustf(t, store.SetDisputeStatus(ctx, disputeID, "upheld"), "uphold: %v")

	balAfter, err := store.BuyerPrepaidBalanceMicros(ctx, f.buyerID)
	must(t, err)
	if balAfter != wantCashAfter {
		t.Fatalf("prepaid after uphold=%d, want cash %d (restored seed)", balAfter, wantCashAfter)
	}

	// Case 1: realtime capacity must equal cash, not cash+D.
	// need = cash admits; need = cash+1µ must refuse. With the phantom of +D,
	// cash+1µ would still admit when D ≥ 1µ (always here).
	tx, err := pool.Begin(ctx)
	must(t, err)
	errCash := evaluateRealtimeBuyerFunding(ctx, tx, f.buyerID, balAfter*1000)
	_ = tx.Rollback(ctx)
	if errCash != nil {
		t.Fatalf("funding at exact cash refused: balance=%d err=%v", balAfter, errCash)
	}

	tx, err = pool.Begin(ctx)
	must(t, err)
	errOver := evaluateRealtimeBuyerFunding(ctx, tx, f.buyerID, (balAfter+1)*1000)
	_ = tx.Rollback(ctx)
	if errOver == nil {
		t.Fatalf("funding admitted need=cash+1µ after prepaid dispute refund: "+
			"balance=%d (cash); phantom prepaidDebited add-back of charge=%d still counted — "+
			"dispute path credited balance without KindPrepaidRestore netting",
			balAfter, charge)
	}
	if !errors.Is(errOver, errRealtimeInsufficientFunds) &&
		!errors.Is(errOver, errRealtimeTopupRequired) {
		t.Fatalf("over-cash funding err=%v, want insufficient/topup", errOver)
	}
}

func TestDisputeSequentialReDisputeCannotMintCapacity(t *testing.T) {
	installSettlementCurrencyForTest(t, "usd")
	ctx, store, pool := openIsolatedTestStore(t)
	t.Setenv("MERC_CANARY_MODE", "false")
	t.Setenv("MERC_CANARY_DISABLE_DECISION_REF", "test:dispute-prepaid-rediispute")

	const seedMicros int64 = 5_000_000
	f := seedBuyerRefundFixture(t, ctx, pool, buyerRefundOpts{
		withPrepaidDebit: true,
		prepaidBalance:   seedMicros,
	})
	charge := f.chargeMicros

	d1, err := store.RecordDispute(ctx, f.jobID, f.buyerID, "first upheld prepaid dispute")
	mustf(t, err, "first RecordDispute: %v")
	mustf(t, store.SetDisputeStatus(ctx, d1, "upheld"), "first uphold: %v")

	bal1, err := store.BuyerPrepaidBalanceMicros(ctx, f.buyerID)
	must(t, err)
	if bal1 != seedMicros {
		t.Fatalf("after first uphold balance=%d, want seed %d", bal1, seedMicros)
	}

	// A second dispute can be filed after the first is terminal. Full resolve
	// then fails closed on risk-reserve causal refunds (buyer_refund rows are
	// already keyed to d1), but the funding arm still sees prepaid_debit +
	// buyer_refund and will re-credit unless restore is debit-keyed. Exercise
	// that funding re-entry under a fresh dispute id — that is the mint path.
	d2, err := store.RecordDispute(ctx, f.jobID, f.buyerID, "second dispute must not re-credit prepaid")
	mustf(t, err, "second RecordDispute: %v")

	tx, err := pool.Begin(ctx)
	must(t, err)
	_, err = applyDisputeBuyerRefundFundingTx(ctx, tx, f.jobID, d2, disputeBuyerRefundResult{
		BuyerRefundMicros: f.chargeMicros,
		Currency:          "usd",
	})
	mustf(t, err, "second dispute funding apply: %v")
	must(t, tx.Commit(ctx))

	bal2, err := store.BuyerPrepaidBalanceMicros(ctx, f.buyerID)
	must(t, err)
	if bal2 != bal1 {
		t.Fatalf("sequential re-dispute minted prepaid: %d → %d (extra credit of charge=%d)",
			bal1, bal2, charge)
	}
	if bal2 > seedMicros {
		t.Fatalf("prepaid above seed after re-dispute: balance=%d seed=%d", bal2, seedMicros)
	}

	// Capacity still equals cash — not cash + n*D.
	tx, err = pool.Begin(ctx)
	must(t, err)
	errOver := evaluateRealtimeBuyerFunding(ctx, tx, f.buyerID, (bal2+1)*1000)
	_ = tx.Rollback(ctx)
	if errOver == nil {
		t.Fatalf("after sequential re-dispute funding, admitted cash+1µ: balance=%d charge=%d — capacity minted",
			bal2, charge)
	}
	if !errors.Is(errOver, errRealtimeInsufficientFunds) &&
		!errors.Is(errOver, errRealtimeTopupRequired) {
		t.Fatalf("over-cash funding err=%v, want insufficient/topup", errOver)
	}
}

func TestDisputePrepaidRestoreIdempotent(t *testing.T) {
	installSettlementCurrencyForTest(t, "usd")
	ctx, store, pool := openIsolatedTestStore(t)
	t.Setenv("MERC_CANARY_MODE", "false")
	t.Setenv("MERC_CANARY_DISABLE_DECISION_REF", "test:dispute-prepaid-idemp")

	const seedMicros int64 = 2_500_000
	f := seedBuyerRefundFixture(t, ctx, pool, buyerRefundOpts{
		withPrepaidDebit: true,
		prepaidBalance:   seedMicros,
	})

	d1, err := store.RecordDispute(ctx, f.jobID, f.buyerID, "idempotent prepaid restore")
	mustf(t, err, "RecordDispute: %v")
	mustf(t, store.SetDisputeStatus(ctx, d1, "upheld"), "uphold: %v")

	bal1, err := store.BuyerPrepaidBalanceMicros(ctx, f.buyerID)
	must(t, err)
	if bal1 != seedMicros {
		t.Fatalf("after uphold balance=%d, want seed %d", bal1, seedMicros)
	}

	// Re-apply funding side-effect under a fresh transaction (same dispute id).
	// Restore receipts are keyed to the debit, not the dispute, so a second
	// apply must not credit again.
	tx, err := pool.Begin(ctx)
	must(t, err)
	_, err = applyDisputeBuyerRefundFundingTx(ctx, tx, f.jobID, d1, disputeBuyerRefundResult{
		BuyerRefundMicros: f.chargeMicros,
		Currency:          "usd",
	})
	mustf(t, err, "second applyDisputeBuyerRefundFundingTx: %v")
	must(t, tx.Commit(ctx))

	bal2, err := store.BuyerPrepaidBalanceMicros(ctx, f.buyerID)
	must(t, err)
	if bal2 != bal1 {
		t.Fatalf("double-apply credited prepaid: %d → %d", bal1, bal2)
	}

	// Same-dispute resolve is idempotent and must not move money.
	mustf(t, store.resolveDispute(ctx, d1, "upheld"), "re-resolve: %v")
	bal3, err := store.BuyerPrepaidBalanceMicros(ctx, f.buyerID)
	must(t, err)
	if bal3 != bal1 {
		t.Fatalf("re-resolve moved prepaid: %d → %d", bal1, bal3)
	}
}

// TestDisputePrepaidRestoreDoesNotBreakRealtimeOrSLAArms is the regression pin
// that paths already correct still restore materialisation without inventing
// capacity. Realtime arm nets; SLA arm credits balance only (spent still holds
// the premium charge).
func TestDisputePrepaidRestoreDoesNotBreakRealtimeOrSLAArms(t *testing.T) {
	// Delegate to the dedicated pins — if either regresses under the structural
	// formula change, this package-level run fails closed.
	t.Run("realtime", TestRealtimeRefundRestoresPrepaidMaterialisation)
	t.Run("realtimeIdemp", TestRealtimeRefundIdempotentPrepaidRestore)
	t.Run("sla", TestSLAMissRestoresPrepaidPremiumMaterialisation)
}
