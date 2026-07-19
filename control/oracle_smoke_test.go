//go:build integration

package main

// oracle_smoke_test.go — self-validating proof that the differential capture harness
// (oracle_capture_test.go) works against the real schema: every capture query runs
// (so a wrong column name fails here, not silently in Checkpoint A), the normalization
// is deterministic (two captures of identical state are byte-equal), and the requested
// tables are all present. No committed golden required — the data-bearing goldens are
// captured per-reconstruction in Checkpoint A.

import (
	"bytes"
	"testing"
)

func TestOracleCaptureSmoke(t *testing.T) {
	reset(t)
	// A small embed job WITH redundancy + honeypot so tasks carries is_honeypot/
	// is_redundancy variety and the reserve/plan rows exist.
	_, taskCount := submitEmbedJob(t, 4, 0.5, 0.5, 0)
	if taskCount <= 0 {
		t.Fatalf("submitEmbedJob returned task_count=%d", taskCount)
	}

	domains := []string{"jobs", "tasks", "task_verdicts", "verification_events", "ledger", "reserve", "webhooks"}

	// Every capture query executes against the live schema — a mistyped column fails here.
	a := captureObservable(t, domains...)
	b := captureObservable(t, domains...)

	// Determinism: two captures of identical state must be byte-equal after normalization.
	if ja, jb := oracleCanonicalJSON(t, a), oracleCanonicalJSON(t, b); !bytes.Equal(ja, jb) {
		t.Fatalf("capture is not deterministic across identical state:\n--- a ---\n%s\n--- b ---\n%s", ja, jb)
	}

	// Structural: the submitted job is captured, and every projected table is keyed.
	jobs, ok := a["jobs"].([]any)
	if !ok || len(jobs) == 0 {
		t.Fatalf("expected >=1 job captured, got %#v", a["jobs"])
	}
	for _, tbl := range []string{
		"jobs", "tasks", "task_verdicts", "task_verdict_resolutions",
		"verification_events", "ledger_entries", "job_economic_plans",
		"job_economic_reserves", "webhooks",
	} {
		if _, ok := a[tbl]; !ok {
			t.Fatalf("capture missing table %q", tbl)
		}
	}

	// The plan + reserve rows must exist for the submitted job (proves the reserve
	// domain SQL projects real columns and the job created its economic envelope).
	if plans, _ := a["job_economic_plans"].([]any); len(plans) == 0 {
		t.Fatalf("expected a job_economic_plans row for the submitted job")
	}
}
