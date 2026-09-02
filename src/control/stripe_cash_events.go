package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/jackc/pgx/v5"
)

const (
	stripeEventChargeRefunded         = "charge.refunded"
	stripeEventDisputeCreated         = "charge.dispute.created"
	stripeEventDisputeFundsWithdrawn  = "charge.dispute.funds_withdrawn"
	stripeEventDisputeFundsReinstated = "charge.dispute.funds_reinstated"
	stripeEventDisputeClosed          = "charge.dispute.closed"
)

type stripeDisputeCashEffect int

const (
	stripeDisputeCashNoEffect stripeDisputeCashEffect = iota
	stripeDisputeCashUnavailable
	stripeDisputeCashAvailable
)

type stripeCashEvent struct {
	EventID       string
	EventType     string
	ObjectID      string
	ChargeID      string
	PaymentIntent string
	Currency      string
	Status        string
	EventCreated  int64
	AmountCents   int64
	RefundedCents int64
	PayloadSHA256 string
	DisputeEffect stripeDisputeCashEffect
	EffectRank    int
}

type stripeCashEventResult struct {
	Duplicate              bool
	CashEffectApplied      bool
	CurrentCashEffectRank  int
	LinkedCollection       bool
	UnavailableCents       int64
	CompromisedFundingRows int
	ReversalRequiredRows   int64
}

func isStripeCashEventType(eventType string) bool {
	switch eventType {
	case stripeEventChargeRefunded,
		stripeEventDisputeCreated,
		stripeEventDisputeFundsWithdrawn,
		stripeEventDisputeFundsReinstated,
		stripeEventDisputeClosed:
		return true
	default:
		return false
	}
}

func disputeCashEffect(eventType, status string) (stripeDisputeCashEffect, int) {
	switch eventType {
	case stripeEventDisputeCreated:
		if strings.HasPrefix(status, "warning_") || status == "prevented" {
			return stripeDisputeCashNoEffect, 0
		}
		return stripeDisputeCashUnavailable, 10
	case stripeEventDisputeFundsWithdrawn:
		return stripeDisputeCashUnavailable, 20
	case stripeEventDisputeClosed:
		if status == "lost" {
			return stripeDisputeCashUnavailable, 30
		}
		return stripeDisputeCashNoEffect, 0
	case stripeEventDisputeFundsReinstated:
		return stripeDisputeCashAvailable, 40
	default:
		return stripeDisputeCashNoEffect, 0
	}
}

func stripeExpandableID(raw json.RawMessage, expectedObject string) (string, error) {
	if len(raw) == 0 || string(raw) == "null" {
		return "", nil
	}
	var id string
	if err := json.Unmarshal(raw, &id); err == nil {
		return strings.TrimSpace(id), nil
	}
	var expanded struct {
		Object string `json:"object"`
		ID     string `json:"id"`
	}
	if err := json.Unmarshal(raw, &expanded); err != nil {
		return "", errors.New("stripe expandable reference is neither an id nor an object")
	}
	if strings.TrimSpace(expanded.Object) != expectedObject {
		return "", fmt.Errorf("stripe expandable reference has object kind %q, want %q",
			strings.TrimSpace(expanded.Object), expectedObject)
	}
	return strings.TrimSpace(expanded.ID), nil
}

