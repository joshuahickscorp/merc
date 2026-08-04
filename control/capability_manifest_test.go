package main

import (
	"strings"
	"testing"
)

// The one value the agent and the control plane must compute identically.
//
// merc-agent refuses any dispatched task whose runtime_matrix_sha256 is not its
// own, so a disagreement between the two implementations grounds the fleet rather
// than producing a wrong answer. agent/src/runtime_authority.rs pins the same
// constant; a capability change is expected to move it, and to move it in BOTH
// places in one commit.
const pinnedCapabilityMatrixDigest = "0b569a272b2bef7f553cad0efbe7bac9fc42a18944b16ac60b590763f497d60c"

func TestCapabilityMatrixDigestIsPinnedAcrossBothImplementations(t *testing.T) {
	if generatedRuntimeMatrixSHA256 != pinnedCapabilityMatrixDigest {
		t.Fatalf("capability matrix digest moved\n  got  %s\n  want %s\n"+
			"If this is a deliberate capability change, update the pin here AND in "+
			"agent/src/runtime_authority.rs, and rebuild the agent.",
			generatedRuntimeMatrixSHA256, pinnedCapabilityMatrixDigest)
	}
}

// The whole point of the split: promotion must not move the dispatch identity.
//
// Every one of these mutations is what a promotion, a demotion, a quarantine or a
// new benchmark receipt does to the document. Under the previous file-bytes digest
// each of them changed the value every agent compares against, so a lifecycle edit
// could not be deployed without rebuilding and restarting the whole fleet first.
func TestActivationPolicyChangesDoNotMoveTheCapabilityMatrixDigest(t *testing.T) {
	for name, mutate := range map[string]func(d *runtimeAuthorityDocument){
		"profile promoted": func(d *runtimeAuthorityDocument) {
			d.Runtimes[2].Lifecycle = runtimeLifecycleCanary
		},
		"profile quarantined": func(d *runtimeAuthorityDocument) {
			d.Runtimes[0].Lifecycle = runtimeLifecycleQuarantined
		},
		"cell promoted": func(d *runtimeAuthorityDocument) {
			d.Runtimes[2].Cells[1].Lifecycle = runtimeLifecycleCanary
		},
		"cell rejected": func(d *runtimeAuthorityDocument) {
			d.Runtimes[2].Cells[1].Lifecycle = runtimeLifecycleRejectedForContract
			d.Runtimes[2].Cells[1].RejectionReason = "measured unsuitable"
		},
		"promotion receipt": func(d *runtimeAuthorityDocument) {
			d.Runtimes[0].BenchmarkAuthority = "evidence/somewhere/else.json"
			d.Runtimes[0].Cells[0].BenchmarkAuthority = "evidence/somewhere/else.json"
		},
		"quality tier": func(d *runtimeAuthorityDocument) {
			d.Runtimes[0].QualityTier = "MODEL_EXACT"
			d.Runtimes[0].Cells[0].QualityTier = "MODEL_EXACT"
		},
		"evidence list": func(d *runtimeAuthorityDocument) {
			d.Runtimes[0].Evidence = append(d.Runtimes[0].Evidence, "evidence/new.json")
		},
		"supersession": func(d *runtimeAuthorityDocument) {
			d.Runtimes[1].SupersededBy = "candle_metal"
		},
		"matrix version label": func(d *runtimeAuthorityDocument) {
			d.MatrixVersion = "2099-01-01.1"
		},
	} {
		t.Run(name, func(t *testing.T) {
			doc := deepCopyAuthority(t)
			mutate(&doc)
			got, err := capabilityMatrixDigest(doc)
			if err != nil {
				t.Fatal(err)
			}
			if got != generatedRuntimeMatrixSHA256 {
				t.Fatalf("an activation-policy change moved the capability matrix digest; "+
					"every agent would have to be rebuilt to deploy it\n  got  %s\n  want %s",
					got, generatedRuntimeMatrixSHA256)
			}
		})
	}
}

