package main

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"math"
	"reflect"
	"slices"
	"sort"
	"strings"
	"time"
)

// What a supplier can actually produce per second, derived from a receipt
// instead of asserted.
//
// This replaces jobTypeThroughput, a two-entry map of bare numbers that decided
// every supplier's offered hourly rate. It said batch_infer was 4 units/s while
// the receipt behind the price board recorded 138.7, and embed was 200 while the
// receipt recorded 1967. Nothing in the build could notice, because the map was
// its own authority: no receipt, no date, no hardware, no unit, and no way for a
// reader to tell a measurement from a placeholder.
//
// The concrete failure that caused: scripts/install.sh writes
// min_payout_usd_per_hr = 0.05, the scheduler filters on
// offered_rate_usd_hr >= min_payout_usd_hr, and the offered rate computed from
// those numbers came out an order of magnitude under the floor. A default
// install claimed no work, silently, forever, and reported no reason.
//
// The binding is per CELL, matching every other piece of runtime evidence in
// this package: llama_cpp_metal's embed cell and its generation cell have
// different receipts, different quality tiers and different lifecycles, so they
// have different throughput too.

const (
	// cellThroughputMeasured: a receipt measured this cell's profile on this
	// cell's model and published a rate at a batch the cell permits.
	cellThroughputMeasured = "MEASURED"
	// cellThroughputStale: the same, but the measurement is older than the
	// revalidation window. It degrades rather than silently passing, because a
	// number nobody has re-taken in six months is a claim about a machine, an
	// engine build and a model artifact that have all moved since.
	cellThroughputStale = "STALE_REQUIRES_REVALIDATION"
	// cellThroughputUnproven: no usable measurement. The rate is the named
	// fallback below, not a plausible-looking guess.
	cellThroughputUnproven = "UNPROVEN_CONSERVATIVE_FALLBACK"
)

const (
	// measuredThroughputHaircut turns a benchmark into something admission may
	// promise. The receipt was taken on one idle machine running one workload
	// with no other tenant; a supplier's box is thermally throttled, shares the
	// GPU with whatever else the owner is doing, and pays for model load and
	// task handoff that the benchmark harness does not.
	measuredThroughputHaircut = 0.70
	// staleThroughputHaircut is what "degraded" means in numbers. Half the fresh
	// haircut: still derived from the measurement, so a fast cell stays faster
	// than a slow one, but no longer good enough to carry a marginal cell over
	// an admission floor on the strength of a stale run.
	staleThroughputHaircut = 0.35
	// benchmarkRevalidationWindow is how long a runtime measurement is allowed
	// to stand unreviewed. Six months is one engine major and several model
	// artifact revisions in this ecosystem.
	benchmarkRevalidationWindow = 180 * 24 * time.Hour
	// unprovenFallbackUnitsPerSec is the ONLY rate in this file with no
	// measurement behind it, and it is deliberately too small to clear any
	// realistic payout floor. An unmeasured cell must fail admission and say so,
	// not be offered at a number that looks measured.
	unprovenFallbackUnitsPerSec = 1.0

	// Version 1 predates an execution-build identity and remains readable only
	// as a self-contained historical snapshot. Version 2 is the current
	// benchmark/hardware/build authority. Keeping the policy revisions distinct
	// prevents a legacy rate from becoming current merely because its JSON still
	// decodes after the field was added.
	frozenRuntimeCellPerformanceLegacyVersion  = 1
	frozenRuntimeCellPerformanceVersion        = 2
	runtimeCellPerformanceLegacyPolicyRevision = "runtime-cell-performance-authority-v1"
	runtimeCellPerformancePolicyRevision       = "runtime-cell-performance-authority-v2"
)

// Performance-unit scopes are closed semantic dimensions. A shared label such
// as "tokens" cannot authorize arithmetic when one side measures decode output
// and the other settles token-like input plus a maximum output allowance.
const (
	performanceUnitScopeDecodeOutputTokens              = "decode_output_tokens"
	performanceUnitScopeTokenLikeInputPlusOutputTokens  = "token_like_input_plus_max_output_tokens"
	performanceUnitScopeCompletedEmbeddingRecords       = "completed_embedding_records"
	performanceUnitScopeTokenLikeInputGeometry          = "token_like_input_geometry"
	performanceUnitScopeSingleObjectInputByteQuarters   = "single_object_input_byte_quarters"
	performanceUnitScopeFullInputByteQuartersPerSegment = "full_input_byte_quarters_per_segment"
	performanceUnitScopeDeclaredOutputPixelsPerScene    = "declared_output_pixels_per_scene"
)

// defaultInstallMinPayoutUSDHr is what scripts/install.sh writes into a fresh
// agent config. It is duplicated here so the admission arithmetic can be tested
// against the value a real default install actually carries;
// TestDefaultPayoutFloorMatchesTheInstaller keeps the two in step.
const defaultInstallMinPayoutUSDHr = 0.05

// runtimeCellPerformanceNow is the wall clock used only at current-admission
// boundaries. Historical validators never read it. Keeping the clock at this
// narrow seam also lets the ingress test exercise the exact revalidation
// boundary without sleeping or rewriting a receipt.
var runtimeCellPerformanceNow = time.Now

// RuntimeCellPerformance is the governed performance binding for one runtime
// cell: what it runs, on what, measured by whom, when, and what admission is
// therefore allowed to promise.
type RuntimeCellPerformance struct {
	CellID           string `json:"cell_id"`
	RuntimeProfileID string `json:"runtime_profile_id"`
	ProfileRevision  string `json:"profile_revision"`
	JobType          string `json:"job_type"`
	ModelID          string `json:"model_id"`
	ModelRevision    string `json:"model_revision"`
	WireKind         string `json:"wire_kind"`
	Precision        string `json:"precision"`
	QualityTier      string `json:"quality_tier"`
	Lifecycle        string `json:"lifecycle"`

	// MeasuredOnHWClass is the box the benchmark ran on, not the supplier's.
	MeasuredOnHWClass string   `json:"measured_on_hw_class"`
	HardwareClasses   []string `json:"hardware_classes"`

	OperatingBatch     int `json:"operating_batch"`
	CellMaxBatch       int `json:"cell_max_batch"`
	CellMaxConcurrency int `json:"cell_max_concurrency"`

	BenchmarkAuthority string `json:"benchmark_authority"`
	// EngineBuildHash is the exact source-bound execution build whose measured
	// rate supports the supplier active-hour floor. Workers advertise this same
	// 16-lowerhex value and dispatch must match it exactly.
	EngineBuildHash           string `json:"engine_build_hash,omitempty"`
	EngineBuildIdentityPolicy string `json:"engine_build_identity_policy,omitempty"`
	HardwareIdentity          string `json:"hardware_identity,omitempty"`
	BenchmarkedAt             string `json:"benchmarked_at,omitempty"`
	BenchmarkBasis            string `json:"benchmark_basis,omitempty"`

	Unit string `json:"unit"`
	// UnitScope is the semantic denominator behind Unit. It is copied from the
	// receipt rather than inferred from Unit or free-text benchmark basis.
	UnitScope           string  `json:"unit_scope,omitempty"`
	ObservedUnitsPerSec float64 `json:"observed_units_per_sec"`
	// ObservedBestUnitsPerSec is the best number anywhere in the receipt's
	// sweep. It is NOT necessarily a peak: on a comparison receipt it is the
	// best batch's MEDIAN of five repetitions. It exists to be refused, never
	// reached for.
	ObservedBestUnitsPerSec float64 `json:"observed_best_units_per_sec"`
	Haircut                 float64 `json:"haircut"`
	// ConservativeUnitsPerSec is the only rate any caller may use for money or
	// admission. It is a haircut lower bound, never the best point of a sweep.
	ConservativeUnitsPerSec float64 `json:"conservative_units_per_sec"`

	Status     string  `json:"status"`
	Confidence float64 `json:"confidence"`
	Reason     string  `json:"reason"`
}

