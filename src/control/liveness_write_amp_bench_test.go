package main

// Durable write-amplification harness for MERC_LIVENESS_INDEX_AUTHORITATIVE.
//
// Measures how many durable heartbeat UPDATEs an actively-heartbeating fleet
// actually performs with the live-index flag OFF vs ON, whether any offer's
// last_seen_at ages out of the 45s eligibility window while it is still
// heartbeating, caller latency by outcome class, and the retained heap of the
// three per-offer in-process structures on Store.
//
// Opt-in only — never part of make test / make ci:
//
//	MERC_WRITE_AMP_BENCH=1 \
//	MERC_TEST_DATABASE_URL=postgres://cx:cx@localhost:5432/cx?sslmode=disable \
//	  go test -count=1 -run '^TestLivenessWriteAmplificationBench$' -timeout 45m .
//
// Writes evidence/perf/liveness-write-amplification.json when run from src/control/.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
	"unsafe"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
)

const (
	writeAmpBenchEnv         = "MERC_WRITE_AMP_BENCH"
	writeAmpBenchFleetEnv    = "MERC_WRITE_AMP_BENCH_FLEET"
	writeAmpBenchIntervalEnv = "MERC_WRITE_AMP_BENCH_INTERVAL_SEC"
	writeAmpBenchDurationEnv = "MERC_WRITE_AMP_BENCH_DURATION_SEC"
	writeAmpBenchMaxConnsEnv = "MERC_WRITE_AMP_BENCH_MAX_CONNS"
	writeAmpBenchEvidenceRel = "evidence/perf/liveness-write-amplification.json"

	writeAmpDefaultDurationSec = 50
)

