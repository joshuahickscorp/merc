package main

import (
	"encoding/json"
	"os"
	"strings"
	"testing"
)

// Scoreboard before this change: four lifecycle-routable candle cells, one of
// which actually bound. After: only the bindable one remains advertised.

func TestOnlyBindableAuthorityCellsAreRoutable(t *testing.T) {
	// Reasons the audit named. Each demotion is a predicate result, not a
	// hand-edited lifecycle field.
	wantDemoted := map[string]string{
		"candle-metal-ffmpeg-transcode": "not a git object",
		"candle-metal-scene-render":     "merc_source_commit is missing",
		"candle-metal-llama1-infer":     "profile_revision",
	}
	// Embed is the sole bindable ordinary cell at this commit.
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
		t.Fatal("the bindable embed cell is not advertised")
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
	const path = "evidence/perf/runtime-benchmarks/embed-cell-candle-vs-llama-cpp-r1.json"
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
		t.Fatal("embed cell must start routable under bindable authority")
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
	}
}

func TestAdvertisedSurfaceIsTheBindableSingleton(t *testing.T) {
	// Ordinary admission freezes one advertised cell per (job, model). Today the
	// whole advertised surface is a single bindable embed cell.
	caps := advertisedRuntimeCapabilities()
	if len(caps) != 1 {
		t.Fatalf("advertised %d cells, want 1 bindable cell", len(caps))
	}
	if caps[0].ID != "candle-metal-minilm-embed" || caps[0].Job != "embed" {
		t.Fatalf("advertised %+v, want candle-metal-minilm-embed/embed", caps[0])
	}
}
