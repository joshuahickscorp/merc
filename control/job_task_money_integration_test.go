package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
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
	TaskCount    int
	TaskStatus   string // queued | running | verifying | complete
	ClaimWorker  bool   // set claimed_by / execution fields for the primary worker
	SLAPremium   float64
	SeedPlanRows bool // insert job_economic_plans + reserves for non-SubmitJobTx tests
	SeedJob      bool
}

func openMoneyPathStore(t *testing.T) (context.Context, *Store, *pgxpool.Pool) {
	t.Helper()
	// Disabling canary requires a recorded decision reference, because the same
	// switch also opens self-serve signup; one decision gates both.
	t.Setenv("MERC_CANARY_MODE", "false")
	t.Setenv("MERC_CANARY_DISABLE_DECISION_REF", "TEST-money-path")
	return openAdminMutationTestStore(t)
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
		1,
		1,
		0,
		0,
		QuoteTime{P50Secs: 60, P90Secs: 120, WorstCaseSecs: 240},
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
		decision, 1, 128, microsToUSD(money.BuyerDebitMicros), &originPlan,
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
		                  firm_quote,sla_premium_usd)
		VALUES ($1,$2,'running','embed','all-minilm-l6-v2','money/input',$3,0,
		        10.0,0,'batch',$4,0,false,$5)`,
		f.JobID, f.BuyerID, opts.TaskCount, f.Plan.InitialBuyerChargeUSD, opts.SLAPremium); err != nil {
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
		1,
		len(tasks),
		0,
		0,
		QuoteTime{P50Secs: 60, P90Secs: 120, WorstCaseSecs: 240},
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
			WorstCaseSecs: job.ComputePlan.ETAWorstCaseSecs,
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
