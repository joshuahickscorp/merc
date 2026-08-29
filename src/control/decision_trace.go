package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"sort"
	"strings"
	"time"

	"github.com/google/uuid"
)

// A decision trace for one accepted workload: the candidate physical
// executions actually present in current supply, a prediction per term from a
// real source or an explicit gap, the choice, and — after execution — the
// realized value beside each prediction with a signed error.
//
// This is the show-your-work artifact for choosing among live workers. It does
// not replace MarketDecision, WorkerPlacement, ComputePlan, or the claim SQL.
// Those remain their own authorities. This producer enumerates what was
// available, what could be predicted from this commit, which candidate won and
// why, and what then happened.
//
// Honesty rules:
//
//   - Candidates come from the supply snapshot the caller passed, filtered by
//     the same live-eligibility gates Match uses (liveness, throttle, memory,
//     hardware pin). They are not a hypothetical fleet.
//   - Match itself already collapses to one hardware class by reputation×TPS.
//     That is a selection, not a candidate set, so this producer does not call
//     it. Fastest raw device is not the ranking key.
//   - A term is PREDICTED only when a real source in this tree can form the
//     number. Otherwise it is UNPREDICTED with the reason, except energy,
//     which is UNKNOWN unless a per-execution energy source exists. Converting
//     sustainedWattsByHWClass × duration would be modeling energy; this file
//     refuses that.
//   - After execution, every term is paired with a realized value or UNOBSERVED.
//     A trace that only holds predictions is half the artifact.

const decisionTraceVersion = 1

const (
	decisionTraceKind            = "physical_execution_decision_trace"
	decisionTraceSelectionRule   = "best_verified_expected_outcome"
	physicalSupplyLiveness       = 60 * time.Second // same window as Match in scheduler.go
	decisionTraceTermPredicted   = "PREDICTED"
	decisionTraceTermUnpredicted = "UNPREDICTED"
	decisionTraceTermUnknown     = "UNKNOWN"
	decisionTraceRealized        = "REALIZED"
	decisionTraceUnobserved      = "UNOBSERVED"
	decisionTraceCompared        = "COMPARED"
	decisionTraceIncomparable    = "INCOMPARABLE"
)

// Closed predicted-term vocabulary. Every candidate carries every term.
const (
	termCompletionTime   = "completion_time"
	termPrice            = "price"
	termReliability      = "reliability"
	termMovementTransfer = "movement_transfer"
	termStartup          = "startup"
	termModelLoad        = "model_load"
	termMemory           = "memory"
	termVerificationCost = "verification_cost"
	termEnergy           = "energy"
	termFailureRetryCost = "failure_retry_cost"
)

var decisionTraceTermOrder = []string{
	termCompletionTime,
	termPrice,
	termReliability,
	termMovementTransfer,
	termStartup,
	termModelLoad,
	termMemory,
	termVerificationCost,
	termEnergy,
	termFailureRetryCost,
}

// AcceptedWorkload is the slice of an already-accepted job this producer
// needs. It is not a second WorkloadDecision: callers copy the frozen fields
// they already have. EstimatedWorkUnits is the same unit the worker's TPS is
// denominated in (tokens, records, …); without both, completion time cannot
// be formed.
type AcceptedWorkload struct {
	ID                        uuid.UUID
	JobType                   string
	ModelRef                  string
	MinMemoryGB               float32
	HWClasses                 []string // empty = any
	PinEngine                 string
	PinBuildHash              string
	PinBuildIdentityPolicy    string
	PinHardwareIdentity       string
	EstimatedWorkUnits        float64
	WorkUnit                  string
	VerificationOverheadUSD   float64
	VerificationOverheadKnown bool
	Now                       time.Time
}

// PhysicalExecution is one currently advertised worker. LastSeen, Throttled,
// MemoryGB and HWClass are eligibility facts; TPS, LoadMS, ask, warmth and
// terminal outcomes are prediction sources. A field left zero means the
// snapshot did not carry that measurement, not that the value is zero.
type PhysicalExecution struct {
	WorkerID            uuid.UUID
	SupplierID          uuid.UUID
	HWClass             string
	Engine              string
	BuildHash           string
	BuildIdentityPolicy string
	HardwareIdentity    string
	MemoryGB            float32
	MinPayoutUSDHr      float64
	TPS                 float32
	LoadMS              uint64
	WarmModel           bool
	WarmPrefixDepth     int
	ThermalDegraded     bool
	LastSeen            time.Time
	Throttled           bool
	TerminalAttempts    int
	TerminalFails       int
}

// PredictedTerm is one predicted quantity, or an explicit refusal to invent it.
type PredictedTerm struct {
	Term   string   `json:"term"`
	Status string   `json:"status"`
	Value  *float64 `json:"value,omitempty"`
	Unit   string   `json:"unit,omitempty"`
	Source string   `json:"source,omitempty"`
	Reason string   `json:"reason,omitempty"`
}

