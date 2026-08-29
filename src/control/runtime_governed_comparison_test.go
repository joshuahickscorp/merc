package main

import (
	"encoding/json"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// syntheticUnboundEmbedCosts is arithmetic-only test data. It is intentionally
// UNBOUND and has no evidence path, producer identity, settlement observation,
// or verification run. Pure tests may exercise selector arithmetic with it; no
// receipt writer may relabel these rows or derive BOUND authority from them.
func syntheticUnboundEmbedCosts(t *testing.T) map[string]MeasuredSupplierLiabilityProxy {
	t.Helper()
	const supplier = miniLMSupplierUSDPerUnit
	return map[string]MeasuredSupplierLiabilityProxy{
		candleEmbedCell: {
			CellID: candleEmbedCell, RuntimeID: "candle_metal", Engine: "candle",
			JobType: "embed", ModelRef: "all-minilm-l6-v2", HWClass: "apple_silicon_ultra",
			HardwareIdentity: "Apple M3 Ultra",
			Samples:          minSupplierLiabilitySamples, Units: 640, MedianMsPerUnit: 0.21875,
			SupplierUSDPerUnit: supplier, VerificationSamples: 20, TerminalAttempts: 20,
			Measured: true, SourceBinding: BindingUnbound, UnknownPlatformCostComponents: unknownPlatformCostComponents(),
		},
		llamaEmbedCell: {
			CellID: llamaEmbedCell, RuntimeID: "llama_cpp_metal", Engine: "llama_cpp",
			JobType: "embed", ModelRef: "all-minilm-l6-v2", HWClass: "apple_silicon_ultra",
			HardwareIdentity: "Apple M3 Ultra",
			Samples:          minSupplierLiabilitySamples, Units: 640, MedianMsPerUnit: 0.28125,
			SupplierUSDPerUnit: supplier, VerificationSamples: 20, TerminalAttempts: 20,
			Measured: true, SourceBinding: BindingUnbound, UnknownPlatformCostComponents: unknownPlatformCostComponents(),
		},
	}
}

// syntheticUnboundLatencies copies only the latency-shaped fields needed by the
// comparison API. It deliberately preserves UNBOUND rather than turning a unit
// fixture into a latency receipt.
func syntheticUnboundLatencies(costs map[string]MeasuredSupplierLiabilityProxy) map[string]GovernedLatencyActual {
	out := make(map[string]GovernedLatencyActual, len(costs))
	for id, cost := range costs {
		out[id] = GovernedLatencyActual{
			CellID: id, RuntimeID: cost.RuntimeID, Engine: cost.Engine, HWClass: cost.HWClass,
			HardwareIdentity: cost.HardwareIdentity,
			Samples:          cost.Samples, Units: cost.Units, MedianMsPerUnit: cost.MedianMsPerUnit,
			SourceBinding: BindingUnbound,
		}
	}
	return out
}

// loadCohortEmbedActuals is the only path from the historical paired-cohort
// receipt into this comparison harness. WITHDRAWN/invalidated evidence is
// terminal, and UNBOUND evidence is citation-only: neither returns rankable
// rows. Only a BOUND, valid receipt can supply actuals to a governed decision.
func loadCohortEmbedActuals(path string) (map[string]MeasuredSupplierLiabilityProxy, string, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, "", err
	}
	var doc struct {
		BindingStatus string                                    `json:"binding_status"`
		Validity      string                                    `json:"validity"`
		MeasuredCost  map[string]MeasuredSupplierLiabilityProxy `json:"measured_cost"`
	}
	if err := json.Unmarshal(raw, &doc); err != nil {
		return nil, "", err
	}

	status := strings.ToUpper(strings.TrimSpace(doc.BindingStatus))
	validity := strings.ToUpper(strings.TrimSpace(doc.Validity))
	if status == BindingWithdrawn || (validity != "" && validity != "VALID") {
		return nil, BindingWithdrawn, nil
	}
	switch status {
	case BindingUnbound, BindingSuperseded:
		return nil, status, nil
	case BindingBound:
		if len(doc.MeasuredCost) == 0 {
			return nil, status, fmt.Errorf("BOUND cohort receipt has no measured_cost rows")
		}
		for id, row := range doc.MeasuredCost {
			row.SourceBinding = BindingBound
			doc.MeasuredCost[id] = row
		}
		return doc.MeasuredCost, status, nil
	default:
		return nil, status, fmt.Errorf("missing or invalid binding_status %q", doc.BindingStatus)
	}
}

func cohortReceiptPath() string {
	return filepath.Join("..", "..", "evidence", "perf", "selector", "paired-cohort-embed.json")
}

func requireRankableCohortActuals(path string) (map[string]MeasuredSupplierLiabilityProxy, error) {
	costs, status, err := loadCohortEmbedActuals(path)
	if err != nil {
		return nil, err
	}
	// This authority id has been withdrawn. A replacement must use a new path;
	// manually relabelling this file BOUND cannot resurrect it.
	if filepath.Clean(path) == filepath.Clean(cohortReceiptPath()) {
		return nil, fmt.Errorf("paired cohort authority id is terminally WITHDRAWN (current status %s); it cannot order actual_winner and a replacement requires a new path", status)
	}
	if status != BindingBound {
		return nil, fmt.Errorf("paired cohort is %s and citation-only; it cannot order actual_winner", status)
	}
	return requireBoundGovernedActuals(costs)
}

func requireBoundGovernedActuals(costs map[string]MeasuredSupplierLiabilityProxy) (map[string]MeasuredSupplierLiabilityProxy, error) {
	if actualsBinding(costs) != BindingBound {
		return nil, fmt.Errorf("governed actuals are not BOUND and cannot order actual_winner")
	}
	return costs, nil
}

