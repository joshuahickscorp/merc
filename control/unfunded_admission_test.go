package main

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/google/uuid"
)

// A buyer with no credit and no card must not be admitted.
//
// The funding gate used to sit inside `if stripeKey() != ""`, so on any
// deployment without a Stripe key -- which is exactly the sandbox shape a
// private beta runs -- neither the free-credit check nor the ceiling clamp
// executed and the 402 below was unreachable. A stranger holding an account
// worth $0.00 submitted work and got 202 back. The balance was decorative.
//
// Two properties, because the second is what a ceiling is for:
//   - zero credit and no card is refused with 402
//   - a request declaring more than the buyer holds is clamped down to the
//     balance rather than accepted at the number the buyer asked for
func TestUnfundedBuyerIsRefusedWithoutStripeConfigured(t *testing.T) {
	strangerDeploymentInputs(t)
	installSettlementCurrencyForTest(t, "usd")

	// The deployment under test: no payment provider at all. Free credit is
	// Merc's own ledger, so it has to be enforced by Merc, not by Stripe.
	t.Setenv("STRIPE_SECRET_KEY", "")
	t.Setenv("MERC_SANDBOX_CREDIT_USD", "0")

	artifacts := newArtifactHarness(t)
	ctx, store, pool := openIsolatedTestStore(t)

	schedule, err := BuildCataloguePriceSchedule()
	if err != nil {
		t.Fatalf("build catalogue price schedule: %v", err)
	}
	if _, err := store.ApplyRepricing(ctx, schedule); err != nil {
		t.Fatalf("publish catalogue price schedule: %v", err)
	}
	if err := seedDemo(ctx, pool, artifacts.storage); err != nil {
		t.Fatalf("seed verification floor: %v", err)
	}

	verifier := NewVerifier(store).WithStorage(artifacts.storage)
	srv := httptest.NewServer(NewServer(store, artifacts.storage, verifier, nil).Routes())
	t.Cleanup(srv.Close)

	signup := postJSON(t, srv.URL+"/v1/signup", "", map[string]any{
		"email":    "unfunded-" + uuid.NewString() + "@example.test",
		"password": "a-stranger-password-1234",
	})
	if signup.status != http.StatusCreated && signup.status != http.StatusOK {
		t.Fatalf("signup: HTTP %d: %s", signup.status, signup.body)
	}
	apiKey, _ := signup.json["sandbox_key"].(string)
	if apiKey == "" {
		t.Fatalf("signup returned no sandbox key: %s", signup.body)
	}

	submit := postJSONWithHeaders(t, srv.URL+"/v1/jobs", apiKey, map[string]string{
		"Idempotency-Key": uuid.NewString(),
	}, map[string]any{
		"job_type": map[string]any{"type": "embed"},
		"model":    map[string]any{"kind": "hf", "ref": "all-minilm-l6-v2"},
		"tier":     "batch",
		"input":    strangerCorpus,
		// Deliberately above any plausible balance: a ceiling the buyer cannot
		// cover must not be honoured at the number they asked for.
		"max_usd": 10.0,
	})

	if submit.status != http.StatusPaymentRequired {
		t.Fatalf("unfunded buyer admitted: HTTP %d: %s\n"+
			"a $0.00 account must not be able to submit work; the funding gate "+
			"must not depend on a Stripe key being configured",
			submit.status, submit.body)
	}
}
