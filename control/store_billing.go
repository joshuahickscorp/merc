package main

import (
	"context"
	_ "embed"
	"errors"
	"fmt"
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
	err = s.pool.QueryRow(ctx,
		`SELECT buyer_id, COALESCE(actual_usd,0), firm_quote, COALESCE(firm_quote_max_usd,0),
		        COALESCE((SELECT SUM(le.amount_usd) FROM ledger_entries le
		                  WHERE le.kind = 'sla_refund'
		                    AND le.payout_ref = 'sla-' || jobs.id::text), 0)::float8
		   FROM jobs WHERE id=$1`,
		jobID).Scan(&buyerID, &actualUSD, &firmQuote, &firmMax, &slaRefund)
	if errors.Is(err, pgx.ErrNoRows) {
		err = errNotFound
		return
	}
	if err != nil {
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
	Currency        string   `json:"currency"`
	EstimatedUSD    float64  `json:"estimated_usd"`
	ActualUSD       float64  `json:"actual_usd"`
	ChargedUSD      float64  `json:"charged_usd"`
	SupplierPaidUSD float64  `json:"supplier_credit_usd"`
	PlatformTakeUSD float64  `json:"platform_take_usd"`
	QuotedUSD       *float64 `json:"quoted_usd,omitempty"`
	FirmQuote       bool     `json:"firm_quote,omitempty"`
	FirmQuoteMaxUSD *float64 `json:"firm_quote_max_usd,omitempty"`
	BilledUSD       *float64 `json:"billed_usd,omitempty"`
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
}

func (s *Store) JobInvoice(ctx context.Context, jobID, buyerID uuid.UUID) (*InvoiceView, error) {
	iv := InvoiceView{JobID: jobID}
	var firmMax, billed *float64
	var chargeBatchID *uuid.UUID
	var stripePI *string
	var slaGuarantee int
	var slaPremium *float64
	err := s.pool.QueryRow(ctx,
		`SELECT buyer_id, status, job_type, created_at,
		        COALESCE(estimated_usd,0), COALESCE(actual_usd,0),
		        firm_quote, firm_quote_max_usd, billed_usd,
		        charge_batch_id, stripe_pi,
		        COALESCE(sla_guarantee_secs,0), sla_premium_usd, sla_met
		 FROM jobs WHERE id = $1 AND buyer_id = $2`,
		jobID, buyerID,
	).Scan(&iv.BuyerID, &iv.Status, &iv.JobType, &iv.CreatedAt, &iv.EstimatedUSD, &iv.ActualUSD,
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
		case "platform_take":
			iv.PlatformTakeUSD += amt
		case "buyer_charge":
			iv.ChargedUSD += amt
		}
	}
	if iv.ChargedUSD == 0 {
		if iv.ActualUSD > 0 {
			iv.ChargedUSD = iv.ActualUSD
		} else {
			iv.ChargedUSD = iv.EstimatedUSD
		}
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	if iv.ProcessorFeeAllocatedUSD != nil {
		netMicros := usdToMicros(iv.PlatformTakeUSD) -
			usdToMicros(*iv.ProcessorFeeAllocatedUSD)
		netUSD := microsToUSD(netMicros)
		iv.PlatformNetAfterProcessorUSD = &netUSD
	}
	if iv.Currency == "" {
		// Pre-settlement invoice: no ledger rows yet — report the deployment
		// settlement currency (the only currency new charges will use).
		iv.Currency = SettlementCurrencyCode()
	}
	if quoted, ok, qerr := s.QuotedUSDForJob(ctx, jobID); qerr != nil {
		return nil, qerr
	} else if ok {
		iv.QuotedUSD = &quoted
	}
	return &iv, nil
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
