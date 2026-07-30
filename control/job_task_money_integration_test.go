package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
)

// moneyPathFixture seeds the shared actors for job/task money-path tests:
// buyer, active supplier + worker with an authorized capability, and optional
// job/tasks already in a chosen state. Seeding reuses the two existing dialects
// (CreateBuyerAccount + capability inserts from scheduler_ask_claim; statement
// loops + cleanup from dispute_payout). Ids and emails are unique per run.
type moneyPathFixture struct {
	BuyerID         uuid.UUID
	SupplierID      uuid.UUID
	WorkerID        uuid.UUID
	OtherWorkerID   uuid.UUID
	OtherSupplierID uuid.UUID
	JobID           uuid.UUID
	TaskIDs         []uuid.UUID
	Plan            EconomicPlan
}

type moneyPathSeedOpts struct {
	TaskCount       int
	TaskStatus      string // queued | running | verifying | complete
	ClaimWorker     bool   // set claimed_by / execution fields for the primary worker
	SLAPremium      float64
	SeedPlanRows    bool // insert job_economic_plans + reserves for non-SubmitJobTx tests
	SeedJob         bool
	PrepaidRequired bool
}

func openMoneyPathStore(t *testing.T) (context.Context, *Store, *pgxpool.Pool) {
	t.Helper()
	// Disabling canary requires a recorded decision reference, because the same
	// switch also opens self-serve signup; one decision gates both.
	t.Setenv("MERC_CANARY_MODE", "false")
	t.Setenv("MERC_CANARY_DISABLE_DECISION_REF", "TEST-money-path")
	return openAdminMutationTestStore(t)
}

func openIsolatedMoneyPathStore(t *testing.T) (context.Context, *Store, *pgxpool.Pool) {
	t.Helper()
	t.Setenv("MERC_CANARY_MODE", "false")
	t.Setenv("MERC_CANARY_DISABLE_DECISION_REF", "TEST-money-path-isolated")
	return openIsolatedTestStore(t)
}

func TestSubmitExactReuseBatchJobFreezesWorkloadDecision(t *testing.T) {
	ctx, store, pool := openMoneyPathStore(t)
	f := seedMoneyPathFixture(t, ctx, store, pool, moneyPathSeedOpts{
		TaskCount: 1,
	})

	sub, herr := normalizeAndValidateJobSubmit(jobSubmit{
		JobType: JobType{Type: "embed"},
		Model:   ModelRef{Kind: "hf", Ref: "all-minilm-l6-v2"},
		Constraints: JobConstraints{
			MaxDurationSecs: 3600,
		},
		Tier: "batch",
	})
	if herr != nil {
		t.Fatalf("normalize exact-reuse workload: %s", herr.msg)
	}
	decision, err := buildWorkloadDecision(sub, strings.Repeat("f", 64))
	if err != nil {
		t.Fatalf("build exact-reuse workload decision: %v", err)
	}
	money := SettleReuseHitMoney(1000, 0.002)
	originPlan, err := newDistributedComputePlan(
		decision,
		1,
		128,
		testInputDepthProfile(1),
		1,
		1,
		0,
		0,
		quoteTimeFromETABands(60, 0, false),
		"static",
		0.20,
		0,
		QuoteConfidence{Score: 0.9, Reasons: []string{"exact-reuse origin fixture"}},
		nil,
	)
	if err != nil {
		t.Fatalf("build exact-reuse origin plan: %v", err)
	}
	computePlan, err := newExactReuseComputePlan(
		decision, 1, 128, testInputDepthProfile(1), microsToUSD(money.BuyerDebitMicros), &originPlan,
	)
	if err != nil {
		t.Fatalf("build exact-reuse compute plan: %v", err)
	}
	quoteID := uuid.New()
	originSHA256, err := computePlanDigest(originPlan)
	if err != nil {
		t.Fatalf("hash exact-reuse origin plan: %v", err)
	}
	originEconomic := BuildEconomicPlan(EconomicPlanInput{
		BaseComputeUSD: originPlan.BaseComputeUSD, InitialTaskCount: 1,
		ExtraTaskReserve: 1, SupplierShare: supplierShareRate,
	}, testEconomicSchedule())
	authority := catalogueAuthorityFixture(
		t, decision, originEconomic.Schedule.Currency, originEconomic.Input.SupplierShare,
	)
	originPlacement := placementForPricingFixture(t, decision, authority)
	originPricing, err := newDistributedPricingDecision(
		decision, originPlan, originPlacement, originEconomic, authority, sub.Tier, "",
	)
	if err != nil {
		t.Fatalf("build exact-reuse origin pricing: %v", err)
	}
	originPricingJSON, err := json.Marshal(originPricing)
	if err != nil {
		t.Fatal(err)
	}
	originPricingSHA256, err := pricingDecisionDigest(originPricing)
	if err != nil {
		t.Fatal(err)
	}
	originPlacementJSON, err := json.Marshal(originPlacement)
	if err != nil {
		t.Fatal(err)
	}
	originPlacementSHA256, err := placementRequirementDigest(originPlacement)
	if err != nil {
		t.Fatal(err)
	}
	workloadSHA256, err := workloadDecisionDigest(decision)
	if err != nil {
		t.Fatal(err)
	}
	quoteJSON, err := json.Marshal(map[string]any{
		"placement_requirement": originPlacement,
		"pricing_decision":      originPricing,
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `
		INSERT INTO quotes
		  (id,buyer_id,job_type,compute_plan_sha256,
		   workload_decision_sha256,
		   placement_requirement,placement_requirement_sha256,
		   pricing_decision,pricing_decision_sha256,quote_json)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10)`,
		quoteID, f.BuyerID, sub.JobType.Type, originSHA256,
		workloadSHA256, originPlacementJSON, originPlacementSHA256,
		originPricingJSON, originPricingSHA256, quoteJSON,
	); err != nil {
		t.Fatalf("seed exact-reuse origin quote: %v", err)
	}
	badComputePlan := computePlan
	badComputePlan.OriginComputePlanSHA256 = strings.Repeat("b", 64)
	badPricing, err := newExactReusePricingDecision(
		decision, badComputePlan, authority, sub.Tier,
		microsToUSD(money.BuyerDebitMicros), originPricingSHA256,
	)
	if err != nil {
		t.Fatal(err)
	}
	reusePricing, err := newExactReusePricingDecision(
		decision, computePlan, authority, sub.Tier,
		microsToUSD(money.BuyerDebitMicros), originPricingSHA256,
	)
	if err != nil {
		t.Fatal(err)
	}
	badJobID := uuid.New()
	if err := store.SubmitExactReuseBatchJob(
		ctx,
		f.BuyerID,
		badJobID,
		sub.JobType.Type,
		sub.Model.Ref,
		"jobs/"+badJobID.String()+"/input.jsonl",
		"jobs/"+badJobID.String()+"/output.jsonl",
		sub.Tier,
		ExactCacheHit{
			ResultRef:    "cas/sha256/" + strings.Repeat("1", 64),
			OutputTokens: 1000,
		},
		money,
		1,
		128,
		"",
		"",
		decision,
		badComputePlan,
		badPricing,
		quoteID,
		true,
		1,
	); err == nil || !strings.Contains(err.Error(), "origin quote") {
		t.Fatalf("exact reuse accepted mismatched origin quote authority: %v", err)
	}
	if countJobRows(t, ctx, pool, badJobID) != 0 {
		t.Fatal("failed exact-reuse quote binding left a job row")
	}
	overCapJobID := uuid.New()
	if err := store.SubmitExactReuseBatchJob(
		ctx,
		f.BuyerID,
		overCapJobID,
		sub.JobType.Type,
		sub.Model.Ref,
		"jobs/"+overCapJobID.String()+"/input.jsonl",
		"jobs/"+overCapJobID.String()+"/output.jsonl",
		sub.Tier,
		ExactCacheHit{
			ResultRef:    "cas/sha256/" + strings.Repeat("1", 64),
			OutputTokens: 1000,
		},
		money,
		1,
		128,
		"",
		"",
		decision,
		computePlan,
		reusePricing,
		quoteID,
		true,
		microsToUSD(money.BuyerDebitMicros)/2,
	); err == nil || !strings.Contains(err.Error(), "firm quote maximum") {
		t.Fatalf("exact reuse accepted a charge over the firm quote maximum: %v", err)
	}
	if countJobRows(t, ctx, pool, overCapJobID) != 0 {
		t.Fatal("firm-cap rejection left an exact-reuse job row")
	}
	if err := store.SubmitExactReuseBatchJob(
		ctx,
		f.BuyerID,
		f.JobID,
		sub.JobType.Type,
		sub.Model.Ref,
		"jobs/"+f.JobID.String()+"/input.jsonl",
		"jobs/"+f.JobID.String()+"/output.jsonl",
		sub.Tier,
		ExactCacheHit{
			ResultRef:    "cas/sha256/" + strings.Repeat("1", 64),
			OutputTokens: 1000,
		},
		money,
		1,
		128,
		"",
		"",
		decision,
		computePlan,
		reusePricing,
		quoteID,
		true,
		1,
	); err != nil {
		t.Fatalf("submit exact-reuse job: %v", err)
	}

	stored, err := store.JobWorkloadDecision(ctx, f.JobID)
	if err != nil {
		t.Fatalf("load exact-reuse workload decision: %v", err)
	}
	if stored == nil || stored.BindingSHA256 != decision.BindingSHA256 {
		t.Fatalf("exact-reuse path did not freeze workload authority: got %+v", stored)
	}
	storedCompute, err := store.JobComputePlan(ctx, f.JobID)
	if err != nil {
		t.Fatalf("load exact-reuse compute plan: %v", err)
	}
	if storedCompute == nil || storedCompute.ExecutionMode != computeExecutionExactReuse {
		t.Fatalf("exact-reuse path did not freeze compute authority: got %+v", storedCompute)
	}
	var storedQuoteID *uuid.UUID
	var firmQuote bool
	var firmQuoteMaxUSD float64
	if err := pool.QueryRow(ctx, `
		SELECT quote_id, firm_quote, COALESCE(firm_quote_max_usd,0)::float8
		  FROM jobs WHERE id=$1`, f.JobID).
		Scan(&storedQuoteID, &firmQuote, &firmQuoteMaxUSD); err != nil {
		t.Fatalf("load exact-reuse quote provenance: %v", err)
	}
	if storedQuoteID == nil || *storedQuoteID != quoteID || !firmQuote || firmQuoteMaxUSD != 1 {
		t.Fatalf("exact reuse lost quote provenance: quote=%v firm=%v max=%v",
			storedQuoteID, firmQuote, firmQuoteMaxUSD)
	}
	storedPricing, err := store.JobPricingDecision(ctx, f.JobID)
	if err != nil {
		t.Fatalf("load exact-reuse pricing decision: %v", err)
	}
	receipt := assembleClearingReceipt(
		f.JobID, "complete", stored, storedCompute, nil, storedPricing,
		nil, Verification{}, nil, nil,
	)
	if receipt.Workload == nil || receipt.Workload.BindingSHA256 != decision.BindingSHA256 {
		t.Fatal("exact-reuse receipt omitted the frozen workload decision")
	}
	if receipt.ComputePlan == nil || receipt.ComputePlan.ExecutionMode != computeExecutionExactReuse {
		t.Fatal("exact-reuse receipt omitted the frozen compute plan")
	}
	if receipt.Pricing == nil ||
		receipt.Pricing.PrimarySupplierCost.Status != pricingCostNotApplicable ||
		receipt.Pricing.PrimarySupplierCost.Amount != 0 {
		t.Fatal("exact-reuse receipt did not disclose zero physical supplier work")
	}
}

func buildTestEconomicPlan(t *testing.T, tasks int, slaPremium float64) EconomicPlan {
	t.Helper()
	if tasks <= 0 {
		tasks = 1
	}
	plan := BuildEconomicPlan(EconomicPlanInput{
		BaseComputeUSD:   0.20 * float64(tasks),
		InitialTaskCount: tasks,
		ExtraTaskReserve: economicExtraTaskReserve(tasks),
		SupplierShare:    0.97,
		SLAPremiumUSD:    slaPremium,
	}, testEconomicSchedule())
	if !plan.Executable {
		t.Fatalf("test economic plan blocked: %s", plan.BlockReason)
	}
	if err := ValidateEconomicPlanSnapshot(plan); err != nil {
		t.Fatalf("test economic plan invalid: %v", err)
	}
	return plan
}

func seedMoneyPathFixture(t *testing.T, ctx context.Context, store *Store, pool *pgxpool.Pool, opts moneyPathSeedOpts) moneyPathFixture {
	t.Helper()
	if opts.TaskCount <= 0 {
		opts.TaskCount = 1
	}
	if opts.TaskStatus == "" {
		opts.TaskStatus = "queued"
	}

	suffix := uuid.NewString()
	buyerID, err := store.CreateBuyerAccount(ctx, "money-"+suffix+"@example.test", "integration-password", 100)
	if err != nil {
		t.Fatalf("create buyer: %v", err)
	}

	f := moneyPathFixture{
		BuyerID:         buyerID,
		SupplierID:      uuid.New(),
		WorkerID:        uuid.New(),
		OtherSupplierID: uuid.New(),
		OtherWorkerID:   uuid.New(),
		JobID:           uuid.New(),
		Plan:            buildTestEconomicPlan(t, opts.TaskCount, opts.SLAPremium),
	}
	for i := 0; i < opts.TaskCount; i++ {
		f.TaskIDs = append(f.TaskIDs, uuid.New())
	}

	// Dialect from scheduler_ask_claim_integration_test.go: active supplier +
	// worker with authorized capability using generatedRuntimeMatrixSHA256.
	seedWorker := func(supplierID, workerID uuid.UUID, emailPrefix string) {
		t.Helper()
		if _, err := pool.Exec(ctx, `
			INSERT INTO suppliers (id,email,status,reputation,completed_tasks)
			VALUES ($1,$2,'active',0.95,100)`,
			supplierID, emailPrefix+"-"+uuid.NewString()+"@example.test"); err != nil {
			t.Fatalf("insert supplier: %v", err)
		}
		if _, err := pool.Exec(ctx, `
			INSERT INTO workers (id,supplier_id,hw_class,memory_gb,effective_memory_gb,
			                     last_seen_at,throttled,min_payout_usd_hr,engine,build_hash)
			VALUES ($1,$2,'apple_silicon_max',64,64,now(),false,0.10,'candle','deadbeefdeadbeef')`,
			workerID, supplierID); err != nil {
			t.Fatalf("insert worker: %v", err)
		}
		if _, err := pool.Exec(ctx, `
			INSERT INTO worker_authorized_capabilities
			  (worker_id,cell_id,runtime_id,job_type,model_ref,model_kind,matrix_sha256)
			VALUES ($1,'cell','rt','embed','all-minilm-l6-v2','hf',$2)`,
			workerID, generatedRuntimeMatrixSHA256); err != nil {
			t.Fatalf("insert authorized capability: %v", err)
		}
	}
	seedWorker(f.SupplierID, f.WorkerID, "money-sup")
	seedWorker(f.OtherSupplierID, f.OtherWorkerID, "money-other")

	if opts.SeedJob {
		seedMoneyPathJob(t, ctx, pool, f, opts)
	}

	t.Cleanup(func() {
		c, cancel := context.WithTimeout(context.Background(), 20*time.Second)
		defer cancel()
		// verification_work references tasks with ON DELETE RESTRICT.
		_, _ = pool.Exec(c, `DELETE FROM task_failures WHERE job_id=$1 OR task_id = ANY($2::uuid[])`,
			f.JobID, f.TaskIDs)
		_, _ = pool.Exec(c, `DELETE FROM verification_work_plans WHERE work_id IN (
			SELECT id FROM verification_work WHERE job_id=$1 OR task_id = ANY($2::uuid[]))`,
			f.JobID, f.TaskIDs)
		_, _ = pool.Exec(c, `DELETE FROM verification_work WHERE job_id=$1 OR task_id = ANY($2::uuid[])`,
			f.JobID, f.TaskIDs)
		_, _ = pool.Exec(c, `DELETE FROM ledger_entries WHERE buyer_id=$1
			OR task_id = ANY($2::uuid[])
			OR payout_ref=$3`,
			f.BuyerID, f.TaskIDs, slaPremiumChargeRef(f.JobID))
		_, _ = pool.Exec(c, `DELETE FROM tasks WHERE job_id=$1 OR id = ANY($2::uuid[])`, f.JobID, f.TaskIDs)
		_, _ = pool.Exec(c, `DELETE FROM job_economic_reserves WHERE job_id=$1`, f.JobID)
		_, _ = pool.Exec(c, `DELETE FROM job_economic_plans WHERE job_id=$1`, f.JobID)
		_, _ = pool.Exec(c, `DELETE FROM job_events WHERE job_id=$1`, f.JobID)
		_, _ = pool.Exec(c, `DELETE FROM jobs WHERE id=$1`, f.JobID)
		for _, workerID := range []uuid.UUID{f.WorkerID, f.OtherWorkerID} {
			_, _ = pool.Exec(c, `DELETE FROM worker_authorized_capabilities WHERE worker_id=$1`, workerID)
			_, _ = pool.Exec(c, `DELETE FROM workers WHERE id=$1`, workerID)
		}
		for _, supplierID := range []uuid.UUID{f.SupplierID, f.OtherSupplierID} {
			_, _ = pool.Exec(c, `DELETE FROM suppliers WHERE id=$1`, supplierID)
		}
		_, _ = pool.Exec(c, `DELETE FROM buyers WHERE id=$1`, f.BuyerID)
	})

	return f
}

func seedMoneyPathJob(t *testing.T, ctx context.Context, pool *pgxpool.Pool, f moneyPathFixture, opts moneyPathSeedOpts) {
	t.Helper()
	if _, err := pool.Exec(ctx, `
		INSERT INTO jobs (id,buyer_id,status,job_type,model_ref,input_ref,task_count,tasks_done,
		                  offered_rate_usd_hr,min_memory_gb,tier,estimated_usd,actual_usd,
			                  firm_quote,sla_premium_usd,prepaid_required)
		VALUES ($1,$2,'running','embed','all-minilm-l6-v2','money/input',$3,0,
		        10.0,0,'batch',$4,0,false,$5,$6)`,
		f.JobID, f.BuyerID, opts.TaskCount, f.Plan.InitialBuyerChargeUSD, opts.SLAPremium, opts.PrepaidRequired); err != nil {
		t.Fatalf("insert job: %v", err)
	}

	if opts.SeedPlanRows {
		planJSON, err := json.Marshal(f.Plan)
		if err != nil {
			t.Fatalf("marshal plan: %v", err)
		}
		if _, err := pool.Exec(ctx, `
			INSERT INTO job_economic_plans (
			  job_id,plan_version,schedule_version,plan_json,initial_task_count,
			  buyer_charge_per_task_usd,supplier_payout_per_task_usd,
			  initial_buyer_charge_usd,reserved_buyer_charge_usd,sla_premium_usd,firm_quote_max_usd
			) VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11)`,
			f.JobID, f.Plan.Version, f.Plan.Schedule.Version, planJSON,
			f.Plan.Input.InitialTaskCount, f.Plan.BuyerChargePerTaskUSD,
			f.Plan.SupplierPayoutPerTaskUSD, f.Plan.InitialBuyerChargeUSD,
			f.Plan.ReservedBuyerChargeUSD, f.Plan.Input.SLAPremiumUSD,
			nullPosFloat(f.Plan.Input.FirmQuoteMaxUSD)); err != nil {
			t.Fatalf("insert economic plan: %v", err)
		}
		if _, err := pool.Exec(ctx, `
			INSERT INTO job_economic_reserves (job_id,reserved_tasks,consumed_tasks)
			VALUES ($1,$2,0)`, f.JobID, f.Plan.Input.ExtraTaskReserve); err != nil {
			t.Fatalf("insert economic reserve: %v", err)
		}
	}

	for i, taskID := range f.TaskIDs {
		resultKey := taskAttemptResultKey(f.JobID, taskID, 0)
		var claimedBy, workerID, execWorker, execSupplier any
		var execHW, execEngine, execBuild any
		if opts.ClaimWorker {
			claimedBy = f.WorkerID
			workerID = f.WorkerID
			execWorker = f.WorkerID
			execSupplier = f.SupplierID
			execHW = "apple_silicon_max"
			execEngine = "candle"
			execBuild = "deadbeefdeadbeef"
		}
		if _, err := pool.Exec(ctx, `
			INSERT INTO tasks
			  (id,job_id,status,input_ref,result_key,chunk_index,retry_count,
			   claimed_by,worker_id,execution_worker_id,execution_supplier_id,
			   execution_hw_class,execution_engine,execution_build_hash,
			   economic_buyer_charge_usd,economic_supplier_payout_usd)
			VALUES ($1,$2,$3,'money/input',$4,$5,0,
			        $6,$7,$8,$9,$10,$11,$12,$13,$14)`,
			taskID, f.JobID, opts.TaskStatus, resultKey, i,
			claimedBy, workerID, execWorker, execSupplier,
			execHW, execEngine, execBuild,
			f.Plan.BuyerChargePerTaskUSD, f.Plan.SupplierPayoutPerTaskUSD); err != nil {
			t.Fatalf("insert task %d: %v", i, err)
		}
	}
}

func validJobRow(t *testing.T, f moneyPathFixture, tasks []taskRow) *jobRow {
	t.Helper()
	workload, err := buildWorkloadDecision(jobSubmit{
		JobType: JobType{Type: "embed"},
		Model:   ModelRef{Kind: "hf", Ref: "all-minilm-l6-v2"},
		Tier:    "batch",
		Constraints: JobConstraints{
			MaxDurationSecs: 3600,
		},
	}, strings.Repeat("a", 64))
	if err != nil {
		t.Fatalf("build test workload decision: %v", err)
	}
	economicPlan := f.Plan
	if economicPlan.Input.InitialTaskCount != len(tasks) {
		economicPlan = buildTestEconomicPlan(t, len(tasks), economicPlan.Input.SLAPremiumUSD)
	}
	inputRecords := len(tasks)
	inputBytes := int64(inputRecords * 128)
	computePlan, err := newDistributedComputePlan(
		workload,
		inputRecords,
		inputBytes,
		testInputDepthProfile(inputRecords),
		1,
		len(tasks),
		0,
		0,
		quoteTimeFromETABands(60, 0, false),
		"static",
		economicPlan.Input.BaseComputeUSD,
		0,
		QuoteConfidence{Score: 0.9, Reasons: []string{"integration fixture uses exact task geometry"}},
		[]string{"integration fixture has no live fleet estimate"},
	)
	if err != nil {
		t.Fatalf("build test compute plan: %v", err)
	}
	authority := catalogueAuthorityFixture(
		t, workload, economicPlan.Schedule.Currency, economicPlan.Input.SupplierShare,
	)
	placement := placementForPricingFixture(t, workload, authority)
	pricing, err := newDistributedPricingDecision(
		workload, computePlan, placement, economicPlan, authority,
		workload.Binding.Tier, "",
	)
	if err != nil {
		t.Fatalf("build test pricing decision: %v", err)
	}
	return &jobRow{
		ID:                   f.JobID,
		BuyerID:              f.BuyerID,
		JobType:              "embed",
		ModelRef:             "all-minilm-l6-v2",
		InputRef:             "money/input-" + f.JobID.String(),
		OutputRef:            "money/output-" + f.JobID.String(),
		Tier:                 "batch",
		EstimatedUSD:         economicPlan.InitialBuyerChargeUSD,
		TaskCount:            len(tasks),
		MinMemoryGB:          float32(workload.MinimumMemoryGB),
		MaxDurationSecs:      3600,
		SplitSize:            1,
		OfferedRateUsdHr:     placement.OfferedRateUsdHr,
		ETASecs:              60,
		ETARawSecs:           60,
		SLAPremiumUSD:        economicPlan.Input.SLAPremiumUSD,
		EconomicInputRecords: int64(inputRecords),
		EconomicInputBytes:   inputBytes,
		EconomicPlan:         economicPlan,
		WorkloadDecision:     workload,
		ComputePlan:          computePlan,
		PlacementRequirement: placement,
		PricingDecision:      pricing,
	}
}

func makeTasks(f moneyPathFixture, n int) []taskRow {
	out := make([]taskRow, n)
	for i := 0; i < n; i++ {
		id := uuid.New()
		if i < len(f.TaskIDs) {
			id = f.TaskIDs[i]
		}
		out[i] = taskRow{
			ID:         id,
			JobID:      f.JobID,
			InputRef:   fmt.Sprintf("money/input/chunk-%d", i),
			ResultKey:  taskAttemptResultKey(f.JobID, id, 0),
			ChunkIndex: i,
		}
	}
	return out
}

func countBuyerLedger(t *testing.T, ctx context.Context, pool *pgxpool.Pool, buyerID uuid.UUID) int {
	t.Helper()
	var n int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM ledger_entries WHERE buyer_id=$1`, buyerID).Scan(&n); err != nil {
		t.Fatalf("count buyer ledger: %v", err)
	}
	return n
}

func countJobRows(t *testing.T, ctx context.Context, pool *pgxpool.Pool, jobID uuid.UUID) int {
	t.Helper()
	var n int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM jobs WHERE id=$1`, jobID).Scan(&n); err != nil {
		t.Fatalf("count jobs: %v", err)
	}
	return n
}

