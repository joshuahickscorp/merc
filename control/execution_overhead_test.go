package main

import (
	"context"
	"math"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
)

func overheadRows(t *testing.T, pool *pgxpool.Pool, ctx context.Context,
	jobID uuid.UUID) map[string]OverheadRow {
	t.Helper()
	rows, err := pool.Query(ctx,
		`SELECT overhead_class, tasks, attempts, tokens,
		        measured_supplier_usd, avoided_estimate_usd,
		        runtime_profile_id, hw_class, job_terminal_status
		   FROM execution_overhead_actuals WHERE job_id=$1`, jobID)
	if err != nil {
		t.Fatalf("read overhead: %v", err)
	}
	defer rows.Close()
	out := map[string]OverheadRow{}
	for rows.Next() {
		var r OverheadRow
		var status string
		var tasks, attempts int
		if err := rows.Scan(&r.OverheadClass, &tasks, &attempts, &r.Tokens,
			&r.MeasuredSupplierUSD, &r.AvoidedEstimateUSD,
			&r.RuntimeProfileID, &r.HWClass, &status); err != nil {
			t.Fatalf("scan overhead: %v", err)
		}
		r.Tasks, r.Attempts = int64(tasks), int64(attempts)
		out[r.OverheadClass] = r
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("overhead rows: %v", err)
	}
	return out
}

// The reason this table exists: a lane can have a perfectly calibrated base
// planner while the cost of a delivered outcome on it is far higher, and
// plan_actuals is designed not to see that.
func TestExecutionOverheadSeparatesEveryCostClass(t *testing.T) {
	store, pool, ctx := planActualsTestStore(t)
	jobType := "ovh_" + uuid.NewString()[:8]

	// Two primaries (one retried twice), a hedge of chunk 1, a honeypot, a
	// redundancy tiebreak that is ALSO a hedge, one failed and one cancelled.
	jobID := planActualsFixtureJob(t, pool, ctx, jobType, 2, 2000, 1.00, 1.00,
		[]planActualsFixtureTask{
			{status: "complete", tokens: 500, chunkIndex: 0, explicitChunk: true,
				runtimeID: "candle_metal", hwClass: "apple_silicon_ultra"},
			{status: "complete", tokens: 500, retries: 2, chunkIndex: 1, explicitChunk: true,
				runtimeID: "candle_metal", hwClass: "apple_silicon_ultra"},
			{status: "complete", tokens: 500, chunkIndex: 1, explicitChunk: true,
				hedgedFromChunk: chunk(1),
				runtimeID:       "candle_metal", hwClass: "apple_silicon_ultra"},
			{status: "complete", honeypot: true, tokens: 100, chunkIndex: 2, explicitChunk: true,
				runtimeID: "candle_metal", hwClass: "apple_silicon_ultra"},
			// A tiebreak: redundancy AND hedged. Verification wins the priority,
			// so it must not also be counted as a hedge.
			{status: "complete", redundancy: true, tokens: 100, chunkIndex: 0, explicitChunk: true,
				hedgedFromChunk: chunk(0),
				runtimeID:       "candle_metal", hwClass: "apple_silicon_ultra"},
			{status: "failed", chunkIndex: 3, explicitChunk: true,
				runtimeID: "candle_metal", hwClass: "apple_silicon_ultra"},
			{status: "cancelled", chunkIndex: 4, explicitChunk: true,
				runtimeID: "candle_metal", hwClass: "apple_silicon_ultra"},
		}, planActualsFixtureOptions{})

	if err := store.RecordExecutionOverhead(ctx, jobID); err != nil {
		t.Fatalf("RecordExecutionOverhead: %v", err)
	}
	got := overheadRows(t, pool, ctx, jobID)

	for _, want := range []struct {
		class           string
		tasks, attempts int64
		tokens          int64
	}{
		// honeypot + redundancy tiebreak. The tiebreak is verification work even
		// though it is also a hedge; counting it twice would overstate the cost
		// of a heavily-verified lane.
		{overheadClassVerification, 2, 2, 200},
		// Only the plain hedge, not the tiebreak.
		{overheadClassHedge, 1, 1, 500},
		{overheadClassFailed, 1, 1, 0},
		{overheadClassCancelled, 1, 1, 0},
		// One task carried two extra attempts. Rows, not attempts, is the wrong
		// reading here: this class measures attempts.
		{overheadClassRetry, 1, 2, 0},
	} {
		row, ok := got[want.class]
		if !ok {
			t.Errorf("%s missing", want.class)
			continue
		}
		if row.Tasks != want.tasks || row.Attempts != want.attempts || row.Tokens != want.tokens {
			t.Errorf("%s: tasks/attempts/tokens = %d/%d/%d, want %d/%d/%d",
				want.class, row.Tasks, row.Attempts, row.Tokens,
				want.tasks, want.attempts, want.tokens)
		}
		if row.AvoidedEstimateUSD != 0 {
			t.Errorf("%s carries an avoided estimate; that column is CACHE_AVOIDED only",
				want.class)
		}
	}
	if _, present := got[overheadClassCacheAvoided]; present {
		t.Error("a distributed job produced a CACHE_AVOIDED row")
	}

	// The four partitioning classes must account for every non-primary row
	// exactly once: 2 verification + 1 hedge + 1 failed + 1 cancelled = 5 of the
	// 7 task rows, leaving the 2 primaries.
	var partitioned int64
	for _, class := range []string{overheadClassVerification, overheadClassHedge,
		overheadClassFailed, overheadClassCancelled} {
		partitioned += got[class].Tasks
	}
	if partitioned != 5 {
		t.Fatalf("partitioning classes cover %d task rows, want 5 of 7 (2 are primary)",
			partitioned)
	}
}

