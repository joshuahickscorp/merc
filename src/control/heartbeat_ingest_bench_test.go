package main

// Heartbeat-ingest ceiling harness.
//
// Measures the production HeartbeatRealtimeOffer entry point (the same store
// method POST /v1/worker/realtime/heartbeat calls after authWorker) under a
// synthetic fleet. There is no Go imitation of the write path and no synthetic
// UPDATE that bypasses HeartbeatRealtimeOffer.
//
// Opt-in only — never part of make test / make ci:
//
//	MERC_HEARTBEAT_INGEST_BENCH=1 \
//	MERC_TEST_DATABASE_URL=postgres://cx:cx@localhost:5432/cx?sslmode=disable \
//	bash ops/scripts/with-isolated-test-db.sh \
//	  bash -c 'cd src/control && go test -count=1 -run '^TestHeartbeatIngestBench$' -timeout 2h .'
//
// Writes evidence/perf/heartbeat-ingest-ceiling.json.
//
// Modes (MERC_HEARTBEAT_INGEST_MODE):
//   both      — baseline (batch off) then batched (default)
//   baseline  — one statement per device (MERC_LIVENESS_BATCH=0 shape)
//   batched   — coalesced multi-row path
//
// Optional constrained Postgres: point MERC_TEST_DATABASE_URL at a 1 vCPU /
// 961 MB cgroup-limited container (see evidence artifact host notes). The
// harness records whatever host it is given; it does not start Docker itself.

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

const (
	heartbeatIngestEnv         = "MERC_HEARTBEAT_INGEST_BENCH"
	heartbeatIngestModeEnv     = "MERC_HEARTBEAT_INGEST_MODE"
	heartbeatIngestFleetEnv    = "MERC_HEARTBEAT_INGEST_FLEET"
	heartbeatIngestConcEnv     = "MERC_HEARTBEAT_INGEST_CONCURRENCY"
	heartbeatIngestDurationEnv = "MERC_HEARTBEAT_INGEST_DURATION_SEC"
	heartbeatIngestMaxConnsEnv = "MERC_HEARTBEAT_INGEST_MAX_CONNS"
	heartbeatIngestEvidenceRel = "evidence/perf/heartbeat-ingest-ceiling.json"
	heartbeatIngestSeed        = int64(20260810)
)

func TestHeartbeatIngestBench(t *testing.T) {
	if os.Getenv(heartbeatIngestEnv) != "1" {
		t.Skip("set MERC_HEARTBEAT_INGEST_BENCH=1 to measure heartbeat ingest ceiling")
	}
	installSettlementCurrencyForTest(t, "usd")
	t.Setenv("MERC_TOKEN_KEY", "heartbeat-ingest-bench-key-32bytes-min!")

	mode := strings.TrimSpace(os.Getenv(heartbeatIngestModeEnv))
	if mode == "" {
		mode = "both"
	}
	fleets := parseIntListEnv(t, heartbeatIngestFleetEnv, []int{1_000, 10_000, 50_000})
	concs := parseIntListEnv(t, heartbeatIngestConcEnv, []int{1, 8, 20, 40})
	durationSec := 8
	if v := strings.TrimSpace(os.Getenv(heartbeatIngestDurationEnv)); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 2 {
			t.Fatalf("%s=%q: need integer >= 2", heartbeatIngestDurationEnv, v)
		}
		durationSec = n
	}
	maxConns := int32(20)
	if v := strings.TrimSpace(os.Getenv(heartbeatIngestMaxConnsEnv)); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 1 {
			t.Fatalf("%s=%q: need positive integer", heartbeatIngestMaxConnsEnv, v)
		}
		maxConns = int32(n)
	}

	loadBefore, _ := readLoadAverage()
	host, _ := os.Hostname()
	commit := mercSourceCommitSHA()
	startedAt := time.Now().UTC()

	// Postgres identity / resource note for the artifact.
	store, pool := openHeartbeatIngestStore(t, maxConns)
	ctx := context.Background()
	pgInfo := probePostgresInfo(ctx, pool)

	report := heartbeatIngestReport{
		SchemaVersion:     2,
		GeneratedAt:       startedAt.Format(time.RFC3339),
		SourceCommit:      commit,
		Seed:              heartbeatIngestSeed,
		Host:              host,
		NumCPU:            runtime.NumCPU(),
		LoadAverageBefore: loadBefore,
		Invocation: heartbeatIngestInvocation{
			EnvGate:                heartbeatIngestEnv + "=1",
			Command:                "bash ops/scripts/with-isolated-test-db.sh bash -c 'cd src/control && go test -count=1 -run ^TestHeartbeatIngestBench$ -timeout 2h .'",
			ExcludedFromNormalGate: true,
			ExclusionProof:         "TestHeartbeatIngestBench skips unless MERC_HEARTBEAT_INGEST_BENCH=1; listed in ops/scripts/allowed-test-skips.txt; make test / make ci never set the env var",
			Mode:                   mode,
			DurationSec:            durationSec,
			MaxConns:               int(maxConns),
			FleetLadder:            fleets,
			ConcurrencyLadder:      concs,
		},
		Postgres:  pgInfo,
		HostClass: probeHostClass(),
		Honesty: heartbeatIngestHonesty{
			WhatThisProves:       "sustained durable heartbeats/sec through the production HeartbeatRealtimeOffer entry point on one constrained PostgreSQL, baseline vs coalesced batching, plus selector eligibility/probe latency and relation footprint at each fleet size",
			WhatThisDoesNotProve: "a DigitalOcean x86 droplet vCPU, physical multi-region fleet performance, network RTT from real agents, or 10M devices without naming the break",
			Guards: []string{
				"failure is not fast: failed samples are counted and excluded from success-only latency percentiles",
				"empty is not fast: a zero-success cell is refused, not reported as zero-latency",
				"refusal timed as success is a lie: only HeartbeatRealtimeOffer err==nil samples enter p50/p99",
				"cold start is not silently dropped: per-cell warmup latencies are recorded under warmup_ms",
				"bimodal cell has no quotable p50: cells with p99/p50 > 20 are flagged unstable_population",
				"this container is a PROXY: Docker --cpus=1 --memory=961m on an Apple M3 host, not a DO droplet x86 vCPU",
				"10M is not extrapolated: implied_fleet is floor(hb/s * 45s); 10M / implied_fleet is the host-count lower bound only while the working set fits RAM and the selector stays linear",
			},
			LivenessWindowSeconds: int(realtimeOfferLivenessWindow / time.Second),
			AgentHeartbeatSeconds: 15,
			ImpliedFleetFormula:   "floor(sustained_heartbeats_per_sec * liveness_window_seconds)",
			ProxyCaveat:           "M3 ARM core behind Docker Desktop, not a DigitalOcean x86 vCPU. The droplet number is proportionally lower; this is an UPPER bound for the 1vCPU/961MB class.",
		},
		Modes: map[string]heartbeatIngestModeResult{},
	}

	modes := []string{}
	switch mode {
	case "both":
		modes = []string{"baseline", "batched"}
	case "both_tight":
		modes = []string{"baseline", "batched", "batched_tight"}
	case "baseline", "batched", "batched_tight":
		modes = []string{mode}
	default:
		t.Fatalf("unknown %s=%q (want both|baseline|batched)", heartbeatIngestModeEnv, mode)
	}

	profile := sortedVLLMProfiles()[0]
	for _, m := range modes {
		t.Logf("mode=%s starting", m)
		// Fresh book per mode so sample-table growth from baseline does not
		// contaminate batched WAL numbers.
		if err := wipeHeartbeatIngestTables(ctx, pool); err != nil {
			t.Fatalf("wipe: %v", err)
		}
		result := runHeartbeatIngestMode(t, ctx, store, pool, profile, m, fleets, concs, durationSec, maxConns)
		report.Modes[m] = result
	}

	loadAfter, _ := readLoadAverage()
	report.LoadAverageAfter = loadAfter
	report.FinishedAt = time.Now().UTC().Format(time.RFC3339)
	report.WallClockSeconds = time.Since(startedAt).Seconds()
	report.Verdict = buildHeartbeatIngestVerdict(report)

	if err := writeHeartbeatIngestEvidence(report); err != nil {
		t.Fatalf("write evidence: %v", err)
	}
	if name := strings.TrimSpace(os.Getenv("MERC_HEARTBEAT_INGEST_DOCKER_CONTAINER")); name != "" &&
		os.Getenv("MERC_HEARTBEAT_INGEST_SKIP_RESTART") != "1" {
		report.Restart = measurePostgresRestart(t, name, pool)
		if err := writeHeartbeatIngestEvidence(report); err != nil {
			t.Fatalf("rewrite evidence with restart: %v", err)
		}
	}
	t.Logf("wrote %s verdict=%s", heartbeatIngestEvidenceRel, report.Verdict.OneSentence)
}

