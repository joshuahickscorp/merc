package main

import (
	"fmt"
	"sort"
)

// The control-plane runtime adapter boundary.
//
// This interface deliberately does NOT contain launch, health, execute, cancel,
// drain or metrics. The control plane never runs a model; the agent does. Those
// methods belong to the agent-side Rust driver, and declaring them here would
// produce a Go interface whose implementations could only panic or lie.
//
// What the control plane genuinely does is decide: is this profile well formed,
// may this worker claim it, can it serve this workload, and what should we
// expect if it does. Those five questions are the boundary.
//
// Today every registered engine uses the same generic adapter, because nothing
// in the control plane is engine-specific — a grep for mlx/vllm/tensorrt across
// the scheduler, shape routing, multi-GPU admission and benchmark code finds
// only one comment. The registry exists so that stays true by construction: a
// second runtime with genuinely different admission rules registers an adapter
// instead of adding a branch to the scheduler.

// RuntimeWorkload is the part of an admitted workload a runtime decision needs.
// It is deliberately narrow: an adapter that could see the buyer, the price or
// the economic plan would eventually be asked to consider them.
type RuntimeWorkload struct {
	JobType  string
	ModelRef string
	// MinimumMemoryGB is the frozen placement floor from the workload decision.
	MinimumMemoryGB float64
}

// Throughput evidence sources. The distinction is the whole point of the type:
// a worker's self-reported benchmark and a profile's receipt-bound benchmark
// authority are not the same kind of claim, and a runtime tournament that
// treats them as interchangeable is rigged before it starts.
//
// Only two sources exist today, because the control plane derives no rate from
// a profile's benchmark authority — it reads the worker's own report and
// nothing else. A "profile_benchmark_authority" source belongs in the change
// that actually reads the receipt (a second profile's benchmark), not here
// where it would be a label with no measurement behind it.
const (
	throughputSourceWorkerReported = "worker_reported"
	throughputSourceUnmeasured     = "unmeasured"
)

// RuntimeEstimate is what the control plane expects if a workload runs on a
// profile at a worker. Comparable is separate from TokensPerSec on purpose:
// a number without comparable evidence behind it must not decide a tournament.
type RuntimeEstimate struct {
	RuntimeProfileID string  `json:"runtime_profile_id"`
	Supported        bool    `json:"supported"`
	Reason           string  `json:"reason,omitempty"`
	TokensPerSec     float64 `json:"tokens_per_sec"`
	ThroughputSource string  `json:"throughput_source"`
	QualityTier      string  `json:"quality_tier"`
	Routable         bool    `json:"routable"`
	// Comparable is true only when the profile names a benchmark authority. A
	// worker-reported rate on an unproven profile is real information about that
	// worker and is not evidence that the profile is faster.
	Comparable bool     `json:"comparable"`
	Evidence   []string `json:"evidence,omitempty"`
}

// RuntimeProfileAdapter is the control-plane half of a runtime integration.
type RuntimeProfileAdapter interface {
	ID() string
	ValidateProfile(profile authorityRuntimeProfile) error
	ValidateWorker(profile authorityRuntimeProfile, cap WorkerCapability) error
	Supports(workload RuntimeWorkload, profile authorityRuntimeProfile, cap WorkerCapability) bool
	Estimate(workload RuntimeWorkload, profile authorityRuntimeProfile, cap WorkerCapability) RuntimeEstimate
}

// genericProfileAdapter derives every answer from the profile document. It is
// correct for any runtime whose admission rules are fully expressed there, which
// is all of them today.
type genericProfileAdapter struct{ adapter string }

func (a genericProfileAdapter) ID() string { return a.adapter }

func (a genericProfileAdapter) ValidateProfile(profile authorityRuntimeProfile) error {
	if profile.Adapter != a.adapter {
		return fmt.Errorf("profile %q names adapter %q, not %q",
			profile.RuntimeID, profile.Adapter, a.adapter)
	}
	// The document-level validator already enforces the rest at load. Repeating
	// it here would create a second, drifting copy of the same rules.
	return nil
}

func (a genericProfileAdapter) ValidateWorker(
	profile authorityRuntimeProfile, cap WorkerCapability,
) error {
	if err := a.ValidateProfile(profile); err != nil {
		return err
	}
	return ValidateWorkerAgainstProfile(profile, cap)
}

// cellFor returns the profile cell serving a workload, if any.
func cellFor(profile authorityRuntimeProfile, workload RuntimeWorkload) (authorityCell, bool) {
	for _, cell := range profile.Cells {
		if cell.Job == workload.JobType && cell.Model == workload.ModelRef {
			return cell, true
		}
	}
	return authorityCell{}, false
}

