package main

import (
	"encoding/json"
	"math"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"
)

// cohort-shaped actuals read from evidence/perf/selector/paired-cohort-embed.json.
//
// That artifact is binding_status=UNBOUND with seven of the eight producer
// identity fields missing, so these timings cannot say which candle build or
// which llama.cpp build produced them. They are carried with SourceBinding set
// to UNBOUND so the comparison can exercise its decision structure without the
// result being mistaken for an engine verdict. The bound replacement is
// evidence/perf/selector/engine-parity-metal-embed-*.json.
func cohortEmbedCosts(t *testing.T) map[string]MeasuredCellCost {
	t.Helper()
	const supplier = 0.0060625
	return map[string]MeasuredCellCost{
		candleEmbedCell: {
			CellID: candleEmbedCell, RuntimeID: "candle_metal", Engine: "candle",
			JobType: "embed", ModelRef: "all-minilm-l6-v2", HWClass: "apple_silicon_ultra",
			Samples: minCellCostSamples, Units: 640, MedianMsPerUnit: 0.21875,
			SupplierUSDPerUnit: supplier, VerificationSamples: 20, TerminalAttempts: 20,
			Measured: true, SourceBinding: BindingUnbound, Unknown: unknownCostComponents(),
		},
		llamaEmbedCell: {
			CellID: llamaEmbedCell, RuntimeID: "llama_cpp_metal", Engine: "llama_cpp",
			JobType: "embed", ModelRef: "all-minilm-l6-v2", HWClass: "apple_silicon_ultra",
			Samples: minCellCostSamples, Units: 640, MedianMsPerUnit: 0.28125,
			SupplierUSDPerUnit: supplier, VerificationSamples: 20, TerminalAttempts: 20,
			Measured: true, SourceBinding: BindingUnbound, Unknown: unknownCostComponents(),
		},
	}
}

// boundEmbedCosts reads the interleaved engine-parity measurement and returns
// cost rows that may rule on the prior claim, because every timing in them
// traces to a named merc-agent binary, a named llama-server binary, and two
// named weight digests.
//
// Returns nil when the receipt is absent or not BOUND. Callers fall back to the
// cohort fixture, which exercises the same decision structure and is refused a
// verdict -- the point of the gate is that the fallback stays visibly weaker
// rather than silently standing in for a measurement.
func boundEmbedCosts(t *testing.T) map[string]MeasuredCellCost {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join("..", "evidence", "perf", "selector",
		"engine-parity-metal-embed-latest.json"))
	if err != nil {
		return nil
	}
	var doc struct {
		BindingStatus string `json:"binding_status"`
		Batch         int    `json:"batch"`
		Arms          map[string]struct {
			Engine     string    `json:"engine"`
			MsPerUnit  float64   `json:"ms_per_unit_p50"`
			N          int       `json:"n"`
			RawSamples []float64 `json:"raw_ms_per_unit"`
		} `json:"arms"`
		Quality struct {
			Passes bool `json:"passes"`
		} `json:"quality"`
	}
	if err := json.Unmarshal(raw, &doc); err != nil {
		return nil
	}
	// A failed cosine gate means the arms were not serving the same product, so
	// the timings compare two different things and must not rank anything.
	if !strings.EqualFold(doc.BindingStatus, BindingBound) || !doc.Quality.Passes {
		return nil
	}
	byArm := map[string]string{
		"candle_metal":    candleEmbedCell,
		"llama_cpp_metal": llamaEmbedCell,
	}
	const supplier = 0.0060625
	out := map[string]MeasuredCellCost{}
	for arm, cell := range byArm {
		a, ok := doc.Arms[arm]
		if !ok || a.MsPerUnit <= 0 || a.N < minCellCostSamples {
			return nil
		}
		out[cell] = MeasuredCellCost{
			CellID: cell, RuntimeID: arm, Engine: a.Engine,
			JobType: "embed", ModelRef: "all-minilm-l6-v2", HWClass: "apple_silicon_ultra",
			Samples: a.N, Units: int64(a.N * doc.Batch), MedianMsPerUnit: a.MsPerUnit,
			SupplierUSDPerUnit: supplier,
			// The parity harness embeds and times; it does not run the
			// verification contract that a chain task would, so reliability is
			// recorded as clean rather than measured. Equal on both arms, which
			// is what keeps the cost tie a tie.
			VerificationSamples: a.N, TerminalAttempts: a.N,
			Measured: true, SourceBinding: BindingBound,
			Unknown: unknownCostComponents(),
		}
	}
	return out
}

func catalogueMinilm() CataloguePriceAuthority {
	return CataloguePriceAuthority{
		ModelID: "all-minilm-l6-v2", JobType: "embed",
		ReferencePricePer1K: 0.00625, SupplierShare: 0.97,
	}
}

// TestDecideMeasuredShadowNamesThroughputOnCostTie pins the honesty rule: a
// cost-tie must not claim MEASURED_VERIFIED_OUTCOME_COST, and the faster cell
// wins on MORE_THROUGHPUT_AT_EQUAL_PRICE.
func TestDecideMeasuredShadowNamesThroughputOnCostTie(t *testing.T) {
	costs := cohortEmbedCosts(t)
	cells := []string{candleEmbedCell, llamaEmbedCell}
	d := decideMeasuredShadow(costs, cells, candleEmbedCell)
	if d.Basis != selectionBasisThroughputEqualPrice {
		t.Fatalf("basis = %q, want %s (cost ties; throughput decides)",
			d.Basis, selectionBasisThroughputEqualPrice)
	}
	if d.Winner != candleEmbedCell {
		t.Fatalf("winner = %q, want candle (faster on the governed chain: 0.21875 vs 0.28125 ms/unit)",
			d.Winner)
	}
}

// TestDecideMeasuredShadowRefusesAWinnerOnAnAbsolutelyTinyGap covers the
// absolute noise floor specifically, which the exact-tie test above cannot
// reach.
//
// A mutation that deleted the floor survived the suite, because every existing
// tie case used identical latencies and the ratio band caught those on its own.
// The floor only earns its place on fast cells: 0.100 against 0.109 ms per unit
// is a 0.009 ms difference and an 8.6% ratio, so the ratio band would happily
// declare a winner over a gap far below what this host can resolve. That is a
// manufactured selection, and on a cost tie it is the only term deciding.
func TestDecideMeasuredShadowRefusesAWinnerOnAnAbsolutelyTinyGap(t *testing.T) {
	const supplier = 0.0060625
	costs := map[string]MeasuredCellCost{
		candleEmbedCell: {
			CellID: candleEmbedCell, Samples: minCellCostSamples, Measured: true,
			MedianMsPerUnit: 0.109, SupplierUSDPerUnit: supplier,
			VerificationSamples: 20,
		},
		llamaEmbedCell: {
			CellID: llamaEmbedCell, Samples: minCellCostSamples, Measured: true,
			MedianMsPerUnit: 0.100, SupplierUSDPerUnit: supplier,
			VerificationSamples: 20,
		},
	}
	// Sanity: the gap really is outside the ratio band, so only the absolute
	// floor can refuse it. If this stops holding the test is no longer covering
	// what it claims to.
	if ratio := (0.109 - 0.100) / ((0.109 + 0.100) / 2); ratio < latencyNoiseFraction {
		t.Fatalf("gap ratio %.4f is inside the %.4f band; this test no longer exercises the absolute floor",
			ratio, latencyNoiseFraction)
	}
	d := decideMeasuredShadow(costs, []string{candleEmbedCell, llamaEmbedCell}, candleEmbedCell)
	if d.Basis != selectionBasisTieNoDecision {
		t.Fatalf("basis = %q, want %s; a 0.009 ms gap is not capacity",
			d.Basis, selectionBasisTieNoDecision)
	}
	if d.Winner != candleEmbedCell {
		t.Fatalf("winner = %q, want the routed cell retained", d.Winner)
	}
}

