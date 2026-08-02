package main

import (
	"strings"
	"testing"
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
			if err != nil {
				t.Fatal(err)
			}
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
	if err != nil {
		t.Fatal(err)
	}
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
	if err != nil {
		t.Fatal(err)
	}
	if measured.Status != "ACCEPTED" || measured.SchedulerShape != TopologyLocalGang || measured.PlacementMode != ModeLocalCluster || measured.Parallelism != "tensor_parallel" {
		t.Fatalf("measured fabric did not admit local gang: %+v", measured)
	}

	tooFew, err := PlanTopology(TopologyRequest{
		Parallelism: WorkloadParallelism{Mode: "tensor_parallel", TensorParallelDegree: 4},
		Fabric:      FabricLowLatencySite, CandidateDeviceCount: 3,
	})
	if err != nil {
		t.Fatal(err)
	}
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
	if err != nil {
		t.Fatal(err)
	}
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