// --- measurement core -------------------------------------------------------

func runHeartbeatIngestMode(
	t *testing.T, ctx context.Context, store *Store, pool *pgxpool.Pool,
	profile VLLMRuntimeProfile, mode string, fleets, concs []int, durationSec int, maxConns int32,
) heartbeatIngestModeResult {
	t.Helper()
	batchOn := strings.HasPrefix(mode, "batched")
	flush := defaultLivenessBatchFlushInterval
	if strings.Contains(mode, "tight") {
		flush = 2 * time.Millisecond
	}
	if v := strings.TrimSpace(os.Getenv("MERC_HEARTBEAT_INGEST_FLUSH_MS")); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n >= 0 {
			flush = time.Duration(n) * time.Millisecond
		}
	}
	// Re-bind coalescer config for this mode. NewStore already ran; override
	// via a fresh store sharing the same pool so Once does not stick to the
	// wrong config across modes.
	store = NewStore(pool)
	store.SetLivenessBatchConfigForTest(livenessBatchConfig{
		Enabled:       batchOn,
		MaxBatch:      defaultLivenessBatchMaxSize,
		FlushInterval: flush,
	})

	out := heartbeatIngestModeResult{
		Mode:                 mode,
		BatchEnabled:         batchOn,
		MaxBatch:             defaultLivenessBatchMaxSize,
		FlushInterval:        flush.String(),
		FlushIntervalWhySafe: "last_seen_at is the clamped device/receipt observation time, not flush-time now(); a dead device that stops at T0 has last_seen_at≤T0 and leaves eligibility at T0+45s regardless of flush delay B",
		Cells:                []heartbeatIngestCell{},
		Refusals:             []heartbeatIngestRefusal{},
	}

	// Seed the largest fleet once; smaller fleet measurements slice the worker
	// list. Seeding uses COPY for setup cost only — timed samples always call
	// HeartbeatRealtimeOffer. Step down if the constrained host cannot hold
	// the requested max (OOM / disk / statement timeout).
	maxFleet := fleets[0]
	for _, f := range fleets {
		if f > maxFleet {
			maxFleet = f
		}
	}
	seedOrder := []int{maxFleet}
	for i := len(fleets) - 1; i >= 0; i-- {
		if fleets[i] < maxFleet {
			seedOrder = append(seedOrder, fleets[i])
		}
	}
	var workers []WorkerAuth
	var err error
	seedStart := time.Now()
	for _, n := range seedOrder {
		t.Logf("mode=%s seeding fleet=%d…", mode, n)
		if wipeErr := wipeHeartbeatIngestTables(ctx, pool); wipeErr != nil {
			out.Refusals = append(out.Refusals, heartbeatIngestRefusal{
				Kind: "wipe_failed", Summary: wipeErr.Error(),
			})
			return out
		}
		workers, err = seedHeartbeatIngestFleet(ctx, pool, profile, n, heartbeatIngestSeed)
		if err == nil {
			maxFleet = n
			break
		}
		t.Logf("mode=%s seed fleet=%d failed: %v — stepping down", mode, n, err)
		out.Refusals = append(out.Refusals, heartbeatIngestRefusal{
			Kind: "seed_stepdown", Summary: fmt.Sprintf("fleet=%d: %v", n, err),
		})
		workers = nil
	}
	if err != nil || len(workers) == 0 {
		out.Refusals = append(out.Refusals, heartbeatIngestRefusal{
			Kind: "seed_failed", Summary: fmt.Sprintf("%v", err),
		})
		return out
	}
	out.SeedWallSeconds = time.Since(seedStart).Seconds()
	out.SeededFleet = len(workers)
	out.FootprintAfterSeed = probeRelationFootprint(ctx, pool, len(workers))
	t.Logf("mode=%s seed done in %.1fs fleet=%d bytes/device=%.0f devices/GB=%.0f",
		mode, out.SeedWallSeconds, len(workers),
		out.FootprintAfterSeed.BytesPerDevice, out.FootprintAfterSeed.DevicesPerGB)

	// Warm the production path once so the first cell is not pure cold-start.
	// Recorded, not silent.
	warmHB := RealtimeOfferHeartbeat{
		RuntimeProfileID: profile.RuntimeProfileID, Warmth: "HOT",
		AvailableSequences: 1000, Status: "ACTIVE",
	}
	warmStart := time.Now()
	_ = store.HeartbeatRealtimeOffer(ctx, workers[0], warmHB)
	out.PathWarmupMS = float64(time.Since(warmStart).Milliseconds())

	for _, fleet := range fleets {
		if fleet > len(workers) {
			out.Refusals = append(out.Refusals, heartbeatIngestRefusal{
				Kind: "fleet_exceeds_seed", Summary: fmt.Sprintf("fleet=%d > seeded=%d", fleet, len(workers)),
			})
			continue
		}
		slice := workers[:fleet]
		for _, conc := range concs {
			cell := measureHeartbeatIngestCell(t, ctx, store, pool, profile, mode, slice, conc, durationSec, maxConns)
			out.Cells = append(out.Cells, cell)
			t.Logf("mode=%s fleet=%d conc=%d thr=%.0f/s p50=%.2fms p99=%.2fms fail=%d sat=%s implied_fleet=%.0f sel_elig_p50=%.2fms sel_probe_p50=%.2fms stmts/s=%.0f wal/s=%.0f",
				mode, fleet, conc, cell.HeartbeatsPerSec, cell.LatencyMS.P50, cell.LatencyMS.P99,
				cell.Failures, cell.SaturatingResource, cell.ImpliedFleetCeiling,
				cell.SelectorEligibilityMS.P50, cell.SelectorBranchProbeMS.P50,
				cell.StatementsPerSec, cell.WALBytesPerSec)
			if cell.Refusal != nil {
				out.Refusals = append(out.Refusals, *cell.Refusal)
			}
		}
	}

	// Pick the best sustained success throughput as the mode ceiling (among
	// non-refused, non-unstable cells).
	var best float64
	var bestCell *heartbeatIngestCell
	for i := range out.Cells {
		c := &out.Cells[i]
		if c.Refusal != nil || c.UnstablePopulation {
			continue
		}
		if c.HeartbeatsPerSec > best {
			best = c.HeartbeatsPerSec
			bestCell = c
		}
	}
	if bestCell != nil {
		out.BestSustainedHeartbeatsPerSec = best
		out.BestCell = fmt.Sprintf("fleet=%d conc=%d", bestCell.Fleet, bestCell.Concurrency)
		out.SaturatingResourceAtBest = bestCell.SaturatingResource
		out.ImpliedFleetCeiling = math.Floor(best * float64(int(realtimeOfferLivenessWindow/time.Second)))
		out.WALBytesPerHeartbeatAtBest = bestCell.WALBytesPerHeartbeat
	} else {
		out.Refusals = append(out.Refusals, heartbeatIngestRefusal{
			Kind: "no_quotable_cell", Summary: "every cell refused or unstable; no ceiling published for this mode",
		})
	}
	return out
}

