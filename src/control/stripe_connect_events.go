package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

const (
	stripeConnectEventAccountUpdated           = "account.updated"
	stripeConnectEventCapabilityUpdated        = "capability.updated"
	stripeConnectEventPayoutCreated            = "payout.created"
	stripeConnectEventPayoutUpdated            = "payout.updated"
	stripeConnectEventPayoutPaid               = "payout.paid"
	stripeConnectEventPayoutFailed             = "payout.failed"
	stripeConnectEventPayoutCanceled           = "payout.canceled"
	stripeConnectEventPayoutReconciliationDone = "payout.reconciliation_completed"
)

var (
	errInvalidStripeConnectEvent = errors.New("invalid Stripe Connect event")
	errUnknownConnectAccount     = errors.New("unknown connected account")
)

type stripeConnectEvent struct {
	EventID          string
	EventType        string
	AccountID        string
	ObjectID         string
	EventCreated     int64
	PayloadSHA256    string
	PayoutsEnabled   *bool
	CapabilityStatus string
}

type stripeConnectEventResult struct {
	Duplicate bool
	Stale     bool
	Applied   bool
}

func isStripeConnectEventType(eventType string) bool {
	switch eventType {
	case stripeConnectEventAccountUpdated,
		stripeConnectEventCapabilityUpdated,
		stripeConnectEventPayoutCreated,
		stripeConnectEventPayoutUpdated,
		stripeConnectEventPayoutPaid,
		stripeConnectEventPayoutFailed,
		stripeConnectEventPayoutCanceled,
		stripeConnectEventPayoutReconciliationDone:
		return true
	default:
		return false
	}
}

func parseStripeConnectEvent(
	eventID, eventType, envelopeAccount string, eventCreated int64,
	object map[string]any, payload []byte,
) (stripeConnectEvent, error) {
	hash := sha256.Sum256(payload)
	event := stripeConnectEvent{
		EventID:       strings.TrimSpace(eventID),
		EventType:     strings.TrimSpace(eventType),
		EventCreated:  eventCreated,
		PayloadSHA256: hex.EncodeToString(hash[:]),
	}
	if event.EventID == "" || event.EventCreated <= 0 || !isStripeConnectEventType(event.EventType) || object == nil {
		return stripeConnectEvent{}, errInvalidStripeConnectEvent
	}

	objectID, _ := object["id"].(string)
	objectID = strings.TrimSpace(objectID)
	event.ObjectID = objectID
	switch event.EventType {
	case stripeConnectEventAccountUpdated:
		objectType, _ := object["object"].(string)
		if strings.TrimSpace(objectType) != "account" {
			return stripeConnectEvent{}, fmt.Errorf("%w: account.updated has the wrong Stripe object kind", errInvalidStripeConnectEvent)
		}
		objectAccount, _ := object["id"].(string)
		objectAccount = strings.TrimSpace(objectAccount)
		envelopeAccount = strings.TrimSpace(envelopeAccount)
		if stripeConnectedAccountMismatch(envelopeAccount, objectAccount) {
			return stripeConnectEvent{}, fmt.Errorf("%w: envelope account does not match account object", errInvalidStripeConnectEvent)
		}
		event.AccountID = objectAccount
		if event.AccountID == "" {
			event.AccountID = envelopeAccount
		}
		if !validStripeObjectID(event.AccountID, "acct_") {
			return stripeConnectEvent{}, fmt.Errorf("%w: account id is not an acct_* identifier", errInvalidStripeConnectEvent)
		}
		payoutsEnabled, ok := object["payouts_enabled"].(bool)
		if !ok {
			return stripeConnectEvent{}, fmt.Errorf("%w: account.updated omitted payouts_enabled", errInvalidStripeConnectEvent)
		}
		event.PayoutsEnabled = &payoutsEnabled
	case stripeConnectEventCapabilityUpdated:
		objectType, _ := object["object"].(string)
		if strings.TrimSpace(objectType) != "capability" {
			return stripeConnectEvent{}, fmt.Errorf("%w: capability.updated has the wrong Stripe object kind", errInvalidStripeConnectEvent)
		}
		objectAccount, _ := object["account"].(string)
		objectAccount = strings.TrimSpace(objectAccount)
		envelopeAccount = strings.TrimSpace(envelopeAccount)
		if stripeConnectedAccountMismatch(envelopeAccount, objectAccount) {
			return stripeConnectEvent{}, fmt.Errorf("%w: envelope account does not match capability account", errInvalidStripeConnectEvent)
		}
		event.AccountID = envelopeAccount
		if event.AccountID == "" {
			event.AccountID = objectAccount
		}
		if !validStripeObjectID(event.AccountID, "acct_") || !validStripeCapabilityID(event.ObjectID) {
			return stripeConnectEvent{}, fmt.Errorf("%w: capability event is missing its connected account or capability id", errInvalidStripeConnectEvent)
		}
		status, _ := object["status"].(string)
		event.CapabilityStatus = strings.TrimSpace(status)
		if !isStripeCapabilityStatus(event.CapabilityStatus) {
			return stripeConnectEvent{}, fmt.Errorf("%w: capability.updated has an invalid capability status", errInvalidStripeConnectEvent)
		}
	case stripeConnectEventPayoutCreated, stripeConnectEventPayoutUpdated,
		stripeConnectEventPayoutPaid, stripeConnectEventPayoutFailed,
		stripeConnectEventPayoutCanceled, stripeConnectEventPayoutReconciliationDone:
		objectType, _ := object["object"].(string)
		if strings.TrimSpace(objectType) != "payout" {
			return stripeConnectEvent{}, fmt.Errorf("%w: payout event has the wrong Stripe object kind", errInvalidStripeConnectEvent)
		}
		event.AccountID = strings.TrimSpace(envelopeAccount)
		if !validStripeObjectID(event.AccountID, "acct_") || !validStripeObjectID(objectID, "po_") {
			return stripeConnectEvent{}, fmt.Errorf("%w: payout event is missing its connected account or object id", errInvalidStripeConnectEvent)
		}
	}
	return event, nil
}

