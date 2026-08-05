package main

import (
	"context"
	_ "embed"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

// Split out of store.go, which had grown to 5,727 lines across roughly two
// dozen unrelated responsibilities.  Same package, same behaviour: this is a
// file move so that a reviewer can hold one subject at a time and two people
// can edit payouts and job submission without conflicting.

func (s *Store) GetBillingCustomer(ctx context.Context, buyerID uuid.UUID) (custID, pm string, err error) {
	err = s.pool.QueryRow(ctx,
		`SELECT COALESCE(stripe_customer_id,''), COALESCE(default_payment_method,'')
		   FROM billing_customers WHERE buyer_id=$1`, buyerID).Scan(&custID, &pm)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", "", errNotFound
	}
	return custID, pm, err
}

func (s *Store) UpsertBillingCustomer(ctx context.Context, buyerID uuid.UUID, custID string) error {
	_, err := s.pool.Exec(ctx,
		`INSERT INTO billing_customers (buyer_id, stripe_customer_id) VALUES ($1, $2)
		   ON CONFLICT (buyer_id) DO UPDATE
		   SET stripe_customer_id = EXCLUDED.stripe_customer_id, updated_at = now()`,
		buyerID, custID)
	return err
}

func (s *Store) SetBillingPMByCustomer(ctx context.Context, custID, pm string) error {
	tag, err := s.pool.Exec(ctx,
		`UPDATE billing_customers
		    SET default_payment_method=$2, updated_at=now()
		  WHERE stripe_customer_id=$1`, custID, pm)
	if err != nil {
		return err
	}
	return validateBillingPMUpdateCount(tag.RowsAffected())
}

func validateBillingPMUpdateCount(rows int64) error {
	switch rows {
	case 1:
		return nil
	case 0:
		return errNotFound
	default:
		return fmt.Errorf("billing customer mapping matched %d rows, want exactly one", rows)
	}
}

func (s *Store) JobChargeInfo(ctx context.Context, jobID uuid.UUID) (buyerID uuid.UUID, chargeUSD float64, err error) {
	var actualUSD float64
	var firmQuote bool
	var firmMax float64
	var slaRefund float64
	var prepaidCoverage float64
	var currency string
	err = s.pool.QueryRow(ctx,
		`SELECT buyer_id,currency,COALESCE(actual_usd,0),firm_quote,COALESCE(firm_quote_max_usd,0),
		        COALESCE((SELECT SUM(le.amount_usd) FROM ledger_entries le
		                  WHERE le.kind = 'sla_refund'
		                    AND le.payout_ref = 'sla-' || jobs.id::text), 0)::float8,
		        COALESCE((SELECT SUM(-le.amount_usd) FROM ledger_entries le
		                  WHERE le.kind = 'prepaid_debit'
		                    AND (le.task_id IN (SELECT id FROM tasks WHERE job_id=jobs.id)
		                         OR le.payout_ref = 'prepaid-sla-' || jobs.id::text)), 0)::float8
		   FROM jobs WHERE id=$1`,
		jobID).Scan(&buyerID, &currency, &actualUSD, &firmQuote, &firmMax, &slaRefund, &prepaidCoverage)
	if errors.Is(err, pgx.ErrNoRows) {
		err = errNotFound
		return
	}
	if err != nil {
		return
	}
	if err = RequireSettlementCurrency(currency); err != nil {
		err = fmt.Errorf("job %s cannot be charged under this deployment: %w", jobID, err)
		return
	}
	chargeUSD = actualUSD
	if firmQuote && firmMax > 0 && actualUSD > firmMax {
		chargeUSD = firmMax
	}
	if slaRefund > 0 {
		chargeUSD -= slaRefund
		if chargeUSD < 0 {
			chargeUSD = 0 // the remedy nets the bill down, never into a negative charge
		}
	}
	if prepaidCoverage > 0 {
		chargeUSD -= prepaidCoverage
		if chargeUSD < 0 {
			chargeUSD = 0
		}
	}
	return
}

func (s *Store) InsertLedgerEntries(ctx context.Context, entries []LedgerEntry) error {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	for _, e := range entries {
		if _, err = insertLedgerEntryOnTaskConflictDoNothingTx(ctx, tx, ledgerInsertFromEntry(e)); err != nil {
			return err
		}
	}
	return tx.Commit(ctx)
}

func (s *Store) SetChargeStatus(ctx context.Context, jobID uuid.UUID, status string) error {
	_, err := s.pool.Exec(ctx, `UPDATE jobs SET charge_status = $2 WHERE id = $1`, jobID, status)
	return err
}

