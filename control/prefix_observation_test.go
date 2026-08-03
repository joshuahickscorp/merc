package main

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"
)

// A stale index entry is corrected by observation, not trusted forever.
// Believed-warm + observed cached_tokens == 0 must drop the row so the next
// DeepestWarmPrefix returns 0. A pure TTL that has not yet expired is not
// enough: the engine already reported a miss.

func TestStalePrefixIndexCorrectedByObservationMiss(t *testing.T) {
	ctx, store, pool := openPayoutTestStore(t)

	supplier := uuid.New()
	if _, err := pool.Exec(ctx,
		`INSERT INTO suppliers (id,email,reputation,status) VALUES ($1,$2,0.5,'active')
		 ON CONFLICT (id) DO NOTHING`, supplier, supplier.String()+"@obs.invalid"); err != nil {
		t.Fatal(err)
	}
	worker := uuid.New()
	if _, err := store.CreateWorkerToken(ctx, worker, supplier); err != nil {
		t.Fatal(err)
	}

	nonce := int(uuid.New().ID())
	tokens := make([]int, 200)
	for i := range tokens {
		tokens[i] = nonce + i
	}
	chain := ComputePrefixChain(tokens)
	if len(chain) == 0 {
		t.Fatal("empty chain")
	}
	f := seedPayoutFixture(t, ctx, pool, payoutFixtureOpts{creditUSD: 1.00, supplierID: supplier})
	if err := store.RecordJobPrefixChain(ctx, f.jobID, chain); err != nil {
		t.Fatal(err)
	}
	if err := store.MarkPrefixChainWarm(ctx, worker, chain); err != nil {
		t.Fatal(err)
	}

	// Precondition: belief says warm.
	depth, err := store.DeepestWarmPrefix(ctx, worker, f.jobID)
	if err != nil {
		t.Fatal(err)
	}
	if depth == 0 {
		t.Fatal("fixture not warm before observation")
	}

	// Engine reports a miss (vLLM-shaped cached_tokens == 0).
	out, err := store.CorrectPrefixBeliefFromObservation(ctx, worker, f.jobID, true, 0)
	if err != nil {
		t.Fatal(err)
	}
	if out.Action != PrefixObsInvalidated {
		t.Fatalf("action = %s, want %s (believed=%d cached=%d)",
			out.Action, PrefixObsInvalidated, out.BelievedDepth, out.CachedTokens)
	}
	if out.InvalidatedRows == 0 {
		t.Fatal("observation miss invalidated zero rows; stale belief still present")
	}

	// Postcondition: no longer trusted.
	depth, err = store.DeepestWarmPrefix(ctx, worker, f.jobID)
	if err != nil {
		t.Fatal(err)
	}
	if depth != 0 {
		t.Fatalf("stale belief still reports depth %d after observed miss", depth)
	}

	// A second observation against the now-cold index is a cold_serve, not a
	// repeated invalidate — we do not thrash empty state.
	out2, err := store.CorrectPrefixBeliefFromObservation(ctx, worker, f.jobID, true, 0)
	if err != nil {
		t.Fatal(err)
	}
	if out2.Action != PrefixObsColdServe {
		t.Fatalf("second miss against cold index: action=%s, want %s", out2.Action, PrefixObsColdServe)
	}
}

func TestObservationHitConfirmsRatherThanInvalidates(t *testing.T) {
	ctx, store, pool := openPayoutTestStore(t)

	supplier := uuid.New()
	if _, err := pool.Exec(ctx,
		`INSERT INTO suppliers (id,email,reputation,status) VALUES ($1,$2,0.5,'active')
		 ON CONFLICT (id) DO NOTHING`, supplier, supplier.String()+"@obs-hit.invalid"); err != nil {
		t.Fatal(err)
	}
	worker := uuid.New()
	if _, err := store.CreateWorkerToken(ctx, worker, supplier); err != nil {
		t.Fatal(err)
	}
	nonce := int(uuid.New().ID())
	tokens := make([]int, 128)
	for i := range tokens {
		tokens[i] = nonce + i
	}
	chain := ComputePrefixChain(tokens)
	f := seedPayoutFixture(t, ctx, pool, payoutFixtureOpts{creditUSD: 1.00, supplierID: supplier})
	if err := store.RecordJobPrefixChain(ctx, f.jobID, chain); err != nil {
		t.Fatal(err)
	}
	if err := store.MarkPrefixChainWarm(ctx, worker, chain); err != nil {
		t.Fatal(err)
	}

	out, err := store.CorrectPrefixBeliefFromObservation(ctx, worker, f.jobID, true, 64)
	if err != nil {
		t.Fatal(err)
	}
	if out.Action != PrefixObsConfirmed {
		t.Fatalf("action = %s, want %s", out.Action, PrefixObsConfirmed)
	}
	depth, err := store.DeepestWarmPrefix(ctx, worker, f.jobID)
	if err != nil {
		t.Fatal(err)
	}
	if depth == 0 {
		t.Fatal("confirmed hit cleared warmth; confirm must not invalidate")
	}
}