// FrozenRuntimeCellPerformance is the self-contained benchmark authority that
// admission accepted for one placement. It carries the embedded receipt summary
// as it existed at acceptance, not merely its mutable manifest path. Historical
// validation therefore checks this immutable snapshot and its digest; only new
// admission consults benchmarkAuthorityManifest and wall-clock freshness.
type FrozenRuntimeCellPerformance struct {
	Version        int    `json:"version"`
	PolicyRevision string `json:"policy_revision"`

	Performance RuntimeCellPerformance `json:"performance"`

	// BenchmarkSnapshot is the cell-exact projection of the embedded receipt
	// summary used to derive Performance. Comparison receipts may cover several
	// runtime arms and artifact formats; the frozen projection retains only this
	// cell's selected weight digests so a sibling canonical format cannot become
	// historical authority by sharing the logical model id.
	BenchmarkSnapshot       benchmarkReceiptSummary `json:"benchmark_snapshot"`
	BenchmarkSnapshotSHA256 string                  `json:"benchmark_snapshot_sha256"`
	// ModelArtifactPins are the exact weight identities the runtime catalogue
	// required when this benchmark was accepted. Historical replay compares the
	// frozen receipt to these frozen pins rather than to a later catalogue.
	ModelArtifactWireKind string   `json:"model_artifact_wire_kind"`
	ModelArtifactPins     []string `json:"model_artifact_pins,omitempty"`

	// Digest covers this entire block with Digest cleared.
	Digest string `json:"digest"`
}

func cloneBenchmarkReceiptSummary(in benchmarkReceiptSummary) benchmarkReceiptSummary {
	out := in
	out.RuntimeProfileIDs = append([]string(nil), in.RuntimeProfileIDs...)
	out.ModelIDs = append([]string(nil), in.ModelIDs...)
	out.ModelArtifactSHA256s = append([]string(nil), in.ModelArtifactSHA256s...)
	if in.Throughput != nil {
		out.Throughput = make(map[string]benchmarkThroughput, len(in.Throughput))
		for key, value := range in.Throughput {
			out.Throughput[key] = value
		}
	}
	return out
}

func benchmarkReceiptSummarySHA256(summary benchmarkReceiptSummary) (string, error) {
	raw, err := json.Marshal(summary)
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(raw)
	return hex.EncodeToString(sum[:]), nil
}

func frozenRuntimeCellPerformanceDigest(binding FrozenRuntimeCellPerformance) (string, error) {
	binding.Digest = ""
	raw, err := json.Marshal(binding)
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(raw)
	return hex.EncodeToString(sum[:]), nil
}

// exactWeightDigestsForCurrentPerformance re-resolves the cell named by a
// current performance projection and returns only the weights selected by that
// cell's wire kind. It is intentionally current-authority code; historical
// validation uses only ModelArtifactWireKind, ModelArtifactPins and the frozen
// benchmark projection below.
func exactWeightDigestsForCurrentPerformance(performance RuntimeCellPerformance) ([]string, error) {
	for _, profile := range runtimeAuthority.Runtimes {
		if profile.RuntimeID != performance.RuntimeProfileID ||
			profile.Revision != performance.ProfileRevision {
			continue
		}
		for _, cell := range profile.Cells {
			if cell.ID != performance.CellID || cell.Job != performance.JobType ||
				cell.Model != performance.ModelID {
				continue
			}
			model, ok := runtimeAuthorityModels[cell.Model]
			if !ok {
				return nil, fmt.Errorf("runtime cell %q names undefined model %q",
					cell.ID, cell.Model)
			}
			kind := wireKindFor(cell, model.WireKind)
			if performance.WireKind != kind {
				return nil, fmt.Errorf(
					"runtime cell %q performance wire kind %q does not match selected authority kind %q",
					cell.ID, performance.WireKind, kind)
			}
			return exactWeightDigestsForCell(cell, runtimeAuthorityModels)
		}
	}
	return nil, fmt.Errorf(
		"runtime cell performance %q/%q does not resolve to current exact artifact authority",
		performance.RuntimeProfileID, performance.CellID)
}

// projectBenchmarkSummaryToExactCell freezes only the selected cell's weight
// identities out of a potentially multi-arm receipt. All other receipt facts
// remain unchanged and continue to be checked by historical replay.
func projectBenchmarkSummaryToExactCell(
	summary benchmarkReceiptSummary, exactPins []string,
) benchmarkReceiptSummary {
	summary = cloneBenchmarkReceiptSummary(summary)
	summary.ModelArtifactSHA256s = append([]string(nil), exactPins...)
	return summary
}

// freezeRuntimeCellPerformance snapshots the current admission authority. This
// is called only while building a new PlacementRequirement; replay must call
// validateFrozenRuntimeCellPerformance and must not resolve today's manifest.
func freezeRuntimeCellPerformance(performance RuntimeCellPerformance) (*FrozenRuntimeCellPerformance, error) {
	if err := validateCurrentRuntimeCellPerformanceAuthority(performance); err != nil {
		return nil, err
	}
	exactPins, err := exactWeightDigestsForCurrentPerformance(performance)
	if err != nil {
		return nil, fmt.Errorf("freeze exact runtime-cell artifact authority: %w", err)
	}
	summary, ok := benchmarkAuthorityManifest[performance.BenchmarkAuthority]
	if !ok {
		return nil, fmt.Errorf("benchmark authority %q is absent from the embedded manifest",
			performance.BenchmarkAuthority)
	}
	summary = projectBenchmarkSummaryToExactCell(summary, exactPins)
	canonicalCommit, err := canonicalFrozenMercSourceCommit(summary.MercSourceCommit)
	if err != nil {
		return nil, fmt.Errorf("freeze benchmark source commit: %w", err)
	}
	summary.MercSourceCommit = canonicalCommit
	summarySHA, err := benchmarkReceiptSummarySHA256(summary)
	if err != nil {
		return nil, fmt.Errorf("digest benchmark authority %q: %w", performance.BenchmarkAuthority, err)
	}
	out := FrozenRuntimeCellPerformance{
		Version:                 frozenRuntimeCellPerformanceVersion,
		PolicyRevision:          runtimeCellPerformancePolicyRevision,
		Performance:             performance,
		BenchmarkSnapshot:       summary,
		BenchmarkSnapshotSHA256: summarySHA,
		ModelArtifactWireKind:   performance.WireKind,
		ModelArtifactPins:       append([]string(nil), exactPins...),
	}
	digest, err := frozenRuntimeCellPerformanceDigest(out)
	if err != nil {
		return nil, fmt.Errorf("digest frozen runtime-cell performance: %w", err)
	}
	out.Digest = digest
	if err := validateFrozenRuntimeCellPerformance(&out); err != nil {
		return nil, err
	}
	return &out, nil
}

