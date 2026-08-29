package main

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
)

// Pin the warmth consumers' current behaviour against a fresh measured row.
// These SQL predicates are the contract scheduler/planner/quote/watchdog share;
// residency measurements must not change what "warm within 60s" means for them.

func TestWarmthConsumersTreatFreshStateAsWarm(t *testing.T) {
	ctx, store, pool := openPayoutTestStore(t)
	workerID := seedResidencyTestWorker(t, ctx, store, pool)
	modelID := "all-minilm-l6-v2"
	seedMeasuredWarmResidency(t, ctx, pool, workerID, modelID)

	// quote.go:1054-1071 — WarmEligibleWorkerCountFor
	n, err := store.WarmEligibleWorkerCount(ctx, "embed", modelID, 0)
	must(t, err)
	if n < 1 {
		t.Fatalf("fresh measured warm row not counted by quote: n=%d", n)
	}

	// planner.go:236-256 via FleetRateSnapshot — Warm flag
	rows, err := store.FleetRateSnapshot(ctx, "embed", modelID, 0)
	must(t, err)
	warm := false
	for _, r := range rows {
		if r.WorkerID == workerID && r.Warm {
			warm = true
			break
		}
	}
	if !warm {
		t.Fatalf("fresh measured warm row not Warm in fleet snapshot: %+v", rows)
	}

	// latency_watchdog.go:19-38 — cold-straggler predicate is the inverse of warm
	// EXISTS. A fresh row means the worker is NOT a cold-model straggler.
	taskID := seedRunningTaskForWorker(t, ctx, pool, workerID, "embed", modelID)
	cold, err := store.isColdModelStraggler(ctx, taskID)
	must(t, err)
	if cold {
		t.Fatal("fresh measured warm row classified worker as cold-model straggler")
	}

	// scheduler warm_for_task predicate (scheduler.go:560-570) — same EXISTS shape
	var schedulerWarm bool
	if err := pool.QueryRow(ctx, `
		SELECT (COALESCE($2::text,'') <> '' AND EXISTS (
			SELECT 1 FROM worker_model_state wms
			 WHERE wms.worker_id = $1 AND wms.model_id = $2
			   AND wms.last_seen_warm > now() - interval '60 seconds'
		))`, workerID, modelID).Scan(&schedulerWarm); err != nil {
		t.Fatal(err)
	}
	if !schedulerWarm {
		t.Fatal("fresh measured warm row failed the scheduler warm_for_task predicate")
	}
}

func TestEvictedModelStopsBeingWarmWithinOneHeartbeat(t *testing.T) {
	ctx, store, pool := openPayoutTestStore(t)
	workerID := seedResidencyTestWorker(t, ctx, store, pool)
	modelID := "llama-3.2-1b-instruct-q4"

	// First heartbeat: model is resident with measurements.
	if err := store.HeartbeatTx(ctx, workerID, WorkerResources{
		ResidentModels: []ResidentModel{{
			ModelID: modelID, RSSDeltaBytes: 200 * 1024 * 1024, LoadMS: 2_500,
		}},
	}); err != nil {
		t.Fatal(err)
	}
	n, err := store.WarmEligibleWorkerCount(ctx, "batch_infer", modelID, 0)
	if err != nil || n < 1 {
		t.Fatalf("pre-eviction warm count=%d err=%v", n, err)
	}

	// Second heartbeat: agent reports the idle eviction. Control must DELETE the
	// row so warmth consumers stop ranking the worker as warm immediately —
	// not after the 60s last_seen_warm TTL.
	if err := store.HeartbeatTx(ctx, workerID, WorkerResources{
		EvictedModels: []string{modelID},
	}); err != nil {
		t.Fatal(err)
	}
	var rows int
	if err := pool.QueryRow(ctx,
		`SELECT count(*) FROM worker_model_state WHERE worker_id=$1 AND model_id=$2`,
		workerID, modelID).Scan(&rows); err != nil {
		t.Fatal(err)
	}
	if rows != 0 {
		t.Fatalf("evicted model still has worker_model_state row (count=%d)", rows)
	}
	n, err = store.WarmEligibleWorkerCount(ctx, "batch_infer", modelID, 0)
	must(t, err)
	if n != 0 {
		t.Fatalf("evicted model still counted warm by quote: n=%d", n)
	}
	var schedulerWarm bool
	if err := pool.QueryRow(ctx, `
		SELECT EXISTS (
			SELECT 1 FROM worker_model_state wms
			 WHERE wms.worker_id = $1 AND wms.model_id = $2
			   AND wms.last_seen_warm > now() - interval '60 seconds'
		)`, workerID, modelID).Scan(&schedulerWarm); err != nil {
		t.Fatal(err)
	}
	if schedulerWarm {
		t.Fatal("evicted model still warm under scheduler predicate")
	}
}

