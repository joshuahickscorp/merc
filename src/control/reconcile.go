package main

import (
	"context"
	"fmt"
	"log"
	"math"
	"net/url"
	"sort"
	"strings"
	"time"

	"github.com/google/uuid"
)

const reconcileInterval = 15 * time.Minute

func reconcileEpsilonMajor() float64 {
	settlement, err := SettlementCurrency()
	if err != nil {
		return 0
	}
	micros, err := settlement.MinorToMicros(1)
	if err != nil {
		return 0
	}
	return microsToUSD(micros)
}

func (wk *Workers) reconcileLedger(ctx context.Context) error {
	if stripeKey() == "" {
		log.Print("workers: reconcile skipped  -  Stripe not configured (no transfers to reconcile against)")
		return nil
	}

	rollups, err := wk.store.ListPayoutsAdmin(ctx)
	if err != nil {
		return err
	}
	accounts, err := wk.store.ListSupplierStripeAccounts(ctx)
	if err != nil {
		return err
	}
	transferExpectations, err := wk.store.ListStripePayoutTransferExpectations(ctx)
	if err != nil {
		return err
	}

	type supplierCashExpectation struct {
		cashSentUSD         float64
		liabilityUSD        float64
		carriedUSD          float64
		releasedWithoutCash int
		outcomeUnknown      int
		transferRefs        map[string]StripePayoutTransferExpectation
	}
	expected := make(map[uuid.UUID]*supplierCashExpectation)
	accountBySupplier := make(map[uuid.UUID]string, len(accounts))
	for _, account := range accounts {
		if previous, ok := accountBySupplier[account.SupplierID]; ok && previous != account.AccountID {
			return fmt.Errorf("supplier %s has multiple Stripe connected account bindings", account.SupplierID)
		}
		accountBySupplier[account.SupplierID] = account.AccountID
		e := expected[account.SupplierID]
		if e == nil {
			e = &supplierCashExpectation{}
			expected[account.SupplierID] = e
		}
	}
	for _, r := range rollups {
		if r.SupplierID == (uuid.UUID{}) {
			continue
		}
		e := expected[r.SupplierID]
		if e == nil {
			e = &supplierCashExpectation{}
			expected[r.SupplierID] = e
		}
		e.cashSentUSD += r.CashSentUSD
		e.liabilityUSD += r.AmountUSD
		e.carriedUSD += r.CarriedRemainderUSD
		e.releasedWithoutCash += r.ReleasedWithoutCashCount
		e.outcomeUnknown += r.OutcomeUnknownCount
	}
	for _, transfer := range transferExpectations {
		e := expected[transfer.SupplierID]
		if e == nil {
			e = &supplierCashExpectation{}
			expected[transfer.SupplierID] = e
		}
		if e.transferRefs == nil {
			e.transferRefs = make(map[string]StripePayoutTransferExpectation)
		}
		if _, exists := e.transferRefs[transfer.TransferRef]; exists {
			return fmt.Errorf("supplier %s has duplicate Stripe payout reference %s", transfer.SupplierID, transfer.TransferRef)
		}
		e.transferRefs[transfer.TransferRef] = transfer
	}
	suppliers := make([]uuid.UUID, 0, len(expected))
	for supplierID, e := range expected {
		if _, hasAccount := accountBySupplier[supplierID]; hasAccount ||
			e.cashSentUSD > 0 || len(e.transferRefs) > 0 ||
			e.releasedWithoutCash > 0 || e.outcomeUnknown > 0 {
			suppliers = append(suppliers, supplierID)
		}
	}
	sort.Slice(suppliers, func(i, j int) bool { return suppliers[i].String() < suppliers[j].String() })

	var checked, drifted int
	for _, supplierID := range suppliers {
		e := expected[supplierID]
		acct, hasAccount := accountBySupplier[supplierID]
		supplierDrifted := false
		markDrift := func() {
			if supplierDrifted {
				return
			}
			supplierDrifted = true
			drifted++
			metrics.reconcileDrift.Add(1)
		}
		if !hasAccount {
			if e.cashSentUSD > 0 || len(e.transferRefs) > 0 || e.releasedWithoutCash > 0 || e.outcomeUnknown > 0 {
				log.Printf("workers: reconcile DRIFT: supplier %s shows %s %.6f cash sent (%.6f liability, %.6f carried) but has no connected Stripe account",
					supplierID, SettlementCurrencyCode(), e.cashSentUSD, e.liabilityUSD, e.carriedUSD)
				markDrift()
			}
			continue
		}
		if e.releasedWithoutCash > 0 {
			log.Printf("workers: reconcile DRIFT: supplier %s (%s) has %d released liability row(s) without a cash-moved payout operation (rollup liability %s %.6f)",
				supplierID, acct, e.releasedWithoutCash, SettlementCurrencyCode(), e.liabilityUSD)
			markDrift()
		}
		if e.outcomeUnknown > 0 {
			log.Printf("workers: reconcile DRIFT: supplier %s (%s) has %d unresolved provider outcome(s); possible cash must be resolved by exact payout key",
				supplierID, acct, e.outcomeUnknown)
			markDrift()
		}
		if (e.cashSentUSD > 0) != (len(e.transferRefs) > 0) {
			log.Printf("workers: reconcile DRIFT: supplier %s (%s) ledger cash rollup and exact payout references disagree (cash sent %.6f, transfer refs %d)",
				supplierID, acct, e.cashSentUSD, len(e.transferRefs))
			markDrift()
		}
		snapshot, terr := fetchStripeTransferSnapshot(ctx, acct)
		if terr != nil {
			log.Printf("workers: reconcile: supplier %s (%s) stripe transfers: %v", supplierID, acct, terr)
			continue
		}
		checked++
		refs := make([]StripePayoutTransferExpectation, 0, len(e.transferRefs))
		for _, transfer := range e.transferRefs {
			refs = append(refs, transfer)
		}
		if transferErr := compareStripePayoutTransfers(refs, snapshot); transferErr != nil {
			log.Printf("workers: reconcile DRIFT: supplier %s (%s): %v", supplierID, acct, transferErr)
			markDrift()
		}
		transferred, terr := stripeTransferSnapshotUSD(snapshot)
		if terr != nil {
			log.Printf("workers: reconcile: supplier %s (%s) stripe transfer total: %v", supplierID, acct, terr)
			continue
		}
		if e.cashSentUSD > 0 {
			if delta := e.cashSentUSD - transferred; math.Abs(delta) >= reconcileEpsilonMajor() {
				log.Printf("workers: reconcile DRIFT: supplier %s (%s): ledger cash sent %s %.6f vs stripe transferred %s %.6f (delta %s %.6f; liability %.6f, carried %.6f)",
					supplierID, acct, SettlementCurrencyCode(), e.cashSentUSD, SettlementCurrencyCode(), transferred,
					SettlementCurrencyCode(), delta, e.liabilityUSD, e.carriedUSD)
				markDrift()
			}
		}
	}
	log.Printf("workers: reconcile complete  -  %d supplier(s) checked, %d with drift", checked, drifted)
	return nil
}

