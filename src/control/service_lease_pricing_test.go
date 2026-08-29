package main

import (
	"context"
	"encoding/json"
	"reflect"
	"strings"
	"testing"

	"github.com/google/uuid"
)

func serviceLeasePricingFixture(t *testing.T) ServiceLeasePricingInputs {
	t.Helper()
	installSettlementCurrencyForTest(t, "usd")
	profile := sortedVLLMProfiles()[0]
	return ServiceLeasePricingInputs{
		Profile: profile, Currency: MustParseCurrency("usd"), Region: "ca-central-1",
		MinimumReplicas: 1, MaximumReplicas: 3, TermSeconds: 3600,
		MaximumP95LatencyMilliseconds:   500,
		SupplierNanosPerReplicaHour:     2_000_000_000,
		ResidencyNanosPerReplicaHour:    200_000_000,
		ControlPlaneNanosPerReplicaHour: 100_000_000,
		RiskReserveNanosPerReplicaHour:  0,
		ContributionNanosPerReplicaHour: 300_000_000,
		BuyerDeclaredCeilingNanos:       8_100_000_000,
	}
}

func TestServiceLeasePricingDecisionBindsExactLeaseAuthority(t *testing.T) {
	in := serviceLeasePricingFixture(t)
	decision, err := newServiceLeasePricingDecision(in)
	must(t, err)
	if decision.ExecutionMode != pricingExecutionServiceLease || decision.ServiceLease == nil ||
		decision.ServiceLease.Version != serviceLeasePricingAuthorityVersion ||
		decision.FixedPoint == nil || decision.FixedPoint.BuyerChargeNanos != 7_800_000_000 ||
		decision.FixedPoint.SupplierEntitlementsNanos != 6_600_000_000 ||
		decision.FixedPoint.KnownVariableCostsNanos != 300_000_000 ||
		decision.FixedPoint.KnownCostContributionNanos != 900_000_000 ||
		decision.FixedPoint.TrueNetContributionNanos != nil ||
		decision.EgressCost.Status != pricingCostUnknown || decision.RiskReserve.Status != pricingCostUnknown ||
		decision.ProviderCost.Status != pricingCostNotApplicable || len(decision.FixedPoint.UnknownCostCategories) != 4 {
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
		full.SupplierPayable.Nanos != in.SupplierNanosPerReplicaHour+in.ResidencyNanosPerReplicaHour ||
		full.RiskReserve.Nanos != 0 ||
		full.BuyerCharge.Nanos != full.SupplierPayable.Nanos+
			full.ControlPlaneCost.Nanos+full.MercContribution.Nanos {
		t.Fatalf("meter authority lost conserved aggregate economics: full=%+v part=%+v", full, part)
	}
	second, err := ServiceLeaseMoneyForReplicaDuration(in.Currency, *decision.ServiceLease, 3_600*1_000_000_000)
	if err != nil || second != full {
		t.Fatalf("cumulative meter is not deterministic: got=%+v err=%v want=%+v", second, err, full)
	}
}

func TestServiceLeaseHistoricalV1PricingReplaysWithoutV2Reinterpretation(t *testing.T) {
	in := serviceLeasePricingFixture(t)
	current, err := newServiceLeasePricingDecision(in)
	must(t, err)

	legacy := current
	legacy.Currency = "cad"
	legacyAuthority := *current.ServiceLease
	legacyAuthority.Version = serviceLeasePricingAuthorityLegacyVersion
	legacyAuthority.Currency = "cad"
	legacyAuthority.RiskReserveNanosPerReplicaHour = 100_000_000
	legacy.ServiceLease = &legacyAuthority
	legacy.BuyerPrice, legacy.MaximumBuyerPrice = 8.1, 8.1
	legacy.PrimarySupplierCost = PricingCostComponent{Status: pricingCostModeled, Amount: 6, Basis: "historical selected supplier floor"}
	legacy.ProviderCost = PricingCostComponent{Status: pricingCostModeled, Amount: 0.6, Basis: "historical residency allocation with no bound beneficiary"}
	legacy.RiskReserve = PricingCostComponent{Status: pricingCostModeled, Amount: 0.3, Basis: "historical modeled availability reserve"}
	legacy.EgressCost = PricingCostComponent{Status: pricingCostNotApplicable, Basis: "historical service decision did not price request egress"}
	legacy.FixedPoint = &FixedPointPricingDecision{
		Currency: "cad", BuyerChargeNanos: 8_100_000_000, AcceptedCeilingNanos: 8_100_000_000,
		SupplierEntitlementsNanos: 6_000_000_000, KnownVariableCostsNanos: 1_200_000_000,
		MercGrossSpreadNanos: 2_100_000_000, KnownCostContributionNanos: 900_000_000,
		UnknownCostCategories: []string{"processor fee allocation from prepaid funding receipt"},
	}
	raw, err := json.Marshal(legacy)
	must(t, err)
	digest, err := pricingDecisionDigest(legacy)
	must(t, err)
	replayed, err := decodeServiceLeasePricing(raw, digest)
	must(t, err)

	money, err := ServiceLeaseMoneyForReplicaDuration(MustParseCurrency("cad"), *replayed.ServiceLease, nanosecondsPerHour)
	must(t, err)
	if money.BuyerCharge.Nanos != 2_700_000_000 || money.SupplierPayable.Nanos != 2_000_000_000 ||
		money.ResidencyCost.Nanos != 200_000_000 || money.RiskReserve.Nanos != 100_000_000 {
		t.Fatalf("historical v1 economics were reinterpreted as v2: %+v", money)
	}
}

func TestServiceLeasePricingRefusesCeilingCurrencyAndZeroFloor(t *testing.T) {
	in := serviceLeasePricingFixture(t)
	in.BuyerDeclaredCeilingNanos = 7_800_000_000 - 1
	if _, err := newServiceLeasePricingDecision(in); err == nil {
		t.Fatal("buyer ceiling breach passed")
	}
	in = serviceLeasePricingFixture(t)
	in.Currency = MustParseCurrency("cad")
	if _, err := newServiceLeasePricingDecision(in); err == nil {
		t.Fatal("non-USD current service pricing passed without frozen FX authority")
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

func TestCurrentServiceLeasePricingRefusesCurrencyCutoverWithoutFrozenFX(t *testing.T) {
	in := serviceLeasePricingFixture(t)
	accepted, err := newServiceLeasePricingDecision(in)
	if err != nil {
		t.Fatalf("USD service pricing should be admissible: %v", err)
	}
	acceptedRaw, err := json.Marshal(accepted)
	must(t, err)
	acceptedDigest, err := pricingDecisionDigest(accepted)
	must(t, err)

	// The exact same numeric offer must not be reinterpreted after a deployment
	// cutover. Historical replay uses the frozen authority directly; only new
	// admission is refused here.
	installSettlementCurrencyForTest(t, "cad")
	replayed, err := decodeServiceLeasePricing(acceptedRaw, acceptedDigest)
	if err != nil || !reflect.DeepEqual(replayed, accepted) {
		t.Fatalf("accepted v2 USD authority stopped replaying after CAD cutover: replay=%+v err=%v", replayed, err)
	}
	if _, err := newServiceLeasePricingDecision(in); err == nil ||
		!strings.Contains(err.Error(), "settlement currency") {
		t.Fatalf("stale USD service rates survived CAD cutover: %v", err)
	}
	in.Currency = MustParseCurrency("cad")
	if _, err := newServiceLeasePricingDecision(in); err == nil ||
		!strings.Contains(err.Error(), "frozen FX-bearing") {
		t.Fatalf("CAD service rates were admitted without frozen FX authority: %v", err)
	}
}

func TestAcceptedServiceLeaseReservationMustBePositiveAndExact(t *testing.T) {
	in := serviceLeasePricingFixture(t)
	// Exercise the half-away-from-zero ledger projection rather than a ceiling
	// already divisible by one micro-unit.
	in.BuyerDeclaredCeilingNanos = 8_100_000_500
	pricing, err := newServiceLeasePricingDecision(in)
	must(t, err)
	id := uuid.New()
	lease := ServiceLease{
		ID: id, RuntimeProfileID: in.Profile.RuntimeProfileID,
		RuntimeProfileSHA256: in.Profile.ProfileSHA256, Region: in.Region,
		MinimumReplicas: in.MinimumReplicas, MaximumReplicas: in.MaximumReplicas,
		MaximumP95LatencyMillis: in.MaximumP95LatencyMilliseconds, TermSeconds: in.TermSeconds,
		Pricing: pricing, PricingAcceptanceID: &id,
	}
	if err := validateServiceLeaseAcceptedPricingBinding(lease); err == nil ||
		!strings.Contains(err.Error(), "positive prepaid reservation") {
		t.Fatalf("accepted lease with zero reserve passed: %v", err)
	}
	if err := settleFinalServiceLeaseTx(context.Background(), nil, &lease); err == nil ||
		!strings.Contains(err.Error(), "positive frozen prepaid reservation") {
		t.Fatalf("terminal settlement silently skipped an accepted zero reserve: %v", err)
	}
	want, err := LedgerMicrosFromNanos(MoneyNanos{
		Currency: in.Currency, Nanos: pricing.FixedPoint.AcceptedCeilingNanos,
	})
	must(t, err)
	if want != 8_100_001 {
		t.Fatalf("half-micro accepted ceiling projected to %d, want 8100001", want)
	}
	lease.ReservedBuyerMicros = want - 1
	if err := validateServiceLeaseAcceptedPricingBinding(lease); err == nil ||
		!strings.Contains(err.Error(), "differs from accepted pricing ceiling") {
		t.Fatalf("accepted lease with inexact reserve passed: %v", err)
	}
	lease.ReservedBuyerMicros = want
	must(t, validateServiceLeaseAcceptedPricingBinding(lease))
}
