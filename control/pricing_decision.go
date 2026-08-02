package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"reflect"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
)

const (
	pricingDecisionVersion        = 1
	pricingDecisionPolicyRevision = "pricing-decision-v1"

	pricingCostModeled       = "modeled"
	pricingCostNotApplicable = "not_applicable"
	pricingCostUnknown       = "unknown"
)

// CataloguePriceAuthority is the self-contained, append-only price schedule
// entry used for one model. It prevents a quote from naming only a mutable
// models.price_per_1k cell while omitting the market board, FX and supplier
// share that made that price authoritative.
type CataloguePriceAuthority struct {
	Version                     int     `json:"version"`
	ModelID                     string  `json:"model_id"`
	JobType                     string  `json:"job_type"`
	PriceSource                 string  `json:"price_source"`
	ScheduleSHA256              string  `json:"schedule_sha256"`
	ScheduleVersion             int     `json:"schedule_version"`
	ReferenceCurrency           string  `json:"reference_currency"`
	ReferencePricePer1K         float64 `json:"reference_price_per_1k"`
	SettlementCurrency          string  `json:"settlement_currency"`
	SettlementPricePer1K        float64 `json:"settlement_price_per_1k"`
	ReferenceToSettlementRate   float64 `json:"reference_to_settlement_rate"`
	FXRevision                  string  `json:"fx_revision"`
	BoardSHA256                 string  `json:"board_sha256"`
	PriceFormula                string  `json:"price_formula"`
	SupplierShare               float64 `json:"supplier_share"`
	SupplierSharePolicyRevision string  `json:"supplier_share_policy_revision,omitempty"`
}

// PricingCostComponent never uses an unexplained zero. A component is either
// modeled, genuinely not applicable, or explicitly unknown.
type PricingCostComponent struct {
	Status string  `json:"status"`
	Amount float64 `json:"amount"`
	Basis  string  `json:"basis"`
}

// FixedPointPricingDecision is the currency-bound conservation half of a new
// PricingDecision. Human-facing floats remain projections for compatibility;
// admission, ceilings and economic identity use these integer nano-major-units.
// Historical decisions omit this object and retain their frozen arithmetic.
//
// Conservation (exact integers):
//
//	BuyerChargeNanos = SupplierEntitlementsNanos
//	                 + KnownVariableCostsNanos
//	                 + KnownCostContributionNanos
//
// KnownVariableCostsNanos includes every modeled variable cost (processor,
// control-plane, storage, egress, provider, risk). A modeled PricingCostComponent
// left out of that sum fails validation closed.
type FixedPointPricingDecision struct {
	Currency                   string   `json:"currency"`
	BuyerChargeNanos           int64    `json:"buyer_charge_nanos"`
	AcceptedCeilingNanos       int64    `json:"accepted_ceiling_nanos"`
	SupplierEntitlementsNanos  int64    `json:"supplier_entitlements_nanos"`
	KnownVariableCostsNanos    int64    `json:"known_variable_costs_nanos"`
	MercGrossSpreadNanos       int64    `json:"merc_gross_spread_nanos"`
	KnownCostContributionNanos int64    `json:"known_cost_contribution_nanos"`
	TrueNetContributionNanos   *int64   `json:"true_net_contribution_nanos,omitempty"`
	UnknownCostCategories      []string `json:"unknown_cost_categories,omitempty"`
}

// PricingDecision is the composite economic authority accepted for a quote or
// job. Every independently valid sibling decision is bound by digest here.
// Legacy rows remain readable with no PricingDecision; they are not
// retroactively reconstructed from today's catalogue.
type PricingDecision struct {
	Version        int    `json:"version"`
	PolicyRevision string `json:"policy_revision"`
	ExecutionMode  string `json:"execution_mode"`
	Currency       string `json:"currency"`
	Tier           string `json:"tier"`

	WorkloadDecisionSHA256     string `json:"workload_decision_sha256"`
	ComputePlanSHA256          string `json:"compute_plan_sha256"`
	PlacementRequirementSHA256 string `json:"placement_requirement_sha256,omitempty"`
	EconomicPlanSHA256         string `json:"economic_plan_sha256,omitempty"`
	EconomicScheduleSHA256     string `json:"economic_schedule_sha256,omitempty"`
	// CostScheduleSHA256 binds the versioned cost-policy rates (storage, egress,
	// risk reserve). Empty means a historical decision frozen before cost
	// schedule attribution; such decisions keep their unknown categories forever
	// and are never re-read under a later schedule.
	CostScheduleSHA256   string `json:"cost_schedule_sha256,omitempty"`
	CostScheduleRevision string `json:"cost_schedule_revision,omitempty"`
	// StorageAcceptedBytes / EgressAcceptedBytes are the upper-bound byte counts
	// used to model those components at acceptance. Settlement records actual
	// bytes beside these bounds in job_cost_settlements, never by rewriting this
	// decision.
	StorageAcceptedBytes             int64  `json:"storage_accepted_bytes,omitempty"`
	EgressAcceptedBytes              int64  `json:"egress_accepted_bytes,omitempty"`
	OriginQuotePricingDecisionSHA256 string `json:"origin_quote_pricing_decision_sha256,omitempty"`
	OriginPricingDecisionSHA256      string `json:"origin_pricing_decision_sha256,omitempty"`

	Catalogue     CataloguePriceAuthority        `json:"catalogue"`
	Realtime      *RealtimePricingAuthority      `json:"realtime,omitempty"`
	RealtimeReuse *RealtimeReusePricingAuthority `json:"realtime_reuse,omitempty"`
	ServiceLease  *ServiceLeasePricingAuthority  `json:"service_lease,omitempty"`

	BillableUnits                 float64 `json:"billable_units"`
	ExpectedSupplierUnitsPerSec   float64 `json:"expected_supplier_units_per_sec"`
	ExpectedSupplierSeconds       float64 `json:"expected_supplier_seconds"`
	SupplierAdmissionCeilingUSDHr float64 `json:"supplier_admission_ceiling_usd_hr"`
	ExpectedSupplierGrossUSDHr    float64 `json:"expected_supplier_gross_usd_hr"`

	// The exact entitlement and the exact floor it must clear, per task, in
	// nano-major-units. SupplierEntitlementPolicy names the arithmetic; empty means
	// a legacy plan that is still decided by the hourly comparison above.
	SupplierGrossNanos        int64  `json:"supplier_gross_nanos,omitempty"`
	SupplierRequiredNanos     int64  `json:"supplier_required_nanos,omitempty"`
	SupplierEntitlementPolicy string `json:"supplier_entitlement_policy,omitempty"`

	BuyerPrice        float64 `json:"buyer_price"`
	MaximumBuyerPrice float64 `json:"maximum_buyer_price"`

	PrimarySupplierCost  PricingCostComponent       `json:"primary_supplier_cost"`
	VerificationCost     PricingCostComponent       `json:"verification_cost"`
	PaymentCost          PricingCostComponent       `json:"payment_cost"`
	ControlPlaneCost     PricingCostComponent       `json:"control_plane_cost"`
	StorageCost          PricingCostComponent       `json:"storage_cost"`
	EgressCost           PricingCostComponent       `json:"egress_cost"`
	ProviderCost         PricingCostComponent       `json:"provider_cost"`
	RiskReserve          PricingCostComponent       `json:"risk_reserve"`
	PlatformContribution PricingCostComponent       `json:"platform_contribution"`
	FixedPoint           *FixedPointPricingDecision `json:"fixed_point,omitempty"`

	Confidence  float64  `json:"confidence"`
	Assumptions []string `json:"assumptions"`
	Unknowns    []string `json:"unknowns,omitempty"`
}

func fixedPointPricingFromScenario(
	currency string,
	buyerPrice, maximumBuyerPrice float64,
	scenario EconomicScenario,
	unknowns []string,
) (*FixedPointPricingDecision, error) {
	// Legacy float path: re-quantises already-rounded scenario fields through
	// micro-USD. Prefer fixedPointPricingFromPlan when an exact economic plan is
	// available so supplier/buyer legs match admission.
	return fixedPointPricingFromScenarioWithExtras(
		currency, buyerPrice, maximumBuyerPrice, scenario, 0, unknowns,
	)
}

// fixedPointPricingFromScenarioWithExtras is the legacy float path with
// additional modeled variable costs (storage, egress, provider, risk) folded
// into the conservation equation. Use only when the economic plan has no exact
// nano authority; exact plans must go through fixedPointPricingFromPlanWithExtras
// so the supplier and buyer legs stay the integers admission proved.
//
//	buyer = supplier + (processor+control+extras) + (contribution-extras)
//
// A negative residual contribution fails closed: the job cannot be accepted
// under a cost schedule that exceeds the economic headroom.
func fixedPointPricingFromScenarioWithExtras(
	currency string,
	buyerPrice, maximumBuyerPrice float64,
	scenario EconomicScenario,
	extraVariableNanos int64,
	unknowns []string,
) (*FixedPointPricingDecision, error) {
	toNanos := func(value float64) int64 { return usdToMicros(value) * NanosPerMicro }
	buyer := toNanos(buyerPrice)
	ceiling := toNanos(maximumBuyerPrice)
	supplier := toNanos(scenario.SupplierLiabilityUSD)
	baseVariable := toNanos(scenario.ProcessorFeeUSD) + toNanos(scenario.ControlPlaneCostUSD)
	baseContribution := toNanos(scenario.ContributionMarginUSD)
	if buyer <= 0 || ceiling < buyer || supplier <= 0 || baseContribution <= 0 {
		return nil, errors.New("fixed-point pricing lacks positive buyer, supplier, ceiling, or contribution authority")
	}
	if buyer != supplier+baseVariable+baseContribution {
		return nil, fmt.Errorf(
			"fixed-point pricing does not conserve: buyer %d != supplier %d + variable %d + contribution %d",
			buyer, supplier, baseVariable, baseContribution)
	}
	if extraVariableNanos < 0 {
		return nil, errors.New("fixed-point pricing refuses negative extra variable costs")
	}
	if extraVariableNanos >= baseContribution {
		return nil, fmt.Errorf(
			"modeled variable costs %d nanos exceed or equal known contribution headroom %d nanos",
			extraVariableNanos, baseContribution)
	}
	variable := baseVariable + extraVariableNanos
	contribution := baseContribution - extraVariableNanos
	fixed := &FixedPointPricingDecision{
		Currency: currency, BuyerChargeNanos: buyer, AcceptedCeilingNanos: ceiling,
		SupplierEntitlementsNanos: supplier, KnownVariableCostsNanos: variable,
		MercGrossSpreadNanos:       buyer - supplier,
		KnownCostContributionNanos: contribution,
		UnknownCostCategories:      append([]string(nil), unknowns...),
	}
	if len(unknowns) == 0 {
		trueNet := contribution
		fixed.TrueNetContributionNanos = &trueNet
	}
	return fixed, nil
}

