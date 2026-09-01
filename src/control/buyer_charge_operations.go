package main

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

var errBuyerChargeOutcomeUnknown = errors.New("buyer charge outcome unknown")

var errBuyerChargeDefinitelyFailed = errors.New("buyer charge definitely failed")

// buyerChargeProviderKey is stable for the first request and changes only
// after Stripe has explicitly rejected a prior request. The durable operation
// key remains unchanged in metadata, so a later payment_intent.succeeded event
// still reconciles the same job or batch.
func buyerChargeProviderKey(operationKey string, attempt int) string {
	if attempt <= 1 {
		return operationKey
	}
	return operationKey + "-retry-" + strconv.Itoa(attempt)
}

func (s *Store) BeginBuyerChargeOperation(
	ctx context.Context,
	operationKey, sourceKind string,
	sourceID, buyerID uuid.UUID,
	customerID, paymentMethodID string,
	amountCents int64,
	currency string,
) (bool, string, error) {
	operationKey = strings.TrimSpace(operationKey)
	customerID = strings.TrimSpace(customerID)
	paymentMethodID = strings.TrimSpace(paymentMethodID)
	if operationKey == "" || sourceID == uuid.Nil || buyerID == uuid.Nil ||
		customerID == "" || paymentMethodID == "" || amountCents <= 0 || RequireSettlementCurrency(currency) != nil {
		return false, "", errors.New("invalid buyer charge operation identity")
	}
	var jobID, batchID *uuid.UUID
	switch sourceKind {
	case "job":
		jobID = &sourceID
	case "batch":
		batchID = &sourceID
	default:
		return false, "", fmt.Errorf("invalid buyer charge source kind %q", sourceKind)
	}

	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return false, "", err
	}
	defer tx.Rollback(ctx)

	var actualBuyer uuid.UUID
	var sourceStatus string
	if sourceKind == "job" {
		err = tx.QueryRow(ctx,
			`SELECT buyer_id,charge_status FROM jobs WHERE id=$1 FOR UPDATE`, sourceID,
		).Scan(&actualBuyer, &sourceStatus)
	} else {
		err = tx.QueryRow(ctx,
			`SELECT buyer_id,status FROM charge_batches WHERE id=$1 FOR UPDATE`, sourceID,
		).Scan(&actualBuyer, &sourceStatus)
	}
	if err != nil {
		return false, "", err
	}
	if actualBuyer != buyerID {
		return false, "", fmt.Errorf("buyer charge %s source buyer changed", operationKey)
	}
	if sourceStatus == "charged" {
		return false, "", fmt.Errorf("buyer charge %s source is already charged", operationKey)
	}

	tag, err := tx.Exec(ctx, `
		INSERT INTO buyer_charge_operations
		  (operation_key,source_kind,job_id,charge_batch_id,buyer_id,
		   stripe_customer,stripe_payment_method,amount_cents,currency,status)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,'outcome_unknown')
		ON CONFLICT (operation_key) DO NOTHING`,
		operationKey, sourceKind, jobID, batchID, buyerID, customerID,
		paymentMethodID, amountCents, currency)
	if err != nil {
		return false, "", err
	}
	if tag.RowsAffected() == 0 {
		var storedKind, storedCustomer, storedPM, storedCurrency, storedStatus string
		var storedJob, storedBatch *uuid.UUID
		var storedBuyer uuid.UUID
		var storedAmount, storedAttempt int64
		if err := tx.QueryRow(ctx, `
			SELECT source_kind,job_id,charge_batch_id,buyer_id,stripe_customer,
			       stripe_payment_method,amount_cents,currency,status,provider_attempt
			  FROM buyer_charge_operations WHERE operation_key=$1 FOR UPDATE`, operationKey,
		).Scan(&storedKind, &storedJob, &storedBatch, &storedBuyer, &storedCustomer,
			&storedPM, &storedAmount, &storedCurrency, &storedStatus, &storedAttempt); err != nil {
			return false, "", err
		}
		if storedKind != sourceKind || storedBuyer != buyerID || storedCustomer != customerID ||
			(storedStatus != "failed" && storedPM != paymentMethodID) ||
			storedAmount != amountCents || storedCurrency != currency ||
			!sameChargeOptionalUUID(storedJob, jobID) || !sameChargeOptionalUUID(storedBatch, batchID) {
			return false, "", fmt.Errorf("%w: buyer charge operation %s conflicts with its durable request binding",
				errBuyerChargeOutcomeUnknown, operationKey)
		}
		if storedStatus == "failed" {
			if storedAttempt <= 0 || storedAttempt >= 1<<30 {
				return false, "", fmt.Errorf("buyer charge %s has an invalid provider attempt", operationKey)
			}
			if sourceKind == "job" {
				if sourceStatus != "failed" {
					return false, "", fmt.Errorf("buyer charge %s failed operation has source status %q", operationKey, sourceStatus)
				}
				if tag, err := tx.Exec(ctx, `
					UPDATE jobs SET charge_status='outcome_unknown',charge_next_at=NULL
					 WHERE id=$1 AND charge_status='failed'`, sourceID); err != nil {
					return false, "", err
				} else if tag.RowsAffected() != 1 {
					return false, "", fmt.Errorf("buyer charge %s lost its retry-state CAS", operationKey)
				}
			} else {
				if sourceStatus != "attempting" && sourceStatus != "outcome_unknown" {
					return false, "", fmt.Errorf("buyer charge %s failed batch has source status %q", operationKey, sourceStatus)
				}
				if tag, err := tx.Exec(ctx, `
					UPDATE charge_batches SET status='attempting',next_at=NULL
					 WHERE id=$1 AND status IN ('attempting','outcome_unknown')`, sourceID); err != nil {
					return false, "", err
				} else if tag.RowsAffected() != 1 {
					return false, "", fmt.Errorf("buyer charge %s lost its retry-state CAS", operationKey)
				}
			}
			nextAttempt := storedAttempt + 1
			if tag, err := tx.Exec(ctx, `
				UPDATE buyer_charge_operations
				   SET status='outcome_unknown',stripe_payment_method=$3,
				       provider_attempt=$2,last_error=NULL,updated_at=now()
				 WHERE operation_key=$1 AND status='failed'`, operationKey, nextAttempt, paymentMethodID); err != nil {
				return false, "", err
			} else if tag.RowsAffected() != 1 {
				return false, "", fmt.Errorf("buyer charge %s lost its operation retry CAS", operationKey)
			}
			if err := tx.Commit(ctx); err != nil {
				return false, "", err
			}
			return true, buyerChargeProviderKey(operationKey, int(nextAttempt)), nil
		}
		if err := tx.Commit(ctx); err != nil {
			return false, "", err
		}
		return false, "", nil
	}

	if sourceKind == "job" {
		tag, err = tx.Exec(ctx, `
			UPDATE jobs SET charge_status='outcome_unknown'
			 WHERE id=$1 AND charge_status<>'charged'`, sourceID)
	} else {
		tag, err = tx.Exec(ctx, `
			UPDATE charge_batches SET status='outcome_unknown'
			 WHERE id=$1 AND status='attempting'`, sourceID)
		if err == nil && tag.RowsAffected() == 1 {
			_, err = tx.Exec(ctx, `
				UPDATE jobs SET charge_status='outcome_unknown'
				 WHERE charge_batch_id=$1 AND charge_status<>'charged'`, sourceID)
		}
	}
	if err != nil {
		return false, "", err
	}
	if tag.RowsAffected() != 1 {
		return false, "", fmt.Errorf("buyer charge %s source lost its request-boundary CAS", operationKey)
	}
	if err := tx.Commit(ctx); err != nil {
		return false, "", err
	}
	return true, operationKey, nil
}

