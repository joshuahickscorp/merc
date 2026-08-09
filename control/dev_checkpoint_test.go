package main

import (
	"crypto/sha256"
	"encoding/hex"
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
	must(t, os.WriteFile(path, []byte("package main // original\n"), 0o644))
	for _, argv := range [][]string{{"add", "."}, {"commit", "-qm", "base"}} {
		cmd := exec.Command("git", argv...)
		cmd.Dir = root
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v (%s)", argv, err, out)
		}
	}

	before, err := worktreeContentDigest(root)
	must(t, err)
	// The same byte count, so anything comparing sizes would miss it.
	must(t, os.WriteFile(path, []byte("package main // MUTATED!\n"), 0o644))
	mutated, err := worktreeContentDigest(root)
	must(t, err)
	if mutated == before {
		t.Fatal("a same-length content mutation did not move the worktree digest")
	}
	must(t, os.WriteFile(path, []byte("package main // original\n"), 0o644))
	restored, err := worktreeContentDigest(root)
	must(t, err)
	if restored != before {
		t.Fatal("a correctly restored tree did not reproduce its digest")
	}

	// A tracked file deleted rather than modified is also a difference.
	must(t, os.Remove(path))
	deleted, err := worktreeContentDigest(root)
	mustf(t, err, "digest failed on a missing tracked file instead of recording it: %v")
	if deleted == before {
		t.Fatal("deleting a tracked file did not move the worktree digest")
	}
}

// The frozen release branch must not be checkpointed: a checkpoint binds a
// working candidate rather than a sealed release branch.
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
	must(t, err)
	var back DevCheckpointReceipt
	must(t, json.Unmarshal(blob, &back))
	if back.Head != receipt.Head || !back.MutationRestored ||
		len(back.Steps) != 1 || back.CapabilityMatrixSHA256 != generatedRuntimeMatrixSHA256 {
		t.Fatalf("receipt did not round-trip: %+v", back)
	}
}

// The lock the checkpoint reads must be the lock the mutation script writes.
//
// This exists because the rule "never run CI while mutation tooling modifies the
// same tree" was broken by hand: a checkpoint was killed mid-mutation, its
// mutation-test.sh survived, and it kept rewriting source files through a later
// checkpoint and into a `make ci` run that then failed on four tests exercising
// mutated code. The script already publishes a lock; the gate now refuses to
// start or to trust a restoration digest while it is held. If the two ever
// derive different paths, the guard silently stops guarding.
func TestCheckpointReadsTheMutationScriptsOwnLock(t *testing.T) {
	root, err := git(".", "rev-parse", "--show-toplevel")
	if err != nil {
		t.Skipf("not inside a git repository: %v", err)
	}
	script, err := os.ReadFile(filepath.Join(root, "scripts", "mutation-test.sh"))
	must(t, err)
	// The script derives it as:
	//   repo_lock_id="$(printf '%s' "$PWD" | shasum -a 256 | cut -c1-16)"
	//   MUTATION_LOCK="${TMPDIR:-/tmp}/merc-mutation-${repo_lock_id}.lock"
	for _, fragment := range []string{
		`printf '%s' "$PWD" | shasum -a 256 | cut -c1-16`,
		`merc-mutation-${repo_lock_id}.lock`,
	} {
		if !strings.Contains(string(script), fragment) {
			t.Fatalf("mutation-test.sh no longer derives its lock as %q; "+
				"mutationLockHeld reads a path nothing writes", fragment)
		}
	}

	sum := sha256.Sum256([]byte(root))
	want := filepath.Join(os.TempDir(),
		"merc-mutation-"+hex.EncodeToString(sum[:])[:16]+".lock")
	mustf(t, os.MkdirAll(want, 0o755), "could not create the lock the script would: %v")
	t.Cleanup(func() { _ = os.Remove(want) })

	got, held := mutationLockHeld(root)
	if !held {
		t.Fatalf("the checkpoint did not see a held lock at %s", want)
	}
	if got != want {
		t.Fatalf("checkpoint watches %s, script writes %s", got, want)
	}
	_ = os.Remove(want)
	if _, held := mutationLockHeld(root); held {
		t.Fatal("the checkpoint reports a lock that is gone")
	}
}

func TestCheckpointUsesTheIsolatedParallelMutationRunner(t *testing.T) {
	root, err := git(".", "rev-parse", "--show-toplevel")
	if err != nil {
		t.Skipf("not inside a git repository: %v", err)
	}
	checkpoint, err := os.ReadFile(filepath.Join(root, "control", "dev_checkpoint.go"))
	must(t, err)
	if !strings.Contains(string(checkpoint), `"bash", "scripts/mutation-test-parallel.sh"`) {
		t.Fatal("checkpoint no longer runs the isolated parallel mutation campaign")
	}
	parallel, err := os.ReadFile(filepath.Join(root, "scripts", "mutation-test-parallel.sh"))
	must(t, err)
	for _, fragment := range []string{
		`git worktree add --detach`,
		`MERC_MUTATION_CASE_IDS`,
		`MERC_MUTATION_TEST_STRATEGY=adaptive`,
		`MERC_MUTATION_WALLCLOCK_SECONDS`,
		`git -C "$worktree" lfs checkout`,
		`verify-lfs-corpus.py --root "$ROOT"`,
		`initdb --no-locale`,
		`pg_ctl -D "$cluster"`,
		`candidate tree changed while shards ran`,
	} {
		if !strings.Contains(string(parallel), fragment) {
			t.Fatalf("parallel mutation runner is missing required isolation guard %q", fragment)
		}
	}
}
