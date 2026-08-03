package main

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"math"
	"sort"
	"strings"
	"time"
)

// Normalized serving-engine tournament matrix.
//
// One harness, one corpus, many engines. The point is to make every serving
// engine compete for the work on identical axes so a selector has real
// candidates rather than a hardcode with a lookup table in front of it.
//
// Rules the harness ENFORCES, not suggests:
//
//  1. At least 5× as many prompts as the maximum concurrency at each level.
//     Fewer is a burst, not a steady state, and is refused as incomparable.
//  2. Same model digest across comparison arms, or the comparison is refused.
//     Same model name is not enough — same artifact hash. Different
//     quantizations of one model are different arms, never the same arm.
//  3. Refuse, don't degrade. An engine that cannot serve a matrix point is
//     refused at that point with a reason string. A refused point never counts
//     as a win or a loss.
//  4. The budget/gate is evaluated at EVERY concurrency level the run claims.
//     A gate that only checks the cheapest level and then reports
//     gate_passed: true is a defect (see scripts/gateway-parity.py
//     evaluate_budget, which still only inspects concurrency=1).
//  5. The default subset is explicit. Dropped points are machine-readable in
//     the artifact with a reason. Silent truncation is treated as coverage fraud.

const (
	servingMatrixSchemaVersion = 1
	servingMatrixHarnessID     = "merc-serving-matrix-v1"
	// steadyStatePromptMultiple is the minimum prompts-per-concurrency-level
	// ratio for a run to be steady-state rather than a burst.
	steadyStatePromptMultiple = 5
)

// Full axes. The cartesian product is enormous; default subset selection is the
// only way a run is allowed to cover less than all of it.
var (
	servingMatrixFullConcurrency = []int{1, 2, 4, 8, 16, 32, 64, 128}
	servingMatrixFullPromptTok   = []int{32, 256, 1024, 8192, 32768}
	servingMatrixFullOutputTok   = []int{16, 128, 512, 2048}
	servingMatrixFullStates      = []string{"cold", "warm", "prefix-hit"}
	servingMatrixFullLanes       = []string{"interactive", "throughput"}
	// Precisions are arm identity, not a free axis of one arm. Listed here so
	// the full space is documented; an arm that cannot serve a precision is
	// refused at points that request it.
	servingMatrixFullPrecisions = []string{"BF16", "FP8", "INT8", "AWQ", "GPTQ", "Q4_K_M"}
)

// ServingMatrixPoint is one cell of the tournament matrix.
type ServingMatrixPoint struct {
	Concurrency  int    `json:"concurrency"`
	PromptTokens int    `json:"prompt_tokens"`
	OutputTokens int    `json:"output_tokens"`
	State        string `json:"state"`
	Lane         string `json:"lane"`
	Precision    string `json:"precision"`
}

// Key is a stable identity for maps and refusal records.
func (p ServingMatrixPoint) Key() string {
	return fmt.Sprintf("c=%d/p=%d/o=%d/state=%s/lane=%s/prec=%s",
		p.Concurrency, p.PromptTokens, p.OutputTokens, p.State, p.Lane, p.Precision)
}

// DroppedMatrixPoint records a full-matrix point the run did not execute, with
// the reason it was dropped. Present so a reader never mistakes a subset for
// full coverage. Prefer DroppedAxis for wholesale axis omissions so the artifact
// does not balloon to the full cartesian product size.
type DroppedMatrixPoint struct {
	Point  ServingMatrixPoint `json:"point"`
	Reason string             `json:"reason"`
}

// DroppedAxis is a machine-readable record that an entire axis value was
// omitted from the selection (not silently truncated).
type DroppedAxis struct {
	Axis   string   `json:"axis"`
	Values []string `json:"values"`
	Reason string   `json:"reason"`
}

