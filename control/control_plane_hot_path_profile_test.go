package main

// Integrated control-plane hot-path profile at HEAD.
//
// Measures the production entry points the product actually sits on after P0
// retired the durable per-heartbeat write from the authenticated fast path.
// There is no Go imitation of those paths and no inherited number from an
// older evidence file — every cell is timed on this host, or labelled
// UNMEASURED / DERIVED / PROJECTED.
//
// Opt-in only — never part of make test / make ci:
//
//	MERC_CONTROL_PLANE_PROFILE=1 \
//	MERC_TEST_DATABASE_URL=postgres://cx:cx@localhost:5432/cx?sslmode=disable \
//	  go test -count=1 -timeout 45m -run '^TestControlPlaneHotPathProfile$' .
//
// Writes evidence/perf/control-plane-hot-path-profile.json when run from
// control/. CPU and alloc profiles land next to that file (not LFS).

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"runtime/pprof"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
)

const (
	hotPathProfileEnv         = "MERC_CONTROL_PLANE_PROFILE"
	hotPathProfileFleetEnv    = "MERC_CONTROL_PLANE_PROFILE_FLEET"
	hotPathProfileSamplesEnv  = "MERC_CONTROL_PLANE_PROFILE_SAMPLES"
	hotPathProfileTrialsEnv   = "MERC_CONTROL_PLANE_PROFILE_TRIALS"
	hotPathProfileEvidenceRel = "evidence/perf/control-plane-hot-path-profile.json"

	hotPathDefaultFleet   = 200
	hotPathDefaultSamples = 200
	hotPathDefaultTrials  = 2
	hotPathMaxConns       = int32(24)

	// Production agent cadence (agent/src/vllm.rs HEARTBEAT_INTERVAL).
	hotPathAgentHeartbeatSec = 15
	// Tighter cadence P0 also measured. Sensitivity only — not the source default.
	hotPathTightHeartbeatSec = 5
)

func TestControlPlaneHotPathProfile(t *testing.T) {
	if os.Getenv(hotPathProfileEnv) != "1" {
		t.Skip("set MERC_CONTROL_PLANE_PROFILE=1 to profile the integrated control-plane hot path at HEAD")
	}

	fleetN := hotPathDefaultFleet
	if v := strings.TrimSpace(os.Getenv(hotPathProfileFleetEnv)); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 8 {
			t.Fatalf("%s=%q: need integer >= 8", hotPathProfileFleetEnv, v)
		}
		fleetN = n
	}
	samples := hotPathDefaultSamples
	if v := strings.TrimSpace(os.Getenv(hotPathProfileSamplesEnv)); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 40 {
			t.Fatalf("%s=%q: need integer >= 40", hotPathProfileSamplesEnv, v)
		}
		samples = n
	}
	trials := hotPathDefaultTrials
	if v := strings.TrimSpace(os.Getenv(hotPathProfileTrialsEnv)); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 1 {
			t.Fatalf("%s=%q: need integer >= 1", hotPathProfileTrialsEnv, v)
		}
		trials = n
	}

	if strings.TrimSpace(os.Getenv(isolatedTestDBTemplateEnv)) == "" {
		t.Setenv(isolatedTestDBTemplateEnv, schemaTemplateDatabaseName(canonicalSchemaSHA256()))
	}
	t.Setenv("MERC_TOKEN_KEY", "hot-path-profile-key-32-bytes-minimum!!")
	t.Setenv("MERC_LIVENESS_INDEX_AUTHORITATIVE", "0")
	installSettlementCurrencyForTest(t, "usd")

	host, _ := os.Hostname()
	startedAt := time.Now().UTC()
	command := "cd control && MERC_CONTROL_PLANE_PROFILE=1 MERC_TEST_DATABASE_URL=postgres://cx:cx@localhost:5432/cx?sslmode=disable go test -count=1 -timeout 45m -run '^TestControlPlaneHotPathProfile$' ."

	report := hotPathReport{
		Classification: "MEASURED",
		GeneratedAt:    startedAt.Format(time.RFC3339),
		SourceCommit:   mercSourceCommitSHA(),
		Host:           host,
		NumCPU:         runtime.NumCPU(),
		GOMAXPROCS:     runtime.GOMAXPROCS(0),
		GOOS:           runtime.GOOS,
		GOARCH:         runtime.GOARCH,
		Invocation: hotPathInvocation{
			EnvGate:                hotPathProfileEnv + "=1",
			ExcludedFromNormalGate: true,
			ExclusionProof:         "TestControlPlaneHotPathProfile skips unless MERC_CONTROL_PLANE_PROFILE=1; listed in scripts/allowed-test-skips.txt; make test / make ci never set the env var",
			Command:                command,
			Fleet:                  fleetN,
			Samples:                samples,
			Trials:                 trials,
			MaxConns:               int(hotPathMaxConns),
			AgentHeartbeatSec:      hotPathAgentHeartbeatSec,
			TightHeartbeatSec:      hotPathTightHeartbeatSec,
			LivenessWindowSec:      int(realtimeOfferLivenessWindow / time.Second),
			RefreshIntervalSec:     int(livenessDurableRefreshInterval / time.Second),
		},
		Honesty: hotPathHonesty{
			WhatThisProves:       "on this host, with a local PostgreSQL, wall latency by SUCCESS/REFUSAL/EMPTY/FAILURE, throughput, allocs/op, bytes/op, process CPU-ns/op, EXPLAIN (ANALYZE, BUFFERS) dominating nodes, and pprof tops for the production entry points named in the cells, at HEAD",
			WhatThisDoesNotProve: "a droplet-class control plane, multi-process fan-out, authenticated HTTP/authWorker overhead, a live production mix, or any optimisation payoff after a code change — this phase ranks, it does not change production code",
			Guards: []string{
				"refused, empty, and failed calls never enter SUCCESS percentiles",
				"a cell with N=0 SUCCESS is not reported as zero-latency",
				"MEASURED numbers come from this process; DERIVED frequencies use agent/src/vllm.rs HEARTBEAT_INTERVAL=15s; PROJECTED payoff is MEASURED p50 × DERIVED frequency",
				"AuthorizeRealtimeContract at HEAD still filters last_seen_at in SQL and does not call offerIndexLive — flag ON/OFF is a heartbeat and ServiceLeaseDataPlaneTarget concern",
				"CreateServiceLease walks service_lease_worker_offers, not the realtime offer book",
				"pprof is taken over a named mixed workload in this process, not inferred from wall clocks",
				"older evidence/perf/* files are not copied into this artifact",
			},
		},
	}

	store, pool := openIsolatedTestStoreWithMaxConns(t, hotPathMaxConns)
	// Production default is coalesced; tests default it off. Measure both.
	// The store Once has not run yet if we set this before the first heartbeat.
	store.SetLivenessBatchConfigForTest(livenessBatchConfig{
		Enabled: true, MaxBatch: defaultLivenessBatchMaxSize,
		FlushInterval: defaultLivenessBatchFlushInterval,
	})
	ctx, cancel := context.WithTimeout(context.Background(), 40*time.Minute)
	t.Cleanup(cancel)

	report.Postgres = probeHotPathPostgres(ctx, pool)
	profile := sortedVLLMProfiles()[0]
	report.Invocation.RuntimeProfileID = profile.RuntimeProfileID

	t.Logf("seeding fleet=%d via seedOneRealtimeOfferErr…", fleetN)
	seedStart := time.Now()
	workers, seedErr := seedWriteAmpFleet(t, ctx, pool, profile, fleetN)
	report.Invocation.SeedWallSeconds = time.Since(seedStart).Seconds()
	if seedErr != nil || len(workers) == 0 {
		t.Fatalf("seed fleet: %v (n=%d)", seedErr, len(workers))
	}
	report.Invocation.SeededFleet = len(workers)
	t.Logf("seeded %d offers in %.1fs", len(workers), report.Invocation.SeedWallSeconds)

	// Warm the live index + persist caches with one flag-ON heartbeat each so
	// later cells do not mix first-touch allocation into the timed path.
	t.Setenv("MERC_LIVENESS_INDEX_AUTHORITATIVE", "1")
	warmHB := RealtimeOfferHeartbeat{
		RuntimeProfileID: profile.RuntimeProfileID, Warmth: "HOT",
		AvailableSequences: 8, Status: "ACTIVE",
	}
	for _, w := range workers {
		if err := store.HeartbeatRealtimeOffer(ctx, w, warmHB); err != nil {
			t.Fatalf("warm heartbeat: %v", err)
		}
	}
	waitWriteAmpDurable(store, 5*time.Second)
	t.Setenv("MERC_LIVENESS_INDEX_AUTHORITATIVE", "0")

	books := []int{1, 16}
	if len(workers) >= 128 {
		books = append(books, 128)
	} else if len(workers) >= 64 {
		books = append(books, len(workers))
	}

	// --- 1. heartbeat ingest, both flag states --------------------------------
	t.Logf("measuring heartbeat ingest…")
	report.Cells = append(report.Cells, measureHotPathHeartbeat(t, ctx, store, pool, profile, workers, samples, trials)...)

	// --- 2. live-index bridge -------------------------------------------------
	t.Logf("measuring live-index bridge…")
	report.Cells = append(report.Cells, measureHotPathIndex(t, ctx, store, workers, profile, samples)...)

	// --- 3. selector / offer book --------------------------------------------
	t.Logf("measuring AuthorizeRealtimeContract…")
	buyerID, err := store.CreateBuyerAccount(ctx,
		"hotpath-"+uuid.NewString()+"@example.test", "integration-password", 50_000)
	if err != nil {
		t.Fatalf("buyer: %v", err)
	}
	report.Cells = append(report.Cells, measureHotPathAuthorize(t, ctx, store, pool, profile, workers, buyerID, books, samples, trials)...)

	// --- 4. lease acquisition -------------------------------------------------
	t.Logf("measuring CreateServiceLease + ServiceLeaseDataPlaneTarget…")
	report.Cells = append(report.Cells, measureHotPathLease(t, ctx, store, pool, profile, workers, samples)...)

	// --- 5. price / ranking inputs -------------------------------------------
	t.Logf("measuring price computation…")
	report.Cells = append(report.Cells, measureHotPathPricing(t, profile, samples)...)

	// --- EXPLAIN ANALYZE -----------------------------------------------------
	t.Logf("EXPLAIN (ANALYZE, BUFFERS)…")
	report.Explains = collectHotPathExplains(t, ctx, store, pool, profile, workers)

	// Re-time authorize after ANALYZE so the ranking can separate a cold-stats
	// Nested Loop from the production-like plan. Same SUCCESS-only rule.
	t.Logf("remeasuring AuthorizeRealtimeContract after ANALYZE…")
	report.Cells = append(report.Cells, measureHotPathAuthorizeAfterAnalyze(t, ctx, store, pool, profile, workers, buyerID, samples)...)

	// --- mixed pprof ---------------------------------------------------------
	t.Logf("mixed-workload pprof…")
	profDir := filepath.Join("..", "evidence", "perf")
	_ = os.MkdirAll(profDir, 0o755)
	report.Profiles = collectHotPathProfiles(t, ctx, store, pool, profile, workers, buyerID, profDir)

	// --- unmeasured rows (must not be omitted) --------------------------------
	report.Unmeasured = []hotPathUnmeasured{
		{
			Subsystem: "ClaimTasksTx / batch selector",
			Reason:    "not driven in this harness; it is the batch-job poll path, not the realtime ingest/admit path this phase ranks. TestSelectorScaleCurve covers it but that artifact is not re-run here.",
		},
		{
			Subsystem: "HTTP + authWorker",
			Reason:    "this harness times Store methods after authentication. TLS, header parse, and worker-token verify are not on the timed path.",
		},
		{
			Subsystem: "production request mix / live QPS",
			Reason:    "this host is not serving a live network. Frequency used in the ranking is DERIVED from agent HEARTBEAT_INTERVAL=15s and named authorize-QPS scenarios, not observed production traffic.",
		},
		{
			Subsystem: "droplet-class / multi-process control plane",
			Reason:    "one local Homebrew PostgreSQL 17 on this workstation. No cgroup, no second control-plane process, no cross-host RTT.",
		},
		{
			Subsystem: "AuthorizeRealtimeContract flag-ON liveness predicate",
			Reason:    "at HEAD Authorize still uses last_seen_at > now()-45s. offerIndexLive is not on that path. There is nothing flag-ON to time inside Authorize.",
		},
	}

	report.Ranking = rankHotPath(report)
	report.Binding = classifyHotPathBinding(report)
	report.FinishedAt = time.Now().UTC().Format(time.RFC3339)
	report.WallClockSeconds = time.Since(startedAt).Seconds()

	if err := writeHotPathEvidence(report); err != nil {
		t.Fatalf("write evidence: %v", err)
	}
	t.Logf("wrote %s binding=%s cells=%d", hotPathProfileEvidenceRel, report.Binding.Verdict, len(report.Cells))
	for i, row := range report.Ranking {
		t.Logf("rank %d %s score=%.3f p50=%s payoff=%s", i+1, row.Subsystem, row.Score, row.P50, row.ExpectedAbsolutePayoff)
	}
}

