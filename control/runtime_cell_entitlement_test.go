package main

import (
	"math"
	"reflect"
	"strings"
	"testing"
)

// TestCellEntitlementResolutionIsWrittenOnNewAdmission pins that every new
// freeze carries cell_resolved_platform_v1 and the model contract identity.
func TestCellEntitlementResolutionIsWrittenOnNewAdmission(t *testing.T) {
	_, _, placement, _, pricing := distributedPricingFixture(t)
	f := pricing.RuntimeCell
	if f == nil {
		t.Fatal("new admission froze no runtime-cell economics")
	}
	if f.EntitlementResolution != cellEntitlementResolutionCellResolved {
		t.Fatalf("entitlement resolution = %q, want %q",
			f.EntitlementResolution, cellEntitlementResolutionCellResolved)
	}
	if f.ModelContract.ModelID == "" || f.ModelContract.JobType == "" {
		t.Fatalf("model contract identity missing: %+v", f.ModelContract)
	}
	if f.ModelContract.ModelID != pricing.Catalogue.ModelID ||
		f.ModelContract.JobType != pricing.Catalogue.JobType {
		t.Fatalf("model contract does not match catalogue: %+v vs %+v",
			f.ModelContract, pricing.Catalogue)
	}
	if f.CellID != placement.RuntimeCellID {
		t.Fatalf("cell id %q != placement %q", f.CellID, placement.RuntimeCellID)
	}
	// Provenances must be visible and non-empty.
	for name, p := range map[string]EconomicsFieldProvenance{
		"throughput": f.ThroughputProvenance,
		"duration":   f.DurationProvenance,
		"supplier":   f.SupplierProvenance,
		"buyer":      f.BuyerProvenance,
		"build":      f.BuildIdentity,
	} {
		if strings.TrimSpace(p.Knowledge) == "" || strings.TrimSpace(p.Basis) == "" {
			t.Fatalf("%s provenance incomplete: %+v", name, p)
		}
	}
	// Build identity is an honest gap today.
	if f.BuildIdentity.Knowledge != fieldProvenanceUnknown {
		t.Fatalf("build identity claims %q; placement binds no build_digest",
			f.BuildIdentity.Knowledge)
	}
	if !strings.Contains(f.BuildIdentity.WouldRequire, "build_digest") {
		t.Fatalf("build identity must say what would be required: %+v", f.BuildIdentity)
	}
	// Platform delivery must be populated under cell resolution.
	if f.PlatformDeliveryCostStatus == "" || f.PlatformDeliveryCostBasis == "" {
		t.Fatalf("platform delivery incomplete: status=%q basis=%q",
			f.PlatformDeliveryCostStatus, f.PlatformDeliveryCostBasis)
	}
	if f.PlatformDeliveryCostUSD <= 0 {
		t.Fatalf("platform delivery total %v; supplier leg should make it positive",
			f.PlatformDeliveryCostUSD)
	}
}

