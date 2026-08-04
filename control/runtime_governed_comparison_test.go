package main

import (
	"encoding/json"
	"math"
	"os"
	"path/filepath"
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
	t.Logf("predicted_winner=%s actual_winner=%s basis=%s cost_regret=%.6e latency_regret_ms=%.6f promoted=%v",
		cmp.Decision.PredictedWinner, cmp.Decision.ActualWinner, cmp.Decision.ActualBasis,
		cmp.Decision.CostRegretUSD, cmp.Decision.LatencyRegretMs, cmp.Decision.Promoted)
}
