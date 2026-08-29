package main

import (
	"context"
	"encoding/json"
	"fmt"
	"math"

	"github.com/google/uuid"
)

// Greppable refuse strings for denormalized-vs-plan money mismatches.
// Fail closed; never repair; never log-and-continue.
const (
	errDenormalizedPlanMoneyMismatch = "denormalized job_economic_plans money columns disagree with plan_json"
	errTaskEconomicNanosMismatch     = "task economic nanos disagree with frozen plan per-task freeze"
)

// denormalizedEconomicPlanMoney is the money authority frozen beside plan_json.
// When BuyerChargePerTaskNanos/SupplierPayoutPerTaskNanos are non-NULL the nano
// pair is the settlement authority; the float pair is its six-decimal projection.
type denormalizedEconomicPlanMoney struct {
	InitialTaskCount           int
	BuyerChargePerTaskUSD      float64
	SupplierPayoutPerTaskUSD   float64
	InitialBuyerChargeUSD      float64
	ReservedBuyerChargeUSD     float64
	SLAPremiumUSD              float64
	FirmQuoteMaxUSD            *float64
	BuyerChargePerTaskNanos    *int64
	SupplierPayoutPerTaskNanos *int64
}

// assertDenormalizedEconomicPlanMoney loads plan_json, unmarshals EconomicPlan,
// and asserts every denormalized money column equals the plan's own field.
// Call inside the same transaction that is about to return amounts for money
// movement. Refuse with a distinct error on mismatch — do not repair.
func assertDenormalizedEconomicPlanMoney(
	ctx context.Context, q ledgerExec, jobID uuid.UUID,
) (EconomicPlan, denormalizedEconomicPlanMoney, error) {
	var (
		planJSON                  []byte
		denorm                    denormalizedEconomicPlanMoney
		firmQuote                 *float64
		buyerNanos, supplierNanos *int64
	)
	err := q.QueryRow(ctx, `
		SELECT plan_json,
		       initial_task_count,
		       buyer_charge_per_task_usd::float8,
		       supplier_payout_per_task_usd::float8,
		       initial_buyer_charge_usd::float8,
		       reserved_buyer_charge_usd::float8,
		       sla_premium_usd::float8,
		       firm_quote_max_usd::float8,
		       buyer_charge_per_task_nanos,
		       supplier_payout_per_task_nanos
		  FROM job_economic_plans
		 WHERE job_id = $1`, jobID).
		Scan(&planJSON,
			&denorm.InitialTaskCount,
			&denorm.BuyerChargePerTaskUSD,
			&denorm.SupplierPayoutPerTaskUSD,
			&denorm.InitialBuyerChargeUSD,
			&denorm.ReservedBuyerChargeUSD,
			&denorm.SLAPremiumUSD,
			&firmQuote,
			&buyerNanos,
			&supplierNanos)
	if err != nil {
		return EconomicPlan{}, denormalizedEconomicPlanMoney{}, err
	}
	denorm.FirmQuoteMaxUSD = firmQuote
	denorm.BuyerChargePerTaskNanos = buyerNanos
	denorm.SupplierPayoutPerTaskNanos = supplierNanos

	var plan EconomicPlan
	if err := json.Unmarshal(planJSON, &plan); err != nil {
		return EconomicPlan{}, denormalizedEconomicPlanMoney{}, fmt.Errorf(
			"decode plan_json for denormalized money check: %w", err)
	}

	mismatch := func(field string, got, want any) error {
		return fmt.Errorf("%s: %s got %v want %v (job %s)",
			errDenormalizedPlanMoneyMismatch, field, got, want, jobID)
	}
	if denorm.InitialTaskCount != plan.Input.InitialTaskCount {
		return EconomicPlan{}, denorm, mismatch("initial_task_count",
			denorm.InitialTaskCount, plan.Input.InitialTaskCount)
	}
	if !moneyEqual6(denorm.BuyerChargePerTaskUSD, plan.BuyerChargePerTaskUSD) {
		return EconomicPlan{}, denorm, mismatch("buyer_charge_per_task_usd",
			denorm.BuyerChargePerTaskUSD, plan.BuyerChargePerTaskUSD)
	}
	if !moneyEqual6(denorm.SupplierPayoutPerTaskUSD, plan.SupplierPayoutPerTaskUSD) {
		return EconomicPlan{}, denorm, mismatch("supplier_payout_per_task_usd",
			denorm.SupplierPayoutPerTaskUSD, plan.SupplierPayoutPerTaskUSD)
	}
	if !moneyEqual6(denorm.InitialBuyerChargeUSD, plan.InitialBuyerChargeUSD) {
		return EconomicPlan{}, denorm, mismatch("initial_buyer_charge_usd",
			denorm.InitialBuyerChargeUSD, plan.InitialBuyerChargeUSD)
	}
	if !moneyEqual6(denorm.ReservedBuyerChargeUSD, plan.ReservedBuyerChargeUSD) {
		return EconomicPlan{}, denorm, mismatch("reserved_buyer_charge_usd",
			denorm.ReservedBuyerChargeUSD, plan.ReservedBuyerChargeUSD)
	}
	if !moneyEqual6(denorm.SLAPremiumUSD, plan.Input.SLAPremiumUSD) {
		return EconomicPlan{}, denorm, mismatch("sla_premium_usd",
			denorm.SLAPremiumUSD, plan.Input.SLAPremiumUSD)
	}
	planFirm := plan.Input.FirmQuoteMaxUSD
	if planFirm > 0 {
		if firmQuote == nil || !moneyEqual6(*firmQuote, planFirm) {
			got := any(nil)
			if firmQuote != nil {
				got = *firmQuote
			}
			return EconomicPlan{}, denorm, mismatch("firm_quote_max_usd", got, planFirm)
		}
	} else if firmQuote != nil {
		return EconomicPlan{}, denorm, mismatch("firm_quote_max_usd", *firmQuote, nil)
	}

	// Nano columns: both NULL (legacy) or both equal to plan nanos.
	planHasNanos := plan.EconomicRoundingPolicy == economicRoundingPolicy &&
		plan.BuyerChargePerTaskNanos > 0 && plan.SupplierPayoutPerTaskNanos > 0
	if planHasNanos {
		if buyerNanos == nil || supplierNanos == nil {
			return EconomicPlan{}, denorm, fmt.Errorf(
				"%s: plan carries exact nanos but denormalized nano columns are NULL (job %s)",
				errDenormalizedPlanMoneyMismatch, jobID)
		}
		if *buyerNanos != plan.BuyerChargePerTaskNanos {
			return EconomicPlan{}, denorm, mismatch("buyer_charge_per_task_nanos",
				*buyerNanos, plan.BuyerChargePerTaskNanos)
		}
		if *supplierNanos != plan.SupplierPayoutPerTaskNanos {
			return EconomicPlan{}, denorm, mismatch("supplier_payout_per_task_nanos",
				*supplierNanos, plan.SupplierPayoutPerTaskNanos)
		}
	} else if buyerNanos != nil || supplierNanos != nil {
		return EconomicPlan{}, denorm, fmt.Errorf(
			"%s: denormalized nano columns set on a legacy plan (job %s)",
			errDenormalizedPlanMoneyMismatch, jobID)
	}

	return plan, denorm, nil
}

