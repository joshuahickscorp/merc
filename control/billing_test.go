package main

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

func withStripeTestServer(t *testing.T, handler http.Handler) *httptest.Server {
	t.Helper()
	ts := httptest.NewServer(handler)
	oldURL, oldClient := stripeAPIBaseURL, stripeHTTPClient
	stripeAPIBaseURL, stripeHTTPClient = ts.URL, ts.Client()
	t.Setenv("STRIPE_SECRET_KEY", "sk_test_cash_boundary")
	t.Cleanup(func() {
		stripeAPIBaseURL, stripeHTTPClient = oldURL, oldClient
		ts.Close()
	})
	return ts
}

func TestChargePaymentIntentRequiresExactTerminalCashFact(t *testing.T) {
	const good = `{"id":"pi_exact","latest_charge":{"id":"ch_exact"},"status":"succeeded","currency":"usd","amount":123,"amount_received":123}`
	cases := []struct {
		name string
		body string
	}{
		{"requires action", `{"id":"pi_action","status":"requires_action","currency":"usd","amount":123,"amount_received":0}`},
		{"still processing", `{"id":"pi_processing","status":"processing","currency":"usd","amount":123,"amount_received":0}`},
		{"missing id", `{"status":"succeeded","currency":"usd","amount":123,"amount_received":123}`},
		{"wrong currency", `{"id":"pi_currency","status":"succeeded","currency":"cad","amount":123,"amount_received":123}`},
		{"request amount mismatch", `{"id":"pi_amount","status":"succeeded","currency":"usd","amount":122,"amount_received":123}`},
		{"received amount mismatch", `{"id":"pi_received","status":"succeeded","currency":"usd","amount":123,"amount_received":122}`},
		{"fractional minor units", `{"id":"pi_fraction","status":"succeeded","currency":"usd","amount":123,"amount_received":122.5}`},
		{"negative minor units", `{"id":"pi_negative","status":"succeeded","currency":"usd","amount":123,"amount_received":-1}`},
		{"missing received amount", `{"id":"pi_missing","status":"succeeded","currency":"usd","amount":123}`},
		{"error shaped 2xx", `{"error":{"type":"card_error","message":"declined"}}`},
		{"malformed JSON", `{"id":`},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			withStripeTestServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				_, _ = w.Write([]byte(tc.body))
			}))
			if _, err := chargePaymentIntent(context.Background(), "cus_test", "pm_test", 123, "usd", "job-stable"); err == nil {
				t.Fatalf("accepted a non-cash 2xx response: %s", tc.body)
			}
		})
	}

	t.Run("exact success", func(t *testing.T) {
		withStripeTestServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.Method != http.MethodPost || r.URL.Path != "/payment_intents" {
				t.Errorf("request = %s %s, want POST /payment_intents", r.Method, r.URL.Path)
			}
			if got := r.Header.Get("Idempotency-Key"); got != "job-stable" {
				t.Errorf("Idempotency-Key = %q, want job-stable", got)
			}
			if err := r.ParseForm(); err != nil {
				t.Errorf("ParseForm: %v", err)
			}
			for key, want := range map[string]string{
				"amount": "123", "currency": "usd", "customer": "cus_test",
				"payment_method": "pm_test", "confirm": "true", "off_session": "true",
				"expand[]": "latest_charge", "metadata[cx_operation_key]": "job-stable",
			} {
				if got := r.Form.Get(key); got != want {
					t.Errorf("form[%s] = %q, want %q", key, got, want)
				}
			}
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(good))
		}))
		got, err := chargePaymentIntent(context.Background(), "cus_test", "pm_test", 123, "usd", "job-stable")
		if err != nil {
			t.Fatalf("chargePaymentIntent: %v", err)
		}
		want := (ChargeResult{PaymentIntentID: "pi_exact", ChargeID: "ch_exact", RequestedCents: 123, ReceivedCents: 123, Currency: "usd"})
		if got != want {
			t.Fatalf("result = %+v, want %+v", got, want)
		}
	})
}

func TestChargePaymentIntentResponseLossKeepsIdempotencyKey(t *testing.T) {
	var (
		mu   sync.Mutex
		keys []string
		n    int
	)
	withStripeTestServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		keys = append(keys, r.Header.Get("Idempotency-Key"))
		n++
		attempt := n
		mu.Unlock()
		if attempt == 1 {
			hj, ok := w.(http.Hijacker)
			if !ok {
				t.Error("httptest response writer cannot hijack connection")
				return
			}
			conn, _, err := hj.Hijack()
			if err != nil {
				t.Errorf("Hijack: %v", err)
				return
			}
			_ = conn.Close() // ambiguous response loss after the request arrived
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = fmt.Fprint(w, `{"id":"pi_replayed","latest_charge":"ch_replayed","status":"succeeded","currency":"usd","amount":321,"amount_received":321}`)
	}))

	const key = "job-response-loss"
	if _, err := chargePaymentIntent(context.Background(), "cus_test", "pm_test", 321, "usd", key); err == nil {
		t.Fatal("lost first response must surface as an ambiguous error")
	}
	got, err := chargePaymentIntent(context.Background(), "cus_test", "pm_test", 321, "usd", key)
	if err != nil {
		t.Fatalf("idempotent retry: %v", err)
	}
	if got.PaymentIntentID != "pi_replayed" || got.ReceivedCents != 321 {
		t.Fatalf("retry result = %+v", got)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(keys) != 2 || keys[0] != key || keys[1] != key {
		t.Fatalf("request idempotency keys = %q, want [%q %q]", keys, key, key)
	}
}