// TestLegacyDecisionWithoutCellBlockSettlesIdentically is half of the gate:
// a decision frozen before cell resolution (RuntimeCell == nil) settles on the
// same buyer/supplier amounts as the modern decision that carries a cell block.
// Cell resolution must not rewrite supplier settlement.
func TestLegacyDecisionWithoutCellBlockSettlesIdentically(t *testing.T) {
	workload, compute, placement, economic, modern := distributedPricingFixture(t)
	legacy := modern
	legacy.RuntimeCell = nil

	// Snapshot validation still accepts the legacy shape.
	if err := ValidateDistributedPricingDecisionSnapshot(
		legacy, workload, compute, placement, economic,
	); err != nil {
		t.Fatalf("legacy decision refused: %v", err)
	}
	if !isLegacyModelLevelEntitlement(legacy) {
		t.Fatal("nil RuntimeCell must count as model-level entitlement")
	}
	if isLegacyModelLevelEntitlement(modern) {
		t.Fatal("new admission with cell_resolved must not count as legacy")
	}

	// Settlement reads only frozen amounts: legacy and modern agree on money.
	lb, ls, le, err := settledAmountsFromFrozenDecision(legacy)
	if err != nil {
		t.Fatal(err)
	}
	mb, ms, me, err := settledAmountsFromFrozenDecision(modern)
	if err != nil {
		t.Fatal(err)
	}
	if lb != mb || ls != ms {
		t.Fatalf("settlement amounts diverged: legacy buyer=%d supplier=%d; modern buyer=%d supplier=%d",
			lb, ls, mb, ms)
	}
	if modern.FixedPoint != nil {
		if lb != modern.FixedPoint.BuyerChargeNanos ||
			ls != modern.FixedPoint.SupplierEntitlementsNanos {
			t.Fatalf("settled amounts %d/%d do not match fixed-point %d/%d",
				lb, ls, modern.FixedPoint.BuyerChargeNanos, modern.FixedPoint.SupplierEntitlementsNanos)
		}
	} else {
		// Float path: buyer from BuyerPrice, supplier from PrimarySupplierCost
		// or SupplierGrossNanos. Both legs must be positive.
		if lb <= 0 || ls < 0 {
			t.Fatalf("settled amounts non-positive: buyer=%d supplier=%d", lb, ls)
		}
	}
	// Modern evidence is richer; legacy evidence names the absence.
	if !containsSubstring(le, "runtime_cell:absent_legacy_decision") {
		t.Fatalf("legacy evidence missing absence marker: %v", le)
	}
	if !containsSubstring(me, "frozen_runtime_cell_digest:") {
		t.Fatalf("modern evidence missing frozen digest: %v", me)
	}
	// Primary supplier cost is identical either way — cell resolution does not
	// rewrite the supplier leg of the decision.
	if modern.PrimarySupplierCost != legacy.PrimarySupplierCost {
		t.Fatal("cell resolution rewrote primary supplier cost")
	}
	if modern.SupplierGrossNanos != legacy.SupplierGrossNanos {
		t.Fatal("cell resolution rewrote supplier gross nanos")
	}
}

// TestSettledAmountUnchangedWhenBetterBenchmarkPublished is the freeze proof
// the directive asks for: accept a decision, publish a better benchmark for the
// same cell, re-resolve settlement, assert the settled amount is unchanged and
// still traceable to the frozen evidence identity.
//
// This is not a permission to reprice. settledAmountsFromFrozenDecision is the
// only settlement view used here; it never re-reads a live benchmark.
func TestSettledAmountUnchangedWhenBetterBenchmarkPublished(t *testing.T) {
	workload, compute, placement, economic, settled := distributedPricingFixture(t)
	if settled.RuntimeCell == nil {
		t.Fatal("fixture froze no runtime-cell economics")
	}
	beforeBuyer, beforeSupplier, beforeEvidence, err := settledAmountsFromFrozenDecision(settled)
	if err != nil {
		t.Fatal(err)
	}
	beforeDigest := settled.RuntimeCell.Digest
	beforeCell := *settled.RuntimeCell
	beforeBuyerPrice := settled.BuyerPrice
	beforeSupplierUSD := settled.PrimarySupplierCost.Amount

	// Publish a better benchmark: 1.62× faster (the candle→llama cohort ratio).
	// A NEW admission at that rate would freeze different economics; the settled
	// decision must not move.
	faster := settled.ExpectedSupplierUnitsPerSec * 1.62
	fasterPlacement := placement
	fasterCeiling := expectedSupplierUSDHr(
		faster, settled.Catalogue.ReferencePricePer1K,
		settled.Catalogue.SupplierShare, settled.Tier)
	fasterPlacement.OfferedRateUsdHr = float32(fasterCeiling)
	revalidated, err := distributedPricingDecisionAtRate(
		workload, compute, fasterPlacement, economic, settled.Catalogue,
		settled.Tier, "", faster,
	)
	if err != nil {
		t.Fatalf("rebuild at revalidated rate: %v", err)
	}
	if revalidated.RuntimeCell == nil {
		t.Fatal("revalidated decision froze no cell block")
	}
	if revalidated.RuntimeCell.Digest == beforeDigest {
		t.Fatal("1.62× revalidation produced identical freeze; proof would be vacuous")
	}
	if revalidated.ExpectedSupplierSeconds >= settled.ExpectedSupplierSeconds {
		t.Fatalf("revalidation did not shorten expected duration: %v -> %v",
			settled.ExpectedSupplierSeconds, revalidated.ExpectedSupplierSeconds)
	}

	// Re-resolve settlement against the SETTLED decision (not the revalidated
	// one). Amounts and evidence identity must be unchanged.
	afterBuyer, afterSupplier, afterEvidence, err := settledAmountsFromFrozenDecision(settled)
	if err != nil {
		t.Fatal(err)
	}
	if afterBuyer != beforeBuyer || afterSupplier != beforeSupplier {
		t.Fatalf("settled amounts moved under revalidation: buyer %d→%d supplier %d→%d",
			beforeBuyer, afterBuyer, beforeSupplier, afterSupplier)
	}
	if !reflect.DeepEqual(beforeEvidence, afterEvidence) {
		t.Fatalf("evidence identity moved:\n  before=%v\n  after=%v", beforeEvidence, afterEvidence)
	}
	if !reflect.DeepEqual(beforeCell, *settled.RuntimeCell) {
		t.Fatal("settled RuntimeCell block was mutated by revalidation")
	}
	if settled.BuyerPrice != beforeBuyerPrice ||
		settled.PrimarySupplierCost.Amount != beforeSupplierUSD {
		t.Fatal("settled money fields moved")
	}
	if settled.FixedPoint != nil &&
		(settled.FixedPoint.BuyerChargeNanos != beforeBuyer ||
			settled.FixedPoint.SupplierEntitlementsNanos != beforeSupplier) {
		t.Fatal("fixed-point money moved")
	}
	// Evidence still names the frozen cell digest.
	if !containsSubstring(afterEvidence, "frozen_runtime_cell_digest:"+beforeDigest) {
		t.Fatalf("settlement evidence lost frozen digest: %v", afterEvidence)
	}
	t.Logf("FREEZE PROOF: settled buyer_nanos=%d supplier_nanos=%d digest=%s; "+
		"revalidated expected_seconds=%.6f (was %.6f) platform_delivery=$%.8f (was $%.8f) — settled amounts unchanged",
		afterBuyer, afterSupplier, beforeDigest,
		revalidated.ExpectedSupplierSeconds, settled.ExpectedSupplierSeconds,
		revalidated.RuntimeCell.PlatformDeliveryCostUSD,
		beforeCell.PlatformDeliveryCostUSD)
}