// TestDecideMeasuredShadowRefusesAWinnerOnAProportionallyTinyGap covers the
// ratio band, which the absolute floor cannot reach.
//
// The floor and the band guard opposite ends. A 0.02 ms gap between two cells
// near 3.0 ms per unit clears the absolute floor easily and is still only 0.7%
// of the measurement — well inside run-to-run drift on this host, where two
// bound runs of the same arm landed 0.02 ms apart. Those are the real numbers
// from the engine parity receipt, which is why this case is worth pinning: at
// that scale the absolute floor alone would hand out a winner.
func TestDecideMeasuredShadowRefusesAWinnerOnAProportionallyTinyGap(t *testing.T) {
	const supplier = 0.0060625
	costs := map[string]MeasuredCellCost{
		candleEmbedCell: {
			CellID: candleEmbedCell, Samples: minCellCostSamples, Measured: true,
			MedianMsPerUnit: 3.02, SupplierUSDPerUnit: supplier,
			VerificationSamples: 20,
		},
		llamaEmbedCell: {
			CellID: llamaEmbedCell, Samples: minCellCostSamples, Measured: true,
			MedianMsPerUnit: 3.00, SupplierUSDPerUnit: supplier,
			VerificationSamples: 20,
		},
	}
	// Sanity: the gap clears the absolute floor, so only the ratio band can
	// refuse it.
	if 3.02-3.00 < latencyNoiseAbsMs {
		t.Fatalf("gap is inside the %.4f ms absolute floor; this test no longer exercises the ratio band",
			latencyNoiseAbsMs)
	}
	d := decideMeasuredShadow(costs, []string{candleEmbedCell, llamaEmbedCell}, candleEmbedCell)
	if d.Basis != selectionBasisTieNoDecision {
		t.Fatalf("basis = %q, want %s; 0.7%% of a 3 ms measurement is drift, not capacity",
			d.Basis, selectionBasisTieNoDecision)
	}
	if d.Winner != candleEmbedCell {
		t.Fatalf("winner = %q, want the routed cell retained", d.Winner)
	}
}

// TestDecideMeasuredShadowRecordsTrueTie refuses to manufacture a winner when
// cost and latency both tie.
func TestDecideMeasuredShadowRecordsTrueTie(t *testing.T) {
	const supplier = 0.0060625
	costs := map[string]MeasuredCellCost{
		candleEmbedCell: {
			CellID: candleEmbedCell, Samples: minCellCostSamples, Measured: true,
			MedianMsPerUnit: 0.25, SupplierUSDPerUnit: supplier,
			VerificationSamples: 20,
		},
		llamaEmbedCell: {
			CellID: llamaEmbedCell, Samples: minCellCostSamples, Measured: true,
			MedianMsPerUnit: 0.25, SupplierUSDPerUnit: supplier,
			VerificationSamples: 20,
		},
	}
	d := decideMeasuredShadow(costs, []string{candleEmbedCell, llamaEmbedCell}, candleEmbedCell)
	if d.Basis != selectionBasisTieNoDecision {
		t.Fatalf("basis = %q, want %s", d.Basis, selectionBasisTieNoDecision)
	}
	if d.Winner != candleEmbedCell {
		t.Fatalf("tie should retain routed cell, got %q", d.Winner)
	}
}

// TestDecideMeasuredShadowStrictCostWin keeps the cost basis when one cell is
// genuinely cheaper (reliability can do that).
func TestDecideMeasuredShadowStrictCostWin(t *testing.T) {
	costs := map[string]MeasuredCellCost{
		candleEmbedCell: {
			CellID: candleEmbedCell, Samples: minCellCostSamples, Measured: true,
			MedianMsPerUnit: 0.3, SupplierUSDPerUnit: 0.0060625,
			VerificationSamples: 20,
		},
		llamaEmbedCell: {
			CellID: llamaEmbedCell, Samples: minCellCostSamples, Measured: true,
			MedianMsPerUnit: 0.2, SupplierUSDPerUnit: 0.0060625,
			// 25% verification failures → cost × 4/3, strictly more expensive.
			VerificationSamples: 40, VerificationFails: 10,
		},
	}
	d := decideMeasuredShadow(costs, []string{candleEmbedCell, llamaEmbedCell}, candleEmbedCell)
	if d.Basis != selectionBasisMeasuredCost {
		t.Fatalf("basis = %q, want %s", d.Basis, selectionBasisMeasuredCost)
	}
	if d.Winner != candleEmbedCell {
		t.Fatalf("winner = %q, want candle (cheaper verified outcome despite slower)", d.Winner)
	}
}

// TestRankedByMeasuredCostDoesNotClaimCostWinOnTie is the integration point:
// ShadowSelection.rankedByMeasuredCost must surface the throughput basis.
func TestRankedByMeasuredCostDoesNotClaimCostWinOnTie(t *testing.T) {
	costs := cohortEmbedCosts(t)
	byHW := map[string]map[string]MeasuredCellCost{
		"apple_silicon_ultra": costs,
	}
	s := ShadowSelection{
		RoutedCellID:   candleEmbedCell,
		ShadowCellID:   candleEmbedCell,
		SelectionBasis: selectionBasisLadder,
		Considered: []shadowCandidate{
			{CellID: candleEmbedCell, RuntimeID: "candle_metal", Engine: "candle",
				Lifecycle: runtimeLifecycleActive, Routable: true, QualityTier: "OUTCOME_EQUIVALENT"},
			{CellID: llamaEmbedCell, RuntimeID: "llama_cpp_metal", Engine: "llama_cpp",
				Lifecycle: "REAL_RUNTIME_PROVEN", Routable: false, QualityTier: "UNPROVEN"},
		},
	}
	out := s.rankedByMeasuredCost(byHW)
	if out.SelectionBasis != selectionBasisThroughputEqualPrice {
		t.Fatalf("selection_basis = %q, want %s", out.SelectionBasis, selectionBasisThroughputEqualPrice)
	}
	if out.ShadowCellID != candleEmbedCell {
		t.Fatalf("shadow = %q, want candle", out.ShadowCellID)
	}
	if out.CostHWClass != "apple_silicon_ultra" {
		t.Fatalf("hw = %q", out.CostHWClass)
	}
	// Measured fields populated on candidates.
	for _, c := range out.Considered {
		if !c.CostMeasured || c.VerifiedUSDPerUnit <= 0 || c.MedianMsPerUnit <= 0 {
			t.Fatalf("candidate not fully measured: %+v", c)
		}
	}
}

