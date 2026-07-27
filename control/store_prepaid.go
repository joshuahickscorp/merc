package main

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

// Prepaid-balance store methods.
//
// Kept beside store_billing.go rather than inside it so the prepaid liability
// -- which is money the platform holds and owes back -- has one obvious home,
// and so that the split of store.go stays a mechanical move rather than
// absorbing new behaviour.

// errInsufficientPrepaid is returned when a debit or refund cannot cover the
// requested amount from the buyer's prepaid liability.
var errInsufficientPrepaid = errors.New("insufficient prepaid balance")

// BuyerPrepaidBalanceMicros returns the materialised prepaid liability for the
// buyer (0 if no row yet). This is not free_credit_usd — see schema comment.

// BuyerPrepaidBalanceMicros returns the materialised prepaid liability for the
// buyer (0 if no row yet). This is not free_credit_usd — see schema comment.
func (s *Store) BuyerPrepaidBalanceMicros(ctx context.Context, buyerID uuid.UUID) (int64, error) {
	var bal int64
	err := s.pool.QueryRow(ctx, `
		SELECT COALESCE((SELECT balance_micros FROM buyer_prepaid_balances WHERE buyer_id=$1), 0)`,
		buyerID).Scan(&bal)
	return bal, err
}

// BuyerPrepaidAvailableMicros is balance minus open-job estimated spend
// (queued/running/verifying). Used at job admission so concurrent open jobs
// cannot oversubscribe the same liability.

func (s *Store) BeginPrepaidTopup(ctx context.Context, operationKey string, buyerID uuid.UUID, amountCents int64) error {
	operationKey = strings.TrimSpace(operationKey)
	if operationKey == "" || buyerID == uuid.Nil || amountCents <= 0 {
		return fmt.Errorf("invalid prepaid top-up identity")
	}
	_, err := s.pool.Exec(ctx, `
		INSERT INTO prepaid_topup_operations
		  (operation_key,buyer_id,amount_cents,currency,status)
		VALUES ($1,$2,$3,$4,'pending')
		ON CONFLICT (operation_key) DO NOTHING`,
		operationKey, buyerID, amountCents, SettlementCurrencyCode())
	return err
}

// CreditPrepaidTopup is the single-writer credit path: materialised balance +
// prepaid_topup ledger row + top-up cash collection. Idempotent on operation_key
// and payment_intent.
func (s *Store) CreditPrepaidTopup(ctx context.Context, operationKey string, buyerID uuid.UUID, charge ChargeResult) error {
	operationKey = strings.TrimSpace(operationKey)
	if operationKey == "" || buyerID == uuid.Nil || charge.PaymentIntentID == "" || charge.ChargeID == "" ||
		charge.ReceivedCents <= 0 || charge.ReceivedCents != charge.RequestedCents || RequireSettlementCurrency(charge.Currency) != nil {
		return fmt.Errorf("invalid prepaid top-up credit")
	}
	micros := charge.ReceivedCents * microUSDPerCent
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)

	var status string
	var storedBuyer uuid.UUID
	var amountCents int64
	err = tx.QueryRow(ctx, `
		SELECT buyer_id, amount_cents, status FROM prepaid_topup_operations
		 WHERE operation_key=$1 FOR UPDATE`, operationKey,
	).Scan(&storedBuyer, &amountCents, &status)
	if errors.Is(err, pgx.ErrNoRows) {
		// Webhook may race ahead of BeginPrepaidTopup on a lost response; insert pending.
		if _, err := tx.Exec(ctx, `
			INSERT INTO prepaid_topup_operations
			  (operation_key,buyer_id,amount_cents,currency,status)
			VALUES ($1,$2,$3,$4,'pending')`,
			operationKey, buyerID, charge.ReceivedCents, SettlementCurrencyCode()); err != nil {
			return err
		}
		storedBuyer, amountCents, status = buyerID, charge.ReceivedCents, "pending"
	} else if err != nil {
		return err
	}
	if storedBuyer != buyerID {
		return fmt.Errorf("top-up %s buyer mismatch", operationKey)
	}
	if amountCents != charge.ReceivedCents {
		return fmt.Errorf("top-up %s amount mismatch: stored=%d charge=%d", operationKey, amountCents, charge.ReceivedCents)
	}
	if status == "succeeded" {
		return tx.Commit(ctx)
	}
	if status != "pending" {
		return fmt.Errorf("top-up %s is %s, cannot credit", operationKey, status)
	}

	if _, err := tx.Exec(ctx, `
		INSERT INTO buyer_prepaid_balances (buyer_id, balance_micros, updated_at)
		VALUES ($1, $2, now())
		ON CONFLICT (buyer_id) DO UPDATE
		  SET balance_micros = buyer_prepaid_balances.balance_micros + EXCLUDED.balance_micros,
		      updated_at = now()`, buyerID, micros); err != nil {
		return err
	}
	buyer := buyerID
	if _, err := insertLedgerEntryIfAbsentByRefTx(ctx, tx, ledgerInsert{
		Kind: KindPrepaidTopup, BuyerID: &buyer, AmountMicros: micros,
		PayoutStatus: PayoutReleased, PayoutRef: charge.PaymentIntentID,
	}); err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, `
		INSERT INTO buyer_cash_collections
		  (payment_intent,charge_id,buyer_id,source_kind,job_id,charge_batch_id,
		   requested_cents,received_cents,currency)
		VALUES ($1,$2,$3,'topup',NULL,NULL,$4,$5,$6)
		ON CONFLICT (payment_intent) DO NOTHING`,
		charge.PaymentIntentID, charge.ChargeID, buyerID,
		charge.RequestedCents, charge.ReceivedCents, SettlementCurrencyCode()); err != nil {
		return err
	}
	ct, err := tx.Exec(ctx, `
		UPDATE prepaid_topup_operations
		   SET status='succeeded', payment_intent=$2, charge_id=$3,
		       credited_at=now(), updated_at=now()
		 WHERE operation_key=$1 AND status='pending'`,
		operationKey, charge.PaymentIntentID, charge.ChargeID)
	if err != nil {
		return err
	}
	if ct.RowsAffected() != 1 {
		return fmt.Errorf("top-up %s lost credit CAS", operationKey)
	}
	return tx.Commit(ctx)
}