func measureHeartbeatIngestCell(
	t *testing.T, ctx context.Context, store *Store, pool *pgxpool.Pool,
	profile VLLMRuntimeProfile, mode string, workers []WorkerAuth, conc, durationSec int, maxConns int32,
) heartbeatIngestCell {
	t.Helper()
	fleet := len(workers)
	cell := heartbeatIngestCell{
		Mode: mode, Fleet: fleet, Concurrency: conc, DurationSec: durationSec, MaxConns: int(maxConns),
	}

	// Per-cell serial warmup (cold buffers / plan). Recorded under warmup_ms.
	const warmupN = 8
	warmHB := RealtimeOfferHeartbeat{
		RuntimeProfileID: profile.RuntimeProfileID, Warmth: "HOT",
		AvailableSequences: 1000, Status: "ACTIVE",
	}
	for i := 0; i < warmupN; i++ {
		w := workers[i%len(workers)]
		start := time.Now()
		err := store.HeartbeatRealtimeOffer(ctx, w, warmHB)
		cell.WarmupMS = append(cell.WarmupMS, float64(time.Since(start).Microseconds())/1000.0)
		if err != nil {
			cell.WarmupFailures++
		}
	}

	walBefore := walLSN(ctx, pool)
	bytesBefore := dbRelationBytes(ctx, pool)
	statBefore := pgStatDatabase(ctx, pool)
	flushBefore, itemsBefore := store.LivenessFlushStats()
	loadBefore, _ := readLoadAverage()
	waitBefore := poolWaitSnapshot(pool)
	memBefore := dockerMemBytes()

	duration := time.Duration(durationSec) * time.Second
	deadline := time.Now().Add(duration)
	var (
		okN      atomic.Int64
		failN    atomic.Int64
		idx      atomic.Uint64
		latMu    sync.Mutex
		success  []float64
		failNote atomic.Value
	)
	failNote.Store("")

	var (
		selMu        sync.Mutex
		eligMS       []float64
		probeMS      []float64
		selFail      atomic.Int64
		selectorStop = make(chan struct{})
		selectorDone = make(chan struct{})
	)
	go func() {
		defer close(selectorDone)
		ticker := time.NewTicker(50 * time.Millisecond)
		defer ticker.Stop()
		for {
			select {
			case <-selectorStop:
				return
			case <-ticker.C:
				e, p, err := sampleSelectorUnderLoad(ctx, pool, profile)
				if err != nil {
					selFail.Add(1)
					continue
				}
				selMu.Lock()
				eligMS = append(eligMS, e)
				probeMS = append(probeMS, p)
				selMu.Unlock()
			}
		}
	}()

	var wg sync.WaitGroup
	for c := 0; c < conc; c++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			hb := RealtimeOfferHeartbeat{
				RuntimeProfileID: profile.RuntimeProfileID, Warmth: "HOT",
				AvailableSequences: 1000, Status: "ACTIVE",
			}
			for time.Now().Before(deadline) {
				i := int(idx.Add(1)-1) % len(workers)
				// Device observation = receipt time (nil ObservedAtUnixMs).
				start := time.Now()
				err := store.HeartbeatRealtimeOffer(ctx, workers[i], hb)
				elapsedMS := float64(time.Since(start).Microseconds()) / 1000.0
				if err != nil {
					failN.Add(1)
					if failNote.Load().(string) == "" {
						failNote.Store(err.Error())
					}
					continue
				}
				okN.Add(1)
				latMu.Lock()
				success = append(success, elapsedMS)
				latMu.Unlock()
			}
		}()
	}
	wg.Wait()
	close(selectorStop)
	<-selectorDone
	wall := duration.Seconds()

	walAfter := walLSN(ctx, pool)
	bytesAfter := dbRelationBytes(ctx, pool)
	statAfter := pgStatDatabase(ctx, pool)
	flushAfter, itemsAfter := store.LivenessFlushStats()
	loadAfter, _ := readLoadAverage()
	waitAfter := poolWaitSnapshot(pool)
	memAfter := dockerMemBytes()
	cell.Footprint = probeRelationFootprint(ctx, pool, fleet)
	cell.PostgresRSSBytesBefore = memBefore
	cell.PostgresRSSBytesAfter = memAfter

	ok := okN.Load()
	fail := failN.Load()
	cell.Successes = ok
	cell.Failures = fail
	cell.FirstError = failNote.Load().(string)
	cell.HeartbeatsPerSec = float64(ok) / wall
	cell.AttemptsPerSec = float64(ok+fail) / wall
	cell.LoadAverageBefore = loadBefore
	cell.LoadAverageAfter = loadAfter
	cell.PoolAcquiredBefore = waitBefore
	cell.PoolAcquiredAfter = waitAfter

	walDelta := walAfter - walBefore
	if walDelta < 0 {
		walDelta = 0
	}
	cell.WALBytes = walDelta
	if ok > 0 {
		cell.WALBytesPerHeartbeat = float64(walDelta) / float64(ok)
	}
	cell.WALBytesPerSec = float64(walDelta) / wall
	cell.RelationBytesDelta = bytesAfter - bytesBefore

	if ok == 0 {
		cell.Refusal = &heartbeatIngestRefusal{
			Kind: "all_samples_failed",
			Summary: fmt.Sprintf("%s fleet=%d conc=%d: every timed heartbeat failed (%s) — empty/failure is not reported as fast",
				mode, fleet, conc, cell.FirstError),
		}
		return cell
	}

	cell.LatencyMS = percentileMS(success)
	if cell.LatencyMS.P50 > 0 && cell.LatencyMS.P99/cell.LatencyMS.P50 > 20 {
		cell.UnstablePopulation = true
		// Fat tail with zero failures is typical 1vCPU fsync/checkpoint, not a
		// mixed success/refuse population. Flag it; still quote p50.
		if cell.Failures > 0 {
			cell.Refusal = &heartbeatIngestRefusal{
				Kind: "unstable_population",
				Summary: fmt.Sprintf("%s fleet=%d conc=%d: p99/p50=%.1f with failures — bimodal cell has no quotable p50",
					mode, fleet, conc, cell.LatencyMS.P99/cell.LatencyMS.P50),
			}
		}
	}

	cell.ImpliedFleetCeiling = math.Floor(cell.HeartbeatsPerSec * float64(int(realtimeOfferLivenessWindow/time.Second)))

	// Statements: baseline is 2 SQL statements per heartbeat (UPDATE+INSERT)
	// plus the transaction. Batched uses coalescer flush counts when present.
	xactDelta := statAfter.XactCommit - statBefore.XactCommit
	if xactDelta < 0 {
		xactDelta = 0
	}
	cell.XactCommitDelta = xactDelta
	if d := statAfter.TupUpdated - statBefore.TupUpdated; d > 0 {
		cell.TupUpdatedDelta = d
	}
	if d := statAfter.TupInserted - statBefore.TupInserted; d > 0 {
		cell.TupInsertedDelta = d
	}
	cell.XactPerSec = float64(xactDelta) / wall
	flushes := flushAfter - flushBefore
	items := itemsAfter - itemsBefore
	cell.CoalescerFlushes = flushes
	cell.CoalescerItems = items
	if strings.HasPrefix(mode, "batched") && flushes > 0 {
		// One UPDATE + one INSERT per multi-row flush (single-item flushes
		// still use the one-row shape). Plus BEGIN/COMMIT around each flush.
		cell.StatementsPerSec = float64(flushes*2) / wall
		cell.StatementsPerHeartbeat = float64(flushes*2) / float64(ok)
	} else {
		cell.StatementsPerSec = float64(ok*2) / wall
		if ok > 0 {
			cell.StatementsPerHeartbeat = 2
		}
	}

	selMu.Lock()
	cell.SelectorEligibilityMS = percentileMS(eligMS)
	cell.SelectorBranchProbeMS = percentileMS(probeMS)
	selMu.Unlock()
	cell.SelectorSampleFailures = selFail.Load()

	cell.SaturatingResource = diagnoseSaturation(cell, conc, int(maxConns))
	return cell
}

