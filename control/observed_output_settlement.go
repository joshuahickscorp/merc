package main

import (
	"context"
	"encoding/json"
	"fmt"
	"math"

	"github.com/google/uuid"
)

// minBillableSettlementUSD is the smallest positive buyer charge the ledger can
// represent. Settlement may rebate unused generative output down to this floor
// but never to zero when frozen economics were positive.
const minBillableSettlementUSD = 1.0 / float64(microUSDPerUSD)

// minBillableSettlementNanos is the same floor in integer nano-major-units.
const minBillableSettlementNanos int64 = NanosPerMicro

// observedOutputSettlement is the ledger-time adjustment of frozen per-task
// economics by the unused generative output share. Frozen task columns are
// never written; only the amounts passed to splitFrozenCharge change.
type observedOutputSettlement struct {
	BilledCharge   float64
	SupplierPayout float64
	// Nano authority for exact-policy freezes. Zero means the float pair is
	// the (legacy) authority and splitFrozenCharge is used as before.
	BilledChargeNanos   int64
	SupplierPayoutNanos int64
	HasNanos            bool
	// Applied is true when the observed-output formula adjusted the freeze.
	// False means settle exactly at the frozen pair (non-generative, missing
	// plan/observation, or zero ceiling).
	Applied bool
	// FloorClamped is true when the token-proportional rebate was reduced so
	// the settled charge still covers supplier + processor + control +
	// required contribution under the frozen schedule.
	FloorClamped bool
	// Evidence for buyer receipts. Zero when Applied is false.
	CeilingTokens  int64
	ObservedTokens int64
	UnusedShare    float64
	RebateUSD      float64
	// UnclampedRebateUSD is the token-proportional rebate before the
	// contribution-floor clamp; set when FloorClamped is true so the receipt
	// can show why the full rebate was not applied.
	UnclampedRebateUSD float64
}

// effectiveObservedOutputMaxTokens returns the frozen per-record ceiling used
// by pricing, settlement and presentation. Generative requests that omitted an
// explicit max_tokens were priced and planned with defaultQuoteMaxTokens, so a
// zero in the original binding must resolve to that same default everywhere.
func effectiveObservedOutputMaxTokens(workload WorkloadDecision, plan ComputePlan) uint32 {
	maxTokens := workload.Binding.JobType.MaxTokens
	if maxTokens == 0 && generativeJobType(workload.Binding.JobType.Type) &&
		plan.EstimatedOutputTokens > 0 {
		return defaultQuoteMaxTokens
	}
	return maxTokens
}

// settlementInputUnitsForComputePlan preserves the input-unit composition that
// created the frozen price. Version 3 and later record the exact catalogue
// authority. Version-1/2 plans predate that field, so they retain their existing rounded
// whole-input compatibility rule instead of being re-priced from selected-body
// depth estimates after acceptance.
func settlementInputUnitsForComputePlan(plan ComputePlan) float64 {
	if plan.Version == computePlanVersionV3 || plan.Version == computePlanVersion {
		return plan.SettlementInputUnits
	}
	return float64(estimatedInputTokensForComputePlanV1(plan.InputRecords, plan.InputBytes))
}

// settleObservedOutputTokens bounds generative batch settlement by tokens the
// worker actually reported, relative to the frozen output ceiling.
//
// Arithmetic (invariants win on conflict):
//
//	outputUnitShare = estimatedOut / (estimatedIn + estimatedOut)
//	ceilingTokens   = expectedOutputRecords * maxTokens
//	observed        = clamp(reportedTokens, 0, ceilingTokens)
//	unusedShare     = outputUnitShare * (1 - observed/ceilingTokens)
//	billedCharge    = round(frozenCharge * (1 - unusedShare))
//	billedCharge    = max(billedCharge, minBillable)   // still never above freeze
//	supplierPayout' = round(frozenPayout * billedCharge / frozenCharge)
//	// then clamp so contribution floor still holds under the frozen schedule
//
// Missing plan inputs, non-generative work (estimatedOut == 0), a zero
// ceiling, or a missing reported-token observation settle at the freeze.
// Settlement may only reduce relative to the freeze.
func settleObservedOutputTokens(
	frozenCharge, frozenPayout float64,
	estimatedIn float64, estimatedOut int64,
	expectedOutputRecords int64,
	maxTokens uint32,
	reportedTokens int64,
	hasReported bool,
) observedOutputSettlement {
	return settleObservedOutputTokensWithSchedule(
		frozenCharge, frozenPayout,
		0, 0, false,
		estimatedIn, estimatedOut,
		expectedOutputRecords, maxTokens,
		reportedTokens, hasReported,
		EconomicSchedule{}, 1,
	)
}

