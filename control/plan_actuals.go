package main

import (
	"context"
	"log"
	"math"
	"sort"

	"github.com/google/uuid"
)

// Plan accuracy is the perf lane's ground truth.
//
// eta_calibration already closes the estimate-versus-actual loop for exactly one
// predicted quantity: duration. Every other number the compute plan freezes —
// output tokens, task fan-out, retries, compute cost — has never been compared
// against what happened. Phase 1 of the execution frontier is calibrated compute
// planning, and there is no honest way to calibrate an estimator whose error has
// never been measured.
//
// This file only OBSERVES. Nothing in the money, reserve, pricing, or admission
// path reads plan_actuals, and that is deliberate rather than unfinished:
// reserving less because a learner says outputs are usually short would
// under-fund the job whose outputs are long, and the frozen economic plan is
// release-proven authority. Promoting any estimator from this evidence is a
// separate, gated change that must state its own quality and money effect.
//
// Metrics recorded here are the ones the tree can actually observe today. Three
// quantities the goal names are NOT recorded, because recording a guess as a
// measurement is worse than recording nothing:
//
//   - peak VRAM / KV growth: worker_memory_samples carries host-level
//     available/effective GB per worker, not per-job peak device memory. The
//     compute plan's minimum_memory_gb is an admission floor, so pairing it with
//     a host sample would measure the wrong thing.
//   - storage and transfer bytes: jobs.economic_output_bytes is realized, but the
//     compute plan freezes no predicted byte count to compare it against.
//   - duration: already owned by eta_calibration. Recording it twice would create
//     two learners over one quantity.
//
// Closing those gaps needs worker-side telemetry and a predicted byte count in
// the plan, not another table.
const (
	planMetricOutputTokens = "output_tokens"
	planMetricTaskCount    = "task_count"
	planMetricTaskAttempts = "task_attempts"
	planMetricComputeUSD   = "compute_usd"
)

// recordPlanActuals is the fire-and-forget finalize hook, shaped like
// recordEtaCalibration: an observation failure must never affect a finalize that
// has already committed money.
func recordPlanActuals(ctx context.Context, store *Store, jobID uuid.UUID) {
	if err := store.RecordPlanActuals(ctx, jobID); err != nil {
		log.Printf("plan actuals for job %s: %v (finalize unaffected)", jobID, err)
	}
}

// RecordPlanActuals writes one row per observable metric for a finalized job,
// pairing the frozen compute-plan prediction with the realized value.
//
// Only distributed plans are recorded. An exact-reuse plan performs no physical
// execution: its task geometry is zero by validation and its base_compute_usd is
// a delivery charge, so comparing either against a fleet is meaningless.
//
// runtime_id and hw_class are recorded only when every task that actually
// executed agrees on one value. A job spread across two hardware classes must
// not be attributed to either, so it lands in the empty-string bucket instead of
// biasing a cell it only half used.
func (s *Store) RecordPlanActuals(ctx context.Context, jobID uuid.UUID) error {
	_, err := s.pool.Exec(ctx, `
		WITH job AS (
		  SELECT j.id, j.job_type, j.tier, COALESCE(j.model_ref,'') AS model_ref,
		         CASE
		           WHEN j.compute_plan->'input_depth_profile'->>'p90_depth_band'
		                IN ('short','medium','long')
		           THEN j.compute_plan->'input_depth_profile'->>'p90_depth_band'
		         END AS depth_band,
		         (j.compute_plan->>'estimated_output_tokens')::float8 AS pred_output_tokens,
		         (j.compute_plan->>'total_initial_tasks')::float8     AS pred_tasks,
		         ((j.compute_plan->>'base_compute_usd')::float8
		          + COALESCE((j.compute_plan->>'verification_overhead_usd')::float8, 0))
		           AS pred_usd,
		         COALESCE(j.actual_usd, 0)::float8 AS real_usd
		    FROM jobs j
		   WHERE j.id = $1
		     AND j.compute_plan IS NOT NULL
		     AND j.compute_plan->>'execution_mode' = 'distributed'
		), fleet AS (
		  SELECT COUNT(*)::float8 AS task_rows,
		         (COUNT(*) + COALESCE(SUM(retry_count), 0))::float8 AS attempts,
		         COALESCE(SUM(
		           CASE WHEN NOT COALESCE(is_honeypot, false)
		                 AND NOT COALESCE(is_redundancy, false)
		                 AND status = 'complete'
		                THEN COALESCE(reported_tokens_used, 0) ELSE 0 END), 0)::float8
		           AS output_tokens,
		         CASE WHEN COUNT(DISTINCT runtime_id)
		                     FILTER (WHERE COALESCE(runtime_id,'') <> '') = 1
		              THEN MIN(runtime_id) FILTER (WHERE COALESCE(runtime_id,'') <> '')
		              ELSE '' END AS runtime_id,
		         CASE WHEN COUNT(DISTINCT execution_hw_class)
		                     FILTER (WHERE COALESCE(execution_hw_class,'') <> '') = 1
		              THEN MIN(execution_hw_class) FILTER (WHERE COALESCE(execution_hw_class,'') <> '')
		              ELSE '' END AS hw_class
		    FROM tasks
		   WHERE job_id = $1
		), pairs AS (
		            SELECT 'output_tokens'::text AS metric, j.pred_output_tokens AS predicted,
		                   f.output_tokens AS realized FROM job j, fleet f
		  UNION ALL SELECT 'task_count',    j.pred_tasks, f.task_rows FROM job j, fleet f
		  UNION ALL SELECT 'task_attempts', j.pred_tasks, f.attempts  FROM job j, fleet f
		  UNION ALL SELECT 'compute_usd',   j.pred_usd,   j.real_usd  FROM job j
		)
		INSERT INTO plan_actuals
		  (job_id, metric, job_type, tier, model_ref, input_depth_band,
		   runtime_id, hw_class, predicted, realized)
		SELECT j.id, p.metric, j.job_type, j.tier, j.model_ref, j.depth_band,
		       f.runtime_id, f.hw_class, p.predicted, p.realized
		  FROM job j, fleet f, pairs p
		 WHERE p.predicted > 0
		   AND p.realized >= 0
		   AND p.predicted = p.predicted   -- reject NaN from a malformed frozen plan
		   AND p.realized = p.realized
		ON CONFLICT (job_id, metric) DO NOTHING`, jobID)
	return err
}

