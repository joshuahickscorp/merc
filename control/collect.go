package main

import (
	"context"
	"errors"
	"fmt"
	"log"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

const (
	chargeCollectInterval = 60 * time.Second
	defaultChargeMinUSD   = 5.00
	chargeBatchMaxAge     = 24 * time.Hour
	chargeRetryStep       = 30 * time.Minute
	chargeRetryMax        = 6 * time.Hour
)

const firmChargeAmountSQL = `GREATEST(0, CASE
	WHEN firm_quote AND COALESCE(firm_quote_max_usd,0) > 0
	     AND COALESCE(actual_usd,0) > firm_quote_max_usd
	THEN firm_quote_max_usd
	ELSE COALESCE(actual_usd,0)
END - COALESCE((SELECT SUM(le.amount_usd) FROM ledger_entries le
                WHERE le.kind = 'sla_refund'
                  AND le.payout_ref = 'sla-' || jobs.id::text), 0))`

const stripeMinChargeUSD = 0.50

func chargeMinUSD() float64 {
	s := strings.TrimSpace(os.Getenv("CX_CHARGE_MIN_USD"))
	if s == "" {
		return defaultChargeMinUSD
	}
	v, err := strconv.ParseFloat(s, 64)
	if err != nil {
		return defaultChargeMinUSD
	}
	if v < 0 {
		return 0
	}
	return v
}

func shouldDeferCharge(actualUSD, thresholdUSD float64) bool {
	return actualUSD < thresholdUSD
}

func chargeRetryBackoff(attempts int) time.Duration {
	if attempts < 1 {
		attempts = 1
	}
	d := time.Duration(attempts) * chargeRetryStep
	if d > chargeRetryMax {
		d = chargeRetryMax
	}
	return d
}

type ChargeBatch struct {
	ID        uuid.UUID
	BuyerID   uuid.UUID
	AmountUSD float64
}

func recordBuyerCashCollection(
	ctx context.Context,
	tx pgx.Tx,
	buyerID uuid.UUID,
	sourceKind string,
	sourceID uuid.UUID,
	charge ChargeResult,
) error {
	var jobID, batchID *uuid.UUID
	switch sourceKind {
	case "job":
		jobID = &sourceID
	case "batch":
		batchID = &sourceID
	default:
		return fmt.Errorf("invalid buyer cash source kind %q", sourceKind)
	}
	var recorded string
	err := tx.QueryRow(ctx, `
		INSERT INTO buyer_cash_collections
		  (payment_intent,charge_id,buyer_id,source_kind,job_id,charge_batch_id,
		   requested_cents,received_cents,currency)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9)
		ON CONFLICT (payment_intent) DO UPDATE SET
		  payment_intent=EXCLUDED.payment_intent
		WHERE buyer_cash_collections.charge_id=EXCLUDED.charge_id
		  AND buyer_cash_collections.buyer_id=EXCLUDED.buyer_id
		  AND buyer_cash_collections.source_kind=EXCLUDED.source_kind
		  AND buyer_cash_collections.job_id IS NOT DISTINCT FROM EXCLUDED.job_id
		  AND buyer_cash_collections.charge_batch_id IS NOT DISTINCT FROM EXCLUDED.charge_batch_id
		  AND buyer_cash_collections.requested_cents=EXCLUDED.requested_cents
		  AND buyer_cash_collections.received_cents=EXCLUDED.received_cents
		  AND buyer_cash_collections.currency=EXCLUDED.currency
		RETURNING payment_intent`,
		charge.PaymentIntentID, charge.ChargeID, buyerID, sourceKind, jobID, batchID,
		charge.RequestedCents, charge.ReceivedCents, charge.Currency,
	).Scan(&recorded)
	if errors.Is(err, pgx.ErrNoRows) {
		return fmt.Errorf("payment intent %s is already bound to a different cash source or amount", charge.PaymentIntentID)
	}
	if err != nil {
		return fmt.Errorf("recording canonical buyer cash %s: %w", charge.PaymentIntentID, err)
	}
	var conflictingState bool
	if err := tx.QueryRow(ctx, `
		SELECT EXISTS(
		  SELECT 1 FROM stripe_charge_cash_state
		   WHERE charge_id=$1 AND (
		     (payment_intent IS NOT NULL AND payment_intent<>$2)
		     OR amount_cents<>$3 OR currency<>$4)
		  UNION ALL
		  SELECT 1 FROM stripe_dispute_cash_state
		   WHERE charge_id=$1 AND (
		     (payment_intent IS NOT NULL AND payment_intent<>$2)
		     OR amount_cents>$3 OR currency<>$4)
		)`, charge.ChargeID, charge.PaymentIntentID, charge.ReceivedCents, charge.Currency).Scan(&conflictingState); err != nil {
		return err
	}
	if conflictingState {
		return fmt.Errorf("charge %s webhook state is bound to a different PaymentIntent", charge.ChargeID)
	}
	if _, err := tx.Exec(ctx, `
		UPDATE stripe_charge_cash_state SET payment_intent=$2,updated_at=now()
		 WHERE charge_id=$1 AND payment_intent IS NULL`, charge.ChargeID, charge.PaymentIntentID); err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, `
		UPDATE stripe_dispute_cash_state SET payment_intent=$2,updated_at=now()
		 WHERE charge_id=$1 AND payment_intent IS NULL`, charge.ChargeID, charge.PaymentIntentID); err != nil {
		return err
	}
	return nil
}

