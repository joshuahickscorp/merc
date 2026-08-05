package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"testing"
	"time"
)

// pinnedGGUFDigest is the authority pin for Llama-3.2-1B-Instruct-Q4_K_M.gguf.
const pinnedGGUFDigest = "3f5a22426976ab26cfe84dba63c1d08391717abb1af893e10f1b2968d862dcc1"

// Live measurement against llama.cpp on this host. Skips when llama-server or
// the pinned GGUF is unavailable; never fabricates numbers.
//
// Default backend is Metal on Apple Silicon (empty MERC_LLAMA_DEVICE → -ngl 99).
// The previous default of --device none was a sandbox workaround that polluted
// the metal-local receipt with CPU numbers. CPU is only used when the operator
// sets MERC_LLAMA_DEVICE=none explicitly.
//
// Evidence write is opt-in: set MERC_SERVING_MATRIX_PERF=1 to seal
// evidence/perf/runtime-benchmarks/serving-matrix-llama-cpp-metal-local-r2.json
// (r1 is the superseded CPU-mislabel receipt).
func TestLiveServingMatrixLlamaCppMetal(t *testing.T) {
	if os.Getenv("MERC_RUN_LIVE_SERVING_MATRIX") == "" {
		t.Skip("MERC_RUN_LIVE_SERVING_MATRIX not set")
	}

	llamaServer, err := exec.LookPath("llama-server")
	if err != nil {
		t.Skip("llama-server not on PATH")
	}
	gguf := resolvePinnedGGUF()
	if gguf == "" {
		t.Skip("pinned GGUF not found; set MERC_LLAMA_GGUF")
	}

	sum, err := fileSHA256(gguf)
	mustf(t, err, "hash gguf: %v")
	if sum != pinnedGGUFDigest {
		t.Fatalf("GGUF digest %s != authority pin %s; refusing to measure a different artifact", sum, pinnedGGUFDigest)
	}

	// Device selection:
	//   MERC_LLAMA_DEVICE unset  → Metal (-ngl 99). This is the host truth on
	//                              Apple Silicon; seatbelt sandboxes that cannot
	//                              create a Metal command queue will fail to
	//                              start, which is preferred over a silent CPU
	//                              fallback that re-pollutes a "metal" receipt.
	//   MERC_LLAMA_DEVICE=none   → explicit CPU (--device none)
	//   MERC_LLAMA_DEVICE=<id>   → --device <id>
	//   MERC_LLAMA_DEVICE=""     → same as unset (Metal via -ngl 99); use LookupEnv
	device, deviceSet := os.LookupEnv("MERC_LLAMA_DEVICE")
	useMetal := !deviceSet || device == ""
	if deviceSet && device == "none" {
		useMetal = false
	}

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	must(t, err)
	port := ln.Addr().(*net.TCPAddr).Port
	_ = ln.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 45*time.Minute)
	defer cancel()

	args := []string{
		"-m", gguf,
		"--port", strconv.Itoa(port),
		"--host", "127.0.0.1",
		"-c", "4096",
		"-np", "8",
		"--alias", "llama-3.2-1b-instruct-q4",
		"--fit", "off",
	}
	var deviceLabel string
	switch {
	case useMetal:
		args = append(args, "-ngl", "99")
		deviceLabel = "metal"
	case device == "none":
		args = append(args, "--device", "none")
		deviceLabel = "cpu"
	default:
		args = append(args, "--device", device)
		deviceLabel = device
	}

	var stderrBuf syncBuffer
	cmd := exec.CommandContext(ctx, llamaServer, args...)
	cmd.Stdout = os.Stdout
	cmd.Stderr = io.MultiWriter(os.Stderr, &stderrBuf)
	mustf(t, cmd.Start(), "start llama-server: %v")
	// done is closed when Wait returns so readiness probing can fail fast on exit.
	waitDone := make(chan error, 1)
	go func() { waitDone <- cmd.Wait() }()
	defer func() {
		_ = cmd.Process.Kill()
		select {
		case <-waitDone:
		case <-time.After(2 * time.Second):
		}
	}()

	base := "http://127.0.0.1:" + strconv.Itoa(port) + "/v1"
	if err := waitHTTPOrExit(ctx, base+"/models", 2*time.Minute, waitDone, &stderrBuf); err != nil {
		// Surface Metal command-queue failures clearly: a seatbelt sandbox that
		// blocks MTLCreateSystemDefaultDevice produces this path. Do not
		// silently re-run on CPU and call it metal.
		errLog := stderrBuf.String()
		if useMetal && metalInitFailed(errLog) {
			t.Fatalf("llama-server failed to start on Metal (command queue / context init). "+
				"Apple GPU is present (vendor 0x106b) but this process cannot create a Metal "+
				"command queue — typically a seatbelt sandbox. Re-run unsandboxed (gate profile) "+
				"with MERC_RUN_LIVE_SERVING_MATRIX=1 MERC_SERVING_MATRIX_PERF=1. "+
				"Do not set MERC_LLAMA_DEVICE=none to manufacture a metal-labelled CPU receipt. "+
				"ready_err=%v stderr_tail=%q", err, trimTail(errLog, 800))
		}
		t.Fatalf("llama-server never became ready: %v; stderr_tail=%q", err, trimTail(errLog, 800))
	}

	// Confirm Metal was actually engaged when requested.
	if useMetal {
		errLog := stderrBuf.String()
		if metalInitFailed(errLog) {
			t.Fatalf("Metal command queue failed after ready probe; refusing CPU-fallback numbers: %s", trimTail(errLog, 400))
		}
	}

	arm := ServingMatrixArm{
		Engine:              "llama_cpp",
		RuntimeProfileID:    "llama_cpp_metal",
		CellID:              "llama-cpp-metal-llama1-infer",
		ModelID:             "llama-3.2-1b-instruct-q4",
		ModelDigest:         pinnedGGUFDigest,
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

	hw := hostHardwareIdentity(deviceLabel)
	scope := engineTournamentScope(pinnedGGUFDigest, "Q4_K_M", deviceLabel, []string{"llama_cpp"})

	budget := ServingMatrixBudget{RequireMeasuredAtEveryLevel: true}
	notes := []string{
		fmt.Sprintf("local host evidence on %s (llama-server device_label=%s); numbers are physical measurements, never fabricated", deviceLabel, deviceLabel),
		"supersedes evidence/perf/runtime-benchmarks/serving-matrix-llama-cpp-metal-local-r1.json which was measured on CPU (--device none) under a seatbelt sandbox that could not create a Metal command queue, then labelled metal-local",
		"CUDA engines (vllm/sglang/tensorrt_llm/lmdeploy) are not enterable on this host: no NVIDIA device, docker has no GPU runtime, vLLM publishes zero darwin files across its PyPI releases",
		"MLX cannot load this GGUF; any llama.cpp↔MLX pair is INCOMPARABLE_ARMS under RefuseMismatchedModelDigests (different weight artifacts)",
		"candle can load the same GGUF digest but is in-process only; the serving-matrix harness speaks OpenAI HTTP, so candle is not a same-digest serving-matrix arm here",
		"llama.cpp batch_infer cell stays REJECTED_FOR_CONTRACT for byte_exact; this receipt measures physical throughput only",
		"finding: scripts/gateway-parity.py evaluate_budget only inspects concurrency=1; this harness evaluates every claimed level",
	}
	art := BuildServingMatrixArtifact(
		[]ServingMatrixArm{arm}, selection, cells, budget,
		time.Now().UTC(),
		notes,
	)
	if commit, err := ResolveRepoSourceCommit(".."); err == nil {
		art.MercSourceCommit = commit
	} else {
		t.Fatalf("resolve source commit: %v", err)
	}

	type envelope struct {
		ServingMatrixArtifact
		RuntimeProfileID string         `json:"runtime_profile_id"`
		ProfileRevision  string         `json:"profile_revision"`
		Engine           string         `json:"engine"`
		Transport        string         `json:"transport"`
		ModelID          string         `json:"model_id"`
		ModelDigest      string         `json:"model_digest"`
		Hardware         map[string]any `json:"hardware"`
		TournamentScope  map[string]any `json:"tournament_scope"`
		Supersedes       map[string]any `json:"supersedes,omitempty"`
	}
	out := envelope{
		ServingMatrixArtifact: art,
		RuntimeProfileID:      arm.RuntimeProfileID,
		ProfileRevision:       "r8",
		Engine:                arm.Engine,
		Transport:             "openai_http",
		ModelID:               arm.ModelID,
		ModelDigest:           arm.ModelDigest,
		Hardware:              hw,
		TournamentScope:       scope,
		Supersedes: map[string]any{
			"path": "evidence/perf/runtime-benchmarks/serving-matrix-llama-cpp-metal-local-r1.json",
			"reasons": []string{
				"r1 defaulted to --device none because a seatbelt sandbox could not create a Metal command queue",
				"r1 hardware.note claimed Metal unavailable; host truth is Apple M3 Ultra Metal Supported (vendor 0x106b)",
				"r1 numbers are real CPU measurements and remain as historical record; they are not Metal serving-matrix evidence",
			},
		},
	}

	raw, err := json.Marshal(out)
	must(t, err)
	var payload map[string]any
	must(t, json.Unmarshal(raw, &payload))

	// Opt-in write: verification suite runs must not dirty tracked evidence.
	// Set MERC_SERVING_MATRIX_PERF=1 to seal the Metal (or explicit-CPU) receipt.
	if os.Getenv("MERC_SERVING_MATRIX_PERF") == "1" {
		if useMetal && deviceLabel != "metal" {
			t.Fatal("refusing to seal: Metal was requested but device_label is not metal")
		}
		dest := filepath.Join("..", "evidence", "perf", "runtime-benchmarks",
			"serving-matrix-llama-cpp-metal-local-r2.json")
		id, bin, err := DefaultBoundIdentity("..", "control/serving_matrix_live_test.go",
			"embedded serving matrix cells + tournament_scope", "embedded cells[].metrics samples")
		must(t, err)
		id.ModelArtifactDigest = IdentitySlotValue(arm.ModelDigest)
		id.ImageDigest = IdentitySlotNA("local llama-server process; no container image")
		id.CorpusDigest = IdentitySlotNA("synthetic ServingMatrixPromptCorpus; no external corpus digest")
		if err := WriteBoundEvidenceJSON(EvidenceWriteRequest{
			RepoRoot: "..", Path: dest, Payload: payload,
			Identity: id, BuildBinaryPath: bin,
		}); err != nil {
			t.Fatalf("write evidence: %v", err)
		}
		t.Logf("wrote %s; benchmark_status=%s gate_passed=%v measured_cells=%d device=%s",
			dest, art.BenchmarkStatus, art.Gate.GatePassed, countMeasured(cells), deviceLabel)
	} else {
		t.Logf("skipping evidence write (set MERC_SERVING_MATRIX_PERF=1 to seal); "+
			"benchmark_status=%s measured_cells=%d device=%s",
			art.BenchmarkStatus, countMeasured(cells), deviceLabel)
	}

	if countMeasured(cells) == 0 {
		t.Fatal("live run produced no MEASURED cells")
	}
}

