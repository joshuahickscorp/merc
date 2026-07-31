package main

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
)

// The cost arithmetic, without a database.
//
// These are the cases where getting it wrong promotes the wrong cell, so they are
// asserted on numbers rather than on "it returned something".
func TestVerifiedOutcomeCostChargesForRetriesAndRejections(t *testing.T) {
	for _, tc := range []struct {
		name string
		cost MeasuredCellCost
		want float64
		ok   bool
	}{{
		name: "clean cell costs what it costs",
		cost: MeasuredCellCost{SupplierUSDPerUnit: 0.001, VerificationSamples: 40},
		want: 0.001, ok: true,
	}, {
		// One retry in every two tasks is half again the work, and the buyer's
		// verified unit has to carry it.
		name: "retries add attempts",
		cost: MeasuredCellCost{SupplierUSDPerUnit: 0.001, RetryRate: 0.5, VerificationSamples: 40},
		want: 0.0015, ok: true,
	}, {
		// The case the whole file exists for: a cell 20% cheaper per attempt that
		// fails a quarter of its verifications is NOT cheaper.
		name: "a rejected result buys nothing",
		cost: MeasuredCellCost{
			SupplierUSDPerUnit: 0.0008, VerificationSamples: 40, VerificationFails: 10,
		},
		want: 0.0008 / 0.75, ok: true,
	}, {
		name: "every result rejected has no verified-outcome cost at all",
		cost: MeasuredCellCost{
			SupplierUSDPerUnit: 0.0008, VerificationSamples: 4, VerificationFails: 4,
		},
		ok: false,
	}, {
		name: "a cell with no measured supplier cost is not free",
		cost: MeasuredCellCost{SupplierUSDPerUnit: 0, VerificationSamples: 40},
		ok:   false,
	}, {
		// The hole the completed-task sample cannot see: a cell that failed a
		// quarter of what it claimed. Those tasks delivered nothing, so the units
		// that DID arrive have to carry them.
		name: "a task that crashed buys nothing either",
		cost: MeasuredCellCost{
			SupplierUSDPerUnit: 0.001, VerificationSamples: 30,
			TerminalAttempts: 40, TerminalFails: 10,
		},
		want: 0.001 / 0.75, ok: true,
	}, {
		name: "a cell that fails everything has no verified-outcome cost",
		cost: MeasuredCellCost{
			SupplierUSDPerUnit: 0.001, VerificationSamples: 1,
			TerminalAttempts: 8, TerminalFails: 8,
		},
		ok: false,
	}} {
		t.Run(tc.name, func(t *testing.T) {
			got, ok := tc.cost.ExpectedVerifiedOutcomeUSDPerUnit()
			if ok != tc.ok {
				t.Fatalf("ok = %v, want %v", ok, tc.ok)
			}
			if !tc.ok {
				return
			}
			if diff := got - tc.want; diff > 1e-12 || diff < -1e-12 {
				t.Fatalf("cost = %.12f, want %.12f", got, tc.want)
			}
		})
	}
}

// A decision where only the winner has data must not report zero regret. That
// number would be indistinguishable from "the winner was measured to be best",
// and it is really "nothing else was tried".
func TestRegretIsNotScoredWhenOnlyTheWinnerIsMeasured(t *testing.T) {
	measured := MeasuredCellCost{
		SupplierUSDPerUnit: 0.001, VerificationSamples: 40, Measured: true,
	}
	cheaper := MeasuredCellCost{
		SupplierUSDPerUnit: 0.0005, VerificationSamples: 40, Measured: true,
	}
	underSampled := MeasuredCellCost{
		SupplierUSDPerUnit: 0.0001, VerificationSamples: 3, Samples: 3,
	}
	decision := shadowDecisionRow{
		RoutedCell: "a", ShadowCell: "a", Considered: []string{"a", "b"},
	}

	if _, _, ok := scoreDecisionRegret(decision, map[string]MeasuredCellCost{
		"a": measured,
	}); ok {
		t.Fatal("scored a decision whose only candidate was the routed cell")
	}
	if _, _, ok := scoreDecisionRegret(decision, map[string]MeasuredCellCost{
		"a": measured, "b": underSampled,
	}); ok {
		t.Fatal("scored against an under-sampled cell")
	}
	regret, cheapest, ok := scoreDecisionRegret(decision, map[string]MeasuredCellCost{
		"a": measured, "b": cheaper,
	})
	if !ok {
		t.Fatal("two measured candidates should score")
	}
	if cheapest != "b" {
		t.Fatalf("cheapest = %q, want b", cheapest)
	}
	if diff := regret - 0.0005; diff > 1e-12 || diff < -1e-12 {
		t.Fatalf("regret = %.12f, want 0.0005", regret)
	}
}