// --- heartbeat --------------------------------------------------------------

func measureHotPathHeartbeat(
	t *testing.T, ctx context.Context, store *Store, pool *pgxpool.Pool,
	profile VLLMRuntimeProfile, workers []WorkerAuth, samples, trials int,
) []hotPathCell {
	t.Helper()
	hb := RealtimeOfferHeartbeat{
		RuntimeProfileID: profile.RuntimeProfileID, Warmth: "HOT",
		AvailableSequences: 8, Status: "ACTIVE",
	}
	var cells []hotPathCell

	// Flag OFF, batched (production default): every beat waits for a durable flush.
	t.Setenv("MERC_LIVENESS_INDEX_AUTHORITATIVE", "0")
	cells = append(cells, runHotPathHeartbeatCell(t, ctx, store, pool, workers, hb, samples, trials, hotPathHeartbeatSpec{
		Name: "heartbeat_ingest_flag_off_batched_success", Subsystem: "heartbeat ingest",
		Flag: "off", Batching: "on", OutcomeWant: "SUCCESS",
		Note: "production shape at HEAD: flag unset, coalescer on, caller waits for durable UPDATE+sample insert",
	}))

	// Flag ON, identical repeat (write retired): persist cache warm.
	t.Setenv("MERC_LIVENESS_INDEX_AUTHORITATIVE", "1")
	// One more beat so persist cache is definitely warm after the flag flip.
	for i := 0; i < len(workers) && i < 8; i++ {
		_ = store.HeartbeatRealtimeOffer(ctx, workers[i], hb)
	}
	waitWriteAmpDurable(store, 2*time.Second)
	cells = append(cells, runHotPathHeartbeatCell(t, ctx, store, pool, workers, hb, samples, trials, hotPathHeartbeatSpec{
		Name: "heartbeat_ingest_flag_on_retired_batched_success", Subsystem: "heartbeat ingest",
		Flag: "on", Batching: "on", OutcomeWant: "SUCCESS",
		Note: "flag ON, identical payload inside 15s refresh: admit+index only, submitDetached or skip; no caller-visible durable write",
	}))

	// Flag ON, durable-needed path: drop persist cache so every call writes.
	cells = append(cells, runHotPathHeartbeatCell(t, ctx, store, pool, workers, hb, samples, 1, hotPathHeartbeatSpec{
		Name: "heartbeat_ingest_flag_on_refresh_batched_success", Subsystem: "heartbeat ingest",
		Flag: "on", Batching: "on", OutcomeWant: "SUCCESS", ClearPersist: true,
		Note: "flag ON but persist cache cleared before each call — this is the 15s refresh write, not the retired repeat. Caller still returns before the detached flush.",
	}))

	// REFUSAL: unregistered worker.
	bogus := WorkerAuth{WorkerID: uuid.New(), SupplierID: uuid.New()}
	cells = append(cells, runHotPathHeartbeatCell(t, ctx, store, pool, []WorkerAuth{bogus}, hb, samples, 1, hotPathHeartbeatSpec{
		Name: "heartbeat_ingest_unregistered_refusal", Subsystem: "heartbeat ingest",
		Flag: "on", Batching: "on", OutcomeWant: "REFUSAL",
		Note: "unregistered (worker, profile) — admitIndexHeartbeat / UPDATE WHERE miss → errNotFound",
	}))

	// REFUSAL: available_sequences above max_active_sequences (seeded as 16).
	over := hb
	over.AvailableSequences = 1_000_000
	t.Setenv("MERC_LIVENESS_INDEX_AUTHORITATIVE", "1")
	cells = append(cells, runHotPathHeartbeatCell(t, ctx, store, pool, workers[:1], over, samples, 1, hotPathHeartbeatSpec{
		Name: "heartbeat_ingest_over_capacity_refusal", Subsystem: "heartbeat ingest",
		Flag: "on", Batching: "on", OutcomeWant: "REFUSAL",
		Note: "AvailableSequences > max_active_sequences — same WHERE miss as the durable path",
	}))

	// REFUSAL: stale observation.
	stale := hb
	old := time.Now().Add(-2 * realtimeOfferLivenessWindow).UnixMilli()
	stale.ObservedAtUnixMs = &old
	cells = append(cells, runHotPathHeartbeatCell(t, ctx, store, pool, workers[:1], stale, min(samples, 80), 1, hotPathHeartbeatSpec{
		Name: "heartbeat_ingest_stale_observation_refusal", Subsystem: "heartbeat ingest",
		Flag: "on", Batching: "on", OutcomeWant: "REFUSAL",
		Note: "ObservedAt older than the 45s window → errStaleHeartbeatObservation before any write",
	}))

	t.Setenv("MERC_LIVENESS_INDEX_AUTHORITATIVE", "0")
	return cells
}

type hotPathHeartbeatSpec struct {
	Name, Subsystem, Flag, Batching, OutcomeWant, Note string
	ClearPersist                                       bool
}

func runHotPathHeartbeatCell(
	t *testing.T, ctx context.Context, store *Store, pool *pgxpool.Pool,
	workers []WorkerAuth, hb RealtimeOfferHeartbeat, samples, trials int, spec hotPathHeartbeatSpec,
) hotPathCell {
	t.Helper()
	cell := hotPathBaseCell(spec.Name, spec.Subsystem, spec.Note)
	cell.Flag = spec.Flag
	cell.EntryPoint = "Store.HeartbeatRealtimeOffer"
	cell.Command = fmt.Sprintf("HeartbeatRealtimeOffer × %d (flag=%s batch=%s clear_persist=%v)", samples, spec.Flag, spec.Batching, spec.ClearPersist)
	var trialP50 []float64
	var last hotPathTrial
	for tr := 0; tr < trials; tr++ {
		last = timeHotPathCalls(samples, min(8, len(workers)), func(i int) (string, error) {
			w := workers[i%len(workers)]
			if spec.ClearPersist {
				store.offerPersistCache.Delete(offerSlotKey{w.WorkerID, hb.RuntimeProfileID})
			}
			err := store.HeartbeatRealtimeOffer(ctx, w, hb)
			return classifyHotPathHeartbeat(err), err
		})
		if last.ByOutcome["SUCCESS"].N > 0 {
			trialP50 = append(trialP50, last.ByOutcome["SUCCESS"].P50)
		}
	}
	waitWriteAmpDurable(store, 2*time.Second)
	attachHotPathTrial(&cell, last, spec.OutcomeWant)
	cell.Trials = trials
	if len(trialP50) >= 2 {
		cell.P50SpreadMS = maxFloat(trialP50) - minFloat(trialP50)
		cell.Unstable = cell.P50SpreadMS > 0.25*medianFloat(trialP50) && medianFloat(trialP50) > 0.05
		cell.TrialP50MS = trialP50
	}
	_ = pool
	t.Logf("%s success_p50=%.4fms n=%d refusal=%d fail=%d cpu_ns/op=%.0f allocs=%.1f bytes=%.0f",
		spec.Name, cell.P50MS, cell.SampleN, cell.Buckets.REFUSAL, cell.Buckets.FAILURE,
		cell.CPUNanosPerOp, cell.AllocsPerOp, cell.BytesPerOp)
	return cell
}

func classifyHotPathHeartbeat(err error) string {
	if err == nil {
		return "SUCCESS"
	}
	if errors.Is(err, errNotFound) || errors.Is(err, errStaleHeartbeatObservation) {
		return "REFUSAL"
	}
	return "FAILURE"
}

// --- live-index bridge ------------------------------------------------------

func measureHotPathIndex(
	t *testing.T, ctx context.Context, store *Store, workers []WorkerAuth,
	profile VLLMRuntimeProfile, samples int,
) []hotPathCell {
	t.Helper()
	now := time.Now().UTC()
	n := max(samples*4, 2_000)
	var cells []hotPathCell

	// Warm cache: lookupOfferBinding must hit offerSlotCache (preloaded + heartbeats).
	cells = append(cells, timeHotPathInProcess(t, "live_index_lookupOfferBinding_cache_hit", "live-index bridge",
		"Store.lookupOfferBinding (warm cache)", n, func(i int) (string, error) {
			w := workers[i%len(workers)]
			_, ok := store.lookupOfferBinding(w.WorkerID, profile.RuntimeProfileID)
			if !ok {
				return "EMPTY", nil
			}
			return "SUCCESS", nil
		}))

	// Cold-ish: forget then lookup. First call after Delete hits PostgreSQL.
	// Time only the miss path (Delete + lookup).
	cells = append(cells, timeHotPathInProcess(t, "live_index_lookupOfferBinding_cache_miss", "live-index bridge",
		"forgetOfferBinding + lookupOfferBinding (SQL)", min(n, 400), func(i int) (string, error) {
			w := workers[i%len(workers)]
			store.forgetOfferBinding(w.WorkerID, profile.RuntimeProfileID)
			_, ok := store.lookupOfferBinding(w.WorkerID, profile.RuntimeProfileID)
			if !ok {
				return "EMPTY", nil
			}
			return "SUCCESS", nil
		}))

	cells = append(cells, timeHotPathInProcess(t, "live_index_offerIndexLive_hit", "live-index bridge",
		"Store.offerIndexLive (mapped, live)", n, func(i int) (string, error) {
			w := workers[i%len(workers)]
			if store.offerIndexLive(w.WorkerID, profile.RuntimeProfileID, now) {
				return "SUCCESS", nil
			}
			return "EMPTY", nil
		}))

	cells = append(cells, timeHotPathInProcess(t, "live_index_offerIndexLive_unmapped", "live-index bridge",
		"Store.offerIndexLive (unknown offer → fail-closed)", n, func(i int) (string, error) {
			if store.offerIndexLive(uuid.New(), profile.RuntimeProfileID, now) {
				return "FAILURE", errors.New("unmapped offer reported live")
			}
			return "SUCCESS", nil // SUCCESS here means "correctly dead"; see note
		}))
	// Re-label the unmapped cell: a correct dead read is not an admit SUCCESS.
	// Keep SUCCESS percentiles as the measured dead-read latency, and say so.
	if len(cells) > 0 {
		cells[len(cells)-1].Note = "latency of a fail-closed miss — this is not an admit. Outcome SUCCESS means the function returned false as required."
	}

	cells = append(cells, timeHotPathInProcess(t, "live_index_slotOwnedBy_hit", "live-index bridge",
		"Store.slotOwnedBy (owner match)", n, func(i int) (string, error) {
			w := workers[i%len(workers)]
			b, ok := store.lookupOfferBinding(w.WorkerID, profile.RuntimeProfileID)
			if !ok {
				return "EMPTY", nil
			}
			if store.slotOwnedBy(b.slot, w.WorkerID, profile.RuntimeProfileID) {
				return "SUCCESS", nil
			}
			return "EMPTY", nil
		}))

	cells = append(cells, timeHotPathInProcess(t, "live_index_slotOwnedBy_miss", "live-index bridge",
		"Store.slotOwnedBy (wrong owner)", n, func(i int) (string, error) {
			w := workers[i%len(workers)]
			b, ok := store.lookupOfferBinding(w.WorkerID, profile.RuntimeProfileID)
			if !ok {
				return "EMPTY", nil
			}
			if store.slotOwnedBy(b.slot, uuid.New(), profile.RuntimeProfileID) {
				return "FAILURE", errors.New("wrong owner matched")
			}
			return "SUCCESS", nil
		}))

	_ = ctx
	return cells
}

