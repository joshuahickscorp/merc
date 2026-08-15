package main

import (
	"testing"
	"time"
)

// TestFlagOnHeartbeatDoesNotWaitForDurability is the P0A proof: with the live
// index authoritative, an authenticated heartbeat must reach the index and
// return WITHOUT blocking on a durable PostgreSQL commit.
//
// The measurement uses the flush interval as the fault injection. A 3s interval
// is far longer than any commit, so it isolates the caller's wait: flag OFF must
// block for roughly the flush interval (that is today's contract — the caller
// waits until the row is durable), and flag ON must return immediately.
//
// This is deliberately a latency assertion, because "off the critical path" is a
// latency property. The bounds are loose (an order of magnitude apart) so the
// test measures the architecture, not the host.
func TestFlagOnHeartbeatDoesNotWaitForDurability(t *testing.T) {
	const flush = 3 * time.Second

	hb := func(t *testing.T, authoritative bool) time.Duration {
		t.Helper()
		if authoritative {
			t.Setenv("MERC_LIVENESS_INDEX_AUTHORITATIVE", "1")
		}
		ctx, store, pool := openIsolatedTestStore(t)
		store.SetLivenessBatchConfigForTest(livenessBatchConfig{
			Enabled: true, MaxBatch: 1000, FlushInterval: flush,
		})
		t.Setenv("MERC_TOKEN_KEY", "liveness-detach-test-key-32-bytes!!!")
		installSettlementCurrencyForTest(t, "usd")
		profile := sortedVLLMProfiles()[0]
		worker := seedOneRealtimeOffer(t, ctx, store, pool, profile)

		start := time.Now()
		if err := store.HeartbeatRealtimeOffer(ctx, worker, RealtimeOfferHeartbeat{
			RuntimeProfileID: profile.RuntimeProfileID, Warmth: "HOT",
			AvailableSequences: 8, Status: "ACTIVE",
		}); err != nil {
			t.Fatalf("heartbeat: %v", err)
		}
		elapsed := time.Since(start)

		if authoritative && !store.offerIndexLive(worker.WorkerID, profile.RuntimeProfileID, time.Now().UTC()) {
			t.Fatal("flag ON: heartbeat returned but the offer is not index-live — " +
				"liveness must be recorded before the caller is released")
		}
		return elapsed
	}

	t.Run("flag_off_blocks_until_durable", func(t *testing.T) {
		if got := hb(t, false); got < flush/2 {
			t.Fatalf("flag OFF returned in %v, expected to block ~%v waiting for the "+
				"durable write; the blocking contract has changed", got, flush)
		}
	})

	t.Run("flag_on_returns_without_durable_commit", func(t *testing.T) {
		if got := hb(t, true); got > flush/4 {
			t.Fatalf("flag ON returned in %v — the caller is still waiting on the "+
				"durable write (flush interval %v). The write is not off the fast path", got, flush)
		}
	})
}

