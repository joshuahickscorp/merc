package main

import (
	"errors"
	"fmt"
	"reflect"
)

const (
	pricingExecutionServiceLease              = "service_lease"
	serviceLeasePricingAuthorityLegacyVersion = 1
	serviceLeasePricingAuthorityVersion       = 2
	// currentServiceLeasePricingCurrency is deliberately narrower than the
	// process-wide supported-currency registry. Service pricing v2 freezes exact
	// rates but no USD-reference/FX authority, so admitting it outside USD would
	// relabel supplier, residency, and policy nanos rather than convert them.
	currentServiceLeasePricingCurrency       = "usd"
	serviceLeasePricingRoundingPolicy        = "service-lease-replica-nanos-v1"
	serviceLeaseMaximumTermSeconds     int64 = 7 * 24 * 60 * 60
)

const (
	serviceLeaseBlockerProcessor = "PROCESSOR_FEE_ALLOCATION_FROM_PREPAID_FUNDING_RECEIPT_UNKNOWN"
	serviceLeaseBlockerEgress    = "EGRESS_PROVIDER_BILLING_AUTHORITY_UNKNOWN"
	serviceLeaseBlockerResidency = "RESIDENCY_LIABILITY_BENEFICIARY_AND_PAYOUT_FINALITY"
	serviceLeaseBlockerReserve   = "LEASE_RESERVE_AND_SLO_REFUND_FINALITY_UNDEFINED"
)

func serviceLeaseEconomicFinalityBlockers() []string {
	return []string{
		serviceLeaseBlockerProcessor,
		serviceLeaseBlockerEgress,
		serviceLeaseBlockerResidency,
		serviceLeaseBlockerReserve,
	}
}

// validateCurrentServiceLeaseCurrency is for new offer/order/pricing ingress.
// Historical v1/v2 decoding intentionally does not call it: accepted authority
// may remain readable in another supported currency, but no new non-USD lease
// can be admitted until a later version freezes an FX authority.
func validateCurrentServiceLeaseCurrency(raw string) (Currency, error) {
	currency, err := ParseCurrency(raw)
	if err != nil || currency.Code() != raw {
		return Currency{}, errors.New("service lease currency must be a supported canonical ISO code")
	}
	if currency.Code() != currentServiceLeasePricingCurrency {
		return Currency{}, fmt.Errorf(
			"service lease pricing supports new %s authority only; %s requires a frozen FX-bearing pricing version",
			currentServiceLeasePricingCurrency, currency.Code(),
		)
	}
	if err := RequireSettlementCurrency(currency.Code()); err != nil {
		return Currency{}, fmt.Errorf("service lease currency is not this deployment's settlement currency: %w", err)
	}
	return currency, nil
}

// ServiceLeasePricingAuthority freezes the terms behind a reserved, warm
// service. All monetary fields are settlement-currency nano-major-units per
// replica-hour. Version 2 treats the supplier and residency rates as one
// all-in entitlement to the selected supplier: the offer provides both rates
// and no separate residency beneficiary exists. The separate residency field
// remains frozen so selection and the all-in payout can be audited.
//
// Version 1 is historical-read-only. It paid only SupplierNanosPerReplicaHour
// and treated residency and a modeled risk charge as platform variable costs.
// Version 2 does not repeat that unsupported liability allocation: its risk
// rate must be zero and its receipt keeps reserve/refund finality UNKNOWN.
//
// The authority is used for both the maximum reservation and each meter. A
// meter prices aggregate replica-time from lease start, then records only the
// delta from the prior aggregate, so heartbeat cadence cannot change economics.
type ServiceLeasePricingAuthority struct {
	Version                         int    `json:"version"`
	Currency                        string `json:"currency"`
	RuntimeProfileID                string `json:"runtime_profile_id"`
	RuntimeProfileSHA256            string `json:"runtime_profile_sha256"`
	Region                          string `json:"region"`
	MinimumReplicas                 int    `json:"minimum_replicas"`
	MaximumReplicas                 int    `json:"maximum_replicas"`
	TermSeconds                     int64  `json:"term_seconds"`
	MaximumP95LatencyMilliseconds   int64  `json:"maximum_p95_latency_milliseconds"`
	SupplierNanosPerReplicaHour     int64  `json:"supplier_nanos_per_replica_hour"`
	ResidencyNanosPerReplicaHour    int64  `json:"residency_nanos_per_replica_hour"`
	ControlPlaneNanosPerReplicaHour int64  `json:"control_plane_nanos_per_replica_hour"`
	RiskReserveNanosPerReplicaHour  int64  `json:"risk_reserve_nanos_per_replica_hour"`
	ContributionNanosPerReplicaHour int64  `json:"contribution_nanos_per_replica_hour"`
	RoundingPolicy                  string `json:"rounding_policy"`
}

