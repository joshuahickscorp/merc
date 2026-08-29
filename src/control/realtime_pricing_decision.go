package main

import (
	"errors"
	"fmt"
	"reflect"
)

const (
	pricingExecutionRealtime        = "realtime_physical"
	realtimePricingAuthorityVersion = 2
	realtimePricingLegacyVersion    = 1
	realtimePricingRoundingPolicy   = "realtime-token-reference-fx-nanos-v2"
	realtimePricingLegacyRounding   = "realtime-token-nanos-v1"
)

// RealtimePricingAuthority is the immutable input set for one physical
// realtime PricingDecision. Buyer rates come from the embedded runtime profile;
// supplier rates come from the selected offer. Counts are token bounds, not
// observed settlement usage.
type RealtimePricingAuthority struct {
	Version                                int                  `json:"version"`
	Currency                               string               `json:"currency"`
	ReferenceCurrency                      string               `json:"reference_currency,omitempty"`
	SettlementCurrency                     string               `json:"settlement_currency,omitempty"`
	FX                                     *RealtimeFXAuthority `json:"fx,omitempty"`
	RuntimeProfileID                       string               `json:"runtime_profile_id"`
	RuntimeProfileSHA256                   string               `json:"runtime_profile_sha256"`
	PlacementPlanSHA256                    string               `json:"placement_plan_sha256"`
	InputCommitment                        string               `json:"input_commitment"`
	RequestSHA256                          string               `json:"request_sha256"`
	MaximumPromptTokens                    int64                `json:"maximum_prompt_tokens"`
	MaximumCompletionTokens                int64                `json:"maximum_completion_tokens"`
	EstimatedPromptTokens                  int64                `json:"estimated_prompt_tokens"`
	EstimatedCompletionTokens              int64                `json:"estimated_completion_tokens"`
	BuyerInputReferenceNanosPerMillion     int64                `json:"buyer_input_reference_nanos_per_million,omitempty"`
	BuyerOutputReferenceNanosPerMillion    int64                `json:"buyer_output_reference_nanos_per_million,omitempty"`
	SupplierInputReferenceNanosPerMillion  int64                `json:"supplier_input_reference_nanos_per_million,omitempty"`
	SupplierOutputReferenceNanosPerMillion int64                `json:"supplier_output_reference_nanos_per_million,omitempty"`
	BuyerInputNanosPerMillion              int64                `json:"buyer_input_nanos_per_million"`
	BuyerOutputNanosPerMillion             int64                `json:"buyer_output_nanos_per_million"`
	SupplierInputNanosPerMillion           int64                `json:"supplier_input_nanos_per_million"`
	SupplierOutputNanosPerMillion          int64                `json:"supplier_output_nanos_per_million"`
	ReferenceExpectedBuyerChargeNanos      int64                `json:"reference_expected_buyer_charge_nanos,omitempty"`
	ReferenceMaximumBuyerChargeNanos       int64                `json:"reference_maximum_buyer_charge_nanos,omitempty"`
	BuyerDeclaredCeilingReferenceNanos     int64                `json:"buyer_declared_ceiling_reference_nanos,omitempty"`
	BuyerDeclaredCeilingNanos              int64                `json:"buyer_declared_ceiling_nanos,omitempty"`
	RoundingPolicy                         string               `json:"rounding_policy"`
}

type RealtimePricingInputs struct {
	Profile                            VLLMRuntimeProfile
	Placement                          RealtimePlacementPlan
	InputCommitment                    string
	RequestSHA256                      string
	MaximumPromptTokens                int64
	MaximumCompletionTokens            int64
	EstimatedPromptTokens              int64
	EstimatedCompletionTokens          int64
	SupplierInputRate                  float64
	SupplierOutputRate                 float64
	BuyerDeclaredCeiling               float64
	BuyerDeclaredCeilingReferenceNanos int64
	FX                                 RealtimeFXAuthority
	Currency                           Currency
}