// RealizedTerm is what execution observed for one vocabulary term.
type RealizedTerm struct {
	Term   string   `json:"term"`
	Status string   `json:"status"`
	Value  *float64 `json:"value,omitempty"`
	Unit   string   `json:"unit,omitempty"`
	Reason string   `json:"reason,omitempty"`
}

// TermComparison is predicted versus realized for one term, with signed error
// realized − predicted when both numbers exist.
type TermComparison struct {
	Term              string        `json:"term"`
	Predicted         PredictedTerm `json:"predicted"`
	Realized          RealizedTerm  `json:"realized"`
	SignedError       *float64      `json:"signed_error,omitempty"`
	SignedErrorUnit   string        `json:"signed_error_unit,omitempty"`
	SignedErrorStatus string        `json:"signed_error_status"`
}

// CandidatePrediction is one eligible physical execution and its predictions.
type CandidatePrediction struct {
	Rank               int             `json:"rank"`
	WorkerID           uuid.UUID       `json:"worker_id"`
	SupplierID         uuid.UUID       `json:"supplier_id"`
	HWClass            string          `json:"hw_class"`
	Engine             string          `json:"engine,omitempty"`
	HardwareIdentity   string          `json:"hardware_identity,omitempty"`
	HWClassCostRank    int             `json:"hw_class_cost_rank"`
	MinPayoutUSDHr     float64         `json:"min_payout_usd_hr"`
	AdvertisedMemoryGB float32         `json:"advertised_memory_gb"`
	WarmModel          bool            `json:"warm_model"`
	WarmPrefixDepth    int             `json:"warm_prefix_depth"`
	ThermalDegraded    bool            `json:"thermal_degraded"`
	TPS                float32         `json:"tps"`
	Terms              []PredictedTerm `json:"terms"`
}

// SupplyExclusion records a supplied worker that was not a candidate, and why.
type SupplyExclusion struct {
	WorkerID uuid.UUID `json:"worker_id"`
	Reason   string    `json:"reason"`
}

// TraceChoice is the selection and the reason the winner beat the runner-up
// (or why only one candidate existed).
type TraceChoice struct {
	SelectedWorkerID     uuid.UUID `json:"selected_worker_id"`
	SelectedHWClass      string    `json:"selected_hw_class"`
	SelectedRank         int       `json:"selected_rank"`
	RunnerUpWorkerID     uuid.UUID `json:"runner_up_worker_id,omitempty"`
	RunnerUpHWClass      string    `json:"runner_up_hw_class,omitempty"`
	SelectionRule        string    `json:"selection_rule"`
	ExpectedOutcomeBasis string    `json:"expected_outcome_basis"`
	WinnerBeatRunnerUp   string    `json:"winner_beat_runner_up,omitempty"`
	SingletonReason      string    `json:"singleton_reason,omitempty"`
}

// TraceRealization is the post-execution half of the artifact.
type TraceRealization struct {
	ExecutedWorkerID uuid.UUID        `json:"executed_worker_id"`
	Terms            []TermComparison `json:"terms"`
}

// RealizedExecution is what the caller observed after the chosen worker ran.
// A nil pointer means that term was not observed; it is not a zero.
type RealizedExecution struct {
	WorkerID              uuid.UUID
	CompletionTimeSecs    *float64
	PriceUSD              *float64
	Reliability           *float64
	MovementTransferBytes *float64
	StartupSecs           *float64
	ModelLoadMS           *float64
	MemoryGB              *float64
	VerificationCostUSD   *float64
	EnergyJoules          *float64
	FailureRetryCostUSD   *float64
}

// DecisionTrace is the full artifact: candidates, predictions, choice, and
// optionally realized-versus-predicted.
type DecisionTrace struct {
	Version          int                   `json:"version"`
	Kind             string                `json:"kind"`
	WorkloadID       uuid.UUID             `json:"workload_id"`
	JobType          string                `json:"job_type,omitempty"`
	ModelRef         string                `json:"model_ref,omitempty"`
	DecidedAt        string                `json:"decided_at"`
	SelectionRule    string                `json:"selection_rule"`
	SupplyConsidered int                   `json:"supply_considered"`
	SupplyEligible   int                   `json:"supply_eligible"`
	Exclusions       []SupplyExclusion     `json:"exclusions,omitempty"`
	Candidates       []CandidatePrediction `json:"candidates"`
	Choice           TraceChoice           `json:"choice"`
	Realized         *TraceRealization     `json:"realized,omitempty"`
}