func TestNoEngineSignalLeavesBeliefUntouched(t *testing.T) {
	ctx, store, pool := openPayoutTestStore(t)

	supplier := uuid.New()
	if _, err := pool.Exec(ctx,
		`INSERT INTO suppliers (id,email,reputation,status) VALUES ($1,$2,0.5,'active')
		 ON CONFLICT (id) DO NOTHING`, supplier, supplier.String()+"@obs-nosig.invalid"); err != nil {
		t.Fatal(err)
	}
	worker := uuid.New()
	if _, err := store.CreateWorkerToken(ctx, worker, supplier); err != nil {
		t.Fatal(err)
	}
	nonce := int(uuid.New().ID())
	tokens := make([]int, 128)
	for i := range tokens {
		tokens[i] = nonce + i
	}
	chain := ComputePrefixChain(tokens)
	f := seedPayoutFixture(t, ctx, pool, payoutFixtureOpts{creditUSD: 1.00, supplierID: supplier})
	if err := store.RecordJobPrefixChain(ctx, f.jobID, chain); err != nil {
		t.Fatal(err)
	}
	if err := store.MarkPrefixChainWarm(ctx, worker, chain); err != nil {
		t.Fatal(err)
	}

	// hasSignal=false: absence of the field is not a miss.
	out, err := store.CorrectPrefixBeliefFromObservation(ctx, worker, f.jobID, false, 0)
	if err != nil {
		t.Fatal(err)
	}
	if out.Action != PrefixObsNoSignal {
		t.Fatalf("action = %s, want %s", out.Action, PrefixObsNoSignal)
	}
	depth, err := store.DeepestWarmPrefix(ctx, worker, f.jobID)
	if err != nil {
		t.Fatal(err)
	}
	if depth == 0 {
		t.Fatal("no-signal path must not clear belief (would thrash local engines)")
	}
}

// Cross-class claim path: warm_prefix_depth must not override cheaper_class_online.
// cheaper_class_online is order-only (unlike cheaper_ask which hard-defers), so
// the proof needs two jobs — the same shape as TestClaimingNVIDIADefersSharedWorkToIdleOwnedMac:
//
//	shared job  — both classes eligible; expensive worker is fully warm on its chain
//	dear-only   — pinned to the expensive class
//
// The expensive claimer must take dear-only (cheaper_class_online=false) over the
// warm shared job (cheaper_class_online=true), even though warm_prefix_depth is
// higher on the shared job. If cost rank is stripped from ORDER BY, warmth pulls
// the shared job first and this fails.
func TestWarmExpensiveClassDoesNotBeatColdCheapClass(t *testing.T) {
	ctx, store, pool, buyerID := seedPrefixClaimEnv(t)

	cheap := mkPrefixClaimWorker(t, ctx, pool, "apple_silicon_base", 0.50)
	dear := mkPrefixClaimWorker(t, ctx, pool, "nvidia_24gb", 0.50)
	if hwClassCostRank(cheap.hwClass) >= hwClassCostRank(dear.hwClass) {
		t.Fatalf("fixture ranks wrong: cheap=%d dear=%d",
			hwClassCostRank(cheap.hwClass), hwClassCostRank(dear.hwClass))
	}

	chain := uniqueTokenChain(t, 256)
	// Shared job: both classes can take it; expensive worker is warm.
	sharedJob, sharedTask := seedPrefixClaimJob(t, ctx, pool, store, buyerID, chain)
	if err := store.MarkPrefixChainWarm(ctx, dear.workerID, chain); err != nil {
		t.Fatal(err)
	}
	// Age shared older so without cost-class preference age would pick it first.
	if _, err := pool.Exec(ctx,
		`UPDATE tasks SET created_at = now() - interval '2 minutes',
		                     visible_at = now() - interval '2 minutes' WHERE id=$1`,
		sharedTask); err != nil {
		t.Fatal(err)
	}
	_ = sharedJob

	// Dear-only job: hw_classes pin excludes the cheap class.
	dearOnlyJob, dearOnlyTask := uuid.New(), uuid.New()
	if _, err := pool.Exec(ctx, `
		INSERT INTO jobs (id,buyer_id,status,job_type,model_ref,input_ref,task_count,
		                  offered_rate_usd_hr,min_memory_gb,tier,hw_classes)
		VALUES ($1,$2,'running','embed','all-minilm-l6-v2','in',1,10.0,0,'batch',
		        ARRAY['nvidia_24gb'])`,
		dearOnlyJob, buyerID); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `
		INSERT INTO tasks (id,job_id,status,input_ref,result_key)
		VALUES ($1,$2,'queued','in','rk')`, dearOnlyTask, dearOnlyJob); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		c, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cancel()
		_, _ = pool.Exec(c, `DELETE FROM tasks WHERE id=$1`, dearOnlyTask)
		_, _ = pool.Exec(c, `DELETE FROM jobs WHERE id=$1`, dearOnlyJob)
	})

	got := claimAs(t, ctx, store, dear)
	if got == nil {
		t.Fatal("expensive class claimed nothing")
	}
	if got.TaskID != dearOnlyTask {
		t.Fatalf("warm expensive class took task %s (job %s); want dear-only %s, not warm shared %s — cost class must outrank prefix warmth",
			got.TaskID, got.JobID, dearOnlyTask, sharedTask)
	}

	// Cold cheap class still gets the shared job.
	gotCheap := claimAs(t, ctx, store, cheap)
	if gotCheap == nil || gotCheap.TaskID != sharedTask {
		t.Fatalf("cheap class should claim the shared warm job; got %+v", gotCheap)
	}
}
