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
	stripeRiskEventEarlyFraudWarningCreated = "radar.early_fraud_warning.created"
	stripeRiskEventEarlyFraudWarningUpdated = "radar.early_fraud_warning.updated"
	stripeRiskObjectEarlyFraudWarning       = "radar.early_fraud_warning"
)

type stripeRiskEvent struct {
	EventID       string
	EventType     string
	WarningID     string
	ChargeID      string
	PaymentIntent string
	FraudType     string
	Actionable    bool
	EventCreated  int64
	PayloadSHA256 string
}

type stripeRiskEventResult struct {
	Duplicate bool
}

func isStripeRiskEventType(eventType string) bool {
	return eventType == stripeRiskEventEarlyFraudWarningCreated ||
		eventType == stripeRiskEventEarlyFraudWarningUpdated
}

func parseStripeRiskEvent(
	eventID, eventType string, eventCreated int64,
	object map[string]any, payload []byte,
) (stripeRiskEvent, error) {
	hash := sha256.Sum256(payload)
	event := stripeRiskEvent{
		EventID:       strings.TrimSpace(eventID),
		EventType:     strings.TrimSpace(eventType),
		EventCreated:  eventCreated,
		PayloadSHA256: hex.EncodeToString(hash[:]),
	}
	if event.EventID == "" || event.EventCreated <= 0 || !isStripeRiskEventType(event.EventType) || object == nil {
		return stripeRiskEvent{}, errors.New("invalid Stripe risk event")
	}
	objectType, _ := object["object"].(string)
	if strings.TrimSpace(objectType) != stripeRiskObjectEarlyFraudWarning {
		return stripeRiskEvent{}, errors.New("early-fraud warning has the wrong Stripe object kind")
	}
	event.WarningID, _ = object["id"].(string)
	event.WarningID = strings.TrimSpace(event.WarningID)
	var err error
	event.ChargeID, err = stripeExpandableMapID(object, "charge", "charge")
	if err != nil {
		return stripeRiskEvent{}, fmt.Errorf("decode early-fraud charge: %w", err)
	}
	event.PaymentIntent, err = stripeExpandableMapID(object, "payment_intent", "payment_intent")
	if err != nil {
		return stripeRiskEvent{}, fmt.Errorf("decode early-fraud PaymentIntent: %w", err)
	}
	event.FraudType, _ = object["fraud_type"].(string)
	event.FraudType = strings.TrimSpace(event.FraudType)
	event.Actionable, _ = object["actionable"].(bool)
	if event.WarningID == "" || event.ChargeID == "" || event.FraudType == "" {
		return stripeRiskEvent{}, errors.New("early-fraud warning is missing its id, charge, or fraud type")
	}
	if _, ok := object["actionable"].(bool); !ok {
		return stripeRiskEvent{}, errors.New("early-fraud warning is missing its actionable fact")
	}
	if err := validateStripeRiskEvent(event); err != nil {
		return stripeRiskEvent{}, err
	}
	return event, nil
}

func stripeExpandableMapID(object map[string]any, field, expectedObject string) (string, error) {
	raw, ok := object[field]
	if !ok || raw == nil {
		return "", nil
	}
	encoded, err := json.Marshal(raw)
	if err != nil {
		return "", err
	}
	return stripeExpandableID(encoded, expectedObject)
}

func validateStripeRiskEvent(event stripeRiskEvent) error {
	if strings.TrimSpace(event.EventID) == "" || !isStripeRiskEventType(event.EventType) ||
		!validStripeObjectID(event.WarningID, "issfr_") || !validStripeObjectID(event.ChargeID, "ch_") ||
		(event.PaymentIntent != "" && !validStripeObjectID(event.PaymentIntent, "pi_")) ||
		strings.TrimSpace(event.FraudType) == "" || event.EventCreated <= 0 ||
		!isSHA256Hex(event.PayloadSHA256) {
		return errors.New("invalid Stripe risk event")
	}
	return nil
}