// canonicalFrozenMercSourceCommit resolves a short, currently bindable git
// object name once at admission and stores the full object id. Historical
// replay then needs only shape validation and works in production containers
// that intentionally do not ship the repository's .git directory.
func canonicalFrozenMercSourceCommit(commit string) (string, error) {
	commit = strings.TrimSpace(commit)
	if err := validateMercSourceCommit(commit); err != nil {
		return "", err
	}
	if len(commit) == 40 && hexObjectName.MatchString(commit) {
		return strings.ToLower(commit), nil
	}
	for _, root := range []string{".", ".."} {
		resolved, err := gitBytes(root, "rev-parse", commit+"^{commit}")
		if err != nil {
			continue
		}
		full := strings.ToLower(strings.TrimSpace(string(resolved)))
		if len(full) == 40 && hexObjectName.MatchString(full) {
			return full, nil
		}
	}
	return "", fmt.Errorf("merc_source_commit %q could not be canonicalized to a full commit id", commit)
}

// validateCurrentRuntimeCellPerformanceAuthority is a new-admission check. It
// deliberately consults today's runtime catalogue and evidence manifest; the
// frozen validator below deliberately does not. Keeping those roles separate
// prevents a withdrawn receipt from entering a new PlacementRequirement while
// preserving an already accepted historical snapshot after later replacement.
func validateCurrentRuntimeCellPerformanceAuthority(performance RuntimeCellPerformance) error {
	return validateCurrentRuntimeCellPerformanceAuthorityAt(performance, runtimeCellPerformanceNow())
}

// validateCurrentRuntimeCellPerformanceAuthorityAt is the clock-explicit form
// of the new-admission validator. In addition to re-resolving identity, it
// compares the entire current projection. In particular, a quote accepted just
// before the revalidation deadline cannot retain the fresh 0.70 haircut after
// the same receipt crosses into the stale 0.35 posture.
func validateCurrentRuntimeCellPerformanceAuthorityAt(
	performance RuntimeCellPerformance,
	at time.Time,
) error {
	for _, profile := range runtimeAuthority.Runtimes {
		if profile.RuntimeID != performance.RuntimeProfileID ||
			profile.Revision != performance.ProfileRevision {
			continue
		}
		for _, cell := range profile.Cells {
			if cell.ID != performance.CellID || cell.Job != performance.JobType ||
				cell.Model != performance.ModelID {
				continue
			}
			if ok, reason := cellAuthorityBindable(profile, cell); !ok {
				return fmt.Errorf("runtime cell %q benchmark authority is not bindable for new admission: %s",
					performance.CellID, reason)
			}
			if performance.BenchmarkAuthority != cell.benchmarkAuthorityFor(profile) {
				return fmt.Errorf(
					"runtime cell %q now binds benchmark authority %q, not frozen authority %q",
					performance.CellID, cell.benchmarkAuthorityFor(profile),
					performance.BenchmarkAuthority)
			}
			if err := validateCurrentPerformanceSettlementAuthority(performance); err != nil {
				return err
			}
			current := resolveCellPerformance(profile, cell, at)
			// Reason is explanatory prose. In the stale posture it embeds the
			// rounded age in days, so it may change while every authority-bearing
			// field remains identical. Compare the complete typed projection with
			// only that diagnostic field cleared on both sides.
			frozenProjection, currentProjection := performance, current
			frozenProjection.Reason, currentProjection.Reason = "", ""
			if !reflect.DeepEqual(frozenProjection, currentProjection) {
				return fmt.Errorf(
					"runtime cell %q frozen performance projection no longer matches current authority: frozen status=%q haircut=%g conservative_rate=%g, current status=%q haircut=%g conservative_rate=%g",
					performance.CellID,
					performance.Status, performance.Haircut, performance.ConservativeUnitsPerSec,
					current.Status, current.Haircut, current.ConservativeUnitsPerSec)
			}
			return nil
		}
	}
	return fmt.Errorf("runtime cell performance %q/%q does not resolve to the current runtime authority",
		performance.RuntimeProfileID, performance.CellID)
}

type performanceUnitAuthority struct {
	Unit  string
	Scope string
}

// currentSettlementAuthorityForJobType names both the unit and the semantic
// scope a new ComputePlan actually settles for each distributed runtime lane.
// Throughput may enter admission arithmetic only when its receipt measures the
// same pair.
//
// Embed plans currently settle max(records, raw input bytes/4). That is a
// token-like input geometry, not an embedding count: one long record still
// produces one embedding while carrying many settlement units. Until a receipt
// freezes either throughput in that exact geometry or an explicit conversion,
// an embeddings/s benchmark cannot price or predict those units.
func currentSettlementAuthorityForJobType(jobType string) (performanceUnitAuthority, bool) {
	switch jobType {
	case "batch_infer":
		return performanceUnitAuthority{
			Unit: "tokens", Scope: performanceUnitScopeTokenLikeInputPlusOutputTokens,
		}, true
	case "embed":
		return performanceUnitAuthority{
			Unit: "token_like_input_units", Scope: performanceUnitScopeTokenLikeInputGeometry,
		}, true
	case "media_transcode":
		return performanceUnitAuthority{
			Unit: "media_work_units", Scope: performanceUnitScopeFullInputByteQuartersPerSegment,
		}, true
	case "media_rendering":
		return performanceUnitAuthority{
			Unit: "pixels", Scope: performanceUnitScopeDeclaredOutputPixelsPerScene,
		}, true
	default:
		return performanceUnitAuthority{}, false
	}
}

// validateCurrentPerformanceSettlementAuthority is deliberately a current/new
// admission rule. validateFrozenRuntimeCellPerformance must not call it:
// historical PlacementRequirement v2 snapshots replay their self-contained
// unit and scope rather than being rewritten by today's settlement policy.
func validateCurrentPerformanceSettlementAuthority(performance RuntimeCellPerformance) error {
	want, ok := currentSettlementAuthorityForJobType(performance.JobType)
	if !ok {
		return fmt.Errorf(
			"runtime cell %q has no governed settlement unit for job %q",
			performance.CellID, performance.JobType)
	}
	if strings.TrimSpace(performance.UnitScope) == "" {
		return fmt.Errorf(
			"runtime cell %q benchmark throughput unit %q has no explicit performance-unit scope; job %q settles %q/%q and no frozen unit conversion authority is present",
			performance.CellID, performance.Unit, performance.JobType, want.Unit, want.Scope)
	}
	if performance.Unit != want.Unit || performance.UnitScope != want.Scope {
		return fmt.Errorf(
			"runtime cell %q benchmark throughput authority %q/%q is incompatible with job %q settlement authority %q/%q; no frozen unit conversion authority covers this unit/scope mismatch",
			performance.CellID, performance.Unit, performance.UnitScope,
			performance.JobType, want.Unit, want.Scope)
	}
	return nil
}

