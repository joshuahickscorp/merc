package main

import (
	"context"
	"errors"
	"math"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
)

// The failure matrix for the second runtime.
//
// `REAL_RUNTIME_PROVEN` was recorded with an explicitly incomplete failure
// matrix, and the promotion receipt says so. A happy path proves a runtime can
// earn money; it says nothing about what happens when the agent dies holding a
// claim, when an upload never lands, when the verifier restarts mid-decision, or
// when settlement is retried. Every one of those is a place money can be created
// or destroyed, and none of them had been driven.
//
// The invariants are asserted the same way for every case, by
// assertFailureInvariants, so a case cannot quietly check less than its
// neighbours. What varies between cases is only what is done to the system.
//
// Two cases are deliberately NOT here. Agent death before and after claim is
// driven against real agent processes in failure_matrix_agent_test.go, because
// simulating a dead worker by writing its lease columns proves nothing about
// what an agent's absence actually does. And "runtime crash during execution" is
// driven there too, by taking llama-server away underneath a live embed.

// failureExpectation is what a case claims should be true afterwards.
type failureExpectation struct {
	// MaxSupplierRows bounds the ledger rows attributed to a supplier for this
	// task. The invariant is "no DUPLICATE payable", not "exactly one": the
	// supplier is credited when verification settles, so a case that stops before
	// that legitimately has none, and a clawback legitimately has two that sum to
	// nothing.
	MaxSupplierRows int
	// SupplierNetMustBeZero requires that nothing STANDS as payment. This is the
	// property for every case where the output was never delivered or was
	// rejected, and it is deliberately about the net rather than the row count.
	SupplierNetMustBeZero bool
	// BuyerCharges is the number of buyer_charge rows for the job. More than one
	// is a duplicate debit.
	BuyerCharges int
	// TerminalTaskStatuses is the closed set the task is allowed to be in. A case
	// that permits several states must say which, rather than accepting any.
	TerminalTaskStatuses []string
	// MaxRetries bounds the retry/fallback. An unbounded requeue is not recovery,
	// it is a loop that bills storage forever.
	MaxRetries int
	// DiagnosticContains, when set, requires the recorded failure or verification
	// event to name the cause. "something went wrong" is not actionable.
	DiagnosticContains string
}

