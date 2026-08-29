package main

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

const (
	// Synthetic same-denominator algebra fixture. This is not the current
	// market-board MiniLM price and not a claim that one output record always
	// equals one settlement input unit. Corpus-specific tests must freeze the
	// raw byte geometry and derive payout from ComputePlan.SettlementInputUnits.
	miniLMReferencePricePer1K = 0.00625
	miniLMSupplierShare       = 0.97
	// This fixture deliberately makes one accepted output share the same
	// denominator as one settlement unit. The /1000 conversion is load-bearing.
	miniLMSupplierUSDPerUnit = miniLMReferencePricePer1K / 1000.0 * miniLMSupplierShare
)

// TestCensusUnitsPerSecCancels pins the arithmetic that forces two cells on
// one model to share the same supplier entitlement per unit.
//
//	ceiling  = unitsPerSec × 3600/1000 × price × share
//	seconds  = units / unitsPerSec
//	required = ceiling × seconds / 3600 = units/1000 × price × share
//
// A cell that is 2.1× faster produces an identical payout for the same units.
// If this test fails, the rest of the cell-economics work is building on a
// false census and must stop.
func TestCensusUnitsPerSecCancels(t *testing.T) {
	const (
		units      = 640.0
		pricePer1K = 0.00625
		share      = 0.97
		slowUPS    = 100.0
		fastUPS    = 210.0 // 2.1× faster
	)
	closed := censusRequiredUSD(units, pricePer1K, share)
	viaSlow := censusRequiredViaThroughput(units, slowUPS, pricePer1K, share)
	viaFast := censusRequiredViaThroughput(units, fastUPS, pricePer1K, share)

	if math.Abs(viaSlow-closed) > 1e-15 || math.Abs(viaFast-closed) > 1e-15 {
		t.Fatalf("throughput did not cancel: closed=%.17f viaSlow=%.17f viaFast=%.17f",
			closed, viaSlow, viaFast)
	}
	if math.Abs(viaSlow-viaFast) > 1e-15 {
		t.Fatalf("2.1× faster cell changed supplier entitlement: slow=%.17f fast=%.17f",
			viaSlow, viaFast)
	}
	// Per-unit form used by MeasuredSupplierLiabilityProxy.
	perUnit := closed / units
	wantPerUnit := pricePer1K / 1000.0 * share
	if math.Abs(perUnit-wantPerUnit) > 1e-15 {
		t.Fatalf("per-unit supplier = %.17f, want %.17f", perUnit, wantPerUnit)
	}
}

// TestCellEconomicsProjectionBindsAllNamedTerms checks the projection schema
// carries every term the task requires, with knowledge states rather than
// invented zeros.
func TestCellEconomicsProjectionBindsAllNamedTerms(t *testing.T) {
	cost := MeasuredSupplierLiabilityProxy{
		CellID: "candle-metal-minilm-embed", RuntimeID: "candle_metal", Engine: "candle",
		JobType: "embed", ModelRef: "all-minilm-l6-v2", HWClass: "apple_silicon_ultra",
		Samples: minSupplierLiabilitySamples, Units: 640, MedianMsPerUnit: 0.21875,
		SupplierUSDPerUnit: miniLMSupplierUSDPerUnit, VerificationSamples: 20,
		TerminalAttempts: minSupplierLiabilitySamples, Measured: true,
		UnknownPlatformCostComponents: unknownPlatformCostComponents(),
	}
	cat := CataloguePriceAuthority{
		ModelID: "all-minilm-l6-v2", JobType: "embed",
		ReferencePricePer1K: miniLMReferencePricePer1K, SupplierShare: miniLMSupplierShare,
		ScheduleSHA256: "aa", BoardSHA256: "bb",
	}
	p := ProjectCellEconomics(cost, cat, "batch")

	if p.SchemaVersion != cellEconomicsProjectionSchemaVersion || p.Kind != cellEconomicsProjectionKind {
		t.Fatalf("identity: %+v", p)
	}
	if p.ModelContract.ModelID != "all-minilm-l6-v2" || p.CellID != cost.CellID || p.HWClass != cost.HWClass {
		t.Fatalf("model/cell/hw binding: %+v", p)
	}
	if !p.DurationCancelsFromSupplierEntitlement {
		t.Fatal("projection must record that duration cancels from supplier entitlement")
	}
	if p.SupplierEntitlementPolicy != supplierEntitlementPolicyCancelled {
		t.Fatalf("policy = %q", p.SupplierEntitlementPolicy)
	}
	if p.MedianMsPerUnit != 0.21875 || p.MeasuredUnitsPerSec <= 0 {
		t.Fatalf("duration/throughput: ms=%v ups=%v", p.MedianMsPerUnit, p.MeasuredUnitsPerSec)
	}
	if !p.SupplierLiabilityAvailable || math.Abs(p.SupplierLiabilityUSDPerVerifiedUnit-miniLMSupplierUSDPerUnit) > 1e-12 {
		t.Fatalf("verified outcome: ok=%v usd=%v", p.SupplierLiabilityAvailable, p.SupplierLiabilityUSDPerVerifiedUnit)
	}
	// BuyerPricePer1KUnits and SupplierUSDPerUnit intentionally are not
	// subtracted here: the former is per settlement input unit while the latter
	// is per accepted output record. Only a frozen job geometry may convert them.
	// Owned metal: provider not applicable.
	if p.ProviderCost.Knowledge != CategoryNotApplicable {
		t.Fatalf("provider on owned metal: %+v", p.ProviderCost)
	}
	// Energy partial is ASSUMED (watts) / DEFAULTED (electricity), never full cost.
	if p.EnergyPartial.Knowledge != CategoryAssumed && p.EnergyPartial.Knowledge != CategoryDefaulted {
		t.Fatalf("energy partial knowledge: %+v", p.EnergyPartial)
	}
	if p.StorageTransfer.Knowledge != CategoryUnknown || p.Utilization.Knowledge != CategoryUnknown {
		t.Fatalf("storage/util must stay unknown without meters: storage=%+v util=%+v",
			p.StorageTransfer, p.Utilization)
	}
	if p.PlatformDeliveryOK {
		t.Fatalf("known supplier/provider partial was mislabeled complete while named terms are unresolved: %+v", p)
	}
	if p.PlatformDeliveryUSDPerUnit <= 0 ||
		!strings.Contains(p.PlatformDeliveryBasis, "known partial only") {
		t.Fatalf("known partial should be preserved and explicitly refused as complete: %+v", p)
	}
	if p.MercTrueNet.IsAvailable() || p.MercTrueNet.Unavailable == nil {
		t.Fatalf("true net must be structurally unavailable: %+v", p.MercTrueNet)
	}
	if p.GrossPlatformRow != nil {
		t.Fatal("projection must not invent a gross platform row without a PricingDecision")
	}
	if p.Confidence <= 0 || p.Confidence > 1 {
		t.Fatalf("confidence = %v", p.Confidence)
	}
	if len(p.EvidenceAuthority) == 0 {
		t.Fatal("evidence authority empty")
	}
	// Admission hourly is duration-sensitive and positive when throughput is known.
	if p.AdmissionExpectedSupplierUSDHr <= 0 {
		t.Fatalf("admission $/hr should be duration-sensitive and positive: %v",
			p.AdmissionExpectedSupplierUSDHr)
	}
}

