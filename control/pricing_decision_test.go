package main

import (
	"reflect"
	"strings"
	"testing"
)

func distributedPricingFixture(t *testing.T) (
	WorkloadDecision,
	ComputePlan,
	PlacementRequirement,
	EconomicPlan,
	PricingDecision,
) {
	t.Helper()
	workload, compute, economic := computePlanFixture(t)
	authority := catalogueAuthorityFixture(
		t, workload, economic.Schedule.Currency, economic.Input.SupplierShare,
	)
	placement := placementForPricingFixture(t, workload, authority)
	pricing, err := newDistributedPricingDecision(
		workload, compute, placement, economic, authority,
		workload.Binding.Tier, "",
	)
	if err != nil {
		t.Fatalf("build distributed pricing fixture: %v", err)
	}
	return workload, compute, placement, economic, pricing
}

func TestFixedPointPricingConservesAndRefusesFalseTrueNet(t *testing.T) {
	scenario := EconomicScenario{
		NetBilledUSD: 0.000010, SupplierLiabilityUSD: 0.000004,
		ProcessorFeeUSD: 0.000001, ControlPlaneCostUSD: 0.000001,
		ContributionMarginUSD: 0.000004,
	}
	fixed, err := fixedPointPricingFromScenario(
		"cad", 0.000010, 0.000020, scenario,
		[]string{"storage cost", "risk reserve"},
	)
	if err != nil {
		t.Fatal(err)
	}
	if fixed.BuyerChargeNanos != 10_000 || fixed.SupplierEntitlementsNanos != 4_000 ||
		fixed.KnownVariableCostsNanos != 2_000 ||
		fixed.KnownCostContributionNanos != 4_000 {
		t.Fatalf("fixed-point amounts drifted: %+v", fixed)
	}
	decision := PricingDecision{Currency: "cad", FixedPoint: fixed}
	if err := validateFixedPointPricing(decision); err != nil {
		t.Fatalf("valid fixed-point decision refused: %v", err)
	}
	if fixed.TrueNetContributionNanos != nil {
		t.Fatal("unknown costs became true net contribution")
	}

	mutant := *fixed
	mutant.BuyerChargeNanos++
	decision.FixedPoint = &mutant
	if err := validateFixedPointPricing(decision); err == nil {
		t.Fatal("non-conserving fixed-point buyer charge was accepted")
	}
	mutant = *fixed
	trueNet := mutant.KnownCostContributionNanos
	mutant.TrueNetContributionNanos = &trueNet
	decision.FixedPoint = &mutant
	if err := validateFixedPointPricing(decision); err == nil {
		t.Fatal("true net contribution was accepted while costs remain unknown")
	}
}

func TestPricingDecisionRejectsArbitraryPositiveSupplierAdmissionRate(t *testing.T) {
	workload, compute, placement, economic, pricing := distributedPricingFixture(t)
	mutantPlacement := placement
	mutantPlacement.OfferedRateUsdHr *= 100
	if _, err := newDistributedPricingDecision(
		workload, compute, mutantPlacement, economic, pricing.Catalogue,
		pricing.Tier, "",
	); err == nil {
		t.Fatal("arbitrary positive supplier admission rate survived derivation")
	}

	// Even an attacker who updates the placement digest and decision field
	// together cannot turn two independently valid siblings into a valid
	// composite decision.
	mutant := pricing
	mutant.PlacementRequirementSHA256, _ = placementRequirementDigest(mutantPlacement)
	mutant.SupplierAdmissionCeilingUSDHr = float64(mutantPlacement.OfferedRateUsdHr)
	if err := ValidateDistributedPricingDecisionSnapshot(
		mutant, workload, compute, mutantPlacement, economic,
	); err == nil {
		t.Fatal("co-mutated placement and pricing decision survived deterministic validation")
	}
}

func TestPricingDecisionDigestBindsEveryEconomicAuthorityFamily(t *testing.T) {
	_, _, _, _, base := distributedPricingFixture(t)
	original, err := pricingDecisionDigest(base)
	if err != nil {
		t.Fatal(err)
	}
	tests := []struct {
		name   string
		mutate func(*PricingDecision)
	}{
		{"workload", func(p *PricingDecision) { p.WorkloadDecisionSHA256 = strings.Repeat("1", 64) }},
		{"compute", func(p *PricingDecision) { p.ComputePlanSHA256 = strings.Repeat("2", 64) }},
		{"placement", func(p *PricingDecision) { p.PlacementRequirementSHA256 = strings.Repeat("3", 64) }},
		{"economic plan", func(p *PricingDecision) { p.EconomicPlanSHA256 = strings.Repeat("4", 64) }},
		{"economic schedule", func(p *PricingDecision) { p.EconomicScheduleSHA256 = strings.Repeat("5", 64) }},
		{"catalogue schedule", func(p *PricingDecision) { p.Catalogue.ScheduleSHA256 = strings.Repeat("6", 64) }},
		{"market board", func(p *PricingDecision) { p.Catalogue.BoardSHA256 = strings.Repeat("7", 64) }},
		{"FX", func(p *PricingDecision) { p.Catalogue.FXRevision += "-changed" }},
		{"supplier share", func(p *PricingDecision) { p.Catalogue.SupplierShare -= 0.01 }},
		{"buyer price", func(p *PricingDecision) { p.BuyerPrice += 0.01 }},
		{"supplier cost", func(p *PricingDecision) { p.PrimarySupplierCost.Amount += 0.01 }},
		{"payment cost", func(p *PricingDecision) { p.PaymentCost.Amount += 0.01 }},
		{"unknown cost status", func(p *PricingDecision) { p.StorageCost.Status = pricingCostModeled }},
		{"confidence", func(p *PricingDecision) { p.Confidence -= 0.1 }},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			mutant := base
			tc.mutate(&mutant)
			got, err := pricingDecisionDigest(mutant)
			if err != nil {
				t.Fatal(err)
			}
			if got == original {
				t.Fatalf("%s mutation did not change pricing digest", tc.name)
			}
		})
	}
}

