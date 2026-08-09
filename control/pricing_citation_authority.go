package main

import (
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"time"
)

const (
	catalogueThroughputFreshnessPolicy = "catalogue-throughput-receipt-v1/max-age-180d/no-future-timestamps"
	catalogueThroughputMaxAge          = benchmarkRevalidationWindow
	cataloguePowerFreshnessPolicy      = "catalogue-power-receipt-v1/max-age-30d/no-future-timestamps"
	cataloguePowerMaxAge               = 30 * 24 * time.Hour
	cataloguePowerAuthorityScope       = "hardware_class_conservative_max_envelope"
	cataloguePowerAggregation          = "maximum_across_covered_workloads"
	cataloguePowerOperatingProtocol    = "catalogue-power-envelope-v1/steady-state-inference-after-warmup"
)

var catalogueThroughputNow = func() time.Time { return time.Now().UTC() }

// cataloguePowerNow is separate from the market-board clock: a test that pins
// an old committed board must not silently make a newly generated power
// receipt old or future-dated. Production uses the actual UTC clock.
var cataloguePowerNow = func() time.Time { return time.Now().UTC() }

// Pricing citation authority.
//
// A catalogue price row that cites an unbindable artifact sets a live buyer
// price from a number nobody can reproduce. Cell routability already refused
// that for media (fdc8eec1); this file is the same gate for catalogue pricing.
//
// Pricing uses the same BOUND producer-identity bar as other production
// authority. Legacy receipts remain readable as diagnostics, but their absence
// of complete identity is not an exception that can set a buyer price.

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
	// The release image has no git and no repo tree; it carries the cited
	// receipts under /etc/merc alongside pricing/board.json. Without this the
	// citation gate makes the container unbootable, which is how it was found.
	out = append(out, "/etc/merc")
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
	_, err := pricingThroughputAuthoritySnapshot(b, catalogueThroughputNow())
	return err
}

