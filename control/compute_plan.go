package main

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"math"
)

// computePlanVersion is the version written for newly frozen plans.
// Historical version-1 plans remain readable and settleable.
const computePlanVersion = 2

const computePlanVersionV1 = 1

const (
	computeExecutionDistributed = "distributed"
	computeExecutionExactReuse  = "exact_result_reuse"
)

// ComputePlan is the immutable execution geometry for one accepted workload.
// It closes the authority gap where quote and submit independently chose split
// size, task fan-out, memory and ETA from different input samples or fleet
// snapshots. Dynamic supply may affect when a task is claimed, but never the
// purchased geometry recorded here.
type ComputePlan struct {
	Version                 int                `json:"version"`
	ExecutionMode           string             `json:"execution_mode"`
	WorkloadBindingSHA256   string             `json:"workload_binding_sha256"`
	WorkloadDecisionSHA256  string             `json:"workload_decision_sha256"`
	OriginComputePlanSHA256 string             `json:"origin_compute_plan_sha256,omitempty"`
	InputRecords            int                `json:"input_records"`
	InputBytes              int64              `json:"input_bytes"`
	EstimatedInputTokens    int64              `json:"estimated_input_tokens"`
	EstimatedOutputTokens   int64              `json:"estimated_output_tokens"`
	InputDepthProfile       *InputDepthProfile `json:"input_depth_profile,omitempty"`
	SplitSize               int                `json:"split_size"`
	PrimaryTasks            int                `json:"primary_tasks"`
	RedundancyTasks         int                `json:"redundancy_tasks"`
	HoneypotTasks           int                `json:"honeypot_tasks"`
	TotalInitialTasks       int                `json:"total_initial_tasks"`
	MinimumMemoryGB         float64            `json:"minimum_memory_gb"`
	ETAP50Secs              int                `json:"eta_p50_secs"`
	ETAP90Secs              int                `json:"eta_p90_secs"`
	ETAWorstCaseSecs        int                `json:"eta_worst_case_secs"`
	ETASource               string             `json:"eta_source"`
	BaseComputeUSD          float64            `json:"base_compute_usd"`
	VerificationOverheadUSD float64            `json:"verification_overhead_usd"`
	Confidence              float64            `json:"confidence"`
	ConfidenceReasons       []string           `json:"confidence_reasons"`
	Unknowns                []string           `json:"unknowns,omitempty"`
}

func supportedComputePlanVersion(version int) bool {
	return version == computePlanVersionV1 || version == computePlanVersion
}

func computePlanDigest(plan ComputePlan) (string, error) {
	if !supportedComputePlanVersion(plan.Version) {
		return "", fmt.Errorf("unsupported compute plan version %d", plan.Version)
	}
	blob, err := json.Marshal(plan)
	if err != nil {
		return "", fmt.Errorf("marshal compute plan: %w", err)
	}
	sum := sha256.Sum256(blob)
	return hex.EncodeToString(sum[:]), nil
}

// estimatedInputTokensForComputePlanV1 is the historical whole-input bytes/4 rule
// used by version-1 plans. Version-2 plans derive tokens from InputDepthProfile.
func estimatedInputTokensForComputePlanV1(records int, inputBytes int64) int64 {
	if records <= 0 || inputBytes <= 0 {
		return 0
	}
	byBytes := int64(math.Ceil(float64(inputBytes) / bytesPerTokenHeuristic))
	if byBytes < int64(records) {
		return int64(records)
	}
	return byBytes
}

func estimatedOutputTokensForComputePlan(decision WorkloadDecision, records int) int64 {
	if records <= 0 || !generativeJobType(decision.Binding.JobType.Type) {
		return 0
	}
	perRecord := decision.Binding.JobType.MaxTokens
	if perRecord == 0 {
		perRecord = defaultQuoteMaxTokens
	}
	return int64(records) * int64(perRecord)
}

