package main

import "strings"

// deriveProjectIRTopologies adds only a shape decision to each step. It does
// not inspect supplier offers or claim that any shape can be admitted; that
// distinction lets the project compiler remain useful before a buyer has a
// quote while keeping tight work fail-closed.
func deriveProjectIRTopologies(ir *ProjectWorkloadIR) {
	if ir == nil {
		return
	}
	for i := range ir.Steps {
		ir.Steps[i].Topology = deriveProjectIRTopology(ir.Steps[i])
	}
}

func deriveProjectIRTopology(step ProjectIRStep) *ProjectIRTopology {
	topology := &ProjectIRTopology{
		Version:     topologyPlanVersion,
		Parallelism: strings.ToUpper(strings.TrimSpace(step.Parallelism)),
		Degree:      1,
		Status:      "UNRESOLVED_REFUSE",
		Reason:      "step parallelism is unresolved; no scheduler shape is admitted",
		Evidence:    []string{"compiler-derived from reviewed step kind and parallelism"},
	}
	if topology.Parallelism == "" || topology.Parallelism == "UNRESOLVED_REFUSE" {
		return topology
	}

	// Kind-specific decomposition is more precise than the generic INDEPENDENT
	// label: rendering units and warm service replicas have different recovery
	// and settlement semantics even when neither uses a collective.
	switch step.Kind {
	case "media_rendering", "image_video":
		topology.Parallelism = "FRAME_TILE_SAMPLE"
		topology.SchedulerShape = string(TopologyFrameTileSample)
		topology.Coupling = "INDEPENDENT"
		topology.Status = "SHAPE_DERIVED_NOT_PLACED"
		topology.Reason = "frames, tiles, or samples are independently verifiable units; asset locality and workers remain unresolved"
		topology.Evidence = append(topology.Evidence, "media kind selects frame/tile/sample decomposition")
		return topology
	case "service_deployment", "realtime_inference":
		topology.Parallelism = "REPLICA_SERVICE"
		topology.SchedulerShape = string(TopologyReplicaService)
		topology.Coupling = "INDEPENDENT"
		topology.Status = "SHAPE_DERIVED_NOT_PLACED"
		topology.Reason = "complete replicas serve independent requests; warm capacity and SLO evidence remain unresolved"
		topology.Evidence = append(topology.Evidence, "service kind selects replica-service shape")
		return topology
	}

	switch topology.Parallelism {
	case "INDEPENDENT":
		topology.Parallelism = "INDEPENDENT_TASK_FANOUT"
		topology.SchedulerShape = string(TopologyIndependentChunks)
		topology.Coupling = "INDEPENDENT"
		topology.Status = "SHAPE_DERIVED_NOT_PLACED"
		topology.Reason = "independent task fan-out requires no inter-worker collective; capacity and locality remain unresolved"
	case "SINGLE_DEVICE":
		topology.Parallelism = "SINGLE_DEVICE"
		topology.SchedulerShape = string(TopologySingleDevice)
		topology.Coupling = "INDEPENDENT"
		topology.Status = "SHAPE_DERIVED_NOT_PLACED"
		topology.Reason = "one authority-selected device is the bounded execution unit; hardware fit remains unresolved"
	case "TIGHT":
		topology.Parallelism = "TIGHT_PARALLELISM"
		topology.Coupling = "TIGHT"
		topology.SchedulerShape = "GANG_UNMEASURED_REFUSE"
		topology.Status = "REFUSED_UNMEASURED_FABRIC"
		topology.Reason = "tightly coupled work requires a measured collective fabric and topology-specific economics before placement"
		topology.Evidence = append(topology.Evidence, "public WAN and unmeasured fabric are refused for coupled work")
	default:
		return topology
	}
	return topology
}
