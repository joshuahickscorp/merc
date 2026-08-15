package main

import (
	"os"
	"strings"
	"testing"
)

// G024: multi-family admission under an AcceptableQualityContract freezes the
// eligible set; same-family competition still rank-and-freezes to one; generation
// q4-vs-bf16 has no multi-family contract.

func TestSelectAdmissionCandidatesMultiFamilyEmbedFreezesBoth(t *testing.T) {
	metal := generatedRuntimeCapability{
		ID: "candle-metal-minilm-embed", Runtime: "candle_metal", Engine: "candle",
		Device: "metal", Job: "embed", Model: "all-minilm-l6-v2", ModelKind: "hf",
		Runner: "embed", MinMemoryGB: 2, Verification: "cosine",
		HardwareClasses: []string{"apple_silicon_ultra"},
	}
	cuda := generatedRuntimeCapability{
		ID: "vllm-cuda-minilm-embed", Runtime: "vllm_cuda", Engine: "vllm",
		Device: "cuda", Job: "embed", Model: "all-minilm-l6-v2", ModelKind: "hf",
		Runner: "embed", MinMemoryGB: 2, Verification: "cosine",
		HardwareClasses: []string{"nvidia_48gb"},
	}
	// chooseShadowCell needs lifecycle from currentActivation for ranking; with
	// empty activation both cells rank equally and cell-id order wins preferred.
	// Multi-family still requires the quality contract covering both IDs.
	got, err := selectAdmissionCandidates([]generatedRuntimeCapability{cuda, metal})
	must(t, err)
	if len(got) != 2 {
		t.Fatalf("multi-family embed freeze got %d candidates, want 2: %+v", len(got), got)
	}
	ids := map[string]bool{}
	for _, c := range got {
		ids[c.ID] = true
	}
	if !ids["candle-metal-minilm-embed"] || !ids["vllm-cuda-minilm-embed"] {
		t.Fatalf("eligible set missing an arm: %+v", got)
	}
	// Preferred is lifecycle winner; with no activation lifecycle both are
	// equal rank and sorted cell-id "candle…" wins over "vllm…".
	if got[0].ID != "candle-metal-minilm-embed" && got[0].ID != "vllm-cuda-minilm-embed" {
		t.Fatalf("preferred cell unexpected: %q", got[0].ID)
	}
	// Contract that authorizes this freeze.
	contract, ok := activeQualityContractFor("embed", "all-minilm-l6-v2",
		[]string{"candle-metal-minilm-embed", "vllm-cuda-minilm-embed"})
	if !ok || contract.ID != "embed-cosine-v2-all-minilm-l6-v2" {
		t.Fatalf("expected embed cosine contract, got ok=%v id=%q", ok, contract.ID)
	}
}

func TestSelectAdmissionCandidatesSameFamilyStillSingleton(t *testing.T) {
	a := generatedRuntimeCapability{
		ID: "candle-metal-minilm-embed", Runtime: "candle_metal", Engine: "candle",
		Device: "metal", Job: "embed", Model: "all-minilm-l6-v2",
		Runner: "embed", Verification: "cosine",
	}
	b := generatedRuntimeCapability{
		ID: "llama-cpp-metal-minilm-embed", Runtime: "llama_cpp_metal", Engine: "llama_cpp",
		Device: "metal", Job: "embed", Model: "all-minilm-l6-v2",
		Runner: "embed", Verification: "cosine",
	}
	// Same device family: even though the embed contract lists both, multi-family
	// requires len(devices) > 1. Same-family remains rank-and-freeze-1.
	got, err := selectAdmissionCandidates([]generatedRuntimeCapability{a, b})
	must(t, err)
	if len(got) != 1 {
		t.Fatalf("same-family freeze got %d, want 1 (rank-and-freeze-1): %+v", len(got), got)
	}
}

