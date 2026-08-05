package main

import (
	"testing"

	"github.com/google/uuid"
)

// A $0.50 charge hands Stripe 0.029*0.50 + 0.30 = $0.3145, i.e. 62.9%, while
// the buyer price floor was computed assuming 2.9% + 0.30/5.00 = 8.9%. The 24h
// age branch used to fire at exactly that amount, so every age-triggered charge
// lost roughly $0.27 the economic plan had already allocated elsewhere.

func TestStripeMinimumIsNotAnEconomicThreshold(t *testing.T) {
	// The API floor and the economic floor are different numbers and must not
	// be confused: at the API floor the processor takes most of the charge.
	if stripeMinChargeUSD >= defaultChargeMinUSD {
		t.Fatalf("stripeMinChargeUSD (%v) should be far below the economic threshold (%v)",
			stripeMinChargeUSD, defaultChargeMinUSD)
	}
	atAPIFloor := (0.029*stripeMinChargeUSD + 0.30) / stripeMinChargeUSD
	atEconomicFloor := (0.029*defaultChargeMinUSD + 0.30) / defaultChargeMinUSD
	if atAPIFloor < 0.5 {
		t.Fatalf("expected the API floor to be ruinous, got %.1f%%", atAPIFloor*100)
	}
	if atEconomicFloor > 0.10 {
		t.Fatalf("economic floor should keep the processor near 8.9%%, got %.1f%%", atEconomicFloor*100)
	}
}

// The regression: a buyer sitting on a small balance must NOT be charged when
// the batch merely gets old.
func TestAgedSubThresholdBalanceIsNotCharged(t *testing.T) {
	ctx, store, pool := openIsolatedTestStore(t)

	buyer := uuid.New()
	if _, err := pool.Exec(ctx,
		`INSERT INTO buyers (id,email,password_hash,free_credit_usd)
		 VALUES ($1,$2,'x',0) ON CONFLICT (id) DO NOTHING`,
		buyer, buyer.String()+"@threshold.invalid"); err != nil {
		t.Fatalf("seed buyer: %v", err)
	}
	// $0.60 of deferred work, deferred well beyond the 24h age trigger: above
	// Stripe's $0.50 API minimum, far below the $5.00 economic threshold.
	if _, err := pool.Exec(ctx,
		`INSERT INTO jobs (id,buyer_id,status,job_type,input_ref,charge_status,actual_usd,currency,deferred_at,terminal_at)
		 VALUES ($1,$2,'complete','embed','t/i','deferred',0.60,$3,now() - interval '30 days',now())`,
		uuid.New(), buyer, SettlementCurrencyCode()); err != nil {
		t.Fatalf("seed aged deferred job: %v", err)
	}

	due, err := store.BuyersDueForBatch(ctx, defaultChargeMinUSD, chargeBatchMaxAge, 100)
	mustf(t, err, "BuyersDueForBatch: %v")
	for _, id := range due {
		if id == buyer {
			t.Fatalf("a $0.60 balance was charged on age alone; Stripe would keep %.1f%% of it",
				(0.029*0.60+0.30)/0.60*100)
		}
	}
}

// The threshold must not become a black hole either: once the balance is worth
// collecting, ageing still selects it.
func TestBalanceAtThresholdIsCharged(t *testing.T) {
	ctx, store, pool := openIsolatedTestStore(t)

	buyer := uuid.New()
	if _, err := pool.Exec(ctx,
		`INSERT INTO buyers (id,email,password_hash,free_credit_usd)
		 VALUES ($1,$2,'x',0) ON CONFLICT (id) DO NOTHING`,
		buyer, buyer.String()+"@threshold.invalid"); err != nil {
		t.Fatalf("seed buyer: %v", err)
	}
	if _, err := pool.Exec(ctx,
		`INSERT INTO jobs (id,buyer_id,status,job_type,input_ref,charge_status,actual_usd,currency,deferred_at,terminal_at)
		 VALUES ($1,$2,'complete','embed','t/i','deferred',$3,$4,now() - interval '30 days',now())`,
		uuid.New(), buyer, defaultChargeMinUSD, SettlementCurrencyCode()); err != nil {
		t.Fatalf("seed at-threshold job: %v", err)
	}

	due, err := store.BuyersDueForBatch(ctx, defaultChargeMinUSD, chargeBatchMaxAge, 100)
	mustf(t, err, "BuyersDueForBatch: %v")
	found := false
	for _, id := range due {
		if id == buyer {
			found = true
		}
	}
	if !found {
		t.Fatalf("a balance at the $%.2f threshold should be collected", defaultChargeMinUSD)
	}
}