// pricingThroughputAuthoritySnapshot both validates current throughput
// authority and returns the exact immutable fields a v3 schedule must freeze.
// Keeping those operations together prevents BuildCataloguePriceSchedule from
// validating one read and serializing facts from another.
func pricingThroughputAuthoritySnapshot(
	b measuredThroughput,
	now time.Time,
) (CatalogueThroughputAuthoritySnapshot, error) {
	if strings.TrimSpace(b.Unit) == "" || strings.TrimSpace(b.UnitScope) == "" {
		return CatalogueThroughputAuthoritySnapshot{}, fmt.Errorf(
			"%s/%s: pricing throughput authority requires explicit unit and unit_scope",
			b.ModelID, b.JobType)
	}
	path, fragment, ok := strings.Cut(b.SourceCitation, "#")
	if !ok || strings.TrimSpace(fragment) == "" {
		return CatalogueThroughputAuthoritySnapshot{}, fmt.Errorf("%s/%s: citation %q names no fragment",
			b.ModelID, b.JobType, b.SourceCitation)
	}
	profile, cell, summary, err := currentRuntimeCellBenchmarkIdentity(b.RuntimeCellID)
	if err != nil {
		return CatalogueThroughputAuthoritySnapshot{}, fmt.Errorf("%s/%s: %w", b.ModelID, b.JobType, err)
	}
	authorityPath := cell.benchmarkAuthorityFor(profile)
	full, err := resolveCitedEvidencePath(path)
	if err != nil {
		return CatalogueThroughputAuthoritySnapshot{}, fmt.Errorf("%s/%s: %w", b.ModelID, b.JobType, err)
	}
	authorityFull, err := resolveCitedEvidencePath(authorityPath)
	if err != nil {
		return CatalogueThroughputAuthoritySnapshot{}, fmt.Errorf("%s/%s: resolve exact runtime-cell benchmark authority: %w", b.ModelID, b.JobType, err)
	}
	fullAbs, _ := filepath.Abs(full)
	authorityAbs, _ := filepath.Abs(authorityFull)
	if filepath.Clean(fullAbs) != filepath.Clean(authorityAbs) {
		return CatalogueThroughputAuthoritySnapshot{}, fmt.Errorf(
			"%s/%s: citation %q is not exact selected cell %s benchmark authority %q",
			b.ModelID, b.JobType, b.SourceCitation, b.RuntimeCellID, authorityPath)
	}
	raw, err := os.ReadFile(full)
	if err != nil {
		return CatalogueThroughputAuthoritySnapshot{}, fmt.Errorf("%s/%s: read cited receipt: %w", b.ModelID, b.JobType, err)
	}
	var receipt map[string]any
	if err := json.Unmarshal(raw, &receipt); err != nil {
		return CatalogueThroughputAuthoritySnapshot{}, fmt.Errorf("%s/%s: cited receipt is not JSON: %w", b.ModelID, b.JobType, err)
	}
	if err := ensurePricingCitationBindable(path, receipt); err != nil {
		return CatalogueThroughputAuthoritySnapshot{}, fmt.Errorf("%s/%s: %w", b.ModelID, b.JobType, err)
	}
	if !engineBuildHashPattern.MatchString(b.EngineBuildHash) ||
		!validCurrentEngineBuildIdentityPolicy(b.EngineBuildIdentityPolicy) ||
		!validCanonicalHardwareIdentity(b.HardwareIdentity) {
		return CatalogueThroughputAuthoritySnapshot{}, fmt.Errorf(
			"%s/%s: pricing throughput row lacks exact canonical build/device identity",
			b.ModelID, b.JobType)
	}
	if summary.EngineBuildHash != b.EngineBuildHash ||
		summary.EngineBuildIdentityPolicy != b.EngineBuildIdentityPolicy ||
		summary.HardwareIdentity != b.HardwareIdentity || summary.HWClass != b.HWClass {
		return CatalogueThroughputAuthoritySnapshot{}, fmt.Errorf(
			"%s/%s: pricing row build/device/class %q/%q/%q does not equal selected cell benchmark %q/%q/%q",
			b.ModelID, b.JobType, b.EngineBuildHash, b.HardwareIdentity, b.HWClass,
			summary.EngineBuildHash, summary.HardwareIdentity, summary.HWClass)
	}
	if got := pricingReceiptEngineBuildHash(receipt); got != b.EngineBuildHash {
		return CatalogueThroughputAuthoritySnapshot{}, fmt.Errorf(
			"%s/%s: cited receipt engine build %q does not equal row %q",
			b.ModelID, b.JobType, got, b.EngineBuildHash)
	}
	if got := pricingReceiptEngineBuildIdentityPolicy(receipt); got != b.EngineBuildIdentityPolicy {
		return CatalogueThroughputAuthoritySnapshot{}, fmt.Errorf(
			"%s/%s: cited receipt build identity policy %q does not equal row %q",
			b.ModelID, b.JobType, got, b.EngineBuildIdentityPolicy)
	}
	if got := pricingReceiptHardwareIdentity(receipt); got != b.HardwareIdentity {
		return CatalogueThroughputAuthoritySnapshot{}, fmt.Errorf(
			"%s/%s: cited receipt hardware identity %q does not equal row %q",
			b.ModelID, b.JobType, got, b.HardwareIdentity)
	}
	validUntil, err := pricingThroughputValidUntil(receipt, now)
	if err != nil {
		return CatalogueThroughputAuthoritySnapshot{}, fmt.Errorf("%s/%s: %w", b.ModelID, b.JobType, err)
	}
	if got, _ := receipt["hardware_class"].(string); got != b.HWClass {
		return CatalogueThroughputAuthoritySnapshot{}, fmt.Errorf("%s/%s: constant claims hardware class %q, receipt measured %q",
			b.ModelID, b.JobType, b.HWClass, got)
	}
	section, ok := receipt[fragment].(map[string]any)
	if !ok {
		return CatalogueThroughputAuthoritySnapshot{}, fmt.Errorf("%s/%s: cited receipt has no %q section",
			b.ModelID, b.JobType, fragment)
	}
	if err := validatePricingThroughputRuntimeCellIdentity(b, section); err != nil {
		return CatalogueThroughputAuthoritySnapshot{}, err
	}
	if got, _ := section["engine_build_hash"].(string); got != b.EngineBuildHash {
		return CatalogueThroughputAuthoritySnapshot{}, fmt.Errorf(
			"%s/%s: receipt section engine_build_hash %q does not equal row %q",
			b.ModelID, b.JobType, got, b.EngineBuildHash)
	}
	if got, _ := section["engine_build_identity_policy"].(string); got != b.EngineBuildIdentityPolicy {
		return CatalogueThroughputAuthoritySnapshot{}, fmt.Errorf(
			"%s/%s: receipt section engine_build_identity_policy %q does not equal row %q",
			b.ModelID, b.JobType, got, b.EngineBuildIdentityPolicy)
	}
	if got, _ := section["hardware_identity"].(string); got != b.HardwareIdentity {
		return CatalogueThroughputAuthoritySnapshot{}, fmt.Errorf(
			"%s/%s: receipt section hardware_identity %q does not equal row %q",
			b.ModelID, b.JobType, got, b.HardwareIdentity)
	}
	model, _ := section["model"].(string)
	if strings.TrimSpace(model) == "" {
		return CatalogueThroughputAuthoritySnapshot{}, fmt.Errorf("%s/%s: receipt section %q requires an explicit model identity",
			b.ModelID, b.JobType, fragment)
	}
	if model != b.ModelID {
		return CatalogueThroughputAuthoritySnapshot{}, fmt.Errorf("%s/%s: constant claims model %q, receipt measured %q",
			b.ModelID, b.JobType, b.ModelID, model)
	}
	identity, err := pricingCitationIdentity(receipt)
	if err != nil {
		return CatalogueThroughputAuthoritySnapshot{}, fmt.Errorf("%s/%s: %w", b.ModelID, b.JobType, err)
	}
	producerModelDigest := strings.TrimSpace(identity.ModelArtifactDigest.Value)
	rowModelDigest := strings.TrimSpace(b.ModelArtifactDigest)
	sectionModelDigest, _ := section["model_artifact_digest"].(string)
	sectionModelDigest = strings.TrimSpace(sectionModelDigest)
	if !identity.ModelArtifactDigest.Present() || !digestPattern.MatchString(producerModelDigest) {
		return CatalogueThroughputAuthoritySnapshot{}, fmt.Errorf(
			"%s/%s: pricing producer_identity.model_artifact_digest requires an actual lowercase sha256 value; N/A or blank cannot select a model price",
			b.ModelID, b.JobType)
	}
	if !digestPattern.MatchString(rowModelDigest) {
		return CatalogueThroughputAuthoritySnapshot{}, fmt.Errorf("%s/%s: pricing throughput row requires an exact lowercase sha256 model_artifact_digest",
			b.ModelID, b.JobType)
	}
	if !digestPattern.MatchString(sectionModelDigest) {
		return CatalogueThroughputAuthoritySnapshot{}, fmt.Errorf("%s/%s: receipt section %q requires an exact lowercase sha256 model_artifact_digest",
			b.ModelID, b.JobType, fragment)
	}
	if producerModelDigest != rowModelDigest || sectionModelDigest != rowModelDigest {
		return CatalogueThroughputAuthoritySnapshot{}, fmt.Errorf(
			"%s/%s: model_artifact_digest mismatch: row=%s receipt_section=%s producer_identity=%s",
			b.ModelID, b.JobType, rowModelDigest, sectionModelDigest, producerModelDigest)
	}
	if !modelArtifactDigestsBound(b.ModelID, []string{rowModelDigest}) {
		return CatalogueThroughputAuthoritySnapshot{}, fmt.Errorf(
			"%s/%s: model_artifact_digest %s does not match a canonical weight artifact pinned by runtime authority",
			b.ModelID, b.JobType, rowModelDigest)
	}
	receiptUnit, _ := section["unit"].(string)
	receiptUnitScope, _ := section["unit_scope"].(string)
	if strings.TrimSpace(receiptUnit) == "" || strings.TrimSpace(receiptUnitScope) == "" {
		return CatalogueThroughputAuthoritySnapshot{}, fmt.Errorf(
			"%s/%s: receipt section %q requires explicit unit and unit_scope",
			b.ModelID, b.JobType, fragment)
	}
	if receiptUnit != b.Unit || receiptUnitScope != b.UnitScope {
		return CatalogueThroughputAuthoritySnapshot{}, fmt.Errorf(
			"%s/%s: pricing throughput authority claims %q/%q, receipt measured %q/%q",
			b.ModelID, b.JobType, b.Unit, b.UnitScope, receiptUnit, receiptUnitScope)
	}
	settlement, ok := currentSettlementAuthorityForJobType(b.JobType)
	if !ok {
		return CatalogueThroughputAuthoritySnapshot{}, fmt.Errorf("%s/%s: no governed settlement unit authority", b.ModelID, b.JobType)
	}
	if b.Unit != settlement.Unit || b.UnitScope != settlement.Scope {
		return CatalogueThroughputAuthoritySnapshot{}, fmt.Errorf(
			"%s/%s: pricing throughput authority %q/%q is incompatible with current settlement authority %q/%q; no exact frozen conversion authority is present",
			b.ModelID, b.JobType, b.Unit, b.UnitScope, settlement.Unit, settlement.Scope)
	}
	measured, ok := measuredUnitsPerSecond(section)
	if !ok {
		return CatalogueThroughputAuthoritySnapshot{}, fmt.Errorf("%s/%s: receipt section %q reports no recognised throughput",
			b.ModelID, b.JobType, fragment)
	}
	benchmarkMeasurement, ok := summary.Throughput[b.RuntimeProfileID]
	if !ok || benchmarkMeasurement.Unit != b.Unit || benchmarkMeasurement.UnitScope != b.UnitScope ||
		benchmarkMeasurement.UnitsPerSecAtOperatingBatch != b.UnitsPerSec || measured != b.UnitsPerSec ||
		summary.MeasuredAt != strings.TrimSpace(stringField(receipt, "measured_at")) {
		return CatalogueThroughputAuthoritySnapshot{}, fmt.Errorf(
			"%s/%s: pricing citation does not exactly equal selected cell benchmark unit/scope/time/observed rate",
			b.ModelID, b.JobType)
	}
	// Conservative bound only: constant may sit slightly below the measurement,
	// never above it. Same rule as TestEveryRepricingBenchmarkMatchesItsCitedReceipt.
	const maxConservativeShortfall = 0.01
	switch {
	case b.UnitsPerSec > measured+1e-4:
		return CatalogueThroughputAuthoritySnapshot{}, fmt.Errorf("%s/%s: constant %.6f units/sec exceeds cited measurement %.6f",
			b.ModelID, b.JobType, b.UnitsPerSec, measured)
	case b.UnitsPerSec < measured*(1-maxConservativeShortfall):
		return CatalogueThroughputAuthoritySnapshot{}, fmt.Errorf("%s/%s: constant %.6f units/sec is more than 1%% below cited measurement %.6f",
			b.ModelID, b.JobType, b.UnitsPerSec, measured)
	}
	exactPins, err := exactWeightDigestsForCell(cell, runtimeAuthorityModels)
	if err != nil {
		return CatalogueThroughputAuthoritySnapshot{}, err
	}
	projectedSummary := projectBenchmarkSummaryToExactCell(summary, exactPins)
	canonicalCommit, err := canonicalFrozenMercSourceCommit(projectedSummary.MercSourceCommit)
	if err != nil {
		return CatalogueThroughputAuthoritySnapshot{}, err
	}
	projectedSummary.MercSourceCommit = canonicalCommit
	summarySHA, err := benchmarkReceiptSummarySHA256(projectedSummary)
	if err != nil {
		return CatalogueThroughputAuthoritySnapshot{}, err
	}
	measuredAt, _ := time.Parse(time.RFC3339, strings.TrimSpace(stringField(receipt, "measured_at")))
	return CatalogueThroughputAuthoritySnapshot{
		Citation:                   strings.TrimSpace(b.SourceCitation),
		ReceiptSHA256:              fmt.Sprintf("%x", sha256.Sum256(raw)),
		BenchmarkSummarySHA256:     summarySHA,
		EngineBuildHash:            b.EngineBuildHash,
		EngineBuildIdentityPolicy:  b.EngineBuildIdentityPolicy,
		HardwareIdentity:           b.HardwareIdentity,
		FreshnessPolicy:            catalogueThroughputFreshnessPolicy,
		MeasuredAt:                 measuredAt.UTC().Format(time.RFC3339),
		ValidUntil:                 validUntil.UTC().Format(time.RFC3339),
		ObservedUnitsPerSecond:     b.UnitsPerSec,
		HaircutPolicyRevision:      runtimeCellPerformancePolicyRevision,
		Haircut:                    measuredThroughputHaircut,
		ConservativeUnitsPerSecond: catalogueConservativeUnitsPerSecond(b),
	}, nil
}

