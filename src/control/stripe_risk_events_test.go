package main

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"
)

func TestStripeRiskWebhookIsRecordedReplaySafeAndNonCash(t *testing.T) {
	ctx, store, pool := openIsolatedTestStore(t)
	buyerID := insertTestBuyer(t, pool, ctx)
	_, err := pool.Exec(ctx, `
		INSERT INTO buyer_cash_collections
		  (payment_intent,charge_id,buyer_id,source_kind,requested_cents,received_cents,currency)
		VALUES ('pi_risk_1','ch_risk_1',$1,'topup',500,500,'usd')`, buyerID)
	mustf(t, err, "seed risk cash collection: %v")
	secret := "whsec_risk_events_" + strings.Repeat("r", 21)
	handler := func(w http.ResponseWriter, r *http.Request) {
		handleStripeWebhookWithAllHandlersAtModeAndRisk(
			w, r, secret, nil, nil, nil, false, store.ApplyStripeRiskEvent,
		)
	}

	payload, err := json.Marshal(map[string]any{
		"id":          "evt_risk_created",
		"type":        "radar.early_fraud_warning.created",
		"api_version": stripeAPIVersion,
		"livemode":    false,
		"created":     int64(1_700_000_100),
		"data": map[string]any{"object": map[string]any{
			"object": "radar.early_fraud_warning", "id": "issfr_risk_1", "charge": "ch_risk_1", "payment_intent": "pi_risk_1",
			"fraud_type": "made_with_stolen_card", "actionable": true,
		}},
	})
	must(t, err)
	rec := l2Post(handler, "/v1/stripe/webhook", secret, payload)
	if rec.Code != http.StatusOK || rec.Header().Get("X-Merc-Stripe-Event-Outcome") != "recorded" {
		t.Fatalf("risk event status=%d outcome=%q body=%s", rec.Code,
			rec.Header().Get("X-Merc-Stripe-Event-Outcome"), rec.Body.String())
	}
	rec = l2Post(handler, "/v1/stripe/webhook", secret, payload)
	if rec.Code != http.StatusOK || rec.Header().Get("X-Merc-Stripe-Event-Outcome") != "duplicate" {
		t.Fatalf("risk replay status=%d outcome=%q body=%s", rec.Code,
			rec.Header().Get("X-Merc-Stripe-Event-Outcome"), rec.Body.String())
	}

	updatedPayload, err := json.Marshal(map[string]any{
		"id":          "evt_risk_updated",
		"type":        "radar.early_fraud_warning.updated",
		"api_version": stripeAPIVersion,
		"livemode":    false,
		"created":     int64(1_700_000_101),
		"data": map[string]any{"object": map[string]any{
			"object": "radar.early_fraud_warning", "id": "issfr_risk_1", "charge": "ch_risk_1",
			"fraud_type": "made_with_stolen_card", "actionable": false,
		}},
	})
	must(t, err)
	rec = l2Post(handler, "/v1/stripe/webhook", secret, updatedPayload)
	if rec.Code != http.StatusOK || rec.Header().Get("X-Merc-Stripe-Event-Outcome") != "recorded" {
		t.Fatalf("risk update status=%d outcome=%q body=%s", rec.Code,
			rec.Header().Get("X-Merc-Stripe-Event-Outcome"), rec.Body.String())
	}

	var riskRows, cashRows, disputeRows int
	mustf(t, pool.QueryRow(ctx, `SELECT count(*) FROM stripe_risk_events`).Scan(&riskRows),
		"count risk events: %v")
	mustf(t, pool.QueryRow(ctx, `SELECT count(*) FROM stripe_charge_cash_state`).Scan(&cashRows),
		"count cash states: %v")
	mustf(t, pool.QueryRow(ctx, `SELECT count(*) FROM stripe_dispute_cash_state`).Scan(&disputeRows),
		"count dispute states: %v")
	if riskRows != 2 || cashRows != 0 || disputeRows != 0 {
		t.Fatalf("risk/cash/dispute rows=%d/%d/%d, want 2/0/0", riskRows, cashRows, disputeRows)
	}
	rows, err := store.ListStripeRiskEvents(ctx, 10)
	mustf(t, err, "list risk events: %v")
	if len(rows) != 2 || rows[0].EventType != stripeRiskEventEarlyFraudWarningUpdated || rows[0].Actionable {
		t.Fatalf("risk list=%+v, want newest non-actionable update first", rows)
	}
	if rows[0].Collection == nil || rows[0].Collection.PaymentIntent != "pi_risk_1" ||
		rows[0].Collection.BuyerID != buyerID || rows[0].Collection.SourceKind != "topup" ||
		rows[0].Collection.ReceivedCents != 500 || rows[0].Collection.Currency != "usd" {
		t.Fatalf("risk collection linkage=%+v, want the recorded cash collection", rows[0].Collection)
	}
}

func TestStripeRiskWebhookRejectsIncompleteWarning(t *testing.T) {
	secret := "whsec_risk_incomplete_" + strings.Repeat("r", 28)
	payload := []byte(`{"id":"evt_risk_incomplete","type":"radar.early_fraud_warning.created","api_version":"` + stripeAPIVersion + `","livemode":false,"created":1700000100,"data":{"object":{"object":"radar.early_fraud_warning","id":"issfr_incomplete","charge":"ch_incomplete","fraud_type":"misc"}}}`)
	rec := l2Post(func(w http.ResponseWriter, r *http.Request) {
		handleStripeWebhookWithAllHandlersAtModeAndRisk(w, r, secret, nil, nil, nil, false,
			func(_ context.Context, _ stripeRiskEvent) (stripeRiskEventResult, error) {
				return stripeRiskEventResult{}, nil
			})
	}, "/v1/stripe/webhook", secret, payload)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("incomplete risk event status=%d body=%s, want 400", rec.Code, rec.Body.String())
	}
}

