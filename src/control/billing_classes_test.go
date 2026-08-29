package main

import (
	"math"
	"testing"
)

// The retired 145x claim was a measurement expressing reuse as inference. These
// tests keep the same conflation out of the money path.

func TestPhysicalAndDeliveredAreSeparate(t *testing.T) {
	// The retired benchmark profile, expressed as billing classes:
	// batch 128, 192-token prompts of which 160 shared, 32 output.
	acct := TokenAccounting{
		ClassUncachedInput:     128 * 32, // unique suffix, actually prefilled
		ClassPrefixReusedInput: 128*160 - 160,
		ClassGeneratedOutput:   128 * 32,
	}
	// The shared prefix itself was computed once and belongs to physical work.
	acct[ClassUncachedInput] += 160

	if got, want := acct.PhysicalTokens(), int64(8352); got != want {
		t.Fatalf("physical tokens = %d, want %d", got, want)
	}
	if got, want := acct.DeliveredTokens(), int64(28672); got != want {
		t.Fatalf("delivered tokens = %d, want %d", got, want)
	}
	if r := acct.ReuseRatio(); math.Abs(r-3.43) > 0.01 {
		t.Fatalf("reuse ratio = %.2f, want ~3.43", r)
	}
	if acct.PhysicalTokens() >= acct.DeliveredTokens() {
		t.Fatal("physical must be below delivered whenever anything is reused")
	}
}

func TestWithoutReusePhysicalEqualsDelivered(t *testing.T) {
	acct := TokenAccounting{
		ClassUncachedInput:   4096,
		ClassGeneratedOutput: 1024,
	}
	if acct.PhysicalTokens() != acct.DeliveredTokens() {
		t.Fatalf("no reuse: physical %d != delivered %d",
			acct.PhysicalTokens(), acct.DeliveredTokens())
	}
	if r := acct.ReuseRatio(); math.Abs(r-1.0) > 1e-9 {
		t.Fatalf("no reuse should give ratio 1.0, got %.4f", r)
	}
}

// Reused tokens are cheaper for the buyer but never free: merc still stores,
// looks up, delivers and verifies them.
func TestReusedTokensAreDiscountedButNotFree(t *testing.T) {
	const full = 0.000036
	const tokens = 100_000

	physical := float64(tokens) / 1000.0 * full
	reused := PriceReusedTokens(full, tokens)

	if reused <= 0 {
		t.Fatal("reused tokens priced at zero; merc still bears storage, lookup and delivery cost")
	}
	if reused >= physical {
		t.Fatalf("reuse must be cheaper than fresh compute: reused %.8f >= physical %.8f",
			reused, physical)
	}
	saving := (physical - reused) / physical
	if math.Abs(saving-reuseDiscountShare) > 1e-9 {
		t.Fatalf("buyer saving %.4f does not match the declared share %.4f",
			saving, reuseDiscountShare)
	}
	// The efficiency gain must be shared, not wholly given away.
	if reuseDiscountShare >= 1.0 {
		t.Fatal("passing 100% of the saving leaves nothing for supplier or merc")
	}
	if reuseDiscountShare <= 0 {
		t.Fatal("passing none of the saving means the buyer never sees the efficiency")
	}
}

func TestPriceAccountingChargesEachClassCorrectly(t *testing.T) {
	const full = 0.000036
	acct := TokenAccounting{
		ClassUncachedInput:     10_000,
		ClassGeneratedOutput:   10_000,
		ClassPrefixReusedInput: 80_000,
	}
	got := PriceAccounting(acct, full)

	want := 20_000/1000.0*full + PriceReusedTokens(full, 80_000)
	if math.Abs(got-roundUSD(want)) > 1e-9 {
		t.Fatalf("priced %.10f, want %.10f", got, want)
	}

	// Charging every delivered token at the full rate would overcharge for work
	// nobody performed. Confirm we are materially below that.
	naive := float64(acct.DeliveredTokens()) / 1000.0 * full
	if got >= naive {
		t.Fatalf("reuse did not reduce the bill: %.10f >= %.10f", got, naive)
	}
}

func TestUnknownClassIsRefused(t *testing.T) {
	ctx, store, pool := openPayoutTestStore(t)
	f := seedPayoutFixture(t, ctx, pool, payoutFixtureOpts{creditUSD: 1.00})

	err := store.RecordTokenAccounting(ctx, f.jobID, TokenAccounting{"made_up_class": 10})
	if err == nil {
		t.Fatal("an unknown billing class must be refused, not silently stored")
	}
	if err := store.RecordTokenAccounting(ctx, f.jobID,
		TokenAccounting{ClassGeneratedOutput: -5}); err == nil {
		t.Fatal("a negative token count must be refused")
	}
}

func TestTokenAccountingRoundTrips(t *testing.T) {
	ctx, store, pool := openPayoutTestStore(t)
	f := seedPayoutFixture(t, ctx, pool, payoutFixtureOpts{creditUSD: 1.00})

	want := TokenAccounting{
		ClassUncachedInput:     1234,
		ClassPrefixReusedInput: 5678,
		ClassGeneratedOutput:   910,
		ClassExactResultReuse:  11,
	}
	mustf(t, store.RecordTokenAccounting(ctx, f.jobID, want), "record: %v")
	got, err := store.JobTokenAccounting(ctx, f.jobID)
	mustf(t, err, "read back: %v")
	for class, n := range want {
		if got[class] != n {
			t.Fatalf("class %s: got %d want %d", class, got[class], n)
		}
	}
	if got.PhysicalTokens() != 1234+910 {
		t.Fatalf("physical from stored rows = %d, want %d", got.PhysicalTokens(), 1234+910)
	}

	// Re-recording must overwrite rather than accumulate.
	mustf(t, store.RecordTokenAccounting(ctx, f.jobID, TokenAccounting{ClassUncachedInput: 1}), "re-record: %v")
	again, err := store.JobTokenAccounting(ctx, f.jobID)
	mustf(t, err, "re-read: %v")
	if again[ClassUncachedInput] != 1 {
		t.Fatalf("re-record accumulated instead of replacing: %d", again[ClassUncachedInput])
	}
}
