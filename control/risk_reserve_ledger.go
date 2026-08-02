package main

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

// Risk reserve is real money, not a subtracted number that never leaves the
// platform. Lifecycle:
//
//  1. Accrue at settlement: KindRiskReserveAccrual posts RiskReserveBasisPoints
//     of the buyer charge to the platform risk-reserve account (positive amount,
//     held until release or consume).
//  2. Release after the dispute window elapses with no refund or open dispute
//     against the job: KindRiskReserveRelease moves the accrual into
//     contribution (negative of the accrual amount against the reserve).
//  3. Consume when a refund or dispute lands: KindRiskReserveConsume burns the
//     reserve against the refund liability.
//
// Balance for a job = sum(accrual) + sum(release) + sum(consume). Accrual is
// positive; release and consume are negative. A healthy closed job ends at zero.

const (
	KindRiskReserveAccrual = "risk_reserve_accrual"
	KindRiskReserveRelease = "risk_reserve_release"
	KindRiskReserveConsume = "risk_reserve_consume"

	// riskReserveDisputeWindow matches the seven-day dispute filing window
	// (errDisputeWindowClosed / job submit default payout hold).
	riskReserveDisputeWindow = 7 * 24 * time.Hour
)

func riskReserveAccrualRef(jobID uuid.UUID) string {
	return "risk-reserve-accrual-" + jobID.String()
}

func riskReserveReleaseRef(jobID uuid.UUID) string {
	return "risk-reserve-release-" + jobID.String()
}

func riskReserveConsumeRef(jobID uuid.UUID) string {
	return "risk-reserve-consume-" + jobID.String()
}

// AccrueRiskReserveAtSettlement posts the decision's modeled risk reserve for a
// job. No-op when the decision has no cost schedule, risk is not modeled, or the
// amount is zero. Idempotent on payout_ref.
func (s *Store) AccrueRiskReserveAtSettlement(
	ctx context.Context, jobID uuid.UUID, pricing PricingDecision,
) error {
	if pricing.CostScheduleSHA256 == "" {
		return nil
	}
	if pricing.RiskReserve.Status != pricingCostModeled {
		return nil
	}
	micros := usdToMicros(pricing.RiskReserve.Amount)
	if micros <= 0 {
		return nil
	}
	currency := pricing.Currency
	if currency == "" {
		currency = SettlementCurrencyCode()
	}
	releaseAt := time.Now().UTC().Add(riskReserveDisputeWindow)
	_, err := insertLedgerEntryIfAbsentByRefTx(ctx, s.pool, ledgerInsert{
		Kind:         KindRiskReserveAccrual,
		AmountMicros: micros,
		Currency:     currency,
		PayoutStatus: PayoutHeld,
		ReleaseAt:    &releaseAt,
		PayoutRef:    riskReserveAccrualRef(jobID),
	})
	return err
}

// AccrueRiskReserveAtSettlementTx is the transactional form used from job
// finalization.
func AccrueRiskReserveAtSettlementTx(
	ctx context.Context, db ledgerExec, jobID uuid.UUID, pricing PricingDecision,
) error {
	if pricing.CostScheduleSHA256 == "" || pricing.RiskReserve.Status != pricingCostModeled {
		return nil
	}
	micros := usdToMicros(pricing.RiskReserve.Amount)
	if micros <= 0 {
		return nil
	}
	currency := pricing.Currency
	if currency == "" {
		currency = SettlementCurrencyCode()
	}
	releaseAt := time.Now().UTC().Add(riskReserveDisputeWindow)
	_, err := insertLedgerEntryIfAbsentByRefTx(ctx, db, ledgerInsert{
		Kind:         KindRiskReserveAccrual,
		AmountMicros: micros,
		Currency:     currency,
		PayoutStatus: PayoutHeld,
		ReleaseAt:    &releaseAt,
		PayoutRef:    riskReserveAccrualRef(jobID),
	})
	return err
}

