package main

import (
	"strings"
	"testing"

	"github.com/google/uuid"
)

// TestRuntimeShadowSchemaSurvivesApplicationRollbackRoundTrip proves the
// expand/contract seam the production rollback script depends on:
//
//	current schema -> previous binary reapplies its embedded constraints and
//	writes a legacy row -> current schema reapplies.
//
// The two schema generations retain their own columns. Neither migration is
// allowed to rewrite an immutable decision into the other generation's terms.
func TestRuntimeShadowSchemaSurvivesApplicationRollbackRoundTrip(t *testing.T) {
	ctx, store, pool := openIsolatedTestStore(t)
	matrix := strings.Repeat("a", 64)
	currentID := uuid.New()
	legacyID := uuid.New()

	// A current decision leaves the previous binary's projection blank. That is
	// what lets its embedded basis/cost constraints be reinstalled during an
	// application-only rollback.
	_, err := pool.Exec(ctx, `
		INSERT INTO runtime_shadow_selections
		  (job_id, runtime_matrix_sha256, policy_revision, job_type, model_ref,
		   model_kind, workload_class, latency_class, routed_cell_id, shadow_cell_id,
		   considered_cells, excluded_cells, selection_policy, selection_basis_v3,
		   supplier_liability_hw_class, supplier_liability_hardware_identity)
		VALUES ($1,$2,1,'embed','all-minilm-l6-v2','hf','batch_embedding','standard_batch',
		        'routed','shadow','[]'::jsonb,'[]'::jsonb,
		        'eligibility-and-measured-supplier-liability-v3',
		        'MORE_THROUGHPUT_AT_EQUAL_SUPPLIER_LIABILITY','apple_silicon_ultra',
		        'Apple M3 Ultra')`,
		currentID, matrix)
	mustf(t, err, "insert current shadow decision: %v")

	// Exact compatibility constraints embedded by the previous application
	// generation. Reinstalling them must accept every current row because current
	// code does not overload the legacy columns.
	_, err = pool.Exec(ctx, `
		ALTER TABLE runtime_shadow_selections
		  DROP CONSTRAINT IF EXISTS runtime_shadow_selections_basis_known;
		ALTER TABLE runtime_shadow_selections
		  ADD CONSTRAINT runtime_shadow_selections_basis_known
		  CHECK (selection_basis IN ('', 'LIFECYCLE_LADDER', 'MEASURED_VERIFIED_OUTCOME_COST'));
		ALTER TABLE runtime_shadow_selections
		  DROP CONSTRAINT IF EXISTS runtime_shadow_selections_cost_scope;
		ALTER TABLE runtime_shadow_selections
		  ADD CONSTRAINT runtime_shadow_selections_cost_scope
		  CHECK ((selection_basis = 'MEASURED_VERIFIED_OUTCOME_COST') =
		         (btrim(cost_hw_class) <> ''))`)
	mustf(t, err, "previous schema constraints rejected current rows: %v")

	// While rolled back, the previous binary may legitimately append decisions.
	// Its row remains legacy on the next upgrade; migration must not relabel it as
	// though v3 had made the decision.
	_, err = pool.Exec(ctx, `
		INSERT INTO runtime_shadow_selections
		  (job_id, runtime_matrix_sha256, policy_revision, job_type, model_ref,
		   model_kind, workload_class, latency_class, routed_cell_id, shadow_cell_id,
		   considered_cells, excluded_cells, selection_policy, selection_basis,
		   cost_hw_class)
		VALUES ($1,$2,1,'embed','all-minilm-l6-v2','hf','batch_embedding','standard_batch',
		        'routed','legacy-shadow','[]'::jsonb,'[]'::jsonb,
		        'eligibility-and-measured-cost-v2',
		        'MEASURED_VERIFIED_OUTCOME_COST','apple_silicon_ultra')`,
		legacyID, matrix)
	mustf(t, err, "previous binary could not append its legacy decision: %v")

	mustf(t, store.Migrate(ctx), "current schema could not reapply after rollback: %v")

	var legacyBasis, legacyHW, currentBasis, currentHW, currentHardwareIdentity string
	if err := pool.QueryRow(ctx, `
		SELECT selection_basis, cost_hw_class, selection_basis_v3,
		       supplier_liability_hw_class, supplier_liability_hardware_identity
		  FROM runtime_shadow_selections WHERE job_id=$1`, currentID).
		Scan(&legacyBasis, &legacyHW, &currentBasis, &currentHW,
			&currentHardwareIdentity); err != nil {
		t.Fatal(err)
	}
	if legacyBasis != "" || legacyHW != "" ||
		currentBasis != "MORE_THROUGHPUT_AT_EQUAL_SUPPLIER_LIABILITY" ||
		currentHW != "apple_silicon_ultra" || currentHardwareIdentity != "Apple M3 Ultra" {
		t.Fatalf("current row crossed schema generations: legacy=%q/%q current=%q/%q",
			legacyBasis, legacyHW, currentBasis, currentHW)
	}

	if err := pool.QueryRow(ctx, `
		SELECT selection_basis, cost_hw_class, selection_basis_v3,
		       supplier_liability_hw_class, supplier_liability_hardware_identity
		  FROM runtime_shadow_selections WHERE job_id=$1`, legacyID).
		Scan(&legacyBasis, &legacyHW, &currentBasis, &currentHW,
			&currentHardwareIdentity); err != nil {
		t.Fatal(err)
	}
	if legacyBasis != "MEASURED_VERIFIED_OUTCOME_COST" ||
		legacyHW != "apple_silicon_ultra" || currentBasis != "" || currentHW != "" ||
		currentHardwareIdentity != "" {
		t.Fatalf("legacy row was rewritten as current authority: legacy=%q/%q current=%q/%q",
			legacyBasis, legacyHW, currentBasis, currentHW)
	}

	// A row cannot claim both interpretations. The append-only trigger makes a
	// stored mistake permanent, so the insert must fail closed.
	_, err = pool.Exec(ctx, `
		INSERT INTO runtime_shadow_selections
		  (job_id, runtime_matrix_sha256, policy_revision, job_type, model_ref,
		   model_kind, workload_class, latency_class, routed_cell_id, shadow_cell_id,
		   considered_cells, excluded_cells, selection_policy, selection_basis,
		   selection_basis_v3)
		VALUES ($1,$2,1,'embed','all-minilm-l6-v2','hf','batch_embedding','standard_batch',
		        'routed','shadow','[]'::jsonb,'[]'::jsonb,'invalid-dual-authority',
		        'LIFECYCLE_LADDER','LIFECYCLE_LADDER')`, uuid.New(), matrix)
	if err == nil {
		t.Fatal("database accepted both legacy and current selector authority on one row")
	}
}
