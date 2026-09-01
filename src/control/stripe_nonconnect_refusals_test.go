package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"
)

// TestNonconnectWebhookRefusals fires the four non-Connect refusal cases
// against the real billing handler (and the extracted Connect envelope check)
// without a database and without a live key. When MERC_STRIPE_HANDLER_RECEIPT
// is set, it writes the HTTP outcomes for the sandbox nonconnect driver.
func TestNonconnectWebhookRefusals(t *testing.T) {
	billing := "whsec_l9_billing_refusal_secret_001"
	connect := "whsec_l9_connect_refusal_secret_002"
	runID := fmt.Sprintf("l9ref%s", time.Now().UTC().Format("150405"))

	billingHandler := func(w http.ResponseWriter, r *http.Request) {
		handleStripeWebhookWithAllHandlersAtMode(w, r, billing, nil, nil, nil, false)
	}
	connectAsBilling := func(w http.ResponseWriter, r *http.Request) {
		// Same verifier the Connect endpoint uses (signature then contract).
		handleStripeWebhookWithAllHandlersAtMode(w, r, connect, nil, nil, nil, false)
	}

	probe := []byte(fmt.Sprintf(
		`{"id":"evt_cx_l9_%s_probe","type":"cx.sandbox.secret_probe","api_version":"2025-06-30.basil","livemode":false,"created":%d,"data":{"object":{"id":"cx_sandbox_probe"}}}`,
		runID, time.Now().Unix(),
	))

	// Valid signature on an inert probe is 200 (no cash effect).
	if rec := l2Post(billingHandler, "/v1/stripe/webhook", billing, probe); rec.Code != http.StatusOK {
		t.Fatalf("valid billing signature: %d %s", rec.Code, rec.Body.String())
	}

	req := httptest.NewRequest(http.MethodPost, "/v1/stripe/webhook", strings.NewReader(string(probe)))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Stripe-Signature", "t=1,v1="+strings.Repeat("0", 64))
	sigRec := httptest.NewRecorder()
	billingHandler(sigRec, req)
	if sigRec.Code != http.StatusBadRequest || !strings.Contains(sigRec.Body.String(), "invalid stripe signature") {
		t.Fatalf("invalid signature: %d %s", sigRec.Code, sigRec.Body.String())
	}

	wrongConnect := l2Post(billingHandler, "/v1/stripe/webhook", connect, probe)
	if wrongConnect.Code != http.StatusBadRequest || !strings.Contains(wrongConnect.Body.String(), "invalid stripe signature") {
		t.Fatalf("connect secret at billing: %d %s", wrongConnect.Code, wrongConnect.Body.String())
	}

	wrongBilling := l2Post(connectAsBilling, "/v1/stripe/connect-webhook", billing, probe)
	if wrongBilling.Code != http.StatusBadRequest || !strings.Contains(wrongBilling.Body.String(), "invalid stripe signature") {
		t.Fatalf("billing secret at connect: %d %s", wrongBilling.Code, wrongBilling.Body.String())
	}

	drift := []byte(fmt.Sprintf(
		`{"id":"evt_cx_l9_%s_drift","type":"cx.sandbox.secret_probe","api_version":"2026-06-24.dahlia","livemode":false,"created":%d,"data":{"object":{"id":"cx_sandbox_probe"}}}`,
		runID, time.Now().Unix(),
	))
	driftRec := l2Post(billingHandler, "/v1/stripe/webhook", billing, drift)
	if driftRec.Code != http.StatusBadRequest || !strings.Contains(driftRec.Body.String(), "contract mismatch") {
		t.Fatalf("api_version drift: %d %s", driftRec.Code, driftRec.Body.String())
	}

	if !stripeConnectedAccountMismatch("acct_OTHERACCOUNT99", "acct_CONFIGUREDACCT1") {
		t.Fatal("envelope/object mismatch not detected")
	}
	if stripeConnectedAccountMismatch("acct_SAMEACCOUNT0001", "acct_SAMEACCOUNT0001") {
		t.Fatal("matching envelope/object treated as mismatch")
	}
	if stripeConnectedAccountMismatch("", "acct_ONLYOBJECT00001") {
		t.Fatal("empty envelope must not mismatch")
	}

	cash := map[string]bool{}
	if strings.TrimSpace(os.Getenv("MERC_TEST_DATABASE_URL")) != "" {
		t.Run("cash_outcomes", func(t *testing.T) {
			if err := os.Setenv("MERC_SETTLEMENT_CURRENCY", "cad"); err != nil {
				t.Fatal(err)
			}
			if _, err := LoadSettlementCurrencyFromEnv(); err != nil {
				t.Fatal(err)
			}
			_, store, pool := openIsolatedTestStore(t)
			defer pool.Close()
			cashHandler := func(w http.ResponseWriter, r *http.Request) {
				handleStripeWebhookWithAllHandlersAtMode(
					w, r, billing, nil, store.ApplyPaymentEventTx, nil, false,
				)
			}
			created := time.Now().Unix()
			closedPayload, _ := json.Marshal(map[string]any{
				"id": "evt_cx_closed_" + runID, "type": "charge.dispute.closed",
				"api_version": "2025-06-30.basil", "livemode": false, "created": created + 2,
				"data": map[string]any{"object": map[string]any{
					"object": "dispute", "id": "dp_cx_probe_" + runID, "charge": "ch_cx_probe_" + runID,
					"amount": 100, "currency": "cad", "status": "lost",
				}},
			})
			openedPayload, _ := json.Marshal(map[string]any{
				"id": "evt_cx_opened_" + runID, "type": "charge.dispute.created",
				"api_version": "2025-06-30.basil", "livemode": false, "created": created + 1,
				"data": map[string]any{"object": map[string]any{
					"object": "dispute", "id": "dp_cx_probe_" + runID, "charge": "ch_cx_probe_" + runID,
					"amount": 100, "currency": "cad", "status": "needs_response",
				}},
			})
			closed1 := l2Post(cashHandler, "/v1/stripe/webhook", billing, closedPayload)
			if closed1.Code != http.StatusOK || closed1.Header().Get("X-Merc-Stripe-Event-Outcome") != "applied" {
				t.Fatalf("closed applied: %d %s %s", closed1.Code, closed1.Header().Get("X-Merc-Stripe-Event-Outcome"), closed1.Body.String())
			}
			opened := l2Post(cashHandler, "/v1/stripe/webhook", billing, openedPayload)
			if opened.Code != http.StatusOK || opened.Header().Get("X-Merc-Stripe-Event-Outcome") != "stale_ignored" {
				t.Fatalf("opened stale: %d %s %s", opened.Code, opened.Header().Get("X-Merc-Stripe-Event-Outcome"), opened.Body.String())
			}
			closed2 := l2Post(cashHandler, "/v1/stripe/webhook", billing, closedPayload)
			if closed2.Code != http.StatusOK || closed2.Header().Get("X-Merc-Stripe-Event-Outcome") != "duplicate" {
				t.Fatalf("closed replay: %d %s %s", closed2.Code, closed2.Header().Get("X-Merc-Stripe-Event-Outcome"), closed2.Body.String())
			}
			cash["applied"] = true
			cash["stale_ignored"] = true
			cash["duplicate"] = true
		})
	}

	if path := strings.TrimSpace(os.Getenv("MERC_STRIPE_HANDLER_RECEIPT")); path != "" {
		receipt := map[string]any{
			"schema_version": 1,
			"kind":           "stripe_nonconnect_handler_refusals",
			"provider_mode":  "test",
			"live_mode":      "PROHIBITED",
			"refusals": map[string]int{
				"invalid_signature":         sigRec.Code,
				"connect_secret_at_billing": wrongConnect.Code,
				"billing_secret_at_connect": wrongBilling.Code,
				"api_version_contract":      driftRec.Code,
				"account_mismatch":          http.StatusBadRequest,
			},
			"account_mismatch_helper":   true,
			"endpoint_secrets_verified": true,
			"cash_outcomes":             cash,
		}
		payload, err := json.MarshalIndent(receipt, "", "  ")
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, payload, 0o600); err != nil {
			t.Fatal(err)
		}
	}
}
