package main

import (
	"errors"
	"fmt"
	"reflect"
)

const (
	pricingExecutionRealtime        = "realtime_physical"
	realtimePricingAuthorityVersion = 1
	realtimePricingRoundingPolicy   = "realtime-token-nanos-v1"
)

// RealtimePricingAuthority is the immutable input set for one physical
// realtime PricingDecision. Buyer rates come from the embedded runtime profile;
// supplier rates come from the selected offer. Counts are token bounds, not
// observed settlement usage.
type RealtimePricingAuthority struct {
	Version                       int    `json:"version"`
	Currency                      string `json:"currency"`
	RuntimeProfileID              string `json:"runtime_profile_id"`
	RuntimeProfileSHA256          string `json:"runtime_profile_sha256"`
	PlacementPlanSHA256           string `json:"placement_plan_sha256"`
	InputCommitment               string `json:"input_commitment"`
	RequestSHA256                 string `json:"request_sha256"`
	MaximumPromptTokens           int64  `json:"maximum_prompt_tokens"`
	MaximumCompletionTokens       int64  `json:"maximum_completion_tokens"`
	EstimatedPromptTokens         int64  `json:"estimated_prompt_tokens"`
	EstimatedCompletionTokens     int64  `json:"estimated_completion_tokens"`
	BuyerInputNanosPerMillion     int64  `json:"buyer_input_nanos_per_million"`
	BuyerOutputNanosPerMillion    int64  `json:"buyer_output_nanos_per_million"`
	SupplierInputNanosPerMillion  int64  `json:"supplier_input_nanos_per_million"`
	SupplierOutputNanosPerMillion int64  `json:"supplier_output_nanos_per_million"`
	BuyerDeclaredCeilingNanos     int64  `json:"buyer_declared_ceiling_nanos,omitempty"`
	RoundingPolicy                string `json:"rounding_policy"`
}

type RealtimePricingInputs struct {
	Profile                   VLLMRuntimeProfile
	Placement                 RealtimePlacementPlan
	InputCommitment           string
	RequestSHA256             string
	MaximumPromptTokens       int64
	MaximumCompletionTokens   int64
	EstimatedPromptTokens     int64
	EstimatedCompletionTokens int64
	SupplierInputRate         float64
	SupplierOutputRate        float64
	BuyerDeclaredCeiling      float64
	Currency                  Currency
}

