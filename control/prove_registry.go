// cx prove — validate, report, and optionally execute the canonical 5/5 proof
// registry (proof/5x5-gates.json). Go port of scripts/five-by-five.py: same
// validation rules, same source.json/ledger.jsonl contract. When --run executes
// gate commands, the cx binary's own directory is prepended to PATH so gate
// commands can invoke `cx verify` / `cx source-id` without a separate install.
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
	"time"
)

type proveGate struct {
	ID                string `json:"id"`
	Role              string `json:"role"`
	Scope             string `json:"scope"`
	State             string `json:"state"`
	Acceptance        string `json:"acceptance"`
	Owner             string `json:"owner"`
	Command           string `json:"command"`
	EvidenceValidator string `json:"evidence_validator"`
	NextAction        string `json:"next_action"`
}

func (g proveGate) role() string {
	if g.Role == "" {
		return "outcome"
	}
	return g.Role
}
func (g proveGate) runnable() string {
	if g.Command != "" {
		return g.Command
	}
	return g.EvidenceValidator
}

type proveFacet struct {
	ID            string      `json:"id"`
	Target        int         `json:"target"`
	DefinitionOf5 string      `json:"definition_of_5"`
	Gates         []proveGate `json:"gates"`
}

type proveRegistry struct {
	SchemaVersion int          `json:"schema_version"`
	Facets        []proveFacet `json:"facets"`
}

var proveAllowedStates = map[string]bool{
	"planned": true, "in_progress": true, "ready": true,
	"proven": true, "external_pending": true, "blocked": true,
}
var proveAllowedRoles = map[string]bool{"outcome": true, "prerequisite": true}

// loadRegistry parses + validates the registry, matching five-by-five.py:load_registry.
func loadRegistry(path string) (proveRegistry, []byte, error) {
	var reg proveRegistry
	raw, err := os.ReadFile(path)
	if err != nil {
		return reg, nil, err
	}
	if err := json.Unmarshal(raw, &reg); err != nil {
		return reg, raw, err
	}
	if reg.SchemaVersion != 1 {
		return reg, raw, fmt.Errorf("unsupported schema_version (want 1)")
	}
	if len(reg.Facets) == 0 {
		return reg, raw, fmt.Errorf("facets must be a non-empty list")
	}
	facetIDs := map[string]bool{}
	gateIDs := map[string]bool{}
	for _, f := range reg.Facets {
		if f.ID == "" {
			return reg, raw, fmt.Errorf("every facet needs a non-empty id")
		}
		if facetIDs[f.ID] {
			return reg, raw, fmt.Errorf("duplicate facet id: %s", f.ID)
		}
		facetIDs[f.ID] = true
		if f.Target != 5 {
			return reg, raw, fmt.Errorf("%s: target must be 5", f.ID)
		}
		if f.DefinitionOf5 == "" {
			return reg, raw, fmt.Errorf("%s: definition_of_5 is required", f.ID)
		}
		if len(f.Gates) == 0 {
			return reg, raw, fmt.Errorf("%s: gates must be a non-empty list", f.ID)
		}
		for _, g := range f.Gates {
			qualified := f.ID + "/" + g.ID
			if g.ID == "" {
				return reg, raw, fmt.Errorf("%s: every gate needs a non-empty id", f.ID)
			}
			if gateIDs[qualified] {
				return reg, raw, fmt.Errorf("duplicate gate id: %s", qualified)
			}
			gateIDs[qualified] = true
			if !proveAllowedStates[g.State] {
				return reg, raw, fmt.Errorf("%s: invalid state %q", qualified, g.State)
			}
			if !proveAllowedRoles[g.role()] {
				return reg, raw, fmt.Errorf("%s: invalid role %q", qualified, g.Role)
			}
			if g.Scope == "" || g.Acceptance == "" || g.Owner == "" {
				return reg, raw, fmt.Errorf("%s: scope, acceptance and owner are required", qualified)
			}
			if g.Command != "" && g.EvidenceValidator != "" {
				return reg, raw, fmt.Errorf("%s: choose command or evidence_validator, not both", qualified)
			}
			if g.State == "proven" && g.runnable() == "" {
				return reg, raw, fmt.Errorf("%s: a proven gate needs a repeatable command or evidence_validator", qualified)
			}
			if g.State != "proven" && g.NextAction == "" {
				return reg, raw, fmt.Errorf("%s: every unproven gate needs a concrete next_action", qualified)
			}
		}
	}
	return reg, raw, nil
}

func selectFacets(reg proveRegistry, wanted []string) ([]proveFacet, error) {
	if len(wanted) == 0 {
		return reg.Facets, nil
	}
	byID := map[string]proveFacet{}
	for _, f := range reg.Facets {
		byID[f.ID] = f
	}
	var unknown, out []string
	var sel []proveFacet
	_ = out
	for _, w := range wanted {
		f, ok := byID[w]
		if !ok {
			unknown = append(unknown, w)
			continue
		}
		sel = append(sel, f)
	}
	if len(unknown) > 0 {
		sort.Strings(unknown)
		return nil, fmt.Errorf("unknown facet(s): %s", strings.Join(unknown, ", "))
	}
	return sel, nil
}