// ServiceLeasePricingInputs are server-resolved terms. A buyer gives a ceiling
// and SLO, never an offered price. A supplier gives its floor and residency
// allocation. Every remaining rate is bound by the server policy revision.
type ServiceLeasePricingInputs struct {
	Profile                          VLLMRuntimeProfile
	Currency                         Currency
	Region                           string
	MinimumReplicas, MaximumReplicas int
	TermSeconds                      int64
	MaximumP95LatencyMilliseconds    int64
	SupplierNanosPerReplicaHour      int64
	ResidencyNanosPerReplicaHour     int64
	ControlPlaneNanosPerReplicaHour  int64
	RiskReserveNanosPerReplicaHour   int64
	ContributionNanosPerReplicaHour  int64
	BuyerDeclaredCeilingNanos        int64
}

// ServiceLeaseMoney is the exact economic result for one cumulative
// replica-time value. Components are rounded only after the whole aggregate
// duration is multiplied. Buyer charge is their sum, making conservation an
// identity rather than a tolerance between independently rounded paths.
type ServiceLeaseMoney struct {
	BuyerCharge      MoneyNanos
	SupplierPayable  MoneyNanos
	ResidencyCost    MoneyNanos
	ControlPlaneCost MoneyNanos
	RiskReserve      MoneyNanos
	MercContribution MoneyNanos
}

func validateServiceLeasePricingAuthority(a ServiceLeasePricingAuthority, currency string) error {
	if (a.Version != serviceLeasePricingAuthorityLegacyVersion && a.Version != serviceLeasePricingAuthorityVersion) || a.Currency != currency ||
		a.RoundingPolicy != serviceLeasePricingRoundingPolicy {
		return errors.New("service lease pricing authority has unsupported version, currency, or rounding policy")
	}
	if a.RuntimeProfileID == "" || !validSHA256(a.RuntimeProfileSHA256) || a.Region == "" ||
		a.MinimumReplicas < 1 || a.MaximumReplicas < a.MinimumReplicas ||
		a.TermSeconds < 60 || a.TermSeconds > serviceLeaseMaximumTermSeconds ||
		a.MaximumP95LatencyMilliseconds < 1 {
		return errors.New("service lease pricing authority has invalid workload or capacity bounds")
	}
	for name, rate := range map[string]int64{
		"supplier floor":           a.SupplierNanosPerReplicaHour,
		"residency allocation":     a.ResidencyNanosPerReplicaHour,
		"control-plane allocation": a.ControlPlaneNanosPerReplicaHour,
		"Merc contribution":        a.ContributionNanosPerReplicaHour,
	} {
		if rate <= 0 {
			return fmt.Errorf("service lease %s must be a positive exact hourly rate", name)
		}
	}
	if a.Version == serviceLeasePricingAuthorityLegacyVersion {
		if a.RiskReserveNanosPerReplicaHour <= 0 {
			return errors.New("historical service lease risk reserve must retain its positive accepted rate")
		}
	} else if a.RiskReserveNanosPerReplicaHour != 0 {
		return errors.New("current service lease pricing may not charge an unimplemented risk reserve")
	}
	return nil
}

func serviceLeaseComponent(c Currency, rate int64, replicaDurationNanos int64) (MoneyNanos, error) {
	if rate <= 0 || replicaDurationNanos <= 0 {
		return MoneyNanos{}, errors.New("service lease rate and aggregate replica duration must be positive")
	}
	nanos, err := mulDiv(rate, replicaDurationNanos, nanosecondsPerHour, true)
	if err != nil {
		return MoneyNanos{}, err
	}
	return NewMoneyNanos(c, nanos)
}

