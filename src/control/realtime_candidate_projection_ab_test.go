package main

import (
	"context"
	"fmt"
	"os"
	"strconv"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"
)

// A/B for the realtime candidate projection, against ONE database.
//
// The first attempt to compare the wide and narrow rankings used two full
// diagnostic runs on two freshly-created isolated databases. The sort spill
// fell 131,896 kB -> 11,168 kB exactly as intended, and the statement got
// SLOWER, 1,979 ms -> 4,844 ms. Both numbers were real and the comparison was
// worthless: the planner's row estimate for the same WHERE clause was 199,681
// in one run and 1 in the other, so one plan hash-joined and the other
// nested-looped 100,000 times. What differed was when ANALYZE ran relative to
// the offer heartbeat, not the query.
//
// A before/after claim needs an identical workload. So both variants run here
// against the same rows, the same statistics and the same session, in both
// orders, and the artifact reports the sort method and spill for each — those
// are the mechanism, and they are what the change was actually aimed at.
//
// This is a measurement, not a gate: it asserts only that both variants are
// still valid SQL that plans over the seeded book. It refuses to assert a
// speed ordering, because a planner is allowed to change its mind.
func TestRealtimeCandidateProjectionABOnOneDatabase(t *testing.T) {
	if os.Getenv("MERC_REALTIME_CANDIDATE_AB") != "1" {
		t.Skip("set MERC_REALTIME_CANDIDATE_AB=1 to run the realtime candidate projection A/B")
	}
	offers := 100_000
	if raw := strings.TrimSpace(os.Getenv("MERC_REALTIME_CANDIDATE_AB_OFFERS")); raw != "" {
		n, err := strconv.Atoi(raw)
		if err != nil || n <= 0 {
			t.Fatalf("MERC_REALTIME_CANDIDATE_AB_OFFERS=%q is not a positive count", raw)
		}
		offers = n
	}
	installSettlementCurrencyForTest(t, "usd")
	t.Setenv("MERC_TOKEN_KEY", "realtime-candidate-ab-key-32bytes-min!!!")

	_, pool := openSelectorScaleStore(t, 8)
	ctx := context.Background()
	profile := sortedVLLMProfiles()[0]

	if err := seedSelectorRealtimeBook(ctx, pool, profile, offers, 20260810); err != nil {
		t.Fatalf("seed %d offers: %v", offers, err)
	}
	if _, err := pool.Exec(ctx, `ANALYZE`); err != nil {
		t.Fatalf("analyze: %v", err)
	}

	// The book must be live for the whole comparison, or one variant measures
	// selection and the other measures an empty scan.
	refresh := func() {
		if _, err := pool.Exec(ctx, `UPDATE realtime_worker_offers SET last_seen_at=now(), status='ACTIVE'`); err != nil {
			t.Fatalf("refresh liveness: %v", err)
		}
	}

	// A comparison over an empty book is the same failure as a filtered test run
	// that matches nothing: it produces timings, and they mean nothing. Prove the
	// eligible set is the size the measurement claims BEFORE measuring it.
	refresh()
	var eligible int
	if err := pool.QueryRow(ctx, `
		SELECT count(*)::int FROM realtime_worker_offers o
		 JOIN suppliers s ON s.id = o.supplier_id
		 WHERE o.runtime_profile_id=$1 AND o.runtime_profile_sha256=$2
		   AND o.status='ACTIVE' AND o.available_sequences > 0
		   AND o.last_seen_at > now()-interval '45 seconds'
		   AND s.status='active' AND s.quarantined_at IS NULL
		   AND o.supplier_input_usd_per_million_tokens <= $3
		   AND o.supplier_output_usd_per_million_tokens <= $4`,
		profile.RuntimeProfileID, profile.ProfileSHA256,
		profile.BuyerInputUSDPerMillionTokens, profile.BuyerOutputUSDPerMillionTokens,
	).Scan(&eligible); err != nil {
		t.Fatalf("count eligible: %v", err)
	}
	if eligible < offers {
		t.Fatalf("seeded %d offers but only %d are eligible for the ranking; "+
			"a ranking measured over %d rows says nothing about a book of %d",
			offers, eligible, eligible, offers)
	}
	t.Logf("eligible candidates under the production predicate: %d", eligible)

	variants := map[string]string{
		"narrow(current)": realtimeAuthorizeCandidatesCTE,
		"wide(previous)":  realtimeAuthorizeCandidatesCTEWidePrevious,
	}
	// Both orders, so a cache-warming advantage cannot be mistaken for a plan.
	order := []string{"narrow(current)", "wide(previous)", "wide(previous)", "narrow(current)"}

	type run struct {
		execMS     float64
		sortMethod string
		rows       int
	}
	results := map[string][]run{}
	for pass, name := range order {
		refresh()
		// The production statement, not a wrapper. An outer ORDER BY ... LIMIT 1
		// invites a top-N heapsort the real claim path never performs, which
		// would make the A/B a comparison of two things neither of which ships.
		body := variants[name] + realtimeAuthorizeABTailSQL
		lines, err := explainCandidateBody(ctx, pool, body, profile)
		if err != nil {
			t.Fatalf("pass %d explain %s: %v", pass, name, err)
		}
		r := run{}
		for _, line := range lines {
			trimmed := strings.TrimSpace(line)
			if strings.HasPrefix(trimmed, "Execution Time:") {
				fmt.Sscanf(trimmed, "Execution Time: %f ms", &r.execMS)
			}
			if strings.HasPrefix(trimmed, "Sort Method:") && r.sortMethod == "" {
				r.sortMethod = trimmed
			}
			if r.rows == 0 && strings.Contains(trimmed, "actual time=") && strings.Contains(trimmed, "rows=") {
				r.rows = -1 // present; exact count is in the plan text
			}
		}
		results[name] = append(results[name], r)
		t.Logf("pass %d %-16s exec=%.1fms  %s", pass, name, r.execMS, r.sortMethod)
	}

	// Whether the host can resolve the comparison at all.
	//
	// Two passes of the SAME sql differing by orders of magnitude means the
	// number being measured is the machine, not the query. Reporting a mean
	// across that is how a contended box gets published as a speedup, so the
	// spread decides whether a timing may be quoted — and it is measured, not
	// assumed from load average.
	const maxWithinVariantSpread = 2.0
	quotable := true
	for _, name := range []string{"narrow(current)", "wide(previous)"} {
		rs := results[name]
		if len(rs) == 0 {
			t.Fatalf("variant %q never ran", name)
		}
		lo, hi, sum := rs[0].execMS, rs[0].execMS, 0.0
		methods := map[string]bool{}
		for _, r := range rs {
			if r.execMS <= 0 {
				t.Fatalf("variant %q produced no Execution Time — the plan was not measured", name)
			}
			lo, hi = min(lo, r.execMS), max(hi, r.execMS)
			sum += r.execMS
			methods[r.sortMethod] = true
		}
		spread := hi / lo
		t.Logf("RESULT %-16s mean=%.1fms min=%.1fms max=%.1fms spread=%.1fx sort_methods=%d",
			name, sum/float64(len(rs)), lo, hi, spread, len(methods))
		if spread > maxWithinVariantSpread {
			quotable = false
			t.Logf("  NOT QUOTABLE: %q varied %.1fx across identical passes", name, spread)
		}
		if len(methods) > 1 {
			quotable = false
			t.Logf("  NOT QUOTABLE: %q chose %d different sort methods for identical SQL", name, len(methods))
		}
	}
	if !quotable {
		t.Logf("VERDICT: no timing comparison may be drawn from this run. The structural " +
			"result still stands on its own — what the ranking sorts, and how many times " +
			"the cost expression is evaluated, are properties of the SQL rather than of " +
			"the host. Re-run on a quiet box to obtain a latency claim.")
		return
	}
	t.Logf("VERDICT: passes were stable; the means above are comparable at offers=%d.", offers)
}

