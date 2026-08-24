package main

import (
	"context"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
)

// Claim-path independence for redundancy copies.
//
// Two separate rules live in the claim predicate:
//
//  1. Prior-executor / current-holder exclusion applies to EVERY redundancy
//     row. A worker (or another machine owned by the same supplier) that
//     already executed or is currently holding a sibling for the same
//     (job_id, COALESCE(chunk_index,0)) cannot take the check copy.
//  2. Frozen verification-class pinning belongs to hedged third-opinion
//     rows only. Ordinary redundancy has no frozen class.
//
// The race this file closes is not just the gate: identity on
// execution_worker_id exists only after a task has executed. An agent can
// hold a sibling with claimed_by set and execution_worker_id still NULL.
// Exclusion keys on claimed_by (and the holder’s supplier) as well.

func TestClaimTaskSQLAppliesPriorExclusionToEveryRedundancyRow(t *testing.T) {
	for _, predicate := range []string{
		"t.claimed_by = $1 AND t.started_at IS NULL",
		"t.claimed_by IS NULL",
	} {
		query := ClaimTaskSQL(predicate)
		for _, required := range []string{
			predicate,
			"NOT COALESCE(t.is_redundancy,false)",
			"prior.claimed_by",
			"holders.supplier_id",
			"prior.execution_worker_id",
			"t.verification_hw_class",
			"ej.claim_engine=t.verification_engine",
			"ej.claim_build_hash=t.verification_build_hash",
			"FROM task_execution_history history",
			"executed.worker_id=ej.claim_worker_id",
			"executed.supplier_id=ej.claim_supplier_id",
		} {
			if !strings.Contains(query, required) {
				t.Fatalf("claim branch %q is missing redundancy independence predicate %q",
					predicate, required)
			}
		}
		// The old gate applied exclusion only to hedged third-opinion rows.
		if strings.Contains(query, "NOT (COALESCE(t.is_redundancy,false) AND t.hedged_from IS NOT NULL)") {
			t.Fatalf("claim branch %q still gates prior-executor exclusion on hedged_from", predicate)
		}
		// Pinning must remain hedged-only. If it is required of ordinary
		// redundancy, those rows have no frozen class and become unclaimable.
		hedgedIdx := strings.Index(query, "t.hedged_from IS NULL")
		pinIdx := strings.Index(query, "NULLIF(COALESCE(t.verification_hw_class,''),'') IS NOT NULL")
		if hedgedIdx < 0 || pinIdx < 0 || pinIdx < hedgedIdx {
			t.Fatalf("claim branch %q must keep frozen verification-class pinning inside the hedged arm",
				predicate)
		}
	}
}

func TestSameWorkerCannotClaimPrimaryAndOrdinaryRedundancy(t *testing.T) {
	ctx, store, pool, env := openRedundancyClaimEnv(t)
	primaryWorker := env.addWorker(t, uuid.New())
	jobID, primaryID, redundancyID := env.insertOrdinaryRedundancyJob(t, 0)

	got := claimRedundancyAs(t, ctx, store, primaryWorker)
	if got == nil || got.TaskID != primaryID {
		t.Fatalf("first claim=%+v, want primary %s", got, primaryID)
	}

	got = claimRedundancyAs(t, ctx, store, primaryWorker)
	if got != nil && got.TaskID == redundancyID {
		t.Fatalf("worker %s claimed ordinary redundancy %s after taking primary %s on job %s — self-verification is not verification",
			primaryWorker.workerID, redundancyID, primaryID, jobID)
	}
	if got != nil {
		t.Fatalf("same worker claimed unexpected task %s after taking the primary", got.TaskID)
	}
	assertQueuedUnclaimed(t, ctx, pool, redundancyID)
}

