package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"strings"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

// contributionJobIDs returns a stable, duplicate-free input set. The report
// queries already order their jobs, but keeping this boundary explicit prevents
// a caller from making one durable fact appear twice in a rollup.
func contributionJobIDs(jobIDs []uuid.UUID) []uuid.UUID {
	seen := make(map[uuid.UUID]struct{}, len(jobIDs))
	out := make([]uuid.UUID, 0, len(jobIDs))
	for _, jobID := range jobIDs {
		if jobID == uuid.Nil {
			continue
		}
		if _, ok := seen[jobID]; ok {
			continue
		}
		seen[jobID] = struct{}{}
		out = append(out, jobID)
	}
	return out
}

func contributionFactsFromJobRow(
	jobID uuid.UUID,
	status, currency string,
	pricingBlob []byte,
	pricingSHA256 string,
	chargeBatchID *uuid.UUID,
	stripePI *string,
	slaGuaranteeSecs int,
	slaMet *bool,
) (contributionJobFacts, error) {
	facts := contributionJobFacts{
		Status: status, Currency: currency,
		PricingSHA256: pricingSHA256, ChargeBatchID: chargeBatchID,
		StripePI: stripePI, SLAGuaranteeSecs: slaGuaranteeSecs, SLAMet: slaMet,
	}
	if len(pricingBlob) == 0 {
		return facts, errors.New("job has no pricing decision settlement authority")
	}
	if err := json.Unmarshal(pricingBlob, &facts.Pricing); err != nil {
		return facts, fmt.Errorf("decode pricing decision for contribution: %w", err)
	}
	pricingSHA, err := pricingDecisionDigest(facts.Pricing)
	if err != nil {
		return facts, err
	}
	if !validSHA256(facts.PricingSHA256) || facts.PricingSHA256 != pricingSHA {
		return facts, fmt.Errorf("job %s pricing decision digest mismatch", jobID)
	}
	if facts.Pricing.Currency != facts.Currency {
		return facts, fmt.Errorf("%w: job %s pricing currency %s differs from job currency %s",
			errCurrencyMismatch, jobID, facts.Pricing.Currency, facts.Currency)
	}
	if _, err := ParseCurrency(facts.Currency); err != nil {
		return facts, err
	}
	return facts, nil
}

func applyContributionLedgerFact(
	facts *contributionJobFacts,
	jobID uuid.UUID,
	kind, currency string,
	amount int64,
) error {
	if facts == nil {
		return errors.New("nil contribution facts")
	}
	if err := requireContributionCurrency(jobID, facts.Currency, currency); err != nil {
		return err
	}
	facts.HasSettlementLedger = true
	var err error
	switch kind {
	case KindBuyerCharge:
		if amount > 0 {
			return errors.New("buyer charge has an invalid positive sign")
		}
		err = checkedContributionAdd(&facts.BuyerGrossNanos, -amount)
	case KindBuyerRefund:
		if amount < 0 {
			return errors.New("buyer refund has an invalid negative sign")
		}
		err = checkedContributionAdd(&facts.BuyerRefundNanos, amount)
	case KindSLARefund:
		if amount < 0 {
			return errors.New("SLA refund has an invalid negative sign")
		}
		err = checkedContributionAdd(&facts.SLARefundNanos, amount)
	case KindSupplierCredit:
		if amount < 0 {
			return errors.New("supplier credit has an invalid negative sign")
		}
		err = checkedContributionAdd(&facts.SupplierCreditNanos, amount)
	case KindClawback:
		if amount > 0 {
			return errors.New("supplier clawback has an invalid positive sign")
		}
		err = checkedContributionAdd(&facts.SupplierClawbackNanos, -amount)
	case KindRiskReserveAccrual:
		err = checkedContributionAdd(&facts.RiskAccrualNanos, amount)
	case KindRiskReserveRelease:
		err = checkedContributionAdd(&facts.RiskReleaseNanos, -amount)
	case KindRiskReserveConsume:
		err = checkedContributionAdd(&facts.RiskConsumeNanos, -amount)
	}
	return err
}

