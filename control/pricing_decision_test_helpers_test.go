package main

import (
	"strings"
	"testing"
)

func supplierShareForTest(t *testing.T, jobType, modelID string) float64 {
	t.Helper()
	share, err := supplierShareForWorkload(jobType, modelID)
	if err != nil {
		t.Fatalf("supplier share policy for %s/%s: %v", jobType, modelID, err)
	}
	return share
}

func catalogueAuthorityFixture(t *testing.T, workload WorkloadDecision, currency string, supplierShare float64) CataloguePriceAuthority {
	t.Helper()
	rate := 1.0
	fxRevision := "identity-usd"
	if currency != "usd" {
		rate = 1.35
		fxRevision = "test-fx-" + currency
	}
	referencePrice := 0.01
	authority := CataloguePriceAuthority{
		Version:                     cataloguePriceScheduleVersion,
		ModelID:                     workload.Binding.Model.Ref,
		JobType:                     workload.RuntimeJobType,
		PriceSource:                 "market_board",
		ScheduleSHA256:              strings.Repeat("c", 64),
		ScheduleVersion:             cataloguePriceScheduleVersion,
		ReferenceCurrency:           catalogueReferenceCurrency,
		ReferencePricePer1K:         referencePrice,
		SettlementCurrency:          currency,
		SettlementPricePer1K:        ceilPricePer1K(referencePrice * rate),
		ReferenceToSettlementRate:   rate,
		FXRevision:                  fxRevision,
		BoardSHA256:                 strings.Repeat("d", 64),
		PriceFormula:                "test market-board authority",
		SupplierShare:               supplierShare,
		SupplierSharePolicyRevision: supplierSharePolicyRevision,
	}
	if err := validateCataloguePriceAuthority(authority); err != nil {
		t.Fatalf("catalogue authority fixture: %v", err)
	}
	return authority
}

func placementForPricingFixture(
	t *testing.T,
	workload WorkloadDecision,
	authority CataloguePriceAuthority,
) PlacementRequirement {
	t.Helper()
	binding := workload.Binding
	ceiling, err := supplierAdmissionCeilingUSDHr(
		authority, workload.RuntimeJobType, binding.Tier,
		admissionCellsForWorkload(workload),
	)
	if err != nil {
		t.Fatalf("derive placement ceiling fixture: %v", err)
	}
	placement, err := placementRequirementFor(jobSubmit{
		JobType: binding.JobType, Model: binding.Model, Constraints: binding.Constraints,
		Tier: binding.Tier, MinReputation: binding.MinReputation,
	}, workload, float32(ceiling))
	if err != nil {
		t.Fatalf("build placement fixture: %v", err)
	}
	return placement
}