func requireBoundGovernedInputs(
	costs map[string]MeasuredSupplierLiabilityProxy,
	latencies map[string]GovernedLatencyActual,
) error {
	if _, err := requireBoundGovernedActuals(costs); err != nil {
		return err
	}
	if status := latencyActualsBinding(latencies); status != BindingBound {
		return fmt.Errorf("governed latency actuals are %s, not BOUND", status)
	}
	if status := governedActualsBinding(costs, latencies); status != BindingBound {
		return fmt.Errorf("combined governed actuals are %s, not BOUND", status)
	}
	return nil
}

func TestLoadCohortEmbedActualsKeepsNonAuthoritativeEvidenceOutOfRanking(t *testing.T) {
	row := MeasuredSupplierLiabilityProxy{
		CellID: candleEmbedCell, Samples: minSupplierLiabilitySamples, Units: 640,
		MedianMsPerUnit: 0.2, SupplierUSDPerUnit: miniLMSupplierUSDPerUnit,
		VerificationSamples: 20, TerminalAttempts: 20, Measured: true,
	}
	tests := []struct {
		name       string
		binding    string
		validity   string
		wantStatus string
		wantRows   bool
	}{
		{name: "withdrawn binding", binding: BindingWithdrawn, validity: "WITHDRAWN", wantStatus: BindingWithdrawn},
		{name: "invalidated validity overrides bound label", binding: BindingBound, validity: "INVALIDATED_PENDING_RERUN", wantStatus: BindingWithdrawn},
		{name: "unbound is citation only", binding: BindingUnbound, validity: "VALID", wantStatus: BindingUnbound},
		{name: "superseded is terminal", binding: BindingSuperseded, validity: "VALID", wantStatus: BindingSuperseded},
		{name: "bound valid rows may rank", binding: BindingBound, validity: "VALID", wantStatus: BindingBound, wantRows: true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "cohort.json")
			raw, err := json.Marshal(map[string]any{
				"binding_status": tc.binding,
				"validity":       tc.validity,
				"measured_cost":  map[string]MeasuredSupplierLiabilityProxy{candleEmbedCell: row},
			})
			must(t, err)
			must(t, os.WriteFile(path, raw, 0o644))

			costs, status, err := loadCohortEmbedActuals(path)
			must(t, err)
			if status != tc.wantStatus {
				t.Fatalf("status = %q, want %q", status, tc.wantStatus)
			}
			if tc.wantRows {
				if len(costs) != 1 || costs[candleEmbedCell].SourceBinding != BindingBound {
					t.Fatalf("BOUND rows not admitted with bound source: %+v", costs)
				}
			} else if len(costs) != 0 {
				t.Fatalf("non-authoritative receipt returned rankable rows: %+v", costs)
			}
		})
	}
}

func TestWeakestEvidenceBindingPreservesTerminalStates(t *testing.T) {
	tests := []struct {
		statuses []string
		want     string
	}{
		{statuses: []string{BindingBound, BindingBound}, want: BindingBound},
		{statuses: []string{BindingBound, ""}, want: BindingUnbound},
		{statuses: []string{BindingBound, BindingSuperseded}, want: BindingSuperseded},
		{statuses: []string{BindingUnbound, BindingWithdrawn}, want: BindingWithdrawn},
	}
	for _, tc := range tests {
		if got := weakestEvidenceBinding(tc.statuses...); got != tc.want {
			t.Fatalf("weakestEvidenceBinding(%v) = %q, want %q", tc.statuses, got, tc.want)
		}
	}
}

func TestWithdrawnPairedCohortCannotOrderActualWinner(t *testing.T) {
	costs, status, err := loadCohortEmbedActuals(cohortReceiptPath())
	must(t, err)
	if status != BindingWithdrawn {
		t.Fatalf("paired cohort status = %q, want %s", status, BindingWithdrawn)
	}
	if len(costs) != 0 {
		t.Fatalf("withdrawn cohort returned rankable rows: %+v", costs)
	}
	if _, err := requireRankableCohortActuals(cohortReceiptPath()); err == nil ||
		!strings.Contains(err.Error(), "cannot order actual_winner") {
		t.Fatalf("withdrawn cohort did not terminate governed comparison construction: %v", err)
	}
}

// boundEmbedLatencies reads the interleaved engine-parity measurement into a
// latency-only type. That harness never observed settlement, retries, rejected
// payouts, or terminal attempts, so this function has no supplier-liability or
// reliability fields to populate. Its BOUND status can rule on latency only.
func boundEmbedLatencies(t *testing.T) map[string]GovernedLatencyActual {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join("..", "..", "evidence", "perf", "selector",
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
	byArm := map[string]struct {
		cell   string
		engine string
	}{
		"candle_metal":    {cell: candleEmbedCell, engine: "candle"},
		"llama_cpp_metal": {cell: llamaEmbedCell, engine: "llama_cpp"},
	}
	out := map[string]GovernedLatencyActual{}
	for arm, expected := range byArm {
		a, ok := doc.Arms[arm]
		if !ok || !strings.EqualFold(a.Engine, expected.engine) ||
			a.MsPerUnit <= 0 || math.IsNaN(a.MsPerUnit) || math.IsInf(a.MsPerUnit, 0) ||
			a.N < governedLatencyMinSamples || len(a.RawSamples) != a.N || doc.Batch <= 0 {
			return nil
		}
		for _, sample := range a.RawSamples {
			if sample <= 0 || math.IsNaN(sample) || math.IsInf(sample, 0) {
				return nil
			}
		}
		out[expected.cell] = GovernedLatencyActual{
			CellID: expected.cell, RuntimeID: arm, Engine: expected.engine,
			HWClass: "apple_silicon_ultra",
			Samples: a.N, Units: int64(a.N * doc.Batch), MedianMsPerUnit: a.MsPerUnit,
			SourceBinding: BindingBound,
		}
	}
	return out
}

func catalogueMinilm() CataloguePriceAuthority {
	return CataloguePriceAuthority{
		ModelID: "all-minilm-l6-v2", JobType: "embed",
		ReferencePricePer1K: miniLMReferencePricePer1K, SupplierShare: miniLMSupplierShare,
	}
}

func governedComparisonShadowFixture() ShadowSelection {
	return ShadowSelection{
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
	}
}

func TestBuildGovernedComparisonRequiresExplicitPriceAndShare(t *testing.T) {
	costs := syntheticUnboundEmbedCosts(t)
	latencies := syntheticUnboundLatencies(costs)
	shadow := governedComparisonShadowFixture()

	tests := []struct {
		name      string
		catalogue CataloguePriceAuthority
		want      string
	}{
		{
			name: "missing reference price",
			catalogue: CataloguePriceAuthority{
				ModelID: "all-minilm-l6-v2", JobType: "embed", SupplierShare: miniLMSupplierShare,
			},
			want: "no explicit positive catalogue reference price",
		},
		{
			name: "missing supplier share",
			catalogue: CataloguePriceAuthority{
				ModelID: "all-minilm-l6-v2", JobType: "embed", ReferencePricePer1K: miniLMReferencePricePer1K,
			},
			want: "no explicit positive supplier share",
		},
		{
			name: "non-finite reference price",
			catalogue: CataloguePriceAuthority{
				ModelID: "all-minilm-l6-v2", JobType: "embed",
				ReferencePricePer1K: math.NaN(), SupplierShare: miniLMSupplierShare,
			},
			want: "no explicit positive catalogue reference price",
		},
		{
			name: "non-finite supplier share",
			catalogue: CataloguePriceAuthority{
				ModelID: "all-minilm-l6-v2", JobType: "embed",
				ReferencePricePer1K: miniLMReferencePricePer1K, SupplierShare: math.Inf(1),
			},
			want: "no explicit positive supplier share",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			_, err := BuildGovernedComparison(shadow, costs, latencies, tc.catalogue, nil, false)
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("error = %v, want refusal containing %q", err, tc.want)
			}
		})
	}
}