func diagnoseSaturation(cell heartbeatIngestCell, conc, maxConns int) string {
	// Heuristic naming of the binding resource. Published as a finding, not a proof.
	memLimit := int64(961 * 1024 * 1024)
	switch {
	case cell.Failures > cell.Successes/10 && strings.Contains(strings.ToLower(cell.FirstError), "connect"):
		return "connection_pool"
	case cell.Failures > cell.Successes/10 && (strings.Contains(strings.ToLower(cell.FirstError), "oom") ||
		strings.Contains(strings.ToLower(cell.FirstError), "out of memory")):
		return "memory_oom"
	case cell.PostgresRSSBytesAfter > 0 && cell.PostgresRSSBytesAfter > memLimit*85/100:
		return "memory_working_set"
	case conc >= maxConns && cell.LatencyMS.P99 > 5*cell.LatencyMS.P50 && cell.LatencyMS.P50 > 5:
		return "connection_pool_or_client_wait"
	case cell.WALBytesPerSec > 80<<20: // >80 MB/s WAL
		return "wal_fsync_or_wal_volume"
	case cell.WALBytesPerSec > 20<<20 && cell.LatencyMS.P95 > 20:
		return "wal_or_fsync"
	case cell.SelectorEligibilityMS.P95 > 50 && cell.SelectorEligibilityMS.N > 4 &&
		cell.HeartbeatsPerSec < float64(conc)*20:
		return "selector_scan_superlinear_or_io"
	case cell.LatencyMS.P99 > 50 && cell.HeartbeatsPerSec < float64(conc)*50:
		return "cpu_or_lock_contention"
	case cell.LatencyMS.P50 < 2 && cell.HeartbeatsPerSec > float64(conc)*200:
		return "client_round_trips_or_under_loaded"
	default:
		if conc >= maxConns {
			return "likely_connection_pool_ceiling"
		}
		return "cpu_or_mixed_host_bound"
	}
}

// --- seeding ----------------------------------------------------------------

func seedHeartbeatIngestFleet(ctx context.Context, pool *pgxpool.Pool, profile VLLMRuntimeProfile, n int, seed int64) ([]WorkerAuth, error) {
	if n < 1 {
		return nil, fmt.Errorf("fleet must be >= 1")
	}
	// Reuse the selector-scale COPY seeder shape for setup only.
	if err := seedSelectorRealtimeBook(ctx, pool, profile, n, seed); err != nil {
		return nil, err
	}
	out := make([]WorkerAuth, n)
	for i := 0; i < n; i++ {
		out[i] = WorkerAuth{
			WorkerID:   detUUID(seed, "rtw", i),
			SupplierID: detUUID(seed, "rts", i),
		}
	}
	return out, nil
}

func wipeHeartbeatIngestTables(ctx context.Context, pool *pgxpool.Pool) error {
	_, err := pool.Exec(ctx, `
		TRUNCATE
		  realtime_offer_samples, realtime_worker_offers,
		  worker_tokens, workers, suppliers
		RESTART IDENTITY CASCADE`)
	return err
}

// --- postgres probes --------------------------------------------------------

