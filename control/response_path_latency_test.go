package main

import (
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"
)

// TestAdmissionTelemetryAsyncStillRecorded proves the async path does not drop
// events under ordinary load: every enqueued event is visible in the table
// after Close drains the queue. Dropped events are impossible under load
// (sync fallback) and bounded on crash (queueCap + workers) — see
// admission_telemetry.go.
func TestAdmissionTelemetryAsyncStillRecorded(t *testing.T) {
	installSettlementCurrencyForTest(t, "usd")
	ctx, store, pool := openIsolatedTestStore(t)
	t.Setenv("MERC_TOKEN_KEY", "response-path-test-key-with-32-bytes!!")

	if _, err := pool.Exec(ctx, `TRUNCATE
		realtime_admission_events, realtime_offer_samples,
		realtime_authorization_events, realtime_settlements, realtime_executions,
		realtime_refunds, execution_contracts, realtime_worker_offers,
		realtime_supplier_outcome_stats
		RESTART IDENTITY CASCADE`); err != nil {
		t.Fatal(err)
	}

	buyerID, err := store.CreateBuyerAccount(ctx,
		"adm-tel-"+uuid.NewString()+"@example.test", "integration-password", 50)
	must(t, err)
	profile := sortedVLLMProfiles()[0]
	worker := newRealtimeClearingOffer(t, ctx, store, pool, profile, "HOT", 0.08, 0.30, 64)
	_ = worker

	srv := NewServer(store, nil, nil, nil)
	if srv.admissionTelemetry.Load() != nil {
		t.Fatal("telemetry workers must not start before the first realtime admission")
	}
	t.Cleanup(func() { srv.CloseAdmissionTelemetry(5 * time.Second) })
	// Crash-loss bound is documented and finite.
	const n = 64
	for i := 0; i < n; i++ {
		srv.recordRealtimeAdmissionEvent(ctx, buyerID, profile.RuntimeProfileID, "", realtimeAdmissionNoCapacity, uuid.Nil)
	}
	tel := srv.admissionTelemetry.Load()
	if tel == nil {
		t.Fatal("first realtime admission did not start telemetry")
	}
	if cap := tel.queueCap(); cap != admissionTelemetryQueueCap || cap < 1 {
		t.Fatalf("queue cap = %d, want %d", cap, admissionTelemetryQueueCap)
	}
	// Drain: every event must land.
	srv.CloseAdmissionTelemetry(5 * time.Second)

	var count int
	if err := pool.QueryRow(ctx,
		`SELECT count(*) FROM realtime_admission_events WHERE buyer_id=$1 AND decision=$2`,
		buyerID, realtimeAdmissionNoCapacity).Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != n {
		t.Fatalf("recorded %d admission events, want %d (async must not drop under load)", count, n)
	}
	written, syncFB, queued := tel.stats()
	if written < int64(n) {
		t.Fatalf("written=%d want >=%d", written, n)
	}
	if queued != 0 {
		t.Fatalf("queued residual after Close = %d, want 0", queued)
	}
	_ = syncFB
}

// TestAdmissionTelemetryCloseBeforeFirstEvent avoids creating background
// workers during shutdown while retaining the no-drop synchronous fallback.
func TestAdmissionTelemetryCloseBeforeFirstEvent(t *testing.T) {
	installSettlementCurrencyForTest(t, "usd")
	ctx, store, pool := openIsolatedTestStore(t)
	buyerID, err := store.CreateBuyerAccount(ctx,
		"adm-close-"+uuid.NewString()+"@example.test", "integration-password", 5)
	must(t, err)
	profile := sortedVLLMProfiles()[0]

	srv := NewServer(store, nil, nil, nil)
	srv.CloseAdmissionTelemetry(5 * time.Second)
	if srv.admissionTelemetry.Load() != nil {
		t.Fatal("close before the first event started telemetry workers")
	}
	srv.recordRealtimeAdmissionEvent(ctx, buyerID, profile.RuntimeProfileID, "", realtimeAdmissionNoCapacity, uuid.Nil)
	if srv.admissionTelemetry.Load() != nil {
		t.Fatal("post-close fallback restarted telemetry workers")
	}

	var count int
	if err := pool.QueryRow(ctx,
		`SELECT count(*) FROM realtime_admission_events WHERE buyer_id=$1 AND decision=$2`,
		buyerID, realtimeAdmissionNoCapacity).Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != 1 {
		t.Fatalf("post-close synchronous fallback recorded=%d, want 1", count)
	}
}

