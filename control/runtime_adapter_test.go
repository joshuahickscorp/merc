package main

import (
	"strings"
	"testing"
)

func adapterTestWorker() WorkerCapability {
	return WorkerCapability{
		Engine:          "candle",
		HWClass:         "apple_silicon_ultra",
		MemoryGB:        96,
		SupportedJobs:   []string{"embed", "batch_infer"},
		SupportedModels: []string{"all-minilm-l6-v2", "llama-3.2-1b-instruct-q4"},
		Benchmarks: []BenchResult{
			{JobType: "batch_infer", ModelID: "llama-3.2-1b-instruct-q4", TPS: 285},
			{JobType: "embed", ModelID: "all-minilm-l6-v2", EPS: 1200},
		},
	}
}

func inferWorkload() RuntimeWorkload {
	return RuntimeWorkload{
		JobType: "batch_infer", ModelRef: "llama-3.2-1b-instruct-q4", MinimumMemoryGB: 4,
	}
}

// Every registered engine must have an adapter, and an unregistered one must
// fail closed. Guessing which of the document and the registry is right is how
// an ungoverned runtime gets admitted.
func TestEveryRegisteredEngineHasAnAdapterAndUnknownOnesFailClosed(t *testing.T) {
	for _, profile := range runtimeAuthority.Runtimes {
		adapter, err := AdapterForProfile(profile)
		if err != nil {
			t.Errorf("profile %q has no adapter: %v", profile.RuntimeID, err)
			continue
		}
		if adapter.ID() != profile.Adapter {
			t.Errorf("profile %q resolved adapter %q, want %q",
				profile.RuntimeID, adapter.ID(), profile.Adapter)
		}
		if err := adapter.ValidateProfile(profile); err != nil {
			t.Errorf("adapter rejected its own profile %q: %v", profile.RuntimeID, err)
		}
	}

	rogue, _ := runtimeProfileByID("candle_metal")
	rogue.Adapter = "merc-not-registered"
	if _, err := AdapterForProfile(rogue); err == nil {
		t.Fatal("an unregistered adapter resolved")
	}

	// An adapter must refuse a profile that names a different one, or the
	// registry lookup would be the only thing keeping them aligned.
	candleAdapter := runtimeAdapters["merc-candle"]
	mismatched, _ := runtimeProfileByID("mlx_metal")
	if err := candleAdapter.ValidateProfile(mismatched); err == nil {
		t.Fatal("the candle adapter accepted an mlx profile")
	}
}

// The load-bearing distinction in RuntimeEstimate: a worker's self-reported
// benchmark and a profile's receipt-bound benchmark authority are different
// kinds of claim. A tournament that treats them as interchangeable is rigged
// before it starts.
func TestEstimateNeverCallsAnUnbenchmarkedProfileComparable(t *testing.T) {
	cap := adapterTestWorker()
	estimates, err := EstimateAcrossRegisteredRuntimes(inferWorkload(), cap)
	mustf(t, err, "EstimateAcrossRegisteredRuntimes: %v")
	if len(estimates) != len(runtimeAuthority.Runtimes) {
		t.Fatalf("%d estimates for %d registered runtimes",
			len(estimates), len(runtimeAuthority.Runtimes))
	}

	byID := map[string]RuntimeEstimate{}
	for _, e := range estimates {
		byID[e.RuntimeProfileID] = e
	}

	candle := byID["candle_metal"]
	if !candle.Supported || !candle.Routable {
		t.Fatalf("candle_metal estimate = %+v, want supported and routable", candle)
	}
	// candle_metal is now measured on the same harness as its challenger, so it
	// is comparable. It was NOT for one commit, when its receipt named the
	// profile and measured nothing — Comparable tracks the measurement, not the
	// existence of a citation.
	if !candle.Comparable {
		t.Error("candle_metal has a measured receipt but is not comparable")
	}
	if candle.TokensPerSec != 285 {
		t.Errorf("candle_metal tokens/sec = %v, want the worker's 285", candle.TokensPerSec)
	}
	// The rate came from the agent, not from the profile's benchmark receipt.
	// Labelling it as profile authority because the profile happens to have one
	// would launder a self-report into evidence.
	if candle.ThroughputSource != throughputSourceWorkerReported {
		t.Errorf("throughput source = %q, want %q; the rate is the worker's own report",
			candle.ThroughputSource, throughputSourceWorkerReported)
	}

	// llama_cpp_metal is the only profile with a measured throughput receipt,
	// and it is still not routable: measurement is necessary, not sufficient.
	if !byID["llama_cpp_metal"].Comparable {
		t.Error("llama_cpp_metal has a measured receipt but is not comparable")
	}
	for _, id := range []string{"mlx_metal", "vllm_cuda"} {
		if byID[id].Comparable {
			t.Errorf("%s has no measured receipt but is marked comparable", id)
		}
	}
	for _, id := range []string{"mlx_metal", "llama_cpp_metal", "vllm_cuda"} {
		if byID[id].Routable {
			t.Errorf("%s is marked routable", id)
		}
	}
}

