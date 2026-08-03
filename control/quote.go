package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"net/http"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

const fieldSampleN = 512

const bytesPerTokenHeuristic = 4.0

const nonASCIITokensPerRune = 0.9

func estimateTokens(text []byte) int64 {
	asciiCount := 0
	for _, b := range text {
		if b < 128 {
			asciiCount++
		}
	}
	return estimateTokensFromCounts(utf8.RuneCount(text), asciiCount, len(text))
}

func estimateTokensFromCounts(runeCount, asciiCount, byteLen int) int64 {
	if runeCount == 0 || byteLen == 0 {
		return 0
	}
	if float64(asciiCount)/float64(byteLen) < 0.5 {
		return int64(math.Ceil(float64(runeCount) * nonASCIITokensPerRune))
	}
	return int64(math.Ceil(float64(runeCount) / bytesPerTokenHeuristic))
}

type FieldStat struct {
	Field        string  `json:"field"`
	AvgStringLen float64 `json:"avg_string_len"` // mean rune length of this field's string values in the sample
	Occurrences  int     `json:"occurrences"`    // sampled records that carried this field
}

type QuoteInputScan struct {
	Records          int         `json:"records"`          // non-blank JSONL lines
	Bytes            int         `json:"bytes"`            // total input bytes
	EstimatedTokens  int64       `json:"estimated_tokens"` // selected-body estimate; same authority as ComputePlan
	MalformedRecords int         `json:"malformed_records"`
	BlankRecords     int         `json:"blank_records"`   // blank/whitespace lines (skipped, never records)
	SkippedRecords   int         `json:"skipped_records"` // blank + malformed: lines NOT usable as input (item 23)
	FirstBadLine     int         `json:"first_bad_line"`  // 1-based line of the first malformed record; 0 = none
	MaxLineBytes     int         `json:"max_line_bytes"`
	SampledRecords   int         `json:"sampled_records"` // records inspected for field names
	DetectedFields   []string    `json:"detected_fields"` // sorted union of top-level keys in the sample
	RecommendedField string      `json:"recommended_field,omitempty"`
	FieldStats       []FieldStat `json:"field_stats,omitempty"`
	// InputDepth is the deterministic body-depth profile over every accepted
	// record and is frozen into ComputePlan.
	InputDepth InputDepthProfile `json:"input_depth,omitempty"`
}

func scanJSONL(data []byte) QuoteInputScan {
	scan := QuoteInputScan{}
	fields := map[string]bool{}
	fieldStrLen := map[string]int{}
	fieldOccur := map[string]int{}
	lineNo := 0
	depthAcc := newInputDepthAccumulator()
	for _, raw := range bytes.Split(data, []byte("\n")) {
		lineNo++
		ln := bytes.TrimRight(raw, "\r")
		if len(bytes.TrimSpace(ln)) == 0 {
			scan.BlankRecords++
			continue // blank line carries no record
		}
		scan.Records++
		scan.Bytes += len(ln)
		if len(ln) > scan.MaxLineBytes {
			scan.MaxLineBytes = len(ln)
		}
		obj, err := decodeStrictRawJSONObject(ln)
		if err != nil {
			scan.MalformedRecords++
			if scan.FirstBadLine == 0 {
				scan.FirstBadLine = lineNo
			}
			continue
		}
		if body, err := selectJSONLBody(obj); err == nil {
			depthAcc.addBody(body)
		}
		if scan.SampledRecords < fieldSampleN {
			for k, v := range obj {
				fields[k] = true
				fieldOccur[k]++
				if s, ok := jsonStringValue(v); ok {
					fieldStrLen[k] += utf8.RuneCountInString(s)
				}
			}
			scan.SampledRecords++
		}
	}
	scan.SkippedRecords = scan.BlankRecords + scan.MalformedRecords
	scan.DetectedFields = sortedKeys(fields)
	scan.FieldStats, scan.RecommendedField = recommendField(fields, fieldStrLen, fieldOccur)
	if depth, err := depthAcc.profile(); err == nil {
		scan.InputDepth = depth
		scan.EstimatedTokens = depth.EstimatedTokens
	}
	return scan
}

func jsonStringValue(raw json.RawMessage) (string, bool) {
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) == 0 || trimmed[0] != '"' {
		return "", false
	}
	var s string
	if err := json.Unmarshal(trimmed, &s); err != nil {
		return "", false
	}
	return s, true
}

func recommendField(fields map[string]bool, strLen, occur map[string]int) ([]FieldStat, string) {
	if len(fields) == 0 {
		return nil, ""
	}
	stats := make([]FieldStat, 0, len(fields))
	for f := range fields {
		n := occur[f]
		avg := 0.0
		if n > 0 {
			avg = float64(strLen[f]) / float64(n)
		}
		stats = append(stats, FieldStat{Field: f, AvgStringLen: avg, Occurrences: n})
	}
	less := func(a, b FieldStat) bool {
		if a.AvgStringLen != b.AvgStringLen {
			return a.AvgStringLen > b.AvgStringLen
		}
		return a.Field < b.Field
	}
	for i := 1; i < len(stats); i++ {
		for j := i; j > 0 && less(stats[j], stats[j-1]); j-- {
			stats[j-1], stats[j] = stats[j], stats[j-1]
		}
	}
	recommended := ""
	if stats[0].AvgStringLen > 0 {
		recommended = stats[0].Field
	}
	return stats, recommended
}

type Quote struct {
	QuoteID       string               `json:"quote_id"`
	JobType       string               `json:"job_type"`
	Model         string               `json:"model"`
	Tier          string               `json:"tier"`
	Currency      string               `json:"currency"`
	TierSemantics string               `json:"tier_semantics"`
	Workload      WorkloadDecision     `json:"workload_decision"`
	Placement     PlacementRequirement `json:"placement_requirement"`
	ComputePlan   ComputePlan          `json:"compute_plan"`
	Pricing       PricingDecision      `json:"pricing_decision"`
	Input         QuoteInputScan       `json:"input"`
	Execution     QuoteExecution       `json:"execution"`
	Cost          QuoteCost            `json:"cost"`
	Time          QuoteTime            `json:"time"`
	Confidence    QuoteConfidence      `json:"confidence"`
	Budget        QuoteBudget          `json:"budget"`
	Warnings      []string             `json:"warnings"`
	ExpiresAt     time.Time            `json:"expires_at"`   // quote stops being bindable after this (Plane D D7)
	InputSHA256   string               `json:"input_sha256"` // sha256 of the scanned input bytes (best-effort submit match)
	SLA           *QuoteSLA            `json:"sla,omitempty"`
	Economics     EconomicPlan         `json:"economics"`

	bareID        uuid.UUID // quotes.id primary key (the <uuid> inside QuoteID); not on the wire
	etaRawP50Secs int       // pre-calibration learner input; persisted separately, never a buyer promise
}

func serviceTierSemantics(tier string) string {
	switch tier {
	case "priority":
		return "bounded queue preference per eligible worker; after three consecutive ordinary priority claims, one eligible batch opportunity is served; no device reservation, wider-fanout guarantee, or SLA"
	case "trusted":
		return "restricts execution to the trusted supplier tier; it is not queue priority, device reservation, wider fan-out, or an SLA"
	default:
		return "standard queue service with a bounded batch opportunity under priority contention; no device reservation, wider-fanout guarantee, or SLA"
	}
}

const quoteTTL = 15 * time.Minute

