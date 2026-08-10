package main

import (
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"
)

const topologyDecisionVersion = 1

// TopologyDecision statuses. ACCEPTED and REFUSED are planner outcomes bound at
// accept. NOT_APPLICABLE is an explicit recorded refusal of topology as a
// decision axis (service lease freezes worker+pricing; exact reuse delivers a
// cached result without placing ranks).
const (
	topologyDecisionAccepted      = "ACCEPTED"
	topologyDecisionRefused       = "REFUSED"
	topologyDecisionNotApplicable = "NOT_APPLICABLE"
)

// Accept lanes that may freeze a TopologyDecision.
const (
	topologyLaneBatch        = "batch"
	topologyLaneRealtime     = "realtime"
	topologyLaneServiceLease = "service_lease"
	topologyLaneExactReuse   = "exact_result_reuse"
)

// topologyDecisionDigestCheck verifies that a claimed digest matches a fresh
// hash of the decision body. Tests may neutralise it to show failing-before
// tamper behaviour; production always leaves it true.
var topologyDecisionDigestCheck = true

// topologyDecisionEnforceWANTightRefusal refuses to treat a WAN or unmeasured
// tightly-coupled plan as ACCEPTED without a permitted cloud backstop. The
// planner already refuses; this gate is the accept-time tripwire so a forged
// ACCEPTED decision cannot be sealed. Tests may neutralise it for failing-before.
var topologyDecisionEnforceWANTightRefusal = true

// TopologyConstructionRefusal records a topology that is refused by construction
// for this accept path. It is not a ModeRefusal (supply-lane); it names a shape
// the programme will not admit until named authorities exist.
type TopologyConstructionRefusal struct {
	// Topology is a stable refusal identity (LOCAL_CLUSTER, WAN_TIGHT_MULTI_HOST).
	Topology string `json:"topology"`
	Reason   string `json:"reason"`
	// Enforced points at the code that makes the refusal permanent today.
	Enforced string `json:"enforced"`
}

// TopologyDecision is the accept-time record of which topology was chosen or
// refused. TopologyPlan remains the pure planner result; this type freezes what
// acceptance believed — degree, device/failure-domain bounds, fabric evidence
// and freshness, host-authority citations, and construction refusals.
//
// It does not invent gang scheduling, cross-machine tight topology, or a
// topology-economic authority. Those remain refused by construction.
type TopologyDecision struct {
	Version int    `json:"version"`
	Status  string `json:"status"`
	// Lane is the accept path that froze this decision.
	Lane string `json:"lane"`

	Parallelism    string                 `json:"parallelism"`
	Degree         int                    `json:"degree"`
	SchedulerShape TopologySchedulerShape `json:"scheduler_shape,omitempty"`
	// PlacementMode is the network supply lane (POOL / REPLICA_SERVICE / …).
	// Empty when Status is REFUSED or NOT_APPLICABLE.
	PlacementMode ExecutionMode `json:"placement_mode,omitempty"`
	Fabric        FabricClass   `json:"fabric"`

	// CandidateDeviceCount is the authority-reported device bound used when
	// deciding. Zero means the bound was not supplied (batch today).
	CandidateDeviceCount int `json:"candidate_device_count,omitempty"`
	// HostGPUCount / Interconnect bound a single-host plan (realtime).
	HostGPUCount int    `json:"host_gpu_count,omitempty"`
	Interconnect string `json:"interconnect,omitempty"`
	// HostTensorParallelDegree is internal host ranks. Buyer-facing Degree stays
	// 1 for replica service. Zero when not a host-TP decision.
	HostTensorParallelDegree int `json:"host_tensor_parallel_degree,omitempty"`

	// Fabric evidence believed at decide time. Empty when no evaluation was
	// consulted (batch admission always; current evaluations never admit gang).
	FabricEvaluationID       string `json:"fabric_evaluation_id,omitempty"`
	FabricEvidenceStatus     string `json:"fabric_evidence_status,omitempty"`
	FabricEvidenceFreshUntil string `json:"fabric_evidence_fresh_until,omitempty"`
	// FabricEvidenceDigest is reserved for a future receipt digest of the
	// evaluation body. Empty today: evaluations are not accept authority.
	FabricEvidenceDigest string `json:"fabric_evidence_digest,omitempty"`

	// HostTopologyAuthority names the existing host plan type when this decision
	// cites rather than re-decides it (realtime: RealtimePlacementPlan).
	HostTopologyAuthority string `json:"host_topology_authority,omitempty"`
	HostTopologyDigest    string `json:"host_topology_digest,omitempty"`

	WorkloadDecisionSHA256 string `json:"workload_decision_sha256,omitempty"`

	Reason   string        `json:"reason"`
	Refused  []ModeRefusal `json:"refused,omitempty"`
	Evidence []string      `json:"evidence,omitempty"`

	// ConstructionRefusals permanently record topologies refused by construction
	// for this accept path (measured local gang, WAN-tight multi-host, …).
	ConstructionRefusals []TopologyConstructionRefusal `json:"construction_refusals,omitempty"`
}