func slaPremiumChargeRef(jobID uuid.UUID) string { return "sla-premium-" + jobID.String() }

func (s *Store) EnsureJobSLAPremiumCharge(ctx context.Context, jobID uuid.UUID) error {
	_, err := insertJobSLAPremiumChargeTx(ctx, s.pool, jobID, slaPremiumChargeRef(jobID))
	return err
}

type InvoiceView struct {
	JobID     uuid.UUID `json:"job_id"`
	BuyerID   uuid.UUID `json:"buyer_id"`
	Status    string    `json:"status"`
	JobType   string    `json:"job_type"`
	CreatedAt time.Time `json:"created_at"`
	// Currency is the settlement currency of this invoice's cash (ISO code).
	// Historical rows keep the currency they settled in.
	Currency        string  `json:"currency"`
	EstimatedUSD    float64 `json:"estimated_usd"`
	ActualUSD       float64 `json:"actual_usd"`
	ChargedUSD      float64 `json:"charged_usd"`
	SupplierPaidUSD float64 `json:"supplier_credit_usd"`
	PlatformTakeUSD float64 `json:"platform_take_usd"`
	// PlatformGrossSpreadUSD is the honest name for the legacy platform_take
	// ledger row: buyer charge less supplier entitlement, before Merc's costs.
	// It is not profit. Contribution separates every cost status and only emits a
	// true net amount when none is unknown.
	PlatformGrossSpreadUSD float64                   `json:"platform_gross_spread_usd"`
	Contribution           *EconomicContributionView `json:"contribution,omitempty"`
	// BuyerRefundUSD is the positive sum of buyer_refund ledger rows for this
	// job (task-scoped plus dispute-keyed SLA premium refunds). Zero when none.
	BuyerRefundUSD float64 `json:"buyer_refund_usd,omitempty"`
	// NetChargedUSD is gross charged minus buyer refunds, both in the job
	// currency. Present when the ledger has charge and/or refund rows.
	NetChargedUSD *float64 `json:"net_charged_usd,omitempty"`
	// RefundCause is a buyer-visible reason when a dispute (or other) refund
	// exists — e.g. "dispute_upheld". Empty when no refund.
	RefundCause string `json:"refund_cause,omitempty"`
	// RefundFundingDestination records where the refund went:
	// prepaid_balance | external_card_pending | ledger_only.
	RefundFundingDestination string   `json:"refund_funding_destination,omitempty"`
	QuotedUSD                *float64 `json:"quoted_usd,omitempty"`
	FirmQuote                bool     `json:"firm_quote,omitempty"`
	FirmQuoteMaxUSD          *float64 `json:"firm_quote_max_usd,omitempty"`
	BilledUSD                *float64 `json:"billed_usd,omitempty"`
	// ProcessorFeeAllocatedUSD is reconciliation attribution, not an
	// additional buyer charge. Batch fees appear only after the complete,
	// conserved allocation is durable.
	ProcessorFeeAllocatedUSD     *float64 `json:"processor_fee_allocated_usd,omitempty"`
	ProcessorFeeAllocationMethod *string  `json:"processor_fee_allocation_method,omitempty"`
	// PlatformNetAfterProcessorUSD makes the gross platform-take convention
	// explicit whenever the processor fee is known.
	PlatformNetAfterProcessorUSD *float64 `json:"platform_net_after_processor_usd,omitempty"`
	SLAGuaranteeSecs             int      `json:"sla_guarantee_secs,omitempty"`
	SLAPremiumUSD                *float64 `json:"sla_premium_usd,omitempty"`
	SLARefundUSD                 *float64 `json:"sla_refund_usd,omitempty"`
	SLAMet                       *bool    `json:"sla_met,omitempty"`
	// Generative batch observed-output settlement evidence (job totals).
	// Present when any task carried an output-token ceiling so the buyer can
	// audit ceiling vs observed vs rebate without a second API.
	OutputTokenCeiling   *int64   `json:"output_token_ceiling,omitempty"`
	ObservedOutputTokens *int64   `json:"observed_output_tokens,omitempty"`
	FrozenBuyerChargeUSD *float64 `json:"frozen_buyer_charge_usd,omitempty"`
	OutputTokenRebateUSD *float64 `json:"output_token_rebate_usd,omitempty"`
}

type ContributionComponentView struct {
	Status    string   `json:"status"`
	AmountUSD *float64 `json:"amount_usd,omitempty"`
	Basis     string   `json:"basis"`
}