func pricingReceiptEngineBuildHash(receipt map[string]any) string {
	for _, key := range []string{"engine_build_hash", "agent_build_hash", "build_hash"} {
		if value, _ := receipt[key].(string); strings.TrimSpace(value) != "" {
			return strings.TrimSpace(value)
		}
	}
	if raw, ok := receipt["raw_measurement"].(map[string]any); ok {
		value, _ := raw["build_hash"].(string)
		return strings.TrimSpace(value)
	}
	return ""
}

func pricingReceiptEngineBuildIdentityPolicy(receipt map[string]any) string {
	if value, _ := receipt["engine_build_identity_policy"].(string); strings.TrimSpace(value) != "" {
		return strings.TrimSpace(value)
	}
	if raw, ok := receipt["raw_measurement"].(map[string]any); ok {
		value, _ := raw["build_identity_policy"].(string)
		return strings.TrimSpace(value)
	}
	return ""
}

func pricingReceiptHardwareIdentity(receipt map[string]any) string {
	for _, key := range []string{"hardware_identity", "device_label"} {
		if value, _ := receipt[key].(string); strings.TrimSpace(value) != "" {
			return canonicalHardwareIdentity(value)
		}
	}
	if hardware, ok := receipt["hardware"].(map[string]any); ok {
		value, _ := hardware["gpu"].(string)
		return canonicalHardwareIdentity(value)
	}
	return ""
}

func validatePricingThroughputRuntimeCellIdentity(
	b measuredThroughput,
	section map[string]any,
) error {
	for field, want := range map[string]string{
		"runtime_cell_id":    b.RuntimeCellID,
		"runtime_profile_id": b.RuntimeProfileID,
		"profile_revision":   b.ProfileRevision,
		"engine":             b.Engine,
		"engine_revision":    b.EngineRevision,
	} {
		got, _ := section[field].(string)
		if got != want {
			return fmt.Errorf(
				"%s/%s: pricing throughput %s %q does not equal receipt value %q",
				b.ModelID, b.JobType, field, want, got)
		}
	}
	if b.RuntimeCellID == "" || b.RuntimeProfileID == "" || b.ProfileRevision == "" || b.Engine == "" {
		return fmt.Errorf("%s/%s: pricing throughput runtime-cell identity is incomplete",
			b.ModelID, b.JobType)
	}
	if err := validatePricingRuntimeCellModelArtifact(b); err != nil {
		return err
	}
	for _, profile := range runtimeAuthority.Runtimes {
		if profile.RuntimeID != b.RuntimeProfileID || profile.Revision != b.ProfileRevision ||
			profile.Engine != b.Engine || profile.EngineRevision != b.EngineRevision {
			continue
		}
		hardwareCovered := false
		for _, hardware := range profile.Hardware.Platforms {
			if hardware == b.HWClass {
				hardwareCovered = true
				break
			}
		}
		if !hardwareCovered {
			return fmt.Errorf("%s/%s: runtime profile %s/%s does not cover hardware class %s",
				b.ModelID, b.JobType, b.RuntimeProfileID, b.ProfileRevision, b.HWClass)
		}
		for _, cell := range profile.Cells {
			if cell.ID != b.RuntimeCellID {
				continue
			}
			if cell.Job != b.JobType || cell.Model != b.ModelID {
				return fmt.Errorf(
					"%s/%s: runtime cell %s currently binds %s/%s",
					b.ModelID, b.JobType, b.RuntimeCellID, cell.Model, cell.Job)
			}
			if ok, reason := cellAuthorityBindable(profile, cell); !ok {
				return fmt.Errorf(
					"%s/%s: runtime cell %s is not bindable for catalogue publication: %s",
					b.ModelID, b.JobType, b.RuntimeCellID, reason)
			}
			if !cell.Routable(profile) {
				return fmt.Errorf("%s/%s: runtime cell %s is not routable",
					b.ModelID, b.JobType, b.RuntimeCellID)
			}
			return nil
		}
	}
	return fmt.Errorf(
		"%s/%s: pricing throughput runtime cell %s/%s@%s engine %s@%s does not resolve to current runtime authority",
		b.ModelID, b.JobType, b.RuntimeProfileID, b.RuntimeCellID,
		b.ProfileRevision, b.Engine, b.EngineRevision)
}