// TestEngineTournamentIncomparableArmsAndHostScope records the honest tournament
// outcome for this host without fabricating cross-engine numbers.
//
// RefuseMismatchedModelDigests is the law: MLX cannot load the pinned GGUF, so
// llama.cpp↔MLX is INCOMPARABLE_ARMS — a result, not a harness failure.
//
// Set MERC_ENGINE_TOURNAMENT_PERF=1 to seal
// evidence/perf/runtime-benchmarks/engine-tournament-metal-host-scope-r1.json.
func TestEngineTournamentIncomparableArmsAndHostScope(t *testing.T) {
	ggufDigest := pinnedGGUFDigest
	// Distinct placeholder digest for "MLX 4-bit weights that are not the GGUF".
	// We do not invent throughput. We only need a different valid sha256 so the
	// comparison rule fires on digest mismatch rather than on empty-field shape.
	// No MLX weights are present on this host to hash; the constant is the
	// documented non-GGUF arm identity used by the refusal path only.
	mlxNonGGUFDigest := "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"

	llamaArm := ServingMatrixArm{
		Engine:           "llama_cpp",
		RuntimeProfileID: "llama_cpp_metal",
		CellID:           "llama-cpp-metal-llama1-infer",
		ModelID:          "llama-3.2-1b-instruct-q4",
		ModelDigest:      ggufDigest,
		Precision:        "Q4_K_M",
	}
	mlxArm := ServingMatrixArm{
		Engine:           "mlx",
		RuntimeProfileID: "mlx_metal",
		CellID:           "mlx-metal-llama1-infer",
		ModelID:          "llama-3.2-1b-instruct-q4", // same model name — not enough
		ModelDigest:      mlxNonGGUFDigest,
		Precision:        "4bit",
	}

	// Multi-arm comparison with mismatched digests → INCOMPARABLE_ARMS.
	sel := LocalEvidenceServingMatrixSelection("Q4_K_M")
	art := BuildServingMatrixArtifact(
		[]ServingMatrixArm{llamaArm, mlxArm},
		sel,
		nil, // no fabricated cells
		ServingMatrixBudget{RequireMeasuredAtEveryLevel: true},
		time.Now().UTC(),
		[]string{
			"tournament result: INCOMPARABLE_ARMS between llama_cpp (GGUF Q4_K_M) and mlx (non-GGUF 4-bit)",
			"RefuseMismatchedModelDigests is not relaxed; same model name does not authorise comparison",
			"MLX cannot load the pinned GGUF; an honest cross-engine comparison therefore refuses",
			"CUDA engines are not enterable on this host (no NVIDIA); listed under not_entered, not as losses",
			"candle shares the GGUF digest with llama_cpp but has no OpenAI HTTP surface for this harness — not a serving-matrix arm",
		},
	)
	if art.Comparable {
		t.Fatal("llama_cpp vs mlx marked comparable despite digest mismatch")
	}
	if art.BenchmarkStatus != "INCOMPARABLE_ARMS" {
		t.Fatalf("status=%s, want INCOMPARABLE_ARMS", art.BenchmarkStatus)
	}
	if len(art.ComparisonRefusals) == 0 {
		t.Fatal("expected comparison_refusals for digest mismatch")
	}
	joined := strings.Join(art.ComparisonRefusals, "; ")
	if !strings.Contains(joined, "digest mismatch") {
		t.Fatalf("refusals did not name digest mismatch: %s", joined)
	}

	// Same-digest candle+llama pair: comparable by digest rule, but candle is
	// not HTTP-enterable for this harness — record that explicitly.
	candleArm := ServingMatrixArm{
		Engine:           "candle",
		RuntimeProfileID: "candle_metal",
		CellID:           "candle-metal-llama1-infer",
		ModelID:          "llama-3.2-1b-instruct-q4",
		ModelDigest:      ggufDigest,
		Precision:        "Q4_K_M",
	}
	sameDigestRefusals := RefuseMismatchedModelDigests([]ServingMatrixArm{llamaArm, candleArm})
	if len(sameDigestRefusals) != 0 {
		t.Fatalf("candle+llama same GGUF digest refused: %v", sameDigestRefusals)
	}

	deviceLabel := "metal"
	if runtime.GOOS != "darwin" || runtime.GOARCH != "arm64" {
		deviceLabel = "unknown_host"
	}
	// Probe whether this process can create a Metal queue (does not measure throughput).
	metalUsable := probeMetalCommandQueue(t)

	// llama.cpp is the only OpenAI-HTTP arm that can enter on this host with the
	// pinned GGUF. It is "entered" only when Metal is usable in this process —
	// the live serving-matrix measurement (TestLiveServingMatrixLlamaCppMetal)
	// is the physical seal; this scope receipt names the entrant honestly.
	var entered []string
	if metalUsable {
		entered = []string{"llama_cpp"}
	}
	scope := engineTournamentScope(ggufDigest, "Q4_K_M", deviceLabel, entered)
	scope["metal_command_queue_usable_in_this_process"] = metalUsable
	if metalUsable {
		scope["entered_measurement"] = "evidence/perf/runtime-benchmarks/serving-matrix-llama-cpp-metal-local-r2.json"
	} else {
		scope["entered_measurement"] = "pending: metal command queue not usable in this process; re-run under unsandboxed gate"
	}
	scope["incomparable_pairs"] = []map[string]any{
		{
			"engines":          []string{"llama_cpp", "mlx"},
			"status":           "INCOMPARABLE_ARMS",
			"model_name_match": true,
			"digest_match":     false,
			"reason":           "MLX cannot load the pinned GGUF; RefuseMismatchedModelDigests requires one identical model sha256 across arms",
			"llama_digest":     ggufDigest,
			"mlx_digest_note":  "non-GGUF 4-bit weights; not present on this host to hash; placeholder digest used only to exercise the mismatch rule",
		},
	}
	scope["same_digest_metal_pairs"] = []map[string]any{
		{
			"engines":                   []string{"candle", "llama_cpp"},
			"model_digest":              ggufDigest,
			"digest_match":              true,
			"serving_matrix_comparable": false,
			"reason":                    "candle loads the same GGUF in-process; serving-matrix harness requires OpenAI HTTP. Prior in-process comparison exists at evidence/perf/runtime-benchmarks/candle-vs-llama-cpp-metal-r3.json (UNBOUND, not this harness).",
		},
	}
	scope["comparison_refusals"] = art.ComparisonRefusals
	scope["benchmark_status"] = art.BenchmarkStatus
	scope["comparable"] = art.Comparable

	commit, err := ResolveRepoSourceCommit("..")
	mustf(t, err, "resolve source commit: %v")
	type envelope struct {
		SchemaVersion      int      `json:"schema_version"`
		Kind               string   `json:"kind"`
		Harness            string   `json:"harness"`
		MeasuredAt         string   `json:"measured_at"`
		MercSourceCommit   string   `json:"merc_source_commit"`
		BenchmarkStatus    string   `json:"benchmark_status"`
		Comparable         bool     `json:"comparable"`
		ComparisonRefusals []string `json:"comparison_refusals"`
		// profiles names every runtime the tournament considered so the
		// embedded evidence-manifest can bind a multi-engine refusal receipt.
		Profiles        map[string]any     `json:"profiles"`
		Arms            []ServingMatrixArm `json:"arms"`
		Notes           []string           `json:"notes"`
		Hardware        map[string]any     `json:"hardware"`
		TournamentScope map[string]any     `json:"tournament_scope"`
		Gate            ServingMatrixGate  `json:"gate"`
	}
	llamaRole := "entered_if_metal_http"
	if metalUsable {
		llamaRole = "entered_metal_http"
	}
	notes := append([]string(nil), art.Notes...)
	if metalUsable {
		notes = append(notes,
			"llama_cpp entered on Metal in this process; physical cells sealed at serving-matrix-llama-cpp-metal-local-r2.json when MERC_SERVING_MATRIX_PERF=1",
			"honest tournament result on this host: single measured OpenAI-HTTP arm (llama_cpp) + documented refusal set; no same-digest multi-engine serving-matrix pair exists",
		)
	} else {
		notes = append(notes,
			"llama_cpp not entered in this process: metal_command_queue_usable_in_this_process=false (seatbelt); re-run unsandboxed to seal r2",
		)
	}
	out := envelope{
		SchemaVersion:      1,
		Kind:               "engine_tournament_host_scope",
		Harness:            servingMatrixHarnessID,
		MeasuredAt:         time.Now().UTC().Format(time.RFC3339),
		MercSourceCommit:   commit,
		BenchmarkStatus:    art.BenchmarkStatus,
		Comparable:         art.Comparable,
		ComparisonRefusals: art.ComparisonRefusals,
		Profiles: map[string]any{
			"llama_cpp_metal": map[string]any{
				"engine": "llama_cpp", "role": llamaRole, "model_digest": ggufDigest,
			},
			"mlx_metal": map[string]any{
				"engine": "mlx", "role": "incomparable_digest", "model_digest_note": "non-GGUF 4-bit",
			},
		},
		Arms:            []ServingMatrixArm{llamaArm, mlxArm},
		Notes:           notes,
		Hardware:        hostHardwareIdentity(deviceLabel),
		TournamentScope: scope,
		Gate:            art.Gate,
	}

	raw, err := json.Marshal(out)
	must(t, err)
	var payload map[string]any
	must(t, json.Unmarshal(raw, &payload))

	if os.Getenv("MERC_ENGINE_TOURNAMENT_PERF") == "1" {
		dest := filepath.Join("..", "evidence", "perf", "runtime-benchmarks",
			"engine-tournament-metal-host-scope-r1.json")
		id, bin, err := DefaultBoundIdentity("..",
			"control/serving_matrix_live_test.go#TestEngineTournamentIncomparableArmsAndHostScope",
			"embedded arms + tournament_scope; no fabricated throughput",
			"N/A: scope/refusal receipt; no request samples")
		must(t, err)
		id.ModelArtifactDigest = IdentitySlotValue(ggufDigest)
		id.ImageDigest = IdentitySlotNA("no container image; host-scope refusal receipt")
		id.CorpusDigest = IdentitySlotNA("no corpus executed; comparison refused before measurement")
		id.RawSamples = IdentitySlotNA("no request samples; INCOMPARABLE_ARMS result with zero fabricated cells")
		if err := WriteBoundEvidenceJSON(EvidenceWriteRequest{
			RepoRoot: "..", Path: dest, Payload: payload,
			Identity: id, BuildBinaryPath: bin,
		}); err != nil {
			t.Fatalf("write evidence: %v", err)
		}
		t.Logf("wrote %s status=%s metal_usable=%v", dest, art.BenchmarkStatus, metalUsable)
	} else {
		t.Logf("skipping evidence write (set MERC_ENGINE_TOURNAMENT_PERF=1 to seal); status=%s metal_usable=%v",
			art.BenchmarkStatus, metalUsable)
	}
}

