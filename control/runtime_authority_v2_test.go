package main

import (
	"encoding/json"
	"strings"
	"testing"
)

// mutableAuthority returns a deep copy of the embedded document so a test can
// corrupt one field without the mutation leaking into the process-wide
// authority every other test reads.
func mutableAuthority(t *testing.T) runtimeAuthorityDocument {
	t.Helper()
	var copied runtimeAuthorityDocument
	if err := json.Unmarshal(runtimeAuthorityJSON, &copied); err != nil {
		t.Fatalf("decode embedded authority: %v", err)
	}
	return copied
}

func runtimeIndex(t *testing.T, doc runtimeAuthorityDocument, runtimeID string) int {
	t.Helper()
	for i, profile := range doc.Runtimes {
		if profile.RuntimeID == runtimeID {
			return i
		}
	}
	t.Fatalf("runtime %q is not in the embedded authority", runtimeID)
	return -1
}

// The whole reason a second runtime can be registered at all: a non-routable
// profile is fully described, addressable and comparable, and still cannot be
// advertised, quoted or matched. If registering MLX widened the sellable
// surface by one cell, the lifecycle state would be decoration.
func TestNonRoutableProfilesDoNotWidenTheSellableSurface(t *testing.T) {
	doc := mutableAuthority(t)
	if len(doc.Runtimes) < 2 {
		t.Fatal("this test is meaningless with fewer than two registered runtimes")
	}

	routable := doc.RoutableRuntimes()
	if len(routable) != 1 || routable[0].RuntimeID != "candle_metal" {
		t.Fatalf("routable runtimes = %+v, want candle_metal alone", routable)
	}
	for _, profile := range doc.Runtimes {
		if profile.RuntimeID == "candle_metal" {
			continue
		}
		if runtimeLifecycleRoutable(profile.Lifecycle) {
			t.Errorf("%s is routable at lifecycle %s without a Merc canary chain",
				profile.RuntimeID, profile.Lifecycle)
		}
	}

	for _, cap := range generatedAdvertisedRuntimeCapabilities {
		if cap.Runtime != "candle_metal" {
			t.Errorf("non-routable runtime %q reached the advertised projection: %+v",
				cap.Runtime, cap)
		}
	}
	if len(generatedAdvertisedRuntimeCapabilities) != 2 {
		t.Fatalf("advertised projection has %d cells, want the 2 candle cells",
			len(generatedAdvertisedRuntimeCapabilities))
	}
}

// The old loader panicked on anything but two models and two cells. That was
// fail-closed and it also made registering a runtime impossible. The replacement
// must still refuse every shape the count check was standing in for.
func TestRuntimeAuthorityValidationRefusesEveryUngovernedShape(t *testing.T) {
	if err := validateRuntimeAuthorityDocument(mutableAuthority(t)); err != nil {
		t.Fatalf("the embedded authority does not validate: %v", err)
	}

	for _, tc := range []struct {
		name   string
		want   string
		mutate func(*runtimeAuthorityDocument)
	}{
		{"a v1 document", "schema_version", func(d *runtimeAuthorityDocument) {
			d.SchemaVersion = 1
		}},
		{"an unnamed matrix", "matrix_version", func(d *runtimeAuthorityDocument) {
			d.MatrixVersion = ""
		}},
		{"no runtimes at all", "no runtimes", func(d *runtimeAuthorityDocument) {
			d.Runtimes = nil
		}},
		{"a runtime on an unregistered engine", "engine registry",
			func(d *runtimeAuthorityDocument) {
				d.Runtimes[0].Engine = "definitely-not-an-engine"
			}},
		{"an adapter that does not match its engine", "declares adapter",
			func(d *runtimeAuthorityDocument) {
				d.Runtimes[0].Adapter = "merc-something-else"
			}},
		{"a duplicate runtime identity", "declared twice",
			func(d *runtimeAuthorityDocument) {
				d.Runtimes = append(d.Runtimes, d.Runtimes[0])
			}},
		{"an unknown lifecycle", "unknown lifecycle", func(d *runtimeAuthorityDocument) {
			d.Runtimes[0].Lifecycle = "PROBABLY_FINE"
		}},
		{"a runtime with no cells", "declares no cells", func(d *runtimeAuthorityDocument) {
			d.Runtimes[runtimeIndex(t, *d, "mlx_metal")].Cells = nil
		}},
		{"a cell pointing at a model that does not exist", "undefined model",
			func(d *runtimeAuthorityDocument) {
				d.Runtimes[0].Cells[0].Model = "gpt-imaginary"
			}},
		{"an invalid device_count range", "device_count",
			func(d *runtimeAuthorityDocument) {
				d.Runtimes[0].Hardware.DeviceCount.Maximum = 0
			}},
		{"a runtime with no hardware platform", "hardware platform",
			func(d *runtimeAuthorityDocument) {
				d.Runtimes[0].Hardware.Platforms = nil
			}},
		{"a routable runtime with no benchmark authority", "benchmark authority",
			func(d *runtimeAuthorityDocument) {
				d.Runtimes[runtimeIndex(t, *d, "candle_metal")].BenchmarkAuthority = ""
			}},
		{"a routable runtime with no quality tier", "benchmark authority",
			func(d *runtimeAuthorityDocument) {
				d.Runtimes[runtimeIndex(t, *d, "candle_metal")].QualityTier = ""
			}},
		{"an unproven runtime claiming a capability", "unproven capabilities",
			func(d *runtimeAuthorityDocument) {
				d.Runtimes[runtimeIndex(t, *d, "mlx_metal")].Capabilities.Speculation = true
			}},
		{"an unproven runtime claiming tensor parallelism", "unproven capabilities",
			func(d *runtimeAuthorityDocument) {
				d.Runtimes[runtimeIndex(t, *d, "vllm_cuda")].Parallelism.TensorParallel = true
			}},
		{"two routable runtimes claiming one cell", "claimed by both",
			func(d *runtimeAuthorityDocument) {
				mlx := runtimeIndex(t, *d, "mlx_metal")
				d.Runtimes[mlx].Lifecycle = runtimeLifecycleActive
				d.Runtimes[mlx].BenchmarkAuthority = "docs/SPEED_LANE_2026-07-27.md"
				d.Runtimes[mlx].QualityTier = "OUTCOME_EQUIVALENT"
				d.Runtimes[mlx].Cells[0].ID = "candle-metal-llama1-infer"
			}},
		{"a sellable model no routable runtime serves", "no routable runtime profile serves",
			func(d *runtimeAuthorityDocument) {
				candle := runtimeIndex(t, *d, "candle_metal")
				d.Runtimes[candle].Lifecycle = runtimeLifecycleQuarantined
			}},
	} {
		doc := mutableAuthority(t)
		tc.mutate(&doc)
		err := validateRuntimeAuthorityDocument(doc)
		if err == nil {
			t.Errorf("%s was accepted", tc.name)
			continue
		}
		if !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%s refused with %q, want a message containing %q",
				tc.name, err.Error(), tc.want)
		}
	}
}

