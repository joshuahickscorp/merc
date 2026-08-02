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
// Historical version-1 through version-3 plans remain readable and settleable.
const computePlanVersion = 4

const computePlanVersionV1 = 1

const computePlanVersionV2 = 2

const computePlanVersionV3 = 3

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
	Version int `json:"version"`
	// ExecutionMode is the billing/geometry path for this plan
	// ("distributed" or "exact_result_reuse"). It is NOT the network placement
	// axis in execution_mode.go (POOL / REPLICA_SERVICE / LOCAL_CLUSTER /
	// CLOUD_BACKSTOP). Both axes serialize under the JSON key "execution_mode";
	// the value sets are required to stay disjoint
	// (TestExecutionModeValueSetsAreDisjoint). Do not rename this key here —
	// that is a storage migration.
	ExecutionMode           string `json:"execution_mode"`
	WorkloadBindingSHA256   string `json:"workload_binding_sha256"`
	WorkloadDecisionSHA256  string `json:"workload_decision_sha256"`
	OriginComputePlanSHA256 string `json:"origin_compute_plan_sha256,omitempty"`
	InputRecords            int    `json:"input_records"`
	InputBytes              int64  `json:"input_bytes"`
	EstimatedInputTokens    int64  `json:"estimated_input_tokens"`
	// SettlementInputUnits is the exact workload-unit count that the frozen
	// catalogue price used. It is deliberately separate from
	// EstimatedInputTokens: the latter is a selected-body depth estimate for
	// planning and ETA. Text/transcode use bounded input geometry; deterministic
	// rendering uses declared output pixels. Version 3 writes the applicable
	// fractional geometry without a rounding conversion, so pricing, settlement,
	// and the receipt share one authority.
	SettlementInputUnits  float64            `json:"settlement_input_units,omitempty"`
	EstimatedOutputTokens int64              `json:"estimated_output_tokens"`
	InputDepthProfile     *InputDepthProfile `json:"input_depth_profile,omitempty"`
	SplitSize             int                `json:"split_size"`
	PrimaryTasks          int                `json:"primary_tasks"`
	RedundancyTasks       int                `json:"redundancy_tasks"`
	HoneypotTasks         int                `json:"honeypot_tasks"`
	TotalInitialTasks     int                `json:"total_initial_tasks"`
	MinimumMemoryGB       float64            `json:"minimum_memory_gb"`
	ETAP50Secs            int                `json:"eta_p50_secs"`
	ETAP90Secs            int                `json:"eta_p90_secs"`
	ETAWorstCaseSecs      int                `json:"eta_worst_case_secs"`
	ETASource             string             `json:"eta_source"`
	// ETAConfidenceBandMethod makes the semantic source of eta_p90_secs
	// explicit. It is deliberately separate from ETASource: the latter explains
	// the p50 authority, while this describes how the uncertainty bands were
	// constructed. Version 4 writes it; its omission on older plans preserves
	// their historical JSON and digest.
	ETAConfidenceBandMethod string  `json:"eta_confidence_band_method,omitempty"`
	BaseComputeUSD          float64 `json:"base_compute_usd"`
	VerificationOverheadUSD float64 `json:"verification_overhead_usd"`
	// VerificationClass is the governed class every primary task of this job is
	// verified under, and VerificationClassPolicy names the rule set that
	// assigned it. Empty means SAMPLED, which is what every plan meant before the
	// classes existed — so omitting them leaves historical plans byte-identical
	// and their digests unchanged, which is why this did not need a plan version.
	//
	// Bound into the plan because the class is priced: a REQUIRED job buys
	// verification for every task, and a plan that did not record which class it
	// was frozen under could not be settled against the work that was done.
	VerificationClass       string   `json:"verification_class,omitempty"`
	VerificationClassPolicy string   `json:"verification_class_policy,omitempty"`
	Confidence              float64  `json:"confidence"`
	ConfidenceReasons       []string `json:"confidence_reasons"`
	Unknowns                []string `json:"unknowns,omitempty"`
}