// And the other half: a real capability change MUST move it, because an agent
// running the old capability set must not execute work described by the new one.
func TestCapabilityChangesMoveTheCapabilityMatrixDigest(t *testing.T) {
	for name, mutate := range map[string]func(d *runtimeAuthorityDocument){
		"engine adapter": func(d *runtimeAuthorityDocument) {
			d.Engines[0].Adapter = "merc-something-else"
		},
		"profile engine":   func(d *runtimeAuthorityDocument) { d.Runtimes[0].Engine = "mlx" },
		"profile revision": func(d *runtimeAuthorityDocument) { d.Runtimes[0].Revision = "r99" },
		"engine revision": func(d *runtimeAuthorityDocument) {
			d.Runtimes[0].EngineRevision = "b1946ac9"
		},
		"adapter": func(d *runtimeAuthorityDocument) { d.Runtimes[0].Adapter = "merc-mlx" },
		"tokenizer revision": func(d *runtimeAuthorityDocument) {
			d.Runtimes[0].TokenizerRevision = "different"
		},
		"chat template": func(d *runtimeAuthorityDocument) {
			d.Runtimes[0].ChatTemplateID = "different"
		},
		"source identity": func(d *runtimeAuthorityDocument) {
			d.Runtimes[0].SourceIdentity = "elsewhere"
		},
		"device": func(d *runtimeAuthorityDocument) { d.Runtimes[0].Device = "cuda" },
		"platform": func(d *runtimeAuthorityDocument) {
			d.Runtimes[0].Hardware.Platforms = []string{"apple_silicon_base"}
		},
		"device count": func(d *runtimeAuthorityDocument) {
			d.Runtimes[0].Hardware.DeviceCount.Maximum = 8
		},
		"parallelism flag": func(d *runtimeAuthorityDocument) {
			d.Runtimes[0].Parallelism.TensorParallel = true
		},
		// A flag turned OFF, not on. declaredCapabilities lists only what is
		// claimed, so a digest built from it would be blind to this direction.
		"runtime feature removed": func(d *runtimeAuthorityDocument) {
			d.Runtimes[0].Capabilities.PrefixCache = false
		},
		"cell memory floor": func(d *runtimeAuthorityDocument) {
			d.Runtimes[0].Cells[0].MinMemoryGB = 3
		},
		"cell verification contract": func(d *runtimeAuthorityDocument) {
			d.Runtimes[0].Cells[0].Verification = "byte_exact"
		},
		"cell wire kind": func(d *runtimeAuthorityDocument) {
			d.Runtimes[0].Cells[0].WireKind = "gguf"
		},
		"cell max batch": func(d *runtimeAuthorityDocument) { d.Runtimes[0].Cells[0].MaxBatch = 64 },
		"cell max concurrency": func(d *runtimeAuthorityDocument) {
			d.Runtimes[0].Cells[0].MaxConcurrency = 7
		},
		"model artifact digest": func(d *runtimeAuthorityDocument) {
			d.Models[0].Artifacts[0].SHA256 = strings.Repeat("a", 64)
		},
		"model artifact revision": func(d *runtimeAuthorityDocument) {
			d.Models[0].HFRevision = strings.Repeat("b", 40)
		},
		"model memory floor": func(d *runtimeAuthorityDocument) { d.Models[0].MinMemoryGB = 9 },
	} {
		t.Run(name, func(t *testing.T) {
			doc := deepCopyAuthority(t)
			mutate(&doc)
			got, err := capabilityMatrixDigest(doc)
			if err != nil {
				t.Fatal(err)
			}
			if got == generatedRuntimeMatrixSHA256 {
				t.Fatalf("changing the %s did not move the capability matrix digest; "+
					"an agent running the old capability set would execute work "+
					"described by the new one", name)
			}
		})
	}
}

// Two profiles declaring identical cell rows must not produce one cell identity.
// A cell is not portable: "MiniLM from a GGUF at a 2 GB floor" means something
// different on llama.cpp Metal than on vLLM CUDA, and activation policy is keyed
// by cell identity.
func TestCellCapabilityDigestsAreUniquePerProfile(t *testing.T) {
	seen := map[string]string{}
	for _, profile := range runtimeAuthority.Runtimes {
		for _, cell := range profile.Cells {
			digest, err := profile.CellCapabilityDigest(cell, runtimeAuthorityModels)
			if err != nil {
				t.Fatal(err)
			}
			where := profile.RuntimeID + "/" + cell.ID
			if owner, taken := seen[digest]; taken {
				t.Fatalf("%s and %s share one cell capability digest", owner, where)
			}
			seen[digest] = where
		}
	}
	if len(seen) == 0 {
		t.Fatal("no cells produced a capability digest")
	}
}

// The two questions are different, and conflating them is what welded a lifecycle
// promotion to a fleet rebuild.
func TestDocumentDigestAndCapabilityDigestAreDistinct(t *testing.T) {
	if generatedRuntimeAuthorityFileSHA256 == generatedRuntimeMatrixSHA256 {
		t.Fatal("the document byte digest and the capability digest are the same value")
	}
}

// deepCopyAuthority returns a document whose nested slices can be mutated without
// touching the process-wide authority. mutableAuthority re-decodes the top level
// but the cells and artifacts inside it are freshly allocated per call, so this
// exists to make the intent explicit at the call sites above.
func deepCopyAuthority(t *testing.T) runtimeAuthorityDocument {
	t.Helper()
	doc := mutableAuthority(t)
	for i := range doc.Runtimes {
		doc.Runtimes[i].Cells = append([]authorityCell(nil), doc.Runtimes[i].Cells...)
		doc.Runtimes[i].Evidence = append([]string(nil), doc.Runtimes[i].Evidence...)
		doc.Runtimes[i].Hardware.Platforms =
			append([]string(nil), doc.Runtimes[i].Hardware.Platforms...)
	}
	for i := range doc.Models {
		doc.Models[i].Artifacts = append([]authorityArtifact(nil), doc.Models[i].Artifacts...)
	}
	doc.Engines = append([]authorityEngine(nil), doc.Engines...)
	return doc
}
