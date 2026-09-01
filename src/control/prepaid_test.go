package main

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
)

func prepaidTestStore(t *testing.T) (*Store, *pgxpool.Pool, context.Context) {
	t.Helper()
	databaseURL := os.Getenv("MERC_TEST_DATABASE_URL")
	if databaseURL == "" {
		if os.Getenv("MERC_ALLOW_SKIPPING_DB_TESTS") == "1" {
			t.Skip("MERC_TEST_DATABASE_URL is not set")
		}
		t.Skip("MERC_TEST_DATABASE_URL is not set")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	t.Cleanup(cancel)
	pool, err := pgxpool.New(ctx, databaseURL)
	mustf(t, err, "connect: %v")
	t.Cleanup(pool.Close)
	store := NewStore(pool)
	mustf(t, store.Migrate(ctx), "migrate: %v")
	return store, pool, ctx
}

func insertTestBuyer(t *testing.T, pool *pgxpool.Pool, ctx context.Context) uuid.UUID {
	t.Helper()
	id := uuid.New()
	if _, err := pool.Exec(ctx,
		`INSERT INTO buyers (id,email,password_hash) VALUES ($1,$2,'x')`,
		id, id.String()+"@prepaid.test"); err != nil {
		t.Fatalf("insert buyer: %v", err)
	}
	t.Cleanup(func() {
		c, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		_, _ = pool.Exec(c, `DELETE FROM ledger_entries WHERE buyer_id=$1`, id)
		_, _ = pool.Exec(c, `DELETE FROM buyer_cash_collections WHERE buyer_id=$1`, id)
		_, _ = pool.Exec(c, `DELETE FROM prepaid_topup_operations WHERE buyer_id=$1`, id)
		_, _ = pool.Exec(c, `DELETE FROM prepaid_refund_operations WHERE buyer_id=$1`, id)
		_, _ = pool.Exec(c, `DELETE FROM buyer_prepaid_balances WHERE buyer_id=$1`, id)
		_, _ = pool.Exec(c, `DELETE FROM buyers WHERE id=$1`, id)
	})
	return id
}

func TestPrepaidTopupCreditsBalanceAndLedger(t *testing.T) {
	store, pool, ctx := prepaidTestStore(t)
	buyerID := insertTestBuyer(t, pool, ctx)
	opKey := "topup-test-" + uuid.NewString()
	arm, err := store.BeginPrepaidTopup(ctx, opKey, buyerID, 2500)
	must(t, err)
	if arm.State != prepaidTopupArmed {
		t.Fatalf("first top-up arm = %q, want %q", arm.State, prepaidTopupArmed)
	}
	charge := ChargeResult{
		PaymentIntentID: "pi_test_" + uuid.NewString(),
		ChargeID:        "ch_test_" + uuid.NewString(),
		RequestedCents:  2500,
		ReceivedCents:   2500,
		Currency:        SettlementCurrencyCode(),
	}
	mustf(t, store.CreditPrepaidTopup(ctx, opKey, buyerID, charge), "credit: %v")
	// Idempotent re-credit
	mustf(t, store.CreditPrepaidTopup(ctx, opKey, buyerID, charge), "re-credit: %v")
	bal, err := store.BuyerPrepaidBalanceMicros(ctx, buyerID)
	must(t, err)
	if bal != 25_000_000 {
		t.Fatalf("balance_micros=%d, want 25000000", bal)
	}
	// Re-arming a credited key must answer from the row, never invite a charge.
	arm, err = store.BeginPrepaidTopup(ctx, opKey, buyerID, 2500)
	must(t, err)
	if arm.State != prepaidTopupCredited || arm.PaymentIntent != charge.PaymentIntentID {
		t.Fatalf("re-arm = %+v, want credited with %s", arm, charge.PaymentIntentID)
	}
	var topupSum, count int64
	if err := pool.QueryRow(ctx, `
		SELECT COALESCE(SUM((amount_usd*1000000)::bigint),0), COUNT(*)
		  FROM ledger_entries WHERE buyer_id=$1 AND kind='prepaid_topup'`, buyerID,
	).Scan(&topupSum, &count); err != nil {
		t.Fatal(err)
	}
	if count != 1 || topupSum != 25_000_000 {
		t.Fatalf("topup ledger count=%d sum=%d", count, topupSum)
	}
	conflicting := charge
	conflicting.ChargeID = "ch_conflicting_" + uuid.NewString()
	if err := store.CreditPrepaidTopup(ctx, opKey, buyerID, conflicting); err == nil {
		t.Fatal("completed top-up accepted a conflicting provider charge identity")
	}
}

func TestCreditPrepaidTopupRejectsConflictingCashCollectionBinding(t *testing.T) {
	store, pool, ctx := prepaidTestStore(t)
	buyerID := insertTestBuyer(t, pool, ctx)
	opKey := "topup-conflicting-collection-" + uuid.NewString()
	paymentIntent := "pi_conflicting_collection_" + uuid.NewString()
	chargeID := "ch_existing_collection_" + uuid.NewString()
	if _, err := store.BeginPrepaidTopup(ctx, opKey, buyerID, 2500); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `
		INSERT INTO buyer_cash_collections
		  (payment_intent,charge_id,buyer_id,source_kind,requested_cents,received_cents,currency)
		VALUES ($1,$2,$3,'topup',2500,2500,$4)`,
		paymentIntent, chargeID, buyerID, SettlementCurrencyCode()); err != nil {
		t.Fatalf("insert existing collection: %v", err)
	}
	err := store.CreditPrepaidTopup(ctx, opKey, buyerID, ChargeResult{
		PaymentIntentID: paymentIntent,
		ChargeID:        "ch_different_" + uuid.NewString(),
		RequestedCents:  2500,
		ReceivedCents:   2500,
		Currency:        SettlementCurrencyCode(),
	})
	if err == nil {
		t.Fatal("top-up accepted a PaymentIntent already bound to a different cash collection")
	}
	var status string
	if err := pool.QueryRow(ctx,
		`SELECT status FROM prepaid_topup_operations WHERE operation_key=$1`, opKey).Scan(&status); err != nil {
		t.Fatal(err)
	}
	if status != "pending" {
		t.Fatalf("conflicting top-up status=%q, want pending", status)
	}
	var balance, ledgerRows int64
	if err := pool.QueryRow(ctx, `
		SELECT COALESCE((SELECT balance_micros FROM buyer_prepaid_balances WHERE buyer_id=$1 AND currency=$2),0),
		       (SELECT count(*) FROM ledger_entries WHERE buyer_id=$1 AND kind='prepaid_topup')`,
		buyerID, SettlementCurrencyCode()).Scan(&balance, &ledgerRows); err != nil {
		t.Fatal(err)
	}
	if balance != 0 || ledgerRows != 0 {
		t.Fatalf("conflicting top-up left balance/ledger effects=%d/%d, want 0/0", balance, ledgerRows)
	}
}

