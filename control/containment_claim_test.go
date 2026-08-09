package main

import (
	"encoding/json"
	"os"
	"strings"
	"testing"

	"github.com/google/uuid"
)

// Containment becomes claim eligibility, not a recorded attribute.
//
// Bible §18: public heterogeneous supply is not routable until trust classes
// and workload restrictions are explicit. A worker with sandboxed=false and
// unsandboxed_opt_in=false previously passed every claim filter — the defect
// this file locks closed.

// TestUncontainedWorkerCannotClaimOrdinaryBuyerWork is the failing-before
// proof: sandboxed=false, opt_in=false must receive no ordinary buyer supply.
func TestUncontainedWorkerCannotClaimOrdinaryBuyerWork(t *testing.T) {
	installBoundCataloguePublicationAuthorityForTest(t)
	ctx, store, pool := openIsolatedTestStore(t)
	installed := currentActivation()
	activeRuntimeActivation.Store(newRuntimeActivation(
		installed.PolicyRevision, map[string]string{}, nil))

	suffix := uuid.NewString()
	buyerID, err := store.CreateBuyerAccount(ctx, "uncontained-buyer-"+suffix+"@example.test", "pw", 100)
	must(t, err)
	supplierID, workerID := uuid.New(), uuid.New()
	if _, err := pool.Exec(ctx, `INSERT INTO suppliers (id,email,status,reputation,completed_tasks)
		 VALUES ($1,$2,'active',0.95,100)`,
		supplierID, "uncontained-sup-"+suffix+"@example.test"); err != nil {
		t.Fatal(err)
	}
	registerContainmentCleanup(t, pool, []uuid.UUID{workerID}, nil)

	cap := testWorkerCapability(workerID, supplierID)
	// Honest non-mac / uncontained report: neither sandboxed nor opted in.
	// This is the defect shape — currently claimable, must become ineligible.
	cap.Sandboxed = false
	cap.UnsandboxedOptIn = false
	mustf(t, store.UpsertWorker(ctx, cap), "register uncontained worker: %v")

	var sandboxed, optIn bool
	if err := pool.QueryRow(ctx,
		`SELECT sandboxed, unsandboxed_opt_in FROM workers WHERE id=$1`, workerID,
	).Scan(&sandboxed, &optIn); err != nil {
		t.Fatal(err)
	}
	if sandboxed || optIn {
		t.Fatalf("fixture not uncontained: sandboxed=%v opt_in=%v", sandboxed, optIn)
	}

	jobID, taskID := uuid.New(), uuid.New()
	registerContainmentCleanup(t, pool, nil, []uuid.UUID{jobID})
	// Ordinary buyer job: no directed_cell_id, no workload decision pin.
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
	if got != nil && (got.TaskID == taskID || got.JobID == jobID) {
		t.Fatalf("uncontained worker claimed ordinary buyer task %s on job %s — "+
			"sandboxed=false must be ineligible for public supply", got.TaskID, got.JobID)
	}
	var status string
	var claimedBy *uuid.UUID
	if err := pool.QueryRow(ctx,
		`SELECT status, claimed_by FROM tasks WHERE id=$1`, taskID,
	).Scan(&status, &claimedBy); err != nil {
		t.Fatal(err)
	}
	if status != "queued" || claimedBy != nil {
		t.Fatalf("ordinary task status=%s claimed_by=%v; uncontained worker must not take it",
			status, claimedBy)
	}
}