// computePlanETASource names the strongest authority behind a quoted ETA.
// "calibrated" outranks the rest: it means realized-versus-predicted history for
// this job type and tier measurably corrected the number, which is a claim about
// the estimate's accuracy that the weaker sources cannot make.
func computePlanETASource(plannerBacked, observedHistory, calibrated bool) string {
	switch {
	case calibrated:
		return "calibrated"
	case observedHistory:
		return "historical"
	case plannerBacked:
		return "planner"
	default:
		return "static"
	}
}

// selectSubmissionSplitSize keeps the complete quote plan authoritative. The
// callback form is intentional: a bound submit must not even consult the live
// fleet planner, because doing so would reintroduce a second source of truth.
func selectSubmissionSplitSize(bound *boundQuote, unbound func() int) (int, error) {
	if bound != nil {
		if bound.ComputePlan.ExecutionMode != computeExecutionDistributed ||
			bound.ComputePlan.SplitSize <= 0 {
			return 0, errors.New("bound quote has no distributed split authority")
		}
		return bound.ComputePlan.SplitSize, nil
	}
	splitSize := unbound()
	if splitSize <= 0 {
		return 0, errors.New("unbound planner returned an invalid split size")
	}
	return splitSize, nil
}

func newDistributedComputePlan(
	decision WorkloadDecision,
	inputRecords int,
	inputBytes int64,
	depth InputDepthProfile,
	splitSize int,
	primaryTasks int,
	redundancyTasks int,
	honeypotTasks int,
	eta QuoteTime,
	etaSource string,
	baseComputeUSD float64,
	verificationOverheadUSD float64,
	confidence QuoteConfidence,
	unknowns []string,
) (ComputePlan, error) {
	decisionSHA256, err := workloadDecisionDigest(decision)
	if err != nil {
		return ComputePlan{}, err
	}
	if err := validateInputDepthProfile(depth); err != nil {
		return ComputePlan{}, fmt.Errorf("invalid input depth profile: %w", err)
	}
	classified, ok := checkedInputDepthRecordCount(
		depth.ShortRecords, depth.MediumRecords, depth.LongRecords,
	)
	if !ok || classified != inputRecords {
		return ComputePlan{}, fmt.Errorf(
			"input depth profile record counts %d do not match input_records %d",
			classified, inputRecords)
	}
	depthCopy := depth
	plan := ComputePlan{
		Version:                 computePlanVersion,
		ExecutionMode:           computeExecutionDistributed,
		WorkloadBindingSHA256:   decision.BindingSHA256,
		WorkloadDecisionSHA256:  decisionSHA256,
		InputRecords:            inputRecords,
		InputBytes:              inputBytes,
		EstimatedInputTokens:    depth.EstimatedTokens,
		EstimatedOutputTokens:   estimatedOutputTokensForComputePlan(decision, inputRecords),
		InputDepthProfile:       &depthCopy,
		SplitSize:               splitSize,
		PrimaryTasks:            primaryTasks,
		RedundancyTasks:         redundancyTasks,
		HoneypotTasks:           honeypotTasks,
		TotalInitialTasks:       primaryTasks + redundancyTasks + honeypotTasks,
		MinimumMemoryGB:         decision.MinimumMemoryGB,
		ETAP50Secs:              eta.P50Secs,
		ETAP90Secs:              eta.P90Secs,
		ETAWorstCaseSecs:        eta.WorstCaseSecs,
		ETASource:               etaSource,
		BaseComputeUSD:          roundEconomicUSD(baseComputeUSD),
		VerificationOverheadUSD: roundEconomicUSD(verificationOverheadUSD),
		Confidence:              confidence.Score,
		ConfidenceReasons:       append([]string(nil), confidence.Reasons...),
		Unknowns:                append([]string(nil), unknowns...),
	}
	if err := ValidateFrozenComputePlanSnapshot(plan, decision); err != nil {
		return ComputePlan{}, err
	}
	return plan, nil
}