// ProduceDecisionTrace enumerates eligible physical executions from supply,
// predicts every vocabulary term, and selects on best verified expected
// outcome. Realized is left empty until RecordDecisionTraceRealized.
func ProduceDecisionTrace(workload AcceptedWorkload, supply []PhysicalExecution) (DecisionTrace, error) {
	if workload.ID == uuid.Nil {
		return DecisionTrace{}, errors.New("decision trace requires an accepted workload id")
	}
	now := workload.Now
	if now.IsZero() {
		now = time.Now().UTC()
	}

	var (
		eligible   []PhysicalExecution
		exclusions []SupplyExclusion
	)
	for _, worker := range supply {
		if reason := ineligiblePhysicalExecution(workload, worker, now); reason != "" {
			exclusions = append(exclusions, SupplyExclusion{WorkerID: worker.WorkerID, Reason: reason})
			continue
		}
		eligible = append(eligible, worker)
	}
	if len(eligible) == 0 {
		return DecisionTrace{}, fmt.Errorf("%w: %d supplied, none eligible", ErrNoSupply, len(supply))
	}

	candidates := make([]CandidatePrediction, 0, len(eligible))
	for _, worker := range eligible {
		candidates = append(candidates, predictPhysicalCandidate(workload, worker))
	}

	basis := selectVerifiedExpectedOutcome(candidates)
	sort.SliceStable(candidates, func(i, j int) bool {
		return betterVerifiedExpectedOutcome(candidates[i], candidates[j], basis)
	})
	for i := range candidates {
		candidates[i].Rank = i + 1
	}

	choice := TraceChoice{
		SelectedWorkerID:     candidates[0].WorkerID,
		SelectedHWClass:      candidates[0].HWClass,
		SelectedRank:         1,
		SelectionRule:        decisionTraceSelectionRule,
		ExpectedOutcomeBasis: basis,
	}
	if len(candidates) == 1 {
		choice.SingletonReason = singletonReason(len(supply), exclusions)
	} else {
		choice.RunnerUpWorkerID = candidates[1].WorkerID
		choice.RunnerUpHWClass = candidates[1].HWClass
		choice.WinnerBeatRunnerUp = explainWinnerBeatRunnerUp(candidates[0], candidates[1], basis)
	}

	trace := DecisionTrace{
		Version:          decisionTraceVersion,
		Kind:             decisionTraceKind,
		WorkloadID:       workload.ID,
		JobType:          workload.JobType,
		ModelRef:         workload.ModelRef,
		DecidedAt:        now.UTC().Format(time.RFC3339Nano),
		SelectionRule:    decisionTraceSelectionRule,
		SupplyConsidered: len(supply),
		SupplyEligible:   len(eligible),
		Exclusions:       exclusions,
		Candidates:       candidates,
		Choice:           choice,
	}
	if err := ValidateDecisionTrace(trace); err != nil {
		return DecisionTrace{}, err
	}
	return trace, nil
}

// RecordDecisionTraceRealized fills the post-execution half: every predicted
// term paired with the realized value (or UNOBSERVED) and a signed error.
func RecordDecisionTraceRealized(trace *DecisionTrace, got RealizedExecution) error {
	if trace == nil {
		return errors.New("decision trace is nil")
	}
	if got.WorkerID == uuid.Nil {
		return errors.New("realized execution requires a worker id")
	}
	if trace.Choice.SelectedWorkerID != got.WorkerID {
		return fmt.Errorf("realized worker %s is not the selected worker %s",
			got.WorkerID, trace.Choice.SelectedWorkerID)
	}
	winner, ok := trace.candidate(got.WorkerID)
	if !ok {
		return fmt.Errorf("selected worker %s is not in the candidate set", got.WorkerID)
	}

	realizedByTerm := map[string]struct {
		value *float64
		unit  string
	}{
		termCompletionTime:   {got.CompletionTimeSecs, "s"},
		termPrice:            {got.PriceUSD, "USD"},
		termReliability:      {got.Reliability, "delivered_fraction"},
		termMovementTransfer: {got.MovementTransferBytes, "bytes"},
		termStartup:          {got.StartupSecs, "s"},
		termModelLoad:        {got.ModelLoadMS, "ms"},
		termMemory:           {got.MemoryGB, "GB"},
		termVerificationCost: {got.VerificationCostUSD, "USD"},
		termEnergy:           {got.EnergyJoules, "J"},
		termFailureRetryCost: {got.FailureRetryCostUSD, "USD"},
	}

	comparisons := make([]TermComparison, 0, len(decisionTraceTermOrder))
	for _, name := range decisionTraceTermOrder {
		predicted := termByName(winner.Terms, name)
		obs := realizedByTerm[name]
		realized := RealizedTerm{Term: name, Status: decisionTraceUnobserved, Unit: obs.unit}
		if obs.value != nil {
			realized.Status = decisionTraceRealized
			realized.Value = obs.value
		} else {
			realized.Reason = "no observed actual for this term on the executed worker"
		}
		cmp := TermComparison{
			Term:      name,
			Predicted: predicted,
			Realized:  realized,
		}
		if predicted.Status == decisionTraceTermPredicted && predicted.Value != nil &&
			realized.Status == decisionTraceRealized && realized.Value != nil {
			errVal := *realized.Value - *predicted.Value
			unit := predicted.Unit
			if unit == "" {
				unit = realized.Unit
			}
			if unit == "USD" {
				errVal = roundUSD(errVal)
			}
			cmp.SignedError = &errVal
			cmp.SignedErrorUnit = unit
			cmp.SignedErrorStatus = decisionTraceCompared
		} else {
			cmp.SignedErrorStatus = decisionTraceIncomparable
		}
		comparisons = append(comparisons, cmp)
	}
	trace.Realized = &TraceRealization{
		ExecutedWorkerID: got.WorkerID,
		Terms:            comparisons,
	}
	return ValidateDecisionTrace(*trace)
}

