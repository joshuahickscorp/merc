package main

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestStripeTransportPinsVersionImmediatelyBeforeIO(t *testing.T) {
	var got string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got = r.Header.Get("Stripe-Version")
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(server.Close)

	req, err := http.NewRequestWithContext(context.Background(), http.MethodGet, server.URL, nil)
	must(t, err)
	// A stale or caller-supplied value must not win over the compiled contract.
	req.Header.Set("Stripe-Version", "account-default-or-stale")
	resp, err := doStripeRequest(server.Client(), req)
	must(t, err)
	_ = resp.Body.Close()
	if got != stripeAPIVersion {
		t.Fatalf("Stripe-Version at transport = %q, want %q", got, stripeAPIVersion)
	}
}

func TestStripeProviderSourcesCannotBypassPinnedTransport(t *testing.T) {
	for _, name := range []string{"billing.go", "payment.go", "stripe_settlement.go"} {
		body, err := os.ReadFile(name)
		must(t, err)
		for _, forbidden := range []string{"stripeHTTPClient.Do(", "p.http.Do("} {
			if strings.Contains(string(body), forbidden) {
				t.Fatalf("%s bypasses doStripeRequest with %q", name, forbidden)
			}
		}
		if !strings.Contains(string(body), "doStripeRequest(") {
			t.Fatalf("%s no longer routes its Stripe I/O through doStripeRequest", name)
		}
	}
}

func TestEveryStripeOperatorScriptPinsSameVersion(t *testing.T) {
	root := filepath.Join("..", "..", "ops", "scripts")
	entries, err := os.ReadDir(root)
	must(t, err)
	wantHeader := "Stripe-Version: " + stripeAPIVersion
	var checked []string
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".sh") {
			continue
		}
		path := filepath.Join(root, entry.Name())
		body, err := os.ReadFile(path)
		must(t, err)
		if !strings.Contains(string(body), "api.stripe.com") {
			continue
		}
		checked = append(checked, entry.Name())
		pinsLiteral := strings.Contains(string(body), wantHeader)
		pinsVariable := strings.Contains(string(body), "STRIPE_API_VERSION="+stripeAPIVersion) &&
			strings.Contains(string(body), "Stripe-Version: $STRIPE_API_VERSION")
		pinsSandboxAuthority := strings.Contains(string(body),
			`source "$ROOT/ops/scripts/lib/stripe-sandbox-contract.sh"`) &&
			strings.Contains(string(body), "Stripe-Version: $MERC_STRIPE_API_VERSION")
		if !pinsLiteral && !pinsVariable && !pinsSandboxAuthority {
			t.Errorf("%s calls Stripe without pinned header %q", entry.Name(), wantHeader)
		}
		if entry.Name() == "stripe-webhooks.sh" {
			for _, required := range []string{
				`-d "api_version=$STRIPE_API_VERSION"`,
				`endpoint_payload_version_matches "$existing_json"`,
				`endpoint_payload_version_matches "$resp"`,
				`select_endpoint_from_inventory "$inventory" "$url"`,
				`account.updated,payout.created,payout.paid,payout.failed`,
			} {
				if !strings.Contains(string(body), required) {
					t.Errorf("%s is missing webhook payload-version guard %q", entry.Name(), required)
				}
			}
			if strings.Contains(string(body), ".connect // false") {
				t.Errorf("%s relies on a connect field absent from Stripe's webhook endpoint response object",
					entry.Name())
			}
		}
		if entry.Name() == "stripe-sandbox-scenarios.sh" {
			for _, required := range []string{
				`merc_stripe_endpoint_contract`,
				`MERC_STRIPE_API_VERSION`,
				`payload_api_version:$stripe_api_version`,
				`staging_urls_exact:true`,
				`application_outcomes_verified:true`,
				`verify_cash_outcome "$cash_probe_closed_payload" applied 30`,
				`verify_cash_outcome "$cash_probe_opened_payload" stale_ignored 30`,
				`verify_cash_outcome "$cash_probe_closed_payload" duplicate`,
				`settlement:{currency:$settlement_currency`,
			} {
				if !strings.Contains(string(body), required) {
					t.Errorf("%s is missing webhook receipt-version guard %q", entry.Name(), required)
				}
			}
		}
	}
	if len(checked) != 5 {
		t.Fatalf("audited %d Stripe operator scripts %v, want 5; update the inventory deliberately",
			len(checked), checked)
	}
}

