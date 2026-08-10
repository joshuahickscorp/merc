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

// A version-1 realtime authority must agree with the durable contract's
// supplier rates, not only its buyer rates.
//
// TestAttachRealtimeContractPricingRefusesDigestAndLegacyAuthorityDrift covers
// drift, but every contract it builds is version 2, so it only ever exercises
// the reference-rate branch. The legacy branch — the `else if` that guards
// pre-FX authorities — had no test at all, and a mutation that deletes its
// supplier-input term survives the entire suite.
//
// What that would let through: a legacy contract whose frozen authority pays
// the supplier at a rate the durable contract does not record. The buyer side
// still reconciles, so nothing else notices; the divergence is entirely in what
// the supplier is owed.
func TestLegacyRealtimeAuthorityMustAgreeWithDurableSupplierRates(t *testing.T) {
	in := realtimePricingFixture(t)
	decision, err := newRealtimePricingDecision(in)
	must(t, err)
	placementSHA, err := realtimePlacementPlanDigest(in.Placement)
	must(t, err)

	// Downgrade the v2 authority to the legacy shape: settlement nanos only, no
	// reference rates, no FX, legacy rounding policy.
	legacy := *decision.Realtime
	legacy.Version = realtimePricingLegacyVersion
	legacy.RoundingPolicy = realtimePricingLegacyRounding
	legacy.ReferenceCurrency, legacy.SettlementCurrency = "", ""
	legacy.FX = nil
	legacy.BuyerInputReferenceNanosPerMillion = 0
	legacy.BuyerOutputReferenceNanosPerMillion = 0
	legacy.SupplierInputReferenceNanosPerMillion = 0
	legacy.SupplierOutputReferenceNanosPerMillion = 0
	legacy.ReferenceExpectedBuyerChargeNanos = 0
	legacy.ReferenceMaximumBuyerChargeNanos = 0
	legacy.BuyerDeclaredCeilingReferenceNanos = 0

	legacyDecision := decision
	legacyDecision.Realtime = &legacy
	raw, err := json.Marshal(legacyDecision)
	must(t, err)
	digest, err := pricingDecisionDigest(legacyDecision)
	must(t, err)
	expected, maximum, err := realtimePricingLegacyProjection(legacyDecision)
	must(t, err)

	// A legacy authority carries settlement nanos, and the durable contract
	// carries the float projection of those same rates. Deriving one from the
	// other is what makes the sound case genuinely sound rather than
	// accidentally equal when FX happens to be 1.
	rate := func(nanos int64) float64 { return float64(nanos) / float64(NanosPerMajorUnit) }

	base := RealtimeContract{
		RuntimeProfileID: in.Profile.RuntimeProfileID, RuntimeProfileSHA256: in.Profile.ProfileSHA256,
		InputCommitment: in.InputCommitment, RequestSHA256: in.RequestSHA256,
		PlacementPlan: in.Placement, PlacementPlanSHA256: placementSHA,
		MaximumPriceUSD: maximum, EstimatedPriceUSD: expected,
		BuyerInputUSDPerMillionTokens:     rate(legacy.BuyerInputNanosPerMillion),
		BuyerOutputUSDPerMillionTokens:    rate(legacy.BuyerOutputNanosPerMillion),
		SupplierInputUSDPerMillionTokens:  rate(legacy.SupplierInputNanosPerMillion),
		SupplierOutputUSDPerMillionTokens: rate(legacy.SupplierOutputNanosPerMillion),
		MaximumPromptTokens:               in.MaximumPromptTokens, MaximumCompletionTokens: in.MaximumCompletionTokens,
		EstimatedPromptTokens: in.EstimatedPromptTokens, EstimatedCompletionTokens: in.EstimatedCompletionTokens,
		BuyerDeclaredCeilingNanos: legacy.BuyerDeclaredCeilingNanos,
		Currency:                  in.Currency.Code(), PricingDecisionSHA256: digest,
	}

	sound := base
	if err := attachRealtimeContractPricing(&sound, raw); err != nil || sound.Pricing == nil {
		t.Fatalf("a well-formed legacy realtime authority did not attach: pricing=%v err=%v", sound.Pricing, err)
	}

	// Each rate the legacy branch is supposed to bind, one at a time, so a
	// refusal cannot be credited to the wrong term.
	for name, drift := range map[string]func(*RealtimeContract){
		"supplier input":  func(c *RealtimeContract) { c.SupplierInputUSDPerMillionTokens += 0.001 },
		"supplier output": func(c *RealtimeContract) { c.SupplierOutputUSDPerMillionTokens += 0.001 },
		"buyer input":     func(c *RealtimeContract) { c.BuyerInputUSDPerMillionTokens += 0.001 },
		"buyer output":    func(c *RealtimeContract) { c.BuyerOutputUSDPerMillionTokens += 0.001 },
	} {
		drifted := base
		drift(&drifted)
		err := attachRealtimeContractPricing(&drifted, raw)
		if err == nil {
			t.Fatalf("a legacy authority whose %s rate disagrees with the durable contract attached anyway; "+
				"the legacy rate comparison does not bind that term", name)
		}
		if !strings.Contains(err.Error(), "legacy realtime rates disagree") {
			t.Fatalf("legacy %s drift was refused for the wrong reason: %v", name, err)
		}
	}
}

