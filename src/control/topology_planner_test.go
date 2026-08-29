package main

import (
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
)

func TestPlanTopologyChoosesBoundedIndependentShapes(t *testing.T) {
	for _, tc := range []struct {
		name  string
		req   TopologyRequest
		shape TopologySchedulerShape
		mode  ExecutionMode
	}{
		{
			name: "finite chunks", req: TopologyRequest{
				WorkloadClass: "batch_embedding", Parallelism: WorkloadParallelism{Mode: "independent_task_fanout", TensorParallelDegree: 1},
				Fabric: FabricWAN, CommunityCapacityAvailable: true,
			}, shape: TopologyIndependentChunks, mode: ModePool,
		},
		{
			name: "render samples", req: TopologyRequest{
				WorkloadClass: "media_rendering", Parallelism: WorkloadParallelism{Mode: "render_samples", TensorParallelDegree: 1},
				Fabric: FabricWAN, CommunityCapacityAvailable: true,
			}, shape: TopologyFrameTileSample, mode: ModePool,
		},
		{
			name: "warm replicas", req: TopologyRequest{
				WorkloadClass: "realtime_generation", Parallelism: WorkloadParallelism{Mode: "replica_service", TensorParallelDegree: 1},
				Fabric: FabricWAN, CommunityCapacityAvailable: true, LongRunningService: true,
			}, shape: TopologyReplicaService, mode: ModeReplicaService,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			plan, err := PlanTopology(tc.req)
			must(t, err)
			if plan.Status != "ACCEPTED" || plan.SchedulerShape != tc.shape || plan.PlacementMode != tc.mode || plan.Reason == "" {
				t.Fatalf("plan=%+v", plan)
			}
			if len(plan.Evidence) == 0 {
				t.Fatal("accepted topology has no evidence")
			}
		})
	}
}

func TestPlanTopologyRequiresMeasuredFabricForLocalGang(t *testing.T) {
	unknown, err := PlanTopology(TopologyRequest{
		WorkloadClass:              "realtime_generation",
		Parallelism:                WorkloadParallelism{Mode: "tensor_parallel", TensorParallelDegree: 4},
		Fabric:                     FabricUnknown,
		CommunityCapacityAvailable: true,
	})
	must(t, err)
	if unknown.Status != "REFUSED" || unknown.SchedulerShape != "" || unknown.PlacementMode != "" {
		t.Fatalf("unmeasured fabric was admitted: %+v", unknown)
	}
	if !strings.Contains(unknown.Reason, "unmeasured topology is refused") || len(unknown.Refused) == 0 {
		t.Fatalf("refusal lost physical reason: %+v", unknown)
	}

	measured, err := PlanTopology(TopologyRequest{
		WorkloadClass:              "realtime_generation",
		Parallelism:                WorkloadParallelism{Mode: "tensor_parallel", TensorParallelDegree: 4},
		Fabric:                     FabricLowLatencySite,
		CommunityCapacityAvailable: true,
		CandidateDeviceCount:       4,
	})
	must(t, err)
	if measured.Status != "ACCEPTED" || measured.SchedulerShape != TopologyLocalGang || measured.PlacementMode != ModeLocalCluster || measured.Parallelism != "tensor_parallel" {
		t.Fatalf("measured fabric did not admit local gang: %+v", measured)
	}

	tooFew, err := PlanTopology(TopologyRequest{
		Parallelism: WorkloadParallelism{Mode: "tensor_parallel", TensorParallelDegree: 4},
		Fabric:      FabricLowLatencySite, CandidateDeviceCount: 3,
	})
	must(t, err)
	if tooFew.Status != "REFUSED" || !strings.Contains(tooFew.Reason, "exceeds") {
		t.Fatalf("planner shrank a gang to fit insufficient devices: %+v", tooFew)
	}
}

func TestPlanTopologyUsesProviderOnlyForAdmittedTightGang(t *testing.T) {
	plan, err := PlanTopology(TopologyRequest{
		WorkloadClass: "realtime_generation",
		Parallelism:   WorkloadParallelism{Mode: "pipeline_parallel", TensorParallelDegree: 2},
		Fabric:        FabricWAN, CloudBackstopPermitted: true,
	})
	must(t, err)
	if plan.Status != "ACCEPTED" || plan.PlacementMode != ModeCloudBackstop || plan.SchedulerShape != TopologyCloudBackstopGang {
		t.Fatalf("tight WAN plan did not use explicit provider gang: %+v", plan)
	}
	if !strings.Contains(plan.Reason, "faster and cheaper") {
		t.Fatalf("provider reason lost economics: %q", plan.Reason)
	}
}

func TestPlanTopologyRefusesUnknownShape(t *testing.T) {
	if _, err := PlanTopology(TopologyRequest{Parallelism: WorkloadParallelism{Mode: "mystery_parallelism", TensorParallelDegree: 2}}); err == nil {
		t.Fatal("unknown topology mode was silently accepted")
	}
}