func printReport(facets []proveFacet) {
	marker := map[string]string{
		"proven": "PROVEN", "ready": "READY", "in_progress": "WORK",
		"external_pending": "EXT", "blocked": "BLOCK", "planned": "PLAN",
	}
	for _, f := range facets {
		var outcomes, prereqs, outProven, preProven, runnable, external int
		allProven := true
		for _, g := range f.Gates {
			if g.State != "proven" {
				allProven = false
			}
			if g.role() == "prerequisite" {
				prereqs++
				if g.State == "proven" {
					preProven++
				}
			} else {
				outcomes++
				if g.State == "proven" {
					outProven++
				}
			}
			if g.runnable() != "" {
				runnable++
			}
			if g.State == "external_pending" {
				external++
			}
		}
		yes := "NO"
		if allProven {
			yes = "YES"
		}
		fmt.Printf("%s: 5/5 %s | outcomes proven %d/%d | prerequisites proven %d/%d | runnable %d | external pending %d\n",
			f.ID, yes, outProven, outcomes, preProven, prereqs, runnable, external)
		for _, g := range f.Gates {
			role := ""
			if g.role() == "prerequisite" {
				role = " (prerequisite)"
			}
			fmt.Printf("  [%-7s] %s%s: %s\n", marker[g.State], g.ID, role, g.Acceptance)
			if g.State != "proven" {
				fmt.Printf("             -> %s\n", g.NextAction)
			}
		}
	}
}

type proveLedgerRecord struct {
	Facet         string `json:"facet"`
	Gate          string `json:"gate"`
	Command       string `json:"command"`
	ExecutionKind string `json:"execution_kind"`
	Head          string `json:"head"`
	SourceSHA256  string `json:"source_sha256"`
	ExitCode      int    `json:"exit_code"`
	DurationMs    int64  `json:"duration_ms"`
	Stdout        string `json:"stdout"`
	Stderr        string `json:"stderr"`
}