// A class with nothing in it carries no information. Writing it anyway would
// triple the table and make "we measured zero" indistinguishable from
// "we recorded a placeholder".
func TestExecutionOverheadWritesNothingForACleanJob(t *testing.T) {
	store, pool, ctx := planActualsTestStore(t)
	jobType := "ovh_" + uuid.NewString()[:8]
	jobID := planActualsFixtureJob(t, pool, ctx, jobType, 2, 2000, 1.00, 1.00,
		[]planActualsFixtureTask{
			{status: "complete", tokens: 500, runtimeID: "candle_metal", hwClass: "apple_silicon_ultra"},
			{status: "complete", tokens: 500, runtimeID: "candle_metal", hwClass: "apple_silicon_ultra"},
		}, planActualsFixtureOptions{})
	if err := store.RecordExecutionOverhead(ctx, jobID); err != nil {
		t.Fatalf("RecordExecutionOverhead: %v", err)
	}
	if rows := overheadRows(t, pool, ctx, jobID); len(rows) != 0 {
		t.Fatalf("a clean job wrote %d overhead rows, want 0: %v", len(rows), rows)
	}
}

// Failed and cancelled jobs are exactly where the cost this table exists to
// surface lives. plan_actuals refuses them; this must not.
func TestExecutionOverheadRecordsFailedAndCancelledJobs(t *testing.T) {
	store, pool, ctx := planActualsTestStore(t)
	for _, status := range []string{"failed", "cancelled"} {
		jobType := "ovh_" + uuid.NewString()[:8]
		jobID := planActualsFixtureJob(t, pool, ctx, jobType, 2, 2000, 1.00, 0,
			[]planActualsFixtureTask{
				{status: "failed", runtimeID: "candle_metal", hwClass: "apple_silicon_ultra"},
			}, planActualsFixtureOptions{jobStatus: status})
		if err := store.RecordExecutionOverhead(ctx, jobID); err != nil {
			t.Fatalf("RecordExecutionOverhead(%s): %v", status, err)
		}
		rows := overheadRows(t, pool, ctx, jobID)
		if len(rows) == 0 {
			t.Errorf("a %s job produced no overhead rows", status)
		}

		// And plan_actuals must still refuse it, so the two datasets stay apart.
		if err := store.RecordPlanActuals(ctx, jobID); err != nil {
			t.Fatalf("RecordPlanActuals(%s): %v", status, err)
		}
		var baseRows int
		if err := pool.QueryRow(ctx,
			`SELECT COUNT(*) FROM plan_actuals WHERE job_id=$1`, jobID).Scan(&baseRows); err != nil {
			t.Fatalf("count plan_actuals: %v", err)
		}
		if baseRows != 0 {
			t.Errorf("a %s job wrote %d plan_actuals rows; base actuals must stay clean",
				status, baseRows)
		}
	}
}

