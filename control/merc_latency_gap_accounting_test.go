package main

import (
	"bufio"
	"bytes"
	"context"
	"fmt"
	"io"
	"log"
	"math"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
)

// gapCellResult is one (concurrency) measurement cell for latency-gap accounting.
type gapCellResult struct {
	Concurrency      int            `json:"concurrency"`
	Samples          int            `json:"samples"`
	Warmup           int            `json:"warmup"`
	MercOK           int            `json:"merc_ok"`
	MercFail         int            `json:"merc_fail"`
	DirectOK         int            `json:"direct_ok"`
	DirectFail       int            `json:"direct_fail"`
	PathLines        int            `json:"path_timing_lines"`
	LoadAvg          []float64      `json:"load_average_at_cell"`
	Quiet            bool           `json:"quiet"`
	Client           map[string]any `json:"client_ttft_ms"`
	Direct           map[string]any `json:"direct_ttft_ms"`
	Overhead         map[string]any `json:"overhead_merc_minus_direct_ms"`
	Stages           map[string]any `json:"stages_ms"`
	MercOwned        map[string]any `json:"merc_owned_ttft_ms"`
	EngineFacing     map[string]any `json:"engine_facing_ms"`
	HandlerTTFT      map[string]any `json:"handler_ttft_ms"`
	MarkedSum        map[string]any `json:"marked_ttft_sum_ms"`
	UnmarkedResidual map[string]any `json:"unmarked_residual_ms"`
	AuthLookup       map[string]any `json:"auth_lookup_ms"`
	Attribution      map[string]any `json:"attribution"`
	Notes            []string       `json:"notes"`
}

type gapSamp struct {
	ttft time.Duration
	ok   bool
	err  string
}

type pathTimingLineExt struct {
	pathTimingLine
	HandlerTTFTMS float64
	MarkedSumMS   float64
	ResidualMS    float64
	HasTTFT       bool
}

