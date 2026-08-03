package main

import (
	"encoding/json"
	"os"
	"strings"
	"testing"
)

// Scoreboard before the bindable raise: four lifecycle-routable candle cells,
// one of which actually bound at the merc_source_commit bar. After the BOUND
// raise: only the cell whose authority is genuinely BOUND remains advertised.

func TestOnlyBindableAuthorityCellsAreRoutable(t *testing.T) {
	// Reasons the audit named. Each demotion is a predicate result, not a
	// hand-edited lifecycle field. The three still fail at the pre-BOUND checks;
	// embed is the sole cell that reaches the BOUND bar.
	wantDemoted := map[string]string{
		"candle-metal-ffmpeg-transcode": "not a git object",
		"candle-metal-scene-render":     "merc_source_commit is missing",
		"candle-metal-llama1-infer":     "profile_revision",
	}
	// Embed is the sole BOUND ordinary cell at this commit.
	const wantRoutable = "candle-metal-minilm-embed"

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
				continue
			}
			// Lifecycle says CANARY/ACTIVE but Routable is false — must be one of
			// the three demotions the audit named, with a matching reason.
			if substr, named := wantDemoted[cell.ID]; named {
				if ok {
					t.Errorf("%s should not bind; got ok with reason %q", cell.ID, reason)
				}
				if !strings.Contains(reason, substr) {
					// llama1 also fails digests; accept either refusal the audit named.
					if cell.ID == "candle-metal-llama1-infer" &&
						(strings.Contains(reason, "model artifact digest") ||
							strings.Contains(reason, "profile_revision")) {
						continue
					}
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
	if len(got) != 1 || got[0] != wantRoutable {
		t.Fatalf("routable cells = %v, want exactly [%s]", got, wantRoutable)
	}
	if n := len(advertisedRuntimeCapabilities()); n != 1 {
		t.Fatalf("advertised projection has %d cells, want 1", n)
	}
	if !advertisedRuntimeCell(wantRoutable) {
		t.Fatal("the BOUND embed cell is not advertised")
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
	if err != nil {
		t.Fatalf("resolve HEAD: %v", err)
	}
	if err := validateMercSourceCommit(strings.TrimSpace(string(head))); err != nil {
		t.Fatalf("real HEAD refused: %v", err)
	}
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

	if err := InvalidateBenchmarkAuthority(path, authorityValidityWithdrawn); err != nil {
		t.Fatal(err)
	}
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
		if err := InvalidateBenchmarkAuthority(path, v); err != nil {
			t.Fatal(err)
		}
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

func TestAdvertisedSurfaceIsTheBindableSingleton(t *testing.T) {
	// Ordinary admission freezes one advertised cell per (job, model). Today the
	// whole advertised surface is a single BOUND embed cell.
	caps := advertisedRuntimeCapabilities()
	if len(caps) != 1 {
		t.Fatalf("advertised %d cells, want 1 BOUND cell", len(caps))
	}
	if caps[0].ID != "candle-metal-minilm-embed" || caps[0].Job != "embed" {
		t.Fatalf("advertised %+v, want candle-metal-minilm-embed/embed", caps[0])
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