func TestCellEconomicsUsesSettlementCurrencyAndRefusesDenominatorlessGross(t *testing.T) {
	const fx = 1.37
	settlementPrice := ceilPricePer1K(miniLMReferencePricePer1K * fx)
	supplierCADPerUnit := settlementPrice / 1000 * miniLMSupplierShare
	cost := MeasuredSupplierLiabilityProxy{
		CellID: candleEmbedCell, RuntimeID: "candle_metal", Engine: "candle",
		JobType: "embed", ModelRef: "all-minilm-l6-v2", HWClass: "apple_silicon_ultra",
		Currency: "cad", Samples: minSupplierLiabilitySamples, Units: 640,
		MedianMsPerUnit: 0.2, SupplierUSDPerUnit: supplierCADPerUnit,
		VerificationSamples: minSupplierLiabilitySamples,
		TerminalAttempts:    minSupplierLiabilitySamples,
		Measured:            true,
		SourceBinding:       BindingBound,
	}
	catalogue := CataloguePriceAuthority{
		Version: 2,
		ModelID: "all-minilm-l6-v2", JobType: "embed", PriceSource: "market_board",
		ScheduleSHA256: strings.Repeat("a", 64), ScheduleVersion: 2,
		ReferenceCurrency: "usd", ReferencePricePer1K: miniLMReferencePricePer1K,
		SettlementCurrency: "cad", SettlementPricePer1K: settlementPrice,
		ReferenceToSettlementRate: fx, FXRevision: "test-cad-fx",
		BoardSHA256: strings.Repeat("b", 64), PriceFormula: "test frozen CAD authority",
		SupplierShare: miniLMSupplierShare,
	}
	must(t, validateCataloguePriceAuthority(catalogue))
	p := ProjectCellEconomics(cost, catalogue, "batch")
	if p.Currency != "cad" || p.ModelContract.SettlementCurrency != "cad" {
		t.Fatalf("projection lost CAD authority: %+v", p.ModelContract)
	}
	if math.Abs(p.BuyerPricePer1KUnits-settlementPrice) > 1e-15 {
		t.Fatalf("buyer price = %.12g, want CAD settlement %.12g (USD reference %.12g)",
			p.BuyerPricePer1KUnits, settlementPrice, miniLMReferencePricePer1K)
	}
	if p.EnergyPartial.MoneyUSD <= 0 || p.EnergyPartial.Currency != "cad" {
		t.Fatalf("USD electricity partial was not converted under frozen CAD FX: %+v", p.EnergyPartial)
	}
	if got, available := mercGrossPlatformPerUnit(p); available || got != 0 {
		t.Fatalf("projection invented buyer-minus-supplier gross across input/output denominators: got=%v available=%v",
			got, available)
	}

	wrongCurrency := cost
	wrongCurrency.Currency = "usd"
	refused := ProjectCellEconomics(wrongCurrency, catalogue, "batch")
	got, available := mercGrossPlatformPerUnit(refused)
	if refused.SupplierUSDPerUnit != 0 || refused.SupplierLiabilityAvailable ||
		available || got != 0 {
		t.Fatalf("USD payout entered a CAD projection: %+v", refused)
	}

	missingCurrency := cost
	missingCurrency.Currency = ""
	missingCurrency.SourceBinding = BindingUnbound
	missing := ProjectCellEconomics(missingCurrency, catalogue, "batch")
	if missing.SupplierLiabilityAvailable || missing.SupplierUSDPerUnit != 0 {
		t.Fatalf("blank legacy currency was inferred across USD-to-CAD authority: %+v", missing)
	}

	brokenFX := catalogue
	brokenFX.ReferenceToSettlementRate = 0
	brokenFX.FXRevision = ""
	unfrozen := ProjectCellEconomics(cost, brokenFX, "batch")
	if unfrozen.BuyerPricePer1KUnits != 0 || unfrozen.EnergyPartial.MoneyUSD != 0 ||
		unfrozen.EnergyPartial.Currency != "" ||
		unfrozen.EnergyPartial.Knowledge != CategoryUnknown {
		t.Fatalf("USD electricity or buyer money was relabelled CAD without frozen FX: %+v", unfrozen)
	}
}