// TestMercLatencyGapAccounting closes the accounting between client wall-clock
// TTFT (parity style) and named path-timing stages against a *real* engine.
//
//	MERC_LATENCY_GAP_ACCOUNTING=1 \
//	MERC_REALTIME_UPSTREAM=http://127.0.0.1:8095/v1 \
//	MERC_REALTIME_UPSTREAM_KEY=merc-canary-key \
//	MERC_TEST_DATABASE_URL=postgres://cx:cx@localhost:5432/cx?sslmode=disable \
//	go test -count=1 -run TestMercLatencyGapAccounting -timeout 45m ./control
//
// Does not optimise. Emits a bound receipt that attributes every millisecond
// of merc-added latency at c=1 and c=32, p50-to-p50 and p95-to-p95.
func TestMercLatencyGapAccounting(t *testing.T) {
	if os.Getenv("MERC_LATENCY_GAP_ACCOUNTING") != "1" {
		t.Skip("set MERC_LATENCY_GAP_ACCOUNTING=1 to run the latency-gap accounting harness")
	}
	upstream := strings.TrimSpace(os.Getenv("MERC_REALTIME_UPSTREAM"))
	upstreamKey := strings.TrimSpace(os.Getenv("MERC_REALTIME_UPSTREAM_KEY"))
	if upstream == "" || upstreamKey == "" {
		t.Fatal("MERC_REALTIME_UPSTREAM and MERC_REALTIME_UPSTREAM_KEY are required")
	}
	probe, err := http.NewRequest(http.MethodGet, strings.TrimRight(upstream, "/")+"/models", nil)
	if err != nil {
		t.Fatal(err)
	}
	probe.Header.Set("Authorization", "Bearer "+upstreamKey)
	probeResp, err := http.DefaultClient.Do(probe)
	if err != nil {
		t.Fatalf("engine not reachable at %s: %v", upstream, err)
	}
	probeResp.Body.Close()
	if probeResp.StatusCode != http.StatusOK {
		t.Fatalf("engine at %s answered HTTP %d", upstream, probeResp.StatusCode)
	}

	installSettlementCurrencyForTest(t, "usd")
	t.Setenv("MERC_TOKEN_KEY", "merc-latency-gap-key-with-at-least-32-bytes!!")
	t.Setenv("STRIPE_SECRET_KEY", "")
	t.Setenv("MERC_CANARY_ENABLED", "false")
	t.Setenv("MERC_REALTIME_PATH_TIMING", "1")
	t.Setenv(parityUpstreamCaptureEnv, "1")

	ctx, store, pool := openIsolatedTestStore(t)
	profile := sortedVLLMProfiles()[0]

	const (
		samplesC1  = 40
		samplesC32 = 96
		warmup     = 8
		offerCap   = 256
		maxBuyers  = 32
	)
	buyerKeys := make([]string, maxBuyers)
	for i := 0; i < maxBuyers; i++ {
		id, cerr := store.CreateBuyerAccount(ctx,
			fmt.Sprintf("gap-lat-%d-%s@example.test", i, uuid.NewString()[:8]),
			"integration-password", 100_000)
		if cerr != nil {
			t.Fatal(cerr)
		}
		_, key, _, kerr := store.CreateAPIKey(ctx, id, "gap accounting", true)
		if kerr != nil {
			t.Fatal(kerr)
		}
		buyerKeys[i] = key
	}
	supplierID, workerID := uuid.New(), uuid.New()
	if _, err := pool.Exec(ctx, `INSERT INTO suppliers (id,email,status) VALUES ($1,$2,'active')`,
		supplierID, "gap-lat-supplier-"+uuid.NewString()+"@example.test"); err != nil {
		t.Fatal(err)
	}
	if _, err := store.CreateWorkerToken(ctx, workerID, supplierID); err != nil {
		t.Fatal(err)
	}
	worker := WorkerAuth{WorkerID: workerID, SupplierID: supplierID}
	if err := store.UpsertRealtimeOffer(ctx, worker, RealtimeOfferRegistration{
		RuntimeProfileID: profile.RuntimeProfileID, RuntimeProfileSHA256: profile.ProfileSHA256,
		HWClass: "nvidia_24gb", GPUCount: 1, MemoryGBPerGPU: 24,
		UpstreamBaseURL: strings.TrimRight(upstream, "/"), UpstreamToken: upstreamKey,
		Warmth: "HOT", MaxActiveSequences: offerCap, AvailableSequences: offerCap,
		SupplierInputUSDPerMillionTokens: 0.08, SupplierOutputUSDPerMillionTokens: 0.30,
	}); err != nil {
		t.Fatal(err)
	}
	refresh := time.NewTicker(8 * time.Second)
	t.Cleanup(refresh.Stop)
	go func() {
		for range refresh.C {
			c, cancel := context.WithTimeout(context.Background(), 4*time.Second)
			_ = store.HeartbeatRealtimeOffer(c, worker, RealtimeOfferHeartbeat{
				RuntimeProfileID: profile.RuntimeProfileID, Warmth: "HOT",
				AvailableSequences: offerCap, Status: "ACTIVE",
			})
			cancel()
		}
	}()

	server := httptest.NewServer(NewServer(store, nil, nil, nil).Routes())
	t.Cleanup(server.Close)

	logWriter := &syncBuffer{}
	prevWriter := log.Writer()
	log.SetOutput(io.MultiWriter(prevWriter, logWriter))
	t.Cleanup(func() { log.SetOutput(prevWriter) })

	body := []byte(`{"model":"cx-chat-1b","messages":[{"role":"user","content":"Write a short factual paragraph about the water cycle. Be specific and do not repeat yourself."}],"stream":true,"max_tokens":32,"temperature":0,"top_p":0.95,"stream_options":{"include_usage":true}}`)

	httpClient := &http.Client{
		Timeout: 5 * time.Minute,
		Transport: &http.Transport{
			MaxIdleConns:        256,
			MaxIdleConnsPerHost: 256,
			IdleConnTimeout:     90 * time.Second,
		},
	}

	loadAvg, loadN := readLoadAverage()
	quiet := machineLoadQuiet()
	host, _ := os.Hostname()
	measuredAt := time.Now().UTC()

	runCell := func(c, n int) gapCellResult {
		if _, err := pool.Exec(ctx, `
			UPDATE realtime_worker_offers
			   SET available_sequences=max_active_sequences, status='ACTIVE', last_seen_at=now()
			 WHERE worker_id=$1 AND runtime_profile_id=$2`,
			workerID, profile.RuntimeProfileID); err != nil {
			t.Fatal(err)
		}
		cellLoad, _ := readLoadAverage()

		for i := 0; i < warmup; i++ {
			_ = doSegmentStreamRequest(httpClient, server.URL, buyerKeys[i%len(buyerKeys)], body)
			_, _ = measureClientTTFTDirect(httpClient, strings.TrimRight(upstream, "/"), upstreamKey, body)
		}
		logWriter.Reset()

		// Merc wave.
		mercS := make([]gapSamp, n)
		var wg sync.WaitGroup
		jobs := make(chan int)
		for i := 0; i < c; i++ {
			wi := i
			wg.Add(1)
			go func() {
				defer wg.Done()
				key := buyerKeys[wi%len(buyerKeys)]
				for idx := range jobs {
					ttft, err := measureClientTTFT(httpClient, server.URL, key, body)
					if err != nil {
						mercS[idx] = gapSamp{ttft: ttft, ok: false, err: err.Error()}
						continue
					}
					mercS[idx] = gapSamp{ttft: ttft, ok: true}
				}
			}()
		}
		for i := 0; i < n; i++ {
			jobs <- i
		}
		close(jobs)
		wg.Wait()
		time.Sleep(150 * time.Millisecond)
		lines := parsePathTimingLinesExtended(logWriter.String())

		// Direct wave (sequential after merc; engine peak ≤ c).
		directS := make([]gapSamp, n)
		jobs = make(chan int)
		var dwg sync.WaitGroup
		for i := 0; i < c; i++ {
			dwg.Add(1)
			go func() {
				defer dwg.Done()
				for idx := range jobs {
					ttft, err := measureClientTTFTDirect(httpClient, strings.TrimRight(upstream, "/"), upstreamKey, body)
					if err != nil {
						directS[idx] = gapSamp{ttft: ttft, ok: false, err: err.Error()}
						continue
					}
					directS[idx] = gapSamp{ttft: ttft, ok: true}
				}
			}()
		}
		for i := 0; i < n; i++ {
			jobs <- i
		}
		close(jobs)
		dwg.Wait()

		mercTTFT := gapDurationsOK(mercS)
		directTTFT := gapDurationsOK(directS)
		mercOK, mercFail := gapCountOK(mercS)
		directOK, directFail := gapCountOK(directS)
		if mercOK == 0 {
			t.Fatalf("c=%d: zero merc successes (fail=%d); first err=%s", c, mercFail, mercS[0].err)
		}
		if directOK == 0 {
			t.Fatalf("c=%d: zero direct successes (fail=%d); first err=%s", c, directFail, directS[0].err)
		}

		// Convert for existing stage aggregator.
		baseLines := make([]pathTimingLine, len(lines))
		for i, l := range lines {
			baseLines[i] = l.pathTimingLine
		}
		stages := aggregateStageSamples(baseLines)
		handlerTTFTs, markedSums, residuals, auths := extendedFields(lines)

		mercOwnedKeys := []string{
			"auth_lookup", "read_body", "prepare_json", "intake_control",
			"authorize_contract", "admission_event", "arrival_batch",
			"pre_upstream", "settlement_intent", "post_upstream",
		}
		// upstream_connect is nested inside upstream_ttfb (httptrace dial);
		// do not sum both or engine_facing double-counts the dial.
		engineKeys := []string{"upstream_ttfb", "upstream_first_sse"}
		mercOwned := sumStageSamplesExt(lines, mercOwnedKeys)
		engineFacing := sumStageSamplesExt(lines, engineKeys)

		overhead := map[string]any{
			"p50":    pctDurMs(mercTTFT, 0.50) - pctDurMs(directTTFT, 0.50),
			"p95":    pctDurMs(mercTTFT, 0.95) - pctDurMs(directTTFT, 0.95),
			"p99":    pctDurMs(mercTTFT, 0.99) - pctDurMs(directTTFT, 0.99),
			"mean":   meanDurMs(mercTTFT) - meanDurMs(directTTFT),
			"method": "percentile_difference (merc_pX − direct_pX); PercentileNearestRank with p in [0,1]",
			"n_merc": len(mercTTFT), "n_direct": len(directTTFT),
		}

		mercOwnedP50 := pctDurMs(mercOwned, 0.50)
		mercOwnedP95 := pctDurMs(mercOwned, 0.95)
		engineP50 := pctDurMs(engineFacing, 0.50)
		engineP95 := pctDurMs(engineFacing, 0.95)
		directP50 := pctDurMs(directTTFT, 0.50)
		directP95 := pctDurMs(directTTFT, 0.95)
		engineExcessP50 := engineP50 - directP50
		engineExcessP95 := engineP95 - directP95
		ohP50 := overhead["p50"].(float64)
		ohP95 := overhead["p95"].(float64)
		unexplainedP50 := ohP50 - mercOwnedP50 - engineExcessP50
		unexplainedP95 := ohP95 - mercOwnedP95 - engineExcessP95

		notes := []string{
			"merc and direct waves run sequentially (engine peak ≤ c), matching gateway-parity interleaved policy",
			"one prepaid buyer per in-flight slot on merc arm",
			"path timing requires MERC_REALTIME_PATH_TIMING=1 (set by harness)",
		}
		if mercFail > 0 {
			notes = append(notes, fmt.Sprintf("merc failures: %d", mercFail))
		}
		if directFail > 0 {
			notes = append(notes, fmt.Sprintf("direct failures: %d", directFail))
		}

		return gapCellResult{
			Concurrency: c, Samples: n, Warmup: warmup,
			MercOK: mercOK, MercFail: mercFail, DirectOK: directOK, DirectFail: directFail,
			PathLines: len(lines), LoadAvg: cellLoad, Quiet: machineLoadQuiet(),
			Client:           summarizeDurMs(mercTTFT),
			Direct:           summarizeDurMs(directTTFT),
			Overhead:         overhead,
			Stages:           summarizeStageMapMs(stages),
			MercOwned:        summarizeDurMs(mercOwned),
			EngineFacing:     summarizeDurMs(engineFacing),
			HandlerTTFT:      summarizeDurMs(handlerTTFTs),
			MarkedSum:        summarizeDurMs(markedSums),
			UnmarkedResidual: summarizeDurMs(residuals),
			AuthLookup:       summarizeDurMs(auths),
			Attribution: map[string]any{
				"p50_ms": map[string]any{
					"parity_overhead":         ohP50,
					"merc_owned_named":        mercOwnedP50,
					"engine_facing_on_merc":   engineP50,
					"direct_ttft":             directP50,
					"engine_excess_vs_direct": engineExcessP50,
					"unexplained_after_split": unexplainedP50,
					"equation":                "overhead ≟ merc_owned + (engine_facing − direct_ttft) + unexplained",
				},
				"p95_ms": map[string]any{
					"parity_overhead":         ohP95,
					"merc_owned_named":        mercOwnedP95,
					"engine_facing_on_merc":   engineP95,
					"direct_ttft":             directP95,
					"engine_excess_vs_direct": engineExcessP95,
					"unexplained_after_split": unexplainedP95,
					"equation":                "overhead ≟ merc_owned + (engine_facing − direct_ttft) + unexplained",
				},
				"engine_misattribution_verdict": engineMisattributionVerdict(engineExcessP50, engineExcessP95, ohP50, ohP95),
			},
			Notes: notes,
		}
	}

	c1 := runCell(1, samplesC1)
	t.Logf("c=1: merc p50=%.3f p95=%.3f | direct p50=%.3f p95=%.3f | oh p50=%.3f p95=%.3f | owned p50=%.3f p95=%.3f | residual p50=%.3f path_lines=%d",
		c1.Client["p50"], c1.Client["p95"], c1.Direct["p50"], c1.Direct["p95"],
		c1.Overhead["p50"], c1.Overhead["p95"], c1.MercOwned["p50"], c1.MercOwned["p95"],
		c1.UnmarkedResidual["p50"], c1.PathLines)
	c32 := runCell(32, samplesC32)
	t.Logf("c=32: merc p50=%.3f p95=%.3f | direct p50=%.3f p95=%.3f | oh p50=%.3f p95=%.3f | owned p50=%.3f p95=%.3f | residual p50=%.3f path_lines=%d",
		c32.Client["p50"], c32.Client["p95"], c32.Direct["p50"], c32.Direct["p95"],
		c32.Overhead["p50"], c32.Overhead["p95"], c32.MercOwned["p50"], c32.MercOwned["p95"],
		c32.UnmarkedResidual["p50"], c32.PathLines)

	attackList := buildAttackList(c1, c32)

	out := map[string]any{
		"schema_version": 1,
		"kind":           "merc_latency_gap_accounting_receipt",
		"measured_at":    measuredAt.Format(time.RFC3339),
		"method": map[string]any{
			"what": "Close the accounting between client wall-clock TTFT (parity style) and named " +
				"path-timing stages against a real llama.cpp/Metal engine at c=1 and c=32. " +
				"Report p50-to-p50 and p95-to-p95. Name residual. Detect engine misattribution.",
			"engine": map[string]any{
				"base_url": strings.TrimRight(upstream, "/"),
				"note":     strings.TrimSpace(os.Getenv("MERC_GATEWAY_PARITY_ENGINE_NOTE")),
			},
			"control_plane":     "httptest wrapping NewServer(store).Routes() on isolated Postgres",
			"path_timing":       "MERC_REALTIME_PATH_TIMING=1; stages include auth_lookup, pre_upstream, post_upstream, upstream_first_sse, unmarked residual",
			"percentile_method": "nearest-rank: ceil(p*n)-1 on sorted samples",
			"samples":           map[string]int{"c1": samplesC1, "c32": samplesC32, "warmup_per_cell": warmup},
			"machine": map[string]any{
				"hostname": host, "goos": runtime.GOOS, "goarch": runtime.GOARCH,
				"num_cpu": runtime.NumCPU(), "load_average": loadAvg, "load_n": loadN,
				"quiet": quiet, "quiet_reason": quietReason(quiet, loadAvg, loadN),
			},
			"segment_definitions": map[string]string{
				"auth_lookup":        "buyer middleware LookupAPIKey/session (before handler t0)",
				"read_body":          "io.ReadAll of request body",
				"prepare_json":       "prepareRealtimeRequest",
				"intake_control":     "OperationalControlPaused DB read",
				"authorize_contract": "AuthorizeRealtimeContract transaction",
				"admission_event":    "recordRealtimeAdmissionEvent (async enqueue when configured)",
				"arrival_batch":      "arrival batcher join (no-op when disabled)",
				"pre_upstream":       "construct upstream request + headers + client select",
				"upstream_connect":   "TCP connect via httptrace when a dial occurs",
				"upstream_ttfb":      "client.Do until response headers (includes dial + engine header delay)",
				"settlement_intent":  "residual wait for InsertRealtimeSettlementIntent before first buyer byte",
				"post_upstream":      "content-type check + response header assembly (intent wait excluded)",
				"upstream_first_sse": "wait inside proxySSE for first complete upstream SSE event after headers",
				"unmarked_residual":  "ttft_handler_ms − sum(named TTFT stages)",
				"merc_owned":         "auth_lookup + read_body + prepare_json + intake_control + authorize + admission + arrival_batch + pre_upstream + settlement_intent + post_upstream",
				"engine_facing":      "upstream_ttfb + upstream_connect + upstream_first_sse",
				"parity_overhead":    "merc_client_ttft_pX − direct_client_ttft_pX",
				"engine_excess":      "engine_facing_pX − direct_ttft_pX (positive ⇒ parity attributes engine time to Merc)",
			},
		},
		"cells": map[string]any{
			"c=1":  c1,
			"c=32": c32,
		},
		"accounting_table": buildAccountingTable(c1, c32),
		"p50_to_p50": map[string]any{
			"c1":  comparisonBlock(c1),
			"c32": comparisonBlock(c32),
		},
		"p95_to_p95": map[string]any{
			"c1":  comparisonBlockP95(c1),
			"c32": comparisonBlockP95(c32),
		},
		"concurrency_growth": map[string]any{
			"overhead_p50_growth_factor": safeDiv(asFloat(c32.Overhead["p50"]), asFloat(c1.Overhead["p50"])),
			"overhead_p95_growth_factor": safeDiv(asFloat(c32.Overhead["p95"]), asFloat(c1.Overhead["p95"])),
			"merc_owned_p50_growth":      safeDiv(asFloat(c32.MercOwned["p50"]), asFloat(c1.MercOwned["p50"])),
			"merc_owned_p95_growth":      safeDiv(asFloat(c32.MercOwned["p95"]), asFloat(c1.MercOwned["p95"])),
			"engine_facing_p50_growth":   safeDiv(asFloat(c32.EngineFacing["p50"]), asFloat(c1.EngineFacing["p50"])),
			"engine_facing_p95_growth":   safeDiv(asFloat(c32.EngineFacing["p95"]), asFloat(c1.EngineFacing["p95"])),
			"authorize_p50_c1":           stageP(c1, "authorize_contract", "p50"),
			"authorize_p95_c1":           stageP(c1, "authorize_contract", "p95"),
			"authorize_p50_c32":          stageP(c32, "authorize_contract", "p50"),
			"authorize_p95_c32":          stageP(c32, "authorize_contract", "p95"),
		},
		"prior_receipts": map[string]any{
			"segment_stub": map[string]any{
				"source":                "evidence/perf/merc-segment-latency-latest.json (stub upstream)",
				"c1_merc_added_p50_ms":  1.354,
				"c32_merc_added_p50_ms": 20.86,
				"note":                  "prior reported p50 only; p95 on last run c=1≈1.8–35, c=32≈97–127",
			},
			"parity_metal": map[string]any{
				"source":               "evidence/perf/gateway-parity-v2-local-metal.json",
				"c1_overhead_p50_ms":   3.909,
				"c1_overhead_p95_ms":   12.534,
				"c32_overhead_p50_ms":  33.668,
				"c32_overhead_p95_ms":  262.242,
			},
		},
		"ranked_attack_list": attackList,
		"verdict":            buildVerdict(c1, c32),
		"does_not_prove": []string{
			"that optimisations have been applied (this lane measures only)",
			"cross-host network RTT",
			"vLLM continuous-batching behaviour",
			"production multi-tenant load shape",
		},
		"can_prove": []string{
			"named attribution of merc-added TTFT at c=1 and c=32 against a real Metal engine",
			"p50-to-p50 and p95-to-p95 comparison of parity overhead vs path-timing stages",
			"whether the parity harness attributes engine queueing to Merc (engine_excess)",
			"which named segment grows with concurrency",
		},
	}

	dir := filepath.Join("..", "evidence", "perf")
	stamp := measuredAt.Format("20060102T150405Z")
	path := filepath.Join(dir, fmt.Sprintf("merc-latency-gap-accounting-%s.json", stamp))
	id, bin, err := DefaultBoundIdentity("..", "control/merc_latency_gap_accounting_test.go",
		"embedded method + cells + accounting_table", "embedded cell summaries; raw durations not retained")
	if err != nil {
		t.Fatal(err)
	}
	id.ModelArtifactDigest = IdentitySlotValue("3f5a22426976ab26cfe84dba63c1d08391717abb1af893e10f1b2968d862dcc1")
	if err := WriteBoundEvidenceJSON(EvidenceWriteRequest{
		RepoRoot: "..", Path: path, Payload: out,
		Identity: id, BuildBinaryPath: bin,
	}); err != nil {
		t.Fatal(err)
	}
	alias := filepath.Join(dir, "merc-latency-gap-accounting-latest.json")
	if err := WriteBoundEvidenceJSON(EvidenceWriteRequest{
		RepoRoot: "..", Path: alias, Payload: out,
		Identity: id, BuildBinaryPath: bin,
	}); err != nil {
		t.Fatal(err)
	}
	t.Logf("wrote bound receipt %s and %s", path, alias)
}

