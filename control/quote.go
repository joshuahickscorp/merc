package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
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
	EstimatedTokens  int64       `json:"estimated_tokens"` // byte/token heuristic, not exact
	MalformedRecords int         `json:"malformed_records"`
	BlankRecords     int         `json:"blank_records"`   // blank/whitespace lines (skipped, never records)
	SkippedRecords   int         `json:"skipped_records"` // blank + malformed: lines NOT usable as input (item 23)
	FirstBadLine     int         `json:"first_bad_line"`  // 1-based line of the first malformed record; 0 = none
	MaxLineBytes     int         `json:"max_line_bytes"`
	SampledRecords   int         `json:"sampled_records"` // records inspected for field names
	DetectedFields   []string    `json:"detected_fields"` // sorted union of top-level keys in the sample
	RecommendedField string      `json:"recommended_field,omitempty"`
	FieldStats       []FieldStat `json:"field_stats,omitempty"`
}

func scanJSONL(data []byte) QuoteInputScan {
	scan := QuoteInputScan{}
	fields := map[string]bool{}
	fieldStrLen := map[string]int{}
	fieldOccur := map[string]int{}
	lineNo := 0
	totalRunes, totalASCII := 0, 0
	for _, raw := range bytes.Split(data, []byte("\n")) {
		lineNo++
		ln := bytes.TrimRight(raw, "\r")
		if len(bytes.TrimSpace(ln)) == 0 {
			scan.BlankRecords++
			continue // blank line carries no record
		}
		scan.Records++
		scan.Bytes += len(ln)
		totalRunes += utf8.RuneCount(ln)
		for _, b := range ln {
			if b < 128 {
				totalASCII++
			}
		}
		if len(ln) > scan.MaxLineBytes {
			scan.MaxLineBytes = len(ln)
		}
		if !json.Valid(ln) {
			scan.MalformedRecords++
			if scan.FirstBadLine == 0 {
				scan.FirstBadLine = lineNo
			}
			continue
		}
		if scan.SampledRecords < fieldSampleN {
			var obj map[string]json.RawMessage
			if json.Unmarshal(ln, &obj) == nil {
				for k, v := range obj {
					fields[k] = true
					fieldOccur[k]++
					if s, ok := jsonStringValue(v); ok {
						fieldStrLen[k] += utf8.RuneCountInString(s)
					}
				}
			}
			scan.SampledRecords++
		}
	}
	scan.SkippedRecords = scan.BlankRecords + scan.MalformedRecords
	scan.EstimatedTokens = estimateTokensFromCounts(totalRunes, totalASCII, scan.Bytes)
	scan.DetectedFields = sortedKeys(fields)
	scan.FieldStats, scan.RecommendedField = recommendField(fields, fieldStrLen, fieldOccur)
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
	QuoteID       string          `json:"quote_id"`
	JobType       string          `json:"job_type"`
	Model         string          `json:"model"`
	Tier          string          `json:"tier"`
	TierSemantics string          `json:"tier_semantics"`
	Input         QuoteInputScan  `json:"input"`
	Execution     QuoteExecution  `json:"execution"`
	Cost          QuoteCost       `json:"cost"`
	Time          QuoteTime       `json:"time"`
	Confidence    QuoteConfidence `json:"confidence"`
	Budget        QuoteBudget     `json:"budget"`
	Warnings      []string        `json:"warnings"`
	ExpiresAt     time.Time       `json:"expires_at"`   // quote stops being bindable after this (Plane D D7)
	InputSHA256   string          `json:"input_sha256"` // sha256 of the scanned input bytes (best-effort submit match)
	SLA           *QuoteSLA       `json:"sla,omitempty"`
	Economics     EconomicPlan    `json:"economics"`

	bareID uuid.UUID // quotes.id primary key (the <uuid> inside QuoteID); not on the wire
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
	PlatformTakeUSD         float64 `json:"platform_take_usd"`
}

type QuoteTime struct {
	P50Secs       int `json:"p50_secs"`
	P90Secs       int `json:"p90_secs"`
	WorstCaseSecs int `json:"worst_case_secs"`
}

type QuoteConfidence struct {
	Score   float64  `json:"score"`
	Reasons []string `json:"reasons"`
}

type QuoteBudget struct {
	SuggestedMaxUSD       float64 `json:"suggested_max_usd"`
	CancelBeforeExceeding bool    `json:"cancel_before_exceeding"`
}

