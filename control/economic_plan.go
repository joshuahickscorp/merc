package main

import (
	"errors"
	"fmt"
	"math"
	"os"
	"reflect"
	"strconv"
	"strings"
)

type EconomicSchedule struct {
	Version string `json:"version"`
	// Currency is the ISO settlement currency for every major-unit amount in
	// this schedule and the plan derived from it. Historical field names retain
	// their _usd suffix, but that suffix never overrides this authority.
	Currency          string  `json:"currency"`
	ProcessorPercent  float64 `json:"processor_percent"`
	ProcessorFixedUSD float64 `json:"processor_fixed_usd"`
	// MinChargeBatchUSD is the smallest amount the collector will ever put on a
	// single PaymentIntent.  The processor's fixed fee is charged once per that
	// event, not once per task, so it amortises across everything in the batch.
	// Sourced from chargeMinUSD() (control/collect.go) so the pricing model and
	// the collector cannot drift apart.
	MinChargeBatchUSD      float64 `json:"min_charge_batch_usd"`
	ControlPlanePerTaskUSD float64 `json:"control_plane_per_task_usd"`
	TargetMarginRate       float64 `json:"target_margin_rate"`
}

// processorFloorTerms splits the processor's cost into the percentage rate and
// the per-task fixed component that the price floor has to cover.
//
// With a batching floor configured the fixed fee is spread across a
// minimum-size charge, so it becomes part of the rate and nothing is charged
// per task.  Without one, every task is assumed to trigger its own charge and
// must cover a whole fixed fee -- the original, conservative behaviour.
//
// processorFeeFor below is the same model read forwards, and the two must stay
// in agreement: solving the floor under one assumption while scoring scenarios
// under another is what let this file hold a $0.344547 per-task floor while its
// own scenarios charged the fixed fee once.
func (s EconomicSchedule) processorFloorTerms() (rate, perTaskFixed float64) {
	if s.MinChargeBatchUSD <= 0 {
		return s.ProcessorPercent, s.ProcessorFixedUSD
	}
	return s.ProcessorPercent + s.ProcessorFixedUSD/s.MinChargeBatchUSD, 0
}

// processorFeeFor is the fee a charge of netUSD actually incurs.  A charge at or
// above the batch floor stands alone and pays the whole fixed fee; a smaller one
// rides along with other jobs in the same PaymentIntent and pays its share.
func (s EconomicSchedule) processorFeeFor(netUSD float64) float64 {
	if netUSD <= 0 {
		return 0
	}
	fixedShare := 1.0
	if s.MinChargeBatchUSD > 0 && netUSD < s.MinChargeBatchUSD {
		fixedShare = netUSD / s.MinChargeBatchUSD
	}
	return ceilEconomicUSD(netUSD*s.ProcessorPercent + s.ProcessorFixedUSD*fixedShare)
}

type EconomicPlanInput struct {
	BaseComputeUSD   float64 `json:"base_compute_usd"`
	InitialTaskCount int     `json:"initial_task_count"`
	ExtraTaskReserve int     `json:"extra_task_reserve"`
	SupplierShare    float64 `json:"supplier_share"`
	SLAPremiumUSD    float64 `json:"sla_premium_usd"`
	FirmQuoteMaxUSD  float64 `json:"firm_quote_max_usd,omitempty"`
}

type EconomicScenario struct {
	Name                  string  `json:"name"`
	AcceptedTasks         int     `json:"accepted_tasks"`
	GrossChargeUSD        float64 `json:"gross_charge_usd"`
	RefundUSD             float64 `json:"refund_usd"`
	NetBilledUSD          float64 `json:"net_billed_usd"`
	SupplierLiabilityUSD  float64 `json:"supplier_liability_usd"`
	ProcessorFeeUSD       float64 `json:"processor_fee_usd"`
	ControlPlaneCostUSD   float64 `json:"control_plane_cost_usd"`
	ContributionMarginUSD float64 `json:"contribution_margin_usd"`
	RequiredMarginUSD     float64 `json:"required_margin_usd"`
	MarginHeadroomUSD     float64 `json:"margin_headroom_usd"`
}

type EconomicPlan struct {
	Version                  int                `json:"version"`
	Schedule                 EconomicSchedule   `json:"schedule"`
	Input                    EconomicPlanInput  `json:"input"`
	Executable               bool               `json:"executable"`
	BlockReason              string             `json:"block_reason,omitempty"`
	BaseComputePerTaskUSD    float64            `json:"base_compute_per_task_usd"`
	BuyerChargePerTaskUSD    float64            `json:"buyer_charge_per_task_usd"`
	SupplierPayoutPerTaskUSD float64            `json:"supplier_payout_per_task_usd"`
	SupplierSettlementPolicy string             `json:"supplier_settlement_policy"`
	BuyerSafetyFeePerTaskUSD float64            `json:"buyer_safety_fee_per_task_usd"`
	InitialBuyerChargeUSD    float64            `json:"initial_buyer_charge_usd"`
	ReservedBuyerChargeUSD   float64            `json:"reserved_buyer_charge_usd"`
	MinimumScenario          string             `json:"minimum_scenario,omitempty"`
	MinimumMarginHeadroomUSD float64            `json:"minimum_margin_headroom_usd"`
	Scenarios                []EconomicScenario `json:"scenarios"`
	Assumptions              []string           `json:"assumptions"`
}

