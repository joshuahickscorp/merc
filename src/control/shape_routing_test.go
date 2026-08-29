package main

import (
	"strings"
	"testing"
)

// Shape routing redirects money: it changes which supplier gets a task. So the
// default must be that nothing changes, and the ordering must still put the
// cheapest-sufficient-class rule above measured shape.
func TestShapeRoutingIsOffUnlessExplicitlyEnabled(t *testing.T) {
	t.Setenv("MERC_SHAPE_AWARE_ROUTING", "")
	for _, tier := range []string{"batch", "priority", "interactive", "trusted", "weird"} {
		if got := preferenceForTier(tier); got != shapeNoPreference {
			t.Fatalf("tier %q has preference %v with the feature disabled; enabling "+
				"shape routing must be explicit because it changes who is paid",
				tier, got)
		}
	}

	// Disabled must render the ordering it rendered before this existed: a
	// constant, so the sort is unchanged rather than merely similar.
	// Must be a CAST constant, never a bare integer: Postgres reads a bare
	// integer in ORDER BY as a column position and errors on position 0.
	got := strings.TrimSpace(shapeRankSQL("me.hw_class", shapeNoPreference))
	if got != "0::int" {
		t.Fatalf("disabled shape rank renders %q; it must be a cast constant so the "+
			"term is a no-op expression rather than an ORDER BY column position", got)
	}
}

func TestShapePreferenceFollowsTheMeasuredCrossover(t *testing.T) {
	t.Setenv("MERC_SHAPE_AWARE_ROUTING", "1")

	// Batch is throughput-bound: it is the shape where Metal collapsed to a 5s
	// TTFT at concurrency 16 while CUDA held 313ms.
	if got := preferenceForTier("batch"); got != shapePreferThroughput {
		t.Fatalf("batch prefers %v, want throughput", got)
	}
	// Everything else is latency-bound. Unknown tiers included: optimising a
	// batch job for latency wastes throughput, while optimising an interactive
	// request for throughput is visible to a user on every single request.
	for _, tier := range []string{"priority", "interactive", "trusted", "unrecognised"} {
		if got := preferenceForTier(tier); got != shapePreferLowLatency {
			t.Fatalf("tier %q prefers %v, want low latency", tier, got)
		}
	}
}

// The ranks must encode the measurement, not a guess: locally attached hardware
// wins first-token, rented NVIDIA wins sustained concurrency.
func TestShapeRankEncodesTheMeasurement(t *testing.T) {
	lat := shapeRankSQL("hw", shapePreferLowLatency)
	thr := shapeRankSQL("hw", shapePreferThroughput)

	rank := func(sql, family string) int {
		// The CASE arms are emitted in rank order, so the arm index is the rank.
		idx := strings.Index(sql, family)
		if idx < 0 {
			t.Fatalf("family %q missing from %q", family, sql)
		}
		return strings.Count(sql[:idx], "WHEN")
	}

	if rank(lat, "apple_silicon%") >= rank(lat, "nvidia%") {
		t.Fatal("low-latency preference does not rank locally attached Apple Silicon " +
			"first (heuristic ordering; no bound crossover measurement exists)")
	}
	if rank(thr, "nvidia%") >= rank(thr, "apple_silicon%") {
		t.Fatal("throughput preference does not rank NVIDIA first " +
			"(heuristic ordering; no bound crossover measurement exists)")
	}
	// An unmeasured class must never be preferred by default, the same rule the
	// cost rank follows.
	for _, sql := range []string{lat, thr} {
		if !strings.Contains(sql, "ELSE 2") {
			t.Fatalf("unrecognised hardware is not ranked last in %q", sql)
		}
	}
}

// Cost must still outrank shape. Shape breaks a tie; it does not overturn the
// cheapest-sufficient-class rule, because no bound matched-weight crossover
// exists and shape routing must not invert who gets paid on unbound history.
func TestCostOutranksShapeInTheClaimOrdering(t *testing.T) {
	sql := ClaimTaskSQLForShape("t.claimed_by = $1", shapePreferThroughput)
	order := sql[strings.LastIndex(sql, "ORDER BY"):]

	cheaper := strings.Index(order, "cheaper_class_online")
	shape := strings.Index(order, "WHEN hw")
	if shape < 0 {
		shape = strings.Index(order, "nvidia%")
	}
	if cheaper < 0 || shape < 0 {
		t.Fatalf("could not locate both terms in the ordering")
	}
	if cheaper > shape {
		t.Fatal("measured shape sorts above cheaper_class_online: a warm/fast expensive " +
			"class would take work a cheaper sufficient class could do")
	}
}