func (s *Server) quoteInitialEconomicTaskCount(ctx context.Context, sub jobSubmit, primaryTasks int) (int, error) {
	if primaryTasks <= 0 {
		return 0, nil
	}
	redundancy := fracCount(primaryTasks, sub.Verification.RedundancyFrac)
	honeypots := fracCount(primaryTasks, sub.Verification.HoneypotFrac)
	if !sub.Verification.SkipVerificationFloor &&
		sub.Verification.RedundancyFrac <= 0 && sub.Verification.HoneypotFrac <= 0 && honeypots == 0 {
		honeypots = 1
	}
	if honeypots > 0 {
		available, err := s.store.AvailableSeedHoneypots(ctx, sub.JobType.Type, sub.Model.Ref, sub.JobType.MaxTokens, honeypots)
		if err != nil {
			return 0, err
		}
		honeypots = len(available)
	}
	return primaryTasks + redundancy + honeypots, nil
}

func (s *Server) buildQuoteWithSchedule(ctx context.Context, buyerID uuid.UUID, sub jobSubmit, inputBytes []byte, schedule EconomicSchedule) Quote {
	jobType := sub.JobType.Type
	tier := sub.Tier
	scan := scanJSONL(inputBytes)

	avgLineBytes := 0.0
	if scan.Records > 0 {
		avgLineBytes = float64(len(inputBytes)) / float64(scan.Records)
	}
	split := adaptiveSplitSize(jobType, sub.Params, avgLineBytes)
	if !hasExplicitSplitSize(sub.Params) {
		split = s.adaptiveSplitSizeLive(ctx, jobType, sub.Model.Ref,
			sub.Constraints.MinMemoryGB, sub.JobType.MaxTokens, avgLineBytes, split, scan.Records)
	}
	tasks := 0
	if scan.Records > 0 && split > 0 {
		tasks = (scan.Records + split - 1) / split
	}

	expected := s.estimateJobUSD(ctx, sub.JobType.Type, sub.Model.Ref, len(inputBytes), scan.Records, sub.JobType.MaxTokens, sub.Tier)
	verifOverhead := roundUSD(expected * float64(sub.Verification.RedundancyFrac+sub.Verification.HoneypotFrac))
	wantVerificationFloor := !sub.Verification.SkipVerificationFloor &&
		sub.Verification.RedundancyFrac <= 0 && sub.Verification.HoneypotFrac <= 0
	if wantVerificationFloor && tasks > 0 && fracCount(tasks, sub.Verification.HoneypotFrac) == 0 {
		verifOverhead = roundUSD(math.Max(verifOverhead, expected/float64(tasks)))
	}
	platformTake := roundUSD((expected + verifOverhead) * platformTakeRate)

	initialEconomicTasks, economicCountErr := s.quoteInitialEconomicTaskCount(ctx, sub, tasks)
	baseComputeUSD := expected
	if tasks > 0 && initialEconomicTasks > 0 {
		baseComputeUSD = roundEconomicUSD(expected * float64(initialEconomicTasks) / float64(tasks))
		verifOverhead = roundEconomicUSD(math.Max(0, baseComputeUSD-expected))
	}

	costMin := roundUSD(expected * 0.85)
	costMax := roundUSD((expected + verifOverhead) * 1.5)

	minMem := sub.Constraints.MinMemoryGB
	var modelMinMem float32
	if m, err := s.store.GetModel(ctx, sub.Model.Ref); err == nil {
		modelMinMem = m.MinMemoryGB
		if modelMinMem > minMem {
			minMem = modelMinMem
		}
	}

	p50, conservativeSecs, plannerBacked := s.etaBandSecs(ctx, jobType, sub.Model.Ref, minMem, tasks)
	observedP90ms, _, hErr := s.store.HistoricalP90DurationMs(ctx, jobType, sub.Model.Ref)
	usedObservedHistory := hErr == nil && observedP90ms > 0
	p50 = sustainedBatchETASecs(p50, tier, usedObservedHistory)
	eta := QuoteTime{P50Secs: p50, P90Secs: p50 * 2, WorstCaseSecs: p50 * 4}

	eligibleNow, _ := s.store.EligibleWorkerCount(ctx, jobType, sub.Model.Ref, minMem)
	warmEligible, _ := s.store.WarmEligibleWorkerCount(ctx, jobType, sub.Model.Ref, minMem)

	oomRisk, coldRisk, conf, warnings := assessRisk(scan, eligibleNow, warmEligible, modelMinMem)

	poolRep, _ := s.store.EligiblePoolReputation(ctx, jobType, sub.Model.Ref, minMem)
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
		SupplierShare:    supplierShareRate,
	}
	baseEconomicPlan := BuildEconomicPlan(basePlanInput, schedule)
	if economicCountErr != nil {
		baseEconomicPlan = blockedEconomicPlan(basePlanInput, schedule, "counting initial verification work: "+economicCountErr.Error())
	}
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
	if economicCountErr != nil {
		economicPlan = blockedEconomicPlan(planInput, schedule, "counting initial verification work: "+economicCountErr.Error())
	}
	if economicPlan.Executable {
		expected = baseEconomicPlan.InitialBuyerChargeUSD
		costMax = baseEconomicPlan.ReservedBuyerChargeUSD
		costMin = baseEconomicPlan.BuyerChargePerTaskUSD
		platformTake = roundEconomicUSD(
			baseEconomicPlan.InitialBuyerChargeUSD -
				baseEconomicPlan.SupplierPayoutPerTaskUSD*float64(initialEconomicTasks),
		)
	} else {
		warnings = append(warnings, "economics blocked: "+economicPlan.BlockReason)
	}

	return Quote{
		QuoteID:       "q_" + bareID.String(),
		bareID:        bareID,
		ExpiresAt:     time.Now().Add(quoteTTL).UTC(),
		InputSHA256:   hex.EncodeToString(sum[:]),
		JobType:       jobType,
		Model:         sub.Model.Ref,
		Tier:          tier,
		TierSemantics: serviceTierSemantics(tier),
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
			VerificationOverheadUSD: verifOverhead, PlatformTakeUSD: platformTake,
		},
		Time:       eta,
		Confidence: conf,
		Budget: QuoteBudget{
			SuggestedMaxUSD:       costMax,
			CancelBeforeExceeding: true,
		},
		Warnings: warnings,
	}
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
	var sub jobSubmit
	if err := json.NewDecoder(r.Body).Decode(&sub); err != nil {
		writeErr(w, http.StatusBadRequest, "invalid quote request json: "+err.Error())
		return
	}
	if sub.JobType.Type == "" || !validJobTypes[sub.JobType.Type] {
		writeErr(w, http.StatusBadRequest, "valid job_type.type is required")
		return
	}
	if sub.Tier == "" {
		sub.Tier = "batch"
	}
	if !validTiers[sub.Tier] {
		writeErr(w, http.StatusBadRequest, "invalid tier: "+sub.Tier)
		return
	}
	canonicalModel, err := normalizeAdvertisedRuntimeModelRef(sub.JobType.Type, sub.Model)
	if err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	sub.Model = canonicalModel
	schedule, err := LoadEconomicScheduleFromEnv()
	if err != nil {
		writeErr(w, http.StatusServiceUnavailable, "economic schedule unavailable: "+err.Error())
		return
	}
	inputReader, _, err := s.resolveInput(r.Context(), auth.BuyerID, sub.Input)
	if err != nil {
		writeErr(w, http.StatusBadRequest, "resolving input: "+err.Error())
		return
	}
	inputBytes, err := readSynchronousInput(inputReader)
	if err != nil {
		status := http.StatusBadRequest
		if errors.Is(err, errSynchronousInputTooLarge) {
			status = http.StatusRequestEntityTooLarge
		}
		writeErr(w, status, "reading input: "+err.Error())
		return
	}
	q := s.buildQuoteWithSchedule(r.Context(), auth.BuyerID, sub, inputBytes, schedule)
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
	var n int
	err := s.pool.QueryRow(ctx,
		`SELECT count(*)
		   FROM workers w JOIN suppliers s ON s.id = w.supplier_id
		  WHERE w.last_seen_at IS NOT NULL
		    AND w.last_seen_at > now() - interval '60 seconds'
		    AND s.status = 'active'
		    AND NOT COALESCE(w.throttled, false)
		    AND COALESCE($3,0) <= COALESCE(w.effective_memory_gb, w.memory_gb, 0)
		    AND EXISTS (
		      SELECT 1 FROM worker_authorized_capabilities wac
		       WHERE wac.worker_id = w.id
		         AND wac.job_type = $1
		         AND wac.model_ref = $2
		         AND wac.matrix_sha256 = $4
		    )`,
		jobType, modelRef, minMemGB, generatedRuntimeMatrixSHA256,
	).Scan(&n)
	return n, err
}