// TestSandboxedWorkerStillClaimsOrdinaryBuyerWork keeps the product open for
// contained supply.
func TestSandboxedWorkerStillClaimsOrdinaryBuyerWork(t *testing.T) {
	installBoundCataloguePublicationAuthorityForTest(t)
	ctx, store, pool := openIsolatedTestStore(t)
	installed := currentActivation()
	activeRuntimeActivation.Store(newRuntimeActivation(
		installed.PolicyRevision, map[string]string{}, nil))

	suffix := uuid.NewString()
	buyerID, err := store.CreateBuyerAccount(ctx, "sandboxed-buyer-"+suffix+"@example.test", "pw", 100)
	must(t, err)
	supplierID, workerID := uuid.New(), uuid.New()
	if _, err := pool.Exec(ctx, `INSERT INTO suppliers (id,email,status,reputation,completed_tasks)
		 VALUES ($1,$2,'active',0.95,100)`,
		supplierID, "sandboxed-sup-"+suffix+"@example.test"); err != nil {
		t.Fatal(err)
	}
	registerContainmentCleanup(t, pool, []uuid.UUID{workerID}, nil)

	cap := testWorkerCapability(workerID, supplierID)
	cap.Sandboxed = true
	cap.UnsandboxedOptIn = false
	mustf(t, store.UpsertWorker(ctx, cap), "register sandboxed worker: %v")

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
		t.Fatalf("sandboxed worker claim=%+v, want task %s — gate closed the product", got, taskID)
	}
}

// TestUnsandboxedOptInRemainsExcludedFromOrdinaryWork independently of sandboxed.
func TestUnsandboxedOptInRemainsExcludedFromOrdinaryWork(t *testing.T) {
	installBoundCataloguePublicationAuthorityForTest(t)
	ctx, store, pool := openIsolatedTestStore(t)
	installed := currentActivation()
	activeRuntimeActivation.Store(newRuntimeActivation(
		installed.PolicyRevision, map[string]string{}, nil))

	suffix := uuid.NewString()
	buyerID, err := store.CreateBuyerAccount(ctx, "optin-buyer-"+suffix+"@example.test", "pw", 100)
	must(t, err)
	supplierID, workerID := uuid.New(), uuid.New()
	if _, err := pool.Exec(ctx, `INSERT INTO suppliers (id,email,status,reputation,completed_tasks)
		 VALUES ($1,$2,'active',0.95,100)`,
		supplierID, "optin-sup-"+suffix+"@example.test"); err != nil {
		t.Fatal(err)
	}
	registerContainmentCleanup(t, pool, []uuid.UUID{workerID}, nil)

	// Deliberate opt-in with sandboxed=false (the macOS MERC_ALLOW_UNSANDBOXED path).
	cap := testWorkerCapability(workerID, supplierID)
	cap.Sandboxed = false
	cap.UnsandboxedOptIn = true
	mustf(t, store.UpsertWorker(ctx, cap), "register opt-in worker: %v")

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
	if got != nil && (got.TaskID == taskID || got.JobID == jobID) {
		t.Fatalf("unsandboxed_opt_in worker claimed ordinary task %s — opt-in must remain excluded",
			got.TaskID)
	}
}