func validateContributionRiskState(
	jobID uuid.UUID,
	facts *contributionJobFacts,
	riskPricingSHA, riskCurrency string,
) error {
	if facts == nil {
		return errors.New("nil contribution facts")
	}
	facts.RiskCanonical = true
	if riskPricingSHA != facts.PricingSHA256 {
		return errors.New("risk reserve pricing digest disagrees with contribution key")
	}
	if err := requireContributionCurrency(jobID, facts.Currency, riskCurrency); err != nil {
		return err
	}
	closedRiskNanos := facts.RiskHeldNanos
	if err := checkedContributionAdd(&closedRiskNanos, facts.RiskConsumeNanos); err != nil {
		return err
	}
	if err := checkedContributionAdd(&closedRiskNanos, facts.RiskReleaseNanos); err != nil {
		return err
	}
	if facts.RiskAccrualNanos <= 0 || facts.RiskHeldNanos < 0 ||
		facts.RiskConsumeNanos < 0 || facts.RiskReleaseNanos < 0 ||
		facts.RiskAccrualNanos != closedRiskNanos {
		return errors.New("risk reserve exact-nano state does not conserve")
	}
	return nil
}

func loadContributionJobFactsBatch(
	ctx context.Context,
	tx pgx.Tx,
	jobIDs []uuid.UUID,
) (map[uuid.UUID]contributionJobFacts, error) {
	jobIDs = contributionJobIDs(jobIDs)
	factsByJob := make(map[uuid.UUID]contributionJobFacts, len(jobIDs))
	if len(jobIDs) == 0 {
		return factsByJob, nil
	}

	rows, err := tx.Query(ctx, `
		SELECT id,status,currency,pricing_decision,
		       COALESCE(pricing_decision_sha256,''),charge_batch_id,stripe_pi,
		       COALESCE(sla_guarantee_secs,0),sla_met
		  FROM jobs
		 WHERE id = ANY($1::uuid[])
		 ORDER BY id`, jobIDs)
	if err != nil {
		return nil, err
	}
	for rows.Next() {
		var (
			jobID            uuid.UUID
			status, currency string
			pricingBlob      []byte
			pricingSHA256    string
			chargeBatchID    *uuid.UUID
			stripePI         *string
			slaGuaranteeSecs int
			slaMet           *bool
		)
		if err := rows.Scan(&jobID, &status, &currency, &pricingBlob,
			&pricingSHA256, &chargeBatchID, &stripePI, &slaGuaranteeSecs, &slaMet); err != nil {
			rows.Close()
			return nil, err
		}
		facts, err := contributionFactsFromJobRow(
			jobID, status, currency, pricingBlob, pricingSHA256,
			chargeBatchID, stripePI, slaGuaranteeSecs, slaMet)
		if err != nil {
			rows.Close()
			return nil, err
		}
		factsByJob[jobID] = facts
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return nil, err
	}
	rows.Close()
	if len(factsByJob) != len(jobIDs) {
		return nil, errNotFound
	}

	// Task-bound ledger rows and job-level lifecycle rows are the same immutable
	// money facts as the single-job loader. UNION removes a duplicate only when
	// one row happens to match both paths; the old OR predicate returned it once.
	rows, err = tx.Query(ctx, `
		WITH target_jobs AS (
		  SELECT id FROM jobs WHERE id = ANY($1::uuid[])
		), ledger_rows AS (
		  SELECT t.job_id, le.id, le.created_at, le.kind,
		         (le.amount_usd*1000000000)::bigint AS amount_nanos, le.currency
		    FROM target_jobs j
		    JOIN tasks t ON t.job_id=j.id
		    JOIN ledger_entries le ON le.task_id=t.id
		  UNION
		  SELECT j.id, le.id, le.created_at, le.kind,
		         (le.amount_usd*1000000000)::bigint AS amount_nanos, le.currency
		    FROM target_jobs j
		    JOIN ledger_entries le ON
		         le.payout_ref IN (
		           'sla-premium-' || j.id::text,
		           'sla-' || j.id::text,
		           'risk-reserve-accrual-' || j.id::text,
		           'risk-reserve-release-' || j.id::text,
		           'risk-reserve-consume-' || j.id::text
		         )
		      OR le.payout_ref IN (
		           SELECT 'dispute-sla-refund-' || r.dispute_id::text
		             FROM job_dispute_refunds r
		            WHERE r.job_id=j.id
		         )
		)
		SELECT job_id,kind,amount_nanos,currency
		  FROM ledger_rows
		 ORDER BY job_id,created_at,id`, jobIDs)
	if err != nil {
		return nil, err
	}
	for rows.Next() {
		var jobID uuid.UUID
		var kind, currency string
		var amount int64
		if err := rows.Scan(&jobID, &kind, &amount, &currency); err != nil {
			rows.Close()
			return nil, err
		}
		facts, ok := factsByJob[jobID]
		if !ok {
			rows.Close()
			return nil, fmt.Errorf("ledger fact returned for unknown contribution job %s", jobID)
		}
		if err := applyContributionLedgerFact(&facts, jobID, kind, currency, amount); err != nil {
			rows.Close()
			return nil, err
		}
		factsByJob[jobID] = facts
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return nil, err
	}
	rows.Close()

	rows, err = tx.Query(ctx, `
		SELECT job_id,pricing_decision_sha256,currency,
		       accrued_nanos,held_nanos,consumed_nanos,released_nanos
		  FROM job_risk_reserves
		 WHERE job_id = ANY($1::uuid[])
		 ORDER BY job_id`, jobIDs)
	if err != nil {
		return nil, err
	}
	for rows.Next() {
		var (
			jobID                             uuid.UUID
			pricingSHA, currency              string
			accrued, held, consumed, released int64
		)
		if err := rows.Scan(&jobID, &pricingSHA, &currency,
			&accrued, &held, &consumed, &released); err != nil {
			rows.Close()
			return nil, err
		}
		facts, ok := factsByJob[jobID]
		if !ok {
			rows.Close()
			return nil, fmt.Errorf("risk reserve returned for unknown contribution job %s", jobID)
		}
		facts.RiskAccrualNanos = accrued
		facts.RiskHeldNanos = held
		facts.RiskConsumeNanos = consumed
		facts.RiskReleaseNanos = released
		if err := validateContributionRiskState(jobID, &facts, pricingSHA, currency); err != nil {
			rows.Close()
			return nil, err
		}
		factsByJob[jobID] = facts
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return nil, err
	}
	rows.Close()

	if err := loadContributionProcessorFeesBatch(ctx, tx, factsByJob, jobIDs); err != nil {
		return nil, err
	}
	if err := loadContributionObservedOutputRebatesBatch(ctx, tx, factsByJob, jobIDs); err != nil {
		return nil, err
	}

	rows, err = tx.Query(ctx, `
		SELECT liability_job_id,currency,COALESCE(SUM(amount_cents),0)::bigint
		  FROM supplier_payout_funding
		 WHERE liability_job_id = ANY($1::uuid[])
		   AND source_kind='platform_subsidy'
		 GROUP BY liability_job_id,currency
		 ORDER BY liability_job_id,currency`, jobIDs)
	if err != nil {
		return nil, err
	}
	subsidyCurrencies := make(map[uuid.UUID][]string, len(jobIDs))
	for rows.Next() {
		var jobID uuid.UUID
		var currency string
		var amountMinor int64
		if err := rows.Scan(&jobID, &currency, &amountMinor); err != nil {
			rows.Close()
			return nil, err
		}
		facts, ok := factsByJob[jobID]
		if !ok {
			rows.Close()
			return nil, fmt.Errorf("subsidy returned for unknown contribution job %s", jobID)
		}
		subsidyCurrencies[jobID] = append(subsidyCurrencies[jobID], currency)
		if err := requireContributionCurrency(jobID, facts.Currency, currency); err != nil {
			rows.Close()
			return nil, err
		}
		if err := checkedContributionAdd(&facts.SubsidyNanos, amountMinor); err != nil {
			rows.Close()
			return nil, err
		}
		factsByJob[jobID] = facts
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return nil, err
	}
	rows.Close()
	for jobID, currencies := range subsidyCurrencies {
		if len(currencies) > 1 {
			return nil, fmt.Errorf("%w: job %s subsidy funding mixes currencies", errCurrencyMismatch, jobID)
		}
		facts := factsByJob[jobID]
		currency, _ := ParseCurrency(facts.Currency)
		// The query accumulated minor units in SubsidyNanos' temporary slot;
		// convert it below after checking the exact currency, then replace it
		// with the same nano result produced by the single-job loader.
		minor := facts.SubsidyNanos
		subsidyMicros, err := currency.MinorToMicros(minor)
		if err != nil {
			return nil, err
		}
		if subsidyMicros > math.MaxInt64/NanosPerMicro {
			return nil, errMoneyOverflow
		}
		facts.SubsidyNanos = subsidyMicros * NanosPerMicro
		factsByJob[jobID] = facts
	}

	rows, err = tx.Query(ctx, `
		SELECT job_id,
		       COUNT(*) FILTER (WHERE status IN ('open','no_peer','reverifying','unresolvable'))
		  FROM disputes
		 WHERE job_id = ANY($1::uuid[])
		 GROUP BY job_id
		 ORDER BY job_id`, jobIDs)
	if err != nil {
		return nil, err
	}
	for rows.Next() {
		var jobID uuid.UUID
		var open int64
		if err := rows.Scan(&jobID, &open); err != nil {
			rows.Close()
			return nil, err
		}
		facts, ok := factsByJob[jobID]
		if !ok {
			rows.Close()
			return nil, fmt.Errorf("dispute returned for unknown contribution job %s", jobID)
		}
		facts.OpenDisputes = open
		factsByJob[jobID] = facts
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return nil, err
	}
	rows.Close()

	rows, err = tx.Query(ctx, `
		SELECT job_id,
		       (COALESCE(SUM(buyer_refund_usd),0)*1000000000)::bigint,
		       MIN(currency)
		  FROM job_dispute_refunds
		 WHERE job_id = ANY($1::uuid[])
		 GROUP BY job_id
		 ORDER BY job_id`, jobIDs)
	if err != nil {
		return nil, err
	}
	for rows.Next() {
		var jobID uuid.UUID
		var refund int64
		var currency *string
		if err := rows.Scan(&jobID, &refund, &currency); err != nil {
			rows.Close()
			return nil, err
		}
		facts, ok := factsByJob[jobID]
		if !ok {
			rows.Close()
			return nil, fmt.Errorf("dispute refund returned for unknown contribution job %s", jobID)
		}
		facts.DisputeRefundNanos = refund
		if currency != nil {
			if err := requireContributionCurrency(jobID, facts.Currency, *currency); err != nil {
				rows.Close()
				return nil, err
			}
		}
		factsByJob[jobID] = facts
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return nil, err
	}
	rows.Close()

	for jobID, facts := range factsByJob {
		if facts.BuyerRefundNanos > math.MaxInt64-facts.SLARefundNanos {
			return nil, errMoneyOverflow
		}
		factsByJob[jobID] = facts
	}
	return factsByJob, nil
}

