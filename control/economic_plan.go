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
	Version                string  `json:"version"`
	ProcessorPercent       float64 `json:"processor_percent"`
	ProcessorFixedUSD      float64 `json:"processor_fixed_usd"`
	ControlPlanePerTaskUSD float64 `json:"control_plane_per_task_usd"`
	TargetMarginRate       float64 `json:"target_margin_rate"`
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
	economicScheduleVersionEnv = "CX_ECON_SCHEDULE_VERSION"
	processorPercentBPSEnv     = "CX_PROCESSOR_PERCENT_BPS"
	processorFixedUSDEnv       = "CX_PROCESSOR_FIXED_USD"
	controlPerTaskUSDEnv       = "CX_CONTROL_PLANE_PER_TASK_USD"
	targetMarginBPSEnv         = "CX_TARGET_MARGIN_BPS"
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
		Version:                version,
		ProcessorPercent:       processorBPS / 10_000,
		ProcessorFixedUSD:      fixed,
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
	if s.ProcessorPercent+s.TargetMarginRate >= 1 {
		return "processor_percent plus target_margin_rate must be below 1"
	}
	return ""
}

func blockedEconomicPlan(in EconomicPlanInput, schedule EconomicSchedule, reason string) EconomicPlan {
	return EconomicPlan{
		Version: economicPlanVersion, Schedule: schedule, Input: in,
		Executable: false, BlockReason: reason, MinimumMarginHeadroomUSD: -1,
		SupplierSettlementPolicy: supplierSettlementPolicyFloorCentCarryV1,
		Assumptions: []string{
			"quote-derived settlement is revenue, never independent execution cost",
			"actual processor fees are reconciled after collection",
		},
	}
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

	computePerTask := in.BaseComputeUSD / float64(in.InitialTaskCount)
	supplierPerTask := roundEconomicUSD(computePerTask * in.SupplierShare)
	denominator := 1 - schedule.ProcessorPercent - schedule.TargetMarginRate
	minimumBuyerPerTask := (supplierPerTask + schedule.ProcessorFixedUSD + schedule.ControlPlanePerTaskUSD) / denominator
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
			"supplier payout is frozen from base compute, independent of buyer safety fee and refundable SLA premium",
			"supplier liability is reserved at six decimals; provider cash floors to whole cents and every sub-cent remainder stays durably owed",
			"one accepted task must cover a standalone processor fixed fee",
			"extra accepted work is billable only while atomically consuming the frozen reserve",
			"SLA premium is excluded from supplier liability and may be fully refunded",
			"actual processor fees and contribution margin are reconciled from Stripe and ledger facts",
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
		processor := 0.0
		if net > 0 {
			processor = ceilEconomicUSD(net*schedule.ProcessorPercent + schedule.ProcessorFixedUSD)
		}
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