// validateCurrentPlacementPerformanceAuthority is the durable-ingress backstop.
// Quote construction normally catches a mismatch before a placement exists,
// but SubmitJobTx must independently refuse an old quote after settlement
// semantics, the cell's receipt pointer, or the receipt's binding/identity has
// changed. Historical readers call validateFrozenRuntimeCellPerformance only.
func validateCurrentPlacementPerformanceAuthority(placement PlacementRequirement) error {
	return validateCurrentPlacementPerformanceAuthorityAt(placement, runtimeCellPerformanceNow())
}

func validateCurrentPlacementPerformanceAuthorityAt(
	placement PlacementRequirement,
	at time.Time,
) error {
	if placement.Version != placementRequirementVersion {
		return fmt.Errorf(
			"new durable job admission requires placement version %d with frozen performance/build authority; version %d is historical-read-only",
			placementRequirementVersion, placement.Version)
	}
	if placement.PerformanceAuthority == nil {
		return fmt.Errorf("current placement lacks frozen runtime-cell performance authority")
	}
	if placement.RuntimeMatrixSHA256 != generatedRuntimeMatrixSHA256 {
		return fmt.Errorf(
			"current capability matrix is %q, not frozen matrix %q; the placement is historical-only and must be rebuilt",
			generatedRuntimeMatrixSHA256, placement.RuntimeMatrixSHA256)
	}
	frozen := placement.PerformanceAuthority
	if frozen.Version != frozenRuntimeCellPerformanceVersion {
		return fmt.Errorf(
			"frozen runtime-cell performance version %d is historical-read-only; current admission requires version %d with exact engine build identity",
			frozen.Version, frozenRuntimeCellPerformanceVersion)
	}
	if !engineBuildHashPattern.MatchString(placement.EngineBuildHash) ||
		placement.EngineBuildHash != frozen.Performance.EngineBuildHash {
		return fmt.Errorf(
			"current placement engine_build_hash %q does not exactly bind frozen measured execution build %q",
			placement.EngineBuildHash, frozen.Performance.EngineBuildHash)
	}
	if !validCurrentEngineBuildIdentityPolicy(placement.EngineBuildIdentityPolicy) ||
		placement.EngineBuildIdentityPolicy != frozen.Performance.EngineBuildIdentityPolicy {
		return fmt.Errorf(
			"current placement engine_build_identity_policy %q does not bind frozen measured policy %q",
			placement.EngineBuildIdentityPolicy, frozen.Performance.EngineBuildIdentityPolicy)
	}
	if !validCanonicalHardwareIdentity(placement.HardwareIdentity) ||
		placement.HardwareIdentity != frozen.Performance.HardwareIdentity {
		return fmt.Errorf(
			"current placement hardware_identity %q does not exactly bind frozen measured hardware %q",
			placement.HardwareIdentity, frozen.Performance.HardwareIdentity)
	}
	if err := validateCurrentRuntimeCellPerformanceAuthorityAt(frozen.Performance, at); err != nil {
		return err
	}
	if err := validateFrozenRuntimeCellPerformance(frozen); err != nil {
		return err
	}
	current, ok := benchmarkAuthorityManifest[frozen.Performance.BenchmarkAuthority]
	if !ok {
		return fmt.Errorf("current benchmark authority %q is absent from the embedded manifest",
			frozen.Performance.BenchmarkAuthority)
	}
	currentPins, err := exactWeightDigestsForCurrentPerformance(frozen.Performance)
	if err != nil {
		return fmt.Errorf("resolve current exact runtime-cell artifact authority: %w", err)
	}
	current = projectBenchmarkSummaryToExactCell(current, currentPins)
	canonicalCommit, err := canonicalFrozenMercSourceCommit(current.MercSourceCommit)
	if err != nil {
		return fmt.Errorf("current benchmark source identity is not bindable: %w", err)
	}
	current.MercSourceCommit = canonicalCommit
	currentSHA, err := benchmarkReceiptSummarySHA256(current)
	if err != nil {
		return fmt.Errorf("digest current benchmark authority: %w", err)
	}
	if currentSHA != frozen.BenchmarkSnapshotSHA256 {
		return fmt.Errorf(
			"current benchmark authority %q no longer matches the frozen admission snapshot; the quote is historical-only and must be rebuilt",
			frozen.Performance.BenchmarkAuthority)
	}
	if frozen.ModelArtifactWireKind != frozen.Performance.WireKind {
		return fmt.Errorf(
			"current model artifact wire kind %q no longer matches frozen performance kind %q; the quote is historical-only and must be rebuilt",
			frozen.ModelArtifactWireKind, frozen.Performance.WireKind)
	}
	if !slices.Equal(currentPins, frozen.ModelArtifactPins) {
		return fmt.Errorf(
			"current model artifact pins for %q no longer match the frozen performance authority; the quote is historical-only and must be rebuilt",
			frozen.Performance.ModelID)
	}
	return nil
}

