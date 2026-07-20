package main

import (
	"context"
	"fmt"
	"net/http"
	"sort"
	"strconv"
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

	ctx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
	defer cancel()

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

	now := time.Now()
	fmt.Fprintf(w, "# HELP cx_ticker_seconds_since_success Seconds since a background ticker last completed a successful run.\n")
	fmt.Fprintf(w, "# TYPE cx_ticker_seconds_since_success gauge\n")
	for name, secs := range liveness.snapshot(now, workersStarted()) {
		fmt.Fprintf(w, "cx_ticker_seconds_since_success{ticker=%q} %.3f\n", name, secs)
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