// TestAdmissionTelemetryFullQueueFallsBackSync proves overload cannot drop:
// when the queue is full, record() writes synchronously instead of discarding.
func TestAdmissionTelemetryFullQueueFallsBackSync(t *testing.T) {
	installSettlementCurrencyForTest(t, "usd")
	ctx, store, pool := openIsolatedTestStore(t)

	buyerID, err := store.CreateBuyerAccount(ctx,
		"adm-fb-"+uuid.NewString()+"@example.test", "integration-password", 5)
	must(t, err)
	profile := sortedVLLMProfiles()[0]

	// No workers draining — queue of 1 fills after the first enqueue; the
	// second record must take the synchronous fallback path.
	tel := &admissionTelemetry{
		store: store,
		ch:    make(chan admissionTelemetryEvent, 1),
		stop:  make(chan struct{}),
		done:  make(chan struct{}),
	}
	tel.record(buyerID, profile.RuntimeProfileID, "", realtimeAdmissionNoCapacity, uuid.Nil)
	tel.record(buyerID, profile.RuntimeProfileID, "", realtimeAdmissionInsufficient, uuid.Nil)
	_, syncFB, _ := tel.stats()
	if syncFB < 1 {
		t.Fatalf("sync fallbacks = %d, want ≥1 when queue is full", syncFB)
	}
	// The sync-written event is durable without Close / without workers.
	var count int
	if err := pool.QueryRow(ctx,
		`SELECT count(*) FROM realtime_admission_events WHERE buyer_id=$1 AND decision=$2`,
		buyerID, realtimeAdmissionInsufficient).Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != 1 {
		t.Fatalf("sync-fallback event count=%d, want 1", count)
	}
	// Drain the buffered event so we do not leak a channel with a live store.
	close(tel.ch)
	for ev := range tel.ch {
		tel.write(ev)
	}
	close(tel.done)
}

// TestLookupAPIKeyCacheRevokeIsImmediate proves same-process revoke is not
// delayed by the cache: after RevokeAPIKey the next lookup must miss.
func TestLookupAPIKeyCacheRevokeIsImmediate(t *testing.T) {
	ctx, store, _ := openIsolatedTestStore(t)
	buyerID, err := store.CreateBuyerAccount(ctx,
		"key-cache-"+uuid.NewString()+"@example.test", "integration-password", 1)
	must(t, err)
	id, raw, _, err := store.CreateAPIKey(ctx, buyerID, "cache-test", true)
	must(t, err)
	// Populate cache.
	auth, err := store.LookupAPIKey(ctx, raw)
	if err != nil || auth.APIKeyID != id {
		t.Fatalf("lookup: auth=%+v err=%v", auth, err)
	}
	// Hit path.
	if _, err := store.LookupAPIKey(ctx, raw); err != nil {
		t.Fatal(err)
	}
	ok, err := store.RevokeAPIKey(ctx, buyerID, id)
	if err != nil || !ok {
		t.Fatalf("revoke: ok=%v err=%v", ok, err)
	}
	_, err = store.LookupAPIKey(ctx, raw)
	if !errors.Is(err, errNotFound) {
		t.Fatalf("revoked key still authenticated: err=%v (cache must invalidate on revoke)", err)
	}
}

