package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"math"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
)

// TestMercSegmentLatencyMeasure is the repeatable per-segment latency harness
// for Merc-owned control-plane work on the real streaming path against a stub
// upstream. Opt-in only:
//
//	MERC_SEGMENT_LATENCY_MEASURE=1 \
//	MERC_TEST_DATABASE_URL=postgres://cx:cx@localhost:5432/cx?sslmode=disable \
//	go test -count=1 -run TestMercSegmentLatencyMeasure -timeout 30m ./control
//
// It does NOT change production code paths. It times path-timing stages
// (MERC_REALTIME_PATH_TIMING=1), AuthorizeRealtimeContract under concurrency,
// an authorize decomposition probe that mirrors the durable segments the
// c=1 floor claim depends on, and FinalizeRealtimeSuccess.
//
// segmentRunCell is one (run × concurrency) measurement cell.
type segmentRunCell struct {
	Run                int                              `json:"run"`
	Concurrency        int                              `json:"concurrency"`
	Samples            int                              `json:"samples"`
	Warmup             int                              `json:"warmup"`
	E2EOK              int                              `json:"e2e_ok"`
	E2EFail            int                              `json:"e2e_fail"`
	AuthOK             int                              `json:"authorize_ok"`
	AuthFail           int                              `json:"authorize_fail"`
	ClientTTFT         segmentLatencySummary            `json:"client_ttft_ms"`
	MercAdded          segmentLatencySummary            `json:"merc_added_ttft_ms"`
	Stages             map[string]segmentLatencySummary `json:"stages_ms"`
	AuthorizeAdmission segmentLatencySummary            `json:"authorize_admission_ms"`
	AuthorizeDecomp    map[string]segmentLatencySummary `json:"authorize_decomposition_ms"`
	SettlementFinalize segmentLatencySummary            `json:"settlement_finalize_ms"`
	SettlementPath     segmentLatencySummary            `json:"settlement_path_ms"`
	// E2E concurrent arm uses one buyer per in-flight slot so buyer funding
	// locks do not ABBA-deadlock with the single offer row during settlement.
	// Authorize concurrent arm intentionally uses ONE buyer (same-buyer
	// serialization is part of what is being measured).
	E2EBuyerMode string `json:"e2e_buyer_mode"`
}