type QuoteExecution struct {
	RecommendedSplitSize int     `json:"recommended_split_size"`
	EstimatedTasks       int     `json:"estimated_tasks"`
	EligibleWorkersNow   int     `json:"eligible_workers_now"`
	WarmEligibleWorkers  int     `json:"warm_eligible_workers"` // eligible workers that ALSO have the model warm (warm-routing, D3)
	ModelMinMemoryGB     float32 `json:"model_min_memory_gb"`   // catalogue floor; the per-task memory requirement
	OOMRisk              string  `json:"oom_risk"`              // low|medium|high
	ColdStartRisk        string  `json:"cold_start_risk"`       // low|medium|high
	SLAEligible          bool    `json:"sla_eligible"`          // supply >= threshold -> a project-SLA ETA is offerable (research §6.2 launch gate)
	PoolReputation       float64 `json:"pool_reputation"`       // avg reputation (0..1) of the eligible supplier pool (routing transparency, research §4)
}

const slaMinEligibleWorkers = 5

const (
	slaPremiumRate = 0.15

	slaSafetyMarginFactor = 1.25

	slaMergeAllowanceSecs = 60
)

const slaRemedyText = "If your job's results are not merged within guaranteed_secs of submission, the SLA premium is refunded automatically as a ledger credit (netted off the amount collected) and the miss is recorded on the job timeline. The job always runs to completion  -  a miss triggers the refund, never a kill. Your existing partial-settle rights (pay only for completed tasks) are unchanged."

type QuoteSLA struct {
	GuaranteedSecs        int     `json:"guaranteed_secs"`
	PremiumUSD            float64 `json:"premium_usd"`
	ConservativeModelSecs int     `json:"conservative_model_secs"` // planner conservative band [MODELED from measured rates]
	SafetyMarginFactor    float64 `json:"safety_margin_factor"`
	MergeAllowanceSecs    int     `json:"merge_allowance_secs"`
	Remedy                string  `json:"remedy"`
}

func slaGuaranteedSecs(conservativeSecs int) int {
	if conservativeSecs <= 0 {
		return 0
	}
	return int(math.Ceil(float64(conservativeSecs)*slaSafetyMarginFactor)) + slaMergeAllowanceSecs
}

func deriveQuoteSLA(slaEligible, plannerBacked bool, conservativeSecs int, expectedUSD float64) *QuoteSLA {
	if !slaEligible || !plannerBacked || conservativeSecs <= 0 {
		return nil
	}
	premium := roundUSD(expectedUSD * slaPremiumRate)
	if premium <= 0 {
		return nil
	}
	return &QuoteSLA{
		GuaranteedSecs:        slaGuaranteedSecs(conservativeSecs),
		PremiumUSD:            premium,
		ConservativeModelSecs: conservativeSecs,
		SafetyMarginFactor:    slaSafetyMarginFactor,
		MergeAllowanceSecs:    slaMergeAllowanceSecs,
		Remedy:                slaRemedyText,
	}
}

const sustainedThroughputGap = 0.366

const sustainedDeratingFactor = 1.0 / (1.0 - sustainedThroughputGap)

const sustainedETAThresholdSecs = 120

func sustainedBatchETASecs(peakP50Secs int, tier string, usedObservedHistory bool) int {
	if tier != "batch" || usedObservedHistory || peakP50Secs < sustainedETAThresholdSecs {
		return peakP50Secs
	}
	adjusted := int(math.Ceil(float64(peakP50Secs) * sustainedDeratingFactor))
	if adjusted < peakP50Secs {
		adjusted = peakP50Secs // never shorten (defensive; the factor is >1)
	}
	return adjusted
}

type QuoteCost struct {
	MinUSD                  float64 `json:"min_usd"`
	ExpectedUSD             float64 `json:"expected_usd"`
	MaxUSD                  float64 `json:"max_usd"`
	VerificationOverheadUSD float64 `json:"verification_overhead_usd"`
	// PlatformTakeUSD is retained for existing clients. It is the gross ledger
	// spread (buyer charge less supplier entitlement), never Merc's profit.
	PlatformTakeUSD          float64 `json:"platform_take_usd"`
	PlatformGrossSpreadUSD   float64 `json:"platform_gross_spread_usd"`
	KnownCostContributionUSD float64 `json:"known_cost_contribution_usd"`
	// TrueNetContributionUSD is absent while any named cost category is
	// unknown. A quote must not promote a modeled contribution into profit.
	TrueNetContributionUSD *float64 `json:"true_net_contribution_usd,omitempty"`
}

type QuoteTime struct {
	P50Secs int `json:"p50_secs"`
	// P90Secs is a compatibility field. Its exact semantics are declared by
	// ConfidenceBandMethod; it is never implied to be an observed percentile.
	P90Secs              int    `json:"p90_secs"`
	WorstCaseSecs        int    `json:"worst_case_secs"`
	ConfidenceBandMethod string `json:"confidence_band_method,omitempty"`
}

type QuoteConfidence struct {
	Score   float64  `json:"score"`
	Reasons []string `json:"reasons"`
}

type QuoteBudget struct {
	SuggestedMaxUSD       float64 `json:"suggested_max_usd"`
	CancelBeforeExceeding bool    `json:"cancel_before_exceeding"`
}

const (
	claimableWorkerPredicateVersion = 1
	placementRequirementVersion     = claimableWorkerPredicateVersion
)

// PlacementRequirement is the immutable, buyer-visible worker eligibility
// authority used for both capacity claims and later job dispatch. It includes
// every worker/supplier predicate that can make an otherwise-capable device
// unable to claim the accepted job.
type PlacementRequirement struct {
	Version             int      `json:"version"`
	JobType             string   `json:"job_type"`
	ModelRef            string   `json:"model_ref"`
	ModelKind           string   `json:"model_kind"`
	RuntimeCellID       string   `json:"runtime_cell_id"`
	RuntimeID           string   `json:"runtime_id"`
	Engine              string   `json:"engine"`
	RuntimeMatrixSHA256 string   `json:"runtime_matrix_sha256"`
	MinMemoryGB         float32  `json:"min_memory_gb"`
	HWClasses           []string `json:"hw_classes,omitempty"`
	DataResidency       []string `json:"data_residency,omitempty"`
	MinReputation       float32  `json:"min_reputation"`
	TrustedOnly         bool     `json:"trusted_only"`
	// OfferedRateUsdHr is retained on the wire for compatibility. Its exact
	// meaning is the modeled supplier ask admission ceiling in USD/hour, not
	// guaranteed realized hourly pay.
	OfferedRateUsdHr float32 `json:"offered_rate_usd_hr"`
}

func placementRequirementFor(
	sub jobSubmit,
	workload WorkloadDecision,
	offeredRateUsdHr float32,
) (PlacementRequirement, error) {
	if len(workload.RuntimeCandidates) != 1 {
		return PlacementRequirement{}, fmt.Errorf(
			"placement authority requires exactly one runtime candidate, got %d",
			len(workload.RuntimeCandidates),
		)
	}
	candidate := workload.RuntimeCandidates[0]
	// Placement model_kind is the frozen cell's artifact format, not the buyer's
	// declaration. Claim matching already uses the cell's kind; capacity and
	// worker filters must agree or a directed/multi-kind freeze would look for
	// workers under the wrong wire kind.
	modelKind := candidate.ModelKind
	if modelKind == "" {
		modelKind = sub.Model.Kind
	}
	out := PlacementRequirement{
		Version:             placementRequirementVersion,
		JobType:             workload.RuntimeJobType,
		ModelRef:            sub.Model.Ref,
		ModelKind:           modelKind,
		RuntimeCellID:       candidate.CellID,
		RuntimeID:           candidate.RuntimeID,
		Engine:              candidate.Engine,
		RuntimeMatrixSHA256: generatedRuntimeMatrixSHA256,
		MinMemoryGB:         float32(workload.MinimumMemoryGB),
		HWClasses:           append([]string(nil), sub.Constraints.HWClasses...),
		DataResidency:       append([]string(nil), sub.Constraints.DataResidency...),
		MinReputation:       sub.MinReputation,
		TrustedOnly:         sub.Tier == "trusted",
		OfferedRateUsdHr:    offeredRateUsdHr,
	}
	if err := validatePlacementRequirement(out, workload); err != nil {
		return PlacementRequirement{}, err
	}
	return out, nil
}