func ServiceLeaseMoneyForReplicaDuration(c Currency, authority ServiceLeasePricingAuthority, replicaDurationNanos int64) (ServiceLeaseMoney, error) {
	if err := validateServiceLeasePricingAuthority(authority, c.Code()); err != nil {
		return ServiceLeaseMoney{}, err
	}
	supplier, err := serviceLeaseComponent(c, authority.SupplierNanosPerReplicaHour, replicaDurationNanos)
	if err != nil {
		return ServiceLeaseMoney{}, err
	}
	residency, err := serviceLeaseComponent(c, authority.ResidencyNanosPerReplicaHour, replicaDurationNanos)
	if err != nil {
		return ServiceLeaseMoney{}, err
	}
	control, err := serviceLeaseComponent(c, authority.ControlPlaneNanosPerReplicaHour, replicaDurationNanos)
	if err != nil {
		return ServiceLeaseMoney{}, err
	}
	risk, err := NewMoneyNanos(c, 0)
	if err != nil {
		return ServiceLeaseMoney{}, err
	}
	if authority.Version == serviceLeasePricingAuthorityLegacyVersion {
		risk, err = serviceLeaseComponent(c, authority.RiskReserveNanosPerReplicaHour, replicaDurationNanos)
		if err != nil {
			return ServiceLeaseMoney{}, err
		}
	}
	contribution, err := serviceLeaseComponent(c, authority.ContributionNanosPerReplicaHour, replicaDurationNanos)
	if err != nil {
		return ServiceLeaseMoney{}, err
	}
	supplierPayable := supplier
	if authority.Version == serviceLeasePricingAuthorityVersion {
		// The selected supplier advertised both values. With no separate
		// beneficiary, residency is part of that supplier's all-in warm-capacity
		// entitlement and must never fall into platform_take.
		supplierPayable, err = supplier.Add(residency)
	}
	buyer := supplierPayable
	if authority.Version == serviceLeasePricingAuthorityLegacyVersion && err == nil {
		buyer, err = buyer.Add(residency)
	}
	if err == nil {
		buyer, err = buyer.Add(control)
	}
	if err == nil && authority.Version == serviceLeasePricingAuthorityLegacyVersion {
		buyer, err = buyer.Add(risk)
	}
	if err == nil {
		buyer, err = buyer.Add(contribution)
	}
	if err != nil || buyer.Nanos <= supplierPayable.Nanos {
		return ServiceLeaseMoney{}, errors.New("service lease buyer charge lacks positive conserved Merc contribution")
	}
	return ServiceLeaseMoney{BuyerCharge: buyer, SupplierPayable: supplierPayable, ResidencyCost: residency,
		ControlPlaneCost: control, RiskReserve: risk, MercContribution: contribution}, nil
}

func serviceLeaseMaximumReplicaDuration(a ServiceLeasePricingAuthority) (int64, error) {
	return mulDiv(int64(a.MaximumReplicas), a.TermSeconds, 1, false)
}

