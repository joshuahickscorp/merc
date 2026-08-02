package main

import (
	"fmt"
	"strings"
)

const topologyPlanVersion = 1

// TopologySchedulerShape is the bounded scheduling shape chosen after
// parallelism is interpreted. It is intentionally separate from
// ExecutionMode: POOL/REPLICA_SERVICE/LOCAL_CLUSTER describe the supply lane,
// while these values describe how the workload's units are coordinated.
type TopologySchedulerShape string

const (
	TopologySingleDevice      TopologySchedulerShape = "SINGLE_DEVICE"
	TopologyIndependentChunks TopologySchedulerShape = "INDEPENDENT_CHUNK_SPLIT"
	TopologyReplicaService    TopologySchedulerShape = "REPLICA_SERVICE"
	TopologyFrameTileSample   TopologySchedulerShape = "FRAME_TILE_SAMPLE"
	TopologyLocalGang         TopologySchedulerShape = "LOCAL_GANG"
	TopologyCloudBackstopGang TopologySchedulerShape = "CLOUD_BACKSTOP_GANG"
)

// TopologyRequest contains only authority-side placement facts. In particular,
// Fabric is a measurement result, never a buyer-supplied request label.
type TopologyRequest struct {
	WorkloadClass              string
	Parallelism                WorkloadParallelism
	Fabric                     FabricClass
	LongRunningService         bool
	CommunityCapacityAvailable bool
	CloudBackstopPermitted     bool
	DeadlineAtRisk             bool
	// CandidateDeviceCount is optional because the current workload authority
	// does not yet expose a multi-device inventory. When present, the planner
	// refuses a degree that cannot fit instead of silently shrinking it.
	CandidateDeviceCount int
}

// TopologyPlan is the stable, receipt-friendly result of topology planning.
// A REFUSED plan is useful evidence: it preserves the requested shape and the
// exact physical/economic reason no placement was admitted.
type TopologyPlan struct {
	Version        int                    `json:"version"`
	Status         string                 `json:"status"`
	Parallelism    string                 `json:"parallelism"`
	Degree         int                    `json:"degree"`
	SchedulerShape TopologySchedulerShape `json:"scheduler_shape"`
	PlacementMode  ExecutionMode          `json:"placement_mode"`
	Fabric         FabricClass            `json:"fabric"`
	Reason         string                 `json:"reason"`
	Refused        []ModeRefusal          `json:"refused,omitempty"`
	Evidence       []string               `json:"evidence,omitempty"`
}

// Explain keeps the supply-lane refusals and the selected scheduler shape in a
// single stable sentence for shadow receipts and operator review.
func (p TopologyPlan) Explain() string {
	parts := []string{p.Reason}
	for _, refusal := range p.Refused {
		parts = append(parts, fmt.Sprintf("refused %s: %s", refusal.Mode, refusal.Reason))
	}
	if p.SchedulerShape != "" {
		parts = append(parts, fmt.Sprintf("topology=%s", p.SchedulerShape))
	}
	return strings.Join(parts, "; ")
}

func topologyParallelism(mode string, degree int) (string, int, error) {
	normalized := strings.ToLower(strings.TrimSpace(mode))
	if normalized == "" {
		normalized = "single_device"
	}
	if degree <= 0 {
		degree = 1
	}
	switch normalized {
	case "single_device", "single-device":
		if degree != 1 {
			return "", 0, fmt.Errorf("single-device topology requires degree 1, got %d", degree)
		}
		return "single_device", degree, nil
	case "independent_task_fanout", "independent_chunks", "independent_chunk_split":
		if degree != 1 {
			return "", 0, fmt.Errorf("independent task fan-out cannot carry a coupled degree %d", degree)
		}
		return "independent_task_fanout", degree, nil
	case "replica_service", "replica-service":
		if degree != 1 {
			return "", 0, fmt.Errorf("replica service requires one complete replica per worker, got degree %d", degree)
		}
		return "replica_service", degree, nil
	case "tensor_parallel", "tensor-parallel", "pipeline_parallel", "pipeline-parallel",
		"data_parallel", "data-parallel", "expert_parallel", "expert-parallel":
		if degree < 2 {
			return "", 0, fmt.Errorf("%s requires a coupled degree of at least 2", normalized)
		}
		return normalized, degree, nil
	case "render_frames", "render_tiles", "render_samples", "frame_tile_sample", "frame-tile-sample":
		return "frame_tile_sample", degree, nil
	default:
		return "", 0, fmt.Errorf("unsupported workload parallelism mode %q", mode)
	}
}

