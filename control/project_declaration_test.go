package main

import (
	"encoding/json"
	"strings"
	"testing"
)

func projectDeclarationFixture() ProjectDeclaration {
	return ProjectDeclaration{
		Version: 1,
		Steps: []ProjectIRStep{
			{ID: "render", Kind: "media_rendering", DependsOn: []string{"extract"}, Inputs: []string{"project://scene"}, Outputs: []string{"project://frames"}, RuntimeContract: strings.Repeat("a", 64), ModelContract: strings.Repeat("b", 64), ResourceEstimate: ProjectIRResourceEstimate{State: "BOUNDED_PROBE_REQUIRED"}, Parallelism: "INDEPENDENT", CheckpointPolicy: "REQUIRED", Verification: "frame_hash_and_quality"},
			{ID: "extract", Kind: "structured_extraction", Inputs: []string{"project://input"}, Outputs: []string{"project://scene"}, RuntimeContract: strings.Repeat("c", 64), ModelContract: strings.Repeat("d", 64), ResourceEstimate: ProjectIRResourceEstimate{State: "BOUNDED_PROBE_REQUIRED"}, Parallelism: "SINGLE_DEVICE", CheckpointPolicy: "NOT_APPLICABLE", Verification: "schema"},
		},
		Privacy:   ProjectIRPrivacy{Egress: "DENY", DataLocation: "CA"},
		Quality:   ProjectIRQuality{Requirement: "buyer-fixture-v1", Verification: "independent"},
		Result:    ProjectIRResult{Contract: "artifact-set-v1", Retention: "30d", Delivery: "object-store"},
		Economics: ProjectIREconomics{Currency: "cad", MaximumBuyerPriceNanos: 50_000_000, SupplierFloor: "UNRESOLVED_REFUSE", MercContribution: "UNRESOLVED_REFUSE"},
	}
}

func writeDeclarationFixture(t *testing.T, root string, declaration ProjectDeclaration) {
	t.Helper()
	blob, err := json.Marshal(declaration)
	if err != nil {
		t.Fatal(err)
	}
	writeProjectFixture(t, root, projectDeclarationName, string(blob))
}

func TestProjectDeclarationSuppliesEvidenceBoundDAG(t *testing.T) {
	root := t.TempDir()
	declaration := projectDeclarationFixture()
	writeDeclarationFixture(t, root, declaration)
	writeProjectFixture(t, root, "pipeline.py", "json_schema rendering")
	ir, err := compileProject(projectCompileOptions{Root: root})
	if err != nil {
		t.Fatal(err)
	}
	if len(ir.Steps) != 2 || ir.Steps[0].ID != "extract" || ir.Steps[1].ID != "render" {
		t.Fatalf("declared graph was not canonicalized: %+v", ir.Steps)
	}
	if len(ir.Steps[1].DependsOn) != 1 || ir.Steps[1].DependsOn[0] != "extract" {
		t.Fatalf("declared dataflow was lost: %+v", ir.Steps)
	}
	if ir.Privacy.Egress != "DENY" || ir.Economics.Currency != "cad" || ir.Economics.MaximumBuyerPriceNanos != 50_000_000 {
		t.Fatalf("buyer constraints were not frozen: %+v", ir)
	}
	if !strings.Contains(strings.Join(ir.RefusalReasons, "\n"), "resolved to 0 routable cells") {
		t.Fatal("buyer-declared contract hashes became server authority")
	}
}

func TestProjectDeclarationResolvesExactAdvertisedContract(t *testing.T) {
	contracts, err := advertisedProjectRuntimeContracts()
	if err != nil {
		t.Fatal(err)
	}
	var contract ProjectRuntimeContract
	for _, candidate := range contracts {
		if candidate.WorkloadKind == "embeddings" {
			contract = candidate
			break
		}
	}
	if contract.RuntimeID == "" {
		t.Fatal("no advertised embeddings contract")
	}
	root := t.TempDir()
	declaration := projectDeclarationFixture()
	declaration.Steps = []ProjectIRStep{{
		ID: "embed", Kind: contract.WorkloadKind,
		Inputs: []string{"project://input"}, Outputs: []string{"project://vectors"},
		RuntimeContract: contract.RuntimeContractSHA256, ModelContract: contract.ModelContractSHA256,
		ResourceEstimate: ProjectIRResourceEstimate{State: "BOUNDED_PROBE_REQUIRED"}, Parallelism: "INDEPENDENT",
		CheckpointPolicy: "NOT_APPLICABLE", Verification: contract.Verification,
	}}
	writeDeclarationFixture(t, root, declaration)
	writeProjectFixture(t, root, "pipeline.py", "embedding")
	ir, err := compileProject(projectCompileOptions{Root: root})
	if err != nil {
		t.Fatal(err)
	}
	if len(ir.Steps) != 1 || ir.Steps[0].RuntimeID != contract.RuntimeID || ir.Steps[0].ModelID != contract.ModelID {
		t.Fatalf("exact advertised contract did not resolve: %+v", ir.Steps)
	}
	if strings.Contains(strings.Join(ir.RefusalReasons, "\n"), "runtime/model contract pair") {
		t.Fatalf("resolved contract retained resolution refusal: %+v", ir.RefusalReasons)
	}
}

