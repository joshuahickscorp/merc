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

var errBuyerChargeOperationNotFound = errors.New("buyer charge operation not found")

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
	var insertedRows, sourceUpdatedRows, dependentUpdatedRows int64
	if sourceKind == "job" {
		err = tx.QueryRow(ctx, `
			WITH source AS MATERIALIZED (
				SELECT buyer_id,charge_status AS source_status
				  FROM jobs WHERE id=$2 FOR UPDATE
			), inserted AS MATERIALIZED (
				INSERT INTO buyer_charge_operations
				  (operation_key,source_kind,job_id,charge_batch_id,buyer_id,
				   stripe_customer,stripe_payment_method,amount_cents,currency,status)
				SELECT $1,'job',$2,NULL,$3,$4,$5,$6,$7,'outcome_unknown'
				  FROM source
				 WHERE source.buyer_id=$3 AND source.source_status<>'charged'
				ON CONFLICT (operation_key) DO NOTHING
				RETURNING operation_key
			), marked AS (
				UPDATE jobs j
				   SET charge_status='outcome_unknown'
				  FROM inserted
				 WHERE j.id=$2 AND j.charge_status<>'charged'
				RETURNING j.id
			)
			SELECT source.buyer_id,source.source_status,
			       COALESCE((SELECT count(*) FROM inserted),0)::bigint,
			       COALESCE((SELECT count(*) FROM marked),0)::bigint,
			       0::bigint
			  FROM source`,
			operationKey, sourceID, buyerID, customerID, paymentMethodID, amountCents, currency,
		).Scan(&actualBuyer, &sourceStatus, &insertedRows, &sourceUpdatedRows, &dependentUpdatedRows)
	} else {
		err = tx.QueryRow(ctx, `
			WITH source AS MATERIALIZED (
				SELECT buyer_id,status AS source_status
				  FROM charge_batches WHERE id=$2 FOR UPDATE
			), inserted AS MATERIALIZED (
				INSERT INTO buyer_charge_operations
				  (operation_key,source_kind,job_id,charge_batch_id,buyer_id,
				   stripe_customer,stripe_payment_method,amount_cents,currency,status)
				SELECT $1,'batch',NULL,$2,$3,$4,$5,$6,$7,'outcome_unknown'
				  FROM source
				 WHERE source.buyer_id=$3 AND source.source_status<>'charged'
				ON CONFLICT (operation_key) DO NOTHING
				RETURNING operation_key
			), marked AS (
				UPDATE charge_batches b
				   SET status='outcome_unknown'
				  FROM inserted
				 WHERE b.id=$2 AND b.status='attempting'
				RETURNING b.id
			), dependent_marked AS (
				UPDATE jobs j
				   SET charge_status='outcome_unknown'
				  FROM marked
				 WHERE j.charge_batch_id=$2 AND j.charge_status<>'charged'
				RETURNING j.id
			)
			SELECT source.buyer_id,source.source_status,
			       COALESCE((SELECT count(*) FROM inserted),0)::bigint,
			       COALESCE((SELECT count(*) FROM marked),0)::bigint,
			       COALESCE((SELECT count(*) FROM dependent_marked),0)::bigint
			  FROM source`,
			operationKey, sourceID, buyerID, customerID, paymentMethodID, amountCents, currency,
		).Scan(&actualBuyer, &sourceStatus, &insertedRows, &sourceUpdatedRows, &dependentUpdatedRows)
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
	if insertedRows == 1 {
		if sourceUpdatedRows != 1 {
			return false, "", fmt.Errorf("buyer charge %s source lost its request-boundary CAS", operationKey)
		}
		// dependentUpdatedRows is deliberately returned by the batch CTE so
		// its write remains part of this one statement. The update is allowed
		// to affect zero rows when every dependent job is already charged.
		_ = dependentUpdatedRows
		if err := tx.Commit(ctx); err != nil {
			return false, "", err
		}
		return true, operationKey, nil
	}

	if insertedRows == 0 {
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
	return false, "", fmt.Errorf("buyer charge %s source operation insert returned an invalid row count", operationKey)
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
// immutable operation row and the failed reason. The single data-modifying
// statement is the transaction boundary: if it cannot commit, callers must
// treat the provider outcome as unknown instead.
func (s *Store) MarkBuyerChargeDefinitelyFailed(ctx context.Context, operationKey string, cause error) error {
	reason := "Stripe explicitly rejected the charge before cash movement"
	if cause != nil {
		reason = truncate(cause.Error(), 500)
	}
	var sourceKind, status string
	var jobID, batchID *uuid.UUID
	var sourceUpdatedRows, operationUpdatedRows int64
	err := s.pool.QueryRow(ctx, `
		WITH operation AS MATERIALIZED (
			SELECT source_kind,job_id,charge_batch_id,status
			  FROM buyer_charge_operations
			 WHERE operation_key=$1
			 FOR UPDATE
		), job_updated AS MATERIALIZED (
			UPDATE jobs j
			   SET charge_status='failed',charge_next_at=NULL
			  FROM operation
			 WHERE operation.status='outcome_unknown'
			   AND operation.source_kind='job'
			   AND operation.job_id IS NOT NULL
			   AND j.id=operation.job_id
			   AND j.charge_status IN ('outcome_unknown','failed')
			RETURNING j.id
		), batch_updated AS MATERIALIZED (
			UPDATE charge_batches b
			   SET status='attempting',next_at=NULL
			  FROM operation
			 WHERE operation.status='outcome_unknown'
			   AND operation.source_kind='batch'
			   AND operation.charge_batch_id IS NOT NULL
			   AND b.id=operation.charge_batch_id
			   AND b.status IN ('attempting','outcome_unknown')
			RETURNING b.id
		), source_updated AS MATERIALIZED (
			SELECT 'job'::text AS source_kind,id FROM job_updated
			 UNION ALL
			SELECT 'batch'::text AS source_kind,id FROM batch_updated
		), updated AS MATERIALIZED (
			UPDATE buyer_charge_operations o
			   SET status='failed',last_error=$2,updated_at=now()
			  FROM operation
			  JOIN source_updated
			    ON source_updated.source_kind=operation.source_kind
			   AND ((operation.source_kind='job' AND source_updated.id=operation.job_id)
			        OR (operation.source_kind='batch' AND source_updated.id=operation.charge_batch_id))
			 WHERE o.operation_key=$1
			   AND operation.status='outcome_unknown'
			RETURNING o.operation_key
		)
		SELECT operation.source_kind,operation.job_id,operation.charge_batch_id,operation.status,
		       COALESCE((SELECT count(*) FROM source_updated),0)::bigint,
		       COALESCE((SELECT count(*) FROM updated),0)::bigint
		  FROM operation`, operationKey, reason).
		Scan(&sourceKind, &jobID, &batchID, &status, &sourceUpdatedRows, &operationUpdatedRows)
	if err != nil {
		return err
	}
	if status == "failed" {
		return nil
	}
	if status != "outcome_unknown" {
		return fmt.Errorf("buyer charge %s cannot become failed from status %q", operationKey, status)
	}
	switch {
	case sourceKind == "job" && jobID != nil:
		if sourceUpdatedRows != 1 {
			return fmt.Errorf("buyer charge %s source job lost its failure-state CAS", operationKey)
		}
	case sourceKind == "batch" && batchID != nil:
		if sourceUpdatedRows != 1 {
			return fmt.Errorf("buyer charge %s source batch lost its failure-state CAS", operationKey)
		}
	default:
		return fmt.Errorf("buyer charge %s has invalid source binding", operationKey)
	}
	if operationUpdatedRows != 1 {
		return fmt.Errorf("buyer charge %s lost its failure-state operation CAS", operationKey)
	}
	return nil
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
	if sourceKind, sourceID, ok := parseCanonicalBuyerChargeOperationKey(operationKey); ok {
		err := s.confirmReconciledBuyerCharge(ctx, sourceKind, sourceID, charge)
		if !errors.Is(err, errBuyerChargeOperationNotFound) {
			return err
		}
		// The canonical job/batch namespace is also a valid caller-supplied
		// reference for a prepaid top-up. If no job/batch operation exists, keep
		// the historical top-up fallback rather than turning a webhook into a
		// new charge authority.
		return s.reconcilePrepaidTopup(ctx, operationKey, charge)
	}

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

// parseCanonicalBuyerChargeOperationKey extracts only the two operation-key
// formats Merc itself emits. The exact spelling check prevents a provider
// metadata value such as job-cas-<uuid> from being treated as an authority to
// address an arbitrary source; those legacy/operator references use the
// durable lookup below.
func parseCanonicalBuyerChargeOperationKey(operationKey string) (string, uuid.UUID, bool) {
	operationKey = strings.TrimSpace(operationKey)
	for _, prefix := range []struct {
		key  string
		kind string
	}{
		{key: "job-", kind: "job"},
		{key: "cxbatch-", kind: "batch"},
	} {
		if !strings.HasPrefix(operationKey, prefix.key) {
			continue
		}
		rawID := strings.TrimPrefix(operationKey, prefix.key)
		id, err := uuid.Parse(rawID)
		if err != nil || id == uuid.Nil || operationKey != prefix.key+id.String() {
			return "", uuid.Nil, false
		}
		return prefix.kind, id, true
	}
	return "", uuid.Nil, false
}

// confirmReconciledBuyerCharge uses the same locked confirmation CTE as the
// direct charge path, but requires the durable buyer_charge_operations row to
// exist. The statement's require-operation predicate makes an absent operation
// a zero-write result, so ReconcileBuyerChargeOperation can preserve the
// prepaid/legacy fallback without committing a cash fact from provider metadata
// alone.
func (s *Store) confirmReconciledBuyerCharge(
	ctx context.Context, sourceKind string, sourceID uuid.UUID, charge ChargeResult,
) error {
	if err := validateChargeResultShape(charge); err != nil {
		return fmt.Errorf("refusing invalid reconciled charge confirmation: %w", err)
	}
	out, err := s.confirmChargeInOneStatement(ctx, s.pool, sourceKind, sourceID, &charge, true)
	if errors.Is(err, pgx.ErrNoRows) {
		return errBuyerChargeOperationNotFound
	}
	if err != nil {
		if out.authorityCurrency != "" {
			return fmt.Errorf("%s %s cannot confirm %s: %w", sourceKind, sourceID, charge.Currency, err)
		}
		return err
	}
	if !out.operationPresent {
		return errBuyerChargeOperationNotFound
	}
	label := "job"
	if sourceKind == "batch" {
		label = "charge batch"
	}
	if err := validateChargeConfirmation(label, sourceKind, sourceID, charge, out); err != nil {
		return err
	}
	return nil
}