const economicPlanVersion = 2

func economicExtraTaskReserve(primaryTasks int) int {
	if primaryTasks <= 0 {
		return 0
	}
	return primaryTasks
}

const (
	economicScheduleVersionEnv = "MERC_ECON_SCHEDULE_VERSION"
	processorPercentBPSEnv     = "MERC_PROCESSOR_PERCENT_BPS"
	processorFixedUSDEnv       = "MERC_PROCESSOR_FIXED_USD"
	controlPerTaskUSDEnv       = "MERC_CONTROL_PLANE_PER_TASK_USD"
	targetMarginBPSEnv         = "MERC_TARGET_MARGIN_BPS"
)

func LoadEconomicScheduleFromEnv() (EconomicSchedule, error) {
	version := strings.TrimSpace(os.Getenv(economicScheduleVersionEnv))
	if version == "" {
		return EconomicSchedule{}, fmt.Errorf("%s is required", economicScheduleVersionEnv)
	}
	parseRequired := func(name string) (float64, error) {
		raw := strings.TrimSpace(os.Getenv(name))
		if raw == "" {
			return 0, fmt.Errorf("%s is required", name)
		}
		value, err := strconv.ParseFloat(raw, 64)
		if err != nil || !finiteNonNegative(value) {
			return 0, fmt.Errorf("%s must be a finite non-negative number", name)
		}
		return value, nil
	}
	processorBPS, err := parseRequired(processorPercentBPSEnv)
	if err != nil {
		return EconomicSchedule{}, err
	}
	fixed, err := parseRequired(processorFixedUSDEnv)
	if err != nil {
		return EconomicSchedule{}, err
	}
	controlPerTask, err := parseRequired(controlPerTaskUSDEnv)
	if err != nil {
		return EconomicSchedule{}, err
	}
	marginBPS, err := parseRequired(targetMarginBPSEnv)
	if err != nil {
		return EconomicSchedule{}, err
	}
	schedule := EconomicSchedule{
		Version:           version,
		Currency:          SettlementCurrencyCode(),
		ProcessorPercent:  processorBPS / 10_000,
		ProcessorFixedUSD: fixed,
		// Read from the collector rather than its own env var: the batch floor
		// and the price floor describe the same event, and two settings that
		// must agree will eventually disagree.
		MinChargeBatchUSD:      chargeMinUSD(),
		ControlPlanePerTaskUSD: controlPerTask,
		TargetMarginRate:       marginBPS / 10_000,
	}
	if reason := validateEconomicSchedule(schedule); reason != "" {
		return EconomicSchedule{}, fmt.Errorf("invalid economic schedule: %s", reason)
	}
	return schedule, nil
}

func finiteNonNegative(v float64) bool {
	return !math.IsNaN(v) && !math.IsInf(v, 0) && v >= 0
}

func ceilEconomicUSD(v float64) float64 {
	return math.Ceil((v-1e-12)*1_000_000) / 1_000_000
}

func minEconomic(a, b float64) float64 {
	if a < b {
		return a
	}
	return b
}

func validateEconomicSchedule(s EconomicSchedule) string {
	if s.Version == "" {
		return "economic schedule version is required"
	}
	currency, err := ParseCurrency(s.Currency)
	if err != nil || s.Currency != currency.Code() {
		return "economic schedule currency must be a supported ISO settlement currency"
	}
	if !finiteNonNegative(s.ProcessorPercent) || s.ProcessorPercent >= 1 {
		return "processor_percent must be finite and in [0,1)"
	}
	if !finiteNonNegative(s.ProcessorFixedUSD) {
		return "processor_fixed_usd must be finite and non-negative"
	}
	if !finiteNonNegative(s.ControlPlanePerTaskUSD) {
		return "control_plane_per_task_usd must be finite and non-negative"
	}
	if !finiteNonNegative(s.TargetMarginRate) || s.TargetMarginRate >= 1 {
		return "target_margin_rate must be finite and in [0,1)"
	}
	if !finiteNonNegative(s.MinChargeBatchUSD) {
		return "min_charge_batch_usd must be finite and non-negative"
	}
	if rate, _ := s.processorFloorTerms(); rate+s.TargetMarginRate >= 1 {
		return "effective processor rate plus target_margin_rate must be below 1"
	}
	return ""
}

