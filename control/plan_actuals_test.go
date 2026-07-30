package main

import (
	"context"
	"math"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
)

func TestPlanAccuracySummaryMatchesPercentileContOnKnownWindows(t *testing.T) {
	for _, tc := range []struct {
		name                string
		predicted, realized []float64
		samples             int
		median, p90, mape   float64
	}{
		{
			name:      "a perfect estimator",
			predicted: []float64{100, 100, 100},
			realized:  []float64{100, 100, 100},
			samples:   3, median: 1, p90: 1, mape: 0,
		},
		{
			name:      "a ceiling estimator over-predicts by half",
			predicted: []float64{100, 100, 100, 100},
			realized:  []float64{50, 50, 50, 50},
			samples:   4, median: 0.5, p90: 0.5, mape: 50,
		},
		{
			name:      "interpolated median on an even window",
			predicted: []float64{100, 100},
			realized:  []float64{50, 150},
			samples:   2, median: 1, p90: 1.4, mape: 50,
		},
		{
			name:      "a malformed frozen plan is dropped, not propagated as NaN",
			predicted: []float64{100, 0, 100, math.NaN(), 100},
			realized:  []float64{100, 500, 100, 100, 100},
			samples:   3, median: 1, p90: 1, mape: 0,
		},
		{
			name:      "a non-finite realized value cannot poison the bucket",
			predicted: []float64{100, 100},
			realized:  []float64{100, math.Inf(1)},
			samples:   1, median: 1, p90: 1, mape: 0,
		},
		{
			name:      "an empty window reports nothing rather than a fabricated 1.0",
			predicted: []float64{},
			realized:  []float64{},
			samples:   0, median: 0, p90: 0, mape: 0,
		},
	} {
		samples, median, p90, mape := planAccuracySummary(tc.predicted, tc.realized)
		if samples != tc.samples {
			t.Errorf("%s: samples = %d, want %d", tc.name, samples, tc.samples)
		}
		for _, got := range []struct {
			label     string
			got, want float64
		}{
			{"median", median, tc.median},
			{"p90", p90, tc.p90},
			{"mape", mape, tc.mape},
		} {
			if math.Abs(got.got-got.want) > 0.000001 {
				t.Errorf("%s: %s = %v, want %v", tc.name, got.label, got.got, got.want)
			}
		}
	}
}

func TestPlanAccuracySummaryRefusesMismatchedSeries(t *testing.T) {
	if samples, _, _, _ := planAccuracySummary([]float64{1, 2}, []float64{1}); samples != 0 {
		t.Fatalf("mismatched series reported %d samples, want 0", samples)
	}
}

func planActualsTestStore(t *testing.T) (*Store, *pgxpool.Pool, context.Context) {
	t.Helper()
	databaseURL := requireTestDatabase(t)
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	t.Cleanup(cancel)
	pool, err := pgxpool.New(ctx, databaseURL)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	t.Cleanup(pool.Close)
	store := NewStore(pool)
	if err := store.Migrate(ctx); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	return store, pool, ctx
}