// TestCellResolvedProviderCostDiffersByDuration is the other half of the gate:
// under cell_resolved_platform_v1, a faster cell produces a genuinely lower
// provider cost and therefore a lower platform delivery cost. Supplier
// entitlement (cancelled form) stays identical for equal units.
func TestCellResolvedProviderCostDiffersByDuration(t *testing.T) {
	rate, ok := providerRatesByHWClass["nvidia_80gb"]
	if !ok || rate.CostPerHrUSD <= 0 {
		t.Fatal("nvidia_80gb governed rate missing")
	}
	// Same units, two throughputs: 100 u/s vs 162 u/s (1.62×).
	const units = 1000.0
	slowSec := units / 100.0
	fastSec := units / 162.0

	delta, differs, err := providerCostDiffersByDuration(rate.CostPerHrUSD, slowSec, fastSec)
	if err != nil {
		t.Fatal(err)
	}
	if !differs {
		t.Fatal("provider cost did not differ across a 1.62× throughput gap")
	}
	if delta <= 0 {
		t.Fatalf("slow cell should cost more provider dollars; delta=%v", delta)
	}

	// Platform delivery: equal supplier, different provider → different total.
	supplier := 0.01 // cancelled form, identical
	slowProvider := providerCostComponentForPlacement(
		"vllm-cuda-llama1-infer", []string{"nvidia_80gb"}, slowSec,
	)
	fastProvider := providerCostComponentForPlacement(
		"vllm-cuda-llama1-infer", []string{"nvidia_80gb"}, fastSec,
	)
	if slowProvider.Status != pricingCostModeled || fastProvider.Status != pricingCostModeled {
		t.Fatalf("provider not modeled: slow=%+v fast=%+v", slowProvider, fastProvider)
	}
	ver := modeledCost(0.001, "test verification")
	slowDel := resolveCellPlatformDelivery(supplier, slowProvider, ver, units,
		cellEntitlementResolutionCellResolved)
	fastDel := resolveCellPlatformDelivery(supplier, fastProvider, ver, units,
		cellEntitlementResolutionCellResolved)

	if slowDel.SupplierUSD != fastDel.SupplierUSD {
		t.Fatalf("supplier leg diverged: slow=%v fast=%v (cancellation broken)",
			slowDel.SupplierUSD, fastDel.SupplierUSD)
	}
	if !(fastDel.TotalUSD < slowDel.TotalUSD) {
		t.Fatalf("faster cell platform delivery $%v is not lower than slow $%v",
			fastDel.TotalUSD, slowDel.TotalUSD)
	}
	if !(fastDel.ProviderUSD < slowDel.ProviderUSD) {
		t.Fatalf("faster cell provider $%v is not lower than slow $%v",
			fastDel.ProviderUSD, slowDel.ProviderUSD)
	}
	// Absolute delta, not only a ratio.
	absDelta := slowDel.TotalUSD - fastDel.TotalUSD
	t.Logf("CELL-LEVEL ENTITLEMENT: supplier=$%.6f (identical); "+
		"provider slow=$%.6f fast=$%.6f; platform delivery slow=$%.6f fast=$%.6f; "+
		"absolute saving $%.6f (%.3f ms/unit-equivalent at 1000 units)",
		supplier, slowDel.ProviderUSD, fastDel.ProviderUSD,
		slowDel.TotalUSD, fastDel.TotalUSD, absDelta,
		(slowSec-fastSec)/units*1000)

	// True-net direction: higher provider → lower true net, all else equal.
	// Buyer and supplier fixed; provider is the only variable.
	buyer := 0.05
	slowTrueNet := buyer - supplier - slowDel.ProviderUSD - ver.Amount
	fastTrueNet := buyer - supplier - fastDel.ProviderUSD - ver.Amount
	if !(fastTrueNet > slowTrueNet) {
		t.Fatalf("faster cell true net $%v is not higher than slow $%v",
			fastTrueNet, slowTrueNet)
	}
}

