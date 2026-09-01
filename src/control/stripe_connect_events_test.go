package main

import (
	"errors"
	"strings"
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

func TestStripeExternalIdentityIDsFailClosedOnWrongOrEmptyPrefix(t *testing.T) {
	for _, tc := range []struct {
		name, value, prefix string
		want                bool
	}{
		{"customer", "cus_valid", "cus_", true},
		{"account", "acct_valid", "acct_", true},
		{"empty suffix", "cus_", "cus_", false},
		{"wrong prefix", "acct_wrong", "cus_", false},
		{"whitespace", " cus_valid ", "cus_", true},
		{"query delimiter", "acct_bad&limit=1", "acct_", false},
		{"path delimiter", "pi_bad/other", "pi_", false},
		{"space", "pm_bad value", "pm_", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := validStripeObjectID(tc.value, tc.prefix); got != tc.want {
				t.Fatalf("validStripeObjectID(%q,%q)=%v, want %v", tc.value, tc.prefix, got, tc.want)
			}
		})
	}
}

func TestStripeConnectAccountStatusRequiresExactProviderResponse(t *testing.T) {
	for _, tc := range []struct {
		name string
		acct string
		out  map[string]any
		want bool
		good bool
	}{
		{
			name: "enabled exact account",
			acct: "acct_status_exact",
			out:  map[string]any{"object": "account", "id": "acct_status_exact", "payouts_enabled": true},
			want: true,
			good: true,
		},
		{
			name: "disabled exact account",
			acct: "acct_status_disabled",
			out:  map[string]any{"object": "account", "id": "acct_status_disabled", "payouts_enabled": false},
			good: true,
		},
		{
			name: "wrong object type",
			acct: "acct_status_object",
			out:  map[string]any{"object": "person", "id": "acct_status_object", "payouts_enabled": true},
		},
		{
			name: "missing object type",
			acct: "acct_status_missing_object",
			out:  map[string]any{"id": "acct_status_missing_object", "payouts_enabled": true},
		},
		{
			name: "wrong account",
			acct: "acct_status_expected",
			out:  map[string]any{"object": "account", "id": "acct_status_other", "payouts_enabled": true},
		},
		{
			name: "missing readiness",
			acct: "acct_status_missing_readiness",
			out:  map[string]any{"object": "account", "id": "acct_status_missing_readiness"},
		},
		{
			name: "wrong readiness type",
			acct: "acct_status_wrong_readiness",
			out:  map[string]any{"object": "account", "id": "acct_status_wrong_readiness", "payouts_enabled": "true"},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := parseStripeConnectAccountStatus(tc.out, tc.acct)
			if tc.good {
				if err != nil {
					t.Fatalf("parse error=%v", err)
				}
				if got != tc.want {
					t.Fatalf("payouts_enabled=%v, want %v", got, tc.want)
				}
				return
			}
			if err == nil {
				t.Fatal("malformed provider response was accepted")
			}
		})
	}
}

func TestStripeConnectAccountStatusReportsTransfersCapabilitySeparately(t *testing.T) {
	for _, tc := range []struct {
		name       string
		out        map[string]any
		wantStatus string
		good       bool
	}{
		{
			name: "active capability",
			out: map[string]any{
				"object": "account", "id": "acct_status_capability_active", "payouts_enabled": true,
				"capabilities": map[string]any{"transfers": "active"},
			},
			wantStatus: "active", good: true,
		},
		{
			name: "non-active capability",
			out: map[string]any{
				"object": "account", "id": "acct_status_capability_pending", "payouts_enabled": true,
				"capabilities": map[string]any{"transfers": "pending"},
			},
			wantStatus: "pending", good: true,
		},
		{
			name: "historical response without capability",
			out: map[string]any{
				"object": "account", "id": "acct_status_capability_missing", "payouts_enabled": false,
			},
			wantStatus: stripeConnectTransfersCapabilityUnknown, good: true,
		},
		{
			name: "malformed capability map",
			out: map[string]any{
				"object": "account", "id": "acct_status_capability_map", "payouts_enabled": true,
				"capabilities": "active",
			},
		},
		{
			name: "invalid capability status",
			out: map[string]any{
				"object": "account", "id": "acct_status_capability_invalid", "payouts_enabled": true,
				"capabilities": map[string]any{"transfers": "restricted"},
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			status, err := parseStripeConnectAccountStatusDetails(tc.out, tc.out["id"].(string))
			if tc.good {
				if err != nil {
					t.Fatalf("parse error=%v", err)
				}
				if status.TransfersCapability != tc.wantStatus {
					t.Fatalf("transfers capability=%q, want %q", status.TransfersCapability, tc.wantStatus)
				}
				return
			}
			if err == nil {
				t.Fatal("malformed provider response was accepted")
			}
		})
	}
}

