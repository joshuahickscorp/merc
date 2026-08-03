package main

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// Pricing citation authority.
//
// A catalogue price row that cites an unbindable artifact sets a live buyer
// price from a number nobody can reproduce. Cell routability already refused
// that for media (fdc8eec1); this file is the same gate for catalogue pricing.
//
// Bindability for pricing is intentionally the source-commit half of cell
// authority, not the full producer-identity BOUND status: legacy catalogue
// receipts (evidence/benchmarks/…) predate identity fields and still price
// embed / batch_infer. What is refused is a free-string or missing
// merc_source_commit on a runtime-benchmark receipt — the exact failure that
// left media rows pricing after their cells were quarantined.

// pricingRepoRootCandidates are tried in order when resolving a SourceCitation
// path. go test runs with cwd=control/; production usually starts at the repo
// root or a deploy root that still holds evidence/ beside the binary's workdir.
func pricingRepoRootCandidates() []string {
	var out []string
	if root, err := gitBytes(".", "rev-parse", "--show-toplevel"); err == nil {
		out = append(out, strings.TrimSpace(string(root)))
	}
	if root, err := gitBytes("..", "rev-parse", "--show-toplevel"); err == nil {
		out = append(out, strings.TrimSpace(string(root)))
	}
	out = append(out, ".", "..")
	// Dedup while preserving order.
	seen := map[string]bool{}
	uniq := make([]string, 0, len(out))
	for _, r := range out {
		if r == "" || seen[r] {
			continue
		}
		seen[r] = true
		uniq = append(uniq, r)
	}
	return uniq
}

// resolveCitedEvidencePath returns the absolute path of a citation file under
// one of the candidate repo roots, or an error if none can read it.
func resolveCitedEvidencePath(citationPath string) (string, error) {
	citationPath = strings.TrimSpace(citationPath)
	if citationPath == "" {
		return "", fmt.Errorf("empty citation path")
	}
	if filepath.IsAbs(citationPath) {
		if _, err := os.Stat(citationPath); err != nil {
			return "", fmt.Errorf("cited receipt %q: %w", citationPath, err)
		}
		return citationPath, nil
	}
	var lastErr error
	for _, root := range pricingRepoRootCandidates() {
		full := filepath.Join(root, citationPath)
		if _, err := os.Stat(full); err == nil {
			return full, nil
		} else {
			lastErr = err
		}
	}
	return "", fmt.Errorf("cited receipt %q not found under any repo root: %v", citationPath, lastErr)
}

// validateRepricingBenchmarkCitation refuses a price row whose SourceCitation
// does not resolve or whose artifact cannot bind for pricing.
func validateRepricingBenchmarkCitation(b measuredThroughput) error {
	path, fragment, ok := strings.Cut(b.SourceCitation, "#")
	if !ok || strings.TrimSpace(fragment) == "" {
		return fmt.Errorf("%s/%s: citation %q names no fragment",
			b.ModelID, b.JobType, b.SourceCitation)
	}
	full, err := resolveCitedEvidencePath(path)
	if err != nil {
		return fmt.Errorf("%s/%s: %w", b.ModelID, b.JobType, err)
	}
	raw, err := os.ReadFile(full)
	if err != nil {
		return fmt.Errorf("%s/%s: read cited receipt: %w", b.ModelID, b.JobType, err)
	}
	var receipt map[string]any
	if err := json.Unmarshal(raw, &receipt); err != nil {
		return fmt.Errorf("%s/%s: cited receipt is not JSON: %w", b.ModelID, b.JobType, err)
	}
	if err := ensurePricingCitationBindable(path, receipt); err != nil {
		return fmt.Errorf("%s/%s: %w", b.ModelID, b.JobType, err)
	}
	section, ok := receipt[fragment].(map[string]any)
	if !ok {
		return fmt.Errorf("%s/%s: cited receipt has no %q section",
			b.ModelID, b.JobType, fragment)
	}
	if model, _ := section["model"].(string); model != "" && model != b.ModelID {
		return fmt.Errorf("%s/%s: constant claims model %q, receipt measured %q",
			b.ModelID, b.JobType, b.ModelID, model)
	}
	measured, ok := measuredUnitsPerSecond(section)
	if !ok {
		return fmt.Errorf("%s/%s: receipt section %q reports no recognised throughput",
			b.ModelID, b.JobType, fragment)
	}
	// Conservative bound only: constant may sit slightly below the measurement,
	// never above it. Same rule as TestEveryRepricingBenchmarkMatchesItsCitedReceipt.
	const maxConservativeShortfall = 0.01
	switch {
	case b.UnitsPerSec > measured+1e-4:
		return fmt.Errorf("%s/%s: constant %.6f units/sec exceeds cited measurement %.6f",
			b.ModelID, b.JobType, b.UnitsPerSec, measured)
	case b.UnitsPerSec < measured*(1-maxConservativeShortfall):
		return fmt.Errorf("%s/%s: constant %.6f units/sec is more than 1%% below cited measurement %.6f",
			b.ModelID, b.JobType, b.UnitsPerSec, measured)
	}
	return nil
}

