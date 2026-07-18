// cx source-id / cx prove / cx verify — the Go evidence/proof authority (Phase C),
// replacing the Python scripts/ proof programs. Each subcommand reuses the shared
// evidence core (evidence.go) rather than re-implementing hashing/JSON/exec.
package main

import (
	"crypto/sha256"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// cmdSourceID implements `cx source-id` — the Go port of scripts/source_fingerprint.py.
//
//	cx source-id [--root DIR] [--field head|dirty|file_count|status_sha256|source_sha256]
func cmdSourceID(args []string) {
	root := repoRootOrCwd()
	field := ""
	for i := 0; i < len(args); i++ {
		switch args[i] {
		case "--root":
			if i+1 >= len(args) {
				fatalf("--root needs a directory")
			}
			root = args[i+1]
			i++
		case "--field":
			if i+1 >= len(args) {
				fatalf("--field needs a name")
			}
			field = args[i+1]
			i++
		default:
			fatalf("unknown flag %q", args[i])
		}
	}
	res, err := sourceFingerprint(root)
	if err != nil {
		fmt.Fprintf(os.Stderr, "source-fingerprint: %s\n", err)
		os.Exit(2)
	}
	if field != "" {
		switch field {
		case "head":
			fmt.Println(res.Head)
		case "dirty":
			fmt.Println(boolLower(res.Dirty))
		case "file_count":
			fmt.Println(res.FileCount)
		case "status_sha256":
			fmt.Println(res.StatusSHA256)
		case "source_sha256":
			fmt.Println(res.SourceSHA256)
		default:
			fatalf("unknown field %q", field)
		}
		return
	}
	b, err := canonicalProofJSON(res.toMap())
	if err != nil {
		fatalf("encode: %v", err)
	}
	fmt.Println(string(b))
}

func boolLower(b bool) string {
	if b {
		return "true"
	}
	return "false"
}

// repoRootOrCwd defaults --root to the proof script's historical default: the
// repository root (source_fingerprint.py used the script's parent-of-parent).
func repoRootOrCwd() string {
	if out, err := gitBytes(".", "rev-parse", "--show-toplevel"); err == nil {
		return trimNL(string(out))
	}
	wd, _ := os.Getwd()
	return filepath.Clean(wd)
}

func trimNL(s string) string {
	for len(s) > 0 && (s[len(s)-1] == '\n' || s[len(s)-1] == '\r') {
		s = s[:len(s)-1]
	}
	return s
}

// ---- cx verify: the Go port of scripts/verify_proof_ledger.py ----

// ledgerError mirrors the Python LedgerError (partial/stale/malformed ledger).
type ledgerError struct{ msg string }

func (e ledgerError) Error() string { return e.msg }

type parsedLedger struct {
	meta         map[string]string
	passes       map[string][]string
	skips        map[string][]string
	failures     map[string][]string
	ledgerSHA256 string
}

var requiredLedgerMeta = []string{
	"commit", "dirty", "source_sha256", "status_sha256",
	"source_sha256_end", "status_sha256_end",
	"started_at", "completed_at", "proof_mode", "status",
}

// parseLedger reads a three-column TSV ledger, byte-for-byte matching
// verify_proof_ledger.py:parse_ledger (same rows, same failure messages).
func parseLedger(path string) (parsedLedger, error) {
	var p parsedLedger
	raw, err := os.ReadFile(path)
	if err != nil {
		return p, ledgerError{fmt.Sprintf("cannot read ledger: %v", err)}
	}
	p.meta = map[string]string{}
	p.passes = map[string][]string{}
	p.skips = map[string][]string{}
	p.failures = map[string][]string{}
	// Python splitlines() drops the trailing newline; skip empty lines.
	for i, line := range strings.Split(strings.ReplaceAll(string(raw), "\r\n", "\n"), "\n") {
		number := i + 1
		if line == "" {
			continue
		}
		parts := strings.SplitN(line, "\t", 3)
		if len(parts) != 3 {
			return p, ledgerError{fmt.Sprintf("line %d is not a three-column ledger row", number)}
		}
		status, key, detail := parts[0], parts[1], parts[2]
		switch status {
		case "META":
			if _, dup := p.meta[key]; dup {
				return p, ledgerError{"duplicate META key: " + key}
			}
			p.meta[key] = detail
		case "PASS":
			p.passes[key] = append(p.passes[key], detail)
		case "SKIP":
			p.skips[key] = append(p.skips[key], detail)
		case "FAIL":
			p.failures[key] = append(p.failures[key], detail)
		default:
			return p, ledgerError{fmt.Sprintf("line %d has unknown status '%s'", number, status)}
		}
	}
	var missing []string
	for _, k := range requiredLedgerMeta {
		if _, ok := p.meta[k]; !ok {
			missing = append(missing, k)
		}
	}
	sort.Strings(missing)
	if len(missing) > 0 {
		return p, ledgerError{"missing terminal/source META: " + strings.Join(missing, ", ")}
	}
	if p.meta["status"] != "PASS" {
		return p, ledgerError{fmt.Sprintf("terminal status is '%s', not PASS", p.meta["status"])}
	}
	if len(p.failures) > 0 {
		return p, ledgerError{"ledger contains FAIL rows: " + strings.Join(sortedMapKeys(p.failures), ", ")}
	}
	if p.meta["source_sha256"] != p.meta["source_sha256_end"] {
		return p, ledgerError{"start/end source fingerprints differ"}
	}
	if p.meta["status_sha256"] != p.meta["status_sha256_end"] {
		return p, ledgerError{"start/end git-status fingerprints differ"}
	}
	if _, ok := p.passes["source-stability"]; !ok {
		return p, ledgerError{"source-stability PASS is missing"}
	}
	if m := p.meta["proof_mode"]; m != "contract_only" && m != "full_local" {
		return p, ledgerError{fmt.Sprintf("unknown proof mode '%s'", m)}
	}
	sum := sha256.Sum256(raw)
	p.ledgerSHA256 = fmt.Sprintf("%x", sum)
	return p, nil
}

func validateLedger(path, requiredMode string, requiredPasses []string, requireCurrentSource bool, root string) (parsedLedger, error) {
	p, err := parseLedger(path)
	if err != nil {
		return p, err
	}
	if requiredMode != "" && p.meta["proof_mode"] != requiredMode {
		return p, ledgerError{fmt.Sprintf("proof mode '%s' cannot satisfy required mode '%s'", p.meta["proof_mode"], requiredMode)}
	}
	var missing []string
	for _, rp := range requiredPasses {
		if _, ok := p.passes[rp]; !ok {
			missing = append(missing, rp)
		}
	}
	sort.Strings(missing)
	if len(missing) > 0 {
		return p, ledgerError{"required PASS rows missing: " + strings.Join(missing, ", ")}
	}
	if requireCurrentSource {
		if root == "" {
			root = repoRootOrCwd()
		}
		cur, ferr := sourceFingerprint(root)
		if ferr != nil {
			return p, ledgerError{fmt.Sprintf("cannot fingerprint current source: %v", ferr)}
		}
		if cur.SourceSHA256 != p.meta["source_sha256"] {
			return p, ledgerError{fmt.Sprintf("ledger is stale for current source: ledger=%s current=%s", p.meta["source_sha256"], cur.SourceSHA256)}
		}
		if cur.StatusSHA256 != p.meta["status_sha256"] {
			return p, ledgerError{"ledger git-status fingerprint is stale for current source"}
		}
	}
	return p, nil
}

// cmdVerify implements `cx verify` (replaces scripts/verify_proof_ledger.py):
//
//	cx verify --ledger PATH [--mode contract_only|full_local]
//	          [--require-pass KEY ...] [--current-source] [--root DIR]
func cmdVerify(args []string) {
	var ledger, mode, root string
	var requirePass []string
	currentSource := false
	for i := 0; i < len(args); i++ {
		switch args[i] {
		case "--ledger":
			ledger = next(args, &i)
		case "--mode":
			mode = next(args, &i)
		case "--require-pass":
			requirePass = append(requirePass, next(args, &i))
		case "--current-source":
			currentSource = true
		case "--root":
			root = next(args, &i)
		default:
			fatalf("unknown flag %q", args[i])
		}
	}
	if ledger == "" {
		fatalf("cx verify: --ledger is required")
	}
	res, err := validateLedger(ledger, mode, requirePass, currentSource, root)
	if err != nil {
		fmt.Fprintf(os.Stderr, "proof-ledger: FAIL: %s\n", err)
		os.Exit(1)
	}
	passRows, skipRows := 0, 0
	for _, rows := range res.passes {
		passRows += len(rows)
	}
	for _, rows := range res.skips {
		skipRows += len(rows)
	}
	rp := append([]string(nil), requirePass...)
	sort.Strings(rp)
	summary := map[string]any{
		"status":               "PASS",
		"proof_mode":           res.meta["proof_mode"],
		"source_sha256":        res.meta["source_sha256"],
		"ledger_sha256":        res.ledgerSHA256,
		"pass_rows":            passRows,
		"skip_rows":            skipRows,
		"required_passes":      rp,
		"current_source_bound": currentSource,
	}
	b, _ := canonicalProofJSON(summary)
	fmt.Println(string(b))
}

func next(args []string, i *int) string {
	if *i+1 >= len(args) {
		fatalf("%s needs a value", args[*i])
	}
	*i++
	return args[*i]
}

func sortedMapKeys(m map[string][]string) []string {
	ks := make([]string, 0, len(m))
	for k := range m {
		ks = append(ks, k)
	}
	sort.Strings(ks)
	return ks
}
