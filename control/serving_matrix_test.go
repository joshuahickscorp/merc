package main

import (
	"strings"
	"testing"
	"time"
)

// DRAFT CUDA challengers are visible and declared, and must not be routable.
// A definition, schema row or standalone benchmark never confers routability.
func TestUnprovenCUDAChallengerCellsAreNotRoutable(t *testing.T) {
	want := []struct {
		runtime, cell, engine string
	}{
		{"sglang_cuda", "sglang-cuda-llama1-infer", "sglang"},
		{"tensorrt_llm_cuda", "tensorrt-llm-cuda-llama1-infer", "tensorrt_llm"},
		{"lmdeploy_cuda", "lmdeploy-cuda-llama1-infer", "lmdeploy"},
		{"vllm_cuda", "vllm-cuda-llama1-infer", "vllm"},
	}
	for _, tc := range want {
		profile, ok := runtimeProfileByID(tc.runtime)
		if !ok {
			t.Fatalf("runtime %q is not registered", tc.runtime)
		}
		if profile.Lifecycle != runtimeLifecycleDraft {
			t.Errorf("%s lifecycle=%s, want DRAFT (unproven)", tc.runtime, profile.Lifecycle)
		}
		if profile.Engine != tc.engine {
			t.Errorf("%s engine=%s, want %s", tc.runtime, profile.Engine, tc.engine)
		}
		var cell authorityCell
		found := false
		for _, c := range profile.Cells {
			if c.ID == tc.cell {
				cell = c
				found = true
				break
			}
		}
		if !found {
			t.Fatalf("runtime %q has no cell %q", tc.runtime, tc.cell)
		}
		if cell.Routable(profile) {
			t.Errorf("cell %q is routable while unproven; a definition must not grant live placement", tc.cell)
		}
		if cell.ReachableByDirectedRouting(profile) {
			t.Errorf("cell %q is reachable by directed routing at DRAFT; DRAFT is below VALIDATED", tc.cell)
		}
		// Must not appear in the advertised buyer projection.
		for _, cap := range advertisedRuntimeCapabilities() {
			if cap.ID == tc.cell {
				t.Errorf("unproven cell %q reached the advertised projection", tc.cell)
			}
		}
	}
	// Engine registry carries the three new adapters so admission can name them.
	for _, engine := range []string{"sglang", "tensorrt_llm", "lmdeploy"} {
		if !validEngines[engine] {
			t.Errorf("engine %q is not in validEngines derived from the authority registry", engine)
		}
	}
}

func TestCUDAChallengerEnginesAdmitOnlyOnNVIDIA(t *testing.T) {
	for _, engine := range []string{"sglang", "tensorrt_llm", "lmdeploy", "vllm"} {
		if !EngineAdmissibleFor(engine, "nvidia_80gb") {
			t.Errorf("%s refused on nvidia_80gb", engine)
		}
		if EngineAdmissibleFor(engine, "apple_silicon_ultra") {
			t.Errorf("%s admitted on apple_silicon_ultra", engine)
		}
	}
	// Unknown spelling stays refused.
	if EngineAdmissibleFor("tensorrt", "nvidia_80gb") {
		t.Fatal("bare 'tensorrt' (not tensorrt_llm) must not be admitted")
	}
}

// Same model name, different artifact hash → comparison refused.
func TestServingMatrixRefusesMismatchedModelDigests(t *testing.T) {
	digestA := "3f5a22426976ab26cfe84dba63c1d08391717abb1af893e10f1b2968d862dcc1"
	digestB := strings.Repeat("ab", 32)
	arms := []ServingMatrixArm{
		{Engine: "llama_cpp", ModelID: "llama-3.2-1b-instruct-q4", ModelDigest: digestA, Precision: "Q4_K_M"},
		{Engine: "vllm", ModelID: "llama-3.2-1b-instruct-q4", ModelDigest: digestB, Precision: "BF16"},
	}
	refusals := RefuseMismatchedModelDigests(arms)
	if len(refusals) == 0 {
		t.Fatal("mismatched digests produced no comparison refusal")
	}
	joined := strings.Join(refusals, "; ")
	if !strings.Contains(joined, "digest mismatch") {
		t.Fatalf("refusal did not name digest mismatch: %s", joined)
	}

	// Matching digests compare cleanly even across engines.
	arms[1].ModelDigest = digestA
	if got := RefuseMismatchedModelDigests(arms); len(got) != 0 {
		t.Fatalf("matching digests refused: %v", got)
	}

	// Missing / non-hex digests refuse rather than "probably the same model".
	arms[1].ModelDigest = ""
	if got := RefuseMismatchedModelDigests(arms); len(got) == 0 {
		t.Fatal("empty digest was accepted as comparable")
	}
}

