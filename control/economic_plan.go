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
	MinChargeBatchUSD float64 `json:"min_charge_batch_usd"`
	// Historical schedules charged this once for every physical task. It remains
	// in the snapshot so old receipts rebuild byte-for-byte, but new schedules
	// use ControlPlanePerBatchUSD: account/invoice overhead is incurred once per
	// economic collection batch and allocated pro rata, just like the processor's
	// fixed fee. Treating it as a task cost produced the commercially meaningless
	// 11,564-micro buyer / 2-micro supplier split on a two-task embedding job.
	ControlPlanePerTaskUSD       float64 `json:"control_plane_per_task_usd,omitempty"`
	ControlPlanePerBatchUSD      float64 `json:"control_plane_per_batch_usd,omitempty"`
	ControlPlaneAllocationPolicy string  `json:"control_plane_allocation_policy,omitempty"`
	// MinimumContributionUSD prevents a percentage target from rounding to zero
	// on a micro-job. It is the minimum absolute Merc contribution for the full
	// initial economic batch; other scenarios receive the same per-task share.
	MinimumContributionUSD float64 `json:"minimum_contribution_usd,omitempty"`
	TargetMarginRate       float64 `json:"target_margin_rate"`
}

const controlPlaneAllocationChargeBatchV1 = "charge_batch_pro_rata_v1"

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
//
// Pro-rata shares of a batch fee are continuous. Micro-ceiling each share that
// is a fraction of a micro reintroduces the commercially meaningless split that
// batch allocation exists to remove: processor and control each jump to one
// micro, the headroom loop then inflates the buyer charge around those phantom
// floors, and a supplier entitled to ~0.94 of catalogue compute is left with a
// 20% commercial share on the CAD stranger fixture (2 µ of a 10 µ charge). A
// standalone charge still ceils so we never understate a whole PaymentIntent's
// fee; a partial share rounds half-away to the ledger micro, matching how the
// rest of the six-decimal domain treats non-entitlement amounts.
func (s EconomicSchedule) processorFeeFor(netUSD float64) float64 {
	if netUSD <= 0 {
		return 0
	}
	fixedShare := 1.0
	if s.MinChargeBatchUSD > 0 && netUSD < s.MinChargeBatchUSD {
		fixedShare = netUSD / s.MinChargeBatchUSD
	}
	raw := netUSD*s.ProcessorPercent + s.ProcessorFixedUSD*fixedShare
	return roundProRataBatchFeeUSD(raw, fixedShare)
}

// controlPlaneFloorTerms and controlPlaneCostFor are the inverse and forward
// views of one allocation policy. New schedules allocate declared fixed
// account/invoice overhead across the same minimum-size economic batch the
// collector actually forms. An empty policy is a historical per-task schedule.
func (s EconomicSchedule) controlPlaneFloorTerms() (rate, perTaskFixed float64) {
	if s.ControlPlaneAllocationPolicy != controlPlaneAllocationChargeBatchV1 {
		return 0, s.ControlPlanePerTaskUSD
	}
	if s.MinChargeBatchUSD <= 0 {
		return 0, s.ControlPlanePerBatchUSD
	}
	return s.ControlPlanePerBatchUSD / s.MinChargeBatchUSD, 0
}

func (s EconomicSchedule) controlPlaneCostFor(netUSD float64, tasks int) float64 {
	if netUSD <= 0 || tasks <= 0 {
		return 0
	}
	if s.ControlPlaneAllocationPolicy != controlPlaneAllocationChargeBatchV1 {
		return roundEconomicUSD(s.ControlPlanePerTaskUSD * float64(tasks))
	}
	fixedShare := 1.0
	if s.MinChargeBatchUSD > 0 && netUSD < s.MinChargeBatchUSD {
		fixedShare = netUSD / s.MinChargeBatchUSD
	}
	return roundProRataBatchFeeUSD(s.ControlPlanePerBatchUSD*fixedShare, fixedShare)
}