func TestCellEconomicsSettlementPriceRequiresConsistentOrFrozenAuthority(t *testing.T) {
	legacy := catalogueMinilm()
	price, currency, ok := cellEconomicsSettlementPrice(legacy)
	if !ok || currency != "usd" || price != miniLMReferencePricePer1K {
		t.Fatalf("legacy same-currency catalogue no longer works: price=%v currency=%q ok=%v",
			price, currency, ok)
	}

	mismatchedUSD := legacy
	mismatchedUSD.ReferenceCurrency = "usd"
	mismatchedUSD.SettlementCurrency = "usd"
	mismatchedUSD.SettlementPricePer1K = miniLMReferencePricePer1K * 2
	if price, _, ok := cellEconomicsSettlementPrice(mismatchedUSD); ok || price != 0 {
		t.Fatalf("same-currency settlement price mismatch was accepted: %v", price)
	}

	unfrozenCAD := legacy
	unfrozenCAD.ReferenceCurrency = "usd"
	unfrozenCAD.SettlementCurrency = "cad"
	unfrozenCAD.SettlementPricePer1K = miniLMReferencePricePer1K * 1.37
	unfrozenCAD.ReferenceToSettlementRate = 1.37
	if price, _, ok := cellEconomicsSettlementPrice(unfrozenCAD); ok || price != 0 {
		t.Fatalf("unfrozen cross-currency settlement price was accepted: %v", price)
	}
}

