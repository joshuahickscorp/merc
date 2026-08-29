package main

import (
	"bytes"
	"crypto/sha256"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"hash"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
)

// v2 binds LFS-tracked paths by pointer OID + size (verified against the
// payload), not by hashing whatever happens to sit on disk. v1 hashed the
// three-line pointer when unsmudged and the full payload when smudged — two
// digests for one logical tree. Historical v1 digests are enumerated in
// evidence/state/fingerprint-supersession.json; they are not rewritten.
var sourceFingerprintDomain = []byte("computexchange-source-fingerprint-v2\x00")

const sourceFingerprintSchema = 2

func isGeneratedReleaseEvidencePath(path string) bool {
	switch path {
	case "evidence/census/CODEBASE_CENSUS.json", "evidence/census/CODEBASE_CENSUS.md",
		"ops/readiness.json", "ops/go-no-go.json", "docs/RELEASE_READINESS.md":
		return true
	default:
		return false
	}
}

func canonicalProofJSON(v any) ([]byte, error) {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(v); err != nil {
		return nil, err
	}
	return bytes.TrimRight(buf.Bytes(), "\n"), nil
}

func atomicWrite(path string, data []byte, perm os.FileMode) error {
	dir := filepath.Dir(path)
	tmp, err := os.CreateTemp(dir, ".merc-tmp-*")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName)
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Chmod(perm); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmpName, path)
}

func framed(h hash.Hash, value []byte) {
	var n [8]byte
	binary.BigEndian.PutUint64(n[:], uint64(len(value)))
	h.Write(n[:])
	h.Write(value)
}

func gitBytes(root string, args ...string) ([]byte, error) {
	cmd := exec.Command("git", append([]string{"-C", root}, args...)...)
	var out, errb bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = &errb
	if err := cmd.Run(); err != nil {
		detail := strings.TrimSpace(errb.String())
		if detail == "" {
			detail = fmt.Sprintf("git %s failed", strings.Join(args, " "))
		}
		return nil, fmt.Errorf("%s", detail)
	}
	return out.Bytes(), nil
}

type sourceFingerprintResult struct {
	SchemaVersion int    `json:"schema_version"`
	Head          string `json:"head"`
	Dirty         bool   `json:"dirty"`
	FileCount     int    `json:"file_count"`
	StatusSHA256  string `json:"status_sha256"`
	SourceSHA256  string `json:"source_sha256"`
}

func (r sourceFingerprintResult) toMap() map[string]any {
	return map[string]any{
		"schema_version": r.SchemaVersion,
		"head":           r.Head,
		"dirty":          r.Dirty,
		"file_count":     r.FileCount,
		"status_sha256":  r.StatusSHA256,
		"source_sha256":  r.SourceSHA256,
	}
}

func sourceFingerprint(root string) (sourceFingerprintResult, error) {
	var zero sourceFingerprintResult
	topRaw, err := gitBytes(root, "rev-parse", "--show-toplevel")
	if err != nil {
		return zero, err
	}
	repo := strings.TrimSpace(string(topRaw))

	head := "UNBORN"
	if h, err := gitBytes(repo, "rev-parse", "HEAD"); err == nil {
		head = strings.TrimSpace(string(h))
	}

	rawPaths, err := gitBytes(repo, "ls-files", "-z", "--cached", "--others", "--exclude-standard")
	if err != nil {
		return zero, err
	}
	seen := map[string]struct{}{}
	var paths []string
	for _, p := range bytes.Split(rawPaths, []byte{0}) {
		if len(p) == 0 {
			continue
		}
		s := string(p)
		if isGeneratedReleaseEvidencePath(s) {
			continue
		}
		if _, ok := seen[s]; !ok {
			seen[s] = struct{}{}
			paths = append(paths, s)
		}
	}
	sort.Strings(paths) // byte-wise, matches Python bytes sort

	rawStatus, err := gitBytes(repo, "status", "--porcelain=v1", "-z", "--untracked-files=all")
	if err != nil {
		return zero, err
	}

	lfsIndex, err := loadLFSIndex(repo)
	if err != nil {
		return zero, err
	}

	digest := sha256.New()
	digest.Write(sourceFingerprintDomain)
	// Producer / source identity: the commit this tree is bound to.
	framed(digest, []byte(head))
	for _, rel := range paths {
		framed(digest, []byte(rel))
		abs := filepath.Join(repo, rel)
		fi, lerr := os.Lstat(abs)
		if lerr != nil {
			framed(digest, []byte("missing"))
			continue
		}
		mode := fi.Mode()
		switch {
		case mode&os.ModeSymlink != 0:
			framed(digest, []byte("symlink"))
			target, err := os.Readlink(abs)
			if err != nil {
				return zero, fmt.Errorf("readlink %s: %w", rel, err)
			}
			framed(digest, []byte(target))
		case mode.IsRegular():
			if entry, ok := lfsIndex[rel]; ok {
				// LFS-tracked: bind path + OID + size after verifying payload.
				// Never hash the three-line pointer; never accept an absent body.
				if err := verifyLFSPayload(repo, abs, entry); err != nil {
					return zero, fmt.Errorf("source fingerprint LFS %s: %w", rel, err)
				}
				if mode.Perm()&0o100 != 0 {
					framed(digest, []byte("lfs+x"))
				} else {
					framed(digest, []byte("lfs"))
				}
				framed(digest, []byte(entry.oid))
				framed(digest, []byte(fmt.Sprintf("%d", entry.size)))
				continue
			}
			if mode.Perm()&0o100 != 0 {
				framed(digest, []byte("file+x"))
			} else {
				framed(digest, []byte("file"))
			}
			sum, err := hashFile(abs)
			if err != nil {
				return zero, err
			}
			framed(digest, sum)
		case mode.IsDir():
			framed(digest, []byte("gitlink"))
			gl, err := gitBytes(repo, "rev-parse", "HEAD:"+rel)
			if err != nil {
				return zero, err
			}
			framed(digest, gl) // includes git's trailing newline, matching Python
		default:
			return zero, fmt.Errorf("unsupported source path type: %s", rel)
		}
	}

	statusSum := sha256.Sum256(rawStatus)
	return sourceFingerprintResult{
		SchemaVersion: sourceFingerprintSchema,
		Head:          head,
		Dirty:         len(rawStatus) > 0,
		FileCount:     len(paths),
		StatusSHA256:  fmt.Sprintf("%x", statusSum),
		SourceSHA256:  fmt.Sprintf("%x", digest.Sum(nil)),
	}, nil
}

