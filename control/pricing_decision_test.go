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
