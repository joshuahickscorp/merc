package main

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestSubmitJobTxWritesEvidenceEnvelopeInAcceptTransaction(t *testing.T) {
	ctx, store, pool, f, job, tasks, _ := currentUniformMoneyPathJob(t)
	// Supply a request digest so the request link is BOUND and comparable.
	job.SubmitRequestSHA256 = strings.Repeat("ab", 32)

	mustf(t, store.SubmitJobTx(ctx, job, tasks), "SubmitJobTx: %v")

	var jobEnvelopeSHA, workloadSHA, placementSHA, pricingSHA, topologySHA, requestSHA string
	mustf(t, pool.QueryRow(ctx, `
		SELECT COALESCE(evidence_envelope_sha256,''),
		       COALESCE(workload_decision_sha256,''),
		       COALESCE(placement_requirement_sha256,''),
		       COALESCE(pricing_decision_sha256,''),
		       COALESCE(topology_decision_sha256,''),
		       COALESCE(submit_request_sha256,'')
		  FROM jobs WHERE id=$1`, f.JobID,
	).Scan(&jobEnvelopeSHA, &workloadSHA, &placementSHA, &pricingSHA, &topologySHA, &requestSHA),
		"load job digests: %v")
	if !validSHA256(jobEnvelopeSHA) {
		t.Fatalf("job missing evidence_envelope_sha256: %q", jobEnvelopeSHA)
	}
	if !validSHA256(topologySHA) {
		t.Fatalf("job missing topology_decision_sha256: %q", topologySHA)
	}

	env, err := store.loadEvidenceEnvelope(ctx, EnvelopeLaneBatch, f.JobID)
	mustf(t, err, "load evidence envelope: %v")
	if env.EnvelopeSHA256 != jobEnvelopeSHA {
		t.Fatalf("jobs.evidence_envelope_sha256=%s envelope root=%s",
			jobEnvelopeSHA, env.EnvelopeSHA256)
	}
	if err := ValidateEvidenceEnvelope(*env); err != nil {
		t.Fatalf("stored envelope fails validation: %v", err)
	}

	// Existing receipt/job digests are the ones bound — not recomputed copies.
	for kind, want := range map[string]string{
		EnvelopeLinkWorkload:  workloadSHA,
		EnvelopeLinkPlacement: placementSHA,
		EnvelopeLinkPricing:   pricingSHA,
		EnvelopeLinkTopology:  topologySHA,
		EnvelopeLinkRequest:   requestSHA,
	} {
		link, ok := env.linkByKind(kind)
		if !ok || link.Status != EnvelopeLinkBound {
			t.Fatalf("%s not BOUND: %+v", kind, link)
		}
		if link.Digest != want {
			t.Fatalf("%s bound digest %q != jobs column %q", kind, link.Digest, want)
		}
	}

	// Recompute pricing from the stored body and prove the envelope cites the
	// same jobs column digest (which matches the body), not a second hash.
	var pricingJSON []byte
	mustf(t, pool.QueryRow(ctx, `SELECT pricing_decision FROM jobs WHERE id=$1`, f.JobID).
		Scan(&pricingJSON), "load pricing body: %v")
	var pricing PricingDecision
	mustf(t, json.Unmarshal(pricingJSON, &pricing), "decode pricing: %v")
	bodyDigest, err := pricingDecisionDigest(pricing)
	mustf(t, err, "pricing body digest: %v")
	if bodyDigest != pricingSHA {
		t.Fatalf("jobs.pricing_decision_sha256 diverged from body: col=%s body=%s",
			pricingSHA, bodyDigest)
	}
	pricingLink, _ := env.linkByKind(EnvelopeLinkPricing)
	if pricingLink.Digest != bodyDigest {
		t.Fatalf("envelope pricing digest %s != body digest %s (recomputed copy?)",
			pricingLink.Digest, bodyDigest)
	}

	// Append-only: mutation of the stored envelope is refused.
	if _, err := pool.Exec(ctx, `
		UPDATE evidence_envelopes
		   SET envelope = envelope || '{"version":99}'::jsonb
		 WHERE envelope_sha256=$1`, jobEnvelopeSHA); err == nil {
		t.Fatal("database allowed evidence envelope mutation")
	}
	if _, err := pool.Exec(ctx, `
		UPDATE jobs SET evidence_envelope_sha256=$2 WHERE id=$1`,
		f.JobID, strings.Repeat("0", 64)); err == nil {
		t.Fatal("database allowed jobs.evidence_envelope_sha256 rewrite")
	}
}

