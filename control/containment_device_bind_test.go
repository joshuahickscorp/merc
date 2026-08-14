package main

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/google/uuid"
)

// Containment device-binding: sandboxed=true is self-declared; ordinary buyer
// work also requires an active device-bound supply credential. These tests
// assert the bypass, not the happy path — through real claim/quote eligibility.

func installContainmentDeviceBindFixture(t *testing.T) {
	t.Helper()
	installBoundCataloguePublicationAuthorityForTest(t)
	installed := currentActivation()
	activeRuntimeActivation.Store(newRuntimeActivation(
		installed.PolicyRevision, map[string]string{}, nil))
}

// TestUnboundSandboxedWorkerIneligibleForOrdinaryBuyerWork: attacker reports
// sandboxed=true and holds only an unbound CreateWorkerToken mint.
func TestUnboundSandboxedWorkerIneligibleForOrdinaryBuyerWork(t *testing.T) {
	installContainmentDeviceBindFixture(t)
	ctx, store, pool := openIsolatedTestStore(t)

	suffix := uuid.NewString()
	buyerID, err := store.CreateBuyerAccount(ctx, "unbound-sand-buyer-"+suffix+"@example.test", "pw", 100)
	must(t, err)
	supplierID, workerID := uuid.New(), uuid.New()
	if _, err := pool.Exec(ctx, `INSERT INTO suppliers (id,email,status,reputation,completed_tasks)
		 VALUES ($1,$2,'active',0.95,100)`,
		supplierID, "unbound-sand-sup-"+suffix+"@example.test"); err != nil {
		t.Fatal(err)
	}
	registerContainmentCleanup(t, pool, []uuid.UUID{workerID}, nil)

	cap := testWorkerCapability(workerID, supplierID)
	cap.Sandboxed = true
	cap.UnsandboxedOptIn = false
	mustf(t, store.UpsertWorker(ctx, cap), "register unbound sandboxed worker: %v")
	if _, err := store.CreateWorkerToken(ctx, workerID, supplierID); err != nil {
		t.Fatalf("mint unbound token (attacker path): %v", err)
	}
	var bound bool
	if err := pool.QueryRow(ctx, `
		SELECT EXISTS (
		  SELECT 1 FROM worker_tokens
		   WHERE worker_id=$1 AND revoked=false AND device_fingerprint IS NOT NULL
		)`, workerID).Scan(&bound); err != nil {
		t.Fatal(err)
	}
	if bound {
		t.Fatal("fixture accidentally device-bound — test would not exercise the bypass")
	}
	var sandboxed bool
	if err := pool.QueryRow(ctx,
		`SELECT sandboxed FROM workers WHERE id=$1`, workerID,
	).Scan(&sandboxed); err != nil {
		t.Fatal(err)
	}
	if !sandboxed {
		t.Fatal("fixture not sandboxed=true — self-declaration must still be stored")
	}

	jobID, taskID := uuid.New(), uuid.New()
	registerContainmentCleanup(t, pool, nil, []uuid.UUID{jobID})
	if _, err := pool.Exec(ctx, `
		INSERT INTO jobs (id,buyer_id,status,job_type,model_ref,input_ref,task_count,
		                  offered_rate_usd_hr,min_memory_gb,tier)
		VALUES ($1,$2,'running','embed','all-minilm-l6-v2','in',1,10.0,0,'batch')`,
		jobID, buyerID); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `
		INSERT INTO tasks (id,job_id,status,input_ref,result_key)
		VALUES ($1,$2,'queued','in','rk')`, taskID, jobID); err != nil {
		t.Fatal(err)
	}

	// Real claim path.
	got, err := store.ClaimTasksTx(ctx, WorkerAuth{WorkerID: workerID, SupplierID: supplierID})
	mustf(t, err, "claim: %v")
	if got != nil && (got.TaskID == taskID || got.JobID == jobID) {
		t.Fatalf("unbound sandboxed worker claimed ordinary task %s — "+
			"device binding must be required alongside sandboxed=true", got.TaskID)
	}
	var status string
	var claimedBy *uuid.UUID
	if err := pool.QueryRow(ctx,
		`SELECT status, claimed_by FROM tasks WHERE id=$1`, taskID,
	).Scan(&status, &claimedBy); err != nil {
		t.Fatal(err)
	}
	if status != "queued" || claimedBy != nil {
		t.Fatalf("ordinary task status=%s claimed_by=%v; unbound sandboxed worker must not take it",
			status, claimedBy)
	}

	// Real quote path: this worker alone must not inflate capacity.
	// Baseline is an empty fleet for this model under the test DB's isolation
	// once only this unbound sandboxed worker is registered.
	n, err := store.EligibleWorkerCountFor(ctx, "embed", "all-minilm-l6-v2",
		QuoteSupplyRequirements{MinMemoryGB: 0})
	mustf(t, err, "quote eligible count: %v")
	if n != 0 {
		t.Fatalf("quote capacity=%d with only unbound sandboxed supply; claim would refuse", n)
	}
}

