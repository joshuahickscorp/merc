package main

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
)

// The compiler is deliberately a bounded proposal/probe surface. A buyer may
// upload a small project archive and receive a digest-bound IR, but this route
// does not create a quote, reserve capacity, or authorize execution.
const (
	maxProjectCompileUploadBytes   = 64 << 20
	maxProjectCompileExpandedBytes = 64 << 20
	maxProjectCompileFileBytes     = 32 << 20
)

func (s *Server) handleProjectCompile(w http.ResponseWriter, r *http.Request) {
	raw, err := io.ReadAll(io.LimitReader(r.Body, maxProjectCompileUploadBytes+1))
	if err != nil || len(raw) > maxProjectCompileUploadBytes {
		writeErr(w, http.StatusRequestEntityTooLarge, "project archive exceeds the 64 MiB compile limit")
		return
	}
	root, err := extractProjectArchive(raw, r.Header.Get("Content-Type"))
	if err != nil {
		writeErr(w, http.StatusBadRequest, "project archive refused: "+err.Error())
		return
	}
	defer os.RemoveAll(root)
	probeRequested, err := parseProjectProbeHeader(r.Header.Get("X-Merc-Bounded-Probe"))
	if err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	approved := strings.TrimSpace(r.Header.Get("X-Merc-Approved-IR-SHA256"))
	ir, err := compileProject(projectCompileOptions{
		Root: root, ProbeRequested: probeRequested, BuyerApprovedIRSHA256: approved,
	})
	if err != nil {
		writeErr(w, http.StatusBadRequest, "project compile refused: "+err.Error())
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("X-Merc-Project-SHA256", ir.ProjectSHA256)
	w.Header().Set("X-Merc-Project-IR-SHA256", ir.IRSHA256)
	if probeRequested {
		w.Header().Set("X-Merc-Bounded-Probe", "executed")
	} else {
		w.Header().Set("X-Merc-Bounded-Probe", "not_requested")
	}
	writeJSON(w, http.StatusOK, ir)
}

func parseProjectProbeHeader(raw string) (bool, error) {
	switch strings.ToLower(strings.TrimSpace(raw)) {
	case "", "0", "false", "no":
		return false, nil
	case "1", "true", "yes":
		return true, nil
	default:
		return false, errors.New("X-Merc-Bounded-Probe must be true or false")
	}
}

func extractProjectArchive(raw []byte, contentType string) (string, error) {
	if len(raw) == 0 {
		return "", errors.New("archive is empty")
	}
	tempRoot, err := os.MkdirTemp("", "merc-project-compile-")
	if err != nil {
		return "", fmt.Errorf("create isolated project root: %w", err)
	}
	cleanup := true
	defer func() {
		if cleanup {
			_ = os.RemoveAll(tempRoot)
		}
	}()

	var input io.Reader = bytes.NewReader(raw)
	if strings.Contains(strings.ToLower(contentType), "gzip") || (len(raw) >= 2 && raw[0] == 0x1f && raw[1] == 0x8b) {
		gz, err := gzip.NewReader(bytes.NewReader(raw))
		if err != nil {
			return "", fmt.Errorf("open gzip archive: %w", err)
		}
		defer gz.Close()
		input = io.LimitReader(gz, maxProjectCompileExpandedBytes+1)
	}
	tr := tar.NewReader(input)
	seen := make(map[string]struct{})
	var expanded int64
	entries := 0
	for {
		header, err := tr.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return "", fmt.Errorf("read tar entry: %w", err)
		}
		name, err := cleanProjectArchivePath(header.Name)
		if err != nil {
			return "", err
		}
		if _, exists := seen[name]; exists {
			return "", fmt.Errorf("archive repeats path %q", name)
		}
		seen[name] = struct{}{}
		entries++
		if entries > projectMaxFiles {
			return "", fmt.Errorf("archive exceeds %d-file inspection ceiling", projectMaxFiles)
		}
		path := filepath.Join(tempRoot, filepath.FromSlash(name))
		switch header.Typeflag {
		case tar.TypeDir:
			if header.Size != 0 {
				return "", fmt.Errorf("directory %q carries file bytes", name)
			}
			if err := os.MkdirAll(path, 0o700); err != nil {
				return "", fmt.Errorf("create directory %q: %w", name, err)
			}
		case tar.TypeReg, tar.TypeRegA:
			if header.Size < 0 || header.Size > maxProjectCompileFileBytes {
				return "", fmt.Errorf("file %q exceeds the %d-byte compile file limit", name, maxProjectCompileFileBytes)
			}
			if expanded > maxProjectCompileExpandedBytes-header.Size {
				return "", fmt.Errorf("archive expands beyond the %d-byte compile limit", maxProjectCompileExpandedBytes)
			}
			if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
				return "", fmt.Errorf("create parent for %q: %w", name, err)
			}
			file, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
			if err != nil {
				return "", fmt.Errorf("create file %q: %w", name, err)
			}
			written, copyErr := io.CopyN(file, tr, header.Size)
			closeErr := file.Close()
			if copyErr != nil {
				return "", fmt.Errorf("read file %q: %w", name, copyErr)
			}
			if closeErr != nil {
				return "", fmt.Errorf("close file %q: %w", name, closeErr)
			}
			if written != header.Size {
				return "", fmt.Errorf("file %q ended before its declared size", name)
			}
			expanded += written
		case tar.TypeSymlink, tar.TypeLink:
			return "", fmt.Errorf("archive link %q is not allowed", name)
		default:
			return "", fmt.Errorf("archive entry %q has unsupported type", name)
		}
	}
	if entries == 0 {
		return "", errors.New("archive contains no entries")
	}
	cleanup = false
	return tempRoot, nil
}

func cleanProjectArchivePath(raw string) (string, error) {
	name := strings.TrimPrefix(strings.TrimSpace(raw), "./")
	if name == "" || strings.ContainsRune(name, '\x00') || strings.Contains(name, "\\") || strings.HasPrefix(name, "/") {
		return "", fmt.Errorf("archive path %q is not a safe relative path", raw)
	}
	clean := filepath.ToSlash(filepath.Clean(filepath.FromSlash(name)))
	if clean == "." || clean == ".." || strings.HasPrefix(clean, "../") || strings.Contains(clean, "//") {
		return "", fmt.Errorf("archive path %q escapes the project root", raw)
	}
	return clean, nil
}