// assertTaskEconomicNanosMatchPlan refuses settlement when a task's frozen nano
// pair does not equal the plan's per-task freeze. NULL nanos mean legacy and
// are not checked here (float path remains).
func assertTaskEconomicNanosMatchPlan(
	taskBuyerNanos, taskSupplierNanos *int64,
	plan EconomicPlan,
	taskID uuid.UUID,
) error {
	if taskBuyerNanos == nil && taskSupplierNanos == nil {
		return nil // legacy task
	}
	if taskBuyerNanos == nil || taskSupplierNanos == nil {
		return fmt.Errorf("%s: partial nano pair on task %s", errTaskEconomicNanosMismatch, taskID)
	}
	if plan.BuyerChargePerTaskNanos <= 0 || plan.SupplierPayoutPerTaskNanos <= 0 {
		return fmt.Errorf(
			"%s: task %s has nanos but plan has no exact per-task freeze",
			errTaskEconomicNanosMismatch, taskID)
	}
	if *taskBuyerNanos != plan.BuyerChargePerTaskNanos ||
		*taskSupplierNanos != plan.SupplierPayoutPerTaskNanos {
		return fmt.Errorf(
			"%s: task %s buyer/supplier nanos (%d/%d) != plan (%d/%d)",
			errTaskEconomicNanosMismatch, taskID,
			*taskBuyerNanos, *taskSupplierNanos,
			plan.BuyerChargePerTaskNanos, plan.SupplierPayoutPerTaskNanos)
	}
	return nil
}