func TestSubmitJobTxRolledBackAcceptLeavesNoEvidenceEnvelope(t *testing.T) {
	ctx, store, pool, f, job, tasks, _ := currentUniformMoneyPathJob(t)

	// Force a late failure inside the accept TX: insert succeeds through job +
	// envelope only if we get past them; mismatched bound-quote compute fails
	// before the jobs INSERT. Use a post-INSERT failure via economic plan by
	// opening a manual TX that mirrors the accept write and rolls back.
	workloadSHA, err := workloadDecisionDigest(job.WorkloadDecision)
	mustf(t, err, "workload digest: %v")
	computeSHA, err := computePlanDigest(job.ComputePlan)
	mustf(t, err, "compute digest: %v")
	placementSHA, err := placementRequirementDigest(job.PlacementRequirement)
	mustf(t, err, "placement digest: %v")
	pricingSHA, err := pricingDecisionDigest(job.PricingDecision)
	mustf(t, err, "pricing digest: %v")
	topologyDecision, err := buildBatchTopologyDecision(job.WorkloadDecision)
	mustf(t, err, "topology decision: %v")
	topologySHA, err := topologyDecisionDigest(topologyDecision)
	mustf(t, err, "topology digest: %v")
	envelope, err := buildBatchAcceptEvidenceEnvelope(job.ID, batchAcceptBoundDigests{
		RequestSHA256:     job.SubmitRequestSHA256,
		WorkloadSHA256:    workloadSHA,
		PlacementSHA256:   placementSHA,
		PricingSHA256:     pricingSHA,
		TopologySHA256:    topologySHA,
		ComputePlanSHA256: computeSHA,
	})
	mustf(t, err, "build envelope: %v")
	envelopeJSON, err := json.Marshal(envelope)
	mustf(t, err, "marshal envelope: %v")

	tx, err := pool.Begin(ctx)
	mustf(t, err, "begin: %v")
	// Intentionally do not commit — prove rolled-back accept leaves none.
	if err := insertEvidenceEnvelopeTx(ctx, tx, envelope, envelopeJSON); err != nil {
		_ = tx.Rollback(ctx)
		t.Fatalf("insert envelope in TX: %v", err)
	}
	var midCount int
	mustf(t, tx.QueryRow(ctx, `
		SELECT count(*) FROM evidence_envelopes WHERE subject_id=$1`, f.JobID,
	).Scan(&midCount), "count in TX: %v")
	if midCount != 1 {
		_ = tx.Rollback(ctx)
		t.Fatalf("envelope not visible inside TX: count=%d", midCount)
	}
	mustf(t, tx.Rollback(ctx), "rollback: %v")

	var afterCount int
	mustf(t, pool.QueryRow(ctx, `
		SELECT count(*) FROM evidence_envelopes WHERE subject_id=$1`, f.JobID,
	).Scan(&afterCount), "count after rollback: %v")
	if afterCount != 0 {
		t.Fatalf("rolled-back accept left %d evidence envelope rows", afterCount)
	}

	// Fail-closed SubmitJobTx also leaves none.
	tasks = tasks[:1]
	job.TaskCount = 1
	if err := store.SubmitJobTx(ctx, job, tasks); err == nil {
		t.Fatal("expected SubmitJobTx to fail closed")
	}
	mustf(t, pool.QueryRow(ctx, `
		SELECT count(*) FROM evidence_envelopes WHERE subject_id=$1`, f.JobID,
	).Scan(&afterCount), "count after failed submit: %v")
	if afterCount != 0 {
		t.Fatalf("failed accept left %d evidence envelope rows", afterCount)
	}
	if countJobRows(t, ctx, pool, f.JobID) != 0 {
		t.Fatal("failed accept left a job row")
	}
}

func TestSubmitJobTxFailedAfterEnvelopeWouldRollBackTogether(t *testing.T) {
	// Drive a real SubmitJobTx path that begins a TX and fails after the
	// jobs+envelope writes would have been prepared: bind a quote that
	// mismatches after digests are sealed but the failure is before commit.
	// bound_quote_compute_mismatch fails before jobs INSERT — still proves
	// no envelope. For a true post-insert rollback, corrupt the economic
	// plan insert by using an isolated helper TX with envelope + deliberate
	// error.
	ctx, _, pool, f, job, _, _ := currentUniformMoneyPathJob(t)
	workloadSHA, err := workloadDecisionDigest(job.WorkloadDecision)
	mustf(t, err, "workload: %v")
	computeSHA, err := computePlanDigest(job.ComputePlan)
	mustf(t, err, "compute: %v")
	placementSHA, err := placementRequirementDigest(job.PlacementRequirement)
	mustf(t, err, "placement: %v")
	pricingSHA, err := pricingDecisionDigest(job.PricingDecision)
	mustf(t, err, "pricing: %v")
	// Step 10 landed TopologyDecision after this fixture was written. Production
	// (store_jobs.go) freezes a topology digest in the accept transaction, so an
	// envelope built without one is now a broken link rather than an ABSENT one --
	// which is the envelope behaving correctly: an authority that exists cannot be
	// reported missing. The fixture, not the production path, was stale.
	topologyDecision, err := buildBatchTopologyDecision(job.WorkloadDecision)
	mustf(t, err, "topology decision: %v")
	topologySHA, err := topologyDecisionDigest(topologyDecision)
	mustf(t, err, "topology digest: %v")
	envelope, err := buildBatchAcceptEvidenceEnvelope(job.ID, batchAcceptBoundDigests{
		WorkloadSHA256:    workloadSHA,
		PlacementSHA256:   placementSHA,
		PricingSHA256:     pricingSHA,
		TopologySHA256:    topologySHA,
		ComputePlanSHA256: computeSHA,
	})
	mustf(t, err, "build: %v")

	tx, err := pool.Begin(ctx)
	mustf(t, err, "begin: %v")
	if err := insertEvidenceEnvelopeTx(ctx, tx, envelope, nil); err != nil {
		_ = tx.Rollback(ctx)
		t.Fatalf("insert: %v", err)
	}
	// Simulate a later accept-step failure: force rollback.
	mustf(t, tx.Rollback(ctx), "rollback after simulated failure: %v")
	var n int
	mustf(t, pool.QueryRow(ctx, `
		SELECT count(*) FROM evidence_envelopes WHERE subject_id=$1`, f.JobID,
	).Scan(&n), "count: %v")
	if n != 0 {
		t.Fatalf("error-aborted accept TX left %d envelopes", n)
	}
}
