package main

import (
	"testing"

	"github.com/google/uuid"
)

// WorkerEarnings.CarriedUSD is what a supplier is told is still owed but not
// yet payable. Under account-level accrual each settlement row records the
// carry AFTER that entry -- a running balance, not an independent remainder.
// Summing them across entries counts the same money repeatedly.
func TestEarningsCarryMatchesTheAuthoritativeAccrual(t *testing.T) {
	ctx, store, pool := openPayoutTestStore(t)
	t.Setenv("MERC_CANARY_MODE", "false")
	t.Setenv("MERC_CANARY_DISABLE_DECISION_REF", "TEST-earnings")

	supplier := uuid.New()
	// Five sub-cent credits: every one exercises the carry path.
	for i := 0; i < 5; i++ {
		f := seedPayoutFixture(t, ctx, pool, payoutFixtureOpts{
			creditUSD: 0.004, supplierID: supplier,
		})
		if _, _, err := store.ClaimPayout(ctx, f.entryID); err != nil {
			t.Fatalf("claim %d: %v", i, err)
		}
	}

	acc, err := store.SupplierAccrual(ctx, supplier)
	mustf(t, err, "read accrual: %v")
	earnings, err := store.WorkerEarnings(ctx, supplier)
	mustf(t, err, "WorkerEarnings: %v")

	wantCarried := float64(acc.AccruedMicros) / 1_000_000.0
	if diff := earnings.CarriedUSD - wantCarried; diff > 1e-9 || diff < -1e-9 {
		t.Fatalf("earnings reports $%.6f carried, the accrual holds $%.6f "+
			"(difference $%.6f): the supplier is being shown a balance that does not exist",
			earnings.CarriedUSD, wantCarried, diff)
	}
	// Carry can never exceed one cent, by definition of the accrual.
	if earnings.CarriedUSD >= 0.01 {
		t.Fatalf("carried $%.6f is at or above one cent; it should have been paid out",
			earnings.CarriedUSD)
	}
}