func TestLivenessWriteAmplificationBench(t *testing.T) {
	if os.Getenv(writeAmpBenchEnv) != "1" {
		t.Skip("set MERC_WRITE_AMP_BENCH=1 to measure durable heartbeat write amplification")
	}

	fleets := parseIntListEnv(t, writeAmpBenchFleetEnv, []int{100, 1_000, 10_000})
	intervalsSec := parseIntListEnv(t, writeAmpBenchIntervalEnv, []int{5, 1})
	durationSec := writeAmpDefaultDurationSec
	if v := strings.TrimSpace(os.Getenv(writeAmpBenchDurationEnv)); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 1 {
			t.Fatalf("%s=%q: need integer >= 1", writeAmpBenchDurationEnv, v)
		}
		durationSec = n
	}
	maxConns := int32(24)
	if v := strings.TrimSpace(os.Getenv(writeAmpBenchMaxConnsEnv)); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 1 {
			t.Fatalf("%s=%q: need positive integer", writeAmpBenchMaxConnsEnv, v)
		}
		maxConns = int32(n)
	}

	host, _ := os.Hostname()
	startedAt := time.Now().UTC()
	duration := time.Duration(durationSec) * time.Second
	refreshSec := int(livenessDurableRefreshInterval / time.Second)
	windowSec := int(realtimeOfferLivenessWindow / time.Second)

	report := writeAmpReport{
		Classification: "MEASURED",
		GeneratedAt:    startedAt.Format(time.RFC3339),
		SourceCommit:   mercSourceCommitSHA(),
		Host:           host,
		NumCPU:         runtime.NumCPU(),
		GOMAXPROCS:     runtime.GOMAXPROCS(0),
		GOOS:           runtime.GOOS,
		GOARCH:         runtime.GOARCH,
		Invocation: writeAmpInvocation{
			EnvGate:                writeAmpBenchEnv + "=1",
			ExcludedFromNormalGate: true,
			ExclusionProof:         "TestLivenessWriteAmplificationBench skips unless MERC_WRITE_AMP_BENCH=1; listed in ops/scripts/allowed-test-skips.txt; make test / make ci never set the env var",
			Command:                "cd src/control && MERC_WRITE_AMP_BENCH=1 MERC_TEST_DATABASE_URL=postgres://cx:cx@localhost:5432/cx?sslmode=disable go test -count=1 -run '^TestLivenessWriteAmplificationBench$' -timeout 45m .",
			FleetLadder:            append([]int(nil), fleets...),
			HeartbeatIntervalSec:   append([]int(nil), intervalsSec...),
			DurationSec:            durationSec,
			RefreshIntervalSec:     refreshSec,
			LivenessWindowSec:      windowSec,
			MaxConns:               int(maxConns),
			BatchEnabled:           true,
			MaxBatch:               defaultLivenessBatchMaxSize,
			FlushInterval:          defaultLivenessBatchFlushInterval.String(),
			TimeSimulation:         "none — HeartbeatRealtimeOffer stamps serverNow=time.Now(); the 15s refresh cannot be advanced without changing production code",
			SeedMethod:             "parallel seedOneRealtimeOffer fixture (supplier+worker INSERT, then production UpsertRealtimeOffer); not COPY",
		},
		Honesty: writeAmpHonesty{
			WhatThisProves:       "on this host, with a local PostgreSQL, how many realtime_offer_samples rows (one per matched heartbeat UPDATE) an N-offer fleet produces when every offer heartbeats every H for wall-clock D, with MERC_LIVENESS_INDEX_AUTHORITATIVE off vs on; whether any of those offers' durable last_seen_at left now()-45s while still heartbeating; caller HeartbeatRealtimeOffer latency by SUCCESS/REFUSAL/EMPTY/FAILURE; and retained HeapInuse/HeapAlloc bytes/offer of offerSlotCache, offerPersistCache, and slotOwner after GC, against LiveDeviceIndex.HotBytes()/N",
			WhatThisDoesNotProve: "this is one host with a local PostgreSQL, not a droplet-class control plane; it does not measure the selector, lease acquisition, or settlement; it does not prove a 10M-device network — the N values actually run are invocation.fleet_ladder and cells[].n; the flag remains OFF in production; it does not measure the authenticated HTTP ingest path or a multi-process control plane",
			Guards: []string{
				"no simulated elapsed time: D is wall-clock; sleeps are the agent's real cadence H, not a stand-in for the 15s refresh",
				"durable writes are SELECT count(*) FROM realtime_offer_samples deltas, not in-process intentions",
				"refused, empty, and failed calls are bucketed separately and never enter the SUCCESS latency percentiles",
				"a cell whose last_seen_at aged past 45s on a still-heartbeating offer is INVALID and is kept, not dropped",
				"a cell that cannot hold cadence H is dropped and named in dropped_cells — the ladder is not silently resized",
				"production coalescer is on (MaxBatch=1000, FlushInterval=2ms) so a large tick can finish inside H; each offer heartbeats once per tick so same-offer batch dedup cannot fire",
				"flag-ON detached flushes are drained before the sample counter and last_seen_at check",
				"memory cells populate the three Store caches directly (no N postgres rows) and compare HeapInuse/HeapAlloc after runtime.GC, matching TestLiveDeviceIndexBench",
				"12.41 B/device is LiveDeviceIndex.HotBytes()/capacity of a full index; it is not process RSS and is reported here only as contrast",
				"MERC_LIVENESS_INDEX_AUTHORITATIVE is unset in production; this harness is the only writer of this file",
			},
		},
	}

	report.Memory = measureWriteAmpMemoryLadder(t, fleets)
	for _, m := range report.Memory {
		t.Logf("memory n=%d slot_cache=%.1f B/off persist=%.1f B/off slot_owner=%.1f B/off (cap=%d) process=%.1f B/off index_hot/n=%.2f index_hot/slot=%.2f method=%s",
			m.N, m.OfferSlotCache.HeapInusePerOffer, m.OfferPersistCache.HeapInusePerOffer,
			m.SlotOwner.HeapInusePerOffer, m.SlotOwnerCapacity,
			m.ProcessTotal.HeapInusePerOffer, m.IndexHotBytesPerOffer, m.IndexHotBytesPerSlot, m.PopulateMethod)
	}

	if strings.TrimSpace(os.Getenv(isolatedTestDBTemplateEnv)) == "" {
		t.Setenv(isolatedTestDBTemplateEnv, schemaTemplateDatabaseName(canonicalSchemaSHA256()))
	}
	t.Setenv("MERC_TOKEN_KEY", "write-amp-bench-key-32-bytes-minimum!")
	installSettlementCurrencyForTest(t, "usd")

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Minute)
	t.Cleanup(cancel)
	_, pool := openIsolatedTestStoreWithMaxConns(t, maxConns)

	maxFleet := fleets[0]
	for _, n := range fleets {
		if n > maxFleet {
			maxFleet = n
		}
	}
	profile := sortedVLLMProfiles()[0]
	t.Logf("seeding fleet=%d via seedOneRealtimeOffer fixture…", maxFleet)
	seedStart := time.Now()
	workers, seedErr := seedWriteAmpFleet(t, ctx, pool, profile, maxFleet)
	report.Invocation.SeededFleet = len(workers)
	report.Invocation.SeedWallSeconds = time.Since(seedStart).Seconds()
	if seedErr != nil || len(workers) == 0 {
		report.Dropped = append(report.Dropped, writeAmpDropped{
			Reason: "seed_failed", Detail: fmt.Sprintf("%v", seedErr),
		})
		t.Logf("seed failed: %v", seedErr)
	} else {
		t.Logf("seeded %d offers in %.1fs", len(workers), report.Invocation.SeedWallSeconds)
		if len(workers) < maxFleet {
			report.Dropped = append(report.Dropped, writeAmpDropped{
				Reason: "seed_short",
				Detail: fmt.Sprintf("requested %d, seeded %d", maxFleet, len(workers)),
				N:      maxFleet,
			})
		}
		batchCfg := livenessBatchConfig{
			Enabled:       true,
			MaxBatch:      defaultLivenessBatchMaxSize,
			FlushInterval: defaultLivenessBatchFlushInterval,
		}
		for _, n := range fleets {
			if n > len(workers) {
				report.Dropped = append(report.Dropped, writeAmpDropped{
					N: n, Reason: "fleet_exceeds_seed",
					Detail: fmt.Sprintf("n=%d > seeded=%d", n, len(workers)),
				})
				continue
			}
			slice := workers[:n]
			for _, hSec := range intervalsSec {
				h := time.Duration(hSec) * time.Second
				if h < time.Second {
					report.Dropped = append(report.Dropped, writeAmpDropped{
						N: n, IntervalSec: hSec, Reason: "interval_too_small",
						Detail: "heartbeat interval must be >= 1s",
					})
					continue
				}
				t.Logf("cell n=%d H=%s D=%s flag=OFF", n, h, duration)
				off, offDrop := measureWriteAmpSide(t, ctx, pool, profile, slice, h, duration, false, batchCfg, maxConns)
				if offDrop != nil {
					offDrop.N = n
					offDrop.IntervalSec = hSec
					offDrop.Flag = "off"
					report.Dropped = append(report.Dropped, *offDrop)
					t.Logf("dropped n=%d H=%ds flag=OFF: %s (%s)", n, hSec, offDrop.Reason, offDrop.Detail)
					continue
				}
				t.Logf("cell n=%d H=%s D=%s flag=ON", n, h, duration)
				on, onDrop := measureWriteAmpSide(t, ctx, pool, profile, slice, h, duration, true, batchCfg, maxConns)
				if onDrop != nil {
					onDrop.N = n
					onDrop.IntervalSec = hSec
					onDrop.Flag = "on"
					report.Dropped = append(report.Dropped, *onDrop)
					t.Logf("dropped n=%d H=%ds flag=ON: %s (%s)", n, hSec, onDrop.Reason, onDrop.Detail)
					continue
				}
				cell := assembleWriteAmpCell(n, hSec, durationSec, off, on)
				report.Cells = append(report.Cells, cell)
				t.Logf("cell n=%d H=%ds submitted_off=%d submitted_on=%d durable_off=%d durable_on=%d ratio_off/on=%.3f valid=%v aged_off=%d aged_on=%d succ_p50_off=%.3fms succ_p50_on=%.3fms",
					n, hSec, cell.HeartbeatsSubmittedOff, cell.HeartbeatsSubmittedOn,
					cell.DurableWritesOff, cell.DurableWritesOn, cell.RatioOffOverOn,
					cell.Valid, cell.SafetyOff.AgedOffers, cell.SafetyOn.AgedOffers,
					cell.LatencyMSOff.SUCCESS.P50, cell.LatencyMSOn.SUCCESS.P50)
			}
		}
	}

	report.Surprises = detectWriteAmpSurprises(report)
	report.FinishedAt = time.Now().UTC().Format(time.RFC3339)
	report.WallClockSeconds = time.Since(startedAt).Seconds()
	if err := writeWriteAmpEvidence(report); err != nil {
		t.Fatalf("write evidence: %v", err)
	}
	t.Logf("wrote %s classification=%s cells=%d dropped=%d surprises=%d wall=%.1fs",
		writeAmpBenchEvidenceRel, report.Classification, len(report.Cells), len(report.Dropped), len(report.Surprises), report.WallClockSeconds)
}