func newExactReuseComputePlan(
	decision WorkloadDecision,
	inputRecords int,
	inputBytes int64,
	depth InputDepthProfile,
	buyerChargeUSD float64,
	origin *ComputePlan,
) (ComputePlan, error) {
	decisionSHA256, err := workloadDecisionDigest(decision)
	if err != nil {
		return ComputePlan{}, err
	}
	if err := validateInputDepthProfile(depth); err != nil {
		return ComputePlan{}, fmt.Errorf("invalid input depth profile: %w", err)
	}
	classified, ok := checkedInputDepthRecordCount(
		depth.ShortRecords, depth.MediumRecords, depth.LongRecords,
	)
	if !ok || classified != inputRecords {
		return ComputePlan{}, fmt.Errorf(
			"input depth profile record counts %d do not match input_records %d",
			classified, inputRecords)
	}
	depthCopy := depth
	plan := ComputePlan{
		Version:                computePlanVersion,
		ExecutionMode:          computeExecutionExactReuse,
		WorkloadBindingSHA256:  decision.BindingSHA256,
		WorkloadDecisionSHA256: decisionSHA256,
		InputRecords:           inputRecords,
		InputBytes:             inputBytes,
		EstimatedInputTokens:   depth.EstimatedTokens,
		EstimatedOutputTokens:  estimatedOutputTokensForComputePlan(decision, inputRecords),
		InputDepthProfile:      &depthCopy,
		ETASource:              computeExecutionExactReuse,
		BaseComputeUSD:         roundEconomicUSD(buyerChargeUSD),
		MinimumMemoryGB:        decision.MinimumMemoryGB,
		Confidence:             1,
		ConfidenceReasons: []string{
			"content-addressed result identity matched the complete frozen workload decision",
			"physical execution fan-out is zero because a verified prior result is materialized",
		},
		Unknowns: []string{
			"the original physical execution timing is not attributed to this reuse delivery",
		},
	}
	if origin != nil {
		plan.OriginComputePlanSHA256, err = computePlanDigest(*origin)
		if err != nil {
			return ComputePlan{}, fmt.Errorf("hash origin compute plan: %w", err)
		}
	}
	if err := ValidateFrozenComputePlanSnapshot(plan, decision); err != nil {
		return ComputePlan{}, err
	}
	return plan, nil
}

func validSHA256(value string) bool {
	if len(value) != sha256.Size*2 {
		return false
	}
	_, err := hex.DecodeString(value)
	return err == nil
}