func resolvePinnedGGUF() string {
	if v := os.Getenv("MERC_LLAMA_GGUF"); v != "" {
		return v
	}
	candidates := []string{
		filepath.Join(os.Getenv("HOME"), ".cache/huggingface/hub/models--unsloth--Llama-3.2-1B-Instruct-GGUF/snapshots/b69aef112e9f895e6f98d7ae0949f72ff09aa401/Llama-3.2-1B-Instruct-Q4_K_M.gguf"),
		filepath.Join(os.Getenv("HOME"), ".cache/huggingface/models--unsloth--Llama-3.2-1B-Instruct-GGUF/snapshots/b69aef112e9f895e6f98d7ae0949f72ff09aa401/Llama-3.2-1B-Instruct-Q4_K_M.gguf"),
	}
	for _, c := range candidates {
		if st, err := os.Stat(c); err == nil && st.Size() > 0 {
			return c
		}
	}
	return ""
}

// hostHardwareIdentity is observational host truth for the receipt envelope.
// It does not claim attestation or a supplier hw_class pin.
func hostHardwareIdentity(deviceLabel string) map[string]any {
	hw := map[string]any{
		"device":      deviceLabel,
		"goos":        runtime.GOOS,
		"goarch":      runtime.GOARCH,
		"nvidia_smi":  "absent",
		"hw_class":    "apple_silicon_ultra",
		"hw_attested": false,
		"note":        "Apple Silicon host; Metal is Supported at the OS level (vendor 0x106b). Seatbelt sandboxes may still fail ggml_metal_init command-queue creation — that is a process restriction, not hardware absence.",
	}
	if runtime.GOOS == "darwin" {
		hw["gpu"] = "Apple M3 Ultra"
		hw["metal"] = "Supported"
		hw["vendor_id"] = "0x106b"
	}
	if deviceLabel == "cpu" {
		hw["note"] = "explicit CPU run (MERC_LLAMA_DEVICE=none). Metal exists on this host; this receipt deliberately measured CPU."
	}
	return hw
}