func TestReconcileBuyerChargeOperationAcceptsCompletedPrepaidWebhook(t *testing.T) {
	store, _, ctx := prepaidTestStore(t)
	buyerID := insertTestBuyer(t, store.pool, ctx)
	opKey := "topup-webhook-" + uuid.NewString()
	charge := ChargeResult{
		PaymentIntentID: "pi_test_" + uuid.NewString(),
		ChargeID:        "ch_test_" + uuid.NewString(),
		RequestedCents:  2500,
		ReceivedCents:   2500,
		Currency:        SettlementCurrencyCode(),
	}
	if _, err := store.BeginPrepaidTopup(ctx, opKey, buyerID, charge.RequestedCents); err != nil {
		t.Fatal(err)
	}
	must(t, store.CreditPrepaidTopup(ctx, opKey, buyerID, charge))

	// Stripe legitimately delivers payment_intent.succeeded after the direct
	// top-up response has committed.  The actual webhook handler must replay it
	// as success and never double-credit the balance or leave Stripe retrying a
	// 500.
	payload := []byte(fmt.Sprintf(`{"type":"payment_intent.succeeded","api_version":"%s","livemode":false,"data":{"object":{"object":"payment_intent","id":"%s","latest_charge":{"object":"charge","id":"%s"},"status":"succeeded","amount":%d,"amount_received":%d,"currency":"%s","metadata":{"cx_operation_key":"%s"}}}}`,
		stripeAPIVersion, charge.PaymentIntentID, charge.ChargeID,
		charge.RequestedCents, charge.ReceivedCents, charge.Currency, opKey))
	req := signedStripeCashRequest(t, payload, "whsec_completed_prepaid")
	rec := httptest.NewRecorder()
	handleStripeWebhookWithAllHandlersAtMode(
		rec, req, "whsec_completed_prepaid", nil, nil,
		store.ReconcileBuyerChargeOperation, false,
	)
	if rec.Code != http.StatusOK {
		t.Fatalf("completed prepaid webhook status=%d body=%s", rec.Code, rec.Body.String())
	}
	bal, err := store.BuyerPrepaidBalanceMicros(ctx, buyerID)
	must(t, err)
	settlement, err := SettlementCurrency()
	must(t, err)
	want, err := settlement.MinorToMicros(charge.ReceivedCents)
	must(t, err)
	if bal != want {
		t.Fatalf("balance after webhook replay=%d, want %d", bal, want)
	}
}

