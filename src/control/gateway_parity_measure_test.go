package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
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
// the real local engine as the only realtime offer, and drives the authoritative
// control harness (RunGatewayParityInterleavedLevel + BuildGatewayParityReceipt).
// Does NOT shell the invalidated ops/scripts/gateway-parity.py.
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
	must(t, err)
	probe.Header.Set("Authorization", "Bearer "+upstreamKey)
	probeResp, err := http.DefaultClient.Do(probe)
	mustf(t, err, "local engine is not reachable at %s: %v (start llama-server rather than fabricating numbers)", upstream)
	probeResp.Body.Close()
	if probeResp.StatusCode != http.StatusOK {
		t.Fatalf("local engine at %s answered HTTP %d", upstream, probeResp.StatusCode)
	}

	databaseURL := requireTestDatabase(t)
	t.Setenv("MERC_TOKEN_KEY", "gateway-parity-measure-key-with-at-least-32-bytes")
	t.Setenv("STRIPE_SECRET_KEY", "")
	t.Setenv("MERC_CANARY_ENABLED", "false")
	t.Setenv("MERC_REALTIME_PATH_TIMING", "1")
	// Capture so body identity can be proven for PARITY_EVIDENCE.
	t.Setenv(parityUpstreamCaptureEnv, "1")

	// Matrix mode (full competitive ladder including c=128) needs a longer
	// wall budget than the single-shape ladder; single-shape keeps 20m.
	measureTimeout := 20 * time.Minute
	if os.Getenv("MERC_GATEWAY_PARITY_MATRIX") == "1" {
		measureTimeout = 90 * time.Minute
	}
	ctx, cancel := context.WithTimeout(context.Background(), measureTimeout)
	defer cancel()
	pool, err := pgxpool.New(ctx, databaseURL)
	must(t, err)
	defer pool.Close()
	store := NewStore(pool)
	must(t, store.Migrate(ctx))
	// Own the realtime tables for this measurement so concurrent test runs
	// cannot steal capacity or leave EXECUTING rows that block admission.
	var resetErr error
	for attempt := 0; attempt < 8; attempt++ {
		_, resetErr = pool.Exec(ctx, `TRUNCATE
			realtime_authorization_events, realtime_settlements, realtime_executions,
			realtime_refunds, execution_contracts, realtime_worker_offers,
			realtime_supplier_outcome_stats
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
	must(t, err)
	_, buyerKey, _, err := store.CreateAPIKey(ctx, buyerID, "gateway parity", true)
	must(t, err)
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
	// Max sequences high enough for c=128 ladder attempts. The catalogue
	// profile still describes vLLM; the actual upstream is whatever
	// MERC_REALTIME_UPSTREAM serves (llama.cpp/Metal for LOCAL_METAL_PARITY).
	// We do NOT relax validateVLLMRuntimeProfile — the profile remains the
	// catalogue pin used for routing/auth; evidence_class carries the scope.
	const offerSequences = 256
	if err := store.UpsertRealtimeOffer(ctx, WorkerAuth{WorkerID: workerID, SupplierID: supplierID}, RealtimeOfferRegistration{
		RuntimeProfileID: profile.RuntimeProfileID, RuntimeProfileSHA256: profile.ProfileSHA256,
		HWClass: "nvidia_24gb", GPUCount: 1, MemoryGBPerGPU: 24,
		UpstreamBaseURL: strings.TrimRight(upstream, "/"), UpstreamToken: upstreamKey,
		Warmth: "HOT", MaxActiveSequences: offerSequences, AvailableSequences: offerSequences,
		SupplierInputUSDPerMillionTokens: 0.08, SupplierOutputUSDPerMillionTokens: 0.30,
	}); err != nil {
		t.Fatal(err)
	}
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
						AvailableSequences: offerSequences, Status: "ACTIVE",
					})
			}
		}
	}()
	defer close(hbDone)

	server := httptest.NewServer(NewServer(store, nil, nil, nil).Routes())
	defer server.Close()

	// Smoke one merc completion before the sweep.
	smokeBody := []byte(`{"model":"cx-chat-1b","messages":[{"role":"user","content":"ping"}],"max_tokens":4,"stream":true,"temperature":0}`)
	smokeReq, err := http.NewRequestWithContext(ctx, http.MethodPost, server.URL+"/v1/chat/completions", bytes.NewReader(smokeBody))
	must(t, err)
	smokeReq.Header.Set("Authorization", "Bearer "+buyerKey)
	smokeReq.Header.Set("Content-Type", "application/json")
	smokeResp, err := http.DefaultClient.Do(smokeReq)
	mustf(t, err, "merc smoke request failed: %v")
	smokeBytes, _ := readAllLimited(smokeResp.Body, 1<<20)
	smokeResp.Body.Close()
	if smokeResp.StatusCode != http.StatusOK {
		t.Fatalf("merc smoke status=%d body=%s", smokeResp.StatusCode, smokeBytes)
	}

	outPath := filepath.Join("..", "..", "evidence", "perf", "gateway-parity.json")
	if envOut := os.Getenv("MERC_GATEWAY_PARITY_OUT"); envOut != "" {
		outPath = envOut
	}
	must(t, os.MkdirAll(filepath.Dir(outPath), 0o755))

	maxTokens := 32
	if v := os.Getenv("MERC_GATEWAY_PARITY_MAX_TOKENS"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			maxTokens = n
		}
	}
	// Llama-3.2-1B-Instruct Q4_K_M — same digest as
	// evidence/perf/runtime-benchmarks/llama-cpp-metal-llama1-q4-r1.json and
	// ops/model-provenance.json.
	artifactSHA := "3f5a22426976ab26cfe84dba63c1d08391717abb1af893e10f1b2968d862dcc1"
	if v := os.Getenv("MERC_GATEWAY_PARITY_MODEL_ARTIFACT_SHA256"); v != "" {
		artifactSHA = v
	}

	// Evidence class: default LOCAL_METAL_PARITY for real-engine host runs so a
	// Metal/llama.cpp receipt cannot be quoted as catalogue PARITY_EVIDENCE.
	// Operators targeting a true vLLM dual-arm may set MERC_GATEWAY_PARITY_EVIDENCE_CLASS=PARITY_EVIDENCE.
	evidenceClass := "LOCAL_METAL_PARITY"
	if v := strings.TrimSpace(os.Getenv("MERC_GATEWAY_PARITY_EVIDENCE_CLASS")); v != "" {
		evidenceClass = v
	}

	contract := GatewayParitySamplingContract{
		Model: profile.ModelAlias, Prompt: "Write a short factual paragraph about the water cycle. Be specific and do not repeat yourself.",
		Temperature: 0, TopP: 0.95, MaxTokens: maxTokens, Stream: true,
		ModelDigest: artifactSHA, RuntimeProfileID: profile.RuntimeProfileID,
		RuntimeProfileSHA256: profile.ProfileSHA256,
	}

	engineNote := strings.TrimSpace(os.Getenv("MERC_GATEWAY_PARITY_ENGINE_NOTE"))
	if engineNote == "" {
		engineNote = "local engine at MERC_REALTIME_UPSTREAM (expect llama.cpp/Metal for LOCAL_METAL_PARITY)"
	}
	topology := GatewayParityNetworkTopology{
		ClientHost: "measure-test-process", ControlPlane: "httptest control plane",
		Engine:          engineNote,
		ClientToControl: "loopback", ControlToEngine: "loopback",
		ClientToEngine: "loopback",
		Notes:          "opt-in live measure via control harness (not gateway-parity.py)",
	}

	// Matrix mode (revision-1 competitive CUDA): prompt × output × state ×
	// concurrency, every cell independently gated. Single-shape ladder remains
	// the default when MERC_GATEWAY_PARITY_MATRIX is unset.
	if os.Getenv("MERC_GATEWAY_PARITY_MATRIX") == "1" {
		selection := CompetitiveCUDAParityMatrixSelection()
		if os.Getenv("MERC_GATEWAY_PARITY_MATRIX_DEFAULT") == "1" {
			selection = DefaultGatewayParityMatrixSelection()
		}
		t.Logf("=== matrix mode: %d cells — %s ===", len(selection.Selected), selection.Rationale)
		hostStart := CaptureGatewayParityHostLoad()
		cells := RunGatewayParityMatrix(
			ctx,
			server.URL+"/v1", buyerKey,
			strings.TrimRight(upstream, "/"), upstreamKey,
			contract, selection,
		)
		hostEnd := CaptureGatewayParityHostLoad()
		notes := []string{
			"driven by TestGatewayParityAgainstRealEngine matrix mode (src/control/gateway_parity_matrix.go)",
			"invalidated path ops/scripts/gateway-parity.py is not invoked",
			fmt.Sprintf("evidence_class=%s (env MERC_GATEWAY_PARITY_EVIDENCE_CLASS)", evidenceClass),
			fmt.Sprintf("catalogue runtime_profile_id=%s used for merc routing/auth only", profile.RuntimeProfileID),
			"selection=CompetitiveCUDAParityMatrixSelection unless MERC_GATEWAY_PARITY_MATRIX_DEFAULT=1",
			"do not quote the withdrawn 'Merc is 17.5% behind' claim; this receipt supersedes it",
		}
		rec := BuildGatewayParityMatrixReceipt(
			contract, topology, selection, cells,
			DefaultGatewayParityBudget(), hostStart, hostEnd,
			evidenceClass, notes,
		)
		raw, err := json.MarshalIndent(rec, "", "  ")
		must(t, err)
		must(t, os.WriteFile(outPath, append(raw, '\n'), 0o644))
		t.Logf("wrote matrix receipt to %s cells=%d gate_passed=%v verdict=%s comparable=%v",
			outPath, len(rec.Cells), rec.GatePassed, rec.Gate.Verdict, rec.Comparable)
		if rec.MeasuredAt == "" || rec.MercSourceCommit == "" || rec.GateVersion == "" {
			t.Fatal("matrix receipt missing required identity fields")
		}
		if rec.SamplingContract.ModelDigest != artifactSHA {
			t.Fatalf("model digest not pinned: %q want %q", rec.SamplingContract.ModelDigest, artifactSHA)
		}
		for _, cell := range rec.Cells {
			t.Logf("  cell %s status=%s gate=%s", cell.Spec.Key(), cell.Status, cell.Gate.Verdict)
		}
		return
	}

	// Default claimed levels; operators can narrow or extend (e.g. 1,8,32,64,128).
	// Do not claim a level you did not run — set MERC_GATEWAY_PARITY_CONCURRENCY.
	claimed := []int{1, 8, 32}
	if v := os.Getenv("MERC_GATEWAY_PARITY_CONCURRENCY"); v != "" {
		claimed = claimed[:0]
		for _, p := range strings.Split(v, ",") {
			p = strings.TrimSpace(p)
			if p == "" {
				continue
			}
			n, err := strconv.Atoi(p)
			if err != nil || n < 1 {
				t.Fatalf("bad MERC_GATEWAY_PARITY_CONCURRENCY %q", p)
			}
			claimed = append(claimed, n)
		}
	}

	body, err := contract.BuildChatCompletionsBody()
	must(t, err)

	maxC := claimed[0]
	for _, c := range claimed {
		if c > maxC {
			maxC = c
		}
	}
	client := NewGatewayParityClient(maxC)
	hostStart := CaptureGatewayParityHostLoad()
	levels := map[string]GatewayParityLevelResult{}
	for _, c := range claimed {
		// Attempt above the floor so a single transport hiccup does not drop
		// RequestsOK under the floor. The gate still requires the floor.
		floor := GatewayParitySampleFloor(c)
		n := floor + floor/10 + 2
		// Optional override for short smoke — RefuseGatewayParitySampleCount will
		// still refuse gate_passed below the floor.
		if v := os.Getenv("MERC_GATEWAY_PARITY_REQUESTS"); v != "" {
			if override, err := strconv.Atoi(v); err == nil && override > 0 {
				n = override
			}
		}
		t.Logf("=== alternating-batch interleaved c=%d n=%d (floor %d) per arm ===", c, n, floor)
		merc, direct := RunGatewayParityInterleavedLevel(
			ctx, client,
			server.URL+"/v1", buyerKey,
			strings.TrimRight(upstream, "/"), upstreamKey,
			body, c, n,
		)
		levels[fmt.Sprintf("merc@c=%d", c)] = merc
		levels[fmt.Sprintf("direct@c=%d", c)] = direct
		t.Logf("  merc: status=%s ok=%d/%d peak=%d ttft_p95=%v",
			merc.Status, merc.RequestsOK, merc.RequestsAttempted, merc.PeakInFlight, merc.TTFTp95)
		t.Logf("  direct: status=%s ok=%d/%d peak=%d ttft_p95=%v",
			direct.Status, direct.RequestsOK, direct.RequestsAttempted, direct.PeakInFlight, direct.TTFTp95)
	}
	hostEnd := CaptureGatewayParityHostLoad()

	identity := ProveGatewayParityBodyIdentity(body, claimed, levels)
	notes := []string{
		"driven by TestGatewayParityAgainstRealEngine using src/control/gateway_parity_harness.go",
		"invalidated path ops/scripts/gateway-parity.py is not invoked",
		fmt.Sprintf("evidence_class=%s (env MERC_GATEWAY_PARITY_EVIDENCE_CLASS)", evidenceClass),
		fmt.Sprintf("catalogue runtime_profile_id=%s used for merc routing/auth only; engine under test is the upstream, not the catalogue engine pin",
			profile.RuntimeProfileID),
		"validateVLLMRuntimeProfile was not relaxed; Metal run is scoped via evidence_class, not profile rewrite",
	}
	rec := BuildGatewayParityReceipt(
		contract, topology, client, claimed, levels, identity,
		DefaultGatewayParityBudget(), hostStart, hostEnd,
		evidenceClass,
		notes,
	)

	raw, err := json.MarshalIndent(rec, "", "  ")
	must(t, err)
	must(t, os.WriteFile(outPath, append(raw, '\n'), 0o644))
	t.Logf("wrote receipt to %s gate_passed=%v verdict=%s comparable=%v evidence_class=%s refusals=%v",
		outPath, rec.GatePassed, rec.Gate.Verdict, rec.Comparable, rec.EvidenceClass, rec.Refusals)

	// Live measure asserts the harness produced a complete receipt; pass/fail
	// of the budget is the scientific claim and must not be silently greenwashed.
	// Operators inspect gate_passed on the written receipt.
	if rec.MeasuredAt == "" {
		t.Fatal("receipt missing measured_at")
	}
	if rec.MercSourceCommit == "" {
		t.Fatal("receipt missing merc_source_commit")
	}
	if rec.GateVersion == "" {
		t.Fatal("receipt missing gate_version")
	}
	if rec.EvidenceClass != evidenceClass && rec.EvidenceClass != "INCOMPLETE_LADDER" {
		t.Fatalf("evidence_class=%s want %s (or INCOMPLETE_LADDER)", rec.EvidenceClass, evidenceClass)
	}
	if rec.SamplingContract.ModelDigest != artifactSHA {
		t.Fatalf("model digest not pinned: %q want %q", rec.SamplingContract.ModelDigest, artifactSHA)
	}
	if evidenceClass == "LOCAL_METAL_PARITY" && len(rec.DoesNotProve) == 0 && rec.EvidenceClass == "LOCAL_METAL_PARITY" {
		t.Fatal("LOCAL_METAL_PARITY receipt missing does_not_prove")
	}
	// Log primary metric per level for the operator.
	for _, gl := range rec.Gate.Levels {
		if gl.TTFTShiftQ95BudgetMs != nil {
			t.Logf("c=%d verdict=%s ttft_shift_q95=%.3f CI[%.3f,%.3f] MDE=%.3f budget=%.3f",
				gl.Concurrency, gl.Verdict, gl.TTFTShiftQ95BudgetMs.Point,
				gl.TTFTShiftQ95BudgetMs.CI95Low, gl.TTFTShiftQ95BudgetMs.CI95High,
				gl.MinimumDetectableEffectMs, gl.TTFTBudgetMs)
		} else {
			t.Logf("c=%d verdict=%s refusals=%v", gl.Concurrency, gl.Verdict, gl.Refusals)
		}
	}
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