// Bound receipt: evidence/perf/merc-segment-latency-*.json (and -latest).
func TestMercSegmentLatencyMeasure(t *testing.T) {
	if os.Getenv("MERC_SEGMENT_LATENCY_MEASURE") != "1" {
		t.Skip("set MERC_SEGMENT_LATENCY_MEASURE=1 to run the segment latency harness")
	}
	installSettlementCurrencyForTest(t, "usd")
	t.Setenv("MERC_TOKEN_KEY", "merc-segment-latency-key-with-at-least-32-bytes!")
	t.Setenv("STRIPE_SECRET_KEY", "")
	t.Setenv("MERC_CANARY_ENABLED", "false")
	t.Setenv("MERC_REALTIME_PATH_TIMING", "1")

	ctx, store, pool := openIsolatedTestStore(t)
	profile := sortedVLLMProfiles()[0]

	const (
		samplesPerCell = 60
		warmupPerCell  = 10
		// Independent full passes so p50 has a run-to-run variance estimate.
		independentRuns = 3
		offerCapacity   = 50_000
	)
	concurrencies := []int{1, 8, 32}

	// Stub upstream: honest about being a stub. Instant SSE with reconcilable
	// usage so the full stream path (settlement_intent + proxy_sse + settlement)
	// completes. No model engine, no paid cloud.
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/chat/completions" {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		flusher, _ := w.(http.Flusher)
		for _, event := range []string{
			`data: {"id":"chatcmpl_stub","object":"chat.completion.chunk","created":1,"model":"cx-chat-1b","choices":[{"index":0,"delta":{"content":"hi"},"finish_reason":null}],"usage":null}` + "\n\n",
			`data: {"id":"chatcmpl_stub","object":"chat.completion.chunk","created":1,"model":"cx-chat-1b","choices":[],"usage":{"prompt_tokens":7,"completion_tokens":2,"total_tokens":9}}` + "\n\n",
			"data: [DONE]\n\n",
		} {
			_, _ = io.WriteString(w, event)
			if flusher != nil {
				flusher.Flush()
			}
		}
	}))
	t.Cleanup(upstream.Close)

	// Primary buyer: used for authorize concurrent (same-buyer serialization)
	// and for serial decomp/settlement probes.
	buyerID, err := store.CreateBuyerAccount(ctx,
		"seg-lat-"+uuid.NewString()+"@example.test", "integration-password", 100_000)
	if err != nil {
		t.Fatal(err)
	}
	_, buyerKey, _, err := store.CreateAPIKey(ctx, buyerID, "segment latency", true)
	if err != nil {
		t.Fatal(err)
	}
	// E2E concurrent buyers: one per max concurrency slot. Concurrent full-path
	// streams with a single buyer deadlocked (offer row lock vs buyer funding
	// lock vs settlement) in pilot runs; multi-buyer keeps the measurement of
	// offer-row serialization without the ABBA deadlock. Documented as a
	// method choice and as a production defect observation under single-buyer.
	const maxE2EBuyers = 32
	e2eKeys := make([]string, maxE2EBuyers)
	e2eKeys[0] = buyerKey
	for i := 1; i < maxE2EBuyers; i++ {
		id, cerr := store.CreateBuyerAccount(ctx,
			fmt.Sprintf("seg-lat-e2e-%d-%s@example.test", i, uuid.NewString()[:8]),
			"integration-password", 100_000)
		if cerr != nil {
			t.Fatal(cerr)
		}
		_, key, _, kerr := store.CreateAPIKey(ctx, id, "segment e2e", true)
		if kerr != nil {
			t.Fatal(kerr)
		}
		e2eKeys[i] = key
	}
	supplierID, workerID := uuid.New(), uuid.New()
	if _, err := pool.Exec(ctx, `INSERT INTO suppliers (id,email,status) VALUES ($1,$2,'active')`,
		supplierID, "seg-lat-supplier-"+uuid.NewString()+"@example.test"); err != nil {
		t.Fatal(err)
	}
	if _, err := store.CreateWorkerToken(ctx, workerID, supplierID); err != nil {
		t.Fatal(err)
	}
	worker := WorkerAuth{WorkerID: workerID, SupplierID: supplierID}
	if err := store.UpsertRealtimeOffer(ctx, worker, RealtimeOfferRegistration{
		RuntimeProfileID: profile.RuntimeProfileID, RuntimeProfileSHA256: profile.ProfileSHA256,
		HWClass: "nvidia_24gb", GPUCount: 1, MemoryGBPerGPU: 24,
		UpstreamBaseURL: upstream.URL + "/v1", UpstreamToken: "seg-lat-stub-token",
		Warmth: "HOT", MaxActiveSequences: offerCapacity, AvailableSequences: offerCapacity,
		SupplierInputUSDPerMillionTokens: 0.08, SupplierOutputUSDPerMillionTokens: 0.30,
	}); err != nil {
		t.Fatal(err)
	}
	// Keep last_seen fresh across the measurement window.
	refresh := time.NewTicker(8 * time.Second)
	t.Cleanup(refresh.Stop)
	go func() {
		for range refresh.C {
			c, cancel := context.WithTimeout(context.Background(), 4*time.Second)
			_ = store.HeartbeatRealtimeOffer(c, worker, RealtimeOfferHeartbeat{
				RuntimeProfileID: profile.RuntimeProfileID, Warmth: "HOT",
				AvailableSequences: offerCapacity, Status: "ACTIVE",
			})
			cancel()
		}
	}()

	server := httptest.NewServer(NewServer(store, nil, nil, nil).Routes())
	t.Cleanup(server.Close)

	// Capture structured path-timing lines without swallowing other logs.
	// logWriter is safe for concurrent Write/Reset/String from request
	// handlers and the measuring goroutine.
	logWriter := &syncBuffer{}
	prevWriter := log.Writer()
	log.SetOutput(io.MultiWriter(prevWriter, logWriter))
	t.Cleanup(func() { log.SetOutput(prevWriter) })

	requestBody := []byte(`{"model":"cx-chat-1b","messages":[{"role":"user","content":"say hello"}],"stream":true,"max_tokens":8,"stream_options":{"include_usage":true}}`)

	var allCells []segmentRunCell
	machineQuiet := machineLoadQuiet()
	host, _ := os.Hostname()
	measuredAt := time.Now().UTC()

	// Shared client with keep-alive so we do not measure dial tax as merc cost.
	httpClient := &http.Client{
		Timeout: 60 * time.Second,
		Transport: &http.Transport{
			MaxIdleConns:        256,
			MaxIdleConnsPerHost: 256,
			IdleConnTimeout:     90 * time.Second,
		},
	}

	for run := 1; run <= independentRuns; run++ {
		t.Logf("=== independent run %d/%d ===", run, independentRuns)
		for _, c := range concurrencies {
			// Replenish offer capacity between cells.
			if _, err := pool.Exec(ctx, `
				UPDATE realtime_worker_offers
				   SET available_sequences=max_active_sequences, status='ACTIVE', last_seen_at=now()
				 WHERE worker_id=$1 AND runtime_profile_id=$2`,
				workerID, profile.RuntimeProfileID); err != nil {
				t.Fatal(err)
			}

			// Clear path-timing capture for this cell.
			logWriter.Reset()

			// Warmup (serial) so cold caches do not dominate the first sample.
			for i := 0; i < warmupPerCell; i++ {
				if err := doSegmentStreamRequest(httpClient, server.URL, buyerKey, requestBody); err != nil {
					t.Fatalf("warmup c=%d: %v", c, err)
				}
			}
			logWriter.Reset() // discard warmup path-timing lines

			// E2E streaming wave: client TTFT + path stages.
			// Multi-buyer: worker i uses e2eKeys[i%c] so concurrent streams do
			// not share the buyer funding advisory lock.
			type e2eSample struct {
				clientTTFT time.Duration
				ok         bool
				errText    string
			}
			e2eSamples := make([]e2eSample, samplesPerCell)
			var wg sync.WaitGroup
			jobs := make(chan int)
			for i := 0; i < c; i++ {
				workerIdx := i
				wg.Add(1)
				go func() {
					defer wg.Done()
					key := e2eKeys[workerIdx%len(e2eKeys)]
					for idx := range jobs {
						ttft, err := measureClientTTFT(httpClient, server.URL, key, requestBody)
						if err != nil {
							e2eSamples[idx] = e2eSample{clientTTFT: ttft, ok: false, errText: err.Error()}
							continue
						}
						e2eSamples[idx] = e2eSample{clientTTFT: ttft, ok: true}
					}
				}()
			}
			for i := 0; i < samplesPerCell; i++ {
				jobs <- i
			}
			close(jobs)
			wg.Wait()

			// Drain a moment so late path-timing logs land.
			time.Sleep(80 * time.Millisecond)
			timingLines := parsePathTimingLines(logWriter.String())

			clientTTFT := make([]time.Duration, 0, samplesPerCell)
			e2eOK, e2eFail := 0, 0
			for _, s := range e2eSamples {
				if s.ok {
					e2eOK++
					clientTTFT = append(clientTTFT, s.clientTTFT)
				} else {
					e2eFail++
				}
			}
			if e2eOK == 0 {
				t.Fatalf("e2e c=%d: zero successful streams (fail=%d); first err: %s",
					c, e2eFail, e2eSamples[0].errText)
			}
			if e2eFail > 0 {
				t.Logf("e2e c=%d: ok=%d fail=%d (failures recorded, not fatal unless zero ok)", c, e2eOK, e2eFail)
			}
			stageSamples := aggregateStageSamples(timingLines)
			mercAdded := mercAddedTTFTSamples(timingLines)

			// AuthorizeRealtimeContract concurrent wave (void via failure).
			// SINGLE buyer — same-buyer funding serialization is intentional.
			// Ceilings must match PricingDecision projection exactly.
			authMaxUSD, authEstUSD, authMaxPrompt, authMaxCompletion := realtimeAuthCeiling(t, profile, 7, 2)
			type authSample struct {
				d  time.Duration
				ok bool
			}
			authRaw := make([]authSample, samplesPerCell)
			var authWG sync.WaitGroup
			authJobs := make(chan int)
			for i := 0; i < c; i++ {
				authWG.Add(1)
				go func() {
					defer authWG.Done()
					for idx := range authJobs {
						start := time.Now()
						contract, _, aerr := store.AuthorizeRealtimeContract(context.Background(), RealtimeContractAuthorization{
							RequestID: "seg-auth-" + uuid.NewString(), BuyerID: buyerID, Profile: profile,
							InputCommitment: strings.Repeat("a", 64), RequestSHA256: strings.Repeat("b", 64),
							MaximumPriceUSD: authMaxUSD, EstimatedPriceUSD: authEstUSD, DeadlineAt: time.Now().Add(time.Minute),
							MaximumPromptTokens: authMaxPrompt, MaximumCompletionTokens: authMaxCompletion,
							EstimatedPromptTokens: 7, EstimatedCompletionTokens: 2,
						})
						elapsed := time.Since(start)
						if aerr != nil {
							authRaw[idx] = authSample{d: elapsed, ok: false}
							continue
						}
						_, _ = store.FinalizeRealtimeFailure(context.Background(), contract.ID, uuid.New(), 500, 1,
							"segment_latency_probe", "probe teardown", false)
						authRaw[idx] = authSample{d: elapsed, ok: true}
					}
				}()
			}
			for i := 0; i < samplesPerCell; i++ {
				authJobs <- i
			}
			close(authJobs)
			authWG.Wait()
			authSamples := make([]time.Duration, 0, samplesPerCell)
			authOK, authFail := 0, 0
			for _, s := range authRaw {
				if s.ok {
					authOK++
					authSamples = append(authSamples, s.d)
				} else {
					authFail++
				}
			}
			if authOK == 0 {
				t.Fatalf("authorize c=%d: zero successes (fail=%d)", c, authFail)
			}
			if authFail > 0 {
				t.Logf("authorize c=%d: ok=%d fail=%d", c, authOK, authFail)
			}

			// Authorize decomposition (serial; concurrency would interleave lock hold).
			// At c>1 we still report decomposition at serial load — the floor claim
			// is a c=1 durable-work argument. Under c=8/32 the concurrent authorize
			// numbers capture contention; the decomp remains the floor anatomy.
			decompN := samplesPerCell
			if c > 1 {
				// Fewer serial decomp samples under high-c cells to keep wall time sane.
				decompN = 30
			}
			decomp := measureAuthorizeDecomposition(t, store, pool, buyerID, profile, decompN)

			// Isolated FinalizeRealtimeSuccess (serial).
			settleSamples := measureSettlementFinalize(t, store, buyerID, profile, decompN)

			cell := segmentRunCell{
				Run:                run,
				Concurrency:        c,
				Samples:            samplesPerCell,
				Warmup:             warmupPerCell,
				E2EOK:              e2eOK,
				E2EFail:            e2eFail,
				AuthOK:             authOK,
				AuthFail:           authFail,
				ClientTTFT:         summarizeSegmentLatency(clientTTFT),
				MercAdded:          summarizeSegmentLatency(mercAdded),
				Stages:             summarizeStageMap(stageSamples),
				AuthorizeAdmission: summarizeSegmentLatency(authSamples),
				AuthorizeDecomp:    summarizeStageMap(decomp),
				SettlementFinalize: summarizeSegmentLatency(settleSamples),
				SettlementPath:     summarizeSegmentLatency(stageSamples["settlement"]),
				E2EBuyerMode:       "one_buyer_per_inflight_slot",
			}
			allCells = append(allCells, cell)
			t.Logf("run=%d c=%d e2e_ok=%d/%d client_ttft p50=%.3f merc_added p50=%.3f authorize p50=%.3f (ok=%d) settlement p50=%.3f path_lines=%d",
				run, c, e2eOK, samplesPerCell, cell.ClientTTFT.P50, cell.MercAdded.P50, cell.AuthorizeAdmission.P50,
				authOK, cell.SettlementFinalize.P50, len(timingLines))
		}
	}

	// Aggregate across independent runs: median-of-p50s and run-to-run range.
	aggregated := aggregateAcrossRuns(allCells, concurrencies)

	// Compare to the unciteable wave numbers (not authority — just delta table).
	prior := map[string]any{
		"note": "unciteable wave numbers from the programme gap statement; no producer identity",
		"merc_added_ttft_p50_ms": map[string]float64{"c1": 1.84, "c8": 15.66, "c32": 65.50},
		"merc_added_ttft_p95_ms_c1": 2.49,
		"authorize_p50_ms": map[string]float64{
			"c1": 1.35, "c8": 16.18, "c32": 114.33, "c64": 88.92, "c128": 62.31,
		},
		"c1_stage_p50_ms": map[string]float64{
			"authorize": 1.219, "admission_event": 0.160, "settlement_intent": 0.122,
			"intake_control": 0.045, "prepare_json": 0.018, "proxy_sse": 0.037, "read_body": 0.003,
		},
		"settlement_finalize_p50_ms": 1.652,
		"floor_claim_c1_ms": map[string]float64{
			"funding": 0.20, "offer_claim": 0.20, "pricing": 0.05,
			"contract_insert": 0.35, "event_insert": 0.10, "commit": 0.15, "sum": 1.0,
		},
		"floor_range_ms": []float64{0.7, 1.2},
	}

	// Floor verdict from measured c=1 decomp (median across runs).
	floorVerdict := judgeFloorClaim(aggregated)

	loadAvg, loadN := readLoadAverage()
	out := map[string]any{
		"schema_version": 1,
		"kind":           "merc_segment_latency_receipt",
		"measured_at":    measuredAt.Format(time.RFC3339),
		"method": map[string]any{
			"what": "Per-segment wall time of Merc-owned control-plane work on the real " +
				"streaming chat-completions path against a stub upstream, plus an " +
				"AuthorizeRealtimeContract concurrent wave and a durable authorize " +
				"decomposition probe.",
			"stub_upstream": map[string]any{
				"kind":        "httptest SSE stand-in",
				"behaviour":   "returns two SSE chunks + [DONE] with reconcilable usage immediately; no model inference",
				"honesty":     "this is a STUB. Receipt measures Merc-owned control-plane latency, not engine latency.",
				"base_url":    upstream.URL + "/v1",
				"token_shape": "static test token (not a live engine credential)",
			},
			"control_plane": map[string]any{
				"server":             "httptest wrapping NewServer(store).Routes()",
				"database":           "isolated Postgres created by openIsolatedTestStore (not the shared suite DB)",
				"path_timing":        "MERC_REALTIME_PATH_TIMING=1 structured log lines parsed per sample",
				"offer_capacity":     offerCapacity,
				"offer_count":        1,
				"buyer_funding_authorize_arm": "single prepaid free-credit buyer (same-buyer serialization is the subject)",
				"buyer_funding_e2e_arm":       "one prepaid buyer per in-flight slot (avoids ABBA deadlock with offer row during concurrent settlement)",
				"observed_defect": "single-buyer concurrent e2e streams (c>=8) produced PostgreSQL deadlocks " +
					"(SQLSTATE 40P01) between authorize (offer row then buyer funding) and settlement in pilot runs; " +
					"reported, not fixed in this measurement lane",
				"arrival_batcher": "nil (NewServer default) — arrival_batch stage is a no-op mark",
			},
			"segment_boundaries": map[string]any{
				"read_body":          "io.ReadAll of request body until mark; excludes auth middleware",
				"prepare_json":       "prepareRealtimeRequest JSON parse + profile bind + token ceilings",
				"intake_control":     "OperationalControlPaused(controlIntake) DB read",
				"authorize_contract": "full AuthorizeRealtimeContract transaction (path timing stage)",
				"admission_event":    "recordRealtimeAdmissionEvent (placement verify + insert)",
				"settlement_intent":  "InsertRealtimeSettlementIntent after upstream first byte, before buyer first byte",
				"proxy_sse":          "proxySSE relay of stub events (includes per-event sha256 chain); stub engine is near-zero so this is mostly Merc hash/parse",
				"settlement":         "FinalizeRealtimeSuccess after full stream delivery (NOT on client TTFT path)",
				"upstream_ttfb":      "httptest dial + stub header/first-byte; EXCLUDED from merc_added_ttft",
				"upstream_connect":   "TCP connect duration via httptrace when dial occurs",
				"client_ttft":        "client wall from request write start to first SSE 'data:' line",
				"merc_added_ttft": "sum of path-timing stages on the client TTFT path excluding " +
					"upstream_ttfb and upstream_connect: read_body + prepare_json + intake_control + " +
					"authorize_contract + admission_event + arrival_batch + settlement_intent. " +
					"proxy_sse is after first client byte and is reported separately.",
				"authorize_admission_ms": "separate concurrent wave timing AuthorizeRealtimeContract only; " +
					"voided via FinalizeRealtimeFailure (settlement work not included)",
				"authorize_decomposition": map[string]any{
					"order": "production order after late funding lock: begin → offer_claim → " +
						"pricing → funding → contract_insert → event_insert → commit",
					"offer_claim":     "realtimeAuthorizeSelectOfferSQL (atomic capacity decrement + rank)",
					"pricing":         "decode placement + newRealtimePricingDecision + digests + market receipt (in-process + small allocs)",
					"funding":         "evaluateRealtimeBuyerFunding (advisory xact lock + multi-aggregate FOR UPDATE batch)",
					"contract_insert": "INSERT execution_contracts (triggers fire; sequential for component timing)",
					"event_insert":    "INSERT realtime_authorization_events RESERVED (sequential for component timing)",
					"commit":          "tx.Commit durable flush",
					"note": "Production batches contract_insert + event_insert in one pgx.Batch; " +
						"this probe times them sequentially so the floor anatomy is visible. " +
						"Sum of sequential components is therefore a slight over-estimate of " +
						"batched RTT but matches the durable statement work the floor claims.",
				},
				"settlement_finalize_ms": "AuthorizeRealtimeContract then FinalizeRealtimeSuccess with " +
					"synthetic reconcilable evidence; times only FinalizeRealtimeSuccess",
			},
			"samples_per_cell":     samplesPerCell,
			"warmup_per_cell":      warmupPerCell,
			"independent_runs":     independentRuns,
			"concurrency_levels":   concurrencies,
			"percentile_method":    "nearest-rank: ceil(p*n)-1 on sorted samples (same as realtime_auth_latency_probe)",
			"run_to_run_variance":  "each independent run recomputes p50; aggregated.p50_across_runs is the median of those p50s; p50_run_min/max and p50_run_stdev report spread",
			"machine": map[string]any{
				"hostname":     host,
				"goos":         runtime.GOOS,
				"goarch":       runtime.GOARCH,
				"num_cpu":      runtime.NumCPU(),
				"load_average": loadAvg,
				"load_n":       loadN,
				"quiet":        machineQuiet,
				"quiet_reason": quietReason(machineQuiet, loadAvg, loadN),
			},
		},
		"does_not_prove": []string{
			"engine / model inference latency (upstream is an httptest stub with zero generation work)",
			"GPU / CUDA / Metal / vLLM / llama-server TTFT or ITL",
			"cross-host network RTT to a real worker",
			"Stripe, object-store, or any paid-cloud path",
			"that avoidable Merc latency is 10x lower than any prior baseline",
			"that multi-offer capacity removes the offer-row serialisation at c=32",
			"production load-shape tail latency under multi-tenant contention",
		},
		"can_prove": []string{
			"Merc-owned stage wall times on the streaming path with a stub upstream",
			"AuthorizeRealtimeContract p50/p95/p99 at c=1/8/32 on this machine and Postgres",
			"durable authorize segment anatomy (funding, offer claim, pricing, inserts, commit)",
			"whether the published c=1 floor range of roughly 0.7-1.2 ms is consistent with a re-run",
			"run-to-run variance of the p50 estimates under the stated load average",
		},
		"prior_unciteable_numbers": prior,
		"cells":                    allCells,
		"aggregated":               aggregated,
		"floor_claim_verdict":      floorVerdict,
		"deltas_vs_prior":          deltasVsPrior(aggregated, prior),
	}

	dir := filepath.Join("..", "evidence", "perf")
	stamp := measuredAt.Format("20060102T150405Z")
	path := filepath.Join(dir, fmt.Sprintf("merc-segment-latency-%s.json", stamp))
	id, bin, err := DefaultBoundIdentity("..", "control/merc_segment_latency_measure_test.go",
		"embedded method + cells + aggregated", "embedded cells[] samples summarised; raw durations not retained")
	if err != nil {
		t.Fatal(err)
	}
	if err := WriteBoundEvidenceJSON(EvidenceWriteRequest{
		RepoRoot: "..", Path: path, Payload: out,
		Identity: id, BuildBinaryPath: bin,
	}); err != nil {
		t.Fatal(err)
	}
	alias := filepath.Join(dir, "merc-segment-latency-latest.json")
	if err := WriteBoundEvidenceJSON(EvidenceWriteRequest{
		RepoRoot: "..", Path: alias, Payload: out,
		Identity: id, BuildBinaryPath: bin,
	}); err != nil {
		t.Fatal(err)
	}
	t.Logf("wrote bound receipt %s", path)
	t.Logf("floor verdict: %s", floorVerdict["summary"])
}

