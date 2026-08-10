package main

import (
	"encoding/json"
	"strings"
	"testing"
)

// TestSubmitJobTxBindsTopologyDecisionInAcceptTransaction proves every accepted
// batch job freezes TopologyDecision inside the same transaction as money and
// placement — never only as post-commit shadow TopologyPlan.
func TestSubmitJobTxBindsTopologyDecisionInAcceptTransaction(t *testing.T) {
	ctx, store, pool, f, job, tasks, _ := currentUniformMoneyPathJob(t)
	mustf(t, store.SubmitJobTx(ctx, job, tasks), "SubmitJobTx: %v")

	var topologyJSON []byte
	var topologySHA, envelopeSHA string
	mustf(t, pool.QueryRow(ctx, `
		SELECT topology_decision, COALESCE(topology_decision_sha256,''),
		       COALESCE(evidence_envelope_sha256,'')
		  FROM jobs WHERE id=$1`, f.JobID,
	).Scan(&topologyJSON, &topologySHA, &envelopeSHA), "load topology: %v")
	if len(topologyJSON) == 0 || !validSHA256(topologySHA) {
		t.Fatalf("accepted job bound neither topology decision nor digest: json=%d sha=%q",
			len(topologyJSON), topologySHA)
	}

	var decision TopologyDecision
	mustf(t, json.Unmarshal(topologyJSON, &decision), "decode topology: %v")
	if err := ValidateTopologyDecisionDigest(decision, topologySHA); err != nil {
		t.Fatalf("stored topology fails digest: %v", err)
	}
	// Batch today: independent fan-out → POOL, never neither.
	if decision.Status != topologyDecisionAccepted &&
		decision.Status != topologyDecisionRefused &&
		decision.Status != topologyDecisionNotApplicable {
		t.Fatalf("accepted job topology status is neither accept nor refusal: %+v", decision)
	}
	if strings.TrimSpace(decision.Reason) == "" {
		t.Fatal("topology decision missing reason")
	}
	if decision.Lane != topologyLaneBatch {
		t.Fatalf("lane=%q want batch", decision.Lane)
	}
	// Production batch shape: accepted independent POOL with LOCAL_CLUSTER refused.
	if decision.Status != topologyDecisionAccepted ||
		decision.PlacementMode != ModePool ||
		decision.SchedulerShape != TopologyIndependentChunks {
		t.Fatalf("batch production topology shape: %+v", decision)
	}
	foundLocalRefuse := false
	for _, r := range decision.ConstructionRefusals {
		if r.Topology == string(ModeLocalCluster) {
			foundLocalRefuse = true
		}
	}
	if !foundLocalRefuse {
		t.Fatalf("batch decision must record LOCAL_CLUSTER construction refusal: %+v",
			decision.ConstructionRefusals)
	}

	// Evidence envelope cites the same jobs column digest.
	if !validSHA256(envelopeSHA) {
		t.Fatalf("missing envelope root: %q", envelopeSHA)
	}
	env, err := store.loadEvidenceEnvelope(ctx, EnvelopeLaneBatch, f.JobID)
	mustf(t, err, "load envelope: %v")
	link, ok := env.linkByKind(EnvelopeLinkTopology)
	if !ok || link.Status != EnvelopeLinkBound || link.Authority != "TopologyDecision" {
		t.Fatalf("envelope topology link: %+v", link)
	}
	if link.Digest != topologySHA {
		t.Fatalf("envelope topology digest %s != jobs column %s", link.Digest, topologySHA)
	}

	// Immutability: rewrite refused.
	if _, err := pool.Exec(ctx, `
		UPDATE jobs SET topology_decision_sha256=$2 WHERE id=$1`,
		f.JobID, strings.Repeat("0", 64)); err == nil {
		t.Fatal("database allowed topology_decision_sha256 rewrite")
	}
}
