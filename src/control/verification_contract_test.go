package main

import (
	"strings"
	"testing"

	"github.com/google/uuid"
)

// Step 12 defect: a buyer-visible method/strategy claim can name an evaluator
// while SAMPLED selection is false. The strategy string alone is therefore not
// a completed-check claim; verificationClaimMayCite is what gates "verified
// under X".
func TestBuyerVisibleVerificationClaimCannotOutrunSelection(t *testing.T) {
	// The free strategy string that historically sat alone on the receipt.
	strategy := verificationStrategyFor(
		WorkloadBinding{Verification: VerificationPolicy{}},
		generatedRuntimeCapability{Verification: "cosine"},
	)
	if strategy != "cosine+platform_honeypot_floor" {
		t.Fatalf("strategy projection %q", strategy)
	}

	// A SAMPLED task that sampling declined must not support a "verified under
	// cosine" claim even though the strategy names cosine.
	selectedFalse := false
	if verificationClaimMayCite(VerificationClassSampled, &selectedFalse) {
		t.Fatal("SAMPLED + selected=false allowed a verified-under-X claim")
	}
	if verificationClaimMayCite(VerificationClassSampled, nil) {
		t.Fatal("SAMPLED + no selection record allowed a verified-under-X claim")
	}

	// Only an explicit selection record authorises the claim for ordinary traffic.
	selectedTrue := true
	if !verificationClaimMayCite(VerificationClassSampled, &selectedTrue) {
		t.Fatal("SAMPLED + selected=true refused a verified-under-X claim")
	}

	// Always-checked classes still require the selected bit once decided: the
	// class removes the coin flip, it does not invent an outcome that has not
	// been recorded.
	if verificationClaimMayCite(VerificationClassRequired, nil) {
		t.Fatal("REQUIRED with no selection record allowed a claim")
	}
	if !verificationClaimMayCite(VerificationClassRequired, &selectedTrue) {
		t.Fatal("REQUIRED + selected=true refused a claim")
	}
	if verificationClaimMayCite(VerificationClassRequired, &selectedFalse) {
		t.Fatal("REQUIRED + selected=false allowed a claim (contradicts always-checked)")
	}
}

func TestVerificationContractBindsAcceptanceAuthorities(t *testing.T) {
	contract, err := buildVerificationContract(
		VerificationClassSampled, "cosine", "embed", VerificationPolicy{},
	)
	mustf(t, err, "build embed contract: %v")

	if contract.Class != VerificationClassSampled {
		t.Errorf("class %q", contract.Class)
	}
	if contract.ClassPolicy != verificationClassPolicyRevision {
		t.Errorf("class policy %q", contract.ClassPolicy)
	}
	if contract.SamplingPolicy != verificationSamplingPolicy {
		t.Errorf("sampling policy %q", contract.SamplingPolicy)
	}
	if contract.EvaluatorKind != verificationEvaluatorEmbedCosine {
		t.Errorf("evaluator %q", contract.EvaluatorKind)
	}
	if contract.ComparatorRevision != embeddingComparatorRevision {
		t.Errorf("comparator revision %q", contract.ComparatorRevision)
	}
	if contract.MeanThreshold != embeddingMeanCosineThreshold ||
		contract.RowThreshold != embeddingRowCosineThreshold ||
		contract.MinimumNorm != embeddingMinimumNorm {
		t.Errorf("thresholds not frozen to live embed policy: mean=%v row=%v norm=%v",
			contract.MeanThreshold, contract.RowThreshold, contract.MinimumNorm)
	}
	if contract.ReferenceBinding != verificationReferenceBindingCheckTime {
		t.Errorf("reference binding %q", contract.ReferenceBinding)
	}
	if contract.RecomputePolicy != verificationRecomputePolicyAbsent {
		t.Errorf("recompute policy %q; named policy is ABSENT until product requires one",
			contract.RecomputePolicy)
	}
	if contract.LegacyStrategy != "cosine+platform_honeypot_floor" {
		t.Errorf("legacy strategy %q", contract.LegacyStrategy)
	}
	wantVocab := failureConsequenceVocabulary()
	if len(contract.FailureConsequenceVocabulary) != len(wantVocab) {
		t.Fatalf("vocabulary length %d want %d",
			len(contract.FailureConsequenceVocabulary), len(wantVocab))
	}
	for i := range wantVocab {
		if contract.FailureConsequenceVocabulary[i] != wantVocab[i] {
			t.Errorf("vocab[%d]=%q want %q",
				i, contract.FailureConsequenceVocabulary[i], wantVocab[i])
		}
	}

	digest, err := verificationContractDigest(contract)
	mustf(t, err, "digest: %v")
	if len(digest) != 64 {
		t.Errorf("digest length %d", len(digest))
	}
	// Tamper: changing a threshold must change the digest and fail validation.
	tampered := contract
	tampered.MeanThreshold = 0.5
	if err := validateVerificationContract(tampered); err == nil {
		t.Fatal("tampered thresholds passed validation")
	}
}

