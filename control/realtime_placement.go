package main

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"math"
)

const realtimePlacementPlanVersion = 1

const (
	realtimePlacementTopologyOnly = "declared_topology"
	realtimePlacementMemoryFit    = "memory_and_topology"
)

// RealtimePlacementPlan is the immutable, control-plane-authored placement
// decision behind one vLLM offer. The offer may refresh capacity and warmth,
// but it cannot change this plan without registering again. Authorization
// copies the exact JSON and digest into the execution contract so later worker
// heartbeats cannot rewrite what hardware the buyer was promised.
type RealtimePlacementPlan struct {
	Version                  int     `json:"version"`
	RuntimeProfileID         string  `json:"runtime_profile_id"`
	RuntimeProfileSHA256     string  `json:"runtime_profile_sha256"`
	HWClass                  string  `json:"hw_class"`
	GPUCount                 int     `json:"gpu_count"`
	MemoryGBPerGPU           float64 `json:"memory_gb_per_gpu"`
	MemoryGBInUse            float64 `json:"memory_gb_in_use"`
	Interconnect             string  `json:"interconnect,omitempty"`
	ModelWeightsGB           float64 `json:"model_weights_gb,omitempty"`
	PerRankOverheadGB        float64 `json:"per_rank_overhead_gb,omitempty"`
	AttentionHeads           int     `json:"attention_heads,omitempty"`
	ConfiguredTensorParallel int     `json:"configured_tensor_parallel"`
	AdmittedTensorParallel   int     `json:"admitted_tensor_parallel"`
	PerRankMemoryGB          float64 `json:"per_rank_memory_gb,omitempty"`
	AdmissionBasis           string  `json:"admission_basis"`
	Rationale                string  `json:"rationale"`
	// ExecutionMode is the market shape of this realtime placement. Tensor
	// parallel ranks inside one host remain one complete replica as far as a
	// buyer request is concerned; it is never evidence of a LOCAL_CLUSTER.
	ExecutionMode       string `json:"execution_mode"`
	ExecutionModeReason string `json:"execution_mode_reason"`
}

// realtimeReplicaExecutionDecision freezes the same placement decision for
// every realtime offer. Each worker advertises one complete model replica and
// a request is routed to one of those replicas; requests do not communicate
// with one another, even when a single replica uses tensor parallelism inside
// its own host. This is a real REPLICA_SERVICE mode, not a service lease: it
// grants neither reserved warm capacity nor a residency charge.
func realtimeReplicaExecutionDecision() (PlacementDecision, error) {
	return ChooseExecutionMode(PlacementRequest{
		WorkloadClass:              "realtime_inference",
		Coupling:                   CouplingIndependent,
		Degree:                     1,
		Fabric:                     FabricWAN,
		LongRunningService:         true,
		CommunityCapacityAvailable: true,
		CloudBackstopPermitted:     false,
	})
}

func realtimePlacementPlanDigest(plan RealtimePlacementPlan) (string, error) {
	if plan.Version != realtimePlacementPlanVersion {
		return "", fmt.Errorf("unsupported realtime placement plan version %d", plan.Version)
	}
	blob, err := json.Marshal(plan)
	if err != nil {
		return "", fmt.Errorf("marshal realtime placement plan: %w", err)
	}
	sum := sha256.Sum256(blob)
	return hex.EncodeToString(sum[:]), nil
}

