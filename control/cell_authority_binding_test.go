package main

import (
	"encoding/json"
	"fmt"
	"os"
	"slices"
	"sort"
	"strings"
	"testing"
)

// Scoreboard after exact execution identity became mandatory. G070 bound
// candle-metal-llama1-infer under settlement geometry
// tokens/token_like_input_plus_max_output_tokens (r5). The prior llama1 r4
// receipt remains readable history and is SUPERSEDED (incomplete agent content
// root). Embed stays parked (embeddings/s receipt, not settlement geometry).
// Media remains unbound. Mechanics tests still install explicit TEST_ONLY
// authorities without relabeling evidence.

func TestOnlyBindableAuthorityCellsAreRoutable(t *testing.T) {
	// Reasons the audit named. Each demotion is a predicate result, not a
	// hand-edited lifecycle field. Media cells still fail pre-BOUND checks;
	// the one BOUND generation cell is current-routable under r5.
	wantDemoted := map[string]string{
		"candle-metal-minilm-embed":     "engine_build_hash",
		"candle-metal-ffmpeg-transcode": "not a git object",
		"candle-metal-scene-render":     "merc_source_commit is missing",
	}
	wantRoutable := map[string]bool{
		"candle-metal-llama1-infer": true,
	}

	var got []string
	for _, profile := range runtimeAuthority.Runtimes {
		for _, cell := range profile.Cells {
			if !runtimeLifecycleRoutable(cell.EffectiveLifecycle(profile)) {
				continue
			}
			ok, reason := cellAuthorityBindable(profile, cell)
			if cell.Routable(profile) {
				got = append(got, cell.ID)
				if !ok {
					t.Errorf("%s is Routable but bindable says no: %s", cell.ID, reason)
				}
				if !wantRoutable[cell.ID] {
					t.Errorf("unexpected routable cell %s", cell.ID)
				}
				continue
			}
			// Lifecycle says CANARY/ACTIVE but Routable is false — must be one of
			// the demotions the audit named, with a matching reason.
			if substr, named := wantDemoted[cell.ID]; named {
				if ok {
					t.Errorf("%s should not bind; got ok with reason %q", cell.ID, reason)
				}
				if !strings.Contains(reason, substr) {
					if cell.ID == "candle-metal-scene-render" &&
						(strings.Contains(reason, "merc_source_commit") ||
							strings.Contains(reason, "harness")) {
						continue
					}
					t.Errorf("%s demoted for %q, want reason containing %q",
						cell.ID, reason, substr)
				}
				continue
			}
			t.Errorf("unexpected demotion of lifecycle-routable cell %s: %s", cell.ID, reason)
		}
	}
	if len(got) != len(wantRoutable) {
		t.Fatalf("routable cells = %v, want exactly the BOUND set %v", got, wantRoutable)
	}
	for id := range wantRoutable {
		found := false
		for _, g := range got {
			if g == id {
				found = true
				break
			}
		}
		if !found {
			t.Errorf("expected routable cell %s missing from %v", id, got)
		}
	}
	if n := len(advertisedRuntimeCapabilities()); n != len(wantRoutable) {
		t.Fatalf("advertised projection has %d cells, want %d", n, len(wantRoutable))
	}
	for id := range wantRoutable {
		if !advertisedRuntimeCell(id) {
			t.Errorf("BOUND cell %s is not advertised", id)
		}
	}
	for id := range wantDemoted {
		if advertisedRuntimeCell(id) {
			t.Errorf("demoted cell %s is still advertised", id)
		}
	}
}

