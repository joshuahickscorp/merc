package main

import (
	"context"
	"net/http/httptest"
	"os"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
)

// The three failure cases that a real agent process has to be present for.
//
// Simulating a dead worker by writing its lease columns proves the requeue
// query works and nothing about what an agent's absence actually does to the
// system: whether the worker is still advertised, whether the task is still
// claimable, whether anything was paid. These kill a real process, and one of
// them takes the runtime away underneath a live execution.
//
// Expensive, so there are three of them rather than seventeen. The cases that do
// not need a process — duplicate commit, digest mismatch, upload interruption,
// verifier restart, finalizer and settlement retry, cancellation, database
// restart — are in failure_matrix_test.go and run in seconds.

func requireAgentAndLlama(t *testing.T) string {
	t.Helper()
	agentBinaryPath(t)
	llamaURL := os.Getenv("MERC_LLAMA_EMBED_URL")
	if llamaURL == "" {
		t.Skip("MERC_LLAMA_EMBED_URL is not set; the llama.cpp agent has no engine to reach")
	}
	return llamaURL
}

// killAgent stops the process and waits for it, so the next assertion is not
// racing a still-running worker.
func killAgent(t *testing.T, agent *enrolledAgent) {
	t.Helper()
	if agent.cmd.Process == nil {
		t.Fatalf("%s: no process to kill", agent.name)
	}
	if err := agent.cmd.Process.Kill(); err != nil {
		t.Fatalf("%s: kill: %v", agent.name, err)
	}
	_, _ = agent.cmd.Process.Wait()
	t.Logf("%s: agent process killed", agent.name)
}

// submitDirectedEmbedJob puts one governed embedding job on the queue.
func submitDirectedEmbedJob(
	t *testing.T, ctx context.Context, store *Store, pool *pgxpool.Pool,
	artifacts *artifactHarness, cell string,
) (moneyPathFixture, uuid.UUID) {
	t.Helper()
	f := seedMoneyPathFixture(t, ctx, store, pool, moneyPathSeedOpts{TaskCount: 1})
	tasks := makeTasks(f, 1)
	f.TaskIDs = []uuid.UUID{tasks[0].ID}
	job := validJobRowDirected(t, f, tasks, cell)
	corpus := []byte(`{"id":"0","text":"the agent may not survive this"}` + "\n")
	for _, key := range []string{job.InputRef, tasks[0].InputRef} {
		if err := artifacts.storage.PutObject(ctx, key, corpus, "application/x-ndjson"); err != nil {
			t.Fatalf("upload %s: %v", key, err)
		}
	}
	if err := store.SubmitJobTx(ctx, job, tasks); err != nil {
		t.Fatalf("submit: %v", err)
	}
	return f, tasks[0].ID
}

// An agent that dies before claiming anything must leave the queue exactly as it
// found it. Nothing owed, nothing held, and the work still claimable by whoever
// comes next.
func TestFailureMatrixAgentDeathBeforeClaim(t *testing.T) {
	llamaURL := requireAgentAndLlama(t)
	t.Setenv("MERC_VERIFICATION_SAMPLE_SECRET",
		"failure-matrix-verification-sampling-secret-0123456789")
	artifacts := newArtifactHarness(t)
	ctx, store, pool := openIsolatedMoneyPathStore(t)
	verifier := NewVerifier(store).WithStorage(artifacts.storage)
	srv := httptest.NewServer(NewServer(store, artifacts.storage, verifier, nil).Routes())
	t.Cleanup(srv.Close)

	agent := launchAgent(t, ctx, store, pool, srv.URL, "llama_cpp", "llama_cpp_metal", llamaURL)
	waitForEnrolment(t, ctx, pool, agent)
	killAgent(t, agent)

	// The job arrives after the agent is gone, so there is no window in which it
	// could have claimed anything.
	f, taskID := submitDirectedEmbedJob(t, ctx, store, pool, artifacts, llamaEmbedCell)

	// Long enough that a live agent would certainly have polled.
	time.Sleep(5 * time.Second)

	assertFailureInvariants(t, ctx, pool, "agent death before claim", f.JobID, taskID,
		failureExpectation{
			MaxSupplierRows:       0,
			SupplierNetMustBeZero: true,
			BuyerCharges:          0,
			TerminalTaskStatuses:  []string{"queued"},
			MaxRetries:            0,
		})

	// And the work is still claimable — a dead agent must not have taken it with
	// it.
	var claimable bool
	if err := pool.QueryRow(ctx, `
		SELECT status='queued' AND claimed_by IS NULL
		       AND COALESCE(visible_at, created_at) <= now()
		  FROM tasks WHERE id=$1`, taskID).Scan(&claimable); err != nil {
		t.Fatal(err)
	}
	if !claimable {
		t.Error("the task is not claimable after the only agent died before claiming it")
	}
}

