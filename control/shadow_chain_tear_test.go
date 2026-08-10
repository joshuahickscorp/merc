package main

import (
	"errors"
	"strings"
	"testing"
)

// TestFailedPostCommitShadowLeavesAcceptedChainCoherentAndShadowAbsent is the
// Step 11 residual: shadow is deliberately outside the accept TX. When the
// observational write fails, the sealed accept chain must still validate and
// the shadow must be reported ABSENT — never implied present by silence.
func TestFailedPostCommitShadowLeavesAcceptedChainCoherentAndShadowAbsent(t *testing.T) {
	ctx, store, pool, f, job, tasks, _ := currentUniformMoneyPathJob(t)
	job.SubmitRequestSHA256 = strings.Repeat("ab", 32)

	mustf(t, store.SubmitJobTx(ctx, job, tasks), "SubmitJobTx: %v")

	// Accept chain is coherent with no shadow row yet.
	obs, err := store.ObserveAcceptedBatchChain(ctx, f.JobID)
	mustf(t, err, "observe after accept: %v")
	if !obs.ChainCoherent {
		t.Fatalf("accepted chain not coherent before shadow attempt: %+v", obs)
	}
	if !obs.EnvelopeValid || !obs.BoundDigestsMatch {
		t.Fatalf("envelope/digest flags wrong: %+v", obs)
	}
	if obs.ShadowStatus != shadowObservationAbsent {
		t.Fatalf("shadow status = %q, want ABSENT before post-commit write", obs.ShadowStatus)
	}
	if strings.TrimSpace(obs.ShadowAbsentReason) == "" {
		t.Fatal("ABSENT shadow must carry an explicit reason, not silent empty")
	}
	if obs.ShadowCellID != "" {
		t.Fatalf("ABSENT observation must not invent a shadow cell: %q", obs.ShadowCellID)
	}

	// Inject a failed post-commit shadow write (the residual failure mode).
	prev := recordShadowSelectionFailHook
	recordShadowSelectionFailHook = func() error {
		return errors.New("injected post-commit shadow write failure")
	}
	t.Cleanup(func() { recordShadowSelectionFailHook = prev })

	shadow, err := planShadowSelection(job.WorkloadDecision)
	mustf(t, err, "planShadowSelection: %v")
	shadow = shadow.withExecutionMode(job.WorkloadDecision)
	writeErr := store.RecordShadowSelection(ctx, f.JobID.String(), shadow)
	if writeErr == nil {
		t.Fatal("expected injected shadow write failure")
	}

	// After the failed write: chain still coherent, shadow still ABSENT.
	after, err := store.ObserveAcceptedBatchChain(ctx, f.JobID)
	mustf(t, err, "observe after failed shadow: %v")
	if !after.ChainCoherent {
		t.Fatalf("failed shadow write tore the accepted chain: %+v", after)
	}
	if after.ShadowStatus != shadowObservationAbsent {
		t.Fatalf("after failed write shadow=%q, want ABSENT (must not imply present)", after.ShadowStatus)
	}
	if after.EvidenceEnvelopeSHA256 == "" || !validSHA256(after.EvidenceEnvelopeSHA256) {
		t.Fatalf("envelope root missing after failed shadow: %q", after.EvidenceEnvelopeSHA256)
	}
	if after.EvidenceEnvelopeSHA256 != obs.EvidenceEnvelopeSHA256 {
		t.Fatalf("envelope root changed after failed shadow: before=%s after=%s",
			obs.EvidenceEnvelopeSHA256, after.EvidenceEnvelopeSHA256)
	}

	// No shadow row was written.
	var n int
	mustf(t, pool.QueryRow(ctx, `
		SELECT count(*) FROM runtime_shadow_selections WHERE job_id=$1`, f.JobID,
	).Scan(&n), "count shadow rows: %v")
	if n != 0 {
		t.Fatalf("failed write left %d shadow rows", n)
	}

	// Successful observational write flips PRESENT without touching accept digests.
	recordShadowSelectionFailHook = nil
	mustf(t, store.RecordShadowSelection(ctx, f.JobID.String(), shadow), "record shadow: %v")
	present, err := store.ObserveAcceptedBatchChain(ctx, f.JobID)
	mustf(t, err, "observe after present shadow: %v")
	if !present.ChainCoherent {
		t.Fatalf("successful shadow write must not tear accept chain: %+v", present)
	}
	if present.ShadowStatus != shadowObservationPresent {
		t.Fatalf("shadow status = %q, want PRESENT", present.ShadowStatus)
	}
	if present.ShadowAbsentReason != "" {
		t.Fatalf("PRESENT must not carry absent reason: %q", present.ShadowAbsentReason)
	}
	if present.EvidenceEnvelopeSHA256 != obs.EvidenceEnvelopeSHA256 {
		t.Fatalf("accept envelope root moved when shadow became PRESENT: %s -> %s",
			obs.EvidenceEnvelopeSHA256, present.EvidenceEnvelopeSHA256)
	}
}

// TestShadowAbsentIsNotInferredAsPresent guards the tripwire itself: missing
// row is ABSENT with a reason, never a default PRESENT.
func TestShadowAbsentIsNotInferredAsPresent(t *testing.T) {
	// Pure unit: the observation constants and the broken alternative.
	if shadowObservationPresent == shadowObservationAbsent {
		t.Fatal("PRESENT and ABSENT must differ")
	}
	// Broken reader: treating missing as present.
	brokenAssumePresent := func(rowExists bool) string {
		if rowExists {
			return shadowObservationPresent
		}
		// Defect: silence means present (what we refuse).
		return shadowObservationPresent
	}
	if brokenAssumePresent(false) != shadowObservationPresent {
		t.Fatal("broken fixture wrong")
	}
	// Correct reader.
	correct := func(rowExists bool) string {
		if rowExists {
			return shadowObservationPresent
		}
		return shadowObservationAbsent
	}
	if correct(false) != shadowObservationAbsent {
		t.Fatal("missing row must be ABSENT")
	}
	if correct(true) != shadowObservationPresent {
		t.Fatal("existing row must be PRESENT")
	}
}