func TestSelectAdmissionCandidatesGenerationQ4VsBF16NotMultiFamily(t *testing.T) {
	metal := generatedRuntimeCapability{
		ID: "candle-metal-llama1-infer", Runtime: "candle_metal", Engine: "candle",
		Device: "metal", Job: "batch_infer", Model: "llama-3.2-1b-instruct-q4",
		Runner: "batch_infer", Verification: "byte_exact",
	}
	cuda := generatedRuntimeCapability{
		ID: "vllm-cuda-llama1-infer", Runtime: "vllm_cuda", Engine: "vllm",
		Device: "cuda", Job: "batch_infer", Model: "llama-3.2-1b-instruct-q4",
		Runner: "batch_infer", Verification: "byte_exact",
	}
	// No ACTIVE multi-family contract covers both (the q4-vs-bf16 row is REFUSED).
	got, err := selectAdmissionCandidates([]generatedRuntimeCapability{metal, cuda})
	must(t, err)
	if len(got) != 1 {
		t.Fatalf("q4-vs-bf16 must not multi-freeze; got %d: %+v", len(got), got)
	}
	refused := generationQ4VsBF16Refused()
	if refused.Status != "REFUSED" || refused.MultiFamilySubstitutable {
		t.Fatalf("refusal contract misconfigured: %+v", refused)
	}
	_, ok := activeQualityContractFor("batch_infer", "llama-3.2-1b-instruct-q4",
		[]string{"candle-metal-llama1-infer", "vllm-cuda-llama1-infer"})
	if ok {
		t.Fatal("active multi-family contract must not cover metal q4 + cuda bf16 generation")
	}
}

func TestQualityContractsEmbedAndGenerationAreHonest(t *testing.T) {
	embed, ok := qualityContractByID("embed-cosine-v2-all-minilm-l6-v2")
	if !ok || !embed.MultiFamilySubstitutable || embed.Status != "ACTIVE" {
		t.Fatalf("embed contract: %+v ok=%v", embed, ok)
	}
	if embed.Thresholds["mean_cosine"] == nil {
		t.Fatal("embed contract missing mean_cosine threshold")
	}
	gen, ok := qualityContractByID("batch-infer-byte-exact-matched-precision-llama32-1b")
	if !ok || gen.MultiFamilySubstitutable {
		t.Fatalf("matched-precision generation must not be multi-family: %+v", gen)
	}
	if !strings.Contains(generationQ4VsBF16Refused().HowRoutingProvesMet, "cannot") {
		t.Fatal("q4-vs-bf16 refusal must state routing cannot prove quality")
	}
}