// ServingMatrixArm is one competing engine configuration.
//
// ModelDigest is the artifact hash, not the model name. Two arms that disagree
// on digest are incomparable even when they share a model id string.
type ServingMatrixArm struct {
	Engine           string `json:"engine"`
	RuntimeProfileID string `json:"runtime_profile_id"`
	CellID           string `json:"cell_id,omitempty"`
	ModelID          string `json:"model_id"`
	ModelDigest      string `json:"model_digest"`
	Precision        string `json:"precision"`
	Endpoint         string `json:"endpoint,omitempty"`
	// SupportedPrecisions is the closed set this arm can serve. Empty means
	// only the arm's own Precision is supported.
	SupportedPrecisions []string `json:"supported_precisions,omitempty"`
	// MaxContextTokens is the longest prompt+output this arm will accept.
	// Zero means unstated; the harness then refuses only when the engine
	// adapter itself refuses, never invents a ceiling.
	MaxContextTokens int `json:"max_context_tokens,omitempty"`
	// SupportsPrefixHit is true only when the arm has proven prefix-cache
	// behaviour. An unproven claim does not unlock prefix-hit points.
	SupportsPrefixHit bool `json:"supports_prefix_hit"`
	// ProviderUSDPerHour is the billable host rate when known. Zero with
	// ProviderCostKnown=false means the economic fields stay null rather than
	// zero-dollar.
	ProviderUSDPerHour float64 `json:"provider_usd_per_hour,omitempty"`
	ProviderCostKnown  bool    `json:"provider_cost_known"`
}

// ServingMatrixMetrics are the numbers recorded per measured cell.
//
// Fields that could not be measured on this host stay null (JSON omitempty on
// pointers) rather than zero — a zero that looks measured is a lie.
type ServingMatrixMetrics struct {
	TTFTp50Ms  *float64 `json:"ttft_p50_ms,omitempty"`
	TTFTp95Ms  *float64 `json:"ttft_p95_ms,omitempty"`
	TTFTp99Ms  *float64 `json:"ttft_p99_ms,omitempty"`
	TPOTMs     *float64 `json:"tpot_ms,omitempty"`
	ITLMs      *float64 `json:"itl_ms,omitempty"`
	ReqPerSec  *float64 `json:"req_per_sec,omitempty"`
	InTokPerS  *float64 `json:"input_tok_per_sec,omitempty"`
	OutTokPerS *float64 `json:"output_tok_per_sec,omitempty"`

	GPUUtilPct    *float64 `json:"gpu_util_pct,omitempty"`
	HBMUtilPct    *float64 `json:"hbm_util_pct,omitempty"`
	CPUUtilPct    *float64 `json:"cpu_util_pct,omitempty"`
	MemOccupancyB *int64   `json:"memory_occupancy_bytes,omitempty"`

	JoulesPerToken            *float64 `json:"joules_per_token,omitempty"`
	ProviderUSDPerHour        *float64 `json:"provider_usd_per_hour,omitempty"`
	USDPerMillionOutputTokens *float64 `json:"usd_per_million_output_tokens,omitempty"`
	USDPerVerifiedOutcome     *float64 `json:"usd_per_verified_successful_outcome,omitempty"`

	RequestsOK        int `json:"requests_ok"`
	RequestsAttempted int `json:"requests_attempted"`
	Errors            int `json:"errors"`
}

// ServingMatrixCellResult is one arm at one matrix point: either measured
// metrics or a hard refusal. Never both, and never a silent skip.
type ServingMatrixCellResult struct {
	ArmKey  string                `json:"arm_key"`
	Point   ServingMatrixPoint    `json:"point"`
	Status  string                `json:"status"` // MEASURED | REFUSED | INCOMPARABLE
	Reason  string                `json:"reason,omitempty"`
	Metrics *ServingMatrixMetrics `json:"metrics,omitempty"`
}

// ServingMatrixGateLevel is the budget evaluation at one concurrency.
type ServingMatrixGateLevel struct {
	Concurrency int      `json:"concurrency"`
	Passed      bool     `json:"passed"`
	Refusals    []string `json:"refusals"`
}

// ServingMatrixGate is the multi-level gate. GatePassed is true only when every
// claimed concurrency level passed. Checking only concurrency=1 is a defect.
type ServingMatrixGate struct {
	Version       string                   `json:"version"`
	GatePassed    bool                     `json:"gate_passed"`
	Levels        []ServingMatrixGateLevel `json:"levels"`
	Budget        ServingMatrixBudget      `json:"budget"`
	Basis         string                   `json:"basis"`
	ClaimedLevels []int                    `json:"claimed_concurrency_levels"`
}

// ServingMatrixBudget is the policy ceiling applied at every concurrency level.
type ServingMatrixBudget struct {
	// MaxTTFTp95Ms is optional. Zero means no TTFT ceiling is claimed.
	MaxTTFTp95Ms float64 `json:"max_ttft_p95_ms,omitempty"`
	// MinReqPerSec is optional. Zero means no throughput floor is claimed.
	MinReqPerSec float64 `json:"min_req_per_sec,omitempty"`
	// RequireMeasuredAtEveryLevel refuses the gate when any claimed level has
	// no MEASURED cell for the arm under test.
	RequireMeasuredAtEveryLevel bool `json:"require_measured_at_every_level"`
}

