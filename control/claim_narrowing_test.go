package main

import (
	"context"
	"reflect"
	"testing"
	"time"

	"github.com/google/uuid"
)

func TestValidateClaimNarrowingLadderNonIncreasing(t *testing.T) {
	good := []ClaimNarrowingStageCount{
		{Stage: claimNarrowHardContract, Kind: claimStageHardFilter, Surviving: 10},
		{Stage: claimNarrowTrustSandboxPrivacy, Kind: claimStageHardFilter, Surviving: 8},
		{Stage: claimNarrowRegionFailureDomain, Kind: claimStageHardFilter, Surviving: 7,
			Note: "data_residency only"},
		{Stage: claimNarrowWorkloadRuntime, Kind: claimStageHardFilter, Surviving: 5},
		{Stage: claimNarrowArtifactLocality, Kind: claimStagePreferenceOnly, Surviving: 5,
			Note: "preference only"},
		{Stage: claimNarrowPrefixCacheLocality, Kind: claimStagePreferenceOnly, Surviving: 5,
			Note: "preference only"},
		{Stage: claimNarrowAvailabilityQueue, Kind: claimStageHardFilter, Surviving: 3},
		{Stage: claimNarrowEconomicShortlist, Kind: claimStageSoftDeferral, Surviving: 2},
		{Stage: claimNarrowExpensiveScoring, Kind: claimStageAbsent, Surviving: 2,
			Note: "absent scorer"},
	}
	if err := ValidateClaimNarrowingLadder(good); err != nil {
		t.Fatalf("valid ladder rejected: %v", err)
	}
	// Non-decreasing upward: reverse of non-increasing down.
	for i := len(good) - 1; i > 0; i-- {
		if good[i-1].Surviving < good[i].Surviving {
			t.Fatalf("upward non-decreasing violated at %s", good[i].Stage)
		}
	}

	// Increasing down the ladder must fail.
	bad := append([]ClaimNarrowingStageCount(nil), good...)
	bad[6].Surviving = 9 // availability > runtime
	if err := ValidateClaimNarrowingLadder(bad); err == nil {
		t.Fatal("expected rejection when surviving increases down the ladder")
	}

	// Preference-only must not invent a drop.
	bad2 := append([]ClaimNarrowingStageCount(nil), good...)
	bad2[4].Surviving = 4
	if err := ValidateClaimNarrowingLadder(bad2); err == nil {
		t.Fatal("preference_only stage must equal prior surviving")
	}
}

func TestClaimObservationFamiliesRefuseEpochInvention(t *testing.T) {
	o := DefaultClaimObservationFamilies()
	if o.CandidateEpoch != batchCandidateEpochNone {
		t.Fatalf("candidate epoch = %q, want %s", o.CandidateEpoch, batchCandidateEpochNone)
	}
	if o.FailureDomainBound {
		t.Fatal("failure_domain must not be bound")
	}
	if o.WorkerPeerLivenessSecs != 60 || o.WACAuthorizationDays != 7 {
		t.Fatalf("TTL families mismatch: %+v", o)
	}
	if o.RealtimeOfferSecs != realtimeOfferWarmthTTLSecs {
		t.Fatal("realtime offer TTL must be documented as not-bound contrast")
	}
}

func TestRealtimeAuthorizeNarrowingStagesDocumentProfileScope(t *testing.T) {
	stages := RealtimeAuthorizeNarrowingStages(4)
	if err := ValidateClaimNarrowingLadder(stages); err != nil {
		t.Fatal(err)
	}
	// Intermediate stages collapse to the final book size — honest about
	// missing per-stage cardinality in production offer SQL.
	for _, s := range stages {
		if s.Surviving != 4 {
			t.Fatalf("stage %s surviving %d want 4", s.Stage, s.Surviving)
		}
	}
}

