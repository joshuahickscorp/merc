package main

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestRepriceFromSupplierEconomicsMathIsCorrect(t *testing.T) {
	b := measuredThroughput{
		ModelID: "test-model", JobType: "embed", UnitsPerSec: 1000.0, HWClass: "apple_silicon_pro",
		SourceCitation: "test",
	}
	got := repriceFromSupplierEconomics(b, 0.97, 0.10)
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

func TestRepriceFromSupplierEconomicsHigherThroughputMeansLowerPrice(t *testing.T) {
	slow := measuredThroughput{ModelID: "slow", JobType: "batch_infer", UnitsPerSec: 100, HWClass: "apple_silicon_pro"}
	fast := measuredThroughput{ModelID: "fast", JobType: "batch_infer", UnitsPerSec: 1000, HWClass: "apple_silicon_pro"}
	slowPrice := repriceFromSupplierEconomics(slow, 0.97, 0.15).PricePer1K
	fastPrice := repriceFromSupplierEconomics(fast, 0.97, 0.15).PricePer1K
	if fastPrice >= slowPrice {
		t.Fatalf("10x throughput should reprice to a materially lower price_per_1k: slow=%.8f fast=%.8f", slowPrice, fastPrice)
	}
}

func TestRepriceFromSupplierEconomicsUnknownHWClassFallsBackConservatively(t *testing.T) {
	b := measuredThroughput{ModelID: "m", JobType: "embed", UnitsPerSec: 500, HWClass: "some_future_chip"}
	got := repriceFromSupplierEconomics(b, 0.97, 0.15)
	if got.PricePer1K <= 0 {
		t.Fatalf("unknown hw_class should still yield a positive price, got %v", got.PricePer1K)
	}
	if !strings.Contains(got.Formula, "30W") {
		t.Fatalf("unknown hw_class should fall back to the 30W conservative default, formula: %s", got.Formula)
	}
}

func TestRepriceCatalogueFromSupplierEconomicsOmitsUnmeasuredModels(t *testing.T) {
	results := RepriceCatalogueFromSupplierEconomics(0.97)
	if len(results) == 0 {
		t.Fatal("expected at least the two really-measured models")
	}
	seen := map[string]bool{}
	for _, r := range results {
		seen[r.ModelID] = true
		if r.PricePer1K <= 0 {
			t.Fatalf("repriced model %s has non-positive price %v", r.ModelID, r.PricePer1K)
		}
	}
	if !seen["all-minilm-l6-v2"] || !seen["llama-3.2-1b-instruct-q4"] {
		t.Fatalf("expected the two really-measured models in the result, got %v", seen)
	}
	if seen["unsupported-model"] {
		t.Fatal("unmeasured model must never be repriced")
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
