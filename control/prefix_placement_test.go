package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/google/uuid"
)

// Prefix affinity must never promote a more expensive cost class. This is the
// pure form of the scheduler rule: warm_prefix_depth sits after
// cheaper_class_online in ORDER BY. If CostRank is removed from the comparator
// the warm-expensive candidate wins and this test fails.

func TestPrefixAffinityNeverPromotesMoreExpensiveCostClass(t *testing.T) {
	cheapCold := PrefixAffinityCandidate{
		WorkerID: uuid.MustParse("00000000-0000-0000-0000-000000000001"),
		HWClass:  "apple_silicon_base",
		CostRank: hwClassCostRank("apple_silicon_base"),
		AskUSDHr: 0.50,
		// No prefix, no model warmth.
	}
	expensiveWarm := PrefixAffinityCandidate{
		WorkerID:        uuid.MustParse("00000000-0000-0000-0000-000000000002"),
		HWClass:         "nvidia_80gb",
		CostRank:        hwClassCostRank("nvidia_80gb"),
		AskUSDHr:        0.50, // same ask; class cost is the differentiator
		WarmPrefixDepth: 2048, // deepest possible belief
		WarmModel:       true,
	}
	if cheapCold.CostRank >= expensiveWarm.CostRank {
		t.Fatalf("fixture inverted: cheap rank %d, expensive rank %d",
			cheapCold.CostRank, expensiveWarm.CostRank)
	}

	ranked := RankByCostThenPrefixAffinity([]PrefixAffinityCandidate{expensiveWarm, cheapCold})
	if ranked[0].WorkerID != cheapCold.WorkerID {
		t.Fatalf("warm expensive class won placement over cold cheap class: got %s (rank %d depth %d), want cheap %s (rank %d)",
			ranked[0].WorkerID, ranked[0].CostRank, ranked[0].WarmPrefixDepth,
			cheapCold.WorkerID, cheapCold.CostRank)
	}
}

// If the cost-rank term is stripped from RankByCostThenPrefixAffinity, the
// warm-expensive candidate would sort first. This test re-implements a broken
// comparator (prefix first) and asserts it disagrees with the real ranker —
// so a future edit that accidentally inverts the rule cannot hide behind a
// still-green "warm expensive loses" assertion written only against the
// production function (which would have been rewritten together with the bug).
func TestPrefixAffinityCostRankGateIsLoadBearing(t *testing.T) {
	cheapCold := PrefixAffinityCandidate{
		WorkerID: uuid.MustParse("00000000-0000-0000-0000-000000000011"),
		HWClass:  "apple_silicon_base",
		CostRank: hwClassCostRank("apple_silicon_base"),
	}
	expensiveWarm := PrefixAffinityCandidate{
		WorkerID:        uuid.MustParse("00000000-0000-0000-0000-000000000012"),
		HWClass:         "nvidia_24gb",
		CostRank:        hwClassCostRank("nvidia_24gb"),
		WarmPrefixDepth: 512,
	}

	// Broken comparator: prefix depth first, cost second. This is what we
	// refuse to ship.
	broken := []PrefixAffinityCandidate{cheapCold, expensiveWarm}
	if broken[0].WarmPrefixDepth < broken[1].WarmPrefixDepth {
		broken[0], broken[1] = broken[1], broken[0]
	}
	if broken[0].WorkerID != expensiveWarm.WorkerID {
		t.Fatal("broken fixture did not put warm expensive first")
	}

	correct := RankByCostThenPrefixAffinity([]PrefixAffinityCandidate{cheapCold, expensiveWarm})
	if correct[0].WorkerID != cheapCold.WorkerID {
		t.Fatal("production ranker promoted expensive class")
	}
	if correct[0].WorkerID == broken[0].WorkerID {
		t.Fatal("production ranker agrees with prefix-first ordering; cost gate is not load-bearing")
	}

	// Source-level: RankByCostThenPrefixAffinity must compare CostRank before
	// WarmPrefixDepth. A rewrite that drops the CostRank arm fails here even
	// if the pure behavioural test above is edited in the same change.
	src, err := os.ReadFile("prefix_placement.go")
	must(t, err)
	body := string(src)
	fnStart := strings.Index(body, "func RankByCostThenPrefixAffinity")
	if fnStart < 0 {
		t.Fatal("RankByCostThenPrefixAffinity not found")
	}
	fn := body[fnStart:]
	if end := strings.Index(fn[1:], "\nfunc "); end > 0 {
		fn = fn[:end+1]
	}
	costPos := strings.Index(fn, "CostRank")
	prefixPos := strings.Index(fn, "WarmPrefixDepth")
	if costPos < 0 || prefixPos < 0 {
		t.Fatalf("ranker missing CostRank (%d) or WarmPrefixDepth (%d)", costPos, prefixPos)
	}
	if prefixPos < costPos {
		t.Fatal("WarmPrefixDepth compared before CostRank in RankByCostThenPrefixAffinity")
	}

	// Claim SQL must keep the same order: cheaper_class_online before warm_prefix_depth.
	sql := ClaimTaskSQL("t.claimed_by IS NULL")
	orderIdx := strings.Index(sql, "ORDER BY")
	if orderIdx < 0 {
		t.Fatal("claim SQL has no ORDER BY")
	}
	order := sql[orderIdx:]
	cheapPos := strings.Index(order, "cheaper_class_online")
	warmPos := strings.Index(order, "warm_prefix_depth")
	if cheapPos < 0 || warmPos < 0 || warmPos < cheapPos {
		t.Fatalf("claim ORDER BY lost cost-before-prefix: cheaper=%d warm=%d", cheapPos, warmPos)
	}
	_ = filepath.Separator
}