type EconomicContributionView struct {
	Currency                     string                    `json:"currency"`
	SupplierExecutionEntitlement ContributionComponentView `json:"supplier_execution_entitlement"`
	VerificationEntitlement      ContributionComponentView `json:"verification_supplier_entitlement"`
	CloudProviderCost            ContributionComponentView `json:"cloud_provider_cost"`
	StorageEgressCost            ContributionComponentView `json:"storage_egress_cost"`
	// StorageCost and EgressCost are reported separately so a receipt can state
	// which of the two was modeled, not applicable, or unknown.
	StorageCost                 ContributionComponentView `json:"storage_cost"`
	EgressCost                  ContributionComponentView `json:"egress_cost"`
	PaymentProcessingAllocation ContributionComponentView `json:"payment_processing_allocation"`
	FraudRefundReserve          ContributionComponentView `json:"fraud_refund_reserve"`
	AllocatedControlPlaneCost   ContributionComponentView `json:"allocated_control_plane_cost"`
	MercGrossSpread             ContributionComponentView `json:"merc_gross_spread"`
	MercNetContribution         ContributionComponentView `json:"merc_net_contribution"`
	// TrueNetContributionNanos is set only when every named cost is modeled or
	// not_applicable (no unknowns). It is the integer authority; the USD view
	// above is the display projection.
	TrueNetContributionNanos *int64 `json:"true_net_contribution_nanos,omitempty"`
	// CostComponentStatuses lists every component's status for per-request,
	// per-account, per-workload and per-invoice queries.
	CostComponentStatuses map[string]string `json:"cost_component_statuses,omitempty"`
	// CostScheduleRevision / CostScheduleSHA256 identify the policy rates that
	// produced modeled storage/egress/risk. Empty on historical decisions.
	CostScheduleRevision string                    `json:"cost_schedule_revision,omitempty"`
	CostScheduleSHA256   string                    `json:"cost_schedule_sha256,omitempty"`
	Subsidy              ContributionComponentView `json:"subsidy"`
}

func contributionAmount(status string, amount float64, basis string) ContributionComponentView {
	value := roundEconomicUSD(amount)
	return ContributionComponentView{Status: status, AmountUSD: &value, Basis: basis}
}

func contributionUnknown(basis string) ContributionComponentView {
	return ContributionComponentView{Status: pricingCostUnknown, Basis: basis}
}

func pricingContributionComponent(c PricingCostComponent) ContributionComponentView {
	if c.Status == pricingCostUnknown {
		return contributionUnknown(c.Basis)
	}
	return contributionAmount(c.Status, c.Amount, c.Basis)
}

