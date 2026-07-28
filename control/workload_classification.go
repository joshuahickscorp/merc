package main

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"sort"
	"strings"
)

const (
	workloadDecisionVersion = 1
	workloadBindingVersion  = 1
	maxEmbedBatchSize       = 4096
)

// WorkloadBinding is the canonical request shape a quote commits to.
//
// Buyer budget, webhook delivery, idempotency and the quote handle are
// deliberately absent: changing any of them does not change what runs. Every
// field that can change runtime selection, verification work, privacy,
// placement, latency or price is present.
type WorkloadBinding struct {
	Version       int                `json:"version"`
	JobType       JobType            `json:"job_type"`
	Model         ModelRef           `json:"model"`
	Params        json.RawMessage    `json:"params,omitempty"`
	Constraints   JobConstraints     `json:"constraints"`
	Verification  VerificationPolicy `json:"verification"`
	Tier          string             `json:"tier"`
	MinReputation float32            `json:"min_reputation"`
	DeadlineSecs  int                `json:"deadline_secs"`
	InputSHA256   string             `json:"input_sha256"`
}

type WorkloadRuntimeCandidate struct {
	CellID          string   `json:"cell_id"`
	RuntimeID       string   `json:"runtime_id"`
	Engine          string   `json:"engine"`
	Device          string   `json:"device"`
	HardwareClasses []string `json:"hardware_classes"`
}

type WorkloadParallelism struct {
	Mode                 string `json:"mode"`
	TensorParallelDegree int    `json:"tensor_parallel_degree"`
}

// WorkloadDecision is server authority, not a caller label. It is derived from
// a canonical model/runtime cell plus the complete normalized request shape.
// Both the request-shape digest and a digest of this full derived decision are
// persisted on quotes. The full decision is frozen on jobs and exposed in the
// buyer receipt.
type WorkloadDecision struct {
	Version                    int                        `json:"version"`
	BindingSHA256              string                     `json:"binding_sha256"`
	Binding                    WorkloadBinding            `json:"binding"`
	WorkloadClass              string                     `json:"workload_class"`
	RuntimeJobType             string                     `json:"runtime_job_type"`
	ModelRevision              string                     `json:"model_revision"`
	RuntimeCandidates          []WorkloadRuntimeCandidate `json:"runtime_candidates"`
	MinimumMemoryGB            float64                    `json:"minimum_memory_gb"`
	Deterministic              bool                       `json:"deterministic"`
	ExactResultCacheEligible   bool                       `json:"exact_result_cache_eligible"`
	PrefixReuseEligible        bool                       `json:"prefix_reuse_eligible"`
	InflightCoalescingEligible bool                       `json:"inflight_coalescing_eligible"`
	VerificationStrategy       string                     `json:"verification_strategy"`
	LatencyClass               string                     `json:"latency_class"`
	PrivacyClass               string                     `json:"privacy_class"`
	Parallelism                WorkloadParallelism        `json:"parallelism"`
	Confidence                 float64                    `json:"confidence"`
	Evidence                   []string                   `json:"evidence"`
	Unknowns                   []string                   `json:"unknowns,omitempty"`
}

func canonicalizeJobParams(raw json.RawMessage) (json.RawMessage, error) {
	raw = bytes.TrimSpace(raw)
	if len(raw) == 0 || bytes.Equal(raw, []byte("null")) {
		return nil, nil
	}
	var value any
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	if err := dec.Decode(&value); err != nil {
		return nil, fmt.Errorf("params must be valid JSON: %w", err)
	}
	if _, ok := value.(map[string]any); !ok {
		return nil, errors.New("params must be a JSON object")
	}
	if err := requireJSONEOF(dec); err != nil {
		return nil, err
	}
	canonical, err := json.Marshal(value)
	if err != nil {
		return nil, fmt.Errorf("canonicalize params: %w", err)
	}
	return canonical, nil
}

func normalizeUniqueStrings(values []string, normalize func(string) string) []string {
	if len(values) == 0 {
		return nil
	}
	seen := make(map[string]struct{}, len(values))
	out := make([]string, 0, len(values))
	for _, value := range values {
		value = normalize(value)
		if _, exists := seen[value]; exists {
			continue
		}
		seen[value] = struct{}{}
		out = append(out, value)
	}
	sort.Strings(out)
	return out
}

