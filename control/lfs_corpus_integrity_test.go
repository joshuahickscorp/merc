package main

import (
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"testing"
)

// lfsCorpusLedger is the fail-closed expected-count surface for the corpus
// verifier. Counts are derived from the live tree and compared here; silent
// drift is a failure that names the new count and the ledger path.
type lfsCorpusLedger struct {
	SchemaVersion int `json:"schema_version"`
	Expected      struct {
		TrackedPointerFiles       int `json:"tracked_pointer_files"`
		UniquePayloadOIDs         int `json:"unique_payload_oids"`
		MissingObjects            int `json:"missing_objects"`
		CorruptObjects            int `json:"corrupt_objects"`
		ResolvedPayloadMismatches int `json:"resolved_payload_mismatches"`
	} `json:"expected"`
}

// TestLFSCorpusIntegrity is the independent content-integrity authority for
// tracked LFS payloads. It hashes object-store bytes itself and does not
// consult `git lfs fsck` for the pass/fail verdict. The Python entry point
// scripts/verify-lfs-corpus.py is the operator-facing twin of this gate.
func TestLFSCorpusIntegrity(t *testing.T) {
	root, err := filepath.Abs("..")
	if err != nil {
		t.Fatalf("resolve repo root: %v", err)
	}
	if _, err := os.Stat(filepath.Join(root, ".gitattributes")); err != nil {
		t.Skip("not at repo root; skip corpus gate")
	}

	ledgerPath := filepath.Join(root, "evidence", "state", "lfs-corpus-ledger.json")
	raw, err := os.ReadFile(ledgerPath)
	if err != nil {
		t.Fatalf("read corpus ledger: %v", err)
	}
	var ledger lfsCorpusLedger
	if err := json.Unmarshal(raw, &ledger); err != nil {
		t.Fatalf("parse corpus ledger: %v", err)
	}
	if ledger.Expected.TrackedPointerFiles <= 0 || ledger.Expected.UniquePayloadOIDs <= 0 {
		t.Fatalf("ledger expected counts must be positive: %+v", ledger.Expected)
	}

	idx, err := loadLFSIndex(root)
	if err != nil {
		t.Fatalf("load LFS index: %v", err)
	}

	byOID := map[string][]string{}
	for path, entry := range idx {
		byOID[entry.oid] = append(byOID[entry.oid], path)
	}
	for oid := range byOID {
		sort.Strings(byOID[oid])
	}

	var missing, corrupt, mismatches int
	var failLines []string
	var aliasLines []string

	for oid, paths := range byOID {
		obj, err := lfsObjectPath(root, oid)
		if err != nil {
			t.Fatalf("object path for %s: %v", oid, err)
		}
		data, err := os.ReadFile(obj)
		if err != nil {
			if os.IsNotExist(err) {
				missing++
				failLines = append(failLines, fmt.Sprintf("missing object oid=%s paths=[%s]", oid, strings.Join(paths, ", ")))
				continue
			}
			t.Fatalf("read LFS object %s: %v", oid, err)
		}
		sum := sha256.Sum256(data)
		got := fmt.Sprintf("%x", sum[:])
		if got != oid {
			corrupt++
			failLines = append(failLines, fmt.Sprintf("corrupt object oid=%s sha256=%s paths=[%s]", oid, got, strings.Join(paths, ", ")))
			continue
		}
		// Size from any path sharing this oid.
		size := idx[paths[0]].size
		if int64(len(data)) != size {
			corrupt++
			failLines = append(failLines, fmt.Sprintf("corrupt object oid=%s size %d != pointer size %d paths=[%s]", oid, len(data), size, strings.Join(paths, ", ")))
		}
		if len(paths) > 1 {
			aliasLines = append(aliasLines, fmt.Sprintf("oid %s n=%d → %s", oid, len(paths), strings.Join(paths, ", ")))
		}
	}
	sort.Strings(aliasLines)

	for path, entry := range idx {
		abs := filepath.Join(root, path)
		raw, err := os.ReadFile(abs)
		if err != nil {
			continue
		}
		if _, _, isPtr := parseLFSPointer(raw); isPtr {
			continue
		}
		sum := sha256.Sum256(raw)
		got := fmt.Sprintf("%x", sum[:])
		if got != entry.oid {
			mismatches++
			failLines = append(failLines, fmt.Sprintf("resolved-payload mismatch path=%s oid=%s got=%s", path, entry.oid, got))
		} else if int64(len(raw)) != entry.size {
			mismatches++
			failLines = append(failLines, fmt.Sprintf("resolved-payload size mismatch path=%s size %d != %d", path, len(raw), entry.size))
		}
	}

	observedPointers := len(idx)
	observedOIDs := len(byOID)
	dupPointers := observedPointers - observedOIDs

	t.Logf("tracked LFS pointer files      == %d (expected %d)", observedPointers, ledger.Expected.TrackedPointerFiles)
	t.Logf("unique payload OIDs            == %d (expected %d)", observedOIDs, ledger.Expected.UniquePayloadOIDs)
	t.Logf("missing objects                == %d (expected %d)", missing, ledger.Expected.MissingObjects)
	t.Logf("corrupt objects                == %d (expected %d)", corrupt, ledger.Expected.CorruptObjects)
	t.Logf("resolved-payload mismatches    == %d (expected %d)", mismatches, ledger.Expected.ResolvedPayloadMismatches)
	t.Logf("content-addressed aliases      == %d extra pointers", dupPointers)
	for _, line := range aliasLines {
		t.Logf("  %s", line)
	}

	// Supplementary fsck — never authority.
	if out, err := exec.Command("git", "-C", root, "lfs", "fsck").CombinedOutput(); err != nil {
		t.Logf("git lfs fsck (supplementary) → FAIL: %v\n%s", err, out)
	} else {
		t.Logf("git lfs fsck (supplementary) → %s", strings.TrimSpace(string(out)))
	}

	var countFails []string
	if observedPointers != ledger.Expected.TrackedPointerFiles {
		countFails = append(countFails, fmt.Sprintf(
			"%d pointers, expected %d — update the ledger (evidence/state/lfs-corpus-ledger.json)",
			observedPointers, ledger.Expected.TrackedPointerFiles))
	}
	if observedOIDs != ledger.Expected.UniquePayloadOIDs {
		countFails = append(countFails, fmt.Sprintf(
			"%d unique payload OIDs, expected %d — update the ledger (evidence/state/lfs-corpus-ledger.json)",
			observedOIDs, ledger.Expected.UniquePayloadOIDs))
	}
	if missing != ledger.Expected.MissingObjects {
		countFails = append(countFails, fmt.Sprintf("missing_objects: observed %d, expected %d", missing, ledger.Expected.MissingObjects))
	}
	if corrupt != ledger.Expected.CorruptObjects {
		countFails = append(countFails, fmt.Sprintf("corrupt_objects: observed %d, expected %d", corrupt, ledger.Expected.CorruptObjects))
	}
	if mismatches != ledger.Expected.ResolvedPayloadMismatches {
		countFails = append(countFails, fmt.Sprintf("resolved_payload_mismatches: observed %d, expected %d", mismatches, ledger.Expected.ResolvedPayloadMismatches))
	}

	if len(countFails) > 0 || len(failLines) > 0 {
		for _, f := range append(countFails, failLines...) {
			t.Error(f)
		}
	}
}