func buildEconomicContributionView(
	pricing *PricingDecision,
	currency string,
	grossSpread float64,
	processorFee *float64,
	subsidyUSD float64,
) *EconomicContributionView {
	if pricing == nil {
		return nil
	}
	out := &EconomicContributionView{
		Currency:                     currency,
		SupplierExecutionEntitlement: pricingContributionComponent(pricing.PrimarySupplierCost),
		VerificationEntitlement:      pricingContributionComponent(pricing.VerificationCost),
		CloudProviderCost:            pricingContributionComponent(pricing.ProviderCost),
		StorageCost:                  pricingContributionComponent(pricing.StorageCost),
		EgressCost:                   pricingContributionComponent(pricing.EgressCost),
		FraudRefundReserve:           pricingContributionComponent(pricing.RiskReserve),
		AllocatedControlPlaneCost:    pricingContributionComponent(pricing.ControlPlaneCost),
		CostScheduleRevision:         pricing.CostScheduleRevision,
		CostScheduleSHA256:           pricing.CostScheduleSHA256,
		MercGrossSpread: contributionAmount("settled", grossSpread,
			"buyer settlement less supplier settlement; gross spread, not profit"),
		Subsidy: contributionAmount("settled", subsidyUSD,
			"authorized platform-subsidy funding attributed to this job"),
		CostComponentStatuses: map[string]string{
			"primary_supplier":      pricing.PrimarySupplierCost.Status,
			"verification":          pricing.VerificationCost.Status,
			"payment":               pricing.PaymentCost.Status,
			"control_plane":         pricing.ControlPlaneCost.Status,
			"storage":               pricing.StorageCost.Status,
			"egress":                pricing.EgressCost.Status,
			"provider":              pricing.ProviderCost.Status,
			"risk_reserve":          pricing.RiskReserve.Status,
			"platform_contribution": pricing.PlatformContribution.Status,
		},
	}
	// Storage and egress stay separate in PricingDecision because they have
	// different evidence, but the commercial receipt also reports their sum as
	// the externally requested category when neither is unknown.
	if pricing.StorageCost.Status == pricingCostUnknown ||
		pricing.EgressCost.Status == pricingCostUnknown {
		out.StorageEgressCost = contributionUnknown(
			"storage or egress cost is not yet independently attributed")
	} else {
		status := "modeled"
		if pricing.StorageCost.Status == pricingCostNotApplicable &&
			pricing.EgressCost.Status == pricingCostNotApplicable {
			status = pricingCostNotApplicable
		}
		out.StorageEgressCost = contributionAmount(status,
			pricing.StorageCost.Amount+pricing.EgressCost.Amount,
			"frozen storage plus egress allocation")
	}
	if processorFee != nil {
		out.PaymentProcessingAllocation = contributionAmount("settled", *processorFee,
			"immutable processor fee allocated from the collected charge")
	} else if pricing.PaymentCost.Status == pricingCostModeled {
		// Acceptance modeled the processor fee from the economic schedule; cash
		// reconciliation may still be pending. Prefer the modeled amount so a
		// complete cost schedule can report true net before Stripe settles.
		out.PaymentProcessingAllocation = pricingContributionComponent(pricing.PaymentCost)
	} else {
		out.PaymentProcessingAllocation = contributionUnknown(
			"processor cash fee has not yet been reconciled and allocated")
	}

	// Prefer the frozen fixed-point true net when the decision carries it. That
	// is the integer authority; do not re-derive a historical decision under a
	// later schedule.
	if pricing.FixedPoint != nil && pricing.FixedPoint.TrueNetContributionNanos != nil {
		// The fixed-point unknown set predates the frozen runtime cell and names
		// only four categories: storage, egress, provider, risk reserve. The cell
		// block also tracks energy allocation and reliability retry overhead, and
		// refuses a true net when either is unknown.
		//
		// When they disagree, the receipt contradicts itself: runtime_cell says
		// REFUSED_UNKNOWN_COST_CATEGORIES while this branch publishes a settled
		// amount described as "after every named cost" -- and because
		// known_variable_costs_nanos is 0 on that path, the amount it publishes is
		// the gross spread, unreduced. That is the gross-ledger-row-as-profit
		// defect the contribution view exists to prevent, emitted by the
		// contribution view itself.
		//
		// The stricter authority wins. Refuse rather than publish a number the
		// same document declines to compute.
		if cell := pricing.RuntimeCell; cell != nil && !cell.MercTrueNetAvailable() {
			out.MercNetContribution = contributionUnknown(
				"frozen runtime cell refuses a true net: " + cell.MercTrueNetStatus)
			return out
		}
		trueNet := *pricing.FixedPoint.TrueNetContributionNanos
		out.TrueNetContributionNanos = &trueNet
		out.MercNetContribution = contributionAmount("settled",
			microsToUSD(trueNet/NanosPerMicro)-subsidyUSD,
			"true net contribution from frozen fixed-point authority after every named cost")
		return out
	}

	known := processorFee != nil || pricing.PaymentCost.Status == pricingCostModeled
	knownCost := subsidyUSD
	if processorFee != nil {
		knownCost += *processorFee
	} else if pricing.PaymentCost.Status == pricingCostModeled {
		knownCost += pricing.PaymentCost.Amount
	}
	for _, component := range []PricingCostComponent{
		pricing.ControlPlaneCost, pricing.StorageCost, pricing.EgressCost,
		pricing.ProviderCost, pricing.RiskReserve,
	} {
		if component.Status == pricingCostUnknown {
			known = false
			continue
		}
		if component.Status == pricingCostModeled {
			knownCost += component.Amount
		}
	}
	if known {
		out.MercNetContribution = contributionAmount("settled",
			grossSpread-knownCost,
			"gross spread less processor, control, storage, egress, provider, risk and subsidy costs")
	} else {
		out.MercNetContribution = contributionUnknown(
			"true net contribution is unavailable until every cost category is attributed")
	}
	return out
}

// AccountTrueNetContribution sums true-net nanos across an account's jobs that
// carry a populated TrueNetContributionNanos. Jobs with unknown categories are
// counted in UnknownJobs rather than silently zeroed.
type AccountTrueNetContribution struct {
	AccountID                uuid.UUID `json:"account_id"`
	Currency                 string    `json:"currency"`
	TrueNetContributionNanos int64     `json:"true_net_contribution_nanos"`
	JobsWithTrueNet          int       `json:"jobs_with_true_net"`
	JobsWithUnknownCosts     int       `json:"jobs_with_unknown_costs"`
	JobsWithoutDecision      int       `json:"jobs_without_pricing_decision"`
}