// ServingMatrixArtifact is the on-disk receipt shape under
// evidence/perf/runtime-benchmarks/.
type ServingMatrixArtifact struct {
	SchemaVersion    int    `json:"schema_version"`
	Harness          string `json:"harness"`
	MeasuredAt       string `json:"measured_at"`
	MercSourceCommit string `json:"merc_source_commit,omitempty"`

	// FullAxes documents the complete tournament space.
	FullAxes ServingMatrixAxes `json:"full_axes"`
	// SelectedPoints is exactly what this run executed (or refused per arm).
	SelectedPoints []ServingMatrixPoint `json:"selected_points"`
	// DroppedAxes is the machine-readable set of axis values omitted from the
	// full product. Prefer this over enumerating every cartesian cell.
	DroppedAxes []DroppedAxis `json:"dropped_axes"`
	// DroppedPoints holds individual points that were selected by a broader
	// subset and then further dropped (e.g. local_evidence narrowing default).
	// It is NOT the full cartesian remainder — that lives in DroppedAxes.
	DroppedPoints []DroppedMatrixPoint `json:"dropped_points"`
	// DroppedSummary is a human-readable view of the same decision.
	DroppedSummary []string `json:"dropped_summary"`
	// FullCartesianCount is len(full product) so a reader can see the scale of
	// what was not run without materialising it.
	FullCartesianCount int `json:"full_cartesian_count"`

	Arms  []ServingMatrixArm        `json:"arms"`
	Cells []ServingMatrixCellResult `json:"cells"`
	Gate  ServingMatrixGate         `json:"gate"`
	// ComparisonRefusals are arm-set level refusals (digest mismatch, etc.).
	// When non-empty the arms are incomparable and no winner may be declared.
	ComparisonRefusals []string `json:"comparison_refusals"`
	Comparable         bool     `json:"comparable"`

	BenchmarkStatus string   `json:"benchmark_status"`
	Notes           []string `json:"notes,omitempty"`
}

// ServingMatrixAxes is the documented full space.
type ServingMatrixAxes struct {
	Concurrency  []int    `json:"concurrency"`
	PromptTokens []int    `json:"prompt_tokens"`
	OutputTokens []int    `json:"output_tokens"`
	State        []string `json:"state"`
	Lane         []string `json:"lane"`
	Precision    []string `json:"precision"`
}

// FullServingMatrixAxes returns the complete documented axis set.
func FullServingMatrixAxes() ServingMatrixAxes {
	return ServingMatrixAxes{
		Concurrency:  append([]int(nil), servingMatrixFullConcurrency...),
		PromptTokens: append([]int(nil), servingMatrixFullPromptTok...),
		OutputTokens: append([]int(nil), servingMatrixFullOutputTok...),
		State:        append([]string(nil), servingMatrixFullStates...),
		Lane:         append([]string(nil), servingMatrixFullLanes...),
		Precision:    append([]string(nil), servingMatrixFullPrecisions...),
	}
}

// FullServingMatrixPoints is the full cartesian product. Expensive; used to
// compute the dropped set against a selected subset.
func FullServingMatrixPoints() []ServingMatrixPoint {
	axes := FullServingMatrixAxes()
	out := make([]ServingMatrixPoint, 0,
		len(axes.Concurrency)*len(axes.PromptTokens)*len(axes.OutputTokens)*
			len(axes.State)*len(axes.Lane)*len(axes.Precision))
	for _, c := range axes.Concurrency {
		for _, p := range axes.PromptTokens {
			for _, o := range axes.OutputTokens {
				for _, s := range axes.State {
					for _, lane := range axes.Lane {
						for _, prec := range axes.Precision {
							out = append(out, ServingMatrixPoint{
								Concurrency: c, PromptTokens: p, OutputTokens: o,
								State: s, Lane: lane, Precision: prec,
							})
						}
					}
				}
			}
		}
	}
	return out
}

// ServingMatrixSelection is the result of choosing a subset of the full axes.
type ServingMatrixSelection struct {
	Selected       []ServingMatrixPoint
	DroppedPoints  []DroppedMatrixPoint
	DroppedAxes    []DroppedAxis
	DroppedSummary []string
}