// --- measurement helpers ----------------------------------------------------

// syncBuffer is a mutex-guarded bytes.Buffer for concurrent log capture.
type syncBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (s *syncBuffer) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.Write(p)
}

func (s *syncBuffer) Reset() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.b.Reset()
}

func (s *syncBuffer) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.String()
}

func doSegmentStreamRequest(client *http.Client, base, buyerKey string, body []byte) error {
	_, err := measureClientTTFT(client, base, buyerKey, body)
	return err
}

// measureClientTTFT posts a streaming completion and returns time to first SSE
// data line (client-visible TTFT).
func measureClientTTFT(client *http.Client, base, buyerKey string, body []byte) (time.Duration, error) {
	req, err := http.NewRequest(http.MethodPost, base+"/v1/chat/completions", bytes.NewReader(body))
	if err != nil {
		return 0, err
	}
	req.Header.Set("Authorization", "Bearer "+buyerKey)
	req.Header.Set("Content-Type", "application/json")
	start := time.Now()
	resp, err := client.Do(req)
	if err != nil {
		return time.Since(start), err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 4<<10))
		return time.Since(start), fmt.Errorf("status=%d body=%s", resp.StatusCode, b)
	}
	reader := bufio.NewReader(resp.Body)
	var ttft time.Duration
	ttftSet := false
	for {
		line, err := reader.ReadString('\n')
		if !ttftSet && strings.HasPrefix(line, "data:") {
			ttft = time.Since(start)
			ttftSet = true
		}
		if strings.Contains(line, "[DONE]") {
			// Drain remainder.
			_, _ = io.Copy(io.Discard, reader)
			break
		}
		if err == io.EOF {
			break
		}
		if err != nil {
			if ttftSet {
				return ttft, nil
			}
			return time.Since(start), err
		}
	}
	if !ttftSet {
		return time.Since(start), fmt.Errorf("no SSE data line observed")
	}
	return ttft, nil
}