type stripeTransferObservation struct {
	ID          string
	AmountCents int64
	Currency    string
}

type stripeTransferSnapshot struct {
	Transfers       map[string]stripeTransferObservation
	TotalMinorUnits int64
}

func compareStripePayoutTransfers(expected []StripePayoutTransferExpectation, actual stripeTransferSnapshot) error {
	expectedByRef := make(map[string]StripePayoutTransferExpectation, len(expected))
	for _, transfer := range expected {
		ref := strings.TrimSpace(transfer.TransferRef)
		if !validStripeObjectID(ref, "tr_") {
			return fmt.Errorf("Merc payout has invalid Stripe transfer reference %q", ref)
		}
		if _, exists := expectedByRef[ref]; exists {
			return fmt.Errorf("Merc payout references duplicate Stripe transfer %s", ref)
		}
		expectedByRef[ref] = transfer
	}

	actualIDs := make([]string, 0, len(actual.Transfers))
	for id := range actual.Transfers {
		actualIDs = append(actualIDs, id)
	}
	sort.Strings(actualIDs)
	for _, id := range actualIDs {
		providerTransfer := actual.Transfers[id]
		expectedTransfer, ok := expectedByRef[id]
		if !ok {
			return fmt.Errorf("Stripe transfer %s is not represented by a Merc payout operation", id)
		}
		if providerTransfer.AmountCents != expectedTransfer.SentCents ||
			!strings.EqualFold(providerTransfer.Currency, expectedTransfer.Currency) {
			return fmt.Errorf("Stripe transfer %s differs from Merc payout: provider=%d %s ledger=%d %s",
				id, providerTransfer.AmountCents, providerTransfer.Currency,
				expectedTransfer.SentCents, expectedTransfer.Currency)
		}
	}

	expectedIDs := make([]string, 0, len(expectedByRef))
	for id := range expectedByRef {
		expectedIDs = append(expectedIDs, id)
	}
	sort.Strings(expectedIDs)
	for _, id := range expectedIDs {
		if _, ok := actual.Transfers[id]; !ok {
			return fmt.Errorf("Merc payout transfer %s is absent from the Stripe account", id)
		}
	}
	return nil
}

func stripeTransferSnapshotUSD(snapshot stripeTransferSnapshot) (float64, error) {
	settlement, err := SettlementCurrency()
	if err != nil {
		return 0, err
	}
	totalMicros, err := settlement.MinorToMicros(snapshot.TotalMinorUnits)
	if err != nil {
		return 0, err
	}
	return microsToUSD(totalMicros), nil
}