// validateFrozenRuntimeCellPerformance validates only the accepted snapshot.
// It deliberately does not read benchmarkAuthorityManifest or time.Now: doing
// either would let a later receipt replacement or policy clock rewrite history.
func validateFrozenRuntimeCellPerformance(binding *FrozenRuntimeCellPerformance) error {
	if binding == nil {
		return fmt.Errorf("placement lacks frozen runtime-cell performance authority")
	}
	legacy := false
	switch binding.Version {
	case frozenRuntimeCellPerformanceLegacyVersion:
		legacy = true
		if binding.PolicyRevision != runtimeCellPerformanceLegacyPolicyRevision {
			return fmt.Errorf("unsupported frozen runtime-cell performance authority %d/%q",
				binding.Version, binding.PolicyRevision)
		}
	case frozenRuntimeCellPerformanceVersion:
		if binding.PolicyRevision != runtimeCellPerformancePolicyRevision {
			return fmt.Errorf("unsupported frozen runtime-cell performance authority %d/%q",
				binding.Version, binding.PolicyRevision)
		}
	default:
		return fmt.Errorf("unsupported frozen runtime-cell performance authority %d/%q",
			binding.Version, binding.PolicyRevision)
	}
	if !validSHA256(binding.BenchmarkSnapshotSHA256) || !validSHA256(binding.Digest) {
		return fmt.Errorf("frozen runtime-cell performance carries an invalid digest")
	}
	wantSummary, err := benchmarkReceiptSummarySHA256(binding.BenchmarkSnapshot)
	if err != nil || wantSummary != binding.BenchmarkSnapshotSHA256 {
		return fmt.Errorf("frozen benchmark snapshot digest mismatch")
	}
	wantDigest, err := frozenRuntimeCellPerformanceDigest(*binding)
	if err != nil || wantDigest != binding.Digest {
		return fmt.Errorf("frozen runtime-cell performance digest mismatch")
	}
	p := binding.Performance
	if p.CellID == "" || p.RuntimeProfileID == "" || p.JobType == "" || p.ModelID == "" ||
		p.WireKind == "" || p.BenchmarkAuthority == "" || p.MeasuredOnHWClass == "" || p.Unit == "" {
		return fmt.Errorf("frozen runtime-cell performance identity is incomplete")
	}
	if binding.ModelArtifactWireKind != p.WireKind {
		return fmt.Errorf(
			"frozen model artifact wire kind %q does not match accepted cell wire kind %q",
			binding.ModelArtifactWireKind, p.WireKind)
	}
	if legacy {
		if p.EngineBuildHash != "" || p.EngineBuildIdentityPolicy != "" ||
			binding.BenchmarkSnapshot.EngineBuildHash != "" ||
			binding.BenchmarkSnapshot.EngineBuildIdentityPolicy != "" ||
			p.HardwareIdentity != "" || binding.BenchmarkSnapshot.HardwareIdentity != "" {
			return fmt.Errorf("legacy frozen runtime-cell performance carries a future build or exact-hardware identity")
		}
	} else if !engineBuildHashPattern.MatchString(p.EngineBuildHash) ||
		binding.BenchmarkSnapshot.EngineBuildHash != p.EngineBuildHash ||
		!historicalEngineBuildIdentityPolicyMatches(
			p.EngineBuildIdentityPolicy,
			binding.BenchmarkSnapshot.EngineBuildIdentityPolicy,
		) ||
		!validCanonicalHardwareIdentity(p.HardwareIdentity) ||
		binding.BenchmarkSnapshot.HardwareIdentity != p.HardwareIdentity {
		return fmt.Errorf(
			"frozen runtime-cell performance build/hardware %q/%q does not exactly match benchmark %q/%q",
			p.EngineBuildHash, p.HardwareIdentity,
			binding.BenchmarkSnapshot.EngineBuildHash, binding.BenchmarkSnapshot.HardwareIdentity)
	}
	if !slices.Contains(p.HardwareClasses, p.MeasuredOnHWClass) {
		return fmt.Errorf("frozen measured hardware %q is outside governed platforms %v",
			p.MeasuredOnHWClass, p.HardwareClasses)
	}
	wantHaircut := 0.0
	switch p.Status {
	case cellThroughputMeasured:
		wantHaircut = measuredThroughputHaircut
	case cellThroughputStale:
		wantHaircut = staleThroughputHaircut
	default:
		return fmt.Errorf("frozen runtime-cell performance status %q is not admissible", p.Status)
	}
	if p.Haircut != wantHaircut || p.ObservedUnitsPerSec <= 0 ||
		math.IsNaN(p.ObservedUnitsPerSec) || math.IsInf(p.ObservedUnitsPerSec, 0) {
		return fmt.Errorf("frozen runtime-cell performance rate or haircut is invalid")
	}
	wantRate := p.ObservedUnitsPerSec * p.Haircut
	if math.Abs(p.ConservativeUnitsPerSec-wantRate) > math.Abs(wantRate)*1e-12 {
		return fmt.Errorf("frozen conservative throughput %g does not equal observed %g × haircut %g",
			p.ConservativeUnitsPerSec, p.ObservedUnitsPerSec, p.Haircut)
	}
	summary := binding.BenchmarkSnapshot
	if !strings.EqualFold(strings.TrimSpace(summary.BindingStatus), BindingBound) ||
		!summary.isEvidenceFor(p.RuntimeProfileID) ||
		(len(summary.ModelIDs) > 0 && !summary.measures(p.ModelID)) ||
		summary.HWClass != p.MeasuredOnHWClass || summary.MeasuredAt != p.BenchmarkedAt ||
		!summary.ThroughputMeasured {
		return fmt.Errorf("frozen benchmark snapshot does not bind the accepted runtime/model/hardware measurement")
	}
	if reason := authorityValidityRefusal(summary.Validity); reason != "" {
		return fmt.Errorf("frozen benchmark snapshot was accepted with forbidden validity %s", reason)
	}
	commit := strings.TrimSpace(summary.MercSourceCommit)
	if len(commit) != 40 || !hexObjectName.MatchString(commit) {
		return fmt.Errorf("frozen benchmark source identity is not a canonical 40-character git object id")
	}
	if strings.TrimSpace(summary.Harness) == "" {
		return fmt.Errorf("frozen benchmark snapshot names no harness")
	}
	if revision := strings.TrimSpace(summary.ProfileRevision); revision != "" &&
		revision != p.ProfileRevision {
		return fmt.Errorf("frozen benchmark profile revision %q does not match accepted revision %q",
			revision, p.ProfileRevision)
	}
	for i, digest := range binding.ModelArtifactPins {
		if digest != strings.ToLower(strings.TrimSpace(digest)) || !validSHA256(digest) {
			return fmt.Errorf("frozen model artifact pin is not a canonical SHA-256 digest")
		}
		if i > 0 && binding.ModelArtifactPins[i-1] >= digest {
			return fmt.Errorf("frozen model artifact pins are not a unique sorted exact set")
		}
	}
	for i, digest := range summary.ModelArtifactSHA256s {
		if digest != strings.ToLower(strings.TrimSpace(digest)) || !validSHA256(digest) {
			return fmt.Errorf("frozen benchmark model artifact identity is not a canonical SHA-256 digest")
		}
		if i > 0 && summary.ModelArtifactSHA256s[i-1] >= digest {
			return fmt.Errorf("frozen benchmark model artifact identity is not a unique sorted exact set")
		}
	}
	if !slices.Equal(binding.ModelArtifactPins, summary.ModelArtifactSHA256s) {
		return fmt.Errorf(
			"frozen benchmark snapshot model artifact identity does not exactly cross-bind the selected %q cell weight pins",
			binding.ModelArtifactWireKind)
	}
	measurement, ok := summary.Throughput[p.RuntimeProfileID]
	if !ok || measurement.Unit != p.Unit || measurement.UnitScope != p.UnitScope ||
		measurement.Precision != p.Precision ||
		measurement.OperatingBatch != p.OperatingBatch ||
		measurement.UnitsPerSecAtOperatingBatch != p.ObservedUnitsPerSec ||
		measurement.BestObservedUnitsPerSec != p.ObservedBestUnitsPerSec ||
		measurement.Basis != p.BenchmarkBasis {
		return fmt.Errorf("frozen benchmark throughput does not match the accepted performance projection")
	}
	return nil
}

