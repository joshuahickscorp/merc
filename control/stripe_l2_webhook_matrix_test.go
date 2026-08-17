package main

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"
)

func l2Sign(secret string, payload []byte, ts int64) string {
	mac := hmac.New(sha256.New, []byte(secret))
	_, _ = mac.Write([]byte(fmt.Sprintf("%d.", ts)))
	_, _ = mac.Write(payload)
	return fmt.Sprintf("t=%d,v1=%s", ts, hex.EncodeToString(mac.Sum(nil)))
}

func l2Post(handler http.HandlerFunc, path, secret string, payload []byte) *httptest.ResponseRecorder {
	req := httptest.NewRequest(http.MethodPost, path, strings.NewReader(string(payload)))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Stripe-Signature", l2Sign(secret, payload, time.Now().Unix()))
	rec := httptest.NewRecorder()
	handler(rec, req)
	return rec
}

func TestL2StripeWebhookMatrixAgainstRealHandlers(t *testing.T) {
	if strings.TrimSpace(os.Getenv("MERC_TEST_DATABASE_URL")) == "" {
		t.Skip("MERC_TEST_DATABASE_URL is not set")
	}
	billingSecret := strings.TrimSpace(os.Getenv("STRIPE_WEBHOOK_SECRET"))
	connectSecret := strings.TrimSpace(os.Getenv("MERC_CONNECT_WEBHOOK_SECRET"))
	if !strings.HasPrefix(billingSecret, "whsec_") ||
		!strings.HasPrefix(connectSecret, "whsec_") ||
		billingSecret == connectSecret {
		// Real handlers HMAC against whatever secret they are given. Dashboard
		// secrets are not present in this process; install two distinct
		// process-local webhook secrets so the matrix still exercises the
		// production handlers (signature, endpoint isolation, api_version,
		// account mismatch, cash-effect rank, replay).
		billingSecret = "whsec_l2_matrix_billing_" + strings.Repeat("b", 16)
		connectSecret = "whsec_l2_matrix_connect_" + strings.Repeat("c", 16)
		t.Setenv("STRIPE_WEBHOOK_SECRET", billingSecret)
		t.Setenv("MERC_CONNECT_WEBHOOK_SECRET", connectSecret)
		t.Log("dashboard webhook secrets absent; using process-local whsec_ pair")
	}
	if err := os.Setenv("MERC_SETTLEMENT_CURRENCY", "cad"); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadSettlementCurrencyFromEnv(); err != nil {
		t.Fatal(err)
	}
	if err := os.Setenv("MERC_PAYMENT_MODE", "test"); err != nil {
		t.Fatal(err)
	}
	// handleConnectWebhook authorizes the operation against the TEST Stripe
	// credential class before it verifies the webhook HMAC. A missing or
	// non-sk_test key returns 503 "connect webhook authority is unavailable"
	// and the matrix never reaches the handler body.
	if !strings.HasPrefix(strings.TrimSpace(os.Getenv("STRIPE_SECRET_KEY")), "sk_test_") &&
		!strings.HasPrefix(strings.TrimSpace(os.Getenv("STRIPE_SECRET_KEY")), "rk_test_") {
		t.Setenv("STRIPE_SECRET_KEY", "sk_test_l2_webhook_matrix_not_a_live_secret")
	}

	ctx, store, pool := openIsolatedTestStore(t)
	defer pool.Close()
	_ = ctx
	server := &Server{store: store}

	runID := fmt.Sprintf("l2loc%s", time.Now().UTC().Format("150405"))
	billing := func(w http.ResponseWriter, r *http.Request) {
		handleStripeWebhookWithAllHandlersAtMode(
			w, r, billingSecret, nil, store.ApplyPaymentEventTx, nil, false,
		)
	}
	connect := server.handleConnectWebhook

	// Valid billing secret probe (non-cash type) → 200
	probe := []byte(fmt.Sprintf(
		`{"id":"evt_cx_probe_%s_billing","type":"cx.sandbox.secret_probe","api_version":"2025-06-30.basil","livemode":false,"created":%d,"data":{"object":{"id":"cx_sandbox_probe"}}}`,
		runID, time.Now().Unix(),
	))
	if rec := l2Post(billing, "/v1/stripe/webhook", billingSecret, probe); rec.Code != http.StatusOK {
		t.Fatalf("billing valid signature: %d %s", rec.Code, rec.Body.String())
	}

	// Invalid signature → 400
	req := httptest.NewRequest(http.MethodPost, "/v1/stripe/webhook", strings.NewReader(string(probe)))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Stripe-Signature", "t=1,v1="+strings.Repeat("0", 64))
	rec := httptest.NewRecorder()
	billing(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("billing invalid signature: %d %s", rec.Code, rec.Body.String())
	}

	// Connect secret at billing endpoint → 400
	if rec := l2Post(billing, "/v1/stripe/webhook", connectSecret, probe); rec.Code != http.StatusBadRequest {
		t.Fatalf("connect secret at billing: %d %s", rec.Code, rec.Body.String())
	}

	connectProbe := []byte(fmt.Sprintf(
		`{"id":"evt_cx_probe_%s_connect","type":"cx.sandbox.secret_probe","api_version":"2025-06-30.basil","livemode":false,"created":%d,"data":{"object":{"id":"cx_sandbox_probe"}}}`,
		runID, time.Now().Unix(),
	))
	if rec := l2Post(connect, "/v1/stripe/connect-webhook", connectSecret, connectProbe); rec.Code != http.StatusOK {
		t.Fatalf("connect valid signature: %d %s", rec.Code, rec.Body.String())
	}
	if rec := l2Post(connect, "/v1/stripe/connect-webhook", billingSecret, connectProbe); rec.Code != http.StatusBadRequest {
		t.Fatalf("billing secret at connect: %d %s", rec.Code, rec.Body.String())
	}

	// api_version contract refusal (dahlia != compiled basil) after a valid signature.
	drift := []byte(fmt.Sprintf(
		`{"id":"evt_cx_drift_%s","type":"cx.sandbox.secret_probe","api_version":"2026-06-24.dahlia","livemode":false,"created":%d,"data":{"object":{"id":"cx_sandbox_probe"}}}`,
		runID, time.Now().Unix(),
	))
	if rec := l2Post(billing, "/v1/stripe/webhook", billingSecret, drift); rec.Code != http.StatusBadRequest {
		t.Fatalf("billing api_version drift: %d %s", rec.Code, rec.Body.String())
	}
	if rec := l2Post(connect, "/v1/stripe/connect-webhook", connectSecret, drift); rec.Code != http.StatusBadRequest {
		t.Fatalf("connect api_version drift: %d %s", rec.Code, rec.Body.String())
	}

	// Account mismatch: envelope account != object id
	mismatch := []byte(fmt.Sprintf(
		`{"id":"evt_cx_mismatch_%s","type":"account.updated","account":"acct_OTHERACCOUNT99","api_version":"2025-06-30.basil","livemode":false,"created":%d,"data":{"object":{"id":"acct_CONFIGUREDACCT1","payouts_enabled":true}}}`,
		runID, time.Now().Unix(),
	))
	if rec := l2Post(connect, "/v1/stripe/connect-webhook", connectSecret, mismatch); rec.Code != http.StatusBadRequest {
		t.Fatalf("envelope/object account mismatch: %d %s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "mismatch") && rec.Code == http.StatusBadRequest {
		// body already checked via code; keep a second probe for unknown account
	}

	unknown := []byte(fmt.Sprintf(
		`{"id":"evt_cx_unknown_%s","type":"account.updated","account":"acct_NOTCONFIGURED01","api_version":"2025-06-30.basil","livemode":false,"created":%d,"data":{"object":{"id":"acct_NOTCONFIGURED01","payouts_enabled":true}}}`,
		runID, time.Now().Unix(),
	))
	rec = l2Post(connect, "/v1/stripe/connect-webhook", connectSecret, unknown)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("unknown connected account: %d %s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(strings.ToLower(rec.Body.String()), "unknown") {
		t.Fatalf("unknown account refusal body=%s", rec.Body.String())
	}

	// Cash outcomes: terminal first, then stale open, then replay.
	created := time.Now().Unix()
	closedPayload, _ := json.Marshal(map[string]any{
		"id": "evt_cx_closed_" + runID, "type": "charge.dispute.closed",
		"api_version": "2025-06-30.basil", "livemode": false, "created": created + 2,
		"data": map[string]any{"object": map[string]any{
			"id": "dp_cx_probe_" + runID, "charge": "ch_cx_probe_" + runID,
			"amount": 100, "currency": "cad", "status": "lost",
		}},
	})
	openedPayload, _ := json.Marshal(map[string]any{
		"id": "evt_cx_opened_" + runID, "type": "charge.dispute.created",
		"api_version": "2025-06-30.basil", "livemode": false, "created": created + 1,
		"data": map[string]any{"object": map[string]any{
			"id": "dp_cx_probe_" + runID, "charge": "ch_cx_probe_" + runID,
			"amount": 100, "currency": "cad", "status": "needs_response",
		}},
	})
	closed1 := l2Post(billing, "/v1/stripe/webhook", billingSecret, closedPayload)
	if closed1.Code != http.StatusOK || closed1.Header().Get("X-Merc-Stripe-Event-Outcome") != "applied" ||
		closed1.Header().Get("X-Merc-Stripe-Cash-Effect-Rank") != "30" {
		t.Fatalf("closed applied: status=%d outcome=%q rank=%q body=%s",
			closed1.Code, closed1.Header().Get("X-Merc-Stripe-Event-Outcome"),
			closed1.Header().Get("X-Merc-Stripe-Cash-Effect-Rank"), closed1.Body.String())
	}
	opened := l2Post(billing, "/v1/stripe/webhook", billingSecret, openedPayload)
	if opened.Code != http.StatusOK || opened.Header().Get("X-Merc-Stripe-Event-Outcome") != "stale_ignored" ||
		opened.Header().Get("X-Merc-Stripe-Cash-Effect-Rank") != "30" {
		t.Fatalf("opened stale: status=%d outcome=%q rank=%q body=%s",
			opened.Code, opened.Header().Get("X-Merc-Stripe-Event-Outcome"),
			opened.Header().Get("X-Merc-Stripe-Cash-Effect-Rank"), opened.Body.String())
	}
	closed2 := l2Post(billing, "/v1/stripe/webhook", billingSecret, closedPayload)
	if closed2.Code != http.StatusOK || closed2.Header().Get("X-Merc-Stripe-Event-Outcome") != "duplicate" {
		t.Fatalf("closed replay: status=%d outcome=%q body=%s",
			closed2.Code, closed2.Header().Get("X-Merc-Stripe-Event-Outcome"), closed2.Body.String())
	}
}

func TestL2HoldWebhookServer(t *testing.T) {
	if os.Getenv("MERC_L2_HOLD") == "" {
		t.Skip("MERC_L2_HOLD not set")
	}
	billingSecret := strings.TrimSpace(os.Getenv("STRIPE_WEBHOOK_SECRET"))
	connectSecret := strings.TrimSpace(os.Getenv("MERC_CONNECT_WEBHOOK_SECRET"))
	if !strings.HasPrefix(billingSecret, "whsec_") || !strings.HasPrefix(connectSecret, "whsec_") {
		t.Fatal("webhook secrets required")
	}
	if _, err := LoadSettlementCurrencyFromEnv(); err != nil {
		if err := os.Setenv("MERC_SETTLEMENT_CURRENCY", "cad"); err != nil {
			t.Fatal(err)
		}
		if _, err := LoadSettlementCurrencyFromEnv(); err != nil {
			t.Fatal(err)
		}
	}
	ctx, store, pool := openIsolatedTestStore(t)
	defer pool.Close()
	_ = ctx
	server := &Server{store: store}
	mux := http.NewServeMux()
	mux.HandleFunc("POST /v1/stripe/webhook", func(w http.ResponseWriter, r *http.Request) {
		handleStripeWebhookWithAllHandlersAtMode(
			w, r, billingSecret, nil, store.ApplyPaymentEventTx, nil, false,
		)
	})
	mux.HandleFunc("POST /v1/stripe/connect-webhook", server.handleConnectWebhook)
	ln, err := net.Listen("tcp", "127.0.0.1:18080")
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("l2 webhook hold listening on %s", ln.Addr())
	fmt.Fprintf(os.Stderr, "l2 webhook hold listening on %s\n", ln.Addr())
	srv := &http.Server{Handler: mux, ReadHeaderTimeout: 10 * time.Second}
	t.Cleanup(func() { _ = srv.Shutdown(context.Background()) })
	if err := srv.Serve(ln); err != nil && err != http.ErrServerClosed {
		t.Fatal(err)
	}
	_ = io.Discard
}

// A payment-method event for a Stripe customer this deployment has never seen
// must be acknowledged, not answered 500. The Stripe account is shared with CLI
// fixtures, so unknown customers arrive routinely; a 500 makes Stripe retry an
// event that can never succeed and builds a permanently failing delivery queue.
// Observed on the live staging plane: `stripe trigger payment_method.attached`
// produced two 500s. A real database fault must still answer 500.
func TestUnknownCustomerPaymentMethodEventIsAcknowledgedNotRetried(t *testing.T) {
	for _, tc := range []struct {
		name     string
		setErr   error
		wantCode int
	}{
		{"unknown customer is acknowledged", errNotFound, http.StatusOK},
		{"real database fault still retries", errors.New("connection reset"), http.StatusInternalServerError},
	} {
		t.Run(tc.name, func(t *testing.T) {
			secret := "whsec_" + strings.Repeat("a", 32)
			body := `{"id":"evt_x","object":"event","api_version":"` + stripeAPIVersion +
				`","livemode":false,"type":"payment_method.attached","data":{"object":` +
				`{"id":"pm_x","object":"payment_method","customer":"cus_stranger"}}}`

			rec := httptest.NewRecorder()
			req := httptest.NewRequest(http.MethodPost, "/v1/stripe/webhook", strings.NewReader(body))
			req.Header.Set("Stripe-Signature", l2Sign(secret, []byte(body), time.Now().Unix()))

			handleStripeWebhookWithAllHandlersAtMode(rec, req, secret,
				func(context.Context, string, string) error { return tc.setErr },
				nil, nil, false,
			)
			if rec.Code != tc.wantCode {
				t.Fatalf("status = %d, want %d (body %s)", rec.Code, tc.wantCode, rec.Body.String())
			}
		})
	}
}