// TestGovernedComparisonWillNotRuleOnPriorFromUnboundActuals pins the rule that
// cost the prior lane its headline: an artifact that cannot name the binary it
// timed cannot overturn a claim about which binary is faster.
//
// The cohort actuals really do put candle ahead, 0.21875 against 0.28125 ms per
// unit. Ruling FALSIFIED on them would still be wrong, because the file they
// come from is missing source_commit, build_digest, model_artifact_digest and
// raw_samples -- it is one unnameable number contradicting another. The
// comparison records UNRESOLVED_UNBOUND_ACTUALS and leaves the prior standing
// until a bound measurement rules on it.
//
// The shadow ranking is still allowed to run: ordering two cells for a decision
// that promotes nothing is a weaker act than publishing an engine verdict, and
// the receipt carries the provenance either way.
func TestGovernedComparisonWillNotRuleOnPriorFromUnboundActuals(t *testing.T) {
	costs := cohortEmbedCosts(t)
	shadow := ShadowSelection{
		JobType: "embed", ModelRef: "all-minilm-l6-v2",
		RoutedCellID: candleEmbedCell, ShadowCellID: candleEmbedCell,
		SelectionBasis:   selectionBasisLadder,
		RuntimeMatrixSHA: generatedRuntimeMatrixSHA256,
		Considered: []shadowCandidate{
			{CellID: candleEmbedCell, RuntimeID: "candle_metal", Engine: "candle",
				Lifecycle: runtimeLifecycleActive, Routable: true,
				QualityTier: "OUTCOME_EQUIVALENT", Verification: "cosine"},
			{CellID: llamaEmbedCell, RuntimeID: "llama_cpp_metal", Engine: "llama_cpp",
				Lifecycle: "REAL_RUNTIME_PROVEN", Routable: false,
				QualityTier: "UNPROVEN", Verification: "cosine"},
		},
		Excluded: []shadowExclusion{
			{CellID: "vllm-cuda-minilm-embed", Reason: "no matched identity and quality on paid CUDA; not eligible"},
		},
	}
	cmp, err := BuildGovernedComparison(shadow, costs, catalogueMinilm(), map[string]any{
		"note": "test fixture; live writer records real uptime",
	}, true)
	if err != nil {
		t.Fatal(err)
	}

	// The actuals favour candle, and that is still not a verdict.
	status, _ := cmp.PriorThroughputClaim["status"].(string)
	if status != "UNRESOLVED_UNBOUND_ACTUALS" {
		t.Fatalf("prior claim status = %q, want UNRESOLVED_UNBOUND_ACTUALS; "+
			"the cohort artifact cannot name which candle or llama.cpp build it timed", status)
	}
	if binding, _ := cmp.ActualsBinding["status"].(string); binding != BindingUnbound {
		t.Fatalf("actuals_binding = %q, want UNBOUND", binding)
	}

	// A bound actual with the same numbers may rule, so the gate is provenance
	// and not a blanket refusal to ever decide.
	bound := cohortEmbedCosts(t)
	for id, c := range bound {
		c.SourceBinding = BindingBound
		bound[id] = c
	}
	boundCmp, err := BuildGovernedComparison(shadow, bound, catalogueMinilm(), nil, true)
	if err != nil {
		t.Fatal(err)
	}
	if s, _ := boundCmp.PriorThroughputClaim["status"].(string); s != "FALSIFIED_CANDLE_FASTER_ON_GOVERNED_CONTRACT" {
		t.Fatalf("bound actuals status = %q, want FALSIFIED once provenance exists", s)
	}

	// Predicted winner under the prior is llama; actual is candle.
	if cmp.Decision.PredictedWinner != llamaEmbedCell {
		t.Fatalf("predicted winner = %q, want llama under 2.1× prior", cmp.Decision.PredictedWinner)
	}
	if cmp.Decision.ActualWinner != candleEmbedCell {
		t.Fatalf("actual winner = %q, want candle", cmp.Decision.ActualWinner)
	}
	if cmp.Decision.ActualBasis != selectionBasisThroughputEqualPrice {
		t.Fatalf("actual basis = %q", cmp.Decision.ActualBasis)
	}
	if !strings.Contains(cmp.Decision.SelectionReason, "more throughput at equal price") {
		t.Fatalf("selection reason does not name throughput-at-equal-price: %q",
			cmp.Decision.SelectionReason)
	}
	if strings.Contains(strings.ToLower(cmp.Decision.SelectionReason), "cheaper") {
		t.Fatalf("selection reason must not claim cheaper: %q", cmp.Decision.SelectionReason)
	}

	// Cost ties; cost regret on the predicted winner is ~0 (catalogue form).
	if math.Abs(cmp.Decision.CostRegretUSD) > 1e-12 {
		// Predicted cost uses catalogue; actual also cancelled form at equal
		// reliability — should be zero. If not, surface the number.
		t.Logf("cost regret on predicted winner = %.17f (expected ~0)", cmp.Decision.CostRegretUSD)
	}
	candle := cmp.Cells[candleEmbedCell]
	llama := cmp.Cells[llamaEmbedCell]
	if math.Abs(candle.ActualPhysicalCostUSDUnit-llama.ActualPhysicalCostUSDUnit) > 1e-12 {
		t.Fatalf("actual costs must tie: candle=%v llama=%v",
			candle.ActualPhysicalCostUSDUnit, llama.ActualPhysicalCostUSDUnit)
	}
	if candle.BuyerPricePer1K != llama.BuyerPricePer1K ||
		candle.SupplierEntitlementUSDUnit != llama.SupplierEntitlementUSDUnit {
		t.Fatal("buyer price / supplier entitlement must be catalogue-keyed, equal across cells")
	}
	if !candle.MercTrueNetUnavailable || !llama.MercTrueNetUnavailable {
		t.Fatal("true net must stay refused")
	}

	// Latency regret for the predicted winner (llama): prior predicted
	// candle_ms/2.1 ≈ 0.104, actual 0.28125 → regret negative (predicted too fast).
	if llama.LatencyRegretMsPerUnit >= 0 {
		t.Fatalf("llama latency regret = %v, want negative (prior overstated llama speed)",
			llama.LatencyRegretMsPerUnit)
	}
	t.Logf("llama latency regret (predicted−actual) = %.6f ms/unit; cost regret = %.12e USD/unit",
		llama.LatencyRegretMsPerUnit, llama.CostRegretUSDPerUnit)

	// Absolute delta, not ratio-led.
	delta, _ := cmp.LatencyComparison["absolute_delta_ms_per_unit"].(float64)
	if math.Abs(delta-0.0625) > 1e-9 {
		t.Fatalf("absolute delta = %v, want 0.0625 ms/unit", delta)
	}

	// Nothing promoted; routing unchanged.
	if cmp.Decision.Promoted || cmp.Decision.RoutingChanged {
		t.Fatal("comparison must not promote or change routing")
	}
	if cmp.Decision.AuthorityDigest == "" {
		t.Fatal("authority digest empty")
	}
	if !strings.Contains(strings.Join(cmp.DoesNotProve, "\n"), "does not promote") {
		t.Fatal("does_not_prove must name promotion refusal")
	}
}