// --- helpers -----------------------------------------------------------------

func parsePathTimingLinesExtended(raw string) []pathTimingLineExt {
	var out []pathTimingLineExt
	for _, line := range strings.Split(raw, "\n") {
		if !strings.Contains(line, "realtime_path_timing") {
			continue
		}
		base := parsePathTimingLines(line)
		if len(base) == 0 {
			continue
		}
		ext := pathTimingLineExt{pathTimingLine: base[0]}
		if v, ok := scanFloatField(line, "ttft_handler_ms="); ok {
			ext.HandlerTTFTMS = v
			ext.HasTTFT = true
		}
		if v, ok := scanFloatField(line, "marked_ttft_sum_ms="); ok {
			ext.MarkedSumMS = v
		}
		if v, ok := scanFloatField(line, "unmarked_residual_ms="); ok {
			ext.ResidualMS = v
		}
		out = append(out, ext)
	}
	return out
}

func scanFloatField(line, key string) (float64, bool) {
	idx := strings.Index(line, key)
	if idx < 0 {
		return 0, false
	}
	var v float64
	if _, err := fmt.Sscanf(line[idx+len(key):], "%f", &v); err != nil {
		return 0, false
	}
	return v, true
}

func extendedFields(lines []pathTimingLineExt) (handler, marked, residual, auth []time.Duration) {
	for _, l := range lines {
		if l.HasTTFT {
			handler = append(handler, time.Duration(l.HandlerTTFTMS*float64(time.Millisecond)))
			marked = append(marked, time.Duration(l.MarkedSumMS*float64(time.Millisecond)))
			residual = append(residual, time.Duration(l.ResidualMS*float64(time.Millisecond)))
		}
		if v, ok := l.Stages["auth_lookup"]; ok {
			auth = append(auth, time.Duration(v*float64(time.Millisecond)))
		}
	}
	return
}