// DefaultServingMatrixSelection is the subset the harness runs when the caller
// does not ask for the full product. Every omission is explained via DroppedAxes
// (wholesale axis values) rather than enumerating the full cartesian remainder.
//
// Rationale for the defaults (also emitted into DroppedSummary):
//
//   - concurrency {1, 8, 32}: covers interactive (1), moderate concurrency (8)
//     and a throughput stress (32). Extremes 64/128 and the intermediate
//     steps 2/4/16 are wall-time and machine-size limited on a single host.
//   - prompt {32, 256}: short and medium. 1K/8K/32K need large KV and are
//     reserved for an explicit long-context profile.
//   - output {16, 128}: latency-shaped and moderate decode. 512/2K are long
//     decode and reserved for an explicit decode-throughput profile.
//   - state {cold, warm, prefix-hit}: prefix-hit is selected so the per-arm
//     refusal path fires when SupportsPrefixHit is false.
//   - lane {interactive, throughput}: both are selected.
//   - precision: arm identity — other precisions are axis-dropped.
func DefaultServingMatrixSelection(armPrecision string) ServingMatrixSelection {
	if armPrecision == "" {
		armPrecision = "UNSPECIFIED"
	}
	keepC := []int{1, 8, 32}
	keepP := []int{32, 256}
	keepO := []int{16, 128}
	keepS := []string{"cold", "warm", "prefix-hit"}
	keepLane := []string{"interactive", "throughput"}

	sel := ServingMatrixSelection{
		DroppedSummary: []string{
			"default_subset: concurrency kept {1,8,32}; dropped {2,4,16,64,128} (wall-time / single-host scale)",
			"default_subset: prompt_tokens kept {32,256}; dropped {1024,8192,32768} (long-context profile only)",
			"default_subset: output_tokens kept {16,128}; dropped {512,2048} (long-decode profile only)",
			"default_subset: state kept {cold,warm,prefix-hit}; prefix-hit is refused per-arm without proven prefix cache",
			"default_subset: precision is arm identity (" + armPrecision + "), not cartesian-expanded",
			"default_subset: both lanes {interactive,throughput} kept",
			"silent truncation is forbidden: dropped_axes enumerates every omitted full-axis value",
		},
		DroppedAxes: []DroppedAxis{
			{Axis: "concurrency", Values: strInts([]int{2, 4, 16, 64, 128}), Reason: "outside default {1,8,32}; wall-time / single-host scale"},
			{Axis: "prompt_tokens", Values: strInts([]int{1024, 8192, 32768}), Reason: "outside default {32,256}; long-context profile only"},
			{Axis: "output_tokens", Values: strInts([]int{512, 2048}), Reason: "outside default {16,128}; long-decode profile only"},
			{Axis: "precision", Values: otherPrecisions(armPrecision), Reason: "precision is arm identity; arm declares " + armPrecision},
		},
	}
	for _, c := range keepC {
		for _, p := range keepP {
			for _, o := range keepO {
				for _, s := range keepS {
					for _, lane := range keepLane {
						sel.Selected = append(sel.Selected, ServingMatrixPoint{
							Concurrency: c, PromptTokens: p, OutputTokens: o,
							State: s, Lane: lane, Precision: armPrecision,
						})
					}
				}
			}
		}
	}
	return sel
}

func strInts(xs []int) []string {
	out := make([]string, len(xs))
	for i, x := range xs {
		out[i] = fmt.Sprintf("%d", x)
	}
	return out
}

func otherPrecisions(keep string) []string {
	var out []string
	for _, p := range servingMatrixFullPrecisions {
		if !strings.EqualFold(p, keep) {
			out = append(out, p)
		}
	}
	return out
}

// RequiredPromptCount is the steady-state floor: 5× concurrency.
func RequiredPromptCount(concurrency int) int {
	if concurrency < 1 {
		concurrency = 1
	}
	return concurrency * steadyStatePromptMultiple
}

// RefuseSubSteadyState returns a non-empty reason when promptCount is below the
// 5× concurrency floor. Callers must treat a non-empty return as incomparable.
func RefuseSubSteadyState(concurrency, promptCount int) string {
	need := RequiredPromptCount(concurrency)
	if promptCount < need {
		return fmt.Sprintf(
			"incomparable: %d prompts at concurrency %d is a burst, not steady state; need at least %d (5× concurrency)",
			promptCount, concurrency, need)
	}
	return ""
}

