package main

import (
	"errors"
	"fmt"
	"reflect"
)

const (
	pricingExecutionServiceLease              = "service_lease"
	serviceLeasePricingAuthorityVersion       = 1
	serviceLeasePricingRoundingPolicy         = "service-lease-replica-nanos-v1"
	serviceLeaseMaximumTermSeconds      int64 = 7 * 24 * 60 * 60
)

// ServiceLeasePricingAuthority freezes the terms behind a reserved, warm
// service. All monetary fields are settlement-currency nano-major-units per
// replica-hour. It deliberately names residency, control and risk allocations
// instead of hiding them inside a platform-margin row.
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
	if a.Version != serviceLeasePricingAuthorityVersion || a.Currency != currency ||
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
		"risk reserve":             a.RiskReserveNanosPerReplicaHour,
		"Merc contribution":        a.ContributionNanosPerReplicaHour,
	} {
		if rate <= 0 {
			return fmt.Errorf("service lease %s must be a positive exact hourly rate", name)
		}
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
	risk, err := serviceLeaseComponent(c, authority.RiskReserveNanosPerReplicaHour, replicaDurationNanos)
	if err != nil {
		return ServiceLeaseMoney{}, err
	}
	contribution, err := serviceLeaseComponent(c, authority.ContributionNanosPerReplicaHour, replicaDurationNanos)
	if err != nil {
		return ServiceLeaseMoney{}, err
	}
	buyer, err := supplier.Add(residency)
	if err == nil {
		buyer, err = buyer.Add(control)
	}
	if err == nil {
		buyer, err = buyer.Add(risk)
	}
	if err == nil {
		buyer, err = buyer.Add(contribution)
	}
	if err != nil || buyer.Nanos <= supplier.Nanos {
		return ServiceLeaseMoney{}, errors.New("service lease buyer charge lacks positive conserved Merc contribution")
	}
	return ServiceLeaseMoney{BuyerCharge: buyer, SupplierPayable: supplier, ResidencyCost: residency,
		ControlPlaneCost: control, RiskReserve: risk, MercContribution: contribution}, nil
}

func serviceLeaseMaximumReplicaDuration(a ServiceLeasePricingAuthority) (int64, error) {
	return mulDiv(int64(a.MaximumReplicas), a.TermSeconds, 1, false)
}

func newServiceLeasePricingDecision(in ServiceLeasePricingInputs) (PricingDecision, error) {
	if err := RequireSettlementCurrency(in.Currency.Code()); err != nil {
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
	variable := maximum.ResidencyCost.Nanos + maximum.ControlPlaneCost.Nanos + maximum.RiskReserve.Nanos
	if variable < maximum.ResidencyCost.Nanos || variable < maximum.ControlPlaneCost.Nanos {
		return PricingDecision{}, errors.New("service lease variable cost overflow")
	}
	unknowns := []string{"processor fee allocation from prepaid funding receipt"}
	decision := PricingDecision{
		Version: pricingDecisionVersion, PolicyRevision: pricingDecisionPolicyRevision,
		ExecutionMode: pricingExecutionServiceLease, Currency: in.Currency.Code(), Tier: "reserved_service",
		ServiceLease:  &authority,
		BillableUnits: float64(in.MaximumReplicas) * float64(in.TermSeconds) / 3600,
		BuyerPrice:    maximum.BuyerCharge.USDFloat(), MaximumBuyerPrice: maximum.BuyerCharge.USDFloat(),
		PrimarySupplierCost:  PricingCostComponent{Status: pricingCostModeled, Amount: maximum.SupplierPayable.USDFloat(), Basis: "selected supplier's exact warm-replica floor"},
		VerificationCost:     PricingCostComponent{Status: pricingCostNotApplicable, Basis: "service availability is metered by lease health receipts, not redundant output verification"},
		PaymentCost:          PricingCostComponent{Status: pricingCostUnknown, Basis: "processor fee is allocated only from the linked prepaid funding receipt"},
		ControlPlaneCost:     PricingCostComponent{Status: pricingCostModeled, Amount: maximum.ControlPlaneCost.USDFloat(), Basis: "versioned continuous service control allocation"},
		StorageCost:          PricingCostComponent{Status: pricingCostNotApplicable, Basis: "the service lease does not price artifact storage"},
		EgressCost:           PricingCostComponent{Status: pricingCostNotApplicable, Basis: "the service lease does not price request egress"},
		ProviderCost:         PricingCostComponent{Status: pricingCostModeled, Amount: maximum.ResidencyCost.USDFloat(), Basis: "declared-region residency allocation"},
		RiskReserve:          PricingCostComponent{Status: pricingCostModeled, Amount: maximum.RiskReserve.USDFloat(), Basis: "versioned availability and refund reserve"},
		PlatformContribution: PricingCostComponent{Status: pricingCostModeled, Amount: maximum.MercContribution.USDFloat(), Basis: "known contribution before the separately attributed processor fee"},
		FixedPoint: &FixedPointPricingDecision{Currency: in.Currency.Code(), BuyerChargeNanos: maximum.BuyerCharge.Nanos,
			AcceptedCeilingNanos: in.BuyerDeclaredCeilingNanos, SupplierEntitlementsNanos: maximum.SupplierPayable.Nanos,
			KnownVariableCostsNanos: variable, MercGrossSpreadNanos: maximum.BuyerCharge.Nanos - maximum.SupplierPayable.Nanos,
			KnownCostContributionNanos: maximum.MercContribution.Nanos, UnknownCostCategories: unknowns},
		Confidence:  0.5,
		Assumptions: []string{"reserved maximum capacity is prepaid before activation", "supplier region is declared operational scope, not a legal residency certification"},
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