// ReleaseRiskReserveAfterDisputeWindow posts a release when the dispute window
// has elapsed, no open dispute exists, and no buyer refund has been posted for
// the job. Idempotent.
func (s *Store) ReleaseRiskReserveAfterDisputeWindow(
	ctx context.Context, jobID uuid.UUID, now time.Time,
) error {
	var accruedMicros int64
	var currency string
	var releaseAt *time.Time
	err := s.pool.QueryRow(ctx, `
		SELECT (amount_usd * 1000000)::bigint, currency, release_at
		  FROM ledger_entries
		 WHERE kind = $1 AND payout_ref = $2`,
		KindRiskReserveAccrual, riskReserveAccrualRef(jobID),
	).Scan(&accruedMicros, &currency, &releaseAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil // nothing to release
	}
	if err != nil {
		return err
	}
	if accruedMicros <= 0 {
		return nil
	}
	if releaseAt != nil && now.Before(*releaseAt) {
		return fmt.Errorf(
			"risk reserve release refused: dispute window open until %s for job %s",
			releaseAt.UTC().Format(time.RFC3339), jobID)
	}
	// Open dispute blocks release.
	var openDispute int
	if err := s.pool.QueryRow(ctx, `
		SELECT COUNT(*) FROM disputes
		 WHERE job_id = $1
		   AND status IN ('open','no_peer','reverifying','unresolvable')`,
		jobID).Scan(&openDispute); err != nil {
		return err
	}
	if openDispute > 0 {
		return fmt.Errorf(
			"risk reserve release refused: job %s has an open dispute", jobID)
	}
	// Any buyer refund on the job means the reserve should be consumed, not
	// released.
	var refundMicros int64
	if err := s.pool.QueryRow(ctx, `
		SELECT COALESCE(SUM((amount_usd * 1000000)::bigint),0)
		  FROM ledger_entries le
		 WHERE le.kind = $1
		   AND (
		     le.task_id IN (SELECT id FROM tasks WHERE job_id = $2)
		     OR le.payout_ref LIKE 'dispute-%%' || $2::text || '%%'
		     OR le.payout_ref = $3
		   )`,
		KindBuyerRefund, jobID, "dispute-sla-refund-"+jobID.String(),
	).Scan(&refundMicros); err != nil {
		return err
	}
	if refundMicros > 0 {
		return fmt.Errorf(
			"risk reserve release refused: job %s has a buyer refund; consume instead",
			jobID)
	}
	// Already released?
	var released int
	if err := s.pool.QueryRow(ctx, `
		SELECT COUNT(*) FROM ledger_entries
		 WHERE kind = $1 AND payout_ref = $2`,
		KindRiskReserveRelease, riskReserveReleaseRef(jobID),
	).Scan(&released); err != nil {
		return err
	}
	if released > 0 {
		return nil
	}
	// Already consumed?
	var consumed int
	if err := s.pool.QueryRow(ctx, `
		SELECT COUNT(*) FROM ledger_entries
		 WHERE kind = $1 AND payout_ref = $2`,
		KindRiskReserveConsume, riskReserveConsumeRef(jobID),
	).Scan(&consumed); err != nil {
		return err
	}
	if consumed > 0 {
		return fmt.Errorf(
			"risk reserve release refused: job %s reserve was already consumed", jobID)
	}
	_, err = insertLedgerEntryIfAbsentByRefTx(ctx, s.pool, ledgerInsert{
		Kind:         KindRiskReserveRelease,
		AmountMicros: -accruedMicros,
		Currency:     currency,
		PayoutStatus: PayoutReleased,
		PayoutRef:    riskReserveReleaseRef(jobID),
	})
	return err
}

// ConsumeRiskReserveOnRefund burns the accrued reserve when a refund or dispute
// lands against the job. Idempotent. No-op when nothing was accrued.
func (s *Store) ConsumeRiskReserveOnRefund(ctx context.Context, jobID uuid.UUID) error {
	var accruedMicros int64
	var currency string
	err := s.pool.QueryRow(ctx, `
		SELECT (amount_usd * 1000000)::bigint, currency
		  FROM ledger_entries
		 WHERE kind = $1 AND payout_ref = $2`,
		KindRiskReserveAccrual, riskReserveAccrualRef(jobID),
	).Scan(&accruedMicros, &currency)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil
	}
	if err != nil {
		return err
	}
	if accruedMicros <= 0 {
		return nil
	}
	// Already released — cannot consume what was released to contribution.
	var released int
	if err := s.pool.QueryRow(ctx, `
		SELECT COUNT(*) FROM ledger_entries
		 WHERE kind = $1 AND payout_ref = $2`,
		KindRiskReserveRelease, riskReserveReleaseRef(jobID),
	).Scan(&released); err != nil {
		return err
	}
	if released > 0 {
		return fmt.Errorf(
			"risk reserve consume refused: job %s reserve was already released", jobID)
	}
	_, err = insertLedgerEntryIfAbsentByRefTx(ctx, s.pool, ledgerInsert{
		Kind:         KindRiskReserveConsume,
		AmountMicros: -accruedMicros,
		Currency:     currency,
		PayoutStatus: PayoutReleased,
		PayoutRef:    riskReserveConsumeRef(jobID),
	})
	return err
}

// RiskReserveBalanceMicros returns the net reserve balance for a job in
// micro-major-units. Positive means still held; zero means released or consumed
// or never accrued.
func (s *Store) RiskReserveBalanceMicros(ctx context.Context, jobID uuid.UUID) (int64, error) {
	var balance int64
	err := s.pool.QueryRow(ctx, `
		SELECT COALESCE(SUM((amount_usd * 1000000)::bigint),0)
		  FROM ledger_entries
		 WHERE kind IN ($1, $2, $3)
		   AND payout_ref IN ($4, $5, $6)`,
		KindRiskReserveAccrual, KindRiskReserveRelease, KindRiskReserveConsume,
		riskReserveAccrualRef(jobID), riskReserveReleaseRef(jobID), riskReserveConsumeRef(jobID),
	).Scan(&balance)
	return balance, err
}