// realtimeAuthorizeABTailSQL is the claim tail from
// realtimeAuthorizeSelectOfferSQLSkip with the mutating CTEs removed, so the
// A/B plans the same candidate ranking and considered-book aggregation the
// claim path plans without decrementing capacity on every EXPLAIN.
const realtimeAuthorizeABTailSQL = realtimeAuthorizeBookAggSQL + `
		SELECT c.worker_id, c.selected_rank, c.candidate_count, b.considered
		  FROM candidates c CROSS JOIN book b
		 WHERE c.selected_rank = 1`

// explainCandidateBody runs EXPLAIN (ANALYZE, BUFFERS) for a candidates-CTE
// body with the same five parameters AuthorizeRealtimeContract binds.
func explainCandidateBody(ctx context.Context, pool *pgxpool.Pool, body string, profile VLLMRuntimeProfile) ([]string, error) {
	rows, err := pool.Query(ctx, `EXPLAIN (ANALYZE, BUFFERS) `+body,
		profile.RuntimeProfileID, profile.ProfileSHA256,
		profile.BuyerInputUSDPerMillionTokens, profile.BuyerOutputUSDPerMillionTokens,
		minRealtimeOutcomeSamples)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var line string
		if err := rows.Scan(&line); err != nil {
			return nil, err
		}
		out = append(out, line)
	}
	return out, rows.Err()
}