// debitPrepaidForTaskTx reduces prepaid liability for a settled task's buyer_charge.
// Concurrency: SELECT … FOR UPDATE on the balance row; the UPDATE requires
// balance_micros >= amount so two simultaneous debits cannot both succeed when
// only one fits.

func (s *Store) ListRefundableTopups(ctx context.Context, buyerID uuid.UUID) ([]refundableTopup, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT payment_intent, amount_cents
		  FROM prepaid_topup_operations
		 WHERE buyer_id=$1 AND status='succeeded' AND payment_intent IS NOT NULL
		 ORDER BY credited_at DESC, operation_key DESC`, buyerID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []refundableTopup
	for rows.Next() {
		var t refundableTopup
		if err := rows.Scan(&t.PaymentIntent, &t.AmountCents); err != nil {
			return nil, err
		}
		out = append(out, t)
	}
	return out, rows.Err()
}

// DebitPrepaidRefund returns unspent liability to the buyer (card refund already
// initiated by the caller). Records prepaid_refund ledger row and drops balance.

// DebitPrepaidRefund returns unspent liability to the buyer (card refund already
// initiated by the caller). Records prepaid_refund ledger row and drops balance.
func (s *Store) DebitPrepaidRefund(ctx context.Context, operationKey string, buyerID uuid.UUID, amountMicros int64, stripeRefundIDs string) error {
	if amountMicros <= 0 || buyerID == uuid.Nil || strings.TrimSpace(operationKey) == "" {
		return fmt.Errorf("invalid prepaid refund")
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	if _, err := tx.Exec(ctx, `
		INSERT INTO buyer_prepaid_balances (buyer_id, balance_micros)
		VALUES ($1, 0) ON CONFLICT (buyer_id) DO NOTHING`, buyerID); err != nil {
		return err
	}
	var bal int64
	if err := tx.QueryRow(ctx, `
		SELECT balance_micros FROM buyer_prepaid_balances
		 WHERE buyer_id=$1 FOR UPDATE`, buyerID).Scan(&bal); err != nil {
		return err
	}
	if bal < amountMicros {
		return errInsufficientPrepaid
	}
	ct, err := tx.Exec(ctx, `
		UPDATE buyer_prepaid_balances
		   SET balance_micros = balance_micros - $2, updated_at = now()
		 WHERE buyer_id=$1 AND balance_micros >= $2`, buyerID, amountMicros)
	if err != nil {
		return err
	}
	if ct.RowsAffected() != 1 {
		return errInsufficientPrepaid
	}
	buyer := buyerID
	if _, err := insertLedgerEntryIfAbsentByRefTx(ctx, tx, ledgerInsert{
		Kind: KindPrepaidRefund, BuyerID: &buyer, AmountMicros: -amountMicros,
		PayoutStatus: PayoutReleased, PayoutRef: operationKey,
	}); err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, `
		INSERT INTO prepaid_refund_operations
		  (operation_key,buyer_id,amount_cents,currency,status,stripe_refund_id)
		VALUES ($1,$2,$3,$4,'succeeded',$5)
		ON CONFLICT (operation_key) DO NOTHING`,
		operationKey, buyerID, amountMicros/microUSDPerCent, SettlementCurrencyCode(), stripeRefundIDs); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

// debitPrepaidForSLAPremiumTx debits prepaid for the job-level SLA premium
// buyer_charge (no task_id). Idempotent on payout_ref.

func (s *Store) PrepaidTopupPending(ctx context.Context, operationKey string) (buyerID uuid.UUID, amountCents int64, err error) {
	err = s.pool.QueryRow(ctx, `
		SELECT buyer_id, amount_cents FROM prepaid_topup_operations
		 WHERE operation_key=$1`, operationKey).Scan(&buyerID, &amountCents)
	if errors.Is(err, pgx.ErrNoRows) {
		return uuid.Nil, 0, errNotFound
	}
	return buyerID, amountCents, err
}

// CreditPrepaidTopup is the single-writer credit path: materialised balance +
// prepaid_topup ledger row + top-up cash collection. Idempotent on operation_key
// and payment_intent.

type refundableTopup struct {
	PaymentIntent string
	AmountCents   int64
}

// SeedPrepaidBalance is a test/admin helper that credits balance without Stripe.
// Production credits go only through CreditPrepaidTopup.
func (s *Store) SeedPrepaidBalance(ctx context.Context, buyerID uuid.UUID, micros int64, ref string) error {
	if micros <= 0 {
		return fmt.Errorf("seed amount must be positive")
	}
	if ref == "" {
		ref = "seed-" + uuid.NewString()
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	if _, err := tx.Exec(ctx, `
		INSERT INTO buyer_prepaid_balances (buyer_id, balance_micros, updated_at)
		VALUES ($1, $2, now())
		ON CONFLICT (buyer_id) DO UPDATE
		  SET balance_micros = buyer_prepaid_balances.balance_micros + EXCLUDED.balance_micros,
		      updated_at = now()`, buyerID, micros); err != nil {
		return err
	}
	buyer := buyerID
	if _, err := insertLedgerEntryIfAbsentByRefTx(ctx, tx, ledgerInsert{
		Kind: KindPrepaidTopup, BuyerID: &buyer, AmountMicros: micros,
		PayoutStatus: PayoutReleased, PayoutRef: ref,
	}); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

// BuyerPrepaidAvailableMicros is balance minus open-job estimated spend
// (queued/running/verifying). Used at job admission so concurrent open jobs
// cannot oversubscribe the same liability.
func (s *Store) BuyerPrepaidAvailableMicros(ctx context.Context, buyerID uuid.UUID) (int64, error) {
	var available int64
	err := s.pool.QueryRow(ctx, `
		SELECT GREATEST(
		  COALESCE((SELECT balance_micros FROM buyer_prepaid_balances WHERE buyer_id=$1), 0)
		  - COALESCE((
		      SELECT SUM((COALESCE(estimated_usd,0)*1000000)::bigint)
		        FROM jobs
		       WHERE buyer_id=$1 AND status IN ('queued','running','verifying')
		    ), 0),
		  0)`, buyerID).Scan(&available)
	return available, err
}

// DebitPrepaidForTask is the pool-level wrapper used by unit tests.
func (s *Store) DebitPrepaidForTask(ctx context.Context, buyerID, taskID uuid.UUID, amountMicros int64) error {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	if err := debitPrepaidForTaskTx(ctx, tx, buyerID, taskID, amountMicros); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

// debitPrepaidForTaskTx reduces prepaid liability for a settled task's buyer_charge.
// Concurrency: SELECT … FOR UPDATE on the balance row; the UPDATE requires
// balance_micros >= amount so two simultaneous debits cannot both succeed when
// only one fits.
func debitPrepaidForTaskTx(ctx context.Context, tx pgx.Tx, buyerID, taskID uuid.UUID, amountMicros int64) error {
	if amountMicros <= 0 {
		return fmt.Errorf("prepaid debit requires positive micro-USD, got %d", amountMicros)
	}
	// Ensure row exists so FOR UPDATE can serialise concurrent first-time spenders.
	if _, err := tx.Exec(ctx, `
		INSERT INTO buyer_prepaid_balances (buyer_id, balance_micros)
		VALUES ($1, 0) ON CONFLICT (buyer_id) DO NOTHING`, buyerID); err != nil {
		return err
	}
	var bal int64
	if err := tx.QueryRow(ctx, `
		SELECT balance_micros FROM buyer_prepaid_balances
		 WHERE buyer_id=$1 FOR UPDATE`, buyerID).Scan(&bal); err != nil {
		return err
	}
	if bal < amountMicros {
		return errInsufficientPrepaid
	}
	ct, err := tx.Exec(ctx, `
		UPDATE buyer_prepaid_balances
		   SET balance_micros = balance_micros - $2, updated_at = now()
		 WHERE buyer_id=$1 AND balance_micros >= $2`, buyerID, amountMicros)
	if err != nil {
		return err
	}
	if ct.RowsAffected() != 1 {
		return errInsufficientPrepaid
	}
	buyer := buyerID
	task := taskID
	// prepaid_debit is negative (liability reduction). Task/kind uniqueness
	// makes settlement retries idempotent.
	ct, err = insertLedgerEntryOnTaskConflictDoNothingTx(ctx, tx, ledgerInsert{
		Kind: KindPrepaidDebit, BuyerID: &buyer, TaskID: &task,
		AmountMicros: -amountMicros, PayoutStatus: PayoutReleased,
	})
	if err != nil {
		return err
	}
	if ct.RowsAffected() == 0 {
		// Idempotent retry: reverse the materialised debit we just applied.
		if _, err := tx.Exec(ctx, `
			UPDATE buyer_prepaid_balances
			   SET balance_micros = balance_micros + $2, updated_at = now()
			 WHERE buyer_id=$1`, buyerID, amountMicros); err != nil {
			return err
		}
	}
	return nil
}

// DebitPrepaidForTask is the pool-level wrapper used by unit tests.
