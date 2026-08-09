package main

import (
	"encoding/json"
	"strings"
	"testing"
)

func realtimePricingFixture(t *testing.T) RealtimePricingInputs {
	t.Helper()
	installSettlementCurrencyForTest(t, "cad")
	installRealtimeCADFXForTest(t)
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
	must(t, err)
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
	must(t, err)
	if decision.ExecutionMode != pricingExecutionRealtime || decision.Currency != "cad" ||
		decision.Realtime == nil || decision.FixedPoint == nil ||
		decision.Realtime.ReferenceCurrency != "usd" ||
		decision.Realtime.SettlementCurrency != "cad" ||
		decision.Realtime.FX.ReferenceToSettlementNanos != 1_370_000_000 ||
		decision.Realtime.BuyerInputReferenceNanosPerMillion == decision.Realtime.BuyerInputNanosPerMillion ||
		decision.FixedPoint.TrueNetContributionNanos != nil ||
		decision.FixedPoint.KnownCostContributionNanos <= 0 ||
		decision.FixedPoint.BuyerChargeNanos != decision.FixedPoint.SupplierEntitlementsNanos+
			decision.FixedPoint.KnownCostContributionNanos {
		t.Fatalf("realtime PricingDecision lost exact/gross-vs-net authority: %+v", decision)
	}
	must(t, ValidateRealtimePricingDecisionSnapshot(decision, in))
	if digest, err := pricingDecisionDigest(decision); err != nil || !validSHA256(digest) {
		t.Fatalf("pricing digest=(%q,%v)", digest, err)
	}
}

func TestLegacyRealtimePricingAuthoritiesOmitFutureFXFields(t *testing.T) {
	physical, err := json.Marshal(RealtimePricingAuthority{
		Version: realtimePricingLegacyVersion, Currency: "cad",
		RoundingPolicy: realtimePricingLegacyRounding,
	})
	must(t, err)
	reuse, err := json.Marshal(RealtimeReusePricingAuthority{
		Version: realtimeReusePricingLegacyVersion, Currency: "cad",
		RoundingPolicy: realtimeReusePricingLegacyRounding,
	})
	must(t, err)
	for name, raw := range map[string][]byte{"physical": physical, "reuse": reuse} {
		if strings.Contains(string(raw), `"fx"`) ||
			strings.Contains(string(raw), `"reference_currency"`) ||
			strings.Contains(string(raw), `"settlement_currency"`) {
			t.Fatalf("legacy %s authority changed its canonical JSON with future FX fields: %s", name, raw)
		}
	}
}

func TestRealtimePricingDecisionRefusesTamperingAndCeilingBreach(t *testing.T) {
	in := realtimePricingFixture(t)
	decision, err := newRealtimePricingDecision(in)
	must(t, err)
	decision.Realtime.SupplierInputNanosPerMillion++
	if err := ValidateRealtimePricingDecisionSnapshot(decision, in); err == nil {
		t.Fatal("tampered supplier offer authority rebuilt")
	}
	in.BuyerDeclaredCeiling = 0.000001
	if _, err := newRealtimePricingDecision(in); err == nil || !strings.Contains(err.Error(), "exceeds buyer ceiling") {
		t.Fatalf("buyer ceiling breach passed: %v", err)
	}
}

