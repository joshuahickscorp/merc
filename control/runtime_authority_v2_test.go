package main

import (
	"encoding/json"
	"os"
	"path/filepath"
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

	for _, cap := range advertisedRuntimeCapabilities() {
		if cap.Runtime != "candle_metal" {
			t.Errorf("non-routable runtime %q reached the advertised projection: %+v",
				cap.Runtime, cap)
		}
	}
	if len(advertisedRuntimeCapabilities()) != 1 {
		t.Fatalf("advertised projection has %d cells, want the 1 bindable candle cell",
			len(advertisedRuntimeCapabilities()))
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
				// llama_cpp_metal rather than mlx_metal: it is the profile that
				// actually has a bound benchmark receipt, so this mutation fails
				// the cell-collision rule rather than tripping the evidence
				// binding first and testing nothing.
				// The embed cell, whose verification is cosine rather than
				// byte_exact: this mutation is about the cell collision, and a
				// byte_exact cell would trip the determinism rule first and test
				// nothing.
				challenger := runtimeIndex(t, *d, "llama_cpp_metal")
				d.Runtimes[challenger].Lifecycle = runtimeLifecycleActive
				d.Runtimes[challenger].QualityTier = "OUTCOME_EQUIVALENT"
				// A well-formed cell in every OTHER respect, so the collision
				// rule is what refuses it. Its benchmark authority must measure
				// this cell's model, or the per-cell evidence rule fires first
				// and this stops testing what it says it tests.
				d.Runtimes[challenger].Cells = []authorityCell{{
					ID: "candle-metal-minilm-embed", Job: "embed",
					Model: "all-minilm-l6-v2", Runner: "embed",
					MinMemoryGB: 2, Verification: "cosine",
					Lifecycle:   runtimeLifecycleActive,
					QualityTier: "OUTCOME_EQUIVALENT",
					BenchmarkAuthority: "evidence/perf/runtime-benchmarks/" +
						"embed-cell-candle-vs-llama-cpp-r2.json",
				}}
			}},
		{"a sellable model no CANARY/ACTIVE runtime serves", "no CANARY/ACTIVE runtime cell serves",
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
		// REAL_RUNTIME_PROVEN and above is a claim that this profile completed a
		// Merc product chain, so it must cite a chain receipt that EXISTS.
		//
		// This used to hardcode "only candle_metal", which was true when written
		// and became false the moment llama.cpp completed the chain. Naming a
		// profile made the rule un-passable by design; requiring the artifact makes
		// it a real gate that the next runtime can also satisfy — and still fails
		// for a profile that claims the state with nothing behind it.
		rank, _ := runtimeLifecycleRank(profile.Lifecycle)
		provenRank, _ := runtimeLifecycleRank(runtimeLifecycleRealRuntimeProven)
		if rank >= provenRank {
			cited := false
			for _, evidence := range profile.Evidence {
				if !strings.HasPrefix(evidence, "evidence/") {
					continue
				}
				if _, err := os.Stat(filepath.Join("..", evidence)); err != nil {
					continue
				}
				if strings.Contains(evidence, "/chain/") ||
					strings.Contains(evidence, "/canary/") {
					cited = true
				}
			}
			if !cited {
				t.Errorf("%s claims %s but cites no existing Merc-chain or canary receipt: %v",
					profile.RuntimeID, profile.Lifecycle, profile.Evidence)
			}
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
	// Pinned under capability manifest v2.
	//
	// These values moved without any profile taking a new revision, which is the
	// one case where editing the constants in place is correct rather than a
	// violation: the DEFINITION of the digest changed, the profiles did not.
	// Cell lifecycle, quality tier, benchmark authority and rejection reason used
	// to be inside the digest and are now activation policy, so a promotion no
	// longer forces a revision bump — and no longer forces every agent to be
	// rebuilt before it can enrol.
	//
	// The upgrade is handled by capability_manifest_version on runtime_profiles:
	// a row written under v1 is recognised and rewritten rather than reported as
	// content drift, and its old digest is retained so anything that recorded it
	// still resolves.
	pinned := map[string]string{
		"candle_metal":      "b9f27c5095194d97ed6a48fca5bf6f3dce56de9ab912ad6177d400c43e1718c3",
		"mlx_metal":         "caa99a2d13e1a742d757500c22aa073c3a3514f4f6e034aea7ec8d8c9b755086",
		"llama_cpp_metal":   "4f3da7514fca79fe5a1f25a57a5333df3eb0a091ff9179da70eeb0a3ab223efe",
		"vllm_cuda":         "9f4a241f9c3a0bb017303cf50b036aaf31ace5934e9d6562051c887e1d42f5e3",
		"sglang_cuda":       "e6762ad45654e3756b98b75b89779e23009914590a8f8ce15a11c674665a08f8",
		"tensorrt_llm_cuda": "3f5b97cf14da1671983f8f265f7619ee54c56b6c9efdb0d1e491ace8efac3e17",
		"lmdeploy_cuda":     "d31ba7d54d55691f3e43073d9e5df00a11c91a34607b1966b89f5c73fab65650",
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
		got, err := profile.CapabilityDigest(runtimeAuthorityModels)
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
	want, err := base.CapabilityDigest(runtimeAuthorityModels)
	if err != nil {
		t.Fatal(err)
	}
	for _, mutate := range []func(p *authorityRuntimeProfile){
		func(p *authorityRuntimeProfile) { p.Lifecycle = runtimeLifecycleQuarantined },
		func(p *authorityRuntimeProfile) { p.Lifecycle = runtimeLifecycleCanary },
		func(p *authorityRuntimeProfile) { p.SupersededBy = "something_else" },
		// Activation policy, not capability. The quality tier and the receipt
		// that justified a promotion both move as evidence accumulates, and both
		// used to sit inside the digest — so recording a new benchmark forced a
		// revision bump and, because the agent binds the same document at compile
		// time, a rebuild of every agent before any of them could enrol.
		func(p *authorityRuntimeProfile) { p.QualityTier = "MODEL_EXACT" },
		func(p *authorityRuntimeProfile) { p.BenchmarkAuthority = "elsewhere" },
	} {
		moved := base
		mutate(&moved)
		got, err := moved.CapabilityDigest(runtimeAuthorityModels)
		if err != nil {
			t.Fatal(err)
		}
		if got != want {
			t.Errorf("a lifecycle or supersession change altered the content digest")
		}
	}

	// Anything that changes what the profile MEANS must move the digest.
	for name, mutate := range map[string]func(p *authorityRuntimeProfile){
		"engine":   func(p *authorityRuntimeProfile) { p.Engine = "mlx" },
		"revision": func(p *authorityRuntimeProfile) { p.Revision = "r99" },
		// A real other model, not a made-up id: the digest now resolves each
		// cell's artifacts, so an undefined model is a hard error rather than a
		// different digest, and the mutation would prove nothing.
		"cell model":     func(p *authorityRuntimeProfile) { p.Cells[0].Model = "llama-3.2-1b-instruct-q4" },
		"cell wire kind": func(p *authorityRuntimeProfile) { p.Cells[0].WireKind = "gguf" },
		"memory floor":   func(p *authorityRuntimeProfile) { p.Cells[0].MinMemoryGB = 99 },
		"device count":   func(p *authorityRuntimeProfile) { p.Hardware.DeviceCount.Maximum = 8 },
		"capability":     func(p *authorityRuntimeProfile) { p.Capabilities.Speculation = true },
	} {
		moved := base
		moved.Cells = append([]authorityCell(nil), base.Cells...)
		mutate(&moved)
		got, err := moved.CapabilityDigest(runtimeAuthorityModels)
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

// The hole this closes was real and I put it there: candle_metal claimed
// routability backed by control/runtime-profiles/vllm-llama-3.2-1b-instruct-bf16.json
// — a receipt for a different ENGINE, a different dtype, a different model
// revision, whose own benchmark_status said UNPROVEN. The check at the time was
// "benchmark_authority is a non-empty string", which any path satisfies.
//
// A profile's evidence must at minimum name the profile it is evidence for.
// This is not proof of a measurement: candle's honest receipt names candle and
// measures nothing, and that is the correct state to be in until it is
// benchmarked on the same harness as its challenger.
func TestBenchmarkAuthorityMustNameTheProfileItEvidences(t *testing.T) {
	if err := validateRuntimeAuthorityDocument(mutableAuthority(t)); err != nil {
		t.Fatalf("the embedded authority does not validate: %v", err)
	}

	for _, tc := range []struct {
		name, want string
		mutate     func(*runtimeAuthorityDocument)
	}{
		{"a receipt for another profile", "evidence for",
			func(d *runtimeAuthorityDocument) {
				d.Runtimes[runtimeIndex(t, *d, "candle_metal")].BenchmarkAuthority =
					"evidence/perf/runtime-benchmarks/llama-cpp-metal-llama1-q4-r1.json"
			}},
		{"a path that is not a known receipt", "not a known receipt",
			func(d *runtimeAuthorityDocument) {
				d.Runtimes[runtimeIndex(t, *d, "candle_metal")].BenchmarkAuthority =
					"docs/SPEED_LANE_2026-07-27.md"
			}},
		{"the original defect verbatim", "not a known receipt",
			func(d *runtimeAuthorityDocument) {
				d.Runtimes[runtimeIndex(t, *d, "candle_metal")].BenchmarkAuthority =
					"control/runtime-profiles/vllm-llama-3.2-1b-instruct-bf16.json"
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

// The embedded manifest is what the control plane validates against at process
// start, because a container ships the binary and not evidence/. That makes it a
// second copy of facts the receipts already state, and a second copy drifts.
//
// The comment it replaces claimed the manifest was generated by
// scripts/gen-benchmark-manifest.py, which does not exist in this tree. A check
// is stronger than a generator anyway: it catches a receipt edited after the
// manifest was written, which regeneration only catches if someone remembers to
// run it.
func TestBenchmarkManifestMatchesTheReceipts(t *testing.T) {
	for path, summary := range benchmarkAuthorityManifest {
		raw, err := os.ReadFile("../" + path)
		if err != nil {
			t.Errorf("manifest names %s, which does not exist: %v", path, err)
			continue
		}
		var receipt map[string]any
		if err := json.Unmarshal(raw, &receipt); err != nil {
			t.Errorf("%s is not JSON: %v", path, err)
			continue
		}
		if len(summary.RuntimeProfileIDs) == 0 {
			t.Errorf("%s is evidence for no profile", path)
			continue
		}
		// Every profile the manifest claims must be one the document defines.
		// A manifest naming a profile nobody registered is a pointer to nothing.
		for _, id := range summary.RuntimeProfileIDs {
			found := false
			for _, profile := range runtimeAuthority.Runtimes {
				if profile.RuntimeID == id {
					found = true
				}
			}
			if !found {
				t.Errorf("%s claims evidence for unregistered profile %q", path, id)
			}
		}
		// And the receipt itself must name them, in whichever of the two shapes
		// it uses: a single-profile receipt carries runtime_profile_id, a
		// comparison carries a profiles object keyed by id.
		named := map[string]bool{}
		if single, ok := receipt["runtime_profile_id"].(string); ok {
			named[single] = true
		}
		if profiles, ok := receipt["profiles"].(map[string]any); ok {
			for id := range profiles {
				named[id] = true
			}
		}
		for _, id := range summary.RuntimeProfileIDs {
			if !named[id] {
				t.Errorf("manifest says %s is evidence for %q, but the receipt does not name it",
					path, id)
			}
		}
	}
}

// llama_cpp_metal now carries a real measurement taken on this machine against
// the exact pinned artifact. It is still not routable, and the receipt says why.
func TestLlamaCppBenchmarkReceiptIsBoundAndHonestAboutItsLimits(t *testing.T) {
	raw, err := os.ReadFile("../evidence/perf/runtime-benchmarks/llama-cpp-metal-llama1-q4-r1.json")
	if err != nil {
		t.Fatalf("read receipt: %v", err)
	}
	var receipt struct {
		RuntimeProfileID    string            `json:"runtime_profile_id"`
		ModelArtifactSHA256 string            `json:"model_artifact_sha256"`
		ModelRevision       string            `json:"model_revision"`
		BenchmarkStatus     string            `json:"benchmark_status"`
		NotEstablished      map[string]string `json:"not_established"`
		PhysicalThroughput  struct {
			PrefillTokensPerSec float64 `json:"prefill_tokens_per_sec"`
			DecodeTokensPerSec  float64 `json:"decode_tokens_per_sec"`
		} `json:"physical_throughput"`
	}
	if err := json.Unmarshal(raw, &receipt); err != nil {
		t.Fatalf("decode receipt: %v", err)
	}

	// Bound to the exact artifact the catalogue prices, not to "a llama 1B".
	var model authorityModel
	for _, m := range runtimeAuthority.Models {
		if m.ID == "llama-3.2-1b-instruct-q4" {
			model = m
		}
	}
	if receipt.ModelRevision != model.HFRevision {
		t.Errorf("receipt model revision %s, authority pins %s",
			receipt.ModelRevision, model.HFRevision)
	}
	var pinnedSHA string
	for _, a := range model.Artifacts {
		if strings.HasSuffix(a.Path, ".gguf") {
			pinnedSHA = a.SHA256
		}
	}
	if receipt.ModelArtifactSHA256 != pinnedSHA {
		t.Errorf("receipt artifact %s, authority pins %s",
			receipt.ModelArtifactSHA256, pinnedSHA)
	}
	if receipt.PhysicalThroughput.PrefillTokensPerSec <= 0 ||
		receipt.PhysicalThroughput.DecodeTokensPerSec <= 0 {
		t.Error("the receipt claims PHYSICAL_THROUGHPUT_MEASURED with no rates")
	}

	// And it must name what it does NOT establish, because a benchmark receipt
	// read three months later is exactly where an unstated gap becomes a claim.
	for _, gap := range []string{"quality_tier", "delivered_throughput", "merc_chain", "cost"} {
		if receipt.NotEstablished[gap] == "" {
			t.Errorf("receipt does not state what it fails to establish about %q", gap)
		}
	}

	profile, ok := runtimeProfileByID("llama_cpp_metal")
	if !ok {
		t.Fatal("llama_cpp_metal is not registered")
	}
	if runtimeLifecycleRoutable(profile.Lifecycle) {
		t.Error("a physical-throughput measurement made llama_cpp_metal routable; " +
			"routability also needs a proven quality tier and a complete Merc chain")
	}
}

// The measurement that produced this rule, kept as a regression.
//
// llama_cpp_metal came back 4.31x faster than the incumbent at peak on identical
// inputs, and diverged from its OWN serial output at every batch size tested.
// The batch_infer cell declares byte_exact verification. Promoting on throughput
// alone would have routed buyer work to an engine that cannot satisfy the
// verification contract the cell sells.
func TestThroughputCannotPromoteAProfileThatFailsItsVerificationContract(t *testing.T) {
	doc := mutableAuthority(t)
	challenger := runtimeIndex(t, doc, "llama_cpp_metal")

	// Its receipt says it is faster. That is not enough.
	summary, ok := benchmarkAuthorityManifest[doc.Runtimes[challenger].BenchmarkAuthority]
	if !ok {
		t.Fatalf("llama_cpp_metal has no known benchmark receipt")
	}
	if !summary.ThroughputMeasured {
		t.Fatal("llama_cpp_metal's receipt records no throughput; this test is vacuous")
	}
	if summary.ByteDeterministic {
		t.Fatal("llama_cpp_metal's receipt now claims byte determinism; " +
			"re-measure before relaxing this test")
	}

	// Promoting it must be refused, naming the cell whose contract it breaks.
	//
	// The promotion has to be attempted on the CELL now, not on the profile.
	// Making the profile ACTIVE no longer promotes anything by itself — that is
	// the correction cell-level authority exists to make — so a mutation that
	// only touched the profile would be refused by nothing and pass this test
	// while proving the opposite of what it claims.
	doc.Runtimes[challenger].Lifecycle = runtimeLifecycleActive
	doc.Runtimes[challenger].QualityTier = "OUTCOME_EQUIVALENT"
	byteExact := -1
	for i, cell := range doc.Runtimes[challenger].Cells {
		if cell.Verification == "byte_exact" {
			byteExact = i
		}
	}
	if byteExact < 0 {
		t.Fatal("llama_cpp_metal declares no byte_exact cell; this test is vacuous")
	}
	doc.Runtimes[challenger].Cells[byteExact].Lifecycle = runtimeLifecycleActive
	doc.Runtimes[challenger].Cells[byteExact].RejectionReason = ""
	doc.Runtimes[challenger].Cells[byteExact].QualityTier = "OUTCOME_EQUIVALENT"
	err := validateRuntimeAuthorityDocument(doc)
	if err == nil {
		t.Fatal("a non-byte-deterministic engine was promoted onto a byte_exact cell")
	}
	if !strings.Contains(err.Error(), "not byte-deterministic") {
		t.Errorf("refusal said %q, want it to name the determinism failure", err.Error())
	}

	// The incumbent measures byte-identical at every batch size, so the rule
	// does not simply refuse everything.
	clean := mutableAuthority(t)
	if err := validateRuntimeAuthorityDocument(clean); err != nil {
		t.Fatalf("the byte-deterministic incumbent was refused: %v", err)
	}
}

// Both profiles are now measured on one harness, so a comparison is finally
// meaningful — and the comparison is what says the fast one may not be used.
func TestBothMetalProfilesAreMeasuredAndComparable(t *testing.T) {
	cap := adapterTestWorker()
	estimates, err := EstimateAcrossRegisteredRuntimes(inferWorkload(), cap)
	if err != nil {
		t.Fatal(err)
	}
	byID := map[string]RuntimeEstimate{}
	for _, e := range estimates {
		byID[e.RuntimeProfileID] = e
	}
	for _, id := range []string{"candle_metal", "llama_cpp_metal"} {
		if !byID[id].Comparable {
			t.Errorf("%s has a measured receipt but is not comparable", id)
		}
	}
	for _, id := range []string{"mlx_metal", "vllm_cuda"} {
		if byID[id].Comparable {
			t.Errorf("%s has no measured receipt but is comparable", id)
		}
	}
	if byID["llama_cpp_metal"].Routable {
		t.Error("the faster profile is routable; it fails byte_exact verification")
	}
}

// Phase 0 D: the digest must cover every field that changes what a profile
// MEANS, and none that describe where it is in its lifecycle.
//
// Two gaps were real and are closed here: tokenizer/chat-template revision
// (identical weights under a different template are a different product, and a
// benchmark under one does not transfer to the other) and source identity (two
// documents could otherwise define byte-identical content and be
// indistinguishable in provenance).
func TestContentDigestCoversEverySemanticFieldAndNoLifecycleField(t *testing.T) {
	base := mutableAuthority(t).Runtimes[0]
	want, err := base.CapabilityDigest(runtimeAuthorityModels)
	if err != nil {
		t.Fatal(err)
	}

	// Semantic: must move the digest.
	for name, mutate := range map[string]func(p *authorityRuntimeProfile){
		"engine":             func(p *authorityRuntimeProfile) { p.Engine = "mlx" },
		"engine revision":    func(p *authorityRuntimeProfile) { p.EngineRevision = "deadbeef" },
		"tokenizer revision": func(p *authorityRuntimeProfile) { p.TokenizerRevision = "other" },
		"chat template":      func(p *authorityRuntimeProfile) { p.ChatTemplateID = "other" },
		"source identity":    func(p *authorityRuntimeProfile) { p.SourceIdentity = "elsewhere" },
		"adapter":            func(p *authorityRuntimeProfile) { p.Adapter = "merc-mlx" },
		"revision":           func(p *authorityRuntimeProfile) { p.Revision = "r99" },
		"device":             func(p *authorityRuntimeProfile) { p.Device = "cuda" },
		"platform":           func(p *authorityRuntimeProfile) { p.Hardware.Platforms = []string{"x"} },
		"device count":       func(p *authorityRuntimeProfile) { p.Hardware.DeviceCount.Maximum = 8 },
		"parallelism":        func(p *authorityRuntimeProfile) { p.Parallelism.TensorParallel = true },
		"capability":         func(p *authorityRuntimeProfile) { p.Capabilities.Speculation = true },
		"cell model":         func(p *authorityRuntimeProfile) { p.Cells[0].Model = "llama-3.2-1b-instruct-q4" },
		"cell memory":        func(p *authorityRuntimeProfile) { p.Cells[0].MinMemoryGB = 99 },
		// Artifact format is what the runtime actually loads. candle's Cells[0]
		// serves MiniLM as `hf`; flipping it to `gguf` resolves a different file
		// entirely, and a digest that missed that would let a profile swap every
		// executed byte while keeping its revision.
		"cell wire kind": func(p *authorityRuntimeProfile) { p.Cells[0].WireKind = "gguf" },
		// A value the cell does not already have: candle's Cells[0] is the embed
		// cell, which is cosine already, so asserting "cosine" tested nothing.
		"cell verification": func(p *authorityRuntimeProfile) {
			p.Cells[0].Verification = "definitely-not-the-current-strategy"
		},
	} {
		moved := base
		moved.Cells = append([]authorityCell(nil), base.Cells...)
		mutate(&moved)
		got, err := moved.CapabilityDigest(runtimeAuthorityModels)
		if err != nil {
			t.Fatal(err)
		}
		if got == want {
			t.Errorf("changing the %s did not move the content digest", name)
		}
	}

	// Lifecycle: must NOT move the digest, or promotion would look like
	// replacement and every promoted profile would need a new revision.
	for name, mutate := range map[string]func(p *authorityRuntimeProfile){
		"lifecycle":     func(p *authorityRuntimeProfile) { p.Lifecycle = runtimeLifecycleCanary },
		"quarantine":    func(p *authorityRuntimeProfile) { p.Lifecycle = runtimeLifecycleQuarantined },
		"superseded_by": func(p *authorityRuntimeProfile) { p.SupersededBy = "something" },
		// Activation policy: both describe how much is KNOWN about a profile, not
		// what it is. Keeping them in the digest meant a new receipt was
		// indistinguishable from a new runtime.
		"quality tier":        func(p *authorityRuntimeProfile) { p.QualityTier = "MODEL_EXACT" },
		"benchmark authority": func(p *authorityRuntimeProfile) { p.BenchmarkAuthority = "elsewhere" },
		"cell lifecycle":      func(p *authorityRuntimeProfile) { p.Cells[0].Lifecycle = runtimeLifecycleValidated },
	} {
		moved := base
		mutate(&moved)
		got, err := moved.CapabilityDigest(runtimeAuthorityModels)
		if err != nil {
			t.Fatal(err)
		}
		if got != want {
			t.Errorf("changing the %s moved the content digest", name)
		}
	}
}

// Every registered profile must carry the provenance the digest binds. A blank
// source identity would make the digest cover an empty string rather than a
// fact.
func TestEveryProfileDeclaresItsProvenance(t *testing.T) {
	for _, p := range mutableAuthority(t).Runtimes {
		if p.SourceIdentity == "" {
			t.Errorf("%s declares no source_identity", p.RuntimeID)
		}
		if p.ChatTemplateID == "" {
			t.Errorf("%s declares no chat_template_id", p.RuntimeID)
		}
	}
}

// Artifact format belongs to the (runtime, model) pair, not to the model.
//
// This was the blocker the last tranche stopped at: candle serves
// all-minilm-l6-v2 from safetensors and llama.cpp serves the same logical model
// from a GGUF, and a globally-declared wire_kind could not express it. The
// measurement that made it worth fixing rather than working around: the two
// agree at 0.999999 mean cosine against a 0.999 verification gate.
func TestWireKindBelongsToTheRuntimeModelPair(t *testing.T) {
	doc := mutableAuthority(t)
	if err := validateRuntimeAuthorityDocument(doc); err != nil {
		t.Fatalf("embedded authority does not validate: %v", err)
	}

	var embedCell *authorityCell
	for i, p := range doc.Runtimes {
		if p.RuntimeID != "llama_cpp_metal" {
			continue
		}
		for j := range doc.Runtimes[i].Cells {
			if doc.Runtimes[i].Cells[j].Job == "embed" {
				embedCell = &doc.Runtimes[i].Cells[j]
			}
		}
	}
	if embedCell == nil {
		t.Fatal("llama_cpp_metal has no embed cell")
	}
	if embedCell.WireKind != "gguf" {
		t.Fatalf("embed cell wire kind = %q, want gguf", embedCell.WireKind)
	}
	if embedCell.Verification != "cosine" {
		t.Fatalf("embed cell verification = %q; the whole point is that it is not "+
			"byte_exact, which llama.cpp cannot satisfy under batching",
			embedCell.Verification)
	}

	// The model itself still declares 'hf' — candle's format. Two runtimes, one
	// logical model, two artifact formats, and the document now says so.
	var model authorityModel
	for _, m := range doc.Models {
		if m.ID == "all-minilm-l6-v2" {
			model = m
		}
	}
	if model.WireKind != "hf" {
		t.Fatalf("model wire kind = %q, want hf (candle's format)", model.WireKind)
	}

	// An unset cell wire kind still inherits the model's, which is what every
	// cell did when one runtime existed.
	if got := wireKindFor(authorityCell{}, model.WireKind); got != "hf" {
		t.Errorf("an unset cell wire kind resolved to %q, want the model's hf", got)
	}
	if got := wireKindFor(authorityCell{WireKind: "gguf"}, model.WireKind); got != "gguf" {
		t.Errorf("a declared cell wire kind resolved to %q, want gguf", got)
	}

	// The format set is still closed: an agent cannot be asked to load something
	// it has no loader for.
	bad := mutableAuthority(t)
	for i := range bad.Runtimes {
		if bad.Runtimes[i].RuntimeID == "llama_cpp_metal" {
			bad.Runtimes[i].Cells[0].WireKind = "onnx"
		}
	}
	err := validateRuntimeAuthorityDocument(bad)
	if err == nil {
		t.Fatal("an unknown wire kind was accepted")
	}
	if !strings.Contains(err.Error(), "unknown wire kind") {
		t.Errorf("refusal said %q", err.Error())
	}
}

// Registering the embed cell must not have widened what is sellable: the cell is
// on a VALIDATED profile and cannot reach the advertised projection.
func TestTheNewEmbedCellIsNotYetSellable(t *testing.T) {
	for _, cap := range advertisedRuntimeCapabilities() {
		if cap.ID == "llama-cpp-metal-minilm-embed" {
			t.Fatal("a cell on a VALIDATED profile reached the advertised projection")
		}
		if cap.Runtime != "candle_metal" {
			t.Errorf("non-routable runtime %q is advertised", cap.Runtime)
		}
	}
	if len(advertisedRuntimeCapabilities()) != 1 {
		t.Fatalf("advertised projection has %d cells, want the 1 bindable candle cell",
			len(advertisedRuntimeCapabilities()))
	}
}