// TestDeviceBoundSandboxedWorkerEligibleForOrdinaryBuyerWork: the control still
// works when sandboxed=true is paired with a device-bound credential.
func TestDeviceBoundSandboxedWorkerEligibleForOrdinaryBuyerWork(t *testing.T) {
	installContainmentDeviceBindFixture(t)
	ctx, store, pool := openIsolatedTestStore(t)

	suffix := uuid.NewString()
	buyerID, err := store.CreateBuyerAccount(ctx, "bound-sand-buyer-"+suffix+"@example.test", "pw", 100)
	must(t, err)
	supplierID, workerID := uuid.New(), uuid.New()
	if _, err := pool.Exec(ctx, `INSERT INTO suppliers (id,email,status,reputation,completed_tasks)
		 VALUES ($1,$2,'active',0.95,100)`,
		supplierID, "bound-sand-sup-"+suffix+"@example.test"); err != nil {
		t.Fatal(err)
	}
	registerContainmentCleanup(t, pool, []uuid.UUID{workerID}, nil)

	cap := testWorkerCapability(workerID, supplierID)
	cap.Sandboxed = true
	cap.UnsandboxedOptIn = false
	mustf(t, store.UpsertWorker(ctx, cap), "register device-bound sandboxed worker: %v")
	if _, err := store.IssueDeviceBoundWorkerToken(ctx, workerID, supplierID,
		"bound-device-"+workerID.String()); err != nil {
		t.Fatalf("issue device-bound token: %v", err)
	}

	jobID, taskID := uuid.New(), uuid.New()
	registerContainmentCleanup(t, pool, nil, []uuid.UUID{jobID})
	if _, err := pool.Exec(ctx, `
		INSERT INTO jobs (id,buyer_id,status,job_type,model_ref,input_ref,task_count,
		                  offered_rate_usd_hr,min_memory_gb,tier)
		VALUES ($1,$2,'running','embed','all-minilm-l6-v2','in',1,10.0,0,'batch')`,
		jobID, buyerID); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `
		INSERT INTO tasks (id,job_id,status,input_ref,result_key)
		VALUES ($1,$2,'queued','in','rk')`, taskID, jobID); err != nil {
		t.Fatal(err)
	}

	got, err := store.ClaimTasksTx(ctx, WorkerAuth{WorkerID: workerID, SupplierID: supplierID})
	mustf(t, err, "claim: %v")
	if got == nil || got.TaskID != taskID {
		t.Fatalf("device-bound sandboxed worker claim=%+v, want task %s — control closed the product",
			got, taskID)
	}
}

