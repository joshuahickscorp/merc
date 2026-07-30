package main

import (
	"context"
	"log"
	"time"

	"github.com/google/uuid"
)

// Overhead actuals answer the question plan_actuals deliberately does not.
//
// plan_actuals measures the BASE workload: primary successful work, hedges
// collapsed, verification and failures excluded. That is the right training set
// for a runtime duration, output-token or base-compute estimator, and it is the
// wrong basis for a price. A lane can carry a perfectly calibrated base planner
// while the real cost of delivering a successful outcome on it is twice the
// estimate, because it retries constantly or verifies heavily. Averaging the two
// into one dataset makes the base estimator worse AND the price wrong.
//
// So this is a second authority with its own table, its own recorder and its own
// rule: it may inform failure reserve, verification price, retry policy,
// topology risk, supplier and provider cost, and pricing confidence. It may
// never train an ordinary runtime estimate.
//
// Unlike plan_actuals, this records failed and cancelled jobs too — that is
// where FAILED_COMPUTE and CANCELLED_COMPUTE live, and excluding them would hide
// exactly the cost this table exists to surface.
const (
	overheadClassVerification = "VERIFICATION_COMPUTE"
	overheadClassHedge        = "HEDGE_COMPUTE"
	overheadClassFailed       = "FAILED_COMPUTE"
	overheadClassCancelled    = "CANCELLED_COMPUTE"
	overheadClassRetry        = "RETRY_COMPUTE"
	overheadClassCacheAvoided = "CACHE_AVOIDED"
)

// COALESCING_AVOIDED is deliberately absent. ClaimInflightLeader has no
// production caller, so no job can be a coalesced follower, and a class nothing
// can produce would report a permanent zero that reads as "we measured this and
// there was none". Add it in the same change that wires coalescing.

// overheadSweepBatch bounds one pass. The sweep is a background observer; it
// must never hold a long transaction against jobs the money path is using.
const overheadSweepBatch = 200

// sweepExecutionOverhead records overhead for terminal jobs that have none yet.
//
// This is a sweep rather than a hook inside failJobAndSettleOnce or the cancel
// transaction on purpose: those run inside the money transaction, and an
// observation failure must never be able to roll back a settlement. Recording
// from outside also gives one code path for all three terminal statuses instead
// of three hooks that can drift apart.
func (wk *Workers) sweepExecutionOverhead(ctx context.Context) error {
	jobIDs, err := wk.store.JobsMissingOverheadActuals(ctx, overheadSweepBatch)
	if err != nil {
		return err
	}
	for _, jobID := range jobIDs {
		if err := wk.store.RecordExecutionOverhead(ctx, jobID); err != nil {
			// One malformed job must not stall the sweep for every other job.
			log.Printf("execution overhead for job %s: %v (sweep continues)", jobID, err)
			continue
		}
	}
	return nil
}

