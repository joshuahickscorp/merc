package main

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// The rule the gate exists for: a receipt with a skipped or failed step
// authorizes nothing.
//
// The failure this replaces was a shell pipeline that chained `git commit` after
// a test run with `&&` in a way that did not gate on the result. Recording a skip
// and then treating the receipt as complete would be the same mistake with better
// paperwork — it would look like evidence.
func TestOnlyACompleteCheckpointReceiptAuthorizesAPush(t *testing.T) {
	complete := DevCheckpointReceipt{
		Head: strings.Repeat("a", 40), MutationRestored: true,
		Steps: []devCheckpointStep{{Name: "full-ci"}},
	}
	if !devCheckpointIsPushEligible(complete) {
		t.Fatal("a complete receipt did not authorize a push")
	}

	for name, mutate := range map[string]func(*DevCheckpointReceipt){
		"a skipped step": func(r *DevCheckpointReceipt) {
			r.Steps = append(r.Steps, devCheckpointStep{Name: "mutation-suite", Skipped: true})
		},
		"a failed step": func(r *DevCheckpointReceipt) {
			r.Steps = append(r.Steps, devCheckpointStep{Name: "full-ci", ExitCode: 1})
		},
		"an unrestored tree": func(r *DevCheckpointReceipt) { r.MutationRestored = false },
		"no steps at all":    func(r *DevCheckpointReceipt) { r.Steps = nil },
		"no commit":          func(r *DevCheckpointReceipt) { r.Head = "" },
	} {
		t.Run(name, func(t *testing.T) {
			receipt := complete
			receipt.Steps = append([]devCheckpointStep(nil), complete.Steps...)
			mutate(&receipt)
			if devCheckpointIsPushEligible(receipt) {
				t.Fatalf("%s still authorized a push", name)
			}
		})
	}
}

// The restoration proof has to read file CONTENT.
//
// `git status --porcelain` is the usual check, and it would have caught this
// case too — but a mutation runner that rewrites a file and restores it to
// DIFFERENT bytes of the same length leaves HEAD where it was and can leave the
// index looking clean. Hashing the bytes answers the question that is actually
// being asked: is this the source that was tested.
func TestWorktreeDigestNoticesAContentChange(t *testing.T) {
	root := t.TempDir()
	for _, argv := range [][]string{
		{"init", "-q"},
		{"config", "user.email", "checkpoint@test"},
		{"config", "user.name", "checkpoint"},
	} {
		cmd := exec.Command("git", argv...)
		cmd.Dir = root
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Skipf("git is unavailable in this environment: %v (%s)", err, out)
		}
	}
	path := filepath.Join(root, "source.go")
	if err := os.WriteFile(path, []byte("package main // original\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	for _, argv := range [][]string{{"add", "."}, {"commit", "-qm", "base"}} {
		cmd := exec.Command("git", argv...)
		cmd.Dir = root
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v (%s)", argv, err, out)
		}
	}

	before, err := worktreeContentDigest(root)
	if err != nil {
		t.Fatal(err)
	}
	// The same byte count, so anything comparing sizes would miss it.
	if err := os.WriteFile(path, []byte("package main // MUTATED!\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	mutated, err := worktreeContentDigest(root)
	if err != nil {
		t.Fatal(err)
	}
	if mutated == before {
		t.Fatal("a same-length content mutation did not move the worktree digest")
	}
	if err := os.WriteFile(path, []byte("package main // original\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	restored, err := worktreeContentDigest(root)
	if err != nil {
		t.Fatal(err)
	}
	if restored != before {
		t.Fatal("a correctly restored tree did not reproduce its digest")
	}

	// A tracked file deleted rather than modified is also a difference.
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	deleted, err := worktreeContentDigest(root)
	if err != nil {
		t.Fatalf("digest failed on a missing tracked file instead of recording it: %v", err)
	}
	if deleted == before {
		t.Fatal("deleting a tracked file did not move the worktree digest")
	}
}

// The frozen release branch must not be checkpointed: the mutation suite writes
// to the tree, and that tree is frozen.
func TestCheckpointRefusesTheFrozenReleaseBranch(t *testing.T) {
	if !devCheckpointFrozenBranches["release/rc1-go-closure"] {
		t.Fatal("release/rc1-go-closure is no longer listed as frozen")
	}
	if _, err := performDevCheckpoint(devCheckpointOptions{root: t.TempDir()}); err == nil {
		t.Fatal("a checkpoint ran outside a git repository")
	}
}

// The receipt is JSON on disk that a hook reads; keep it decodable by anything
// that is not this binary.
func TestCheckpointReceiptRoundTrips(t *testing.T) {
	receipt := DevCheckpointReceipt{
		SchemaVersion: devCheckpointSchemaVersion, Kind: "dev_checkpoint_receipt",
		Head: strings.Repeat("b", 40), Branch: "perf/execution-frontier",
		WorktreeDigest: strings.Repeat("c", 64), MutationRestored: true,
		CapabilityMatrixSHA256: generatedRuntimeMatrixSHA256,
		Steps:                  []devCheckpointStep{{Name: "full-ci", Command: "make ci", DurationMS: 1}},
	}
	blob, err := json.Marshal(receipt)
	if err != nil {
		t.Fatal(err)
	}
	var back DevCheckpointReceipt
	if err := json.Unmarshal(blob, &back); err != nil {
		t.Fatal(err)
	}
	if back.Head != receipt.Head || !back.MutationRestored ||
		len(back.Steps) != 1 || back.CapabilityMatrixSHA256 != generatedRuntimeMatrixSHA256 {
		t.Fatalf("receipt did not round-trip: %+v", back)
	}
}