func TestStripeAccountLinkResponseRequiresStripeHostedSingleUseURL(t *testing.T) {
	for _, tc := range []struct {
		name string
		out  map[string]any
		want string
		good bool
	}{
		{
			name: "exact hosted link",
			out:  map[string]any{"object": "account_link", "url": "https://connect.stripe.com/setup/c/acct_link_exact/token"},
			want: "https://connect.stripe.com/setup/c/acct_link_exact/token",
			good: true,
		},
		{
			name: "trailing dot hostname",
			out:  map[string]any{"object": "account_link", "url": "https://connect.stripe.com./setup/c/acct_link_dot/token"},
			want: "https://connect.stripe.com./setup/c/acct_link_dot/token",
			good: true,
		},
		{
			name: "wrong object type",
			out:  map[string]any{"object": "account", "url": "https://connect.stripe.com/setup/c/acct_link_object/token"},
		},
		{
			name: "missing object type",
			out:  map[string]any{"url": "https://connect.stripe.com/setup/c/acct_link_missing/token"},
		},
		{
			name: "wrong host",
			out:  map[string]any{"object": "account_link", "url": "https://example.invalid/setup/c/acct_link_host/token"},
		},
		{
			name: "insecure scheme",
			out:  map[string]any{"object": "account_link", "url": "http://connect.stripe.com/setup/c/acct_link_http/token"},
		},
		{
			name: "credentials in URL",
			out:  map[string]any{"object": "account_link", "url": "https://user:pass@connect.stripe.com/setup/c/acct_link_userinfo/token"},
		},
		{
			name: "fragment in URL",
			out:  map[string]any{"object": "account_link", "url": "https://connect.stripe.com/setup/c/acct_link_fragment/token#fragment"},
		},
		{
			name: "missing URL",
			out:  map[string]any{"object": "account_link"},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := parseStripeAccountLinkResponse(tc.out)
			if tc.good {
				if err != nil {
					t.Fatalf("parse error=%v", err)
				}
				if got != tc.want {
					t.Fatalf("link=%q, want %q", got, tc.want)
				}
				return
			}
			if err == nil {
				t.Fatal("malformed account-link response was accepted")
			}
		})
	}
}

