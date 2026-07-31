package main

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// merc dev checkpoint: the gate that makes pushing a red tree impossible.
//
// This exists because a knowingly unverified checkpoint was pushed. The commit
// command was chained after a test run with `&&` in a way that did not gate on
// the result, the suite was red, and the push went out anyway. That is not a
// discipline problem to be solved by being more careful next time — a shell
// pipeline whose failure mode is "push anyway" will eventually be typed again.
//
// So the ordering is a program rather than a habit. Each step runs to completion
// before the next begins, mutation testing can never overlap CI on the same tree,
// the working tree is proved restored after the mutation run rather than assumed
// to be, and the receipt is bound to the exact commit that was tested. A pre-push
// hook that requires a matching receipt then makes "push an untested HEAD" fail
// closed instead of relying on anyone remembering.
//
// Remote CI stays authoritative. This is the local gate that stops a red tree
// reaching it.

const devCheckpointSchemaVersion = 1

// Branches this repository does not check point against. release/rc1-go-closure
// is frozen; running a mutation suite on it would modify a frozen tree.
var devCheckpointFrozenBranches = map[string]bool{
	"release/rc1-go-closure": true,
}

type devCheckpointStep struct {
	Name       string `json:"name"`
	Command    string `json:"command"`
	DurationMS int64  `json:"duration_ms"`
	ExitCode   int    `json:"exit_code"`
	Skipped    bool   `json:"skipped,omitempty"`
	SkipReason string `json:"skip_reason,omitempty"`
}

// DevCheckpointReceipt binds a verification run to the exact source it verified.
type DevCheckpointReceipt struct {
	SchemaVersion int    `json:"schema_version"`
	Kind          string `json:"kind"`
	Head          string `json:"head"`
	Branch        string `json:"branch"`
	// WorktreeDigest is a content digest over every tracked file, taken before
	// the first step and again after the mutation run. HEAD alone would not
	// notice a mutation the restore missed, because a modified tracked file
	// leaves HEAD exactly where it was.
	WorktreeDigest string `json:"worktree_digest"`
	// MutationRestored records that the second digest matched the first. It is a
	// measurement, not an assertion about the mutation script's intentions.
	MutationRestored bool `json:"mutation_restored"`
	// The identities anything downstream binds, recorded so a receipt can be
	// checked against a deployed binary rather than against a commit message.
	CapabilityMatrixSHA256  string              `json:"capability_matrix_sha256"`
	AuthorityDocumentSHA256 string              `json:"authority_document_sha256"`
	Steps                   []devCheckpointStep `json:"steps"`
	CreatedAt               string              `json:"created_at"`
}

// devCheckpointReceiptPath is deliberately outside version control (see
// .gitignore). A receipt is bound to a commit hash, so committing one would
// create a commit that itself has no receipt and the pre-push hook would refuse
// the very commit carrying the proof.
func devCheckpointReceiptPath(root, head string) string {
	return filepath.Join(root, "evidence", "checkpoint", head+".json")
}

// dispatchDev routes `merc dev <subcommand>`.
func dispatchDev(command string, args []string) bool {
	if command != "dev" {
		return false
	}
	if len(args) == 0 {
		fmt.Fprintln(os.Stderr, "usage: merc dev checkpoint [flags]")
		os.Exit(2)
	}
	switch args[0] {
	case "checkpoint":
		os.Exit(runDevCheckpoint(args[1:]))
	case "checkpoint-verify":
		os.Exit(runDevCheckpointVerify(args[1:]))
	default:
		fmt.Fprintf(os.Stderr, "unknown dev subcommand %q\n", args[0])
		os.Exit(2)
	}
	return true
}

func git(root string, args ...string) (string, error) {
	cmd := exec.Command("git", args...)
	cmd.Dir = root
	out, err := cmd.Output()
	if err != nil {
		return "", fmt.Errorf("git %s: %w", strings.Join(args, " "), err)
	}
	return strings.TrimSpace(string(out)), nil
}

// worktreeContentDigest hashes the CONTENT of every tracked file.
//
// `git status --porcelain` is the usual check and it is not enough on its own
// here: it reports what changed, and the question after a mutation run is
// whether the bytes are the ones that were tested. Hashing them answers that
// directly, and it also catches a restore that rewrote a file to different
// content with the same mtime.
func worktreeContentDigest(root string) (string, error) {
	listed, err := git(root, "ls-files", "-z")
	if err != nil {
		return "", err
	}
	paths := strings.Split(strings.TrimRight(listed, "\x00"), "\x00")
	sort.Strings(paths)
	sum := sha256.New()
	for _, path := range paths {
		if path == "" {
			continue
		}
		body, err := os.ReadFile(filepath.Join(root, path))
		if err != nil {
			if os.IsNotExist(err) {
				// A tracked file that is not on disk is itself a difference; record
				// it rather than skipping it.
				fmt.Fprintf(sum, "%s\x00MISSING\x00", path)
				continue
			}
			return "", err
		}
		fileSum := sha256.Sum256(body)
		fmt.Fprintf(sum, "%s\x00%s\x00", path, hex.EncodeToString(fileSum[:]))
	}
	return hex.EncodeToString(sum.Sum(nil)), nil
}