func (s *Store) EligiblePoolReputation(ctx context.Context, jobType, modelRef string, minMemGB float32) (float64, error) {
	var r float64
	err := s.pool.QueryRow(ctx,
		`SELECT COALESCE(AVG(s.reputation), 0)
		   FROM workers w JOIN suppliers s ON s.id = w.supplier_id
		  WHERE w.last_seen_at IS NOT NULL
		    AND w.last_seen_at > now() - interval '60 seconds'
		    AND s.status = 'active'
		    AND NOT COALESCE(w.throttled, false)
		    AND COALESCE($3,0) <= COALESCE(w.effective_memory_gb, w.memory_gb, 0)
		    AND EXISTS (
		      SELECT 1 FROM worker_authorized_capabilities wac
		       WHERE wac.worker_id = w.id
		         AND wac.job_type = $1
		         AND wac.model_ref = $2
		         AND wac.matrix_sha256 = $4
		    )`,
		jobType, modelRef, minMemGB, generatedRuntimeMatrixSHA256,
	).Scan(&r)
	return r, err
}

func (s *Store) WarmEligibleWorkerCount(ctx context.Context, jobType, modelRef string, minMemGB float32) (int, error) {
	if modelRef == "" {
		return 0, nil
	}
	var n int
	err := s.pool.QueryRow(ctx,
		`SELECT count(*)
		   FROM workers w
		   JOIN suppliers s ON s.id = w.supplier_id
		   JOIN worker_model_state wms
		     ON wms.worker_id = w.id
		    AND wms.model_id = $2
		    AND wms.last_seen_warm > now() - interval '60 seconds'
		  WHERE w.last_seen_at IS NOT NULL
		    AND w.last_seen_at > now() - interval '60 seconds'
		    AND s.status = 'active'
		    AND NOT COALESCE(w.throttled, false)
		    AND COALESCE($3,0) <= COALESCE(w.effective_memory_gb, w.memory_gb, 0)
		    AND EXISTS (
		      SELECT 1 FROM worker_authorized_capabilities wac
		       WHERE wac.worker_id = w.id
		         AND wac.job_type = $1
		         AND wac.model_ref = $2
		         AND wac.matrix_sha256 = $4
		    )`,
		jobType, modelRef, minMemGB, generatedRuntimeMatrixSHA256,
	).Scan(&n)
	return n, err
}