type contributionProcessorFeeBatchRow struct {
	jobID             uuid.UUID
	chargeBatchID     *uuid.UUID
	stripePI          *string
	chargeBatchStatus *string
	feeNanos          *int64
	feeCurrency       *string
	memberCount       *int64
	allocationCount   *int64
	allocationMethods *int64
	allocatedNanos    *int64
	jobFeeNanos       *int64
	allocationMethod  *string
}

func loadContributionProcessorFeesBatch(
	ctx context.Context,
	tx pgx.Tx,
	factsByJob map[uuid.UUID]contributionJobFacts,
	jobIDs []uuid.UUID,
) error {
	rows, err := tx.Query(ctx, `
		WITH target_jobs AS (
		  SELECT id FROM jobs WHERE id = ANY($1::uuid[])
		), batch_members AS (
		  SELECT j.charge_batch_id,COUNT(*)::bigint AS member_count
		    FROM jobs j
		   WHERE j.charge_batch_id IN (
		           SELECT charge_batch_id FROM jobs
		            WHERE id = ANY($1::uuid[])
		              AND charge_batch_id IS NOT NULL
		         )
		   GROUP BY j.charge_batch_id
		), allocation_stats AS (
		  SELECT a.charge_batch_id,
		         COUNT(*)::bigint AS allocation_count,
		         COUNT(DISTINCT a.allocation_method)::bigint AS allocation_methods,
		         (COALESCE(SUM(a.allocated_fee_usd),0)*1000000000)::bigint AS allocated_nanos
		    FROM charge_batch_fee_allocations a
		   WHERE a.charge_batch_id IN (SELECT charge_batch_id FROM batch_members)
		   GROUP BY a.charge_batch_id
		)
		SELECT j.id,j.charge_batch_id,j.stripe_pi,cb.status,
		       (-fee.amount_usd*1000000000)::bigint,fee.currency,
		       bm.member_count,ast.allocation_count,ast.allocation_methods,
		       ast.allocated_nanos,
		       (a.allocated_fee_usd*1000000000)::bigint,a.allocation_method
		  FROM target_jobs ids
		  JOIN jobs j ON j.id=ids.id
		  LEFT JOIN charge_batches cb ON cb.id=j.charge_batch_id
		  LEFT JOIN ledger_entries fee
		    ON fee.kind='stripe_fee'
		   AND fee.payout_ref=CASE
		         WHEN j.charge_batch_id IS NOT NULL THEN cb.stripe_pi
		         ELSE j.stripe_pi
		       END
		  LEFT JOIN batch_members bm ON bm.charge_batch_id=j.charge_batch_id
		  LEFT JOIN allocation_stats ast ON ast.charge_batch_id=j.charge_batch_id
		  LEFT JOIN charge_batch_fee_allocations a
		    ON a.charge_batch_id=j.charge_batch_id AND a.job_id=j.id
		 ORDER BY j.id`, jobIDs)
	if err != nil {
		return err
	}
	seen := make(map[uuid.UUID]struct{}, len(jobIDs))
	for rows.Next() {
		var row contributionProcessorFeeBatchRow
		if err := rows.Scan(&row.jobID, &row.chargeBatchID, &row.stripePI,
			&row.chargeBatchStatus, &row.feeNanos, &row.feeCurrency,
			&row.memberCount, &row.allocationCount, &row.allocationMethods,
			&row.allocatedNanos, &row.jobFeeNanos, &row.allocationMethod); err != nil {
			rows.Close()
			return err
		}
		if _, duplicate := seen[row.jobID]; duplicate {
			rows.Close()
			return fmt.Errorf("multiple processor-fee authority rows for job %s", row.jobID)
		}
		seen[row.jobID] = struct{}{}
		facts, ok := factsByJob[row.jobID]
		if !ok {
			rows.Close()
			return fmt.Errorf("processor fee returned for unknown contribution job %s", row.jobID)
		}
		if row.chargeBatchID == nil {
			if row.stripePI == nil || strings.TrimSpace(*row.stripePI) == "" || row.feeNanos == nil {
				facts.ProcessorFeeNanos = nil
				facts.ProcessorFeeSource = ""
			} else {
				if *row.feeNanos < 0 {
					rows.Close()
					return fmt.Errorf("job %s processor fee has an invalid sign", row.jobID)
				}
				if row.feeCurrency == nil {
					rows.Close()
					return fmt.Errorf("%w: job %s processor fee has no currency", errCurrencyMismatch, row.jobID)
				}
				if err := requireContributionCurrency(row.jobID, facts.Currency, *row.feeCurrency); err != nil {
					rows.Close()
					return err
				}
				fee := *row.feeNanos
				facts.ProcessorFeeNanos = &fee
				facts.ProcessorFeeSource = "ledger_entries.stripe_fee:payout_ref"
			}
			factsByJob[row.jobID] = facts
			continue
		}

		// A batch that is not durably charged, or a charged batch whose provider
		// fee row is absent, has no settled processor fee yet. This matches the
		// single-job inner-join query and does not manufacture zero authority.
		if row.chargeBatchStatus == nil || *row.chargeBatchStatus != "charged" || row.feeNanos == nil {
			facts.ProcessorFeeNanos = nil
			facts.ProcessorFeeSource = ""
			factsByJob[row.jobID] = facts
			continue
		}
		if row.feeCurrency == nil || row.memberCount == nil ||
			row.allocationCount == nil || row.allocationMethods == nil ||
			row.allocatedNanos == nil || row.jobFeeNanos == nil ||
			row.allocationMethod == nil {
			rows.Close()
			return fmt.Errorf("job %s charge batch processor-fee allocation is incomplete or inconsistent", row.jobID)
		}
		if err := requireContributionCurrency(row.jobID, facts.Currency, *row.feeCurrency); err != nil {
			rows.Close()
			return err
		}
		if *row.memberCount <= 0 || *row.allocationCount != *row.memberCount ||
			*row.allocationMethods != 1 ||
			(*row.allocationMethod != batchFeeAllocationHamiltonV1 &&
				*row.allocationMethod != batchFeeAllocationLegacyV0) ||
			*row.feeNanos < 0 || *row.allocatedNanos != *row.feeNanos ||
			*row.jobFeeNanos < 0 {
			rows.Close()
			return fmt.Errorf("job %s charge batch processor-fee allocation is incomplete or inconsistent", row.jobID)
		}
		fee := *row.jobFeeNanos
		facts.ProcessorFeeNanos = &fee
		facts.ProcessorFeeSource = "charge_batch_fee_allocations:" + *row.allocationMethod
		factsByJob[row.jobID] = facts
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return err
	}
	rows.Close()
	if len(seen) != len(jobIDs) {
		return errNotFound
	}
	return nil
}

