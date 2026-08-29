package main

// realtimeAuthorizeCandidatesCTEWidePrevious is the projection this replaced,
// preserved verbatim from a984b8f7~4 so the A/B compares against what actually
// ran rather than a paraphrase of it. Test-only: nothing in production reads it.
const realtimeAuthorizeCandidatesCTEWidePrevious = `
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
			       -- ExpectedSupplierLiabilityUSDPerVerifiedUnit). Unmeasured rates leave the
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