func TestStripeAccountLinkResponseBindsRequestedAccount(t *testing.T) {
	acct := "acct_requested_link"
	good := map[string]any{
		"object": "account_link",
		"url":    "https://connect.stripe.com/setup/c/" + acct + "/token",
	}
	link, err := parseStripeAccountLinkResponseForAccount(good, acct)
	mustf(t, err, "parse account-bound link: %v")
	if link != good["url"] {
		t.Fatalf("link=%q, want %q", link, good["url"])
	}

	for _, tc := range []struct {
		name string
		acct string
		url  string
	}{
		{
			name: "different account",
			acct: acct,
			url:  "https://connect.stripe.com/setup/c/acct_other/token",
		},
		{
			name: "wrong setup path",
			acct: acct,
			url:  "https://connect.stripe.com/setup/s/" + acct + "/token",
		},
		{
			name: "missing account path segment",
			acct: acct,
			url:  "https://connect.stripe.com/setup/c/token",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := parseStripeAccountLinkResponseForAccount(
				map[string]any{"object": "account_link", "url": tc.url}, tc.acct,
			); err == nil {
				t.Fatal("accepted an account link not bound to the requested account")
			}
		})
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
			obj:  map[string]any{"object": "account", "id": "acct_parser_missing"},
			want: errInvalidStripeConnectEvent,
		},
		{
			name: "envelope object mismatch",
			acct: "acct_parser_envelope",
			obj:  map[string]any{"object": "account", "id": "acct_parser_object", "payouts_enabled": true},
			want: errInvalidStripeConnectEvent,
		},
		{
			name: "payout object prefix",
			acct: "acct_parser_payout",
			obj:  map[string]any{"object": "account", "id": "not_a_payout"},
			want: errInvalidStripeConnectEvent,
		},
		{
			name: "account wrong object kind",
			acct: "acct_parser_wrong_kind",
			obj:  map[string]any{"object": "payout", "id": "acct_parser_wrong_kind", "payouts_enabled": true},
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

func TestStripeConnectPayoutParserRequiresPayoutObject(t *testing.T) {
	_, err := parseStripeConnectEvent(
		"evt_connect_wrong_payout_object", stripeConnectEventPayoutCreated, "acct_parser_payout_kind",
		1_700_000_005, map[string]any{"object": "account", "id": "po_parser_payout_kind", "status": "pending"},
		[]byte(`{"event":"connect"}`),
	)
	if !errors.Is(err, errInvalidStripeConnectEvent) {
		t.Fatalf("parse error=%v, want %v", err, errInvalidStripeConnectEvent)
	}
}

func TestStripeConnectCapabilityParserBindsAccountIDAndStatus(t *testing.T) {
	acct := "acct_parser_capability"
	event, err := parseStripeConnectEvent(
		"evt_connect_capability", stripeConnectEventCapabilityUpdated, acct, 1_700_000_006,
		map[string]any{
			"object": "capability", "id": "transfers", "account": acct, "status": "active",
		}, []byte(`{"event":"capability.updated"}`))
	mustf(t, err, "parse capability event: %v")
	if event.AccountID != acct || event.ObjectID != "transfers" || event.CapabilityStatus != "active" {
		t.Fatalf("capability event=%+v, want bound account/id/status", event)
	}

	for _, tc := range []struct {
		name string
		acct string
		obj  map[string]any
	}{
		{
			name: "wrong object kind",
			acct: acct,
			obj:  map[string]any{"object": "account", "id": "transfers", "account": acct, "status": "active"},
		},
		{
			name: "account mismatch",
			acct: acct,
			obj:  map[string]any{"object": "capability", "id": "transfers", "account": "acct_other", "status": "active"},
		},
		{
			name: "invalid capability id",
			acct: acct,
			obj:  map[string]any{"object": "capability", "id": "transfers/other", "account": acct, "status": "active"},
		},
		{
			name: "invalid status",
			acct: acct,
			obj:  map[string]any{"object": "capability", "id": "transfers", "account": acct, "status": "restricted"},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := parseStripeConnectEvent("evt_connect_capability_"+tc.name,
				stripeConnectEventCapabilityUpdated, tc.acct, 1_700_000_007, tc.obj,
				[]byte(`{"event":"capability.updated"}`))
			if !errors.Is(err, errInvalidStripeConnectEvent) {
				t.Fatalf("parse error=%v, want %v", err, errInvalidStripeConnectEvent)
			}
		})
	}
}