// EmitDecisionTrace returns the canonical indented JSON artifact.
func EmitDecisionTrace(trace DecisionTrace) ([]byte, error) {
	if err := ValidateDecisionTrace(trace); err != nil {
		return nil, err
	}
	return json.MarshalIndent(trace, "", "  ")
}

// ValidateDecisionTrace checks the closed term set, the choice, and — when
// present — the realized-versus-predicted pairing.
func ValidateDecisionTrace(trace DecisionTrace) error {
	if trace.Version != decisionTraceVersion {
		return fmt.Errorf("decision trace has unsupported version %d", trace.Version)
	}
	if trace.Kind != decisionTraceKind {
		return fmt.Errorf("decision trace has unknown kind %q", trace.Kind)
	}
	if trace.WorkloadID == uuid.Nil {
		return errors.New("decision trace requires a workload id")
	}
	if trace.SelectionRule != decisionTraceSelectionRule ||
		trace.Choice.SelectionRule != decisionTraceSelectionRule {
		return errors.New("decision trace must select on best_verified_expected_outcome")
	}
	if len(trace.Candidates) == 0 {
		return errors.New("decision trace has no candidates")
	}
	if trace.SupplyEligible != len(trace.Candidates) {
		return fmt.Errorf("supply_eligible %d != candidate count %d",
			trace.SupplyEligible, len(trace.Candidates))
	}
	seen := map[uuid.UUID]struct{}{}
	for i, cand := range trace.Candidates {
		if cand.WorkerID == uuid.Nil {
			return fmt.Errorf("candidate %d has no worker id", i)
		}
		if _, dup := seen[cand.WorkerID]; dup {
			return fmt.Errorf("duplicate candidate worker %s", cand.WorkerID)
		}
		seen[cand.WorkerID] = struct{}{}
		if cand.Rank != i+1 {
			return fmt.Errorf("candidate %s rank %d, want %d", cand.WorkerID, cand.Rank, i+1)
		}
		if err := validatePredictedTerms(cand.Terms); err != nil {
			return fmt.Errorf("candidate %s: %w", cand.WorkerID, err)
		}
	}
	if trace.Choice.SelectedWorkerID != trace.Candidates[0].WorkerID ||
		trace.Choice.SelectedRank != 1 {
		return errors.New("choice.selected_worker_id must be the rank-1 candidate")
	}
	switch len(trace.Candidates) {
	case 1:
		if strings.TrimSpace(trace.Choice.SingletonReason) == "" {
			return errors.New("a singleton candidate set must say why only one existed")
		}
		if trace.Choice.WinnerBeatRunnerUp != "" || trace.Choice.RunnerUpWorkerID != uuid.Nil {
			return errors.New("a singleton candidate set has no runner-up")
		}
	default:
		if strings.TrimSpace(trace.Choice.WinnerBeatRunnerUp) == "" {
			return errors.New("choice must say why the winner beat the runner-up")
		}
		if trace.Choice.RunnerUpWorkerID != trace.Candidates[1].WorkerID {
			return errors.New("choice.runner_up_worker_id must be the rank-2 candidate")
		}
	}
	if trace.Realized != nil {
		if trace.Realized.ExecutedWorkerID != trace.Choice.SelectedWorkerID {
			return errors.New("realized worker must be the selected worker")
		}
		if err := validateTermComparisons(trace.Realized.Terms); err != nil {
			return err
		}
	}
	return nil
}

func validatePredictedTerms(terms []PredictedTerm) error {
	if len(terms) != len(decisionTraceTermOrder) {
		return fmt.Errorf("want %d predicted terms, got %d", len(decisionTraceTermOrder), len(terms))
	}
	for i, name := range decisionTraceTermOrder {
		term := terms[i]
		if term.Term != name {
			return fmt.Errorf("term %d is %q, want %q", i, term.Term, name)
		}
		switch term.Status {
		case decisionTraceTermPredicted:
			if term.Value == nil {
				return fmt.Errorf("term %s is PREDICTED without a value", name)
			}
			if strings.TrimSpace(term.Source) == "" {
				return fmt.Errorf("term %s is PREDICTED without a source", name)
			}
		case decisionTraceTermUnpredicted:
			if strings.TrimSpace(term.Reason) == "" {
				return fmt.Errorf("term %s is UNPREDICTED without a reason", name)
			}
			if name == termEnergy {
				return errors.New("energy must be UNKNOWN or PREDICTED, not UNPREDICTED")
			}
		case decisionTraceTermUnknown:
			if name != termEnergy {
				return fmt.Errorf("term %s may not use UNKNOWN; energy is the UNKNOWN vocabulary", name)
			}
			if strings.TrimSpace(term.Reason) == "" {
				return errors.New("energy is UNKNOWN without a reason")
			}
		default:
			return fmt.Errorf("term %s has unknown status %q", name, term.Status)
		}
	}
	return nil
}

