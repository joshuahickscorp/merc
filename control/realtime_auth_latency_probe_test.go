package main

import (
	"context"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"runtime/debug"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
)

// TestRealtimeAuthLatencyProbe measures control-plane admission latency with
// seeded execution_contracts history. Opt-in: set MERC_AUTH_LATENCY_PROBE=1.
// Writes evidence/perf/realtime-auth-reputation-*.json.
//
// This is NOT end-to-end TTFT against a real engine. It times
// AuthorizeRealtimeContract (buyer funding lock + offer selection + EXECUTING
// insert) and the isolated reputation read (legacy full aggregate vs stats
// table lookup).
func TestRealtimeAuthLatencyProbe(t *testing.T) {
	if os.Getenv("MERC_AUTH_LATENCY_PROBE") != "1" {
		t.Skip("set MERC_AUTH_LATENCY_PROBE=1 to run admission latency probe")
	}
	installSettlementCurrencyForTest(t, "usd")
	// Isolated DB so 100k synthetic contracts do not pollute the shared suite DB
	// (settlements are append-only and cannot be cleaned up cheaply).
	ctx, store, pool := openIsolatedTestStore(t)
	t.Setenv("MERC_TOKEN_KEY", "auth-latency-probe-key-with-32-bytes-min!")

	// Use an isolated profile-scoped seed so we do not TRUNCATE shared tables
	// for minutes of history seeding; we seed contracts attributed to a real
	// supplier/profile and leave them for the measurement window, then clean.
	profile := sortedVLLMProfiles()[0]
	// Authorizing buyer stays lean so the funding check (sum of EXECUTING
	// ceilings for this buyer) is not confused with the reputation scan.
	// History is seeded under a separate ballast buyer — reputation aggregates
	// by runtime_profile_id, not buyer_id.
	buyerID, err := store.CreateBuyerAccount(ctx,
		"auth-lat-"+uuid.NewString()+"@example.test", "integration-password", 10_000)
	must(t, err)
	ballastBuyerID, err := store.CreateBuyerAccount(ctx,
		"auth-lat-ballast-"+uuid.NewString()+"@example.test", "integration-password", 1)
	must(t, err)

	// One live offer with huge capacity for concurrent authorizations.
	worker := newRealtimeClearingOffer(t, ctx, store, pool, profile, "HOT", 0.08, 0.30, 10_000)
	// Keep last_seen fresh across the long seed/measure window.
	refresh := time.NewTicker(10 * time.Second)
	t.Cleanup(refresh.Stop)
	go func() {
		for range refresh.C {
			c, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			_, _ = pool.Exec(c, `
				UPDATE realtime_worker_offers
				   SET last_seen_at=now(), available_sequences=max_active_sequences, status='ACTIVE'
				 WHERE worker_id=$1 AND runtime_profile_id=$2`,
				worker.WorkerID, profile.RuntimeProfileID)
			cancel()
		}
	}()

	commit := mercSourceCommit()
	sizes := []int{1_300, 100_000}
	concurrencies := []int{1, 8, 32}
	samplesPerCell := 40

	type cell struct {
		Rows        int                `json:"execution_contracts_rows"`
		Concurrency int                `json:"concurrency"`
		Samples     int                `json:"samples"`
		AuthorizeMs latencySummaryJSON `json:"authorize_admission_ms"`
		LegacyAggMs latencySummaryJSON `json:"legacy_reputation_aggregate_ms"`
		StatsReadMs latencySummaryJSON `json:"stats_table_reputation_read_ms"`
		Notes       string             `json:"notes"`
	}
	var cells []cell

	// Settlements are append-only, so sizes are reached by adding rows (never
	// deleting). Measure after each cumulative target.
	var seeded int
	for _, n := range sizes {
		delta := n - seeded
		if delta < 0 {
			delta = 0
		}
		if delta > 0 {
			t.Logf("seeding +%d terminal contracts (target ~%d) on profile %s",
				delta, n, profile.RuntimeProfileID)
			seedBulkTerminalContracts(t, ctx, pool, ballastBuyerID, worker, profile, delta)
			seeded += delta
		}

		// Confirm row count on profile (may include prior debris; report actual).
		var actualRows int
		if err := pool.QueryRow(ctx, `
			SELECT count(*) FROM execution_contracts WHERE runtime_profile_id=$1`,
			profile.RuntimeProfileID).Scan(&actualRows); err != nil {
			t.Fatal(err)
		}
		t.Logf("profile has %d execution_contracts rows", actualRows)

		// Confirm stats populated for the worker supplier.
		var attempts int
		if err := pool.QueryRow(ctx, `
			SELECT terminal_attempts FROM realtime_supplier_outcome_stats
			 WHERE runtime_profile_id=$1 AND supplier_id=$2`,
			profile.RuntimeProfileID, worker.SupplierID).Scan(&attempts); err != nil {
			t.Fatalf("stats row missing after seed: %v", err)
		}
		if attempts < n/2 {
			t.Fatalf("stats terminal_attempts=%d too low for seed n=%d", attempts, n)
		}

		for _, c := range concurrencies {
			// Replenish capacity between cells.
			if _, err := pool.Exec(ctx, `
				UPDATE realtime_worker_offers
				   SET available_sequences=max_active_sequences, status='ACTIVE', last_seen_at=now()
				 WHERE worker_id=$1 AND runtime_profile_id=$2`,
				worker.WorkerID, profile.RuntimeProfileID); err != nil {
				t.Fatal(err)
			}

			authSamples := measureConcurrent(t, c, samplesPerCell, func() time.Duration {
				start := time.Now()
				contract, _, err := store.AuthorizeRealtimeContract(context.Background(), RealtimeContractAuthorization{
					RequestID: "lat-" + uuid.NewString(), BuyerID: buyerID, Profile: profile,
					InputCommitment: strings.Repeat("a", 64), RequestSHA256: strings.Repeat("b", 64),
					MaximumPriceUSD: 0.001, EstimatedPriceUSD: 0.0005, DeadlineAt: time.Now().Add(time.Minute),
					MaximumPromptTokens: 8_330, MaximumCompletionTokens: 1,
					EstimatedPromptTokens: 4_163, EstimatedCompletionTokens: 1, BuyerDeclaredCeilingUSD: 0.0011,
				})
				elapsed := time.Since(start)
				if err != nil {
					t.Errorf("authorize: %v", err)
					return elapsed
				}
				// Fail fast without settlement work dominating — void via failure.
				_, _ = store.FinalizeRealtimeFailure(context.Background(), contract.ID, uuid.New(), 500, 1,
					"latency_probe", "probe teardown", false)
				return elapsed
			})

			legacySamples := measureConcurrent(t, c, samplesPerCell, func() time.Duration {
				start := time.Now()
				_, err := probeRealtimeClearingWinnerLegacy(context.Background(), pool,
					profile.RuntimeProfileID, profile.ProfileSHA256,
					profile.BuyerInputUSDPerMillionTokens, profile.BuyerOutputUSDPerMillionTokens)
				_ = err
				return time.Since(start)
			})

			statsSamples := measureConcurrent(t, c, samplesPerCell, func() time.Duration {
				start := time.Now()
				_, err := probeRealtimeClearingWinnerStats(context.Background(), pool,
					profile.RuntimeProfileID, profile.ProfileSHA256,
					profile.BuyerInputUSDPerMillionTokens, profile.BuyerOutputUSDPerMillionTokens)
				_ = err
				return time.Since(start)
			})

			cells = append(cells, cell{
				Rows:        actualRows,
				Concurrency: c,
				Samples:     samplesPerCell,
				AuthorizeMs: summarizeLatency(authSamples),
				LegacyAggMs: summarizeLatency(legacySamples),
				StatsReadMs: summarizeLatency(statsSamples),
				Notes: "authorize_admission_ms is full AuthorizeRealtimeContract after the change " +
					"(stats join). legacy_reputation_aggregate_ms is the pre-change full-table CTE " +
					"ranking probe (the term that dominated before). stats_table_reputation_read_ms " +
					"is the same ranking with the incremental table.",
			})
			t.Logf("rows=%d c=%d authorize p50=%.2fms p95=%.2fms  legacy p50=%.2fms  stats p50=%.2fms",
				actualRows, c,
				cells[len(cells)-1].AuthorizeMs.P50, cells[len(cells)-1].AuthorizeMs.P95,
				cells[len(cells)-1].LegacyAggMs.P50, cells[len(cells)-1].StatsReadMs.P50)
		}
	}

	out := map[string]any{
		"schema_version":     1,
		"kind":               "realtime_auth_reputation_admission_latency",
		"measured_at":        time.Now().UTC().Format(time.RFC3339),
		"merc_source_commit": commit,
		"scope": map[string]any{
			"what": "control-plane realtime admission latency (AuthorizeRealtimeContract) " +
				"and isolated reputation ranking probes",
			"not_measured": "end-to-end TTFT against a real engine; GPU/CUDA c=32 deficit; " +
				"provider or Stripe calls",
			"runtime_profile_id":   profile.RuntimeProfileID,
			"samples_per_cell":     samplesPerCell,
			"concurrency_levels":   concurrencies,
			"seed_row_targets":     sizes,
			"buyer_funding_lock":   "unchanged scope (advisory xact lock held through EXECUTING insert)",
			"reputation_mechanism": "realtime_supplier_outcome_stats incremental counters via triggers",
		},
		"cells": cells,
		"interpretation": "" +
			"Before this change, authorize ran the full execution_contracts aggregate inside the " +
			"buyer funding critical section; legacy_reputation_aggregate_ms is that cost. After, " +
			"authorize joins realtime_supplier_outcome_stats (stats_table_reputation_read_ms). " +
			"authorize_admission_ms is the full admission path after the change. The old path's " +
			"cost scaled with table size; the new path's reputation term must not.",
	}

	dir := filepath.Join("..", "evidence", "perf")
	name := fmt.Sprintf("realtime-auth-reputation-%s.json", time.Now().UTC().Format("20060102T150405Z"))
	path := filepath.Join(dir, name)
	id, bin, err := DefaultBoundIdentity("..", "control/realtime_auth_latency_probe_test.go",
		"embedded scope + cells", "embedded cells[].samples")
	must(t, err)
	if err := WriteBoundEvidenceJSON(EvidenceWriteRequest{
		RepoRoot: "..", Path: path, Payload: out,
		Identity: id, BuildBinaryPath: bin,
	}); err != nil {
		t.Fatal(err)
	}
	// Stable alias for the latest probe on this branch (also bound).
	alias := filepath.Join(dir, "realtime-auth-reputation-latest.json")
	if err := WriteBoundEvidenceJSON(EvidenceWriteRequest{
		RepoRoot: "..", Path: alias, Payload: out,
		Identity: id, BuildBinaryPath: bin,
	}); err != nil {
		t.Fatal(err)
	}
	t.Logf("wrote %s", path)
}