func TestClaimNarrowingStagesAndEligibilityUnchanged(t *testing.T) {
	ctx, store, pool := openIsolatedTestStore(t)
	installSettlementCurrencyForTest(t, "usd")
	claimNarrowingMeasureOnHotPath.Store(true)
	t.Cleanup(func() { claimNarrowingMeasureOnHotPath.Store(false) })

	// Ensure Step 18 peer indexes exist on the isolated schema (Migrate applies
	// schema.sql which includes them; re-create is idempotent).
	if err := createClaimPeerIndexes(ctx, pool); err != nil {
		t.Fatal(err)
	}

	suffix := uuid.NewString()
	buyerID, err := store.CreateBuyerAccount(ctx, "narrow-"+suffix+"@example.test", "integration-password", 100)
	must(t, err)

	type worker struct {
		supplierID, workerID uuid.UUID
		ask                  float64
	}
	mk := func(ask float64) worker {
		w := worker{supplierID: uuid.New(), workerID: uuid.New(), ask: ask}
		if _, err := pool.Exec(ctx,
			`INSERT INTO suppliers (id,email,status,reputation,completed_tasks)
			 VALUES ($1,$2,'active',0.95,100)`,
			w.supplierID, "narrow-sup-"+uuid.NewString()+"@example.test"); err != nil {
			t.Fatal(err)
		}
		if _, err := pool.Exec(ctx,
			`INSERT INTO workers (id,supplier_id,hw_class,memory_gb,effective_memory_gb,
			                      last_seen_at,throttled,min_payout_usd_hr)
			 VALUES ($1,$2,'apple_silicon_max',64,64,now(),false,$3)`,
			w.workerID, w.supplierID, ask); err != nil {
			t.Fatal(err)
		}
		bindLegacyTestWorkerExactExecutionIdentity(t, pool, ctx, w.workerID)
		if _, err := pool.Exec(ctx,
			`INSERT INTO worker_authorized_capabilities
			   (worker_id,cell_id,runtime_id,job_type,model_ref,model_kind,matrix_sha256)
			 VALUES ($1,'cell','rt','embed','all-minilm-l6-v2','hf',$2)`,
			w.workerID, generatedRuntimeMatrixSHA256); err != nil {
			t.Fatal(err)
		}
		return w
	}
	w := mk(0.10)

	jobID := uuid.New()
	taskID := uuid.New()
	if _, err := pool.Exec(ctx, `
		INSERT INTO jobs (id,buyer_id,status,job_type,model_ref,input_ref,task_count,
		                  offered_rate_usd_hr,min_memory_gb,tier,currency)
		VALUES ($1,$2,'running','embed','all-minilm-l6-v2','in',1,10.0,0,'batch','usd')`,
		jobID, buyerID); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `
		INSERT INTO tasks (id,job_id,status,input_ref,result_key)
		VALUES ($1,$2,'queued','in','rk')`, taskID, jobID); err != nil {
		t.Fatal(err)
	}

	auth := WorkerAuth{WorkerID: w.workerID, SupplierID: w.supplierID}
	selfRank := hwClassCostRank("apple_silicon_max")

	// Stage cardinalities: non-increasing, preference/absent pass-through.
	trace, err := store.MeasureClaimNarrowing(ctx, auth, selfRank, w.ask, "usd")
	mustf(t, err, "MeasureClaimNarrowing: %v")
	if err := ValidateClaimNarrowingLadder(trace.Stages); err != nil {
		t.Fatalf("ladder validation: %v", err)
	}
	// Fixture job should survive through availability for this worker.
	var ready int
	for _, s := range trace.Stages {
		if s.Stage == claimNarrowAvailabilityQueue {
			ready = s.Surviving
		}
	}
	if ready < 1 {
		t.Fatalf("expected fixture job at availability stage, got stages=%+v", trace.Stages)
	}
	if trace.Observation.CandidateEpoch != batchCandidateEpochNone {
		t.Fatalf("observation must refuse frozen epoch, got %q", trace.Observation.CandidateEpoch)
	}

	// Eligibility set before claim.
	before, err := store.listEligibleClaimJobIDs(ctx, auth, selfRank, w.ask, "usd")
	mustf(t, err, "list eligible before: %v")
	if len(before) < 1 {
		t.Fatal("worker should be eligible for fixture job")
	}

	// Tripwire: break the currency hard filter deliberately — eligibility empties.
	claimEligibilityTripwireRejectAll.Store(true)
	t.Cleanup(func() { claimEligibilityTripwireRejectAll.Store(false) })
	broken, err := store.listEligibleClaimJobIDs(ctx, auth, selfRank, w.ask, "usd")
	mustf(t, err, "list eligible with tripwire: %v")
	if len(broken) != 0 {
		t.Fatalf("tripwire must empty eligibility, got %v", broken)
	}
	// Claim must also refuse under the tripwire.
	got, err := store.ClaimTasksTx(ctx, auth)
	mustf(t, err, "claim under tripwire: %v")
	if got != nil {
		t.Fatalf("tripwire must prevent claim, got task %s", got.TaskID)
	}

	// Restore filter — same worker can claim the same task again.
	claimEligibilityTripwireRejectAll.Store(false)
	after, err := store.listEligibleClaimJobIDs(ctx, auth, selfRank, w.ask, "usd")
	mustf(t, err, "list eligible after restore: %v")
	if !reflect.DeepEqual(before, after) {
		t.Fatalf("eligibility changed after restore: before=%v after=%v", before, after)
	}
	got, err = store.ClaimTasksTx(ctx, auth)
	mustf(t, err, "claim after restore: %v")
	if got == nil {
		t.Fatal("worker must claim after tripwire restored")
	}
	if got.TaskID != taskID {
		t.Fatalf("claimed %s want %s", got.TaskID, taskID)
	}
	if got.Narrowing == nil {
		t.Fatal("ClaimTasksTx must attach narrowing stage cardinalities")
	}
	if err := ValidateClaimNarrowingLadder(got.Narrowing.Stages); err != nil {
		t.Fatalf("claim narrowing invalid: %v", err)
	}
	if got.WorkerPlacement == nil || got.WorkerPlacement.ClaimEligibility == nil {
		t.Fatal("claim must bind WorkerPlacement eligibility")
	}
	if got.WorkerPlacement.ClaimEligibility.CandidateEpoch != batchCandidateEpochNone {
		t.Fatal("batch placement must not invent candidate epoch")
	}
	if got.WorkerPlacement.ClaimEligibility.Observation == nil {
		t.Fatal("claim eligibility must record observation families")
	}
}