// localClusterConstructionRefusal is the permanent Step-10 refusal of measured
// local gang admission. Fabric evaluations force LocalClusterAdmissible=false
// and never emit LOCAL_CLUSTER_ADMITTED_V1.
func localClusterConstructionRefusal() TopologyConstructionRefusal {
	return TopologyConstructionRefusal{
		Topology: string(ModeLocalCluster),
		Reason: "measured local gang admission is refused by construction: " +
			"fabric evaluations force LocalClusterAdmissible=false and never emit " +
			fabricTopologyAdmittedStatus + "; gang scheduler, customer collectives, " +
			"and topology pricing authorities are absent",
		Enforced: "control/fabric_topology.go:LocalClusterAdmissible=false; " +
			"control/fabric_topology_planner.go:fabricTopologyAdmittedStatus never emitted",
	}
}

func wanTightConstructionRefusal(degree int, fabric FabricClass) TopologyConstructionRefusal {
	return TopologyConstructionRefusal{
		Topology: "WAN_TIGHT_MULTI_HOST",
		Reason: fmt.Sprintf(
			"tightly coupled degree %d over fabric %s is refused: the collective would "+
				"dominate and no measured low-latency site fabric admits the ranks",
			degree, fabric),
		Enforced: "control/execution_mode.go:ChooseExecutionMode WAN/unmeasured tight refusal; " +
			"control/topology_planner.go:PlanTopology",
	}
}

func topologyDecisionDigest(d TopologyDecision) (string, error) {
	if d.Version != topologyDecisionVersion {
		return "", fmt.Errorf("unsupported topology decision version %d", d.Version)
	}
	return canonicalDigest("topology decision", d)
}

// ValidateTopologyDecisionSnapshot checks structural rules without a digest.
func ValidateTopologyDecisionSnapshot(d TopologyDecision) error {
	if d.Version != topologyDecisionVersion {
		return fmt.Errorf("unsupported topology decision version %d", d.Version)
	}
	switch d.Status {
	case topologyDecisionAccepted, topologyDecisionRefused, topologyDecisionNotApplicable:
	default:
		return fmt.Errorf("topology decision has unknown status %q", d.Status)
	}
	switch d.Lane {
	case topologyLaneBatch, topologyLaneRealtime, topologyLaneServiceLease, topologyLaneExactReuse:
	default:
		return fmt.Errorf("topology decision has unknown lane %q", d.Lane)
	}
	if strings.TrimSpace(d.Reason) == "" {
		return errors.New("topology decision requires a reason")
	}
	if d.Degree < 0 {
		return errors.New("topology decision degree cannot be negative")
	}
	switch d.Status {
	case topologyDecisionAccepted:
		if d.PlacementMode == "" || d.SchedulerShape == "" {
			return errors.New("accepted topology decision requires placement mode and scheduler shape")
		}
		if d.Degree < 1 {
			return errors.New("accepted topology decision requires degree >= 1")
		}
		if d.Parallelism == "" {
			return errors.New("accepted topology decision requires parallelism")
		}
	case topologyDecisionRefused:
		if d.PlacementMode != "" || d.SchedulerShape != "" {
			return errors.New("refused topology decision must not carry an admitted placement")
		}
		if len(d.Refused) == 0 && len(d.ConstructionRefusals) == 0 {
			return errors.New("refused topology decision must record at least one refusal")
		}
	case topologyDecisionNotApplicable:
		if d.PlacementMode != "" || d.SchedulerShape != "" {
			return errors.New("not-applicable topology decision must not admit a placement")
		}
		if len(d.ConstructionRefusals) == 0 {
			return errors.New("not-applicable topology decision must record construction refusals")
		}
	}
	if d.HostTopologyAuthority != "" {
		if !validSHA256(d.HostTopologyDigest) {
			return errors.New("host topology authority requires a valid digest")
		}
	}
	if d.HostTopologyDigest != "" && d.HostTopologyAuthority == "" {
		return errors.New("host topology digest requires an authority name")
	}
	if d.WorkloadDecisionSHA256 != "" && !validSHA256(d.WorkloadDecisionSHA256) {
		return errors.New("workload decision digest is not a sha256 hex digest")
	}
	return nil
}