func blockedEconomicPlan(in EconomicPlanInput, schedule EconomicSchedule, reason string) EconomicPlan {
	return EconomicPlan{
		Version: economicPlanVersion, Schedule: schedule, Input: in,
		Executable: false, BlockReason: reason, MinimumMarginHeadroomUSD: -1,
		SupplierSettlementPolicy: supplierSettlementPolicyFloorCentCarryV1,
		Assumptions: []string{
			"every major-unit amount is denominated in schedule.currency; legacy _usd field names do not override that authority",
			"quote-derived settlement is revenue, never independent execution cost",
			"actual processor fees are reconciled after collection",
		},
	}
}

// minBillableSupplierMicros is the smallest non-zero liability the ledger can
// represent. A supplier credit of 0 is not "carried for later" — nothing is
// accrued — so work performed against a $0 reservation is unpaid forever.
const minBillableSupplierMicros int64 = 1

// minBillableBaseComputeMicros is the smallest total base_compute (micro-USD)
// such that after dividing across tasks and applying supplier share, each
// task's supplier payout rounds to at least one micro-USD.
//
// Derived by search, not hardcoded — same pattern as minLoRAQuoteMicros. A
// change to SupplierShare cannot silently reintroduce a zero-payout window.
func minBillableBaseComputeMicros(share float64, tasks int) int64 {
	if tasks < 1 || share <= 0 || share > 1 || math.IsNaN(share) || math.IsInf(share, 0) {
		return minBillableSupplierMicros
	}
	for total := int64(1); total < 10_000_000; total++ {
		computePerTask := microsToUSD(total) / float64(tasks)
		supplier := roundEconomicUSD(computePerTask * share)
		if usdToMicros(supplier) >= minBillableSupplierMicros {
			return total
		}
	}
	panic("no base compute can pay the supplier; share constants are inconsistent")
}