func validateStripeConnectEvent(event stripeConnectEvent) error {
	if strings.TrimSpace(event.EventID) == "" || !isStripeConnectEventType(event.EventType) ||
		!validStripeObjectID(event.AccountID, "acct_") || event.EventCreated <= 0 ||
		!isSHA256Hex(event.PayloadSHA256) {
		return errInvalidStripeConnectEvent
	}
	if event.EventType == stripeConnectEventCapabilityUpdated {
		if !validStripeCapabilityID(event.ObjectID) || !isStripeCapabilityStatus(event.CapabilityStatus) {
			return fmt.Errorf("%w: capability event has an invalid id or status", errInvalidStripeConnectEvent)
		}
		return nil
	}
	objectPrefix := "po_"
	if event.EventType == stripeConnectEventAccountUpdated {
		objectPrefix = "acct_"
	}
	if !validStripeObjectID(event.ObjectID, objectPrefix) {
		return fmt.Errorf("%w: event object id has the wrong Stripe prefix", errInvalidStripeConnectEvent)
	}
	if event.EventType == stripeConnectEventAccountUpdated && event.PayoutsEnabled == nil {
		return fmt.Errorf("%w: account.updated has no payouts_enabled fact", errInvalidStripeConnectEvent)
	}
	return nil
}

func (s *Store) ApplyConnectWebhookEvent(ctx context.Context, event stripeConnectEvent) (stripeConnectEventResult, error) {
	var result stripeConnectEventResult
	if err := validateStripeConnectEvent(event); err != nil {
		return result, err
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return result, err
	}
	defer tx.Rollback(ctx)

	var supplierID uuid.UUID
	var currentCreated int64
	var currentID string
	if err := tx.QueryRow(ctx, `
		SELECT id,COALESCE(payouts_enabled_event_created,0),COALESCE(payouts_enabled_event_id,'')
		  FROM suppliers WHERE stripe_acct=$1 FOR UPDATE`, event.AccountID).
		Scan(&supplierID, &currentCreated, &currentID); errors.Is(err, pgx.ErrNoRows) {
		return result, errUnknownConnectAccount
	} else if err != nil {
		return result, err
	}

	var capabilityStatus *string
	if event.EventType == stripeConnectEventCapabilityUpdated {
		capabilityStatus = &event.CapabilityStatus
	}
	tag, err := tx.Exec(ctx, `
		INSERT INTO stripe_connect_webhook_events
		  (event_id,event_type,account_id,object_id,capability_status,event_created,payload_sha256)
		VALUES ($1,$2,$3,$4,$5,$6,$7)
		ON CONFLICT (event_id) DO NOTHING`,
		event.EventID, event.EventType, event.AccountID, event.ObjectID,
		capabilityStatus, event.EventCreated, event.PayloadSHA256)
	if err != nil {
		return result, err
	}
	if tag.RowsAffected() == 0 {
		var storedType, storedAccount, storedObject, storedCapabilityStatus, storedHash string
		var storedCreated int64
		if err := tx.QueryRow(ctx, `
				SELECT event_type,account_id,object_id,COALESCE(capability_status,''),event_created,payload_sha256
				  FROM stripe_connect_webhook_events WHERE event_id=$1`, event.EventID).
			Scan(&storedType, &storedAccount, &storedObject, &storedCapabilityStatus, &storedCreated, &storedHash); err != nil {
			return result, err
		}
		if storedType != event.EventType || storedAccount != event.AccountID ||
			storedObject != event.ObjectID || storedCreated != event.EventCreated ||
			storedCapabilityStatus != event.CapabilityStatus || storedHash != event.PayloadSHA256 {
			return result, fmt.Errorf("Stripe Connect event %s conflicts with its durable event binding", event.EventID)
		}
		result.Duplicate = true
		if err := tx.Commit(ctx); err != nil {
			return stripeConnectEventResult{}, err
		}
		return result, nil
	}

	if event.EventType == stripeConnectEventAccountUpdated {
		newer := event.EventCreated > currentCreated ||
			(event.EventCreated == currentCreated && event.EventID > currentID)
		if newer {
			if _, err := tx.Exec(ctx, `
				UPDATE suppliers
				   SET payouts_enabled=$2,payouts_enabled_event_created=$3,payouts_enabled_event_id=$4
				 WHERE id=$1`, supplierID, *event.PayoutsEnabled, event.EventCreated, event.EventID); err != nil {
				return result, err
			}
			result.Applied = true
		} else {
			result.Stale = true
		}
	} else {
		// capability.updated and payout.* are durable provider observations only:
		// Merc's internal supplier credit, Stripe transfers, and connected-account
		// bank payouts are different objects and must not be settled by this
		// notification.
		result.Applied = true
	}
	if err := tx.Commit(ctx); err != nil {
		return stripeConnectEventResult{}, err
	}
	return result, nil
}

func validStripeCapabilityID(value string) bool {
	value = strings.TrimSpace(value)
	if value == "" || len(value) > 100 {
		return false
	}
	for i := 0; i < len(value); i++ {
		c := value[i]
		if !((c >= 'a' && c <= 'z') || (c >= '0' && c <= '9') || c == '_') {
			return false
		}
	}
	return true
}

func isStripeCapabilityStatus(status string) bool {
	switch strings.TrimSpace(status) {
	case "active", "inactive", "pending", "unrequested":
		return true
	default:
		return false
	}
}
