package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
)

// The ledger settles in the configured MERC_SETTLEMENT_CURRENCY. Send rejects
// any other currency (payment.go). That invariant is only half enforced,
// because a Stripe platform can accept a transfer request and then fail it with
// `balance_insufficient` when the account cannot hold the configured currency.
//
// A Canadian platform is the case that motivated this: a USD top-up settles
// into the CAD balance while the code was hardcoded to USD, so every supplier
// payout failed at the moment money was supposed to move.
//
// GET /v1/balance lists one bucket per *enabled settlement currency*, including
// currencies whose balance is zero. So the presence of a bucket for the
// CONFIGURED currency is a direct answer to "can this platform pay a supplier",
// and its absence predicts the failure before any money is at stake.

type stripeBalanceResponse struct {
	Available []struct {
		Currency string `json:"currency"`
	} `json:"available"`
	Pending []struct {
		Currency string `json:"currency"`
	} `json:"pending"`
	Error *struct {
		Message string `json:"message"`
	} `json:"error"`
}

// settlementCurrencies reports every currency this Stripe platform can settle.
func (p StripePayout) settlementCurrencies(ctx context.Context) ([]string, error) {
	if p.secret == "" {
		return nil, errPayoutUnconfigured
	}
	if _, err := authorizePaymentOperation(
		paymentOperationRead, 0, "", p.secret,
	); err != nil {
		return nil, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, "https://api.stripe.com/v1/balance", nil)
	if err != nil {
		return nil, err
	}
	req.SetBasicAuth(p.secret, "")
	resp, err := doStripeRequest(p.http, req)
	if err != nil {
		return nil, fmt.Errorf("stripe balance probe: %w", err)
	}
	defer resp.Body.Close()

	body, err := readBoundedRemoteBody(resp.Body, stripeAPIResponseMaxBytes)
	if err != nil {
		return nil, fmt.Errorf("stripe balance response read: %w", err)
	}
	var parsed stripeBalanceResponse
	if err := json.Unmarshal(body, &parsed); err != nil {
		return nil, fmt.Errorf("stripe balance response decode: %w", err)
	}
	if parsed.Error != nil {
		return nil, fmt.Errorf("stripe balance probe: %s", parsed.Error.Message)
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("stripe balance probe: HTTP %d", resp.StatusCode)
	}

	seen := map[string]bool{}
	var out []string
	for _, b := range parsed.Available {
		if c := strings.ToLower(b.Currency); c != "" && !seen[c] {
			seen[c] = true
			out = append(out, c)
		}
	}
	for _, b := range parsed.Pending {
		if c := strings.ToLower(b.Currency); c != "" && !seen[c] {
			seen[c] = true
			out = append(out, c)
		}
	}
	return out, nil
}

// errSettlementCurrencyUnsupported is returned when the configured platform
// cannot hold the currency the ledger settles in.
type errSettlementCurrencyUnsupported struct {
	Want string
	Have []string
}

func (e errSettlementCurrencyUnsupported) Error() string {
	have := "none"
	if len(e.Have) > 0 {
		have = strings.Join(e.Have, ",")
	}
	return fmt.Sprintf(
		"stripe platform cannot settle %s (enabled settlement currencies: %s); "+
			"every supplier payout would fail with balance_insufficient. "+
			"Add %s as a settlement currency on the Stripe account, or use a platform whose country supports it",
		strings.ToUpper(e.Want), have, strings.ToUpper(e.Want))
}

// verifySettlementCurrency fails closed when the platform cannot settle the
// configured MERC_SETTLEMENT_CURRENCY.
func (p StripePayout) verifySettlementCurrency(ctx context.Context) error {
	want, err := SettlementCurrency()
	if err != nil {
		return err
	}
	have, err := p.settlementCurrencies(ctx)
	if err != nil {
		return err
	}
	for _, c := range have {
		if c == want.Code() {
			return nil
		}
	}
	return errSettlementCurrencyUnsupported{Want: want.Code(), Have: have}
}

// stripeAccountCountry is reported alongside a failure so the operator knows
// whether the fix is a settlement-currency addition or a different account.
func (p StripePayout) accountCountry(ctx context.Context) string {
	if _, err := authorizePaymentOperation(
		paymentOperationRead, 0, "", p.secret,
	); err != nil {
		return ""
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, "https://api.stripe.com/v1/account", nil)
	if err != nil {
		return ""
	}
	req.SetBasicAuth(p.secret, "")
	resp, err := doStripeRequest(p.http, req)
	if err != nil {
		return ""
	}
	defer resp.Body.Close()
	body, err := readBoundedRemoteBody(resp.Body, stripeAPIResponseMaxBytes)
	if err != nil {
		return ""
	}
	var acct struct {
		Country         string `json:"country"`
		DefaultCurrency string `json:"default_currency"`
	}
	if json.Unmarshal(body, &acct) != nil {
		return ""
	}
	if acct.Country == "" {
		return ""
	}
	return fmt.Sprintf("%s/%s", acct.Country, strings.ToUpper(acct.DefaultCurrency))
}
