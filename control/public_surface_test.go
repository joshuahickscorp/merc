package main

import (
	"os"
	"strings"
	"testing"
)

func readSurfaceFixture(t *testing.T, path string) string {
	t.Helper()
	raw, err := os.ReadFile(path)
	must(t, err)
	return string(raw)
}

func TestBuyerSurfaceCallsEveryRequiredBuyerCapability(t *testing.T) {
	raw, err := os.ReadFile("../web/buyer.html")
	must(t, err)
	page := string(raw)
	for _, required := range []string{
		"/v1/public/config", "/v1/signup", "/v1/login", "/v1/me",
		"/v1/billing/setup", "/v1/billing/status", "/v1/billing/topup",
		"/v1/keys", "/v1/quote", "/v1/jobs", "/events", "/failures",
		"/results", "/invoice", "/receipt", "/dispute",
		"/v1/chat/completions", "/v1/images/generations", "/v1/realtime/requests/", "/v1/projects/",
		"/v1/service-leases", "/receipt", "/cancel", "cancel and settle observed usage", "majorToNanos",
	} {
		if !strings.Contains(page, required) {
			t.Errorf("buyer surface does not call %s", required)
		}
	}
	for _, forbidden := range []string{"localStorage", "sessionStorage", "pk_live_"} {
		if strings.Contains(page, forbidden) {
			t.Errorf("buyer surface contains forbidden browser authority %q", forbidden)
		}
	}
	for _, required := range []string{"amount_major:amountMajor,currency"} {
		if !strings.Contains(page, required) {
			t.Errorf("buyer top-up surface does not bind %q", required)
		}
	}
	if strings.Contains(page, "amount_usd") {
		t.Error("buyer top-up surface still exposes a USD-labeled payment amount")
	}
}

func TestSupplierSurfaceSeparatesOwnerAndWorkerAuthority(t *testing.T) {
	raw, err := os.ReadFile("../web/supplier.html")
	must(t, err)
	page := string(raw)
	for _, required := range []string{
		"/v1/public/config", "/v1/supplier/onboard", "/v1/supplier/status",
		"/v1/supplier/worker-tokens", "/v1/supplier/worker-credentials",
		"/v1/supplier/credential-audit", "/v1/worker/earnings", "/v1/worker/ledger",
		"/v1/worker/connect/status", "/v1/worker/viability", "/v1/worker/verification",
		"/v1/worker/service-leases/active",
		"/v1/supplier/enrollment-approvals",
		"headers.Authorization='Bearer '+ownerToken",
		"headers['X-Worker-Token']=workerToken",
		"c.credential_id",
		"device_request",
	} {
		if !strings.Contains(page, required) {
			t.Errorf("supplier surface lacks %q", required)
		}
	}
	for _, forbidden := range []string{"merc settles in USD", "localStorage", "sessionStorage"} {
		if strings.Contains(page, forbidden) {
			t.Errorf("supplier surface contains stale or unsafe text %q", forbidden)
		}
	}
}

func TestDeploymentWiresPublicBrowserAndContactAuthority(t *testing.T) {
	for _, path := range []string{"../docker-compose.prod.yml", "../ops/staging/compose.go-closure.yml"} {
		deployment := readSurfaceFixture(t, path)
		for _, required := range []string{
			"STRIPE_PUBLISHABLE_KEY", "MERC_SUPPORT_EMAIL", "MERC_SECURITY_EMAIL",
			"MERC_STATUS_URL", "MERC_TERMS_URL", "MERC_PRIVACY_URL",
		} {
			if !strings.Contains(deployment, required) {
				t.Errorf("%s does not wire %s", path, required)
			}
		}
	}
	staging := readSurfaceFixture(t, "../ops/staging/compose.go-closure.yml")
	for _, required := range []string{
		"/supplier?connect=return", "/supplier?connect=refresh",
	} {
		if !strings.Contains(staging, required) {
			t.Errorf("staging Connect flow does not return to %s", required)
		}
	}
}