// RefuseMismatchedModelDigests returns reasons when arms cannot be compared.
//
// Same model name is irrelevant. Different quantizations are different arms
// and must not be collapsed into one comparison.
func RefuseMismatchedModelDigests(arms []ServingMatrixArm) []string {
	var refusals []string
	if len(arms) < 2 {
		return nil // a single arm is not a comparison; no digest rule fires
	}
	for i, arm := range arms {
		if !isSHA256Hex(arm.ModelDigest) {
			refusals = append(refusals, fmt.Sprintf(
				"arm %d (%s): model_digest %q is not a sha256 hex digest; comparison refused",
				i, arm.Engine, arm.ModelDigest))
		}
	}
	if len(refusals) > 0 {
		return refusals
	}
	canonical := arms[0].ModelDigest
	for i := 1; i < len(arms); i++ {
		if !strings.EqualFold(arms[i].ModelDigest, canonical) {
			refusals = append(refusals, fmt.Sprintf(
				"model digest mismatch: arm %s has %s, arm %s has %s; same artifact hash required, not the same model name",
				arms[0].Engine, canonical, arms[i].Engine, arms[i].ModelDigest))
		}
	}
	return refusals
}

func isSHA256Hex(s string) bool {
	if len(s) != 64 {
		return false
	}
	_, err := hex.DecodeString(s)
	return err == nil
}

// RefuseMatrixPoint returns a reason when this arm cannot serve this point.
// Empty means the point may be measured. Never silently skip.
func RefuseMatrixPoint(arm ServingMatrixArm, point ServingMatrixPoint) string {
	// Precision: arm identity. A point whose precision is not in the arm's
	// supported set (defaulting to the arm's own precision) is refused.
	supported := arm.SupportedPrecisions
	if len(supported) == 0 && arm.Precision != "" {
		supported = []string{arm.Precision}
	}
	if point.Precision != "" && len(supported) > 0 {
		ok := false
		for _, p := range supported {
			if strings.EqualFold(p, point.Precision) {
				ok = true
				break
			}
		}
		if !ok {
			return fmt.Sprintf(
				"refused: engine %s does not serve precision %s (supports %v)",
				arm.Engine, point.Precision, supported)
		}
	}
	if point.State == "prefix-hit" && !arm.SupportsPrefixHit {
		return fmt.Sprintf(
			"refused: engine %s has no proven prefix-hit path (SupportsPrefixHit=false); unproven capability claims do not unlock this point",
			arm.Engine)
	}
	if arm.MaxContextTokens > 0 {
		need := point.PromptTokens + point.OutputTokens
		if need > arm.MaxContextTokens {
			return fmt.Sprintf(
				"refused: engine %s max context %d tokens; point needs prompt %d + output %d = %d",
				arm.Engine, arm.MaxContextTokens, point.PromptTokens, point.OutputTokens, need)
		}
	}
	// Lane/concurrency sanity: interactive lane at concurrency 128 is allowed
	// to run (it is a valid stress), so no refusal here — the axes are free.
	return ""
}

// ArmKey identifies an arm in cell results.
func ArmKey(arm ServingMatrixArm) string {
	return fmt.Sprintf("%s|%s|%s", arm.Engine, arm.Precision, shortDigest(arm.ModelDigest))
}

func shortDigest(d string) string {
	if len(d) < 12 {
		return d
	}
	return d[:12]
}

