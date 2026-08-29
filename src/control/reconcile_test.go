package main

import (
	"context"
	"fmt"
	"net/http"
	"testing"
)

func TestStripeTransferredUsesSettlementMinorUnitsAndCurrency(t *testing.T) {
	installSettlementCurrencyForTest(t, "jpy")
	withStripeTestServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/transfers" || r.URL.Query().Get("destination") != "acct_jpy" {
			t.Errorf("request = %s", r.URL.String())
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = fmt.Fprint(w, `{"data":[{"id":"tr_1","amount":2,"currency":"jpy"},{"id":"tr_2","amount":3,"currency":"jpy"}],"has_more":false}`)
	}))
	got, err := stripeTransferredUSD(context.Background(), "acct_jpy")
	if err != nil || got != 5 {
		t.Fatalf("JPY transferred = %v, %v; want 5 JPY", got, err)
	}
}

func TestStripeTransferredRefusesMismatchedOrFractionalCash(t *testing.T) {
	installSettlementCurrencyForTest(t, "cad")
	for name, body := range map[string]string{
		"currency mismatch":     `{"data":[{"id":"tr_wrong","amount":5,"currency":"usd"}],"has_more":false}`,
		"fractional minor unit": `{"data":[{"id":"tr_fraction","amount":5.5,"currency":"cad"}],"has_more":false}`,
	} {
		t.Run(name, func(t *testing.T) {
			withStripeTestServer(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				_, _ = fmt.Fprint(w, body)
			}))
			if _, err := stripeTransferredUSD(context.Background(), "acct_cad"); err == nil {
				t.Fatalf("accepted invalid Stripe transfer response: %s", body)
			}
		})
	}
}
