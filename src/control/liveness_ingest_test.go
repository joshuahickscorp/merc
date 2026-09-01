package main

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
)

func TestResolveHeartbeatObservationClampAndReject(t *testing.T) {
	now := time.Date(2026, 8, 10, 12, 0, 0, 0, time.UTC)

	// Nil observation → receipt time (server now).
	got, err := resolveHeartbeatObservation(nil, now)
	if err != nil {
		t.Fatalf("nil observation: %v", err)
	}
	if !got.Equal(now) {
		t.Fatalf("nil observation: got %v want %v", got, now)
	}

	// Future observation is clamped, never written ahead of server now.
	future := now.Add(30 * time.Second).UnixMilli()
	got, err = resolveHeartbeatObservation(&future, now)
	if err != nil {
		t.Fatalf("future observation: %v", err)
	}
	if !got.Equal(now) {
		t.Fatalf("future must clamp to server now; got %v", got)
	}

	// Exactly at the window edge is accepted.
	edge := now.Add(-realtimeOfferLivenessWindow).UnixMilli()
	got, err = resolveHeartbeatObservation(&edge, now)
	if err != nil {
		t.Fatalf("window-edge observation: %v", err)
	}
	if !got.Equal(now.Add(-realtimeOfferLivenessWindow)) {
		t.Fatalf("window-edge: got %v", got)
	}

	// Older than the window is rejected — not floored to now (that would
	// revive a dead device).
	stale := now.Add(-realtimeOfferLivenessWindow - time.Second).UnixMilli()
	_, err = resolveHeartbeatObservation(&stale, now)
	if !errors.Is(err, errStaleHeartbeatObservation) {
		t.Fatalf("stale observation: got %v want errStaleHeartbeatObservation", err)
	}
}

var benchmarkUpdatedOfferKeySink updatedOfferKey

func BenchmarkUpdatedRowKey(b *testing.B) {
	workerID := uuid.MustParse("00000000-0000-0000-0000-000000000001")
	supplierID := uuid.MustParse("00000000-0000-0000-0000-000000000002")
	profileID := "vllm-cx-chat-1b"
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		benchmarkUpdatedOfferKeySink = updatedRowKey(workerID, supplierID, profileID)
	}
}

func TestRealtimeHeartbeatHTTPRequiresWorkerAuth(t *testing.T) {
	// Prove there is no unauthenticated liveness write path on the production
	// HTTP surface. Building device-supplied timestamps on top of an open
	// endpoint would be unsafe; stop-and-report if this ever regresses.
	ctx, store, _ := openIsolatedTestStore(t)
	_ = ctx
	store.SetLivenessBatchConfigForTest(livenessBatchConfig{Enabled: false})
	// Zero CanaryPolicy is disabled → allowsWorker returns true; we only need
	// authWorker to run far enough to demand X-Worker-Token.
	srv := &Server{store: store, workerLimiter: newRateLimiter(1_000_000, 1_000_000)}
	handler := srv.authWorker(http.HandlerFunc(srv.handleRealtimeWorkerHeartbeat))

	req := httptest.NewRequest(http.MethodPost, "/v1/worker/realtime/heartbeat", strings.NewReader(`{
		"runtime_profile_id":"x","warmth":"HOT","available_sequences":1,"status":"ACTIVE"
	}`))
	// Deliberately no X-Worker-Token.
	rr := httptest.NewRecorder()
	handler.ServeHTTP(rr, req)
	if rr.Code != http.StatusUnauthorized {
		t.Fatalf("missing token: status %d body %q; unauthenticated liveness must be refused",
			rr.Code, rr.Body.String())
	}
	if !strings.Contains(rr.Body.String(), "X-Worker-Token") &&
		!strings.Contains(rr.Body.String(), "worker token") &&
		!strings.Contains(rr.Body.String(), "missing") {
		// authWorker writes "missing X-Worker-Token"
		t.Fatalf("expected auth error body, got %q", rr.Body.String())
	}
}