func validateRealtimePricingAuthority(a RealtimePricingAuthority, currency string) error {
	if a.Currency != currency {
		return errors.New("realtime pricing authority currency does not match PricingDecision")
	}
	settlement, settlementErr := ParseCurrency(currency)
	if settlementErr != nil {
		return errors.New("realtime pricing authority settlement currency is unsupported")
	}
	switch a.Version {
	case realtimePricingLegacyVersion:
		if a.RoundingPolicy != realtimePricingLegacyRounding {
			return errors.New("legacy realtime pricing authority has unsupported rounding policy")
		}
		if a.ReferenceCurrency != "" || a.SettlementCurrency != "" ||
			a.FX != nil ||
			a.BuyerInputReferenceNanosPerMillion != 0 ||
			a.BuyerOutputReferenceNanosPerMillion != 0 ||
			a.SupplierInputReferenceNanosPerMillion != 0 ||
			a.SupplierOutputReferenceNanosPerMillion != 0 ||
			a.ReferenceExpectedBuyerChargeNanos != 0 ||
			a.ReferenceMaximumBuyerChargeNanos != 0 ||
			a.BuyerDeclaredCeilingReferenceNanos != 0 {
			return errors.New("legacy realtime pricing authority carries future FX fields")
		}
	case realtimePricingAuthorityVersion:
		if a.ReferenceCurrency != realtimeReferenceCurrency ||
			a.SettlementCurrency != currency ||
			a.RoundingPolicy != realtimePricingRoundingPolicy {
			return errors.New("realtime pricing authority has unsupported currency pair or rounding policy")
		}
		if a.FX == nil {
			return errors.New("realtime pricing authority lacks frozen FX authority")
		}
		if err := validateRealtimeFXAuthority(*a.FX, settlement); err != nil {
			return err
		}
		if a.FX.ReferenceCurrency != a.ReferenceCurrency ||
			a.FX.SettlementCurrency != a.SettlementCurrency {
			return errors.New("realtime pricing authority currency pair disagrees with FX authority")
		}
	default:
		return errors.New("realtime pricing authority has unsupported version")
	}
	if a.RuntimeProfileID == "" || !validSHA256(a.RuntimeProfileSHA256) ||
		!validSHA256(a.PlacementPlanSHA256) || !validSHA256(a.InputCommitment) ||
		!validSHA256(a.RequestSHA256) {
		return errors.New("realtime pricing authority lacks immutable identities")
	}
	if a.MaximumPromptTokens <= 0 || a.MaximumCompletionTokens <= 0 ||
		a.EstimatedPromptTokens <= 0 || a.EstimatedCompletionTokens <= 0 ||
		a.EstimatedPromptTokens > a.MaximumPromptTokens ||
		a.EstimatedCompletionTokens > a.MaximumCompletionTokens {
		return errors.New("realtime pricing authority has invalid token bounds")
	}
	if a.BuyerInputNanosPerMillion <= 0 || a.BuyerOutputNanosPerMillion <= 0 ||
		a.SupplierInputNanosPerMillion <= 0 || a.SupplierOutputNanosPerMillion <= 0 {
		return errors.New("realtime pricing authority has a zero token-rate floor")
	}
	if a.BuyerDeclaredCeilingNanos < 0 || a.BuyerDeclaredCeilingReferenceNanos < 0 {
		return errors.New("realtime pricing authority has a negative buyer ceiling")
	}
	if a.Version == realtimePricingAuthorityVersion {
		if a.BuyerInputReferenceNanosPerMillion <= 0 ||
			a.BuyerOutputReferenceNanosPerMillion <= 0 ||
			a.SupplierInputReferenceNanosPerMillion <= 0 ||
			a.SupplierOutputReferenceNanosPerMillion <= 0 ||
			a.ReferenceExpectedBuyerChargeNanos <= 0 ||
			a.ReferenceMaximumBuyerChargeNanos < a.ReferenceExpectedBuyerChargeNanos {
			return errors.New("realtime pricing authority lacks positive reference-currency nanos")
		}
		pairs := [][2]int64{
			{a.BuyerInputReferenceNanosPerMillion, a.BuyerInputNanosPerMillion},
			{a.BuyerOutputReferenceNanosPerMillion, a.BuyerOutputNanosPerMillion},
			{a.SupplierInputReferenceNanosPerMillion, a.SupplierInputNanosPerMillion},
			{a.SupplierOutputReferenceNanosPerMillion, a.SupplierOutputNanosPerMillion},
		}
		for _, pair := range pairs {
			converted, err := convertRealtimeReferenceRate(
				NanoMajorPerMillionTokens(pair[0]), settlement, *a.FX)
			if err != nil || int64(converted) != pair[1] {
				return errors.New("realtime pricing authority settlement rate disagrees with frozen reference rate and FX")
			}
		}
		if (a.BuyerDeclaredCeilingReferenceNanos == 0) != (a.BuyerDeclaredCeilingNanos == 0) {
			return errors.New("realtime pricing authority has only one side of the buyer currency ceiling")
		}
		if a.BuyerDeclaredCeilingReferenceNanos > 0 {
			converted, err := convertRealtimeReferenceNanos(
				a.BuyerDeclaredCeilingReferenceNanos, settlement, *a.FX, false)
			if err != nil || converted.Nanos != a.BuyerDeclaredCeilingNanos {
				return errors.New("realtime pricing authority buyer ceiling disagrees with frozen FX")
			}
		}
	}
	return nil
}

