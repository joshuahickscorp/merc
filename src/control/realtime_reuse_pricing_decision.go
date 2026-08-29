package main

import (
	"errors"
	"reflect"
)

const (
	pricingExecutionRealtimeReuse        = "realtime_exact_reuse"
	realtimeReusePricingAuthorityVersion = 2
	realtimeReusePricingLegacyVersion    = 1
	realtimeReusePricingRoundingPolicy   = "realtime-reuse-reference-fx-nanos-v2"
	realtimeReusePricingLegacyRounding   = "realtime-reuse-nanos-v1"
)

type RealtimeReusePricingAuthority struct {
	Version                             int                  `json:"version"`
	Currency                            string               `json:"currency"`
	ReferenceCurrency                   string               `json:"reference_currency,omitempty"`
	SettlementCurrency                  string               `json:"settlement_currency,omitempty"`
	FX                                  *RealtimeFXAuthority `json:"fx,omitempty"`
	RuntimeProfileID                    string               `json:"runtime_profile_id"`
	RuntimeProfileSHA256                string               `json:"runtime_profile_sha256"`
	InputCommitment                     string               `json:"input_commitment"`
	RequestSHA256                       string               `json:"request_sha256"`
	ResultCommitment                    string               `json:"result_commitment"`
	ReuseClass                          string               `json:"reuse_class"`
	DeliveredTokens                     int64                `json:"delivered_tokens"`
	ReferenceFullRateNanosPerMillion    int64                `json:"reference_full_rate_nanos_per_million,omitempty"`
	FullRateNanosPerMillion             int64                `json:"full_rate_nanos_per_million"`
	ReferenceBuyerChargeNanos           int64                `json:"reference_buyer_charge_nanos,omitempty"`
	ReferenceMinimumDeliveryChargeNanos int64                `json:"reference_minimum_delivery_charge_nanos,omitempty"`
	RetainedShareNanos                  int64                `json:"retained_share_nanos"`
	MinimumDeliveryChargeNanos          int64                `json:"minimum_delivery_charge_nanos"`
	MinimumChargeApplied                bool                 `json:"minimum_charge_applied"`
	BuyerDeclaredCeilingReferenceNanos  int64                `json:"buyer_declared_ceiling_reference_nanos,omitempty"`
	BuyerDeclaredCeilingNanos           int64                `json:"buyer_declared_ceiling_nanos,omitempty"`
	RoundingPolicy                      string               `json:"rounding_policy"`
}

type RealtimeReusePricingInputs struct {
	Profile                                                      VLLMRuntimeProfile
	InputCommitment, RequestSHA256, ResultCommitment, ReuseClass string
	DeliveredTokens                                              int64
	BuyerDeclaredCeiling                                         float64
	BuyerDeclaredCeilingReferenceNanos                           int64
	FX                                                           RealtimeFXAuthority
	Currency                                                     Currency
}

