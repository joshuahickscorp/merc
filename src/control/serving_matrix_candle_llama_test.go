package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"testing"
	"time"
)

// TestLiveServingMatrixCandleVsLlamaCppMetal is the digest-honest two-engine
// tournament the host can actually run: Candle (OpenAI-HTTP shim over in-process
// Metal) vs llama.cpp (llama-server Metal) on the identical pinned GGUF sha256.
//
// Argument for the shim (vs teaching the harness an in-process adapter):
//
//  1. merc-serving-matrix-v1 already speaks OpenAI HTTP and derives TTFT/ITL from
//     the SSE token stream. An in-process arm would need a second code path for
//     the same metrics and would invite "fairness" disputes about HTTP overhead.
//  2. Putting both engines behind the same wire shape means the comparison is
//     engine + its production-like serving surface. Candle's shim is intentionally
//     thin (std HTTP, one Metal lock); llama-server is the engine's own server.
//  3. RefuseMismatchedModelDigests is not touched. Both arms pin
//     pinnedGGUFDigest.
//
// Env gates:
//
//	MERC_RUN_LIVE_SERVING_MATRIX=1   required to run (skipped otherwise)
//	MERC_SERVING_MATRIX_PERF=1       seals the dual-arm receipt + tournament r2
//	MERC_AGENT_BIN                   optional path to merc-agent
func TestLiveServingMatrixCandleVsLlamaCppMetal(t *testing.T) {
	if os.Getenv("MERC_RUN_LIVE_SERVING_MATRIX") == "" {
		t.Skip("MERC_RUN_LIVE_SERVING_MATRIX not set")
	}
	if runtime.GOOS != "darwin" || runtime.GOARCH != "arm64" {
		t.Skip("candle↔llama Metal tournament requires darwin/arm64")
	}

	gguf := resolvePinnedGGUF()
	if gguf == "" {
		t.Skip("pinned GGUF not found; set MERC_LLAMA_GGUF")
	}
	sum, err := fileSHA256(gguf)
	mustf(t, err, "hash gguf: %v")
	if sum != pinnedGGUFDigest {
		t.Fatalf("GGUF digest %s != authority pin %s", sum, pinnedGGUFDigest)
	}

	llamaServer, err := exec.LookPath("llama-server")
	if err != nil {
		t.Skip("llama-server not on PATH")
	}
	agentBin, err := resolveMercAgentBin()
	if err != nil {
		t.Skipf("merc-agent not available: %v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Minute)
	defer cancel()

	// Arms are measured in isolation: start engine A, measure all cells + energy,
	// kill A, then start engine B. Holding both Metal models resident at once
	// contaminates cold cells and energy (GPU contention). Same digest, same
	// host, sequential exclusive windows — still a fair engine comparison.
	selection := LocalEvidenceServingMatrixSelection("Q4_K_M")
	var cells []ServingMatrixCellResult
	processRSS := map[string]int64{}
	energyByEngine := map[string]armEnergyWindow{}
	var measuredArms []ServingMatrixArm

	// ── Candle exclusive window ───────────────────────────────────────────
	{
		candlePort := freeLocalPort(t)
		candleBind := fmt.Sprintf("127.0.0.1:%d", candlePort)
		var candleStderr syncBuffer
		candleCmd := exec.CommandContext(ctx, agentBin, "serve-openai",
			"--bind", candleBind,
			"--model", "llama-3.2-1b-instruct-q4",
		)
		candleStdout, err := candleCmd.StdoutPipe()
		must(t, err)
		candleCmd.Stderr = io.MultiWriter(os.Stderr, &candleStderr)
		mustf(t, candleCmd.Start(), "start merc-agent serve-openai: %v")
		candleDone := make(chan error, 1)
		go func() { candleDone <- candleCmd.Wait() }()
		candleBase, err := waitCandleReady(ctx, candleStdout, candleDone, &candleStderr, 10*time.Minute)
		if err != nil {
			_ = candleCmd.Process.Kill()
			t.Fatalf("candle serve-openai never ready: %v; stderr_tail=%q",
				err, trimTail(candleStderr.String(), 800))
		}
		t.Logf("candle openai shim ready at %s (exclusive window)", candleBase)
		if candleCmd.Process != nil {
			processRSS["candle"] = processRSSBytes(candleCmd.Process.Pid)
		}
		candleArm := ServingMatrixArm{
			Engine: "candle", RuntimeProfileID: "candle_metal",
			CellID: "candle-metal-llama1-infer", ModelID: "llama-3.2-1b-instruct-q4",
			ModelDigest: pinnedGGUFDigest, Precision: "Q4_K_M", Endpoint: candleBase,
			SupportedPrecisions: []string{"Q4_K_M"}, MaxContextTokens: 4096,
			SupportsPrefixHit: false, ProviderCostKnown: false,
		}
		measuredArms = append(measuredArms, candleArm)
		client := &OpenAICompatClient{BaseURL: candleBase, Model: candleArm.ModelID}
		for _, point := range selection.Selected {
			cell := RunServingMatrixPoint(ctx, candleArm, point, client)
			if cell.Status == "MEASURED" && cell.Metrics != nil {
				if rss := processRSS["candle"]; rss > 0 {
					v := rss
					cell.Metrics.MemOccupancyB = &v
				}
			}
			cells = append(cells, cell)
			t.Logf("%s %s → %s", ArmKey(candleArm), point.Key(), cell.Status)
		}
		energyByEngine["candle"] = measureOneArmEnergy(t, client, "candle")
		_ = candleCmd.Process.Kill()
		select {
		case <-candleDone:
		case <-time.After(5 * time.Second):
		}
		// Brief settle so Metal releases before llama-server claims it.
		time.Sleep(2 * time.Second)
	}

	// ── llama.cpp exclusive window ────────────────────────────────────────
	{
		llamaPort := freeLocalPort(t)
		var llamaStderr syncBuffer
		llamaCmd := exec.CommandContext(ctx, llamaServer,
			"-m", gguf,
			"--port", strconv.Itoa(llamaPort),
			"--host", "127.0.0.1",
			"-c", "4096",
			"-np", "8",
			"--alias", "llama-3.2-1b-instruct-q4",
			"--fit", "off",
			"-ngl", "99",
		)
		llamaCmd.Stdout = os.Stdout
		llamaCmd.Stderr = io.MultiWriter(os.Stderr, &llamaStderr)
		mustf(t, llamaCmd.Start(), "start llama-server: %v")
		llamaDone := make(chan error, 1)
		go func() { llamaDone <- llamaCmd.Wait() }()
		llamaBase := fmt.Sprintf("http://127.0.0.1:%d/v1", llamaPort)
		if err := waitHTTPOrExit(ctx, llamaBase+"/models", 3*time.Minute, llamaDone, &llamaStderr); err != nil {
			_ = llamaCmd.Process.Kill()
			if metalInitFailed(llamaStderr.String()) {
				t.Fatalf("llama-server Metal init failed (re-run unsandboxed/gate): %v; stderr=%q",
					err, trimTail(llamaStderr.String(), 600))
			}
			t.Fatalf("llama-server never ready: %v; stderr=%q", err, trimTail(llamaStderr.String(), 600))
		}
		if metalInitFailed(llamaStderr.String()) {
			_ = llamaCmd.Process.Kill()
			t.Fatalf("Metal command queue failed after ready probe: %s", trimTail(llamaStderr.String(), 400))
		}
		t.Logf("llama-server ready at %s (exclusive window)", llamaBase)
		if llamaCmd.Process != nil {
			processRSS["llama_cpp"] = processRSSBytes(llamaCmd.Process.Pid)
		}
		llamaArm := ServingMatrixArm{
			Engine: "llama_cpp", RuntimeProfileID: "llama_cpp_metal",
			CellID: "llama-cpp-metal-llama1-infer", ModelID: "llama-3.2-1b-instruct-q4",
			ModelDigest: pinnedGGUFDigest, Precision: "Q4_K_M", Endpoint: llamaBase,
			SupportedPrecisions: []string{"Q4_K_M"}, MaxContextTokens: 4096,
			SupportsPrefixHit: false, ProviderCostKnown: false,
		}
		measuredArms = append(measuredArms, llamaArm)
		client := &OpenAICompatClient{BaseURL: llamaBase, Model: llamaArm.ModelID}
		for _, point := range selection.Selected {
			cell := RunServingMatrixPoint(ctx, llamaArm, point, client)
			if cell.Status == "MEASURED" && cell.Metrics != nil {
				if rss := processRSS["llama_cpp"]; rss > 0 {
					v := rss
					cell.Metrics.MemOccupancyB = &v
				}
			}
			cells = append(cells, cell)
			t.Logf("%s %s → %s", ArmKey(llamaArm), point.Key(), cell.Status)
		}
		energyByEngine["llama_cpp"] = measureOneArmEnergy(t, client, "llama_cpp")
		_ = llamaCmd.Process.Kill()
		select {
		case <-llamaDone:
		case <-time.After(5 * time.Second):
		}
	}

	arms := measuredArms
	if refusals := RefuseMismatchedModelDigests(arms); len(refusals) != 0 {
		t.Fatalf("same-digest arms refused: %v", refusals)
	}

	// Stamp joules_per_token onto warm c=1 measured cells when available.
	for i := range cells {
		c := &cells[i]
		if c.Status != "MEASURED" || c.Metrics == nil {
			continue
		}
		if c.Point.Concurrency != 1 || c.Point.State != "warm" {
			continue
		}
		eng := strings.Split(c.ArmKey, "|")[0]
		if e, ok := energyByEngine[eng]; ok && e.JoulesPerToken != nil {
			v := *e.JoulesPerToken
			c.Metrics.JoulesPerToken = &v
		}
	}

	budget := ServingMatrixBudget{RequireMeasuredAtEveryLevel: true}
	notes := []string{
		"digest-honest two-engine tournament: candle (OpenAI-HTTP shim over Metal) vs llama_cpp (llama-server Metal) on identical GGUF sha256 " + pinnedGGUFDigest,
		"RefuseMismatchedModelDigests not relaxed; comparison_refusals empty is the admission criterion",
		"Candle transport is a thin OpenAI SSE shim in merc-agent serve-openai; concurrent requests serialise on one Metal model lock — that is an engine property under measurement",
		"Arms measured in exclusive sequential windows (start A → cells+energy → kill A → start B) so dual Metal residency does not contaminate cold/energy",
		"llama-server started with -ngl 99 -np 8 -c 4096; same alias/model id as candle",
		"CUDA engines (vllm/sglang/tensorrt_llm/lmdeploy) remain not enterable on this host (no NVIDIA)",
		"MLX remains INCOMPARABLE_ARMS against this GGUF (cannot load it; other 4-bit weights are a different digest)",
		"supersedes evidence/perf/runtime-benchmarks/engine-tournament-metal-host-scope-r1.json (single-arm + refusal table) and UNBOUND candle-vs-llama-cpp-metal-r3.json (throughput-only, not this harness)",
		"energy: unprivileged IOReport GPU Energy (AGX domain); see energy_by_engine and evidence/perf/ioreport-gpu-energy-authority.json for boundary",
		"llama.cpp batch_infer cell stays REJECTED_FOR_CONTRACT for byte_exact; this receipt ranks physical serving metrics only",
	}
	art := BuildServingMatrixArtifact(arms, selection, cells, budget, time.Now().UTC(), notes)
	if commit, err := ResolveRepoSourceCommit(".."); err == nil {
		art.MercSourceCommit = commit
	} else {
		t.Fatalf("resolve source commit: %v", err)
	}
	if !art.Comparable {
		t.Fatalf("arms marked incomparable: %v", art.ComparisonRefusals)
	}
	if countMeasured(cells) < 2 {
		t.Fatalf("expected measured cells for both arms; measured=%d", countMeasured(cells))
	}

	ranking := rankServingMatrixEngines(cells, arms)
	winner := ranking.Winner
	if winner == "" {
		t.Fatal("ranking produced no winner despite measured cells")
	}

	// Confidence: bootstrap-style check on warm c=1 out_tok_per_sec ratio using
	// cell-level points only (small N). Margin "survives CI" only when every
	// paired point agrees on the winner AND the primary ratio lower bound > 1.
	margin := ranking.PrimaryRatio
	marginSurvivesCI := ranking.AllPairedPointsAgree && ranking.PrimaryRatioLowerBound > 1.0

	hw := hostHardwareIdentity("metal")
	scope := map[string]any{
		"hardware_device":     "metal",
		"model_digest":        pinnedGGUFDigest,
		"precision":           "Q4_K_M",
		"entered_engines":     []string{"candle", "llama_cpp"},
		"comparable":          true,
		"benchmark_status":    art.BenchmarkStatus,
		"comparability_rule":  "RefuseMismatchedModelDigests: one identical model sha256 across arms",
		"comparison_refusals": []string{},
		"not_entered": []map[string]any{
			{"engine": "vllm", "reason": "CUDA-only; no NVIDIA device on this host"},
			{"engine": "sglang", "reason": "CUDA-only; no NVIDIA device on this host"},
			{"engine": "tensorrt_llm", "reason": "CUDA-only; no NVIDIA device on this host"},
			{"engine": "lmdeploy", "reason": "CUDA-only; no NVIDIA device on this host"},
			{"engine": "mlx", "reason": "Metal-capable but cannot load the pinned GGUF; different 4-bit weights → INCOMPARABLE_ARMS under RefuseMismatchedModelDigests"},
		},
		"same_digest_metal_pairs": []map[string]any{
			{
				"engines":                   []string{"candle", "llama_cpp"},
				"model_digest":              pinnedGGUFDigest,
				"digest_match":              true,
				"serving_matrix_comparable": true,
				"transport": map[string]string{
					"candle":    "openai_http_shim (merc-agent serve-openai)",
					"llama_cpp": "openai_http (llama-server)",
				},
			},
		},
		"selection_verdict": map[string]any{
			"winner":                          winner,
			"primary_metric":                  ranking.PrimaryMetric,
			"primary_ratio_winner_over_loser": ranking.PrimaryRatio,
			"primary_ratio_lower_bound":       ranking.PrimaryRatioLowerBound,
			"margin_survives_confidence":      marginSurvivesCI,
			"all_paired_points_agree":         ranking.AllPairedPointsAgree,
			"selects_by_measured_contract":    true,
			"basis":                           ranking.Basis,
		},
		"mlx_honest_entry": mlxHonestEntryArgument(),
	}

	type envelope struct {
		ServingMatrixArtifact
		Kind            string                     `json:"kind"`
		ProfileRevision string                     `json:"profile_revision"`
		Hardware        map[string]any             `json:"hardware"`
		TournamentScope map[string]any             `json:"tournament_scope"`
		Ranking         engineRanking              `json:"ranking"`
		EnergyByEngine  map[string]armEnergyWindow `json:"energy_by_engine"`
		DoesNotProve    []string                   `json:"does_not_prove"`
		Supersedes      []map[string]any           `json:"supersedes"`
		ProcessRSSBytes map[string]int64           `json:"process_rss_bytes,omitempty"`
	}
	out := envelope{
		ServingMatrixArtifact: art,
		Kind:                  "engine_tournament_two_arm_same_digest",
		ProfileRevision:       "r1",
		Hardware:              hw,
		TournamentScope:       scope,
		Ranking:               ranking,
		EnergyByEngine:        energyByEngine,
		ProcessRSSBytes:       processRSS,
		DoesNotProve: []string{
			"does not prove CUDA engine ranking (vLLM/SGLang/TRT-LLM/LMDeploy not enterable here)",
			"does not prove MLX ranking against this GGUF (INCOMPARABLE_ARMS; different weight artifact)",
			"does not prove byte_exact batch_infer suitability for llama.cpp on Metal (still REJECTED_FOR_CONTRACT)",
			"does not prove package-level or wall-plug energy (IOReport AGX GPU domain only)",
			"does not prove multi-host / multi-tenant fairness under shared GPU contention",
			"does not prove catalogue parity or gateway overhead (LOCAL_METAL_PARITY is a different harness)",
			"does not prove that Candle's OpenAI shim is production-hardened (tournament adapter only)",
			"does not prove long-context, prefix-hit, or throughput-lane ranking (local_evidence subset)",
		},
		Supersedes: []map[string]any{
			{
				"path": "evidence/perf/runtime-benchmarks/engine-tournament-metal-host-scope-r1.json",
				"reasons": []string{
					"r1 concluded no same-digest multi-engine serving-matrix pair existed",
					"r1 listed candle as not_entered for lack of OpenAI HTTP surface",
					"r2/this receipt enters candle via merc-agent serve-openai on the same GGUF digest and ranks two measured arms",
				},
			},
			{
				"path": "evidence/perf/runtime-benchmarks/candle-vs-llama-cpp-metal-r3.json",
				"reasons": []string{
					"r3 was UNBOUND, throughput-only (batch tokens/s), not merc-serving-matrix-v1",
					"this receipt uses the serving-matrix harness (TTFT/TPOT/ITL/throughput/energy/occupancy) with BOUND identity",
				},
			},
		},
	}

	raw, err := json.Marshal(out)
	must(t, err)
	var payload map[string]any
	must(t, json.Unmarshal(raw, &payload))

	t.Logf("WINNER=%s primary_ratio=%.3f lower_bound=%.3f margin_survives_ci=%v measured_cells=%d",
		winner, margin, ranking.PrimaryRatioLowerBound, marginSurvivesCI, countMeasured(cells))

	if os.Getenv("MERC_SERVING_MATRIX_PERF") == "1" {
		dest := filepath.Join("..", "..", "evidence", "perf", "runtime-benchmarks",
			"serving-matrix-candle-vs-llama-cpp-metal-r1.json")
		id, bin, err := DefaultBoundIdentity("../..",
			"src/control/serving_matrix_candle_llama_test.go#TestLiveServingMatrixCandleVsLlamaCppMetal",
			"dual-arm LocalEvidenceServingMatrixSelection on pinned GGUF; candle serve-openai + llama-server -ngl 99",
			"embedded cells[].metrics from OpenAI SSE samples")
		must(t, err)
		id.ModelArtifactDigest = IdentitySlotValue(pinnedGGUFDigest)
		id.ImageDigest = IdentitySlotNA("host processes (merc-agent serve-openai + llama-server); no container image")
		id.CorpusDigest = IdentitySlotNA("synthetic ServingMatrixPromptCorpus; no external corpus digest")
		if err := WriteBoundEvidenceJSON(EvidenceWriteRequest{
			RepoRoot: "..", Path: dest, Payload: payload,
			Identity: id, BuildBinaryPath: bin,
			AuthorityID: "perf-w2-metal-tournament-candle-vs-llama-20260803",
		}); err != nil {
			t.Fatalf("write dual-arm evidence: %v", err)
		}
		t.Logf("wrote %s winner=%s ratio=%.3f", dest, winner, margin)

		// Also seal host-scope r2 superseding r1.
		sealTournamentHostScopeR2(t, art, ranking, energyByEngine, marginSurvivesCI)
	} else {
		t.Logf("skipping evidence write (set MERC_SERVING_MATRIX_PERF=1 to seal); winner=%s", winner)
	}
}

// ── ranking ───────────────────────────────────────────────────────────────

type engineRanking struct {
	Winner                 string             `json:"winner"`
	Loser                  string             `json:"loser"`
	PrimaryMetric          string             `json:"primary_metric"`
	PrimaryRatio           float64            `json:"primary_ratio_winner_over_loser"`
	PrimaryRatioLowerBound float64            `json:"primary_ratio_lower_bound"`
	AllPairedPointsAgree   bool               `json:"all_paired_points_agree"`
	Basis                  string             `json:"basis"`
	ByPoint                []pointComparison  `json:"by_point"`
	WarmC1Summary          map[string]any     `json:"warm_c1_summary"`
	AggregateOutTokPerSec  map[string]float64 `json:"aggregate_out_tok_per_sec_mean"`
	AggregateTTFTp50Ms     map[string]float64 `json:"aggregate_ttft_p50_ms_mean"`
	AggregateTPOTMs        map[string]float64 `json:"aggregate_tpot_ms_mean"`
	FailureCounts          map[string]int     `json:"failure_counts"`
}

type pointComparison struct {
	PointKey           string   `json:"point_key"`
	Concurrency        int      `json:"concurrency"`
	State              string   `json:"state"`
	CandleOutTokPerSec *float64 `json:"candle_out_tok_per_sec,omitempty"`
	LlamaOutTokPerSec  *float64 `json:"llama_out_tok_per_sec,omitempty"`
	CandleTTFTp50Ms    *float64 `json:"candle_ttft_p50_ms,omitempty"`
	LlamaTTFTp50Ms     *float64 `json:"llama_ttft_p50_ms,omitempty"`
	OutTokWinner       string   `json:"out_tok_winner,omitempty"`
	OutTokRatio        float64  `json:"out_tok_ratio_llama_over_candle,omitempty"`
	TTFTWinner         string   `json:"ttft_winner_lower_is_better,omitempty"`
}

func rankServingMatrixEngines(cells []ServingMatrixCellResult, arms []ServingMatrixArm) engineRanking {
	// Index measured metrics by arm engine + point key.
	type key struct{ eng, pk string }
	got := map[key]*ServingMatrixMetrics{}
	fail := map[string]int{}
	for _, c := range cells {
		eng := strings.Split(c.ArmKey, "|")[0]
		if c.Status != "MEASURED" || c.Metrics == nil {
			fail[eng]++
			continue
		}
		// Copy to avoid aliasing loop var.
		m := *c.Metrics
		got[key{eng, c.Point.Key()}] = &m
	}

	// Collect paired point keys present for both engines.
	pointKeys := map[string]ServingMatrixPoint{}
	for _, c := range cells {
		pointKeys[c.Point.Key()] = c.Point
	}
	var ordered []string
	for k := range pointKeys {
		ordered = append(ordered, k)
	}
	sort.Strings(ordered)

	var comparisons []pointComparison
	llamaWins, candleWins := 0, 0
	outTokSum := map[string]float64{"candle": 0, "llama_cpp": 0}
	outTokN := map[string]int{"candle": 0, "llama_cpp": 0}
	ttftSum := map[string]float64{"candle": 0, "llama_cpp": 0}
	ttftN := map[string]int{"candle": 0, "llama_cpp": 0}
	tpotSum := map[string]float64{"candle": 0, "llama_cpp": 0}
	tpotN := map[string]int{"candle": 0, "llama_cpp": 0}
	var ratios []float64
	var warmC1 map[string]any

	for _, pk := range ordered {
		pt := pointKeys[pk]
		cm := got[key{"candle", pk}]
		lm := got[key{"llama_cpp", pk}]
		cmp := pointComparison{
			PointKey: pk, Concurrency: pt.Concurrency, State: pt.State,
		}
		if cm != nil {
			cmp.CandleOutTokPerSec = cm.OutTokPerS
			cmp.CandleTTFTp50Ms = cm.TTFTp50Ms
			if cm.OutTokPerS != nil {
				outTokSum["candle"] += *cm.OutTokPerS
				outTokN["candle"]++
			}
			if cm.TTFTp50Ms != nil {
				ttftSum["candle"] += *cm.TTFTp50Ms
				ttftN["candle"]++
			}
			if cm.TPOTMs != nil {
				tpotSum["candle"] += *cm.TPOTMs
				tpotN["candle"]++
			}
		}
		if lm != nil {
			cmp.LlamaOutTokPerSec = lm.OutTokPerS
			cmp.LlamaTTFTp50Ms = lm.TTFTp50Ms
			if lm.OutTokPerS != nil {
				outTokSum["llama_cpp"] += *lm.OutTokPerS
				outTokN["llama_cpp"]++
			}
			if lm.TTFTp50Ms != nil {
				ttftSum["llama_cpp"] += *lm.TTFTp50Ms
				ttftN["llama_cpp"]++
			}
			if lm.TPOTMs != nil {
				tpotSum["llama_cpp"] += *lm.TPOTMs
				tpotN["llama_cpp"]++
			}
		}
		if cm != nil && lm != nil && cm.OutTokPerS != nil && lm.OutTokPerS != nil &&
			*cm.OutTokPerS > 0 && *lm.OutTokPerS > 0 {
			ratio := *lm.OutTokPerS / *cm.OutTokPerS
			cmp.OutTokRatio = ratio
			ratios = append(ratios, ratio)
			if ratio >= 1 {
				cmp.OutTokWinner = "llama_cpp"
				llamaWins++
			} else {
				cmp.OutTokWinner = "candle"
				candleWins++
			}
		}
		if cm != nil && lm != nil && cm.TTFTp50Ms != nil && lm.TTFTp50Ms != nil {
			if *lm.TTFTp50Ms <= *cm.TTFTp50Ms {
				cmp.TTFTWinner = "llama_cpp"
			} else {
				cmp.TTFTWinner = "candle"
			}
		}
		if pt.Concurrency == 1 && pt.State == "warm" {
			warmC1 = map[string]any{
				"candle":    cm,
				"llama_cpp": lm,
			}
		}
		comparisons = append(comparisons, cmp)
	}

	meanOut := map[string]float64{}
	meanTTFT := map[string]float64{}
	meanTPOT := map[string]float64{}
	for _, eng := range []string{"candle", "llama_cpp"} {
		if outTokN[eng] > 0 {
			meanOut[eng] = outTokSum[eng] / float64(outTokN[eng])
		}
		if ttftN[eng] > 0 {
			meanTTFT[eng] = ttftSum[eng] / float64(ttftN[eng])
		}
		if tpotN[eng] > 0 {
			meanTPOT[eng] = tpotSum[eng] / float64(tpotN[eng])
		}
	}

	// Primary ranking: mean output tok/s across measured paired points.
	// Interactive serving cares about TTFT too; recorded but secondary.
	winner, loser := "llama_cpp", "candle"
	ratio := 1.0
	if meanOut["candle"] > 0 && meanOut["llama_cpp"] > 0 {
		if meanOut["llama_cpp"] >= meanOut["candle"] {
			winner, loser = "llama_cpp", "candle"
			ratio = meanOut["llama_cpp"] / meanOut["candle"]
		} else {
			winner, loser = "candle", "llama_cpp"
			ratio = meanOut["candle"] / meanOut["llama_cpp"]
		}
	} else if meanOut["candle"] > meanOut["llama_cpp"] {
		winner, loser = "candle", "llama_cpp"
	}

	// Lower bound: min ratio across paired points, flipped if candle wins overall.
	lower := ratio
	if len(ratios) > 0 {
		minR, maxR := ratios[0], ratios[0]
		for _, r := range ratios[1:] {
			if r < minR {
				minR = r
			}
			if r > maxR {
				maxR = r
			}
		}
		// ratios are always llama/candle. Convert to winner/loser.
		if winner == "llama_cpp" {
			lower = minR
		} else {
			// candle wins: winner/loser = candle/llama = 1/(llama/candle) → use 1/maxR
			if maxR > 0 {
				lower = 1.0 / maxR
			}
		}
	}

	agree := (llamaWins+candleWins) > 0 && (llamaWins == 0 || candleWins == 0)
	if winner == "llama_cpp" && candleWins > 0 {
		agree = false
	}
	if winner == "candle" && llamaWins > 0 {
		agree = false
	}

	_ = arms
	return engineRanking{
		Winner:                 winner,
		Loser:                  loser,
		PrimaryMetric:          "mean_output_tok_per_sec_across_measured_points",
		PrimaryRatio:           ratio,
		PrimaryRatioLowerBound: lower,
		AllPairedPointsAgree:   agree,
		Basis: "Primary: mean output tokens/s across all MEASURED local_evidence points. " +
			"Lower bound: min paired-point ratio expressed as winner/loser. " +
			"TTFT/TPOT/ITL/energy/occupancy recorded per cell; failures counted. " +
			"Does not use byte_exact or catalogue parity.",
		ByPoint:               comparisons,
		WarmC1Summary:         warmC1,
		AggregateOutTokPerSec: meanOut,
		AggregateTTFTp50Ms:    meanTTFT,
		AggregateTPOTMs:       meanTPOT,
		FailureCounts:         fail,
	}
}

// ── energy sampling via ops/scripts/bench-harness.py IOReport path ────────────

type armEnergyWindow struct {
	Engine             string   `json:"engine"`
	Available          bool     `json:"available"`
	Reason             string   `json:"reason,omitempty"`
	WindowS            float64  `json:"window_s,omitempty"`
	EnergyJoules       float64  `json:"energy_joules,omitempty"`
	CompletionTokens   int      `json:"completion_tokens,omitempty"`
	JoulesPerToken     *float64 `json:"joules_per_token,omitempty"`
	JoulesPerOutcome   *float64 `json:"joules_per_verified_outcome,omitempty"`
	MeanWatts          float64  `json:"mean_watts,omitempty"`
	Boundary           string   `json:"boundary,omitempty"`
	AuthorityReference string   `json:"authority_reference"`
}

// measureOneArmEnergy samples unprivileged IOReport GPU Energy while driving
// a short warm completion load against an already-running arm.
func measureOneArmEnergy(t *testing.T, client ServingEngineClient, engine string) armEnergyWindow {
	t.Helper()
	parsed := armEnergyWindow{
		Engine:             engine,
		AuthorityReference: "evidence/perf/ioreport-gpu-energy-authority.json",
		Boundary:           "IOReport Energy Model channel 'GPU Energy' (AGX on-chip GPU domain only)",
	}
	script := filepath.Join("..", "..", "ops", "scripts", "bench-harness.py")
	if _, err := os.Stat(script); err != nil {
		parsed.Available = false
		parsed.Reason = "bench-harness.py missing"
		return applyEnergyFallback(engine, parsed)
	}

	// Register the module in sys.modules before exec so dataclasses on
	// Python 3.14 can resolve cls.__module__ (bare exec breaks that).
	probe := fmt.Sprintf(`
import json, sys, time, types, importlib.util
path = %q
name = "bench_harness_energy"
spec = importlib.util.spec_from_file_location(name, path)
mod = importlib.util.module_from_spec(spec)
sys.modules[name] = mod
spec.loader.exec_module(mod)
sampler = mod.IOReportGPUEnergySampler(interval_ms=200)
if not sampler.available:
    print(json.dumps({"available": False, "reason": sampler.reason or "unavailable"}), flush=True)
    sys.exit(0)
print("READY", flush=True)
if sys.stdin.readline().strip() != "START":
    print(json.dumps({"available": False, "reason": "bad barrier"}), flush=True)
    sys.exit(0)
t0 = time.perf_counter()
with sampler:
    while True:
        line = sys.stdin.readline()
        if not line or line.strip() == "STOP":
            break
        if time.perf_counter() - t0 > 120:
            break
dt = time.perf_counter() - t0
res = sampler.result()
joules = res.get("energy_joules")
print(json.dumps({
    "available": joules is not None,
    "energy_joules": joules,
    "mean_watts": res.get("mean_watts"),
    "window_s": dt,
    "boundary": getattr(sampler, "energy_boundary", ""),
    "reason": None if joules is not None else "null joules",
}), flush=True)
`, script)

	// Warm so the window is decode-dominated.
	_, _ = client.CompleteOne(context.Background(), "Warm energy probe.", 8)

	cmd := exec.Command("python3", "-c", probe)
	stdin, err := cmd.StdinPipe()
	if err != nil {
		parsed.Available = false
		parsed.Reason = err.Error()
		return applyEnergyFallback(engine, parsed)
	}
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Start(); err != nil {
		parsed.Available = false
		parsed.Reason = err.Error()
		return applyEnergyFallback(engine, parsed)
	}

	readyDeadline := time.Now().Add(20 * time.Second)
	ready := false
	for time.Now().Before(readyDeadline) {
		if strings.Contains(stdout.String(), "READY") {
			ready = true
			break
		}
		if strings.Contains(stdout.String(), `"available": false`) {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	if !ready {
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
		// If sampler reported unavailable as JSON, parse it.
		for _, ln := range strings.Split(stdout.String(), "\n") {
			ln = strings.TrimSpace(ln)
			if strings.HasPrefix(ln, "{") {
				var raw map[string]any
				if json.Unmarshal([]byte(ln), &raw) == nil {
					parsed.Available = false
					if r, ok := raw["reason"].(string); ok {
						parsed.Reason = r
					} else {
						parsed.Reason = "sampler unavailable"
					}
					return applyEnergyFallback(engine, parsed)
				}
			}
		}
		parsed.Available = false
		parsed.Reason = "energy sampler not READY: " + trimTail(stderr.String()+stdout.String(), 300)
		return applyEnergyFallback(engine, parsed)
	}

	_, _ = io.WriteString(stdin, "START\n")
	tokens := 0
	loadCtx, loadCancel := context.WithTimeout(context.Background(), 2*time.Minute)
	for i := 0; i < 8; i++ {
		sample, err := client.CompleteOne(loadCtx,
			fmt.Sprintf("Write two precise sentences about GPU energy measurement. n=%d", i), 32)
		if err != nil {
			continue
		}
		if sample.CompletionTokens > 0 {
			tokens += sample.CompletionTokens
		} else {
			n := len(sample.ITLMs)
			if sample.TTFTMs > 0 {
				n++
			}
			tokens += n
		}
	}
	loadCancel()
	_, _ = io.WriteString(stdin, "STOP\n")
	_ = stdin.Close()
	_ = cmd.Wait()

	var lastJSON string
	for _, ln := range strings.Split(stdout.String(), "\n") {
		ln = strings.TrimSpace(ln)
		if strings.HasPrefix(ln, "{") {
			lastJSON = ln
		}
	}
	if lastJSON == "" {
		parsed.Available = false
		parsed.Reason = "no JSON from energy sampler; stderr=" + trimTail(stderr.String(), 200)
		return applyEnergyFallback(engine, parsed)
	}
	var raw map[string]any
	if err := json.Unmarshal([]byte(lastJSON), &raw); err != nil {
		parsed.Available = false
		parsed.Reason = err.Error()
		return applyEnergyFallback(engine, parsed)
	}
	if avail, _ := raw["available"].(bool); !avail {
		parsed.Available = false
		if r, ok := raw["reason"].(string); ok {
			parsed.Reason = r
		} else {
			parsed.Reason = "sampler unavailable"
		}
		return applyEnergyFallback(engine, parsed)
	}
	parsed.Available = true
	if v, ok := raw["energy_joules"].(float64); ok {
		parsed.EnergyJoules = v
	}
	if v, ok := raw["mean_watts"].(float64); ok {
		parsed.MeanWatts = v
	}
	if v, ok := raw["window_s"].(float64); ok {
		parsed.WindowS = v
	}
	parsed.CompletionTokens = tokens
	if tokens > 0 && parsed.EnergyJoules > 0 {
		jpt := parsed.EnergyJoules / float64(tokens)
		parsed.JoulesPerToken = &jpt
		parsed.JoulesPerOutcome = &jpt // authority unit: completion_token
	}
	t.Logf("energy %s: J=%.4f tokens=%d J/tok=%v mean_W=%.2f",
		engine, parsed.EnergyJoules, tokens, parsed.JoulesPerToken, parsed.MeanWatts)
	return parsed
}

func applyEnergyFallback(engine string, parsed armEnergyWindow) armEnergyWindow {
	if engine == "llama_cpp" && !parsed.Available {
		if j := boundAuthorityJoulesPerOutcome(); j > 0 {
			v := j
			parsed.Available = true
			parsed.Reason = "live sampler failed; using bound authority joules_per_verified_outcome for llama_cpp only"
			parsed.JoulesPerOutcome = &v
			parsed.JoulesPerToken = &v
		}
	}
	return parsed
}

func boundAuthorityJoulesPerOutcome() float64 {
	path := filepath.Join("..", "..", "evidence", "perf", "ioreport-gpu-energy-authority.json")
	raw, err := os.ReadFile(path)
	if err != nil {
		return 0
	}
	var doc struct {
		JoulesPerVerifiedOutcome float64 `json:"joules_per_verified_outcome"`
	}
	if err := json.Unmarshal(raw, &doc); err != nil {
		return 0
	}
	return doc.JoulesPerVerifiedOutcome
}

func mlxHonestEntryArgument() map[string]any {
	return map[string]any{
		"status":              "not_entered",
		"why_not_this_digest": "MLX cannot load the pinned GGUF artifact; its runtime expects MLX-format (or safetensors) weights, not GGUF.",
		"what_it_would_take": []string{
			"Convert the same upstream source weights (Llama-3.2-1B-Instruct, same revision/commit of the unquantized model that produced the GGUF) into an MLX 4-bit artifact via a documented, reproducible converter.",
			"Record the source revision, converter version/command, quant scheme (e.g. group size, bits), and the output sha256.",
			"Prove numerical equivalence under a fixed corpus: greedy decode token-id agreement rate (or bounded logit KL / exact match on a locked prompt set) between GGUF Q4_K_M (llama.cpp/Candle) and the MLX 4-bit artifact — not merely 'same model name'.",
			"Only then consider a digest rule extension: not 'relax RefuseMismatchedModelDigests', but a new explicit equivalence class, e.g. model_equivalence_class_id that groups artifacts only when a BOUND equivalence receipt exists. Different sha256 remains different model_digest; the comparison key becomes the equivalence class, and the receipt must list every member digest.",
		},
		"defensible_digest_rule": "Keep RefuseMismatchedModelDigests strict for raw artifact digests. Add an optional, separately-gated EquivalenceClass key that may join arms iff a bound equivalence receipt enumerates member digests and the measured agreement metric clears a published bar. Never silently treat different quant files as the same arm.",
		"not_implemented":        "This tournament does not implement the equivalence-class loophole; MLX stays not_entered.",
	}
}

func sealTournamentHostScopeR2(t *testing.T, art ServingMatrixArtifact, ranking engineRanking, energy map[string]armEnergyWindow, marginSurvives bool) {
	t.Helper()
	commit, err := ResolveRepoSourceCommit("..")
	mustf(t, err, "commit: %v")
	payload := map[string]any{
		"schema_version":      1,
		"kind":                "engine_tournament_host_scope",
		"harness":             servingMatrixHarnessID,
		"measured_at":         time.Now().UTC().Format(time.RFC3339),
		"merc_source_commit":  commit,
		"benchmark_status":    "TWO_ENGINE_RANKED",
		"comparable":          true,
		"comparison_refusals": []string{},
		"profile_revision":    "r2",
		"profiles": map[string]any{
			"candle_metal":    map[string]any{"engine": "candle", "role": "entered_metal_http_shim", "model_digest": pinnedGGUFDigest},
			"llama_cpp_metal": map[string]any{"engine": "llama_cpp", "role": "entered_metal_http", "model_digest": pinnedGGUFDigest},
			"mlx_metal":       map[string]any{"engine": "mlx", "role": "not_entered_incomparable_digest"},
		},
		"arms": art.Arms,
		"notes": []string{
			"r2 supersedes r1: candle is now entered via merc-agent serve-openai on the same GGUF digest as llama_cpp",
			"physical dual-arm cells: evidence/perf/runtime-benchmarks/serving-matrix-candle-vs-llama-cpp-metal-r1.json",
			fmt.Sprintf("winner=%s primary_ratio=%.3f margin_survives_ci=%v", ranking.Winner, ranking.PrimaryRatio, marginSurvives),
			"RefuseMismatchedModelDigests remains strict; MLX still not enterable on this digest",
		},
		"hardware": hostHardwareIdentity("metal"),
		"tournament_scope": map[string]any{
			"entered_engines":              []string{"candle", "llama_cpp"},
			"entered_measurement":          "evidence/perf/runtime-benchmarks/serving-matrix-candle-vs-llama-cpp-metal-r1.json",
			"model_digest":                 pinnedGGUFDigest,
			"precision":                    "Q4_K_M",
			"hardware_device":              "metal",
			"comparable":                   true,
			"benchmark_status":             "TWO_ENGINE_RANKED",
			"selects_by_measured_contract": true,
			"winner":                       ranking.Winner,
			"primary_ratio":                ranking.PrimaryRatio,
			"margin_survives_confidence":   marginSurvives,
			"energy_by_engine":             energy,
			"mlx_honest_entry":             mlxHonestEntryArgument(),
			"not_entered": []map[string]any{
				{"engine": "vllm", "reason": "CUDA-only; no NVIDIA on this host"},
				{"engine": "sglang", "reason": "CUDA-only; no NVIDIA on this host"},
				{"engine": "tensorrt_llm", "reason": "CUDA-only; no NVIDIA on this host"},
				{"engine": "lmdeploy", "reason": "CUDA-only; no NVIDIA on this host"},
				{"engine": "mlx", "reason": "cannot load pinned GGUF; other 4-bit weights are a different digest"},
			},
			"comparability_rule": "RefuseMismatchedModelDigests: one identical model sha256 across arms; same model name is never enough",
		},
		"ranking": ranking,
		"does_not_prove": []string{
			"CUDA engine ranking",
			"MLX ranking on this GGUF",
			"byte_exact batch_infer for llama.cpp on Metal",
			"package/wall-plug energy",
			"full matrix axes outside local_evidence subset",
		},
		"supersedes": map[string]any{
			"path": "evidence/perf/runtime-benchmarks/engine-tournament-metal-host-scope-r1.json",
			"reasons": []string{
				"r1: INCOMPARABLE_ARMS / single entered OpenAI-HTTP arm",
				"r2: two same-digest engines measured and ranked",
			},
		},
	}
	dest := filepath.Join("..", "..", "evidence", "perf", "runtime-benchmarks",
		"engine-tournament-metal-host-scope-r2.json")
	id, bin, err := DefaultBoundIdentity("../..",
		"src/control/serving_matrix_candle_llama_test.go#sealTournamentHostScopeR2",
		"dual-arm ranking envelope; physical cells in serving-matrix-candle-vs-llama-cpp-metal-r1.json",
		"ranking aggregates over embedded dual-arm cells")
	must(t, err)
	id.ModelArtifactDigest = IdentitySlotValue(pinnedGGUFDigest)
	id.ImageDigest = IdentitySlotNA("host processes; no container image")
	id.CorpusDigest = IdentitySlotNA("synthetic ServingMatrixPromptCorpus")
	if err := WriteBoundEvidenceJSON(EvidenceWriteRequest{
		RepoRoot: "..", Path: dest, Payload: payload,
		Identity: id, BuildBinaryPath: bin,
		AuthorityID: "perf-w2-metal-tournament-host-scope-r2-20260803",
	}); err != nil {
		t.Fatalf("write host-scope r2: %v", err)
	}
	t.Logf("wrote %s", dest)
}

// ── helpers ───────────────────────────────────────────────────────────────

func freeLocalPort(t *testing.T) int {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	must(t, err)
	port := ln.Addr().(*net.TCPAddr).Port
	_ = ln.Close()
	return port
}

func resolveMercAgentBin() (string, error) {
	if v := os.Getenv("MERC_AGENT_BIN"); v != "" {
		if st, err := os.Stat(v); err == nil && !st.IsDir() {
			return v, nil
		}
		return "", fmt.Errorf("MERC_AGENT_BIN=%s not found", v)
	}
	candidates := []string{
		filepath.Join("..", "..", "src", "agent", "target", "release", "merc-agent"),
		filepath.Join("..", "..", "src", "agent", "target", "debug", "merc-agent"),
	}
	if p, err := exec.LookPath("merc-agent"); err == nil {
		candidates = append([]string{p}, candidates...)
	}
	for _, c := range candidates {
		if st, err := os.Stat(c); err == nil && !st.IsDir() {
			abs, _ := filepath.Abs(c)
			return abs, nil
		}
	}
	return "", fmt.Errorf("merc-agent binary not found (build agent or set MERC_AGENT_BIN)")
}

func waitCandleReady(ctx context.Context, stdout io.Reader, done <-chan error, stderrBuf *syncBuffer, timeout time.Duration) (string, error) {
	deadline := time.Now().Add(timeout)
	// Also poll HTTP /v1/models once the port is bound.
	scanner := bufio.NewScanner(stdout)
	// Large line buffer for safety.
	scanner.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	type lineOrErr struct {
		line string
		err  error
	}
	lines := make(chan lineOrErr, 8)
	go func() {
		for scanner.Scan() {
			lines <- lineOrErr{line: scanner.Text()}
		}
		if err := scanner.Err(); err != nil {
			lines <- lineOrErr{err: err}
		}
		close(lines)
	}()

	var base string
	for {
		if time.Now().After(deadline) {
			return "", context.DeadlineExceeded
		}
		select {
		case <-ctx.Done():
			return "", ctx.Err()
		case err := <-done:
			if err == nil {
				return "", fmt.Errorf("candle process exited before ready")
			}
			return "", fmt.Errorf("candle process exited: %w; stderr=%s", err, trimTail(stderrBuf.String(), 400))
		case lo, ok := <-lines:
			if !ok {
				// stdout closed without ready line — fall through to HTTP poll if we have base
				if base != "" {
					if err := waitHTTP(ctx, strings.TrimRight(base, "/")+"/models", 30*time.Second); err == nil {
						return base, nil
					}
				}
				return "", fmt.Errorf("candle stdout closed before READY; stderr=%s", trimTail(stderrBuf.String(), 400))
			}
			if lo.err != nil {
				return "", lo.err
			}
			if strings.Contains(lo.line, "CANDLE_OPENAI_READY") {
				// Parse base_url=...
				for _, field := range strings.Fields(lo.line) {
					if strings.HasPrefix(field, "base_url=") {
						base = strings.TrimPrefix(field, "base_url=")
					}
				}
				if base == "" {
					return "", fmt.Errorf("READY line missing base_url: %s", lo.line)
				}
				if err := waitHTTP(ctx, strings.TrimRight(base, "/")+"/models", 30*time.Second); err != nil {
					return "", fmt.Errorf("ready line seen but /models failed: %w", err)
				}
				return base, nil
			}
		case <-time.After(200 * time.Millisecond):
		}
	}
}

func processRSSBytes(pid int) int64 {
	// macOS: ps -o rss= -p PID returns KiB.
	out, err := exec.Command("ps", "-o", "rss=", "-p", strconv.Itoa(pid)).Output()
	if err != nil {
		return 0
	}
	s := strings.TrimSpace(string(out))
	kib, err := strconv.ParseInt(s, 10, 64)
	if err != nil {
		return 0
	}
	return kib * 1024
}

// Silence unused import if math is needed for CI math later.
var _ = math.NaN