func (s *Store) NoteBuyerChargeOutcomeUnknown(ctx context.Context, operationKey string, cause error) error {
	reason := "Stripe outcome requires reconciliation"
	if cause != nil {
		reason = truncate(cause.Error(), 500)
	}
	_, err := s.pool.Exec(ctx, `
		UPDATE buyer_charge_operations SET last_error=$2,updated_at=now()
		 WHERE operation_key=$1 AND status='outcome_unknown'`, operationKey, reason)
	return err
}

// MarkBuyerChargeDefinitelyFailed closes a provider-confirmed rejection. It
// moves the source back onto its ordinary retry queue while retaining the
// immutable operation row and the failed reason. If this transaction cannot
// commit, callers must treat the provider outcome as unknown instead.
func (s *Store) MarkBuyerChargeDefinitelyFailed(ctx context.Context, operationKey string, cause error) error {
	reason := "Stripe explicitly rejected the charge before cash movement"
	if cause != nil {
		reason = truncate(cause.Error(), 500)
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)

	var sourceKind, status string
	var jobID, batchID *uuid.UUID
	if err := tx.QueryRow(ctx, `
		SELECT source_kind,job_id,charge_batch_id,status
		  FROM buyer_charge_operations WHERE operation_key=$1 FOR UPDATE`, operationKey,
	).Scan(&sourceKind, &jobID, &batchID, &status); err != nil {
		return err
	}
	if status == "failed" {
		return tx.Commit(ctx)
	}
	if status != "outcome_unknown" {
		return fmt.Errorf("buyer charge %s cannot become failed from status %q", operationKey, status)
	}
	if _, err := tx.Exec(ctx, `
		UPDATE buyer_charge_operations
		   SET status='failed',last_error=$2,updated_at=now()
		 WHERE operation_key=$1 AND status='outcome_unknown'`, operationKey, reason); err != nil {
		return err
	}
	switch {
	case sourceKind == "job" && jobID != nil:
		if tag, err := tx.Exec(ctx, `
			UPDATE jobs SET charge_status='failed',charge_next_at=NULL
			 WHERE id=$1 AND charge_status IN ('outcome_unknown','failed')`, *jobID); err != nil {
			return err
		} else if tag.RowsAffected() != 1 {
			return fmt.Errorf("buyer charge %s source job lost its failure-state CAS", operationKey)
		}
	case sourceKind == "batch" && batchID != nil:
		if tag, err := tx.Exec(ctx, `
			UPDATE charge_batches SET status='attempting',next_at=NULL
			 WHERE id=$1 AND status IN ('attempting','outcome_unknown')`, *batchID); err != nil {
			return err
		} else if tag.RowsAffected() != 1 {
			return fmt.Errorf("buyer charge %s source batch lost its failure-state CAS", operationKey)
		}
	default:
		return fmt.Errorf("buyer charge %s has invalid source binding", operationKey)
	}
	return tx.Commit(ctx)
}

