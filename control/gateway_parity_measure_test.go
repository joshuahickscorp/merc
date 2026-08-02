package main

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
)

// TestGatewayParityAgainstRealEngine is an opt-in measurement, not a CI gate.
//
//	MERC_GATEWAY_PARITY=1 \
//	MERC_REALTIME_UPSTREAM=http://127.0.0.1:8095/v1 \
//	MERC_REALTIME_UPSTREAM_KEY=merc-canary-key \
//	MERC_TEST_DATABASE_URL=postgres://… \
//	go test -count=1 -run TestGatewayParityAgainstRealEngine -timeout 20m .
//
// Boots a control-plane httptest server against the shared test database, registers
// the real local engine as the only realtime offer, and drives
// scripts/gateway-parity.py so the receipt compares merc → engine vs engine
// directly under identical model/max_tokens.
func TestGatewayParityAgainstRealEngine(t *testing.T) {
	if os.Getenv("MERC_GATEWAY_PARITY") != "1" {
		t.Skip("set MERC_GATEWAY_PARITY=1 to run the live gateway overhead measurement")
	}
	upstream := strings.TrimSpace(os.Getenv("MERC_REALTIME_UPSTREAM"))
	upstreamKey := strings.TrimSpace(os.Getenv("MERC_REALTIME_UPSTREAM_KEY"))
	if upstream == "" || upstreamKey == "" {
		t.Fatal("MERC_REALTIME_UPSTREAM and MERC_REALTIME_UPSTREAM_KEY are required")
	}
	// Refuse to invent numbers when the engine is down.
	probe, err := http.NewRequest(http.MethodGet, strings.TrimRight(upstream, "/")+"/models", nil)
	if err != nil {
		t.Fatal(err)
	}
	probe.Header.Set("Authorization", "Bearer "+upstreamKey)
	probeResp, err := http.DefaultClient.Do(probe)
	if err != nil {
		t.Fatalf("local engine is not reachable at %s: %v (start llama-server rather than fabricating numbers)", upstream, err)
	}
	probeResp.Body.Close()
	if probeResp.StatusCode != http.StatusOK {
		t.Fatalf("local engine at %s answered HTTP %d", upstream, probeResp.StatusCode)
	}

	databaseURL := requireTestDatabase(t)
	t.Setenv("MERC_TOKEN_KEY", "gateway-parity-measure-key-with-at-least-32-bytes")
	t.Setenv("STRIPE_SECRET_KEY", "")
	t.Setenv("MERC_CANARY_ENABLED", "false")
	t.Setenv("MERC_REALTIME_PATH_TIMING", "1")

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Minute)
	defer cancel()
	pool, err := pgxpool.New(ctx, databaseURL)
	if err != nil {
		t.Fatal(err)
	}
	defer pool.Close()
	store := NewStore(pool)
	if err := store.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	// Own the realtime tables for this measurement so concurrent test runs
	// cannot steal capacity or leave EXECUTING rows that block admission.
	// Retry TRUNCATE: the shared merc_rt2 database can hold short locks from
	// a concurrent integration test, and a single 40P01 is not a real failure.
	var resetErr error
	for attempt := 0; attempt < 8; attempt++ {
		_, resetErr = pool.Exec(ctx, `TRUNCATE
			realtime_authorization_events, realtime_settlements, realtime_executions,
			realtime_refunds, execution_contracts, realtime_worker_offers
			RESTART IDENTITY CASCADE`)
		if resetErr == nil {
			break
		}
		time.Sleep(time.Duration(attempt+1) * 100 * time.Millisecond)
	}
	if resetErr != nil {
		t.Fatalf("reset realtime state: %v", resetErr)
	}
	if _, err := pool.Exec(ctx,
		`DELETE FROM ledger_entries WHERE execution_contract_id IS NOT NULL`); err != nil {
		t.Fatalf("reset realtime ledger rows: %v", err)
	}

	suffix := uuid.NewString()
	buyerID, err := store.CreateBuyerAccount(ctx, "gateway-parity-"+suffix+"@example.test", "integration-password", 50)
	if err != nil {
		t.Fatal(err)
	}
	_, buyerKey, _, err := store.CreateAPIKey(ctx, buyerID, "gateway parity", true)
	if err != nil {
		t.Fatal(err)
	}
	supplierID := uuid.New()
	if _, err := pool.Exec(ctx, `INSERT INTO suppliers (id,email,status) VALUES ($1,$2,'active')`,
		supplierID, "supplier-parity-"+suffix+"@example.test"); err != nil {
		t.Fatal(err)
	}
	workerID := uuid.New()
	if _, err := store.CreateWorkerToken(ctx, workerID, supplierID); err != nil {
		t.Fatal(err)
	}

	profile := sortedVLLMProfiles()[0]
	// The offer must present a generated-style credential; the real engine key
	// is what the gateway forwards as Bearer to the upstream.
	// validateRealtimeOfferRegistration requires cx_vllm_ prefix; the engine
	// itself may expect a different key, so we store the real engine key as the
	// sealed upstream token via UpsertRealtimeOffer which encrypts it.
	//
	// Registration validation is only applied on the HTTP worker route. The
	// store method used here accepts the token as-is after the test constructs
	// a valid-looking cx_vllm_ value… unless the engine requires its own key.
	// Forward the real key: openToken seals whatever we put in UpstreamToken.
	// validateRealtimeOfferRegistration is NOT called by UpsertRealtimeOffer.
	if err := store.UpsertRealtimeOffer(ctx, WorkerAuth{WorkerID: workerID, SupplierID: supplierID}, RealtimeOfferRegistration{
		RuntimeProfileID: profile.RuntimeProfileID, RuntimeProfileSHA256: profile.ProfileSHA256,
		HWClass: "nvidia_24gb", GPUCount: 1, MemoryGBPerGPU: 24,
		UpstreamBaseURL: strings.TrimRight(upstream, "/"), UpstreamToken: upstreamKey,
		Warmth: "HOT", MaxActiveSequences: 64, AvailableSequences: 64,
		SupplierInputUSDPerMillionTokens: 0.08, SupplierOutputUSDPerMillionTokens: 0.30,
	}); err != nil {
		t.Fatal(err)
	}
	// Heartbeat loop so available capacity stays fresh across the sweep.
	hbDone := make(chan struct{})
	go func() {
		ticker := time.NewTicker(10 * time.Second)
		defer ticker.Stop()
		for {
			select {
			case <-hbDone:
				return
			case <-ticker.C:
				_ = store.HeartbeatRealtimeOffer(context.Background(),
					WorkerAuth{WorkerID: workerID, SupplierID: supplierID},
					RealtimeOfferHeartbeat{
						RuntimeProfileID: profile.RuntimeProfileID, Warmth: "HOT",
						AvailableSequences: 64, Status: "ACTIVE",
					})
			}
		}
	}()
	defer close(hbDone)

	server := httptest.NewServer(NewServer(store, nil, nil, nil).Routes())
	defer server.Close()

	// Smoke one merc completion before the sweep so a mis-registered offer
	// fails fast with a readable error instead of a long multi-level timeout.
	smokeBody := []byte(`{"model":"cx-chat-1b","messages":[{"role":"user","content":"ping"}],"max_tokens":4,"stream":true,"temperature":0}`)
	smokeReq, err := http.NewRequestWithContext(ctx, http.MethodPost, server.URL+"/v1/chat/completions", bytes.NewReader(smokeBody))
	if err != nil {
		t.Fatal(err)
	}
	smokeReq.Header.Set("Authorization", "Bearer "+buyerKey)
	smokeReq.Header.Set("Content-Type", "application/json")
	smokeResp, err := http.DefaultClient.Do(smokeReq)
	if err != nil {
		t.Fatalf("merc smoke request failed: %v", err)
	}
	smokeBytes, _ := readAllLimited(smokeResp.Body, 1<<20)
	smokeResp.Body.Close()
	if smokeResp.StatusCode != http.StatusOK {
		t.Fatalf("merc smoke status=%d body=%s", smokeResp.StatusCode, smokeBytes)
	}

	outPath := filepath.Join("..", "evidence", "perf", "gateway-parity.json")
	if envOut := os.Getenv("MERC_GATEWAY_PARITY_OUT"); envOut != "" {
		outPath = envOut
	}
	if err := os.MkdirAll(filepath.Dir(outPath), 0o755); err != nil {
		t.Fatal(err)
	}

	// Default to minCellCostSamples (20): p95/p99 under nearest-rank need that
	// floor, and the promotion gate elsewhere refuses fewer. Operators can
	// override with MERC_GATEWAY_PARITY_REQUESTS for a short smoke, but the
	// harness will refuse gate_passed below the floor.
	requestsPerLevel := strconv.Itoa(minCellCostSamples)
	if v := os.Getenv("MERC_GATEWAY_PARITY_REQUESTS"); v != "" {
		requestsPerLevel = v
	}
	maxTokens := "32"
	if v := os.Getenv("MERC_GATEWAY_PARITY_MAX_TOKENS"); v != "" {
		maxTokens = v
	}
	// Llama-3.2-1B-Instruct Q4_K_M — same digest as
	// evidence/perf/runtime-benchmarks/llama-cpp-metal-llama1-q4-r1.json and
	// ops/model-provenance.json. Required so same-weights is proved, not
	// inferred from /models or a one-token smoke.
	artifactSHA := "3f5a22426976ab26cfe84dba63c1d08391717abb1af893e10f1b2968d862dcc1"
	if v := os.Getenv("MERC_GATEWAY_PARITY_MODEL_ARTIFACT_SHA256"); v != "" {
		artifactSHA = v
	}

	cmd := exec.CommandContext(ctx, "python3", filepath.Join("..", "scripts", "gateway-parity.py"),
		"--merc-base-url", server.URL+"/v1",
		"--direct-base-url", strings.TrimRight(upstream, "/"),
		"--model", profile.ModelAlias,
		"--max-tokens", maxTokens,
		"--concurrency", "1,8,32",
		"--requests-per-level", requestsPerLevel,
		"--merc-label", "merc control plane (httptest) -> same local engine",
		"--direct-label", "direct local llama-server Metal / cx-chat-1b GGUF-Q4",
		"--model-artifact-sha256", artifactSHA,
		"--precision", "GGUF-Q4_K_M",
		"--engine", "llama_cpp",
		"--hw-class", "apple_silicon_ultra",
		"--out", outPath,
	)
	cmd.Env = append(os.Environ(),
		"MERC_BENCHMARK_API_KEY="+buyerKey,
		"MERC_DIRECT_VLLM_API_KEY="+upstreamKey,
	)
	output, err := cmd.CombinedOutput()
	t.Logf("gateway-parity.py output:\n%s", output)
	if err != nil {
		t.Fatalf("gateway-parity harness failed: %v", err)
	}

	raw, err := os.ReadFile(outPath)
	if err != nil {
		t.Fatal(err)
	}
	var receipt struct {
		Comparable       bool     `json:"comparable"`
		GatePassed       bool     `json:"gate_passed"`
		MeasuredAt       string   `json:"measured_at"`
		MercSourceCommit string   `json:"merc_source_commit"`
		GateVersion      string   `json:"gate_version"`
		Refusals         []string `json:"refusals"`
		Summary          struct {
			TTFTP50 *float64 `json:"ttft_overhead_p50_ms"`
			TTFTP95 *float64 `json:"ttft_overhead_p95_ms"`
			TTFTP99 *float64 `json:"ttft_overhead_p99_ms"`
		} `json:"summary_concurrency_1"`
		Scope struct {
			MercModelArtifactSHA256   string `json:"merc_model_artifact_sha256"`
			DirectModelArtifactSHA256 string `json:"direct_model_artifact_sha256"`
		} `json:"scope"`
	}
	if err := json.Unmarshal(raw, &receipt); err != nil {
		t.Fatal(err)
	}
	if !receipt.Comparable || !receipt.GatePassed {
		t.Fatalf("gateway parity receipt is not comparable/gate-passed: %s", raw)
	}
	if receipt.MeasuredAt == "" {
		t.Fatal("receipt missing measured_at")
	}
	if receipt.MercSourceCommit == "" {
		t.Fatal("receipt missing merc_source_commit")
	}
	if receipt.GateVersion == "" {
		t.Fatal("receipt missing gate_version")
	}
	if len(receipt.Refusals) != 0 {
		t.Fatalf("receipt refusals non-empty: %v", receipt.Refusals)
	}
	if receipt.Scope.MercModelArtifactSHA256 != artifactSHA ||
		receipt.Scope.DirectModelArtifactSHA256 != artifactSHA {
		t.Fatalf("scope artifact digests not pinned: merc=%q direct=%q want %q",
			receipt.Scope.MercModelArtifactSHA256,
			receipt.Scope.DirectModelArtifactSHA256, artifactSHA)
	}
	t.Logf("concurrency=1 TTFT overhead p50=%v p95=%v p99=%v ms (merc − direct)",
		derefFloat(receipt.Summary.TTFTP50),
		derefFloat(receipt.Summary.TTFTP95),
		derefFloat(receipt.Summary.TTFTP99))
}

func derefFloat(p *float64) any {
	if p == nil {
		return nil
	}
	return *p
}

func readAllLimited(r interface{ Read([]byte) (int, error) }, n int64) ([]byte, error) {
	buf := make([]byte, 0, 4096)
	tmp := make([]byte, 4096)
	var total int64
	for {
		nr, err := r.Read(tmp)
		if nr > 0 {
			total += int64(nr)
			if total > n {
				return buf, nil
			}
			buf = append(buf, tmp[:nr]...)
		}
		if err != nil {
			return buf, nil
		}
	}
}
