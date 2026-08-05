package main

import (
	"context"
	"fmt"
	"sort"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// realtimeSupplierOutcomeStats is the four reputation inputs that used to be
// recomputed per authorization by scanning every execution_contracts row for
// the runtime profile. They adjust verified-outcome ranking only; they never
// enter buyer charge or supplier payable arithmetic.
type realtimeSupplierOutcomeStats struct {
	RuntimeProfileID    string
	SupplierID          uuid.UUID
	TerminalAttempts    int
	TerminalFails       int
	VerifiedSettlements int
	RefundCount         int
}

// realtimeAuthorizeSelectOfferSQL is the atomic offer reservation used by
// AuthorizeRealtimeContract. Reputation joins realtime_supplier_outcome_stats
// (PK lookup) instead of aggregating execution_contracts history per request.
//
// Tail note (see evidence/perf/authorize-auth-tails-*.json and
// docs/RUNTIME_AND_PERF.md): under concurrency every contender used to
// target selected_rank=1 and serialise on that one offer row even when other
// equal-or-next-rank offers had free capacity. The claim path now prefers
// FOR UPDATE SKIP LOCKED ordered by rank so multi-offer books admit in
// parallel; when every candidate is locked (the single-offer case) a
// blocking claim waits instead of returning no-supply while capacity sits
// idle. available_sequences > 0 on the UPDATE still makes decrement-and-check
// atomic. Lock hierarchy is unchanged: this runs only after
// evaluateRealtimeBuyerFunding.
//
// The multi-buyer single-offer p95 (~50–75 ms at c=32) is a thin-book /
// fixture characterisation: one (worker, profile) capacity row is one
// Postgres row lock held through commit. Seed and single-agent local paths
// create that shape deliberately; a healthy multi-supplier book does not.
// Do not split available_sequences into slot rows against that number —
// capacity truth stays on the counter + release path.
const realtimeAuthorizeSelectOfferSQL = realtimeAuthorizeSelectOfferSQLBlocking

// realtimeAuthorizeCandidatesCTE is the shared ranking body. $1 profile id,
// $2 profile sha, $3/$4 buyer ceilings, $5 min outcome samples.
const realtimeAuthorizeCandidatesCTE = `
		WITH candidates AS (
			SELECT c.worker_id,c.runtime_profile_id,c.supplier_id,c.upstream_base_url,
			       c.upstream_token_sealed,c.supplier_input_usd_per_million_tokens::float8 AS supplier_input,
			       c.supplier_output_usd_per_million_tokens::float8 AS supplier_output,
			       c.placement_plan,c.placement_plan_sha256,c.warmth,
			       COALESCE(st.terminal_attempts,0)::int AS terminal_attempts,
			       COALESCE(st.terminal_fails,0)::int AS terminal_fails,
			       COALESCE(st.verified_settlements,0)::int AS verified_settlements,
			       COALESCE(st.refund_count,0)::int AS refund_count,
			       -- verified_outcome_cost: base ask, then divide by delivered and
			       -- kept rates when measured (same arithmetic as
			       -- ExpectedVerifiedOutcomeUSDPerUnit). Unmeasured rates leave the
			       -- base ask unchanged rather than inventing a coefficient.
			       (
			         (c.supplier_input_usd_per_million_tokens + c.supplier_output_usd_per_million_tokens)
			         * CASE
			             WHEN COALESCE(st.terminal_attempts,0) >= $5
			              AND COALESCE(st.terminal_fails,0) >= st.terminal_attempts
			             THEN 1e12::numeric
			             WHEN COALESCE(st.terminal_attempts,0) >= $5
			              AND COALESCE(st.terminal_fails,0) < st.terminal_attempts
			             THEN st.terminal_attempts::numeric
			                  / (st.terminal_attempts - st.terminal_fails)::numeric
			             ELSE 1::numeric
			           END
			         * CASE
			             WHEN COALESCE(st.verified_settlements,0) >= $5
			              AND COALESCE(st.refund_count,0) >= st.verified_settlements
			             THEN 1e12::numeric
			             WHEN COALESCE(st.verified_settlements,0) >= $5
			              AND COALESCE(st.refund_count,0) < st.verified_settlements
			             THEN st.verified_settlements::numeric
			                  / (st.verified_settlements - st.refund_count)::numeric
			             ELSE 1::numeric
			           END
			       ) AS verified_outcome_cost,
			       count(*) OVER ()::int AS candidate_count,
			       row_number() OVER (ORDER BY
			         (
			           (c.supplier_input_usd_per_million_tokens + c.supplier_output_usd_per_million_tokens)
			           * CASE
			               WHEN COALESCE(st.terminal_attempts,0) >= $5
			                AND COALESCE(st.terminal_fails,0) >= st.terminal_attempts
			               THEN 1e12::numeric
			               WHEN COALESCE(st.terminal_attempts,0) >= $5
			                AND COALESCE(st.terminal_fails,0) < st.terminal_attempts
			               THEN st.terminal_attempts::numeric
			                    / (st.terminal_attempts - st.terminal_fails)::numeric
			               ELSE 1::numeric
			             END
			           * CASE
			               WHEN COALESCE(st.verified_settlements,0) >= $5
			                AND COALESCE(st.refund_count,0) >= st.verified_settlements
			               THEN 1e12::numeric
			               WHEN COALESCE(st.verified_settlements,0) >= $5
			                AND COALESCE(st.refund_count,0) < st.verified_settlements
			               THEN st.verified_settlements::numeric
			                    / (st.verified_settlements - st.refund_count)::numeric
			               ELSE 1::numeric
			             END
			         ) ASC,
			         CASE c.warmth WHEN 'HOT' THEN 0 WHEN 'WARM' THEN 1 WHEN 'CACHED' THEN 2 ELSE 3 END,
			         c.available_sequences DESC, c.last_seen_at DESC, c.worker_id ASC)::int AS selected_rank
			  FROM realtime_worker_offers c
			  JOIN suppliers s ON s.id = c.supplier_id
			  LEFT JOIN realtime_supplier_outcome_stats st
			    ON st.supplier_id = c.supplier_id
			   AND st.runtime_profile_id = c.runtime_profile_id
			 WHERE c.runtime_profile_id=$1 AND c.runtime_profile_sha256=$2
			   AND c.status='ACTIVE' AND c.available_sequences > 0
			   AND c.last_seen_at > now()-interval '45 seconds'
			   AND s.status='active' AND s.quarantined_at IS NULL
			   AND c.supplier_input_usd_per_million_tokens <= $3
			   AND c.supplier_output_usd_per_million_tokens <= $4
		)`

// realtimeAuthorizeSelectOfferSQLSkip claims the best currently-unlocked offer
// by rank. Contenders step to the next free candidate instead of queueing on
// rank-1 when other offers have capacity.
const realtimeAuthorizeSelectOfferSQLSkip = realtimeAuthorizeCandidatesCTE + `
		, picked AS (
			SELECT c.worker_id,c.runtime_profile_id,c.supplier_id,c.upstream_base_url,
			       c.upstream_token_sealed,c.supplier_input,c.supplier_output,
			       c.placement_plan,c.placement_plan_sha256,c.warmth,
			       c.terminal_attempts,c.terminal_fails,c.verified_settlements,c.refund_count,
			       c.candidate_count,c.selected_rank
			  FROM candidates c
			  JOIN realtime_worker_offers o
			    ON o.worker_id = c.worker_id AND o.runtime_profile_id = c.runtime_profile_id
			 ORDER BY c.selected_rank
			 FOR UPDATE OF o SKIP LOCKED
			 LIMIT 1
		), updated AS (
			UPDATE realtime_worker_offers o
			   SET available_sequences = o.available_sequences - 1, updated_at = now()
			  FROM picked c
			 WHERE o.worker_id = c.worker_id AND o.runtime_profile_id = c.runtime_profile_id
			   AND o.available_sequences > 0
			 RETURNING o.worker_id,o.supplier_id,o.upstream_base_url,o.upstream_token_sealed,
			           o.supplier_input_usd_per_million_tokens::float8,
			           o.supplier_output_usd_per_million_tokens::float8,
			           o.placement_plan,o.placement_plan_sha256,o.warmth,
			           c.candidate_count,c.selected_rank,
			           c.terminal_attempts,c.terminal_fails,
			           c.verified_settlements,c.refund_count
		)
		SELECT worker_id,supplier_id,upstream_base_url,upstream_token_sealed,
		       supplier_input_usd_per_million_tokens::float8,
		       supplier_output_usd_per_million_tokens::float8,
		       placement_plan,placement_plan_sha256,warmth,
		       candidate_count,selected_rank,
		       terminal_attempts,terminal_fails,verified_settlements,refund_count
		  FROM updated`

// realtimeAuthorizeSelectOfferSQLBlocking waits for the best-ranked offer row.
// Used when SKIP LOCKED found nothing: either the only candidate is locked
// (wait for capacity) or the book is empty (still returns no rows).
const realtimeAuthorizeSelectOfferSQLBlocking = realtimeAuthorizeCandidatesCTE + `
		, picked AS (
			SELECT c.worker_id,c.runtime_profile_id,c.supplier_id,c.upstream_base_url,
			       c.upstream_token_sealed,c.supplier_input,c.supplier_output,
			       c.placement_plan,c.placement_plan_sha256,c.warmth,
			       c.terminal_attempts,c.terminal_fails,c.verified_settlements,c.refund_count,
			       c.candidate_count,c.selected_rank
			  FROM candidates c
			  JOIN realtime_worker_offers o
			    ON o.worker_id = c.worker_id AND o.runtime_profile_id = c.runtime_profile_id
			 ORDER BY c.selected_rank
			 FOR UPDATE OF o
			 LIMIT 1
		), updated AS (
			UPDATE realtime_worker_offers o
			   SET available_sequences = o.available_sequences - 1, updated_at = now()
			  FROM picked c
			 WHERE o.worker_id = c.worker_id AND o.runtime_profile_id = c.runtime_profile_id
			   AND o.available_sequences > 0
			 RETURNING o.worker_id,o.supplier_id,o.upstream_base_url,o.upstream_token_sealed,
			           o.supplier_input_usd_per_million_tokens::float8,
			           o.supplier_output_usd_per_million_tokens::float8,
			           o.placement_plan,o.placement_plan_sha256,o.warmth,
			           c.candidate_count,c.selected_rank,
			           c.terminal_attempts,c.terminal_fails,
			           c.verified_settlements,c.refund_count
		)
		SELECT worker_id,supplier_id,upstream_base_url,upstream_token_sealed,
		       supplier_input_usd_per_million_tokens::float8,
		       supplier_output_usd_per_million_tokens::float8,
		       placement_plan,placement_plan_sha256,warmth,
		       candidate_count,selected_rank,
		       terminal_attempts,terminal_fails,verified_settlements,refund_count
		  FROM updated`

// realtimeClearingProbeSelectSQLShared is the candidates ranking body used by
// both the legacy full-aggregate probe and the stats-table probe. $1 profile,
// $2 profile sha, $3/$4 buyer ceilings, $5 min samples. Reputation source is
// injected by the two probe wrappers below.
const realtimeClearingProbeSelectSQLShared = `
			SELECT c.worker_id,c.supplier_id,c.warmth,
			       c.supplier_input_usd_per_million_tokens::float8 AS supplier_input,
			       c.supplier_output_usd_per_million_tokens::float8 AS supplier_output,
			       COALESCE(o.terminal_attempts,0)::int AS terminal_attempts,
			       COALESCE(o.terminal_fails,0)::int AS terminal_fails,
			       COALESCE(rf.verified_settlements,0)::int AS verified_settlements,
			       COALESCE(rf.refund_count,0)::int AS refund_count,
			       (
			         (c.supplier_input_usd_per_million_tokens + c.supplier_output_usd_per_million_tokens)
			         * CASE
			             WHEN COALESCE(o.terminal_attempts,0) >= $5
			              AND COALESCE(o.terminal_fails,0) >= o.terminal_attempts
			             THEN 1e12::numeric
			             WHEN COALESCE(o.terminal_attempts,0) >= $5
			              AND COALESCE(o.terminal_fails,0) < o.terminal_attempts
			             THEN o.terminal_attempts::numeric
			                  / (o.terminal_attempts - o.terminal_fails)::numeric
			             ELSE 1::numeric
			           END
			         * CASE
			             WHEN COALESCE(rf.verified_settlements,0) >= $5
			              AND COALESCE(rf.refund_count,0) >= rf.verified_settlements
			             THEN 1e12::numeric
			             WHEN COALESCE(rf.verified_settlements,0) >= $5
			              AND COALESCE(rf.refund_count,0) < rf.verified_settlements
			             THEN rf.verified_settlements::numeric
			                  / (rf.verified_settlements - rf.refund_count)::numeric
			             ELSE 1::numeric
			           END
			       ) AS verified_outcome_cost,
			       row_number() OVER (ORDER BY
			         (
			           (c.supplier_input_usd_per_million_tokens + c.supplier_output_usd_per_million_tokens)
			           * CASE
			               WHEN COALESCE(o.terminal_attempts,0) >= $5
			                AND COALESCE(o.terminal_fails,0) >= o.terminal_attempts
			               THEN 1e12::numeric
			               WHEN COALESCE(o.terminal_attempts,0) >= $5
			                AND COALESCE(o.terminal_fails,0) < o.terminal_attempts
			               THEN o.terminal_attempts::numeric
			                    / (o.terminal_attempts - o.terminal_fails)::numeric
			               ELSE 1::numeric
			             END
			           * CASE
			               WHEN COALESCE(rf.verified_settlements,0) >= $5
			                AND COALESCE(rf.refund_count,0) >= rf.verified_settlements
			               THEN 1e12::numeric
			               WHEN COALESCE(rf.verified_settlements,0) >= $5
			                AND COALESCE(rf.refund_count,0) < rf.verified_settlements
			               THEN rf.verified_settlements::numeric
			                    / (rf.verified_settlements - rf.refund_count)::numeric
			               ELSE 1::numeric
			             END
			         ) ASC,
			         CASE c.warmth WHEN 'HOT' THEN 0 WHEN 'WARM' THEN 1 WHEN 'CACHED' THEN 2 ELSE 3 END,
			         c.available_sequences DESC, c.last_seen_at DESC, c.worker_id ASC)::int AS selected_rank
			  FROM realtime_worker_offers c
			  JOIN suppliers s ON s.id = c.supplier_id
`

// realtimeClearingProbeLegacySQL is the pre-change ranking path: full aggregate
// over execution_contracts + settlements/refunds. Kept as a CI twin so a
// divergence between counters and truth fails loudly.
const realtimeClearingProbeLegacySQL = `
		WITH supplier_outcomes AS (
			SELECT supplier_id,
			       count(*) FILTER (WHERE state IN ('VERIFIED','FAILED'))::int AS terminal_attempts,
			       count(*) FILTER (WHERE state = 'FAILED')::int AS terminal_fails
			  FROM execution_contracts
			 WHERE runtime_profile_id = $1
			 GROUP BY supplier_id
		), supplier_refunds AS (
			SELECT c.supplier_id,
			       count(*)::int AS verified_settlements,
			       count(r.id)::int AS refund_count
			  FROM execution_contracts c
			  JOIN realtime_settlements s ON s.contract_id = c.id
			  LEFT JOIN realtime_refunds r ON r.contract_id = c.id
			 WHERE c.runtime_profile_id = $1 AND c.state = 'VERIFIED'
			 GROUP BY c.supplier_id
		), candidates AS (` + realtimeClearingProbeSelectSQLShared + `
			  LEFT JOIN supplier_outcomes o ON o.supplier_id = c.supplier_id
			  LEFT JOIN supplier_refunds rf ON rf.supplier_id = c.supplier_id
			 WHERE c.runtime_profile_id=$1 AND c.runtime_profile_sha256=$2
			   AND c.status='ACTIVE' AND c.available_sequences > 0
			   AND c.last_seen_at > now()-interval '45 seconds'
			   AND s.status='active' AND s.quarantined_at IS NULL
			   AND c.supplier_input_usd_per_million_tokens <= $3
			   AND c.supplier_output_usd_per_million_tokens <= $4
		)
		SELECT worker_id, supplier_id, warmth, supplier_input, supplier_output,
		       terminal_attempts, terminal_fails, verified_settlements, refund_count,
		       verified_outcome_cost::text
		  FROM candidates WHERE selected_rank = 1`

// realtimeClearingProbeStatsSQL is the production ranking path (stats table)
// without the capacity decrement, for side-by-side comparison with the legacy
// aggregate probe.
const realtimeClearingProbeStatsSQL = `
		WITH candidates AS (` + realtimeClearingProbeSelectSQLShared + `
			  LEFT JOIN realtime_supplier_outcome_stats o
			    ON o.supplier_id = c.supplier_id
			   AND o.runtime_profile_id = c.runtime_profile_id
			  LEFT JOIN realtime_supplier_outcome_stats rf
			    ON rf.supplier_id = c.supplier_id
			   AND rf.runtime_profile_id = c.runtime_profile_id
			 WHERE c.runtime_profile_id=$1 AND c.runtime_profile_sha256=$2
			   AND c.status='ACTIVE' AND c.available_sequences > 0
			   AND c.last_seen_at > now()-interval '45 seconds'
			   AND s.status='active' AND s.quarantined_at IS NULL
			   AND c.supplier_input_usd_per_million_tokens <= $3
			   AND c.supplier_output_usd_per_million_tokens <= $4
		)
		SELECT worker_id, supplier_id, warmth, supplier_input, supplier_output,
		       terminal_attempts, terminal_fails, verified_settlements, refund_count,
		       verified_outcome_cost::text
		  FROM candidates WHERE selected_rank = 1`

// realtimeClearingProbeWinner is one non-mutating clearing decision for the
// identical-winner / identical-cost comparison.
type realtimeClearingProbeWinner struct {
	WorkerID            uuid.UUID
	SupplierID          uuid.UUID
	Warmth              string
	SupplierInput       float64
	SupplierOutput      float64
	TerminalAttempts    int
	TerminalFails       int
	VerifiedSettlements int
	RefundCount         int
	// VerifiedOutcomeCost is the exact numeric text Postgres produced so the
	// comparison does not re-round through float64.
	VerifiedOutcomeCost string
}

func scanRealtimeClearingProbeWinner(row pgx.Row) (realtimeClearingProbeWinner, error) {
	var w realtimeClearingProbeWinner
	err := row.Scan(
		&w.WorkerID, &w.SupplierID, &w.Warmth,
		&w.SupplierInput, &w.SupplierOutput,
		&w.TerminalAttempts, &w.TerminalFails,
		&w.VerifiedSettlements, &w.RefundCount,
		&w.VerifiedOutcomeCost,
	)
	return w, err
}

// probeRealtimeClearingWinnerLegacy runs the historical full-aggregate ranking.
func probeRealtimeClearingWinnerLegacy(
	ctx context.Context, q interface {
		QueryRow(context.Context, string, ...any) pgx.Row
	},
	profileID, profileSHA string,
	buyerInput, buyerOutput float64,
) (realtimeClearingProbeWinner, error) {
	return scanRealtimeClearingProbeWinner(q.QueryRow(ctx, realtimeClearingProbeLegacySQL,
		profileID, profileSHA, buyerInput, buyerOutput, minRealtimeOutcomeSamples))
}

// probeRealtimeClearingWinnerStats runs the production stats-table ranking.
func probeRealtimeClearingWinnerStats(
	ctx context.Context, q interface {
		QueryRow(context.Context, string, ...any) pgx.Row
	},
	profileID, profileSHA string,
	buyerInput, buyerOutput float64,
) (realtimeClearingProbeWinner, error) {
	return scanRealtimeClearingProbeWinner(q.QueryRow(ctx, realtimeClearingProbeStatsSQL,
		profileID, profileSHA, buyerInput, buyerOutput, minRealtimeOutcomeSamples))
}

// loadRealtimeSupplierOutcomeStatsFromAggregate recomputes the four counters
// with the historical full-table SQL. Used by drift detection — not by authorize.
func loadRealtimeSupplierOutcomeStatsFromAggregate(
	ctx context.Context, pool *pgxpool.Pool, runtimeProfileID string,
) ([]realtimeSupplierOutcomeStats, error) {
	rows, err := pool.Query(ctx, `
		WITH outcomes AS (
			SELECT supplier_id,
			       count(*) FILTER (WHERE state IN ('VERIFIED','FAILED'))::int AS terminal_attempts,
			       count(*) FILTER (WHERE state = 'FAILED')::int AS terminal_fails
			  FROM execution_contracts
			 WHERE runtime_profile_id = $1 AND supplier_id IS NOT NULL
			 GROUP BY supplier_id
		), refunds AS (
			SELECT c.supplier_id,
			       count(*)::int AS verified_settlements,
			       count(r.id)::int AS refund_count
			  FROM execution_contracts c
			  JOIN realtime_settlements s ON s.contract_id = c.id
			  LEFT JOIN realtime_refunds r ON r.contract_id = c.id
			 WHERE c.runtime_profile_id = $1 AND c.state = 'VERIFIED' AND c.supplier_id IS NOT NULL
			 GROUP BY c.supplier_id
		)
		SELECT COALESCE(o.supplier_id, r.supplier_id),
		       COALESCE(o.terminal_attempts, 0),
		       COALESCE(o.terminal_fails, 0),
		       COALESCE(r.verified_settlements, 0),
		       COALESCE(r.refund_count, 0)
		  FROM outcomes o
		  FULL OUTER JOIN refunds r ON r.supplier_id = o.supplier_id
		 ORDER BY 1`, runtimeProfileID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []realtimeSupplierOutcomeStats
	for rows.Next() {
		var s realtimeSupplierOutcomeStats
		s.RuntimeProfileID = runtimeProfileID
		if err := rows.Scan(&s.SupplierID, &s.TerminalAttempts, &s.TerminalFails,
			&s.VerifiedSettlements, &s.RefundCount); err != nil {
			return nil, err
		}
		out = append(out, s)
	}
	return out, rows.Err()
}

// loadRealtimeSupplierOutcomeStatsFromTable reads the incrementally maintained
// counters for one runtime profile.
func loadRealtimeSupplierOutcomeStatsFromTable(
	ctx context.Context, pool *pgxpool.Pool, runtimeProfileID string,
) ([]realtimeSupplierOutcomeStats, error) {
	rows, err := pool.Query(ctx, `
		SELECT runtime_profile_id, supplier_id,
		       terminal_attempts, terminal_fails,
		       verified_settlements, refund_count
		  FROM realtime_supplier_outcome_stats
		 WHERE runtime_profile_id = $1
		 ORDER BY supplier_id`, runtimeProfileID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []realtimeSupplierOutcomeStats
	for rows.Next() {
		var s realtimeSupplierOutcomeStats
		if err := rows.Scan(&s.RuntimeProfileID, &s.SupplierID,
			&s.TerminalAttempts, &s.TerminalFails,
			&s.VerifiedSettlements, &s.RefundCount); err != nil {
			return nil, err
		}
		// Zero rows are equivalent to absence for ranking (LEFT JOIN COALESCE 0).
		// Drift compares meaningful counters; drop pure-zero rows so a spuriously
		// created empty key does not fail a clean aggregate.
		if s.TerminalAttempts == 0 && s.TerminalFails == 0 &&
			s.VerifiedSettlements == 0 && s.RefundCount == 0 {
			continue
		}
		out = append(out, s)
	}
	return out, rows.Err()
}

// diffRealtimeSupplierOutcomeStats reports every (supplier, field) mismatch
// between the fresh aggregate and the incremental table. Empty means equal.
func diffRealtimeSupplierOutcomeStats(aggregate, table []realtimeSupplierOutcomeStats) []string {
	type key = uuid.UUID
	agg := map[key]realtimeSupplierOutcomeStats{}
	tab := map[key]realtimeSupplierOutcomeStats{}
	for _, s := range aggregate {
		agg[s.SupplierID] = s
	}
	for _, s := range table {
		tab[s.SupplierID] = s
	}
	ids := make([]uuid.UUID, 0, len(agg)+len(tab))
	seen := map[key]struct{}{}
	for id := range agg {
		if _, ok := seen[id]; !ok {
			seen[id] = struct{}{}
			ids = append(ids, id)
		}
	}
	for id := range tab {
		if _, ok := seen[id]; !ok {
			seen[id] = struct{}{}
			ids = append(ids, id)
		}
	}
	sort.Slice(ids, func(i, j int) bool { return ids[i].String() < ids[j].String() })

	var diffs []string
	for _, id := range ids {
		a, aOK := agg[id]
		t, tOK := tab[id]
		if !aOK {
			diffs = append(diffs, fmt.Sprintf(
				"supplier %s present only in stats table: attempts=%d fails=%d settlements=%d refunds=%d",
				id, t.TerminalAttempts, t.TerminalFails, t.VerifiedSettlements, t.RefundCount))
			continue
		}
		if !tOK {
			diffs = append(diffs, fmt.Sprintf(
				"supplier %s present only in fresh aggregate: attempts=%d fails=%d settlements=%d refunds=%d",
				id, a.TerminalAttempts, a.TerminalFails, a.VerifiedSettlements, a.RefundCount))
			continue
		}
		if a.TerminalAttempts != t.TerminalAttempts || a.TerminalFails != t.TerminalFails ||
			a.VerifiedSettlements != t.VerifiedSettlements || a.RefundCount != t.RefundCount {
			diffs = append(diffs, fmt.Sprintf(
				"supplier %s: aggregate(attempts=%d fails=%d settlements=%d refunds=%d) != table(attempts=%d fails=%d settlements=%d refunds=%d)",
				id,
				a.TerminalAttempts, a.TerminalFails, a.VerifiedSettlements, a.RefundCount,
				t.TerminalAttempts, t.TerminalFails, t.VerifiedSettlements, t.RefundCount,
			))
		}
	}
	return diffs
}

// assertRealtimeClearingProbesMatch fails when the legacy full-aggregate ranking
// and the stats-table ranking disagree on the winner or the exact verified
// outcome cost. This is the property that keeps clearing decisions unchanged.
func assertRealtimeClearingProbesMatch(legacy, stats realtimeClearingProbeWinner) error {
	if legacy.WorkerID != stats.WorkerID {
		return fmt.Errorf("winner worker diverged: legacy=%s stats=%s", legacy.WorkerID, stats.WorkerID)
	}
	if legacy.SupplierID != stats.SupplierID {
		return fmt.Errorf("winner supplier diverged: legacy=%s stats=%s", legacy.SupplierID, stats.SupplierID)
	}
	if legacy.VerifiedOutcomeCost != stats.VerifiedOutcomeCost {
		return fmt.Errorf("verified_outcome_cost diverged: legacy=%s stats=%s",
			legacy.VerifiedOutcomeCost, stats.VerifiedOutcomeCost)
	}
	if legacy.TerminalAttempts != stats.TerminalAttempts ||
		legacy.TerminalFails != stats.TerminalFails ||
		legacy.VerifiedSettlements != stats.VerifiedSettlements ||
		legacy.RefundCount != stats.RefundCount {
		return fmt.Errorf("reputation inputs diverged: legacy=(%d,%d,%d,%d) stats=(%d,%d,%d,%d)",
			legacy.TerminalAttempts, legacy.TerminalFails, legacy.VerifiedSettlements, legacy.RefundCount,
			stats.TerminalAttempts, stats.TerminalFails, stats.VerifiedSettlements, stats.RefundCount)
	}
	if legacy.SupplierInput != stats.SupplierInput || legacy.SupplierOutput != stats.SupplierOutput {
		return fmt.Errorf("selected rates diverged: legacy=(%g,%g) stats=(%g,%g)",
			legacy.SupplierInput, legacy.SupplierOutput, stats.SupplierInput, stats.SupplierOutput)
	}
	return nil
}