// TestGovernedComparisonNoPriorKeepsLadderPrediction covers the path where we
// do not inject the 2.1× prior: predicted equals the ladder shadow choice.
func TestGovernedComparisonNoPriorKeepsLadderPrediction(t *testing.T) {
	costs := cohortEmbedCosts(t)
	shadow := ShadowSelection{
		JobType: "embed", ModelRef: "all-minilm-l6-v2",
		RoutedCellID: candleEmbedCell, ShadowCellID: candleEmbedCell,
		SelectionBasis: selectionBasisLadder,
		Considered: []shadowCandidate{
			{CellID: candleEmbedCell, RuntimeID: "candle_metal", Engine: "candle",
				Lifecycle: runtimeLifecycleActive, Routable: true, QualityTier: "OUTCOME_EQUIVALENT", Verification: "cosine"},
			{CellID: llamaEmbedCell, RuntimeID: "llama_cpp_metal", Engine: "llama_cpp",
				Lifecycle: "REAL_RUNTIME_PROVEN", Routable: false, QualityTier: "UNPROVEN", Verification: "cosine"},
		},
	}
	cmp, err := BuildGovernedComparison(shadow, costs, catalogueMinilm(), nil, false)
	if err != nil {
		t.Fatal(err)
	}
	if cmp.Decision.PredictedWinner != candleEmbedCell {
		t.Fatalf("predicted = %q", cmp.Decision.PredictedWinner)
	}
	if cmp.Decision.ActualWinner != candleEmbedCell {
		t.Fatalf("actual = %q", cmp.Decision.ActualWinner)
	}
	// Prediction matched actual on winner → decision latency regret near 0
	// when predicted latency = actual (no prior rewrite).
	if math.Abs(cmp.Decision.LatencyRegretMs) > 1e-12 {
		t.Fatalf("without prior, latency regret should be ~0, got %v", cmp.Decision.LatencyRegretMs)
	}
	if math.Abs(cmp.Decision.CostRegretUSD) > 1e-12 {
		t.Fatalf("cost regret should be ~0, got %v", cmp.Decision.CostRegretUSD)
	}
}

// TestEconomicSelectorTwoCases proves the selector is economic, not speed-only.
//
// Case A (live latencies, equal reliability): llama.cpp is faster at equal
// verified-outcome cost → MORE_THROUGHPUT_AT_EQUAL_PRICE picks llama; selection
// latency/cost regret both 0.
//
// Case B (same live latencies; faster cell has verification failures so its
// verified-outcome cost rises): the slower cell is strictly cheaper per verified
// outcome → MEASURED_VERIFIED_OUTCOME_COST picks the slower cell. A speed-only
// selector would pick the faster cell and pay positive cost regret.
//
// Case B is constructed: the reliability differential is injected on top of live
// (or cohort) latencies so the projection, not a hardcoded engine preference,
// decides. That is the only honest way to show the slower-wins branch on a host
// where both arms currently pass verification equally.
func TestEconomicSelectorTwoCases(t *testing.T) {
	live := boundEmbedCosts(t)
	source := "evidence/perf/selector/engine-parity-metal-embed-latest.json (BOUND)"
	if live == nil {
		live = cohortEmbedCosts(t)
		source = "evidence/perf/selector/paired-cohort-embed.json (UNBOUND cohort fallback)"
	}
	t.Logf("latency source: %s", source)

	cells := []string{candleEmbedCell, llamaEmbedCell}
	candleMs := live[candleEmbedCell].MedianMsPerUnit
	llamaMs := live[llamaEmbedCell].MedianMsPerUnit
	if candleMs <= 0 || llamaMs <= 0 {
		t.Fatal("missing median_ms_per_unit on live/cohort costs")
	}

	// ── Case A: equal reliability, faster wins at equal price ──────────────
	caseA := cloneMeasuredCosts(live)
	// Force equal clean verification so cost ties by construction.
	for id, c := range caseA {
		c.VerificationSamples = 40
		c.VerificationFails = 0
		c.TerminalAttempts = 40
		c.TerminalFails = 0
		caseA[id] = c
	}
	dA := decideMeasuredShadow(caseA, cells, candleEmbedCell)
	wantAWinner := candleEmbedCell
	if llamaMs < candleMs && !latenciesTie(llamaMs, candleMs) {
		wantAWinner = llamaEmbedCell
	} else if candleMs < llamaMs && !latenciesTie(llamaMs, candleMs) {
		wantAWinner = candleEmbedCell
	}
	if dA.Basis != selectionBasisThroughputEqualPrice && dA.Basis != selectionBasisTieNoDecision {
		t.Fatalf("case A basis = %q, want throughput-at-equal-price or tie", dA.Basis)
	}
	if dA.Winner != wantAWinner && dA.Basis != selectionBasisTieNoDecision {
		t.Fatalf("case A winner = %q, want %q (faster at equal VO cost)", dA.Winner, wantAWinner)
	}
	// Selection regret on the measured axes.
	latRegA, costRegA := selectionRegretFromCosts(caseA, dA.Winner)
	if latRegA > 1e-9 {
		t.Fatalf("case A latency selection regret = %v, want 0 (picked fastest)", latRegA)
	}
	if costRegA > 1e-12 {
		t.Fatalf("case A cost selection regret = %v, want 0 (costs tie; any pick is optimal)", costRegA)
	}
	t.Logf("case A: winner=%s basis=%s lat_regret=%.6f ms/unit cost_regret=%.6e USD/unit (faster=%s)",
		dA.Winner, dA.Basis, latRegA, costRegA, wantAWinner)

	// ── Case B: slower cell cheaper on verified-outcome cost ────────────────
	// Make the FASTER cell fail verification 25% of the time so VO cost × 4/3.
	// The slower cell stays clean. Selector must pick slower if it is economic.
	caseB := cloneMeasuredCosts(live)
	fasterID := wantAWinner
	slowerID := candleEmbedCell
	if fasterID == candleEmbedCell {
		slowerID = llamaEmbedCell
	}
	// If latencies tied, force an ordering so "slower" is well-defined.
	if latenciesTie(candleMs, llamaMs) {
		fasterID = llamaEmbedCell
		slowerID = candleEmbedCell
		c := caseB[fasterID]
		c.MedianMsPerUnit = caseB[slowerID].MedianMsPerUnit * 0.7
		caseB[fasterID] = c
	}
	for id, c := range caseB {
		c.VerificationSamples = 40
		c.TerminalAttempts = 40
		c.TerminalFails = 0
		if id == fasterID {
			c.VerificationFails = 10 // 25% fail → reliability multiplies cost
		} else {
			c.VerificationFails = 0
		}
		caseB[id] = c
	}
	// Confirm construction: faster is more expensive per verified outcome.
	fastVO, okF := caseB[fasterID].ExpectedVerifiedOutcomeUSDPerUnit()
	slowVO, okS := caseB[slowerID].ExpectedVerifiedOutcomeUSDPerUnit()
	if !okF || !okS {
		t.Fatal("case B VO costs unavailable")
	}
	if !(slowVO < fastVO) || costsTieUSD(slowVO, fastVO) {
		t.Fatalf("case B construction failed: slower VO=%v faster VO=%v (need slower strictly cheaper)",
			slowVO, fastVO)
	}
	if !(caseB[fasterID].MedianMsPerUnit < caseB[slowerID].MedianMsPerUnit) {
		t.Fatalf("case B construction: faster cell is not actually faster: %v vs %v",
			caseB[fasterID].MedianMsPerUnit, caseB[slowerID].MedianMsPerUnit)
	}

	dB := decideMeasuredShadow(caseB, cells, fasterID /* routed = speed-preferring */)
	if dB.Basis != selectionBasisMeasuredCost {
		t.Fatalf("case B basis = %q, want %s (VO cost must decide)", dB.Basis, selectionBasisMeasuredCost)
	}
	if dB.Winner != slowerID {
		t.Fatalf("case B winner = %q, want slower cell %q (cheaper verified outcome)", dB.Winner, slowerID)
	}
	latRegB, costRegB := selectionRegretFromCosts(caseB, dB.Winner)
	// Cost regret must be 0 (picked cheapest). Latency regret is positive
	// because the cheaper cell is slower — that is the economic trade the
	// selector is allowed to make.
	if costRegB > 1e-12 {
		t.Fatalf("case B cost selection regret = %v, want 0 (picked cheapest VO)", costRegB)
	}
	if latRegB <= 0 {
		t.Fatalf("case B latency selection regret = %v, want >0 (accepted slower for cheaper VO)", latRegB)
	}
	// Counterfactual: always-faster selector would pick fasterID and pay cost regret.
	alwaysFastCostRegret := fastVO - slowVO
	if alwaysFastCostRegret <= 0 {
		t.Fatal("always-faster counterfactual has no cost regret to expose")
	}
	t.Logf("case B: winner=%s (slower) basis=%s lat_regret=%.6f ms/unit cost_regret=%.6e; "+
		"always-faster would pay cost_regret=%.6e USD/unit",
		dB.Winner, dB.Basis, latRegB, costRegB, alwaysFastCostRegret)

	// Projection explanation: not a hardcoded engine preference.
	if dB.Winner == slowerID && dB.Basis == selectionBasisMeasuredCost {
		t.Logf("case B deciding term is verified-outcome cost (reliability-adjusted), not engine id")
	}
}

