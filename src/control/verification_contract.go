package main

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
)

// ---------------------------------------------------------------------------
// Canonical VerificationContract (Network V2 Step 12)
//
// Acceptance-time authority for what verification *means* for one accepted
// plan. It is not an outcome record. Outcomes remain VerificationWorkPlan /
// VerificationDecision / decision_sha256; this type must never compete with
// those digests for settlement identity.
//
// The defect it closes: buyer-visible surfaces could name a method
// (WorkloadDecision.VerificationStrategy free string such as
// "cosine+platform_honeypot_floor") while SAMPLED selection was false, and
// embed checks dropped EmbeddingComparison to a boolean so threshold /
// revision / reference were not acceptance-bound on one ordinary-path object.
//
// What is already authority elsewhere and is *cited / projected* here rather
// than re-authored as a second digest:
//   - governed class + class policy (verification_class.go, ComputePlan)
//   - sampling policy identity (verification_work_plan.go)
//   - failure effect vocabulary (verification_plan.go)
//   - embed comparator policy (embedding_comparator.go)
//
// Named recompute policy remains ABSENT until product requires one.
// ---------------------------------------------------------------------------

const (
	verificationContractVersion           = 1
	verificationContractPolicyRevision    = "verification-contract-v1"
	verificationRecomputePolicyAbsent     = "absent"
	verificationEvaluatorByteExact        = "byte_exact"
	verificationEvaluatorEmbedCosine      = "embed_cosine"
	verificationReferenceBindingCheckTime = "check_time_peer_or_honeypot"
)

// VerificationContract is the accepted immutable authority for verifier class,
// evaluator / comparator revision / frozen thresholds, sampling policy identity,
// failure-consequence vocabulary, and recompute policy (currently absent).
//
// Per-task selection outcomes and per-check reference digests are not here:
// selection is pinned on VerificationWorkPlan at check time, and the governed
// reference bytes (peer or honeypot answer) only exist then.
type VerificationContract struct {
	Version        int    `json:"version"`
	PolicyRevision string `json:"policy_revision"`

	// Class authority. Empty class on historical compute plans means SAMPLED;
	// new contracts always spell the class out.
	Class       string `json:"class"`
	ClassPolicy string `json:"class_policy"`

	// Sampling policy identity. Selection probability and the selected bit are
	// check-time outcomes on VerificationWorkPlan, not acceptance claims.
	SamplingPolicy string `json:"sampling_policy"`

	// Evaluator sold by the winning cell / job type.
	EvaluatorKind      string  `json:"evaluator_kind"`
	CellVerification   string  `json:"cell_verification"`
	ComparatorRevision string  `json:"comparator_revision,omitempty"`
	MeanThreshold      float64 `json:"mean_threshold,omitempty"`
	RowThreshold       float64 `json:"row_threshold,omitempty"`
	MinimumNorm        float64 `json:"minimum_norm,omitempty"`
	// ReferenceBinding states when the governed reference becomes known.
	// Acceptance cannot freeze peer/honeypot bytes that do not exist yet.
	ReferenceBinding string `json:"reference_binding"`

	// Buyer composition knobs that the legacy strategy string projects from.
	RedundancyFrac        float32 `json:"redundancy_frac"`
	HoneypotFrac          float32 `json:"honeypot_frac"`
	PlatformHoneypotFloor bool    `json:"platform_honeypot_floor"`

	// Closed vocabulary of effect kinds apply is allowed to emit. Apply still
	// plans concrete effects after the check; this freezes the allowed set.
	FailureConsequenceVocabulary []string `json:"failure_consequence_vocabulary"`

	// Named recompute policy is ABSENT until product requires one.
	RecomputePolicy string `json:"recompute_policy"`

	// LegacyStrategy is a projection of this contract for WorkloadDecision
	// compatibility. It is a method/composition label, not a claim that any
	// particular task was checked. Readers that need "was it verified under X"
	// must also see verification_selected (or an always-checked class).
	LegacyStrategy string `json:"legacy_strategy"`
}

// failureConsequenceVocabulary is the closed set of VerificationEffectKind
// values apply may emit. Sorted for stable digests.
func failureConsequenceVocabulary() []string {
	kinds := []string{
		string(VerificationEffectDockReputation),
		string(VerificationEffectRecordEvent),
		string(VerificationEffectClawbackCredit),
		string(VerificationEffectQuarantine),
		string(VerificationEffectRequeue),
		string(VerificationEffectInsertTiebreak),
	}
	sort.Strings(kinds)
	return kinds
}

