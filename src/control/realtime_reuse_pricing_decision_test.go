package main

import (
	"encoding/json"
	"strings"
	"testing"
)

func realtimeReusePricingFixture(t *testing.T) RealtimeReusePricingInputs {
	t.Helper()
	installSettlementCurrencyForTest(t, "cad")
	installRealtimeCADFXForTest(t)
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
		BuyerInputUSDPerMillionTokens:  in.Profile.BuyerInputUSDPerMillionTokens,
		BuyerOutputUSDPerMillionTokens: in.Profile.BuyerOutputUSDPerMillionTokens,
		Currency:                       in.Currency.Code(), ReuseClass: in.ReuseClass, ReuseResultCommitment: in.ResultCommitment,
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
		p.RealtimeReuse.ReferenceCurrency != "usd" || p.RealtimeReuse.SettlementCurrency != "cad" ||
		p.RealtimeReuse.ReferenceBuyerChargeNanos != 1_260 ||
		p.FixedPoint.BuyerChargeNanos != 1_727 || p.FixedPoint.TrueNetContributionNanos != nil ||
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

// The reuse arm of the same gap. See
// TestBuyerCeilingHoldsInTheSettlementCurrencyNotOnlyTheReferenceOne for the
// mechanism: the ceiling check is a disjunction over reference and settlement
// amounts, charges round UP into settlement and ceilings round DOWN, and every
// existing test runs where the two currencies are one number so only the
// reference arm is ever exercised.
//
// A cache hit is the cheapest thing merc sells, which is exactly why this
// matters: at a ceiling set to the reuse charge, the rounding step is a large
// fraction of the charge itself rather than a rounding error on a big number.
func TestReuseBuyerCeilingHoldsInTheSettlementCurrencyNotOnlyTheReferenceOne(t *testing.T) {
	in := realtimeReusePricingFixture(t)
	// A rate with enough precision to leave a remainder; the shared 1.37 helper
	// converts this fixture exactly and would not exercise the arm at all.
	t.Setenv(priceFXRateEnv, "1.3712345")
	t.Setenv(priceFXRevisionEnv, "reuse-settlement-ceiling-rounding-2026-08-10")

	probe := in
	probe.BuyerDeclaredCeiling = 0
	probe.BuyerDeclaredCeilingReferenceNanos = 0
	priced, err := newRealtimeReusePricingDecision(probe)
	must(t, err)
	referenceCharge := priced.RealtimeReuse.ReferenceBuyerChargeNanos
	if referenceCharge <= 0 {
		t.Fatalf("fixture produced no reference charge (%d); the ceiling below would not bind",
			referenceCharge)
	}

	// Prove the rounding step actually exists at this rate, so a pass cannot be
	// a fixture that never diverged.
	up, err := mulDiv(referenceCharge, priced.RealtimeReuse.FX.ReferenceToSettlementNanos, realtimeFXRateScale, true)
	must(t, err)
	down, err := mulDiv(referenceCharge, priced.RealtimeReuse.FX.ReferenceToSettlementNanos, realtimeFXRateScale, false)
	must(t, err)
	if up == down {
		t.Fatalf("this FX rate converts the reuse charge exactly (%d nanos, no rounding step), "+
			"so accepting a contract at the cap would be correct and this test would prove "+
			"nothing; choose a rate with a remainder", up)
	}

	atCap := in
	atCap.BuyerDeclaredCeilingReferenceNanos = referenceCharge
	atCap.BuyerDeclaredCeiling = float64(referenceCharge) / float64(NanosPerMajorUnit)
	if _, err := newRealtimeReusePricingDecision(atCap); err == nil {
		t.Fatal("a reuse charge at exactly the buyer's USD ceiling was accepted under a " +
			"non-unit FX rate. The charge rounds UP into settlement and the ceiling rounds " +
			"DOWN, so the buyer pays above the cap they declared in the currency they are " +
			"billed in; the reference arm cannot see it, because there the two are equal.")
	} else if !strings.Contains(err.Error(), "exceeds buyer ceiling") {
		t.Fatalf("refused for the wrong reason: %v", err)
	}
}
