package main

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
)

// A claiming rented NVIDIA worker must prefer work no cheaper class can take when
// an owned Mac is idle and eligible. Claim $3 is hwClassCostRank(self); the
// cheaper-class EXISTS ranks rivals in SQL. When the Go twin under-ranked CUDA
// as 0, EXISTS was always false and the rented worker never stepped back.
func TestClaimingNVIDIADefersSharedWorkToIdleOwnedMac(t *testing.T) {
	previousActivation := activeRuntimeActivation.Load()
	t.Cleanup(func() { activeRuntimeActivation.Store(previousActivation) })
	databaseURL := requireTestDatabase(t)
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	pool, err := pgxpool.New(ctx, databaseURL)
	must(t, err)
	defer pool.Close()
	store := NewStore(pool)
	must(t, store.Migrate(ctx))

	buyerID, err := store.CreateBuyerAccount(ctx,
		"hw-defer-"+uuid.NewString()+"@example.test", "integration-password", 100)
	must(t, err)

	// Unique model_ref so leftover workers from the shared test DB cannot match
	// the cheaper-class EXISTS (or claim eligibility) for these jobs.
	modelRef := "hw-cost-rank-" + uuid.NewString()

	type worker struct {
		supplierID, workerID uuid.UUID
		hwClass              string
	}
	mk := func(hwClass string) worker {
		w := worker{supplierID: uuid.New(), workerID: uuid.New(), hwClass: hwClass}
		if _, err := pool.Exec(ctx,
			`INSERT INTO suppliers (id,email,status,reputation,completed_tasks)
			 VALUES ($1,$2,'active',0.95,100)`,
			w.supplierID, "hw-sup-"+uuid.NewString()+"@example.test"); err != nil {
			t.Fatal(err)
		}
		// Claim freezes execution identity from the worker row. Provide the
		// governed build/device columns so the claim transition can write them.
		if _, err := pool.Exec(ctx,
			`INSERT INTO workers (id,supplier_id,hw_class,hardware_identity,memory_gb,effective_memory_gb,
			                      last_seen_at,throttled,min_payout_usd_hr,engine,build_hash,build_identity_policy)
			 VALUES ($1,$2,$3,$4,80,80,now(),false,0,'candle',$5,$6)`,
			w.workerID, w.supplierID, hwClass, testOnlyHardwareIdentity,
			testOnlyEngineBuildHash, currentEngineBuildIdentityPolicy); err != nil {
			t.Fatal(err)
		}
		bindWorkerToGovernedProfile(t, pool, ctx, w.workerID)
		if _, err := pool.Exec(ctx,
			`INSERT INTO worker_authorized_capabilities
			   (worker_id,cell_id,runtime_id,job_type,model_ref,model_kind,matrix_sha256,routable)
			 VALUES ($1,'cell','rt','embed',$2,'hf',$3,true)`,
			w.workerID, modelRef, generatedRuntimeMatrixSHA256); err != nil {
			t.Fatal(err)
		}
		return w
	}

	ultra := mk("apple_silicon_ultra")
	nvidia := mk("nvidia_80gb")

	// Job A (older): both classes eligible. Job B (newer): nvidia-only pin.
	// With a correct selfCostRank the NVIDIA claimer sees cheaper_class_online on
	// A and prefers B. With selfCostRank 0 both look equal and A wins by age —
	// the rented card takes work the idle Mac should have had.
	jobA, taskA := uuid.New(), uuid.New()
	jobB, taskB := uuid.New(), uuid.New()
	if _, err := pool.Exec(ctx, `
		INSERT INTO jobs (id,buyer_id,status,job_type,model_ref,input_ref,task_count,
		                  offered_rate_usd_hr,min_memory_gb,tier,created_at)
		VALUES ($1,$2,'running','embed',$3,'in',1,10.0,0,'batch',
		        now() - interval '2 minutes')`,
		jobA, buyerID, modelRef); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `
		INSERT INTO tasks (id,job_id,status,input_ref,result_key,created_at)
		VALUES ($1,$2,'queued','in','rk',now() - interval '2 minutes')`,
		taskA, jobA); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `
		INSERT INTO jobs (id,buyer_id,status,job_type,model_ref,input_ref,task_count,
		                  offered_rate_usd_hr,min_memory_gb,tier,hw_classes,created_at)
		VALUES ($1,$2,'running','embed',$3,'in',1,10.0,0,'batch',
		        ARRAY['nvidia_80gb'], now() - interval '1 minute')`,
		jobB, buyerID, modelRef); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `
		INSERT INTO tasks (id,job_id,status,input_ref,result_key,created_at)
		VALUES ($1,$2,'queued','in','rk',now() - interval '1 minute')`,
		taskB, jobB); err != nil {
		t.Fatal(err)
	}

	t.Cleanup(func() {
		c, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cancel()
		for _, jobID := range []uuid.UUID{jobA, jobB} {
			_, _ = pool.Exec(c, `DELETE FROM tasks WHERE job_id=$1`, jobID)
			_, _ = pool.Exec(c, `DELETE FROM jobs WHERE id=$1`, jobID)
		}
		for _, w := range []worker{ultra, nvidia} {
			_, _ = pool.Exec(c, `DELETE FROM worker_authorized_capabilities WHERE worker_id=$1`, w.workerID)
			_, _ = pool.Exec(c, `DELETE FROM workers WHERE id=$1`, w.workerID)
			_, _ = pool.Exec(c, `DELETE FROM suppliers WHERE id=$1`, w.supplierID)
		}
	})

	// Sanity: the money bug is selfCostRank for the claiming class. If Go still
	// ranks nvidia as free, the rest of this test is explaining a symptom of that.
	selfRank := hwClassCostRank("nvidia_80gb")
	ultraRank := hwClassCostRank("apple_silicon_ultra")
	if selfRank <= ultraRank {
		t.Fatalf("claiming nvidia_80gb selfCostRank=%d is not above idle apple_silicon_ultra rank=%d: "+
			"EXISTS (cheaper class) can never fire, so a rented GPU never defers to an owned Mac",
			selfRank, ultraRank)
	}

	// Direct evaluation of the claim predicate's cheaper-class EXISTS with the
	// same $3 binding ClaimTasksTx uses. This is the money wire: if it is false
	// while ultra is idle and eligible, ORDER BY never reorders.
	var cheaperOnShared bool
	q := `SELECT EXISTS (
		SELECT 1 FROM workers w2
		  JOIN suppliers s2 ON s2.id = w2.supplier_id
		WHERE w2.id <> $1
		  AND w2.last_seen_at IS NOT NULL
		  AND w2.last_seen_at > now() - interval '60 seconds'
		  AND s2.status = 'active'
		  AND NOT COALESCE(w2.throttled, false)
		  AND (` + hwClassCostRankSQL("w2.hw_class") + `) < $2
		  AND COALESCE(0,0) <= COALESCE(w2.effective_memory_gb, w2.memory_gb, 0)
		  AND EXISTS (
		    SELECT 1 FROM worker_authorized_capabilities wac2
		     WHERE wac2.worker_id = w2.id
		       AND wac2.job_type = 'embed'
		       AND wac2.model_ref = $3
		       AND wac2.matrix_sha256 = $4
		       AND wac2.routable
		  )
	)`
	if err := pool.QueryRow(ctx, q, nvidia.workerID, selfRank, modelRef, generatedRuntimeMatrixSHA256).
		Scan(&cheaperOnShared); err != nil {
		t.Fatalf("cheaper-class EXISTS: %v", err)
	}
	if !cheaperOnShared {
		t.Fatalf("claiming nvidia_80gb (selfCostRank=%d) does not see idle apple_silicon_ultra as cheaper: "+
			"EXISTS is false, so a rented GPU never defers to an owned Mac", selfRank)
	}

	got, err := store.ClaimTasksTx(ctx, WorkerAuth{
		WorkerID: nvidia.workerID, SupplierID: nvidia.supplierID,
	})
	mustf(t, err, "nvidia claim: %v")
	if got == nil {
		t.Fatal("nvidia claimed nothing; expected the nvidia-only task")
	}
	if got.TaskID != taskB {
		t.Fatalf("nvidia claimed shared task %s while idle apple_silicon_ultra could take it "+
			"(selfCostRank=%d); want nvidia-only task %s — rented hardware did not defer to owned",
			got.TaskID, selfRank, taskB)
	}
}