// settleObservedOutputTokensWithSchedule is the full settlement path: optional
// nano freeze, optional schedule for the contribution-floor clamp.
// initialTaskCount is the plan's InitialTaskCount used to prorate
// MinimumContributionUSD; pass 1 when unknown (conservative: full minimum).
func settleObservedOutputTokensWithSchedule(
	frozenCharge, frozenPayout float64,
	frozenChargeNanos, frozenPayoutNanos int64,
	hasNanos bool,
	estimatedIn float64, estimatedOut int64,
	expectedOutputRecords int64,
	maxTokens uint32,
	reportedTokens int64,
	hasReported bool,
	schedule EconomicSchedule,
	initialTaskCount int,
) observedOutputSettlement {
	out := observedOutputSettlement{
		BilledCharge:   frozenCharge,
		SupplierPayout: frozenPayout,
		HasNanos:       hasNanos,
	}
	if hasNanos {
		out.BilledChargeNanos = frozenChargeNanos
		out.SupplierPayoutNanos = frozenPayoutNanos
		out.BilledCharge = projectNanosToUSD(frozenChargeNanos)
		out.SupplierPayout = projectNanosToUSD(frozenPayoutNanos)
	}
	if hasNanos {
		if frozenChargeNanos <= 0 || frozenPayoutNanos < 0 || frozenPayoutNanos > frozenChargeNanos {
			return out
		}
	} else if !moneyUSDInDomain(frozenCharge) || !moneyUSDInDomain(frozenPayout) ||
		frozenCharge <= 0 || frozenPayout < 0 || frozenPayout > frozenCharge {
		return out
	}
	if !hasReported || estimatedOut <= 0 || math.IsNaN(estimatedIn) ||
		math.IsInf(estimatedIn, 0) || estimatedIn < 0 {
		return out
	}
	totalUnits := estimatedIn + float64(estimatedOut)
	if totalUnits <= 0 {
		return out
	}
	if expectedOutputRecords <= 0 || maxTokens == 0 {
		return out
	}
	// expectedOutputRecords * maxTokens can overflow int64 for absurd inputs;
	// refuse to invent a ceiling and settle at the freeze.
	if expectedOutputRecords > math.MaxInt64/int64(maxTokens) {
		return out
	}
	ceiling := expectedOutputRecords * int64(maxTokens)
	if ceiling <= 0 {
		return out
	}

	if reportedTokens < 0 {
		// The production commit API accepts uint64 and therefore cannot
		// produce a negative observation. If the durable projection is ever
		// negative, treat it as corrupted authority and keep the freeze rather
		// than turning the corruption into the maximum possible rebate.
		return out
	}
	observed := reportedTokens
	if observed > ceiling {
		observed = ceiling
	}

	outputUnitShare := float64(estimatedOut) / totalUnits
	unusedShare := outputUnitShare * (1.0 - float64(observed)/float64(ceiling))
	if unusedShare < 0 {
		unusedShare = 0
	}
	if unusedShare > outputUnitShare {
		unusedShare = outputUnitShare
	}
	// No unused output → freeze stands (including the over-report clamp).
	if unusedShare == 0 {
		out.CeilingTokens = ceiling
		out.ObservedTokens = observed
		return out
	}

	var (
		billed, payout           float64
		billedNanos, payoutNanos int64
		unclampedRebate          float64
	)
	if hasNanos {
		// Token-proportional rebate in integer nanos.
		// billed = round(frozen * (1 - unusedShare)); use half-away via float then ceil-safe cast.
		raw := float64(frozenChargeNanos) * (1.0 - unusedShare)
		billedNanos = int64(math.Round(raw))
		if billedNanos < minBillableSettlementNanos {
			billedNanos = minBillableSettlementNanos
		}
		if billedNanos > frozenChargeNanos {
			billedNanos = frozenChargeNanos
		}
		// Scale supplier proportionally in nanos.
		if frozenChargeNanos > 0 {
			payoutNanos = int64(math.Round(float64(frozenPayoutNanos) * float64(billedNanos) / float64(frozenChargeNanos)))
		}
		if payoutNanos < 0 {
			payoutNanos = 0
		}
		if payoutNanos > frozenPayoutNanos {
			payoutNanos = frozenPayoutNanos
		}
		if payoutNanos > billedNanos {
			payoutNanos = billedNanos
		}
		unclampedRebate = projectNanosToUSD(frozenChargeNanos - billedNanos)

		// Contribution-floor clamp in nano space using projected USD for fee models
		// that still live in the float schedule.
		if schedule.Version != "" {
			clamped, clampedPayout, didClamp := clampSettlementToContributionFloorNanos(
				billedNanos, payoutNanos, frozenChargeNanos, frozenPayoutNanos,
				schedule, initialTaskCount,
			)
			if didClamp {
				out.FloorClamped = true
				out.UnclampedRebateUSD = unclampedRebate
				billedNanos, payoutNanos = clamped, clampedPayout
			}
		}
		billed = projectNanosToUSD(billedNanos)
		payout = projectNanosToUSD(payoutNanos)
	} else {
		billed = roundUSD(frozenCharge * (1.0 - unusedShare))
		if billed < minBillableSettlementUSD {
			billed = minBillableSettlementUSD
		}
		// Invariant 2: never increase relative to the freeze.
		if billed > frozenCharge {
			billed = frozenCharge
		}
		// Scaling from freeze must keep supplier within [0, billed] and
		// [0, frozenPayout].
		payout = roundUSD(frozenPayout * billed / frozenCharge)
		if payout < 0 {
			payout = 0
		}
		if payout > frozenPayout {
			payout = frozenPayout
		}
		if payout > billed {
			payout = billed
		}
		unclampedRebate = roundUSD(frozenCharge - billed)

		if schedule.Version != "" {
			clamped, clampedPayout, didClamp := clampSettlementToContributionFloorUSD(
				billed, payout, frozenCharge, frozenPayout,
				schedule, initialTaskCount,
			)
			if didClamp {
				out.FloorClamped = true
				out.UnclampedRebateUSD = unclampedRebate
				billed, payout = clamped, clampedPayout
			}
		}
	}

	if hasNanos {
		if r := frozenChargeNanos - billedNanos; r > 0 {
			out.RebateUSD = projectNanosToUSD(r)
		} else {
			out.RebateUSD = 0
		}
		out.Applied = billedNanos < frozenChargeNanos || payoutNanos < frozenPayoutNanos || out.FloorClamped
	} else {
		rebate := roundUSD(frozenCharge - billed)
		if rebate < 0 {
			rebate = 0
		}
		out.RebateUSD = rebate
		out.Applied = billed < frozenCharge || payout < frozenPayout || out.FloorClamped
	}
	out.BilledCharge = billed
	out.SupplierPayout = payout
	out.BilledChargeNanos = billedNanos
	out.SupplierPayoutNanos = payoutNanos
	out.HasNanos = hasNanos
	out.CeilingTokens = ceiling
	out.ObservedTokens = observed
	out.UnusedShare = unusedShare
	// Always surface ceiling/observed once we had a generative ceiling, even
	// when the floor pinned the charge (buyer can still audit the observation).
	if out.CeilingTokens == 0 {
		out.CeilingTokens = ceiling
		out.ObservedTokens = observed
	}
	return out
}