// WorkloadClassTrueNetContribution is the same rollup keyed by workload class.
type WorkloadClassTrueNetContribution struct {
	WorkloadClass            string `json:"workload_class"`
	Currency                 string `json:"currency"`
	TrueNetContributionNanos int64  `json:"true_net_contribution_nanos"`
	JobsWithTrueNet          int    `json:"jobs_with_true_net"`
	JobsWithUnknownCosts     int    `json:"jobs_with_unknown_costs"`
}

// TrueNetContributionForAccount rolls frozen true-net figures for a buyer.
// Historical decisions without true net are counted, not re-derived.
func (s *Store) TrueNetContributionForAccount(
	ctx context.Context, accountID uuid.UUID,
) (AccountTrueNetContribution, error) {
	out := AccountTrueNetContribution{AccountID: accountID, Currency: SettlementCurrencyCode()}
	rows, err := s.pool.Query(ctx, `
		SELECT pricing_decision
		  FROM jobs
		 WHERE buyer_id = $1 AND pricing_decision IS NOT NULL`, accountID)
	if err != nil {
		return out, err
	}
	defer rows.Close()
	for rows.Next() {
		var blob []byte
		if err := rows.Scan(&blob); err != nil {
			return out, err
		}
		var p PricingDecision
		if err := json.Unmarshal(blob, &p); err != nil {
			return out, err
		}
		if p.FixedPoint == nil {
			out.JobsWithoutDecision++
			continue
		}
		if p.FixedPoint.TrueNetContributionNanos != nil {
			out.TrueNetContributionNanos += *p.FixedPoint.TrueNetContributionNanos
			out.JobsWithTrueNet++
			if p.Currency != "" {
				out.Currency = p.Currency
			}
		} else {
			out.JobsWithUnknownCosts++
		}
	}
	return out, rows.Err()
}

// TrueNetContributionForWorkloadClass rolls frozen true-net by workload class.
func (s *Store) TrueNetContributionForWorkloadClass(
	ctx context.Context, workloadClass string,
) (WorkloadClassTrueNetContribution, error) {
	out := WorkloadClassTrueNetContribution{
		WorkloadClass: workloadClass, Currency: SettlementCurrencyCode(),
	}
	rows, err := s.pool.Query(ctx, `
		SELECT pricing_decision
		  FROM jobs
		 WHERE COALESCE(workload_decision->>'workload_class','') = $1
		   AND pricing_decision IS NOT NULL`, workloadClass)
	if err != nil {
		return out, err
	}
	defer rows.Close()
	for rows.Next() {
		var blob []byte
		if err := rows.Scan(&blob); err != nil {
			return out, err
		}
		var p PricingDecision
		if err := json.Unmarshal(blob, &p); err != nil {
			return out, err
		}
		if p.FixedPoint != nil && p.FixedPoint.TrueNetContributionNanos != nil {
			out.TrueNetContributionNanos += *p.FixedPoint.TrueNetContributionNanos
			out.JobsWithTrueNet++
			if p.Currency != "" {
				out.Currency = p.Currency
			}
		} else {
			out.JobsWithUnknownCosts++
		}
	}
	return out, rows.Err()
}