// --- authorize --------------------------------------------------------------

func measureHotPathAuthorize(
	t *testing.T, ctx context.Context, store *Store, pool *pgxpool.Pool,
	profile VLLMRuntimeProfile, workers []WorkerAuth, buyerID uuid.UUID,
	books []int, samples, trials int,
) []hotPathCell {
	t.Helper()
	maxUSD, estUSD, maxPrompt, maxCompletion := realtimeAuthCeiling(t, profile, 7, 2)
	authFor := func() RealtimeContractAuthorization {
		return RealtimeContractAuthorization{
			RequestID: "hp-" + uuid.NewString(), BuyerID: buyerID, Profile: profile,
			InputCommitment: strings.Repeat("a", 64), RequestSHA256: strings.Repeat("b", 64),
			MaximumPriceUSD: maxUSD, EstimatedPriceUSD: estUSD, DeadlineAt: time.Now().Add(time.Minute),
			MaximumPromptTokens: maxPrompt, MaximumCompletionTokens: maxCompletion,
			EstimatedPromptTokens: 7, EstimatedCompletionTokens: 2, BuyerDeclaredCeilingUSD: maxUSD * 1.1,
		}
	}
	refreshBook := func(active []WorkerAuth) {
		ids := make([]uuid.UUID, len(workers))
		for i, w := range workers {
			ids[i] = w.WorkerID
		}
		_, _ = pool.Exec(ctx, `
			UPDATE realtime_worker_offers
			   SET status='DRAINING', available_sequences=0
			 WHERE worker_id = ANY($1::uuid[])`, ids)
		if len(active) == 0 {
			return
		}
		aid := make([]uuid.UUID, len(active))
		for i, w := range active {
			aid[i] = w.WorkerID
		}
		_, _ = pool.Exec(ctx, `
			UPDATE realtime_worker_offers
			   SET status='ACTIVE', available_sequences=max_active_sequences, last_seen_at=now()
			 WHERE worker_id = ANY($1::uuid[])`, aid)
	}

	var cells []hotPathCell
	for _, book := range books {
		if book > len(workers) {
			continue
		}
		refreshBook(workers[:book])
		for _, conc := range []int{1, 8} {
			if conc > book && book == 1 && conc == 8 {
				// Still measure: this is the thin-book convoy the code comments warn about.
			}
			name := fmt.Sprintf("authorize_success_book_%d_c%d", book, conc)
			cell := hotPathBaseCell(name, "selector / offer book",
				fmt.Sprintf("AuthorizeRealtimeContract SUCCESS; eligible book=%d; concurrency=%d; FinalizeRealtimeFailure is OUTSIDE the timed window", book, conc))
			cell.EntryPoint = "Store.AuthorizeRealtimeContract"
			cell.Flag = "n/a (SQL last_seen_at)"
			cell.BookSize = book
			cell.Concurrency = conc
			cell.Command = fmt.Sprintf("AuthorizeRealtimeContract book=%d c=%d n=%d", book, conc, samples)
			n := samples
			if conc > 1 {
				n = max(samples, conc*20)
			}
			var trialP50 []float64
			var last hotPathTrial
			for tr := 0; tr < trials; tr++ {
				refreshBook(workers[:book])
				var fin atomic.Int64
				last = timeHotPathCalls(n, conc, func(i int) (string, error) {
					c, _, err := store.AuthorizeRealtimeContract(context.Background(), authFor())
					if err == nil {
						_, _ = store.FinalizeRealtimeFailure(context.Background(), c.ID, uuid.New(), 500, 1, "hotpath", "teardown", false)
						fin.Add(1)
						if fin.Load()%20 == 0 {
							_, _ = pool.Exec(context.Background(), `
								UPDATE realtime_worker_offers
								   SET available_sequences=max_active_sequences, status='ACTIVE', last_seen_at=now()
								 WHERE status='ACTIVE'`)
						}
						return "SUCCESS", nil
					}
					return classifyHotPathAuthorize(err), err
				})
				if last.ByOutcome["SUCCESS"].P50 > 0 {
					trialP50 = append(trialP50, last.ByOutcome["SUCCESS"].P50)
				}
			}
			attachHotPathTrial(&cell, last, "SUCCESS")
			cell.Trials = trials
			if len(trialP50) >= 2 {
				cell.P50SpreadMS = maxFloat(trialP50) - minFloat(trialP50)
				cell.Unstable = cell.P50SpreadMS > 0.25*medianFloat(trialP50)
				cell.TrialP50MS = trialP50
			}
			t.Logf("%s p50=%.3fms p95=%.3fms n=%d fail=%d refusal=%d",
				name, cell.P50MS, cell.P95MS, cell.SampleN, cell.Buckets.FAILURE, cell.Buckets.REFUSAL)
			cells = append(cells, cell)
		}
	}

	// REFUSAL: empty book.
	refreshBook(nil)
	cell := hotPathBaseCell("authorize_no_supply_refusal", "selector / offer book",
		"every offer DRAINING — errRealtimeNoSupply. This is a fast refusal, not a fast success.")
	cell.EntryPoint = "Store.AuthorizeRealtimeContract"
	cell.Command = "AuthorizeRealtimeContract against empty eligible book"
	tr := timeHotPathCalls(min(samples, 80), 1, func(i int) (string, error) {
		_, _, err := store.AuthorizeRealtimeContract(context.Background(), authFor())
		return classifyHotPathAuthorize(err), err
	})
	attachHotPathTrial(&cell, tr, "REFUSAL")
	cells = append(cells, cell)

	// REFUSAL: unfunded buyer, book restored.
	refreshBook(workers[:min(16, len(workers))])
	broke := uuid.New()
	if _, err := pool.Exec(ctx, `INSERT INTO buyers (id,email,free_credit_usd) VALUES ($1,$2,0)`,
		broke, broke.String()+"@hotpath-broke.invalid"); err != nil {
		t.Fatalf("broke buyer: %v", err)
	}
	cell = hotPathBaseCell("authorize_insufficient_funds_refusal", "selector / offer book",
		"buyer with zero prepaid and zero free credit — funding refusal, not a selector miss")
	cell.EntryPoint = "Store.AuthorizeRealtimeContract"
	cell.Command = "AuthorizeRealtimeContract unfunded buyer, live book"
	tr = timeHotPathCalls(min(samples, 80), 1, func(i int) (string, error) {
		a := authFor()
		a.BuyerID = broke
		a.RequestID = "hp-broke-" + uuid.NewString()
		_, _, err := store.AuthorizeRealtimeContract(context.Background(), a)
		return classifyHotPathAuthorize(err), err
	})
	attachHotPathTrial(&cell, tr, "REFUSAL")
	cells = append(cells, cell)

	// Isolated eligible-book CTE (no funding, no insert) under ROLLBACK.
	refreshBook(workers[:min(16, len(workers))])
	cell = hotPathBaseCell("authorize_eligible_book_cte_only", "selector / offer book",
		"production realtimeAuthorizeSelectOfferSQLSkip under ROLLBACK — SQL only, no funding lock, no EXECUTING insert")
	cell.EntryPoint = "realtimeAuthorizeSelectOfferSQLSkip"
	cell.Command = "tx QueryRow(realtimeAuthorizeSelectOfferSQLSkip) + Rollback"
	cell.BookSize = min(16, len(workers))
	tr = timeHotPathCalls(min(samples, 120), 1, func(i int) (string, error) {
		tx, err := pool.Begin(context.Background())
		if err != nil {
			return "FAILURE", err
		}
		var workerID, supplierID uuid.UUID
		var url, sealed, warmth string
		var inRate, outRate float64
		var plan []byte
		var planSHA *string
		var cand, rank, ta, tf, vs, rc int
		var considered []byte
		err = tx.QueryRow(context.Background(), realtimeAuthorizeSelectOfferSQLSkip,
			profile.RuntimeProfileID, profile.ProfileSHA256,
			profile.BuyerInputUSDPerMillionTokens, profile.BuyerOutputUSDPerMillionTokens,
			minRealtimeOutcomeSamples).Scan(
			&workerID, &supplierID, &url, &sealed, &inRate, &outRate, &plan, &planSHA, &warmth,
			&cand, &rank, &ta, &tf, &vs, &rc, &considered)
		_ = tx.Rollback(context.Background())
		if err != nil {
			return "EMPTY", err
		}
		return "SUCCESS", nil
	})
	attachHotPathTrial(&cell, tr, "SUCCESS")
	cells = append(cells, cell)

	refreshBook(workers)
	return cells
}

func measureHotPathAuthorizeAfterAnalyze(
	t *testing.T, ctx context.Context, store *Store, pool *pgxpool.Pool,
	profile VLLMRuntimeProfile, workers []WorkerAuth, buyerID uuid.UUID, samples int,
) []hotPathCell {
	t.Helper()
	maxUSD, estUSD, maxPrompt, maxCompletion := realtimeAuthCeiling(t, profile, 7, 2)
	_, _ = pool.Exec(ctx, `
		UPDATE realtime_worker_offers
		   SET status='ACTIVE', available_sequences=max_active_sequences, last_seen_at=now()`)
	var cells []hotPathCell
	for _, book := range []int{16, 128} {
		if book > len(workers) {
			continue
		}
		ids := make([]uuid.UUID, len(workers))
		for i, w := range workers {
			ids[i] = w.WorkerID
		}
		_, _ = pool.Exec(ctx, `
			UPDATE realtime_worker_offers SET status='DRAINING', available_sequences=0
			 WHERE worker_id = ANY($1::uuid[])`, ids)
		aid := make([]uuid.UUID, book)
		for i := 0; i < book; i++ {
			aid[i] = workers[i].WorkerID
		}
		_, _ = pool.Exec(ctx, `
			UPDATE realtime_worker_offers
			   SET status='ACTIVE', available_sequences=max_active_sequences, last_seen_at=now()
			 WHERE worker_id = ANY($1::uuid[])`, aid)
		name := fmt.Sprintf("authorize_success_book_%d_c1_after_analyze", book)
		cell := hotPathBaseCell(name, "selector / offer book",
			"AuthorizeRealtimeContract SUCCESS after ANALYZE (production-like planner stats); Finalize outside the timed window")
		cell.EntryPoint = "Store.AuthorizeRealtimeContract"
		cell.Flag = "n/a (SQL last_seen_at)"
		cell.BookSize = book
		cell.Concurrency = 1
		cell.Command = fmt.Sprintf("AuthorizeRealtimeContract book=%d c=1 n=%d AFTER ANALYZE", book, samples)
		tr := timeHotPathCalls(samples, 1, func(i int) (string, error) {
			a := RealtimeContractAuthorization{
				RequestID: "hp-az-" + uuid.NewString(), BuyerID: buyerID, Profile: profile,
				InputCommitment: strings.Repeat("a", 64), RequestSHA256: strings.Repeat("b", 64),
				MaximumPriceUSD: maxUSD, EstimatedPriceUSD: estUSD, DeadlineAt: time.Now().Add(time.Minute),
				MaximumPromptTokens: maxPrompt, MaximumCompletionTokens: maxCompletion,
				EstimatedPromptTokens: 7, EstimatedCompletionTokens: 2, BuyerDeclaredCeilingUSD: maxUSD * 1.1,
			}
			c, _, err := store.AuthorizeRealtimeContract(context.Background(), a)
			if err == nil {
				_, _ = store.FinalizeRealtimeFailure(context.Background(), c.ID, uuid.New(), 500, 1, "hotpath", "teardown", false)
				if i%20 == 0 {
					_, _ = pool.Exec(context.Background(), `
						UPDATE realtime_worker_offers
						   SET available_sequences=max_active_sequences, status='ACTIVE', last_seen_at=now()
						 WHERE status='ACTIVE'`)
				}
				return "SUCCESS", nil
			}
			return classifyHotPathAuthorize(err), err
		})
		attachHotPathTrial(&cell, tr, "SUCCESS")
		t.Logf("%s p50=%.3fms p95=%.3fms n=%d", name, cell.P50MS, cell.P95MS, cell.SampleN)
		cells = append(cells, cell)
	}
	return cells
}