type pathTimingLine struct {
	TotalMS float64
	Stages  map[string]float64 // ms
}

func parsePathTimingLines(raw string) []pathTimingLine {
	var out []pathTimingLine
	for _, line := range strings.Split(raw, "\n") {
		if !strings.Contains(line, "realtime_path_timing") {
			continue
		}
		// stages_ms=a=1.2,b=3.4
		idx := strings.Index(line, "stages_ms=")
		if idx < 0 {
			continue
		}
		stagesPart := line[idx+len("stages_ms="):]
		// Trim trailing noise after the stages field (none today, but be safe).
		if sp := strings.IndexAny(stagesPart, " \t"); sp >= 0 {
			stagesPart = stagesPart[:sp]
		}
		stages := map[string]float64{}
		for _, part := range strings.Split(stagesPart, ",") {
			kv := strings.SplitN(part, "=", 2)
			if len(kv) != 2 {
				continue
			}
			var v float64
			if _, err := fmt.Sscanf(kv[1], "%f", &v); err == nil {
				stages[kv[0]] = v
			}
		}
		var total float64
		if tIdx := strings.Index(line, "total_ms="); tIdx >= 0 {
			fmt.Sscanf(line[tIdx+len("total_ms="):], "%f", &total)
		}
		out = append(out, pathTimingLine{TotalMS: total, Stages: stages})
	}
	return out
}