func (s *Store) JobInvoice(ctx context.Context, jobID, buyerID uuid.UUID) (*InvoiceView, error) {
	iv := InvoiceView{JobID: jobID}
	var firmMax, billed *float64
	var chargeBatchID *uuid.UUID
	var stripePI *string
	var slaGuarantee int
	var slaPremium *float64
	err := s.pool.QueryRow(ctx,
		`SELECT buyer_id,status,job_type,created_at,currency,
		        COALESCE(estimated_usd,0), COALESCE(actual_usd,0),
		        firm_quote, firm_quote_max_usd, billed_usd,
		        charge_batch_id, stripe_pi,
		        COALESCE(sla_guarantee_secs,0), sla_premium_usd, sla_met
		 FROM jobs WHERE id = $1 AND buyer_id = $2`,
		jobID, buyerID,
	).Scan(&iv.BuyerID, &iv.Status, &iv.JobType, &iv.CreatedAt, &iv.Currency,
		&iv.EstimatedUSD, &iv.ActualUSD,
		&iv.FirmQuote, &firmMax, &billed, &chargeBatchID, &stripePI,
		&slaGuarantee, &slaPremium, &iv.SLAMet)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, errNotFound
	}
	if err != nil {
		return nil, err
	}
	iv.FirmQuoteMaxUSD = firmMax
	iv.BilledUSD = billed
	iv.SLAGuaranteeSecs = slaGuarantee
	iv.SLAPremiumUSD = slaPremium
	processorFee, allocationMethod, err := s.jobProcessorFeeAllocatedUSD(
		ctx, jobID, chargeBatchID, stripePI,
	)
	if err != nil {
		return nil, err
	}
	iv.ProcessorFeeAllocatedUSD = processorFee
	iv.ProcessorFeeAllocationMethod = allocationMethod
	if slaGuarantee > 0 {
		var refund float64
		if rerr := s.pool.QueryRow(ctx,
			`SELECT COALESCE(SUM(amount_usd),0)::float8 FROM ledger_entries
			  WHERE kind = 'sla_refund' AND payout_ref = $1`,
			"sla-"+jobID.String()).Scan(&refund); rerr != nil {
			return nil, rerr
		}
		if refund > 0 {
			iv.SLARefundUSD = &refund
		}
	}
	rows, err := s.pool.Query(ctx,
		`SELECT le.kind, COALESCE(SUM(le.amount_usd),0), le.currency
		 FROM ledger_entries le JOIN tasks t ON t.id = le.task_id
		 WHERE t.job_id = $1 GROUP BY le.kind, le.currency`, jobID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var chargeLedger, refundLedger float64
	hasChargeOrRefundLedger := false
	for rows.Next() {
		var kind, currency string
		var amt float64
		if err := rows.Scan(&kind, &amt, &currency); err != nil {
			return nil, err
		}
		if iv.Currency == "" {
			iv.Currency = currency
		} else if currency != "" && currency != iv.Currency {
			// Distinct currencies on one job's ledger is a configuration bug;
			// surface the first and refuse to silently sum across them.
			return nil, fmt.Errorf("%w: job %s ledger mixes %s and %s",
				errCurrencyMismatch, jobID, iv.Currency, currency)
		}
		switch kind {
		case "supplier_credit", "clawback":
			iv.SupplierPaidUSD += amt // clawback is negative -> reduces net paid
		case "platform_take", "platform_refund":
			// platform_refund is negative and reduces net platform take
			iv.PlatformTakeUSD += amt
		case "buyer_charge":
			chargeLedger += amt
			hasChargeOrRefundLedger = true
		case "buyer_refund":
			refundLedger += amt
			hasChargeOrRefundLedger = true
		}
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	// Job-level SLA premium charge/refund rows have no task_id and are not
	// joined above. Include them so the receipt shows the full buyer picture.
	var slaCharge, slaRefund float64
	if err := s.pool.QueryRow(ctx, `
		SELECT
		  COALESCE(SUM(amount_usd) FILTER (WHERE kind='buyer_charge'),0)::float8,
		  COALESCE(SUM(amount_usd) FILTER (WHERE kind='buyer_refund'),0)::float8
		  FROM ledger_entries
		 WHERE task_id IS NULL AND buyer_id = $1
		   AND (
		     payout_ref = $2
		     OR payout_ref LIKE 'dispute-sla-refund-%'
		   )`, buyerID, slaPremiumChargeRef(jobID)).
		Scan(&slaCharge, &slaRefund); err != nil {
		return nil, err
	}
	if slaCharge != 0 || slaRefund != 0 {
		chargeLedger += slaCharge
		refundLedger += slaRefund
		hasChargeOrRefundLedger = true
	}

	// ChargedUSD historically held the signed sum of buyer_charge (negative).
	// Preserve that assignment when ledger charges exist so existing clients
	// that inspect the signed total still see the gross charge rows; when no
	// charge rows exist, fall back to actual/estimated as before.
	if chargeLedger != 0 {
		iv.ChargedUSD = chargeLedger
	} else if iv.ChargedUSD == 0 {
		if iv.ActualUSD > 0 {
			iv.ChargedUSD = iv.ActualUSD
		} else {
			iv.ChargedUSD = iv.EstimatedUSD
		}
	}
	if refundLedger > 0 {
		iv.BuyerRefundUSD = refundLedger
	}
	if hasChargeOrRefundLedger {
		// Net spend: -(charge + refund) with charge negative, refund positive.
		net := -(chargeLedger + refundLedger)
		// Present as the remaining amount owed/kept: positive means buyer still
		// pays that much; zero means fully refunded.
		iv.NetChargedUSD = &net
	}
	if iv.BuyerRefundUSD > 0 {
		var cause, funding string
		if err := s.pool.QueryRow(ctx, `
			SELECT reason_code, funding_destination
			  FROM job_dispute_refunds
			 WHERE job_id = $1
			 ORDER BY created_at DESC
			 LIMIT 1`, jobID).Scan(&cause, &funding); err == nil {
			iv.RefundCause = strings.ToLower(cause)
			iv.RefundFundingDestination = funding
		} else if !errors.Is(err, pgx.ErrNoRows) {
			return nil, err
		} else {
			iv.RefundCause = "buyer_refund"
		}
	}
	if iv.ProcessorFeeAllocatedUSD != nil {
		netMicros := usdToMicros(iv.PlatformTakeUSD) -
			usdToMicros(*iv.ProcessorFeeAllocatedUSD)
		netUSD := microsToUSD(netMicros)
		iv.PlatformNetAfterProcessorUSD = &netUSD
	}
	iv.PlatformGrossSpreadUSD = iv.PlatformTakeUSD
	pricing, err := s.JobPricingDecision(ctx, jobID)
	if err != nil {
		return nil, err
	}
	var subsidyMinorUnits int64
	if err := s.pool.QueryRow(ctx, `
		SELECT COALESCE(SUM(f.amount_cents),0)::bigint
		  FROM supplier_payout_funding f
		 WHERE f.liability_job_id=$1 AND f.source_kind='platform_subsidy'`, jobID).
		Scan(&subsidyMinorUnits); err != nil {
		return nil, err
	}
	invoiceCurrency, err := ParseCurrency(iv.Currency)
	if err != nil {
		return nil, fmt.Errorf("invoice %s has invalid settlement currency %q: %w", jobID, iv.Currency, err)
	}
	subsidyMicros, err := invoiceCurrency.MinorToMicros(subsidyMinorUnits)
	if err != nil {
		return nil, err
	}
	subsidyUSD := microsToUSD(subsidyMicros)
	iv.Contribution = buildEconomicContributionView(
		pricing, iv.Currency, iv.PlatformGrossSpreadUSD,
		iv.ProcessorFeeAllocatedUSD, subsidyUSD,
	)
	if quoted, ok, qerr := s.QuotedUSDForJob(ctx, jobID); qerr != nil {
		return nil, qerr
	} else if ok {
		iv.QuotedUSD = &quoted
	}
	if err := s.attachObservedOutputInvoiceEvidence(ctx, &iv); err != nil {
		return nil, err
	}
	return &iv, nil
}

// attachObservedOutputInvoiceEvidence fills job-level ceiling/observed/rebate
// totals from the same durable task columns and ledger amounts the per-task
// receipt uses. No new tables; generative jobs only.
func (s *Store) attachObservedOutputInvoiceEvidence(ctx context.Context, iv *InvoiceView) error {
	if iv == nil {
		return nil
	}
	plan, err := s.JobComputePlan(ctx, iv.JobID)
	if err != nil {
		return err
	}
	if plan == nil || plan.EstimatedOutputTokens <= 0 {
		return nil
	}
	workload, err := s.JobWorkloadDecision(ctx, iv.JobID)
	if err != nil {
		return err
	}
	if workload == nil {
		return nil
	}
	maxTokens := effectiveObservedOutputMaxTokens(*workload, *plan)
	if maxTokens == 0 {
		return nil
	}
	var (
		ceilingSum        int64
		observedSum       int64
		observationsValid = true
		frozenSum         float64
		rebateSum         float64
		tasks             int
	)
	rows, err := s.pool.Query(ctx, `
		SELECT t.id,
		       COALESCE(t.expected_output_records,0),
		       t.reported_tokens_used,
		       t.economic_buyer_charge_usd::float8,
		       COALESCE((
		         SELECT -le.amount_usd::float8 FROM ledger_entries le
		          WHERE le.task_id = t.id AND le.kind = 'buyer_charge'
		          LIMIT 1
		       ),0)
		  FROM tasks t
		 WHERE t.job_id = $1`, iv.JobID)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var (
			taskID          uuid.UUID
			expectedRecords int64
			reported        *int64
			frozen          *float64
			billed          float64
		)
		if err := rows.Scan(&taskID, &expectedRecords, &reported, &frozen, &billed); err != nil {
			return err
		}
		if expectedRecords <= 0 || maxTokens <= 0 ||
			expectedRecords > math.MaxInt64/int64(maxTokens) {
			continue
		}
		ceiling := expectedRecords * int64(maxTokens)
		ceilingSum += ceiling
		if reported != nil {
			obs := *reported
			if obs < 0 {
				observationsValid = false
			} else if obs > ceiling {
				obs = ceiling
			}
			if obs >= 0 {
				observedSum += obs
			}
		}
		if frozen != nil && *frozen > 0 {
			frozenSum += *frozen
			var rebate float64
			if billed <= 0 {
				// Same loader as settlement so nano authority and floor clamp
				// cannot disagree with the invoice.
				settled, serr := loadObservedOutputSettlement(ctx, s.pool, taskID)
				if serr != nil {
					return serr
				}
				billed = settled.BilledCharge
				rebate = settled.RebateUSD
			} else {
				rebate = roundUSD(*frozen - billed)
			}
			if rebate > 0 {
				rebateSum += rebate
			}
		}
		tasks++
	}
	if err := rows.Err(); err != nil {
		return err
	}
	if tasks == 0 || ceilingSum == 0 {
		return nil
	}
	iv.OutputTokenCeiling = &ceilingSum
	if observationsValid {
		iv.ObservedOutputTokens = &observedSum
	}
	if frozenSum > 0 {
		fs := roundUSD(frozenSum)
		iv.FrozenBuyerChargeUSD = &fs
	}
	if rebateSum > 0 {
		rs := roundUSD(rebateSum)
		iv.OutputTokenRebateUSD = &rs
	}
	return nil
}