type devCheckpointOptions struct {
	root         string
	skipMutation bool
	skipCI       bool
	databaseURL  string
}

func runDevCheckpoint(args []string) int {
	fs := flag.NewFlagSet("dev checkpoint", flag.ContinueOnError)
	skipMutation := fs.Bool("skip-mutation", false,
		"skip the mutation suite (records the skip in the receipt; the receipt is then not push-eligible)")
	skipCI := fs.Bool("skip-ci", false,
		"skip the full CI target (records the skip; the receipt is then not push-eligible)")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	root, err := git(".", "rev-parse", "--show-toplevel")
	if err != nil {
		fmt.Fprintf(os.Stderr, "checkpoint: not inside a git repository: %v\n", err)
		return 2
	}
	opts := devCheckpointOptions{
		root:         root,
		skipMutation: *skipMutation,
		skipCI:       *skipCI,
		databaseURL:  os.Getenv("MERC_TEST_DATABASE_URL"),
	}
	receipt, err := performDevCheckpoint(opts)
	if err != nil {
		fmt.Fprintf(os.Stderr, "checkpoint FAILED: %v\n", err)
		return 1
	}
	path := devCheckpointReceiptPath(root, receipt.Head)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		fmt.Fprintf(os.Stderr, "checkpoint: %v\n", err)
		return 1
	}
	blob, err := json.MarshalIndent(receipt, "", "  ")
	if err != nil {
		fmt.Fprintf(os.Stderr, "checkpoint: %v\n", err)
		return 1
	}
	if err := os.WriteFile(path, append(blob, '\n'), 0o644); err != nil {
		fmt.Fprintf(os.Stderr, "checkpoint: %v\n", err)
		return 1
	}
	fmt.Printf("checkpoint receipt written for %s (%s)\n  %s\n",
		receipt.Head[:12], receipt.Branch, path)
	if !devCheckpointIsPushEligible(receipt) {
		fmt.Fprintln(os.Stderr,
			"checkpoint: this receipt has skipped steps and does not authorize a push")
		return 1
	}
	return 0
}

// devCheckpointIsPushEligible reports whether a receipt authorizes a push.
//
// A skipped step is recorded rather than hidden, and a receipt with one does not
// authorize anything. Recording the skip and then treating the receipt as
// complete would be worse than having no receipt at all: it would look like
// evidence.
func devCheckpointIsPushEligible(receipt DevCheckpointReceipt) bool {
	if receipt.Head == "" || !receipt.MutationRestored {
		return false
	}
	for _, step := range receipt.Steps {
		if step.Skipped || step.ExitCode != 0 {
			return false
		}
	}
	return len(receipt.Steps) > 0
}

