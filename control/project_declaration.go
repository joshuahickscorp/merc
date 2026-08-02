package main

import (
	"errors"
	"fmt"
	"math"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"
)

const projectDeclarationName = "merc.project.json"

var projectStepIDPattern = regexp.MustCompile(`^[a-z][a-z0-9_-]{0,63}$`)

var projectWorkloadKinds = map[string]bool{
	"realtime_inference": true, "batch_inference": true, "batch_compute": true,
	"embeddings": true, "structured_extraction": true, "media_rendering": true,
	"image_video": true, "lora_training": true, "model_evaluation": true,
	"bounded_container": true, "service_deployment": true,
}

type ProjectDeclaration struct {
	Version   int                `json:"version"`
	Steps     []ProjectIRStep    `json:"steps"`
	Privacy   ProjectIRPrivacy   `json:"privacy"`
	Quality   ProjectIRQuality   `json:"quality"`
	Deadline  *ProjectIRDeadline `json:"deadline,omitempty"`
	Result    ProjectIRResult    `json:"result"`
	Economics ProjectIREconomics `json:"economics"`
}

func projectDeclarationFromFiles(files []projectFile) (ProjectDeclaration, bool, error) {
	for _, file := range files {
		if file.rel != projectDeclarationName {
			continue
		}
		if file.size > projectMaxFileBytes || int64(len(file.content)) != file.size {
			return ProjectDeclaration{}, false, errors.New("merc.project.json exceeds the complete static-inspection bound")
		}
		var declaration ProjectDeclaration
		if err := decodeStrictJSONObject(file.content, &declaration); err != nil {
			return ProjectDeclaration{}, false, fmt.Errorf("decode merc.project.json: %w", err)
		}
		if err := validateProjectDeclaration(&declaration); err != nil {
			return ProjectDeclaration{}, false, fmt.Errorf("validate merc.project.json: %w", err)
		}
		return declaration, true, nil
	}
	return ProjectDeclaration{}, false, nil
}

