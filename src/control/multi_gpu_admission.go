package main

import (
	"errors"
	"fmt"
)

// Single-host multi-GPU admission.
//
// Serving a model too large for one GPU means splitting its weights across
// several on the same host -- tensor parallelism. What makes this a control
// plane problem rather than a runtime one is that the decision to admit a job
// is made BEFORE anything loads, and getting it wrong is not a rejection but an
// out-of-memory kill partway through a run the buyer is already being charged
// for.
//
// Three things are easy to get wrong, and each has a distinct failure:
//
//  1. Dividing total weight memory by GPU count and stopping there. Tensor
//     parallelism splits the WEIGHTS; it does not split the KV cache, the
//     activations, or the CUDA context, and every rank carries a full copy of
//     some of that. A host admitted on weights-only arithmetic OOMs the moment
//     the first long request arrives -- which is to say, in production and not
//     in testing.
//
//  2. Ignoring interconnect. Tensor parallelism exchanges activations at every
//     layer. Over NVLink that is cheap; over PCIe it can cost more than the
//     compute it enables, so a job that "fits" is slower than the single-GPU
//     path it was split to beat and still bills for the whole host.
//
//  3. Allowing a degree that does not divide the model's attention heads. The
//     runtime cannot split 40 heads across 3 ranks, and it discovers this at
//     load time -- after the contract exists.
//
// Refusing here costs a buyer one 503. Admitting wrongly costs a killed run, a
// disputed charge and a supplier-hour nobody can bill.

// gpuInterconnect describes how the GPUs on one host talk to each other.
type gpuInterconnect string

const (
	interconnectNVLink  gpuInterconnect = "nvlink"
	interconnectPCIe    gpuInterconnect = "pcie"
	interconnectUnknown gpuInterconnect = ""
)

// maxPCIeTensorParallel bounds tensor parallelism over PCIe.
//
// Two ranks over PCIe is usually worth it for a model that genuinely does not
// fit on one GPU. Beyond that the all-reduce traffic at every layer grows
// faster than the compute it buys, and merc would be selling a slower service
// for more money. NVLink hosts are not bounded here.
const maxPCIeTensorParallel = 2

// hostTopology is what a worker declares about one physical machine.
type hostTopology struct {
	HWClass string
	// GPUCount on this host. Multi-GPU is single-host by definition: merc does
	// not split a model across machines, because a network hop at every layer
	// is not tensor parallelism, it is a distributed-systems problem the buyer
	// did not ask for.
	GPUCount       int
	MemoryGBPerGPU float64
	Interconnect   gpuInterconnect
	// MemoryGBInUse is already committed to other work on this host.
	MemoryGBInUse float64
}

// modelPlacement is what a model needs.
type modelPlacement struct {
	ModelID string
	// WeightsGB is the full model, before any split.
	WeightsGB float64
	// PerRankOverheadGB is what each rank needs on top of its weight shard: KV
	// cache, activations, the CUDA context and the allocator's headroom. This
	// does NOT shrink with the degree, which is the arithmetic mistake this
	// type exists to make impossible to write.
	PerRankOverheadGB float64
	// AttentionHeads must be divisible by the tensor-parallel degree.
	AttentionHeads int
}

var (
	errNoMultiGPUCapacity  = errors.New("no admissible tensor-parallel placement on this host")
	errTopologyUndeclared  = errors.New("host topology is incomplete")
	errPlacementUndeclared = errors.New("model placement requirements are incomplete")
)

type tensorParallelPlan struct {
	Degree          int
	PerRankMemoryGB float64
	Interconnect    gpuInterconnect
	Rationale       string
}

func validateHostTopology(h hostTopology) error {
	if !validHWClasses[h.HWClass] {
		return fmt.Errorf("%w: hardware class %q is not admitted", errTopologyUndeclared, h.HWClass)
	}
	if h.GPUCount < 1 {
		return fmt.Errorf("%w: host declares %d GPUs", errTopologyUndeclared, h.GPUCount)
	}
	if h.MemoryGBPerGPU <= 0 {
		return fmt.Errorf("%w: host declares %v GB per GPU", errTopologyUndeclared, h.MemoryGBPerGPU)
	}
	if h.MemoryGBInUse < 0 || h.MemoryGBInUse > h.MemoryGBPerGPU {
		return fmt.Errorf("%w: %v GB in use against %v GB per GPU",
			errTopologyUndeclared, h.MemoryGBInUse, h.MemoryGBPerGPU)
	}
	if h.GPUCount > 1 && h.Interconnect == interconnectUnknown {
		// Not defaulted to PCIe: a host that has not said how its GPUs are
		// connected has not been characterised, and guessing the pessimistic
		// case would silently admit work at a performance merc cannot promise.
		return fmt.Errorf("%w: a multi-GPU host must declare its interconnect",
			errTopologyUndeclared)
	}
	return nil
}

func validateModelPlacement(m modelPlacement) error {
	if m.WeightsGB <= 0 {
		return fmt.Errorf("%w: model %q declares %v GB of weights",
			errPlacementUndeclared, m.ModelID, m.WeightsGB)
	}
	if m.PerRankOverheadGB < 0 {
		return fmt.Errorf("%w: per-rank overhead cannot be negative", errPlacementUndeclared)
	}
	if m.AttentionHeads < 1 {
		return fmt.Errorf("%w: model %q declares %d attention heads",
			errPlacementUndeclared, m.ModelID, m.AttentionHeads)
	}
	return nil
}

// perRankMemoryGB is the memory ONE rank needs at a given degree.
//
// The weight shard divides; the overhead does not. Writing this as a named
// function rather than inline arithmetic is deliberate: the whole class of bug
// here is someone dividing the total by the degree and forgetting that the KV
// cache and CUDA context are per-rank.
func perRankMemoryGB(m modelPlacement, degree int) float64 {
	return m.WeightsGB/float64(degree) + m.PerRankOverheadGB
}