func TestHeartbeatObservationTimestampIsDurableNotFlushTime(t *testing.T) {
	ctx, store, pool := openIsolatedTestStore(t)
	// Force a long flush window so a bug that stamps now() at flush would be
	// visible as last_seen_at ≫ observation.
	store.SetLivenessBatchConfigForTest(livenessBatchConfig{
		Enabled: true, MaxBatch: 100, FlushInterval: 80 * time.Millisecond,
	})
	t.Setenv("MERC_TOKEN_KEY", "liveness-obs-test-key-32-bytes-min!!")
	installSettlementCurrencyForTest(t, "usd")
	profile := sortedVLLMProfiles()[0]
	worker := seedOneRealtimeOffer(t, ctx, store, pool, profile)

	// Observation 2s in the past — well inside the window, clearly not "now"
	// at flush (~80ms later).
	obs := time.Now().UTC().Add(-2 * time.Second)
	ms := obs.UnixMilli()
	if err := store.HeartbeatRealtimeOffer(ctx, worker, RealtimeOfferHeartbeat{
		RuntimeProfileID: profile.RuntimeProfileID, Warmth: "HOT",
		AvailableSequences: 8, Status: "ACTIVE", ObservedAtUnixMs: &ms,
	}); err != nil {
		t.Fatalf("heartbeat: %v", err)
	}

	var lastSeen time.Time
	if err := pool.QueryRow(ctx, `
		SELECT last_seen_at FROM realtime_worker_offers
		 WHERE worker_id=$1 AND runtime_profile_id=$2`,
		worker.WorkerID, profile.RuntimeProfileID).Scan(&lastSeen); err != nil {
		t.Fatal(err)
	}
	lastSeen = lastSeen.UTC()
	// Allow 1s clock skew / truncation; must not land near flush-time now.
	if lastSeen.Sub(obs) > time.Second || obs.Sub(lastSeen) > time.Second {
		t.Fatalf("last_seen_at=%v obs=%v; durable stamp must be the observation, not flush time",
			lastSeen, obs)
	}
	if time.Since(lastSeen) < 1500*time.Millisecond {
		t.Fatalf("last_seen_at=%v looks like flush-time now(); observation was 2s ago", lastSeen)
	}
}

func TestHeartbeatFutureObservationClampedOnWrite(t *testing.T) {
	ctx, store, pool := openIsolatedTestStore(t)
	store.SetLivenessBatchConfigForTest(livenessBatchConfig{Enabled: false})
	t.Setenv("MERC_TOKEN_KEY", "liveness-future-test-key-32bytes-min!")
	installSettlementCurrencyForTest(t, "usd")
	profile := sortedVLLMProfiles()[0]
	worker := seedOneRealtimeOffer(t, ctx, store, pool, profile)

	before := time.Now().UTC()
	future := before.Add(10 * time.Minute).UnixMilli()
	if err := store.HeartbeatRealtimeOffer(ctx, worker, RealtimeOfferHeartbeat{
		RuntimeProfileID: profile.RuntimeProfileID, Warmth: "HOT",
		AvailableSequences: 8, Status: "ACTIVE", ObservedAtUnixMs: &future,
	}); err != nil {
		t.Fatalf("heartbeat: %v", err)
	}
	after := time.Now().UTC()

	var lastSeen time.Time
	if err := pool.QueryRow(ctx, `
		SELECT last_seen_at FROM realtime_worker_offers
		 WHERE worker_id=$1 AND runtime_profile_id=$2`,
		worker.WorkerID, profile.RuntimeProfileID).Scan(&lastSeen); err != nil {
		t.Fatal(err)
	}
	lastSeen = lastSeen.UTC()
	if lastSeen.After(after.Add(time.Second)) {
		t.Fatalf("future observation was not clamped: last_seen_at=%v after=%v", lastSeen, after)
	}
	if lastSeen.Before(before.Add(-time.Second)) {
		t.Fatalf("clamped stamp too old: last_seen_at=%v before=%v", lastSeen, before)
	}
}

