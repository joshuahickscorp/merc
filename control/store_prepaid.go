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

// PrepaidPendingTopupCents reports money the buyer's card was already asked for
// that the balance does not yet hold. A top-up that is refused because it
// already crossed the Stripe boundary leaves the buyer with exactly one
// question — did the deposit land? — and balance alone cannot answer it.
func (s *Store) PrepaidPendingTopupCents(ctx context.Context, buyerID uuid.UUID) (int64, int64, error) {
	var count, cents int64
	err := s.pool.QueryRow(ctx, `
		SELECT count(*), COALESCE(SUM(amount_cents),0) FROM prepaid_topup_operations
		 WHERE buyer_id=$1 AND status='pending'`, buyerID).Scan(&count, &cents)
	return count, cents, err
}

// errPrepaidTopupConflict is returned when an operation key is reused for a
// materially different top-up. Answering such a request from the stored row
// would charge the wrong amount or the wrong buyer.
var errPrepaidTopupConflict = errors.New("top-up idempotency key already belongs to a different request")

const (
	prepaidTopupArmed    = "armed"     // this request owns the Stripe charge
	prepaidTopupInFlight = "in_flight" // an earlier request already crossed the provider boundary
	prepaidTopupCredited = "credited"  // the balance already holds this money
)

type prepaidTopupArm struct {
	State         string
	PaymentIntent string
	ChargeID      string
}

