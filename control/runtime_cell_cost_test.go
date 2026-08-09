package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"math"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
)

func TestAdminSelectorLiabilityRegretRequiresCompleteScope(t *testing.T) {
	server := &Server{}
	req := httptest.NewRequest(http.MethodGet,
		"/admin/runtime/selector/regret?job_type=embed&model_ref=model-a", nil)
	rec := httptest.NewRecorder()
	server.handleAdminSelectorLiabilityRegret(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusBadRequest)
	}
	if !strings.Contains(rec.Body.String(), "hw_class") {
		t.Fatalf("error body does not name the missing scope dimension: %s", rec.Body.String())
	}
}

func TestAdminSelectorPromotionRequiresCompleteScope(t *testing.T) {
	server := &Server{}
	req := httptest.NewRequest(http.MethodGet,
		"/admin/runtime/selector/promotion?job_type=embed&model_ref=model-a&incumbent_cell=old",
		nil)
	rec := httptest.NewRecorder()
	server.handleAdminSelectorPromotion(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusBadRequest)
	}
	for _, field := range []string{"model_revision", "verification_contract", "hardware_identity", "runtime_id", "cell_id"} {
		if !strings.Contains(rec.Body.String(), field) {
			t.Fatalf("error body does not name missing scope field %q: %s", field, rec.Body.String())
		}
	}
}

func TestCellPromotionEvidenceIsAppendOnlyAndIdempotent(t *testing.T) {
	ctx, store, pool := openIsolatedMoneyPathStore(t)
	evidence := CellPromotionEvidence{
		GateVersion: promotionGateVersion,
		EvaluatedAt: time.Now().UTC().Truncate(time.Microsecond),
		Scope: CellPromotionScope{
			JobType:          "embed",
			ModelRef:         "all-minilm-l6-v2",
			ModelRevision:    "model-revision-1",
			QualityTier:      "OUTCOME_EQUIVALENT",
			Verification:     "cosine_similarity",
			HWClass:          "apple_silicon_ultra",
			HardwareIdentity: "Apple M3 Ultra",
			LatencyClass:     "standard_batch",
			RuntimeID:        "llama_cpp_metal",
			CellID:           "llama_cpp_embed",
		},
		IncumbentCell:                 "candle_embed",
		PolicyRevision:                1,
		RollbackTargetRevision:        1,
		RuntimeMatrixSHA256:           generatedRuntimeMatrixSHA256,
		Refusals:                      []string{"no measured supplier-liability proxy"},
		UnknownPlatformCostComponents: []string{"startup"},
	}
	inserted, err := store.RecordCellPromotionEvaluation(ctx, evidence)
	mustf(t, err, "record promotion evidence: %v")
	if !inserted {
		t.Fatal("first promotion evidence write was not inserted")
	}
	inserted, err = store.RecordCellPromotionEvaluation(ctx, evidence)
	mustf(t, err, "record duplicate promotion evidence: %v")
	if inserted {
		t.Fatal("duplicate promotion evidence was inserted twice")
	}

	digest, err := evidence.Digest()
	mustf(t, err, "digest: %v")
	var count int
	if err := pool.QueryRow(ctx,
		`SELECT COUNT(*) FROM runtime_cell_promotion_evaluations WHERE evidence_sha256=$1`, digest).Scan(&count); err != nil {
		t.Fatalf("count promotion evidence: %v", err)
	}
	if count != 1 {
		t.Fatalf("promotion evidence rows = %d, want 1", count)
	}
	if _, err := pool.Exec(ctx,
		`DELETE FROM runtime_cell_promotion_evaluations WHERE evidence_sha256=$1`, digest); err == nil {
		t.Fatal("append-only promotion evidence was deleted")
	}
}