// buildVerificationContract freezes acceptance-time verification identity from
// the governed class, cell verification string, buyer policy knobs, and the
// live evaluator / sampling constants. It projects the legacy strategy string
// rather than treating that string as authority.
func buildVerificationContract(
	class string,
	cellVerification string,
	jobType string,
	policy VerificationPolicy,
) (VerificationContract, error) {
	cellVerification = strings.TrimSpace(cellVerification)
	if cellVerification == "" {
		return VerificationContract{}, fmt.Errorf("verification contract requires cell verification method")
	}

	if class == "" {
		class = VerificationClassSampled
	}
	if !knownVerificationClass(class) {
		return VerificationContract{}, fmt.Errorf("unknown verification class %q", class)
	}
	if class == VerificationClassHoneypot || class == VerificationClassRedundant {
		return VerificationContract{}, fmt.Errorf(
			"verification class %q is assigned per task, not as a job-wide contract class", class)
	}

	evaluator, err := verificationEvaluatorFor(jobType, cellVerification)
	if err != nil {
		return VerificationContract{}, err
	}

	out := VerificationContract{
		Version:                      verificationContractVersion,
		PolicyRevision:               verificationContractPolicyRevision,
		Class:                        class,
		ClassPolicy:                  verificationClassPolicyRevision,
		SamplingPolicy:               verificationSamplingPolicy,
		EvaluatorKind:                evaluator.kind,
		CellVerification:             cellVerification,
		ComparatorRevision:           evaluator.revision,
		MeanThreshold:                evaluator.meanThreshold,
		RowThreshold:                 evaluator.rowThreshold,
		MinimumNorm:                  evaluator.minimumNorm,
		ReferenceBinding:             verificationReferenceBindingCheckTime,
		RedundancyFrac:               policy.RedundancyFrac,
		HoneypotFrac:                 policy.HoneypotFrac,
		PlatformHoneypotFloor:        policy.RedundancyFrac <= 0 && policy.HoneypotFrac <= 0,
		FailureConsequenceVocabulary: failureConsequenceVocabulary(),
		RecomputePolicy:              verificationRecomputePolicyAbsent,
	}
	out.LegacyStrategy = projectLegacyVerificationStrategy(out)
	if err := validateVerificationContract(out); err != nil {
		return VerificationContract{}, err
	}
	return out, nil
}

type verificationEvaluatorSpec struct {
	kind          string
	revision      string
	meanThreshold float64
	rowThreshold  float64
	minimumNorm   float64
}

func verificationEvaluatorFor(jobType, cellVerification string) (verificationEvaluatorSpec, error) {
	jobType = strings.TrimSpace(jobType)
	cellVerification = strings.TrimSpace(cellVerification)
	// Evaluator follows resultsAgree's job-type arms, not the free cell string
	// alone. Cell verification is a consistency check against the catalogue
	// promise: an embed cell must sell cosine, a byte-compared job must sell
	// byte_exact. A mismatch is refused rather than silently remapped.
	switch jobType {
	case "embed":
		if cellVerification != "cosine" {
			return verificationEvaluatorSpec{}, fmt.Errorf(
				"embed job sells cell verification %q; want cosine", cellVerification)
		}
		pol := embeddingPolicyV2()
		return verificationEvaluatorSpec{
			kind:          verificationEvaluatorEmbedCosine,
			revision:      pol.Revision,
			meanThreshold: pol.MeanThreshold,
			rowThreshold:  pol.RowThreshold,
			minimumNorm:   pol.MinimumNorm,
		}, nil
	case "batch_infer", "media_transcode", "media_rendering":
		if cellVerification != "byte_exact" {
			return verificationEvaluatorSpec{}, fmt.Errorf(
				"%s job sells cell verification %q; want byte_exact", jobType, cellVerification)
		}
		return verificationEvaluatorSpec{kind: verificationEvaluatorByteExact}, nil
	default:
		return verificationEvaluatorSpec{}, fmt.Errorf(
			"no verification evaluator for job type %q", jobType)
	}
}

// projectLegacyVerificationStrategy rebuilds the composite strategy string that
// WorkloadDecision historically carried. Same inputs → same string as
// verificationStrategyFor, so the decision can keep projecting without policy
// loss while the contract is the acceptance authority.
func projectLegacyVerificationStrategy(c VerificationContract) string {
	parts := []string{c.CellVerification}
	if c.RedundancyFrac > 0 {
		parts = append(parts, "redundant_execution")
	}
	if c.HoneypotFrac > 0 {
		parts = append(parts, "honeypot")
	}
	if c.RedundancyFrac <= 0 && c.HoneypotFrac <= 0 {
		parts = append(parts, "platform_honeypot_floor")
	}
	return strings.Join(parts, "+")
}