func TestOrdinaryRedundancyExcludesAClaimedButUnexecutedSibling(t *testing.T) {
	ctx, store, pool, env := openRedundancyClaimEnv(t)
	holder := env.addWorker(t, uuid.New())
	peer := env.addWorker(t, uuid.New())
	_, primaryID, redundancyID := env.insertOrdinaryRedundancyJob(t, 0)

	// Pin-then-start window: claimed_by is set, execution_worker_id is not.
	// Status is running so the holder cannot re-take this row via the pin
	// branch. If exclusion keyed only on execution_worker_id, the holder
	// would still be handed the redundancy copy.
	if _, err := pool.Exec(ctx, `
		UPDATE tasks
		   SET status='running', claimed_by=$2, claimed_at=now(),
		       started_at=now(), worker_id=$2,
		       execution_worker_id=NULL, execution_supplier_id=NULL
		 WHERE id=$1`, primaryID, holder.workerID); err != nil {
		t.Fatal(err)
	}
	var execWorker *uuid.UUID
	if err := pool.QueryRow(ctx,
		`SELECT execution_worker_id FROM tasks WHERE id=$1`, primaryID,
	).Scan(&execWorker); err != nil {
		t.Fatal(err)
	}
	if execWorker != nil {
		t.Fatal("fixture is not the race: execution_worker_id is already set")
	}

	got := claimRedundancyAs(t, ctx, store, holder)
	if got != nil && got.TaskID == redundancyID {
		t.Fatalf("holder %s claimed redundancy %s while merely holding sibling %s (claimed_by set, execution_worker_id NULL)",
			holder.workerID, redundancyID, primaryID)
	}
	if got != nil {
		t.Fatalf("holder claimed unexpected task %s", got.TaskID)
	}
	assertQueuedUnclaimed(t, ctx, pool, redundancyID)

	got = claimRedundancyAs(t, ctx, store, peer)
	if got == nil || got.TaskID != redundancyID {
		t.Fatalf("independent worker claim=%+v, want ordinary redundancy %s", got, redundancyID)
	}
}

func TestOrdinaryRedundancyRemainsClaimableByAnIndependentWorker(t *testing.T) {
	ctx, store, _, env := openRedundancyClaimEnv(t)
	primaryWorker := env.addWorker(t, uuid.New())
	peer := env.addWorker(t, uuid.New())
	_, primaryID, redundancyID := env.insertOrdinaryRedundancyJob(t, 0)

	got := claimRedundancyAs(t, ctx, store, primaryWorker)
	if got == nil || got.TaskID != primaryID {
		t.Fatalf("primary claim=%+v, want %s", got, primaryID)
	}

	got = claimRedundancyAs(t, ctx, store, peer)
	if got == nil || got.TaskID != redundancyID {
		t.Fatalf("independent worker claim=%+v, want ordinary redundancy %s — exclusion must not make redundancy unclaimable",
			got, redundancyID)
	}
}

func TestOrdinaryRedundancyRejectsAnotherMachineFromTheSameSupplier(t *testing.T) {
	ctx, store, _, env := openRedundancyClaimEnv(t)
	supplierID := uuid.New()
	first := env.addWorker(t, supplierID)
	sibling := env.addWorker(t, supplierID)
	peer := env.addWorker(t, uuid.New())
	_, primaryID, redundancyID := env.insertOrdinaryRedundancyJob(t, 0)

	got := claimRedundancyAs(t, ctx, store, first)
	if got == nil || got.TaskID != primaryID {
		t.Fatalf("primary claim=%+v, want %s", got, primaryID)
	}

	got = claimRedundancyAs(t, ctx, store, sibling)
	if got != nil && got.TaskID == redundancyID {
		t.Fatalf("supplier %s verified itself: worker %s took redundancy %s after worker %s took the primary",
			supplierID, sibling.workerID, redundancyID, first.workerID)
	}
	if got != nil {
		t.Fatalf("same-supplier worker claimed unexpected task %s", got.TaskID)
	}

	got = claimRedundancyAs(t, ctx, store, peer)
	if got == nil || got.TaskID != redundancyID {
		t.Fatalf("independent supplier claim=%+v, want ordinary redundancy %s", got, redundancyID)
	}
}