// ensurePricingCitationBindable is the load-bearing check: a free-string or
// missing merc_source_commit on a runtime-benchmark receipt cannot set a price.
//
// Rules:
//  1. If merc_source_commit is present, it must be a real git object
//     (validateMercSourceCommit).
//  2. If producer_identity.source_commit.value is present (BOUND shape), it must
//     also be a real git object.
//  3. Receipts under evidence/perf/runtime-benchmarks/ must carry a bindable
//     merc_source_commit (or BOUND producer_identity source_commit). Missing is
//     a refusal — that is how the rendering receipt failed.
//  4. Legacy catalogue receipts outside runtime-benchmarks/ that predate the
//     commit field remain usable only while they do not claim a bad commit.
func ensurePricingCitationBindable(citationPath string, receipt map[string]any) error {
	if commit, ok := receipt["merc_source_commit"].(string); ok {
		if err := validateMercSourceCommit(commit); err != nil {
			return fmt.Errorf("cited artifact is unbindable: %w", err)
		}
		return nil
	}
	if pi, ok := receipt["producer_identity"].(map[string]any); ok {
		if sc, ok := pi["source_commit"].(map[string]any); ok {
			if v, ok := sc["value"].(string); ok && strings.TrimSpace(v) != "" {
				if err := validateMercSourceCommit(v); err != nil {
					return fmt.Errorf("cited artifact is unbindable: %w", err)
				}
				return nil
			}
		}
	}
	// Normalise path separators for the runtime-benchmarks check.
	norm := filepath.ToSlash(citationPath)
	if strings.Contains(norm, "evidence/perf/runtime-benchmarks/") {
		return fmt.Errorf("cited runtime-benchmark artifact is unbindable: merc_source_commit is missing")
	}
	return nil
}

// measuredUnitsPerSecond reads the throughput a receipt section reports, in the
// unit the repricing constant is expressed in.
//
// Lanes report different keys because they measure different things —
// embeddings per second, tokens per second, and media work units / pixels.
// batch_infer additionally reports both a serial and a batched figure; the
// BATCHED one is the constant's basis, because that is the shape a supplier
// actually serves.
func measuredUnitsPerSecond(section map[string]any) (float64, bool) {
	for _, key := range []string{
		"throughput_eps", "batch_32_tokens_per_second", "throughput_tps",
	} {
		if v, ok := section[key].(float64); ok {
			return v, true
		}
	}
	return 0, false
}

// validateAllRepricingBenchmarkCitations runs the bindability gate over every
// row that is allowed to set a buyer price. Empty table is refused so the
// catalogue cannot silently have no measured basis.
func validateAllRepricingBenchmarkCitations() error {
	if len(repricingBenchmarks) == 0 {
		return fmt.Errorf("no repricing benchmark is declared, so the catalogue has no measured basis")
	}
	for _, b := range repricingBenchmarks {
		if err := validateRepricingBenchmarkCitation(b); err != nil {
			return err
		}
	}
	// Quarantined rows must stay out of the priced set until they bind. If a
	// future edit re-adds one without fixing the citation, this still catches
	// "priced and unbindable" by requiring that unpriced candidates fail the
	// bind check (so they have not been quietly fixed without promotion).
	for _, b := range unpricedThroughputUntilBound {
		for _, priced := range repricingBenchmarks {
			if priced.ModelID == b.ModelID && priced.JobType == b.JobType {
				return fmt.Errorf(
					"%s/%s is in both repricingBenchmarks and unpricedThroughputUntilBound; "+
						"a model is either priced or refused, not both",
					b.ModelID, b.JobType)
			}
		}
		if err := validateRepricingBenchmarkCitation(b); err == nil {
			return fmt.Errorf(
				"%s/%s is listed in unpricedThroughputUntilBound but its citation now binds; "+
					"promote it to repricingBenchmarks or remove the stale quarantine entry",
				b.ModelID, b.JobType)
		}
	}
	return nil
}