// resolveCellPerformance derives the binding for one cell at one instant.
//
// It fails toward "unproven" at every step rather than toward a number: a
// receipt that names another engine, measures another model, or publishes no
// rate for this profile all land on the named fallback with a stated reason.
func resolveCellPerformance(
	profile authorityRuntimeProfile, cell authorityCell, at time.Time,
) RuntimeCellPerformance {
	out := RuntimeCellPerformance{
		CellID:                  cell.ID,
		RuntimeProfileID:        profile.RuntimeID,
		ProfileRevision:         profile.Revision,
		JobType:                 cell.Job,
		ModelID:                 cell.Model,
		QualityTier:             cell.qualityTierFor(profile),
		Lifecycle:               cell.EffectiveLifecycle(profile),
		HardwareClasses:         append([]string(nil), profile.Hardware.Platforms...),
		CellMaxBatch:            cell.MaxBatch,
		CellMaxConcurrency:      cell.MaxConcurrency,
		BenchmarkAuthority:      cell.benchmarkAuthorityFor(profile),
		ConservativeUnitsPerSec: unprovenFallbackUnitsPerSec,
		Status:                  cellThroughputUnproven,
	}
	for _, model := range runtimeAuthority.Models {
		if model.ID == cell.Model {
			out.ModelRevision = model.HFRevision
			out.WireKind = wireKindFor(cell, model.WireKind)
		}
	}

	if out.BenchmarkAuthority == "" {
		out.Reason = fmt.Sprintf("cell %q names no benchmark authority", cell.ID)
		return out
	}
	if ok, reason := cellAuthorityBindable(profile, cell); !ok {
		out.Reason = fmt.Sprintf("benchmark authority for cell %q is not bindable: %s", cell.ID, reason)
		return out
	}
	receipt, known := benchmarkAuthorityManifest[out.BenchmarkAuthority]
	if known {
		out.EngineBuildHash = receipt.EngineBuildHash
		out.EngineBuildIdentityPolicy = receipt.EngineBuildIdentityPolicy
		out.HardwareIdentity = receipt.HardwareIdentity
	}
	switch {
	case !known:
		out.Reason = fmt.Sprintf("benchmark authority %q is not a known receipt",
			out.BenchmarkAuthority)
		return out
	case !receipt.isEvidenceFor(profile.RuntimeID):
		out.Reason = fmt.Sprintf("receipt %q is evidence for %v, not %q",
			out.BenchmarkAuthority, receipt.RuntimeProfileIDs, profile.RuntimeID)
		return out
	case len(receipt.ModelIDs) > 0 && !receipt.measures(cell.Model):
		out.Reason = fmt.Sprintf("receipt %q measures %v, not %q",
			out.BenchmarkAuthority, receipt.ModelIDs, cell.Model)
		return out
	case !receipt.ThroughputMeasured:
		out.Reason = fmt.Sprintf("receipt %q records no throughput measurement",
			out.BenchmarkAuthority)
		return out
	}

	measurement, ok := receipt.Throughput[profile.RuntimeID]
	if !ok || measurement.UnitsPerSecAtOperatingBatch <= 0 || measurement.Unit == "" {
		out.Reason = fmt.Sprintf(
			"receipt %q publishes no per-second rate for %q in a stated unit",
			out.BenchmarkAuthority, profile.RuntimeID)
		return out
	}
	// A rate taken at a batch the cell does not permit is a rate the cell cannot
	// deliver. Refusing is the point: the alternative is promising 128-wide
	// throughput from a cell capped at 8.
	if cell.MaxBatch > 0 && measurement.OperatingBatch > cell.MaxBatch {
		out.Reason = fmt.Sprintf(
			"receipt %q measured batch %d, above cell %q's declared max_batch %d",
			out.BenchmarkAuthority, measurement.OperatingBatch, cell.ID, cell.MaxBatch)
		return out
	}

	out.MeasuredOnHWClass = receipt.HWClass
	out.OperatingBatch = measurement.OperatingBatch
	out.Unit = measurement.Unit
	out.UnitScope = measurement.UnitScope
	out.Precision = measurement.Precision
	out.BenchmarkedAt = receipt.MeasuredAt
	out.BenchmarkBasis = measurement.Basis
	out.ObservedUnitsPerSec = measurement.UnitsPerSecAtOperatingBatch
	out.ObservedBestUnitsPerSec = measurement.BestObservedUnitsPerSec

	measuredAt, err := time.Parse(time.RFC3339, receipt.MeasuredAt)
	if err != nil {
		// An undated measurement can never be revalidated, so it is not treated
		// as fresh forever - it is treated as no measurement at all.
		out.Reason = fmt.Sprintf("receipt %q has no parseable measurement date",
			out.BenchmarkAuthority)
		out.ObservedUnitsPerSec, out.ObservedBestUnitsPerSec = 0, 0
		return out
	}
	measuredAt = measuredAt.UTC()
	at = at.UTC()
	if measuredAt.After(at) {
		out.Reason = fmt.Sprintf(
			"receipt %q is future-dated: measured_at=%s current_authority_time=%s",
			out.BenchmarkAuthority, measuredAt.Format(time.RFC3339), at.Format(time.RFC3339))
		out.ObservedUnitsPerSec, out.ObservedBestUnitsPerSec = 0, 0
		return out
	}
	age := at.Sub(measuredAt)
	out.Status, out.Haircut, out.Confidence = cellThroughputMeasured, measuredThroughputHaircut, 0.7
	out.Reason = fmt.Sprintf(
		"measured on %s at batch %d, %s", receipt.HWClass, measurement.OperatingBatch,
		measurement.Basis)
	if age > benchmarkRevalidationWindow {
		out.Status, out.Haircut, out.Confidence = cellThroughputStale, staleThroughputHaircut, 0.3
		out.Reason = fmt.Sprintf(
			"benchmark %s is %.0f days old, past the %.0f-day revalidation window; "+
				"the rate is degraded until it is re-taken",
			receipt.MeasuredAt, age.Hours()/24, benchmarkRevalidationWindow.Hours()/24)
	}
	out.ConservativeUnitsPerSec = measurement.UnitsPerSecAtOperatingBatch * out.Haircut
	return out
}

// routableCellPerformance is every cell that may take ordinary buyer work for
// this job type and model, slowest first. Slowest first because the callers
// that pick one element pick the conservative one.
//
// onlyCells, when non-empty, narrows the set to the cells a frozen workload
// decision may actually land on. Without it the whole catalogue for the model is
// in scope, so a job pinned to a fast cell is priced at a slow cell it can never
// be routed to.
func routableCellPerformance(
	jobType, modelID string, onlyCells []string, at time.Time,
) []RuntimeCellPerformance {
	var out []RuntimeCellPerformance
	for _, profile := range runtimeAuthority.Runtimes {
		for _, cell := range profile.Cells {
			if cell.Job != jobType || cell.Model != modelID {
				continue
			}
			// A pinned set is admission's answer, not a hint.
			//
			// Directed routing exists to send real buyer work to a cell that is
			// PROVEN but deliberately not in the advertised catalogue. Filtering
			// on Routable() dropped exactly those cells, so a directed workload
			// found no cells at all, took the no-routable-cell branch, and was
			// offered unprovenFallbackUnitsPerSec with the reason "no routable
			// runtime cell serves job X on model Y" -- false about a cell that
			// serves it and measured, and the wrong rate by three orders of
			// magnitude. Which cells a workload may land on is admission's
			// decision; this file's job is to price the ones it named.
			if len(onlyCells) > 0 {
				if !slices.Contains(onlyCells, cell.ID) {
					continue
				}
				if !cell.ReachableByDirectedRouting(profile) {
					continue
				}
			} else if !cell.Routable(profile) {
				continue
			}
			out = append(out, resolveCellPerformance(profile, cell, at))
		}
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].ConservativeUnitsPerSec != out[j].ConservativeUnitsPerSec {
			return out[i].ConservativeUnitsPerSec < out[j].ConservativeUnitsPerSec
		}
		return out[i].CellID < out[j].CellID
	})
	return out
}