func TestGovernedComparisonKeepsCADBuyerAndSupplierMoneyInOneCurrency(t *testing.T) {
	const fx = 1.37
	settlementPrice := ceilPricePer1K(miniLMReferencePricePer1K * fx)
	supplierCADPerUnit := settlementPrice / 1000 * miniLMSupplierShare
	costs := syntheticUnboundEmbedCosts(t)
	for id, cost := range costs {
		cost.Currency = "cad"
		cost.SupplierUSDPerUnit = supplierCADPerUnit
		costs[id] = cost
	}
	catalogue := catalogueMinilm()
	catalogue.Version = 2
	catalogue.PriceSource = "market_board"
	catalogue.ScheduleVersion = 2
	catalogue.ScheduleSHA256 = strings.Repeat("c", 64)
	catalogue.BoardSHA256 = strings.Repeat("d", 64)
	catalogue.ReferenceCurrency = "usd"
	catalogue.SettlementCurrency = "cad"
	catalogue.SettlementPricePer1K = settlementPrice
	catalogue.ReferenceToSettlementRate = fx
	catalogue.FXRevision = "test-cad-fx"
	catalogue.PriceFormula = "test frozen CAD authority"
	must(t, validateCataloguePriceAuthority(catalogue))

	comparison, err := BuildGovernedComparison(
		governedComparisonShadowFixture(), costs, syntheticUnboundLatencies(costs),
		catalogue, nil, false,
	)
	must(t, err)
	for id, row := range comparison.Cells {
		if row.Currency != "cad" || math.Abs(row.BuyerPricePer1K-settlementPrice) > 1e-15 {
			t.Fatalf("cell %s mixed buyer currency/price: %+v", id, row)
		}
		if row.MercGrossPlatformAvailable || row.MercGrossPlatformUSDUnit != 0 ||
			!strings.Contains(row.MercGrossPlatformBasis, "input/output") {
			t.Fatalf("cell %s invented denominatorless CAD gross: %+v", id, row)
		}
	}
	if comparison.Decision.Currency != "cad" {
		t.Fatalf("decision currency = %q, want cad", comparison.Decision.Currency)
	}
	if got := comparison.CostComparison["currency"]; got != "cad" {
		t.Fatalf("comparison cost currency = %v, want cad", got)
	}
	usdDecision := comparison.Decision
	usdDecision.Currency = "usd"
	usdDecision.AuthorityDigest = ""
	usdDigest, err := digestGovernedDecision(usdDecision)
	must(t, err)
	if usdDigest == comparison.Decision.AuthorityDigest {
		t.Fatal("decision authority digest does not bind its money currency")
	}
}

func TestBuildGovernedComparisonRefusesSupplierCurrencyMismatchBeforeRanking(t *testing.T) {
	shadow := governedComparisonShadowFixture()
	costs := syntheticUnboundEmbedCosts(t)
	wrong := costs[llamaEmbedCell]
	wrong.Currency = "cad"
	costs[llamaEmbedCell] = wrong
	_, err := BuildGovernedComparison(
		shadow, costs, syntheticUnboundLatencies(costs), catalogueMinilm(), nil, false)
	if err == nil || !strings.Contains(err.Error(), "cross-currency ranking is refused") {
		t.Fatalf("USD/CAD proxies reached governed ranking: %v", err)
	}
}