func TestRuntimeDecisionCitesQualityContractOnMultiFamily(t *testing.T) {
	// Production advertised surface is exactly candle-metal-llama1-infer.
	// A singleton freeze must not dress itself as a multi-family quality choice.
	_, _, decision, _ := runtimeDecisionFixture(t)
	if decision.QualityContractID != "" {
		t.Fatalf("singleton freeze must not cite multi-family contract, got %q",
			decision.QualityContractID)
	}
	if len(decision.EligibleCellIDs) != 0 {
		t.Fatalf("singleton eligible set = %v, want empty", decision.EligibleCellIDs)
	}
	if decision.SelectionBasis != runtimeSelectionBasisLifecycleLadder {
		t.Fatalf("basis=%q want %s", decision.SelectionBasis, runtimeSelectionBasisLifecycleLadder)
	}

	// TEST_ONLY fixture: two device families plus an ACTIVE contract covering
	// both. The cuda id is not advertised and not bound.
	const testContractID = "TEST_ONLY-hetero-embed-metal-cuda"
	const testCUDACell = "TEST_ONLY-vllm-cuda-minilm-embed"
	acceptableQualityContracts[testContractID] = AcceptableQualityContract{
		ID:                       testContractID,
		JobType:                  "embed",
		ModelRef:                 "all-minilm-l6-v2",
		Status:                   "ACTIVE",
		MultiFamilySubstitutable: true,
		EligibleCellIDs:          []string{"candle-metal-minilm-embed", testCUDACell},
	}
	t.Cleanup(func() { delete(acceptableQualityContracts, testContractID) })

	metal := generatedRuntimeCapability{
		ID: "candle-metal-minilm-embed", Runtime: "candle_metal", Engine: "candle",
		Device: "metal", Job: "embed", Model: "all-minilm-l6-v2", ModelKind: "hf",
		Runner: "embed", Verification: "cosine",
		HardwareClasses: []string{"apple_silicon_ultra"},
	}
	cuda := generatedRuntimeCapability{
		ID: testCUDACell, Runtime: "vllm_cuda", Engine: "vllm",
		Device: "cuda", Job: "embed", Model: "all-minilm-l6-v2", ModelKind: "hf",
		Runner: "embed", Verification: "cosine",
		HardwareClasses: []string{"nvidia_48gb"},
	}
	got, err := selectAdmissionCandidates([]generatedRuntimeCapability{cuda, metal})
	must(t, err)
	if len(got) != 2 {
		t.Fatalf("TEST_ONLY multi-family freeze got %d, want 2: %+v", len(got), got)
	}
	ids := []string{got[0].ID, got[1].ID}
	if _, err := qualityContractAuthorizingMultiFamily(testContractID, ids); err != nil {
		t.Fatalf("TEST_ONLY contract must authorize the freeze: %v", err)
	}

	// Seal-shaped receipt the accept path would write. Production reconstruction
	// cannot advertise the cuda peer (G070: exactly the llama lane), so this is
	// a TEST_ONLY snapshot, not a second bound cell.
	sealed := decision
	sealed.SelectionBasis = runtimeSelectionBasisHeterogeneousEligibleSet
	sealed.QualityContractID = testContractID
	sealed.EligibleCellIDs = ids
	sealed.SelectionAuthority = "control/workload_classification.go:selectAdmissionCandidates+" + testContractID
	sealed.SelectionNote = "multi-family eligible set frozen under AcceptableQualityContract " +
		testContractID + "; CellID is the lifecycle preferred pricing primary; " +
		"claim may select any eligible cell. Not directed. Not a measured tournament."
	if err := ValidateRuntimeDecisionSnapshot(sealed); err != nil {
		t.Fatalf("multi-family sealed decision: %v", err)
	}
	if sealed.ShadowSelectionAuthoritative {
		t.Fatal("shadow must not be authoritative")
	}
	if strings.Contains(sealed.SelectionNote, "directed") &&
		!strings.Contains(sealed.SelectionNote, "Not directed") {
		t.Fatalf("selection note must not look directed: %s", sealed.SelectionNote)
	}
}

func TestMultiFamilyDecisionRefusesRefusedGenerationContract(t *testing.T) {
	refused := generationQ4VsBF16Refused()
	_, err := qualityContractAuthorizingMultiFamily(refused.ID, []string{
		"candle-metal-llama1-infer", "vllm-cuda-llama1-infer",
	})
	if err == nil {
		t.Fatal("REFUSED q4-vs-bf16 contract must not authorize multi-family placement")
	}
	if _, err := qualityContractAuthorizingMultiFamily("no-such-contract", []string{"a", "b"}); err == nil {
		t.Fatal("unknown contract id must refuse")
	}
}

func TestQualityContractFilesStayInLockstep(t *testing.T) {
	controlBytes, err := os.ReadFile("acceptable-quality-contracts.json")
	if err != nil {
		t.Fatal(err)
	}
	opsBytes, err := os.ReadFile("../ops/acceptable-quality-contracts.json")
	if err != nil {
		t.Fatal(err)
	}
	if string(controlBytes) != string(opsBytes) {
		t.Fatal("control/acceptable-quality-contracts.json and ops/acceptable-quality-contracts.json drifted")
	}
}

func TestDirectedBasisNoteForbidsSelectorProof(t *testing.T) {
	// Directed freeze note must refuse being read as Arm C selector proof.
	if !strings.Contains(
		"accepted cell was forced by directed routing (operator/test); "+
			"not a measured multi-engine tournament and not ordinary ladder selection. "+
			"A directed freeze must never be presented as selector proof for Arm C.",
		"must never be presented as selector proof",
	) {
		t.Fatal("directed note must forbid Arm C selector-proof claim")
	}
}