func validateRealtimePricingAuthority(a RealtimePricingAuthority, currency string) error {
	if a.Version != realtimePricingAuthorityVersion || a.Currency != currency ||
		a.RoundingPolicy != realtimePricingRoundingPolicy {
		return errors.New("realtime pricing authority has unsupported version, currency, or rounding policy")
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
	if a.BuyerDeclaredCeilingNanos < 0 {
		return errors.New("realtime pricing authority has a negative buyer ceiling")
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
	buyerInput, err := nanoRatePerMillionFromFloat(in.Profile.BuyerInputUSDPerMillionTokens)
	if err != nil {
		return PricingDecision{}, err
	}
	buyerOutput, err := nanoRatePerMillionFromFloat(in.Profile.BuyerOutputUSDPerMillionTokens)
	if err != nil {
		return PricingDecision{}, err
	}
	supplierInput, err := nanoRatePerMillionFromFloat(in.SupplierInputRate)
	if err != nil {
		return PricingDecision{}, err
	}
	supplierOutput, err := nanoRatePerMillionFromFloat(in.SupplierOutputRate)
	if err != nil {
		return PricingDecision{}, err
	}
	if err := validateRealtimeOfferRates(in.Profile, RealtimeOfferRegistration{
		SupplierInputUSDPerMillionTokens:  in.SupplierInputRate,
		SupplierOutputUSDPerMillionTokens: in.SupplierOutputRate,
	}); err != nil {
		return PricingDecision{}, err
	}
	authority := RealtimePricingAuthority{
		Version: realtimePricingAuthorityVersion, Currency: in.Currency.Code(),
		RuntimeProfileID: in.Profile.RuntimeProfileID, RuntimeProfileSHA256: in.Profile.ProfileSHA256,
		PlacementPlanSHA256: placementSHA, InputCommitment: in.InputCommitment,
		RequestSHA256: in.RequestSHA256, MaximumPromptTokens: in.MaximumPromptTokens,
		MaximumCompletionTokens:   in.MaximumCompletionTokens,
		EstimatedPromptTokens:     in.EstimatedPromptTokens,
		EstimatedCompletionTokens: in.EstimatedCompletionTokens,
		BuyerInputNanosPerMillion: int64(buyerInput), BuyerOutputNanosPerMillion: int64(buyerOutput),
		SupplierInputNanosPerMillion: int64(supplierInput), SupplierOutputNanosPerMillion: int64(supplierOutput),
		RoundingPolicy: realtimePricingRoundingPolicy,
	}
	if in.BuyerDeclaredCeiling > 0 {
		ceiling, err := MoneyNanosFromUSDFloat(in.Currency, in.BuyerDeclaredCeiling)
		if err != nil {
			return PricingDecision{}, err
		}
		authority.BuyerDeclaredCeilingNanos = ceiling.Nanos
	}
	if err := validateRealtimePricingAuthority(authority, in.Currency.Code()); err != nil {
		return PricingDecision{}, err
	}
	buyerExpected, err := BuyerRealtimeTokenChargeNanos(in.Currency,
		in.EstimatedPromptTokens, in.EstimatedCompletionTokens, buyerInput, buyerOutput)
	if err != nil {
		return PricingDecision{}, err
	}
	buyerMaximum, err := BuyerRealtimeTokenChargeNanos(in.Currency,
		in.MaximumPromptTokens, in.MaximumCompletionTokens, buyerInput, buyerOutput)
	if err != nil {
		return PricingDecision{}, err
	}
	supplierExpected, err := SupplierRealtimeTokenEntitlementNanos(in.Currency,
		in.EstimatedPromptTokens, in.EstimatedCompletionTokens, supplierInput, supplierOutput)
	if err != nil {
		return PricingDecision{}, err
	}
	contribution, err := buyerExpected.Sub(supplierExpected)
	if err != nil || contribution.Nanos <= 0 || buyerExpected.Nanos <= 0 ||
		buyerMaximum.Nanos < buyerExpected.Nanos || supplierExpected.Nanos <= 0 {
		return PricingDecision{}, errors.New("realtime pricing lacks positive conserved expected/max authority")
	}
	if authority.BuyerDeclaredCeilingNanos > 0 && buyerMaximum.Nanos > authority.BuyerDeclaredCeilingNanos {
		return PricingDecision{}, fmt.Errorf("maximum realtime price %d nanos exceeds buyer ceiling %d nanos",
			buyerMaximum.Nanos, authority.BuyerDeclaredCeilingNanos)
	}
	unknowns := []string{"processor fee", "control-plane cost", "storage cost", "egress cost", "risk reserve"}
	decision := PricingDecision{
		Version: pricingDecisionVersion, PolicyRevision: pricingDecisionPolicyRevision,
		ExecutionMode: pricingExecutionRealtime, Currency: in.Currency.Code(), Tier: "realtime",
		Realtime: &authority, BillableUnits: float64(in.EstimatedPromptTokens + in.EstimatedCompletionTokens),
		BuyerPrice: buyerExpected.USDFloat(), MaximumBuyerPrice: buyerMaximum.USDFloat(),
		PrimarySupplierCost:  PricingCostComponent{Status: pricingCostModeled, Amount: supplierExpected.USDFloat(), Basis: "selected supplier offer at estimated token bounds"},
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
		Assumptions: []string{"token maxima are conservative byte/output bounds", "selected supplier ask remains fixed for settlement"},
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