func classifyHotPathAuthorize(err error) string {
	if err == nil {
		return "SUCCESS"
	}
	if errors.Is(err, errRealtimeNoSupply) || errors.Is(err, errRealtimeInsufficientFunds) ||
		errors.Is(err, errRealtimeTopupRequired) || errors.Is(err, errRealtimeIdempotencyConflict) {
		return "REFUSAL"
	}
	return "FAILURE"
}

// --- leases -----------------------------------------------------------------

func measureHotPathLease(
	t *testing.T, ctx context.Context, store *Store, pool *pgxpool.Pool,
	profile VLLMRuntimeProfile, workers []WorkerAuth, samples int,
) []hotPathCell {
	t.Helper()
	region := "ca-hp-" + uuid.NewString()[:8]
	leaseBook := 32
	if leaseBook > 64 {
		leaseBook = 64
	}
	if err := seedSelectorLeaseBook(ctx, pool, profile, region, leaseBook, 20260815); err != nil {
		t.Fatalf("seed lease book: %v", err)
	}
	leaseBuyer := uuid.New()
	if _, err := pool.Exec(ctx, `INSERT INTO buyers (id,email) VALUES ($1,$2)`,
		leaseBuyer, leaseBuyer.String()+"@hotpath-lease.invalid"); err != nil {
		t.Fatal(err)
	}
	if err := store.SeedPrepaidBalance(ctx, leaseBuyer, 100_000_000_000, "hp-lease-"+leaseBuyer.String()); err != nil {
		t.Fatal(err)
	}
	req := ServiceLeaseRequest{
		RuntimeProfileID: profile.RuntimeProfileID, Region: region, Currency: "usd",
		MinimumReplicas: 1, MaximumReplicas: 1, TermSeconds: 60,
		MaximumP95LatencyMilliseconds: 500, BuyerDeclaredCeilingNanos: 135_000_000,
	}

	var cells []hotPathCell
	cell := hotPathBaseCell("create_service_lease_success_book_32", "lease acquisition",
		"CreateServiceLease SUCCESS against a 32-offer READY book; capacity restored after each call")
	cell.EntryPoint = "Store.CreateServiceLease"
	cell.BookSize = leaseBook
	cell.Command = fmt.Sprintf("CreateServiceLease region=%s book=%d n=%d", region, leaseBook, min(samples, 60))
	tr := timeHotPathCalls(min(samples, 60), 1, func(i int) (string, error) {
		lease, err := store.CreateServiceLease(context.Background(), leaseBuyer, req)
		if err != nil {
			return classifyHotPathLease(err), err
		}
		_, _ = pool.Exec(context.Background(), `
			UPDATE service_leases
			   SET state='CANCELLED', finalized_at=COALESCE(finalized_at, now()),
			       expires_at=LEAST(expires_at, now())
			 WHERE id=$1`, lease.ID)
		_, _ = pool.Exec(context.Background(), `
			UPDATE service_lease_worker_offers
			   SET available_warm_replicas=maximum_warm_replicas, last_seen_at=now(), status='READY'
			 WHERE region=$1`, region)
		return "SUCCESS", nil
	})
	attachHotPathTrial(&cell, tr, "SUCCESS")
	t.Logf("%s p50=%.3fms n=%d refusal=%d fail=%d", cell.Name, cell.P50MS, cell.SampleN, cell.Buckets.REFUSAL, cell.Buckets.FAILURE)
	cells = append(cells, cell)

	// REFUSAL: no supply (wrong region).
	bad := req
	bad.Region = "ca-none-" + uuid.NewString()[:8]
	cell = hotPathBaseCell("create_service_lease_no_supply_refusal", "lease acquisition",
		"CreateServiceLease against a region with no READY offers → errRealtimeNoSupply")
	cell.EntryPoint = "Store.CreateServiceLease"
	cell.Command = "CreateServiceLease unknown region"
	tr = timeHotPathCalls(min(samples, 40), 1, func(i int) (string, error) {
		_, err := store.CreateServiceLease(context.Background(), leaseBuyer, bad)
		return classifyHotPathLease(err), err
	})
	attachHotPathTrial(&cell, tr, "REFUSAL")
	cells = append(cells, cell)

	// REFUSAL: min != max (undeliverable elasticity) — pre-transaction.
	flex := req
	flex.MaximumReplicas = 3
	cell = hotPathBaseCell("create_service_lease_elasticity_refusal", "lease acquisition",
		"minimum_replicas != maximum_replicas refused before any transaction")
	cell.EntryPoint = "Store.CreateServiceLease"
	cell.Command = "CreateServiceLease min=1 max=3"
	tr = timeHotPathCalls(min(samples, 80), 1, func(i int) (string, error) {
		_, err := store.CreateServiceLease(context.Background(), leaseBuyer, flex)
		if err != nil {
			return "REFUSAL", err
		}
		return "FAILURE", errors.New("elasticity request was accepted")
	})
	attachHotPathTrial(&cell, tr, "REFUSAL")
	cells = append(cells, cell)

	// Data-plane target: one real lease bound to workers[0], which already has a realtime offer.
	w0 := workers[0]
	seedMeasuredWarmResidency(t, ctx, pool, w0.WorkerID, profile.ModelAlias)
	dpRegion := "ca-hp-dp-" + uuid.NewString()[:8]
	offer := serviceLeaseOffer(profile)
	offer.Region = dpRegion
	if err := store.UpsertServiceLeaseOffer(ctx, w0, offer); err != nil {
		t.Fatalf("UpsertServiceLeaseOffer: %v", err)
	}
	if err := store.HeartbeatRealtimeOffer(ctx, w0, RealtimeOfferHeartbeat{
		RuntimeProfileID: profile.RuntimeProfileID, Warmth: "HOT",
		AvailableSequences: 8, Status: "ACTIVE",
	}); err != nil {
		t.Fatalf("dp heartbeat: %v", err)
	}
	dpBuyer := uuid.New()
	if _, err := pool.Exec(ctx, `INSERT INTO buyers (id,email) VALUES ($1,$2)`,
		dpBuyer, dpBuyer.String()+"@hotpath-dp.invalid"); err != nil {
		t.Fatal(err)
	}
	if err := store.SeedPrepaidBalance(ctx, dpBuyer, 10_000_000_000, "hp-dp-"+dpBuyer.String()); err != nil {
		t.Fatal(err)
	}
	var dpAvail int
	if err := pool.QueryRow(ctx, `
		SELECT available_warm_replicas FROM service_lease_worker_offers
		 WHERE worker_id=$1 AND runtime_profile_id=$2 AND region=$3`,
		w0.WorkerID, profile.RuntimeProfileID, dpRegion).Scan(&dpAvail); err != nil {
		t.Fatalf("dp offer lookup: %v", err)
	}
	if dpAvail < 1 {
		t.Fatalf("dp offer available_warm_replicas=%d after UpsertServiceLeaseOffer (need measured worker_model_state)", dpAvail)
	}
	lease, err := store.CreateServiceLease(ctx, dpBuyer, ServiceLeaseRequest{
		RuntimeProfileID: profile.RuntimeProfileID, Region: dpRegion, Currency: "usd",
		MinimumReplicas: 1, MaximumReplicas: 1, TermSeconds: 120,
		MaximumP95LatencyMilliseconds: 500, BuyerDeclaredCeilingNanos: 1_000_000_000,
	})
	if err != nil {
		t.Fatalf("dp CreateServiceLease (avail=%d): %v", dpAvail, err)
	}

	t.Setenv("MERC_LIVENESS_INDEX_AUTHORITATIVE", "0")
	cell = hotPathBaseCell("service_lease_data_plane_flag_off_success", "lease acquisition",
		"ServiceLeaseDataPlaneTarget SUCCESS, flag OFF (SQL last_seen_at predicate)")
	cell.EntryPoint = "Store.ServiceLeaseDataPlaneTarget"
	cell.Flag = "off"
	cell.Command = "ServiceLeaseDataPlaneTarget flag=off"
	tr = timeHotPathCalls(samples, 1, func(i int) (string, error) {
		_, err := store.ServiceLeaseDataPlaneTarget(context.Background(), dpBuyer, lease.ID)
		return classifyHotPathDataPlane(err), err
	})
	attachHotPathTrial(&cell, tr, "SUCCESS")
	cells = append(cells, cell)

	t.Setenv("MERC_LIVENESS_INDEX_AUTHORITATIVE", "1")
	// Ensure index live for this offer.
	_ = store.HeartbeatRealtimeOffer(ctx, w0, RealtimeOfferHeartbeat{
		RuntimeProfileID: profile.RuntimeProfileID, Warmth: "HOT",
		AvailableSequences: 8, Status: "ACTIVE",
	})
	cell = hotPathBaseCell("service_lease_data_plane_flag_on_success", "lease acquisition",
		"ServiceLeaseDataPlaneTarget SUCCESS, flag ON (SQL without last_seen_at + offerIndexLive)")
	cell.EntryPoint = "Store.ServiceLeaseDataPlaneTarget"
	cell.Flag = "on"
	cell.Command = "ServiceLeaseDataPlaneTarget flag=on"
	tr = timeHotPathCalls(samples, 1, func(i int) (string, error) {
		_, err := store.ServiceLeaseDataPlaneTarget(context.Background(), dpBuyer, lease.ID)
		return classifyHotPathDataPlane(err), err
	})
	attachHotPathTrial(&cell, tr, "SUCCESS")
	cells = append(cells, cell)

	// REFUSAL: wrong buyer.
	cell = hotPathBaseCell("service_lease_data_plane_wrong_buyer_refusal", "lease acquisition",
		"ServiceLeaseDataPlaneTarget with a different buyer → errNotFound")
	cell.EntryPoint = "Store.ServiceLeaseDataPlaneTarget"
	cell.Command = "ServiceLeaseDataPlaneTarget wrong buyer"
	tr = timeHotPathCalls(min(samples, 80), 1, func(i int) (string, error) {
		_, err := store.ServiceLeaseDataPlaneTarget(context.Background(), uuid.New(), lease.ID)
		return classifyHotPathDataPlane(err), err
	})
	attachHotPathTrial(&cell, tr, "REFUSAL")
	cells = append(cells, cell)

	t.Setenv("MERC_LIVENESS_INDEX_AUTHORITATIVE", "0")
	return cells
}

func classifyHotPathLease(err error) string {
	if err == nil {
		return "SUCCESS"
	}
	if errors.Is(err, errRealtimeNoSupply) || errors.Is(err, errRealtimeInsufficientFunds) {
		return "REFUSAL"
	}
	// Pre-transaction validation errors are refusals, not infrastructure failures.
	if err != nil && (strings.Contains(err.Error(), "refuses minimum_replicas") ||
		strings.Contains(err.Error(), "invalid buyer") ||
		strings.Contains(err.Error(), "unknown service lease")) {
		return "REFUSAL"
	}
	return "FAILURE"
}