func measureWriteAmpSide(
	t *testing.T, ctx context.Context, pool *pgxpool.Pool, profile VLLMRuntimeProfile,
	workers []WorkerAuth, interval, duration time.Duration, authoritative bool,
	batchCfg livenessBatchConfig, maxConns int32,
) (writeAmpSide, *writeAmpDropped) {
	t.Helper()
	n := len(workers)
	side := writeAmpSide{Authoritative: authoritative, N: n}

	if authoritative {
		t.Setenv("MERC_LIVENESS_INDEX_AUTHORITATIVE", "1")
	} else {
		t.Setenv("MERC_LIVENESS_INDEX_AUTHORITATIVE", "0")
	}

	store := NewStore(pool)
	store.SetLivenessBatchConfigForTest(batchCfg)
	preloadStart := time.Now()
	store.ensureLiveDeviceIndex()
	side.PreloadMS = float64(time.Since(preloadStart).Microseconds()) / 1000.0

	ids := make([]uuid.UUID, n)
	for i, w := range workers {
		ids[i] = w.WorkerID
	}
	// Seed may have taken minutes; pin last_seen_at so the 45s window measures
	// the heartbeating period, not fixture setup. This UPDATE is not a heartbeat
	// and does not insert realtime_offer_samples.
	if _, err := pool.Exec(ctx, `
		UPDATE realtime_worker_offers SET last_seen_at=now()
		 WHERE worker_id = ANY($1::uuid[])`, ids); err != nil {
		return side, &writeAmpDropped{Reason: "reset_last_seen_failed", Detail: err.Error()}
	}

	samplesBefore, err := countAllOfferSamples(ctx, pool)
	if err != nil {
		return side, &writeAmpDropped{Reason: "sample_count_failed", Detail: err.Error()}
	}
	xactBefore := pgXactCommit(ctx, pool)
	flushBefore, itemsBefore := store.LivenessFlushStats()

	ticks := int(duration/interval) + 1
	conc := writeAmpConcurrency(n, int(maxConns))
	side.Workers = conc
	side.TicksPlanned = ticks

	start := time.Now()
	var (
		successMS []float64
		refusalMS []float64
		failureMS []float64
		emptyMS   []float64
		tickMS    []float64
	)
	var agedPeak int64
	var oldestPeak float64
	hb := RealtimeOfferHeartbeat{
		RuntimeProfileID:   profile.RuntimeProfileID,
		Warmth:             "HOT",
		AvailableSequences: 8,
		Status:             "ACTIVE",
	}

	for k := 0; k < ticks; k++ {
		target := start.Add(time.Duration(k) * interval)
		if now := time.Now(); now.Before(target) {
			time.Sleep(target.Sub(now))
		}
		if k > 0 && time.Since(target) > interval {
			side.MissedBeats++
		}
		tickStart := time.Now()
		res := heartbeatWriteAmpTick(ctx, store, workers, hb, conc)
		if authoritative {
			waitWriteAmpDurable(store, 15*time.Second)
		}
		tickMS = append(tickMS, float64(time.Since(tickStart).Nanoseconds())/1e6)

		successMS = append(successMS, res.successMS...)
		refusalMS = append(refusalMS, res.refusalMS...)
		failureMS = append(failureMS, res.failureMS...)
		emptyMS = append(emptyMS, res.emptyMS...)
		side.Submitted += res.submitted
		side.Buckets.SUCCESS += res.buckets.SUCCESS
		side.Buckets.REFUSAL += res.buckets.REFUSAL
		side.Buckets.EMPTY += res.buckets.EMPTY
		side.Buckets.FAILURE += res.buckets.FAILURE
		if res.firstErr != "" && side.FirstError == "" {
			side.FirstError = res.firstErr
		}

		safety, sErr := readWriteAmpSafety(ctx, pool, ids)
		if sErr != nil {
			return side, &writeAmpDropped{Reason: "safety_query_failed", Detail: sErr.Error()}
		}
		if safety.AgedOffers > agedPeak {
			agedPeak = safety.AgedOffers
		}
		if safety.OldestAgeSec > oldestPeak {
			oldestPeak = safety.OldestAgeSec
		}
		side.Safety = safety
		if safety.AgedOffers > 0 {
			side.Invalid = true
			side.InvalidReason = fmt.Sprintf("tick=%d: %d still-heartbeating offer(s) had last_seen_at <= now()-45s (oldest_age_sec=%.3f)",
				k, safety.AgedOffers, safety.OldestAgeSec)
		}
	}

	if store.livenessCoalescer != nil {
		waitWriteAmpDurable(store, 15*time.Second)
		store.livenessCoalescer.close()
	}
	samplesAfter, err := countAllOfferSamples(ctx, pool)
	if err != nil {
		return side, &writeAmpDropped{Reason: "sample_count_failed", Detail: err.Error()}
	}
	side.SamplesBefore = samplesBefore
	side.SamplesAfter = samplesAfter
	side.DurableWrites = samplesAfter - samplesBefore
	if side.DurableWrites < 0 {
		side.DurableWrites = 0
	}
	side.XactCommitDelta = pgXactCommit(ctx, pool) - xactBefore
	if side.XactCommitDelta < 0 {
		side.XactCommitDelta = 0
	}
	flushAfter, itemsAfter := store.LivenessFlushStats()
	side.CoalescerFlushes = flushAfter - flushBefore
	side.CoalescerItems = itemsAfter - itemsBefore
	side.WallSeconds = time.Since(start).Seconds()
	side.TicksRan = ticks
	side.TickMS = percentileMSWriteAmp(tickMS)
	side.LatencyMS = writeAmpLatencySet{
		SUCCESS: percentileMSWriteAmp(successMS),
		REFUSAL: percentileMSWriteAmp(refusalMS),
		EMPTY:   percentileMSWriteAmp(emptyMS),
		FAILURE: percentileMSWriteAmp(failureMS),
	}
	side.Safety.AgedOffers = agedPeak
	if oldestPeak > side.Safety.OldestAgeSec {
		side.Safety.OldestAgeSec = oldestPeak
	}

	// Cadence honesty: if the typical tick cannot finish inside H, this is not
	// "a fleet heartbeating every H". Drop rather than publish a stretched H.
	if side.TickMS.N > 0 && side.TickMS.P95 > float64(interval.Milliseconds()) {
		return side, &writeAmpDropped{
			Reason: "cadence_unsustainable",
			Detail: fmt.Sprintf("tick p95=%.1fms > H=%s (missed_beats=%d submitted=%d durable=%d)",
				side.TickMS.P95, interval, side.MissedBeats, side.Submitted, side.DurableWrites),
		}
	}
	if side.Buckets.SUCCESS == 0 {
		return side, &writeAmpDropped{
			Reason: "no_successful_heartbeats",
			Detail: fmt.Sprintf("buckets=%+v first_error=%s", side.Buckets, side.FirstError),
		}
	}
	return side, nil
}