func TestStripeConnectExternalAccountParserBindsAccountAndObject(t *testing.T) {
	acct := "acct_parser_external"
	for _, tc := range []struct {
		name      string
		eventType string
		object    map[string]any
		wantID    string
	}{
		{
			name:      "bank account created",
			eventType: stripeConnectEventExternalAccountCreated,
			object:    map[string]any{"object": "bank_account", "id": "ba_parser_external", "account": acct},
			wantID:    "ba_parser_external",
		},
		{
			name:      "card updated",
			eventType: stripeConnectEventExternalAccountUpdated,
			object:    map[string]any{"object": "card", "id": "card_parser_external", "account": acct},
			wantID:    "card_parser_external",
		},
		{
			name:      "bank account deleted",
			eventType: stripeConnectEventExternalAccountDeleted,
			object:    map[string]any{"object": "bank_account", "id": "ba_parser_external_deleted"},
			wantID:    "ba_parser_external_deleted",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			event, err := parseStripeConnectEvent(
				"evt_external_"+strings.ReplaceAll(tc.name, " ", "_"), tc.eventType, acct,
				1_700_000_008, tc.object, []byte(`{"event":"external-account"}`))
			mustf(t, err, "parse external-account event: %v")
			if event.AccountID != acct || event.ObjectID != tc.wantID || event.PayoutsEnabled != nil {
				t.Fatalf("external-account event=%+v, want account=%s object=%s and no readiness fact", event, acct, tc.wantID)
			}
		})
	}

	for _, tc := range []struct {
		name   string
		object map[string]any
	}{
		{
			name:   "wrong object kind",
			object: map[string]any{"object": "payment_method", "id": "pm_external", "account": acct},
		},
		{
			name:   "wrong object id prefix",
			object: map[string]any{"object": "bank_account", "id": "card_external", "account": acct},
		},
		{
			name:   "wrong account binding",
			object: map[string]any{"object": "bank_account", "id": "ba_external", "account": "acct_other"},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := parseStripeConnectEvent(
				"evt_external_bad_"+strings.ReplaceAll(tc.name, " ", "_"),
				stripeConnectEventExternalAccountUpdated, acct, 1_700_000_009, tc.object,
				[]byte(`{"event":"external-account"}`)); err == nil {
				t.Fatal("accepted an invalid external-account object")
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
		map[string]any{"object": "account", "id": acct, "payouts_enabled": true}, payload)
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
		map[string]any{"object": "account", "id": acct, "payouts_enabled": false}, stalePayload)
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
		map[string]any{"object": "payout", "id": "po_connect_1", "status": "pending"}, payoutPayload)
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

	capabilityPayload := []byte(`{"id":"evt_connect_capability","type":"capability.updated","created":1700000004}`)
	capabilityEvent, err := parseStripeConnectEvent(
		"evt_connect_capability", stripeConnectEventCapabilityUpdated, acct, 1_700_000_004,
		map[string]any{"object": "capability", "id": "transfers", "account": acct, "status": "active"},
		capabilityPayload)
	mustf(t, err, "parse capability event: %v")
	result, err = store.ApplyConnectWebhookEvent(ctx, capabilityEvent)
	mustf(t, err, "apply capability event: %v")
	if !result.Applied || result.Duplicate || result.Stale {
		t.Fatalf("capability result=%+v, want recorded/applied only", result)
	}
	var capabilityStatus string
	mustf(t, pool.QueryRow(ctx, `
		SELECT capability_status FROM stripe_connect_webhook_events WHERE event_id=$1`, capabilityEvent.EventID).
		Scan(&capabilityStatus), "read capability status: %v")
	if capabilityStatus != "active" {
		t.Fatalf("capability status=%q, want active", capabilityStatus)
	}
	result, err = store.ApplyConnectWebhookEvent(ctx, capabilityEvent)
	mustf(t, err, "replay capability event: %v")
	if !result.Duplicate || result.Applied || result.Stale {
		t.Fatalf("capability replay result=%+v, want duplicate only", result)
	}

	externalPayload := []byte(`{"id":"evt_connect_external","type":"account.external_account.updated","created":1700000005}`)
	externalEvent, err := parseStripeConnectEvent(
		"evt_connect_external", stripeConnectEventExternalAccountUpdated, acct, 1_700_000_005,
		map[string]any{"object": "bank_account", "id": "ba_connect_external", "account": acct}, externalPayload)
	mustf(t, err, "parse external-account event: %v")
	result, err = store.ApplyConnectWebhookEvent(ctx, externalEvent)
	mustf(t, err, "apply external-account event: %v")
	if !result.Applied || result.Duplicate || result.Stale {
		t.Fatalf("external-account result=%+v, want recorded/applied only", result)
	}
	var externalType, externalID string
	mustf(t, pool.QueryRow(ctx, `
		SELECT event_type,object_id FROM stripe_connect_webhook_events WHERE event_id=$1`, externalEvent.EventID).
		Scan(&externalType, &externalID), "read external-account event: %v")
	if externalType != stripeConnectEventExternalAccountUpdated || externalID != "ba_connect_external" {
		t.Fatalf("external-account row=(%q,%q), want updated/ba_connect_external", externalType, externalID)
	}
	mustf(t, pool.QueryRow(ctx, `SELECT payouts_enabled FROM suppliers WHERE id=$1`, supplierID).
		Scan(&enabled), "read readiness after external-account event: %v")
	if !enabled {
		t.Fatal("external-account event changed supplier account readiness")
	}

	for i, eventType := range []string{
		stripeConnectEventPayoutUpdated,
		stripeConnectEventPayoutPaid,
		stripeConnectEventPayoutFailed,
		stripeConnectEventPayoutCanceled,
		stripeConnectEventPayoutReconciliationDone,
	} {
		eventID := "evt_connect_payout_lifecycle_" + eventType[strings.LastIndex(eventType, ".")+1:]
		payload := []byte(`{"type":"` + eventType + `"}`)
		lifecycleEvent, err := parseStripeConnectEvent(
			eventID, eventType, acct, int64(1_700_000_010+i),
			map[string]any{"object": "payout", "id": "po_connect_lifecycle_" + eventType[strings.LastIndex(eventType, ".")+1:]}, payload)
		mustf(t, err, "parse %s: %v", eventType)
		result, err = store.ApplyConnectWebhookEvent(ctx, lifecycleEvent)
		mustf(t, err, "apply %s: %v", eventType)
		if !result.Applied || result.Duplicate || result.Stale {
			t.Fatalf("%s result=%+v, want recorded/applied only", eventType, result)
		}
	}
	conflictPayload := []byte(`{"id":"evt_connect_new","type":"account.updated","created":1700000002,"changed":true}`)
	conflictEvent, err := parseStripeConnectEvent(
		"evt_connect_new", stripeConnectEventAccountUpdated, acct, 1_700_000_002,
		map[string]any{"object": "account", "id": acct, "payouts_enabled": false}, conflictPayload)
	mustf(t, err, "parse conflicting event: %v")
	if _, err := store.ApplyConnectWebhookEvent(ctx, conflictEvent); err == nil {
		t.Fatal("same Connect event id with different payload was accepted")
	}

	unknown, err := parseStripeConnectEvent(
		"evt_connect_unknown", stripeConnectEventPayoutPaid, "acct_missing",
		1_700_000_004, map[string]any{"object": "payout", "id": "po_connect_2"}, []byte(`{"x":1}`))
	mustf(t, err, "parse unknown-account event: %v")
	if _, err := store.ApplyConnectWebhookEvent(ctx, unknown); !errors.Is(err, errUnknownConnectAccount) {
		t.Fatalf("unknown account error=%v, want %v", err, errUnknownConnectAccount)
	}

	_, statusAcct, _, err := store.SupplierStatusForBuyer(ctx, buyerID)
	mustf(t, err, "read supplier status: %v")
	if statusAcct != acct {
		t.Fatalf("supplier status account=%q, want %q", statusAcct, acct)
	}
	_, err = pool.Exec(ctx, `UPDATE suppliers SET stripe_acct=$2 WHERE id=$1`, supplierID, "acct_bad&destination=other")
	mustf(t, err, "seed malformed legacy account: %v")
	if _, _, _, err := store.SupplierStatusForBuyer(ctx, buyerID); err == nil {
		t.Fatal("supplier status accepted a malformed stored Stripe account")
	}
}