// A non-routable profile is still estimated, with a stated reason. Dropping it
// here would hide exactly the comparison the runtime tournament is trying to
// make, and the shadow selector needs the rejection to check its own
// eligibility logic.
func TestEstimateReportsRejectionsRatherThanOmittingThem(t *testing.T) {
	cap := adapterTestWorker()
	estimates, err := EstimateAcrossRegisteredRuntimes(inferWorkload(), cap)
	must(t, err)
	for _, e := range estimates {
		if e.RuntimeProfileID == "candle_metal" {
			continue
		}
		if e.Supported {
			t.Errorf("%s supported a workload it may not take: %+v", e.RuntimeProfileID, e)
		}
		if e.Reason == "" {
			t.Errorf("%s was rejected without a stated reason", e.RuntimeProfileID)
		}
	}

	// Order is stable so a recorded shadow decision does not churn.
	for i := 1; i < len(estimates); i++ {
		if estimates[i-1].RuntimeProfileID > estimates[i].RuntimeProfileID {
			t.Fatalf("estimates are not ordered by runtime id: %v", estimates)
		}
	}
}

// The frozen placement floor from the workload decision is immutable authority.
// A cell whose own floor is lower must not be able to admit a worker the frozen
// decision already excluded.
func TestEstimateHonoursTheFrozenPlacementFloorOverTheCellFloor(t *testing.T) {
	candle, _ := runtimeProfileByID("candle_metal")
	adapter, err := AdapterForProfile(candle)
	must(t, err)
	cap := adapterTestWorker()
	cap.MemoryGB = 6 // above the 4 GB cell floor

	workload := inferWorkload()
	if e := adapter.Estimate(workload, candle, cap); !e.Supported {
		t.Fatalf("a 6 GB worker was refused a 4 GB cell: %s", e.Reason)
	}

	workload.MinimumMemoryGB = 32 // the frozen decision demanded more
	e := adapter.Estimate(workload, candle, cap)
	if e.Supported {
		t.Fatal("the cell floor overrode the frozen placement floor")
	}
	if !strings.Contains(e.Reason, "32.000 GB floor") {
		t.Errorf("refusal cited %q, want the frozen 32 GB floor", e.Reason)
	}
}

// A workload no cell serves is unsupported with a specific reason, not a
// generic failure that would read as a worker problem.
func TestEstimateNamesAnUnservedWorkloadPrecisely(t *testing.T) {
	candle, _ := runtimeProfileByID("candle_metal")
	adapter, _ := AdapterForProfile(candle)
	e := adapter.Estimate(
		RuntimeWorkload{JobType: "batch_infer", ModelRef: "some-unknown-model"},
		candle, adapterTestWorker())
	if e.Supported {
		t.Fatal("an unserved model was supported")
	}
	if !strings.Contains(e.Reason, "no cell for job") {
		t.Errorf("reason = %q, want it to name the missing cell", e.Reason)
	}
	if e.ThroughputSource != throughputSourceUnmeasured || e.TokensPerSec != 0 {
		t.Errorf("an unsupported estimate carries a throughput: %+v", e)
	}
}

// Supports and Estimate must agree. Two code paths answering the same question
// is how a selector ends up routing to something admission then refuses.
func TestSupportsAgreesWithEstimate(t *testing.T) {
	cap := adapterTestWorker()
	for _, workload := range []RuntimeWorkload{
		inferWorkload(),
		{JobType: "embed", ModelRef: "all-minilm-l6-v2", MinimumMemoryGB: 2},
		{JobType: "batch_infer", ModelRef: "nonexistent"},
		{JobType: "batch_infer", ModelRef: "llama-3.2-1b-instruct-q4", MinimumMemoryGB: 4096},
	} {
		for _, profile := range runtimeAuthority.Runtimes {
			adapter, err := AdapterForProfile(profile)
			must(t, err)
			supports := adapter.Supports(workload, profile, cap)
			estimate := adapter.Estimate(workload, profile, cap)
			if supports != estimate.Supported {
				t.Errorf("%s/%s: Supports=%v but Estimate.Supported=%v",
					profile.RuntimeID, workload.ModelRef, supports, estimate.Supported)
			}
		}
	}
}