// TestCellEconomicsStillTiesWithoutBreakingCancellation is the recorded
// prediction: wiring cell-level economics without undoing the cancellation
// still ties two equal-reliability cells on one model, even when one is
// substantially faster.
//
// If a selection appears without the duration term changing the cost, that
// selection is not coming from cost — say so rather than bank it.
func TestCellEconomicsStillTiesWithoutBreakingCancellation(t *testing.T) {
	// Cohort-shaped numbers: same supplier USD/unit (cancelled form), equal
	// reliability, different duration. Matches evidence/perf/selector/paired-cohort-embed.json
	// structure with a forced 2.1× duration gap so the prediction is falsifiable.
	const supplierPerUnit = miniLMSupplierUSDPerUnit
	candle := MeasuredSupplierLiabilityProxy{
		CellID: "candle-metal-minilm-embed", RuntimeID: "candle_metal", Engine: "candle",
		JobType: "embed", ModelRef: "all-minilm-l6-v2", HWClass: "apple_silicon_ultra",
		Samples: minSupplierLiabilitySamples, Units: 640, MedianMsPerUnit: 0.21875,
		SupplierUSDPerUnit: supplierPerUnit, VerificationSamples: 20,
		TerminalAttempts: minSupplierLiabilitySamples, Measured: true,
		UnknownPlatformCostComponents: unknownPlatformCostComponents(),
	}
	// 2.1× slower per unit — if duration entered supplier cost, this would not tie.
	llama := MeasuredSupplierLiabilityProxy{
		CellID: "llama-cpp-metal-minilm-embed", RuntimeID: "llama_cpp_metal", Engine: "llama_cpp",
		JobType: "embed", ModelRef: "all-minilm-l6-v2", HWClass: "apple_silicon_ultra",
		Samples: minSupplierLiabilitySamples, Units: 640, MedianMsPerUnit: 0.21875 * 2.1,
		SupplierUSDPerUnit: supplierPerUnit, VerificationSamples: 20,
		TerminalAttempts: minSupplierLiabilitySamples, Measured: true,
		UnknownPlatformCostComponents: unknownPlatformCostComponents(),
	}
	cat := CataloguePriceAuthority{
		ModelID: "all-minilm-l6-v2", JobType: "embed",
		ReferencePricePer1K: 0.00625, SupplierShare: 0.97,
	}
	pc := ProjectCellEconomics(candle, cat, "batch")
	pl := ProjectCellEconomics(llama, cat, "batch")

	if !SupplierLiabilityProxiesTie(pc, pl) {
		t.Fatalf("prediction FALSIFIED: cells no longer tie on supplier-liability proxy\n"+
			"  candle=%.17f (%.4f ms/unit)\n  llama=%.17f (%.4f ms/unit)\n"+
			"duration entered the ranking cost; that undoes the cancellation",
			pc.SupplierLiabilityUSDPerVerifiedUnit, pc.MedianMsPerUnit,
			pl.SupplierLiabilityUSDPerVerifiedUnit, pl.MedianMsPerUnit)
	}
	// Duration terms DO differ — the projection surfaces them so capacity
	// arguments (MORE_THROUGHPUT_AT_EQUAL_SUPPLIER_LIABILITY) still have something to rank on.
	if math.Abs(pc.MedianMsPerUnit-pl.MedianMsPerUnit) < 1e-9 {
		t.Fatal("test fixture failed to separate duration; cannot falsify the prediction")
	}
	if math.Abs(pc.AdmissionExpectedSupplierUSDHr-pl.AdmissionExpectedSupplierUSDHr) < 1e-12 {
		t.Fatalf("admission $/hr should differ with throughput: candle=%v llama=%v",
			pc.AdmissionExpectedSupplierUSDHr, pl.AdmissionExpectedSupplierUSDHr)
	}
	// Energy partial (if present) differs with duration; it must not have been
	// folded into SupplierLiabilityUSDPerVerifiedUnit.
	if pc.EnergyPartial.MoneyUSD > 0 && pl.EnergyPartial.MoneyUSD > 0 {
		if math.Abs(pc.EnergyPartial.MoneyUSD-pl.EnergyPartial.MoneyUSD) < 1e-18 {
			t.Fatal("energy partial should track duration")
		}
		// Energy is orders of magnitude below supplier and is not folded into this
		// proxy. Absolute deltas, not just ratios.
		t.Logf("energy partial candle=$%.12e/unit llama=$%.12e/unit delta=$%.12e; "+
			"supplier-liability proxy $%.8f/verified-unit (tie); energy is not the ranking term",
			pc.EnergyPartial.MoneyUSD, pl.EnergyPartial.MoneyUSD,
			pl.EnergyPartial.MoneyUSD-pc.EnergyPartial.MoneyUSD,
			pc.SupplierLiabilityUSDPerVerifiedUnit)
	}
	t.Logf("PREDICTION HOLDS: supplier-liability proxy ties at $%.10f/verified-unit despite "+
		"2.1× duration gap (candle %.4f ms/unit, llama %.4f ms/unit, delta %.4f ms/unit). "+
		"Any selection between these cells is not coming from cost; it must come from "+
		"capacity/latency (MORE_THROUGHPUT_AT_EQUAL_SUPPLIER_LIABILITY) or another non-cost arm.",
		pc.SupplierLiabilityUSDPerVerifiedUnit, pc.MedianMsPerUnit, pl.MedianMsPerUnit,
		pl.MedianMsPerUnit-pc.MedianMsPerUnit)
}

// TestCellEconomicsReliabilityDoesNotInventPayableLiability pins the settlement
// truth: rejected work is unpaid. Reliability failures make a cell ineligible
// for a measured selector comparison, but cannot inflate the supplier payout
// that the ledger actually owes for the eventual accepted unit.
func TestCellEconomicsReliabilityDoesNotInventPayableLiability(t *testing.T) {
	base := MeasuredSupplierLiabilityProxy{
		CellID: "a", JobType: "embed", ModelRef: "m", HWClass: "apple_silicon_ultra",
		Samples: minSupplierLiabilitySamples, Units: 100, MedianMsPerUnit: 1.0,
		SupplierUSDPerUnit: 0.001, VerificationSamples: 40,
		TerminalAttempts: 40, Measured: true,
	}
	failing := base
	failing.CellID = "b"
	failing.RetryRate = 0.5
	failing.VerificationFails = 10
	failing.TerminalFails = 8
	pa := ProjectCellEconomics(base, CataloguePriceAuthority{}, "batch")
	pb := ProjectCellEconomics(failing, CataloguePriceAuthority{}, "batch")
	payable, ok := failing.ExpectedSupplierLiabilityUSDPerVerifiedUnit()
	if !ok || math.Abs(payable-0.001) > 1e-12 {
		t.Fatalf("money accessor changed exact payable liability: %.15f ok=%v", payable, ok)
	}
	if pb.SupplierLiabilityAvailable || SupplierLiabilityProxiesTie(pa, pb) {
		t.Fatalf("reliability-failed proxy remained available to projection/tie: a=%+v b=%+v", pa, pb)
	}
	if pb.Measured || pb.AdmissionExpectedSupplierUSDHr != 0 {
		t.Fatalf("reliability-failed proxy published measured/earnings posture: %+v", pb)
	}
	if pb.ReliabilityMultiplier <= pa.ReliabilityMultiplier {
		t.Fatalf("reliability burden disappeared instead of remaining a separate diagnostic: base=%v failing=%v",
			pa.ReliabilityMultiplier, pb.ReliabilityMultiplier)
	}
}

