package main

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
)

// TestPrepaidFundingAdmissionSerializesReservations proves the important
// property the old soft balance check could not: two concurrent submissions
// cannot both reserve the same collected balance.
func TestPrepaidFundingAdmissionSerializesReservations(t *testing.T) {
	ctx, store, pool := openIsolatedMoneyPathStore(t)
	f := seedMoneyPathFixture(t, ctx, store, pool, moneyPathSeedOpts{TaskCount: 1})
	firstTasks := makeTasks(f, 1)
	first := validJobRow(t, f, firstTasks)
	first.PrepaidRequired = true

	secondFixture := f
	secondFixture.JobID = uuid.New()
	secondFixture.TaskIDs = []uuid.UUID{uuid.New()}
	secondTasks := makeTasks(secondFixture, 1)
	second := validJobRow(t, secondFixture, secondTasks)
	second.PrepaidRequired = true

	reserve := usdToMicros(first.EconomicPlan.ReservedBuyerChargeUSD)
	if reserve != usdToMicros(second.EconomicPlan.ReservedBuyerChargeUSD) {
		t.Fatal("fixture reservations differ")
	}
	must(t, store.SeedPrepaidBalance(ctx, f.BuyerID, reserve, "seed-admission-"+uuid.NewString()))

	ctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	start := make(chan struct{})
	errs := make(chan error, 2)
	var wg sync.WaitGroup
	for _, submission := range []struct {
		job   *jobRow
		tasks []taskRow
	}{{first, firstTasks}, {second, secondTasks}} {
		wg.Add(1)
		go func(submission struct {
			job   *jobRow
			tasks []taskRow
		}) {
			defer wg.Done()
			<-start
			errs <- store.SubmitJobTx(ctx, submission.job, submission.tasks)
		}(submission)
	}
	close(start)
	wg.Wait()
	close(errs)

	var accepted, insufficient int
	for err := range errs {
		switch {
		case err == nil:
			accepted++
		case errors.Is(err, errInsufficientPrepaid):
			insufficient++
		default:
			t.Fatalf("unexpected concurrent submit error: %v", err)
		}
	}
	if accepted != 1 || insufficient != 1 {
		t.Fatalf("concurrent prepaid admission accepted=%d insufficient=%d, want 1/1", accepted, insufficient)
	}
	var jobs int
	must(t, pool.QueryRow(ctx, `SELECT count(*) FROM jobs WHERE buyer_id=$1 AND prepaid_required`, f.BuyerID).Scan(&jobs))
	if jobs != 1 {
		t.Fatalf("prepaid jobs=%d, want exactly one", jobs)
	}
}