func parseStripeCashEvent(
	eventID, eventType string,
	eventCreated int64,
	object json.RawMessage,
	payload []byte,
) (stripeCashEvent, error) {
	hash := sha256.Sum256(payload)
	out := stripeCashEvent{
		EventID:       strings.TrimSpace(eventID),
		EventType:     strings.TrimSpace(eventType),
		EventCreated:  eventCreated,
		PayloadSHA256: hex.EncodeToString(hash[:]),
	}
	if out.EventID == "" || out.EventCreated <= 0 || !isStripeCashEventType(out.EventType) {
		return stripeCashEvent{}, errors.New("stripe cash event is missing a supported type, id, or creation time")
	}

	switch out.EventType {
	case stripeEventChargeRefunded:
		var charge struct {
			Object         string          `json:"object"`
			ID             string          `json:"id"`
			PaymentIntent  json.RawMessage `json:"payment_intent"`
			Amount         int64           `json:"amount"`
			AmountRefunded int64           `json:"amount_refunded"`
			Currency       string          `json:"currency"`
		}
		if err := json.Unmarshal(object, &charge); err != nil {
			return stripeCashEvent{}, fmt.Errorf("decode charge.refunded object: %w", err)
		}
		if strings.TrimSpace(charge.Object) != "charge" {
			return stripeCashEvent{}, errors.New("charge.refunded has the wrong Stripe object kind")
		}
		pi, err := stripeExpandableID(charge.PaymentIntent, "payment_intent")
		if err != nil {
			return stripeCashEvent{}, err
		}
		out.ObjectID = strings.TrimSpace(charge.ID)
		out.ChargeID = out.ObjectID
		out.PaymentIntent = pi
		out.AmountCents = charge.Amount
		out.RefundedCents = charge.AmountRefunded
		out.Currency = strings.ToLower(strings.TrimSpace(charge.Currency))
	case stripeEventDisputeCreated, stripeEventDisputeFundsWithdrawn,
		stripeEventDisputeFundsReinstated, stripeEventDisputeClosed:
		var dispute struct {
			Object        string          `json:"object"`
			ID            string          `json:"id"`
			Charge        json.RawMessage `json:"charge"`
			PaymentIntent json.RawMessage `json:"payment_intent"`
			Amount        int64           `json:"amount"`
			Currency      string          `json:"currency"`
			Status        string          `json:"status"`
		}
		if err := json.Unmarshal(object, &dispute); err != nil {
			return stripeCashEvent{}, fmt.Errorf("decode Stripe dispute object: %w", err)
		}
		if strings.TrimSpace(dispute.Object) != "dispute" {
			return stripeCashEvent{}, errors.New("Stripe dispute event has the wrong object kind")
		}
		chargeID, err := stripeExpandableID(dispute.Charge, "charge")
		if err != nil {
			return stripeCashEvent{}, err
		}
		pi, err := stripeExpandableID(dispute.PaymentIntent, "payment_intent")
		if err != nil {
			return stripeCashEvent{}, err
		}
		out.ObjectID = strings.TrimSpace(dispute.ID)
		out.ChargeID = chargeID
		out.PaymentIntent = pi
		out.AmountCents = dispute.Amount
		out.Currency = strings.ToLower(strings.TrimSpace(dispute.Currency))
		out.Status = strings.TrimSpace(dispute.Status)
		out.DisputeEffect, out.EffectRank = disputeCashEffect(out.EventType, out.Status)
	}

	if err := validateStripeCashEvent(out); err != nil {
		return stripeCashEvent{}, err
	}
	return out, nil
}

func validateStripeCashEvent(event stripeCashEvent) error {
	if strings.TrimSpace(event.EventID) == "" || strings.TrimSpace(event.ObjectID) == "" ||
		strings.TrimSpace(event.ChargeID) == "" || event.EventCreated <= 0 ||
		event.AmountCents <= 0 || strings.TrimSpace(event.Currency) == "" ||
		!isSHA256Hex(event.PayloadSHA256) || !isStripeCashEventType(event.EventType) {
		return errors.New("stripe cash event has invalid identifiers, amount, currency, timestamp, or digest")
	}
	if event.EventType == stripeEventChargeRefunded {
		if !validStripeObjectID(event.ObjectID, "ch_") || !validStripeObjectID(event.ChargeID, "ch_") ||
			(event.PaymentIntent != "" && !validStripeObjectID(event.PaymentIntent, "pi_")) {
			return errors.New("charge.refunded has invalid Stripe object identifiers")
		}
		if event.RefundedCents <= 0 || event.RefundedCents > event.AmountCents {
			return errors.New("charge.refunded has an invalid cumulative amount_refunded")
		}
		return nil
	}
	if !validStripeObjectID(event.ObjectID, "dp_") || !validStripeObjectID(event.ChargeID, "ch_") ||
		(event.PaymentIntent != "" && !validStripeObjectID(event.PaymentIntent, "pi_")) {
		return errors.New("Stripe dispute has invalid object identifiers")
	}
	if event.Status == "" {
		return errors.New("stripe dispute event is missing status")
	}
	expectedEffect, expectedRank := disputeCashEffect(event.EventType, event.Status)
	if event.DisputeEffect != expectedEffect || event.EffectRank != expectedRank {
		return errors.New("stripe dispute event cash effect does not match its type and status")
	}
	return nil
}