func loadContributionObservedOutputRebatesBatch(
	ctx context.Context,
	tx pgx.Tx,
	factsByJob map[uuid.UUID]contributionJobFacts,
	jobIDs []uuid.UUID,
) error {
	rows, err := tx.Query(ctx, `
		SELECT t.id,
		       t.economic_buyer_charge_usd::float8,
		       t.economic_supplier_payout_usd::float8,
		       t.economic_buyer_charge_nanos,
		       t.economic_supplier_payout_nanos,
		       COALESCE(t.expected_output_records,0),
		       t.reported_tokens_used,
		       j.id,
		       j.workload_decision,
		       COALESCE(j.workload_decision_sha256,''),
		       j.compute_plan,
		       COALESCE(j.compute_plan_sha256,''),
		       ep.plan_json
		  FROM tasks t
		  JOIN jobs j ON j.id=t.job_id
		  LEFT JOIN job_economic_plans ep ON ep.job_id=j.id
		 WHERE t.job_id = ANY($1::uuid[])
		   AND COALESCE(t.expected_output_records,0) > 0
		   AND t.reported_tokens_used IS NOT NULL
		   AND t.economic_buyer_charge_usd IS NOT NULL
		 ORDER BY t.job_id,t.id`, jobIDs)
	if err != nil {
		return err
	}
	taskFactsByJob := make(map[uuid.UUID][]observedOutputSettlementTaskFacts, len(jobIDs))
	for rows.Next() {
		var facts observedOutputSettlementTaskFacts
		if err := rows.Scan(
			&facts.taskID,
			&facts.frozenCharge,
			&facts.frozenPayout,
			&facts.buyerNanos,
			&facts.supplierNanos,
			&facts.expectedRecords,
			&facts.reportedTokens,
			&facts.jobID,
			&facts.workloadJSON,
			&facts.workloadSHA256,
			&facts.computePlanJSON,
			&facts.computePlanSHA256,
			&facts.economicPlanJSON,
		); err != nil {
			rows.Close()
			return err
		}
		taskFactsByJob[facts.jobID] = append(taskFactsByJob[facts.jobID], facts)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return err
	}
	rows.Close()
	observedJobIDs := make([]uuid.UUID, 0, len(taskFactsByJob))
	for _, jobID := range jobIDs {
		if len(taskFactsByJob[jobID]) > 0 {
			observedJobIDs = append(observedJobIDs, jobID)
		}
	}
	economicPlans, err := loadContributionEconomicPlansBatch(ctx, tx, observedJobIDs)
	if err != nil {
		return err
	}
	for _, jobID := range jobIDs {
		facts := factsByJob[jobID]
		rebate, err := reduceContributionObservedOutputRebateTaskFacts(
			ctx, tx, jobID, taskFactsByJob[jobID], economicPlans[jobID])
		if err != nil {
			return err
		}
		facts.ObservedOutputRebateNanos = rebate
		factsByJob[jobID] = facts
	}
	return nil
}