func TestHeartbeatStaleObservationRejected(t *testing.T) {
	ctx, store, pool := openIsolatedTestStore(t)
	store.SetLivenessBatchConfigForTest(livenessBatchConfig{Enabled: false})
	t.Setenv("MERC_TOKEN_KEY", "liveness-stale-test-key-32-bytes-min!")
	installSettlementCurrencyForTest(t, "usd")
	profile := sortedVLLMProfiles()[0]
	worker := seedOneRealtimeOffer(t, ctx, store, pool, profile)

	// Seed a known last_seen_at so a rejected write cannot be confused with
	// a successful one.
	fixed := time.Now().UTC().Add(-10 * time.Second).Truncate(time.Microsecond)
	if _, err := pool.Exec(ctx, `
		UPDATE realtime_worker_offers SET last_seen_at=$1
		 WHERE worker_id=$2`, fixed, worker.WorkerID); err != nil {
		t.Fatal(err)
	}

	stale := time.Now().UTC().Add(-realtimeOfferLivenessWindow - 5*time.Second).UnixMilli()
	err := store.HeartbeatRealtimeOffer(ctx, worker, RealtimeOfferHeartbeat{
		RuntimeProfileID: profile.RuntimeProfileID, Warmth: "HOT",
		AvailableSequences: 8, Status: "ACTIVE", ObservedAtUnixMs: &stale,
	})
	if !errors.Is(err, errStaleHeartbeatObservation) {
		t.Fatalf("got %v want errStaleHeartbeatObservation", err)
	}

	var lastSeen time.Time
	if err := pool.QueryRow(ctx, `
		SELECT last_seen_at FROM realtime_worker_offers WHERE worker_id=$1`,
		worker.WorkerID).Scan(&lastSeen); err != nil {
		t.Fatal(err)
	}
	if !lastSeen.UTC().Equal(fixed) {
		t.Fatalf("rejected write must not mutate last_seen_at: got %v want %v", lastSeen.UTC(), fixed)
	}
}