// fixedPointPricingFromPlan derives FixedPoint from the plan's integer nano
// authority for the supplier and buyer legs. Variable costs (processor,
// control plane) remain float-derived for now — they still cross the micro-USD
// scenario seam — but the supplier entitlement and buyer charge are the same
// integers admission used, not a re-quantisation of already-rounded floats.
func fixedPointPricingFromPlan(
	economic EconomicPlan,
	scenario EconomicScenario,
	unknowns []string,
) (*FixedPointPricingDecision, error) {
	return fixedPointPricingFromPlanWithExtras(economic, scenario, 0, unknowns)
}

// fixedPointPricingFromPlanWithExtras is the single derivation that does both:
// plan nanos for the supplier and buyer legs, plus modeled extras inside the
// conservation equation.
//
//	buyer = supplier + (processor + control + extras) + (contribution - extras)
//
// Supplier and buyer come from the plan's integers. Extras are Merc's costs and
// never touch the supplier entitlement. A residual contribution that is not
// strictly positive fails closed. Plans with no exact nanos fall through to the
// legacy float path with the same extras.
func fixedPointPricingFromPlanWithExtras(
	economic EconomicPlan,
	scenario EconomicScenario,
	extraVariableNanos int64,
	unknowns []string,
) (*FixedPointPricingDecision, error) {
	if economic.EconomicRoundingPolicy != economicRoundingPolicy ||
		economic.SupplierPayoutPerTaskNanos <= 0 ||
		economic.BuyerChargePerTaskNanos <= 0 {
		return fixedPointPricingFromScenarioWithExtras(
			economic.Schedule.Currency,
			economic.InitialBuyerChargeUSD, economic.ReservedBuyerChargeUSD,
			scenario, extraVariableNanos, unknowns,
		)
	}
	if extraVariableNanos < 0 {
		return nil, errors.New("fixed-point pricing refuses negative extra variable costs")
	}
	tasks := scenario.AcceptedTasks
	if tasks <= 0 {
		return nil, errors.New("fixed-point pricing requires a positive accepted task count")
	}
	// Variable costs remain float-derived (processor fee, control-plane cost).
	toNanos := func(value float64) int64 { return usdToMicros(value) * NanosPerMicro }
	baseVariable := toNanos(scenario.ProcessorFeeUSD) + toNanos(scenario.ControlPlaneCostUSD)

	supplier := economic.SupplierPayoutPerTaskNanos * int64(tasks)
	slaNanos := int64(0)
	if economic.Input.SLAPremiumUSD > 0 {
		// SLA premium is still a schedule float; convert once at the nano boundary.
		// full_success_sla_met includes the premium in the buyer charge; sla_miss
		// refunds it, so net buyer nanos drop by the premium.
		sn, err := MoneyNanosFromUSDFloat(
			MustParseCurrency(economic.Schedule.Currency), economic.Input.SLAPremiumUSD)
		if err != nil {
			return nil, err
		}
		slaNanos = sn.Nanos
	}
	buyer := economic.BuyerChargePerTaskNanos*int64(tasks) + slaNanos
	if scenario.RefundUSD > 0 && slaNanos > 0 {
		// Refund is the SLA premium on sla_miss scenarios.
		buyer -= slaNanos
	}
	ceiling := economic.BuyerChargePerTaskNanos*
		int64(economic.Input.InitialTaskCount+economic.Input.ExtraTaskReserve) + slaNanos
	if economic.Input.FirmQuoteMaxUSD > 0 {
		firm, err := MoneyNanosFromUSDFloat(
			MustParseCurrency(economic.Schedule.Currency), economic.Input.FirmQuoteMaxUSD)
		if err != nil {
			return nil, err
		}
		if firm.Nanos > 0 && firm.Nanos < ceiling {
			ceiling = firm.Nanos
		}
	}
	if buyer <= 0 || ceiling < buyer || supplier <= 0 {
		return nil, errors.New("fixed-point pricing lacks positive buyer, supplier, ceiling, or contribution authority")
	}
	// Contribution is the residual that makes conservation exact in integers.
	// Variable costs may still carry micro-quantisation; the residual absorbs it.
	// Modeled extras (storage/egress/provider/risk) sit inside the variable leg
	// and reduce contribution by the same amount so the supplier leg is untouched.
	baseContribution := buyer - supplier - baseVariable
	if baseContribution <= 0 {
		return nil, errors.New("fixed-point pricing lacks positive buyer, supplier, ceiling, or contribution authority")
	}
	if extraVariableNanos >= baseContribution {
		return nil, fmt.Errorf(
			"modeled variable costs %d nanos exceed or equal known contribution headroom %d nanos",
			extraVariableNanos, baseContribution)
	}
	variable := baseVariable + extraVariableNanos
	contribution := baseContribution - extraVariableNanos
	fixed := &FixedPointPricingDecision{
		Currency: economic.Schedule.Currency, BuyerChargeNanos: buyer, AcceptedCeilingNanos: ceiling,
		SupplierEntitlementsNanos: supplier, KnownVariableCostsNanos: variable,
		MercGrossSpreadNanos:       buyer - supplier,
		KnownCostContributionNanos: contribution,
		UnknownCostCategories:      append([]string(nil), unknowns...),
	}
	if len(unknowns) == 0 {
		trueNet := contribution
		fixed.TrueNetContributionNanos = &trueNet
	}
	return fixed, nil
}

func validateFixedPointPricing(p PricingDecision) error {
	if p.FixedPoint == nil {
		return nil
	}
	f := p.FixedPoint
	if f.Currency != p.Currency || f.BuyerChargeNanos <= 0 ||
		f.AcceptedCeilingNanos < f.BuyerChargeNanos ||
		f.SupplierEntitlementsNanos < 0 ||
		(p.ExecutionMode != pricingExecutionRealtimeReuse && f.SupplierEntitlementsNanos == 0) ||
		(p.ExecutionMode == pricingExecutionRealtimeReuse && f.SupplierEntitlementsNanos != 0) ||
		f.KnownVariableCostsNanos < 0 ||
		f.KnownCostContributionNanos <= 0 ||
		f.MercGrossSpreadNanos != f.BuyerChargeNanos-f.SupplierEntitlementsNanos ||
		f.BuyerChargeNanos != f.SupplierEntitlementsNanos+
			f.KnownVariableCostsNanos+f.KnownCostContributionNanos {
		return errors.New("fixed-point pricing violates currency, ceiling, or conservation authority")
	}
	if len(f.UnknownCostCategories) == 0 {
		if f.TrueNetContributionNanos == nil ||
			*f.TrueNetContributionNanos != f.KnownCostContributionNanos {
			return errors.New("complete fixed-point pricing lacks true net contribution")
		}
	} else if f.TrueNetContributionNanos != nil {
		return errors.New("fixed-point pricing claims true net contribution with unknown costs")
	}
	// When a cost schedule is bound, every modeled variable cost component must
	// be inside KnownVariableCostsNanos (or be the supplier / contribution leg).
	// A modeled cost left out of the sum is the exact bug true-net attribution
	// exists to prevent.
	if p.CostScheduleSHA256 != "" {
		if err := validateModeledCostsAccountedInFixedPoint(p); err != nil {
			return err
		}
	}
	return nil
}

// validateModeledCostsAccountedInFixedPoint fails closed when a modeled
// PricingCostComponent is not reflected in the fixed-point conservation sum.
// Supplier legs (primary + verification) and platform contribution are the
// non-variable legs; everything else modeled must land in KnownVariableCostsNanos.
func validateModeledCostsAccountedInFixedPoint(p PricingDecision) error {
	f := p.FixedPoint
	if f == nil {
		return nil
	}
	toNanos := func(amount float64) int64 { return usdToMicros(amount) * NanosPerMicro }

	// Exact settlement authority: FixedPoint supplier is plan-nano authority.
	// Float PrimarySupplierCost/VerificationCost are six-decimal projections and
	// may differ by the same micro-quantisation gap that FixedPoint-from-plan
	// exists to close. Only the legacy float path requires those projections to
	// equal the FixedPoint supplier leg. validateFixedPointMatchesPlan is the
	// supplier invariant under the exact policy.
	if p.SupplierEntitlementPolicy != economicRoundingPolicy {
		supplierNanos := int64(0)
		if p.PrimarySupplierCost.Status == pricingCostModeled {
			supplierNanos += toNanos(p.PrimarySupplierCost.Amount)
		}
		if p.VerificationCost.Status == pricingCostModeled {
			supplierNanos += toNanos(p.VerificationCost.Amount)
		}
		if supplierNanos != f.SupplierEntitlementsNanos {
			return fmt.Errorf(
				"modeled supplier cost components %d nanos are not accounted for in "+
					"SupplierEntitlementsNanos %d",
				supplierNanos, f.SupplierEntitlementsNanos)
		}
	}
	if p.PlatformContribution.Status == pricingCostModeled {
		// Plan-path contribution may carry sub-micro residual nanos. The float
		// component is the micro projection (integer division by NanosPerMicro);
		// compare against that projection so exact and float paths share one check.
		projected := (f.KnownCostContributionNanos / NanosPerMicro) * NanosPerMicro
		if toNanos(p.PlatformContribution.Amount) != projected {
			return fmt.Errorf(
				"modeled platform contribution %d nanos is not accounted for in "+
					"KnownCostContributionNanos %d (micro-projected %d)",
				toNanos(p.PlatformContribution.Amount), f.KnownCostContributionNanos, projected)
		}
	}

	variableParts := []PricingCostComponent{
		p.PaymentCost, p.ControlPlaneCost, p.StorageCost, p.EgressCost,
		p.ProviderCost, p.RiskReserve,
	}
	var sum int64
	for _, c := range variableParts {
		if c.Status != pricingCostModeled {
			continue
		}
		sum += toNanos(c.Amount)
	}
	if sum != f.KnownVariableCostsNanos {
		return fmt.Errorf(
			"modeled variable cost components %d nanos are not accounted for in "+
				"KnownVariableCostsNanos %d",
			sum, f.KnownVariableCostsNanos)
	}
	return nil
}