// TestFlagOnAdmissionMatchesDurableWhereClause pins the security boundary of the
// write retirement: the in-process admission gate must refuse exactly what the
// durable UPDATE's WHERE clause refuses. If admission were looser than the SQL
// it replaces, a heartbeat that could never have updated a row would still mark
// the offer live — an ephemeral structure inventing eligibility.
func TestFlagOnAdmissionMatchesDurableWhereClause(t *testing.T) {
	t.Setenv("MERC_LIVENESS_INDEX_AUTHORITATIVE", "1")
	ctx, store, pool := openIsolatedTestStore(t)
	t.Setenv("MERC_TOKEN_KEY", "liveness-admit-test-key-32-bytes!!!!")
	installSettlementCurrencyForTest(t, "usd")
	profile := sortedVLLMProfiles()[0]
	worker := seedOneRealtimeOffer(t, ctx, store, pool, profile)

	var maxActive int
	if err := pool.QueryRow(ctx, `
		SELECT max_active_sequences FROM realtime_worker_offers
		 WHERE worker_id=$1 AND runtime_profile_id=$2`,
		worker.WorkerID, profile.RuntimeProfileID).Scan(&maxActive); err != nil {
		t.Fatal(err)
	}

	// Registration is itself a liveness assertion, so the seeded store already
	// reads live. Restart onto a fresh index instead: that is the fail-closed
	// "dead" baseline, and it makes the assertion the strong one — a refused
	// heartbeat must not be able to RESURRECT a dead offer.
	store = NewStore(pool)
	if err := store.adoptMigratedSchema(ctx); err != nil {
		t.Fatalf("restart adopt: %v", err)
	}
	live := func() bool {
		return store.offerIndexLive(worker.WorkerID, profile.RuntimeProfileID, time.Now().UTC())
	}
	if live() {
		t.Fatal("fresh store must read dead before any heartbeat (fail-closed)")
	}

	t.Run("unregistered_profile_is_refused_and_not_live", func(t *testing.T) {
		other := worker
		err := store.HeartbeatRealtimeOffer(ctx, other, RealtimeOfferHeartbeat{
			RuntimeProfileID: profile.RuntimeProfileID + "-absent", Warmth: "HOT",
			AvailableSequences: 1, Status: "ACTIVE",
		})
		if err == nil {
			t.Fatal("heartbeat for an unregistered offer must be refused")
		}
		if live() {
			t.Fatal("a refused heartbeat marked the offer live")
		}
	})

	t.Run("wrong_supplier_is_refused_and_not_live", func(t *testing.T) {
		impostor := worker
		impostor.SupplierID = worker.WorkerID // any id that is not the owning supplier
		if err := store.HeartbeatRealtimeOffer(ctx, impostor, RealtimeOfferHeartbeat{
			RuntimeProfileID: profile.RuntimeProfileID, Warmth: "HOT",
			AvailableSequences: 1, Status: "ACTIVE",
		}); err == nil {
			t.Fatal("heartbeat carrying a supplier that does not own the offer must be refused")
		}
		if live() {
			t.Fatal("a wrong-supplier heartbeat marked the offer live")
		}
	})

	t.Run("over_capacity_is_refused_and_not_live", func(t *testing.T) {
		if err := store.HeartbeatRealtimeOffer(ctx, worker, RealtimeOfferHeartbeat{
			RuntimeProfileID: profile.RuntimeProfileID, Warmth: "HOT",
			AvailableSequences: maxActive + 1, Status: "ACTIVE",
		}); err == nil {
			t.Fatalf("heartbeat claiming %d of %d sequences must be refused", maxActive+1, maxActive)
		}
		if live() {
			t.Fatal("an over-capacity heartbeat marked the offer live")
		}
	})

	t.Run("legitimate_heartbeat_is_admitted_and_live", func(t *testing.T) {
		if err := store.HeartbeatRealtimeOffer(ctx, worker, RealtimeOfferHeartbeat{
			RuntimeProfileID: profile.RuntimeProfileID, Warmth: "HOT",
			AvailableSequences: maxActive, Status: "ACTIVE",
		}); err != nil {
			t.Fatalf("legitimate heartbeat: %v", err)
		}
		if !live() {
			t.Fatal("an admitted heartbeat did not mark the offer live")
		}
	})
}

// TestDurableHeartbeatChangeGate covers the retirement decision itself. A
// repeated identical heartbeat inside the refresh interval must not earn a
// durable transaction; any state change must, immediately, because status and
// capacity are money-selection inputs that authorize still reads off the row.
func TestDurableHeartbeatChangeGate(t *testing.T) {
	store := &Store{}
	worker := WorkerAuth{}
	base := RealtimeOfferHeartbeat{RuntimeProfileID: "p", Warmth: "HOT", AvailableSequences: 8, Status: "ACTIVE"}
	t0 := time.Now().UTC()

	if !store.durableHeartbeatNeeded(worker, base, t0) {
		t.Fatal("the first heartbeat for an offer must always be persisted")
	}
	store.recordDurableHeartbeat(worker, base, t0)

	if store.durableHeartbeatNeeded(worker, base, t0.Add(time.Second)) {
		t.Fatal("an identical repeat inside the refresh interval must not be persisted")
	}

	for name, changed := range map[string]RealtimeOfferHeartbeat{
		"status":   {RuntimeProfileID: "p", Warmth: "HOT", AvailableSequences: 8, Status: "FAILED"},
		"warmth":   {RuntimeProfileID: "p", Warmth: "COLD", AvailableSequences: 8, Status: "ACTIVE"},
		"capacity": {RuntimeProfileID: "p", Warmth: "HOT", AvailableSequences: 0, Status: "ACTIVE"},
	} {
		if !store.durableHeartbeatNeeded(worker, changed, t0.Add(time.Second)) {
			t.Fatalf("a changed %s must be persisted immediately, not deferred", name)
		}
	}

	if !store.durableHeartbeatNeeded(worker, base, t0.Add(livenessDurableRefreshInterval)) {
		t.Fatal("an unchanged heartbeat must still refresh once the interval elapses, " +
			"or the offer would age out of the 45s authorize window")
	}

	if livenessDurableRefreshInterval >= realtimeOfferLivenessWindow {
		t.Fatalf("refresh interval %v must stay well inside the %v liveness window",
			livenessDurableRefreshInterval, realtimeOfferLivenessWindow)
	}
}