func sumStageSamplesExt(lines []pathTimingLineExt, keys []string) []time.Duration {
	out := make([]time.Duration, 0, len(lines))
	for _, l := range lines {
		var sum float64
		for _, k := range keys {
			sum += l.Stages[k]
		}
		out = append(out, time.Duration(sum*float64(time.Millisecond)))
	}
	return out
}

func gapDurationsOK(s []gapSamp) []time.Duration {
	out := make([]time.Duration, 0, len(s))
	for _, x := range s {
		if x.ok {
			out = append(out, x.ttft)
		}
	}
	return out
}

func gapCountOK(s []gapSamp) (ok, fail int) {
	for _, x := range s {
		if x.ok {
			ok++
		} else {
			fail++
		}
	}
	return
}

// pctDurMs returns nearest-rank percentile. p is in [0,1] (0.50 / 0.95 / 0.99),
// matching PercentileNearestRank / gateway-parity — NOT 0–100.
func pctDurMs(ds []time.Duration, p float64) float64 {
	if len(ds) == 0 {
		return math.NaN()
	}
	xs := make([]float64, len(ds))
	for i, d := range ds {
		xs[i] = float64(d.Microseconds()) / 1000.0
	}
	sort.Float64s(xs)
	return PercentileNearestRank(xs, p)
}