func TestSupersededIncompleteAgentContentRootCannotAuthorizeCurrentAdmission(t *testing.T) {
	// G070: the cell is current-bound under r5. The incomplete-content-root
	// guard still holds: the superseded r4 credential (97acc / SUPERSEDED)
	// cannot authorize current admission if a cell is temporarily repointed at
	// it. The live cell remains advertised only via its current r5 authority.
	const cellID = "candle-metal-llama1-infer"
	const supersededPath = "evidence/perf/runtime-benchmarks/candle-metal-llama1-q4-r4.json"

	r4, ok := benchmarkAuthorityManifest[supersededPath]
	if !ok {
		t.Fatalf("historical r4 receipt missing from manifest: %s", supersededPath)
	}
	if reason := authorityValidityRefusal(r4.Validity); reason != authorityValiditySuperseded {
		t.Fatalf("r4 validity=%q, want SUPERSEDED incomplete-content-root credential", r4.Validity)
	}
	if r4.EngineBuildHash != "97acc6fe17daca56" {
		t.Fatalf("r4 engine_build_hash=%q, want the incomplete-content-root 97acc credential",
			r4.EngineBuildHash)
	}

	// Live cell must resolve under r5 and stay buyer-advertised.
	_, _, current, err := currentRuntimeCellBenchmarkIdentity(cellID)
	if err != nil {
		t.Fatalf("current r5-bound cell must authorize: %v", err)
	}
	if current.EngineBuildHash == "97acc6fe17daca56" ||
		strings.Contains(strings.ToLower(current.Harness), "r4") {
		t.Fatalf("current identity still carries the superseded r4 credential: %+v", current)
	}
	if !advertisedRuntimeCell(cellID) {
		t.Fatalf("BOUND cell %s is not advertised under its current r5 authority", cellID)
	}

	// Incomplete-content-root credential cannot authorize: repoint only this
	// test's in-memory cell at the SUPERSEDED r4 path and prove routability
	// collapses without touching disk evidence.
	savedAuthority := runtimeAuthority
	savedActivation := activeRuntimeActivation.Load()
	edited := runtimeAuthority
	edited.Runtimes = append([]authorityRuntimeProfile(nil), runtimeAuthority.Runtimes...)
	found := false
	for i := range edited.Runtimes {
		edited.Runtimes[i].Cells = append([]authorityCell(nil), edited.Runtimes[i].Cells...)
		for j := range edited.Runtimes[i].Cells {
			if edited.Runtimes[i].Cells[j].ID != cellID {
				continue
			}
			edited.Runtimes[i].Cells[j].BenchmarkAuthority = supersededPath
			found = true
		}
	}
	if !found {
		t.Fatalf("cell %s missing from runtime authority", cellID)
	}
	runtimeAuthority = edited
	activeRuntimeActivation.Store(newRuntimeActivation(
		currentActivation().PolicyRevision, map[string]string{}, nil))
	t.Cleanup(func() {
		runtimeAuthority = savedAuthority
		activeRuntimeActivation.Store(savedActivation)
	})

	var profile authorityRuntimeProfile
	var cell authorityCell
	for _, p := range runtimeAuthority.Runtimes {
		for _, c := range p.Cells {
			if c.ID == cellID {
				profile, cell = p, c
			}
		}
	}
	if cell.Routable(profile) {
		t.Fatal("cell stayed routable when repointed at the SUPERSEDED incomplete-content-root r4 credential")
	}
	okBind, reason := cellAuthorityBindable(profile, cell)
	if okBind || !strings.Contains(reason, authorityValiditySuperseded) {
		t.Fatalf("repointed r4 credential bindable ok=%v reason=%q, want SUPERSEDED refusal",
			okBind, reason)
	}
	_, _, _, err = currentRuntimeCellBenchmarkIdentity(cellID)
	if err == nil || !strings.Contains(err.Error(), authorityValiditySuperseded) {
		t.Fatalf("current benchmark lookup accepted the legacy 97acc credential after repoint: %v", err)
	}
}