func TestVerifyStripeSig(t *testing.T) {
	secret, payload, ts := "whsec_test", []byte(`{"type":"setup_intent.succeeded"}`), "1700000000"
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write([]byte(ts + "." + string(payload)))
	good := "t=" + ts + ",v1=" + hex.EncodeToString(mac.Sum(nil))
	tsTime := time.Unix(1700000000, 0)

	if !verifyStripeSigAt(payload, good, secret, tsTime) {
		t.Fatal("valid signature at its own timestamp was rejected")
	}
	if verifyStripeSigAt(payload, "t="+ts+",v1=deadbeef", secret, tsTime) {
		t.Fatal("forged signature was accepted")
	}
	if verifyStripeSigAt(payload, good, "wrong-secret", tsTime) {
		t.Fatal("wrong secret was accepted")
	}
	if verifyStripeSigAt(payload, "", secret, tsTime) {
		t.Fatal("empty signature header was accepted")
	}
	rotating := "t=" + ts + ",v1=deadbeef,v1=" + hex.EncodeToString(mac.Sum(nil))
	if !verifyStripeSigAt(payload, rotating, secret, tsTime) {
		t.Fatal("valid secondary v1 signature during endpoint-secret rotation was rejected")
	}
}

func TestVerifyStripeSigRejectsReplayOutsideTolerance(t *testing.T) {
	secret, payload, ts := "whsec_test", []byte(`{"type":"setup_intent.succeeded"}`), "1700000000"
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write([]byte(ts + "." + string(payload)))
	sig := "t=" + ts + ",v1=" + hex.EncodeToString(mac.Sum(nil))
	tsTime := time.Unix(1700000000, 0)

	if !verifyStripeSigAt(payload, sig, secret, tsTime.Add(4*time.Minute)) {
		t.Fatal("a signature 4 minutes old (within the 5-minute tolerance) was rejected")
	}
	if verifyStripeSigAt(payload, sig, secret, tsTime.Add(10*time.Minute)) {
		t.Fatal("a signature replayed 10 minutes later (past tolerance) was accepted")
	}
	if verifyStripeSigAt(payload, sig, secret, tsTime.Add(-10*time.Minute)) {
		t.Fatal("a signature claiming to be 10 minutes in the FUTURE was accepted")
	}
}

func TestStripeWebhookRetriesSavedCardDatabaseFailures(t *testing.T) {
	const secret = "whsec_webhook_test"
	payload := []byte(`{"type":"setup_intent.succeeded","data":{"object":{"customer":"cus_known","payment_method":"pm_new"}}}`)
	now := time.Now()
	ts := fmt.Sprint(now.Unix())
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write([]byte(ts + "." + string(payload)))
	sig := "t=" + ts + ",v1=" + hex.EncodeToString(mac.Sum(nil))

	for _, tc := range []struct {
		name       string
		updateErr  error
		wantStatus int
	}{
		{name: "success", wantStatus: http.StatusOK},
		{name: "database failure", updateErr: errors.New("database unavailable"), wantStatus: http.StatusInternalServerError},
		{name: "unknown customer", updateErr: errNotFound, wantStatus: http.StatusInternalServerError},
	} {
		t.Run(tc.name, func(t *testing.T) {
			calls := 0
			req := httptest.NewRequest(http.MethodPost, "/v1/stripe/webhook", strings.NewReader(string(payload)))
			req.Header.Set("Stripe-Signature", sig)
			rec := httptest.NewRecorder()

			handleStripeWebhookWithSetter(rec, req, secret, func(_ context.Context, customer, paymentMethod string) error {
				calls++
				if customer != "cus_known" || paymentMethod != "pm_new" {
					t.Fatalf("update target=(%q,%q), want (cus_known,pm_new)", customer, paymentMethod)
				}
				return tc.updateErr
			})

			if rec.Code != tc.wantStatus {
				t.Fatalf("status=%d body=%s, want %d", rec.Code, rec.Body.String(), tc.wantStatus)
			}
			if calls != 1 {
				t.Fatalf("saved-card update calls=%d, want 1", calls)
			}
			if tc.updateErr != nil && (strings.Contains(rec.Body.String(), "cus_known") ||
				strings.Contains(rec.Body.String(), tc.updateErr.Error())) {
				t.Fatalf("500 response leaked customer/database detail: %s", rec.Body.String())
			}
		})
	}
}

func TestValidateBillingPMUpdateCount(t *testing.T) {
	if err := validateBillingPMUpdateCount(1); err != nil {
		t.Fatalf("one updated customer rejected: %v", err)
	}
	if err := validateBillingPMUpdateCount(0); !errors.Is(err, errNotFound) {
		t.Fatalf("zero updated customers error=%v, want errNotFound", err)
	}
	if err := validateBillingPMUpdateCount(2); err == nil {
		t.Fatal("multiple customer mappings were silently accepted")
	}
}
