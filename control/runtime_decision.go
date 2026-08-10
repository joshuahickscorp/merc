package main

import (
	"errors"
	"fmt"
	"strings"
)

const runtimeDecisionVersion = 1

// RuntimeDecision statuses. ACCEPTED is the only batch-production status today:
// ordinary admission always freezes a singleton cell (lifecycle ladder or
// directed). There is no REFUSED runtime decision on the happy path — a missing
// cell fails workload classification before accept.
const (
	runtimeDecisionAccepted = "ACCEPTED"
)

// Accept lanes that may freeze a RuntimeDecision.
const (
	runtimeLaneBatch = "batch"
)

// Accepted selection bases for RuntimeDecision. These name how the *accept*
// path chose the cell. Measured shadow bases must never appear here.
const (
	// runtimeSelectionBasisLifecycleLadder is the production ordinary path:
	// rankAndFreezeAdmissionCell / chooseShadowCell on the lifecycle+quality
	// ladder, or the degenerate singleton when only one cell is advertised.
	// It is NOT a multi-engine measured tournament.
	runtimeSelectionBasisLifecycleLadder = "LIFECYCLE_LADDER"
	// runtimeSelectionBasisDirectedCell is an operator/test force onto a named
	// cell via buildWorkloadDecisionDirected. Still not a measured tournament.
	runtimeSelectionBasisDirectedCell = "DIRECTED_CELL"
)

// runtimeDecisionDigestCheck verifies that a claimed digest matches a fresh
// hash of the decision body. Tests may neutralise it to show failing-before
// tamper behaviour; production always leaves it true.
var runtimeDecisionDigestCheck = true

// runtimeDecisionEnforceBasisHonesty refuses measured or shadow-authoritative
// bases on an accepted RuntimeDecision. Tests may neutralise it for
// failing-before proof; production always leaves it true.
var runtimeDecisionEnforceBasisHonesty = true

// RuntimeDecision is the immutable accepted runtime authority for one job.
//
// It binds engine, cell, model/artifact, precision/quality contract, hardware
// class, runtime revision, benchmark authority and activation revision at
// accept. It cites existing WorkloadDecision and PlacementRequirement digests
// rather than inventing a second copy of those facts.
//
// Selection basis is honest: ordinary production freezes via the lifecycle
// ladder (rankAndFreezeAdmissionCell), not a measured multi-engine tournament.
// runtime_shadow_selections remains post-commit observation and must never
// become money or routing authority (ShadowSelectionAuthoritative is always
// false on a sealed decision).
type RuntimeDecision struct {
	Version int    `json:"version"`
	Status  string `json:"status"`
	// Lane is the accept path that froze this decision.
	Lane string `json:"lane"`

	// SelectionBasis is LIFECYCLE_LADDER or DIRECTED_CELL. Measured shadow
	// bases (MORE_THROUGHPUT_…, TIE_NO_DECISION) are refused.
	SelectionBasis string `json:"selection_basis"`
	// SelectionAuthority names the code path that produced the freeze.
	SelectionAuthority string `json:"selection_authority"`
	// SelectionNote records honesty about what did NOT happen (no tournament).
	SelectionNote string `json:"selection_note"`

	Engine    string `json:"engine"`
	RuntimeID string `json:"runtime_id"`
	CellID    string `json:"cell_id"`

	ModelRef      string `json:"model_ref"`
	ModelKind     string `json:"model_kind"`
	ModelRevision string `json:"model_revision,omitempty"`

	// Precision/quality contract frozen from the performance authority and
	// workload verification projection.
	Precision            string `json:"precision,omitempty"`
	QualityTier          string `json:"quality_tier,omitempty"`
	VerificationStrategy string `json:"verification_strategy,omitempty"`
	Lifecycle            string `json:"lifecycle,omitempty"`

	HardwareClasses  []string `json:"hardware_classes,omitempty"`
	HardwareIdentity string   `json:"hardware_identity,omitempty"`

	// Runtime revision / build identity (existing placement fields).
	RuntimeMatrixSHA256       string `json:"runtime_matrix_sha256"`
	EngineBuildHash           string `json:"engine_build_hash,omitempty"`
	EngineBuildIdentityPolicy string `json:"engine_build_identity_policy,omitempty"`
	ProfileRevision           string `json:"profile_revision,omitempty"`

	// Benchmark authority: cite the existing FrozenRuntimeCellPerformance
	// digest rather than re-embedding the receipt body.
	BenchmarkAuthority         string `json:"benchmark_authority,omitempty"`
	PerformanceAuthorityDigest string `json:"performance_authority_digest"`
	BenchmarkSnapshotSHA256    string `json:"benchmark_snapshot_sha256,omitempty"`

	// ActivationPolicyRevision freezes the admit-guard epoch that was only
	// checked (not stored) before Step 8.
	ActivationPolicyRevision int64 `json:"activation_policy_revision"`

	// Citations to existing accept authorities — not parallel fact owners.
	WorkloadDecisionSHA256     string `json:"workload_decision_sha256"`
	PlacementRequirementSHA256 string `json:"placement_requirement_sha256"`

	// ShadowSelectionAuthoritative is always false on a sealed decision.
	// Shadow rows may observe a measured re-ranking; they do not accept work.
	ShadowSelectionAuthoritative bool `json:"shadow_selection_authoritative"`

	Reason   string   `json:"reason"`
	Evidence []string `json:"evidence,omitempty"`
}

