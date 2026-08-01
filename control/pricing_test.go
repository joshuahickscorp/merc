package main

import (
	"encoding/json"
	"math"
	"strings"
	"testing"
)

func TestDiagnosticCostFloorMathIsCorrect(t *testing.T) {
	b := measuredThroughput{
		ModelID: "test-model", JobType: "embed", UnitsPerSec: 1000.0, HWClass: "apple_silicon_pro",
		SourceCitation: "test",
	}
	got := diagnosticCostFloorFromSupplierEconomics(b, 0.97, 0.10)
	wantPrice := (targetSupplierUSDHr + 0.003) / (3600000.0 / 1000.0 * 0.97)
	if diff := got.PricePer1K - wantPrice; diff > 1e-9 || diff < -1e-9 {
		t.Fatalf("price_per_1k = %.10f, want %.10f", got.PricePer1K, wantPrice)
	}
	if got.ModelID != "test-model" || got.JobType != "embed" {
		t.Fatalf("result header wrong: %+v", got)
	}
	for _, want := range []string{"test", "apple_silicon_pro", "30W", "target_supplier_usd_hr=2.00"} {
		if !strings.Contains(got.Formula, want) {
			t.Fatalf("formula missing %q: %s", want, got.Formula)
		}
	}
}

func TestDiagnosticCostFloorHigherThroughputMeansLowerFloor(t *testing.T) {
	slow := measuredThroughput{ModelID: "slow", JobType: "batch_infer", UnitsPerSec: 100, HWClass: "apple_silicon_pro"}
	fast := measuredThroughput{ModelID: "fast", JobType: "batch_infer", UnitsPerSec: 1000, HWClass: "apple_silicon_pro"}
	slowPrice := diagnosticCostFloorFromSupplierEconomics(slow, 0.97, 0.15).PricePer1K
	fastPrice := diagnosticCostFloorFromSupplierEconomics(fast, 0.97, 0.15).PricePer1K
	if fastPrice >= slowPrice {
		t.Fatalf("10x throughput should set a materially lower diagnostic cost floor: slow=%.8f fast=%.8f", slowPrice, fastPrice)
	}
}

func TestDiagnosticCostFloorUnknownHWClassFallsBackConservatively(t *testing.T) {
	b := measuredThroughput{ModelID: "m", JobType: "embed", UnitsPerSec: 500, HWClass: "some_future_chip"}
	got := diagnosticCostFloorFromSupplierEconomics(b, 0.97, 0.15)
	if got.PricePer1K <= 0 {
		t.Fatalf("unknown hw_class should still yield a positive price, got %v", got.PricePer1K)
	}
	if !strings.Contains(got.Formula, "30W") {
		t.Fatalf("unknown hw_class should fall back to the 30W conservative default, formula: %s", got.Formula)
	}
}

func TestPublishedCatalogueResultsOmitsUnmeasuredModels(t *testing.T) {
	results := PublishedCatalogueResults()
	if len(results) == 0 {
		t.Fatal("expected at least the two board-mapped measured models")
	}
	seen := map[string]bool{}
	for _, r := range results {
		seen[r.ModelID] = true
		if r.PricePer1K <= 0 {
			t.Fatalf("published model %s has non-positive price %v", r.ModelID, r.PricePer1K)
		}
		if !strings.Contains(r.Formula, "confidence_weighted_median(board[") {
			t.Fatalf("published catalogue price must cite market board, formula: %s", r.Formula)
		}
	}
	if !seen["all-minilm-l6-v2"] || !seen["llama-3.2-1b-instruct-q4"] {
		t.Fatalf("expected the two board-mapped models in the result, got %v", seen)
	}
	if seen["unsupported-model"] {
		t.Fatal("unmeasured model must never be published")
	}
}

func TestMarketBoardIsWeightedMedianTimesMultiplier(t *testing.T) {
	board, err := loadPriceBoard()
	if err != nil {
		t.Fatalf("load price board: %v", err)
	}
	class := board.Classes["embed_small"]
	median, _, err := confidenceWeightedMedianUSDPer1K("embed_small", class)
	if err != nil {
		t.Fatal(err)
	}
	r, ok := repriceFromMarketBoard("all-minilm-l6-v2", "embed", board)
	if !ok {
		t.Fatal("expected board price for all-minilm-l6-v2")
	}
	want := median * board.PositioningMultiplier
	if diff := r.PricePer1K - want; diff > 1e-12 || diff < -1e-12 {
		t.Fatalf("price = %.12f, want weighted-median×mult = %.12f", r.PricePer1K, want)
	}
}