func TestHedgedThirdOpinionKeepsFrozenVerificationClassPinning(t *testing.T) {
	ctx, store, pool, env := openRedundancyClaimEnv(t)
	primaryWorker := env.addWorker(t, uuid.New())
	matchingPeer := env.addWorker(t, uuid.New())
	wrongClass := env.addWorker(t, uuid.New())
	_, primaryID, hedgedID := env.insertHedgedThirdOpinionJob(t, "apple_silicon_pro")

	if _, err := pool.Exec(ctx, `
		UPDATE tasks
		   SET status='running', claimed_by=$2, claimed_at=now(),
		       started_at=now(), worker_id=$2,
		       execution_worker_id=$2, execution_supplier_id=$3,
		       execution_hw_class='apple_silicon_pro',
		       execution_engine='candle',
		       execution_build_hash=$4,
		       execution_build_identity_policy=$5
		 WHERE id=$1`,
		primaryID, primaryWorker.workerID, primaryWorker.supplierID,
		testOnlyPublicationBuildHash, currentEngineBuildIdentityPolicy,
	); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx,
		`UPDATE workers SET hw_class='apple_silicon_max' WHERE id=$1`,
		wrongClass.workerID,
	); err != nil {
		t.Fatal(err)
	}

	got := claimRedundancyAs(t, ctx, store, primaryWorker)
	if got != nil && got.TaskID == hedgedID {
		t.Fatalf("prior executor %s claimed hedged third-opinion %s", primaryWorker.workerID, hedgedID)
	}

	got = claimRedundancyAs(t, ctx, store, wrongClass)
	if got != nil && got.TaskID == hedgedID {
		t.Fatalf("worker %s with hw_class=apple_silicon_max claimed hedged task frozen to apple_silicon_pro",
			wrongClass.workerID)
	}
	assertQueuedUnclaimed(t, ctx, pool, hedgedID)

	got = claimRedundancyAs(t, ctx, store, matchingPeer)
	if got == nil || got.TaskID != hedgedID {
		t.Fatalf("class-matched independent worker claim=%+v, want hedged third-opinion %s",
			got, hedgedID)
	}
}

func TestNonRedundancyRowsAreUnaffectedByPriorExecutorExclusion(t *testing.T) {
	ctx, store, _, env := openRedundancyClaimEnv(t)
	worker := env.addWorker(t, uuid.New())
	jobID := uuid.New()
	firstID, secondID := uuid.New(), uuid.New()
	if _, err := env.pool.Exec(ctx, `
		INSERT INTO jobs (id,buyer_id,status,job_type,model_ref,input_ref,task_count,
		                  offered_rate_usd_hr,min_memory_gb,tier,currency)
		VALUES ($1,$2,'running','embed','all-minilm-l6-v2','in',2,10.0,0,'batch',$3)`,
		jobID, env.buyerID, SettlementCurrencyCode()); err != nil {
		t.Fatal(err)
	}
	if _, err := env.pool.Exec(ctx, `
		INSERT INTO tasks (id,job_id,status,input_ref,result_key,chunk_index,is_redundancy,created_at)
		VALUES ($1,$3,'queued','in','rk-0',0,false,now()-interval '2 seconds'),
		       ($2,$3,'queued','in','rk-1',1,false,now()-interval '1 second')`,
		firstID, secondID, jobID); err != nil {
		t.Fatal(err)
	}

	got := claimRedundancyAs(t, ctx, store, worker)
	if got == nil || got.TaskID != firstID {
		t.Fatalf("first non-redundancy claim=%+v, want %s", got, firstID)
	}
	got = claimRedundancyAs(t, ctx, store, worker)
	if got == nil || got.TaskID != secondID {
		t.Fatalf("second non-redundancy claim=%+v, want %s — exclusion must not apply to non-redundancy rows",
			got, secondID)
	}
}

type redundancyClaimWorker struct {
	supplierID, workerID uuid.UUID
}

type redundancyClaimEnv struct {
	ctx     context.Context
	store   *Store
	pool    *pgxpool.Pool
	buyerID uuid.UUID
}

func openRedundancyClaimEnv(t *testing.T) (context.Context, *Store, *pgxpool.Pool, *redundancyClaimEnv) {
	t.Helper()
	installBoundCataloguePublicationAuthorityForTest(t)
	ctx, store, pool := openIsolatedTestStore(t)
	installed := currentActivation()
	activeRuntimeActivation.Store(newRuntimeActivation(
		installed.PolicyRevision, map[string]string{}, nil))

	buyerID, err := store.CreateBuyerAccount(ctx, "redun-"+uuid.NewString()+"@example.test", "pw", 100)
	must(t, err)
	return ctx, store, pool, &redundancyClaimEnv{ctx: ctx, store: store, pool: pool, buyerID: buyerID}
}

