package main

import (
	"errors"
	"testing"

	"github.com/google/uuid"
)

func TestStripeExternalIdentityIdempotencyKeysAreStableAndScoped(t *testing.T) {
	buyerID, supplierID := uuid.New(), uuid.New()
	if got, want := stripeCustomerIdempotencyKey(buyerID), "cx-customer-"+buyerID.String(); got != want {
		t.Fatalf("customer idempotency key=%q, want %q", got, want)
	}
	if got, want := stripeConnectAccountIdempotencyKey(supplierID), "cx-connect-account-"+supplierID.String(); got != want {
		t.Fatalf("Connect idempotency key=%q, want %q", got, want)
	}
	if stripeCustomerIdempotencyKey(buyerID) == stripeConnectAccountIdempotencyKey(supplierID) {
		t.Fatal("customer and Connect identity idempotency keys share a namespace")
	}
}

func TestStripeConnectEventParserRequiresBoundAccountReadiness(t *testing.T) {
	for _, tc := range []struct {
		name string
		acct string
		obj  map[string]any
		want error
	}{
		{
			name: "missing readiness fact",
			acct: "acct_parser_missing",
			obj:  map[string]any{"id": "acct_parser_missing"},
			want: errInvalidStripeConnectEvent,
		},
		{
			name: "envelope object mismatch",
			acct: "acct_parser_envelope",
			obj:  map[string]any{"id": "acct_parser_object", "payouts_enabled": true},
			want: errInvalidStripeConnectEvent,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := parseStripeConnectEvent("evt_parser_"+tc.name, stripeConnectEventAccountUpdated,
				tc.acct, 1_700_000_000, tc.obj, []byte(`{"event":"connect"}`))
			if !errors.Is(err, tc.want) {
				t.Fatalf("parse error=%v, want %v", err, tc.want)
			}
		})
	}
}

func TestStripeConnectEventsAreDurableMonotonicAndSeparateFromMercPayouts(t *testing.T) {
	ctx, store, pool := openIsolatedTestStore(t)
	supplierID, buyerID := uuid.New(), uuid.New()
	acct := "acct_connect_order_" + supplierID.String()[:8]
	_, err := pool.Exec(ctx, `INSERT INTO buyers (id,email) VALUES ($1,$2)`,
		buyerID, buyerID.String()+"@connect-events.invalid")
	mustf(t, err, "insert buyer: %v")
	_, err = pool.Exec(ctx, `
		INSERT INTO suppliers (id,email,owner_buyer_id,stripe_acct,payouts_enabled)
		VALUES ($1,$2,$3,$4,false)`, supplierID, supplierID.String()+"@connect-events.invalid", buyerID, acct)
	mustf(t, err, "insert supplier: %v")

	payload := []byte(`{"id":"evt_connect_new","type":"account.updated","created":1700000002}`)
	newEvent, err := parseStripeConnectEvent(
		"evt_connect_new", stripeConnectEventAccountUpdated, acct, 1_700_000_002,
		map[string]any{"id": acct, "payouts_enabled": true}, payload)
	mustf(t, err, "parse new account event: %v")
	result, err := store.ApplyConnectWebhookEvent(ctx, newEvent)
	mustf(t, err, "apply new account event: %v")
	if !result.Applied || result.Duplicate || result.Stale {
		t.Fatalf("new event result=%+v, want applied only", result)
	}

	var enabled bool
	var eventCreated int64
	var eventID string
	mustf(t, pool.QueryRow(ctx, `
		SELECT payouts_enabled,payouts_enabled_event_created,payouts_enabled_event_id
		  FROM suppliers WHERE id=$1`, supplierID).Scan(&enabled, &eventCreated, &eventID),
		"read supplier readiness: %v")
	if !enabled || eventCreated != 1_700_000_002 || eventID != "evt_connect_new" {
		t.Fatalf("readiness=(%v,%d,%q), want true/new event", enabled, eventCreated, eventID)
	}

	stalePayload := []byte(`{"id":"evt_connect_old","type":"account.updated","created":1700000001}`)
	staleEvent, err := parseStripeConnectEvent(
		"evt_connect_old", stripeConnectEventAccountUpdated, acct, 1_700_000_001,
		map[string]any{"id": acct, "payouts_enabled": false}, stalePayload)
	mustf(t, err, "parse stale account event: %v")
	result, err = store.ApplyConnectWebhookEvent(ctx, staleEvent)
	mustf(t, err, "apply stale account event: %v")
	if !result.Stale || result.Applied || result.Duplicate {
		t.Fatalf("stale event result=%+v, want stale only", result)
	}
	mustf(t, pool.QueryRow(ctx, `SELECT payouts_enabled FROM suppliers WHERE id=$1`, supplierID).
		Scan(&enabled), "read readiness after stale event: %v")
	if !enabled {
		t.Fatal("stale account.updated regressed payouts_enabled")
	}

	result, err = store.ApplyConnectWebhookEvent(ctx, newEvent)
	mustf(t, err, "replay account event: %v")
	if !result.Duplicate || result.Applied || result.Stale {
		t.Fatalf("replay result=%+v, want duplicate only", result)
	}

	payoutPayload := []byte(`{"id":"evt_connect_payout","type":"payout.created","created":1700000003}`)
	payoutEvent, err := parseStripeConnectEvent(
		"evt_connect_payout", stripeConnectEventPayoutCreated, acct, 1_700_000_003,
		map[string]any{"id": "po_connect_1", "status": "pending"}, payoutPayload)
	mustf(t, err, "parse payout event: %v")
	result, err = store.ApplyConnectWebhookEvent(ctx, payoutEvent)
	mustf(t, err, "apply payout event: %v")
	if !result.Applied || result.Duplicate || result.Stale {
		t.Fatalf("payout result=%+v, want recorded/applied only", result)
	}
	mustf(t, pool.QueryRow(ctx, `SELECT payouts_enabled FROM suppliers WHERE id=$1`, supplierID).
		Scan(&enabled), "read readiness after payout event: %v")
	if !enabled {
		t.Fatal("payout.created changed supplier account readiness")
	}

	conflictPayload := []byte(`{"id":"evt_connect_new","type":"account.updated","created":1700000002,"changed":true}`)
	conflictEvent, err := parseStripeConnectEvent(
		"evt_connect_new", stripeConnectEventAccountUpdated, acct, 1_700_000_002,
		map[string]any{"id": acct, "payouts_enabled": false}, conflictPayload)
	mustf(t, err, "parse conflicting event: %v")
	if _, err := store.ApplyConnectWebhookEvent(ctx, conflictEvent); err == nil {
		t.Fatal("same Connect event id with different payload was accepted")
	}

	unknown, err := parseStripeConnectEvent(
		"evt_connect_unknown", stripeConnectEventPayoutPaid, "acct_missing",
		1_700_000_004, map[string]any{"id": "po_connect_2"}, []byte(`{"x":1}`))
	mustf(t, err, "parse unknown-account event: %v")
	if _, err := store.ApplyConnectWebhookEvent(ctx, unknown); !errors.Is(err, errUnknownConnectAccount) {
		t.Fatalf("unknown account error=%v, want %v", err, errUnknownConnectAccount)
	}
}