func TestCatalogueScheduleDigestBindsBoardPolicyAndEveryResult(t *testing.T) {
	pinBoardClockForPublication(t)
	schedule, err := BuildCataloguePriceSchedule()
	if err != nil {
		t.Fatal(err)
	}
	if err := validateCataloguePriceSchedule(schedule); err != nil {
		t.Fatalf("valid schedule rejected: %v", err)
	}
	tests := []struct {
		name   string
		mutate func(*CataloguePriceSchedule)
	}{
		{"version", func(s *CataloguePriceSchedule) { s.Version++ }},
		{"reference currency", func(s *CataloguePriceSchedule) { s.ReferenceCurrency = "cad" }},
		{"settlement currency", func(s *CataloguePriceSchedule) { s.SettlementCurrency = "cad" }},
		{"FX rate", func(s *CataloguePriceSchedule) { s.ReferenceToSettlement += 0.01 }},
		{"FX revision", func(s *CataloguePriceSchedule) { s.FXRevision += "-changed" }},
		{"board digest", func(s *CataloguePriceSchedule) { s.BoardSHA256 = strings.Repeat("f", 64) }},
		{"board fetch", func(s *CataloguePriceSchedule) { s.BoardFetchedAt += "-changed" }},
		{"positioning", func(s *CataloguePriceSchedule) { s.PositioningMultiplier += 0.01 }},
		{"supplier share policy", func(s *CataloguePriceSchedule) { s.SupplierSharePolicyRevision += "-changed" }},
		{"workload supplier share", func(s *CataloguePriceSchedule) { s.Results[0].SupplierShare -= 0.01 }},
		{"model price", func(s *CataloguePriceSchedule) { s.Results[0].PricePer1K *= 2 }},
		{"formula", func(s *CataloguePriceSchedule) { s.Results[0].Formula += " changed" }},
		{"missing model", func(s *CataloguePriceSchedule) { s.Results = s.Results[:1] }},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			mutant := schedule
			mutant.Results = append([]RepriceResult(nil), schedule.Results...)
			tc.mutate(&mutant)
			if err := validateCataloguePriceSchedule(mutant); err == nil {
				t.Fatalf("%s mutation survived schedule validation", tc.name)
			}
		})
	}
}

func TestPublishedCatalogueCarriesDistinctPhysicalWorkloadShares(t *testing.T) {
	pinBoardClockForPublication(t)
	schedule, err := BuildCataloguePriceSchedule()
	if err != nil {
		t.Fatal(err)
	}
	if schedule.SupplierShare != 0 || schedule.SupplierSharePolicyRevision != supplierSharePolicyRevision {
		t.Fatalf("schedule still carries a global supplier share: %+v", schedule)
	}
	seen := map[float64]bool{}
	for _, result := range schedule.Results {
		want := supplierShareForTest(t, result.JobType, result.ModelID)
		if result.SupplierShare != want {
			t.Fatalf("%s/%s supplier share=%v want workload policy %v",
				result.JobType, result.ModelID, result.SupplierShare, want)
		}
		if !strings.Contains(result.Formula, "supplier_share_policy="+supplierSharePolicyRevision) {
			t.Fatalf("%s formula lacks policy provenance: %s", result.ModelID, result.Formula)
		}
		seen[result.SupplierShare] = true
	}
	if len(seen) < 2 {
		t.Fatalf("all %d physical catalogue rows share one supplier percentage: %v",
			len(schedule.Results), seen)
	}
}

func TestCatalogueScheduleRequiresExplicitCrossCurrencyFX(t *testing.T) {
	pinBoardClockForPublication(t)
	installSettlementCurrencyForTest(t, "cad")
	t.Setenv(priceFXRateEnv, "")
	t.Setenv(priceFXRevisionEnv, "")
	if _, err := BuildCataloguePriceSchedule(); err == nil ||
		!strings.Contains(err.Error(), priceFXRateEnv) ||
		!strings.Contains(err.Error(), priceFXRevisionEnv) {
		t.Fatalf("cross-currency schedule without FX authority error=%v", err)
	}

	t.Setenv(priceFXRateEnv, "1.375")
	t.Setenv(priceFXRevisionEnv, "operator-approved-2026-07-28T1400Z")
	schedule, err := BuildCataloguePriceSchedule()
	if err != nil {
		t.Fatal(err)
	}
	if schedule.ReferenceCurrency != "usd" ||
		schedule.SettlementCurrency != "cad" ||
		schedule.ReferenceToSettlement != 1.375 ||
		schedule.FXRevision != "operator-approved-2026-07-28T1400Z" {
		t.Fatalf("FX authority not bound into schedule: %+v", schedule)
	}
	for _, result := range schedule.Results {
		want := ceilPricePer1K(result.ReferencePricePer1K * schedule.ReferenceToSettlement)
		if math.Abs(result.PricePer1K-want) > 1e-12 {
			t.Fatalf("%s settlement price=%v want=%v", result.ModelID, result.PricePer1K, want)
		}
		for _, authority := range []string{
			"reference_currency=usd",
			"settlement_currency=cad",
			"fx_revision=" + schedule.FXRevision,
		} {
			if !strings.Contains(result.Formula, authority) {
				t.Fatalf("%s formula missing %q: %s", result.ModelID, authority, result.Formula)
			}
		}
	}

	// Even an attacker who recomputes the outer digest cannot change the FX
	// rate without also changing every converted price and its formula.
	mutant := schedule
	mutant.ReferenceToSettlement = 1.5
	mutant.SHA256, err = cataloguePriceScheduleDigest(mutant)
	if err != nil {
		t.Fatal(err)
	}
	if err := validateCataloguePriceSchedule(mutant); err == nil ||
		!strings.Contains(err.Error(), "inconsistent with FX") {
		t.Fatalf("internally inconsistent recomputed FX schedule accepted: %v", err)
	}
}