func TestNonGitMercSourceCommitIsRejected(t *testing.T) {
	err := validateMercSourceCommit("working-tree-before-media-authority")
	if err == nil {
		t.Fatal("free-string merc_source_commit was accepted")
	}
	if !strings.Contains(err.Error(), "not a git object") {
		t.Fatalf("unexpected error: %v", err)
	}
	// Empty is also a binding failure.
	if err := validateMercSourceCommit(""); err == nil {
		t.Fatal("empty merc_source_commit was accepted")
	}
	// A real object in this repo must pass.
	head, err := gitBytes("..", "rev-parse", "HEAD")
	if err != nil {
		// control/ tests run with cwd=control; try .
		head, err = gitBytes(".", "rev-parse", "HEAD")
	}
	mustf(t, err, "resolve HEAD: %v")
	mustf(t, validateMercSourceCommit(strings.TrimSpace(string(head))), "real HEAD refused: %v")
}

// Automatic demotion: invalidate an authority and the dependent cell leaves the
// routable set without any lifecycle field being edited.
func TestInvalidatingAuthorityDemotesDependentCell(t *testing.T) {
	// G070: the live cell cites r5. Invalidate that authority path so demotion
	// targets the credential currently authorizing the cell, not historical r4.
	const path = "evidence/perf/runtime-benchmarks/candle-metal-llama1-q4-r5.json"
	const cellID = "candle-metal-llama1-infer"

	profile, ok := runtimeProfileByID("candle_metal")
	if !ok {
		t.Fatal("candle_metal missing")
	}
	// documentActivation may overlay policy; use the embedded profile for a pure
	// authority check.
	var embedded authorityRuntimeProfile
	for _, p := range runtimeAuthority.Runtimes {
		if p.RuntimeID == "candle_metal" {
			embedded = p
			break
		}
	}
	var cell authorityCell
	for _, c := range embedded.Cells {
		if c.ID == cellID {
			cell = c
			break
		}
	}
	previous := cloneBenchmarkReceiptSummary(benchmarkAuthorityManifest[path])
	t.Cleanup(func() { benchmarkAuthorityManifest[path] = previous })
	synthetic := cloneBenchmarkReceiptSummary(previous)
	synthetic.Validity = authorityValidityValid
	synthetic.EngineBuildHash = testOnlyEngineBuildHash
	synthetic.EngineBuildIdentityPolicy = currentEngineBuildIdentityPolicy
	synthetic.HardwareIdentity = testOnlyHardwareIdentity
	benchmarkAuthorityManifest[path] = synthetic
	if !cell.Routable(embedded) {
		t.Fatal("TEST_ONLY validity restoration did not make the otherwise exact authority routable")
	}

	must(t, InvalidateBenchmarkAuthority(path, authorityValidityWithdrawn))
	if cell.Routable(embedded) {
		t.Fatal("cell stayed routable after its authority was WITHDRAWN")
	}
	ok, reason := cellAuthorityBindable(embedded, cell)
	if ok || !strings.Contains(reason, authorityValidityWithdrawn) {
		t.Fatalf("bindable after withdrawal: ok=%v reason=%q", ok, reason)
	}
	// INVALIDATED and SUPERSEDED have the same force.
	for _, v := range []string{
		authorityValidityInvalidated,
		"INVALIDATED_PENDING_RERUN",
		authorityValiditySuperseded,
	} {
		must(t, InvalidateBenchmarkAuthority(path, v))
		if cell.Routable(embedded) {
			t.Fatalf("cell stayed routable under validity %s", v)
		}
	}
	// Restoring TEST_ONLY validity restores routability — lifecycle never moved.
	RestoreBenchmarkAuthorityValidity(path, authorityValidityValid)
	if !cell.Routable(embedded) {
		t.Fatal("restoring authority validity did not restore routability")
	}
	_ = profile // silence if unused under policy overlay
}