// TestDirectedWorkStillReachesUncontainedUnboundSupply: directed_cell_id remains
// the explicit uncontained permit; device binding must not be required.
func TestDirectedWorkStillReachesUncontainedUnboundSupply(t *testing.T) {
	installContainmentDeviceBindFixture(t)
	ctx, store, pool := openIsolatedTestStore(t)

	suffix := uuid.NewString()
	buyerID, err := store.CreateBuyerAccount(ctx, "directed-unbound-buyer-"+suffix+"@example.test", "pw", 100)
	must(t, err)
	supplierID, workerID := uuid.New(), uuid.New()
	if _, err := pool.Exec(ctx, `INSERT INTO suppliers (id,email,status,reputation,completed_tasks)
		 VALUES ($1,$2,'active',0.95,100)`,
		supplierID, "directed-unbound-sup-"+suffix+"@example.test"); err != nil {
		t.Fatal(err)
	}
	registerContainmentCleanup(t, pool, []uuid.UUID{workerID}, nil)

	cap := testWorkerCapability(workerID, supplierID)
	cap.Sandboxed = false
	cap.UnsandboxedOptIn = false
	mustf(t, store.UpsertWorker(ctx, cap), "register uncontained directed worker: %v")
	if _, err := store.CreateWorkerToken(ctx, workerID, supplierID); err != nil {
		t.Fatalf("unbound token for directed worker: %v", err)
	}

	var directedCell string
	for _, c := range directedRuntimeCapabilities() {
		if c.Job == "embed" && c.Model == "all-minilm-l6-v2" && c.Engine == "candle" {
			directedCell = c.ID
			break
		}
	}
	if directedCell == "" {
		for _, c := range directedRuntimeCapabilities() {
			if c.Job == "embed" && c.Model == "all-minilm-l6-v2" {
				directedCell = c.ID
				break
			}
		}
	}
	if directedCell == "" {
		t.Fatal("no directed embed cell available for uncontained permit proof")
	}
	decision, err := buildWorkloadDecisionDirected(
		jobSubmit{
			JobType: JobType{Type: "embed"},
			Model:   ModelRef{Ref: "all-minilm-l6-v2"},
			Tier:    "batch",
		},
		strings.Repeat("a", 64),
		directedCell,
	)
	mustf(t, err, "directed decision: %v")
	if decision.DirectedCellID == "" {
		t.Fatal("directed decision missing directed_cell_id")
	}
	if len(decision.RuntimeCandidates) == 0 {
		t.Fatal("directed decision has no runtime candidates")
	}

	cand := decision.RuntimeCandidates[0]
	if _, err := pool.Exec(ctx, `
		INSERT INTO worker_authorized_capabilities
		  (worker_id,cell_id,runtime_id,job_type,model_ref,model_kind,matrix_sha256,routable,authorized_at)
		VALUES ($1,$2,$3,'embed','all-minilm-l6-v2',$4,$5,false,now())
		ON CONFLICT DO NOTHING`,
		workerID, cand.CellID, cand.RuntimeID, cand.ModelKind, generatedRuntimeMatrixSHA256,
	); err != nil {
		_, _ = pool.Exec(ctx, `
			INSERT INTO worker_authorized_capabilities
			  (worker_id,cell_id,runtime_id,job_type,model_ref,model_kind,matrix_sha256,routable,authorized_at)
			SELECT $1,$2,$3,'embed','all-minilm-l6-v2',$4,$5,false,now()
			 WHERE NOT EXISTS (
			   SELECT 1 FROM worker_authorized_capabilities
			    WHERE worker_id=$1 AND cell_id=$2 AND runtime_id=$3
			      AND job_type='embed' AND model_ref='all-minilm-l6-v2'
			      AND matrix_sha256=$5
			 )`,
			workerID, cand.CellID, cand.RuntimeID, cand.ModelKind, generatedRuntimeMatrixSHA256)
	}
	if _, err := pool.Exec(ctx,
		`UPDATE workers SET engine=$2 WHERE id=$1`, workerID, cand.Engine); err != nil {
		t.Fatal(err)
	}

	jobID, taskID := uuid.New(), uuid.New()
	registerContainmentCleanup(t, pool, nil, []uuid.UUID{jobID})
	decisionJSON, err := json.Marshal(decision)
	mustf(t, err, "marshal decision: %v")
	if _, err := pool.Exec(ctx, `
		INSERT INTO jobs (id,buyer_id,status,job_type,model_ref,input_ref,task_count,
		                  offered_rate_usd_hr,min_memory_gb,tier,workload_decision)
		VALUES ($1,$2,'running','embed','all-minilm-l6-v2','in',1,10.0,0,'batch',$3::jsonb)`,
		jobID, buyerID, decisionJSON); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `
		INSERT INTO tasks (id,job_id,status,input_ref,result_key)
		VALUES ($1,$2,'queued','in','rk')`, taskID, jobID); err != nil {
		t.Fatal(err)
	}

	got, err := store.ClaimTasksTx(ctx, WorkerAuth{WorkerID: workerID, SupplierID: supplierID})
	mustf(t, err, "directed claim: %v")
	if got == nil || got.TaskID != taskID {
		t.Fatalf("uncontained unbound worker claim=%+v on directed job, want task %s — "+
			"device bind must not block the directed exception", got, taskID)
	}
}