func TestPlanTopologyFromCurrentFabricEvaluationNeverPromotesSyntheticEvidence(t *testing.T) {
	now := time.Date(2026, time.August, 2, 12, 0, 0, 0, time.UTC)
	evaluation := FabricTopologyEvaluation{
		EvaluationID:                uuid.New(),
		Status:                      "SYNTHETIC_COLLECTIVES_MEASURED_GANG_SCHEDULER_REQUIRED",
		EvaluatedAt:                 now.Add(-time.Minute),
		EvidenceFreshUntil:          now.Add(14 * time.Minute),
		RequiredDirectedLinks:       2,
		VerifiedDirectedLinks:       2,
		RequiredDirectedCollectives: 2,
		VerifiedDirectedCollectives: 2,
		LocalClusterAdmissible:      false,
		NonAdmissionReasons:         []string{"no gang scheduler or workload admission path consumes the measured topology"},
	}
	plan, err := PlanTopologyFromFabricEvaluation(evaluation, TopologyRequest{
		WorkloadClass:              "realtime_generation",
		Parallelism:                WorkloadParallelism{Mode: "tensor_parallel", TensorParallelDegree: 2},
		CommunityCapacityAvailable: true,
	}, now)
	must(t, err)
	if plan.Status != "REFUSED" || plan.Fabric != FabricUnknown || plan.PlacementMode != "" || plan.SchedulerShape != "" {
		t.Fatalf("synthetic topology evidence promoted a tight plan: %+v", plan)
	}
	joined := strings.Join(plan.Evidence, "\n")
	if !strings.Contains(joined, evaluation.EvaluationID.String()) ||
		!strings.Contains(joined, "fabric_topology_refusal=") ||
		!strings.Contains(plan.Reason, "remains non-admissible") {
		t.Fatalf("plan lost receipt-bound refusal evidence: %+v", plan)
	}
}

func TestPlanTopologyFromFabricEvaluationRequiresAnExplicitFutureAdmission(t *testing.T) {
	now := time.Date(2026, time.August, 2, 12, 0, 0, 0, time.UTC)
	base := FabricTopologyEvaluation{
		EvaluationID:                uuid.New(),
		Status:                      fabricTopologyAdmittedStatus,
		EvaluatedAt:                 now.Add(-time.Minute),
		EvidenceFreshUntil:          now.Add(time.Minute),
		RequiredDirectedLinks:       2,
		VerifiedDirectedLinks:       2,
		RequiredDirectedCollectives: 2,
		VerifiedDirectedCollectives: 2,
		LocalClusterAdmissible:      true,
	}
	plan, err := PlanTopologyFromFabricEvaluation(base, TopologyRequest{
		WorkloadClass:              "realtime_generation",
		Parallelism:                WorkloadParallelism{Mode: "tensor_parallel", TensorParallelDegree: 2},
		CandidateDeviceCount:       2,
		CommunityCapacityAvailable: true,
	}, now)
	must(t, err)
	if plan.Status != "ACCEPTED" || plan.Fabric != FabricLowLatencySite || plan.PlacementMode != ModeLocalCluster || plan.SchedulerShape != TopologyLocalGang {
		t.Fatalf("explicitly admitted fabric did not produce the bounded local gang plan: %+v", plan)
	}
	if !strings.Contains(strings.Join(plan.Evidence, "\n"), "fabric_topology_class=LOW_LATENCY_SITE") {
		t.Fatalf("accepted plan did not retain fabric class evidence: %+v", plan)
	}
}

func TestPlanTopologyFromFabricEvaluationRefusesStaleOrForgedAdmission(t *testing.T) {
	now := time.Date(2026, time.August, 2, 12, 0, 0, 0, time.UTC)
	for _, tc := range []struct {
		name   string
		mutate func(*FabricTopologyEvaluation)
	}{
		{name: "stale", mutate: func(e *FabricTopologyEvaluation) { e.EvidenceFreshUntil = now.Add(-time.Second) }},
		{name: "non-admission reason", mutate: func(e *FabricTopologyEvaluation) { e.NonAdmissionReasons = []string{"pricing authority missing"} }},
		{name: "short mesh", mutate: func(e *FabricTopologyEvaluation) { e.VerifiedDirectedLinks = 1 }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			evaluation := FabricTopologyEvaluation{
				EvaluationID:                uuid.New(),
				Status:                      fabricTopologyAdmittedStatus,
				EvidenceFreshUntil:          now.Add(time.Minute),
				RequiredDirectedLinks:       2,
				VerifiedDirectedLinks:       2,
				RequiredDirectedCollectives: 2,
				VerifiedDirectedCollectives: 2,
				LocalClusterAdmissible:      true,
			}
			tc.mutate(&evaluation)
			plan, err := PlanTopologyFromFabricEvaluation(evaluation, TopologyRequest{
				WorkloadClass:              "realtime_generation",
				Parallelism:                WorkloadParallelism{Mode: "tensor_parallel", TensorParallelDegree: 2},
				CommunityCapacityAvailable: true,
			}, now)
			must(t, err)
			if plan.Status != "REFUSED" || plan.Fabric != FabricUnknown || plan.PlacementMode != "" {
				t.Fatalf("%s forged/stale evaluation was admitted: %+v", tc.name, plan)
			}
		})
	}
}