func TestServiceLeaseOfferCannotAdvertiseMoreWarmThanMeasured(t *testing.T) {
	installSettlementCurrencyForTest(t, "usd")
	ctx, store, pool := openPayoutTestStore(t)
	profile := sortedVLLMProfiles()[0]
	worker, _ := newFabricMeasurementWorker(t, ctx, store)
	offer := serviceLeaseOffer(profile)
	offer.Region = "ca-measured-" + uuid.NewString()
	offer.MaximumWarmReplicas, offer.AvailableWarmReplicas = 3, 3

	// Unmeasurable: no worker_model_state row → available must be zero.
	must(t, store.UpsertServiceLeaseOffer(ctx, worker, offer))
	var available int
	if err := pool.QueryRow(ctx, `SELECT available_warm_replicas FROM service_lease_worker_offers
		WHERE worker_id=$1 AND runtime_profile_id=$2 AND region=$3`,
		worker.WorkerID, profile.RuntimeProfileID, offer.Region).Scan(&available); err != nil {
		t.Fatal(err)
	}
	if available != 0 {
		t.Fatalf("unmeasurable offer advertised available=%d, want 0", available)
	}

	// Measured warm for the profile model unlocks the declared capacity.
	seedMeasuredWarmResidency(t, ctx, pool, worker.WorkerID, profile.ModelAlias)
	must(t, store.UpsertServiceLeaseOffer(ctx, worker, offer))
	if err := pool.QueryRow(ctx, `SELECT available_warm_replicas FROM service_lease_worker_offers
		WHERE worker_id=$1 AND runtime_profile_id=$2 AND region=$3`,
		worker.WorkerID, profile.RuntimeProfileID, offer.Region).Scan(&available); err != nil {
		t.Fatal(err)
	}
	if available != 3 {
		t.Fatalf("measured offer available=%d, want declared 3", available)
	}

	// Declared-only warmth (last_seen_warm without measurements) is not enough.
	if _, err := pool.Exec(ctx, `UPDATE worker_model_state
		SET rss_delta_bytes=NULL, load_ms=NULL WHERE worker_id=$1 AND model_id=$2`,
		worker.WorkerID, profile.ModelAlias); err != nil {
		t.Fatal(err)
	}
	must(t, store.UpsertServiceLeaseOffer(ctx, worker, offer))
	if err := pool.QueryRow(ctx, `SELECT available_warm_replicas FROM service_lease_worker_offers
		WHERE worker_id=$1 AND runtime_profile_id=$2 AND region=$3`,
		worker.WorkerID, profile.RuntimeProfileID, offer.Region).Scan(&available); err != nil {
		t.Fatal(err)
	}
	if available != 0 {
		t.Fatalf("declared-only (unmeasured) warmth advertised available=%d, want 0", available)
	}
}

