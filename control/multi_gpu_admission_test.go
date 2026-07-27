package main

import (
	"errors"
	"math/rand"
	"strings"
	"testing"
)

func nvlinkHost(gpus int, memPerGPU float64) hostTopology {
	return hostTopology{
		HWClass: "nvidia_80gb", GPUCount: gpus,
		MemoryGBPerGPU: memPerGPU, Interconnect: interconnectNVLink,
	}
}

// The arithmetic mistake this whole file exists to prevent: tensor parallelism
// splits the WEIGHTS, not the KV cache, activations or CUDA context. A host
// admitted on weights-only division OOMs on the first long request -- in
// production, not in testing.
func TestPerRankOverheadDoesNotShrinkWithDegree(t *testing.T) {
	model := modelPlacement{
		ModelID: "big-1", WeightsGB: 140, PerRankOverheadGB: 18, AttentionHeads: 64,
	}
	// Weights-only arithmetic says 140/4 = 35 GB, which fits a 40 GB GPU.
	// Reality is 35 + 18 = 53 GB, which does not.
	if got := perRankMemoryGB(model, 4); got <= model.WeightsGB/4 {
		t.Fatalf("per-rank memory %v ignored the per-rank overhead", got)
	}
	host := nvlinkHost(4, 40)
	_, err := planTensorParallel(host, model)
	if !errors.Is(err, errNoMultiGPUCapacity) {
		t.Fatalf("a placement that fits only on weights-only arithmetic was admitted: %v", err)
	}
	if !strings.Contains(err.Error(), "does not shrink") {
		t.Fatalf("refusal did not explain the overhead: %v", err)
	}

	// The same model on GPUs large enough for shard + overhead is admitted.
	if plan, err := planTensorParallel(nvlinkHost(4, 64), model); err != nil {
		t.Fatalf("a genuinely fitting placement was refused: %v", err)
	} else if plan.Degree != 4 {
		t.Fatalf("degree %d, want 4", plan.Degree)
	}
}

// Smallest admissible degree, not largest: each extra rank adds an all-reduce
// at every layer and occupies a GPU another buyer could have used.
func TestPlanPicksTheSmallestAdmissibleDegree(t *testing.T) {
	model := modelPlacement{
		ModelID: "mid-1", WeightsGB: 40, PerRankOverheadGB: 8, AttentionHeads: 32,
	}
	// 40 + 8 = 48 fits an 80 GB GPU on its own.
	plan, err := planTensorParallel(nvlinkHost(8, 80), model)
	if err != nil {
		t.Fatalf("plan: %v", err)
	}
	if plan.Degree != 1 {
		t.Fatalf("degree %d on a host where the model fits one GPU; splitting it "+
			"occupies %d GPUs for nothing", plan.Degree, plan.Degree)
	}

	// Halve the GPUs' memory and it must split, but only as far as it must.
	plan, err = planTensorParallel(nvlinkHost(8, 32), model)
	if err != nil {
		t.Fatalf("plan: %v", err)
	}
	// 40/2+8 = 28 <= 32, so TP=2 suffices.
	if plan.Degree != 2 {
		t.Fatalf("degree %d, want the smallest that fits (2)", plan.Degree)
	}
}

// The runtime cannot split attention heads unevenly, and it discovers that at
// load time -- after the contract exists and the buyer is committed.
func TestDegreeMustDivideAttentionHeads(t *testing.T) {
	model := modelPlacement{
		ModelID: "odd-heads", WeightsGB: 60, PerRankOverheadGB: 4, AttentionHeads: 3,
	}
	// TP=2 would fit on memory (30+4=34 <= 40) but 3 heads do not halve, so the
	// planner must skip it rather than admit a job that dies at load.
	plan, err := planTensorParallel(nvlinkHost(4, 40), model)
	if err == nil && plan.Degree == 2 {
		t.Fatal("admitted TP=2 for a model with 3 attention heads")
	}
	if err == nil && model.AttentionHeads%plan.Degree != 0 {
		t.Fatalf("admitted TP=%d for %d heads", plan.Degree, model.AttentionHeads)
	}

	// 3 heads do divide by 3, so a 3-GPU host serves it.
	if plan, err := planTensorParallel(nvlinkHost(3, 32), model); err != nil {
		t.Fatalf("TP=3 for 3 heads was refused: %v", err)
	} else if plan.Degree != 3 {
		t.Fatalf("degree %d, want 3", plan.Degree)
	}
}