func walLSN(ctx context.Context, pool *pgxpool.Pool) int64 {
	var lsn string
	if err := pool.QueryRow(ctx, `SELECT pg_current_wal_lsn()::text`).Scan(&lsn); err != nil {
		return 0
	}
	// Convert LSN text to a comparable int via pg_wal_lsn_diff against 0/0.
	var bytes int64
	if err := pool.QueryRow(ctx, `SELECT pg_wal_lsn_diff(pg_current_wal_lsn(), '0/0')`).Scan(&bytes); err != nil {
		return 0
	}
	return bytes
}

func dbRelationBytes(ctx context.Context, pool *pgxpool.Pool) int64 {
	var n int64
	_ = pool.QueryRow(ctx, `
		SELECT COALESCE(sum(pg_total_relation_size(oid)),0)
		  FROM pg_class
		 WHERE relname IN ('realtime_worker_offers','realtime_offer_samples','workers')`).Scan(&n)
	return n
}

func poolWaitSnapshot(pool *pgxpool.Pool) map[string]int32 {
	stat := pool.Stat()
	return map[string]int32{
		"acquired":     stat.AcquiredConns(),
		"idle":         stat.IdleConns(),
		"total":        stat.TotalConns(),
		"max":          stat.MaxConns(),
		"constructing": stat.ConstructingConns(),
	}
}

func probePostgresInfo(ctx context.Context, pool *pgxpool.Pool) heartbeatIngestPostgres {
	var version, sharedBuffers, fsync, synCommit string
	var maxConn int
	var maxConnText string
	_ = pool.QueryRow(ctx, `SHOW server_version`).Scan(&version)
	_ = pool.QueryRow(ctx, `SHOW shared_buffers`).Scan(&sharedBuffers)
	_ = pool.QueryRow(ctx, `SHOW max_connections`).Scan(&maxConnText)
	_ = pool.QueryRow(ctx, `SHOW fsync`).Scan(&fsync)
	_ = pool.QueryRow(ctx, `SHOW synchronous_commit`).Scan(&synCommit)
	maxConn, _ = strconv.Atoi(strings.TrimSpace(maxConnText))
	// cgroup limits are not visible from SQL; the harness records an operator
	// note when MERC_HEARTBEAT_INGEST_HOST_NOTE is set.
	return heartbeatIngestPostgres{
		ServerVersion:     version,
		SharedBuffers:     sharedBuffers,
		MaxConnections:    maxConn,
		Fsync:             fsync,
		SynchronousCommit: synCommit,
		HostNote:          strings.TrimSpace(os.Getenv("MERC_HEARTBEAT_INGEST_HOST_NOTE")),
		ContainerName:     strings.TrimSpace(os.Getenv("MERC_HEARTBEAT_INGEST_DOCKER_CONTAINER")),
		CgroupMemoryBytes: 961 * 1024 * 1024,
		CgroupCPU:         1,
		ConfigSource:      "ops/smallhost/postgresql.conf values applied as -c flags on postgres:17",
	}
}

func probeHostClass() heartbeatIngestHostClass {
	arch := runtime.GOARCH
	note := strings.TrimSpace(os.Getenv("MERC_HEARTBEAT_INGEST_HOST_NOTE"))
	if note == "" {
		note = "Docker --cpus=1 --memory=961m --memory-swap=3009m on this machine is a PROXY for a DigitalOcean 1vCPU/961MB droplet. The host CPU is " + arch + " (Apple M3-class when GOARCH=arm64), not a droplet x86 vCPU. Report the measured proxy number; the exact droplet number needs running on the droplet and is proportionally lower."
	}
	return heartbeatIngestHostClass{
		Label:             "droplet-class PROXY",
		Claimed:           "1 vCPU / 961 MB DigitalOcean-class host",
		Actual:            fmt.Sprintf("docker --cpus=1 --memory=961m on %s/%s (%d host logical CPUs)", runtime.GOOS, arch, runtime.NumCPU()),
		Architecture:      arch,
		IsDropletHardware: false,
		Caveat:            note,
	}
}

type pgStatSnap struct {
	XactCommit  int64
	TupUpdated  int64
	TupInserted int64
}

func pgStatDatabase(ctx context.Context, pool *pgxpool.Pool) pgStatSnap {
	var s pgStatSnap
	_ = pool.QueryRow(ctx, `
		SELECT xact_commit, tup_updated, tup_inserted
		  FROM pg_stat_database WHERE datname = current_database()`).
		Scan(&s.XactCommit, &s.TupUpdated, &s.TupInserted)
	return s
}

func probeRelationFootprint(ctx context.Context, pool *pgxpool.Pool, fleet int) heartbeatIngestFootprint {
	var dbBytes, offers, samples, workers int64
	_ = pool.QueryRow(ctx, `SELECT pg_database_size(current_database())`).Scan(&dbBytes)
	_ = pool.QueryRow(ctx, `SELECT pg_total_relation_size('realtime_worker_offers')`).Scan(&offers)
	_ = pool.QueryRow(ctx, `SELECT pg_total_relation_size('realtime_offer_samples')`).Scan(&samples)
	_ = pool.QueryRow(ctx, `SELECT pg_total_relation_size('workers')`).Scan(&workers)
	var live int64
	_ = pool.QueryRow(ctx, `
		SELECT count(*) FROM realtime_worker_offers
		 WHERE status='ACTIVE' AND last_seen_at > now() - interval '45 seconds'`).Scan(&live)
	out := heartbeatIngestFootprint{
		DatabaseBytes:    dbBytes,
		OffersBytes:      offers,
		SamplesBytes:     samples,
		WorkersBytes:     workers,
		LiveDevices:      live,
		SeededDevices:    int64(fleet),
		PostgresRSSBytes: dockerMemBytes(),
	}
	if fleet > 0 {
		out.BytesPerDevice = float64(offers+workers) / float64(fleet)
		if out.BytesPerDevice > 0 {
			out.DevicesPerGB = (1 << 30) / out.BytesPerDevice
		}
	}
	if live > 0 {
		out.BytesPerLiveDevice = float64(offers+workers) / float64(live)
	}
	return out
}

func sampleSelectorUnderLoad(ctx context.Context, pool *pgxpool.Pool, profile VLLMRuntimeProfile) (eligMS, probeMS float64, err error) {
	// Production liveness-bound selector prefix: unbounded eligibility COUNT
	// (historical full-book scan; shows superlinear/IO) and the LIMIT-2 branch
	// probe authorize actually runs. Not full AuthorizeRealtimeContract — that
	// would consume capacity and money-path locks; these are the statements
	// whose cost grows with the live book.
	qctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	start := time.Now()
	var n int
	if err = pool.QueryRow(qctx, realtimeOfferBookUnboundedCountSQL,
		profile.RuntimeProfileID, profile.ProfileSHA256,
		profile.BuyerInputUSDPerMillionTokens, profile.BuyerOutputUSDPerMillionTokens,
	).Scan(&n); err != nil {
		return 0, 0, err
	}
	eligMS = float64(time.Since(start).Microseconds()) / 1000.0
	start = time.Now()
	if err = pool.QueryRow(qctx, realtimeOfferBookBranchProbeSQL,
		profile.RuntimeProfileID, profile.ProfileSHA256,
		profile.BuyerInputUSDPerMillionTokens, profile.BuyerOutputUSDPerMillionTokens,
	).Scan(&n); err != nil {
		return eligMS, 0, err
	}
	probeMS = float64(time.Since(start).Microseconds()) / 1000.0
	return eligMS, probeMS, nil
}