func TestClaimExistsExplainBeforeAfterIndex(t *testing.T) {
	ctx, store, pool := openIsolatedTestStore(t)
	installSettlementCurrencyForTest(t, "usd")
	_ = store

	// Seed a modest peer fleet so EXPLAIN has something to plan against.
	buyerID, err := store.CreateBuyerAccount(ctx, "explain-"+uuid.NewString()+"@example.test", "integration-password", 100)
	must(t, err)
	claimSup := uuid.New()
	claimW := uuid.New()
	if _, err := pool.Exec(ctx,
		`INSERT INTO suppliers (id,email,status,reputation,completed_tasks)
		 VALUES ($1,$2,'active',0.95,100)`, claimSup, "ex-sup-"+uuid.NewString()+"@example.test"); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx,
		`INSERT INTO workers (id,supplier_id,hw_class,memory_gb,effective_memory_gb,
		                      last_seen_at,throttled,min_payout_usd_hr)
		 VALUES ($1,$2,'apple_silicon_max',64,64,now(),false,5.0)`,
		claimW, claimSup); err != nil {
		t.Fatal(err)
	}
	bindLegacyTestWorkerExactExecutionIdentity(t, pool, ctx, claimW)
	if _, err := pool.Exec(ctx,
		`INSERT INTO worker_authorized_capabilities
		   (worker_id,cell_id,runtime_id,job_type,model_ref,model_kind,matrix_sha256)
		 VALUES ($1,'cell','rt','embed','all-minilm-l6-v2','hf',$2)`,
		claimW, generatedRuntimeMatrixSHA256); err != nil {
		t.Fatal(err)
	}

	// Drop peer indexes if present so "before" is unindexed partials (schema
	// migrate may already have created them — drop for the measure).
	for _, name := range []string{
		"workers_live_ask_seen_idx",
		"workers_live_hwclass_seen_idx",
		"worker_authorized_capabilities_fresh_supply_idx",
	} {
		if _, err := pool.Exec(ctx, `DROP INDEX IF EXISTS `+name); err != nil {
			t.Fatal(err)
		}
	}

	const fleet = 80
	for i := 0; i < fleet; i++ {
		sid, wid := uuid.New(), uuid.New()
		ask := 0.05 + float64(i%10)*0.01
		if _, err := pool.Exec(ctx,
			`INSERT INTO suppliers (id,email,status,reputation,completed_tasks)
			 VALUES ($1,$2,'active',0.9,50)`, sid, "peer-"+uuid.NewString()+"@example.test"); err != nil {
			t.Fatal(err)
		}
		if _, err := pool.Exec(ctx,
			`INSERT INTO workers (id,supplier_id,hw_class,memory_gb,effective_memory_gb,
			                      last_seen_at,throttled,min_payout_usd_hr,sandboxed)
			 VALUES ($1,$2,'apple_silicon_base',32,32,now(),false,$3,true)`,
			wid, sid, ask); err != nil {
			t.Fatal(err)
		}
		bindLegacyTestWorkerExactExecutionIdentity(t, pool, ctx, wid)
		if _, err := pool.Exec(ctx,
			`INSERT INTO worker_authorized_capabilities
			   (worker_id,cell_id,runtime_id,job_type,model_ref,model_kind,matrix_sha256)
			 VALUES ($1,'cell','rt','embed','all-minilm-l6-v2','hf',$2)`,
			wid, generatedRuntimeMatrixSHA256); err != nil {
			t.Fatal(err)
		}
	}

	jobID := uuid.New()
	if _, err := pool.Exec(ctx, `
		INSERT INTO jobs (id,buyer_id,status,job_type,model_ref,input_ref,task_count,
		                  offered_rate_usd_hr,min_memory_gb,tier,currency)
		VALUES ($1,$2,'running','embed','all-minilm-l6-v2','in',1,10.0,0,'batch','usd')`,
		jobID, buyerID); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `
		INSERT INTO tasks (id,job_id,status,input_ref,result_key)
		VALUES ($1,$2,'queued','in','rk')`, uuid.New(), jobID); err != nil {
		t.Fatal(err)
	}

	// Load average is reported for the measure, not an SLO.
	// (uptime captured by the outer gate script; here we only EXPLAIN.)
	before, err := explainClaimExistsSubplan(ctx, pool, jobID, claimW, 5.0, generatedRuntimeMatrixSHA256)
	mustf(t, err, "explain before: %v")
	t.Logf("EXPLAIN BEFORE (fleet=%d):\n%s", fleet, before)

	if err := createClaimPeerIndexes(ctx, pool); err != nil {
		t.Fatal(err)
	}
	// Nudge planner stats.
	if _, err := pool.Exec(ctx, `ANALYZE workers; ANALYZE worker_authorized_capabilities;`); err != nil {
		t.Fatal(err)
	}

	after, err := explainClaimExistsSubplan(ctx, pool, jobID, claimW, 5.0, generatedRuntimeMatrixSHA256)
	mustf(t, err, "explain after: %v")
	t.Logf("EXPLAIN AFTER (fleet=%d):\n%s", fleet, after)

	// Plan-shape flip is the important result when it happens; at this fixture
	// size the planner may still choose seq scan. Record both plans; do not
	// invent a latency number or claim a flip that did not occur.
	beforeHasIdx := planMentionsAny(before, "workers_live_ask_seen_idx", "workers_live_hwclass_seen_idx",
		"worker_authorized_capabilities_fresh_supply_idx")
	afterHasIdx := planMentionsAny(after, "workers_live_ask_seen_idx", "workers_live_hwclass_seen_idx",
		"worker_authorized_capabilities_fresh_supply_idx")
	t.Logf("plan used new index before=%v after=%v", beforeHasIdx, afterHasIdx)
	if beforeHasIdx {
		t.Log("before plan already referenced a Step 18 index name (unexpected after DROP); recorded as-is")
	}
	// Residual honesty: EXISTS still evaluates job-relative predicates that no
	// static index can fully eliminate without changing claim semantics
	// (containment, residency, frozen runtime candidates, independence, ...).
	_ = time.Now()
}