// planActualsFixtureJob inserts a job with a frozen distributed compute plan and
// the tasks that realized it. It writes only the columns RecordPlanActuals reads,
// so it cannot accidentally assert behaviour that depends on money state.
func planActualsFixtureJob(
	t *testing.T, pool *pgxpool.Pool, ctx context.Context,
	jobType string, predictedTasks int, predictedOutputTokens int64,
	predictedUSD, realizedUSD float64,
	tasks []planActualsFixtureTask,
) uuid.UUID {
	t.Helper()
	supplierID, workerID := uuid.New(), uuid.New()
	if _, err := pool.Exec(ctx,
		`INSERT INTO suppliers (id,email,reputation,status) VALUES ($1,$2,0.5,'active')`,
		supplierID, "plan-actuals+"+supplierID.String()+"@example.test"); err != nil {
		t.Fatalf("insert supplier: %v", err)
	}
	if _, err := pool.Exec(ctx,
		`INSERT INTO workers (id,supplier_id,hw_class) VALUES ($1,$2,'apple_silicon_ultra')`,
		workerID, supplierID); err != nil {
		t.Fatalf("insert worker: %v", err)
	}
	jobID := uuid.New()
	plan := map[string]any{
		"execution_mode":            "distributed",
		"total_initial_tasks":       predictedTasks,
		"estimated_output_tokens":   predictedOutputTokens,
		"base_compute_usd":          predictedUSD,
		"verification_overhead_usd": 0,
		"input_depth_profile":       map[string]any{"p90_depth_band": "medium"},
	}
	if _, err := pool.Exec(ctx,
		`INSERT INTO jobs (id, buyer_id, status, job_type, tier, model_ref, input_ref,
		                   actual_usd, compute_plan)
		 VALUES ($1,$2,'complete',$3,'batch','plan-actuals-model','in',$4,$5)`,
		jobID, uuid.New(), jobType, realizedUSD, plan); err != nil {
		t.Fatalf("insert job: %v", err)
	}
	for _, task := range tasks {
		// tasks_execution_identity_complete and tasks_runtime_provenance_complete:
		// a task that names an executing hardware class or runtime must carry the
		// whole identity. The fixture obeys the same rules the runtime does, so
		// the test cannot pass against a shape production could never produce.
		if _, err := pool.Exec(ctx,
			`INSERT INTO tasks (id, job_id, status, is_honeypot, is_redundancy,
			                    retry_count, reported_tokens_used,
			                    runtime_id, runtime_cell_id, runtime_matrix_sha256, model_kind,
			                    execution_worker_id, execution_supplier_id,
			                    execution_hw_class, execution_engine,
			                    execution_build_hash)
			 VALUES ($1,$2,$3,$4,$5,$6,$7,$8,'plan-actuals-cell',
			         repeat('a',64),'gguf',$9,$10,$11,'candle','plan-actuals-build')`,
			uuid.New(), jobID, task.status, task.honeypot, task.redundancy,
			task.retries, task.tokens, task.runtimeID,
			workerID, supplierID, task.hwClass); err != nil {
			t.Fatalf("insert task: %v", err)
		}
	}
	t.Cleanup(func() {
		c, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		_, _ = pool.Exec(c, `DELETE FROM plan_actuals WHERE job_id=$1`, jobID)
		_, _ = pool.Exec(c, `DELETE FROM tasks WHERE job_id=$1`, jobID)
		_, _ = pool.Exec(c, `DELETE FROM jobs WHERE id=$1`, jobID)
		_, _ = pool.Exec(c, `DELETE FROM workers WHERE id=$1`, workerID)
		_, _ = pool.Exec(c, `DELETE FROM suppliers WHERE id=$1`, supplierID)
	})
	return jobID
}

type planActualsFixtureTask struct {
	status               string
	honeypot, redundancy bool
	retries              int
	tokens               int64
	runtimeID, hwClass   string
}

func planActualsRow(t *testing.T, pool *pgxpool.Pool, ctx context.Context,
	jobID uuid.UUID, metric string) (predicted, realized float64, runtimeID, hwClass, band string) {
	t.Helper()
	if err := pool.QueryRow(ctx,
		`SELECT predicted, realized, runtime_id, hw_class, COALESCE(input_depth_band,'')
		   FROM plan_actuals WHERE job_id=$1 AND metric=$2`, jobID, metric).
		Scan(&predicted, &realized, &runtimeID, &hwClass, &band); err != nil {
		t.Fatalf("read plan_actuals %s: %v", metric, err)
	}
	return predicted, realized, runtimeID, hwClass, band
}