// BeginPrepaidTopup is the durable request boundary for a card top-up: exactly
// one caller is told to charge, and every retry of the same operation key is
// answered from the stored row instead of creating a second payment intent.
func (s *Store) BeginPrepaidTopup(ctx context.Context, operationKey string, buyerID uuid.UUID, amountCents int64) (prepaidTopupArm, error) {
	operationKey = strings.TrimSpace(operationKey)
	if operationKey == "" || buyerID == uuid.Nil || amountCents <= 0 {
		return prepaidTopupArm{}, fmt.Errorf("invalid prepaid top-up identity")
	}
	tag, err := s.pool.Exec(ctx, `
		INSERT INTO prepaid_topup_operations
		  (operation_key,buyer_id,amount_cents,currency,status)
		VALUES ($1,$2,$3,$4,'pending')
		ON CONFLICT (operation_key) DO NOTHING`,
		operationKey, buyerID, amountCents, SettlementCurrencyCode())
	if err != nil {
		return prepaidTopupArm{}, err
	}
	if tag.RowsAffected() == 1 {
		return prepaidTopupArm{State: prepaidTopupArmed}, nil
	}
	var (
		storedBuyer  uuid.UUID
		storedCents  int64
		status       string
		intent, chID string
	)
	if err := s.pool.QueryRow(ctx, `
		SELECT buyer_id, amount_cents, status,
		       COALESCE(payment_intent,''), COALESCE(charge_id,'')
		  FROM prepaid_topup_operations WHERE operation_key=$1`, operationKey,
	).Scan(&storedBuyer, &storedCents, &status, &intent, &chID); err != nil {
		return prepaidTopupArm{}, err
	}
	if storedBuyer != buyerID || storedCents != amountCents {
		return prepaidTopupArm{}, errPrepaidTopupConflict
	}
	if status == "succeeded" {
		return prepaidTopupArm{State: prepaidTopupCredited, PaymentIntent: intent, ChargeID: chID}, nil
	}
	return prepaidTopupArm{State: prepaidTopupInFlight, PaymentIntent: intent, ChargeID: chID}, nil
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
	settlement, err := SettlementCurrency()
	if err != nil {
		return err
	}
	micros, err := settlement.MinorToMicros(charge.ReceivedCents)
	if err != nil {
		return err
	}
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

// prepaidRefundSlice is one payment intent's share of a refund. Refunds are
// traced back to the intents that actually collected the cash so a refund can
// never exceed what the buyer funded.
type prepaidRefundSlice struct {
	OperationKey  string
	PaymentIntent string
	Cents         int64
	RefundID      string
}

type prepaidRefundPlan struct {
	OperationKey string
	Cents        int64
	Slices       []prepaidRefundSlice
	Replayed     bool
}

// prepaidRefundOperationKey is derived, not random, so an operator replaying the
// same incident reference resumes the same refund instead of starting a second one.
func prepaidRefundOperationKey(buyerID uuid.UUID, correlationRef string) string {
	return "prepaid-refund-" + buyerID.String() + "-" + strings.TrimSpace(correlationRef)
}

// BeginPrepaidRefund debits the buyer's unreserved balance, writes the audited
// operation rows and returns the per-intent slices the caller must hand to
// Stripe. Everything durable happens here, before any provider call.
func (s *Store) BeginPrepaidRefund(
	ctx context.Context,
	actor AdminActor,
	buyerID uuid.UUID,
	reason, correlationRef string,
) (prepaidRefundPlan, error) {
	intent, err := prepareAdminMutation(actor, adminMutationIntent{
		Kind: adminActionPrepaidRefunded, TargetKind: adminTargetBuyer,
		TargetID: buyerID, Reason: reason, CorrelationRef: correlationRef,
	})
	if err != nil {
		return prepaidRefundPlan{}, err
	}
	operationKey := prepaidRefundOperationKey(buyerID, intent.CorrelationRef)
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return prepaidRefundPlan{}, err
	}
	defer tx.Rollback(ctx)
	if err := revalidateAdminActor(ctx, tx, actor); err != nil {
		return prepaidRefundPlan{}, err
	}
	replay, err := acquireAdminMutationReplay(ctx, tx, actor, intent)
	if err != nil {
		return prepaidRefundPlan{}, err
	}
	if replay.Found {
		// The balance was already debited by the original request. Hand back the
		// same slices so an interrupted refund finishes instead of doubling.
		slices, cents, err := prepaidRefundSlicesTx(ctx, tx, operationKey)
		if err != nil {
			return prepaidRefundPlan{}, err
		}
		if err := tx.Commit(ctx); err != nil {
			return prepaidRefundPlan{}, err
		}
		return prepaidRefundPlan{OperationKey: operationKey, Cents: cents, Slices: slices, Replayed: true}, nil
	}

	if _, err := tx.Exec(ctx, `
		INSERT INTO buyer_prepaid_balances (buyer_id, balance_micros)
		VALUES ($1, 0) ON CONFLICT (buyer_id) DO NOTHING`, buyerID); err != nil {
		return prepaidRefundPlan{}, err
	}
	var balance int64
	if err := tx.QueryRow(ctx, `
		SELECT balance_micros FROM buyer_prepaid_balances
		 WHERE buyer_id=$1 FOR UPDATE`, buyerID).Scan(&balance); err != nil {
		return prepaidRefundPlan{}, err
	}
	// Only the unreserved remainder is refundable. Live prepay jobs were admitted
	// against the frozen part of this balance and settlement still has to debit it.
	reserved, err := prepaidOpenReservationMicros(ctx, tx, buyerID)
	if err != nil {
		return prepaidRefundPlan{}, err
	}
	available := balance - reserved
	if available <= 0 {
		return prepaidRefundPlan{}, errInsufficientPrepaid
	}
	settlement, err := SettlementCurrency()
	if err != nil {
		return prepaidRefundPlan{}, err
	}
	microsPerMinor, err := settlement.MicrosPerMinorUnit()
	if err != nil {
		return prepaidRefundPlan{}, err
	}
	// Stripe refunds are integer settlement minor units; fractional minor dust
	// stays on the balance until another top-up or debit absorbs it.
	cents := available / microsPerMinor
	if cents <= 0 {
		return prepaidRefundPlan{}, fmt.Errorf(
			"unreserved prepaid balance %d micros is below one %s minor unit and cannot be refunded via card rails",
			available, settlement.Code())
	}
	refundMicros, err := settlement.MinorToMicros(cents)
	if err != nil {
		return prepaidRefundPlan{}, err
	}
	slices, err := planPrepaidRefundSlicesTx(ctx, tx, operationKey, buyerID, cents)
	if err != nil {
		return prepaidRefundPlan{}, err
	}
	tag, err := tx.Exec(ctx, `
		UPDATE buyer_prepaid_balances
		   SET balance_micros = balance_micros - $2, updated_at = now()
		 WHERE buyer_id=$1 AND balance_micros >= $2`, buyerID, refundMicros)
	if err != nil {
		return prepaidRefundPlan{}, err
	}
	if tag.RowsAffected() != 1 {
		return prepaidRefundPlan{}, errInsufficientPrepaid
	}
	buyer := buyerID
	if _, err := insertLedgerEntryIfAbsentByRefTx(ctx, tx, ledgerInsert{
		Kind: KindPrepaidRefund, BuyerID: &buyer, AmountMicros: -refundMicros,
		PayoutStatus: PayoutReleased, PayoutRef: operationKey,
	}); err != nil {
		return prepaidRefundPlan{}, err
	}
	for _, slice := range slices {
		if _, err := tx.Exec(ctx, `
			INSERT INTO prepaid_refund_operations
			  (operation_key,refund_key,buyer_id,amount_cents,currency,status,payment_intent)
			VALUES ($1,$2,$3,$4,$5,'pending',$6)`,
			slice.OperationKey, operationKey, buyerID, slice.Cents,
			SettlementCurrencyCode(), slice.PaymentIntent); err != nil {
			return prepaidRefundPlan{}, err
		}
	}
	if err := insertAdminMutationAction(ctx, tx, actor, intent, nil, nil, nil,
		map[string]any{"prepaid_balance_micros": balance, "reserved_micros": reserved, "refundable_micros": available},
		map[string]any{"prepaid_balance_micros": balance - refundMicros, "reserved_micros": reserved, "refunded_micros": refundMicros},
	); err != nil {
		return prepaidRefundPlan{}, err
	}
	if err := tx.Commit(ctx); err != nil {
		return prepaidRefundPlan{}, err
	}
	return prepaidRefundPlan{OperationKey: operationKey, Cents: cents, Slices: slices}, nil
}

