package main

import (
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
)

func TestTopologyDecisionBatchIndependentFanoutIsAccepted(t *testing.T) {
	// Direct DecideTopology path (does not require a full valid WorkloadDecision body).
	out, err := DecideTopology(topologyLaneBatch, TopologyRequest{
		WorkloadClass:              "batch_embedding",
		Parallelism:                WorkloadParallelism{Mode: "independent_task_fanout", TensorParallelDegree: 1},
		Fabric:                     FabricUnknown,
		CommunityCapacityAvailable: true,
	}, strings.Repeat("b", 64))
	must(t, err)
	if out.Status != topologyDecisionAccepted || out.PlacementMode != ModePool ||
		out.SchedulerShape != TopologyIndependentChunks || out.Degree != 1 {
		t.Fatalf("batch independent decision: %+v", out)
	}
	if len(out.ConstructionRefusals) == 0 {
		t.Fatal("accepted decision lost LOCAL_CLUSTER construction refusal")
	}
	digest, err := topologyDecisionDigest(out)
	must(t, err)
	if err := ValidateTopologyDecisionDigest(out, digest); err != nil {
		t.Fatalf("digest validate: %v", err)
	}
}

func TestTopologyDecisionWANTightIsRefusedAndRecorded(t *testing.T) {
	req := TopologyRequest{
		WorkloadClass:              "realtime_generation",
		Parallelism:                WorkloadParallelism{Mode: "tensor_parallel", TensorParallelDegree: 4},
		Fabric:                     FabricWAN,
		CommunityCapacityAvailable: true,
		CloudBackstopPermitted:     false,
	}
	out, err := DecideTopology(topologyLaneBatch, req, "")
	must(t, err)
	if out.Status != topologyDecisionRefused {
		t.Fatalf("WAN-tight must be REFUSED, got %+v", out)
	}
	if out.PlacementMode != "" || out.SchedulerShape != "" {
		t.Fatalf("refused decision admitted a placement: %+v", out)
	}
	joined := out.Reason + strings.Join(func() []string {
		parts := make([]string, 0, len(out.ConstructionRefusals)+len(out.Refused))
		for _, r := range out.ConstructionRefusals {
			parts = append(parts, r.Topology+":"+r.Reason)
		}
		for _, r := range out.Refused {
			parts = append(parts, string(r.Mode)+":"+r.Reason)
		}
		return parts
	}(), "\n")
	if !strings.Contains(joined, "WAN_TIGHT_MULTI_HOST") &&
		!strings.Contains(out.Reason, "WAN") &&
		!strings.Contains(out.Reason, "unmeasured") {
		// Planner reason mentions WAN collective; construction refusal names the class.
		t.Fatalf("refusal lost WAN-tight reason: %+v", out)
	}
	foundWAN := false
	for _, r := range out.ConstructionRefusals {
		if r.Topology == "WAN_TIGHT_MULTI_HOST" {
			foundWAN = true
		}
	}
	if !foundWAN {
		t.Fatalf("missing WAN_TIGHT_MULTI_HOST construction refusal: %+v", out.ConstructionRefusals)
	}

	// Failing-before: neutralise the accept-time tripwire and show a forged
	// ACCEPTED decision would slip past when the check is off.
	forged := out
	forged.Status = topologyDecisionAccepted
	forged.PlacementMode = ModeLocalCluster
	forged.SchedulerShape = TopologyLocalGang
	forged.Degree = 4
	forged.Parallelism = "tensor_parallel"
	forged.Reason = "forged admission of WAN-tight gang"
	// Structure would fail Validate because REFUSED→ACCEPTED needs proper shape;
	// we already set them. Clear construction-only path via enforce.
	prev := topologyDecisionEnforceWANTightRefusal
	topologyDecisionEnforceWANTightRefusal = false
	t.Cleanup(func() { topologyDecisionEnforceWANTightRefusal = prev })
	if err := enforceTopologyDecisionInvariants(forged, req); err != nil {
		t.Fatalf("neutralised WAN-tight check should not fire: %v", err)
	}
	topologyDecisionEnforceWANTightRefusal = true
	if err := enforceTopologyDecisionInvariants(forged, req); err == nil {
		t.Fatal("WAN-tight tripwire must refuse a forged ACCEPTED decision")
	}
}

