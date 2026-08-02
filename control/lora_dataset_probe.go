package main

import (
	"bufio"
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"sort"
	"strconv"
	"strings"
)

// loraDatasetSchemaV1 is a deliberately small, complete schema language for
// the bounded local LoRA probe. It is not JSON Schema: accepting a broad
// standard while implementing only part of it would falsely certify a dataset
// that the trainer interprets differently. A future execution cell may support
// a richer, versioned schema only by adding a new explicit probe version.
const loraDatasetSchemaV1 = "MERC_LORA_DATASET_SCHEMA_V1"

type loraDatasetSchema struct {
	Version  string            `json:"version"`
	Fields   map[string]string `json:"fields"`
	Required []string          `json:"required"`
}

type loraDatasetProbeResult struct {
	rows   int64
	rowIDs map[string]struct{}
}

// validateBoundedLoRAProjectDatasets validates only data that static
// inspection retained completely. The probe neither runs a trainer nor grants
// a runtime cell authority. Its evidence is useful only for refusing malformed
// or overlapping training/held-out data before either data set leaves the
// buyer's machine.
func validateBoundedLoRAProjectDatasets(files []projectFile, ir *ProjectWorkloadIR) {
	byPath := make(map[string]projectFile, len(files))
	for _, file := range files {
		byPath[file.rel] = file
	}
	for stepIndex := range ir.Steps {
		step := &ir.Steps[stepIndex]
		if step.Kind != "lora_training" || step.LoRA == nil {
			continue
		}
		result, err := validateBoundedLoRADatasetStep(byPath, *step.LoRA)
		if err != nil {
			step.ResourceEstimate.State = "DATASET_SCHEMA_REFUSED"
			ir.RefusalReasons = append(ir.RefusalReasons,
				fmt.Sprintf("LoRA dataset probe for step %s refused: %v", step.ID, err))
			ir.Probe.Observations = append(ir.Probe.Observations,
				fmt.Sprintf("lora:%s:dataset_schema_refused", step.ID))
			continue
		}
		if step.ResourceEstimate.State == "SHAPE_MEASURED_CALIBRATION_REQUIRED" {
			step.ResourceEstimate.State = "DATASET_SCHEMA_VALIDATED_CALIBRATION_REQUIRED"
		}
		step.ResourceEstimate.SampleRecords = result.rows
		ir.Probe.Observations = append(ir.Probe.Observations,
			fmt.Sprintf("lora:%s:dataset_schema=%s:training_rows=%d:held_out_rows=%d",
				step.ID, loraDatasetSchemaV1, result.trainingRows, result.heldOutRows))
	}
	sort.Strings(ir.Probe.Observations)
}

type loraDatasetStepResult struct {
	trainingRows int64
	heldOutRows  int64
	rows         int64
}

func validateBoundedLoRADatasetStep(files map[string]projectFile, contract ProjectIRLoRA) (loraDatasetStepResult, error) {
	schemaFile, err := completeLoRAProjectFile(files, contract.DatasetSchema.Artifact, "dataset schema")
	if err != nil {
		return loraDatasetStepResult{}, err
	}
	var schema loraDatasetSchema
	if err := decodeStrictJSONObject(schemaFile.content, &schema); err != nil {
		return loraDatasetStepResult{}, fmt.Errorf("dataset schema is invalid: %w", err)
	}
	if err := validateLoRADatasetSchema(schema); err != nil {
		return loraDatasetStepResult{}, err
	}
	trainingFile, err := completeLoRAProjectFile(files, contract.TrainingSet.Artifact, "training set")
	if err != nil {
		return loraDatasetStepResult{}, err
	}
	heldOutFile, err := completeLoRAProjectFile(files, contract.HeldOutSet.Artifact, "held-out set")
	if err != nil {
		return loraDatasetStepResult{}, err
	}
	training, err := validateLoRAJSONL(trainingFile.content, schema, "training set")
	if err != nil {
		return loraDatasetStepResult{}, err
	}
	heldOut, err := validateLoRAJSONL(heldOutFile.content, schema, "held-out set")
	if err != nil {
		return loraDatasetStepResult{}, err
	}
	for rowID := range heldOut.rowIDs {
		if _, exists := training.rowIDs[rowID]; exists {
			return loraDatasetStepResult{}, errors.New("training and held-out sets contain the same canonical record")
		}
	}
	return loraDatasetStepResult{
		trainingRows: training.rows,
		heldOutRows:  heldOut.rows,
		rows:         training.rows + heldOut.rows,
	}, nil
}

func completeLoRAProjectFile(files map[string]projectFile, artifact, role string) (projectFile, error) {
	if !strings.HasPrefix(artifact, "project://") {
		return projectFile{}, fmt.Errorf("%s is not a project artifact", role)
	}
	file, exists := files[strings.TrimPrefix(artifact, "project://")]
	if !exists {
		return projectFile{}, fmt.Errorf("%s was absent from the inspected project", role)
	}
	if file.size == 0 || file.size > projectMaxFileBytes || int64(len(file.content)) != file.size {
		return projectFile{}, fmt.Errorf("%s requires complete validation within the %d-byte bounded probe", role, projectMaxFileBytes)
	}
	return file, nil
}

