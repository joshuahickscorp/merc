package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func testRepoRoot(t *testing.T) string {
	t.Helper()
	// control/ tests run with cwd = control/; repo root is parent.
	root := ".."
	if _, err := os.Stat(filepath.Join(root, ".git")); err != nil {
		// Worktrees use .git as a file; also accept that.
		if _, err2 := os.Stat(filepath.Join(root, "control")); err2 != nil {
			t.Fatalf("cannot locate repo root from cwd: %v", err)
		}
	}
	return root
}

func completeIdentity(t *testing.T, root string) (ReceiptIdentity, string) {
	t.Helper()
	// Hash a stable file as stand-in "binary" for unit tests.
	bin := filepath.Join(root, "control", "receipt_identity.go")
	sum, err := sha256FileHex(bin)
	must(t, err)
	commit, err := ResolveRepoSourceCommit(root)
	must(t, err)
	id := ReceiptIdentity{
		SourceCommit:        IdentitySlotValue(commit),
		BuildDigest:         IdentitySlotValue(sum),
		ModelArtifactDigest: IdentitySlotNA("unit test: no model"),
		ImageDigest:         IdentitySlotNA("unit test: no image"),
		HarnessRevision:     IdentitySlotValue("receipt_identity_test"),
		CorpusDigest:        IdentitySlotNA("unit test: no corpus"),
		ExactConfig:         IdentitySlotValue("embedded"),
		RawSamples:          IdentitySlotValue("embedded"),
	}
	return id, bin
}

func TestIdentitySlotComplete(t *testing.T) {
	if (IdentitySlot{}).Complete() {
		t.Fatal("empty slot must be incomplete")
	}
	if !IdentitySlotValue("x").Complete() {
		t.Fatal("value slot must be complete")
	}
	if !IdentitySlotNA("because").Complete() {
		t.Fatal("na slot must be complete")
	}
	both := IdentitySlot{Value: "x", NA: "y"}
	if both.Complete() {
		t.Fatal("value+na must be incomplete")
	}
}

func TestValidateReceiptIdentityRejectsFreeStringCommit(t *testing.T) {
	root := testRepoRoot(t)
	id, bin := completeIdentity(t, root)
	id.SourceCommit = IdentitySlotValue("working-tree-before-media-authority")
	err := ValidateReceiptIdentity(root, id, bin)
	if err == nil {
		t.Fatal("expected free-string commit to be refused")
	}
	if !strings.Contains(err.Error(), "not a git object") {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestValidateReceiptIdentityRejectsEmptySlots(t *testing.T) {
	root := testRepoRoot(t)
	id, bin := completeIdentity(t, root)
	id.ImageDigest = IdentitySlot{}
	err := ValidateReceiptIdentity(root, id, bin)
	if err == nil {
		t.Fatal("expected empty image_digest to be refused")
	}
	if !strings.Contains(err.Error(), "image_digest") {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestValidateReceiptIdentityRejectsMismatchedBuildDigest(t *testing.T) {
	root := testRepoRoot(t)
	id, bin := completeIdentity(t, root)
	id.BuildDigest = IdentitySlotValue(strings.Repeat("ab", 32))
	err := ValidateReceiptIdentity(root, id, bin)
	if err == nil {
		t.Fatal("expected mismatched build_digest to be refused")
	}
	if !strings.Contains(err.Error(), "does not match") {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestWriteBoundEvidenceJSONRoundTrip(t *testing.T) {
	root := testRepoRoot(t)
	id, bin := completeIdentity(t, root)
	dir := t.TempDir()
	path := filepath.Join(dir, "sample.json")
	err := WriteBoundEvidenceJSON(EvidenceWriteRequest{
		RepoRoot:        root,
		Path:            path,
		Payload:         map[string]any{"kind": "unit_test", "schema_version": 1},
		Identity:        id,
		BuildBinaryPath: bin,
	})
	must(t, err)
	raw, err := os.ReadFile(path)
	must(t, err)
	var got map[string]any
	must(t, json.Unmarshal(raw, &got))
	if got["binding_status"] != BindingBound {
		t.Fatalf("binding_status=%v", got["binding_status"])
	}
	if got["kind"] != "unit_test" {
		t.Fatalf("payload lost: %v", got["kind"])
	}
	pi, ok := got["producer_identity"].(map[string]any)
	if !ok {
		t.Fatalf("producer_identity missing: %T", got["producer_identity"])
	}
	sc, _ := pi["source_commit"].(map[string]any)
	if sc["value"] == nil || sc["value"] == "" {
		t.Fatalf("source_commit value missing: %v", sc)
	}
}

func TestWriteBoundEvidenceJSONStickyWithdrawal(t *testing.T) {
	root := testRepoRoot(t)
	id, bin := completeIdentity(t, root)
	dir := t.TempDir()
	path := filepath.Join(dir, "withdrawn.json")

	// Seed a withdrawn artifact (bypass writer — historical shape).
	seed := map[string]any{
		"kind":           "gateway_parity",
		"validity":       "INVALIDATED_PENDING_RERUN",
		"binding_status": BindingWithdrawn,
	}
	raw, _ := json.MarshalIndent(seed, "", "  ")
	must(t, os.WriteFile(path, append(raw, '\n'), 0o644))

	// Non-withdrawn overwrite without authority must fail.
	err := WriteBoundEvidenceJSON(EvidenceWriteRequest{
		RepoRoot:        root,
		Path:            path,
		Payload:         map[string]any{"kind": "gateway_parity", "gate_passed": true},
		Identity:        id,
		BuildBinaryPath: bin,
	})
	if err == nil {
		t.Fatal("expected sticky withdrawal refusal")
	}
	if !strings.Contains(err.Error(), "withdrawn") {
		t.Fatalf("unexpected error: %v", err)
	}

	// With authority id, overwrite is allowed.
	err = WriteBoundEvidenceJSON(EvidenceWriteRequest{
		RepoRoot:        root,
		Path:            path,
		Payload:         map[string]any{"kind": "gateway_parity", "gate_passed": true},
		Identity:        id,
		BuildBinaryPath: bin,
		AuthorityID:     "test-authority-rerun-1",
	})
	must(t, err)
	got, err := readJSONObjectFile(path)
	must(t, err)
	if got["binding_status"] != BindingBound {
		t.Fatalf("binding_status=%v", got["binding_status"])
	}
	if got["supersession_authority_id"] != "test-authority-rerun-1" {
		t.Fatalf("authority not recorded: %v", got["supersession_authority_id"])
	}
}

func TestWriteBoundEvidenceJSONRefusesIncomplete(t *testing.T) {
	root := testRepoRoot(t)
	id, bin := completeIdentity(t, root)
	id.CorpusDigest = IdentitySlot{}
	dir := t.TempDir()
	err := WriteBoundEvidenceJSON(EvidenceWriteRequest{
		RepoRoot:        root,
		Path:            filepath.Join(dir, "x.json"),
		Payload:         map[string]any{"kind": "x"},
		Identity:        id,
		BuildBinaryPath: bin,
	})
	if err == nil {
		t.Fatal("expected refusal")
	}
	// File must not exist.
	if _, statErr := os.Stat(filepath.Join(dir, "x.json")); !os.IsNotExist(statErr) {
		t.Fatal("incomplete write must not create the file")
	}
}