func performDevCheckpoint(opts devCheckpointOptions) (DevCheckpointReceipt, error) {
	branch, err := git(opts.root, "rev-parse", "--abbrev-ref", "HEAD")
	if err != nil {
		return DevCheckpointReceipt{}, err
	}
	if devCheckpointFrozenBranches[branch] {
		return DevCheckpointReceipt{}, fmt.Errorf(
			"branch %q is frozen; a checkpoint runs mutation tooling that modifies the tree", branch)
	}
	head, err := git(opts.root, "rev-parse", "HEAD")
	if err != nil {
		return DevCheckpointReceipt{}, err
	}
	status, err := git(opts.root, "status", "--porcelain")
	if err != nil {
		return DevCheckpointReceipt{}, err
	}
	if status != "" {
		return DevCheckpointReceipt{}, fmt.Errorf(
			"working tree is not clean; a checkpoint binds a receipt to a commit, "+
				"and uncommitted work is not in that commit:\n%s", status)
	}
	// Refuse to start while another mutation runner owns the tree.
	//
	// This is not hypothetical. A checkpoint was killed mid-mutation, its
	// mutation-test.sh survived the kill, and it went on rewriting source files
	// for the next hour — through a later checkpoint whose restoration digest
	// happened to be taken in a clean moment, and into a `make ci` run that then
	// failed on four tests that were testing mutated code. "Never run CI while
	// mutation tooling modifies the same tree" has to be enforced rather than
	// remembered, and the mutation script already publishes a lock for it.
	if owner, held := mutationLockHeld(opts.root); held {
		return DevCheckpointReceipt{}, fmt.Errorf(
			"a mutation run owns this tree (%s); it is rewriting source files, so "+
				"nothing measured here would describe the commit. Wait for it, or if it "+
				"is dead, verify the tree against HEAD before removing the lock", owner)
	}
	before, err := worktreeContentDigest(opts.root)
	if err != nil {
		return DevCheckpointReceipt{}, err
	}

	receipt := DevCheckpointReceipt{
		SchemaVersion:           devCheckpointSchemaVersion,
		Kind:                    "dev_checkpoint_receipt",
		Head:                    head,
		Branch:                  branch,
		WorktreeDigest:          before,
		CapabilityMatrixSHA256:  generatedRuntimeMatrixSHA256,
		AuthorityDocumentSHA256: generatedRuntimeAuthorityFileSHA256,
		CreatedAt:               time.Now().UTC().Format(time.RFC3339),
	}

	// Sequential by construction. The mutation suite rewrites source files in
	// place; a CI run overlapping it would compile a tree nobody chose.
	// The mutation suite runs WITHOUT object storage or a local engine.
	//
	// Its targets are the money and reuse paths, which are database-backed. With
	// MERC_TEST_S3_* and MERC_LLAMA_EMBED_URL in the environment each of the 33
	// mutations re-runs the artifact, chain and two-agent tests as well — around
	// fourteen minutes per mutation, seven hours for the suite — to re-prove
	// object-storage round trips that no mutation touches. Stripping them is a
	// scoping decision, not a weakening: what is being asked is whether the tests
	// CATCH a money defect, and the tests that catch money defects are the ones
	// that still run.
	mutationEnv := os.Environ()
	stripped := mutationEnv[:0]
	for _, entry := range mutationEnv {
		switch {
		case strings.HasPrefix(entry, "MERC_TEST_S3_"),
			strings.HasPrefix(entry, "MERC_LLAMA_EMBED_URL="):
			continue
		}
		stripped = append(stripped, entry)
	}
	mutationEnv = stripped

	run := func(name string, skip bool, skipReason string, dir string, argv ...string) error {
		step := devCheckpointStep{Name: name, Command: strings.Join(argv, " ")}
		if skip {
			step.Skipped, step.SkipReason = true, skipReason
			receipt.Steps = append(receipt.Steps, step)
			fmt.Printf("  ~ %-24s SKIPPED (%s)\n", name, skipReason)
			return nil
		}
		fmt.Printf("  > %-24s %s\n", name, step.Command)
		started := time.Now()
		cmd := exec.Command(argv[0], argv[1:]...)
		cmd.Dir = filepath.Join(opts.root, dir)
		cmd.Stdout, cmd.Stderr = os.Stdout, os.Stderr
		cmd.Env = os.Environ()
		if name == "mutation-suite" {
			cmd.Env = mutationEnv
		}
		err := cmd.Run()
		step.DurationMS = time.Since(started).Milliseconds()
		if err != nil {
			step.ExitCode = 1
			if exit, ok := err.(*exec.ExitError); ok {
				step.ExitCode = exit.ExitCode()
			}
			receipt.Steps = append(receipt.Steps, step)
			return fmt.Errorf("step %q failed (exit %d)", name, step.ExitCode)
		}
		receipt.Steps = append(receipt.Steps, step)
		return nil
	}

	// 1. Source and worktree validation happened above; record it as a step so
	//    the receipt shows the order that actually ran.
	receipt.Steps = append(receipt.Steps, devCheckpointStep{
		Name: "worktree-validation", Command: "git status --porcelain (clean)",
	})

	// 2. Targeted authority tests. Run before the expensive suites so an
	//    authority regression is reported in seconds rather than in an hour.
	if err := run("authority-tests", false, "", "control",
		"go", "test", "-count=1", "-run",
		"Authority|Capability|Activation|Digest|Isolation|Directed|Verification", "."); err != nil {
		return receipt, err
	}

	// 3. Mutation suite. Modifies the tree.
	if err := run("mutation-suite", opts.skipMutation, "--skip-mutation", ".",
		"bash", "scripts/mutation-test.sh"); err != nil {
		return receipt, err
	}

	// 4. Prove the mutation runner restored the tree. Content, not status: a
	//    mutation the restore missed leaves HEAD exactly where it was.
	// The lock first: a digest taken while a runner is still alive is a snapshot
	// of a moving tree, and it can match by luck between two mutations.
	if owner, held := mutationLockHeld(opts.root); held {
		return receipt, fmt.Errorf(
			"the mutation runner still holds %s after its suite returned; a content "+
				"digest taken now would describe a tree that is still moving", owner)
	}
	after, err := worktreeContentDigest(opts.root)
	if err != nil {
		return receipt, err
	}
	receipt.MutationRestored = after == before
	receipt.Steps = append(receipt.Steps, devCheckpointStep{
		Name: "mutation-restoration", Command: "worktree content digest before == after",
	})
	if !receipt.MutationRestored {
		return receipt, fmt.Errorf(
			"the mutation runner did not restore the tree: worktree digest %s became %s; "+
				"CI must not run against a tree nobody chose", before[:12], after[:12])
	}

	// 5. Full CI, only now that the tree is proved restored.
	if err := run("full-ci", opts.skipCI, "--skip-ci", ".", "make", "ci"); err != nil {
		return receipt, err
	}

	// 6. Race suite where policy requires it: the concurrency the money path
	//    actually has, not the whole suite.
	if err := run("race-suite", opts.skipCI, "--skip-ci", "control",
		"go", "test", "-count=1", "-race", "-run",
		"Inflight|Coalesc|Claim|Lease|Concurren", "."); err != nil {
		return receipt, err
	}

	// 7. Receipt/source verification: HEAD must not have moved under the run.
	nowHead, err := git(opts.root, "rev-parse", "HEAD")
	if err != nil {
		return receipt, err
	}
	if nowHead != head {
		return receipt, fmt.Errorf(
			"HEAD moved from %s to %s during the checkpoint; the receipt would name a "+
				"commit that was never tested as a whole", head[:12], nowHead[:12])
	}
	final, err := worktreeContentDigest(opts.root)
	if err != nil {
		return receipt, err
	}
	if final != before {
		// Name the files. `make ci` is not read-only — rename-residue-audit.py and
		// validate-repo-boundary.py regenerate their census receipts — so this
		// fires whenever a commit lands with a stale census, and "the tree changed"
		// alone sends the reader looking for a rogue test rather than at a receipt
		// that needs committing.
		changed, statusErr := git(opts.root, "status", "--porcelain")
		if statusErr != nil {
			changed = "(git status failed: " + statusErr.Error() + ")"
		}
		return receipt, fmt.Errorf(
			"the working tree changed during the checkpoint (%s -> %s); the receipt "+
				"would name a commit that is not what was tested:\n%s\n"+
				"`make ci` regenerates census receipts, so a stale one shows up here — "+
				"commit the regenerated file and run the checkpoint again",
			before[:12], final[:12], changed)
	}
	receipt.Steps = append(receipt.Steps, devCheckpointStep{
		Name: "source-verification", Command: "HEAD and worktree digest unchanged",
	})
	return receipt, nil
}