func TestBuyerSettlementPriceAndWorkerUSDFloorStayDistinct(t *testing.T) {
	installSettlementCurrencyForTest(t, "cad")
	model := ModelRow{
		ID:         "currency-bound-model",
		PricePer1K: 1.375, PriceCurrency: "cad",
		ReferencePricePer1K: 1, PriceReferenceCurrency: "usd",
	}
	buyerPrice, err := modelPrice(model)
	if err != nil {
		t.Fatal(err)
	}
	workerFloorPrice, err := modelReferencePriceUSD(model)
	if err != nil {
		t.Fatal(err)
	}
	if buyerPrice != 1.375 || workerFloorPrice != 1 {
		t.Fatalf("buyer price=%v CAD worker reference=%v USD", buyerPrice, workerFloorPrice)
	}
	model.PriceCurrency = "usd"
	if _, err := modelPrice(model); err == nil {
		t.Fatal("USD buyer price accepted under CAD settlement")
	}
}

func TestCostFloorExceedsMarketBoard(t *testing.T) {
	// Document the economic finding: laptop cost-plus at $2/hr cannot meet
	// observed hyperscaler list prices for these model classes.
	gaps := CompareCostFloorToMarketBoard(0.97)
	if len(gaps) == 0 {
		t.Fatal("expected cost/market comparison rows")
	}
	for _, g := range gaps {
		if g.GapRatio <= 1 {
			t.Fatalf("%s: expected cost-plus floor above market board (gap_ratio=%v cost=%v market=%v)",
				g.ModelID, g.GapRatio, g.CostPlusPer1K, g.MarketBoardPer1K)
		}
	}
}

func TestFinalizeCostDriftRowNamesBasisAndFailsClosed(t *testing.T) {
	row := finalizeCostDriftRow(CostDriftRow{
		JobType:      "batch_infer",
		ModelRef:     "test-model",
		Samples:      1000,
		AvgQuotedUSD: 1.00,
		AvgActualUSD: 1.20,
	})
	if diff := row.DriftRatio - 1.20; diff > 1e-9 || diff < -1e-9 {
		t.Fatalf("drift ratio = %v, want 1.2", row.DriftRatio)
	}
	if diff := row.DriftPct - 20.0; diff > 1e-9 || diff < -1e-9 {
		t.Fatalf("drift pct = %v, want 20", row.DriftPct)
	}
	if row.ActualUSDBasis != actualUSDBasisQuoteDerivedSettlement {
		t.Fatalf("actual_usd basis = %q, want %q", row.ActualUSDBasis, actualUSDBasisQuoteDerivedSettlement)
	}
	if row.UsingForTuning {
		t.Fatal("quote-derived settlement must fail closed even with 1,000 samples")
	}
	if row.TuningBlockReason != priceTuningBlockedNoIndependentTelemetry {
		t.Fatalf("tuning block reason = %q, want %q", row.TuningBlockReason, priceTuningBlockedNoIndependentTelemetry)
	}
	encoded, err := json.Marshal(row)
	if err != nil {
		t.Fatalf("marshal admin row: %v", err)
	}
	for _, want := range []string{
		`"actual_usd_basis":"quote_derived_per_task_buyer_charge_settlement"`,
		`"using_for_tuning":false`,
		`"tuning_block_reason":"independent_execution_cost_telemetry_unavailable"`,
	} {
		if !strings.Contains(string(encoded), want) {
			t.Fatalf("admin row JSON missing %s: %s", want, encoded)
		}
	}
}
