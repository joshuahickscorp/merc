package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/google/uuid"
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

func stripeExpandableID(raw json.RawMessage) (string, error) {
	if len(raw) == 0 || string(raw) == "null" {
		return "", nil
	}
	var id string
	if err := json.Unmarshal(raw, &id); err == nil {
		return strings.TrimSpace(id), nil
	}
	var expanded struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal(raw, &expanded); err != nil {
		return "", errors.New("stripe expandable reference is neither an id nor an object")
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
			ID             string          `json:"id"`
			PaymentIntent  json.RawMessage `json:"payment_intent"`
			Amount         int64           `json:"amount"`
			AmountRefunded int64           `json:"amount_refunded"`
			Currency       string          `json:"currency"`
		}
		if err := json.Unmarshal(object, &charge); err != nil {
			return stripeCashEvent{}, fmt.Errorf("decode charge.refunded object: %w", err)
		}
		pi, err := stripeExpandableID(charge.PaymentIntent)
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
		chargeID, err := stripeExpandableID(dispute.Charge)
		if err != nil {
			return stripeCashEvent{}, err
		}
		pi, err := stripeExpandableID(dispute.PaymentIntent)
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
		len(event.PayloadSHA256) != sha256.Size*2 || !isStripeCashEventType(event.EventType) {
		return errors.New("stripe cash event has invalid identifiers, amount, currency, timestamp, or digest")
	}
	if event.EventType == stripeEventChargeRefunded {
		if event.RefundedCents <= 0 || event.RefundedCents > event.AmountCents {
			return errors.New("charge.refunded has an invalid cumulative amount_refunded")
		}
		return nil
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

	tag, err := tx.Exec(ctx, `
		INSERT INTO stripe_webhook_events
		  (event_id,event_type,object_id,charge_id,payment_intent,event_created,payload_sha256)
		VALUES ($1,$2,$3,$4,$5,$6,$7)
		ON CONFLICT (event_id) DO NOTHING`,
		event.EventID, event.EventType, event.ObjectID, event.ChargeID,
		nullableStripeID(event.PaymentIntent), event.EventCreated, event.PayloadSHA256,
	)
	if err != nil {
		return result, err
	}
	if tag.RowsAffected() == 0 {
		var storedType, storedObject, storedCharge, storedPI, storedHash string
		var storedCreated int64
		if err := tx.QueryRow(ctx, `
			SELECT event_type,object_id,charge_id,COALESCE(payment_intent,''),event_created,payload_sha256
			  FROM stripe_webhook_events WHERE event_id=$1`, event.EventID,
		).Scan(&storedType, &storedObject, &storedCharge, &storedPI, &storedCreated, &storedHash); err != nil {
			return result, err
		}
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

	resolvedPI := event.PaymentIntent
	if resolvedPI == "" {
		_ = tx.QueryRow(ctx, `
			SELECT payment_intent FROM buyer_cash_collections WHERE charge_id=$1`,
			event.ChargeID).Scan(&resolvedPI)
	}
	if resolvedPI == "" {
		_ = tx.QueryRow(ctx, `
			SELECT COALESCE(payment_intent,'') FROM stripe_charge_cash_state WHERE charge_id=$1`,
			event.ChargeID).Scan(&resolvedPI)
	}
	if resolvedPI == "" && event.EventType != stripeEventChargeRefunded {
		_ = tx.QueryRow(ctx, `
			SELECT COALESCE(payment_intent,'') FROM stripe_dispute_cash_state WHERE dispute_id=$1`,
			event.ObjectID).Scan(&resolvedPI)
	}

	var collectionReceived int64
	var collectionCurrency, collectionChargeID string
	if resolvedPI != "" {
		err = tx.QueryRow(ctx, `
			SELECT received_cents,currency,COALESCE(charge_id,'') FROM buyer_cash_collections
			 WHERE payment_intent=$1 FOR UPDATE`, resolvedPI,
		).Scan(&collectionReceived, &collectionCurrency, &collectionChargeID)
		if err == nil {
			result.LinkedCollection = true
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
		} else if !errors.Is(err, pgx.ErrNoRows) {
			return result, err
		}
	}

	if event.EventType == stripeEventChargeRefunded {
		resolvedPI, err = applyStripeChargeRefundState(ctx, tx, event, resolvedPI)
	} else {
		resolvedPI, err = applyStripeDisputeState(ctx, tx, event, resolvedPI)
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
) (string, error) {
	effectCreated, effectRank, unavailable := int64(0), 0, false
	if event.DisputeEffect != stripeDisputeCashNoEffect {
		effectCreated, effectRank = event.EventCreated, event.EffectRank
		unavailable = event.DisputeEffect == stripeDisputeCashUnavailable
	}
	var boundPI string
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
		RETURNING COALESCE(payment_intent,'')`,
		event.ObjectID, event.ChargeID, nullableStripeID(resolvedPI), event.AmountCents,
		event.Currency, event.Status, unavailable, effectCreated, effectRank,
		event.EventID, event.EventType, event.EventCreated,
	).Scan(&boundPI)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", fmt.Errorf("stripe dispute %s conflicts with its durable object binding", event.ObjectID)
	}
	return boundPI, err
}

func stripeCollectionUnavailableCents(ctx context.Context, tx pgx.Tx, paymentIntent string, received int64) (int64, error) {
	var unavailable int64
	err := tx.QueryRow(ctx, `
		SELECT LEAST($2::bigint,
		  COALESCE((SELECT sum(refunded_cents) FROM stripe_charge_cash_state
		             WHERE payment_intent=$1),0)::bigint
		  + COALESCE((SELECT sum(amount_cents) FROM stripe_dispute_cash_state
		               WHERE payment_intent=$1 AND cash_unavailable),0)::bigint)`,
		paymentIntent, received,
	).Scan(&unavailable)
	return unavailable, err
}

func recomputeStripeCollectionFunding(
	ctx context.Context,
	tx pgx.Tx,
	paymentIntent string,
	received int64,
	event stripeCashEvent,
) (unavailable int64, compromisedRows int, reversalRows int64, err error) {
	unavailable, err = stripeCollectionUnavailableCents(ctx, tx, paymentIntent, received)
	if err != nil {
		return 0, 0, 0, err
	}
	available := received - unavailable
	rows, err := tx.Query(ctx, `
		SELECT id,ledger_entry_id,amount_cents,
		       GREATEST(0::bigint,LEAST(amount_cents,
		         sum(amount_cents) OVER (ORDER BY created_at,id ROWS UNBOUNDED PRECEDING)-$2::bigint))::bigint
		  FROM supplier_payout_funding
		 WHERE source_kind='buyer_collection' AND collection_payment_intent=$1
		 ORDER BY created_at,id`, paymentIntent, available)
	if err != nil {
		return 0, 0, 0, err
	}
	type fundingExposure struct {
		fundingID, entryID  uuid.UUID
		amount, compromised int64
	}
	var exposures []fundingExposure
	for rows.Next() {
		var exposure fundingExposure
		if err := rows.Scan(&exposure.fundingID, &exposure.entryID, &exposure.amount, &exposure.compromised); err != nil {
			rows.Close()
			return 0, 0, 0, err
		}
		exposures = append(exposures, exposure)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return 0, 0, 0, err
	}

	for _, exposure := range exposures {
		state := "available"
		reason := fmt.Sprintf("collection %s remains available after Stripe event %s", paymentIntent, event.EventID)
		if exposure.compromised > 0 {
			state = "compromised"
			reason = fmt.Sprintf(
				"collection %s has %d unavailable cents after %s; this reservation is impaired by %d cents",
				paymentIntent, unavailable, event.EventType, exposure.compromised)
			compromisedRows++
		}
		if _, err := tx.Exec(ctx, `
			INSERT INTO supplier_payout_funding_state
			  (funding_id,status,compromised_cents,last_event_id,reason)
			VALUES ($1,$2,$3,$4,$5)
			ON CONFLICT (funding_id) DO UPDATE SET
			  status=EXCLUDED.status,compromised_cents=EXCLUDED.compromised_cents,
			  last_event_id=EXCLUDED.last_event_id,reason=EXCLUDED.reason,updated_at=now()`,
			exposure.fundingID, state, exposure.compromised, event.EventID, reason); err != nil {
			return 0, 0, 0, err
		}
		if exposure.compromised == 0 {
			continue
		}
		tag, err := tx.Exec(ctx, `
			UPDATE supplier_payout_operations
			   SET status='reversal_required',last_error=$2,updated_at=now()
			 WHERE funding_id=$1 AND status<>'reversed'
			   AND (cash_moved OR outcome_unknown OR status='sending')`,
			exposure.fundingID, reason)
		if err != nil {
			return 0, 0, 0, err
		}
		reversalRows += tag.RowsAffected()
		if _, err := tx.Exec(ctx, `
			UPDATE ledger_entries le SET payout_status='reversal_required'
			  FROM supplier_payout_operations op
			 WHERE op.funding_id=$1 AND op.ledger_entry_id=le.id AND op.status='reversal_required'`,
			exposure.fundingID); err != nil {
			return 0, 0, 0, err
		}
	}
	return unavailable, compromisedRows, reversalRows, nil
}
