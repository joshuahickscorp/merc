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
// buyer in the current settlement currency (0 if no row yet). Historical
// buckets remain available to their source-bound settlement/refund paths and
// are never numerically relabelled. This is not free_credit_usd.
func (s *Store) BuyerPrepaidBalanceMicros(ctx context.Context, buyerID uuid.UUID) (int64, error) {
	return s.buyerPrepaidBalanceMicrosInCurrency(ctx, buyerID, SettlementCurrencyCode())
}

func (s *Store) buyerPrepaidBalanceMicrosInCurrency(ctx context.Context, buyerID uuid.UUID, currency string) (int64, error) {
	cur, err := ParseCurrency(currency)
	if err != nil {
		return 0, err
	}
	var bal int64
	err = s.pool.QueryRow(ctx, `
		SELECT COALESCE((SELECT balance_micros FROM buyer_prepaid_balances
		                  WHERE buyer_id=$1 AND currency=$2), 0)`,
		buyerID, cur.Code()).Scan(&bal)
	return bal, err
}

// PrepaidPendingTopupMinorUnits reports money the buyer's card was already asked for
// that the balance does not yet hold. A top-up that is refused because it
// already crossed the Stripe boundary leaves the buyer with exactly one
// question — did the deposit land? — and balance alone cannot answer it.
//
// The durable schema predates multi-currency settlement and names the column
// amount_cents. Its values are ISO minor units for the row currency; this
// public boundary must not turn that legacy storage name into a false promise
// that every currency has cents.
func (s *Store) PrepaidPendingTopupMinorUnits(ctx context.Context, buyerID uuid.UUID) (int64, int64, error) {
	currency := SettlementCurrencyCode()
	if _, err := ParseCurrency(currency); err != nil {
		return 0, 0, err
	}
	var count, cents int64
	err := s.pool.QueryRow(ctx, `
		SELECT count(*), COALESCE(SUM(amount_cents),0) FROM prepaid_topup_operations
		 WHERE buyer_id=$1 AND currency=$2 AND status='pending'`, buyerID, currency).Scan(&count, &cents)
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
	currency := SettlementCurrencyCode()
	if _, err := ParseCurrency(currency); err != nil {
		return prepaidTopupArm{}, err
	}
	tag, err := s.pool.Exec(ctx, `
		INSERT INTO prepaid_topup_operations
		  (operation_key,buyer_id,amount_cents,currency,status)
		VALUES ($1,$2,$3,$4,'pending')
		ON CONFLICT (operation_key) DO NOTHING`,
		operationKey, buyerID, amountCents, currency)
	if err != nil {
		return prepaidTopupArm{}, err
	}
	if tag.RowsAffected() == 1 {
		return prepaidTopupArm{State: prepaidTopupArmed}, nil
	}
	var (
		storedBuyer    uuid.UUID
		storedCents    int64
		status         string
		storedCurrency string
		intent, chID   string
	)
	if err := s.pool.QueryRow(ctx, `
		SELECT buyer_id, amount_cents, currency, status,
		       COALESCE(payment_intent,''), COALESCE(charge_id,'')
		  FROM prepaid_topup_operations WHERE operation_key=$1`, operationKey,
	).Scan(&storedBuyer, &storedCents, &storedCurrency, &status, &intent, &chID); err != nil {
		return prepaidTopupArm{}, err
	}
	if storedBuyer != buyerID || storedCents != amountCents || storedCurrency != currency {
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
	if operationKey == "" || buyerID == uuid.Nil {
		return fmt.Errorf("invalid prepaid top-up credit")
	}
	if err := validateChargeResultShape(charge); err != nil {
		return fmt.Errorf("invalid prepaid top-up credit: %w", err)
	}
	chargeCurrency, err := ParseCurrency(charge.Currency)
	if err != nil {
		return fmt.Errorf("invalid prepaid top-up credit currency: %w", err)
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)

	var status string
	var storedBuyer uuid.UUID
	var amountCents int64
	var storedCurrency string
	err = tx.QueryRow(ctx, `
		SELECT buyer_id, amount_cents, currency, status FROM prepaid_topup_operations
		 WHERE operation_key=$1 FOR UPDATE`, operationKey,
	).Scan(&storedBuyer, &amountCents, &storedCurrency, &status)
	if errors.Is(err, pgx.ErrNoRows) {
		// Webhook may race ahead of BeginPrepaidTopup on a lost response; insert pending.
		if _, err := tx.Exec(ctx, `
			INSERT INTO prepaid_topup_operations
			  (operation_key,buyer_id,amount_cents,currency,status)
			VALUES ($1,$2,$3,$4,'pending')`,
			operationKey, buyerID, charge.ReceivedCents, chargeCurrency.Code()); err != nil {
			return err
		}
		storedBuyer, amountCents, storedCurrency, status = buyerID, charge.ReceivedCents, chargeCurrency.Code(), "pending"
	} else if err != nil {
		return err
	}
	if storedBuyer != buyerID {
		return fmt.Errorf("top-up %s buyer mismatch", operationKey)
	}
	if amountCents != charge.ReceivedCents {
		return fmt.Errorf("top-up %s amount mismatch: stored=%d charge=%d", operationKey, amountCents, charge.ReceivedCents)
	}
	settlement, err := ParseCurrency(storedCurrency)
	if err != nil {
		return fmt.Errorf("top-up %s has invalid stored currency: %w", operationKey, err)
	}
	if chargeCurrency.Code() != settlement.Code() {
		return fmt.Errorf("top-up %s currency mismatch: stored=%s charge=%s", operationKey, settlement.Code(), charge.Currency)
	}
	micros, err := settlement.MinorToMicros(charge.ReceivedCents)
	if err != nil {
		return err
	}
	if status == "succeeded" {
		return tx.Commit(ctx)
	}
	if status != "pending" {
		return fmt.Errorf("top-up %s is %s, cannot credit", operationKey, status)
	}

	if _, err := tx.Exec(ctx, `
		INSERT INTO buyer_prepaid_balances (buyer_id, currency, balance_micros, updated_at)
		VALUES ($1, $2, $3, now())
		ON CONFLICT (buyer_id,currency) DO UPDATE
		  SET balance_micros = buyer_prepaid_balances.balance_micros + EXCLUDED.balance_micros,
		      updated_at = now()`, buyerID, settlement.Code(), micros); err != nil {
		return err
	}
	buyer := buyerID
	if _, err := insertLedgerEntryIfAbsentByRefTx(ctx, tx, ledgerInsert{
		Kind: KindPrepaidTopup, BuyerID: &buyer, AmountMicros: micros,
		Currency: settlement.Code(), CurrencyAuthority: ledgerCurrencyAuthorityPrepaid,
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
		charge.RequestedCents, charge.ReceivedCents, settlement.Code()); err != nil {
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
	Currency     string
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
//
// Lock order (buyer money rails): pg_advisory_xact_lock("realtime-buyer-funding|"+buyerID)
// first, then buyer_prepaid_balances FOR UPDATE. Do not reorder the advisory
// after the balance row; do not take buyers or offer locks on this path.
// Admin revalidation / mutation-replay locks are orthogonal (different keys /
// tables) and are taken earlier in this function.
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
		slices, cents, currency, err := prepaidRefundSlicesTx(ctx, tx, operationKey)
		if err != nil {
			return prepaidRefundPlan{}, err
		}
		if err := tx.Commit(ctx); err != nil {
			return prepaidRefundPlan{}, err
		}
		return prepaidRefundPlan{OperationKey: operationKey, Currency: currency, Cents: cents, Slices: slices, Replayed: true}, nil
	}
	settlement, err := SettlementCurrency()
	if err != nil {
		return prepaidRefundPlan{}, err
	}
	currency := settlement.Code()

	// Same buyer-funding advisory as realtime / free-credit / prepaid admit.
	// Taken before the balance row so concurrent authorize cannot observe a
	// non-locking balance subquery between our check and debit, and so we do
	// not invert the hierarchy against rails that take advisory then buyers.
	if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtextextended($1, 0))`,
		"realtime-buyer-funding|"+buyerID.String()); err != nil {
		return prepaidRefundPlan{}, err
	}

	if _, err := tx.Exec(ctx, `
		INSERT INTO buyer_prepaid_balances (buyer_id, currency, balance_micros)
		VALUES ($1, $2, 0) ON CONFLICT (buyer_id,currency) DO NOTHING`, buyerID, currency); err != nil {
		return prepaidRefundPlan{}, err
	}
	var balance int64
	if err := tx.QueryRow(ctx, `
		SELECT balance_micros FROM buyer_prepaid_balances
		 WHERE buyer_id=$1 AND currency=$2 FOR UPDATE`, buyerID, currency).Scan(&balance); err != nil {
		return prepaidRefundPlan{}, err
	}
	// Only the unreserved remainder is refundable. Live prepay jobs were admitted
	// against the frozen part of this balance and settlement still has to debit it.
	reserved, err := prepaidOpenReservationMicrosInCurrency(ctx, tx, buyerID, currency)
	if err != nil {
		return prepaidRefundPlan{}, err
	}
	// Pure non-envelope EXECUTING ceilings are held at admission via
	// sqlOpenNonEnvelopeExecutingCeilingMicros but are outside
	// sqlBuyerOpenExposureMicros / prepaidOpenReservationMicros (that omission
	// keeps free-credit batch exposure out of prepaid open-reservation).
	//
	// Funding source at refund time — not determinable from the contract's own
	// spend rows. While state=EXECUTING there is no buyer_charge, no
	// prepaid_debit, and no funding-source column on execution_contracts.
	// evaluateRealtimeBuyerFunding pools free_credit + prepaid against committed
	// money at admit; maybeDebitPrepaidForRealtimeTx chooses free-first only at
	// settle. Spend rows appear only after settle, when the contract is no
	// longer EXECUTING and this hold term is already zero. From persisted state
	// alone every pure non-envelope EXECUTING ceiling is therefore ambiguous:
	// free credit alone may cover admit today, but free can be consumed before
	// settle and prepaid may still be required.
	//
	// Subtract the full hold anyway: refusing a refund is recoverable (buyer
	// retries after settle); paying out cash that settlement still needs is not.
	// This can over-hold prepaid when free credit alone covered admission —
	// deliberate and conservative. Same fragment admission uses; does not change
	// sqlBuyerOpenExposureMicros or any settled amount.
	var executingHold int64
	if err := tx.QueryRow(ctx,
		`SELECT (`+sqlOpenNonEnvelopeExecutingCeilingMicros("$1", "$2")+`)`,
		buyerID, currency,
	).Scan(&executingHold); err != nil {
		return prepaidRefundPlan{}, err
	}
	available := balance - reserved - executingHold
	if available <= 0 {
		return prepaidRefundPlan{}, errInsufficientPrepaid
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
	slices, err := planPrepaidRefundSlicesTx(ctx, tx, operationKey, buyerID, currency, cents)
	if err != nil {
		return prepaidRefundPlan{}, err
	}
	tag, err := tx.Exec(ctx, `
		UPDATE buyer_prepaid_balances
		   SET balance_micros = balance_micros - $2, updated_at = now()
		 WHERE buyer_id=$1 AND currency=$3 AND balance_micros >= $2`, buyerID, refundMicros, currency)
	if err != nil {
		return prepaidRefundPlan{}, err
	}
	if tag.RowsAffected() != 1 {
		return prepaidRefundPlan{}, errInsufficientPrepaid
	}
	buyer := buyerID
	if _, err := insertLedgerEntryIfAbsentByRefTx(ctx, tx, ledgerInsert{
		Kind: KindPrepaidRefund, BuyerID: &buyer, AmountMicros: -refundMicros,
		Currency: currency, CurrencyAuthority: ledgerCurrencyAuthorityPrepaid,
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
			currency, slice.PaymentIntent); err != nil {
			return prepaidRefundPlan{}, err
		}
	}
	if err := insertAdminMutationAction(ctx, tx, actor, intent, nil, nil, nil,
		map[string]any{
			"prepaid_balance_micros": balance, "reserved_micros": reserved,
			"executing_hold_micros": executingHold, "refundable_micros": available,
		},
		map[string]any{
			"prepaid_balance_micros": balance - refundMicros, "reserved_micros": reserved,
			"executing_hold_micros": executingHold, "refunded_micros": refundMicros,
		},
	); err != nil {
		return prepaidRefundPlan{}, err
	}
	if err := tx.Commit(ctx); err != nil {
		return prepaidRefundPlan{}, err
	}
	return prepaidRefundPlan{OperationKey: operationKey, Currency: currency, Cents: cents, Slices: slices}, nil
}

// planPrepaidRefundSlicesTx allocates the requested cents across the payment
// intents that funded the buyer, net of what earlier refunds already returned
// against each one. Failing to cover the amount aborts the transaction rather
// than refunding money no collection can back.
func planPrepaidRefundSlicesTx(
	ctx context.Context, tx pgx.Tx, operationKey string, buyerID uuid.UUID, currency string, cents int64,
) ([]prepaidRefundSlice, error) {
	rows, err := tx.Query(ctx, `
		SELECT t.payment_intent,
		       t.amount_cents - COALESCE((
		         SELECT SUM(r.amount_cents) FROM prepaid_refund_operations r
		          WHERE r.payment_intent = t.payment_intent AND r.currency=t.currency), 0) AS refundable
		  FROM prepaid_topup_operations t
		 WHERE t.buyer_id=$1 AND t.currency=$2
		   AND t.status='succeeded' AND t.payment_intent IS NOT NULL
		 ORDER BY t.credited_at DESC, t.operation_key DESC`, buyerID, currency)
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
func prepaidRefundSlicesTx(ctx context.Context, tx pgx.Tx, operationKey string) ([]prepaidRefundSlice, int64, string, error) {
	rows, err := tx.Query(ctx, `
		SELECT operation_key, COALESCE(payment_intent,''), amount_cents,
		       COALESCE(stripe_refund_id,''), currency
		  FROM prepaid_refund_operations
		 WHERE refund_key = $1
		 ORDER BY operation_key`, operationKey)
	if err != nil {
		return nil, 0, "", err
	}
	defer rows.Close()
	var (
		slices   []prepaidRefundSlice
		total    int64
		currency string
	)
	for rows.Next() {
		var slice prepaidRefundSlice
		var rowCurrency string
		if err := rows.Scan(&slice.OperationKey, &slice.PaymentIntent, &slice.Cents, &slice.RefundID, &rowCurrency); err != nil {
			return nil, 0, "", err
		}
		if currency == "" {
			currency = rowCurrency
		} else if currency != rowCurrency {
			return nil, 0, "", fmt.Errorf("prepaid refund %s mixes currencies", operationKey)
		}
		slices = append(slices, slice)
		total += slice.Cents
	}
	if err := rows.Err(); err != nil {
		return nil, 0, "", err
	}
	if len(slices) == 0 || currency == "" {
		return nil, 0, "", fmt.Errorf("prepaid refund %s has no durable slices", operationKey)
	}
	if _, err := ParseCurrency(currency); err != nil {
		return nil, 0, "", err
	}
	return slices, total, currency, nil
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
	currency := SettlementCurrencyCode()
	if _, err := ParseCurrency(currency); err != nil {
		return err
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	if _, err := tx.Exec(ctx, `
		INSERT INTO buyer_prepaid_balances (buyer_id, currency, balance_micros, updated_at)
		VALUES ($1, $2, $3, now())
		ON CONFLICT (buyer_id,currency) DO UPDATE
		  SET balance_micros = buyer_prepaid_balances.balance_micros + EXCLUDED.balance_micros,
		      updated_at = now()`, buyerID, currency, micros); err != nil {
		return err
	}
	buyer := buyerID
	if _, err := insertLedgerEntryIfAbsentByRefTx(ctx, tx, ledgerInsert{
		Kind: KindPrepaidTopup, BuyerID: &buyer, AmountMicros: micros,
		Currency: currency, CurrencyAuthority: ledgerCurrencyAuthorityPrepaid,
		PayoutStatus: PayoutReleased, PayoutRef: ref,
	}); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

// BuyerPrepaidAvailableMicros is an informational view of materialised balance
// less open prepaid reservations (sqlBuyerOpenExposureMicros) and pure
// non-envelope EXECUTING ceilings. Matches BeginPrepaidRefund's refundable
// quantity (without locking). Admission uses a locking reservation helper
// instead, so this read can never be mistaken for authorization.
//
// The EXECUTING sibling is held even when free credit may have covered
// admission: funding source is not determinable from the contract's spend rows
// while EXECUTING (no charge/debit yet; free-vs-prepaid is chosen only at
// settle — see BeginPrepaidRefund). sqlBuyerOpenExposureMicros is unchanged.
func (s *Store) BuyerPrepaidAvailableMicros(ctx context.Context, buyerID uuid.UUID) (int64, error) {
	currency := SettlementCurrencyCode()
	bal, err := s.buyerPrepaidBalanceMicrosInCurrency(ctx, buyerID, currency)
	if err != nil {
		return 0, err
	}
	reserved, err := prepaidOpenReservationMicrosInCurrency(ctx, s.pool, buyerID, currency)
	if err != nil {
		return 0, err
	}
	var executingHold int64
	if err := s.pool.QueryRow(ctx,
		`SELECT (`+sqlOpenNonEnvelopeExecutingCeilingMicros("$1", "$2")+`)`,
		buyerID, currency,
	).Scan(&executingHold); err != nil {
		return 0, err
	}
	held := reserved + executingHold
	if bal <= held {
		return 0, nil
	}
	return bal - held, nil
}

// reservePrepaidForJobTx is the admission-side serialization point. It takes
// the same buyer-funding advisory realtime and free-credit use (first — before
// any row lock), then locks the buyer's balance row and includes every other
// open job's *remaining* frozen reservation and every active service lease's
// maximum, so neither concurrent submits nor a concurrent settlement can
// oversubscribe cash already collected from the buyer.
//
// Lock order: pg_advisory_xact_lock("realtime-buyer-funding|"+buyerID) →
// buyer_prepaid_balances FOR UPDATE. Do not take offer/capacity locks before
// this; do not reorder the advisory after the balance row.
func reservePrepaidForJobTx(ctx context.Context, tx pgx.Tx, buyerID uuid.UUID, amountMicros int64) error {
	if buyerID == uuid.Nil || amountMicros <= 0 {
		return fmt.Errorf("prepaid reservation requires buyer and positive ledger micros")
	}
	currency := SettlementCurrencyCode()
	if _, err := ParseCurrency(currency); err != nil {
		return err
	}
	// Same lock key as evaluateRealtimeBuyerFunding / reserveFreeCreditForJobTx
	// — one buyer-serialised critical section across prepaid batch, free-credit
	// batch, and realtime funding.
	if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtextextended($1, 0))`,
		"realtime-buyer-funding|"+buyerID.String()); err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, `
		INSERT INTO buyer_prepaid_balances (buyer_id, currency, balance_micros)
		VALUES ($1, $2, 0) ON CONFLICT (buyer_id,currency) DO NOTHING`, buyerID, currency); err != nil {
		return err
	}
	var balance int64
	if err := tx.QueryRow(ctx, `
		SELECT balance_micros FROM buyer_prepaid_balances
		 WHERE buyer_id=$1 AND currency=$2 FOR UPDATE`, buyerID, currency).Scan(&balance); err != nil {
		return err
	}
	reserved, err := prepaidOpenReservationMicrosInCurrency(ctx, tx, buyerID, currency)
	if err != nil {
		return err
	}
	// Pure non-envelope EXECUTING ceilings are held by realtime/free-credit via
	// sqlBuyerCommittedMoneyMicros but are intentionally outside the prepaid
	// open-exposure expression (sqlBuyerOpenExposureMicros must not treat
	// free-credit-only batch work as prepaid cash). Admission and refund both
	// add this sibling explicitly. After the shared advisory serialises
	// check/commit/check, admission still has to observe a committed realtime
	// ceiling funded from this balance — otherwise reverse-order (realtime
	// first, prepaid second) re-admits and oversubscribes. Same fragment
	// realtime uses; not a second lock key and not a change to
	// sqlBuyerOpenExposureMicros.
	var executingHold int64
	if err := tx.QueryRow(ctx,
		`SELECT (`+sqlOpenNonEnvelopeExecutingCeilingMicros("$1", "$2")+`)`,
		buyerID, currency,
	).Scan(&executingHold); err != nil {
		return err
	}
	if balance-reserved-executingHold < amountMicros {
		return errInsufficientPrepaid
	}
	return nil
}