// The buyer's ceiling must hold in the currency they actually pay in, not only
// in the reference currency.
//
// Both ceiling checks are two-armed disjunctions over the same quantity in two
// currencies — reference and settlement. Every existing ceiling test runs in
// USD, where the two are the same number, so the reference arm always fires
// first and the settlement arm has never been exercised. Deleting it survives
// the whole suite.
//
// The arms genuinely differ, and deliberately so. convertRealtimeReferenceNanos
// rounds charges UP so conversion cannot erase a positive liability, and rounds
// buyer ceilings DOWN so conversion cannot spend more than the cap the buyer
// supplied. Under a non-unit FX rate with a remainder, a maximum charge that
// exactly equals the ceiling in USD converts to a settlement maximum ABOVE the
// converted settlement ceiling — the reference arm sees equality and passes,
// and only the settlement arm refuses.
//
// What the missing guard would allow: a buyer's declared ceiling honoured in
// USD and quietly exceeded in CAD, by the width of the rounding step, on every
// contract priced at exactly the cap.
func TestBuyerCeilingHoldsInTheSettlementCurrencyNotOnlyTheReferenceOne(t *testing.T) {
	in := realtimePricingFixture(t)
	// The shared CAD fixture uses 1.37, and this fixture's reference maximum
	// happens to convert exactly at that rate — no remainder, so no divergence,
	// and the invariant would not be exercised at all. Real FX rates carry more
	// precision than two decimals; this one produces a one-nano rounding step,
	// which is the smallest case the settlement arm has to catch.
	t.Setenv(priceFXRateEnv, "1.3712345")
	t.Setenv(priceFXRevisionEnv, "settlement-ceiling-rounding-2026-08-10")

	// Learn this fixture's exact reference maximum by pricing it under a
	// ceiling that cannot bind.
	probe := in
	probe.BuyerDeclaredCeiling = 0
	probe.BuyerDeclaredCeilingReferenceNanos = 0
	priced, err := newRealtimePricingDecision(probe)
	must(t, err)
	referenceMaximum := priced.Realtime.ReferenceMaximumBuyerChargeNanos
	if referenceMaximum <= 0 {
		t.Fatalf("fixture produced no reference maximum (%d); the ceiling below would not bind",
			referenceMaximum)
	}

	// Set the ceiling to exactly the reference maximum. In USD this is legal to
	// the nano: the charge equals the cap and does not exceed it.
	atCap := in
	atCap.BuyerDeclaredCeilingReferenceNanos = referenceMaximum
	atCap.BuyerDeclaredCeiling = float64(referenceMaximum) / float64(NanosPerMajorUnit)

	_, err = newRealtimePricingDecision(atCap)
	if err == nil {
		t.Fatal("a contract priced at exactly the buyer's USD ceiling was accepted under a " +
			"non-unit FX rate. The maximum charge rounds UP into settlement and the ceiling " +
			"rounds DOWN, so the buyer is charged above the cap they declared, in the currency " +
			"they actually pay in. The reference arm cannot catch this — it sees equality.")
	}
	if !strings.Contains(err.Error(), "exceeds buyer ceiling") {
		t.Fatalf("refused for the wrong reason: %v", err)
	}

	// And the refusal must be the SETTLEMENT arm doing the work, not the
	// reference arm. If reference maximum still exceeded reference ceiling the
	// test would pass for a reason that has nothing to do with the gap.
	if !strings.Contains(err.Error(), "exceeds buyer ceiling") ||
		!strings.Contains(err.Error(), "nano-cad") {
		t.Fatalf("refusal does not name the settlement-currency amounts, so it cannot be "+
			"attributed to the settlement arm: %v", err)
	}
}