// lfsIndexEntry is the logical identity of an LFS-tracked path: the pointer's
// content OID (sha256 of the payload) and declared size.
type lfsIndexEntry struct {
	oid  string
	size int64
}

// loadLFSIndex maps tracked paths to their index-pointer OID and size.
// Uses `git lfs ls-files --long` for the path set and the index blob (always
// the pointer for LFS-tracked files) for size, so smudge state is irrelevant.
func loadLFSIndex(repo string) (map[string]lfsIndexEntry, error) {
	out, err := gitBytes(repo, "lfs", "ls-files", "--long")
	if err != nil {
		// git-lfs absent or not installed: treat as no LFS paths only when the
		// error is clearly "not a command". Otherwise fail closed.
		if strings.Contains(err.Error(), "is not a git command") ||
			strings.Contains(err.Error(), "git-lfs") && strings.Contains(err.Error(), "not found") {
			return map[string]lfsIndexEntry{}, nil
		}
		// Empty repo / no LFS files still exits 0 with empty stdout. Other
		// failures are real.
		return nil, fmt.Errorf("git lfs ls-files: %w", err)
	}
	// git-lfs can include a path from HEAD while a staged rename is already
	// present in the index. The index is the authority this verifier hashes, so
	// discard any stale path before asking git for its index blob.
	trackedRaw, err := gitBytes(repo, "ls-files", "-z")
	if err != nil {
		return nil, fmt.Errorf("git ls-files: %w", err)
	}
	tracked := map[string]bool{}
	for _, path := range strings.Split(string(trackedRaw), "\x00") {
		if path != "" {
			tracked[path] = true
		}
	}
	idx := map[string]lfsIndexEntry{}
	for _, line := range strings.Split(string(out), "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		// Format: <64-hex-oid> <*|-> <path>
		fields := strings.Fields(line)
		if len(fields) < 3 {
			continue
		}
		oid := strings.ToLower(fields[0])
		if len(oid) != 64 {
			continue
		}
		// Path may contain spaces; take everything after the status marker.
		pathStart := strings.Index(line, fields[1])
		if pathStart < 0 {
			continue
		}
		rel := strings.TrimSpace(line[pathStart+len(fields[1]):])
		if rel == "" || !tracked[rel] {
			continue
		}
		ptr, err := gitBytes(repo, "cat-file", "blob", ":"+rel)
		if err != nil {
			return nil, fmt.Errorf("LFS index pointer for %s: %w", rel, err)
		}
		parsedOID, size, ok := parseLFSPointer(ptr)
		if !ok {
			return nil, fmt.Errorf("LFS path %s: index blob is not a valid pointer", rel)
		}
		if parsedOID != oid {
			return nil, fmt.Errorf("LFS path %s: ls-files oid %s != pointer oid %s", rel, oid, parsedOID)
		}
		idx[rel] = lfsIndexEntry{oid: oid, size: size}
	}
	return idx, nil
}

