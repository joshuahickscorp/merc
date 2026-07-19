package main

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

var errBuyerChargeOutcomeUnknown = errors.New("buyer charge outcome unknown")

func (s *Store) BeginBuyerChargeOperation(
	ctx context.Context,
	operationKey, sourceKind string,
	sourceID, buyerID uuid.UUID,
	customerID, paymentMethodID string,
	amountCents int64,
	currency string,
) (bool, error) {
	operationKey = strings.TrimSpace(operationKey)
	customerID = strings.TrimSpace(customerID)
	paymentMethodID = strings.TrimSpace(paymentMethodID)
	if operationKey == "" || sourceID == uuid.Nil || buyerID == uuid.Nil ||
		customerID == "" || paymentMethodID == "" || amountCents <= 0 || currency != "usd" {
		return false, errors.New("invalid buyer charge operation identity")
	}
	var jobID, batchID *uuid.UUID
	switch sourceKind {
	case "job":
		jobID = &sourceID
	case "batch":
		batchID = &sourceID
	default:
		return false, fmt.Errorf("invalid buyer charge source kind %q", sourceKind)
	}

	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return false, err
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
		return false, err
	}
	if actualBuyer != buyerID {
		return false, fmt.Errorf("buyer charge %s source buyer changed", operationKey)
	}
	if sourceStatus == "charged" {
		return false, fmt.Errorf("buyer charge %s source is already charged", operationKey)
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
		return false, err
	}
	if tag.RowsAffected() == 0 {
		var storedKind, storedCustomer, storedPM, storedCurrency, storedStatus string
		var storedJob, storedBatch *uuid.UUID
		var storedBuyer uuid.UUID
		var storedAmount int64
		if err := tx.QueryRow(ctx, `
			SELECT source_kind,job_id,charge_batch_id,buyer_id,stripe_customer,
			       stripe_payment_method,amount_cents,currency,status
			  FROM buyer_charge_operations WHERE operation_key=$1 FOR UPDATE`, operationKey,
		).Scan(&storedKind, &storedJob, &storedBatch, &storedBuyer, &storedCustomer,
			&storedPM, &storedAmount, &storedCurrency, &storedStatus); err != nil {
			return false, err
		}
		if storedKind != sourceKind || storedBuyer != buyerID || storedCustomer != customerID ||
			storedPM != paymentMethodID || storedAmount != amountCents || storedCurrency != currency ||
			!sameChargeOptionalUUID(storedJob, jobID) || !sameChargeOptionalUUID(storedBatch, batchID) {
			return false, fmt.Errorf("%w: buyer charge operation %s conflicts with its durable request binding",
				errBuyerChargeOutcomeUnknown, operationKey)
		}
		if err := tx.Commit(ctx); err != nil {
			return false, err
		}
		return false, nil
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
		return false, err
	}
	if tag.RowsAffected() != 1 {
		return false, fmt.Errorf("buyer charge %s source lost its request-boundary CAS", operationKey)
	}
	if err := tx.Commit(ctx); err != nil {
		return false, err
	}
	return true, nil
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
	).Scan(&sourceKind, &jobID, &batchID); err != nil {
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