func newRealtimePricingDecision(in RealtimePricingInputs) (PricingDecision, error) {
	if err := RequireSettlementCurrency(in.Currency.Code()); err != nil {
		return PricingDecision{}, err
	}
	if err := validateVLLMRuntimeProfile(in.Profile); err != nil {
		return PricingDecision{}, fmt.Errorf("runtime profile: %w", err)
	}
	governedProfile, ok := vllmProfileByID(in.Profile.RuntimeProfileID)
	if !ok || !reflect.DeepEqual(in.Profile, governedProfile) {
		return PricingDecision{}, errors.New("realtime pricing profile does not match embedded authority")
	}
	if in.Profile.ProfileSHA256 == "" || in.Placement.RuntimeProfileID != in.Profile.RuntimeProfileID ||
		in.Placement.RuntimeProfileSHA256 != in.Profile.ProfileSHA256 {
		return PricingDecision{}, errors.New("realtime placement does not bind the runtime profile")
	}
	placementSHA, err := realtimePlacementPlanDigest(in.Placement)
	if err != nil {
		return PricingDecision{}, err
	}
	fx := in.FX
	if zeroRealtimeFXAuthority(fx) {
		fx, err = loadRealtimeFXAuthority(in.Currency)
		if err != nil {
			return PricingDecision{}, err
		}
	} else if err := validateRealtimeFXAuthority(fx, in.Currency); err != nil {
		return PricingDecision{}, err
	}
	buyerInputReference, err := nanoRatePerMillionFromFloat(in.Profile.BuyerInputUSDPerMillionTokens)
	if err != nil {
		return PricingDecision{}, err
	}
	buyerOutputReference, err := nanoRatePerMillionFromFloat(in.Profile.BuyerOutputUSDPerMillionTokens)
	if err != nil {
		return PricingDecision{}, err
	}
	supplierInputReference, err := nanoRatePerMillionFromFloat(in.SupplierInputRate)
	if err != nil {
		return PricingDecision{}, err
	}
	supplierOutputReference, err := nanoRatePerMillionFromFloat(in.SupplierOutputRate)
	if err != nil {
		return PricingDecision{}, err
	}
	if err := validateRealtimeOfferRates(in.Profile, RealtimeOfferRegistration{
		SupplierInputUSDPerMillionTokens:  in.SupplierInputRate,
		SupplierOutputUSDPerMillionTokens: in.SupplierOutputRate,
	}); err != nil {
		return PricingDecision{}, err
	}
	buyerInput, err := convertRealtimeReferenceRate(buyerInputReference, in.Currency, fx)
	if err != nil {
		return PricingDecision{}, err
	}
	buyerOutput, err := convertRealtimeReferenceRate(buyerOutputReference, in.Currency, fx)
	if err != nil {
		return PricingDecision{}, err
	}
	supplierInput, err := convertRealtimeReferenceRate(supplierInputReference, in.Currency, fx)
	if err != nil {
		return PricingDecision{}, err
	}
	supplierOutput, err := convertRealtimeReferenceRate(supplierOutputReference, in.Currency, fx)
	if err != nil {
		return PricingDecision{}, err
	}
	referenceExpected, err := realtimeReferenceTokenCharge(
		in.EstimatedPromptTokens, in.EstimatedCompletionTokens,
		buyerInputReference, buyerOutputReference, false)
	if err != nil {
		return PricingDecision{}, err
	}
	referenceMaximum, err := realtimeReferenceTokenCharge(
		in.MaximumPromptTokens, in.MaximumCompletionTokens,
		buyerInputReference, buyerOutputReference, false)
	if err != nil {
		return PricingDecision{}, err
	}
	referenceSupplierExpected, err := realtimeReferenceTokenCharge(
		in.EstimatedPromptTokens, in.EstimatedCompletionTokens,
		supplierInputReference, supplierOutputReference, true)
	if err != nil {
		return PricingDecision{}, err
	}
	buyerExpected, err := convertRealtimeReferenceNanos(referenceExpected.Nanos, in.Currency, fx, true)
	if err != nil {
		return PricingDecision{}, err
	}
	buyerMaximum, err := convertRealtimeReferenceNanos(referenceMaximum.Nanos, in.Currency, fx, true)
	if err != nil {
		return PricingDecision{}, err
	}
	supplierExpected, err := convertRealtimeReferenceNanos(referenceSupplierExpected.Nanos, in.Currency, fx, true)
	if err != nil {
		return PricingDecision{}, err
	}
	referenceCeiling := in.BuyerDeclaredCeilingReferenceNanos
	if referenceCeiling < 0 {
		return PricingDecision{}, errors.New("realtime buyer USD ceiling cannot be negative")
	}
	if referenceCeiling == 0 {
		referenceCeiling, err = referenceCeilingNanosFromLegacyFloat(in.BuyerDeclaredCeiling)
		if err != nil {
			return PricingDecision{}, err
		}
	} else if in.BuyerDeclaredCeiling != 0 &&
		(!finiteNonNegative(in.BuyerDeclaredCeiling) ||
			in.BuyerDeclaredCeiling != float64(referenceCeiling)/float64(NanosPerMajorUnit)) {
		return PricingDecision{}, errors.New("realtime buyer USD ceiling float projection and exact nanos disagree")
	}
	authority := RealtimePricingAuthority{
		Version: realtimePricingAuthorityVersion, Currency: in.Currency.Code(),
		ReferenceCurrency: realtimeReferenceCurrency, SettlementCurrency: in.Currency.Code(), FX: &fx,
		RuntimeProfileID: in.Profile.RuntimeProfileID, RuntimeProfileSHA256: in.Profile.ProfileSHA256,
		PlacementPlanSHA256: placementSHA, InputCommitment: in.InputCommitment,
		RequestSHA256: in.RequestSHA256, MaximumPromptTokens: in.MaximumPromptTokens,
		MaximumCompletionTokens:                in.MaximumCompletionTokens,
		EstimatedPromptTokens:                  in.EstimatedPromptTokens,
		EstimatedCompletionTokens:              in.EstimatedCompletionTokens,
		BuyerInputReferenceNanosPerMillion:     int64(buyerInputReference),
		BuyerOutputReferenceNanosPerMillion:    int64(buyerOutputReference),
		SupplierInputReferenceNanosPerMillion:  int64(supplierInputReference),
		SupplierOutputReferenceNanosPerMillion: int64(supplierOutputReference),
		BuyerInputNanosPerMillion:              int64(buyerInput), BuyerOutputNanosPerMillion: int64(buyerOutput),
		SupplierInputNanosPerMillion: int64(supplierInput), SupplierOutputNanosPerMillion: int64(supplierOutput),
		ReferenceExpectedBuyerChargeNanos:  referenceExpected.Nanos,
		ReferenceMaximumBuyerChargeNanos:   referenceMaximum.Nanos,
		BuyerDeclaredCeilingReferenceNanos: referenceCeiling,
		RoundingPolicy:                     realtimePricingRoundingPolicy,
	}
	if referenceCeiling > 0 {
		ceiling, err := convertRealtimeReferenceNanos(referenceCeiling, in.Currency, fx, false)
		if err != nil {
			return PricingDecision{}, err
		}
		authority.BuyerDeclaredCeilingNanos = ceiling.Nanos
	}
	if err := validateRealtimePricingAuthority(authority, in.Currency.Code()); err != nil {
		return PricingDecision{}, err
	}
	contribution, err := buyerExpected.Sub(supplierExpected)
	if err != nil || contribution.Nanos <= 0 || buyerExpected.Nanos <= 0 ||
		buyerMaximum.Nanos < buyerExpected.Nanos || supplierExpected.Nanos <= 0 {
		return PricingDecision{}, errors.New("realtime pricing lacks positive conserved expected/max authority")
	}
	if referenceCeiling > 0 && (referenceMaximum.Nanos > referenceCeiling ||
		buyerMaximum.Nanos > authority.BuyerDeclaredCeilingNanos) {
		return PricingDecision{}, fmt.Errorf(
			"maximum realtime price %d nano-USD / %d nano-%s exceeds buyer ceiling %d nano-USD / %d nano-%s",
			referenceMaximum.Nanos, buyerMaximum.Nanos, in.Currency.Code(), referenceCeiling,
			authority.BuyerDeclaredCeilingNanos, in.Currency.Code())
	}
	unknowns := []string{"processor fee", "control-plane cost", "storage cost", "egress cost", "risk reserve"}
	decision := PricingDecision{
		Version: pricingDecisionVersion, PolicyRevision: pricingDecisionPolicyRevision,
		ExecutionMode: pricingExecutionRealtime, Currency: in.Currency.Code(), Tier: "realtime",
		Realtime: &authority, BillableUnits: float64(in.EstimatedPromptTokens + in.EstimatedCompletionTokens),
		BuyerPrice: buyerExpected.USDFloat(), MaximumBuyerPrice: buyerMaximum.USDFloat(),
		PrimarySupplierCost:  PricingCostComponent{Status: pricingCostModeled, Amount: supplierExpected.USDFloat(), Basis: "selected USD supplier offer converted through frozen FX at estimated token bounds"},
		VerificationCost:     PricingCostComponent{Status: pricingCostNotApplicable, Basis: "V0 response-byte and usage reconciliation has no redundant execution"},
		PaymentCost:          PricingCostComponent{Status: pricingCostUnknown, Basis: "processor fee is allocated only from provider cash evidence"},
		ControlPlaneCost:     PricingCostComponent{Status: pricingCostUnknown, Basis: "realtime control-plane cost cohort is not calibrated"},
		StorageCost:          PricingCostComponent{Status: pricingCostUnknown, Basis: "best-effort exact-cache storage cost is unattributed"},
		EgressCost:           PricingCostComponent{Status: pricingCostUnknown, Basis: "response egress cost is unattributed"},
		ProviderCost:         PricingCostComponent{Status: pricingCostNotApplicable, Basis: "supplier ask is the platform's execution liability"},
		RiskReserve:          PricingCostComponent{Status: pricingCostUnknown, Basis: "realtime refund and failure risk cohort is not calibrated"},
		PlatformContribution: PricingCostComponent{Status: pricingCostModeled, Amount: contribution.USDFloat(), Basis: "gross spread after selected supplier entitlement; not true net"},
		FixedPoint: &FixedPointPricingDecision{
			Currency: in.Currency.Code(), BuyerChargeNanos: buyerExpected.Nanos,
			AcceptedCeilingNanos: buyerMaximum.Nanos, SupplierEntitlementsNanos: supplierExpected.Nanos,
			KnownVariableCostsNanos: 0, MercGrossSpreadNanos: contribution.Nanos,
			KnownCostContributionNanos: contribution.Nanos, UnknownCostCategories: unknowns,
		},
		Confidence:  0.5,
		Assumptions: []string{"token maxima are conservative byte/output bounds", "selected USD supplier ask and governed FX remain frozen for settlement"},
		Unknowns:    unknowns,
	}
	if in.Profile.BenchmarkStatus == "PASSED" {
		decision.Confidence = 0.8
	}
	if err := validatePricingCostShape(decision); err != nil {
		return PricingDecision{}, err
	}
	return decision, nil
}

