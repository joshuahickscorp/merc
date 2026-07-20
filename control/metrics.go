package main

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"os"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

type metricsState struct {
	jobsSubmitted             atomic.Int64
	tasksDispatched           atomic.Int64
	tasksCompleted            atomic.Int64
	verificationMismatch      atomic.Int64
	payoutsReleased           atomic.Int64
	quarantines               atomic.Int64 // suppliers auto-quarantined (Verification V2 / fraud)
	hedges                    atomic.Int64 // straggler hedge tasks inserted
	resultMerges              atomic.Int64 // buyer-ready artifacts merged
	tiebreaks                 atomic.Int64 // third-worker tiebreak tasks inserted
	taskFailures              atomic.Int64 // typed task failures reported via POST /v1/worker/task/{id}/fail
	quotes                    atomic.Int64 // POST /v1/quote priced + persisted (every quote the Brain issued)
	budgetStops               atomic.Int64 // capped jobs paused before breach (Budget Governor stop, §14 D8)
	longPollTimeouts          atomic.Int64 // worker long-poll waits that returned empty on timeout (§7 D1; 0 until the long-poll slice lands)
	stuckCancels              atomic.Int64 // runs auto-cancelled past their deadline with no progress (checkpointed + partially settled; strike >= 1)
	stuckRescues              atomic.Int64 // runs rescued on their FIRST stuck verdict (unfinished tasks requeued; strike 0 -> 1)
	watchdogNearMiss          atomic.Int64 // finalized jobs that finished LATE (realized > 1.2 × predicted eta_secs)  -  the data that tunes stuckEtaFactor
	reconcileDrift            atomic.Int64 // ledger-vs-Stripe discrepancies found by the reconcile audit
	throttledHedges           atomic.Int64
	hashTrustedRedundancy     atomic.Int64
	noPeerRequeues            atomic.Int64
	coldModelHedgesSuppressed atomic.Int64
	endgameRaces              atomic.Int64
	slaMisses                 atomic.Int64
}

var metrics metricsState

type histSeries struct {
	buckets  []int64
	overflow int64
	sum      float64
	count    int64
}

type labeledHistogram struct {
	bounds []float64 // fixed upper bounds, ascending
	mu     sync.Mutex
	series map[string]*histSeries
}

func newLabeledHistogram(bounds []float64) *labeledHistogram {
	return &labeledHistogram{bounds: bounds, series: map[string]*histSeries{}}
}

func (h *labeledHistogram) observe(label string, value float64) {
	h.mu.Lock()
	defer h.mu.Unlock()
	s := h.series[label]
	if s == nil {
		s = &histSeries{buckets: make([]int64, len(h.bounds))}
		h.series[label] = s
	}
	s.count++
	s.sum += value
	placed := false
	for i, bound := range h.bounds {
		if value <= bound {
			s.buckets[i]++
			placed = true
			break
		}
	}
	if !placed {
		s.overflow++
	}
}

type histSnapshot struct {
	label      string
	cumulative []int64
	count      int64
	sum        float64
}

func (h *labeledHistogram) snapshot() []histSnapshot {
	h.mu.Lock()
	labels := make([]string, 0, len(h.series))
	for l := range h.series {
		labels = append(labels, l)
	}
	sort.Strings(labels)
	out := make([]histSnapshot, 0, len(labels))
	for _, l := range labels {
		s := h.series[l]
		cumulative := make([]int64, len(h.bounds))
		var running int64
		for i := range h.bounds {
			running += s.buckets[i]
			cumulative[i] = running
		}
		out = append(out, histSnapshot{label: l, cumulative: cumulative, count: s.count, sum: s.sum})
	}
	h.mu.Unlock()
	return out
}

var transferLatencyBucketsMs = []float64{1, 5, 10, 25, 50, 100, 250, 500, 1000, 2500, 5000, 15000}

var transferSizeBucketsBytes = []float64{
	1 << 10, 4 << 10, 16 << 10, 64 << 10, 256 << 10,
	1 << 20, 4 << 20, 16 << 20, 64 << 20, 256 << 20, 1 << 30,
}

var httpDurationBucketsMs = []float64{1, 2, 5, 10, 25, 50, 100, 250, 500, 1000, 2500, 5000, 15000}

var (
	transferLatency = newLabeledHistogram(transferLatencyBucketsMs)
	transferSize    = newLabeledHistogram(transferSizeBucketsBytes)
)

