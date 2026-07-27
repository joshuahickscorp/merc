package main

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// stripeBalanceStub serves a canned /v1/balance body and records the version header.
func stripeBalanceStub(t *testing.T, status int, body string) (*httptest.Server, *string) {
	t.Helper()
	var gotVersion string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotVersion = r.Header.Get("Stripe-Version")
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_, _ = w.Write([]byte(body))
	}))
	t.Cleanup(srv.Close)
	return srv, &gotVersion
}

// payoutAgainst points a StripePayout at a stub by rewriting the request host.
func payoutAgainst(srv *httptest.Server, secret string) StripePayout {
	return StripePayout{
		secret: secret,
		http: &http.Client{
			Timeout:   5 * time.Second,
			Transport: rewriteHostTransport{base: srv.Listener.Addr().String()},
		},
	}
}

type rewriteHostTransport struct{ base string }

func (t rewriteHostTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	r.URL.Scheme = "http"
	r.URL.Host = t.base
	return http.DefaultTransport.RoundTrip(r)
}

func TestSettlementPreflightAcceptsPlatformThatCanHoldUSD(t *testing.T) {
	srv, version := stripeBalanceStub(t, http.StatusOK,
		`{"available":[{"currency":"usd"},{"currency":"cad"}],"pending":[{"currency":"usd"}]}`)
	if err := payoutAgainst(srv, "sk_test_x").verifySettlementCurrency(context.Background()); err != nil {
		t.Fatalf("USD bucket present but preflight rejected it: %v", err)
	}
	if *version != stripeAPIVersion {
		t.Fatalf("probe did not pin the API version: got %q want %q", *version, stripeAPIVersion)
	}
}

// The regression this file exists for: a Canadian platform reports only a CAD
// bucket, accepts a USD transfer request, and then fails it with
// balance_insufficient once money is meant to move.
func TestSettlementPreflightRejectsCADOnlyPlatform(t *testing.T) {
	srv, _ := stripeBalanceStub(t, http.StatusOK, `{"available":[{"currency":"cad"}],"pending":[]}`)
	err := payoutAgainst(srv, "sk_test_x").verifySettlementCurrency(context.Background())
	if err == nil {
		t.Fatal("CAD-only platform accepted; every payout would fail at transfer time")
	}
	var unsupported errSettlementCurrencyUnsupported
	if !errors.As(err, &unsupported) {
		t.Fatalf("want errSettlementCurrencyUnsupported, got %T: %v", err, err)
	}
	if !strings.Contains(err.Error(), "cad") {
		t.Fatalf("error should name what the platform can settle: %v", err)
	}
	if !strings.Contains(err.Error(), "balance_insufficient") {
		t.Fatalf("error should name the failure it prevents: %v", err)
	}
}

// A zero USD balance is still a USD-capable platform: Stripe lists a bucket per
// enabled settlement currency regardless of amount. Rejecting on amount would
// refuse to boot a correctly configured platform that simply has not been funded.
func TestSettlementPreflightAcceptsZeroUSDBalance(t *testing.T) {
	srv, _ := stripeBalanceStub(t, http.StatusOK, `{"available":[{"currency":"usd"}],"pending":[]}`)
	if err := payoutAgainst(srv, "sk_test_x").verifySettlementCurrency(context.Background()); err != nil {
		t.Fatalf("zero USD balance rejected: %v", err)
	}
}

// Fail closed, never open: an unreachable or erroring Stripe must not read as
// "settlement is fine".
func TestSettlementPreflightFailsClosedOnAPIError(t *testing.T) {
	for _, tc := range []struct {
		name, body string
		status     int
	}{
		{"stripe error object", `{"error":{"message":"Invalid API Key provided"}}`, http.StatusUnauthorized},
		{"non-200 without body", `{}`, http.StatusInternalServerError},
		{"unparseable body", `not json`, http.StatusOK},
	} {
		t.Run(tc.name, func(t *testing.T) {
			srv, _ := stripeBalanceStub(t, tc.status, tc.body)
			if err := payoutAgainst(srv, "sk_test_x").verifySettlementCurrency(context.Background()); err == nil {
				t.Fatal("preflight passed on an API failure; it must fail closed")
			}
		})
	}
}

func TestSettlementPreflightRequiresACredential(t *testing.T) {
	if err := (StripePayout{}).verifySettlementCurrency(context.Background()); !errors.Is(err, errPayoutUnconfigured) {
		t.Fatalf("want errPayoutUnconfigured, got %v", err)
	}
}

func TestIsProductionEnvMatchesHardeningSpellings(t *testing.T) {
	for _, s := range []string{"production", "PRODUCTION", "Prod", "prod"} {
		if !isProductionEnv(s) {
			t.Fatalf("%q should be production", s)
		}
	}
	for _, s := range []string{"", "dev", "staging", "prod-like", "productionish"} {
		if isProductionEnv(s) {
			t.Fatalf("%q should not be production", s)
		}
	}
}