// validateFixedPointMatchesPlan is the invariant whose absence was the whole
// defect: FixedPoint supplier/buyer legs must equal the plan's nano authority
// times task count (plus SLA premium on the buyer leg). Fail closed.
func validateFixedPointMatchesPlan(p PricingDecision, economic EconomicPlan, scenario EconomicScenario) error {
	if p.FixedPoint == nil || economic.EconomicRoundingPolicy != economicRoundingPolicy {
		return nil
	}
	if economic.SupplierPayoutPerTaskNanos <= 0 || economic.BuyerChargePerTaskNanos <= 0 {
		return nil
	}
	tasks := int64(scenario.AcceptedTasks)
	if tasks <= 0 {
		return errors.New("fixed-point plan match requires positive settled task count")
	}
	wantSupplier := economic.SupplierPayoutPerTaskNanos * tasks
	slaNanos := int64(0)
	if economic.Input.SLAPremiumUSD > 0 {
		sn, err := MoneyNanosFromUSDFloat(
			MustParseCurrency(economic.Schedule.Currency), economic.Input.SLAPremiumUSD)
		if err != nil {
			return err
		}
		slaNanos = sn.Nanos
	}
	wantBuyer := economic.BuyerChargePerTaskNanos*tasks + slaNanos
	if scenario.RefundUSD > 0 && slaNanos > 0 {
		wantBuyer -= slaNanos
	}
	if p.FixedPoint.SupplierEntitlementsNanos != wantSupplier {
		return fmt.Errorf(
			"fixed-point supplier entitlements %d nanos != plan supplier_payout_per_task_nanos %d × %d tasks (= %d)",
			p.FixedPoint.SupplierEntitlementsNanos,
			economic.SupplierPayoutPerTaskNanos, tasks, wantSupplier)
	}
	if p.FixedPoint.BuyerChargeNanos != wantBuyer {
		return fmt.Errorf(
			"fixed-point buyer charge %d nanos != plan buyer_charge_per_task_nanos %d × %d tasks + SLA (= %d)",
			p.FixedPoint.BuyerChargeNanos,
			economic.BuyerChargePerTaskNanos, tasks, wantBuyer)
	}
	return nil
}

func canonicalDigest(label string, value any) (string, error) {
	blob, err := json.Marshal(value)
	if err != nil {
		return "", fmt.Errorf("marshal %s: %w", label, err)
	}
	sum := sha256.Sum256(blob)
	return hex.EncodeToString(sum[:]), nil
}

func placementRequirementDigest(p PlacementRequirement) (string, error) {
	if p.Version != placementRequirementVersion {
		return "", fmt.Errorf("unsupported placement requirement version %d", p.Version)
	}
	return canonicalDigest("placement requirement", p)
}

func economicPlanDigest(p EconomicPlan) (string, error) {
	if err := ValidateEconomicPlanSnapshot(p); err != nil {
		return "", err
	}
	return canonicalDigest("economic plan", p)
}

func economicScheduleDigest(s EconomicSchedule) (string, error) {
	if reason := validateEconomicSchedule(s); reason != "" {
		return "", errors.New(reason)
	}
	return canonicalDigest("economic schedule", s)
}

func pricingDecisionDigest(p PricingDecision) (string, error) {
	if p.Version != pricingDecisionVersion {
		return "", fmt.Errorf("unsupported pricing decision version %d", p.Version)
	}
	return canonicalDigest("pricing decision", p)
}

func validateCataloguePriceAuthority(a CataloguePriceAuthority) error {
	if a.Version != cataloguePriceScheduleVersion ||
		a.ScheduleVersion != cataloguePriceScheduleVersion {
		return errors.New("catalogue authority has an unsupported schedule version")
	}
	if a.ModelID == "" || a.JobType == "" || a.PriceSource != "market_board" {
		return errors.New("catalogue authority lacks governed model identity")
	}
	if !validSHA256(a.ScheduleSHA256) || !validSHA256(a.BoardSHA256) {
		return errors.New("catalogue authority lacks valid schedule and board digests")
	}
	if a.ReferenceCurrency != catalogueReferenceCurrency ||
		a.SettlementCurrency == "" ||
		a.FXRevision == "" ||
		strings.TrimSpace(a.PriceFormula) == "" {
		return errors.New("catalogue authority lacks currency, FX, or formula provenance")
	}
	currency, err := ParseCurrency(a.SettlementCurrency)
	if err != nil || currency.Code() != a.SettlementCurrency {
		return errors.New("catalogue authority settlement currency is unsupported")
	}
	for name, value := range map[string]float64{
		"reference price":  a.ReferencePricePer1K,
		"settlement price": a.SettlementPricePer1K,
		"FX rate":          a.ReferenceToSettlementRate,
		"supplier share":   a.SupplierShare,
	} {
		if !finiteNonNegative(value) || value <= 0 {
			return fmt.Errorf("catalogue authority %s must be finite and positive", name)
		}
	}
	if a.SupplierShare > 1 {
		return errors.New("catalogue authority supplier share exceeds one")
	}
	wantSettlement := ceilPricePer1K(a.ReferencePricePer1K * a.ReferenceToSettlementRate)
	if math.Abs(a.SettlementPricePer1K-wantSettlement) > 0.0000000001 {
		return errors.New("catalogue settlement price does not match frozen reference price and FX")
	}
	return nil
}

// LoadCataloguePriceAuthority reads the current model pointer and its matching
// append-only schedule/history rows in one statement. A seed or partially
// migrated price is not accepted as production pricing authority.
func (s *Store) LoadCataloguePriceAuthority(ctx context.Context, modelID string) (CataloguePriceAuthority, error) {
	var a CataloguePriceAuthority
	err := s.pool.QueryRow(ctx, `
		SELECT s.version,m.id,COALESCE(m.job_type,''),COALESCE(m.price_source,''),
		       s.sha256,COALESCE(m.price_schedule_version,0),
		       s.reference_currency,h.reference_price_per_1k::float8,
		       s.settlement_currency,h.price_per_1k::float8,
		       s.reference_to_settlement_rate,s.fx_revision,s.board_sha256,
		       h.price_formula,COALESCE(s.schedule_json->>'supplier_share_policy_revision',''),
		       CASE WHEN s.version=1 THEN s.supplier_share ELSE h.supplier_share END
		  FROM models m
		  JOIN catalogue_price_schedules s ON s.sha256=m.price_schedule_sha256
		  JOIN model_price_history h
		    ON h.schedule_sha256=s.sha256 AND h.model_id=m.id
		 WHERE m.id=$1
		   AND m.price_source='market_board'
		   AND m.price_per_1k=h.price_per_1k
		   AND m.price_reference_per_1k=h.reference_price_per_1k
		   AND m.price_currency=h.price_currency
		   AND m.price_formula=h.price_formula`,
		modelID,
	).Scan(
		&a.Version, &a.ModelID, &a.JobType, &a.PriceSource,
		&a.ScheduleSHA256, &a.ScheduleVersion,
		&a.ReferenceCurrency, &a.ReferencePricePer1K,
		&a.SettlementCurrency, &a.SettlementPricePer1K,
		&a.ReferenceToSettlementRate, &a.FXRevision, &a.BoardSHA256,
		&a.PriceFormula, &a.SupplierSharePolicyRevision, &a.SupplierShare,
	)
	if errors.Is(err, pgx.ErrNoRows) {
		return CataloguePriceAuthority{}, fmt.Errorf(
			"model %s has no complete append-only catalogue price authority", modelID,
		)
	}
	if err != nil {
		return CataloguePriceAuthority{}, err
	}
	if err := validateCataloguePriceAuthority(a); err != nil {
		return CataloguePriceAuthority{}, fmt.Errorf("model %s catalogue authority: %w", modelID, err)
	}
	if a.SupplierSharePolicyRevision != supplierSharePolicyRevision {
		return CataloguePriceAuthority{}, fmt.Errorf(
			"model %s catalogue authority has no current supplier-share policy revision", modelID)
	}
	if err := validateSupplierSharePolicy(a.JobType, a.ModelID, a.SupplierShare); err != nil {
		return CataloguePriceAuthority{}, fmt.Errorf("model %s catalogue authority supplier share: %w", modelID, err)
	}
	if err := RequireSettlementCurrency(a.SettlementCurrency); err != nil {
		return CataloguePriceAuthority{}, fmt.Errorf(
			"model %s catalogue authority is not current settlement currency: %w",
			modelID, err,
		)
	}
	return a, nil
}

// LoadCataloguePriceAuthorityAtSchedule resolves a catalogue row that existed
// under a specific schedule digest, not "today's price for this model".
// Already-accepted jobs stay valid when a later reprice moves models.price_*.
// Missing schedule digests fail closed: the caller must not treat silence as
// acceptance.
func (s *Store) LoadCataloguePriceAuthorityAtSchedule(
	ctx context.Context,
	scheduleSHA256 string,
	scheduleVersion int,
	modelID string,
	jobType string,
) (CataloguePriceAuthority, error) {
	var a CataloguePriceAuthority
	// price_source is market_board for every schedule-applied history row; do not
	// read models.price_source, which may have been re-seeded since the schedule.
	err := s.pool.QueryRow(ctx, `
		SELECT s.version,m.id,COALESCE(m.job_type,''),'market_board',
		       s.sha256,s.version,
		       s.reference_currency,h.reference_price_per_1k::float8,
		       s.settlement_currency,h.price_per_1k::float8,
		       s.reference_to_settlement_rate,s.fx_revision,s.board_sha256,
		       h.price_formula,COALESCE(s.schedule_json->>'supplier_share_policy_revision',''),
		       CASE WHEN s.version=1 THEN s.supplier_share ELSE h.supplier_share END
		  FROM catalogue_price_schedules s
		  JOIN model_price_history h
		    ON h.schedule_sha256=s.sha256 AND h.model_id=$2
		  JOIN models m ON m.id=h.model_id
		 WHERE s.sha256=$1
		   AND s.version=$3
		   AND m.id=$2
		   AND COALESCE(m.job_type,'')=$4`,
		scheduleSHA256, modelID, scheduleVersion, jobType,
	).Scan(
		&a.Version, &a.ModelID, &a.JobType, &a.PriceSource,
		&a.ScheduleSHA256, &a.ScheduleVersion,
		&a.ReferenceCurrency, &a.ReferencePricePer1K,
		&a.SettlementCurrency, &a.SettlementPricePer1K,
		&a.ReferenceToSettlementRate, &a.FXRevision, &a.BoardSHA256,
		&a.PriceFormula, &a.SupplierSharePolicyRevision, &a.SupplierShare,
	)
	if errors.Is(err, pgx.ErrNoRows) {
		return CataloguePriceAuthority{}, fmt.Errorf(
			"catalogue schedule digest %s version %d is not resolvable for model %s job type %s",
			scheduleSHA256, scheduleVersion, modelID, jobType,
		)
	}
	if err != nil {
		return CataloguePriceAuthority{}, err
	}
	if err := validateCataloguePriceAuthority(a); err != nil {
		return CataloguePriceAuthority{}, fmt.Errorf(
			"catalogue schedule digest %s resolved to invalid authority: %w",
			scheduleSHA256, err,
		)
	}
	return a, nil
}