func TestTopologyDecisionStaleOrAbsentFabricCannotAdmitTight(t *testing.T) {
	now := time.Date(2026, time.August, 9, 12, 0, 0, 0, time.UTC)
	req := TopologyRequest{
		WorkloadClass:              "realtime_generation",
		Parallelism:                WorkloadParallelism{Mode: "tensor_parallel", TensorParallelDegree: 2},
		CommunityCapacityAvailable: true,
		CandidateDeviceCount:       2,
	}

	// Absent measured admission status (production evaluation shape).
	absent := FabricTopologyEvaluation{
		EvaluationID:                uuid.New(),
		Status:                      "SYNTHETIC_COLLECTIVES_MEASURED_GANG_SCHEDULER_REQUIRED",
		EvaluatedAt:                 now.Add(-time.Minute),
		EvidenceFreshUntil:          now.Add(14 * time.Minute),
		RequiredDirectedLinks:       2,
		VerifiedDirectedLinks:       2,
		RequiredDirectedCollectives: 2,
		VerifiedDirectedCollectives: 2,
		LocalClusterAdmissible:      false,
		NonAdmissionReasons:         []string{"no gang scheduler"},
	}
	out, err := DecideTopologyFromFabricEvaluation(topologyLaneBatch, absent, req, "", now)
	must(t, err)
	if out.Status != topologyDecisionRefused || out.Fabric != FabricUnknown {
		t.Fatalf("absent fabric admission promoted tight plan: %+v", out)
	}
	if out.FabricEvaluationID != absent.EvaluationID.String() {
		t.Fatalf("decision lost evaluation identity: %+v", out)
	}
	if out.FabricEvidenceStatus != absent.Status {
		t.Fatalf("decision lost fabric evidence status: %+v", out)
	}

	// Stale evidence even with forged admission flags.
	stale := FabricTopologyEvaluation{
		EvaluationID:                uuid.New(),
		Status:                      fabricTopologyAdmittedStatus,
		EvidenceFreshUntil:          now.Add(-time.Second),
		RequiredDirectedLinks:       2,
		VerifiedDirectedLinks:       2,
		RequiredDirectedCollectives: 2,
		VerifiedDirectedCollectives: 2,
		LocalClusterAdmissible:      true,
	}
	out, err = DecideTopologyFromFabricEvaluation(topologyLaneBatch, stale, req, "", now)
	must(t, err)
	if out.Status != topologyDecisionRefused || out.Fabric != FabricUnknown {
		t.Fatalf("stale fabric evidence admitted tight topology: %+v", out)
	}
	if out.FabricEvidenceFreshUntil == "" {
		t.Fatal("stale decision must record the freshness bound it believed")
	}
}

func TestTopologyDecisionTamperFailsDigest(t *testing.T) {
	out, err := DecideTopology(topologyLaneBatch, TopologyRequest{
		WorkloadClass:              "batch_embedding",
		Parallelism:                WorkloadParallelism{Mode: "independent_task_fanout", TensorParallelDegree: 1},
		Fabric:                     FabricUnknown,
		CommunityCapacityAvailable: true,
	}, strings.Repeat("c", 64))
	must(t, err)
	digest, err := topologyDecisionDigest(out)
	must(t, err)

	// Failing-before: with digest check neutralised, a mutated body incorrectly
	// validates against the old digest.
	mutated := out
	mutated.Degree = out.Degree + 1
	prev := topologyDecisionDigestCheck
	topologyDecisionDigestCheck = false
	t.Cleanup(func() { topologyDecisionDigestCheck = prev })
	if err := ValidateTopologyDecisionDigest(mutated, digest); err != nil {
		// Structure may still fail if Degree change breaks ACCEPTED rules —
		// Degree+1 is still valid for independent. If structure fails, force a
		// reason mutation instead.
		mutated = out
		mutated.Reason = out.Reason + " tampered"
		if err := ValidateTopologyDecisionDigest(mutated, digest); err != nil {
			t.Fatalf("neutralised digest check should accept structure: %v", err)
		}
	}

	topologyDecisionDigestCheck = true
	mutated = out
	mutated.Reason = out.Reason + " tampered"
	if err := ValidateTopologyDecisionDigest(mutated, digest); err == nil {
		t.Fatal("mutated topology decision must fail its digest")
	}
}