// An agent that dies HOLDING a claim is the case that can strand work forever.
//
// The lease must expire, the task must return to the queue exactly once per
// death, and nothing may have been paid for an execution that produced no
// artifact.
func TestFailureMatrixAgentDeathAfterClaim(t *testing.T) {
	llamaURL := requireAgentAndLlama(t)
	t.Setenv("MERC_VERIFICATION_SAMPLE_SECRET",
		"failure-matrix-verification-sampling-secret-0123456789")
	artifacts := newArtifactHarness(t)
	ctx, store, pool := openIsolatedMoneyPathStore(t)
	verifier := NewVerifier(store).WithStorage(artifacts.storage)
	srv := httptest.NewServer(NewServer(store, artifacts.storage, verifier, nil).Routes())
	t.Cleanup(srv.Close)

	agent := launchAgent(t, ctx, store, pool, srv.URL, "llama_cpp", "llama_cpp_metal", llamaURL)
	waitForEnrolment(t, ctx, pool, agent)
	f, taskID := submitDirectedEmbedJob(t, ctx, store, pool, artifacts, llamaEmbedCell)

	// Wait for the CLAIM, not for the commit: killing after the commit would test
	// a different thing.
	claimed := false
	deadline := time.Now().Add(120 * time.Second)
	for time.Now().Before(deadline) {
		var status string
		var holder *uuid.UUID
		if err := pool.QueryRow(ctx,
			`SELECT status, claimed_by FROM tasks WHERE id=$1`, taskID).
			Scan(&status, &holder); err != nil {
			t.Fatal(err)
		}
		if holder != nil && status == "running" {
			claimed = true
			t.Logf("agent claimed the task (holder=%s)", *holder)
			break
		}
		if status == "verifying" || status == "complete" {
			t.Skip("the agent finished before it could be killed mid-claim; " +
				"this host is too fast for this case to be driven by timing")
		}
		time.Sleep(100 * time.Millisecond)
	}
	if !claimed {
		t.Fatal("the agent never claimed the task")
	}
	killAgent(t, agent)

	// Re-check AFTER the kill, not only before it.
	//
	// The skip above closes the window up to the last poll; it does not close the
	// window between that poll and the signal actually landing. On this host the
	// embed cell measures ~1,950 embeddings/sec, so a three-record task can be
	// claimed, executed, uploaded and committed inside one 100ms poll interval —
	// and the test then asserts "the task returned to the queue" against a task
	// that legitimately finished. That is a flake, not a defect, and the case it
	// exists for (death while HOLDING the claim) simply did not occur.
	var afterKill string
	if err := pool.QueryRow(ctx,
		`SELECT status FROM tasks WHERE id=$1`, taskID).Scan(&afterKill); err != nil {
		t.Fatal(err)
	}
	if afterKill != "running" {
		t.Skipf("the agent committed (%s) between the claim observation and the kill; "+
			"this host is too fast for this case to be driven by timing", afterKill)
	}

	// The stale-task sweep is what production runs; call the same function.
	if err := store.RequeueStaleTask(ctx, taskID, 0); err != nil {
		t.Fatalf("requeue after agent death: %v", err)
	}
	assertFailureInvariants(t, ctx, pool, "agent death after claim", f.JobID, taskID,
		failureExpectation{
			MaxSupplierRows:       0,
			SupplierNetMustBeZero: true,
			BuyerCharges:          0,
			TerminalTaskStatuses:  []string{"queued"},
			MaxRetries:            1,
		})

	// Exactly once per death: a second sweep against an unclaimed task must not
	// keep incrementing the retry count, or a supervisor loop would exhaust the
	// task's retries without anything having failed.
	if err := store.RequeueStaleTask(ctx, taskID, 0); err != nil {
		t.Fatalf("second sweep: %v", err)
	}
	var retries int
	if err := pool.QueryRow(ctx, `SELECT retry_count FROM tasks WHERE id=$1`, taskID).
		Scan(&retries); err != nil {
		t.Fatal(err)
	}
	if retries != 1 {
		t.Errorf("retry_count is %d after one death and two sweeps, want 1", retries)
	}
}