// PlanTopology chooses a bounded shape and then delegates the supply-lane
// decision to ChooseExecutionMode. It never turns an unmeasured fabric into a
// local cluster: a tight plan can be REFUSED even though its shape is known.
func PlanTopology(req TopologyRequest) (TopologyPlan, error) {
	parallelism, degree, err := topologyParallelism(req.Parallelism.Mode, req.Parallelism.TensorParallelDegree)
	if err != nil {
		return TopologyPlan{}, err
	}
	out := TopologyPlan{
		Version: topologyPlanVersion, Status: "REFUSED", Parallelism: parallelism,
		Degree: degree, Fabric: req.Fabric, Refused: []ModeRefusal{}, Evidence: []string{},
	}
	if req.CandidateDeviceCount > 0 && degree > req.CandidateDeviceCount {
		out.Reason = fmt.Sprintf("degree %d exceeds the %d authority-reported candidate devices", degree, req.CandidateDeviceCount)
		out.Evidence = append(out.Evidence, "candidate device count was authority-reported")
		return out, nil
	}

	tight := parallelism == "tensor_parallel" || parallelism == "pipeline_parallel" ||
		parallelism == "data_parallel" || parallelism == "expert_parallel"
	placement, placementErr := ChooseExecutionMode(PlacementRequest{
		WorkloadClass: req.WorkloadClass, Coupling: map[bool]ParallelismCoupling{true: CouplingTight, false: CouplingIndependent}[tight],
		Degree: degree, Fabric: req.Fabric, LongRunningService: req.LongRunningService || parallelism == "replica_service",
		CommunityCapacityAvailable: req.CommunityCapacityAvailable,
		CloudBackstopPermitted:     req.CloudBackstopPermitted, DeadlineAtRisk: req.DeadlineAtRisk,
	})
	if placementErr != nil {
		if refused, ok := placementErr.(errNoAdmissibleExecutionMode); ok {
			out.Refused = append(out.Refused, refused.refused...)
		}
		out.Reason = placementErr.Error()
		out.Evidence = append(out.Evidence, "placement failed closed because every admissible supply lane was refused")
		if tight {
			out.Evidence = append(out.Evidence, "tightly coupled ranks require a measured collective fabric or a permitted provider")
		}
		return out, nil
	}

	out.Status = "ACCEPTED"
	out.PlacementMode = placement.Mode
	out.Reason = placement.Reason
	out.Refused = append(out.Refused, placement.Refused...)
	switch {
	case tight && placement.Mode == ModeLocalCluster:
		out.SchedulerShape = TopologyLocalGang
		out.Evidence = append(out.Evidence, "measured low-latency site fabric admits a coordinated gang")
	case tight:
		out.SchedulerShape = TopologyCloudBackstopGang
		out.Evidence = append(out.Evidence, "provider lane is the only admitted tightly coupled placement")
	case parallelism == "replica_service":
		out.SchedulerShape = TopologyReplicaService
		out.Evidence = append(out.Evidence, "complete replicas serve independent requests without inter-worker collectives")
	case parallelism == "frame_tile_sample":
		out.SchedulerShape = TopologyFrameTileSample
		out.Evidence = append(out.Evidence, "frames, tiles, or samples are independently verifiable work units")
	case parallelism == "single_device":
		out.SchedulerShape = TopologySingleDevice
		out.Evidence = append(out.Evidence, "one authority-selected device is the bounded execution unit")
	default:
		out.SchedulerShape = TopologyIndependentChunks
		out.Evidence = append(out.Evidence, "independent chunks require no inter-worker communication")
	}
	return out, nil
}
