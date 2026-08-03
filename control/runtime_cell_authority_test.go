package main

import (
	"strings"
	"testing"
)

func cellIn(t *testing.T, doc runtimeAuthorityDocument, runtimeID, cellID string) (int, int) {
	t.Helper()
	profileIdx := runtimeIndex(t, doc, runtimeID)
	for i, cell := range doc.Runtimes[profileIdx].Cells {
		if cell.ID == cellID {
			return profileIdx, i
		}
	}
	t.Fatalf("runtime %q has no cell %q", runtimeID, cellID)
	return -1, -1
}

// The correction this whole design exists for.
//
// llama_cpp_metal's embed cell is measured faster than candle at every batch
// size and clears the governed cosine gate at 0.999998. Its batch_infer cell
// diverges from its own serial output in every batched configuration on Metal
// and cannot satisfy the byte_exact contract it sells. Promoting the profile
// must not promote both.
func TestProvingOneCellDoesNotPromoteAnother(t *testing.T) {
	doc := mutableAuthority(t)
	profile, embed := cellIn(t, doc, "llama_cpp_metal", "llama-cpp-metal-minilm-embed")
	_, infer := cellIn(t, doc, "llama_cpp_metal", "llama-cpp-metal-llama1-infer")

	// Promote the embed cell as far as this tranche allows, and the profile
	// with it — the most generous reading of "the embedding is proven".
	doc.Runtimes[profile].Lifecycle = runtimeLifecycleActive
	doc.Runtimes[profile].Cells[embed].Lifecycle = runtimeLifecycleActive

	if err := validateRuntimeAuthorityDocument(doc); err != nil {
		t.Fatalf("promoting the proven cell was refused: %v", err)
	}

	p := doc.Runtimes[profile]
	if !p.Cells[embed].Routable(p) {
		t.Fatal("the proven embed cell did not become routable")
	}
	if p.Cells[infer].Routable(p) {
		t.Fatal("promoting the embed cell also promoted the rejected generation cell")
	}
	if got := p.Cells[infer].EffectiveLifecycle(p); got != runtimeLifecycleRejectedForContract {
		t.Fatalf("the rejected cell drifted to %s under a promoted profile", got)
	}
}

// A profile cannot inflate a cell, and a cell cannot outrank its profile.
func TestCellLifecycleIsFlooredByItsProfile(t *testing.T) {
	profile := authorityRuntimeProfile{Lifecycle: runtimeLifecycleValidated}
	for _, tc := range []struct {
		cell, want string
	}{
		{"", runtimeLifecycleValidated},                                            // inherits
		{runtimeLifecycleDraft, runtimeLifecycleDraft},                             // below: kept
		{runtimeLifecycleValidated, runtimeLifecycleValidated},                     // equal: kept
		{runtimeLifecycleActive, runtimeLifecycleValidated},                        // above: floored
		{runtimeLifecycleCanary, runtimeLifecycleValidated},                        // above: floored
		{runtimeLifecycleRejectedForContract, runtimeLifecycleRejectedForContract}, // terminal
	} {
		got := authorityCell{Lifecycle: tc.cell}.EffectiveLifecycle(profile)
		if got != tc.want {
			t.Errorf("cell %q under a %s profile resolved to %s, want %s",
				tc.cell, profile.Lifecycle, got, tc.want)
		}
	}
}

// REAL_RUNTIME_PROVEN is evidence, not permission. A cell that became routable
// by reaching it would be promoting itself on its own proof.
func TestRealRuntimeProvenIsReachableOnlyByDirectedRouting(t *testing.T) {
	profile := authorityRuntimeProfile{Lifecycle: runtimeLifecycleActive}
	proven := authorityCell{Lifecycle: runtimeLifecycleRealRuntimeProven}
	if proven.Routable(profile) {
		t.Fatal("REAL_RUNTIME_PROVEN is routable for ordinary buyer work")
	}
	if !proven.ReachableByDirectedRouting(profile) {
		t.Fatal("REAL_RUNTIME_PROVEN cannot be reached even by directed routing, so no " +
			"further evidence could ever be collected on it")
	}
	rejected := authorityCell{Lifecycle: runtimeLifecycleRejectedForContract}
	if rejected.ReachableByDirectedRouting(profile) {
		t.Fatal("a rejected cell is reachable by directed routing")
	}
}

// A cell's benchmark authority must measure that cell's model. A MiniLM
// embedding comparison is not evidence about Llama generation on the same
// engine, and the entire reason cells carry their own authority is to stop one
// standing in for the other.
func TestCellBenchmarkAuthorityCannotBeReusedForAnotherModel(t *testing.T) {
	doc := mutableAuthority(t)
	profile, infer := cellIn(t, doc, "candle_metal", "candle-metal-llama1-infer")
	doc.Runtimes[profile].Cells[infer].BenchmarkAuthority =
		"evidence/perf/runtime-benchmarks/embed-cell-candle-vs-llama-cpp-r2.json"

	err := validateRuntimeAuthorityDocument(doc)
	if err == nil {
		t.Fatal("a generation cell was allowed to cite an embedding benchmark")
	}
	if !strings.Contains(err.Error(), "measures") {
		t.Errorf("refusal said %q", err.Error())
	}
}

