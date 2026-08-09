package main

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"
)

func TestPlacementSnapshotAndClaimSharePayoutAndRuntimeEligibility(t *testing.T) {
	ctx, store, pool := openMoneyPathStore(t)
	fixture := seedMoneyPathFixture(t, ctx, store, pool, moneyPathSeedOpts{TaskCount: 1})
	tasks := makeTasks(fixture, 1)
	fixture.TaskIDs = []uuid.UUID{tasks[0].ID}
	job := validJobRow(t, fixture, tasks)

	candidate := job.WorkloadDecision.RuntimeCandidates[0]
	placement := job.PlacementRequirement
	if len(placement.HWClasses) != 1 {
		t.Fatalf("placement hardware classes=%v, want one exact benchmarked class", placement.HWClasses)
	}
	// Every worker starts unaffordable, so the floor has to be stated relative to
	// what this job actually offers. It used to be the literal 10, which only
	// exceeded the offered rate while that rate came from a hardcoded throughput
	// constant; the moment admission started deriving throughput from a benchmark
	// the sentinel stopped separating anything.
	unaffordableFloor := job.OfferedRateUsdHr * 2
	for _, workerID := range []uuid.UUID{fixture.WorkerID, fixture.OtherWorkerID} {
		if _, err := pool.Exec(ctx, `
			UPDATE workers
			   SET min_payout_usd_hr=$3,engine=$2,last_seen_at=now(),throttled=false,
			       hw_class=$4,build_hash=$5,hardware_identity=$6
			 WHERE id=$1`,
			workerID, candidate.Engine, unaffordableFloor, placement.HWClasses[0],
			placement.EngineBuildHash, placement.HardwareIdentity); err != nil {
			t.Fatal(err)
		}
		if _, err := pool.Exec(ctx, `
			UPDATE worker_authorized_capabilities
			   SET cell_id=$2,runtime_id=$3,model_kind=$4,matrix_sha256=$5
			 WHERE worker_id=$1 AND job_type=$6 AND model_ref=$7`,
			workerID, candidate.CellID, candidate.RuntimeID, job.WorkloadDecision.Binding.Model.Kind,
			generatedRuntimeMatrixSHA256, job.JobType, job.ModelRef); err != nil {
			t.Fatal(err)
		}
	}

	var extraWorkers []uuid.UUID
	var extraSuppliers []uuid.UUID
	for i := 0; i < 3; i++ {
		supplierID, workerID := uuid.New(), uuid.New()
		extraSuppliers = append(extraSuppliers, supplierID)
		extraWorkers = append(extraWorkers, workerID)
		if _, err := pool.Exec(ctx, `
			INSERT INTO suppliers (id,email,status,reputation,completed_tasks)
			VALUES ($1,$2,'active',0.95,100)`,
			supplierID, "placement-"+supplierID.String()+"@example.test"); err != nil {
			t.Fatal(err)
		}
		if _, err := pool.Exec(ctx, `
			INSERT INTO workers (
			  id,supplier_id,hw_class,memory_gb,effective_memory_gb,last_seen_at,
			  throttled,min_payout_usd_hr,engine,build_hash,hardware_identity
			) VALUES ($1,$2,$5,64,64,now(),false,$4,$3,$6,$7)`,
			workerID, supplierID, candidate.Engine, unaffordableFloor,
			placement.HWClasses[0], placement.EngineBuildHash, placement.HardwareIdentity); err != nil {
			t.Fatal(err)
		}
		bindWorkerToGovernedProfile(t, pool, ctx, workerID)
		if _, err := pool.Exec(ctx, `
			INSERT INTO worker_authorized_capabilities (
			  worker_id,cell_id,runtime_id,job_type,model_ref,model_kind,matrix_sha256
			) VALUES ($1,$2,$3,$4,$5,$6,$7)`,
			workerID, candidate.CellID, candidate.RuntimeID, job.JobType, job.ModelRef,
			job.WorkloadDecision.Binding.Model.Kind, generatedRuntimeMatrixSHA256); err != nil {
			t.Fatal(err)
		}
	}
	// A cheaper worker with the same job/model/matrix but the wrong frozen
	// runtime must not be counted by planning or trigger the claim engine's
	// cheaper-ask deferral. This is the adversarial shape that previously made a
	// second suite run intermittently return no task despite one exact worker.
	decoySupplierID, decoyWorkerID := uuid.New(), uuid.New()
	extraSuppliers = append(extraSuppliers, decoySupplierID)
	extraWorkers = append(extraWorkers, decoyWorkerID)
	if _, err := pool.Exec(ctx, `
		INSERT INTO suppliers (id,email,status,reputation,completed_tasks)
		VALUES ($1,$2,'active',0.95,100)`,
		decoySupplierID, "placement-decoy-"+decoySupplierID.String()+"@example.test"); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `
		INSERT INTO workers (
		  id,supplier_id,hw_class,memory_gb,effective_memory_gb,last_seen_at,
		  throttled,min_payout_usd_hr,engine,build_hash,hardware_identity
		) VALUES ($1,$2,$4,64,64,now(),false,0,$3,$5,$6)`,
		decoyWorkerID, decoySupplierID, candidate.Engine, placement.HWClasses[0],
		placement.EngineBuildHash, placement.HardwareIdentity); err != nil {
		t.Fatal(err)
	}
	bindWorkerToGovernedProfile(t, pool, ctx, decoyWorkerID)
	if _, err := pool.Exec(ctx, `
		INSERT INTO worker_authorized_capabilities (
		  worker_id,cell_id,runtime_id,job_type,model_ref,model_kind,matrix_sha256
		) VALUES ($1,'wrong-cell','wrong-runtime',$2,$3,$4,$5)`,
		decoyWorkerID, job.JobType, job.ModelRef,
		job.WorkloadDecision.Binding.Model.Kind, generatedRuntimeMatrixSHA256); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		cleanupCtx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
		defer cancel()
		// Age offline first: workers is ON DELETE RESTRICT from several tables,
		// and a leftover live worker changes later placement fleet counts and
		// cheaper_ask deferral. Discarding cleanup errors is not a cleanup.
		for _, workerID := range extraWorkers {
			if _, err := pool.Exec(cleanupCtx,
				`UPDATE workers SET last_seen_at = now() - interval '10 minutes' WHERE id=$1`,
				workerID); err != nil {
				t.Errorf("age placement worker offline: %v", err)
			}
			if _, err := pool.Exec(cleanupCtx,
				`DELETE FROM worker_authorized_capabilities WHERE worker_id=$1`, workerID); err != nil {
				t.Errorf("cleanup worker_authorized_capabilities: %v", err)
			}
		}
	})

	mustf(t, store.SubmitJobTx(ctx, job, tasks), "submit placement-parity job: %v")
	rebuiltPlacement, err := placementRequirementFor(jobSubmit{
		JobType:       job.WorkloadDecision.Binding.JobType,
		Model:         job.WorkloadDecision.Binding.Model,
		Constraints:   job.WorkloadDecision.Binding.Constraints,
		Tier:          job.WorkloadDecision.Binding.Tier,
		MinReputation: job.WorkloadDecision.Binding.MinReputation,
	}, job.WorkloadDecision, job.OfferedRateUsdHr)
	must(t, err)
	placement = rebuiltPlacement
	strict := placement.supplyRequirements()
	relaxed := strict
	relaxed.OfferedRate = nil

	if n, err := store.EligibleWorkerCountFor(ctx, job.JobType, job.ModelRef, relaxed); err != nil {
		t.Fatal(err)
	} else if n != 5 {
		t.Fatalf("runtime-capable live fleet=%d, want 5 before payout-floor filtering", n)
	}
	if n, err := store.EligibleWorkerCountFor(ctx, job.JobType, job.ModelRef, strict); err != nil {
		t.Fatal(err)
	} else if n != 0 {
		t.Fatalf("quote capacity counted %d workers that all reject the offered rate", n)
	}
	if rows, err := store.FleetRateSnapshotFor(ctx, job.JobType, job.ModelRef, strict); err != nil {
		t.Fatal(err)
	} else if len(rows) != 0 {
		t.Fatalf("planner capacity counted %d workers that all reject the offered rate", len(rows))
	}
	if sla := deriveQuoteSLA(false, true, 100, 1); sla != nil {
		t.Fatalf("zero-claimable placement produced an SLA: %+v", sla)
	}
	if claimed, err := store.ClaimTasksTx(ctx, WorkerAuth{
		WorkerID: fixture.WorkerID, SupplierID: fixture.SupplierID,
	}); err != nil {
		t.Fatal(err)
	} else if claimed != nil {
		t.Fatalf("claim engine ignored the same payout floor: %+v", claimed)
	}

	for _, tc := range []struct {
		name, build, hardware string
	}{
		{"missing build", "", placement.HardwareIdentity},
		{"wrong build", "0000000000000000", placement.HardwareIdentity},
		{"missing device", placement.EngineBuildHash, ""},
		{"wrong device", placement.EngineBuildHash, "Apple M1 Ultra"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := pool.Exec(ctx, `
				UPDATE workers
				   SET min_payout_usd_hr=$2,build_hash=$3,hardware_identity=$4
				 WHERE id=$1`, fixture.WorkerID, job.OfferedRateUsdHr/2, tc.build, tc.hardware); err != nil {
				t.Fatal(err)
			}
			if n, err := store.EligibleWorkerCountFor(ctx, job.JobType, job.ModelRef, strict); err != nil {
				t.Fatal(err)
			} else if n != 0 {
				t.Fatalf("quote capacity counted %d workers with %s", n, tc.name)
			}
			if claimed, err := store.ClaimTasksTx(ctx, WorkerAuth{
				WorkerID: fixture.WorkerID, SupplierID: fixture.SupplierID,
			}); err != nil {
				t.Fatal(err)
			} else if claimed != nil {
				t.Fatalf("claim accepted worker with %s: %+v", tc.name, claimed)
			}
		})
	}
	if _, err := pool.Exec(ctx, `
		UPDATE workers
		   SET min_payout_usd_hr=$2,build_hash=$3,hardware_identity=$4
		 WHERE id=$1`, fixture.WorkerID, job.OfferedRateUsdHr/2,
		placement.EngineBuildHash, placement.HardwareIdentity); err != nil {
		t.Fatal(err)
	}
	if n, err := store.EligibleWorkerCountFor(ctx, job.JobType, job.ModelRef, strict); err != nil {
		t.Fatal(err)
	} else if n != 1 {
		t.Fatalf("strict quote capacity=%d after one worker became affordable, want 1", n)
	}
	if rows, err := store.FleetRateSnapshotFor(ctx, job.JobType, job.ModelRef, strict); err != nil {
		t.Fatal(err)
	} else if len(rows) != 1 || rows[0].WorkerID != fixture.WorkerID {
		t.Fatalf("planner snapshot did not select the one claimable worker: %+v", rows)
	}
	if claimed, err := store.ClaimTasksTx(ctx, WorkerAuth{
		WorkerID: fixture.WorkerID, SupplierID: fixture.SupplierID,
	}); err != nil {
		t.Fatal(err)
	} else if claimed == nil || claimed.TaskID != tasks[0].ID {
		t.Fatalf("claim engine disagreed with quote/planner eligibility: %+v", claimed)
	}
}
