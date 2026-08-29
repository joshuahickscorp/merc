package main

import (
	"errors"
	"fmt"
	"net/http"
)

// stripeAPIVersion freezes the request/response semantics used by every Stripe
// call in this binary. Without this header Stripe applies the account's default
// API version, which an operator can change outside the Merc candidate and
// thereby change money-moving behaviour without a code or activation change.
const stripeAPIVersion = "2025-06-30.basil"

// doStripeRequest is the only transport boundary for Stripe API calls. Pinning
// immediately before client.Do makes the contract survive refactors that build
// requests in different helpers, and prevents a caller from accidentally
// clearing the header between request construction and network I/O.
func doStripeRequest(client *http.Client, req *http.Request) (*http.Response, error) {
	if client == nil {
		return nil, errors.New("Stripe HTTP client is nil")
	}
	if req == nil {
		return nil, errors.New("Stripe HTTP request is nil")
	}
	req.Header.Set("Stripe-Version", stripeAPIVersion)
	return client.Do(req)
}

// validateStripeEventContract binds signed webhook payloads to the same schema
// and value domain as outbound calls. Signature validity alone proves who sent
// bytes, not that the bytes were rendered under the reviewed API version or
// belong to the configured test/live mode.
func validateStripeEventContract(apiVersion string, livemode *bool, expectedLive bool) error {
	if apiVersion != stripeAPIVersion {
		return fmt.Errorf("Stripe event API version %q does not match compiled contract %q",
			apiVersion, stripeAPIVersion)
	}
	if livemode == nil {
		return errors.New("Stripe event livemode is missing")
	}
	if *livemode != expectedLive {
		return fmt.Errorf("Stripe event livemode=%t does not match expected mode=%t",
			*livemode, expectedLive)
	}
	return nil
}
