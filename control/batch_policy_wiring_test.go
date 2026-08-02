package main

import (
	"testing"
)

// A batching policy function that claims to govern live batching and has no
// production caller must fail a test. This is the anti-gaming assertion for
// SelectBatch / EstimatedTTFT / ValidateBatchBudget: the live path authorizes
// one realtime contract at a time and does not pack multi-request batches by
// token budget. Wiring them without a real batcher would invent a capability.
//
// When one of these is wired, TestKnownUnwiredMechanismsAreStillUnwired fails
// first and this test's companion list must move into productionReachability.
func TestBatchingPolicyWithNoProductionCallerFailsATest(t *testing.T) {
	graph := buildCallGraph(t)

	// Production symbols that claim to govern live batching.
	batchingClaims := []string{
		"SelectBatch",
		"EstimatedTTFT",
		"ValidateBatchBudget",
	}
	// Roots that are themselves known-unwired batching helpers must not count
	// as production callers of each other.
	skipRoot := map[string]bool{}
	for name := range knownUnwired {
		skipRoot[name] = true
	}
	for _, target := range batchingClaims {
		skipRoot[target] = true
	}

	var roots []string
	for key := range graph.edges {
		if skipRoot[key] {
			continue
		}
		// Only production declarations (non-test files already filtered by
		// buildCallGraph).
		roots = append(roots, key)
	}

	for _, target := range batchingClaims {
		if _, declared := graph.edges[target]; !declared {
			t.Errorf("%s is no longer declared; delete the batching-policy assertion "+
				"for it or restore the symbol", target)
			continue
		}
		for _, root := range roots {
			if path := graph.reaches(root, map[string]bool{target: true}); path != nil {
				t.Errorf("%s is reachable from production %s via %v.\n\n"+
					"If a real multi-candidate batcher now consumes it, move %s into "+
					"productionReachability and remove it from knownUnwired. Until then "+
					"this failure means the gap can no longer be presented as unwired.",
					target, root, path, target)
			}
		}
		if _, listed := knownUnwired[target]; !listed {
			t.Errorf("%s has no production caller but is missing from knownUnwired; "+
				"the gap would not be asserted", target)
		}
	}
}
