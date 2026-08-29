package main

import (
	"encoding/json"
	"errors"
	"math"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
)

func TestDecisionTraceTwoCandidatesChoosesVerifiedOutcomeNotFastest(t *testing.T) {
	now := time.Date(2026, 8, 24, 18, 0, 0, 0, time.UTC)
	workload := twoCandidateWorkload(now)
	apple := appleWorker(now)
	nvidia := nvidiaWorker(now)
	stale := nvidia
	stale.WorkerID = uuid.MustParse("00000000-0000-0000-0000-0000000000c9")
	stale.LastSeen = now.Add(-2 * time.Minute)

	trace, err := ProduceDecisionTrace(workload, []PhysicalExecution{nvidia, apple, stale})
	mustf(t, err, "produce: %v")

	if len(trace.Candidates) != 2 {
		t.Fatalf("candidates=%d, want 2 eligible (stale excluded)", len(trace.Candidates))
	}
	if trace.SupplyConsidered != 3 || trace.SupplyEligible != 2 {
		t.Fatalf("supply considered=%d eligible=%d, want 3/2", trace.SupplyConsidered, trace.SupplyEligible)
	}
	if trace.Choice.SelectedWorkerID != apple.WorkerID {
		t.Fatalf("selected %s (%s), want cheaper Apple %s; fastest NVIDIA must not win",
			trace.Choice.SelectedWorkerID, trace.Choice.SelectedHWClass, apple.WorkerID)
	}
	if trace.Choice.RunnerUpWorkerID != nvidia.WorkerID {
		t.Fatalf("runner-up %s, want NVIDIA %s", trace.Choice.RunnerUpWorkerID, nvidia.WorkerID)
	}
	if trace.Choice.SelectionRule != decisionTraceSelectionRule {
		t.Fatalf("selection_rule=%q", trace.Choice.SelectionRule)
	}
	reason := trace.Choice.WinnerBeatRunnerUp
	if reason == "" {
		t.Fatal("winner_beat_runner_up is empty")
	}
	if !strings.Contains(reason, "not fastest raw device") &&
		!strings.Contains(reason, "was not the selection key") {
		t.Fatalf("reason must say completion_time was not the selection key: %s", reason)
	}
	if !strings.Contains(reason, "completion_time favored the runner-up") {
		t.Fatalf("reason must admit the runner-up was faster: %s", reason)
	}

	for _, cand := range trace.Candidates {
		assertClosedTermSet(t, cand)
		energy := termByName(cand.Terms, termEnergy)
		if energy.Status != decisionTraceTermUnknown {
			t.Fatalf("energy status=%s, want UNKNOWN", energy.Status)
		}
		if energy.Reason == "" {
			t.Fatal("energy UNKNOWN without a reason")
		}
	}

	// Match would pick the fast NVIDIA class by reputation×TPS and drop Apple.
	// Match uses time.Since, not the workload clock.
	matchApple := matchWorkerFrom(apple, workload.JobType)
	matchNvidia := matchWorkerFrom(nvidia, workload.JobType)
	matchApple.LastSeen = time.Now()
	matchNvidia.LastSeen = time.Now()
	matched, err := Match(MatchTask{
		JobType:     workload.JobType,
		MinMemoryGB: workload.MinMemoryGB,
	}, []MatchWorker{matchApple, matchNvidia})
	mustf(t, err, "Match: %v")
	if len(matched) == 0 || matched[0].ID != nvidia.WorkerID {
		t.Fatalf("fixture broken: Match should prefer NVIDIA TPS, got %+v", matched)
	}

	blob, err := EmitDecisionTrace(trace)
	mustf(t, err, "emit: %v")
	t.Logf("emitted decision trace:\n%s", blob)
	var roundtrip DecisionTrace
	if err := json.Unmarshal(blob, &roundtrip); err != nil {
		t.Fatalf("emitted JSON must decode: %v", err)
	}
	if roundtrip.Choice.SelectedWorkerID != apple.WorkerID {
		t.Fatalf("roundtrip selected %s", roundtrip.Choice.SelectedWorkerID)
	}
}

