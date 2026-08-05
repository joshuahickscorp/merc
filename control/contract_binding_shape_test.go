package main

import (
	"encoding/json"
	"errors"
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
	installSettlementCurrencyForTest(t, "usd")
	ctx, store, _ := openAdminMutationTestStore(t)

	buyerID := uuid.New()
	if _, err := store.pool.Exec(ctx,
		`INSERT INTO buyers (id,email) VALUES ($1,$2) ON CONFLICT DO NOTHING`,
		buyerID, "binding-"+buyerID.String()+"@example.invalid"); err != nil {
		t.Fatalf("seed buyer: %v", err)
	}

	// Insert with explicit control over the four bindings and the supplier rates.
	insert := func(worker, supplier any, upstream, sealed any, supRate float64) error {
		modelAlias := "m"
		runtimeProfileID := "rp"
		runtimeProfileSHA256 := strings.Repeat("a", 64)
		var placementJSON any
		var placementSHA any
		var currency = "usd"
		maximumPrice, estimatedPrice := 1.0, 1.0
		var maximumPromptTokens any
		var maximumCompletionTokens any
		var estimatedPromptTokens any
		var estimatedCompletionTokens any
		var pricingJSON any
		var pricingSHA any
		if worker != nil {
			profiles := sortedVLLMProfiles()
			if len(profiles) == 0 {
				return errors.New("no realtime profile")
			}
			profile := profiles[0]
			plan, err := newRealtimePlacementPlan(profile, RealtimeOfferRegistration{
				RuntimeProfileID: profile.RuntimeProfileID, RuntimeProfileSHA256: profile.ProfileSHA256,
				HWClass: "nvidia_80gb", GPUCount: 1, MemoryGBPerGPU: 80,
				SupplierInputUSDPerMillionTokens: supRate, SupplierOutputUSDPerMillionTokens: supRate,
			})
			if err != nil {
				return err
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
			modelAlias = profile.ModelAlias
			runtimeProfileID = profile.RuntimeProfileID
			runtimeProfileSHA256 = profile.ProfileSHA256
			maximumPromptTokens, maximumCompletionTokens = int64(10_000), int64(1_000)
			estimatedPromptTokens, estimatedCompletionTokens = int64(5_000), int64(500)
			decision, err := newRealtimePricingDecision(RealtimePricingInputs{
				Profile: profile, Placement: plan,
				InputCommitment: strings.Repeat("b", 64), RequestSHA256: strings.Repeat("c", 64),
				MaximumPromptTokens: 10_000, MaximumCompletionTokens: 1_000,
				EstimatedPromptTokens: 5_000, EstimatedCompletionTokens: 500,
				SupplierInputRate: supRate, SupplierOutputRate: supRate,
				Currency: usd(t),
			})
			if err != nil {
				return err
			}
			pricingJSON, err = json.Marshal(decision)
			if err != nil {
				return err
			}
			pricingSHA, err = pricingDecisionDigest(decision)
			if err != nil {
				return err
			}
			estimatedPrice, maximumPrice, err = realtimePricingLegacyProjection(decision)
			if err != nil {
				return err
			}
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
			  currency,maximum_prompt_tokens,maximum_completion_tokens,
			  estimated_prompt_tokens,estimated_completion_tokens,
			  pricing_decision,pricing_decision_sha256,finalized_at)
			VALUES ($1,$2,$3,'CHAT_COMPLETION','/v1/chat/completions',$4,$5,
			        $6,$7,$8,repeat('b',64),repeat('c',64),
			        $9,$10,1.0,1.0,$11,$11,now()+interval '1 hour','V0',
			        'VERIFIED',$12,$13,$14,$15,$16,$17,$18,$19,$20,$21,$22,now())`,
			uuid.New(), "req-"+uuid.NewString(), buyerID,
			modelAlias, runtimeProfileID, runtimeProfileSHA256, placementJSON, placementSHA,
			maximumPrice, estimatedPrice, supRate, worker, supplier, upstream, sealed, currency,
			maximumPromptTokens, maximumCompletionTokens, estimatedPromptTokens,
			estimatedCompletionTokens, pricingJSON, pricingSHA)
		return err
	}

	// A reuse contract: nothing bound, nothing owed to a supplier. Allowed.
	mustf(t, insert(nil, nil, nil, nil, 0), "a well-formed exact-reuse contract was rejected: %v")

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
		`INSERT INTO workers (id,supplier_id,hw_class) VALUES ($1,$2,'nvidia_80gb')
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
	mustf(t, insert(workerID, supplierID, "https://upstream.invalid", "enc:x", 0.08), "a fully bound contract was rejected: %v")
}