// ValidateFrozenComputePlanSnapshot validates only authority embedded in the
// plan and its frozen workload decision. It deliberately does not consult the
// current fleet, model catalogue or runtime matrix.
//
// Version 1 keeps the historical whole-input bytes/4 token rule and requires no
// depth profile. Version 2 requires a self-consistent InputDepthProfile and
// binds EstimatedInputTokens to that profile.
func ValidateFrozenComputePlanSnapshot(plan ComputePlan, decision WorkloadDecision) error {
	if !supportedComputePlanVersion(plan.Version) {
		return fmt.Errorf("unsupported compute plan version %d", plan.Version)
	}
	if err := ValidateFrozenWorkloadDecisionSnapshot(decision); err != nil {
		return fmt.Errorf("invalid workload decision for compute plan: %w", err)
	}
	decisionSHA256, err := workloadDecisionDigest(decision)
	if err != nil {
		return err
	}
	if plan.WorkloadBindingSHA256 != decision.BindingSHA256 ||
		plan.WorkloadDecisionSHA256 != decisionSHA256 {
		return errors.New("compute plan does not match its frozen workload authority")
	}
	if plan.InputRecords <= 0 || plan.InputBytes <= 0 {
		return errors.New("compute plan requires positive input records and bytes")
	}
	switch plan.Version {
	case computePlanVersionV1:
		if plan.InputDepthProfile != nil {
			return errors.New("version-1 compute plan cannot carry an input depth profile")
		}
		if plan.EstimatedInputTokens != estimatedInputTokensForComputePlanV1(plan.InputRecords, plan.InputBytes) {
			return errors.New("compute plan input-token estimate does not match its frozen input geometry")
		}
	case computePlanVersion:
		if plan.InputDepthProfile == nil {
			return errors.New("version-2 compute plan requires an input depth profile")
		}
		if err := validateInputDepthProfile(*plan.InputDepthProfile); err != nil {
			return fmt.Errorf("compute plan input depth profile invalid: %w", err)
		}
		classified, ok := checkedInputDepthRecordCount(
			plan.InputDepthProfile.ShortRecords,
			plan.InputDepthProfile.MediumRecords,
			plan.InputDepthProfile.LongRecords,
		)
		if !ok || classified != plan.InputRecords {
			return fmt.Errorf("compute plan input_records=%d does not match depth profile counts %d",
				plan.InputRecords, classified)
		}
		if plan.InputDepthProfile.BodyBytes > plan.InputBytes {
			return errors.New("compute plan input depth body bytes exceed its frozen input bytes")
		}
		if plan.EstimatedInputTokens != plan.InputDepthProfile.EstimatedTokens {
			return errors.New("compute plan input-token estimate does not match its frozen input depth profile")
		}
	}
	if plan.EstimatedOutputTokens != estimatedOutputTokensForComputePlan(decision, plan.InputRecords) {
		return errors.New("compute plan output-token estimate does not match its frozen workload")
	}
	for name, value := range map[string]float64{
		"minimum_memory_gb":         plan.MinimumMemoryGB,
		"base_compute_usd":          plan.BaseComputeUSD,
		"verification_overhead_usd": plan.VerificationOverheadUSD,
		"confidence":                plan.Confidence,
	} {
		if math.IsNaN(value) || math.IsInf(value, 0) {
			return fmt.Errorf("compute plan %s is not finite", name)
		}
	}
	if math.Abs(plan.MinimumMemoryGB-decision.MinimumMemoryGB) > 0.000001 {
		return errors.New("compute plan memory floor does not match workload placement authority")
	}
	if plan.BaseComputeUSD <= 0 || plan.VerificationOverheadUSD < 0 {
		return errors.New("compute plan has invalid compute cost estimates")
	}
	if plan.Confidence < 0 || plan.Confidence > 1 || len(plan.ConfidenceReasons) == 0 {
		return errors.New("compute plan has invalid or unexplained confidence")
	}

	switch plan.ExecutionMode {
	case computeExecutionDistributed:
		if plan.OriginComputePlanSHA256 != "" {
			return errors.New("distributed compute plan cannot carry an origin plan")
		}
		if plan.SplitSize <= 0 || plan.PrimaryTasks <= 0 ||
			plan.RedundancyTasks < 0 || plan.HoneypotTasks < 0 {
			return errors.New("distributed compute plan has invalid task geometry")
		}
		expectedPrimary := (plan.InputRecords + plan.SplitSize - 1) / plan.SplitSize
		if plan.PrimaryTasks != expectedPrimary {
			return fmt.Errorf("compute plan primary_tasks=%d does not match input/split geometry %d",
				plan.PrimaryTasks, expectedPrimary)
		}
		expectedTotal := plan.PrimaryTasks + plan.RedundancyTasks + plan.HoneypotTasks
		if plan.TotalInitialTasks != expectedTotal {
			return fmt.Errorf("compute plan total_initial_tasks=%d does not match task classes %d",
				plan.TotalInitialTasks, expectedTotal)
		}
		if plan.ETAP50Secs < 0 || plan.ETAP90Secs < plan.ETAP50Secs ||
			plan.ETAWorstCaseSecs < plan.ETAP90Secs {
			return errors.New("compute plan has invalid ETA bands")
		}
		switch plan.ETASource {
		case "planner", "historical", "static", "calibrated":
		default:
			return errors.New("compute plan has invalid ETA source")
		}
	case computeExecutionExactReuse:
		if plan.SplitSize != 0 || plan.PrimaryTasks != 0 || plan.RedundancyTasks != 0 ||
			plan.HoneypotTasks != 0 || plan.TotalInitialTasks != 0 {
			return errors.New("exact-reuse compute plan cannot carry physical task geometry")
		}
		if plan.ETAP50Secs != 0 || plan.ETAP90Secs != 0 || plan.ETAWorstCaseSecs != 0 ||
			plan.ETASource != computeExecutionExactReuse {
			return errors.New("exact-reuse compute plan must freeze zero execution ETA")
		}
		if plan.VerificationOverheadUSD != 0 {
			return errors.New("exact-reuse compute plan cannot charge physical verification overhead")
		}
		if plan.OriginComputePlanSHA256 != "" && !validSHA256(plan.OriginComputePlanSHA256) {
			return errors.New("exact-reuse compute plan has invalid origin plan digest")
		}
	default:
		return fmt.Errorf("unsupported compute execution mode %q", plan.ExecutionMode)
	}
	return nil
}

