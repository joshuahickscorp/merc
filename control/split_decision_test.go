package main

import (
	"math"
	"strings"
	"testing"
)

// Bound embed comparison used as the worker-strength authority for these
// tests. r1/r2 are SUPERSEDED; r3 is the current BOUND receipt in the
// embedded evidence-manifest.
const splitEmbedReceiptR3 = "evidence/perf/runtime-benchmarks/embed-cell-candle-vs-llama-cpp-r3.json"

// MiniLM load and p99 from evidence/autonomous/hardware-characterization.json
// at this commit (M3 Ultra, all-minilm-l6-v2 embed). The file lives under
// evidence/ and is not embedded; the numbers are copied here as citations,
// not as estimates.
const (
	miniLMCharacterizationLoadMS = 229
	miniLMCharacterizationP99MS  = 4
)

func splitEmbedThroughput(t *testing.T, profileID string) (float64, string) {
	t.Helper()
	receipt, ok := benchmarkAuthorityManifest[splitEmbedReceiptR3]
	if !ok {
		t.Fatalf("embedded evidence-manifest has no %s", splitEmbedReceiptR3)
	}
	if !receipt.ThroughputMeasured {
		t.Fatalf("%s is not marked throughput_measured", splitEmbedReceiptR3)
	}
	th, ok := receipt.Throughput[profileID]
	if !ok || th.UnitsPerSecAtOperatingBatch <= 0 {
		t.Fatalf("%s has no operating-batch rate for %s", splitEmbedReceiptR3, profileID)
	}
	if th.Unit != "embeddings" {
		t.Fatalf("%s %s unit=%q, want embeddings", splitEmbedReceiptR3, profileID, th.Unit)
	}
	return th.UnitsPerSecAtOperatingBatch, splitEmbedReceiptR3 + " " + profileID +
		" units_per_sec_at_operating_batch"
}

func splitMiniLMSafetensorsBytes(t *testing.T) int64 {
	t.Helper()
	model, ok := runtimeAuthorityModels["all-minilm-l6-v2"]
	if !ok {
		t.Fatal("runtime authority has no all-minilm-l6-v2")
	}
	for _, a := range model.Artifacts {
		if a.Path == "model.safetensors" {
			if a.Bytes <= 0 {
				t.Fatalf("model.safetensors bytes=%d, want a counted size", a.Bytes)
			}
			return a.Bytes
		}
	}
	t.Fatal("all-minilm-l6-v2 has no model.safetensors artifact")
	return 0
}

func splitEmbedWorkload(t *testing.T, batches int64) SplitWorkload {
	t.Helper()
	if batches <= 0 {
		t.Fatal("batches must be positive")
	}
	// Operating batch on the r3 receipt is 128. Payload bytes scale the
	// paired-cohort corpus (32 records, 2380 exact bytes) so the transfer
	// size is counted from a real JSONL, not guessed from tokens.
	units := batches * 128
	if int64(cohortRecordsPerTask) <= 0 {
		t.Fatal("paired-cohort record count is missing")
	}
	payload := units * int64(cohortCorpusBytes) / int64(cohortRecordsPerTask)
	return SplitWorkload{
		Units:        units,
		PayloadBytes: payload,
		PayloadEvidence: "paired-cohort corpus 2380 bytes / 32 records " +
			"(control/paired_cohort_test.go) scaled by r3 operating_batch=128",
	}
}

func splitEmbedWorkers(t *testing.T, candleStartup, llamaStartup SplitMeasuredSecs) []SplitWorker {
	t.Helper()
	candleRate, candleEv := splitEmbedThroughput(t, "candle_metal")
	llamaRate, llamaEv := splitEmbedThroughput(t, "llama_cpp_metal")
	if candleRate <= llamaRate {
		t.Fatalf("r3 candle rate %v is not stronger than llama.cpp %v; the matched-vs-equal claim needs unequal strength",
			candleRate, llamaRate)
	}
	return []SplitWorker{
		{
			ID: "candle_metal", UnitsPerSec: candleRate, ThroughputEvidence: candleEv,
			Startup: candleStartup,
		},
		{
			ID: "llama_cpp_metal", UnitsPerSec: llamaRate, ThroughputEvidence: llamaEv,
			Startup: llamaStartup,
		},
	}
}

