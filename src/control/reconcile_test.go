package main

import (
	"context"
	"fmt"
	"net/http"
	"strings"
	"testing"

	"github.com/google/uuid"
)

func TestStripePayoutReconciliationRequiresExactTransferReferences(t *testing.T) {
	expected := []StripePayoutTransferExpectation{
		{TransferRef: "tr_expected", SentCents: 100, Currency: "usd"},
	}

	t.Run("exact reference and amount", func(t *testing.T) {
		actual := stripeTransferSnapshot{Transfers: map[string]stripeTransferObservation{
			"tr_expected": {ID: "tr_expected", AmountCents: 100, Currency: "usd"},
		}}
		if err := compareStripePayoutTransfers(expected, actual); err != nil {
			t.Fatalf("exact transfer set rejected: %v", err)
		}
	})

	t.Run("equal total replacement is drift", func(t *testing.T) {
		actual := stripeTransferSnapshot{Transfers: map[string]stripeTransferObservation{
			"tr_rogue": {ID: "tr_rogue", AmountCents: 100, Currency: "usd"},
		}}
		err := compareStripePayoutTransfers(expected, actual)
		if err == nil || !strings.Contains(err.Error(), "not represented") {
			t.Fatalf("equal-sized replacement accepted: %v", err)
		}
	})

	t.Run("missing provider transfer is drift", func(t *testing.T) {
		err := compareStripePayoutTransfers(expected, stripeTransferSnapshot{
			Transfers: map[string]stripeTransferObservation{},
		})
		if err == nil || !strings.Contains(err.Error(), "absent") {
			t.Fatalf("missing provider transfer accepted: %v", err)
		}
	})

	for name, provider := range map[string]stripeTransferObservation{
		"amount drift":   {ID: "tr_expected", AmountCents: 101, Currency: "usd"},
		"currency drift": {ID: "tr_expected", AmountCents: 100, Currency: "cad"},
	} {
		t.Run(name, func(t *testing.T) {
			err := compareStripePayoutTransfers(expected, stripeTransferSnapshot{
				Transfers: map[string]stripeTransferObservation{"tr_expected": provider},
			})
			if err == nil || !strings.Contains(err.Error(), "differs") {
				t.Fatalf("provider %s accepted: %v", name, err)
			}
		})
	}
}

func TestStoreStripePayoutReconciliationInputsIncludeIdleAccounts(t *testing.T) {
	ctx, store, pool := openIsolatedTestStore(t)
	f := seedPayoutFixture(t, ctx, pool, payoutFixtureOpts{creditUSD: 1.00})
	acct := "acct_reconcile_" + uuid.NewString()
	mustf(t, store.SetSupplierStripeAcct(ctx, f.supplierID, acct), "bind supplier Stripe account: %v")
	transferRef := "tr_expected_" + uuid.NewString()
	_, err := pool.Exec(ctx, `
		INSERT INTO supplier_payout_operations
		  (ledger_entry_id,supplier_id,requested_cents,sent_cents,currency,status,cash_moved,transfer_ref)
		VALUES ($1,$2,$3,$3,$4,'released',true,$5)`,
		f.entryID, f.supplierID, f.creditCents, f.currency, transferRef)
	mustf(t, err, "seed cash-moving payout operation: %v")

	accounts, err := store.ListSupplierStripeAccounts(ctx)
	mustf(t, err, "list supplier Stripe accounts: %v")
	var found bool
	for _, account := range accounts {
		if account.SupplierID == f.supplierID && account.AccountID == acct {
			found = true
			break
		}
	}
	if !found {
		t.Fatalf("bound account %s was not enumerated: %+v", acct, accounts)
	}

	expectations, err := store.ListStripePayoutTransferExpectations(ctx)
	mustf(t, err, "list Stripe payout expectations: %v")
	var expected bool
	for _, expectation := range expectations {
		if expectation.SupplierID == f.supplierID && expectation.TransferRef == transferRef &&
			expectation.SentCents == f.creditCents && expectation.Currency == f.currency {
			expected = true
			break
		}
	}
	if !expected {
		t.Fatalf("cash-moving transfer %s was not enumerated: %+v", transferRef, expectations)
	}
}