func TestStripeRiskEventRejectsConflictingCashIdentity(t *testing.T) {
	ctx, store, pool := openIsolatedTestStore(t)
	buyerID := insertTestBuyer(t, pool, ctx)
	if _, err := pool.Exec(ctx, `
		INSERT INTO buyer_cash_collections
		  (payment_intent,charge_id,buyer_id,source_kind,requested_cents,received_cents,currency)
		VALUES ('pi_risk_bound','ch_risk_bound',$1,'topup',500,500,'usd')`, buyerID); err != nil {
		t.Fatal(err)
	}
	event, err := parseStripeRiskEvent(
		"evt_risk_conflict", stripeRiskEventEarlyFraudWarningCreated, 1_700_000_400,
		map[string]any{
			"object": "radar.early_fraud_warning", "id": "issfr_risk_conflict",
			"charge": "ch_risk_bound", "payment_intent": "pi_risk_other",
			"fraud_type": "made_with_stolen_card", "actionable": true,
		}, []byte(`{"signed":"risk-conflict"}`),
	)
	must(t, err)
	if _, err := store.ApplyStripeRiskEvent(ctx, event); err == nil {
		t.Fatal("accepted fraud evidence bound to a different PaymentIntent")
	}
	var rows int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM stripe_risk_events`).Scan(&rows); err != nil {
		t.Fatal(err)
	}
	if rows != 0 {
		t.Fatalf("conflicting risk event rows=%d, want 0", rows)
	}
}

func TestStripeRiskEventRejectsWarningIdentityChange(t *testing.T) {
	ctx, store, pool := openIsolatedTestStore(t)
	first, err := parseStripeRiskEvent(
		"evt_risk_identity_1", stripeRiskEventEarlyFraudWarningCreated, 1_700_000_500,
		map[string]any{
			"object": "radar.early_fraud_warning", "id": "issfr_risk_identity",
			"charge": "ch_risk_identity", "fraud_type": "made_with_stolen_card", "actionable": true,
		}, []byte(`{"signed":"risk-identity-1"}`),
	)
	must(t, err)
	if _, err := store.ApplyStripeRiskEvent(ctx, first); err != nil {
		t.Fatalf("recorded first fraud warning evidence: %v", err)
	}
	second, err := parseStripeRiskEvent(
		"evt_risk_identity_2", stripeRiskEventEarlyFraudWarningUpdated, 1_700_000_501,
		map[string]any{
			"object": "radar.early_fraud_warning", "id": "issfr_risk_identity",
			"charge": "ch_risk_other", "fraud_type": "made_with_stolen_card", "actionable": false,
		}, []byte(`{"signed":"risk-identity-2"}`),
	)
	must(t, err)
	if _, err := store.ApplyStripeRiskEvent(ctx, second); err == nil {
		t.Fatal("accepted fraud warning evidence rewritten to a different charge")
	}
	var rows int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM stripe_risk_events`).Scan(&rows); err != nil {
		t.Fatal(err)
	}
	if rows != 1 {
		t.Fatalf("fraud warning rows=%d, want only the original evidence", rows)
	}
}

func TestStripeRiskParserRejectsWrongWarningObjectKind(t *testing.T) {
	for _, objectKind := range []string{"charge", "early_fraud_warning"} {
		t.Run(objectKind, func(t *testing.T) {
			_, err := parseStripeRiskEvent(
				"evt_risk_wrong_object_"+strings.ReplaceAll(objectKind, "_", "-"),
				stripeRiskEventEarlyFraudWarningCreated, 1_700_000_200,
				map[string]any{
					"object": objectKind, "id": "issfr_wrong_kind", "charge": "ch_risk_wrong", "fraud_type": "misc", "actionable": false,
				}, []byte(`{"signed":"risk"}`),
			)
			if err == nil {
				t.Fatalf("accepted invalid early-fraud-warning Stripe object kind %q", objectKind)
			}
		})
	}
}

func TestStripeRiskParserRejectsWrongExpandedReferenceKinds(t *testing.T) {
	for _, tc := range []struct {
		name string
		set  func(map[string]any)
	}{
		{
			name: "charge field",
			set: func(object map[string]any) {
				object["charge"] = map[string]any{"object": "payment_intent", "id": "ch_wrong_kind"}
			},
		},
		{
			name: "payment intent field",
			set: func(object map[string]any) {
				object["payment_intent"] = map[string]any{"object": "charge", "id": "pi_wrong_kind"}
			},
		},
		{
			name: "missing expanded object kind",
			set: func(object map[string]any) {
				object["charge"] = map[string]any{"id": "ch_missing_kind"}
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			object := map[string]any{
				"object":         "radar.early_fraud_warning",
				"id":             "issfr_expanded_kind",
				"charge":         "ch_risk_expanded",
				"payment_intent": "pi_risk_expanded",
				"fraud_type":     "misc",
				"actionable":     false,
			}
			tc.set(object)
			if _, err := parseStripeRiskEvent(
				"evt_risk_expanded_"+strings.ReplaceAll(tc.name, " ", "_"),
				stripeRiskEventEarlyFraudWarningCreated, 1_700_000_300, object, []byte(`{"signed":"risk"}`),
			); err == nil {
				t.Fatal("accepted a wrong expanded Stripe object kind")
			}
		})
	}
}