func TestDecisionTraceRecordsRealizedVersusPredictedPerTerm(t *testing.T) {
	now := time.Date(2026, 8, 24, 18, 5, 0, 0, time.UTC)
	workload := twoCandidateWorkload(now)
	apple := appleWorker(now)
	nvidia := nvidiaWorker(now)
	trace, err := ProduceDecisionTrace(workload, []PhysicalExecution{apple, nvidia})
	mustf(t, err, "produce: %v")
	if trace.Realized != nil {
		t.Fatal("pre-execution trace must not carry realized")
	}

	predictedCompletion, ok := predictedValue(trace.Candidates[0], termCompletionTime)
	if !ok {
		t.Fatal("winner completion_time should be PREDICTED in this fixture")
	}
	predictedPrice, ok := predictedValue(trace.Candidates[0], termPrice)
	if !ok {
		t.Fatal("winner price should be PREDICTED in this fixture")
	}

	realizedCompletion := predictedCompletion + 12
	realizedPrice := roundUSD(predictedPrice + 0.01)
	realizedReliability := 1.0
	realizedStartup := 1.5
	realizedModelLoad := 0.0
	realizedVerify := 0.02
	got := RealizedExecution{
		WorkerID:            apple.WorkerID,
		CompletionTimeSecs:  &realizedCompletion,
		PriceUSD:            &realizedPrice,
		Reliability:         &realizedReliability,
		StartupSecs:         &realizedStartup,
		ModelLoadMS:         &realizedModelLoad,
		VerificationCostUSD: &realizedVerify,
		// movement, memory, energy, failure_retry left unobserved
	}
	mustf(t, RecordDecisionTraceRealized(&trace, got), "record realized: %v")
	if trace.Realized == nil {
		t.Fatal("realized half missing")
	}
	realizedBlob, err := EmitDecisionTrace(trace)
	mustf(t, err, "emit realized: %v")
	t.Logf("emitted realized decision trace:\n%s", realizedBlob)
	if len(trace.Realized.Terms) != len(decisionTraceTermOrder) {
		t.Fatalf("realized terms=%d, want %d", len(trace.Realized.Terms), len(decisionTraceTermOrder))
	}

	compared := 0
	incomparable := 0
	for _, cmp := range trace.Realized.Terms {
		switch cmp.SignedErrorStatus {
		case decisionTraceCompared:
			compared++
			if cmp.SignedError == nil {
				t.Fatalf("term %s COMPARED without signed error", cmp.Term)
			}
			if cmp.Predicted.Value == nil || cmp.Realized.Value == nil {
				t.Fatalf("term %s COMPARED without both values", cmp.Term)
			}
			want := *cmp.Realized.Value - *cmp.Predicted.Value
			if math.Abs(*cmp.SignedError-want) > 1e-12 {
				t.Fatalf("term %s signed_error=%v, want realized-predicted=%v",
					cmp.Term, *cmp.SignedError, want)
			}
		case decisionTraceIncomparable:
			incomparable++
			if cmp.SignedError != nil {
				t.Fatalf("term %s INCOMPARABLE but has signed_error", cmp.Term)
			}
		default:
			t.Fatalf("term %s missing signed_error_status", cmp.Term)
		}
	}
	if compared == 0 {
		t.Fatal("expected at least one COMPARED term with a signed error")
	}
	if incomparable == 0 {
		t.Fatal("expected INCOMPARABLE terms for UNPREDICTED/UNKNOWN/unobserved pairs")
	}

	comp := comparisonByTerm(t, trace, termCompletionTime)
	if comp.SignedErrorStatus != decisionTraceCompared || comp.SignedError == nil {
		t.Fatalf("completion_time signed error: %+v", comp)
	}
	if math.Abs(*comp.SignedError-12) > 1e-9 {
		t.Fatalf("completion_time signed_error=%v, want +12 (slower than predicted)", *comp.SignedError)
	}

	priceCmp := comparisonByTerm(t, trace, termPrice)
	if priceCmp.SignedErrorStatus != decisionTraceCompared || priceCmp.SignedError == nil {
		t.Fatalf("price signed error: %+v", priceCmp)
	}
	if math.Abs(*priceCmp.SignedError-0.01) > 1e-12 {
		t.Fatalf("price signed_error=%v, want +0.01 (realized above predicted)", *priceCmp.SignedError)
	}

	energy := comparisonByTerm(t, trace, termEnergy)
	if energy.Predicted.Status != decisionTraceTermUnknown {
		t.Fatalf("energy predicted status=%s", energy.Predicted.Status)
	}
	if energy.SignedErrorStatus != decisionTraceIncomparable || energy.SignedError != nil {
		t.Fatalf("energy must be INCOMPARABLE: %+v", energy)
	}
	startup := comparisonByTerm(t, trace, termStartup)
	if startup.Predicted.Status != decisionTraceTermUnpredicted {
		t.Fatalf("startup predicted status=%s", startup.Predicted.Status)
	}
	if startup.Realized.Status != decisionTraceRealized {
		t.Fatalf("startup was observed, status=%s", startup.Realized.Status)
	}
	if startup.SignedErrorStatus != decisionTraceIncomparable {
		t.Fatalf("observed-but-unpredicted startup must be INCOMPARABLE, got %s", startup.SignedErrorStatus)
	}
}