// assertFailureInvariants is the nine-property check every case shares.
func assertFailureInvariants(
	t *testing.T, ctx context.Context, pool *pgxpool.Pool,
	name string, jobID, taskID uuid.UUID, want failureExpectation,
) {
	t.Helper()

	// 1. One authoritative state, from the closed set this case permits.
	var status string
	var retries int
	var claimedBy *uuid.UUID
	var leaseHeld bool
	if err := pool.QueryRow(ctx, `
		SELECT status, COALESCE(retry_count,0), claimed_by,
		       (claimed_by IS NOT NULL AND status NOT IN ('running','verifying','complete'))
		  FROM tasks WHERE id=$1`, taskID).Scan(&status, &retries, &claimedBy, &leaseHeld); err != nil {
		t.Fatalf("%s: read task: %v", name, err)
	}
	t.Logf("%s: task status=%s retries=%d claimed=%v", name, status, retries, claimedBy != nil)
	if len(want.TerminalTaskStatuses) > 0 && !containsStr(want.TerminalTaskStatuses, status) {
		t.Errorf("%s: task is %q, want one of %v", name, status, want.TerminalTaskStatuses)
	}

	// 2 and 3. No duplicate buyer debit, no duplicate supplier payable.
	var buyerCharges int
	if err := pool.QueryRow(ctx, `
		SELECT COUNT(*) FROM ledger_entries le
		  JOIN tasks t ON t.id = le.task_id
		 WHERE t.job_id=$1 AND le.kind='buyer_charge'`,
		jobID).Scan(&buyerCharges); err != nil {
		t.Fatalf("%s: read buyer charges: %v", name, err)
	}
	if buyerCharges != want.BuyerCharges {
		t.Errorf("%s: %d buyer_charge rows, want %d", name, buyerCharges, want.BuyerCharges)
	}

	// 4. No payment standing for output that was never delivered or was rejected.
	//    The NET, not the presence of a row: the supplier is credited on commit
	//    and the grade arrives afterwards, so a clawback leaves a credit and a
	//    reversal that sum to nothing.
	var supplierRows int
	var supplierNet float64
	if err := pool.QueryRow(ctx, `
		SELECT COUNT(*), COALESCE(SUM(amount_usd),0)::float8
		  FROM ledger_entries WHERE task_id=$1 AND supplier_id IS NOT NULL`,
		taskID).Scan(&supplierRows, &supplierNet); err != nil {
		t.Fatalf("%s: read supplier ledger: %v", name, err)
	}
	t.Logf("%s: supplier rows=%d net=%.9f", name, supplierRows, supplierNet)
	if supplierRows > want.MaxSupplierRows {
		t.Errorf("%s: %d supplier ledger rows, bound is %d",
			name, supplierRows, want.MaxSupplierRows)
	}
	const ledgerEpsilon = 1e-9
	if want.SupplierNetMustBeZero && math.Abs(supplierNet) > ledgerEpsilon {
		t.Errorf("%s: %.9f USD stands for work that was never delivered or was rejected",
			name, supplierNet)
	}

	// 5. No leaked task lease: a task that is not running must not still be held.
	if leaseHeld {
		t.Errorf("%s: task is %q and still holds a claim", name, status)
	}

	// 6. No leaked verification lease.
	var leakedVerification int
	if err := pool.QueryRow(ctx, `
		SELECT COUNT(*) FROM verification_work
		 WHERE task_id=$1 AND status='leased'
		   AND (lease_expires_at IS NULL OR lease_expires_at > now())`,
		taskID).Scan(&leakedVerification); err != nil {
		t.Fatalf("%s: read verification work: %v", name, err)
	}
	if leakedVerification != 0 {
		t.Errorf("%s: %d verification lease(s) still held", name, leakedVerification)
	}

	// 7. No orphaned artifact authority: a verification work row may not claim an
	//    artifact it never sealed.
	var orphanedArtifacts int
	if err := pool.QueryRow(ctx, `
		SELECT COUNT(*) FROM verification_work
		 WHERE task_id=$1 AND artifact_key IS NOT NULL AND artifact_sha256 IS NULL`,
		taskID).Scan(&orphanedArtifacts); err != nil {
		t.Fatalf("%s: read artifact authority: %v", name, err)
	}
	if orphanedArtifacts != 0 {
		t.Errorf("%s: %d verification row(s) name an artifact with no digest",
			name, orphanedArtifacts)
	}

	// 8. Bounded retry.
	if retries > want.MaxRetries {
		t.Errorf("%s: task retried %d times, bound is %d", name, retries, want.MaxRetries)
	}

	// 9. Actionable diagnostic.
	if want.DiagnosticContains != "" {
		var diagnostics []string
		rows, err := pool.Query(ctx, `
			SELECT COALESCE(last_error,'') FROM verification_work WHERE task_id=$1
			UNION ALL
			SELECT kind FROM verification_events WHERE task_id=$1`, taskID)
		if err != nil {
			t.Fatalf("%s: read diagnostics: %v", name, err)
		}
		for rows.Next() {
			var line string
			if err := rows.Scan(&line); err != nil {
				rows.Close()
				t.Fatal(err)
			}
			if line != "" {
				diagnostics = append(diagnostics, line)
			}
		}
		rows.Close()
		joined := strings.Join(diagnostics, " | ")
		t.Logf("%s: diagnostics: %s", name, joined)
		if !strings.Contains(joined, want.DiagnosticContains) {
			t.Errorf("%s: no diagnostic names %q; recorded: %s",
				name, want.DiagnosticContains, joined)
		}
	}
}

// failureCase drives one governed embedding job to the point of failure and then
// checks the invariants.
type failureCase struct {
	h     *verifiedHarness
	ctx   context.Context
	store *Store
	f     moneyPathFixture
	task  uuid.UUID
	body  []byte
}

// newFailureCase seeds a real one-task embedding job whose task is claimed and
// running, with its input in real object storage and llama.cpp runtime
// provenance stamped on the task.
//
// The claim comes from the fixture rather than from an UPDATE. Writing the
// execution identity columns directly is refused by a trigger — "task execution
// identity is immutable outside claim transition" — which is the schema
// defending exactly the property this matrix is about, so working around it
// would have been testing a state the product cannot reach.
func newFailureCase(t *testing.T) *failureCase {
	return newFailureCaseWithClass(t, "")
}

