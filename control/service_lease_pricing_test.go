package main

import "testing"

func serviceLeasePricingFixture(t *testing.T) ServiceLeasePricingInputs {
	t.Helper()
	installSettlementCurrencyForTest(t, "cad")
	profile := sortedVLLMProfiles()[0]
	return ServiceLeasePricingInputs{
		Profile: profile, Currency: MustParseCurrency("cad"), Region: "ca-central-1",
		MinimumReplicas: 1, MaximumReplicas: 3, TermSeconds: 3600,
		MaximumP95LatencyMilliseconds:   500,
		SupplierNanosPerReplicaHour:     2_000_000_000,
		ResidencyNanosPerReplicaHour:    200_000_000,
		ControlPlaneNanosPerReplicaHour: 100_000_000,
		RiskReserveNanosPerReplicaHour:  100_000_000,
		ContributionNanosPerReplicaHour: 300_000_000,
		BuyerDeclaredCeilingNanos:       8_100_000_000,
	}
}

func TestServiceLeasePricingDecisionBindsExactLeaseAuthority(t *testing.T) {
	in := serviceLeasePricingFixture(t)
	decision, err := newServiceLeasePricingDecision(in)
	must(t, err)
	if decision.ExecutionMode != pricingExecutionServiceLease || decision.ServiceLease == nil ||
		decision.FixedPoint == nil || decision.FixedPoint.BuyerChargeNanos != 8_100_000_000 ||
		decision.FixedPoint.SupplierEntitlementsNanos != 6_000_000_000 ||
		decision.FixedPoint.KnownVariableCostsNanos != 1_200_000_000 ||
		decision.FixedPoint.KnownCostContributionNanos != 900_000_000 ||
		decision.FixedPoint.TrueNetContributionNanos != nil {
		t.Fatalf("service pricing did not preserve exact authority or unknown true net: %+v", decision)
	}
	must(t, ValidateServiceLeasePricingDecisionSnapshot(decision, in))
}

func TestServiceLeaseCumulativeMeteringDoesNotChangeEconomicsAtHeartbeatBoundaries(t *testing.T) {
	in := serviceLeasePricingFixture(t)
	decision, err := newServiceLeasePricingDecision(in)
	must(t, err)
	full, err := ServiceLeaseMoneyForReplicaDuration(in.Currency, *decision.ServiceLease, 3_600*1_000_000_000)
	must(t, err)
	part, err := ServiceLeaseMoneyForReplicaDuration(in.Currency, *decision.ServiceLease, 1_799*1_000_000_000)
	must(t, err)
	whole, err := ServiceLeaseMoneyForReplicaDuration(in.Currency, *decision.ServiceLease, 1_800*1_000_000_000)
	must(t, err)
	if full.BuyerCharge.Nanos <= whole.BuyerCharge.Nanos || part.SupplierPayable.Nanos <= 0 ||
		full.BuyerCharge.Nanos != full.SupplierPayable.Nanos+full.ResidencyCost.Nanos+
			full.ControlPlaneCost.Nanos+full.RiskReserve.Nanos+full.MercContribution.Nanos {
		t.Fatalf("meter authority lost conserved aggregate economics: full=%+v part=%+v", full, part)
	}
	second, err := ServiceLeaseMoneyForReplicaDuration(in.Currency, *decision.ServiceLease, 3_600*1_000_000_000)
	if err != nil || second != full {
		t.Fatalf("cumulative meter is not deterministic: got=%+v err=%v want=%+v", second, err, full)
	}
}

func TestServiceLeasePricingRefusesCeilingCurrencyAndZeroFloor(t *testing.T) {
	in := serviceLeasePricingFixture(t)
	in.BuyerDeclaredCeilingNanos--
	if _, err := newServiceLeasePricingDecision(in); err == nil {
		t.Fatal("buyer ceiling breach passed")
	}
	in = serviceLeasePricingFixture(t)
	in.Currency = MustParseCurrency("usd")
	if _, err := newServiceLeasePricingDecision(in); err == nil {
		t.Fatal("currency mismatch passed")
	}
	in = serviceLeasePricingFixture(t)
	in.SupplierNanosPerReplicaHour = 0
	if _, err := newServiceLeasePricingDecision(in); err == nil {
		t.Fatal("zero supplier floor passed")
	}
	in = serviceLeasePricingFixture(t)
	in.Profile.BuyerInputUSDPerMillionTokens += 0.01
	if _, err := newServiceLeasePricingDecision(in); err == nil {
		t.Fatal("divergent runtime authority passed")
	}
}
