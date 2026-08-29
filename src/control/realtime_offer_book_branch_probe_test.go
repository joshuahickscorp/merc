package main

import (
	"fmt"
	"strings"
	"testing"
)

// TestRealtimeOfferBookBranchProbeMatchesUnboundedCount proves the bounded
// LIMIT-2 probe and the historical unbounded COUNT agree on the only branch
// decision production makes (probe > 1), for small eligible books and for
// offers that fail every eligibility filter.
func TestRealtimeOfferBookBranchProbeMatchesUnboundedCount(t *testing.T) {
	installSettlementCurrencyForTest(t, "usd")
	t.Setenv("MERC_TOKEN_KEY", "rt-branch-probe-key-32bytes-min!!!!!!")

	ctx, store, pool := openPayoutTestStore(t)
	resetRealtimeClearingState(t, ctx, pool)
	profile := sortedVLLMProfiles()[0]

	// Predicate bodies must stay shared: the optimisation is free only while
	// the WHERE list is byte-identical apart from projection and LIMIT.
	if !strings.Contains(realtimeOfferBookBranchProbeSQL, realtimeOfferBookBranchProbePredicate) {
		t.Fatal("branch probe lost the shared eligibility predicate")
	}
	if !strings.Contains(realtimeOfferBookUnboundedCountSQL, realtimeOfferBookBranchProbePredicate) {
		t.Fatal("unbounded count lost the shared eligibility predicate")
	}
	if !strings.Contains(realtimeOfferBookBranchProbeSQL, "LIMIT 2") {
		t.Fatal("branch probe must stop after two rows")
	}
	if strings.Contains(realtimeOfferBookUnboundedCountSQL, "LIMIT") {
		t.Fatal("unbounded count must not carry a LIMIT")
	}

	runCounts := func(t *testing.T) (unbounded, probe int) {
		t.Helper()
		if err := pool.QueryRow(ctx, realtimeOfferBookUnboundedCountSQL,
			profile.RuntimeProfileID, profile.ProfileSHA256,
			profile.BuyerInputUSDPerMillionTokens, profile.BuyerOutputUSDPerMillionTokens,
		).Scan(&unbounded); err != nil {
			t.Fatalf("unbounded count: %v", err)
		}
		if err := pool.QueryRow(ctx, realtimeOfferBookBranchProbeSQL,
			profile.RuntimeProfileID, profile.ProfileSHA256,
			profile.BuyerInputUSDPerMillionTokens, profile.BuyerOutputUSDPerMillionTokens,
		).Scan(&probe); err != nil {
			t.Fatalf("branch probe: %v", err)
		}
		return unbounded, probe
	}
	assertBranchAgree := func(t *testing.T, wantEligible int) {
		t.Helper()
		unbounded, probe := runCounts(t)
		if unbounded != wantEligible {
			t.Fatalf("unbounded count=%d want eligible=%d", unbounded, wantEligible)
		}
		// Probe saturates at 2.
		wantProbe := wantEligible
		if wantProbe > 2 {
			wantProbe = 2
		}
		if probe != wantProbe {
			t.Fatalf("branch probe=%d want %d (eligible=%d)", probe, wantProbe, wantEligible)
		}
		if (unbounded > 1) != (probe > 1) {
			t.Fatalf("branch disagree: unbounded=%d probe=%d ( >1 differs)", unbounded, probe)
		}
	}

	// --- eligible book sizes 0, 1, 2, 5 ---
	assertBranchAgree(t, 0)

	for _, n := range []int{1, 2, 5} {
		t.Run(fmt.Sprintf("eligible_%d", n), func(t *testing.T) {
			resetRealtimeClearingState(t, ctx, pool)
			for i := 0; i < n; i++ {
				_ = newRealtimeClearingOffer(t, ctx, store, pool, profile, "HOT", 0.08, 0.30, 4)
			}
			assertBranchAgree(t, n)
		})
	}

	// --- ineligible offers excluded identically ---
	t.Run("ineligible_excluded_identically", func(t *testing.T) {
		resetRealtimeClearingState(t, ctx, pool)
		// One live eligible offer so the book is non-empty.
		live := newRealtimeClearingOffer(t, ctx, store, pool, profile, "HOT", 0.08, 0.30, 4)

		// Stale last_seen_at (outside the 45s window).
		stale := newRealtimeClearingOffer(t, ctx, store, pool, profile, "HOT", 0.08, 0.30, 4)
		if _, err := pool.Exec(ctx, `
			UPDATE realtime_worker_offers
			   SET last_seen_at = now() - interval '120 seconds'
			 WHERE worker_id=$1`, stale.WorkerID); err != nil {
			t.Fatal(err)
		}

		// Zero available sequences.
		empty := newRealtimeClearingOffer(t, ctx, store, pool, profile, "HOT", 0.08, 0.30, 4)
		if _, err := pool.Exec(ctx, `
			UPDATE realtime_worker_offers SET available_sequences=0 WHERE worker_id=$1`,
			empty.WorkerID); err != nil {
			t.Fatal(err)
		}

		// Quarantined supplier.
		quarantined := newRealtimeClearingOffer(t, ctx, store, pool, profile, "HOT", 0.08, 0.30, 4)
		if _, err := pool.Exec(ctx, `
			UPDATE suppliers SET quarantined_at=now() WHERE id=$1`, quarantined.SupplierID); err != nil {
			t.Fatal(err)
		}

		// Over-ceiling price: insert a valid offer, then raise ask above the
		// buyer ceilings the probe binds as $3/$4. UpsertRealtimeOffer refuses
		// a registration that leaves no Merc contribution, so the over-ceiling
		// shape is applied post-insert.
		over := newRealtimeClearingOffer(t, ctx, store, pool, profile, "HOT", 0.08, 0.30, 4)
		if _, err := pool.Exec(ctx, `
			UPDATE realtime_worker_offers
			   SET supplier_input_usd_per_million_tokens = $2,
			       supplier_output_usd_per_million_tokens = $3
			 WHERE worker_id=$1`,
			over.WorkerID,
			profile.BuyerInputUSDPerMillionTokens+1.0,
			profile.BuyerOutputUSDPerMillionTokens+1.0,
		); err != nil {
			t.Fatal(err)
		}

		// Only `live` should count. Keep it fresh.
		if _, err := pool.Exec(ctx, `
			UPDATE realtime_worker_offers SET last_seen_at=now(), status='ACTIVE'
			 WHERE worker_id=$1`, live.WorkerID); err != nil {
			t.Fatal(err)
		}
		assertBranchAgree(t, 1)

		// Add a second live offer: branch must flip to multi-offer.
		_ = newRealtimeClearingOffer(t, ctx, store, pool, profile, "WARM", 0.08, 0.30, 4)
		assertBranchAgree(t, 2)
	})
}