func runtimeDecisionDigest(d RuntimeDecision) (string, error) {
	if d.Version != runtimeDecisionVersion {
		return "", fmt.Errorf("unsupported runtime decision version %d", d.Version)
	}
	return canonicalDigest("runtime decision", d)
}

// isMeasuredShadowSelectionBasis reports bases that exist only on the
// post-commit shadow scorer. They must never seal an accepted RuntimeDecision.
func isMeasuredShadowSelectionBasis(basis string) bool {
	switch basis {
	case selectionBasisThroughputEqualLiability,
		selectionBasisTieNoDecision:
		return true
	default:
		return false
	}
}

// isAcceptedRuntimeSelectionBasis is the closed set RuntimeDecision may seal.
func isAcceptedRuntimeSelectionBasis(basis string) bool {
	switch basis {
	case runtimeSelectionBasisLifecycleLadder, runtimeSelectionBasisDirectedCell:
		return true
	default:
		return false
	}
}

// ValidateRuntimeDecisionSnapshot checks structural rules without a digest.
func ValidateRuntimeDecisionSnapshot(d RuntimeDecision) error {
	if d.Version != runtimeDecisionVersion {
		return fmt.Errorf("unsupported runtime decision version %d", d.Version)
	}
	if d.Status != runtimeDecisionAccepted {
		return fmt.Errorf("runtime decision has unknown status %q", d.Status)
	}
	if d.Lane != runtimeLaneBatch {
		return fmt.Errorf("runtime decision has unknown lane %q", d.Lane)
	}
	if strings.TrimSpace(d.Reason) == "" {
		return errors.New("runtime decision requires a reason")
	}
	if strings.TrimSpace(d.Engine) == "" || strings.TrimSpace(d.RuntimeID) == "" ||
		strings.TrimSpace(d.CellID) == "" {
		return errors.New("runtime decision requires engine, runtime_id and cell_id")
	}
	if strings.TrimSpace(d.ModelRef) == "" || strings.TrimSpace(d.ModelKind) == "" {
		return errors.New("runtime decision requires model_ref and model_kind")
	}
	if strings.TrimSpace(d.RuntimeMatrixSHA256) == "" || !validSHA256(d.RuntimeMatrixSHA256) {
		return errors.New("runtime decision requires a valid runtime matrix digest")
	}
	if !validSHA256(d.PerformanceAuthorityDigest) {
		return errors.New("runtime decision requires performance authority digest")
	}
	if d.BenchmarkSnapshotSHA256 != "" && !validSHA256(d.BenchmarkSnapshotSHA256) {
		return errors.New("runtime decision benchmark snapshot digest is not a sha256 hex digest")
	}
	if d.ActivationPolicyRevision <= 0 {
		return errors.New("runtime decision requires a positive activation policy revision")
	}
	if !validSHA256(d.WorkloadDecisionSHA256) {
		return errors.New("runtime decision requires workload decision digest")
	}
	if !validSHA256(d.PlacementRequirementSHA256) {
		return errors.New("runtime decision requires placement requirement digest")
	}
	if strings.TrimSpace(d.SelectionAuthority) == "" {
		return errors.New("runtime decision requires selection_authority")
	}
	if strings.TrimSpace(d.SelectionNote) == "" {
		return errors.New("runtime decision requires selection_note")
	}
	if err := enforceRuntimeDecisionBasisHonesty(d); err != nil {
		return err
	}
	return nil
}