// JobsMissingOverheadActuals returns terminal jobs with no overhead rows yet,
// oldest first so a backlog drains in the order it accumulated.
func (s *Store) JobsMissingOverheadActuals(ctx context.Context, limit int) ([]uuid.UUID, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT j.id
		  FROM jobs j
		 WHERE j.terminal_at IS NOT NULL
		   AND j.compute_plan IS NOT NULL
		   AND NOT EXISTS (
		     SELECT 1 FROM execution_overhead_actuals o WHERE o.job_id = j.id
		   )
		 ORDER BY j.terminal_at
		 LIMIT $1`, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []uuid.UUID
	for rows.Next() {
		var id uuid.UUID
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		out = append(out, id)
	}
	return out, rows.Err()
}

// RecordExecutionOverhead writes one row per non-zero overhead class for a
// terminal job.
//
// The first four classes partition the job's non-primary task rows by a strict
// priority: verification work is verification work even when it was created as a
// tiebreak hedge, so a redundancy row lands in VERIFICATION_COMPUTE and not in
// HEDGE_COMPUTE. Without that priority a tiebreak would be counted twice and the
// total overhead of a heavily-verified lane would be overstated.
//
// RETRY_COMPUTE is intentionally outside that partition. It measures attempts,
// not rows: a retried verification task contributes its row to
// VERIFICATION_COMPUTE and its extra attempts here. Summing tasks across all six
// classes is therefore meaningless, and summing measured_supplier_usd across the
// first four is not.
//
// Zero-valued classes are not written. A row saying "this job had no hedges"
// carries no information and would triple the table.
func (s *Store) RecordExecutionOverhead(ctx context.Context, jobID uuid.UUID) error {
	_, err := s.pool.Exec(ctx, `
		WITH job AS (
		  SELECT j.id, j.job_type, j.tier, j.status,
		         COALESCE(j.model_ref,'') AS model_ref,
		         COALESCE(j.workload_decision->>'workload_class','') AS workload_class,
		         CASE
		           WHEN j.compute_plan->'input_depth_profile'->>'p90_depth_band'
		                IN ('short','medium','long')
		           THEN j.compute_plan->'input_depth_profile'->>'p90_depth_band'
		         END AS depth_band,
		         j.compute_plan->>'execution_mode' AS execution_mode,
		         COALESCE((j.compute_plan->>'base_compute_usd')::float8, 0) AS plan_base_usd
		    FROM jobs j
		   WHERE j.id = $1
		     AND j.terminal_at IS NOT NULL
		     AND j.compute_plan IS NOT NULL
		), classified AS (
		  SELECT
		    CASE
		      WHEN COALESCE(t.is_honeypot,false) OR COALESCE(t.is_redundancy,false)
		        THEN 'VERIFICATION_COMPUTE'
		      WHEN t.hedged_from IS NOT NULL THEN 'HEDGE_COMPUTE'
		      WHEN t.status = 'failed' THEN 'FAILED_COMPUTE'
		      WHEN t.status = 'cancelled' THEN 'CANCELLED_COMPUTE'
		      ELSE 'PRIMARY'
		    END AS overhead_class,
		    COALESCE(t.reported_tokens_used, 0) AS tokens,
		    COALESCE(t.economic_supplier_payout_usd, 0)::float8 AS supplier_usd,
		    COALESCE(t.retry_count, 0) AS retries,
		    COALESCE(t.runtime_id,'') AS runtime_id,
		    COALESCE(t.execution_hw_class,'') AS hw_class
		    FROM tasks t
		   WHERE t.job_id = $1
		), fleet AS (
		  SELECT
		    CASE WHEN COUNT(DISTINCT runtime_id) FILTER (WHERE runtime_id <> '') = 1
		         THEN MIN(runtime_id) FILTER (WHERE runtime_id <> '') ELSE '' END AS runtime_id,
		    CASE WHEN COUNT(DISTINCT hw_class) FILTER (WHERE hw_class <> '') = 1
		         THEN MIN(hw_class) FILTER (WHERE hw_class <> '') ELSE '' END AS hw_class
		    FROM classified
		), partitioned AS (
		  SELECT overhead_class,
		         COUNT(*)::int AS tasks,
		         COUNT(*)::int AS attempts,
		         COALESCE(SUM(tokens), 0)::bigint AS tokens,
		         COALESCE(SUM(supplier_usd), 0) AS measured_supplier_usd,
		         0::float8 AS avoided_estimate_usd
		    FROM classified
		   WHERE overhead_class <> 'PRIMARY'
		   GROUP BY overhead_class
		), retries AS (
		  SELECT 'RETRY_COMPUTE'::text AS overhead_class,
		         COUNT(*) FILTER (WHERE retries > 0)::int AS tasks,
		         COALESCE(SUM(retries), 0)::int AS attempts,
		         0::bigint AS tokens,
		         0::float8 AS measured_supplier_usd,
		         0::float8 AS avoided_estimate_usd
		    FROM classified
		), cache AS (
		  SELECT 'CACHE_AVOIDED'::text AS overhead_class,
		         0::int AS tasks, 0::int AS attempts, 0::bigint AS tokens,
		         0::float8 AS measured_supplier_usd,
		         j.plan_base_usd AS avoided_estimate_usd
		    FROM job j
		   WHERE j.execution_mode = 'exact_result_reuse'
		), all_classes AS (
		  SELECT * FROM partitioned
		  UNION ALL SELECT * FROM retries
		  UNION ALL SELECT * FROM cache
		)
		INSERT INTO execution_overhead_actuals
		  (job_id, overhead_class, job_type, workload_class, tier, model_ref,
		   input_depth_band, runtime_profile_id, hw_class, job_terminal_status,
		   tasks, attempts, tokens, measured_supplier_usd, avoided_estimate_usd)
		SELECT j.id, c.overhead_class, j.job_type, j.workload_class, j.tier,
		       j.model_ref, j.depth_band, f.runtime_id, f.hw_class, j.status,
		       c.tasks, c.attempts, c.tokens, c.measured_supplier_usd,
		       c.avoided_estimate_usd
		  FROM job j, fleet f, all_classes c
		 WHERE c.tasks > 0 OR c.attempts > 0 OR c.tokens > 0
		    OR c.measured_supplier_usd > 0 OR c.avoided_estimate_usd > 0
		ON CONFLICT (job_id, overhead_class) DO NOTHING`, jobID)
	return err
}

// OverheadRow is one (class, scope) rollup. It is reported separately from
// PlanAccuracy so a reader cannot mistake overhead cost for base estimator error.
type OverheadRow struct {
	OverheadClass       string  `json:"overhead_class"`
	JobType             string  `json:"job_type"`
	WorkloadClass       string  `json:"workload_class"`
	Tier                string  `json:"tier"`
	ModelRef            string  `json:"model_ref"`
	RuntimeProfileID    string  `json:"runtime_profile_id"`
	HWClass             string  `json:"hw_class"`
	Jobs                int     `json:"jobs"`
	Tasks               int64   `json:"tasks"`
	Attempts            int64   `json:"attempts"`
	Tokens              int64   `json:"tokens"`
	MeasuredSupplierUSD float64 `json:"measured_supplier_usd"`
	AvoidedEstimateUSD  float64 `json:"avoided_estimate_usd"`
	WindowHours         float64 `json:"window_hours"`
}

// ExecutionOverhead is the read side. It reports cost, never an estimator ratio:
// there is no prediction to compare against, because nothing in the compute plan
// forecasts how much a lane will retry.
func (s *Store) ExecutionOverhead(ctx context.Context) ([]OverheadRow, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT overhead_class, job_type, workload_class, tier, model_ref,
		       runtime_profile_id, hw_class,
		       COUNT(DISTINCT job_id), COALESCE(SUM(tasks),0), COALESCE(SUM(attempts),0),
		       COALESCE(SUM(tokens),0), COALESCE(SUM(measured_supplier_usd),0),
		       COALESCE(SUM(avoided_estimate_usd),0)
		  FROM execution_overhead_actuals
		 WHERE created_at > now() - make_interval(secs => $1)
		 GROUP BY overhead_class, job_type, workload_class, tier, model_ref,
		          runtime_profile_id, hw_class
		 ORDER BY overhead_class, job_type, workload_class, tier, model_ref,
		          runtime_profile_id, hw_class`,
		int(driftWindow.Seconds()))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []OverheadRow{}
	for rows.Next() {
		var r OverheadRow
		if err := rows.Scan(&r.OverheadClass, &r.JobType, &r.WorkloadClass, &r.Tier,
			&r.ModelRef, &r.RuntimeProfileID, &r.HWClass, &r.Jobs, &r.Tasks,
			&r.Attempts, &r.Tokens, &r.MeasuredSupplierUSD,
			&r.AvoidedEstimateUSD); err != nil {
			return nil, err
		}
		r.WindowHours = driftWindow.Hours()
		out = append(out, r)
	}
	return out, rows.Err()
}

// overheadSweepInterval is slow on purpose. Overhead is an accounting question,
// not a control loop: nothing waits on the answer, and a tight interval would
// scan the jobs table for no benefit.
const overheadSweepInterval = 30 * time.Second