func TestPrepaidFundingSettlementDebitsOnceAndFailsClosed(t *testing.T) {
	t.Run("settles observed buyer charge once", func(t *testing.T) {
		ctx, store, pool := openIsolatedMoneyPathStore(t)
		f := seedMoneyPathFixture(t, ctx, store, pool, moneyPathSeedOpts{
			TaskCount: 1, TaskStatus: "verifying", ClaimWorker: true,
			SeedJob: true, SeedPlanRows: true, PrepaidRequired: true,
		})
		amount := usdToMicros(f.Plan.ReservedBuyerChargeUSD)
		must(t, store.SeedPrepaidBalance(ctx, f.BuyerID, amount, "seed-settle-"+uuid.NewString()))
		entries := splitFrozenCharge(f.BuyerID, f.SupplierID, f.TaskIDs[0], SettlementCurrencyCode(),
			f.Plan.BuyerChargePerTaskUSD, f.Plan.SupplierPayoutPerTaskUSD, 0, time.Now().UTC())
		info := &CommitTaskInfo{
			TaskID: f.TaskIDs[0], JobID: f.JobID, WorkerID: f.WorkerID, SupplierID: f.SupplierID,
			jobType: "embed", ModelRef: "all-minilm-l6-v2", SplitSize: 1, engine: "candle", buildHash: "deadbeefdeadbeef",
		}
		applied, err := store.applyVerificationDecision(ctx, info, VerificationDecision{Outcome: OutcomePass}, entries, nil, nil)
		if err != nil || !applied.Applied {
			t.Fatalf("settle prepaid task=(%+v,%v)", applied, err)
		}
		wantDebit := usdToMicros(f.Plan.BuyerChargePerTaskUSD)
		balance, err := store.BuyerPrepaidBalanceMicros(ctx, f.BuyerID)
		must(t, err)
		if balance != amount-wantDebit {
			t.Fatalf("prepaid balance=%d, want %d", balance, amount-wantDebit)
		}
		var debitRows, debitMicros int64
		if err := pool.QueryRow(ctx, `
			SELECT count(*),COALESCE(SUM((-amount_usd*1000000)::bigint),0)
			  FROM ledger_entries WHERE task_id=$1 AND kind='prepaid_debit'`, f.TaskIDs[0]).
			Scan(&debitRows, &debitMicros); err != nil {
			t.Fatal(err)
		}
		if debitRows != 1 || debitMicros != wantDebit {
			t.Fatalf("prepaid task debit rows/micros=%d/%d, want 1/%d", debitRows, debitMicros, wantDebit)
		}
	})

	t.Run("underfunded settlement rolls back every effect", func(t *testing.T) {
		ctx, store, pool := openIsolatedMoneyPathStore(t)
		f := seedMoneyPathFixture(t, ctx, store, pool, moneyPathSeedOpts{
			TaskCount: 1, TaskStatus: "verifying", ClaimWorker: true,
			SeedJob: true, SeedPlanRows: true, PrepaidRequired: true,
		})
		wantDebit := usdToMicros(f.Plan.BuyerChargePerTaskUSD)
		must(t, store.SeedPrepaidBalance(ctx, f.BuyerID, wantDebit-1, "seed-underfunded-"+uuid.NewString()))
		entries := splitFrozenCharge(f.BuyerID, f.SupplierID, f.TaskIDs[0], SettlementCurrencyCode(),
			f.Plan.BuyerChargePerTaskUSD, f.Plan.SupplierPayoutPerTaskUSD, 0, time.Now().UTC())
		info := &CommitTaskInfo{
			TaskID: f.TaskIDs[0], JobID: f.JobID, WorkerID: f.WorkerID, SupplierID: f.SupplierID,
			jobType: "embed", ModelRef: "all-minilm-l6-v2", SplitSize: 1, engine: "candle", buildHash: "deadbeefdeadbeef",
		}
		_, err := store.applyVerificationDecision(ctx, info, VerificationDecision{Outcome: OutcomePass}, entries, nil, nil)
		if !errors.Is(err, errInsufficientPrepaid) {
			t.Fatalf("underfunded settlement error=%v, want insufficient prepaid", err)
		}
		if got := taskStatus(t, ctx, pool, f.TaskIDs[0]); got != "verifying" {
			t.Fatalf("underfunded task status=%s, want verifying", got)
		}
		var ledgerRows int
		must(t, pool.QueryRow(ctx, `SELECT count(*) FROM ledger_entries WHERE task_id=$1`, f.TaskIDs[0]).Scan(&ledgerRows))
		if ledgerRows != 0 {
			t.Fatalf("underfunded settlement wrote %d ledger rows", ledgerRows)
		}
	})
}