func TestDistributedPricingDecisionUsesExplicitUnknownCostStates(t *testing.T) {
	_, _, _, _, pricing := distributedPricingFixture(t)
	for name, component := range map[string]PricingCostComponent{
		"storage":  pricing.StorageCost,
		"egress":   pricing.EgressCost,
		"provider": pricing.ProviderCost,
		"risk":     pricing.RiskReserve,
	} {
		if component.Status != pricingCostUnknown || component.Amount != 0 ||
			component.Basis == "" {
			t.Fatalf("%s cost silently became modeled zero: %+v", name, component)
		}
	}
	if pricing.ExpectedSupplierGrossUSDHr+0.000001 <
		pricing.SupplierAdmissionCeilingUSDHr {
		t.Fatalf("modeled supplier gross %.6f is below admission ceiling %.6f",
			pricing.ExpectedSupplierGrossUSDHr,
			pricing.SupplierAdmissionCeilingUSDHr)
	}
}

func TestV4PricingBillableUnitsUseFrozenSettlementAuthority(t *testing.T) {
	workload, compute, placement, economic, pricing := distributedPricingFixture(t)
	if compute.Version != computePlanVersion {
		t.Fatalf("fixture plan version=%d, want current v4", compute.Version)
	}
	if compute.EstimatedInputTokens == int64(compute.SettlementInputUnits) {
		t.Fatal("fixture does not distinguish selected-body planning tokens from settlement units")
	}
	want := compute.SettlementInputUnits + float64(compute.EstimatedOutputTokens)
	if pricing.BillableUnits != want {
		t.Fatalf("pricing billable_units=%v, want frozen settlement units %v", pricing.BillableUnits, want)
	}
	if pricing.BillableUnits == float64(compute.EstimatedInputTokens+compute.EstimatedOutputTokens) {
		t.Fatal("pricing still presents selected-body planning tokens as money units")
	}

	// Historical v2 decisions retain their original computed presentation and
	// remain verifiable; version 3 and later plans carry the reconciled field.
	historical := compute
	historical.Version = computePlanVersionV2
	historical.SettlementInputUnits = 0
	historical.ETAConfidenceBandMethod = ""
	historicalPricing, err := newDistributedPricingDecision(
		workload, historical, placement, economic, pricing.Catalogue,
		workload.Binding.Tier, "",
	)
	if err != nil {
		t.Fatalf("rebuild historical v2 pricing: %v", err)
	}
	wantHistorical := float64(historical.EstimatedInputTokens + historical.EstimatedOutputTokens)
	if historicalPricing.BillableUnits != wantHistorical {
		t.Fatalf("historical v2 billable_units=%v, want preserved %v", historicalPricing.BillableUnits, wantHistorical)
	}
}

func TestExactReusePricingHasNoPhysicalSupplierOrPlacement(t *testing.T) {
	workload, origin, _, _, originPricing := distributedPricingFixture(t)
	originSHA, err := pricingDecisionDigest(originPricing)
	if err != nil {
		t.Fatal(err)
	}
	reuseCompute, err := newExactReuseComputePlan(
		workload, origin.InputRecords, origin.InputBytes, testInputDepthProfile(origin.InputRecords), 0.01, &origin,
	)
	if err != nil {
		t.Fatal(err)
	}
	reuse, err := newExactReusePricingDecision(
		workload, reuseCompute, originPricing.Catalogue,
		workload.Binding.Tier, 0.01, originSHA,
	)
	if err != nil {
		t.Fatal(err)
	}
	if reuse.PlacementRequirementSHA256 != "" ||
		reuse.PrimarySupplierCost.Status != pricingCostNotApplicable ||
		reuse.VerificationCost.Status != pricingCostNotApplicable ||
		reuse.PrimarySupplierCost.Amount != 0 ||
		reuse.VerificationCost.Amount != 0 {
		t.Fatalf("exact reuse attributes physical work: %+v", reuse)
	}
	mutant := reuse
	mutant.PrimarySupplierCost = modeledCost(0.001, "forged physical work")
	if reflect.DeepEqual(mutant, reuse) {
		t.Fatal("test mutation failed")
	}
	if err := ValidateExactReusePricingDecisionSnapshot(
		mutant, workload, reuseCompute,
	); err == nil {
		t.Fatal("exact reuse accepted forged physical supplier work")
	}
}