func newRealtimePlacementPlan(profile VLLMRuntimeProfile, reg RealtimeOfferRegistration) (RealtimePlacementPlan, error) {
	topology := hostTopology{
		HWClass:        reg.HWClass,
		GPUCount:       reg.GPUCount,
		MemoryGBPerGPU: reg.MemoryGBPerGPU,
		MemoryGBInUse:  reg.MemoryGBInUse,
		Interconnect:   gpuInterconnect(reg.Interconnect),
	}
	if topology.Interconnect != interconnectUnknown &&
		topology.Interconnect != interconnectNVLink &&
		topology.Interconnect != interconnectPCIe {
		return RealtimePlacementPlan{}, fmt.Errorf("%w: interconnect %q is not recognised",
			errTopologyUndeclared, reg.Interconnect)
	}
	if err := validateHostTopology(topology); err != nil {
		return RealtimePlacementPlan{}, err
	}
	if !EngineAdmissibleFor("vllm", topology.HWClass) {
		return RealtimePlacementPlan{}, fmt.Errorf(
			"%w: vllm is not admitted on hardware class %q", errTopologyUndeclared, topology.HWClass)
	}
	if profile.TensorParallelSize > topology.GPUCount {
		return RealtimePlacementPlan{}, fmt.Errorf(
			"%w: runtime profile requires TP=%d but host declares %d GPUs",
			errNoMultiGPUCapacity, profile.TensorParallelSize, topology.GPUCount)
	}

	plan := RealtimePlacementPlan{
		Version:                  realtimePlacementPlanVersion,
		RuntimeProfileID:         profile.RuntimeProfileID,
		RuntimeProfileSHA256:     profile.ProfileSHA256,
		HWClass:                  topology.HWClass,
		GPUCount:                 topology.GPUCount,
		MemoryGBPerGPU:           topology.MemoryGBPerGPU,
		MemoryGBInUse:            topology.MemoryGBInUse,
		Interconnect:             string(topology.Interconnect),
		ModelWeightsGB:           profile.ModelWeightsGB,
		PerRankOverheadGB:        profile.PerRankOverheadGB,
		AttentionHeads:           profile.AttentionHeads,
		ConfiguredTensorParallel: profile.TensorParallelSize,
		AdmittedTensorParallel:   profile.TensorParallelSize,
	}
	mode, err := realtimeReplicaExecutionDecision()
	if err != nil {
		return RealtimePlacementPlan{}, fmt.Errorf("realtime replica execution mode: %w", err)
	}
	plan.ExecutionMode = string(mode.Mode)
	plan.ExecutionModeReason = mode.Explain()

	if profile.hasPlacementRequirements() {
		selected, err := planTensorParallel(topology, modelPlacement{
			ModelID:           profile.ModelAlias,
			WeightsGB:         profile.ModelWeightsGB,
			PerRankOverheadGB: profile.PerRankOverheadGB,
			AttentionHeads:    profile.AttentionHeads,
		})
		if err != nil {
			return RealtimePlacementPlan{}, err
		}
		if selected.Degree != profile.TensorParallelSize {
			return RealtimePlacementPlan{}, fmt.Errorf(
				"runtime profile fixes TP=%d but the smallest admissible frozen placement is TP=%d",
				profile.TensorParallelSize, selected.Degree)
		}
		plan.PerRankMemoryGB = selected.PerRankMemoryGB
		plan.AdmissionBasis = realtimePlacementMemoryFit
		plan.Rationale = selected.Rationale
	} else {
		if profile.TensorParallelSize != 1 {
			return RealtimePlacementPlan{}, errors.New(
				"multi-GPU runtime profile has no model placement requirements")
		}
		plan.AdmissionBasis = realtimePlacementTopologyOnly
		plan.Rationale = "single-GPU profile bound to declared CUDA topology; model memory fit remains unproven"
	}
	if err := ValidateFrozenRealtimePlacementPlan(plan); err != nil {
		return RealtimePlacementPlan{}, err
	}
	return plan, nil
}

