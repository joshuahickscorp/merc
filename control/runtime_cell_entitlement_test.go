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
	// Current placement binds the exact receipt-measured execution build and
	// device; only legacy placements retain the explicit UNKNOWN shape.
	if f.BuildIdentity.Knowledge != fieldProvenanceMeasured {
		t.Fatalf("build identity knowledge=%q, want measured exact authority",
			f.BuildIdentity.Knowledge)
	}
	if !strings.Contains(f.BuildIdentity.Basis, placement.EngineBuildHash) ||
		!strings.Contains(f.BuildIdentity.Basis, placement.HardwareIdentity) ||
		!strings.Contains(f.BuildIdentity.Source, placement.PerformanceAuthority.BenchmarkSnapshotSHA256) {
		t.Fatalf("build identity does not bind placement build/device/summary: %+v", f.BuildIdentity)
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
	if f.PlatformDeliveryCostStatus != platformDeliveryComplete {
		t.Fatalf("platform delivery leg status=%q, want %q",
			f.PlatformDeliveryCostStatus, platformDeliveryComplete)
	}
	if f.PlatformDeliveryCostStatus == frozenVOCostComplete {
		t.Fatal("three-leg platform subtotal claims every named economics term is modeled")
	}
	if f.ExpectedVOCostStatus != frozenVOCostPartial {
		t.Fatalf("named cost stack status=%q, want partial while reliability and other terms are unknown",
			f.ExpectedVOCostStatus)
	}
	if (f.EnergyKnowledge == string(wattKindAssumed) ||
		f.EnergyKnowledge == string(wattKindVendorWallUpperBound)) &&
		f.EnergyPartial.Status != pricingCostUnknown {
		t.Fatalf("non-MEASURED watts entered canonical platform money: %+v", f.EnergyPartial)
	}
	if f.EnergyKnowledge == string(wattKindAssumed) && f.EnergyJoules <= 0 {
		t.Fatalf("assumed energy lost its non-authoritative diagnostic geometry: %+v", f)
	}
	if f.ReliabilityCost.Status != pricingCostUnknown ||
		!strings.Contains(f.ReliabilityCost.Basis, "unpaid") ||
		strings.Contains(f.ReliabilityCost.Basis, "further supplier entitlement") {
		t.Fatalf("retry overhead misstates unpaid failures as supplier liability: %+v",
			f.ReliabilityCost)
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

// The A40 receipt proves the rate×duration arithmetic for that exact provider
// allocation, but the accepted nvidia_48gb class is memory-only. Canonical
// pricing must keep provider cost unknown until placement freezes provider/SKU.
func TestProviderDurationFormulaCannotAuthorizeGenericCapacityTier(t *testing.T) {
	_, _, _, _, pricing := distributedPricingFixture(t)
	catalogue := pricing.Catalogue
	rate, ok := providerRatesByHWClass["nvidia_48gb"]
	if !ok || rate.CostPerHrUSD <= 0 {
		t.Fatal("A40 reference rate missing")
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

	// The arithmetic observation cannot become a canonical platform-cost value
	// for a placement that says only "some 48GB NVIDIA card".
	slowProvider := providerCostComponentForPlacement(
		"vllm-cuda-llama1-infer", []string{"nvidia_48gb"}, slowSec, catalogue,
	)
	fastProvider := providerCostComponentForPlacement(
		"vllm-cuda-llama1-infer", []string{"nvidia_48gb"}, fastSec, catalogue,
	)
	if slowProvider.Status != pricingCostUnknown || fastProvider.Status != pricingCostUnknown ||
		slowProvider.Amount != 0 || fastProvider.Amount != 0 {
		t.Fatalf("generic 48GB class inherited exact-A40 provider money: slow=%+v fast=%+v",
			slowProvider, fastProvider)
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

// Selector projections obey the same provider/SKU refusal as canonical pricing.
func TestSelectorPlatformDeliveryRefusesGenericCapacityTier(t *testing.T) {
	// Use a real cloud-backed cell id from the authority.
	cloud, found := cellIsCloudBacked("vllm-cuda-llama1-infer")
	if !found || !cloud {
		t.Skip("vllm-cuda-llama1-infer not cloud-backed in this build")
	}
	cat := CataloguePriceAuthority{
		ModelID: "meta-llama/Llama-3.2-1B-Instruct", JobType: "batch_infer",
		ReferencePricePer1K: 0.01, SupplierShare: 0.97,
	}
	slow := MeasuredSupplierLiabilityProxy{
		CellID: "vllm-cuda-llama1-infer", RuntimeID: "vllm_cuda", Engine: "vllm",
		JobType: "batch_infer", ModelRef: cat.ModelID, HWClass: "nvidia_48gb",
		Samples: minSupplierLiabilitySamples, Units: 1000, MedianMsPerUnit: 10.0, // 100 u/s
		SupplierUSDPerUnit:            0.0000097,
		VerificationSamples:           minSupplierLiabilitySamples,
		TerminalAttempts:              minSupplierLiabilitySamples,
		Measured:                      true,
		UnknownPlatformCostComponents: unknownPlatformCostComponents(),
	}
	fast := slow
	fast.MedianMsPerUnit = 10.0 / 1.62 // 1.62× faster

	ps := ProjectCellEconomics(slow, cat, "batch")
	pf := ProjectCellEconomics(fast, cat, "batch")

	// Supplier verified-outcome still ties (cancellation + equal reliability).
	if !SupplierLiabilityProxiesTie(ps, pf) {
		t.Fatalf("supplier verified-outcome should still tie: slow=%v fast=%v",
			ps.SupplierLiabilityUSDPerVerifiedUnit, pf.SupplierLiabilityUSDPerVerifiedUnit)
	}
	if ps.ProviderCost.Knowledge != CategoryUnknown || pf.ProviderCost.Knowledge != CategoryUnknown ||
		ps.PlatformDeliveryOK || pf.PlatformDeliveryOK {
		t.Fatalf("selector converted memory-tier identity into provider/SKU money: slow=%+v fast=%+v", ps, pf)
	}
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