func validateTermComparisons(terms []TermComparison) error {
	if len(terms) != len(decisionTraceTermOrder) {
		return fmt.Errorf("realized comparison count %d, want %d", len(terms), len(decisionTraceTermOrder))
	}
	for i, name := range decisionTraceTermOrder {
		cmp := terms[i]
		if cmp.Term != name {
			return fmt.Errorf("realized term %d is %q, want %q", i, cmp.Term, name)
		}
		if cmp.Predicted.Term != name || cmp.Realized.Term != name {
			return fmt.Errorf("realized pairing for %s is mislabelled", name)
		}
		switch cmp.SignedErrorStatus {
		case decisionTraceCompared:
			if cmp.SignedError == nil {
				return fmt.Errorf("term %s is COMPARED without a signed error", name)
			}
			if cmp.Predicted.Status != decisionTraceTermPredicted || cmp.Predicted.Value == nil ||
				cmp.Realized.Status != decisionTraceRealized || cmp.Realized.Value == nil {
				return fmt.Errorf("term %s is COMPARED without two numbers", name)
			}
		case decisionTraceIncomparable:
			if cmp.SignedError != nil {
				return fmt.Errorf("term %s is INCOMPARABLE but carries a signed error", name)
			}
		default:
			return fmt.Errorf("term %s has unknown signed_error_status %q", name, cmp.SignedErrorStatus)
		}
	}
	return nil
}

func ineligiblePhysicalExecution(workload AcceptedWorkload, worker PhysicalExecution, now time.Time) string {
	if worker.LastSeen.IsZero() || now.Sub(worker.LastSeen) > physicalSupplyLiveness {
		return "stale liveness (last_seen older than 60s Match window)"
	}
	if worker.Throttled {
		return "throttled (memory pressure; Match will not dispatch)"
	}
	if worker.MemoryGB < workload.MinMemoryGB {
		return fmt.Sprintf("advertised memory %.4g GB < job min_memory_gb %.4g",
			worker.MemoryGB, workload.MinMemoryGB)
	}
	if len(workload.HWClasses) > 0 && !containsStr(workload.HWClasses, worker.HWClass) {
		return fmt.Sprintf("hw_class %s is outside the job pin %s",
			worker.HWClass, strings.Join(workload.HWClasses, ","))
	}
	if workload.PinEngine != "" && worker.Engine != workload.PinEngine {
		return fmt.Sprintf("engine %s != pin %s", worker.Engine, workload.PinEngine)
	}
	if workload.PinBuildHash != "" && worker.BuildHash != workload.PinBuildHash {
		return "engine build hash does not match the job pin"
	}
	if workload.PinBuildIdentityPolicy != "" &&
		worker.BuildIdentityPolicy != workload.PinBuildIdentityPolicy {
		return "engine build identity policy does not match the job pin"
	}
	if workload.PinHardwareIdentity != "" &&
		worker.HardwareIdentity != workload.PinHardwareIdentity {
		return "hardware identity does not match the job pin"
	}
	return ""
}

func predictPhysicalCandidate(workload AcceptedWorkload, worker PhysicalExecution) CandidatePrediction {
	cand := CandidatePrediction{
		WorkerID:           worker.WorkerID,
		SupplierID:         worker.SupplierID,
		HWClass:            worker.HWClass,
		Engine:             worker.Engine,
		HardwareIdentity:   worker.HardwareIdentity,
		HWClassCostRank:    hwClassCostRank(worker.HWClass),
		MinPayoutUSDHr:     worker.MinPayoutUSDHr,
		AdvertisedMemoryGB: worker.MemoryGB,
		WarmModel:          worker.WarmModel,
		WarmPrefixDepth:    worker.WarmPrefixDepth,
		ThermalDegraded:    worker.ThermalDegraded,
		TPS:                worker.TPS,
	}
	completion := predictCompletionTime(workload, worker)
	price := predictPrice(worker, completion)
	reliability := predictReliability(worker)
	cand.Terms = []PredictedTerm{
		completion,
		price,
		reliability,
		predictMovementTransfer(),
		predictStartup(),
		predictModelLoad(worker),
		predictMemory(),
		predictVerificationCost(workload),
		predictEnergy(),
		predictFailureRetryCost(price, reliability),
	}
	return cand
}