// cataloguePriceAuthorityFieldMismatch refuses a stored catalogue that does not
// equal the append-only authority, naming the disagreeing field.
func cataloguePriceAuthorityFieldMismatch(stored, authority CataloguePriceAuthority) error {
	floatDiffers := func(a, b float64) bool {
		return math.Abs(a-b) > 0.0000000001
	}
	switch {
	case stored.Version != authority.Version:
		return errors.New("pricing decision catalogue Version does not match append-only authority")
	case stored.ModelID != authority.ModelID:
		return errors.New("pricing decision catalogue ModelID does not match append-only authority")
	case stored.JobType != authority.JobType:
		return errors.New("pricing decision catalogue JobType does not match append-only authority")
	case stored.PriceSource != authority.PriceSource:
		return errors.New("pricing decision catalogue PriceSource does not match append-only authority")
	case stored.ScheduleSHA256 != authority.ScheduleSHA256:
		return errors.New("pricing decision catalogue ScheduleSHA256 does not match append-only authority")
	case stored.ScheduleVersion != authority.ScheduleVersion:
		return errors.New("pricing decision catalogue ScheduleVersion does not match append-only authority")
	case stored.ReferenceCurrency != authority.ReferenceCurrency:
		return errors.New("pricing decision catalogue ReferenceCurrency does not match append-only authority")
	case floatDiffers(stored.ReferencePricePer1K, authority.ReferencePricePer1K):
		return errors.New("pricing decision catalogue ReferencePricePer1K does not match append-only authority")
	case stored.SettlementCurrency != authority.SettlementCurrency:
		return errors.New("pricing decision catalogue SettlementCurrency does not match append-only authority")
	case floatDiffers(stored.SettlementPricePer1K, authority.SettlementPricePer1K):
		return errors.New("pricing decision catalogue SettlementPricePer1K does not match append-only authority")
	case floatDiffers(stored.ReferenceToSettlementRate, authority.ReferenceToSettlementRate):
		return errors.New("pricing decision catalogue ReferenceToSettlementRate does not match append-only authority")
	case stored.FXRevision != authority.FXRevision:
		return errors.New("pricing decision catalogue FXRevision does not match append-only authority")
	case stored.BoardSHA256 != authority.BoardSHA256:
		return errors.New("pricing decision catalogue BoardSHA256 does not match append-only authority")
	case stored.PriceFormula != authority.PriceFormula:
		return errors.New("pricing decision catalogue PriceFormula does not match append-only authority")
	case floatDiffers(stored.SupplierShare, authority.SupplierShare):
		return errors.New("pricing decision catalogue SupplierShare does not match append-only authority")
	case stored.SupplierSharePolicyRevision != authority.SupplierSharePolicyRevision:
		return errors.New("pricing decision catalogue SupplierSharePolicyRevision does not match append-only authority")
	}
	return nil
}

// selectCataloguePriceAuthority makes the quote pinning rule executable: a
// bound submission must not consult the mutable models pointer at all.
func selectCataloguePriceAuthority(
	bound *boundQuote,
	unbound func() (CataloguePriceAuthority, error),
) (CataloguePriceAuthority, error) {
	if bound != nil {
		if err := validateCataloguePriceAuthority(bound.Pricing.Catalogue); err != nil {
			return CataloguePriceAuthority{}, fmt.Errorf(
				"bound quote lacks valid catalogue authority: %w", err,
			)
		}
		return bound.Pricing.Catalogue, nil
	}
	return unbound()
}

func estimateJobSettlementWithAuthority(
	a CataloguePriceAuthority,
	jobType string,
	inputBytesLen, nLines int,
	maxTokens uint32,
	tier string,
) (float64, error) {
	return estimateJobSettlementForJobType(a, JobType{Type: jobType, MaxTokens: maxTokens}, inputBytesLen, nLines, tier)
}

// estimateJobSettlementForJobType keeps the complete request geometry in the
// pricing authority. In particular, media_rendering is sold in declared output
// pixels, not in the size of the closed scene JSON document.
func estimateJobSettlementForJobType(
	a CataloguePriceAuthority,
	jobType JobType,
	inputBytesLen, nLines int,
	tier string,
) (float64, error) {
	if err := validateCataloguePriceAuthority(a); err != nil {
		return 0, err
	}
	if a.JobType != jobType.Type {
		return 0, fmt.Errorf("catalogue job type %s does not match %s", a.JobType, jobType.Type)
	}
	units := settlementBillableUnitsForJobType(a, jobType, inputBytesLen, nLines)
	gross, err := CatalogueGrossNanos(
		MustParseCurrency(a.SettlementCurrency),
		catalogueSettlementPriceNanosPer1K(a, tier),
		NanoWorkUnitsFromFloat(units),
	)
	if err != nil {
		return 0, err
	}
	// The micro-USD PROJECTION of the exact figure, not a second derivation of it.
	//
	// This function used to compute the same product in float64 and round it, which
	// made it an independent authority for the quantity CatalogueGrossNanos now
	// owns — and the two disagreed by 30% on a job small enough for one micro to
	// matter. It is kept because the ledger, the wire contract and every frozen
	// plan present money in micro-USD; it is no longer where the number comes from.
	rounded := roundUSD(gross.USDFloat())
	if rounded == 0 && units > 0 {
		return microsToUSD(1), nil
	}
	return rounded, nil
}

// settlementBillableUnitsForGeometry is the complete billable unit count for one
// job: input-side settlement units plus, for generative work, the output tokens
// the buyer bought the right to.
//
// Split out because the exact money path and the micro projection must count the
// same units. They did not have to before, and a units formula that lives inside
// one of two pricing functions is a second authority waiting to drift.
func settlementBillableUnitsForGeometry(
	a CataloguePriceAuthority, jobType string, inputBytesLen, nLines int, maxTokens uint32,
) float64 {
	return settlementBillableUnitsForJobType(a, JobType{Type: jobType, MaxTokens: maxTokens}, inputBytesLen, nLines)
}

func settlementBillableUnitsForJobType(
	a CataloguePriceAuthority, jobType JobType, inputBytesLen, nLines int,
) float64 {
	units := settlementInputUnitsForJobType(jobType, nLines, int64(inputBytesLen))
	if generativeJobType(jobType.Type) && nLines > 0 {
		outTokensPerRecord := jobType.MaxTokens
		if outTokensPerRecord == 0 {
			outTokensPerRecord = defaultQuoteMaxTokens
		}
		units += float64(nLines) * float64(outTokensPerRecord)
	}
	return units
}

// exactBaseComputeNanos is the economic plan's base compute, exact, derived from
// the catalogue for the same geometry the compute plan will freeze.
//
// Per PRIMARY task first, then multiplied back up by the number of tasks the
// economic plan counts. That order matters: the pricing decision derives the
// supplier's floor from the units in ONE task, so deriving the plan's base from a
// whole-job total and dividing it again introduces a second rounding between two
// numbers that have to be equal. Multiplying a per-task figure by a task count is
// exact.
//
// Returns 0 when the geometry cannot produce an exact figure, which BuildEconomicPlan
// reads as "no exact base was offered" and falls back to the float seam.
func exactBaseComputeNanos(
	a CataloguePriceAuthority,
	jobType, tier string,
	inputBytesLen, nLines int,
	maxTokens uint32,
	primaryTasks, initialEconomicTasks int,
) int64 {
	return exactBaseComputeNanosForJobType(a, JobType{Type: jobType, MaxTokens: maxTokens}, tier,
		inputBytesLen, nLines, primaryTasks, initialEconomicTasks)
}

func exactBaseComputeNanosForJobType(
	a CataloguePriceAuthority,
	jobType JobType,
	tier string,
	inputBytesLen, nLines int,
	primaryTasks, initialEconomicTasks int,
) int64 {
	if primaryTasks <= 0 || initialEconomicTasks <= 0 {
		return 0
	}
	units := settlementBillableUnitsForJobType(a, jobType, inputBytesLen, nLines)
	if units <= 0 {
		return 0
	}
	gross, _, err := exactTaskEconomics(a, tier, units/float64(primaryTasks))
	if err != nil || gross.Nanos <= 0 {
		return 0
	}
	total, err := mulDiv(gross.Nanos, int64(initialEconomicTasks), 1, false)
	if err != nil {
		return 0
	}
	return total
}

// catalogueSettlementPriceNanosPer1K is the tier-adjusted catalogue price, exact.
//
// The settlement price, deliberately — not the USD reference price. Deriving the
// supplier floor from ReferencePricePer1K while the entitlement was denominated in
// the settlement currency put an entire FX rate between two numbers that were
// then compared as though they were the same currency. The comparison was off by
// 1.37x on this deployment and nothing in the type system could object, because
// both sides were float64 named USD.
func catalogueSettlementPriceNanosPer1K(
	a CataloguePriceAuthority, tier string,
) NanoUSDPerThousandUnits {
	return nanosPer1KFromFloat(a.SettlementPricePer1K * tierMultiplier(tier))
}