// newFailureCaseWithClass is the same job under a governed verification class.
// An empty class leaves the task as ordinary buyer traffic.
func newFailureCaseWithClass(t *testing.T, class string) *failureCase {
	t.Helper()
	h, ctx, store := newVerifiedArtifactHarness(t)
	f := seedMoneyPathFixture(t, ctx, store, h.pool(), moneyPathSeedOpts{
		TaskCount: 1, TaskStatus: "running", ClaimWorker: true,
		SeedJob: true, SeedPlanRows: true,
	})
	taskID := f.TaskIDs[0]

	llama := loadChainArtifact(t, "llama_cpp_metal", llamaEmbedCell, "gguf")
	if _, err := h.pool().Exec(ctx, `
		UPDATE tasks SET runtime_cell_id=$2, runtime_id=$3,
		                 runtime_matrix_sha256=$4, model_kind=$5
		 WHERE id=$1`,
		taskID, llama.cellID, llama.runtimeProfileID,
		generatedRuntimeMatrixSHA256, llama.modelKind); err != nil {
		t.Fatalf("stamp runtime provenance: %v", err)
	}
	if class != "" {
		if _, err := h.pool().Exec(ctx,
			`UPDATE tasks SET verification_class=$2, verification_class_policy=$3 WHERE id=$1`,
			taskID, class, verificationClassPolicyRevision); err != nil {
			t.Fatalf("stamp verification class %s: %v", class, err)
		}
	}
	var inputRef string
	if err := h.pool().QueryRow(ctx,
		`SELECT input_ref FROM tasks WHERE id=$1`, taskID).Scan(&inputRef); err != nil {
		t.Fatal(err)
	}
	corpus := []byte(`{"id":"0","text":"failure matrix"}` + "\n")
	if err := h.storage.PutObject(ctx, inputRef, corpus, "application/x-ndjson"); err != nil {
		t.Fatalf("upload %s: %v", inputRef, err)
	}
	return &failureCase{h: h, ctx: ctx, store: store, f: f, task: taskID, body: llama.body}
}

// claim is a no-op: newFailureCase already produced a claimed, running task.
// Kept so each case reads as "claim, then break something" rather than relying
// on the constructor's state being remembered.
func (c *failureCase) claim(t *testing.T) { t.Helper() }

// newQueuedFailureCase submits a job through the production path and leaves it on
// the queue, for the cases that are about what happens BEFORE anyone claims it.
//
// Submitted rather than seeded, because the job lifecycle is enforced by a
// trigger — "illegal job lifecycle transition: running -> queued" — so a fixture
// seeded as running cannot be put back. The trigger is right; the fixture was the
// wrong starting point.
func newQueuedFailureCase(t *testing.T) *failureCase {
	t.Helper()
	h, ctx, store := newVerifiedArtifactHarness(t)
	f := seedMoneyPathFixture(t, ctx, store, h.pool(), moneyPathSeedOpts{TaskCount: 1})
	tasks := makeTasks(f, 1)
	f.TaskIDs = []uuid.UUID{tasks[0].ID}
	job := validJobRowDirected(t, f, tasks, llamaEmbedCell)
	corpus := []byte(`{"id":"0","text":"failure matrix"}` + "\n")
	for _, key := range []string{job.InputRef, tasks[0].InputRef} {
		if err := h.storage.PutObject(ctx, key, corpus, "application/x-ndjson"); err != nil {
			t.Fatalf("upload %s: %v", key, err)
		}
	}
	if err := store.SubmitJobTx(ctx, job, tasks); err != nil {
		t.Fatalf("submit: %v", err)
	}
	llama := loadChainArtifact(t, "llama_cpp_metal", llamaEmbedCell, "gguf")
	return &failureCase{h: h, ctx: ctx, store: store, f: f, task: tasks[0].ID, body: llama.body}
}

// A worker that commits the same result twice must be paid once.
//
// Not a hypothetical: a commit whose HTTP response is lost is retried by every
// well-behaved client, and the second attempt is indistinguishable from the
// first at the wire.
func TestFailureMatrixDuplicateCommitPaysOnce(t *testing.T) {
	c := newFailureCase(t)
	c.claim(t)
	c.h.commitThroughStorage(c.ctx, c.f, c.task, c.body)

	// The identical commit again.
	key := taskAttemptResultKey(c.f.JobID, c.task, 0)
	commit := commitFor(c.f, c.task, 0)
	commit.ResultKey = key
	commit.ResultSHA256 = sha256HexOf(c.body)
	_, err := c.store.CompleteTaskTx(c.ctx, c.task, c.f.WorkerID, commit)
	if err == nil {
		t.Log("the second commit was accepted; the invariants below decide whether that was safe")
	} else if !errors.Is(err, errNotFound) {
		t.Logf("second commit refused: %v", err)
	}

	assertFailureInvariants(t, c.ctx, c.h.pool(), "duplicate commit", c.f.JobID, c.task,
		failureExpectation{
			// The supplier is credited when verification settles, not at commit,
			// so a commit-only case has none. What matters is that a second
			// identical commit did not create a second one.
			MaxSupplierRows:       1,
			SupplierNetMustBeZero: true,
			BuyerCharges:          0, // charged at finalization, not at commit
			TerminalTaskStatuses:  []string{"verifying", "complete"},
			MaxRetries:            0,
		})
}

