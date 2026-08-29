package main

import (
	"fmt"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"
)

// Measurement harness for the batch claim path (ClaimTasksTx / ClaimTaskSQL).
// Run with:
//
//	go test -count=1 -timeout 10m -v -run TestMeasureClaimPathSLADeferral .
//
// Writes plan text and wall-time stats to the path in G054_MEASURE_OUT (default
// /tmp/g054-claim-measure.txt). Not a correctness gate.

func TestMeasureClaimPathSLADeferral(t *testing.T) {
	t.Parallel()
	ctx, store, pool := openIsolatedTestStore(t)
	// Fixture matches the no-SLA regression case: cheaper_ask online, young task.
	// EXPLAIN uses a non-mutating SELECT of the claim CTE so the plan is the
	// claim path without permanently claiming.
	cheap, dear, jobID, taskID := setupSLADeferFixture(t, ctx, store, pool, nil, nil)
	_ = jobID
	_ = taskID
	_ = cheap

	// Resolve claim params the same way ClaimTasksTx does.
	var hwClass string
	var selfMinPayout float64
	if err := pool.QueryRow(ctx,
		`SELECT COALESCE(hw_class,''), COALESCE(min_payout_usd_hr, 0) FROM workers WHERE id=$1`,
		dear.workerID,
	).Scan(&hwClass, &selfMinPayout); err != nil {
		t.Fatal(err)
	}
	selfCostRank := hwClassCostRank(hwClass)
	var rep float32
	var jobsDone uint64
	if err := pool.QueryRow(ctx,
		`SELECT s.reputation, s.completed_tasks FROM suppliers s WHERE s.id=$1`,
		dear.supplierID,
	).Scan(&rep, &jobsDone); err != nil {
		t.Fatal(err)
	}
	tier := reputationTier(rep, jobsDone)
	settlement := SettlementCurrencyCode()
	if settlement == "" {
		t.Fatal("settlement currency unset")
	}

	// Strip the UPDATE/RETURNING so EXPLAIN plans the claim selection only
	// (the expensive MATERIALIZED eligible_jobs + next CTE). The production
	// path still ends in UPDATE tasks ... FOR UPDATE SKIP LOCKED LIMIT 1;
	// that terminal step is unchanged by the SLA predicate.
	full := ClaimTaskSQL("t.claimed_by IS NULL")
	// Keep everything through the next CTE's SELECT/FOR UPDATE/LIMIT; drop UPDATE.
	idx := strings.Index(full, "\n\t UPDATE tasks")
	if idx < 0 {
		// tolerate different whitespace
		idx = strings.Index(full, "UPDATE tasks")
	}
	if idx < 0 {
		t.Fatal("could not locate UPDATE tasks in ClaimTaskSQL")
	}
	selectSQL := full[:idx]
	// next CTE ends with FOR UPDATE OF t SKIP LOCKED LIMIT 1 — for EXPLAIN as a
	// standalone statement we wrap: EXPLAIN of the WITH ... SELECT from next.
	// The claim SQL is WITH me AS (...), eligible_jobs AS (...), next AS (... SELECT ... LIMIT 1)
	// then UPDATE. So selectSQL is the WITH chain ending at next. Append SELECT * FROM next.
	explainSQL := "EXPLAIN (ANALYZE, BUFFERS, FORMAT TEXT)\n" + selectSQL + "\nSELECT * FROM next"

	rows, err := pool.Query(ctx, explainSQL,
		dear.workerID, int(tier), selfCostRank, generatedRuntimeMatrixSHA256,
		selfMinPayout, askDeferralWindow.String(), settlement,
	)
	if err != nil {
		t.Fatalf("EXPLAIN claim path: %v\nSQL head: %.200s", err, explainSQL)
	}
	var planLines []string
	for rows.Next() {
		var line string
		if err := rows.Scan(&line); err != nil {
			rows.Close()
			t.Fatal(err)
		}
		planLines = append(planLines, line)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}

	// Wall time: N ClaimTasksTx calls on a fresh young task each time.
	// Re-queue after each claim so the path stays "young + cheaper_ask online".
	const samples = 30
	durations := make([]time.Duration, 0, samples)
	for i := 0; i < samples; i++ {
		// Ensure task is ready and young for every sample.
		if _, err := pool.Exec(ctx, `
			UPDATE tasks SET status='queued', claimed_by=NULL, worker_id=NULL,
			        claimed_at=NULL, started_at=NULL,
			        created_at=now(), visible_at=NULL
			 WHERE id=$1`, taskID); err != nil {
			t.Fatal(err)
		}
		start := time.Now()
		_, err := store.ClaimTasksTx(ctx, WorkerAuth{WorkerID: dear.workerID, SupplierID: dear.supplierID})
		elapsed := time.Since(start)
		if err != nil {
			t.Fatalf("claim sample %d: %v", i, err)
		}
		durations = append(durations, elapsed)
	}

	var sum time.Duration
	minD, maxD := durations[0], durations[0]
	for _, d := range durations {
		sum += d
		if d < minD {
			minD = d
		}
		if d > maxD {
			maxD = d
		}
	}
	avg := sum / time.Duration(len(durations))

	loadNote := loadAverageNote()
	outPath := os.Getenv("G054_MEASURE_OUT")
	if outPath == "" {
		outPath = "/tmp/g054-claim-measure.txt"
	}
	var b strings.Builder
	fmt.Fprintf(&b, "phase=%s samples=%d load=%s\n", os.Getenv("G054_MEASURE_PHASE"), samples, loadNote)
	fmt.Fprintf(&b, "claim_wall_min_ms=%.3f avg_ms=%.3f max_ms=%.3f\n",
		float64(minD.Microseconds())/1000, float64(avg.Microseconds())/1000, float64(maxD.Microseconds())/1000)
	fmt.Fprintf(&b, "worker=%s task=%s cheaper_online_peer=%s\n", dear.workerID, taskID, cheap.workerID)
	b.WriteString("--- EXPLAIN (ANALYZE, BUFFERS) ---\n")
	for _, line := range planLines {
		b.WriteString(line)
		b.WriteByte('\n')
	}
	// Plan shape: strip costs/timings for comparison.
	b.WriteString("--- PLAN SHAPE (node types only) ---\n")
	for _, line := range planLines {
		shape := planShapeLine(line)
		if shape != "" {
			b.WriteString(shape)
			b.WriteByte('\n')
		}
	}
	if err := os.WriteFile(outPath, []byte(b.String()), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Logf("wrote measure to %s\n%s", outPath, b.String())
}

func planShapeLine(line string) string {
	// Keep indentation + leading node type; drop cost/actuals.
	// e.g. " Nested Loop  (cost=... rows=...)" → " Nested Loop"
	trimmed := strings.TrimRight(line, " \t")
	if trimmed == "" {
		return ""
	}
	// Skip pure Planning/Execution Timing / Buffers summary lines for shape.
	if strings.HasPrefix(strings.TrimSpace(trimmed), "Planning") ||
		strings.HasPrefix(strings.TrimSpace(trimmed), "Execution") ||
		strings.HasPrefix(strings.TrimSpace(trimmed), "Buffers:") ||
		strings.HasPrefix(strings.TrimSpace(trimmed), "I/O Timings:") {
		return ""
	}
	if i := strings.Index(trimmed, "  (cost="); i >= 0 {
		return trimmed[:i]
	}
	if i := strings.Index(trimmed, " (cost="); i >= 0 {
		return trimmed[:i]
	}
	if i := strings.Index(trimmed, "  (actual"); i >= 0 {
		return trimmed[:i]
	}
	return trimmed
}

func loadAverageNote() string {
	out, err := exec.Command("sysctl", "-n", "vm.loadavg").Output()
	if err != nil {
		return "unavailable"
	}
	return strings.TrimSpace(string(out))
}