func predictCompletionTime(workload AcceptedWorkload, worker PhysicalExecution) PredictedTerm {
	term := PredictedTerm{Term: termCompletionTime, Unit: "s"}
	if worker.TPS <= 0 {
		term.Status = decisionTraceTermUnpredicted
		term.Reason = "worker has no measured TPS for this job type; MatchWorker.TPS is the only completion-rate source this producer will use"
		return term
	}
	if workload.EstimatedWorkUnits <= 0 {
		term.Status = decisionTraceTermUnpredicted
		term.Reason = "workload has no estimated work units, so seconds cannot be formed from TPS"
		return term
	}
	secs := workload.EstimatedWorkUnits / float64(worker.TPS)
	term.Status = decisionTraceTermPredicted
	term.Value = floatPtr(secs)
	unit := workload.WorkUnit
	if unit == "" {
		unit = "work_units"
	}
	term.Source = fmt.Sprintf(
		"worker benchmark TPS (%.6g %s/s) × workload estimated_work_units (%.6g %s)",
		worker.TPS, unit, workload.EstimatedWorkUnits, unit)
	if worker.ThermalDegraded {
		term.Reason = "thermal_degraded is recorded as a fact; the 0.7 Match score penalty is not a duration model and was not applied"
	}
	return term
}

func predictPrice(worker PhysicalExecution, completion PredictedTerm) PredictedTerm {
	term := PredictedTerm{Term: termPrice, Unit: "USD"}
	if worker.MinPayoutUSDHr <= 0 {
		term.Status = decisionTraceTermUnpredicted
		term.Reason = "worker min_payout_usd_hr is missing or non-positive; no supplier ask exists to form a job price"
		return term
	}
	if completion.Status != decisionTraceTermPredicted || completion.Value == nil {
		term.Status = decisionTraceTermUnpredicted
		term.Reason = "hourly ask is known but completion_time is UNPREDICTED, so a job price cannot be formed"
		return term
	}
	hours := *completion.Value / 3600.0
	price := roundUSD(worker.MinPayoutUSDHr * hours)
	term.Status = decisionTraceTermPredicted
	term.Value = floatPtr(price)
	term.Source = fmt.Sprintf(
		"worker min_payout_usd_hr (%.6g USD/h) × predicted completion_time (%.6g s)",
		worker.MinPayoutUSDHr, *completion.Value)
	return term
}

func predictReliability(worker PhysicalExecution) PredictedTerm {
	term := PredictedTerm{Term: termReliability, Unit: "delivered_fraction"}
	if worker.TerminalAttempts < minRealtimeOutcomeSamples {
		term.Status = decisionTraceTermUnpredicted
		term.Reason = fmt.Sprintf(
			"terminal outcome sample count %d is below floor %d (minRealtimeOutcomeSamples); reputation is a Match score input, not a failure-rate measurement",
			worker.TerminalAttempts, minRealtimeOutcomeSamples)
		return term
	}
	if worker.TerminalFails < 0 || worker.TerminalFails > worker.TerminalAttempts {
		term.Status = decisionTraceTermUnpredicted
		term.Reason = "terminal_fails is outside [0, terminal_attempts]; the sample is not a rate"
		return term
	}
	delivered := worker.TerminalAttempts - worker.TerminalFails
	rate := float64(delivered) / float64(worker.TerminalAttempts)
	term.Status = decisionTraceTermPredicted
	term.Value = floatPtr(rate)
	term.Source = fmt.Sprintf(
		"observed terminal outcomes: delivered %d / attempts %d (same floor as realtimeVerifiedOutcomeCostNanos)",
		delivered, worker.TerminalAttempts)
	return term
}

func predictMovementTransfer() PredictedTerm {
	return PredictedTerm{
		Term:   termMovementTransfer,
		Status: decisionTraceTermUnpredicted,
		Reason: "no transfer-byte meter on physical supply; warm_prefix_depth is a locality signal and CostSchedule egress is buyer object-store egress, not worker input movement",
	}
}

func predictStartup() PredictedTerm {
	return PredictedTerm{
		Term:   termStartup,
		Status: decisionTraceTermUnpredicted,
		Reason: "no per-phase startup estimator exists at this commit; eta_calibration predicted_ms is NULL for startup and TaskPhase startup is an actual",
	}
}

func predictModelLoad(worker PhysicalExecution) PredictedTerm {
	term := PredictedTerm{Term: termModelLoad, Unit: "ms"}
	if worker.WarmModel {
		term.Status = decisionTraceTermPredicted
		term.Value = floatPtr(0)
		term.Source = "worker_model_state warm (model weights already resident; load is skipped)"
		return term
	}
	if worker.LoadMS > 0 {
		term.Status = decisionTraceTermPredicted
		term.Value = floatPtr(float64(worker.LoadMS))
		term.Source = "BenchResult.load_ms on the worker's advertised benchmark"
		return term
	}
	term.Status = decisionTraceTermUnpredicted
	term.Reason = "worker is cold and no BenchResult.load_ms is present"
	return term
}

func predictMemory() PredictedTerm {
	return PredictedTerm{
		Term:   termMemory,
		Status: decisionTraceTermUnpredicted,
		Reason: "no per-execution memory predictor; ComputePlan.minimum_memory_gb is an admission floor and worker MemoryGB is advertised capacity, neither is predicted RSS, and no realized-RSS source is read here",
	}
}