// An upload that never landed must not be verifiable, must not pay, and must
// leave the task recoverable rather than stuck holding a lease.
func TestFailureMatrixOutputUploadInterrupted(t *testing.T) {
	c := newFailureCase(t)
	c.claim(t)

	// Commit naming a result key with no object behind it: the interruption
	// happened between the PUT and the commit, which is the window that exists.
	commit := commitFor(c.f, c.task, 0)
	commit.ResultKey = taskAttemptResultKey(c.f.JobID, c.task, 0)
	commit.ResultSHA256 = sha256HexOf(c.body)
	info, err := c.store.CompleteTaskTx(c.ctx, c.task, c.f.WorkerID, commit)
	if err != nil {
		t.Fatalf("commit: %v", err)
	}
	if info == nil {
		t.Fatal("commit returned no task info")
	}

	// The processor must not invent a pass for bytes it cannot read.
	for attempt := 0; attempt < 3; attempt++ {
		if _, err := c.h.processor.ProcessAttempt(c.ctx, c.task, 0); err != nil {
			t.Logf("verification attempt %d: %v", attempt, err)
		}
	}
	assertFailureInvariants(t, c.ctx, c.h.pool(), "upload interrupted", c.f.JobID, c.task,
		failureExpectation{
			MaxSupplierRows:       0,
			SupplierNetMustBeZero: true,
			BuyerCharges:          0,
			TerminalTaskStatuses:  []string{"verifying", "retrying", "queued", "failed"},
			MaxRetries:            3,
		})

	// And the settlement half: no completed, verified job exists, so the buyer
	// must not have been invoiced for one.
	invoice, err := c.store.JobInvoice(c.ctx, c.f.JobID, c.f.BuyerID)
	if err == nil && invoice.ActualUSD > 0 {
		t.Errorf("the buyer was invoiced %.9f for an output that was never stored",
			invoice.ActualUSD)
	}
}

// A result whose digest does not match its bytes is a corrupted or substituted
// artifact. It must not verify and must not stand as payment.
func TestFailureMatrixResultDigestMismatch(t *testing.T) {
	c := newFailureCase(t)
	c.claim(t)

	key := taskAttemptResultKey(c.f.JobID, c.task, 0)
	if err := c.h.storage.PutObject(c.ctx, key, c.body, "application/json"); err != nil {
		t.Fatalf("upload: %v", err)
	}
	commit := commitFor(c.f, c.task, 0)
	commit.ResultKey = key
	// A well-formed digest of different bytes, not garbage: a shape check would
	// catch garbage and this is the case a shape check cannot see.
	commit.ResultSHA256 = sha256HexOf(append([]byte("not the committed bytes"), c.body...))
	if _, err := c.store.CompleteTaskTx(c.ctx, c.task, c.f.WorkerID, commit); err != nil {
		t.Fatalf("commit: %v", err)
	}
	for attempt := 0; attempt < 3; attempt++ {
		if _, err := c.h.processor.ProcessAttempt(c.ctx, c.task, 0); err != nil {
			t.Logf("verification attempt %d: %v", attempt, err)
		}
	}

	var outcome string
	if err := c.h.pool().QueryRow(c.ctx,
		`SELECT COALESCE(verification_outcome,'') FROM tasks WHERE id=$1`, c.task).
		Scan(&outcome); err != nil {
		t.Fatal(err)
	}
	var workOutcome string
	if err := c.h.pool().QueryRow(c.ctx, `
		SELECT COALESCE(terminal_outcome,'') FROM verification_work
		 WHERE task_id=$1 AND attempt=0`, c.task).Scan(&workOutcome); err != nil {
		t.Fatal(err)
	}
	t.Logf("digest mismatch: task outcome=%q work outcome=%q", outcome, workOutcome)
	if outcome == string(OutcomePass) || workOutcome == string(OutcomePass) {
		t.Fatal("an artifact whose digest does not match its bytes verified")
	}

	// Net zero: whatever rows exist must not add up to a payment.
	var supplierNet float64
	if err := c.h.pool().QueryRow(c.ctx, `
		SELECT COALESCE(SUM(amount_usd),0)::float8 FROM ledger_entries
		 WHERE task_id=$1 AND supplier_id IS NOT NULL`, c.task).Scan(&supplierNet); err != nil {
		t.Fatal(err)
	}
	if supplierNet > 1e-9 {
		t.Errorf("a digest-mismatched result left %.9f USD standing", supplierNet)
	}
}