// The regression that matters: before this table the compute plan predicted
// output tokens, task fan-out and cost, and nothing ever compared any of them to
// what happened. A ceiling estimator could over-predict output by 10x forever
// and no report would show it.
func TestRecordPlanActualsCapturesEveryObservableMetric(t *testing.T) {
	store, pool, ctx := planActualsTestStore(t)
	jobType := "planact_" + uuid.NewString()[:8]

	// Predicted 4 initial tasks and 4000 output tokens for $2.00.
	// Realized: 4 primary tasks (900 tokens each = 3600), one of which retried
	// twice, plus a honeypot and a redundancy task that must not count toward
	// buyer output. Realized cost $1.80.
	jobID := planActualsFixtureJob(t, pool, ctx, jobType, 4, 4000, 2.00, 1.80,
		[]planActualsFixtureTask{
			{status: "complete", tokens: 900, runtimeID: "candle_metal", hwClass: "apple_silicon_ultra"},
			{status: "complete", tokens: 900, runtimeID: "candle_metal", hwClass: "apple_silicon_ultra"},
			{status: "complete", tokens: 900, runtimeID: "candle_metal", hwClass: "apple_silicon_ultra"},
			{status: "complete", tokens: 900, retries: 2, runtimeID: "candle_metal", hwClass: "apple_silicon_ultra"},
			{status: "complete", honeypot: true, tokens: 500, runtimeID: "candle_metal", hwClass: "apple_silicon_ultra"},
			{status: "complete", redundancy: true, tokens: 500, runtimeID: "candle_metal", hwClass: "apple_silicon_ultra"},
		})

	if err := store.RecordPlanActuals(ctx, jobID); err != nil {
		t.Fatalf("RecordPlanActuals: %v", err)
	}

	for _, want := range []struct {
		metric              string
		predicted, realized float64
	}{
		// Honeypot and redundancy tokens are excluded: they are verification
		// work, not the buyer's output.
		{planMetricOutputTokens, 4000, 3600},
		// Six task rows realized against four planned: verification fan-out is
		// visible instead of hidden inside a duration average.
		{planMetricTaskCount, 4, 6},
		// Attempts add the two retries on top of the six rows.
		{planMetricTaskAttempts, 4, 8},
		{planMetricComputeUSD, 2.00, 1.80},
	} {
		predicted, realized, runtimeID, hwClass, band := planActualsRow(t, pool, ctx, jobID, want.metric)
		if math.Abs(predicted-want.predicted) > 0.000001 || math.Abs(realized-want.realized) > 0.000001 {
			t.Errorf("%s: predicted/realized = %v/%v, want %v/%v",
				want.metric, predicted, realized, want.predicted, want.realized)
		}
		if runtimeID != "candle_metal" || hwClass != "apple_silicon_ultra" {
			t.Errorf("%s: runtime/hw = %q/%q, want candle_metal/apple_silicon_ultra",
				want.metric, runtimeID, hwClass)
		}
		if band != "medium" {
			t.Errorf("%s: input_depth_band = %q, want medium (from the frozen plan)", want.metric, band)
		}
	}

	// Re-running finalize must not double-count. A job that is swept twice would
	// otherwise write a second row per metric and halve the apparent error.
	if err := store.RecordPlanActuals(ctx, jobID); err != nil {
		t.Fatalf("RecordPlanActuals (repeat): %v", err)
	}
	var rowCount int
	if err := pool.QueryRow(ctx,
		`SELECT COUNT(*) FROM plan_actuals WHERE job_id=$1`, jobID).Scan(&rowCount); err != nil {
		t.Fatalf("count: %v", err)
	}
	if rowCount != 4 {
		t.Fatalf("plan_actuals rows after two finalizes = %d, want 4", rowCount)
	}
}

// A job spread over two hardware classes cannot be attributed to either one.
// Recording it against the first task's class would credit or blame a cell for
// work it only partly did.
func TestRecordPlanActualsRefusesToAttributeAMixedFleetJob(t *testing.T) {
	store, pool, ctx := planActualsTestStore(t)
	jobType := "planact_" + uuid.NewString()[:8]
	jobID := planActualsFixtureJob(t, pool, ctx, jobType, 2, 2000, 1.00, 1.00,
		[]planActualsFixtureTask{
			{status: "complete", tokens: 1000, runtimeID: "candle_metal", hwClass: "apple_silicon_ultra"},
			{status: "complete", tokens: 1000, runtimeID: "candle_metal", hwClass: "apple_silicon_base"},
		})
	if err := store.RecordPlanActuals(ctx, jobID); err != nil {
		t.Fatalf("RecordPlanActuals: %v", err)
	}
	_, _, runtimeID, hwClass, _ := planActualsRow(t, pool, ctx, jobID, planMetricOutputTokens)
	if runtimeID != "candle_metal" {
		t.Errorf("runtime_id = %q, want candle_metal (both tasks agree)", runtimeID)
	}
	if hwClass != "" {
		t.Errorf("hw_class = %q, want \"\" for a mixed-hardware job", hwClass)
	}
}

