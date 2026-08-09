package main

import (
	"strings"
	"testing"

	"github.com/google/uuid"
)

// The placement decision has to survive the round trip, and a mode with no reason
// has to be refused by the database rather than by a convention.
func TestRecordedShadowSelectionCarriesItsExecutionMode(t *testing.T) {
	ctx, store, pool := openIsolatedMoneyPathStore(t)
	f := seedMoneyPathFixture(t, ctx, store, pool, moneyPathSeedOpts{TaskCount: 1})
	tasks := makeTasks(f, 1)
	f.TaskIDs = []uuid.UUID{tasks[0].ID}
	job := validJobRowDirected(t, f, tasks, candleEmbedCell)
	mustf(t, store.SubmitJobTx(ctx, job, tasks), "submit: %v")

	shadow, err := planShadowSelection(job.WorkloadDecision)
	mustf(t, err, "plan shadow selection: %v")
	shadow = shadow.withExecutionMode(job.WorkloadDecision)
	if shadow.ExecutionMode != string(ModePool) {
		t.Fatalf("execution mode = %q, want POOL: independent task fan-out over "+
			"heterogeneous machines is what the batch lane does", shadow.ExecutionMode)
	}
	if !strings.Contains(shadow.ExecutionModeReason, "no inter-worker communication") {
		t.Fatalf("reason does not state the property that decided it: %q",
			shadow.ExecutionModeReason)
	}
	// The refusals travel with it, which is what makes the placement reviewable.
	if !strings.Contains(shadow.ExecutionModeReason, "refused") {
		t.Fatalf("reason names no refused mode, so it records a label not a choice: %q",
			shadow.ExecutionModeReason)
	}

	mustf(t, store.RecordShadowSelection(ctx, f.JobID.String(), shadow), "record: %v")
	var mode, reason, topology string
	if err := pool.QueryRow(ctx, `
		SELECT execution_mode, execution_mode_reason, topology_plan::text
		  FROM runtime_shadow_selections WHERE job_id=$1`, f.JobID).
		Scan(&mode, &reason, &topology); err != nil {
		t.Fatalf("read back: %v", err)
	}
	if mode != string(ModePool) || reason != shadow.ExecutionModeReason {
		t.Fatalf("round trip lost the placement: mode=%q reason=%q", mode, reason)
	}
	if !strings.Contains(topology, `"status": "ACCEPTED"`) || !strings.Contains(topology, `"scheduler_shape": "INDEPENDENT_CHUNK_SPLIT"`) {
		t.Fatalf("round trip lost bounded topology plan: %s", topology)
	}

	// A named mode with no reason must not be storable at all.
	other := seedMoneyPathFixture(t, ctx, store, pool, moneyPathSeedOpts{TaskCount: 1})
	if _, err := pool.Exec(ctx, `
		INSERT INTO runtime_shadow_selections
		  (job_id, runtime_matrix_sha256, policy_revision, job_type, model_ref,
		   model_kind, workload_class, latency_class, routed_cell_id, shadow_cell_id,
		   considered_cells, excluded_cells, selection_policy, selection_basis_v3,
		   supplier_liability_hw_class, execution_mode, execution_mode_reason)
		VALUES ($1,$2,1,'embed','all-minilm-l6-v2','hf','batch_embedding','standard_batch',
		        'cell','cell','[]'::jsonb,'[]'::jsonb,'p','LIFECYCLE_LADDER','','POOL','')`,
		other.JobID, generatedRuntimeMatrixSHA256); err == nil {
		t.Fatal("the database stored a placement mode with no reason")
	}
}

// A tightly coupled workload has nowhere to go without a measured fabric or a
// permitted provider, and the recorded decision says so by naming no mode rather
// than by defaulting to one.
func TestTightlyCoupledWorkloadRecordsNoMode(t *testing.T) {
	decision := WorkloadDecision{
		WorkloadClass: "realtime_generation",
		Parallelism: WorkloadParallelism{
			Mode: "tensor_parallel", TensorParallelDegree: 4,
		},
	}
	shadow := ShadowSelection{}.withExecutionMode(decision)
	if shadow.ExecutionMode != "" {
		t.Fatalf("placed tightly coupled degree 4 work in %s with no measured fabric "+
			"and no provider permission", shadow.ExecutionMode)
	}
	if shadow.ExecutionModeReason != "" {
		t.Fatalf("a refused placement recorded a reason as though it had placed: %q",
			shadow.ExecutionModeReason)
	}
}