// enforceRuntimeDecisionBasisHonesty is the accept-time tripwire: a measured
// shadow basis or an authoritative shadow flag cannot seal. Neutralisable in
// tests for failing-before proof only.
func enforceRuntimeDecisionBasisHonesty(d RuntimeDecision) error {
	if !runtimeDecisionEnforceBasisHonesty {
		// Still refuse empty basis even when the honesty tripwire is off —
		// structure without a basis is not a decision.
		if strings.TrimSpace(d.SelectionBasis) == "" {
			return errors.New("runtime decision requires selection_basis")
		}
		return nil
	}
	if d.ShadowSelectionAuthoritative {
		return errors.New("runtime decision refuses shadow selection as money/routing authority")
	}
	if isMeasuredShadowSelectionBasis(d.SelectionBasis) {
		return fmt.Errorf(
			"runtime decision refuses measured shadow selection basis %q as accept authority",
			d.SelectionBasis)
	}
	if !isAcceptedRuntimeSelectionBasis(d.SelectionBasis) {
		return fmt.Errorf("runtime decision has unknown or disallowed selection_basis %q",
			d.SelectionBasis)
	}
	// LIFECYCLE_LADDER must never be re-labelled as a measured basis by note
	// abuse alone — the enum already forbids measured values. Defense in depth:
	// if someone set basis to ladder but claimed shadow authority, refused above.
	return nil
}

// ValidateRuntimeDecisionDigest checks structure and, when the tamper gate is
// on, that the claimed digest matches the body.
func ValidateRuntimeDecisionDigest(d RuntimeDecision, digest string) error {
	if err := ValidateRuntimeDecisionSnapshot(d); err != nil {
		return err
	}
	if !validSHA256(digest) {
		return errors.New("runtime decision digest is not a sha256 hex digest")
	}
	if !runtimeDecisionDigestCheck {
		return nil
	}
	got, err := runtimeDecisionDigest(d)
	if err != nil {
		return err
	}
	if got != digest {
		return fmt.Errorf("runtime decision digest mismatch: claimed %s recomputed %s", digest, got)
	}
	return nil
}

