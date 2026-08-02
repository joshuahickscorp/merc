package main

import (
	"strings"
	"testing"
)

func placementTestProfile() VLLMRuntimeProfile {
	profile := sortedVLLMProfiles()[0]
	profile.ProfileSHA256 = strings.Repeat("a", 64)
	return profile
}

func placementTestRegistration() RealtimeOfferRegistration {
	return RealtimeOfferRegistration{
		HWClass: "nvidia_24gb", GPUCount: 1, MemoryGBPerGPU: 24,
	}
}

func TestRealtimeSingleGPUPlacementFreezesDeclaredTopology(t *testing.T) {
	profile := placementTestProfile()
	plan, err := newRealtimePlacementPlan(profile, placementTestRegistration())
	if err != nil {
		t.Fatal(err)
	}
	if plan.AdmissionBasis != realtimePlacementTopologyOnly ||
		plan.AdmittedTensorParallel != 1 || plan.HWClass != "nvidia_24gb" ||
		plan.GPUCount != 1 || plan.MemoryGBPerGPU != 24 ||
		plan.ExecutionMode != string(ModeReplicaService) ||
		!strings.Contains(plan.ExecutionModeReason, "complete per-worker replicas") {
		t.Fatalf("unexpected single-GPU plan: %+v", plan)
	}
	blob, digest, err := encodeRealtimePlacementPlan(plan)
	if err != nil {
		t.Fatal(err)
	}
	decoded, err := decodeRealtimePlacementPlan(blob, digest)
	if err != nil {
		t.Fatal(err)
	}
	if decoded != plan {
		t.Fatalf("placement round trip changed authority: got=%+v want=%+v", decoded, plan)
	}
}

func TestRealtimeMultiGPUPlacementActivatesAdmissionPlanner(t *testing.T) {
	profile := placementTestProfile()
	profile.TensorParallelSize = 2
	profile.ModelWeightsGB = 60
	profile.PerRankOverheadGB = 8
	profile.AttentionHeads = 32
	reg := placementTestRegistration()
	reg.HWClass = "nvidia_48gb"
	reg.GPUCount = 2
	reg.MemoryGBPerGPU = 48
	reg.Interconnect = "nvlink"

	plan, err := newRealtimePlacementPlan(profile, reg)
	if err != nil {
		t.Fatal(err)
	}
	if plan.AdmissionBasis != realtimePlacementMemoryFit ||
		plan.AdmittedTensorParallel != 2 || plan.PerRankMemoryGB != 38 {
		t.Fatalf("multi-GPU planner was not authoritative: %+v", plan)
	}
}

func TestRealtimePlacementRefusesUnknownOrUnprovenMultiGPUShapes(t *testing.T) {
	profile := placementTestProfile()
	reg := placementTestRegistration()
	reg.GPUCount = 2
	if _, err := newRealtimePlacementPlan(profile, reg); err == nil {
		t.Fatal("multi-GPU host with undeclared interconnect was admitted")
	}

	reg = placementTestRegistration()
	reg.HWClass = "apple_silicon_max"
	if _, err := newRealtimePlacementPlan(profile, reg); err == nil {
		t.Fatal("non-CUDA hardware was admitted to vLLM")
	}

	profile.TensorParallelSize = 2
	reg = placementTestRegistration()
	reg.HWClass = "nvidia_48gb"
	reg.GPUCount = 2
	reg.MemoryGBPerGPU = 48
	reg.Interconnect = "nvlink"
	if _, err := newRealtimePlacementPlan(profile, reg); err == nil {
		t.Fatal("TP>1 profile without model placement requirements was admitted")
	}
}

func TestRealtimePlacementRefusesConfiguredDegreeThatIsNotFrozenMinimum(t *testing.T) {
	profile := placementTestProfile()
	profile.TensorParallelSize = 4
	profile.ModelWeightsGB = 60
	profile.PerRankOverheadGB = 8
	profile.AttentionHeads = 32
	reg := placementTestRegistration()
	reg.HWClass = "nvidia_48gb"
	reg.GPUCount = 4
	reg.MemoryGBPerGPU = 48
	reg.Interconnect = "nvlink"

	if _, err := newRealtimePlacementPlan(profile, reg); err == nil {
		t.Fatal("profile fixed TP=4 even though the frozen planner selected TP=2")
	}
}

func TestFrozenRealtimePlacementValidatorRefusesDegreeMutation(t *testing.T) {
	profile := placementTestProfile()
	profile.TensorParallelSize = 2
	profile.ModelWeightsGB = 60
	profile.PerRankOverheadGB = 8
	profile.AttentionHeads = 32
	reg := placementTestRegistration()
	reg.HWClass = "nvidia_48gb"
	reg.GPUCount = 2
	reg.MemoryGBPerGPU = 48
	reg.Interconnect = "nvlink"
	plan, err := newRealtimePlacementPlan(profile, reg)
	if err != nil {
		t.Fatal(err)
	}

	plan.GPUCount = 4
	plan.ConfiguredTensorParallel = 4
	plan.AdmittedTensorParallel = 4
	if err := ValidateFrozenRealtimePlacementPlan(plan); err == nil {
		t.Fatal("frozen placement validator accepted a degree different from the reproducible minimum")
	}
}

func TestFrozenRealtimePlacementValidatorRefusesExecutionModeMutation(t *testing.T) {
	plan, err := newRealtimePlacementPlan(placementTestProfile(), placementTestRegistration())
	if err != nil {
		t.Fatal(err)
	}
	plan.ExecutionMode = string(ModePool)
	if err := ValidateFrozenRealtimePlacementPlan(plan); err == nil ||
		!strings.Contains(err.Error(), "execution mode") {
		t.Fatalf("mutated replica placement mode was accepted: %v", err)
	}
}

func TestRealtimePlacementDigestDetectsTampering(t *testing.T) {
	profile := placementTestProfile()
	plan, err := newRealtimePlacementPlan(profile, placementTestRegistration())
	if err != nil {
		t.Fatal(err)
	}
	blob, digest, err := encodeRealtimePlacementPlan(plan)
	if err != nil {
		t.Fatal(err)
	}
	blob = []byte(strings.Replace(string(blob),
		`"rationale":"single-GPU profile bound to declared CUDA topology; model memory fit remains unproven"`,
		`"rationale":"tampered but structurally valid rationale"`, 1))
	if _, err := decodeRealtimePlacementPlan(blob, digest); err == nil {
		t.Fatal("tampered placement JSON retained authority")
	}
}

func TestVLLMProfileValidationRequiresPlacementAuthorityForTPGreaterThanOne(t *testing.T) {
	profile := placementTestProfile()
	profile.TensorParallelSize = 2
	if err := validateVLLMRuntimeProfile(profile); err == nil {
		t.Fatal("multi-GPU profile without model placement requirements passed validation")
	}
	profile.ModelWeightsGB = 60
	profile.PerRankOverheadGB = 8
	profile.AttentionHeads = 32
	if err := validateVLLMRuntimeProfile(profile); err != nil {
		t.Fatalf("complete multi-GPU placement profile was rejected: %v", err)
	}
}