// Cost may never be compared across hardware classes: the difference would be the
// machine, not the runtime.
func TestComparableHardwareRefusesToMixMachines(t *testing.T) {
	measured := func(usd float64) MeasuredCellCost {
		return MeasuredCellCost{
			SupplierUSDPerUnit: usd, VerificationSamples: 40,
			Samples: minCellCostSamples, Measured: true,
		}
	}
	byHW := map[string]map[string]MeasuredCellCost{
		// One cell each: no comparison is possible on either class alone.
		"apple_silicon_max":   {"a": measured(0.001)},
		"apple_silicon_ultra": {"b": measured(0.0005)},
	}
	if hw := comparableHardwareFor(byHW, []string{"a", "b"}); hw != "" {
		t.Fatalf("compared across hardware classes: chose %q", hw)
	}

	byHW["apple_silicon_ultra"]["a"] = measured(0.002)
	if hw := comparableHardwareFor(byHW, []string{"a", "b"}); hw != "apple_silicon_ultra" {
		t.Fatalf("hw = %q, want apple_silicon_ultra where both cells are measured", hw)
	}
	// On that class the cheaper cell wins, and it is the one measured THERE — not
	// cell "a" at its cheaper price on the other machine.
	ranked := rankCellsByMeasuredCost(byHW["apple_silicon_ultra"], []string{"a", "b"})
	if len(ranked) != 2 || ranked[0] != "b" {
		t.Fatalf("ranked = %v, want b first", ranked)
	}
}

// costWorker is a supplier/worker pair on one hardware class and engine.
//
// Separate from the money-path fixture's worker because that one is always
// apple_silicon_max running candle, and the whole question here is whether cost
// is kept separate per hardware class.
type costWorker struct {
	workerID   uuid.UUID
	supplierID uuid.UUID
	hwClass    string
	engine     string
}

func seedCostWorker(
	t *testing.T, ctx context.Context, pool *pgxpool.Pool, hwClass, engine, runtimeID string,
) costWorker {
	t.Helper()
	w := costWorker{workerID: uuid.New(), supplierID: uuid.New(), hwClass: hwClass, engine: engine}
	if _, err := pool.Exec(ctx, `
		INSERT INTO suppliers (id,email,status,reputation,completed_tasks)
		VALUES ($1,$2,'active',0.95,100)`,
		w.supplierID, "cost-"+uuid.NewString()+"@example.test"); err != nil {
		t.Fatalf("insert cost supplier: %v", err)
	}
	var revision, digest string
	if err := pool.QueryRow(ctx, `
		SELECT revision, profile_digest FROM runtime_profiles
		 WHERE runtime_profile_id=$1 AND is_current`, runtimeID).Scan(&revision, &digest); err != nil {
		t.Fatalf("resolve current %s profile: %v", runtimeID, err)
	}
	if _, err := pool.Exec(ctx, `
		INSERT INTO workers (id,supplier_id,hw_class,memory_gb,effective_memory_gb,
		                     last_seen_at,throttled,min_payout_usd_hr,engine,build_hash,
		                     runtime_profile_id,runtime_profile_revision,runtime_profile_digest)
		VALUES ($1,$2,$3,64,64,now(),false,0.10,$4,'deadbeefdeadbeef',$5,$6,$7)`,
		w.workerID, w.supplierID, hwClass, engine, runtimeID, revision, digest); err != nil {
		t.Fatalf("insert cost worker: %v", err)
	}
	return w
}