func TestPrefixAffinityBreaksTiesWithinSameCostClass(t *testing.T) {
	rank := hwClassCostRank("apple_silicon_max")
	cold := PrefixAffinityCandidate{
		WorkerID: uuid.MustParse("00000000-0000-0000-0000-000000000021"),
		HWClass:  "apple_silicon_max",
		CostRank: rank,
		AskUSDHr: 1.0,
	}
	shallow := PrefixAffinityCandidate{
		WorkerID:        uuid.MustParse("00000000-0000-0000-0000-000000000022"),
		HWClass:         "apple_silicon_max",
		CostRank:        rank,
		AskUSDHr:        1.0,
		WarmPrefixDepth: 32,
	}
	deep := PrefixAffinityCandidate{
		WorkerID:        uuid.MustParse("00000000-0000-0000-0000-000000000023"),
		HWClass:         "apple_silicon_max",
		CostRank:        rank,
		AskUSDHr:        1.0,
		WarmPrefixDepth: 256,
	}
	ranked := RankByCostThenPrefixAffinity([]PrefixAffinityCandidate{cold, shallow, deep})
	if ranked[0].WorkerID != deep.WorkerID {
		t.Fatalf("within a cost class, deepest warm must win: got depth %d", ranked[0].WarmPrefixDepth)
	}
	if ranked[1].WorkerID != shallow.WorkerID {
		t.Fatalf("shallow should rank above cold: got %v", ranked[1])
	}

	winner, moved := PreferPrefixWithinCostClass([]PrefixAffinityCandidate{cold, deep})
	if !moved || winner.WorkerID != deep.WorkerID {
		t.Fatalf("affinity should move the pick within a class: moved=%v winner=%s", moved, winner.WorkerID)
	}
	// Across classes, affinity must not move the pick.
	expensiveDeep := PrefixAffinityCandidate{
		WorkerID:        uuid.MustParse("00000000-0000-0000-0000-000000000024"),
		HWClass:         "nvidia_48gb",
		CostRank:        hwClassCostRank("nvidia_48gb"),
		WarmPrefixDepth: 2048,
	}
	_, movedAcross := PreferPrefixWithinCostClass([]PrefixAffinityCandidate{cold, expensiveDeep})
	if movedAcross {
		t.Fatal("affinity must not move placement across a cost-class boundary")
	}
}