func (s *Store) InsertQuote(ctx context.Context, buyerID uuid.UUID, q Quote) error {
	blob, err := json.Marshal(q)
	if err != nil {
		return err
	}
	planBlob, err := json.Marshal(q.Economics)
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
		    oom_risk, confidence, quote_json, expires_at, input_sha256,
		    sla_guaranteed_secs, sla_premium_usd,
		    economic_schedule_version, economic_plan, economic_executable)
		 VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,$15,$16,$17,$18,$19,$20,$21,$22,$23,$24,$25,$26,$27)`,
		q.bareID, buyerID, q.JobType, q.Model, q.Tier, q.Input.Records, q.Input.Bytes,
		q.Input.EstimatedTokens, q.Input.MalformedRecords, q.Execution.RecommendedSplitSize,
		q.Execution.EstimatedTasks, q.Execution.EligibleWorkersNow,
		q.Cost.ExpectedUSD, q.Cost.MinUSD, q.Cost.MaxUSD, q.Time.P50Secs, q.Time.P90Secs,
		q.Execution.OOMRisk, q.Confidence.Score, blob, q.ExpiresAt, q.InputSHA256,
		slaSecs, slaPremium,
		q.Economics.Schedule.Version, planBlob, q.Economics.Executable,
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
	InputSHA256             string
	CostExpUSD              float64
	CostMaxUSD              float64
	Expired                 bool
	SLAGuaranteedSecs       int
	SLAPremiumUSD           float64
	EconomicScheduleVersion string
	EconomicPlan            EconomicPlan
	EconomicExecutable      bool
}

func (s *Store) GetBindableQuote(ctx context.Context, quoteID, buyerID uuid.UUID) (*boundQuote, error) {
	var q boundQuote
	var planBlob []byte
	err := s.pool.QueryRow(ctx,
		`SELECT id, job_type, COALESCE(model_ref,''), COALESCE(tier,''),
		        COALESCE(input_sha256,''), COALESCE(cost_expected_usd,0),
		        COALESCE(cost_max_usd,0),
		        (expires_at IS NOT NULL AND expires_at <= now()) AS expired,
		        COALESCE(sla_guaranteed_secs,0), COALESCE(sla_premium_usd,0)::float8,
		        COALESCE(economic_schedule_version,''), economic_plan,
		        COALESCE(economic_executable,false)
		   FROM quotes
		  WHERE id = $1 AND buyer_id = $2`,
		quoteID, buyerID,
	).Scan(&q.ID, &q.JobType, &q.ModelRef, &q.Tier, &q.InputSHA256, &q.CostExpUSD, &q.CostMaxUSD, &q.Expired,
		&q.SLAGuaranteedSecs, &q.SLAPremiumUSD, &q.EconomicScheduleVersion, &planBlob, &q.EconomicExecutable)
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
	return &q, nil
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
