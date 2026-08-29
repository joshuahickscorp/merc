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
	// Objective is what the buyer asked to optimize for. Empty on historical
	// bindings and on requests that omit it. Changing it changes the digest.
	Objective   string `json:"objective,omitempty"`
	InputSHA256 string `json:"input_sha256"`
}

type WorkloadRuntimeCandidate struct {
	CellID    string `json:"cell_id"`
	RuntimeID string `json:"runtime_id"`
	Engine    string `json:"engine"`
	// ModelKind is the artifact format THIS cell loads, which is not necessarily
	// the one the buyer named.
	//
	// Format belongs to the (runtime, model) pair: candle serves MiniLM from
	// safetensors and llama.cpp serves the same logical model from a GGUF, and
	// the two agree to 0.999998 cosine. Matching a worker against the BINDING's
	// kind therefore let a buyer pick the runtime by naming a format — a request
	// for all-minilm-l6-v2 as `hf` could never reach llama.cpp, whatever the
	// evidence said. The frozen cell is what will execute, so the cell supplies
	// the kind and the binding keeps recording what the buyer asked for.
	//
	// Empty on rows frozen before this field existed; readers fall back to the
	// binding's kind, which is what those rows meant.
	ModelKind       string   `json:"model_kind,omitempty"`
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
	Version           int                        `json:"version"`
	BindingSHA256     string                     `json:"binding_sha256"`
	Binding           WorkloadBinding            `json:"binding"`
	WorkloadClass     string                     `json:"workload_class"`
	RuntimeJobType    string                     `json:"runtime_job_type"`
	ModelRevision     string                     `json:"model_revision"`
	RuntimeCandidates []WorkloadRuntimeCandidate `json:"runtime_candidates"`
	// QualityContractID cites the AcceptableQualityContract that authorized
	// multi-family substitutability when len(RuntimeCandidates) > 1 across
	// device families. Empty for ordinary single-cell freezes and for
	// directed single-cell freezes.
	QualityContractID string `json:"quality_contract_id,omitempty"`
	// DirectedCellID names the cell an operator or a test forced this workload
	// onto, or is empty for ordinary admission. Frozen into the decision so a
	// receipt records that the choice was made by directed routing rather than by
	// the catalogue, and so the stored decision reconstructs as itself.
	DirectedCellID             string              `json:"directed_cell_id,omitempty"`
	MinimumMemoryGB            float64             `json:"minimum_memory_gb"`
	Deterministic              bool                `json:"deterministic"`
	ExactResultCacheEligible   bool                `json:"exact_result_cache_eligible"`
	PrefixReuseEligible        bool                `json:"prefix_reuse_eligible"`
	InflightCoalescingEligible bool                `json:"inflight_coalescing_eligible"`
	VerificationStrategy       string              `json:"verification_strategy"`
	LatencyClass               string              `json:"latency_class"`
	PrivacyClass               string              `json:"privacy_class"`
	Parallelism                WorkloadParallelism `json:"parallelism"`
	Confidence                 float64             `json:"confidence"`
	Evidence                   []string            `json:"evidence"`
	Unknowns                   []string            `json:"unknowns,omitempty"`
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
	objective, err := canonicalWorkloadObjective(sub.Objective)
	if err != nil {
		return WorkloadBinding{}, err
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
		Objective:     objective,
		InputSHA256:   inputSHA256,
	}, nil
}

