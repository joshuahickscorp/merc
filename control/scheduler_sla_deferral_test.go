package main

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
)

// SLA-aware ask deferral: a task may wait for a cheaper ask only when that wait
// demonstrably fits inside the job's paid speed guarantee. These tests drive
// ClaimTasksTx end-to-end the same way scheduler_ask_claim_integration_test does.

type slaDeferWorker struct {
	supplierID, workerID uuid.UUID
	askUSDHr             float64
}

func setupSLADeferFixture(t *testing.T, ctx context.Context, store *Store, pool *pgxpool.Pool,
	guaranteeSecs *int, etaSecs *int,
) (cheap, dear slaDeferWorker, jobID, taskID uuid.UUID) {
	t.Helper()
	suffix := uuid.NewString()
	buyerID, err := store.CreateBuyerAccount(ctx, "sla-def-"+suffix+"@example.test", "integration-password", 100)
	must(t, err)

	mk := func(ask float64) slaDeferWorker {
		w := slaDeferWorker{supplierID: uuid.New(), workerID: uuid.New(), askUSDHr: ask}
		if _, err := pool.Exec(ctx,
			`INSERT INTO suppliers (id,email,status,reputation,completed_tasks)
			 VALUES ($1,$2,'active',0.95,100)`,
			w.supplierID, "sla-def-sup-"+uuid.NewString()+"@example.test"); err != nil {
			t.Fatal(err)
		}
		if _, err := pool.Exec(ctx,
			`INSERT INTO workers (id,supplier_id,hw_class,memory_gb,effective_memory_gb,
			                      last_seen_at,throttled,min_payout_usd_hr)
			 VALUES ($1,$2,'apple_silicon_max',64,64,now(),false,$3)`,
			w.workerID, w.supplierID, ask); err != nil {
			t.Fatal(err)
		}
		bindLegacyTestWorkerExactExecutionIdentity(t, pool, ctx, w.workerID)
		if _, err := pool.Exec(ctx,
			`INSERT INTO worker_authorized_capabilities
			   (worker_id,cell_id,runtime_id,job_type,model_ref,model_kind,matrix_sha256)
			 VALUES ($1,'cell','rt','embed','all-minilm-l6-v2','hf',$2)`,
			w.workerID, generatedRuntimeMatrixSHA256); err != nil {
			t.Fatal(err)
		}
		return w
	}
	cheap = mk(0.10)
	dear = mk(5.00)

	jobID = uuid.New()
	taskID = uuid.New()
	settlement := SettlementCurrencyCode()
	if settlement == "" {
		t.Fatal("SLA deferral fixture requires a settlement currency")
	}
	// sla_guarantee_secs / eta_secs are the columns under test; pass NULL via Go nils.
	if _, err := pool.Exec(ctx, `
		INSERT INTO jobs (id,buyer_id,status,job_type,model_ref,input_ref,task_count,
		                  offered_rate_usd_hr,min_memory_gb,tier,
		                  sla_guarantee_secs,eta_secs,currency)
		VALUES ($1,$2,'running','embed','all-minilm-l6-v2','in',1,10.0,0,'batch',
		        $3,$4,$5)`,
		jobID, buyerID, guaranteeSecs, etaSecs, settlement); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `
		INSERT INTO tasks (id,job_id,status,input_ref,result_key)
		VALUES ($1,$2,'queued','in','rk')`, taskID, jobID); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		c, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cancel()
		_, _ = pool.Exec(c, `DELETE FROM tasks WHERE job_id=$1`, jobID)
		_, _ = pool.Exec(c, `DELETE FROM jobs WHERE id=$1`, jobID)
		for _, w := range []slaDeferWorker{cheap, dear} {
			_, _ = pool.Exec(c, `DELETE FROM worker_authorized_capabilities WHERE worker_id=$1`, w.workerID)
			_, _ = pool.Exec(c, `DELETE FROM workers WHERE id=$1`, w.workerID)
			_, _ = pool.Exec(c, `DELETE FROM suppliers WHERE id=$1`, w.supplierID)
		}
	})
	return cheap, dear, jobID, taskID
}

func claimAsSLA(t *testing.T, ctx context.Context, store *Store, w slaDeferWorker) *ClaimedTask {
	t.Helper()
	got, err := store.ClaimTasksTx(ctx, WorkerAuth{WorkerID: w.workerID, SupplierID: w.supplierID})
	mustf(t, err, "ClaimTasksTx: %v")
	return got
}

func intPtr(v int) *int { return &v }