// seedCompletedCellTasks writes n completed primary tasks on one cell, carrying
// the execution identity, units, duration, frozen money and verification outcome
// a real commit leaves behind.
//
// INSERTed rather than transitioned: `tasks` protects execution identity, frozen
// economics and expected_output_records with BEFORE UPDATE triggers, so a test
// that walked a row through the lifecycle would be re-testing the money path
// instead of the read under test here. Every CHECK constraint and foreign key
// still applies to the insert, so the rows cannot be nonsense — the execution
// identity has to match a real worker's hardware, engine and build.
func seedCompletedCellTasks(
	t *testing.T, ctx context.Context, store *Store, pool *pgxpool.Pool,
	w costWorker, cellID, runtimeID string, n int, unitsPerTask int64,
	durationMs int64, supplierUSDPerTask, buyerUSDPerTask float64,
	verificationFails int,
) uuid.UUID {
	t.Helper()
	f := seedMoneyPathFixture(t, ctx, store, pool, moneyPathSeedOpts{TaskCount: 1})
	tasks := makeTasks(f, 1)
	f.TaskIDs = []uuid.UUID{tasks[0].ID}
	job := validJobRowDirected(t, f, tasks, cellID)
	if err := store.SubmitJobTx(ctx, job, tasks); err != nil {
		t.Fatalf("submit %s job: %v", cellID, err)
	}
	for i := 0; i < n; i++ {
		outcome := "pass"
		if i < verificationFails {
			outcome = "fail"
		}
		id := uuid.New()
		if _, err := pool.Exec(ctx, `
			INSERT INTO tasks
			  (id, job_id, input_ref, input_depth_band, result_key, chunk_index, status,
			   started_at, completed_at, worker_id, claimed_by,
			   execution_worker_id, execution_supplier_id, execution_hw_class,
			   execution_engine, execution_build_hash,
			   runtime_cell_id, runtime_id, runtime_matrix_sha256, model_kind,
			   expected_output_records, reported_duration_ms,
			   economic_buyer_charge_usd, economic_supplier_payout_usd,
			   verification_outcome, verified_at)
			VALUES ($1,$2,'money/input/chunk-0','short',$3,$4,'complete',
			        now(),now(),$5,$5,$5,$6,$7,$8,'deadbeefdeadbeef',
			        $9,$10,$11,'hf',$12,$13,$14,$15,$16,now())`,
			id, f.JobID, taskAttemptResultKey(f.JobID, id, 0), 1000+i,
			w.workerID, w.supplierID, w.hwClass, w.engine,
			cellID, runtimeID, generatedRuntimeMatrixSHA256,
			unitsPerTask, durationMs, buyerUSDPerTask, supplierUSDPerTask, outcome,
		); err != nil {
			t.Fatalf("insert completed task on %s: %v", cellID, err)
		}
	}
	return f.JobID
}

