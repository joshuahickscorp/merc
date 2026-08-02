package main

import (
	"fmt"
	"sort"
	"strings"
	"testing"
)

// A mechanism nobody calls is a claim, not a capability.
//
// A caller census over this package at e4fd8993 found seven mechanisms whose only
// non-test appearance was their own declaration, every one of them described in a
// comment or a status document as working:
//
//	RenewInflightLease        a 30s lease with nothing renewing it, so any
//	                          coalesced leader slower than the TTL was taken over
//	                          and its followers fanned out to execute alone
//	sweepExpiredInflight      a DELETE with no ticker, so inflight_executions grew
//	                          without bound
//	EvictPrefixCacheToBudget  value-ranked eviction that never ran
//	DeepestWarmPrefix         a second, dead definition of warm depth beside the
//	                          scheduler's live inline SQL
//	preferenceForTier         the thing MERC_SHAPE_AWARE_ROUTING claims to switch
//	SelectBatch               the token-budget batcher
//	ClassCoalescedDelivery    a billing class registered and never written
//
// Finding those took a manual census. This test is that census, standing. Each
// entry names a mechanism, the production entry point that must reach it, and the
// consequence if it does not — so a regression fails with the reason rather than
// with a missing edge.
//
// Entries here are load-bearing WIRING, not every function in the package. The
// bar for adding one is that its absence is silent: the code compiles, the tests
// pass, and something quietly stops happening.

type reachabilityClaim struct {
	// From is a production entry point: an HTTP handler, the workers loop, or a
	// store transaction the product runs.
	From string
	// Target is the mechanism that must be reachable from it.
	Target string
	// Consequence is what silently stops happening when the edge is gone.
	Consequence string
}

var productionReachability = []reachabilityClaim{
	{
		From:   "Server.handleChatCompletions",
		Target: "Store.RenewInflightLease",
		Consequence: "inflightLeaseTTL is 30 seconds and the realtime contract deadline is two " +
			"minutes, so a coalesced leader slower than the TTL is taken over by the " +
			"next arrival. Its followers are NOT re-collapsed onto the new leader: " +
			"AwaitInflightResult returns no-result on lease expiry and sends each of " +
			"them off to execute alone. The failure mode is a fan-out that costs one " +
			"supplier execution per waiting buyer.",
	},
	{
		From:   "Store.EvaluateCellPromotion",
		Target: "TrafficClassForWorkload",
		Consequence: "the promotion gate would compare verified-outcome cost and nothing " +
			"else, so a cell at half the price and four times slower per unit would clear " +
			"it — including for realtime traffic, where latency is the thing the buyer is " +
			"paying for. The edge is what makes a slower challenger a refusal there and a " +
			"permitted trade for batch work, whose deadline absorbs it.",
	},
	{
		From:   "Server.createJob",
		Target: "ChooseExecutionMode",
		Consequence: "no job records which execution mode it was placed in or why. Batch " +
			"work reaches POOL by construction today, so nothing about placement CHANGES " +
			"when this edge is gone — which is exactly the failure mode: the moment a " +
			"second mode is reachable, 'by construction' and 'by decision' become " +
			"indistinguishable, and only a stored reason tells them apart afterwards.",
	},
	{
		From:   "Workers.Run",
		Target: "Store.sweepExpiredInflight",
		Consequence: "expired inflight_executions rows are never deleted and the table grows " +
			"without bound. Nothing else removes them.",
	},
	{
		From:   "Server.handleChatCompletions",
		Target: "Store.ClaimInflightExecution",
		Consequence: "in-flight coalescing stops happening entirely and every identical " +
			"concurrent request executes on its own supplier.",
	},
	{
		From:   "Workers.Run",
		Target: "Workers.sweepExecutionOverhead",
		Consequence: "execution_overhead_actuals stops being recorded, and overhead cost " +
			"becomes unmeasurable rather than zero.",
	},
	{
		From:   "Workers.Run",
		Target: "Store.EvictPrefixCacheToBudget",
		Consequence: "worker_prefix_state would be bounded only by age, not by its " +
			"advisory per-worker residency budget; stale routing hints could crowd out " +
			"the high-value warm prefixes that the scheduler is meant to prefer.",
	},
	{
		From:   "Server.handleChatCompletions",
		Target: "Store.RecordRealtimeAdmissionEvent",
		Consequence: "capacity refusals leave no execution contract, so without this " +
			"edge the realtime liquidity report would silently omit denied demand and " +
			"misstate capacity fill as a completion-only rate.",
	},
	{
		From:   "Server.handleAdminRealtimeMarketLiquidity",
		Target: "Store.RealtimeMarketLiquidity",
		Consequence: "operators lose the bounded, receipt-shaped observation of live " +
			"offer utilization, current supplier ask depth, capacity refusals, and status " +
			"churn; no deployment decision may replace it with a dashboard guess.",
	},
	{
		From:   "Workers.Run",
		Target: "Store.DeleteOldRealtimeLiquidityTelemetry",
		Consequence: "offer and admission telemetry grows without its 30-day retention " +
			"bound, turning a measured liquidity view into indefinite operational data " +
			"retention.",
	},
	{
		From:   "Server.createJob",
		Target: "Store.RecordShadowSelection",
		Consequence: "shadow runtime selection stops being recorded. Nothing about routing " +
			"changes -- that is the point of it -- so the loss is silent: the only " +
			"evidence about whether a proven-but-directed-only cell would have been " +
			"chosen simply stops accumulating.",
	},
	{
		From:   "Server.createJob",
		Target: "Store.SubmitJobTx",
		Consequence: "job submission stops persisting anything. Included as a control: if " +
			"this claim ever fails, the graph itself is broken and every other " +
			"result in this file is worthless.",
	},
}