func TestBatchedHeartbeatDeadDeviceLeavesWithinWindow(t *testing.T) {
	// The correctness trap: flush delay must not keep a dead device eligible
	// past the 45s window. Measure eligibility, do not reason about it.
	ctx, store, pool := openIsolatedTestStore(t)
	store.SetLivenessBatchConfigForTest(livenessBatchConfig{
		Enabled: true, MaxBatch: 64, FlushInterval: 50 * time.Millisecond,
	})
	t.Setenv("MERC_TOKEN_KEY", "liveness-evict-test-key-32-bytes-min!")
	installSettlementCurrencyForTest(t, "usd")
	profile := sortedVLLMProfiles()[0]
	worker := seedOneRealtimeOffer(t, ctx, store, pool, profile)

	// Last authentic observation: "now". Capture wall clock around the call.
	obsBefore := time.Now().UTC()
	if err := store.HeartbeatRealtimeOffer(ctx, worker, RealtimeOfferHeartbeat{
		RuntimeProfileID: profile.RuntimeProfileID, Warmth: "HOT",
		AvailableSequences: 8, Status: "ACTIVE",
	}); err != nil {
		t.Fatalf("heartbeat: %v", err)
	}
	obsAfter := time.Now().UTC()

	var lastSeen time.Time
	if err := pool.QueryRow(ctx, `
		SELECT last_seen_at FROM realtime_worker_offers WHERE worker_id=$1`,
		worker.WorkerID).Scan(&lastSeen); err != nil {
		t.Fatal(err)
	}
	lastSeen = lastSeen.UTC()
	if lastSeen.Before(obsBefore.Add(-time.Second)) || lastSeen.After(obsAfter.Add(time.Second)) {
		t.Fatalf("unexpected last_seen_at=%v (obs window %v..%v)", lastSeen, obsBefore, obsAfter)
	}

	// Force the durable stamp to just inside the window edge relative to a
	// frozen "death" moment, then advance eligibility by rewriting the stamp
	// to deathTime and measuring when the production predicate drops it.
	//
	// We cannot sleep 45s in unit tests. Instead: set last_seen_at to
	// now()-44s (still eligible), then now()-46s (must be ineligible). The
	// production SQL predicate is the authority; batching is exercised by the
	// heartbeat above that produced the stamp shape.
	eligibleAt := time.Now().UTC().Add(-realtimeOfferLivenessWindow + 2*time.Second)
	if _, err := pool.Exec(ctx, `
		UPDATE realtime_worker_offers SET last_seen_at=$1 WHERE worker_id=$2`,
		eligibleAt, worker.WorkerID); err != nil {
		t.Fatal(err)
	}
	var eligible bool
	if err := pool.QueryRow(ctx, `
		SELECT EXISTS (
		  SELECT 1 FROM realtime_worker_offers o
		   WHERE o.worker_id=$1 AND o.status='ACTIVE'
		     AND o.last_seen_at > now() - interval '45 seconds')`,
		worker.WorkerID).Scan(&eligible); err != nil {
		t.Fatal(err)
	}
	if !eligible {
		t.Fatal("offer with last_seen_at inside the window must still be eligible")
	}

	deadAt := time.Now().UTC().Add(-realtimeOfferLivenessWindow - time.Second)
	if _, err := pool.Exec(ctx, `
		UPDATE realtime_worker_offers SET last_seen_at=$1 WHERE worker_id=$2`,
		deadAt, worker.WorkerID); err != nil {
		t.Fatal(err)
	}
	if err := pool.QueryRow(ctx, `
		SELECT EXISTS (
		  SELECT 1 FROM realtime_worker_offers o
		   WHERE o.worker_id=$1 AND o.status='ACTIVE'
		     AND o.last_seen_at > now() - interval '45 seconds')`,
		worker.WorkerID).Scan(&eligible); err != nil {
		t.Fatal(err)
	}
	if eligible {
		t.Fatal("offer with last_seen_at older than 45s must leave the eligible set — window was widened or predicate broken")
	}

	// Explicit measurement: time from last observation to ineligibility equals
	// the window when last_seen_at is the observation (not observation+flush).
	// Synthetic but exact: death_instant = last_seen_at; ineligible when
	// now > last_seen_at + 45s. Flush delay is not in the formula.
	death := lastSeen // from the real batched heartbeat above
	ineligibleAt := death.Add(realtimeOfferLivenessWindow)
	// At death+window-1s the production predicate would still admit; at
	// death+window+1s it must not. We already checked both sides with the
	// rewrites above. Record the measured eviction horizon for the artifact.
	_ = ineligibleAt
	t.Logf("eviction_horizon last_seen_at=%s + window=%s → ineligible_at=%s (flush delay excluded by observation stamp)",
		death.Format(time.RFC3339Nano), realtimeOfferLivenessWindow, ineligibleAt.Format(time.RFC3339Nano))
}

