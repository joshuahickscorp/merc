package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"
)

func TestPublicConfigRefusesPlaceholdersAndLivePublishableKey(t *testing.T) {
	installSettlementCurrencyForTest(t, "CAD")
	t.Setenv("STRIPE_PUBLISHABLE_KEY", "pk_live_must_not_reach_browser")
	t.Setenv("MERC_SUPPORT_EMAIL", "support@example.invalid")
	t.Setenv("MERC_SECURITY_EMAIL", "[SECURITY CONTACT REQUIRED]")
	t.Setenv("MERC_STATUS_URL", "http://status.example.com")
	t.Setenv("MERC_TERMS_URL", "https://terms.example.invalid")
	t.Setenv("MERC_PRIVACY_URL", "")

	config := currentPublicBrowserConfig()
	if config.SettlementCurrency != "cad" || config.StripePaymentFormEnabled || config.StripePublishableKey != "" {
		t.Fatalf("unsafe browser config: %+v", config)
	}
	if config.Contacts.Configured || config.Contacts.SupportEmail != "" || config.Contacts.SecurityEmail != "" {
		t.Fatalf("placeholder contacts escaped: %+v", config.Contacts)
	}
	wantMissing := []string{"privacy_url", "security_email", "status_url", "support_email", "terms_url"}
	if !reflect.DeepEqual(config.Contacts.Missing, wantMissing) {
		t.Fatalf("missing = %v, want %v", config.Contacts.Missing, wantMissing)
	}
}

func TestPublicConfigReturnsOnlyCompleteTestModeBrowserAuthority(t *testing.T) {
	installSettlementCurrencyForTest(t, "cad")
	t.Setenv("STRIPE_PUBLISHABLE_KEY", "pk_test_browser_fixture")
	t.Setenv("MERC_SUPPORT_EMAIL", "support@merc.test")
	t.Setenv("MERC_SECURITY_EMAIL", "security@merc.test")
	t.Setenv("MERC_STATUS_URL", "https://status.merc.test")
	t.Setenv("MERC_TERMS_URL", "https://merc.test/terms")
	t.Setenv("MERC_PRIVACY_URL", "https://merc.test/privacy")

	recorder := httptest.NewRecorder()
	(&Server{}).handlePublicConfig(recorder, httptest.NewRequest(http.MethodGet, "/v1/public/config", nil))
	if recorder.Code != http.StatusOK || recorder.Header().Get("Cache-Control") != "no-store" {
		t.Fatalf("status=%d cache=%q", recorder.Code, recorder.Header().Get("Cache-Control"))
	}
	var got publicBrowserConfig
	if err := json.Unmarshal(recorder.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	if !got.StripePaymentFormEnabled || got.StripePublishableKey != "pk_test_browser_fixture" ||
		!got.Contacts.Configured || len(got.Contacts.Missing) != 0 {
		t.Fatalf("config = %+v", got)
	}
}

func TestProductionSecurityTxtIsGeneratedFromStaffedRuntimeAuthority(t *testing.T) {
	t.Setenv("MERC_ENV", "production")
	t.Setenv("MERC_SECURITY_EMAIL", "security@merc.test")
	t.Setenv("MERC_PUBLIC_CONTROL_ORIGIN", "https://staging.merc.test")
	recorder := httptest.NewRecorder()
	(&Server{}).handleSecurityTxt(recorder, httptest.NewRequest(http.MethodGet, "/.well-known/security.txt", nil))
	if recorder.Code != http.StatusOK || !strings.Contains(recorder.Body.String(), "Contact: mailto:security@merc.test") ||
		!strings.Contains(recorder.Body.String(), "Canonical: https://staging.merc.test/.well-known/security.txt") {
		t.Fatalf("status=%d body=%q", recorder.Code, recorder.Body.String())
	}

	t.Setenv("MERC_SECURITY_EMAIL", "security@example.invalid")
	recorder = httptest.NewRecorder()
	(&Server{}).handleSecurityTxt(recorder, httptest.NewRequest(http.MethodGet, "/.well-known/security.txt", nil))
	if recorder.Code != http.StatusServiceUnavailable || strings.Contains(recorder.Body.String(), "example.invalid") {
		t.Fatalf("placeholder production security.txt status=%d body=%q", recorder.Code, recorder.Body.String())
	}
}