// EvaluateServingMatrixGate checks the budget at every claimed concurrency
// level. A gate that only inspected the cheapest level would be a defect; this
// one iterates claimedLevels and refuses the overall gate if any level fails
// or is missing.
func EvaluateServingMatrixGate(
	claimedLevels []int,
	cells []ServingMatrixCellResult,
	armKey string,
	budget ServingMatrixBudget,
) ServingMatrixGate {
	gate := ServingMatrixGate{
		Version:       "serving-matrix-gate-v1",
		Budget:        budget,
		ClaimedLevels: append([]int(nil), claimedLevels...),
		Basis: "budget evaluated independently at every claimed concurrency level; " +
			"a pass at concurrency=1 alone cannot set gate_passed",
	}
	if len(claimedLevels) == 0 {
		gate.GatePassed = false
		gate.Levels = []ServingMatrixGateLevel{{
			Concurrency: 0,
			Passed:      false,
			Refusals:    []string{"no concurrency levels claimed; gate cannot pass vacuously"},
		}}
		return gate
	}

	// Index measured metrics by concurrency for this arm.
	byConc := map[int][]ServingMatrixCellResult{}
	for _, cell := range cells {
		if cell.ArmKey != armKey {
			continue
		}
		byConc[cell.Point.Concurrency] = append(byConc[cell.Point.Concurrency], cell)
	}

	allPassed := true
	for _, c := range claimedLevels {
		level := ServingMatrixGateLevel{Concurrency: c}
		group := byConc[c]
		if len(group) == 0 {
			level.Refusals = append(level.Refusals, fmt.Sprintf(
				"concurrency %d: no cells recorded for arm %s", c, armKey))
			level.Passed = false
			allPassed = false
			gate.Levels = append(gate.Levels, level)
			continue
		}
		measured := 0
		for _, cell := range group {
			switch cell.Status {
			case "MEASURED":
				measured++
				if cell.Metrics == nil {
					level.Refusals = append(level.Refusals, fmt.Sprintf(
						"concurrency %d point %s: MEASURED with nil metrics", c, cell.Point.Key()))
					continue
				}
				if budget.MaxTTFTp95Ms > 0 {
					if cell.Metrics.TTFTp95Ms == nil {
						level.Refusals = append(level.Refusals, fmt.Sprintf(
							"concurrency %d point %s: ttft_p95_ms missing under a TTFT budget",
							c, cell.Point.Key()))
					} else if *cell.Metrics.TTFTp95Ms > budget.MaxTTFTp95Ms {
						level.Refusals = append(level.Refusals, fmt.Sprintf(
							"concurrency %d point %s: ttft_p95_ms %.3f exceeds budget %.3f",
							c, cell.Point.Key(), *cell.Metrics.TTFTp95Ms, budget.MaxTTFTp95Ms))
					}
				}
				if budget.MinReqPerSec > 0 {
					if cell.Metrics.ReqPerSec == nil {
						level.Refusals = append(level.Refusals, fmt.Sprintf(
							"concurrency %d point %s: req_per_sec missing under a throughput floor",
							c, cell.Point.Key()))
					} else if *cell.Metrics.ReqPerSec < budget.MinReqPerSec {
						level.Refusals = append(level.Refusals, fmt.Sprintf(
							"concurrency %d point %s: req_per_sec %.3f below floor %.3f",
							c, cell.Point.Key(), *cell.Metrics.ReqPerSec, budget.MinReqPerSec))
					}
				}
			case "REFUSED":
				// Refused points neither win nor lose; they do not satisfy a
				// "must be measured" budget, but they also do not fail a
				// latency ceiling.
			case "INCOMPARABLE":
				level.Refusals = append(level.Refusals, fmt.Sprintf(
					"concurrency %d point %s: incomparable (%s)", c, cell.Point.Key(), cell.Reason))
			}
		}
		if budget.RequireMeasuredAtEveryLevel && measured == 0 {
			level.Refusals = append(level.Refusals, fmt.Sprintf(
				"concurrency %d: no MEASURED cell; gate requires a measurement at every claimed level", c))
		}
		level.Passed = len(level.Refusals) == 0
		if !level.Passed {
			allPassed = false
		}
		gate.Levels = append(gate.Levels, level)
	}
	gate.GatePassed = allPassed
	return gate
}