func TestBatchedHeartbeatCoalescesConcurrentWrites(t *testing.T) {
	ctx, store, pool := openIsolatedTestStore(t)
	store.SetLivenessBatchConfigForTest(livenessBatchConfig{
		Enabled: true, MaxBatch: 64, FlushInterval: 30 * time.Millisecond,
	})
	t.Setenv("MERC_TOKEN_KEY", "liveness-coal-test-key-32-bytes-min!!")
	installSettlementCurrencyForTest(t, "usd")
	profile := sortedVLLMProfiles()[0]

	const n = 32
	workers := make([]WorkerAuth, n)
	for i := 0; i < n; i++ {
		workers[i] = seedOneRealtimeOffer(t, ctx, store, pool, profile)
	}

	var wg sync.WaitGroup
	errCh := make(chan error, n)
	start := make(chan struct{})
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(w WorkerAuth) {
			defer wg.Done()
			<-start
			errCh <- store.HeartbeatRealtimeOffer(ctx, w, RealtimeOfferHeartbeat{
				RuntimeProfileID: profile.RuntimeProfileID, Warmth: "HOT",
				AvailableSequences: 8, Status: "ACTIVE",
			})
		}(workers[i])
	}
	close(start)
	wg.Wait()
	close(errCh)
	for err := range errCh {
		if err != nil {
			t.Fatalf("batched heartbeat: %v", err)
		}
	}

	var live int
	if err := pool.QueryRow(ctx, `
		SELECT count(*) FROM realtime_worker_offers
		 WHERE status='ACTIVE' AND last_seen_at > now() - interval '45 seconds'`).Scan(&live); err != nil {
		t.Fatal(err)
	}
	if live != n {
		t.Fatalf("live offers=%d want %d", live, n)
	}
}

func TestLivenessWindowConstantMatchesSQLPredicate(t *testing.T) {
	if realtimeOfferLivenessWindow != 45*time.Second {
		t.Fatalf("realtimeOfferLivenessWindow=%v; production SQL uses 45 seconds", realtimeOfferLivenessWindow)
	}
	if formatLivenessWindowSQL() != "45 seconds" {
		t.Fatalf("formatLivenessWindowSQL=%q", formatLivenessWindowSQL())
	}
}

func TestCoalescerFlushDelayDoesNotWidenEligibilityWindow(t *testing.T) {
	// Adversarial: a long flush must not keep a dead device eligible past
	// last_seen_at + 45s. last_seen_at is the clamped observation, never
	// flush-time now(). Measure remaining eligibility after a delayed flush.
	ctx, store, pool := openIsolatedTestStore(t)
	store.SetLivenessBatchConfigForTest(livenessBatchConfig{
		Enabled: true, MaxBatch: 64, FlushInterval: 200 * time.Millisecond,
	})
	t.Setenv("MERC_TOKEN_KEY", "liveness-widen-test-key-32-bytes-min!")
	installSettlementCurrencyForTest(t, "usd")
	profile := sortedVLLMProfiles()[0]
	worker := seedOneRealtimeOffer(t, ctx, store, pool, profile)

	obs := time.Now().UTC().Add(-3 * time.Second)
	ms := obs.UnixMilli()
	if err := store.HeartbeatRealtimeOffer(ctx, worker, RealtimeOfferHeartbeat{
		RuntimeProfileID: profile.RuntimeProfileID, Warmth: "HOT",
		AvailableSequences: 8, Status: "ACTIVE", ObservedAtUnixMs: &ms,
	}); err != nil {
		t.Fatalf("heartbeat: %v", err)
	}
	var lastSeen time.Time
	if err := pool.QueryRow(ctx, `
		SELECT last_seen_at FROM realtime_worker_offers WHERE worker_id=$1`,
		worker.WorkerID).Scan(&lastSeen); err != nil {
		t.Fatal(err)
	}
	lastSeen = lastSeen.UTC()
	if lastSeen.Sub(obs) > time.Second || obs.Sub(lastSeen) > time.Second {
		t.Fatalf("last_seen_at=%v obs=%v; flush delay must not replace observation", lastSeen, obs)
	}
	// Remaining eligibility from *now* must be <= window. If last_seen_at were
	// flush-time now(), remaining would be ~45s; with a 3s-old observation it
	// must be ~42s.
	remaining := lastSeen.Add(realtimeOfferLivenessWindow).Sub(time.Now().UTC())
	if remaining > realtimeOfferLivenessWindow {
		t.Fatalf("eligibility remaining %s exceeds the 45s contract — window was widened", remaining)
	}
	if remaining > realtimeOfferLivenessWindow-2*time.Second {
		t.Fatalf("eligibility remaining %s looks like flush-time stamp (want ~42s from 3s-old obs)", remaining)
	}
	if remaining < realtimeOfferLivenessWindow-5*time.Second {
		t.Fatalf("eligibility remaining %s unexpectedly short", remaining)
	}

	// After the device stops, the production predicate must drop it once the
	// observation is older than the window — flush delay is not in the formula.
	deadAt := time.Now().UTC().Add(-realtimeOfferLivenessWindow - time.Second)
	if _, err := pool.Exec(ctx, `
		UPDATE realtime_worker_offers SET last_seen_at=$1 WHERE worker_id=$2`,
		deadAt, worker.WorkerID); err != nil {
		t.Fatal(err)
	}
	var eligible bool
	if err := pool.QueryRow(ctx, `
		SELECT EXISTS (
		  SELECT 1 FROM realtime_worker_offers o
		   WHERE o.worker_id=$1 AND o.status='ACTIVE'
		     AND o.last_seen_at > now() - interval '45 seconds')`,
		worker.WorkerID).Scan(&eligible); err != nil {
		t.Fatal(err)
	}
	if eligible {
		t.Fatal("dead device still eligible — coalescing widened the 45s window")
	}
}