func reduceContributionObservedOutputRebateTaskFacts(
	ctx context.Context,
	tx pgx.Tx,
	jobID uuid.UUID,
	taskFacts []observedOutputSettlementTaskFacts,
	preloadedEconomicPlan *EconomicPlan,
) (int64, error) {
	economicPlan := preloadedEconomicPlan
	for _, facts := range taskFacts {
		if facts.jobID != jobID {
			return 0, fmt.Errorf("observed-output task %s belongs to job %s, want %s",
				facts.taskID, facts.jobID, jobID)
		}
		if economicPlan == nil && len(facts.economicPlanJSON) > 0 {
			plan, _, err := assertDenormalizedEconomicPlanMoney(ctx, tx, jobID)
			if err != nil {
				return 0, err
			}
			economicPlan = &plan
			break
		}
	}

	var total int64
	for _, facts := range taskFacts {
		settled, err := settleObservedOutputSettlementTaskFacts(
			ctx, tx, facts, economicPlan)
		if err != nil {
			return 0, fmt.Errorf("load task %s observed-output settlement: %w", facts.taskID, err)
		}
		if !settled.Applied {
			continue
		}
		var rebate int64
		if settled.HasNanos && facts.buyerNanos != nil {
			rebate = *facts.buyerNanos - settled.BilledChargeNanos
		} else {
			rebate = usdToMicros(settled.RebateUSD) * NanosPerMicro
		}
		if rebate < 0 {
			return 0, fmt.Errorf("task %s observed-output settlement exceeds its accepted charge", facts.taskID)
		}
		if err := checkedContributionAdd(&total, rebate); err != nil {
			return 0, err
		}
	}
	return total, nil
}