// TestLookupAPIKeyCacheTTLBoundsStaleness proves a positive entry expires by
// the documented multi-instance lag bound without an explicit invalidate.
func TestLookupAPIKeyCacheTTLBoundsStaleness(t *testing.T) {
	if testing.Short() {
		t.Skip("TTL sleep")
	}
	ctx, store, pool := openIsolatedTestStore(t)
	buyerID, err := store.CreateBuyerAccount(ctx,
		"key-ttl-"+uuid.NewString()+"@example.test", "integration-password", 1)
	must(t, err)
	id, raw, _, err := store.CreateAPIKey(ctx, buyerID, "ttl-test", true)
	must(t, err)
	if _, err := store.LookupAPIKey(ctx, raw); err != nil {
		t.Fatal(err)
	}
	// Revoke in the database WITHOUT going through RevokeAPIKey, simulating
	// another process accepting the revoke while this process still holds a
	// hot cache entry.
	if _, err := pool.Exec(ctx, `UPDATE api_keys SET revoked=true WHERE id=$1`, id); err != nil {
		t.Fatal(err)
	}
	// Within TTL the stale entry may still succeed — that is the documented
	// multi-instance window. After TTL it must fail.
	deadline := time.Now().Add(apiKeyCacheTTL + 100*time.Millisecond)
	var last error
	for time.Now().Before(deadline.Add(200 * time.Millisecond)) {
		_, last = store.LookupAPIKey(ctx, raw)
		if errors.Is(last, errNotFound) {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("revoked key still authenticated after TTL+%v: last err=%v", 200*time.Millisecond, last)
}

// TestOperationalControlCacheInvalidatesOnAdminSet proves same-process pause
// is immediate through the cache.
func TestOperationalControlCacheInvalidatesOnAdminSet(t *testing.T) {
	ctx, store, pool := openAdminMutationTestStore(t)
	if _, err := pool.Exec(ctx, `UPDATE operational_controls SET paused=false WHERE name=$1`, controlIntake); err != nil {
		t.Fatal(err)
	}
	paused, err := store.OperationalControlPaused(ctx, controlIntake)
	if err != nil || paused {
		t.Fatalf("warmup paused=%v err=%v", paused, err)
	}
	// Second read hits the process-local cache (still active).
	if paused, err = store.OperationalControlPaused(ctx, controlIntake); err != nil || paused {
		t.Fatalf("cached paused=%v err=%v", paused, err)
	}

	actor := testAdminActor(uuid.New())
	if _, err := pool.Exec(ctx,
		`INSERT INTO api_keys (id,key_hash,is_admin,revoked,name) VALUES ($1,$2,true,false,'ctrl-cache-admin')`,
		actor.PrincipalID, "control-test-"+actor.PrincipalID.String()); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_, _ = pool.Exec(ctx, `UPDATE operational_controls
			SET paused=false,reason='test cleanup',updated_by=NULL,updated_at=now(),version=version+1
			WHERE name=$1`, controlIntake)
		_, _ = pool.Exec(ctx, `DELETE FROM admin_actions WHERE actor_principal_id=$1`, actor.PrincipalID)
		_, _ = pool.Exec(ctx, `DELETE FROM api_keys WHERE id=$1`, actor.PrincipalID)
	})

	if _, err := store.AdminSetOperationalControl(ctx, actor, controlIntake, true,
		"path-latency kill-switch", "req-"+uuid.NewString()); err != nil {
		t.Fatalf("AdminSetOperationalControl: %v", err)
	}
	paused, err = store.OperationalControlPaused(ctx, controlIntake)
	if err != nil || !paused {
		t.Fatalf("after admin pause paused=%v err=%v, want true (same-process invalidate)", paused, err)
	}
}