func TestProductionEntryPointsReachTheMechanismsTheyClaim(t *testing.T) {
	graph := buildCallGraph(t)

	// The control claim first. A graph that cannot see the most obvious edge in
	// the package would report every other mechanism as dead and be believed.
	control := productionReachability[len(productionReachability)-1]
	if path := graph.reaches(control.From, map[string]bool{control.Target: true}); path == nil {
		t.Fatalf("the call graph cannot see %s -> %s, so it cannot be trusted about "+
			"anything else in this file", control.From, control.Target)
	}

	for _, claim := range productionReachability {
		t.Run(claim.From+"->"+claim.Target, func(t *testing.T) {
			if _, declared := graph.edges[claim.From]; !declared {
				t.Fatalf("%s is not declared in this package; the claim guards a name "+
					"that no longer exists, which guards nothing", claim.From)
			}
			if _, declared := graph.edges[claim.Target]; !declared {
				t.Fatalf("%s is not declared in this package; either it was renamed and "+
					"this claim was not, or it was deleted and the claim is stale",
					claim.Target)
			}
			path := graph.reaches(claim.From, map[string]bool{claim.Target: true})
			if path == nil {
				t.Fatalf("%s does not reach %s.\n\n%s\n\n"+
					"If the mechanism was deliberately removed, delete this claim in the "+
					"same commit and say why. A claim that is quietly deleted to make a "+
					"build pass is how the seven dead mechanisms above survived.",
					claim.From, claim.Target, claim.Consequence)
			}
			t.Logf("%s", strings.Join(path, " -> "))
		})
	}
}

// The other half: mechanisms this package KNOWS are unwired, listed so that the
// list is a decision rather than an oversight.
//
// Every entry is a real symbol with no production caller today. Wiring one is
// welcome — it fails this test, which is the point. The failure says "you wired
// something; move it into productionReachability and name what it now does."
//
// This is deliberately not a lint. A linter would report unused symbols; this
// reports symbols someone described as working.
var knownUnwired = map[string]string{
	"Store.DeepestWarmPrefix": "a second definition of warm prefix depth. The scheduler " +
		"uses its own inline SQL, so two definitions exist and only one is live.",
	"preferenceForTier": "the shape preference MERC_SHAPE_AWARE_ROUTING advertises. " +
		"ClaimTaskSQL passes shapeNoPreference unconditionally, so the flag is inert.",
	"SelectBatch": "the token-budget batcher. Nothing batches by token budget in " +
		"production. The four traffic classes now exist and carry promoted budgets " +
		"(traffic_class.go), so the missing half is a batcher that consumes them.",
	"TokenBudgetFor": "the per-class token budget SelectBatch would consume. Superseded " +
		"in scope by TokenBudgetForTrafficClass, which reads the promoted per-class " +
		"table; both are unwired for the same reason.",
	"TrafficClassForRealtime": "the realtime lane's traffic class. handleChatCompletions " +
		"does not consult it, so INTERACTIVE's 4,096-token budget and 2s queue wait " +
		"bound nothing in production.",
	"buildWorkloadDecisionDirected": "directed routing. Real and reachable only from " +
		"tests; there is no operator entry point, so every second-runtime chain " +
		"proof submits a test-constructed job row rather than going through the API.",
}

func TestKnownUnwiredMechanismsAreStillUnwired(t *testing.T) {
	graph := buildCallGraph(t)
	// Any production entry point will do as a root; what matters is whether the
	// symbol is reached from ANY declaration that is not itself unwired.
	var roots []string
	for key := range graph.edges {
		if _, unwired := knownUnwired[key]; unwired {
			continue
		}
		roots = append(roots, key)
	}
	sort.Strings(roots)

	for target, why := range knownUnwired {
		if _, declared := graph.edges[target]; !declared {
			t.Errorf("%s is no longer declared. If it was deleted, delete this entry "+
				"too — a list of dead mechanisms that names a symbol which does not "+
				"exist is worse than no list.", target)
			continue
		}
		var reachedFrom []string
		for _, root := range roots {
			if path := graph.reaches(root, map[string]bool{target: true}); path != nil {
				reachedFrom = append(reachedFrom, fmt.Sprintf("%s (%s)",
					root, strings.Join(path, " -> ")))
				if len(reachedFrom) >= 3 {
					break
				}
			}
		}
		if len(reachedFrom) > 0 {
			t.Errorf("%s is now reachable from production:\n  %s\n\n"+
				"That is good news, not a failure of the code. Move it into "+
				"productionReachability with the consequence of losing the edge, and "+
				"delete it here. It was listed as: %s",
				target, strings.Join(reachedFrom, "\n  "), why)
		}
	}
}
