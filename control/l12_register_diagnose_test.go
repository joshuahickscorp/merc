package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
)

func l12EnsurePriceBoard(t *testing.T) {
	t.Helper()
	if p := strings.TrimSpace(os.Getenv(priceBoardPathEnv)); p != "" {
		if _, err := os.Stat(p); err == nil {
			return
		}
	}
	for _, candidate := range []string{
		filepath.Join("..", "pricing", "board.json"),
		"/tmp/merc-l12/board.json",
	} {
		if _, err := os.Stat(candidate); err == nil {
			abs, aerr := filepath.Abs(candidate)
			mustf(t, aerr, "price board: %v")
			t.Setenv(priceBoardPathEnv, abs)
			return
		}
	}
	t.Fatal("pricing/board.json is not on disk; set MERC_PRICE_BOARD to the governed board")
}

func l12EnsureIsolatedTemplate(t *testing.T) {
	t.Helper()
	if strings.TrimSpace(os.Getenv(isolatedTestDBTemplateEnv)) != "" {
		return
	}
	t.Setenv(isolatedTestDBTemplateEnv, schemaTemplateDatabaseName(canonicalSchemaSHA256()))
}

func l12InferJobBody(maxUSD float64) map[string]any {
	return map[string]any{
		"job_type": map[string]any{"type": "batch_infer", "max_tokens": 16},
		"model":    map[string]any{"kind": "gguf", "ref": "llama-3.2-1b-instruct-q4"},
		"tier":     "batch",
		"input":    `{"id":"l12-0","prompt":"operator-controlled l12 infer rehearsal"}` + "\n",
		"max_usd":  maxUSD,
		"verification": map[string]any{
			"redundancy_frac": 1.0,
			"honeypot_frac":   0.1,
		},
	}
}

func l12EmbedJobBody(maxUSD float64) map[string]any {
	return map[string]any{
		"job_type": map[string]any{"type": "embed"},
		"model":    map[string]any{"kind": "hf", "ref": "all-minilm-l6-v2"},
		"tier":     "batch",
		"input":    `{"id":"l12-embed","text":"operator-controlled l12 embed probe"}` + "\n",
		"max_usd":  maxUSD,
	}
}

func l12WriteReceipt(t *testing.T, name string, doc map[string]any) {
	t.Helper()
	root, err := filepath.Abs("..")
	mustf(t, err, "repo root: %v")
	path := filepath.Join(root, "evidence", "canary", "l12-p1-canary-rehearsal-"+name+".json")
	stamped := map[string]any{
		"schema_version":         1,
		"kind":                   "p1_canary_rehearsal_" + name,
		"gate":                   "P1-CANARY-REHEARSAL",
		"classification":         "ALPHA_CONTROL",
		"does_not_satisfy":       "EXTERNAL_ALPHA_PROVEN",
		"participant_class":      "operator_controlled",
		"synthetic":              true,
		"controlled_by_operator": true,
		"operator_owned":         true,
		"external_alpha_proven":  false,
		"observed_at":            time.Now().UTC().Format(time.RFC3339),
		"rehearsal":              true,
	}
	for k, v := range doc {
		stamped[k] = v
	}
	body, err := json.MarshalIndent(stamped, "", "  ")
	mustf(t, err, "render receipt: %v")
	mustf(t, os.MkdirAll(filepath.Dir(path), 0o755), "mkdir: %v")
	mustf(t, os.WriteFile(path, append(body, '\n'), 0o644), "write %s: %v", path)
	t.Logf("wrote %s", path)
}

func TestL12DiagnoseWorkerProjection(t *testing.T) {
	profile, cell, bench, err := currentRuntimeCellBenchmarkIdentity("candle-metal-llama1-infer")
	if err != nil {
		t.Fatalf("infer identity: %v", err)
	}
	t.Logf("profile=%s cell=%s hash=%s hw=%s unit=%s/%s rate=%.4f",
		profile.RuntimeID, cell.ID, bench.EngineBuildHash, bench.HardwareIdentity,
		bench.Throughput[profile.RuntimeID].Unit,
		bench.Throughput[profile.RuntimeID].UnitScope,
		bench.Throughput[profile.RuntimeID].UnitsPerSecAtOperatingBatch)
	rate := bench.Throughput[profile.RuntimeID].UnitsPerSecAtOperatingBatch
	cap := WorkerCapability{
		HWClass:             bench.HWClass,
		Engine:              "candle",
		BuildHash:           bench.EngineBuildHash,
		BuildIdentityPolicy: bench.EngineBuildIdentityPolicy,
		HardwareIdentity:    bench.HardwareIdentity,
		MemoryGB:            96,
		MemoryBwGbps:        800,
		SupportedJobs:       []string{"batch_infer", "embed", "media_transcode", "media_rendering"},
		SupportedModels: []string{
			"llama-3.2-1b-instruct-q4", "all-minilm-l6-v2",
			"ffmpeg-transcode-v1", "svg-scene-render-v1",
		},
		AgentVersion: "0.1.0",
		OSVersion:    "macos",
		Sandboxed:    true,
		Benchmarks: []BenchResult{{
			ModelID:      "llama-3.2-1b-instruct-q4",
			JobType:      "batch_infer",
			TPS:          float32(rate),
			ThermalOK:    true,
			Unit:         "tokens",
			UnitScope:    performanceUnitScopeTokenLikeInputPlusOutputTokens,
			MeasuredUnix: uint64(time.Now().Unix()),
		}},
	}
	if err := validateWorkerRuntimeProjection(cap); err != nil {
		t.Fatalf("projection refused: %v", err)
	}
}