func supportedComputePlanVersion(version int) bool {
	return version == computePlanVersionV1 || version == computePlanVersionV2 ||
		version == computePlanVersionV3 || version == computePlanVersion
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

// settlementInputUnitsForGeometry is the input-side unit formula already used
// by estimateJobSettlementWithAuthority. Keep it in one place: a fractional
// byte-derived unit is real pricing authority, not a token-estimator rounding
// detail. New compute plans freeze this exact result in SettlementInputUnits.
func settlementInputUnitsForGeometry(records int, inputBytes int64) float64 {
	if records <= 0 || inputBytes <= 0 {
		return 0
	}
	units := float64(records)
	if byteUnits := float64(inputBytes) / bytesPerTokenHeuristic; byteUnits > units {
		units = byteUnits
	}
	return units
}

// settlementInputUnitsForJobType is the workload-specific extension of the
// historical byte geometry. Text and media-transcode jobs are still priced from
// their bounded input geometry. A deterministic renderer, however, is measured
// and executed in output pixels; charging only the JSON scene bytes would make
// the frozen money authority unrelated to the physical work being sold.
func settlementInputUnitsForJobType(jobType JobType, records int, inputBytes int64) float64 {
	if jobType.Type == "media_rendering" {
		if records <= 0 || jobType.RenderWidth == 0 || jobType.RenderHeight == 0 {
			return 0
		}
		pixels := uint64(jobType.RenderWidth) * uint64(jobType.RenderHeight)
		return float64(records) * float64(pixels)
	}
	return settlementInputUnitsForGeometry(records, inputBytes)
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

const (
	// etaBandMethodPlannerConservativeBound means eta_p90_secs contains the
	// planner's conservative rate-model bound. It is not represented as an
	// empirically measured statistical percentile.
	etaBandMethodPlannerConservativeBound = "planner_conservative_bound"
	// etaBandMethodSyntheticMultiples means eta_p90_secs and worst_case_secs
	// are deterministic multiples of p50, rather than observed percentiles.
	etaBandMethodSyntheticMultiples = "synthetic_multiples"
	etaBandMethodExactResultReuse   = "exact_result_reuse"
)

func saturatingETAMultiple(secs, multiple int) int {
	if secs <= 0 || multiple <= 0 {
		return 0
	}
	maxInt := int(^uint(0) >> 1)
	if secs > maxInt/multiple {
		return maxInt
	}
	return secs * multiple
}

func maxETA(a, b int) int {
	if a > b {
		return a
	}
	return b
}

// quoteTimeFromETABands freezes buyer-visible ETA bands without calling a
// modeled conservative rate bound a measured percentile. Planner-backed
// estimates surface that bound in the compatibility p90 field and label its
// method. Fallback estimates retain their advisory synthetic bands with the
// same explicit label.
func quoteTimeFromETABands(p50Secs, conservativeSecs int, plannerBacked bool) QuoteTime {
	if p50Secs < 0 {
		p50Secs = 0
	}
	if plannerBacked {
		if conservativeSecs < p50Secs {
			conservativeSecs = p50Secs
		}
		return QuoteTime{
			P50Secs:              p50Secs,
			P90Secs:              conservativeSecs,
			WorstCaseSecs:        maxETA(saturatingETAMultiple(p50Secs, 4), saturatingETAMultiple(conservativeSecs, 2)),
			ConfidenceBandMethod: etaBandMethodPlannerConservativeBound,
		}
	}
	return QuoteTime{
		P50Secs:              p50Secs,
		P90Secs:              saturatingETAMultiple(p50Secs, 2),
		WorstCaseSecs:        saturatingETAMultiple(p50Secs, 4),
		ConfidenceBandMethod: etaBandMethodSyntheticMultiples,
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
		SettlementInputUnits:    settlementInputUnitsForJobType(decision.Binding.JobType, inputRecords, inputBytes),
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
		ETAConfidenceBandMethod: eta.ConfidenceBandMethod,
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
		Version:                 computePlanVersion,
		ExecutionMode:           computeExecutionExactReuse,
		WorkloadBindingSHA256:   decision.BindingSHA256,
		WorkloadDecisionSHA256:  decisionSHA256,
		InputRecords:            inputRecords,
		InputBytes:              inputBytes,
		EstimatedInputTokens:    depth.EstimatedTokens,
		SettlementInputUnits:    settlementInputUnitsForJobType(decision.Binding.JobType, inputRecords, inputBytes),
		EstimatedOutputTokens:   estimatedOutputTokensForComputePlan(decision, inputRecords),
		InputDepthProfile:       &depthCopy,
		ETASource:               computeExecutionExactReuse,
		ETAConfidenceBandMethod: etaBandMethodExactResultReuse,
		BaseComputeUSD:          roundEconomicUSD(buyerChargeUSD),
		MinimumMemoryGB:         decision.MinimumMemoryGB,
		Confidence:              1,
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
// Version 1 keeps the historical rounded whole-input bytes/4 token rule and
// requires no depth profile. Version 2 adds a self-consistent InputDepthProfile
// and binds EstimatedInputTokens to that profile. Version 3 additionally
// freezes the exact fractional input unit count that catalogue pricing used, so
// settlement, pricing receipts, and supplier-time modeling cannot call the
// planning depth estimate a money unit. Version 4 adds the explicit semantics
// behind the frozen ETA uncertainty bands.
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
	if err := validateComputePlanVerificationClass(plan); err != nil {
		return err
	}
	switch plan.Version {
	case computePlanVersionV1:
		if plan.InputDepthProfile != nil {
			return errors.New("version-1 compute plan cannot carry an input depth profile")
		}
		if plan.SettlementInputUnits != 0 {
			return errors.New("version-1 compute plan cannot carry settlement input units")
		}
		if plan.EstimatedInputTokens != estimatedInputTokensForComputePlanV1(plan.InputRecords, plan.InputBytes) {
			return errors.New("compute plan input-token estimate does not match its frozen input geometry")
		}
	case computePlanVersionV2, computePlanVersionV3, computePlanVersion:
		if plan.InputDepthProfile == nil {
			return fmt.Errorf("version-%d compute plan requires an input depth profile", plan.Version)
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
		if plan.Version == computePlanVersionV2 {
			if plan.SettlementInputUnits != 0 {
				return errors.New("version-2 compute plan cannot carry settlement input units")
			}
		} else {
			wantSettlementUnits := settlementInputUnitsForJobType(decision.Binding.JobType, plan.InputRecords, plan.InputBytes)
			if math.IsNaN(plan.SettlementInputUnits) || math.IsInf(plan.SettlementInputUnits, 0) ||
				plan.SettlementInputUnits <= 0 ||
				math.Abs(plan.SettlementInputUnits-wantSettlementUnits) > 0.000000001 {
				return errors.New("compute plan settlement input units do not match its frozen input geometry")
			}
		}
	}
	if plan.Version < computePlanVersion && plan.ETAConfidenceBandMethod != "" {
		return errors.New("pre-version-4 compute plan cannot carry ETA confidence-band semantics")
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
		if plan.Version == computePlanVersion {
			switch plan.ETAConfidenceBandMethod {
			case etaBandMethodPlannerConservativeBound:
				if plan.ETASource == "static" {
					return errors.New("planner conservative ETA bands cannot claim a static p50 source")
				}
				if plan.ETAWorstCaseSecs != maxETA(
					saturatingETAMultiple(plan.ETAP50Secs, 4),
					saturatingETAMultiple(plan.ETAP90Secs, 2),
				) {
					return errors.New("planner conservative ETA bands do not match their frozen formula")
				}
			case etaBandMethodSyntheticMultiples:
				if plan.ETASource == "planner" {
					return errors.New("planner-backed p50 cannot carry synthetic ETA bands")
				}
				if plan.ETAP90Secs != saturatingETAMultiple(plan.ETAP50Secs, 2) ||
					plan.ETAWorstCaseSecs != saturatingETAMultiple(plan.ETAP50Secs, 4) {
					return errors.New("synthetic ETA bands do not match their frozen formula")
				}
			default:
				return errors.New("compute plan has invalid ETA confidence-band method")
			}
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
		if plan.Version == computePlanVersion && plan.ETAConfidenceBandMethod != etaBandMethodExactResultReuse {
			return errors.New("exact-reuse compute plan must identify its zero ETA semantics")
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

// validateComputePlanVerificationClass checks the governed class a plan carries.
//
// A plan recording "REQUIRED" with no policy revision says a decision was taken
// and refuses to say under which rules, which is the half that matters when the
// decision is re-read years later. An empty class is the historical default and
// means SAMPLED, so old plans keep validating unchanged.
func validateComputePlanVerificationClass(plan ComputePlan) error {
	if plan.VerificationClass == "" {
		if plan.VerificationClassPolicy != "" {
			return errors.New("compute plan names a verification class policy with no class")
		}
		return nil
	}
	if !knownVerificationClass(plan.VerificationClass) {
		return fmt.Errorf("compute plan names unknown verification class %q", plan.VerificationClass)
	}
	if plan.VerificationClass == VerificationClassHoneypot ||
		plan.VerificationClass == VerificationClassRedundant {
		return fmt.Errorf("compute plan names per-task verification class %q as a job-wide class",
			plan.VerificationClass)
	}
	if plan.VerificationClassPolicy == "" {
		return fmt.Errorf("compute plan declares verification class %q with no policy revision",
			plan.VerificationClass)
	}
	return nil
}