// A cache hit spends no physical compute. The saving is real and its value is
// counterfactual, so it lands in a column that says so.
func TestExecutionOverheadRecordsCacheSavingAsAnEstimate(t *testing.T) {
	store, pool, ctx := planActualsTestStore(t)
	jobID := uuid.New()
	if _, err := pool.Exec(ctx,
		`INSERT INTO jobs (id, buyer_id, status, job_type, tier, model_ref, input_ref,
		                   actual_usd, compute_plan)
		 VALUES ($1,$2,'complete','batch_infer','batch','plan-actuals-model','in',0.5,$3)`,
		jobID, uuid.New(), map[string]any{
			"execution_mode":          "exact_result_reuse",
			"base_compute_usd":        2.25,
			"estimated_output_tokens": 1000,
		}); err != nil {
		t.Fatalf("insert reuse job: %v", err)
	}
	t.Cleanup(func() {
		c, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		_, _ = pool.Exec(c, `DELETE FROM execution_overhead_actuals WHERE job_id=$1`, jobID)
		_, _ = pool.Exec(c, `DELETE FROM plan_actuals WHERE job_id=$1`, jobID)
		_, _ = pool.Exec(c, `DELETE FROM jobs WHERE id=$1`, jobID)
	})
	if err := store.RecordExecutionOverhead(ctx, jobID); err != nil {
		t.Fatalf("RecordExecutionOverhead: %v", err)
	}
	row, ok := overheadRows(t, pool, ctx, jobID)[overheadClassCacheAvoided]
	if !ok {
		t.Fatal("a cache hit produced no CACHE_AVOIDED row")
	}
	if math.Abs(row.AvoidedEstimateUSD-2.25) > 0.000001 {
		t.Errorf("avoided_estimate_usd = %v, want the frozen 2.25", row.AvoidedEstimateUSD)
	}
	if row.MeasuredSupplierUSD != 0 {
		t.Errorf("a cache hit reported %v measured supplier cost; nothing was spent",
			row.MeasuredSupplierUSD)
	}
}

// The sweep must find terminal jobs exactly once. Running it twice against the
// same job would double every overhead figure it reports.
func TestExecutionOverheadSweepIsIdempotent(t *testing.T) {
	store, pool, ctx := planActualsTestStore(t)
	jobType := "ovh_" + uuid.NewString()[:8]
	jobID := planActualsFixtureJob(t, pool, ctx, jobType, 1, 1000, 1.00, 1.00,
		[]planActualsFixtureTask{
			{status: "failed", runtimeID: "candle_metal", hwClass: "apple_silicon_ultra"},
		}, planActualsFixtureOptions{})

	pending, err := store.JobsMissingOverheadActuals(ctx, 500)
	if err != nil {
		t.Fatalf("JobsMissingOverheadActuals: %v", err)
	}
	found := false
	for _, id := range pending {
		if id == jobID {
			found = true
		}
	}
	if !found {
		t.Fatal("a terminal job with no overhead rows was not offered to the sweep")
	}

	for i := 0; i < 2; i++ {
		if err := store.RecordExecutionOverhead(ctx, jobID); err != nil {
			t.Fatalf("RecordExecutionOverhead pass %d: %v", i, err)
		}
	}
	if got := overheadRows(t, pool, ctx, jobID)[overheadClassFailed].Tasks; got != 1 {
		t.Fatalf("FAILED_COMPUTE tasks after two sweeps = %d, want 1", got)
	}

	// After recording, the job must not be offered again.
	pending, err = store.JobsMissingOverheadActuals(ctx, 500)
	if err != nil {
		t.Fatalf("JobsMissingOverheadActuals: %v", err)
	}
	for _, id := range pending {
		if id == jobID {
			t.Fatal("a swept job is still offered to the sweep")
		}
	}
}

// The load-bearing separation. Overhead may eventually inform failure reserve
// and verification price; base calibration may not inform money at all. Neither
// may train the other, and a comment saying so is not enforcement.
func TestOverheadAndBaseActualsCannotTrainEachOther(t *testing.T) {
	baseNeedles := []string{"plan_actuals", "ResolvePlanCalibration", "PlanCalibration"}
	overheadNeedles := []string{
		"execution_overhead_actuals", "RecordExecutionOverhead", "ExecutionOverhead",
		"OverheadRow", "overheadClass",
	}

	if refs := codeReferences(t, "plan_calibration.go", overheadNeedles); len(refs) != 0 {
		t.Errorf("plan_calibration.go references %v: overhead must not train base "+
			"runtime estimates", refs)
	}
	if refs := codeReferences(t, "execution_overhead.go", baseNeedles); len(refs) != 0 {
		t.Errorf("execution_overhead.go references %v: the two authorities must "+
			"stay separate", refs)
	}

	// Overhead is observed-only for now, exactly like base calibration. When it
	// is promoted into failure reserve or verification price that is a deliberate
	// change to this list, not an accident.
	allowed := map[string]bool{
		"execution_overhead.go": true, "execution_overhead_test.go": true,
		"workers.go": true,
	}
	guarded, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range guarded {
		if allowed[name] || strings.HasSuffix(name, "_test.go") {
			continue
		}
		if refs := codeReferences(t, name, overheadNeedles); len(refs) != 0 {
			t.Errorf("%s references %v but is not on the overhead allowlist; "+
				"promoting overhead into a decision is a deliberate change", name, refs)
		}
	}
}