// engineTournamentScope states what was compared, what was refused, and what
// could not be entered — required for any honest "selects by measured contract"
// claim on this host.
func engineTournamentScope(modelDigest, precision, deviceLabel string, entered []string) map[string]any {
	if entered == nil {
		entered = []string{}
	}
	return map[string]any{
		"hardware_device": deviceLabel,
		"model_digest":    modelDigest,
		"precision":       precision,
		"entered_engines": entered,
		"not_entered": []map[string]any{
			{
				"engine": "vllm",
				"reason": "CUDA-only; no NVIDIA device on this host; nvidia-smi absent; docker runtimes are runc only; vLLM publishes zero darwin files across PyPI releases",
			},
			{
				"engine": "sglang",
				"reason": "CUDA-only; no NVIDIA device / no GPU docker runtime on this host",
			},
			{
				"engine": "tensorrt_llm",
				"reason": "CUDA-only; no NVIDIA device / no GPU docker runtime on this host",
			},
			{
				"engine": "lmdeploy",
				"reason": "CUDA-only; no NVIDIA device / no GPU docker runtime on this host",
			},
			{
				"engine": "candle",
				"reason": "loads the same GGUF digest in-process but has no OpenAI HTTP surface for merc-serving-matrix-v1; not entered as a serving-matrix arm",
			},
			{
				"engine": "mlx",
				"reason": "Metal-capable but cannot load the pinned GGUF; different 4-bit weights would be a different digest → INCOMPARABLE_ARMS, not a tournament win/loss",
			},
		},
		"comparability_rule": "RefuseMismatchedModelDigests: one identical model sha256 across arms; same model name is never enough",
	}
}

