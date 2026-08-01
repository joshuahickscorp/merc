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
			{ID: "render", Kind: "media_rendering", DependsOn: []string{"extract"}, Inputs: []string{"project://scene"}, Outputs: []string{"project://frames"}, RuntimeContract: strings.Repeat("a", 64), ModelContract: strings.Repeat("b", 64), ResourceEstimate: "BOUNDED_PROBE_REQUIRED", Parallelism: "INDEPENDENT", CheckpointPolicy: "REQUIRED", Verification: "frame_hash_and_quality"},
			{ID: "extract", Kind: "structured_extraction", Inputs: []string{"project://input"}, Outputs: []string{"project://scene"}, RuntimeContract: strings.Repeat("c", 64), ModelContract: strings.Repeat("d", 64), ResourceEstimate: "BOUNDED_PROBE_REQUIRED", Parallelism: "SINGLE_DEVICE", CheckpointPolicy: "NOT_APPLICABLE", Verification: "schema"},
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
	if !strings.Contains(strings.Join(ir.RefusalReasons, "\n"), "not been resolved") {
		t.Fatal("buyer-declared contract hashes became server authority")
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