func aggregateStageSamples(lines []pathTimingLine) map[string][]time.Duration {
	out := map[string][]time.Duration{}
	for _, line := range lines {
		for k, ms := range line.Stages {
			out[k] = append(out[k], time.Duration(ms*float64(time.Millisecond)))
		}
	}
	return out
}

// mercAddedTTFTSamples sums Merc-owned stages on the TTFT path per path-timing line.
func mercAddedTTFTSamples(lines []pathTimingLine) []time.Duration {
	// Stages before (or gating) the buyer's first byte, excluding stub upstream.
	keys := []string{
		"read_body", "prepare_json", "intake_control", "exact_reuse", "coalesce",
		"authorize_contract", "admission_event", "arrival_batch", "settlement_intent",
	}
	out := make([]time.Duration, 0, len(lines))
	for _, line := range lines {
		var sum float64
		for _, k := range keys {
			sum += line.Stages[k]
		}
		out = append(out, time.Duration(sum*float64(time.Millisecond)))
	}
	return out
}

func measureAuthorizeDecomposition(
	t *testing.T, store *Store, pool *pgxpool.Pool,
	buyerID uuid.UUID, profile VLLMRuntimeProfile, n int,
) map[string][]time.Duration {
	t.Helper()
	out := map[string][]time.Duration{
		"begin": {}, "offer_claim": {}, "pricing": {}, "funding": {},
		"contract_insert": {}, "event_insert": {}, "commit": {}, "total": {},
	}
	for i := 0; i < n; i++ {
		seg, err := oneAuthorizeDecomposition(store, pool, buyerID, profile)
		if err != nil {
			t.Errorf("authorize decomp sample %d: %v", i, err)
			continue
		}
		for k, d := range seg {
			out[k] = append(out[k], d)
		}
		// Void the reserved contract so funding ceiling does not accumulate.
		// The probe commits an EXECUTING row intentionally (floor is durable work).
		// Look up latest EXECUTING for this buyer created by the probe.
	}
	// Teardown: fail any EXECUTING contracts left by the decomp probe for this buyer.
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	rows, err := pool.Query(ctx, `
		SELECT id FROM execution_contracts
		 WHERE buyer_id=$1 AND state='EXECUTING' AND request_id LIKE 'seg-decomp-%'`,
		buyerID)
	if err == nil {
		defer rows.Close()
		for rows.Next() {
			var id uuid.UUID
			if rows.Scan(&id) == nil {
				_, _ = store.FinalizeRealtimeFailure(ctx, id, uuid.New(), 500, 1,
					"segment_decomp_teardown", "teardown", false)
			}
		}
	}
	return out
}