// validatePricingRuntimeCellModelArtifact narrows a model-level weight pin to
// the exact bytes the selected cell can load. Multi-format models can pin both
// safetensors and GGUF; a Candle measurement cannot authorize the sibling
// llama.cpp artifact merely because both belong to the same logical model.
func validatePricingRuntimeCellModelArtifact(b measuredThroughput) error {
	model, ok := runtimeAuthorityModels[b.ModelID]
	if !ok {
		return fmt.Errorf("%s/%s: model is absent from current runtime authority",
			b.ModelID, b.JobType)
	}
	for _, profile := range runtimeAuthority.Runtimes {
		if profile.RuntimeID != b.RuntimeProfileID || profile.Revision != b.ProfileRevision ||
			profile.Engine != b.Engine || profile.EngineRevision != b.EngineRevision {
			continue
		}
		for _, cell := range profile.Cells {
			if cell.ID != b.RuntimeCellID {
				continue
			}
			if cell.Model != b.ModelID || cell.Job != b.JobType {
				return fmt.Errorf(
					"%s/%s: runtime cell %s currently binds %s/%s",
					b.ModelID, b.JobType, b.RuntimeCellID, cell.Model, cell.Job)
			}
			wireKind := wireKindFor(cell, model.WireKind)
			weightDigests, err := exactWeightDigestsForCell(cell, runtimeAuthorityModels)
			if err != nil {
				return fmt.Errorf("%s/%s: exact runtime-cell model artifact authority: %w",
					b.ModelID, b.JobType, err)
			}
			if len(weightDigests) != 1 {
				return fmt.Errorf(
					"%s/%s: exact cell %s wire kind %s resolves %d weight artifacts, but catalogue physical authority requires one exact model_artifact_digest",
					b.ModelID, b.JobType, b.RuntimeCellID, wireKind, len(weightDigests))
			}
			if weightDigests[0] == b.ModelArtifactDigest {
				return nil
			}
			return fmt.Errorf(
				"%s/%s: model_artifact_digest %s does not match a canonical weight artifact pinned by runtime authority for exact cell %s wire kind %s (allowed %v)",
				b.ModelID, b.JobType, b.ModelArtifactDigest, b.RuntimeCellID, wireKind, weightDigests)
		}
	}
	return fmt.Errorf(
		"%s/%s: cannot resolve exact runtime cell %s for model artifact authority",
		b.ModelID, b.JobType, b.RuntimeCellID)
}

// ensurePricingCitationBindable is the load-bearing check: only a BOUND receipt
// with complete formal producer identity and a real producer source commit can
// set a price.
//
// Rules:
//  1. binding_status must be exactly BOUND and the receipt must not carry a
//     terminal validity marker.
//  2. All eight producer_identity slots must contain a value or explicit N/A.
//  3. producer_identity.source_commit must contain a real git commit value; N/A
//     is not valid for an authorizing measurement.
//  4. A legacy merc_source_commit, when also present, must be valid and must
//     identify the same object as producer_identity.source_commit.
func ensurePricingCitationBindable(_ string, receipt map[string]any) error {
	status, _ := receipt["binding_status"].(string)
	if strings.TrimSpace(status) != BindingBound {
		if strings.TrimSpace(status) == "" {
			status = "missing"
		}
		return fmt.Errorf("cited artifact is unbindable: binding_status=%s, require BOUND", status)
	}
	if reason := authorityValidityRefusal(stringField(receipt, "validity")); reason != "" {
		return fmt.Errorf("cited artifact is unbindable: validity=%s", reason)
	}
	if missing, ok := receipt["missing_identity_fields"].([]any); ok && len(missing) > 0 {
		return fmt.Errorf("cited artifact is unbindable: BOUND receipt declares missing_identity_fields")
	}
	identity, err := pricingCitationIdentity(receipt)
	if err != nil {
		return err
	}
	if missing := identity.IncompleteFields(); len(missing) > 0 {
		return fmt.Errorf("cited artifact is unbindable: incomplete producer_identity: %s",
			strings.Join(missing, ", "))
	}
	if !identity.SourceCommit.Present() {
		return fmt.Errorf("cited artifact is unbindable: producer_identity.source_commit requires a value")
	}
	producerCommit := strings.TrimSpace(identity.SourceCommit.Value)
	if err := validateMercSourceCommit(producerCommit); err != nil {
		return fmt.Errorf("cited artifact is unbindable: producer_identity.source_commit: %w", err)
	}
	if legacyCommit, ok := receipt["merc_source_commit"].(string); ok {
		legacyCommit = strings.TrimSpace(legacyCommit)
		if err := validateMercSourceCommit(legacyCommit); err != nil {
			return fmt.Errorf("cited artifact is unbindable: %w", err)
		}
		if legacyCommit != producerCommit {
			return fmt.Errorf(
				"cited artifact is unbindable: merc_source_commit %q disagrees with producer_identity.source_commit %q",
				legacyCommit, producerCommit)
		}
	}
	return nil
}

func pricingCitationIdentity(receipt map[string]any) (ReceiptIdentity, error) {
	pi, ok := receipt["producer_identity"].(map[string]any)
	if !ok {
		return ReceiptIdentity{}, fmt.Errorf("cited artifact is unbindable: producer_identity is missing")
	}
	rawIdentity, err := json.Marshal(pi)
	if err != nil {
		return ReceiptIdentity{}, fmt.Errorf("cited artifact is unbindable: encode producer_identity: %w", err)
	}
	var identity ReceiptIdentity
	if err := json.Unmarshal(rawIdentity, &identity); err != nil {
		return ReceiptIdentity{}, fmt.Errorf("cited artifact is unbindable: decode producer_identity: %w", err)
	}
	return identity, nil
}

func stringField(receipt map[string]any, key string) string {
	value, _ := receipt[key].(string)
	return value
}

