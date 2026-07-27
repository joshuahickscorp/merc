package main

import (
	"sync"
	"testing"

	"github.com/google/uuid"
)

// The defect this file exists for: supplier credits at catalogue prices are
// worth a fraction of a cent. Flooring each entry independently produced zero
// cents every time, moved the entry to payout_status='carried', and nothing
// ever moved it back -- so supplier lifetime cash was $0.00 by construction.
//
// These tests assert the money is now conserved and eventually paid.

// creditUSD below one cent: 0.004 USD = 4000 micro-USD = 0.4 of a cent.
const subCentCreditUSD = 0.004

func TestSubCentCreditsAccrueAndEventuallyPay(t *testing.T) {
	ctx, store, pool := openPayoutTestStore(t)
	t.Setenv("MERC_CANARY_MODE", "false")
	t.Setenv("MERC_CANARY_DISABLE_DECISION_REF", "TEST-accrual")

	supplier := uuid.New()

	// 0.4 cent each: the first two must carry, the third crosses one cent.
	var paidCents int64
	for i := 1; i <= 3; i++ {
		f := seedPayoutFixture(t, ctx, pool, payoutFixtureOpts{
			creditUSD:  subCentCreditUSD,
			supplierID: supplier,
		})
		claimed, ok, err := store.ClaimPayout(ctx, f.entryID)
		if err != nil {
			t.Fatalf("claim %d: %v", i, err)
		}
		paidCents += claimed.RequestedCents

		switch i {
		case 1, 2:
			if ok || claimed.RequestedCents != 0 {
				t.Fatalf("entry %d: sub-cent credit should carry, got ok=%v cents=%d",
					i, ok, claimed.RequestedCents)
			}
		case 3:
			// 3 x 4000 = 12000 micro-USD = 1 cent, 2000 carried forward.
			if claimed.RequestedCents != 1 {
				t.Fatalf("entry 3 should pay the accumulated cent, got %d cents (ok=%v)",
					claimed.RequestedCents, ok)
			}
		}
	}
	if paidCents != 1 {
		t.Fatalf("three 0.4-cent credits should pay exactly 1 cent, paid %d", paidCents)
	}

	acc, err := store.SupplierAccrual(ctx, supplier)
	if err != nil {
		t.Fatalf("read accrual: %v", err)
	}
	// Nothing invented, nothing lost.
	if acc.LifetimeAbsorbed != 3*usdToMicros(subCentCreditUSD) {
		t.Fatalf("absorbed %d micro-USD, want %d", acc.LifetimeAbsorbed, 3*usdToMicros(subCentCreditUSD))
	}
	if acc.LifetimeAbsorbed != acc.LifetimePaidCent*microUSDPerCent+acc.AccruedMicros {
		t.Fatalf("accrual does not balance: absorbed=%d paid=%d accrued=%d",
			acc.LifetimeAbsorbed, acc.LifetimePaidCent, acc.AccruedMicros)
	}
	if acc.AccruedMicros != 2000 {
		t.Fatalf("carry forward should be 2000 micro-USD, got %d", acc.AccruedMicros)
	}
}