// TestChangeGateNeverDelaysEviction pins the safety claim that lets the durable
// row stay a derived projection of the index rather than a competing authority.
//
// Skipping writes makes `last_seen_at` STALE, and a stale stamp expires EARLIER,
// never later: an offer leaves the authorize book at (last written observation +
// 45s), which can only be at or before (last heartbeat + 45s). So the change-gate
// can shorten a dead worker's eligibility but never extend it — the false-negative
// direction the corruption boundary permits.
//
// The test walks a heartbeating fleet-of-one through simulated time and asserts
// the gap between persisted observations never exceeds the refresh interval,
// which is what makes that bound hold.
func TestChangeGateNeverDelaysEviction(t *testing.T) {
	for _, interval := range []time.Duration{time.Second, 5 * time.Second, 11 * time.Second} {
		t.Run(interval.String(), func(t *testing.T) {
			store := &Store{}
			worker := WorkerAuth{}
			hb := RealtimeOfferHeartbeat{
				RuntimeProfileID: "p", Warmth: "HOT", AvailableSequences: 8, Status: "ACTIVE",
			}

			start := time.Now().UTC()
			var lastPersist time.Time
			var worstGap time.Duration

			for at := start; at.Sub(start) <= 5*time.Minute; at = at.Add(interval) {
				if store.durableHeartbeatNeeded(worker, hb, at) {
					store.recordDurableHeartbeat(worker, hb, at)
					lastPersist = at
					continue
				}
				if gap := at.Sub(lastPersist); gap > worstGap {
					worstGap = gap
				}
			}

			// worstGap bounds how stale last_seen_at can be. Eviction happens at
			// (stamp + 45s), so a bound below the refresh interval is what keeps
			// it at or before (last heartbeat + 45s).
			if worstGap >= livenessDurableRefreshInterval {
				t.Fatalf("heartbeats every %v went %v without a durable write (refresh interval %v); "+
					"last_seen_at would lag far enough to matter", interval, worstGap, livenessDurableRefreshInterval)
			}
		})
	}
}

// TestDetachedHeartbeatWritesCommitInSubmissionOrder guards the ordering the
// detached path had to restore. Detaching the caller removed the merc-agent's
// implicit serialization: it used to await each heartbeat's commit before
// sending the next, so an ACTIVE was always durable before the DRAINING/FAILED
// that followed it. With concurrent flushers that earlier ACTIVE could commit
// LAST and strand a dead worker as selectable — and the terminal report is
// single-shot (agent/src/vllm.rs report_terminal_state sends one heartbeat, then
// the container dies), so nothing would ever heal the row.
//
// MaxBatch 1 makes every heartbeat its own batch, so this drives the queue
// directly. The structural guarantee is single-consumer FIFO enqueued under the
// same lock that takes the batch; this test is the regression guard on it, not a
// proof — a reordering pool might still pass by luck on a quiet host.
func TestDetachedHeartbeatWritesCommitInSubmissionOrder(t *testing.T) {
	t.Setenv("MERC_LIVENESS_INDEX_AUTHORITATIVE", "1")
	ctx, store, pool := openIsolatedTestStore(t)
	store.SetLivenessBatchConfigForTest(livenessBatchConfig{
		Enabled: true, MaxBatch: 1, FlushInterval: time.Hour,
	})
	t.Setenv("MERC_TOKEN_KEY", "liveness-order-test-key-32-bytes-min!")
	installSettlementCurrencyForTest(t, "usd")
	profile := sortedVLLMProfiles()[0]
	worker := seedOneRealtimeOffer(t, ctx, store, pool, profile)

	// Distinct statuses so the durable row reveals which write landed last.
	// FAILED is the terminal report the agent sends exactly once.
	for _, status := range []string{"ACTIVE", "DRAINING", "FAILED"} {
		if err := store.HeartbeatRealtimeOffer(ctx, worker, RealtimeOfferHeartbeat{
			RuntimeProfileID: profile.RuntimeProfileID, Warmth: "HOT",
			AvailableSequences: 8, Status: status,
		}); err != nil {
			t.Fatalf("heartbeat %s: %v", status, err)
		}
	}

	read := func() string {
		t.Helper()
		var s string
		if err := pool.QueryRow(ctx, `
			SELECT status FROM realtime_worker_offers
			 WHERE worker_id=$1 AND runtime_profile_id=$2`,
			worker.WorkerID, profile.RuntimeProfileID).Scan(&s); err != nil {
			t.Fatal(err)
		}
		return s
	}

	deadline := time.Now().Add(10 * time.Second)
	for read() != "FAILED" && time.Now().Before(deadline) {
		time.Sleep(20 * time.Millisecond)
	}
	if got := read(); got != "FAILED" {
		t.Fatalf("durable status=%q after draining detached writes; want FAILED (the last submitted)", got)
	}
	// A late-committing earlier write would surface as a revert.
	time.Sleep(300 * time.Millisecond)
	if got := read(); got != "FAILED" {
		t.Fatalf("durable status reverted to %q — an earlier write committed after the terminal FAILED", got)
	}
}