func validatePlacementRequirement(p PlacementRequirement, workload WorkloadDecision) error {
	if p.Version != placementRequirementVersion {
		return fmt.Errorf("unsupported placement requirement version %d", p.Version)
	}
	if len(workload.RuntimeCandidates) != 1 {
		return errors.New("placement requirement needs one frozen runtime candidate")
	}
	candidate := workload.RuntimeCandidates[0]
	binding := workload.Binding
	// Prefer the frozen cell's kind; fall back to the binding only for decisions
	// written before runtime candidates carried model_kind.
	wantKind := candidate.ModelKind
	if wantKind == "" {
		wantKind = binding.Model.Kind
	}
	if p.JobType != workload.RuntimeJobType ||
		p.ModelRef != binding.Model.Ref ||
		p.ModelKind != wantKind ||
		p.RuntimeCellID != candidate.CellID ||
		p.RuntimeID != candidate.RuntimeID ||
		p.Engine != candidate.Engine ||
		p.RuntimeMatrixSHA256 != generatedRuntimeMatrixSHA256 ||
		p.MinMemoryGB != float32(workload.MinimumMemoryGB) ||
		!sameStrings(p.HWClasses, binding.Constraints.HWClasses) ||
		!sameStrings(p.DataResidency, binding.Constraints.DataResidency) ||
		p.MinReputation != binding.MinReputation ||
		p.TrustedOnly != (binding.Tier == "trusted") ||
		math.IsNaN(float64(p.OfferedRateUsdHr)) ||
		math.IsInf(float64(p.OfferedRateUsdHr), 0) ||
		p.OfferedRateUsdHr < 0 {
		return errors.New("placement requirement conflicts with frozen workload authority")
	}
	return nil
}

func sameStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// QuoteSupplyRequirements is the internal query form. A nil offered rate and
// empty exact-runtime fields preserve the legacy diagnostic wrappers; quotes
// and planner calls always use PlacementRequirement.supplyRequirements().
type QuoteSupplyRequirements struct {
	JobType       string
	ModelRef      string
	ModelKind     string
	RuntimeCellID string
	RuntimeID     string
	Engine        string
	MatrixSHA256  string
	MinMemoryGB   float32
	HWClasses     []string
	DataResidency []string
	MinReputation float32
	TrustedOnly   bool
	OfferedRate   *float32
}

func (p PlacementRequirement) supplyRequirements() QuoteSupplyRequirements {
	offered := p.OfferedRateUsdHr
	return QuoteSupplyRequirements{
		JobType: p.JobType, ModelRef: p.ModelRef, ModelKind: p.ModelKind,
		RuntimeCellID: p.RuntimeCellID, RuntimeID: p.RuntimeID, Engine: p.Engine,
		MatrixSHA256:  p.RuntimeMatrixSHA256,
		MinMemoryGB:   p.MinMemoryGB,
		HWClasses:     append([]string(nil), p.HWClasses...),
		DataResidency: append([]string(nil), p.DataResidency...),
		MinReputation: p.MinReputation, TrustedOnly: p.TrustedOnly,
		OfferedRate: &offered,
	}
}

func (s *Server) quoteInitialEconomicTaskCounts(ctx context.Context, sub jobSubmit, primaryTasks int) (redundancy, honeypots, total int, err error) {
	if primaryTasks <= 0 {
		return 0, 0, 0, nil
	}
	redundancy = fracCount(primaryTasks, sub.Verification.RedundancyFrac)
	// Media has no known-answer corpus. Its independent byte-exact execution
	// is the verification contract, so quote and submit must never inject a
	// JSONL honeypot or silently price one when the request carries media.
	if isBinaryMediaJob(sub) {
		if redundancy == 0 {
			redundancy = primaryTasks
		}
		return redundancy, 0, primaryTasks + redundancy, nil
	}
	honeypots = fracCount(primaryTasks, sub.Verification.HoneypotFrac)
	if sub.Verification.RedundancyFrac <= 0 && sub.Verification.HoneypotFrac <= 0 && honeypots == 0 {
		honeypots = 1
	}
	if honeypots > 0 {
		available, availableErr := s.store.AvailableSeedHoneypots(ctx, sub.JobType.Type, sub.Model.Ref, sub.JobType.MaxTokens, honeypots)
		if availableErr != nil {
			return redundancy, 0, primaryTasks + redundancy,
				fmt.Errorf("%w: reading seeded honeypots: %v", errQuoteVerificationUnavailable, availableErr)
		}
		honeypots = len(available)
		if honeypots == 0 {
			return redundancy, 0, primaryTasks + redundancy,
				fmt.Errorf("%w: verification requires a seeded honeypot but none is available", errQuoteVerificationUnavailable)
		}
	}
	return redundancy, honeypots, primaryTasks + redundancy + honeypots, nil
}

var errQuoteVerificationUnavailable = errors.New("quote verification authority is unavailable")