func validDataCountry(value string) bool {
	if len(value) != 2 {
		return false
	}
	for _, r := range value {
		if r < 'A' || r > 'Z' {
			return false
		}
	}
	return true
}

func canonicalWorkloadBinding(sub jobSubmit, inputSHA256 string) (WorkloadBinding, error) {
	if len(inputSHA256) != sha256.Size*2 {
		return WorkloadBinding{}, errors.New("workload binding requires a SHA-256 input commitment")
	}
	if _, err := hex.DecodeString(inputSHA256); err != nil {
		return WorkloadBinding{}, errors.New("workload binding input commitment is not hexadecimal")
	}
	return WorkloadBinding{
		Version:       workloadBindingVersion,
		JobType:       sub.JobType,
		Model:         sub.Model,
		Params:        append(json.RawMessage(nil), sub.Params...),
		Constraints:   sub.Constraints,
		Verification:  sub.Verification,
		Tier:          sub.Tier,
		MinReputation: sub.MinReputation,
		DeadlineSecs:  sub.DeadlineSecs,
		InputSHA256:   inputSHA256,
	}, nil
}

func workloadBindingDigest(binding WorkloadBinding) (string, error) {
	if binding.Version != workloadBindingVersion {
		return "", fmt.Errorf("unsupported workload binding version %d", binding.Version)
	}
	blob, err := json.Marshal(binding)
	if err != nil {
		return "", fmt.Errorf("marshal workload binding: %w", err)
	}
	sum := sha256.Sum256(blob)
	return hex.EncodeToString(sum[:]), nil
}

func workloadDecisionDigest(decision WorkloadDecision) (string, error) {
	if decision.Version != workloadDecisionVersion {
		return "", fmt.Errorf("unsupported workload decision version %d", decision.Version)
	}
	blob, err := json.Marshal(decision)
	if err != nil {
		return "", fmt.Errorf("marshal workload decision: %w", err)
	}
	sum := sha256.Sum256(blob)
	return hex.EncodeToString(sum[:]), nil
}

func runtimeCapabilityForBinding(binding WorkloadBinding) (generatedRuntimeCapability, error) {
	var matches []generatedRuntimeCapability
	for _, candidate := range generatedAdvertisedRuntimeCapabilities {
		if candidate.Job == binding.JobType.Type && candidate.Model == binding.Model.Ref &&
			candidate.ModelKind == binding.Model.Kind {
			matches = append(matches, candidate)
		}
	}
	if len(matches) != 1 {
		return generatedRuntimeCapability{}, fmt.Errorf(
			"workload classification requires one runtime cell for job_type=%q model=%q kind=%q; found %d",
			binding.JobType.Type, binding.Model.Ref, binding.Model.Kind, len(matches))
	}
	return matches[0], nil
}

func modelRevisionFor(modelID string) string {
	for _, model := range runtimeAuthority.Models {
		if model.ID == modelID {
			return model.HFRevision
		}
	}
	return ""
}

func verificationStrategyFor(binding WorkloadBinding, capability generatedRuntimeCapability) string {
	parts := []string{capability.Verification}
	if binding.Verification.RedundancyFrac > 0 {
		parts = append(parts, "redundant_execution")
	}
	if binding.Verification.HoneypotFrac > 0 {
		parts = append(parts, "honeypot")
	}
	if binding.Verification.RedundancyFrac <= 0 && binding.Verification.HoneypotFrac <= 0 {
		parts = append(parts, "platform_honeypot_floor")
	}
	return strings.Join(parts, "+")
}