func TestBenchmarkManifestIdentityMatchesTheReceipts(t *testing.T) {
	for path, summary := range benchmarkAuthorityManifest {
		raw, err := os.ReadFile("../" + path)
		if err != nil {
			t.Errorf("manifest names %s, unreadable: %v", path, err)
			continue
		}
		var receipt map[string]any
		if err := json.Unmarshal(raw, &receipt); err != nil {
			t.Errorf("%s is not JSON: %v", path, err)
			continue
		}
		commit, _ := receipt["merc_source_commit"].(string)
		if strings.TrimSpace(commit) != strings.TrimSpace(summary.MercSourceCommit) {
			t.Errorf("%s: manifest merc_source_commit %q, receipt %q",
				path, summary.MercSourceCommit, commit)
		}
		// Hex-shaped commits must resolve in this repo. Free strings are the
		// demotion case and are expected to fail validateMercSourceCommit.
		if summary.MercSourceCommit != "" && hexObjectName.MatchString(summary.MercSourceCommit) {
			if err := validateMercSourceCommit(summary.MercSourceCommit); err != nil {
				t.Errorf("%s: hex-shaped commit failed git check: %v", path, err)
			}
		}
		rev, _ := receipt["profile_revision"].(string)
		if rev == "" {
			rev, _ = receipt["runtime_revision"].(string)
		}
		if strings.TrimSpace(rev) != strings.TrimSpace(summary.ProfileRevision) {
			t.Errorf("%s: manifest profile_revision %q, receipt %q",
				path, summary.ProfileRevision, rev)
		}
		harness, _ := receipt["harness"].(string)
		if strings.TrimSpace(harness) != strings.TrimSpace(summary.Harness) {
			t.Errorf("%s: manifest harness %q, receipt %q",
				path, summary.Harness, harness)
		}
		// When the manifest records binding_status, the on-disk receipt must match.
		// Historical UNBOUND receipts often carry binding_status on disk without a
		// mirrored field in the manifest; missing is treated as not BOUND by the
		// routability predicate, so that is fine.
		if bs := strings.TrimSpace(summary.BindingStatus); bs != "" {
			status, _ := receipt["binding_status"].(string)
			if !strings.EqualFold(strings.TrimSpace(status), bs) {
				t.Errorf("%s: manifest binding_status %q, receipt %q",
					path, summary.BindingStatus, status)
			}
		}
		if err := benchmarkManifestReceiptIdentityMismatch(summary, receipt); err != nil {
			t.Errorf("%s: %v", path, err)
		}
		if strings.EqualFold(summary.BindingStatus, BindingBound) {
			receiptArtifacts := receiptModelArtifactSHA256s(receipt)
			for _, profile := range runtimeAuthority.Runtimes {
				for _, cell := range profile.Cells {
					if cell.benchmarkAuthorityFor(profile) != path {
						continue
					}
					pins, err := exactWeightDigestsForCell(cell, runtimeAuthorityModels)
					if err != nil {
						t.Errorf("%s/%s exact artifact pins: %v", profile.RuntimeID, cell.ID, err)
						continue
					}
					for _, pin := range pins {
						if !slices.Contains(summary.ModelArtifactSHA256s, pin) ||
							!slices.Contains(receiptArtifacts, pin) {
							t.Errorf("%s/%s exact artifact pin %s is not sealed in both manifest %v and receipt %v",
								profile.RuntimeID, cell.ID, pin, summary.ModelArtifactSHA256s, receiptArtifacts)
						}
					}
				}
			}
		}
	}
}

func benchmarkManifestReceiptIdentityMismatch(
	summary benchmarkReceiptSummary,
	receipt map[string]any,
) error {
	if got := pricingReceiptEngineBuildHash(receipt); summary.EngineBuildHash != "" && got != summary.EngineBuildHash {
		return fmt.Errorf("manifest engine_build_hash %q, receipt %q", summary.EngineBuildHash, got)
	}
	if got := pricingReceiptEngineBuildIdentityPolicy(receipt); summary.EngineBuildIdentityPolicy != "" &&
		got != summary.EngineBuildIdentityPolicy {
		return fmt.Errorf("manifest engine_build_identity_policy %q, receipt %q",
			summary.EngineBuildIdentityPolicy, got)
	}
	if got := pricingReceiptHardwareIdentity(receipt); summary.HardwareIdentity != "" && got != summary.HardwareIdentity {
		return fmt.Errorf("manifest hardware_identity %q, receipt %q", summary.HardwareIdentity, got)
	}
	if len(summary.ModelArtifactSHA256s) > 0 {
		got := receiptModelArtifactSHA256s(receipt)
		want := append([]string(nil), summary.ModelArtifactSHA256s...)
		sort.Strings(want)
		for _, digest := range got {
			if !slices.Contains(want, digest) {
				return fmt.Errorf("manifest model artifact set %v omits receipt artifact %s", want, digest)
			}
		}
	}
	return nil
}