func oneAuthorizeDecomposition(
	store *Store, pool *pgxpool.Pool, buyerID uuid.UUID, profile VLLMRuntimeProfile,
) (map[string]time.Duration, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	seg := map[string]time.Duration{}
	totalStart := time.Now()

	t0 := time.Now()
	tx, err := pool.Begin(ctx)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback(ctx)
	seg["begin"] = time.Since(t0)

	// offer_claim — same SQL as production.
	t0 = time.Now()
	var (
		workerID, supplierID uuid.UUID
		baseURL, sealed      string
		supplierInput        float64
		supplierOutput       float64
		placementJSON        []byte
		placementSHA256      string
		candidateCount       int
		selectedRank         int
		selectedWarmth       string
		terminalAttempts     int
		terminalFails        int
		verifiedSettlements  int
		refundCount          int
	)
	err = tx.QueryRow(ctx, realtimeAuthorizeSelectOfferSQL,
		profile.RuntimeProfileID, profile.ProfileSHA256,
		profile.BuyerInputUSDPerMillionTokens, profile.BuyerOutputUSDPerMillionTokens,
		minRealtimeOutcomeSamples).
		Scan(&workerID, &supplierID, &baseURL, &sealed, &supplierInput, &supplierOutput,
			&placementJSON, &placementSHA256, &selectedWarmth, &candidateCount, &selectedRank,
			&terminalAttempts, &terminalFails, &verifiedSettlements, &refundCount)
	if err != nil {
		return nil, fmt.Errorf("offer_claim: %w", err)
	}
	seg["offer_claim"] = time.Since(t0)

	// pricing — placement decode + PricingDecision + market receipt (production order).
	t0 = time.Now()
	placementPlan, err := decodeRealtimePlacementPlan(placementJSON, placementSHA256)
	if err != nil {
		return nil, err
	}
	currency, err := SettlementCurrency()
	if err != nil {
		return nil, err
	}
	// Match the concurrent authorize wave token bounds so decomp and admission
	// measure the same durable row shape.
	maxPrompt, maxCompletion := int64(100), int64(8)
	estPrompt, estCompletion := int64(7), int64(2)
	if maxPrompt < 100 {
		maxPrompt = 100
	}
	pricing, err := newRealtimePricingDecision(RealtimePricingInputs{
		Profile: profile, Placement: placementPlan,
		InputCommitment: strings.Repeat("c", 64), RequestSHA256: strings.Repeat("d", 64),
		MaximumPromptTokens: maxPrompt, MaximumCompletionTokens: maxCompletion,
		EstimatedPromptTokens: estPrompt, EstimatedCompletionTokens: estCompletion,
		SupplierInputRate: supplierInput, SupplierOutputRate: supplierOutput,
		BuyerDeclaredCeiling: 0, Currency: currency,
	})
	if err != nil {
		return nil, err
	}
	// Align legacy projection fields with PricingDecision (same as production).
	estUSD, maxUSD, err := realtimePricingLegacyProjection(pricing)
	if err != nil {
		return nil, err
	}
	pricingJSON, err := json.Marshal(pricing)
	if err != nil {
		return nil, err
	}
	pricingSHA256, err := pricingDecisionDigest(pricing)
	if err != nil {
		return nil, err
	}
	inputNanos, err := nanoRatePerMillionFromFloat(supplierInput)
	if err != nil {
		return nil, err
	}
	outputNanos, err := nanoRatePerMillionFromFloat(supplierOutput)
	if err != nil {
		return nil, err
	}
	rankingInputs := buildRealtimeClearingRankingInputs(
		int64(inputNanos), int64(outputNanos),
		terminalAttempts, terminalFails, verifiedSettlements, refundCount,
		selectedWarmth,
	)
	market, err := newRealtimeMarketClearingReceipt(
		candidateCount, selectedRank, workerID, supplierID, supplierInput, supplierOutput, pricing, pricingSHA256,
		rankingInputs)
	if err != nil {
		return nil, err
	}
	marketJSON, err := json.Marshal(market)
	if err != nil {
		return nil, err
	}
	seg["pricing"] = time.Since(t0)

	// funding — late lock (production order).
	t0 = time.Now()
	if err := evaluateRealtimeBuyerFunding(ctx, tx, buyerID, maxUSD); err != nil {
		return nil, fmt.Errorf("funding: %w", err)
	}
	seg["funding"] = time.Since(t0)

	contractID := uuid.New()
	requestID := "seg-decomp-" + uuid.NewString()

	// contract_insert — sequential (not batched) for component timing.
	t0 = time.Now()
	_, err = tx.Exec(ctx, `
		INSERT INTO execution_contracts
		 (id,request_id,buyer_id,workload_type,route,model_alias,runtime_profile_id,
		  runtime_profile_sha256,input_commitment,request_sha256,
		  placement_plan,placement_plan_sha256,maximum_price_usd,
		  estimated_price_usd,buyer_input_usd_per_million_tokens,
		  buyer_output_usd_per_million_tokens,supplier_input_usd_per_million_tokens,
		  supplier_output_usd_per_million_tokens,deadline_at,verification_tier,
		  idempotency_key,state,worker_id,supplier_id,upstream_base_url,upstream_token_sealed,
			 currency,maximum_prompt_tokens,maximum_completion_tokens,
			 estimated_prompt_tokens,estimated_completion_tokens,buyer_declared_ceiling_nanos,
			 pricing_decision,pricing_decision_sha256,market_clearing)
		VALUES ($1,$2,$3,'CHAT_COMPLETION','/v1/chat/completions',$4,$5,$6,$7,$8,$9,$10,
		        $11,$12,$13,$14,$15,$16,$17,'V0',$18,'EXECUTING',$19,$20,$21,$22,$23,
		        $24,$25,$26,$27,$28,$29,$30,$31)`,
		contractID, requestID, buyerID, profile.ModelAlias,
		profile.RuntimeProfileID, profile.ProfileSHA256,
		strings.Repeat("c", 64), strings.Repeat("d", 64), placementJSON, placementSHA256,
		maxUSD, estUSD, profile.BuyerInputUSDPerMillionTokens,
		profile.BuyerOutputUSDPerMillionTokens, supplierInput, supplierOutput,
		time.Now().Add(time.Minute), nil, workerID, supplierID, baseURL, sealed,
		SettlementCurrencyCode(), maxPrompt, maxCompletion,
		estPrompt, estCompletion,
		pricing.Realtime.BuyerDeclaredCeilingNanos, pricingJSON, pricingSHA256, marketJSON)
	if err != nil {
		return nil, fmt.Errorf("contract_insert: %w", err)
	}
	seg["contract_insert"] = time.Since(t0)

	t0 = time.Now()
	_, err = tx.Exec(ctx, `
		INSERT INTO realtime_authorization_events (contract_id,kind,amount_usd)
		VALUES ($1,'RESERVED',$2)`, contractID, maxUSD)
	if err != nil {
		return nil, fmt.Errorf("event_insert: %w", err)
	}
	seg["event_insert"] = time.Since(t0)

	t0 = time.Now()
	if err := tx.Commit(ctx); err != nil {
		return nil, fmt.Errorf("commit: %w", err)
	}
	seg["commit"] = time.Since(t0)
	seg["total"] = time.Since(totalStart)
	_ = store // reserved for symmetry with callers
	return seg, nil
}

func measureSettlementFinalize(
	t *testing.T, store *Store, buyerID uuid.UUID, profile VLLMRuntimeProfile, n int,
) []time.Duration {
	t.Helper()
	out := make([]time.Duration, 0, n)
	maxUSD, estUSD, maxPrompt, maxCompletion := realtimeAuthCeiling(t, profile, 7, 2)
	for i := 0; i < n; i++ {
		contract, _, err := store.AuthorizeRealtimeContract(context.Background(), RealtimeContractAuthorization{
			RequestID: "seg-settle-" + uuid.NewString(), BuyerID: buyerID, Profile: profile,
			InputCommitment: strings.Repeat("e", 64), RequestSHA256: strings.Repeat("f", 64),
			MaximumPriceUSD: maxUSD, EstimatedPriceUSD: estUSD, DeadlineAt: time.Now().Add(time.Minute),
			MaximumPromptTokens: maxPrompt, MaximumCompletionTokens: maxCompletion,
			EstimatedPromptTokens: 7, EstimatedCompletionTokens: 2,
		})
		if err != nil {
			t.Errorf("settle-prep authorize: %v", err)
			continue
		}
		evidence := RealtimeExecutionEvidence{
			ID: uuid.New(), HTTPStatus: http.StatusOK,
			StreamRootSHA256: strings.Repeat("1", 64), OutputCommitment: strings.Repeat("2", 64),
			PromptTokens: 7, CompletionTokens: 2, TotalTokens: 9,
			StreamEventCount: 3, DurationMS: 5,
		}
		start := time.Now()
		_, err = store.FinalizeRealtimeSuccess(context.Background(), contract.ID, evidence)
		elapsed := time.Since(start)
		if err != nil {
			// Bounds / pricing mismatch — void and skip sample.
			_, _ = store.FinalizeRealtimeFailure(context.Background(), contract.ID, evidence.ID, 500, 1,
				"segment_settle_skip", err.Error(), false)
			t.Logf("settlement sample skipped: %v", err)
			continue
		}
		out = append(out, elapsed)
	}
	return out
}

// --- stats / aggregation ----------------------------------------------------

type segmentLatencySummary struct {
	P50  float64 `json:"p50"`
	P95  float64 `json:"p95"`
	P99  float64 `json:"p99"`
	Min  float64 `json:"min"`
	Max  float64 `json:"max"`
	Mean float64 `json:"mean"`
	N    int     `json:"n"`
}

func summarizeSegmentLatency(samples []time.Duration) segmentLatencySummary {
	if len(samples) == 0 {
		return segmentLatencySummary{}
	}
	ms := make([]float64, len(samples))
	var sum float64
	for i, d := range samples {
		ms[i] = float64(d) / float64(time.Millisecond)
		sum += ms[i]
	}
	sort.Float64s(ms)
	pct := func(p float64) float64 {
		if len(ms) == 1 {
			return ms[0]
		}
		idx := int(math.Ceil(p*float64(len(ms)))) - 1
		if idx < 0 {
			idx = 0
		}
		if idx >= len(ms) {
			idx = len(ms) - 1
		}
		return ms[idx]
	}
	return segmentLatencySummary{
		P50: pct(0.50), P95: pct(0.95), P99: pct(0.99),
		Min: ms[0], Max: ms[len(ms)-1],
		Mean: sum / float64(len(ms)), N: len(ms),
	}
}

