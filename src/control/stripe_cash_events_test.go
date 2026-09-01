package main

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestParseStripeChargeRefundedUsesCumulativeExactMinorUnits(t *testing.T) {
	payload := []byte(`{"id":"evt_refund","type":"charge.refunded"}`)
	object := []byte(`{"object":"charge","id":"ch_exact","payment_intent":{"object":"payment_intent","id":"pi_exact"},"amount":1200,"amount_refunded":275,"currency":"USD"}`)
	event, err := parseStripeCashEvent(
		"evt_refund", stripeEventChargeRefunded, 1_700_000_000, object, payload,
	)
	mustf(t, err, "parseStripeCashEvent: %v")
	if event.ObjectID != "ch_exact" || event.ChargeID != "ch_exact" ||
		event.PaymentIntent != "pi_exact" || event.AmountCents != 1200 ||
		event.RefundedCents != 275 || event.Currency != "usd" {
		t.Fatalf("parsed refund = %+v", event)
	}
	wantHash := sha256.Sum256(payload)
	if event.PayloadSHA256 != hex.EncodeToString(wantHash[:]) {
		t.Fatalf("payload hash=%q, want %x", event.PayloadSHA256, wantHash)
	}
}

func TestStripeExpandableReferenceBindsExpandedObjectKind(t *testing.T) {
	for _, tc := range []struct {
		name, raw, expected, want string
		wantErr                   bool
	}{
		{name: "string reference", raw: `"pi_string"`, expected: "payment_intent", want: "pi_string"},
		{name: "matching expanded reference", raw: `{"object":"charge","id":"ch_expanded"}`, expected: "charge", want: "ch_expanded"},
		{name: "wrong expanded reference", raw: `{"object":"payment_intent","id":"ch_wrong"}`, expected: "charge", wantErr: true},
		{name: "expanded reference missing object kind", raw: `{"id":"ch_missing_kind"}`, expected: "charge", wantErr: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := stripeExpandableID(json.RawMessage(tc.raw), tc.expected)
			if (err != nil) != tc.wantErr {
				t.Fatalf("stripeExpandableID() error=%v, wantErr=%t", err, tc.wantErr)
			}
			if err == nil && got != tc.want {
				t.Fatalf("stripeExpandableID()=%q, want %q", got, tc.want)
			}
		})
	}
}