func buildWorkloadDecisionFromBinding(binding WorkloadBinding) (WorkloadDecision, error) {
	digest, err := workloadBindingDigest(binding)
	if err != nil {
		return WorkloadDecision{}, err
	}
	capability, err := runtimeCapabilityForBinding(binding)
	if err != nil {
		return WorkloadDecision{}, err
	}

	workloadClass := ""
	switch {
	case capability.Runner == "embed" && capability.Job == "embed":
		workloadClass = "embeddings"
	case capability.Runner == "batch_infer" && capability.Job == "batch_infer":
		workloadClass = "batch_generation"
	default:
		return WorkloadDecision{}, fmt.Errorf(
			"runtime cell %q has no admitted workload classifier for runner=%q job=%q",
			capability.ID, capability.Runner, capability.Job)
	}

	minMemory := capability.MinMemoryGB
	if requested := float64(binding.Constraints.MinMemoryGB); requested > minMemory {
		minMemory = requested
	}
	privacyClass := "unrestricted_residency"
	if len(binding.Constraints.DataResidency) > 0 {
		privacyClass = "country_restricted"
	}
	latencyClass := "standard_batch"
	switch binding.Tier {
	case "priority":
		latencyClass = "priority_queue"
	case "trusted":
		latencyClass = "trusted_supply"
	}
	prefixEligible := workloadClass == "batch_generation"

	return WorkloadDecision{
		Version:        workloadDecisionVersion,
		BindingSHA256:  digest,
		Binding:        binding,
		WorkloadClass:  workloadClass,
		RuntimeJobType: capability.Job,
		ModelRevision:  modelRevisionFor(binding.Model.Ref),
		RuntimeCandidates: []WorkloadRuntimeCandidate{{
			CellID: capability.ID, RuntimeID: capability.Runtime, Engine: capability.Engine,
			Device:          capability.Device,
			HardwareClasses: append([]string(nil), capability.HardwareClasses...),
		}},
		MinimumMemoryGB:            minMemory,
		Deterministic:              true,
		ExactResultCacheEligible:   true,
		PrefixReuseEligible:        prefixEligible,
		InflightCoalescingEligible: false,
		VerificationStrategy:       verificationStrategyFor(binding, capability),
		LatencyClass:               latencyClass,
		PrivacyClass:               privacyClass,
		Parallelism: WorkloadParallelism{
			Mode: "independent_task_fanout", TensorParallelDegree: 1,
		},
		Confidence: 0.9,
		Evidence: []string{
			"runtime cell resolved from the embedded, digest-addressed authority matrix",
			"model revision and runtime job type are server-derived",
			"request parameters, placement constraints, verification policy and input commitment are digest-bound",
		},
		Unknowns: []string{
			"data rights are not expressible on the current batch API",
			"power and provider-cost estimates are not part of workload classification v1",
		},
	}, nil
}

func buildWorkloadDecision(sub jobSubmit, inputSHA256 string) (WorkloadDecision, error) {
	binding, err := canonicalWorkloadBinding(sub, inputSHA256)
	if err != nil {
		return WorkloadDecision{}, err
	}
	return buildWorkloadDecisionFromBinding(binding)
}

func ValidateWorkloadDecisionSnapshot(decision WorkloadDecision) error {
	if err := ValidateFrozenWorkloadDecisionSnapshot(decision); err != nil {
		return err
	}
	if decision.Version != workloadDecisionVersion {
		return fmt.Errorf("unsupported workload decision version %d", decision.Version)
	}
	expected, err := buildWorkloadDecisionFromBinding(decision.Binding)
	if err != nil {
		return err
	}
	gotJSON, err := json.Marshal(decision)
	if err != nil {
		return err
	}
	wantJSON, err := json.Marshal(expected)
	if err != nil {
		return err
	}
	if !bytes.Equal(gotJSON, wantJSON) {
		return errors.New("workload decision does not match its canonical binding and runtime authority")
	}
	return nil
}