func TestVerificationContractByteExactHasNoEmbedThresholds(t *testing.T) {
	contract, err := buildVerificationContract(
		VerificationClassRequired, "byte_exact", "batch_infer",
		VerificationPolicy{RedundancyFrac: 0.1, HoneypotFrac: 0.05},
	)
	mustf(t, err, "build byte_exact contract: %v")
	if contract.EvaluatorKind != verificationEvaluatorByteExact {
		t.Errorf("evaluator %q", contract.EvaluatorKind)
	}
	if contract.ComparatorRevision != "" || contract.MeanThreshold != 0 {
		t.Errorf("byte_exact carried embed fields: %+v", contract)
	}
	if contract.LegacyStrategy != "byte_exact+redundant_execution+honeypot" {
		t.Errorf("legacy strategy %q", contract.LegacyStrategy)
	}
	if contract.Class != VerificationClassRequired {
		t.Errorf("class %q", contract.Class)
	}
}

func TestVerificationContractRefusesCellJobMismatch(t *testing.T) {
	if _, err := buildVerificationContract(
		VerificationClassSampled, "byte_exact", "embed", VerificationPolicy{},
	); err == nil {
		t.Fatal("embed job with byte_exact cell was accepted")
	}
	if _, err := buildVerificationContract(
		VerificationClassSampled, "cosine", "batch_infer", VerificationPolicy{},
	); err == nil {
		t.Fatal("batch_infer job with cosine cell was accepted")
	}
	if _, err := buildVerificationContract(
		VerificationClassHoneypot, "cosine", "embed", VerificationPolicy{},
	); err == nil {
		t.Fatal("per-task HONEYPOT class was accepted as a job-wide contract")
	}
}

func TestVerificationContractProjectsLegacyStrategyWithoutPolicyLoss(t *testing.T) {
	cases := []struct {
		cell string
		pol  VerificationPolicy
	}{
		{"cosine", VerificationPolicy{}},
		{"byte_exact", VerificationPolicy{}},
		{"byte_exact", VerificationPolicy{RedundancyFrac: 0.2}},
		{"cosine", VerificationPolicy{HoneypotFrac: 0.1}},
		{"byte_exact", VerificationPolicy{RedundancyFrac: 0.1, HoneypotFrac: 0.05}},
	}
	for _, tc := range cases {
		want := verificationStrategyFor(
			WorkloadBinding{Verification: tc.pol},
			generatedRuntimeCapability{Verification: tc.cell},
		)
		jobType := "batch_infer"
		if tc.cell == "cosine" {
			jobType = "embed"
		}
		contract, err := buildVerificationContract(
			VerificationClassSampled, tc.cell, jobType, tc.pol,
		)
		mustf(t, err, "build: %v")
		if contract.LegacyStrategy != want {
			t.Errorf("cell=%s pol=%+v: contract %q != strategyFor %q",
				tc.cell, tc.pol, contract.LegacyStrategy, want)
		}
	}
}

func TestGovernComputePlanVerificationContractBindsDigest(t *testing.T) {
	plan := ComputePlan{}
	contract, err := buildVerificationContract(
		VerificationClassSampled, "cosine", "embed", VerificationPolicy{},
	)
	mustf(t, err, "build: %v")

	stamped, err := governComputePlanVerificationContract(plan, contract)
	mustf(t, err, "stamp: %v")
	if stamped.VerificationClass != VerificationClassSampled {
		t.Errorf("class not stamped onto empty plan: %q", stamped.VerificationClass)
	}
	if stamped.VerificationContract == nil || stamped.VerificationContractSHA256 == "" {
		t.Fatal("contract body or digest missing after stamp")
	}
	if err := validateComputePlanVerificationContract(stamped); err != nil {
		t.Fatalf("stamped plan failed contract validation: %v", err)
	}

	// Digest mismatch is refused.
	stamped.VerificationContractSHA256 = strings.Repeat("0", 64)
	if err := validateComputePlanVerificationContract(stamped); err == nil {
		t.Fatal("digest mismatch was accepted")
	}

	// Body without digest is refused.
	half := stamped
	half.VerificationContractSHA256 = ""
	// restore a valid body pointer
	c := contract
	half.VerificationContract = &c
	if err := validateComputePlanVerificationContract(half); err == nil {
		t.Fatal("body without digest was accepted")
	}

	// Historical empty remains valid.
	if err := validateComputePlanVerificationContract(ComputePlan{}); err != nil {
		t.Fatalf("empty historical plan refused: %v", err)
	}
}