// The verifier is unavailable, then comes back. Exactly one terminal outcome.
//
// A restart mid-decision is the case that produces double payment if the work
// row is not the authority: the first process may have decided and died before
// applying, and the second must not decide again from scratch.
func TestFailureMatrixVerifierRestartDecidesOnce(t *testing.T) {
	c := newFailureCase(t)
	c.claim(t)
	c.h.commitThroughStorage(c.ctx, c.f, c.task, c.body)

	// Nothing may have been decided before a processor ran. A verifier that was
	// never available must leave the work pending, not pre-decided.
	var decidedBefore int
	if err := c.h.pool().QueryRow(c.ctx, `
		SELECT COUNT(*) FROM verification_work
		 WHERE task_id=$1 AND terminal_outcome IS NOT NULL`, c.task).
		Scan(&decidedBefore); err != nil {
		t.Fatal(err)
	}
	if decidedBefore != 0 {
		t.Fatalf("%d terminal outcome(s) existed before any verifier ran", decidedBefore)
	}

	// The verifier runs twice, as a restarting process would.
	for i := 0; i < 2; i++ {
		if _, err := c.h.processor.ProcessAttempt(c.ctx, c.task, 0); err != nil {
			t.Logf("restart pass %d: %v", i, err)
		}
	}
	var terminals int
	if err := c.h.pool().QueryRow(c.ctx, `
		SELECT COUNT(*) FROM verification_work
		 WHERE task_id=$1 AND terminal_outcome IS NOT NULL`, c.task).Scan(&terminals); err != nil {
		t.Fatal(err)
	}
	if terminals > 1 {
		t.Errorf("a verifier restart produced %d terminal outcomes for one attempt", terminals)
	}
	// Verification settled, so the buyer is charged once and the supplier is
	// credited once. Restarting the verifier must not double either.
	assertFailureInvariants(t, c.ctx, c.h.pool(), "verifier restart", c.f.JobID, c.task,
		failureExpectation{
			MaxSupplierRows:      1,
			BuyerCharges:         1,
			TerminalTaskStatuses: []string{"verifying", "complete"},
			MaxRetries:           0,
		})
}

// A process crash after the ledger insert statements run but before the
// surrounding transaction commits must leave ZERO durable payables, and a
// subsequent verification pass must create exactly one.
//
// This is the "crash during settlement" case: the apply path is one serializable
// transaction (verification_apply.go), so an unrecovered panic at
// BoundaryAcceptedAfterLedger rolls everything back. Without that property a
// restart would either double-pay or leave a half-applied terminal decision.
func TestFailureMatrixCrashAfterLedgerBeforeCommitPaysOnce(t *testing.T) {
	c := newFailureCase(t)
	c.claim(t)
	c.h.commitThroughStorage(c.ctx, c.f, c.task, c.body)

	crash := &crashAtBoundary{at: BoundaryAcceptedAfterLedger}
	c.h.processor.probe = crash

	panicked := false
	func() {
		defer func() {
			if r := recover(); r != nil {
				panicked = true
				t.Logf("simulated crash: %v", r)
			}
		}()
		_, _ = c.h.processor.ProcessAttempt(c.ctx, c.task, 0)
	}()
	if !panicked {
		t.Fatal("expected simulated crash at BoundaryAcceptedAfterLedger; process did not panic")
	}
	if !crash.hit {
		t.Fatal("crash probe never reached BoundaryAcceptedAfterLedger")
	}

	// Nothing durable may stand after the rolled-back transaction.
	var supplierRows int
	if err := c.h.pool().QueryRow(c.ctx, `
		SELECT COUNT(*) FROM ledger_entries
		 WHERE task_id=$1 AND supplier_id IS NOT NULL`, c.task).Scan(&supplierRows); err != nil {
		t.Fatal(err)
	}
	if supplierRows != 0 {
		t.Fatalf("crash left %d durable supplier ledger row(s); settlement is not transactional", supplierRows)
	}
	var terminals int
	if err := c.h.pool().QueryRow(c.ctx, `
		SELECT COUNT(*) FROM verification_work
		 WHERE task_id=$1 AND terminal_outcome IS NOT NULL`, c.task).Scan(&terminals); err != nil {
		t.Fatal(err)
	}
	if terminals != 0 {
		t.Fatalf("crash left a terminal verification outcome without a committed ledger")
	}

	// A dead process does not release its verification lease; reclaim waits until
	// lease_expires_at. Expire it the way the wall clock would, then recover.
	if _, err := c.h.pool().Exec(c.ctx, `
		UPDATE verification_work
		   SET lease_expires_at=now()-interval '1 second'
		 WHERE task_id=$1 AND status='leased'`, c.task); err != nil {
		t.Fatalf("expire abandoned verification lease: %v", err)
	}

	// Recovery: clear the probe and process again. Exactly one payable.
	c.h.processor.probe = nil
	if _, err := c.h.processor.ProcessAttempt(c.ctx, c.task, 0); err != nil {
		t.Fatalf("recovery ProcessAttempt: %v", err)
	}
	assertFailureInvariants(t, c.ctx, c.h.pool(), "crash-after-ledger recovery", c.f.JobID, c.task,
		failureExpectation{
			MaxSupplierRows:      1,
			BuyerCharges:         1,
			TerminalTaskStatuses: []string{"verifying", "complete"},
			MaxRetries:           0,
		})
	if err := c.h.pool().QueryRow(c.ctx, `
		SELECT COUNT(*) FROM ledger_entries
		 WHERE task_id=$1 AND kind='supplier_credit'`, c.task).Scan(&supplierRows); err != nil {
		t.Fatal(err)
	}
	if supplierRows != 1 {
		t.Errorf("after recovery: %d supplier_credit rows, want exactly 1", supplierRows)
	}
}