func ValidateRealtimePricingDecisionSnapshot(decision PricingDecision, in RealtimePricingInputs) error {
	rebuilt, err := newRealtimePricingDecision(in)
	if err != nil {
		return err
	}
	if !reflect.DeepEqual(decision, rebuilt) {
		return errors.New("realtime pricing decision does not match its deterministic authority")
	}
	return nil
}

// realtimePhysicalMoneyFromAuthority derives settlement from the frozen
// PricingDecision only. Version 2 first computes the USD reference charge and
// entitlement, then converts each complete amount through the frozen exact FX
// factor. Version 1 remains readable under its historical same-number policy;
// it is never produced for new work.
func realtimePhysicalMoneyFromAuthority(
	a RealtimePricingAuthority,
	promptTokens, completionTokens int64,
) (buyer, supplier MoneyNanos, err error) {
	settlement, err := ParseCurrency(a.Currency)
	if err != nil {
		return MoneyNanos{}, MoneyNanos{}, err
	}
	if err := validateRealtimePricingAuthority(a, a.Currency); err != nil {
		return MoneyNanos{}, MoneyNanos{}, err
	}
	switch a.Version {
	case realtimePricingLegacyVersion:
		buyer, err = BuyerRealtimeTokenChargeNanos(
			settlement, promptTokens, completionTokens,
			NanoMajorPerMillionTokens(a.BuyerInputNanosPerMillion),
			NanoMajorPerMillionTokens(a.BuyerOutputNanosPerMillion))
		if err != nil {
			return MoneyNanos{}, MoneyNanos{}, err
		}
		supplier, err = SupplierRealtimeTokenEntitlementNanos(
			settlement, promptTokens, completionTokens,
			NanoMajorPerMillionTokens(a.SupplierInputNanosPerMillion),
			NanoMajorPerMillionTokens(a.SupplierOutputNanosPerMillion))
		return buyer, supplier, err
	case realtimePricingAuthorityVersion:
		referenceBuyer, err := realtimeReferenceTokenCharge(
			promptTokens, completionTokens,
			NanoMajorPerMillionTokens(a.BuyerInputReferenceNanosPerMillion),
			NanoMajorPerMillionTokens(a.BuyerOutputReferenceNanosPerMillion), false)
		if err != nil {
			return MoneyNanos{}, MoneyNanos{}, err
		}
		referenceSupplier, err := realtimeReferenceTokenCharge(
			promptTokens, completionTokens,
			NanoMajorPerMillionTokens(a.SupplierInputReferenceNanosPerMillion),
			NanoMajorPerMillionTokens(a.SupplierOutputReferenceNanosPerMillion), true)
		if err != nil {
			return MoneyNanos{}, MoneyNanos{}, err
		}
		buyer, err = convertRealtimeReferenceNanos(referenceBuyer.Nanos, settlement, *a.FX, true)
		if err != nil {
			return MoneyNanos{}, MoneyNanos{}, err
		}
		supplier, err = convertRealtimeReferenceNanos(referenceSupplier.Nanos, settlement, *a.FX, true)
		return buyer, supplier, err
	default:
		return MoneyNanos{}, MoneyNanos{}, errors.New("unsupported realtime pricing authority version")
	}
}

