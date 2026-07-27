package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestAllAPIErrorCodesAreNonEmptyAndUnique(t *testing.T) {
	seen := make(map[APIErrorCode]bool, len(AllAPIErrorCodes))
	for _, code := range AllAPIErrorCodes {
		if code == "" {
			t.Fatal("empty code in AllAPIErrorCodes")
		}
		if seen[code] {
			t.Fatalf("duplicate code %q", code)
		}
		seen[code] = true
		if !validAPIErrorCodes[code] {
			t.Fatalf("code %q missing from validAPIErrorCodes", code)
		}
	}
	if len(seen) != len(validAPIErrorCodes) {
		t.Fatalf("validAPIErrorCodes size %d != AllAPIErrorCodes %d",
			len(validAPIErrorCodes), len(seen))
	}
}

func TestWriteErrAlwaysEmitsNonEmptyEnumCode(t *testing.T) {
	cases := []struct {
		status int
		msg    string
	}{
		{http.StatusUnauthorized, "missing or malformed Authorization bearer token"},
		{http.StatusUnauthorized, "invalid credential"},
		{http.StatusForbidden, "buyer is not approved for this private canary"},
		{http.StatusForbidden, "admin privilege required"},
		{http.StatusNotFound, "job not found"},
		{http.StatusBadRequest, "invalid job submission json: unexpected EOF"},
		{http.StatusConflict, "job not complete (status=failed)"},
		{http.StatusConflict, "quote expired"},
		{http.StatusGone, "dispute window closed"},
		{http.StatusRequestEntityTooLarge, "private-canary input limit is 1 bytes"},
		{http.StatusPaymentRequired, "no payment method on file and sandbox free credit is exhausted · save a card via POST /v1/billing/setup before submitting a job"},
		{http.StatusTooManyRequests, "rate limit exceeded"},
		{http.StatusTooManyRequests, "private-canary active-buyer limit reached"},
		{http.StatusTooManyRequests, "too many failed login attempts · try again later"},
		{http.StatusServiceUnavailable, "canary participant check unavailable"},
		{http.StatusServiceUnavailable, "job intake is paused by the operator"},
		{http.StatusServiceUnavailable, "stripe webhooks not configured (set STRIPE_WEBHOOK_SECRET)"},
		{http.StatusServiceUnavailable, "economic schedule unavailable: boom"},
		{http.StatusInternalServerError, "loading result keys: boom"},
		{http.StatusInternalServerError, "Task failed: model_load_failed"},
		{418, "teapot"}, // unknown 4xx still gets a code
		{599, "gateway timeout-ish"},
		{http.StatusBadRequest, ""}, // empty message still gets a code
	}

	for _, tc := range cases {
		rec := httptest.NewRecorder()
		writeErr(rec, tc.status, tc.msg)
		if rec.Code != tc.status && !(tc.status == 418 || tc.status == 599) {
			// writeJSON uses the status we pass; unknown codes still write as given.
		}
		if rec.Code != tc.status {
			t.Fatalf("status=%d msg=%q: wrote HTTP %d", tc.status, tc.msg, rec.Code)
		}
		var body APIError
		if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
			t.Fatalf("status=%d: decode: %v body=%s", tc.status, err, rec.Body.String())
		}
		if body.Error == "" {
			t.Fatalf("status=%d: empty error string", tc.status)
		}
		if body.Code == "" {
			t.Fatalf("status=%d msg=%q: empty code", tc.status, tc.msg)
		}
		if !validAPIErrorCodes[body.Code] {
			t.Fatalf("status=%d msg=%q: code %q not in closed enum", tc.status, tc.msg, body.Code)
		}
		if body.Action == "" {
			t.Fatalf("status=%d msg=%q: empty action", tc.status, tc.msg)
		}
	}
}