func TestStripeTransferredUsesSettlementMinorUnitsAndCurrency(t *testing.T) {
	installSettlementCurrencyForTest(t, "jpy")
	withStripeTestServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/transfers" || r.URL.Query().Get("destination") != "acct_jpy" {
			t.Errorf("request = %s", r.URL.String())
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = fmt.Fprint(w, `{"data":[{"object":"transfer","id":"tr_1","destination":"acct_jpy","amount":2,"currency":"jpy"},{"object":"transfer","id":"tr_2","destination":"acct_jpy","amount":3,"currency":"jpy"}],"has_more":false}`)
	}))
	got, err := stripeTransferredUSD(context.Background(), "acct_jpy")
	if err != nil || got != 5 {
		t.Fatalf("JPY transferred = %v, %v; want 5 JPY", got, err)
	}
}

func TestStripeTransferredRefusesMismatchedOrFractionalCash(t *testing.T) {
	installSettlementCurrencyForTest(t, "cad")
	for name, body := range map[string]string{
		"currency mismatch":     `{"data":[{"object":"transfer","id":"tr_wrong","destination":"acct_cad","amount":5,"currency":"usd"}],"has_more":false}`,
		"fractional minor unit": `{"data":[{"object":"transfer","id":"tr_fraction","destination":"acct_cad","amount":5.5,"currency":"cad"}],"has_more":false}`,
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

func TestStripeTransferredRequiresCompleteTypedPages(t *testing.T) {
	installSettlementCurrencyForTest(t, "cad")
	for name, body := range map[string]string{
		"wrong transfer object":        `{"data":[{"id":"pi_wrong","object":"transfer","destination":"acct_cad","amount":5,"currency":"cad"}],"has_more":false}`,
		"wrong transfer object type":   `{"data":[{"id":"tr_wrong_type","object":"payment_intent","destination":"acct_cad","amount":5,"currency":"cad"}],"has_more":false}`,
		"wrong transfer destination":   `{"data":[{"id":"tr_wrong_destination","object":"transfer","destination":"acct_other","amount":5,"currency":"cad"}],"has_more":false}`,
		"missing transfer object":      `{"data":[{"id":"tr_missing_object","destination":"acct_cad","amount":5,"currency":"cad"}],"has_more":false}`,
		"missing transfer destination": `{"data":[{"id":"tr_missing_destination","object":"transfer","amount":5,"currency":"cad"}],"has_more":false}`,
		"missing transfer id":          `{"data":[{"object":"transfer","destination":"acct_cad","amount":5,"currency":"cad"}],"has_more":false}`,
		"missing has_more":             `{"data":[{"object":"transfer","id":"tr_one","destination":"acct_cad","amount":5,"currency":"cad"}]}`,
		"empty continuation":           `{"data":[],"has_more":true}`,
	} {
		t.Run(name, func(t *testing.T) {
			withStripeTestServer(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				_, _ = fmt.Fprint(w, body)
			}))
			if _, err := stripeTransferredUSD(context.Background(), "acct_cad"); err == nil {
				t.Fatalf("accepted incomplete or wrong-object transfer page: %s", body)
			}
		})
	}
}

func TestStripeTransferredPaginatesByTypedCursor(t *testing.T) {
	installSettlementCurrencyForTest(t, "cad")
	var cursors []string
	withStripeTestServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		cursors = append(cursors, r.URL.Query().Get("starting_after"))
		if r.URL.Query().Get("destination") != "acct_cad-safe" || r.URL.Query().Get("limit") != "100" {
			t.Errorf("query = %s", r.URL.RawQuery)
		}
		w.Header().Set("Content-Type", "application/json")
		if len(cursors) == 1 {
			_, _ = fmt.Fprint(w, `{"data":[{"object":"transfer","id":"tr_first","destination":"acct_cad-safe","amount":5,"currency":"cad"}],"has_more":true}`)
			return
		}
		_, _ = fmt.Fprint(w, `{"data":[{"object":"transfer","id":"tr_second","destination":"acct_cad-safe","amount":7,"currency":"cad"}],"has_more":false}`)
	}))
	got, err := stripeTransferredUSD(context.Background(), "acct_cad-safe")
	if err != nil || got != 0.12 {
		t.Fatalf("paginated transferred = %v, %v; want 0.12 cad", got, err)
	}
	if len(cursors) != 2 || cursors[0] != "" || cursors[1] != "tr_first" {
		t.Fatalf("pagination cursors = %q, want [empty tr_first]", cursors)
	}
}