func meanDurMs(ds []time.Duration) float64 {
	if len(ds) == 0 {
		return math.NaN()
	}
	var s float64
	for _, d := range ds {
		s += float64(d.Microseconds()) / 1000.0
	}
	return s / float64(len(ds))
}

func summarizeDurMs(ds []time.Duration) map[string]any {
	if len(ds) == 0 {
		return map[string]any{"n": 0}
	}
	xs := make([]float64, len(ds))
	for i, d := range ds {
		xs[i] = float64(d.Microseconds()) / 1000.0
	}
	sort.Float64s(xs)
	var sum float64
	for _, v := range xs {
		sum += v
	}
	return map[string]any{
		"p50":  PercentileNearestRank(xs, 0.50),
		"p95":  PercentileNearestRank(xs, 0.95),
		"p99":  PercentileNearestRank(xs, 0.99),
		"min":  xs[0],
		"max":  xs[len(xs)-1],
		"mean": sum / float64(len(xs)),
		"n":    len(xs),
	}
}

func summarizeStageMapMs(stages map[string][]time.Duration) map[string]any {
	out := map[string]any{}
	for k, ds := range stages {
		out[k] = summarizeDurMs(ds)
	}
	return out
}

func asFloat(v any) float64 {
	switch x := v.(type) {
	case float64:
		return x
	case int:
		return float64(x)
	default:
		return math.NaN()
	}
}