// clampSettlementToContributionFloorUSD finds the largest rebate (smallest
// billed charge) such that:
//
//	billed - supplier - processor - control >= required contribution
//
// under the frozen schedule. Returns the clamped pair and whether clamping
// raised the charge above the token-proportional amount.
func clampSettlementToContributionFloorUSD(
	billed, payout, frozenCharge, frozenPayout float64,
	schedule EconomicSchedule,
	initialTaskCount int,
) (float64, float64, bool) {
	if initialTaskCount <= 0 {
		initialTaskCount = 1
	}
	covers := func(charge, supplier float64) bool {
		if charge <= 0 {
			return false
		}
		processor := schedule.processorFeeFor(charge)
		control := schedule.controlPlaneCostFor(charge, 1)
		required := roundEconomicUSD(math.Max(
			charge*schedule.TargetMarginRate,
			schedule.MinimumContributionUSD/float64(initialTaskCount),
		))
		margin := roundEconomicUSD(charge - supplier - processor - control)
		return margin+1e-12 >= required
	}
	// Token-proportional amount already covers the floor → no clamp.
	if covers(billed, payout) {
		return billed, payout, false
	}
	// Freeze itself must cover (plan proved it); if not, settle at freeze.
	freezePayout := frozenPayout
	if freezePayout > frozenCharge {
		freezePayout = frozenCharge
	}
	if !covers(frozenCharge, freezePayout) {
		return frozenCharge, freezePayout, true
	}
	// Binary search the minimum charge in [billed, frozenCharge] that covers.
	// Micro-USD granularity.
	lo := usdToMicros(billed)
	hi := usdToMicros(frozenCharge)
	best := hi
	for lo <= hi {
		mid := (lo + hi) / 2
		charge := microsToUSD(mid)
		supplier := roundUSD(frozenPayout * charge / frozenCharge)
		if supplier > charge {
			supplier = charge
		}
		if supplier > frozenPayout {
			supplier = frozenPayout
		}
		if covers(charge, supplier) {
			best = mid
			hi = mid - 1
		} else {
			lo = mid + 1
		}
	}
	charge := microsToUSD(best)
	supplier := roundUSD(frozenPayout * charge / frozenCharge)
	if supplier > charge {
		supplier = charge
	}
	if supplier > frozenPayout {
		supplier = frozenPayout
	}
	return charge, supplier, true
}

