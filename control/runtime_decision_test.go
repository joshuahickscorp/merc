package main

import (
	"strings"
	"testing"

	"github.com/google/uuid"
)

// runtimeDecisionFixture builds a production-shaped batch RuntimeDecision from
// the standard combined-token workload + placement path.
func runtimeDecisionFixture(t *testing.T) (WorkloadDecision, PlacementRequirement, RuntimeDecision, string) {
	t.Helper()
	sub := testOnlyCombinedTokenSubmit(t)
	workload, err := buildWorkloadDecision(sub, strings.Repeat("a", 64))
	mustf(t, err, "workload: %v")
	placement, err := placementRequirementFor(sub, workload, 1)
	mustf(t, err, "placement: %v")
	activationRev := activationAdmissionRevision(0)
	if activationRev <= 0 {
		activationRev = 1
	}
	decision, err := buildBatchRuntimeDecision(workload, placement, activationRev)
	mustf(t, err, "build runtime decision: %v")
	digest, err := runtimeDecisionDigest(decision)
	mustf(t, err, "digest: %v")
	return workload, placement, decision, digest
}

func TestRuntimeDecisionStatesLifecycleLadderBasisNotMeasured(t *testing.T) {
	_, _, decision, digest := runtimeDecisionFixture(t)
	if err := ValidateRuntimeDecisionDigest(decision, digest); err != nil {
		t.Fatalf("valid decision fails: %v", err)
	}
	if decision.SelectionBasis != runtimeSelectionBasisLifecycleLadder {
		t.Fatalf("selection_basis=%q want %s", decision.SelectionBasis, runtimeSelectionBasisLifecycleLadder)
	}
	if decision.ShadowSelectionAuthoritative {
		t.Fatal("shadow must not be authoritative on accepted RuntimeDecision")
	}
	if isMeasuredShadowSelectionBasis(decision.SelectionBasis) {
		t.Fatalf("accepted basis must not be a measured shadow basis: %q", decision.SelectionBasis)
	}
	if !strings.Contains(strings.ToLower(decision.SelectionNote), "not a measured multi-engine tournament") {
		t.Fatalf("selection note must deny measured tournament: %q", decision.SelectionNote)
	}
	if decision.ActivationPolicyRevision <= 0 {
		t.Fatal("activation revision must be frozen on the decision")
	}
	if decision.CellID == "" || decision.Engine == "" || decision.PerformanceAuthorityDigest == "" {
		t.Fatalf("missing core bindings: %+v", decision)
	}
	if decision.Lane != runtimeLaneBatch || decision.Status != runtimeDecisionAccepted {
		t.Fatalf("lane/status: %+v", decision)
	}
}

func TestRuntimeDecisionLadderBasisCannotBeLabelledMeasured(t *testing.T) {
	_, _, decision, _ := runtimeDecisionFixture(t)

	// Forge: re-label the ladder decision as a measured shadow basis.
	forged := decision
	forged.SelectionBasis = selectionBasisThroughputEqualLiability
	forged.SelectionNote = "forged measured tournament win"

	// Failing-before: with the honesty tripwire neutralised, a measured basis
	// incorrectly passes the honesty gate.
	prev := runtimeDecisionEnforceBasisHonesty
	runtimeDecisionEnforceBasisHonesty = false
	t.Cleanup(func() { runtimeDecisionEnforceBasisHonesty = prev })
	if err := enforceRuntimeDecisionBasisHonesty(forged); err != nil {
		t.Fatalf("neutralised basis honesty should not fire: %v", err)
	}
	// Passing-after: restore the tripwire; measured basis must refuse.
	runtimeDecisionEnforceBasisHonesty = true
	if err := enforceRuntimeDecisionBasisHonesty(forged); err == nil {
		t.Fatal("measured shadow basis must not seal as accepted RuntimeDecision")
	}
	if err := ValidateRuntimeDecisionSnapshot(forged); err == nil {
		t.Fatal("ValidateRuntimeDecisionSnapshot must refuse measured basis")
	}

	// Also refuse TIE_NO_DECISION and shadow-authoritative flag.
	forged.SelectionBasis = selectionBasisTieNoDecision
	if err := ValidateRuntimeDecisionSnapshot(forged); err == nil {
		t.Fatal("TIE_NO_DECISION must not seal as accept basis")
	}
	forged.SelectionBasis = runtimeSelectionBasisLifecycleLadder
	forged.ShadowSelectionAuthoritative = true
	if err := ValidateRuntimeDecisionSnapshot(forged); err == nil {
		t.Fatal("shadow_selection_authoritative=true must refuse")
	}
}