// pricingThroughputValidUntil applies the same 180-day revalidation window as
// runtime-cell admission and returns the exact expiry boundary. Schedule
// persistence can freeze this boundary rather than re-reading wall-clock
// receipt state during historical replay.
func pricingThroughputValidUntil(receipt map[string]any, now time.Time) (time.Time, error) {
	if policy, _ := receipt["freshness_policy"].(string); policy != catalogueThroughputFreshnessPolicy {
		return time.Time{}, fmt.Errorf(
			"catalogue publication throughput receipt requires freshness_policy=%q, got %q",
			catalogueThroughputFreshnessPolicy, policy)
	}
	measuredAtText, _ := receipt["measured_at"].(string)
	measuredAt, err := time.Parse(time.RFC3339, strings.TrimSpace(measuredAtText))
	if err != nil {
		return time.Time{}, fmt.Errorf(
			"catalogue publication throughput receipt requires an explicit RFC3339 measured_at: %v", err)
	}
	measuredAt = measuredAt.UTC()
	now = now.UTC()
	if measuredAt.After(now) {
		return time.Time{}, fmt.Errorf(
			"catalogue publication throughput receipt is future-dated under %s: measured_at=%s now=%s",
			catalogueThroughputFreshnessPolicy, measuredAt.Format(time.RFC3339), now.Format(time.RFC3339))
	}
	validUntil := measuredAt.Add(catalogueThroughputMaxAge)
	if now.After(validUntil) {
		return time.Time{}, fmt.Errorf(
			"catalogue publication throughput receipt is stale under %s: measured_at=%s valid_until=%s",
			catalogueThroughputFreshnessPolicy, measuredAt.Format(time.RFC3339), validUntil.Format(time.RFC3339))
	}
	return validUntil, nil
}

// validatePricingPowerCitation turns a MEASURED table row into weight-bearing
// publication authority only when the exact receipt bytes are pinned and the
// BOUND receipt says exactly what the row says. Free-form provenance remains
// useful for diagnostics but cannot authorize a buyer price.
func validatePricingPowerCitation(
	b measuredThroughput,
	entry governedSustainedWatts,
	now time.Time,
) error {
	_, err := pricingPowerAuthoritySnapshot(b, entry, now)
	return err
}

