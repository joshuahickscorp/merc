package main

import (
	"testing"
)

// The claim predicate defers a task when a CHEAPER class could also take it, so
// this ranking decides where work lands and therefore what merc pays for it.
//
// These assertions exist because the ranking was wrong in the expensive
// direction: the four CUDA classes were absent and fell through to ELSE 0, which
// ranked a rented A100 as cheap as a base Mac. An owned apple_silicon_ultra
// (rank 3) then saw a "cheaper" rented nvidia_80gb (rank 0) and deferred to it,
// so owned hardware stepped back for rented hardware on every claim.
func TestHardwareCostRankOrdersOwnedBeforeRented(t *testing.T) {
	ctx, store, _ := openAdminMutationTestStore(t)

	rank := func(class string) int {
		var got int
		// Evaluate the real SQL rather than a Go copy of it. A test that
		// reimplements the CASE would keep passing while the query it is meant
		// to protect drifted.
		q := "SELECT " + hwClassCostRankSQL("$1::text")
		if err := store.pool.QueryRow(ctx, q, class).Scan(&got); err != nil {
			t.Fatalf("rank(%q): %v", class, err)
		}
		return got
	}

	owned := []string{
		"apple_silicon_base", "apple_silicon_pro",
		"apple_silicon_max", "apple_silicon_ultra",
	}
	rented := []string{"nvidia_24gb", "nvidia_48gb", "nvidia_80gb", "nvidia_180gb"}

	// Every owned class must rank cheaper than every rented one. Marginal cost
	// of a Mac merc already has is electricity; a rented GPU bills by the hour
	// whether or not it is busy.
	for _, o := range owned {
		for _, r := range rented {
			if rank(o) >= rank(r) {
				t.Fatalf("owned %s (rank %d) is not cheaper than rented %s (rank %d): "+
					"owned hardware would defer to rented hardware",
					o, rank(o), r, rank(r))
			}
		}
	}

	// Within each family, bigger is more expensive.
	for _, family := range [][]string{owned, rented} {
		for i := 1; i < len(family); i++ {
			if rank(family[i]) <= rank(family[i-1]) {
				t.Fatalf("%s (rank %d) does not rank above %s (rank %d)",
					family[i], rank(family[i]), family[i-1], rank(family[i-1]))
			}
		}
	}

	// An unrecognised class must rank LAST, not first. Treating an unknown class
	// as free is exactly the defect above; what cannot be priced must be assumed
	// expensive so it is never preferred by default.
	unknown := rank("some_future_accelerator")
	for _, known := range append(append([]string{}, owned...), rented...) {
		if unknown <= rank(known) {
			t.Fatalf("unknown class ranks %d, at or below known %s (%d): an "+
				"unpriced class would be preferred as if it were free",
				unknown, known, rank(known))
		}
	}

	// Every class the control plane ADMITS must be priced. A class that can
	// register but has no rank silently becomes the ELSE case.
	for class := range validHWClasses {
		if rank(class) >= 99 {
			t.Fatalf("admitted hardware class %q has no cost rank and falls to the "+
				"unknown case; it can register but cannot be priced", class)
		}
	}
}
