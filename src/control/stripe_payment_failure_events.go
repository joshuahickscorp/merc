package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
)

const stripePaymentIntentFailedEvent = "payment_intent.payment_failed"

type stripePaymentFailureEvent struct {
	EventID       string
	EventType     string
	PaymentIntent string
	OperationKey  string
	CustomerID    string
	Status        string
	FailureType   string
	FailureCode   string
	DeclineCode   string
	EventCreated  int64
	PayloadSHA256 string
}

type stripePaymentFailureEventResult struct {
	Duplicate bool
}

func parseStripePaymentFailureEvent(
	eventID string, eventCreated int64, object map[string]any, payload []byte,
) (stripePaymentFailureEvent, error) {
	hash := sha256.Sum256(payload)
	event := stripePaymentFailureEvent{
		EventID:       strings.TrimSpace(eventID),
		EventType:     stripePaymentIntentFailedEvent,
		EventCreated:  eventCreated,
		PayloadSHA256: hex.EncodeToString(hash[:]),
	}
	if event.EventID == "" || event.EventCreated <= 0 || object == nil {
		return stripePaymentFailureEvent{}, errors.New("invalid Stripe payment-failure event")
	}
	objectType, _ := object["object"].(string)
	if strings.TrimSpace(objectType) != "payment_intent" {
		return stripePaymentFailureEvent{}, errors.New("payment-failure event has the wrong Stripe object kind")
	}
	event.PaymentIntent, _ = object["id"].(string)
	event.PaymentIntent = strings.TrimSpace(event.PaymentIntent)
	event.Status, _ = object["status"].(string)
	event.Status = strings.TrimSpace(event.Status)
	if event.Status == "" {
		event.Status = "unknown"
	}
	if !validStripeObjectID(event.PaymentIntent, "pi_") || event.Status == "succeeded" {
		return stripePaymentFailureEvent{}, errors.New("payment-failure event has an invalid PaymentIntent or status")
	}
	if metadata, ok := object["metadata"].(map[string]any); ok {
		event.OperationKey, _ = metadata["cx_operation_key"].(string)
		event.OperationKey = strings.TrimSpace(event.OperationKey)
	}
	var err error
	event.CustomerID, err = stripeExpandableMapID(object, "customer", "customer")
	if err != nil {
		return stripePaymentFailureEvent{}, fmt.Errorf("decode payment-failure customer: %w", err)
	}
	if failure, ok := object["last_payment_error"].(map[string]any); ok {
		event.FailureType, _ = failure["type"].(string)
		event.FailureCode, _ = failure["code"].(string)
		event.DeclineCode, _ = failure["decline_code"].(string)
	}
	event.FailureType = truncate(strings.TrimSpace(event.FailureType), 120)
	event.FailureCode = truncate(strings.TrimSpace(event.FailureCode), 120)
	event.DeclineCode = truncate(strings.TrimSpace(event.DeclineCode), 120)
	return event, nil
}

func validateStripePaymentFailureEvent(event stripePaymentFailureEvent) error {
	if strings.TrimSpace(event.EventID) == "" || event.EventType != stripePaymentIntentFailedEvent ||
		!validStripeObjectID(event.PaymentIntent, "pi_") ||
		strings.TrimSpace(event.Status) == "" || event.Status == "succeeded" || event.EventCreated <= 0 ||
		!isSHA256Hex(event.PayloadSHA256) {
		return errors.New("invalid Stripe payment-failure event")
	}
	return nil
}