func TestOutOfRangeResidencyIsRefusedNotClamped(t *testing.T) {
	// Residency bounds need one admissible model for the positive controls. The
	// production projection is honestly empty, so use the scoped TEST_ONLY
	// combined-token model without changing checked-in evidence.
	sub := testOnlyCombinedTokenSubmit(t)
	modelID := sub.Model.Ref
	// Pure validation: no database required.
	overRSS := ResidentModel{
		ModelID: modelID,
		// One byte past the operational ceiling.
		RSSDeltaBytes: maxResidencyRSSDeltaBytes + 1,
		LoadMS:        100,
	}
	if err := validateHeartbeatResidentModels([]ResidentModel{overRSS}); err == nil {
		t.Fatal("rss_delta_bytes above operational max was accepted")
	}
	underRSS := overRSS
	underRSS.RSSDeltaBytes = -maxResidencyRSSDeltaBytes - 1
	if err := validateHeartbeatResidentModels([]ResidentModel{underRSS}); err == nil {
		t.Fatal("rss_delta_bytes below operational min was accepted")
	}
	overLoad := ResidentModel{
		ModelID:       modelID,
		RSSDeltaBytes: 1,
		LoadMS:        maxBenchmarkLoadMS + 1,
	}
	if err := validateHeartbeatResidentModels([]ResidentModel{overLoad}); err == nil {
		t.Fatal("load_ms above 24h operational max was accepted")
	}

	// In-range values still pass, including a negative delta inside the bound
	// (RSS noise around a small model is real; clamping it to zero would invent data).
	// Only models with a bindable advertised cell (or a service-lease alias) may
	// be warm. The positive control therefore uses the explicitly scoped model.
	if err := validateHeartbeatResidentModels([]ResidentModel{{
		ModelID: modelID, RSSDeltaBytes: -1024, LoadMS: 50,
	}}); err != nil {
		t.Fatalf("in-range residency rejected: %v", err)
	}
	if err := validateHeartbeatResidentModels([]ResidentModel{{
		ModelID: modelID, RSSDeltaBytes: maxResidencyRSSDeltaBytes, LoadMS: maxBenchmarkLoadMS,
	}}); err != nil {
		t.Fatalf("in-range residency at operational max rejected: %v", err)
	}

	// Store path also refuses rather than writing a clamped figure.
	ctx, store, pool := openPayoutTestStore(t)
	workerID := seedResidencyTestWorker(t, ctx, store, pool)
	err := store.HeartbeatTx(ctx, workerID, WorkerResources{
		ResidentModels: []ResidentModel{{
			ModelID: modelID, RSSDeltaBytes: maxResidencyRSSDeltaBytes + 1, LoadMS: 10,
		}},
	})
	if err == nil {
		t.Fatal("HeartbeatTx accepted out-of-range rss_delta_bytes")
	}
	var rows int
	if err := pool.QueryRow(ctx,
		`SELECT count(*) FROM worker_model_state WHERE worker_id=$1`, workerID).Scan(&rows); err != nil {
		t.Fatal(err)
	}
	if rows != 0 {
		t.Fatalf("out-of-range residency still wrote %d worker_model_state row(s)", rows)
	}
}

func TestHeartbeatPersistsMeasuredResidency(t *testing.T) {
	ctx, store, pool := openPayoutTestStore(t)
	workerID := seedResidencyTestWorker(t, ctx, store, pool)
	modelID := "all-minilm-l6-v2"
	const wantRSS int64 = 50 * 1024 * 1024
	const wantLoad int64 = 1234
	if err := store.HeartbeatTx(ctx, workerID, WorkerResources{
		ResidentModels: []ResidentModel{{
			ModelID: modelID, RSSDeltaBytes: wantRSS, LoadMS: uint64(wantLoad),
		}},
	}); err != nil {
		t.Fatal(err)
	}
	var gotRSS, gotLoad int64
	var lastSeen time.Time
	if err := pool.QueryRow(ctx, `
		SELECT rss_delta_bytes, load_ms, last_seen_warm
		  FROM worker_model_state WHERE worker_id=$1 AND model_id=$2`,
		workerID, modelID).Scan(&gotRSS, &gotLoad, &lastSeen); err != nil {
		t.Fatal(err)
	}
	if gotRSS != wantRSS || gotLoad != wantLoad {
		t.Fatalf("stored residency rss=%d load=%d, want rss=%d load=%d", gotRSS, gotLoad, wantRSS, wantLoad)
	}
	if time.Since(lastSeen) > 5*time.Second {
		t.Fatalf("last_seen_warm not refreshed: %v", lastSeen)
	}
}