func (s *Server) buildQuoteWithSchedule(ctx context.Context, buyerID uuid.UUID, sub jobSubmit, inputBytes []byte, workload WorkloadDecision, schedule EconomicSchedule) (Quote, error) {
	jobType := sub.JobType.Type
	tier := sub.Tier
	var scan QuoteInputScan
	var scanErr error
	mediaSegments := 1
	if isBinaryMediaJob(sub) {
		if isMediaRenderingJob(sub) {
			scan, scanErr = renderingInputScan(inputBytes)
		} else {
			mediaSegments, scanErr = mediaSegmentCountFromParams(sub.Params)
			if scanErr == nil {
				scan, scanErr = mediaInputScan(inputBytes, mediaSegments)
			}
		}
	} else {
		scan = scanJSONL(inputBytes)
	}
	if scanErr != nil {
		return Quote{}, fmt.Errorf("scanning input: %w", scanErr)
	}
	if isMediaTranscodeJob(sub) {
		if err := refuseSegmentedMediaCrossSupplierRedundancy(mediaSegments, sub.Verification.RedundancyFrac); err != nil {
			return Quote{}, err
		}
	}
	catalogue, err := s.store.LoadCataloguePriceAuthority(ctx, sub.Model.Ref)
	if err != nil {
		return Quote{}, fmt.Errorf("resolving catalogue price authority: %w", err)
	}
	offeredRate64, err := supplierAdmissionCeilingUSDHr(
		catalogue, jobType, tier, admissionCellsForWorkload(workload),
	)
	if err != nil {
		return Quote{}, fmt.Errorf("deriving supplier admission ceiling: %w", err)
	}
	offeredRate := float32(offeredRate64)
	placement, err := placementRequirementFor(sub, workload, offeredRate)
	if err != nil {
		return Quote{}, fmt.Errorf("building placement requirement: %w", err)
	}
	supply := placement.supplyRequirements()

	avgLineBytes := 0.0
	if scan.Records > 0 {
		avgLineBytes = float64(len(inputBytes)) / float64(scan.Records)
	}
	split := adaptiveSplitSize(jobType, sub.Params, avgLineBytes)
	if isBinaryMediaJob(sub) {
		// Media decomposes by segment_count into N records; each task still
		// owns exactly one segment (split_size=1). Byte-slicing a container
		// would create invalid inputs, so the adaptive JSONL splitter is off.
		split = 1
	} else if jobType == "embed" && sub.JobType.BatchSize > 0 && !hasExplicitSplitSize(sub.Params) {
		split = sub.JobType.BatchSize
	} else if !hasExplicitSplitSize(sub.Params) {
		split = s.adaptiveSplitSizeLiveFor(
			ctx, supply, sub.JobType.MaxTokens, avgLineBytes, split, scan.Records,
		)
	}
	tasks := 0
	if scan.Records > 0 && split > 0 {
		tasks = (scan.Records + split - 1) / split
	}

	expected, err := estimateJobSettlementForJobType(
		catalogue, sub.JobType, len(inputBytes), scan.Records, sub.Tier,
	)
	if err != nil {
		return Quote{}, fmt.Errorf("pricing quote: %w", err)
	}
	primaryComputeUSD := expected
	verifOverhead := roundUSD(expected * float64(sub.Verification.RedundancyFrac+sub.Verification.HoneypotFrac))
	wantVerificationFloor := sub.Verification.RedundancyFrac <= 0 && sub.Verification.HoneypotFrac <= 0
	if wantVerificationFloor && tasks > 0 && fracCount(tasks, sub.Verification.HoneypotFrac) == 0 {
		verifOverhead = roundUSD(math.Max(verifOverhead, expected/float64(tasks)))
	}
	redundancyTasks, honeypotTasks, initialEconomicTasks, economicCountErr :=
		s.quoteInitialEconomicTaskCounts(ctx, sub, tasks)
	if economicCountErr != nil {
		return Quote{}, economicCountErr
	}
	baseComputeUSD := expected
	if tasks > 0 && initialEconomicTasks > 0 {
		baseComputeUSD = roundEconomicUSD(expected * float64(initialEconomicTasks) / float64(tasks))
		verifOverhead = roundEconomicUSD(math.Max(0, baseComputeUSD-expected))
	}
	// The same base compute, exact, straight from the catalogue.
	//
	// Everything above this line has already been through roundEconomicUSD at least
	// once; on a job whose whole value is under two micro-USD that costs 30% before
	// the supplier's share is taken. This is the unrounded figure the pricing
	// decision derives the supplier's floor from, so the plan and the floor are the
	// same expression rather than two roundings of it.
	baseComputeNanos := exactBaseComputeNanosForJobType(
		catalogue, sub.JobType, tier, len(inputBytes), scan.Records,
		tasks, initialEconomicTasks,
	)

	costMin := roundUSD(expected * 0.85)
	costMax := roundUSD((expected + verifOverhead) * 1.5)

	var modelMinMem float32
	if m, err := s.store.GetModel(ctx, sub.Model.Ref); err == nil {
		modelMinMem = m.MinMemoryGB
	}

	depthBand := scan.InputDepth.P90DepthBand
	p50, conservativeSecs, plannerBacked := s.etaBandSecsFor(ctx, supply, tasks, depthBand)
	observedP90ms, _, hErr := s.store.HistoricalP90DurationMs(ctx, jobType, sub.Model.Ref, depthBand)
	usedObservedHistory := hErr == nil && observedP90ms > 0
	p50 = sustainedBatchETASecs(p50, tier, usedObservedHistory)
	if plannerBacked && conservativeSecs < p50 {
		// Sustained-throughput derating applies to the p50 after planner output;
		// do not leave its conservative bound below that buyer-visible estimate.
		conservativeSecs = p50
	}
	rawP50 := p50
	// Correct for measured optimism before the number is frozen. The SLA bound
	// below is derived from conservativeSecs, so it has to move with the p50 or
	// calibration would tighten the promise it was meant to make honest.
	etaBias, etaBiasSamples, etaBiasErr := s.store.ETABiasFactor(
		ctx, jobType, tier, sub.Model.Ref, depthBand,
	)
	etaCalibrated := etaBiasErr == nil && etaBiasSamples >= driftMinSamples && etaBias > 1
	if etaCalibrated {
		p50 = applyETABias(p50, etaBias)
		conservativeSecs = applyETABias(conservativeSecs, etaBias)
	}
	eta := quoteTimeFromETABands(p50, conservativeSecs, plannerBacked)

	eligibleNow, _ := s.store.EligibleWorkerCountFor(ctx, jobType, sub.Model.Ref, supply)
	warmEligible, _ := s.store.WarmEligibleWorkerCountFor(ctx, jobType, sub.Model.Ref, supply)

	oomRisk, coldRisk, conf, warnings := assessRisk(scan, eligibleNow, warmEligible, modelMinMem)

	poolRep, _ := s.store.EligiblePoolReputationFor(ctx, jobType, sub.Model.Ref, supply)
	slaEligible := eligibleNow >= slaMinEligibleWorkers
	if !slaEligible {
		warnings = append(warnings, fmt.Sprintf(
			"supply below the SLA threshold (%d eligible, need %d): ETA is advisory only, no project-SLA guarantee",
			eligibleNow, slaMinEligibleWorkers))
	}
	basePlanInput := EconomicPlanInput{
		BaseComputeUSD:   baseComputeUSD,
		InitialTaskCount: initialEconomicTasks,
		ExtraTaskReserve: economicExtraTaskReserve(tasks),
		SupplierShare:    catalogue.SupplierShare,
		BaseComputeNanos: baseComputeNanos,
	}
	baseEconomicPlan := BuildEconomicPlan(basePlanInput, schedule)
	if baseEconomicPlan.Executable && initialEconomicTasks >= tasks {
		verifOverhead = roundEconomicUSD(
			baseEconomicPlan.BuyerChargePerTaskUSD * float64(initialEconomicTasks-tasks),
		)
	}
	quoteSLA := deriveQuoteSLA(
		slaEligible && baseEconomicPlan.Executable,
		plannerBacked,
		conservativeSecs,
		baseEconomicPlan.InitialBuyerChargeUSD,
	)
	if quoteSLA != nil {
		warnings = append(warnings, fmt.Sprintf(
			"speed-SLA offer: guaranteed completion within %ds of submission for a $%.6f premium (auto-refunded on a miss); binds only when you submit with firm_quote=true and this quote_id",
			quoteSLA.GuaranteedSecs, quoteSLA.PremiumUSD))
	}

	if modelMinMem > 0 {
		if median, ok, err := s.store.MedianEffectiveMemoryGB(ctx, jobType, sub.Model.Ref); err == nil && ok {
			oomRisk, conf = applyMemoryFloorRisk(oomRisk, conf, modelMinMem, median)
		}
	}

	bareID := uuid.New()
	sum := sha256.Sum256(inputBytes)

	slaPremium := 0.0
	if quoteSLA != nil {
		slaPremium = quoteSLA.PremiumUSD
	}
	planInput := basePlanInput
	planInput.SLAPremiumUSD = slaPremium
	economicPlan := BuildEconomicPlan(planInput, schedule)
	if economicPlan.Executable {
		expected = baseEconomicPlan.InitialBuyerChargeUSD
		costMax = baseEconomicPlan.ReservedBuyerChargeUSD
		costMin = baseEconomicPlan.BuyerChargePerTaskUSD
	} else {
		warnings = append(warnings, "economics blocked: "+economicPlan.BlockReason)
	}

	etaSource := computePlanETASource(plannerBacked, usedObservedHistory, etaCalibrated)
	if err := validateInputDepthProfile(scan.InputDepth); err != nil {
		return Quote{}, fmt.Errorf("measuring input depth profile: %w", err)
	}
	classified, ok := checkedInputDepthRecordCount(
		scan.InputDepth.ShortRecords,
		scan.InputDepth.MediumRecords,
		scan.InputDepth.LongRecords,
	)
	if !ok || classified != scan.Records {
		return Quote{}, fmt.Errorf(
			"input depth profile covers %d records but scan counted %d",
			classified, scan.Records)
	}
	computePlan, err := newDistributedComputePlan(
		workload,
		scan.Records,
		int64(len(inputBytes)),
		scan.InputDepth,
		split,
		tasks,
		redundancyTasks,
		honeypotTasks,
		eta,
		etaSource,
		primaryComputeUSD,
		math.Max(0, baseComputeUSD-primaryComputeUSD),
		conf,
		[]string{
			"token counts use a documented body-depth estimator rather than the model tokenizer",
			"ETA is a frozen estimate; queue contention after acceptance can change wall-clock completion",
			"power draw and provider-specific energy cost are not yet modeled",
		},
	)
	if err != nil {
		return Quote{}, fmt.Errorf("building compute plan: %w", err)
	}
	pricing, err := newDistributedPricingDecision(
		workload, computePlan, placement, economicPlan, catalogue, tier, "",
	)
	if err != nil {
		return Quote{}, fmt.Errorf("building composite pricing decision: %w", err)
	}
	platformGrossSpread := roundEconomicUSD(
		pricing.BuyerPrice - pricing.PrimarySupplierCost.Amount - pricing.VerificationCost.Amount,
	)
	knownContribution := pricing.PlatformContribution.Amount
	var trueNetContribution *float64
	if fixed := pricing.FixedPoint; fixed != nil && fixed.TrueNetContributionNanos != nil {
		amount := float64(*fixed.TrueNetContributionNanos) / float64(NanosPerMajorUnit)
		trueNetContribution = &amount
	}

	return Quote{
		QuoteID:       "q_" + bareID.String(),
		bareID:        bareID,
		etaRawP50Secs: rawP50,
		ExpiresAt:     time.Now().Add(quoteTTL).UTC(),
		InputSHA256:   hex.EncodeToString(sum[:]),
		JobType:       jobType,
		Model:         sub.Model.Ref,
		Tier:          tier,
		Currency:      SettlementCurrencyCode(),
		TierSemantics: serviceTierSemantics(tier),
		Workload:      workload,
		Placement:     placement,
		ComputePlan:   computePlan,
		Pricing:       pricing,
		Input:         scan,
		SLA:           quoteSLA,
		Economics:     economicPlan,
		Execution: QuoteExecution{
			RecommendedSplitSize: split,
			EstimatedTasks:       tasks,
			EligibleWorkersNow:   eligibleNow,
			WarmEligibleWorkers:  warmEligible,
			ModelMinMemoryGB:     modelMinMem,
			OOMRisk:              oomRisk,
			ColdStartRisk:        coldRisk,
			SLAEligible:          slaEligible,
			PoolReputation:       poolRep,
		},
		Cost: QuoteCost{
			MinUSD: costMin, ExpectedUSD: expected, MaxUSD: costMax,
			VerificationOverheadUSD:  verifOverhead,
			PlatformTakeUSD:          platformGrossSpread,
			PlatformGrossSpreadUSD:   platformGrossSpread,
			KnownCostContributionUSD: knownContribution,
			TrueNetContributionUSD:   trueNetContribution,
		},
		Time:       eta,
		Confidence: conf,
		Budget: QuoteBudget{
			SuggestedMaxUSD:       costMax,
			CancelBeforeExceeding: true,
		},
		Warnings: warnings,
	}, nil
}