func TestTopologyDecisionRealtimeCitesPlacementPlan(t *testing.T) {
	mode, err := realtimeReplicaExecutionDecision()
	must(t, err)
	// Build a minimal frozen plan the same way production does for single-GPU.
	plan := RealtimePlacementPlan{
		Version:                  realtimePlacementPlanVersion,
		RuntimeProfileID:         "test-profile",
		RuntimeProfileSHA256:     strings.Repeat("d", 64),
		HWClass:                  "nvidia_24gb",
		GPUCount:                 1,
		MemoryGBPerGPU:           24,
		MemoryGBInUse:            0,
		ConfiguredTensorParallel: 1,
		AdmittedTensorParallel:   1,
		AdmissionBasis:           realtimePlacementTopologyOnly,
		Rationale:                "single-GPU test plan",
		ExecutionMode:            string(mode.Mode),
		ExecutionModeReason:      mode.Explain(),
	}
	must(t, ValidateFrozenRealtimePlacementPlan(plan))
	digest, err := realtimePlacementPlanDigest(plan)
	must(t, err)
	out, err := buildRealtimeTopologyDecision(plan, digest)
	must(t, err)
	if out.Status != topologyDecisionAccepted || out.HostTopologyAuthority != "RealtimePlacementPlan" ||
		out.HostTopologyDigest != digest || out.PlacementMode != ModeReplicaService {
		t.Fatalf("realtime topology decision: %+v", out)
	}
	if out.HostTensorParallelDegree != 1 || out.Degree != 1 {
		t.Fatalf("host TP / buyer degree wrong: %+v", out)
	}
	// Tampered plan digest must fail.
	if _, err := buildRealtimeTopologyDecision(plan, strings.Repeat("0", 64)); err == nil {
		t.Fatal("mismatched placement digest must be refused")
	}
}

func TestTopologyDecisionServiceLeaseIsExplicitNotApplicable(t *testing.T) {
	out, err := buildServiceLeaseTopologyDecision()
	must(t, err)
	if out.Status != topologyDecisionNotApplicable || out.Lane != topologyLaneServiceLease {
		t.Fatalf("service lease topology: %+v", out)
	}
	if out.PlacementMode != "" || len(out.ConstructionRefusals) == 0 {
		t.Fatalf("service lease must record construction refusals without admitting placement: %+v", out)
	}
	digest, err := topologyDecisionDigest(out)
	must(t, err)
	if err := ValidateTopologyDecisionDigest(out, digest); err != nil {
		t.Fatalf("digest: %v", err)
	}
}

func TestTopologyDecisionExactReuseIsNotApplicable(t *testing.T) {
	out, err := buildExactReuseTopologyDecision(strings.Repeat("e", 64))
	must(t, err)
	if out.Status != topologyDecisionNotApplicable {
		t.Fatalf("exact reuse: %+v", out)
	}
}

func TestTopologyDecisionNeverNeitherAcceptedNorRefusal(t *testing.T) {
	// Every builder path must produce ACCEPTED, REFUSED, or NOT_APPLICABLE with
	// a non-empty reason — never an empty struct that could be mistaken for unset.
	cases := []TopologyDecision{}
	batch, err := DecideTopology(topologyLaneBatch, TopologyRequest{
		WorkloadClass: "batch_embedding",
		Parallelism:   WorkloadParallelism{Mode: "independent_task_fanout", TensorParallelDegree: 1},
		Fabric:        FabricUnknown, CommunityCapacityAvailable: true,
	}, "")
	must(t, err)
	cases = append(cases, batch)

	refused, err := DecideTopology(topologyLaneBatch, TopologyRequest{
		Parallelism: WorkloadParallelism{Mode: "tensor_parallel", TensorParallelDegree: 2},
		Fabric:      FabricWAN,
	}, "")
	must(t, err)
	cases = append(cases, refused)

	lease, err := buildServiceLeaseTopologyDecision()
	must(t, err)
	cases = append(cases, lease)

	reuse, err := buildExactReuseTopologyDecision("")
	must(t, err)
	cases = append(cases, reuse)

	for i, d := range cases {
		switch d.Status {
		case topologyDecisionAccepted, topologyDecisionRefused, topologyDecisionNotApplicable:
		default:
			t.Fatalf("case %d has neither accept nor refusal: %+v", i, d)
		}
		if strings.TrimSpace(d.Reason) == "" {
			t.Fatalf("case %d missing reason: %+v", i, d)
		}
		if err := ValidateTopologyDecisionSnapshot(d); err != nil {
			t.Fatalf("case %d invalid: %v", i, err)
		}
	}
}