type tickResult struct {
	submitted int64
	successMS []float64
	refusalMS []float64
	failureMS []float64
	emptyMS   []float64
	buckets   writeAmpBuckets
	firstErr  string
}

func heartbeatWriteAmpTick(
	ctx context.Context, store *Store, workers []WorkerAuth,
	hb RealtimeOfferHeartbeat, conc int,
) tickResult {
	n := len(workers)
	if conc < 1 {
		conc = 1
	}
	if conc > n {
		conc = n
	}
	var (
		next   atomic.Int64
		okN    atomic.Int64
		refN   atomic.Int64
		failN  atomic.Int64
		emptyN atomic.Int64
		first  atomic.Value
		mu     sync.Mutex
		succ   []float64
		refu   []float64
		fail   []float64
		empty  []float64
	)
	first.Store("")
	var wg sync.WaitGroup
	for c := 0; c < conc; c++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			localS := make([]float64, 0, (n/conc)+8)
			localR := make([]float64, 0, 4)
			localF := make([]float64, 0, 4)
			localE := make([]float64, 0, 1)
			for {
				i := int(next.Add(1) - 1)
				if i >= n {
					break
				}
				start := time.Now()
				err := store.HeartbeatRealtimeOffer(ctx, workers[i], hb)
				elapsed := float64(time.Since(start).Nanoseconds()) / 1e6
				switch classifyWriteAmpCall(err) {
				case "SUCCESS":
					okN.Add(1)
					localS = append(localS, elapsed)
				case "REFUSAL":
					refN.Add(1)
					localR = append(localR, elapsed)
					if err != nil && first.Load().(string) == "" {
						first.Store(err.Error())
					}
				case "EMPTY":
					emptyN.Add(1)
					localE = append(localE, elapsed)
				default:
					failN.Add(1)
					localF = append(localF, elapsed)
					if err != nil && first.Load().(string) == "" {
						first.Store(err.Error())
					}
				}
			}
			mu.Lock()
			succ = append(succ, localS...)
			refu = append(refu, localR...)
			fail = append(fail, localF...)
			empty = append(empty, localE...)
			mu.Unlock()
		}()
	}
	wg.Wait()
	return tickResult{
		submitted: int64(n),
		successMS: succ,
		refusalMS: refu,
		failureMS: fail,
		emptyMS:   empty,
		buckets: writeAmpBuckets{
			SUCCESS: okN.Load(),
			REFUSAL: refN.Load(),
			EMPTY:   emptyN.Load(),
			FAILURE: failN.Load(),
		},
		firstErr: first.Load().(string),
	}
}

func classifyWriteAmpCall(err error) string {
	if err == nil {
		return "SUCCESS"
	}
	if errors.Is(err, errNotFound) || errors.Is(err, errStaleHeartbeatObservation) {
		return "REFUSAL"
	}
	return "FAILURE"
}

func writeAmpConcurrency(n, maxConns int) int {
	conc := runtime.GOMAXPROCS(0) * 4
	if conc < 8 {
		conc = 8
	}
	if conc > 64 {
		conc = 64
	}
	if maxConns > 0 && conc > maxConns*2 {
		conc = maxConns * 2
	}
	if conc > n {
		conc = n
	}
	if conc < 1 {
		conc = 1
	}
	return conc
}

func waitWriteAmpDurable(store *Store, timeout time.Duration) {
	c := store.livenessCoalescer
	if c == nil {
		return
	}
	deadline := time.Now().Add(timeout)
	for {
		c.mu.Lock()
		pending := len(c.pending)
		c.mu.Unlock()
		if pending == 0 && len(c.detachedQ) == 0 {
			time.Sleep(15 * time.Millisecond)
			c.mu.Lock()
			pending = len(c.pending)
			c.mu.Unlock()
			if pending == 0 && len(c.detachedQ) == 0 {
				return
			}
		}
		if !time.Now().Before(deadline) {
			return
		}
		time.Sleep(2 * time.Millisecond)
	}
}

func seedWriteAmpFleet(t *testing.T, ctx context.Context, pool *pgxpool.Pool, profile VLLMRuntimeProfile, n int) ([]WorkerAuth, error) {
	t.Helper()
	if n < 1 {
		return nil, fmt.Errorf("fleet must be >= 1")
	}
	// Same store constructor the fixture uses; batching is irrelevant for
	// registration. A dedicated store keeps Once state off the measured path.
	store := NewStore(pool)
	out := make([]WorkerAuth, n)
	var (
		mu      sync.Mutex
		first   error
		next    atomic.Int64
		workers = runtime.GOMAXPROCS(0) * 2
	)
	if workers < 4 {
		workers = 4
	}
	if workers > 16 {
		workers = 16
	}
	if workers > n {
		workers = n
	}
	var wg sync.WaitGroup
	for w := 0; w < workers; w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				i := int(next.Add(1) - 1)
				if i >= n {
					return
				}
				worker, err := seedOneRealtimeOfferErr(ctx, store, profile)
				if err != nil {
					mu.Lock()
					if first == nil {
						first = fmt.Errorf("offer %d: %w", i, err)
					}
					mu.Unlock()
					return
				}
				out[i] = worker
			}
		}()
	}
	wg.Wait()
	if first != nil {
		return nil, first
	}
	return out, nil
}

