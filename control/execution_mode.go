package main

import (
	"fmt"
	"strings"
)

// The four execution modes, and the rule that refuses a topology the physics
// does not support.
//
// merc.md §14's exit gate is not "Merc can run in four modes" — it is that Merc
// can CHOOSE among them and explain why a workload was placed in one rather than
// another. So the output here is a decision carrying its own reasoning, and the
// interesting half is the refusals: a selector that can only ever say yes has not
// chosen anything.
//
// The refusal that matters is tightly coupled parallelism over the public
// internet. Tensor, pipeline and expert parallelism synchronise per layer or per
// token; on a WAN the collective dominates and seven machines in seven houses are
// slower AND more expensive than one cloud node. Nothing in this tree has
// measured otherwise, so the mode is refused rather than offered — which is the
// same fail-closed posture the runtime authority takes toward an unproven cell.

// ExecutionMode is where and how a workload's tasks are placed.
type ExecutionMode string

const (
	// ModePool is independent chunks over heterogeneous machines on the public
	// internet. Communication between workers is nil, so unreliable and
	// differently configured hosts can contribute safely.
	ModePool ExecutionMode = "POOL"
	// ModeReplicaService is one complete model per worker with Merc routing
	// independent requests between replicas. No worker talks to another.
	ModeReplicaService ExecutionMode = "REPLICA_SERVICE"
	// ModeLocalCluster is several machines at ONE site jointly executing one
	// tightly coupled workload over a measured low-latency fabric.
	ModeLocalCluster ExecutionMode = "LOCAL_CLUSTER"
	// ModeCloudBackstop is provider capacity used to protect a deadline or to
	// serve a workload the community fleet cannot.
	ModeCloudBackstop ExecutionMode = "CLOUD_BACKSTOP"
)

// FabricClass is what is actually known about the links between the machines a
// workload would be placed on.
type FabricClass string

const (
	// FabricUnknown is the default, and it is a refusal for anything that needs a
	// fabric. A topology nobody measured is not a topology.
	FabricUnknown FabricClass = "UNKNOWN"
	// FabricWAN is the public internet: independent work only.
	FabricWAN FabricClass = "WAN"
	// FabricLowLatencySite is one site, one owner, wired, with MEASURED collective
	// bandwidth and latency.
	FabricLowLatencySite FabricClass = "LOW_LATENCY_SITE"
)

// ParallelismCoupling says whether the workload's ranks must talk to each other.
type ParallelismCoupling string

const (
	// CouplingIndependent is task fan-out: every unit completes alone.
	CouplingIndependent ParallelismCoupling = "INDEPENDENT"
	// CouplingTight is tensor, pipeline or expert parallelism — a collective per
	// layer or per token.
	CouplingTight ParallelismCoupling = "TIGHT"
)

// PlacementRequest is what the placement rule is allowed to know. Every field is
// something the tree can actually establish; nothing here is inferred from a
// buyer's request, because a buyer naming a topology would be choosing their own
// supply.
type PlacementRequest struct {
	WorkloadClass string
	// Coupling and Degree come from the frozen workload decision's parallelism.
	Coupling ParallelismCoupling
	Degree   int
	// Fabric is what has been MEASURED about the candidate machines' links.
	Fabric FabricClass
	// LongRunningService distinguishes a warm replica lease from a finite project.
	LongRunningService bool
	// CommunityCapacityAvailable and CloudBackstopPermitted are supply facts, not
	// preferences: the first is whether the fleet can serve this at all, the
	// second whether the buyer's privacy and region terms allow a provider.
	CommunityCapacityAvailable bool
	CloudBackstopPermitted     bool
	// DeadlineAtRisk is set when the community path cannot meet the accepted
	// completion window.
	DeadlineAtRisk bool
}

// PlacementDecision is the chosen mode with the argument for it.
type PlacementDecision struct {
	Mode ExecutionMode `json:"mode"`
	// Reason is one sentence naming the property that decided it.
	Reason string `json:"reason"`
	// Refused lists the modes that were considered and rejected, each with the
	// reason in the workload's own terms. This is the half that makes a placement
	// reviewable: "it chose POOL" is not checkable, "it refused LOCAL_CLUSTER
	// because the workload is tightly coupled and the fabric is UNKNOWN" is.
	Refused []ModeRefusal `json:"refused"`
}

// ModeRefusal is one rejected mode.
type ModeRefusal struct {
	Mode   ExecutionMode `json:"mode"`
	Reason string        `json:"reason"`
}

// Explain renders the decision as one auditable line.
func (d PlacementDecision) Explain() string {
	var b strings.Builder
	fmt.Fprintf(&b, "%s: %s", d.Mode, d.Reason)
	for _, r := range d.Refused {
		fmt.Fprintf(&b, "; refused %s: %s", r.Mode, r.Reason)
	}
	return b.String()
}

// errNoAdmissibleExecutionMode is returned when every mode is refused. It is an
// error rather than a default, because defaulting to POOL for a workload that
// needs a fabric would place tightly coupled work on the public internet.
type errNoAdmissibleExecutionMode struct{ refused []ModeRefusal }

func (e errNoAdmissibleExecutionMode) Error() string {
	parts := make([]string, 0, len(e.refused))
	for _, r := range e.refused {
		parts = append(parts, fmt.Sprintf("%s: %s", r.Mode, r.Reason))
	}
	return "no admissible execution mode (" + strings.Join(parts, "; ") + ")"
}