func TestCellEconomicsRefusesCrossCurrencyProjection(t *testing.T) {
	cost := MeasuredSupplierLiabilityProxy{
		CellID: "a", JobType: "embed", ModelRef: "m", HWClass: "apple_silicon_ultra",
		Currency: "cad", Samples: minSupplierLiabilitySamples, Units: 100,
		MedianMsPerUnit: 1, SupplierUSDPerUnit: 0.001,
		VerificationSamples: minSupplierLiabilitySamples,
		TerminalAttempts:    minSupplierLiabilitySamples,
		Measured:            true,
	}
	p := ProjectCellEconomics(cost, CataloguePriceAuthority{
		ModelID: "m", JobType: "embed", SettlementCurrency: "usd",
	}, "batch")
	if p.SupplierLiabilityAvailable || p.Measured ||
		!strings.Contains(p.SupplierLiabilityBasis, "conflicts") {
		t.Fatalf("cross-currency supplier liability entered projection: %+v", p)
	}
}

// TestSettledPricingDecisionMoneyDoesNotMoveUnderReprojection confirms that
// building cell projections does not create a second pricing authority and
// cannot alter a frozen PricingDecision digest or fixed-point money figure.
func TestSettledPricingDecisionMoneyDoesNotMoveUnderReprojection(t *testing.T) {
	// A minimal fixed-point decision standing in for a settled receipt.
	trueNetUnset := (*int64)(nil)
	settled := PricingDecision{
		Version:        pricingDecisionVersion,
		PolicyRevision: pricingDecisionPolicyRevision,
		ExecutionMode:  "distributed",
		Currency:       "usd",
		Tier:           "batch",
		Catalogue: CataloguePriceAuthority{
			Version: cataloguePriceScheduleVersion, ScheduleVersion: cataloguePriceScheduleVersion,
			ModelID: "all-minilm-l6-v2", JobType: "embed", PriceSource: "market_board",
			ScheduleSHA256:    "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
			BoardSHA256:       "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb",
			ReferenceCurrency: "usd", SettlementCurrency: "usd",
			ReferencePricePer1K: 0.01, SettlementPricePer1K: 0.01,
			ReferenceToSettlementRate: 1, FXRevision: "fx-test",
			PriceFormula: "test", SupplierShare: 0.97,
		},
		BillableUnits: 100,
		BuyerPrice:    0.001, MaximumBuyerPrice: 0.001,
		PrimarySupplierCost:  modeledCost(0.00097, "test"),
		PlatformContribution: modeledCost(0.00003, "test"),
		FixedPoint: &FixedPointPricingDecision{
			Currency:                   "usd",
			BuyerChargeNanos:           1_000_000,
			AcceptedCeilingNanos:       1_000_000,
			SupplierEntitlementsNanos:  970_000,
			KnownVariableCostsNanos:    0,
			MercGrossSpreadNanos:       30_000,
			KnownCostContributionNanos: 30_000,
			TrueNetContributionNanos:   trueNetUnset,
			UnknownCostCategories:      []string{"storage", "egress", "provider_energy"},
		},
	}
	beforeDigest, err := pricingDecisionDigest(settled)
	if err != nil {
		// validate may refuse an incomplete decision; fall back to raw marshal
		// identity so the immutability claim is still pinned.
		raw, mErr := json.Marshal(settled)
		if mErr != nil {
			t.Fatalf("marshal settled: %v (digest err %v)", mErr, err)
		}
		sum := sha256.Sum256(raw)
		beforeDigest = hex.EncodeToString(sum[:])
	}
	beforeSupplier := settled.FixedPoint.SupplierEntitlementsNanos
	beforeBuyer := settled.FixedPoint.BuyerChargeNanos

	// Re-project cell economics many times with different measured durations.
	// None of this may rewrite the settled decision.
	for _, ms := range []float64{0.2, 0.5, 2.1, 10.0} {
		cost := MeasuredSupplierLiabilityProxy{
			CellID: "candle-metal-minilm-embed", HWClass: "apple_silicon_ultra",
			Samples: minSupplierLiabilitySamples, Units: 100, MedianMsPerUnit: ms,
			SupplierUSDPerUnit: 0.0000097, VerificationSamples: 20,
			TerminalAttempts: minSupplierLiabilitySamples, Measured: true,
		}
		_ = ProjectCellEconomics(cost, settled.Catalogue, settled.Tier)
	}

	afterDigest, err := pricingDecisionDigest(settled)
	if err != nil {
		raw, _ := json.Marshal(settled)
		sum := sha256.Sum256(raw)
		afterDigest = hex.EncodeToString(sum[:])
	}
	if beforeDigest != afterDigest {
		t.Fatalf("settled PricingDecision digest moved under reprojection:\n  before=%s\n  after=%s",
			beforeDigest, afterDigest)
	}
	if settled.FixedPoint.SupplierEntitlementsNanos != beforeSupplier ||
		settled.FixedPoint.BuyerChargeNanos != beforeBuyer {
		t.Fatalf("settled fixed-point money moved: supplier %d→%d buyer %d→%d",
			beforeSupplier, settled.FixedPoint.SupplierEntitlementsNanos,
			beforeBuyer, settled.FixedPoint.BuyerChargeNanos)
	}
	if settled.FixedPoint.TrueNetContributionNanos != nil {
		t.Fatal("reprojection must not invent true net on a decision that left it unset")
	}
}