// validateFrozenRealtimePricingDecision validates a persisted decision against
// its own immutable bytes. It deliberately does not consult today's embedded
// runtime profile or today's FX environment; those are current-admission
// authorities and cannot redefine historical money.
func validateFrozenRealtimePricingDecision(p PricingDecision) error {
	if p.ExecutionMode != pricingExecutionRealtime || p.Realtime == nil || p.FixedPoint == nil {
		return errors.New("frozen realtime pricing decision lacks physical authority")
	}
	if err := validatePricingCostShape(p); err != nil {
		return err
	}
	a := *p.Realtime
	expectedBuyer, expectedSupplier, err := realtimePhysicalMoneyFromAuthority(
		a, a.EstimatedPromptTokens, a.EstimatedCompletionTokens)
	if err != nil {
		return err
	}
	maximumBuyer, _, err := realtimePhysicalMoneyFromAuthority(
		a, a.MaximumPromptTokens, a.MaximumCompletionTokens)
	if err != nil {
		return err
	}
	contribution, err := expectedBuyer.Sub(expectedSupplier)
	if err != nil || contribution.Nanos <= 0 {
		return errors.New("frozen realtime pricing authority has no positive contribution")
	}
	if p.FixedPoint.BuyerChargeNanos != expectedBuyer.Nanos ||
		p.FixedPoint.AcceptedCeilingNanos != maximumBuyer.Nanos ||
		p.FixedPoint.SupplierEntitlementsNanos != expectedSupplier.Nanos ||
		p.FixedPoint.KnownCostContributionNanos != contribution.Nanos ||
		p.BuyerPrice != expectedBuyer.USDFloat() ||
		p.MaximumBuyerPrice != maximumBuyer.USDFloat() ||
		p.PrimarySupplierCost.Amount != expectedSupplier.USDFloat() ||
		p.PlatformContribution.Amount != contribution.USDFloat() {
		return errors.New("frozen realtime pricing decision money disagrees with its exact authority")
	}
	if a.Version == realtimePricingAuthorityVersion {
		referenceExpected, err := realtimeReferenceTokenCharge(
			a.EstimatedPromptTokens, a.EstimatedCompletionTokens,
			NanoMajorPerMillionTokens(a.BuyerInputReferenceNanosPerMillion),
			NanoMajorPerMillionTokens(a.BuyerOutputReferenceNanosPerMillion), false)
		if err != nil || referenceExpected.Nanos != a.ReferenceExpectedBuyerChargeNanos {
			return errors.New("frozen realtime expected USD charge disagrees with source rate authority")
		}
		referenceMaximum, err := realtimeReferenceTokenCharge(
			a.MaximumPromptTokens, a.MaximumCompletionTokens,
			NanoMajorPerMillionTokens(a.BuyerInputReferenceNanosPerMillion),
			NanoMajorPerMillionTokens(a.BuyerOutputReferenceNanosPerMillion), false)
		if err != nil || referenceMaximum.Nanos != a.ReferenceMaximumBuyerChargeNanos {
			return errors.New("frozen realtime maximum USD charge disagrees with source rate authority")
		}
		if a.BuyerDeclaredCeilingReferenceNanos > 0 &&
			(a.ReferenceMaximumBuyerChargeNanos > a.BuyerDeclaredCeilingReferenceNanos ||
				maximumBuyer.Nanos > a.BuyerDeclaredCeilingNanos) {
			return errors.New("frozen realtime maximum exceeds its buyer USD ceiling")
		}
	}
	return nil
}