func assessRisk(scan QuoteInputScan, eligibleNow, warmEligible int, modelMinMem float32) (oom, cold string, conf QuoteConfidence, warnings []string) {
	reasons := []string{}
	score := 0.8

	switch {
	case eligibleNow == 0:
		oom = "high"
		score -= 0.3
		reasons = append(reasons, "no workers currently pass the memory/model filter for this job")
		warnings = append(warnings, "no eligible workers online right now; the job may queue until supply appears")
	case eligibleNow < 3:
		oom = "medium"
		score -= 0.1
		reasons = append(reasons, fmt.Sprintf("%d eligible worker(s) with enough effective memory (thin supply)", eligibleNow))
	default:
		oom = "low"
		reasons = append(reasons, fmt.Sprintf("%d eligible workers have enough effective memory", eligibleNow))
	}
	if modelMinMem > 0 {
		reasons = append(reasons, fmt.Sprintf("model memory floor is %.0f GB; supply count is filtered against effective memory", modelMinMem))
	} else {
		reasons = append(reasons, "model not in the catalogue; using a conservative default price + no memory floor")
		score -= 0.1
	}

	if warmEligible > 0 {
		cold = "low"
		score += 0.1
		reasons = append(reasons, fmt.Sprintf("%d eligible worker(s) already have this model warm; cold-start unlikely", warmEligible))
	} else {
		cold = "medium"
		reasons = append(reasons, "no eligible worker currently has this model warm; a cold model load is possible")
	}

	if scan.Records == 0 {
		score -= 0.4
		warnings = append(warnings, "input has no records")
	}
	if scan.MalformedRecords > 0 {
		score -= 0.2
		warnings = append(warnings, fmt.Sprintf("%d malformed JSONL record(s); first at line %d", scan.MalformedRecords, scan.FirstBadLine))
	}
	reasons = append(reasons, "token count is a byte heuristic, not an exact tokenizer count")

	if score < 0.05 {
		score = 0.05
	}
	if score > 0.95 {
		score = 0.95
	}
	return oom, cold, QuoteConfidence{Score: roundUSD(score), Reasons: reasons}, warnings
}

func applyMemoryFloorRisk(oom string, conf QuoteConfidence, modelMinMem, medianEffectiveGB float32) (string, QuoteConfidence) {
	switch {
	case modelMinMem > medianEffectiveGB:
		oom = escalateRisk(oom)
		conf.Score = roundUSD(clampScore(conf.Score - 0.15))
		conf.Reasons = append(conf.Reasons, fmt.Sprintf(
			"model memory floor %.0f GB exceeds the median effective memory of eligible workers (%.1f GB); the typical eligible worker is tight on this model",
			modelMinMem, medianEffectiveGB))
	case medianEffectiveGB-modelMinMem <= memFloorTightMargin:
		conf.Score = roundUSD(clampScore(conf.Score - 0.05))
		conf.Reasons = append(conf.Reasons, fmt.Sprintf(
			"model memory floor %.0f GB is close to the median effective memory of eligible workers (%.1f GB); little headroom",
			modelMinMem, medianEffectiveGB))
	default:
		conf.Reasons = append(conf.Reasons, fmt.Sprintf(
			"median effective memory of eligible workers (%.1f GB) comfortably clears the model floor (%.0f GB)",
			medianEffectiveGB, modelMinMem))
	}
	return oom, conf
}

const memFloorTightMargin = 2.0

func escalateRisk(r string) string {
	switch r {
	case "low":
		return "medium"
	case "medium":
		return "high"
	default:
		return "high"
	}
}

func clampScore(s float64) float64 {
	if s < 0.05 {
		return 0.05
	}
	if s > 0.95 {
		return 0.95
	}
	return s
}