func (s *Store) ApplyStripeRiskEvent(ctx context.Context, event stripeRiskEvent) (stripeRiskEventResult, error) {
	var result stripeRiskEventResult
	if err := validateStripeRiskEvent(event); err != nil {
		return result, err
	}
	// The cash and warning locks, both identity checks, the append-only insert,
	// and the duplicate read are one atomic server statement. PostgreSQL's
	// implicit transaction holds the xact locks through that statement, so the
	// uncontended path needs no explicit BEGIN or COMMIT round trip. CTE
	// dependencies retain the old lock order (cash, then warning) and make the
	// checks happen before the insert without weakening any fail-closed binding
	// rule.
	var (
		chargeConflict, paymentIntentConflict, cashConflict                        bool
		storedType, storedWarning, storedCharge, storedPI, storedFraud, storedHash string
		storedActionable                                                           bool
		storedCreated                                                              int64
		inserted                                                                   bool
	)
	if err := s.pool.QueryRow(ctx, `
		WITH cash_lock AS MATERIALIZED (
			SELECT pg_advisory_xact_lock(
				hashtextextended('merc:stripe-cash:' || $4, 0)) AS locked
		), warning_lock AS MATERIALIZED (
			SELECT pg_advisory_xact_lock(
				hashtextextended('merc:stripe-risk-warning:' || $3, 0)) AS locked
			  FROM cash_lock
		), warning_binding AS MATERIALIZED (
			SELECT EXISTS(
				SELECT 1 FROM stripe_risk_events
				 WHERE warning_id=$3 AND charge_id<>$4
			) AS charge_conflict,
			       $5<>'' AND EXISTS(
				SELECT 1 FROM stripe_risk_events
				 WHERE warning_id=$3
				   AND payment_intent IS NOT NULL
				   AND payment_intent<>$5
			) AS payment_intent_conflict
			  FROM warning_lock
		), cash_binding AS MATERIALIZED (
			SELECT CASE WHEN $5='' THEN false ELSE EXISTS(
				SELECT 1 FROM buyer_cash_collections
				 WHERE charge_id=$4 AND payment_intent<>$5
				UNION ALL
				SELECT 1 FROM stripe_charge_cash_state
				 WHERE charge_id=$4
				   AND payment_intent IS NOT NULL
				   AND payment_intent<>$5
				UNION ALL
				SELECT 1 FROM stripe_dispute_cash_state
				 WHERE charge_id=$4
				   AND payment_intent IS NOT NULL
				   AND payment_intent<>$5
			) END AS cash_conflict
			  FROM warning_lock
		), inserted AS (
			INSERT INTO stripe_risk_events
			  (event_id,event_type,warning_id,charge_id,payment_intent,fraud_type,actionable,event_created,payload_sha256)
			SELECT $1,$2,$3,$4,NULLIF($5,''),$6,$7,$8,$9
			  FROM warning_binding w
			 CROSS JOIN cash_binding c
			 WHERE NOT w.charge_conflict
			   AND NOT w.payment_intent_conflict
			   AND NOT c.cash_conflict
			ON CONFLICT (event_id) DO NOTHING
			RETURNING event_type,warning_id,charge_id,COALESCE(payment_intent,'') AS payment_intent,
			          fraud_type,actionable,event_created,payload_sha256,true AS inserted
		), durable AS (
			SELECT event_type,warning_id,charge_id,payment_intent,fraud_type,actionable,
			       event_created,payload_sha256,inserted
			  FROM inserted
			UNION ALL
			SELECT event_type,warning_id,charge_id,COALESCE(payment_intent,''),fraud_type,actionable,
			       event_created,payload_sha256,false
			  FROM stripe_risk_events
			 WHERE event_id=$1
		)
		SELECT w.charge_conflict,w.payment_intent_conflict,c.cash_conflict,
		       COALESCE(d.event_type,''),COALESCE(d.warning_id,''),COALESCE(d.charge_id,''),
		       COALESCE(d.payment_intent,''),COALESCE(d.fraud_type,''),COALESCE(d.actionable,false),
		       COALESCE(d.event_created,0),COALESCE(d.payload_sha256,''),COALESCE(d.inserted,false)
		  FROM warning_binding w
		 CROSS JOIN cash_binding c
		 LEFT JOIN durable d ON TRUE`,
		event.EventID, event.EventType, event.WarningID, event.ChargeID, event.PaymentIntent,
		event.FraudType, event.Actionable, event.EventCreated, event.PayloadSHA256).
		Scan(&chargeConflict, &paymentIntentConflict, &cashConflict,
			&storedType, &storedWarning, &storedCharge, &storedPI, &storedFraud,
			&storedActionable, &storedCreated, &storedHash, &inserted); err != nil {
		return result, err
	}
	if chargeConflict {
		return result, fmt.Errorf("Stripe risk warning %s conflicts with a different charge", event.WarningID)
	}
	if paymentIntentConflict {
		return result, fmt.Errorf("Stripe risk warning %s PaymentIntent %s conflicts with prior warning evidence",
			event.WarningID, event.PaymentIntent)
	}
	if cashConflict {
		return result, fmt.Errorf("Stripe risk event %s PaymentIntent %s conflicts with charge %s cash state",
			event.EventID, event.PaymentIntent, event.ChargeID)
	}
	if storedType == "" {
		// A concurrent same-ID insert can win after this statement's snapshot
		// was taken. The normal path above remains one round trip; this is only
		// the wait-recovery path needed to read that now-committed winner.
		if err := s.pool.QueryRow(ctx, `
			SELECT event_type,warning_id,charge_id,COALESCE(payment_intent,''),fraud_type,
			       actionable,event_created,payload_sha256
			  FROM stripe_risk_events WHERE event_id=$1`, event.EventID).
			Scan(&storedType, &storedWarning, &storedCharge, &storedPI, &storedFraud,
				&storedActionable, &storedCreated, &storedHash); err != nil {
			return result, err
		}
	}
	if !inserted {
		if storedType != event.EventType || storedWarning != event.WarningID ||
			storedCharge != event.ChargeID || storedPI != event.PaymentIntent ||
			storedFraud != event.FraudType || storedActionable != event.Actionable ||
			storedCreated != event.EventCreated || storedHash != event.PayloadSHA256 {
			return result, fmt.Errorf("Stripe risk event %s conflicts with its durable event binding", event.EventID)
		}
		result.Duplicate = true
	}
	return result, nil
}