// planTensorParallel picks the smallest admissible degree, or refuses.
//
// Smallest, not largest: every extra rank adds an all-reduce at every layer and
// occupies a GPU that could serve another buyer. Splitting more than necessary
// makes merc slower and its capacity smaller at the same time.
func planTensorParallel(h hostTopology, m modelPlacement) (tensorParallelPlan, error) {
	if err := validateHostTopology(h); err != nil {
		return tensorParallelPlan{}, err
	}
	if err := validateModelPlacement(m); err != nil {
		return tensorParallelPlan{}, err
	}

	available := h.MemoryGBPerGPU - h.MemoryGBInUse
	maxDegree := h.GPUCount
	if h.GPUCount > 1 && h.Interconnect == interconnectPCIe && maxDegree > maxPCIeTensorParallel {
		maxDegree = maxPCIeTensorParallel
	}

	for degree := 1; degree <= maxDegree; degree++ {
		if m.AttentionHeads%degree != 0 {
			// The runtime cannot split heads unevenly, and it finds out at load
			// time -- after the contract exists and the buyer is committed.
			continue
		}
		need := perRankMemoryGB(m, degree)
		if need > available {
			continue
		}
		rationale := fmt.Sprintf(
			"%s needs %.2f GB per rank at TP=%d (%.2f GB weight shard + %.2f GB per-rank "+
				"overhead) against %.2f GB free per GPU",
			m.ModelID, need, degree, m.WeightsGB/float64(degree), m.PerRankOverheadGB, available)
		return tensorParallelPlan{
			Degree: degree, PerRankMemoryGB: need,
			Interconnect: h.Interconnect, Rationale: rationale,
		}, nil
	}

	// Say WHY, distinguishing the three refusals: a supplier told "no capacity"
	// cannot tell whether to add GPUs, upgrade the interconnect, or nothing.
	switch {
	case h.GPUCount > 1 && h.Interconnect == interconnectPCIe && h.GPUCount > maxPCIeTensorParallel:
		return tensorParallelPlan{}, fmt.Errorf(
			"%w: %s does not fit within TP=%d, and this host's %d GPUs are on PCIe where "+
				"merc caps tensor parallelism at %d (beyond that the per-layer all-reduce "+
				"costs more than the compute it enables)",
			errNoMultiGPUCapacity, m.ModelID, maxPCIeTensorParallel, h.GPUCount, maxPCIeTensorParallel)
	case allDegreesIndivisible(m.AttentionHeads, maxDegree):
		return tensorParallelPlan{}, fmt.Errorf(
			"%w: %s has %d attention heads, which no degree from 1 to %d divides evenly",
			errNoMultiGPUCapacity, m.ModelID, m.AttentionHeads, maxDegree)
	default:
		return tensorParallelPlan{}, fmt.Errorf(
			"%w: %s needs %.2f GB per rank even at TP=%d, and this host has %.2f GB free "+
				"per GPU; note the %.2f GB per-rank overhead does not shrink with the degree",
			errNoMultiGPUCapacity, m.ModelID, perRankMemoryGB(m, maxDegree), maxDegree,
			available, m.PerRankOverheadGB)
	}
}

func allDegreesIndivisible(heads, maxDegree int) bool {
	for d := 1; d <= maxDegree; d++ {
		if heads%d == 0 {
			return false
		}
	}
	return true
}

// hostTopologyFromRegistration reads what a worker declared.
//
// Absent fields mean one GPU. An agent built before these fields existed is a
// single-GPU host as far as admission is concerned, which is exactly what it
// was before -- defaulting the other way would silently reinterpret every
// existing worker as multi-GPU on the day the field shipped.
//
// A worker cannot talk its way into more capacity than its class allows: the
// declared per-GPU memory is capped at the class ceiling, so claiming 900 GB on
// an 80 GB class is clamped rather than believed.
func hostTopologyFromRegistration(reg WorkerCapability) (hostTopology, error) {
	count := reg.GPUCount
	if count == 0 {
		count = 1
	}
	perGPU := float64(reg.MemoryGBPerGPU)
	if perGPU == 0 {
		// A single-GPU host may report only its total, which is the same number.
		perGPU = float64(reg.MemoryGB)
	}
	if ceiling := hwClassMemoryCeilingGB(reg.HWClass); ceiling > 0 && perGPU > ceiling {
		perGPU = ceiling
	}
	topology := hostTopology{
		HWClass:        reg.HWClass,
		GPUCount:       count,
		MemoryGBPerGPU: perGPU,
		Interconnect:   gpuInterconnect(reg.Interconnect),
	}
	if topology.Interconnect != interconnectUnknown &&
		topology.Interconnect != interconnectNVLink &&
		topology.Interconnect != interconnectPCIe {
		return hostTopology{}, fmt.Errorf("%w: interconnect %q is not recognised",
			errTopologyUndeclared, reg.Interconnect)
	}
	if err := validateHostTopology(topology); err != nil {
		return hostTopology{}, err
	}
	return topology, nil
}

// hwClassMemoryCeilingGB is the most memory one GPU of a class can have. A
// worker's self-declaration is clamped to it: registration is the one place a
// supplier controls the numbers merc schedules on.
func hwClassMemoryCeilingGB(hwClass string) float64 {
	switch hwClass {
	case "nvidia_24gb":
		return 24
	case "nvidia_48gb":
		return 48
	case "nvidia_80gb":
		return 80
	case "nvidia_180gb":
		return 180
	}
	// Apple Silicon is unified memory with no per-GPU split; the existing
	// memory admission path governs it.
	return 0
}