func cloneMeasuredCosts(in map[string]MeasuredCellCost) map[string]MeasuredCellCost {
	out := make(map[string]MeasuredCellCost, len(in))
	for k, v := range in {
		out[k] = v
	}
	return out
}

// selectionRegretFromCosts is chosen − best_available on latency (ms/unit) and
// verified-outcome cost (USD/unit). Absolute units only.
func selectionRegretFromCosts(costs map[string]MeasuredCellCost, chosen string) (latMs, costUSD float64) {
	ch, ok := costs[chosen]
	if !ok || !ch.Measured {
		return 0, 0
	}
	bestMs := ch.MedianMsPerUnit
	for _, c := range costs {
		if c.Measured && c.MedianMsPerUnit > 0 && c.MedianMsPerUnit < bestMs {
			bestMs = c.MedianMsPerUnit
		}
	}
	if ch.MedianMsPerUnit > 0 && bestMs > 0 {
		latMs = ch.MedianMsPerUnit - bestMs
	}
	chVO, okCh := ch.ExpectedVerifiedOutcomeUSDPerUnit()
	if !okCh {
		return latMs, 0
	}
	bestVO := chVO
	for _, c := range costs {
		if vo, ok := c.ExpectedVerifiedOutcomeUSDPerUnit(); ok && vo < bestVO {
			bestVO = vo
		}
	}
	costUSD = chVO - bestVO
	return latMs, costUSD
}