// Phase 0 B: summing monetary overhead classes must neither mint nor
// double-count supplier cost.
//
// The specific hazard: a retried task's supplier payout already sits in
// whichever partitioning class owns that task row. If RETRY_COMPUTE also
// carried money, a job with retries would report operational cost that was
// never paid to anyone.
func TestOverheadMoneyIsIncrementalAndConserved(t *testing.T) {
	store, pool, ctx := planActualsTestStore(t)
	jobType := "ovh_" + uuid.NewString()[:8]

	// Every task carries a supplier payout. Two are primary (one retried
	// twice), the rest are overhead of one class each.
	jobID := planActualsFixtureJob(t, pool, ctx, jobType, 2, 2000, 1.00, 1.00,
		[]planActualsFixtureTask{
			{status: "complete", tokens: 500, chunkIndex: 0, explicitChunk: true,
				buyerUSD: 1.00, supplierUSD: 0.25,
				runtimeID: "candle_metal", hwClass: "apple_silicon_ultra"},
			{status: "complete", tokens: 500, retries: 2, chunkIndex: 1, explicitChunk: true,
				buyerUSD: 1.00, supplierUSD: 0.25,
				runtimeID: "candle_metal", hwClass: "apple_silicon_ultra"},
			{status: "complete", tokens: 500, chunkIndex: 1, explicitChunk: true,
				hedgedFromChunk: chunk(1), buyerUSD: 1.00, supplierUSD: 0.25,
				runtimeID: "candle_metal", hwClass: "apple_silicon_ultra"},
			{status: "complete", redundancy: true, tokens: 100, chunkIndex: 0, explicitChunk: true,
				buyerUSD: 1.00, supplierUSD: 0.25,
				runtimeID: "candle_metal", hwClass: "apple_silicon_ultra"},
			{status: "failed", chunkIndex: 3, explicitChunk: true,
				buyerUSD: 1.00, supplierUSD: 0.25,
				runtimeID: "candle_metal", hwClass: "apple_silicon_ultra"},
		}, planActualsFixtureOptions{})

	if err := store.RecordExecutionOverhead(ctx, jobID); err != nil {
		t.Fatalf("RecordExecutionOverhead: %v", err)
	}

	var jobSupplierTotal, overheadTotal, retryMoney float64
	if err := pool.QueryRow(ctx,
		`SELECT COALESCE(SUM(economic_supplier_payout_usd),0)::float8
		   FROM tasks WHERE job_id=$1`, jobID).Scan(&jobSupplierTotal); err != nil {
		t.Fatalf("sum task payouts: %v", err)
	}
	if err := pool.QueryRow(ctx,
		`SELECT COALESCE(SUM(measured_supplier_usd),0)
		   FROM execution_overhead_actuals WHERE job_id=$1`, jobID).Scan(&overheadTotal); err != nil {
		t.Fatalf("sum overhead: %v", err)
	}
	if err := pool.QueryRow(ctx,
		`SELECT COALESCE(SUM(measured_supplier_usd),0)
		   FROM execution_overhead_actuals WHERE job_id=$1 AND overhead_class=$2`,
		jobID, overheadClassRetry).Scan(&retryMoney); err != nil {
		t.Fatalf("sum retry money: %v", err)
	}

	if retryMoney != 0 {
		t.Errorf("RETRY_COMPUTE carries $%.4f; retries measure extra ATTEMPTS, and "+
			"the payout already sits in the class that owns the task row", retryMoney)
	}
	// Three overhead task rows out of five, at $0.25 each.
	if math.Abs(overheadTotal-0.75) > 0.000001 {
		t.Errorf("overhead supplier total $%.4f, want $0.75 (hedge + redundancy + failed)",
			overheadTotal)
	}
	// Minting check: overhead can never exceed what the job actually paid out.
	if overheadTotal > jobSupplierTotal+0.000001 {
		t.Errorf("overhead $%.4f exceeds the job's total supplier payout $%.4f: minted cost",
			overheadTotal, jobSupplierTotal)
	}
	// And the two primary rows must be left for the base authority.
	if math.Abs((jobSupplierTotal-overheadTotal)-0.50) > 0.000001 {
		t.Errorf("primary remainder $%.4f, want $0.50 (two primary task rows)",
			jobSupplierTotal-overheadTotal)
	}
}