// Within a cost class, a colder cheaper ask must beat a warmer dearer ask.
// Affinity is only a tie-break at equal CostRank AND equal AskUSDHr — any
// positive ask gap (here $0.01/hr) is the measured gap at which cost still
// dominates infinite WarmPrefixDepth.
func TestPrefixAffinityNeverPromotesHigherAskWithinClass(t *testing.T) {
	rank := hwClassCostRank("apple_silicon_max")
	cheapCold := PrefixAffinityCandidate{
		WorkerID: uuid.MustParse("00000000-0000-0000-0000-000000000041"),
		HWClass:  "apple_silicon_max",
		CostRank: rank,
		AskUSDHr: 1.00,
	}
	dearWarm := PrefixAffinityCandidate{
		WorkerID:        uuid.MustParse("00000000-0000-0000-0000-000000000042"),
		HWClass:         "apple_silicon_max",
		CostRank:        rank,
		AskUSDHr:        1.01, // $0.01/hr dearer
		WarmPrefixDepth: 1 << 20,
		WarmModel:       true,
	}
	ranked := RankByCostThenPrefixAffinity([]PrefixAffinityCandidate{dearWarm, cheapCold})
	if ranked[0].WorkerID != cheapCold.WorkerID {
		t.Fatalf("warm dearer ask won over cold cheaper ask within class: got ask=%.2f depth=%d, want ask=%.2f",
			ranked[0].AskUSDHr, ranked[0].WarmPrefixDepth, cheapCold.AskUSDHr)
	}
	// Claim SQL must keep cheaper_ask_online before warm_prefix_depth.
	sql := ClaimTaskSQL("t.claimed_by IS NULL")
	order := sql[strings.Index(sql, "ORDER BY"):]
	askPos := strings.Index(order, "cheaper_ask_online")
	warmPos := strings.Index(order, "warm_prefix_depth")
	if askPos < 0 || warmPos < 0 || warmPos < askPos {
		t.Fatalf("claim ORDER BY lost ask-before-prefix: ask=%d warm=%d", askPos, warmPos)
	}
}

// Prefix affinity is a refinement of warm-model placement: same cost tier,
// deeper prefix outranks model-only warmth; cost still outranks both.
func TestPrefixAffinityIsRefinementOfWarmModelNotCompetitor(t *testing.T) {
	rank := hwClassCostRank("apple_silicon_pro")
	modelOnly := PrefixAffinityCandidate{
		WorkerID:  uuid.MustParse("00000000-0000-0000-0000-000000000031"),
		HWClass:   "apple_silicon_pro",
		CostRank:  rank,
		WarmModel: true,
	}
	prefixOnly := PrefixAffinityCandidate{
		WorkerID:        uuid.MustParse("00000000-0000-0000-0000-000000000032"),
		HWClass:         "apple_silicon_pro",
		CostRank:        rank,
		WarmPrefixDepth: 128,
	}
	ranked := RankByCostThenPrefixAffinity([]PrefixAffinityCandidate{modelOnly, prefixOnly})
	if ranked[0].WorkerID != prefixOnly.WorkerID {
		t.Fatal("within a cost class, prefix depth must outrank model-only warmth")
	}

	// Claim SQL: warm_prefix_depth DESC appears before warm_for_task DESC.
	sql := ClaimTaskSQL("t.claimed_by IS NULL")
	order := sql[strings.Index(sql, "ORDER BY"):]
	p := strings.Index(order, "warm_prefix_depth DESC")
	m := strings.Index(order, "warm_for_task DESC")
	if p < 0 || m < 0 || m < p {
		t.Fatalf("claim ORDER BY must rank prefix depth before model warmth: p=%d m=%d", p, m)
	}
}
