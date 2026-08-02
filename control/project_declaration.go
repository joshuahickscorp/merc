package main

import (
	"errors"
	"fmt"
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
