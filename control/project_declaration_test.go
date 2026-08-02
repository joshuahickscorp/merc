package main

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"strings"
	"testing"
)

func projectDeclarationFixture() ProjectDeclaration {
	seed := int64(0)
	return ProjectDeclaration{
		Version: 1,
		Steps: []ProjectIRStep{
			{ID: "render", Kind: "media_rendering", DependsOn: []string{"extract"}, Inputs: []string{"project://scene", "project://scene.blend", "project://engine.bin", "project://textures/albedo.png", "project://plugins/denoise.bin", "project://fonts/inter.ttf"}, Outputs: []string{"project://frames"}, RuntimeContract: strings.Repeat("a", 64), ModelContract: strings.Repeat("b", 64), ResourceEstimate: ProjectIRResourceEstimate{State: "BOUNDED_PROBE_REQUIRED"}, Parallelism: "INDEPENDENT", CheckpointPolicy: "REQUIRED", Verification: "frame_hash_and_quality", Rendering: &ProjectIRRendering{
				Scene:           ProjectIRArtifactPin{Artifact: "project://scene.blend", SHA256: strings.Repeat("e", 64)},
				Assets:          []ProjectIRArtifactPin{{Artifact: "project://textures/albedo.png", SHA256: strings.Repeat("f", 64)}},
				Engine:          ProjectIRArtifactPin{Artifact: "project://engine.bin", SHA256: strings.Repeat("a", 64)},
				Plugins:         []ProjectIRArtifactPin{{Artifact: "project://plugins/denoise.bin", SHA256: strings.Repeat("b", 64)}},
				Fonts:           []ProjectIRArtifactPin{{Artifact: "project://fonts/inter.ttf", SHA256: strings.Repeat("c", 64)}},
				ColorManagement: "ACEScg-v1.3", FrameStart: 1, FrameEnd: 24, Cameras: []string{"hero"}, Width: 1920, Height: 1080,
				TileWidth: 256, TileHeight: 256, Samples: 64, Seed: &seed, Mode: "FINAL",
				Assembly: "FRAME_CAMERA_TILE_LEXICOGRAPHIC_V1",
			}},
			{ID: "extract", Kind: "structured_extraction", Inputs: []string{"project://input"}, Outputs: []string{"project://scene"}, RuntimeContract: strings.Repeat("c", 64), ModelContract: strings.Repeat("d", 64), ResourceEstimate: ProjectIRResourceEstimate{State: "BOUNDED_PROBE_REQUIRED"}, Parallelism: "SINGLE_DEVICE", CheckpointPolicy: "NOT_APPLICABLE", Verification: "schema"},
		},
		Privacy:   ProjectIRPrivacy{Egress: "DENY", DataLocation: "CA"},
		Quality:   ProjectIRQuality{Requirement: "buyer-fixture-v1", Verification: "independent"},
		Result:    ProjectIRResult{Contract: "artifact-set-v1", Retention: "30d", Delivery: "object-store"},
		Economics: ProjectIREconomics{Currency: "cad", MaximumBuyerPriceNanos: 50_000_000, SupplierFloor: "UNRESOLVED_REFUSE", MercContribution: "UNRESOLVED_REFUSE"},
	}
}

