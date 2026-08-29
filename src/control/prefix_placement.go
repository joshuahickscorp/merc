package main

import (
	"sort"

	"github.com/google/uuid"
)

// Prefix-affinity placement, subordinate to cost rank.
//
// # Relation to existing warmth / residency placement
//
// The claim path already has two warmth signals:
//
//   - warm_for_task: model weights resident on the worker (worker_model_state,
//     60s TTL). Residency measurement feeds this.
//   - warm_prefix_depth: KV prefix nodes believed resident (worker_prefix_state,
//     90s TTL). This file's pure ranker is the unit-testable form of that
//     preference.
//
// Prefix affinity is a *refinement* of warm-model placement, not a competitor
// and not a second placement authority. Both signals sit BELOW cost rank and
// cheaper-ask deferral in the claim ORDER BY (see scheduler.go). A deep prefix
// hit on an expensive class never outranks a cold cheap class that meets the
// contract. Within a cost class, deeper prefix beats shallower; model warmth
// breaks the remaining tie.
//
// RankByCostThenPrefixAffinity exists so the cost-before-prefix rule can be
// proven without a database: if cost rank is removed from the comparator, the
// accompanying test fails.

// PrefixAffinityCandidate is one eligible worker for a single placement
// decision. CostRank must come from hwClassCostRank (or the SQL twin); a
// missing rank must be hwClassCostRankUnknown, never zero-as-cheap.
type PrefixAffinityCandidate struct {
	WorkerID        uuid.UUID
	HWClass         string
	CostRank        int
	AskUSDHr        float64
	WarmPrefixDepth int
	WarmModel       bool
}

// RankByCostThenPrefixAffinity orders candidates for placement.
//
// Order (ascending is "better" / claimed first when the worker pulls):
//
//  1. CostRank ASC  — cheaper hardware class first
//  2. AskUSDHr ASC  — within a class, cheaper supplier ask first
//  3. WarmPrefixDepth DESC — deeper believed KV reuse first
//  4. WarmModel DESC — model-weight residency as the final warmth tiebreak
//  5. WorkerID ASC  — stable final order for tests
//
// Prefix depth never moves a candidate across a cost-rank boundary. That is
// the load-bearing rule this function exists to pin.
func RankByCostThenPrefixAffinity(cands []PrefixAffinityCandidate) []PrefixAffinityCandidate {
	out := make([]PrefixAffinityCandidate, len(cands))
	copy(out, cands)
	sort.SliceStable(out, func(i, j int) bool {
		a, b := out[i], out[j]
		if a.CostRank != b.CostRank {
			return a.CostRank < b.CostRank
		}
		if a.AskUSDHr != b.AskUSDHr {
			return a.AskUSDHr < b.AskUSDHr
		}
		if a.WarmPrefixDepth != b.WarmPrefixDepth {
			return a.WarmPrefixDepth > b.WarmPrefixDepth
		}
		if a.WarmModel != b.WarmModel {
			return a.WarmModel && !b.WarmModel
		}
		return a.WorkerID.String() < b.WorkerID.String()
	})
	return out
}

// PreferPrefixWithinCostClass reports whether the winner differs from a pure
// cost-then-ask ranking solely because of prefix depth (or model warmth). Used
// by measurement to separate "affinity changed the pick" from "cost already
// decided".
func PreferPrefixWithinCostClass(cands []PrefixAffinityCandidate) (winner PrefixAffinityCandidate, affinityMoved bool) {
	if len(cands) == 0 {
		return PrefixAffinityCandidate{}, false
	}
	ranked := RankByCostThenPrefixAffinity(cands)
	winner = ranked[0]

	// Cost-only ranking: zero every warmth signal, re-rank, compare.
	costOnly := make([]PrefixAffinityCandidate, len(cands))
	copy(costOnly, cands)
	for i := range costOnly {
		costOnly[i].WarmPrefixDepth = 0
		costOnly[i].WarmModel = false
	}
	costWinner := RankByCostThenPrefixAffinity(costOnly)[0]
	affinityMoved = costWinner.WorkerID != winner.WorkerID
	return winner, affinityMoved
}