// canonicalWorkloadObjective accepts the empty (omitted) value and the three
// directive names. Anything else is unknown.
func canonicalWorkloadObjective(value string) (string, error) {
	if value == "" {
		return "", nil
	}
	if !validWorkloadObjectives[value] {
		return "", fmt.Errorf("invalid objective: %s", value)
	}
	return value, nil
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

// runtimeCapabilityForBindingDirected resolves the cell that will execute a
// workload, optionally forced to a named one.
//
// Ordinary admission freezes the eligible runtime candidate set for a workload.
//
// Historical behaviour (pre multi-family): when several advertised cells served
// the same (job type, model), rankAndFreezeAdmissionCell ranked them with the
// shadow lifecycle ladder and froze exactly one winner
// (src/control/workload_classification.go:rankAndFreezeAdmissionCell). Freezing N
// without a quality contract would have handed selection to the claim query's
// ORDER BY cell_id LIMIT 1, which decides alphabetically rather than by
// governed ranking. That rank-and-freeze-1 is the named Metal-only singleton
// mechanism: with only candle_metal cells advertised it always freezes one
// Metal cell, and CUDA DRAFT cells never enter the pool.
//
// Multi-family behaviour (G024): when two or more *device families* (e.g.
// metal and cuda) are advertised for the same (job, model) AND an ACTIVE
// AcceptableQualityContract marks them multi-family substitutable, admission
// freezes the full contract-covered set. RuntimeCandidates[0] is still the
// lifecycle-ladder preferred cell (pricing primary). Claim already matches any
// frozen candidate via jsonb_array_elements(runtime_candidates). Placement v4
// opens the HW union and drops the single-device identity pin so a second
// family can actually claim.
//
// Same-family competition (two Metal cells, no multi-family contract covering
// both) still rank-and-freezes to one — that path is not a Metal-vs-CUDA choice.
//
// Artifact format is not a match key. A buyer names a model; each cell decides
// which of that model's artifacts it loads. The frozen candidate carries the
// cell's own ModelKind.
//
// With a directed cell an operator or a test names the cell explicitly, and the
// search widens to cells reachable by directed routing — a superset that
// includes cells being proven. The directed name never comes from the buyer
// wire; see buildWorkloadDecisionDirected.
func runtimeCapabilityForBindingDirected(
	binding WorkloadBinding, directedCellID string,
) (generatedRuntimeCapability, error) {
	caps, err := runtimeCapabilitiesForBindingDirected(binding, directedCellID)
	if err != nil {
		return generatedRuntimeCapability{}, err
	}
	return caps[0], nil
}

// runtimeCapabilitiesForBindingDirected is the multi-candidate admission entry.
// Directed freezes exactly the named cell. Ordinary freezes one cell, or a
// multi-family set under an AcceptableQualityContract.
func runtimeCapabilitiesForBindingDirected(
	binding WorkloadBinding, directedCellID string,
) ([]generatedRuntimeCapability, error) {
	pool := advertisedRuntimeCapabilities()
	if directedCellID != "" {
		pool = directedRuntimeCapabilities()
	}
	var matches []generatedRuntimeCapability
	for _, candidate := range pool {
		if directedCellID != "" && candidate.ID != directedCellID {
			continue
		}
		// The cell still has to fit the workload. Naming a cell is a choice of
		// runtime, never a licence to run a model on a cell that does not serve it.
		if candidate.Job != binding.JobType.Type || candidate.Model != binding.Model.Ref {
			continue
		}
		matches = append(matches, candidate)
	}
	if directedCellID != "" {
		if len(matches) != 1 {
			return nil, fmt.Errorf(
				"directed runtime cell %q does not serve job_type=%q model=%q kind=%q "+
					"(matched %d reachable cells)",
				directedCellID, binding.JobType.Type, binding.Model.Ref,
				binding.Model.Kind, len(matches))
		}
		return matches, nil
	}
	if len(matches) == 0 {
		return nil, fmt.Errorf(
			"workload classification requires one runtime cell for job_type=%q model=%q; found 0",
			binding.JobType.Type, binding.Model.Ref)
	}
	if len(matches) == 1 {
		return matches, nil
	}
	return selectAdmissionCandidates(matches)
}

// selectAdmissionCandidates is the load-bearing multi-family gate.
//
// When matches span more than one device family and an ACTIVE multi-family
// AcceptableQualityContract covers them, freeze the full covered set with the
// lifecycle-ladder winner first. Otherwise fall back to rank-and-freeze-1 so
// same-family competition does not become alphabetical claim selection.
func selectAdmissionCandidates(matches []generatedRuntimeCapability) ([]generatedRuntimeCapability, error) {
	if len(matches) == 0 {
		return nil, fmt.Errorf("selectAdmissionCandidates: empty match set")
	}
	if len(matches) == 1 {
		return matches, nil
	}
	devices := devicesAmongCapabilities(matches)
	cellIDs := make([]string, 0, len(matches))
	byID := make(map[string]generatedRuntimeCapability, len(matches))
	for _, m := range matches {
		cellIDs = append(cellIDs, m.ID)
		byID[m.ID] = m
	}
	contract, covered := activeQualityContractFor(matches[0].Job, matches[0].Model, cellIDs)
	if len(devices) > 1 && covered && contract.MultiFamilySubstitutable {
		// Multi-family under a quality contract: freeze every covered cell.
		// Preferred (lifecycle winner) is index 0 for src/pricing/placement primary.
		winner, err := rankAndFreezeAdmissionCell(matches)
		if err != nil {
			return nil, err
		}
		out := []generatedRuntimeCapability{winner}
		var rest []string
		for _, id := range cellIDs {
			if id == winner.ID {
				continue
			}
			// Only cells the contract names stay in the eligible set.
			eligible := false
			for _, e := range contract.EligibleCellIDs {
				if e == id {
					eligible = true
					break
				}
			}
			if eligible {
				rest = append(rest, id)
			}
		}
		sort.Strings(rest)
		for _, id := range rest {
			out = append(out, byID[id])
		}
		return out, nil
	}
	// Single family, or no multi-family quality contract covering the set:
	// rank-and-freeze exactly one so claim does not become alphabetical.
	winner, err := rankAndFreezeAdmissionCell(matches)
	if err != nil {
		return nil, err
	}
	return []generatedRuntimeCapability{winner}, nil
}

// rankAndFreezeAdmissionCell promotes chooseShadowCell onto the freeze path for
// the *preferred* cell. Used as the sole freeze under single-family admission,
// and as the pricing primary under multi-family admission.
func rankAndFreezeAdmissionCell(matches []generatedRuntimeCapability) (generatedRuntimeCapability, error) {
	byID := make(map[string]generatedRuntimeCapability, len(matches))
	considered := make([]shadowCandidate, 0, len(matches))
	activation := currentActivation()
	for _, capability := range matches {
		byID[capability.ID] = capability
		lifecycle := ""
		quality := ""
		for _, profile := range activation.profiles() {
			for _, cell := range profile.Cells {
				if cell.ID != capability.ID {
					continue
				}
				lifecycle = cell.EffectiveLifecycle(profile)
				quality = cell.qualityTierFor(profile)
			}
		}
		considered = append(considered, shadowCandidate{
			CellID: capability.ID, RuntimeID: capability.Runtime, Engine: capability.Engine,
			ModelKind: capability.ModelKind, Lifecycle: lifecycle, QualityTier: quality,
			Routable: true, Verification: capability.Verification,
		})
	}
	sort.Slice(considered, func(i, j int) bool {
		return considered[i].CellID < considered[j].CellID
	})
	// No preferred routed cell at admission: the ranking itself is the decision.
	// Ties keep the first winner in sorted cell-id order so reconstruction is
	// deterministic for ValidateWorkloadDecisionSnapshot.
	winnerID := chooseShadowCell(considered, "")
	if winnerID == "" {
		return generatedRuntimeCapability{}, fmt.Errorf(
			"workload classification ranked %d cells for job_type=%q model=%q but none survived the ladder",
			len(matches), matches[0].Job, matches[0].Model)
	}
	winner, ok := byID[winnerID]
	if !ok {
		return generatedRuntimeCapability{}, fmt.Errorf(
			"workload classification ranked unknown cell %q", winnerID)
	}
	return winner, nil
}

func modelRevisionFor(modelID string) string {
	for _, model := range runtimeAuthority.Models {
		if model.ID == modelID {
			return model.HFRevision
		}
	}
	return ""
}

// verificationStrategyFor projects the legacy composite strategy string from
// cell method + buyer composition knobs. It is not the acceptance-time
// verification authority — VerificationContract is. Callers that need to know
// whether a check ran must also read verification_selected / class.
func verificationStrategyFor(binding WorkloadBinding, capability generatedRuntimeCapability) string {
	// Built through the contract projector so strategy and contract cannot drift.
	return projectLegacyVerificationStrategy(VerificationContract{
		CellVerification: capability.Verification,
		RedundancyFrac:   binding.Verification.RedundancyFrac,
		HoneypotFrac:     binding.Verification.HoneypotFrac,
	})
}

func buildWorkloadDecisionFromBinding(binding WorkloadBinding) (WorkloadDecision, error) {
	return buildWorkloadDecisionFromBindingDirected(binding, "")
}

func buildWorkloadDecisionFromBindingDirected(
	binding WorkloadBinding, directedCellID string,
) (WorkloadDecision, error) {
	digest, err := workloadBindingDigest(binding)
	if err != nil {
		return WorkloadDecision{}, err
	}
	capabilities, err := runtimeCapabilitiesForBindingDirected(binding, directedCellID)
	if err != nil {
		return WorkloadDecision{}, err
	}
	capability := capabilities[0]

	workloadClass := ""
	switch {
	case capability.Runner == "embed" && capability.Job == "embed":
		workloadClass = "embeddings"
	case capability.Runner == "batch_infer" && capability.Job == "batch_infer":
		workloadClass = "batch_generation"
	case capability.Runner == "media_transcode" && capability.Job == "media_transcode":
		workloadClass = "media_transcode"
	case capability.Runner == "media_rendering" && capability.Job == "media_rendering":
		workloadClass = "media_rendering"
	default:
		return WorkloadDecision{}, fmt.Errorf(
			"runtime cell %q has no admitted workload classifier for runner=%q job=%q",
			capability.ID, capability.Runner, capability.Job)
	}

	minMemory := capability.MinMemoryGB
	for _, c := range capabilities {
		if c.MinMemoryGB > minMemory {
			minMemory = c.MinMemoryGB
		}
	}
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

	candidates := make([]WorkloadRuntimeCandidate, 0, len(capabilities))
	cellIDs := make([]string, 0, len(capabilities))
	for _, c := range capabilities {
		candidates = append(candidates, WorkloadRuntimeCandidate{
			CellID: c.ID, RuntimeID: c.Runtime, Engine: c.Engine,
			ModelKind:       c.ModelKind,
			Device:          c.Device,
			HardwareClasses: append([]string(nil), c.HardwareClasses...),
		})
		cellIDs = append(cellIDs, c.ID)
	}

	qualityContractID := ""
	evidence := []string{
		"runtime cell resolved from the embedded, digest-addressed authority matrix",
		"model revision and runtime job type are server-derived",
		"request parameters, placement constraints, verification policy and input commitment are digest-bound",
	}
	if directedCellID == "" && len(candidates) > 1 {
		if contract, ok := activeQualityContractFor(capability.Job, binding.Model.Ref, cellIDs); ok {
			qualityContractID = contract.ID
			evidence = append(evidence,
				"multi-family eligible set frozen under acceptable quality contract "+contract.ID,
				"RuntimeCandidates[0] is the lifecycle-ladder preferred cell; claim may select any eligible cell")
		}
	}

	return WorkloadDecision{
		Version:           workloadDecisionVersion,
		BindingSHA256:     digest,
		Binding:           binding,
		WorkloadClass:     workloadClass,
		RuntimeJobType:    capability.Job,
		ModelRevision:     modelRevisionFor(binding.Model.Ref),
		DirectedCellID:    directedCellID,
		QualityContractID: qualityContractID,
		RuntimeCandidates: candidates,
		MinimumMemoryGB:   minMemory,
		Deterministic:     true,
		// Batch exact-result reuse is intentionally disabled until its price
		// authority can meter the full reused workload. The prior path stored
		// merged record counts as cache "output_tokens" and then billed those
		// counts as token units, which is not valid for either embedding input
		// geometry or generative output. A future batch_infer-only path may use
		// its canonical completion-token meter; embedding remains excluded.
		// Realtime reuse has a separate, completion-token-metered path.
		ExactResultCacheEligible:   false,
		PrefixReuseEligible:        prefixEligible,
		InflightCoalescingEligible: false,
		VerificationStrategy:       verificationStrategyFor(binding, capability),
		LatencyClass:               latencyClass,
		PrivacyClass:               privacyClass,
		Parallelism: WorkloadParallelism{
			Mode: "independent_task_fanout", TensorParallelDegree: 1,
		},
		Confidence: 0.9,
		Evidence:   evidence,
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

// buildWorkloadDecisionForSubmit is the production admission freeze path.
// Ordinary buyer traffic leaves directedCellID empty. The admin directed route
// sets it so a named non-routable cell can be driven through the money path.
func buildWorkloadDecisionForSubmit(sub jobSubmit, inputSHA256 string) (WorkloadDecision, error) {
	if sub.directedCellID != "" {
		return buildWorkloadDecisionDirected(sub, inputSHA256, sub.directedCellID)
	}
	return buildWorkloadDecision(sub, inputSHA256)
}

// buildWorkloadDecisionDirected freezes a decision onto a named runtime cell.
//
// The directed cell is a SERVER-side argument and is never read from jobSubmit,
// so a buyer cannot name a cell by crafting a request. It is frozen into the
// decision rather than applied and forgotten, so a receipt can say which cell was
// chosen and by what authority, and so ValidateWorkloadDecisionSnapshot can
// reconstruct the same decision from the stored row.
func buildWorkloadDecisionDirected(
	sub jobSubmit, inputSHA256, directedCellID string,
) (WorkloadDecision, error) {
	binding, err := canonicalWorkloadBinding(sub, inputSHA256)
	if err != nil {
		return WorkloadDecision{}, err
	}
	return buildWorkloadDecisionFromBindingDirected(binding, directedCellID)
}

func ValidateWorkloadDecisionSnapshot(decision WorkloadDecision) error {
	if err := ValidateFrozenWorkloadDecisionSnapshot(decision); err != nil {
		return err
	}
	if decision.Version != workloadDecisionVersion {
		return fmt.Errorf("unsupported workload decision version %d", decision.Version)
	}
	// Reconstructed WITH the stored directed cell: a decision frozen onto a
	// directed cell must validate as itself, not as the decision ordinary
	// admission would have produced.
	expected, err := buildWorkloadDecisionFromBindingDirected(
		decision.Binding, decision.DirectedCellID)
	if err != nil {
		return err
	}
	// A decision frozen before runtime candidates carried model_kind keeps its
	// bytes and therefore its digest. Reconstructing it today would add the
	// field and fail a decision that was correct when it was written, so the
	// reconstruction drops back to the stored shape for those rows.
	//
	// Scoped to absence, not to disagreement: a stored kind that DIFFERS from the
	// authority's is tampering and still fails. This only forgives a field that
	// did not exist yet.
	for i := range expected.RuntimeCandidates {
		if i < len(decision.RuntimeCandidates) &&
			decision.RuntimeCandidates[i].ModelKind == "" {
			expected.RuntimeCandidates[i].ModelKind = ""
		}
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
	if _, err := canonicalWorkloadObjective(decision.Binding.Objective); err != nil {
		return err
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

// parseWorkloadJSONLRecord mirrors the two admitted agent runners. It stops
// malformed or shape-less records before storage, scheduling and billing,
// rather than letting a worker discover them after a contract exists.
//
// Body selection is centralized in selectJSONLBody so validation, quote depth
// scanning and streaming submit cannot diverge from the Rust runner rule
// (text present and non-null wins; otherwise prompt; empty winning text fails).
// The selected body is returned so streaming submit can validate and measure
// each record with one JSON decode.
func parseWorkloadJSONLRecord(jobType string, line []byte, lineNumber int) (string, error) {
	object, err := decodeStrictRawJSONObject(line)
	if err != nil {
		return "", &workloadInputError{Line: lineNumber, Msg: err.Error()}
	}
	if raw, exists := object["id"]; exists && !bytes.Equal(bytes.TrimSpace(raw), []byte("null")) {
		var id string
		if err := json.Unmarshal(raw, &id); err != nil {
			return "", &workloadInputError{Line: lineNumber, Msg: "id must be a string when present"}
		}
	}

	body, err := selectJSONLBody(object)
	if err != nil {
		return "", &workloadInputError{
			Line: lineNumber,
			Msg:  fmt.Sprintf("%s %s", jobType, err.Error()),
		}
	}
	return body, nil
}

func validateWorkloadJSONLRecord(jobType string, line []byte, lineNumber int) error {
	_, err := parseWorkloadJSONLRecord(jobType, line, lineNumber)
	return err
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