func (s *Server) handleQuote(w http.ResponseWriter, r *http.Request) {
	auth := r.Context().Value(ctxBuyer).(*AuthResult)
	raw, err := io.ReadAll(r.Body)
	if err != nil {
		var mbe *http.MaxBytesError
		if errors.As(err, &mbe) {
			writeErr(w, http.StatusRequestEntityTooLarge,
				fmt.Sprintf("request body exceeds the %d byte quote limit", mbe.Limit))
			return
		}
		writeErr(w, http.StatusBadRequest, "reading quote request: "+err.Error())
		return
	}
	var sub jobSubmit
	if err := decodeStrictJSONObject(raw, &sub); err != nil {
		writeErr(w, http.StatusBadRequest, "invalid quote request json: "+err.Error())
		return
	}
	normalized, herr := s.normalizeWorkloadRequest(sub)
	if herr != nil {
		writeErr(w, herr.status, herr.msg)
		return
	}
	sub = normalized
	schedule, err := LoadEconomicScheduleFromEnv()
	if err != nil {
		writeErr(w, http.StatusServiceUnavailable, "economic schedule unavailable: "+err.Error())
		return
	}
	inputReader, _, err := s.resolveInput(r.Context(), auth.BuyerID, sub.JobType.Type, sub.Input)
	if err != nil {
		writeErr(w, http.StatusBadRequest, "resolving input: "+err.Error())
		return
	}
	inputLimit := int64(maxSynchronousInputBytes)
	if isMediaTranscodeJob(sub) {
		inputLimit = maxMediaControlBytes
	} else if isMediaRenderingJob(sub) {
		inputLimit = maxRenderingControlBytes
	}
	inputBytes, err := readAndCloseBounded(inputReader, inputLimit)
	if err != nil {
		status := http.StatusBadRequest
		if errors.Is(err, errSynchronousInputTooLarge) {
			status = http.StatusRequestEntityTooLarge
		}
		writeErr(w, status, "reading input: "+err.Error())
		return
	}
	if isMediaTranscodeJob(sub) {
		if err := validateMediaInputBytes(inputBytes); err != nil {
			writeErr(w, http.StatusBadRequest, err.Error())
			return
		}
	} else if isMediaRenderingJob(sub) {
		if err := validateRenderingInputBytes(inputBytes, sub.JobType.RenderWidth, sub.JobType.RenderHeight); err != nil {
			writeErr(w, http.StatusBadRequest, err.Error())
			return
		}
	} else if err := validateWorkloadJSONL(sub.JobType.Type, inputBytes); err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	inputSum := sha256.Sum256(inputBytes)
	workload, err := buildWorkloadDecision(sub, hex.EncodeToString(inputSum[:]))
	if err != nil {
		writeErr(w, http.StatusBadRequest, "classifying workload: "+err.Error())
		return
	}
	q, err := s.buildQuoteWithSchedule(r.Context(), auth.BuyerID, sub, inputBytes, workload, schedule)
	if err != nil {
		status := http.StatusInternalServerError
		if errors.Is(err, errQuoteVerificationUnavailable) {
			status = http.StatusServiceUnavailable
		}
		writeErr(w, status, err.Error())
		return
	}
	if !q.Economics.Executable {
		writeErr(w, http.StatusConflict, "quote is not executable: "+q.Economics.BlockReason)
		return
	}
	if err := s.store.InsertQuote(r.Context(), auth.BuyerID, q); err != nil {
		writeErr(w, http.StatusInternalServerError, "persisting quote: "+err.Error())
		return
	}
	metrics.quotes.Add(1) // observability (Plane D D21): a quote was priced + persisted
	writeJSON(w, http.StatusOK, q)
}

type pipelineQuoteStage struct {
	Op    string `json:"op"`
	Model string `json:"model"`
}

type pipelineQuoteRequest struct {
	Input  json.RawMessage      `json:"input"`
	Tier   string               `json:"tier"`
	Stages []pipelineQuoteStage `json:"stages"`
}

func (s *Store) EligibleWorkerCount(ctx context.Context, jobType, modelRef string, minMemGB float32) (int, error) {
	return s.EligibleWorkerCountFor(ctx, jobType, modelRef, QuoteSupplyRequirements{MinMemoryGB: minMemGB})
}

const claimableWorkerPredicateSQL = `
	w.last_seen_at IS NOT NULL
	AND w.last_seen_at > now() - interval '60 seconds'
	AND s.status = 'active'
	AND NOT COALESCE(w.throttled, false)
	AND COALESCE($3,0) <= COALESCE(w.effective_memory_gb, w.memory_gb, 0)
	AND ($4::text[] IS NULL OR w.hw_class = ANY($4))
	AND ($5::text[] IS NULL OR s.data_country = ANY($5))
	AND COALESCE(s.reputation,0) >= $6
	AND (NOT $7 OR (COALESCE(s.reputation,0) >= 0.80 AND COALESCE(s.completed_tasks,0) >= 500))
	AND ($9::real IS NULL OR COALESCE(w.min_payout_usd_hr,0) <= $9)
	AND EXISTS (
	  SELECT 1 FROM worker_authorized_capabilities wac
	   WHERE wac.worker_id = w.id
	     AND wac.job_type = $1
	     AND wac.model_ref = $2
	     AND wac.matrix_sha256 = $8
	     AND ($10 = '' OR (
	       wac.model_kind = $10
	       AND wac.cell_id = $11
	       AND wac.runtime_id = $12
	       AND COALESCE(w.engine,'') = $13
	     ))
	)`

func normalizedSupplyRequirements(jobType, modelRef string, req QuoteSupplyRequirements) QuoteSupplyRequirements {
	if req.JobType == "" {
		req.JobType = jobType
	}
	if req.ModelRef == "" {
		req.ModelRef = modelRef
	}
	if req.MatrixSHA256 == "" {
		req.MatrixSHA256 = generatedRuntimeMatrixSHA256
	}
	return req
}

func supplyRequirementQueryArgs(req QuoteSupplyRequirements) []any {
	var offered any
	if req.OfferedRate != nil {
		offered = *req.OfferedRate
	}
	return []any{
		req.JobType, req.ModelRef, req.MinMemoryGB, nullStrSlice(req.HWClasses),
		nullStrSlice(req.DataResidency), req.MinReputation, req.TrustedOnly,
		req.MatrixSHA256, offered, req.ModelKind, req.RuntimeCellID, req.RuntimeID, req.Engine,
	}
}

func (s *Store) EligibleWorkerCountFor(ctx context.Context, jobType, modelRef string, req QuoteSupplyRequirements) (int, error) {
	req = normalizedSupplyRequirements(jobType, modelRef, req)
	var n int
	err := s.pool.QueryRow(ctx,
		`SELECT count(*)
		   FROM workers w JOIN suppliers s ON s.id = w.supplier_id
		  WHERE `+claimableWorkerPredicateSQL,
		supplyRequirementQueryArgs(req)...,
	).Scan(&n)
	return n, err
}

func (s *Store) EligiblePoolReputation(ctx context.Context, jobType, modelRef string, minMemGB float32) (float64, error) {
	return s.EligiblePoolReputationFor(ctx, jobType, modelRef, QuoteSupplyRequirements{MinMemoryGB: minMemGB})
}

func (s *Store) EligiblePoolReputationFor(ctx context.Context, jobType, modelRef string, req QuoteSupplyRequirements) (float64, error) {
	req = normalizedSupplyRequirements(jobType, modelRef, req)
	var r float64
	err := s.pool.QueryRow(ctx,
		`SELECT COALESCE(AVG(s.reputation), 0)
		   FROM workers w JOIN suppliers s ON s.id = w.supplier_id
		  WHERE `+claimableWorkerPredicateSQL,
		supplyRequirementQueryArgs(req)...,
	).Scan(&r)
	return r, err
}