// Quarantining a runtime is not a demotion to "nearly routable". Ranking it
// above DRAFT would let a quarantined profile satisfy a lifecycle floor that a
// draft one cannot, which is exactly backwards.
func TestRuntimeLifecycleRankTreatsTerminalStatesAsExclusions(t *testing.T) {
	draft, _ := runtimeLifecycleRank(runtimeLifecycleDraft)
	for _, terminal := range []string{runtimeLifecycleQuarantined, runtimeLifecycleRetired} {
		rank, known := runtimeLifecycleRank(terminal)
		if !known {
			t.Fatalf("%s is not a known lifecycle", terminal)
		}
		if rank >= draft {
			t.Errorf("%s ranks %d, at or above DRAFT (%d); a terminal state is not progress",
				terminal, rank, draft)
		}
		if runtimeLifecycleRoutable(terminal) {
			t.Errorf("%s is routable", terminal)
		}
	}
	for _, state := range []string{
		runtimeLifecycleDraft, runtimeLifecycleValidated, runtimeLifecycleRealRuntimeProven,
	} {
		if runtimeLifecycleRoutable(state) {
			t.Errorf("%s is routable without a Merc canary chain", state)
		}
	}
	for _, state := range []string{runtimeLifecycleCanary, runtimeLifecycleActive} {
		if !runtimeLifecycleRoutable(state) {
			t.Errorf("%s is not routable", state)
		}
	}
	if _, known := runtimeLifecycleRank("ACTIVE_ISH"); known {
		t.Error("an invented lifecycle was accepted as known")
	}
}

// Lifecycle states are claims about evidence. Every registered profile must name
// the evidence behind its own claim, and a benchmark that ran outside the
// product may not carry a profile past VALIDATED.
func TestRegisteredRuntimesCiteEvidenceForTheirLifecycle(t *testing.T) {
	doc := mutableAuthority(t)
	for _, profile := range doc.Runtimes {
		if len(profile.Evidence) == 0 {
			t.Errorf("%s is registered at %s with no evidence cited",
				profile.RuntimeID, profile.Lifecycle)
		}
		rank, _ := runtimeLifecycleRank(profile.Lifecycle)
		provenRank, _ := runtimeLifecycleRank(runtimeLifecycleRealRuntimeProven)
		if rank >= provenRank && profile.RuntimeID != "candle_metal" {
			t.Errorf("%s claims %s; only candle_metal has a Merc-chain receipt today",
				profile.RuntimeID, profile.Lifecycle)
		}
	}
}