func TestStripeTransfersCapabilityStatusUsesNewestObservation(t *testing.T) {
	ctx, store, pool := openIsolatedTestStore(t)
	supplierID := uuid.New()
	acct := "acct_capability_order_" + supplierID.String()[:8]
	_, err := pool.Exec(ctx, `
		INSERT INTO suppliers (id,email,stripe_acct,status)
		VALUES ($1,$2,$3,'active')`, supplierID, supplierID.String()+"@capability-order.invalid", acct)
	mustf(t, err, "insert capability supplier: %v")

	for _, event := range []struct {
		id      string
		created int64
		status  string
	}{
		{id: "evt_capability_old", created: 1_700_000_100, status: "inactive"},
		{id: "evt_capability_new", created: 1_700_000_101, status: "active"},
	} {
		parsed, err := parseStripeConnectEvent(event.id, stripeConnectEventCapabilityUpdated, acct, event.created,
			map[string]any{"object": "capability", "id": "transfers", "account": acct, "status": event.status},
			[]byte(`{"type":"capability.updated"}`+event.id))
		mustf(t, err, "parse capability %s: %v", event.id)
		result, err := store.ApplyConnectWebhookEvent(ctx, parsed)
		mustf(t, err, "apply capability %s: %v", event.id)
		if !result.Applied || result.Duplicate || result.Stale {
			t.Fatalf("capability %s result=%+v, want applied", event.id, result)
		}
	}

	status, observed, err := store.stripeTransfersCapabilityStatus(ctx, acct)
	mustf(t, err, "read newest transfers capability: %v")
	if !observed || status != "active" {
		t.Fatalf("transfers capability=(%q,%v), want (active,true)", status, observed)
	}
}