func loadContributionEconomicPlansBatch(
	ctx context.Context,
	tx pgx.Tx,
	jobIDs []uuid.UUID,
) (map[uuid.UUID]*EconomicPlan, error) {
	plans := make(map[uuid.UUID]*EconomicPlan, len(jobIDs))
	if len(jobIDs) == 0 {
		return plans, nil
	}
	rows, err := tx.Query(ctx, `
		SELECT job_id,plan_json,
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
		 WHERE job_id = ANY($1::uuid[])
		 ORDER BY job_id`, jobIDs)
	if err != nil {
		return nil, err
	}
	for rows.Next() {
		var (
			jobID         uuid.UUID
			planJSON      []byte
			denorm        denormalizedEconomicPlanMoney
			firmQuote     *float64
			buyerNanos    *int64
			supplierNanos *int64
		)
		if err := rows.Scan(&jobID, &planJSON,
			&denorm.InitialTaskCount,
			&denorm.BuyerChargePerTaskUSD,
			&denorm.SupplierPayoutPerTaskUSD,
			&denorm.InitialBuyerChargeUSD,
			&denorm.ReservedBuyerChargeUSD,
			&denorm.SLAPremiumUSD,
			&firmQuote,
			&buyerNanos,
			&supplierNanos); err != nil {
			rows.Close()
			return nil, err
		}
		if _, duplicate := plans[jobID]; duplicate {
			rows.Close()
			return nil, fmt.Errorf("multiple economic plan authority rows for job %s", jobID)
		}
		denorm.FirmQuoteMaxUSD = firmQuote
		denorm.BuyerChargePerTaskNanos = buyerNanos
		denorm.SupplierPayoutPerTaskNanos = supplierNanos
		plan, err := validateDenormalizedEconomicPlanMoney(jobID, planJSON, denorm)
		if err != nil {
			rows.Close()
			return nil, err
		}
		planCopy := plan
		plans[jobID] = &planCopy
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return nil, err
	}
	rows.Close()
	return plans, nil
}