// clampSettlementToContributionFloorNanos is the nano-authority form of the
// contribution-floor clamp. Fee models still evaluate in float via projection
// of the candidate charge; the search itself is over integer nanos.
func clampSettlementToContributionFloorNanos(
	billed, payout, frozenCharge, frozenPayout int64,
	schedule EconomicSchedule,
	initialTaskCount int,
) (int64, int64, bool) {
	if initialTaskCount <= 0 {
		initialTaskCount = 1
	}
	covers := func(chargeNanos, supplierNanos int64) bool {
		if chargeNanos <= 0 {
			return false
		}
		charge := projectNanosToUSD(chargeNanos)
		supplier := projectNanosToUSD(supplierNanos)
		if supplier > charge {
			supplier = charge
		}
		processor := schedule.processorFeeFor(charge)
		control := schedule.controlPlaneCostFor(charge, 1)
		required := roundEconomicUSD(math.Max(
			charge*schedule.TargetMarginRate,
			schedule.MinimumContributionUSD/float64(initialTaskCount),
		))
		margin := roundEconomicUSD(charge - supplier - processor - control)
		return margin+1e-12 >= required
	}
	if covers(billed, payout) {
		return billed, payout, false
	}
	freezePayout := frozenPayout
	if freezePayout > frozenCharge {
		freezePayout = frozenCharge
	}
	if !covers(frozenCharge, freezePayout) {
		return frozenCharge, freezePayout, true
	}
	lo, hi := billed, frozenCharge
	best := hi
	for lo <= hi {
		mid := (lo + hi) / 2
		supplier := int64(0)
		if frozenCharge > 0 {
			supplier = int64(math.Round(float64(frozenPayout) * float64(mid) / float64(frozenCharge)))
		}
		if supplier > mid {
			supplier = mid
		}
		if supplier > frozenPayout {
			supplier = frozenPayout
		}
		if covers(mid, supplier) {
			best = mid
			hi = mid - 1
		} else {
			lo = mid + 1
		}
	}
	supplier := int64(0)
	if frozenCharge > 0 {
		supplier = int64(math.Round(float64(frozenPayout) * float64(best) / float64(frozenCharge)))
	}
	if supplier > best {
		supplier = best
	}
	if supplier > frozenPayout {
		supplier = frozenPayout
	}
	return best, supplier, true
}