func TestProjectDeclarationRefusesCycleAndMoneyAuthority(t *testing.T) {
	t.Run("cycle", func(t *testing.T) {
		root := t.TempDir()
		declaration := projectDeclarationFixture()
		declaration.Steps[1].DependsOn = []string{"render"}
		writeDeclarationFixture(t, root, declaration)
		if _, err := compileProject(projectCompileOptions{Root: root}); err == nil || !strings.Contains(err.Error(), "cycle") {
			t.Fatalf("cyclic project compiled: %v", err)
		}
	})
	t.Run("pricing authority", func(t *testing.T) {
		root := t.TempDir()
		declaration := projectDeclarationFixture()
		declaration.Economics.PricingDecisionSHA256 = strings.Repeat("e", 64)
		writeDeclarationFixture(t, root, declaration)
		if _, err := compileProject(projectCompileOptions{Root: root}); err == nil || !strings.Contains(err.Error(), "may not supply server PricingDecision") {
			t.Fatalf("buyer supplied pricing authority: %v", err)
		}
	})
}

func TestProjectDeclarationRequiresFixedPointCeiling(t *testing.T) {
	root := t.TempDir()
	declaration := projectDeclarationFixture()
	declaration.Economics.MaximumBuyerPriceNanos = 0
	writeDeclarationFixture(t, root, declaration)
	if _, err := compileProject(projectCompileOptions{Root: root}); err == nil || !strings.Contains(err.Error(), "maximum_buyer_price_nanos") {
		t.Fatalf("zero buyer ceiling compiled: %v", err)
	}
}

func TestDeclaredStepProbeIsScopedToItsInputArtifact(t *testing.T) {
	contracts, err := advertisedProjectRuntimeContracts()
	if err != nil {
		t.Fatal(err)
	}
	var contract ProjectRuntimeContract
	for _, candidate := range contracts {
		if candidate.WorkloadKind == "embeddings" {
			contract = candidate
			break
		}
	}
	if contract.RuntimeID == "" {
		t.Fatal("no embeddings contract")
	}
	root := t.TempDir()
	declaration := projectDeclarationFixture()
	declaration.Steps = []ProjectIRStep{{
		ID: "embed", Kind: "embeddings", Inputs: []string{"project://samples/input.jsonl"},
		Outputs: []string{"project://vectors"}, RuntimeContract: contract.RuntimeContractSHA256,
		ModelContract:    contract.ModelContractSHA256,
		ResourceEstimate: ProjectIRResourceEstimate{State: "BOUNDED_PROBE_REQUIRED"},
		Parallelism:      "INDEPENDENT", CheckpointPolicy: "NOT_APPLICABLE", Verification: contract.Verification,
	}}
	writeDeclarationFixture(t, root, declaration)
	writeProjectFixture(t, root, "pipeline.py", "embedding")
	input := "{\"text\":\"alpha\"}\n{\"text\":\"beta\"}\n"
	writeProjectFixture(t, root, "samples/input.jsonl", input)
	proposal, err := compileProject(projectCompileOptions{Root: root})
	if err != nil {
		t.Fatal(err)
	}
	probed, err := compileProject(projectCompileOptions{
		Root: root, ProbeRequested: true, BuyerApprovedIRSHA256: proposal.IRSHA256,
	})
	if err != nil {
		t.Fatal(err)
	}
	resource := probed.Steps[0].ResourceEstimate
	if resource.State != "SHAPE_MEASURED_CALIBRATION_REQUIRED" ||
		resource.ArtifactBytes != int64(len(input)) || resource.SampleRecords != 2 ||
		resource.ProbeKind != "NON_EXECUTING_FILE_SHAPE_V1" {
		t.Fatalf("step probe escaped or missed its input artifact: %+v", resource)
	}
}
