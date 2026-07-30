package main

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
)

// A supplier's asking price must change who gets work, not merely who is
// eligible for it.
//
// This is the first test to drive ClaimTasksTx at all -- the scheduler's hot
// path had no test caller, so every claim-ordering claim in this repository was
// previously asserted by string-matching the generated SQL rather than by
// running it.
func TestClaimTasksTxDefersToACheaperAskingWorker(t *testing.T) {
	databaseURL := requireTestDatabase(t)
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	pool, err := pgxpool.New(ctx, databaseURL)
	if err != nil {
		t.Fatal(err)
	}
	defer pool.Close()
	store := NewStore(pool)
	if err := store.Migrate(ctx); err != nil {
		t.Fatal(err)
	}

	suffix := uuid.NewString()
	buyerID, err := store.CreateBuyerAccount(ctx, "ask-"+suffix+"@example.test", "integration-password", 100)
	if err != nil {
		t.Fatal(err)
	}

	// Two suppliers, identical in every respect the scheduler filters on, except
	// what they ask to be paid.
	type worker struct {
		supplierID, workerID uuid.UUID
		askUSDHr             float64
	}
	mk := func(ask float64) worker {
		w := worker{supplierID: uuid.New(), workerID: uuid.New(), askUSDHr: ask}
		if _, err := pool.Exec(ctx,
			`INSERT INTO suppliers (id,email,status,reputation,completed_tasks)
			 VALUES ($1,$2,'active',0.95,100)`,
			w.supplierID, "ask-sup-"+uuid.NewString()+"@example.test"); err != nil {
			t.Fatal(err)
		}
		if _, err := pool.Exec(ctx,
			`INSERT INTO workers (id,supplier_id,hw_class,memory_gb,effective_memory_gb,
			                      last_seen_at,throttled,min_payout_usd_hr)
			 VALUES ($1,$2,'apple_silicon_max',64,64,now(),false,$3)`,
			w.workerID, w.supplierID, ask); err != nil {
			t.Fatal(err)
		}
		bindWorkerToGovernedProfile(t, pool, ctx, w.workerID)
		if _, err := pool.Exec(ctx,
			`INSERT INTO worker_authorized_capabilities
			   (worker_id,cell_id,runtime_id,job_type,model_ref,model_kind,matrix_sha256)
			 VALUES ($1,'cell','rt','embed','all-minilm-l6-v2','hf',$2)`,
			w.workerID, generatedRuntimeMatrixSHA256); err != nil {
			t.Fatal(err)
		}
		return w
	}
	cheap := mk(0.10)
	dear := mk(5.00)

	jobID := uuid.New()
	taskID := uuid.New()
	if _, err := pool.Exec(ctx, `
		INSERT INTO jobs (id,buyer_id,status,job_type,model_ref,input_ref,task_count,
		                  offered_rate_usd_hr,min_memory_gb,tier)
		VALUES ($1,$2,'running','embed','all-minilm-l6-v2','in',1,10.0,0,'batch')`,
		jobID, buyerID); err != nil {
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
		for _, w := range []worker{cheap, dear} {
			_, _ = pool.Exec(c, `DELETE FROM worker_authorized_capabilities WHERE worker_id=$1`, w.workerID)
			_, _ = pool.Exec(c, `DELETE FROM workers WHERE id=$1`, w.workerID)
			_, _ = pool.Exec(c, `DELETE FROM suppliers WHERE id=$1`, w.supplierID)
		}
	})

	claim := func(w worker) (*ClaimedTask, error) {
		return store.ClaimTasksTx(ctx, WorkerAuth{WorkerID: w.workerID, SupplierID: w.supplierID})
	}

	// The expensive worker asks first.  A cheaper, equally-capable worker is
	// online and the job can afford it, so the task must not go to the dear one.
	got, err := claim(dear)
	if err != nil {
		t.Fatalf("expensive worker claim: %v", err)
	}
	if got != nil {
		t.Fatalf("task went to the worker asking $%.2f/hr while one asking $%.2f/hr was online and eligible",
			dear.askUSDHr, cheap.askUSDHr)
	}

	// The cheaper worker takes it.
	got, err = claim(cheap)
	if err != nil {
		t.Fatalf("cheap worker claim: %v", err)
	}
	if got == nil {
		t.Fatal("cheapest eligible worker was not given the task")
	}
	if got.TaskID != taskID {
		t.Fatalf("claimed task = %s, want %s", got.TaskID, taskID)
	}

	// The hold must expire: a cheap worker that advertises itself and never polls
	// cannot be allowed to starve the queue.  Age the task past the window and the
	// expensive worker becomes eligible again.
	if _, err := pool.Exec(ctx,
		`UPDATE tasks SET created_at = now() - $2::interval, visible_at = NULL
		   WHERE id = $1`, taskID, (askDeferralWindow + 5*time.Second).String()); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx,
		`UPDATE tasks SET status='queued', claimed_by=NULL, worker_id=NULL,
		        claimed_at=NULL, started_at=NULL WHERE id=$1`, taskID); err != nil {
		t.Fatal(err)
	}
	aged, err := claim(dear)
	if err != nil {
		t.Fatalf("expensive worker claim after the window: %v", err)
	}
	if aged == nil {
		t.Fatal("task stayed deferred past askDeferralWindow: a cheap worker that never polls would starve the queue")
	}
}