// ValidateTopologyDecisionDigest checks structure and, when the tamper gate is
// on, that the claimed digest matches the body.
func ValidateTopologyDecisionDigest(d TopologyDecision, digest string) error {
	if err := ValidateTopologyDecisionSnapshot(d); err != nil {
		return err
	}
	if !validSHA256(digest) {
		return errors.New("topology decision digest is not a sha256 hex digest")
	}
	if !topologyDecisionDigestCheck {
		return nil
	}
	got, err := topologyDecisionDigest(d)
	if err != nil {
		return err
	}
	if got != digest {
		return fmt.Errorf("topology decision digest mismatch: claimed %s recomputed %s", digest, got)
	}
	return nil
}

// enforceTopologyDecisionInvariants is the accept-time tripwire for physics
// refusals that the pure planner already encodes. A forged ACCEPTED decision for
// WAN/unmeasured tight work is refused here.
func enforceTopologyDecisionInvariants(d TopologyDecision, req TopologyRequest) error {
	if err := ValidateTopologyDecisionSnapshot(d); err != nil {
		return err
	}
	if !topologyDecisionEnforceWANTightRefusal {
		return nil
	}
	parallelism, degree, err := topologyParallelism(req.Parallelism.Mode, req.Parallelism.TensorParallelDegree)
	if err != nil {
		// Malformed requests are refused by PlanTopology; nothing further to enforce.
		return nil
	}
	tight := parallelism == "tensor_parallel" || parallelism == "pipeline_parallel" ||
		parallelism == "data_parallel" || parallelism == "expert_parallel"
	if !tight {
		return nil
	}
	if req.Fabric != FabricWAN && req.Fabric != FabricUnknown {
		return nil
	}
	if req.CloudBackstopPermitted {
		// Cloud backstop may accept as TopologyCloudBackstopGang; that is not WAN multi-host.
		return nil
	}
	if d.Status != topologyDecisionRefused {
		return fmt.Errorf(
			"WAN/unmeasured tight topology (degree %d, fabric %s) must be recorded as REFUSED, got %s",
			degree, req.Fabric, d.Status)
	}
	return nil
}

func topologyDecisionFromPlan(lane string, plan TopologyPlan, req TopologyRequest, workloadSHA string) (TopologyDecision, error) {
	out := TopologyDecision{
		Version:                topologyDecisionVersion,
		Lane:                   lane,
		Parallelism:            plan.Parallelism,
		Degree:                 plan.Degree,
		Fabric:                 plan.Fabric,
		CandidateDeviceCount:   req.CandidateDeviceCount,
		WorkloadDecisionSHA256: workloadSHA,
		Reason:                 plan.Reason,
		Refused:                append([]ModeRefusal(nil), plan.Refused...),
		Evidence:               append([]string(nil), plan.Evidence...),
		ConstructionRefusals:   []TopologyConstructionRefusal{localClusterConstructionRefusal()},
	}
	switch plan.Status {
	case "ACCEPTED":
		out.Status = topologyDecisionAccepted
		out.SchedulerShape = plan.SchedulerShape
		out.PlacementMode = plan.PlacementMode
	case "REFUSED", "":
		out.Status = topologyDecisionRefused
		if out.Reason == "" {
			out.Reason = "topology planning refused without an admitted placement"
		}
		// Preserve planner refusals; also name the WAN-tight construction refusal
		// when the request was tight over WAN/unknown without cloud backstop.
		parallelism := plan.Parallelism
		if parallelism == "" {
			p, _, _ := topologyParallelism(req.Parallelism.Mode, req.Parallelism.TensorParallelDegree)
			parallelism = p
		}
		tight := parallelism == "tensor_parallel" || parallelism == "pipeline_parallel" ||
			parallelism == "data_parallel" || parallelism == "expert_parallel"
		if tight && (req.Fabric == FabricWAN || req.Fabric == FabricUnknown) && !req.CloudBackstopPermitted {
			out.ConstructionRefusals = append(out.ConstructionRefusals,
				wanTightConstructionRefusal(plan.Degree, req.Fabric))
		}
	default:
		return TopologyDecision{}, fmt.Errorf("topology plan has unknown status %q", plan.Status)
	}
	if err := enforceTopologyDecisionInvariants(out, req); err != nil {
		return TopologyDecision{}, err
	}
	if err := ValidateTopologyDecisionSnapshot(out); err != nil {
		return TopologyDecision{}, err
	}
	return out, nil
}