// Money conservation across a long run of awkward amounts: whatever is absorbed
// is either paid in whole cents or still carried, never lost and never invented.
func TestAccrualConservesEveryMicroUSD(t *testing.T) {
	ctx, store, pool := openPayoutTestStore(t)
	t.Setenv("MERC_CANARY_MODE", "false")
	t.Setenv("MERC_CANARY_DISABLE_DECISION_REF", "TEST-accrual")

	supplier := uuid.New()
	amounts := []float64{0.004, 0.0071, 0.0003, 0.019, 0.0009, 0.0454, 0.0002}

	var absorbed, paid int64
	for i, usd := range amounts {
		f := seedPayoutFixture(t, ctx, pool, payoutFixtureOpts{
			creditUSD:  usd,
			supplierID: supplier,
		})
		claimed, _, err := store.ClaimPayout(ctx, f.entryID)
		if err != nil {
			t.Fatalf("claim %d (%v USD): %v", i, usd, err)
		}
		absorbed += usdToMicros(usd)
		paid += claimed.RequestedCents
	}

	acc, err := store.SupplierAccrual(ctx, supplier)
	if err != nil {
		t.Fatalf("read accrual: %v", err)
	}
	if acc.LifetimeAbsorbed != absorbed {
		t.Fatalf("absorbed %d, expected %d", acc.LifetimeAbsorbed, absorbed)
	}
	if acc.LifetimePaidCent != paid {
		t.Fatalf("accrual recorded %d cents paid, claims returned %d", acc.LifetimePaidCent, paid)
	}
	if got := paid*microUSDPerCent + acc.AccruedMicros; got != absorbed {
		t.Fatalf("micro-USD lost or invented: paid %d cents + carry %d = %d, absorbed %d",
			paid, acc.AccruedMicros, got, absorbed)
	}
	if acc.AccruedMicros >= microUSDPerCent {
		t.Fatalf("carry %d should always be drained below one cent", acc.AccruedMicros)
	}
	if paid == 0 {
		t.Fatal("a supplier earning real work was still paid nothing")
	}
}

// Claiming the same entry twice must not absorb its liability twice.
func TestAccrualIsIdempotentPerEntry(t *testing.T) {
	ctx, store, pool := openPayoutTestStore(t)
	t.Setenv("MERC_CANARY_MODE", "false")
	t.Setenv("MERC_CANARY_DISABLE_DECISION_REF", "TEST-accrual")

	supplier := uuid.New()
	f := seedPayoutFixture(t, ctx, pool, payoutFixtureOpts{
		creditUSD:  subCentCreditUSD,
		supplierID: supplier,
	})
	if _, _, err := store.ClaimPayout(ctx, f.entryID); err != nil {
		t.Fatalf("first claim: %v", err)
	}
	first, err := store.SupplierAccrual(ctx, supplier)
	if err != nil {
		t.Fatalf("read accrual: %v", err)
	}
	// A carried entry is no longer 'held', so a replay is refused upstream; the
	// accrual must be unchanged either way.
	_, _, _ = store.ClaimPayout(ctx, f.entryID)
	second, err := store.SupplierAccrual(ctx, supplier)
	if err != nil {
		t.Fatalf("re-read accrual: %v", err)
	}
	if first != second {
		t.Fatalf("replaying a claim changed the accrual: %+v -> %+v", first, second)
	}
}

// A credit that already exceeds a cent must still pay immediately -- the
// accrual must not delay money that was always payable.
func TestWholeCentCreditPaysImmediately(t *testing.T) {
	ctx, store, pool := openPayoutTestStore(t)
	t.Setenv("MERC_CANARY_MODE", "false")
	t.Setenv("MERC_CANARY_DISABLE_DECISION_REF", "TEST-accrual")

	f := seedPayoutFixture(t, ctx, pool, payoutFixtureOpts{creditUSD: 1.00})
	claimed, ok, err := store.ClaimPayout(ctx, f.entryID)
	if err != nil || !ok {
		t.Fatalf("claim: ok=%v err=%v", ok, err)
	}
	if claimed.RequestedCents != 100 {
		t.Fatalf("$1.00 credit should request 100 cents, got %d", claimed.RequestedCents)
	}
}

// Concurrency and long-run conservation.
//
// The accrual is read-modify-written under a row lock. If that lock is wrong,
// two concurrent claims can both read the same carry and both spend it, minting
// money that was never earned. Money bugs under load are the expensive kind, so
// this drives real goroutines at the real database.

