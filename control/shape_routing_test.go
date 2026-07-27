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
			"first, contradicting the measured 14.4ms vs 181ms TTFT")
	}
	if rank(thr, "nvidia%") >= rank(thr, "apple_silicon%") {
		t.Fatal("throughput preference does not rank NVIDIA first, contradicting the " +
			"measured 1617 vs 211 tok/s at concurrency 16")
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
// cheapest-sufficient-class rule, because one crossover measurement on two
// different models is not grounds for inverting who gets paid.
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