func (e *redundancyClaimEnv) addWorker(t *testing.T, supplierID uuid.UUID) redundancyClaimWorker {
	t.Helper()
	if supplierID == uuid.Nil {
		supplierID = uuid.New()
	}
	workerID := uuid.New()
	var exists bool
	if err := e.pool.QueryRow(e.ctx, `SELECT EXISTS(SELECT 1 FROM suppliers WHERE id=$1)`, supplierID).
		Scan(&exists); err != nil {
		t.Fatal(err)
	}
	if !exists {
		if _, err := e.pool.Exec(e.ctx, `
			INSERT INTO suppliers (id,email,status,reputation,completed_tasks)
			VALUES ($1,$2,'active',0.95,100)`,
			supplierID, "redun-sup-"+uuid.NewString()+"@example.test"); err != nil {
			t.Fatal(err)
		}
	}
	cap := testWorkerCapability(workerID, supplierID)
	cap.Sandboxed = true
	cap.UnsandboxedOptIn = false
	mustf(t, e.store.UpsertWorker(e.ctx, cap), "register worker: %v")
	bindWorkerDeviceCredential(t, e.pool, e.ctx, workerID)
	return redundancyClaimWorker{supplierID: supplierID, workerID: workerID}
}

func (e *redundancyClaimEnv) insertOrdinaryRedundancyJob(t *testing.T, chunk int) (jobID, primaryID, redundancyID uuid.UUID) {
	t.Helper()
	return e.insertRedundancyPair(t, chunk, false, "")
}

func (e *redundancyClaimEnv) insertHedgedThirdOpinionJob(t *testing.T, frozenClass string) (jobID, primaryID, redundancyID uuid.UUID) {
	t.Helper()
	return e.insertRedundancyPair(t, 0, true, frozenClass)
}

func (e *redundancyClaimEnv) insertRedundancyPair(
	t *testing.T, chunk int, hedged bool, frozenClass string,
) (jobID, primaryID, redundancyID uuid.UUID) {
	t.Helper()
	jobID, primaryID, redundancyID = uuid.New(), uuid.New(), uuid.New()
	currency := SettlementCurrencyCode()
	if currency == "" {
		t.Fatal("redundancy claim fixture requires a settlement currency")
	}
	if _, err := e.pool.Exec(e.ctx, `
		INSERT INTO jobs (id,buyer_id,status,job_type,model_ref,input_ref,task_count,
		                  offered_rate_usd_hr,min_memory_gb,tier,currency)
		VALUES ($1,$2,'running','embed','all-minilm-l6-v2','in',2,10.0,0,'batch',$3)`,
		jobID, e.buyerID, currency); err != nil {
		t.Fatal(err)
	}
	if _, err := e.pool.Exec(e.ctx, `
		INSERT INTO tasks (id,job_id,status,input_ref,result_key,chunk_index,is_redundancy,created_at)
		VALUES ($1,$2,'queued','in','rk-primary',$3,false,now()-interval '2 seconds')`,
		primaryID, jobID, chunk); err != nil {
		t.Fatal(err)
	}
	var hedgedFrom any
	if hedged {
		hedgedFrom = primaryID
	}
	if _, err := e.pool.Exec(e.ctx, `
		INSERT INTO tasks
		  (id,job_id,status,input_ref,result_key,chunk_index,is_redundancy,hedged_from,
		   verification_hw_class,created_at)
		VALUES ($1,$2,'queued','in','rk-redundancy',$3,true,$4,NULLIF($5,''),now()-interval '1 second')`,
		redundancyID, jobID, chunk, hedgedFrom, frozenClass); err != nil {
		t.Fatal(err)
	}
	return jobID, primaryID, redundancyID
}

func claimRedundancyAs(t *testing.T, ctx context.Context, store *Store, w redundancyClaimWorker) *ClaimedTask {
	t.Helper()
	got, err := store.ClaimTasksTx(ctx, WorkerAuth{WorkerID: w.workerID, SupplierID: w.supplierID})
	mustf(t, err, "claim: %v")
	return got
}

func assertQueuedUnclaimed(t *testing.T, ctx context.Context, pool *pgxpool.Pool, taskID uuid.UUID) {
	t.Helper()
	var status string
	var claimedBy *uuid.UUID
	if err := pool.QueryRow(ctx,
		`SELECT status, claimed_by FROM tasks WHERE id=$1`, taskID,
	).Scan(&status, &claimedBy); err != nil {
		t.Fatal(err)
	}
	if status != "queued" || claimedBy != nil {
		t.Fatalf("task %s status=%s claimed_by=%v, want queued and unclaimed", taskID, status, claimedBy)
	}
}