type latencySummaryJSON struct {
	P50 float64 `json:"p50"`
	P95 float64 `json:"p95"`
	Min float64 `json:"min"`
	Max float64 `json:"max"`
	N   int     `json:"n"`
}

func summarizeLatency(samples []time.Duration) latencySummaryJSON {
	if len(samples) == 0 {
		return latencySummaryJSON{}
	}
	ms := make([]float64, len(samples))
	for i, d := range samples {
		ms[i] = float64(d) / float64(time.Millisecond)
	}
	sort.Float64s(ms)
	pct := func(p float64) float64 {
		if len(ms) == 1 {
			return ms[0]
		}
		idx := int(math.Ceil(p*float64(len(ms)))) - 1
		if idx < 0 {
			idx = 0
		}
		if idx >= len(ms) {
			idx = len(ms) - 1
		}
		return ms[idx]
	}
	return latencySummaryJSON{
		P50: pct(0.50),
		P95: pct(0.95),
		Min: ms[0],
		Max: ms[len(ms)-1],
		N:   len(ms),
	}
}

func measureConcurrent(t *testing.T, concurrency, samples int, fn func() time.Duration) []time.Duration {
	t.Helper()
	if concurrency < 1 {
		concurrency = 1
	}
	out := make([]time.Duration, samples)
	var wg sync.WaitGroup
	jobs := make(chan int)
	for i := 0; i < concurrency; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for idx := range jobs {
				out[idx] = fn()
			}
		}()
	}
	for i := 0; i < samples; i++ {
		jobs <- i
	}
	close(jobs)
	wg.Wait()
	return out
}

