package main

import (
	"testing"

	"github.com/google/uuid"
)

// BuyerFreeCreditRemaining had no test, and a KILL-RT edit removed a term from
// its GREATEST(...) list along with the comma separating the final argument.
// The result was invalid SQL that shipped with `make ci` green, because nothing
// ever executed the query. This test executes it.
func TestBuyerFreeCreditRemainingIsValidSQL(t *testing.T) {
	ctx, store, pool := openPayoutTestStore(t)

	buyer := uuid.New()
	if _, err := pool.Exec(ctx,
		`INSERT INTO buyers (id,email,password_hash,free_credit_usd)
		 VALUES ($1,$2,'x',5.00) ON CONFLICT (id) DO NOTHING`,
		buyer, buyer.String()+"@credit.invalid"); err != nil {
		t.Fatalf("seed buyer: %v", err)
	}

	remaining, err := store.BuyerFreeCreditRemaining(ctx, buyer)
	mustf(t, err, "BuyerFreeCreditRemaining returned an error -- the query does not execute: %v")
	if remaining != 5.00 {
		t.Fatalf("a buyer with $5.00 credit and no spend has %.2f remaining", remaining)
	}

	// An unknown buyer is zero, not an error.
	if r, err := store.BuyerFreeCreditRemaining(ctx, uuid.New()); err != nil || r != 0 {
		t.Fatalf("unknown buyer: remaining=%v err=%v", r, err)
	}

	// Credit never goes negative: GREATEST(..., 0) is the reason the missing
	// comma mattered.
	if _, err := pool.Exec(ctx,
		`INSERT INTO jobs (id,buyer_id,status,job_type,input_ref,charge_status,estimated_usd)
		 VALUES ($1,$2,'queued','embed','x','not_attempted',999.00)`,
		uuid.New(), buyer); err != nil {
		t.Fatalf("seed oversized job: %v", err)
	}
	r, err := store.BuyerFreeCreditRemaining(ctx, buyer)
	mustf(t, err, "query failed with a large reservation: %v")
	if r < 0 {
		t.Fatalf("free credit went negative: %v", r)
	}
}