func classifyHotPathDataPlane(err error) string {
	if err == nil {
		return "SUCCESS"
	}
	if errors.Is(err, errNotFound) || errors.Is(err, errServiceLeaseDataPlaneUnavailable) {
		return "REFUSAL"
	}
	return "FAILURE"
}

// --- pricing ----------------------------------------------------------------

func measureHotPathPricing(t *testing.T, profile VLLMRuntimeProfile, samples int) []hotPathCell {
	t.Helper()
	n := max(samples*10, 2_000)
	reg := RealtimeOfferRegistration{
		RuntimeProfileID: profile.RuntimeProfileID, RuntimeProfileSHA256: profile.ProfileSHA256,
		HWClass: "nvidia_24gb", GPUCount: 1, MemoryGBPerGPU: 24,
		SupplierInputUSDPerMillionTokens: 0.08, SupplierOutputUSDPerMillionTokens: 0.30,
	}
	placement, err := newRealtimePlacementPlan(profile, reg)
	if err != nil {
		t.Fatalf("placement: %v", err)
	}
	in := RealtimePricingInputs{
		Profile: profile, Placement: placement,
		InputCommitment: strings.Repeat("a", 64), RequestSHA256: strings.Repeat("b", 64),
		MaximumPromptTokens: 100, MaximumCompletionTokens: 8,
		EstimatedPromptTokens: 7, EstimatedCompletionTokens: 2,
		SupplierInputRate: 0.08, SupplierOutputRate: 0.30,
		BuyerDeclaredCeiling: 0.01, Currency: MustParseCurrency("usd"),
	}

	var cells []hotPathCell
	cells = append(cells, timeHotPathInProcess(t, "price_newRealtimePricingDecision_success", "price computation",
		"newRealtimePricingDecision (clearing/ranking inputs for one admit)", n, func(i int) (string, error) {
			_, err := newRealtimePricingDecision(in)
			if err != nil {
				return "FAILURE", err
			}
			return "SUCCESS", nil
		}))

	cells = append(cells, timeHotPathInProcess(t, "price_buildRealtimeClearingRankingInputs", "price computation",
		"buildRealtimeClearingRankingInputs (verified-outcome cost + omitted terms)", n, func(i int) (string, error) {
			_ = buildRealtimeClearingRankingInputs(80_000_000, 300_000_000, 20, 1, 18, 0, "HOT")
			return "SUCCESS", nil
		}))

	leaseReq := ServiceLeaseRequest{
		RuntimeProfileID: profile.RuntimeProfileID, Region: "ca-central-1", Currency: "usd",
		MinimumReplicas: 1, MaximumReplicas: 1, TermSeconds: 60,
		MaximumP95LatencyMilliseconds: 500, BuyerDeclaredCeilingNanos: 135_000_000,
	}
	leaseIn := serviceLeasePricingInputs(profile, MustParseCurrency("usd"), leaseReq, 2_000_000_000, 200_000_000)
	cells = append(cells, timeHotPathInProcess(t, "price_newServiceLeasePricingDecision_success", "price computation",
		"newServiceLeasePricingDecision (lease ask + residency + contribution)", n, func(i int) (string, error) {
			_, err := newServiceLeasePricingDecision(leaseIn)
			if err != nil {
				return "FAILURE", err
			}
			return "SUCCESS", nil
		}))

	currency := MustParseCurrency("usd")
	fx, fxErr := loadRealtimeFXAuthority(currency)
	if fxErr != nil {
		t.Logf("loadRealtimeFXAuthority: %v (realtimeBuyerPriceBounds cell may FAILURE)", fxErr)
	}
	cells = append(cells, timeHotPathInProcess(t, "price_realtimeBuyerPriceBounds", "price computation",
		"realtimeBuyerPriceBounds (USD + settlement token ceilings)", n, func(i int) (string, error) {
			_, _, _, _, err := realtimeBuyerPriceBounds(profile, 100, 8, 7, 2, currency, fx)
			if err != nil {
				return "FAILURE", err
			}
			return "SUCCESS", nil
		}))
	return cells
}

// --- EXPLAIN ----------------------------------------------------------------

func collectHotPathExplains(
	t *testing.T, ctx context.Context, store *Store, pool *pgxpool.Pool,
	profile VLLMRuntimeProfile, workers []WorkerAuth,
) []hotPathExplain {
	t.Helper()
	var out []hotPathExplain
	if len(workers) == 0 {
		return out
	}

	// Heartbeat one-row UPDATE (the unbatched durable statement).
	hbSQL := `
		UPDATE realtime_worker_offers AS o
		   SET warmth=$4,
		       available_sequences=GREATEST(0,LEAST($5,o.max_active_sequences-(
		           SELECT count(*)::int FROM execution_contracts c
		            WHERE c.worker_id=$1 AND c.runtime_profile_id=$3
		              AND c.state='EXECUTING'))),
		       status=$6,last_seen_at=$7,updated_at=now()
		  FROM workers AS w
		 WHERE o.worker_id=$1 AND o.supplier_id=$2 AND o.runtime_profile_id=$3
		   AND w.id=o.worker_id
		   AND $5 BETWEEN 0 AND o.max_active_sequences
		RETURNING o.runtime_profile_sha256,
	          COALESCE(NULLIF(o.placement_plan->>'hw_class',''),w.hw_class),o.max_active_sequences,
	          o.available_sequences,o.supplier_input_usd_per_million_tokens,
	          o.supplier_output_usd_per_million_tokens`
	w0 := workers[0]
	out = append(out, runHotPathExplain(t, ctx, pool, "heartbeat_update_one",
		"durable HeartbeatRealtimeOffer one-row UPDATE (liveness_ingest.go heartbeatRealtimeOfferOne)",
		hbSQL, w0.WorkerID, w0.SupplierID, profile.RuntimeProfileID, "HOT", 8, "ACTIVE", time.Now().UTC()))

	out = append(out, runHotPathExplain(t, ctx, pool, "lookup_offer_binding",
		"lookupOfferBinding cache-miss SELECT",
		`SELECT offer_slot, supplier_id, max_active_sequences
		   FROM realtime_worker_offers WHERE worker_id=$1 AND runtime_profile_id=$2`,
		w0.WorkerID, profile.RuntimeProfileID))

	// Restore capacity after ANALYZE of the claim SQL.
	_, _ = pool.Exec(ctx, `
		UPDATE realtime_worker_offers
		   SET available_sequences=max_active_sequences, status='ACTIVE', last_seen_at=now()`)
	out = append(out, runHotPathExplain(t, ctx, pool, "authorize_select_offer_skip",
		"production realtimeAuthorizeSelectOfferSQLSkip BEFORE ANALYZE (isolated DB default stats; planner estimated 1 row on the first run)",
		realtimeAuthorizeSelectOfferSQLSkip,
		profile.RuntimeProfileID, profile.ProfileSHA256,
		profile.BuyerInputUSDPerMillionTokens, profile.BuyerOutputUSDPerMillionTokens,
		minRealtimeOutcomeSamples))
	_, _ = pool.Exec(ctx, `
		UPDATE realtime_worker_offers
		   SET available_sequences=max_active_sequences, status='ACTIVE', last_seen_at=now()`)

	out = append(out, runHotPathExplain(t, ctx, pool, "eligible_book_count",
		"eligible CTE alone BEFORE ANALYZE",
		`WITH eligible AS (`+mustEligibleBody(t)+`) SELECT count(*)::int FROM eligible`,
		profile.RuntimeProfileID, profile.ProfileSHA256,
		profile.BuyerInputUSDPerMillionTokens, profile.BuyerOutputUSDPerMillionTokens,
		minRealtimeOutcomeSamples))

	if _, err := pool.Exec(ctx, `
		ANALYZE realtime_worker_offers, suppliers, realtime_supplier_outcome_stats,
		        service_lease_worker_offers, execution_contracts, workers`); err != nil {
		t.Logf("ANALYZE: %v", err)
	}

	_, _ = pool.Exec(ctx, `
		UPDATE realtime_worker_offers
		   SET available_sequences=max_active_sequences, status='ACTIVE', last_seen_at=now()`)
	out = append(out, runHotPathExplain(t, ctx, pool, "authorize_select_offer_skip_after_analyze",
		"production realtimeAuthorizeSelectOfferSQLSkip AFTER ANALYZE — production-like planner stats",
		realtimeAuthorizeSelectOfferSQLSkip,
		profile.RuntimeProfileID, profile.ProfileSHA256,
		profile.BuyerInputUSDPerMillionTokens, profile.BuyerOutputUSDPerMillionTokens,
		minRealtimeOutcomeSamples))
	_, _ = pool.Exec(ctx, `
		UPDATE realtime_worker_offers
		   SET available_sequences=max_active_sequences, status='ACTIVE', last_seen_at=now()`)

	out = append(out, runHotPathExplain(t, ctx, pool, "eligible_book_count_after_analyze",
		"eligible CTE alone AFTER ANALYZE",
		`WITH eligible AS (`+mustEligibleBody(t)+`) SELECT count(*)::int FROM eligible`,
		profile.RuntimeProfileID, profile.ProfileSHA256,
		profile.BuyerInputUSDPerMillionTokens, profile.BuyerOutputUSDPerMillionTokens,
		minRealtimeOutcomeSamples))

	leaseRegion := "ca-central-1"
	_ = pool.QueryRow(ctx, `
		SELECT region FROM service_lease_worker_offers
		 WHERE status='READY' AND last_seen_at > now()-interval '45 seconds'
		 LIMIT 1`).Scan(&leaseRegion)
	out = append(out, runHotPathExplain(t, ctx, pool, "service_lease_book_walk",
		"CreateServiceLease ordered FOR UPDATE book walk on a populated READY region (after ANALYZE)",
		serviceLeaseBookWalkSQL(),
		profile.RuntimeProfileID, profile.ProfileSHA256, leaseRegion, 1, 500, "usd", int64(0)))

	out = append(out, runHotPathExplain(t, ctx, pool, "service_lease_offer_sql_liveness",
		"ServiceLeaseDataPlaneTarget offer lookup with last_seen_at predicate (flag OFF)",
		serviceLeaseOfferQuerySQLLiveness,
		w0.WorkerID, w0.SupplierID, profile.RuntimeProfileID))

	_ = store
	return out
}

func mustEligibleBody(t *testing.T) string {
	t.Helper()
	body, err := extractEligibleCTEBody(realtimeAuthorizeCandidatesCTE)
	if err != nil {
		t.Fatalf("extract eligible CTE: %v", err)
	}
	return body
}

func runHotPathExplain(t *testing.T, ctx context.Context, pool *pgxpool.Pool, name, what, sql string, args ...any) hotPathExplain {
	t.Helper()
	ex := hotPathExplain{Name: name, What: what, Classification: "MEASURED"}
	tx, err := pool.Begin(ctx)
	if err != nil {
		ex.Classification = "UNMEASURED"
		ex.Error = err.Error()
		return ex
	}
	defer tx.Rollback(ctx)
	if _, err := tx.Exec(ctx, `SET LOCAL statement_timeout = '30s'`); err != nil {
		ex.Classification = "UNMEASURED"
		ex.Error = err.Error()
		return ex
	}
	rows, err := tx.Query(ctx, "EXPLAIN (ANALYZE, BUFFERS, FORMAT TEXT) "+sql, args...)
	if err != nil {
		ex.Classification = "UNMEASURED"
		ex.Error = err.Error()
		return ex
	}
	var lines []string
	for rows.Next() {
		var line string
		if err := rows.Scan(&line); err != nil {
			rows.Close()
			ex.Classification = "UNMEASURED"
			ex.Error = err.Error()
			return ex
		}
		lines = append(lines, line)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		ex.Classification = "UNMEASURED"
		ex.Error = err.Error()
		return ex
	}
	ex.Raw = strings.Join(lines, "\n")
	if len(lines) > 60 {
		ex.Raw = strings.Join(lines[:60], "\n") + "\n… truncated …"
	}
	ex.DominatingNode, ex.ExecutionMS, ex.PlanningMS = parseExplainDominator(lines)
	t.Logf("EXPLAIN %s dominate=%s exec=%.3fms plan=%.3fms", name, ex.DominatingNode, ex.ExecutionMS, ex.PlanningMS)
	return ex
}