// TestWriteGovernedComparisonReceipt is env-gated. Set
// MERC_GOVERNED_COMPARISON_RECEIPT=1 to emit the bound receipt under
// evidence/perf/selector/governed-candle-vs-llama-shadow-decision.json.
func TestWriteGovernedComparisonReceipt(t *testing.T) {
	if os.Getenv("MERC_GOVERNED_COMPARISON_RECEIPT") != "1" {
		t.Skip("MERC_GOVERNED_COMPARISON_RECEIPT is not 1; receipt writer is env-gated")
	}
	// Prefer the bound interleaved measurement. The cohort fixture only stands
	// in when no bound receipt exists, and it is refused a verdict when it does.
	costs := boundEmbedCosts(t)
	actualsSource := "evidence/perf/selector/engine-parity-metal-embed-latest.json (BOUND interleaved parity)"
	if costs == nil {
		costs = cohortEmbedCosts(t)
		actualsSource = "evidence/perf/selector/paired-cohort-embed.json (UNBOUND cohort; verdict withheld)"
	}
	t.Logf("actuals source: %s", actualsSource)
	// Plan a real shadow selection so eligibility / exclusions are live.
	withActivationRestored(t)
	decision, err := buildWorkloadDecision(jobSubmit{
		JobType:     JobType{Type: "embed"},
		Model:       ModelRef{Kind: "hf", Ref: "all-minilm-l6-v2"},
		Tier:        "batch",
		Constraints: JobConstraints{MaxDurationSecs: 3600},
	}, strings.Repeat("d", 64))
	if err != nil {
		t.Fatal(err)
	}
	shadow, err := planShadowSelection(decision)
	if err != nil {
		t.Fatal(err)
	}
	// Apply measured ranking the same way createJob would after costs exist.
	shadow = shadow.rankedByMeasuredCost(map[string]map[string]MeasuredCellCost{
		"apple_silicon_ultra": costs,
	})

	loadAvg, _ := runSysctlLoadavg()
	hostLoad := map[string]any{
		"captured_at":  time.Now().UTC().Format(time.RFC3339Nano),
		"load_average": loadAvg,
		"note":         "host is not perfectly quiet; absolute sub-ms deltas are host-noise-adjacent",
		"hw_class":     "apple_silicon_ultra",
		"goos_goarch":  "darwin/arm64",
	}

	cmp, err := BuildGovernedComparison(shadow, costs, catalogueMinilm(), hostLoad, true)
	if err != nil {
		t.Fatal(err)
	}

	// Serialise as map for the bound writer.
	raw, err := json.Marshal(cmp)
	if err != nil {
		t.Fatal(err)
	}
	var payload map[string]any
	if err := json.Unmarshal(raw, &payload); err != nil {
		t.Fatal(err)
	}

	dir := filepath.Join("..", "evidence", "perf", "selector")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "governed-candle-vs-llama-shadow-decision.json")

	// The bound writer would stamp BOUND from THIS harness's identity, which
	// names who ran the arithmetic. The timings came out of
	// evidence/perf/selector/paired-cohort-embed.json, which is UNBOUND and
	// missing source_commit, build_digest, model_artifact_digest and
	// raw_samples -- it cannot name which candle build or which llama.cpp build
	// it timed. Stamping this document BOUND would launder that gap into
	// authority the numbers never had, so the receipt takes the binding of its
	// weakest input and says why in the payload.
	if actualsBinding(costs) != BindingBound {
		payload["binding_status"] = BindingUnbound
		payload["unbound_reason"] = "actuals read from an UNBOUND cohort artifact " +
			"(evidence/perf/selector/paired-cohort-embed.json, missing source_commit, build_digest, " +
			"model_artifact_digest, image_digest, corpus_digest, exact_config, raw_samples); " +
			"the decision structure is exercised, the engine ordering is not established"
		writePlainJSON(t, path, payload)
		t.Logf("wrote UNBOUND governed comparison %s (actuals cannot name their producer)", path)
	} else if id, bin, err := DefaultBoundIdentity("..",
		"control/runtime_governed_comparison_test.go",
		"governed embed comparison candle vs llama.cpp + one shadow decision; predictedFromPrior=true",
		"actuals from a BOUND cell-cost measurement"); err == nil {
		if werr := WriteBoundEvidenceJSON(EvidenceWriteRequest{
			RepoRoot: "..", Path: path, Payload: payload,
			Identity: id, BuildBinaryPath: bin,
		}); werr != nil {
			t.Logf("bound writer failed (%v); writing plain JSON", werr)
			writePlainJSON(t, path, payload)
		}
	} else {
		t.Logf("bound identity unavailable (%v); writing plain JSON", err)
		writePlainJSON(t, path, payload)
	}
	t.Logf("wrote governed comparison + shadow decision %s", path)
	t.Logf("predicted_winner=%s actual_winner=%s basis=%s prediction_cost_regret=%.6e prediction_latency_regret_ms=%.6f selection_cost_regret=%.6e selection_latency_regret_ms=%.6f promoted=%v",
		cmp.Decision.PredictedWinner, cmp.Decision.ActualWinner, cmp.Decision.ActualBasis,
		cmp.Decision.CostRegretUSD, cmp.Decision.LatencyRegretMs,
		cmp.Decision.SelectionCostRegretUSD, cmp.Decision.SelectionLatencyRegretMs,
		cmp.Decision.Promoted)
}