// ValidateFrozenRealtimePlacementPlan uses only the plan snapshot. It never
// consults the live fleet or mutable profile catalogue.
func ValidateFrozenRealtimePlacementPlan(plan RealtimePlacementPlan) error {
	if plan.Version != realtimePlacementPlanVersion {
		return fmt.Errorf("unsupported realtime placement plan version %d", plan.Version)
	}
	if plan.RuntimeProfileID == "" || !validSHA256(plan.RuntimeProfileSHA256) {
		return errors.New("realtime placement plan has invalid runtime profile authority")
	}
	for name, value := range map[string]float64{
		"memory_gb_per_gpu":    plan.MemoryGBPerGPU,
		"memory_gb_in_use":     plan.MemoryGBInUse,
		"model_weights_gb":     plan.ModelWeightsGB,
		"per_rank_overhead_gb": plan.PerRankOverheadGB,
		"per_rank_memory_gb":   plan.PerRankMemoryGB,
	} {
		if math.IsNaN(value) || math.IsInf(value, 0) {
			return fmt.Errorf("realtime placement plan %s is not finite", name)
		}
	}
	topology := hostTopology{
		HWClass:        plan.HWClass,
		GPUCount:       plan.GPUCount,
		MemoryGBPerGPU: plan.MemoryGBPerGPU,
		MemoryGBInUse:  plan.MemoryGBInUse,
		Interconnect:   gpuInterconnect(plan.Interconnect),
	}
	if topology.Interconnect != interconnectUnknown &&
		topology.Interconnect != interconnectNVLink &&
		topology.Interconnect != interconnectPCIe {
		return errors.New("realtime placement plan has an unrecognised interconnect")
	}
	if err := validateHostTopology(topology); err != nil {
		return err
	}
	if !EngineAdmissibleFor("vllm", plan.HWClass) {
		return errors.New("realtime placement plan does not bind an admitted CUDA class")
	}
	if plan.ConfiguredTensorParallel < 1 ||
		plan.AdmittedTensorParallel != plan.ConfiguredTensorParallel ||
		plan.AdmittedTensorParallel > plan.GPUCount ||
		plan.Rationale == "" || plan.ExecutionMode == "" || plan.ExecutionModeReason == "" {
		return errors.New("realtime placement plan has invalid tensor-parallel authority")
	}
	expectedMode, err := realtimeReplicaExecutionDecision()
	if err != nil {
		return err
	}
	if plan.ExecutionMode != string(expectedMode.Mode) ||
		plan.ExecutionModeReason != expectedMode.Explain() {
		return errors.New("realtime placement plan has a non-reproducible execution mode")
	}
	switch plan.AdmissionBasis {
	case realtimePlacementTopologyOnly:
		if plan.ConfiguredTensorParallel != 1 || plan.ModelWeightsGB != 0 ||
			plan.PerRankOverheadGB != 0 || plan.AttentionHeads != 0 ||
			plan.PerRankMemoryGB != 0 {
			return errors.New("topology-only placement is allowed only for a single-GPU profile")
		}
	case realtimePlacementMemoryFit:
		selected, err := planTensorParallel(topology, modelPlacement{
			ModelID:           plan.RuntimeProfileID,
			WeightsGB:         plan.ModelWeightsGB,
			PerRankOverheadGB: plan.PerRankOverheadGB,
			AttentionHeads:    plan.AttentionHeads,
		})
		if err != nil {
			return err
		}
		if selected.Degree != plan.AdmittedTensorParallel ||
			math.Abs(selected.PerRankMemoryGB-plan.PerRankMemoryGB) > 0.000001 {
			return errors.New("frozen realtime placement no longer reproduces its admitted degree")
		}
	default:
		return errors.New("realtime placement plan has an unknown admission basis")
	}
	return nil
}

func encodeRealtimePlacementPlan(plan RealtimePlacementPlan) ([]byte, string, error) {
	if err := ValidateFrozenRealtimePlacementPlan(plan); err != nil {
		return nil, "", err
	}
	blob, err := json.Marshal(plan)
	if err != nil {
		return nil, "", err
	}
	digest, err := realtimePlacementPlanDigest(plan)
	return blob, digest, err
}

func decodeRealtimePlacementPlan(blob []byte, digest string) (RealtimePlacementPlan, error) {
	if len(blob) == 0 || digest == "" {
		return RealtimePlacementPlan{}, errors.New("realtime placement plan is missing")
	}
	var plan RealtimePlacementPlan
	if err := decodeStrictJSONObject(blob, &plan); err != nil {
		return RealtimePlacementPlan{}, fmt.Errorf("decode realtime placement plan: %w", err)
	}
	if err := ValidateFrozenRealtimePlacementPlan(plan); err != nil {
		return RealtimePlacementPlan{}, err
	}
	actual, err := realtimePlacementPlanDigest(plan)
	if err != nil {
		return RealtimePlacementPlan{}, err
	}
	if actual != digest {
		return RealtimePlacementPlan{}, errors.New("realtime placement plan digest mismatch")
	}
	return plan, nil
}