// Fewer than 5× concurrency prompts is a burst, not steady state.
func TestServingMatrixRefusesSubSteadyStatePromptCount(t *testing.T) {
	if reason := RefuseSubSteadyState(32, 100); reason == "" {
		t.Fatal("100 prompts at concurrency 32 was accepted; need 160")
	} else if !strings.Contains(reason, "incomparable") || !strings.Contains(reason, "5×") {
		t.Fatalf("refusal phrasing unexpected: %s", reason)
	}
	if reason := RefuseSubSteadyState(32, 160); reason != "" {
		t.Fatalf("exactly 5× was refused: %s", reason)
	}
	if reason := RefuseSubSteadyState(1, 5); reason != "" {
		t.Fatalf("concurrency 1 with 5 prompts was refused: %s", reason)
	}
	if RequiredPromptCount(8) != 40 {
		t.Fatalf("RequiredPromptCount(8)=%d, want 40", RequiredPromptCount(8))
	}
}

// The gate must fail when only the cheapest concurrency level is measured but
// higher levels were claimed. Checking only c=1 is the defect shipped in
// scripts/gateway-parity.py evaluate_budget.
func TestServingMatrixGateEvaluatesEveryClaimedConcurrencyLevel(t *testing.T) {
	arm := ServingMatrixArm{
		Engine: "llama_cpp", ModelDigest: strings.Repeat("cd", 32), Precision: "Q4_K_M",
	}
	key := ArmKey(arm)
	ttft := 10.0
	rps := 5.0
	// Only concurrency=1 measured; 8 and 32 claimed but absent.
	cells := []ServingMatrixCellResult{{
		ArmKey: key,
		Point:  ServingMatrixPoint{Concurrency: 1, PromptTokens: 32, OutputTokens: 16, State: "cold", Lane: "interactive", Precision: "Q4_K_M"},
		Status: "MEASURED",
		Metrics: &ServingMatrixMetrics{
			TTFTp95Ms: &ttft, ReqPerSec: &rps, RequestsOK: 5, RequestsAttempted: 5,
		},
	}}
	budget := ServingMatrixBudget{RequireMeasuredAtEveryLevel: true}
	gate := EvaluateServingMatrixGate([]int{1, 8, 32}, cells, key, budget)
	if gate.GatePassed {
		t.Fatal("gate_passed=true when only concurrency=1 was measured; multi-level claim must fail")
	}
	if len(gate.Levels) != 3 {
		t.Fatalf("gate recorded %d levels, want 3 (one per claimed concurrency)", len(gate.Levels))
	}
	if !gate.Levels[0].Passed {
		t.Fatalf("concurrency 1 should pass: %+v", gate.Levels[0])
	}
	for _, level := range gate.Levels[1:] {
		if level.Passed {
			t.Fatalf("concurrency %d passed with no measurement", level.Concurrency)
		}
		if len(level.Refusals) == 0 {
			t.Fatalf("concurrency %d produced no refusal", level.Concurrency)
		}
	}

	// Measuring every claimed level clears the gate.
	for _, c := range []int{8, 32} {
		cells = append(cells, ServingMatrixCellResult{
			ArmKey: key,
			Point:  ServingMatrixPoint{Concurrency: c, PromptTokens: 32, OutputTokens: 16, State: "cold", Lane: "interactive", Precision: "Q4_K_M"},
			Status: "MEASURED",
			Metrics: &ServingMatrixMetrics{
				TTFTp95Ms: &ttft, ReqPerSec: &rps, RequestsOK: RequiredPromptCount(c), RequestsAttempted: RequiredPromptCount(c),
			},
		})
	}
	gateOK := EvaluateServingMatrixGate([]int{1, 8, 32}, cells, key, budget)
	if !gateOK.GatePassed {
		t.Fatalf("gate failed with every level measured: %+v", gateOK)
	}
}