// TestCreateWorkerTokenCannotProduceOrdinaryWorkEligibleWorker: the unbound mint
// path (and its supplier activation) cannot yield ordinary-work eligibility.
func TestCreateWorkerTokenCannotProduceOrdinaryWorkEligibleWorker(t *testing.T) {
	installContainmentDeviceBindFixture(t)
	ctx, store, pool := openIsolatedTestStore(t)

	suffix := uuid.NewString()
	buyerID, err := store.CreateBuyerAccount(ctx, "mint-path-buyer-"+suffix+"@example.test", "pw", 100)
	must(t, err)
	supplierID, workerID := uuid.New(), uuid.New()
	if _, err := pool.Exec(ctx, `INSERT INTO suppliers (id,email,status,reputation,completed_tasks)
		 VALUES ($1,$2,'pending',0.95,100)`,
		supplierID, "mint-path-sup-"+suffix+"@example.test"); err != nil {
		t.Fatal(err)
	}
	registerContainmentCleanup(t, pool, []uuid.UUID{workerID}, nil)

	token, err := store.CreateWorkerToken(ctx, workerID, supplierID)
	mustf(t, err, "CreateWorkerToken: %v")
	if token == "" {
		t.Fatal("empty token")
	}
	var status string
	if err := pool.QueryRow(ctx, `SELECT status FROM suppliers WHERE id=$1`, supplierID).Scan(&status); err != nil {
		t.Fatal(err)
	}
	if status != "active" {
		t.Fatalf("CreateWorkerToken did not activate supplier (status=%s)", status)
	}
	cap := testWorkerCapability(workerID, supplierID)
	cap.Sandboxed = true
	mustf(t, store.UpsertWorker(ctx, cap), "register after mint: %v")

	jobID, taskID := uuid.New(), uuid.New()
	registerContainmentCleanup(t, pool, nil, []uuid.UUID{jobID})
	if _, err := pool.Exec(ctx, `
		INSERT INTO jobs (id,buyer_id,status,job_type,model_ref,input_ref,task_count,
		                  offered_rate_usd_hr,min_memory_gb,tier)
		VALUES ($1,$2,'running','embed','all-minilm-l6-v2','in',1,10.0,0,'batch')`,
		jobID, buyerID); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `
		INSERT INTO tasks (id,job_id,status,input_ref,result_key)
		VALUES ($1,$2,'queued','in','rk')`, taskID, jobID); err != nil {
		t.Fatal(err)
	}

	got, err := store.ClaimTasksTx(ctx, WorkerAuth{WorkerID: workerID, SupplierID: supplierID})
	mustf(t, err, "claim after CreateWorkerToken: %v")
	if got != nil && got.TaskID == taskID {
		t.Fatal("CreateWorkerToken path produced ordinary-work-eligible worker")
	}

	t.Setenv("MERC_ENV", "production")
	_, err = store.CreateWorkerToken(ctx, uuid.New(), supplierID)
	if !errors.Is(err, errWorkerTokenUnboundForbidden) {
		t.Fatalf("production CreateWorkerToken err=%v, want %v", err, errWorkerTokenUnboundForbidden)
	}
}

// TestStripeAcctUniquenessAtEnrolmentAndConstraint: named refusal at enrolment
// and UNIQUE holds if the check is bypassed.
func TestStripeAcctUniquenessAtEnrolmentAndConstraint(t *testing.T) {
	ctx, store, pool := openIsolatedTestStore(t)
	suffix := uuid.NewString()
	acct := "acct_shared_" + suffix[:8]

	supA, supB := uuid.New(), uuid.New()
	if _, err := pool.Exec(ctx, `INSERT INTO suppliers (id,email,status)
		 VALUES ($1,$2,'active'), ($3,$4,'active')`,
		supA, "stripe-a-"+suffix+"@example.test",
		supB, "stripe-b-"+suffix+"@example.test"); err != nil {
		t.Fatal(err)
	}

	mustf(t, store.SetSupplierStripeAcct(ctx, supA, acct), "first enrolment: %v")
	err := store.SetSupplierStripeAcct(ctx, supB, acct)
	if !errors.Is(err, errStripeAcctTaken) {
		t.Fatalf("second enrolment err=%v, want named %v", err, errStripeAcctTaken)
	}

	_, err = pool.Exec(ctx, `UPDATE suppliers SET stripe_acct=$2 WHERE id=$1`, supB, acct)
	if err == nil {
		t.Fatal("direct UPDATE sharing stripe_acct succeeded — UNIQUE missing")
	}
	if !isUniqueViolation(err) && !strings.Contains(err.Error(), "suppliers_stripe_acct_uniq") {
		t.Fatalf("direct UPDATE err=%v, want unique violation on suppliers_stripe_acct_uniq", err)
	}

	mustf(t, store.SetSupplierStripeAcct(ctx, supA, acct), "idempotent re-set: %v")
}

