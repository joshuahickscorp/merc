package main

import (
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

func TestRealtimeReusePricingDecisionBindsZeroPhysicalExactAuthority(t *testing.T) {
	in := realtimeReusePricingFixture(t)
	p, err := newRealtimeReusePricingDecision(in)
	if err != nil {
		t.Fatal(err)
	}
	if p.ExecutionMode != pricingExecutionRealtimeReuse || p.Realtime != nil || p.RealtimeReuse == nil ||
		p.FixedPoint == nil || p.FixedPoint.SupplierEntitlementsNanos != 0 ||
		p.FixedPoint.BuyerChargeNanos != 1_260 || p.FixedPoint.TrueNetContributionNanos != nil ||
		p.FixedPoint.KnownCostContributionNanos != p.FixedPoint.BuyerChargeNanos {
		t.Fatalf("reuse PricingDecision misstates physical work, exact money, or true net: %+v", p)
	}
	if err := ValidateRealtimeReusePricingDecisionSnapshot(p, in); err != nil {
		t.Fatal(err)
	}
	if digest, err := pricingDecisionDigest(p); err != nil || !validSHA256(digest) {
		t.Fatalf("reuse pricing digest=(%q,%v)", digest, err)
	}
}

func TestRealtimeReusePricingDecisionRefusesTamperCurrencyCeilingAndClass(t *testing.T) {
	in := realtimeReusePricingFixture(t)
	p, err := newRealtimeReusePricingDecision(in)
	if err != nil {
		t.Fatal(err)
	}
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
