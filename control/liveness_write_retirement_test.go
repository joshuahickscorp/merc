package main

import (
	"errors"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
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

// TestFlagDivergenceClassesAreEnumerated pins every way flag ON can disagree
// with flag OFF at the one selection entry point the flag actually flips
// (ServiceLeaseDataPlaneTarget). The migration contract is "no silent
// divergence": each combination of durable-liveness and index-liveness must land
// in a named class with a stated safety direction, and an unclassified outcome
// is a failure rather than a curiosity.
//
// D1 index dead / SQL live  — ON refuses, OFF selects. ON is MORE RESTRICTIVE.
//
//	This is the fail-closed direction and the whole
//	point of the flip.
//
// D2 index live / SQL stale — ON selects, OFF refuses. ON is MORE PERMISSIVE,
//
//	and this is the only such class. It is reachable
//	ONLY when durable writes lag or fail while
//	heartbeats keep arriving, so the worker is
//	genuinely alive and the index is the more truthful
//	authority. Safe, but it is a real divergence and
//	must stay documented rather than discovered later.
//
// D3 both live              — identical.
// D4 both dead              — identical.
func TestFlagDivergenceClassesAreEnumerated(t *testing.T) {
	type outcome struct{ selectable bool }
	run := func(t *testing.T, flagOn bool, ageIndex, ageSQL bool) outcome {
		t.Helper()
		if flagOn {
			t.Setenv("MERC_LIVENESS_INDEX_AUTHORITATIVE", "1")
		} else {
			t.Setenv("MERC_LIVENESS_INDEX_AUTHORITATIVE", "0")
		}
		ctx, store, pool, buyerID, lease, worker, profile := seedLeaseWithRealtimeOffer(t)
		if err := store.HeartbeatRealtimeOffer(ctx, worker, RealtimeOfferHeartbeat{
			RuntimeProfileID: profile.RuntimeProfileID, Warmth: "HOT",
			AvailableSequences: 8, Status: "ACTIVE",
		}); err != nil {
			t.Fatalf("heartbeat: %v", err)
		}
		if ageIndex {
			ageOfferIndexSlot(t, store, worker.WorkerID, profile.RuntimeProfileID)
		}
		if ageSQL {
			if _, err := pool.Exec(ctx, `
				UPDATE realtime_worker_offers
				   SET last_seen_at = now() - interval '120 seconds'
				 WHERE worker_id=$1 AND runtime_profile_id=$2`,
				worker.WorkerID, profile.RuntimeProfileID); err != nil {
				t.Fatal(err)
			}
		}
		_, err := store.ServiceLeaseDataPlaneTarget(ctx, buyerID, lease.ID)
		if err != nil && !errors.Is(err, errServiceLeaseDataPlaneUnavailable) {
			t.Fatalf("unexpected error class: %v", err)
		}
		return outcome{selectable: err == nil}
	}

	for _, tc := range []struct {
		class            string
		ageIndex, ageSQL bool
		wantOff, wantOn  bool
		note             string
	}{
		{"D1_index_dead_sql_live", true, false, true, false, "ON must be more restrictive (fail closed)"},
		{"D2_index_live_sql_stale", false, true, false, true, "ON more permissive: durable write lagged, worker is genuinely heartbeating"},
		{"D3_both_live", false, false, true, true, "no divergence"},
		{"D4_both_dead", true, true, false, false, "no divergence"},
	} {
		t.Run(tc.class, func(t *testing.T) {
			off := run(t, false, tc.ageIndex, tc.ageSQL)
			on := run(t, true, tc.ageIndex, tc.ageSQL)
			if off.selectable != tc.wantOff {
				t.Fatalf("%s: flag OFF selectable=%v want %v (%s)", tc.class, off.selectable, tc.wantOff, tc.note)
			}
			if on.selectable != tc.wantOn {
				t.Fatalf("%s: flag ON selectable=%v want %v (%s)", tc.class, on.selectable, tc.wantOn, tc.note)
			}
		})
	}
}

// TestSettlementUnchangedWhenOfferStateArrivesViaDetachedWrite closes a hole the
// existing settle-parity test does not reach. That test never sends a heartbeat,
// so it proves the flag does not change settlement arithmetic but never exercises
// the path production would actually run: batching ON, flag ON, offer state
// reaching PostgreSQL through the DETACHED write rather than a blocking one.
//
// Here the money path reads a row written asynchronously. The heartbeat is
// awaited only to the extent of confirming it landed — that is the point, not a
// workaround: authorize must see the same durable state either way.
func TestSettlementUnchangedWhenOfferStateArrivesViaDetachedWrite(t *testing.T) {
	shot := func(t *testing.T, authoritative bool) settledRealtimeShot {
		t.Helper()
		if authoritative {
			t.Setenv("MERC_LIVENESS_INDEX_AUTHORITATIVE", "1")
		} else {
			t.Setenv("MERC_LIVENESS_INDEX_AUTHORITATIVE", "0")
		}
		ctx, store, pool := openIsolatedTestStore(t)
		// The production shape: coalescer on. Under flag ON this makes the
		// durable write detached; under flag OFF the caller still blocks on it.
		store.SetLivenessBatchConfigForTest(livenessBatchConfig{
			Enabled: true, MaxBatch: 1000, FlushInterval: 2 * time.Millisecond,
		})
		t.Setenv("MERC_TOKEN_KEY", "liveness-settle-detached-key-32bytes!")
		installSettlementCurrencyForTest(t, "usd")
		profile, supplierID, workerID := realtimeFundingFixture(t, ctx, store, pool)

		worker := WorkerAuth{WorkerID: workerID, SupplierID: supplierID}
		if err := store.HeartbeatRealtimeOffer(ctx, worker, RealtimeOfferHeartbeat{
			RuntimeProfileID: profile.RuntimeProfileID, Warmth: "HOT",
			AvailableSequences: 8, Status: "ACTIVE",
		}); err != nil {
			t.Fatalf("heartbeat: %v", err)
		}
		// Confirm the write actually reached PostgreSQL. Under flag ON the call
		// above returned before the commit, so this is the observable that
		// distinguishes "detached" from "lost".
		deadline := time.Now().Add(10 * time.Second)
		for {
			var warmth string
			if err := pool.QueryRow(ctx, `
				SELECT warmth FROM realtime_worker_offers
				 WHERE worker_id=$1 AND runtime_profile_id=$2`,
				workerID, profile.RuntimeProfileID).Scan(&warmth); err != nil {
				t.Fatal(err)
			}
			if warmth == "HOT" {
				break
			}
			if time.Now().After(deadline) {
				t.Fatal("detached heartbeat write never reached PostgreSQL")
			}
			time.Sleep(10 * time.Millisecond)
		}

		buyerID := uuid.New()
		if _, err := pool.Exec(ctx, `INSERT INTO buyers (id,email,free_credit_usd) VALUES ($1,$2,0)`,
			buyerID, buyerID.String()+"@liveness-detached.invalid"); err != nil {
			t.Fatal(err)
		}
		must(t, store.SeedPrepaidBalance(ctx, buyerID, 5_000_000, "liveness-detached-"+buyerID.String()))

		maxUSD, estUSD, maxPrompt, maxCompletion := realtimeAuthCeiling(t, profile, 7, 2)
		contract, _, err := store.AuthorizeRealtimeContract(ctx, RealtimeContractAuthorization{
			RequestID: "req-liv-detached-" + uuid.NewString(), BuyerID: buyerID, Profile: profile,
			InputCommitment: strings.Repeat("c", 64), RequestSHA256: strings.Repeat("d", 64),
			MaximumPriceUSD: maxUSD, EstimatedPriceUSD: estUSD, DeadlineAt: time.Now().Add(time.Minute),
			MaximumPromptTokens: maxPrompt, MaximumCompletionTokens: maxCompletion,
			EstimatedPromptTokens: 7, EstimatedCompletionTokens: 2,
		})
		mustf(t, err, "authorize: %v")

		settlement, err := store.FinalizeRealtimeSuccess(ctx, contract.ID, RealtimeExecutionEvidence{
			ID: uuid.New(), HTTPStatus: http.StatusOK, StreamRootSHA256: strings.Repeat("1", 64),
			OutputCommitment: strings.Repeat("2", 64), PromptTokens: 7, CompletionTokens: 2, TotalTokens: 9,
		})
		mustf(t, err, "finalize: %v")

		rows, err := pool.Query(ctx, `
			SELECT kind, currency, (amount_usd*1000000)::bigint
			  FROM ledger_entries
			 WHERE execution_contract_id=$1
			 ORDER BY kind, (amount_usd*1000000)::bigint, currency`, contract.ID)
		if err != nil {
			t.Fatal(err)
		}
		defer rows.Close()
		var ledger []settledLedgerRow
		for rows.Next() {
			var r settledLedgerRow
			if err := rows.Scan(&r.kind, &r.currency, &r.micros); err != nil {
				t.Fatal(err)
			}
			ledger = append(ledger, r)
		}
		if err := rows.Err(); err != nil {
			t.Fatal(err)
		}
		if len(ledger) == 0 {
			t.Fatal("settlement produced no ledger rows")
		}
		return settledRealtimeShot{
			buyerChargeNanos:     settlement.BuyerChargeNanos,
			supplierPayableNanos: settlement.SupplierPayableNanos,
			ledger:               ledger,
		}
	}

	off := shot(t, false)
	on := shot(t, true)

	if off.buyerChargeNanos != on.buyerChargeNanos || off.supplierPayableNanos != on.supplierPayableNanos {
		t.Fatalf("detached write changed settlement money: off charge=%d payable=%d; on charge=%d payable=%d",
			off.buyerChargeNanos, off.supplierPayableNanos, on.buyerChargeNanos, on.supplierPayableNanos)
	}
	if len(off.ledger) != len(on.ledger) {
		t.Fatalf("detached write changed ledger row count: off=%d on=%d", len(off.ledger), len(on.ledger))
	}
	for i := range off.ledger {
		if off.ledger[i] != on.ledger[i] {
			t.Fatalf("detached write changed ledger[%d]: off=%+v on=%+v", i, off.ledger[i], on.ledger[i])
		}
	}
}

// TestLivenessStructuresCannotAlterCapabilityOrTrust pins P0 invariants 11 and
// 12 behaviourally. Existing coverage only scanned LiveDeviceIndex's METHOD
// NAMES for "capabilit"/"trust"/"grant", which proves the API surface does not
// advertise such a thing but not that exercising it changes nothing.
//
// Here the liveness machinery is driven hard — heartbeats under both flag
// settings, index mutation, an admission refusal, and a corrupt-mapping plant —
// and the capability and trust/quarantine surfaces are snapshotted before and
// after. Liveness may only ever answer "is this offer alive"; if it can move a
// capability grant or a supplier's trust standing, the corruption boundary is
// not where the design claims it is.
func TestLivenessStructuresCannotAlterCapabilityOrTrust(t *testing.T) {
	t.Setenv("MERC_LIVENESS_INDEX_AUTHORITATIVE", "1")
	ctx, store, pool := openIsolatedTestStore(t)
	store.SetLivenessBatchConfigForTest(livenessBatchConfig{Enabled: false})
	t.Setenv("MERC_TOKEN_KEY", "liveness-capability-key-32-bytes-min!!")
	installSettlementCurrencyForTest(t, "usd")
	profile := sortedVLLMProfiles()[0]
	worker := seedOneRealtimeOffer(t, ctx, store, pool, profile)

	snapshot := func() (caps, trust, quarantine string) {
		t.Helper()
		if err := pool.QueryRow(ctx, `
			SELECT COALESCE(string_agg(t::text, '|' ORDER BY t::text), '')
			  FROM worker_authorized_capabilities t`).Scan(&caps); err != nil {
			t.Fatal(err)
		}
		if err := pool.QueryRow(ctx, `
			SELECT COALESCE(string_agg(t::text, '|' ORDER BY t::text), '')
			  FROM realtime_supplier_outcome_stats t`).Scan(&trust); err != nil {
			t.Fatal(err)
		}
		if err := pool.QueryRow(ctx, `
			SELECT COALESCE(string_agg(s.id::text || ':' || COALESCE(s.quarantined_at::text,'-')
			       || ':' || s.status, '|' ORDER BY s.id::text), '')
			  FROM suppliers s`).Scan(&quarantine); err != nil {
			t.Fatal(err)
		}
		return
	}

	beforeCaps, beforeTrust, beforeQuar := snapshot()

	// Every liveness lever we have.
	for _, status := range []string{"ACTIVE", "DRAINING", "ACTIVE"} {
		if err := store.HeartbeatRealtimeOffer(ctx, worker, RealtimeOfferHeartbeat{
			RuntimeProfileID: profile.RuntimeProfileID, Warmth: "HOT",
			AvailableSequences: 8, Status: status,
		}); err != nil {
			t.Fatalf("heartbeat %s: %v", status, err)
		}
	}
	// A refused admission.
	impostor := worker
	impostor.SupplierID = uuid.New()
	_ = store.HeartbeatRealtimeOffer(ctx, impostor, RealtimeOfferHeartbeat{
		RuntimeProfileID: profile.RuntimeProfileID, Warmth: "HOT",
		AvailableSequences: 8, Status: "ACTIVE",
	})
	// A corrupt mapping plant, and direct index mutation.
	if slot, ok := store.lookupOfferSlot(worker.WorkerID, profile.RuntimeProfileID); ok {
		store.indexHeartbeatSlot(slot, worker.WorkerID, profile.RuntimeProfileID, time.Now().UTC(), time.Now().UTC())
	}
	ageOfferIndexSlot(t, store, worker.WorkerID, profile.RuntimeProfileID)
	_ = store.offerIndexLive(worker.WorkerID, profile.RuntimeProfileID, time.Now().UTC())

	afterCaps, afterTrust, afterQuar := snapshot()

	if beforeCaps != afterCaps {
		t.Fatalf("liveness activity changed worker_authorized_capabilities:\n before=%s\n after=%s", beforeCaps, afterCaps)
	}
	if beforeTrust != afterTrust {
		t.Fatalf("liveness activity changed realtime_supplier_outcome_stats (trust):\n before=%s\n after=%s", beforeTrust, afterTrust)
	}
	if beforeQuar != afterQuar {
		t.Fatalf("liveness activity changed supplier status/quarantine:\n before=%s\n after=%s", beforeQuar, afterQuar)
	}
}

// TestReplayedHeartbeatCannotResurrectDeadOfferBeyondWindow pins P0 invariant 6
// at OFFER grain. Existing replay coverage was index-primitive only.
//
// A captured heartbeat replayed after its observation has aged past the window
// must be REFUSED outright, so it cannot restart liveness for an offer that has
// gone quiet. A replay still inside the window is accepted — correctly, since it
// attests presence within the contract — and that is exactly why the durable
// stamp is the clamped observation and not receipt time: the replay can never
// extend liveness further than the original observation would have.
func TestReplayedHeartbeatCannotResurrectDeadOfferBeyondWindow(t *testing.T) {
	t.Setenv("MERC_LIVENESS_INDEX_AUTHORITATIVE", "1")
	ctx, store, pool := openIsolatedTestStore(t)
	store.SetLivenessBatchConfigForTest(livenessBatchConfig{Enabled: false})
	t.Setenv("MERC_TOKEN_KEY", "liveness-replay-key-32-bytes-minimum!")
	installSettlementCurrencyForTest(t, "usd")
	profile := sortedVLLMProfiles()[0]
	worker := seedOneRealtimeOffer(t, ctx, store, pool, profile)

	// Restart onto a fresh index: the offer is dead-for-routing (fail-closed).
	store = NewStore(pool)
	if err := store.adoptMigratedSchema(ctx); err != nil {
		t.Fatalf("restart adopt: %v", err)
	}
	if store.offerIndexLive(worker.WorkerID, profile.RuntimeProfileID, time.Now().UTC()) {
		t.Fatal("fresh store must read dead")
	}

	// Replay a captured observation from beyond the window.
	stale := time.Now().UTC().Add(-realtimeOfferLivenessWindow - 5*time.Second).UnixMilli()
	err := store.HeartbeatRealtimeOffer(ctx, worker, RealtimeOfferHeartbeat{
		RuntimeProfileID: profile.RuntimeProfileID, Warmth: "HOT",
		AvailableSequences: 8, Status: "ACTIVE", ObservedAtUnixMs: &stale,
	})
	if !errors.Is(err, errStaleHeartbeatObservation) {
		t.Fatalf("replayed out-of-window heartbeat: got %v want errStaleHeartbeatObservation", err)
	}
	if store.offerIndexLive(worker.WorkerID, profile.RuntimeProfileID, time.Now().UTC()) {
		t.Fatal("a replayed out-of-window heartbeat resurrected a dead offer")
	}

	// A replay from inside the window is accepted, but only ever carries the
	// original observation — it cannot buy more liveness than the original had.
	inside := time.Now().UTC().Add(-realtimeOfferLivenessWindow + 10*time.Second)
	insideMs := inside.UnixMilli()
	if err := store.HeartbeatRealtimeOffer(ctx, worker, RealtimeOfferHeartbeat{
		RuntimeProfileID: profile.RuntimeProfileID, Warmth: "HOT",
		AvailableSequences: 8, Status: "ACTIVE", ObservedAtUnixMs: &insideMs,
	}); err != nil {
		t.Fatalf("in-window replay: %v", err)
	}
	var lastSeen time.Time
	if err := pool.QueryRow(ctx, `
		SELECT last_seen_at FROM realtime_worker_offers
		 WHERE worker_id=$1 AND runtime_profile_id=$2`,
		worker.WorkerID, profile.RuntimeProfileID).Scan(&lastSeen); err != nil {
		t.Fatal(err)
	}
	if lastSeen.UTC().After(inside.Add(time.Second)) {
		t.Fatalf("last_seen_at=%v is later than the replayed observation %v; a replay must not "+
			"buy liveness the original observation did not have", lastSeen.UTC(), inside)
	}
}