// seedOneRealtimeOfferErr is seedOneRealtimeOffer returning an error so the
// bench can seed in parallel. Same inserts, same UpsertRealtimeOffer, same
// rates — not a synthetic offer COPY.
func seedOneRealtimeOfferErr(ctx context.Context, store *Store, profile VLLMRuntimeProfile) (WorkerAuth, error) {
	supplierID := uuid.New()
	workerID := uuid.New()
	if _, err := store.pool.Exec(ctx, `
		INSERT INTO suppliers (id,email,status) VALUES ($1,$2,'active')`,
		supplierID, supplierID.String()+"@liveness.test"); err != nil {
		return WorkerAuth{}, fmt.Errorf("supplier: %w", err)
	}
	if _, err := store.pool.Exec(ctx, `
		INSERT INTO workers (id,supplier_id,hw_class,last_seen_at)
		VALUES ($1,$2,'nvidia_24gb',now())`, workerID, supplierID); err != nil {
		return WorkerAuth{}, fmt.Errorf("worker: %w", err)
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
		return WorkerAuth{}, fmt.Errorf("UpsertRealtimeOffer: %w", err)
	}
	return worker, nil
}

func countAllOfferSamples(ctx context.Context, pool *pgxpool.Pool) (int64, error) {
	var n int64
	err := pool.QueryRow(ctx, `SELECT count(*) FROM realtime_offer_samples`).Scan(&n)
	return n, err
}

func pgXactCommit(ctx context.Context, pool *pgxpool.Pool) int64 {
	var n int64
	_ = pool.QueryRow(ctx, `SELECT xact_commit FROM pg_stat_database WHERE datname = current_database()`).Scan(&n)
	return n
}

func readWriteAmpSafety(ctx context.Context, pool *pgxpool.Pool, ids []uuid.UUID) (writeAmpSafety, error) {
	var s writeAmpSafety
	s.CheckedOffers = int64(len(ids))
	err := pool.QueryRow(ctx, `
		SELECT
		  count(*) FILTER (WHERE last_seen_at <= now() - interval '45 seconds'),
		  COALESCE(EXTRACT(EPOCH FROM now() - min(last_seen_at)), 0),
		  COALESCE(EXTRACT(EPOCH FROM now() - max(last_seen_at)), 0)
		 FROM realtime_worker_offers
		 WHERE worker_id = ANY($1::uuid[])`, ids).Scan(&s.AgedOffers, &s.OldestAgeSec, &s.NewestAgeSec)
	if err != nil {
		return s, err
	}
	s.InsideWindow = s.AgedOffers == 0
	return s, nil
}

func assembleWriteAmpCell(n, intervalSec, durationSec int, off, on writeAmpSide) writeAmpCell {
	cell := writeAmpCell{
		Classification:         "MEASURED",
		N:                      n,
		HeartbeatIntervalSec:   intervalSec,
		DurationSec:            durationSec,
		Ticks:                  off.TicksRan,
		Valid:                  !off.Invalid && !on.Invalid,
		HeartbeatsSubmittedOff: off.Submitted,
		HeartbeatsSubmittedOn:  on.Submitted,
		DurableWritesOff:       off.DurableWrites,
		DurableWritesOn:        on.DurableWrites,
		BucketsOff:             off.Buckets,
		BucketsOn:              on.Buckets,
		LatencyMSOff:           off.LatencyMS,
		LatencyMSOn:            on.LatencyMS,
		SafetyOff:              off.Safety,
		SafetyOn:               on.Safety,
		TickMSOff:              off.TickMS,
		TickMSOn:               on.TickMS,
		XactCommitDeltaOff:     off.XactCommitDelta,
		XactCommitDeltaOn:      on.XactCommitDelta,
		CoalescerFlushesOff:    off.CoalescerFlushes,
		CoalescerFlushesOn:     on.CoalescerFlushes,
		CoalescerItemsOff:      off.CoalescerItems,
		CoalescerItemsOn:       on.CoalescerItems,
		PreloadMSOff:           off.PreloadMS,
		PreloadMSOn:            on.PreloadMS,
		WallSecondsOff:         off.WallSeconds,
		WallSecondsOn:          on.WallSeconds,
		MissedBeatsOff:         off.MissedBeats,
		MissedBeatsOn:          on.MissedBeats,
		FirstErrorOff:          off.FirstError,
		FirstErrorOn:           on.FirstError,
	}
	if off.DurableWrites > 0 {
		cell.RatioOnOverOff = float64(on.DurableWrites) / float64(off.DurableWrites)
	}
	if on.DurableWrites > 0 {
		cell.RatioOffOverOn = float64(off.DurableWrites) / float64(on.DurableWrites)
	}
	switch {
	case off.Invalid && on.Invalid:
		cell.InvalidReason = "flag_off: " + off.InvalidReason + "; flag_on: " + on.InvalidReason
	case off.Invalid:
		cell.InvalidReason = "flag_off: " + off.InvalidReason
	case on.Invalid:
		cell.InvalidReason = "flag_on: " + on.InvalidReason
	}
	return cell
}