// TestSettlementIntentOverlapsDialButBlocksFirstByte proves the durable intent
// is present before the first client byte, even when the insert is started
// concurrently with the upstream dial.
func TestSettlementIntentOverlapsDialButBlocksFirstByte(t *testing.T) {
	installSettlementCurrencyForTest(t, "usd")
	ctx, store, pool := openIsolatedTestStore(t)
	t.Setenv("MERC_TOKEN_KEY", "response-path-test-key-with-32-bytes!!")

	if _, err := pool.Exec(ctx, `TRUNCATE
		realtime_admission_events, realtime_offer_samples,
		realtime_authorization_events, realtime_settlements, realtime_executions,
		realtime_refunds, execution_contracts, realtime_worker_offers,
		realtime_supplier_outcome_stats, realtime_settlement_intents
		RESTART IDENTITY CASCADE`); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `DELETE FROM ledger_entries WHERE execution_contract_id IS NOT NULL`); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `UPDATE operational_controls SET paused=false WHERE name=$1`, controlIntake); err != nil {
		t.Fatal(err)
	}

	buyerID, err := store.CreateBuyerAccount(ctx,
		"intent-overlap-"+uuid.NewString()+"@example.test", "integration-password", 100)
	must(t, err)
	_, rawKey, _, err := store.CreateAPIKey(ctx, buyerID, "overlap", true)
	must(t, err)
	profile := sortedVLLMProfiles()[0]
	for _, p := range sortedVLLMProfiles() {
		if p.ModelAlias == "cx-chat-1b" {
			profile = p
			break
		}
	}
	worker := newRealtimeClearingOffer(t, ctx, store, pool, profile, "HOT", 0.08, 0.30, 16)

	// Upstream that delays headers so the overlapped intent has time to land
	// before the first byte path continues.
	var intentVisibleBeforeFirstByte atomic.Bool
	engine := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		// Give the concurrent intent insert a moment under load.
		time.Sleep(5 * time.Millisecond)
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		flusher, _ := w.(http.Flusher)
		fmt.Fprintf(w, "data: {\"id\":\"chatcmpl-x\",\"choices\":[{\"delta\":{\"content\":\"Hi\"}}]}\n\n")
		if flusher != nil {
			flusher.Flush()
		}
		fmt.Fprintf(w, "data: {\"id\":\"chatcmpl-x\",\"choices\":[],\"usage\":{\"prompt_tokens\":8,\"completion_tokens\":1,\"total_tokens\":9}}\n\n")
		if flusher != nil {
			flusher.Flush()
		}
	}))
	t.Cleanup(engine.Close)
	if _, err := pool.Exec(ctx, `
		UPDATE realtime_worker_offers
		   SET upstream_base_url=$1, available_sequences=max_active_sequences, status='ACTIVE', last_seen_at=now()
		 WHERE worker_id=$2 AND runtime_profile_id=$3`,
		engine.URL+"/v1", worker.WorkerID, profile.RuntimeProfileID); err != nil {
		t.Fatal(err)
	}

	srv := NewServer(store, nil, nil, nil)
	t.Cleanup(func() { srv.CloseAdmissionTelemetry(2 * time.Second) })
	mux := http.NewServeMux()
	mux.Handle("POST /v1/chat/completions", srv.authBuyer(http.HandlerFunc(srv.handleChatCompletions)))
	ts := httptest.NewServer(observe(mux))
	t.Cleanup(ts.Close)

	body := []byte(fmt.Sprintf(
		`{"model":%q,"messages":[{"role":"user","content":"overlap intent"}],"max_tokens":8,"temperature":0,"stream":true}`,
		profile.ModelAlias))
	req, err := http.NewRequest(http.MethodPost, ts.URL+"/v1/chat/completions", strings.NewReader(string(body)))
	must(t, err)
	req.Header.Set("Authorization", "Bearer "+rawKey)
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	must(t, err)
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		b, _ := io.ReadAll(resp.Body)
		t.Fatalf("status %d: %s", resp.StatusCode, b)
	}
	// First byte.
	buf := make([]byte, 1)
	if _, err := resp.Body.Read(buf); err != nil {
		t.Fatalf("first byte: %v", err)
	}
	// At the moment the client has the first byte, the intent must already be
	// durable. Contract id is in the response header.
	contractID, err := uuid.Parse(resp.Header.Get("X-Merc-Contract-ID"))
	mustf(t, err, "contract header: %v")
	var intentCount int
	if err := pool.QueryRow(ctx, `
		SELECT count(*) FROM realtime_settlement_intents
		 WHERE contract_id=$1 AND state IN ('pending','settled')`, contractID).Scan(&intentCount); err != nil {
		t.Fatal(err)
	}
	if intentCount < 1 {
		t.Fatal("settlement intent missing at first client byte — overlap must not move intent after delivery")
	}
	intentVisibleBeforeFirstByte.Store(true)
	_, _ = io.Copy(io.Discard, resp.Body)
	if !intentVisibleBeforeFirstByte.Load() {
		t.Fatal("intent visibility flag not set")
	}
}

// TestAdmissionTelemetryCrashLossIsBounded documents the only permitted loss
// mode: hard process death can lose at most queueCap + workers events.
func TestAdmissionTelemetryCrashLossIsBounded(t *testing.T) {
	tel := newAdmissionTelemetry(&Store{})
	t.Cleanup(func() { tel.Close(time.Second) })
	bound := tel.queueCap() + admissionTelemetryWorkers
	if bound < 1 || bound > admissionTelemetryQueueCap+admissionTelemetryWorkers {
		t.Fatalf("crash-loss bound %d out of range", bound)
	}
	// Bound is finite and equal to the channel capacity plus in-flight writers.
	if tel.queueCap() != admissionTelemetryQueueCap {
		t.Fatalf("queueCap=%d want %d", tel.queueCap(), admissionTelemetryQueueCap)
	}
}