// Unsupported precision / prefix-hit / context is REFUSED with a reason, never
// silently skipped.
func TestServingMatrixRefusesUnsupportedPoints(t *testing.T) {
	arm := ServingMatrixArm{
		Engine:            "llama_cpp",
		Precision:         "Q4_K_M",
		SupportsPrefixHit: false,
		MaxContextTokens:  4096,
	}
	if reason := RefuseMatrixPoint(arm, ServingMatrixPoint{Precision: "FP8", Concurrency: 1, PromptTokens: 32, OutputTokens: 16, State: "cold", Lane: "interactive"}); reason == "" || !strings.Contains(reason, "FP8") {
		t.Fatalf("FP8 on Q4_K_M arm was not refused: %q", reason)
	}
	if reason := RefuseMatrixPoint(arm, ServingMatrixPoint{Precision: "Q4_K_M", Concurrency: 1, PromptTokens: 32, OutputTokens: 16, State: "prefix-hit", Lane: "interactive"}); reason == "" || !strings.Contains(reason, "prefix") {
		t.Fatalf("prefix-hit without support was not refused: %q", reason)
	}
	if reason := RefuseMatrixPoint(arm, ServingMatrixPoint{Precision: "Q4_K_M", Concurrency: 1, PromptTokens: 8000, OutputTokens: 100, State: "cold", Lane: "interactive"}); reason == "" || !strings.Contains(reason, "context") {
		t.Fatalf("oversize context was not refused: %q", reason)
	}
	// Supported point is not refused.
	if reason := RefuseMatrixPoint(arm, ServingMatrixPoint{Precision: "Q4_K_M", Concurrency: 1, PromptTokens: 32, OutputTokens: 16, State: "cold", Lane: "interactive"}); reason != "" {
		t.Fatalf("supported point refused: %s", reason)
	}
}

// Default subset documents every drop; silent truncation is coverage fraud.
func TestServingMatrixDefaultSubsetDocumentsDrops(t *testing.T) {
	sel := DefaultServingMatrixSelection("Q4_K_M")
	if len(sel.Selected) == 0 {
		t.Fatal("default selection is empty")
	}
	if len(sel.DroppedAxes) == 0 {
		t.Fatal("default selection dropped no axes; full matrix is enormous and must not be silently claimed")
	}
	if len(sel.DroppedSummary) == 0 {
		t.Fatal("dropped_summary is empty")
	}
	full := FullServingMatrixPoints()
	if len(full) < 1000 {
		t.Fatalf("full matrix has only %d points; axes look truncated", len(full))
	}
	for _, p := range sel.Selected {
		if p.Precision != "Q4_K_M" {
			t.Fatalf("selected point has precision %s, want arm precision", p.Precision)
		}
		if p.Concurrency == 128 {
			t.Fatal("concurrency 128 reached default selection")
		}
	}
	foundDrop128 := false
	for _, d := range sel.DroppedAxes {
		if d.Axis == "concurrency" {
			for _, v := range d.Values {
				if v == "128" && d.Reason != "" {
					foundDrop128 = true
				}
			}
		}
	}
	if !foundDrop128 {
		t.Fatal("concurrency 128 was not recorded as a dropped axis value with a reason")
	}
}

// Artifact assembly preserves refusals and will not set comparable on digest mismatch.
func TestServingMatrixArtifactRefusesIncomparableArms(t *testing.T) {
	digestA := "3f5a22426976ab26cfe84dba63c1d08391717abb1af893e10f1b2968d862dcc1"
	digestB := strings.Repeat("11", 32)
	arms := []ServingMatrixArm{
		{Engine: "llama_cpp", ModelDigest: digestA, Precision: "Q4_K_M"},
		{Engine: "sglang", ModelDigest: digestB, Precision: "BF16"},
	}
	sel := ServingMatrixSelection{Selected: []ServingMatrixPoint{{
		Concurrency: 1, PromptTokens: 32, OutputTokens: 16,
		State: "cold", Lane: "interactive", Precision: "Q4_K_M",
	}}}
	art := BuildServingMatrixArtifact(arms, sel, nil,
		ServingMatrixBudget{}, time.Date(2026, 8, 2, 0, 0, 0, 0, time.UTC), nil)
	if art.Comparable {
		t.Fatal("artifact marked comparable with mismatched digests")
	}
	if art.Gate.GatePassed {
		t.Fatal("gate_passed with incomparable arms")
	}
	if art.BenchmarkStatus != "INCOMPARABLE_ARMS" {
		t.Fatalf("status=%s, want INCOMPARABLE_ARMS", art.BenchmarkStatus)
	}
}

// Prompt corpus length is exactly the steady-state floor.
func TestServingMatrixPromptCorpusIsSteadyState(t *testing.T) {
	point := ServingMatrixPoint{Concurrency: 8, PromptTokens: 32, OutputTokens: 16}
	corpus := ServingMatrixPromptCorpus(point)
	if len(corpus) != 40 {
		t.Fatalf("corpus size %d, want 40", len(corpus))
	}
	if reason := RefuseSubSteadyState(point.Concurrency, len(corpus)); reason != "" {
		t.Fatalf("corpus failed its own steady-state rule: %s", reason)
	}
}
