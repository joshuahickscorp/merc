package main

import (
	"errors"
	"reflect"
)

const (
	pricingExecutionRealtimeReuse        = "realtime_exact_reuse"
	realtimeReusePricingAuthorityVersion = 1
	realtimeReusePricingRoundingPolicy   = "realtime-reuse-nanos-v1"
)

type RealtimeReusePricingAuthority struct {
	Version                    int    `json:"version"`
	Currency                   string `json:"currency"`
	RuntimeProfileID           string `json:"runtime_profile_id"`
	RuntimeProfileSHA256       string `json:"runtime_profile_sha256"`
	InputCommitment            string `json:"input_commitment"`
	RequestSHA256              string `json:"request_sha256"`
	ResultCommitment           string `json:"result_commitment"`
	ReuseClass                 string `json:"reuse_class"`
	DeliveredTokens            int64  `json:"delivered_tokens"`
	FullRateNanosPerMillion    int64  `json:"full_rate_nanos_per_million"`
	RetainedShareNanos         int64  `json:"retained_share_nanos"`
	MinimumDeliveryChargeNanos int64  `json:"minimum_delivery_charge_nanos"`
	MinimumChargeApplied       bool   `json:"minimum_charge_applied"`
	BuyerDeclaredCeilingNanos  int64  `json:"buyer_declared_ceiling_nanos,omitempty"`
	RoundingPolicy             string `json:"rounding_policy"`
}

type RealtimeReusePricingInputs struct {
	Profile                                                      VLLMRuntimeProfile
	InputCommitment, RequestSHA256, ResultCommitment, ReuseClass string
	DeliveredTokens                                              int64
	BuyerDeclaredCeiling                                         float64
	Currency                                                     Currency
}

func validateRealtimeReusePricingAuthority(a RealtimeReusePricingAuthority, currency string) error {
	if a.Version != realtimeReusePricingAuthorityVersion || a.Currency != currency ||
		a.RoundingPolicy != realtimeReusePricingRoundingPolicy ||
		(a.ReuseClass != ClassExactResultReuse && a.ReuseClass != ClassCoalescedDelivery) {
		return errors.New("realtime reuse authority has unsupported version, currency, class, or rounding policy")
	}
	if a.RuntimeProfileID == "" || !validSHA256(a.RuntimeProfileSHA256) ||
		!validSHA256(a.InputCommitment) || !validSHA256(a.RequestSHA256) ||
		!validSHA256(a.ResultCommitment) || a.DeliveredTokens <= 0 ||
		a.FullRateNanosPerMillion <= 0 || a.RetainedShareNanos != realtimeReuseRetainedShareNanos ||
		a.MinimumDeliveryChargeNanos != realtimeReuseMinimumChargeNanos || a.BuyerDeclaredCeilingNanos < 0 {
		return errors.New("realtime reuse authority lacks immutable positive pricing identity")
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
	inputRate, err := nanoRatePerMillionFromFloat(in.Profile.BuyerInputUSDPerMillionTokens)
	if err != nil {
		return PricingDecision{}, err
	}
	outputRate, err := nanoRatePerMillionFromFloat(in.Profile.BuyerOutputUSDPerMillionTokens)
	if err != nil {
		return PricingDecision{}, err
	}
	fullRate := inputRate
	if outputRate > fullRate {
		fullRate = outputRate
	}
	charge, minimumApplied, err := RealtimeReuseBuyerChargeNanos(in.Currency, in.DeliveredTokens, fullRate)
	if err != nil {
		return PricingDecision{}, err
	}
	a := RealtimeReusePricingAuthority{Version: realtimeReusePricingAuthorityVersion, Currency: in.Currency.Code(),
		RuntimeProfileID: in.Profile.RuntimeProfileID, RuntimeProfileSHA256: in.Profile.ProfileSHA256,
		InputCommitment: in.InputCommitment, RequestSHA256: in.RequestSHA256, ResultCommitment: in.ResultCommitment,
		ReuseClass: in.ReuseClass, DeliveredTokens: in.DeliveredTokens, FullRateNanosPerMillion: int64(fullRate),
		RetainedShareNanos: realtimeReuseRetainedShareNanos, MinimumDeliveryChargeNanos: realtimeReuseMinimumChargeNanos,
		MinimumChargeApplied: minimumApplied, RoundingPolicy: realtimeReusePricingRoundingPolicy}
	if in.BuyerDeclaredCeiling > 0 {
		ceiling, e := MoneyNanosFromUSDFloat(in.Currency, in.BuyerDeclaredCeiling)
		if e != nil {
			return PricingDecision{}, e
		}
		a.BuyerDeclaredCeilingNanos = ceiling.Nanos
	}
	if err := validateRealtimeReusePricingAuthority(a, in.Currency.Code()); err != nil {
		return PricingDecision{}, err
	}
	if a.BuyerDeclaredCeilingNanos > 0 && charge.Nanos > a.BuyerDeclaredCeilingNanos {
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
		PlatformContribution: PricingCostComponent{Status: pricingCostModeled, Amount: charge.USDFloat(), Basis: "gross reuse receipt before unknown costs; not true net"},
		FixedPoint: &FixedPointPricingDecision{Currency: in.Currency.Code(), BuyerChargeNanos: charge.Nanos,
			AcceptedCeilingNanos: charge.Nanos, SupplierEntitlementsNanos: 0, MercGrossSpreadNanos: charge.Nanos,
			KnownCostContributionNanos: charge.Nanos, UnknownCostCategories: unknowns},
		Confidence: 0.8, Assumptions: []string{"physical supplier work is zero", "minimum charge is an external-ledger delivery floor"}, Unknowns: unknowns}
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