// ValidateFrozenWorkloadDecisionSnapshot verifies a decision using only the
// authority frozen inside it. Historical jobs must remain readable after the
// live runtime matrix advances; re-deriving them from today's matrix would turn
// an immutable receipt into a moving target.
//
// Admission still uses ValidateWorkloadDecisionSnapshot, which additionally
// proves that a new decision matches the current embedded runtime authority.
func ValidateFrozenWorkloadDecisionSnapshot(decision WorkloadDecision) error {
	if decision.Version != workloadDecisionVersion {
		return fmt.Errorf("unsupported workload decision version %d", decision.Version)
	}
	bindingSHA256, err := workloadBindingDigest(decision.Binding)
	if err != nil {
		return err
	}
	if decision.BindingSHA256 != bindingSHA256 {
		return errors.New("workload decision binding digest does not match its frozen binding")
	}
	if len(decision.Binding.InputSHA256) != sha256.Size*2 {
		return errors.New("workload decision input commitment is not a SHA-256 digest")
	}
	if _, err := hex.DecodeString(decision.Binding.InputSHA256); err != nil {
		return errors.New("workload decision input commitment is not hexadecimal")
	}
	if decision.WorkloadClass == "" || decision.RuntimeJobType == "" ||
		decision.ModelRevision == "" || decision.VerificationStrategy == "" ||
		decision.LatencyClass == "" || decision.PrivacyClass == "" {
		return errors.New("workload decision is missing frozen classification authority")
	}
	if len(decision.RuntimeCandidates) == 0 {
		return errors.New("workload decision has no frozen runtime candidate")
	}
	for _, candidate := range decision.RuntimeCandidates {
		if candidate.CellID == "" || candidate.RuntimeID == "" ||
			candidate.Engine == "" || candidate.Device == "" {
			return errors.New("workload decision contains an incomplete runtime candidate")
		}
	}
	if math.IsNaN(decision.MinimumMemoryGB) || math.IsInf(decision.MinimumMemoryGB, 0) ||
		decision.MinimumMemoryGB < 0 {
		return errors.New("workload decision has invalid minimum memory")
	}
	if math.IsNaN(decision.Confidence) || math.IsInf(decision.Confidence, 0) ||
		decision.Confidence < 0 || decision.Confidence > 1 {
		return errors.New("workload decision has invalid confidence")
	}
	if decision.Parallelism.Mode == "" || decision.Parallelism.TensorParallelDegree < 1 {
		return errors.New("workload decision has invalid parallelism authority")
	}
	return nil
}

type workloadInputError struct {
	Line int
	Msg  string
}

func (e *workloadInputError) Error() string {
	return fmt.Sprintf("invalid workload input at line %d: %s", e.Line, e.Msg)
}

// validateWorkloadJSONLRecord mirrors the two admitted agent runners. It stops
// malformed or shape-less records before storage, scheduling and billing,
// rather than letting a worker discover them after a contract exists.
func validateWorkloadJSONLRecord(jobType string, line []byte, lineNumber int) error {
	var object map[string]json.RawMessage
	if err := json.Unmarshal(line, &object); err != nil {
		return &workloadInputError{Line: lineNumber, Msg: "record must be a JSON object"}
	}
	if object == nil {
		return &workloadInputError{Line: lineNumber, Msg: "record must be a JSON object"}
	}
	if raw, exists := object["id"]; exists && !bytes.Equal(bytes.TrimSpace(raw), []byte("null")) {
		var id string
		if err := json.Unmarshal(raw, &id); err != nil {
			return &workloadInputError{Line: lineNumber, Msg: "id must be a string when present"}
		}
	}

	bodyFound := false
	for _, field := range []string{"text", "prompt"} {
		raw, exists := object[field]
		if !exists || bytes.Equal(bytes.TrimSpace(raw), []byte("null")) {
			continue
		}
		var body string
		if err := json.Unmarshal(raw, &body); err != nil {
			return &workloadInputError{Line: lineNumber, Msg: field + " must be a string"}
		}
		if strings.TrimSpace(body) != "" {
			bodyFound = true
		}
	}
	if !bodyFound {
		return &workloadInputError{
			Line: lineNumber,
			Msg:  fmt.Sprintf("%s records require a non-empty text or prompt string", jobType),
		}
	}
	return nil
}

func validateWorkloadJSONL(jobType string, data []byte) error {
	records := 0
	for index, raw := range bytes.Split(data, []byte("\n")) {
		line := bytes.TrimSpace(bytes.TrimSuffix(raw, []byte("\r")))
		if len(line) == 0 {
			continue
		}
		records++
		if err := validateWorkloadJSONLRecord(jobType, line, index+1); err != nil {
			return err
		}
	}
	if records == 0 {
		return &workloadInputError{Line: 1, Msg: "at least one non-blank JSONL record is required"}
	}
	return nil
}

func validFiniteUnitFloat(value float32) bool {
	v := float64(value)
	return !math.IsNaN(v) && !math.IsInf(v, 0) && v >= 0 && v <= 1
}