func newServiceLeasePricingDecision(in ServiceLeasePricingInputs) (PricingDecision, error) {
	if _, err := validateCurrentServiceLeaseCurrency(in.Currency.Code()); err != nil {
		return PricingDecision{}, err
	}
	governed, ok := vllmProfileByID(in.Profile.RuntimeProfileID)
	if !ok || !reflect.DeepEqual(governed, in.Profile) {
		return PricingDecision{}, errors.New("service lease profile does not match embedded authority")
	}
	authority := ServiceLeasePricingAuthority{
		Version: serviceLeasePricingAuthorityVersion, Currency: in.Currency.Code(),
		RuntimeProfileID: in.Profile.RuntimeProfileID, RuntimeProfileSHA256: in.Profile.ProfileSHA256,
		Region: in.Region, MinimumReplicas: in.MinimumReplicas, MaximumReplicas: in.MaximumReplicas,
		TermSeconds: in.TermSeconds, MaximumP95LatencyMilliseconds: in.MaximumP95LatencyMilliseconds,
		SupplierNanosPerReplicaHour:     in.SupplierNanosPerReplicaHour,
		ResidencyNanosPerReplicaHour:    in.ResidencyNanosPerReplicaHour,
		ControlPlaneNanosPerReplicaHour: in.ControlPlaneNanosPerReplicaHour,
		RiskReserveNanosPerReplicaHour:  in.RiskReserveNanosPerReplicaHour,
		ContributionNanosPerReplicaHour: in.ContributionNanosPerReplicaHour,
		RoundingPolicy:                  serviceLeasePricingRoundingPolicy,
	}
	if err := validateServiceLeasePricingAuthority(authority, in.Currency.Code()); err != nil {
		return PricingDecision{}, err
	}
	replicaSeconds, err := serviceLeaseMaximumReplicaDuration(authority)
	if err != nil {
		return PricingDecision{}, err
	}
	if replicaSeconds > (int64(^uint64(0)>>1) / 1_000_000_000) {
		return PricingDecision{}, errors.New("service lease maximum replica duration overflows nanoseconds")
	}
	maximum, err := ServiceLeaseMoneyForReplicaDuration(in.Currency, authority, replicaSeconds*1_000_000_000)
	if err != nil {
		return PricingDecision{}, err
	}
	if in.BuyerDeclaredCeilingNanos <= 0 || maximum.BuyerCharge.Nanos > in.BuyerDeclaredCeilingNanos {
		return PricingDecision{}, errors.New("service lease maximum charge exceeds buyer ceiling")
	}
	// In v2 residency belongs to the supplier entitlement and the unimplemented
	// risk reserve is not charged. The only known platform variable cost is the
	// control-plane allocation. Unknown categories remain explicit blockers; a
	// request/response byte diagnostic is not provider billing authority.
	variable := maximum.ControlPlaneCost.Nanos
	unknowns := serviceLeaseEconomicFinalityBlockers()
	decision := PricingDecision{
		Version: pricingDecisionVersion, PolicyRevision: pricingDecisionPolicyRevision,
		ExecutionMode: pricingExecutionServiceLease, Currency: in.Currency.Code(), Tier: "reserved_service",
		ServiceLease:  &authority,
		BillableUnits: float64(in.MaximumReplicas) * float64(in.TermSeconds) / 3600,
		BuyerPrice:    maximum.BuyerCharge.USDFloat(), MaximumBuyerPrice: maximum.BuyerCharge.USDFloat(),
		PrimarySupplierCost:  PricingCostComponent{Status: pricingCostModeled, Amount: maximum.SupplierPayable.USDFloat(), Basis: "selected supplier's exact all-in warm-capacity ask (supplier plus residency)"},
		VerificationCost:     PricingCostComponent{Status: pricingCostNotApplicable, Basis: "service availability is metered by lease health receipts, not redundant output verification"},
		PaymentCost:          PricingCostComponent{Status: pricingCostUnknown, Basis: "processor fee is allocated only from the linked prepaid funding receipt"},
		ControlPlaneCost:     PricingCostComponent{Status: pricingCostModeled, Amount: maximum.ControlPlaneCost.USDFloat(), Basis: "versioned continuous service control allocation"},
		StorageCost:          PricingCostComponent{Status: pricingCostNotApplicable, Basis: "the service lease does not price artifact storage"},
		EgressCost:           PricingCostComponent{Status: pricingCostUnknown, Basis: "application bytes are diagnostic only; governed transfer and provider billing authority are absent"},
		ProviderCost:         PricingCostComponent{Status: pricingCostNotApplicable, Basis: "residency has no separate beneficiary and is included in the selected supplier's all-in entitlement"},
		RiskReserve:          PricingCostComponent{Status: pricingCostUnknown, Basis: "lease-subject reserve lifecycle and a governed SLO refund policy are not implemented"},
		PlatformContribution: PricingCostComponent{Status: pricingCostModeled, Amount: maximum.MercContribution.USDFloat(), Basis: "known contribution before the separately attributed processor fee"},
		FixedPoint: &FixedPointPricingDecision{Currency: in.Currency.Code(), BuyerChargeNanos: maximum.BuyerCharge.Nanos,
			AcceptedCeilingNanos: in.BuyerDeclaredCeilingNanos, SupplierEntitlementsNanos: maximum.SupplierPayable.Nanos,
			KnownVariableCostsNanos: variable, MercGrossSpreadNanos: maximum.BuyerCharge.Nanos - maximum.SupplierPayable.Nanos,
			KnownCostContributionNanos: maximum.MercContribution.Nanos, UnknownCostCategories: unknowns},
		Confidence:  0.4,
		Assumptions: []string{"reserved maximum capacity is prepaid before activation", "supplier region is declared operational scope, not a legal residency certification", "the selected supplier is the beneficiary of its complete supplier-plus-residency offer"},
		Unknowns:    unknowns,
	}
	if err := validatePricingCostShape(decision); err != nil {
		return PricingDecision{}, err
	}
	return decision, nil
}

func ValidateServiceLeasePricingDecisionSnapshot(decision PricingDecision, in ServiceLeasePricingInputs) error {
	rebuilt, err := newServiceLeasePricingDecision(in)
	if err != nil {
		return err
	}
	if !reflect.DeepEqual(decision, rebuilt) {
		return errors.New("service lease PricingDecision does not match deterministic authority")
	}
	return nil
}