// admissionUnitsPerSec is the rate the control plane may offer a supplier for
// this workload, and the cell that rate came from.
//
// It is the SLOWEST cell the workload can land on, because a posted job carries
// one offered rate and may reach any of them. Offering the fastest cell's rate
// would admit suppliers whose floor the cell they actually get cannot clear,
// which reintroduces the silent no-claim failure one level down.
//
// An unproven cell REFUSES instead of joining that minimum. Its rate is
// unprovenFallbackUnitsPerSec, deliberately below every realistic payout floor,
// so one unproven routable cell would drag the offered rate for the whole
// (job, model) to nothing and no supplier could claim any of it - the silent
// no-claim failure this file exists to remove, one level further down. Of the
// three ways out, refusing is the only loud one: excluding the cell from the
// minimum prices the job off cells it may not land on, so a supplier routed to
// the unproven one is promised a rate nothing has shown that cell can produce;
// and a named degraded posture is the same 1 unit/s wearing a label. The state
// is also not as impossible as it looks. schema.sql's
// runtime_profile_models_evidenced CHECK forbids a routable cell with an EMPTY
// benchmark_authority, not one whose named receipt fails to measure it - a
// receipt that loses its throughput block, names another engine, or publishes a
// batch above the cell's max_batch lands here with the CHECK satisfied. That is
// a contradiction between two governed documents, and it should stop admission
// with the cell named rather than quietly reprice the market.
//
// No routable cell at all is a different fact and keeps the fallback: refusing
// to price a workload nothing can execute would turn a routing gap into a 5xx
// on submit. The rate is small enough that admission rejects it, which is the
// honest outcome.
func admissionUnitsPerSec(
	jobType, modelID string, onlyCells []string, at time.Time,
) (float64, RuntimeCellPerformance, error) {
	cells := routableCellPerformance(jobType, modelID, onlyCells, at)
	if len(cells) == 0 {
		return unprovenFallbackUnitsPerSec, RuntimeCellPerformance{
			JobType: jobType, ModelID: modelID,
			ConservativeUnitsPerSec: unprovenFallbackUnitsPerSec,
			Status:                  cellThroughputUnproven,
			Reason: fmt.Sprintf(
				"no routable runtime cell serves job %q on model %q", jobType, modelID),
		}, nil
	}
	for _, cell := range cells {
		if cell.Status == cellThroughputUnproven {
			return 0, cell, unprovenRoutableCellRefusal(cell, jobType, modelID)
		}
		if err := validateCurrentPerformanceSettlementAuthority(cell); err != nil {
			return 0, cell, err
		}
	}
	return cells[0].ConservativeUnitsPerSec, cells[0], nil
}

// performanceBoundHardwareClasses narrows an accepted placement to the exact
// hardware class on which its governed throughput was measured. Runtime
// profiles describe where a binary can run; they do not prove that a benchmark
// taken on an Ultra also prices a Base, Max, or NVIDIA class. A buyer's hardware
// list is an allowed set, so narrowing it to the one evidenced class preserves
// the buyer constraint while preventing a different class from inheriting the
// measured duration and provider economics.
func performanceBoundHardwareClasses(
	performance RuntimeCellPerformance, requested []string,
) ([]string, error) {
	hw := performance.MeasuredOnHWClass
	if performance.Status == cellThroughputUnproven || hw == "" {
		return nil, fmt.Errorf(
			"runtime cell %q has no usable per-hardware throughput binding: %s",
			performance.CellID, performance.Reason)
	}
	if !slices.Contains(performance.HardwareClasses, hw) {
		return nil, fmt.Errorf(
			"runtime cell %q was measured on hardware class %q outside its governed platform set %v",
			performance.CellID, hw, performance.HardwareClasses)
	}
	if len(requested) > 0 && !slices.Contains(requested, hw) {
		return nil, fmt.Errorf(
			"runtime cell %q is priced from a benchmark on hardware class %q, which is outside the buyer's allowed set %v",
			performance.CellID, hw, requested)
	}
	return []string{hw}, nil
}

func unprovenRoutableCellRefusal(cell RuntimeCellPerformance, jobType, modelID string) error {
	return fmt.Errorf(
		"cell %q may take job %q on model %q but has no usable benchmark authority, "+
			"so no rate may be offered for it: %s",
		cell.CellID, jobType, modelID, cell.Reason)
}

// governedAdmissionUnitRates is every supplier unit rate the cell authority can
// produce for a historical PlacementRequirement v1 workload.
//
// A frozen pricing decision is verified against this set rather than against the
// single rate resolvable at the instant of verification, because which posture a
// cell resolves to is a function of the wall clock: the same receipt is MEASURED
// before its revalidation window closes and STALE after. Re-resolving would make
// every already-accepted job in the database fail its own snapshot check on the
// day a receipt aged out, months after acceptance and with nothing about the
// decision having changed. Both governed haircuts are therefore admissible - and
// a number that is neither did not come from the receipt at all, which is the
// property the snapshot check actually needs.
//
// V1 did not freeze a performance snapshot. Its replay must therefore retain
// the historical rate-set check, but must not apply today's settlement-unit
// compatibility policy: doing so would rewrite an accepted v1 embed decision
// years later. All new writes use v2 and are independently gated by
// admissionUnitsPerSec, freezeRuntimeCellPerformance, and SubmitJobTx.
//
// Callers pass the decision's frozen runtime candidates, and
// validatePlacementRequirement admits exactly one, so in the pricing path this
// set describes one cell.
func governedAdmissionUnitRates(
	jobType, modelID string, onlyCells []string, at time.Time,
) ([]float64, error) {
	cells := routableCellPerformance(jobType, modelID, onlyCells, at)
	if len(cells) == 0 {
		return []float64{unprovenFallbackUnitsPerSec}, nil
	}
	out := make([]float64, 0, 2*len(cells))
	for _, cell := range cells {
		if cell.Status == cellThroughputUnproven {
			return nil, unprovenRoutableCellRefusal(cell, jobType, modelID)
		}
		out = append(out,
			cell.ObservedUnitsPerSec*measuredThroughputHaircut,
			cell.ObservedUnitsPerSec*staleThroughputHaircut)
	}
	return out, nil
}

// admissionCellsForWorkload is the frozen candidate set a workload decision may
// be routed to. Pricing reads it so the offered rate describes the cells this
// job can actually reach, not every cell in the catalogue that serves the model.
func admissionCellsForWorkload(workload WorkloadDecision) []string {
	out := make([]string, 0, len(workload.RuntimeCandidates))
	for _, candidate := range workload.RuntimeCandidates {
		out = append(out, candidate.CellID)
	}
	return out
}