func (s *Store) WarmEligibleWorkerCount(ctx context.Context, jobType, modelRef string, minMemGB float32) (int, error) {
	return s.WarmEligibleWorkerCountFor(ctx, jobType, modelRef, QuoteSupplyRequirements{MinMemoryGB: minMemGB})
}

func (s *Store) WarmEligibleWorkerCountFor(ctx context.Context, jobType, modelRef string, req QuoteSupplyRequirements) (int, error) {
	if modelRef == "" {
		return 0, nil
	}
	req = normalizedSupplyRequirements(jobType, modelRef, req)
	var n int
	err := s.pool.QueryRow(ctx,
		`SELECT count(*)
		   FROM workers w
		   JOIN suppliers s ON s.id = w.supplier_id
		   JOIN worker_model_state wms
		     ON wms.worker_id = w.id
		    AND wms.model_id = $2
		    AND wms.last_seen_warm > now() - interval '60 seconds'
		  WHERE `+claimableWorkerPredicateSQL,
		supplyRequirementQueryArgs(req)...,
	).Scan(&n)
	return n, err
}

func (s *Store) InsertQuote(ctx context.Context, buyerID uuid.UUID, q Quote) error {
	if err := RequireSettlementCurrency(q.Currency); err != nil {
		return fmt.Errorf("refusing quote outside the settlement currency: %w", err)
	}
	if q.Economics.Schedule.Currency != q.Currency {
		return fmt.Errorf(
			"refusing quote whose economic currency %q differs from quote currency %q",
			q.Economics.Schedule.Currency, q.Currency,
		)
	}
	if err := validatePlacementRequirement(q.Placement, q.Workload); err != nil {
		return fmt.Errorf("refusing quote without valid placement authority: %w", err)
	}
	if err := ValidateWorkloadDecisionSnapshot(q.Workload); err != nil {
		return fmt.Errorf("refusing quote without valid workload decision: %w", err)
	}
	if err := ValidateComputePlanEconomicSnapshot(q.ComputePlan, q.Workload, q.Economics); err != nil {
		return fmt.Errorf("refusing quote without valid compute plan: %w", err)
	}
	if err := ValidateDistributedPricingDecisionSnapshotWithStore(
		ctx, s, q.Pricing, q.Workload, q.ComputePlan, q.Placement, q.Economics,
	); err != nil {
		return fmt.Errorf("refusing quote without valid composite pricing authority: %w", err)
	}
	if q.etaRawP50Secs <= 0 || q.Time.P50Secs < q.etaRawP50Secs ||
		q.ComputePlan.ETAP50Secs != q.Time.P50Secs ||
		q.ComputePlan.ETAP90Secs != q.Time.P90Secs ||
		q.ComputePlan.ETAWorstCaseSecs != q.Time.WorstCaseSecs ||
		q.ComputePlan.ETAConfidenceBandMethod != q.Time.ConfidenceBandMethod {
		return errors.New("refusing quote without valid raw and calibrated ETA authority")
	}
	decisionSHA256, err := workloadDecisionDigest(q.Workload)
	if err != nil {
		return fmt.Errorf("hashing quote workload decision: %w", err)
	}
	computeSHA256, err := computePlanDigest(q.ComputePlan)
	if err != nil {
		return fmt.Errorf("hashing quote compute plan: %w", err)
	}
	placementSHA256, err := placementRequirementDigest(q.Placement)
	if err != nil {
		return fmt.Errorf("hashing quote placement requirement: %w", err)
	}
	pricingSHA256, err := pricingDecisionDigest(q.Pricing)
	if err != nil {
		return fmt.Errorf("hashing quote pricing decision: %w", err)
	}
	blob, err := json.Marshal(q)
	if err != nil {
		return err
	}
	planBlob, err := json.Marshal(q.Economics)
	if err != nil {
		return err
	}
	computeBlob, err := json.Marshal(q.ComputePlan)
	if err != nil {
		return err
	}
	placementBlob, err := json.Marshal(q.Placement)
	if err != nil {
		return err
	}
	pricingBlob, err := json.Marshal(q.Pricing)
	if err != nil {
		return err
	}
	var slaSecs *int
	var slaPremium *float64
	if q.SLA != nil {
		slaSecs, slaPremium = &q.SLA.GuaranteedSecs, &q.SLA.PremiumUSD
	}
	_, err = s.pool.Exec(ctx,
		`INSERT INTO quotes
		   (id, buyer_id, job_type, model_ref, tier, records, input_bytes,
		    estimated_tokens, malformed_records, split_size, task_count, eligible_now,
		    cost_expected_usd, cost_min_usd, cost_max_usd, eta_p50_secs, eta_p90_secs,
		    eta_p50_secs_raw,
		    oom_risk, confidence, quote_json, expires_at, input_sha256,
		    sla_guaranteed_secs, sla_premium_usd,
		    economic_schedule_version, economic_plan, economic_executable,
		    workload_binding_sha256, workload_decision_sha256,
		    compute_plan, compute_plan_sha256,
		    placement_requirement, placement_requirement_sha256,
		    pricing_decision, pricing_decision_sha256, currency)
		 VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,$15,$16,$17,$18,$19,$20,$21,$22,$23,$24,$25,$26,$27,$28,$29,$30,$31,$32,$33,$34,$35,$36,$37)`,
		q.bareID, buyerID, q.JobType, q.Model, q.Tier, q.Input.Records, q.Input.Bytes,
		q.Input.EstimatedTokens, q.Input.MalformedRecords, q.Execution.RecommendedSplitSize,
		q.Execution.EstimatedTasks, q.Execution.EligibleWorkersNow,
		q.Cost.ExpectedUSD, q.Cost.MinUSD, q.Cost.MaxUSD, q.Time.P50Secs, q.Time.P90Secs,
		q.etaRawP50Secs,
		q.Execution.OOMRisk, q.Confidence.Score, blob, q.ExpiresAt, q.InputSHA256,
		slaSecs, slaPremium,
		q.Economics.Schedule.Version, planBlob, q.Economics.Executable,
		q.Workload.BindingSHA256, decisionSHA256,
		computeBlob, computeSHA256,
		placementBlob, placementSHA256, pricingBlob, pricingSHA256,
		q.Currency,
	)
	return err
}

func quoteIDToUUID(handle string) (uuid.UUID, error) {
	id, err := uuid.Parse(strings.TrimPrefix(strings.TrimSpace(handle), "q_"))
	if err != nil {
		return uuid.UUID{}, fmt.Errorf("invalid quote_id %q", handle)
	}
	return id, nil
}

type boundQuote struct {
	ID                      uuid.UUID
	JobType                 string
	ModelRef                string
	Tier                    string
	Currency                string
	Placement               PlacementRequirement
	InputSHA256             string
	CostExpUSD              float64
	CostMaxUSD              float64
	ETARawSecs              int
	Expired                 bool
	SLAGuaranteedSecs       int
	SLAPremiumUSD           float64
	EconomicScheduleVersion string
	EconomicPlan            EconomicPlan
	EconomicExecutable      bool
	WorkloadBindingSHA256   string
	WorkloadDecisionSHA256  string
	ComputePlan             ComputePlan
	ComputePlanSHA256       string
	PlacementSHA256         string
	Pricing                 PricingDecision
	PricingDecisionSHA256   string
}

