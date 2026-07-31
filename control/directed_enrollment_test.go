package main

import (
	"os"
	"strings"
	"testing"
)

// A worker must be able to advertise the cell it is being asked to prove.
//
// Enrolment used to project only the ADVERTISED set, which made the llama.cpp
// embed cell unreachable in a way that could never resolve itself: the cell is
// VALIDATED, so no worker could offer it, so no worker could execute the chain
// that would prove it, so it could never leave VALIDATED. Enrolment now projects
// the DIRECTED set, which is the advertised set plus cells reachable only by
// operator or test routing.
func TestLlamaCppWorkerCanEnrolAgainstItsDirectedCell(t *testing.T) {
	cap := WorkerCapability{
		HWClass:         "apple_silicon_max",
		Engine:          "llama_cpp",
		MemoryGB:        64,
		SupportedJobs:   []string{"embed"},
		SupportedModels: []string{"all-minilm-l6-v2"},
	}
	projected, err := projectWorkerRuntimeCapabilities(cap)
	if err != nil {
		t.Fatalf("a llama.cpp embed worker could not enrol: %v", err)
	}
	if len(projected) != 1 || projected[0].ID != llamaEmbedCell {
		t.Fatalf("projected %+v, want exactly the llama.cpp embed cell", projected)
	}
	// And it is recorded as NOT routable, which is what keeps the widening from
	// widening the product.
	if advertisedRuntimeCell(projected[0].ID) {
		t.Fatal("the llama.cpp embed cell is in the advertised buyer projection")
	}
	if !advertisedRuntimeCell(candleEmbedCell) {
		t.Fatal("the candle embed cell is not in the advertised buyer projection")
	}
}

// The widening must not reach a cell that measurement rejected. A rejected cell
// is not "unproven pending evidence"; it is a decision.
func TestRejectedCellCannotBeEnrolledAgainst(t *testing.T) {
	cap := WorkerCapability{
		HWClass:         "apple_silicon_max",
		Engine:          "llama_cpp",
		MemoryGB:        64,
		SupportedJobs:   []string{"batch_infer"},
		SupportedModels: []string{"llama-3.2-1b-instruct-q4"},
	}
	_, err := projectWorkerRuntimeCapabilities(cap)
	if err == nil {
		t.Fatal("a worker enrolled against the REJECTED_FOR_CONTRACT generation cell")
	}
	// The refusal is now about ACTIVATION rather than about the advertisement.
	// A llama.cpp host really can load a GGUF and generate; what it may not do is
	// sell the byte_exact contract this cell declares, which measurement decided.
	// Enrolment validates the declaration against capability and authorizes
	// against policy, so this is refused at the second step, and saying so is more
	// useful to a supplier than "not advertised".
	if !strings.Contains(err.Error(), "is activated") &&
		!strings.Contains(err.Error(), "not advertised") &&
		!strings.Contains(err.Error(), "no reachable") {
		t.Errorf("refusal said %q", err.Error())
	}
}

// Enrolment still refuses an engine or hardware class the authority does not
// describe at all. Widening to directed cells is not widening to anything.
func TestEnrolmentStillRefusesUnknownEngineAndHardware(t *testing.T) {
	for name, cap := range map[string]WorkerCapability{
		"unknown engine": {
			HWClass: "apple_silicon_max", Engine: "tensorrt", MemoryGB: 64,
			SupportedJobs: []string{"embed"}, SupportedModels: []string{"all-minilm-l6-v2"},
		},
		"unknown hardware": {
			HWClass: "definitely-not-a-class", Engine: "candle", MemoryGB: 64,
			SupportedJobs: []string{"embed"}, SupportedModels: []string{"all-minilm-l6-v2"},
		},
	} {
		if _, err := projectWorkerRuntimeCapabilities(cap); err == nil {
			t.Errorf("%s was accepted", name)
		}
	}
}

// The scheduler's legacy branch is the one filter that matches a capability
// against no named cell, so it is the one place the enrolment widening could have
// leaked ordinary buyer work onto an unproven runtime.
//
// This asserts the guard exists in the query rather than reasoning about it: all
// three legacy branches must require the capability to be routable.
func TestLegacyClaimBranchRequiresARoutableCapability(t *testing.T) {
	// Read the source: the claim SQL is inline in ClaimTasksTx rather than a
	// named constant, and asserting against the text is how the other authority
	// gates in this package check a property of the query itself.
	raw, err := os.ReadFile("scheduler.go")
	if err != nil {
		t.Fatal(err)
	}
	sql := string(raw)
	legacy := strings.Count(sql, "j.workload_decision IS NULL")
	if legacy == 0 {
		t.Skip("the legacy no-decision branch has been removed; this guard is obsolete")
	}
	guarded := strings.Count(sql, "j.workload_decision IS NULL AND wac")
	if guarded != legacy {
		t.Fatalf("%d legacy no-decision branches but only %d require a routable "+
			"capability; a directed-only worker could claim ordinary buyer work",
			legacy, guarded)
	}
}
