package main

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
)

func TestUnquotedJobDurablePerformanceExpiryReturns503WithZeroWrites(t *testing.T) {
	strangerDeploymentInputs(t)
	installSettlementCurrencyForTest(t, "usd")
	installBoundCataloguePublicationAuthorityForTest(t)
	installTestOnlyCombinedTokenAuthority(t)
	pinBoardClockForPublication(t)

	artifacts := newArtifactHarness(t)
	ctx, store, pool := openIsolatedTestStore(t)
	schedule, err := BuildCataloguePriceSchedule()
	mustf(t, err, "build physical catalogue schedule: %v")
	_, err = store.ApplyRepricing(ctx, schedule)
	mustf(t, err, "publish physical catalogue schedule: %v")
	mustf(t, seedDemo(ctx, pool, artifacts.storage), "seed verification floor: %v")

	sub := testOnlyCombinedTokenSubmit(t)
	workload, err := buildWorkloadDecision(sub, strings.Repeat("a", 64))
	mustf(t, err, "build performance-expiry workload: %v")
	_, performance, err := admissionUnitsPerSec(
		workload.RuntimeJobType,
		workload.Binding.Model.Ref,
		admissionCellsForWorkload(workload),
		time.Now(),
	)
	mustf(t, err, "resolve current performance authority: %v")
	measuredAt, err := time.Parse(time.RFC3339, performance.BenchmarkedAt)
	mustf(t, err, "parse current performance measurement time: %v")

	beforeExpiry := measuredAt.Add(time.Hour)
	afterExpiry := measuredAt.Add(benchmarkRevalidationWindow + time.Nanosecond)
	previousClock := runtimeCellPerformanceNow
	previousHook := durableAdmissionPhysicalRecheckHook
	runtimeCellPerformanceNow = func() time.Time { return beforeExpiry }
	durableAdmissionPhysicalRecheckHook = func() {
		runtimeCellPerformanceNow = func() time.Time { return afterExpiry }
		durableAdmissionPhysicalRecheckHook = nil
	}
	t.Cleanup(func() {
		runtimeCellPerformanceNow = previousClock
		durableAdmissionPhysicalRecheckHook = previousHook
	})

	buyerID := uuid.New()
	_, err = pool.Exec(ctx,
		`INSERT INTO buyers (id,email,password_hash,free_credit_usd) VALUES ($1,$2,'x',5)`,
		buyerID, "performance-expiry-job-"+buyerID.String()+"@test",
	)
	mustf(t, err, "insert buyer: %v")
	var jobsBefore int
	mustf(t, pool.QueryRow(ctx, `SELECT count(*) FROM jobs`).Scan(&jobsBefore),
		"count jobs before expired-performance submission: %v")

	body, err := json.Marshal(testOnlyBatchPublicRequest(strangerBatchCorpus, 1))
	mustf(t, err, "marshal unquoted job submission: %v")
	req := httptest.NewRequest(http.MethodPost, "/v1/jobs", bytes.NewReader(body))
	req.Header.Set("Idempotency-Key", uuid.NewString())
	req = req.WithContext(context.WithValue(req.Context(), ctxBuyer, &AuthResult{BuyerID: buyerID}))
	recorder := httptest.NewRecorder()
	verifier := NewVerifier(store).WithStorage(artifacts.storage)
	NewServer(store, artifacts.storage, verifier, nil).handleCreateJob(recorder, req)

	if recorder.Code != http.StatusServiceUnavailable {
		t.Fatalf("expired runtime performance authority status=%d, want 503: %s",
			recorder.Code, recorder.Body.String())
	}
	for _, want := range []string{"physical authority unavailable", "durable placement ingress"} {
		if !strings.Contains(recorder.Body.String(), want) {
			t.Fatalf("expired runtime performance refusal omits %q: %s", want, recorder.Body.String())
		}
	}
	var jobsAfter int
	mustf(t, pool.QueryRow(ctx, `SELECT count(*) FROM jobs`).Scan(&jobsAfter),
		"count jobs after expired-performance submission: %v")
	if jobsAfter != jobsBefore {
		t.Fatalf("expired runtime performance submission wrote %d job rows", jobsAfter-jobsBefore)
	}
}