// exactTaskEconomics is THE derivation. Every per-task money figure in the
// distributed path comes from here and nowhere else.
//
//	catalogue settlement unit price
//	  x exact fractional units in the task
//	  -> exact buyer gross      (rounds DOWN, the buyer's direction)
//	  x explicit supplier share
//	  -> exact supplier entitlement AND the floor it must clear (rounds UP)
//
// The floor and the entitlement are the same expression evaluated once. That is
// why admission can compare them as an identity: they are not two estimates of one
// quantity that have to be reconciled within a tolerance, they ARE one quantity.
//
// The throughput and the modeled duration are deliberately absent. They cancel:
//
//	ceiling  = unitsPerSec x 3600/1000 x price x share
//	seconds  = units / unitsPerSec
//	required = ceiling x seconds / 3600 = units/1000 x price x share
//
// Carrying them through the arithmetic anyway is what produced the 29,093-nano
// floor — an integer division truncated the sub-second duration to a whole second
// — and it also made a money figure depend on a dated benchmark that can be
// revalidated out from under an already-accepted receipt.
func exactTaskEconomics(
	a CataloguePriceAuthority, tier string, unitsPerTask float64,
) (gross, entitlement MoneyNanos, err error) {
	currency, err := ParseCurrency(a.SettlementCurrency)
	if err != nil {
		return MoneyNanos{}, MoneyNanos{}, err
	}
	// Refuse rather than return zero. A zero floor is not a cheap job, it is a
	// floor that admits anything — and the caller reads an error as "this plan
	// cannot be compared exactly" while it would read a zero as an answer.
	// NanoWorkUnitsFromFloat also returns 0 for a non-finite or overflowing unit
	// count, so this is the same guard for both.
	units := NanoWorkUnitsFromFloat(unitsPerTask)
	if units <= 0 {
		return MoneyNanos{}, MoneyNanos{}, fmt.Errorf(
			"cannot derive exact task economics from %v billable units", unitsPerTask)
	}
	gross, err = CatalogueGrossNanos(
		currency, catalogueSettlementPriceNanosPer1K(a, tier), units,
	)
	if err != nil {
		return MoneyNanos{}, MoneyNanos{}, err
	}
	if gross.Nanos <= 0 {
		return MoneyNanos{}, MoneyNanos{}, fmt.Errorf(
			"catalogue prices %v units at zero, so no supplier floor can be derived",
			unitsPerTask)
	}
	entitlement, err = SupplierEntitlementNanos(gross, a.SupplierShare)
	if err != nil {
		return MoneyNanos{}, MoneyNanos{}, err
	}
	return gross, entitlement, nil
}

// pricingBillableUnitsForComputePlan keeps historical pricing decisions
// verifiable while making every version-3-and-later decision use the exact
// input-side units that created the catalogue money estimate. Version-1 and
// version-2 receipts retain their historical body/whole-input presentation;
// they must not be silently reinterpreted or re-priced after acceptance.
func pricingBillableUnitsForComputePlan(compute ComputePlan) float64 {
	input := float64(compute.EstimatedInputTokens)
	if compute.Version == computePlanVersionV3 || compute.Version == computePlanVersion {
		input = compute.SettlementInputUnits
	}
	return input + float64(compute.EstimatedOutputTokens)
}

// supplierAdmissionCeilingUSDHr is a modeled ask ceiling used solely by the
// scheduler's min-payout eligibility gate. It is not a promise of realized
// hourly earnings: actual payment follows accepted task units.
//
// The throughput comes from the governed runtime-cell performance binding, not
// from a constant in this package. A hardcoded rate here decided whether a
// default install could claim any work at all, and it was wrong by more than an
// order of magnitude with nothing in the build able to notice.
//
// candidateCells is the frozen workload's runtime candidate set. An empty set
// prices against every routable cell for the model, which is only correct for a
// model-level display rate that is not attached to a particular job.
func supplierAdmissionCeilingUSDHr(
	a CataloguePriceAuthority, jobType, tier string, candidateCells []string,
) (float64, error) {
	if err := validateCataloguePriceAuthority(a); err != nil {
		return 0, err
	}
	if a.JobType != jobType {
		return 0, errors.New("catalogue job type does not match placement job type")
	}
	unitsPerSec, _, err := admissionUnitsPerSec(jobType, a.ModelID, candidateCells, time.Now())
	if err != nil {
		return 0, err
	}
	return expectedSupplierUSDHr(unitsPerSec, a.ReferencePricePer1K, a.SupplierShare, tier), nil
}

func modeledCost(amount float64, basis string) PricingCostComponent {
	return PricingCostComponent{Status: pricingCostModeled, Amount: roundEconomicUSD(amount), Basis: basis}
}

func unknownCost(basis string) PricingCostComponent {
	return PricingCostComponent{Status: pricingCostUnknown, Basis: basis}
}

func notApplicableCost(basis string) PricingCostComponent {
	return PricingCostComponent{Status: pricingCostNotApplicable, Basis: basis}
}

func fullSuccessEconomicScenario(plan EconomicPlan) (EconomicScenario, error) {
	for _, scenario := range plan.Scenarios {
		if scenario.Name == "full_success_sla_met" {
			return scenario, nil
		}
	}
	return EconomicScenario{}, errors.New("economic plan lacks full_success_sla_met scenario")
}

func newDistributedPricingDecision(
	workload WorkloadDecision,
	compute ComputePlan,
	placement PlacementRequirement,
	economic EconomicPlan,
	catalogue CataloguePriceAuthority,
	tier string,
	originQuotePricingSHA string,
) (PricingDecision, error) {
	unitsPerSec, _, err := admissionUnitsPerSec(
		workload.RuntimeJobType, catalogue.ModelID,
		admissionCellsForWorkload(workload), time.Now(),
	)
	if err != nil {
		return PricingDecision{}, err
	}
	return distributedPricingDecisionAtRate(
		workload, compute, placement, economic, catalogue, tier,
		originQuotePricingSHA, unitsPerSec,
	)
}