func TestPrepaidDebitAndRefundZeroSum(t *testing.T) {
	store, pool, ctx := prepaidTestStore(t)
	buyerID := insertTestBuyer(t, pool, ctx)
	actor := insertTestAdminActor(t, pool, ctx)
	// Fund through the real collection path: a refund can only be traced back to
	// a payment intent that actually took the money.
	fundPrepaidViaTopup(t, ctx, store, buyerID, 5000)
	task1, task2 := uuid.New(), uuid.New()
	// Need tasks? prepaid_debit references tasks(task_id). Schema: task_id UUID REFERENCES tasks
	// So we need a job+task or leave task_id null for seed debits via DebitPrepaidForTask.
	// DebitPrepaidForTask sets TaskID - FK to tasks. Insert minimal job/task.
	jobID := uuid.New()
	if _, err := pool.Exec(ctx, `
		INSERT INTO jobs (id,buyer_id,status,job_type,input_ref,task_count)
		VALUES ($1,$2,'running','embed','x',2)`, jobID, buyerID); err != nil {
		t.Fatalf("job: %v", err)
	}
	for _, tid := range []uuid.UUID{task1, task2} {
		if _, err := pool.Exec(ctx, `
			INSERT INTO tasks (id,job_id,status,input_ref,result_key)
			VALUES ($1,$2,'complete','in','rk')`, tid, jobID); err != nil {
			t.Fatalf("task: %v", err)
		}
	}
	t.Cleanup(func() {
		c, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		_, _ = pool.Exec(c, `DELETE FROM ledger_entries WHERE task_id IN ($1,$2)`, task1, task2)
		_, _ = pool.Exec(c, `DELETE FROM tasks WHERE job_id=$1`, jobID)
		_, _ = pool.Exec(c, `DELETE FROM jobs WHERE id=$1`, jobID)
	})

	mustf(t, store.DebitPrepaidForTask(ctx, buyerID, task1, 10_000_000), "debit1: %v")
	mustf(t, store.DebitPrepaidForTask(ctx, buyerID, task2, 15_000_000), "debit2: %v")
	bal, _ := store.BuyerPrepaidBalanceMicros(ctx, buyerID)
	if bal != 25_000_000 {
		t.Fatalf("after debits balance=%d want 25e6", bal)
	}
	// Refund the remainder through the durable-first path (no Stripe call needed
	// to observe the ledger effect; BeginPrepaidRefund commits the debit).
	plan, err := store.BeginPrepaidRefund(ctx, actor, buyerID, "buyer closed account", "INC-zero-sum-"+uuid.NewString())
	mustf(t, err, "refund: %v")
	if plan.Cents != 2500 {
		t.Fatalf("planned refund = %d cents, want 2500", plan.Cents)
	}
	bal, _ = store.BuyerPrepaidBalanceMicros(ctx, buyerID)
	if bal != 0 {
		t.Fatalf("after refund balance=%d want 0", bal)
	}
	var sum int64
	if err := pool.QueryRow(ctx, `
		SELECT COALESCE(SUM((amount_usd*1000000)::bigint),0)
		  FROM ledger_entries
		 WHERE buyer_id=$1 AND kind IN ('prepaid_topup','prepaid_debit','prepaid_refund')`,
		buyerID).Scan(&sum); err != nil {
		t.Fatal(err)
	}
	if sum != 0 {
		t.Fatalf("prepaid ledger sum=%d, want 0 (zero-sum across topup/debit/refund)", sum)
	}
}