func TestLivenessBatchDefaultsOffInsideGoTest(t *testing.T) {
	// Guard the test-suite default: an accidental default-on 50ms flush would
	// hide regressions and inflate every HeartbeatRealtimeOffer.
	if strings.TrimSpace(os.Getenv("MERC_LIVENESS_BATCH")) != "" {
		t.Skip("MERC_LIVENESS_BATCH is set; cannot assert the empty-env default")
	}
	cfg := livenessBatchConfigFromEnv()
	if cfg.Enabled {
		t.Fatal("liveness batching must default off inside go test unless MERC_LIVENESS_BATCH is set")
	}
}

func seedOneRealtimeOffer(t *testing.T, ctx context.Context, store *Store, _ *pgxpool.Pool, profile VLLMRuntimeProfile) WorkerAuth {
	t.Helper()
	// Worker row is a prerequisite; the offer itself is registered through the
	// production UpsertRealtimeOffer entry point — not a synthetic offer INSERT.
	supplierID := uuid.New()
	workerID := uuid.New()
	if _, err := store.pool.Exec(ctx, `
		INSERT INTO suppliers (id,email,status) VALUES ($1,$2,'active')`,
		supplierID, supplierID.String()+"@liveness.test"); err != nil {
		t.Fatalf("supplier: %v", err)
	}
	if _, err := store.pool.Exec(ctx, `
		INSERT INTO workers (id,supplier_id,hw_class,last_seen_at)
		VALUES ($1,$2,'nvidia_24gb',now())`, workerID, supplierID); err != nil {
		t.Fatalf("worker: %v", err)
	}
	worker := WorkerAuth{WorkerID: workerID, SupplierID: supplierID}
	reg := RealtimeOfferRegistration{
		RuntimeProfileID: profile.RuntimeProfileID, RuntimeProfileSHA256: profile.ProfileSHA256,
		HWClass: "nvidia_24gb", GPUCount: 1, MemoryGBPerGPU: 24,
		UpstreamBaseURL: "http://127.0.0.1:8811/v1", UpstreamToken: "cx_vllm_liveness_test_token_123456",
		Warmth: "HOT", MaxActiveSequences: 16, AvailableSequences: 8,
		SupplierInputUSDPerMillionTokens: 0.08, SupplierOutputUSDPerMillionTokens: 0.30,
	}
	if err := store.UpsertRealtimeOffer(ctx, worker, reg); err != nil {
		t.Fatalf("UpsertRealtimeOffer: %v", err)
	}
	return worker
}
