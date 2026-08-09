package main

import (
	"os"
	"strings"
	"testing"
)

// The batch reuse key must bind the RUNTIME AUTHORITY, not only the request
// shape.
//
// Two jobs can ask for the same model, the same input, the same parameters and
// the same output shape, and still be different purchases when they freeze
// different runtime authorities. A reuse key derived from the binding alone
// collides across them, and the second buyer can be served the first runtime's
// bytes at the reuse price with a receipt naming a cell that never ran.
//
// This is asserted at SOURCE rather than through behaviour, and the reason is
// worth stating rather than hiding: `batchExactReuseEnabled` is a compile-time
// `false`, so batchRequestIdentity returns "" before it reaches the digest and no
// runtime test can distinguish the two derivations. The mutation suite found
// exactly that — "exact reuse hashes request shape but not runtime authority"
// SURVIVED, because the code it mutates is unreachable. A surviving mutation on
// disabled code is not a passing grade; it is a hole waiting for the day the flag
// is flipped back.
func TestBatchReuseIdentityBindsRuntimeAuthorityNotJustRequestShape(t *testing.T) {
	// First the property itself, so the source assertion below is anchored to a
	// real difference rather than to a spelling. Production intentionally has no
	// currently bindable lane, so build one decision under the explicit TEST_ONLY
	// combined-token authority and derive a second frozen authority projection in
	// memory. This is a digest property, not an assertion that either alternate
	// cell is a production route.
	base, err := buildWorkloadDecision(
		testOnlyCombinedTokenSubmit(t), strings.Repeat("a", 64),
	)
	mustf(t, err, "build workload decision: %v")
	directed := base
	directed.RuntimeCandidates = append(
		[]WorkloadRuntimeCandidate(nil), base.RuntimeCandidates...,
	)
	directed.RuntimeCandidates[0].CellID = "TEST_ONLY-alternate-runtime-cell"
	directed.RuntimeCandidates[0].RuntimeID = "TEST_ONLY_alternate_runtime"
	directed.RuntimeCandidates[0].Engine = "TEST_ONLY-alternate-engine"
	directed.DirectedCellID = directed.RuntimeCandidates[0].CellID

	baseBinding, err := workloadBindingDigest(base.Binding)
	must(t, err)
	directedBinding, err := workloadBindingDigest(directed.Binding)
	must(t, err)
	baseDecision, err := workloadDecisionDigest(base)
	must(t, err)
	directedDecision, err := workloadDecisionDigest(directed)
	must(t, err)

	// The bindings agree — same buyer request — and the decisions do not, because
	// one is frozen onto a different runtime cell. That gap IS the reuse hazard.
	if baseBinding != directedBinding {
		t.Fatalf("the two requests do not share a binding, so this proves nothing:\n"+
			"  %s\n  %s", baseBinding, directedBinding)
	}
	if baseDecision == directedDecision {
		t.Fatal("two decisions frozen onto different runtime cells produced one " +
			"decision digest; the reuse key could not tell them apart even if it " +
			"bound the whole decision")
	}

	// Now the derivation actually in the source. Read rather than called, because
	// the flag makes the call unreachable.
	raw, err := os.ReadFile("exact_reuse_batch.go")
	must(t, err)
	source := string(raw)
	if !strings.Contains(source, "const batchExactReuseEnabled = false") {
		t.Skip("batch exact reuse is enabled again; assert this through behaviour " +
			"instead and delete this source check")
	}
	if !strings.Contains(source, "decisionSHA256, err := workloadDecisionDigest(decision)") {
		t.Fatal("batchRequestIdentity no longer derives its key from the whole " +
			"workload decision. If it now hashes the binding alone, two jobs frozen " +
			"onto different runtime cells share a reuse key, and the second buyer is " +
			"served the first runtime's bytes at the reuse price with a receipt " +
			"naming a cell that never ran.")
	}
	if strings.Contains(source, "workloadBindingDigest(decision.Binding)") {
		t.Fatal("batchRequestIdentity hashes the binding rather than the decision")
	}
}
