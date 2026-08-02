package main

import (
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/google/uuid"
)

// fabricTopologyAdmittedStatus is intentionally not emitted by
// EvaluateFabricTopology today. The current evidence statuses describe link
// and synthetic-collective measurements that still lack site, customer-data,
// gang-scheduler, and topology-economic authority. Keeping the admitted value
// as an explicit allow-list prevents a future status string or a hand-edited
// boolean from widening placement by accident.
const fabricTopologyAdmittedStatus = "LOCAL_CLUSTER_ADMITTED_V1"

// fabricClassForTopologyEvaluation converts a persisted topology receipt into
// the one FabricClass the planner may consume. It is a separate gate from
// PlanTopology because a receipt's connectivity facts and a scheduler's
// admission authority are different claims.
func fabricClassForTopologyEvaluation(evaluation FabricTopologyEvaluation, now time.Time) (FabricClass, []string) {
	reasons := append([]string(nil), evaluation.NonAdmissionReasons...)
	if evaluation.EvaluationID == uuid.Nil {
		reasons = append(reasons, "fabric topology evaluation has no immutable identity")
	}
	if now.IsZero() {
		now = time.Now().UTC()
	}
	if !evaluation.EvidenceFreshUntil.After(now) {
		reasons = append(reasons, "fabric topology evaluation evidence is stale")
	}
	if evaluation.Status != fabricTopologyAdmittedStatus {
		reasons = append(reasons, fmt.Sprintf("fabric topology status %q is not an explicit admission", evaluation.Status))
	}
	if !evaluation.LocalClusterAdmissible {
		reasons = append(reasons, "fabric topology evaluation is explicitly non-admissible")
	}
	if evaluation.RequiredDirectedLinks <= 0 || evaluation.VerifiedDirectedLinks != evaluation.RequiredDirectedLinks {
		reasons = append(reasons, fmt.Sprintf("fabric topology has %d/%d verified directed links", evaluation.VerifiedDirectedLinks, evaluation.RequiredDirectedLinks))
	}
	if evaluation.RequiredDirectedCollectives <= 0 || evaluation.VerifiedDirectedCollectives != evaluation.RequiredDirectedCollectives {
		reasons = append(reasons, fmt.Sprintf("fabric topology has %d/%d verified directed collectives", evaluation.VerifiedDirectedCollectives, evaluation.RequiredDirectedCollectives))
	}
	// A future authority may remove the explicit reasons when it can actually
	// govern the missing claims. Dedupe and sort here so the resulting plan and
	// its digest are deterministic even if a receipt repeats a reason.
	reasons = normalizeUniqueStrings(reasons, func(value string) string { return strings.TrimSpace(value) })
	sort.Strings(reasons)
	if len(reasons) > 0 {
		return FabricUnknown, reasons
	}
	return FabricLowLatencySite, nil
}

// PlanTopologyFromFabricEvaluation is the receipt-bound planner entry point
// for a future local-fabric admission caller. Current evaluations always map to
// FabricUnknown and therefore refuse tightly coupled work; independent work
// may still use the existing pool/replica decision, with the refusal evidence
// carried on the plan rather than discarded.
func PlanTopologyFromFabricEvaluation(
	evaluation FabricTopologyEvaluation, request TopologyRequest, now time.Time,
) (TopologyPlan, error) {
	if evaluation.EvaluationID == uuid.Nil {
		return TopologyPlan{}, fmt.Errorf("fabric topology planning requires an immutable evaluation identity")
	}
	fabric, reasons := fabricClassForTopologyEvaluation(evaluation, now)
	request.Fabric = fabric
	plan, err := PlanTopology(request)
	if err != nil {
		return TopologyPlan{}, err
	}
	plan.Evidence = append(plan.Evidence,
		fmt.Sprintf("fabric_topology_evaluation=%s", evaluation.EvaluationID),
		fmt.Sprintf("fabric_topology_status=%s", evaluation.Status),
		fmt.Sprintf("fabric_topology_class=%s", fabric),
	)
	for _, reason := range reasons {
		plan.Evidence = append(plan.Evidence, "fabric_topology_refusal="+reason)
	}
	sort.Strings(plan.Evidence)
	if len(reasons) > 0 {
		plan.Reason = fmt.Sprintf("%s; fabric evaluation %s remains non-admissible", plan.Reason, evaluation.EvaluationID)
	}
	return plan, nil
}