func TestDecisionTraceSingletonExplainsWhyOnlyOneExisted(t *testing.T) {
	now := time.Date(2026, 8, 24, 18, 10, 0, 0, time.UTC)
	workload := twoCandidateWorkload(now)
	apple := appleWorker(now)
	stale := nvidiaWorker(now)
	stale.LastSeen = now.Add(-90 * time.Second)
	throttled := nvidiaWorker(now)
	throttled.WorkerID = uuid.MustParse("00000000-0000-0000-0000-0000000000c2")
	throttled.Throttled = true
	tooSmall := nvidiaWorker(now)
	tooSmall.WorkerID = uuid.MustParse("00000000-0000-0000-0000-0000000000c3")
	tooSmall.MemoryGB = 8

	trace, err := ProduceDecisionTrace(workload, []PhysicalExecution{apple, stale, throttled, tooSmall})
	mustf(t, err, "produce: %v")
	if len(trace.Candidates) != 1 {
		t.Fatalf("candidates=%d, want 1", len(trace.Candidates))
	}
	if trace.Choice.SelectedWorkerID != apple.WorkerID {
		t.Fatalf("selected %s", trace.Choice.SelectedWorkerID)
	}
	if strings.TrimSpace(trace.Choice.SingletonReason) == "" {
		t.Fatal("singleton reason missing")
	}
	if trace.Choice.WinnerBeatRunnerUp != "" {
		t.Fatalf("singleton must not invent a runner-up reason: %s", trace.Choice.WinnerBeatRunnerUp)
	}
	reason := trace.Choice.SingletonReason
	if !strings.Contains(reason, "only 1 of 4") {
		t.Fatalf("singleton reason should name 1 of 4: %s", reason)
	}
	if !strings.Contains(reason, "stale") ||
		!strings.Contains(reason, "throttled") ||
		!strings.Contains(reason, "memory") {
		t.Fatalf("singleton reason should name each exclusion: %s", reason)
	}
	if len(trace.Exclusions) != 3 {
		t.Fatalf("exclusions=%d, want 3", len(trace.Exclusions))
	}
}

func TestDecisionTraceReliabilityRetryBeatsFasterUnreliablePeer(t *testing.T) {
	now := time.Date(2026, 8, 24, 18, 15, 0, 0, time.UTC)
	workload := twoCandidateWorkload(now)
	reliable := appleWorker(now)
	reliable.TerminalAttempts = 20
	reliable.TerminalFails = 0
	reliable.TPS = 10
	unreliableFast := nvidiaWorker(now)
	// Ask is high enough that 20× retry loses, but low enough that raw price
	// (no retry) would beat Apple: 2 USD/h × 10s = 0.0056 vs Apple 0.05.
	unreliableFast.MinPayoutUSDHr = 2.00
	unreliableFast.TPS = 360
	unreliableFast.TerminalAttempts = 20
	unreliableFast.TerminalFails = 19 // delivered 1/20 → 20× retry multiplier

	trace, err := ProduceDecisionTrace(workload, []PhysicalExecution{reliable, unreliableFast})
	mustf(t, err, "produce: %v")
	if trace.Choice.SelectedWorkerID != reliable.WorkerID {
		t.Fatalf("selected %s (%s), want reliable Apple; fast+failing NVIDIA is not the verified outcome",
			trace.Choice.SelectedWorkerID, trace.Choice.SelectedHWClass)
	}
	if trace.Choice.ExpectedOutcomeBasis != expectedBasisPricePlusRetry {
		t.Fatalf("basis=%s, want %s", trace.Choice.ExpectedOutcomeBasis, expectedBasisPricePlusRetry)
	}
	if !strings.Contains(trace.Choice.WinnerBeatRunnerUp, termFailureRetryCost) {
		t.Fatalf("reason should cite failure_retry_cost: %s", trace.Choice.WinnerBeatRunnerUp)
	}
}