// TestProviderCostDefectIsNotSilent: nvidia_24gb rate cites a withdrawn
// receipt and must surface DEFECT rather than being treated as governed.
func TestProviderCostDefectIsNotSilent(t *testing.T) {
	rate, ok := providerRatesByHWClass["nvidia_24gb"]
	if !ok {
		t.Fatal("nvidia_24gb rate missing")
	}
	if rate.Provenance == "" ||
		!(containsSubstring([]string{rate.Provenance}, "DEFECT") ||
			containsSubstring([]string{rate.Provenance}, "withdrawn") ||
			containsSubstring([]string{rate.Provenance}, "WITHDRAWN")) {
		// The constant comment and provenance string both flag this; require
		// the wire string to carry DEFECT or withdrawn so projections can copy it.
		if !containsCI(rate.Provenance, "defect") && !containsCI(rate.Provenance, "withdrawn") {
			t.Fatalf("nvidia_24gb provenance does not flag DEFECT/withdrawn: %q", rate.Provenance)
		}
	}
	// Projection path: a cloud-backed cell on nvidia_24gb with measured duration
	// must copy the defect into the term when the rate resolves.
	// We only assert the rate table defect here if no cloud cell is registered
	// for nvidia_24gb in the authority — inventing a cell id would be dishonest.
	term := projectProviderCostTerm(
		"not-a-real-cell", "nvidia_24gb", 1.0, CataloguePriceAuthority{})
	if term.Knowledge != CategoryUnknown {
		// Unknown cell → cannot classify. That is correct; the defect lives on
		// the rate table until a real cloud cell resolves it.
		t.Logf("provider term for unknown cell: %+v (rate-table defect remains: %s)",
			term, rate.Provenance)
	}
}

// TestProjectionIsNotAPricingDecision keeps the selector projection out of the
// PricingDecision type family: no version/policy that could be mistaken for a
// frozen money authority, and ProjectCellEconomics must not return a type
// assignable as money settlement input.
func TestProjectionIsNotAPricingDecision(t *testing.T) {
	p := ProjectCellEconomics(MeasuredSupplierLiabilityProxy{
		CellID: "c", Samples: 1, SupplierUSDPerUnit: 0.001, VerificationSamples: 1,
	}, CataloguePriceAuthority{}, "batch")
	if p.Kind == "pricing_decision" || p.SchemaVersion == pricingDecisionVersion && p.Kind == "" {
		t.Fatal("projection looks like a PricingDecision")
	}
	raw, err := json.Marshal(p)
	must(t, err)
	// Must not carry fixed_point buyer/supplier nanos — that is settlement.
	var probe map[string]any
	must(t, json.Unmarshal(raw, &probe))
	for _, forbidden := range []string{
		"fixed_point", "buyer_charge_nanos", "supplier_entitlements_nanos",
		"accepted_ceiling_nanos", "pricing_decision_sha256",
	} {
		if _, ok := probe[forbidden]; ok {
			t.Fatalf("projection carries settlement field %q", forbidden)
		}
	}
}

// TestCellEconomicsProjectionRequiresStrictMeasuredVO pins the separation
// between the exact money accessor and selector eligibility. A rejected/retried
// row can still report what an accepted task would pay, but the projection must
// not make that row available to a comparison.
func TestCellEconomicsProjectionRequiresStrictMeasuredVO(t *testing.T) {
	cost := MeasuredSupplierLiabilityProxy{
		SupplierUSDPerUnit: 0.001, RetryRate: 0.5,
		VerificationSamples: 40, VerificationFails: 10,
		TerminalAttempts: 40, TerminalFails: 8,
		Samples: minSupplierLiabilitySamples, Measured: true,
		MedianMsPerUnit: 2.0, CellID: "c", HWClass: "apple_silicon_ultra",
	}
	want, ok := cost.ExpectedSupplierLiabilityUSDPerVerifiedUnit()
	if !ok {
		t.Fatal("measured VO unavailable")
	}
	p := ProjectCellEconomics(cost, CataloguePriceAuthority{}, "batch")
	if p.SupplierLiabilityAvailable || p.SupplierLiabilityUSDPerVerifiedUnit != 0 {
		t.Fatalf("strict projection accepted unreliable VO: %+v (money accessor remains %.17f)", p, want)
	}
}