func receiptModelArtifactSHA256s(receipt map[string]any) []string {
	set := map[string]bool{}
	if value, _ := receipt["model_artifact_sha256"].(string); digestPattern.MatchString(value) {
		set[value] = true
	}
	if values, ok := receipt["model_artifact_sha256s"].([]any); ok {
		for _, raw := range values {
			if value, ok := raw.(string); ok && digestPattern.MatchString(value) {
				set[value] = true
			}
		}
	}
	var walk func(any)
	walk = func(value any) {
		switch value := value.(type) {
		case map[string]any:
			for key, child := range value {
				if key == "sha256" {
					if digest, ok := child.(string); ok && digestPattern.MatchString(digest) {
						set[digest] = true
					}
				}
				walk(child)
			}
		case []any:
			for _, child := range value {
				walk(child)
			}
		}
	}
	if artifacts, ok := receipt["model_artifacts"]; ok {
		walk(artifacts)
	}
	if len(set) == 0 {
		if producer, ok := receipt["producer_identity"].(map[string]any); ok {
			if slot, ok := producer["model_artifact_digest"].(map[string]any); ok {
				if value, _ := slot["value"].(string); digestPattern.MatchString(value) {
					set[value] = true
				}
			}
		}
	}
	out := make([]string, 0, len(set))
	for digest := range set {
		out = append(out, digest)
	}
	sort.Strings(out)
	return out
}

func TestBenchmarkManifestExactExecutionIdentityCannotDivergeFromReceipt(t *testing.T) {
	const path = "evidence/perf/runtime-benchmarks/candle-metal-llama1-q4-r4.json"
	raw, err := os.ReadFile("../" + path)
	must(t, err)
	var receipt map[string]any
	must(t, json.Unmarshal(raw, &receipt))
	base := benchmarkAuthorityManifest[path]
	for _, mutate := range []func(*benchmarkReceiptSummary){
		func(summary *benchmarkReceiptSummary) { summary.EngineBuildHash = "0000000000000000" },
		func(summary *benchmarkReceiptSummary) {
			summary.EngineBuildIdentityPolicy = currentEngineBuildIdentityPolicy
		},
		func(summary *benchmarkReceiptSummary) { summary.HardwareIdentity = "Apple M1 Ultra" },
		func(summary *benchmarkReceiptSummary) {
			summary.ModelArtifactSHA256s = []string{strings.Repeat("a", 64)}
		},
	} {
		mutant := cloneBenchmarkReceiptSummary(base)
		mutate(&mutant)
		if err := benchmarkManifestReceiptIdentityMismatch(mutant, receipt); err == nil {
			t.Fatal("mutated embedded benchmark identity still matched receipt bytes")
		}
	}
}

func TestAdvertisedSurfaceIsTheBindableSet(t *testing.T) {
	// G070: exactly candle-metal-llama1-infer is current-bindable under r5
	// settlement geometry. Prior r4 remains SUPERSEDED history; embed/media
	// stay unadvertised. Reject any extra cell.
	want := map[string]string{
		"candle-metal-llama1-infer": "batch_infer",
	}
	caps := advertisedRuntimeCapabilities()
	if len(caps) != len(want) {
		ids := make([]string, 0, len(caps))
		for _, cap := range caps {
			ids = append(ids, cap.ID+"/"+cap.Job)
		}
		t.Fatalf("advertised %v, want exactly %v", ids, want)
	}
	for _, cap := range caps {
		job, ok := want[cap.ID]
		if !ok {
			t.Errorf("unexpected advertised cell %s/%s", cap.ID, cap.Job)
			continue
		}
		if cap.Job != job {
			t.Errorf("cell %s job=%s, want %s", cap.ID, cap.Job, job)
		}
	}
	for id := range want {
		if !advertisedRuntimeCell(id) {
			t.Errorf("BOUND cell %s missing from advertised surface", id)
		}
	}
}