// roundProRataBatchFeeUSD projects a continuous pro-rata batch fee into the
// six-decimal ledger domain. fixedShare < 1 means the job rides a larger
// PaymentIntent; fixedShare == 1 means this charge stands alone and must cover
// a whole fixed fee, so the projection ceils.
func roundProRataBatchFeeUSD(raw, fixedShare float64) float64 {
	if raw <= 0 {
		return 0
	}
	if fixedShare < 1.0 {
		return roundEconomicUSD(raw)
	}
	return ceilEconomicUSD(raw)
}

type EconomicPlanInput struct {
	BaseComputeUSD   float64 `json:"base_compute_usd"`
	InitialTaskCount int     `json:"initial_task_count"`
	ExtraTaskReserve int     `json:"extra_task_reserve"`
	SupplierShare    float64 `json:"supplier_share"`
	SLAPremiumUSD    float64 `json:"sla_premium_usd"`
	FirmQuoteMaxUSD  float64 `json:"firm_quote_max_usd,omitempty"`

	// BaseComputeNanos is the SAME base compute, exact, in nano-major-units of the
	// schedule currency, as the catalogue derived it before anything rounded it.
	//
	// BaseComputeUSD is quantised to micro-USD. On a three-record embed the exact
	// figure is 1,436 nanos and the micro-USD projection of it is 1,000 — a 30.4%
	// haircut, taken before the supplier's share was applied. The supplier was then
	// entitled to 970 nanos against a 1,393-nano floor derived from the unrounded
	// catalogue, and admission refused every job small enough for that to happen.
	//
	// Additive and optional. Zero means a plan whose base compute reached it only
	// as a float — every plan frozen before this field existed, and any caller that
	// still has no exact catalogue figure to offer. Those keep deriving their nanos
	// from the float, which is the migration rule: a historical plan is never
	// re-read under later arithmetic.
	BaseComputeNanos int64 `json:"base_compute_nanos,omitempty"`
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

	// The EXACT half, in integer nano-major-units, alongside the micro-USD fields
	// above rather than replacing them.
	//
	// SupplierPayoutPerTaskUSD is rounded to micro-USD, and admission used to
	// compare it — divided back out into an hourly rate — against a ceiling derived
	// in continuous dollars. One lost micro became a 1.676% hourly shortfall and
	// every small job was refused. These fields are the same entitlement computed
	// without that rounding, so the comparison has two exact sides.
	//
	// Zero with an empty EconomicRoundingPolicy means a LEGACY plan, frozen before
	// this authority existed. Such a plan keeps its own arithmetic and is never
	// re-read under these rules.
	SupplierPayoutPerTaskNanos int64  `json:"supplier_payout_per_task_nanos,omitempty"`
	BuyerChargePerTaskNanos    int64  `json:"buyer_charge_per_task_nanos,omitempty"`
	BaseComputePerTaskNanos    int64  `json:"base_compute_per_task_nanos,omitempty"`
	EconomicRoundingPolicy     string `json:"economic_rounding_policy,omitempty"`
}

// ceilUSDToNanos ceils a major-unit float into integer nano-major-units.
//
// The buyer's charge rounds UP because the contribution floor must hold: a
// fraction of a nano shaved off the charge can leave supplier + processor +
// control + required contribution uncovered. Distortion is bounded by one nano
// per task (not one micro), which is the whole point of moving the ceil out of
// the six-decimal ledger domain.
func ceilUSDToNanos(usd float64) (int64, error) {
	if !finiteNonNegative(usd) || usd <= 0 {
		return 0, fmt.Errorf("cannot ceil non-positive amount %v to nanos", usd)
	}
	scaled := usd * float64(NanosPerMajorUnit)
	if scaled > math.MaxInt64 {
		return 0, errMoneyOverflow
	}
	// Same epsilon style as ceilEconomicUSD: resist float noise just below an
	// integer boundary without ever rounding down a true fractional part.
	return int64(math.Ceil(scaled - 1e-6)), nil
}