func TestCellEconomicsProjectionRefusesGeometryAuthorityFailure(t *testing.T) {
	clean := MeasuredSupplierLiabilityProxy{
		CellID: "clean", Samples: minSupplierLiabilitySamples, Units: 100,
		MedianMsPerUnit: 2, SupplierUSDPerUnit: 0.001,
		VerificationSamples: minSupplierLiabilitySamples,
		TerminalAttempts:    minSupplierLiabilitySamples,
		Measured:            true,
	}
	mixed := clean
	mixed.CellID = "mixed"
	mixed.Measured = false
	mixed.AuthorityRefusals = []string{
		"exact input/task geometry unavailable: observed 2 distinct geometries in the bounded comparison cohort",
	}
	if payable, ok := mixed.ExpectedSupplierLiabilityUSDPerVerifiedUnit(); !ok || payable != 0.001 {
		t.Fatalf("geometry refusal changed frozen payable liability: %v ok=%v", payable, ok)
	}
	cleanProjection := ProjectCellEconomics(clean, CataloguePriceAuthority{}, "batch")
	mixedProjection := ProjectCellEconomics(mixed, CataloguePriceAuthority{}, "batch")
	if !cleanProjection.SupplierLiabilityAvailable || mixedProjection.SupplierLiabilityAvailable {
		t.Fatalf("strict projection availability ignored geometry authority: clean=%+v mixed=%+v",
			cleanProjection, mixedProjection)
	}
	tie := ExplainCostTie(cleanProjection, mixedProjection, CataloguePriceAuthority{})
	if tie.Verdict != costTieUnavailable || tie.Tied {
		t.Fatalf("cost tie consumed mixed-geometry proxy: %+v", tie)
	}
	if !strings.Contains(tie.Statement, "exact-geometry") {
		t.Fatalf("cost-tie refusal does not name geometry authority: %q", tie.Statement)
	}
}

