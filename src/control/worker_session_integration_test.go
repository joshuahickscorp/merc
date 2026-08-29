package main

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"
)

func TestWorkerRegistrationPersistsProcessSessionTransitions(t *testing.T) {
	// TEST_ONLY publication before store open so enrolment projects activated
	// cells; match the synthetic exact physical identity on the worker.
	installBoundCataloguePublicationAuthorityForTest(t)
	ctx, store, pool := openAdminMutationTestStore(t)
	installed := currentActivation()
	activeRuntimeActivation.Store(newRuntimeActivation(
		installed.PolicyRevision, map[string]string{}, nil))
	buyerID := uuid.New()
	supplierID := uuid.New()
	workerID := uuid.New()
	firstSession := uuid.New()
	secondSession := uuid.New()
	t.Cleanup(func() {
		cleanupCtx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
		defer cancel()
		for _, statement := range []struct {
			sql  string
			args []any
		}{
			{`DELETE FROM benchmark_results WHERE worker_id=$1`, []any{workerID}},
			{`DELETE FROM worker_tps_cache WHERE worker_id=$1`, []any{workerID}},
			{`DELETE FROM workers WHERE id=$1`, []any{workerID}},
			{`DELETE FROM suppliers WHERE id=$1`, []any{supplierID}},
			{`DELETE FROM buyers WHERE id=$1`, []any{buyerID}},
		} {
			if _, err := pool.Exec(cleanupCtx, statement.sql, statement.args...); err != nil {
				t.Errorf("clean worker-session fixture: %v", err)
			}
		}
	})

	if _, err := pool.Exec(ctx, `
		INSERT INTO buyers (id,email,free_credit_usd)
		VALUES ($1,$2,10)`,
		buyerID, "worker-session-"+buyerID.String()+"@example.invalid",
	); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `
		INSERT INTO suppliers (id,email,owner_buyer_id,status)
		VALUES ($1,$2,$3,'active')`,
		supplierID, "worker-session-supplier-"+supplierID.String()+"@example.invalid", buyerID,
	); err != nil {
		t.Fatal(err)
	}

	capability := productionMetalCapability()
	capability.WorkerID = workerID
	capability.SupplierID = supplierID
	capability.AgentVersion = "0.1.0"
	capability.HWClass = "apple_silicon_pro"
	capability.BuildHash = testOnlyPublicationBuildHash
	capability.BuildIdentityPolicy = currentEngineBuildIdentityPolicy
	capability.HardwareIdentity = testOnlyPublicationHardware
	capability.Benchmarks = []BenchResult{
		{JobType: "embed", ModelID: "all-minilm-l6-v2", EPS: 3000, ThermalOK: true,
			Unit: "token_like_input_units", UnitScope: performanceUnitScopeTokenLikeInputGeometry,
			MeasuredUnix: uint64(runtimeCellPerformanceNow().Unix())},
		{JobType: "batch_infer", ModelID: "llama-3.2-1b-instruct-q4", TPS: 200, ThermalOK: true,
			Unit: "tokens", UnitScope: performanceUnitScopeTokenLikeInputPlusOutputTokens,
			MeasuredUnix: uint64(runtimeCellPerformanceNow().Unix())},
	}
	capability.AgentSessionID = &firstSession
	mustf(t, store.UpsertWorker(ctx, capability), "first registration: %v")

	var stored uuid.UUID
	var firstStarted time.Time
	if err := pool.QueryRow(ctx, `
		SELECT agent_session_id,agent_session_started_at
		  FROM workers WHERE id=$1`, workerID,
	).Scan(&stored, &firstStarted); err != nil {
		t.Fatal(err)
	}
	if stored != firstSession || firstStarted.IsZero() {
		t.Fatalf("first process session not persisted: id=%s started=%s", stored, firstStarted)
	}

	time.Sleep(10 * time.Millisecond)
	mustf(t, store.UpsertWorker(ctx, capability), "same-session registration: %v")
	var sameStarted time.Time
	if err := pool.QueryRow(ctx, `
		SELECT agent_session_started_at FROM workers WHERE id=$1`, workerID,
	).Scan(&sameStarted); err != nil {
		t.Fatal(err)
	}
	if !sameStarted.Equal(firstStarted) {
		t.Fatalf("same process reset session start: before=%s after=%s", firstStarted, sameStarted)
	}

	time.Sleep(10 * time.Millisecond)
	capability.AgentSessionID = &secondSession
	mustf(t, store.UpsertWorker(ctx, capability), "second-process registration: %v")
	var secondStarted time.Time
	if err := pool.QueryRow(ctx, `
		SELECT agent_session_id,agent_session_started_at
		  FROM workers WHERE id=$1`, workerID,
	).Scan(&stored, &secondStarted); err != nil {
		t.Fatal(err)
	}
	if stored != secondSession || !secondStarted.After(firstStarted) {
		t.Fatalf("new process session transition not persisted: id=%s before=%s after=%s",
			stored, firstStarted, secondStarted)
	}
}