func (s *Store) ApplyStripePaymentFailureEvent(
	ctx context.Context, event stripePaymentFailureEvent,
) (stripePaymentFailureEventResult, error) {
	var result stripePaymentFailureEventResult
	if err := validateStripePaymentFailureEvent(event); err != nil {
		return result, err
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return result, err
	}
	defer tx.Rollback(ctx)

	tag, err := tx.Exec(ctx, `
		INSERT INTO stripe_payment_failure_events
		  (event_id,event_type,payment_intent,operation_key,customer_id,status,
		   failure_type,failure_code,decline_code,event_created,payload_sha256)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11)
		ON CONFLICT (event_id) DO NOTHING`,
		event.EventID, event.EventType, event.PaymentIntent, nullableStripeID(event.OperationKey),
		nullableStripeID(event.CustomerID), event.Status, nullableStripeID(event.FailureType),
		nullableStripeID(event.FailureCode), nullableStripeID(event.DeclineCode),
		event.EventCreated, event.PayloadSHA256)
	if err != nil {
		return result, err
	}
	if tag.RowsAffected() == 0 {
		var storedType, storedPI, storedOperation, storedCustomer, storedStatus string
		var storedFailureType, storedFailureCode, storedDeclineCode, storedHash string
		var storedCreated int64
		if err := tx.QueryRow(ctx, `
			SELECT event_type,payment_intent,COALESCE(operation_key,''),COALESCE(customer_id,''),status,
			       COALESCE(failure_type,''),COALESCE(failure_code,''),COALESCE(decline_code,''),
			       event_created,payload_sha256
			  FROM stripe_payment_failure_events WHERE event_id=$1`, event.EventID).
			Scan(&storedType, &storedPI, &storedOperation, &storedCustomer, &storedStatus,
				&storedFailureType, &storedFailureCode, &storedDeclineCode, &storedCreated, &storedHash); err != nil {
			return result, err
		}
		if storedType != event.EventType || storedPI != event.PaymentIntent ||
			storedOperation != event.OperationKey || storedCustomer != event.CustomerID ||
			storedStatus != event.Status || storedFailureType != event.FailureType ||
			storedFailureCode != event.FailureCode || storedDeclineCode != event.DeclineCode ||
			storedCreated != event.EventCreated || storedHash != event.PayloadSHA256 {
			return result, fmt.Errorf("Stripe payment-failure event %s conflicts with its durable event binding", event.EventID)
		}
		result.Duplicate = true
	}
	if err := tx.Commit(ctx); err != nil {
		return stripePaymentFailureEventResult{}, err
	}
	return result, nil
}

type StripePaymentFailureEventRecord struct {
	EventID       string `json:"event_id"`
	EventType     string `json:"event_type"`
	PaymentIntent string `json:"payment_intent"`
	OperationKey  string `json:"operation_key,omitempty"`
	CustomerID    string `json:"customer_id,omitempty"`
	Status        string `json:"status"`
	FailureType   string `json:"failure_type,omitempty"`
	FailureCode   string `json:"failure_code,omitempty"`
	DeclineCode   string `json:"decline_code,omitempty"`
	EventCreated  int64  `json:"event_created"`
}

func (s *Store) ListStripePaymentFailureEvents(
	ctx context.Context, limit int,
) ([]StripePaymentFailureEventRecord, error) {
	if limit <= 0 || limit > 200 {
		return nil, fmt.Errorf("Stripe payment-failure event limit must be between 1 and 200")
	}
	rows, err := s.pool.Query(ctx, `
		SELECT event_id,event_type,payment_intent,COALESCE(operation_key,''),COALESCE(customer_id,''),
		       status,COALESCE(failure_type,''),COALESCE(failure_code,''),COALESCE(decline_code,''),event_created
		  FROM stripe_payment_failure_events
		 ORDER BY event_created DESC,event_id DESC
		 LIMIT $1`, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []StripePaymentFailureEventRecord
	for rows.Next() {
		var row StripePaymentFailureEventRecord
		if err := rows.Scan(&row.EventID, &row.EventType, &row.PaymentIntent, &row.OperationKey,
			&row.CustomerID, &row.Status, &row.FailureType, &row.FailureCode,
			&row.DeclineCode, &row.EventCreated); err != nil {
			return nil, err
		}
		out = append(out, row)
	}
	return out, rows.Err()
}