// Tensor parallelism exchanges activations at every layer. Over PCIe that can
// cost more than the compute it enables, so merc would be selling a slower
// service for the price of a whole host.
func TestPCIeCapsTensorParallelism(t *testing.T) {
	model := modelPlacement{
		ModelID: "wide-1", WeightsGB: 200, PerRankOverheadGB: 10, AttentionHeads: 64,
	}
	pcie := hostTopology{
		HWClass: "nvidia_80gb", GPUCount: 8,
		MemoryGBPerGPU: 40, Interconnect: interconnectPCIe,
	}
	_, err := planTensorParallel(pcie, model)
	if !errors.Is(err, errNoMultiGPUCapacity) {
		t.Fatalf("PCIe host admitted a wide split: %v", err)
	}
	if !strings.Contains(err.Error(), "PCIe") {
		t.Fatalf("refusal did not name the interconnect: %v", err)
	}

	// The identical host on NVLink serves it: 200/8+10 = 35 <= 40.
	nvlink := pcie
	nvlink.Interconnect = interconnectNVLink
	plan, err := planTensorParallel(nvlink, model)
	if err != nil {
		t.Fatalf("NVLink host refused the same placement: %v", err)
	}
	if plan.Degree <= maxPCIeTensorParallel {
		t.Fatalf("degree %d does not exercise the NVLink allowance", plan.Degree)
	}
}

// A multi-GPU host that has not said how its GPUs are connected has not been
// characterised. Defaulting to PCIe would silently admit work at a performance
// merc cannot promise; defaulting to NVLink would admit work that thrashes.
func TestUndeclaredInterconnectIsRefusedNotGuessed(t *testing.T) {
	host := hostTopology{HWClass: "nvidia_80gb", GPUCount: 4, MemoryGBPerGPU: 80}
	model := modelPlacement{ModelID: "m", WeightsGB: 10, PerRankOverheadGB: 1, AttentionHeads: 8}
	_, err := planTensorParallel(host, model)
	if !errors.Is(err, errTopologyUndeclared) {
		t.Fatalf("an uncharacterised multi-GPU host was admitted: %v", err)
	}

	// A single-GPU host needs no interconnect: there is nothing to connect.
	single := hostTopology{HWClass: "nvidia_80gb", GPUCount: 1, MemoryGBPerGPU: 80}
	if _, err := planTensorParallel(single, model); err != nil {
		t.Fatalf("single-GPU host required an interconnect: %v", err)
	}
}

// Whatever the planner admits must actually fit. Examples cover the shapes
// someone thought of; this covers the ones nobody did.
func TestAdmittedPlansAlwaysFit(t *testing.T) {
	rng := rand.New(rand.NewSource(20260727))
	admitted, refused := 0, 0
	for i := 0; i < 50000; i++ {
		gpus := 1 + rng.Intn(8)
		host := hostTopology{
			HWClass:        "nvidia_80gb",
			GPUCount:       gpus,
			MemoryGBPerGPU: 1 + rng.Float64()*180,
			MemoryGBInUse:  0,
			Interconnect:   interconnectNVLink,
		}
		if rng.Intn(2) == 0 {
			host.Interconnect = interconnectPCIe
		}
		host.MemoryGBInUse = rng.Float64() * host.MemoryGBPerGPU
		model := modelPlacement{
			ModelID:           "fuzz",
			WeightsGB:         0.1 + rng.Float64()*400,
			PerRankOverheadGB: rng.Float64() * 40,
			AttentionHeads:    1 + rng.Intn(128),
		}

		plan, err := planTensorParallel(host, model)
		if err != nil {
			refused++
			continue
		}
		admitted++

		if plan.Degree < 1 || plan.Degree > host.GPUCount {
			t.Fatalf("iteration %d: degree %d on a %d-GPU host", i, plan.Degree, host.GPUCount)
		}
		if model.AttentionHeads%plan.Degree != 0 {
			t.Fatalf("iteration %d: degree %d does not divide %d heads",
				i, plan.Degree, model.AttentionHeads)
		}
		free := host.MemoryGBPerGPU - host.MemoryGBInUse
		if need := perRankMemoryGB(model, plan.Degree); need > free {
			t.Fatalf("iteration %d: admitted a plan needing %.3f GB per rank against "+
				"%.3f GB free -- this run would be OOM-killed mid-job", i, need, free)
		}
		if host.Interconnect == interconnectPCIe && plan.Degree > maxPCIeTensorParallel {
			t.Fatalf("iteration %d: PCIe host admitted TP=%d", i, plan.Degree)
		}
		if plan.Degree > 1 {
			// Smallest admissible: the degree below it must genuinely not work.
			for lower := 1; lower < plan.Degree; lower++ {
				if model.AttentionHeads%lower != 0 {
					continue
				}
				if perRankMemoryGB(model, lower) <= free {
					t.Fatalf("iteration %d: chose TP=%d when TP=%d also fits",
						i, plan.Degree, lower)
				}
			}
		}
	}
	// A planner that refused everything would pass every assertion above.
	if admitted == 0 || refused == 0 {
		t.Fatalf("degenerate fuzz: admitted %d refused %d", admitted, refused)
	}
	t.Logf("admitted %d, refused %d", admitted, refused)
}
