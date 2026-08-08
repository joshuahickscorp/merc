package main

import (
	"encoding/json"
	"strings"
	"testing"
)

func realtimeReusePricingFixture(t *testing.T) RealtimeReusePricingInputs {
	t.Helper()
	installSettlementCurrencyForTest(t, "cad")
	profile := sortedVLLMProfiles()[0]
	return RealtimeReusePricingInputs{Profile: profile,
		InputCommitment: strings.Repeat("a", 64), RequestSHA256: strings.Repeat("b", 64),
		ResultCommitment: strings.Repeat("c", 64), ReuseClass: ClassExactResultReuse,
		DeliveredTokens: 7, BuyerDeclaredCeiling: 0.001, Currency: cad(t)}
}

func TestAttachRealtimeReusePricingRefusesPersistedAuthorityDrift(t *testing.T) {
	in := realtimeReusePricingFixture(t)
	p, err := newRealtimeReusePricingDecision(in)
	must(t, err)
	raw, err := json.Marshal(p)
	must(t, err)
	digest, err := pricingDecisionDigest(p)
	must(t, err)
	expected, maximum, err := realtimePricingLegacyProjection(p)
	must(t, err)
	contract := RealtimeContract{RuntimeProfileID: in.Profile.RuntimeProfileID,
		RuntimeProfileSHA256: in.Profile.ProfileSHA256, InputCommitment: in.InputCommitment,
		RequestSHA256: in.RequestSHA256, MaximumPriceUSD: maximum, EstimatedPriceUSD: expected,
		Currency: in.Currency.Code(), ReuseClass: in.ReuseClass, ReuseResultCommitment: in.ResultCommitment,
		ReuseDeliveredTokens: in.DeliveredTokens, BuyerDeclaredCeilingNanos: p.RealtimeReuse.BuyerDeclaredCeilingNanos,
		PricingDecisionSHA256: digest}
	if err := attachRealtimeContractPricing(&contract, raw); err != nil || contract.Pricing == nil {
		t.Fatalf("valid reuse pricing did not attach: %+v err=%v", contract, err)
	}
	contract.Pricing = nil
	contract.ReuseDeliveredTokens++
	if err := attachRealtimeContractPricing(&contract, raw); err == nil {
		t.Fatal("persisted delivered-token drift passed reuse PricingDecision validation")
	}
}

func TestRealtimeReusePricingDecisionBindsZeroPhysicalExactAuthority(t *testing.T) {
	in := realtimeReusePricingFixture(t)
	p, err := newRealtimeReusePricingDecision(in)
	must(t, err)
	if p.ExecutionMode != pricingExecutionRealtimeReuse || p.Realtime != nil || p.RealtimeReuse == nil ||
		p.FixedPoint == nil || p.FixedPoint.SupplierEntitlementsNanos != 0 ||
		p.FixedPoint.BuyerChargeNanos != 1_260 || p.FixedPoint.TrueNetContributionNanos != nil ||
		p.FixedPoint.KnownCostContributionNanos != p.FixedPoint.BuyerChargeNanos {
		t.Fatalf("reuse PricingDecision misstates physical work, exact money, or true net: %+v", p)
	}
	must(t, ValidateRealtimeReusePricingDecisionSnapshot(p, in))
	if digest, err := pricingDecisionDigest(p); err != nil || !validSHA256(digest) {
		t.Fatalf("reuse pricing digest=(%q,%v)", digest, err)
	}
}

func TestRealtimeReusePricingDecisionRefusesTamperCurrencyCeilingAndClass(t *testing.T) {
	in := realtimeReusePricingFixture(t)
	p, err := newRealtimeReusePricingDecision(in)
	must(t, err)
	p.RealtimeReuse.DeliveredTokens++
	if err := ValidateRealtimeReusePricingDecisionSnapshot(p, in); err == nil {
		t.Fatal("tampered delivered-token authority passed")
	}
	in = realtimeReusePricingFixture(t)
	in.Currency = usd(t)
	if _, err := newRealtimeReusePricingDecision(in); err == nil {
		t.Fatal("cross-currency reuse pricing passed")
	}
	in = realtimeReusePricingFixture(t)
	in.BuyerDeclaredCeiling = 0.000001
	if _, err := newRealtimeReusePricingDecision(in); err == nil {
		t.Fatal("reuse buyer ceiling breach passed")
	}
	in = realtimeReusePricingFixture(t)
	in.ReuseClass = "physical"
	if _, err := newRealtimeReusePricingDecision(in); err == nil {
		t.Fatal("unknown reuse class passed")
	}
	in = realtimeReusePricingFixture(t)
	in.Profile.BuyerOutputUSDPerMillionTokens += 0.01
	if _, err := newRealtimeReusePricingDecision(in); err == nil {
		t.Fatal("divergent embedded reuse profile passed")
	}
}
