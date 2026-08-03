package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"testing"
	"time"
)

// Live measurement against llama.cpp on this host. Skips when llama-server or
// the pinned GGUF is unavailable; never fabricates numbers.
//
// Writes evidence/perf/runtime-benchmarks/serving-matrix-llama-cpp-metal-local-r1.json
// in the existing artifact shape (schema_version, harness, measured_at, runs).
func TestLiveServingMatrixLlamaCppMetal(t *testing.T) {
	if os.Getenv("MERC_RUN_LIVE_SERVING_MATRIX") == "" && testing.Short() {
		t.Skip("set MERC_RUN_LIVE_SERVING_MATRIX=1 (and do not pass -short) to run the live matrix")
	}
	// Always allow explicit opt-in even under -short.
	if os.Getenv("MERC_RUN_LIVE_SERVING_MATRIX") == "" {
		// Default path for CI: skip. Local executor sets the env.
		t.Skip("MERC_RUN_LIVE_SERVING_MATRIX not set")
	}

	llamaServer, err := exec.LookPath("llama-server")
	if err != nil {
		t.Skip("llama-server not on PATH")
	}
	gguf := os.Getenv("MERC_LLAMA_GGUF")
	if gguf == "" {
		candidates := []string{
			filepath.Join(os.Getenv("HOME"), ".cache/huggingface/hub/models--unsloth--Llama-3.2-1B-Instruct-GGUF/snapshots/b69aef112e9f895e6f98d7ae0949f72ff09aa401/Llama-3.2-1B-Instruct-Q4_K_M.gguf"),
			filepath.Join(os.Getenv("HOME"), ".cache/huggingface/models--unsloth--Llama-3.2-1B-Instruct-GGUF/snapshots/b69aef112e9f895e6f98d7ae0949f72ff09aa401/Llama-3.2-1B-Instruct-Q4_K_M.gguf"),
		}
		for _, c := range candidates {
			if st, err := os.Stat(c); err == nil && st.Size() > 0 {
				gguf = c
				break
			}
		}
	}
	if gguf == "" {
		t.Skip("pinned GGUF not found; set MERC_LLAMA_GGUF")
	}

	// Digest must match the authority artifact.
	wantDigest := "3f5a22426976ab26cfe84dba63c1d08391717abb1af893e10f1b2968d862dcc1"
	sum, err := fileSHA256(gguf)
	if err != nil {
		t.Fatalf("hash gguf: %v", err)
	}
	if sum != wantDigest {
		t.Fatalf("GGUF digest %s != authority pin %s; refusing to measure a different artifact", sum, wantDigest)
	}

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	port := ln.Addr().(*net.TCPAddr).Port
	_ = ln.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 45*time.Minute)
	defer cancel()

	// Prefer Metal when available. This sandbox cannot create a Metal command
	// queue, so default to --device none (CPU). Override with MERC_LLAMA_DEVICE
	// (empty string clears --device for host Metal runs). Numbers are still real
	// measurements of this process — never fabricated.
	device := "none"
	if v, ok := os.LookupEnv("MERC_LLAMA_DEVICE"); ok {
		device = v
	}
	args := []string{
		"-m", gguf,
		"--port", strconv.Itoa(port),
		"--host", "127.0.0.1",
		"-c", "4096",
		"-np", "8",
		"--alias", "llama-3.2-1b-instruct-q4",
		"--fit", "off",
	}
	if device != "" {
		args = append(args, "--device", device)
	} else {
		args = append(args, "-ngl", "99")
	}
	cmd := exec.CommandContext(ctx, llamaServer, args...)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	if err := cmd.Start(); err != nil {
		t.Fatalf("start llama-server: %v", err)
	}
	defer func() {
		_ = cmd.Process.Kill()
		_, _ = cmd.Process.Wait()
	}()

	base := "http://127.0.0.1:" + strconv.Itoa(port) + "/v1"
	if err := waitHTTP(ctx, base+"/models", 2*time.Minute); err != nil {
		t.Fatalf("llama-server never became ready: %v", err)
	}

	arm := ServingMatrixArm{
		Engine:              "llama_cpp",
		RuntimeProfileID:    "llama_cpp_metal",
		CellID:              "llama-cpp-metal-llama1-infer",
		ModelID:             "llama-3.2-1b-instruct-q4",
		ModelDigest:         wantDigest,
		Precision:           "Q4_K_M",
		Endpoint:            base,
		SupportedPrecisions: []string{"Q4_K_M"},
		MaxContextTokens:    4096,
		SupportsPrefixHit:   false, // not proven on this profile
		ProviderCostKnown:   false,
	}
	selection := LocalEvidenceServingMatrixSelection(arm.Precision)
	client := &OpenAICompatClient{
		BaseURL: base,
		Model:   "llama-3.2-1b-instruct-q4",
	}

	var cells []ServingMatrixCellResult
	for _, point := range selection.Selected {
		// Honour per-point refusal before paying for a run.
		if reason := RefuseMatrixPoint(arm, point); reason != "" {
			cells = append(cells, ServingMatrixCellResult{
				ArmKey: ArmKey(arm), Point: point, Status: "REFUSED", Reason: reason,
			})
			continue
		}
		cell := RunServingMatrixPoint(ctx, arm, point, client)
		cells = append(cells, cell)
		t.Logf("%s %s → %s %s", ArmKey(arm), point.Key(), cell.Status, cell.Reason)
	}

	budget := ServingMatrixBudget{RequireMeasuredAtEveryLevel: true}
	art := BuildServingMatrixArtifact(
		[]ServingMatrixArm{arm}, selection, cells, budget,
		time.Now().UTC(),
		[]string{
			"local host evidence (CPU --device none in sandbox; Metal on host with MERC_LLAMA_DEVICE=)",
			"CUDA engines (sglang/tensorrt_llm/lmdeploy/vllm) remain DRAFT/unproven — no fabricated numbers",
			"llama.cpp batch_infer cell stays REJECTED_FOR_CONTRACT for byte_exact; this receipt measures physical throughput only",
			"finding: scripts/gateway-parity.py evaluate_budget only inspects concurrency=1; this harness evaluates every claimed level",
		},
	)
	// Also record hardware identity for the existing evidence shape readers.
	type envelope struct {
		ServingMatrixArtifact
		RuntimeProfileID string         `json:"runtime_profile_id"`
		ProfileRevision  string         `json:"profile_revision"`
		Engine           string         `json:"engine"`
		Transport        string         `json:"transport"`
		ModelID          string         `json:"model_id"`
		ModelDigest      string         `json:"model_digest"`
		Hardware         map[string]any `json:"hardware"`
	}
	out := envelope{
		ServingMatrixArtifact: art,
		RuntimeProfileID:      arm.RuntimeProfileID,
		ProfileRevision:       "r8",
		Engine:                arm.Engine,
		Transport:             "openai_http",
		ModelID:               arm.ModelID,
		ModelDigest:           arm.ModelDigest,
		Hardware: map[string]any{
			"device":       deviceOrCPU(device),
			"llama_device": device,
			"note":         "local developer host; hw_class not attested; Metal unavailable in this environment so default is CPU (--device none)",
		},
	}

	raw, err := json.Marshal(out)
	if err != nil {
		t.Fatal(err)
	}
	var payload map[string]any
	if err := json.Unmarshal(raw, &payload); err != nil {
		t.Fatal(err)
	}
	// Write next to the other runtime-benchmark receipts via the bound path.
	dest := filepath.Join("..", "evidence", "perf", "runtime-benchmarks",
		"serving-matrix-llama-cpp-metal-local-r1.json")
	id, bin, err := DefaultBoundIdentity("..", "control/serving_matrix_live_test.go",
		"embedded serving matrix cells", "embedded cell raw samples")
	if err != nil {
		t.Fatal(err)
	}
	if arm.ModelDigest != "" {
		id.ModelArtifactDigest = IdentitySlotValue(arm.ModelDigest)
	}
	if err := WriteBoundEvidenceJSON(EvidenceWriteRequest{
		RepoRoot: "..", Path: dest, Payload: payload,
		Identity: id, BuildBinaryPath: bin,
	}); err != nil {
		t.Fatalf("write evidence: %v", err)
	}
	t.Logf("wrote %s; benchmark_status=%s gate_passed=%v measured_cells=%d",
		dest, art.BenchmarkStatus, art.Gate.GatePassed, countMeasured(cells))

	if countMeasured(cells) == 0 {
		t.Fatal("live run produced no MEASURED cells")
	}
}

func deviceOrCPU(device string) string {
	if device == "" || device == "none" {
		return "cpu"
	}
	return device
}

func countMeasured(cells []ServingMatrixCellResult) int {
	n := 0
	for _, c := range cells {
		if c.Status == "MEASURED" {
			n++
		}
	}
	return n
}

func fileSHA256(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

func waitHTTP(ctx context.Context, url string, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	client := &http.Client{Timeout: 2 * time.Second}
	for time.Now().Before(deadline) {
		select {
		case <-ctx.Done():
			return ctx.Err()
		default:
		}
		req, _ := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
		resp, err := client.Do(req)
		if err == nil {
			resp.Body.Close()
			if resp.StatusCode >= 200 && resp.StatusCode < 500 {
				return nil
			}
		}
		time.Sleep(500 * time.Millisecond)
	}
	return context.DeadlineExceeded
}