func seedResidencyTestWorker(t *testing.T, ctx context.Context, store *Store, pool *pgxpool.Pool) uuid.UUID {
	t.Helper()
	supplierID, workerID := uuid.New(), uuid.New()
	if _, err := pool.Exec(ctx, `INSERT INTO suppliers (id,email,status) VALUES ($1,$2,'active')`,
		supplierID, "residency-"+uuid.NewString()+"@example.test"); err != nil {
		t.Fatal(err)
	}
	// This worker is deliberately claimable: fresh last_seen_at, authorized for
	// both production models, thermally fine. That makes it visible to every
	// other test in the package, because they share one database — and the batch
	// claim predicate defers a task whenever some other eligible worker is
	// online. Left behind, it silently changes what a placement test observes.
	// Take it offline rather than delete it: capabilities, tps cache and warm
	// rows all reference workers ON DELETE RESTRICT, so a DELETE here can only
	// fail. Eligibility is last_seen_at within sixty seconds, so ageing the
	// heartbeat is the same thing the fleet does when a worker stops, and it
	// cannot fail on a foreign key.
	t.Cleanup(func() {
		cleanupCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if _, err := pool.Exec(cleanupCtx,
			`UPDATE workers SET last_seen_at = now() - interval '1 hour' WHERE id=$1`,
			workerID); err != nil {
			t.Errorf("residency test worker not taken offline; it stays eligible for "+
				"every later test in this package: %v", err)
		}
	})
	if _, err := store.CreateWorkerToken(ctx, workerID, supplierID); err != nil {
		t.Fatal(err)
	}
	// Heartbeat and claimable predicates need last_seen_at fresh and a live row.
	// Pin min_payout high so this fixture cannot appear as a "cheaper asking
	// worker" to scheduler claim tests that share the package DB — a fresh
	// embed-capable worker with the default 0 floor was the order-dependent
	// polluter of TestClaimTasksTxDefersToACheaperAskingWorker.
	if _, err := pool.Exec(ctx, `UPDATE workers SET last_seen_at=now(), memory_gb=16,
		effective_memory_gb=16, hw_class='cpu', thermal_ok=true, min_payout_usd_hr=1e9 WHERE id=$1`, workerID); err != nil {
		t.Fatal(err)
	}
	bindWorkerToGovernedProfile(t, pool, ctx, workerID)
	// Authorize the two production models so FleetRateSnapshot / warm counts
	// that join worker_authorized_capabilities still see the worker.
	for _, model := range []struct {
		job, model, kind string
	}{
		{"embed", "all-minilm-l6-v2", "hf"},
		{"batch_infer", "llama-3.2-1b-instruct-q4", "gguf"},
	} {
		if _, err := pool.Exec(ctx, `
			INSERT INTO worker_authorized_capabilities
			  (worker_id, cell_id, runtime_id, job_type, model_ref, model_kind, matrix_sha256, routable)
			VALUES ($1,$2,'rt',$3,$4,$5,$6,true)`,
			workerID, "cell-"+model.model, model.job, model.model, model.kind, generatedRuntimeMatrixSHA256); err != nil {
			t.Fatalf("authorize %s: %v", model.model, err)
		}
	}
	if _, err := pool.Exec(ctx, `
		INSERT INTO worker_tps_cache (worker_id, job_type, tps, updated_at)
		VALUES ($1,'embed',100,now()), ($1,'batch_infer',50,now())
		ON CONFLICT (worker_id, job_type) DO UPDATE SET tps=EXCLUDED.tps, updated_at=now()`,
		workerID); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		c, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cancel()
		_, _ = pool.Exec(c, `DELETE FROM worker_model_state WHERE worker_id=$1`, workerID)
		_, _ = pool.Exec(c, `DELETE FROM worker_tps_cache WHERE worker_id=$1`, workerID)
		_, _ = pool.Exec(c, `DELETE FROM worker_authorized_capabilities WHERE worker_id=$1`, workerID)
		_, _ = pool.Exec(c, `DELETE FROM worker_tokens WHERE worker_id=$1`, workerID)
		_, _ = pool.Exec(c, `DELETE FROM workers WHERE id=$1`, workerID)
		_, _ = pool.Exec(c, `DELETE FROM suppliers WHERE id=$1`, supplierID)
	})
	return workerID
}

func seedRunningTaskForWorker(t *testing.T, ctx context.Context, pool *pgxpool.Pool, workerID uuid.UUID, jobType, modelRef string) uuid.UUID {
	t.Helper()
	buyerID, jobID, taskID := uuid.New(), uuid.New(), uuid.New()
	if _, err := pool.Exec(ctx, `INSERT INTO buyers (id,email) VALUES ($1,$2)`,
		buyerID, buyerID.String()+"@residency.invalid"); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `
		INSERT INTO jobs (id, buyer_id, status, job_type, model_ref, input_ref, task_count, tier)
		VALUES ($1,$2,'running',$3,$4,'in',1,'batch')`,
		jobID, buyerID, jobType, modelRef); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `
		INSERT INTO tasks (id, job_id, worker_id, status, started_at, claimed_by, claimed_at, retry_count, input_ref, result_key)
		VALUES ($1,$2,$3,'running',now(),$3,now(),0,'in','rk')`,
		taskID, jobID, workerID); err != nil {
		t.Fatal(err)
	}
	return taskID
}