// runProve executes attached gate commands for the selected facets and writes
// source.json + ledger.jsonl, matching five-by-five.py:run_commands. cx's own
// dir is prepended to PATH so `cx verify`/`cx source-id` gate commands resolve.
func runProve(facets []proveFacet, artifactDir, registryPath string, rawRegistry []byte, skip map[string]bool) int {
	if err := os.MkdirAll(artifactDir, 0o755); err != nil {
		fmt.Fprintf(os.Stderr, "5x5 artifact dir: %v\n", err)
		return 2
	}
	ledgerPath := filepath.Join(artifactDir, "ledger.jsonl")
	metaPath := filepath.Join(artifactDir, "source.json")

	root := repoRootOrCwd()
	sourceStart, err := sourceFingerprint(root)
	if err != nil {
		fmt.Fprintf(os.Stderr, "5x5 source fingerprint error: %v\n", err)
		return 2
	}

	regSum := sha256.Sum256(rawRegistry)
	var unexecuted, facetIDs []string
	selectedGates := 0
	allProvenStart := true
	for _, f := range facets {
		facetIDs = append(facetIDs, f.ID)
		for _, g := range f.Gates {
			selectedGates++
			if g.State != "proven" {
				allProvenStart = false
			}
			if g.runnable() == "" {
				unexecuted = append(unexecuted, f.ID+"/"+g.ID)
			}
		}
	}
	meta := map[string]any{
		"schema_version":      1,
		"evidence_scope":      "attached_commands_only_not_facet_5x5",
		"run_status":          "RUNNING",
		"started_at_unix":     time.Now().Unix(),
		"registry":            registryPath,
		"registry_sha256":     fmt.Sprintf("%x", regSum),
		"facets":              facetIDs,
		"selected_gate_count": selectedGates,
		"facet_5x5_at_start":  allProvenStart,
		"unexecuted_gates":    unexecuted,
		"source_start":        sourceStart.toMap(),
	}

	// cx-on-PATH for gate subprocesses.
	env := os.Environ()
	if exe, err := os.Executable(); err == nil {
		env = withPathPrefix(env, filepath.Dir(exe))
	}

	ledger, err := os.Create(ledgerPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "5x5 ledger: %v\n", err)
		return 2
	}
	failures, commandsRun := 0, 0
	var skipped []string
	for _, f := range facets {
		for _, g := range f.Gates {
			cmdStr := g.runnable()
			if cmdStr == "" {
				continue
			}
			ref := f.ID + "/" + g.ID
			if skip[ref] {
				skipped = append(skipped, ref)
				fmt.Printf("SKIP %s: excluded from this run (infra-dependent)\n", ref)
				continue
			}
			commandsRun++
			kind := "command"
			if g.Command == "" {
				kind = "evidence_validator"
			}
			fmt.Printf("RUN %s: %s\n", ref, cmdStr)
			start := time.Now()
			c := exec.Command("/bin/bash", "-c", cmdStr)
			c.Dir = root
			c.Env = env
			var stdout, stderr strings.Builder
			c.Stdout = &stdout
			c.Stderr = &stderr
			runErr := c.Run()
			exitCode := 0
			if runErr != nil {
				if ee, ok := runErr.(*exec.ExitError); ok {
					exitCode = ee.ExitCode()
				} else {
					exitCode = 1
				}
			}
			rec := proveLedgerRecord{
				Facet: f.ID, Gate: g.ID, Command: cmdStr, ExecutionKind: kind,
				Head: sourceStart.Head, SourceSHA256: sourceStart.SourceSHA256,
				ExitCode: exitCode, DurationMs: time.Since(start).Milliseconds(),
				Stdout: stdout.String(), Stderr: stderr.String(),
			}
			line, _ := json.Marshal(rec)
			ledger.Write(line)
			ledger.Write([]byte("\n"))
			status := "PASS"
			if exitCode != 0 {
				status = "FAIL"
				failures++
			}
			fmt.Printf("%s %s\n", status, ref)
			if exitCode != 0 && stderr.Len() > 0 {
				fmt.Fprintln(os.Stderr, strings.TrimRight(stderr.String(), "\n"))
			}
		}
	}
	ledger.Close()

	sourceEnd, err := sourceFingerprint(root)
	if err != nil {
		fmt.Fprintf(os.Stderr, "5x5 final source fingerprint error: %v\n", err)
		return 2
	}
	stable := sourceStart.SourceSHA256 == sourceEnd.SourceSHA256 &&
		sourceStart.StatusSHA256 == sourceEnd.StatusSHA256
	commandFailures := failures
	if commandsRun == 0 {
		failures++
		fmt.Fprintln(os.Stderr, "FAIL no attached gate commands were executed")
	}
	if !stable {
		failures++
		fmt.Fprintln(os.Stderr, "FAIL source changed while attached gate commands were running")
	}
	ledgerBytes, _ := os.ReadFile(ledgerPath)
	ledgerSum := sha256.Sum256(ledgerBytes)

	meta["finished_at_unix"] = time.Now().Unix()
	meta["source_end"] = sourceEnd.toMap()
	meta["source_stable"] = stable
	meta["commands_run"] = commandsRun
	if skipped == nil {
		skipped = []string{}
	}
	meta["skipped_gates"] = skipped
	meta["command_failures"] = commandFailures
	meta["total_failures"] = failures
	if failures == 0 {
		meta["run_status"] = "PASS"
	} else {
		meta["run_status"] = "FAIL"
	}
	meta["ledger_sha256"] = fmt.Sprintf("%x", ledgerSum)

	body, _ := json.MarshalIndent(meta, "", "  ")
	body = append(body, '\n')
	if err := atomicWrite(metaPath, body, 0o644); err != nil {
		fmt.Fprintf(os.Stderr, "5x5 source.json: %v\n", err)
		return 2
	}
	fmt.Printf("ledger: %s\n", ledgerPath)
	fmt.Printf("source: %s\n", metaPath)
	if failures > 0 {
		return 1
	}
	return 0
}

func withPathPrefix(env []string, dir string) []string {
	for i, kv := range env {
		if strings.HasPrefix(kv, "PATH=") {
			env[i] = "PATH=" + dir + string(os.PathListSeparator) + kv[len("PATH="):]
			return env
		}
	}
	return append(env, "PATH="+dir)
}

// cmdProve implements `cx prove` (replaces scripts/five-by-five.py):
//
//	cx prove [--registry P] [--facet ID ...] [--run] [--skip-gate F/G ...] [--artifact-dir DIR]
func cmdProve(args []string) {
	registryPath := filepath.Join(repoRootOrCwd(), "proof", "5x5-gates.json")
	var wantFacets, skipGates []string
	run := false
	artifactDir := ""
	for i := 0; i < len(args); i++ {
		switch args[i] {
		case "--registry":
			registryPath = next(args, &i)
		case "--facet":
			wantFacets = append(wantFacets, next(args, &i))
		case "--run":
			run = true
		case "--skip-gate":
			skipGates = append(skipGates, next(args, &i))
		case "--artifact-dir":
			artifactDir = next(args, &i)
		default:
			fatalf("cx prove: unknown flag %q", args[i])
		}
	}
	abs, err := filepath.Abs(registryPath)
	if err == nil {
		registryPath = abs
	}
	reg, raw, err := loadRegistry(registryPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "5x5 registry error: %v\n", err)
		os.Exit(2)
	}
	facets, err := selectFacets(reg, wantFacets)
	if err != nil {
		fmt.Fprintf(os.Stderr, "5x5 registry error: %v\n", err)
		os.Exit(2)
	}
	printReport(facets)
	if run {
		if artifactDir == "" {
			fatalf("cx prove --run requires --artifact-dir")
		}
		abs, err := filepath.Abs(artifactDir)
		if err == nil {
			artifactDir = abs
		}
		skip := map[string]bool{}
		for _, s := range skipGates {
			skip[s] = true
		}
		os.Exit(runProve(facets, artifactDir, registryPath, raw, skip))
	}
}