// BuildServingMatrixArtifact assembles a complete receipt from arms, a point
// selection, and per-cell results. It re-checks comparison and steady-state
// rules so a caller cannot smuggle an incomparable run into a clean artifact.
func BuildServingMatrixArtifact(
	arms []ServingMatrixArm,
	selection ServingMatrixSelection,
	cells []ServingMatrixCellResult,
	budget ServingMatrixBudget,
	now time.Time,
	notes []string,
) ServingMatrixArtifact {
	art := ServingMatrixArtifact{
		SchemaVersion:      servingMatrixSchemaVersion,
		Harness:            servingMatrixHarnessID,
		MeasuredAt:         now.UTC().Format(time.RFC3339),
		FullAxes:           FullServingMatrixAxes(),
		FullCartesianCount: len(FullServingMatrixPoints()),
		SelectedPoints:     append([]ServingMatrixPoint(nil), selection.Selected...),
		DroppedAxes:        append([]DroppedAxis(nil), selection.DroppedAxes...),
		DroppedPoints:      append([]DroppedMatrixPoint(nil), selection.DroppedPoints...),
		DroppedSummary:     append([]string(nil), selection.DroppedSummary...),
		Arms:               append([]ServingMatrixArm(nil), arms...),
		Cells:              append([]ServingMatrixCellResult(nil), cells...),
		ComparisonRefusals: RefuseMismatchedModelDigests(arms),
		Notes:              append([]string(nil), notes...),
	}
	art.Comparable = len(art.ComparisonRefusals) == 0

	// Claimed concurrency levels = unique concurrencies in selected points.
	seen := map[int]bool{}
	var levels []int
	for _, p := range selection.Selected {
		if !seen[p.Concurrency] {
			seen[p.Concurrency] = true
			levels = append(levels, p.Concurrency)
		}
	}
	sort.Ints(levels)

	// Evaluate the gate for the first arm when comparable; multi-arm gates are
	// per-arm and the artifact records the primary (first) arm's gate. A
	// comparison refusal makes the gate fail closed.
	if len(arms) == 0 {
		art.Gate = ServingMatrixGate{
			Version:    "serving-matrix-gate-v1",
			GatePassed: false,
			Basis:      "no arms",
			Levels: []ServingMatrixGateLevel{{
				Passed: false, Refusals: []string{"no arms declared"},
			}},
		}
		art.BenchmarkStatus = "REFUSED_NO_ARMS"
		return art
	}
	primary := ArmKey(arms[0])
	art.Gate = EvaluateServingMatrixGate(levels, cells, primary, budget)
	if !art.Comparable {
		art.Gate.GatePassed = false
		art.Gate.Levels = append(art.Gate.Levels, ServingMatrixGateLevel{
			Concurrency: 0,
			Passed:      false,
			Refusals:    append([]string(nil), art.ComparisonRefusals...),
		})
		art.BenchmarkStatus = "INCOMPARABLE_ARMS"
		return art
	}

	measured := 0
	refused := 0
	incomp := 0
	for _, c := range cells {
		switch c.Status {
		case "MEASURED":
			measured++
		case "REFUSED":
			refused++
		case "INCOMPARABLE":
			incomp++
		}
	}
	switch {
	case measured > 0:
		art.BenchmarkStatus = "PHYSICAL_THROUGHPUT_MEASURED"
	case refused > 0 && incomp == 0:
		art.BenchmarkStatus = "POINTS_REFUSED_NO_MEASUREMENT"
	case incomp > 0:
		art.BenchmarkStatus = "INCOMPARABLE"
	default:
		art.BenchmarkStatus = "NO_CELLS"
	}
	return art
}

// PercentileNearestRank matches scripts/runtime-parity-sweep.py / gateway-parity.
func PercentileNearestRank(xs []float64, p float64) float64 {
	if len(xs) == 0 {
		return math.NaN()
	}
	ordered := append([]float64(nil), xs...)
	sort.Float64s(ordered)
	if len(ordered) == 1 {
		return ordered[0]
	}
	idx := int(float64(len(ordered)) * p)
	if idx >= len(ordered) {
		idx = len(ordered) - 1
	}
	return ordered[idx]
}

// DeriveMetricsFromSamples builds ServingMatrixMetrics from raw request samples.
// Resource and energy fields stay nil when unmeasured.
func DeriveMetricsFromSamples(
	ttftMs, tpotMs, itlMs []float64,
	totalInputTokens, totalOutputTokens int,
	requestsOK, requestsAttempted, errors int,
	wallSeconds float64,
	arm ServingMatrixArm,
) ServingMatrixMetrics {
	m := ServingMatrixMetrics{
		RequestsOK:        requestsOK,
		RequestsAttempted: requestsAttempted,
		Errors:            errors,
	}
	if len(ttftMs) > 0 {
		p50 := PercentileNearestRank(ttftMs, 0.50)
		p95 := PercentileNearestRank(ttftMs, 0.95)
		p99 := PercentileNearestRank(ttftMs, 0.99)
		m.TTFTp50Ms, m.TTFTp95Ms, m.TTFTp99Ms = &p50, &p95, &p99
	}
	if len(tpotMs) > 0 {
		v := PercentileNearestRank(tpotMs, 0.50)
		m.TPOTMs = &v
	}
	if len(itlMs) > 0 {
		v := PercentileNearestRank(itlMs, 0.50)
		m.ITLMs = &v
	}
	if wallSeconds > 0 && requestsOK > 0 {
		rps := float64(requestsOK) / wallSeconds
		m.ReqPerSec = &rps
		if totalInputTokens > 0 {
			v := float64(totalInputTokens) / wallSeconds
			m.InTokPerS = &v
		}
		if totalOutputTokens > 0 {
			v := float64(totalOutputTokens) / wallSeconds
			m.OutTokPerS = &v
		}
	}
	if arm.ProviderCostKnown && arm.ProviderUSDPerHour > 0 {
		v := arm.ProviderUSDPerHour
		m.ProviderUSDPerHour = &v
		if m.OutTokPerS != nil && *m.OutTokPerS > 0 {
			// $/M output tokens = (USD/hour) / (tok/s * 3600) * 1e6
			perM := (arm.ProviderUSDPerHour / (*m.OutTokPerS * 3600.0)) * 1e6
			m.USDPerMillionOutputTokens = &perM
		}
		if m.ReqPerSec != nil && *m.ReqPerSec > 0 {
			// One request ≈ one verified outcome for this harness.
			perOutcome := arm.ProviderUSDPerHour / (*m.ReqPerSec * 3600.0)
			m.USDPerVerifiedOutcome = &perOutcome
		}
	}
	return m
}