// The measurement is read out of the money path, per cell and per hardware class.
func TestMeasuredCellCostsReadTheMoneyPathPerCell(t *testing.T) {
	ctx, store, pool := openIsolatedMoneyPathStore(t)
	const hw = "apple_silicon_ultra"
	candleWorker := seedCostWorker(t, ctx, pool, hw, "candle", "candle_metal")
	llamaWorker := seedCostWorker(t, ctx, pool, hw, "llama_cpp", "llama_cpp_metal")
	otherMachine := seedCostWorker(t, ctx, pool, "apple_silicon_max", "llama_cpp", "llama_cpp_metal")

	seedCompletedCellTasks(t, ctx, store, pool, candleWorker,
		candleEmbedCell, "candle_metal", minCellCostSamples,
		100, 500, 0.000100, 0.000200, 0)
	// The challenger is 20% cheaper per attempt and fails one verification in
	// twenty, so its verified-outcome cost is 0.00008/0.95 — MORE than a naive
	// per-attempt comparison would report.
	seedCompletedCellTasks(t, ctx, store, pool, llamaWorker,
		llamaEmbedCell, "llama_cpp_metal", minCellCostSamples,
		100, 250, 0.000080, 0.000200, 1)
	// A handful of samples on a different machine must not join the comparison.
	seedCompletedCellTasks(t, ctx, store, pool, otherMachine,
		llamaEmbedCell, "llama_cpp_metal", 3,
		100, 100, 0.000010, 0.000200, 0)

	byHW, err := store.MeasuredCellCostsByHardware(ctx, "embed", "all-minilm-l6-v2")
	if err != nil {
		t.Fatalf("measured cell costs: %v", err)
	}
	ultra := byHW[hw]
	if len(ultra) != 2 {
		t.Fatalf("cells measured on %s = %d, want 2 (%v)", hw, len(ultra), ultra)
	}
	candle, llama := ultra[candleEmbedCell], ultra[llamaEmbedCell]
	if !candle.Measured || !llama.Measured {
		t.Fatalf("both cells should be measured at %d samples: candle=%v llama=%v",
			minCellCostSamples, candle.Measured, llama.Measured)
	}
	if candle.Samples != minCellCostSamples || llama.Samples != minCellCostSamples {
		t.Fatalf("samples candle=%d llama=%d", candle.Samples, llama.Samples)
	}
	if llama.VerificationFails != 1 {
		t.Fatalf("llama verification fails = %d, want 1", llama.VerificationFails)
	}
	if candle.MedianMsPerUnit != 5 || llama.MedianMsPerUnit != 2.5 {
		t.Fatalf("median ms per unit candle=%v llama=%v, want 5 and 2.5",
			candle.MedianMsPerUnit, llama.MedianMsPerUnit)
	}
	// Cost per unit: 0.0001 over 100 units, and 0.00008 over 100 units.
	if diff := candle.SupplierUSDPerUnit - 0.000001; diff > 1e-15 || diff < -1e-15 {
		t.Fatalf("candle supplier usd per unit = %.15f", candle.SupplierUSDPerUnit)
	}
	candleCost, ok := candle.ExpectedVerifiedOutcomeUSDPerUnit()
	if !ok {
		t.Fatal("candle has no verified-outcome cost")
	}
	llamaCost, ok := llama.ExpectedVerifiedOutcomeUSDPerUnit()
	if !ok {
		t.Fatal("llama has no verified-outcome cost")
	}
	if llamaCost <= candle.SupplierUSDPerUnit*0.8 {
		t.Fatalf("verification failure was not charged: llama=%.15f candle=%.15f",
			llamaCost, candleCost)
	}
	if llamaCost >= candleCost {
		t.Fatalf("llama should still be cheaper after the failure: llama=%.15f candle=%.15f",
			llamaCost, candleCost)
	}
	// The three-sample rows on the other machine exist but are not measured.
	if max := byHW["apple_silicon_max"][llamaEmbedCell]; max.Measured {
		t.Fatalf("3 samples reported as measured: %+v", max)
	}
	// And no comparison is possible on that machine, because only one cell ran there.
	if hwChoice := comparableHardwareFor(byHW, []string{candleEmbedCell, llamaEmbedCell}); hwChoice != hw {
		t.Fatalf("comparable hardware = %q, want %q", hwChoice, hw)
	}
}