func pricingPowerAuthoritySnapshot(
	b measuredThroughput,
	entry governedSustainedWatts,
	now time.Time,
) (CataloguePowerAuthoritySnapshot, error) {
	hwClass := b.HWClass
	path, fragment, ok := strings.Cut(entry.Provenance(), "#")
	if !ok || strings.TrimSpace(fragment) == "" {
		return CataloguePowerAuthoritySnapshot{}, fmt.Errorf(
			"catalogue publication MEASURED power authority for %q requires a cited receipt section, got %q",
			hwClass, entry.Provenance())
	}
	expectedDigest := strings.TrimSpace(entry.ReceiptSHA256())
	if !digestPattern.MatchString(expectedDigest) {
		return CataloguePowerAuthoritySnapshot{}, fmt.Errorf(
			"catalogue publication MEASURED power authority for %q requires a pinned lowercase sha256 receipt digest",
			hwClass)
	}
	full, err := resolveCitedEvidencePath(path)
	if err != nil {
		return CataloguePowerAuthoritySnapshot{}, fmt.Errorf("catalogue publication power authority for %q: %w", hwClass, err)
	}
	raw, err := os.ReadFile(full)
	if err != nil {
		return CataloguePowerAuthoritySnapshot{}, fmt.Errorf("catalogue publication power authority for %q: read cited receipt: %w", hwClass, err)
	}
	actualDigest := fmt.Sprintf("%x", sha256.Sum256(raw))
	if actualDigest != expectedDigest {
		return CataloguePowerAuthoritySnapshot{}, fmt.Errorf(
			"catalogue publication power receipt digest mismatch for %q: pinned=%s actual=%s",
			hwClass, expectedDigest, actualDigest)
	}
	var receipt map[string]any
	if err := json.Unmarshal(raw, &receipt); err != nil {
		return CataloguePowerAuthoritySnapshot{}, fmt.Errorf("catalogue publication power authority for %q: cited receipt is not JSON: %w", hwClass, err)
	}
	if err := ensurePricingCitationBindable(path, receipt); err != nil {
		return CataloguePowerAuthoritySnapshot{}, fmt.Errorf("catalogue publication power authority for %q: %w", hwClass, err)
	}
	if !engineBuildHashPattern.MatchString(b.EngineBuildHash) ||
		!validCurrentEngineBuildIdentityPolicy(b.EngineBuildIdentityPolicy) ||
		!validCanonicalHardwareIdentity(b.HardwareIdentity) {
		return CataloguePowerAuthoritySnapshot{}, fmt.Errorf(
			"catalogue publication power authority for %q requires exact row build/device identity", hwClass)
	}
	if rootHW, _ := receipt["hardware_class"].(string); rootHW != hwClass {
		return CataloguePowerAuthoritySnapshot{}, fmt.Errorf(
			"catalogue publication power authority claims hardware class %q, receipt measured %q",
			hwClass, rootHW)
	}
	if got := pricingReceiptEngineBuildHash(receipt); got != b.EngineBuildHash {
		return CataloguePowerAuthoritySnapshot{}, fmt.Errorf(
			"catalogue publication power receipt engine build %q does not equal selected cell build %q",
			got, b.EngineBuildHash)
	}
	if got := pricingReceiptEngineBuildIdentityPolicy(receipt); got != b.EngineBuildIdentityPolicy {
		return CataloguePowerAuthoritySnapshot{}, fmt.Errorf(
			"catalogue publication power receipt build identity policy %q does not equal selected cell policy %q",
			got, b.EngineBuildIdentityPolicy)
	}
	if got := pricingReceiptHardwareIdentity(receipt); got != b.HardwareIdentity {
		return CataloguePowerAuthoritySnapshot{}, fmt.Errorf(
			"catalogue publication power receipt hardware identity %q does not equal selected cell device %q",
			got, b.HardwareIdentity)
	}
	section, ok := receipt[fragment].(map[string]any)
	if !ok {
		return CataloguePowerAuthoritySnapshot{}, fmt.Errorf("catalogue publication power receipt has no %q section", fragment)
	}
	if measuredHW, _ := section["hardware_class"].(string); measuredHW != hwClass {
		return CataloguePowerAuthoritySnapshot{}, fmt.Errorf(
			"catalogue publication power authority claims hardware class %q, receipt section measured %q",
			hwClass, measuredHW)
	}
	if got, _ := section["engine_build_hash"].(string); got != b.EngineBuildHash {
		return CataloguePowerAuthoritySnapshot{}, fmt.Errorf(
			"catalogue publication power section engine build %q does not equal selected cell build %q",
			got, b.EngineBuildHash)
	}
	if got, _ := section["engine_build_identity_policy"].(string); got != b.EngineBuildIdentityPolicy {
		return CataloguePowerAuthoritySnapshot{}, fmt.Errorf(
			"catalogue publication power section build identity policy %q does not equal selected cell policy %q",
			got, b.EngineBuildIdentityPolicy)
	}
	if got, _ := section["hardware_identity"].(string); got != b.HardwareIdentity {
		return CataloguePowerAuthoritySnapshot{}, fmt.Errorf(
			"catalogue publication power section hardware identity %q does not equal selected cell device %q",
			got, b.HardwareIdentity)
	}
	if status, _ := section["measurement_status"].(string); status != string(wattKindMeasured) {
		return CataloguePowerAuthoritySnapshot{}, fmt.Errorf(
			"catalogue publication power receipt for %q requires measurement_status=MEASURED, got %q",
			hwClass, status)
	}
	const (
		measurementBoundary = "whole_package"
		workloadClass       = "inference_shaped"
		powerUnit           = "watts"
	)
	if got, _ := section["measurement_boundary"].(string); got != measurementBoundary {
		return CataloguePowerAuthoritySnapshot{}, fmt.Errorf(
			"catalogue publication power receipt for %q requires measurement_boundary=%q, got %q",
			hwClass, measurementBoundary, got)
	}
	if got, _ := section["workload_class"].(string); got != workloadClass {
		return CataloguePowerAuthoritySnapshot{}, fmt.Errorf(
			"catalogue publication power receipt for %q requires workload_class=%q, got %q",
			hwClass, workloadClass, got)
	}
	if got, _ := section["unit"].(string); got != powerUnit {
		return CataloguePowerAuthoritySnapshot{}, fmt.Errorf(
			"catalogue publication power receipt for %q requires unit=%q, got %q",
			hwClass, powerUnit, got)
	}
	if got, _ := section["authority_scope"].(string); got != cataloguePowerAuthorityScope {
		return CataloguePowerAuthoritySnapshot{}, fmt.Errorf(
			"catalogue publication power receipt for %q requires authority_scope=%q, got %q",
			hwClass, cataloguePowerAuthorityScope, got)
	}
	if got, _ := section["aggregation"].(string); got != cataloguePowerAggregation {
		return CataloguePowerAuthoritySnapshot{}, fmt.Errorf(
			"catalogue publication power receipt for %q requires aggregation=%q, got %q",
			hwClass, cataloguePowerAggregation, got)
	}
	if got, _ := section["operating_protocol"].(string); got != cataloguePowerOperatingProtocol {
		return CataloguePowerAuthoritySnapshot{}, fmt.Errorf(
			"catalogue publication power receipt for %q requires operating_protocol=%q, got %q",
			hwClass, cataloguePowerOperatingProtocol, got)
	}
	coverageRaw, ok := section["covered_workloads"].([]any)
	if !ok || len(coverageRaw) == 0 {
		return CataloguePowerAuthoritySnapshot{}, fmt.Errorf(
			"catalogue publication power receipt for %q requires explicit covered_workloads", hwClass)
	}
	coverage := make([]CataloguePowerCoveredWorkload, 0, len(coverageRaw))
	coveredTarget := false
	seenCoverage := map[string]bool{}
	for _, rawWorkload := range coverageRaw {
		workload, ok := rawWorkload.(map[string]any)
		if !ok {
			return CataloguePowerAuthoritySnapshot{}, fmt.Errorf(
				"catalogue publication power receipt for %q has malformed covered_workloads", hwClass)
		}
		modelID, _ := workload["model_id"].(string)
		jobType, _ := workload["job_type"].(string)
		modelArtifactDigest, _ := workload["model_artifact_digest"].(string)
		runtimeCellID, _ := workload["runtime_cell_id"].(string)
		runtimeProfileID, _ := workload["runtime_profile_id"].(string)
		engine, _ := workload["engine"].(string)
		engineBuildHash, _ := workload["engine_build_hash"].(string)
		engineBuildIdentityPolicy, _ := workload["engine_build_identity_policy"].(string)
		hardwareIdentity, _ := workload["hardware_identity"].(string)
		if strings.TrimSpace(modelID) == "" || strings.TrimSpace(jobType) == "" ||
			!digestPattern.MatchString(modelArtifactDigest) ||
			strings.TrimSpace(runtimeCellID) == "" || strings.TrimSpace(runtimeProfileID) == "" ||
			strings.TrimSpace(engine) == "" || !engineBuildHashPattern.MatchString(engineBuildHash) ||
			!validCurrentEngineBuildIdentityPolicy(engineBuildIdentityPolicy) ||
			!validCanonicalHardwareIdentity(hardwareIdentity) {
			return CataloguePowerAuthoritySnapshot{}, fmt.Errorf(
				"catalogue publication power receipt for %q has incomplete covered_workloads identity", hwClass)
		}
		coveredBenchmark, benchmarkErr := currentRepricingBenchmark(modelID, jobType)
		if benchmarkErr != nil {
			return CataloguePowerAuthoritySnapshot{}, fmt.Errorf(
				"catalogue publication power receipt for %q has coverage outside the current exact publication set: %w",
				hwClass, benchmarkErr)
		}
		if coveredBenchmark.ModelArtifactDigest != modelArtifactDigest {
			return CataloguePowerAuthoritySnapshot{}, fmt.Errorf(
				"catalogue publication power receipt for %q covers %s/%s with model artifact digest %s outside canonical runtime authority for the exact selected cell (want %s)",
				hwClass, modelID, jobType, modelArtifactDigest,
				coveredBenchmark.ModelArtifactDigest)
		}
		if coveredBenchmark.RuntimeCellID != runtimeCellID ||
			coveredBenchmark.RuntimeProfileID != runtimeProfileID ||
			coveredBenchmark.Engine != engine ||
			coveredBenchmark.EngineBuildHash != engineBuildHash ||
			coveredBenchmark.EngineBuildIdentityPolicy != engineBuildIdentityPolicy ||
			coveredBenchmark.HardwareIdentity != hardwareIdentity ||
			coveredBenchmark.HWClass != hwClass {
			return CataloguePowerAuthoritySnapshot{}, fmt.Errorf(
				"catalogue publication power receipt coverage %s/%s does not equal exact selected runtime/build/device authority",
				modelID, jobType)
		}
		if artifactErr := validatePricingRuntimeCellModelArtifact(coveredBenchmark); artifactErr != nil {
			return CataloguePowerAuthoritySnapshot{}, fmt.Errorf(
				"catalogue publication power receipt for %q covers %s with a model artifact digest outside exact runtime-cell authority: %w",
				hwClass, modelID, artifactErr)
		}
		key := modelID + "\x00" + jobType
		if seenCoverage[key] {
			return CataloguePowerAuthoritySnapshot{}, fmt.Errorf(
				"catalogue publication power receipt for %q repeats covered workload %s/%s",
				hwClass, modelID, jobType)
		}
		seenCoverage[key] = true
		coverage = append(coverage, CataloguePowerCoveredWorkload{
			ModelID: modelID, JobType: jobType, ModelArtifactDigest: modelArtifactDigest,
			RuntimeCellID: runtimeCellID, RuntimeProfileID: runtimeProfileID,
			Engine: engine, EngineBuildHash: engineBuildHash,
			EngineBuildIdentityPolicy: engineBuildIdentityPolicy,
			HardwareIdentity:          hardwareIdentity,
		})
		if modelID == b.ModelID && jobType == b.JobType &&
			modelArtifactDigest == b.ModelArtifactDigest &&
			runtimeCellID == b.RuntimeCellID && runtimeProfileID == b.RuntimeProfileID &&
			engine == b.Engine && engineBuildHash == b.EngineBuildHash &&
			engineBuildIdentityPolicy == b.EngineBuildIdentityPolicy &&
			hardwareIdentity == b.HardwareIdentity {
			coveredTarget = true
		}
	}
	if !coveredTarget {
		return CataloguePowerAuthoritySnapshot{}, fmt.Errorf(
			"catalogue publication power envelope for %q does not cover exact workload %s/%s",
			hwClass, b.ModelID, b.JobType)
	}
	measuredWatts, ok := section["sustained_watts"].(float64)
	if !ok || !finiteNonNegative(measuredWatts) || measuredWatts <= 0 || measuredWatts != entry.Watts() {
		return CataloguePowerAuthoritySnapshot{}, fmt.Errorf(
			"catalogue publication power authority for %q claims %.6f watts, receipt measured %v",
			hwClass, entry.Watts(), section["sustained_watts"])
	}
	if policy, _ := section["freshness_policy"].(string); policy != cataloguePowerFreshnessPolicy {
		return CataloguePowerAuthoritySnapshot{}, fmt.Errorf(
			"catalogue publication power receipt for %q requires freshness_policy=%q, got %q",
			hwClass, cataloguePowerFreshnessPolicy, policy)
	}
	measuredAtText, _ := section["measured_at"].(string)
	measuredAt, err := time.Parse(time.RFC3339, strings.TrimSpace(measuredAtText))
	if err != nil {
		return CataloguePowerAuthoritySnapshot{}, fmt.Errorf(
			"catalogue publication power receipt for %q requires an explicit RFC3339 measured_at: %v",
			hwClass, err)
	}
	now = now.UTC()
	measuredAt = measuredAt.UTC()
	if measuredAt.After(now) {
		return CataloguePowerAuthoritySnapshot{}, fmt.Errorf(
			"catalogue publication power receipt for %q is future-dated under %s: measured_at=%s now=%s",
			hwClass, cataloguePowerFreshnessPolicy, measuredAt.Format(time.RFC3339), now.Format(time.RFC3339))
	}
	validUntil := measuredAt.Add(cataloguePowerMaxAge)
	if now.After(validUntil) {
		return CataloguePowerAuthoritySnapshot{}, fmt.Errorf(
			"catalogue publication power receipt for %q is stale under %s: measured_at=%s age=%s max=%s",
			hwClass, cataloguePowerFreshnessPolicy, measuredAt.Format(time.RFC3339),
			now.Sub(measuredAt), cataloguePowerMaxAge)
	}
	return CataloguePowerAuthoritySnapshot{
		Citation:                  strings.TrimSpace(entry.Provenance()),
		ReceiptSHA256:             actualDigest,
		RuntimeCellID:             b.RuntimeCellID,
		RuntimeProfileID:          b.RuntimeProfileID,
		Engine:                    b.Engine,
		EngineBuildHash:           b.EngineBuildHash,
		EngineBuildIdentityPolicy: b.EngineBuildIdentityPolicy,
		HWClass:                   b.HWClass,
		HardwareIdentity:          b.HardwareIdentity,
		FreshnessPolicy:           cataloguePowerFreshnessPolicy,
		MeasurementBoundary:       measurementBoundary,
		WorkloadClass:             workloadClass,
		Unit:                      powerUnit,
		AuthorityScope:            cataloguePowerAuthorityScope,
		Aggregation:               cataloguePowerAggregation,
		OperatingProtocol:         cataloguePowerOperatingProtocol,
		CoveredWorkloads:          coverage,
		Watts:                     measuredWatts,
		MeasuredAt:                measuredAt.Format(time.RFC3339),
		ValidUntil:                validUntil.UTC().Format(time.RFC3339),
	}, nil
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
		"throughput_units_per_second", "throughput_eps", "batch_32_tokens_per_second", "throughput_tps",
	} {
		if v, ok := section[key].(float64); ok {
			return v, true
		}
	}
	return 0, false
}