func detectWriteAmpSurprises(report writeAmpReport) []string {
	var out []string
	for _, m := range report.Memory {
		if m.N <= 0 {
			continue
		}
		if m.IndexHotBytesPerSlot > 0 && m.ProcessTotal.HeapInusePerOffer > 10*m.IndexHotBytesPerSlot {
			out = append(out, fmt.Sprintf(
				"n=%d: process caches are %.1f B/offer vs index HotBytes/slot %.2f — the 12.41 B/device headline is not representative of the process footprint",
				m.N, m.ProcessTotal.HeapInusePerOffer, m.IndexHotBytesPerSlot))
		}
		if m.OfferSlotCache.FallbackUsed || m.OfferPersistCache.FallbackUsed || m.SlotOwner.FallbackUsed || m.ProcessTotal.FallbackUsed {
			out = append(out, fmt.Sprintf("n=%d: HeapInuse delta went negative on at least one structure; used after.HeapInuse fallback (noisy at small N)", m.N))
		}
		if m.SlotOwnerCapacity > uint32(m.N) {
			out = append(out, fmt.Sprintf(
				"n=%d: slotOwner is allocated to liveIndexCapacity=%d (min 1024 + 65536 headroom), not to N; structural 8 B/slot becomes %.1f B/offer",
				m.N, m.SlotOwnerCapacity, 8*float64(m.SlotOwnerCapacity)/float64(m.N)))
		}
	}
	for _, c := range report.Cells {
		if !c.Valid {
			out = append(out, fmt.Sprintf("n=%d H=%ds: INVALID — %s", c.N, c.HeartbeatIntervalSec, c.InvalidReason))
		}
		if c.DurableWritesOn > c.DurableWritesOff && c.DurableWritesOff > 0 {
			out = append(out, fmt.Sprintf("n=%d H=%ds: flag ON wrote MORE durable samples than flag OFF (%d > %d)",
				c.N, c.HeartbeatIntervalSec, c.DurableWritesOn, c.DurableWritesOff))
		}
		if c.LatencyMSOn.SUCCESS.N > 0 && c.LatencyMSOff.SUCCESS.N > 0 &&
			c.LatencyMSOn.SUCCESS.P50 > c.LatencyMSOff.SUCCESS.P50 {
			out = append(out, fmt.Sprintf("n=%d H=%ds: flag ON SUCCESS p50 (%.3fms) was slower than flag OFF (%.3fms)",
				c.N, c.HeartbeatIntervalSec, c.LatencyMSOn.SUCCESS.P50, c.LatencyMSOff.SUCCESS.P50))
		}
		if c.BucketsOff.REFUSAL+c.BucketsOn.REFUSAL+c.BucketsOff.FAILURE+c.BucketsOn.FAILURE+c.BucketsOff.EMPTY+c.BucketsOn.EMPTY > 0 {
			out = append(out, fmt.Sprintf("n=%d H=%ds: non-success buckets off=%+v on=%+v",
				c.N, c.HeartbeatIntervalSec, c.BucketsOff, c.BucketsOn))
		}
		if c.XactCommitDeltaOff > 0 && c.DurableWritesOff > 0 &&
			c.XactCommitDeltaOff*2 < c.DurableWritesOff {
			out = append(out, fmt.Sprintf(
				"n=%d H=%ds flag OFF: xact_commit_delta=%d << durable_writes=%d — coalescer folded many UPDATEs per transaction (expected with batching)",
				c.N, c.HeartbeatIntervalSec, c.XactCommitDeltaOff, c.DurableWritesOff))
		}
	}
	if len(report.Dropped) > 0 {
		out = append(out, fmt.Sprintf("%d cell(s) dropped (see dropped_cells); ladder actually published is cells[].n", len(report.Dropped)))
	}
	return out
}

// --- memory -----------------------------------------------------------------

func measureWriteAmpMemoryLadder(t *testing.T, fleets []int) []writeAmpMemoryCell {
	t.Helper()
	out := make([]writeAmpMemoryCell, 0, len(fleets))
	profile := "vllm.llama-3.1-8b.tp1" // interned; production keys share few profile strings
	if ids := sortedVLLMProfiles(); len(ids) > 0 {
		profile = ids[0].RuntimeProfileID
	}
	now := time.Now().UTC()
	for _, n := range fleets {
		cell := writeAmpMemoryCell{
			N:              n,
			PopulateMethod: "direct Store cache population (no postgres rows); keys use uuid.New() plus one interned runtime_profile_id",
			TypeSizeof: writeAmpTypeSizeof{
				OfferSlotKeyBytes:      int(unsafe.Sizeof(offerSlotKey{})),
				OfferBindingBytes:      int(unsafe.Sizeof(offerBinding{})),
				OfferPersistStateBytes: int(unsafe.Sizeof(offerPersistState{})),
				SlotOwnerElemBytes:     int(unsafe.Sizeof(uint64(0))),
			},
		}
		capN := liveIndexCapacity(uint32(n), uint32(n))
		cell.SlotOwnerCapacity = capN

		slotCache := measureHeapDelta(n, func() any {
			s := &Store{}
			for i := 0; i < n; i++ {
				wid := uuid.New()
				s.offerSlotCache.Store(offerSlotKey{worker: wid, profile: profile},
					offerBinding{slot: uint32(i), supplier: wid, maxActive: 16})
			}
			return s
		})
		cell.OfferSlotCache = slotCache.writeAmpHeap
		persist := measureHeapDelta(n, func() any {
			s := &Store{}
			for i := 0; i < n; i++ {
				wid := uuid.New()
				s.offerPersistCache.Store(offerSlotKey{worker: wid, profile: profile},
					offerPersistState{warmth: "HOT", status: "ACTIVE", available: 8, at: now})
			}
			return s
		})
		cell.OfferPersistCache = persist.writeAmpHeap
		owner := measureHeapDelta(n, func() any {
			s := &Store{}
			s.slotOwner = make([]uint64, capN)
			for i := 0; i < n; i++ {
				wid := uuid.New()
				s.claimSlotOwner(uint32(i), offerFingerprint(wid, profile))
			}
			return s
		})
		cell.SlotOwner = owner.writeAmpHeap
		total := measureHeapDelta(n, func() any {
			s := &Store{}
			s.slotOwner = make([]uint64, capN)
			for i := 0; i < n; i++ {
				wid := uuid.New()
				key := offerSlotKey{worker: wid, profile: profile}
				s.offerSlotCache.Store(key, offerBinding{slot: uint32(i), supplier: wid, maxActive: 16})
				s.offerPersistCache.Store(key, offerPersistState{warmth: "HOT", status: "ACTIVE", available: 8, at: now})
				s.claimSlotOwner(uint32(i), offerFingerprint(wid, profile))
			}
			return s
		})
		cell.ProcessTotal = total.writeAmpHeap

		idxHeld := measureHeapDelta(n, func() any {
			idx := NewLiveDeviceIndex(capN)
			epoch := uint32(time.Now().Unix())
			if epoch < 1_000_000 {
				epoch = 1_700_000_000
			}
			for i := 0; i < n; i++ {
				_ = idx.Heartbeat(uint32(i), epoch, epoch)
			}
			return idx
		})
		cell.IndexHeap = idxHeld.writeAmpHeap
		if idx, _ := idxHeld.held.(*LiveDeviceIndex); idx != nil {
			cell.IndexHotBytes = idx.HotBytes()
			if n > 0 {
				cell.IndexHotBytesPerOffer = float64(cell.IndexHotBytes) / float64(n)
			}
			if slots := idx.Slots(); slots > 0 {
				cell.IndexHotBytesPerSlot = float64(cell.IndexHotBytes) / float64(slots)
			}
		}
		cell.Headline12_41Representative = cell.ProcessTotal.HeapInusePerOffer > 0 &&
			cell.IndexHotBytesPerSlot > 0 &&
			cell.ProcessTotal.HeapInusePerOffer <= 2*cell.IndexHotBytesPerSlot
		out = append(out, cell)
	}
	return out
}