// An exact-reuse plan performs no physical execution: its task geometry is zero
// by validation and its base_compute_usd is a delivery charge. Recording it would
// report a 0/0 fan-out error against a fleet that never ran.
func TestRecordPlanActualsIgnoresExactReuseDelivery(t *testing.T) {
	store, pool, ctx := planActualsTestStore(t)
	jobID := uuid.New()
	if _, err := pool.Exec(ctx,
		`INSERT INTO jobs (id, buyer_id, status, job_type, tier, input_ref, actual_usd, compute_plan)
		 VALUES ($1,$2,'complete','batch_infer','batch','in',0.5,$3)`,
		jobID, uuid.New(), map[string]any{
			"execution_mode":   "exact_result_reuse",
			"base_compute_usd": 0.5,
		}); err != nil {
		t.Fatalf("insert reuse job: %v", err)
	}
	t.Cleanup(func() {
		c, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		_, _ = pool.Exec(c, `DELETE FROM plan_actuals WHERE job_id=$1`, jobID)
		_, _ = pool.Exec(c, `DELETE FROM jobs WHERE id=$1`, jobID)
	})
	if err := store.RecordPlanActuals(ctx, jobID); err != nil {
		t.Fatalf("RecordPlanActuals: %v", err)
	}
	var rowCount int
	if err := pool.QueryRow(ctx,
		`SELECT COUNT(*) FROM plan_actuals WHERE job_id=$1`, jobID).Scan(&rowCount); err != nil {
		t.Fatalf("count: %v", err)
	}
	if rowCount != 0 {
		t.Fatalf("exact-reuse job wrote %d plan_actuals rows, want 0", rowCount)
	}
}

// The report must agree with the Go summary over the same window, and must not
// present a thin bucket as evidence.
func TestPlanAccuracyReportsUntrustedBucketsWithoutHidingThem(t *testing.T) {
	store, pool, ctx := planActualsTestStore(t)
	jobType := "planact_" + uuid.NewString()[:8]

	for i := 0; i < driftMinSamples; i++ {
		jobID := planActualsFixtureJob(t, pool, ctx, jobType, 2, 1000, 1.00, 1.00,
			[]planActualsFixtureTask{
				{status: "complete", tokens: 250, runtimeID: "candle_metal", hwClass: "apple_silicon_ultra"},
				{status: "complete", tokens: 250, runtimeID: "candle_metal", hwClass: "apple_silicon_ultra"},
			})
		if err := store.RecordPlanActuals(ctx, jobID); err != nil {
			t.Fatalf("RecordPlanActuals: %v", err)
		}
	}

	rows, err := store.PlanAccuracy(ctx)
	if err != nil {
		t.Fatalf("PlanAccuracy: %v", err)
	}
	var found *PlanAccuracyRow
	for i := range rows {
		if rows[i].JobType == jobType && rows[i].Metric == planMetricOutputTokens {
			found = &rows[i]
		}
	}
	if found == nil {
		t.Fatalf("no %s row for job type %s in %d rows", planMetricOutputTokens, jobType, len(rows))
	}
	if found.Samples != driftMinSamples || !found.Trusted {
		t.Fatalf("samples=%d trusted=%v, want %d and trusted",
			found.Samples, found.Trusted, driftMinSamples)
	}
	// A 1000-token ceiling against 500 realized is exactly a 0.5 ratio and 50%
	// error. If the report ever disagrees with planAccuracySummary on the same
	// window, one of them is lying about the estimator.
	predicted := make([]float64, driftMinSamples)
	realized := make([]float64, driftMinSamples)
	for i := range predicted {
		predicted[i], realized[i] = 1000, 500
	}
	_, median, p90, mape := planAccuracySummary(predicted, realized)
	for _, cmp := range []struct {
		label          string
		sql, inProcess float64
	}{
		{"median_ratio", found.MedianRatio, median},
		{"p90_ratio", found.P90Ratio, p90},
		{"mape", found.MAPE, mape},
	} {
		if math.Abs(cmp.sql-cmp.inProcess) > 0.000001 {
			t.Errorf("%s: SQL rollup %v disagrees with in-process summary %v",
				cmp.label, cmp.sql, cmp.inProcess)
		}
	}
	if found.WindowHours != driftWindow.Hours() {
		t.Errorf("window_hours = %v, want %v", found.WindowHours, driftWindow.Hours())
	}
}
