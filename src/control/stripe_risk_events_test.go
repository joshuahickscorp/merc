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
			"id": "issfr_risk_1", "charge": "ch_risk_1", "payment_intent": "pi_risk_1",
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
			"id": "issfr_risk_1", "charge": "ch_risk_1",
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
}

func TestStripeRiskWebhookRejectsIncompleteWarning(t *testing.T) {
	secret := "whsec_risk_incomplete_" + strings.Repeat("r", 28)
	payload := []byte(`{"id":"evt_risk_incomplete","type":"radar.early_fraud_warning.created","api_version":"` + stripeAPIVersion + `","livemode":false,"created":1700000100,"data":{"object":{"id":"issfr_incomplete","charge":"ch_incomplete","fraud_type":"misc"}}}`)
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