// projectNanosToUSD projects exact nanos to the six-decimal ledger float once.
// Half-away-from-zero at the micro boundary, with a one-micro floor for any
// positive amount — matching LedgerMicrosFromNanos.
func projectNanosToUSD(nanos int64) float64 {
	if nanos <= 0 {
		return 0
	}
	whole := nanos / NanosPerMicro
	remainder := nanos % NanosPerMicro
	if remainder*2 >= NanosPerMicro {
		whole++
	}
	if whole == 0 {
		whole = 1
	}
	return microsToUSD(whole)
}

// exactPerTaskNanos derives the exact per-task amounts from the same inputs the
// micro-USD fields come from.
//
// Supplier entitlement rounds UP (never shave a positive entitlement). Buyer
// charge rounds UP into nanos so the contribution floor still holds; the ceil
// granularity is one nano per task, not one micro. The share is carried as a
// nano-fraction so the multiplication stays integer.
func exactPerTaskNanos(
	c Currency, baseComputeUSD, share, buyerPerTaskUSD float64, tasks int,
	baseComputeNanos int64,
) (baseNanos, supplierNanos, buyerNanos int64, err error) {
	if tasks <= 0 {
		return 0, 0, 0, errors.New("exact per-task economics need at least one task")
	}
	// The exact figure when the caller has one, the float seam only when it does
	// not. A caller that reached the catalogue holds a number that was never
	// rounded; converting its micro-USD projection back into nanos here would
	// re-import the loss the exact field exists to avoid.
	total := MoneyNanos{Currency: c, Nanos: baseComputeNanos}
	if baseComputeNanos <= 0 {
		total, err = MoneyNanosFromUSDFloat(c, baseComputeUSD)
		if err != nil {
			return 0, 0, 0, err
		}
	}
	// Per task rounds DOWN so the per-task amounts can never sum above the total
	// the buyer was quoted.
	baseNanos, err = mulDiv(total.Nanos, 1, int64(tasks), false)
	if err != nil {
		return 0, 0, 0, err
	}
	shareNanos, err := MoneyNanosFromUSDFloat(c, share)
	if err != nil {
		return 0, 0, 0, err
	}
	// UP: a supplier's entitlement must never be shaved by a rounding step.
	supplierNanos, err = mulDiv(baseNanos, shareNanos.Nanos, NanosPerMajorUnit, true)
	if err != nil {
		return 0, 0, 0, err
	}
	// Buyer: ceil into nanos (not micros). See ceilUSDToNanos.
	buyerNanos, err = ceilUSDToNanos(buyerPerTaskUSD)
	if err != nil {
		return 0, 0, 0, err
	}
	if supplierNanos > buyerNanos {
		// Conservation: supplier cannot be owed more than the buyer is charged.
		// Lift the buyer to the supplier entitlement (still one-nano granularity).
		buyerNanos = supplierNanos
	}
	return baseNanos, supplierNanos, buyerNanos, nil
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
	controlPerBatchUSDEnv      = "MERC_CONTROL_PLANE_PER_BATCH_USD"
	minimumContributionUSDEnv  = "MERC_MIN_CONTRIBUTION_PER_BATCH_USD"
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
	controlPerBatch, err := parseRequired(controlPerBatchUSDEnv)
	if err != nil {
		return EconomicSchedule{}, err
	}
	minimumContribution, err := parseRequired(minimumContributionUSDEnv)
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
		MinChargeBatchUSD:            chargeMinUSD(),
		ControlPlanePerBatchUSD:      controlPerBatch,
		ControlPlaneAllocationPolicy: controlPlaneAllocationChargeBatchV1,
		MinimumContributionUSD:       minimumContribution,
		TargetMarginRate:             marginBPS / 10_000,
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
	if !finiteNonNegative(s.ControlPlanePerBatchUSD) {
		return "control_plane_per_batch_usd must be finite and non-negative"
	}
	switch s.ControlPlaneAllocationPolicy {
	case "":
		if s.ControlPlanePerBatchUSD != 0 {
			return "legacy control-plane allocation cannot carry a batch cost"
		}
	case controlPlaneAllocationChargeBatchV1:
		if s.ControlPlanePerTaskUSD != 0 || s.MinChargeBatchUSD <= 0 {
			return "batch control-plane allocation needs a positive charge batch and no per-task cost"
		}
	default:
		return "control-plane allocation policy is unsupported"
	}
	if !finiteNonNegative(s.TargetMarginRate) || s.TargetMarginRate >= 1 {
		return "target_margin_rate must be finite and in [0,1)"
	}
	if !finiteNonNegative(s.MinimumContributionUSD) {
		return "minimum_contribution_usd must be finite and non-negative"
	}
	if !finiteNonNegative(s.MinChargeBatchUSD) {
		return "min_charge_batch_usd must be finite and non-negative"
	}
	processorRate, _ := s.processorFloorTerms()
	controlRate, _ := s.controlPlaneFloorTerms()
	if processorRate+controlRate+s.TargetMarginRate >= 1 {
		return "effective processor rate plus target_margin_rate must be below 1"
	}
	return ""
}