// buildBatchRuntimeDecision freezes the runtime batch admission actually
// chose: the lifecycle-ranked (or directed) singleton on WorkloadDecision,
// with hardware/benchmark facts from PlacementRequirement.PerformanceAuthority
// and the activation epoch that guarded the accept transaction.
//
// It does not re-select a cell. It names the freeze that already happened and
// records that the basis was a lifecycle ladder (or directed force), not a
// measured tournament. Shadow selection is explicitly non-authoritative.
func buildBatchRuntimeDecision(
	workload WorkloadDecision,
	placement PlacementRequirement,
	activationRevision int64,
) (RuntimeDecision, error) {
	if err := ValidateWorkloadDecisionSnapshot(workload); err != nil {
		return RuntimeDecision{}, fmt.Errorf("runtime decision workload: %w", err)
	}
	if err := validatePlacementRequirement(placement, workload); err != nil {
		return RuntimeDecision{}, fmt.Errorf("runtime decision placement: %w", err)
	}
	if len(workload.RuntimeCandidates) != 1 {
		return RuntimeDecision{}, fmt.Errorf(
			"runtime decision requires exactly one frozen runtime candidate, got %d",
			len(workload.RuntimeCandidates))
	}
	if placement.PerformanceAuthority == nil {
		return RuntimeDecision{}, errors.New(
			"runtime decision requires PlacementRequirement.PerformanceAuthority")
	}
	if err := validateFrozenRuntimeCellPerformance(placement.PerformanceAuthority); err != nil {
		return RuntimeDecision{}, fmt.Errorf("runtime decision performance authority: %w", err)
	}
	if activationRevision <= 0 {
		return RuntimeDecision{}, errors.New(
			"runtime decision requires a positive activation policy revision")
	}

	candidate := workload.RuntimeCandidates[0]
	if candidate.CellID != placement.RuntimeCellID ||
		candidate.RuntimeID != placement.RuntimeID ||
		candidate.Engine != placement.Engine {
		return RuntimeDecision{}, fmt.Errorf(
			"runtime decision: workload candidate (%s/%s/%s) disagrees with placement (%s/%s/%s)",
			candidate.Engine, candidate.RuntimeID, candidate.CellID,
			placement.Engine, placement.RuntimeID, placement.RuntimeCellID)
	}

	workloadSHA, err := workloadDecisionDigest(workload)
	if err != nil {
		return RuntimeDecision{}, fmt.Errorf("runtime decision workload digest: %w", err)
	}
	placementSHA, err := placementRequirementDigest(placement)
	if err != nil {
		return RuntimeDecision{}, fmt.Errorf("runtime decision placement digest: %w", err)
	}

	perf := placement.PerformanceAuthority.Performance
	modelKind := candidate.ModelKind
	if modelKind == "" {
		modelKind = placement.ModelKind
	}

	// Ordinary production freezes via runtimeCapabilityForBindingDirected, which
	// either returns the sole advertised cell or rankAndFreezeAdmissionCell's
	// lifecycle-ranked winner. Both are LIFECYCLE_LADDER — never a measured
	// tournament. Directed freezes name DIRECTED_CELL instead.
	basis := runtimeSelectionBasisLifecycleLadder
	authority := "control/workload_classification.go:runtimeCapabilityForBindingDirected/" +
		"rankAndFreezeAdmissionCell"
	note := "accepted cell is the lifecycle-ranked freeze from ordinary admission " +
		"(singleton advertised cell, or chooseShadowCell ladder when multiple cells " +
		"advertise the same model). Not a measured multi-engine tournament. " +
		"Shadow measured re-ranking is post-commit observational only."
	if strings.TrimSpace(workload.DirectedCellID) != "" {
		basis = runtimeSelectionBasisDirectedCell
		authority = "control/workload_classification.go:buildWorkloadDecisionDirected"
		note = "accepted cell was forced by directed routing (operator/test); " +
			"not a measured multi-engine tournament and not ordinary ladder selection."
		if workload.DirectedCellID != candidate.CellID {
			return RuntimeDecision{}, fmt.Errorf(
				"runtime decision directed cell %q disagrees with frozen candidate %q",
				workload.DirectedCellID, candidate.CellID)
		}
	}

	out := RuntimeDecision{
		Version:                      runtimeDecisionVersion,
		Status:                       runtimeDecisionAccepted,
		Lane:                         runtimeLaneBatch,
		SelectionBasis:               basis,
		SelectionAuthority:           authority,
		SelectionNote:                note,
		Engine:                       candidate.Engine,
		RuntimeID:                    candidate.RuntimeID,
		CellID:                       candidate.CellID,
		ModelRef:                     placement.ModelRef,
		ModelKind:                    modelKind,
		ModelRevision:                firstNonEmpty(workload.ModelRevision, perf.ModelRevision),
		Precision:                    perf.Precision,
		QualityTier:                  perf.QualityTier,
		VerificationStrategy:         workload.VerificationStrategy,
		Lifecycle:                    perf.Lifecycle,
		HardwareClasses:              append([]string(nil), placement.HWClasses...),
		HardwareIdentity:             firstNonEmpty(placement.HardwareIdentity, perf.HardwareIdentity),
		RuntimeMatrixSHA256:          placement.RuntimeMatrixSHA256,
		EngineBuildHash:              firstNonEmpty(placement.EngineBuildHash, perf.EngineBuildHash),
		EngineBuildIdentityPolicy:    firstNonEmpty(placement.EngineBuildIdentityPolicy, perf.EngineBuildIdentityPolicy),
		ProfileRevision:              perf.ProfileRevision,
		BenchmarkAuthority:           perf.BenchmarkAuthority,
		PerformanceAuthorityDigest:   placement.PerformanceAuthority.Digest,
		BenchmarkSnapshotSHA256:      placement.PerformanceAuthority.BenchmarkSnapshotSHA256,
		ActivationPolicyRevision:     activationRevision,
		WorkloadDecisionSHA256:       workloadSHA,
		PlacementRequirementSHA256:   placementSHA,
		ShadowSelectionAuthoritative: false,
		Reason: "batch accept freezes the lifecycle-ranked (or directed) runtime " +
			"singleton as immutable accepted authority",
		Evidence: []string{
			"engine/cell identity cited from WorkloadDecision.RuntimeCandidates[0]",
			"hardware/build/benchmark cited from PlacementRequirement.PerformanceAuthority",
			"activation_policy_revision frozen from admit-guard context at SubmitJobTx",
			"runtime_shadow_selections is not accept authority",
		},
	}
	if err := ValidateRuntimeDecisionSnapshot(out); err != nil {
		return RuntimeDecision{}, err
	}
	return out, nil
}