// The runtime is unreachable. The agent must fail the task loudly rather than
// commit whatever it can produce without it.
//
// This is the case that decides whether a second runtime is safe to route to at
// all: an agent that quietly degrades to some other path when llama-server is
// down would be selling a llama.cpp execution and delivering something else.
func TestFailureMatrixRuntimeUnavailable(t *testing.T) {
	agentBinaryPath(t)
	if os.Getenv("MERC_LLAMA_EMBED_URL") == "" {
		t.Skip("MERC_LLAMA_EMBED_URL is not set; this host cannot run the llama.cpp agent at all")
	}
	t.Setenv("MERC_VERIFICATION_SAMPLE_SECRET",
		"failure-matrix-verification-sampling-secret-0123456789")
	artifacts := newArtifactHarness(t)
	ctx, store, pool := openIsolatedMoneyPathStore(t)
	verifier := NewVerifier(store).WithStorage(artifacts.storage)
	srv := httptest.NewServer(NewServer(store, artifacts.storage, verifier, nil).Routes())
	t.Cleanup(srv.Close)

	// Seed and submit FIRST, on the store's own context. The agent phase then
	// runs on a context of its own: an agent spends about forty seconds
	// benchmarking both retained models before it registers, and doing that
	// inside the store context left no time to create a buyer afterwards.
	f, taskID := submitDirectedEmbedJob(t, ctx, store, pool, artifacts, llamaEmbedCell)

	agentCtx, cancel := context.WithTimeout(context.Background(), 240*time.Second)
	t.Cleanup(cancel)

	// A port nothing is listening on. Taking down the shared llama-server would
	// break every other test in the package for the sake of one case.
	const dead = "http://127.0.0.1:1"
	agent := launchAgent(t, agentCtx, store, pool, srv.URL, "llama_cpp", "llama_cpp_metal", dead)

	// The agent may refuse to enrol at all, and that is a legitimate outcome — a
	// worker that cannot reach its runtime should not be advertised. What must
	// never happen is a committed result.
	var status string
	var failureClass *string
	deadline := time.Now().Add(150 * time.Second)
	for time.Now().Before(deadline) {
		if err := pool.QueryRow(agentCtx, `
			SELECT status,
			       (SELECT failure_class FROM task_failures
			         WHERE task_id=$1 ORDER BY created_at DESC LIMIT 1)
			  FROM tasks WHERE id=$1`, taskID).Scan(&status, &failureClass); err != nil {
			t.Fatal(err)
		}
		if failureClass != nil || status == "retrying" || status == "failed" {
			break
		}
		if status == "verifying" || status == "complete" {
			t.Fatalf("the agent committed a result with its runtime unreachable "+
				"(status=%s); a llama.cpp execution was sold and something else delivered",
				status)
		}
		time.Sleep(500 * time.Millisecond)
	}
	// Silence is not a result. Either the agent refused to advertise itself —
	// which is the correct response to a runtime it cannot reach — or it enrolled
	// and then failed the task. An agent that enrolled and left the work sitting
	// is a stuck fleet, and accepting "nothing happened" would call that a pass.
	// Scoped to THIS agent's worker. The money-path fixture seeds its own workers
	// with capabilities, so an unscoped count reports the fixture's rows and
	// accuses the agent of advertising something it never did.
	var advertised int
	if err := pool.QueryRow(agentCtx,
		`SELECT COUNT(*) FROM worker_authorized_capabilities WHERE worker_id=$1`,
		agent.workerID).Scan(&advertised); err != nil {
		t.Fatal(err)
	}
	switch {
	case failureClass != nil:
		t.Logf("agent enrolled and failed the task with class %q", *failureClass)
	case advertised == 0:
		t.Logf("agent refused to advertise itself with its runtime unreachable; "+
			"task is %q and claimable by a healthy worker", status)
	default:
		t.Fatalf("the agent advertised %d capability set(s) with its runtime "+
			"unreachable and then left the task %q; a buyer's work would sit on a "+
			"worker that cannot execute it", advertised, status)
	}

	// Whatever happened, the supplier was not paid and the buyer was not charged
	// for an execution that never reached a runtime.
	var supplierNet float64
	var buyerCharges int
	if err := pool.QueryRow(agentCtx, `
		SELECT (SELECT COALESCE(SUM(amount_usd),0)::float8 FROM ledger_entries
		         WHERE task_id=$1 AND supplier_id IS NOT NULL),
		       (SELECT COUNT(*) FROM ledger_entries le JOIN tasks t ON t.id=le.task_id
		         WHERE t.job_id=$2 AND le.kind='buyer_charge')`,
		taskID, f.JobID).Scan(&supplierNet, &buyerCharges); err != nil {
		t.Fatal(err)
	}
	if supplierNet > 1e-9 {
		t.Errorf("%.9f USD stands for an execution whose runtime was unreachable", supplierNet)
	}
	if buyerCharges != 0 {
		t.Errorf("%d buyer_charge rows for an execution that never reached a runtime", buyerCharges)
	}
}