// ensureStripeRiskWarningIdentityTx keeps all deliveries for one provider
// warning attached to one charge. Stripe may add a PaymentIntent reference on
// a later update, but it must not rewrite either the charge or an already
// observed non-empty PaymentIntent identity. The advisory lock closes the
// first-writer gap: two different events for the same warning can arrive
// concurrently while referring to different charges.
func ensureStripeRiskWarningIdentityTx(ctx context.Context, tx pgx.Tx, event stripeRiskEvent) error {
	if strings.TrimSpace(event.WarningID) == "" {
		return errors.New("Stripe risk warning identity requires a warning id")
	}
	if _, err := tx.Exec(ctx,
		`SELECT pg_advisory_xact_lock(hashtextextended($1, 0))`,
		"merc:stripe-risk-warning:"+event.WarningID,
	); err != nil {
		return err
	}
	var chargeConflict, paymentIntentConflict bool
	err := tx.QueryRow(ctx, `
		SELECT EXISTS(
		         SELECT 1 FROM stripe_risk_events
		          WHERE warning_id=$1 AND charge_id<>$2
		       ),
		       $3<>'' AND EXISTS(
		         SELECT 1 FROM stripe_risk_events
		          WHERE warning_id=$1
		            AND payment_intent IS NOT NULL
		            AND payment_intent<>$3
		       )`, event.WarningID, event.ChargeID, event.PaymentIntent).
		Scan(&chargeConflict, &paymentIntentConflict)
	if err != nil {
		return err
	}
	if chargeConflict {
		return fmt.Errorf("Stripe risk warning %s conflicts with a different charge", event.WarningID)
	}
	if paymentIntentConflict {
		return fmt.Errorf("Stripe risk warning %s PaymentIntent %s conflicts with prior warning evidence",
			event.WarningID, event.PaymentIntent)
	}
	return nil
}

