package main

import (
	"errors"
	"strings"
	"testing"

	"github.com/google/uuid"
)

func TestPrepaidBalancesRemainCurrencyKeyedAcrossCutoverAndHistoricalWebhook(t *testing.T) {
	installSettlementCurrencyForTest(t, "usd")
	ctx, store, pool := openIsolatedTestStore(t)
	buyerID := insertTestBuyer(t, pool, ctx)

	credit := func(key, currency string, minor int64) {
		t.Helper()
		must(t, store.CreditPrepaidTopup(ctx, key, buyerID, ChargeResult{
			PaymentIntentID: "pi_" + currency + "_" + uuid.NewString(),
			ChargeID:        "ch_" + currency + "_" + uuid.NewString(),
			RequestedCents:  minor,
			ReceivedCents:   minor,
			Currency:        currency,
		}))
	}

	usdSettled := "prepaid-usd-settled-" + uuid.NewString()
	_, err := store.BeginPrepaidTopup(ctx, usdSettled, buyerID, 100)
	must(t, err)
	credit(usdSettled, "usd", 100)

	// This operation crossed the durable USD boundary before the deployment
	// changed currency, but its webhook arrives afterwards.
	usdLate := "prepaid-usd-late-" + uuid.NewString()
	_, err = store.BeginPrepaidTopup(ctx, usdLate, buyerID, 100)
	must(t, err)

	installSettlementCurrencyForTest(t, "cad")
	current, err := store.BuyerPrepaidBalanceMicros(ctx, buyerID)
	must(t, err)
	if current != 0 {
		t.Fatalf("CAD balance inherited USD numerics: got %d, want 0", current)
	}
	credit(usdLate, "usd", 100)
	if _, err := store.BeginPrepaidTopup(ctx, usdSettled, buyerID, 100); !errors.Is(err, errPrepaidTopupConflict) {
		t.Fatalf("cross-currency idempotency reuse err=%v, want conflict", err)
	}

	cadKey := "prepaid-cad-" + uuid.NewString()
	_, err = store.BeginPrepaidTopup(ctx, cadKey, buyerID, 300)
	must(t, err)
	credit(cadKey, "cad", 300)

	usd, err := store.buyerPrepaidBalanceMicrosInCurrency(ctx, buyerID, "usd")
	must(t, err)
	cad, err := store.buyerPrepaidBalanceMicrosInCurrency(ctx, buyerID, "cad")
	must(t, err)
	if usd != 2_000_000 || cad != 3_000_000 {
		t.Fatalf("currency buckets usd=%d cad=%d; want 2000000/3000000", usd, cad)
	}
	var rows int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM buyer_prepaid_balances WHERE buyer_id=$1`, buyerID).Scan(&rows); err != nil {
		t.Fatal(err)
	}
	if rows != 2 {
		t.Fatalf("balance buckets=%d, want 2", rows)
	}
	var usdLedger, cadLedger int64
	if err := pool.QueryRow(ctx, `
		SELECT COALESCE(SUM((amount_usd*1000000)::bigint) FILTER (WHERE currency='usd'),0),
		       COALESCE(SUM((amount_usd*1000000)::bigint) FILTER (WHERE currency='cad'),0)
		  FROM ledger_entries WHERE buyer_id=$1 AND kind='prepaid_topup'`, buyerID).
		Scan(&usdLedger, &cadLedger); err != nil {
		t.Fatal(err)
	}
	if usdLedger != usd || cadLedger != cad {
		t.Fatalf("ledger buckets usd=%d cad=%d; balance usd=%d cad=%d", usdLedger, cadLedger, usd, cad)
	}
	if _, err := pool.Exec(ctx, `UPDATE prepaid_topup_operations SET currency='jpy' WHERE operation_key=$1`, cadKey); err == nil {
		t.Fatal("direct SQL relabelled a collected CAD top-up as JPY")
	}

	actor := insertTestAdminActor(t, pool, ctx)
	ref := "currency-cutover-" + uuid.NewString()
	first, err := store.BeginPrepaidRefund(ctx, actor, buyerID, "return CAD bucket", ref)
	must(t, err)
	if first.Currency != "cad" || first.Cents != 300 || first.Replayed {
		t.Fatalf("first CAD refund=%+v", first)
	}
	installSettlementCurrencyForTest(t, "usd")
	replay, err := store.BeginPrepaidRefund(ctx, actor, buyerID, "return CAD bucket", ref)
	must(t, err)
	if replay.Currency != "cad" || replay.Cents != 300 || !replay.Replayed {
		t.Fatalf("post-cutover refund replay=%+v, want frozen CAD", replay)
	}
	remainingUSD, err := store.BuyerPrepaidBalanceMicros(ctx, buyerID)
	must(t, err)
	if remainingUSD != 2_000_000 {
		t.Fatalf("CAD refund touched USD bucket: got %d, want 2000000", remainingUSD)
	}
}

func TestFreeCreditUSDDoesNotFundCADRealtime(t *testing.T) {
	installSettlementCurrencyForTest(t, "cad")
	ctx, store, pool := openIsolatedTestStore(t)
	buyerID := uuid.New()
	_, err := pool.Exec(ctx, `INSERT INTO buyers (id,email,free_credit_usd) VALUES ($1,$2,100)`, buyerID, buyerID.String()+"@cad.invalid")
	must(t, err)

	remaining, err := store.BuyerFreeCreditRemaining(ctx, buyerID)
	must(t, err)
	if remaining != 0 {
		t.Fatalf("CAD free-credit projection=%v, want 0 for USD-only grant", remaining)
	}
	tx, err := pool.Begin(ctx)
	must(t, err)
	err = evaluateRealtimeBuyerFunding(ctx, tx, buyerID, 1)
	_ = tx.Rollback(ctx)
	// Still a refusal, but a buyer holding 100 USD of credit under CAD settlement
	// is not empty, and telling them to top up would send them to buy money they
	// already hold. The refusal names the mismatch and stays distinct from the
	// generic empty-balance case.
	if !errors.Is(err, errRealtimeFreeCreditCurrencyMismatch) {
		t.Fatalf("CAD realtime funding from free_credit_usd err=%v, want free-credit currency mismatch", err)
	}
	if errors.Is(err, errRealtimeInsufficientFunds) || errors.Is(err, errRealtimeTopupRequired) {
		t.Fatalf("currency mismatch collapsed into an empty-balance or top-up refusal: %v", err)
	}
	if !strings.Contains(err.Error(), "cad") || !strings.Contains(err.Error(), "usd") {
		t.Fatalf("refusal does not name both currencies: %v", err)
	}

	must(t, store.SeedPrepaidBalance(ctx, buyerID, 2_000_000, "cad-cash-"+uuid.NewString()))
	tx, err = pool.Begin(ctx)
	must(t, err)
	err = evaluateRealtimeBuyerFunding(ctx, tx, buyerID, 1)
	_ = tx.Rollback(ctx)
	if err != nil {
		t.Fatalf("collected CAD prepaid cash did not fund CAD realtime request: %v", err)
	}
}

func TestLegacyPrepaidBalanceMigrationFreezesUSDMeaning(t *testing.T) {
	ctx, store, pool := openIsolatedTestStore(t)
	buyerID := uuid.New()
	_, err := pool.Exec(ctx, `INSERT INTO buyers (id,email) VALUES ($1,$2)`, buyerID, buyerID.String()+"@legacy.invalid")
	must(t, err)
	_, err = pool.Exec(ctx, `DROP TABLE buyer_prepaid_balances`)
	must(t, err)
	_, err = pool.Exec(ctx, `
		CREATE TABLE buyer_prepaid_balances (
		  buyer_id UUID PRIMARY KEY REFERENCES buyers(id) ON DELETE RESTRICT,
		  balance_micros BIGINT NOT NULL DEFAULT 0 CHECK (balance_micros >= 0),
		  updated_at TIMESTAMPTZ NOT NULL DEFAULT now()
		)`)
	must(t, err)
	_, err = pool.Exec(ctx, `INSERT INTO buyer_prepaid_balances (buyer_id,balance_micros) VALUES ($1,1234567)`, buyerID)
	must(t, err)
	must(t, store.Migrate(ctx))

	var currency string
	var amount int64
	if err := pool.QueryRow(ctx, `SELECT currency,balance_micros FROM buyer_prepaid_balances WHERE buyer_id=$1`, buyerID).
		Scan(&currency, &amount); err != nil {
		t.Fatal(err)
	}
	if currency != "usd" || amount != 1_234_567 {
		t.Fatalf("legacy balance migrated as %s/%d, want usd/1234567", currency, amount)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO buyer_prepaid_balances (buyer_id,currency,balance_micros) VALUES ($1,'cad',9)`, buyerID); err != nil {
		t.Fatalf("composite currency key did not admit separate CAD liability: %v", err)
	}
}

