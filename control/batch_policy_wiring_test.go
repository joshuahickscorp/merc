package main

import (
	"testing"
)

// SelectBatch / EstimatedTTFT / ValidateBatchBudget must stay reachable from
// the arrival batcher. This is the inverse of the old "no production caller"
// gate: once the batcher is wired, losing the edge is a silent regression.
func TestBatchingPolicyIsReachedFromArrivalBatcher(t *testing.T) {
	graph := buildCallGraph(t)
	required := []struct {
		from   string
		target string
	}{
		{"ArrivalBatcher.Admit", "SelectBatch"},
		{"ArrivalBatcher.Admit", "ValidateBatchBudget"},
		{"ArrivalBatcher.Admit", "HoldWouldMissDeadline"},
		{"JoinWindowForArrival", "EstimatedTTFT"},
		{"HoldWouldMissDeadline", "EstimatedTTFT"},
		{"Server.handleChatCompletions", "ArrivalBatcher.Admit"},
		{"Server.handleChatCompletions", "TrafficClassForAdmittedWork"},
	}
	for _, edge := range required {
		if _, ok := graph.edges[edge.from]; !ok {
			t.Errorf("%s is not declared", edge.from)
			continue
		}
		if path := graph.reaches(edge.from, map[string]bool{edge.target: true}); path == nil {
			t.Errorf("%s no longer reaches %s; arrival batching would silently stop using it",
				edge.from, edge.target)
		}
	}
}