// TestCellResolvedCommunitySupplyPlatformDeliveryEqualsSupplier pins that on
// owned/community metal (provider N/A), platform delivery collapses to the
// supplier leg — so equal-reliability cells still tie on cost. The latency win
// remains a latency win, not a fabricated cost win.
func TestCellResolvedCommunitySupplyPlatformDeliveryEqualsSupplier(t *testing.T) {
	_, _, _, _, pricing := distributedPricingFixture(t)
	f := pricing.RuntimeCell
	if f == nil {
		t.Fatal("no freeze")
	}
	// Community fixture: provider not applicable.
	if f.ProviderCost.Status != pricingCostNotApplicable {
		t.Fatalf("fixture provider status=%s; want not_applicable for community supply",
			f.ProviderCost.Status)
	}
	// Platform delivery = supplier + verification (both modeled), no provider.
	want := f.SupplierEntitlementUSD
	if f.VerificationCost.Status == pricingCostModeled {
		want += f.VerificationCost.Amount
	}
	if math.Abs(f.PlatformDeliveryCostUSD-roundEconomicUSD(want)) > 1e-12 {
		t.Fatalf("platform delivery $%v != supplier+verification $%v",
			f.PlatformDeliveryCostUSD, want)
	}
}

// TestSelectorPlatformDeliveryDiffersWhenProviderKnown shows the selector
// projection is no longer blind on cloud-backed cells: platform delivery
// includes provider and tracks duration.
func TestSelectorPlatformDeliveryDiffersWhenProviderKnown(t *testing.T) {
	// Use a real cloud-backed cell id from the authority.
	cloud, found := cellIsCloudBacked("vllm-cuda-llama1-infer")
	if !found || !cloud {
		t.Skip("vllm-cuda-llama1-infer not cloud-backed in this build")
	}
	cat := CataloguePriceAuthority{
		ModelID: "meta-llama/Llama-3.2-1B-Instruct", JobType: "batch_infer",
		ReferencePricePer1K: 0.01, SupplierShare: 0.97,
	}
	slow := MeasuredCellCost{
		CellID: "vllm-cuda-llama1-infer", RuntimeID: "vllm_cuda", Engine: "vllm",
		JobType: "batch_infer", ModelRef: cat.ModelID, HWClass: "nvidia_80gb",
		Samples: minCellCostSamples, Units: 1000, MedianMsPerUnit: 10.0, // 100 u/s
		SupplierUSDPerUnit: 0.0000097, VerificationSamples: 20, Measured: true,
		Unknown: unknownCostComponents(),
	}
	fast := slow
	fast.MedianMsPerUnit = 10.0 / 1.62 // 1.62× faster

	ps := ProjectCellEconomics(slow, cat, "batch")
	pf := ProjectCellEconomics(fast, cat, "batch")

	// Supplier verified-outcome still ties (cancellation + equal reliability).
	if !VerifiedOutcomeCostsTie(ps, pf) {
		t.Fatalf("supplier verified-outcome should still tie: slow=%v fast=%v",
			ps.VerifiedOutcomeUSDPerUnit, pf.VerifiedOutcomeUSDPerUnit)
	}
	// Platform delivery must see the provider difference when provider is known.
	if ps.ProviderCost.Knowledge != CategoryKnown && ps.ProviderCost.Knowledge != CategoryAssumed {
		// nvidia_80gb is governed without DEFECT; if rate missing, skip honestly.
		if ps.ProviderCost.Knowledge == CategoryUnknown {
			t.Fatalf("provider unknown for cloud cell: %+v", ps.ProviderCost)
		}
	}
	if !ps.PlatformDeliveryOK || !pf.PlatformDeliveryOK {
		t.Fatalf("platform delivery not ok: slow=%v (%s) fast=%v (%s)",
			ps.PlatformDeliveryOK, ps.PlatformDeliveryBasis,
			pf.PlatformDeliveryOK, pf.PlatformDeliveryBasis)
	}
	if !(pf.PlatformDeliveryUSDPerUnit < ps.PlatformDeliveryUSDPerUnit) {
		t.Fatalf("faster cell platform delivery $%v is not lower than slow $%v",
			pf.PlatformDeliveryUSDPerUnit, ps.PlatformDeliveryUSDPerUnit)
	}
	// Absolute delta.
	t.Logf("selector platform delivery: slow=$%.10f/unit fast=$%.10f/unit delta=$%.10f; "+
		"supplier VO tied at $%.10f/unit",
		ps.PlatformDeliveryUSDPerUnit, pf.PlatformDeliveryUSDPerUnit,
		ps.PlatformDeliveryUSDPerUnit-pf.PlatformDeliveryUSDPerUnit,
		ps.VerifiedOutcomeUSDPerUnit)
}