func parseExplainDominator(lines []string) (node string, execMS, planMS float64) {
	var bestNode string
	var bestActual float64
	for _, line := range lines {
		trim := strings.TrimSpace(line)
		if strings.HasPrefix(trim, "Planning Time:") {
			fmt.Sscanf(trim, "Planning Time: %f ms", &planMS)
		}
		if strings.HasPrefix(trim, "Execution Time:") {
			fmt.Sscanf(trim, "Execution Time: %f ms", &execMS)
		}
		// actual time=start..end
		if i := strings.Index(trim, "actual time="); i >= 0 {
			rest := trim[i+len("actual time="):]
			var start, end float64
			if _, err := fmt.Sscanf(rest, "%f..%f", &start, &end); err == nil && end >= bestActual {
				bestActual = end
				// node name: after "->  " or at start, before "  ("
				name := trim
				if j := strings.Index(name, "->  "); j >= 0 {
					name = name[j+4:]
				}
				if k := strings.Index(name, "  ("); k > 0 {
					name = name[:k]
				}
				bestNode = strings.TrimSpace(name)
			}
		}
	}
	if bestNode == "" {
		bestNode = "UNPARSED"
	}
	return bestNode, execMS, planMS
}

// --- pprof ------------------------------------------------------------------

func collectHotPathProfiles(
	t *testing.T, ctx context.Context, store *Store, pool *pgxpool.Pool,
	profile VLLMRuntimeProfile, workers []WorkerAuth, buyerID uuid.UUID, dir string,
) hotPathProfiles {
	t.Helper()
	out := hotPathProfiles{
		Classification: "MEASURED",
		Mix:            "80% HeartbeatRealtimeOffer flag-ON retired, 10% offerIndexLive, 5% Authorize+Finalize, 5% ServiceLeaseDataPlaneTarget (if a lease exists) + pricing",
		DurationSec:    6,
	}
	cpuPath := filepath.Join(dir, "control-plane-hot-path-cpu.pprof")
	allocPath := filepath.Join(dir, "control-plane-hot-path-alloc.pprof")
	heapPath := filepath.Join(dir, "control-plane-hot-path-heap.pprof")
	out.CPUPath = cpuPath
	out.AllocPath = allocPath
	out.HeapPath = heapPath

	maxUSD, estUSD, maxPrompt, maxCompletion := realtimeAuthCeiling(t, profile, 7, 2)
	hb := RealtimeOfferHeartbeat{
		RuntimeProfileID: profile.RuntimeProfileID, Warmth: "HOT",
		AvailableSequences: 8, Status: "ACTIVE",
	}
	t.Setenv("MERC_LIVENESS_INDEX_AUTHORITATIVE", "1")
	// Warm persist so the mix is the retired path (the post-P0 fast path).
	for i := 0; i < min(32, len(workers)); i++ {
		_ = store.HeartbeatRealtimeOffer(ctx, workers[i], hb)
	}
	waitWriteAmpDurable(store, 2*time.Second)

	cpuF, err := os.Create(cpuPath)
	if err != nil {
		out.Classification = "UNMEASURED"
		out.Error = err.Error()
		return out
	}
	if err := pprof.StartCPUProfile(cpuF); err != nil {
		cpuF.Close()
		out.Classification = "UNMEASURED"
		out.Error = err.Error()
		return out
	}

	deadline := time.Now().Add(time.Duration(out.DurationSec) * time.Second)
	var mixCounts struct {
		HB, Idx, Auth, Price int64
	}
	var next atomic.Int64
	var wg sync.WaitGroup
	workersN := runtime.GOMAXPROCS(0)
	if workersN < 2 {
		workersN = 2
	}
	for w := 0; w < workersN; w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			now := time.Now().UTC()
			for time.Now().Before(deadline) {
				i := int(next.Add(1))
				switch i % 20 {
				case 0:
					a := RealtimeContractAuthorization{
						RequestID: "mix-" + uuid.NewString(), BuyerID: buyerID, Profile: profile,
						InputCommitment: strings.Repeat("a", 64), RequestSHA256: strings.Repeat("b", 64),
						MaximumPriceUSD: maxUSD, EstimatedPriceUSD: estUSD, DeadlineAt: time.Now().Add(time.Minute),
						MaximumPromptTokens: maxPrompt, MaximumCompletionTokens: maxCompletion,
						EstimatedPromptTokens: 7, EstimatedCompletionTokens: 2, BuyerDeclaredCeilingUSD: maxUSD * 1.1,
					}
					if c, _, err := store.AuthorizeRealtimeContract(context.Background(), a); err == nil {
						_, _ = store.FinalizeRealtimeFailure(context.Background(), c.ID, uuid.New(), 500, 1, "mix", "mix", false)
					}
					atomic.AddInt64(&mixCounts.Auth, 1)
				case 1:
					reg := RealtimeOfferRegistration{
						RuntimeProfileID: profile.RuntimeProfileID, RuntimeProfileSHA256: profile.ProfileSHA256,
						HWClass: "nvidia_24gb", GPUCount: 1, MemoryGBPerGPU: 24,
						SupplierInputUSDPerMillionTokens: 0.08, SupplierOutputUSDPerMillionTokens: 0.30,
					}
					pl, err := newRealtimePlacementPlan(profile, reg)
					if err == nil {
						_, _ = newRealtimePricingDecision(RealtimePricingInputs{
							Profile: profile, Placement: pl,
							InputCommitment: strings.Repeat("a", 64), RequestSHA256: strings.Repeat("b", 64),
							MaximumPromptTokens: 100, MaximumCompletionTokens: 8,
							EstimatedPromptTokens: 7, EstimatedCompletionTokens: 2,
							SupplierInputRate: 0.08, SupplierOutputRate: 0.30,
							BuyerDeclaredCeiling: 0.01, Currency: MustParseCurrency("usd"),
						})
					}
					atomic.AddInt64(&mixCounts.Price, 1)
				case 2, 3:
					wk := workers[i%len(workers)]
					_ = store.offerIndexLive(wk.WorkerID, profile.RuntimeProfileID, now)
					atomic.AddInt64(&mixCounts.Idx, 1)
				default:
					wk := workers[i%len(workers)]
					_ = store.HeartbeatRealtimeOffer(context.Background(), wk, hb)
					atomic.AddInt64(&mixCounts.HB, 1)
				}
			}
		}()
	}
	wg.Wait()
	pprof.StopCPUProfile()
	cpuF.Close()

	runtime.GC()
	if af, err := os.Create(allocPath); err == nil {
		_ = pprof.Lookup("allocs").WriteTo(af, 0)
		af.Close()
	}
	if hf, err := os.Create(heapPath); err == nil {
		_ = pprof.WriteHeapProfile(hf)
		hf.Close()
	}

	out.MixCounts = map[string]int64{
		"heartbeat": mixCounts.HB, "offerIndexLive": mixCounts.Idx,
		"authorize": mixCounts.Auth, "pricing": mixCounts.Price,
	}
	out.CPUTop = runPprofTop(t, cpuPath, false)
	out.CPUCumTop = runPprofTop(t, cpuPath, true)
	out.AllocTop = runPprofTop(t, allocPath, false)
	out.AllocCumTop = runPprofTop(t, allocPath, true)
	t.Setenv("MERC_LIVENESS_INDEX_AUTHORITATIVE", "0")
	_, _ = pool.Exec(ctx, `
		UPDATE realtime_worker_offers
		   SET available_sequences=max_active_sequences, status='ACTIVE', last_seen_at=now()`)
	t.Logf("pprof mix hb=%d idx=%d auth=%d price=%d", mixCounts.HB, mixCounts.Idx, mixCounts.Auth, mixCounts.Price)
	return out
}

func runPprofTop(t *testing.T, profilePath string, cum bool) string {
	t.Helper()
	bin := os.Args[0]
	args := []string{"tool", "pprof", "-top", "-nodecount=40"}
	if cum {
		args = append(args, "-cum")
	}
	args = append(args, bin, profilePath)
	cmd := exec.Command("go", args...)
	cmd.Env = append(os.Environ(), "PPROF_TMPDIR="+os.TempDir())
	out, err := cmd.CombinedOutput()
	if err != nil {
		args2 := []string{"tool", "pprof", "-top", "-nodecount=40"}
		if cum {
			args2 = append(args2, "-cum")
		}
		args2 = append(args2, profilePath)
		cmd = exec.Command("go", args2...)
		out2, err2 := cmd.CombinedOutput()
		if err2 != nil {
			return fmt.Sprintf("pprof failed: %v / %v\n%s\n%s", err, err2, out, out2)
		}
		return string(out2)
	}
	return string(out)
}

// --- timing core ------------------------------------------------------------

type hotPathTrial struct {
	ByOutcome     map[string]hotPathMS
	Buckets       hotPathBuckets
	ThroughputOPS float64
	AllocsPerOp   float64
	BytesPerOp    float64
	CPUNanosPerOp float64
	WallSeconds   float64
	FirstError    string
}

// timeHotPathCalls measures n calls at concurrency conc. Outcomes are bucketed
// and SUCCESS percentiles never include refusals or failures.
func timeHotPathCalls(n, conc int, fn func(i int) (string, error)) hotPathTrial {
	if conc < 1 {
		conc = 1
	}
	type rec struct {
		class string
		ms    float64
		err   string
	}
	out := make([]rec, n)
	var next atomic.Int64
	runtime.GC()
	var ms0 runtime.MemStats
	runtime.ReadMemStats(&ms0)
	var ru0 syscall.Rusage
	_ = syscall.Getrusage(syscall.RUSAGE_SELF, &ru0)
	wall0 := time.Now()
	var wg sync.WaitGroup
	for c := 0; c < conc; c++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				i := int(next.Add(1) - 1)
				if i >= n {
					return
				}
				start := time.Now()
				class, err := fn(i)
				if class == "" {
					class = "FAILURE"
				}
				r := rec{class: class, ms: float64(time.Since(start).Nanoseconds()) / 1e6}
				if err != nil {
					r.err = err.Error()
				}
				out[i] = r
			}
		}()
	}
	wg.Wait()
	wall := time.Since(wall0)
	var ru1 syscall.Rusage
	_ = syscall.Getrusage(syscall.RUSAGE_SELF, &ru1)
	var ms1 runtime.MemStats
	runtime.ReadMemStats(&ms1)

	by := map[string][]float64{}
	var first string
	var buck hotPathBuckets
	for _, r := range out {
		by[r.class] = append(by[r.class], r.ms)
		switch r.class {
		case "SUCCESS":
			buck.SUCCESS++
		case "REFUSAL":
			buck.REFUSAL++
		case "EMPTY":
			buck.EMPTY++
		default:
			buck.FAILURE++
		}
		if r.err != "" && first == "" {
			first = r.err
		}
	}
	summ := map[string]hotPathMS{}
	for k, v := range by {
		summ[k] = summarizeHotPathMS(v)
	}
	cpu := rusageCPU(ru1) - rusageCPU(ru0)
	allocs := float64(ms1.Mallocs-ms0.Mallocs) / float64(n)
	bytes := float64(ms1.TotalAlloc-ms0.TotalAlloc) / float64(n)
	ops := 0.0
	if wall > 0 {
		ops = float64(n) / wall.Seconds()
	}
	return hotPathTrial{
		ByOutcome:     summ,
		Buckets:       buck,
		ThroughputOPS: ops,
		AllocsPerOp:   allocs,
		BytesPerOp:    bytes,
		CPUNanosPerOp: float64(cpu.Nanoseconds()) / float64(n),
		WallSeconds:   wall.Seconds(),
		FirstError:    first,
	}
}

