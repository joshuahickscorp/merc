package main

import (
	"strings"
	"testing"
)

// candleEmbedCell and llamaEmbedCell moved to runtime_governed_comparison.go:
// the comparison builder names them, and a non-test file cannot reference a
// constant declared only under a test build.
const llamaInferCell = "llama-cpp-metal-llama1-infer"

func embedSubmit() jobSubmit {
	return jobSubmit{
		JobType:     JobType{Type: "embed"},
		Model:       ModelRef{Kind: "hf", Ref: "all-minilm-l6-v2"},
		Tier:        "batch",
		Constraints: JobConstraints{MaxDurationSecs: 3600},
	}
}

// Ordinary admission resolves the advertised cell and records no directed
// choice. The directed field must stay empty on the path a buyer can reach, or
// every job would carry a claim about routing authority that nobody made.
func TestOrdinaryAdmissionRecordsNoDirectedCell(t *testing.T) {
	decision, err := buildWorkloadDecision(embedSubmit(), strings.Repeat("a", 64))
	if err != nil {
		t.Fatalf("build decision: %v", err)
	}
	if decision.DirectedCellID != "" {
		t.Fatalf("ordinary admission recorded directed cell %q", decision.DirectedCellID)
	}
	if len(decision.RuntimeCandidates) != 1 ||
		decision.RuntimeCandidates[0].CellID != candleEmbedCell {
		t.Fatalf("ordinary admission froze %+v, want the advertised candle cell",
			decision.RuntimeCandidates)
	}
}

// The mechanism the second-runtime chain needs: one governed workload, forced
// onto either cell, with the choice frozen into the decision rather than applied
// and forgotten.
func TestDirectedRoutingReachesEitherEmbedCell(t *testing.T) {
	for _, cellID := range []string{candleEmbedCell, llamaEmbedCell} {
		decision, err := buildWorkloadDecisionDirected(
			embedSubmit(), strings.Repeat("b", 64), cellID)
		if err != nil {
			t.Fatalf("directed decision for %s: %v", cellID, err)
		}
		if decision.DirectedCellID != cellID {
			t.Errorf("decision recorded directed cell %q, want %q",
				decision.DirectedCellID, cellID)
		}
		if len(decision.RuntimeCandidates) != 1 ||
			decision.RuntimeCandidates[0].CellID != cellID {
			t.Errorf("directed decision froze %+v, want cell %s",
				decision.RuntimeCandidates, cellID)
		}
		// The same governed workload either way: same binding digest, same
		// model revision, same verification strategy. If these differed, the two
		// executions would not be comparable and the whole comparison would be
		// measuring the admission path rather than the runtimes.
		ordinary, err := buildWorkloadDecision(embedSubmit(), strings.Repeat("b", 64))
		if err != nil {
			t.Fatal(err)
		}
		if decision.BindingSHA256 != ordinary.BindingSHA256 {
			t.Errorf("%s changed the binding digest; the workloads are not comparable", cellID)
		}
		if decision.ModelRevision != ordinary.ModelRevision {
			t.Errorf("%s changed the model revision", cellID)
		}
		if decision.VerificationStrategy != ordinary.VerificationStrategy {
			t.Errorf("%s changed the verification strategy from %q to %q",
				cellID, ordinary.VerificationStrategy, decision.VerificationStrategy)
		}
	}
}

// Directed routing is a choice of runtime, never a licence to run a model on a
// cell that does not serve it.
func TestDirectedRoutingRefusesACellThatDoesNotServeTheWorkload(t *testing.T) {
	// The llama.cpp GENERATION cell, asked to serve an embed workload.
	_, err := buildWorkloadDecisionDirected(
		embedSubmit(), strings.Repeat("c", 64), llamaInferCell)
	if err == nil {
		t.Fatal("an embed workload was directed onto a batch_infer cell")
	}
	if !strings.Contains(err.Error(), "does not serve") {
		t.Errorf("refusal said %q", err.Error())
	}
}

// A rejected cell is not reachable by any route, directed or otherwise. This is
// the property that makes REJECTED_FOR_CONTRACT a decision rather than a label:
// the operator escape hatch does not reopen it.
func TestDirectedRoutingCannotReachARejectedCell(t *testing.T) {
	for _, capability := range directedRuntimeCapabilities() {
		if capability.ID == llamaInferCell {
			t.Fatal("the rejected byte_exact cell is reachable by directed routing")
		}
	}
	// And asking for it by name fails rather than silently falling back to the
	// advertised cell, which would execute somewhere the caller did not choose.
	infer := jobSubmit{
		JobType:     JobType{Type: "batch_infer", MaxTokens: 16},
		Model:       ModelRef{Kind: "gguf", Ref: "llama-3.2-1b-instruct-q4"},
		Tier:        "batch",
		Constraints: JobConstraints{MaxDurationSecs: 3600},
	}
	if _, err := buildWorkloadDecisionDirected(
		infer, strings.Repeat("d", 64), llamaInferCell); err == nil {
		t.Fatal("a rejected cell was reachable by naming it")
	}
}

// The directed set is a superset of the advertised set, and the advertised set
// is unchanged by its existence. Widening what an operator can name must not
// widen what a buyer can be sold.
func TestDirectedSetIsASupersetThatDoesNotWidenTheCatalogue(t *testing.T) {
	directed := map[string]bool{}
	for _, capability := range directedRuntimeCapabilities() {
		directed[capability.ID] = true
	}
	for _, capability := range advertisedRuntimeCapabilities() {
		if !directed[capability.ID] {
			t.Errorf("advertised cell %q is not reachable by directed routing", capability.ID)
		}
	}
	if len(advertisedRuntimeCapabilities()) != 2 {
		t.Fatalf("the advertised catalogue has %d cells, want the 2 bindable candle cells (embed + batch_infer)",
			len(advertisedRuntimeCapabilities()))
	}
	// The llama.cpp embed cell is nameable and NOT sellable, which is exactly the
	// state a cell has to be in to be driven through the chain that proves it.
	if !directed[llamaEmbedCell] {
		t.Fatal("the llama.cpp embed cell is not reachable by directed routing, " +
			"so it can never be driven through the chain that would prove it")
	}
	for _, capability := range advertisedRuntimeCapabilities() {
		if capability.ID == llamaEmbedCell {
			t.Fatal("the llama.cpp embed cell reached the advertised catalogue " +
				"before its chain proof")
		}
	}
}

// A stored decision must reconstruct as itself. Validation rebuilds the decision
// from the binding, and a directed decision rebuilt WITHOUT its directed cell
// would resolve to the advertised cell and be rejected as tampered — turning
// every directed job into a validation failure at read time.
func TestStoredDirectedDecisionValidatesAsItself(t *testing.T) {
	decision, err := buildWorkloadDecisionDirected(
		embedSubmit(), strings.Repeat("e", 64), llamaEmbedCell)
	if err != nil {
		t.Fatal(err)
	}
	if err := ValidateWorkloadDecisionSnapshot(decision); err != nil {
		t.Fatalf("a directed decision did not validate as itself: %v", err)
	}

	// And the directed cell is covered by the tamper check: swapping it must be
	// caught, or an attacker could redirect a frozen job to another runtime.
	tampered := decision
	tampered.DirectedCellID = candleEmbedCell
	if err := ValidateWorkloadDecisionSnapshot(tampered); err == nil {
		t.Fatal("swapping the directed cell on a frozen decision was accepted")
	}
}
