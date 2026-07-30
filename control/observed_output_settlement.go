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

// observedOutputSettlement is the ledger-time adjustment of frozen per-task
// economics by the unused generative output share. Frozen task columns are
// never written; only the amounts passed to splitFrozenCharge change.
type observedOutputSettlement struct {
	BilledCharge   float64
	SupplierPayout float64
	// Applied is true when the observed-output formula adjusted the freeze.
	// False means settle exactly at the frozen pair (non-generative, missing
	// plan/observation, or zero ceiling).
	Applied bool
	// Evidence for buyer receipts. Zero when Applied is false.
	CeilingTokens  int64
	ObservedTokens int64
	UnusedShare    float64
	RebateUSD      float64
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
	out := observedOutputSettlement{
		BilledCharge:   frozenCharge,
		SupplierPayout: frozenPayout,
	}
	if !moneyUSDInDomain(frozenCharge) || !moneyUSDInDomain(frozenPayout) ||
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

	billed := roundUSD(frozenCharge * (1.0 - unusedShare))
	if billed < minBillableSettlementUSD {
		billed = minBillableSettlementUSD
	}
	// Invariant 2: never increase relative to the freeze.
	if billed > frozenCharge {
		billed = frozenCharge
	}
	// Scaling from freeze must keep supplier within [0, billed] and
	// [0, frozenPayout].
	payout := roundUSD(frozenPayout * billed / frozenCharge)
	if payout < 0 {
		payout = 0
	}
	if payout > frozenPayout {
		payout = frozenPayout
	}
	if payout > billed {
		payout = billed
	}

	rebate := roundUSD(frozenCharge - billed)
	if rebate < 0 {
		rebate = 0
	}
	out.BilledCharge = billed
	out.SupplierPayout = payout
	out.Applied = billed < frozenCharge || payout < frozenPayout
	out.CeilingTokens = ceiling
	out.ObservedTokens = observed
	out.UnusedShare = unusedShare
	out.RebateUSD = rebate
	// Always surface ceiling/observed once we had a generative ceiling, even
	// when the floor pinned the charge (buyer can still audit the observation).
	if out.CeilingTokens == 0 {
		out.CeilingTokens = ceiling
		out.ObservedTokens = observed
	}
	return out
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
		expectedRecords            int64
		reportedTokens             *int64
		workloadJSON               []byte
		workloadSHA256             string
		computePlanJSON            []byte
		computePlanSHA256          string
		economicPlanJSON           []byte
	)
	if err := q.QueryRow(ctx, `
		SELECT t.economic_buyer_charge_usd::float8, t.economic_supplier_payout_usd::float8,
		       COALESCE(t.expected_output_records,0), t.reported_tokens_used,
		       j.workload_decision, COALESCE(j.workload_decision_sha256,''),
		       j.compute_plan, COALESCE(j.compute_plan_sha256,''),
		       ep.plan_json
		  FROM tasks t JOIN jobs j ON j.id = t.job_id
		  LEFT JOIN job_economic_plans ep ON ep.job_id = j.id
		 WHERE t.id = $1`, taskID).
		Scan(&frozenCharge, &frozenPayout, &expectedRecords, &reportedTokens,
			&workloadJSON, &workloadSHA256, &computePlanJSON, &computePlanSHA256,
			&economicPlanJSON); err != nil {
		return observedOutputSettlement{}, err
	}
	if !moneyUSDInDomain(frozenCharge) || !moneyUSDInDomain(frozenPayout) ||
		frozenCharge <= 0 || frozenPayout < 0 || frozenPayout > frozenCharge {
		return observedOutputSettlement{}, fmt.Errorf("task %s has invalid frozen economics", taskID)
	}
	out := observedOutputSettlement{BilledCharge: frozenCharge, SupplierPayout: frozenPayout}
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
		var economic EconomicPlan
		if err := json.Unmarshal(economicPlanJSON, &economic); err != nil {
			return observedOutputSettlement{}, fmt.Errorf(
				"decode economic plan for settlement: %w", err)
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
	return settleObservedOutputTokens(
		frozenCharge, frozenPayout,
		settlementInputUnitsForComputePlan(plan), plan.EstimatedOutputTokens,
		expectedRecords, effectiveObservedOutputMaxTokens(workload, plan),
		reported, hasReported,
	), nil
}