// DecideTopology freezes a TopologyDecision from a pure planner request. Callers
// that accept work must persist the result (or an explicit NOT_APPLICABLE
// record); shadow-only TopologyPlan is not accept authority.
func DecideTopology(lane string, req TopologyRequest, workloadSHA string) (TopologyDecision, error) {
	plan, err := PlanTopology(req)
	if err != nil {
		// Malformed parallelism: record an explicit refusal rather than omitting
		// a decision (accepted work must never bind neither).
		out := TopologyDecision{
			Version: topologyDecisionVersion, Status: topologyDecisionRefused, Lane: lane,
			Parallelism: req.Parallelism.Mode, Degree: req.Parallelism.TensorParallelDegree,
			Fabric: req.Fabric, CandidateDeviceCount: req.CandidateDeviceCount,
			WorkloadDecisionSHA256: workloadSHA,
			Reason:                 err.Error(),
			ConstructionRefusals: []TopologyConstructionRefusal{
				localClusterConstructionRefusal(),
			},
			Evidence: []string{"topology planner rejected the request shape"},
		}
		if out.Degree <= 0 {
			out.Degree = 0
		}
		if err := ValidateTopologyDecisionSnapshot(out); err != nil {
			return TopologyDecision{}, err
		}
		return out, nil
	}
	return topologyDecisionFromPlan(lane, plan, req, workloadSHA)
}

// DecideTopologyFromFabricEvaluation binds a decision to a fabric evaluation
// receipt. Current evaluations always map to FabricUnknown and refuse tightly
// coupled work; independent work may still accept POOL/REPLICA with the refusal
// evidence retained.
func DecideTopologyFromFabricEvaluation(
	lane string, evaluation FabricTopologyEvaluation, req TopologyRequest, workloadSHA string, now time.Time,
) (TopologyDecision, error) {
	plan, err := PlanTopologyFromFabricEvaluation(evaluation, req, now)
	if err != nil {
		return TopologyDecision{}, err
	}
	// Re-apply fabric class the planner used so the decision records what it believed.
	req.Fabric = plan.Fabric
	out, err := topologyDecisionFromPlan(lane, plan, req, workloadSHA)
	if err != nil {
		return TopologyDecision{}, err
	}
	if evaluation.EvaluationID != uuid.Nil {
		out.FabricEvaluationID = evaluation.EvaluationID.String()
	}
	out.FabricEvidenceStatus = evaluation.Status
	if !evaluation.EvidenceFreshUntil.IsZero() {
		out.FabricEvidenceFreshUntil = evaluation.EvidenceFreshUntil.UTC().Format(time.RFC3339Nano)
	}
	// Re-validate after evidence fields are filled (digest will include them).
	if err := ValidateTopologyDecisionSnapshot(out); err != nil {
		return TopologyDecision{}, err
	}
	if err := enforceTopologyDecisionInvariants(out, req); err != nil {
		return TopologyDecision{}, err
	}
	return out, nil
}

// buildBatchTopologyDecision freezes the topology batch admission actually
// chooses today: independent task fan-out at degree 1 over an unmeasured fabric
// (POOL), with LOCAL_CLUSTER and tight multi-host refused by construction.
// Batch does not consume fabric measurements at accept.
func buildBatchTopologyDecision(decision WorkloadDecision) (TopologyDecision, error) {
	workloadSHA, err := workloadDecisionDigest(decision)
	if err != nil {
		return TopologyDecision{}, fmt.Errorf("topology decision workload digest: %w", err)
	}
	req := TopologyRequest{
		WorkloadClass:              decision.WorkloadClass,
		Parallelism:                decision.Parallelism,
		Fabric:                     FabricUnknown,
		CommunityCapacityAvailable: true,
		CloudBackstopPermitted:     false,
	}
	out, err := DecideTopology(topologyLaneBatch, req, workloadSHA)
	if err != nil {
		return TopologyDecision{}, err
	}
	out.Evidence = append(out.Evidence,
		"batch admission freezes independent_task_fanout at degree 1",
		"batch accept does not consume fabric topology evaluations (FabricUnknown is honest)",
	)
	if err := ValidateTopologyDecisionSnapshot(out); err != nil {
		return TopologyDecision{}, err
	}
	return out, nil
}