func buildCatalogueResultPhysicalAuthority(b measuredThroughput) (CatalogueResultPhysicalAuthority, error) {
	throughput, err := pricingThroughputAuthoritySnapshot(b, catalogueThroughputNow())
	if err != nil {
		return CatalogueResultPhysicalAuthority{}, err
	}
	powerEntry, err := sustainedWattsEntryForPublication(b.HWClass)
	if err != nil {
		return CatalogueResultPhysicalAuthority{}, err
	}
	power, err := pricingPowerAuthoritySnapshot(b, powerEntry, cataloguePowerNow())
	if err != nil {
		return CatalogueResultPhysicalAuthority{}, err
	}
	throughputUntil, _ := time.Parse(time.RFC3339, throughput.ValidUntil)
	powerUntil, _ := time.Parse(time.RFC3339, power.ValidUntil)
	validUntil := throughputUntil
	if powerUntil.Before(validUntil) {
		validUntil = powerUntil
	}
	return CatalogueResultPhysicalAuthority{
		Version:                   catalogueResultPhysicalAuthorityVersion,
		ModelID:                   b.ModelID,
		JobType:                   b.JobType,
		RuntimeCellID:             b.RuntimeCellID,
		RuntimeProfileID:          b.RuntimeProfileID,
		ProfileRevision:           b.ProfileRevision,
		Engine:                    b.Engine,
		EngineRevision:            b.EngineRevision,
		EngineBuildHash:           b.EngineBuildHash,
		EngineBuildIdentityPolicy: b.EngineBuildIdentityPolicy,
		HWClass:                   b.HWClass,
		HardwareIdentity:          b.HardwareIdentity,
		Unit:                      b.Unit,
		UnitScope:                 b.UnitScope,
		ModelArtifactDigest:       b.ModelArtifactDigest,
		Throughput:                throughput,
		Power:                     power,
		ValidUntil:                validUntil.UTC().Format(time.RFC3339),
	}, nil
}