// TestSeededDevelopmentFleetTakesOrdinaryWork: seed fleet remains locally
// ordinary-work eligible via real device binding (not a production exception).
func TestSeededDevelopmentFleetTakesOrdinaryWork(t *testing.T) {
	installContainmentDeviceBindFixture(t)
	ctx, store, pool := openIsolatedTestStore(t)
	mustf(t, seedDemo(ctx, pool, nil), "seed demo fleet: %v")

	var bound, sandboxed bool
	if err := pool.QueryRow(ctx, `
		SELECT w.sandboxed,
		       EXISTS (
		         SELECT 1 FROM worker_tokens wt
		          WHERE wt.worker_id = w.id AND wt.revoked = false
		            AND wt.device_fingerprint IS NOT NULL
		            AND btrim(wt.device_fingerprint) <> ''
		            AND (wt.expires_at IS NULL OR wt.expires_at > now())
		       )
		  FROM workers w WHERE w.id = $1`, demoWorkerID).Scan(&sandboxed, &bound); err != nil {
		t.Fatal(err)
	}
	if !sandboxed || !bound {
		t.Fatalf("seed fleet not ordinary-eligible: sandboxed=%v device_bound=%v", sandboxed, bound)
	}

	buyerID, err := uuid.Parse(demoBuyerID)
	must(t, err)
	workerID, err := uuid.Parse(demoWorkerID)
	must(t, err)
	supplierID, err := uuid.Parse(demoSupplierID)
	must(t, err)

	if _, err := pool.Exec(ctx, `UPDATE workers SET last_seen_at=now() WHERE id=$1`, workerID); err != nil {
		t.Fatal(err)
	}
	// Align seed worker identity with the publication authority cell so claim
	// can authorize embed capability. Device bind on the seed token is the
	// containment fact under test; capability projection is fixture setup.
	cap := testWorkerCapability(workerID, supplierID)
	cap.Sandboxed = true
	mustf(t, store.UpsertWorker(ctx, cap), "refresh seed worker capability: %v")
	if err := pool.QueryRow(ctx, `
		SELECT EXISTS (
		  SELECT 1 FROM worker_tokens
		   WHERE worker_id=$1 AND revoked=false AND device_fingerprint IS NOT NULL
		)`, workerID).Scan(&bound); err != nil || !bound {
		t.Fatalf("seed device bind lost after capability refresh: bound=%v err=%v", bound, err)
	}

	jobID, taskID := uuid.New(), uuid.New()
	registerContainmentCleanup(t, pool, nil, []uuid.UUID{jobID})
	if _, err := pool.Exec(ctx, `
		INSERT INTO jobs (id,buyer_id,status,job_type,model_ref,input_ref,task_count,
		                  offered_rate_usd_hr,min_memory_gb,tier)
		VALUES ($1,$2,'running','embed','all-minilm-l6-v2','in',1,10.0,0,'batch')`,
		jobID, buyerID); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `
		INSERT INTO tasks (id,job_id,status,input_ref,result_key)
		VALUES ($1,$2,'queued','in','rk')`, taskID, jobID); err != nil {
		t.Fatal(err)
	}

	got, err := store.ClaimTasksTx(ctx, WorkerAuth{WorkerID: workerID, SupplierID: supplierID})
	mustf(t, err, "seed fleet claim: %v")
	if got == nil || got.TaskID != taskID {
		t.Fatalf("seed fleet claim=%+v, want task %s — local development ordinary work broken",
			got, taskID)
	}
}
