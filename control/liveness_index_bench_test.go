package main

// In-process LiveDeviceIndex ceiling harness.
//
// Measures the standalone presence engine (no PostgreSQL, no selector
// wiring). Opt-in only — never part of make test / make ci:
//
//	MERC_LIVENESS_INDEX_BENCH=1 \
//	  go test -count=1 -run '^TestLiveDeviceIndexBench$' -timeout 30m ./control
//
// Writes evidence/perf/liveness-index-bench.json when run from control/.

import (
	"encoding/json"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

const (
	livenessIndexBenchEnv         = "MERC_LIVENESS_INDEX_BENCH"
	livenessIndexBenchFleetEnv    = "MERC_LIVENESS_INDEX_BENCH_FLEET"
	livenessIndexBenchDurationEnv = "MERC_LIVENESS_INDEX_BENCH_DURATION_SEC"
	livenessIndexBenchEvidenceRel = "evidence/perf/liveness-index-bench.json"
)

func TestLiveDeviceIndexBench(t *testing.T) {
	if os.Getenv(livenessIndexBenchEnv) != "1" {
		t.Skip("set MERC_LIVENESS_INDEX_BENCH=1 to measure the in-process live-device index")
	}

	fleets := parseIntListEnv(t, livenessIndexBenchFleetEnv, []int{100_000, 1_000_000, 10_000_000})
	durationSec := 2
	if v := os.Getenv(livenessIndexBenchDurationEnv); v != "" {
		n := 0
		if _, err := fmt.Sscanf(v, "%d", &n); err != nil || n < 1 {
			t.Fatalf("%s=%q: need integer >= 1", livenessIndexBenchDurationEnv, v)
		}
		durationSec = n
	}

	host, _ := os.Hostname()
	startedAt := time.Now().UTC()
	report := livenessIndexBenchReport{
		GeneratedAt:  startedAt.Format(time.RFC3339),
		SourceCommit: mercSourceCommitSHA(),
		Host:         host,
		NumCPU:       runtime.NumCPU(),
		GOMAXPROCS:   runtime.GOMAXPROCS(0),
		GOOS:         runtime.GOOS,
		GOARCH:       runtime.GOARCH,
		Invocation: livenessIndexBenchInvocation{
			EnvGate:                livenessIndexBenchEnv + "=1",
			ExcludedFromNormalGate: true,
			ExclusionProof:         "TestLiveDeviceIndexBench skips unless MERC_LIVENESS_INDEX_BENCH=1; listed in scripts/allowed-test-skips.txt; make test / make ci never set the env var",
			DurationSec:            durationSec,
			FleetLadder:            fleets,
			TargetHeartbeatPerSec:  250_000,
			BytesPerDeviceTarget:   32,
			BytesPerDeviceStretch:  16,
		},
		Honesty: livenessIndexBenchHonesty{
			WhatThisProves:       "in-process Heartbeat/IsLive/Tick/LiveSlots cost and retained bytes/device of LiveDeviceIndex on this host, with no PostgreSQL on the path",
			WhatThisDoesNotProve: "production selector intersection, authenticated HTTP ingest, multi-host presence, or a droplet-class 1vCPU ceiling",
			Guards: []string{
				"no PostgreSQL in this path — a heartbeat is an in-process O(1) update",
				"bytes/device is HotBytes()/N (epochs + membership + wheel occupancy), plus a HeapInuse delta after GC",
				"IsLive p50/p95 includes clock-read overhead around a few-nanosecond load",
				"LiveSlots allocates and fills a compact []uint32 of currently-live slots",
				"this index is not wired to eligibility; numbers are presence-engine only",
			},
		},
	}

	for _, n := range fleets {
		t.Logf("fleet=%d starting", n)
		cell := measureLiveDeviceIndexFleet(t, n, durationSec)
		report.Cells = append(report.Cells, cell)
		t.Logf("fleet=%d hot=%.2f B/dev heap=%.2f B/dev wheel=%.2f B/dev rebuild=%.3fs (%.0f hb/s) hb=%.0f/s islive_p50=%.1fns islive_p95=%.1fns liveslots=%.2fms tick_idle=%.2fms tick_expire=%.2fms",
			n, cell.HotBytesPerDevice, cell.HeapBytesPerDevice, cell.WheelBytesPerDevice,
			cell.RebuildSeconds, cell.RebuildHeartbeatsPerSec, cell.HeartbeatPerSec,
			cell.IsLiveNS.P50, cell.IsLiveNS.P95, cell.LiveSlotsMS, cell.TickIdleMS, cell.TickExpireMS)
	}

	report.FinishedAt = time.Now().UTC().Format(time.RFC3339)
	report.WallClockSeconds = time.Since(startedAt).Seconds()
	if err := writeLivenessIndexBenchEvidence(report); err != nil {
		t.Fatalf("write evidence: %v", err)
	}
	t.Logf("wrote %s", livenessIndexBenchEvidenceRel)
}

func measureLiveDeviceIndexFleet(t *testing.T, n, durationSec int) livenessIndexBenchCell {
	t.Helper()
	cell := livenessIndexBenchCell{Fleet: n, DurationSec: durationSec, Workers: runtime.GOMAXPROCS(0)}
	if cell.Workers < 1 {
		cell.Workers = 1
	}

	runtime.GC()
	runtime.GC()
	var msBefore runtime.MemStats
	runtime.ReadMemStats(&msBefore)

	idx := NewLiveDeviceIndex(uint32(n))
	now := uint32(time.Now().Unix())
	if now < 1_000_000 {
		now = liveIdxTestNow
	}

	rebuildStart := time.Now()
	for slot := uint32(0); slot < uint32(n); slot++ {
		if err := idx.Heartbeat(slot, now, now); err != nil {
			t.Fatalf("rebuild heartbeat slot %d: %v", slot, err)
		}
	}
	cell.RebuildSeconds = time.Since(rebuildStart).Seconds()
	if cell.RebuildSeconds > 0 {
		cell.RebuildHeartbeatsPerSec = float64(n) / cell.RebuildSeconds
	}

	runtime.GC()
	runtime.GC()
	var msAfter runtime.MemStats
	runtime.ReadMemStats(&msAfter)

	cell.HotBytes = idx.HotBytes()
	cell.EpochBytes = idx.EpochBytes()
	cell.WheelBytes = idx.WheelBytes()
	cell.HeapInuseDelta = int64(msAfter.HeapInuse) - int64(msBefore.HeapInuse)
	if cell.HeapInuseDelta < 0 {
		cell.HeapInuseDelta = int64(msAfter.HeapInuse)
	}
	if n > 0 {
		cell.HotBytesPerDevice = float64(cell.HotBytes) / float64(n)
		cell.EpochBytesPerDevice = float64(cell.EpochBytes) / float64(n)
		cell.WheelBytesPerDevice = float64(cell.WheelBytes) / float64(n)
		cell.HeapBytesPerDevice = float64(cell.HeapInuseDelta) / float64(n)
	}
	cell.Under32 = cell.HotBytesPerDevice <= 32
	cell.Under16 = cell.HotBytesPerDevice <= 16

	lsStart := time.Now()
	got := idx.LiveSlots(now)
	cell.LiveSlotsMS = float64(time.Since(lsStart).Microseconds()) / 1000.0
	cell.LiveSlotsCount = len(got)
	if cell.LiveSlotsCount != n {
		t.Fatalf("fleet=%d LiveSlots=%d want %d after rebuild", n, cell.LiveSlotsCount, n)
	}

	// Tick costs against the just-rebuilt population, before the
	// throughput loop mutates epochs across later seconds.
	idleStart := time.Now()
	idx.Tick(now + 1)
	cell.TickIdleMS = float64(time.Since(idleStart).Microseconds()) / 1000.0

	expireStart := time.Now()
	idx.Tick(now + liveDeviceWindowEpochs + 1)
	cell.TickExpireMS = float64(time.Since(expireStart).Microseconds()) / 1000.0
	cell.LiveAfterExpire = len(idx.LiveSlots(now + liveDeviceWindowEpochs + 1))

	// Epochs are still `now` (Tick only drops the wheel). IsLive(now) is
	// still true; the throughput loop puts slots back on the wheel.
	cell.HeartbeatPerSec, cell.HeartbeatLatencyNS = measureIndexHeartbeatThroughput(idx, uint32(n), now, cell.Workers, time.Duration(durationSec)*time.Second)
	cell.IsLiveNS = measureIndexIsLiveLatency(idx, uint32(n), now)
	cell.IsLivePerSec = measureIndexIsLiveThroughput(idx, uint32(n), now, cell.Workers, time.Second)

	// Release the index before the next fleet so 10M is not sitting
	// next to a 1M leftover.
	idx = nil
	runtime.GC()
	return cell
}

func measureIndexHeartbeatThroughput(idx *LiveDeviceIndex, n, now uint32, workers int, d time.Duration) (perSec float64, lat livenessIndexNS) {
	deadline := time.Now().Add(d)
	var (
		ok   atomic.Int64
		next atomic.Uint64
		mu   sync.Mutex
		ns   []float64
	)
	// Bound the per-call sample so we do not retain 10M latencies.
	const maxSamples = 200_000
	var wg sync.WaitGroup
	for w := 0; w < workers; w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			local := make([]float64, 0, 4096)
			for time.Now().Before(deadline) {
				slot := uint32(next.Add(1)-1) % n
				// Use wall-clock now so a 2s run occupies a couple of
				// buckets, matching a real 1 Hz tick more than a single
				// frozen second. Clamp is a no-op (obs == now).
				obs := uint32(time.Now().Unix())
				if obs < now {
					obs = now
				}
				start := time.Now()
				err := idx.Heartbeat(slot, obs, obs)
				elapsed := float64(time.Since(start).Nanoseconds())
				if err != nil {
					continue
				}
				ok.Add(1)
				if len(local) < maxSamples/workers+1 {
					local = append(local, elapsed)
				}
			}
			mu.Lock()
			ns = append(ns, local...)
			mu.Unlock()
		}()
	}
	wg.Wait()
	sec := d.Seconds()
	if sec <= 0 {
		sec = 1
	}
	return float64(ok.Load()) / sec, percentileNS(ns)
}

