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
//	DeepestWarmPrefix         WAS a second dead definition of warm depth; now
//	                          production via observeAndMarkPrefixForCommit so an
//	                          engine-reported cache miss can invalidate belief
//	preferenceForTier         WAS unwired (ClaimTaskSQL forced shapeNoPreference);
//	                          now production via ClaimTaskSQL → shapeOrderSQL
//	SelectBatch               the token-budget batcher — now production via
//	                          ArrivalBatcher.Admit (see productionReachability)
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
		From:   "Server.handleWorkerCommit",
		Target: "Store.DeepestWarmPrefix",
		Consequence: "prefix affinity would keep believing a worker holds a prefix it has " +
			"already evicted. This is the edge that corrects the belief from what the " +
			"engine actually reported: completeTaskTx -> observeAndMarkPrefixForCommit -> " +
			"CorrectPrefixBeliefFromObservation reads the measured depth here. Lose it and " +
			"the index is never contradicted by reality, so placement concentrates load on " +
			"whichever worker was warm first and calls the resulting misses a cache. It was " +
			"listed as unwired for exactly as long as the scheduler's inline SQL was the " +
			"only live definition of warm prefix depth.",
	},
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
		Consequence: "the promotion gate would lose the independent latency contract. " +
			"Unequal supplier liability already refuses as incomplete total-cost evidence, " +
			"but realtime traffic also needs to record that a slower challenger violates the " +
			"thing the buyer is paying for. The edge adds that refusal without fabricating a " +
			"latency-product rule for batch traffic.",
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
		From:   "Workers.Run",
		Target: "Workers.sweepExactResultCache",
		Consequence: "exact_result_cache rows and their content-addressed objects accumulate " +
			"without age or size bound. Nothing else deletes them.",
	},
	{
		From:   "Server.handleChatCompletions",
		Target: "Store.ClaimInflightExecution",
		Consequence: "in-flight coalescing stops happening entirely and every identical " +
			"concurrent request executes on its own supplier.",
	},
	{
		From:   "Server.handleChatCompletions",
		Target: "TrafficClassForAdmittedWork",
		Consequence: "realtime admits would no longer resolve INTERACTIVE from the product " +
			"path, so the class that bounds the arrival-join window and the token budget " +
			"would not be a property of admitted work.",
	},
	{
		From:   "Server.handleChatCompletions",
		Target: "ArrivalBatcher.Admit",
		Consequence: "compatible realtime requests would forward individually with no " +
			"join window, so a continuous-batching engine would only ever see staggered " +
			"arrivals from this control plane.",
	},
	{
		From:   "ArrivalBatcher.Admit",
		Target: "SelectBatch",
		Consequence: "the arrival batcher would not pack by the measured token budget, so " +
			"a lane could grow past the prefill knee and the interactive latency contract " +
			"encoded by EstimatedTTFT would not constrain the set that is forwarded.",
	},
	{
		From:   "ArrivalBatcher.Admit",
		Target: "HoldWouldMissDeadline",
		Consequence: "an interactive request could be held in a join window past its " +
			"deadline; the batcher would no longer refuse a hold that EstimatedTTFT says " +
			"cannot meet the contract.",
	},
	{
		From:   "ArrivalBatcher.Admit",
		Target: "ValidateBatchBudget",
		Consequence: "a class token budget that cannot meet the remaining deadline would " +
			"still enter a join window instead of bypassing, reopening the gap where a " +
			"throughput-sized budget breaches an interactive latency contract.",
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
		From:   "Store.CompleteTaskTx",
		Target: "Store.DeepestWarmPrefix",
		Consequence: "engine-reported cached_prompt_tokens would be ignored and a stale " +
			"warm belief would keep attracting work after the engine had already " +
			"evicted the prefix. Observation correction (CorrectPrefixBeliefFromObservation) " +
			"reads DeepestWarmPrefix to decide confirm vs invalidate; without this edge " +
			"the index is a guess with a data structure around it. Claim ranking still " +
			"uses its own inline SQL for warm_prefix_depth — this edge is the " +
			"observation path, not a second placement authority.",
	},
	{
		From:   "Server.handleChatCompletions",
		Target: "Store.RecordRealtimeAdmissionEvent",
		Consequence: "capacity refusals leave no execution contract, so without this " +
			"edge the realtime liquidity report would silently omit denied demand and " +
			"misstate capacity fill as a completion-only rate.",
	},
	{
		From:   "Store.UpsertRealtimeOffer",
		Target: "ChooseExecutionMode",
		Consequence: "a realtime offer would still bind a host topology but no longer " +
			"freeze whether buyer traffic reaches an independent warm replica, allowing a " +
			"receipt to imply service mode from a mutable routing implementation.",
	},
	{
		From:   "Server.handleAdminRealtimeMarketLiquidity",
		Target: "Store.RealtimeMarketLiquidity",
		Consequence: "operators lose the bounded, receipt-shaped observation of live " +
			"offer utilization, current supplier ask depth, capacity refusals, and status " +
			"churn; no deployment decision may replace it with a dashboard guess.",
	},
	{
		From:   "Store.AuthorizeRealtimeContract",
		Target: "newRealtimePushMarketDecision",
		Consequence: "a realtime buyer order would still reserve one supplier sequence, but its " +
			"contract and receipt would lose the MarketDecision push book, candidate depth, " +
			"selected rank, lock-skip exclusions, and fixed-point offer identity needed to prove " +
			"that a live order book—not a hard-coded worker—cleared it.",
	},
	{
		From:   "Server.handleAdminServiceLeaseMarketLiquidity",
		Target: "Store.ServiceLeaseMarketLiquidity",
		Consequence: "the warm-service offer and admission evidence would be retained but no operator " +
			"could retrieve its bounded fill, utilization, churn, and fixed-point price-depth receipt; " +
			"a dashboard could then substitute a guess for the observed lane.",
	},
	{
		From:   "Server.tryRealtimeExactReuse",
		Target: "realtimeIdentityFromPreparedBody",
		Consequence: "the exact-reuse lane would decode and canonicalise tools/schema independently from " +
			"coalescing and cache population, losing the bounded tenant/profile/policy-scoped identity " +
			"cache and making its measured hit/miss evidence inert while responses still look correct.",
	},
	{
		From:   "Workers.Run",
		Target: "Store.DeleteOldRealtimeLiquidityTelemetry",
		Consequence: "offer and admission telemetry grows without its 30-day retention " +
			"bound, turning a measured liquidity view into indefinite operational data " +
			"retention.",
	},
	{
		From:   "Workers.Run",
		Target: "Store.DeleteOldServiceLeaseLiquidityTelemetry",
		Consequence: "service-lane operational samples would outlive the 30-day evidence window and become " +
			"indefinite supplier capacity and buyer-demand retention.",
	},
	{
		From:   "Server.handleWorkerFabricReceipt",
		Target: "Store.RecordFabricLinkMeasurement",
		Consequence: "candidate local-fabric measurements would be generated by the agent but never " +
			"reach the control-plane evidence boundary, leaving no authenticated record to bind to " +
			"the later worker-identity, site, collective, and economics gates.",
	},
	{
		From:   "Server.handleWorkerFabricCollectiveReceipt",
		Target: "Store.RecordFabricCollectiveMeasurement",
		Consequence: "a real certificate-bound two-rank collective could complete at the agents but " +
			"remain an unauditable local file, so later topology work would have no retained evidence " +
			"while a route response still looked successful.",
	},
	{
		From:   "Server.handleWorkerFabricSessionCreate",
		Target: "Store.CreateFabricProbeSession",
		Consequence: "a peer id supplied to the agent would never be bound to a short-lived, " +
			"same-supplier control-plane session, so a receiving worker could not separately attest " +
			"to the particular direct link the initiator claims to have measured.",
	},
	{
		From:   "Server.handleWorkerFabricObservation",
		Target: "Store.RecordFabricProbeObservation",
		Consequence: "the peer agent's independently authenticated observation would be dropped, " +
			"leaving every receipt self-reported even when two enrolled workers actually completed " +
			"the signed direct-transfer exchange.",
	},
	{
		From:   "Server.handleWorkerFabricObservationStatus",
		Target: "Store.FabricProbeObservationStatus",
		Consequence: "the initiating agent could not wait for the independently authenticated peer " +
			"observations and would race straight to a self-reported receipt, despite both workers " +
			"having completed the signed direct-transfer exchange.",
	},
	{
		From:   "Server.handleWorkerFabricTopologyEvaluate",
		Target: "Store.EvaluateFabricTopology",
		Consequence: "the supplier-facing mesh command could receive a successful HTTP response " +
			"without deriving fresh bidirectional certificate-bound links or preserving the exact " +
			"non-admissible evidence receipt that a later collective gate must examine.",
	},
	{
		From:   "Server.handleWorkerFabricTopologyEvaluate",
		Target: "PlanTopologyFromFabricEvaluation",
		Consequence: "a worker-facing fabric receipt would not be bound to the topology planner, " +
			"so operators could see mesh measurements without the same explicit placement refusal " +
			"that protects shadow routing.",
	},
	{
		From:   "Server.handleWorkerFabricTopologyGet",
		Target: "Store.GetFabricTopologyEvaluation",
		Consequence: "a persisted topology evaluation could be written but not replayed by the " +
			"authenticated requester, forcing operators to trust a transient POST response and " +
			"losing the exact immutable evidence/refusal needed before any future gang gate.",
	},
	{
		From:   "Server.handleCreateServiceLease",
		Target: "Store.CreateServiceLease",
		Consequence: "a buyer-facing service-lease route could validate JSON and return a shape " +
			"without reserving supplier warm capacity, checking prepaid maximum exposure, or freezing " +
			"the currency-bound PricingDecision that every later meter must reproduce.",
	},
	{
		From:   "Server.handleProjectCompile",
		Target: "compileProject",
		Consequence: "an authenticated project upload could return a decorative response while the " +
			"real static detector, bounded probe, digest binding, and unsafe-input refusal remained " +
			"test-only; no quote or execution authority is implied by this proposal route.",
	},
	{
		From:   "Server.handleProjectCompile",
		Target: "Store.RecordProjectCompileReceipt",
		Consequence: "the compiler would return a digest-shaped response but lose the durable " +
			"buyer-scoped proposal/probe evidence that later project quotes need to bind without " +
			"re-inspecting a changed source tree.",
	},
	{
		From:   "Server.handleProjectCompileReceipt",
		Target: "Store.GetProjectCompileReceipt",
		Consequence: "a compile receipt would be written but no production caller could retrieve " +
			"the immutable IR and verify its buyer scope; support and quote tooling would fall back " +
			"to an unscoped database read.",
	},
	{
		From:   "Server.handleProjectCompileRenderUnit",
		Target: "projectRenderWorkUnitAt",
		Consequence: "the render decomposition route would return a shaped response without " +
			"expanding the same deterministic frame/camera/tile/sample ordinal, so a buyer " +
			"could not bind one requested unit to the durable compile receipt before any " +
			"future runtime admission; the route must remain decomposition-only.",
	},
	{
		From:   "Server.handleProjectCompileRenderAssembly",
		Target: "Store.RecordProjectRenderAssemblyReceipt",
		Consequence: "a buyer-supplied render manifest could receive a successful HTTP response without " +
			"durably checking complete ordinal coverage, failed-attempt replacement, or the exact buyer-scoped " +
			"compile receipt; the route must stop at a non-executable evidence receipt.",
	},
	{
		From:   "Store.RecordProjectRenderAssemblyReceipt",
		Target: "validateProjectRenderAssemblyManifest",
		Consequence: "the durable render assembly row would accept missing, duplicate, or worker-failure " +
			"ordinals, allowing a later assembler to mistake a partial manifest for a complete deterministic " +
			"frame/tile/sample result.",
	},
	{
		From:   "Server.handleProjectRenderAssemblyReceipt",
		Target: "Store.GetProjectRenderAssemblyReceipt",
		Consequence: "an assembly receipt would be written but no production buyer caller could replay its " +
			"immutable replacement history and revalidate it against the stored render IR.",
	},
	{
		From:   "Server.handleWorkerLoRAEvaluation",
		Target: "Store.RecordProjectLoRAEvaluation",
		Consequence: "an evaluator worker could receive a successful HTTP response while its score report " +
			"was not bound to a probed buyer IR, independently owned supplier accounts, or an append-only " +
			"receipt; a later outcome decision would then have no production evidence boundary.",
	},
	{
		From:   "Store.RecordProjectLoRAEvaluation",
		Target: "validateProjectLoRAEvaluationEvidence",
		Consequence: "a durable LoRA score row could accept a same-account evaluator, held-out mismatch, " +
			"dataset leakage, non-finite score, or wrong metric-direction improvement, making the outcome " +
			"receipt an incentive-shaped claim rather than an independent measurement.",
	},
	{
		From:   "Server.handleProjectLoRAEvaluationReceipt",
		Target: "Store.GetProjectLoRAEvaluationReceipt",
		Consequence: "an evaluator report could be written but no buyer caller could replay its immutable " +
			"identity, score direction, account separation, or explicit non-executable refusal.",
	},
	{
		From:   "Server.handleAdminNetworkMarketLiquidity",
		Target: "Store.NetworkMarketLiquidity",
		Consequence: "operators would have separate lane reports but no single bounded receipt " +
			"that exposes the realtime and warm-service demand/supply window together; a " +
			"dashboard could then merge unlike windows or mistake one lane for global liquidity.",
	},
	{
		From:   "Server.handleCreateServiceLease",
		Target: "Store.RecordServiceLeaseAdmissionEvent",
		Consequence: "successful and capacity-refused buyer service requests would leave no retained " +
			"demand evidence, making any service fill-rate report a completion-only claim.",
	},
	{
		From:   "Store.UpsertServiceLeaseOffer",
		Target: "recordServiceLeaseOfferSampleTx",
		Consequence: "supplier agent offer refreshes would change current warm capacity without recording " +
			"the prompt-free sample required to measure utilization and availability churn over time.",
	},
	{
		From:   "Server.handleCancelServiceLease",
		Target: "Store.CancelServiceLease",
		Consequence: "a buyer could receive a cancellation response while its warm capacity remained " +
			"reserved, or while the control plane billed the unobserved tail after the worker's final " +
			"authenticated heartbeat. The edge ends the reservation and returns the immutable terminal receipt.",
	},
	{
		From:   "Server.handleServiceLeaseHeartbeat",
		Target: "Store.HeartbeatServiceLease",
		Consequence: "worker health would be accepted at HTTP level but cumulative replica-time, " +
			"SLO bounds, rolling-update facts, and exact payable accrual would never change.",
	},
	{
		From:   "Workers.Run",
		Target: "Store.RecoverServiceLeases",
		Consequence: "a dead service worker would continue to look active and accrue through the " +
			"detector tick rather than stopping at its final authenticated heartbeat.",
	},
	{
		From:   "Workers.Run",
		Target: "Store.FailoverServiceLease",
		Consequence: "a lease in FAILOVER_REQUIRED after worker loss would never acquire a " +
			"replacement under the frozen ceilings. Billing already stopped at the last " +
			"authenticated heartbeat, but the buyer's prepaid reservation would remain " +
			"frozen and the data plane would return 503 until expires_at (up to 7 days).",
	},
	{
		From:   "Workers.Run",
		Target: "Store.TerminateServiceLeaseNoReplacement",
		Consequence: "when no replacement clears the frozen ceiling the lease would sit in " +
			"FAILOVER_REQUIRED until expires_at, holding prepaid capacity against supply " +
			"that will never arrive. The only escape would be the buyer noticing and cancelling.",
	},
	{
		From:   "Workers.Run",
		Target: "Store.FinalizeExpiredServiceLeases",
		Consequence: "expired warm-capacity reservations would retain capacity and remain billable " +
			"instead of producing a bounded terminal service receipt.",
	},
	{
		From:   "ClaimTaskSQL",
		Target: "preferenceForTier",
		Consequence: "MERC_SHAPE_AWARE_ROUTING would be an inert flag: claim ordering would " +
			"ignore measured hardware-shape fitness and always pass shapeNoPreference, so the " +
			"only measured cross-hardware advantage could never influence placement.",
	},
	{
		From:   "Store.ClaimPayout",
		Target: "reserveServiceLeasePayoutFunding",
		Consequence: "a terminal service supplier credit would be visible as held money but could " +
			"never bind to the buyer's collected prepaid top-up: the payout path would either invent a " +
			"job or leave the liability permanently awaiting funding. The edge preserves the exact lease, " +
			"payment intent, currency, and minor-unit allocation before any provider operation.",
	},
	{
		From:   "Store.ClaimPayout",
		Target: "reserveBuyerTopupPayoutFunding",
		Consequence: "realtime execution-contract supplier credits and fully-prepaid job credits would " +
			"remain awaiting_funding forever even though the buyer already settled top-up cash that should " +
			"back those liabilities.",
	},
	{
		From:   "Workers.Run",
		Target: "Store.SettlePendingRealtimeIntents",
		Consequence: "a fully delivered SSE stream whose request-path finalize failed would never settle, " +
			"leaving the buyer with free compute and the supplier unpaid.",
	},
	{
		From:   "Workers.Run",
		Target: "Store.DeleteOldFabricLinkMeasurements",
		Consequence: "self-reported candidate-link observations would be retained indefinitely even " +
			"though no scheduler may use them, expanding operational metadata retention without " +
			"advancing a placement capability.",
	},
	{
		From:   "Workers.Run",
		Target: "Store.DeleteOldFabricCollectiveMeasurements",
		Consequence: "non-admissible synthetic collective evidence would outlive its declared 30-day " +
			"operational retention bound, retaining supplier topology metadata without creating any " +
			"placement capability.",
	},
	{
		From:   "Workers.Run",
		Target: "Store.DeleteOldFabricProbeSessions",
		Consequence: "short-lived peer-session identity bindings and their observations would grow " +
			"without bound after their evidence window, retaining operational topology metadata beyond " +
			"the stated 30-day period.",
	},
	{
		From:   "Workers.Run",
		Target: "Store.DeleteOldFabricTopologyEvaluations",
		Consequence: "derived mesh receipts would persist after the underlying link evidence is " +
			"operationally stale, retaining supplier topology metadata without a declared bound.",
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
		From:   "Server.handleAdminSelectorLiabilityRegret",
		Target: "Store.SelectorLiabilityRegretForScope",
		Consequence: "operators lose the measured outcome regret that distinguishes a " +
			"promotion-ready RuntimeSelector scope from a winner-only or partially " +
			"measured report; without this edge, the selection gate can only be " +
			"inspected through tests or direct database access.",
	},
	{
		From:   "Server.handleAdminSelectorPromotion",
		Target: "Store.EvaluateCellPromotion",
		Consequence: "the narrow promotion gate would remain test-only, leaving operators with no " +
			"receipt-preserving production path to compare a directed challenger against the " +
			"routed incumbent before a separately audited activation-policy write.",
	},
	{
		From:   "Server.handleAdminSelectorPromotion",
		Target: "Store.RecordCellPromotionEvaluation",
		Consequence: "the promotion response would contain a digest but no durable evidence row, " +
			"so a refusal or pass could disappear with the HTTP request and could not be audited " +
			"before any separate activation-policy decision.",
	},
	{
		From:   "Server.handleAdminSelectorRollback",
		Target: "Store.RollbackActivationPolicy",
		Consequence: "a selector promotion could be applied by policy but its production operator " +
			"rollback authority would remain store-test-only, forcing an unsafe manual database edit " +
			"or leaving a bad cell routable after a measured failure.",
	},
	{
		From:   "Server.handleAdminSelectorActivation",
		Target: "Store.ApplyActivationPolicy",
		Consequence: "operators could evaluate a promotion and roll one back, but could not apply a " +
			"reviewed activation-policy write through a production entry point — leaving promotion " +
			"as a store-test-only or manual-database action after the gate receipt exists.",
	},
	{
		From:   "Server.handleAdminDirectedJob",
		Target: "buildWorkloadDecisionDirected",
		Consequence: "directed routing would remain test-only, so a non-routable challenger cell " +
			"could never accumulate primary-task samples through the money path, and " +
			"regret.ScoredDecisions would stay structurally pinned at zero for every challenger.",
	},
	{
		From:   "Server.handleAdminDirectedJob",
		Target: "Server.createJob",
		Consequence: "the directed entry point would freeze a cell without submitting through the " +
			"ordinary money path, so challenger executions would not produce the paid primary " +
			"tasks promotion and regret measurement require.",
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
	// TokenBudgetFor is the two-way LatencyClass helper. Production packing
	// reads budgets through PolicyForTrafficClass / TokenBudgetForTrafficClass
	// via the arrival batcher; this spelling remains for the batch_policy tests
	// and for ValidateBatchBudget's LatencyClass argument path.
	"TokenBudgetFor": "the two-way INTERACTIVE/BATCH token budget. Production packing " +
		"uses PolicyForTrafficClass via ArrivalBatcher; this helper is kept for the " +
		"batch_policy tests and is not itself a production entry point.",
	// buildWorkloadDecisionDirected is production via handleAdminDirectedJob →
	// createJob → buildWorkloadDecisionForSubmit. It is deliberately absent here.
	"TokenAccounting.ReuseRatio": "the guard against reporting reuse as a speed " +
		"multiple, with no production caller. Its own comment says it is 'a cost " +
		"ratio, never a speed multiple' — but nothing computes it on a live " +
		"delivery, so the refusal it encodes is not actually enforced anywhere. " +
		"Until a settlement or metrics path calls it, the separation is held only " +
		"by the split counters in metrics.go (see " +
		"TestZeroWorkDeliveriesDoNotCountAsVerifiedExecutions), not by this.",
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