func safeDiv(a, b float64) float64 {
	if b == 0 || math.IsNaN(b) || math.IsNaN(a) {
		return math.NaN()
	}
	return a / b
}

func stageP(c gapCellResult, stage, pct string) float64 {
	st, ok := c.Stages[stage]
	if !ok {
		return math.NaN()
	}
	m, ok := st.(map[string]any)
	if !ok {
		return math.NaN()
	}
	return asFloat(m[pct])
}

func comparisonBlock(c gapCellResult) map[string]any {
	return map[string]any{
		"merc_client_ttft_p50_ms":  c.Client["p50"],
		"direct_ttft_p50_ms":       c.Direct["p50"],
		"overhead_p50_ms":          c.Overhead["p50"],
		"merc_owned_p50_ms":        c.MercOwned["p50"],
		"engine_facing_p50_ms":     c.EngineFacing["p50"],
		"unmarked_residual_p50_ms": c.UnmarkedResidual["p50"],
		"attribution":              c.Attribution["p50_ms"],
	}
}

func comparisonBlockP95(c gapCellResult) map[string]any {
	return map[string]any{
		"merc_client_ttft_p95_ms":  c.Client["p95"],
		"direct_ttft_p95_ms":       c.Direct["p95"],
		"overhead_p95_ms":          c.Overhead["p95"],
		"merc_owned_p95_ms":        c.MercOwned["p95"],
		"engine_facing_p95_ms":     c.EngineFacing["p95"],
		"unmarked_residual_p95_ms": c.UnmarkedResidual["p95"],
		"attribution":              c.Attribution["p95_ms"],
	}
}