// BOUND alone is not current authority: an older receipt without build/device
// identity remains historical-only. A synthetic in-memory completion proves
// the separate BOUND predicate without relabelling the receipt on disk.
func TestBoundAuthorityIsRequiredForRoutability(t *testing.T) {
	const path = "evidence/perf/runtime-benchmarks/embed-cell-candle-vs-llama-cpp-r2.json"
	summary, ok := benchmarkAuthorityManifest[path]
	if !ok {
		t.Fatalf("manifest missing %s", path)
	}
	if !strings.EqualFold(summary.BindingStatus, BindingBound) {
		t.Fatalf("embed r2 binding_status=%q, want BOUND", summary.BindingStatus)
	}
	profile, ok := runtimeProfileByID("candle_metal")
	if !ok {
		t.Fatal("candle_metal missing")
	}
	var embedded authorityRuntimeProfile
	for _, p := range runtimeAuthority.Runtimes {
		if p.RuntimeID == "candle_metal" {
			embedded = p
			break
		}
	}
	var embedCell authorityCell
	for _, c := range embedded.Cells {
		if c.ID == "candle-metal-minilm-embed" {
			embedCell = c
			break
		}
	}
	if embedCell.Routable(embedded) {
		t.Fatal("embed cell with missing exact build/device identity became current-routable")
	}
	if ok, reason := cellAuthorityBindable(embedded, embedCell); ok ||
		!strings.Contains(reason, "engine_build_hash") {
		t.Fatalf("missing-build refusal: ok=%v reason=%q", ok, reason)
	}
	// Strip BOUND and the cell must leave ordinary routing without any lifecycle edit.
	saved := benchmarkAuthorityManifest[path]
	t.Cleanup(func() { benchmarkAuthorityManifest[path] = saved })
	stripped := saved
	stripped.EngineBuildHash = testOnlyEngineBuildHash
	stripped.EngineBuildIdentityPolicy = currentEngineBuildIdentityPolicy
	stripped.HardwareIdentity = testOnlyHardwareIdentity
	benchmarkAuthorityManifest[path] = stripped
	if !embedCell.Routable(embedded) {
		ok, reason := cellAuthorityBindable(embedded, embedCell)
		t.Fatalf("synthetic exact build/device did not reach BOUND predicate: ok=%v reason=%q", ok, reason)
	}
	stripped.BindingStatus = BindingUnbound
	benchmarkAuthorityManifest[path] = stripped
	if embedCell.Routable(embedded) {
		t.Fatal("cell stayed routable after its authority was reduced to UNBOUND")
	}
	ok, reason := cellAuthorityBindable(embedded, embedCell)
	if ok || !strings.Contains(reason, "not BOUND") {
		t.Fatalf("expected not-BOUND refusal, got ok=%v reason=%q", ok, reason)
	}
	_ = profile
}