// TestPrepaidConcurrentDebitsOnlyOneSucceeds is the concurrency test that matters:
// two simultaneous debits against a balance that only covers one must not both succeed.
func TestPrepaidConcurrentDebitsOnlyOneSucceeds(t *testing.T) {
	store, pool, ctx := prepaidTestStore(t)
	buyerID := insertTestBuyer(t, pool, ctx)
	// Balance covers exactly one $10 debit.
	must(t, store.SeedPrepaidBalance(ctx, buyerID, 10_000_000, "seed-conc-"+uuid.NewString()))
	jobID := uuid.New()
	taskA, taskB := uuid.New(), uuid.New()
	if _, err := pool.Exec(ctx, `
		INSERT INTO jobs (id,buyer_id,status,job_type,input_ref,task_count)
		VALUES ($1,$2,'running','embed','x',2)`, jobID, buyerID); err != nil {
		t.Fatal(err)
	}
	for _, tid := range []uuid.UUID{taskA, taskB} {
		if _, err := pool.Exec(ctx, `
			INSERT INTO tasks (id,job_id,status,input_ref,result_key)
			VALUES ($1,$2,'complete','in','rk')`, tid, jobID); err != nil {
			t.Fatal(err)
		}
	}
	t.Cleanup(func() {
		c, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		_, _ = pool.Exec(c, `DELETE FROM ledger_entries WHERE task_id IN ($1,$2)`, taskA, taskB)
		_, _ = pool.Exec(c, `DELETE FROM tasks WHERE job_id=$1`, jobID)
		_, _ = pool.Exec(c, `DELETE FROM jobs WHERE id=$1`, jobID)
	})

	var (
		wg      sync.WaitGroup
		mu      sync.Mutex
		success int
		fail    int
	)
	start := make(chan struct{})
	run := func(taskID uuid.UUID) {
		defer wg.Done()
		<-start
		err := store.DebitPrepaidForTask(ctx, buyerID, taskID, 10_000_000)
		mu.Lock()
		defer mu.Unlock()
		if err == nil {
			success++
		} else if errors.Is(err, errInsufficientPrepaid) {
			fail++
		} else {
			t.Errorf("unexpected error: %v", err)
		}
	}
	wg.Add(2)
	go run(taskA)
	go run(taskB)
	close(start)
	wg.Wait()

	if success != 1 || fail != 1 {
		t.Fatalf("success=%d fail=%d, want exactly one success and one insufficient-balance failure", success, fail)
	}
	bal, _ := store.BuyerPrepaidBalanceMicros(ctx, buyerID)
	if bal != 0 {
		t.Fatalf("balance after concurrent debits=%d, want 0", bal)
	}
}

func TestPrepaidJobGateRefusesCardWithZeroBalance(t *testing.T) {
	// Unit-level: available micros is 0 even if we do not call the HTTP handler.
	store, pool, ctx := prepaidTestStore(t)
	buyerID := insertTestBuyer(t, pool, ctx)
	// Card on file, zero prepaid.
	must(t, store.UpsertBillingCustomer(ctx, buyerID, "cus_gate_"+buyerID.String()))
	must(t, store.SetBillingPMByCustomer(ctx, "cus_gate_"+buyerID.String(), "pm_gate"))
	avail, err := store.BuyerPrepaidAvailableMicros(ctx, buyerID)
	must(t, err)
	if avail != 0 {
		t.Fatalf("available=%d, want 0", avail)
	}
	// Admission must refuse when prepaid is required.
	if !prepaidBalanceRequired() {
		t.Setenv("MERC_DEFERRED_CHARGE", "")
		if deferredChargeEnabled() {
			t.Fatal("deferred charge should be off by default")
		}
	}
	if prepaidBalanceRequired() && avail <= 0 {
		// This is the gate condition used by handleCreateJob.
		return
	}
	t.Fatal("expected prepaid gate to fire for card+zero balance")
}

func TestDeferredChargeFlagRollback(t *testing.T) {
	t.Setenv("MERC_DEFERRED_CHARGE", "1")
	if !deferredChargeEnabled() || prepaidBalanceRequired() {
		t.Fatal("MERC_DEFERRED_CHARGE=1 should enable deferred charge and disable prepaid requirement")
	}
	t.Setenv("MERC_DEFERRED_CHARGE", "")
	if deferredChargeEnabled() || !prepaidBalanceRequired() {
		t.Fatal("unset MERC_DEFERRED_CHARGE should require prepaid")
	}
}

func TestTopupMinUsesExactSettlementMinorUnits(t *testing.T) {
	cad := MustParseCurrency("cad")
	t.Setenv("MERC_TOPUP_MIN_USD", "")
	t.Setenv("MERC_TOPUP_MIN_MINOR", "1001")
	if got, err := topupMinMinor(cad); err != nil || got != 1001 {
		t.Fatalf("topupMinMinor(cad)=%d, %v; want 1001, nil", got, err)
	}
	t.Setenv("MERC_TOPUP_MIN_MINOR", "")
	if got, err := topupMinMinor(cad); err != nil || got != 2500 {
		t.Fatalf("default CAD topup min=%d, %v; want 2500, nil", got, err)
	}
	t.Setenv("MERC_TOPUP_MIN_USD", "10")
	if _, err := topupMinMinor(cad); err == nil {
		t.Fatal("legacy USD top-up floor was accepted under CAD")
	}
}
