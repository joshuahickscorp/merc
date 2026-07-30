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

// Profile content is immutable under a (runtime_id, revision) pair. Until the
// governed runtime_profiles table exists to refuse content drift at runtime,
// this pinned digest IS the enforcement: editing what a registered profile means
// without bumping its revision fails CI, and the fix is to add r2 rather than to
// update the constant.
//
// The same mechanism already earned its keep once — the pinned historical
// compute-plan digest caught a cosmetic runtime rename that would have broken
// task provenance.
//
// Lifecycle is deliberately NOT in the digest. Moving candle_metal from ACTIVE to
// QUARANTINED, or mlx_metal from VALIDATED to CANARY, must not require a new
// revision: the progression is the point of the state machine.
func TestRuntimeProfileContentDigestsArePinned(t *testing.T) {
	pinned := map[string]string{
		"candle_metal":    "ddc5c69b975cbf3e6208afc4a76c063459d78c8aa4245f5e4adeb9bdcacf2339",
		"mlx_metal":       "91c1d6b83a1148b58f29a30cd37057631d9d40c4f086c4b9ff79c07c4ed02f88",
		"llama_cpp_metal": "a6416e8a136d9a6538ec009142a0d9ac17c7515125f51c819a6f5a34c5e973ca",
		"vllm_cuda":       "e1f18591a44d6b9c98e257a1187c3a04f0fc5365472789250993938431dd8c99",
	}
	doc := mutableAuthority(t)
	if len(doc.Runtimes) != len(pinned) {
		t.Fatalf("%d registered runtimes, %d pinned digests; a new profile needs a pin",
			len(doc.Runtimes), len(pinned))
	}
	for _, profile := range doc.Runtimes {
		want, ok := pinned[profile.RuntimeID]
		if !ok {
			t.Errorf("runtime %q has no pinned content digest", profile.RuntimeID)
			continue
		}
		got, err := profile.ContentDigest()
		if err != nil {
			t.Fatalf("digest %q: %v", profile.RuntimeID, err)
		}
		if got != want {
			t.Errorf("runtime %q content changed under revision %s\n  got  %s\n  want %s\n"+
				"Bump the revision and pin the new digest; do not edit the constant in place.",
				profile.RuntimeID, profile.Revision, got, want)
		}
	}
}

// Lifecycle and supersession are the two things allowed to change without a new
// revision. If either entered the digest, promoting a runtime would look like
// replacing it.
func TestRuntimeProfileDigestExcludesLifecycleAndSupersession(t *testing.T) {
	base := mutableAuthority(t).Runtimes[0]
	want, err := base.ContentDigest()
	if err != nil {
		t.Fatal(err)
	}
	for _, mutate := range []func(p *authorityRuntimeProfile){
		func(p *authorityRuntimeProfile) { p.Lifecycle = runtimeLifecycleQuarantined },
		func(p *authorityRuntimeProfile) { p.Lifecycle = runtimeLifecycleCanary },
		func(p *authorityRuntimeProfile) { p.SupersededBy = "something_else" },
	} {
		moved := base
		mutate(&moved)
		got, err := moved.ContentDigest()
		if err != nil {
			t.Fatal(err)
		}
		if got != want {
			t.Errorf("a lifecycle or supersession change altered the content digest")
		}
	}

	// Anything that changes what the profile MEANS must move the digest.
	for name, mutate := range map[string]func(p *authorityRuntimeProfile){
		"engine":              func(p *authorityRuntimeProfile) { p.Engine = "mlx" },
		"revision":            func(p *authorityRuntimeProfile) { p.Revision = "r2" },
		"quality tier":        func(p *authorityRuntimeProfile) { p.QualityTier = "MODEL_EXACT" },
		"benchmark authority": func(p *authorityRuntimeProfile) { p.BenchmarkAuthority = "elsewhere" },
		"cell model":          func(p *authorityRuntimeProfile) { p.Cells[0].Model = "other-model" },
		"memory floor":        func(p *authorityRuntimeProfile) { p.Cells[0].MinMemoryGB = 99 },
		"device count":        func(p *authorityRuntimeProfile) { p.Hardware.DeviceCount.Maximum = 8 },
		"capability":          func(p *authorityRuntimeProfile) { p.Capabilities.Speculation = true },
	} {
		moved := base
		moved.Cells = append([]authorityCell(nil), base.Cells...)
		mutate(&moved)
		got, err := moved.ContentDigest()
		if err != nil {
			t.Fatal(err)
		}
		if got == want {
			t.Errorf("changing the %s did not move the content digest", name)
		}
	}
}

// A superseded profile has been replaced. Continuing to route buyer work to it
// means the replacement was never actually adopted.
func TestSupersededProfilesCannotBeRoutable(t *testing.T) {
	for _, tc := range []struct {
		name   string
		want   string
		mutate func(*runtimeAuthorityDocument)
	}{
		{"a routable superseded profile", "still routable",
			func(d *runtimeAuthorityDocument) {
				d.Runtimes[runtimeIndex(t, *d, "candle_metal")].SupersededBy = "mlx_metal"
			}},
		{"a profile superseded by itself", "supersedes itself",
			func(d *runtimeAuthorityDocument) {
				mlx := runtimeIndex(t, *d, "mlx_metal")
				d.Runtimes[mlx].SupersededBy = "mlx_metal"
			}},
		{"a profile superseded by an unregistered runtime", "not registered",
			func(d *runtimeAuthorityDocument) {
				d.Runtimes[runtimeIndex(t, *d, "mlx_metal")].SupersededBy = "ghost_runtime"
			}},
		{"a descriptive revision string", "want r1, r2",
			func(d *runtimeAuthorityDocument) {
				d.Runtimes[0].Revision = "v2-mixed-bit-retune"
			}},
		{"a missing revision", "want r1, r2",
			func(d *runtimeAuthorityDocument) { d.Runtimes[0].Revision = "" }},
		{"a zero revision", "want r1, r2",
			func(d *runtimeAuthorityDocument) { d.Runtimes[0].Revision = "r0" }},
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