func TestCellAuthorityRequiresTheExactSelectedWireKindWeights(t *testing.T) {
	const path = "evidence/perf/runtime-benchmarks/embed-cell-candle-vs-llama-cpp-r2.json"

	find := func(runtimeID, cellID string) (authorityRuntimeProfile, authorityCell) {
		t.Helper()
		for _, profile := range runtimeAuthority.Runtimes {
			if profile.RuntimeID != runtimeID {
				continue
			}
			for _, cell := range profile.Cells {
				if cell.ID == cellID {
					return profile, cell
				}
			}
		}
		t.Fatalf("runtime authority has no cell %s/%s", runtimeID, cellID)
		return authorityRuntimeProfile{}, authorityCell{}
	}
	candleProfile, candleCell := find("candle_metal", "candle-metal-minilm-embed")
	llamaProfile, llamaCell := find("llama_cpp_metal", "llama-cpp-metal-minilm-embed")
	candlePins, err := exactWeightDigestsForCell(candleCell, runtimeAuthorityModels)
	must(t, err)
	llamaPins, err := exactWeightDigestsForCell(llamaCell, runtimeAuthorityModels)
	must(t, err)
	if len(candlePins) != 1 || len(llamaPins) != 1 || candlePins[0] == llamaPins[0] {
		t.Fatalf("MiniLM exact-artifact premise changed: candle=%v llama.cpp=%v",
			candlePins, llamaPins)
	}

	saved := benchmarkAuthorityManifest[path]
	t.Cleanup(func() { benchmarkAuthorityManifest[path] = saved })
	for _, tc := range []struct {
		name    string
		profile authorityRuntimeProfile
		cell    authorityCell
		cited   []string
		wantOK  bool
	}{
		{
			name:    "Candle accepts its safetensors",
			profile: candleProfile, cell: candleCell, cited: candlePins, wantOK: true,
		},
		{
			name:    "Candle refuses sibling canonical GGUF",
			profile: candleProfile, cell: candleCell, cited: llamaPins, wantOK: false,
		},
		{
			name:    "llama.cpp accepts its GGUF",
			profile: llamaProfile, cell: llamaCell, cited: llamaPins, wantOK: true,
		},
		{
			name:    "llama.cpp refuses sibling canonical safetensors",
			profile: llamaProfile, cell: llamaCell, cited: candlePins, wantOK: false,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			summary := cloneBenchmarkReceiptSummary(saved)
			summary.ModelArtifactSHA256s = append([]string(nil), tc.cited...)
			summary.EngineBuildHash = testOnlyEngineBuildHash
			summary.EngineBuildIdentityPolicy = requiredEngineBuildIdentityPolicy(tc.profile, tc.cell)
			summary.HardwareIdentity = testOnlyHardwareIdentity
			benchmarkAuthorityManifest[path] = summary
			ok, reason := cellAuthorityBindable(tc.profile, tc.cell)
			if ok != tc.wantOK {
				t.Fatalf("bindable=%v reason=%q, want %v", ok, reason, tc.wantOK)
			}
			if !tc.wantOK && (!strings.Contains(reason, "sibling format") ||
				!strings.Contains(reason, tc.cell.ID)) {
				t.Fatalf("wrong-wire refusal did not identify the exact cell boundary: %q", reason)
			}
		})
	}
}

func TestExactCellWeightSetRequiresEverySelectedShard(t *testing.T) {
	const (
		shardOne = "1111111111111111111111111111111111111111111111111111111111111111"
		shardTwo = "2222222222222222222222222222222222222222222222222222222222222222"
		sibling  = "3333333333333333333333333333333333333333333333333333333333333333"
	)
	cell := authorityCell{ID: "test-sharded-hf", Model: "test-sharded", WireKind: "hf"}
	models := map[string]authorityModel{
		"test-sharded": {
			ID: "test-sharded", WireKind: "hf",
			Artifacts: []authorityArtifact{
				{Path: "model-00001-of-00002.safetensors", SHA256: shardOne},
				{Path: "model-00002-of-00002.safetensors", SHA256: shardTwo},
				{Path: "sibling.gguf", SHA256: sibling, WireKind: "gguf"},
			},
		},
	}
	pins, err := exactWeightDigestsForCell(cell, models)
	must(t, err)
	if got, want := strings.Join(pins, ","), shardOne+","+shardTwo; got != want {
		t.Fatalf("exact selected shard set=%s, want %s", got, want)
	}
	if missing := missingArtifactDigests(pins, []string{shardOne, sibling}); len(missing) != 1 || missing[0] != shardTwo {
		t.Fatalf("one selected shard plus a sibling format produced missing=%v", missing)
	}
}