func TestRuntimeDecisionShadowSelectionCannotBecomeAcceptedBasis(t *testing.T) {
	_, _, decision, _ := runtimeDecisionFixture(t)
	// A shadow row might record MORE_THROUGHPUT…; that string is never accept authority.
	if isAcceptedRuntimeSelectionBasis(selectionBasisThroughputEqualLiability) {
		t.Fatal("shadow throughput basis must not be in the accepted set")
	}
	if isAcceptedRuntimeSelectionBasis(selectionBasisTieNoDecision) {
		t.Fatal("shadow tie basis must not be in the accepted set")
	}
	if !isAcceptedRuntimeSelectionBasis(runtimeSelectionBasisLifecycleLadder) {
		t.Fatal("LIFECYCLE_LADDER must be accepted")
	}
	if !isAcceptedRuntimeSelectionBasis(runtimeSelectionBasisDirectedCell) {
		t.Fatal("DIRECTED_CELL must be accepted")
	}
	// Rebuilding from accept inputs always yields non-shadow authority.
	if decision.ShadowSelectionAuthoritative ||
		decision.SelectionBasis == selectionBasisThroughputEqualLiability {
		t.Fatalf("builder produced shadow-authoritative decision: %+v", decision)
	}
}

func TestRuntimeDecisionTamperFailsDigest(t *testing.T) {
	_, _, decision, digest := runtimeDecisionFixture(t)
	if err := ValidateRuntimeDecisionDigest(decision, digest); err != nil {
		t.Fatalf("pre-tamper: %v", err)
	}

	mutated := decision
	mutated.CellID = "tampered-cell-id"

	// Failing-before: neutralise digest check — structure alone still passes
	// if basis honesty is intact.
	prev := runtimeDecisionDigestCheck
	runtimeDecisionDigestCheck = false
	t.Cleanup(func() { runtimeDecisionDigestCheck = prev })
	if err := ValidateRuntimeDecisionDigest(mutated, digest); err != nil {
		t.Fatalf("neutralised digest check should accept structure: %v", err)
	}

	// Passing-after: restore; mutation must fail digest.
	runtimeDecisionDigestCheck = true
	if err := ValidateRuntimeDecisionDigest(mutated, digest); err == nil {
		t.Fatal("mutated decision must fail digest")
	}
	mutDigest, err := runtimeDecisionDigest(mutated)
	mustf(t, err, "mut digest: %v")
	if mutDigest == digest {
		t.Fatal("mutating cell_id left digest unchanged")
	}
}

func TestEvidenceEnvelopeBindsRuntimeLinkInsteadOfAbsent(t *testing.T) {
	d := sampleBatchDigests()
	env, err := buildBatchAcceptEvidenceEnvelope(uuid.New(), d)
	mustf(t, err, "build envelope: %v")
	link, ok := env.linkByKind(EnvelopeLinkRuntime)
	if !ok {
		t.Fatal("missing runtime link")
	}
	if link.Status != EnvelopeLinkBound {
		t.Fatalf("runtime status=%s want BOUND (not ABSENT)", link.Status)
	}
	if link.Authority != "RuntimeDecision" {
		t.Fatalf("authority=%q want RuntimeDecision", link.Authority)
	}
	if link.Digest != d.RuntimeSHA256 {
		t.Fatalf("runtime digest %q want %q", link.Digest, d.RuntimeSHA256)
	}
	if link.Reason != "" {
		t.Fatalf("BOUND runtime must not carry reason: %q", link.Reason)
	}
}

func TestRuntimeDecisionDirectedCellBasis(t *testing.T) {
	// Directed routing freezes DIRECTED_CELL rather than implying a ladder
	// tournament. Uses the same combined-token cell forced as directed.
	sub := testOnlyCombinedTokenSubmit(t)
	cellID := "candle-metal-llama1-infer"
	workload, err := buildWorkloadDecisionDirected(sub, strings.Repeat("d", 64), cellID)
	if err != nil {
		// Directed pool may not include the cell without directed capabilities.
		// That is still a useful signal — skip only when directed is unavailable.
		t.Skipf("directed combined-token cell unavailable: %v", err)
	}
	placement, err := placementRequirementFor(sub, workload, 1)
	mustf(t, err, "placement: %v")
	decision, err := buildBatchRuntimeDecision(workload, placement, 1)
	mustf(t, err, "build: %v")
	if decision.SelectionBasis != runtimeSelectionBasisDirectedCell {
		t.Fatalf("basis=%q want %s", decision.SelectionBasis, runtimeSelectionBasisDirectedCell)
	}
	if decision.CellID != cellID {
		t.Fatalf("cell=%q want %s", decision.CellID, cellID)
	}
	if decision.ShadowSelectionAuthoritative {
		t.Fatal("directed freeze must not mark shadow authoritative")
	}
}