// probeMetalCommandQueue returns whether llama-server can initialise Metal in
// this process. Uses a short probe; never fabricates throughput.
func probeMetalCommandQueue(t *testing.T) bool {
	t.Helper()
	if runtime.GOOS != "darwin" {
		return false
	}
	llamaServer, err := exec.LookPath("llama-server")
	if err != nil {
		return false
	}
	gguf := resolvePinnedGGUF()
	if gguf == "" {
		return false
	}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return false
	}
	port := ln.Addr().(*net.TCPAddr).Port
	_ = ln.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 25*time.Second)
	defer cancel()
	var stderrBuf syncBuffer
	cmd := exec.CommandContext(ctx, llamaServer,
		"-m", gguf,
		"--port", strconv.Itoa(port),
		"--host", "127.0.0.1",
		"-c", "512",
		"-np", "1",
		"-ngl", "99",
		"--fit", "off",
	)
	cmd.Stderr = &stderrBuf
	if err := cmd.Start(); err != nil {
		return false
	}
	waitDone := make(chan error, 1)
	go func() { waitDone <- cmd.Wait() }()
	defer func() {
		_ = cmd.Process.Kill()
		select {
		case <-waitDone:
		case <-time.After(2 * time.Second):
		}
	}()
	ready := waitHTTPOrExit(ctx, fmt.Sprintf("http://127.0.0.1:%d/v1/models", port), 20*time.Second, waitDone, &stderrBuf)
	if metalInitFailed(stderrBuf.String()) {
		return false
	}
	return ready == nil
}