// parseLFSPointer extracts oid and size from a git-lfs pointer blob.
func parseLFSPointer(data []byte) (oid string, size int64, ok bool) {
	// Pointers are tiny; a large blob is never a pointer.
	if len(data) == 0 || len(data) > 1024 {
		return "", 0, false
	}
	text := string(data)
	if !strings.HasPrefix(text, "version https://git-lfs.github.com/spec/v1") {
		return "", 0, false
	}
	oid = ""
	size = -1
	for _, line := range strings.Split(text, "\n") {
		line = strings.TrimRight(line, "\r")
		switch {
		case strings.HasPrefix(line, "oid sha256:"):
			oid = strings.ToLower(strings.TrimSpace(line[len("oid sha256:"):]))
		case strings.HasPrefix(line, "size "):
			var n int64
			if _, err := fmt.Sscanf(line, "size %d", &n); err == nil {
				size = n
			}
		}
	}
	if len(oid) != 64 || size < 0 {
		return "", 0, false
	}
	for _, c := range oid {
		if !((c >= '0' && c <= '9') || (c >= 'a' && c <= 'f')) {
			return "", 0, false
		}
	}
	return oid, size, true
}

// verifyLFSPayload resolves the payload for an LFS-tracked path and checks
// sha256(payload)==oid and len(payload)==size. Fails when the object is
// absent — citation of an unverified LFS path is refused at the fingerprint.
func verifyLFSPayload(repo, abs string, entry lfsIndexEntry) error {
	payload, err := resolveLFSPayload(repo, abs, entry.oid)
	if err != nil {
		return err
	}
	sum := sha256.Sum256(payload)
	got := fmt.Sprintf("%x", sum[:])
	if got != entry.oid {
		return fmt.Errorf("payload sha256 %s != oid %s", got, entry.oid)
	}
	if int64(len(payload)) != entry.size {
		return fmt.Errorf("payload size %d != pointer size %d", len(payload), entry.size)
	}
	return nil
}

// resolveLFSPayload returns payload bytes from the working tree (when smudged
// and matching oid) or from the local LFS object store. Never returns the
// pointer text as a payload.
func resolveLFSPayload(repo, abs, oid string) ([]byte, error) {
	if raw, err := os.ReadFile(abs); err == nil {
		diskOID, _, isPtr := parseLFSPointer(raw)
		if !isPtr {
			sum := sha256.Sum256(raw)
			if fmt.Sprintf("%x", sum[:]) == oid {
				return raw, nil
			}
			// Working tree has non-pointer bytes that do not match the tracked
			// oid (dirty edit of an LFS file). Refuse rather than fingerprint
			// an unverified body under the pointer's identity.
			return nil, fmt.Errorf("working-tree bytes for %s do not match LFS oid %s (dirty LFS edit?)", abs, oid)
		}
		// The working tree holds a POINTER. It must name the object the index
		// names. If it does not, this path has been repointed at another
		// object -- and that is not a harmless difference, because the two
		// readers of this tree disagree about which bytes are authoritative:
		// the v2 fingerprint is composed from index-side oid+size, while
		// ops/scripts/lib/evidence_binding.py resolves the payload from the oid
		// written in the ON-DISK pointer. Overwriting a failing receipt with a
		// pointer copied from a passing one therefore swaps what every binding
		// validator reads while leaving source_sha256 byte-identical.
		//
		// Reproduced before this guard existed: an authorize-tail receipt
		// reading p99_ms 1840.0 / verdict FAIL was repointed at an
		// arrival-batching object and read back p99_ms 3.0 / verdict PASS, with
		// `cx verify --current-source` returning PASS both times. Refuse.
		if !strings.EqualFold(strings.TrimSpace(diskOID), oid) {
			return nil, fmt.Errorf(
				"working-tree pointer for %s names LFS oid %s but the index names %s; "+
					"the payload readers resolve the on-disk oid, so this would fingerprint "+
					"one body and validate another", abs, diskOID, oid)
		}
	}
	obj, err := lfsObjectPath(repo, oid)
	if err != nil {
		return nil, err
	}
	data, err := os.ReadFile(obj)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, fmt.Errorf("LFS object absent for oid %s (path %s); refuse citation until payload is present (git lfs pull; for evidence/perf/** use --exclude=\"\" --include=\"evidence/perf/**\")", oid, abs)
		}
		return nil, err
	}
	return data, nil
}

func lfsObjectPath(repo, oid string) (string, error) {
	if len(oid) != 64 {
		return "", fmt.Errorf("invalid LFS oid %q", oid)
	}
	// Worktrees store LFS objects under the common git dir.
	common, err := gitBytes(repo, "rev-parse", "--git-common-dir")
	if err != nil {
		return "", err
	}
	commonDir := strings.TrimSpace(string(common))
	if !filepath.IsAbs(commonDir) {
		commonDir = filepath.Join(repo, commonDir)
	}
	return filepath.Join(commonDir, "lfs", "objects", oid[:2], oid[2:4], oid), nil
}

func hashFile(path string) ([]byte, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return nil, err
	}
	return h.Sum(nil), nil
}
