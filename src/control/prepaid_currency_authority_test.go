package main

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
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
	latePayload, err := json.Marshal(map[string]any{
		"object": "payment_intent", "id": "pi_usd_late", "latest_charge": map[string]any{
			"object": "charge", "id": "ch_usd_late",
		}, "status": "succeeded", "amount": 100, "amount_received": 100,
		"currency": "usd", "metadata": map[string]string{"cx_operation_key": usdLate},
	})
	must(t, err)
	parsedKey, charge, owned, err := parseStripeSucceededPaymentIntent(latePayload)
	mustf(t, err, "parse historical USD payment_intent webhook: %v")
	if !owned || parsedKey != usdLate {
		t.Fatalf("historical webhook parse key=%q owned=%v, want %q/true", parsedKey, owned, usdLate)
	}
	mustf(t, store.ReconcileBuyerChargeOperation(ctx, parsedKey, charge),
		"reconcile historical USD payment_intent webhook: %v")
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
	const needOneMajorNanos int64 = NanosPerMajorUnit // $1 exact
	err = evaluateRealtimeBuyerFunding(ctx, tx, buyerID, needOneMajorNanos)
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
	err = evaluateRealtimeBuyerFunding(ctx, tx, buyerID, needOneMajorNanos)
	_ = tx.Rollback(ctx)
	if err != nil {
		t.Fatalf("collected CAD prepaid cash did not fund CAD realtime request: %v", err)
	}
}

// restoreLegacyUnlabeledPrepaidBalances rebuilds the pre-currency table shape
// so Migrate() re-runs the unlabeled-balance reconstruction path.
func restoreLegacyUnlabeledPrepaidBalances(t *testing.T, ctx context.Context, pool *pgxpool.Pool, buyerID uuid.UUID, micros int64) {
	t.Helper()
	_, err := pool.Exec(ctx, `DROP TABLE buyer_prepaid_balances`)
	must(t, err)
	_, err = pool.Exec(ctx, `
		CREATE TABLE buyer_prepaid_balances (
		  buyer_id UUID PRIMARY KEY REFERENCES buyers(id) ON DELETE RESTRICT,
		  balance_micros BIGINT NOT NULL DEFAULT 0 CHECK (balance_micros >= 0),
		  updated_at TIMESTAMPTZ NOT NULL DEFAULT now()
		)`)
	must(t, err)
	_, err = pool.Exec(ctx, `INSERT INTO buyer_prepaid_balances (buyer_id,balance_micros) VALUES ($1,$2)`, buyerID, micros)
	must(t, err)
}

func TestLegacyPrepaidBalanceMigrationLabelsFromCADOperationsHistory(t *testing.T) {
	// CAD cash collected before the currency column existed must be labelled
	// cad from prepaid_topup_operations — never silently usd.
	installSettlementCurrencyForTest(t, "cad")
	ctx, store, pool := openIsolatedTestStore(t)
	buyerID := uuid.New()
	_, err := pool.Exec(ctx, `INSERT INTO buyers (id,email) VALUES ($1,$2)`, buyerID, buyerID.String()+"@legacy-cad.invalid")
	must(t, err)

	const balanceMicros int64 = 25_000_000 // 25.00 CAD
	const amountCents int64 = 2500
	opKey := "legacy-cad-topup-" + uuid.NewString()
	_, err = pool.Exec(ctx, `
		INSERT INTO prepaid_topup_operations
		  (operation_key,buyer_id,amount_cents,currency,status,payment_intent,charge_id,credited_at)
		VALUES ($1,$2,$3,'cad','succeeded',$4,$5,now())`,
		opKey, buyerID, amountCents, "pi_"+opKey, "ch_"+opKey)
	must(t, err)

	restoreLegacyUnlabeledPrepaidBalances(t, ctx, pool, buyerID, balanceMicros)
	must(t, store.Migrate(ctx))

	var currency string
	var amount int64
	if err := pool.QueryRow(ctx, `SELECT currency,balance_micros FROM buyer_prepaid_balances WHERE buyer_id=$1`, buyerID).
		Scan(&currency, &amount); err != nil {
		t.Fatal(err)
	}
	if currency != "cad" || amount != balanceMicros {
		t.Fatalf("legacy CAD balance migrated as %s/%d, want cad/%d", currency, amount, balanceMicros)
	}

	// Spendable under CAD settlement: the reconstructed bucket funds the buyer.
	got, err := store.BuyerPrepaidBalanceMicros(ctx, buyerID)
	must(t, err)
	if got != balanceMicros {
		t.Fatalf("CAD settlement cannot see migrated CAD balance: got %d want %d", got, balanceMicros)
	}
	// A separate USD liability may still coexist after the composite key lands.
	if _, err := pool.Exec(ctx, `INSERT INTO buyer_prepaid_balances (buyer_id,currency,balance_micros) VALUES ($1,'usd',9)`, buyerID); err != nil {
		t.Fatalf("composite currency key did not admit separate USD liability: %v", err)
	}
}

func TestLegacyPrepaidBalanceMigrationFailsWithoutDeterminableCurrency(t *testing.T) {
	t.Parallel()
	// Non-zero unlabeled cash with no operations history must fail Migrate()
	// loudly — never default to usd.
	ctx, store, pool := openIsolatedTestStore(t)
	buyerID := uuid.New()
	_, err := pool.Exec(ctx, `INSERT INTO buyers (id,email) VALUES ($1,$2)`, buyerID, buyerID.String()+"@legacy-fail.invalid")
	must(t, err)

	restoreLegacyUnlabeledPrepaidBalances(t, ctx, pool, buyerID, 1_234_567)
	err = store.Migrate(ctx)
	if err == nil {
		t.Fatal("Migrate() labelled non-zero unlabeled balance without operations history")
	}
	if !strings.Contains(err.Error(), "cannot determine currency") &&
		!strings.Contains(err.Error(), "unlabeled") {
		t.Fatalf("Migrate() error does not name the currency reconstruction failure: %v", err)
	}
}

func TestLegacyPrepaidZeroBalanceMigrationAcceptsPlaceholderLabel(t *testing.T) {
	t.Parallel()
	// Zero balances hold no cash; a placeholder label is safe when history is empty.
	ctx, store, pool := openIsolatedTestStore(t)
	buyerID := uuid.New()
	_, err := pool.Exec(ctx, `INSERT INTO buyers (id,email) VALUES ($1,$2)`, buyerID, buyerID.String()+"@legacy-zero.invalid")
	must(t, err)

	restoreLegacyUnlabeledPrepaidBalances(t, ctx, pool, buyerID, 0)
	must(t, store.Migrate(ctx))

	var currency string
	var amount int64
	if err := pool.QueryRow(ctx, `SELECT currency,balance_micros FROM buyer_prepaid_balances WHERE buyer_id=$1`, buyerID).
		Scan(&currency, &amount); err != nil {
		t.Fatal(err)
	}
	if amount != 0 {
		t.Fatalf("zero balance changed during migration: %d", amount)
	}
	if currency != "usd" && currency != "cad" && currency != "jpy" {
		t.Fatalf("zero balance label %q is not a supported currency", currency)
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