func TestBuildGovernedComparisonNoDecisionDoesNotDivergeOrPassQuality(t *testing.T) {
	tests := []struct {
		name        string
		mutate      func(*MeasuredSupplierLiabilityProxy)
		wantQuality string
	}{
		{
			name: "missing reliability evidence",
			mutate: func(row *MeasuredSupplierLiabilityProxy) {
				row.VerificationSamples = 0
				row.TerminalAttempts = 0
			},
			wantQuality: "UNVERIFIED_INSUFFICIENT_RELIABILITY_EVIDENCE",
		},
		{
			name: "verification failure",
			mutate: func(row *MeasuredSupplierLiabilityProxy) {
				row.VerificationFails = 1
			},
			wantQuality: "VERIFICATION_FAILURES",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			costs := syntheticUnboundEmbedCosts(t)
			row := costs[llamaEmbedCell]
			tc.mutate(&row)
			costs[llamaEmbedCell] = row
			cmp, err := BuildGovernedComparison(
				governedComparisonShadowFixture(), costs, syntheticUnboundLatencies(costs),
				catalogueMinilm(), nil, false)
			must(t, err)
			if cmp.Decision.ActualWinner != "" || cmp.Decision.ActualBasis != "" {
				t.Fatalf("incomplete cohort produced an actual decision: %+v", cmp.Decision)
			}
			if cmp.Decision.Diverged {
				t.Fatal("no decision was reported as a divergence")
			}
			if cmp.Decision.SupplierLiabilityHWClass != "" || cmp.HWClass != "" {
				t.Fatalf("incomplete cohort invented hardware scope: decision=%q receipt=%q",
					cmp.Decision.SupplierLiabilityHWClass, cmp.HWClass)
			}
			if !strings.Contains(cmp.Decision.QualityOutcome, tc.wantQuality) {
				t.Fatalf("quality = %q, want %q", cmp.Decision.QualityOutcome, tc.wantQuality)
			}
			if cmp.Decision.EconomicsRefusal == "" {
				t.Fatal("incomplete cohort has no selection refusal")
			}
		})
	}
}

func TestBuildGovernedComparisonRefusesMixedSupplierLiabilityHardwareScope(t *testing.T) {
	costs := syntheticUnboundEmbedCosts(t)
	row := costs[llamaEmbedCell]
	row.HWClass = "nvidia_48gb"
	costs[llamaEmbedCell] = row
	_, err := BuildGovernedComparison(
		governedComparisonShadowFixture(), costs, syntheticUnboundLatencies(costs),
		catalogueMinilm(), nil, false)
	if err == nil || !strings.Contains(err.Error(), "hardware scope is mixed") {
		t.Fatalf("mixed hardware scope error = %v", err)
	}
}

func TestBuildGovernedComparisonRefusesLatencyScopeMismatch(t *testing.T) {
	costs := syntheticUnboundEmbedCosts(t)
	latencies := syntheticUnboundLatencies(costs)
	row := latencies[llamaEmbedCell]
	row.Engine = "candle"
	latencies[llamaEmbedCell] = row
	_, err := BuildGovernedComparison(
		governedComparisonShadowFixture(), costs, latencies, catalogueMinilm(), nil, false)
	if err == nil || !strings.Contains(err.Error(), "latency engine") {
		t.Fatalf("latency scope mismatch error = %v", err)
	}
}