// ServingMatrixArtifactDigest is a content identity over the comparable fields.
func ServingMatrixArtifactDigest(art ServingMatrixArtifact) (string, error) {
	// Exclude nothing structural; the whole receipt is the claim.
	raw, err := json.Marshal(art)
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(raw)
	return hex.EncodeToString(sum[:]), nil
}

// ClaimedConcurrencyLevels extracts sorted unique concurrencies from points.
func ClaimedConcurrencyLevels(points []ServingMatrixPoint) []int {
	seen := map[int]bool{}
	var out []int
	for _, p := range points {
		if !seen[p.Concurrency] {
			seen[p.Concurrency] = true
			out = append(out, p.Concurrency)
		}
	}
	sort.Ints(out)
	return out
}

// LocalEvidenceServingMatrixSelection is a wall-time-bounded subset for a single
// Metal/CPU host. It is strictly smaller than DefaultServingMatrixSelection;
// every additional drop relative to the default is recorded as DroppedPoints,
// and the default's DroppedAxes are inherited.
//
// Used to produce real receipts on developer hardware without silently claiming
// the default or full matrix.
func LocalEvidenceServingMatrixSelection(armPrecision string) ServingMatrixSelection {
	if armPrecision == "" {
		armPrecision = "UNSPECIFIED"
	}
	def := DefaultServingMatrixSelection(armPrecision)
	keepC := map[int]bool{1: true, 8: true}
	keepP := map[int]bool{32: true}
	keepO := map[int]bool{16: true}
	keepS := map[string]bool{"cold": true, "warm": true}
	keepLane := map[string]bool{"interactive": true}

	sel := ServingMatrixSelection{
		DroppedAxes: append([]DroppedAxis(nil), def.DroppedAxes...),
		DroppedSummary: append(append([]string(nil), def.DroppedSummary...),
			"local_evidence: further restricted concurrency to {1,8} (dropped 32 from default)",
			"local_evidence: further restricted prompt_tokens to {32}",
			"local_evidence: further restricted output_tokens to {16}",
			"local_evidence: further restricted state to {cold,warm}",
			"local_evidence: further restricted lane to {interactive}",
		),
	}
	// Axis-level record of the further restriction.
	sel.DroppedAxes = append(sel.DroppedAxes,
		DroppedAxis{Axis: "concurrency", Values: []string{"32"}, Reason: "local_evidence further drop from default"},
		DroppedAxis{Axis: "prompt_tokens", Values: []string{"256"}, Reason: "local_evidence further drop from default"},
		DroppedAxis{Axis: "output_tokens", Values: []string{"128"}, Reason: "local_evidence further drop from default"},
		DroppedAxis{Axis: "state", Values: []string{"prefix-hit"}, Reason: "local_evidence further drop from default; still refused per-arm when unproven"},
		DroppedAxis{Axis: "lane", Values: []string{"throughput"}, Reason: "local_evidence further drop from default"},
	)
	for _, p := range def.Selected {
		var reasons []string
		if !keepC[p.Concurrency] {
			reasons = append(reasons, fmt.Sprintf("local_evidence dropped concurrency %d", p.Concurrency))
		}
		if !keepP[p.PromptTokens] {
			reasons = append(reasons, fmt.Sprintf("local_evidence dropped prompt_tokens %d", p.PromptTokens))
		}
		if !keepO[p.OutputTokens] {
			reasons = append(reasons, fmt.Sprintf("local_evidence dropped output_tokens %d", p.OutputTokens))
		}
		if !keepS[p.State] {
			reasons = append(reasons, fmt.Sprintf("local_evidence dropped state %s", p.State))
		}
		if !keepLane[p.Lane] {
			reasons = append(reasons, fmt.Sprintf("local_evidence dropped lane %s", p.Lane))
		}
		if len(reasons) > 0 {
			sel.DroppedPoints = append(sel.DroppedPoints, DroppedMatrixPoint{
				Point: p, Reason: strings.Join(reasons, "; "),
			})
			continue
		}
		sel.Selected = append(sel.Selected, p)
	}
	return sel
}