func loadContributionObservedOutputRebateForJob(
	ctx context.Context,
	tx pgx.Tx,
	jobID uuid.UUID,
) (int64, error) {
	rows, err := tx.Query(ctx, `
		SELECT t.id,
		       t.economic_buyer_charge_usd::float8,
		       t.economic_supplier_payout_usd::float8,
		       t.economic_buyer_charge_nanos,
		       t.economic_supplier_payout_nanos,
		       COALESCE(t.expected_output_records,0),
		       t.reported_tokens_used,
		       j.id,
		       j.workload_decision,
		       COALESCE(j.workload_decision_sha256,''),
		       j.compute_plan,
		       COALESCE(j.compute_plan_sha256,''),
		       ep.plan_json
		  FROM tasks t
		  JOIN jobs j ON j.id=t.job_id
		  LEFT JOIN job_economic_plans ep ON ep.job_id=j.id
		 WHERE t.job_id=$1
		   AND COALESCE(t.expected_output_records,0) > 0
		   AND t.reported_tokens_used IS NOT NULL
		   AND t.economic_buyer_charge_usd IS NOT NULL
		 ORDER BY t.id`, jobID)
	if err != nil {
		return 0, err
	}
	var taskFacts []observedOutputSettlementTaskFacts
	for rows.Next() {
		var facts observedOutputSettlementTaskFacts
		if err := rows.Scan(
			&facts.taskID,
			&facts.frozenCharge,
			&facts.frozenPayout,
			&facts.buyerNanos,
			&facts.supplierNanos,
			&facts.expectedRecords,
			&facts.reportedTokens,
			&facts.jobID,
			&facts.workloadJSON,
			&facts.workloadSHA256,
			&facts.computePlanJSON,
			&facts.computePlanSHA256,
			&facts.economicPlanJSON,
		); err != nil {
			rows.Close()
			return 0, err
		}
		taskFacts = append(taskFacts, facts)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return 0, err
	}
	rows.Close()

	return reduceContributionObservedOutputRebateTaskFacts(ctx, tx, jobID, taskFacts, nil)
}

func (s *Store) contributionSettlementsForJobs(
	ctx context.Context,
	jobIDs []uuid.UUID,
) (map[uuid.UUID]*ContributionSettlement, error) {
	jobIDs = contributionJobIDs(jobIDs)
	out := make(map[uuid.UUID]*ContributionSettlement, len(jobIDs))
	if len(jobIDs) == 0 {
		return out, nil
	}
	tx, err := s.pool.BeginTx(ctx, pgx.TxOptions{
		IsoLevel: pgx.RepeatableRead, AccessMode: pgx.ReadOnly,
	})
	if err != nil {
		return nil, err
	}
	defer tx.Rollback(ctx)
	factsByJob, err := loadContributionJobFactsBatch(ctx, tx, jobIDs)
	if err != nil {
		return nil, err
	}
	for _, jobID := range jobIDs {
		facts, ok := factsByJob[jobID]
		if !ok {
			return nil, errNotFound
		}
		settlement, err := reduceContributionJobFacts(jobID, facts)
		if err != nil {
			return nil, err
		}
		settlementCopy := settlement
		out[jobID] = &settlementCopy
	}
	if err := tx.Commit(ctx); err != nil {
		return nil, err
	}
	return out, nil
}