func buildAccountingTable(c1, c32 gapCellResult) map[string]any {
	row := func(c gapCellResult) map[string]any {
		stages := []string{
			"auth_lookup", "read_body", "prepare_json", "intake_control",
			"authorize_contract", "admission_event", "arrival_batch",
			"pre_upstream", "settlement_intent", "post_upstream",
			"upstream_ttfb", "upstream_connect", "upstream_first_sse",
		}
		named := map[string]any{}
		for _, s := range stages {
			named[s] = c.Stages[s]
		}
		return map[string]any{
			"client_ttft":       c.Client,
			"direct_ttft":       c.Direct,
			"parity_overhead":   c.Overhead,
			"named_stages":      named,
			"merc_owned_sum":    c.MercOwned,
			"engine_facing_sum": c.EngineFacing,
			"handler_ttft":      c.HandlerTTFT,
			"marked_ttft_sum":   c.MarkedSum,
			"unmarked_residual": c.UnmarkedResidual,
			"attribution":       c.Attribution,
		}
	}
	return map[string]any{"c=1": row(c1), "c=32": row(c32)}
}

func engineMisattributionVerdict(engExP50, engExP95, ohP50, ohP95 float64) string {
	frac := func(ex, oh float64) float64 {
		if oh <= 0 || math.IsNaN(oh) || math.IsNaN(ex) {
			return math.NaN()
		}
		return ex / oh
	}
	f50, f95 := frac(engExP50, ohP50), frac(engExP95, ohP95)
	switch {
	case engExP95 > 0 && f95 >= 0.5:
		return fmt.Sprintf("YES — engine_excess is ≥50%% of parity overhead at p95 (excess=%.2f ms, overhead=%.2f ms, frac=%.2f). Parity is over-attributing engine time to Merc.", engExP95, ohP95, f95)
	case engExP50 > 0 && f50 >= 0.35:
		return fmt.Sprintf("PARTIAL — engine_excess is a large share of p50 overhead (excess=%.2f ms, overhead=%.2f ms, frac=%.2f).", engExP50, ohP50, f50)
	case engExP95 > 5 && engExP95 > 0:
		return fmt.Sprintf("MILD — engine_excess p95=%.2f ms is positive but <50%% of overhead p95=%.2f ms (frac=%.2f).", engExP95, ohP95, f95)
	case engExP95 <= 0 && engExP50 <= 0:
		return "NO — engine_facing on merc ≤ direct TTFT at both p50 and p95; parity overhead is not engine backlog misattribution."
	default:
		return fmt.Sprintf("INCONCLUSIVE — engine_excess p50=%.2f p95=%.2f; overhead p50=%.2f p95=%.2f.", engExP50, engExP95, ohP50, ohP95)
	}
}

