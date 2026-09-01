package main

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"
)

func TestStripePaymentFailureWebhookIsRecordedReplaySafeAndNonCash(t *testing.T) {
	ctx, store, pool := openIsolatedTestStore(t)
	secret := "whsec_payment_failures_" + strings.Repeat("f", 24)
	handler := func(w http.ResponseWriter, r *http.Request) {
		handleStripeWebhookWithAllHandlersAtModeAndRiskAndPaymentFailure(
			w, r, secret, nil, nil, nil, false, nil, store.ApplyStripePaymentFailureEvent,
		)
	}

	payload, err := json.Marshal(map[string]any{
		"id":          "evt_payment_failed",
		"type":        stripePaymentIntentFailedEvent,
		"api_version": stripeAPIVersion,
		"livemode":    false,
		"created":     int64(1_700_000_200),
		"data": map[string]any{"object": map[string]any{
			"object": "payment_intent", "id": "pi_payment_failed", "status": "requires_payment_method",
			"customer": "cus_payment_failed",
			"metadata": map[string]any{"cx_operation_key": "job-payment-failed"},
			"last_payment_error": map[string]any{
				"type": "card_error", "code": "card_declined", "decline_code": "do_not_honor",
			},
		}},
	})
	must(t, err)
	rec := l2Post(handler, "/v1/stripe/webhook", secret, payload)
	if rec.Code != http.StatusOK || rec.Header().Get("X-Merc-Stripe-Event-Outcome") != "recorded" {
		t.Fatalf("payment failure status=%d outcome=%q body=%s", rec.Code,
			rec.Header().Get("X-Merc-Stripe-Event-Outcome"), rec.Body.String())
	}
	rec = l2Post(handler, "/v1/stripe/webhook", secret, payload)
	if rec.Code != http.StatusOK || rec.Header().Get("X-Merc-Stripe-Event-Outcome") != "duplicate" {
		t.Fatalf("payment failure replay status=%d outcome=%q body=%s", rec.Code,
			rec.Header().Get("X-Merc-Stripe-Event-Outcome"), rec.Body.String())
	}

	var failureRows, cashRows, disputeRows int
	mustf(t, pool.QueryRow(ctx, `SELECT count(*) FROM stripe_payment_failure_events`).Scan(&failureRows),
		"count payment failures: %v")
	mustf(t, pool.QueryRow(ctx, `SELECT count(*) FROM stripe_charge_cash_state`).Scan(&cashRows),
		"count cash states: %v")
	mustf(t, pool.QueryRow(ctx, `SELECT count(*) FROM stripe_dispute_cash_state`).Scan(&disputeRows),
		"count dispute states: %v")
	if failureRows != 1 || cashRows != 0 || disputeRows != 0 {
		t.Fatalf("failure/cash/dispute rows=%d/%d/%d, want 1/0/0", failureRows, cashRows, disputeRows)
	}
	rows, err := store.ListStripePaymentFailureEvents(ctx, 10)
	mustf(t, err, "list payment failures: %v")
	if len(rows) != 1 || rows[0].PaymentIntent != "pi_payment_failed" ||
		rows[0].OperationKey != "job-payment-failed" || rows[0].DeclineCode != "do_not_honor" {
		t.Fatalf("payment failure list=%+v, want normalized provider fact", rows)
	}
}

func TestStripePaymentFailureParserRejectsContradictorySuccess(t *testing.T) {
	_, err := parseStripePaymentFailureEvent(
		"evt_contradictory_failure", 1_700_000_201,
		map[string]any{"object": "payment_intent", "id": "pi_contradictory", "status": "succeeded"}, []byte("payload"),
	)
	if err == nil {
		t.Fatal("accepted payment_intent.payment_failed with status=succeeded")
	}
}

func TestStripePaymentFailureParserRejectsWrongObjectKind(t *testing.T) {
	_, err := parseStripePaymentFailureEvent(
		"evt_wrong_failure_object", 1_700_000_202,
		map[string]any{"object": "charge", "id": "pi_wrong_failure_object", "status": "requires_payment_method"},
		[]byte("payload"),
	)
	if err == nil {
		t.Fatal("accepted a non-payment_intent Stripe object")
	}
}