// ensureStripeRiskCashBindingTx prevents a risk observation from being
// attributed to a different buyer cash obligation merely because both objects
// mention the same charge. Early warnings may arrive before the canonical cash
// row exists, so absence is retained as an unowned observation and correlated
// later; an existing conflicting identity is refused.
func ensureStripeRiskCashBindingTx(ctx context.Context, tx pgx.Tx, event stripeRiskEvent) error {
	if event.PaymentIntent == "" {
		return nil
	}
	var conflicting bool
	err := tx.QueryRow(ctx, `
		SELECT EXISTS(
		  SELECT 1 FROM buyer_cash_collections
		   WHERE charge_id=$1 AND payment_intent<>$2
		  UNION ALL
		  SELECT 1 FROM stripe_charge_cash_state
		   WHERE charge_id=$1 AND payment_intent IS NOT NULL AND payment_intent<>$2
		  UNION ALL
		  SELECT 1 FROM stripe_dispute_cash_state
		   WHERE charge_id=$1 AND payment_intent IS NOT NULL AND payment_intent<>$2
		)`, event.ChargeID, event.PaymentIntent).Scan(&conflicting)
	if err != nil {
		return err
	}
	if conflicting {
		return fmt.Errorf("Stripe risk event %s PaymentIntent %s conflicts with charge %s cash state",
			event.EventID, event.PaymentIntent, event.ChargeID)
	}
	return nil
}

type StripeRiskEventRecord struct {
	EventID       string                      `json:"event_id"`
	EventType     string                      `json:"event_type"`
	WarningID     string                      `json:"warning_id"`
	ChargeID      string                      `json:"charge_id"`
	PaymentIntent string                      `json:"payment_intent,omitempty"`
	FraudType     string                      `json:"fraud_type"`
	Actionable    bool                        `json:"actionable"`
	EventCreated  int64                       `json:"event_created"`
	Collection    *StripeRiskCollectionRecord `json:"collection,omitempty"`
}

type StripeRiskCollectionRecord struct {
	PaymentIntent string     `json:"payment_intent"`
	BuyerID       uuid.UUID  `json:"buyer_id"`
	SourceKind    string     `json:"source_kind"`
	JobID         *uuid.UUID `json:"job_id,omitempty"`
	ChargeBatchID *uuid.UUID `json:"charge_batch_id,omitempty"`
	ReceivedCents int64      `json:"received_cents"`
	Currency      string     `json:"currency"`
}

func (s *Store) ListStripeRiskEvents(ctx context.Context, limit int) ([]StripeRiskEventRecord, error) {
	if limit <= 0 || limit > 200 {
		return nil, fmt.Errorf("Stripe risk event limit must be between 1 and 200")
	}
	rows, err := s.pool.Query(ctx, `
		SELECT e.event_id,e.event_type,e.warning_id,e.charge_id,COALESCE(e.payment_intent,''),
		       e.fraud_type,e.actionable,e.event_created,
		       c.payment_intent,c.buyer_id,c.source_kind,c.job_id,c.charge_batch_id,
		       c.received_cents,c.currency
		  FROM stripe_risk_events e
		  LEFT JOIN buyer_cash_collections c
		    ON c.charge_id=e.charge_id
		   AND (e.payment_intent IS NULL OR c.payment_intent=e.payment_intent)
		 ORDER BY e.event_created DESC,e.event_id DESC
		 LIMIT $1`, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []StripeRiskEventRecord
	for rows.Next() {
		var row StripeRiskEventRecord
		var collectionPaymentIntent, collectionSourceKind, collectionCurrency *string
		var collectionBuyerID, collectionJobID, collectionBatchID *uuid.UUID
		var collectionReceivedCents *int64
		if err := rows.Scan(&row.EventID, &row.EventType, &row.WarningID, &row.ChargeID,
			&row.PaymentIntent, &row.FraudType, &row.Actionable, &row.EventCreated,
			&collectionPaymentIntent, &collectionBuyerID, &collectionSourceKind,
			&collectionJobID, &collectionBatchID, &collectionReceivedCents,
			&collectionCurrency); err != nil {
			return nil, err
		}
		if collectionBuyerID != nil {
			row.Collection = &StripeRiskCollectionRecord{
				PaymentIntent: *collectionPaymentIntent,
				BuyerID:       *collectionBuyerID,
				SourceKind:    *collectionSourceKind,
				JobID:         collectionJobID,
				ChargeBatchID: collectionBatchID,
				ReceivedCents: *collectionReceivedCents,
				Currency:      *collectionCurrency,
			}
		}
		out = append(out, row)
	}
	return out, rows.Err()
}