func blockedEconomicPlan(in EconomicPlanInput, schedule EconomicSchedule, reason string) EconomicPlan {
	return EconomicPlan{
		Version: economicPlanVersion, Schedule: schedule, Input: in,
		Executable: false, BlockReason: reason, MinimumMarginHeadroomUSD: -1,
		SupplierSettlementPolicy: supplierSettlementPolicyFloorMinorUnitCarryV2,
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
		// This search retains the historical minimum-base boundary; settlement
		// itself rounds the supplier projection upward below.
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
	baseComputeNanos := in.BaseComputeNanos
	if baseComputeNanos < 0 {
		return blockedEconomicPlan(in, schedule, "base_compute_nanos cannot be negative")
	}
	minTotal := minBillableBaseComputeMicros(in.SupplierShare, in.InitialTaskCount)
	if usdToMicros(baseComputeUSD) < minTotal {
		baseComputeUSD = microsToUSD(minTotal)
	}
	// The floor lifts BOTH projections or neither. Lifting only the micro-USD one
	// would leave the exact figure below the smallest amount the ledger can pay,
	// which is the state the floor exists to prevent.
	if baseComputeNanos > 0 && baseComputeNanos < minTotal*NanosPerMicro {
		baseComputeNanos = minTotal * NanosPerMicro
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
		BaseComputeNanos: baseComputeNanos,
	}

	computePerTask := baseComputeUSD / float64(in.InitialTaskCount)
	// Price the buyer off the EXACT base when there is one.
	//
	// BaseComputeUSD is the micro-USD projection and can sit almost a whole micro
	// BELOW the exact figure, while the supplier's entitlement is the exact share
	// of the exact figure. Taking the buyer charge from the smaller of the two is
	// how a supplier ends up owed more than the buyer was ever charged — a
	// conservation break rather than a rounding one. Taking it from the larger
	// makes buyer >= base >= supplier hold by construction at every job size.
	if baseComputeNanos > 0 {
		exactPerTask := float64(baseComputeNanos) /
			float64(NanosPerMajorUnit) / float64(in.InitialTaskCount)
		if exactPerTask > computePerTask {
			computePerTask = exactPerTask
		}
	}
	// Crossing from exact nanos into the six-decimal ledger projection rounds in
	// the supplier's direction. roundEconomicUSD shaved the CAD fixture's exact
	// 1,393-nano entitlement to 1,000 nanos at settlement even though admission
	// correctly proved the larger floor.
	supplierPerTask := roundEconomicUSD(computePerTask * in.SupplierShare)
	if schedule.ControlPlaneAllocationPolicy == controlPlaneAllocationChargeBatchV1 {
		supplierPerTask = ceilEconomicUSD(computePerTask * in.SupplierShare)
	}
	if usdToMicros(supplierPerTask) < minBillableSupplierMicros {
		// Defensive: the search above is the authority; this branch only fires
		// if share arithmetic and roundEconomicUSD disagree on the boundary.
		supplierPerTask = microsToUSD(minBillableSupplierMicros)
	}
	processorRate, processorPerTaskFixed := schedule.processorFloorTerms()
	controlRate, controlPerTaskFixed := schedule.controlPlaneFloorTerms()
	denominator := 1 - processorRate - controlRate - schedule.TargetMarginRate
	minimumContributionPerTask := schedule.MinimumContributionUSD /
		float64(in.InitialTaskCount)
	minimumBuyerPerTask := (supplierPerTask + processorPerTaskFixed +
		controlPerTaskFixed + minimumContributionPerTask) / denominator
	buyerPerTask := ceilEconomicUSD(math.Max(computePerTask, minimumBuyerPerTask))
	// Percentage algebra is continuous; the durable ledger is micro-major-unit.
	// On a tiny job the absolute minimum contribution still rounds to one micro,
	// and a standalone (full-batch) fee still ceils. Pro-rata batch fee shares
	// no longer micro-ceil independently — see processorFeeFor — but the
	// headroom loop remains so any remaining discrete boundary cannot turn a
	// positive configured contribution into break-even or a loss.
	scenarioAt := func(name string, price float64, tasks int, slaMiss bool) EconomicScenario {
		gross := price*float64(tasks) + in.SLAPremiumUSD
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
		controlCost := schedule.controlPlaneCostFor(net, tasks)
		margin := roundEconomicUSD(net - supplier - processor - controlCost)
		required := roundEconomicUSD(math.Max(
			net*schedule.TargetMarginRate,
			schedule.MinimumContributionUSD*float64(tasks)/float64(in.InitialTaskCount),
		))
		return EconomicScenario{
			Name: name, AcceptedTasks: tasks, GrossChargeUSD: gross, RefundUSD: refund,
			NetBilledUSD: net, SupplierLiabilityUSD: supplier, ProcessorFeeUSD: processor,
			ControlPlaneCostUSD: controlCost, ContributionMarginUSD: margin,
			RequiredMarginUSD: required,
			MarginHeadroomUSD: roundEconomicUSD(margin - required),
		}
	}
	scenarioShapes := []struct {
		name    string
		tasks   int
		slaMiss bool
	}{
		{"one_task_partial", 1, true},
		{"full_success_sla_met", in.InitialTaskCount, false},
		{"full_success_sla_miss", in.InitialTaskCount, true},
		{"max_extra_work_sla_miss", in.InitialTaskCount + in.ExtraTaskReserve, true},
	}
	if schedule.ControlPlaneAllocationPolicy == controlPlaneAllocationChargeBatchV1 {
		for attempt := 0; attempt < 32; attempt++ {
			worst := 0.0
			for _, shape := range scenarioShapes {
				headroom := scenarioAt(shape.name, buyerPerTask, shape.tasks, shape.slaMiss).MarginHeadroomUSD
				if headroom < worst {
					worst = headroom
				}
			}
			if worst >= 0 {
				break
			}
			buyerPerTask = ceilEconomicUSD(buyerPerTask + math.Max(0.000001, -worst))
		}
	}
	safetyFee := roundEconomicUSD(math.Max(0, buyerPerTask-computePerTask))

	plan := EconomicPlan{
		Version: economicPlanVersion, Schedule: schedule, Input: in,
		BaseComputePerTaskUSD:    computePerTask,
		BuyerChargePerTaskUSD:    buyerPerTask,
		SupplierPayoutPerTaskUSD: supplierPerTask,
		SupplierSettlementPolicy: supplierSettlementPolicyFloorMinorUnitCarryV2,
		BuyerSafetyFeePerTaskUSD: safetyFee,
		InitialBuyerChargeUSD:    roundEconomicUSD(buyerPerTask*float64(in.InitialTaskCount) + in.SLAPremiumUSD),
		ReservedBuyerChargeUSD:   roundEconomicUSD(buyerPerTask*float64(in.InitialTaskCount+in.ExtraTaskReserve) + in.SLAPremiumUSD),
		MinimumMarginHeadroomUSD: math.Inf(1),
		// EconomicRoundingPolicy is stamped ONLY when exactPerTaskNanos actually
		// populates the nano fields below. An empty policy with zero nanos is the
		// documented legacy shape; stamping the policy on a failed derivation made
		// "cannot price exactly" indistinguishable from legacy.
		EconomicRoundingPolicy: "",
		Assumptions: []string{
			"every major-unit amount is denominated in schedule.currency; legacy _usd field names do not override that authority",
			"supplier payout is frozen from base compute, independent of buyer safety fee and refundable SLA premium",
			"supplier liability is reserved at six decimals; provider cash floors to whole settlement minor units and every sub-minor remainder stays durably owed",
			"the processor fixed fee is amortised over a minimum-size charge batch, matching how chargeOrDeferJob and FormChargeBatch actually settle",
			"extra accepted work is billable only while atomically consuming the frozen reserve",
			"SLA premium is excluded from supplier liability and may be fully refunded",
			"actual processor fees and contribution margin are reconciled from Stripe and ledger facts",
			"base compute is floored to the minimum billable size so a supplier who performed work is never reserved $0 while the buyer is charged",
			"observed-output token rebate is bounded by the accepted contribution floor; the settled charge always covers supplier payout, processor fee, control-plane cost and required contribution under the frozen schedule",
		},
	}

	addScenario := func(name string, tasks int, slaMiss bool) {
		s := scenarioAt(name, buyerPerTask, tasks, slaMiss)
		plan.Scenarios = append(plan.Scenarios, s)
		if s.MarginHeadroomUSD < plan.MinimumMarginHeadroomUSD {
			plan.MinimumMarginHeadroomUSD = s.MarginHeadroomUSD
			plan.MinimumScenario = name
		}
	}

	addScenario("one_task_partial", 1, true)
	addScenario("full_success_sla_met", in.InitialTaskCount, false)
	addScenario("full_success_sla_miss", in.InitialTaskCount, true)
	addScenario("max_extra_work_sla_miss", in.InitialTaskCount+in.ExtraTaskReserve, true)

	// The exact half is the settlement authority. Only stamp the policy when
	// derivation actually populated positive nano fields. On success, the float
	// money fields are rewritten as the six-decimal projection of those nanos so
	// denormalized columns and plan_json agree and the float path stops being a
	// second, independent derivation.
	if base, supplier, buyer, err := exactPerTaskNanos(
		MustParseCurrency(schedule.Currency), in.BaseComputeUSD, in.SupplierShare,
		buyerPerTask, in.InitialTaskCount, in.BaseComputeNanos,
	); err == nil && supplier > 0 && buyer > 0 {
		plan.BaseComputePerTaskNanos = base
		plan.SupplierPayoutPerTaskNanos = supplier
		plan.BuyerChargePerTaskNanos = buyer
		plan.EconomicRoundingPolicy = economicRoundingPolicy

		// Float fields become projections of the nano authority. Prefer the
		// higher of the floor-proven micro-ceil buyer and the nano projection so
		// the contribution floor solved above still holds in the six-decimal
		// domain used by scenarios and dashboards.
		projectedSupplier := projectNanosToUSD(supplier)
		projectedBuyer := projectNanosToUSD(buyer)
		if projectedBuyer < buyerPerTask {
			projectedBuyer = buyerPerTask
			// Keep buyer nanos at least the micro-aligned floor-proven charge.
			if aligned, aerr := ceilUSDToNanos(buyerPerTask); aerr == nil && aligned > plan.BuyerChargePerTaskNanos {
				plan.BuyerChargePerTaskNanos = aligned
			}
		}
		// scenarioAt closes over supplierPerTask; rebind before rebuild.
		supplierPerTask = projectedSupplier
		buyerPerTask = projectedBuyer
		plan.SupplierPayoutPerTaskUSD = projectedSupplier
		plan.BuyerChargePerTaskUSD = projectedBuyer
		plan.BaseComputePerTaskUSD = float64(base) / float64(NanosPerMajorUnit)
		plan.BuyerSafetyFeePerTaskUSD = roundEconomicUSD(math.Max(0, projectedBuyer-plan.BaseComputePerTaskUSD))
		plan.InitialBuyerChargeUSD = roundEconomicUSD(projectedBuyer*float64(in.InitialTaskCount) + in.SLAPremiumUSD)
		plan.ReservedBuyerChargeUSD = roundEconomicUSD(projectedBuyer*float64(in.InitialTaskCount+in.ExtraTaskReserve) + in.SLAPremiumUSD)

		// Rebuild scenarios against the projected floats so supplier liability
		// and margin headroom match the authority settlement will use.
		plan.Scenarios = nil
		plan.MinimumMarginHeadroomUSD = math.Inf(1)
		plan.MinimumScenario = ""
		addScenario("one_task_partial", 1, true)
		addScenario("full_success_sla_met", in.InitialTaskCount, false)
		addScenario("full_success_sla_miss", in.InitialTaskCount, true)
		addScenario("max_extra_work_sla_miss", in.InitialTaskCount+in.ExtraTaskReserve, true)
	}

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
	// A plan frozen before the exact authority existed carries no nano fields and
	// no policy. Rebuilding it produces both, and comparing the two would report
	// every historical plan as tampered with. So the rebuild is brought back to the
	// stored plan's OWN policy before the comparison — legacy rows keep legacy
	// arithmetic, which is the migration rule, not a special case.
	if plan.EconomicRoundingPolicy == "" {
		rebuilt.EconomicRoundingPolicy = ""
		rebuilt.BaseComputePerTaskNanos = 0
		rebuilt.SupplierPayoutPerTaskNanos = 0
		rebuilt.BuyerChargePerTaskNanos = 0
	}
	if !reflect.DeepEqual(plan, rebuilt) {
		return fmt.Errorf("economic plan snapshot field %s does not match its deterministic input and schedule",
			firstEconomicPlanMismatchField(plan, rebuilt))
	}
	if !plan.Executable {
		return fmt.Errorf("economic plan is not executable: %s", plan.BlockReason)
	}
	return nil
}

func firstEconomicPlanMismatchField(stored, rebuilt EconomicPlan) string {
	return firstEconomicValueMismatch(reflect.ValueOf(stored), reflect.ValueOf(rebuilt), "")
}

func firstEconomicValueMismatch(a, b reflect.Value, path string) string {
	if reflect.DeepEqual(a.Interface(), b.Interface()) {
		return "unknown"
	}
	if a.Kind() == reflect.Struct && b.Kind() == reflect.Struct && a.Type() == b.Type() {
		for i := 0; i < a.NumField(); i++ {
			if reflect.DeepEqual(a.Field(i).Interface(), b.Field(i).Interface()) {
				continue
			}
			name := a.Type().Field(i).Name
			if path != "" {
				name = path + "." + name
			}
			return firstEconomicValueMismatch(a.Field(i), b.Field(i), name)
		}
	}
	if path == "" {
		return "unknown"
	}
	return path
}

func EconomicPlansEqual(a, b EconomicPlan) bool { return reflect.DeepEqual(a, b) }