type heapHold struct {
	writeAmpHeap
	held any
}

func measureHeapDelta(n int, populate func() any) heapHold {
	runtime.GC()
	runtime.GC()
	var before runtime.MemStats
	runtime.ReadMemStats(&before)
	held := populate()
	runtime.GC()
	runtime.GC()
	var after runtime.MemStats
	runtime.ReadMemStats(&after)
	runtime.KeepAlive(held)

	inuse := int64(after.HeapInuse) - int64(before.HeapInuse)
	alloc := int64(after.HeapAlloc) - int64(before.HeapAlloc)
	out := heapHold{held: held}
	out.HeapInuseBefore = int64(before.HeapInuse)
	out.HeapInuseAfter = int64(after.HeapInuse)
	out.HeapAllocBefore = int64(before.HeapAlloc)
	out.HeapAllocAfter = int64(after.HeapAlloc)
	out.HeapInuseDelta = inuse
	out.HeapAllocDelta = alloc
	if inuse < 0 {
		out.HeapInuseDelta = int64(after.HeapInuse)
		out.FallbackUsed = true
	}
	if n > 0 {
		out.HeapInusePerOffer = float64(out.HeapInuseDelta) / float64(n)
		out.HeapAllocPerOffer = float64(out.HeapAllocDelta) / float64(n)
	}
	return out
}

