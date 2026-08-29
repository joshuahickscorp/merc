package main

import (
	"context"
	"fmt"
	"log"
	"math"
	"sort"
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

	type supplierCashExpectation struct {
		cashSentUSD         float64
		liabilityUSD        float64
		carriedUSD          float64
		releasedWithoutCash int
		outcomeUnknown      int
	}
	expected := make(map[uuid.UUID]*supplierCashExpectation)
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
	suppliers := make([]uuid.UUID, 0, len(expected))
	for supplierID, e := range expected {
		if e.cashSentUSD > 0 || e.releasedWithoutCash > 0 || e.outcomeUnknown > 0 {
			suppliers = append(suppliers, supplierID)
		}
	}
	sort.Slice(suppliers, func(i, j int) bool { return suppliers[i].String() < suppliers[j].String() })

	var checked, drifted int
	for _, supplierID := range suppliers {
		e := expected[supplierID]
		acct, aerr := wk.store.SupplierStripeAcct(ctx, supplierID)
		if aerr != nil {
			log.Printf("workers: reconcile: supplier %s stripe account lookup: %v", supplierID, aerr)
			continue
		}
		if acct == "" {
			log.Printf("workers: reconcile DRIFT: supplier %s shows %s %.6f cash sent (%.6f liability, %.6f carried) but has no connected Stripe account",
				supplierID, SettlementCurrencyCode(), e.cashSentUSD, e.liabilityUSD, e.carriedUSD)
			drifted++
			metrics.reconcileDrift.Add(1)
			continue
		}
		if e.releasedWithoutCash > 0 {
			log.Printf("workers: reconcile DRIFT: supplier %s (%s) has %d released liability row(s) without a cash-moved payout operation (rollup liability %s %.6f)",
				supplierID, acct, e.releasedWithoutCash, SettlementCurrencyCode(), e.liabilityUSD)
			drifted++
			metrics.reconcileDrift.Add(1)
		}
		if e.outcomeUnknown > 0 {
			log.Printf("workers: reconcile DRIFT: supplier %s (%s) has %d unresolved provider outcome(s); possible cash must be resolved by exact payout key",
				supplierID, acct, e.outcomeUnknown)
			drifted++
			metrics.reconcileDrift.Add(1)
		}
		if e.cashSentUSD <= 0 {
			continue
		}
		transferred, terr := stripeTransferredUSD(ctx, acct)
		if terr != nil {
			log.Printf("workers: reconcile: supplier %s (%s) stripe transfers: %v", supplierID, acct, terr)
			continue
		}
		checked++
		if delta := e.cashSentUSD - transferred; math.Abs(delta) >= reconcileEpsilonMajor() {
			log.Printf("workers: reconcile DRIFT: supplier %s (%s): ledger cash sent %s %.6f vs stripe transferred %s %.6f (delta %s %.6f; liability %.6f, carried %.6f)",
				supplierID, acct, SettlementCurrencyCode(), e.cashSentUSD, SettlementCurrencyCode(), transferred,
				SettlementCurrencyCode(), delta, e.liabilityUSD, e.carriedUSD)
			drifted++
			metrics.reconcileDrift.Add(1)
		}
	}
	log.Printf("workers: reconcile complete  -  %d supplier(s) checked, %d with drift", checked, drifted)
	return nil
}

func stripeTransferredUSD(ctx context.Context, acct string) (float64, error) {
	settlement, err := SettlementCurrency()
	if err != nil {
		return 0, err
	}
	var totalMinorUnits int64
	startingAfter := ""
	for page := 0; page < reconcileMaxPages; page++ {
		path := fmt.Sprintf("transfers?destination=%s&limit=100", acct)
		if startingAfter != "" {
			path += "&starting_after=" + startingAfter
		}
		out, err := stripeGet(ctx, path)
		if err != nil {
			return 0, err
		}
		data, ok := out["data"].([]any)
		if !ok {
			return 0, fmt.Errorf("stripe transfers: missing data array")
		}
		var lastID string
		for _, item := range data {
			t, ok := item.(map[string]any)
			if !ok {
				return 0, fmt.Errorf("stripe transfers: malformed entry")
			}
			currency, _ := t["currency"].(string)
			if err := RequireSettlementCurrency(currency); err != nil {
				return 0, fmt.Errorf("stripe transfers: entry currency refused: %w", err)
			}
			amt, err := stripeIntegerField(t, "amount")
			if err != nil {
				return 0, fmt.Errorf("stripe transfers: %w", err)
			}
			if amt > math.MaxInt64-totalMinorUnits {
				return 0, fmt.Errorf("stripe transfers: amount total overflows settlement range")
			}
			totalMinorUnits += amt
			if id, ok := t["id"].(string); ok {
				lastID = id
			}
		}
		hasMore, _ := out["has_more"].(bool)
		if !hasMore || lastID == "" {
			break
		}
		startingAfter = lastID // cursor onto the next page
	}
	totalMicros, err := settlement.MinorToMicros(totalMinorUnits)
	if err != nil {
		return 0, err
	}
	return microsToUSD(totalMicros), nil
}

const reconcileMaxPages = 100