func validateProjectDeclaration(declaration *ProjectDeclaration) error {
	if declaration.Version != projectIRVersion {
		return fmt.Errorf("version must be %d", projectIRVersion)
	}
	if len(declaration.Steps) == 0 || len(declaration.Steps) > 256 {
		return errors.New("steps must contain 1..256 entries")
	}
	ids := make(map[string]bool, len(declaration.Steps))
	for i := range declaration.Steps {
		step := &declaration.Steps[i]
		if !projectStepIDPattern.MatchString(step.ID) || ids[step.ID] {
			return fmt.Errorf("step %d has invalid or duplicate id %q", i, step.ID)
		}
		ids[step.ID] = true
		if !projectWorkloadKinds[step.Kind] {
			return fmt.Errorf("step %s has unsupported kind %q", step.ID, step.Kind)
		}
		if !validSHA256(step.RuntimeContract) || !validSHA256(step.ModelContract) {
			return fmt.Errorf("step %s runtime/model contracts must be SHA-256 identities", step.ID)
		}
		if step.RuntimeID != "" || step.ModelID != "" {
			return fmt.Errorf("step %s may not supply resolved runtime/model ids", step.ID)
		}
		if step.ResourceEstimate != (ProjectIRResourceEstimate{State: "BOUNDED_PROBE_REQUIRED"}) {
			return fmt.Errorf("step %s resource_estimate must be BOUNDED_PROBE_REQUIRED", step.ID)
		}
		if step.Topology != nil {
			return fmt.Errorf("step %s topology is compiler-derived and may not be supplied by the buyer declaration", step.ID)
		}
		switch step.Parallelism {
		case "INDEPENDENT", "TIGHT", "SINGLE_DEVICE":
		default:
			return fmt.Errorf("step %s has unsupported parallelism %q", step.ID, step.Parallelism)
		}
		if step.CheckpointPolicy != "REQUIRED" && step.CheckpointPolicy != "NOT_APPLICABLE" {
			return fmt.Errorf("step %s has unsupported checkpoint policy", step.ID)
		}
		if strings.TrimSpace(step.Verification) == "" {
			return fmt.Errorf("step %s verification is required", step.ID)
		}
		step.DependsOn = normalizeUniqueStrings(step.DependsOn, strings.TrimSpace)
		step.Inputs = normalizeUniqueStrings(step.Inputs, strings.TrimSpace)
		step.Outputs = normalizeUniqueStrings(step.Outputs, strings.TrimSpace)
		if step.Kind == "media_rendering" {
			if err := validateProjectRendering(step); err != nil {
				return fmt.Errorf("step %s rendering: %w", step.ID, err)
			}
			if step.LoRA != nil {
				return fmt.Errorf("step %s supplies LoRA authority for media rendering", step.ID)
			}
		} else if step.Kind == "lora_training" {
			if err := validateProjectLoRA(step); err != nil {
				return fmt.Errorf("step %s LoRA: %w", step.ID, err)
			}
			if step.Rendering != nil {
				return fmt.Errorf("step %s supplies rendering authority for LoRA training", step.ID)
			}
		} else if step.Rendering != nil {
			return fmt.Errorf("step %s supplies rendering authority for non-rendering kind %q", step.ID, step.Kind)
		} else if step.LoRA != nil {
			return fmt.Errorf("step %s supplies LoRA authority for non-LoRA kind %q", step.ID, step.Kind)
		}
		if len(step.Inputs) == 0 || len(step.Outputs) == 0 {
			return fmt.Errorf("step %s requires input and output artifacts", step.ID)
		}
	}
	for _, step := range declaration.Steps {
		for _, dependency := range step.DependsOn {
			if dependency == step.ID || !ids[dependency] {
				return fmt.Errorf("step %s has invalid dependency %q", step.ID, dependency)
			}
		}
	}
	if err := validateProjectDAG(declaration.Steps); err != nil {
		return err
	}
	if err := validateProjectArtifactDataflow(declaration.Steps); err != nil {
		return err
	}
	switch declaration.Privacy.Egress {
	case "DENY", "ALLOWLIST", "BUYER_APPROVED":
	default:
		return errors.New("privacy.egress must be DENY, ALLOWLIST, or BUYER_APPROVED")
	}
	if strings.TrimSpace(declaration.Privacy.DataLocation) == "" {
		return errors.New("privacy.data_location is required")
	}
	if strings.TrimSpace(declaration.Quality.Requirement) == "" || strings.TrimSpace(declaration.Quality.Verification) == "" {
		return errors.New("quality requirement and verification are required")
	}
	if declaration.Deadline != nil {
		if _, err := time.Parse(time.RFC3339, declaration.Deadline.RFC3339); err != nil {
			return errors.New("deadline must be RFC3339")
		}
	}
	if declaration.Result.Contract == "" || declaration.Result.Retention == "" || declaration.Result.Delivery == "" {
		return errors.New("result contract, retention, and delivery are required")
	}
	if _, err := ParseCurrency(declaration.Economics.Currency); err != nil {
		return fmt.Errorf("economics currency: %w", err)
	}
	declaration.Economics.Currency = strings.ToLower(declaration.Economics.Currency)
	if declaration.Economics.MaximumBuyerPriceNanos <= 0 {
		return errors.New("economics.maximum_buyer_price_nanos must be positive")
	}
	if declaration.Economics.PricingDecisionSHA256 != "" {
		return errors.New("buyer declaration may not supply server PricingDecision authority")
	}
	if declaration.Economics.SupplierFloor != "UNRESOLVED_REFUSE" || declaration.Economics.MercContribution != "UNRESOLVED_REFUSE" {
		return errors.New("buyer declaration may not supply supplier floor or Merc contribution authority")
	}
	sort.Slice(declaration.Steps, func(i, j int) bool { return declaration.Steps[i].ID < declaration.Steps[j].ID })
	return nil
}