func predictVerificationCost(workload AcceptedWorkload) PredictedTerm {
	term := PredictedTerm{Term: termVerificationCost, Unit: "USD"}
	if !workload.VerificationOverheadKnown {
		term.Status = decisionTraceTermUnpredicted
		term.Reason = "no frozen ComputePlan.verification_overhead_usd on this workload"
		return term
	}
	if workload.VerificationOverheadUSD < 0 ||
		math.IsNaN(workload.VerificationOverheadUSD) ||
		math.IsInf(workload.VerificationOverheadUSD, 0) {
		term.Status = decisionTraceTermUnpredicted
		term.Reason = "verification_overhead_usd is not a finite non-negative amount"
		return term
	}
	term.Status = decisionTraceTermPredicted
	term.Value = floatPtr(roundUSD(workload.VerificationOverheadUSD))
	term.Source = "ComputePlan.verification_overhead_usd (frozen at accept; not worker-specific)"
	return term
}

func predictEnergy() PredictedTerm {
	return PredictedTerm{
		Term:   termEnergy,
		Status: decisionTraceTermUnknown,
		Reason: "no per-execution energy source at this commit; sustainedWattsByHWClass is a viability wattage table, not a measured joule actual, and watts × predicted duration would model energy",
	}
}

func predictFailureRetryCost(price, reliability PredictedTerm) PredictedTerm {
	term := PredictedTerm{Term: termFailureRetryCost, Unit: "USD"}
	if price.Status != decisionTraceTermPredicted || price.Value == nil {
		term.Status = decisionTraceTermUnpredicted
		term.Reason = "retry cost needs a predicted price"
		return term
	}
	if reliability.Status != decisionTraceTermPredicted || reliability.Value == nil {
		term.Status = decisionTraceTermUnpredicted
		term.Reason = "retry cost needs a measured failure rate; unmeasured reliability is not treated as zero retries"
		return term
	}
	if *reliability.Value <= 0 {
		term.Status = decisionTraceTermUnpredicted
		term.Reason = "every observed attempt failed; no verified-outcome retry cost exists (realtime ranks this last rather than treating the base ask as honest)"
		return term
	}
	if *reliability.Value > 1 {
		term.Status = decisionTraceTermUnpredicted
		term.Reason = "delivered_fraction is greater than 1; the sample is not a rate"
		return term
	}
	// Same arithmetic as realtimeVerifiedOutcomeCostNanos: expected = price ×
	// attempts/delivered = price / reliability; retry = expected − price.
	retry := roundUSD(*price.Value * (1.0 / *reliability.Value - 1.0))
	term.Status = decisionTraceTermPredicted
	term.Value = floatPtr(retry)
	term.Source = "predicted price × (attempts/delivered − 1) from measured terminal outcomes, matching realtimeVerifiedOutcomeCostNanos"
	return term
}

const (
	expectedBasisPricePlusRetry = "predicted_price_plus_measured_retry_usd"
	expectedBasisPrice          = "predicted_price_usd"
	expectedBasisCostRank       = "hw_class_cost_rank"
)

func selectVerifiedExpectedOutcome(candidates []CandidatePrediction) string {
	allPrice := true
	allRetry := true
	for _, cand := range candidates {
		if termByName(cand.Terms, termPrice).Status != decisionTraceTermPredicted {
			allPrice = false
		}
		if termByName(cand.Terms, termFailureRetryCost).Status != decisionTraceTermPredicted {
			allRetry = false
		}
	}
	switch {
	case allPrice && allRetry:
		return expectedBasisPricePlusRetry
	case allPrice:
		return expectedBasisPrice
	default:
		return expectedBasisCostRank
	}
}

func betterVerifiedExpectedOutcome(a, b CandidatePrediction, basis string) bool {
	aDead, bDead := unrankableVerifiedOutcome(a), unrankableVerifiedOutcome(b)
	if aDead != bDead {
		return !aDead
	}
	switch basis {
	case expectedBasisPricePlusRetry, expectedBasisPrice:
		ae, be := expectedOutcomeUSD(a, basis), expectedOutcomeUSD(b, basis)
		if ae != be {
			return ae < be
		}
	}
	if a.HWClassCostRank != b.HWClassCostRank {
		return a.HWClassCostRank < b.HWClassCostRank
	}
	if a.MinPayoutUSDHr != b.MinPayoutUSDHr {
		return a.MinPayoutUSDHr < b.MinPayoutUSDHr
	}
	aLoad, aLoadOK := predictedValue(a, termModelLoad)
	bLoad, bLoadOK := predictedValue(b, termModelLoad)
	if aLoadOK && bLoadOK && aLoad != bLoad {
		return aLoad < bLoad
	}
	return a.WorkerID.String() < b.WorkerID.String()
}