// TestLFSPointerStubIsNotEvidenceJSON proves a three-line LFS stub is rejected
// as a receipt body rather than parsed as JSON or silently skipped. This is
// the corpus-level twin of the fingerprint binder's refuse-on-pointer rule.
func TestLFSPointerStubIsNotEvidenceJSON(t *testing.T) {
	stub := []byte("version https://git-lfs.github.com/spec/v1\noid sha256:0684258dc2d0ff1d74cfd32cfe5a68c5eba22c5ee8e7282a496e399290246236\nsize 17748\n")
	oid, size, ok := parseLFSPointer(stub)
	if !ok {
		t.Fatal("expected parseLFSPointer to accept a canonical three-line stub")
	}
	if oid != "0684258dc2d0ff1d74cfd32cfe5a68c5eba22c5ee8e7282a496e399290246236" || size != 17748 {
		t.Fatalf("unexpected pointer parse: oid=%s size=%d", oid, size)
	}
	var v any
	if err := json.Unmarshal(stub, &v); err == nil {
		t.Fatalf("three-line LFS stub must not parse as JSON; got %#v", v)
	} else if !strings.Contains(err.Error(), "invalid character") && !strings.Contains(err.Error(), "looking for beginning") {
		// Still a refusal — log the exact error so the proof is legible.
		t.Logf("JSON refuse (acceptable form): %v", err)
	} else {
		t.Logf("JSON refuse: %v", err)
	}
}