func validateRealtimeReusePricingAuthority(a RealtimeReusePricingAuthority, currency string) error {
	if a.Currency != currency ||
		(a.ReuseClass != ClassExactResultReuse && a.ReuseClass != ClassCoalescedDelivery) {
		return errors.New("realtime reuse authority has unsupported currency or class")
	}
	settlement, err := ParseCurrency(currency)
	if err != nil {
		return errors.New("realtime reuse authority settlement currency is unsupported")
	}
	switch a.Version {
	case realtimeReusePricingLegacyVersion:
		if a.RoundingPolicy != realtimeReusePricingLegacyRounding ||
			a.ReferenceCurrency != "" || a.SettlementCurrency != "" ||
			a.FX != nil ||
			a.ReferenceFullRateNanosPerMillion != 0 ||
			a.ReferenceBuyerChargeNanos != 0 ||
			a.ReferenceMinimumDeliveryChargeNanos != 0 ||
			a.BuyerDeclaredCeilingReferenceNanos != 0 {
			return errors.New("legacy realtime reuse authority has unsupported or future fields")
		}
	case realtimeReusePricingAuthorityVersion:
		if a.RoundingPolicy != realtimeReusePricingRoundingPolicy ||
			a.ReferenceCurrency != realtimeReferenceCurrency ||
			a.SettlementCurrency != currency {
			return errors.New("realtime reuse authority has unsupported currency pair or rounding policy")
		}
		if a.FX == nil {
			return errors.New("realtime reuse authority lacks frozen FX authority")
		}
		if err := validateRealtimeFXAuthority(*a.FX, settlement); err != nil {
			return err
		}
		if a.FX.ReferenceCurrency != a.ReferenceCurrency ||
			a.FX.SettlementCurrency != a.SettlementCurrency {
			return errors.New("realtime reuse authority currency pair disagrees with FX authority")
		}
	default:
		return errors.New("realtime reuse authority has unsupported version")
	}
	if a.RuntimeProfileID == "" || !validSHA256(a.RuntimeProfileSHA256) ||
		!validSHA256(a.InputCommitment) || !validSHA256(a.RequestSHA256) ||
		!validSHA256(a.ResultCommitment) || a.DeliveredTokens <= 0 ||
		a.FullRateNanosPerMillion <= 0 || a.RetainedShareNanos != realtimeReuseRetainedShareNanos ||
		a.MinimumDeliveryChargeNanos <= 0 || a.BuyerDeclaredCeilingNanos < 0 ||
		a.BuyerDeclaredCeilingReferenceNanos < 0 {
		return errors.New("realtime reuse authority lacks immutable positive pricing identity")
	}
	if a.Version == realtimeReusePricingLegacyVersion {
		if a.MinimumDeliveryChargeNanos != realtimeReuseMinimumChargeNanos {
			return errors.New("legacy realtime reuse authority has an unknown minimum charge")
		}
		return nil
	}
	if a.ReferenceFullRateNanosPerMillion <= 0 || a.ReferenceBuyerChargeNanos <= 0 ||
		a.ReferenceMinimumDeliveryChargeNanos != realtimeReuseMinimumChargeNanos {
		return errors.New("realtime reuse authority lacks positive USD reference nanos")
	}
	convertedRate, err := convertRealtimeReferenceRate(
		NanoMajorPerMillionTokens(a.ReferenceFullRateNanosPerMillion), settlement, *a.FX)
	if err != nil || int64(convertedRate) != a.FullRateNanosPerMillion {
		return errors.New("realtime reuse settlement rate disagrees with frozen USD rate and FX")
	}
	convertedMinimum, err := convertRealtimeReferenceNanos(
		a.ReferenceMinimumDeliveryChargeNanos, settlement, *a.FX, true)
	if err != nil || convertedMinimum.Nanos != a.MinimumDeliveryChargeNanos {
		return errors.New("realtime reuse minimum charge disagrees with frozen FX")
	}
	if (a.BuyerDeclaredCeilingReferenceNanos == 0) != (a.BuyerDeclaredCeilingNanos == 0) {
		return errors.New("realtime reuse authority has only one side of the buyer ceiling")
	}
	if a.BuyerDeclaredCeilingReferenceNanos > 0 {
		converted, err := convertRealtimeReferenceNanos(
			a.BuyerDeclaredCeilingReferenceNanos, settlement, *a.FX, false)
		if err != nil || converted.Nanos != a.BuyerDeclaredCeilingNanos {
			return errors.New("realtime reuse buyer ceiling disagrees with frozen FX")
		}
	}
	return nil
}