func (s *Store) AttemptingChargeBatches(ctx context.Context, limit int) ([]ChargeBatch, error) {
	rows, err := s.pool.Query(ctx,
		`SELECT id, buyer_id, amount_usd::float8 FROM charge_batches
		 WHERE status = 'attempting' AND (next_at IS NULL OR next_at < now())
		 ORDER BY created_at ASC LIMIT $1`, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []ChargeBatch
	for rows.Next() {
		var b ChargeBatch
		if err := rows.Scan(&b.ID, &b.BuyerID, &b.AmountUSD); err != nil {
			return nil, err
		}
		out = append(out, b)
	}
	return out, rows.Err()
}

func (s *Store) MarkChargeBatchCharged(ctx context.Context, batchID uuid.UUID, charge ChargeResult) error {
	if charge.PaymentIntentID == "" || charge.ChargeID == "" || charge.RequestedCents <= 0 ||
		charge.ReceivedCents != charge.RequestedCents || charge.Currency != "usd" {
		return fmt.Errorf("refusing invalid batch charge confirmation: %+v", charge)
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	var (
		buyerID                      uuid.UUID
		status, existingPI, currency string
		requested, received          int64
	)
	if err := tx.QueryRow(ctx, `
		SELECT buyer_id,status,COALESCE(stripe_pi,''),
		       COALESCE(charge_requested_cents,0),COALESCE(charge_received_cents,0),
		       COALESCE(charge_currency,'')
		  FROM charge_batches WHERE id=$1 FOR UPDATE`, batchID,
	).Scan(&buyerID, &status, &existingPI, &requested, &received, &currency); err != nil {
		return err
	}
	if status == "charged" {
		if existingPI != charge.PaymentIntentID || requested != charge.RequestedCents ||
			received != charge.ReceivedCents || currency != charge.Currency {
			return fmt.Errorf("charge batch %s is already bound to different cash: pi=%s requested=%d received=%d %s",
				batchID, existingPI, requested, received, currency)
		}
		if err := recordBuyerCashCollection(ctx, tx, buyerID, "batch", batchID, charge); err != nil {
			return err
		}
		if err := finalizeBuyerChargeOperation(ctx, tx, "cxbatch-"+batchID.String(), "batch", batchID, charge); err != nil {
			return err
		}
		return tx.Commit(ctx)
	}
	if status != "attempting" && status != "outcome_unknown" {
		return fmt.Errorf("charge batch %s cannot confirm cash from status %q", batchID, status)
	}
	if err := recordBuyerCashCollection(ctx, tx, buyerID, "batch", batchID, charge); err != nil {
		return err
	}
	if err := finalizeBuyerChargeOperation(ctx, tx, "cxbatch-"+batchID.String(), "batch", batchID, charge); err != nil {
		return err
	}
	ct, err := tx.Exec(ctx,
		`UPDATE charge_batches
		    SET status='charged',stripe_pi=$2,charged_at=now(),
		        charge_requested_cents=$3,charge_received_cents=$4,charge_currency=$5
		  WHERE id=$1 AND status=$6`,
		batchID, charge.PaymentIntentID, charge.RequestedCents, charge.ReceivedCents, charge.Currency, status)
	if err != nil {
		return err
	}
	if ct.RowsAffected() != 1 {
		return fmt.Errorf("charge batch %s lost its attempting-state confirmation CAS", batchID)
	}
	if _, err := tx.Exec(ctx,
		`UPDATE jobs SET charge_status = 'charged' WHERE charge_batch_id = $1`, batchID); err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, `
		UPDATE ledger_entries le SET payout_status=$2
		  FROM tasks t JOIN jobs j ON j.id=t.job_id
		 WHERE le.task_id=t.id AND j.charge_batch_id=$1
		   AND le.kind='supplier_credit' AND le.payout_status=$3`,
		batchID, PayoutHeld, PayoutAwaitingFunding); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

func (s *Store) ReflipNoCardJobs(ctx context.Context) (int64, error) {
	tag, err := s.pool.Exec(ctx,
		`UPDATE jobs SET charge_status = 'deferred', deferred_at = now()
		 WHERE charge_status = 'no_payment_method'
		   AND charge_batch_id IS NULL
		   AND COALESCE(actual_usd, 0) > 0
		   AND buyer_id IN (SELECT buyer_id FROM billing_customers
		                    WHERE COALESCE(default_payment_method,'') <> '')`)
	if err != nil {
		return 0, err
	}
	return tag.RowsAffected(), nil
}

func (s *Store) BuyersDueForBatch(ctx context.Context, thresholdUSD float64, maxAge time.Duration, limit int) ([]uuid.UUID, error) {
	rows, err := s.pool.Query(ctx,
		`SELECT buyer_id FROM jobs
		 WHERE charge_status = 'deferred' AND charge_batch_id IS NULL
		   AND COALESCE(actual_usd, 0) > 0
		 GROUP BY buyer_id
		 HAVING (SUM(actual_usd) >= $1
		         OR MIN(COALESCE(deferred_at, created_at)) < now() - make_interval(secs => $2))
		   AND SUM(actual_usd) >= $4
		 ORDER BY MIN(COALESCE(deferred_at, created_at)) ASC LIMIT $3`,
		thresholdUSD, maxAge.Seconds(), limit, stripeMinChargeUSD)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []uuid.UUID
	for rows.Next() {
		var id uuid.UUID
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		out = append(out, id)
	}
	return out, rows.Err()
}

func (s *Store) MarkBuyerDeferredNoCard(ctx context.Context, buyerID uuid.UUID) ([]uuid.UUID, error) {
	rows, err := s.pool.Query(ctx,
		`UPDATE jobs SET charge_status = 'no_payment_method'
		 WHERE buyer_id = $1 AND charge_status = 'deferred' AND charge_batch_id IS NULL
		 RETURNING id`, buyerID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []uuid.UUID
	for rows.Next() {
		var id uuid.UUID
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		out = append(out, id)
	}
	return out, rows.Err()
}

func (s *Store) FormChargeBatch(ctx context.Context, buyerID uuid.UUID) (batch ChargeBatch, formed bool, err error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return batch, false, err
	}
	defer tx.Rollback(ctx)

	rows, err := tx.Query(ctx,
		`SELECT id, `+firmChargeAmountSQL+`::float8 FROM jobs
		 WHERE buyer_id = $1 AND charge_status = 'deferred' AND charge_batch_id IS NULL
		   AND COALESCE(actual_usd, 0) > 0
		 ORDER BY created_at ASC
		 LIMIT 500
		 FOR UPDATE`, buyerID)
	if err != nil {
		return batch, false, err
	}
	var ids []string
	var sum float64
	for rows.Next() {
		var id uuid.UUID
		var usd float64
		if err := rows.Scan(&id, &usd); err != nil {
			rows.Close()
			return batch, false, err
		}
		ids = append(ids, id.String())
		sum += usd
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return batch, false, err
	}
	if len(ids) == 0 || sum <= 0 {
		return batch, false, nil // raced away or nothing chargeable  -  clean no-op
	}
	if sum < stripeMinChargeUSD {
		return batch, false, nil
	}

	if err := tx.QueryRow(ctx,
		`INSERT INTO charge_batches (buyer_id, amount_usd) VALUES ($1, $2) RETURNING id`,
		buyerID, sum).Scan(&batch.ID); err != nil {
		return batch, false, err
	}
	if _, err := tx.Exec(ctx,
		`UPDATE jobs SET charge_batch_id = $1, billed_usd = `+firmChargeAmountSQL+`
		 WHERE id = ANY($2::uuid[]) AND charge_status = 'deferred' AND charge_batch_id IS NULL`,
		batch.ID, ids); err != nil {
		return batch, false, err
	}
	if err := tx.Commit(ctx); err != nil {
		return batch, false, err
	}
	batch.BuyerID, batch.AmountUSD = buyerID, sum
	return batch, true, nil
}

func (s *Store) TerminalUnattemptedJobs(ctx context.Context, limit int) ([]uuid.UUID, error) {
	rows, err := s.pool.Query(ctx,
		`SELECT id FROM jobs
		 WHERE status IN ('complete','failed','cancelled')
		   AND COALESCE(actual_usd, 0) > 0
		   AND charge_status = 'not_attempted'
		 ORDER BY created_at ASC LIMIT $1`, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []uuid.UUID
	for rows.Next() {
		var id uuid.UUID
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		out = append(out, id)
	}
	return out, rows.Err()
}

func (s *Store) FailedChargesDue(ctx context.Context, limit int) ([]uuid.UUID, error) {
	rows, err := s.pool.Query(ctx,
		`SELECT id FROM jobs
		 WHERE charge_status = 'failed'
		   AND (charge_next_at IS NULL OR charge_next_at < now())
		 ORDER BY created_at ASC LIMIT $1`, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []uuid.UUID
	for rows.Next() {
		var id uuid.UUID
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		out = append(out, id)
	}
	return out, rows.Err()
}

func (s *Store) IncrementChargeAttempts(ctx context.Context, jobID uuid.UUID) (int, error) {
	var attempts int
	err := s.pool.QueryRow(ctx,
		`UPDATE jobs SET charge_attempts = charge_attempts + 1 WHERE id = $1
		 RETURNING charge_attempts`, jobID).Scan(&attempts)
	return attempts, err
}

func (s *Store) SetChargeNextAt(ctx context.Context, jobID uuid.UUID, at time.Time) error {
	_, err := s.pool.Exec(ctx, `UPDATE jobs SET charge_next_at = $2 WHERE id = $1`, jobID, at)
	return err
}

func (s *Store) JobChargeStatus(ctx context.Context, jobID uuid.UUID) (string, error) {
	var st string
	err := s.pool.QueryRow(ctx, `SELECT charge_status FROM jobs WHERE id = $1`, jobID).Scan(&st)
	return st, err
}

func (s *Store) MarkJobDeferred(ctx context.Context, jobID uuid.UUID) (bool, error) {
	ct, err := s.pool.Exec(ctx,
		`UPDATE jobs SET charge_status = 'deferred', deferred_at = now()
		 WHERE id = $1 AND charge_status = 'not_attempted'`, jobID)
	if err != nil {
		return false, err
	}
	return ct.RowsAffected() > 0, nil
}

func (s *Store) FreezeChargeAmount(ctx context.Context, jobID uuid.UUID, usd float64) error {
	_, err := s.pool.Exec(ctx,
		`UPDATE jobs SET charge_attempt_usd = $2, billed_usd = COALESCE(billed_usd, $2)
		 WHERE id = $1 AND charge_attempt_usd IS NULL`, jobID, usd)
	return err
}

func (s *Store) JobFrozenChargeInfo(ctx context.Context, jobID uuid.UUID) (uuid.UUID, float64, error) {
	var buyerID uuid.UUID
	var usd float64
	err := s.pool.QueryRow(ctx,
		`SELECT buyer_id, COALESCE(charge_attempt_usd, actual_usd, 0)::float8
		 FROM jobs WHERE id = $1`, jobID).Scan(&buyerID, &usd)
	return buyerID, usd, err
}

func (s *Store) BumpChargeBatchRetry(ctx context.Context, batchID uuid.UUID, backoff func(int) time.Duration) (int, error) {
	var attempts int
	if err := s.pool.QueryRow(ctx,
		`UPDATE charge_batches SET attempts = attempts + 1 WHERE id = $1 RETURNING attempts`,
		batchID).Scan(&attempts); err != nil {
		return 0, err
	}
	_, err := s.pool.Exec(ctx,
		`UPDATE charge_batches SET next_at = now() + make_interval(secs => $2) WHERE id = $1`,
		batchID, backoff(attempts).Seconds())
	return attempts, err
}

func (s *Store) SetJobCharged(ctx context.Context, jobID uuid.UUID, charge ChargeResult) error {
	if charge.PaymentIntentID == "" || charge.ChargeID == "" || charge.RequestedCents <= 0 ||
		charge.ReceivedCents != charge.RequestedCents || charge.Currency != "usd" {
		return fmt.Errorf("refusing invalid job charge confirmation: %+v", charge)
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)

	var (
		buyerID                      uuid.UUID
		status, existingPI, currency string
		requested, received          int64
	)
	if err := tx.QueryRow(ctx, `
		SELECT buyer_id,charge_status,COALESCE(stripe_pi,''),
		       COALESCE(charge_requested_cents,0),COALESCE(charge_received_cents,0),
		       COALESCE(charge_currency,'')
		  FROM jobs WHERE id=$1 FOR UPDATE`, jobID,
	).Scan(&buyerID, &status, &existingPI, &requested, &received, &currency); err != nil {
		return err
	}
	if status == "charged" {
		if existingPI != charge.PaymentIntentID || requested != charge.RequestedCents ||
			received != charge.ReceivedCents || currency != charge.Currency {
			return fmt.Errorf("job %s is already bound to different cash: pi=%s requested=%d received=%d %s",
				jobID, existingPI, requested, received, currency)
		}
		if err := recordBuyerCashCollection(ctx, tx, buyerID, "job", jobID, charge); err != nil {
			return err
		}
		if err := finalizeBuyerChargeOperation(ctx, tx, "job-"+jobID.String(), "job", jobID, charge); err != nil {
			return err
		}
		return tx.Commit(ctx)
	}
	if err := recordBuyerCashCollection(ctx, tx, buyerID, "job", jobID, charge); err != nil {
		return err
	}
	if err := finalizeBuyerChargeOperation(ctx, tx, "job-"+jobID.String(), "job", jobID, charge); err != nil {
		return err
	}
	ct, err := tx.Exec(ctx,
		`UPDATE jobs
		    SET charge_status='charged',stripe_pi=$2,
		        charge_requested_cents=$3,charge_received_cents=$4,charge_currency=$5
		  WHERE id=$1 AND charge_status=$6`,
		jobID, charge.PaymentIntentID, charge.RequestedCents, charge.ReceivedCents, charge.Currency, status)
	if err != nil {
		return err
	}
	if ct.RowsAffected() != 1 {
		return fmt.Errorf("job %s lost its charge-state confirmation CAS", jobID)
	}
	if _, err := tx.Exec(ctx, `
		UPDATE ledger_entries le SET payout_status=$2
		  FROM tasks t
		 WHERE le.task_id=t.id AND t.job_id=$1
		   AND le.kind='supplier_credit' AND le.payout_status=$3`,
		jobID, PayoutHeld, PayoutAwaitingFunding); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

func (s *Store) InsertStripeFee(ctx context.Context, buyerID uuid.UUID, pi string, feeUSD float64) error {
	_, err := s.pool.Exec(ctx,
		`INSERT INTO ledger_entries (kind, buyer_id, amount_usd, payout_status, payout_ref)
		 SELECT 'stripe_fee', $1, $2, 'released', $3
		 WHERE NOT EXISTS (SELECT 1 FROM ledger_entries WHERE kind = 'stripe_fee' AND payout_ref = $3)`,
		buyerID, -feeUSD, pi)
	return err
}

type UnfeedCharge struct {
	BuyerID uuid.UUID
	PI      string
}

func (s *Store) ChargesMissingFeeRows(ctx context.Context, limit int) ([]UnfeedCharge, error) {
	rows, err := s.pool.Query(ctx,
		`SELECT buyer_id, stripe_pi FROM jobs
		 WHERE charge_status = 'charged' AND COALESCE(stripe_pi,'') <> ''
		   AND NOT EXISTS (SELECT 1 FROM ledger_entries
		                   WHERE kind = 'stripe_fee' AND payout_ref = jobs.stripe_pi)
		 UNION ALL
		 SELECT buyer_id, stripe_pi FROM charge_batches
		 WHERE status = 'charged' AND COALESCE(stripe_pi,'') <> ''
		   AND NOT EXISTS (SELECT 1 FROM ledger_entries
		                   WHERE kind = 'stripe_fee' AND payout_ref = charge_batches.stripe_pi)
		 LIMIT $1`, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []UnfeedCharge
	for rows.Next() {
		var u UnfeedCharge
		if err := rows.Scan(&u.BuyerID, &u.PI); err != nil {
			return nil, err
		}
		out = append(out, u)
	}
	return out, rows.Err()
}

const KindSLARefund = "sla_refund"

func slaRefundRef(jobID uuid.UUID) string { return "sla-" + jobID.String() }

func slaRefundAmount(premiumUSD, chargeableUSD float64) float64 {
	if premiumUSD <= 0 || chargeableUSD <= 0 {
		return 0
	}
	if premiumUSD > chargeableUSD {
		return chargeableUSD
	}
	return premiumUSD
}

func slaSpanMissed(createdAt, mergedAt time.Time, guaranteeSecs int) bool {
	return mergedAt.Sub(createdAt) > time.Duration(guaranteeSecs)*time.Second
}

type SLASettleResult struct {
	Decided    bool    // this call stamped sla_met (false: no SLA / already decided / not finalized yet)
	Met        bool    // the outcome stamped by this call
	RefundUSD  float64 // the sla_refund credit recorded by this call (0 on met / nothing chargeable)
	OverBySecs int     // how far past the guarantee the span landed (miss only; for the event text)
}

func (s *Store) SettleJobSLA(ctx context.Context, jobID uuid.UUID) (SLASettleResult, error) {
	var res SLASettleResult
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return res, err
	}
	defer tx.Rollback(ctx)

	var (
		buyerID    uuid.UUID
		guarantee  int
		premium    float64
		met        *bool
		status     string
		createdAt  time.Time
		mergedAt   *time.Time
		chargeable float64 // firm-capped actual BEFORE refund netting (no refund exists yet)
	)
	err = tx.QueryRow(ctx,
		`SELECT buyer_id, COALESCE(sla_guarantee_secs,0), COALESCE(sla_premium_usd,0)::float8,
		        sla_met, status, created_at, results_merged_at,
		        (CASE WHEN firm_quote AND COALESCE(firm_quote_max_usd,0) > 0
		                   AND COALESCE(actual_usd,0) > firm_quote_max_usd
		              THEN firm_quote_max_usd
		              ELSE COALESCE(actual_usd,0) END)::float8
		   FROM jobs WHERE id = $1
		    FOR UPDATE`,
		jobID,
	).Scan(&buyerID, &guarantee, &premium, &met, &status, &createdAt, &mergedAt, &chargeable)
	if err != nil {
		return res, err
	}
	if guarantee <= 0 || met != nil || status != "complete" || mergedAt == nil {
		return res, nil
	}

	if !slaSpanMissed(createdAt, *mergedAt, guarantee) {
		if _, err := tx.Exec(ctx,
			`UPDATE jobs SET sla_met = true WHERE id = $1 AND sla_met IS NULL`, jobID); err != nil {
			return res, err
		}
		if err := tx.Commit(ctx); err != nil {
			return res, err
		}
		return SLASettleResult{Decided: true, Met: true}, nil
	}

	refund := slaRefundAmount(premium, chargeable)
	if refund > 0 {
		if _, err := tx.Exec(ctx,
			`INSERT INTO ledger_entries (kind, buyer_id, amount_usd, payout_status, payout_ref)
			 SELECT $1, $2, $3, 'released', $4
			 WHERE NOT EXISTS (SELECT 1 FROM ledger_entries WHERE kind = $1 AND payout_ref = $4)`,
			KindSLARefund, buyerID, refund, slaRefundRef(jobID)); err != nil {
			return res, err
		}
	}
	if _, err := tx.Exec(ctx,
		`UPDATE jobs SET sla_met = false WHERE id = $1 AND sla_met IS NULL`, jobID); err != nil {
		return res, err
	}
	if err := tx.Commit(ctx); err != nil {
		return res, err
	}
	over := int(mergedAt.Sub(createdAt)/time.Second) - guarantee
	return SLASettleResult{Decided: true, Met: false, RefundUSD: refund, OverBySecs: over}, nil
}

func (s *Store) SLAUndecidedCompleteJobs(ctx context.Context, limit int) ([]uuid.UUID, error) {
	rows, err := s.pool.Query(ctx,
		`SELECT id FROM jobs
		 WHERE COALESCE(sla_guarantee_secs,0) > 0
		   AND sla_met IS NULL
		   AND status = 'complete'
		   AND results_merged_at IS NOT NULL
		 ORDER BY created_at ASC LIMIT $1`, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []uuid.UUID
	for rows.Next() {
		var id uuid.UUID
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		out = append(out, id)
	}
	return out, rows.Err()
}

func settleSLAOutcome(ctx context.Context, store *Store, jobID uuid.UUID) {
	res, err := store.SettleJobSLA(ctx, jobID)
	if err != nil {
		log.Printf("sla: settling outcome for job %s: %v (retried by the collect sweep)", jobID, err)
		return
	}
	if !res.Decided {
		return
	}
	if res.Met {
		log.Printf("sla: job %s met its speed-SLA (guarantee held)", jobID)
		return
	}
	_ = store.InsertJobEvent(ctx, jobID, nil, "sla_missed",
		fmt.Sprintf("Speed SLA missed by %ds · the $%.6f premium was refunded automatically (netted off your charge)", res.OverBySecs, res.RefundUSD), nil)
	metrics.slaMisses.Add(1)
	log.Printf("sla: job %s MISSED its speed-SLA by %ds  -  refunded $%.6f (once, ledger sla_refund)", jobID, res.OverBySecs, res.RefundUSD)
}

func (wk *Workers) settleSLAOutcomes(ctx context.Context) error {
	ids, err := wk.store.SLAUndecidedCompleteJobs(ctx, sweepBatch)
	if err != nil {
		return err
	}
	for _, id := range ids {
		settleSLAOutcome(ctx, wk.store, id)
	}
	return nil
}

func (wk *Workers) collectCharges(ctx context.Context) error {
	if err := wk.settleSLAOutcomes(ctx); err != nil {
		return err
	}

	if stripeKey() == "" {
		log.Print("workers: charge-collect: skipped  -  billing not configured (STRIPE_SECRET_KEY unset), nothing charged or faked")
		return nil
	}

	batches, err := wk.store.AttemptingChargeBatches(ctx, sweepBatch)
	if err != nil {
		return err
	}
	for _, b := range batches {
		wk.chargeBatch(ctx, b)
	}

	if n, rerr := wk.store.ReflipNoCardJobs(ctx); rerr != nil {
		return rerr
	} else if n > 0 {
		log.Printf("workers: charge-collect: %d no_payment_method job(s) re-eligible (buyer saved a card)  -  back to deferred", n)
	}

	threshold := chargeMinUSD()
	buyers, err := wk.store.BuyersDueForBatch(ctx, threshold, chargeBatchMaxAge, sweepBatch)
	if err != nil {
		return err
	}
	for _, buyerID := range buyers {
		cust, pm, gerr := wk.store.GetBillingCustomer(ctx, buyerID)
		if gerr != nil || cust == "" || pm == "" {
			ids, merr := wk.store.MarkBuyerDeferredNoCard(ctx, buyerID)
			if merr != nil {
				log.Printf("workers: charge-collect: marking buyer %s deferred jobs no_payment_method: %v", buyerID, merr)
				continue
			}
			for _, id := range ids {
				_ = wk.store.InsertJobEvent(ctx, id, nil, "charge_failed",
					"Job billed with your other recent jobs but no saved payment method · amount is owed and will be charged once a card is on file", nil)
			}
			log.Printf("workers: charge-collect: buyer %s due for a batch but has no saved card  -  %d job(s) parked no_payment_method (owed)", buyerID, len(ids))
			continue
		}
		batch, formed, ferr := wk.store.FormChargeBatch(ctx, buyerID)
		if ferr != nil {
			log.Printf("workers: charge-collect: forming batch for buyer %s: %v", buyerID, ferr)
			continue
		}
		if !formed {
			continue // raced away between the due-scan and the lock  -  clean no-op
		}
		log.Printf("workers: charge-collect: formed batch %s for buyer %s ($%.6f frozen)", batch.ID, buyerID, batch.AmountUSD)
		wk.chargeBatch(ctx, batch)
	}

	terminal, err := wk.store.TerminalUnattemptedJobs(ctx, sweepBatch)
	if err != nil {
		return err
	}
	for _, id := range terminal {
		chargeOrDeferJob(ctx, wk.store, id)
	}

	due, err := wk.store.FailedChargesDue(ctx, sweepBatch)
	if err != nil {
		return err
	}
	for _, id := range due {
		wk.retryFailedSingle(ctx, id)
	}

	unfeed, err := wk.store.ChargesMissingFeeRows(ctx, sweepBatch)
	if err != nil {
		return err
	}
	for _, u := range unfeed {
		if ferr := recordStripeFee(ctx, wk.store, u.BuyerID, u.PI); ferr != nil {
			log.Printf("workers: charge-collect: fee backfill for pi %s: %v (retried next tick)", u.PI, ferr)
		}
	}

	pendingAllocations, err := wk.store.BatchStripeFeesMissingAllocations(ctx, sweepBatch)
	if err != nil {
		return err
	}
	for _, pi := range pendingAllocations {
		if _, aerr := wk.store.AllocateBatchStripeFee(ctx, pi); aerr != nil {
			log.Printf("workers: charge-collect: batch fee allocation for pi %s: %v (retried next tick)", pi, aerr)
		}
	}
	return nil
}

func (wk *Workers) chargeBatch(ctx context.Context, b ChargeBatch) {
	charge, err := chargeBuyer(ctx, wk.store, b.BuyerID, b.AmountUSD,
		"cxbatch-"+b.ID.String(), "batch", b.ID)
	if err != nil {
		if errors.Is(err, errBuyerChargeOutcomeUnknown) {
			log.Printf("workers: charge-collect: batch %s outcome unknown; automatic re-charge is blocked pending Stripe reconciliation: %v", b.ID, err)
			return
		}
		attempts, aerr := wk.store.BumpChargeBatchRetry(ctx, b.ID, chargeRetryBackoff)
		if aerr != nil {
			log.Printf("workers: charge-collect: bumping retry for batch %s: %v", b.ID, aerr)
		}
		log.Printf("workers: charge-collect: batch %s ($%.6f, buyer %s) charge failed (attempt %d, backed off, still owed): %v",
			b.ID, b.AmountUSD, b.BuyerID, attempts, err)
		return
	}
	if merr := wk.store.MarkChargeBatchCharged(ctx, b.ID, charge); merr != nil {
		log.Printf("workers: charge-collect: batch %s charged (pi %s) but confirmation write failed: %v (reconfirmed next tick)", b.ID, charge.PaymentIntentID, merr)
		return
	}
	log.Printf("workers: charge-collect: batch %s charged ($%.2f received, buyer %s, pi %s)",
		b.ID, float64(charge.ReceivedCents)/100, b.BuyerID, charge.PaymentIntentID)
	if ferr := recordStripeFee(ctx, wk.store, b.BuyerID, charge.PaymentIntentID); ferr != nil {
		log.Printf("workers: charge-collect: stripe fee for batch %s (pi %s) not recorded yet: %v (backfilled next tick)", b.ID, charge.PaymentIntentID, ferr)
	}
}

func (wk *Workers) retryFailedSingle(ctx context.Context, jobID uuid.UUID) {
	buyerID, usd, err := wk.store.JobFrozenChargeInfo(ctx, jobID)
	if err != nil || usd <= 0 {
		return
	}
	charge, err := chargeBuyer(ctx, wk.store, buyerID, usd,
		"job-"+jobID.String(), "job", jobID)
	if err != nil {
		if errors.Is(err, errBuyerChargeOutcomeUnknown) {
			log.Printf("workers: charge-collect: job %s outcome unknown; automatic re-charge is blocked pending Stripe reconciliation: %v", jobID, err)
			return
		}
		attempts, aerr := wk.store.IncrementChargeAttempts(ctx, jobID)
		if aerr != nil {
			log.Printf("workers: charge-collect: bumping charge attempts for job %s: %v", jobID, aerr)
			return
		}
		next := time.Now().Add(chargeRetryBackoff(attempts))
		if serr := wk.store.SetChargeNextAt(ctx, jobID, next); serr != nil {
			log.Printf("workers: charge-collect: scheduling next retry for job %s: %v", jobID, serr)
		}
		log.Printf("workers: charge-collect: retry %d for job %s ($%.6f) failed, next at %s (still owed): %v",
			attempts, jobID, usd, next.UTC().Format(time.RFC3339), err)
		return
	}
	if serr := wk.store.SetJobCharged(ctx, jobID, charge); serr != nil {
		log.Printf("workers: charge-collect: marking job %s charged (pi %s): %v", jobID, charge.PaymentIntentID, serr)
		return
	}
	log.Printf("workers: charge-collect: job %s charged on retry ($%.2f received, pi %s)",
		jobID, float64(charge.ReceivedCents)/100, charge.PaymentIntentID)
	if ferr := recordStripeFee(ctx, wk.store, buyerID, charge.PaymentIntentID); ferr != nil {
		log.Printf("workers: charge-collect: stripe fee for job %s (pi %s) not recorded yet: %v (backfilled next tick)", jobID, charge.PaymentIntentID, ferr)
	}
}