func percentileMSWriteAmp(samples []float64) writeAmpMS {
	if len(samples) == 0 {
		return writeAmpMS{}
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
	return writeAmpMS{
		N:   len(sorted),
		P50: pct(0.50),
		P95: pct(0.95),
		P99: pct(0.99),
		Max: sorted[len(sorted)-1],
		Avg: sum / float64(len(sorted)),
	}
}

func writeWriteAmpEvidence(report writeAmpReport) error {
	rel := writeAmpBenchEvidenceRel
	if v := strings.TrimSpace(os.Getenv("MERC_WRITE_AMP_BENCH_EVIDENCE")); v != "" {
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

// --- types ------------------------------------------------------------------

type writeAmpReport struct {
	Classification   string               `json:"classification"`
	GeneratedAt      string               `json:"generated_at"`
	FinishedAt       string               `json:"finished_at"`
	WallClockSeconds float64              `json:"wall_clock_seconds"`
	SourceCommit     string               `json:"source_commit"`
	Host             string               `json:"host"`
	NumCPU           int                  `json:"num_cpu"`
	GOMAXPROCS       int                  `json:"gomaxprocs"`
	GOOS             string               `json:"goos"`
	GOARCH           string               `json:"goarch"`
	Invocation       writeAmpInvocation   `json:"invocation"`
	Honesty          writeAmpHonesty      `json:"honesty"`
	Cells            []writeAmpCell       `json:"cells"`
	Dropped          []writeAmpDropped    `json:"dropped_cells"`
	Memory           []writeAmpMemoryCell `json:"memory"`
	Surprises        []string             `json:"surprises"`
}

type writeAmpInvocation struct {
	EnvGate                string  `json:"env_gate"`
	ExcludedFromNormalGate bool    `json:"excluded_from_normal_gate"`
	ExclusionProof         string  `json:"exclusion_proof"`
	Command                string  `json:"command"`
	FleetLadder            []int   `json:"fleet_ladder"`
	HeartbeatIntervalSec   []int   `json:"heartbeat_interval_sec"`
	DurationSec            int     `json:"duration_sec"`
	RefreshIntervalSec     int     `json:"refresh_interval_sec"`
	LivenessWindowSec      int     `json:"liveness_window_sec"`
	MaxConns               int     `json:"max_conns"`
	BatchEnabled           bool    `json:"batch_enabled"`
	MaxBatch               int     `json:"max_batch"`
	FlushInterval          string  `json:"flush_interval"`
	TimeSimulation         string  `json:"time_simulation"`
	SeedMethod             string  `json:"seed_method"`
	SeededFleet            int     `json:"seeded_fleet"`
	SeedWallSeconds        float64 `json:"seed_wall_seconds"`
}

type writeAmpHonesty struct {
	WhatThisProves       string   `json:"what_this_proves"`
	WhatThisDoesNotProve string   `json:"what_this_does_not_prove"`
	Guards               []string `json:"guards"`
}

type writeAmpCell struct {
	Classification         string             `json:"classification"`
	N                      int                `json:"n"`
	HeartbeatIntervalSec   int                `json:"heartbeat_interval_sec"`
	DurationSec            int                `json:"duration_sec"`
	Ticks                  int                `json:"ticks"`
	Valid                  bool               `json:"valid"`
	InvalidReason          string             `json:"invalid_reason,omitempty"`
	HeartbeatsSubmittedOff int64              `json:"heartbeats_submitted_off"`
	HeartbeatsSubmittedOn  int64              `json:"heartbeats_submitted_on"`
	DurableWritesOff       int64              `json:"durable_writes_off"`
	DurableWritesOn        int64              `json:"durable_writes_on"`
	RatioOnOverOff         float64            `json:"ratio_on_over_off"`
	RatioOffOverOn         float64            `json:"ratio_off_over_on"`
	BucketsOff             writeAmpBuckets    `json:"buckets_off"`
	BucketsOn              writeAmpBuckets    `json:"buckets_on"`
	LatencyMSOff           writeAmpLatencySet `json:"latency_ms_off"`
	LatencyMSOn            writeAmpLatencySet `json:"latency_ms_on"`
	SafetyOff              writeAmpSafety     `json:"safety_off"`
	SafetyOn               writeAmpSafety     `json:"safety_on"`
	TickMSOff              writeAmpMS         `json:"tick_ms_off"`
	TickMSOn               writeAmpMS         `json:"tick_ms_on"`
	XactCommitDeltaOff     int64              `json:"xact_commit_delta_off"`
	XactCommitDeltaOn      int64              `json:"xact_commit_delta_on"`
	CoalescerFlushesOff    int64              `json:"coalescer_flushes_off"`
	CoalescerFlushesOn     int64              `json:"coalescer_flushes_on"`
	CoalescerItemsOff      int64              `json:"coalescer_items_off"`
	CoalescerItemsOn       int64              `json:"coalescer_items_on"`
	PreloadMSOff           float64            `json:"preload_ms_off"`
	PreloadMSOn            float64            `json:"preload_ms_on"`
	WallSecondsOff         float64            `json:"wall_seconds_off"`
	WallSecondsOn          float64            `json:"wall_seconds_on"`
	MissedBeatsOff         int                `json:"missed_beats_off"`
	MissedBeatsOn          int                `json:"missed_beats_on"`
	FirstErrorOff          string             `json:"first_error_off,omitempty"`
	FirstErrorOn           string             `json:"first_error_on,omitempty"`
}

type writeAmpDropped struct {
	N           int    `json:"n,omitempty"`
	IntervalSec int    `json:"heartbeat_interval_sec,omitempty"`
	Flag        string `json:"flag,omitempty"`
	Reason      string `json:"reason"`
	Detail      string `json:"detail"`
}

type writeAmpSide struct {
	Authoritative    bool
	N                int
	Workers          int
	TicksPlanned     int
	TicksRan         int
	Submitted        int64
	DurableWrites    int64
	SamplesBefore    int64
	SamplesAfter     int64
	XactCommitDelta  int64
	CoalescerFlushes int64
	CoalescerItems   int64
	Buckets          writeAmpBuckets
	LatencyMS        writeAmpLatencySet
	Safety           writeAmpSafety
	TickMS           writeAmpMS
	PreloadMS        float64
	WallSeconds      float64
	MissedBeats      int
	FirstError       string
	Invalid          bool
	InvalidReason    string
}

type writeAmpBuckets struct {
	SUCCESS int64 `json:"SUCCESS"`
	REFUSAL int64 `json:"REFUSAL"`
	EMPTY   int64 `json:"EMPTY"`
	FAILURE int64 `json:"FAILURE"`
}

type writeAmpLatencySet struct {
	SUCCESS writeAmpMS `json:"SUCCESS"`
	REFUSAL writeAmpMS `json:"REFUSAL"`
	EMPTY   writeAmpMS `json:"EMPTY"`
	FAILURE writeAmpMS `json:"FAILURE"`
}

type writeAmpMS struct {
	N   int     `json:"n"`
	P50 float64 `json:"p50"`
	P95 float64 `json:"p95"`
	P99 float64 `json:"p99"`
	Max float64 `json:"max"`
	Avg float64 `json:"avg"`
}

type writeAmpSafety struct {
	CheckedOffers int64   `json:"checked_offers"`
	AgedOffers    int64   `json:"aged_offers"`
	InsideWindow  bool    `json:"inside_window"`
	OldestAgeSec  float64 `json:"oldest_age_sec"`
	NewestAgeSec  float64 `json:"newest_age_sec"`
}

type writeAmpMemoryCell struct {
	N                           int                `json:"n"`
	PopulateMethod              string             `json:"populate_method"`
	TypeSizeof                  writeAmpTypeSizeof `json:"type_sizeof_bytes"`
	SlotOwnerCapacity           uint32             `json:"slot_owner_capacity"`
	OfferSlotCache              writeAmpHeap       `json:"offer_slot_cache"`
	OfferPersistCache           writeAmpHeap       `json:"offer_persist_cache"`
	SlotOwner                   writeAmpHeap       `json:"slot_owner"`
	ProcessTotal                writeAmpHeap       `json:"process_total"`
	IndexHeap                   writeAmpHeap       `json:"index_heap"`
	IndexHotBytes               int64              `json:"index_hot_bytes"`
	IndexHotBytesPerOffer       float64            `json:"index_hot_bytes_per_offer"`
	IndexHotBytesPerSlot        float64            `json:"index_hot_bytes_per_slot"`
	Headline12_41Representative bool               `json:"headline_12_41_b_per_device_is_representative_of_process"`
}

type writeAmpTypeSizeof struct {
	OfferSlotKeyBytes      int `json:"offer_slot_key"`
	OfferBindingBytes      int `json:"offer_binding"`
	OfferPersistStateBytes int `json:"offer_persist_state"`
	SlotOwnerElemBytes     int `json:"slot_owner_elem"`
}

type writeAmpHeap struct {
	HeapInuseBefore   int64   `json:"heap_inuse_before"`
	HeapInuseAfter    int64   `json:"heap_inuse_after"`
	HeapAllocBefore   int64   `json:"heap_alloc_before"`
	HeapAllocAfter    int64   `json:"heap_alloc_after"`
	HeapInuseDelta    int64   `json:"heap_inuse_delta"`
	HeapAllocDelta    int64   `json:"heap_alloc_delta"`
	HeapInusePerOffer float64 `json:"heap_inuse_per_offer"`
	HeapAllocPerOffer float64 `json:"heap_alloc_per_offer"`
	FallbackUsed      bool    `json:"heap_inuse_fallback_used"`
}