func fetchStripeTransferSnapshot(ctx context.Context, acct string) (stripeTransferSnapshot, error) {
	acct = strings.TrimSpace(acct)
	if !validStripeObjectID(acct, "acct_") {
		return stripeTransferSnapshot{}, fmt.Errorf("stripe transfers: invalid connected account id")
	}
	if _, err := SettlementCurrency(); err != nil {
		return stripeTransferSnapshot{}, err
	}
	snapshot := stripeTransferSnapshot{Transfers: make(map[string]stripeTransferObservation)}
	startingAfter := ""
	for page := 0; page < reconcileMaxPages; page++ {
		params := url.Values{"destination": {acct}, "limit": {"100"}}
		if startingAfter != "" {
			params.Set("starting_after", startingAfter)
		}
		path := "transfers?" + params.Encode()
		out, err := stripeGet(ctx, path)
		if err != nil {
			return stripeTransferSnapshot{}, err
		}
		listObject, ok := out["object"].(string)
		if !ok || strings.TrimSpace(listObject) != "list" {
			return stripeTransferSnapshot{}, fmt.Errorf("stripe transfers: response is not a Stripe list object")
		}
		data, ok := out["data"].([]any)
		if !ok {
			return stripeTransferSnapshot{}, fmt.Errorf("stripe transfers: missing data array")
		}
		var lastID string
		for _, item := range data {
			t, ok := item.(map[string]any)
			if !ok {
				return stripeTransferSnapshot{}, fmt.Errorf("stripe transfers: malformed entry")
			}
			id, ok := t["id"].(string)
			id = strings.TrimSpace(id)
			if !ok || !validStripeObjectID(id, "tr_") {
				return stripeTransferSnapshot{}, fmt.Errorf("stripe transfers: entry has an invalid transfer id")
			}
			rawObject, present := t["object"]
			object, ok := rawObject.(string)
			if !present || !ok || object != "transfer" {
				return stripeTransferSnapshot{}, fmt.Errorf("stripe transfers: entry %s is not a transfer object", id)
			}
			rawDestination, present := t["destination"]
			destination, ok := rawDestination.(string)
			if !present || !ok || strings.TrimSpace(destination) != acct {
				return stripeTransferSnapshot{}, fmt.Errorf("stripe transfers: entry %s has an unexpected destination", id)
			}
			currency, _ := t["currency"].(string)
			currency = strings.ToLower(strings.TrimSpace(currency))
			if err := RequireSettlementCurrency(currency); err != nil {
				return stripeTransferSnapshot{}, fmt.Errorf("stripe transfers: entry currency refused: %w", err)
			}
			amt, err := stripeIntegerField(t, "amount")
			if err != nil {
				return stripeTransferSnapshot{}, fmt.Errorf("stripe transfers: %w", err)
			}
			if amt <= 0 {
				return stripeTransferSnapshot{}, fmt.Errorf("stripe transfers: transfer %s has a non-positive amount", id)
			}
			if _, exists := snapshot.Transfers[id]; exists {
				return stripeTransferSnapshot{}, fmt.Errorf("stripe transfers: duplicate transfer %s across pages", id)
			}
			if amt > math.MaxInt64-snapshot.TotalMinorUnits {
				return stripeTransferSnapshot{}, fmt.Errorf("stripe transfers: amount total overflows settlement range")
			}
			snapshot.TotalMinorUnits += amt
			snapshot.Transfers[id] = stripeTransferObservation{
				ID: id, AmountCents: amt, Currency: currency,
			}
			lastID = id
		}
		hasMore, ok := out["has_more"].(bool)
		if !ok {
			return stripeTransferSnapshot{}, fmt.Errorf("stripe transfers: missing has_more flag")
		}
		if !hasMore {
			break
		}
		if lastID == "" {
			return stripeTransferSnapshot{}, fmt.Errorf("stripe transfers: has_more response has no transfer cursor")
		}
		if lastID == startingAfter {
			return stripeTransferSnapshot{}, fmt.Errorf("stripe transfers: pagination cursor did not advance")
		}
		if page == reconcileMaxPages-1 {
			return stripeTransferSnapshot{}, fmt.Errorf("stripe transfers: response exceeded %d pages", reconcileMaxPages)
		}
		startingAfter = lastID // cursor onto the next page
	}
	return snapshot, nil
}

func stripeTransferredUSD(ctx context.Context, acct string) (float64, error) {
	snapshot, err := fetchStripeTransferSnapshot(ctx, acct)
	if err != nil {
		return 0, err
	}
	return stripeTransferSnapshotUSD(snapshot)
}

const reconcileMaxPages = 100