func TestPrepaidFundingSLAPremiumAndCollectionNet(t *testing.T) {
	t.Run("SLA premium is one durable prepaid debit", func(t *testing.T) {
		ctx, store, pool := openIsolatedMoneyPathStore(t)
		f := seedMoneyPathFixture(t, ctx, store, pool, moneyPathSeedOpts{
			TaskCount: 1, TaskStatus: "complete", SeedJob: true, SeedPlanRows: true,
			SLAPremium: 0.15, PrepaidRequired: true,
		})
		premium := usdToMicros(f.Plan.Input.SLAPremiumUSD)
		must(t, store.SeedPrepaidBalance(ctx, f.BuyerID, premium, "seed-sla-"+uuid.NewString()))
		mustf(t, store.FinalizeJobTx(ctx, f.JobID), "finalize SLA job: %v")
		mustf(t, store.FinalizeJobTx(ctx, f.JobID), "repeat finalization: %v")
		balance, err := store.BuyerPrepaidBalanceMicros(ctx, f.BuyerID)
		must(t, err)
		if balance != 0 {
			t.Fatalf("SLA debit balance=%d, want 0", balance)
		}
		var rows, debited int64
		if err := pool.QueryRow(ctx, `
			SELECT count(*),COALESCE(SUM((-amount_usd*1000000)::bigint),0)
			  FROM ledger_entries WHERE kind='prepaid_debit' AND payout_ref=$1`, prepaidSLAPremiumDebitRef(f.JobID)).
			Scan(&rows, &debited); err != nil {
			t.Fatal(err)
		}
		if rows != 1 || debited != premium {
			t.Fatalf("SLA prepaid debit rows/micros=%d/%d, want 1/%d", rows, debited, premium)
		}
	})

	t.Run("fully prepaid job is absent from collection", func(t *testing.T) {
		ctx, store, pool := openIsolatedMoneyPathStore(t)
		buyerID, jobID, taskID := uuid.New(), uuid.New(), uuid.New()
		if _, err := pool.Exec(ctx, `INSERT INTO buyers (id,email,password_hash) VALUES ($1,$2,'x')`, buyerID, buyerID.String()+"@prepaid-collection.invalid"); err != nil {
			t.Fatal(err)
		}
		if _, err := pool.Exec(ctx, `
			INSERT INTO jobs (id,buyer_id,status,job_type,input_ref,actual_usd,prepaid_required)
			VALUES ($1,$2,'complete','embed','prepaid/collection',1.25,true)`, jobID, buyerID); err != nil {
			t.Fatal(err)
		}
		if _, err := pool.Exec(ctx, `INSERT INTO tasks (id,job_id,status) VALUES ($1,$2,'complete')`, taskID, jobID); err != nil {
			t.Fatal(err)
		}
		for _, row := range []struct {
			kind   string
			amount int64
		}{
			{KindBuyerCharge, -1_250_000}, {KindPrepaidDebit, -1_250_000},
		} {
			if _, err := pool.Exec(ctx, `
				INSERT INTO ledger_entries (kind,buyer_id,task_id,amount_usd,currency,payout_status)
				VALUES ($1,$2,$3,($4::numeric/1000000),'usd','released')`, row.kind, buyerID, taskID, row.amount); err != nil {
				t.Fatal(err)
			}
		}
		_, charge, err := store.JobChargeInfo(ctx, jobID)
		if err != nil || charge != 0 {
			t.Fatalf("prepaid JobChargeInfo=(%v,%v), want nil/0", err, charge)
		}
		terminal, err := store.TerminalUnattemptedJobs(ctx, 20)
		must(t, err)
		for _, id := range terminal {
			if id == jobID {
				t.Fatal("fully prepaid job reached terminal card collection")
			}
		}
		if _, err := pool.Exec(ctx, `UPDATE jobs SET charge_status='deferred' WHERE id=$1`, jobID); err != nil {
			t.Fatal(err)
		}
		buyers, err := store.BuyersDueForBatch(ctx, 0.50, time.Hour, 20)
		must(t, err)
		for _, id := range buyers {
			if id == buyerID {
				t.Fatal("fully prepaid buyer reached batch card collection")
			}
		}
		if _, formed, err := store.FormChargeBatch(ctx, buyerID); err != nil || formed {
			t.Fatalf("fully prepaid batch formation=(%v,%v), want nil/false", err, formed)
		}
	})
}

func TestDeferredSubmissionDoesNotRequirePrepaidReservation(t *testing.T) {
	ctx, store, pool := openIsolatedMoneyPathStore(t)
	f := seedMoneyPathFixture(t, ctx, store, pool, moneyPathSeedOpts{TaskCount: 1})
	tasks := makeTasks(f, 1)
	job := validJobRow(t, f, tasks)
	// The zero-value authority represents a deferred/legacy job. No balance row
	// exists and submission remains accepted, preserving rollback behaviour.
	mustf(t, store.SubmitJobTx(ctx, job, tasks), "deferred submit without prepaid balance: %v")
	var prepaidRequired bool
	must(t, pool.QueryRow(ctx, `SELECT prepaid_required FROM jobs WHERE id=$1`, f.JobID).Scan(&prepaidRequired))
	if prepaidRequired {
		t.Fatal("deferred test job unexpectedly froze prepaid funding")
	}
}