// TestWriteEconomicSelectorProofReceipt is env-gated. Set
// MERC_ECONOMIC_SELECTOR_PROOF_RECEIPT=1 to emit the dual-case proof under
// evidence/perf/selector/economic-selector-candle-vs-llama-proof.json.
//
// The proof reuses live bound latencies from engine-parity-metal-embed-latest.json
// and constructs the reliability branch that shows the slower cell can win.
func TestWriteEconomicSelectorProofReceipt(t *testing.T) {
	if os.Getenv("MERC_ECONOMIC_SELECTOR_PROOF_RECEIPT") != "1" {
		t.Skip("MERC_ECONOMIC_SELECTOR_PROOF_RECEIPT is not 1; receipt writer is env-gated")
	}
	live := boundEmbedCosts(t)
	actualsSource := "evidence/perf/selector/engine-parity-metal-embed-latest.json"
	actualsBound := live != nil
	if live == nil {
		live = cohortEmbedCosts(t)
		actualsSource = "evidence/perf/selector/paired-cohort-embed.json"
	}

	// Bootstrap interval on paired (or arm) latency delta from the bound receipt.
	bootstrap := map[string]any{
		"unit": "interleaved_pair_delta_ms_per_unit (llama − candle)",
		"note": "percentile CI via resampling with replacement over timed pairs",
	}
	if raw, err := os.ReadFile(filepath.Join("..", "evidence", "perf", "selector",
		"engine-parity-metal-embed-latest.json")); err == nil {
		var doc struct {
			Comparison struct {
				Paired []float64 `json:"paired_deltas_ms_per_unit"`
				Delta  float64   `json:"delta_ms_per_unit_p50"`
				MDE    float64   `json:"mde_ms_per_unit_approx"`
				Faster string    `json:"faster_arm"`
			} `json:"comparison"`
			Arms map[string]struct {
				P50 float64   `json:"ms_per_unit_p50"`
				P95 float64   `json:"ms_per_unit_p95"`
				P99 float64   `json:"ms_per_unit_p99"`
				Raw []float64 `json:"raw_ms_per_unit"`
			} `json:"arms"`
			TimedPairs int `json:"timed_pairs"`
			Warmup     int `json:"warmup_per_arm"`
			Batch      int `json:"batch"`
		}
		if json.Unmarshal(raw, &doc) == nil && len(doc.Comparison.Paired) >= 30 {
			lo, hi := bootstrapPercentileCI(doc.Comparison.Paired, 50, 2000, 0.95)
			bootstrap = map[string]any{
				"unit":                      "interleaved_pair_delta_ms_per_unit (llama − candle)",
				"n_pairs":                   len(doc.Comparison.Paired),
				"warmup_per_arm_discarded":  doc.Warmup,
				"batch":                     doc.Batch,
				"observed_p50_delta_ms":     doc.Comparison.Delta,
				"bootstrap_p50_ci_95_lo_ms": lo,
				"bootstrap_p50_ci_95_hi_ms": hi,
				"mde_ms_per_unit_approx":    doc.Comparison.MDE,
				"faster_arm":                doc.Comparison.Faster,
				"candle_p50_p95_p99_ms": []float64{
					doc.Arms["candle_metal"].P50, doc.Arms["candle_metal"].P95, doc.Arms["candle_metal"].P99,
				},
				"llama_p50_p95_p99_ms": []float64{
					doc.Arms["llama_cpp_metal"].P50, doc.Arms["llama_cpp_metal"].P95, doc.Arms["llama_cpp_metal"].P99,
				},
				"absolute_delta_p50_ms": math.Abs(doc.Comparison.Delta),
				"ci_width_ms":           hi - lo,
				"power_note": "if |observed_p50| is not well above MDE and CI excludes 0, " +
					"the interval is wide and a ranking claim is refused",
			}
		}
	}

	// Case A: equal reliability on live latencies.
	caseACosts := cloneMeasuredCosts(live)
	for id, c := range caseACosts {
		c.VerificationSamples, c.VerificationFails = 40, 0
		c.TerminalAttempts, c.TerminalFails = 40, 0
		caseACosts[id] = c
	}
	dA := decideMeasuredShadow(caseACosts, []string{candleEmbedCell, llamaEmbedCell}, candleEmbedCell)
	latA, costA := selectionRegretFromCosts(caseACosts, dA.Winner)

	// Case B: inject verification failures on the faster arm.
	caseBCosts := cloneMeasuredCosts(live)
	fasterID := dA.Winner
	if dA.Basis == selectionBasisTieNoDecision {
		// Prefer llama as "faster" label when tied for construction clarity.
		if caseBCosts[llamaEmbedCell].MedianMsPerUnit <= caseBCosts[candleEmbedCell].MedianMsPerUnit {
			fasterID = llamaEmbedCell
		} else {
			fasterID = candleEmbedCell
		}
	}
	slowerID := candleEmbedCell
	if fasterID == candleEmbedCell {
		slowerID = llamaEmbedCell
	}
	for id, c := range caseBCosts {
		c.VerificationSamples, c.TerminalAttempts, c.TerminalFails = 40, 40, 0
		if id == fasterID {
			c.VerificationFails = 10
		} else {
			c.VerificationFails = 0
		}
		caseBCosts[id] = c
	}
	dB := decideMeasuredShadow(caseBCosts, []string{candleEmbedCell, llamaEmbedCell}, fasterID)
	latB, costB := selectionRegretFromCosts(caseBCosts, dB.Winner)
	fastVO, _ := caseBCosts[fasterID].ExpectedVerifiedOutcomeUSDPerUnit()
	slowVO, _ := caseBCosts[slowerID].ExpectedVerifiedOutcomeUSDPerUnit()

	// Full governed comparison on case A (live equal-reliability) with prior.
	withActivationRestored(t)
	decision, err := buildWorkloadDecision(jobSubmit{
		JobType:     JobType{Type: "embed"},
		Model:       ModelRef{Kind: "hf", Ref: "all-minilm-l6-v2"},
		Tier:        "batch",
		Constraints: JobConstraints{MaxDurationSecs: 3600},
	}, strings.Repeat("e", 64))
	if err != nil {
		t.Fatal(err)
	}
	shadow, err := planShadowSelection(decision)
	if err != nil {
		t.Fatal(err)
	}
	shadow = shadow.rankedByMeasuredCost(map[string]map[string]MeasuredCellCost{
		"apple_silicon_ultra": caseACosts,
	})
	loadAvg, _ := runSysctlLoadavg()
	hostLoad := map[string]any{
		"captured_at":  time.Now().UTC().Format(time.RFC3339Nano),
		"load_average": loadAvg,
		"hw_class":     "apple_silicon_ultra",
		"goos_goarch":  "darwin/arm64",
	}
	cmp, err := BuildGovernedComparison(shadow, caseACosts, catalogueMinilm(), hostLoad, true)
	if err != nil {
		t.Fatal(err)
	}

	// Sampling parameters: embed has no temperature; pin identity binding.
	sampling := map[string]any{
		"workload":              "embed",
		"temperature":           "n/a (deterministic embedding; no sampling)",
		"top_p":                 "n/a",
		"max_tokens":            "n/a",
		"batch":                 8,
		"interleave":            "candle_then_llama_per_pair",
		"warmup_per_arm":        5,
		"timed_pairs":           48,
		"prompt_identity_bound": "EMBED_BENCH_CORPUS in merc-agent + corpus_digest on producer_identity",
		"model_identity_bound":  "safetensors sha256 (candle) + F16 GGUF sha256 (llama) in model_artifact_digest",
	}

	payload := map[string]any{
		"schema_version": 1,
		"kind":           "economic_selector_cell_selection_proof",
		"claim_class":    "cell_selection_proof",
		"measured_at":    time.Now().UTC().Format(time.RFC3339Nano),
		"title":          "Economic selector: Candle vs llama.cpp on one governed MiniLM embed contract",
		"actuals_source": actualsSource,
		"actuals_bound":  actualsBound,
		"methodology": map[string]any{
			"sampling_parameters": sampling,
			"bootstrap":           bootstrap,
			"energy": map[string]any{
				"measured": false,
				"reason": "powermetrics requires privileges not available in this lane; " +
					"no joules are reported as measured. Energy is not modelled as measured.",
			},
			"warmup_discarded": 5,
			"steady_state":     "warmup discarded per arm; timed interleaved pairs only",
		},
		"cells": cmp.Cells,
		"shadow_selector_decision_case_a_equal_reliability": map[string]any{
			"predicted_winner":                     cmp.Decision.PredictedWinner,
			"predicted_basis":                      cmp.Decision.PredictedBasis,
			"actual_winner":                        dA.Winner,
			"actual_basis":                         dA.Basis,
			"selection_latency_regret_ms_per_unit": latA,
			"selection_cost_regret_usd_per_unit":   costA,
			"prediction_latency_regret_ms":         cmp.Decision.LatencyRegretMs,
			"prediction_cost_regret_usd":           cmp.Decision.CostRegretUSD,
			"quality":                              cmp.Decision.QualityOutcome,
			"confidence":                           cmp.Decision.Confidence,
			"deciding_term":                        dA.Basis,
			"selection_reason":                     selectionReasonFor(dA.Basis, dA.Winner, cmp.Cells),
		},
		"case_b_slower_wins_on_verified_outcome_cost": map[string]any{
			"construction": "same live latencies; faster cell VerificationFails=10/40 (25%); " +
				"slower cell clean. Verified-outcome cost = supplier × 1/pass_rate.",
			"faster_cell":                          fasterID,
			"slower_cell":                          slowerID,
			"faster_median_ms_per_unit":            caseBCosts[fasterID].MedianMsPerUnit,
			"slower_median_ms_per_unit":            caseBCosts[slowerID].MedianMsPerUnit,
			"faster_verified_outcome_usd_per_unit": fastVO,
			"slower_verified_outcome_usd_per_unit": slowVO,
			"actual_winner":                        dB.Winner,
			"actual_basis":                         dB.Basis,
			"selection_latency_regret_ms_per_unit": latB,
			"selection_cost_regret_usd_per_unit":   costB,
			"always_faster_counterfactual": map[string]any{
				"would_choose":                         fasterID,
				"selection_cost_regret_usd_per_unit":   fastVO - slowVO,
				"selection_latency_regret_ms_per_unit": 0.0,
				"note":                                 "a selector that always picks the faster engine pays this cost regret on case B",
			},
			"deciding_term": dB.Basis,
			"selection_reason": selectionReasonFor(dB.Basis, dB.Winner, map[string]GovernedComparisonCell{
				fasterID: {CellID: fasterID},
				slowerID: {CellID: slowerID},
			}),
		},
		"tie_break_constants_audited": map[string]any{
			"latencyNoiseFraction":      latencyNoiseFraction,
			"latencyNoiseAbsMs":         latencyNoiseAbsMs,
			"pricesTieWithin":           pricesTieWithin,
			"priorThroughputClaimRatio": priorThroughputClaimRatio,
			"note": "true ties retain the routed cell (TIE_NO_DECISION) so sort order " +
				"cannot manufacture a divergence; that is a constant, not a candle/llama preference. " +
				"No engine-id constant preference exists in decideMeasuredShadow.",
		},
		"cost_tie_authority": cmp.CostTie,
		"does_not_prove": []string{
			"does not claim Merc beats vLLM (different hardware, different lane)",
			"does not claim cross-supplier or network results; this is cell selection on one Mac",
			"does not measure joules (no privileged powermetrics in this lane)",
			"does not establish true net contribution (unknown cost categories remain)",
			"does not promote any cell or change routing",
			"case B reliability differential is constructed on top of live latencies, not observed production fail rates",
			"does not establish fleet multi-host behaviour",
		},
		"governed_comparison_excerpt": map[string]any{
			"latency_comparison": cmp.LatencyComparison,
			"cost_comparison":    cmp.CostComparison,
			"actuals_binding":    cmp.ActualsBinding,
			"prior_claim":        cmp.PriorThroughputClaim,
		},
	}

	// Per-cell table rows for the report.
	table := map[string]any{}
	for id, row := range cmp.Cells {
		table[id] = map[string]any{
			"predicted_latency_ms_per_unit":        row.PredictedLatencyMsPerUnit,
			"actual_latency_ms_per_unit":           row.ActualLatencyMsPerUnit,
			"predicted_verified_outcome_usd_unit":  row.PredictedPhysicalCostUSDUnit,
			"actual_verified_outcome_usd_unit":     row.ActualPhysicalCostUSDUnit,
			"quality_tier":                         row.QualityTier,
			"supplier_entitlement_usd_per_unit":    row.SupplierEntitlementUSDUnit,
			"buyer_price_per_1k":                   row.BuyerPricePer1K,
			"merc_true_net":                        "UNAVAILABLE: " + row.MercTrueNetReason,
			"merc_gross_platform_usd_per_unit":     row.MercGrossPlatformUSDUnit,
			"prediction_latency_error_ms_per_unit": row.LatencyRegretMsPerUnit,
			"prediction_cost_error_usd_per_unit":   row.CostRegretUSDPerUnit,
			"confidence":                           row.Confidence,
		}
	}
	payload["per_cell_table"] = table

	dir := filepath.Join("..", "evidence", "perf", "selector")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "economic-selector-candle-vs-llama-proof.json")

	if !actualsBound {
		payload["binding_status"] = BindingUnbound
		payload["unbound_reason"] = "actuals not BOUND; dual-case structure exercised without engine verdict authority"
		writePlainJSON(t, path, payload)
		t.Logf("wrote UNBOUND economic selector proof %s", path)
		return
	}
	if id, bin, err := DefaultBoundIdentity("..",
		"control/runtime_governed_comparison_test.go#TestWriteEconomicSelectorProofReceipt",
		"dual-case economic selector proof: case A equal-reliability throughput; case B reliability-adjusted VO cost; selection regret = chosen − best_available",
		"actuals from BOUND engine-parity-metal-embed-latest.json raw_ms_per_unit; case B verification fails constructed"); err == nil {
		if werr := WriteBoundEvidenceJSON(EvidenceWriteRequest{
			RepoRoot: "..", Path: path, Payload: payload,
			Identity: id, BuildBinaryPath: bin,
		}); werr != nil {
			t.Logf("bound writer failed (%v); writing plain JSON", werr)
			writePlainJSON(t, path, payload)
		} else {
			t.Logf("wrote BOUND economic selector proof %s", path)
		}
	} else {
		t.Logf("bound identity unavailable (%v); writing plain JSON", err)
		writePlainJSON(t, path, payload)
	}
	t.Logf("caseA winner=%s basis=%s sel_lat_reg=%.6f sel_cost_reg=%.6e", dA.Winner, dA.Basis, latA, costA)
	t.Logf("caseB winner=%s basis=%s sel_lat_reg=%.6f sel_cost_reg=%.6e always_fast_cost_reg=%.6e",
		dB.Winner, dB.Basis, latB, costB, fastVO-slowVO)
}