func buildAttackList(c1, c32 gapCellResult) []map[string]any {
	type item struct {
		name   string
		why    string
		p50c1  float64
		p95c1  float64
		p50c32 float64
		p95c32 float64
		score  float64
	}
	var candidates []item
	for _, stage := range []string{
		"authorize_contract", "admission_event", "settlement_intent",
		"intake_control", "auth_lookup", "pre_upstream", "post_upstream",
		"upstream_ttfb", "upstream_first_sse",
	} {
		p95c32 := stageP(c32, stage, "p95")
		if math.IsNaN(p95c32) {
			continue
		}
		kind := "merc_owned"
		if stage == "upstream_ttfb" || stage == "upstream_first_sse" || stage == "upstream_connect" {
			kind = "engine_facing"
		}
		candidates = append(candidates, item{
			name: stage, why: kind,
			p50c1: stageP(c1, stage, "p50"), p95c1: stageP(c1, stage, "p95"),
			p50c32: stageP(c32, stage, "p50"), p95c32: p95c32,
			score: p95c32,
		})
	}
	if attr, ok := c32.Attribution["p95_ms"].(map[string]any); ok {
		ex := asFloat(attr["engine_excess_vs_direct"])
		if ex > 10 {
			a50 := c1.Attribution["p50_ms"].(map[string]any)
			a95 := c1.Attribution["p95_ms"].(map[string]any)
			candidates = append(candidates, item{
				name: "parity_engine_misattribution",
				why:  "measurement_defect: engine_excess counted as Merc overhead by merc−direct TTFT",
				p50c1: asFloat(a50["engine_excess_vs_direct"]), p95c1: asFloat(a95["engine_excess_vs_direct"]),
				p50c32: asFloat(c32.Attribution["p50_ms"].(map[string]any)["engine_excess_vs_direct"]),
				p95c32: ex,
				score:  ex + 1000,
			})
		}
	}
	sort.Slice(candidates, func(i, j int) bool { return candidates[i].score > candidates[j].score })
	out := make([]map[string]any, 0, len(candidates))
	for i, it := range candidates {
		out = append(out, map[string]any{
			"rank":            i + 1,
			"target":          it.name,
			"class":           it.why,
			"c1_p50_ms":       it.p50c1,
			"c1_p95_ms":       it.p95c1,
			"c32_p50_ms":      it.p50c32,
			"c32_p95_ms":      it.p95c32,
			"attack_priority": it.score,
		})
	}
	return out
}

func buildVerdict(c1, c32 gapCellResult) map[string]any {
	return map[string]any{
		"c1_engine_misattribution":  c1.Attribution["engine_misattribution_verdict"],
		"c32_engine_misattribution": c32.Attribution["engine_misattribution_verdict"],
		"summary": "See accounting_table and ranked_attack_list. If engine_excess dominates " +
			"parity overhead, do not optimise Merc control-plane for those milliseconds — " +
			"fix the measurement or the engine saturation shape first.",
	}
}

// measureClientTTFTDirect posts to an engine base URL that already includes /v1.
func measureClientTTFTDirect(client *http.Client, baseURL, apiKey string, body []byte) (time.Duration, error) {
	url := strings.TrimRight(baseURL, "/") + "/chat/completions"
	req, err := http.NewRequest(http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return 0, err
	}
	req.Header.Set("Authorization", "Bearer "+apiKey)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Connection", "keep-alive")
	start := time.Now()
	resp, err := client.Do(req)
	if err != nil {
		return time.Since(start), err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return time.Since(start), fmt.Errorf("http %d: %s", resp.StatusCode, bytes.TrimSpace(b))
	}
	sc := bufio.NewScanner(resp.Body)
	sc.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	var ttft time.Duration
	ttftSet := false
	for sc.Scan() {
		line := sc.Text()
		if !strings.HasPrefix(line, "data: ") {
			continue
		}
		payload := strings.TrimSpace(line[6:])
		if payload == "[DONE]" {
			break
		}
		if strings.Contains(payload, `"content"`) || strings.Contains(payload, `"choices"`) {
			if !ttftSet {
				ttft = time.Since(start)
				ttftSet = true
			}
		}
	}
	if !ttftSet {
		return time.Since(start), fmt.Errorf("no SSE data line observed")
	}
	_, _ = io.Copy(io.Discard, resp.Body)
	return ttft, sc.Err()
}