// 1. SLA-bound task is not deferred.
// elapsed(~0) + askDeferralWindow(20) + eta(10) = 30 > guarantee(15) → wait does not fit → claim now.
func TestClaimTasksTxSLABoundTaskIsNotDeferred(t *testing.T) {
	t.Parallel()
	ctx, store, pool := openIsolatedTestStore(t)
	cheap, dear, _, taskID := setupSLADeferFixture(t, ctx, store, pool, intPtr(15), intPtr(10))
	_ = cheap

	got := claimAsSLA(t, ctx, store, dear)
	if got == nil || got.TaskID != taskID {
		t.Fatalf("SLA-bound task must be claimable immediately by a hard-filter-passing worker; got %+v (want task %s). Today the 20s ask deferral is blind to sla_guarantee_secs.", got, taskID)
	}
}

// 2. Generous SLA still defers — cost saving is not lost when the wait fits.
// elapsed(~0) + 20 + eta(10) = 30 <= guarantee(3600) → defer as today.
func TestClaimTasksTxGenerousSLAStillDefers(t *testing.T) {
	t.Parallel()
	ctx, store, pool := openIsolatedTestStore(t)
	cheap, dear, _, taskID := setupSLADeferFixture(t, ctx, store, pool, intPtr(3600), intPtr(10))

	got := claimAsSLA(t, ctx, store, dear)
	if got != nil && got.TaskID == taskID {
		t.Fatalf("task with roomy SLA went to the $%.2f/hr worker while a $%.2f/hr rival was online; deferral must still apply when the wait fits",
			dear.askUSDHr, cheap.askUSDHr)
	}
	got = claimAsSLA(t, ctx, store, cheap)
	if got == nil || got.TaskID != taskID {
		t.Fatalf("cheapest eligible worker should claim under a generous SLA; got %+v want %s", got, taskID)
	}
}

// 3. Unknown prediction does not defer (fail closed).
// Guarantee set, eta_secs NULL → cannot show the wait fits → do not defer.
func TestClaimTasksTxUnknownETADoesNotDefer(t *testing.T) {
	t.Parallel()
	ctx, store, pool := openIsolatedTestStore(t)
	cheap, dear, _, taskID := setupSLADeferFixture(t, ctx, store, pool, intPtr(3600), nil)
	_ = cheap

	got := claimAsSLA(t, ctx, store, dear)
	if got == nil || got.TaskID != taskID {
		t.Fatalf("guarantee without eta_secs must not defer (unknown is not zero); got %+v want task %s", got, taskID)
	}
}

// 4. No guarantee behaves exactly as today — expensive worker is held; cheap takes it.
func TestClaimTasksTxNoSLAGuaranteeDefersAsToday(t *testing.T) {
	t.Parallel()
	ctx, store, pool := openIsolatedTestStore(t)
	// Both NULL: no SLA columns set. Also covers sla_guarantee_secs=0.
	cheap, dear, _, taskID := setupSLADeferFixture(t, ctx, store, pool, nil, nil)

	got := claimAsSLA(t, ctx, store, dear)
	if got != nil && got.TaskID == taskID {
		t.Fatalf("no-SLA task went to the $%.2f/hr worker while $%.2f/hr was online; regression of unconditional deferral",
			dear.askUSDHr, cheap.askUSDHr)
	}
	got = claimAsSLA(t, ctx, store, cheap)
	if got == nil || got.TaskID != taskID {
		t.Fatalf("cheapest eligible worker was not given the no-SLA task; got %+v want %s", got, taskID)
	}
}

// 5. The bound still holds: after askDeferralWindow an SLA-less task is claimable by anyone.
func TestClaimTasksTxAskDeferralBoundStillHoldsWithoutSLA(t *testing.T) {
	t.Parallel()
	ctx, store, pool := openIsolatedTestStore(t)
	cheap, dear, _, taskID := setupSLADeferFixture(t, ctx, store, pool, nil, nil)

	// Confirm the hold engages first.
	got := claimAsSLA(t, ctx, store, dear)
	if got != nil && got.TaskID == taskID {
		t.Fatalf("precondition: young no-SLA task must be deferred; dear claimed it")
	}
	_ = cheap

	// Age the task past the window; re-queue if anything touched it.
	if _, err := pool.Exec(ctx,
		`UPDATE tasks SET created_at = now() - $2::interval, visible_at = NULL,
		        status='queued', claimed_by=NULL, worker_id=NULL,
		        claimed_at=NULL, started_at=NULL
		   WHERE id = $1`, taskID, (askDeferralWindow + 5*time.Second).String()); err != nil {
		t.Fatal(err)
	}
	aged := claimAsSLA(t, ctx, store, dear)
	if aged == nil || aged.TaskID != taskID {
		t.Fatal("task stayed deferred past askDeferralWindow: a cheap worker that never polls would starve the queue")
	}
}
