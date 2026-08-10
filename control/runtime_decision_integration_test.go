package main

import (
	"encoding/json"
	"strings"
	"testing"
)

// TestSubmitJobTxBindsRuntimeDecisionInAcceptTransaction proves every accepted
// batch job freezes RuntimeDecision inside the same transaction as money and
// placement — never only as post-commit shadow selection.
func TestSubmitJobTxBindsRuntimeDecisionInAcceptTransaction(t *testing.T) {
	ctx, store, pool, f, job, tasks, _ := currentUniformMoneyPathJob(t)
	mustf(t, store.SubmitJobTx(ctx, job, tasks), "SubmitJobTx: %v")

	var runtimeJSON []byte
	var runtimeSHA, envelopeSHA, workloadSHA, placementSHA string
	mustf(t, pool.QueryRow(ctx, `
		SELECT runtime_decision, COALESCE(runtime_decision_sha256,''),
		       COALESCE(evidence_envelope_sha256,''),
		       COALESCE(workload_decision_sha256,''),
		       COALESCE(placement_requirement_sha256,'')
		  FROM jobs WHERE id=$1`, f.JobID,
	).Scan(&runtimeJSON, &runtimeSHA, &envelopeSHA, &workloadSHA, &placementSHA),
		"load runtime: %v")
	if len(runtimeJSON) == 0 || !validSHA256(runtimeSHA) {
		t.Fatalf("accepted job bound neither runtime decision nor digest: json=%d sha=%q",
			len(runtimeJSON), runtimeSHA)
	}

	var decision RuntimeDecision
	mustf(t, json.Unmarshal(runtimeJSON, &decision), "decode runtime: %v")
	if err := ValidateRuntimeDecisionDigest(decision, runtimeSHA); err != nil {
		t.Fatalf("stored runtime fails digest: %v", err)
	}
	if decision.Status != runtimeDecisionAccepted || decision.Lane != runtimeLaneBatch {
		t.Fatalf("status/lane: %+v", decision)
	}
	// Ordinary money-path admission: lifecycle ladder, not measured shadow.
	if decision.SelectionBasis != runtimeSelectionBasisLifecycleLadder {
		t.Fatalf("selection_basis=%q want %s", decision.SelectionBasis, runtimeSelectionBasisLifecycleLadder)
	}
	if decision.ShadowSelectionAuthoritative {
		t.Fatal("shadow must not be money authority on accepted runtime decision")
	}
	if isMeasuredShadowSelectionBasis(decision.SelectionBasis) {
		t.Fatalf("accepted basis is measured: %q", decision.SelectionBasis)
	}
	if decision.ActivationPolicyRevision <= 0 {
		t.Fatal("activation revision not frozen")
	}
	if decision.WorkloadDecisionSHA256 != workloadSHA {
		t.Fatalf("runtime cites workload %s want jobs column %s",
			decision.WorkloadDecisionSHA256, workloadSHA)
	}
	if decision.PlacementRequirementSHA256 != placementSHA {
		t.Fatalf("runtime cites placement %s want jobs column %s",
			decision.PlacementRequirementSHA256, placementSHA)
	}
	if decision.CellID == "" || decision.Engine == "" {
		t.Fatalf("missing engine/cell: %+v", decision)
	}
	if !validSHA256(decision.PerformanceAuthorityDigest) {
		t.Fatalf("missing performance authority digest: %q", decision.PerformanceAuthorityDigest)
	}

	// Evidence envelope cites the same jobs column digest (BOUND, not ABSENT).
	if !validSHA256(envelopeSHA) {
		t.Fatalf("missing envelope root: %q", envelopeSHA)
	}
	env, err := store.loadEvidenceEnvelope(ctx, EnvelopeLaneBatch, f.JobID)
	mustf(t, err, "load envelope: %v")
	link, ok := env.linkByKind(EnvelopeLinkRuntime)
	if !ok || link.Status != EnvelopeLinkBound || link.Authority != "RuntimeDecision" {
		t.Fatalf("envelope runtime link: %+v", link)
	}
	if link.Digest != runtimeSHA {
		t.Fatalf("envelope runtime digest %s != jobs column %s", link.Digest, runtimeSHA)
	}

	// Chain observation includes runtime among BOUND columns.
	obs, err := store.ObserveAcceptedBatchChain(ctx, f.JobID)
	mustf(t, err, "observe chain: %v")
	if !obs.ChainCoherent {
		t.Fatalf("chain incoherent after accept: %+v", obs)
	}

	// Immutability: rewrite refused.
	if _, err := pool.Exec(ctx, `
		UPDATE jobs SET runtime_decision_sha256=$2 WHERE id=$1`,
		f.JobID, strings.Repeat("0", 64)); err == nil {
		t.Fatal("database allowed runtime_decision_sha256 rewrite")
	}
}
