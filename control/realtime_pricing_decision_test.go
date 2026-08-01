package main

import (
	"strings"
	"testing"
)

func realtimePricingFixture(t *testing.T) RealtimePricingInputs {
	t.Helper()
	installSettlementCurrencyForTest(t, "cad")
	profiles := sortedVLLMProfiles()
	if len(profiles) == 0 {
		t.Fatal("no realtime profile")
	}
	profile := profiles[0]
	registration := RealtimeOfferRegistration{
		RuntimeProfileID: profile.RuntimeProfileID, RuntimeProfileSHA256: profile.ProfileSHA256,
		HWClass: "nvidia_80gb", GPUCount: 1, MemoryGBPerGPU: 80,
		SupplierInputUSDPerMillionTokens: 0.08, SupplierOutputUSDPerMillionTokens: 0.30,
	}
	placement, err := newRealtimePlacementPlan(profile, registration)
	if err != nil {
		t.Fatal(err)
	}
	return RealtimePricingInputs{
		Profile: profile, Placement: placement,
		InputCommitment: strings.Repeat("a", 64), RequestSHA256: strings.Repeat("b", 64),
		MaximumPromptTokens: 10_000, MaximumCompletionTokens: 1_000,
		EstimatedPromptTokens: 5_000, EstimatedCompletionTokens: 500,
		SupplierInputRate: 0.08, SupplierOutputRate: 0.30,
		BuyerDeclaredCeiling: 0.01, Currency: cad(t),
	}
}

func TestRealtimePricingDecisionBindsExactCompositeAuthority(t *testing.T) {
	in := realtimePricingFixture(t)
	decision, err := newRealtimePricingDecision(in)
	if err != nil {
		t.Fatal(err)
	}
	if decision.ExecutionMode != pricingExecutionRealtime || decision.Currency != "cad" ||
		decision.Realtime == nil || decision.FixedPoint == nil ||
		decision.FixedPoint.TrueNetContributionNanos != nil ||
		decision.FixedPoint.KnownCostContributionNanos <= 0 ||
		decision.FixedPoint.BuyerChargeNanos != decision.FixedPoint.SupplierEntitlementsNanos+
			decision.FixedPoint.KnownCostContributionNanos {
		t.Fatalf("realtime PricingDecision lost exact/gross-vs-net authority: %+v", decision)
	}
	if err := ValidateRealtimePricingDecisionSnapshot(decision, in); err != nil {
		t.Fatal(err)
	}
	if digest, err := pricingDecisionDigest(decision); err != nil || !validSHA256(digest) {
		t.Fatalf("pricing digest=(%q,%v)", digest, err)
	}
}

func TestRealtimePricingDecisionRefusesTamperingAndCeilingBreach(t *testing.T) {
	in := realtimePricingFixture(t)
	decision, err := newRealtimePricingDecision(in)
	if err != nil {
		t.Fatal(err)
	}
	decision.Realtime.SupplierInputNanosPerMillion++
	if err := ValidateRealtimePricingDecisionSnapshot(decision, in); err == nil {
		t.Fatal("tampered supplier offer authority rebuilt")
	}
	in.BuyerDeclaredCeiling = 0.000001
	if _, err := newRealtimePricingDecision(in); err == nil || !strings.Contains(err.Error(), "exceeds buyer ceiling") {
		t.Fatalf("buyer ceiling breach passed: %v", err)
	}
}

func TestRealtimePricingDecisionRefusesCurrencyMismatchAndZeroFloor(t *testing.T) {
	in := realtimePricingFixture(t)
	in.Currency = usd(t)
	if _, err := newRealtimePricingDecision(in); err == nil {
		t.Fatal("cross-currency realtime pricing passed")
	}
	in = realtimePricingFixture(t)
	in.SupplierInputRate = 0
	if _, err := newRealtimePricingDecision(in); err == nil {
		t.Fatal("zero supplier floor passed")
	}
	in = realtimePricingFixture(t)
	in.Profile.BuyerInputUSDPerMillionTokens += 0.01
	if _, err := newRealtimePricingDecision(in); err == nil || !strings.Contains(err.Error(), "embedded authority") {
		t.Fatalf("divergent runtime pricing authority passed: %v", err)
	}
}