func planMentionsAny(s string, needles ...string) bool {
	for _, n := range needles {
		if len(n) > 0 && (len(s) >= len(n)) {
			for i := 0; i+len(n) <= len(s); i++ {
				if s[i:i+len(n)] == n {
					return true
				}
			}
		}
	}
	return false
}

// Ensure openIsolatedTestStore import of context stays used when tests skip.
var _ = context.Background

// A buyer's declared data residency is a hard claim filter, not a preference.
//
// The claim SQL carries `j.data_residency IS NULL OR me.data_country = ANY(...)`
// (scheduler.go:894), and nothing asserted it. Deleting that line leaves every
// other test green, which is how a compliance filter becomes decorative: the
// buyer names the countries their data may be processed in, the receipt still
// records the job as claimed normally, and the work runs wherever capacity was.
//
// Registered as a network fault ("erase region restriction") against a live
// target, so it is a mutant rather than a deferred entry.
func TestClaimRefusesAWorkerOutsideTheBuyerDeclaredResidency(t *testing.T) {
	ctx, store, pool := openIsolatedTestStore(t)
	installSettlementCurrencyForTest(t, "usd")

	buyerID, err := store.CreateBuyerAccount(ctx,
		"residency-"+uuid.NewString()+"@example.test", "integration-password", 100)
	must(t, err)

	// One worker, in Germany.
	supplierID, workerID := uuid.New(), uuid.New()
	if _, err := pool.Exec(ctx,
		`INSERT INTO suppliers (id,email,status,reputation,completed_tasks,data_country)
		 VALUES ($1,$2,'active',0.95,100,'de')`,
		supplierID, "res-sup-"+uuid.NewString()+"@example.test"); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx,
		`INSERT INTO workers (id,supplier_id,hw_class,memory_gb,effective_memory_gb,
		                      last_seen_at,throttled,min_payout_usd_hr)
		 VALUES ($1,$2,'apple_silicon_max',64,64,now(),false,5.0)`,
		workerID, supplierID); err != nil {
		t.Fatal(err)
	}
	bindLegacyTestWorkerExactExecutionIdentity(t, pool, ctx, workerID)
	if _, err := pool.Exec(ctx,
		`INSERT INTO worker_authorized_capabilities
		   (worker_id,cell_id,runtime_id,job_type,model_ref,model_kind,matrix_sha256)
		 VALUES ($1,'cell','rt','embed','all-minilm-l6-v2','hf',$2)`,
		workerID, generatedRuntimeMatrixSHA256); err != nil {
		t.Fatal(err)
	}

	claimable := func(residency any, label string) bool {
		t.Helper()
		jobID, taskID := uuid.New(), uuid.New()
		if _, err := pool.Exec(ctx, `
			INSERT INTO jobs (id,buyer_id,status,job_type,model_ref,input_ref,task_count,
			                  offered_rate_usd_hr,min_memory_gb,tier,currency,data_residency)
			VALUES ($1,$2,'running','embed','all-minilm-l6-v2','in',1,10.0,0,'batch','usd',$3)`,
			jobID, buyerID, residency); err != nil {
			t.Fatalf("%s: seed job: %v", label, err)
		}
		if _, err := pool.Exec(ctx, `
			INSERT INTO tasks (id,job_id,status,input_ref,result_key,visible_at)
			VALUES ($1,$2,'queued','in','rk',now())`, taskID, jobID); err != nil {
			t.Fatalf("%s: seed task: %v", label, err)
		}
		got, err := store.ClaimTasksTx(ctx, WorkerAuth{WorkerID: workerID, SupplierID: supplierID})
		if err != nil {
			t.Fatalf("%s: claim: %v", label, err)
		}
		return got != nil
	}

	// Control: with no residency restriction the German worker must claim.
	// Without this the refusal below could be any unrelated ineligibility and
	// the test would pass while proving nothing.
	if !claimable(nil, "unrestricted") {
		t.Fatal("the worker could not claim an unrestricted job, so the residency " +
			"refusal below would not be evidence about residency")
	}

	// A job restricted to France and Ireland must not be claimable from Germany.
	if claimable([]string{"fr", "ie"}, "restricted") {
		t.Fatal("a worker in 'de' claimed a job the buyer restricted to fr/ie; " +
			"the claim path is not enforcing declared data residency")
	}
}