func (s *Store) GetBindableQuote(ctx context.Context, quoteID, buyerID uuid.UUID) (*boundQuote, error) {
	var q boundQuote
	var planBlob []byte
	var computeBlob []byte
	var placementBlob []byte
	var pricingBlob []byte
	var quoteBlob []byte
	err := s.pool.QueryRow(ctx,
		`SELECT id, job_type, COALESCE(model_ref,''), COALESCE(tier,''),
		        COALESCE(input_sha256,''), COALESCE(cost_expected_usd,0),
		        COALESCE(cost_max_usd,0),
		        COALESCE(eta_p50_secs_raw,0),
		        (expires_at IS NOT NULL AND expires_at <= now()) AS expired,
		        COALESCE(sla_guaranteed_secs,0), COALESCE(sla_premium_usd,0)::float8,
		        COALESCE(economic_schedule_version,''), economic_plan,
		        COALESCE(economic_executable,false),
		        COALESCE(workload_binding_sha256,''),
		        COALESCE(workload_decision_sha256,''),
		        compute_plan, COALESCE(compute_plan_sha256,''),
		        placement_requirement,COALESCE(placement_requirement_sha256,''),
		        pricing_decision,COALESCE(pricing_decision_sha256,''),
		        currency,
		        quote_json
		   FROM quotes
		  WHERE id = $1 AND buyer_id = $2`,
		quoteID, buyerID,
	).Scan(&q.ID, &q.JobType, &q.ModelRef, &q.Tier, &q.InputSHA256, &q.CostExpUSD, &q.CostMaxUSD,
		&q.ETARawSecs, &q.Expired,
		&q.SLAGuaranteedSecs, &q.SLAPremiumUSD, &q.EconomicScheduleVersion, &planBlob, &q.EconomicExecutable,
		&q.WorkloadBindingSHA256, &q.WorkloadDecisionSHA256,
		&computeBlob, &q.ComputePlanSHA256,
		&placementBlob, &q.PlacementSHA256,
		&pricingBlob, &q.PricingDecisionSHA256,
		&q.Currency, &quoteBlob)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, errNotFound
	}
	if err != nil {
		return nil, err
	}
	if len(planBlob) > 0 {
		if err := json.Unmarshal(planBlob, &q.EconomicPlan); err != nil {
			return nil, fmt.Errorf("decoding quote economic plan: %w", err)
		}
	}
	if len(computeBlob) > 0 {
		if err := json.Unmarshal(computeBlob, &q.ComputePlan); err != nil {
			return nil, fmt.Errorf("decoding quote compute plan: %w", err)
		}
		if q.ETARawSecs <= 0 || q.ComputePlan.ETAP50Secs < q.ETARawSecs {
			return nil, errors.New("frozen quote lacks valid raw ETA authority; request a new quote")
		}
	}
	if len(placementBlob) > 0 {
		if err := json.Unmarshal(placementBlob, &q.Placement); err != nil {
			return nil, fmt.Errorf("decoding quote placement requirement: %w", err)
		}
	}
	if len(pricingBlob) > 0 {
		if err := json.Unmarshal(pricingBlob, &q.Pricing); err != nil {
			return nil, fmt.Errorf("decoding quote pricing decision: %w", err)
		}
	}
	if len(computeBlob) > 0 || q.ComputePlanSHA256 != "" {
		var snapshot Quote
		if err := json.Unmarshal(quoteBlob, &snapshot); err != nil {
			return nil, fmt.Errorf("decoding full quote authority: %w", err)
		}
		if snapshot.Currency == "" || snapshot.Currency != q.Currency ||
			snapshot.Economics.Schedule.Currency != q.Currency ||
			q.EconomicPlan.Schedule.Currency != q.Currency {
			return nil, errors.New("frozen quote currency authority mismatch")
		}
		if err := validatePlacementRequirement(snapshot.Placement, snapshot.Workload); err != nil {
			return nil, fmt.Errorf("invalid frozen quote placement authority: %w", err)
		}
		placementSHA256, err := placementRequirementDigest(q.Placement)
		if err != nil {
			return nil, fmt.Errorf("hashing frozen quote placement authority: %w", err)
		}
		snapshotPlacementSHA256, err := placementRequirementDigest(snapshot.Placement)
		if err != nil {
			return nil, fmt.Errorf("hashing full quote placement authority: %w", err)
		}
		if q.PlacementSHA256 == "" ||
			q.PlacementSHA256 != placementSHA256 ||
			q.PlacementSHA256 != snapshotPlacementSHA256 {
			return nil, errors.New("frozen quote placement requirement digest mismatch")
		}
		if err := ValidateComputePlanEconomicSnapshot(q.ComputePlan, snapshot.Workload, q.EconomicPlan); err != nil {
			return nil, fmt.Errorf("invalid frozen quote compute plan: %w", err)
		}
		computeSHA256, err := computePlanDigest(q.ComputePlan)
		if err != nil {
			return nil, fmt.Errorf("hashing frozen quote compute plan: %w", err)
		}
		snapshotSHA256, err := computePlanDigest(snapshot.ComputePlan)
		if err != nil {
			return nil, fmt.Errorf("hashing full quote compute plan: %w", err)
		}
		if q.ComputePlanSHA256 == "" || q.ComputePlanSHA256 != computeSHA256 ||
			q.ComputePlanSHA256 != snapshotSHA256 {
			return nil, errors.New("frozen quote compute plan digest mismatch")
		}
		if err := ValidateDistributedPricingDecisionSnapshotWithStore(
			ctx, s, q.Pricing, snapshot.Workload, q.ComputePlan, q.Placement, q.EconomicPlan,
		); err != nil {
			return nil, fmt.Errorf("invalid frozen quote pricing decision: %w", err)
		}
		pricingSHA256, err := pricingDecisionDigest(q.Pricing)
		if err != nil {
			return nil, fmt.Errorf("hashing frozen quote pricing decision: %w", err)
		}
		snapshotPricingSHA256, err := pricingDecisionDigest(snapshot.Pricing)
		if err != nil {
			return nil, fmt.Errorf("hashing full quote pricing decision: %w", err)
		}
		if q.PricingDecisionSHA256 == "" ||
			q.PricingDecisionSHA256 != pricingSHA256 ||
			q.PricingDecisionSHA256 != snapshotPricingSHA256 {
			return nil, errors.New("frozen quote pricing decision digest mismatch")
		}
	}
	return &q, nil
}

func validateBoundQuoteCurrency(q *boundQuote) error {
	if q == nil || q.Currency == "" {
		return errors.New("quote has no settlement currency authority")
	}
	if q.EconomicPlan.Schedule.Currency != q.Currency {
		return fmt.Errorf(
			"quote economic currency %q differs from quote currency %q",
			q.EconomicPlan.Schedule.Currency, q.Currency,
		)
	}
	if err := RequireSettlementCurrency(q.Currency); err != nil {
		return fmt.Errorf("quote settlement currency changed: %w", err)
	}
	return nil
}

func (s *Store) QuotedUSDForJob(ctx context.Context, jobID uuid.UUID) (usd float64, ok bool, err error) {
	err = s.pool.QueryRow(ctx,
		`SELECT q.cost_expected_usd
		   FROM jobs j JOIN quotes q ON q.id = j.quote_id
		  WHERE j.id = $1 AND j.quote_id IS NOT NULL`,
		jobID,
	).Scan(&usd)
	if errors.Is(err, pgx.ErrNoRows) {
		return 0, false, nil
	}
	if err != nil {
		return 0, false, err
	}
	return usd, true, nil
}
