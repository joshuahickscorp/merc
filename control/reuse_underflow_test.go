package main

import "testing"

// A reuse hit is billed at a discount off the full rate, which means on a small
// result the discounted charge underflows micro-USD to zero. The settle path
// then refuses it -- correctly, because reuse is cheaper and never free -- and
// the caller falls back to executing live at FULL price.
//
// The failure is silent and self-defeating: the cache is hit, the hit counter
// increments, and the buyer is charged the undiscounted amount anyway. Observed
// against a live merc before this floor existed: two identical 2-token embed
// jobs, the second one hit the cache and still paid the same $0.000125, and the
// only trace was one log line.
func TestReuseHitAlwaysBillsAtLeastOneMicroUSD(t *testing.T) {
	// Prices low enough that the reuse discount underflows, taken from the
	// catalogue rate that actually produced the live failure.
	for _, tc := range []struct {
		name           string
		tokens         int64
		fullPricePer1K float64
	}{
		{"two tokens at the embed rate", 2, 0.000018},
		{"one token at the embed rate", 1, 0.000018},
		{"small result, very low rate", 4, 0.000001},
		{"normal result", 1000, 0.002},
	} {
		t.Run(tc.name, func(t *testing.T) {
			money := SettleReuseHitMoney(tc.tokens, tc.fullPricePer1K)

			if money.BuyerDebitMicros <= 0 {
				t.Fatalf("reuse hit for %d tokens bills %d micro-USD; the settle path "+
					"refuses a zero charge and the caller silently re-executes at full "+
					"price", tc.tokens, money.BuyerDebitMicros)
			}
			// Reuse must never credit a supplier: nobody performed the work.
			if money.SupplierLiabilityMicros != 0 {
				t.Fatalf("reuse credited a supplier %d micro-USD for work nobody did",
					money.SupplierLiabilityMicros)
			}
			// The floor must not break conservation.
			if !money.Conserved() {
				t.Fatalf("reuse settlement not conserved: buyer=%d supplier=%d platform=%d",
					money.BuyerDebitMicros, money.SupplierLiabilityMicros, money.PlatformMicros)
			}
			// Reuse is a discount, so it must never exceed what full rate would
			// have cost. A floor that overcharged would be worse than the bug.
			full := usdToMicros(float64(tc.tokens) / 1000.0 * tc.fullPricePer1K)
			if full > 0 && money.BuyerDebitMicros > full {
				t.Fatalf("reuse charge %d exceeds the full-rate charge %d for %d tokens",
					money.BuyerDebitMicros, full, tc.tokens)
			}
		})
	}
}

// A hit that delivered nothing is not a hit worth billing.
func TestReuseHitWithNoTokensBillsNothing(t *testing.T) {
	money := SettleReuseHitMoney(0, 0.002)
	if money.BuyerDebitMicros != 0 {
		t.Fatalf("a zero-token reuse hit billed %d micro-USD", money.BuyerDebitMicros)
	}
	if !money.Conserved() {
		t.Fatal("zero-token reuse settlement is not conserved")
	}
}
