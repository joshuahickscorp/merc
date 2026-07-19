package main

import (
	"bufio"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

func cmdSourceID(args []string) {
	root, field := repoRootOrCwd(), ""
	for i := 0; i < len(args); i++ {
		switch args[i] {
		case "--root":
			root = next(args, &i)
		case "--field":
			field = next(args, &i)
		default:
			fatalf("unknown flag %q", args[i])
		}
	}
	r, err := sourceFingerprint(root)
	if err != nil {
		fatalf("source fingerprint: %v", err)
	}
	if field != "" {
		values := map[string]any{
			"head": r.Head, "dirty": r.Dirty, "file_count": r.FileCount,
			"status_sha256": r.StatusSHA256, "source_sha256": r.SourceSHA256,
		}
		v, ok := values[field]
		if !ok {
			fatalf("unknown field %q", field)
		}
		fmt.Println(v)
		return
	}
	b, _ := canonicalProofJSON(r.toMap())
	fmt.Println(string(b))
}

func repoRootOrCwd() string {
	if out, err := gitBytes(".", "rev-parse", "--show-toplevel"); err == nil {
		return strings.TrimSpace(string(out))
	}
	wd, _ := os.Getwd()
	return filepath.Clean(wd)
}

type proofRecord struct {
	Status string `json:"status"`
	Gate   string `json:"gate"`
	Detail string `json:"detail"`
}

type proofSummary struct {
	Mode         string   `json:"mode"`
	Ledger       string   `json:"ledger"`
	SourceSHA256 string   `json:"source_sha256"`
	Passed       []string `json:"passed"`
	Skipped      []string `json:"skipped"`
}

func verifyProofLedger(path, mode string, current bool) (proofSummary, error) {
	f, err := os.Open(path)
	if err != nil {
		return proofSummary{}, err
	}
	defer f.Close()
	seen, skipped := map[string]bool{}, map[string]bool{}
	source := ""
	s := bufio.NewScanner(f)
	for line := 1; s.Scan(); line++ {
		var r proofRecord
		if err := json.Unmarshal(s.Bytes(), &r); err != nil {
			return proofSummary{}, fmt.Errorf("ledger line %d: %w", line, err)
		}
		switch r.Status {
		case "PASS":
			seen[r.Gate] = true
		case "SKIP":
			skipped[r.Gate] = true
		default:
			return proofSummary{}, fmt.Errorf("gate %s has terminal status %q", r.Gate, r.Status)
		}
		if r.Gate == "source-bound" {
			source = strings.TrimPrefix(r.Detail, "source_sha256=")
		}
	}
	if err := s.Err(); err != nil {
		return proofSummary{}, err
	}
	required := []string{"source-bound", "local-gates", "dependencies", "schema", "lifecycle", "control", "source-stable", "census"}
	if mode == "full" {
		required = append(required, "two-agents", "customer-path", "money-invariants", "performance")
	} else if mode != "contract" {
		return proofSummary{}, fmt.Errorf("mode must be contract or full")
	}
	for _, gate := range required {
		if !seen[gate] {
			return proofSummary{}, fmt.Errorf("required PASS gate missing: %s", gate)
		}
	}
	if source == "" {
		return proofSummary{}, fmt.Errorf("source-bound gate lacks a fingerprint")
	}
	if current {
		cur, err := sourceFingerprint(repoRootOrCwd())
		if err != nil {
			return proofSummary{}, err
		}
		if cur.SourceSHA256 != source {
			return proofSummary{}, fmt.Errorf("ledger source %s is stale for current source %s", source, cur.SourceSHA256)
		}
	}
	pass, skip := make([]string, 0, len(seen)), make([]string, 0, len(skipped))
	for gate := range seen {
		pass = append(pass, gate)
	}
	for gate := range skipped {
		skip = append(skip, gate)
	}
	sort.Strings(pass)
	sort.Strings(skip)
	return proofSummary{Mode: mode, Ledger: path, SourceSHA256: source, Passed: pass, Skipped: skip}, nil
}

func cmdVerify(args []string) {
	ledger, mode, current := "", "contract", false
	for i := 0; i < len(args); i++ {
		switch args[i] {
		case "--ledger":
			ledger = next(args, &i)
		case "--mode":
			mode = next(args, &i)
		case "--current-source":
			current = true
		default:
			fatalf("unknown flag %q", args[i])
		}
	}
	if ledger == "" {
		fatalf("cx verify: --ledger is required")
	}
	result, err := verifyProofLedger(ledger, mode, current)
	if err != nil {
		fmt.Fprintf(os.Stderr, "proof-ledger: FAIL: %v\n", err)
		os.Exit(1)
	}
	b, _ := canonicalProofJSON(map[string]any{"status": "PASS", "proof": result})
	fmt.Println(string(b))
}

func next(args []string, i *int) string {
	if *i+1 >= len(args) {
		fatalf("%s needs a value", args[*i])
	}
	*i++
	return args[*i]
}
