package main

import (
	"strings"
	"testing"
)

// Rank-and-freeze-1: when several cells serve one model, admission freezes exactly
// one winner rather than N (which would hand selection to ORDER BY cell_id LIMIT 1).
func TestRankAndFreezeAdmissionKeepsExactlyOneCandidate(t *testing.T) {
	// Real advertised catalogue is currently a singleton per model: ranking is a
	// no-op and ordinary admission still freezes exactly one candidate.
	decision, err := buildWorkloadDecision(embedSubmit(), strings.Repeat("c", 64))
	must(t, err)
	if len(decision.RuntimeCandidates) != 1 {
		t.Fatalf("ordinary admission froze %d candidates, want 1", len(decision.RuntimeCandidates))
	}
	if decision.RuntimeCandidates[0].CellID != candleEmbedCell {
		t.Fatalf("froze %q, want the advertised candle cell", decision.RuntimeCandidates[0].CellID)
	}
}

// Wire-kind: a kind no advertised cell serves is refused; the single advertised
// kind is still accepted and filled when omitted.
func TestWireKindAcceptsAnyAdvertisedKindAndDoesNotInventOne(t *testing.T) {
	got, err := normalizeAdvertisedRuntimeModelRef("embed", ModelRef{Ref: "all-minilm-l6-v2"})
	must(t, err)
	if got.Kind != "hf" {
		t.Fatalf("omitted kind with singleton catalogue = %q, want hf", got.Kind)
	}
	got, err = normalizeAdvertisedRuntimeModelRef("embed", ModelRef{Kind: "hf", Ref: "all-minilm-l6-v2"})
	must(t, err)
	if got.Kind != "hf" {
		t.Fatalf("explicit hf = %+v", got)
	}
	_, err = normalizeAdvertisedRuntimeModelRef("embed", ModelRef{Kind: "gguf", Ref: "all-minilm-l6-v2"})
	if err == nil || !strings.Contains(err.Error(), `model.kind="gguf"`) {
		t.Fatalf("unadvertised gguf error=%v", err)
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
func TestSelectorRegretScoresWhenChallengerIsMeasured(t *testing.T) {
	// Same supplier USD/unit on both cells: the structural per-model pricing
	// finding. scoreDecisionRegret still returns ok=true (a scored decision).
	candle := MeasuredCellCost{
		CellID: candleEmbedCell, Samples: minCellCostSamples, Measured: true,
		SupplierUSDPerUnit: 0.194,
	}
	llama := MeasuredCellCost{
		CellID: llamaEmbedCell, Samples: minCellCostSamples, Measured: true,
		SupplierUSDPerUnit: 0.194,
	}
	costMap := map[string]MeasuredCellCost{
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
	regret, cheapest, ok := scoreDecisionRegret(d, costMap)
	if !ok {
		t.Fatalf("scoreDecisionRegret not ok with two measured cells at equal price")
	}
	if regret != 0 {
		t.Fatalf("equal per-model price should score regret 0, got %v cheapest=%q", regret, cheapest)
	}
	t.Logf("scored decision regret=%.9f cheapest=%q — ScoredDecisions can leave zero", regret, cheapest)
}