func metalInitFailed(errLog string) bool {
	return strings.Contains(errLog, "failed to create command queue") ||
		strings.Contains(errLog, "failed to allocate context") ||
		(strings.Contains(errLog, "ggml_metal_init") && strings.Contains(errLog, "error")) ||
		strings.Contains(errLog, "failed to initialize the context: failed to initialize")
}

func deviceOrCPU(device string) string {
	if device == "" {
		return "metal"
	}
	if device == "none" {
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
	return waitHTTPOrExit(ctx, url, timeout, nil, nil)
}

// waitHTTPOrExit returns when the URL is ready, the context ends, timeout hits,
// or (when waitDone is set) the server process exits. Fails fast on Metal init
// errors visible in stderrBuf rather than spinning the full timeout.
func waitHTTPOrExit(ctx context.Context, url string, timeout time.Duration, waitDone <-chan error, stderrBuf *syncBuffer) error {
	deadline := time.Now().Add(timeout)
	client := &http.Client{Timeout: 2 * time.Second}
	for time.Now().Before(deadline) {
		if stderrBuf != nil && metalInitFailed(stderrBuf.String()) {
			return fmt.Errorf("server metal init failed: %s", trimTail(stderrBuf.String(), 400))
		}
		if waitDone != nil {
			select {
			case <-ctx.Done():
				return ctx.Err()
			case err := <-waitDone:
				if err == nil {
					return fmt.Errorf("server exited before becoming ready")
				}
				return fmt.Errorf("server exited before becoming ready: %w", err)
			default:
			}
		} else {
			select {
			case <-ctx.Done():
				return ctx.Err()
			default:
			}
		}
		req, _ := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
		resp, err := client.Do(req)
		if err == nil {
			resp.Body.Close()
			if resp.StatusCode >= 200 && resp.StatusCode < 500 {
				return nil
			}
		}
		time.Sleep(200 * time.Millisecond)
	}
	return context.DeadlineExceeded
}

func trimTail(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[len(s)-n:]
}