func writeRenderFixtureAssets(t *testing.T, root string, declaration *ProjectDeclaration) {
	t.Helper()
	pins := map[string]*ProjectIRArtifactPin{
		"scene.blend":         &declaration.Steps[0].Rendering.Scene,
		"engine.bin":          &declaration.Steps[0].Rendering.Engine,
		"textures/albedo.png": &declaration.Steps[0].Rendering.Assets[0],
		"plugins/denoise.bin": &declaration.Steps[0].Rendering.Plugins[0],
		"fonts/inter.ttf":     &declaration.Steps[0].Rendering.Fonts[0],
	}
	for path, pin := range pins {
		contents := "project render asset: " + path
		writeProjectFixture(t, root, path, contents)
		digest := sha256.Sum256([]byte(contents))
		pin.SHA256 = hex.EncodeToString(digest[:])
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
	writeRenderFixtureAssets(t, root, &declaration)
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
	if render := ir.Steps[1].Rendering; render == nil || render.WorkPlan == nil ||
		render.WorkPlan.UnitCount != 960 || render.WorkPlan.TileColumns != 8 || render.WorkPlan.TileRows != 5 {
		t.Fatalf("compiler did not derive the bounded deterministic render plan: %+v", render)
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

func TestProjectDeclarationRequiresArtifactBoundDependencies(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(*ProjectDeclaration)
		want   string
	}{
		{
			name: "embedding vectors cannot become text input",
			mutate: func(d *ProjectDeclaration) {
				d.Steps[1].Kind = "embeddings"
				d.Steps[0].Kind = "batch_inference"
				d.Steps[0].Rendering = nil
			},
			want: "embedding-vector artifact",
		},
		{
			name: "dependency does not produce an input",
			mutate: func(d *ProjectDeclaration) {
				d.Steps[0].Inputs = d.Steps[0].Inputs[1:]
			},
			want: "consumes none of its output artifacts",
		},
		{
			name: "producer omitted from dependencies",
			mutate: func(d *ProjectDeclaration) {
				d.Steps[0].DependsOn = nil
			},
			want: "without declaring that dependency",
		},
		{
			name: "duplicate artifact producer",
			mutate: func(d *ProjectDeclaration) {
				d.Steps[0].Outputs = []string{"project://scene"}
			},
			want: "produced by both",
		},
		{
			name: "non project input",
			mutate: func(d *ProjectDeclaration) {
				d.Steps[0].Inputs[0] = "https://example.invalid/scene"
			},
			want: "invalid input artifact",
		},
		{
			name: "authority file output",
			mutate: func(d *ProjectDeclaration) {
				d.Steps[0].Outputs = []string{"project://merc.project.json"}
			},
			want: "invalid output artifact",
		},
		{
			name: "buyer may not supply a derived render plan",
			mutate: func(d *ProjectDeclaration) {
				d.Steps[0].Rendering.WorkPlan = &ProjectIRRenderWorkPlan{Version: renderWorkPlanVersion}
			},
			want: "work_plan is compiler-derived",
		},
		{
			name: "render resolution is required",
			mutate: func(d *ProjectDeclaration) {
				d.Steps[0].Rendering.Width = 0
			},
			want: "width must be an even pixel value",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			declaration := projectDeclarationFixture()
			tc.mutate(&declaration)
			err := validateProjectDeclaration(&declaration)
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("validateProjectDeclaration() error = %v, want %q", err, tc.want)
			}
		})
	}
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

func TestRenderWorkPlanRefusesCombinatorialDecomposition(t *testing.T) {
	render := *projectDeclarationFixture().Steps[0].Rendering
	render.Width, render.Height = 32768, 32768
	render.TileWidth, render.TileHeight = 16, 16
	if _, err := deriveProjectRenderWorkPlan(render); err == nil || !strings.Contains(err.Error(), "exceeds") {
		t.Fatalf("unbounded render decomposition was accepted: %v", err)
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

func TestProjectRenderingDeclarationBindsEveryExecutableAsset(t *testing.T) {
	t.Run("missing rendering contract is refused before any probe or price", func(t *testing.T) {
		declaration := projectDeclarationFixture()
		declaration.Steps[0].Rendering = nil
		if err := validateProjectDeclaration(&declaration); err == nil ||
			!strings.Contains(err.Error(), "requires a rendering contract") {
			t.Fatalf("media step without render authority validation error = %v", err)
		}
	})

	t.Run("inventory pin must name the inspected bytes", func(t *testing.T) {
		root := t.TempDir()
		declaration := projectDeclarationFixture()
		writeRenderFixtureAssets(t, root, &declaration)
		declaration.Steps[0].Rendering.Engine = ProjectIRArtifactPin{
			Artifact: "project://engine-replaced.bin", SHA256: strings.Repeat("a", 64),
		}
		declaration.Steps[0].Inputs = append(declaration.Steps[0].Inputs, "project://engine-replaced.bin")
		writeDeclarationFixture(t, root, declaration)
		writeProjectFixture(t, root, "pipeline.py", "json_schema rendering")
		if _, err := compileProject(projectCompileOptions{Root: root}); err == nil ||
			!strings.Contains(err.Error(), "pins absent project artifact") {
			t.Fatalf("absent render asset compile error = %v", err)
		}
	})

	t.Run("declaration digest cannot lie about an inspected asset", func(t *testing.T) {
		root := t.TempDir()
		declaration := projectDeclarationFixture()
		writeRenderFixtureAssets(t, root, &declaration)
		declaration.Steps[0].Rendering.Scene.SHA256 = strings.Repeat("0", 64)
		writeDeclarationFixture(t, root, declaration)
		writeProjectFixture(t, root, "pipeline.py", "json_schema rendering")
		if _, err := compileProject(projectCompileOptions{Root: root}); err == nil ||
			!strings.Contains(err.Error(), "project inventory") {
			t.Fatalf("render asset digest mismatch compile error = %v", err)
		}
	})

	t.Run("valid rendering authority remains in the frozen IR", func(t *testing.T) {
		root := t.TempDir()
		declaration := projectDeclarationFixture()
		writeRenderFixtureAssets(t, root, &declaration)
		writeDeclarationFixture(t, root, declaration)
		writeProjectFixture(t, root, "pipeline.py", "json_schema rendering")
		ir, err := compileProject(projectCompileOptions{Root: root})
		if err != nil {
			t.Fatal(err)
		}
		var render *ProjectIRStep
		for i := range ir.Steps {
			if ir.Steps[i].ID == "render" {
				render = &ir.Steps[i]
			}
		}
		if render == nil || render.Rendering == nil || render.Rendering.Mode != "FINAL" ||
			render.Rendering.Scene.SHA256 != declaration.Steps[0].Rendering.Scene.SHA256 {
			t.Fatalf("compiled IR lost render authority: %+v", render)
		}
		if !strings.Contains(strings.Join(ir.RefusalReasons, "\n"), "resolved to 0 routable cells") {
			t.Fatalf("rendering IR became executable without a governed cell: %+v", ir.RefusalReasons)
		}
	})
}

func loraProjectDeclarationFixture() ProjectDeclaration {
	seed := int64(42)
	return ProjectDeclaration{
		Version: 1,
		Steps: []ProjectIRStep{{
			ID: "train", Kind: "lora_training",
			Inputs:          []string{"project://train.jsonl", "project://schema.json"},
			Outputs:         []string{"project://adapter.safetensors"},
			RuntimeContract: strings.Repeat("1", 64), ModelContract: strings.Repeat("2", 64),
			ResourceEstimate: ProjectIRResourceEstimate{State: "BOUNDED_PROBE_REQUIRED"},
			Parallelism:      "SINGLE_DEVICE", CheckpointPolicy: "REQUIRED", Verification: "independent_held_out_eval",
			LoRA: &ProjectIRLoRA{
				TrainingSet:         ProjectIRArtifactPin{Artifact: "project://train.jsonl", SHA256: strings.Repeat("3", 64)},
				HeldOutSet:          ProjectIRArtifactPin{Artifact: "project://held-out.jsonl", SHA256: strings.Repeat("4", 64)},
				DatasetSchema:       ProjectIRArtifactPin{Artifact: "project://schema.json", SHA256: strings.Repeat("5", 64)},
				DatasetRights:       "buyer-attests-commercial-training-rights-v1",
				BaselineModelSHA256: strings.Repeat("6", 64),
				Rank:                16, Alpha: 32, Epochs: 3, Seed: &seed, TargetModules: []string{"q_proj", "v_proj"},
				EvaluationMetric: "held_out_exact_match", MetricDirection: "HIGHER_IS_BETTER", RequiredImprovement: 0.02,
				EvaluatorSeparation: "DIFFERENT_SUPPLIER_ACCOUNT", AdapterOutput: "project://adapter.safetensors",
				Deployment: "GOVERNED_ONLY", Revocation: "IMMEDIATE_ON_AUTHORIZATION_LOSS",
			},
		}},
		Privacy:   ProjectIRPrivacy{Egress: "DENY", DataLocation: "CA"},
		Quality:   ProjectIRQuality{Requirement: "held-out-improvement-v1", Verification: "independent"},
		Result:    ProjectIRResult{Contract: "adapter-artifact-v1", Retention: "30d", Delivery: "object-store"},
		Economics: ProjectIREconomics{Currency: "cad", MaximumBuyerPriceNanos: 50_000_000, SupplierFloor: "UNRESOLVED_REFUSE", MercContribution: "UNRESOLVED_REFUSE"},
	}
}

func writeLoRAFixtureAssets(t *testing.T, root string, declaration *ProjectDeclaration) {
	t.Helper()
	pins := map[string]*ProjectIRArtifactPin{
		"train.jsonl":    &declaration.Steps[0].LoRA.TrainingSet,
		"held-out.jsonl": &declaration.Steps[0].LoRA.HeldOutSet,
		"schema.json":    &declaration.Steps[0].LoRA.DatasetSchema,
	}
	for path, pin := range pins {
		contents := "LoRA fixture artifact: " + path
		writeProjectFixture(t, root, path, contents)
		digest := sha256.Sum256([]byte(contents))
		pin.SHA256 = hex.EncodeToString(digest[:])
	}
}

func writeSchemaValidatedLoRAFixture(t *testing.T, root string, declaration *ProjectDeclaration, heldOut string) {
	t.Helper()
	assets := map[string]string{
		"schema.json":    `{"version":"MERC_LORA_DATASET_SCHEMA_V1","fields":{"input":"string","target":"string"},"required":["input","target"]}`,
		"train.jsonl":    "{\"input\":\"first prompt\",\"target\":\"first completion\"}\n{\"input\":\"second prompt\",\"target\":\"second completion\"}\n",
		"held-out.jsonl": heldOut,
	}
	pins := map[string]*ProjectIRArtifactPin{
		"train.jsonl":    &declaration.Steps[0].LoRA.TrainingSet,
		"held-out.jsonl": &declaration.Steps[0].LoRA.HeldOutSet,
		"schema.json":    &declaration.Steps[0].LoRA.DatasetSchema,
	}
	for path, contents := range assets {
		writeProjectFixture(t, root, path, contents)
		digest := sha256.Sum256([]byte(contents))
		pins[path].SHA256 = hex.EncodeToString(digest[:])
	}
	writeDeclarationFixture(t, root, *declaration)
}

func TestProjectLoRAOutcomeContractKeepsEvaluationSeparate(t *testing.T) {
	t.Run("held-out data is never a training input", func(t *testing.T) {
		declaration := loraProjectDeclarationFixture()
		declaration.Steps[0].Inputs = append(declaration.Steps[0].Inputs, "project://held-out.jsonl")
		if err := validateProjectDeclaration(&declaration); err == nil || !strings.Contains(err.Error(), "held-out set") {
			t.Fatalf("LoRA declaration leaked its held-out set: %v", err)
		}
	})

	t.Run("independence cannot be weakened in the declaration", func(t *testing.T) {
		declaration := loraProjectDeclarationFixture()
		declaration.Steps[0].LoRA.EvaluatorSeparation = "SAME_SUPPLIER_ALLOWED"
		if err := validateProjectDeclaration(&declaration); err == nil || !strings.Contains(err.Error(), "DIFFERENT_SUPPLIER_ACCOUNT") {
			t.Fatalf("LoRA declaration weakened evaluator independence: %v", err)
		}
	})

	t.Run("compiler carries pinned LoRA authority but refuses an absent runtime", func(t *testing.T) {
		root := t.TempDir()
		declaration := loraProjectDeclarationFixture()
		writeLoRAFixtureAssets(t, root, &declaration)
		writeDeclarationFixture(t, root, declaration)
		writeProjectFixture(t, root, "training.py", "lora adapter training")
		ir, err := compileProject(projectCompileOptions{Root: root})
		if err != nil {
			t.Fatal(err)
		}
		if len(ir.Steps) != 1 || ir.Steps[0].LoRA == nil ||
			ir.Steps[0].LoRA.HeldOutSet.SHA256 != declaration.Steps[0].LoRA.HeldOutSet.SHA256 {
			t.Fatalf("compiled IR lost LoRA authority: %+v", ir.Steps)
		}
		if !strings.Contains(strings.Join(ir.RefusalReasons, "\n"), "resolved to 0 routable cells") {
			t.Fatalf("LoRA IR became executable without a governed cell: %+v", ir.RefusalReasons)
		}
	})
}

func TestLoRAProbeValidatesOnlyCompleteSchemaBoundDatasets(t *testing.T) {
	validHeldOut := "{\"input\":\"held out prompt\",\"target\":\"held out completion\"}\n"
	t.Run("complete distinct records are validated without making training executable", func(t *testing.T) {
		root := t.TempDir()
		declaration := loraProjectDeclarationFixture()
		writeSchemaValidatedLoRAFixture(t, root, &declaration, validHeldOut)
		proposal, err := compileProject(projectCompileOptions{Root: root})
		if err != nil {
			t.Fatal(err)
		}
		probed, err := compileProject(projectCompileOptions{Root: root, ProbeRequested: true, BuyerApprovedIRSHA256: proposal.IRSHA256})
		if err != nil {
			t.Fatal(err)
		}
		if got := probed.Steps[0].ResourceEstimate.State; got != "DATASET_SCHEMA_VALIDATED_CALIBRATION_REQUIRED" {
			t.Fatalf("LoRA dataset probe state=%q, want exact bounded schema validation", got)
		}
		if probed.Steps[0].ResourceEstimate.SampleRecords != 3 {
			t.Fatalf("LoRA dataset record evidence=%+v, want training plus held-out records", probed.Steps[0].ResourceEstimate)
		}
		joined := strings.Join(probed.RefusalReasons, "\n")
		if !strings.Contains(joined, "resolved to 0 routable cells") || strings.Contains(joined, "LoRA dataset probe") {
			t.Fatalf("dataset validation made LoRA runnable or hid runtime refusal: %+v", probed.RefusalReasons)
		}
	})

	t.Run("held-out overlap is refused before a trainer can receive data", func(t *testing.T) {
		root := t.TempDir()
		declaration := loraProjectDeclarationFixture()
		writeSchemaValidatedLoRAFixture(t, root, &declaration, "{\"target\":\"first completion\",\"input\":\"first prompt\"}\n")
		proposal, err := compileProject(projectCompileOptions{Root: root})
		if err != nil {
			t.Fatal(err)
		}
		probed, err := compileProject(projectCompileOptions{Root: root, ProbeRequested: true, BuyerApprovedIRSHA256: proposal.IRSHA256})
		if err != nil {
			t.Fatal(err)
		}
		if got := probed.Steps[0].ResourceEstimate.State; got != "DATASET_SCHEMA_REFUSED" {
			t.Fatalf("overlapping held-out data state=%q", got)
		}
		if !strings.Contains(strings.Join(probed.RefusalReasons, "\n"), "same canonical record") {
			t.Fatalf("overlapping held-out data was not explicitly refused: %+v", probed.RefusalReasons)
		}
	})

	t.Run("duplicates within either dataset are refused before training or evaluation", func(t *testing.T) {
		for _, tc := range []struct {
			name     string
			training string
			heldOut  string
		}{
			{
				name: "training duplicate",
				training: "{\"input\":\"first prompt\",\"target\":\"first completion\"}\n" +
					"{\"target\":\"first completion\",\"input\":\"first prompt\"}\n",
				heldOut: validHeldOut,
			},
			{
				name:     "held-out duplicate",
				training: "{\"input\":\"first prompt\",\"target\":\"first completion\"}\n",
				heldOut: "{\"input\":\"held out prompt\",\"target\":\"held out completion\"}\n" +
					"{\"target\":\"held out completion\",\"input\":\"held out prompt\"}\n",
			},
		} {
			t.Run(tc.name, func(t *testing.T) {
				root := t.TempDir()
				declaration := loraProjectDeclarationFixture()
				writeSchemaValidatedLoRAFixture(t, root, &declaration, tc.heldOut)
				writeProjectFixture(t, root, "train.jsonl", tc.training)
				trainDigest := sha256.Sum256([]byte(tc.training))
				declaration.Steps[0].LoRA.TrainingSet.SHA256 = hex.EncodeToString(trainDigest[:])
				writeDeclarationFixture(t, root, declaration)

				proposal, err := compileProject(projectCompileOptions{Root: root})
				if err != nil {
					t.Fatal(err)
				}
				probed, err := compileProject(projectCompileOptions{Root: root, ProbeRequested: true, BuyerApprovedIRSHA256: proposal.IRSHA256})
				if err != nil {
					t.Fatal(err)
				}
				if got := probed.Steps[0].ResourceEstimate.State; got != "DATASET_SCHEMA_REFUSED" {
					t.Fatalf("duplicate %s state=%q", tc.name, got)
				}
				if !strings.Contains(strings.Join(probed.RefusalReasons, "\n"), "repeats a canonical record") {
					t.Fatalf("duplicate %s was not explicitly refused: %+v", tc.name, probed.RefusalReasons)
				}
			})
		}
	})
}