func taskStatus(t *testing.T, ctx context.Context, pool *pgxpool.Pool, taskID uuid.UUID) string {
	t.Helper()
	var status string
	if err := pool.QueryRow(ctx, `SELECT status FROM tasks WHERE id=$1`, taskID).Scan(&status); err != nil {
		t.Fatalf("task status: %v", err)
	}
	return status
}

func commitFor(f moneyPathFixture, taskID uuid.UUID, attempt int16) TaskCommit {
	return TaskCommit{
		TaskID:       taskID,
		Attempt:      attempt,
		ResultKey:    taskAttemptResultKey(f.JobID, taskID, attempt),
		DurationMS:   12,
		TokensUsed:   3,
		ResultSHA256: strings.Repeat("ab", 32),
	}
}

// --- SubmitJobTx ---

func TestSubmitJobTxCommitsJobTasksAndPlanWithoutLedger(t *testing.T) {
	ctx, store, pool := openMoneyPathStore(t)
	f := seedMoneyPathFixture(t, ctx, store, pool, moneyPathSeedOpts{TaskCount: 2})
	tasks := makeTasks(f, 2)
	// Align fixture task ids with the ones we submit so cleanup finds them.
	f.TaskIDs = []uuid.UUID{tasks[0].ID, tasks[1].ID}
	job := validJobRow(t, f, tasks)

	if err := store.SubmitJobTx(ctx, job, tasks); err != nil {
		t.Fatalf("SubmitJobTx: %v", err)
	}

	if countJobRows(t, ctx, pool, f.JobID) != 1 {
		t.Fatal("job row missing after successful submit")
	}
	var taskCount, planCount, reserveCount int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM tasks WHERE job_id=$1`, f.JobID).Scan(&taskCount); err != nil {
		t.Fatal(err)
	}
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM job_economic_plans WHERE job_id=$1`, f.JobID).Scan(&planCount); err != nil {
		t.Fatal(err)
	}
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM job_economic_reserves WHERE job_id=$1`, f.JobID).Scan(&reserveCount); err != nil {
		t.Fatal(err)
	}
	if taskCount != 2 || planCount != 1 || reserveCount != 1 {
		t.Fatalf("submit projections = tasks:%d plan:%d reserve:%d", taskCount, planCount, reserveCount)
	}
	storedCompute, err := store.JobComputePlan(ctx, f.JobID)
	if err != nil {
		t.Fatalf("load frozen compute plan: %v", err)
	}
	if storedCompute == nil || storedCompute.TotalInitialTasks != 2 ||
		storedCompute.SplitSize != job.SplitSize {
		t.Fatalf("submit did not freeze compute authority: %+v", storedCompute)
	}
	storedPlacement, err := store.JobPlacementRequirement(ctx, f.JobID)
	if err != nil {
		t.Fatalf("load frozen placement requirement: %v", err)
	}
	storedPricing, err := store.JobPricingDecision(ctx, f.JobID)
	if err != nil {
		t.Fatalf("load frozen pricing decision: %v", err)
	}
	if storedPlacement == nil || storedPricing == nil ||
		storedPricing.PlacementRequirementSHA256 == "" ||
		storedPricing.Catalogue.ScheduleSHA256 == "" {
		t.Fatalf("submit did not freeze composite pricing authority: placement=%+v pricing=%+v",
			storedPlacement, storedPricing)
	}
	if _, err := pool.Exec(ctx, `
		UPDATE jobs
		   SET compute_plan = jsonb_set(compute_plan, '{split_size}', '99'::jsonb)
		 WHERE id=$1`, f.JobID); err == nil {
		t.Fatal("database allowed frozen compute authority to be mutated")
	}
	if _, err := pool.Exec(ctx, `
		UPDATE jobs
		   SET pricing_decision = jsonb_set(pricing_decision, '{buyer_price}', '99'::jsonb)
		 WHERE id=$1`, f.JobID); err == nil {
		t.Fatal("database allowed frozen pricing authority to be mutated")
	}
	if _, err := pool.Exec(ctx, `
		UPDATE jobs
		   SET placement_requirement =
		       jsonb_set(placement_requirement, '{offered_rate_usd_hr}', '99'::jsonb)
		 WHERE id=$1`, f.JobID); err == nil {
		t.Fatal("database allowed frozen placement authority to be mutated")
	}
	for name, statement := range map[string]string{
		"job type":     `UPDATE jobs SET job_type='batch_infer' WHERE id=$1`,
		"model":        `UPDATE jobs SET model_ref='different-model' WHERE id=$1`,
		"tier":         `UPDATE jobs SET tier='priority' WHERE id=$1`,
		"memory":       `UPDATE jobs SET min_memory_gb=min_memory_gb+1 WHERE id=$1`,
		"duration":     `UPDATE jobs SET max_duration_secs=max_duration_secs+1 WHERE id=$1`,
		"hardware":     `UPDATE jobs SET hw_classes=ARRAY['apple_silicon_ultra'] WHERE id=$1`,
		"residency":    `UPDATE jobs SET data_residency=ARRAY['US'] WHERE id=$1`,
		"reputation":   `UPDATE jobs SET min_reputation=min_reputation+0.1 WHERE id=$1`,
		"offered rate": `UPDATE jobs SET offered_rate_usd_hr=offered_rate_usd_hr+0.1 WHERE id=$1`,
	} {
		if _, err := pool.Exec(ctx, statement, f.JobID); err == nil {
			t.Fatalf("database allowed frozen job %s authority to be mutated", name)
		}
	}
	if n := countBuyerLedger(t, ctx, pool, f.BuyerID); n != 0 {
		t.Fatalf("SubmitJobTx minted %d ledger rows; submit must mint no money", n)
	}
}

func TestLegacyJobPricingAuthorityRemainsExplicitlyUnverifiable(t *testing.T) {
	ctx, store, pool := openMoneyPathStore(t)
	f := seedMoneyPathFixture(t, ctx, store, pool, moneyPathSeedOpts{
		TaskCount: 1, SeedJob: true,
	})
	placement, err := store.JobPlacementRequirement(ctx, f.JobID)
	if err != nil {
		t.Fatal(err)
	}
	pricing, err := store.JobPricingDecision(ctx, f.JobID)
	if err != nil {
		t.Fatal(err)
	}
	if placement != nil || pricing != nil {
		t.Fatalf("legacy job received invented authority: placement=%+v pricing=%+v",
			placement, pricing)
	}
	var pairCount int
	if err := pool.QueryRow(ctx, `
		SELECT count(*) FROM jobs
		 WHERE id=$1
		   AND placement_requirement IS NULL
		   AND placement_requirement_sha256 IS NULL
		   AND pricing_decision IS NULL
		   AND pricing_decision_sha256 IS NULL`, f.JobID,
	).Scan(&pairCount); err != nil {
		t.Fatal(err)
	}
	if pairCount != 1 {
		t.Fatal("legacy pricing NULL authority shape was not preserved")
	}
}

func TestQuoteJobSchedulerReceiptPreserveExactPricingAuthority(t *testing.T) {
	ctx, store, pool := openMoneyPathStore(t)
	f := seedMoneyPathFixture(t, ctx, store, pool, moneyPathSeedOpts{TaskCount: 1})
	tasks := makeTasks(f, 1)
	f.TaskIDs = []uuid.UUID{tasks[0].ID}
	job := validJobRow(t, f, tasks)

	quoteID := uuid.New()
	quote := Quote{
		QuoteID: "q_" + quoteID.String(), bareID: quoteID,
		etaRawP50Secs: job.ETARawSecs,
		JobType:       job.JobType, Model: job.ModelRef, Tier: job.Tier,
		Currency: job.EconomicPlan.Schedule.Currency,
		Workload: job.WorkloadDecision, Placement: job.PlacementRequirement,
		ComputePlan: job.ComputePlan, Pricing: job.PricingDecision,
		Time: QuoteTime{
			P50Secs: job.ComputePlan.ETAP50Secs, P90Secs: job.ComputePlan.ETAP90Secs,
			WorstCaseSecs: job.ComputePlan.ETAWorstCaseSecs, ConfidenceBandMethod: job.ComputePlan.ETAConfidenceBandMethod,
		},
		Economics:   job.EconomicPlan,
		InputSHA256: strings.Repeat("a", 64),
		ExpiresAt:   time.Now().Add(quoteTTL).UTC(),
	}
	if err := store.InsertQuote(ctx, f.BuyerID, quote); err != nil {
		t.Fatalf("insert composite quote: %v", err)
	}
	t.Cleanup(func() {
		c, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		_, _ = pool.Exec(c, `DELETE FROM quotes WHERE id=$1`, quoteID)
	})
	bound, err := store.GetBindableQuote(ctx, quoteID, f.BuyerID)
	if err != nil {
		t.Fatalf("load composite quote: %v", err)
	}
	quotePricingSHA, err := pricingDecisionDigest(quote.Pricing)
	if err != nil {
		t.Fatal(err)
	}
	if bound.PricingDecisionSHA256 != quotePricingSHA {
		t.Fatalf("bound quote pricing digest=%s want %s",
			bound.PricingDecisionSHA256, quotePricingSHA)
	}
	if bound.ETARawSecs != job.ETARawSecs {
		t.Fatalf("bound quote raw ETA=%d want %d", bound.ETARawSecs, job.ETARawSecs)
	}

	job.QuoteID = quoteID
	if err := store.SubmitJobTx(ctx, job, tasks); err != nil {
		t.Fatalf("submit quote-bound job: %v", err)
	}
	var storedRawETA int
	if err := pool.QueryRow(ctx,
		`SELECT eta_secs_raw FROM jobs WHERE id=$1`, f.JobID,
	).Scan(&storedRawETA); err != nil {
		t.Fatal(err)
	}
	if storedRawETA != bound.ETARawSecs {
		t.Fatalf("job raw ETA=%d want frozen quote %d", storedRawETA, bound.ETARawSecs)
	}

	candidate := job.WorkloadDecision.RuntimeCandidates[0]
	if _, err := pool.Exec(ctx,
		`DELETE FROM worker_authorized_capabilities WHERE worker_id=$1`,
		f.WorkerID,
	); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `
		INSERT INTO worker_authorized_capabilities
		  (worker_id,cell_id,runtime_id,job_type,model_ref,model_kind,matrix_sha256)
		VALUES ($1,$2,$3,$4,$5,$6,$7)`,
		f.WorkerID, candidate.CellID, candidate.RuntimeID,
		job.JobType, job.ModelRef, job.WorkloadDecision.Binding.Model.Kind,
		generatedRuntimeMatrixSHA256,
	); err != nil {
		t.Fatal(err)
	}
	claimed, err := store.ClaimTasksTx(ctx, WorkerAuth{
		WorkerID: f.WorkerID, SupplierID: f.SupplierID,
	})
	if err != nil {
		t.Fatalf("claim quote-bound task: %v", err)
	}
	if claimed == nil || claimed.TaskID != tasks[0].ID ||
		claimed.OfferedRateUsdHr != job.PlacementRequirement.OfferedRateUsdHr {
		t.Fatalf("scheduler did not consume frozen placement authority: %+v", claimed)
	}

	storedWorkload, err := store.JobWorkloadDecision(ctx, f.JobID)
	if err != nil {
		t.Fatal(err)
	}
	storedCompute, err := store.JobComputePlan(ctx, f.JobID)
	if err != nil {
		t.Fatal(err)
	}
	storedPlacement, err := store.JobPlacementRequirement(ctx, f.JobID)
	if err != nil {
		t.Fatal(err)
	}
	storedPricing, err := store.JobPricingDecision(ctx, f.JobID)
	if err != nil {
		t.Fatal(err)
	}
	jobPricingSHA, err := pricingDecisionDigest(*storedPricing)
	if err != nil {
		t.Fatal(err)
	}
	if jobPricingSHA != quotePricingSHA {
		t.Fatalf("quote pricing digest %s changed at accepted job %s",
			quotePricingSHA, jobPricingSHA)
	}
	invoice, err := store.JobInvoice(ctx, f.JobID, f.BuyerID)
	if err != nil {
		t.Fatal(err)
	}
	receipt := assembleClearingReceipt(
		f.JobID, invoice.Status, storedWorkload, storedCompute,
		storedPlacement, storedPricing, invoice, Verification{}, nil, nil,
	)
	if receipt.Authority.PricingDecisionSHA256 != quotePricingSHA ||
		receipt.Pricing == nil ||
		receipt.Reconciliation == nil ||
		receipt.Reconciliation.CatalogueScheduleSHA256 !=
			quote.Pricing.Catalogue.ScheduleSHA256 {
		t.Fatalf("receipt did not preserve exact quote pricing authority: %+v", receipt)
	}
}

func TestSubmitJobTxPlanTaskCountMismatchFailsClosed(t *testing.T) {
	ctx, store, pool := openMoneyPathStore(t)
	f := seedMoneyPathFixture(t, ctx, store, pool, moneyPathSeedOpts{TaskCount: 2})
	tasks := makeTasks(f, 1) // plan expects 2
	f.TaskIDs = []uuid.UUID{tasks[0].ID}
	job := validJobRow(t, f, tasks)
	// validJobRow sets TaskCount=len(tasks)=1; force the plan/task mismatch path.
	job.TaskCount = 1
	job.EconomicPlan = f.Plan // InitialTaskCount still 2

	err := store.SubmitJobTx(ctx, job, tasks)
	if err == nil {
		t.Fatal("plan/task-count mismatch accepted")
	}
	if !strings.Contains(err.Error(), "initial_task_count") && !strings.Contains(err.Error(), "task_count") {
		t.Fatalf("unexpected error: %v", err)
	}
	if countJobRows(t, ctx, pool, f.JobID) != 0 {
		t.Fatal("failed submit left a job row")
	}
	if n := countBuyerLedger(t, ctx, pool, f.BuyerID); n != 0 {
		t.Fatalf("failed submit minted ledger rows: %d", n)
	}
}

func TestSubmitJobTxBoundQuoteComputeMismatchFailsClosed(t *testing.T) {
	ctx, store, pool := openMoneyPathStore(t)
	f := seedMoneyPathFixture(t, ctx, store, pool, moneyPathSeedOpts{TaskCount: 1})
	tasks := makeTasks(f, 1)
	f.TaskIDs = []uuid.UUID{tasks[0].ID}
	job := validJobRow(t, f, tasks)
	job.QuoteID = uuid.New()
	if _, err := pool.Exec(ctx, `
		INSERT INTO quotes (id,buyer_id,job_type,compute_plan_sha256)
		VALUES ($1,$2,$3,$4)`,
		job.QuoteID, f.BuyerID, job.JobType, strings.Repeat("f", 64),
	); err != nil {
		t.Fatalf("seed mismatched quote compute authority: %v", err)
	}

	// Assert the exact refusal, not merely "bound quote": the currency and
	// placement checks a few lines further down produce errors containing that
	// same phrase, so a loose assertion passes even when the compute-authority
	// check has been removed entirely. Mutation testing caught exactly that.
	err := store.SubmitJobTx(ctx, job, tasks)
	if err == nil || !strings.Contains(err.Error(), "job compute plan does not match its bound quote") {
		t.Fatalf("bound quote compute mismatch accepted: %v", err)
	}
	if countJobRows(t, ctx, pool, f.JobID) != 0 {
		t.Fatal("failed bound quote submit left a job row")
	}
}

func TestSubmitJobTxBoundQuoteOfferedRateMismatchFailsClosed(t *testing.T) {
	ctx, store, pool := openMoneyPathStore(t)
	f := seedMoneyPathFixture(t, ctx, store, pool, moneyPathSeedOpts{TaskCount: 1})
	tasks := makeTasks(f, 1)
	f.TaskIDs = []uuid.UUID{tasks[0].ID}
	job := validJobRow(t, f, tasks)
	job.QuoteID = uuid.New()

	computeSHA256, err := computePlanDigest(job.ComputePlan)
	if err != nil {
		t.Fatal(err)
	}
	binding := job.WorkloadDecision.Binding
	placement, err := placementRequirementFor(jobSubmit{
		JobType: binding.JobType, Model: binding.Model, Constraints: binding.Constraints,
		Tier: binding.Tier, MinReputation: binding.MinReputation,
	}, job.WorkloadDecision, job.OfferedRateUsdHr+1)
	if err != nil {
		t.Fatal(err)
	}
	quoteJSON, err := json.Marshal(map[string]any{"placement_requirement": placement})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `
		INSERT INTO quotes (
		  id,buyer_id,job_type,model_ref,compute_plan_sha256,quote_json
		) VALUES ($1,$2,$3,$4,$5,$6)`,
		job.QuoteID, f.BuyerID, job.JobType, job.ModelRef, computeSHA256, quoteJSON,
	); err != nil {
		t.Fatalf("seed rate-mismatched quote authority: %v", err)
	}

	err = store.SubmitJobTx(ctx, job, tasks)
	if err == nil || !strings.Contains(err.Error(), "offered rate") {
		t.Fatalf("bound quote offered-rate mismatch accepted: %v", err)
	}
	if countJobRows(t, ctx, pool, f.JobID) != 0 {
		t.Fatal("failed bound quote rate submit left a job row")
	}
}

func TestSubmitJobTxFirmQuoteMismatchFailsClosed(t *testing.T) {
	ctx, store, pool := openMoneyPathStore(t)
	f := seedMoneyPathFixture(t, ctx, store, pool, moneyPathSeedOpts{TaskCount: 1})
	// Rebuild plan with a firm cap so the snapshot is valid, then desync the job.
	plan := BuildEconomicPlan(EconomicPlanInput{
		BaseComputeUSD: 0.20, InitialTaskCount: 1, ExtraTaskReserve: 1,
		SupplierShare: 0.97, FirmQuoteMaxUSD: 5.0,
	}, testEconomicSchedule())
	if !plan.Executable {
		t.Fatalf("firm plan blocked: %s", plan.BlockReason)
	}
	f.Plan = plan
	tasks := makeTasks(f, 1)
	f.TaskIDs = []uuid.UUID{tasks[0].ID}
	job := validJobRow(t, f, tasks)
	job.FirmQuote = true
	job.FirmQuoteMaxUSD = 9.99 // desync from plan.Input.FirmQuoteMaxUSD
	job.EconomicPlan = plan
	job.EstimatedUSD = plan.InitialBuyerChargeUSD

	err := store.SubmitJobTx(ctx, job, tasks)
	if err == nil {
		t.Fatal("firm-quote mismatch accepted")
	}
	if !strings.Contains(err.Error(), "firm quote") {
		t.Fatalf("unexpected error: %v", err)
	}
	if countJobRows(t, ctx, pool, f.JobID) != 0 {
		t.Fatal("failed firm-quote submit left a job row")
	}
}

func TestSubmitJobTxSLAPremiumMismatchFailsClosed(t *testing.T) {
	ctx, store, pool := openMoneyPathStore(t)
	f := seedMoneyPathFixture(t, ctx, store, pool, moneyPathSeedOpts{TaskCount: 1, SLAPremium: 0.08})
	tasks := makeTasks(f, 1)
	f.TaskIDs = []uuid.UUID{tasks[0].ID}
	job := validJobRow(t, f, tasks)
	job.SLAPremiumUSD = 0.99 // desync from plan.Input.SLAPremiumUSD

	err := store.SubmitJobTx(ctx, job, tasks)
	if err == nil {
		t.Fatal("SLA premium mismatch accepted")
	}
	if !strings.Contains(err.Error(), "SLA premium") {
		t.Fatalf("unexpected error: %v", err)
	}
	if countJobRows(t, ctx, pool, f.JobID) != 0 {
		t.Fatal("failed SLA submit left a job row")
	}
}

// --- StartTask ---

type dynamicTiebreakStartSnapshot struct {
	Status, ClaimedBy, WorkerID, ExecutionWorkerID, ExecutionSupplierID string
	ExecutionHWClass, ExecutionEngine, ExecutionBuildHash               string
	RuntimeCellID, RuntimeID, RuntimeMatrixSHA256, ModelKind            string
	IsRedundancy                                                        bool
	HedgedFrom                                                          string
	RetryCount                                                          int
	VerificationHWClass, VerificationEngine, VerificationBuildHash      string
	StartedAt                                                           string
}

func seedDynamicTiebreakStartFixture(t *testing.T) (
	context.Context, *Store, *pgxpool.Pool, moneyPathFixture, uuid.UUID,
) {
	t.Helper()
	ctx, store, pool := openMoneyPathStore(t)
	f := seedMoneyPathFixture(t, ctx, store, pool, moneyPathSeedOpts{
		TaskCount: 1, TaskStatus: "running", ClaimWorker: true, SeedJob: true, SeedPlanRows: true,
	})
	anchorID := f.TaskIDs[0]
	if _, err := pool.Exec(ctx,
		`UPDATE tasks SET claimed_at=now(),started_at=now() WHERE id=$1`,
		anchorID); err != nil {
		t.Fatalf("stamp anchor execution: %v", err)
	}
	tiebreakID, err := store.InsertTiebreakTask(
		ctx, f.JobID, anchorID, f.OtherWorkerID, "money/input", 0,
	)
	if err != nil {
		t.Fatalf("insert production tiebreak: %v", err)
	}
	inserted := dynamicTiebreakSnapshot(t, ctx, pool, tiebreakID)
	if inserted.Status != "queued" ||
		inserted.ClaimedBy != f.OtherWorkerID.String() ||
		!inserted.IsRedundancy ||
		inserted.HedgedFrom != anchorID.String() ||
		inserted.RetryCount != 0 ||
		inserted.VerificationHWClass != "apple_silicon_max" ||
		inserted.VerificationEngine != "candle" ||
		inserted.VerificationBuildHash != "deadbeefdeadbeef" {
		t.Fatalf("inserted tiebreak has invalid dynamic geometry: %+v", inserted)
	}
	return ctx, store, pool, f, tiebreakID
}

func dynamicTiebreakSnapshot(
	t *testing.T, ctx context.Context, pool *pgxpool.Pool, taskID uuid.UUID,
) dynamicTiebreakStartSnapshot {
	t.Helper()
	var snap dynamicTiebreakStartSnapshot
	if err := pool.QueryRow(ctx, `
		SELECT status,
		       COALESCE(claimed_by::text,''),COALESCE(worker_id::text,''),
		       COALESCE(execution_worker_id::text,''),COALESCE(execution_supplier_id::text,''),
		       COALESCE(execution_hw_class,''),COALESCE(execution_engine,''),
		       COALESCE(execution_build_hash,''),
		       COALESCE(runtime_cell_id,''),COALESCE(runtime_id,''),
		       COALESCE(runtime_matrix_sha256,''),COALESCE(model_kind,''),
		       COALESCE(is_redundancy,false),COALESCE(hedged_from::text,''),
		       COALESCE(retry_count,0),
		       COALESCE(verification_hw_class,''),COALESCE(verification_engine,''),
		       COALESCE(verification_build_hash,''),
		       COALESCE(started_at::text,'')
		  FROM tasks WHERE id=$1`, taskID).
		Scan(&snap.Status, &snap.ClaimedBy, &snap.WorkerID,
			&snap.ExecutionWorkerID, &snap.ExecutionSupplierID,
			&snap.ExecutionHWClass, &snap.ExecutionEngine, &snap.ExecutionBuildHash,
			&snap.RuntimeCellID, &snap.RuntimeID, &snap.RuntimeMatrixSHA256,
			&snap.ModelKind, &snap.IsRedundancy, &snap.HedgedFrom, &snap.RetryCount,
			&snap.VerificationHWClass, &snap.VerificationEngine,
			&snap.VerificationBuildHash, &snap.StartedAt); err != nil {
		t.Fatal(err)
	}
	return snap
}

func dynamicTiebreakHistoryCounts(
	t *testing.T, ctx context.Context, pool *pgxpool.Pool,
	taskID, workerID, supplierID uuid.UUID, attempt int16,
) (exact, total int) {
	t.Helper()
	if err := pool.QueryRow(ctx, `
		SELECT COUNT(*) FILTER (
		         WHERE attempt=$2 AND worker_id=$3 AND supplier_id=$4
		       ),COUNT(*)
		  FROM task_execution_history WHERE task_id=$1`,
		taskID, attempt, workerID, supplierID).Scan(&exact, &total); err != nil {
		t.Fatal(err)
	}
	return exact, total
}

func TestStartTaskClaimedToRunningAndRejectsOtherWorker(t *testing.T) {
	ctx, store, pool := openMoneyPathStore(t)
	f := seedMoneyPathFixture(t, ctx, store, pool, moneyPathSeedOpts{
		TaskCount: 1, TaskStatus: "queued", ClaimWorker: true, SeedJob: true, SeedPlanRows: true,
	})
	taskID := f.TaskIDs[0]

	// Refuse while the task is still queued under another worker's claim — that is
	// the guard under test. Checking only after a successful start would also fail
	// for status='running', which is a different fence.
	if err := store.StartTask(ctx, taskID, f.OtherWorkerID, 0); !errors.Is(err, errNotFound) {
		t.Fatalf("other worker StartTask error = %v, want errNotFound", err)
	}
	if got := taskStatus(t, ctx, pool, taskID); got != "queued" {
		t.Fatalf("foreign start mutated status to %q", got)
	}

	if err := store.StartTask(ctx, taskID, f.WorkerID, 0); err != nil {
		t.Fatalf("StartTask happy path: %v", err)
	}
	if got := taskStatus(t, ctx, pool, taskID); got != "running" {
		t.Fatalf("task status after StartTask = %q, want running", got)
	}
	var execWorker uuid.UUID
	if err := pool.QueryRow(ctx, `SELECT execution_worker_id FROM tasks WHERE id=$1`, taskID).Scan(&execWorker); err != nil {
		t.Fatal(err)
	}
	if execWorker != f.WorkerID {
		t.Fatalf("execution_worker_id = %s, want %s", execWorker, f.WorkerID)
	}

	// A lost/ambiguous acknowledgement must be recoverable by replaying the
	// exact owner+attempt start. The server contract is idempotent for that
	// identity, while a changed attempt remains fenced.
	if err := store.StartTask(ctx, taskID, f.WorkerID, 0); err != nil {
		t.Fatalf("exact StartTask replay: %v", err)
	}
	if err := store.StartTask(ctx, taskID, f.WorkerID, 1); !errors.Is(err, errNotFound) {
		t.Fatalf("wrong-attempt StartTask replay error = %v, want errNotFound", err)
	}
	if got := taskStatus(t, ctx, pool, taskID); got != "running" {
		t.Fatalf("start replays changed task status to %q", got)
	}
}

func TestStartTaskDynamicTiebreakHistoryAndReplayFences(t *testing.T) {
	t.Run("queued start mints one exact history identity", func(t *testing.T) {
		ctx, store, pool, f, taskID := seedDynamicTiebreakStartFixture(t)
		workerID, supplierID := f.OtherWorkerID, f.OtherSupplierID
		before := dynamicTiebreakSnapshot(t, ctx, pool, taskID)
		if before.Status != "queued" || before.ClaimedBy != workerID.String() ||
			before.WorkerID != "" || before.ExecutionWorkerID != "" ||
			before.ExecutionSupplierID != "" || before.StartedAt != "" {
			t.Fatalf("unexpected queued tiebreak identity: %+v", before)
		}
		if exact, total := dynamicTiebreakHistoryCounts(
			t, ctx, pool, taskID, workerID, supplierID, 0,
		); exact != 0 || total != 0 {
			t.Fatalf("queued tiebreak history = exact %d/total %d, want 0/0", exact, total)
		}

		if err := store.StartTask(ctx, taskID, f.WorkerID, 0); !errors.Is(err, errNotFound) {
			t.Fatalf("foreign queued start error = %v, want errNotFound", err)
		}
		if err := store.StartTask(ctx, taskID, workerID, 1); !errors.Is(err, errNotFound) {
			t.Fatalf("wrong-attempt queued start error = %v, want errNotFound", err)
		}
		if got := dynamicTiebreakSnapshot(t, ctx, pool, taskID); got != before {
			t.Fatalf("fenced queued starts mutated task:\nbefore=%+v\nafter=%+v", before, got)
		}

		if err := store.StartTask(ctx, taskID, workerID, 0); err != nil {
			t.Fatalf("exact dynamic tiebreak start: %v", err)
		}
		started := dynamicTiebreakSnapshot(t, ctx, pool, taskID)
		if started.Status != "running" ||
			started.ClaimedBy != workerID.String() ||
			started.WorkerID != workerID.String() ||
			started.ExecutionWorkerID != workerID.String() ||
			started.ExecutionSupplierID != supplierID.String() ||
			started.ExecutionHWClass != "apple_silicon_max" ||
			started.ExecutionEngine != "candle" ||
			started.ExecutionBuildHash != "deadbeefdeadbeef" ||
			started.StartedAt == "" {
			t.Fatalf("started tiebreak has wrong execution identity: %+v", started)
		}
		if started.RuntimeCellID != "" || started.RuntimeID != "" ||
			started.RuntimeMatrixSHA256 != "" || started.ModelKind != "" {
			t.Fatalf("direct StartTask invented runtime provenance: %+v", started)
		}
		if exact, total := dynamicTiebreakHistoryCounts(
			t, ctx, pool, taskID, workerID, supplierID, 0,
		); exact != 1 || total != 1 {
			t.Fatalf("started tiebreak history = exact %d/total %d, want 1/1", exact, total)
		}

		if err := store.StartTask(ctx, taskID, workerID, 0); err != nil {
			t.Fatalf("exact dynamic tiebreak replay: %v", err)
		}
		if err := store.StartTask(ctx, taskID, workerID, 1); !errors.Is(err, errNotFound) {
			t.Fatalf("wrong-attempt running replay error = %v, want errNotFound", err)
		}
		if err := store.StartTask(ctx, taskID, f.WorkerID, 0); !errors.Is(err, errNotFound) {
			t.Fatalf("foreign running replay error = %v, want errNotFound", err)
		}
		if got := dynamicTiebreakSnapshot(t, ctx, pool, taskID); got != started {
			t.Fatalf("replays mutated execution identity:\nstarted=%+v\nafter=%+v", started, got)
		}
		if exact, total := dynamicTiebreakHistoryCounts(
			t, ctx, pool, taskID, workerID, supplierID, 0,
		); exact != 1 || total != 1 {
			t.Fatalf("replayed tiebreak history = exact %d/total %d, want 1/1", exact, total)
		}
	})

	t.Run("claim then start preserves frozen runtime authority", func(t *testing.T) {
		ctx, store, pool, f, taskID := seedDynamicTiebreakStartFixture(t)
		workerID, supplierID := f.OtherWorkerID, f.OtherSupplierID

		claimed, err := store.ClaimTasksTx(ctx, WorkerAuth{
			WorkerID: workerID, SupplierID: supplierID,
		})
		if err != nil {
			t.Fatalf("claim pinned dynamic tiebreak: %v", err)
		}
		if claimed == nil || claimed.TaskID != taskID || claimed.Attempt != 0 {
			t.Fatalf("claimed dynamic tiebreak = %+v, want task %s attempt 0", claimed, taskID)
		}
		claimedSnap := dynamicTiebreakSnapshot(t, ctx, pool, taskID)
		if claimedSnap.Status != "running" ||
			claimedSnap.ClaimedBy != workerID.String() ||
			claimedSnap.WorkerID != workerID.String() ||
			claimedSnap.ExecutionWorkerID != workerID.String() ||
			claimedSnap.ExecutionSupplierID != supplierID.String() ||
			claimedSnap.ExecutionHWClass != "apple_silicon_max" ||
			claimedSnap.ExecutionEngine != "candle" ||
			claimedSnap.ExecutionBuildHash != "deadbeefdeadbeef" ||
			claimedSnap.RuntimeCellID != "cell" ||
			claimedSnap.RuntimeID != "rt" ||
			claimedSnap.RuntimeMatrixSHA256 != generatedRuntimeMatrixSHA256 ||
			claimedSnap.ModelKind != "hf" ||
			claimedSnap.StartedAt == "" {
			t.Fatalf("claimed tiebreak authority is not exact: %+v", claimedSnap)
		}
		if exact, total := dynamicTiebreakHistoryCounts(
			t, ctx, pool, taskID, workerID, supplierID, 0,
		); exact != 1 || total != 1 {
			t.Fatalf("claimed tiebreak history = exact %d/total %d, want 1/1", exact, total)
		}

		if err := store.StartTask(ctx, taskID, workerID, 0); err != nil {
			t.Fatalf("start acknowledgement replay after claim: %v", err)
		}
		if got := dynamicTiebreakSnapshot(t, ctx, pool, taskID); got != claimedSnap {
			t.Fatalf("StartTask changed claim-frozen authority:\nclaimed=%+v\nafter=%+v", claimedSnap, got)
		}
		if exact, total := dynamicTiebreakHistoryCounts(
			t, ctx, pool, taskID, workerID, supplierID, 0,
		); exact != 1 || total != 1 {
			t.Fatalf("claim/start replay history = exact %d/total %d, want 1/1", exact, total)
		}
	})

	t.Run("unrelated history cannot authorize running replay", func(t *testing.T) {
		ctx, store, pool, f, taskID := seedDynamicTiebreakStartFixture(t)
		workerID, supplierID := f.OtherWorkerID, f.OtherSupplierID
		ct, err := pool.Exec(ctx, `
			UPDATE tasks
			   SET status='running',started_at=now(),worker_id=$2,
			       execution_worker_id=$2,execution_supplier_id=$3,
			       execution_hw_class='apple_silicon_max',
			       execution_engine='candle',execution_build_hash='deadbeefdeadbeef'
			 WHERE id=$1 AND status='queued' AND claimed_by=$2`,
			taskID, workerID, supplierID)
		if err != nil {
			t.Fatalf("build hostile running tiebreak: %v", err)
		}
		if ct.RowsAffected() != 1 {
			t.Fatalf("hostile running transition changed %d rows, want 1", ct.RowsAffected())
		}
		if _, err := pool.Exec(ctx, `
			INSERT INTO task_execution_history (task_id,attempt,worker_id,supplier_id)
			VALUES ($1,0,$2,$3)`,
			taskID, f.WorkerID, f.SupplierID); err != nil {
			t.Fatalf("insert unrelated history: %v", err)
		}
		before := dynamicTiebreakSnapshot(t, ctx, pool, taskID)
		if before.Status != "running" ||
			before.ClaimedBy != workerID.String() ||
			before.WorkerID != workerID.String() ||
			before.ExecutionWorkerID != workerID.String() ||
			before.ExecutionSupplierID != supplierID.String() ||
			!before.IsRedundancy ||
			before.HedgedFrom != f.TaskIDs[0].String() ||
			before.RetryCount != 0 {
			t.Fatalf("hostile tiebreak does not reach the exact-history gate: %+v", before)
		}
		if exact, total := dynamicTiebreakHistoryCounts(
			t, ctx, pool, taskID, workerID, supplierID, 0,
		); exact != 0 || total != 1 {
			t.Fatalf("hostile history = exact %d/total %d, want 0/1", exact, total)
		}

		if err := store.StartTask(ctx, taskID, workerID, 0); !errors.Is(err, errNotFound) {
			t.Fatalf("unrelated-history replay error = %v, want errNotFound", err)
		}
		if got := dynamicTiebreakSnapshot(t, ctx, pool, taskID); got != before {
			t.Fatalf("unrelated-history fence mutated task:\nbefore=%+v\nafter=%+v", before, got)
		}
		if exact, total := dynamicTiebreakHistoryCounts(
			t, ctx, pool, taskID, workerID, supplierID, 0,
		); exact != 0 || total != 1 {
			t.Fatalf("fenced history = exact %d/total %d, want 0/1", exact, total)
		}

		if _, err := pool.Exec(ctx, `
			INSERT INTO task_execution_history (task_id,attempt,worker_id,supplier_id)
			VALUES ($1,0,$2,$3)`,
			taskID, workerID, supplierID); err != nil {
			t.Fatalf("insert exact replay history: %v", err)
		}
		if err := store.StartTask(ctx, taskID, workerID, 0); err != nil {
			t.Fatalf("exact-history running replay: %v", err)
		}
		if got := dynamicTiebreakSnapshot(t, ctx, pool, taskID); got != before {
			t.Fatalf("exact running replay mutated task:\nbefore=%+v\nafter=%+v", before, got)
		}
		if exact, total := dynamicTiebreakHistoryCounts(
			t, ctx, pool, taskID, workerID, supplierID, 0,
		); exact != 1 || total != 2 {
			t.Fatalf("authorized history = exact %d/total %d, want 1/2", exact, total)
		}
	})
}

func TestFailTaskTxReleasesRunningOwnerAndFencesForeignIdentity(t *testing.T) {
	ctx, store, pool := openMoneyPathStore(t)
	f := seedMoneyPathFixture(t, ctx, store, pool, moneyPathSeedOpts{
		TaskCount: 1, TaskStatus: "running", ClaimWorker: true, SeedJob: true, SeedPlanRows: true,
	})
	taskID := f.TaskIDs[0]
	if _, err := pool.Exec(ctx,
		`UPDATE tasks SET claimed_at=now(),started_at=now() WHERE id=$1`,
		taskID); err != nil {
		t.Fatal(err)
	}
	report := FailureReport{
		Class: "internal_error", Message: "start_task failed after bounded retries",
		Backend: "embed", Model: "all-minilm-l6-v2", DurationMS: 1400,
	}
	beforeLedger := countBuyerLedger(t, ctx, pool, f.BuyerID)

	outcome, err := store.FailTaskTx(ctx, taskID, f.OtherWorkerID, 0, report)
	if outcome != FailNoop || !errors.Is(err, errNotOwner) {
		t.Fatalf("foreign failure report = (%q,%v), want noop/errNotOwner", outcome, err)
	}
	outcome, err = store.FailTaskTx(ctx, taskID, f.WorkerID, 1, report)
	if outcome != FailNoop || !errors.Is(err, errNotOwner) {
		t.Fatalf("wrong-attempt failure report = (%q,%v), want noop/errNotOwner", outcome, err)
	}
	var failures int
	if err := pool.QueryRow(ctx,
		`SELECT count(*) FROM task_failures WHERE task_id=$1`, taskID).Scan(&failures); err != nil {
		t.Fatal(err)
	}
	if failures != 0 {
		t.Fatalf("fenced reports wrote %d failure rows", failures)
	}

	outcome, err = store.FailTaskTx(ctx, taskID, f.WorkerID, 0, report)
	if err != nil || outcome != FailRequeued {
		t.Fatalf("exact-owner running failure report = (%q,%v), want requeued/nil", outcome, err)
	}
	var (
		status               string
		claimedBy, workerID  *uuid.UUID
		retryCount           int
		visibleAfterDatabase bool
	)
	if err := pool.QueryRow(ctx,
		`SELECT status,claimed_by,worker_id,retry_count,visible_at > now()
		   FROM tasks WHERE id=$1`, taskID,
	).Scan(&status, &claimedBy, &workerID, &retryCount, &visibleAfterDatabase); err != nil {
		t.Fatal(err)
	}
	if status != "retrying" || claimedBy != nil || workerID != nil ||
		retryCount != 1 || !visibleAfterDatabase {
		t.Fatalf("released task = status=%q claimed=%v worker=%v retry=%d future_visible=%t",
			status, claimedBy, workerID, retryCount, visibleAfterDatabase)
	}
	var (
		failureClass                        string
		retryable, buyerFault               bool
		failureWorker, failureJob, eventJob uuid.UUID
	)
	if err := pool.QueryRow(ctx,
		`SELECT failure_class,retryable,buyer_fault,worker_id,job_id
		   FROM task_failures WHERE task_id=$1`, taskID,
	).Scan(&failureClass, &retryable, &buyerFault, &failureWorker, &failureJob); err != nil {
		t.Fatal(err)
	}
	if failureClass != "internal_error" || !retryable || buyerFault ||
		failureWorker != f.WorkerID || failureJob != f.JobID {
		t.Fatalf("persisted failure = class=%q retryable=%t buyer_fault=%t worker=%s job=%s",
			failureClass, retryable, buyerFault, failureWorker, failureJob)
	}
	if err := pool.QueryRow(ctx,
		`SELECT job_id FROM job_events
		  WHERE task_id=$1 AND event='task_requeued'`, taskID,
	).Scan(&eventJob); err != nil {
		t.Fatal(err)
	}
	if eventJob != f.JobID {
		t.Fatalf("requeue event job=%s, want %s", eventJob, f.JobID)
	}
	if got := countBuyerLedger(t, ctx, pool, f.BuyerID); got != beforeLedger {
		t.Fatalf("start-failure release changed buyer ledger rows: before=%d after=%d",
			beforeLedger, got)
	}
}

type taskLeaseProjection struct {
	Status            string
	ClaimedBy         *uuid.UUID
	ClaimedAt         *time.Time
	WorkerID          *uuid.UUID
	ExecutionWorker   *uuid.UUID
	ExecutionSupplier *uuid.UUID
	ExecutionHW       *string
	ExecutionEngine   *string
	ExecutionBuild    *string
	RetryCount        int
}

func readTaskLeaseProjection(
	t *testing.T, ctx context.Context, pool *pgxpool.Pool, taskID uuid.UUID,
) taskLeaseProjection {
	t.Helper()
	var out taskLeaseProjection
	if err := pool.QueryRow(ctx, `
		SELECT status,claimed_by,claimed_at,worker_id,
		       execution_worker_id,execution_supplier_id,execution_hw_class,
		       execution_engine,execution_build_hash,retry_count
		  FROM tasks WHERE id=$1`, taskID).
		Scan(&out.Status, &out.ClaimedBy, &out.ClaimedAt, &out.WorkerID,
			&out.ExecutionWorker, &out.ExecutionSupplier, &out.ExecutionHW,
			&out.ExecutionEngine, &out.ExecutionBuild, &out.RetryCount); err != nil {
		t.Fatal(err)
	}
	return out
}

func TestFailTaskTxTerminalReleasesLeaseAndSettlesOnce(t *testing.T) {
	ctx, store, pool := openMoneyPathStore(t)
	f := seedMoneyPathFixture(t, ctx, store, pool, moneyPathSeedOpts{
		TaskCount: 1, TaskStatus: "running", ClaimWorker: true, SeedJob: true, SeedPlanRows: true,
	})
	taskID := f.TaskIDs[0]
	if _, err := pool.Exec(ctx,
		`UPDATE tasks SET claimed_at=now(),started_at=now() WHERE id=$1`, taskID); err != nil {
		t.Fatal(err)
	}
	beforeLedger := countBuyerLedger(t, ctx, pool, f.BuyerID)
	report := FailureReport{
		Class: "bad_input", Message: "buyer document cannot be decoded",
		Backend: "embed", Model: "all-minilm-l6-v2", DurationMS: 900,
	}

	outcome, err := store.FailTaskTx(ctx, taskID, f.WorkerID, 0, report)
	if err != nil || outcome != FailTerminal {
		t.Fatalf("terminal failure = (%q,%v), want failed/nil", outcome, err)
	}
	projection := readTaskLeaseProjection(t, ctx, pool, taskID)
	if projection.Status != "failed" || projection.ClaimedBy != nil ||
		projection.ClaimedAt != nil || projection.WorkerID != nil ||
		projection.ExecutionWorker == nil || *projection.ExecutionWorker != f.WorkerID ||
		projection.RetryCount != 0 {
		t.Fatalf("terminal task projection = %+v, want failed/released with execution identity", projection)
	}
	var jobStatus string
	if err := pool.QueryRow(ctx, `SELECT status FROM jobs WHERE id=$1`, f.JobID).Scan(&jobStatus); err != nil {
		t.Fatal(err)
	}
	if jobStatus != "failed" {
		t.Fatalf("job status after terminal task failure = %q, want failed", jobStatus)
	}
	var failures, jobFailedEvents int
	if err := pool.QueryRow(ctx,
		`SELECT count(*) FROM task_failures WHERE task_id=$1`, taskID).Scan(&failures); err != nil {
		t.Fatal(err)
	}
	if err := pool.QueryRow(ctx,
		`SELECT count(*) FROM job_events WHERE job_id=$1 AND event='job_failed'`,
		f.JobID).Scan(&jobFailedEvents); err != nil {
		t.Fatal(err)
	}
	if failures != 1 || jobFailedEvents != 1 {
		t.Fatalf("terminal failure durable effects = failures %d/job_failed %d, want 1/1",
			failures, jobFailedEvents)
	}
	if got := countBuyerLedger(t, ctx, pool, f.BuyerID); got != beforeLedger {
		t.Fatalf("terminal failure minted ledger rows: before=%d after=%d", beforeLedger, got)
	}

	replay, err := store.FailTaskTx(ctx, taskID, f.WorkerID, 0, report)
	if replay != FailNoop || !errors.Is(err, errNotOwner) {
		t.Fatalf("released terminal replay = (%q,%v), want noop/errNotOwner", replay, err)
	}
	if err := pool.QueryRow(ctx,
		`SELECT count(*) FROM task_failures WHERE task_id=$1`, taskID).Scan(&failures); err != nil {
		t.Fatal(err)
	}
	if failures != 1 {
		t.Fatalf("terminal replay wrote %d failure rows, want 1", failures)
	}
}

func TestFailTaskTxTerminalPendingVerificationIsAtomicAndRetryable(t *testing.T) {
	ctx, store, pool := openMoneyPathStore(t)
	f := seedMoneyPathFixture(t, ctx, store, pool, moneyPathSeedOpts{
		TaskCount: 2, TaskStatus: "running", ClaimWorker: true, SeedJob: true, SeedPlanRows: true,
	})
	taskID, siblingID := f.TaskIDs[0], f.TaskIDs[1]
	if _, err := pool.Exec(ctx,
		`UPDATE tasks SET claimed_at=now(),started_at=now() WHERE id=ANY($1::uuid[])`,
		f.TaskIDs); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx,
		`UPDATE tasks SET status='verifying' WHERE id=$1`, siblingID); err != nil {
		t.Fatal(err)
	}
	report := FailureReport{
		Class: "bad_input", Message: "terminal input failure while sibling verifies",
		Backend: "embed", Model: "all-minilm-l6-v2", DurationMS: 900,
	}

	outcome, err := store.FailTaskTx(ctx, taskID, f.WorkerID, 0, report)
	if outcome != FailNoop || !errors.Is(err, ErrJobVerificationPending) {
		t.Fatalf("pending terminal failure = (%q,%v), want noop/ErrJobVerificationPending",
			outcome, err)
	}
	pending := readTaskLeaseProjection(t, ctx, pool, taskID)
	if pending.Status != "running" || pending.ClaimedBy == nil ||
		*pending.ClaimedBy != f.WorkerID || pending.ClaimedAt == nil ||
		pending.WorkerID == nil || *pending.WorkerID != f.WorkerID {
		t.Fatalf("pending terminal failure was not fully rolled back: %+v", pending)
	}
	var failures int
	if err := pool.QueryRow(ctx,
		`SELECT count(*) FROM task_failures WHERE task_id=$1`, taskID).Scan(&failures); err != nil {
		t.Fatal(err)
	}
	if failures != 0 {
		t.Fatalf("pending terminal failure left %d failure rows", failures)
	}

	if _, err := pool.Exec(ctx, `
		UPDATE tasks
		   SET status='failed',claimed_by=NULL,claimed_at=NULL,worker_id=NULL
		 WHERE id=$1`, siblingID); err != nil {
		t.Fatal(err)
	}
	outcome, err = store.FailTaskTx(ctx, taskID, f.WorkerID, 0, report)
	if err != nil || outcome != FailTerminal {
		t.Fatalf("terminal failure retry after verification drained = (%q,%v), want failed/nil",
			outcome, err)
	}
	released := readTaskLeaseProjection(t, ctx, pool, taskID)
	if released.Status != "failed" || released.ClaimedBy != nil ||
		released.ClaimedAt != nil || released.WorkerID != nil {
		t.Fatalf("retried terminal failure did not release lease: %+v", released)
	}
}

func TestWorkerFailVerificationPendingWireIsRetryableAndAtomic(t *testing.T) {
	ctx, store, pool := openIsolatedMoneyPathStore(t)
	f := seedMoneyPathFixture(t, ctx, store, pool, moneyPathSeedOpts{
		TaskCount: 2, TaskStatus: "running", ClaimWorker: true,
		SeedJob: true, SeedPlanRows: true,
	})
	taskID, siblingID := f.TaskIDs[0], f.TaskIDs[1]
	if _, err := pool.Exec(ctx, `
		UPDATE tasks
		   SET claimed_at=now(),started_at=now()
		 WHERE id=ANY($1::uuid[])`, f.TaskIDs); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx,
		`UPDATE tasks SET status='verifying' WHERE id=$1`, siblingID); err != nil {
		t.Fatal(err)
	}
	body, err := json.Marshal(FailureReport{
		Class: "bad_input", Message: "terminal input failure while sibling verifies",
		Backend: "embed", Model: "all-minilm-l6-v2", DurationMS: 900,
	})
	if err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequest(
		http.MethodPost, "/v1/worker/task/"+taskID.String()+"/fail",
		strings.NewReader(string(body)),
	)
	req.SetPathValue("id", taskID.String())
	req.Header.Set(taskAttemptHeaderName, "0")
	req = req.WithContext(context.WithValue(
		req.Context(), ctxWorker, &WorkerAuth{WorkerID: f.WorkerID},
	))
	rec := httptest.NewRecorder()

	(&Server{store: store}).handleWorkerFail(rec, req)

	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("pending fail status=%d body=%s, want 503", rec.Code, rec.Body.String())
	}
	if retryAfter := rec.Header().Get("Retry-After"); retryAfter == "" {
		t.Fatal("pending fail response omitted Retry-After")
	}
	var apiErr APIError
	if err := json.Unmarshal(rec.Body.Bytes(), &apiErr); err != nil {
		t.Fatalf("decode pending fail response: %v body=%s", err, rec.Body.String())
	}
	if apiErr.Code != ErrCodeUnavailable || apiErr.Action != ActionRetryAfter ||
		apiErr.Error != ErrJobVerificationPending.Error() {
		t.Fatalf("pending fail response=%+v, want unavailable/retry_after/%q",
			apiErr, ErrJobVerificationPending)
	}
	projection := readTaskLeaseProjection(t, ctx, pool, taskID)
	if projection.Status != "running" || projection.ClaimedBy == nil ||
		*projection.ClaimedBy != f.WorkerID || projection.ClaimedAt == nil ||
		projection.WorkerID == nil || *projection.WorkerID != f.WorkerID {
		t.Fatalf("pending wire response changed retained lease: %+v", projection)
	}
	var failures int
	if err := pool.QueryRow(ctx,
		`SELECT count(*) FROM task_failures WHERE task_id=$1`, taskID).Scan(&failures); err != nil {
		t.Fatal(err)
	}
	if failures != 0 {
		t.Fatalf("pending wire response retained %d rolled-back failure rows", failures)
	}
}

func TestConcurrentTerminalFailuresReleaseBothLeasesWithoutDeadlock(t *testing.T) {
	ctx, store, pool := openMoneyPathStore(t)
	f := seedMoneyPathFixture(t, ctx, store, pool, moneyPathSeedOpts{
		TaskCount: 2, TaskStatus: "running", ClaimWorker: true, SeedJob: true, SeedPlanRows: true,
	})
	if _, err := pool.Exec(ctx,
		`UPDATE tasks SET claimed_at=now(),started_at=now() WHERE id=ANY($1::uuid[])`,
		f.TaskIDs); err != nil {
		t.Fatal(err)
	}
	beforeLedger := countBuyerLedger(t, ctx, pool, f.BuyerID)
	report := FailureReport{
		Class: "bad_input", Message: "two chunks independently rejected input",
		Backend: "embed", Model: "all-minilm-l6-v2", DurationMS: 900,
	}
	type result struct {
		outcome FailOutcome
		err     error
	}
	results := make(chan result, len(f.TaskIDs))
	var wg sync.WaitGroup
	for _, taskID := range f.TaskIDs {
		wg.Add(1)
		go func(taskID uuid.UUID) {
			defer wg.Done()
			outcome, err := store.FailTaskTx(ctx, taskID, f.WorkerID, 0, report)
			results <- result{outcome: outcome, err: err}
		}(taskID)
	}
	wg.Wait()
	close(results)
	for result := range results {
		if result.err != nil || result.outcome != FailTerminal {
			t.Fatalf("concurrent terminal failure = (%q,%v), want failed/nil",
				result.outcome, result.err)
		}
	}
	for _, taskID := range f.TaskIDs {
		projection := readTaskLeaseProjection(t, ctx, pool, taskID)
		if projection.Status != "failed" || projection.ClaimedBy != nil ||
			projection.ClaimedAt != nil || projection.WorkerID != nil {
			t.Fatalf("concurrent terminal task %s retained lease: %+v", taskID, projection)
		}
	}
	var failures, jobFailedEvents int
	if err := pool.QueryRow(ctx,
		`SELECT count(*) FROM task_failures WHERE job_id=$1`, f.JobID).Scan(&failures); err != nil {
		t.Fatal(err)
	}
	if err := pool.QueryRow(ctx,
		`SELECT count(*) FROM job_events WHERE job_id=$1 AND event='job_failed'`,
		f.JobID).Scan(&jobFailedEvents); err != nil {
		t.Fatal(err)
	}
	if failures != 2 || jobFailedEvents != 1 {
		t.Fatalf("concurrent effects = failures %d/job_failed %d, want 2/1",
			failures, jobFailedEvents)
	}
	if got := countBuyerLedger(t, ctx, pool, f.BuyerID); got != beforeLedger {
		t.Fatalf("concurrent terminal failures changed ledger rows: before=%d after=%d",
			beforeLedger, got)
	}
}

func TestFailTaskAndSettleJobReleasesLeaseAndHonorsPending(t *testing.T) {
	t.Run("success and already-terminal task replay", func(t *testing.T) {
		ctx, store, pool := openMoneyPathStore(t)
		f := seedMoneyPathFixture(t, ctx, store, pool, moneyPathSeedOpts{
			TaskCount: 1, TaskStatus: "running", ClaimWorker: true, SeedJob: true, SeedPlanRows: true,
		})
		taskID := f.TaskIDs[0]
		beforeLedger := countBuyerLedger(t, ctx, pool, f.BuyerID)
		if _, err := pool.Exec(ctx,
			`UPDATE tasks SET claimed_at=now(),started_at=now() WHERE id=$1`, taskID); err != nil {
			t.Fatal(err)
		}
		if err := store.FailTaskAndSettleJob(ctx, taskID, f.JobID); err != nil {
			t.Fatalf("FailTaskAndSettleJob: %v", err)
		}
		projection := readTaskLeaseProjection(t, ctx, pool, taskID)
		if projection.Status != "failed" || projection.ClaimedBy != nil ||
			projection.ClaimedAt != nil || projection.WorkerID != nil ||
			projection.ExecutionWorker == nil || *projection.ExecutionWorker != f.WorkerID {
			t.Fatalf("reaper terminal projection = %+v", projection)
		}
		var jobStatus string
		if err := pool.QueryRow(ctx,
			`SELECT status FROM jobs WHERE id=$1`, f.JobID).Scan(&jobStatus); err != nil {
			t.Fatal(err)
		}
		if jobStatus != "failed" {
			t.Fatalf("reaper settlement left job status %q, want failed", jobStatus)
		}
		if got := countBuyerLedger(t, ctx, pool, f.BuyerID); got != beforeLedger {
			t.Fatalf("reaper terminal failure changed ledger rows: before=%d after=%d",
				beforeLedger, got)
		}
		if err := store.FailTaskAndSettleJob(ctx, taskID, f.JobID); err != nil {
			t.Fatalf("replayed FailTaskAndSettleJob: %v", err)
		}
		if err := pool.QueryRow(ctx,
			`SELECT status FROM jobs WHERE id=$1`, f.JobID).Scan(&jobStatus); err != nil {
			t.Fatal(err)
		}
		if jobStatus != "failed" {
			t.Fatalf("replayed reaper settlement changed job status to %q", jobStatus)
		}
		if got := countBuyerLedger(t, ctx, pool, f.BuyerID); got != beforeLedger {
			t.Fatalf("replayed reaper terminal failure changed ledger rows: before=%d after=%d",
				beforeLedger, got)
		}
	})

	t.Run("pending verification rolls back", func(t *testing.T) {
		ctx, store, pool := openMoneyPathStore(t)
		f := seedMoneyPathFixture(t, ctx, store, pool, moneyPathSeedOpts{
			TaskCount: 2, TaskStatus: "running", ClaimWorker: true, SeedJob: true, SeedPlanRows: true,
		})
		taskID, siblingID := f.TaskIDs[0], f.TaskIDs[1]
		if _, err := pool.Exec(ctx,
			`UPDATE tasks SET claimed_at=now(),started_at=now() WHERE id=ANY($1::uuid[])`,
			f.TaskIDs); err != nil {
			t.Fatal(err)
		}
		if _, err := pool.Exec(ctx,
			`UPDATE tasks SET status='verifying' WHERE id=$1`, siblingID); err != nil {
			t.Fatal(err)
		}
		err := store.FailTaskAndSettleJob(ctx, taskID, f.JobID)
		if !errors.Is(err, ErrJobVerificationPending) {
			t.Fatalf("pending FailTaskAndSettleJob error = %v, want ErrJobVerificationPending", err)
		}
		projection := readTaskLeaseProjection(t, ctx, pool, taskID)
		if projection.Status != "running" || projection.ClaimedBy == nil ||
			*projection.ClaimedBy != f.WorkerID || projection.ClaimedAt == nil ||
			projection.WorkerID == nil || *projection.WorkerID != f.WorkerID {
			t.Fatalf("pending reaper terminal failure was not rolled back: %+v", projection)
		}
	})
}

func TestStaleReaperPendingVerificationDoesNotStarveLaterJobs(t *testing.T) {
	ctx, store, pool := openIsolatedMoneyPathStore(t)
	pending := seedMoneyPathFixture(t, ctx, store, pool, moneyPathSeedOpts{
		TaskCount: 2, TaskStatus: "running", ClaimWorker: true,
		SeedJob: true, SeedPlanRows: true,
	})
	later := seedMoneyPathFixture(t, ctx, store, pool, moneyPathSeedOpts{
		TaskCount: 1, TaskStatus: "running", ClaimWorker: true,
		SeedJob: true, SeedPlanRows: true,
	})
	pendingTask, verifyingSibling := pending.TaskIDs[0], pending.TaskIDs[1]
	laterTask := later.TaskIDs[0]
	if _, err := pool.Exec(ctx, `
		UPDATE tasks
		   SET claimed_at=now()-interval '3 hours',
		       started_at=now()-interval '3 hours',
		       retry_count=$2
		 WHERE id=$1`, pendingTask, maxTaskRetries); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `
		UPDATE tasks
		   SET status='verifying',claimed_at=now(),started_at=now()
		 WHERE id=$1`, verifyingSibling); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `
		UPDATE tasks
		   SET claimed_at=now()-interval '2 hours',
		       started_at=now()-interval '2 hours'
		 WHERE id=$1`, laterTask); err != nil {
		t.Fatal(err)
	}

	workers := &Workers{store: store}
	if err := workers.requeueStaleTasks(ctx); err != nil {
		t.Fatalf("stale sweep treated verification pending as fatal: %v", err)
	}
	pendingProjection := readTaskLeaseProjection(t, ctx, pool, pendingTask)
	if pendingProjection.Status != "running" ||
		pendingProjection.ClaimedBy == nil ||
		*pendingProjection.ClaimedBy != pending.WorkerID ||
		pendingProjection.RetryCount != maxTaskRetries {
		t.Fatalf("pending oldest task did not remain atomically owned: %+v",
			pendingProjection)
	}
	laterProjection := readTaskLeaseProjection(t, ctx, pool, laterTask)
	if laterProjection.Status != "queued" ||
		laterProjection.ClaimedBy != nil ||
		laterProjection.WorkerID != nil ||
		laterProjection.RetryCount != 1 {
		t.Fatalf("later stale job was starved behind pending verification: %+v",
			laterProjection)
	}

	if _, err := pool.Exec(ctx, `
		UPDATE tasks
		   SET status='failed',claimed_by=NULL,claimed_at=NULL,worker_id=NULL
		 WHERE id=$1`, verifyingSibling); err != nil {
		t.Fatal(err)
	}
	if err := workers.requeueStaleTasks(ctx); err != nil {
		t.Fatalf("stale sweep after verification drained: %v", err)
	}
	pendingProjection = readTaskLeaseProjection(t, ctx, pool, pendingTask)
	if pendingProjection.Status != "failed" ||
		pendingProjection.ClaimedBy != nil ||
		pendingProjection.WorkerID != nil {
		t.Fatalf("deferred stale terminal failure did not converge: %+v",
			pendingProjection)
	}
	var pendingJobStatus string
	if err := pool.QueryRow(ctx,
		`SELECT status FROM jobs WHERE id=$1`, pending.JobID).Scan(&pendingJobStatus); err != nil {
		t.Fatal(err)
	}
	if pendingJobStatus != "failed" {
		t.Fatalf("deferred stale job status=%q, want failed", pendingJobStatus)
	}
}

func TestStaleRecoveryFencesTerminalParentAtSelectionAndMutation(t *testing.T) {
	t.Run("terminal parent cannot select or requeue running residual", func(t *testing.T) {
		ctx, store, pool := openMoneyPathStore(t)
		f := seedMoneyPathFixture(t, ctx, store, pool, moneyPathSeedOpts{
			TaskCount: 1, TaskStatus: "running", ClaimWorker: true, SeedJob: true, SeedPlanRows: true,
		})
		taskID := f.TaskIDs[0]
		if _, err := pool.Exec(ctx,
			`UPDATE tasks SET claimed_at=now()-interval '2 hours',started_at=now()-interval '2 hours'
			  WHERE id=$1`, taskID); err != nil {
			t.Fatal(err)
		}
		if _, err := pool.Exec(ctx, `UPDATE jobs SET status='failed' WHERE id=$1`, f.JobID); err != nil {
			t.Fatal(err)
		}
		stale, err := store.StaleRunningTasks(ctx, time.Minute, 100)
		if err != nil {
			t.Fatal(err)
		}
		for _, item := range stale {
			if item.ID == taskID {
				t.Fatalf("terminal-parent task %s was selected as stale", taskID)
			}
		}
		if err := store.RequeueStaleTask(ctx, taskID, time.Second); err != nil {
			t.Fatal(err)
		}
		projection := readTaskLeaseProjection(t, ctx, pool, taskID)
		if projection.Status != "running" || projection.RetryCount != 0 ||
			projection.ClaimedBy == nil || *projection.ClaimedBy != f.WorkerID {
			t.Fatalf("terminal-parent stale requeue mutated task: %+v", projection)
		}
		if err := store.FailTaskAndSettleJob(ctx, taskID, f.JobID); err != nil {
			t.Fatalf("terminal-parent stale cleanup: %v", err)
		}
		cleaned := readTaskLeaseProjection(t, ctx, pool, taskID)
		if cleaned.Status != "failed" || cleaned.ClaimedBy != nil ||
			cleaned.ClaimedAt != nil || cleaned.WorkerID != nil {
			t.Fatalf("terminal-parent stale cleanup retained lease: %+v", cleaned)
		}
	})

	t.Run("active parent still selects and requeues", func(t *testing.T) {
		ctx, store, pool := openMoneyPathStore(t)
		f := seedMoneyPathFixture(t, ctx, store, pool, moneyPathSeedOpts{
			TaskCount: 1, TaskStatus: "running", ClaimWorker: true, SeedJob: true, SeedPlanRows: true,
		})
		taskID := f.TaskIDs[0]
		if _, err := pool.Exec(ctx,
			`UPDATE tasks SET claimed_at=now()-interval '2 hours',started_at=now()-interval '2 hours'
			  WHERE id=$1`, taskID); err != nil {
			t.Fatal(err)
		}
		stale, err := store.StaleRunningTasks(ctx, time.Minute, 100)
		if err != nil {
			t.Fatal(err)
		}
		found := false
		for _, item := range stale {
			if item.ID == taskID {
				found = true
			}
		}
		if !found {
			t.Fatalf("active-parent task %s was not selected as stale", taskID)
		}
		if err := store.RequeueStaleTask(ctx, taskID, time.Second); err != nil {
			t.Fatal(err)
		}
		projection := readTaskLeaseProjection(t, ctx, pool, taskID)
		if projection.Status != "queued" || projection.RetryCount != 1 ||
			projection.ClaimedBy != nil || projection.ClaimedAt != nil ||
			projection.WorkerID != nil {
			t.Fatalf("active-parent stale requeue projection = %+v", projection)
		}
	})
}

func TestDetachUnfinishedTasksForTerminalJobMappingsAndIdempotency(t *testing.T) {
	testCases := []struct {
		name       string
		parent     string
		detached   int64
		wantStatus []string
	}{
		{
			name: "active parent untouched", parent: "running", detached: 0,
			wantStatus: []string{"queued", "retrying", "running", "complete", "failed", "cancelled"},
		},
		{
			name: "failed parent maps unfinished to failed", parent: "failed", detached: 3,
			wantStatus: []string{"failed", "failed", "failed", "complete", "failed", "cancelled"},
		},
		{
			name: "cancelled parent maps unfinished to cancelled", parent: "cancelled", detached: 3,
			wantStatus: []string{"cancelled", "cancelled", "cancelled", "complete", "failed", "cancelled"},
		},
		{
			name: "complete parent defensively fails residuals", parent: "complete", detached: 3,
			wantStatus: []string{"failed", "failed", "failed", "complete", "failed", "cancelled"},
		},
	}
	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			ctx, store, pool := openMoneyPathStore(t)
			f := seedMoneyPathFixture(t, ctx, store, pool, moneyPathSeedOpts{
				TaskCount: 6, TaskStatus: "running", ClaimWorker: true,
				SeedJob: true, SeedPlanRows: true,
			})
			seedStatuses := []string{"queued", "retrying", "running", "complete", "failed", "cancelled"}
			for i, status := range seedStatuses {
				retryCount := 0
				if status == "retrying" {
					retryCount = 2
				}
				if _, err := pool.Exec(ctx, `
					UPDATE tasks
					   SET status=$2,retry_count=$3,claimed_at=now(),started_at=now()
					 WHERE id=$1`, f.TaskIDs[i], status, retryCount); err != nil {
					t.Fatalf("seed task %d as %s: %v", i, status, err)
				}
			}
			if tc.parent != "running" {
				if _, err := pool.Exec(ctx,
					`UPDATE jobs SET status=$2 WHERE id=$1`, f.JobID, tc.parent); err != nil {
					t.Fatalf("seed parent %s: %v", tc.parent, err)
				}
			}

			beforeLedger := countBuyerLedger(t, ctx, pool, f.BuyerID)
			var beforeEvents, beforeFailures int
			if err := pool.QueryRow(ctx,
				`SELECT count(*) FROM job_events WHERE job_id=$1`, f.JobID).Scan(&beforeEvents); err != nil {
				t.Fatal(err)
			}
			if err := pool.QueryRow(ctx,
				`SELECT count(*) FROM task_failures WHERE job_id=$1`, f.JobID).Scan(&beforeFailures); err != nil {
				t.Fatal(err)
			}
			selected, err := store.TerminalJobsWithUnfinishedTasks(ctx, 100)
			if err != nil {
				t.Fatal(err)
			}
			found := false
			for _, jobID := range selected {
				found = found || jobID == f.JobID
			}
			if found != (tc.parent != "running") {
				t.Fatalf("terminal janitor selection found=%v for parent %s", found, tc.parent)
			}

			detached, err := store.DetachUnfinishedTasksForTerminalJob(ctx, f.JobID)
			if err != nil {
				t.Fatal(err)
			}
			if detached != tc.detached {
				t.Fatalf("detached rows = %d, want %d", detached, tc.detached)
			}
			for i, taskID := range f.TaskIDs {
				got := readTaskLeaseProjection(t, ctx, pool, taskID)
				if got.Status != tc.wantStatus[i] {
					t.Fatalf("task %d status = %q, want %q", i, got.Status, tc.wantStatus[i])
				}
				if i < 3 && tc.parent != "running" &&
					(got.ClaimedBy != nil || got.ClaimedAt != nil || got.WorkerID != nil) {
					t.Fatalf("detached task %d retained lease: %+v", i, got)
				}
				if i < 3 && tc.parent == "running" &&
					(got.ClaimedBy == nil || *got.ClaimedBy != f.WorkerID ||
						got.ClaimedAt == nil ||
						got.WorkerID == nil || *got.WorkerID != f.WorkerID) {
					t.Fatalf("active-parent task %d lost lease: %+v", i, got)
				}
				if got.ExecutionWorker == nil || *got.ExecutionWorker != f.WorkerID ||
					got.ExecutionSupplier == nil || *got.ExecutionSupplier != f.SupplierID ||
					got.ExecutionHW == nil || *got.ExecutionHW != "apple_silicon_max" ||
					got.ExecutionEngine == nil || *got.ExecutionEngine != "candle" ||
					got.ExecutionBuild == nil || *got.ExecutionBuild != "deadbeefdeadbeef" {
					t.Fatalf("task %d execution provenance changed: %+v", i, got)
				}
			}
			if got := readTaskLeaseProjection(t, ctx, pool, f.TaskIDs[1]).RetryCount; got != 2 {
				t.Fatalf("retrying task retry_count = %d, want preserved 2", got)
			}
			var jobStatus string
			if err := pool.QueryRow(ctx,
				`SELECT status FROM jobs WHERE id=$1`, f.JobID).Scan(&jobStatus); err != nil {
				t.Fatal(err)
			}
			if jobStatus != tc.parent {
				t.Fatalf("detach changed parent %s to %s", tc.parent, jobStatus)
			}
			if got := countBuyerLedger(t, ctx, pool, f.BuyerID); got != beforeLedger {
				t.Fatalf("detach changed buyer ledger rows: before=%d after=%d", beforeLedger, got)
			}
			var afterEvents, afterFailures int
			if err := pool.QueryRow(ctx,
				`SELECT count(*) FROM job_events WHERE job_id=$1`, f.JobID).Scan(&afterEvents); err != nil {
				t.Fatal(err)
			}
			if err := pool.QueryRow(ctx,
				`SELECT count(*) FROM task_failures WHERE job_id=$1`, f.JobID).Scan(&afterFailures); err != nil {
				t.Fatal(err)
			}
			if afterEvents != beforeEvents || afterFailures != beforeFailures {
				t.Fatalf("detach changed audit rows: events %d->%d failures %d->%d",
					beforeEvents, afterEvents, beforeFailures, afterFailures)
			}
			replay, err := store.DetachUnfinishedTasksForTerminalJob(ctx, f.JobID)
			if err != nil || replay != 0 {
				t.Fatalf("detach replay = (%d,%v), want 0/nil", replay, err)
			}
			selected, err = store.TerminalJobsWithUnfinishedTasks(ctx, 100)
			if err != nil {
				t.Fatal(err)
			}
			for _, jobID := range selected {
				if jobID == f.JobID {
					t.Fatalf("job %s remained selected after detach replay", f.JobID)
				}
			}
		})
	}
}

func TestDetachTerminalResidualIgnoresPendingButPreservesVerificationEvidence(t *testing.T) {
	ctx, store, pool := openMoneyPathStore(t)
	f := seedMoneyPathFixture(t, ctx, store, pool, moneyPathSeedOpts{
		TaskCount: 2, TaskStatus: "running", ClaimWorker: true,
		SeedJob: true, SeedPlanRows: true,
	})
	verifyingID, residualID := f.TaskIDs[0], f.TaskIDs[1]
	info, err := store.CompleteTaskTx(ctx, verifyingID, f.WorkerID, commitFor(f, verifyingID, 0))
	if err != nil || info == nil {
		t.Fatalf("seed verification work = (%+v,%v)", info, err)
	}
	if _, err := pool.Exec(ctx,
		`UPDATE tasks SET claimed_at=now(),started_at=now() WHERE id=$1`, residualID); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `UPDATE jobs SET status='failed' WHERE id=$1`, f.JobID); err != nil {
		t.Fatal(err)
	}

	var workID uuid.UUID
	var workStatus string
	if err := pool.QueryRow(ctx, `
		SELECT id,status
		  FROM verification_work
		 WHERE task_id=$1 AND attempt=0`, verifyingID).Scan(&workID, &workStatus); err != nil {
		t.Fatal(err)
	}
	detached, err := store.DetachUnfinishedTasksForTerminalJob(ctx, f.JobID)
	if err != nil || detached != 1 {
		t.Fatalf("terminal+pending detach = (%d,%v), want 1/nil", detached, err)
	}
	residual := readTaskLeaseProjection(t, ctx, pool, residualID)
	if residual.Status != "failed" || residual.ClaimedBy != nil ||
		residual.ClaimedAt != nil || residual.WorkerID != nil ||
		residual.ExecutionWorker == nil || *residual.ExecutionWorker != f.WorkerID {
		t.Fatalf("terminal+pending residual projection = %+v", residual)
	}
	afterVerifying := readTaskLeaseProjection(t, ctx, pool, verifyingID)
	if afterVerifying.Status != "verifying" ||
		afterVerifying.ClaimedBy == nil || *afterVerifying.ClaimedBy != f.WorkerID ||
		afterVerifying.WorkerID == nil || *afterVerifying.WorkerID != f.WorkerID ||
		afterVerifying.ExecutionWorker == nil ||
		*afterVerifying.ExecutionWorker != f.WorkerID ||
		afterVerifying.ExecutionSupplier == nil ||
		*afterVerifying.ExecutionSupplier != f.SupplierID {
		t.Fatalf("detach changed verifying evidence: %+v", afterVerifying)
	}
	var afterWorkStatus string
	if err := pool.QueryRow(ctx,
		`SELECT status FROM verification_work WHERE id=$1`, workID).Scan(&afterWorkStatus); err != nil {
		t.Fatal(err)
	}
	if afterWorkStatus != workStatus {
		t.Fatalf("detach changed verification work %s from %q to %q",
			workID, workStatus, afterWorkStatus)
	}
}

func TestTerminalTaskDetachJanitorRepairsRetainedResidual(t *testing.T) {
	ctx, store, pool := openMoneyPathStore(t)
	f := seedMoneyPathFixture(t, ctx, store, pool, moneyPathSeedOpts{
		TaskCount: 1, TaskStatus: "running", ClaimWorker: true,
		SeedJob: true, SeedPlanRows: true,
	})
	taskID := f.TaskIDs[0]
	if _, err := pool.Exec(ctx,
		`UPDATE tasks SET claimed_at=now(),started_at=now() WHERE id=$1`, taskID); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx,
		`UPDATE jobs SET status='failed' WHERE id=$1`, f.JobID); err != nil {
		t.Fatal(err)
	}
	workers := &Workers{store: store}
	if err := workers.detachTerminalJobTasks(ctx); err != nil {
		t.Fatal(err)
	}
	got := readTaskLeaseProjection(t, ctx, pool, taskID)
	if got.Status != "failed" || got.ClaimedBy != nil ||
		got.ClaimedAt != nil || got.WorkerID != nil {
		t.Fatalf("janitor retained terminal residual: %+v", got)
	}
	if err := workers.detachTerminalJobTasks(ctx); err != nil {
		t.Fatalf("janitor replay: %v", err)
	}
}

func TestConcurrentTerminalDetachPassesAreIdempotent(t *testing.T) {
	ctx, store, pool := openMoneyPathStore(t)
	f := seedMoneyPathFixture(t, ctx, store, pool, moneyPathSeedOpts{
		TaskCount: 3, TaskStatus: "running", ClaimWorker: true,
		SeedJob: true, SeedPlanRows: true,
	})
	if _, err := pool.Exec(ctx,
		`UPDATE tasks SET claimed_at=now(),started_at=now() WHERE id=ANY($1::uuid[])`,
		f.TaskIDs); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `UPDATE jobs SET status='failed' WHERE id=$1`, f.JobID); err != nil {
		t.Fatal(err)
	}

	type result struct {
		detached int64
		err      error
	}
	results := make(chan result, 2)
	for i := 0; i < 2; i++ {
		go func() {
			detached, err := store.DetachUnfinishedTasksForTerminalJob(ctx, f.JobID)
			results <- result{detached: detached, err: err}
		}()
	}
	var total int64
	for i := 0; i < 2; i++ {
		select {
		case got := <-results:
			if got.err != nil {
				t.Fatalf("concurrent detach: %v", got.err)
			}
			total += got.detached
		case <-time.After(15 * time.Second):
			t.Fatal("concurrent detach passes did not finish")
		}
	}
	if total != int64(len(f.TaskIDs)) {
		t.Fatalf("concurrent detach total = %d, want %d", total, len(f.TaskIDs))
	}
	for _, taskID := range f.TaskIDs {
		got := readTaskLeaseProjection(t, ctx, pool, taskID)
		if got.Status != "failed" || got.ClaimedBy != nil ||
			got.ClaimedAt != nil || got.WorkerID != nil {
			t.Fatalf("concurrent detach retained task %s: %+v", taskID, got)
		}
	}
}

func TestConcurrentTerminalDetachCompleteAndFailConvergesWithoutDeadlock(t *testing.T) {
	ctx, store, pool := openIsolatedMoneyPathStore(t)
	f := seedMoneyPathFixture(t, ctx, store, pool, moneyPathSeedOpts{
		TaskCount: 3, TaskStatus: "running", ClaimWorker: true,
		SeedJob: true, SeedPlanRows: true,
	})
	if _, err := pool.Exec(ctx,
		`UPDATE tasks SET claimed_at=now(),started_at=now() WHERE id=ANY($1::uuid[])`,
		f.TaskIDs); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `UPDATE jobs SET status='failed' WHERE id=$1`, f.JobID); err != nil {
		t.Fatal(err)
	}
	beforeLedger := countBuyerLedger(t, ctx, pool, f.BuyerID)
	report := FailureReport{
		Class: "bad_input", Message: "terminal parent concurrency probe",
		Backend: "embed", Model: "all-minilm-l6-v2", DurationMS: 1,
	}
	type result struct {
		kind     string
		outcome  FailOutcome
		detached int64
		err      error
	}
	start := make(chan struct{})
	results := make(chan result, 3)
	go func() {
		<-start
		_, err := store.CompleteTaskTx(ctx, f.TaskIDs[0], f.WorkerID,
			commitFor(f, f.TaskIDs[0], 0))
		results <- result{kind: "complete", err: err}
	}()
	go func() {
		<-start
		outcome, err := store.FailTaskTx(ctx, f.TaskIDs[1], f.WorkerID, 0, report)
		results <- result{kind: "fail", outcome: outcome, err: err}
	}()
	go func() {
		<-start
		detached, err := store.DetachUnfinishedTasksForTerminalJob(ctx, f.JobID)
		results <- result{kind: "detach", detached: detached, err: err}
	}()
	close(start)

	seen := make(map[string]result, 3)
	for i := 0; i < 3; i++ {
		select {
		case got := <-results:
			seen[got.kind] = got
		case <-time.After(15 * time.Second):
			t.Fatal("concurrent terminal detach/complete/fail did not finish")
		}
	}
	if got := seen["complete"]; !errors.Is(got.err, errNotFound) {
		t.Fatalf("complete under terminal parent error = %v, want errNotFound", got.err)
	}
	if got := seen["fail"]; !((got.outcome == FailTerminal && got.err == nil) ||
		(got.outcome == FailNoop && errors.Is(got.err, errNotOwner))) {
		t.Fatalf("concurrent fail result = (%q,%v)", got.outcome, got.err)
	}
	if got := seen["detach"]; got.err != nil {
		t.Fatalf("concurrent detach = (%d,%v)", got.detached, got.err)
	}
	if _, err := store.DetachUnfinishedTasksForTerminalJob(ctx, f.JobID); err != nil {
		t.Fatalf("convergence detach: %v", err)
	}
	for _, taskID := range f.TaskIDs {
		got := readTaskLeaseProjection(t, ctx, pool, taskID)
		if got.Status != "failed" || got.ClaimedBy != nil ||
			got.ClaimedAt != nil || got.WorkerID != nil {
			t.Fatalf("concurrent terminal task %s did not converge: %+v", taskID, got)
		}
	}
	var jobStatus string
	if err := pool.QueryRow(ctx, `SELECT status FROM jobs WHERE id=$1`, f.JobID).Scan(&jobStatus); err != nil {
		t.Fatal(err)
	}
	if jobStatus != "failed" {
		t.Fatalf("concurrent detach reopened parent to %q", jobStatus)
	}
	if got := countBuyerLedger(t, ctx, pool, f.BuyerID); got != beforeLedger {
		t.Fatalf("concurrent detach changed ledger rows: before=%d after=%d", beforeLedger, got)
	}
}

// --- CompleteTaskTx ---

func TestCompleteTaskTxHappyPathMovesTaskTerminal(t *testing.T) {
	ctx, store, pool := openMoneyPathStore(t)
	f := seedMoneyPathFixture(t, ctx, store, pool, moneyPathSeedOpts{
		TaskCount: 1, TaskStatus: "running", ClaimWorker: true, SeedJob: true, SeedPlanRows: true,
	})
	taskID := f.TaskIDs[0]
	beforeLedger := countBuyerLedger(t, ctx, pool, f.BuyerID)

	info, err := store.CompleteTaskTx(ctx, taskID, f.WorkerID, commitFor(f, taskID, 0))
	if err != nil {
		t.Fatalf("CompleteTaskTx: %v", err)
	}
	if info == nil || info.TaskID != taskID {
		t.Fatalf("unexpected commit info: %+v", info)
	}
	if got := taskStatus(t, ctx, pool, taskID); got != "verifying" {
		t.Fatalf("task status after complete = %q, want verifying", got)
	}
	var work int
	if err := pool.QueryRow(ctx,
		`SELECT count(*) FROM verification_work WHERE task_id=$1 AND attempt=0`, taskID).Scan(&work); err != nil {
		t.Fatal(err)
	}
	if work != 1 {
		t.Fatalf("verification_work rows = %d, want 1", work)
	}
	if n := countBuyerLedger(t, ctx, pool, f.BuyerID); n != beforeLedger {
		t.Fatalf("CompleteTaskTx minted ledger rows: before=%d after=%d", beforeLedger, n)
	}

	// A commit can be durable even when every acknowledgement is lost. The
	// agent then reports its commit protocol error through fail_task. Once the
	// task is verifying, that exact owner+attempt failure must be inert: it
	// cannot requeue delivered work, add a failure row, duplicate verification,
	// or create a money effect.
	outcome, err := store.FailTaskTx(ctx, taskID, f.WorkerID, 0, FailureReport{
		Class: "internal_error", Message: "commit_task failed after bounded retries",
		Backend: "embed", Model: "all-minilm-l6-v2", DurationMS: 1400,
	})
	if err != nil || outcome != FailNoop {
		t.Fatalf("failure report after durable commit = (%q,%v), want noop/nil", outcome, err)
	}
	if got := taskStatus(t, ctx, pool, taskID); got != "verifying" {
		t.Fatalf("post-commit failure report changed status to %q", got)
	}
	var failures int
	if err := pool.QueryRow(ctx,
		`SELECT count(*) FROM task_failures WHERE task_id=$1`, taskID).Scan(&failures); err != nil {
		t.Fatal(err)
	}
	if failures != 0 {
		t.Fatalf("post-commit failure report wrote %d failure rows", failures)
	}
	if err := pool.QueryRow(ctx,
		`SELECT count(*) FROM verification_work WHERE task_id=$1 AND attempt=0`, taskID).Scan(&work); err != nil {
		t.Fatal(err)
	}
	if work != 1 {
		t.Fatalf("post-commit failure report changed verification work rows to %d", work)
	}
	if n := countBuyerLedger(t, ctx, pool, f.BuyerID); n != beforeLedger {
		t.Fatalf("post-commit failure report changed ledger rows: before=%d after=%d", beforeLedger, n)
	}
}

func TestCompleteTaskTxFailClosedWrongWorkerAttemptAndTerminal(t *testing.T) {
	ctx, store, pool := openMoneyPathStore(t)
	f := seedMoneyPathFixture(t, ctx, store, pool, moneyPathSeedOpts{
		TaskCount: 1, TaskStatus: "running", ClaimWorker: true, SeedJob: true, SeedPlanRows: true,
	})
	taskID := f.TaskIDs[0]
	ledgerBefore := countBuyerLedger(t, ctx, pool, f.BuyerID)

	// Wrong worker.
	if _, err := store.CompleteTaskTx(ctx, taskID, f.OtherWorkerID, commitFor(f, taskID, 0)); !errors.Is(err, errNotFound) {
		t.Fatalf("wrong worker complete error = %v, want errNotFound", err)
	}
	if got := taskStatus(t, ctx, pool, taskID); got != "running" {
		t.Fatalf("wrong worker mutated status to %q", got)
	}

	// Wrong attempt.
	if _, err := store.CompleteTaskTx(ctx, taskID, f.WorkerID, commitFor(f, taskID, 1)); !errors.Is(err, errNotFound) {
		t.Fatalf("wrong attempt complete error = %v, want errNotFound", err)
	}
	if got := taskStatus(t, ctx, pool, taskID); got != "running" {
		t.Fatalf("wrong attempt mutated status to %q", got)
	}

	// Happy complete, then already-terminal (complete status) must fail closed.
	if _, err := store.CompleteTaskTx(ctx, taskID, f.WorkerID, commitFor(f, taskID, 0)); err != nil {
		t.Fatalf("first complete: %v", err)
	}
	if _, err := pool.Exec(ctx, `UPDATE tasks SET status='complete', completed_at=now() WHERE id=$1`, taskID); err != nil {
		t.Fatal(err)
	}
	if _, err := store.CompleteTaskTx(ctx, taskID, f.WorkerID, commitFor(f, taskID, 0)); !errors.Is(err, errNotFound) {
		t.Fatalf("already-terminal complete error = %v, want errNotFound", err)
	}
	if n := countBuyerLedger(t, ctx, pool, f.BuyerID); n != ledgerBefore {
		t.Fatalf("fail-closed completes minted ledger credit: before=%d after=%d", ledgerBefore, n)
	}
	var work int
	if err := pool.QueryRow(ctx,
		`SELECT count(*) FROM verification_work WHERE task_id=$1`, taskID).Scan(&work); err != nil {
		t.Fatal(err)
	}
	if work != 1 {
		t.Fatalf("fail-closed path created extra verification_work: %d", work)
	}
}

func TestCompleteTaskTxConcurrentCompletesYieldOneSuccessEffect(t *testing.T) {
	ctx, store, pool := openMoneyPathStore(t)
	f := seedMoneyPathFixture(t, ctx, store, pool, moneyPathSeedOpts{
		TaskCount: 1, TaskStatus: "running", ClaimWorker: true, SeedJob: true, SeedPlanRows: true,
	})
	taskID := f.TaskIDs[0]
	commit := commitFor(f, taskID, 0)

	const contenders = 8
	errs := make(chan error, contenders)
	var wg sync.WaitGroup
	for i := 0; i < contenders; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, err := store.CompleteTaskTx(ctx, taskID, f.WorkerID, commit)
			errs <- err
		}()
	}
	wg.Wait()
	close(errs)

	succeeded, failed := 0, 0
	for err := range errs {
		if err == nil {
			succeeded++
			continue
		}
		failed++
		// Idempotent re-entry returns nil; any hard error is unexpected here.
		t.Fatalf("concurrent complete unexpected error: %v", err)
	}
	if succeeded < 1 {
		t.Fatal("no concurrent complete succeeded")
	}
	// Durable effect: exactly one verification_work row, one verifying task.
	var work int
	if err := pool.QueryRow(ctx,
		`SELECT count(*) FROM verification_work WHERE task_id=$1 AND attempt=0`, taskID).Scan(&work); err != nil {
		t.Fatal(err)
	}
	if work != 1 {
		t.Fatalf("concurrent completes left %d verification_work rows, want 1", work)
	}
	if got := taskStatus(t, ctx, pool, taskID); got != "verifying" {
		t.Fatalf("status after concurrent completes = %q", got)
	}
	if n := countBuyerLedger(t, ctx, pool, f.BuyerID); n != 0 {
		t.Fatalf("concurrent completes minted ledger rows: %d", n)
	}
	// Same-worker re-entry is intentionally idempotent (all callers may observe
	// success). The money-safety property is the singular durable work row.
	_ = failed
}

// --- FinalizeJobTx / completeJobEconomics ---

func TestFinalizeJobTxRechecksAfterConcurrentTiebreakInsert(t *testing.T) {
	ctx, store, pool := openIsolatedMoneyPathStore(t)
	const sla = 0.08
	f := seedMoneyPathFixture(t, ctx, store, pool, moneyPathSeedOpts{
		TaskCount: 1, TaskStatus: "complete", ClaimWorker: true,
		SeedJob: true, SeedPlanRows: true, SLAPremium: sla,
	})
	primaryTaskID := f.TaskIDs[0]
	if _, err := pool.Exec(ctx,
		`UPDATE jobs SET status='verifying' WHERE id=$1`, f.JobID); err != nil {
		t.Fatal(err)
	}
	done, err := store.JobAllTasksDone(ctx, f.JobID)
	if err != nil || !done {
		t.Fatalf("pre-race done selection = (%t,%v), want true/nil", done, err)
	}

	blocker, err := pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer blocker.Rollback(ctx)
	var blockedStatus string
	if err := blocker.QueryRow(ctx,
		`SELECT status FROM jobs WHERE id=$1 FOR UPDATE`, f.JobID).Scan(&blockedStatus); err != nil {
		t.Fatal(err)
	}
	if blockedStatus != "verifying" {
		t.Fatalf("blocked job status=%q, want verifying", blockedStatus)
	}

	type insertResult struct {
		id  uuid.UUID
		err error
	}
	inserted := make(chan insertResult, 1)
	go func() {
		id, err := store.InsertTiebreakTask(
			ctx, f.JobID, primaryTaskID, f.OtherWorkerID, "money/input", 0,
		)
		inserted <- insertResult{id: id, err: err}
	}()
	waitForJobLockWaiters := func(want int) {
		t.Helper()
		deadline := time.Now().Add(5 * time.Second)
		for time.Now().Before(deadline) {
			var waiting int
			if err := pool.QueryRow(ctx, `
				SELECT count(*)
				  FROM pg_stat_activity
				 WHERE datname=current_database()
				   AND wait_event_type='Lock'
				   AND query LIKE '%SELECT status FROM jobs WHERE id=$1 FOR UPDATE%'`,
			).Scan(&waiting); err != nil {
				t.Fatal(err)
			}
			if waiting >= want {
				return
			}
			time.Sleep(10 * time.Millisecond)
		}
		t.Fatalf("saw fewer than %d waiter(s) on the finalization job lock", want)
	}
	waitForJobLockWaiters(1)

	finalized := make(chan error, 1)
	go func() {
		finalized <- store.FinalizeJobTx(ctx, f.JobID)
	}()
	waitForJobLockWaiters(2)
	if err := blocker.Commit(ctx); err != nil {
		t.Fatal(err)
	}

	var tiebreakID uuid.UUID
	select {
	case result := <-inserted:
		if result.err != nil || result.id == uuid.Nil {
			t.Fatalf("tiebreak insertion = (%s,%v)", result.id, result.err)
		}
		tiebreakID = result.id
	case <-time.After(10 * time.Second):
		t.Fatal("tiebreak insertion did not finish after blocker committed")
	}
	select {
	case err := <-finalized:
		if !errors.Is(err, ErrJobNotFinalizable) {
			t.Fatalf("finalize behind committed tiebreak error=%v, want ErrJobNotFinalizable", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("finalization did not finish after tiebreak committed")
	}

	var jobStatus, tiebreakStatus string
	var premiumRows, consumed, taskCount int
	if err := pool.QueryRow(ctx, `
		SELECT status,task_count,
		       (SELECT consumed_tasks FROM job_economic_reserves WHERE job_id=$1),
		       (SELECT count(*) FROM ledger_entries
		         WHERE kind='buyer_charge' AND task_id IS NULL AND payout_ref=$2)
		  FROM jobs WHERE id=$1`,
		f.JobID, slaPremiumChargeRef(f.JobID),
	).Scan(&jobStatus, &taskCount, &consumed, &premiumRows); err != nil {
		t.Fatal(err)
	}
	if err := pool.QueryRow(ctx,
		`SELECT status FROM tasks WHERE id=$1`, tiebreakID).Scan(&tiebreakStatus); err != nil {
		t.Fatal(err)
	}
	if jobStatus != "running" || tiebreakStatus != "queued" ||
		taskCount != 2 || consumed != 1 || premiumRows != 0 {
		t.Fatalf("refused finalize projection = job %q/tiebreak %q/tasks %d/reserve %d/premium %d",
			jobStatus, tiebreakStatus, taskCount, consumed, premiumRows)
	}

	if _, err := pool.Exec(ctx, `
		UPDATE tasks
		   SET status='failed',claimed_by=NULL,claimed_at=NULL,worker_id=NULL
		 WHERE id=$1`, tiebreakID); err != nil {
		t.Fatal(err)
	}
	if err := store.FinalizeJobTx(ctx, f.JobID); err != nil {
		t.Fatalf("finalize after obligation drained: %v", err)
	}
	if err := pool.QueryRow(ctx, `
		SELECT status,
		       (SELECT count(*) FROM ledger_entries
		         WHERE kind='buyer_charge' AND task_id IS NULL AND payout_ref=$2)
		  FROM jobs WHERE id=$1`,
		f.JobID, slaPremiumChargeRef(f.JobID),
	).Scan(&jobStatus, &premiumRows); err != nil {
		t.Fatal(err)
	}
	if jobStatus != "complete" || premiumRows != 1 {
		t.Fatalf("converged finalize = status %q/premium %d, want complete/1",
			jobStatus, premiumRows)
	}
}

func TestFinalizeJobTxRejectsNonCompleteTerminalStates(t *testing.T) {
	for _, status := range []string{"failed", "cancelled"} {
		t.Run(status, func(t *testing.T) {
			ctx, store, pool := openIsolatedMoneyPathStore(t)
			f := seedMoneyPathFixture(t, ctx, store, pool, moneyPathSeedOpts{
				TaskCount: 1, TaskStatus: "complete", ClaimWorker: true,
				SeedJob: true, SeedPlanRows: true, SLAPremium: 0.08,
			})
			if _, err := pool.Exec(ctx,
				`UPDATE jobs SET status=$2 WHERE id=$1`, f.JobID, status); err != nil {
				t.Fatal(err)
			}
			err := store.FinalizeJobTx(ctx, f.JobID)
			if !errors.Is(err, ErrJobNotFinalizable) {
				t.Fatalf("FinalizeJobTx(%s) error=%v, want ErrJobNotFinalizable",
					status, err)
			}
			var after string
			var premiumRows int
			if err := pool.QueryRow(ctx, `
				SELECT status,
				       (SELECT count(*) FROM ledger_entries
				         WHERE kind='buyer_charge' AND task_id IS NULL AND payout_ref=$2)
				  FROM jobs WHERE id=$1`,
				f.JobID, slaPremiumChargeRef(f.JobID),
			).Scan(&after, &premiumRows); err != nil {
				t.Fatal(err)
			}
			if after != status || premiumRows != 0 {
				t.Fatalf("rejected %s finalize changed status/premium to %q/%d",
					status, after, premiumRows)
			}
		})
	}
}

func TestFinalizeJobTxActualUSDAndSLAPremiumIdempotent(t *testing.T) {
	ctx, store, pool := openMoneyPathStore(t)
	const sla = 0.08
	f := seedMoneyPathFixture(t, ctx, store, pool, moneyPathSeedOpts{
		TaskCount: 2, TaskStatus: "complete", ClaimWorker: true,
		SeedJob: true, SeedPlanRows: true, SLAPremium: sla,
	})

	// Seed buyer_charge rows the way settlement would (one per completed task).
	// Tests insert ledger rows directly; production writers are out of scope here.
	chargePerTask := f.Plan.BuyerChargePerTaskUSD
	for _, taskID := range f.TaskIDs {
		if _, err := pool.Exec(ctx, `
			INSERT INTO ledger_entries (kind,buyer_id,task_id,amount_usd,payout_status)
			VALUES ('buyer_charge',$1,$2,$3,'pending')`,
			f.BuyerID, taskID, -chargePerTask); err != nil {
			t.Fatalf("seed buyer_charge: %v", err)
		}
	}
	// Job must be non-terminal-complete path: running/verifying/complete.
	if _, err := pool.Exec(ctx, `UPDATE jobs SET status='verifying' WHERE id=$1`, f.JobID); err != nil {
		t.Fatal(err)
	}

	if err := store.FinalizeJobTx(ctx, f.JobID); err != nil {
		t.Fatalf("FinalizeJobTx: %v", err)
	}

	var actual float64
	var status string
	if err := pool.QueryRow(ctx,
		`SELECT COALESCE(actual_usd,0)::float8, status FROM jobs WHERE id=$1`, f.JobID).
		Scan(&actual, &status); err != nil {
		t.Fatal(err)
	}
	if status != "complete" {
		t.Fatalf("job status after finalize = %q, want complete", status)
	}
	wantActual := chargePerTask*float64(len(f.TaskIDs)) + sla
	if diff := actual - wantActual; diff > 0.000001 || diff < -0.000001 {
		t.Fatalf("actual_usd=%.6f, want sum of buyer charges %.6f", actual, wantActual)
	}

	var premiumRows int
	if err := pool.QueryRow(ctx, `
		SELECT count(*) FROM ledger_entries
		 WHERE kind='buyer_charge' AND buyer_id=$1 AND task_id IS NULL AND payout_ref=$2`,
		f.BuyerID, slaPremiumChargeRef(f.JobID)).Scan(&premiumRows); err != nil {
		t.Fatal(err)
	}
	if premiumRows != 1 {
		t.Fatalf("SLA premium rows after first finalize = %d, want 1", premiumRows)
	}

	// Second finalize must not double-insert the SLA premium.
	if err := store.FinalizeJobTx(ctx, f.JobID); err != nil {
		t.Fatalf("second FinalizeJobTx: %v", err)
	}
	if err := pool.QueryRow(ctx, `
		SELECT count(*) FROM ledger_entries
		 WHERE kind='buyer_charge' AND buyer_id=$1 AND task_id IS NULL AND payout_ref=$2`,
		f.BuyerID, slaPremiumChargeRef(f.JobID)).Scan(&premiumRows); err != nil {
		t.Fatal(err)
	}
	if premiumRows != 1 {
		t.Fatalf("second finalize double-inserted SLA premium: rows=%d", premiumRows)
	}
	var actual2 float64
	if err := pool.QueryRow(ctx,
		`SELECT COALESCE(actual_usd,0)::float8 FROM jobs WHERE id=$1`, f.JobID).Scan(&actual2); err != nil {
		t.Fatal(err)
	}
	if diff := actual2 - wantActual; diff > 0.000001 || diff < -0.000001 {
		t.Fatalf("actual_usd after second finalize=%.6f, want %.6f", actual2, wantActual)
	}
}

// --- Dynamic economic obligations ---

type dynamicObligationKind string

const (
	dynamicHedge    dynamicObligationKind = "hedge"
	dynamicTiebreak dynamicObligationKind = "tiebreak"
)

type dynamicObligationSnapshot struct {
	ConsumedTasks int
	TaskCount     int
	HedgeRows     int
	TiebreakRows  int
}

type dynamicTaskProjection struct {
	TaskCount    int
	HedgeRows    int
	TiebreakRows int
}

func insertDynamicObligation(
	ctx context.Context,
	store *Store,
	kind dynamicObligationKind,
	f moneyPathFixture,
	primaryTaskID uuid.UUID,
	chunkIndex int,
) (uuid.UUID, error) {
	switch kind {
	case dynamicHedge:
		return store.InsertHedgeTask(
			ctx, f.JobID, primaryTaskID, f.OtherWorkerID, "money/input", chunkIndex,
		)
	case dynamicTiebreak:
		return store.InsertTiebreakTask(
			ctx, f.JobID, primaryTaskID, f.OtherWorkerID, "money/input", chunkIndex,
		)
	default:
		return uuid.Nil, fmt.Errorf("unknown dynamic obligation kind %q", kind)
	}
}

func readDynamicObligationSnapshot(
	t *testing.T,
	ctx context.Context,
	pool *pgxpool.Pool,
	jobID, primaryTaskID uuid.UUID,
	chunkIndex int,
) dynamicObligationSnapshot {
	t.Helper()
	var out dynamicObligationSnapshot
	if err := pool.QueryRow(ctx, `
		SELECT r.consumed_tasks,j.task_count,
		       (SELECT count(*) FROM tasks h
		         WHERE h.job_id=$1 AND h.hedged_from=$2 AND h.is_redundancy=false),
		       (SELECT count(*) FROM tasks tb
		         WHERE tb.job_id=$1 AND COALESCE(tb.chunk_index,0)=$3
		           AND tb.hedged_from IS NOT NULL AND tb.is_redundancy=true)
		  FROM job_economic_reserves r
		  JOIN jobs j ON j.id=r.job_id
		 WHERE r.job_id=$1`,
		jobID, primaryTaskID, chunkIndex,
	).Scan(&out.ConsumedTasks, &out.TaskCount, &out.HedgeRows, &out.TiebreakRows); err != nil {
		t.Fatal(err)
	}
	return out
}

func readDynamicTaskProjection(
	t *testing.T,
	ctx context.Context,
	pool *pgxpool.Pool,
	jobID, primaryTaskID uuid.UUID,
	chunkIndex int,
) dynamicTaskProjection {
	t.Helper()
	var out dynamicTaskProjection
	if err := pool.QueryRow(ctx, `
		SELECT j.task_count,
		       (SELECT count(*) FROM tasks h
		         WHERE h.job_id=$1 AND h.hedged_from=$2 AND h.is_redundancy=false),
		       (SELECT count(*) FROM tasks tb
		         WHERE tb.job_id=$1 AND COALESCE(tb.chunk_index,0)=$3
		           AND tb.hedged_from IS NOT NULL AND tb.is_redundancy=true)
		  FROM jobs j WHERE j.id=$1`,
		jobID, primaryTaskID, chunkIndex,
	).Scan(&out.TaskCount, &out.HedgeRows, &out.TiebreakRows); err != nil {
		t.Fatal(err)
	}
	return out
}

func TestDynamicTaskInsertsFenceTerminalParentsWithoutReserveMutation(t *testing.T) {
	for _, kind := range []dynamicObligationKind{dynamicHedge, dynamicTiebreak} {
		for _, terminal := range []string{"failed", "cancelled", "complete"} {
			t.Run(string(kind)+"/"+terminal, func(t *testing.T) {
				ctx, store, pool := openMoneyPathStore(t)
				f := seedMoneyPathFixture(t, ctx, store, pool, moneyPathSeedOpts{
					TaskCount: 1, TaskStatus: "running", ClaimWorker: true,
					SeedJob: true, SeedPlanRows: true,
				})
				if _, err := pool.Exec(ctx,
					`UPDATE jobs SET status=$2 WHERE id=$1`, f.JobID, terminal); err != nil {
					t.Fatal(err)
				}
				before := readDynamicObligationSnapshot(
					t, ctx, pool, f.JobID, f.TaskIDs[0], 0,
				)

				id, err := insertDynamicObligation(
					ctx, store, kind, f, f.TaskIDs[0], 0,
				)
				if id != uuid.Nil || !errors.Is(err, ErrEconomicReserveExhausted) {
					t.Fatalf("terminal insert = (%s,%v), want nil/ErrEconomicReserveExhausted",
						id, err)
				}
				after := readDynamicObligationSnapshot(
					t, ctx, pool, f.JobID, f.TaskIDs[0], 0,
				)
				if after != before {
					t.Fatalf("terminal insert changed economics: before=%+v after=%+v",
						before, after)
				}
			})
		}
	}
}

func TestDynamicTaskInsertEconomicGatesAreAtomic(t *testing.T) {
	for _, kind := range []dynamicObligationKind{dynamicHedge, dynamicTiebreak} {
		for _, gate := range []string{"reserve_exhausted", "job_max", "already_charged"} {
			t.Run(string(kind)+"/"+gate, func(t *testing.T) {
				ctx, store, pool := openMoneyPathStore(t)
				f := seedMoneyPathFixture(t, ctx, store, pool, moneyPathSeedOpts{
					TaskCount: 1, TaskStatus: "running", ClaimWorker: true,
					SeedJob: true, SeedPlanRows: true,
				})
				switch gate {
				case "reserve_exhausted":
					if _, err := pool.Exec(ctx, `
						UPDATE job_economic_reserves
						   SET consumed_tasks=reserved_tasks
						 WHERE job_id=$1`, f.JobID); err != nil {
						t.Fatal(err)
					}
				case "job_max":
					if _, err := pool.Exec(ctx,
						`UPDATE jobs SET max_usd=$2 WHERE id=$1`,
						f.JobID, f.Plan.InitialBuyerChargeUSD); err != nil {
						t.Fatal(err)
					}
				case "already_charged":
					if _, err := pool.Exec(ctx,
						`UPDATE jobs SET charge_status='charged' WHERE id=$1`,
						f.JobID); err != nil {
						t.Fatal(err)
					}
				}
				before := readDynamicObligationSnapshot(
					t, ctx, pool, f.JobID, f.TaskIDs[0], 0,
				)

				id, err := insertDynamicObligation(
					ctx, store, kind, f, f.TaskIDs[0], 0,
				)
				if id != uuid.Nil || !errors.Is(err, ErrEconomicReserveExhausted) {
					t.Fatalf("gated insert = (%s,%v), want nil/ErrEconomicReserveExhausted",
						id, err)
				}
				after := readDynamicObligationSnapshot(
					t, ctx, pool, f.JobID, f.TaskIDs[0], 0,
				)
				if after != before {
					t.Fatalf("gated insert changed economics: before=%+v after=%+v",
						before, after)
				}
			})
		}
	}
}

func TestDynamicTaskInsertMissingReservePreservesRejectAndReplay(t *testing.T) {
	for _, kind := range []dynamicObligationKind{dynamicHedge, dynamicTiebreak} {
		t.Run(string(kind)+"/reject", func(t *testing.T) {
			ctx, store, pool := openMoneyPathStore(t)
			f := seedMoneyPathFixture(t, ctx, store, pool, moneyPathSeedOpts{
				TaskCount: 1, TaskStatus: "running", ClaimWorker: true,
				SeedJob: true, SeedPlanRows: true,
			})
			before := readDynamicTaskProjection(
				t, ctx, pool, f.JobID, f.TaskIDs[0], 0,
			)
			if _, err := pool.Exec(ctx,
				`DELETE FROM job_economic_reserves WHERE job_id=$1`, f.JobID); err != nil {
				t.Fatal(err)
			}

			id, err := insertDynamicObligation(
				ctx, store, kind, f, f.TaskIDs[0], 0,
			)
			if id != uuid.Nil || !errors.Is(err, ErrEconomicReserveExhausted) {
				t.Fatalf("missing-reserve insert = (%s,%v), want nil/ErrEconomicReserveExhausted",
					id, err)
			}
			after := readDynamicTaskProjection(
				t, ctx, pool, f.JobID, f.TaskIDs[0], 0,
			)
			if after != before {
				t.Fatalf("missing-reserve reject changed tasks: before=%+v after=%+v",
					before, after)
			}
		})
		t.Run(string(kind)+"/replay", func(t *testing.T) {
			ctx, store, pool := openMoneyPathStore(t)
			f := seedMoneyPathFixture(t, ctx, store, pool, moneyPathSeedOpts{
				TaskCount: 1, TaskStatus: "running", ClaimWorker: true,
				SeedJob: true, SeedPlanRows: true,
			})
			first, err := insertDynamicObligation(
				ctx, store, kind, f, f.TaskIDs[0], 0,
			)
			if err != nil || first == uuid.Nil {
				t.Fatalf("first insert = (%s,%v)", first, err)
			}
			before := readDynamicTaskProjection(
				t, ctx, pool, f.JobID, f.TaskIDs[0], 0,
			)
			if _, err := pool.Exec(ctx,
				`DELETE FROM job_economic_reserves WHERE job_id=$1`, f.JobID); err != nil {
				t.Fatal(err)
			}

			replay, err := insertDynamicObligation(
				ctx, store, kind, f, f.TaskIDs[0], 0,
			)
			if err != nil || replay != first {
				t.Fatalf("missing-reserve replay = (%s,%v), want %s/nil",
					replay, err, first)
			}
			after := readDynamicTaskProjection(
				t, ctx, pool, f.JobID, f.TaskIDs[0], 0,
			)
			if after != before {
				t.Fatalf("missing-reserve replay changed tasks: before=%+v after=%+v",
					before, after)
			}
		})
	}
}

func TestDynamicTaskInsertReplaySurvivesTerminalParent(t *testing.T) {
	for _, kind := range []dynamicObligationKind{dynamicHedge, dynamicTiebreak} {
		for _, terminal := range []string{"failed", "cancelled", "complete"} {
			t.Run(string(kind)+"/"+terminal, func(t *testing.T) {
				ctx, store, pool := openMoneyPathStore(t)
				f := seedMoneyPathFixture(t, ctx, store, pool, moneyPathSeedOpts{
					TaskCount: 1, TaskStatus: "running", ClaimWorker: true,
					SeedJob: true, SeedPlanRows: true,
				})
				first, err := insertDynamicObligation(
					ctx, store, kind, f, f.TaskIDs[0], 0,
				)
				if err != nil || first == uuid.Nil {
					t.Fatalf("first insert = (%s,%v)", first, err)
				}
				before := readDynamicObligationSnapshot(
					t, ctx, pool, f.JobID, f.TaskIDs[0], 0,
				)
				if _, err := pool.Exec(ctx,
					`UPDATE jobs SET status=$2 WHERE id=$1`, f.JobID, terminal); err != nil {
					t.Fatal(err)
				}

				replay, err := insertDynamicObligation(
					ctx, store, kind, f, f.TaskIDs[0], 0,
				)
				if err != nil || replay != first {
					t.Fatalf("terminal replay = (%s,%v), want %s/nil", replay, err, first)
				}
				if _, err := store.DetachUnfinishedTasksForTerminalJob(ctx, f.JobID); err != nil {
					t.Fatalf("detach terminal obligations: %v", err)
				}
				detachedReplay, err := insertDynamicObligation(
					ctx, store, kind, f, f.TaskIDs[0], 0,
				)
				if err != nil || detachedReplay != first {
					t.Fatalf("detached replay = (%s,%v), want %s/nil",
						detachedReplay, err, first)
				}
				after := readDynamicObligationSnapshot(
					t, ctx, pool, f.JobID, f.TaskIDs[0], 0,
				)
				if after != before {
					t.Fatalf("terminal replay changed economics: before=%+v after=%+v",
						before, after)
				}
				var status string
				if err := pool.QueryRow(ctx,
					`SELECT status FROM tasks WHERE id=$1`, first).Scan(&status); err != nil {
					t.Fatal(err)
				}
				wantStatus := "failed"
				if terminal == "cancelled" {
					wantStatus = "cancelled"
				}
				if status != wantStatus {
					t.Fatalf("detached dynamic task status=%q, want %q", status, wantStatus)
				}
			})
		}
	}
}

func TestConcurrentDynamicTaskInsertIsIdempotent(t *testing.T) {
	for _, kind := range []dynamicObligationKind{dynamicHedge, dynamicTiebreak} {
		t.Run(string(kind), func(t *testing.T) {
			ctx, store, pool := openMoneyPathStore(t)
			f := seedMoneyPathFixture(t, ctx, store, pool, moneyPathSeedOpts{
				TaskCount: 1, TaskStatus: "running", ClaimWorker: true,
				SeedJob: true, SeedPlanRows: true,
			})
			type insertResult struct {
				id  uuid.UUID
				err error
			}
			const contenders = 8
			start := make(chan struct{})
			results := make(chan insertResult, contenders)
			for i := 0; i < contenders; i++ {
				go func() {
					<-start
					id, err := insertDynamicObligation(
						ctx, store, kind, f, f.TaskIDs[0], 0,
					)
					results <- insertResult{id: id, err: err}
				}()
			}
			close(start)

			got := make([]insertResult, contenders)
			for i := range got {
				select {
				case got[i] = <-results:
				case <-time.After(15 * time.Second):
					t.Fatal("concurrent dynamic inserts did not finish")
				}
				if got[i].err != nil || got[i].id == uuid.Nil {
					t.Fatalf("concurrent insert %d = (%s,%v)", i, got[i].id, got[i].err)
				}
			}
			for i := 1; i < len(got); i++ {
				if got[i].id != got[0].id {
					t.Fatalf("concurrent insert %d returned %s, want durable replay %s",
						i, got[i].id, got[0].id)
				}
			}
			var durable bool
			if err := pool.QueryRow(ctx, `
				SELECT EXISTS (
				  SELECT 1 FROM tasks
				   WHERE id=$1 AND job_id=$2 AND hedged_from=$3
				     AND is_redundancy=$4
				)`,
				got[0].id, f.JobID, f.TaskIDs[0], kind == dynamicTiebreak,
			).Scan(&durable); err != nil {
				t.Fatal(err)
			}
			if !durable {
				t.Fatalf("concurrent inserts returned non-durable id %s", got[0].id)
			}
			snap := readDynamicObligationSnapshot(
				t, ctx, pool, f.JobID, f.TaskIDs[0], 0,
			)
			if snap.ConsumedTasks != 1 {
				t.Fatalf("concurrent inserts consumed %d reserve tasks, want 1",
					snap.ConsumedTasks)
			}
			switch kind {
			case dynamicHedge:
				if snap.HedgeRows != 1 || snap.TiebreakRows != 0 || snap.TaskCount != 1 {
					t.Fatalf("concurrent hedge projection = %+v", snap)
				}
			case dynamicTiebreak:
				if snap.HedgeRows != 0 || snap.TiebreakRows != 1 || snap.TaskCount != 2 {
					t.Fatalf("concurrent tiebreak projection = %+v", snap)
				}
			}
		})
	}
}

func TestPlannedAndUnplannedTiebreakInsertionConvergeWithoutDeadlock(t *testing.T) {
	ctx, store, pool := openMoneyPathStore(t)
	f := seedMoneyPathFixture(t, ctx, store, pool, moneyPathSeedOpts{
		TaskCount: 1,
	})
	tasks := makeTasks(f, 1)
	job := validJobRow(t, f, tasks)
	if err := store.SubmitJobTx(ctx, job, tasks); err != nil {
		t.Fatalf("submit frozen-authority fixture: %v", err)
	}
	primaryTaskID := tasks[0].ID
	if _, err := pool.Exec(ctx,
		`UPDATE jobs SET status='running' WHERE id=$1`, f.JobID); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `
		UPDATE tasks
		   SET status='running', claimed_by=$2, claimed_at=now(), started_at=now(),
		       worker_id=$2, execution_worker_id=$2, execution_supplier_id=$3,
		       execution_hw_class='apple_silicon_max',
		       execution_engine='candle', execution_build_hash='deadbeefdeadbeef'
		 WHERE id=$1`,
		primaryTaskID, f.WorkerID, f.SupplierID); err != nil {
		t.Fatal(err)
	}
	info, err := store.CompleteTaskTx(
		ctx, primaryTaskID, f.WorkerID, commitFor(f, primaryTaskID, 0),
	)
	if err != nil {
		t.Fatalf("complete primary for planned apply: %v", err)
	}
	settled, err := store.observedOutputSettlementForTask(ctx, info)
	if err != nil {
		t.Fatalf("load planned settlement: %v", err)
	}
	var currency string
	if err := pool.QueryRow(ctx,
		`SELECT currency FROM jobs WHERE id=$1`, f.JobID).Scan(&currency); err != nil {
		t.Fatal(err)
	}
	entries := splitFrozenCharge(
		f.BuyerID, f.SupplierID, primaryTaskID, currency,
		settled.BilledCharge, settled.SupplierPayout, 0, time.Now().UTC(),
	)
	effect := VerificationEffect{
		Kind:          VerificationEffectInsertTiebreak,
		JobID:         f.JobID,
		PeerWorkerID:  f.OtherWorkerID,
		PrimaryTaskID: primaryTaskID,
		InputRef:      info.InputRef,
		ChunkIndex:    info.ChunkIndex,
	}
	effect.ID = verificationEffectPayloadID(info.TaskID, info.Attempt, 0, effect)
	effect.TaskID = effect.ID
	decision := VerificationDecision{
		Outcome: OutcomePass,
		Effects: []VerificationEffect{effect},
	}

	type applyResult struct {
		result VerificationApplyResult
		err    error
	}
	type insertResult struct {
		id  uuid.UUID
		err error
	}
	start := make(chan struct{})
	applied := make(chan applyResult, 1)
	inserted := make(chan insertResult, 1)
	go func() {
		<-start
		result, err := store.applyVerificationDecision(
			ctx, info, decision, entries, nil, nil,
		)
		applied <- applyResult{result: result, err: err}
	}()
	go func() {
		<-start
		id, err := store.InsertTiebreakTask(
			ctx, f.JobID, primaryTaskID, f.OtherWorkerID, info.InputRef, info.ChunkIndex,
		)
		inserted <- insertResult{id: id, err: err}
	}()
	close(start)

	var apply applyResult
	var insert insertResult
	select {
	case apply = <-applied:
	case <-time.After(15 * time.Second):
		t.Fatal("planned tiebreak apply did not finish")
	}
	select {
	case insert = <-inserted:
	case <-time.After(15 * time.Second):
		t.Fatal("unplanned tiebreak insert did not finish")
	}
	if apply.err != nil || !apply.result.Applied {
		t.Fatalf("planned tiebreak apply = (%+v,%v), want applied/nil",
			apply.result, apply.err)
	}
	var durationDepthBand *string
	if err := pool.QueryRow(ctx, `
		SELECT input_depth_band
		  FROM task_durations
		 WHERE task_id=$1
		 ORDER BY created_at DESC
		 LIMIT 1`,
		primaryTaskID,
	).Scan(&durationDepthBand); err != nil {
		t.Fatalf("read applied task duration depth band: %v", err)
	}
	if durationDepthBand == nil || *durationDepthBand != inputDepthBandShort {
		t.Fatalf("applied task duration depth band=%v, want short", durationDepthBand)
	}
	if insert.err != nil || insert.id == uuid.Nil {
		t.Fatalf("unplanned tiebreak insert = (%s,%v)", insert.id, insert.err)
	}
	snap := readDynamicObligationSnapshot(
		t, ctx, pool, f.JobID, primaryTaskID, info.ChunkIndex,
	)
	if snap.ConsumedTasks != 1 || snap.HedgeRows != 0 ||
		snap.TiebreakRows != 1 || snap.TaskCount != 2 {
		t.Fatalf("planned/unplanned convergence = %+v", snap)
	}
	var durableID uuid.UUID
	if err := pool.QueryRow(ctx, `
		SELECT id FROM tasks
		 WHERE job_id=$1 AND COALESCE(chunk_index,0)=$2
		   AND hedged_from IS NOT NULL AND is_redundancy=true`,
		f.JobID, info.ChunkIndex,
	).Scan(&durableID); err != nil {
		t.Fatal(err)
	}
	if insert.id != durableID {
		t.Fatalf("unplanned replay returned %s, durable tiebreak is %s",
			insert.id, durableID)
	}
	if apply.result.TiebreaksInserted != 0 &&
		(apply.result.TiebreaksInserted != 1 || durableID != effect.TaskID) {
		t.Fatalf("planned insertion result %+v conflicts with durable id %s/effect %s",
			apply.result, durableID, effect.TaskID)
	}
}

func TestConcurrentTerminalFailureAndDynamicInsertHasNoReserveLeak(t *testing.T) {
	for _, kind := range []dynamicObligationKind{dynamicHedge, dynamicTiebreak} {
		for _, failureTarget := range []struct {
			name  string
			index int
		}{
			{name: "same_anchor", index: 0},
			{name: "sibling", index: 1},
		} {
			t.Run(string(kind)+"/"+failureTarget.name, func(t *testing.T) {
				ctx, store, pool := openMoneyPathStore(t)
				f := seedMoneyPathFixture(t, ctx, store, pool, moneyPathSeedOpts{
					TaskCount: 2, TaskStatus: "running", ClaimWorker: true,
					SeedJob: true, SeedPlanRows: true,
				})
				if _, err := pool.Exec(ctx,
					`UPDATE tasks SET claimed_at=now(),started_at=now()
					  WHERE id=ANY($1::uuid[])`, f.TaskIDs); err != nil {
					t.Fatal(err)
				}
				report := FailureReport{
					Class: "bad_input", Message: "terminal race with dynamic obligation",
					Backend: "embed", Model: "all-minilm-l6-v2", DurationMS: 1,
				}
				type failResult struct {
					outcome FailOutcome
					err     error
				}
				type insertResult struct {
					id  uuid.UUID
					err error
				}
				start := make(chan struct{})
				failed := make(chan failResult, 1)
				inserted := make(chan insertResult, 1)
				go func() {
					<-start
					outcome, err := store.FailTaskTx(
						ctx, f.TaskIDs[failureTarget.index], f.WorkerID, 0, report,
					)
					failed <- failResult{outcome: outcome, err: err}
				}()
				go func() {
					<-start
					id, err := insertDynamicObligation(
						ctx, store, kind, f, f.TaskIDs[0], 0,
					)
					inserted <- insertResult{id: id, err: err}
				}()
				close(start)

				var failure failResult
				var insertion insertResult
				select {
				case failure = <-failed:
				case <-time.After(15 * time.Second):
					t.Fatal("terminal failure did not finish")
				}
				select {
				case insertion = <-inserted:
				case <-time.After(15 * time.Second):
					t.Fatal("dynamic insertion did not finish")
				}
				if failure.err != nil || failure.outcome != FailTerminal {
					t.Fatalf("terminal failure = (%q,%v), want failed/nil",
						failure.outcome, failure.err)
				}
				if insertion.err != nil &&
					!errors.Is(insertion.err, ErrEconomicReserveExhausted) {
					t.Fatalf("dynamic insert race returned unexpected error: %v", insertion.err)
				}
				if insertion.err == nil && insertion.id == uuid.Nil {
					t.Fatal("successful dynamic insert returned nil id")
				}
				if insertion.err != nil && insertion.id != uuid.Nil {
					t.Fatalf("failed dynamic insert returned non-nil id %s", insertion.id)
				}

				snap := readDynamicObligationSnapshot(
					t, ctx, pool, f.JobID, f.TaskIDs[0], 0,
				)
				dynamicRows := snap.HedgeRows + snap.TiebreakRows
				if snap.ConsumedTasks != dynamicRows {
					t.Fatalf("terminal race leaked reserve: %+v", snap)
				}
				if dynamicRows > 1 {
					t.Fatalf("terminal race inserted %d dynamic obligations", dynamicRows)
				}
				if insertion.err == nil {
					var buyerCharge, supplierPayout float64
					err := pool.QueryRow(ctx, `
						SELECT economic_buyer_charge_usd::float8,
						       economic_supplier_payout_usd::float8
						  FROM tasks
						 WHERE id=$1 AND job_id=$2 AND hedged_from=$3
						   AND is_redundancy=$4`,
						insertion.id, f.JobID, f.TaskIDs[0], kind == dynamicTiebreak,
					).Scan(&buyerCharge, &supplierPayout)
					if err != nil {
						t.Fatalf("successful insert id %s is not durable: %v", insertion.id, err)
					}
					if fmt.Sprintf("%.6f", buyerCharge) !=
						fmt.Sprintf("%.6f", f.Plan.BuyerChargePerTaskUSD) ||
						fmt.Sprintf("%.6f", supplierPayout) !=
							fmt.Sprintf("%.6f", f.Plan.SupplierPayoutPerTaskUSD) {
						t.Fatalf("dynamic economics = %.6f/%.6f, want %.6f/%.6f",
							buyerCharge, supplierPayout,
							f.Plan.BuyerChargePerTaskUSD, f.Plan.SupplierPayoutPerTaskUSD)
					}
				}
				wantTaskCount := 2
				if kind == dynamicTiebreak {
					wantTaskCount += dynamicRows
				}
				if snap.TaskCount != wantTaskCount {
					t.Fatalf("terminal race task_count=%d, want %d (snapshot %+v)",
						snap.TaskCount, wantTaskCount, snap)
				}
				var jobStatus string
				if err := pool.QueryRow(ctx,
					`SELECT status FROM jobs WHERE id=$1`, f.JobID).Scan(&jobStatus); err != nil {
					t.Fatal(err)
				}
				if jobStatus != "failed" {
					t.Fatalf("terminal race left parent %q, want failed", jobStatus)
				}
				if _, err := store.DetachUnfinishedTasksForTerminalJob(ctx, f.JobID); err != nil {
					t.Fatalf("detach terminal race residual: %v", err)
				}
			})
		}
	}
}

func TestCompleteParentTasksAreNeitherClaimedNorExplainedAsEligible(t *testing.T) {
	for _, pinned := range []bool{false, true} {
		name := "unclaimed"
		if pinned {
			name = "pinned"
		}
		t.Run(name, func(t *testing.T) {
			ctx, store, pool := openIsolatedMoneyPathStore(t)
			f := seedMoneyPathFixture(t, ctx, store, pool, moneyPathSeedOpts{
				TaskCount: 1, TaskStatus: "queued", ClaimWorker: false,
				SeedJob: true, SeedPlanRows: true,
			})
			if pinned {
				if _, err := pool.Exec(ctx, `
					UPDATE tasks SET claimed_by=$2,claimed_at=now()
					 WHERE id=$1`, f.TaskIDs[0], f.OtherWorkerID); err != nil {
					t.Fatal(err)
				}
			}
			if _, err := pool.Exec(ctx,
				`UPDATE jobs SET status='complete' WHERE id=$1`, f.JobID); err != nil {
				t.Fatal(err)
			}

			claimed, err := store.ClaimTasksTx(ctx, WorkerAuth{
				WorkerID: f.OtherWorkerID, SupplierID: f.OtherSupplierID,
			})
			if err != nil {
				t.Fatalf("claim against complete parent: %v", err)
			}
			if claimed != nil {
				t.Fatalf("claimed task %s from complete parent", claimed.TaskID)
			}
			var status string
			var claimedBy *uuid.UUID
			if err := pool.QueryRow(ctx,
				`SELECT status,claimed_by FROM tasks WHERE id=$1`, f.TaskIDs[0]).
				Scan(&status, &claimedBy); err != nil {
				t.Fatal(err)
			}
			if status != "queued" {
				t.Fatalf("claim attempt changed complete-parent task to %q", status)
			}
			if pinned && (claimedBy == nil || *claimedBy != f.OtherWorkerID) {
				t.Fatalf("claim attempt changed complete-parent pin to %v", claimedBy)
			}
			if !pinned && claimedBy != nil {
				t.Fatalf("claim attempt added complete-parent pin %v", claimedBy)
			}
			explain, err := store.SchedulerExplain(ctx, f.OtherWorkerID)
			if err != nil {
				t.Fatalf("scheduler explain: %v", err)
			}
			if explain.Eligible != 0 || explain.NoQueuedTasks != 1 {
				t.Fatalf("complete-parent explain = eligible %d/no_queue %d, want 0/1",
					explain.Eligible, explain.NoQueuedTasks)
			}
		})
	}
}

func TestClaimTaskRollsBackWhenParentTerminalizesDuringClaim(t *testing.T) {
	ctx, store, pool := openIsolatedMoneyPathStore(t)
	f := seedMoneyPathFixture(t, ctx, store, pool, moneyPathSeedOpts{
		TaskCount: 1, TaskStatus: "queued", ClaimWorker: false,
		SeedJob: true, SeedPlanRows: true,
	})

	terminalTx, err := pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer terminalTx.Rollback(ctx)
	if _, err := terminalTx.Exec(ctx,
		`UPDATE jobs SET status='complete' WHERE id=$1`, f.JobID); err != nil {
		t.Fatal(err)
	}

	type claimResult struct {
		task *ClaimedTask
		err  error
	}
	result := make(chan claimResult, 1)
	go func() {
		task, err := store.ClaimTasksTx(ctx, WorkerAuth{
			WorkerID: f.OtherWorkerID, SupplierID: f.OtherSupplierID,
		})
		result <- claimResult{task: task, err: err}
	}()

	deadline := time.Now().Add(5 * time.Second)
	waiting := false
	for time.Now().Before(deadline) {
		if err := pool.QueryRow(ctx, `
			SELECT EXISTS (
			  SELECT 1 FROM pg_stat_activity
			   WHERE datname=current_database()
			     AND wait_event_type='Lock'
			     AND query LIKE '%FROM jobs WHERE id=$1 FOR UPDATE%'
			)`).Scan(&waiting); err != nil {
			t.Fatal(err)
		}
		if waiting {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if !waiting {
		t.Fatal("claim did not reach the parent lock behind terminal transition")
	}
	if err := terminalTx.Commit(ctx); err != nil {
		t.Fatal(err)
	}

	select {
	case got := <-result:
		if got.err != nil || got.task != nil {
			t.Fatalf("claim after concurrent terminal transition = (%+v,%v), want nil/nil",
				got.task, got.err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("claim did not finish after terminal transition committed")
	}
	var taskStatus, jobStatus string
	var claimedBy, workerID *uuid.UUID
	if err := pool.QueryRow(ctx, `
		SELECT t.status,t.claimed_by,t.worker_id,j.status
		  FROM tasks t JOIN jobs j ON j.id=t.job_id
		 WHERE t.id=$1`, f.TaskIDs[0]).
		Scan(&taskStatus, &claimedBy, &workerID, &jobStatus); err != nil {
		t.Fatal(err)
	}
	if taskStatus != "queued" || claimedBy != nil || workerID != nil ||
		jobStatus != "complete" {
		t.Fatalf("terminal claim race projection = task %q claimed %v worker %v job %q",
			taskStatus, claimedBy, workerID, jobStatus)
	}
}