func TestDecisionTraceAllFailedReliabilityIsUnrankable(t *testing.T) {
	now := time.Date(2026, 8, 24, 18, 20, 0, 0, time.UTC)
	workload := twoCandidateWorkload(now)
	dead := appleWorker(now)
	dead.TerminalAttempts = 10
	dead.TerminalFails = 10
	dead.MinPayoutUSDHr = 0.01 // would win on price if we ignored certain failure
	live := nvidiaWorker(now)
	live.TerminalAttempts = 10
	live.TerminalFails = 0

	trace, err := ProduceDecisionTrace(workload, []PhysicalExecution{dead, live})
	mustf(t, err, "produce: %v")
	if trace.Choice.SelectedWorkerID != live.WorkerID {
		t.Fatalf("selected %s, want the worker with a verified outcome", trace.Choice.SelectedWorkerID)
	}
	rel := termByName(trace.Candidates[1].Terms, termReliability)
	if trace.Candidates[1].WorkerID != dead.WorkerID {
		// rank-2 should be the unrankable Apple
		for _, cand := range trace.Candidates {
			if cand.WorkerID == dead.WorkerID {
				rel = termByName(cand.Terms, termReliability)
			}
		}
	}
	if rel.Status != decisionTraceTermPredicted || rel.Value == nil || *rel.Value != 0 {
		t.Fatalf("all-failed reliability=%+v", rel)
	}
}

func TestDecisionTraceNoSupply(t *testing.T) {
	now := time.Date(2026, 8, 24, 18, 25, 0, 0, time.UTC)
	workload := twoCandidateWorkload(now)
	_, err := ProduceDecisionTrace(workload, nil)
	if !errors.Is(err, ErrNoSupply) {
		t.Fatalf("empty supply err=%v, want ErrNoSupply", err)
	}
	stale := appleWorker(now)
	stale.LastSeen = now.Add(-time.Hour)
	_, err = ProduceDecisionTrace(workload, []PhysicalExecution{stale})
	if !errors.Is(err, ErrNoSupply) {
		t.Fatalf("stale-only err=%v, want ErrNoSupply", err)
	}
}

func TestDecisionTraceEnergyStaysUnknownWhenWattsTableExists(t *testing.T) {
	now := time.Date(2026, 8, 24, 18, 30, 0, 0, time.UTC)
	workload := twoCandidateWorkload(now)
	trace, err := ProduceDecisionTrace(workload, []PhysicalExecution{appleWorker(now), nvidiaWorker(now)})
	mustf(t, err, "produce: %v")
	for _, cand := range trace.Candidates {
		if _, ok := sustainedWattsByHWClass[cand.HWClass]; !ok {
			t.Fatalf("fixture class %s missing from sustainedWattsByHWClass; test cannot prove we refused watts×time",
				cand.HWClass)
		}
		energy := termByName(cand.Terms, termEnergy)
		if energy.Status != decisionTraceTermUnknown || energy.Value != nil {
			t.Fatalf("energy modeled for %s: %+v", cand.HWClass, energy)
		}
		if !strings.Contains(energy.Reason, "watts") {
			t.Fatalf("energy reason should name the watts table refusal: %s", energy.Reason)
		}
	}
}