// TestNoHistoricalRepricePathFromFrozenDecision greps the settlement view:
// settledAmountsFromFrozenDecision must not call admissionUnitsPerSec or any
// live benchmark resolver. This is a behavioural pin: feeding a decision whose
// ExpectedSupplierUnitsPerSec is nonsense still returns the frozen fixed-point.
func TestNoHistoricalRepricePathFromFrozenDecision(t *testing.T) {
	_, _, _, _, settled := distributedPricingFixture(t)
	wantB, wantS, _, err := settledAmountsFromFrozenDecision(settled)
	if err != nil {
		t.Fatal(err)
	}
	// Poison the live-looking rate fields. Settlement must ignore them.
	mutant := settled
	mutant.ExpectedSupplierUnitsPerSec = 1e9
	mutant.ExpectedSupplierSeconds = 0.000001
	mutant.SupplierAdmissionCeilingUSDHr = 9999
	gotB, gotS, _, err := settledAmountsFromFrozenDecision(mutant)
	if err != nil {
		t.Fatal(err)
	}
	if gotB != wantB || gotS != wantS {
		t.Fatalf("poisoned live rate fields moved settlement: buyer %d→%d supplier %d→%d",
			wantB, gotB, wantS, gotS)
	}
}

// TestPlatformDeliveryDigestIsCovered ensures the new fields participate in
// the freeze digest so a rewrite is detectable.
func TestPlatformDeliveryDigestIsCovered(t *testing.T) {
	_, _, _, _, pricing := distributedPricingFixture(t)
	f := pricing.RuntimeCell
	if f == nil {
		t.Fatal("no freeze")
	}
	for name, mutate := range map[string]func(*FrozenRuntimeCellEconomics){
		"entitlement_resolution": func(m *FrozenRuntimeCellEconomics) {
			m.EntitlementResolution = cellEntitlementResolutionModelLevel
		},
		"platform_delivery_total": func(m *FrozenRuntimeCellEconomics) {
			m.PlatformDeliveryCostUSD += 0.01
		},
		"platform_delivery_per_unit": func(m *FrozenRuntimeCellEconomics) {
			m.PlatformDeliveryCostUSDPerUnit += 0.01
		},
		"model_contract": func(m *FrozenRuntimeCellEconomics) {
			m.ModelContract.ModelID += "-tampered"
		},
		"supplier_provenance": func(m *FrozenRuntimeCellEconomics) {
			m.SupplierProvenance.Knowledge = fieldProvenanceMeasured
		},
	} {
		mutant := *f
		mutate(&mutant)
		got, err := digestFrozenRuntimeCellEconomics(&mutant)
		if err != nil {
			t.Fatal(err)
		}
		if got == f.Digest {
			t.Fatalf("digest is blind to %s", name)
		}
	}
}
