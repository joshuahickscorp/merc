package main

import (
	"errors"
	"os"
	"sync"
	"testing"
)

func TestStripeSimulatorScenarioAndGeneratedProperties(t *testing.T) {
	receipt, err := runDeterministicStripeSimulation(20260719)
	must(t, err)
	if receipt.Status != "SIMULATED PASS" || receipt.Label != "SIMULATED" {
		t.Fatalf("simulator receipt is not explicitly labeled: %+v", receipt)
	}
	for scenario, exercised := range receipt.Scenarios {
		if !exercised {
			t.Fatalf("simulator scenario %q was not exercised", scenario)
		}
	}
	for _, property := range []string{
		"ledger_zero_sum", "no_duplicate_authorization", "no_duplicate_settlement",
		"no_duplicate_payout", "refund_bounded", "dispute_bounded", "hold_enforced",
		"stale_attempt_cannot_settle", "state_non_regression",
		"uncertain_response_recoverable", "reconciliation_identifies_faults",
	} {
		if !receipt.Properties[property] {
			t.Fatalf("simulator property %q was not asserted", property)
		}
	}
	must(t, runGeneratedStripeProperties(4096))
}

func TestStripeSimulatorConcurrentIdempotentRetry(t *testing.T) {
	sim := newDeterministicStripeSimulator(91)
	const workers = 64
	var wg sync.WaitGroup
	ids := make(chan string, workers)
	errs := make(chan error, workers)
	for range workers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			intent, err := sim.createPaymentIntent("concurrent-key", 1200, "attempt-7", true)
			if err != nil {
				errs <- err
				return
			}
			ids <- intent.ID
		}()
	}
	wg.Wait()
	close(ids)
	close(errs)
	for err := range errs {
		t.Fatal(err)
	}
	want := ""
	for id := range ids {
		if want == "" {
			want = id
		}
		if id != want {
			t.Fatalf("concurrent retry produced %q and %q", want, id)
		}
	}
	if _, err := sim.createPaymentIntent("concurrent-key", 1201, "attempt-7", true); !errors.Is(err, errSimConflict) {
		t.Fatalf("conflicting concurrent binding: %v", err)
	}
}

func TestPaymentProviderSimulatorNeverStartsServer(t *testing.T) {
	for _, environment := range []string{"production", "prod", "staging", "development", ""} {
		if err := validatePaymentProviderMode(environment, "simulator"); err == nil {
			t.Fatalf("simulator unexpectedly available to server in %q", environment)
		}
	}
}

func TestStripeCredentialClassificationDoesNotExposeValues(t *testing.T) {
	cases := map[string]string{
		"": "missing", "sk_test_example": "sk_test", "sk_live_example": "sk_live",
		"rk_test_example": "rk_test", "rk_live_example": "rk_live",
		"pk_test_example": "publishable_test", "pk_live_example": "publishable_live",
		"whsec_example": "webhook_present", "opaque": "unknown",
	}
	for input, want := range cases {
		if got := stripeCredentialClass(input); got != want {
			t.Fatalf("class=%q want %q", got, want)
		}
	}
	if !isLiveStripeCredential("rk_live_example") || !isLiveStripeCredential("pk_live_example") {
		t.Fatal("live restricted/publishable classes were not refused")
	}
	_ = os.Unsetenv("MERC_PAYMENT_PROVIDER")
}