// TestDirectedWorkPermitsUncontainedSupply: directed_cell_id on the workload
// decision is the named permit for operator/lab/canary work on uncontained hosts.
// The permission is visible on the job row, not inferred from a missing predicate.
func TestDirectedWorkPermitsUncontainedSupply(t *testing.T) {
	installBoundCataloguePublicationAuthorityForTest(t)
	ctx, store, pool := openIsolatedTestStore(t)
	installed := currentActivation()
	activeRuntimeActivation.Store(newRuntimeActivation(
		installed.PolicyRevision, map[string]string{}, nil))

	suffix := uuid.NewString()
	buyerID, err := store.CreateBuyerAccount(ctx, "directed-buyer-"+suffix+"@example.test", "pw", 100)
	must(t, err)
	supplierID, workerID := uuid.New(), uuid.New()
	if _, err := pool.Exec(ctx, `INSERT INTO suppliers (id,email,status,reputation,completed_tasks)
		 VALUES ($1,$2,'active',0.95,100)`,
		supplierID, "directed-sup-"+suffix+"@example.test"); err != nil {
		t.Fatal(err)
	}
	registerContainmentCleanup(t, pool, []uuid.UUID{workerID}, nil)

	cap := testWorkerCapability(workerID, supplierID)
	cap.Sandboxed = false
	cap.UnsandboxedOptIn = false
	mustf(t, store.UpsertWorker(ctx, cap), "register uncontained directed worker: %v")

	// Pick a directed-reachable candle embed cell the worker can actually hold.
	var directedCell string
	for _, c := range directedRuntimeCapabilities() {
		if c.Job == "embed" && c.Model == "all-minilm-l6-v2" && c.Engine == "candle" {
			directedCell = c.ID
			break
		}
	}
	if directedCell == "" {
		// Fall back to any directed embed cell and re-register the worker to match.
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
		t.Fatal("directed decision missing directed_cell_id — permission would not be visible on the row")
	}
	if len(decision.RuntimeCandidates) == 0 {
		t.Fatal("directed decision has no runtime candidates")
	}

	// Ensure the worker holds the exact directed cell capability the job freezes.
	// UpsertWorker projects from engine identity; when the directed cell is the
	// candle TEST_ONLY cell the existing registration already covers it. When
	// not, plant the authorized capability row matching the frozen candidate.
	cand := decision.RuntimeCandidates[0]
	if _, err := pool.Exec(ctx, `
		INSERT INTO worker_authorized_capabilities
		  (worker_id,cell_id,runtime_id,job_type,model_ref,model_kind,matrix_sha256,routable,authorized_at)
		VALUES ($1,$2,$3,'embed','all-minilm-l6-v2',$4,$5,false,now())
		ON CONFLICT DO NOTHING`,
		workerID, cand.CellID, cand.RuntimeID, cand.ModelKind, generatedRuntimeMatrixSHA256,
	); err != nil {
		// Some schemas use a unique index rather than ON CONFLICT target; fall through.
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
	// Keep engine aligned with the frozen candidate so claim matching accepts it.
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
	// Visibility proof: the permit lives on the row.
	var directedOnRow string
	if err := pool.QueryRow(ctx,
		`SELECT COALESCE(workload_decision->>'directed_cell_id','') FROM jobs WHERE id=$1`,
		jobID,
	).Scan(&directedOnRow); err != nil {
		t.Fatal(err)
	}
	if directedOnRow == "" {
		t.Fatal("directed_cell_id not visible on the job row — permission is not explicit")
	}
	if directedOnRow != decision.DirectedCellID {
		t.Fatalf("row directed_cell_id=%q want %q", directedOnRow, decision.DirectedCellID)
	}

	if _, err := pool.Exec(ctx, `
		INSERT INTO tasks (id,job_id,status,input_ref,result_key)
		VALUES ($1,$2,'queued','in','rk')`, taskID, jobID); err != nil {
		t.Fatal(err)
	}

	got, err := store.ClaimTasksTx(ctx, WorkerAuth{WorkerID: workerID, SupplierID: supplierID})
	mustf(t, err, "claim directed: %v")
	if got == nil || got.TaskID != taskID {
		t.Fatalf("uncontained worker claim of directed work=%+v, want task %s — "+
			"directed_cell_id must permit uncontained supply", got, taskID)
	}
}

// TestQuoteCapacityAgreesWithContainmentClaimGate: an uncontained worker must
// not inflate quote eligible_workers_now when the claim path would refuse it.
func TestQuoteCapacityAgreesWithContainmentClaimGate(t *testing.T) {
	installBoundCataloguePublicationAuthorityForTest(t)
	ctx, store, pool := openIsolatedTestStore(t)
	installed := currentActivation()
	activeRuntimeActivation.Store(newRuntimeActivation(
		installed.PolicyRevision, map[string]string{}, nil))

	supplierID, workerID := uuid.New(), uuid.New()
	if _, err := pool.Exec(ctx, `INSERT INTO suppliers (id,email,status,reputation,completed_tasks)
		 VALUES ($1,$2,'active',0.95,100)`,
		supplierID, "quote-uncontained-"+uuid.NewString()+"@example.test"); err != nil {
		t.Fatal(err)
	}
	registerContainmentCleanup(t, pool, []uuid.UUID{workerID}, nil)

	cap := testWorkerCapability(workerID, supplierID)
	cap.Sandboxed = false
	cap.UnsandboxedOptIn = false
	mustf(t, store.UpsertWorker(ctx, cap), "register uncontained quote worker: %v")

	n, err := store.EligibleWorkerCountFor(ctx, "embed", "all-minilm-l6-v2",
		QuoteSupplyRequirements{MinMemoryGB: 0})
	mustf(t, err, "eligible count: %v")
	if n != 0 {
		t.Fatalf("quote capacity counted %d eligible workers including uncontained supply; "+
			"claim would refuse — quote and claim disagree", n)
	}

	// Sandboxed peer must still be counted so the gate did not zero the market.
	peerSupplier, peerWorker := uuid.New(), uuid.New()
	if _, err := pool.Exec(ctx, `INSERT INTO suppliers (id,email,status,reputation,completed_tasks)
		 VALUES ($1,$2,'active',0.95,100)`,
		peerSupplier, "quote-sandboxed-"+uuid.NewString()+"@example.test"); err != nil {
		t.Fatal(err)
	}
	registerContainmentCleanup(t, pool, []uuid.UUID{peerWorker}, nil)
	peer := testWorkerCapability(peerWorker, peerSupplier)
	peer.Sandboxed = true
	mustf(t, store.UpsertWorker(ctx, peer), "register sandboxed quote peer: %v")

	n, err = store.EligibleWorkerCountFor(ctx, "embed", "all-minilm-l6-v2",
		QuoteSupplyRequirements{MinMemoryGB: 0})
	mustf(t, err, "eligible count after sandboxed peer: %v")
	if n < 1 {
		t.Fatalf("quote capacity=%d after sandboxed peer registered; gate closed the market", n)
	}
}

// TestClaimContainmentPredicatesArePresentOnEveryBranch is a structural lock
// so a future rewrite cannot drop the sandboxed gate from one peer scan.
func TestClaimContainmentPredicatesArePresentOnEveryBranch(t *testing.T) {
	// The helper body is the single source of the predicates.
	rawH, err := os.ReadFile("buyer_supplier_independence.go")
	must(t, err)
	helper := string(rawH)
	for _, predicate := range []string{
		".sandboxed, false)",
		"workload_decision->>'directed_cell_id'",
		".unsandboxed_opt_in, false)",
		"func workerJobContainmentSQL",
	} {
		if !strings.Contains(helper, predicate) {
			t.Errorf("workerJobContainmentSQL helper missing %q", predicate)
		}
	}
	// Claim path splices the helper for me / w2 / w3.
	raw, err := os.ReadFile("scheduler.go")
	must(t, err)
	sql := string(raw)
	for _, call := range []string{
		`workerJobContainmentSQL("me", "j")`,
		`workerJobContainmentSQL("w2", "j")`,
		`workerJobContainmentSQL("w3", "j")`,
	} {
		if !strings.Contains(sql, call) {
			t.Errorf("claim SQL missing containment splice %q", call)
		}
	}
	// Expanded claim SQL must still carry the predicates (string-build path).
	expanded := ClaimTaskSQL("t.claimed_by IS NULL")
	for _, predicate := range []string{
		"COALESCE(me.sandboxed, false)",
		"COALESCE(w2.sandboxed, false)",
		"COALESCE(w3.sandboxed, false)",
		"workload_decision->>'directed_cell_id'",
		"NOT COALESCE(me.unsandboxed_opt_in, false)",
		"NOT COALESCE(w2.unsandboxed_opt_in, false)",
		"NOT COALESCE(w3.unsandboxed_opt_in, false)",
	} {
		if !strings.Contains(expanded, predicate) {
			t.Errorf("expanded ClaimTaskSQL missing containment predicate %q", predicate)
		}
	}
	// Quote capacity must require containment so it cannot advertise unfillable supply.
	rawQ, err := os.ReadFile("quote.go")
	must(t, err)
	qsql := string(rawQ)
	for _, predicate := range []string{
		"COALESCE(w.sandboxed, false)",
		"NOT COALESCE(w.unsandboxed_opt_in, false)",
	} {
		if !strings.Contains(qsql, predicate) {
			t.Errorf("quote claimableWorkerPredicateSQL missing %q", predicate)
		}
	}
}