func splitWarmStartup(id string) SplitMeasuredSecs {
	return SplitMeasuredSecs{
		Secs: 0,
		Evidence: id + " already resident: r3 embed comparison was taken on a loaded " +
			"Apple M3 Ultra (hardware_identity in evidence-manifest); no additional load_ms applies",
	}
}

func splitIndependentOverhead() SplitCostEvidence {
	return SplitCostEvidence{
		DuplicateBytes: 0,
		DuplicateEvidence: "both engines were measured on the same Apple M3 Ultra host " +
			"(embed-cell-candle-vs-llama-cpp-r3.json); MiniLM weights are already resident, " +
			"no duplicated artifact to move",
		Assembly: SplitMeasuredSecs{
			Secs: 0,
			Evidence: "independent embedding rows: TopologyIndependentChunks " +
				"(control/topology_planner.go) — JSONL concatenation is not a reduction",
		},
		Verification: SplitMeasuredSecs{
			Secs: float64(miniLMCharacterizationP99MS) / 1000.0,
			Evidence: "evidence/autonomous/hardware-characterization.json all-minilm-l6-v2 " +
				"embed p99_ms=4, charged once as extra independent-check cost of a second shard " +
				"(a worker may not verify its own answer)",
		},
	}
}

func shardFor(plan SplitPlan, id string) SplitShard {
	for _, sh := range plan.Shards {
		if sh.WorkerID == id {
			return sh
		}
	}
	return SplitShard{}
}

func TestSplitDecisionAcceptsMatchedUnequalWorkers(t *testing.T) {
	// 1000 copies of the r3 operating batch: large enough that matched
	// parallelism on the measured candle vs llama.cpp rates pays for a
	// second-shard check, and small enough to stay a multiple of the
	// receipt's own batch.
	workload := splitEmbedWorkload(t, 1000)
	workers := splitEmbedWorkers(t, splitWarmStartup("candle_metal"), splitWarmStartup("llama_cpp_metal"))
	cost := splitIndependentOverhead()

	got := DecideSplit(workload, workers, cost)
	if got.Status != splitDecisionAccepted {
		t.Fatalf("status=%s killed_by=%s missing=%s reason=%s",
			got.Status, got.KilledBy, got.Missing, got.Reason)
	}
	if got.KilledBy != "" || got.Missing != "" {
		t.Fatalf("accepted decision still names a killer: %+v", got)
	}
	if got.Chosen.Kind != splitPlanMatched {
		t.Fatalf("chosen kind=%s, want %s", got.Chosen.Kind, splitPlanMatched)
	}

	candle := shardFor(got.Chosen, "candle_metal")
	llama := shardFor(got.Chosen, "llama_cpp_metal")
	if candle.Units <= llama.Units {
		t.Fatalf("matched shards did not follow strength: candle=%d llama.cpp=%d",
			candle.Units, llama.Units)
	}
	if candle.Units+llama.Units != workload.Units {
		t.Fatalf("shards sum to %d, want %d", candle.Units+llama.Units, workload.Units)
	}
	a := got.Chosen.Arithmetic
	if a.StrongestWorkerID != "candle_metal" {
		t.Fatalf("strongest=%s, want candle_metal (higher r3 rate)", a.StrongestWorkerID)
	}
	if a.UsefulComputeSecs <= 0 {
		t.Fatalf("accepted split has no useful compute: %+v", a)
	}
	if !a.WorthIt() || a.NetSecs <= 0 {
		t.Fatalf("accepted arithmetic is not worth it: %+v", a)
	}
	if a.UsefulComputeSecs <= a.OverheadSecs {
		t.Fatalf("useful %.6f does not exceed overhead %.6f", a.UsefulComputeSecs, a.OverheadSecs)
	}
	t.Logf("ACCEPTED matched: units=%d candle=%d llama=%d serial=%.6fs parallel=%.6fs useful=%.6fs overhead=%.6fs net=%.6fs",
		workload.Units, candle.Units, llama.Units,
		a.SerialComputeSecs, a.ParallelComputeSecs, a.UsefulComputeSecs, a.OverheadSecs, a.NetSecs)
}