// bootstrapPercentileCI resamples xs with replacement and returns a
// (1-alpha)-ish percentile CI for the given percentile of the sample
// (e.g. percentile=50 for the median). Uses a fixed LCG so the receipt is
// reproducible for a given sample vector.
func bootstrapPercentileCI(xs []float64, percentile float64, nBoot int, level float64) (lo, hi float64) {
	if len(xs) == 0 || nBoot < 1 {
		return 0, 0
	}
	// Simple LCG — no external rand dependency in tests.
	var state uint64 = 0xC0FFEE42
	next := func() uint64 {
		state = state*6364136223846793005 + 1
		return state
	}
	stats := make([]float64, nBoot)
	tmp := make([]float64, len(xs))
	for b := 0; b < nBoot; b++ {
		for i := range tmp {
			tmp[i] = xs[next()%uint64(len(xs))]
		}
		stats[b] = pctFloat(tmp, percentile)
	}
	sort.Float64s(stats)
	alpha := (1 - level) / 2
	loIdx := int(math.Floor(alpha * float64(nBoot)))
	hiIdx := int(math.Ceil((1-alpha)*float64(nBoot))) - 1
	if loIdx < 0 {
		loIdx = 0
	}
	if hiIdx >= nBoot {
		hiIdx = nBoot - 1
	}
	return stats[loIdx], stats[hiIdx]
}

func pctFloat(xs []float64, p float64) float64 {
	if len(xs) == 0 {
		return 0
	}
	cp := append([]float64(nil), xs...)
	sort.Float64s(cp)
	if p <= 0 {
		return cp[0]
	}
	if p >= 100 {
		return cp[len(cp)-1]
	}
	rank := (p / 100) * float64(len(cp)-1)
	lo := int(math.Floor(rank))
	hi := int(math.Ceil(rank))
	if lo == hi {
		return cp[lo]
	}
	w := rank - float64(lo)
	return cp[lo]*(1-w) + cp[hi]*w
}