func TestBoundQuoteCatalogueSelectionNeverReadsCurrentModelPrice(t *testing.T) {
	_, _, _, _, pricing := distributedPricingFixture(t)
	called := false
	got, err := selectCataloguePriceAuthority(
		&boundQuote{Pricing: pricing},
		func() (CataloguePriceAuthority, error) {
			called = true
			mutant := pricing.Catalogue
			mutant.SettlementPricePer1K *= 100
			return mutant, nil
		},
	)
	if err != nil {
		t.Fatal(err)
	}
	if called {
		t.Fatal("bound quote consulted the current model price")
	}
	if !reflect.DeepEqual(got, pricing.Catalogue) {
		t.Fatalf("bound quote catalogue changed: got %+v want %+v",
			got, pricing.Catalogue)
	}
}

// The validator must re-derive the supplier unit rate from the governed cell
// authority, never from the record being checked.
//
// Rebuilding with decision.ExpectedSupplierUnitsPerSec made this self-certifying.
// The rebuild derives the admission ceiling from that rate and compares it to the
// placement's offered rate, so an attacker who can rewrite a stored pricing
// decision - and can therefore rewrite the stored placement beside it - gets a
// composite that verifies at any rate at all. Here the whole set is internally
// consistent and every digest matches; the only thing wrong with it is that no
// benchmark in the tree produces the rate.
func TestStoredPricingDecisionCannotCertifyItsOwnSupplierRate(t *testing.T) {
	workload, compute, placement, economic, pricing := distributedPricingFixture(t)

	forgedRate := pricing.ExpectedSupplierUnitsPerSec * 10
	forgedPlacement := placement
	forgedPlacement.OfferedRateUsdHr = float32(expectedSupplierUSDHr(
		forgedRate, pricing.Catalogue.ReferencePricePer1K,
		pricing.Catalogue.SupplierShare, pricing.Tier,
	))
	forged, err := distributedPricingDecisionAtRate(
		workload, compute, forgedPlacement, economic, pricing.Catalogue,
		pricing.Tier, "", forgedRate,
	)
	if err != nil {
		t.Fatalf("the forgery is meant to be internally consistent: %v", err)
	}
	if err := ValidateDistributedPricingDecisionSnapshot(
		forged, workload, compute, forgedPlacement, economic,
	); err == nil {
		t.Fatalf("a decision claiming %v units/s validated against itself at a "+
			"$%.5f/hr admission ceiling", forgedRate, forged.SupplierAdmissionCeilingUSDHr)
	}
}

// A quote freezes a supplier unit rate; the receipt behind it keeps ageing.
// Rebuilding a bound submission from live evidence compared today's posture
// against the quote's frozen offered rate, so a receipt crossing its 180-day
// revalidation window between quote and submit turned an accepted quote into a
// 409 - on a quote nothing the buyer controls had changed.
func TestQuoteFrozenSupplierRateSurvivesTheRevalidationBoundary(t *testing.T) {
	workload, compute, placement, economic, pricing := distributedPricingFixture(t)

	// What the SAME receipt yields once it is past its window.
	stalePostureRate := pricing.ExpectedSupplierUnitsPerSec /
		measuredThroughputHaircut * staleThroughputHaircut
	stalePlacement := placement
	stalePlacement.OfferedRateUsdHr = float32(expectedSupplierUSDHr(
		stalePostureRate, pricing.Catalogue.ReferencePricePer1K,
		pricing.Catalogue.SupplierShare, pricing.Tier,
	))

	// This is the 409: live resolution refuses a quote frozen on the other side
	// of the boundary, in either direction.
	if _, err := newDistributedPricingDecision(
		workload, compute, stalePlacement, economic, pricing.Catalogue, pricing.Tier, "",
	); err == nil {
		t.Fatal("live re-resolution accepted a rate from the other posture; " +
			"this test no longer describes the boundary")
	}

	// Binding the quote's own frozen rate accepts it, and the stored snapshot
	// still verifies, because both governed postures of the same measurement are
	// admissible and a rate from neither is not.
	bound, err := distributedPricingDecisionAtRate(
		workload, compute, stalePlacement, economic, pricing.Catalogue,
		pricing.Tier, "", stalePostureRate,
	)
	if err != nil {
		t.Fatalf("binding the quote's frozen rate: %v", err)
	}
	if err := ValidateDistributedPricingDecisionSnapshot(
		bound, workload, compute, stalePlacement, economic,
	); err != nil {
		t.Fatalf("a decision at a governed stale-posture rate failed its own "+
			"snapshot check: %v", err)
	}
}