func TestAccrualConservesMoneyUnderConcurrentClaims(t *testing.T) {
	ctx, store, pool := openPayoutTestStore(t)
	t.Setenv("MERC_CANARY_MODE", "false")
	t.Setenv("MERC_CANARY_DISABLE_DECISION_REF", "TEST-accrual")

	supplier := uuid.New()

	// Sub-cent credits, so almost every claim exercises the carry path.
	const entries = 24
	fixtures := make([]payoutFixture, entries)
	var expectedMicros int64
	for i := range fixtures {
		usd := 0.001 * float64(1+i%7) // 0.001..0.007
		fixtures[i] = seedPayoutFixture(t, ctx, pool, payoutFixtureOpts{
			creditUSD: usd, supplierID: supplier,
		})
		expectedMicros += usdToMicros(usd)
	}

	var wg sync.WaitGroup
	claimed := make(chan int64, entries)
	errs := make(chan error, entries)
	for _, f := range fixtures {
		wg.Add(1)
		go func(entryID uuid.UUID) {
			defer wg.Done()
			out, _, err := store.ClaimPayout(ctx, entryID)
			if err != nil {
				errs <- err
				return
			}
			claimed <- out.RequestedCents
		}(f.entryID)
	}
	wg.Wait()
	close(claimed)
	close(errs)

	for err := range errs {
		t.Fatalf("concurrent claim failed: %v", err)
	}
	var paidCents int64
	for c := range claimed {
		paidCents += c
	}

	acc, err := store.SupplierAccrual(ctx, supplier)
	if err != nil {
		t.Fatalf("read accrual: %v", err)
	}

	// The invariant that matters: nothing minted, nothing lost.
	if acc.LifetimeAbsorbed != expectedMicros {
		t.Fatalf("absorbed %d micro-USD, expected %d (money invented or lost under concurrency)",
			acc.LifetimeAbsorbed, expectedMicros)
	}
	if got := acc.LifetimePaidCent*microUSDPerCent + acc.AccruedMicros; got != expectedMicros {
		t.Fatalf("paid %d cents + carry %d != absorbed %d",
			acc.LifetimePaidCent, acc.AccruedMicros, expectedMicros)
	}
	if paidCents != acc.LifetimePaidCent {
		t.Fatalf("claims reported %d cents but the accrual recorded %d",
			paidCents, acc.LifetimePaidCent)
	}
	if acc.AccruedMicros >= microUSDPerCent {
		t.Fatalf("carry %d was not drained below one cent", acc.AccruedMicros)
	}
	t.Logf("concurrent accrual: %d entries, %d micro-USD absorbed, %d cents paid, %d carried",
		entries, acc.LifetimeAbsorbed, acc.LifetimePaidCent, acc.AccruedMicros)
}

// A long run of awkward amounts must not drift. Integer micro-USD should make
// this exact, and this asserts it over enough entries that a rounding bug
// would accumulate visibly.
func TestAccrualHasNoDriftOverALongRun(t *testing.T) {
	ctx, store, pool := openPayoutTestStore(t)
	t.Setenv("MERC_CANARY_MODE", "false")
	t.Setenv("MERC_CANARY_DISABLE_DECISION_REF", "TEST-accrual")

	supplier := uuid.New()
	amounts := []float64{
		0.000001, 0.000007, 0.00013, 0.0031, 0.00777, 0.0001, 0.009999,
		0.000003, 0.00025, 0.0042, 0.000019, 0.00811, 0.0000004, 0.00333,
	}
	var expected int64
	for round := 0; round < 4; round++ {
		for _, usd := range amounts {
			f := seedPayoutFixture(t, ctx, pool, payoutFixtureOpts{
				creditUSD: usd, supplierID: supplier,
			})
			if _, _, err := store.ClaimPayout(ctx, f.entryID); err != nil {
				t.Fatalf("claim %v: %v", usd, err)
			}
			expected += usdToMicros(usd)
		}
	}

	acc, err := store.SupplierAccrual(ctx, supplier)
	if err != nil {
		t.Fatalf("read accrual: %v", err)
	}
	if acc.LifetimeAbsorbed != expected {
		t.Fatalf("drift after %d entries: absorbed %d, expected %d",
			len(amounts)*4, acc.LifetimeAbsorbed, expected)
	}
	if got := acc.LifetimePaidCent*microUSDPerCent + acc.AccruedMicros; got != expected {
		t.Fatalf("ledger does not balance after a long run: %d != %d", got, expected)
	}
	t.Logf("long run: %d entries, %d micro-USD absorbed exactly, %d cents paid",
		len(amounts)*4, acc.LifetimeAbsorbed, acc.LifetimePaidCent)
}