// reservePrepaidForServiceLeaseTx is the service-specific admission boundary.
// A service request reaches the store directly rather than through job submit,
// so it must also prove that its buyer is still active before reserving cash.
// The underlying reservation query includes active services, which gives this
// path the same serializable cash guarantee as batch admission.
//
// Lock order: funding advisory first (via reservePrepaidForJobTx), then the
// existing buyers FOR UPDATE active check is taken *before* the nested call
// would re-acquire the advisory. Taking buyers before the advisory deadlocks
// against realtime/free-credit (advisory → buyers). So: advisory, then buyers,
// then balance — matching free-credit's hierarchy. The nested
// reservePrepaidForJobTx re-takes the advisory (xact locks are reentrant).
func reservePrepaidForServiceLeaseTx(ctx context.Context, tx pgx.Tx, buyerID uuid.UUID, amountMicros int64) error {
	if buyerID == uuid.Nil {
		return errNotFound
	}
	// Advisory before buyers — same key, same order as free-credit / realtime.
	if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtextextended($1, 0))`,
		"realtime-buyer-funding|"+buyerID.String()); err != nil {
		return err
	}
	var active bool
	err := tx.QueryRow(ctx, `SELECT deleted_at IS NULL FROM buyers WHERE id=$1 FOR UPDATE`, buyerID).Scan(&active)
	if errors.Is(err, pgx.ErrNoRows) {
		return errNotFound
	}
	if err != nil {
		return err
	}
	if !active {
		return errNotFound
	}
	return reservePrepaidForJobTx(ctx, tx, buyerID, amountMicros)
}

// prepaidOpenReservationMicros sums the unfunded portion of frozen plans for
// live prepay jobs plus the full frozen maximum for active service leases. A
// task/SLA debit consumes balance and reduces a job reservation by the same
// amount; a service lease deliberately retains its entire maximum until it is
// terminal because its continuous settlement path has no per-task debit yet.
// Terminal jobs and terminal leases release unused reserve by simply leaving
// this query.
//
// Execution envelopes contribute two mutually exclusive terms. An ACTIVE
// envelope holds (cap - spent). Envelope-funded EXECUTING contracts are
// excluded from a second hold while that envelope is ACTIVE, matching
// evaluateRealtimeBuyerFunding: counting both would double-hold. The moment
// ReleaseExpiredExecutionEnvelopes flips the envelope off ACTIVE, the first
// term drops to zero — so the second term must pick up any still-RESERVED
// spend bound to a live EXECUTING contract. Without that fallback an admin
// prepaid refund of "available" can drain the cash still backing in-flight
// work, and settlement then fails to debit. The amount is ceil(reserved_nanos
// / 1000) from execution_envelope_spends — integer micros, no float path.
func prepaidOpenReservationMicros(ctx context.Context, db ledgerExec, buyerID uuid.UUID) (int64, error) {
	return prepaidOpenReservationMicrosInCurrency(ctx, db, buyerID, SettlementCurrencyCode())
}

func prepaidOpenReservationMicrosInCurrency(ctx context.Context, db ledgerExec, buyerID uuid.UUID, currency string) (int64, error) {
	if _, err := ParseCurrency(currency); err != nil {
		return 0, err
	}
	var reserved int64
	// Singular open-exposure definition (buyer_open_exposure.go). Exactly the
	// four canonical terms; no free-credit/realtime siblings here — those are
	// not prepaid cash holds.
	err := db.QueryRow(ctx, `
		SELECT `+sqlBuyerOpenExposureMicros("$1", "$2"),
		buyerID, currency).Scan(&reserved)
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

func prepaidExecutionContractDebitRef(contractID uuid.UUID) string {
	return "prepaid-execution-contract-" + contractID.String()
}

// prepaidExecutionContractRestoreRef keys the once-only prepaid_restore that
// pairs with prepaidExecutionContractDebitRef on an internal realtime refund.
func prepaidExecutionContractRestoreRef(contractID uuid.UUID) string {
	return "prepaid-execution-contract-restore-" + contractID.String()
}

// prepaidTaskRestoreRef keys the once-only prepaid_restore that pairs with a
// task-scoped prepaid_debit (dispute buyer refund). Uniqueness is also
// enforced by (task_id, kind); the ref is audit/idempotency evidence.
func prepaidTaskRestoreRef(taskID uuid.UUID) string {
	return "prepaid-task-restore-" + taskID.String()
}

// prepaidSLAPremiumRestoreRef keys the once-only prepaid_balance_return for an
// SLA premium debit returned on SLA miss. Not a KindPrepaidRestore — see
// restorePrepaidForSLAPremiumRefundTx.
func prepaidSLAPremiumRestoreRef(jobID uuid.UUID) string {
	return "prepaid-sla-restore-" + jobID.String()
}

// restorePrepaidByDebitRefTx re-materialises prepaid after an internal refund
// undoes a charge that was funded by a prepaid_debit keyed by debitRef.
//
// Pairing: evaluateRealtimeBuyerFunding adds back prepaid_debit so spent
// (buyer_charge) is not double-counted against the reduced balance. When a
// refund zeros spent via buyer_refund, the materialised balance must rise by
// the same amount and a KindPrepaidRestore row must net the debit in the
// capacity sum — otherwise available becomes free+B while cash is only B−D.
//
// Idempotency: insertLedgerEntryIfAbsentByRefTx on restoreRef. Balance is
// credited only when that insert lands a new row; a second call is a no-op.
func restorePrepaidByDebitRefTx(
	ctx context.Context, tx pgx.Tx,
	buyerID uuid.UUID, currency string,
	debitRef, restoreRef string,
	restoreMicros int64,
) error {
	if buyerID == uuid.Nil || debitRef == "" || restoreRef == "" {
		return fmt.Errorf("prepaid restore requires buyer, debit ref, and restore ref")
	}
	if restoreMicros <= 0 {
		return nil
	}
	cur, err := ParseCurrency(currency)
	if err != nil {
		return err
	}
	// Fail closed if the debit we are reversing is missing or disagrees.
	var debitAmount int64
	var debitBuyer *uuid.UUID
	var debitCurrency string
	err = tx.QueryRow(ctx, `
		SELECT (amount_usd*1000000)::bigint, buyer_id, currency
		  FROM ledger_entries
		 WHERE kind=$1 AND payout_ref=$2`, KindPrepaidDebit, debitRef).
		Scan(&debitAmount, &debitBuyer, &debitCurrency)
	if errors.Is(err, pgx.ErrNoRows) {
		return fmt.Errorf("prepaid restore: no debit at ref %s", debitRef)
	}
	if err != nil {
		return err
	}
	if debitAmount >= 0 {
		return fmt.Errorf("prepaid restore: debit ref %s has non-negative amount %d", debitRef, debitAmount)
	}
	debitAbs := -debitAmount
	if restoreMicros > debitAbs {
		return fmt.Errorf("prepaid restore %d exceeds debit %d at ref %s", restoreMicros, debitAbs, debitRef)
	}
	if debitCurrency != cur.Code() || !sameOptionalUUID(debitBuyer, &buyerID) {
		return fmt.Errorf("prepaid restore debit identity mismatch at ref %s", debitRef)
	}
	buyer := buyerID
	ct, err := insertLedgerEntryIfAbsentByRefTx(ctx, tx, ledgerInsert{
		Kind: KindPrepaidRestore, BuyerID: &buyer, AmountMicros: restoreMicros,
		Currency: cur.Code(), CurrencyAuthority: ledgerCurrencyAuthorityPrepaid,
		PayoutStatus: PayoutReleased, PayoutRef: restoreRef,
	})
	if err != nil {
		return err
	}
	if ct.RowsAffected() == 0 {
		// Restore already receipted — do not credit balance again.
		return nil
	}
	return creditPrepaidBalanceTx(ctx, tx, buyerID, restoreMicros, cur.Code())
}

// restorePrepaidForExecutionContractRefundTx restores materialised prepaid for
// a realtime contract that was settled with a prepaid_debit. No-op when settle
// was free-credit-only (no debit row).
func restorePrepaidForExecutionContractRefundTx(
	ctx context.Context, tx pgx.Tx, buyerID, contractID uuid.UUID, currency string,
) error {
	if contractID == uuid.Nil {
		return fmt.Errorf("prepaid execution restore requires contract id")
	}
	debitRef := prepaidExecutionContractDebitRef(contractID)
	var debitAbs int64
	err := tx.QueryRow(ctx, `
		SELECT COALESCE((-amount_usd*1000000)::bigint, 0)
		  FROM ledger_entries
		 WHERE kind=$1 AND payout_ref=$2`, KindPrepaidDebit, debitRef).Scan(&debitAbs)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil
	}
	if err != nil {
		return err
	}
	if debitAbs <= 0 {
		return nil
	}
	return restorePrepaidByDebitRefTx(ctx, tx, buyerID, currency,
		debitRef, prepaidExecutionContractRestoreRef(contractID), debitAbs)
}

// restorePrepaidForSLAPremiumRefundTx restores materialised prepaid for the
// job-level SLA premium debit when SettleJobSLA credits an sla_refund. Amount
// is the refund micros (≤ debit). No-op when the premium was not prepaid-funded.
//
// Unlike KindPrepaidRestore paths (realtime refund, dispute refund), this
// writes KindPrepaidBalanceReturn and does NOT net the prepaid_debit out of
// evaluateRealtimeBuyerFunding's +prepaidDebited term. sla_refund is not in
// the buyer spent sum (buyer_charge/buyer_refund only), so the debit must keep
// cancelling the still-present premium buyer_charge in capacity math. Only
// the materialised balance is raised — that is the cash an admin prepaid
// refund can return. The kind is the typed distinction; do not reintroduce a
// payout_ref prefix filter in the capacity formula.
//
// Idempotency: SettleJobSLA decides once (sla_met IS NULL). This helper runs
// only on that first miss path inside the same transaction as the sla_met
// stamp, so a second call never re-enters. A restoreRef receipt still lands
// so a future refactor cannot double-credit without tripping the if-absent
// insert.
func restorePrepaidForSLAPremiumRefundTx(
	ctx context.Context, tx pgx.Tx, buyerID, jobID uuid.UUID, currency string, refundMicros int64,
) error {
	if jobID == uuid.Nil || refundMicros <= 0 {
		return nil
	}
	debitRef := prepaidSLAPremiumDebitRef(jobID)
	var debitAbs int64
	var debitBuyer *uuid.UUID
	var debitCurrency string
	err := tx.QueryRow(ctx, `
		SELECT (-amount_usd*1000000)::bigint, buyer_id, currency
		  FROM ledger_entries
		 WHERE kind=$1 AND payout_ref=$2`, KindPrepaidDebit, debitRef).
		Scan(&debitAbs, &debitBuyer, &debitCurrency)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil
	}
	if err != nil {
		return err
	}
	if debitAbs <= 0 {
		return nil
	}
	cur, err := ParseCurrency(currency)
	if err != nil {
		return err
	}
	if debitCurrency != cur.Code() || !sameOptionalUUID(debitBuyer, &buyerID) {
		return fmt.Errorf("prepaid SLA restore debit identity mismatch at ref %s", debitRef)
	}
	restore := refundMicros
	if restore > debitAbs {
		restore = debitAbs
	}
	buyer := buyerID
	// Receipt for idempotency. KindPrepaidBalanceReturn is excluded from the
	// capacity prepaidDebited sum by kind (not by payout_ref prefix).
	ct, err := insertLedgerEntryIfAbsentByRefTx(ctx, tx, ledgerInsert{
		Kind: KindPrepaidBalanceReturn, BuyerID: &buyer, AmountMicros: restore,
		Currency: cur.Code(), CurrencyAuthority: ledgerCurrencyAuthorityPrepaid,
		PayoutStatus: PayoutReleased, PayoutRef: prepaidSLAPremiumRestoreRef(jobID),
	})
	if err != nil {
		return err
	}
	if ct.RowsAffected() == 0 {
		return nil
	}
	return creditPrepaidBalanceTx(ctx, tx, buyerID, restore, cur.Code())
}

// restorePrepaidForTaskDebitTx re-materialises prepaid for a task-scoped
// prepaid_debit after buyer_refund has zeroed spent. Writes KindPrepaidRestore
// so capacity nets the debit. Idempotent on (task_id, kind): a second dispute
// or funding replay cannot credit twice.
func restorePrepaidForTaskDebitTx(
	ctx context.Context, tx pgx.Tx, buyerID, taskID uuid.UUID, currency string,
) error {
	if buyerID == uuid.Nil || taskID == uuid.Nil {
		return fmt.Errorf("prepaid task restore requires buyer and task")
	}
	cur, err := ParseCurrency(currency)
	if err != nil {
		return err
	}
	var debitAmount int64
	var debitBuyer *uuid.UUID
	var debitCurrency string
	err = tx.QueryRow(ctx, `
		SELECT (amount_usd*1000000)::bigint, buyer_id, currency
		  FROM ledger_entries
		 WHERE kind=$1 AND task_id=$2`, KindPrepaidDebit, taskID).
		Scan(&debitAmount, &debitBuyer, &debitCurrency)
	if errors.Is(err, pgx.ErrNoRows) {
		return fmt.Errorf("prepaid task restore: no debit on task %s", taskID)
	}
	if err != nil {
		return err
	}
	if debitAmount >= 0 {
		return fmt.Errorf("prepaid task restore: debit on task %s has non-negative amount %d", taskID, debitAmount)
	}
	restoreMicros := -debitAmount
	if debitCurrency != cur.Code() || !sameOptionalUUID(debitBuyer, &buyerID) {
		return fmt.Errorf("prepaid task restore debit identity mismatch on task %s", taskID)
	}
	// Fail closed if spent was not zeroed for this task — netting a debit while
	// buyer_charge still counts would under-hold capacity.
	var netSpentMicros int64
	if err := tx.QueryRow(ctx, `
		SELECT COALESCE((SUM(-le.amount_usd) * 1000000)::bigint, 0)
		  FROM ledger_entries le
		 WHERE le.task_id=$1 AND le.kind IN ('buyer_charge','buyer_refund')
		   AND le.currency=$2`, taskID, cur.Code()).Scan(&netSpentMicros); err != nil {
		return err
	}
	if netSpentMicros != 0 {
		return fmt.Errorf("prepaid task restore refused: task %s still has net spent micros %d", taskID, netSpentMicros)
	}
	buyer := buyerID
	task := taskID
	ct, err := insertLedgerEntryOnTaskConflictDoNothingTx(ctx, tx, ledgerInsert{
		Kind: KindPrepaidRestore, BuyerID: &buyer, TaskID: &task,
		AmountMicros: restoreMicros, Currency: cur.Code(),
		CurrencyAuthority: ledgerCurrencyAuthorityPrepaid,
		PayoutStatus:      PayoutReleased, PayoutRef: prepaidTaskRestoreRef(taskID),
	})
	if err != nil {
		return err
	}
	if ct.RowsAffected() == 0 {
		return nil
	}
	return creditPrepaidBalanceTx(ctx, tx, buyerID, restoreMicros, cur.Code())
}

// restorePrepaidForDisputeJobTasksTx restores materialised prepaid for every
// task-scoped prepaid_debit on the job that has a matching buyer_refund.
// Each task restore is independently idempotent; bulk credit without receipts
// is forbidden.
func restorePrepaidForDisputeJobTasksTx(
	ctx context.Context, tx pgx.Tx, buyerID, jobID uuid.UUID, currency string,
) error {
	if buyerID == uuid.Nil || jobID == uuid.Nil {
		return fmt.Errorf("prepaid dispute restore requires buyer and job")
	}
	rows, err := tx.Query(ctx, `
		SELECT d.task_id
		  FROM ledger_entries d
		  JOIN tasks t ON t.id = d.task_id
		  JOIN ledger_entries r ON r.task_id = d.task_id AND r.kind = 'buyer_refund'
		 WHERE t.job_id = $1 AND d.kind = 'prepaid_debit'
		   AND d.task_id IS NOT NULL
		   AND (-d.amount_usd) IS NOT DISTINCT FROM r.amount_usd
		 ORDER BY d.task_id`, jobID)
	if err != nil {
		return err
	}
	defer rows.Close()
	var taskIDs []uuid.UUID
	for rows.Next() {
		var taskID uuid.UUID
		if err := rows.Scan(&taskID); err != nil {
			return err
		}
		taskIDs = append(taskIDs, taskID)
	}
	if err := rows.Err(); err != nil {
		return err
	}
	for _, taskID := range taskIDs {
		if err := restorePrepaidForTaskDebitTx(ctx, tx, buyerID, taskID, currency); err != nil {
			return err
		}
	}
	return nil
}

// maybeDebitPrepaidForRealtimeTx consumes prepaid for a settled realtime buyer
// charge when free credit alone does not cover it. Free-credit sandbox charges
// (grant fully covers the charge) stay ledger-only; when free covers only a
// prefix of the charge, prepaid is debited for the residual only — matching
// evaluateRealtimeBuyerFunding's pooled free+prepaid admit arithmetic.
//
// Arithmetic is int64 micros after the free-grant column read: chargeMicros is
// the authority and must survive exactly. Free credit still arrives via the
// existing float free_credit_usd column (the precision exposure already present
// at admit); no new float step is introduced on residual or conservation.
//
// Lock order: takes buyers FOR UPDATE (and possibly buyer_prepaid_balances)
// before any offer capacity release. Callers must not hold an offer row lock
// first — that inverts the realtime hierarchy documented on
// evaluateRealtimeBuyerFunding and deadlocks under single-buyer concurrency.
func maybeDebitPrepaidForRealtimeTx(ctx context.Context, tx pgx.Tx, buyerID, contractID uuid.UUID, chargeMicros int64) error {
	if buyerID == uuid.Nil || contractID == uuid.Nil || chargeMicros <= 0 {
		return fmt.Errorf("realtime prepaid debit requires buyer, contract, and positive micros")
	}
	var currency string
	var freeCredit, spent float64
	if err := tx.QueryRow(ctx, `
		SELECT c.currency,
		       CASE WHEN c.currency='usd' THEN b.free_credit_usd::float8 ELSE 0::float8 END,
		       COALESCE((SELECT -sum(le.amount_usd) FROM ledger_entries le
		                 WHERE le.buyer_id=b.id
		                   AND le.currency=c.currency
		                   AND le.kind IN ('buyer_charge','buyer_refund')),0)::float8
		  FROM buyers b JOIN execution_contracts c ON c.id=$2 AND c.buyer_id=b.id
		 WHERE b.id=$1 FOR UPDATE OF b`, buyerID, contractID).
		Scan(&currency, &freeCredit, &spent); err != nil {
		return err
	}
	if _, err := ParseCurrency(currency); err != nil {
		return err
	}
	// spent already includes this contract's buyer_charge (written earlier in the
	// same transaction). freeRemainingMicros is the grant available before it,
	// computed entirely in micros so residual = charge − covered is exact.
	freeRemainingMicros := usdToMicros(freeCredit) - (usdToMicros(spent) - chargeMicros)
	covered := freeRemainingMicros
	if covered < 0 {
		covered = 0
	}
	if covered > chargeMicros {
		covered = chargeMicros
	}
	residualMicros := chargeMicros - covered
	if residualMicros == 0 {
		return nil
	}
	return debitPrepaidForExecutionContractTx(ctx, tx, buyerID, contractID, residualMicros)
}

// debitPrepaidByRefTx is the shared path for prepaid debits keyed by an
// immutable payout_ref (execution contracts and service leases). It never sets
// ExecutionContractID: ledger_entries allows only one row per
// (execution_contract_id, kind), and buyer_charge already owns that slot.
// Idempotency is the payout_ref.
func debitPrepaidByRefTx(ctx context.Context, tx pgx.Tx, buyerID uuid.UUID, amountMicros int64, currency, ref, requireMsg, conflictMsg string) error {
	if buyerID == uuid.Nil || amountMicros <= 0 || ref == "" {
		return fmt.Errorf("%s", requireMsg)
	}
	cur, err := ParseCurrency(currency)
	if err != nil {
		return fmt.Errorf("%s: %w", requireMsg, err)
	}
	var existingAmount int64
	var existingBuyer *uuid.UUID
	var existingCurrency string
	err = tx.QueryRow(ctx, `SELECT (amount_usd*1000000)::bigint,buyer_id,currency
		FROM ledger_entries WHERE kind=$1 AND payout_ref=$2`, KindPrepaidDebit, ref).
		Scan(&existingAmount, &existingBuyer, &existingCurrency)
	if err == nil {
		if existingAmount != -amountMicros || existingCurrency != cur.Code() || !sameOptionalUUID(existingBuyer, &buyerID) {
			return fmt.Errorf("%s", conflictMsg)
		}
		return nil
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return err
	}
	if _, err := tx.Exec(ctx, `
		INSERT INTO buyer_prepaid_balances (buyer_id, currency, balance_micros)
		VALUES ($1, $2, 0) ON CONFLICT (buyer_id,currency) DO NOTHING`, buyerID, cur.Code()); err != nil {
		return err
	}
	var balance int64
	if err := tx.QueryRow(ctx, `SELECT balance_micros FROM buyer_prepaid_balances
		WHERE buyer_id=$1 AND currency=$2 FOR UPDATE`, buyerID, cur.Code()).Scan(&balance); err != nil {
		return err
	}
	if balance < amountMicros {
		return errInsufficientPrepaid
	}
	ct, err := tx.Exec(ctx, `UPDATE buyer_prepaid_balances
		SET balance_micros=balance_micros-$2,updated_at=now()
		WHERE buyer_id=$1 AND currency=$3 AND balance_micros >= $2`, buyerID, amountMicros, cur.Code())
	if err != nil {
		return err
	}
	if ct.RowsAffected() != 1 {
		return errInsufficientPrepaid
	}
	buyer := buyerID
	ct, err = insertLedgerEntryIfAbsentByRefTx(ctx, tx, ledgerInsert{
		Kind: KindPrepaidDebit, BuyerID: &buyer,
		AmountMicros: -amountMicros, Currency: cur.Code(), CurrencyAuthority: ledgerCurrencyAuthorityPrepaid,
		PayoutStatus: PayoutReleased, PayoutRef: ref,
	})
	if err != nil {
		return err
	}
	if ct.RowsAffected() == 0 {
		_, err = tx.Exec(ctx, `UPDATE buyer_prepaid_balances
			SET balance_micros=balance_micros+$2,updated_at=now()
			WHERE buyer_id=$1 AND currency=$3`, buyerID, amountMicros, cur.Code())
	}
	return err
}

// debitPrepaidForExecutionContractTx is the realtime counterpart of task and
// service-lease prepaid settlement. It is keyed by an immutable payout
// reference because realtime charges have no task_id.
func debitPrepaidForExecutionContractTx(ctx context.Context, tx pgx.Tx, buyerID, contractID uuid.UUID, amountMicros int64) error {
	if contractID == uuid.Nil {
		return fmt.Errorf("prepaid execution debit requires buyer, contract, and positive ledger micros")
	}
	var currency string
	if err := tx.QueryRow(ctx, `SELECT currency FROM execution_contracts WHERE id=$1 AND buyer_id=$2`, contractID, buyerID).Scan(&currency); err != nil {
		return err
	}
	return debitPrepaidByRefTx(ctx, tx, buyerID, amountMicros, currency,
		prepaidExecutionContractDebitRef(contractID),
		"prepaid execution debit requires buyer, contract, and positive ledger micros",
		fmt.Sprintf("conflicting prepaid execution debit for contract %s", contractID),
	)
}

// debitPrepaidForServiceLeaseTx consumes the final, cumulative service charge
// from the balance that admitted the lease. It is keyed by the immutable lease
// reference because service settlement has no task. The existing-debit check
// happens before touching the materialised balance, so a retry can neither
// double-debit nor transiently fail because another terminal transition already
// consumed the same funds.
func debitPrepaidForServiceLeaseTx(ctx context.Context, tx pgx.Tx, buyerID, leaseID uuid.UUID, amountMicros int64) error {
	if leaseID == uuid.Nil {
		return fmt.Errorf("prepaid service debit requires buyer, lease, and positive ledger micros")
	}
	var currency string
	if err := tx.QueryRow(ctx, `SELECT pricing_decision->>'currency' FROM service_leases WHERE id=$1 AND buyer_id=$2`, leaseID, buyerID).Scan(&currency); err != nil {
		return err
	}
	return debitPrepaidByRefTx(ctx, tx, buyerID, amountMicros, currency,
		prepaidServiceLeaseDebitRef(leaseID),
		"prepaid service debit requires buyer, lease, and positive ledger micros",
		fmt.Sprintf("conflicting prepaid service debit for lease %s", leaseID),
	)
}

// creditPrepaidBalanceTx mutates buyer_prepaid_balances only. It does not write
// a prepaid_topup cash row — the cash already sat on the platform as prepaid
// liability that was spent and is now un-spent. It does not write a capacity
// netting receipt either.
//
// Callers that reverse a prepaid_debit after spent has been zeroed MUST go
// through restorePrepaidByDebitRefTx / restorePrepaidForTaskDebitTx (which
// write KindPrepaidRestore and then call this). Bare credit without a restore
// receipt is how phantom realtime capacity is minted. SLA materialisation is
// the documented exception and uses KindPrepaidBalanceReturn via
// restorePrepaidForSLAPremiumRefundTx.
func creditPrepaidBalanceTx(ctx context.Context, tx pgx.Tx, buyerID uuid.UUID, amountMicros int64, currency string) error {
	if amountMicros <= 0 {
		return fmt.Errorf("prepaid credit requires positive micro-units, got %d", amountMicros)
	}
	if buyerID == uuid.Nil {
		return fmt.Errorf("prepaid credit requires buyer_id")
	}
	cur, err := ParseCurrency(currency)
	if err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, `
		INSERT INTO buyer_prepaid_balances (buyer_id, currency, balance_micros, updated_at)
		VALUES ($1, $2, $3, now())
		ON CONFLICT (buyer_id,currency) DO UPDATE
		  SET balance_micros = buyer_prepaid_balances.balance_micros + EXCLUDED.balance_micros,
		      updated_at = now()`, buyerID, cur.Code(), amountMicros); err != nil {
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
	var currency string
	if err := tx.QueryRow(ctx, `
		SELECT j.currency FROM tasks t JOIN jobs j ON j.id=t.job_id
		 WHERE t.id=$1 AND j.buyer_id=$2`, taskID, buyerID).Scan(&currency); err != nil {
		return err
	}
	cur, err := ParseCurrency(currency)
	if err != nil {
		return err
	}
	// Ensure row exists so FOR UPDATE can serialise concurrent first-time spenders.
	if _, err := tx.Exec(ctx, `
		INSERT INTO buyer_prepaid_balances (buyer_id, currency, balance_micros)
		VALUES ($1, $2, 0) ON CONFLICT (buyer_id,currency) DO NOTHING`, buyerID, cur.Code()); err != nil {
		return err
	}
	var bal int64
	if err := tx.QueryRow(ctx, `
		SELECT balance_micros FROM buyer_prepaid_balances
		 WHERE buyer_id=$1 AND currency=$2 FOR UPDATE`, buyerID, cur.Code()).Scan(&bal); err != nil {
		return err
	}
	if bal < amountMicros {
		return errInsufficientPrepaid
	}
	ct, err := tx.Exec(ctx, `
		UPDATE buyer_prepaid_balances
		   SET balance_micros = balance_micros - $2, updated_at = now()
		 WHERE buyer_id=$1 AND currency=$3 AND balance_micros >= $2`, buyerID, amountMicros, cur.Code())
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
		AmountMicros: -amountMicros, Currency: cur.Code(), CurrencyAuthority: ledgerCurrencyAuthorityPrepaid,
		PayoutStatus: PayoutReleased,
	})
	if err != nil {
		return err
	}
	if ct.RowsAffected() == 0 {
		// Idempotent retry: reverse the materialised debit we just applied.
		if _, err := tx.Exec(ctx, `
			UPDATE buyer_prepaid_balances
			   SET balance_micros = balance_micros + $2, updated_at = now()
			 WHERE buyer_id=$1 AND currency=$3`, buyerID, amountMicros, cur.Code()); err != nil {
			return err
		}
	}
	return nil
}

func prepaidSLAPremiumDebitRef(jobID uuid.UUID) string { return "prepaid-sla-" + jobID.String() }

// prepaidServiceLeaseDebitRef is deliberately distinct from every job-level
// debit reference. A service has no task ID, so using the job task uniqueness
// key would either make the debit non-idempotent or incorrectly attach it to
// an unrelated buyer workload.
func prepaidServiceLeaseDebitRef(leaseID uuid.UUID) string {
	return "prepaid-service-lease-" + leaseID.String()
}

// debitPrepaidForSLAPremiumTx is the job-level counterpart to task settlement.
// It is keyed by an immutable payout reference because SLA premium charges have
// no task_id. The caller creates the corresponding buyer_charge in this same
// transaction before requesting the debit.
func debitPrepaidForSLAPremiumTx(ctx context.Context, tx pgx.Tx, buyerID, jobID uuid.UUID, amountMicros int64) error {
	if amountMicros <= 0 {
		return fmt.Errorf("prepaid SLA debit requires positive micro-USD, got %d", amountMicros)
	}
	var currency string
	if err := tx.QueryRow(ctx, `SELECT currency FROM jobs WHERE id=$1 AND buyer_id=$2`, jobID, buyerID).Scan(&currency); err != nil {
		return err
	}
	cur, err := ParseCurrency(currency)
	if err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, `
		INSERT INTO buyer_prepaid_balances (buyer_id, currency, balance_micros)
		VALUES ($1, $2, 0) ON CONFLICT (buyer_id,currency) DO NOTHING`, buyerID, cur.Code()); err != nil {
		return err
	}
	var balance int64
	if err := tx.QueryRow(ctx, `
		SELECT balance_micros FROM buyer_prepaid_balances
		 WHERE buyer_id=$1 AND currency=$2 FOR UPDATE`, buyerID, cur.Code()).Scan(&balance); err != nil {
		return err
	}
	if balance < amountMicros {
		return errInsufficientPrepaid
	}
	ct, err := tx.Exec(ctx, `
		UPDATE buyer_prepaid_balances SET balance_micros=balance_micros-$2, updated_at=now()
		 WHERE buyer_id=$1 AND currency=$3 AND balance_micros >= $2`, buyerID, amountMicros, cur.Code())
	if err != nil {
		return err
	}
	if ct.RowsAffected() != 1 {
		return errInsufficientPrepaid
	}
	buyer := buyerID
	ct, err = insertLedgerEntryIfAbsentByRefTx(ctx, tx, ledgerInsert{
		Kind: KindPrepaidDebit, BuyerID: &buyer, AmountMicros: -amountMicros,
		Currency: cur.Code(), CurrencyAuthority: ledgerCurrencyAuthorityPrepaid,
		PayoutStatus: PayoutReleased, PayoutRef: prepaidSLAPremiumDebitRef(jobID),
	})
	if err != nil {
		return err
	}
	if ct.RowsAffected() == 0 {
		_, err = tx.Exec(ctx, `
			UPDATE buyer_prepaid_balances SET balance_micros=balance_micros+$2, updated_at=now()
			 WHERE buyer_id=$1 AND currency=$3`, buyerID, amountMicros, cur.Code())
	}
	return err
}

// DebitPrepaidForTask is the pool-level wrapper used by unit tests.