// runDevCheckpointVerify is what the pre-push hook calls: does a push-eligible
// receipt exist for this exact commit?
func runDevCheckpointVerify(args []string) int {
	fs := flag.NewFlagSet("dev checkpoint-verify", flag.ContinueOnError)
	commit := fs.String("commit", "HEAD", "commit to verify a checkpoint receipt for")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	root, err := git(".", "rev-parse", "--show-toplevel")
	if err != nil {
		fmt.Fprintf(os.Stderr, "checkpoint-verify: %v\n", err)
		return 2
	}
	head, err := git(root, "rev-parse", *commit)
	if err != nil {
		fmt.Fprintf(os.Stderr, "checkpoint-verify: %v\n", err)
		return 2
	}
	body, err := os.ReadFile(devCheckpointReceiptPath(root, head))
	if err != nil {
		fmt.Fprintf(os.Stderr,
			"checkpoint-verify: no checkpoint receipt for %s\n"+
				"  run: cd control && go run . dev checkpoint\n", head[:12])
		return 1
	}
	var receipt DevCheckpointReceipt
	if err := json.Unmarshal(body, &receipt); err != nil {
		fmt.Fprintf(os.Stderr, "checkpoint-verify: unreadable receipt: %v\n", err)
		return 1
	}
	if receipt.Head != head {
		fmt.Fprintf(os.Stderr, "checkpoint-verify: receipt names %s, not %s\n",
			receipt.Head, head)
		return 1
	}
	if !devCheckpointIsPushEligible(receipt) {
		fmt.Fprintf(os.Stderr,
			"checkpoint-verify: the receipt for %s has skipped or failed steps\n", head[:12])
		return 1
	}
	fmt.Printf("checkpoint receipt verified for %s\n", head[:12])
	return 0
}

// mutationLockHeld reports whether scripts/mutation-test.sh currently owns the
// tree. It derives the same path the script does: a hash of the repository root,
// under the temporary directory.
func mutationLockHeld(root string) (string, bool) {
	sum := sha256.Sum256([]byte(root))
	lock := filepath.Join(os.TempDir(), "merc-mutation-"+hex.EncodeToString(sum[:])[:16]+".lock")
	if _, err := os.Stat(lock); err == nil {
		return lock, true
	}
	return "", false
}