// distributedPricingDecisionAtRate takes the supplier unit rate rather than
// resolving it, so rebuilding a stored decision does not depend on what the
// runtime evidence says TODAY. The rate became time-dependent when it started
// coming from a dated benchmark: a receipt crossing the revalidation window
// would otherwise change the rebuilt number and make every already-accepted job
// in the database fail its own snapshot check months after acceptance.
//
// The rate handed in here is NOT trusted on the strength of the ceiling equality
// below. That equality only pins the rate to placement.OfferedRateUsdHr, and an
// attacker who can rewrite a stored pricing decision can rewrite the stored
// placement beside it; the two then agree at any rate at all. Every caller that
// takes the rate from a stored record must first put it through
// governedAdmissionUnitRates, which is what ValidateDistributedPricingDecisionSnapshot
// does.
func distributedPricingDecisionAtRate(
	workload WorkloadDecision,
	compute ComputePlan,
	placement PlacementRequirement,
	economic EconomicPlan,
	catalogue CataloguePriceAuthority,
	tier string,
	originQuotePricingSHA string,
	unitsPerSec float64,
) (PricingDecision, error) {
	if err := ValidateComputePlanEconomicSnapshot(compute, workload, economic); err != nil {
		return PricingDecision{}, err
	}
	if err := validatePlacementRequirement(placement, workload); err != nil {
		return PricingDecision{}, err
	}
	if err := validateCataloguePriceAuthority(catalogue); err != nil {
		return PricingDecision{}, err
	}
	if catalogue.ModelID != workload.Binding.Model.Ref ||
		catalogue.JobType != workload.RuntimeJobType ||
		catalogue.SettlementCurrency != economic.Schedule.Currency ||
		tier != workload.Binding.Tier ||
		math.Abs(catalogue.SupplierShare-economic.Input.SupplierShare) > 0.000000001 {
		return PricingDecision{}, errors.New("catalogue, workload and economic authority disagree")
	}
	if originQuotePricingSHA != "" && !validSHA256(originQuotePricingSHA) {
		return PricingDecision{}, errors.New("origin quote pricing digest is invalid")
	}
	if catalogue.JobType != workload.RuntimeJobType {
		return PricingDecision{}, errors.New("catalogue job type does not match placement job type")
	}
	derivedCeiling := expectedSupplierUSDHr(
		unitsPerSec, catalogue.ReferencePricePer1K, catalogue.SupplierShare, tier)
	// The tolerance is relative because the only difference being allowed for is
	// the float32 round-trip the wire contract imposes, and that error scales
	// with the value. A fixed absolute epsilon silently became a real constraint
	// once throughput came from a measurement instead of a small hardcoded
	// number: the same lossless round-trip that passed at $0.007/hr failed at
	// $58/hr. 1e-6 relative is roughly eight float32 ulps and still far below any
	// economically meaningful discrepancy.
	tolerance := math.Abs(derivedCeiling) * 0.000001
	if tolerance < 0.000001 {
		tolerance = 0.000001
	}
	if math.Abs(float64(placement.OfferedRateUsdHr)-derivedCeiling) > tolerance {
		return PricingDecision{}, errors.New("placement supplier admission ceiling was not derived from catalogue authority")
	}
	// The scheduler and wire contract use float32. Freeze that exact operational
	// value in pricing too so the receipt never displays a nearby float64 that
	// was not actually used for admission.
	ceiling := float64(placement.OfferedRateUsdHr)
	workloadSHA, err := workloadDecisionDigest(workload)
	if err != nil {
		return PricingDecision{}, err
	}
	computeSHA, err := computePlanDigest(compute)
	if err != nil {
		return PricingDecision{}, err
	}
	placementSHA, err := placementRequirementDigest(placement)
	if err != nil {
		return PricingDecision{}, err
	}
	economicSHA, err := economicPlanDigest(economic)
	if err != nil {
		return PricingDecision{}, err
	}
	scheduleSHA, err := economicScheduleDigest(economic.Schedule)
	if err != nil {
		return PricingDecision{}, err
	}
	scenario, err := fullSuccessEconomicScenario(economic)
	if err != nil {
		return PricingDecision{}, err
	}
	primarySupplier := economic.SupplierPayoutPerTaskUSD * float64(compute.PrimaryTasks)
	verification := economic.SupplierPayoutPerTaskUSD *
		float64(compute.RedundancyTasks+compute.HoneypotTasks)
	billableUnits := pricingBillableUnitsForComputePlan(compute)
	expectedSeconds := 0.0
	if compute.PrimaryTasks > 0 && unitsPerSec > 0 {
		expectedSeconds = billableUnits / unitsPerSec *
			float64(compute.TotalInitialTasks) / float64(compute.PrimaryTasks)
	}
	// The exact entitlement and the exact floor, per task — one derivation, run
	// once, in the settlement currency, from the catalogue price and the exact
	// fractional units of this task. See exactTaskEconomics for why the throughput
	// and the modeled duration are not in this arithmetic.
	//
	// The entitlement is the plan's own frozen nano figure. When the plan froze its
	// base compute from the same catalogue authority (which every plan built after
	// this change does), the two sides are the same expression and the comparison
	// below is an identity. When the min-billable floor lifted the plan's base, the
	// entitlement is strictly larger, which admission accepts.
	//
	// When the plan carries the exact policy and positive supplier nanos, a
	// failure of exactTaskEconomics MUST refuse the decision rather than fall
	// through to the legacy hourly admission comparison — that silent downgrade
	// is how "cannot price exactly" became indistinguishable from legacy.
	var supplierGrossNanos, supplierRequiredNanos int64
	var supplierEntitlementPolicy string
	if economic.EconomicRoundingPolicy == economicRoundingPolicy &&
		economic.SupplierPayoutPerTaskNanos > 0 && compute.PrimaryTasks > 0 {
		unitsPerTask := billableUnits / float64(compute.PrimaryTasks)
		_, required, rerr := exactTaskEconomics(catalogue, tier, unitsPerTask)
		if rerr != nil {
			return PricingDecision{}, fmt.Errorf(
				"exact entitlement policy %q is set but exactTaskEconomics failed: %w",
				economicRoundingPolicy, rerr)
		}
		supplierGrossNanos = economic.SupplierPayoutPerTaskNanos
		supplierRequiredNanos = required.Nanos
		supplierEntitlementPolicy = economicRoundingPolicy
	}

	expectedGrossUSDHr := 0.0
	if expectedSeconds > 0 {
		settlementSupplier := primarySupplier + verification
		expectedGrossUSDHr = settlementSupplier /
			catalogue.ReferenceToSettlementRate / expectedSeconds * 3600
	}
	controlBasis := "economic schedule per-task control-plane cost"
	contributionBasis := "buyer price less modeled supplier, processor and control-plane costs"
	pricingAssumptions := []string{
		"supplier admission uses the governed runtime-cell benchmark binding, haircut to a conservative lower bound, and the USD reference schedule",
		"actual supplier liability is frozen per accepted task, not by elapsed wall-clock hour",
		"unknown cost components are not silently treated as modeled zero",
	}
	if economic.Schedule.ControlPlaneAllocationPolicy == controlPlaneAllocationChargeBatchV1 {
		controlBasis = "declared account/invoice overhead allocated across the economic charge batch"
		contributionBasis = "known-cost contribution: buyer price less modeled supplier, processor and allocated control-plane costs; not true net while named costs remain unknown"
		pricingAssumptions = append(pricingAssumptions,
			"fixed account/invoice overhead is allocated over the collector's minimum economic charge batch")
	}

	// Cost schedule attribution. Always bind the governed default for new
	// decisions so storage/egress/provider/risk can leave the unknown set.
	// Historical decisions rebuilt without a schedule keep the pre-schedule
	// unknown markers via the migration path below only when schedule load fails
	// closed — LoadCostScheduleFromEnv always returns a validated schedule.
	costSchedule, cerr := LoadCostScheduleFromEnv()
	if cerr != nil {
		// Fall back to currency-bound default when env is incomplete in tests.
		costSchedule = DefaultCostSchedule(economic.Schedule.Currency)
		if reason := validateCostSchedule(costSchedule); reason != "" {
			return PricingDecision{}, fmt.Errorf("cost schedule unavailable: %w", cerr)
		}
	}
	if costSchedule.Currency != economic.Schedule.Currency {
		costSchedule.Currency = economic.Schedule.Currency
	}
	costSHA, cerr := costScheduleDigest(costSchedule)
	if cerr != nil {
		return PricingDecision{}, fmt.Errorf("cost schedule digest: %w", cerr)
	}

	// Storage and egress: upper-bound from frozen compute plan geometry and the
	// job-object retention period. Settlement recomputes from actual bytes.
	storageBytes, egressBytes := declaredOutputBytesBound(compute)
	retention := jobObjectRetentionPeriod()
	storageNanos, serr := storageNanosForBytes(costSchedule, storageBytes, retention)
	if serr != nil {
		return PricingDecision{}, fmt.Errorf("storage cost model: %w", serr)
	}
	egressNanos, eerr := egressNanosForBytes(costSchedule, egressBytes)
	if eerr != nil {
		return PricingDecision{}, fmt.Errorf("egress cost model: %w", eerr)
	}
	storageCost := modeledCost(nanosToEconomicUSD(storageNanos),
		fmt.Sprintf("policy storage bound: %d bytes × retention %s at schedule rate; %s",
			storageBytes, retention, costSchedule.StorageProvenance))
	egressCost := modeledCost(nanosToEconomicUSD(egressNanos),
		fmt.Sprintf("policy egress bound: %d result bytes at schedule rate; %s",
			egressBytes, costSchedule.EgressProvenance))
	// A workload that stores nothing (no input, no declared output) is N/A.
	if storageBytes == 0 {
		storageCost = notApplicableCost(
			"compute plan declares zero retained payload bytes; no object-storage cost applies")
	}
	if egressBytes == 0 {
		egressCost = notApplicableCost(
			"compute plan declares zero result bytes; no egress cost applies")
	}

	// Provider: N/A for community/owned; modeled or unknown for cloud-backed.
	providerCost := providerCostComponentForPlacement(
		placement.RuntimeCellID, placement.HWClasses, expectedSeconds,
	)

	// Risk reserve: real policy money on the buyer charge.
	buyerNanos := usdToMicros(economic.InitialBuyerChargeUSD) * NanosPerMicro
	riskNanos, rerr := riskReserveNanos(costSchedule, buyerNanos)
	if rerr != nil {
		return PricingDecision{}, fmt.Errorf("risk reserve model: %w", rerr)
	}
	riskCost := modeledCost(nanosToEconomicUSD(riskNanos),
		fmt.Sprintf("%d bps of buyer charge as platform risk reserve; %s",
			costSchedule.RiskReserveBasisPoints, costSchedule.RiskReserveProvenance))

	// Extra variable costs beyond processor+control, in the same micro-aligned
	// nanos the fixed-point layer and validateModeledCostsAccountedInFixedPoint
	// use. Taking the rounded component amounts (not the pre-projection nanos)
	// keeps conservation exact under the six-decimal ledger float.
	componentNanos := func(c PricingCostComponent) int64 {
		if c.Status != pricingCostModeled {
			return 0
		}
		return usdToMicros(c.Amount) * NanosPerMicro
	}
	extraVariableNanos := componentNanos(storageCost) + componentNanos(egressCost) +
		componentNanos(providerCost) + componentNanos(riskCost)

	// Contribution basis text; the residual amount is taken from fixed-point
	// after the extras are applied so float and nano legs cannot disagree.
	contributionBasis = "known-cost contribution after modeled supplier, processor, " +
		"control-plane, storage, egress, provider and risk costs"
	if providerCost.Status == pricingCostUnknown ||
		storageCost.Status == pricingCostUnknown ||
		egressCost.Status == pricingCostUnknown ||
		riskCost.Status == pricingCostUnknown {
		contributionBasis += "; not true net while named costs remain unknown"
	} else {
		contributionBasis += "; true net when every named cost is modeled or not applicable"
	}
	contributionUSD := scenario.ContributionMarginUSD
	if extraVariableNanos > 0 {
		contributionUSD = microsToUSD(
			usdToMicros(scenario.ContributionMarginUSD) - extraVariableNanos/NanosPerMicro)
	}

	unknowns := make([]string, 0, 4)
	if storageCost.Status == pricingCostUnknown {
		unknowns = append(unknowns, "storage cost")
	}
	if egressCost.Status == pricingCostUnknown {
		unknowns = append(unknowns, "egress cost")
	}
	if providerCost.Status == pricingCostUnknown {
		unknowns = append(unknowns, "provider cost")
	}
	if riskCost.Status == pricingCostUnknown {
		unknowns = append(unknowns, "risk reserve")
	}
	pricingAssumptions = append(pricingAssumptions,
		"storage and egress use cost-schedule policy rates on declared compute-plan byte bounds and job-object retention; settlement remeasures actual artifact bytes",
		"provider cost is not_applicable for owned/community supply and governed pod-rate × seconds for cloud-backed cells",
		"risk reserve is accrued to a platform ledger account at settlement and released or consumed after the dispute window",
	)

	out := PricingDecision{
		Version: pricingDecisionVersion, PolicyRevision: pricingDecisionPolicyRevision,
		ExecutionMode: computeExecutionDistributed,
		Currency:      economic.Schedule.Currency, Tier: tier,
		WorkloadDecisionSHA256: workloadSHA, ComputePlanSHA256: computeSHA,
		PlacementRequirementSHA256: placementSHA,
		EconomicPlanSHA256:         economicSHA, EconomicScheduleSHA256: scheduleSHA,
		CostScheduleSHA256:               costSHA,
		CostScheduleRevision:             costSchedule.Revision,
		StorageAcceptedBytes:             storageBytes,
		EgressAcceptedBytes:              egressBytes,
		OriginQuotePricingDecisionSHA256: originQuotePricingSHA,
		Catalogue:                        catalogue,
		BillableUnits:                    billableUnits,
		ExpectedSupplierUnitsPerSec:      unitsPerSec,
		ExpectedSupplierSeconds:          expectedSeconds,
		SupplierAdmissionCeilingUSDHr:    ceiling,
		ExpectedSupplierGrossUSDHr:       expectedGrossUSDHr,
		SupplierGrossNanos:               supplierGrossNanos,
		SupplierRequiredNanos:            supplierRequiredNanos,
		SupplierEntitlementPolicy:        supplierEntitlementPolicy,
		BuyerPrice:                       economic.InitialBuyerChargeUSD,
		MaximumBuyerPrice:                economic.ReservedBuyerChargeUSD,
		PrimarySupplierCost: modeledCost(primarySupplier,
			"frozen supplier payout per task × primary task count"),
		VerificationCost: modeledCost(verification,
			"frozen supplier payout per task × redundancy and honeypot task count"),
		PaymentCost: modeledCost(scenario.ProcessorFeeUSD,
			"economic schedule processor percentage and allocated fixed fee"),
		ControlPlaneCost: modeledCost(scenario.ControlPlaneCostUSD,
			controlBasis),
		StorageCost:  storageCost,
		EgressCost:   egressCost,
		ProviderCost: providerCost,
		RiskReserve:  riskCost,
		PlatformContribution: modeledCost(contributionUSD,
			contributionBasis),
		Confidence:  compute.Confidence,
		Assumptions: pricingAssumptions,
	}
	// Nil, not empty, when every category is known: omitempty JSON round-trips
	// drop empty slices to nil, and ValidateDistributedPricingDecisionSnapshot
	// uses reflect.DeepEqual.
	if len(unknowns) > 0 {
		out.Unknowns = append([]string(nil), unknowns...)
	}
	if economic.Schedule.ControlPlaneAllocationPolicy == controlPlaneAllocationChargeBatchV1 {
		fixed, ferr := fixedPointPricingFromPlanWithExtras(
			economic, scenario, extraVariableNanos, out.Unknowns,
		)
		if ferr != nil {
			return PricingDecision{}, ferr
		}
		out.FixedPoint = fixed
		// Float contribution realignment: project KnownCostContributionNanos back
		// through micros so the float conservation check agrees with the nano
		// equation. Runs before validateFixedPointMatchesPlan. The order cannot
		// change the plan match — that validator only reads FixedPoint supplier
		// and buyer legs, which realignment never touches — but realignment must
		// complete before validatePricingCostShape, which checks the float
		// PlatformContribution against the FixedPoint residual.
		if fixed != nil {
			out.PlatformContribution = modeledCost(
				microsToUSD(fixed.KnownCostContributionNanos/NanosPerMicro), contributionBasis)
		}
		if err := validateFixedPointMatchesPlan(out, economic, scenario); err != nil {
			return PricingDecision{}, err
		}
	}
	return out, validatePricingCostShape(out)
}

