package main

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/google/uuid"
)

// Exact-result reuse needs execution_contracts.worker_id / supplier_id /
// upstream_* to be nullable: a cache hit settles money without reserving
// capacity, so there is no worker to bind and no supplier to credit.
//
// Dropping NOT NULL to allow that also allows the failure a scheduling bug
// produces -- a contract with no worker and no supplier, but positive supplier
// rates, so money is owed to nobody. The binding-shape constraint pins the nulls
// to the only case entitled to them. These assertions are what stops a future
// migration from quietly dropping it.
func TestExecutionContractBindingShapeIsEnforced(t *testing.T) {
	ctx, store, _ := openAdminMutationTestStore(t)

	buyerID := uuid.New()
	if _, err := store.pool.Exec(ctx,
		`INSERT INTO buyers (id,email) VALUES ($1,$2) ON CONFLICT DO NOTHING`,
		buyerID, "binding-"+buyerID.String()+"@example.invalid"); err != nil {
		t.Fatalf("seed buyer: %v", err)
	}

	// Insert with explicit control over the four bindings and the supplier rates.
	insert := func(worker, supplier any, upstream, sealed any, supRate float64) error {
		var placementJSON any
		var placementSHA any
		if worker != nil {
			plan := RealtimePlacementPlan{
				Version:          realtimePlacementPlanVersion,
				RuntimeProfileID: "rp", RuntimeProfileSHA256: strings.Repeat("a", 64),
				HWClass: "nvidia_24gb", GPUCount: 1, MemoryGBPerGPU: 24,
				ConfiguredTensorParallel: 1, AdmittedTensorParallel: 1,
				AdmissionBasis: realtimePlacementTopologyOnly,
				Rationale:      "test single-GPU placement",
			}
			raw, err := json.Marshal(plan)
			if err != nil {
				return err
			}
			digest, err := realtimePlacementPlanDigest(plan)
			if err != nil {
				return err
			}
			placementJSON, placementSHA = raw, digest
		}
		_, err := store.pool.Exec(ctx, `
			INSERT INTO execution_contracts
			 (id,request_id,buyer_id,workload_type,route,model_alias,runtime_profile_id,
			  runtime_profile_sha256,placement_plan,placement_plan_sha256,
			  input_commitment,request_sha256,maximum_price_usd,
			  estimated_price_usd,buyer_input_usd_per_million_tokens,
			  buyer_output_usd_per_million_tokens,supplier_input_usd_per_million_tokens,
			  supplier_output_usd_per_million_tokens,deadline_at,verification_tier,
			  state,worker_id,supplier_id,upstream_base_url,upstream_token_sealed,
			  finalized_at)
			VALUES ($1,$2,$3,'CHAT_COMPLETION','/v1/chat/completions','m','rp',
			        repeat('a',64),$4,$5,repeat('b',64),repeat('c',64),
			        1.0,1.0,1.0,1.0,$6,$6,now()+interval '1 hour','V0',
			        'VERIFIED',$7,$8,$9,$10,now())`,
			uuid.New(), "req-"+uuid.NewString(), buyerID,
			placementJSON, placementSHA, supRate,
			worker, supplier, upstream, sealed)
		return err
	}

	// A reuse contract: nothing bound, nothing owed to a supplier. Allowed.
	if err := insert(nil, nil, nil, nil, 0); err != nil {
		t.Fatalf("a well-formed exact-reuse contract was rejected: %v", err)
	}

	// The dangerous shape: no worker and no supplier, but supplier rates are
	// positive. Money is owed and there is nobody to owe it to.
	if err := insert(nil, nil, nil, nil, 1.0); err == nil {
		t.Fatal("a contract with no supplier bound but positive supplier rates was " +
			"accepted: that is money owed to nobody")
	}

	// Half-bound: an upstream present while worker and supplier are null. This is
	// the shape a scheduler bug produces when it drops the binding partway.
	if err := insert(nil, nil, "https://upstream.invalid", "enc:x", 0); err == nil {
		t.Fatal("a half-bound contract (upstream set, worker and supplier null) was accepted")
	}

	// A real contract needs a real worker, so bind one and prove the positive
	// case still works -- otherwise the constraint could be rejecting everything
	// and these assertions would prove nothing.
	supplierID, workerID := uuid.New(), uuid.New()
	// No t.Skip: the positive case is what proves the constraint is not simply
	// rejecting everything, so a seed failure must fail the test.
	if _, err := store.pool.Exec(ctx,
		`INSERT INTO suppliers (id,email,status) VALUES ($1,$2,'active')
		 ON CONFLICT DO NOTHING`,
		supplierID, "supplier-"+supplierID.String()+"@example.invalid"); err != nil {
		t.Fatalf("seed supplier: %v", err)
	}
	if _, err := store.pool.Exec(ctx,
		`INSERT INTO workers (id,supplier_id,hw_class) VALUES ($1,$2,'nvidia_24gb')
		 ON CONFLICT DO NOTHING`, workerID, supplierID); err != nil {
		t.Fatalf("seed worker: %v", err)
	}
	if _, err := store.pool.Exec(ctx, `
		INSERT INTO realtime_worker_offers
		 (worker_id,supplier_id,runtime_profile_id,runtime_profile_sha256,
		  upstream_base_url,upstream_token_sealed,warmth,max_active_sequences,
		  available_sequences,supplier_input_usd_per_million_tokens,
		  supplier_output_usd_per_million_tokens,status)
		VALUES ($1,$2,'rp-no-placement',repeat('a',64),
		        'https://upstream.invalid','enc:x','HOT',1,1,0,0,'ACTIVE')`,
		workerID, supplierID); err == nil {
		t.Fatal("database accepted an ACTIVE realtime offer without placement authority")
	}
	if err := insert(workerID, supplierID, "https://upstream.invalid", "enc:x", 1.0); err != nil {
		t.Fatalf("a fully bound contract was rejected: %v", err)
	}
}