// SupplierCellViability is one row of the answer to "why am I not being offered
// any work". Every field a supplier would need to argue with the number is
// present, including the ones that make the answer unflattering.
type SupplierCellViability struct {
	Performance RuntimeCellPerformance `json:"performance"`

	SupplierHWClass       string  `json:"supplier_hw_class"`
	BuyerPricePer1KUnits  float64 `json:"buyer_price_per_1k_units"`
	SupplierShare         float64 `json:"supplier_share"`
	Tier                  string  `json:"tier"`
	ExpectedUtilization   float64 `json:"expected_utilization"`
	UtilizationBasis      string  `json:"utilization_basis"`
	ExpectedSupplierUSDHr float64 `json:"expected_supplier_usd_hr"`
	MinimumPayoutUSDHr    float64 `json:"minimum_payout_usd_hr"`

	Eligible bool `json:"eligible"`
	// Reason is populated whether or not the row is eligible. An eligible row
	// still says what it is eligible ON, because "measured on hardware you do
	// not have" is something a supplier should read before buying a machine.
	Reason string `json:"reason"`
}

// utilizationBasisWhileExecuting states, rather than hides, what the admission
// comparison actually models. offered_rate_usd_hr and min_payout_usd_hr are
// both rates WHILE EXECUTING; neither side models idle time, and no fleet
// utilization measurement exists to model it with. Publishing a made-up
// utilization factor here would be exactly the defect this lane removed.
const utilizationBasisWhileExecuting = "rate while executing; queue idle time is not measured and is not modeled on either side of the comparison"

// SupplierAdmissionViability evaluates a supplier's payout floor against what
// each routable cell it can actually execute is expected to earn.
//
// catalogueAuthority is injected rather than rebuilding a rate here so the
// report quotes the same append-only price and workload-specific supplier share
// admission used. A report computed from a second price or a process-wide share
// would eventually disagree with the gate it is explaining.
func SupplierAdmissionViability(
	hwClass string,
	minPayoutUSDHr float64,
	tier string,
	at time.Time,
	catalogueAuthority func(modelID string) (CataloguePriceAuthority, error),
) []SupplierCellViability {
	var out []SupplierCellViability
	for _, profile := range runtimeAuthority.Runtimes {
		for _, cell := range profile.Cells {
			if !cell.Routable(profile) {
				continue
			}
			row := SupplierCellViability{
				Performance:         resolveCellPerformance(profile, cell, at),
				SupplierHWClass:     hwClass,
				Tier:                tier,
				ExpectedUtilization: 1,
				UtilizationBasis:    utilizationBasisWhileExecuting,
				MinimumPayoutUSDHr:  minPayoutUSDHr,
			}
			authority, err := catalogueAuthority(cell.Model)
			if err != nil {
				row.Reason = fmt.Sprintf("model %q has no catalogue price authority: %v",
					cell.Model, err)
				out = append(out, row)
				continue
			}
			price := authority.ReferencePricePer1K
			supplierShare := authority.SupplierShare
			if !finiteNonNegative(price) || price <= 0 || !finiteNonNegative(supplierShare) || supplierShare <= 0 || supplierShare > 1 {
				row.Reason = fmt.Sprintf("model %q has incomplete catalogue money authority", cell.Model)
				out = append(out, row)
				continue
			}
			row.BuyerPricePer1KUnits = price
			row.SupplierShare = supplierShare
			// What admission would actually do with this exact cell, asked the
			// same way a directed job asks it.
			_, _, admissionRefusal := admissionUnitsPerSec(cell.Job, cell.Model,
				[]string{cell.ID}, at)
			if admissionRefusal == nil {
				row.ExpectedSupplierUSDHr = expectedSupplierUSDHr(
					row.Performance.ConservativeUnitsPerSec, price, supplierShare, tier)
			} else {
				// A rate admission cannot use is not an earnings estimate, even when
				// another predicate (such as hardware) is also unsatisfied.
				row.ExpectedSupplierUSDHr = 0
			}

			switch {
			case !hardwareClassServed(profile, hwClass):
				row.Reason = fmt.Sprintf(
					"runtime %q does not run on hardware class %q; it serves %v",
					profile.RuntimeID, hwClass, profile.Hardware.Platforms)
			case admissionRefusal != nil:
				// ASK admission rather than restate it.
				//
				// This branch used to carry its own sentence: the cell "is priced
				// at the 1 unit/s conservative fallback rather than as if
				// measured". That was true while the fallback was the answer.
				// Admission now refuses an unproven routable cell outright, and
				// the report went on promising a low rate on work that will never
				// be posted -- a supplier reading "priced very low" tunes their
				// floor and waits forever.
				//
				// Restating a decision is what let the two drift. The report now
				// quotes the refusal admission actually produces, so the next
				// change to that rule cannot leave this text behind.
				row.Reason = fmt.Sprintf(
					"no work will be posted on cell %q at all -- this is not a low "+
						"price, it is no market: %v", cell.ID, admissionRefusal)
				// The earnings column has to agree. A dollar figure beside "no
				// market" is the same disagreement in another column.
			case row.ExpectedSupplierUSDHr < minPayoutUSDHr:
				row.Reason = fmt.Sprintf(
					"expected $%.5f/hr on cell %q is below your minimum payout of $%.5f/hr "+
						"(%.1f %s/s conservative x $%.8f per 1k units x %.2f supplier share)",
					row.ExpectedSupplierUSDHr, cell.ID, minPayoutUSDHr,
					row.Performance.ConservativeUnitsPerSec, row.Performance.Unit,
					price, supplierShare)
			default:
				row.Eligible = true
				row.Reason = fmt.Sprintf(
					"expected $%.5f/hr on cell %q clears your $%.5f/hr floor; %s",
					row.ExpectedSupplierUSDHr, cell.ID, minPayoutUSDHr, row.Performance.Reason)
				if row.Performance.MeasuredOnHWClass != "" &&
					row.Performance.MeasuredOnHWClass != hwClass {
					row.Reason += fmt.Sprintf(
						" (measured on %s, not your %s; your rate will differ)",
						row.Performance.MeasuredOnHWClass, hwClass)
				}
			}
			out = append(out, row)
		}
	}
	sort.Slice(out, func(i, j int) bool {
		return out[i].Performance.CellID < out[j].Performance.CellID
	})
	return out
}

// expectedSupplierUSDHr converts a conservative unit rate into the hourly gross
// a supplier is offered. It is the same arithmetic supplierAdmissionCeilingUSDHr
// performs, kept in one place so the report cannot drift from the gate.
func expectedSupplierUSDHr(unitsPerSec, referencePricePer1K, supplierShare float64, tier string) float64 {
	if !finiteNonNegative(unitsPerSec) || !finiteNonNegative(referencePricePer1K) {
		return 0
	}
	return unitsPerSec * 3600 / 1000 * referencePricePer1K * supplierShare * tierMultiplier(tier)
}

// hardwareClassServed reports whether this profile runs on the supplier's box.
// An unknown class is not assumed to be served: admission that guesses here
// offers CUDA work to a Mac.
func hardwareClassServed(profile authorityRuntimeProfile, hwClass string) bool {
	if hwClass == "" {
		return false
	}
	for _, platform := range profile.Hardware.Platforms {
		if platform == hwClass {
			return true
		}
	}
	return false
}