// A cell reachable by work must name evidence, and the requirement starts at
// REAL_RUNTIME_PROVEN because directed routing sends real buyer work there.
func TestReachableCellsMustNameEvidence(t *testing.T) {
	doc := mutableAuthority(t)
	profile, embed := cellIn(t, doc, "candle_metal", "candle-metal-minilm-embed")
	doc.Runtimes[profile].Cells[embed].BenchmarkAuthority = ""
	// Clear the profile fallback too, or the cell inherits it and proves nothing.
	doc.Runtimes[profile].BenchmarkAuthority = ""

	if err := validateRuntimeAuthorityDocument(doc); err == nil {
		t.Fatal("an ACTIVE cell with no benchmark authority was accepted")
	}
}

// A rejection must say what it was rejected for, and a non-rejected cell must
// not carry a reason it does not have.
func TestRejectionRequiresAndIsLimitedToAReason(t *testing.T) {
	doc := mutableAuthority(t)
	profile, infer := cellIn(t, doc, "llama_cpp_metal", "llama-cpp-metal-llama1-infer")

	stripped := mutableAuthority(t)
	stripped.Runtimes[profile].Cells[infer].RejectionReason = ""
	if err := validateRuntimeAuthorityDocument(stripped); err == nil {
		t.Fatal("a REJECTED_FOR_CONTRACT cell with no reason was accepted")
	} else if !strings.Contains(err.Error(), "rejection_reason") {
		t.Errorf("refusal said %q", err.Error())
	}

	spurious := mutableAuthority(t)
	p2, embed := cellIn(t, spurious, "llama_cpp_metal", "llama-cpp-metal-minilm-embed")
	spurious.Runtimes[p2].Cells[embed].RejectionReason = "no particular reason"
	if err := validateRuntimeAuthorityDocument(spurious); err == nil {
		t.Fatal("a VALIDATED cell carrying a rejection reason was accepted")
	}

	// And the real one states the measurement, not a shrug.
	reason := doc.Runtimes[profile].Cells[infer].RejectionReason
	for _, want := range []string{"byte_exact", "serial"} {
		if !strings.Contains(reason, want) {
			t.Errorf("the rejection reason does not mention %q: %q", want, reason)
		}
	}
}

// A byte_exact cell may not reach work on a benchmark authority that records
// non-determinism, at any level work can reach it.
func TestByteExactCellCannotBeReachableOnANonDeterministicBenchmark(t *testing.T) {
	doc := mutableAuthority(t)
	profile, infer := cellIn(t, doc, "llama_cpp_metal", "llama-cpp-metal-llama1-infer")
	// Try to rehabilitate the rejected cell to the weakest reachable state.
	doc.Runtimes[profile].Lifecycle = runtimeLifecycleRealRuntimeProven
	doc.Runtimes[profile].Cells[infer].Lifecycle = runtimeLifecycleRealRuntimeProven
	doc.Runtimes[profile].Cells[infer].RejectionReason = ""
	doc.Runtimes[profile].Cells[infer].QualityTier = "OUTCOME_EQUIVALENT"

	err := validateRuntimeAuthorityDocument(doc)
	if err == nil {
		t.Fatal("a byte_exact cell became reachable on a non-byte-deterministic benchmark")
	}
	if !strings.Contains(err.Error(), "byte-deterministic") {
		t.Errorf("refusal said %q", err.Error())
	}
}

// Replacing the bytes a cell resolves must move the profile digest, even though
// no field of the profile changed. Restated at the cell level because a cell is
// now the unit that gets promoted.
func TestArtifactReplacementMovesTheDigestThatGatesTheCell(t *testing.T) {
	doc := mutableAuthority(t)
	models := doc.authorityModels()
	profile := doc.Runtimes[runtimeIndex(t, doc, "llama_cpp_metal")]

	before, err := profile.CapabilityDigest(models)
	if err != nil {
		t.Fatal(err)
	}
	swapped := make(map[string]authorityModel, len(models))
	for id, model := range models {
		if id == "all-minilm-l6-v2" {
			artifacts := append([]authorityArtifact(nil), model.Artifacts...)
			for i := range artifacts {
				if artifacts[i].WireKind == "gguf" {
					artifacts[i].SHA256 = strings.Repeat("e", 64)
				}
			}
			model.Artifacts = artifacts
		}
		swapped[id] = model
	}
	after, err := profile.CapabilityDigest(swapped)
	if err != nil {
		t.Fatal(err)
	}
	if before == after {
		t.Fatal("swapping the GGUF the embed cell loads left the digest unchanged")
	}
}

// The advertised set is exactly the cells whose lifecycle is CANARY/ACTIVE and
// whose authority binds. At this commit that is the candle embed cell alone —
// three other candle cells are lifecycle-routable but stand on unbound authority.
func TestCellLifecyclesDidNotWidenTheAdvertisedSurface(t *testing.T) {
	want := map[string]bool{
		"candle-metal-minilm-embed": true,
	}
	if len(advertisedRuntimeCapabilities()) != len(want) {
		t.Fatalf("advertised %d cells, want %d",
			len(advertisedRuntimeCapabilities()), len(want))
	}
	for _, capability := range advertisedRuntimeCapabilities() {
		if !want[capability.ID] {
			t.Errorf("cell %q from runtime %q reached the advertised projection",
				capability.ID, capability.Runtime)
		}
	}
}