func newExactReusePricingDecision(
	workload WorkloadDecision,
	compute ComputePlan,
	catalogue CataloguePriceAuthority,
	tier string,
	buyerCharge float64,
	originPricingSHA string,
) (PricingDecision, error) {
	if err := ValidateFrozenComputePlanSnapshot(compute, workload); err != nil {
		return PricingDecision{}, err
	}
	if compute.ExecutionMode != computeExecutionExactReuse {
		return PricingDecision{}, errors.New("exact-reuse pricing requires exact-reuse compute")
	}
	if err := validateCataloguePriceAuthority(catalogue); err != nil {
		return PricingDecision{}, err
	}
	if catalogue.ModelID != workload.Binding.Model.Ref ||
		catalogue.JobType != workload.RuntimeJobType ||
		catalogue.SettlementCurrency != SettlementCurrencyCode() ||
		tier != workload.Binding.Tier {
		return PricingDecision{}, errors.New("exact-reuse catalogue and workload authority disagree")
	}
	if buyerCharge <= 0 || compute.BaseComputeUSD != roundEconomicUSD(buyerCharge) {
		return PricingDecision{}, errors.New("exact-reuse buyer charge does not match compute authority")
	}
	if originPricingSHA != "" && !validSHA256(originPricingSHA) {
		return PricingDecision{}, errors.New("exact-reuse origin pricing digest is invalid")
	}
	workloadSHA, err := workloadDecisionDigest(workload)
	if err != nil {
		return PricingDecision{}, err
	}
	computeSHA, err := computePlanDigest(compute)
	if err != nil {
		return PricingDecision{}, err
	}
	out := PricingDecision{
		Version: pricingDecisionVersion, PolicyRevision: pricingDecisionPolicyRevision,
		ExecutionMode: computeExecutionExactReuse,
		Currency:      catalogue.SettlementCurrency, Tier: tier,
		WorkloadDecisionSHA256: workloadSHA, ComputePlanSHA256: computeSHA,
		OriginPricingDecisionSHA256: originPricingSHA,
		Catalogue:                   catalogue,
		BillableUnits:               pricingBillableUnitsForComputePlan(compute),
		BuyerPrice:                  buyerCharge, MaximumBuyerPrice: buyerCharge,
		PrimarySupplierCost: notApplicableCost(
			"no physical supplier executes an exact-result reuse delivery"),
		VerificationCost: notApplicableCost(
			"the delivered object reuses an already verified exact result"),
		PaymentCost: unknownCost(
			"actual processor allocation is reconciled after collection"),
		ControlPlaneCost: unknownCost(
			"reuse lookup and materialization are not independently metered"),
		StorageCost: unknownCost(
			"cached-object storage is not independently attributed"),
		EgressCost: unknownCost(
			"result egress is not known at acceptance"),
		ProviderCost: notApplicableCost(
			"no external compute provider executes the reused workload"),
		RiskReserve: unknownCost(
			"no independently calibrated loss reserve is available"),
		PlatformContribution: modeledCost(buyerCharge,
			"gross reuse price before unknown payment, storage, egress and control costs"),
		Confidence: 1,
		Assumptions: []string{
			"exact identity includes the complete frozen workload decision",
			"physical supplier work and verification fan-out are zero for this delivery",
		},
		Unknowns: []string{
			"payment cost", "control-plane cost", "storage cost", "egress cost", "risk reserve",
		},
	}
	return out, validatePricingCostShape(out)
}

// admissionEntitlementRefusal is the entitlement check, in EXACT per-task nanos.
//
// Named and separate because it is the single condition four successive money
// defects all landed on, and a negative control has to be able to address it
// directly rather than through a submit that needs a database, a catalogue and a
// seeded honeypot to reach it.
//
// It used to compare an hourly gross RECONSTRUCTED from a rounded per-task payout
// against a ceiling derived in continuous dollars, and refused every job small
// enough for one lost micro-USD to matter — 0.102978 against 0.104733, a 1.676%
// gap that was entirely the rounding step.
//
// Both sides are now the same expression over the same units in the same currency
// (see exactTaskEconomics), so this is an identity rather than a tolerance.
// Comparing the two ROUNDED numbers would also have passed, and would have left
// the supplier short of the continuous floor by that same fraction — which is why
// the obvious fix was the wrong one.
//
// A plan with no exact fields is legacy and keeps the old comparison. Re-deciding
// a historical plan under new arithmetic is the one thing the rounding-policy
// revision exists to prevent.
func admissionEntitlementRefusal(p PricingDecision) error {
	switch {
	case p.SupplierEntitlementPolicy == economicRoundingPolicy:
		// A floor of zero is not a permissive floor, it is an absent one, and every
		// entitlement clears it. Claiming the exact policy while carrying no floor
		// would turn this check off entirely while still reporting that it ran.
		if p.SupplierRequiredNanos <= 0 {
			return fmt.Errorf(
				"pricing decision claims the exact entitlement policy %q but carries a "+
					"supplier floor of %d nanos, which admits any entitlement at all",
				economicRoundingPolicy, p.SupplierRequiredNanos)
		}
		if p.SupplierGrossNanos < p.SupplierRequiredNanos {
			return fmt.Errorf(
				"exact supplier entitlement %d nanos per task is below the exact floor "+
					"%d nanos the accepted rate requires, a shortfall of %d nanos",
				p.SupplierGrossNanos, p.SupplierRequiredNanos,
				p.SupplierRequiredNanos-p.SupplierGrossNanos)
		}
	case p.ExpectedSupplierGrossUSDHr+0.000001 < p.SupplierAdmissionCeilingUSDHr:
		return fmt.Errorf(
			"modeled supplier gross %.6f USD/hr is below the admission ceiling %.6f "+
				"USD/hr, so a worker admitted at that ceiling could not earn it "+
				"(legacy plan: no exact entitlement was frozen)",
			p.ExpectedSupplierGrossUSDHr, p.SupplierAdmissionCeilingUSDHr)
	}
	return nil
}

