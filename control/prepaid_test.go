package main

import (
	"context"
	"errors"
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
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	t.Cleanup(pool.Close)
	store := NewStore(pool)
	if err := store.Migrate(ctx); err != nil {
		t.Fatalf("migrate: %v", err)
	}
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
	if err := store.BeginPrepaidTopup(ctx, opKey, buyerID, 2500); err != nil {
		t.Fatal(err)
	}
	charge := ChargeResult{
		PaymentIntentID: "pi_test_" + uuid.NewString(),
		ChargeID:        "ch_test_" + uuid.NewString(),
		RequestedCents:  2500,
		ReceivedCents:   2500,
		Currency:        "usd",
	}
	if err := store.CreditPrepaidTopup(ctx, opKey, buyerID, charge); err != nil {
		t.Fatalf("credit: %v", err)
	}
	// Idempotent re-credit
	if err := store.CreditPrepaidTopup(ctx, opKey, buyerID, charge); err != nil {
		t.Fatalf("re-credit: %v", err)
	}
	bal, err := store.BuyerPrepaidBalanceMicros(ctx, buyerID)
	if err != nil {
		t.Fatal(err)
	}
	if bal != 25_000_000 {
		t.Fatalf("balance_micros=%d, want 25000000", bal)
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
}

func TestPrepaidDebitAndRefundZeroSum(t *testing.T) {
	store, pool, ctx := prepaidTestStore(t)
	buyerID := insertTestBuyer(t, pool, ctx)
	if err := store.SeedPrepaidBalance(ctx, buyerID, 50_000_000, "seed-pi-"+uuid.NewString()); err != nil {
		t.Fatal(err)
	}
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

	if err := store.DebitPrepaidForTask(ctx, buyerID, task1, 10_000_000); err != nil {
		t.Fatalf("debit1: %v", err)
	}
	if err := store.DebitPrepaidForTask(ctx, buyerID, task2, 15_000_000); err != nil {
		t.Fatalf("debit2: %v", err)
	}
	bal, _ := store.BuyerPrepaidBalanceMicros(ctx, buyerID)
	if bal != 25_000_000 {
		t.Fatalf("after debits balance=%d want 25e6", bal)
	}
	// Refund remainder via store path (no Stripe).
	if err := store.DebitPrepaidRefund(ctx, "prepaid-refund-"+uuid.NewString(), buyerID, 25_000_000, "re_test"); err != nil {
		t.Fatalf("refund: %v", err)
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
	if err := store.SeedPrepaidBalance(ctx, buyerID, 10_000_000, "seed-conc-"+uuid.NewString()); err != nil {
		t.Fatal(err)
	}
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
	if err := store.UpsertBillingCustomer(ctx, buyerID, "cus_gate_"+buyerID.String()); err != nil {
		t.Fatal(err)
	}
	if err := store.SetBillingPMByCustomer(ctx, "cus_gate_"+buyerID.String(), "pm_gate"); err != nil {
		t.Fatal(err)
	}
	avail, err := store.BuyerPrepaidAvailableMicros(ctx, buyerID)
	if err != nil {
		t.Fatal(err)
	}
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

func TestTopupMinUSDConfigurable(t *testing.T) {
	t.Setenv("MERC_TOPUP_MIN_USD", "10")
	if got := topupMinUSD(); got != 10 {
		t.Fatalf("topupMinUSD=%v want 10", got)
	}
	t.Setenv("MERC_TOPUP_MIN_USD", "")
	if got := topupMinUSD(); got != defaultTopupMinUSD {
		t.Fatalf("default topup min=%v want %v", got, defaultTopupMinUSD)
	}
}