// The promotion gate refuses on each rule independently, and every refusal names
// what would change it.
func TestCellPromotionGateRefusesUnprovenChallengers(t *testing.T) {
	ctx, store, pool := openIsolatedMoneyPathStore(t)
	const hw = "apple_silicon_ultra"
	candleWorker := seedCostWorker(t, ctx, pool, hw, "candle", "candle_metal")
	llamaWorker := seedCostWorker(t, ctx, pool, hw, "llama_cpp", "llama_cpp_metal")
	scope := CellPromotionScope{
		JobType: "embed", ModelRef: "all-minilm-l6-v2", HWClass: hw,
		LatencyClass: "BATCH", RuntimeID: "llama_cpp_metal", CellID: llamaEmbedCell,
		QualityTier: "OUTCOME_EQUIVALENT", Verification: "cosine_similarity",
	}

	// Nothing has run at all.
	evidence, err := store.EvaluateCellPromotion(ctx, scope, candleEmbedCell, time.Now())
	if err != nil {
		t.Fatalf("evaluate with no evidence: %v", err)
	}
	if evidence.Passed() {
		t.Fatal("gate passed with no measurement whatsoever")
	}
	if !containsSubstring(evidence.Refusals, "no measured verified-outcome cost") {
		t.Fatalf("refusals should name the missing measurement: %v", evidence.Refusals)
	}
	if evidence.RollbackTargetRevision != currentActivation().PolicyRevision {
		t.Fatalf("rollback target = %d, want the current policy revision %d",
			evidence.RollbackTargetRevision, currentActivation().PolicyRevision)
	}

	// Both cells measured, but the challenger is only 5% cheaper.
	seedCompletedCellTasks(t, ctx, store, pool, candleWorker,
		candleEmbedCell, "candle_metal", minCellCostSamples,
		100, 500, 0.000100, 0.000200, 0)
	seedCompletedCellTasks(t, ctx, store, pool, llamaWorker,
		llamaEmbedCell, "llama_cpp_metal", minCellCostSamples,
		100, 400, 0.000095, 0.000200, 0)
	evidence, err = store.EvaluateCellPromotion(ctx, scope, candleEmbedCell, time.Now())
	if err != nil {
		t.Fatalf("evaluate thin margin: %v", err)
	}
	if evidence.Passed() {
		t.Fatalf("gate passed a 5%% saving: %+v", evidence)
	}
	if !containsSubstring(evidence.Refusals, "below the required 10% margin") {
		t.Fatalf("refusals should name the margin: %v", evidence.Refusals)
	}

	// A receipt reference is derivable either way, and it changes when the
	// evidence changes — a refusal and a pass cannot share an identity.
	firstRef, err := evidence.ReceiptRef()
	if err != nil {
		t.Fatalf("receipt ref: %v", err)
	}
	if firstRef == "" {
		t.Fatal("empty receipt ref")
	}
	evidence.SavingFraction = 0.99
	secondRef, err := evidence.ReceiptRef()
	if err != nil {
		t.Fatalf("receipt ref after mutation: %v", err)
	}
	if firstRef == secondRef {
		t.Fatal("receipt ref did not change when the evidence did")
	}
}

// A challenger that fails verification cannot be promoted however cheap it is.
func TestCellPromotionGateRefusesACheaperCellThatFailsVerification(t *testing.T) {
	ctx, store, pool := openIsolatedMoneyPathStore(t)
	const hw = "apple_silicon_ultra"
	candleWorker := seedCostWorker(t, ctx, pool, hw, "candle", "candle_metal")
	llamaWorker := seedCostWorker(t, ctx, pool, hw, "llama_cpp", "llama_cpp_metal")
	seedCompletedCellTasks(t, ctx, store, pool, candleWorker,
		candleEmbedCell, "candle_metal", minCellCostSamples,
		100, 500, 0.000100, 0.000200, 0)
	// Half the price per attempt, one verification failure.
	seedCompletedCellTasks(t, ctx, store, pool, llamaWorker,
		llamaEmbedCell, "llama_cpp_metal", minCellCostSamples,
		100, 200, 0.000050, 0.000200, 1)

	evidence, err := store.EvaluateCellPromotion(ctx, CellPromotionScope{
		JobType: "embed", ModelRef: "all-minilm-l6-v2", HWClass: hw,
		LatencyClass: "BATCH", RuntimeID: "llama_cpp_metal", CellID: llamaEmbedCell,
		QualityTier: "OUTCOME_EQUIVALENT", Verification: "cosine_similarity",
	}, candleEmbedCell, time.Now())
	if err != nil {
		t.Fatalf("evaluate: %v", err)
	}
	if evidence.Passed() {
		t.Fatalf("gate promoted a cell that failed verification: %+v", evidence)
	}
	if !containsSubstring(evidence.Refusals, "failed verification") {
		t.Fatalf("refusals should name the verification failure: %v", evidence.Refusals)
	}
	if evidence.SavingFraction <= 0 {
		t.Fatalf("the saving is real and should be reported even when refused: %v",
			evidence.SavingFraction)
	}
}

