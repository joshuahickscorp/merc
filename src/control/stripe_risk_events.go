package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
)

const (
	stripeRiskEventEarlyFraudWarningCreated = "radar.early_fraud_warning.created"
	stripeRiskEventEarlyFraudWarningUpdated = "radar.early_fraud_warning.updated"
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
	event.WarningID, _ = object["id"].(string)
	event.WarningID = strings.TrimSpace(event.WarningID)
	var err error
	event.ChargeID, err = stripeExpandableMapID(object, "charge")
	if err != nil {
		return stripeRiskEvent{}, fmt.Errorf("decode early-fraud charge: %w", err)
	}
	event.PaymentIntent, err = stripeExpandableMapID(object, "payment_intent")
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
	return event, nil
}

func stripeExpandableMapID(object map[string]any, field string) (string, error) {
	raw, ok := object[field]
	if !ok || raw == nil {
		return "", nil
	}
	encoded, err := json.Marshal(raw)
	if err != nil {
		return "", err
	}
	return stripeExpandableID(encoded)
}

func validateStripeRiskEvent(event stripeRiskEvent) error {
	if strings.TrimSpace(event.EventID) == "" || !isStripeRiskEventType(event.EventType) ||
		strings.TrimSpace(event.WarningID) == "" || strings.TrimSpace(event.ChargeID) == "" ||
		strings.TrimSpace(event.FraudType) == "" || event.EventCreated <= 0 ||
		len(event.PayloadSHA256) != sha256.Size*2 {
		return errors.New("invalid Stripe risk event")
	}
	return nil
}

func (s *Store) ApplyStripeRiskEvent(ctx context.Context, event stripeRiskEvent) (stripeRiskEventResult, error) {
	var result stripeRiskEventResult
	if err := validateStripeRiskEvent(event); err != nil {
		return result, err
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return result, err
	}
	defer tx.Rollback(ctx)

	tag, err := tx.Exec(ctx, `
		INSERT INTO stripe_risk_events
		  (event_id,event_type,warning_id,charge_id,payment_intent,fraud_type,actionable,event_created,payload_sha256)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9)
		ON CONFLICT (event_id) DO NOTHING`,
		event.EventID, event.EventType, event.WarningID, event.ChargeID,
		nullableStripeID(event.PaymentIntent), event.FraudType, event.Actionable,
		event.EventCreated, event.PayloadSHA256)
	if err != nil {
		return result, err
	}
	if tag.RowsAffected() == 0 {
		var storedType, storedWarning, storedCharge, storedPI, storedFraud, storedHash string
		var storedActionable bool
		var storedCreated int64
		if err := tx.QueryRow(ctx, `
			SELECT event_type,warning_id,charge_id,COALESCE(payment_intent,''),fraud_type,
			       actionable,event_created,payload_sha256
			  FROM stripe_risk_events WHERE event_id=$1`, event.EventID).
			Scan(&storedType, &storedWarning, &storedCharge, &storedPI, &storedFraud,
				&storedActionable, &storedCreated, &storedHash); err != nil {
			return result, err
		}
		if storedType != event.EventType || storedWarning != event.WarningID ||
			storedCharge != event.ChargeID || storedPI != event.PaymentIntent ||
			storedFraud != event.FraudType || storedActionable != event.Actionable ||
			storedCreated != event.EventCreated || storedHash != event.PayloadSHA256 {
			return result, fmt.Errorf("Stripe risk event %s conflicts with its durable event binding", event.EventID)
		}
		result.Duplicate = true
	}
	if err := tx.Commit(ctx); err != nil {
		return stripeRiskEventResult{}, err
	}
	return result, nil
}

type StripeRiskEventRecord struct {
	EventID       string `json:"event_id"`
	EventType     string `json:"event_type"`
	WarningID     string `json:"warning_id"`
	ChargeID      string `json:"charge_id"`
	PaymentIntent string `json:"payment_intent,omitempty"`
	FraudType     string `json:"fraud_type"`
	Actionable    bool   `json:"actionable"`
	EventCreated  int64  `json:"event_created"`
}

func (s *Store) ListStripeRiskEvents(ctx context.Context, limit int) ([]StripeRiskEventRecord, error) {
	if limit <= 0 || limit > 200 {
		return nil, fmt.Errorf("Stripe risk event limit must be between 1 and 200")
	}
	rows, err := s.pool.Query(ctx, `
		SELECT event_id,event_type,warning_id,charge_id,COALESCE(payment_intent,''),
		       fraud_type,actionable,event_created
		  FROM stripe_risk_events
		 ORDER BY event_created DESC,event_id DESC
		 LIMIT $1`, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []StripeRiskEventRecord
	for rows.Next() {
		var row StripeRiskEventRecord
		if err := rows.Scan(&row.EventID, &row.EventType, &row.WarningID, &row.ChargeID,
			&row.PaymentIntent, &row.FraudType, &row.Actionable, &row.EventCreated); err != nil {
			return nil, err
		}
		out = append(out, row)
	}
	return out, rows.Err()
}