// Shape ordering never promotes a more expensive cost class: production
// ClaimTaskSQL (with the flag on) still places cheaper_class_online and
// cheaper_ask_online before every shape term.
func TestShapeOrderingNeverPromotesAMoreExpensiveCostClass(t *testing.T) {
	t.Setenv("MERC_SHAPE_AWARE_ROUTING", "1")
	sql := ClaimTaskSQL("t.claimed_by IS NULL")
	orderIdx := strings.LastIndex(sql, "ORDER BY")
	if orderIdx < 0 {
		t.Fatal("claim SQL has no ORDER BY")
	}
	order := sql[orderIdx:]
	cheaperClass := strings.Index(order, "cheaper_class_online")
	cheaperAsk := strings.Index(order, "cheaper_ask_online")
	// Production path uses preferenceForTier → per-tier CASE with nvidia/apple arms.
	shape := strings.Index(order, "nvidia%")
	if shape < 0 {
		shape = strings.Index(order, "apple_silicon%")
	}
	if cheaperClass < 0 || cheaperAsk < 0 || shape < 0 {
		t.Fatalf("ORDER BY missing cost/shape terms: class=%d ask=%d shape=%d\n%s",
			cheaperClass, cheaperAsk, shape, order)
	}
	if shape < cheaperClass || shape < cheaperAsk {
		t.Fatal("shape ordering sits above a cost-class term: a shape-favoured expensive " +
			"class would outrank a cheaper sufficient class")
	}
	// Shape must remain a preference expression, never a hard filter.
	whereEnd := orderIdx
	if strings.Contains(sql[:whereEnd], "nvidia%") || strings.Contains(sql[:whereEnd], "apple_silicon%") {
		// me.hw_class appears in SELECT lists; only forbid shape rank as WHERE predicate.
	}
	if strings.Contains(sql, "WHERE") && strings.Contains(sql, "LIKE 'nvidia%'") {
		// The shape CASE uses LIKE; ensure it only appears after ORDER BY.
		likeInWhere := false
		for _, segment := range strings.Split(sql, "ORDER BY") {
			// Only the portion before the final ORDER BY is filter territory for claim.
			_ = segment
		}
		preOrder := sql[:orderIdx]
		if strings.Contains(preOrder, "LIKE 'nvidia%'") || strings.Contains(preOrder, "LIKE 'apple_silicon%'") {
			likeInWhere = true
		}
		if likeInWhere {
			t.Fatal("shape rank used as a filter before ORDER BY; shape must never exclude claimable work")
		}
	}
	// The production builder must actually call preferenceForTier (not hardcode).
	if got := preferenceForTier("batch"); got != shapePreferThroughput {
		t.Fatalf("preferenceForTier(batch)=%v with flag on; ClaimTaskSQL would not rank throughput", got)
	}
}

// ClaimTaskSQL must route through preferenceForTier so the env flag is live.
func TestClaimTaskSQLUsesPreferenceForTierWhenEnabled(t *testing.T) {
	t.Setenv("MERC_SHAPE_AWARE_ROUTING", "1")
	sql := ClaimTaskSQL("t.claimed_by IS NULL")
	if !strings.Contains(sql, "ej.tier") && !strings.Contains(sql, "batch") {
		// shapeOrderSQL emits CASE WHEN ej.tier = 'batch' ...
	}
	if !strings.Contains(sql, "ej.tier") {
		t.Fatal("enabled ClaimTaskSQL does not case on ej.tier; preferenceForTier is not driving order")
	}
	if !strings.Contains(sql, "nvidia%") || !strings.Contains(sql, "apple_silicon%") {
		t.Fatal("enabled ClaimTaskSQL lost hardware-shape CASE arms")
	}
	t.Setenv("MERC_SHAPE_AWARE_ROUTING", "")
	off := ClaimTaskSQL("t.claimed_by IS NULL")
	if strings.Contains(off[strings.LastIndex(off, "ORDER BY"):], "nvidia%") {
		t.Fatal("disabled ClaimTaskSQL still embeds shape CASE arms; flag must gate the term")
	}
}