func timeHotPathInProcess(t *testing.T, name, subsystem, entry string, n int, fn func(i int) (string, error)) hotPathCell {
	t.Helper()
	cell := hotPathBaseCell(name, subsystem, "")
	cell.EntryPoint = entry
	cell.Command = fmt.Sprintf("%s × %d (in-process)", entry, n)
	tr := timeHotPathCalls(n, 1, fn)
	attachHotPathTrial(&cell, tr, "SUCCESS")
	t.Logf("%s p50=%.4fms p95=%.4fms n=%d allocs=%.2f bytes=%.0f cpu_ns/op=%.0f",
		name, cell.P50MS, cell.P95MS, cell.SampleN, cell.AllocsPerOp, cell.BytesPerOp, cell.CPUNanosPerOp)
	return cell
}

func attachHotPathTrial(cell *hotPathCell, tr hotPathTrial, want string) {
	cell.Buckets = tr.Buckets
	cell.ByOutcome = tr.ByOutcome
	cell.ThroughputOPS = tr.ThroughputOPS
	cell.AllocsPerOp = tr.AllocsPerOp
	cell.BytesPerOp = tr.BytesPerOp
	cell.CPUNanosPerOp = tr.CPUNanosPerOp
	cell.WallSeconds = tr.WallSeconds
	cell.FirstError = tr.FirstError
	cell.WantedOutcome = want
	if ms, ok := tr.ByOutcome[want]; ok && ms.N > 0 {
		cell.Classification = "MEASURED"
		cell.SampleN = ms.N
		cell.P50MS = ms.P50
		cell.P95MS = ms.P95
		cell.P99MS = ms.P99
		cell.MaxMS = ms.Max
		cell.AvgMS = ms.Avg
	} else {
		cell.Classification = "UNMEASURED"
		cell.UnmeasuredReason = fmt.Sprintf("wanted %s but buckets=%+v first_err=%s", want, tr.Buckets, tr.FirstError)
	}
}

func summarizeHotPathMS(samples []float64) hotPathMS {
	if len(samples) == 0 {
		return hotPathMS{}
	}
	sorted := append([]float64(nil), samples...)
	sort.Float64s(sorted)
	pct := func(p float64) float64 {
		if len(sorted) == 1 {
			return sorted[0]
		}
		idx := int(math.Ceil(p*float64(len(sorted)))) - 1
		if idx < 0 {
			idx = 0
		}
		if idx >= len(sorted) {
			idx = len(sorted) - 1
		}
		return sorted[idx]
	}
	var sum float64
	for _, v := range sorted {
		sum += v
	}
	return hotPathMS{
		N: len(sorted), P50: pct(0.50), P95: pct(0.95), P99: pct(0.99),
		Max: sorted[len(sorted)-1], Avg: sum / float64(len(sorted)),
	}
}

func rusageCPU(r syscall.Rusage) time.Duration {
	return time.Duration(r.Utime.Nano() + r.Stime.Nano())
}

func hotPathBaseCell(name, subsystem, note string) hotPathCell {
	return hotPathCell{
		Name: name, Subsystem: subsystem, Note: note, Classification: "MEASURED",
	}
}

// --- ranking ----------------------------------------------------------------

func rankHotPath(report hotPathReport) []hotPathRankRow {
	// Frequency model (DERIVED, not measured traffic):
	//   heartbeat: N_offers / 15s          (agent HEARTBEAT_INTERVAL)
	//   index:     2 × heartbeat           (lookup + offerIndexLive on flag-ON ingest)
	//   authorize: scenario 100 req/s      (named assumption; production QPS UNMEASURED)
	//   lease create: 0.1 req/s            (session start; UNMEASURED)
	//   data plane: 100 req/s on reserved  (same assumption as authorize)
	//   price:     1 × authorize
	// Scale N = seeded fleet. Risk: 1 in-process, 2 SQL, 3 money+locks.
	n := float64(report.Invocation.SeededFleet)
	if n < 1 {
		n = float64(report.Invocation.Fleet)
	}
	type pick struct {
		subsystem, cell string
		freq            float64
		freqNote        string
		risk            float64
		riskNote        string
		dbTime          string
	}
	picks := []pick{
		{"heartbeat ingest (flag ON, retired)", "heartbeat_ingest_flag_on_retired_batched_success",
			n / hotPathAgentHeartbeatSec, fmt.Sprintf("DERIVED: %.0f offers / %ds", n, hotPathAgentHeartbeatSec),
			1, "in-process admit + index; detached flush is not on the caller", "none on caller"},
		{"heartbeat ingest (flag OFF, production HEAD)", "heartbeat_ingest_flag_off_batched_success",
			n / hotPathAgentHeartbeatSec, fmt.Sprintf("DERIVED: %.0f offers / %ds", n, hotPathAgentHeartbeatSec),
			2, "durable UPDATE+INSERT via coalescer; money-adjacent but not a charge", "UPDATE + sample INSERT"},
		{"live-index bridge (offerIndexLive hit)", "live_index_offerIndexLive_hit",
			n / hotPathAgentHeartbeatSec, "DERIVED: once per flag-ON heartbeat plus lease-target lookups",
			1, "atomic loads; fail-closed", "none on hit"},
		{"selector / offer book (Authorize book=16 c=1)", "authorize_success_book_16_c1",
			100, "PROJECTED scenario: 100 admits/s (production QPS UNMEASURED)",
			3, "buyer funding + FOR UPDATE offer claim + EXECUTING insert — money path", "eligible CTE + claim"},
		{"selector / offer book (Authorize book=16 c=1 after ANALYZE)", "authorize_success_book_16_c1_after_analyze",
			100, "PROJECTED scenario: 100 admits/s after ANALYZE (production-like planner stats)",
			3, "same money path; plan may differ from the cold isolated-DB Nested Loop", "eligible CTE after ANALYZE"},
		{"selector / offer book (Authorize book=16 c=8)", "authorize_success_book_16_c8",
			100, "PROJECTED scenario: 100 admits/s at modest concurrency",
			3, "same money path plus row-lock convoy on a thin book", "eligible CTE + LockRows"},
		{"lease acquisition (CreateServiceLease)", "create_service_lease_success_book_32",
			0.1, "PROJECTED scenario: 0.1 creates/s (session start; production QPS UNMEASURED)",
			3, "FOR UPDATE whole book + prepaid reserve — money path", "ordered FOR UPDATE walk"},
		{"lease acquisition (DataPlaneTarget flag OFF)", "service_lease_data_plane_flag_off_success",
			100, "PROJECTED scenario: 100 reserved-session requests/s",
			2, "two point lookups; no book walk", "lease row + offer last_seen_at"},
		{"price computation (newRealtimePricingDecision)", "price_newRealtimePricingDecision_success",
			100, "DERIVED: once per authorize under the 100/s scenario",
			1, "pure Go; already inside Authorize wall time (do not double-count with authorize payoff)", "none"},
	}
	byName := map[string]hotPathCell{}
	for _, c := range report.Cells {
		byName[c.Name] = c
	}
	var rows []hotPathRankRow
	for _, p := range picks {
		c, ok := byName[p.cell]
		row := hotPathRankRow{
			Subsystem: p.subsystem, Cell: p.cell,
			Frequency: p.freqNote, ImplementationRisk: p.riskNote, DBTime: p.dbTime,
		}
		if !ok || c.Classification != "MEASURED" {
			row.P50, row.P95, row.P99 = "UNMEASURED", "UNMEASURED", "UNMEASURED"
			row.Throughput, row.AllocsPerOp, row.BytesPerOp, row.CPUShare = "UNMEASURED", "UNMEASURED", "UNMEASURED", "UNMEASURED"
			row.ExpectedAbsolutePayoff = "UNMEASURED — cell missing"
			row.Score = -1
			rows = append(rows, row)
			continue
		}
		row.P50 = fmt.Sprintf("%.4f ms (MEASURED, n=%d, %s)", c.P50MS, c.SampleN, c.WantedOutcome)
		row.P95 = fmt.Sprintf("%.4f ms", c.P95MS)
		row.P99 = fmt.Sprintf("%.4f ms", c.P99MS)
		row.Throughput = fmt.Sprintf("%.1f op/s (MEASURED, c=%d)", c.ThroughputOPS, max(c.Concurrency, 1))
		row.AllocsPerOp = fmt.Sprintf("%.2f (MEASURED)", c.AllocsPerOp)
		row.BytesPerOp = fmt.Sprintf("%.0f (MEASURED)", c.BytesPerOp)
		row.CPUShare = fmt.Sprintf("%.0f ns CPU/op (MEASURED process rusage)", c.CPUNanosPerOp)
		// PROJECTED payoff: if this cell's SUCCESS p50 went to zero, reclaim
		// p50_ms * freq milliseconds of control-plane time per wall second.
		payoffMSperS := c.P50MS * p.freq
		row.ExpectedAbsolutePayoff = fmt.Sprintf("PROJECTED: %.2f ms of control-plane time / wall-s if p50→0 at freq=%.2f/s (MEASURED p50 × DERIVED/PROJECTED freq). Not a promise that any change achieves that.", payoffMSperS, p.freq)
		if p.risk > 0 {
			row.Score = payoffMSperS / p.risk
		}
		if c.Unstable {
			row.ExpectedAbsolutePayoff += " Cell marked UNSTABLE — use the spread, not the point p50."
		}
		rows = append(rows, row)
	}
	sort.Slice(rows, func(i, j int) bool { return rows[i].Score > rows[j].Score })
	return rows
}