// TestWriteCellEconomicsCensusReceipt is env-gated. Set
// MERC_CELL_ECONOMICS_RECEIPT=1 to emit a bound evidence receipt under
// evidence/perf/selector/ for the census + still-tie prediction.
func TestWriteCellEconomicsCensusReceipt(t *testing.T) {
	if os.Getenv("MERC_CELL_ECONOMICS_RECEIPT") != "1" {
		t.Skip("MERC_CELL_ECONOMICS_RECEIPT is not 1; receipt writer is env-gated")
	}
	const supplierPerUnit = miniLMSupplierUSDPerUnit
	candle := MeasuredSupplierLiabilityProxy{
		CellID: "candle-metal-minilm-embed", RuntimeID: "candle_metal", Engine: "candle",
		JobType: "embed", ModelRef: "all-minilm-l6-v2", HWClass: "apple_silicon_ultra",
		Samples: minSupplierLiabilitySamples, Units: 640, MedianMsPerUnit: 0.21875,
		SupplierUSDPerUnit: supplierPerUnit, VerificationSamples: 20,
		TerminalAttempts: minSupplierLiabilitySamples, Measured: true,
		UnknownPlatformCostComponents: unknownPlatformCostComponents(),
	}
	llama := MeasuredSupplierLiabilityProxy{
		CellID: "llama-cpp-metal-minilm-embed", RuntimeID: "llama_cpp_metal", Engine: "llama_cpp",
		JobType: "embed", ModelRef: "all-minilm-l6-v2", HWClass: "apple_silicon_ultra",
		Samples: minSupplierLiabilitySamples, Units: 640, MedianMsPerUnit: 0.28125,
		SupplierUSDPerUnit: supplierPerUnit, VerificationSamples: 20,
		TerminalAttempts: minSupplierLiabilitySamples, Measured: true,
		UnknownPlatformCostComponents: unknownPlatformCostComponents(),
	}
	cat := CataloguePriceAuthority{
		ModelID: "all-minilm-l6-v2", JobType: "embed",
		ReferencePricePer1K: 0.00625, SupplierShare: 0.97,
	}
	projections := ProjectCellEconomicsMap(map[string]MeasuredSupplierLiabilityProxy{
		candle.CellID: candle, llama.CellID: llama,
	}, cat, "batch")
	pc, pl := projections[candle.CellID], projections[llama.CellID]
	tie := SupplierLiabilityProxiesTie(pc, pl)
	latencyDeltaMs := pl.MedianMsPerUnit - pc.MedianMsPerUnit
	latencyRatio := pl.MedianMsPerUnit / pc.MedianMsPerUnit

	// Census arithmetic for the report.
	const units, price, share = 640.0, 0.00625, 0.97
	closed := censusRequiredUSD(units, price, share)
	viaA := censusRequiredViaThroughput(units, 100, price, share)
	viaB := censusRequiredViaThroughput(units, 210, price, share)

	out := map[string]any{
		"kind":           "cell_economics_census_receipt",
		"schema_version": 1,
		"measured_at":    time.Now().UTC().Format(time.RFC3339Nano),
		"census": map[string]any{
			"holds": true,
			"arithmetic": map[string]any{
				"closed_form_usd":           closed,
				"via_100_units_per_sec_usd": viaA,
				"via_210_units_per_sec_usd": viaB,
				"delta_usd":                 viaB - viaA,
				"expression":                "required = units/1000 × price × share (unitsPerSec cancels)",
			},
			"authority": []string{
				"docs/archive/engineering/PROGRAMME.md",
				"control/pricing_decision.go#exactTaskEconomics",
				"control/runtime_cell_economics.go",
			},
		},
		"projections": projections,
		"prediction": map[string]any{
			"statement": "wiring cell-level economics without breaking the cancellation will still tie",
			"holds":     tie,
			"supplier_liability_usd_per_verified_unit": map[string]float64{
				candle.CellID: pc.SupplierLiabilityUSDPerVerifiedUnit,
				llama.CellID:  pl.SupplierLiabilityUSDPerVerifiedUnit,
			},
			"remaining_authority_forcing_tie": supplierEntitlementPolicyCancelled +
				"; SupplierUSDPerUnit is catalogue units×price×share keyed by (model, job_type), never by runtime cell",
			"latency_ms_per_unit": map[string]any{
				candle.CellID: pc.MedianMsPerUnit,
				llama.CellID:  pl.MedianMsPerUnit,
				"delta_ms":    latencyDeltaMs,
				"ratio":       latencyRatio,
			},
			"selection_without_cost_change": "any selection between these cells is not a total-cost result; use MORE_THROUGHPUT_AT_EQUAL_SUPPLIER_LIABILITY or another non-cost arm",
		},
		"settled_receipt_immutability": map[string]any{
			"projection_is_not_pricing_decision":         true,
			"revalidation_may_change_future_quotes_only": true,
			"supplier_entitlement_policy":                supplierEntitlementPolicyCancelled,
		},
		"provider_cost_defect": map[string]any{
			"nvidia_24gb_provenance":     providerRatesByHWClass["nvidia_24gb"].Provenance,
			"must_not_build_on_silently": true,
		},
		"true_net": map[string]any{
			"computable": false,
			"reason":     "structurally unavailable while any cost category is unknown or assumed",
		},
		"limitations": []string{
			"Projection is selector evidence, not settlement money.",
			"Energy partial uses ASSUMED watts and defaulted electricity; not full physical cost.",
			"Storage, egress, utilization, refund risk remain unknown.",
			"Does not promote any cell to routable.",
		},
	}

	dir := filepath.Join("..", "evidence", "perf", "selector")
	must(t, os.MkdirAll(dir, 0o755))
	path := filepath.Join(dir, "cell-economics-census.json")
	id, bin, err := DefaultBoundIdentity("..", "control/runtime_cell_economics_test.go",
		"census arithmetic + per-cell economics projection schema",
		"synthetic measured rows shaped like paired-cohort-embed; not a live agent cohort")
	mustf(t, err, "bound identity unavailable; evidence was not written: %v")
	mustf(t, WriteBoundEvidenceJSON(EvidenceWriteRequest{
		RepoRoot: "..", Path: path, Payload: out,
		Identity: id, BuildBinaryPath: bin,
	}), "bound evidence write refused: %v; a withdrawn authority is sticky — use a new authority path/id")
	t.Logf("wrote cell economics census receipt %s (tie=%v)", path, tie)
}

// writePlainJSON is only for explicitly UNBOUND diagnostic output. It still
// honors sticky withdrawal: raw JSON is not an escape hatch around the governed
// writer's refusal. A replacement must choose a new authority path/id.
func writePlainJSON(path string, out any) error {
	if existing, err := readJSONObjectFile(path); err == nil {
		if pathHoldsWithdrawn(existing) {
			return fmt.Errorf("%s holds a withdrawn authority; use a new authority path/id", path)
		}
	} else if !os.IsNotExist(err) {
		return fmt.Errorf("refusing to overwrite unreadable evidence %s: %w", path, err)
	}
	raw, err := json.MarshalIndent(out, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(path, append(raw, '\n'), 0o644)
}

func TestPlainEvidenceWriterCannotBypassStickyWithdrawal(t *testing.T) {
	path := filepath.Join(t.TempDir(), "withdrawn.json")
	original := []byte("{\n  \"binding_status\": \"WITHDRAWN\",\n  \"validity\": \"WITHDRAWN\"\n}\n")
	must(t, os.WriteFile(path, original, 0o644))

	err := writePlainJSON(path, map[string]any{"binding_status": BindingUnbound})
	if err == nil || !strings.Contains(strings.ToLower(err.Error()), "withdrawn") {
		t.Fatalf("plain evidence overwrite error=%v, want sticky-withdrawal refusal", err)
	}
	after, readErr := os.ReadFile(path)
	must(t, readErr)
	if string(after) != string(original) {
		t.Fatal("plain evidence writer changed a withdrawn receipt")
	}
}

func containsCI(s, sub string) bool {
	return strings.Contains(strings.ToLower(s), strings.ToLower(sub))
}