func TestRealtimePricingDecisionPreservesExactCeilingBeyondFloatIntegerRange(t *testing.T) {
	in := realtimePricingFixture(t)
	const exactCeilingNanos int64 = 10_000_000_000_000_001
	in.BuyerDeclaredCeilingReferenceNanos = exactCeilingNanos
	in.BuyerDeclaredCeiling = float64(exactCeilingNanos) / float64(NanosPerMajorUnit)
	decision, err := newRealtimePricingDecision(in)
	must(t, err)
	if decision.Realtime.BuyerDeclaredCeilingReferenceNanos != exactCeilingNanos {
		t.Fatalf("exact USD ceiling crossed a float round trip: got=%d want=%d",
			decision.Realtime.BuyerDeclaredCeilingReferenceNanos, exactCeilingNanos)
	}
	in.BuyerDeclaredCeiling++
	if _, err := newRealtimePricingDecision(in); err == nil || !strings.Contains(err.Error(), "disagree") {
		t.Fatalf("inconsistent ceiling float projection was accepted: %v", err)
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

func TestAttachRealtimeContractPricingRefusesDigestAndLegacyAuthorityDrift(t *testing.T) {
	in := realtimePricingFixture(t)
	decision, err := newRealtimePricingDecision(in)
	must(t, err)
	raw, err := json.Marshal(decision)
	must(t, err)
	digest, err := pricingDecisionDigest(decision)
	must(t, err)
	expected, maximum, err := realtimePricingLegacyProjection(decision)
	must(t, err)
	placementSHA, err := realtimePlacementPlanDigest(in.Placement)
	must(t, err)
	contract := RealtimeContract{
		RuntimeProfileID: in.Profile.RuntimeProfileID, RuntimeProfileSHA256: in.Profile.ProfileSHA256,
		InputCommitment: in.InputCommitment, RequestSHA256: in.RequestSHA256,
		PlacementPlan: in.Placement, PlacementPlanSHA256: placementSHA,
		MaximumPriceUSD: maximum, EstimatedPriceUSD: expected,
		BuyerInputUSDPerMillionTokens:     in.Profile.BuyerInputUSDPerMillionTokens,
		BuyerOutputUSDPerMillionTokens:    in.Profile.BuyerOutputUSDPerMillionTokens,
		SupplierInputUSDPerMillionTokens:  in.SupplierInputRate,
		SupplierOutputUSDPerMillionTokens: in.SupplierOutputRate,
		MaximumPromptTokens:               in.MaximumPromptTokens, MaximumCompletionTokens: in.MaximumCompletionTokens,
		EstimatedPromptTokens: in.EstimatedPromptTokens, EstimatedCompletionTokens: in.EstimatedCompletionTokens,
		BuyerDeclaredCeilingNanos: decision.Realtime.BuyerDeclaredCeilingNanos,
		Currency:                  in.Currency.Code(), PricingDecisionSHA256: digest,
	}
	if err := attachRealtimeContractPricing(&contract, raw); err != nil || contract.Pricing == nil {
		t.Fatalf("valid frozen realtime authority did not attach: pricing=%v err=%v", contract.Pricing, err)
	}
	// Historical replay must use the frozen FX/profile digest, not today's
	// operator rate. A later current rate cannot rewrite accepted money.
	t.Setenv(priceFXRateEnv, "1.99")
	t.Setenv(priceFXRevisionEnv, "later-rate-must-not-reprice-history")
	contract.Pricing = nil
	if err := attachRealtimeContractPricing(&contract, raw); err != nil {
		t.Fatalf("historical realtime authority depended on current FX: %v", err)
	}
	contract.Pricing = nil
	contract.PricingDecisionSHA256 = strings.Repeat("f", 64)
	if err := attachRealtimeContractPricing(&contract, raw); err == nil || !strings.Contains(err.Error(), "digest mismatch") {
		t.Fatalf("wrong persisted PricingDecision digest passed: %v", err)
	}
	contract.PricingDecisionSHA256 = digest
	contract.SupplierInputUSDPerMillionTokens += 0.001
	if err := attachRealtimeContractPricing(&contract, raw); err == nil {
		t.Fatal("divergent legacy supplier rate passed frozen PricingDecision validation")
	}
	contract.SupplierInputUSDPerMillionTokens = in.SupplierInputRate
	contract.MaximumPromptTokens++
	if err := attachRealtimeContractPricing(&contract, raw); err == nil {
		t.Fatal("divergent legacy token bound passed frozen PricingDecision validation")
	}
}