var httpRequestDuration = newLabeledHistogram(httpDurationBucketsMs)

func observeTransfer(direction string, bytes int, d time.Duration) {
	if bytes <= 0 {
		return
	}
	transferLatency.observe(direction, float64(d)/float64(time.Millisecond))
	transferSize.observe(direction, float64(bytes))
}

func observeHTTPRequest(endpoint string, d time.Duration) {
	httpRequestDuration.observe(endpoint, float64(d)/float64(time.Millisecond))
}

type queueAgeQuantiles struct {
	tier, jobType string
	p50, p95, p99 float64
}

func (s *Store) observabilityQueueAge(ctx context.Context) ([]queueAgeQuantiles, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT j.tier,j.job_type,
		       percentile_cont(0.50) WITHIN GROUP (
		         ORDER BY GREATEST(0,EXTRACT(EPOCH FROM now()-COALESCE(t.visible_at,t.created_at))))::float8,
		       percentile_cont(0.95) WITHIN GROUP (
		         ORDER BY GREATEST(0,EXTRACT(EPOCH FROM now()-COALESCE(t.visible_at,t.created_at))))::float8,
		       percentile_cont(0.99) WITHIN GROUP (
		         ORDER BY GREATEST(0,EXTRACT(EPOCH FROM now()-COALESCE(t.visible_at,t.created_at))))::float8
		  FROM tasks t JOIN jobs j ON j.id=t.job_id
		 WHERE t.status IN ('queued','retrying')
		   AND t.claimed_by IS NULL
		   AND COALESCE(t.visible_at,t.created_at) <= now()
		   AND j.status NOT IN ('cancelled','failed','complete')
		 GROUP BY j.tier,j.job_type
		 ORDER BY j.tier,j.job_type`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []queueAgeQuantiles
	for rows.Next() {
		var q queueAgeQuantiles
		if err := rows.Scan(&q.tier, &q.jobType, &q.p50, &q.p95, &q.p99); err != nil {
			return nil, err
		}
		out = append(out, q)
	}
	return out, rows.Err()
}

type webhookBacklog struct {
	pending, leased, dead int64
	oldestPendingSeconds  float64
}

func (s *Store) observabilityWebhookBacklog(ctx context.Context) (webhookBacklog, error) {
	var out webhookBacklog
	err := s.pool.QueryRow(ctx, `
		SELECT count(*) FILTER (
		         WHERE delivered_at IS NULL AND dead_lettered_at IS NULL
		           AND (lease_token IS NULL OR lease_expires_at <= now())),
		       count(*) FILTER (
		         WHERE delivered_at IS NULL AND dead_lettered_at IS NULL
		           AND lease_token IS NOT NULL AND lease_expires_at > now()),
		       count(*) FILTER (WHERE dead_lettered_at IS NOT NULL),
		       COALESCE(GREATEST(0,EXTRACT(EPOCH FROM now()-MIN(created_at) FILTER (
		         WHERE delivered_at IS NULL AND dead_lettered_at IS NULL))),0)::float8
		  FROM webhooks`).Scan(&out.pending, &out.leased, &out.dead, &out.oldestPendingSeconds)
	return out, err
}

type backupSignal struct {
	configured  bool
	valid       bool
	lastSuccess float64
	ageSeconds  float64
}

func readBackupSignal(now time.Time, path string) backupSignal {
	path = strings.TrimSpace(path)
	if path == "" {
		return backupSignal{}
	}
	out := backupSignal{configured: true}
	f, err := os.Open(path)
	if err != nil {
		return out
	}
	defer f.Close()
	raw, err := io.ReadAll(io.LimitReader(f, 129))
	if err != nil || len(raw) == 0 || len(raw) > 128 {
		return out
	}
	unix, err := strconv.ParseInt(strings.TrimSpace(string(raw)), 10, 64)
	if err != nil || unix <= 0 {
		return out
	}
	when := time.Unix(unix, 0)
	if when.After(now.Add(5 * time.Minute)) {
		return out
	}
	out.valid = true
	out.lastSuccess = float64(unix)
	out.ageSeconds = max(0, now.Sub(when).Seconds())
	return out
}

func metricLabelValue(value string) string {
	value = strings.TrimSpace(value)
	if value == "" {
		return "unknown"
	}
	var b strings.Builder
	for _, r := range value {
		if b.Len() >= 96 {
			break
		}
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9',
			r == '.', r == '_', r == '-', r == '+', r == '/', r == ':', r == '@':
			b.WriteRune(r)
		default:
			b.WriteByte('_')
		}
	}
	if b.Len() == 0 {
		return "unknown"
	}
	return b.String()
}

func (l *tickerLiveness) intervalSnapshot() map[string]float64 {
	l.mu.RLock()
	defer l.mu.RUnlock()
	out := make(map[string]float64, len(l.entries))
	for name, entry := range l.entries {
		out[name] = entry.interval.Seconds()
	}
	return out
}

func writeLabeledHistogram(w http.ResponseWriter, name, help, labelKey string, h *labeledHistogram) {
	fmt.Fprintf(w, "# HELP %s %s\n", name, help)
	fmt.Fprintf(w, "# TYPE %s histogram\n", name)
	for _, s := range h.snapshot() {
		for i, cumulative := range s.cumulative {
			fmt.Fprintf(w, "%s_bucket{%s=%q,le=%q} %d\n",
				name, labelKey, s.label, strconv.FormatFloat(h.bounds[i], 'f', -1, 64), cumulative)
		}
		fmt.Fprintf(w, "%s_bucket{%s=%q,le=\"+Inf\"} %d\n", name, labelKey, s.label, s.count)
		fmt.Fprintf(w, "%s_sum{%s=%q} %s\n", name, labelKey, s.label, strconv.FormatFloat(s.sum, 'f', -1, 64))
		fmt.Fprintf(w, "%s_count{%s=%q} %d\n", name, labelKey, s.label, s.count)
	}
}

func (s *Server) handleMetrics(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/plain; version=0.0.4; charset=utf-8")

	writeCounter(w, "cx_jobs_submitted_total", "Jobs accepted by POST /v1/jobs.", metrics.jobsSubmitted.Load())
	writeCounter(w, "cx_tasks_dispatched_total", "Tasks claimed by workers via poll.", metrics.tasksDispatched.Load())
	writeCounter(w, "cx_tasks_completed_total", "Tasks committed and verified.", metrics.tasksCompleted.Load())
	writeCounter(w, "cx_verification_mismatch_total", "Redundancy/honeypot mismatches detected.", metrics.verificationMismatch.Load())
	writeCounter(w, "cx_payouts_released_total", "Supplier payouts released or marked ready.", metrics.payoutsReleased.Load())
	writeCounter(w, "cx_quarantines_total", "Suppliers auto-quarantined on fraud / low reputation.", metrics.quarantines.Load())
	writeCounter(w, "cx_hedges_total", "Straggler hedge tasks inserted.", metrics.hedges.Load())
	writeCounter(w, "cx_tiebreaks_total", "Third-worker redundancy tiebreak tasks inserted.", metrics.tiebreaks.Load())
	writeCounter(w, "cx_result_merges_total", "Buyer-ready result artifacts merged.", metrics.resultMerges.Load())
	writeCounter(w, "cx_task_failures_total", "Typed task failures reported via the immediate fail endpoint.", metrics.taskFailures.Load())
	writeCounter(w, "cx_quotes_total", "Quotes priced and persisted via POST /v1/quote.", metrics.quotes.Load())
	writeCounter(w, "cx_budget_stops_total", "Capped jobs paused before breach by the Budget Governor.", metrics.budgetStops.Load())
	writeCounter(w, "cx_long_poll_timeouts_total", "Worker long-poll waits that returned empty on timeout.", metrics.longPollTimeouts.Load())
	writeCounter(w, "cx_stuck_cancelled_total", "Stuck runs auto-cancelled by the watchdog (repeat stall past the deadline; checkpointed + settled at completed work).", metrics.stuckCancels.Load())
	writeCounter(w, "cx_stuck_rescued_total", "Stuck runs rescued by the watchdog's first strike (unfinished tasks requeued to a different machine).", metrics.stuckRescues.Load())
	writeCounter(w, "cx_watchdog_near_miss_total", "Jobs that finalized LATE (realized > 1.2x the predicted eta_secs)  -  calibration data for the watchdog's ETA factor.", metrics.watchdogNearMiss.Load())
	writeCounter(w, "cx_reconcile_drift_total", "Ledger-vs-Stripe discrepancies found by the reconcile audit (a genuine anomaly, not routine).", metrics.reconcileDrift.Load())
	writeCounter(w, "cx_throttled_hedges_total", "Straggler hedges triggered by a worker's live throttled=true heartbeat (memory pressure or a detected sustained throughput drop), ahead of the elapsed-time hedge/stale-worker thresholds.", metrics.throttledHedges.Load())
	writeCounter(w, "cx_hash_trusted_redundancy_total", "Redundancy comparisons that trusted a worker/peer SHA-256 match instead of re-fetching the peer's result object from S3 inside the commit request.", metrics.hashTrustedRedundancy.Load())
	writeCounter(w, "cx_no_peer_requeues_total", "Wedged tasks requeued by the class-aware no-peer watchdog (heartbeating worker holding a task with no eligible same-class peer), escaping the 30-minute stale reaper.", metrics.noPeerRequeues.Load())
	writeCounter(w, "cx_cold_model_hedges_suppressed_total", "Straggler hedges suppressed because the slowness was an expected cold GGUF load (worker not yet warm on the model), averting a spurious cold-to-cold hedge storm.", metrics.coldModelHedgesSuppressed.Load())
	writeCounter(w, "cx_endgame_races_total", "Endgame-race duplicates actually inserted onto an idle warm same-class peer (Speed Lane wave 1B): the slowest running chunk raced the moment a job's queue emptied, ahead of the 90s hedge window. A subset of cx_hedges_total, split out so the fan-out planner's tail-latency work has its own signal.", metrics.endgameRaces.Load())
	writeCounter(w, "cx_sla_misses_total", "Jobs whose bound speed-SLA settled as MISSED (Speed Lane wave 2A): the buyer-visible span exceeded the guarantee, sla_met stamped false, the once-only sla_refund credit recorded. Counted once per job (only the settle call that decided the miss bumps it).", metrics.slaMisses.Load())
	writeCounter(w, "cx_no_hedge_peer_total", "Dispatch-time heterogeneous-fleet degradation: a redundancy/hedge peer was needed but no eligible same-class peer existed on the fleet (silent loss of hedging + warm-routing). Alerted on by monitoring/alerts.yml.", NoHedgePeerCount())

	build := currentControlBuildInfo()
	fmt.Fprintf(w, "# HELP cx_release_info Immutable control-plane release identity; exactly one series per process.\n")
	fmt.Fprintf(w, "# TYPE cx_release_info gauge\n")
	fmt.Fprintf(w, "cx_release_info{version=%q,commit=%q,build_date=%q,go_version=%q,platform=%q,modified=%q} 1\n",
		metricLabelValue(build.Version), metricLabelValue(build.Commit), metricLabelValue(build.BuildDate),
		metricLabelValue(build.GoVersion), metricLabelValue(build.Platform), strconv.FormatBool(build.Modified))

	backup := readBackupSignal(time.Now(), os.Getenv("CX_BACKUP_STATUS_FILE"))
	fmt.Fprintf(w, "# HELP cx_backup_signal_configured Whether CX_BACKUP_STATUS_FILE is configured for this process.\n")
	fmt.Fprintf(w, "# TYPE cx_backup_signal_configured gauge\n")
	fmt.Fprintf(w, "cx_backup_signal_configured %d\n", boolMetric(backup.configured))
	fmt.Fprintf(w, "# HELP cx_backup_signal_valid Whether the bounded backup timestamp input is readable and valid.\n")
	fmt.Fprintf(w, "# TYPE cx_backup_signal_valid gauge\n")
	fmt.Fprintf(w, "cx_backup_signal_valid %d\n", boolMetric(backup.valid))
	if backup.valid {
		fmt.Fprintf(w, "# HELP cx_backup_last_success_timestamp_seconds Unix timestamp of the latest post-upload-verified offsite backup.\n")
		fmt.Fprintf(w, "# TYPE cx_backup_last_success_timestamp_seconds gauge\n")
		fmt.Fprintf(w, "cx_backup_last_success_timestamp_seconds %.0f\n", backup.lastSuccess)
		fmt.Fprintf(w, "# HELP cx_backup_age_seconds Seconds since the latest post-upload-verified offsite backup.\n")
		fmt.Fprintf(w, "# TYPE cx_backup_age_seconds gauge\n")
		fmt.Fprintf(w, "cx_backup_age_seconds %.3f\n", backup.ageSeconds)
	}

	ctx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
	defer cancel()

	pool := s.store.pool.Stat()
	maxConns := pool.MaxConns()
	utilization := 0.0
	if maxConns > 0 {
		utilization = float64(pool.AcquiredConns()) / float64(maxConns)
	}
	fmt.Fprintf(w, "# HELP cx_db_pool_connections PostgreSQL pool connections by bounded state.\n")
	fmt.Fprintf(w, "# TYPE cx_db_pool_connections gauge\n")
	fmt.Fprintf(w, "cx_db_pool_connections{state=\"acquired\"} %d\n", pool.AcquiredConns())
	fmt.Fprintf(w, "cx_db_pool_connections{state=\"idle\"} %d\n", pool.IdleConns())
	fmt.Fprintf(w, "cx_db_pool_connections{state=\"constructing\"} %d\n", pool.ConstructingConns())
	fmt.Fprintf(w, "cx_db_pool_connections{state=\"total\"} %d\n", pool.TotalConns())
	fmt.Fprintf(w, "cx_db_pool_connections{state=\"max\"} %d\n", maxConns)
	fmt.Fprintf(w, "# HELP cx_db_pool_utilization_ratio Fraction of configured PostgreSQL connections currently acquired.\n")
	fmt.Fprintf(w, "# TYPE cx_db_pool_utilization_ratio gauge\n")
	fmt.Fprintf(w, "cx_db_pool_utilization_ratio %.6f\n", utilization)
	fmt.Fprintf(w, "# HELP cx_db_pool_empty_acquire_wait_seconds_total Cumulative wait time for a connection while the pool was empty.\n")
	fmt.Fprintf(w, "# TYPE cx_db_pool_empty_acquire_wait_seconds_total counter\n")
	fmt.Fprintf(w, "cx_db_pool_empty_acquire_wait_seconds_total %.6f\n", pool.EmptyAcquireWaitTime().Seconds())
	fmt.Fprintf(w, "# HELP cx_db_pool_canceled_acquires_total Cumulative connection acquisitions cancelled while waiting.\n")
	fmt.Fprintf(w, "# TYPE cx_db_pool_canceled_acquires_total counter\n")
	fmt.Fprintf(w, "cx_db_pool_canceled_acquires_total %d\n", pool.CanceledAcquireCount())

	storageProbeCtx, storageProbeCancel := context.WithTimeout(ctx, 2*time.Second)
	storageProbeStarted := time.Now()
	storageUp := false
	if s.storage != nil && s.storage.internal != nil {
		exists, err := s.storage.internal.BucketExists(storageProbeCtx, s.storage.bucket)
		storageUp = err == nil && exists
	}
	storageProbeCancel()
	fmt.Fprintf(w, "# HELP cx_object_storage_up Whether the configured artifact bucket answered a bounded active probe.\n")
	fmt.Fprintf(w, "# TYPE cx_object_storage_up gauge\n")
	fmt.Fprintf(w, "cx_object_storage_up %d\n", boolMetric(storageUp))
	fmt.Fprintf(w, "# HELP cx_object_storage_probe_duration_seconds Duration of the bounded artifact-store health probe.\n")
	fmt.Fprintf(w, "# TYPE cx_object_storage_probe_duration_seconds gauge\n")
	fmt.Fprintf(w, "cx_object_storage_probe_duration_seconds %.6f\n", time.Since(storageProbeStarted).Seconds())

	fmt.Fprintf(w, "# HELP cx_active_workers Workers seen within the last 60s.\n")
	fmt.Fprintf(w, "# TYPE cx_active_workers gauge\n")
	if n, err := s.store.ActiveWorkerCount(ctx); err != nil {
		fmt.Fprintf(w, "# cx_active_workers unavailable: %s\n", err.Error())
	} else {
		fmt.Fprintf(w, "cx_active_workers %d\n", n)
	}

	fmt.Fprintf(w, "# HELP cx_queue_depth Claimable (queued/retrying, visible, unclaimed) tasks.\n")
	fmt.Fprintf(w, "# TYPE cx_queue_depth gauge\n")
	if rows, err := s.store.QueueDepth(ctx); err != nil {
		fmt.Fprintf(w, "# cx_queue_depth unavailable: %s\n", err.Error())
	} else {
		for _, qd := range rows {
			fmt.Fprintf(w, "cx_queue_depth{tier=%q,job_type=%q} %d\n", qd.Tier, qd.JobType, qd.Count)
		}
	}

	fmt.Fprintf(w, "# HELP cx_queue_age_seconds Claimable task queue-age quantiles by bounded tier and job type.\n")
	fmt.Fprintf(w, "# TYPE cx_queue_age_seconds gauge\n")
	if rows, err := s.store.observabilityQueueAge(ctx); err != nil {
		fmt.Fprintf(w, "# cx_queue_age_seconds unavailable: %s\n", err.Error())
	} else {
		for _, row := range rows {
			fmt.Fprintf(w, "cx_queue_age_seconds{tier=%q,job_type=%q,quantile=\"0.50\"} %.3f\n", row.tier, row.jobType, row.p50)
			fmt.Fprintf(w, "cx_queue_age_seconds{tier=%q,job_type=%q,quantile=\"0.95\"} %.3f\n", row.tier, row.jobType, row.p95)
			fmt.Fprintf(w, "cx_queue_age_seconds{tier=%q,job_type=%q,quantile=\"0.99\"} %.3f\n", row.tier, row.jobType, row.p99)
		}
	}

	fmt.Fprintf(w, "# HELP cx_webhook_backlog Webhook delivery rows by bounded lifecycle state.\n")
	fmt.Fprintf(w, "# TYPE cx_webhook_backlog gauge\n")
	if backlog, err := s.store.observabilityWebhookBacklog(ctx); err != nil {
		fmt.Fprintf(w, "# cx_webhook_backlog unavailable: %s\n", err.Error())
	} else {
		fmt.Fprintf(w, "cx_webhook_backlog{state=\"pending\"} %d\n", backlog.pending)
		fmt.Fprintf(w, "cx_webhook_backlog{state=\"leased\"} %d\n", backlog.leased)
		fmt.Fprintf(w, "cx_webhook_backlog{state=\"dead_letter\"} %d\n", backlog.dead)
		fmt.Fprintf(w, "# HELP cx_webhook_oldest_pending_age_seconds Age of the oldest undelivered non-dead-letter webhook.\n")
		fmt.Fprintf(w, "# TYPE cx_webhook_oldest_pending_age_seconds gauge\n")
		fmt.Fprintf(w, "cx_webhook_oldest_pending_age_seconds %.3f\n", backlog.oldestPendingSeconds)
	}

	now := time.Now()
	fmt.Fprintf(w, "# HELP cx_ticker_seconds_since_success Seconds since a background ticker last completed a successful run.\n")
	fmt.Fprintf(w, "# TYPE cx_ticker_seconds_since_success gauge\n")
	for name, secs := range liveness.snapshot(now, workersStarted()) {
		fmt.Fprintf(w, "cx_ticker_seconds_since_success{ticker=%q} %.3f\n", name, secs)
	}
	fmt.Fprintf(w, "# HELP cx_ticker_interval_seconds Configured interval for each bounded background ticker.\n")
	fmt.Fprintf(w, "# TYPE cx_ticker_interval_seconds gauge\n")
	for name, seconds := range liveness.intervalSnapshot() {
		fmt.Fprintf(w, "cx_ticker_interval_seconds{ticker=%q} %.3f\n", name, seconds)
	}

	fmt.Fprintf(w, "# HELP cx_ticker_failures_total Lifetime count of a background ticker's fn returning an error.\n")
	fmt.Fprintf(w, "# TYPE cx_ticker_failures_total counter\n")
	for name, n := range liveness.failureSnapshot() {
		fmt.Fprintf(w, "cx_ticker_failures_total{ticker=%q} %d\n", name, n)
	}

	fmt.Fprintf(w, "# HELP cx_telemetry_table_rows Live row count of a retention-swept telemetry table.\n")
	fmt.Fprintf(w, "# TYPE cx_telemetry_table_rows gauge\n")
	if counts, err := s.store.TelemetryTableCounts(ctx); err != nil {
		fmt.Fprintf(w, "# cx_telemetry_table_rows unavailable: %s\n", err.Error())
	} else {
		for _, table := range telemetryTables {
			fmt.Fprintf(w, "cx_telemetry_table_rows{table=%q} %d\n", table, counts[table])
		}
	}

	fmt.Fprintf(w, "# HELP cx_task_duration_ms Committed task wall-time (ms), by job_type.\n")
	fmt.Fprintf(w, "# TYPE cx_task_duration_ms histogram\n")
	if hist, err := s.store.TaskDurationHistogram(ctx); err != nil {
		fmt.Fprintf(w, "# cx_task_duration_ms unavailable: %s\n", err.Error())
	} else {
		for _, row := range hist {
			for i, cumulative := range row.Buckets {
				fmt.Fprintf(w, "cx_task_duration_ms_bucket{job_type=%q,le=%q} %d\n",
					row.JobType, strconv.FormatFloat(taskDurationBucketsMs[i], 'f', -1, 64), cumulative)
			}
			fmt.Fprintf(w, "cx_task_duration_ms_bucket{job_type=%q,le=\"+Inf\"} %d\n", row.JobType, row.Count)
			fmt.Fprintf(w, "cx_task_duration_ms_sum{job_type=%q} %d\n", row.JobType, row.SumMs)
			fmt.Fprintf(w, "cx_task_duration_ms_count{job_type=%q} %d\n", row.JobType, row.Count)
		}
	}

	fmt.Fprintf(w, "# HELP cx_claim_duration_ms ClaimTasksTx (the /v1/worker/poll hot path) wall time, ms.\n")
	fmt.Fprintf(w, "# TYPE cx_claim_duration_ms histogram\n")
	{
		buckets, count, sumMs := claimDuration.snapshot()
		for i, cumulative := range buckets {
			fmt.Fprintf(w, "cx_claim_duration_ms_bucket{le=%q} %d\n",
				strconv.FormatFloat(claimDurationBucketsMs[i], 'f', -1, 64), cumulative)
		}
		fmt.Fprintf(w, "cx_claim_duration_ms_bucket{le=\"+Inf\"} %d\n", count)
		fmt.Fprintf(w, "cx_claim_duration_ms_sum %d\n", sumMs)
		fmt.Fprintf(w, "cx_claim_duration_ms_count %d\n", count)
	}

	fmt.Fprintf(w, "# HELP cx_latency_phase_ms p50/p90 milliseconds per end-to-end latency phase, by job_type.\n")
	fmt.Fprintf(w, "# TYPE cx_latency_phase_ms gauge\n")
	if phases, err := s.store.LatencyPhaseDecomposition(ctx); err != nil {
		fmt.Fprintf(w, "# cx_latency_phase_ms unavailable: %s\n", err.Error())
	} else {
		for _, p := range phases {
			fmt.Fprintf(w, "cx_latency_phase_ms{job_type=%q,phase=\"queue_wait\",quantile=\"0.5\"} %.3f\n", p.JobType, p.QueueWaitP50Ms)
			fmt.Fprintf(w, "cx_latency_phase_ms{job_type=%q,phase=\"queue_wait\",quantile=\"0.9\"} %.3f\n", p.JobType, p.QueueWaitP90Ms)
			fmt.Fprintf(w, "cx_latency_phase_ms{job_type=%q,phase=\"dispatch_overhead\",quantile=\"0.5\"} %.3f\n", p.JobType, p.DispatchOverheadP50Ms)
			fmt.Fprintf(w, "cx_latency_phase_ms{job_type=%q,phase=\"dispatch_overhead\",quantile=\"0.9\"} %.3f\n", p.JobType, p.DispatchOverheadP90Ms)
			fmt.Fprintf(w, "cx_latency_phase_ms{job_type=%q,phase=\"run\",quantile=\"0.5\"} %.3f\n", p.JobType, p.RunP50Ms)
			fmt.Fprintf(w, "cx_latency_phase_ms{job_type=%q,phase=\"run\",quantile=\"0.9\"} %.3f\n", p.JobType, p.RunP90Ms)
		}
	}

	writeLabeledHistogram(w, "cx_transfer_duration_ms",
		"Control-side object-store transfer wall time (ms), by direction (get|put).",
		"direction", transferLatency)
	writeLabeledHistogram(w, "cx_transfer_bytes",
		"Control-side object-store transfer size (bytes), by direction (get|put).",
		"direction", transferSize)

	writeLabeledHistogram(w, "cx_http_request_duration_ms",
		"HTTP handler wall time (ms), by matched endpoint (method + route pattern).",
		"endpoint", httpRequestDuration)
}

func writeCounter(w http.ResponseWriter, name, help string, v int64) {
	fmt.Fprintf(w, "# HELP %s %s\n", name, help)
	fmt.Fprintf(w, "# TYPE %s counter\n", name)
	fmt.Fprintf(w, "%s %d\n", name, v)
}

func boolMetric(value bool) int {
	if value {
		return 1
	}
	return 0
}
