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

var sourceFingerprintDomain = []byte("computexchange-source-fingerprint-v1\x00")

const sourceFingerprintSchema = 1

func isGeneratedReleaseEvidencePath(path string) bool {
	switch path {
	case "census/CODEBASE_CENSUS.json", "census/CODEBASE_CENSUS.md",
		"ops/readiness.json", "ops/go-no-go.json", "RELEASE_READINESS.md":
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
	tmp, err := os.CreateTemp(dir, ".cx-tmp-*")
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

	digest := sha256.New()
	digest.Write(sourceFingerprintDomain)
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