// ChooseExecutionMode places a workload and explains the placement.
func ChooseExecutionMode(req PlacementRequest) (PlacementDecision, error) {
	var refused []ModeRefusal
	refuse := func(mode ExecutionMode, format string, args ...any) {
		refused = append(refused, ModeRefusal{Mode: mode, Reason: fmt.Sprintf(format, args...)})
	}

	tight := req.Coupling == CouplingTight || req.Degree > 1

	// LOCAL_CLUSTER first, because it is the only mode that can serve tightly
	// coupled work at all, and the only one with a hard physical precondition.
	switch {
	case !tight:
		refuse(ModeLocalCluster,
			"the workload is independent task fan-out, so gang scheduling would add "+
				"synchronisation cost and buy nothing")
	case req.Fabric == FabricLowLatencySite:
		return PlacementDecision{
			Mode: ModeLocalCluster,
			Reason: fmt.Sprintf(
				"degree %d needs a collective per token and the candidate machines share a "+
					"measured low-latency site fabric", req.Degree),
			Refused: append(refused,
				ModeRefusal{ModePool, "the ranks must synchronise, so they cannot run as independent chunks"},
				ModeRefusal{ModeReplicaService, "no single machine holds a complete replica at this degree"},
			),
		}, nil
	case req.Fabric == FabricWAN:
		refuse(ModeLocalCluster,
			"tightly coupled degree %d over a WAN fabric: the collective would dominate and "+
				"nothing has measured it cheaper than one provider node", req.Degree)
	default:
		refuse(ModeLocalCluster,
			"tightly coupled degree %d and the fabric is %s; an unmeasured topology is refused, "+
				"not assumed", req.Degree, req.Fabric)
	}

	// A tightly coupled workload that could not get LOCAL_CLUSTER cannot be split
	// across independent workers at all. Cloud is the only remaining path.
	if tight {
		refuse(ModePool, "the ranks must synchronise, so they cannot run as independent chunks")
		refuse(ModeReplicaService, "no single community machine holds a complete replica at degree %d", req.Degree)
		if !req.CloudBackstopPermitted {
			refuse(ModeCloudBackstop, "the buyer's privacy or region terms do not permit a provider")
			return PlacementDecision{}, errNoAdmissibleExecutionMode{refused}
		}
		return PlacementDecision{
			Mode: ModeCloudBackstop,
			Reason: fmt.Sprintf(
				"degree %d is tightly coupled and no measured low-latency fabric is available, "+
					"so one provider host is both faster and cheaper than any distributed option",
				req.Degree),
			Refused: refused,
		}, nil
	}

	// Independent work. A long-running service wants a warm replica per worker; a
	// finite project wants chunks spread over whatever is available.
	if req.CommunityCapacityAvailable && !req.DeadlineAtRisk {
		if req.LongRunningService {
			refuse(ModePool, "a warm service must keep a loaded replica between requests, "+
				"which chunked pool work does not")
			refuse(ModeCloudBackstop, "community replicas can hold this model and the deadline is not at risk")
			return PlacementDecision{
				Mode:    ModeReplicaService,
				Reason:  "independent requests against complete per-worker replicas, routed by cost and warm state",
				Refused: refused,
			}, nil
		}
		refuse(ModeReplicaService, "a finite project does not need a warm replica held between requests")
		refuse(ModeCloudBackstop, "the community fleet can serve this and the deadline is not at risk")
		return PlacementDecision{
			Mode:    ModePool,
			Reason:  "independent chunks with no inter-worker communication, so heterogeneous public-internet machines can contribute",
			Refused: refused,
		}, nil
	}

	if !req.CloudBackstopPermitted {
		if !req.CommunityCapacityAvailable {
			refuse(ModePool, "no community capacity is available for this workload")
			refuse(ModeReplicaService, "no community worker can hold a complete replica")
		} else {
			refuse(ModePool, "community capacity cannot meet the accepted completion window")
			refuse(ModeReplicaService, "community capacity cannot meet the accepted completion window")
		}
		refuse(ModeCloudBackstop, "the buyer's privacy or region terms do not permit a provider")
		return PlacementDecision{}, errNoAdmissibleExecutionMode{refused}
	}
	reason := "no community capacity is available for this workload"
	if req.CommunityCapacityAvailable {
		reason = "community capacity cannot meet the accepted completion window"
	}
	refuse(ModePool, "%s", reason)
	refuse(ModeReplicaService, "%s", reason)
	return PlacementDecision{
		Mode:    ModeCloudBackstop,
		Reason:  "provider capacity protects the accepted deadline: " + reason,
		Refused: refused,
	}, nil
}

// CouplingForParallelism maps a frozen workload decision's parallelism onto the
// coupling the placement rule reasons about.
//
// Reads the decision, never the request. "independent_task_fanout" is the only
// mode this tree currently freezes, and anything else is treated as tight —
// failing closed, so a new parallelism mode added upstream cannot silently be
// placed on the public internet.
func CouplingForParallelism(p WorkloadParallelism) ParallelismCoupling {
	if p.Mode == "independent_task_fanout" && p.TensorParallelDegree <= 1 {
		return CouplingIndependent
	}
	return CouplingTight
}