func TestStampVerificationContractOnPlanAgreesWithWorkloadDecision(t *testing.T) {
	decision, err := buildWorkloadDecisionFromBinding(WorkloadBinding{
		Version:      workloadBindingVersion,
		JobType:      JobType{Type: "embed"},
		Model:        ModelRef{Ref: "all-minilm-l6-v2"},
		InputSHA256:  strings.Repeat("ab", 32),
		Verification: VerificationPolicy{},
		Tier:         "batch",
	})
	// buildWorkloadDecisionFromBinding may fail if the model is not in the
	// embedded authority; fall back to a synthetic decision that still exercises
	// the stamp path.
	if err != nil {
		t.Logf("live workload decision unavailable (%v); using synthetic decision", err)
		decision = WorkloadDecision{
			Version:              workloadDecisionVersion,
			RuntimeJobType:       "embed",
			VerificationStrategy: "cosine+platform_honeypot_floor",
			Binding: WorkloadBinding{
				Verification: VerificationPolicy{},
			},
		}
	}

	plan := ComputePlan{VerificationClass: VerificationClassSampled, VerificationClassPolicy: verificationClassPolicyRevision}
	stamped, err := stampVerificationContractOnPlan(plan, decision)
	mustf(t, err, "stamp: %v")
	if stamped.VerificationContract == nil {
		t.Fatal("no contract on stamped plan")
	}
	if stamped.VerificationContract.LegacyStrategy != decision.VerificationStrategy {
		t.Fatalf("contract strategy %q != decision strategy %q",
			stamped.VerificationContract.LegacyStrategy, decision.VerificationStrategy)
	}
	if stamped.AuthorityVerificationContractMissing() {
		t.Fatal("stamped plan reports missing contract authority")
	}
}

// AuthorityVerificationContractMissing is a test helper shape check — implemented
// as a method so the production type surface stays on the contract fields.
func (p ComputePlan) AuthorityVerificationContractMissing() bool {
	return p.VerificationContract == nil || p.VerificationContractSHA256 == ""
}

func TestReceiptAuthorityCitesVerificationContract(t *testing.T) {
	contract, err := buildVerificationContract(
		VerificationClassSampled, "byte_exact", "batch_infer", VerificationPolicy{},
	)
	mustf(t, err, "build: %v")
	digest, err := verificationContractDigest(contract)
	mustf(t, err, "digest: %v")
	plan := ComputePlan{
		VerificationContract:       &contract,
		VerificationContractSHA256: digest,
	}
	receipt := assembleClearingReceipt(
		uuid.Nil, "complete", nil, &plan, nil, nil, nil,
		Verification{}, nil, nil,
	)
	if receipt.Authority.VerificationContractSHA256 != digest {
		t.Fatalf("receipt authority cited %q, want %q",
			receipt.Authority.VerificationContractSHA256, digest)
	}
}

func TestVerificationContractDoesNotCompeteWithDecisionDigest(t *testing.T) {
	// The contract digest and the outcome decision_sha256 are different objects
	// for different stages. Changing the contract must not be expressible as a
	// substitute for an outcome digest, and the contract type must not carry
	// settlement entries.
	contract, err := buildVerificationContract(
		VerificationClassSampled, "cosine", "embed", VerificationPolicy{},
	)
	mustf(t, err, "build: %v")
	cDigest, err := verificationContractDigest(contract)
	mustf(t, err, "contract digest: %v")

	decision := VerificationDecision{Outcome: OutcomePass}
	dDigest, err := verificationDecisionDigest(decision, nil)
	// OutcomePass with nil settlement may fail shape checks inside digest? digest
	// only marshals; it does not validate settlement shape.
	mustf(t, err, "decision digest: %v")
	if cDigest == dDigest {
		t.Fatal("contract digest collided with an empty-settlement decision digest")
	}
	// Contract has no settlement / effects fields — if this type ever grows
	// outcome authority, this test's neighbour (struct shape) will need a hard
	// refuse. For now the type surface is acceptance-only.
	_ = contract.FailureConsequenceVocabulary
}