func currentRepricingBenchmark(modelID, jobType string) (measuredThroughput, error) {
	var match measuredThroughput
	matches := 0
	for _, benchmark := range repricingBenchmarks {
		if benchmark.ModelID == modelID && benchmark.JobType == jobType {
			match = benchmark
			matches++
		}
	}
	if matches != 1 {
		return measuredThroughput{}, fmt.Errorf(
			"catalogue physical authority for %s/%s has %d current benchmark declarations, require exactly one",
			modelID, jobType, matches)
	}
	return match, nil
}

// revalidateCataloguePriceSchedulePhysicalCurrent proves only the physical
// half of a current-use schedule. It deliberately does not consult the current
// market board or rebuild buyer prices: an unexpired bound quote keeps its
// accepted monetary promise across a price-only reprice, but it must still
// prove that the exact throughput and power receipt bytes, runtime declaration,
// build, device and freshness authority remain current.
func revalidateCataloguePriceSchedulePhysicalCurrent(schedule CataloguePriceSchedule) error {
	if schedule.Version != cataloguePriceScheduleVersion {
		return fmt.Errorf(
			"catalogue schedule version %d is historical-only; current use requires version %d physical authority",
			schedule.Version, cataloguePriceScheduleVersion)
	}
	if err := validateCataloguePriceSchedule(schedule); err != nil {
		return err
	}
	for _, result := range schedule.Results {
		if err := revalidateCatalogueResultPhysicalCurrent(result); err != nil {
			return err
		}
	}
	return nil
}

// revalidateCatalogueResultPhysicalCurrent re-opens the exact throughput and
// power citations frozen by one result. It reconstructs the two snapshots from
// those immutable citations rather than from today's mutable publication
// tables, then requires byte-for-byte equality. This is the current physical
// check used by an accepted quote: price declarations may change, physical
// receipt identity and freshness may not.
func revalidateCatalogueResultPhysicalCurrent(result RepriceResult) error {
	physical := result.PhysicalAuthority
	if physical.Version != catalogueResultPhysicalAuthorityVersion {
		return fmt.Errorf(
			"catalogue physical authority for %s/%s version %d is historical-only",
			result.ModelID, result.JobType, physical.Version)
	}
	benchmark := measuredThroughput{
		ModelID:                   physical.ModelID,
		ModelArtifactDigest:       physical.ModelArtifactDigest,
		JobType:                   physical.JobType,
		RuntimeCellID:             physical.RuntimeCellID,
		RuntimeProfileID:          physical.RuntimeProfileID,
		ProfileRevision:           physical.ProfileRevision,
		Engine:                    physical.Engine,
		EngineRevision:            physical.EngineRevision,
		EngineBuildHash:           physical.EngineBuildHash,
		EngineBuildIdentityPolicy: physical.EngineBuildIdentityPolicy,
		HardwareIdentity:          physical.HardwareIdentity,
		Unit:                      physical.Unit,
		UnitScope:                 physical.UnitScope,
		UnitsPerSec:               physical.Throughput.ObservedUnitsPerSecond,
		HWClass:                   physical.HWClass,
		SourceCitation:            physical.Throughput.Citation,
	}
	throughput, err := pricingThroughputAuthoritySnapshot(benchmark, catalogueThroughputNow())
	if err != nil {
		return fmt.Errorf("catalogue throughput authority for %s/%s: %w",
			result.ModelID, result.JobType, err)
	}
	if !reflect.DeepEqual(throughput, physical.Throughput) {
		return fmt.Errorf(
			"catalogue throughput authority for %s/%s no longer equals its exact cited bytes",
			result.ModelID, result.JobType)
	}
	powerEntry := wattsMeasured(
		physical.Power.Watts,
		physical.Power.Citation,
		physical.Power.ReceiptSHA256,
	)
	power, err := pricingPowerAuthoritySnapshot(benchmark, powerEntry, cataloguePowerNow())
	if err != nil {
		return fmt.Errorf("catalogue power authority for %s/%s: %w",
			result.ModelID, result.JobType, err)
	}
	if !reflect.DeepEqual(power, physical.Power) {
		return fmt.Errorf(
			"catalogue power authority for %s/%s no longer equals its exact cited bytes",
			result.ModelID, result.JobType)
	}
	return nil
}

// revalidateCataloguePriceScheduleCurrent is deliberately stronger than
// structural digest validation. Apply and current-pointer reads for newly
// minted authority must prove the exact board, throughput and power bytes are
// still current BOUND authority and still match the process's closed
// declarations. Historical schedule lookup never calls this function.
func revalidateCataloguePriceScheduleCurrent(schedule CataloguePriceSchedule) error {
	if err := revalidateCataloguePriceSchedulePhysicalCurrent(schedule); err != nil {
		return err
	}
	if err := revalidateCatalogueBoardCurrent(schedule); err != nil {
		return err
	}
	rebuilt, err := BuildCataloguePriceSchedule()
	if err != nil {
		return fmt.Errorf("rebuild current catalogue authority: %w", err)
	}
	if !reflect.DeepEqual(schedule, rebuilt) {
		return fmt.Errorf("catalogue schedule does not equal the sole current governed derivation")
	}
	return nil
}

func revalidateCatalogueBoardCurrent(schedule CataloguePriceSchedule) error {
	resolved, err := resolvePriceBoard(os.Getenv("MERC_ENV"))
	if err != nil {
		return fmt.Errorf("resolve current catalogue price board: %w", err)
	}
	raw, err := os.ReadFile(resolved.Path)
	if err != nil {
		return fmt.Errorf("read current catalogue price board: %w", err)
	}
	digest, err := verifyPriceBoardDigest(raw, resolved.ExpectedDigest)
	if err != nil {
		return fmt.Errorf("verify current catalogue price board: %w", err)
	}
	if digest != schedule.BoardSHA256 {
		return fmt.Errorf(
			"current catalogue price board digest %s does not equal schedule board digest %s",
			digest, schedule.BoardSHA256)
	}
	var board priceBoard
	if err := json.Unmarshal(raw, &board); err != nil {
		return fmt.Errorf("parse current catalogue price board: %w", err)
	}
	if board.SchemaVersion != schedule.BoardSchemaVersion ||
		board.FetchedAt != schedule.BoardFetchedAt ||
		board.PositioningMultiplier != schedule.PositioningMultiplier {
		return fmt.Errorf("current catalogue price board metadata no longer equals schedule authority")
	}
	current, err := boardAsOfPublication(&board, priceBoardNow())
	if err != nil {
		return fmt.Errorf("current catalogue price board is not publishable: %w", err)
	}
	validUntil, err := catalogueBoardValidUntil(current)
	if err != nil {
		return err
	}
	if schedule.BoardFreshnessPolicy != catalogueBoardFreshnessPolicy ||
		schedule.BoardValidUntil != validUntil.Format(time.RFC3339) {
		return fmt.Errorf("current catalogue price board freshness boundary no longer equals schedule authority")
	}
	return nil
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