func summarizeStageMap(m map[string][]time.Duration) map[string]segmentLatencySummary {
	out := make(map[string]segmentLatencySummary, len(m))
	for k, samples := range m {
		out[k] = summarizeSegmentLatency(samples)
	}
	return out
}

func aggregateAcrossRuns(cells []segmentRunCell, concurrencies []int) map[string]any {
	// For each concurrency, collect p50s across runs for key metrics.
	out := map[string]any{}
	for _, c := range concurrencies {
		var subset []segmentRunCell
		for _, cell := range cells {
			if cell.Concurrency == c {
				subset = append(subset, cell)
			}
		}
		if len(subset) == 0 {
			continue
		}
		// Latest-run full summaries for reference (last independent run).
		last := subset[len(subset)-1]
		metrics := map[string]any{
			"runs":                            len(subset),
			"client_ttft_p50_across_runs":     acrossRunP50(subset, func(c segmentRunCell) float64 { return c.ClientTTFT.P50 }),
			"merc_added_ttft_p50_across_runs": acrossRunP50(subset, func(c segmentRunCell) float64 { return c.MercAdded.P50 }),
			"authorize_p50_across_runs":       acrossRunP50(subset, func(c segmentRunCell) float64 { return c.AuthorizeAdmission.P50 }),
			"settlement_p50_across_runs":      acrossRunP50(subset, func(c segmentRunCell) float64 { return c.SettlementFinalize.P50 }),
			// Representative percentiles from the last run (full p50/p95/p99).
			"last_run_client_ttft_ms":      last.ClientTTFT,
			"last_run_merc_added_ttft_ms":  last.MercAdded,
			"last_run_authorize_ms":        last.AuthorizeAdmission,
			"last_run_settlement_ms":       last.SettlementFinalize,
			"last_run_stages_ms":           last.Stages,
			"last_run_authorize_decomp_ms": last.AuthorizeDecomp,
		}
		// Decomp component medians across runs.
		decompKeys := []string{"funding", "offer_claim", "pricing", "contract_insert", "event_insert", "commit", "total"}
		decompAcross := map[string]any{}
		for _, dk := range decompKeys {
			dk := dk
			decompAcross[dk] = acrossRunP50(subset, func(c segmentRunCell) float64 {
				if c.AuthorizeDecomp == nil {
					return 0
				}
				return c.AuthorizeDecomp[dk].P50
			})
		}
		metrics["authorize_decomp_p50_across_runs"] = decompAcross
		// Stage p50 across runs for the headline stages.
		stageKeys := []string{
			"read_body", "prepare_json", "intake_control", "authorize_contract",
			"admission_event", "settlement_intent", "proxy_sse", "settlement", "upstream_ttfb",
		}
		stagesAcross := map[string]any{}
		for _, sk := range stageKeys {
			sk := sk
			stagesAcross[sk] = acrossRunP50(subset, func(c segmentRunCell) float64 {
				if c.Stages == nil {
					return 0
				}
				return c.Stages[sk].P50
			})
		}
		metrics["stages_p50_across_runs"] = stagesAcross
		out[fmt.Sprintf("c=%d", c)] = metrics
	}
	return out
}

type acrossRunStats struct {
	MedianP50 float64   `json:"median_of_run_p50s"`
	MinP50    float64   `json:"p50_run_min"`
	MaxP50    float64   `json:"p50_run_max"`
	StdevP50  float64   `json:"p50_run_stdev"`
	RunP50s   []float64 `json:"run_p50s"`
	NRuns     int       `json:"n_runs"`
}

func acrossRunP50(cells []segmentRunCell, get func(segmentRunCell) float64) acrossRunStats {
	vals := make([]float64, 0, len(cells))
	for _, c := range cells {
		vals = append(vals, get(c))
	}
	if len(vals) == 0 {
		return acrossRunStats{}
	}
	sorted := append([]float64(nil), vals...)
	sort.Float64s(sorted)
	median := sorted[len(sorted)/2]
	if len(sorted)%2 == 0 {
		median = (sorted[len(sorted)/2-1] + sorted[len(sorted)/2]) / 2
	}
	var sum, sumSq float64
	for _, v := range vals {
		sum += v
		sumSq += v * v
	}
	mean := sum / float64(len(vals))
	var stdev float64
	if len(vals) > 1 {
		// Population variance; clamp float noise that can go slightly negative.
		v := sumSq/float64(len(vals)) - mean*mean
		if v > 0 {
			stdev = math.Sqrt(v)
		}
	}
	return acrossRunStats{
		MedianP50: median,
		MinP50:    sorted[0],
		MaxP50:    sorted[len(sorted)-1],
		StdevP50:  stdev,
		RunP50s:   vals,
		NRuns:     len(vals),
	}
}

func judgeFloorClaim(aggregated map[string]any) map[string]any {
	c1, _ := aggregated["c=1"].(map[string]any)
	if c1 == nil {
		return map[string]any{"summary": "no c=1 data", "holds": false}
	}
	decomp, _ := c1["authorize_decomp_p50_across_runs"].(map[string]any)
	authAcross, _ := c1["authorize_p50_across_runs"].(acrossRunStats)
	get := func(k string) float64 {
		if decomp == nil {
			return 0
		}
		if s, ok := decomp[k].(acrossRunStats); ok {
			return s.MedianP50
		}
		return 0
	}
	funding := get("funding")
	offer := get("offer_claim")
	pricing := get("pricing")
	contract := get("contract_insert")
	event := get("event_insert")
	commit := get("commit")
	total := get("total")
	sumParts := funding + offer + pricing + contract + event + commit

	// Floor claim: ~1.0 ms durable work, honest range 0.7–1.2 ms.
	const floorLo, floorHi, floorCenter = 0.7, 1.2, 1.0
	// Prefer measured total; fall back to sum of parts.
	measuredFloor := total
	if measuredFloor <= 0 {
		measuredFloor = sumParts
	}
	inRange := measuredFloor >= floorLo && measuredFloor <= floorHi
	// Full AuthorizeRealtimeContract p50 should sit near the decomp total
	// (slightly above: openToken + return construction after commit).
	authP50 := authAcross.MedianP50

	summary := fmt.Sprintf(
		"c=1 authorize decomp median-of-p50s: funding=%.3f offer=%.3f pricing=%.3f contract=%.3f event=%.3f commit=%.3f sum=%.3f total=%.3f ms; "+
			"full AuthorizeRealtimeContract median-of-p50s=%.3f ms (run range %.3f–%.3f). "+
			"Claimed floor ~1.0 ms (range 0.7–1.2). measured durable total=%.3f → in_range=%v.",
		funding, offer, pricing, contract, event, commit, sumParts, total,
		authP50, authAcross.MinP50, authAcross.MaxP50, measuredFloor, inRange)

	return map[string]any{
		"summary": summary,
		"holds_approx_1ms_durable": math.Abs(measuredFloor-floorCenter) <= 0.35 || inRange,
		"holds_0_7_to_1_2_range":   inRange,
		"measured_durable_total_ms": measuredFloor,
		"measured_parts_sum_ms":     sumParts,
		"measured_parts_ms": map[string]float64{
			"funding": funding, "offer_claim": offer, "pricing": pricing,
			"contract_insert": contract, "event_insert": event, "commit": commit,
		},
		"claimed_parts_ms": map[string]float64{
			"funding": 0.20, "offer_claim": 0.20, "pricing": 0.05,
			"contract_insert": 0.35, "event_insert": 0.10, "commit": 0.15, "sum": 1.0,
		},
		"full_authorize_p50_ms": authP50,
		"full_authorize_p50_run_range_ms": []float64{authAcross.MinP50, authAcross.MaxP50},
		"notes": []string{
			"Decomposition uses sequential inserts; production batches contract+event (one fewer RTT).",
			"Full AuthorizeRealtimeContract includes begin/rollback-path overhead and post-commit token open.",
			"If measured total sits above 1.2 ms, the published range is optimistic for this machine/load — not proof the work is avoidable.",
			"If measured total sits below 0.7 ms, the prior claim overstated the floor — report the lower measurement.",
		},
	}
}