func validatePricingCostShape(p PricingDecision) error {
	if p.Version != pricingDecisionVersion || p.PolicyRevision != pricingDecisionPolicyRevision {
		return errors.New("unsupported pricing decision")
	}
	currency, err := ParseCurrency(p.Currency)
	if err != nil || currency.Code() != p.Currency {
		return errors.New("pricing decision currency is unsupported")
	}
	if err := validateFixedPointPricing(p); err != nil {
		return err
	}
	if !finiteNonNegative(p.BuyerPrice) || p.BuyerPrice <= 0 ||
		!finiteNonNegative(p.MaximumBuyerPrice) ||
		p.MaximumBuyerPrice < p.BuyerPrice ||
		!finiteNonNegative(p.Confidence) || p.Confidence > 1 {
		return errors.New("pricing decision has invalid price or confidence")
	}
	components := []PricingCostComponent{
		p.PrimarySupplierCost, p.VerificationCost, p.PaymentCost,
		p.ControlPlaneCost, p.StorageCost, p.EgressCost, p.ProviderCost,
		p.RiskReserve, p.PlatformContribution,
	}
	for _, component := range components {
		if component.Status != pricingCostModeled &&
			component.Status != pricingCostNotApplicable &&
			component.Status != pricingCostUnknown {
			return errors.New("pricing cost component has invalid status")
		}
		if !finiteNonNegative(component.Amount) || strings.TrimSpace(component.Basis) == "" {
			return errors.New("pricing cost component lacks a finite amount or basis")
		}
		if component.Status != pricingCostModeled && component.Amount != 0 {
			return errors.New("unknown or not-applicable pricing cost cannot carry a modeled amount")
		}
	}
	switch p.ExecutionMode {
	case computeExecutionDistributed:
		if p.Realtime != nil || p.RealtimeReuse != nil || p.Currency != p.Catalogue.SettlementCurrency ||
			!validSHA256(p.WorkloadDecisionSHA256) || !validSHA256(p.ComputePlanSHA256) {
			return errors.New("physical batch pricing decision lacks core digest or currency authority")
		}
		// Named, not collapsed. Six conditions behind one sentence meant a submit
		// could 409 with "lacks executable composite authority" and no way to tell
		// whether the throughput was missing, the ceiling was zero or the modeled
		// gross had fallen under it — three different faults with three different
		// owners. The reasons are gathered rather than short-circuited so one
		// refusal reports every failing condition.
		var missing []string
		if !validSHA256(p.PlacementRequirementSHA256) {
			missing = append(missing, "placement requirement digest is not a sha256")
		}
		if !validSHA256(p.EconomicPlanSHA256) {
			missing = append(missing, "economic plan digest is not a sha256")
		}
		if !validSHA256(p.EconomicScheduleSHA256) {
			missing = append(missing, "economic schedule digest is not a sha256")
		}
		if p.ExpectedSupplierUnitsPerSec <= 0 {
			missing = append(missing, fmt.Sprintf(
				"modeled supplier throughput is %.6f units/sec, so no rate can be derived",
				p.ExpectedSupplierUnitsPerSec))
		}
		if p.ExpectedSupplierSeconds <= 0 {
			missing = append(missing, fmt.Sprintf(
				"modeled supplier time is %.6f seconds", p.ExpectedSupplierSeconds))
		}
		if p.SupplierAdmissionCeilingUSDHr <= 0 {
			missing = append(missing, fmt.Sprintf(
				"supplier admission ceiling is %.6f USD/hr", p.SupplierAdmissionCeilingUSDHr))
		}
		if err := admissionEntitlementRefusal(p); err != nil {
			missing = append(missing, err.Error())
		}
		if len(missing) > 0 {
			return fmt.Errorf("physical pricing decision lacks executable composite authority: %s",
				strings.Join(missing, "; "))
		}
		conserved := roundEconomicUSD(
			p.PrimarySupplierCost.Amount + p.VerificationCost.Amount +
				p.PaymentCost.Amount + p.ControlPlaneCost.Amount +
				p.StorageCost.Amount + p.EgressCost.Amount +
				p.ProviderCost.Amount + p.RiskReserve.Amount +
				p.PlatformContribution.Amount,
		)
		if math.Abs(conserved-p.BuyerPrice) > 0.000002 {
			return errors.New("physical pricing decision does not conserve modeled buyer price")
		}
		// Cost schedule, when present, must be a valid SHA-256 and match a
		// decision that has left storage/egress/provider/risk as unknown only
		// for honest reasons (unknown status with a basis), never as a silent
		// zero.
		if p.CostScheduleSHA256 != "" && !validSHA256(p.CostScheduleSHA256) {
			return errors.New("physical pricing decision cost schedule digest is not a sha256")
		}
	case computeExecutionExactReuse:
		if p.Realtime != nil || p.RealtimeReuse != nil || p.Currency != p.Catalogue.SettlementCurrency ||
			!validSHA256(p.WorkloadDecisionSHA256) || !validSHA256(p.ComputePlanSHA256) {
			return errors.New("exact-reuse pricing decision lacks core digest or currency authority")
		}
		if p.PlacementRequirementSHA256 != "" ||
			p.EconomicPlanSHA256 != "" ||
			p.EconomicScheduleSHA256 != "" ||
			p.ExpectedSupplierUnitsPerSec != 0 ||
			p.ExpectedSupplierSeconds != 0 ||
			p.SupplierAdmissionCeilingUSDHr != 0 ||
			p.ExpectedSupplierGrossUSDHr != 0 ||
			p.PrimarySupplierCost.Status != pricingCostNotApplicable ||
			p.VerificationCost.Status != pricingCostNotApplicable {
			return errors.New("exact-reuse pricing falsely attributes physical execution")
		}
	case pricingExecutionRealtime:
		if p.Realtime == nil || p.RealtimeReuse != nil || p.ServiceLease != nil {
			return errors.New("realtime pricing decision lacks realtime authority")
		}
		if p.Catalogue != (CataloguePriceAuthority{}) || p.WorkloadDecisionSHA256 != "" ||
			p.ComputePlanSHA256 != "" || p.PlacementRequirementSHA256 != "" ||
			p.EconomicPlanSHA256 != "" || p.EconomicScheduleSHA256 != "" ||
			p.SupplierGrossNanos != 0 || p.SupplierRequiredNanos != 0 ||
			p.SupplierEntitlementPolicy != "" {
			return errors.New("realtime pricing decision carries unrelated batch authority")
		}
		if err := validateRealtimePricingAuthority(*p.Realtime, p.Currency); err != nil {
			return err
		}
		conserved := p.PrimarySupplierCost.Amount + p.PlatformContribution.Amount
		if math.Abs(conserved-p.BuyerPrice) > 0.000000002 {
			return errors.New("realtime pricing decision does not conserve modeled buyer price")
		}
	case pricingExecutionRealtimeReuse:
		if p.Realtime != nil || p.RealtimeReuse == nil || p.ServiceLease != nil {
			return errors.New("realtime reuse pricing decision lacks reuse authority")
		}
		if p.Catalogue != (CataloguePriceAuthority{}) || p.WorkloadDecisionSHA256 != "" ||
			p.ComputePlanSHA256 != "" || p.PlacementRequirementSHA256 != "" ||
			p.EconomicPlanSHA256 != "" || p.EconomicScheduleSHA256 != "" ||
			p.SupplierGrossNanos != 0 || p.SupplierRequiredNanos != 0 ||
			p.SupplierEntitlementPolicy != "" || p.PrimarySupplierCost.Status != pricingCostNotApplicable ||
			p.VerificationCost.Status != pricingCostNotApplicable {
			return errors.New("realtime reuse pricing falsely attributes physical execution")
		}
		if err := validateRealtimeReusePricingAuthority(*p.RealtimeReuse, p.Currency); err != nil {
			return err
		}
		if math.Abs(p.PlatformContribution.Amount-p.BuyerPrice) > 0.000000002 {
			return errors.New("realtime reuse pricing does not conserve gross buyer price")
		}
	case pricingExecutionServiceLease:
		if p.Realtime != nil || p.RealtimeReuse != nil || p.ServiceLease == nil {
			return errors.New("service lease pricing decision lacks service lease authority")
		}
		if p.Catalogue != (CataloguePriceAuthority{}) || p.WorkloadDecisionSHA256 != "" ||
			p.ComputePlanSHA256 != "" || p.PlacementRequirementSHA256 != "" ||
			p.EconomicPlanSHA256 != "" || p.EconomicScheduleSHA256 != "" ||
			p.SupplierGrossNanos != 0 || p.SupplierRequiredNanos != 0 ||
			p.SupplierEntitlementPolicy != "" || p.VerificationCost.Status != pricingCostNotApplicable ||
			p.StorageCost.Status != pricingCostNotApplicable || p.EgressCost.Status != pricingCostNotApplicable {
			return errors.New("service lease pricing decision carries unrelated execution authority")
		}
		if err := validateServiceLeasePricingAuthority(*p.ServiceLease, p.Currency); err != nil {
			return err
		}
		if p.FixedPoint == nil {
			return errors.New("service lease pricing decision lacks exact fixed-point authority")
		}
		conserved := p.PrimarySupplierCost.Amount + p.ControlPlaneCost.Amount +
			p.ProviderCost.Amount + p.RiskReserve.Amount + p.PlatformContribution.Amount
		if math.Abs(conserved-p.BuyerPrice) > 0.000000002 {
			return errors.New("service lease pricing decision does not conserve modeled buyer price")
		}
	default:
		return fmt.Errorf("unknown pricing execution mode %q", p.ExecutionMode)
	}
	return nil
}

// ValidateDistributedPricingDecisionSnapshot rebuilds a stored decision from
// authority. Like ValidateFrozenComputePlanSnapshot it re-derives its inputs
// rather than reading them off the record being checked.
//
// The supplier unit rate is the one input that cannot simply be recomputed at
// the current instant, so it is re-resolved as a SET: every rate the governed
// runtime-cell authority can produce for this workload's frozen candidate cells.
// Handing decision.ExpectedSupplierUnitsPerSec straight back into the rebuild
// made this validator self-certifying - the rebuild derived the ceiling from the
// record's own rate and compared it to the record's own placement, so a decision
// whose rate and placement were altered together rebuilt to itself and passed at
// any rate an attacker liked.
//
// Catalogue fields still come from decision.Catalogue here. That is also
// self-certifying when the attacker rewrites pricing, placement and economic
// plan together. Production paths that hold a store must use
// ValidateDistributedPricingDecisionSnapshotWithStore, which re-resolves the
// catalogue from append-only schedule tables first.
func ValidateDistributedPricingDecisionSnapshot(
	decision PricingDecision,
	workload WorkloadDecision,
	compute ComputePlan,
	placement PlacementRequirement,
	economic EconomicPlan,
) error {
	governed, err := governedAdmissionUnitRates(
		workload.RuntimeJobType, workload.Binding.Model.Ref,
		admissionCellsForWorkload(workload), time.Now(),
	)
	if err != nil {
		return err
	}
	if !rateIsGoverned(governed, decision.ExpectedSupplierUnitsPerSec) {
		return fmt.Errorf(
			"pricing decision claims %g supplier units/s, which no governed runtime-cell "+
				"benchmark produces for job %q on model %q (admissible: %v)",
			decision.ExpectedSupplierUnitsPerSec, workload.RuntimeJobType,
			workload.Binding.Model.Ref, governed,
		)
	}
	rebuilt, err := distributedPricingDecisionAtRate(
		workload, compute, placement, economic, decision.Catalogue, decision.Tier,
		decision.OriginQuotePricingDecisionSHA256, decision.ExpectedSupplierUnitsPerSec,
	)
	if err != nil {
		return err
	}
	if !reflect.DeepEqual(decision, rebuilt) {
		return errors.New("pricing decision does not match its deterministic composite authority")
	}
	return nil
}

// ValidateDistributedPricingDecisionSnapshotWithStore is the production
// validator for distributed pricing. It resolves the catalogue from append-only
// schedule/history tables by the decision's schedule digest (not the mutable
// models pointer), requires every catalogue field to match, then runs the pure
// composite rebuild. Callers without a database must not silently fall back to
// the pure check; they either use this with a store or report that catalogue
// authority cannot be store-anchored.
func ValidateDistributedPricingDecisionSnapshotWithStore(
	ctx context.Context,
	store *Store,
	decision PricingDecision,
	workload WorkloadDecision,
	compute ComputePlan,
	placement PlacementRequirement,
	economic EconomicPlan,
) error {
	if store == nil {
		return errors.New("catalogue-anchored pricing validation requires a store; pure snapshot check is not a silent substitute")
	}
	if ctx == nil {
		return errors.New("catalogue-anchored pricing validation requires a context")
	}
	authority, err := store.LoadCataloguePriceAuthorityAtSchedule(
		ctx,
		decision.Catalogue.ScheduleSHA256,
		decision.Catalogue.ScheduleVersion,
		decision.Catalogue.ModelID,
		decision.Catalogue.JobType,
	)
	if err != nil {
		return err
	}
	if err := cataloguePriceAuthorityFieldMismatch(decision.Catalogue, authority); err != nil {
		return err
	}
	return ValidateDistributedPricingDecisionSnapshot(
		decision, workload, compute, placement, economic,
	)
}

// rateIsGoverned admits a last-bit difference and nothing wider. The stored
// rate and the rate recomputed from the manifest are the same product of the
// same two numbers, so they agree to the last bits; a relative epsilon is used
// rather than a fixed one because these rates run from 1 to several thousand
// units/s and a fixed epsilon means something different at each end.
func rateIsGoverned(governed []float64, rate float64) bool {
	for _, want := range governed {
		tolerance := math.Abs(want) * 0.000001
		if tolerance < 0.000001 {
			tolerance = 0.000001
		}
		if math.Abs(rate-want) <= tolerance {
			return true
		}
	}
	return false
}

func ValidateExactReusePricingDecisionSnapshot(
	decision PricingDecision,
	workload WorkloadDecision,
	compute ComputePlan,
) error {
	rebuilt, err := newExactReusePricingDecision(
		workload, compute, decision.Catalogue, decision.Tier,
		decision.BuyerPrice, decision.OriginPricingDecisionSHA256,
	)
	if err != nil {
		return err
	}
	if !reflect.DeepEqual(decision, rebuilt) {
		return errors.New("exact-reuse pricing decision does not match its deterministic authority")
	}
	return nil
}