func measureIndexIsLiveLatency(idx *LiveDeviceIndex, n, now uint32) livenessIndexNS {
	// A single IsLive is one atomic load — below time.Now resolution on
	// this host — so each sample is a batch of calls, reported per call.
	const samples = 50_000
	const batch = 64
	ns := make([]float64, samples)
	for i := 0; i < samples; i++ {
		start := time.Now()
		for k := 0; k < batch; k++ {
			_ = idx.IsLive(uint32(i+k)%n, now)
		}
		ns[i] = float64(time.Since(start).Nanoseconds()) / batch
	}
	return percentileNS(ns)
}

func measureIndexIsLiveThroughput(idx *LiveDeviceIndex, n, now uint32, workers int, d time.Duration) float64 {
	deadline := time.Now().Add(d)
	var ok atomic.Int64
	var next atomic.Uint64
	var wg sync.WaitGroup
	for w := 0; w < workers; w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for time.Now().Before(deadline) {
				slot := uint32(next.Add(1)-1) % n
				_ = idx.IsLive(slot, now)
				ok.Add(1)
			}
		}()
	}
	wg.Wait()
	return float64(ok.Load()) / d.Seconds()
}

func percentileNS(samples []float64) livenessIndexNS {
	if len(samples) == 0 {
		return livenessIndexNS{}
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
	return livenessIndexNS{
		N:   len(sorted),
		P50: pct(0.50),
		P95: pct(0.95),
		P99: pct(0.99),
		Max: sorted[len(sorted)-1],
		Avg: sum / float64(len(sorted)),
	}
}

func writeLivenessIndexBenchEvidence(report livenessIndexBenchReport) error {
	rel := livenessIndexBenchEvidenceRel
	if v := os.Getenv("MERC_LIVENESS_INDEX_BENCH_EVIDENCE"); v != "" {
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

type livenessIndexBenchReport struct {
	GeneratedAt      string                       `json:"generated_at"`
	FinishedAt       string                       `json:"finished_at"`
	WallClockSeconds float64                      `json:"wall_clock_seconds"`
	SourceCommit     string                       `json:"source_commit"`
	Host             string                       `json:"host"`
	NumCPU           int                          `json:"num_cpu"`
	GOMAXPROCS       int                          `json:"gomaxprocs"`
	GOOS             string                       `json:"goos"`
	GOARCH           string                       `json:"goarch"`
	Invocation       livenessIndexBenchInvocation `json:"invocation"`
	Honesty          livenessIndexBenchHonesty    `json:"honesty"`
	Cells            []livenessIndexBenchCell     `json:"cells"`
}

type livenessIndexBenchInvocation struct {
	EnvGate                string `json:"env_gate"`
	ExcludedFromNormalGate bool   `json:"excluded_from_normal_gate"`
	ExclusionProof         string `json:"exclusion_proof"`
	DurationSec            int    `json:"duration_sec"`
	FleetLadder            []int  `json:"fleet_ladder"`
	TargetHeartbeatPerSec  int    `json:"target_heartbeat_per_sec"`
	BytesPerDeviceTarget   int    `json:"bytes_per_device_target"`
	BytesPerDeviceStretch  int    `json:"bytes_per_device_stretch"`
}

type livenessIndexBenchHonesty struct {
	WhatThisProves       string   `json:"what_this_proves"`
	WhatThisDoesNotProve string   `json:"what_this_does_not_prove"`
	Guards               []string `json:"guards"`
}

type livenessIndexBenchCell struct {
	Fleet                   int             `json:"fleet"`
	DurationSec             int             `json:"duration_sec"`
	Workers                 int             `json:"workers"`
	HotBytes                int64           `json:"hot_bytes"`
	EpochBytes              int64           `json:"epoch_bytes"`
	WheelBytes              int64           `json:"wheel_bytes"`
	HeapInuseDelta          int64           `json:"heap_inuse_delta"`
	HotBytesPerDevice       float64         `json:"hot_bytes_per_device"`
	EpochBytesPerDevice     float64         `json:"epoch_bytes_per_device"`
	WheelBytesPerDevice     float64         `json:"wheel_bytes_per_device"`
	HeapBytesPerDevice      float64         `json:"heap_bytes_per_device"`
	Under32                 bool            `json:"under_32_byte_target"`
	Under16                 bool            `json:"under_16_byte_stretch"`
	RebuildSeconds          float64         `json:"rebuild_seconds"`
	RebuildHeartbeatsPerSec float64         `json:"rebuild_heartbeats_per_sec"`
	HeartbeatPerSec         float64         `json:"heartbeat_per_sec"`
	HeartbeatLatencyNS      livenessIndexNS `json:"heartbeat_latency_ns"`
	IsLiveNS                livenessIndexNS `json:"islive_latency_ns"`
	IsLivePerSec            float64         `json:"islive_per_sec"`
	LiveSlotsMS             float64         `json:"liveslots_ms"`
	LiveSlotsCount          int             `json:"liveslots_count"`
	TickIdleMS              float64         `json:"tick_idle_ms"`
	TickExpireMS            float64         `json:"tick_expire_ms"`
	LiveAfterExpire         int             `json:"live_after_expire"`
}

type livenessIndexNS struct {
	N   int     `json:"n"`
	P50 float64 `json:"p50"`
	P95 float64 `json:"p95"`
	P99 float64 `json:"p99"`
	Max float64 `json:"max"`
	Avg float64 `json:"avg"`
}