func deltasVsPrior(aggregated map[string]any, prior map[string]any) map[string]any {
	out := map[string]any{}
	priorAuth, _ := prior["authorize_p50_ms"].(map[string]float64)
	priorTTFT, _ := prior["merc_added_ttft_p50_ms"].(map[string]float64)
	for _, c := range []int{1, 8, 32} {
		key := fmt.Sprintf("c=%d", c)
		cell, _ := aggregated[key].(map[string]any)
		if cell == nil {
			continue
		}
		auth, _ := cell["authorize_p50_across_runs"].(acrossRunStats)
		ttft, _ := cell["merc_added_ttft_p50_across_runs"].(acrossRunStats)
		pk := fmt.Sprintf("c%d", c)
		entry := map[string]any{}
		if priorAuth != nil {
			if p, ok := priorAuth[pk]; ok {
				entry["authorize_prior_p50_ms"] = p
				entry["authorize_measured_median_p50_ms"] = auth.MedianP50
				entry["authorize_delta_ms"] = auth.MedianP50 - p
				if p > 0 {
					entry["authorize_ratio"] = auth.MedianP50 / p
				}
			}
		}
		if priorTTFT != nil {
			if p, ok := priorTTFT[pk]; ok {
				entry["merc_added_ttft_prior_p50_ms"] = p
				entry["merc_added_ttft_measured_median_p50_ms"] = ttft.MedianP50
				entry["merc_added_ttft_delta_ms"] = ttft.MedianP50 - p
				if p > 0 {
					entry["merc_added_ttft_ratio"] = ttft.MedianP50 / p
				}
			}
		}
		// Stage deltas at c=1 only (prior published c=1 stage breakdown).
		if c == 1 {
			if stages, ok := prior["c1_stage_p50_ms"].(map[string]float64); ok {
				stageAcross, _ := cell["stages_p50_across_runs"].(map[string]any)
				stageDeltas := map[string]any{}
				// Map prior names → path timing keys.
				mapping := map[string]string{
					"authorize": "authorize_contract", "admission_event": "admission_event",
					"settlement_intent": "settlement_intent", "intake_control": "intake_control",
					"prepare_json": "prepare_json", "proxy_sse": "proxy_sse", "read_body": "read_body",
				}
				for priorName, stageKey := range mapping {
					priorV := stages[priorName]
					var meas float64
					if stageAcross != nil {
						if s, ok := stageAcross[stageKey].(acrossRunStats); ok {
							meas = s.MedianP50
						}
					}
					stageDeltas[priorName] = map[string]float64{
						"prior_p50_ms": priorV, "measured_median_p50_ms": meas, "delta_ms": meas - priorV,
					}
				}
				entry["stages"] = stageDeltas
			}
			if sp, ok := prior["settlement_finalize_p50_ms"].(float64); ok {
				settle, _ := cell["settlement_p50_across_runs"].(acrossRunStats)
				entry["settlement_prior_p50_ms"] = sp
				entry["settlement_measured_median_p50_ms"] = settle.MedianP50
				entry["settlement_delta_ms"] = settle.MedianP50 - sp
			}
		}
		out[key] = entry
	}
	return out
}

func machineLoadQuiet() bool {
	loads, n := readLoadAverage()
	if n <= 0 || len(loads) == 0 {
		return false
	}
	// "Quiet" ≈ 1-minute load below half the CPU count.
	return loads[0] < float64(n)/2
}

func quietReason(quiet bool, loads []float64, n int) string {
	if n <= 0 {
		return "could not read load average or CPU count"
	}
	if quiet {
		return fmt.Sprintf("1-min load %.2f < half of %d CPUs", loads[0], n)
	}
	return fmt.Sprintf("1-min load %.2f is not below half of %d CPUs — machine was NOT quiet; treat tails as noisy", loads[0], n)
}

func readLoadAverage() ([]float64, int) {
	n := runtime.NumCPU()
	// Env override first (set by the runner from `uptime` / sysctl).
	if v := strings.TrimSpace(os.Getenv("MERC_LOADAVG")); v != "" {
		parts := strings.Fields(strings.ReplaceAll(v, ",", " "))
		var loads []float64
		for _, p := range parts {
			var f float64
			if _, err := fmt.Sscanf(p, "%f", &f); err == nil {
				loads = append(loads, f)
			}
		}
		if len(loads) > 0 {
			return loads, n
		}
	}
	// Linux.
	if raw, err := os.ReadFile("/proc/loadavg"); err == nil {
		fields := strings.Fields(string(raw))
		if len(fields) >= 3 {
			var loads []float64
			for i := 0; i < 3; i++ {
				var v float64
				fmt.Sscanf(fields[i], "%f", &v)
				loads = append(loads, v)
			}
			return loads, n
		}
	}
	// macOS: sysctl -n vm.loadavg → "{ 1.2 1.3 1.4 }"
	if out, err := execSoft("sysctl", "-n", "vm.loadavg"); err == nil {
		cleaned := strings.Trim(out, "{}\n\t ")
		parts := strings.Fields(cleaned)
		var loads []float64
		for _, p := range parts {
			var f float64
			if _, err := fmt.Sscanf(p, "%f", &f); err == nil {
				loads = append(loads, f)
			}
		}
		if len(loads) > 0 {
			return loads, n
		}
	}
	return nil, n
}

// execSoft runs a short command and returns trimmed stdout, or error.
// Used only for load-average capture on macOS; failure is non-fatal.
func execSoft(name string, args ...string) (string, error) {
	cmd := exec.Command(name, args...)
	out, err := cmd.Output()
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(out)), nil
}