func verificationContractDigest(c VerificationContract) (string, error) {
	if err := validateVerificationContract(c); err != nil {
		return "", err
	}
	blob, err := json.Marshal(c)
	if err != nil {
		return "", fmt.Errorf("marshal verification contract: %w", err)
	}
	sum := sha256.Sum256(blob)
	return hex.EncodeToString(sum[:]), nil
}

func validateVerificationContract(c VerificationContract) error {
	if c.Version != verificationContractVersion {
		return fmt.Errorf("unsupported verification contract version %d", c.Version)
	}
	if c.PolicyRevision != verificationContractPolicyRevision {
		return fmt.Errorf("unknown verification contract policy revision %q", c.PolicyRevision)
	}
	if !knownVerificationClass(c.Class) {
		return fmt.Errorf("verification contract names unknown class %q", c.Class)
	}
	if c.Class == VerificationClassHoneypot || c.Class == VerificationClassRedundant {
		return fmt.Errorf("verification contract names per-task class %q as job-wide", c.Class)
	}
	if c.ClassPolicy != verificationClassPolicyRevision {
		return fmt.Errorf("verification contract class policy %q does not match live revision %q",
			c.ClassPolicy, verificationClassPolicyRevision)
	}
	if c.SamplingPolicy != verificationSamplingPolicy {
		return fmt.Errorf("verification contract sampling policy %q does not match live %q",
			c.SamplingPolicy, verificationSamplingPolicy)
	}
	switch c.EvaluatorKind {
	case verificationEvaluatorByteExact:
		if c.ComparatorRevision != "" || c.MeanThreshold != 0 || c.RowThreshold != 0 || c.MinimumNorm != 0 {
			return fmt.Errorf("byte_exact evaluator must not carry embed comparator thresholds")
		}
	case verificationEvaluatorEmbedCosine:
		if c.ComparatorRevision != embeddingComparatorRevision {
			return fmt.Errorf("embed evaluator revision %q is not the live comparator %q",
				c.ComparatorRevision, embeddingComparatorRevision)
		}
		if c.MeanThreshold != embeddingMeanCosineThreshold ||
			c.RowThreshold != embeddingRowCosineThreshold ||
			c.MinimumNorm != embeddingMinimumNorm {
			return fmt.Errorf("embed evaluator thresholds do not match live comparator policy")
		}
	default:
		return fmt.Errorf("unknown evaluator kind %q", c.EvaluatorKind)
	}
	if strings.TrimSpace(c.CellVerification) == "" {
		return fmt.Errorf("verification contract missing cell verification method")
	}
	if c.ReferenceBinding != verificationReferenceBindingCheckTime {
		return fmt.Errorf("verification contract reference binding %q is not recognised", c.ReferenceBinding)
	}
	if c.RecomputePolicy != verificationRecomputePolicyAbsent {
		return fmt.Errorf("named recompute policy %q is not admitted; only %q is allowed",
			c.RecomputePolicy, verificationRecomputePolicyAbsent)
	}
	wantVocab := failureConsequenceVocabulary()
	if len(c.FailureConsequenceVocabulary) != len(wantVocab) {
		return fmt.Errorf("failure consequence vocabulary length %d, want %d",
			len(c.FailureConsequenceVocabulary), len(wantVocab))
	}
	for i, kind := range c.FailureConsequenceVocabulary {
		if kind != wantVocab[i] {
			return fmt.Errorf("failure consequence vocabulary mismatch at %d: got %q want %q",
				i, kind, wantVocab[i])
		}
	}
	if c.LegacyStrategy != projectLegacyVerificationStrategy(c) {
		return fmt.Errorf("legacy strategy %q does not project from the contract", c.LegacyStrategy)
	}
	return nil
}

// verificationClaimMayCite reports whether a buyer-visible "verified under X"
// claim is allowed for one task. The strategy / contract method name alone is
// never enough: either the check was selected, or the class is always-checked
// (which forces selection), or the receipt explicitly records not-selected.
//
// selected == nil means "no decision yet" — that is also not a verified claim.
func verificationClaimMayCite(class string, selected *bool) bool {
	if verificationClassAlwaysChecked(class) {
		// Always-checked classes must still show selection true once decided;
		// before a decision, no claim.
		return selected != nil && *selected
	}
	// SAMPLED (and unknown falling back to sampled): require an explicit
	// selection record. selected=false is an honest "not checked" status, not
	// a verified claim — callers that want to display method-without-check use
	// a different surface (contract class + selected=false).
	return selected != nil && *selected
}