// currentUniformCloneTaskEconomics proves that a dynamic current-policy task
// is an exact clone of its anchor before any economic reserve is consumed. The
// caller-provided input identity is checked, while depth/output columns are
// copied from the anchor by SQL. Historical policies keep their prior reserve
// path and report current=false rather than being reinterpreted.
func currentUniformCloneTaskEconomics(
	ctx context.Context,
	q ledgerExec,
	jobID, anchorTaskID uuid.UUID,
	inputRef string,
	chunkIndex int,
) (frozen frozenTaskEconomics, current bool, err error) {
	var (
		policy                          string
		anchorInputRef                  string
		anchorInputSHA256               string
		workloadInputSHA256             string
		anchorChunk                     int
		buyerUSD, supplierUSD           float64
		buyerNanos, supplierPayoutNanos *int64
	)
	err = q.QueryRow(ctx, `
		SELECT COALESCE(j.pricing_decision->>'task_economic_policy',''),
		       t.input_ref,COALESCE(t.input_sha256,''),
		       COALESCE(j.workload_decision #>> '{binding,input_sha256}',''),
		       COALESCE(t.chunk_index,0),
		       t.economic_buyer_charge_usd::float8,
		       t.economic_supplier_payout_usd::float8,
		       t.economic_buyer_charge_nanos,
		       t.economic_supplier_payout_nanos
		  FROM tasks t JOIN jobs j ON j.id=t.job_id
		 WHERE t.id=$1 AND t.job_id=$2`, anchorTaskID, jobID).
		Scan(&policy, &anchorInputRef, &anchorInputSHA256, &workloadInputSHA256,
			&anchorChunk, &buyerUSD, &supplierUSD,
			&buyerNanos, &supplierPayoutNanos)
	if err != nil {
		return frozenTaskEconomics{}, false, err
	}
	if policy != uniformSinglePrimaryTaskEconomicsV1 {
		return frozenTaskEconomics{}, false, nil
	}
	if inputRef != anchorInputRef || chunkIndex != anchorChunk {
		return frozenTaskEconomics{}, true, fmt.Errorf(
			"%w: dynamic task input/chunk %q/%d does not match anchor %q/%d",
			errHeterogeneousTaskEconomicsUnavailable,
			inputRef, chunkIndex, anchorInputRef, anchorChunk)
	}
	if !validSHA256(anchorInputSHA256) || anchorInputSHA256 != workloadInputSHA256 {
		return frozenTaskEconomics{}, true, fmt.Errorf(
			"%w: dynamic task anchor lacks the frozen whole-input digest",
			errHeterogeneousTaskEconomicsUnavailable)
	}
	plan, _, err := assertDenormalizedEconomicPlanMoney(ctx, q, jobID)
	if err != nil {
		return frozenTaskEconomics{}, true, err
	}
	if !moneyEqual6(buyerUSD, plan.BuyerChargePerTaskUSD) ||
		!moneyEqual6(supplierUSD, plan.SupplierPayoutPerTaskUSD) {
		return frozenTaskEconomics{}, true, fmt.Errorf(
			"%s: anchor task %s float money does not match the uniform plan",
			errTaskEconomicNanosMismatch, anchorTaskID)
	}
	if err := assertTaskEconomicNanosMatchPlan(
		buyerNanos, supplierPayoutNanos, plan, anchorTaskID); err != nil {
		return frozenTaskEconomics{}, true, err
	}
	return frozenTaskEconomics{
		BuyerChargeUSD: buyerUSD, SupplierPayoutUSD: supplierUSD,
		BuyerChargeNanos: buyerNanos, SupplierPayoutNanos: supplierPayoutNanos,
	}, true, nil
}

func moneyEqual6(a, b float64) bool {
	return usdToMicros(a) == usdToMicros(b) &&
		!math.IsNaN(a) && !math.IsNaN(b) &&
		!math.IsInf(a, 0) && !math.IsInf(b, 0)
}

// nullEconomicNanos returns SQL NULL for legacy rows and the value otherwise.
func nullEconomicNanos(nanos int64, set bool) any {
	if !set {
		return nil
	}
	return nanos
}