// Supplier liability is settlement money, not a reliability-weighted estimate.
// These cases pin the exact accepted payout even when separate reliability
// evidence records retries, verification rejection, or outright failure.
func TestSupplierLiabilityProxyReturnsAcceptedPayoutWithoutFailureInflation(t *testing.T) {
	for _, tc := range []struct {
		name string
		cost MeasuredSupplierLiabilityProxy
		want float64
		ok   bool
	}{{
		name: "clean cell costs what it costs",
		cost: MeasuredSupplierLiabilityProxy{SupplierUSDPerUnit: 0.001, VerificationSamples: 40},
		want: 0.001, ok: true,
	}, {
		name: "unpaid retries do not add supplier liability",
		cost: MeasuredSupplierLiabilityProxy{SupplierUSDPerUnit: 0.001, RetryRate: 0.5, VerificationSamples: 40},
		want: 0.001, ok: true,
	}, {
		name: "rejected results are not supplier payments",
		cost: MeasuredSupplierLiabilityProxy{
			SupplierUSDPerUnit: 0.0008, VerificationSamples: 40, VerificationFails: 10,
		},
		want: 0.0008, ok: true,
	}, {
		name: "even all rejected leaves the accepted payout unchanged",
		cost: MeasuredSupplierLiabilityProxy{
			SupplierUSDPerUnit: 0.0008, VerificationSamples: 4, VerificationFails: 4,
		},
		want: 0.0008, ok: true,
	}, {
		name: "a cell with no measured supplier cost is not free",
		cost: MeasuredSupplierLiabilityProxy{SupplierUSDPerUnit: 0, VerificationSamples: 40},
		ok:   false,
	}, {
		name: "terminal failures are not supplier payments",
		cost: MeasuredSupplierLiabilityProxy{
			SupplierUSDPerUnit: 0.001, VerificationSamples: 30,
			TerminalAttempts: 40, TerminalFails: 10,
		},
		want: 0.001, ok: true,
	}, {
		name: "even all terminal failures leave accepted payout unchanged",
		cost: MeasuredSupplierLiabilityProxy{
			SupplierUSDPerUnit: 0.001, VerificationSamples: 1,
			TerminalAttempts: 8, TerminalFails: 8,
		},
		want: 0.001, ok: true,
	}} {
		t.Run(tc.name, func(t *testing.T) {
			got, ok := tc.cost.ExpectedSupplierLiabilityUSDPerVerifiedUnit()
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

// Failure observations gate selection and promotion independently of the exact
// payable liability. A caller cannot set Measured=true on a malformed row to
// bypass retry burden, missing verification/terminal evidence, or a known
// failure. Retries are retained as diagnostics and leave the exact settlement
// liability untouched, but cannot rank until a governed reliability threshold
// exists.
func TestMeasuredSupplierLiabilityFailClosesReliabilityEvidence(t *testing.T) {
	clean := MeasuredSupplierLiabilityProxy{
		Samples: minSupplierLiabilitySamples, Measured: true,
		SupplierUSDPerUnit:  0.001,
		VerificationSamples: minSupplierLiabilitySamples,
		TerminalAttempts:    minSupplierLiabilitySamples,
	}
	withRetries := clean
	withRetries.RetryRate = 0.5
	if got, ok := withRetries.ExpectedSupplierLiabilityUSDPerVerifiedUnit(); !ok || got != 0.001 {
		t.Fatalf("accepted payout with retries = %.12f, ok=%v; want 0.001, true", got, ok)
	}
	if got, ok := measuredSupplierLiability(map[string]MeasuredSupplierLiabilityProxy{"cell": withRetries}, "cell"); ok {
		t.Fatalf("retry burden remained selector/promotion eligible at %.12f", got)
	}

	for _, tc := range []struct {
		name   string
		mutate func(*MeasuredSupplierLiabilityProxy)
	}{
		{"retry burden", func(c *MeasuredSupplierLiabilityProxy) { c.RetryRate = 0.5 }},
		{"negative retry rate", func(c *MeasuredSupplierLiabilityProxy) { c.RetryRate = -0.1 }},
		{"non-finite retry rate", func(c *MeasuredSupplierLiabilityProxy) { c.RetryRate = math.Inf(1) }},
		{"not-a-number retry rate", func(c *MeasuredSupplierLiabilityProxy) { c.RetryRate = math.NaN() }},
		{"authority refusal", func(c *MeasuredSupplierLiabilityProxy) { c.AuthorityRefusals = []string{"mixed execution build"} }},
		{"completed sample floor missing", func(c *MeasuredSupplierLiabilityProxy) { c.Samples-- }},
		{"verification sample floor missing", func(c *MeasuredSupplierLiabilityProxy) { c.VerificationSamples-- }},
		{"verification failure", func(c *MeasuredSupplierLiabilityProxy) { c.VerificationFails = 1 }},
		{"terminal sample floor missing", func(c *MeasuredSupplierLiabilityProxy) { c.TerminalAttempts-- }},
		{"terminal failure", func(c *MeasuredSupplierLiabilityProxy) { c.TerminalFails = 1 }},
		{"measurement bit false", func(c *MeasuredSupplierLiabilityProxy) { c.Measured = false }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			candidate := clean
			tc.mutate(&candidate)
			if got, ok := measuredSupplierLiability(
				map[string]MeasuredSupplierLiabilityProxy{"cell": candidate}, "cell"); ok {
				t.Fatalf("unreliable proxy remained eligible at %.12f: %+v", got, candidate)
			}
			if got, ok := candidate.ExpectedSupplierLiabilityUSDPerVerifiedUnit(); !ok || got != clean.SupplierUSDPerUnit {
				t.Fatalf("reliability changed payable liability: got %.12f, ok=%v; want %.12f, true",
					got, ok, clean.SupplierUSDPerUnit)
			}
		})
	}
}

func TestMeasuredSupplierLiabilityProxyCannotBorrowCurrentCellBuildOrDevice(t *testing.T) {
	installTestOnlyCombinedTokenAuthority(t)
	const cellID = "candle-metal-llama1-infer"
	profile, _, benchmark, err := currentRuntimeCellBenchmarkIdentity(cellID)
	mustf(t, err, "resolve current benchmark identity: %v")
	proxy := MeasuredSupplierLiabilityProxy{
		Engine: profile.Engine, ExecutionBuildHash: benchmark.EngineBuildHash,
		ExecutionBuildIdentityPolicy: benchmark.EngineBuildIdentityPolicy,
		HWClass:                      benchmark.HWClass, HardwareIdentity: benchmark.HardwareIdentity,
	}
	mustf(t, validateMeasuredProxyCurrentExecutionIdentity(proxy, cellID),
		"exact current execution identity was refused: %v")

	wrongBuild := proxy
	wrongBuild.ExecutionBuildHash = "0000000000000000"
	if wrongBuild.ExecutionBuildHash == proxy.ExecutionBuildHash {
		wrongBuild.ExecutionBuildHash = "1111111111111111"
	}
	if err := validateMeasuredProxyCurrentExecutionIdentity(wrongBuild, cellID); err == nil {
		t.Fatal("an observation from another execution build borrowed current cell authority")
	}
	wrongPolicy := proxy
	wrongPolicy.ExecutionBuildIdentityPolicy = externalRunnerBuildIdentityPolicy
	if wrongPolicy.ExecutionBuildIdentityPolicy == proxy.ExecutionBuildIdentityPolicy {
		wrongPolicy.ExecutionBuildIdentityPolicy = currentEngineBuildIdentityPolicy
	}
	if err := validateMeasuredProxyCurrentExecutionIdentity(wrongPolicy, cellID); err == nil {
		t.Fatal("an observation from another build-identity policy borrowed current cell authority")
	}
	wrongDevice := proxy
	wrongDevice.HardwareIdentity = "Apple M1 Ultra"
	if err := validateMeasuredProxyCurrentExecutionIdentity(wrongDevice, cellID); err == nil {
		t.Fatal("an observation from another device generation borrowed current cell authority")
	}
}

// A decision where only the winner has data must not report zero regret. That
// number would be indistinguishable from "the winner was measured to be best",
// and it is really "nothing else was tried".
func TestRegretIsNotScoredWhenOnlyTheWinnerIsMeasured(t *testing.T) {
	measured := MeasuredSupplierLiabilityProxy{
		Samples: minSupplierLiabilitySamples, SupplierUSDPerUnit: 0.001,
		VerificationSamples: 40, TerminalAttempts: 40, Measured: true,
	}
	cheaper := MeasuredSupplierLiabilityProxy{
		Samples: minSupplierLiabilitySamples, SupplierUSDPerUnit: 0.0005,
		VerificationSamples: 40, TerminalAttempts: 40, Measured: true,
	}
	underSampled := MeasuredSupplierLiabilityProxy{
		SupplierUSDPerUnit: 0.0001, VerificationSamples: 3, Samples: 3,
	}
	decision := shadowDecisionRow{
		RoutedCell: "a", ShadowCell: "a", Considered: []string{"a", "b"},
	}

	if _, _, ok := scoreDecisionLiabilityRegret(decision, map[string]MeasuredSupplierLiabilityProxy{
		"a": measured,
	}); ok {
		t.Fatal("scored a decision whose only candidate was the routed cell")
	}
	if _, _, ok := scoreDecisionLiabilityRegret(decision, map[string]MeasuredSupplierLiabilityProxy{
		"a": measured, "b": underSampled,
	}); ok {
		t.Fatal("scored against an under-sampled cell")
	}
	regret, cheapest, ok := scoreDecisionLiabilityRegret(decision, map[string]MeasuredSupplierLiabilityProxy{
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
	measured := func(hwClass, identity string, usd float64) MeasuredSupplierLiabilityProxy {
		return MeasuredSupplierLiabilityProxy{
			HWClass: hwClass, HardwareIdentity: identity,
			SupplierUSDPerUnit: usd, VerificationSamples: 40,
			TerminalAttempts: 40, Samples: minSupplierLiabilitySamples, Measured: true,
		}
	}
	byHW := map[string]map[string]MeasuredSupplierLiabilityProxy{
		// One cell each: no comparison is possible on either class alone.
		"apple_silicon_max": {"a": measured(
			"apple_silicon_max", "Apple M3 Max", 0.001)},
		"apple_silicon_ultra": {"b": measured(
			"apple_silicon_ultra", "Apple M3 Ultra", 0.0005)},
	}
	if hw := comparableHardwareFor(byHW, []string{"a", "b"}); hw != "" {
		t.Fatalf("compared across hardware classes: chose %q", hw)
	}

	byHW["apple_silicon_ultra"]["a"] = measured(
		"apple_silicon_ultra", "Apple M1 Ultra", 0.002)
	if hw := comparableHardwareFor(byHW, []string{"a", "b"}); hw != "" {
		t.Fatalf("compared across exact hardware generations inside one class: chose %q", hw)
	}
	byHW["apple_silicon_ultra"]["a"] = measured(
		"apple_silicon_ultra", "Apple M3 Ultra", 0.002)
	if hw := comparableHardwareFor(byHW, []string{"a", "b"}); hw != "apple_silicon_ultra" {
		t.Fatalf("hw = %q, want apple_silicon_ultra where both cells are measured", hw)
	}
	// On that class the cheaper cell wins, and it is the one measured THERE — not
	// cell "a" at its cheaper price on the other machine.
	ranked := rankCellsByMeasuredSupplierLiability(byHW["apple_silicon_ultra"], []string{"a", "b"})
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
	workerID         uuid.UUID
	supplierID       uuid.UUID
	hwClass          string
	hardwareIdentity string
	engine           string
}

func testCostHardwareIdentity(hwClass string) string {
	return "TEST_ONLY " + hwClass
}

func seedCostWorker(
	t *testing.T, ctx context.Context, pool *pgxpool.Pool, hwClass, engine, runtimeID string,
) costWorker {
	t.Helper()
	w := costWorker{
		workerID: uuid.New(), supplierID: uuid.New(), hwClass: hwClass,
		hardwareIdentity: testCostHardwareIdentity(hwClass), engine: engine,
	}
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
		INSERT INTO workers (id,supplier_id,hw_class,hardware_identity,memory_gb,effective_memory_gb,
		                     last_seen_at,throttled,min_payout_usd_hr,engine,build_hash,
		                     runtime_profile_id,runtime_profile_revision,runtime_profile_digest)
		VALUES ($1,$2,$3,$4,64,64,now(),false,0.10,$5,'deadbeefdeadbeef',$6,$7,$8)`,
		w.workerID, w.supplierID, hwClass, w.hardwareIdentity, engine,
		runtimeID, revision, digest); err != nil {
		t.Fatalf("insert cost worker: %v", err)
	}
	return w
}

// seedCompletedCellTasks writes n completed primary tasks on one cell, carrying
// the execution identity, units, duration, frozen money, verification outcome,
// and exact three-row ledger settlement a real accepted commit leaves behind.
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
	return seedCompletedCellTasksWithOptions(t, ctx, store, pool, w, cellID,
		runtimeID, n, unitsPerTask, durationMs, supplierUSDPerTask,
		buyerUSDPerTask, verificationFails, completedCellSeedOptions{})
}

type completedCellSeedOptions struct {
	ExecutionBuildHash           string
	ExecutionBuildIdentityPolicy string
	RuntimeMatrixSHA256          string
	RetryCount                   int
	InputDepthBand               string
	OmitFrozenSettlementGeometry bool
	OmitLedgerSettlement         bool
	LedgerBuyerUSDPerTask        *float64
	LedgerSupplierUSDPerTask     *float64
	MutateShadow                 func(*ShadowSelection)
}

func supplierLiabilityFloat64(v float64) *float64 { return &v }

func historicalSupplierLiabilityShadow(workload WorkloadDecision, routedCell string) ShadowSelection {
	shadow := ShadowSelection{
		RuntimeMatrixSHA: generatedRuntimeMatrixSHA256,
		PolicyRevision:   currentActivation().PolicyRevision,
		JobType:          "embed",
		ModelRef:         "all-minilm-l6-v2",
		ModelKind:        "hf",
		WorkloadClass:    "embeddings",
		LatencyClass:     "standard_batch",
		RoutedCellID:     routedCell,
		ShadowCellID:     routedCell,
		SelectionPolicy:  shadowSelectionPolicy,
		SelectionBasis:   selectionBasisLadder,
		Excluded:         []shadowExclusion{},
		Considered: []shadowCandidate{{
			CellID: candleEmbedCell, RuntimeID: "candle_metal", Engine: "candle",
			ModelKind: "hf", Lifecycle: runtimeLifecycleActive, Routable: true,
			QualityTier: "OUTCOME_EQUIVALENT", Verification: "cosine",
		}, {
			CellID: llamaEmbedCell, RuntimeID: "llama_cpp_metal", Engine: "llama_cpp",
			ModelKind: "gguf", Lifecycle: runtimeLifecycleRealRuntimeProven,
			Routable: true, QualityTier: "OUTCOME_EQUIVALENT", Verification: "cosine",
		}},
	}
	return shadow.withExecutionMode(workload)
}

// seedHistoricalSupplierLiabilityAuthorityJob persists the frozen authority
// needed to exercise the observation reader without asking today's admission
// path to accept it. That distinction is intentional: embed observations can
// exist historically, but new embed admission now refuses until an explicit
// embeddings-to-token-like-settlement conversion is bound. A reader test must
// not defeat that production refusal by relabelling embeddings/s as tokens/s.
//
// The database row still carries immutable workload, compute, placement, and
// pricing snapshots with content digests. It is inserted directly because the
// test is about replaying durable observations, not minting a new quote.
func seedHistoricalSupplierLiabilityAuthorityJob(
	t *testing.T, ctx context.Context, pool *pgxpool.Pool,
	f moneyPathFixture, cellID string, omitFrozenSettlementGeometry bool,
) WorkloadDecision {
	t.Helper()
	sub := jobSubmit{
		JobType: JobType{Type: "embed"},
		Model:   ModelRef{Kind: "hf", Ref: "all-minilm-l6-v2"},
		Tier:    "batch",
		Constraints: JobConstraints{
			MaxDurationSecs: 3600,
		},
	}
	binding, err := canonicalWorkloadBinding(sub, strings.Repeat("a", 64))
	mustf(t, err, "build historical %s workload binding: %v", cellID)
	bindingSHA, err := workloadBindingDigest(binding)
	mustf(t, err, "digest historical %s workload binding: %v", cellID)
	runtimeID, engine, modelKind := "candle_metal", "candle", "hf"
	if cellID == llamaEmbedCell {
		runtimeID, engine, modelKind = "llama_cpp_metal", "llama_cpp", "gguf"
	}
	workload := WorkloadDecision{
		Version: workloadDecisionVersion, BindingSHA256: bindingSHA, Binding: binding,
		WorkloadClass: "embeddings", RuntimeJobType: "embed",
		ModelRevision: modelRevisionFor("all-minilm-l6-v2"), DirectedCellID: cellID,
		RuntimeCandidates: []WorkloadRuntimeCandidate{{
			CellID: cellID, RuntimeID: runtimeID, Engine: engine, ModelKind: modelKind,
			Device: "metal", HardwareClasses: []string{"apple_silicon_base",
				"apple_silicon_pro", "apple_silicon_max", "apple_silicon_ultra"},
		}},
		MinimumMemoryGB: 64, Deterministic: true,
		VerificationStrategy: "cosine+platform_honeypot_floor",
		LatencyClass:         "standard_batch", PrivacyClass: "unrestricted_residency",
		Parallelism: WorkloadParallelism{
			Mode: "independent_task_fanout", TensorParallelDegree: 1,
		},
		Confidence: 0.9,
		Evidence:   []string{"historical replay-only supplier-liability observation fixture"},
	}
	mustf(t, ValidateFrozenWorkloadDecisionSnapshot(workload),
		"validate historical %s workload decision: %v", cellID)

	const inputRecords = 1
	const inputBytes = int64(128)
	compute, err := newDistributedComputePlan(
		workload, inputRecords, inputBytes, testInputDepthProfile(inputRecords),
		1, 1, 0, 0, quoteTimeFromETABands(60, 0, false), "static",
		f.Plan.Input.BaseComputeUSD, 0,
		QuoteConfidence{Score: 0.9, Reasons: []string{"historical observation fixture"}},
		[]string{"historical fixture is replay-only"},
	)
	mustf(t, err, "build historical %s compute authority: %v", cellID)
	if omitFrozenSettlementGeometry {
		// Model a uniformly old/incomplete cohort. Both omissions serialize as
		// absent JSON fields, so COUNT(DISTINCT geometry_signature) alone is one;
		// geometry_complete must independently refuse the missing authority.
		compute.SettlementInputUnits = 0
		compute.InputDepthProfile = nil
	}
	workloadSHA, err := workloadDecisionDigest(workload)
	mustf(t, err, "digest historical %s workload: %v", cellID)
	computeSHA, err := computePlanDigest(compute)
	mustf(t, err, "digest historical %s compute plan: %v", cellID)

	digestJSON := func(name string, value any) ([]byte, string) {
		blob, marshalErr := json.Marshal(value)
		mustf(t, marshalErr, "marshal historical %s %s: %v", cellID, name)
		sum := sha256.Sum256(blob)
		return blob, hex.EncodeToString(sum[:])
	}
	workloadJSON, _ := digestJSON("workload", workload)
	computeJSON, _ := digestJSON("compute", compute)
	placementJSON, placementSHA := digestJSON("placement", map[string]any{
		"version":         2,
		"runtime_cell_id": cellID,
		"authority":       "historical_replay_fixture",
	})
	const catalogueScheduleSHA = "cccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccc"
	const catalogueBoardSHA = "dddddddddddddddddddddddddddddddddddddddddddddddddddddddddddddddd"
	pricingJSON, pricingSHA := digestJSON("pricing", map[string]any{
		"execution_mode":               "distributed",
		"currency":                     "usd",
		"workload_decision_sha256":     workloadSHA,
		"compute_plan_sha256":          computeSHA,
		"placement_requirement_sha256": placementSHA,
		"billable_units":               pricingBillableUnitsForComputePlan(compute),
		"catalogue": map[string]any{
			"schedule_sha256":     catalogueScheduleSHA,
			"board_sha256":        catalogueBoardSHA,
			"settlement_currency": "usd",
		},
	})

	if _, err := pool.Exec(ctx, `
		INSERT INTO jobs (
		  id,buyer_id,status,job_type,model_ref,input_ref,task_count,tasks_done,
		  offered_rate_usd_hr,min_memory_gb,tier,estimated_usd,actual_usd,
		  firm_quote,sla_premium_usd,currency,
		  workload_decision,workload_decision_sha256,
		  compute_plan,compute_plan_sha256,
		  placement_requirement,placement_requirement_sha256,
		  pricing_decision,pricing_decision_sha256)
		VALUES ($1,$2,'running','embed','all-minilm-l6-v2','money/input',1,0,
		        0,0,'batch',$3,0,false,0,'usd',
		        $4,$5,$6,$7,$8,$9,$10,$11)`,
		f.JobID, f.BuyerID, f.Plan.InitialBuyerChargeUSD,
		workloadJSON, workloadSHA, computeJSON, computeSHA,
		placementJSON, placementSHA, pricingJSON, pricingSHA,
	); err != nil {
		t.Fatalf("insert historical %s authority job: %v", cellID, err)
	}
	planJSON, err := json.Marshal(f.Plan)
	mustf(t, err, "marshal historical %s economic authority: %v", cellID)
	if _, err := pool.Exec(ctx, `
		INSERT INTO job_economic_plans (
		  job_id,plan_version,schedule_version,plan_json,initial_task_count,
		  buyer_charge_per_task_usd,supplier_payout_per_task_usd,
		  initial_buyer_charge_usd,reserved_buyer_charge_usd,sla_premium_usd,
		  firm_quote_max_usd,currency,buyer_charge_per_task_nanos,
		  supplier_payout_per_task_nanos)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,'usd',$12,$13)`,
		f.JobID, f.Plan.Version, f.Plan.Schedule.Version, planJSON,
		f.Plan.Input.InitialTaskCount, f.Plan.BuyerChargePerTaskUSD,
		f.Plan.SupplierPayoutPerTaskUSD, f.Plan.InitialBuyerChargeUSD,
		f.Plan.ReservedBuyerChargeUSD, f.Plan.Input.SLAPremiumUSD,
		nullPosFloat(f.Plan.Input.FirmQuoteMaxUSD), f.Plan.BuyerChargePerTaskNanos,
		f.Plan.SupplierPayoutPerTaskNanos); err != nil {
		t.Fatalf("insert historical %s economic authority: %v", cellID, err)
	}
	return workload
}

func seedCompletedCellTasksWithOptions(
	t *testing.T, ctx context.Context, store *Store, pool *pgxpool.Pool,
	w costWorker, cellID, runtimeID string, n int, unitsPerTask int64,
	durationMs int64, supplierUSDPerTask, buyerUSDPerTask float64,
	verificationFails int, options completedCellSeedOptions,
) uuid.UUID {
	t.Helper()
	if options.ExecutionBuildHash == "" {
		options.ExecutionBuildHash = "deadbeefdeadbeef"
	}
	if options.ExecutionBuildIdentityPolicy == "" {
		options.ExecutionBuildIdentityPolicy = currentEngineBuildIdentityPolicy
	}
	if options.RuntimeMatrixSHA256 == "" {
		options.RuntimeMatrixSHA256 = generatedRuntimeMatrixSHA256
	}
	if options.InputDepthBand == "" {
		options.InputDepthBand = inputDepthBandShort
	}
	f := seedMoneyPathFixture(t, ctx, store, pool, moneyPathSeedOpts{TaskCount: 1})
	workload := seedHistoricalSupplierLiabilityAuthorityJob(t, ctx, pool, f, cellID,
		options.OmitFrozenSettlementGeometry)
	shadow := historicalSupplierLiabilityShadow(workload, cellID)
	if options.MutateShadow != nil {
		options.MutateShadow(&shadow)
	}
	mustf(t, store.RecordShadowSelection(ctx, f.JobID.String(), shadow),
		"record %s shadow selection: %v", cellID)
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
			   execution_hardware_identity,
			   execution_engine, execution_build_hash, execution_build_identity_policy,
			   runtime_cell_id, runtime_id, runtime_matrix_sha256, model_kind,
			   expected_output_records, reported_duration_ms, retry_count,
			   economic_buyer_charge_usd, economic_supplier_payout_usd,
			   verification_outcome, verified_at)
			VALUES ($1,$2,'money/input/chunk-0',$21,$3,$4,'complete',
			        now(),now(),$5,$5,$5,$6,$7,$8,$9,$10,$11,
			        $12,$13,$14,'hf',$15,$16,$17,$18,$19,$20,now())`,
			id, f.JobID, taskAttemptResultKey(f.JobID, id, 0), 1000+i,
			w.workerID, w.supplierID, w.hwClass, w.hardwareIdentity,
			w.engine, options.ExecutionBuildHash, options.ExecutionBuildIdentityPolicy,
			cellID, runtimeID, options.RuntimeMatrixSHA256,
			unitsPerTask, durationMs, options.RetryCount,
			buyerUSDPerTask, supplierUSDPerTask, outcome, options.InputDepthBand,
		); err != nil {
			t.Fatalf("insert completed task on %s: %v", cellID, err)
		}
		if outcome == "pass" && !options.OmitLedgerSettlement {
			ledgerBuyer, ledgerSupplier := buyerUSDPerTask, supplierUSDPerTask
			if options.LedgerBuyerUSDPerTask != nil {
				ledgerBuyer = *options.LedgerBuyerUSDPerTask
			}
			if options.LedgerSupplierUSDPerTask != nil {
				ledgerSupplier = *options.LedgerSupplierUSDPerTask
			}
			entries := splitFrozenCharge(
				f.BuyerID, w.supplierID, id, "usd",
				ledgerBuyer, ledgerSupplier, 0, time.Now().UTC(),
			)
			for _, entry := range entries {
				if _, err := insertLedgerEntryTx(ctx, pool, ledgerInsertFromEntry(entry)); err != nil {
					t.Fatalf("insert %s settlement on %s: %v", entry.Kind, cellID, err)
				}
			}
		}
	}
	return f.JobID
}

func testSupplierLiabilityScope(
	t *testing.T, ctx context.Context, store *Store, hwClass string,
) supplierLiabilityScope {
	t.Helper()
	now := time.Now().UTC().Add(time.Second)
	scope := supplierLiabilityScope{
		JobType: "embed", ModelRef: "all-minilm-l6-v2", HWClass: hwClass,
		HardwareIdentity:    testCostHardwareIdentity(hwClass),
		Tier:                "batch",
		RuntimeMatrixSHA256: generatedRuntimeMatrixSHA256,
		ModelRevision:       modelRevisionFor("all-minilm-l6-v2"),
		QualityTier:         "OUTCOME_EQUIVALENT", Verification: "cosine",
		LatencyClass: "standard_batch", SelectionPolicy: shadowSelectionPolicy,
		PolicyRevision: currentActivation().PolicyRevision,
		ObservedBefore: now, ObservedAfter: now.Add(-supplierLiabilityObservationWindow),
	}
	currency, schedule, err := store.resolveSupplierLiabilityEconomicEpoch(ctx, scope)
	mustf(t, err, "resolve test supplier-liability epoch: %v")
	scope.Currency, scope.CatalogueScheduleSHA256 = currency, schedule
	mustf(t, scope.validate(), "validate test supplier-liability scope: %v")
	return scope
}

// The measurement is read out of the money path, per cell and per hardware class.
func TestMeasuredSupplierLiabilityProxiesReadTheMoneyPathPerCell(t *testing.T) {
	ctx, store, pool := openIsolatedMoneyPathStore(t)
	const hw = "apple_silicon_ultra"
	candleWorker := seedCostWorker(t, ctx, pool, hw, "candle", "candle_metal")
	llamaWorker := seedCostWorker(t, ctx, pool, hw, "llama_cpp", "llama_cpp_metal")
	otherMachine := seedCostWorker(t, ctx, pool, "apple_silicon_max", "llama_cpp", "llama_cpp_metal")

	seedCompletedCellTasks(t, ctx, store, pool, candleWorker,
		candleEmbedCell, "candle_metal", minSupplierLiabilitySamples,
		100, 500, 0.000100, 0.000200, 0)
	// The challenger has a 20% lower frozen payout ceiling and fails one
	// verification in twenty. The rejected task has no supplier-credit
	// settlement, so the whole candidate must fail closed instead of treating
	// that missing money fact as zero or as the frozen ceiling.
	seedCompletedCellTasks(t, ctx, store, pool, llamaWorker,
		llamaEmbedCell, "llama_cpp_metal", minSupplierLiabilitySamples,
		100, 250, 0.000080, 0.000200, 1)
	// A handful of samples on a different machine must not join the comparison.
	seedCompletedCellTasks(t, ctx, store, pool, otherMachine,
		llamaEmbedCell, "llama_cpp_metal", 3,
		100, 100, 0.000010, 0.000200, 0)

	byHW, err := store.MeasuredSupplierLiabilityProxiesByHardware(
		ctx, testSupplierLiabilityScope(t, ctx, store, hw))
	mustf(t, err, "measured cell costs: %v")
	ultra := byHW[hw]
	if len(ultra) != 2 {
		t.Fatalf("cells measured on %s = %d, want 2 (%v)", hw, len(ultra), ultra)
	}
	candle, llama := ultra[candleEmbedCell], ultra[llamaEmbedCell]
	if candle.Currency != "usd" || llama.Currency != "usd" {
		t.Fatalf("supplier-liability money lacks explicit settlement currency: candle=%q llama=%q",
			candle.Currency, llama.Currency)
	}
	if !candle.Measured || llama.Measured {
		t.Fatalf("only the clean cell should be eligible at %d samples: candle=%+v llama=%+v",
			minSupplierLiabilitySamples, candle, llama)
	}
	if candle.Samples != minSupplierLiabilitySamples || llama.Samples != minSupplierLiabilitySamples {
		t.Fatalf("samples candle=%d llama=%d", candle.Samples, llama.Samples)
	}
	if !validSHA256(candle.InputGeometrySHA256) ||
		candle.InputGeometrySHA256 != llama.InputGeometrySHA256 {
		t.Fatalf("exact geometry authority is absent or disagrees: candle=%q llama=%q",
			candle.InputGeometrySHA256, llama.InputGeometrySHA256)
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
	candleCost, ok := candle.ExpectedSupplierLiabilityUSDPerVerifiedUnit()
	if !ok {
		t.Fatal("candle has no supplier-liability proxy")
	}
	if diff := candleCost - 0.000001; diff > 1e-15 || diff < -1e-15 {
		t.Fatalf("candle accepted supplier liability = %.15f", candleCost)
	}
	if llamaCost, ok := llama.ExpectedSupplierLiabilityUSDPerVerifiedUnit(); ok || llamaCost != 0 {
		t.Fatalf("missing rejected-task settlement produced a supplier-liability proxy: %.15f", llamaCost)
	}
	if !containsSubstring(llama.AuthorityRefusals, "found 0 supplier_credit") {
		t.Fatalf("missing rejected-task settlement was not named: %v", llama.AuthorityRefusals)
	}
	if _, ok := measuredSupplierLiability(ultra, llamaEmbedCell); ok {
		t.Fatalf("verification-failed row remained selector/promotion eligible: %+v", llama)
	}
	// The three-sample rows on the other machine exist but are not measured, and
	// are read under their own exact hardware scope rather than pooled here.
	maxByHW, err := store.MeasuredSupplierLiabilityProxiesByHardware(
		ctx, testSupplierLiabilityScope(t, ctx, store, "apple_silicon_max"))
	mustf(t, err, "measure other hardware scope: %v")
	if max := maxByHW["apple_silicon_max"][llamaEmbedCell]; max.Measured {
		t.Fatalf("3 samples reported as measured: %+v", max)
	}
	// No comparison is possible on either machine: the ultra challenger failed
	// verification and the max cohort is under-sampled.
	byHW["apple_silicon_max"] = maxByHW["apple_silicon_max"]
	if hwChoice := comparableHardwareFor(byHW, []string{candleEmbedCell, llamaEmbedCell}); hwChoice != "" {
		t.Fatalf("unreliable challenger produced comparable hardware %q", hwChoice)
	}
}

// A broad capacity class is not a physical comparison scope. Apple M1 Ultra
// and M3 Ultra observations must remain separate even when every other
// contract, runtime and task-geometry dimension is identical.
func TestSupplierLiabilityMeasurementRefusesCrossGenerationPooling(t *testing.T) {
	ctx, store, pool := openIsolatedMoneyPathStore(t)
	const hw = "apple_silicon_ultra"
	m1 := seedCostWorker(t, ctx, pool, hw, "candle", "candle_metal")
	m3 := seedCostWorker(t, ctx, pool, hw, "llama_cpp", "llama_cpp_metal")
	m1.hardwareIdentity = "Apple M1 Ultra"
	m3.hardwareIdentity = "Apple M3 Ultra"
	for _, worker := range []costWorker{m1, m3} {
		_, err := pool.Exec(ctx, `UPDATE workers SET hardware_identity=$2 WHERE id=$1`,
			worker.workerID, worker.hardwareIdentity)
		mustf(t, err, "set exact worker hardware identity: %v")
	}
	seedCompletedCellTasks(t, ctx, store, pool, m1,
		candleEmbedCell, "candle_metal", minSupplierLiabilitySamples,
		100, 500, 0.000100, 0.000200, 0)
	seedCompletedCellTasks(t, ctx, store, pool, m3,
		llamaEmbedCell, "llama_cpp_metal", minSupplierLiabilitySamples,
		100, 250, 0.000100, 0.000200, 0)

	for identity, wantCell := range map[string]string{
		"Apple M1 Ultra": candleEmbedCell,
		"Apple M3 Ultra": llamaEmbedCell,
	} {
		scope := testSupplierLiabilityScope(t, ctx, store, hw)
		scope.HardwareIdentity = identity
		byHW, err := store.MeasuredSupplierLiabilityProxiesByHardware(ctx, scope)
		mustf(t, err, "measure exact generation %s: %v", identity)
		if len(byHW[hw]) != 1 || !byHW[hw][wantCell].Measured {
			t.Fatalf("exact generation %s pooled another device: %+v", identity, byHW[hw])
		}
		if byHW[hw][wantCell].HardwareIdentity != identity {
			t.Fatalf("proxy hardware identity=%q, want %q",
				byHW[hw][wantCell].HardwareIdentity, identity)
		}
		if got := comparableHardwareFor(byHW,
			[]string{candleEmbedCell, llamaEmbedCell}); got != "" {
			t.Fatalf("single-generation cohort became a two-cell comparison on %q", got)
		}
	}
}

func TestMeasuredSupplierLiabilityRetryBurdenIsUnpaidAndIneligible(t *testing.T) {
	ctx, store, pool := openIsolatedMoneyPathStore(t)
	const hw = "apple_silicon_ultra"
	candleWorker := seedCostWorker(t, ctx, pool, hw, "candle", "candle_metal")
	llamaWorker := seedCostWorker(t, ctx, pool, hw, "llama_cpp", "llama_cpp_metal")
	seedCompletedCellTasks(t, ctx, store, pool, candleWorker,
		candleEmbedCell, "candle_metal", minSupplierLiabilitySamples,
		100, 500, 0.000100, 0.000200, 0)
	seedCompletedCellTasksWithOptions(t, ctx, store, pool, llamaWorker,
		llamaEmbedCell, "llama_cpp_metal", minSupplierLiabilitySamples,
		100, 250, 0.000080, 0.000200, 0,
		completedCellSeedOptions{RetryCount: 1})

	byHW, err := store.MeasuredSupplierLiabilityProxiesByHardware(
		ctx, testSupplierLiabilityScope(t, ctx, store, hw))
	mustf(t, err, "measure retry-burden cohort: %v")
	retried := byHW[hw][llamaEmbedCell]
	if retried.RetryRate != 1 {
		t.Fatalf("retry rate = %v, want 1", retried.RetryRate)
	}
	if retried.Measured {
		t.Fatalf("retry-burden row remained measured: %+v", retried)
	}
	payable, ok := retried.ExpectedSupplierLiabilityUSDPerVerifiedUnit()
	if !ok {
		t.Fatal("retry burden erased the accepted supplier payout")
	}
	if diff := payable - 0.0000008; diff > 1e-15 || diff < -1e-15 {
		t.Fatalf("retry burden inflated payable supplier liability: %.15f", payable)
	}
	if _, eligible := measuredSupplierLiability(byHW[hw], llamaEmbedCell); eligible {
		t.Fatalf("retry-burden row remained selector/promotion eligible: %+v", retried)
	}
	if hwChoice := comparableHardwareFor(byHW,
		[]string{candleEmbedCell, llamaEmbedCell}); hwChoice != "" {
		t.Fatalf("retry-burden challenger produced comparable hardware %q", hwChoice)
	}

	evidence, err := store.EvaluateCellPromotion(ctx, CellPromotionScope{
		JobType: "embed", ModelRef: "all-minilm-l6-v2",
		ModelRevision: modelRevisionFor("all-minilm-l6-v2"), Tier: "batch", HWClass: hw,
		HardwareIdentity: testCostHardwareIdentity(hw),
		LatencyClass:     "standard_batch", RuntimeID: "llama_cpp_metal", CellID: llamaEmbedCell,
		QualityTier: "OUTCOME_EQUIVALENT", Verification: "cosine",
	}, candleEmbedCell, time.Now())
	mustf(t, err, "evaluate retry-burden promotion: %v")
	if evidence.Passed() {
		t.Fatalf("promotion accepted retry burden: %+v", evidence)
	}
	if !containsSubstring(evidence.Refusals, "retry rate") {
		t.Fatalf("promotion refusal does not name retry burden: %v", evidence.Refusals)
	}
	if evidence.ChallengerSupplierLiabilityUSDPerVerifiedUnit != 0 {
		t.Fatalf("ineligible retry-burden liability entered promotion arithmetic: %v",
			evidence.ChallengerSupplierLiabilityUSDPerVerifiedUnit)
	}
}

func TestSupplierLiabilityMeasurementFailsClosedOnInvalidLedgerSettlement(t *testing.T) {
	for _, tc := range []struct {
		name        string
		options     completedCellSeedOptions
		mutate      func(*testing.T, context.Context, *pgxpool.Pool, uuid.UUID)
		wantRefusal string
	}{
		{
			name: "missing legacy supplier settlement",
			options: completedCellSeedOptions{
				OmitLedgerSettlement: true,
			},
			wantRefusal: "found 0 supplier_credit ledger rows",
		},
		{
			name: "settlement amount disagrees with canonical task authority",
			options: completedCellSeedOptions{
				LedgerSupplierUSDPerTask: supplierLiabilityFloat64(0.000090),
			},
			wantRefusal: "does not match canonical observed-output settlement",
		},
		{
			name: "wrong currency supplier settlement",
			mutate: func(t *testing.T, ctx context.Context, pool *pgxpool.Pool, jobID uuid.UUID) {
				t.Helper()
				if _, err := pool.Exec(ctx, `
					ALTER TABLE ledger_entries DISABLE TRIGGER ledger_currency_target_immutable;
					ALTER TABLE ledger_entries DISABLE TRIGGER ledger_task_currency_bound`); err != nil {
					t.Fatalf("disable currency guards for corruption fixture: %v", err)
				}
				if _, err := pool.Exec(ctx, `
					UPDATE ledger_entries SET currency='cad'
					 WHERE id=(
					   SELECT le.id FROM ledger_entries le
					   JOIN tasks t ON t.id=le.task_id
					    WHERE t.job_id=$1 AND le.kind='supplier_credit'
					    ORDER BY le.id LIMIT 1)`, jobID); err != nil {
					t.Fatalf("corrupt supplier settlement currency: %v", err)
				}
			},
			wantRefusal: "supplier_credit is not uniquely denominated in usd",
		},
		{
			name: "duplicate supplier settlement",
			mutate: func(t *testing.T, ctx context.Context, pool *pgxpool.Pool, jobID uuid.UUID) {
				t.Helper()
				if _, err := pool.Exec(ctx, `DROP INDEX ledger_task_kind_uniq`); err != nil {
					t.Fatalf("drop task-kind uniqueness for corruption fixture: %v", err)
				}
				var taskID, supplierID uuid.UUID
				if err := pool.QueryRow(ctx, `
					SELECT t.id,t.execution_supplier_id
					  FROM tasks t WHERE t.job_id=$1 ORDER BY t.id LIMIT 1`, jobID).
					Scan(&taskID, &supplierID); err != nil {
					t.Fatalf("resolve duplicate-settlement target: %v", err)
				}
				release := time.Now().UTC()
				if _, err := insertLedgerEntryTx(ctx, pool, ledgerInsert{
					Kind: KindSupplierCredit, SupplierID: &supplierID, TaskID: &taskID,
					AmountMicros: 100, Currency: "usd", PayoutStatus: PayoutHeld,
					ReleaseAt: &release,
				}); err != nil {
					t.Fatalf("insert duplicate supplier settlement: %v", err)
				}
			},
			wantRefusal: "found 2 supplier_credit ledger rows",
		},
		{
			name: "supplier credit was clawed back without mutating task outcome",
			mutate: func(t *testing.T, ctx context.Context, pool *pgxpool.Pool, jobID uuid.UUID) {
				t.Helper()
				if _, err := pool.Exec(ctx, `
					UPDATE ledger_entries SET payout_status='clawed_back',release_at=NULL
					 WHERE task_id=(SELECT id FROM tasks WHERE job_id=$1 ORDER BY id LIMIT 1)
					   AND kind='supplier_credit'`, jobID); err != nil {
					t.Fatalf("claw back supplier settlement status: %v", err)
				}
			},
			wantRefusal: "supplier credit is not an active settled liability",
		},
		{
			name: "supplier settlement has a clawback adjustment while task remains pass",
			mutate: func(t *testing.T, ctx context.Context, pool *pgxpool.Pool, jobID uuid.UUID) {
				t.Helper()
				var taskID, supplierID uuid.UUID
				if err := pool.QueryRow(ctx, `
					SELECT id,execution_supplier_id FROM tasks
					 WHERE job_id=$1 ORDER BY id LIMIT 1`, jobID).
					Scan(&taskID, &supplierID); err != nil {
					t.Fatalf("resolve clawback target: %v", err)
				}
				if _, err := insertLedgerEntryTx(ctx, pool, ledgerInsert{
					Kind: KindClawback, SupplierID: &supplierID, TaskID: &taskID,
					AmountMicros: -100, Currency: "usd", PayoutStatus: PayoutClawedBack,
				}); err != nil {
					t.Fatalf("insert clawback adjustment: %v", err)
				}
			},
			wantRefusal: "clawback/refund/adjustment ledger rows",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx, store, pool := openIsolatedMoneyPathStore(t)
			const hw = "apple_silicon_ultra"
			worker := seedCostWorker(t, ctx, pool, hw, "candle", "candle_metal")
			jobID := seedCompletedCellTasksWithOptions(
				t, ctx, store, pool, worker, candleEmbedCell, "candle_metal",
				minSupplierLiabilitySamples, 100, 500, 0.000100, 0.000200, 0,
				tc.options,
			)
			if tc.mutate != nil {
				tc.mutate(t, ctx, pool, jobID)
			}
			byHW, err := store.MeasuredSupplierLiabilityProxiesByHardware(
				ctx, testSupplierLiabilityScope(t, ctx, store, hw))
			mustf(t, err, "measure corrupt supplier settlement: %v")
			got := byHW[hw][candleEmbedCell]
			if got.Measured || got.SupplierUSDPerUnit != 0 {
				t.Fatalf("invalid settlement entered liability arithmetic: %+v", got)
			}
			if !containsSubstring(got.AuthorityRefusals, tc.wantRefusal) {
				t.Fatalf("refusal does not name %q: %v", tc.wantRefusal, got.AuthorityRefusals)
			}
		})
	}
}

func TestSupplierLiabilityMeasurementUsesOneRepeatableDisputeSnapshot(t *testing.T) {
	ctx, store, pool := openIsolatedMoneyPathStore(t)
	const hw = "apple_silicon_ultra"
	worker := seedCostWorker(t, ctx, pool, hw, "candle", "candle_metal")
	jobID := seedCompletedCellTasks(
		t, ctx, store, pool, worker, candleEmbedCell, "candle_metal",
		minSupplierLiabilitySamples, 100, 500, 0.000100, 0.000200, 0,
	)
	scope := testSupplierLiabilityScope(t, ctx, store, hw)

	var taskID uuid.UUID
	if err := pool.QueryRow(ctx, `
		SELECT id FROM tasks WHERE job_id=$1 ORDER BY id LIMIT 1`, jobID).
		Scan(&taskID); err != nil {
		t.Fatalf("resolve dispute target: %v", err)
	}

	// Commit an upheld-dispute shape after the cohort query has established its
	// snapshot but before the liability reader loads ledger/canonical money.
	// REPEATABLE READ must return either the old pass+settlement truth or the new
	// clawed-back truth, never a mixture of the two.
	previousHook := supplierLiabilityAfterCohortReadHook
	t.Cleanup(func() { supplierLiabilityAfterCohortReadHook = previousHook })
	supplierLiabilityAfterCohortReadHook = func() {
		tx, err := pool.Begin(ctx)
		mustf(t, err, "begin concurrent dispute mutation: %v")
		defer tx.Rollback(ctx)
		if _, err := tx.Exec(ctx, `
			UPDATE tasks SET verification_outcome='clawed_back', verified_at=clock_timestamp()
			 WHERE id=$1`, taskID); err != nil {
			t.Fatalf("mark disputed task clawed back: %v", err)
		}
		if _, err := tx.Exec(ctx, `
			UPDATE ledger_entries SET payout_status='clawed_back'
			 WHERE task_id=$1 AND kind='supplier_credit'`, taskID); err != nil {
			t.Fatalf("mark disputed supplier credit clawed back: %v", err)
		}
		if _, err := insertLedgerEntryTx(ctx, tx, ledgerInsert{
			Kind: "clawback", SupplierID: &worker.supplierID, TaskID: &taskID,
			AmountMicros: -100, Currency: "usd", PayoutStatus: PayoutClawedBack,
		}); err != nil {
			t.Fatalf("insert concurrent supplier clawback: %v", err)
		}
		mustf(t, tx.Commit(ctx), "commit concurrent dispute mutation: %v")
	}

	before, err := store.MeasuredSupplierLiabilityProxiesByHardware(ctx, scope)
	mustf(t, err, "measure across concurrent dispute: %v")
	oldSnapshot := before[hw][candleEmbedCell]
	if !oldSnapshot.Measured || oldSnapshot.VerificationFails != 0 {
		t.Fatalf("repeatable old snapshot mixed in concurrent dispute facts: %+v", oldSnapshot)
	}

	// A new measurement starts after the committed dispute and must see the new
	// reliability fact. Disable the hook so it runs exactly once.
	supplierLiabilityAfterCohortReadHook = nil
	after, err := store.MeasuredSupplierLiabilityProxiesByHardware(ctx, scope)
	mustf(t, err, "measure after committed dispute: %v")
	newSnapshot := after[hw][candleEmbedCell]
	if newSnapshot.Measured || newSnapshot.VerificationFails != 1 {
		t.Fatalf("post-dispute snapshot remained measured: %+v", newSnapshot)
	}
}

func TestSettledSupplierLiabilityUsesGenerativeObservedOutputAdjustment(t *testing.T) {
	installSettlementCurrencyForTest(t, "usd")
	installTestOnlyCombinedTokenAuthority(t)
	ctx, store, pool := openIsolatedMoneyPathStore(t)
	f := seedMoneyPathFixture(t, ctx, store, pool, moneyPathSeedOpts{TaskCount: 1})
	economic := BuildEconomicPlan(EconomicPlanInput{
		BaseComputeUSD: 0.20, InitialTaskCount: 1, SupplierShare: 0.50,
	}, EconomicSchedule{Version: "rebate-liability-test-v1", Currency: "usd"})
	if !economic.Executable {
		t.Fatalf("build generative liability economics: %s", economic.BlockReason)
	}
	mustf(t, ValidateEconomicPlanSnapshot(economic), "validate generative liability economics: %v")
	const cellID = "candle-metal-llama1-infer"
	workload, err := buildWorkloadDecisionDirected(jobSubmit{
		JobType: JobType{Type: "batch_infer", MaxTokens: 256},
		Model:   ModelRef{Kind: "gguf", Ref: "llama-3.2-1b-instruct-q4"},
		Tier:    "batch",
		Constraints: JobConstraints{
			MaxDurationSecs: 3600,
		},
	}, strings.Repeat("e", 64), cellID)
	mustf(t, err, "build generative liability workload: %v")
	compute, err := newDistributedComputePlan(
		workload, 1, 4096, testInputDepthProfile(1),
		1, 1, 0, 0, quoteTimeFromETABands(60, 0, false), "static",
		economic.Input.BaseComputeUSD, 0,
		QuoteConfidence{Score: 0.9, Reasons: []string{"settled liability regression fixture"}},
		[]string{"fixture"},
	)
	mustf(t, err, "build generative liability compute plan: %v")
	workloadJSON, err := json.Marshal(workload)
	mustf(t, err, "marshal generative liability workload: %v")
	workloadSHA, err := workloadDecisionDigest(workload)
	mustf(t, err, "digest generative liability workload: %v")
	computeJSON, err := json.Marshal(compute)
	mustf(t, err, "marshal generative liability compute plan: %v")
	computeSHA, err := computePlanDigest(compute)
	mustf(t, err, "digest generative liability compute plan: %v")
	if _, err := pool.Exec(ctx, `
		INSERT INTO jobs (
		  id,buyer_id,status,job_type,model_ref,input_ref,task_count,tasks_done,
		  tier,estimated_usd,actual_usd,currency,
		  workload_decision,workload_decision_sha256,
		  compute_plan,compute_plan_sha256)
		VALUES ($1,$2,'running','batch_infer','llama-3.2-1b-instruct-q4',
		        'money/generative-input',1,0,'batch',$3,0,'usd',$4,$5,$6,$7)`,
		f.JobID, f.BuyerID, economic.InitialBuyerChargeUSD,
		workloadJSON, workloadSHA, computeJSON, computeSHA); err != nil {
		t.Fatalf("insert generative liability job: %v", err)
	}
	planJSON, err := json.Marshal(economic)
	mustf(t, err, "marshal generative liability economic plan: %v")
	if _, err := pool.Exec(ctx, `
		INSERT INTO job_economic_plans (
		  job_id,plan_version,schedule_version,plan_json,initial_task_count,
		  buyer_charge_per_task_usd,supplier_payout_per_task_usd,
		  initial_buyer_charge_usd,reserved_buyer_charge_usd,sla_premium_usd,
		  firm_quote_max_usd,currency,buyer_charge_per_task_nanos,
		  supplier_payout_per_task_nanos)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,'usd',$12,$13)`,
		f.JobID, economic.Version, economic.Schedule.Version, planJSON,
		economic.Input.InitialTaskCount, economic.BuyerChargePerTaskUSD,
		economic.SupplierPayoutPerTaskUSD, economic.InitialBuyerChargeUSD,
		economic.ReservedBuyerChargeUSD, economic.Input.SLAPremiumUSD,
		nullPosFloat(economic.Input.FirmQuoteMaxUSD), economic.BuyerChargePerTaskNanos,
		economic.SupplierPayoutPerTaskNanos); err != nil {
		t.Fatalf("insert generative liability economic plan: %v", err)
	}
	taskID := uuid.New()
	if _, err := pool.Exec(ctx, `
		INSERT INTO tasks (
		  id,job_id,status,input_ref,result_key,chunk_index,
		  worker_id,claimed_by,execution_worker_id,execution_supplier_id,
		  execution_hw_class,execution_engine,execution_build_hash,
		  expected_output_records,reported_tokens_used,
		  economic_buyer_charge_usd,economic_supplier_payout_usd,
		  economic_buyer_charge_nanos,economic_supplier_payout_nanos,
		  verification_outcome,completed_at)
		VALUES ($1,$2,'complete','money/generative-input/chunk-0',$3,0,
		        $4,$4,$4,$5,'apple_silicon_max','candle','deadbeefdeadbeef',1,5,
		        $6,$7,$8,$9,'pass',now())`,
		taskID, f.JobID, taskAttemptResultKey(f.JobID, taskID, 0), f.WorkerID, f.SupplierID,
		economic.BuyerChargePerTaskUSD, economic.SupplierPayoutPerTaskUSD,
		economic.BuyerChargePerTaskNanos, economic.SupplierPayoutPerTaskNanos); err != nil {
		t.Fatalf("insert generative liability task: %v", err)
	}
	settled, err := loadObservedOutputSettlement(ctx, pool, taskID)
	mustf(t, err, "load generative observed-output settlement: %v")
	if !settled.Applied || settled.SupplierPayout >= economic.SupplierPayoutPerTaskUSD {
		t.Fatalf("fixture did not produce a lower adjusted supplier payout: settled=%+v frozen=%.6f",
			settled, economic.SupplierPayoutPerTaskUSD)
	}
	entries, err := splitFrozenChargeNanos(
		f.BuyerID, f.SupplierID, taskID, "usd",
		settled.BilledChargeNanos, settled.SupplierPayoutNanos, 0, time.Now().UTC(),
	)
	mustf(t, err, "split generative adjusted settlement: %v")
	for _, entry := range entries {
		if _, err := insertLedgerEntryTx(ctx, pool, ledgerInsertFromEntry(entry)); err != nil {
			t.Fatalf("insert generative %s settlement: %v", entry.Kind, err)
		}
	}

	facts, err := store.settledSupplierLiabilitiesForTasks(
		ctx, pool, []uuid.UUID{taskID}, "usd")
	mustf(t, err, "load settled generative supplier liability: %v")
	fact := facts[taskID]
	if !fact.valid() {
		t.Fatalf("adjusted generative settlement was refused: %v", fact.Refusals)
	}
	if fact.AmountMicros != projectNanosToMicros(settled.SupplierPayoutNanos) {
		t.Fatalf("settled supplier liability = %d micros, want adjusted %d (frozen ceiling %d)",
			fact.AmountMicros, projectNanosToMicros(settled.SupplierPayoutNanos),
			projectNanosToMicros(economic.SupplierPayoutPerTaskNanos))
	}
}

func TestSupplierLiabilityMeasurementRefusesCrossBuildSamplePooling(t *testing.T) {
	ctx, store, pool := openIsolatedMoneyPathStore(t)
	const hw = "apple_silicon_ultra"
	worker := seedCostWorker(t, ctx, pool, hw, "candle", "candle_metal")

	// Neither build independently clears the sample floor. Pooling them would
	// manufacture a 39-sample measurement for a binary that never produced one.
	seedCompletedCellTasksWithOptions(t, ctx, store, pool, worker,
		candleEmbedCell, "candle_metal", minSupplierLiabilitySamples-1,
		100, 500, 0.000100, 0.000200, 0,
		completedCellSeedOptions{ExecutionBuildHash: "build-revision-a"})
	seedCompletedCellTasksWithOptions(t, ctx, store, pool, worker,
		candleEmbedCell, "candle_metal", minSupplierLiabilitySamples,
		100, 500, 0.000100, 0.000200, 0,
		completedCellSeedOptions{ExecutionBuildHash: "build-revision-b"})

	byHW, err := store.MeasuredSupplierLiabilityProxiesByHardware(
		ctx, testSupplierLiabilityScope(t, ctx, store, hw))
	mustf(t, err, "measure mixed-build cohort: %v")
	got := byHW[hw][candleEmbedCell]
	if got.Samples != 2*minSupplierLiabilitySamples-1 {
		t.Fatalf("raw samples = %d, want %d", got.Samples, 2*minSupplierLiabilitySamples-1)
	}
	if got.Measured {
		t.Fatalf("mixed build identities were pooled into a measured proxy: %+v", got)
	}
	if !containsSubstring(got.AuthorityRefusals, "execution_build_hash") {
		t.Fatalf("refusal does not name unavailable build authority: %v", got.AuthorityRefusals)
	}
}

func TestSupplierLiabilityMeasurementRefusesCrossBuildPolicySamplePooling(t *testing.T) {
	ctx, store, pool := openIsolatedMoneyPathStore(t)
	const hw = "apple_silicon_ultra"
	worker := seedCostWorker(t, ctx, pool, hw, "candle", "candle_metal")

	// The same short build token is not interchangeable across identity-policy
	// epochs. Neither policy independently clears the sample floor; pooling them
	// would manufacture current liability evidence.
	seedCompletedCellTasksWithOptions(t, ctx, store, pool, worker,
		candleEmbedCell, "candle_metal", minSupplierLiabilitySamples-1,
		100, 500, 0.000100, 0.000200, 0,
		completedCellSeedOptions{
			ExecutionBuildHash:           "deadbeefdeadbeef",
			ExecutionBuildIdentityPolicy: currentEngineBuildIdentityPolicy,
		})
	seedCompletedCellTasksWithOptions(t, ctx, store, pool, worker,
		candleEmbedCell, "candle_metal", minSupplierLiabilitySamples,
		100, 500, 0.000100, 0.000200, 0,
		completedCellSeedOptions{
			ExecutionBuildHash:           "deadbeefdeadbeef",
			ExecutionBuildIdentityPolicy: externalRunnerBuildIdentityPolicy,
		})

	byHW, err := store.MeasuredSupplierLiabilityProxiesByHardware(
		ctx, testSupplierLiabilityScope(t, ctx, store, hw))
	mustf(t, err, "measure mixed-build-policy cohort: %v")
	got := byHW[hw][candleEmbedCell]
	if got.Samples != 2*minSupplierLiabilitySamples-1 {
		t.Fatalf("raw samples = %d, want %d", got.Samples, 2*minSupplierLiabilitySamples-1)
	}
	if got.Measured {
		t.Fatalf("mixed build-identity policies were pooled into a measured proxy: %+v", got)
	}
	if !containsSubstring(got.AuthorityRefusals, "execution_build_identity_policy") {
		t.Fatalf("refusal does not name unavailable build-policy authority: %v", got.AuthorityRefusals)
	}
}

func TestSupplierLiabilityMeasurementRefusesCrossGeometryTrafficMix(t *testing.T) {
	ctx, store, pool := openIsolatedMoneyPathStore(t)
	const hw = "apple_silicon_ultra"
	candleWorker := seedCostWorker(t, ctx, pool, hw, "candle", "candle_metal")
	llamaWorker := seedCostWorker(t, ctx, pool, hw, "llama_cpp", "llama_cpp_metal")

	// Same output count, payout, contract, and execution identity; only frozen
	// task input depth differs. The old output denominator made both rows look
	// directly comparable even though deeper traffic can independently raise the
	// catalogue payout (through billable units) and execution time.
	seedCompletedCellTasksWithOptions(t, ctx, store, pool, candleWorker,
		candleEmbedCell, "candle_metal", minSupplierLiabilitySamples,
		100, 1000, 0.000100, 0.000200, 0,
		completedCellSeedOptions{InputDepthBand: inputDepthBandShort})
	seedCompletedCellTasksWithOptions(t, ctx, store, pool, llamaWorker,
		llamaEmbedCell, "llama_cpp_metal", minSupplierLiabilitySamples,
		100, 250, 0.000100, 0.000200, 0,
		completedCellSeedOptions{InputDepthBand: inputDepthBandLong})

	byHW, err := store.MeasuredSupplierLiabilityProxiesByHardware(
		ctx, testSupplierLiabilityScope(t, ctx, store, hw))
	mustf(t, err, "measure mixed-geometry cohort: %v")
	for _, cell := range []string{candleEmbedCell, llamaEmbedCell} {
		got := byHW[hw][cell]
		if got.Measured || got.InputGeometrySHA256 != "" {
			t.Fatalf("mixed geometry remained rankable for %s: %+v", cell, got)
		}
		if !containsSubstring(got.AuthorityRefusals, "distinct geometries") {
			t.Fatalf("%s refusal does not name mixed geometry: %v", cell, got.AuthorityRefusals)
		}
	}

	evidence, err := store.EvaluateCellPromotion(ctx, CellPromotionScope{
		JobType: "embed", ModelRef: "all-minilm-l6-v2",
		ModelRevision: modelRevisionFor("all-minilm-l6-v2"), Tier: "batch", HWClass: hw,
		HardwareIdentity: testCostHardwareIdentity(hw),
		LatencyClass:     "standard_batch", RuntimeID: "llama_cpp_metal", CellID: llamaEmbedCell,
		QualityTier: "OUTCOME_EQUIVALENT", Verification: "cosine",
	}, candleEmbedCell, time.Now())
	mustf(t, err, "evaluate mixed-geometry promotion: %v")
	if evidence.Passed() || evidence.LatencyRatio != 0 {
		t.Fatalf("mixed geometry entered promotion or latency arithmetic: %+v", evidence)
	}
	if !containsSubstring(evidence.Refusals, "distinct geometries") {
		t.Fatalf("promotion refusal does not expose mixed geometry: %v", evidence.Refusals)
	}
}

func TestSupplierLiabilityMeasurementRefusesUniformIncompleteGeometry(t *testing.T) {
	ctx, store, pool := openIsolatedMoneyPathStore(t)
	const hw = "apple_silicon_ultra"
	candleWorker := seedCostWorker(t, ctx, pool, hw, "candle", "candle_metal")
	llamaWorker := seedCostWorker(t, ctx, pool, hw, "llama_cpp", "llama_cpp_metal")

	// Identical null fields produce one distinct JSON signature. They still do
	// not prove the settlement denominator or input-depth geometry, and therefore
	// must never become a measured/rankable cohort.
	for _, candidate := range []struct {
		worker    costWorker
		cellID    string
		runtimeID string
	}{
		{candleWorker, candleEmbedCell, "candle_metal"},
		{llamaWorker, llamaEmbedCell, "llama_cpp_metal"},
	} {
		seedCompletedCellTasksWithOptions(t, ctx, store, pool, candidate.worker,
			candidate.cellID, candidate.runtimeID, minSupplierLiabilitySamples,
			100, 500, 0.000100, 0.000200, 0,
			completedCellSeedOptions{OmitFrozenSettlementGeometry: true})
	}

	byHW, err := store.MeasuredSupplierLiabilityProxiesByHardware(
		ctx, testSupplierLiabilityScope(t, ctx, store, hw))
	mustf(t, err, "measure uniformly incomplete geometry: %v")
	for _, cell := range []string{candleEmbedCell, llamaEmbedCell} {
		got := byHW[hw][cell]
		if got.Measured || got.InputGeometrySHA256 != "" {
			t.Fatalf("uniformly incomplete geometry remained rankable for %s: %+v", cell, got)
		}
		if !containsSubstring(got.AuthorityRefusals, "authority is incomplete") {
			t.Fatalf("%s refusal does not name incomplete geometry authority: %v",
				cell, got.AuthorityRefusals)
		}
	}
}

func TestPromotionRequiresDecisionThatConsideredExactNamedPair(t *testing.T) {
	ctx, store, pool := openIsolatedMoneyPathStore(t)
	const hw = "apple_silicon_ultra"
	candleWorker := seedCostWorker(t, ctx, pool, hw, "candle", "candle_metal")
	llamaWorker := seedCostWorker(t, ctx, pool, hw, "llama_cpp", "llama_cpp_metal")
	// Persist an in-epoch A/B decision with the same contract. It is real
	// production-decision evidence, but it says nothing about the C/D pair this
	// promotion names.
	seedCompletedCellTasksWithOptions(t, ctx, store, pool, candleWorker,
		candleEmbedCell, "candle_metal", 0,
		100, 1000, 0.000100, 0.000200, 0,
		completedCellSeedOptions{MutateShadow: func(shadow *ShadowSelection) {
			shadow.RoutedCellID = "unrelated-a"
			shadow.ShadowCellID = "unrelated-b"
			shadow.Considered = []shadowCandidate{
				{CellID: "unrelated-a", QualityTier: "OUTCOME_EQUIVALENT", Verification: "cosine"},
				{CellID: "unrelated-b", QualityTier: "OUTCOME_EQUIVALENT", Verification: "cosine"},
			}
		}})

	singleArm := func(cellID string) func(*ShadowSelection) {
		return func(shadow *ShadowSelection) {
			for _, candidate := range shadow.Considered {
				if candidate.CellID == cellID {
					shadow.Considered = []shadowCandidate{candidate}
					shadow.ShadowCellID = cellID
					return
				}
			}
			t.Fatalf("planned shadow did not contain single-arm cell %s", cellID)
		}
	}
	seedCompletedCellTasksWithOptions(t, ctx, store, pool, candleWorker,
		candleEmbedCell, "candle_metal", minSupplierLiabilitySamples,
		100, 1000, 0.000100, 0.000200, 0,
		completedCellSeedOptions{MutateShadow: singleArm(candleEmbedCell)})
	seedCompletedCellTasksWithOptions(t, ctx, store, pool, llamaWorker,
		llamaEmbedCell, "llama_cpp_metal", minSupplierLiabilitySamples,
		100, 250, 0.000100, 0.000200, 0,
		completedCellSeedOptions{MutateShadow: singleArm(llamaEmbedCell)})

	evidence, err := store.EvaluateCellPromotion(ctx, CellPromotionScope{
		JobType: "embed", ModelRef: "all-minilm-l6-v2",
		ModelRevision: modelRevisionFor("all-minilm-l6-v2"), Tier: "batch", HWClass: hw,
		HardwareIdentity: testCostHardwareIdentity(hw),
		LatencyClass:     "standard_batch", RuntimeID: "llama_cpp_metal",
		CellID: llamaEmbedCell, QualityTier: "OUTCOME_EQUIVALENT", Verification: "cosine",
	}, candleEmbedCell, time.Now())
	mustf(t, err, "evaluate exact-pair proof: %v")
	if !evidence.ChallengerSupplierLiability.Measured || !evidence.IncumbentSupplierLiability.Measured {
		t.Fatalf("test did not establish measured observations for both cells: challenger=%+v incumbent=%+v",
			evidence.ChallengerSupplierLiability, evidence.IncumbentSupplierLiability)
	}
	if evidence.LiabilityRegret.ExactPairScoredDecisions != 0 ||
		evidence.LiabilityRegret.UnrelatedDecisions == 0 {
		t.Fatalf("unrelated decisions authorized the pair: %+v", evidence.LiabilityRegret)
	}
	if evidence.LiabilityRegret.Currency != "usd" {
		t.Fatalf("liability regret currency=%q, want scoped settlement currency usd",
			evidence.LiabilityRegret.Currency)
	}
	if evidence.Passed() || !containsSubstring(evidence.Refusals, "exact incumbent/challenger pair") {
		t.Fatalf("gate did not refuse missing exact-pair decision proof: %v", evidence.Refusals)
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
		JobType: "embed", ModelRef: "all-minilm-l6-v2",
		ModelRevision: modelRevisionFor("all-minilm-l6-v2"), Tier: "batch", HWClass: hw,
		HardwareIdentity: testCostHardwareIdentity(hw),
		LatencyClass:     "standard_batch", RuntimeID: "llama_cpp_metal", CellID: llamaEmbedCell,
		QualityTier: "OUTCOME_EQUIVALENT", Verification: "cosine",
	}

	// Nothing has run at all.
	evidence, err := store.EvaluateCellPromotion(ctx, scope, candleEmbedCell, time.Now())
	mustf(t, err, "evaluate with no evidence: %v")
	if evidence.Passed() {
		t.Fatal("gate passed with no measurement whatsoever")
	}
	if !containsSubstring(evidence.Refusals, "no measured supplier-liability proxy") {
		t.Fatalf("refusals should name the missing measurement: %v", evidence.Refusals)
	}
	if evidence.RollbackTargetRevision != currentActivation().PolicyRevision {
		t.Fatalf("rollback target = %d, want the current policy revision %d",
			evidence.RollbackTargetRevision, currentActivation().PolicyRevision)
	}

	// Both cells are measured and their supplier-liability proxies differ. That
	// difference cannot authorize a total-cost promotion while platform costs
	// remain unknown, regardless of its size.
	seedCompletedCellTasks(t, ctx, store, pool, candleWorker,
		candleEmbedCell, "candle_metal", minSupplierLiabilitySamples,
		100, 500, 0.000100, 0.000200, 0)
	seedCompletedCellTasks(t, ctx, store, pool, llamaWorker,
		llamaEmbedCell, "llama_cpp_metal", minSupplierLiabilitySamples,
		100, 400, 0.000095, 0.000200, 0)
	evidence, err = store.EvaluateCellPromotion(ctx, scope, candleEmbedCell, time.Now())
	mustf(t, err, "evaluate thin margin: %v")
	if evidence.Passed() {
		t.Fatalf("gate passed an unequal-liability cost claim: %+v", evidence)
	}
	if evidence.Basis != promotionBasisSupplierLiabilityOnly {
		t.Fatalf("basis = %q, want explicit supplier-liability-only refusal", evidence.Basis)
	}
	if !containsSubstring(evidence.Refusals, "platform-cost components remain unknown") {
		t.Fatalf("refusals should name the incomplete cost authority: %v", evidence.Refusals)
	}

	// A receipt reference is derivable either way, and it changes when the
	// evidence changes — a refusal and a pass cannot share an identity.
	firstRef, err := evidence.ReceiptRef()
	mustf(t, err, "receipt ref: %v")
	if firstRef == "" {
		t.Fatal("empty receipt ref")
	}
	evidence.SupplierLiabilityReductionFraction = 0.99
	secondRef, err := evidence.ReceiptRef()
	mustf(t, err, "receipt ref after mutation: %v")
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
		candleEmbedCell, "candle_metal", minSupplierLiabilitySamples,
		100, 500, 0.000100, 0.000200, 0)
	// Half the frozen supplier ceiling, one verification failure. The rejected
	// task has no accepted supplier-credit settlement.
	seedCompletedCellTasks(t, ctx, store, pool, llamaWorker,
		llamaEmbedCell, "llama_cpp_metal", minSupplierLiabilitySamples,
		100, 200, 0.000050, 0.000200, 1)

	evidence, err := store.EvaluateCellPromotion(ctx, CellPromotionScope{
		JobType: "embed", ModelRef: "all-minilm-l6-v2",
		ModelRevision: modelRevisionFor("all-minilm-l6-v2"), Tier: "batch", HWClass: hw,
		HardwareIdentity: testCostHardwareIdentity(hw),
		LatencyClass:     "standard_batch", RuntimeID: "llama_cpp_metal", CellID: llamaEmbedCell,
		QualityTier: "OUTCOME_EQUIVALENT", Verification: "cosine",
	}, candleEmbedCell, time.Now())
	mustf(t, err, "evaluate: %v")
	if evidence.Passed() {
		t.Fatalf("gate promoted a cell that failed verification: %+v", evidence)
	}
	if !containsSubstring(evidence.Refusals, "failed verification") {
		t.Fatalf("refusals should name the verification failure: %v", evidence.Refusals)
	}
	if evidence.ChallengerSupplierLiabilityUSDPerVerifiedUnit != 0 ||
		evidence.SupplierLiabilityReductionFraction != 0 {
		t.Fatalf("ineligible challenger leaked into promotion arithmetic: liability=%v reduction=%v",
			evidence.ChallengerSupplierLiabilityUSDPerVerifiedUnit,
			evidence.SupplierLiabilityReductionFraction)
	}
	payable, ok := evidence.ChallengerSupplierLiability.ExpectedSupplierLiabilityUSDPerVerifiedUnit()
	if ok || payable != 0 {
		t.Fatalf("missing rejected-task settlement became payable liability: payable=%v ok=%v proxy=%+v",
			payable, ok, evidence.ChallengerSupplierLiability)
	}
	if !containsSubstring(evidence.ChallengerSupplierLiability.AuthorityRefusals,
		"found 0 supplier_credit") {
		t.Fatalf("challenger liability did not expose missing settlement: %v",
			evidence.ChallengerSupplierLiability.AuthorityRefusals)
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
	workload := seedHistoricalSupplierLiabilityAuthorityJob(t, ctx, pool, f, cellID, false)
	shadow := historicalSupplierLiabilityShadow(workload, cellID)
	mustf(t, store.RecordShadowSelection(ctx, f.JobID.String(), shadow),
		"record %s failed-task shadow selection: %v", cellID)
	for i := 0; i < n; i++ {
		id := uuid.New()
		if _, err := pool.Exec(ctx, `
			INSERT INTO tasks
			  (id, job_id, input_ref, result_key, chunk_index, status,
			   started_at, worker_id, claimed_by,
			   execution_worker_id, execution_supplier_id, execution_hw_class,
			   execution_hardware_identity,
			   execution_engine, execution_build_hash,
			   runtime_cell_id, runtime_id, runtime_matrix_sha256, model_kind)
			VALUES ($1,$2,'money/input/chunk-0',$3,$4,'failed',
			        now(),$5,$5,$5,$6,$7,$8,$9,'deadbeefdeadbeef',$10,$11,$12,'hf')`,
			id, f.JobID, taskAttemptResultKey(f.JobID, id, 0), 5000+i,
			w.workerID, w.supplierID, w.hwClass, w.hardwareIdentity, w.engine,
			cellID, runtimeID, generatedRuntimeMatrixSHA256,
		); err != nil {
			t.Fatalf("insert failed task on %s: %v", cellID, err)
		}
	}
}

// A cell that crashes on work it claimed is ineligible even when its accepted
// supplier payout is low. The completed-task sample cannot see that on its own,
// and the failure must not be converted into money the supplier never receives.
func TestOutrightFailuresLeavePayableLiabilityExactAndBlockPromotion(t *testing.T) {
	ctx, store, pool := openIsolatedMoneyPathStore(t)
	const hw = "apple_silicon_ultra"
	candleWorker := seedCostWorker(t, ctx, pool, hw, "candle", "candle_metal")
	llamaWorker := seedCostWorker(t, ctx, pool, hw, "llama_cpp", "llama_cpp_metal")

	seedCompletedCellTasks(t, ctx, store, pool, candleWorker,
		candleEmbedCell, "candle_metal", minSupplierLiabilitySamples,
		100, 500, 0.000100, 0.000200, 0)
	// Half the price on everything it finished, and it failed a quarter of what
	// it claimed.
	seedCompletedCellTasks(t, ctx, store, pool, llamaWorker,
		llamaEmbedCell, "llama_cpp_metal", minSupplierLiabilitySamples,
		100, 500, 0.000050, 0.000200, 0)
	seedFailedCellTasks(t, ctx, store, pool, llamaWorker,
		llamaEmbedCell, "llama_cpp_metal", minSupplierLiabilitySamples/2)

	byHW, err := store.MeasuredSupplierLiabilityProxiesByHardware(
		ctx, testSupplierLiabilityScope(t, ctx, store, hw))
	must(t, err)
	llama := byHW[hw][llamaEmbedCell]
	if llama.TerminalFails != minSupplierLiabilitySamples/2 {
		t.Fatalf("terminal fails = %d, want %d", llama.TerminalFails, minSupplierLiabilitySamples/2)
	}
	if llama.TerminalAttempts != minSupplierLiabilitySamples+minSupplierLiabilitySamples/2 {
		t.Fatalf("terminal attempts = %d, want %d",
			llama.TerminalAttempts, minSupplierLiabilitySamples+minSupplierLiabilitySamples/2)
	}
	withFailures, ok := llama.ExpectedSupplierLiabilityUSDPerVerifiedUnit()
	if !ok {
		t.Fatal("accepted supplier payout should remain observable beside failures")
	}
	clean := llama
	clean.TerminalAttempts, clean.TerminalFails = 0, 0
	ignoringFailures, _ := clean.ExpectedSupplierLiabilityUSDPerVerifiedUnit()
	if withFailures != ignoringFailures {
		t.Fatalf("terminal failures changed supplier payout: %.12f vs %.12f",
			withFailures, ignoringFailures)
	}
	if llama.Measured {
		t.Fatalf("terminal-failed row remained measured: %+v", llama)
	}
	if _, eligible := measuredSupplierLiability(byHW[hw], llamaEmbedCell); eligible {
		t.Fatalf("terminal-failed row remained selector/promotion eligible: %+v", llama)
	}

	evidence, err := store.EvaluateCellPromotion(ctx, CellPromotionScope{
		JobType: "embed", ModelRef: "all-minilm-l6-v2",
		ModelRevision: modelRevisionFor("all-minilm-l6-v2"), Tier: "batch", HWClass: hw,
		HardwareIdentity: testCostHardwareIdentity(hw),
		LatencyClass:     "standard_batch", RuntimeID: "llama_cpp_metal", CellID: llamaEmbedCell,
		QualityTier: "OUTCOME_EQUIVALENT", Verification: "cosine",
	}, candleEmbedCell, time.Now())
	must(t, err)
	if evidence.Passed() {
		t.Fatalf("gate promoted a cell that failed a third of its attempts: %+v", evidence)
	}
	if !containsSubstring(evidence.Refusals, "failed outright") {
		t.Fatalf("refusals should name the outright failures: %v", evidence.Refusals)
	}
}

// Two cells serving one model cost the same per unit BY CONSTRUCTION, because the
// supplier payout is priced per model. The comparison is therefore a capacity
// argument, not a saving; it remains non-authorizing until matched-pair authority
// exists.
func TestSamePriceComparisonReportsThroughputButRefusesWithoutMatchedPairAuthority(t *testing.T) {
	ctx, store, pool := openIsolatedMoneyPathStore(t)
	const hw = "apple_silicon_ultra"
	candleWorker := seedCostWorker(t, ctx, pool, hw, "candle", "candle_metal")
	llamaWorker := seedCostWorker(t, ctx, pool, hw, "llama_cpp", "llama_cpp_metal")

	// Identical money, different speed: exactly what the first real cohort measured.
	seedCompletedCellTasks(t, ctx, store, pool, candleWorker,
		candleEmbedCell, "candle_metal", minSupplierLiabilitySamples,
		100, 1000, 0.000100, 0.000200, 0)
	seedCompletedCellTasks(t, ctx, store, pool, llamaWorker,
		llamaEmbedCell, "llama_cpp_metal", minSupplierLiabilitySamples,
		100, 250, 0.000100, 0.000200, 0)

	scope := CellPromotionScope{
		JobType: "embed", ModelRef: "all-minilm-l6-v2",
		ModelRevision: modelRevisionFor("all-minilm-l6-v2"), Tier: "batch", HWClass: hw,
		HardwareIdentity: testCostHardwareIdentity(hw),
		LatencyClass:     "standard_batch", RuntimeID: "llama_cpp_metal",
		CellID: llamaEmbedCell, QualityTier: "OUTCOME_EQUIVALENT", Verification: "cosine",
	}
	evidence, err := store.EvaluateCellPromotion(ctx, scope, candleEmbedCell, time.Now())
	must(t, err)
	if evidence.Basis != promotionBasisThroughput {
		t.Fatalf("basis = %q, want %q for two cells at one price",
			evidence.Basis, promotionBasisThroughput)
	}
	if containsSubstring(evidence.Refusals, "cost-based promotion refused") {
		t.Fatalf("a same-price promotion was refused as a failed COST argument: %v",
			evidence.Refusals)
	}
	// Four times faster clears the 25% throughput margin.
	if evidence.ThroughputGainFraction < 0.7 {
		t.Fatalf("throughput gain = %.3f, want ~0.75", evidence.ThroughputGainFraction)
	}
	if containsSubstring(evidence.Refusals, "throughput margin") {
		t.Fatalf("a 4x faster cell was refused on the throughput margin: %v", evidence.Refusals)
	}
	if evidence.Passed() ||
		!containsSubstring(evidence.Refusals, "no durable matched incumbent/challenger execution-pair authority") {
		t.Fatalf("independent aggregate jobs masqueraded as durable paired evidence: %v",
			evidence.Refusals)
	}
}

// Same price AND same speed buys nothing, and the gate says so on the throughput
// margin rather than on a saving it was never going to find.
func TestSamePriceSameSpeedPromotionIsRefused(t *testing.T) {
	ctx, store, pool := openIsolatedMoneyPathStore(t)
	const hw = "apple_silicon_ultra"
	candleWorker := seedCostWorker(t, ctx, pool, hw, "candle", "candle_metal")
	llamaWorker := seedCostWorker(t, ctx, pool, hw, "llama_cpp", "llama_cpp_metal")
	for _, seed := range []struct {
		w               costWorker
		cell, runtimeID string
	}{
		{candleWorker, candleEmbedCell, "candle_metal"},
		{llamaWorker, llamaEmbedCell, "llama_cpp_metal"},
	} {
		seedCompletedCellTasks(t, ctx, store, pool, seed.w,
			seed.cell, seed.runtimeID, minSupplierLiabilitySamples,
			100, 1000, 0.000100, 0.000200, 0)
	}
	flat, err := store.EvaluateCellPromotion(ctx, CellPromotionScope{
		JobType: "embed", ModelRef: "all-minilm-l6-v2",
		ModelRevision: modelRevisionFor("all-minilm-l6-v2"), Tier: "batch", HWClass: hw,
		HardwareIdentity: testCostHardwareIdentity(hw),
		LatencyClass:     "standard_batch", RuntimeID: "llama_cpp_metal",
		CellID: llamaEmbedCell, QualityTier: "OUTCOME_EQUIVALENT", Verification: "cosine",
	}, candleEmbedCell, time.Now())
	must(t, err)
	if flat.Passed() {
		t.Fatalf("promoted a cell that is neither cheaper nor faster: %+v", flat)
	}
	if !containsSubstring(flat.Refusals, "throughput margin") {
		t.Fatalf("refusals should name the throughput margin: %v", flat.Refusals)
	}
}

// Unequal supplier liability never authorizes a total-cost promotion while
// platform terms are unknown. Observations from batch work also cannot be
// recycled into an interactive contract merely to manufacture latency evidence.
func TestUnequalLiabilityPromotionRefusesCostAndDoesNotReuseBatchForInteractive(t *testing.T) {
	ctx, store, pool := openIsolatedMoneyPathStore(t)
	const hw = "apple_silicon_ultra"
	candleWorker := seedCostWorker(t, ctx, pool, hw, "candle", "candle_metal")
	llamaWorker := seedCostWorker(t, ctx, pool, hw, "llama_cpp", "llama_cpp_metal")

	// The incumbent is fast with higher supplier liability; the challenger has
	// half the liability proxy and is four times slower per unit.
	seedCompletedCellTasks(t, ctx, store, pool, candleWorker,
		candleEmbedCell, "candle_metal", minSupplierLiabilitySamples,
		100, 200, 0.000100, 0.000200, 0)
	seedCompletedCellTasks(t, ctx, store, pool, llamaWorker,
		llamaEmbedCell, "llama_cpp_metal", minSupplierLiabilitySamples,
		100, 800, 0.000050, 0.000200, 0)

	scope := CellPromotionScope{
		JobType: "embed", ModelRef: "all-minilm-l6-v2",
		ModelRevision: modelRevisionFor("all-minilm-l6-v2"), Tier: "batch", HWClass: hw,
		HardwareIdentity: testCostHardwareIdentity(hw),
		RuntimeID:        "llama_cpp_metal", CellID: llamaEmbedCell,
		QualityTier: "OUTCOME_EQUIVALENT", Verification: "cosine",
	}

	scope.LatencyClass = "standard_batch"
	batch, err := store.EvaluateCellPromotion(ctx, scope, candleEmbedCell, time.Now())
	mustf(t, err, "evaluate batch scope: %v")
	if !containsSubstring(batch.Refusals, "cost-based promotion refused") {
		t.Fatalf("batch scope did not refuse the incomplete total-cost claim: %v", batch.Refusals)
	}
	if containsSubstring(batch.Refusals, "slower per unit") {
		t.Fatalf("batch scope fabricated a latency-product refusal: %v", batch.Refusals)
	}
	if batch.LatencyRatio < 3.9 || batch.LatencyRatio > 4.1 {
		t.Fatalf("latency ratio = %v, want ~4 reported even when permitted", batch.LatencyRatio)
	}

	scope.LatencyClass = string(TrafficInteractive)
	interactive, err := store.EvaluateCellPromotion(ctx, scope, candleEmbedCell, time.Now())
	mustf(t, err, "evaluate interactive scope: %v")
	if interactive.ChallengerSupplierLiability.Measured ||
		interactive.IncumbentSupplierLiability.Measured || interactive.LatencyRatio != 0 {
		t.Fatalf("batch observations contaminated interactive scope: %+v", interactive)
	}
	if !containsSubstring(interactive.Refusals, "no immutable job/shadow authority") ||
		!containsSubstring(interactive.Refusals, "no measured supplier-liability proxy") {
		t.Fatalf("interactive refusal did not name unavailable exact-scope authority: %v",
			interactive.Refusals)
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