func (s *Store) jobProcessorFeeAllocatedUSD(
	ctx context.Context,
	jobID uuid.UUID,
	chargeBatchID *uuid.UUID,
	stripePI *string,
) (*float64, *string, error) {
	if chargeBatchID == nil {
		if stripePI == nil || strings.TrimSpace(*stripePI) == "" {
			return nil, nil, nil
		}
		var feeMicros int64
		err := s.pool.QueryRow(ctx, `SELECT (-amount_usd*1000000)::bigint
			FROM ledger_entries
			WHERE kind='stripe_fee' AND payout_ref=$1`, *stripePI).Scan(&feeMicros)
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, nil, nil
		}
		if err != nil {
			return nil, nil, err
		}
		if feeMicros < 0 {
			return nil, nil, fmt.Errorf("job %s processor fee has an invalid sign", jobID)
		}
		feeUSD := float64(feeMicros) / 1_000_000
		method := processorFeeAllocationDirectV1
		return &feeUSD, &method, nil
	}

	var memberCount, allocationCount, allocationMethodCount int64
	var feeMicros, allocatedMicros int64
	var jobFeeMicros *int64
	var allocationMethod *string
	err := s.pool.QueryRow(ctx, `SELECT
			(SELECT COUNT(*) FROM jobs j WHERE j.charge_batch_id=cb.id),
			(SELECT COUNT(*) FROM charge_batch_fee_allocations a
			 WHERE a.charge_batch_id=cb.id),
			(SELECT COUNT(DISTINCT a.allocation_method)
			 FROM charge_batch_fee_allocations a WHERE a.charge_batch_id=cb.id),
			(-fee.amount_usd*1000000)::bigint,
			(SELECT (COALESCE(SUM(a.allocated_fee_usd),0)*1000000)::bigint
			 FROM charge_batch_fee_allocations a WHERE a.charge_batch_id=cb.id),
			(SELECT (a.allocated_fee_usd*1000000)::bigint
			 FROM charge_batch_fee_allocations a
			 WHERE a.charge_batch_id=cb.id AND a.job_id=$2),
			(SELECT a.allocation_method
			 FROM charge_batch_fee_allocations a
			 WHERE a.charge_batch_id=cb.id AND a.job_id=$2)
		FROM charge_batches cb
		JOIN ledger_entries fee
		  ON fee.kind='stripe_fee' AND fee.payout_ref=cb.stripe_pi
		WHERE cb.id=$1 AND cb.status='charged'`,
		*chargeBatchID, jobID,
	).Scan(
		&memberCount, &allocationCount, &allocationMethodCount,
		&feeMicros, &allocatedMicros, &jobFeeMicros, &allocationMethod,
	)
	if errors.Is(err, pgx.ErrNoRows) {
		// Provider fee reconciliation can lag the buyer charge. Omit the
		// attribution until a durable fee fact exists.
		return nil, nil, nil
	}
	if err != nil {
		return nil, nil, err
	}
	if memberCount <= 0 || allocationCount != memberCount ||
		allocationMethodCount != 1 || allocationMethod == nil ||
		(*allocationMethod != batchFeeAllocationHamiltonV1 &&
			*allocationMethod != batchFeeAllocationLegacyV0) ||
		feeMicros < 0 || allocatedMicros != feeMicros ||
		jobFeeMicros == nil || *jobFeeMicros < 0 {
		return nil, nil, fmt.Errorf(
			"job %s charge batch processor-fee allocation is incomplete or inconsistent",
			jobID,
		)
	}
	feeUSD := float64(*jobFeeMicros) / 1_000_000
	return &feeUSD, allocationMethod, nil
}