func TestStripeCashParserRequiresExpectedObjectKinds(t *testing.T) {
	for _, tc := range []struct {
		name, eventType, object string
	}{
		{name: "refund must be charge", eventType: stripeEventChargeRefunded, object: `{"object":"dispute"}`},
		{name: "dispute must be dispute", eventType: stripeEventDisputeCreated, object: `{"object":"charge"}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := parseStripeCashEvent("evt_wrong_cash_object", tc.eventType, 1_700_000_001,
				[]byte(tc.object), []byte(`{"payload":"cash"}`)); err == nil {
				t.Fatal("accepted a Stripe object with the wrong kind")
			}
		})
	}
}

func TestParseStripeSucceededPaymentIntentRequiresOwnedExactCash(t *testing.T) {
	operationKey, charge, owned, err := parseStripeSucceededPaymentIntent([]byte(
		`{"object":"payment_intent","id":"pi_1","latest_charge":{"object":"charge","id":"ch_1"},"status":"succeeded","amount":99,"amount_received":99,"currency":"usd","metadata":{"cx_operation_key":"job-1"}}`,
	))
	if err != nil || !owned || operationKey != "job-1" ||
		charge.PaymentIntentID != "pi_1" || charge.ChargeID != "ch_1" || charge.ReceivedCents != 99 {
		t.Fatalf("parsed successful PI: key=%q owned=%v charge=%+v err=%v", operationKey, owned, charge, err)
	}
	if _, _, owned, err := parseStripeSucceededPaymentIntent([]byte(
		`{"object":"payment_intent","id":"pi_other","status":"succeeded","metadata":{}}`,
	)); err != nil || owned {
		t.Fatalf("unowned PaymentIntent should be ignored: owned=%v err=%v", owned, err)
	}
	if _, _, owned, err := parseStripeSucceededPaymentIntent([]byte(
		`{"object":"payment_intent","id":"pi_bad","latest_charge":"ch_bad","status":"succeeded","amount":99,"amount_received":98,"currency":"usd","metadata":{"cx_operation_key":"job-bad"}}`,
	)); err == nil || !owned {
		t.Fatalf("owned amount mismatch accepted: owned=%v err=%v", owned, err)
	}
	if _, _, owned, err := parseStripeSucceededPaymentIntent([]byte(
		`{"object":"payment_intent","id":"pi_bad_expanded","latest_charge":{"object":"payment_intent","id":"ch_bad_expanded"},"status":"succeeded","amount":99,"amount_received":99,"currency":"usd","metadata":{"cx_operation_key":"job-bad-expanded"}}`,
	)); err == nil || !owned {
		t.Fatalf("wrong expanded latest charge kind accepted: owned=%v err=%v", owned, err)
	}
	if _, _, owned, err := parseStripeSucceededPaymentIntent([]byte(
		`{"object":"charge","id":"pi_wrong_kind","latest_charge":"ch_wrong_kind","status":"succeeded","amount":99,"amount_received":99,"currency":"usd","metadata":{"cx_operation_key":"job-wrong-kind"}}`,
	)); err == nil || owned {
		t.Fatalf("wrong object kind was accepted: owned=%v err=%v", owned, err)
	}
}

func TestStripeDisputeCashEffectsAreConservativeAndOrdered(t *testing.T) {
	for _, tc := range []struct {
		name, eventType, status string
		wantEffect              stripeDisputeCashEffect
		wantRank                int
	}{
		{"formal creation", stripeEventDisputeCreated, "needs_response", stripeDisputeCashUnavailable, 10},
		{"warning inquiry", stripeEventDisputeCreated, "warning_needs_response", stripeDisputeCashNoEffect, 0},
		{"prevented", stripeEventDisputeCreated, "prevented", stripeDisputeCashNoEffect, 0},
		{"funds withdrawn", stripeEventDisputeFundsWithdrawn, "under_review", stripeDisputeCashUnavailable, 20},
		{"lost", stripeEventDisputeClosed, "lost", stripeDisputeCashUnavailable, 30},
		{"won does not prove restoration", stripeEventDisputeClosed, "won", stripeDisputeCashNoEffect, 0},
		{"funds restored", stripeEventDisputeFundsReinstated, "won", stripeDisputeCashAvailable, 40},
	} {
		t.Run(tc.name, func(t *testing.T) {
			gotEffect, gotRank := disputeCashEffect(tc.eventType, tc.status)
			if gotEffect != tc.wantEffect || gotRank != tc.wantRank {
				t.Fatalf("effect=(%d,%d), want (%d,%d)", gotEffect, gotRank, tc.wantEffect, tc.wantRank)
			}
		})
	}
}

func TestValidateStripeDisputeRejectsForgedDerivedEffect(t *testing.T) {
	event := stripeCashEvent{
		EventID: "evt_1", EventType: stripeEventDisputeCreated,
		ObjectID: "dp_1", ChargeID: "ch_1", Currency: "usd", Status: "needs_response",
		EventCreated: 1, AmountCents: 100, PayloadSHA256: strings.Repeat("0", 64),
		DisputeEffect: stripeDisputeCashAvailable, EffectRank: 40,
	}
	if err := validateStripeCashEvent(event); err == nil {
		t.Fatal("forged dispute cash effect was accepted")
	}
}

func signedStripeCashRequest(t *testing.T, payload []byte, secret string) *http.Request {
	t.Helper()
	ts := fmt.Sprint(time.Now().Unix())
	mac := hmac.New(sha256.New, []byte(secret))
	_, _ = mac.Write([]byte(ts + "." + string(payload)))
	req := httptest.NewRequest(http.MethodPost, "/v1/stripe/webhook", strings.NewReader(string(payload)))
	req.Header.Set("Stripe-Signature", "t="+ts+",v1="+hex.EncodeToString(mac.Sum(nil)))
	return req
}

func TestStripeCashWebhookVerifiesThenApplies(t *testing.T) {
	const secret = "whsec_cash_test"
	payload := []byte(`{"id":"evt_cash","type":"charge.refunded","api_version":"2025-06-30.basil","livemode":false,"created":1700000000,"data":{"object":{"object":"charge","id":"ch_cash","payment_intent":"pi_cash","amount":500,"amount_refunded":125,"currency":"usd"}}}`)

	for _, tc := range []struct {
		name       string
		signature  bool
		applyErr   error
		wantStatus int
		wantCalls  int
	}{
		{name: "valid", signature: true, wantStatus: http.StatusOK, wantCalls: 1},
		{name: "durable apply failure retries", signature: true, applyErr: errors.New("database secret detail"), wantStatus: http.StatusInternalServerError, wantCalls: 1},
		{name: "invalid signature", wantStatus: http.StatusBadRequest},
	} {
		t.Run(tc.name, func(t *testing.T) {
			calls := 0
			req := signedStripeCashRequest(t, payload, secret)
			if !tc.signature {
				req.Header.Set("Stripe-Signature", "t=1,v1=forged")
			}
			rec := httptest.NewRecorder()
			handleStripeWebhookWithHandlers(rec, req, secret, nil,
				func(_ context.Context, event stripeCashEvent) (stripeCashEventResult, error) {
					calls++
					if event.EventID != "evt_cash" || event.PaymentIntent != "pi_cash" ||
						event.RefundedCents != 125 {
						t.Fatalf("cash event = %+v", event)
					}
					return stripeCashEventResult{}, tc.applyErr
				})
			if rec.Code != tc.wantStatus || calls != tc.wantCalls {
				t.Fatalf("status=%d calls=%d, want %d/%d; body=%s",
					rec.Code, calls, tc.wantStatus, tc.wantCalls, rec.Body.String())
			}
			if strings.Contains(rec.Body.String(), "database secret detail") {
				t.Fatalf("response leaked durable apply error: %s", rec.Body.String())
			}
		})
	}
}

func TestStripeCashWebhookReturnsSafeApplicationOutcomeHeaders(t *testing.T) {
	const secret = "whsec_cash_outcome_test"
	payload := []byte(`{"id":"evt_outcome","type":"charge.dispute.closed",` +
		`"api_version":"2025-06-30.basil","livemode":false,"created":1700000002,` +
		`"data":{"object":{"object":"dispute","id":"dp_outcome","charge":"ch_outcome",` +
		`"amount":500,"currency":"cad","status":"lost"}}}`)

	for _, tc := range []struct {
		name        string
		result      stripeCashEventResult
		wantOutcome string
		wantRank    string
	}{
		{
			name: "applied",
			result: stripeCashEventResult{
				CashEffectApplied: true, CurrentCashEffectRank: 30,
			},
			wantOutcome: "applied", wantRank: "30",
		},
		{
			name: "stale ignored",
			result: stripeCashEventResult{
				CurrentCashEffectRank: 30,
			},
			wantOutcome: "stale_ignored", wantRank: "30",
		},
		{
			name: "duplicate",
			result: stripeCashEventResult{
				Duplicate: true,
			},
			wantOutcome: "duplicate",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			req := signedStripeCashRequest(t, payload, secret)
			rec := httptest.NewRecorder()
			handleStripeWebhookWithHandlers(
				rec, req, secret, nil,
				func(context.Context, stripeCashEvent) (stripeCashEventResult, error) {
					return tc.result, nil
				},
			)
			if rec.Code != http.StatusOK {
				t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
			}
			if got := rec.Header().Get("X-Merc-Stripe-Event-Outcome"); got != tc.wantOutcome {
				t.Fatalf("outcome header=%q, want %q", got, tc.wantOutcome)
			}
			if got := rec.Header().Get("X-Merc-Stripe-Cash-Effect-Rank"); got != tc.wantRank {
				t.Fatalf("rank header=%q, want %q", got, tc.wantRank)
			}
			if rec.Body.Len() != 0 {
				t.Fatalf("webhook outcome must not expose a response body: %q", rec.Body.String())
			}
		})
	}
}