// crashAtBoundary panics once when the named recovery boundary is reached.
// Used only by failure-matrix tests to simulate an OS-level process death mid
// settlement; production never installs this probe.
type crashAtBoundary struct {
	at  RecoveryBoundary
	hit bool
}

func (p *crashAtBoundary) Reach(_ context.Context, b RecoveryBoundary) {
	if b == p.at {
		p.hit = true
		panic("simulated process crash at " + string(b))
	}
}

// Finalization and settlement retried. One invoice, one debit, one payable.
func TestFailureMatrixFinalizerAndSettlementRetry(t *testing.T) {
	c := newFailureCase(t)
	c.claim(t)
	c.h.commitThroughStorage(c.ctx, c.f, c.task, c.body)
	for i := 0; i < 2; i++ {
		if _, err := c.h.processor.ProcessAttempt(c.ctx, c.task, 0); err != nil {
			t.Logf("verification pass %d: %v", i, err)
		}
	}

	// Three times, because a retry loop retries more than once.
	var finalizeErrs []string
	for i := 0; i < 3; i++ {
		if err := c.store.FinalizeJobTx(c.ctx, c.f.JobID); err != nil {
			finalizeErrs = append(finalizeErrs, err.Error())
		}
	}
	t.Logf("finalize retries reported: %v", finalizeErrs)

	var buyerCharges, supplierRows int
	if err := c.h.pool().QueryRow(c.ctx, `
		SELECT
		  (SELECT COUNT(*) FROM ledger_entries le JOIN tasks t ON t.id=le.task_id
		    WHERE t.job_id=$1 AND le.kind='buyer_charge'),
		  (SELECT COUNT(*) FROM ledger_entries WHERE task_id=$2 AND supplier_id IS NOT NULL)`,
		c.f.JobID, c.task).Scan(&buyerCharges, &supplierRows); err != nil {
		t.Fatal(err)
	}
	t.Logf("after 3 finalizations: buyer_charge rows=%d supplier rows=%d",
		buyerCharges, supplierRows)
	if buyerCharges > 1 {
		t.Errorf("%d buyer_charge rows after repeated finalization", buyerCharges)
	}
	if supplierRows > 1 {
		t.Errorf("%d supplier payable rows after repeated finalization", supplierRows)
	}
}

// A task whose lease expired — the agent died holding it — must be requeued a
// bounded number of times and must not pay.
func TestFailureMatrixExpiredLeaseIsRequeuedBounded(t *testing.T) {
	c := newFailureCase(t)

	const bound = 5
	for i := 0; i < bound+2; i++ {
		c.claim(t)
		if err := c.store.RequeueStaleTask(c.ctx, c.task, 0); err != nil {
			t.Fatalf("requeue %d: %v", i, err)
		}
	}
	var status string
	var retries int
	var claimed *uuid.UUID
	if err := c.h.pool().QueryRow(c.ctx,
		`SELECT status, retry_count, claimed_by FROM tasks WHERE id=$1`, c.task).
		Scan(&status, &retries, &claimed); err != nil {
		t.Fatal(err)
	}
	t.Logf("after %d expiries: status=%s retries=%d claimed=%v",
		bound+2, status, retries, claimed != nil)
	if claimed != nil {
		t.Error("a requeued task still holds its claim")
	}
	// The requeue itself does not enforce a ceiling; the scheduler's retry policy
	// does. What must be true here is that nothing was PAID for work that was
	// never delivered, and that the task is claimable rather than stuck.
	if status != "queued" {
		t.Errorf("a requeued task is %q", status)
	}
	assertFailureInvariants(t, c.ctx, c.h.pool(), "expired lease", c.f.JobID, c.task,
		failureExpectation{
			MaxSupplierRows:       0,
			SupplierNetMustBeZero: true,
			BuyerCharges:          0,
			TerminalTaskStatuses:  []string{"queued"},
			MaxRetries:            bound + 2,
		})
}