// PlanAccuracyRow is one (metric, scope) bucket of realized-versus-predicted
// evidence. Ratios are realized/predicted: 1.0 is a perfect estimator, below 1
// means the plan over-predicted, above 1 means it under-predicted.
type PlanAccuracyRow struct {
	Metric         string  `json:"metric"`
	JobType        string  `json:"job_type"`
	Tier           string  `json:"tier"`
	ModelRef       string  `json:"model_ref"`
	InputDepthBand string  `json:"input_depth_band"`
	RuntimeID      string  `json:"runtime_id"`
	HWClass        string  `json:"hw_class"`
	Samples        int     `json:"samples"`
	MedianRatio    float64 `json:"median_ratio"`
	P90Ratio       float64 `json:"p90_ratio"`
	// MAPE is the mean absolute percentage error over the window. It is the
	// number a promotion argues against: a calibration that moves the median to
	// 1.0 while widening the spread has not improved the estimator.
	MAPE float64 `json:"mape"`
	// Trusted is false below driftMinSamples. A one-sample bucket is displayed
	// rather than hidden, so a reader can see coverage, but it is explicitly not
	// evidence.
	Trusted bool `json:"trusted"`
	// WindowHours names the rolling bound every figure above is limited to, so a
	// reader never mistakes this for all-time history.
	WindowHours float64 `json:"window_hours"`
}

// PlanAccuracy is the reconciliation report Phase 1 gates promotions on. It
// reads plan_actuals only; it never writes and never feeds a quote.
func (s *Store) PlanAccuracy(ctx context.Context) ([]PlanAccuracyRow, error) {
	rows, err := s.pool.Query(ctx,
		`SELECT metric, job_type, tier, model_ref, COALESCE(input_depth_band,''),
		        runtime_id, hw_class,
		        COUNT(*),
		        percentile_cont(0.5) WITHIN GROUP (ORDER BY realized / predicted),
		        percentile_cont(0.9) WITHIN GROUP (ORDER BY realized / predicted),
		        AVG(ABS(realized - predicted) / predicted) * 100
		   FROM plan_actuals
		  WHERE predicted > 0
		    AND created_at > now() - make_interval(secs => $1)
		  GROUP BY metric, job_type, tier, model_ref, COALESCE(input_depth_band,''),
		           runtime_id, hw_class
		  ORDER BY metric, job_type, tier, model_ref, COALESCE(input_depth_band,''),
		           runtime_id, hw_class`,
		int(driftWindow.Seconds()))
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	out := []PlanAccuracyRow{}
	for rows.Next() {
		var r PlanAccuracyRow
		if err := rows.Scan(&r.Metric, &r.JobType, &r.Tier, &r.ModelRef,
			&r.InputDepthBand, &r.RuntimeID, &r.HWClass, &r.Samples,
			&r.MedianRatio, &r.P90Ratio, &r.MAPE); err != nil {
			return nil, err
		}
		r.Trusted = r.Samples >= driftMinSamples
		r.WindowHours = driftWindow.Hours()
		out = append(out, r)
	}
	return out, rows.Err()
}

// planAccuracySummary reduces a window of realized/predicted ratios to the three
// figures a promotion has to move. It is separated from the SQL so the
// arithmetic — including the degenerate windows — is testable without a
// database.
//
// A non-finite or non-positive prediction is dropped rather than propagated: one
// malformed frozen plan must not be able to turn a whole bucket's median into
// NaN and hide a real regression behind an unreadable report.
func planAccuracySummary(predicted, realized []float64) (samples int, median, p90, mape float64) {
	if len(predicted) != len(realized) {
		return 0, 0, 0, 0
	}
	ratios := make([]float64, 0, len(predicted))
	var absPctSum float64
	for i, p := range predicted {
		r := realized[i]
		if p <= 0 || math.IsNaN(p) || math.IsInf(p, 0) ||
			r < 0 || math.IsNaN(r) || math.IsInf(r, 0) {
			continue
		}
		ratios = append(ratios, r/p)
		absPctSum += math.Abs(r-p) / p * 100
	}
	if len(ratios) == 0 {
		return 0, 0, 0, 0
	}
	sort.Float64s(ratios)
	return len(ratios), continuousPercentile(ratios, 0.5), continuousPercentile(ratios, 0.9),
		absPctSum / float64(len(ratios))
}

// continuousPercentile matches PostgreSQL percentile_cont on a sorted slice, so
// the Go summary and the SQL rollup cannot disagree about the same window.
func continuousPercentile(sorted []float64, q float64) float64 {
	if len(sorted) == 0 {
		return 0
	}
	if len(sorted) == 1 {
		return sorted[0]
	}
	pos := q * float64(len(sorted)-1)
	lo := int(math.Floor(pos))
	hi := int(math.Ceil(pos))
	if lo == hi {
		return sorted[lo]
	}
	return sorted[lo] + (sorted[hi]-sorted[lo])*(pos-float64(lo))
}
