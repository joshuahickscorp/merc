package main

import (
	"testing"
	"time"
)

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