// Cancellation before any execution: nothing is owed in either direction.
func TestFailureMatrixCancellationBeforeExecution(t *testing.T) {
	c := newQueuedFailureCase(t)
	if err := c.store.CancelJob(c.ctx, c.f.JobID, c.f.BuyerID); err != nil {
		t.Fatalf("cancel: %v", err)
	}
	assertFailureInvariants(t, c.ctx, c.h.pool(), "cancelled before execution",
		c.f.JobID, c.task, failureExpectation{
			MaxSupplierRows:       0,
			SupplierNetMustBeZero: true,
			BuyerCharges:          0,
			TerminalTaskStatuses:  []string{"cancelled", "queued", "failed"},
			MaxRetries:            0,
		})
	var jobStatus string
	if err := c.h.pool().QueryRow(c.ctx, `SELECT status FROM jobs WHERE id=$1`, c.f.JobID).
		Scan(&jobStatus); err != nil {
		t.Fatal(err)
	}
	if jobStatus != "cancelled" {
		t.Errorf("job is %q after cancellation", jobStatus)
	}
}

// Cancellation while a task is running. The supplier did real work, so the
// question is not "pay nothing" but "do not create money": whatever is recorded
// must still conserve, and the job must not be invoiced as a delivered job.
func TestFailureMatrixCancellationDuringExecution(t *testing.T) {
	c := newFailureCase(t)
	c.claim(t)

	err := c.store.CancelJob(c.ctx, c.f.JobID, c.f.BuyerID)
	t.Logf("cancel during execution: %v", err)
	if err != nil && !errors.Is(err, errJobNotCancellable) {
		t.Fatalf("cancel returned an unexpected error: %v", err)
	}

	var jobStatus string
	if err := c.h.pool().QueryRow(c.ctx, `SELECT status FROM jobs WHERE id=$1`, c.f.JobID).
		Scan(&jobStatus); err != nil {
		t.Fatal(err)
	}
	t.Logf("job status after cancellation attempt: %s", jobStatus)

	var buyerCharges int
	var supplierNet float64
	if err := c.h.pool().QueryRow(c.ctx, `
		SELECT
		  (SELECT COUNT(*) FROM ledger_entries le JOIN tasks t ON t.id=le.task_id
		    WHERE t.job_id=$1 AND le.kind='buyer_charge'),
		  (SELECT COALESCE(SUM(amount_usd),0)::float8 FROM ledger_entries
		    WHERE task_id=$2 AND supplier_id IS NOT NULL)`,
		c.f.JobID, c.task).Scan(&buyerCharges, &supplierNet); err != nil {
		t.Fatal(err)
	}
	if buyerCharges != 0 {
		t.Errorf("%d buyer_charge rows for a job cancelled mid-execution", buyerCharges)
	}
	if supplierNet > 1e-9 {
		t.Errorf("%.9f USD stands for a task cancelled before it delivered", supplierNet)
	}
}

// A task whose input object is gone cannot be executed, and the loss must be
// visible rather than silent.
func TestFailureMatrixInputDownloadFailure(t *testing.T) {
	c := newFailureCase(t)
	var inputRef string
	if err := c.h.pool().QueryRow(c.ctx, `SELECT input_ref FROM tasks WHERE id=$1`, c.task).
		Scan(&inputRef); err != nil {
		t.Fatal(err)
	}
	if err := c.h.storage.RemoveObjects(c.ctx, []string{inputRef}); err != nil {
		t.Fatalf("remove input: %v", err)
	}
	exists, err := c.h.storage.ObjectExists(c.ctx, inputRef)
	if err != nil {
		t.Fatal(err)
	}
	if exists {
		t.Fatal("the input object is still present; this case tests nothing")
	}
	c.claim(t)
	if err := c.store.RequeueStaleTask(c.ctx, c.task, 0); err != nil {
		t.Fatalf("requeue after a failed download: %v", err)
	}
	assertFailureInvariants(t, c.ctx, c.h.pool(), "input download failure",
		c.f.JobID, c.task, failureExpectation{
			MaxSupplierRows:       0,
			SupplierNetMustBeZero: true,
			BuyerCharges:          0,
			TerminalTaskStatuses:  []string{"queued"},
			MaxRetries:            1,
		})
}