func newRealtimeReusePricingDecision(in RealtimeReusePricingInputs) (PricingDecision, error) {
	if err := RequireSettlementCurrency(in.Currency.Code()); err != nil {
		return PricingDecision{}, err
	}
	governed, ok := vllmProfileByID(in.Profile.RuntimeProfileID)
	if !ok || !reflect.DeepEqual(governed, in.Profile) {
		return PricingDecision{}, errors.New("realtime reuse profile does not match embedded authority")
	}
	fx := in.FX
	var err error
	if zeroRealtimeFXAuthority(fx) {
		fx, err = loadRealtimeFXAuthority(in.Currency)
		if err != nil {
			return PricingDecision{}, err
		}
	} else if err := validateRealtimeFXAuthority(fx, in.Currency); err != nil {
		return PricingDecision{}, err
	}
	inputReferenceRate, err := nanoRatePerMillionFromFloat(in.Profile.BuyerInputUSDPerMillionTokens)
	if err != nil {
		return PricingDecision{}, err
	}
	outputReferenceRate, err := nanoRatePerMillionFromFloat(in.Profile.BuyerOutputUSDPerMillionTokens)
	if err != nil {
		return PricingDecision{}, err
	}
	fullReferenceRate := inputReferenceRate
	if outputReferenceRate > fullReferenceRate {
		fullReferenceRate = outputReferenceRate
	}
	referenceCharge, minimumApplied, err := RealtimeReuseBuyerChargeNanos(
		MustParseCurrency(realtimeReferenceCurrency), in.DeliveredTokens, fullReferenceRate)
	if err != nil {
		return PricingDecision{}, err
	}
	charge, err := convertRealtimeReferenceNanos(referenceCharge.Nanos, in.Currency, fx, true)
	if err != nil {
		return PricingDecision{}, err
	}
	fullRate, err := convertRealtimeReferenceRate(fullReferenceRate, in.Currency, fx)
	if err != nil {
		return PricingDecision{}, err
	}
	settlementMinimum, err := convertRealtimeReferenceNanos(
		realtimeReuseMinimumChargeNanos, in.Currency, fx, true)
	if err != nil {
		return PricingDecision{}, err
	}
	referenceCeiling := in.BuyerDeclaredCeilingReferenceNanos
	if referenceCeiling < 0 {
		return PricingDecision{}, errors.New("realtime reuse buyer USD ceiling cannot be negative")
	}
	if referenceCeiling == 0 {
		referenceCeiling, err = referenceCeilingNanosFromLegacyFloat(in.BuyerDeclaredCeiling)
		if err != nil {
			return PricingDecision{}, err
		}
	} else if in.BuyerDeclaredCeiling != 0 &&
		(!finiteNonNegative(in.BuyerDeclaredCeiling) ||
			in.BuyerDeclaredCeiling != float64(referenceCeiling)/float64(NanosPerMajorUnit)) {
		return PricingDecision{}, errors.New("realtime reuse buyer USD ceiling float projection and exact nanos disagree")
	}
	a := RealtimeReusePricingAuthority{Version: realtimeReusePricingAuthorityVersion, Currency: in.Currency.Code(),
		ReferenceCurrency: realtimeReferenceCurrency, SettlementCurrency: in.Currency.Code(), FX: &fx,
		RuntimeProfileID: in.Profile.RuntimeProfileID, RuntimeProfileSHA256: in.Profile.ProfileSHA256,
		InputCommitment: in.InputCommitment, RequestSHA256: in.RequestSHA256, ResultCommitment: in.ResultCommitment,
		ReuseClass: in.ReuseClass, DeliveredTokens: in.DeliveredTokens,
		ReferenceFullRateNanosPerMillion: int64(fullReferenceRate), FullRateNanosPerMillion: int64(fullRate),
		ReferenceBuyerChargeNanos:           referenceCharge.Nanos,
		ReferenceMinimumDeliveryChargeNanos: realtimeReuseMinimumChargeNanos,
		RetainedShareNanos:                  realtimeReuseRetainedShareNanos, MinimumDeliveryChargeNanos: settlementMinimum.Nanos,
		MinimumChargeApplied: minimumApplied, RoundingPolicy: realtimeReusePricingRoundingPolicy}
	if referenceCeiling > 0 {
		ceiling, e := convertRealtimeReferenceNanos(referenceCeiling, in.Currency, fx, false)
		if e != nil {
			return PricingDecision{}, e
		}
		a.BuyerDeclaredCeilingReferenceNanos = referenceCeiling
		a.BuyerDeclaredCeilingNanos = ceiling.Nanos
	}
	if err := validateRealtimeReusePricingAuthority(a, in.Currency.Code()); err != nil {
		return PricingDecision{}, err
	}
	if a.BuyerDeclaredCeilingNanos > 0 &&
		(referenceCharge.Nanos > a.BuyerDeclaredCeilingReferenceNanos ||
			charge.Nanos > a.BuyerDeclaredCeilingNanos) {
		return PricingDecision{}, errors.New("realtime reuse charge exceeds buyer ceiling")
	}
	unknowns := []string{"processor fee", "control-plane cost", "storage cost", "egress cost", "risk reserve"}
	p := PricingDecision{Version: pricingDecisionVersion, PolicyRevision: pricingDecisionPolicyRevision,
		ExecutionMode: pricingExecutionRealtimeReuse, Currency: in.Currency.Code(), Tier: in.ReuseClass,
		RealtimeReuse: &a, BillableUnits: float64(in.DeliveredTokens), BuyerPrice: charge.USDFloat(), MaximumBuyerPrice: charge.USDFloat(),
		PrimarySupplierCost: notApplicableCost("no physical supplier executes a reuse delivery"),
		VerificationCost:    notApplicableCost("the delivered object reuses an already verified result"),
		PaymentCost:         unknownCost("processor allocation is reconciled after collection"),
		ControlPlaneCost:    unknownCost("reuse lookup and materialization are not calibrated"),
		StorageCost:         unknownCost("cached-object storage is unattributed"), EgressCost: unknownCost("result egress is unattributed"),
		ProviderCost:         notApplicableCost("no external compute provider executes the reused delivery"),
		RiskReserve:          unknownCost("reuse failure/refund risk is not calibrated"),
		PlatformContribution: PricingCostComponent{Status: pricingCostModeled, Amount: charge.USDFloat(), Basis: "USD reference reuse receipt converted through frozen FX before unknown costs; not true net"},
		FixedPoint: &FixedPointPricingDecision{Currency: in.Currency.Code(), BuyerChargeNanos: charge.Nanos,
			AcceptedCeilingNanos: charge.Nanos, SupplierEntitlementsNanos: 0, MercGrossSpreadNanos: charge.Nanos,
			KnownCostContributionNanos: charge.Nanos, UnknownCostCategories: unknowns},
		Confidence: 0.8, Assumptions: []string{"physical supplier work is zero", "USD reference rate and governed FX are frozen", "minimum charge is an external-ledger delivery floor"}, Unknowns: unknowns}
	return p, validatePricingCostShape(p)
}