func realtimePricingReferenceLegacyProjection(decision PricingDecision) (expected, maximum float64, err error) {
	if decision.ExecutionMode != pricingExecutionRealtime || decision.Realtime == nil {
		return 0, 0, errors.New("USD projection requires physical realtime pricing")
	}
	a := decision.Realtime
	var expectedNanos, maximumNanos int64
	switch a.Version {
	case realtimePricingAuthorityVersion:
		expectedNanos = a.ReferenceExpectedBuyerChargeNanos
		maximumNanos = a.ReferenceMaximumBuyerChargeNanos
	case realtimePricingLegacyVersion:
		if decision.Currency != realtimeReferenceCurrency || decision.FixedPoint == nil {
			return 0, 0, errors.New("legacy non-USD realtime pricing has no truthful USD projection")
		}
		expectedNanos = decision.FixedPoint.BuyerChargeNanos
		maximumNanos = decision.FixedPoint.AcceptedCeilingNanos
	default:
		return 0, 0, errors.New("unsupported realtime pricing authority version")
	}
	reference := MustParseCurrency(realtimeReferenceCurrency)
	expectedMoney, err := NewMoneyNanos(reference, expectedNanos)
	if err != nil {
		return 0, 0, err
	}
	maximumMoney, err := NewMoneyNanos(reference, maximumNanos)
	if err != nil {
		return 0, 0, err
	}
	expected, err = projectRealtimeNanosToMajor(expectedMoney)
	if err != nil {
		return 0, 0, err
	}
	maximum, err = projectRealtimeNanosToMajor(maximumMoney)
	return expected, maximum, err
}

func realtimePricingLegacyProjection(decision PricingDecision) (expected, maximum float64, err error) {
	if (decision.ExecutionMode != pricingExecutionRealtime && decision.ExecutionMode != pricingExecutionRealtimeReuse) || decision.FixedPoint == nil {
		return 0, 0, errors.New("legacy projection requires realtime fixed-point pricing")
	}
	currency, err := ParseCurrency(decision.Currency)
	if err != nil {
		return 0, 0, err
	}
	expectedNanos, err := NewMoneyNanos(currency, decision.FixedPoint.BuyerChargeNanos)
	if err != nil {
		return 0, 0, err
	}
	maximumNanos, err := NewMoneyNanos(currency, decision.FixedPoint.AcceptedCeilingNanos)
	if err != nil {
		return 0, 0, err
	}
	expectedMicros, err := LedgerMicrosFromNanos(expectedNanos)
	if err != nil {
		return 0, 0, err
	}
	maximumMicros, err := LedgerMicrosFromNanos(maximumNanos)
	if err != nil {
		return 0, 0, err
	}
	return microsToUSD(expectedMicros), microsToUSD(maximumMicros), nil
}