func TestHistoricalDisputeRestoreReturnsCashToAcceptedCurrencyBucket(t *testing.T) {
	installSettlementCurrencyForTest(t, "cad")
	ctx, store, pool := openIsolatedTestStore(t)
	t.Setenv("MERC_CANARY_MODE", "false")
	t.Setenv("MERC_CANARY_DISABLE_DECISION_REF", "test:historical-prepaid-currency")
	fixture := seedBuyerRefundFixture(t, ctx, pool, buyerRefundOpts{
		withPrepaidDebit: true,
		prepaidBalance:   1_250_000,
		currency:         "cad",
	})

	// The accepted CAD job is disputed only after a USD deployment replaces the
	// process view. Refund and materialised liability must remain CAD.
	installSettlementCurrencyForTest(t, "usd")
	disputeID, err := store.RecordDispute(ctx, fixture.jobID, fixture.buyerID, "currency-bound restore")
	must(t, err)
	must(t, store.SetDisputeStatus(ctx, disputeID, "upheld"))

	cad, err := store.buyerPrepaidBalanceMicrosInCurrency(ctx, fixture.buyerID, "cad")
	must(t, err)
	usd, err := store.buyerPrepaidBalanceMicrosInCurrency(ctx, fixture.buyerID, "usd")
	must(t, err)
	if cad != fixture.chargeMicros || usd != 0 {
		t.Fatalf("historical restore cad=%d usd=%d, want %d/0", cad, usd, fixture.chargeMicros)
	}
}