// loadObservedOutputSettlement is the ONE place settlement reads its inputs.
//
// It exists because the producer (verification planning) and the validator
// (verification apply) each recomputed this pair from a different source, and
// they diverged: the recovery path rebuilds CommitTaskInfo from a stored
// snapshot that never carried jobMaxTokens, so the producer saw maxTokens=0 and
// settled at the freeze while the validator read max_tokens from the job and
// computed a rebate. Every apply then failed with "settlement buyer_charge
// conflicts with observed-output task economics" and the job could never
// finalize.
//
// Both callers now read the same columns through the same SQL. `q` is a pgx.Tx
// for the validator (so it sees the transaction's snapshot) and the pool for
// the planner.
func loadObservedOutputSettlement(
	ctx context.Context, q ledgerExec, taskID uuid.UUID,
) (observedOutputSettlement, error) {
	var (
		frozenCharge, frozenPayout float64
		buyerNanos, supplierNanos  *int64
		expectedRecords            int64
		reportedTokens             *int64
		workloadJSON               []byte
		workloadSHA256             string
		computePlanJSON            []byte
		computePlanSHA256          string
		economicPlanJSON           []byte
		jobID                      uuid.UUID
	)
	if err := q.QueryRow(ctx, `
		SELECT t.economic_buyer_charge_usd::float8, t.economic_supplier_payout_usd::float8,
		       t.economic_buyer_charge_nanos, t.economic_supplier_payout_nanos,
		       COALESCE(t.expected_output_records,0), t.reported_tokens_used,
		       j.id, j.workload_decision, COALESCE(j.workload_decision_sha256,''),
		       j.compute_plan, COALESCE(j.compute_plan_sha256,''),
		       ep.plan_json
		  FROM tasks t JOIN jobs j ON j.id = t.job_id
		  LEFT JOIN job_economic_plans ep ON ep.job_id = j.id
		 WHERE t.id = $1`, taskID).
		Scan(&frozenCharge, &frozenPayout, &buyerNanos, &supplierNanos,
			&expectedRecords, &reportedTokens,
			&jobID, &workloadJSON, &workloadSHA256, &computePlanJSON, &computePlanSHA256,
			&economicPlanJSON); err != nil {
		return observedOutputSettlement{}, err
	}
	if !moneyUSDInDomain(frozenCharge) || !moneyUSDInDomain(frozenPayout) ||
		frozenCharge <= 0 || frozenPayout < 0 || frozenPayout > frozenCharge {
		return observedOutputSettlement{}, fmt.Errorf("task %s has invalid frozen economics", taskID)
	}

	hasNanos := buyerNanos != nil && supplierNanos != nil
	var (
		frozenChargeNanos, frozenPayoutNanos int64
		schedule                             EconomicSchedule
		initialTaskCount                     int
		economic                             EconomicPlan
	)
	if hasNanos {
		frozenChargeNanos = *buyerNanos
		frozenPayoutNanos = *supplierNanos
		if frozenChargeNanos <= 0 || frozenPayoutNanos < 0 || frozenPayoutNanos > frozenChargeNanos {
			return observedOutputSettlement{}, fmt.Errorf("task %s has invalid frozen nano economics", taskID)
		}
	}

	// When a plan row exists, assert denormalized money equals plan_json before
	// any billing, and assert task nanos match the plan freeze when non-NULL.
	if len(economicPlanJSON) > 0 {
		plan, _, aerr := assertDenormalizedEconomicPlanMoney(ctx, q, jobID)
		if aerr != nil {
			return observedOutputSettlement{}, aerr
		}
		economic = plan
		if err := assertTaskEconomicNanosMatchPlan(buyerNanos, supplierNanos, plan, taskID); err != nil {
			return observedOutputSettlement{}, err
		}
		schedule = plan.Schedule
		initialTaskCount = plan.Input.InitialTaskCount
	}

	out := observedOutputSettlement{
		BilledCharge: frozenCharge, SupplierPayout: frozenPayout,
		HasNanos: hasNanos,
	}
	if hasNanos {
		out.BilledChargeNanos = frozenChargeNanos
		out.SupplierPayoutNanos = frozenPayoutNanos
	}
	if len(computePlanJSON) == 0 {
		return out, nil // legacy job without a frozen plan settles at the freeze
	}
	if len(workloadJSON) == 0 {
		return observedOutputSettlement{}, fmt.Errorf(
			"task %s has a compute plan without workload authority", taskID)
	}
	var workload WorkloadDecision
	if err := json.Unmarshal(workloadJSON, &workload); err != nil {
		return observedOutputSettlement{}, fmt.Errorf(
			"decode workload decision for settlement: %w", err)
	}
	if err := ValidateFrozenWorkloadDecisionSnapshot(workload); err != nil {
		return observedOutputSettlement{}, fmt.Errorf(
			"invalid frozen workload decision for settlement: %w", err)
	}
	gotWorkloadSHA256, err := workloadDecisionDigest(workload)
	if err != nil {
		return observedOutputSettlement{}, fmt.Errorf(
			"hash workload decision for settlement: %w", err)
	}
	if workloadSHA256 == "" || workloadSHA256 != gotWorkloadSHA256 {
		return observedOutputSettlement{}, fmt.Errorf(
			"frozen workload decision digest mismatch for task %s", taskID)
	}
	var plan ComputePlan
	if err := json.Unmarshal(computePlanJSON, &plan); err != nil {
		return observedOutputSettlement{}, fmt.Errorf("decode compute plan for settlement: %w", err)
	}
	if err := ValidateFrozenComputePlanSnapshot(plan, workload); err != nil {
		return observedOutputSettlement{}, fmt.Errorf(
			"invalid frozen compute plan for settlement: %w", err)
	}
	gotComputeSHA256, err := computePlanDigest(plan)
	if err != nil {
		return observedOutputSettlement{}, fmt.Errorf(
			"hash compute plan for settlement: %w", err)
	}
	if computePlanSHA256 == "" || computePlanSHA256 != gotComputeSHA256 {
		return observedOutputSettlement{}, fmt.Errorf(
			"frozen compute plan digest mismatch for task %s", taskID)
	}
	if plan.ExecutionMode == computeExecutionDistributed {
		if len(economicPlanJSON) == 0 {
			return observedOutputSettlement{}, fmt.Errorf(
				"task %s has no frozen economic authority", taskID)
		}
		if err := ValidateComputePlanEconomicSnapshot(plan, workload, economic); err != nil {
			return observedOutputSettlement{}, fmt.Errorf(
				"compute/economic authority mismatch for settlement: %w", err)
		}
	}
	reported := int64(0)
	hasReported := reportedTokens != nil
	if hasReported {
		reported = *reportedTokens
	}
	return settleObservedOutputTokensWithSchedule(
		frozenCharge, frozenPayout,
		frozenChargeNanos, frozenPayoutNanos, hasNanos,
		settlementInputUnitsForComputePlan(plan), plan.EstimatedOutputTokens,
		expectedRecords, effectiveObservedOutputMaxTokens(workload, plan),
		reported, hasReported,
		schedule, initialTaskCount,
	), nil
}