func unrankableVerifiedOutcome(cand CandidatePrediction) bool {
	rel := termByName(cand.Terms, termReliability)
	return rel.Status == decisionTraceTermPredicted && rel.Value != nil && *rel.Value <= 0
}

func expectedOutcomeUSD(cand CandidatePrediction, basis string) float64 {
	price := termByName(cand.Terms, termPrice)
	if price.Status != decisionTraceTermPredicted || price.Value == nil {
		return math.MaxFloat64
	}
	sum := *price.Value
	if basis == expectedBasisPricePlusRetry {
		retry := termByName(cand.Terms, termFailureRetryCost)
		if retry.Status == decisionTraceTermPredicted && retry.Value != nil {
			sum += *retry.Value
		}
	}
	ver := termByName(cand.Terms, termVerificationCost)
	if ver.Status == decisionTraceTermPredicted && ver.Value != nil {
		sum += *ver.Value
	}
	return sum
}

func explainWinnerBeatRunnerUp(winner, runner CandidatePrediction, basis string) string {
	var b strings.Builder
	fmt.Fprintf(&b, "selected %s (%s, hw_class_cost_rank %d) over runner-up %s (%s, hw_class_cost_rank %d) on %s",
		winner.WorkerID, winner.HWClass, winner.HWClassCostRank,
		runner.WorkerID, runner.HWClass, runner.HWClassCostRank, basis)

	switch basis {
	case expectedBasisPricePlusRetry, expectedBasisPrice:
		fmt.Fprintf(&b, ": verified expected outcome %.6f USD vs %.6f USD",
			expectedOutcomeUSD(winner, basis), expectedOutcomeUSD(runner, basis))
	default:
		fmt.Fprintf(&b, ": cheaper Merc marginal class/ask (price was UNPREDICTED on at least one candidate)")
	}

	for _, name := range []string{termPrice, termFailureRetryCost, termReliability, termVerificationCost, termModelLoad} {
		wt, rt := termByName(winner.Terms, name), termByName(runner.Terms, name)
		fmt.Fprintf(&b, "; %s %s", name, formatTermBrief(wt))
		if wt.Status == decisionTraceTermPredicted && rt.Status == decisionTraceTermPredicted &&
			wt.Value != nil && rt.Value != nil && *wt.Value != *rt.Value {
			fmt.Fprintf(&b, " vs %s", formatTermBrief(rt))
		} else if wt.Status != rt.Status || (wt.Value == nil && rt.Value == nil) {
			fmt.Fprintf(&b, " vs %s", formatTermBrief(rt))
		}
	}

	wTime, wOK := predictedValue(winner, termCompletionTime)
	rTime, rOK := predictedValue(runner, termCompletionTime)
	if wOK && rOK {
		if wTime > rTime {
			fmt.Fprintf(&b,
				". completion_time favored the runner-up (%.6gs vs %.6gs) and was not the selection key (best verified expected outcome, not fastest raw device)",
				wTime, rTime)
		} else {
			fmt.Fprintf(&b,
				". completion_time was %.6gs vs %.6gs and was not the selection key",
				wTime, rTime)
		}
	} else {
		b.WriteString(". completion_time was not the selection key")
	}
	return b.String()
}

func formatTermBrief(term PredictedTerm) string {
	if term.Status == decisionTraceTermPredicted && term.Value != nil {
		if term.Unit != "" {
			return fmt.Sprintf("%s %.6g %s", term.Status, *term.Value, term.Unit)
		}
		return fmt.Sprintf("%s %.6g", term.Status, *term.Value)
	}
	if term.Reason != "" {
		return term.Status + " (" + term.Reason + ")"
	}
	return term.Status
}

func singletonReason(supplied int, exclusions []SupplyExclusion) string {
	var b strings.Builder
	fmt.Fprintf(&b, "only 1 of %d supplied workers was eligible", supplied)
	if len(exclusions) == 0 {
		b.WriteString(": current supply contained a single live worker that met the job pin")
		return b.String()
	}
	b.WriteString(":")
	for _, ex := range exclusions {
		fmt.Fprintf(&b, " %s excluded (%s);", ex.WorkerID, ex.Reason)
	}
	return strings.TrimSuffix(b.String(), ";")
}

func (tr DecisionTrace) candidate(id uuid.UUID) (CandidatePrediction, bool) {
	for _, cand := range tr.Candidates {
		if cand.WorkerID == id {
			return cand, true
		}
	}
	return CandidatePrediction{}, false
}

func termByName(terms []PredictedTerm, name string) PredictedTerm {
	for _, term := range terms {
		if term.Term == name {
			return term
		}
	}
	return PredictedTerm{Term: name, Status: decisionTraceTermUnpredicted, Reason: "term missing from candidate"}
}

func predictedValue(cand CandidatePrediction, name string) (float64, bool) {
	term := termByName(cand.Terms, name)
	if term.Status != decisionTraceTermPredicted || term.Value == nil {
		return 0, false
	}
	return *term.Value, true
}

func floatPtr(v float64) *float64 { return &v }
