package main

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	"net/http/httptest"
)

// logTaskStates prints every task on a job with the fields that decide whether a
// worker can claim it. A task that is never claimed looks identical to one that
// was never created unless the state is actually read.
func logTaskStates(
	t *testing.T, ctx context.Context, pool *pgxpool.Pool, jobID uuid.UUID, when string,
) {
	t.Helper()
	rows, err := pool.Query(ctx, `
		SELECT id, status, is_honeypot, chunk_index,
		       claimed_by IS NOT NULL, COALESCE(result_key,''),
		       COALESCE(visible_at, created_at) <= now()
		  FROM tasks WHERE job_id=$1 ORDER BY chunk_index`, jobID)
	mustf(t, err, "%s: read task states: %v", when)
	defer rows.Close()
	for rows.Next() {
		var id uuid.UUID
		var status string
		var honeypot, claimed, visible bool
		var chunk int
		var resultKey string
		must(t, rows.Scan(&id, &status, &honeypot, &chunk, &claimed, &resultKey, &visible))
		t.Logf("%s: chunk=%d honeypot=%v status=%s claimed=%v visible=%v result=%v",
			when, chunk, honeypot, status, claimed, visible, resultKey != "")
	}
}

// A rejected execution must not leave the supplier paid.
//
// The governed reference is the approved answer for DIFFERENT text, so the
// agent's honestly-produced embedding is graded against something it should not
// match. Forcing the rejection this way rather than by corrupting the artifact
// matters: the agent does everything right, so what is under test is the grading
// and its economic consequence, not the agent's error handling.
//
// A honeypot task must be declared in the compute plan — SubmitJobTx refuses a
// task set whose class counts do not match, because a honeypot the plan never
// priced would be unpaid work — so this submits a primary and a honeypot
// together.
func TestRejectedVerificationLeavesNoSupplierCredit(t *testing.T) {
	agentBinaryPath(t)
	llamaURL := os.Getenv("MERC_LLAMA_EMBED_URL")
	if llamaURL == "" {
		t.Skip("MERC_LLAMA_EMBED_URL is not set; the llama.cpp agent has no engine to reach")
	}
	t.Setenv("MERC_VERIFICATION_SAMPLE_SECRET",
		"rejection-economics-verification-sampling-secret-01234")
	artifacts := newArtifactHarness(t)

	ctx, store, pool := openIsolatedMoneyPathStore(t)
	verifier := NewVerifier(store).WithStorage(artifacts.storage)
	srv := httptest.NewServer(NewServer(store, artifacts.storage, verifier, nil).Routes())
	t.Cleanup(srv.Close)

	llama := launchAgent(t, ctx, store, pool, srv.URL, "llama_cpp", "llama_cpp_metal", llamaURL)
	waitForEnrolment(t, ctx, pool, llama)

	jobCtx, cancel := context.WithTimeout(context.Background(), 400*time.Second)
	t.Cleanup(cancel)

	// A real, well-formed embedding artifact — for other text.
	reference, err := os.ReadFile(
		filepath.Join("..", "evidence", "chain", "embed-candle_metal.json"))
	if err != nil {
		t.Skipf("no governed reference artifact: %v", err)
	}
	var decoded struct {
		Dim     int         `json:"dim"`
		Vectors [][]float64 `json:"vectors"`
	}
	must(t, json.Unmarshal(reference, &decoded))
	// One row, so reference and observation share cardinality and the rejection is
	// about DIRECTION rather than shape. A row-count mismatch would be caught by a
	// structural check and would prove nothing about the cosine floor.
	oneRow := embedArtifact(t, decoded.Dim, decoded.Vectors[:1])

	f := seedMoneyPathFixture(t, jobCtx, store, pool, moneyPathSeedOpts{TaskCount: 2})
	tasks := makeTasks(f, 2)
	f.TaskIDs = []uuid.UUID{tasks[0].ID, tasks[1].ID}
	tasks[1].IsHoneypot = true
	job := validJobRowClasses(t, f, tasks, llamaEmbedCell, 1, 0, 1)

	corpus := []byte(`{"id":"0","text":"Entirely unrelated text the reference never saw."}` + "\n")
	for _, key := range []string{job.InputRef, tasks[0].InputRef, tasks[1].InputRef} {
		if err := artifacts.storage.PutObject(
			jobCtx, key, corpus, "application/x-ndjson"); err != nil {
			t.Fatalf("upload %s: %v", key, err)
		}
	}
	mustf(t, store.InsertHoneypot(jobCtx, "embed", tasks[1].InputRef, oneRow, ""), "seed mismatched governed reference: %v")
	mustf(t, store.SubmitJobTx(jobCtx, job, tasks), "submit: %v")
	logTaskStates(t, jobCtx, pool, f.JobID, "after submit")

	// Wait for the HONEYPOT to reach a terminal verification outcome. Polling both
	// task states on the way, because "did not commit" is not a diagnosis.
	deadline := time.Now().Add(240 * time.Second)
	var outcome string
	for time.Now().Before(deadline) {
		var got *string
		if err := pool.QueryRow(jobCtx, `
			SELECT terminal_outcome FROM verification_work
			 WHERE task_id=$1 AND attempt=0`, tasks[1].ID).Scan(&got); err == nil {
			if got != nil && *got != "" {
				outcome = *got
				break
			}
		}
		time.Sleep(time.Second)
	}
	logTaskStates(t, jobCtx, pool, f.JobID, "after wait")
	if outcome == "" {
		t.Fatal("the honeypot never reached a verification outcome")
	}
	t.Logf("honeypot verification outcome: %q", outcome)
	if outcome == "pass" {
		t.Fatal("an embedding of unrelated text passed against the governed reference")
	}

	// The governed consequence of a failed honeypot, as the policy actually
	// defines it: the credit is clawed back, reputation is docked, and the task is
	// requeued. Quarantine is NOT it — that is reserved for an answer-class
	// mismatch, where the supplier's engine identity does not match what it
	// claimed, which is a different accusation from getting an answer wrong.
	//
	// "Pays nobody" therefore means the credit does not STAND. It is written on
	// commit and the grade arrives afterwards, so asserting it never existed would
	// be asserting a different settlement model than the one Merc runs.
	var netSupplierUSD float64
	var creditRows int
	if err := pool.QueryRow(jobCtx, `
		SELECT COUNT(*), COALESCE(SUM(amount_usd),0) FROM ledger_entries
		 WHERE task_id=$1 AND supplier_id IS NOT NULL`, tasks[1].ID).
		Scan(&creditRows, &netSupplierUSD); err != nil {
		t.Fatal(err)
	}
	var reputation float64
	var quarantined bool
	if err := pool.QueryRow(jobCtx,
		`SELECT COALESCE(reputation,0), status='quarantined' FROM suppliers WHERE id=$1`,
		llama.supplierID).Scan(&reputation, &quarantined); err != nil {
		t.Fatal(err)
	}
	var events int
	if err := pool.QueryRow(jobCtx, `
		SELECT COUNT(*) FROM verification_events
		 WHERE task_id=$1 AND kind='honeypot_fail'`, tasks[1].ID).Scan(&events); err != nil {
		t.Fatal(err)
	}
	t.Logf("after rejection: %d supplier rows netting %.9f USD, reputation=%.3f, "+
		"quarantined=%v, honeypot_fail events=%d",
		creditRows, netSupplierUSD, reputation, quarantined, events)

	// Nothing stands to the supplier. More than one row is expected and correct —
	// the credit and its clawback — so the test is on the NET, not the count.
	if netSupplierUSD > 0 {
		t.Errorf("rejected work left %.9f USD standing to the supplier", netSupplierUSD)
	}
	// And Merc keeps no success margin on work it refused.
	var platformUSD float64
	if err := pool.QueryRow(jobCtx, `
		SELECT COALESCE(SUM(amount_usd),0) FROM ledger_entries
		 WHERE task_id=$1 AND kind='platform_take'`, tasks[1].ID).Scan(&platformUSD); err != nil {
		t.Fatal(err)
	}
	if platformUSD > 0 {
		t.Errorf("Merc retained %.9f USD of platform take on rejected work", platformUSD)
	}
	// The rejection is recorded as a fact, not merely acted upon: a clawback with
	// no event behind it would be unauditable.
	if events == 0 {
		t.Error("a failed honeypot recorded no honeypot_fail verification event")
	}
	if reputation >= 0.95 {
		t.Errorf("reputation is %.3f; a failed honeypot did not dock it from 0.95",
			reputation)
	}
}