func dockerMemBytes() int64 {
	name := strings.TrimSpace(os.Getenv("MERC_HEARTBEAT_INGEST_DOCKER_CONTAINER"))
	if name == "" {
		return 0
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	raw, err := exec.CommandContext(ctx, "docker", "stats", "--no-stream", "--format", "{{.MemUsage}}", name).Output()
	if err != nil {
		return 0
	}
	return parseDockerMemUsed(strings.TrimSpace(string(raw)))
}

func parseDockerMemUsed(s string) int64 {
	// "88.16MiB / 961MiB" or "1.2GiB / 961MiB"
	used := strings.TrimSpace(strings.Split(s, "/")[0])
	return parseByteSize(used)
}

func measurePostgresRestart(t *testing.T, container string, pool *pgxpool.Pool) *heartbeatIngestRestart {
	t.Helper()
	out := &heartbeatIngestRestart{
		Method: "CHECKPOINT then docker kill -s KILL + docker start (loaded cluster)",
		Note:   "crash recovery of the bench database still holding the seeded fleet + samples; PROXY container",
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	_, _ = pool.Exec(ctx, `CHECKPOINT`)
	var dbBytes int64
	_ = pool.QueryRow(ctx, `SELECT pg_database_size(current_database())`).Scan(&dbBytes)
	cancel()

	// SIGKILL the container, then start it. pg_isready is the ready signal.
	if err := exec.Command("docker", "kill", "-s", "KILL", container).Run(); err != nil {
		out.Note += "; docker kill failed: " + err.Error()
		return out
	}
	start := time.Now()
	if err := exec.Command("docker", "start", container).Run(); err != nil {
		out.Note += "; docker start failed: " + err.Error()
		return out
	}
	deadline := time.Now().Add(2 * time.Minute)
	for time.Now().Before(deadline) {
		if exec.Command("docker", "exec", container, "pg_isready", "-U", "cx", "-d", "cx").Run() == nil {
			out.CrashRecoveryMS = float64(time.Since(start).Microseconds()) / 1000.0
			out.Reached = true
			out.Note += fmt.Sprintf("; db_bytes_at_kill=%d", dbBytes)
			return out
		}
		time.Sleep(50 * time.Millisecond)
	}
	out.Note += "; did not become ready within 2m"
	return out
}

func parseByteSize(s string) int64 {
	s = strings.TrimSpace(s)
	if s == "" {
		return 0
	}
	var scale float64 = 1
	upper := strings.ToUpper(s)
	switch {
	case strings.HasSuffix(upper, "GIB"):
		scale = 1 << 30
		s = s[:len(s)-3]
	case strings.HasSuffix(upper, "GB"):
		scale = 1e9
		s = s[:len(s)-2]
	case strings.HasSuffix(upper, "MIB"):
		scale = 1 << 20
		s = s[:len(s)-3]
	case strings.HasSuffix(upper, "MB"):
		scale = 1e6
		s = s[:len(s)-2]
	case strings.HasSuffix(upper, "KIB"):
		scale = 1 << 10
		s = s[:len(s)-3]
	case strings.HasSuffix(upper, "KB"):
		scale = 1e3
		s = s[:len(s)-2]
	case strings.HasSuffix(upper, "B"):
		s = s[:len(s)-1]
	}
	s = strings.TrimSpace(s)
	f, err := strconv.ParseFloat(s, 64)
	if err != nil {
		return 0
	}
	return int64(f * scale)
}

// --- stats / IO -------------------------------------------------------------

func percentileMS(samples []float64) heartbeatIngestLatency {
	if len(samples) == 0 {
		return heartbeatIngestLatency{}
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
	return heartbeatIngestLatency{
		N:   len(sorted),
		P50: pct(0.50),
		P95: pct(0.95),
		P99: pct(0.99),
		Max: sorted[len(sorted)-1],
		Avg: sum / float64(len(sorted)),
	}
}

func parseIntListEnv(t *testing.T, env string, def []int) []int {
	t.Helper()
	raw := strings.TrimSpace(os.Getenv(env))
	if raw == "" {
		return def
	}
	var out []int
	for _, f := range strings.Split(raw, ",") {
		f = strings.TrimSpace(f)
		if f == "" {
			continue
		}
		n, err := strconv.Atoi(f)
		if err != nil || n <= 0 {
			t.Fatalf("%s: %q is not a positive integer", env, f)
		}
		out = append(out, n)
	}
	if len(out) == 0 {
		return def
	}
	return out
}

func openHeartbeatIngestStore(t *testing.T, maxConns int32) (*Store, *pgxpool.Pool) {
	t.Helper()
	// Prefer the isolated URL with-isolated-test-db.sh injects; fall back to
	// openSelectorScaleStore which creates its own database.
	return openSelectorScaleStore(t, maxConns)
}

func writeHeartbeatIngestEvidence(report heartbeatIngestReport) error {
	rel := heartbeatIngestEvidenceRel
	if v := strings.TrimSpace(os.Getenv("MERC_HEARTBEAT_INGEST_EVIDENCE")); v != "" {
		rel = v
	}
	path := filepath.Join("..", "..", rel)
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

func buildHeartbeatIngestVerdict(report heartbeatIngestReport) heartbeatIngestVerdict {
	v := heartbeatIngestVerdict{}
	base, hasBase := report.Modes["baseline"]
	bat, hasBat := report.Modes["batched"]
	if tight, ok := report.Modes["batched_tight"]; ok {
		if !hasBat || tight.BestSustainedHeartbeatsPerSec > bat.BestSustainedHeartbeatsPerSec {
			bat = tight
			hasBat = true
		}
	}
	window := float64(int(realtimeOfferLivenessWindow / time.Second))

	if hasBase {
		v.BaselineHeartbeatsPerSec = base.BestSustainedHeartbeatsPerSec
		v.BaselineImpliedFleet = base.ImpliedFleetCeiling
		v.BaselineSaturatingResource = base.SaturatingResourceAtBest
	}
	if hasBat {
		v.BatchedHeartbeatsPerSec = bat.BestSustainedHeartbeatsPerSec
		v.BatchedImpliedFleet = bat.ImpliedFleetCeiling
		v.BatchedSaturatingResource = bat.SaturatingResourceAtBest
		v.FlushInterval = bat.FlushInterval
		v.FlushIntervalWhySafe = bat.FlushIntervalWhySafe
	}

	// Can 1M devices live on one vCPU? Compare implied fleet to 1e6.
	const target = 1_000_000.0
	ceiling := v.BaselineImpliedFleet
	if hasBat && v.BatchedImpliedFleet > ceiling {
		ceiling = v.BatchedImpliedFleet
	}
	v.CanHoldOneMillionOnOneVCPU = ceiling >= target
	const tenM = 10_000_000.0
	if ceiling > 0 {
		v.TenMillionHostLowerBound = math.Ceil(tenM / ceiling)
	}
	v.TenMillionBreaksWhen = "working set exceeds RAM (bytes/device * N > host RAM after postgres+control+caddy+minio), WAL/fsync saturates the single vCPU, or the selector eligibility scan goes superlinear on the live book. This PROXY number is an M3-core UPPER bound; a droplet x86 vCPU is proportionally lower."
	if hasBase && !hasBat {
		v.OneSentence = fmt.Sprintf(
			"PROXY (M3, not droplet x86): one-statement-per-device path sustains ~%.0f heartbeats/s (saturating on %s), implying ~%.0f devices under a 45s window — %s hold 1M; 10M needs ≥%.0f such hosts or a bigger box.",
			v.BaselineHeartbeatsPerSec, v.BaselineSaturatingResource, v.BaselineImpliedFleet,
			yn(v.BaselineImpliedFleet >= target), v.TenMillionHostLowerBound)
	} else if hasBase && hasBat {
		v.OneSentence = fmt.Sprintf(
			"PROXY (M3 ARM, not a DO droplet x86 vCPU): a 1vCPU/961MB-class host holds ~%.0f devices alive on the one-statement path (%.0f hb/s, saturating on %s) and ~%.0f after coalesced liveness (%.0f hb/s, saturating on %s); 10M is ≥%.0f such hosts / a bigger box / managed PG because the single-vCPU WAL+selector working set is the measured bind.",
			v.BaselineImpliedFleet, v.BaselineHeartbeatsPerSec, v.BaselineSaturatingResource,
			v.BatchedImpliedFleet, v.BatchedHeartbeatsPerSec, v.BatchedSaturatingResource,
			v.TenMillionHostLowerBound)
	} else if hasBat {
		v.OneSentence = fmt.Sprintf(
			"PROXY (M3, not droplet x86): coalesced liveness ~%.0f hb/s (saturating on %s) ⇒ ~%.0f-device ceiling under a 45s window; 10M needs ≥%.0f such hosts.",
			v.BatchedHeartbeatsPerSec, v.BatchedSaturatingResource, v.BatchedImpliedFleet,
			v.TenMillionHostLowerBound)
	} else {
		v.OneSentence = "No quotable mode completed; refuse rather than invent a fleet ceiling."
	}
	_ = window
	return v
}

func yn(ok bool) string {
	if ok {
		return "CAN"
	}
	return "CANNOT"
}

func ynHold(ok bool) string {
	if ok {
		return "within reach on this host class"
	}
	return "still out of reach on this host class"
}

// --- report types -----------------------------------------------------------

type heartbeatIngestReport struct {
	SchemaVersion     int                                  `json:"schema_version"`
	GeneratedAt       string                               `json:"generated_at"`
	FinishedAt        string                               `json:"finished_at,omitempty"`
	WallClockSeconds  float64                              `json:"wall_clock_seconds,omitempty"`
	SourceCommit      string                               `json:"source_commit"`
	Seed              int64                                `json:"seed"`
	Host              string                               `json:"host"`
	NumCPU            int                                  `json:"num_cpu"`
	LoadAverageBefore []float64                            `json:"load_average_before,omitempty"`
	LoadAverageAfter  []float64                            `json:"load_average_after,omitempty"`
	Invocation        heartbeatIngestInvocation            `json:"invocation"`
	Postgres          heartbeatIngestPostgres              `json:"postgres"`
	HostClass         heartbeatIngestHostClass             `json:"host_class"`
	Honesty           heartbeatIngestHonesty               `json:"honesty"`
	Modes             map[string]heartbeatIngestModeResult `json:"modes"`
	Verdict           heartbeatIngestVerdict               `json:"verdict"`
	Restart           *heartbeatIngestRestart              `json:"restart,omitempty"`
}

type heartbeatIngestInvocation struct {
	EnvGate                string `json:"env_gate"`
	Command                string `json:"command"`
	ExcludedFromNormalGate bool   `json:"excluded_from_normal_gate"`
	ExclusionProof         string `json:"exclusion_proof"`
	Mode                   string `json:"mode"`
	DurationSec            int    `json:"duration_sec"`
	MaxConns               int    `json:"max_conns"`
	FleetLadder            []int  `json:"fleet_ladder"`
	ConcurrencyLadder      []int  `json:"concurrency_ladder"`
}

type heartbeatIngestPostgres struct {
	ServerVersion     string `json:"server_version"`
	SharedBuffers     string `json:"shared_buffers"`
	MaxConnections    int    `json:"max_connections"`
	Fsync             string `json:"fsync,omitempty"`
	SynchronousCommit string `json:"synchronous_commit,omitempty"`
	HostNote          string `json:"host_note,omitempty"`
	ContainerName     string `json:"container_name,omitempty"`
	CgroupMemoryBytes int64  `json:"cgroup_memory_bytes,omitempty"`
	CgroupCPU         int    `json:"cgroup_cpu,omitempty"`
	ConfigSource      string `json:"config_source,omitempty"`
}

type heartbeatIngestHostClass struct {
	Label             string `json:"label"`
	Claimed           string `json:"claimed"`
	Actual            string `json:"actual"`
	Architecture      string `json:"architecture"`
	IsDropletHardware bool   `json:"is_droplet_hardware"`
	Caveat            string `json:"caveat"`
}

type heartbeatIngestFootprint struct {
	DatabaseBytes      int64   `json:"database_bytes"`
	OffersBytes        int64   `json:"offers_bytes"`
	SamplesBytes       int64   `json:"samples_bytes"`
	WorkersBytes       int64   `json:"workers_bytes"`
	LiveDevices        int64   `json:"live_devices"`
	SeededDevices      int64   `json:"seeded_devices"`
	BytesPerDevice     float64 `json:"bytes_per_device"`
	BytesPerLiveDevice float64 `json:"bytes_per_live_device,omitempty"`
	DevicesPerGB       float64 `json:"devices_per_gb"`
	PostgresRSSBytes   int64   `json:"postgres_rss_bytes,omitempty"`
}

type heartbeatIngestRestart struct {
	CleanRestartMS  float64 `json:"clean_restart_ms,omitempty"`
	CrashRecoveryMS float64 `json:"crash_recovery_ms,omitempty"`
	Method          string  `json:"method,omitempty"`
	Note            string  `json:"note,omitempty"`
	Reached         bool    `json:"reached"`
}

type heartbeatIngestHonesty struct {
	WhatThisProves        string   `json:"what_this_proves"`
	WhatThisDoesNotProve  string   `json:"what_this_does_not_prove"`
	Guards                []string `json:"guards"`
	LivenessWindowSeconds int      `json:"liveness_window_seconds"`
	AgentHeartbeatSeconds int      `json:"agent_heartbeat_seconds"`
	ImpliedFleetFormula   string   `json:"implied_fleet_formula"`
	ProxyCaveat           string   `json:"proxy_caveat,omitempty"`
}

type heartbeatIngestModeResult struct {
	Mode                          string                   `json:"mode"`
	BatchEnabled                  bool                     `json:"batch_enabled"`
	MaxBatch                      int                      `json:"max_batch,omitempty"`
	FlushInterval                 string                   `json:"flush_interval,omitempty"`
	FlushIntervalWhySafe          string                   `json:"flush_interval_why_safe,omitempty"`
	SeedWallSeconds               float64                  `json:"seed_wall_seconds"`
	SeededFleet                   int                      `json:"seeded_fleet,omitempty"`
	FootprintAfterSeed            heartbeatIngestFootprint `json:"footprint_after_seed"`
	PathWarmupMS                  float64                  `json:"path_warmup_ms"`
	Cells                         []heartbeatIngestCell    `json:"cells"`
	Refusals                      []heartbeatIngestRefusal `json:"refusals,omitempty"`
	BestSustainedHeartbeatsPerSec float64                  `json:"best_sustained_heartbeats_per_sec,omitempty"`
	BestCell                      string                   `json:"best_cell,omitempty"`
	SaturatingResourceAtBest      string                   `json:"saturating_resource_at_best,omitempty"`
	ImpliedFleetCeiling           float64                  `json:"implied_fleet_ceiling,omitempty"`
	WALBytesPerHeartbeatAtBest    float64                  `json:"wal_bytes_per_heartbeat_at_best,omitempty"`
}

type heartbeatIngestCell struct {
	Mode                   string                   `json:"mode"`
	Fleet                  int                      `json:"fleet"`
	Concurrency            int                      `json:"concurrency"`
	DurationSec            int                      `json:"duration_sec"`
	MaxConns               int                      `json:"max_conns"`
	Successes              int64                    `json:"successes"`
	Failures               int64                    `json:"failures"`
	FirstError             string                   `json:"first_error,omitempty"`
	HeartbeatsPerSec       float64                  `json:"heartbeats_per_sec"`
	AttemptsPerSec         float64                  `json:"attempts_per_sec"`
	LatencyMS              heartbeatIngestLatency   `json:"latency_ms"`
	WarmupMS               []float64                `json:"warmup_ms"`
	WarmupFailures         int                      `json:"warmup_failures"`
	WALBytes               int64                    `json:"wal_bytes"`
	WALBytesPerHeartbeat   float64                  `json:"wal_bytes_per_heartbeat"`
	WALBytesPerSec         float64                  `json:"wal_bytes_per_sec"`
	RelationBytesDelta     int64                    `json:"relation_bytes_delta"`
	LoadAverageBefore      []float64                `json:"load_average_before,omitempty"`
	LoadAverageAfter       []float64                `json:"load_average_after,omitempty"`
	PoolAcquiredBefore     map[string]int32         `json:"pool_before"`
	PoolAcquiredAfter      map[string]int32         `json:"pool_after"`
	SaturatingResource     string                   `json:"saturating_resource"`
	ImpliedFleetCeiling    float64                  `json:"implied_fleet_ceiling"`
	UnstablePopulation     bool                     `json:"unstable_population"`
	Refusal                *heartbeatIngestRefusal  `json:"refusal,omitempty"`
	XactCommitDelta        int64                    `json:"xact_commit_delta,omitempty"`
	TupUpdatedDelta        int64                    `json:"tup_updated_delta,omitempty"`
	TupInsertedDelta       int64                    `json:"tup_inserted_delta,omitempty"`
	XactPerSec             float64                  `json:"xact_per_sec,omitempty"`
	StatementsPerSec       float64                  `json:"statements_per_sec,omitempty"`
	StatementsPerHeartbeat float64                  `json:"statements_per_heartbeat,omitempty"`
	CoalescerFlushes       int64                    `json:"coalescer_flushes,omitempty"`
	CoalescerItems         int64                    `json:"coalescer_items,omitempty"`
	SelectorEligibilityMS  heartbeatIngestLatency   `json:"selector_eligibility_ms"`
	SelectorBranchProbeMS  heartbeatIngestLatency   `json:"selector_branch_probe_ms"`
	SelectorSampleFailures int64                    `json:"selector_sample_failures,omitempty"`
	Footprint              heartbeatIngestFootprint `json:"footprint"`
	PostgresRSSBytesBefore int64                    `json:"postgres_rss_bytes_before,omitempty"`
	PostgresRSSBytesAfter  int64                    `json:"postgres_rss_bytes_after,omitempty"`
}

type heartbeatIngestLatency struct {
	N   int     `json:"n"`
	P50 float64 `json:"p50"`
	P95 float64 `json:"p95"`
	P99 float64 `json:"p99"`
	Max float64 `json:"max"`
	Avg float64 `json:"avg"`
}

type heartbeatIngestRefusal struct {
	Kind    string `json:"kind"`
	Summary string `json:"summary"`
}

type heartbeatIngestVerdict struct {
	BaselineHeartbeatsPerSec   float64 `json:"baseline_heartbeats_per_sec,omitempty"`
	BaselineImpliedFleet       float64 `json:"baseline_implied_fleet,omitempty"`
	BaselineSaturatingResource string  `json:"baseline_saturating_resource,omitempty"`
	BatchedHeartbeatsPerSec    float64 `json:"batched_heartbeats_per_sec,omitempty"`
	BatchedImpliedFleet        float64 `json:"batched_implied_fleet,omitempty"`
	BatchedSaturatingResource  string  `json:"batched_saturating_resource,omitempty"`
	FlushInterval              string  `json:"flush_interval,omitempty"`
	FlushIntervalWhySafe       string  `json:"flush_interval_why_safe,omitempty"`
	CanHoldOneMillionOnOneVCPU bool    `json:"can_hold_one_million_on_one_vcpu"`
	TenMillionHostLowerBound   float64 `json:"ten_million_host_lower_bound,omitempty"`
	TenMillionBreaksWhen       string  `json:"ten_million_breaks_when,omitempty"`
	OneSentence                string  `json:"one_sentence"`
}