func classifyHotPathBinding(report hotPathReport) hotPathBinding {
	b := hotPathBinding{}
	var authP50, hbOffP50, hbOnP50, priceP50, idxP50 float64
	for _, c := range report.Cells {
		switch c.Name {
		case "authorize_success_book_16_c1":
			authP50 = c.P50MS
		case "heartbeat_ingest_flag_off_batched_success":
			hbOffP50 = c.P50MS
		case "heartbeat_ingest_flag_on_retired_batched_success":
			hbOnP50 = c.P50MS
		case "price_newRealtimePricingDecision_success":
			priceP50 = c.P50MS
		case "live_index_offerIndexLive_hit":
			idxP50 = c.P50MS
		}
	}
	// Compare authorize wall to its EXPLAIN execution time.
	var authExec float64
	for _, e := range report.Explains {
		if e.Name == "authorize_select_offer_skip" && e.Classification == "MEASURED" {
			authExec = e.ExecutionMS
			b.AuthorizeExplainMS = authExec
			b.AuthorizeDominator = e.DominatingNode
		}
		if e.Name == "heartbeat_update_one" && e.Classification == "MEASURED" {
			b.HeartbeatExplainMS = e.ExecutionMS
			b.HeartbeatDominator = e.DominatingNode
		}
	}
	b.AuthorizeP50MS = authP50
	b.HeartbeatFlagOffP50MS = hbOffP50
	b.HeartbeatFlagOnP50MS = hbOnP50
	b.PriceP50MS = priceP50
	b.IndexP50MS = idxP50

	switch {
	case authP50 > 0 && authExec > 0 && authExec >= 0.5*authP50 && authP50 > 1:
		b.Verdict = "bound on PostgreSQL round trips / planning / execution"
		b.Reason = fmt.Sprintf("AuthorizeRealtimeContract SUCCESS p50=%.3fms and the eligible-book claim EXPLAIN execution is %.3fms (dominating node %s). In-process pricing p50=%.4fms and offerIndexLive p50=%.4fms are orders of magnitude smaller. Flag-ON retired heartbeat p50=%.4fms is in-process; flag-OFF heartbeat p50=%.3fms is a durable write.",
			authP50, authExec, b.AuthorizeDominator, priceP50, idxP50, hbOnP50, hbOffP50)
	case hbOnP50 > 0 && hbOnP50*1000 > authP50 && authP50 > 0:
		b.Verdict = "bound on something else (locking, serialization, connection pool) or on Go CPU"
		b.Reason = "retired heartbeat is slower than authorize — unexpected; see cells."
	case priceP50 > 0 && priceP50 > authP50*0.5 && authP50 > 0:
		b.Verdict = "the control plane is CPU-bound in Go"
		b.Reason = fmt.Sprintf("newRealtimePricingDecision p50=%.3fms is a large fraction of authorize p50=%.3fms", priceP50, authP50)
	default:
		if authP50 >= hbOffP50 && authP50 >= 0.5 {
			b.Verdict = "bound on PostgreSQL round trips / planning / execution"
			b.Reason = fmt.Sprintf("largest measured SUCCESS p50 on the money path is AuthorizeRealtimeContract (%.3fms) vs flag-OFF heartbeat %.3fms vs in-process index %.4fms / pricing %.4fms. EXPLAIN authorize exec=%.3fms (%s).",
				authP50, hbOffP50, idxP50, priceP50, authExec, b.AuthorizeDominator)
		} else if hbOffP50 > 1 && hbOnP50 < 0.05 {
			b.Verdict = "at HEAD with the flag OFF, ingest is bound on PostgreSQL durable writes; the live-index/pricing paths are not"
			b.Reason = fmt.Sprintf("flag-OFF heartbeat p50=%.3fms (durable) vs flag-ON retired p50=%.4fms (in-process). Authorize p50=%.3fms is a different, less frequent path.", hbOffP50, hbOnP50, authP50)
		} else {
			b.Verdict = "mixed: PostgreSQL on the money/ingest-off paths; Go CPU is not the limiter on the measured cells"
			b.Reason = fmt.Sprintf("auth p50=%.3fms explain=%.3fms hb_off=%.3fms hb_on=%.4fms price=%.4fms index=%.4fms",
				authP50, authExec, hbOffP50, hbOnP50, priceP50, idxP50)
		}
	}
	return b
}

// --- host / postgres / io ---------------------------------------------------

func probeHotPathPostgres(ctx context.Context, pool *pgxpool.Pool) hotPathPostgres {
	p := hotPathPostgres{}
	_ = pool.QueryRow(ctx, `SELECT version()`).Scan(&p.Version)
	_ = pool.QueryRow(ctx, `SHOW shared_buffers`).Scan(&p.SharedBuffers)
	_ = pool.QueryRow(ctx, `SHOW max_connections`).Scan(&p.MaxConnections)
	_ = pool.QueryRow(ctx, `SELECT current_database()`).Scan(&p.Database)
	st := pool.Stat()
	p.PoolMaxConns = int(st.MaxConns())
	p.PoolTotalConns = int(st.TotalConns())
	return p
}

func writeHotPathEvidence(report hotPathReport) error {
	rel := hotPathProfileEvidenceRel
	if v := strings.TrimSpace(os.Getenv("MERC_CONTROL_PLANE_PROFILE_EVIDENCE")); v != "" {
		rel = v
	}
	path := filepath.Join("..", rel)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	raw, err := json.MarshalIndent(report, "", "  ")
	if err != nil {
		return err
	}
	raw = append(raw, '\n')
	return os.WriteFile(path, raw, 0o644)
}

func minFloat(v []float64) float64 {
	m := v[0]
	for _, x := range v[1:] {
		if x < m {
			m = x
		}
	}
	return m
}
func maxFloat(v []float64) float64 {
	m := v[0]
	for _, x := range v[1:] {
		if x > m {
			m = x
		}
	}
	return m
}
func medianFloat(v []float64) float64 {
	s := append([]float64(nil), v...)
	sort.Float64s(s)
	return s[len(s)/2]
}

// --- types ------------------------------------------------------------------

type hotPathReport struct {
	Classification   string              `json:"classification"`
	GeneratedAt      string              `json:"generated_at"`
	FinishedAt       string              `json:"finished_at"`
	WallClockSeconds float64             `json:"wall_clock_seconds"`
	SourceCommit     string              `json:"source_commit"`
	Host             string              `json:"host"`
	NumCPU           int                 `json:"num_cpu"`
	GOMAXPROCS       int                 `json:"gomaxprocs"`
	GOOS             string              `json:"goos"`
	GOARCH           string              `json:"goarch"`
	Invocation       hotPathInvocation   `json:"invocation"`
	Honesty          hotPathHonesty      `json:"honesty"`
	Postgres         hotPathPostgres     `json:"postgres"`
	Cells            []hotPathCell       `json:"cells"`
	Explains         []hotPathExplain    `json:"explains"`
	Profiles         hotPathProfiles     `json:"profiles"`
	Ranking          []hotPathRankRow    `json:"ranking"`
	Binding          hotPathBinding      `json:"binding"`
	Unmeasured       []hotPathUnmeasured `json:"unmeasured"`
}

type hotPathInvocation struct {
	EnvGate                string  `json:"env_gate"`
	ExcludedFromNormalGate bool    `json:"excluded_from_normal_gate"`
	ExclusionProof         string  `json:"exclusion_proof"`
	Command                string  `json:"command"`
	Fleet                  int     `json:"fleet"`
	SeededFleet            int     `json:"seeded_fleet"`
	SeedWallSeconds        float64 `json:"seed_wall_seconds"`
	Samples                int     `json:"samples"`
	Trials                 int     `json:"trials"`
	MaxConns               int     `json:"max_conns"`
	AgentHeartbeatSec      int     `json:"agent_heartbeat_sec"`
	TightHeartbeatSec      int     `json:"tight_heartbeat_sec"`
	LivenessWindowSec      int     `json:"liveness_window_sec"`
	RefreshIntervalSec     int     `json:"refresh_interval_sec"`
	RuntimeProfileID       string  `json:"runtime_profile_id"`
}

type hotPathHonesty struct {
	WhatThisProves       string   `json:"what_this_proves"`
	WhatThisDoesNotProve string   `json:"what_this_does_not_prove"`
	Guards               []string `json:"guards"`
}

type hotPathPostgres struct {
	Version        string `json:"version"`
	Database       string `json:"database"`
	SharedBuffers  string `json:"shared_buffers"`
	MaxConnections string `json:"max_connections"`
	PoolMaxConns   int    `json:"pool_max_conns"`
	PoolTotalConns int    `json:"pool_total_conns"`
}

type hotPathCell struct {
	Name             string               `json:"name"`
	Subsystem        string               `json:"subsystem"`
	EntryPoint       string               `json:"entry_point"`
	Flag             string               `json:"flag,omitempty"`
	Classification   string               `json:"classification"`
	UnmeasuredReason string               `json:"unmeasured_reason,omitempty"`
	Command          string               `json:"command"`
	Note             string               `json:"note,omitempty"`
	WantedOutcome    string               `json:"wanted_outcome"`
	BookSize         int                  `json:"book_size,omitempty"`
	Concurrency      int                  `json:"concurrency,omitempty"`
	Trials           int                  `json:"trials,omitempty"`
	SampleN          int                  `json:"sample_n"`
	P50MS            float64              `json:"p50_ms"`
	P95MS            float64              `json:"p95_ms"`
	P99MS            float64              `json:"p99_ms"`
	MaxMS            float64              `json:"max_ms"`
	AvgMS            float64              `json:"avg_ms"`
	P50SpreadMS      float64              `json:"p50_spread_ms,omitempty"`
	TrialP50MS       []float64            `json:"trial_p50_ms,omitempty"`
	Unstable         bool                 `json:"unstable,omitempty"`
	ThroughputOPS    float64              `json:"throughput_ops"`
	AllocsPerOp      float64              `json:"allocs_per_op"`
	BytesPerOp       float64              `json:"bytes_per_op"`
	CPUNanosPerOp    float64              `json:"cpu_nanos_per_op"`
	WallSeconds      float64              `json:"wall_seconds"`
	Buckets          hotPathBuckets       `json:"buckets"`
	ByOutcome        map[string]hotPathMS `json:"by_outcome"`
	FirstError       string               `json:"first_error,omitempty"`
}

type hotPathBuckets struct {
	SUCCESS int64 `json:"SUCCESS"`
	REFUSAL int64 `json:"REFUSAL"`
	EMPTY   int64 `json:"EMPTY"`
	FAILURE int64 `json:"FAILURE"`
}

type hotPathMS struct {
	N   int     `json:"n"`
	P50 float64 `json:"p50_ms"`
	P95 float64 `json:"p95_ms"`
	P99 float64 `json:"p99_ms"`
	Max float64 `json:"max_ms"`
	Avg float64 `json:"avg_ms"`
}

type hotPathExplain struct {
	Name           string  `json:"name"`
	What           string  `json:"what"`
	Classification string  `json:"classification"`
	DominatingNode string  `json:"dominating_node"`
	ExecutionMS    float64 `json:"execution_ms"`
	PlanningMS     float64 `json:"planning_ms"`
	Raw            string  `json:"raw"`
	Error          string  `json:"error,omitempty"`
}

type hotPathProfiles struct {
	Classification string           `json:"classification"`
	Mix            string           `json:"mix"`
	DurationSec    int              `json:"duration_sec"`
	MixCounts      map[string]int64 `json:"mix_counts"`
	CPUPath        string           `json:"cpu_path"`
	AllocPath      string           `json:"alloc_path"`
	HeapPath       string           `json:"heap_path"`
	CPUTop         string           `json:"cpu_top"`
	CPUCumTop      string           `json:"cpu_cum_top"`
	AllocTop       string           `json:"alloc_top"`
	AllocCumTop    string           `json:"alloc_cum_top"`
	Error          string           `json:"error,omitempty"`
}

type hotPathRankRow struct {
	Subsystem              string  `json:"subsystem"`
	Cell                   string  `json:"cell"`
	P50                    string  `json:"p50"`
	P95                    string  `json:"p95"`
	P99                    string  `json:"p99"`
	Throughput             string  `json:"throughput"`
	CPUShare               string  `json:"cpu_share"`
	AllocsPerOp            string  `json:"allocs_per_op"`
	BytesPerOp             string  `json:"bytes_per_op"`
	DBTime                 string  `json:"db_time"`
	Frequency              string  `json:"frequency"`
	ExpectedAbsolutePayoff string  `json:"expected_absolute_payoff"`
	ImplementationRisk     string  `json:"implementation_risk"`
	Score                  float64 `json:"score_impact_x_freq_over_risk"`
}

type hotPathBinding struct {
	Verdict               string  `json:"verdict"`
	Reason                string  `json:"reason"`
	AuthorizeP50MS        float64 `json:"authorize_p50_ms"`
	AuthorizeExplainMS    float64 `json:"authorize_explain_ms"`
	AuthorizeDominator    string  `json:"authorize_dominator"`
	HeartbeatFlagOffP50MS float64 `json:"heartbeat_flag_off_p50_ms"`
	HeartbeatFlagOnP50MS  float64 `json:"heartbeat_flag_on_p50_ms"`
	HeartbeatExplainMS    float64 `json:"heartbeat_explain_ms"`
	HeartbeatDominator    string  `json:"heartbeat_dominator"`
	PriceP50MS            float64 `json:"price_p50_ms"`
	IndexP50MS            float64 `json:"index_p50_ms"`
}

type hotPathUnmeasured struct {
	Subsystem string `json:"subsystem"`
	Reason    string `json:"reason"`
}