func validateLoRADatasetSchema(schema loraDatasetSchema) error {
	if schema.Version != loraDatasetSchemaV1 {
		return fmt.Errorf("dataset schema version must be %s", loraDatasetSchemaV1)
	}
	if len(schema.Fields) == 0 || len(schema.Fields) > 128 {
		return errors.New("dataset schema requires 1..128 named fields")
	}
	for name, kind := range schema.Fields {
		if !projectRenderCameraPattern.MatchString(name) {
			return fmt.Errorf("dataset schema field %q is not a stable identifier", name)
		}
		switch kind {
		case "string", "number", "boolean", "object":
		default:
			return fmt.Errorf("dataset schema field %q has unsupported type %q", name, kind)
		}
	}
	required := normalizeUniqueStrings(schema.Required, strings.TrimSpace)
	if len(required) == 0 || len(required) != len(schema.Required) {
		return errors.New("dataset schema required fields must be non-empty and unique")
	}
	for _, field := range required {
		if _, exists := schema.Fields[field]; !exists {
			return fmt.Errorf("dataset schema required field %q is not defined", field)
		}
	}
	return nil
}

func validateLoRAJSONL(content []byte, schema loraDatasetSchema, role string) (loraDatasetProbeResult, error) {
	scanner := bufio.NewScanner(bytes.NewReader(content))
	scanner.Buffer(make([]byte, 4<<10), projectMaxFileBytes+1)
	result := loraDatasetProbeResult{rowIDs: make(map[string]struct{})}
	lineNumber := 0
	for scanner.Scan() {
		lineNumber++
		line := bytes.TrimSpace(scanner.Bytes())
		if len(line) == 0 {
			continue
		}
		if err := rejectDuplicateJSONKeys(line); err != nil {
			return loraDatasetProbeResult{}, fmt.Errorf("%s line %d is not strict JSON: %w", role, lineNumber, err)
		}
		var record map[string]json.RawMessage
		if err := json.Unmarshal(line, &record); err != nil {
			return loraDatasetProbeResult{}, fmt.Errorf("%s line %d is not a JSON object", role, lineNumber)
		}
		if record == nil {
			return loraDatasetProbeResult{}, fmt.Errorf("%s line %d is not a JSON object", role, lineNumber)
		}
		for field := range record {
			if _, exists := schema.Fields[field]; !exists {
				return loraDatasetProbeResult{}, fmt.Errorf("%s line %d has field %q absent from the closed schema", role, lineNumber, field)
			}
		}
		for _, field := range schema.Required {
			if _, exists := record[field]; !exists {
				return loraDatasetProbeResult{}, fmt.Errorf("%s line %d is missing required field %q", role, lineNumber, field)
			}
		}
		for field, raw := range record {
			if err := validateLoRAJSONValue(raw, schema.Fields[field]); err != nil {
				return loraDatasetProbeResult{}, fmt.Errorf("%s line %d field %q: %w", role, lineNumber, field, err)
			}
		}
		canonical, err := json.Marshal(record)
		if err != nil {
			return loraDatasetProbeResult{}, fmt.Errorf("%s line %d cannot be canonically represented", role, lineNumber)
		}
		digest := sha256.Sum256(canonical)
		result.rowIDs[hex.EncodeToString(digest[:])] = struct{}{}
		result.rows++
	}
	if err := scanner.Err(); err != nil {
		return loraDatasetProbeResult{}, fmt.Errorf("%s exceeds the bounded JSONL record limit: %w", role, err)
	}
	if result.rows == 0 {
		return loraDatasetProbeResult{}, fmt.Errorf("%s contains no JSONL records", role)
	}
	return result, nil
}

func validateLoRAJSONValue(raw json.RawMessage, kind string) error {
	switch kind {
	case "string":
		var value string
		if err := json.Unmarshal(raw, &value); err != nil {
			return errors.New("must be a string")
		}
	case "boolean":
		var value bool
		if err := json.Unmarshal(raw, &value); err != nil {
			return errors.New("must be a boolean")
		}
	case "number":
		decoder := json.NewDecoder(bytes.NewReader(raw))
		decoder.UseNumber()
		var value any
		if err := decoder.Decode(&value); err != nil {
			return errors.New("must be a JSON number")
		}
		number, ok := value.(json.Number)
		if !ok {
			return errors.New("must be a JSON number")
		}
		parsed, err := strconv.ParseFloat(number.String(), 64)
		if err != nil || math.IsNaN(parsed) || math.IsInf(parsed, 0) {
			return errors.New("must be a finite JSON number")
		}
	case "object":
		if err := rejectDuplicateJSONKeys(raw); err != nil {
			return errors.New("must be a strict JSON object")
		}
	default:
		return errors.New("has unsupported schema field type")
	}
	return nil
}