func TestDeploymentAcceptanceRequiresReadyStripeContract(t *testing.T) {
	for _, tc := range []struct {
		path     string
		required []string
	}{
		{
			path: filepath.Join("..", "..", "ops", "scripts", "droplet-deploy.sh"),
			required: []string{
				`"https://${SITE_HOST}/readyz"`,
				`.stripe_api_version == $version`,
				`.payment_mode == $mode`,
				`ready_ok=1`,
			},
		},
		{
			path: filepath.Join("..", "..", "ops", "scripts", "lib", "go-closure-common.sh"),
			required: []string{
				`"$base/readyz"`,
				`.stripe_api_version == "2025-06-30.basil"`,
				`.status == "ready"`,
			},
		},
	} {
		body, err := os.ReadFile(tc.path)
		must(t, err)
		for _, required := range tc.required {
			if !strings.Contains(string(body), required) {
				t.Errorf("%s is missing deployment acceptance guard %q", tc.path, required)
			}
		}
	}
}

func TestStripeGetUsesPinnedContract(t *testing.T) {
	withStripeTestServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.Header.Get("Stripe-Version"); got != stripeAPIVersion {
			t.Errorf("Stripe-Version = %q, want %q", got, stripeAPIVersion)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = fmt.Fprint(w, `{"id":"acct_contract"}`)
	}))
	out, err := stripeGet(context.Background(), "account")
	must(t, err)
	if out["id"] != "acct_contract" {
		t.Fatalf("response = %#v", out)
	}
}

func TestStripeEventContractBindsVersionAndLivemode(t *testing.T) {
	testMode, liveMode := false, true
	for _, tc := range []struct {
		name         string
		version      string
		livemode     *bool
		expectedLive bool
		wantError    bool
	}{
		{name: "test exact", version: stripeAPIVersion, livemode: &testMode},
		{name: "live exact", version: stripeAPIVersion, livemode: &liveMode, expectedLive: true},
		{name: "version absent", livemode: &testMode, wantError: true},
		{name: "version drift", version: "2026-02-25.clover", livemode: &testMode, wantError: true},
		{name: "livemode absent", version: stripeAPIVersion, wantError: true},
		{name: "live into test", version: stripeAPIVersion, livemode: &liveMode, wantError: true},
		{name: "test into live", version: stripeAPIVersion, livemode: &testMode, expectedLive: true, wantError: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := validateStripeEventContract(tc.version, tc.livemode, tc.expectedLive)
			if (err != nil) != tc.wantError {
				t.Fatalf("validateStripeEventContract() error = %v, wantError=%t", err, tc.wantError)
			}
		})
	}
}

func TestSignedStripeWebhookRejectsContractDriftBeforeEffect(t *testing.T) {
	const secret = "whsec_contract_test"
	for _, tc := range []struct {
		name         string
		envelope     string
		expectedLive bool
		wantStatus   int
		wantCalls    int
	}{
		{
			name: "exact test event",
			envelope: `{"type":"setup_intent.succeeded","api_version":"2025-06-30.basil","livemode":false,` +
				`"data":{"object":{"customer":"cus_exact","payment_method":"pm_exact"}}}`,
			wantStatus: http.StatusOK, wantCalls: 1,
		},
		{
			name: "version absent",
			envelope: `{"type":"setup_intent.succeeded","livemode":false,` +
				`"data":{"object":{"customer":"cus_missing","payment_method":"pm_missing"}}}`,
			wantStatus: http.StatusBadRequest,
		},
		{
			name: "version drift",
			envelope: `{"type":"setup_intent.succeeded","api_version":"2026-02-25.clover","livemode":false,` +
				`"data":{"object":{"customer":"cus_drift","payment_method":"pm_drift"}}}`,
			wantStatus: http.StatusBadRequest,
		},
		{
			name: "test event into live",
			envelope: `{"type":"setup_intent.succeeded","api_version":"2025-06-30.basil","livemode":false,` +
				`"data":{"object":{"customer":"cus_test","payment_method":"pm_test"}}}`,
			expectedLive: true, wantStatus: http.StatusBadRequest,
		},
		{
			name: "exact live event",
			envelope: `{"type":"setup_intent.succeeded","api_version":"2025-06-30.basil","livemode":true,` +
				`"data":{"object":{"customer":"cus_live","payment_method":"pm_live"}}}`,
			expectedLive: true, wantStatus: http.StatusOK, wantCalls: 1,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			calls := 0
			req := signedStripeCashRequest(t, []byte(tc.envelope), secret)
			rec := httptest.NewRecorder()
			handleStripeWebhookWithAllHandlersAtMode(
				rec, req, secret,
				func(context.Context, string, string) error {
					calls++
					return nil
				},
				nil, nil, tc.expectedLive,
			)
			if rec.Code != tc.wantStatus || calls != tc.wantCalls {
				t.Fatalf("status/calls = %d/%d, want %d/%d; body=%s",
					rec.Code, calls, tc.wantStatus, tc.wantCalls, rec.Body.String())
			}
		})
	}
}