func TestDecisionTraceRejectsRealizedForWrongWorker(t *testing.T) {
	now := time.Date(2026, 8, 24, 18, 35, 0, 0, time.UTC)
	workload := twoCandidateWorkload(now)
	apple := appleWorker(now)
	nvidia := nvidiaWorker(now)
	trace, err := ProduceDecisionTrace(workload, []PhysicalExecution{apple, nvidia})
	mustf(t, err, "produce: %v")
	err = RecordDecisionTraceRealized(&trace, RealizedExecution{WorkerID: nvidia.WorkerID})
	if err == nil {
		t.Fatal("realized runner-up must be refused")
	}
}

func twoCandidateWorkload(now time.Time) AcceptedWorkload {
	return AcceptedWorkload{
		ID:                        uuid.MustParse("00000000-0000-0000-0000-0000000000b1"),
		JobType:                   "batch_infer",
		ModelRef:                  "llama-3.2-1b-instruct-q4",
		MinMemoryGB:               32,
		EstimatedWorkUnits:        3600,
		WorkUnit:                  "tokens",
		VerificationOverheadUSD:   0.02,
		VerificationOverheadKnown: true,
		Now:                       now,
	}
}

func appleWorker(now time.Time) PhysicalExecution {
	return PhysicalExecution{
		WorkerID:       uuid.MustParse("00000000-0000-0000-0000-0000000000a1"),
		SupplierID:     uuid.MustParse("00000000-0000-0000-0000-0000000000a2"),
		HWClass:        "apple_silicon_ultra",
		Engine:         "candle",
		MemoryGB:       192,
		MinPayoutUSDHr: 0.50,
		TPS:            10, // 3600 tokens → 360s
		LoadMS:         8000,
		WarmModel:      true,
		LastSeen:       now.Add(-5 * time.Second),
	}
}

func nvidiaWorker(now time.Time) PhysicalExecution {
	return PhysicalExecution{
		WorkerID:       uuid.MustParse("00000000-0000-0000-0000-0000000000c1"),
		SupplierID:     uuid.MustParse("00000000-0000-0000-0000-0000000000c2"),
		HWClass:        "nvidia_80gb",
		Engine:         "vllm",
		MemoryGB:       80,
		MinPayoutUSDHr: 36.00,
		TPS:            360, // 3600 tokens → 10s, 36× faster
		LoadMS:         2000,
		WarmModel:      false,
		LastSeen:       now.Add(-5 * time.Second),
	}
}

func matchWorkerFrom(exec PhysicalExecution, jobType string) MatchWorker {
	return MatchWorker{
		ID:              exec.WorkerID,
		SupplierID:      exec.SupplierID,
		HWClass:         exec.HWClass,
		Engine:          exec.Engine,
		MemoryGB:        exec.MemoryGB,
		Reputation:      1,
		TPS:             map[string]float32{jobType: exec.TPS},
		LastSeen:        exec.LastSeen,
		Warm:            exec.WarmModel,
		Throttled:       exec.Throttled,
		ThermalDegraded: exec.ThermalDegraded,
	}
}

func assertClosedTermSet(t *testing.T, cand CandidatePrediction) {
	t.Helper()
	if err := validatePredictedTerms(cand.Terms); err != nil {
		t.Fatalf("candidate %s terms: %v", cand.WorkerID, err)
	}
	for _, term := range cand.Terms {
		switch term.Status {
		case decisionTraceTermPredicted:
			if term.Value == nil || term.Source == "" {
				t.Fatalf("%s PREDICTED incomplete: %+v", term.Term, term)
			}
		case decisionTraceTermUnpredicted, decisionTraceTermUnknown:
			if strings.TrimSpace(term.Reason) == "" {
				t.Fatalf("%s %s without reason", term.Term, term.Status)
			}
		default:
			t.Fatalf("%s status %q", term.Term, term.Status)
		}
	}
}

func comparisonByTerm(t *testing.T, trace DecisionTrace, name string) TermComparison {
	t.Helper()
	if trace.Realized == nil {
		t.Fatal("no realized half")
	}
	for _, cmp := range trace.Realized.Terms {
		if cmp.Term == name {
			return cmp
		}
	}
	t.Fatalf("missing realized term %s", name)
	return TermComparison{}
}