// settlementBaseFromComputeEstimate is the economic Input.BaseComputeUSD that
// BuildEconomicPlan freezes for a given unfloored compute-plan money sum.
//
// The compute plan records estimation authority (primary + verification). The
// economic plan records settlement authority: the min-billable floor may raise
// that estimate so a supplier who performs work is never reserved $0. Routing
// and estimation keep the unfloored discrimination between small jobs; only
// settlement sees the floor. Agreement is therefore:
//
//	economic.Input.BaseComputeUSD == max(computeSum, minBillable)
//
// not a requirement that the compute plan rewrite its estimate upward.
func settlementBaseFromComputeEstimate(computeSum float64, supplierShare float64, initialTasks int) float64 {
	computeSum = roundEconomicUSD(computeSum)
	if minTotal := minBillableBaseComputeMicros(supplierShare, initialTasks); usdToMicros(computeSum) < minTotal {
		return microsToUSD(minTotal)
	}
	return computeSum
}

func ValidateComputePlanEconomicSnapshot(plan ComputePlan, decision WorkloadDecision, economic EconomicPlan) error {
	if err := ValidateFrozenComputePlanSnapshot(plan, decision); err != nil {
		return err
	}
	if plan.ExecutionMode != computeExecutionDistributed {
		return errors.New("only distributed compute plans bind a distributed economic plan")
	}
	if err := ValidateEconomicPlanSnapshot(economic); err != nil {
		return err
	}
	if plan.TotalInitialTasks != economic.Input.InitialTaskCount {
		return fmt.Errorf("compute plan total_initial_tasks=%d does not match economic initial_task_count=%d",
			plan.TotalInitialTasks, economic.Input.InitialTaskCount)
	}
	// Estimation (compute) vs settlement (economic): the economic base must be
	// exactly the floored form of the compute-plan money sum. Exact equality
	// still holds when the estimate is already above the min-billable floor;
	// when the floor raises the base, the delta must be exactly the floor
	// lift — not an arbitrary excess. This is not a relaxation of authority:
	// a compute sum that cannot produce the frozen economic base (or an
	// economic base that invents money beyond floor(compute)) still fails.
	frozenCompute := roundEconomicUSD(plan.BaseComputeUSD + plan.VerificationOverheadUSD)
	expectedEconomic := settlementBaseFromComputeEstimate(
		frozenCompute, economic.Input.SupplierShare, economic.Input.InitialTaskCount,
	)
	if math.Abs(expectedEconomic-economic.Input.BaseComputeUSD) > 0.000001 {
		return fmt.Errorf("compute plan base plus verification $%.6f (settlement form $%.6f) does not match economic base $%.6f",
			frozenCompute, expectedEconomic, economic.Input.BaseComputeUSD)
	}
	return nil
}