// seedBulkTerminalContracts inserts terminal history cheaply (direct SQL) so
// the reputation aggregate has N rows without N full authorize round-trips.
// User triggers (placement/pricing authority and stats maintenance) are
// disabled for the bulk load via session_replication_role=replica; stats are
// then rebuilt from the same aggregate SQL production drift checks use.
func seedBulkTerminalContracts(
	t *testing.T, ctx context.Context, pool *pgxpool.Pool,
	buyerID uuid.UUID, worker WorkerAuth, profile VLLMRuntimeProfile, n int,
) {
	t.Helper()
	if n < 1 {
		return
	}
	verified := n / 2
	failed := n - verified

	tx, err := pool.Begin(ctx)
	mustf(t, err, "begin bulk seed: %v")
	defer tx.Rollback(ctx)

	// Bypass user triggers for bulk synthetic history only. Production writes
	// always go through normal triggers; this is a measurement fixture.
	// Local-to-transaction so a connection return cannot leave the role set.
	if _, err := tx.Exec(ctx, `SELECT set_config('session_replication_role', 'replica', true)`); err != nil {
		t.Fatalf("session_replication_role=replica: %v", err)
	}

	_, err = tx.Exec(ctx, `
		INSERT INTO execution_contracts (
		  id, request_id, buyer_id, workload_type, route, model_alias,
		  runtime_profile_id, runtime_profile_sha256,
		  input_commitment, request_sha256,
		  maximum_price_usd, estimated_price_usd,
		  buyer_input_usd_per_million_tokens, buyer_output_usd_per_million_tokens,
		  supplier_input_usd_per_million_tokens, supplier_output_usd_per_million_tokens,
		  deadline_at, state, worker_id, supplier_id,
		  upstream_base_url, upstream_token_sealed, finalized_at
		)
		SELECT gen_random_uuid(),
		       'bulk-fail-' || g::text || '-' || gen_random_uuid()::text,
		       $1, 'CHAT_COMPLETION', '/v1/chat/completions', $2,
		       $3, $4,
		       repeat('a', 64), repeat('b', 64),
		       0.001, 0.0005,
		       1.0, 1.0, 0.08, 0.30,
		       now() + interval '1 minute', 'FAILED', $5, $6,
		       'http://127.0.0.1:9/v1', 'enc:probe', now()
		  FROM generate_series(1, $7) g`,
		buyerID, profile.ModelAlias, profile.RuntimeProfileID, profile.ProfileSHA256,
		worker.WorkerID, worker.SupplierID, failed)
	mustf(t, err, "bulk fail seed: %v")

	_, err = tx.Exec(ctx, `
		WITH ins AS (
		  INSERT INTO execution_contracts (
		    id, request_id, buyer_id, workload_type, route, model_alias,
		    runtime_profile_id, runtime_profile_sha256,
		    input_commitment, request_sha256,
		    maximum_price_usd, estimated_price_usd,
		    buyer_input_usd_per_million_tokens, buyer_output_usd_per_million_tokens,
		    supplier_input_usd_per_million_tokens, supplier_output_usd_per_million_tokens,
		    deadline_at, state, worker_id, supplier_id,
		    upstream_base_url, upstream_token_sealed, finalized_at
		  )
		  SELECT gen_random_uuid(),
		         'bulk-ok-' || g::text || '-' || gen_random_uuid()::text,
		         $1, 'CHAT_COMPLETION', '/v1/chat/completions', $2,
		         $3, $4,
		         repeat('c', 64), repeat('d', 64),
		         0.001, 0.0005,
		         1.0, 1.0, 0.08, 0.30,
		         now() + interval '1 minute', 'VERIFIED', $5, $6,
		         'http://127.0.0.1:9/v1', 'enc:probe', now()
		    FROM generate_series(1, $7) g
		  RETURNING id
		), execs AS (
		  INSERT INTO realtime_executions (
		    id, contract_id, worker_id, supplier_id, http_status,
		    stream_event_count, stream_root_sha256, output_commitment,
		    prompt_tokens, completion_tokens, total_tokens,
		    time_to_first_event_ms, duration_ms, verification_state
		  )
		  SELECT gen_random_uuid(), id, $5, $6, 200,
		         1, repeat('1', 64), repeat('2', 64),
		         1, 1, 2, 1, 1, 'PASSED'
		    FROM ins
		  RETURNING id AS execution_id, contract_id
		)
		INSERT INTO realtime_settlements (
		  id, contract_id, authoritative_execution_id, receipt_id,
		  buyer_charge_usd, supplier_gross_usd, platform_margin_usd, verification_cost_usd
		)
		SELECT gen_random_uuid(), contract_id, execution_id, 'rcp_' || gen_random_uuid()::text,
		       0.0001, 0.00005, 0.00005, 0
		  FROM execs`,
		buyerID, profile.ModelAlias, profile.RuntimeProfileID, profile.ProfileSHA256,
		worker.WorkerID, worker.SupplierID, verified)
	mustf(t, err, "bulk verified seed: %v")
	mustf(t, tx.Commit(ctx), "commit bulk seed: %v")

	// Rebuild counters from the same aggregate the drift check uses.
	if _, err := pool.Exec(ctx, `
		DELETE FROM realtime_supplier_outcome_stats
		 WHERE runtime_profile_id=$1 AND supplier_id=$2`,
		profile.RuntimeProfileID, worker.SupplierID); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `
		INSERT INTO realtime_supplier_outcome_stats (
		  runtime_profile_id, supplier_id,
		  terminal_attempts, terminal_fails,
		  verified_settlements, refund_count
		)
		SELECT o.runtime_profile_id, o.supplier_id,
		       o.terminal_attempts, o.terminal_fails,
		       COALESCE(r.verified_settlements,0), COALESCE(r.refund_count,0)
		  FROM (
		    SELECT runtime_profile_id, supplier_id,
		           count(*) FILTER (WHERE state IN ('VERIFIED','FAILED'))::int AS terminal_attempts,
		           count(*) FILTER (WHERE state = 'FAILED')::int AS terminal_fails
		      FROM execution_contracts
		     WHERE runtime_profile_id=$1 AND supplier_id=$2
		     GROUP BY runtime_profile_id, supplier_id
		  ) o
		  LEFT JOIN (
		    SELECT c.runtime_profile_id, c.supplier_id,
		           count(*)::int AS verified_settlements,
		           count(rf.id)::int AS refund_count
		      FROM execution_contracts c
		      JOIN realtime_settlements s ON s.contract_id=c.id
		      LEFT JOIN realtime_refunds rf ON rf.contract_id=c.id
		     WHERE c.runtime_profile_id=$1 AND c.supplier_id=$2 AND c.state='VERIFIED'
		     GROUP BY c.runtime_profile_id, c.supplier_id
		  ) r ON true`,
		profile.RuntimeProfileID, worker.SupplierID); err != nil {
		t.Fatalf("rebuild stats after bulk seed: %v", err)
	}
}

func mercSourceCommit() string {
	if v := strings.TrimSpace(os.Getenv("MERC_SOURCE_COMMIT")); v != "" {
		return v
	}
	// Best-effort from build info VCS revision when tests run in a module tree.
	if bi, ok := debug.ReadBuildInfo(); ok {
		for _, s := range bi.Settings {
			if s.Key == "vcs.revision" && s.Value != "" {
				return s.Value
			}
		}
	}
	return "unknown"
}