// Phase 0 A: idempotence must not rest on a timestamp cursor. Concurrent
// sweeps, replays and restarts all converge on one row per (job, class).
func TestOverheadRecordingIsIdempotentUnderConcurrentSweeps(t *testing.T) {
	store, pool, ctx := planActualsTestStore(t)
	jobType := "ovh_" + uuid.NewString()[:8]
	jobID := planActualsFixtureJob(t, pool, ctx, jobType, 1, 1000, 1.00, 1.00,
		[]planActualsFixtureTask{
			{status: "failed", runtimeID: "candle_metal", hwClass: "apple_silicon_ultra"},
			{status: "complete", redundancy: true, tokens: 10,
				runtimeID: "candle_metal", hwClass: "apple_silicon_ultra"},
		}, planActualsFixtureOptions{})

	var wg sync.WaitGroup
	errs := make(chan error, 8)
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if err := store.RecordExecutionOverhead(ctx, jobID); err != nil {
				errs <- err
			}
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Fatalf("concurrent RecordExecutionOverhead: %v", err)
	}

	var rows int
	if err := pool.QueryRow(ctx,
		`SELECT COUNT(*) FROM execution_overhead_actuals WHERE job_id=$1`,
		jobID).Scan(&rows); err != nil {
		t.Fatalf("count: %v", err)
	}
	// FAILED_COMPUTE and VERIFICATION_COMPUTE, once each, after 8 racing sweeps.
	if rows != 2 {
		t.Fatalf("8 concurrent sweeps produced %d rows, want 2", rows)
	}

	// Provenance is stamped, so a later rule change is distinguishable rather
	// than silently averaged with these rows.
	var version int
	var matrix string
	var terminalAt *time.Time
	var taskIDs []uuid.UUID
	if err := pool.QueryRow(ctx,
		`SELECT recorder_version, authority_matrix_sha256, source_terminal_at, source_task_ids
		   FROM execution_overhead_actuals WHERE job_id=$1 AND overhead_class=$2`,
		jobID, overheadClassFailed).Scan(&version, &matrix, &terminalAt, &taskIDs); err != nil {
		t.Fatalf("read provenance: %v", err)
	}
	if version != overheadRecorderVersion {
		t.Errorf("recorder_version = %d, want %d", version, overheadRecorderVersion)
	}
	if matrix != generatedRuntimeMatrixSHA256 {
		t.Error("observation does not record the runtime authority in force")
	}
	if terminalAt == nil {
		t.Error("observation does not record the source terminal time")
	}
	if len(taskIDs) != 1 {
		t.Errorf("source_task_ids has %d entries, want the 1 failed task", len(taskIDs))
	}
}

// Phase 0 C: an analytics backlog must never make the process report not-ready.
func TestOverheadSweepIsAdvisoryAndNeverGatesReadiness(t *testing.T) {
	liveness.registerAdvisory(overheadTickerName, overheadSweepInterval)
	if !liveness.isAdvisory(overheadTickerName) {
		t.Fatal("the overhead sweep is not registered advisory")
	}
	// Far past any staleness budget, with no success ever recorded.
	started := time.Now().Add(-72 * time.Hour)
	for _, name := range liveness.stale(time.Now(), started) {
		if name == overheadTickerName {
			t.Fatal("a stale analytics sweep put the control plane out of rotation")
		}
	}

	// A load-bearing ticker in the same state still does gate readiness, so
	// this is an exclusion for one loop rather than a hole in the check.
	liveness.register("phase0-probe-authoritative", time.Second)
	found := false
	for _, name := range liveness.stale(time.Now(), started) {
		if name == "phase0-probe-authoritative" {
			found = true
		}
	}
	if !found {
		t.Fatal("staleness detection is broken for authoritative tickers")
	}
}

func TestOverheadHealthReportsBacklogWithoutAsserting(t *testing.T) {
	store, _, ctx := planActualsTestStore(t)
	health, err := store.ExecutionOverheadHealth(ctx)
	if err != nil {
		t.Fatalf("ExecutionOverheadHealth: %v", err)
	}
	if health.BacklogThreshold != overheadBacklogAlertThreshold {
		t.Errorf("threshold = %d, want %d", health.BacklogThreshold, overheadBacklogAlertThreshold)
	}
	if health.RecorderVersion != overheadRecorderVersion {
		t.Errorf("recorder version = %d, want %d", health.RecorderVersion, overheadRecorderVersion)
	}
	if health.Degraded != (health.Backlog >= overheadBacklogAlertThreshold) {
		t.Error("degraded does not follow from the backlog it reports")
	}
}