func sameChargeOptionalUUID(a, b *uuid.UUID) bool {
	if a == nil || b == nil {
		return a == nil && b == nil
	}
	return *a == *b
}

func finalizeBuyerChargeOperation(
	ctx context.Context,
	tx pgx.Tx,
	operationKey, sourceKind string,
	sourceID uuid.UUID,
	charge ChargeResult,
) error {
	tag, err := tx.Exec(ctx, `
		UPDATE buyer_charge_operations
		   SET status='succeeded',payment_intent=$4,charge_id=$5,last_error=NULL,updated_at=now()
		 WHERE operation_key=$1 AND source_kind=$2
		   AND (($2='job' AND job_id=$3) OR ($2='batch' AND charge_batch_id=$3))
		   AND amount_cents=$6 AND currency=$7
		   AND status='outcome_unknown'`,
		operationKey, sourceKind, sourceID, charge.PaymentIntentID, charge.ChargeID,
		charge.RequestedCents, charge.Currency)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 1 {
		return nil
	}
	var status, pi, chargeID string
	err = tx.QueryRow(ctx, `
		SELECT status,COALESCE(payment_intent,''),COALESCE(charge_id,'')
		  FROM buyer_charge_operations WHERE operation_key=$1`, operationKey,
	).Scan(&status, &pi, &chargeID)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil // legacy/operator-confirmed cash predating durable request operations
	}
	if err != nil {
		return err
	}
	if status == "succeeded" && pi == charge.PaymentIntentID && chargeID == charge.ChargeID {
		return nil
	}
	return fmt.Errorf("buyer charge operation %s cannot bind cash from status=%s pi=%s charge=%s",
		operationKey, status, pi, chargeID)
}

func (s *Store) ReconcileBuyerChargeOperation(ctx context.Context, operationKey string, charge ChargeResult) error {
	var sourceKind string
	var jobID, batchID *uuid.UUID
	if err := s.pool.QueryRow(ctx, `
		SELECT source_kind,job_id,charge_batch_id
		  FROM buyer_charge_operations WHERE operation_key=$1`, operationKey,
	).Scan(&sourceKind, &jobID, &batchID); errors.Is(err, pgx.ErrNoRows) {
		// A successful PaymentIntent can belong to the prepaid funding lane,
		// which has its own durable operation table rather than a job/batch
		// charge row.  The synchronous top-up response usually wins the
		// credit race, so this also needs to accept the later provider webhook
		// as the same completed operation instead of returning 500 forever.
		return s.reconcilePrepaidTopup(ctx, operationKey, charge)
	} else if err != nil {
		return err
	}
	if sourceKind == "job" && jobID != nil {
		return s.SetJobCharged(ctx, *jobID, charge)
	}
	if sourceKind == "batch" && batchID != nil {
		return s.MarkChargeBatchCharged(ctx, *batchID, charge)
	}
	return fmt.Errorf("buyer charge operation %s has an invalid source binding", operationKey)
}
