package main

import (
	"errors"
	"fmt"
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
		if step.ResourceEstimate != "BOUNDED_PROBE_REQUIRED" {
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

func applyProjectDeclaration(ir *ProjectWorkloadIR, declaration ProjectDeclaration) {
	ir.Steps = append([]ProjectIRStep(nil), declaration.Steps...)
	ir.Privacy = declaration.Privacy
	ir.Quality = declaration.Quality
	ir.Deadline = declaration.Deadline
	ir.Result = declaration.Result
	ir.Economics = declaration.Economics
	ir.Unknowns = []string{
		"declared runtime/model contract resolution against server authority",
		"bounded resource probe", "outcome-linked cost and duration calibration",
	}
	ir.RefusalReasons = append(ir.RefusalReasons,
		"declared runtime/model contracts have not been resolved against server authority")
}