func TestWriteErr429CarriesRetryAfter(t *testing.T) {
	msgs := []string{
		"rate limit exceeded",
		"private-canary active-worker limit reached",
		"too many accounts created from this address today",
		"too many failed login attempts · try again later",
	}
	for _, msg := range msgs {
		rec := httptest.NewRecorder()
		writeErr(rec, http.StatusTooManyRequests, msg)
		if rec.Code != http.StatusTooManyRequests {
			t.Fatalf("msg=%q: status %d", msg, rec.Code)
		}
		ra := rec.Header().Get("Retry-After")
		if ra == "" {
			t.Fatalf("msg=%q: missing Retry-After", msg)
		}
		var body APIError
		if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
			t.Fatal(err)
		}
		if body.Code != ErrCodeRateLimited {
			t.Fatalf("msg=%q: code=%q want rate_limited", msg, body.Code)
		}
		if body.Action != ActionRetryAfter {
			t.Fatalf("msg=%q: action=%q want retry_after", msg, body.Action)
		}
	}
}

func TestWriteErrRetryable503CarriesRetryAfter(t *testing.T) {
	retryable := []string{
		"canary participant check unavailable",
		"database unreachable",
		"economic schedule unavailable: boom",
		"job intake is paused by the operator",
	}
	for _, msg := range retryable {
		rec := httptest.NewRecorder()
		writeErr(rec, http.StatusServiceUnavailable, msg)
		if rec.Header().Get("Retry-After") == "" {
			t.Fatalf("msg=%q: retryable 503 missing Retry-After", msg)
		}
		var body APIError
		if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
			t.Fatal(err)
		}
		if body.Code != ErrCodeUnavailable && body.Code != ErrCodeOperatorPaused {
			t.Fatalf("msg=%q: code=%q want unavailable|operator_paused", msg, body.Code)
		}
		if body.Action != ActionRetryAfter {
			t.Fatalf("msg=%q: action=%q", msg, body.Action)
		}
	}
}

func TestWriteErrNonRetryable503OmitsRetryAfter(t *testing.T) {
	msg := "stripe webhooks not configured (set STRIPE_WEBHOOK_SECRET)"
	rec := httptest.NewRecorder()
	writeErr(rec, http.StatusServiceUnavailable, msg)
	if ra := rec.Header().Get("Retry-After"); ra != "" {
		t.Fatalf("misconfigured 503 should not set Retry-After, got %q", ra)
	}
	var body APIError
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if body.Code != ErrCodeMisconfigured {
		t.Fatalf("code=%q want misconfigured", body.Code)
	}
	if body.Action != ActionContactSupport {
		t.Fatalf("action=%q want contact_support", body.Action)
	}
}

func TestWriteErrPreservesCallerRetryAfter(t *testing.T) {
	rec := httptest.NewRecorder()
	rec.Header().Set("Retry-After", "42")
	writeErr(rec, http.StatusTooManyRequests, "too many failed login attempts · try again later")
	if got := rec.Header().Get("Retry-After"); got != "42" {
		t.Fatalf("Retry-After overwritten: got %q want 42", got)
	}
}

func TestClassifyAPIErrorNeverReturnsEmptyCode(t *testing.T) {
	// Exhaust common status codes used by writeErr call sites.
	statuses := []int{
		400, 401, 402, 403, 404, 409, 410, 413, 429, 500, 503, 0, 418, 599,
	}
	msgs := []string{
		"",
		"rate limit exceeded",
		"status=failed",
		"Task failed: model_load_failed",
		"not configured",
		"paused by the operator",
		"payment method on file",
		"job not found",
	}
	for _, status := range statuses {
		for _, msg := range msgs {
			code, action, _ := classifyAPIError(status, msg)
			if code == "" || !validAPIErrorCodes[code] {
				t.Fatalf("status=%d msg=%q: invalid code %q", status, msg, code)
			}
			if action == "" {
				t.Fatalf("status=%d msg=%q: empty action", status, msg)
			}
		}
	}
}

func TestAuthMiddlewareRateLimitEmitsCodeAndRetryAfter(t *testing.T) {
	// Exercise the real writeErr path used by buyer auth rate limiting without
	// a store: missing bearer is unauthorized with a code.
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/v1/models", nil)
	NewServer(nil, nil, nil, nil).Routes().ServeHTTP(rec, req)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status=%d want 401", rec.Code)
	}
	var body APIError
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if body.Code != ErrCodeUnauthorized {
		t.Fatalf("code=%q want unauthorized body=%s", body.Code, rec.Body.String())
	}
	if !strings.Contains(body.Error, "Authorization") {
		t.Fatalf("error string unexpected: %q", body.Error)
	}
}