func TestSplitDecisionRefusesWhenOverheadExceedsGain(t *testing.T) {
	// The receipt's own 128-text batch. Matched gain is ~11ms; waking the
	// second engine costs the measured MiniLM load_ms=229.
	workload := splitEmbedWorkload(t, 1)
	workers := splitEmbedWorkers(t,
		splitWarmStartup("candle_metal"),
		SplitMeasuredSecs{
			Secs: float64(miniLMCharacterizationLoadMS) / 1000.0,
			Evidence: "evidence/autonomous/hardware-characterization.json all-minilm-l6-v2 " +
				"embed load_ms=229 on Apple M3 Ultra — cold start of the second engine",
		},
	)
	cost := splitIndependentOverhead()
	cost.Verification = SplitMeasuredSecs{
		Secs: 0,
		Evidence: "no extra verification span is charged in this case so startup can be " +
			"named as the dominating term; characterization load_ms is the measured cost",
	}

	got := DecideSplit(workload, workers, cost)
	if got.Status != splitDecisionRefused {
		t.Fatalf("status=%s, want REFUSED: %+v", got.Status, got)
	}
	if got.KilledBy != splitKilledStartup {
		t.Fatalf("killed_by=%q, want %s (startup %.6fs vs useful %.6fs) reason=%s",
			got.KilledBy, splitKilledStartup,
			got.Chosen.Arithmetic.StartupSecs, got.Chosen.Arithmetic.UsefulComputeSecs,
			got.Reason)
	}
	if !strings.Contains(got.Reason, splitKilledStartup) {
		t.Fatalf("refusal reason does not name the dominating term: %s", got.Reason)
	}
	a := got.Chosen.Arithmetic
	if a.UsefulComputeSecs <= 0 {
		t.Fatalf("this case should still have positive matched gain before overhead: %+v", a)
	}
	if a.StartupSecs <= a.UsefulComputeSecs {
		t.Fatalf("startup %.6fs is not larger than useful gain %.6fs", a.StartupSecs, a.UsefulComputeSecs)
	}
	if a.StartupSecs <= a.MovementSecs || a.StartupSecs <= a.AssemblySecs || a.StartupSecs <= a.VerificationSecs {
		t.Fatalf("startup is not the dominating term: %+v", a)
	}
	t.Logf("REFUSED by %s: useful=%.6fs overhead=%.6fs startup=%.6fs movement=%.6fs assembly=%.6fs verification=%.6fs",
		got.KilledBy, a.UsefulComputeSecs, a.OverheadSecs,
		a.StartupSecs, a.MovementSecs, a.AssemblySecs, a.VerificationSecs)
}

func TestSplitDecisionMatchedBeatsEqualShards(t *testing.T) {
	workload := splitEmbedWorkload(t, 1000)
	workers := splitEmbedWorkers(t, splitWarmStartup("candle_metal"), splitWarmStartup("llama_cpp_metal"))
	cost := splitIndependentOverhead()

	got := DecideSplit(workload, workers, cost)
	if got.Chosen.Kind != splitPlanMatched || got.Rival.Kind != splitPlanEqual {
		t.Fatalf("plans: chosen=%s rival=%s", got.Chosen.Kind, got.Rival.Kind)
	}
	matched := got.Chosen.Arithmetic
	equal := got.Rival.Arithmetic
	if matched.NetSecs <= equal.NetSecs {
		t.Fatalf("matched net %.6fs does not beat equal net %.6fs (useful matched=%.6f equal=%.6f)",
			matched.NetSecs, equal.NetSecs, matched.UsefulComputeSecs, equal.UsefulComputeSecs)
	}
	if matched.UsefulComputeSecs <= equal.UsefulComputeSecs {
		t.Fatalf("matched useful %.6fs does not beat equal useful %.6fs",
			matched.UsefulComputeSecs, equal.UsefulComputeSecs)
	}

	eqCandle := shardFor(got.Rival, "candle_metal")
	eqLlama := shardFor(got.Rival, "llama_cpp_metal")
	if math.Abs(float64(eqCandle.Units-eqLlama.Units)) > 1 {
		t.Fatalf("equal plan is not equal: candle=%d llama.cpp=%d", eqCandle.Units, eqLlama.Units)
	}
	mCandle := shardFor(got.Chosen, "candle_metal")
	mLlama := shardFor(got.Chosen, "llama_cpp_metal")
	if mCandle.Units <= mLlama.Units {
		t.Fatalf("matched plan did not give the stronger worker more work: candle=%d llama.cpp=%d",
			mCandle.Units, mLlama.Units)
	}
	// Equal shards on these measured rates make the slow worker the critical
	// path past the single-worker baseline: useful compute is negative.
	if equal.UsefulComputeSecs >= 0 {
		t.Fatalf("expected equal shards to lose to single-worker compute on these rates, useful=%.6f (serial=%.6f parallel=%.6f)",
			equal.UsefulComputeSecs, equal.SerialComputeSecs, equal.ParallelComputeSecs)
	}
	t.Logf("matched useful=%.6fs net=%.6fs shards candle=%d llama=%d; equal useful=%.6fs net=%.6fs shards candle=%d llama=%d",
		matched.UsefulComputeSecs, matched.NetSecs, mCandle.Units, mLlama.Units,
		equal.UsefulComputeSecs, equal.NetSecs, eqCandle.Units, eqLlama.Units)
}