// seedFailedCellTasks writes n primary tasks that reached 'failed' ON a cell:
// execution identity present, no units, no duration, no verification outcome.
func seedFailedCellTasks(
	t *testing.T, ctx context.Context, store *Store, pool *pgxpool.Pool,
	w costWorker, cellID, runtimeID string, n int,
) {
	t.Helper()
	f := seedMoneyPathFixture(t, ctx, store, pool, moneyPathSeedOpts{TaskCount: 1})
	tasks := makeTasks(f, 1)
	f.TaskIDs = []uuid.UUID{tasks[0].ID}
	job := validJobRowDirected(t, f, tasks, cellID)
	if err := store.SubmitJobTx(ctx, job, tasks); err != nil {
		t.Fatalf("submit %s job: %v", cellID, err)
	}
	for i := 0; i < n; i++ {
		id := uuid.New()
		if _, err := pool.Exec(ctx, `
			INSERT INTO tasks
			  (id, job_id, input_ref, result_key, chunk_index, status,
			   started_at, worker_id, claimed_by,
			   execution_worker_id, execution_supplier_id, execution_hw_class,
			   execution_engine, execution_build_hash,
			   runtime_cell_id, runtime_id, runtime_matrix_sha256, model_kind)
			VALUES ($1,$2,'money/input/chunk-0',$3,$4,'failed',
			        now(),$5,$5,$5,$6,$7,$8,'deadbeefdeadbeef',$9,$10,$11,'hf')`,
			id, f.JobID, taskAttemptResultKey(f.JobID, id, 0), 5000+i,
			w.workerID, w.supplierID, w.hwClass, w.engine,
			cellID, runtimeID, generatedRuntimeMatrixSHA256,
		); err != nil {
			t.Fatalf("insert failed task on %s: %v", cellID, err)
		}
	}
}