// cellVerificationFromStrategy extracts the cell method token from a legacy
// composite strategy string (everything before the first '+').
func cellVerificationFromStrategy(strategy string) string {
	strategy = strings.TrimSpace(strategy)
	if strategy == "" {
		return ""
	}
	if i := strings.IndexByte(strategy, '+'); i >= 0 {
		return strategy[:i]
	}
	return strategy
}

// stampVerificationContractOnPlan builds the acceptance-time contract from a
// frozen workload decision and plan class, then binds it onto the plan.
// Idempotent: re-stamping the same decision and class yields the same digest.
func stampVerificationContractOnPlan(plan ComputePlan, decision WorkloadDecision) (ComputePlan, error) {
	class := plan.VerificationClass
	if class == "" {
		class = VerificationClassSampled
	}
	cellMethod := cellVerificationFromStrategy(decision.VerificationStrategy)
	contract, err := buildVerificationContract(
		class, cellMethod, decision.RuntimeJobType, decision.Binding.Verification,
	)
	if err != nil {
		return ComputePlan{}, err
	}
	if contract.LegacyStrategy != decision.VerificationStrategy {
		return ComputePlan{}, fmt.Errorf(
			"verification contract legacy strategy %q disagrees with workload decision strategy %q",
			contract.LegacyStrategy, decision.VerificationStrategy)
	}
	// Fast path: already bound to this exact contract.
	if plan.VerificationContractSHA256 != "" && plan.VerificationContract != nil {
		if err := validateComputePlanVerificationContract(plan); err == nil {
			want, derr := verificationContractDigest(contract)
			if derr == nil && plan.VerificationContractSHA256 == want {
				return plan, nil
			}
		}
	}
	return governComputePlanVerificationContract(plan, contract)
}

// governComputePlanVerificationContract stamps an acceptance-time contract onto
// a frozen plan. Separate from plan construction for the same reason as the
// class stamp: the contract is not part of the priced geometry.
func governComputePlanVerificationContract(plan ComputePlan, contract VerificationContract) (ComputePlan, error) {
	if err := validateVerificationContract(contract); err != nil {
		return ComputePlan{}, err
	}
	// Class on the plan and class on the contract must agree when both are set.
	planClass := plan.VerificationClass
	if planClass == "" {
		planClass = VerificationClassSampled
	}
	if contract.Class != planClass {
		return ComputePlan{}, fmt.Errorf(
			"verification contract class %q disagrees with compute plan class %q",
			contract.Class, planClass)
	}
	// When the plan has no class yet, stamp the contract's so class and
	// contract cannot drift on the same object.
	if plan.VerificationClass == "" {
		stamped, err := governComputePlanVerificationClass(plan, contract.Class)
		if err != nil {
			return ComputePlan{}, err
		}
		plan = stamped
	}
	digest, err := verificationContractDigest(contract)
	if err != nil {
		return ComputePlan{}, err
	}
	// Copy so the plan does not share the caller's backing array for vocabulary.
	cp := contract
	cp.FailureConsequenceVocabulary = append([]string(nil), contract.FailureConsequenceVocabulary...)
	plan.VerificationContract = &cp
	plan.VerificationContractSHA256 = digest
	return plan, nil
}

// validateComputePlanVerificationContract checks optional contract binding on a
// plan. Empty is the historical default (no contract object); when present,
// body and digest must agree and class must match.
func validateComputePlanVerificationContract(plan ComputePlan) error {
	if plan.VerificationContract == nil && plan.VerificationContractSHA256 == "" {
		return nil
	}
	if plan.VerificationContract == nil || plan.VerificationContractSHA256 == "" {
		return fmt.Errorf("compute plan verification contract body and digest must both be set or both empty")
	}
	if err := validateVerificationContract(*plan.VerificationContract); err != nil {
		return err
	}
	got, err := verificationContractDigest(*plan.VerificationContract)
	if err != nil {
		return err
	}
	if got != plan.VerificationContractSHA256 {
		return fmt.Errorf("compute plan verification contract digest mismatch: got %s want %s",
			got, plan.VerificationContractSHA256)
	}
	planClass := plan.VerificationClass
	if planClass == "" {
		planClass = VerificationClassSampled
	}
	if plan.VerificationContract.Class != planClass {
		return fmt.Errorf("compute plan class %q disagrees with verification contract class %q",
			planClass, plan.VerificationContract.Class)
	}
	return nil
}