// buildExactReuseTopologyDecision records that exact-result reuse delivers a
// frozen artifact without placing ranks on a fabric.
func buildExactReuseTopologyDecision(workloadSHA string) (TopologyDecision, error) {
	out := TopologyDecision{
		Version: topologyDecisionVersion, Status: topologyDecisionNotApplicable,
		Lane: topologyLaneExactReuse, Parallelism: "single_device", Degree: 0,
		Fabric: FabricUnknown, WorkloadDecisionSHA256: workloadSHA,
		Reason: "exact result reuse delivers a frozen cached artifact without topology placement",
		ConstructionRefusals: []TopologyConstructionRefusal{
			localClusterConstructionRefusal(),
			{
				Topology: "PHYSICAL_PLACEMENT",
				Reason:   "exact reuse settles from a prior result; no multi-device topology is chosen",
				Enforced: "control/exact_reuse_batch.go: complete job without scheduler claim",
			},
		},
		Evidence: []string{"exact_result_reuse path inserts a complete job with no worker claim"},
	}
	if err := ValidateTopologyDecisionSnapshot(out); err != nil {
		return TopologyDecision{}, err
	}
	return out, nil
}

// buildRealtimeTopologyDecision cites the existing RealtimePlacementPlan as host
// topology authority. It does not re-decide tensor parallel: host TP ranks remain
// one REPLICA_SERVICE replica and are never LOCAL_CLUSTER evidence.
func buildRealtimeTopologyDecision(plan RealtimePlacementPlan, planDigest string) (TopologyDecision, error) {
	if err := ValidateFrozenRealtimePlacementPlan(plan); err != nil {
		return TopologyDecision{}, err
	}
	if !validSHA256(planDigest) {
		return TopologyDecision{}, errors.New("realtime topology decision requires placement plan digest")
	}
	// Confirm the digest matches the plan body (tamper protection).
	got, err := realtimePlacementPlanDigest(plan)
	if err != nil {
		return TopologyDecision{}, err
	}
	if got != planDigest {
		return TopologyDecision{}, fmt.Errorf(
			"realtime placement plan digest mismatch: claimed %s recomputed %s", planDigest, got)
	}
	out := TopologyDecision{
		Version:                  topologyDecisionVersion,
		Status:                   topologyDecisionAccepted,
		Lane:                     topologyLaneRealtime,
		Parallelism:              "replica_service",
		Degree:                   1,
		SchedulerShape:           TopologyReplicaService,
		PlacementMode:            ModeReplicaService,
		Fabric:                   FabricWAN,
		HostGPUCount:             plan.GPUCount,
		Interconnect:             plan.Interconnect,
		HostTensorParallelDegree: plan.AdmittedTensorParallel,
		HostTopologyAuthority:    "RealtimePlacementPlan",
		HostTopologyDigest:       planDigest,
		Reason: "realtime freezes one complete replica per worker; host tensor-parallel " +
			"ranks remain a single-host calculation cited via RealtimePlacementPlan",
		ConstructionRefusals: []TopologyConstructionRefusal{localClusterConstructionRefusal()},
		Evidence: []string{
			"RealtimePlacementPlan is the host topology authority (not re-decided here)",
			fmt.Sprintf("admitted_host_tensor_parallel=%d", plan.AdmittedTensorParallel),
			fmt.Sprintf("execution_mode=%s", plan.ExecutionMode),
			"host TP ranks remain one replica; never LOCAL_CLUSTER evidence",
		},
		Refused: []ModeRefusal{
			{Mode: ModeLocalCluster, Reason: "realtime host TP is single-host; not multi-machine gang evidence"},
			{Mode: ModePool, Reason: "long-running warm replica service is not independent batch fan-out"},
		},
	}
	if err := ValidateTopologyDecisionSnapshot(out); err != nil {
		return TopologyDecision{}, err
	}
	return out, nil
}

// buildServiceLeaseTopologyDecision records that a service lease freezes worker
// identity and PricingDecision, not multi-host topology. The absence is the
// decision — synthesising a multi-host plan would be a false record.
func buildServiceLeaseTopologyDecision() (TopologyDecision, error) {
	out := TopologyDecision{
		Version: topologyDecisionVersion, Status: topologyDecisionNotApplicable,
		Lane: topologyLaneServiceLease, Parallelism: "replica_service", Degree: 0,
		Fabric: FabricUnknown,
		Reason: "service lease freezes worker identity and PricingDecision; " +
			"it does not choose multi-host topology or measured fabric placement",
		ConstructionRefusals: []TopologyConstructionRefusal{
			localClusterConstructionRefusal(),
			{
				Topology: "MULTI_HOST_TOPOLOGY",
				Reason:   "lease admission selects one worker offer by price/latency/region; no topology plan is frozen",
				Enforced: "control/service_leases.go:CreateServiceLease freezes pricing, not topology",
			},
		},
		Evidence: []string{
			"service lease accept path has no TopologyPlan or fabric evaluation input",
			"worker and pricing are the frozen authorities for this lane",
		},
	}
	if err := ValidateTopologyDecisionSnapshot(out); err != nil {
		return TopologyDecision{}, err
	}
	return out, nil
}