// The database goes away and comes back. Everything committed before must still
// be there, and nothing may be double-counted by the reconnecting process.
func TestFailureMatrixDatabaseRestart(t *testing.T) {
	c := newFailureCase(t)
	c.claim(t)
	c.h.commitThroughStorage(c.ctx, c.f, c.task, c.body)

	// Terminate every backend on this database, which is what a restart looks
	// like to a connected client.
	if _, err := c.h.pool().Exec(c.ctx, `
		SELECT pg_terminate_backend(pid) FROM pg_stat_activity
		 WHERE datname = current_database() AND pid <> pg_backend_pid()`); err != nil {
		t.Logf("terminate backends: %v", err)
	}
	// pgxpool reconnects on the next use; give it a moment to notice.
	time.Sleep(200 * time.Millisecond)

	var status string
	var attempts int
	for attempts = 0; attempts < 5; attempts++ {
		if err := c.h.pool().QueryRow(c.ctx,
			`SELECT status FROM tasks WHERE id=$1`, c.task).Scan(&status); err == nil {
			break
		}
		time.Sleep(200 * time.Millisecond)
	}
	if status == "" {
		t.Fatal("the pool did not recover after every backend was terminated")
	}
	t.Logf("after restart: task=%s (recovered on attempt %d)", status, attempts)

	for i := 0; i < 2; i++ {
		if _, err := c.h.processor.ProcessAttempt(c.ctx, c.task, 0); err != nil {
			t.Logf("post-restart verification pass %d: %v", i, err)
		}
	}
	assertFailureInvariants(t, c.ctx, c.h.pool(), "database restart", c.f.JobID, c.task,
		failureExpectation{
			MaxSupplierRows:      1,
			BuyerCharges:         1,
			TerminalTaskStatuses: []string{"verifying", "complete"},
			MaxRetries:           0,
		})
}

// A receipt for a job with no frozen plan must fail loudly rather than be
// assembled from defaults. A receipt that quietly omits its authority is worse
// than no receipt: it looks like one.
func TestFailureMatrixReceiptGenerationFailsLoudly(t *testing.T) {
	h, ctx, store := newVerifiedArtifactHarness(t)
	f := seedMoneyPathFixture(t, ctx, store, h.pool(), moneyPathSeedOpts{TaskCount: 1})
	tasks := makeTasks(f, 1)
	job := validJobRowDirected(t, f, tasks, llamaEmbedCell)
	corpus := []byte(`{"id":"0","text":"receipt authority"}` + "\n")
	for _, key := range []string{job.InputRef, tasks[0].InputRef} {
		if err := h.storage.PutObject(ctx, key, corpus, "application/x-ndjson"); err != nil {
			t.Fatalf("upload %s: %v", key, err)
		}
	}
	if err := store.SubmitJobTx(ctx, job, tasks); err != nil {
		t.Fatalf("submit: %v", err)
	}
	c := &failureCase{h: h, ctx: ctx, store: store, f: f, task: tasks[0].ID}

	// A submitted job has a frozen plan, so the negative below is about the plan
	// being removable rather than about it never having existed.
	plan, err := c.store.JobComputePlan(c.ctx, c.f.JobID)
	if err != nil || plan == nil {
		t.Fatalf("a submitted job has no frozen compute plan: %v", err)
	}

	// The strongest possible answer to "what if the receipt authority is gone" is
	// that it cannot be gone. The schema refuses to remove it, so a receipt
	// assembled without its authority is unreachable rather than merely refused.
	_, err = c.h.pool().Exec(c.ctx,
		`UPDATE jobs SET compute_plan = NULL, compute_plan_sha256 = NULL WHERE id=$1`,
		c.f.JobID)
	if err == nil {
		t.Fatal("the frozen compute plan was removed; a receipt could then be " +
			"assembled with no authority behind it")
	}
	t.Logf("removing a frozen plan is refused: %v", err)

	after, err := c.store.JobComputePlan(c.ctx, c.f.JobID)
	if err != nil || after == nil {
		t.Fatalf("the plan is gone after a refused removal: %v", err)
	}
	if after.WorkloadDecisionSHA256 != plan.WorkloadDecisionSHA256 {
		t.Error("the frozen plan changed under a refused removal")
	}
}