// validateProjectRendering gives a render declaration enough shape to be
// safely materialised later. It does not make the step schedulable: runtime,
// verification, topology, and fixed-point economics are separate authorities.
func validateProjectRendering(step *ProjectIRStep) error {
	render := step.Rendering
	if render == nil {
		return errors.New("media_rendering requires a rendering contract")
	}
	render.Scene.Artifact = strings.TrimSpace(render.Scene.Artifact)
	render.Scene.SHA256 = strings.ToLower(strings.TrimSpace(render.Scene.SHA256))
	render.Engine.Artifact = strings.TrimSpace(render.Engine.Artifact)
	render.Engine.SHA256 = strings.ToLower(strings.TrimSpace(render.Engine.SHA256))
	if err := validateProjectRenderPin("scene", render.Scene); err != nil {
		return err
	}
	if err := validateProjectRenderPin("engine", render.Engine); err != nil {
		return err
	}
	render.ColorManagement = strings.TrimSpace(render.ColorManagement)
	if render.ColorManagement == "" {
		return errors.New("color_management is required")
	}
	if render.FrameStart < 0 || render.FrameEnd < render.FrameStart || render.FrameEnd-render.FrameStart >= 1_000_000 {
		return fmt.Errorf("frame range %d..%d is invalid or exceeds the one-million-frame bound", render.FrameStart, render.FrameEnd)
	}
	if render.WorkPlan != nil {
		return errors.New("work_plan is compiler-derived and may not be supplied by the buyer declaration")
	}
	for name, value := range map[string]int{"width": render.Width, "height": render.Height} {
		if value < 16 || value > 32768 || value%2 != 0 {
			return fmt.Errorf("%s must be an even pixel value in [16,32768]", name)
		}
	}
	render.Cameras = normalizeUniqueStrings(render.Cameras, strings.TrimSpace)
	if len(render.Cameras) == 0 {
		return errors.New("at least one camera is required")
	}
	for _, camera := range render.Cameras {
		if !projectRenderCameraPattern.MatchString(camera) {
			return fmt.Errorf("camera %q is not a stable render identifier", camera)
		}
	}
	if (render.TileWidth == 0) != (render.TileHeight == 0) {
		return errors.New("tile_width and tile_height must be set together")
	}
	for name, value := range map[string]int{"tile_width": render.TileWidth, "tile_height": render.TileHeight} {
		if value != 0 && (value < 16 || value > 8192 || value%16 != 0) {
			return fmt.Errorf("%s must be 0 or a 16-pixel multiple in [16,8192]", name)
		}
	}
	if render.TileWidth != 0 && (render.TileWidth > render.Width || render.TileHeight > render.Height) {
		return errors.New("render tile dimensions may not exceed render resolution")
	}
	if render.Samples < 1 || render.Samples > 1_000_000 {
		return errors.New("samples must be in [1,1000000]")
	}
	if render.Seed == nil {
		return errors.New("seed is required for independently reproducible output")
	}
	switch render.Mode {
	case "PREVIEW", "FINAL":
	default:
		return fmt.Errorf("mode must be PREVIEW or FINAL, got %q", render.Mode)
	}
	if render.Assembly != "FRAME_CAMERA_TILE_LEXICOGRAPHIC_V1" {
		return errors.New("assembly must be FRAME_CAMERA_TILE_LEXICOGRAPHIC_V1")
	}
	if err := normalizeProjectRenderPins("assets", &render.Assets); err != nil {
		return err
	}
	if err := normalizeProjectRenderPins("plugins", &render.Plugins); err != nil {
		return err
	}
	if err := normalizeProjectRenderPins("fonts", &render.Fonts); err != nil {
		return err
	}
	seen := make(map[string]bool)
	for _, pin := range projectRenderPins(*render) {
		if seen[pin.Artifact] {
			return fmt.Errorf("render authority repeats artifact %q across roles", pin.Artifact)
		}
		seen[pin.Artifact] = true
		if !containsString(step.Inputs, pin.Artifact) {
			return fmt.Errorf("pinned render asset %q is not declared in step inputs", pin.Artifact)
		}
	}
	return nil
}

var projectRenderCameraPattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._:-]{0,127}$`)

func validateProjectRenderPin(role string, pin ProjectIRArtifactPin) error {
	if !validProjectArtifactRef(pin.Artifact) || pin.Artifact == "project://input" {
		return fmt.Errorf("%s artifact %q must be a project-local static artifact", role, pin.Artifact)
	}
	if !validSHA256(pin.SHA256) {
		return fmt.Errorf("%s artifact %q requires a SHA-256 identity", role, pin.Artifact)
	}
	return nil
}

func normalizeProjectRenderPins(role string, pins *[]ProjectIRArtifactPin) error {
	seen := make(map[string]bool, len(*pins))
	for i := range *pins {
		pin := &(*pins)[i]
		pin.Artifact = strings.TrimSpace(pin.Artifact)
		pin.SHA256 = strings.ToLower(strings.TrimSpace(pin.SHA256))
		if err := validateProjectRenderPin(role, *pin); err != nil {
			return err
		}
		if seen[pin.Artifact] {
			return fmt.Errorf("%s repeats artifact %q", role, pin.Artifact)
		}
		seen[pin.Artifact] = true
	}
	sort.Slice(*pins, func(i, j int) bool { return (*pins)[i].Artifact < (*pins)[j].Artifact })
	return nil
}

func projectRenderPins(render ProjectIRRendering) []ProjectIRArtifactPin {
	pins := make([]ProjectIRArtifactPin, 0, 2+len(render.Assets)+len(render.Plugins)+len(render.Fonts))
	pins = append(pins, render.Scene, render.Engine)
	pins = append(pins, render.Assets...)
	pins = append(pins, render.Plugins...)
	pins = append(pins, render.Fonts...)
	return pins
}

func containsString(values []string, target string) bool {
	for _, value := range values {
		if value == target {
			return true
		}
	}
	return false
}

func validateProjectLoRA(step *ProjectIRStep) error {
	contract := step.LoRA
	if contract == nil {
		return errors.New("lora_training requires an outcome contract")
	}
	for _, binding := range []struct {
		role string
		pin  *ProjectIRArtifactPin
	}{
		{"training_set", &contract.TrainingSet},
		{"held_out_set", &contract.HeldOutSet},
		{"dataset_schema", &contract.DatasetSchema},
	} {
		role, pin := binding.role, binding.pin
		pin.Artifact = strings.TrimSpace(pin.Artifact)
		pin.SHA256 = strings.ToLower(strings.TrimSpace(pin.SHA256))
		if err := validateProjectRenderPin(role, *pin); err != nil {
			return err
		}
	}
	if contract.TrainingSet.Artifact == contract.HeldOutSet.Artifact ||
		contract.TrainingSet.Artifact == contract.DatasetSchema.Artifact ||
		contract.HeldOutSet.Artifact == contract.DatasetSchema.Artifact {
		return errors.New("training, held-out, and schema artifacts must be distinct")
	}
	if !containsString(step.Inputs, contract.TrainingSet.Artifact) ||
		!containsString(step.Inputs, contract.DatasetSchema.Artifact) {
		return errors.New("training set and dataset schema must be declared training inputs")
	}
	if containsString(step.Inputs, contract.HeldOutSet.Artifact) {
		return errors.New("held-out set must not be delivered to the training worker")
	}
	contract.DatasetRights = strings.TrimSpace(contract.DatasetRights)
	if contract.DatasetRights == "" {
		return errors.New("dataset_rights is required")
	}
	contract.BaselineModelSHA256 = strings.ToLower(strings.TrimSpace(contract.BaselineModelSHA256))
	if !validSHA256(contract.BaselineModelSHA256) {
		return errors.New("baseline_model_sha256 must be a SHA-256 identity")
	}
	if contract.Rank < 1 || contract.Rank > 1024 || contract.Alpha < 1 || contract.Alpha > 4096 ||
		contract.Epochs < 1 || contract.Epochs > 1000 {
		return errors.New("rank, alpha, or epochs are outside governed LoRA bounds")
	}
	if contract.Seed == nil {
		return errors.New("seed is required for reproducible training")
	}
	contract.TargetModules = normalizeUniqueStrings(contract.TargetModules, strings.TrimSpace)
	if len(contract.TargetModules) == 0 {
		return errors.New("target_modules is required")
	}
	for _, module := range contract.TargetModules {
		if !projectRenderCameraPattern.MatchString(module) {
			return fmt.Errorf("target module %q is not a stable identifier", module)
		}
	}
	contract.EvaluationMetric = strings.TrimSpace(contract.EvaluationMetric)
	if contract.EvaluationMetric == "" {
		return errors.New("evaluation_metric is required")
	}
	if contract.MetricDirection != "HIGHER_IS_BETTER" && contract.MetricDirection != "LOWER_IS_BETTER" {
		return errors.New("metric_direction must be HIGHER_IS_BETTER or LOWER_IS_BETTER")
	}
	if math.IsNaN(contract.RequiredImprovement) || math.IsInf(contract.RequiredImprovement, 0) ||
		contract.RequiredImprovement < 0 || contract.RequiredImprovement > 10 {
		return errors.New("required_improvement must be finite and in [0,10]")
	}
	if contract.EvaluatorSeparation != "DIFFERENT_SUPPLIER_ACCOUNT" {
		return errors.New("evaluator_separation must be DIFFERENT_SUPPLIER_ACCOUNT")
	}
	if !validProjectArtifactRef(contract.AdapterOutput) || !containsString(step.Outputs, contract.AdapterOutput) {
		return errors.New("adapter_output must be one of this step's project outputs")
	}
	if contract.Deployment != "GOVERNED_ONLY" || contract.Revocation != "IMMEDIATE_ON_AUTHORIZATION_LOSS" {
		return errors.New("adapter deployment/revocation must remain GOVERNED_ONLY and IMMEDIATE_ON_AUTHORIZATION_LOSS")
	}
	return nil
}

func projectLoRAPins(contract ProjectIRLoRA) []ProjectIRArtifactPin {
	return []ProjectIRArtifactPin{contract.TrainingSet, contract.HeldOutSet, contract.DatasetSchema}
}

func validateProjectDAG(steps []ProjectIRStep) error {
	dependencies := make(map[string][]string, len(steps))
	for _, step := range steps {
		dependencies[step.ID] = step.DependsOn
	}
	state := make(map[string]uint8, len(steps))
	var visit func(string) error
	visit = func(id string) error {
		if state[id] == 1 {
			return fmt.Errorf("project graph contains a cycle at %s", id)
		}
		if state[id] == 2 {
			return nil
		}
		state[id] = 1
		for _, dependency := range dependencies[id] {
			if err := visit(dependency); err != nil {
				return err
			}
		}
		state[id] = 2
		return nil
	}
	for id := range dependencies {
		if err := visit(id); err != nil {
			return err
		}
	}
	return nil
}

// validateProjectArtifactDataflow turns the declared graph into a graph of
// actual, project-scoped artifacts. A dependency with no consumed output is a
// cosmetic ordering edge, and an input produced by an undeclared step can race
// its producer. Neither is a schedulable project graph.
//
// This validates the IR only. The project executor submits dependency-free
// roots first; each downstream step is eligible only after its declared inputs
// are materialized from receipt-bound upstream jobs and re-quoted.
func validateProjectArtifactDataflow(steps []ProjectIRStep) error {
	producer := make(map[string]string)
	byID := make(map[string]ProjectIRStep, len(steps))
	for _, step := range steps {
		byID[step.ID] = step
		for _, output := range step.Outputs {
			if !validProjectArtifactRef(output) || output == "project://input" ||
				output == "project://"+projectDeclarationName {
				return fmt.Errorf("step %s has invalid output artifact %q", step.ID, output)
			}
			if prior, exists := producer[output]; exists {
				return fmt.Errorf("output artifact %q is produced by both %s and %s", output, prior, step.ID)
			}
			producer[output] = step.ID
		}
	}

	for _, step := range steps {
		dependencies := make(map[string]bool, len(step.DependsOn))
		for _, dependency := range step.DependsOn {
			dependencies[dependency] = true
		}
		consumed := make(map[string]bool, len(step.DependsOn))
		for _, input := range step.Inputs {
			if !validProjectArtifactRef(input) {
				return fmt.Errorf("step %s has invalid input artifact %q", step.ID, input)
			}
			if producedBy, exists := producer[input]; exists {
				producerStep := byID[producedBy]
				if producerStep.Kind == "embeddings" && (step.Kind == "embeddings" || step.Kind == "batch_inference") {
					return fmt.Errorf("step %s consumes embedding-vector artifact %q from %s as text input; no governed adapter exists", step.ID, input, producedBy)
				}
				if producedBy == step.ID {
					return fmt.Errorf("step %s reads its own output artifact %q", step.ID, input)
				}
				if !dependencies[producedBy] {
					return fmt.Errorf("step %s consumes %q from %s without declaring that dependency", step.ID, input, producedBy)
				}
				consumed[producedBy] = true
			}
		}
		for _, dependency := range step.DependsOn {
			if !consumed[dependency] {
				return fmt.Errorf("step %s depends on %s but consumes none of its output artifacts", step.ID, dependency)
			}
		}
	}
	return nil
}

func validProjectArtifactRef(ref string) bool {
	if !strings.HasPrefix(ref, "project://") {
		return false
	}
	rel := strings.TrimPrefix(ref, "project://")
	if rel == "input" {
		return true
	}
	clean := filepath.Clean(rel)
	return clean != "." && !filepath.IsAbs(clean) && clean != ".." &&
		!strings.HasPrefix(clean, ".."+string(filepath.Separator)) && clean == rel
}

func applyProjectDeclaration(ir *ProjectWorkloadIR, declaration ProjectDeclaration) {
	ir.Steps = append([]ProjectIRStep(nil), declaration.Steps...)
	ir.Privacy = declaration.Privacy
	ir.Quality = declaration.Quality
	ir.Deadline = declaration.Deadline
	ir.Result = declaration.Result
	ir.Economics = declaration.Economics
	ir.Unknowns = []string{"bounded resource probe", "outcome-linked cost and duration calibration"}
}