func BuildEconomicPlan(in EconomicPlanInput, schedule EconomicSchedule) EconomicPlan {
	if reason := validateEconomicSchedule(schedule); reason != "" {
		return blockedEconomicPlan(in, schedule, reason)
	}
	if !finiteNonNegative(in.BaseComputeUSD) || in.BaseComputeUSD <= 0 {
		return blockedEconomicPlan(in, schedule, "base_compute_usd must be finite and positive")
	}
	if in.InitialTaskCount <= 0 {
		return blockedEconomicPlan(in, schedule, "initial_task_count must be positive")
	}
	if in.ExtraTaskReserve < 0 {
		return blockedEconomicPlan(in, schedule, "extra_task_reserve must be non-negative")
	}
	if !finiteNonNegative(in.SupplierShare) || in.SupplierShare <= 0 || in.SupplierShare > 1 {
		return blockedEconomicPlan(in, schedule, "supplier_share must be finite and in (0,1]")
	}
	if !finiteNonNegative(in.SLAPremiumUSD) || !finiteNonNegative(in.FirmQuoteMaxUSD) {
		return blockedEconomicPlan(in, schedule, "SLA premium and firm quote max must be finite and non-negative")
	}

	// Minimum billable job size: raise base compute when the supplier share would
	// otherwise round to zero while the buyer is still charged the control-plane
	// floor. Same failure class as minLoRAQuoteMicros — derived by search so a
	// share change cannot reintroduce a $0 supplier liability for real work.
	baseComputeUSD := in.BaseComputeUSD
	if minTotal := minBillableBaseComputeMicros(in.SupplierShare, in.InitialTaskCount); usdToMicros(baseComputeUSD) < minTotal {
		baseComputeUSD = microsToUSD(minTotal)
	}
	// Rebuild input so ValidateEconomicPlanSnapshot still round-trips: the
	// floored base is what the plan actually prices, so it is what we freeze.
	in = EconomicPlanInput{
		BaseComputeUSD:   baseComputeUSD,
		InitialTaskCount: in.InitialTaskCount,
		ExtraTaskReserve: in.ExtraTaskReserve,
		SupplierShare:    in.SupplierShare,
		SLAPremiumUSD:    in.SLAPremiumUSD,
		FirmQuoteMaxUSD:  in.FirmQuoteMaxUSD,
	}

	computePerTask := baseComputeUSD / float64(in.InitialTaskCount)
	supplierPerTask := roundEconomicUSD(computePerTask * in.SupplierShare)
	if usdToMicros(supplierPerTask) < minBillableSupplierMicros {
		// Defensive: the search above is the authority; this branch only fires
		// if share arithmetic and roundEconomicUSD disagree on the boundary.
		supplierPerTask = microsToUSD(minBillableSupplierMicros)
	}
	processorRate, processorPerTaskFixed := schedule.processorFloorTerms()
	denominator := 1 - processorRate - schedule.TargetMarginRate
	minimumBuyerPerTask := (supplierPerTask + processorPerTaskFixed + schedule.ControlPlanePerTaskUSD) / denominator
	buyerPerTask := ceilEconomicUSD(math.Max(computePerTask, minimumBuyerPerTask))
	safetyFee := roundEconomicUSD(math.Max(0, buyerPerTask-computePerTask))

	plan := EconomicPlan{
		Version: economicPlanVersion, Schedule: schedule, Input: in,
		BaseComputePerTaskUSD:    computePerTask,
		BuyerChargePerTaskUSD:    buyerPerTask,
		SupplierPayoutPerTaskUSD: supplierPerTask,
		SupplierSettlementPolicy: supplierSettlementPolicyFloorCentCarryV1,
		BuyerSafetyFeePerTaskUSD: safetyFee,
		InitialBuyerChargeUSD:    roundEconomicUSD(buyerPerTask*float64(in.InitialTaskCount) + in.SLAPremiumUSD),
		ReservedBuyerChargeUSD:   roundEconomicUSD(buyerPerTask*float64(in.InitialTaskCount+in.ExtraTaskReserve) + in.SLAPremiumUSD),
		MinimumMarginHeadroomUSD: math.Inf(1),
		Assumptions: []string{
			"every major-unit amount is denominated in schedule.currency; legacy _usd field names do not override that authority",
			"supplier payout is frozen from base compute, independent of buyer safety fee and refundable SLA premium",
			"supplier liability is reserved at six decimals; provider cash floors to whole cents and every sub-cent remainder stays durably owed",
			"the processor fixed fee is amortised over a minimum-size charge batch, matching how chargeOrDeferJob and FormChargeBatch actually settle",
			"extra accepted work is billable only while atomically consuming the frozen reserve",
			"SLA premium is excluded from supplier liability and may be fully refunded",
			"actual processor fees and contribution margin are reconciled from Stripe and ledger facts",
			"base compute is floored to the minimum billable size so a supplier who performed work is never reserved $0 while the buyer is charged",
		},
	}

	addScenario := func(name string, tasks int, slaMiss bool) {
		gross := buyerPerTask*float64(tasks) + in.SLAPremiumUSD
		if in.FirmQuoteMaxUSD > 0 {
			gross = minEconomic(gross, in.FirmQuoteMaxUSD)
		}
		gross = roundEconomicUSD(gross)
		refund := 0.0
		if slaMiss {
			refund = roundEconomicUSD(minEconomic(in.SLAPremiumUSD, gross))
		}
		net := roundEconomicUSD(gross - refund)
		supplier := roundEconomicUSD(supplierPerTask * float64(tasks))
		processor := schedule.processorFeeFor(net)
		controlCost := roundEconomicUSD(schedule.ControlPlanePerTaskUSD * float64(tasks))
		margin := roundEconomicUSD(net - supplier - processor - controlCost)
		required := roundEconomicUSD(net * schedule.TargetMarginRate)
		headroom := roundEconomicUSD(margin - required)
		s := EconomicScenario{
			Name: name, AcceptedTasks: tasks, GrossChargeUSD: gross, RefundUSD: refund,
			NetBilledUSD: net, SupplierLiabilityUSD: supplier, ProcessorFeeUSD: processor,
			ControlPlaneCostUSD: controlCost, ContributionMarginUSD: margin,
			RequiredMarginUSD: required, MarginHeadroomUSD: headroom,
		}
		plan.Scenarios = append(plan.Scenarios, s)
		if headroom < plan.MinimumMarginHeadroomUSD {
			plan.MinimumMarginHeadroomUSD = headroom
			plan.MinimumScenario = name
		}
	}

	addScenario("one_task_partial", 1, true)
	addScenario("full_success_sla_met", in.InitialTaskCount, false)
	addScenario("full_success_sla_miss", in.InitialTaskCount, true)
	addScenario("max_extra_work_sla_miss", in.InitialTaskCount+in.ExtraTaskReserve, true)

	plan.Executable = plan.MinimumMarginHeadroomUSD >= -0.000001
	if !plan.Executable {
		plan.BlockReason = fmt.Sprintf(
			"modeled scenario %s misses the configured margin floor by $%.6f",
			plan.MinimumScenario, -plan.MinimumMarginHeadroomUSD,
		)
	}
	return plan
}

func ValidateEconomicPlanSnapshot(plan EconomicPlan) error {
	rebuilt := BuildEconomicPlan(plan.Input, plan.Schedule)
	if !reflect.DeepEqual(plan, rebuilt) {
		return errors.New("economic plan snapshot does not match its deterministic input and schedule")
	}
	if !plan.Executable {
		return fmt.Errorf("economic plan is not executable: %s", plan.BlockReason)
	}
	return nil
}

func EconomicPlansEqual(a, b EconomicPlan) bool { return reflect.DeepEqual(a, b) }
