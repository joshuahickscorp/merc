package main

import (
	"errors"
	"strings"
	"testing"
)

// The refusal merc.md §14 turns on: tightly coupled inference is never spread
// across the public internet on the strength of it fitting in memory.
func TestTightlyCoupledWorkIsRefusedOnAWANFabric(t *testing.T) {
	decision, err := ChooseExecutionMode(PlacementRequest{
		WorkloadClass: "realtime_generation",
		Coupling:      CouplingTight, Degree: 7,
		Fabric:                 FabricWAN,
		CloudBackstopPermitted: true,
	})
	if err != nil {
		t.Fatalf("a cloud path exists, so this must place: %v", err)
	}
	if decision.Mode != ModeCloudBackstop {
		t.Fatalf("mode = %s, want CLOUD_BACKSTOP", decision.Mode)
	}
	var localRefusal string
	for _, r := range decision.Refused {
		if r.Mode == ModeLocalCluster {
			localRefusal = r.Reason
		}
	}
	if !strings.Contains(localRefusal, "collective would dominate") {
		t.Fatalf("LOCAL_CLUSTER refusal does not name the physics: %q", localRefusal)
	}
	if !strings.Contains(decision.Explain(), "refused POOL") {
		t.Fatalf("explanation omits the pool refusal: %s", decision.Explain())
	}
}

// An unmeasured fabric is refused, not assumed. This is the fail-closed half: a
// cluster nobody benchmarked is not a cluster.
func TestUnknownFabricCannotWinLocalCluster(t *testing.T) {
	_, err := ChooseExecutionMode(PlacementRequest{
		Coupling: CouplingTight, Degree: 4,
		Fabric:                 FabricUnknown,
		CloudBackstopPermitted: false,
	})
	var none errNoAdmissibleExecutionMode
	if !errors.As(err, &none) {
		t.Fatalf("want no admissible mode, got %v", err)
	}
	if !strings.Contains(err.Error(), "unmeasured topology is refused") {
		t.Fatalf("error should name the missing measurement: %v", err)
	}
}

// A measured low-latency site is the one place tightly coupled work belongs.
func TestMeasuredSiteFabricWinsLocalCluster(t *testing.T) {
	decision, err := ChooseExecutionMode(PlacementRequest{
		Coupling: CouplingTight, Degree: 7,
		Fabric: FabricLowLatencySite,
	})
	if err != nil {
		t.Fatal(err)
	}
	if decision.Mode != ModeLocalCluster {
		t.Fatalf("mode = %s, want LOCAL_CLUSTER", decision.Mode)
	}
	if len(decision.Refused) < 2 {
		t.Fatalf("a placement with no stated refusals is not a choice: %+v", decision)
	}
}

func TestIndependentWorkChoosesPoolServiceOrCloud(t *testing.T) {
	for _, tc := range []struct {
		name string
		req  PlacementRequest
		want ExecutionMode
	}{{
		name: "finite project on the community fleet",
		req: PlacementRequest{
			Coupling: CouplingIndependent, Fabric: FabricWAN,
			CommunityCapacityAvailable: true,
		},
		want: ModePool,
	}, {
		name: "warm service keeps a replica per worker",
		req: PlacementRequest{
			Coupling: CouplingIndependent, Fabric: FabricWAN,
			CommunityCapacityAvailable: true, LongRunningService: true,
		},
		want: ModeReplicaService,
	}, {
		name: "deadline at risk falls back to the provider",
		req: PlacementRequest{
			Coupling: CouplingIndependent, Fabric: FabricWAN,
			CommunityCapacityAvailable: true, DeadlineAtRisk: true,
			CloudBackstopPermitted: true,
		},
		want: ModeCloudBackstop,
	}, {
		name: "no community capacity at all falls back to the provider",
		req: PlacementRequest{
			Coupling: CouplingIndependent, Fabric: FabricWAN,
			CloudBackstopPermitted: true,
		},
		want: ModeCloudBackstop,
	}} {
		t.Run(tc.name, func(t *testing.T) {
			decision, err := ChooseExecutionMode(tc.req)
			if err != nil {
				t.Fatalf("place: %v", err)
			}
			if decision.Mode != tc.want {
				t.Fatalf("mode = %s, want %s (%s)", decision.Mode, tc.want, decision.Explain())
			}
			if decision.Reason == "" {
				t.Fatal("a placement with no reason cannot be reviewed")
			}
		})
	}
}

// Region or privacy terms that forbid a provider must produce a refusal, never a
// silent placement on a provider anyway.
func TestCloudIsNotUsedWhenTheBuyerForbidsIt(t *testing.T) {
	_, err := ChooseExecutionMode(PlacementRequest{
		Coupling: CouplingIndependent, Fabric: FabricWAN,
		CommunityCapacityAvailable: false, CloudBackstopPermitted: false,
	})
	if err == nil {
		t.Fatal("placed a workload with neither community capacity nor cloud permission")
	}
	if !strings.Contains(err.Error(), "do not permit a provider") {
		t.Fatalf("error should name the buyer's terms: %v", err)
	}
}

// Coupling is read off the frozen decision, and an unrecognised parallelism mode
// is treated as tight so it cannot be placed on the public internet by default.
func TestCouplingFailsClosedForUnknownParallelism(t *testing.T) {
	if got := CouplingForParallelism(WorkloadParallelism{
		Mode: "independent_task_fanout", TensorParallelDegree: 1,
	}); got != CouplingIndependent {
		t.Fatalf("task fan-out = %s", got)
	}
	if got := CouplingForParallelism(WorkloadParallelism{
		Mode: "independent_task_fanout", TensorParallelDegree: 2,
	}); got != CouplingTight {
		t.Fatalf("degree 2 must be tight, got %s", got)
	}
	if got := CouplingForParallelism(WorkloadParallelism{
		Mode: "expert_parallel_moe", TensorParallelDegree: 1,
	}); got != CouplingTight {
		t.Fatalf("an unrecognised parallelism mode must fail closed, got %s", got)
	}
}
