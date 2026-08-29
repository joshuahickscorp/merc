package main

import (
	"strings"
	"testing"
)

// Rank-and-freeze-1: when several cells serve one model, admission freezes exactly
// one winner rather than N (which would hand selection to ORDER BY cell_id LIMIT 1).
func TestRankAndFreezeAdmissionKeepsExactlyOneCandidate(t *testing.T) {
	// Production currently advertises no bindable cells. Install the explicit
	// in-memory TEST_ONLY combined-token lane so this test remains about the
	// rank-and-freeze mechanic, not about the production evidence census.
	decision, err := buildWorkloadDecision(testOnlyCombinedTokenSubmit(t), strings.Repeat("c", 64))
	must(t, err)
	if len(decision.RuntimeCandidates) != 1 {
		t.Fatalf("ordinary admission froze %d candidates, want 1", len(decision.RuntimeCandidates))
	}
	if decision.RuntimeCandidates[0].CellID != "candle-metal-llama1-infer" {
		t.Fatalf("froze %q, want the TEST_ONLY candle batch cell", decision.RuntimeCandidates[0].CellID)
	}
}

// Wire-kind: a kind no scoped advertised cell serves is refused; the single
// TEST_ONLY kind is still accepted and filled when omitted.
func TestWireKindAcceptsAnyAdvertisedKindAndDoesNotInventOne(t *testing.T) {
	installTestOnlyCombinedTokenAuthority(t)
	got, err := normalizeAdvertisedRuntimeModelRef("batch_infer", ModelRef{Ref: "llama-3.2-1b-instruct-q4"})
	must(t, err)
	if got.Kind != "gguf" {
		t.Fatalf("omitted kind with singleton TEST_ONLY catalogue = %q, want gguf", got.Kind)
	}
	got, err = normalizeAdvertisedRuntimeModelRef("batch_infer", ModelRef{Kind: "gguf", Ref: "llama-3.2-1b-instruct-q4"})
	must(t, err)
	if got.Kind != "gguf" {
		t.Fatalf("explicit gguf = %+v", got)
	}
	_, err = normalizeAdvertisedRuntimeModelRef("batch_infer", ModelRef{Kind: "hf", Ref: "llama-3.2-1b-instruct-q4"})
	if err == nil || !strings.Contains(err.Error(), `model.kind="hf"`) {
		t.Fatalf("unadvertised hf error=%v", err)
	}
}

// chooseShadowCell on the freeze path: prefer higher lifecycle, freeze one.
func TestChooseShadowCellRanksThenPicksOne(t *testing.T) {
	considered := []shadowCandidate{
		{CellID: "low", Lifecycle: runtimeLifecycleRealRuntimeProven, QualityTier: "OUTCOME_EQUIVALENT"},
		{CellID: "high", Lifecycle: runtimeLifecycleActive, QualityTier: "OUTCOME_EQUIVALENT"},
	}
	got := chooseShadowCell(considered, "")
	if got != "high" {
		t.Fatalf("chooseShadowCell=%q, want high (ACTIVE over REAL_RUNTIME_PROVEN)", got)
	}
}

// ScoredDecisions becomes non-zero once a shadow decision has two measured cells.
// Directed routing is what accumulates challenger samples through the money path;
// this unit proves the scoring arm itself is not pinned at zero when that
// evidence exists. Equal per-model prices yield zero *mean regret* — that is a
// correct scored result, not an unscored one.
func TestSelectorLiabilityRegretScoresWhenChallengerIsMeasured(t *testing.T) {
	// Same supplier USD/unit on both cells: the structural per-model pricing
	// finding. scoreDecisionLiabilityRegret still returns ok=true (a scored decision).
	candle := MeasuredSupplierLiabilityProxy{
		CellID: candleEmbedCell, Samples: minSupplierLiabilitySamples, Measured: true,
		SupplierUSDPerUnit:  0.194,
		VerificationSamples: minSupplierLiabilitySamples,
		TerminalAttempts:    minSupplierLiabilitySamples,
	}
	llama := MeasuredSupplierLiabilityProxy{
		CellID: llamaEmbedCell, Samples: minSupplierLiabilitySamples, Measured: true,
		SupplierUSDPerUnit:  0.194,
		VerificationSamples: minSupplierLiabilitySamples,
		TerminalAttempts:    minSupplierLiabilitySamples,
	}
	costMap := map[string]MeasuredSupplierLiabilityProxy{
		candleEmbedCell: candle,
		llamaEmbedCell:  llama,
	}
	d := shadowDecisionRow{
		JobType:    "embed",
		ModelRef:   "all-minilm-l6-v2",
		RoutedCell: candleEmbedCell,
		ShadowCell: llamaEmbedCell,
		Considered: []string{candleEmbedCell, llamaEmbedCell},
	}
	regret, cheapest, ok := scoreDecisionLiabilityRegret(d, costMap)
	if !ok {
		t.Fatalf("scoreDecisionLiabilityRegret not ok with two measured cells at equal price")
	}
	if regret != 0 {
		t.Fatalf("equal per-model price should score regret 0, got %v cheapest=%q", regret, cheapest)
	}
	t.Logf("scored decision regret=%.9f cheapest=%q — ScoredDecisions can leave zero", regret, cheapest)
}
