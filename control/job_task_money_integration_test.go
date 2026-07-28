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
	quoteID := uuid.New()
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
	receipt := assembleClearingReceipt(f.JobID, "complete", stored, nil, Verification{}, nil, nil)
	if receipt.Workload == nil || receipt.Workload.BindingSHA256 != decision.BindingSHA256 {
		t.Fatal("exact-reuse receipt omitted the frozen workload decision")
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
	return &jobRow{
		ID:               f.JobID,
		BuyerID:          f.BuyerID,
		JobType:          "embed",
		ModelRef:         "all-minilm-l6-v2",
		InputRef:         "money/input-" + f.JobID.String(),
		OutputRef:        "money/output-" + f.JobID.String(),
		Tier:             "batch",
		EstimatedUSD:     f.Plan.InitialBuyerChargeUSD,
		TaskCount:        len(tasks),
		MinMemoryGB:      0,
		MaxDurationSecs:  3600,
		SplitSize:        1,
		OfferedRateUsdHr: 1.0,
		ETASecs:          60,
		SLAPremiumUSD:    f.Plan.Input.SLAPremiumUSD,
		EconomicPlan:     f.Plan,
		WorkloadDecision: workload,
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
	if n := countBuyerLedger(t, ctx, pool, f.BuyerID); n != 0 {
		t.Fatalf("SubmitJobTx minted %d ledger rows; submit must mint no money", n)
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