// TestDecideMeasuredShadowNamesThroughputOnCostTie pins the honesty rule: a
// equal supplier liability must not claim a complete cost result, and the
// faster cell may win on MORE_THROUGHPUT_AT_EQUAL_SUPPLIER_LIABILITY.
func TestDecideMeasuredShadowNamesThroughputOnCostTie(t *testing.T) {
	costs := syntheticUnboundEmbedCosts(t)
	cells := []string{candleEmbedCell, llamaEmbedCell}
	d := decideMeasuredSupplierLiabilityShadow(costs, cells, candleEmbedCell)
	if d.Basis != selectionBasisThroughputEqualLiability {
		t.Fatalf("basis = %q, want %s (cost ties; throughput decides)",
			d.Basis, selectionBasisThroughputEqualLiability)
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
	const supplier = miniLMSupplierUSDPerUnit
	costs := map[string]MeasuredSupplierLiabilityProxy{
		candleEmbedCell: {
			CellID: candleEmbedCell, Samples: minSupplierLiabilitySamples, Measured: true,
			MedianMsPerUnit: 0.109, SupplierUSDPerUnit: supplier,
			VerificationSamples: 20, TerminalAttempts: 20,
		},
		llamaEmbedCell: {
			CellID: llamaEmbedCell, Samples: minSupplierLiabilitySamples, Measured: true,
			MedianMsPerUnit: 0.100, SupplierUSDPerUnit: supplier,
			VerificationSamples: 20, TerminalAttempts: 20,
		},
	}
	// Sanity: the gap really is outside the ratio band, so only the absolute
	// floor can refuse it. If this stops holding the test is no longer covering
	// what it claims to.
	if ratio := (0.109 - 0.100) / ((0.109 + 0.100) / 2); ratio < latencyNoiseFraction {
		t.Fatalf("gap ratio %.4f is inside the %.4f band; this test no longer exercises the absolute floor",
			ratio, latencyNoiseFraction)
	}
	d := decideMeasuredSupplierLiabilityShadow(costs, []string{candleEmbedCell, llamaEmbedCell}, candleEmbedCell)
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
	const supplier = miniLMSupplierUSDPerUnit
	costs := map[string]MeasuredSupplierLiabilityProxy{
		candleEmbedCell: {
			CellID: candleEmbedCell, Samples: minSupplierLiabilitySamples, Measured: true,
			MedianMsPerUnit: 3.02, SupplierUSDPerUnit: supplier,
			VerificationSamples: 20, TerminalAttempts: 20,
		},
		llamaEmbedCell: {
			CellID: llamaEmbedCell, Samples: minSupplierLiabilitySamples, Measured: true,
			MedianMsPerUnit: 3.00, SupplierUSDPerUnit: supplier,
			VerificationSamples: 20, TerminalAttempts: 20,
		},
	}
	// Sanity: the gap clears the absolute floor, so only the ratio band can
	// refuse it.
	if 3.02-3.00 < latencyNoiseAbsMs {
		t.Fatalf("gap is inside the %.4f ms absolute floor; this test no longer exercises the ratio band",
			latencyNoiseAbsMs)
	}
	d := decideMeasuredSupplierLiabilityShadow(costs, []string{candleEmbedCell, llamaEmbedCell}, candleEmbedCell)
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
	const supplier = miniLMSupplierUSDPerUnit
	costs := map[string]MeasuredSupplierLiabilityProxy{
		candleEmbedCell: {
			CellID: candleEmbedCell, Samples: minSupplierLiabilitySamples, Measured: true,
			MedianMsPerUnit: 0.25, SupplierUSDPerUnit: supplier,
			VerificationSamples: 20, TerminalAttempts: 20,
		},
		llamaEmbedCell: {
			CellID: llamaEmbedCell, Samples: minSupplierLiabilitySamples, Measured: true,
			MedianMsPerUnit: 0.25, SupplierUSDPerUnit: supplier,
			VerificationSamples: 20, TerminalAttempts: 20,
		},
	}
	d := decideMeasuredSupplierLiabilityShadow(costs, []string{candleEmbedCell, llamaEmbedCell}, candleEmbedCell)
	if d.Basis != selectionBasisTieNoDecision {
		t.Fatalf("basis = %q, want %s", d.Basis, selectionBasisTieNoDecision)
	}
	if d.Winner != candleEmbedCell {
		t.Fatalf("tie should retain routed cell, got %q", d.Winner)
	}
}

// TestDecideMeasuredShadowRefusesVerificationFailedCell proves an unpaid
// rejected result does not inflate supplier liability and cannot remain in the
// measured cohort merely by setting Measured=true.
func TestDecideMeasuredShadowRefusesVerificationFailedCell(t *testing.T) {
	costs := map[string]MeasuredSupplierLiabilityProxy{
		candleEmbedCell: {
			CellID: candleEmbedCell, Samples: minSupplierLiabilitySamples, Measured: true,
			MedianMsPerUnit: 0.3, SupplierUSDPerUnit: miniLMSupplierUSDPerUnit,
			VerificationSamples: 20, TerminalAttempts: 20,
		},
		llamaEmbedCell: {
			CellID: llamaEmbedCell, Samples: minSupplierLiabilitySamples, Measured: true,
			MedianMsPerUnit: 0.2, SupplierUSDPerUnit: miniLMSupplierUSDPerUnit,
			VerificationSamples: 40, VerificationFails: 10, TerminalAttempts: 40,
		},
	}
	failedPayable, ok := costs[llamaEmbedCell].ExpectedSupplierLiabilityUSDPerVerifiedUnit()
	if !ok || math.Abs(failedPayable-miniLMSupplierUSDPerUnit) > 1e-15 {
		t.Fatalf("verification failure changed payable supplier liability: got=%v ok=%v", failedPayable, ok)
	}
	if _, ok := measuredSupplierLiability(costs, llamaEmbedCell); ok {
		t.Fatal("verification-failed cell remained eligible as measured supplier liability")
	}
	d := decideMeasuredSupplierLiabilityShadow(costs, []string{candleEmbedCell, llamaEmbedCell}, candleEmbedCell)
	if d.Basis != "" || d.Winner != "" {
		t.Fatalf("verification-failed cohort produced a decision: %+v", d)
	}
}

// TestRankedByMeasuredCostDoesNotClaimCostWinOnTie is the integration point:
// ShadowSelection.rankedByMeasuredSupplierLiability must surface the throughput basis.
func TestRankedByMeasuredCostDoesNotClaimCostWinOnTie(t *testing.T) {
	costs := syntheticUnboundEmbedCosts(t)
	byHW := map[string]map[string]MeasuredSupplierLiabilityProxy{
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
	out := s.rankedByMeasuredSupplierLiability(byHW)
	if out.SelectionBasis != selectionBasisThroughputEqualLiability {
		t.Fatalf("selection_basis = %q, want %s", out.SelectionBasis, selectionBasisThroughputEqualLiability)
	}
	if out.ShadowCellID != candleEmbedCell {
		t.Fatalf("shadow = %q, want candle", out.ShadowCellID)
	}
	if out.SupplierLiabilityHWClass != "apple_silicon_ultra" {
		t.Fatalf("hw = %q", out.SupplierLiabilityHWClass)
	}
	if out.SupplierLiabilityHardwareIdentity != "Apple M3 Ultra" {
		t.Fatalf("hardware identity = %q", out.SupplierLiabilityHardwareIdentity)
	}
	// Measured fields populated on candidates.
	for _, c := range out.Considered {
		if !c.SupplierLiabilityMeasured || c.SupplierLiabilityUSDPerVerifiedUnit <= 0 || c.MedianMsPerUnit <= 0 {
			t.Fatalf("candidate not fully measured: %+v", c)
		}
	}
}

// TestGovernedComparisonKeepsSyntheticInputsUnbound pins the evidence boundary:
// arithmetic fixtures may exercise comparison structure, but remain UNBOUND in
// every output and cannot rule on the latency prior or reach a receipt writer.
//
// The synthetic rows put candle ahead, 0.21875 against 0.28125 ms per unit.
// Those numbers are useful for the pure arithmetic test below, but their
// UNBOUND label cannot authorize the arithmetic winner or overturn a prior claim.
func TestGovernedComparisonKeepsSyntheticInputsUnbound(t *testing.T) {
	costs := syntheticUnboundEmbedCosts(t)
	latencies := syntheticUnboundLatencies(costs)
	if binding := actualsBinding(costs); binding != BindingUnbound {
		t.Fatalf("synthetic actuals binding = %q, want %s", binding, BindingUnbound)
	}
	if err := requireBoundGovernedInputs(costs, latencies); err == nil ||
		!strings.Contains(err.Error(), "cannot order actual_winner") {
		t.Fatalf("UNBOUND actuals did not stop before comparison construction: %v", err)
	}
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
	cmp, err := BuildGovernedComparison(shadow, costs, latencies, catalogueMinilm(), map[string]any{
		"note": "synthetic arithmetic fixture; no evidence authority",
	}, true)
	must(t, err)
	if s, _ := cmp.PriorThroughputClaim["status"].(string); s != "UNRESOLVED_UNBOUND_ACTUALS" {
		t.Fatalf("synthetic actuals status = %q, want UNRESOLVED_UNBOUND_ACTUALS", s)
	}
	if s, _ := cmp.ActualsBinding["status"].(string); s != BindingUnbound {
		t.Fatalf("combined synthetic binding = %q, want %s", s, BindingUnbound)
	}
	if cmp.Decision.SupplierLiabilityHWClass != "apple_silicon_ultra" {
		t.Fatalf("exact common hardware scope = %q, want apple_silicon_ultra",
			cmp.Decision.SupplierLiabilityHWClass)
	}
	if cmp.Decision.QualityOutcome != "UNRESOLVED_UNBOUND_RELIABILITY_ACTUALS" {
		t.Fatalf("synthetic cohort claimed authoritative quality = %q", cmp.Decision.QualityOutcome)
	}

	// Predicted winner under the prior is llama; actual is candle.
	if cmp.Decision.PredictedWinner != llamaEmbedCell {
		t.Fatalf("predicted winner = %q, want llama under 2.1× prior", cmp.Decision.PredictedWinner)
	}
	if cmp.Decision.ActualWinner != candleEmbedCell {
		t.Fatalf("arithmetic winner = %q, want candle", cmp.Decision.ActualWinner)
	}
	if cmp.Decision.ActualBasis != selectionBasisThroughputEqualLiability {
		t.Fatalf("actual basis = %q", cmp.Decision.ActualBasis)
	}
	if !strings.Contains(cmp.Decision.SelectionReason, "more throughput at equal supplier liability") {
		t.Fatalf("selection reason does not name throughput-at-equal-supplier-liability: %q",
			cmp.Decision.SelectionReason)
	}
	if strings.Contains(strings.ToLower(cmp.Decision.SelectionReason), "cheaper") {
		t.Fatalf("selection reason must not claim cheaper: %q", cmp.Decision.SelectionReason)
	}

	// Cost ties; cost regret on the predicted winner is ~0 (catalogue form).
	if math.Abs(cmp.Decision.SupplierLiabilityPredictionErrorUSD) > 1e-12 {
		// Predicted cost uses catalogue; actual also cancelled form at equal
		// reliability — should be zero. If not, surface the number.
		t.Logf("supplier-liability prediction error = %.17f (expected ~0)", cmp.Decision.SupplierLiabilityPredictionErrorUSD)
	}
	candle := cmp.Cells[candleEmbedCell]
	llama := cmp.Cells[llamaEmbedCell]
	if candle.LatencySourceBinding != BindingUnbound || candle.SupplierLiabilitySourceBinding != BindingUnbound ||
		llama.LatencySourceBinding != BindingUnbound || llama.SupplierLiabilitySourceBinding != BindingUnbound {
		t.Fatalf("synthetic cell rows escaped UNBOUND: candle=%+v llama=%+v", candle, llama)
	}
	if math.Abs(candle.ActualSupplierLiabilityUSDPerVerifiedUnit-llama.ActualSupplierLiabilityUSDPerVerifiedUnit) > 1e-12 {
		t.Fatalf("actual supplier liabilities must tie: candle=%v llama=%v",
			candle.ActualSupplierLiabilityUSDPerVerifiedUnit,
			llama.ActualSupplierLiabilityUSDPerVerifiedUnit)
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
		llama.LatencyRegretMsPerUnit, llama.SupplierLiabilityPredictionErrorUSDPerUnit)

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

func TestBoundEngineParityReceiptIsLatencyOnlyAndCannotAuthorizeSelection(t *testing.T) {
	latencies := boundEmbedLatencies(t)
	if len(latencies) != 2 {
		t.Fatalf("BOUND engine-parity latency rows = %d, want 2", len(latencies))
	}
	if status := latencyActualsBinding(latencies); status != BindingBound {
		t.Fatalf("latency binding = %q, want %s", status, BindingBound)
	}

	// The arithmetic fixture deliberately prefers Candle; the independent BOUND
	// latency receipt currently prefers llama.cpp. If Build accidentally copied
	// the latency receipt into the selector proxy, this winner would flip.
	costs := syntheticUnboundEmbedCosts(t)
	_, err := BuildGovernedComparison(
		governedComparisonShadowFixture(), costs, latencies, catalogueMinilm(), nil, true)
	if err == nil || !strings.Contains(err.Error(), "latency hardware") {
		t.Fatalf("legacy BOUND latency without exact device identity entered selector authority: %v", err)
	}
	if err := requireBoundGovernedInputs(costs, latencies); err == nil {
		t.Fatal("latency-only receipt passed the BOUND governed-input writer gate")
	}
}

func TestBoundLatencyWithoutSupplierActualsProducesNoSelectionOrQualityVerdict(t *testing.T) {
	latencies := boundEmbedLatencies(t)
	if len(latencies) != 2 {
		t.Fatalf("BOUND engine-parity latency rows = %d, want 2", len(latencies))
	}
	cmp, err := BuildGovernedComparison(
		governedComparisonShadowFixture(), nil, latencies, catalogueMinilm(), nil, true)
	must(t, err)
	if cmp.Decision.ActualWinner != "" || cmp.Decision.ActualBasis != "" || cmp.Decision.Diverged {
		t.Fatalf("latency-only input produced a selector decision: %+v", cmp.Decision)
	}
	if cmp.Decision.QualityOutcome != "UNVERIFIED_INSUFFICIENT_RELIABILITY_EVIDENCE" {
		t.Fatalf("latency-only input produced quality outcome %q", cmp.Decision.QualityOutcome)
	}
	if cmp.Decision.SupplierLiabilityHWClass != "" || cmp.HWClass != "" {
		t.Fatalf("latency-only input invented supplier-liability HW scope: decision=%q receipt=%q",
			cmp.Decision.SupplierLiabilityHWClass, cmp.HWClass)
	}
	if got, _ := cmp.ActualsBinding["status"].(string); got != BindingUnbound {
		t.Fatalf("latency-only combined binding = %q, want %s", got, BindingUnbound)
	}
	if status, _ := cmp.PriorThroughputClaim["status"].(string); strings.Contains(status, "UNBOUND") || status == "UNVERIFIED_NO_ACTUAL" {
		t.Fatalf("BOUND latency did not remain available for latency-prior evaluation: %q", status)
	}
	for id, row := range cmp.Cells {
		if row.ActualLatencyMsPerUnit <= 0 || row.LatencySourceBinding != BindingBound {
			t.Fatalf("cell %q lost BOUND latency: %+v", id, row)
		}
		if row.ActualSupplierLiabilityUSDPerVerifiedUnit != 0 ||
			row.SupplierLiabilitySourceBinding != BindingUnbound {
			t.Fatalf("cell %q latency minted supplier actual: %+v", id, row)
		}
	}
}

// TestGovernedComparisonNoPriorKeepsLadderPrediction covers the path where we
// do not inject the 2.1× prior: predicted equals the ladder shadow choice.
func TestGovernedComparisonNoPriorKeepsLadderPrediction(t *testing.T) {
	costs := syntheticUnboundEmbedCosts(t)
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
	cmp, err := BuildGovernedComparison(shadow, costs, syntheticUnboundLatencies(costs), catalogueMinilm(), nil, false)
	must(t, err)
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
	if math.Abs(cmp.Decision.SupplierLiabilityPredictionErrorUSD) > 1e-12 {
		t.Fatalf("supplier-liability prediction error should be ~0, got %v",
			cmp.Decision.SupplierLiabilityPredictionErrorUSD)
	}
}

// TestEconomicSelectorTwoCases exercises selector arithmetic only. Every row is
// an unmistakably UNBOUND unit fixture; this test is not evidence and cannot be
// used by either receipt writer.
//
// Case A (synthetic latencies, equal reliability): the faster arm may win at equal
// measured supplier liability; this is a throughput claim, not a cost claim.
//
// Case B gives the faster fixture verification failures. Rejected work is
// unpaid, so the failure must not inflate supplier liability; it makes that arm
// ineligible and the comparison refuses to select.
func TestEconomicSelectorTwoCases(t *testing.T) {
	live := syntheticUnboundEmbedCosts(t)
	t.Log("source: synthetic UNBOUND selector arithmetic fixture")

	cells := []string{candleEmbedCell, llamaEmbedCell}
	candleMs := live[candleEmbedCell].MedianMsPerUnit
	llamaMs := live[llamaEmbedCell].MedianMsPerUnit
	if candleMs <= 0 || llamaMs <= 0 {
		t.Fatal("missing median_ms_per_unit on live/cohort costs")
	}

	// ── Case A: equal reliability, faster wins at equal supplier liability ─
	caseA := cloneMeasuredCosts(live)
	// Force equal clean verification so cost ties by construction.
	for id, c := range caseA {
		c.VerificationSamples = 40
		c.VerificationFails = 0
		c.TerminalAttempts = 40
		c.TerminalFails = 0
		caseA[id] = c
	}
	dA := decideMeasuredSupplierLiabilityShadow(caseA, cells, candleEmbedCell)
	wantAWinner := candleEmbedCell
	if llamaMs < candleMs && !latenciesTie(llamaMs, candleMs) {
		wantAWinner = llamaEmbedCell
	} else if candleMs < llamaMs && !latenciesTie(llamaMs, candleMs) {
		wantAWinner = candleEmbedCell
	}
	if dA.Basis != selectionBasisThroughputEqualLiability && dA.Basis != selectionBasisTieNoDecision {
		t.Fatalf("case A basis = %q, want throughput-at-equal-liability or tie", dA.Basis)
	}
	if dA.Winner != wantAWinner && dA.Basis != selectionBasisTieNoDecision {
		t.Fatalf("case A winner = %q, want %q (faster at equal supplier liability)", dA.Winner, wantAWinner)
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

	// ── Case B: unequal liability refuses a total-cost decision ─────────────
	// Make the FASTER cell fail verification 25% of the time. The failed work is
	// unpaid, so payable supplier liability must remain unchanged while the arm
	// becomes ineligible for measured selection.
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
			c.VerificationFails = 10
		} else {
			c.VerificationFails = 0
		}
		caseB[id] = c
	}
	// Confirm construction: verification failure does not invent a payout.
	fastVO, okF := caseB[fasterID].ExpectedSupplierLiabilityUSDPerVerifiedUnit()
	slowVO, okS := caseB[slowerID].ExpectedSupplierLiabilityUSDPerVerifiedUnit()
	if !okF || !okS {
		t.Fatal("case B VO costs unavailable")
	}
	if !supplierLiabilitiesTieUSD(slowVO, fastVO) {
		t.Fatalf("verification failure changed payable supplier liability: slower=%v faster=%v",
			slowVO, fastVO)
	}
	if !(caseB[fasterID].MedianMsPerUnit < caseB[slowerID].MedianMsPerUnit) {
		t.Fatalf("case B construction: faster cell is not actually faster: %v vs %v",
			caseB[fasterID].MedianMsPerUnit, caseB[slowerID].MedianMsPerUnit)
	}

	dB := decideMeasuredSupplierLiabilityShadow(caseB, cells, fasterID /* routed = speed-preferring */)
	if dB.Basis != "" || dB.Winner != "" {
		t.Fatalf("case B selected with an ineligible verification-failed arm: %+v", dB)
	}
	if _, ok := measuredSupplierLiability(caseB, fasterID); ok {
		t.Fatal("verification-failed arm remained eligible for measured selection")
	}
	t.Logf("case B: selection refused; failed=%s payable_liability_unchanged=%.6e USD/unit",
		fasterID, fastVO)
}

func cloneMeasuredCosts(in map[string]MeasuredSupplierLiabilityProxy) map[string]MeasuredSupplierLiabilityProxy {
	out := make(map[string]MeasuredSupplierLiabilityProxy, len(in))
	for k, v := range in {
		out[k] = v
	}
	return out
}

// selectionRegretFromCosts is chosen − best_available on latency (ms/unit) and
// verified-outcome cost (USD/unit). Absolute units only.
func selectionRegretFromCosts(costs map[string]MeasuredSupplierLiabilityProxy, chosen string) (latMs, costUSD float64) {
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
	chVO, okCh := ch.ExpectedSupplierLiabilityUSDPerVerifiedUnit()
	if !okCh {
		return latMs, 0
	}
	bestVO := chVO
	for _, c := range costs {
		if vo, ok := c.ExpectedSupplierLiabilityUSDPerVerifiedUnit(); ok && vo < bestVO {
			bestVO = vo
		}
	}
	costUSD = chVO - bestVO
	return latMs, costUSD
}

// TestWriteGovernedComparisonReceipt is an env-gated legacy writer. It fails
// closed while the independent supplier-liability cohort and this output
// authority id are WITHDRAWN. A replacement must use new BOUND inputs and a new
// output path rather than relabel either historical artifact.
func TestWriteGovernedComparisonReceipt(t *testing.T) {
	if os.Getenv("MERC_GOVERNED_COMPARISON_RECEIPT") != "1" {
		t.Skip("MERC_GOVERNED_COMPARISON_RECEIPT is not 1; receipt writer is env-gated")
	}
	latencies := boundEmbedLatencies(t)
	if latencies == nil {
		t.Fatal("governed comparison latency actuals unavailable: engine-parity receipt is absent, invalid, or not BOUND")
	}
	// The latency receipt cannot supply money or reliability. The only historical
	// settlement/verification cohort at this authority id is WITHDRAWN, so this
	// writer fails here until a new BOUND authority path exists.
	costs, err := requireRankableCohortActuals(cohortReceiptPath())
	mustf(t, err, "governed comparison supplier-liability actuals unavailable: %v")
	mustf(t, requireBoundGovernedInputs(costs, latencies),
		"governed comparison inputs unavailable: %v")
	t.Log("latency source: evidence/perf/selector/engine-parity-metal-embed-latest.json (BOUND latency only)")
	t.Log("supplier-liability source: evidence/perf/selector/paired-cohort-embed.json (independent BOUND settlement/verification required)")
	// Plan a real shadow selection so eligibility / exclusions are live.
	withActivationRestored(t)
	decision, err := buildWorkloadDecision(jobSubmit{
		JobType:     JobType{Type: "embed"},
		Model:       ModelRef{Kind: "hf", Ref: "all-minilm-l6-v2"},
		Tier:        "batch",
		Constraints: JobConstraints{MaxDurationSecs: 3600},
	}, strings.Repeat("d", 64))
	must(t, err)
	shadow, err := planShadowSelection(decision)
	must(t, err)
	// Apply measured ranking the same way createJob would after costs exist.
	shadow = shadow.rankedByMeasuredSupplierLiability(map[string]map[string]MeasuredSupplierLiabilityProxy{
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

	cmp, err := BuildGovernedComparison(shadow, costs, latencies, catalogueMinilm(), hostLoad, true)
	must(t, err)

	// Serialise as map for the bound writer.
	raw, err := json.Marshal(cmp)
	must(t, err)
	var payload map[string]any
	must(t, json.Unmarshal(raw, &payload))

	dir := filepath.Join("..", "..", "evidence", "perf", "selector")
	must(t, os.MkdirAll(dir, 0o755))
	path := filepath.Join(dir, "governed-candle-vs-llama-shadow-decision.json")

	// Evidence admission above guarantees that derived comparison authority can
	// never be minted from UNBOUND or WITHDRAWN actuals.
	id, bin, err := DefaultBoundIdentity("../..",
		"src/control/runtime_governed_comparison_test.go",
		"governed embed comparison candle vs llama.cpp + one shadow decision; predictedFromPrior=true",
		"latency from a BOUND engine-parity receipt; supplier liability/reliability from an independent BOUND settlement/verification cohort")
	mustf(t, err, "bound identity unavailable; evidence was not written: %v")
	mustf(t, WriteBoundEvidenceJSON(EvidenceWriteRequest{
		RepoRoot: "..", Path: path, Payload: payload,
		Identity: id, BuildBinaryPath: bin,
	}), "bound evidence write refused: %v; a withdrawn authority is sticky — use a new authority path/id")
	t.Logf("wrote governed comparison + shadow decision %s", path)
	t.Logf("predicted_winner=%s actual_winner=%s basis=%s prediction_cost_regret=%.6e prediction_latency_regret_ms=%.6f selection_cost_regret=%.6e selection_latency_regret_ms=%.6f promoted=%v",
		cmp.Decision.PredictedWinner, cmp.Decision.ActualWinner, cmp.Decision.ActualBasis,
		cmp.Decision.SupplierLiabilityPredictionErrorUSD, cmp.Decision.LatencyRegretMs,
		cmp.Decision.SelectionSupplierLiabilityRegretUSD, cmp.Decision.SelectionLatencyRegretMs,
		cmp.Decision.Promoted)
}

// TestWriteEconomicSelectorProofReceipt is env-gated and intentionally fail-closed.
// The former writer combined BOUND latency with a constructed reliability branch
// and could stamp the derived arithmetic BOUND. A replacement proof requires a
// new authority path backed by an observed settlement/verification cohort.
func TestWriteEconomicSelectorProofReceipt(t *testing.T) {
	if os.Getenv("MERC_ECONOMIC_SELECTOR_PROOF_RECEIPT") != "1" {
		t.Skip("MERC_ECONOMIC_SELECTOR_PROOF_RECEIPT is not 1; receipt writer is env-gated")
	}
	latencies := boundEmbedLatencies(t)
	if latencies == nil {
		t.Fatal("economic selector latency actuals unavailable: engine-parity receipt is absent, invalid, or not BOUND")
	}
	costs, err := requireRankableCohortActuals(cohortReceiptPath())
	if err != nil {
		t.Fatalf("BOUND economic selector proof refused: %v; latency-only evidence cannot supply payout or reliability", err)
	}
	if err := requireBoundGovernedInputs(costs, latencies); err != nil {
		t.Fatalf("BOUND economic selector proof refused: %v", err)
	}
	t.Fatal("BOUND economic selector proof refused: the dual-case reliability branch is constructed, not observed; run a real settlement/verification cohort and use a new authority path")
}