func ValidateRealtimeReusePricingDecisionSnapshot(p PricingDecision, in RealtimeReusePricingInputs) error {
	rebuilt, err := newRealtimeReusePricingDecision(in)
	if err != nil {
		return err
	}
	if !reflect.DeepEqual(p, rebuilt) {
		return errors.New("realtime reuse PricingDecision does not match deterministic authority")
	}
	return nil
}

func realtimeReuseMoneyFromAuthority(a RealtimeReusePricingAuthority) (MoneyNanos, error) {
	settlement, err := ParseCurrency(a.Currency)
	if err != nil {
		return MoneyNanos{}, err
	}
	if err := validateRealtimeReusePricingAuthority(a, a.Currency); err != nil {
		return MoneyNanos{}, err
	}
	switch a.Version {
	case realtimeReusePricingLegacyVersion:
		charge, _, err := RealtimeReuseBuyerChargeNanos(
			settlement, a.DeliveredTokens, NanoMajorPerMillionTokens(a.FullRateNanosPerMillion))
		return charge, err
	case realtimeReusePricingAuthorityVersion:
		referenceCharge, minimumApplied, err := RealtimeReuseBuyerChargeNanos(
			MustParseCurrency(realtimeReferenceCurrency), a.DeliveredTokens,
			NanoMajorPerMillionTokens(a.ReferenceFullRateNanosPerMillion))
		if err != nil {
			return MoneyNanos{}, err
		}
		if referenceCharge.Nanos != a.ReferenceBuyerChargeNanos ||
			minimumApplied != a.MinimumChargeApplied {
			return MoneyNanos{}, errors.New("realtime reuse USD charge disagrees with frozen source rate")
		}
		return convertRealtimeReferenceNanos(referenceCharge.Nanos, settlement, *a.FX, true)
	default:
		return MoneyNanos{}, errors.New("unsupported realtime reuse pricing authority version")
	}
}

func validateFrozenRealtimeReusePricingDecision(p PricingDecision) error {
	if p.ExecutionMode != pricingExecutionRealtimeReuse || p.RealtimeReuse == nil || p.FixedPoint == nil {
		return errors.New("frozen realtime reuse PricingDecision lacks reuse authority")
	}
	if err := validatePricingCostShape(p); err != nil {
		return err
	}
	charge, err := realtimeReuseMoneyFromAuthority(*p.RealtimeReuse)
	if err != nil {
		return err
	}
	a := p.RealtimeReuse
	if p.FixedPoint.BuyerChargeNanos != charge.Nanos ||
		p.FixedPoint.AcceptedCeilingNanos != charge.Nanos ||
		p.FixedPoint.SupplierEntitlementsNanos != 0 ||
		p.FixedPoint.KnownCostContributionNanos != charge.Nanos ||
		p.BuyerPrice != charge.USDFloat() || p.MaximumBuyerPrice != charge.USDFloat() ||
		p.PlatformContribution.Amount != charge.USDFloat() {
		return errors.New("frozen realtime reuse money disagrees with its exact authority")
	}
	if a.Version == realtimeReusePricingAuthorityVersion &&
		a.BuyerDeclaredCeilingReferenceNanos > 0 &&
		(a.ReferenceBuyerChargeNanos > a.BuyerDeclaredCeilingReferenceNanos ||
			charge.Nanos > a.BuyerDeclaredCeilingNanos) {
		return errors.New("frozen realtime reuse charge exceeds its buyer USD ceiling")
	}
	return nil
}

func realtimeReuseReferenceProjection(p PricingDecision) (float64, error) {
	if p.ExecutionMode != pricingExecutionRealtimeReuse || p.RealtimeReuse == nil || p.FixedPoint == nil {
		return 0, errors.New("USD projection requires realtime reuse pricing")
	}
	var referenceNanos int64
	switch p.RealtimeReuse.Version {
	case realtimeReusePricingAuthorityVersion:
		referenceNanos = p.RealtimeReuse.ReferenceBuyerChargeNanos
	case realtimeReusePricingLegacyVersion:
		if p.Currency != realtimeReferenceCurrency {
			return 0, errors.New("legacy non-USD realtime reuse has no truthful USD projection")
		}
		referenceNanos = p.FixedPoint.BuyerChargeNanos
	default:
		return 0, errors.New("unsupported realtime reuse pricing authority version")
	}
	reference, err := NewMoneyNanos(MustParseCurrency(realtimeReferenceCurrency), referenceNanos)
	if err != nil {
		return 0, err
	}
	return projectRealtimeNanosToMajor(reference)
}