// planPrepaidRefundSlicesTx allocates the requested cents across the payment
// intents that funded the buyer, net of what earlier refunds already returned
// against each one. Failing to cover the amount aborts the transaction rather
// than refunding money no collection can back.
func planPrepaidRefundSlicesTx(
	ctx context.Context, tx pgx.Tx, operationKey string, buyerID uuid.UUID, cents int64,
) ([]prepaidRefundSlice, error) {
	rows, err := tx.Query(ctx, `
		SELECT t.payment_intent,
		       t.amount_cents - COALESCE((
		         SELECT SUM(r.amount_cents) FROM prepaid_refund_operations r
		          WHERE r.payment_intent = t.payment_intent), 0) AS refundable
		  FROM prepaid_topup_operations t
		 WHERE t.buyer_id=$1 AND t.status='succeeded' AND t.payment_intent IS NOT NULL
		 ORDER BY t.credited_at DESC, t.operation_key DESC`, buyerID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var (
		slices    []prepaidRefundSlice
		remaining = cents
	)
	for rows.Next() {
		var intentID string
		var refundable int64
		if err := rows.Scan(&intentID, &refundable); err != nil {
			return nil, err
		}
		if remaining <= 0 || refundable <= 0 {
			continue
		}
		take := refundable
		if take > remaining {
			take = remaining
		}
		slices = append(slices, prepaidRefundSlice{
			OperationKey: operationKey + "-" + intentID, PaymentIntent: intentID, Cents: take,
		})
		remaining -= take
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	if remaining > 0 {
		return nil, fmt.Errorf(
			"refund would exceed funded value: %d of %d cents cannot be traced to an unrefunded top-up", remaining, cents)
	}
	return slices, nil
}

// prepaidRefundSlicesTx recovers the slices a refund already wrote. Membership
// is an equality test on refund_key, never a prefix match on the composite
// operation_key: the correlation reference inside that key is operator-typed and
// unrestricted, so a reference carrying a LIKE wildcard ('INC-100%') would match
// slices belonging to other refunds and replay them onto Stripe.
func prepaidRefundSlicesTx(ctx context.Context, tx pgx.Tx, operationKey string) ([]prepaidRefundSlice, int64, error) {
	rows, err := tx.Query(ctx, `
		SELECT operation_key, COALESCE(payment_intent,''), amount_cents, COALESCE(stripe_refund_id,'')
		  FROM prepaid_refund_operations
		 WHERE refund_key = $1
		 ORDER BY operation_key`, operationKey)
	if err != nil {
		return nil, 0, err
	}
	defer rows.Close()
	var (
		slices []prepaidRefundSlice
		total  int64
	)
	for rows.Next() {
		var slice prepaidRefundSlice
		if err := rows.Scan(&slice.OperationKey, &slice.PaymentIntent, &slice.Cents, &slice.RefundID); err != nil {
			return nil, 0, err
		}
		slices = append(slices, slice)
		total += slice.Cents
	}
	return slices, total, rows.Err()
}

// CompletePrepaidRefund records the provider's refund identifiers. It is the
// only step allowed to run after the Stripe call, because it moves no money.
func (s *Store) CompletePrepaidRefund(ctx context.Context, slices []prepaidRefundSlice) error {
	for _, slice := range slices {
		if strings.TrimSpace(slice.RefundID) == "" {
			return fmt.Errorf("prepaid refund %s has no provider refund id", slice.OperationKey)
		}
		if _, err := s.pool.Exec(ctx, `
			UPDATE prepaid_refund_operations
			   SET status='succeeded', stripe_refund_id=$2
			 WHERE operation_key=$1 AND status='pending'`, slice.OperationKey, slice.RefundID); err != nil {
			return err
		}
	}
	return nil
}

func (s *Store) PrepaidTopupPending(ctx context.Context, operationKey string) (buyerID uuid.UUID, amountCents int64, err error) {
	err = s.pool.QueryRow(ctx, `
		SELECT buyer_id, amount_cents FROM prepaid_topup_operations
		 WHERE operation_key=$1`, operationKey).Scan(&buyerID, &amountCents)
	if errors.Is(err, pgx.ErrNoRows) {
		return uuid.Nil, 0, errNotFound
	}
	return buyerID, amountCents, err
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

// BuyerPrepaidAvailableMicros is an informational view of materialised balance
// less every open prepay-required job's remaining frozen reservation. Admission
// uses reservePrepaidForJobTx instead, which locks the balance row.
func (s *Store) BuyerPrepaidAvailableMicros(ctx context.Context, buyerID uuid.UUID) (int64, error) {
	bal, err := s.BuyerPrepaidBalanceMicros(ctx, buyerID)
	if err != nil {
		return 0, err
	}
	reserved, err := prepaidOpenReservationMicros(ctx, s.pool, buyerID)
	if err != nil {
		return 0, err
	}
	if bal <= reserved {
		return 0, nil
	}
	return bal - reserved, nil
}

// reservePrepaidForJobTx is the admission-side serialization point. It locks
// the buyer's balance row and includes every other open job's *remaining*
// frozen reservation, so neither concurrent submits nor a concurrent task
// settlement can oversubscribe cash already collected from the buyer.
func reservePrepaidForJobTx(ctx context.Context, tx pgx.Tx, buyerID uuid.UUID, amountMicros int64) error {
	if buyerID == uuid.Nil || amountMicros <= 0 {
		return fmt.Errorf("prepaid reservation requires buyer and positive micro-USD")
	}
	if _, err := tx.Exec(ctx, `
		INSERT INTO buyer_prepaid_balances (buyer_id, balance_micros)
		VALUES ($1, 0) ON CONFLICT (buyer_id) DO NOTHING`, buyerID); err != nil {
		return err
	}
	var balance int64
	if err := tx.QueryRow(ctx, `
		SELECT balance_micros FROM buyer_prepaid_balances
		 WHERE buyer_id=$1 FOR UPDATE`, buyerID).Scan(&balance); err != nil {
		return err
	}
	reserved, err := prepaidOpenReservationMicros(ctx, tx, buyerID)
	if err != nil {
		return err
	}
	if balance-reserved < amountMicros {
		return errInsufficientPrepaid
	}
	return nil
}

// prepaidOpenReservationMicros sums the unfunded portion of frozen plans for
// live prepay jobs. A task/SLA debit consumes balance and reduces this virtual
// reservation by the same amount; terminal jobs release unused reserve by
// simply leaving this query.
func prepaidOpenReservationMicros(ctx context.Context, db ledgerExec, buyerID uuid.UUID) (int64, error) {
	var reserved int64
	err := db.QueryRow(ctx, `
		SELECT COALESCE(SUM(GREATEST(0::bigint,
			  (p.reserved_buyer_charge_usd * 1000000)::bigint - COALESCE((
			    SELECT SUM((-le.amount_usd * 1000000)::bigint)
			      FROM ledger_entries le
			     WHERE le.kind='prepaid_debit'
			       AND (le.task_id IN (SELECT id FROM tasks WHERE job_id=j.id)
			            OR le.payout_ref='prepaid-sla-' || j.id::text)
			  ), 0)
		)), 0)::bigint
		  FROM jobs j
		  JOIN job_economic_plans p ON p.job_id=j.id
		 WHERE j.buyer_id=$1 AND j.prepaid_required
		   AND j.status IN ('queued','running','verifying')`, buyerID).Scan(&reserved)
	return reserved, err
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

// creditPrepaidBalanceTx restores prepaid liability after a dispute (or other
// internal) refund. It does not write a prepaid_topup cash row — the cash
// already sat on the platform as prepaid liability that was spent and is now
// un-spent. Callers must enforce idempotency (e.g. job_dispute_refunds).
func creditPrepaidBalanceTx(ctx context.Context, tx pgx.Tx, buyerID uuid.UUID, amountMicros int64) error {
	if amountMicros <= 0 {
		return fmt.Errorf("prepaid credit requires positive micro-units, got %d", amountMicros)
	}
	if buyerID == uuid.Nil {
		return fmt.Errorf("prepaid credit requires buyer_id")
	}
	if _, err := tx.Exec(ctx, `
		INSERT INTO buyer_prepaid_balances (buyer_id, balance_micros, updated_at)
		VALUES ($1, $2, now())
		ON CONFLICT (buyer_id) DO UPDATE
		  SET balance_micros = buyer_prepaid_balances.balance_micros + EXCLUDED.balance_micros,
		      updated_at = now()`, buyerID, amountMicros); err != nil {
		return err
	}
	return nil
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

func prepaidSLAPremiumDebitRef(jobID uuid.UUID) string { return "prepaid-sla-" + jobID.String() }

// debitPrepaidForSLAPremiumTx is the job-level counterpart to task settlement.
// It is keyed by an immutable payout reference because SLA premium charges have
// no task_id. The caller creates the corresponding buyer_charge in this same
// transaction before requesting the debit.
func debitPrepaidForSLAPremiumTx(ctx context.Context, tx pgx.Tx, buyerID, jobID uuid.UUID, amountMicros int64) error {
	if amountMicros <= 0 {
		return fmt.Errorf("prepaid SLA debit requires positive micro-USD, got %d", amountMicros)
	}
	if _, err := tx.Exec(ctx, `
		INSERT INTO buyer_prepaid_balances (buyer_id, balance_micros)
		VALUES ($1, 0) ON CONFLICT (buyer_id) DO NOTHING`, buyerID); err != nil {
		return err
	}
	var balance int64
	if err := tx.QueryRow(ctx, `
		SELECT balance_micros FROM buyer_prepaid_balances WHERE buyer_id=$1 FOR UPDATE`, buyerID).Scan(&balance); err != nil {
		return err
	}
	if balance < amountMicros {
		return errInsufficientPrepaid
	}
	ct, err := tx.Exec(ctx, `
		UPDATE buyer_prepaid_balances SET balance_micros=balance_micros-$2, updated_at=now()
		 WHERE buyer_id=$1 AND balance_micros >= $2`, buyerID, amountMicros)
	if err != nil {
		return err
	}
	if ct.RowsAffected() != 1 {
		return errInsufficientPrepaid
	}
	buyer := buyerID
	ct, err = insertLedgerEntryIfAbsentByRefTx(ctx, tx, ledgerInsert{
		Kind: KindPrepaidDebit, BuyerID: &buyer, AmountMicros: -amountMicros,
		PayoutStatus: PayoutReleased, PayoutRef: prepaidSLAPremiumDebitRef(jobID),
	})
	if err != nil {
		return err
	}
	if ct.RowsAffected() == 0 {
		_, err = tx.Exec(ctx, `
			UPDATE buyer_prepaid_balances SET balance_micros=balance_micros+$2, updated_at=now()
			 WHERE buyer_id=$1`, buyerID, amountMicros)
	}
	return err
}

// DebitPrepaidForTask is the pool-level wrapper used by unit tests.