func (a genericProfileAdapter) Supports(
	workload RuntimeWorkload, profile authorityRuntimeProfile, cap WorkerCapability,
) bool {
	return a.Estimate(workload, profile, cap).Supported
}

func (a genericProfileAdapter) Estimate(
	workload RuntimeWorkload, profile authorityRuntimeProfile, cap WorkerCapability,
) RuntimeEstimate {
	out := RuntimeEstimate{
		RuntimeProfileID: profile.RuntimeID,
		QualityTier:      profile.QualityTier,
		Routable:         runtimeLifecycleRoutable(profile.Lifecycle),
		ThroughputSource: throughputSourceUnmeasured,
		Comparable:       profile.BenchmarkAuthority != "",
		Evidence:         append([]string(nil), profile.Evidence...),
	}
	if profile.BenchmarkAuthority != "" {
		out.Evidence = append(out.Evidence, profile.BenchmarkAuthority)
	}
	sort.Strings(out.Evidence)

	cell, ok := cellFor(profile, workload)
	if !ok {
		out.Reason = fmt.Sprintf("profile %q has no cell for job %q model %q",
			profile.RuntimeID, workload.JobType, workload.ModelRef)
		return out
	}
	if err := a.ValidateWorker(profile, cap); err != nil {
		out.Reason = err.Error()
		return out
	}
	// The frozen placement floor wins over the cell floor when it is higher: the
	// workload decision was made against it and is immutable.
	floor := cell.MinMemoryGB
	if workload.MinimumMemoryGB > floor {
		floor = workload.MinimumMemoryGB
	}
	if float64(cap.MemoryGB) < floor {
		out.Reason = fmt.Sprintf("worker memory %.3f GB is below the %.3f GB floor for cell %q",
			cap.MemoryGB, floor, cell.ID)
		return out
	}
	out.Supported = true

	// Throughput. A worker's own benchmark is the only rate the control plane
	// has today, and it says nothing about the profile — so it is labelled as
	// what it is, and Comparable stays governed by the benchmark authority.
	for _, benchmark := range cap.Benchmarks {
		if benchmark.JobType != workload.JobType || benchmark.ModelID != workload.ModelRef {
			continue
		}
		rate := float64(benchmark.TPS)
		if workload.JobType == "embed" {
			rate = float64(benchmark.EPS)
		}
		if rate > 0 {
			// Always worker_reported: this number came from the agent, not from
			// the profile's benchmark receipt. Comparable already carries what the
			// profile proved; overwriting the source here would launder a
			// self-report into an authority.
			out.TokensPerSec = rate
			out.ThroughputSource = throughputSourceWorkerReported
		}
		break
	}
	return out
}

// runtimeAdapters is keyed by adapter name, matching the engine registry in the
// authority document. Registration is derived from that document rather than
// hand-maintained, so an engine cannot be added to one and forgotten in the
// other.
var runtimeAdapters = buildRuntimeAdapters()

func buildRuntimeAdapters() map[string]RuntimeProfileAdapter {
	out := make(map[string]RuntimeProfileAdapter, len(runtimeAuthority.Engines))
	for _, engine := range runtimeAuthority.Engines {
		out[engine.Adapter] = genericProfileAdapter{adapter: engine.Adapter}
	}
	return out
}

// AdapterForProfile fails closed. An unregistered adapter means the document and
// this registry disagree, and guessing which is right is how an ungoverned
// runtime gets admitted.
func AdapterForProfile(profile authorityRuntimeProfile) (RuntimeProfileAdapter, error) {
	adapter, ok := runtimeAdapters[profile.Adapter]
	if !ok {
		return nil, fmt.Errorf("runtime profile %q names unregistered adapter %q",
			profile.RuntimeID, profile.Adapter)
	}
	return adapter, nil
}

// EstimateAcrossRegisteredRuntimes answers "what would each registered profile
// do with this workload on this worker", including the profiles that may not
// take it. That shape is what the shadow selector needs: a rejection with a
// stated reason is the evidence that the selector's eligibility logic is right,
// and dropping non-routable profiles here would hide exactly the comparison the
// runtime tournament is trying to make.
//
// Results are ordered by runtime id so a recorded shadow decision is stable.
func EstimateAcrossRegisteredRuntimes(
	workload RuntimeWorkload, cap WorkerCapability,
) ([]RuntimeEstimate, error) {
	out := make([]RuntimeEstimate, 0, len(runtimeAuthority.Runtimes))
	for _, profile := range runtimeAuthority.Runtimes {
		adapter, err := AdapterForProfile(profile)
		if err != nil {
			return nil, err
		}
		out = append(out, adapter.Estimate(workload, profile, cap))
	}
	sort.Slice(out, func(i, j int) bool {
		return out[i].RuntimeProfileID < out[j].RuntimeProfileID
	})
	return out, nil
}
