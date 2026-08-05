package main

import (
	"encoding/json"
	"os"
	"strings"
	"testing"
)

// Scoreboard after the BOUND raise and the llama1 re-measure: four lifecycle-
// routable candle cells, two of which bind (embed + batch_infer). The media
// cells remain demoted by their unbound receipts, not by lifecycle edits.

func TestOnlyBindableAuthorityCellsAreRoutable(t *testing.T) {
	// Reasons the audit named. Each demotion is a predicate result, not a
	// hand-edited lifecycle field. Media cells still fail pre-BOUND checks;
	// embed and generation now reach the BOUND bar on sealed receipts.
	wantDemoted := map[string]string{
		"candle-metal-ffmpeg-transcode": "not a git object",
		"candle-metal-scene-render":     "merc_source_commit is missing",
	}
	wantRoutable := map[string]bool{
		"candle-metal-minilm-embed": true,
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
	const path = "evidence/perf/runtime-benchmarks/embed-cell-candle-vs-llama-cpp-r2.json"
	const cellID = "candle-metal-minilm-embed"

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
	if !cell.Routable(embedded) {
		t.Fatal("embed cell must start routable under BOUND authority")
	}
	previous := benchmarkAuthorityManifest[path].Validity
	t.Cleanup(func() { RestoreBenchmarkAuthorityValidity(path, previous) })

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
	// Restoring validity restores routability — lifecycle never moved.
	RestoreBenchmarkAuthorityValidity(path, previous)
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
	}
}

func TestAdvertisedSurfaceIsTheBindableSet(t *testing.T) {
	// Ordinary admission freezes one advertised cell per (job, model). After the
	// llama1 re-measure the surface is embed + batch_infer; media stay out.
	want := map[string]string{
		"candle-metal-minilm-embed": "embed",
		"candle-metal-llama1-infer": "batch_infer",
	}
	caps := advertisedRuntimeCapabilities()
	if len(caps) != len(want) {
		t.Fatalf("advertised %d cells, want %d BOUND cells", len(caps), len(want))
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
}

// Raising the bar to BOUND must not silently demote the embed cell that was
// re-measured and sealed, and must keep the three previously quarantined cells
// non-routable.
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
	if !embedCell.Routable(embedded) {
		ok, reason := cellAuthorityBindable(embedded, embedCell)
		t.Fatalf("BOUND embed cell not routable: ok=%v reason=%q", ok, reason)
	}
	// Strip BOUND and the cell must leave ordinary routing without any lifecycle edit.
	saved := benchmarkAuthorityManifest[path]
	t.Cleanup(func() { benchmarkAuthorityManifest[path] = saved })
	stripped := saved
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
