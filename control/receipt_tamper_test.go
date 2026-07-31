package main

import (
	"strings"
	"testing"
)

// The receipt authority must refuse every edit that would change what it says
// ran.
//
// A receipt is only worth having if altering it is detectable. These mutate the
// frozen workload decision — the authority a receipt is assembled from — and
// require ValidateWorkloadDecisionSnapshot to refuse each one. The decision is
// rebuilt from its own binding and compared, so a tamper is caught by
// reconstruction rather than by a checksum someone could recompute.
func TestReceiptAuthorityRefusesEveryTamper(t *testing.T) {
	base, err := buildWorkloadDecisionDirected(
		embedSubmit(), strings.Repeat("9", 64), llamaEmbedCell)
	if err != nil {
		t.Fatal(err)
	}
	if err := ValidateWorkloadDecisionSnapshot(base); err != nil {
		t.Fatalf("the untampered authority does not validate: %v", err)
	}

	for name, mutate := range map[string]func(*WorkloadDecision){
		// The runtime cell: the single most valuable thing to forge, because it
		// is what a two-runtime comparison is comparing.
		"runtime cell": func(d *WorkloadDecision) {
			d.RuntimeCandidates[0].CellID = candleEmbedCell
		},
		"directed authority": func(d *WorkloadDecision) {
			d.DirectedCellID = candleEmbedCell
		},
		"runtime id": func(d *WorkloadDecision) {
			d.RuntimeCandidates[0].RuntimeID = "candle_metal"
		},
		"engine": func(d *WorkloadDecision) {
			d.RuntimeCandidates[0].Engine = "candle"
		},
		// Artifact format decides which bytes were loaded. A receipt that let this
		// move could claim safetensors output came from a GGUF.
		"model kind": func(d *WorkloadDecision) {
			d.RuntimeCandidates[0].ModelKind = "hf"
		},
		"model revision": func(d *WorkloadDecision) {
			d.ModelRevision = strings.Repeat("0", 40)
		},
		"verification strategy": func(d *WorkloadDecision) {
			d.VerificationStrategy = "byte_exact"
		},
		"workload class": func(d *WorkloadDecision) {
			d.WorkloadClass = "batch_generation"
		},
		"minimum memory": func(d *WorkloadDecision) {
			d.MinimumMemoryGB = 0.5
		},
		"quality-relevant binding": func(d *WorkloadDecision) {
			d.Binding.Model.Ref = "llama-3.2-1b-instruct-q4"
		},
	} {
		t.Run(name, func(t *testing.T) {
			tampered := base
			// Deep-copy the slice, or a mutation leaks into every later case and
			// they all "pass" for the wrong reason.
			tampered.RuntimeCandidates = append(
				[]WorkloadRuntimeCandidate(nil), base.RuntimeCandidates...)
			mutate(&tampered)
			if err := ValidateWorkloadDecisionSnapshot(tampered); err == nil {
				t.Fatalf("a receipt with a tampered %s validated", name)
			}
		})
	}
}

// The binding digest must move when the request does, or two different buyer
// requests could share one authority.
func TestReceiptBindingDigestTracksTheRequest(t *testing.T) {
	a, err := buildWorkloadDecisionDirected(
		embedSubmit(), strings.Repeat("1", 64), llamaEmbedCell)
	if err != nil {
		t.Fatal(err)
	}
	b, err := buildWorkloadDecisionDirected(
		embedSubmit(), strings.Repeat("2", 64), llamaEmbedCell)
	if err != nil {
		t.Fatal(err)
	}
	if a.BindingSHA256 == b.BindingSHA256 {
		t.Fatal("two different input commitments produced one binding digest")
	}
	// And the same request on two different cells shares a binding while
	// differing in the runtime authority — that is what makes the two executions
	// comparable and still distinguishable.
	onCandle, err := buildWorkloadDecisionDirected(
		embedSubmit(), strings.Repeat("1", 64), candleEmbedCell)
	if err != nil {
		t.Fatal(err)
	}
	if onCandle.BindingSHA256 != a.BindingSHA256 {
		t.Fatal("directing the same request to another cell changed its binding")
	}
	if onCandle.RuntimeCandidates[0].CellID == a.RuntimeCandidates[0].CellID {
		t.Fatal("two cells produced one runtime authority")
	}
}