func nullableStripeID(value string) any {
	if strings.TrimSpace(value) == "" {
		return nil
	}
	return strings.TrimSpace(value)
}

// lockStripeCashBinding serializes every local writer that can first discover
// or bind a charge's cash evidence. The durable rows are split across several
// tables, so row locks alone cannot protect the gap where a direct charge
// confirmation and a webhook both observe the charge before either commits.
func lockStripeCashBinding(ctx context.Context, tx pgx.Tx, chargeID string) error {
	chargeID = strings.TrimSpace(chargeID)
	if chargeID == "" {
		return errors.New("Stripe cash binding requires a charge id")
	}
	_, err := tx.Exec(ctx,
		`SELECT pg_advisory_xact_lock(hashtextextended($1, 0))`,
		"merc:stripe-cash:"+chargeID,
	)
	return err
}

// bindStripeCashStateToCollection closes the provider-before-canonical-row
// ordering gap. Stripe can deliver a refund or dispute without a
// PaymentIntent reference before the synchronous charge reconciliation creates
// buyer_cash_collections. Once that collection exists, every cash-state row
// for its charge must be bound before any payout-funding code can inspect the
// collection. The identity checks deliberately match recordBuyerCashCollection
// so the top-up path cannot become a weaker cash authority.
func bindStripeCashStateToCollection(
	ctx context.Context,
	tx pgx.Tx,
	chargeID, paymentIntent string,
	received int64,
	currency string,
) error {
	chargeID = strings.TrimSpace(chargeID)
	paymentIntent = strings.TrimSpace(paymentIntent)
	currency = strings.ToLower(strings.TrimSpace(currency))
	if chargeID == "" || paymentIntent == "" || received <= 0 || currency == "" {
		return errors.New("Stripe cash collection binding is incomplete")
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
		  UNION ALL
		  SELECT 1 FROM stripe_risk_events
		   WHERE charge_id=$1 AND payment_intent IS NOT NULL AND payment_intent<>$2
		)`, chargeID, paymentIntent, received, currency).Scan(&conflictingState); err != nil {
		return err
	}
	if conflictingState {
		return fmt.Errorf("charge %s webhook state is bound to a different PaymentIntent", chargeID)
	}
	if _, err := tx.Exec(ctx, `
		UPDATE stripe_charge_cash_state SET payment_intent=$2,updated_at=now()
		 WHERE charge_id=$1 AND payment_intent IS NULL`, chargeID, paymentIntent); err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, `
		UPDATE stripe_dispute_cash_state SET payment_intent=$2,updated_at=now()
		 WHERE charge_id=$1 AND payment_intent IS NULL`, chargeID, paymentIntent); err != nil {
		return err
	}
	return nil
}

func (s *Store) ApplyPaymentEventTx(ctx context.Context, event stripeCashEvent) (stripeCashEventResult, error) {
	var result stripeCashEventResult
	if err := validateStripeCashEvent(event); err != nil {
		return result, err
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return result, err
	}
	defer tx.Rollback(ctx)
	var storedType, storedObject, storedCharge, storedPI, storedHash string
	var storedCreated int64
	var inserted bool
	err = tx.QueryRow(ctx, `
		WITH cash_lock AS MATERIALIZED (
			SELECT pg_advisory_xact_lock(hashtextextended($1, 0)) AS locked
		), inserted AS MATERIALIZED (
			INSERT INTO stripe_webhook_events
			  (event_id,event_type,object_id,charge_id,payment_intent,event_created,payload_sha256)
			SELECT $2,$3,$4,$5,NULLIF($6,''),$7,$8
			  FROM cash_lock
			ON CONFLICT (event_id) DO NOTHING
			RETURNING event_type,object_id,charge_id,COALESCE(payment_intent,'') AS payment_intent,
			          event_created,payload_sha256,true AS inserted
		), durable AS (
			SELECT event_type,object_id,charge_id,payment_intent,event_created,payload_sha256,inserted
			  FROM inserted
			 UNION ALL
			SELECT event_type,object_id,charge_id,COALESCE(payment_intent,''),event_created,payload_sha256,false
			  FROM stripe_webhook_events
			 WHERE event_id=$2
		)
		SELECT event_type,object_id,charge_id,payment_intent,event_created,payload_sha256,inserted
		  FROM durable
		 LIMIT 1`,
		"merc:stripe-cash:"+event.ChargeID, event.EventID, event.EventType, event.ObjectID,
		event.ChargeID, event.PaymentIntent, event.EventCreated, event.PayloadSHA256,
	).Scan(&storedType, &storedObject, &storedCharge, &storedPI, &storedCreated, &storedHash, &inserted)
	if errors.Is(err, pgx.ErrNoRows) {
		// A concurrent insert can win the ON CONFLICT check after this
		// statement's snapshot was taken. Re-read only that rare replay path;
		// the normal lock/insert/replay decision is one database round trip.
		err = tx.QueryRow(ctx, `
			SELECT event_type,object_id,charge_id,COALESCE(payment_intent,''),event_created,payload_sha256
			  FROM stripe_webhook_events WHERE event_id=$1`, event.EventID,
		).Scan(&storedType, &storedObject, &storedCharge, &storedPI, &storedCreated, &storedHash)
		if err != nil {
			return result, err
		}
		inserted = false
	}
	if err != nil {
		return result, err
	}
	if !inserted {
		if storedType != event.EventType || storedObject != event.ObjectID ||
			storedCharge != event.ChargeID || storedPI != event.PaymentIntent ||
			storedCreated != event.EventCreated || storedHash != event.PayloadSHA256 {
			return result, fmt.Errorf("stripe event id %s conflicts with its durable event binding", event.EventID)
		}
		result.Duplicate = true
		if err := tx.Commit(ctx); err != nil {
			return stripeCashEventResult{}, err
		}
		return result, nil
	}

	var resolvedPI string
	var collectionReceived int64
	var collectionCurrency, collectionChargeID string
	var linkedCollection bool
	err = tx.QueryRow(ctx, `
		WITH resolution AS MATERIALIZED (
			SELECT COALESCE(
				NULLIF($1,''),
				(SELECT payment_intent FROM buyer_cash_collections WHERE charge_id=$2),
				(SELECT payment_intent FROM stripe_charge_cash_state WHERE charge_id=$2),
				CASE WHEN $3<>$4 THEN
					(SELECT payment_intent FROM stripe_dispute_cash_state WHERE dispute_id=$5)
				END,
				'') AS payment_intent
		), collection AS MATERIALIZED (
			SELECT r.payment_intent,
			       COALESCE(c.received_cents,0) AS received_cents,
			       COALESCE(c.currency,'') AS currency,
			       COALESCE(c.charge_id,'') AS charge_id,
			       (c.payment_intent IS NOT NULL) AS linked
			  FROM resolution r
			  LEFT JOIN LATERAL (
				SELECT payment_intent,received_cents,currency,charge_id
				  FROM buyer_cash_collections
				 WHERE payment_intent=NULLIF(r.payment_intent,'')
				 FOR UPDATE
			  ) c ON TRUE
		)
		SELECT payment_intent,received_cents,currency,charge_id,linked
		  FROM collection`,
		event.PaymentIntent, event.ChargeID, event.EventType, stripeEventChargeRefunded, event.ObjectID,
	).Scan(&resolvedPI, &collectionReceived, &collectionCurrency, &collectionChargeID, &linkedCollection)
	if err != nil {
		return result, err
	}
	result.LinkedCollection = linkedCollection
	if result.LinkedCollection {
		if collectionCurrency != event.Currency {
			return result, fmt.Errorf("stripe event %s currency %s conflicts with collection %s currency %s",
				event.EventID, event.Currency, resolvedPI, collectionCurrency)
		}
		if event.EventType == stripeEventChargeRefunded && event.AmountCents != collectionReceived {
			return result, fmt.Errorf("stripe charge %s amount %d conflicts with collection %s amount %d",
				event.ChargeID, event.AmountCents, resolvedPI, collectionReceived)
		}
		if collectionChargeID != "" && collectionChargeID != event.ChargeID {
			return result, fmt.Errorf("stripe event %s charge %s conflicts with collection %s charge %s",
				event.EventID, event.ChargeID, resolvedPI, collectionChargeID)
		}
	}

	if event.EventType == stripeEventChargeRefunded {
		resolvedPI, err = applyStripeChargeRefundState(ctx, tx, event, resolvedPI)
	} else {
		var currentEffectCreated int64
		resolvedPI, currentEffectCreated, result.CurrentCashEffectRank, err =
			applyStripeDisputeState(ctx, tx, event, resolvedPI)
		result.CashEffectApplied = event.EffectRank > 0 &&
			currentEffectCreated == event.EventCreated &&
			result.CurrentCashEffectRank == event.EffectRank
	}
	if err != nil {
		return result, err
	}

	if event.EventType == stripeEventChargeRefunded && resolvedPI != "" {
		if _, err := tx.Exec(ctx, `
			UPDATE stripe_dispute_cash_state SET payment_intent=$2,updated_at=now()
			 WHERE charge_id=$1 AND payment_intent IS NULL`, event.ChargeID, resolvedPI); err != nil {
			return result, err
		}
	}

	if result.LinkedCollection {
		result.UnavailableCents, result.CompromisedFundingRows,
			result.ReversalRequiredRows, err = recomputeStripeCollectionFunding(
			ctx, tx, resolvedPI, collectionReceived, event,
		)
		if err != nil {
			return stripeCashEventResult{}, err
		}
	}
	if err := tx.Commit(ctx); err != nil {
		return stripeCashEventResult{}, err
	}
	return result, nil
}

func applyStripeChargeRefundState(
	ctx context.Context,
	tx pgx.Tx,
	event stripeCashEvent,
	resolvedPI string,
) (string, error) {
	var boundPI string
	err := tx.QueryRow(ctx, `
		INSERT INTO stripe_charge_cash_state
		  (charge_id,payment_intent,amount_cents,refunded_cents,currency,last_event_id,last_event_created)
		VALUES ($1,$2,$3,$4,$5,$6,$7)
		ON CONFLICT (charge_id) DO UPDATE SET
		  payment_intent=COALESCE(stripe_charge_cash_state.payment_intent,EXCLUDED.payment_intent),
		  refunded_cents=GREATEST(stripe_charge_cash_state.refunded_cents,EXCLUDED.refunded_cents),
		  last_event_id=CASE
		    WHEN EXCLUDED.last_event_created >= stripe_charge_cash_state.last_event_created
		    THEN EXCLUDED.last_event_id ELSE stripe_charge_cash_state.last_event_id END,
		  last_event_created=GREATEST(stripe_charge_cash_state.last_event_created,EXCLUDED.last_event_created),
		  updated_at=now()
		WHERE stripe_charge_cash_state.amount_cents=EXCLUDED.amount_cents
		  AND stripe_charge_cash_state.currency=EXCLUDED.currency
		  AND (stripe_charge_cash_state.payment_intent IS NULL OR EXCLUDED.payment_intent IS NULL
		       OR stripe_charge_cash_state.payment_intent=EXCLUDED.payment_intent)
		RETURNING COALESCE(payment_intent,'')`,
		event.ChargeID, nullableStripeID(resolvedPI), event.AmountCents, event.RefundedCents,
		event.Currency, event.EventID, event.EventCreated,
	).Scan(&boundPI)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", fmt.Errorf("stripe charge %s conflicts with its durable object binding", event.ChargeID)
	}
	return boundPI, err
}

func applyStripeDisputeState(
	ctx context.Context,
	tx pgx.Tx,
	event stripeCashEvent,
	resolvedPI string,
) (string, int64, int, error) {
	effectCreated, effectRank, unavailable := int64(0), 0, false
	if event.DisputeEffect != stripeDisputeCashNoEffect {
		effectCreated, effectRank = event.EventCreated, event.EffectRank
		unavailable = event.DisputeEffect == stripeDisputeCashUnavailable
	}
	var boundPI string
	var currentEffectCreated int64
	var currentEffectRank int
	err := tx.QueryRow(ctx, `
		INSERT INTO stripe_dispute_cash_state
		  (dispute_id,charge_id,payment_intent,amount_cents,currency,status,cash_unavailable,
		   cash_effect_created,cash_effect_rank,last_event_id,last_event_type,last_event_created)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12)
		ON CONFLICT (dispute_id) DO UPDATE SET
		  payment_intent=COALESCE(stripe_dispute_cash_state.payment_intent,EXCLUDED.payment_intent),
		  status=CASE
		    WHEN EXCLUDED.last_event_created >= stripe_dispute_cash_state.last_event_created
		    THEN EXCLUDED.status ELSE stripe_dispute_cash_state.status END,
		  cash_unavailable=CASE
		    WHEN (EXCLUDED.cash_effect_created,EXCLUDED.cash_effect_rank) >=
		         (stripe_dispute_cash_state.cash_effect_created,stripe_dispute_cash_state.cash_effect_rank)
		         AND EXCLUDED.cash_effect_rank > 0
		    THEN EXCLUDED.cash_unavailable ELSE stripe_dispute_cash_state.cash_unavailable END,
		  cash_effect_created=CASE
		    WHEN (EXCLUDED.cash_effect_created,EXCLUDED.cash_effect_rank) >=
		         (stripe_dispute_cash_state.cash_effect_created,stripe_dispute_cash_state.cash_effect_rank)
		         AND EXCLUDED.cash_effect_rank > 0
		    THEN EXCLUDED.cash_effect_created ELSE stripe_dispute_cash_state.cash_effect_created END,
		  cash_effect_rank=CASE
		    WHEN (EXCLUDED.cash_effect_created,EXCLUDED.cash_effect_rank) >=
		         (stripe_dispute_cash_state.cash_effect_created,stripe_dispute_cash_state.cash_effect_rank)
		         AND EXCLUDED.cash_effect_rank > 0
		    THEN EXCLUDED.cash_effect_rank ELSE stripe_dispute_cash_state.cash_effect_rank END,
		  last_event_id=CASE
		    WHEN EXCLUDED.last_event_created >= stripe_dispute_cash_state.last_event_created
		    THEN EXCLUDED.last_event_id ELSE stripe_dispute_cash_state.last_event_id END,
		  last_event_type=CASE
		    WHEN EXCLUDED.last_event_created >= stripe_dispute_cash_state.last_event_created
		    THEN EXCLUDED.last_event_type ELSE stripe_dispute_cash_state.last_event_type END,
		  last_event_created=GREATEST(stripe_dispute_cash_state.last_event_created,EXCLUDED.last_event_created),
		  updated_at=now()
		WHERE stripe_dispute_cash_state.charge_id=EXCLUDED.charge_id
		  AND stripe_dispute_cash_state.amount_cents=EXCLUDED.amount_cents
		  AND stripe_dispute_cash_state.currency=EXCLUDED.currency
		  AND (stripe_dispute_cash_state.payment_intent IS NULL OR EXCLUDED.payment_intent IS NULL
		       OR stripe_dispute_cash_state.payment_intent=EXCLUDED.payment_intent)
		RETURNING COALESCE(payment_intent,''),cash_effect_created,cash_effect_rank`,
		event.ObjectID, event.ChargeID, nullableStripeID(resolvedPI), event.AmountCents,
		event.Currency, event.Status, unavailable, effectCreated, effectRank,
		event.EventID, event.EventType, event.EventCreated,
	).Scan(&boundPI, &currentEffectCreated, &currentEffectRank)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", 0, 0,
			fmt.Errorf("stripe dispute %s conflicts with its durable object binding", event.ObjectID)
	}
	return boundPI, currentEffectCreated, currentEffectRank, err
}

type stripeCollectionCapacity struct {
	Unavailable int64
	Reserved    int64
}

func stripeCollectionCapacityForPaymentIntent(
	ctx context.Context,
	tx pgx.Tx,
	paymentIntent string,
	received int64,
) (stripeCollectionCapacity, error) {
	var capacity stripeCollectionCapacity
	err := tx.QueryRow(ctx, `
		SELECT LEAST($2::bigint,
		  COALESCE((SELECT sum(refunded_cents) FROM stripe_charge_cash_state
		             WHERE payment_intent=$1),0)::bigint
		  + COALESCE((SELECT sum(amount_cents) FROM stripe_dispute_cash_state
		               WHERE payment_intent=$1 AND cash_unavailable),0)::bigint),
		COALESCE((SELECT sum(amount_cents) FROM supplier_payout_funding
		           WHERE source_kind='buyer_collection' AND collection_payment_intent=$1),0)::bigint`,
		paymentIntent, received,
	).Scan(&capacity.Unavailable, &capacity.Reserved)
	return capacity, err
}

func recomputeStripeCollectionFunding(
	ctx context.Context,
	tx pgx.Tx,
	paymentIntent string,
	received int64,
	event stripeCashEvent,
) (unavailable int64, compromisedRows int, reversalRows int64, err error) {
	var compromisedCount, reversalCount, ledgerCount, stateCount int64
	err = tx.QueryRow(ctx, `
		WITH capacity AS MATERIALIZED (
			SELECT LEAST($2::bigint,
			  COALESCE((SELECT sum(refunded_cents) FROM stripe_charge_cash_state
			             WHERE payment_intent=$1),0)::bigint
			  + COALESCE((SELECT sum(amount_cents) FROM stripe_dispute_cash_state
			               WHERE payment_intent=$1 AND cash_unavailable),0)::bigint
			) AS unavailable
		), raw_exposures AS MATERIALIZED (
			SELECT f.id AS funding_id,f.ledger_entry_id,
			       c.unavailable,
			       GREATEST(0::bigint,LEAST(f.amount_cents,
			         sum(f.amount_cents) OVER (ORDER BY f.created_at,f.id ROWS UNBOUNDED PRECEDING)
			         - ($2::bigint-c.unavailable)))::bigint AS compromised_cents
			  FROM supplier_payout_funding f
			 CROSS JOIN capacity c
			 WHERE f.source_kind='buyer_collection' AND f.collection_payment_intent=$1
		), exposures AS MATERIALIZED (
			SELECT funding_id,ledger_entry_id,unavailable,compromised_cents,
			       CASE WHEN compromised_cents > 0
			            THEN format('collection %s has %s unavailable cents after %s; this reservation is impaired by %s cents',
			                        $1,unavailable,$4::text,compromised_cents)
			            ELSE format('collection %s remains available after Stripe event %s',$1,$3::text)
			       END AS reason
			  FROM raw_exposures
		), states AS (
			INSERT INTO supplier_payout_funding_state
			  (funding_id,status,compromised_cents,last_event_id,reason)
			SELECT funding_id,
			       CASE WHEN compromised_cents > 0 THEN 'compromised' ELSE 'available' END,
			       compromised_cents,$3,reason
			  FROM exposures
			ON CONFLICT (funding_id) DO UPDATE SET
			  status=EXCLUDED.status,compromised_cents=EXCLUDED.compromised_cents,
			  last_event_id=EXCLUDED.last_event_id,reason=EXCLUDED.reason,updated_at=now()
			RETURNING funding_id
		), reversals AS (
			UPDATE supplier_payout_operations op
			   SET status='reversal_required',last_error=e.reason,updated_at=now()
			  FROM exposures e
			 WHERE e.compromised_cents > 0 AND op.funding_id=e.funding_id
			   AND op.status<>'reversed'
			   AND (op.cash_moved OR op.outcome_unknown OR op.status='sending')
			RETURNING op.ledger_entry_id
		), reversal_targets AS (
			-- The old loop marked the ledger entry for every reversal_required
			-- operation belonging to an impaired funding row, including an
			-- operation that was already reversal_required before this event.
			-- A data-modifying sibling CTE cannot see reversals' writes through
			-- the statement snapshot, so union its RETURNING rows with the
			-- pre-existing targets explicitly.
			SELECT op.ledger_entry_id
			  FROM exposures e
			  JOIN supplier_payout_operations op ON op.funding_id=e.funding_id
			 WHERE e.compromised_cents > 0 AND op.status='reversal_required'
			UNION
			SELECT ledger_entry_id FROM reversals
		), ledger AS (
			UPDATE ledger_entries le SET payout_status='reversal_required'
			  FROM reversal_targets r
			 WHERE r.ledger_entry_id=le.id
			RETURNING le.id
		)
		SELECT (SELECT unavailable FROM capacity),
		       COALESCE((SELECT sum((compromised_cents > 0)::int) FROM exposures),0)::bigint,
		       COALESCE((SELECT count(*) FROM reversals),0)::bigint,
		       COALESCE((SELECT count(*) FROM ledger),0)::bigint,
		       COALESCE((SELECT count(*) FROM states),0)::bigint`,
		paymentIntent, received, event.EventID, event.EventType,
	).Scan(&unavailable, &compromisedCount, &reversalCount, &ledgerCount, &stateCount)
	if err != nil {
		return 0, 0, 0, err
	}
	// The final SELECT intentionally observes every data-modifying CTE. The
	// ledger/state counts are not part of the public result, but binding them
	// here makes the one-statement write set explicit and auditable.
	_ = ledgerCount
	_ = stateCount
	return unavailable, int(compromisedCount), reversalCount, nil
}