// A cell that crashes on work it claimed is not cheap, and the completed-task
// sample cannot see that on its own.
func TestOutrightFailuresRaiseMeasuredCostAndBlockPromotion(t *testing.T) {
	ctx, store, pool := openIsolatedMoneyPathStore(t)
	const hw = "apple_silicon_ultra"
	candleWorker := seedCostWorker(t, ctx, pool, hw, "candle", "candle_metal")
	llamaWorker := seedCostWorker(t, ctx, pool, hw, "llama_cpp", "llama_cpp_metal")

	seedCompletedCellTasks(t, ctx, store, pool, candleWorker,
		candleEmbedCell, "candle_metal", minCellCostSamples,
		100, 500, 0.000100, 0.000200, 0)
	// Half the price on everything it finished, and it failed a quarter of what
	// it claimed.
	seedCompletedCellTasks(t, ctx, store, pool, llamaWorker,
		llamaEmbedCell, "llama_cpp_metal", minCellCostSamples,
		100, 500, 0.000050, 0.000200, 0)
	seedFailedCellTasks(t, ctx, store, pool, llamaWorker,
		llamaEmbedCell, "llama_cpp_metal", minCellCostSamples/2)

	byHW, err := store.MeasuredCellCostsByHardware(ctx, "embed", "all-minilm-l6-v2")
	if err != nil {
		t.Fatal(err)
	}
	llama := byHW[hw][llamaEmbedCell]
	if llama.TerminalFails != minCellCostSamples/2 {
		t.Fatalf("terminal fails = %d, want %d", llama.TerminalFails, minCellCostSamples/2)
	}
	if llama.TerminalAttempts != minCellCostSamples+minCellCostSamples/2 {
		t.Fatalf("terminal attempts = %d, want %d",
			llama.TerminalAttempts, minCellCostSamples+minCellCostSamples/2)
	}
	withFailures, ok := llama.ExpectedVerifiedOutcomeUSDPerUnit()
	if !ok {
		t.Fatal("cell with a two-thirds success rate should still have a cost")
	}
	clean := llama
	clean.TerminalAttempts, clean.TerminalFails = 0, 0
	ignoringFailures, _ := clean.ExpectedVerifiedOutcomeUSDPerUnit()
	if withFailures <= ignoringFailures {
		t.Fatalf("failures did not raise the cost: %.12f vs %.12f",
			withFailures, ignoringFailures)
	}

	evidence, err := store.EvaluateCellPromotion(ctx, CellPromotionScope{
		JobType: "embed", ModelRef: "all-minilm-l6-v2", HWClass: hw,
		LatencyClass: "standard_batch", RuntimeID: "llama_cpp_metal", CellID: llamaEmbedCell,
		QualityTier: "OUTCOME_EQUIVALENT", Verification: "cosine_similarity",
	}, candleEmbedCell, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if evidence.Passed() {
		t.Fatalf("gate promoted a cell that failed a third of its attempts: %+v", evidence)
	}
	if !containsSubstring(evidence.Refusals, "failed outright") {
		t.Fatalf("refusals should name the outright failures: %v", evidence.Refusals)
	}
}

// A cheaper cell that is slower per unit may be promoted for batch work, whose
// deadline absorbs it, and never where latency is the product.
func TestCellPromotionWeighsLatencyOnlyWhereItIsTheProduct(t *testing.T) {
	ctx, store, pool := openIsolatedMoneyPathStore(t)
	const hw = "apple_silicon_ultra"
	candleWorker := seedCostWorker(t, ctx, pool, hw, "candle", "candle_metal")
	llamaWorker := seedCostWorker(t, ctx, pool, hw, "llama_cpp", "llama_cpp_metal")

	// The incumbent is fast and dear; the challenger is half the price and four
	// times slower per unit.
	seedCompletedCellTasks(t, ctx, store, pool, candleWorker,
		candleEmbedCell, "candle_metal", minCellCostSamples,
		100, 200, 0.000100, 0.000200, 0)
	seedCompletedCellTasks(t, ctx, store, pool, llamaWorker,
		llamaEmbedCell, "llama_cpp_metal", minCellCostSamples,
		100, 800, 0.000050, 0.000200, 0)

	scope := CellPromotionScope{
		JobType: "embed", ModelRef: "all-minilm-l6-v2", HWClass: hw,
		RuntimeID: "llama_cpp_metal", CellID: llamaEmbedCell,
		QualityTier: "OUTCOME_EQUIVALENT", Verification: "cosine_similarity",
	}

	scope.LatencyClass = "standard_batch"
	batch, err := store.EvaluateCellPromotion(ctx, scope, candleEmbedCell, time.Now())
	if err != nil {
		t.Fatalf("evaluate batch scope: %v", err)
	}
	if containsSubstring(batch.Refusals, "slower per unit") {
		t.Fatalf("batch work refused a cheaper-but-slower cell: %v", batch.Refusals)
	}
	if batch.LatencyRatio < 3.9 || batch.LatencyRatio > 4.1 {
		t.Fatalf("latency ratio = %v, want ~4 reported even when permitted", batch.LatencyRatio)
	}

	scope.LatencyClass = string(TrafficInteractive)
	interactive, err := store.EvaluateCellPromotion(ctx, scope, candleEmbedCell, time.Now())
	if err != nil {
		t.Fatalf("evaluate interactive scope: %v", err)
	}
	if !containsSubstring(interactive.Refusals, "slower per unit") {
		t.Fatalf("an interactive scope accepted a 4x slower cell: %v", interactive.Refusals)
	}
}

func containsSubstring(haystack []string, needle string) bool {
	for _, s := range haystack {
		if strings.Contains(s, needle) {
			return true
		}
	}
	return false
}