func TestSplitDecisionRefusesLackOfEvidence(t *testing.T) {
	workload := splitEmbedWorkload(t, 1000)
	workers := splitEmbedWorkers(t, splitWarmStartup("candle_metal"), splitWarmStartup("llama_cpp_metal"))
	cost := splitIndependentOverhead()

	t.Run("missing throughput", func(t *testing.T) {
		w := append([]SplitWorker(nil), workers...)
		w[1].UnitsPerSec = 0
		w[1].ThroughputEvidence = ""
		got := DecideSplit(workload, w, cost)
		if got.Status != splitDecisionRefused || got.KilledBy != splitKilledEvidence {
			t.Fatalf("status=%s killed_by=%s, want REFUSED/%s", got.Status, got.KilledBy, splitKilledEvidence)
		}
		if !strings.Contains(got.Missing, "throughput") {
			t.Fatalf("missing=%q, want throughput", got.Missing)
		}
		if got.Chosen.Kind != "" {
			t.Fatalf("lack of evidence still scored a plan: %+v", got.Chosen)
		}
	})

	t.Run("missing startup is not a 120s default", func(t *testing.T) {
		w := append([]SplitWorker(nil), workers...)
		w[1].Startup = SplitMeasuredSecs{} // empty evidence, not a zero measurement
		got := DecideSplit(workload, w, cost)
		if got.Status != splitDecisionRefused || got.KilledBy != splitKilledEvidence {
			t.Fatalf("status=%s killed_by=%s, want REFUSED/%s", got.Status, got.KilledBy, splitKilledEvidence)
		}
		if !strings.Contains(got.Missing, "startup") {
			t.Fatalf("missing=%q, want startup", got.Missing)
		}
	})

	t.Run("duplicate artifact without transfer rate", func(t *testing.T) {
		c := cost
		c.DuplicateBytes = splitMiniLMSafetensorsBytes(t)
		c.DuplicateEvidence = "runtime-authority.json all-minilm-l6-v2 model.safetensors bytes"
		c.TransferBytesPerSec = 0
		c.TransferEvidence = ""
		got := DecideSplit(workload, workers, c)
		if got.Status != splitDecisionRefused || got.KilledBy != splitKilledEvidence {
			t.Fatalf("status=%s killed_by=%s, want REFUSED/%s", got.Status, got.KilledBy, splitKilledEvidence)
		}
		if !strings.Contains(got.Missing, "transfer_bytes_per_sec") {
			t.Fatalf("missing=%q, want transfer_bytes_per_sec (must not invent a link rate)", got.Missing)
		}
	})

	t.Run("missing verification is not zero", func(t *testing.T) {
		c := cost
		c.Verification = SplitMeasuredSecs{Secs: 0, Evidence: ""}
		got := DecideSplit(workload, workers, c)
		if got.Status != splitDecisionRefused || got.KilledBy != splitKilledEvidence {
			t.Fatalf("status=%s killed_by=%s, want REFUSED/%s", got.Status, got.KilledBy, splitKilledEvidence)
		}
		if !strings.Contains(got.Missing, "verification") {
			t.Fatalf("missing=%q, want verification", got.Missing)
		}
	})
}
