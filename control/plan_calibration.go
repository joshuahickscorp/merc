package main

import (
	"context"
	"fmt"
	"math"
)

// Calibration resolution and the gate that stands between measured evidence and
// any decision that consumes it.
//
// The exact bucket (job_type, tier, model_ref, depth_band, runtime_id, hw_class)
// is the right thing to measure and the wrong thing to rely on: it goes sparse
// the moment a new model, tier or hardware class appears, and a bucket with two
// samples is noise wearing the costume of evidence. So resolution walks from the
// most specific scope outward until it finds one with enough samples.
//
// It never falls back silently. Every resolution names the level it landed on
// and the sample count behind it, so a caller that acts on a global baseline
// cannot later be described as having acted on a per-hardware measurement.
//
// Nothing in the money, reserve, pricing or admission path calls any of this
// today. CalibrationPromotable exists so that when something eventually does,
// the argument for allowing it is a structure with named, checkable fields
// rather than a paragraph in a pull request.

// Calibration scope levels, most specific first. The order is the resolution
// order; changing it changes what a caller gets.
const (
	calibrationLevelExact             = "exact"
	calibrationLevelRuntimeModelDepth = "runtime_model_depth"
	calibrationLevelModelDepth        = "model_depth"
	calibrationLevelModel             = "model"
	calibrationLevelWorkloadClass     = "workload_class"
	calibrationLevelGlobal            = "global"
	// calibrationLevelNone means no level in the hierarchy had enough samples.
	// It is distinct from "global": global is a measurement over everything,
	// none is the absence of one.
	calibrationLevelNone = "none"
)

// PlanCalibrationScope is the identity a caller wants calibration for. Empty
// fields simply fail to match the narrower levels; they are not wildcards.
type PlanCalibrationScope struct {
	JobType        string
	WorkloadClass  string
	Tier           string
	ModelRef       string
	InputDepthBand string
	RuntimeID      string
	HWClass        string
}

// PlanCalibration is a resolved measurement plus the provenance needed to
// judge it. Level and Samples are not diagnostics: a caller that ignores them
// is treating a global baseline as a per-cell measurement.
type PlanCalibration struct {
	Metric      string  `json:"metric"`
	Level       string  `json:"level"`
	Samples     int     `json:"samples"`
	MedianRatio float64 `json:"median_ratio"`
	P90Ratio    float64 `json:"p90_ratio"`
	MAPE        float64 `json:"mape"`
	WindowHours float64 `json:"window_hours"`
	// ObservationClass is always PRIMARY_EXECUTION. It is recorded on the result
	// so a serialized calibration cannot be mistaken for one that trained on
	// cache hits or fixtures.
	ObservationClass string `json:"observation_class"`
}

// Resolved reports whether any level in the hierarchy produced a measurement.
func (c PlanCalibration) Resolved() bool { return c.Level != calibrationLevelNone }

// calibrationLevelPredicate is the SQL that selects rows for one level. Each
// level is a strict superset of the one before it.
func calibrationLevelPredicate(level string) (string, []any, error) {
	switch level {
	case calibrationLevelExact:
		return `job_type = $2 AND tier = $3 AND model_ref = $4
		        AND COALESCE(input_depth_band,'') = $5
		        AND runtime_id = $6 AND hw_class = $7`,
			[]any{"JobType", "Tier", "ModelRef", "InputDepthBand", "RuntimeID", "HWClass"}, nil
	case calibrationLevelRuntimeModelDepth:
		return `model_ref = $2 AND COALESCE(input_depth_band,'') = $3 AND runtime_id = $4`,
			[]any{"ModelRef", "InputDepthBand", "RuntimeID"}, nil
	case calibrationLevelModelDepth:
		return `model_ref = $2 AND COALESCE(input_depth_band,'') = $3`,
			[]any{"ModelRef", "InputDepthBand"}, nil
	case calibrationLevelModel:
		return `model_ref = $2`, []any{"ModelRef"}, nil
	case calibrationLevelWorkloadClass:
		return `workload_class = $2`, []any{"WorkloadClass"}, nil
	case calibrationLevelGlobal:
		return `true`, nil, nil
	}
	return "", nil, fmt.Errorf("unknown calibration level %q", level)
}

func (s PlanCalibrationScope) field(name string) string {
	switch name {
	case "JobType":
		return s.JobType
	case "WorkloadClass":
		return s.WorkloadClass
	case "Tier":
		return s.Tier
	case "ModelRef":
		return s.ModelRef
	case "InputDepthBand":
		return s.InputDepthBand
	case "RuntimeID":
		return s.RuntimeID
	case "HWClass":
		return s.HWClass
	}
	return ""
}

var calibrationLevels = []string{
	calibrationLevelExact,
	calibrationLevelRuntimeModelDepth,
	calibrationLevelModelDepth,
	calibrationLevelModel,
	calibrationLevelWorkloadClass,
	calibrationLevelGlobal,
}

// ResolvePlanCalibration walks the hierarchy outward and returns the first level
// with at least driftMinSamples PRIMARY_EXECUTION observations in the window.
//
// Only PRIMARY_EXECUTION is read. A cache hit realized zero physical output by
// definition and a fixture realized whatever the fixture said; training ordinary
// planning on either measures bookkeeping rather than the estimator.
//
// A level whose scope fields are empty is skipped rather than matched loosely:
// resolving "every row whose model_ref is the empty string" is not the same
// question as "every row", and conflating them is how a narrow level silently
// becomes a global one.
func (s *Store) ResolvePlanCalibration(
	ctx context.Context, metric string, scope PlanCalibrationScope,
) (PlanCalibration, error) {
	out := PlanCalibration{
		Metric:           metric,
		Level:            calibrationLevelNone,
		WindowHours:      driftWindow.Hours(),
		ObservationClass: planClassPrimaryExecution,
	}
	for _, level := range calibrationLevels {
		predicate, fields, err := calibrationLevelPredicate(level)
		if err != nil {
			return out, err
		}
		args := []any{int(driftWindow.Seconds())}
		skip := false
		for _, name := range fields {
			value := scope.field(name.(string))
			if value == "" {
				skip = true // an unknown scope field cannot address this level
				break
			}
			args = append(args, value)
		}
		if skip {
			continue
		}
		var (
			samples             int
			median, p90, mape   float64
			medianN, p90N, mapN *float64
		)
		err = s.pool.QueryRow(ctx, `
			SELECT COUNT(*),
			       percentile_cont(0.5) WITHIN GROUP (ORDER BY realized / predicted),
			       percentile_cont(0.9) WITHIN GROUP (ORDER BY realized / predicted),
			       AVG(ABS(realized - predicted) / predicted) * 100
			  FROM plan_actuals
			 WHERE observation_class = '`+planClassPrimaryExecution+`'
			   AND metric = $`+fmt.Sprint(len(args)+1)+`
			   AND predicted > 0
			   AND created_at > now() - make_interval(secs => $1)
			   AND (`+predicate+`)`,
			append(args, metric)...,
		).Scan(&samples, &medianN, &p90N, &mapN)
		if err != nil {
			return out, err
		}
		if samples < driftMinSamples || medianN == nil || p90N == nil || mapN == nil {
			continue
		}
		median, p90, mape = *medianN, *p90N, *mapN
		if math.IsNaN(median) || math.IsInf(median, 0) {
			continue
		}
		out.Level, out.Samples = level, samples
		out.MedianRatio, out.P90Ratio, out.MAPE = median, p90, mape
		return out, nil
	}
	return out, nil
}

// Promotion thresholds. These are the numbers a promotion argues against, so
// they live next to the gate rather than in a runbook that can drift from it.
const (
	// calibrationPromotionMinSamples is deliberately far above driftMinSamples.
	// Five samples are enough to display a bucket; they are not enough to change
	// where work is placed.
	calibrationPromotionMinSamples = 100
	// calibrationPromotionMaxMAPE bounds how wrong the estimator may still be.
	// A calibration derived from a bucket this noisy would move the median while
	// leaving individual predictions no better.
	calibrationPromotionMaxMAPE = 35.0
	// calibrationPromotionMaxP90Spread bounds p90/median. A ratio distribution
	// with a long right tail is not stable enough to act on even when its median
	// is perfect: the tail is exactly the case that costs money.
	calibrationPromotionMaxP90Spread = 2.0
)

// CalibrationPromotionRequest is everything required to let a calibration
// influence a decision. The process fields are struct members rather than
// documentation because a gate whose preconditions live in prose is a gate that
// gets skipped on a busy day.
type CalibrationPromotionRequest struct {
	Calibration PlanCalibration
	// Revision names this calibration so a later receipt, rollback or incident
	// can identify exactly which numbers were in force.
	Revision string
	// ShadowComparedJobs is how many jobs ran with the calibration computed and
	// recorded but NOT applied. Shadow evidence is the only evidence that
	// distinguishes a better estimator from a differently-wrong one.
	ShadowComparedJobs int
	// ShadowMAPEImprovement is measured MAPE reduction in percentage points
	// against the uncalibrated estimator over the shadow window. Zero or
	// negative means the calibration did not help.
	ShadowMAPEImprovement float64
	// PromotionReceiptSHA256 is the operator-reviewed receipt for this
	// promotion. Absent receipt, absent promotion.
	PromotionReceiptSHA256 string
	// DriftAlarmActive is the current state of the drift watchdog. Promoting
	// into an active alarm calibrates against a fleet that is already misbehaving.
	DriftAlarmActive bool
	// AffectsMoney must stay false. Placement and estimation may eventually
	// consume calibration; reserve, price and settlement authority may not, and
	// this gate refuses rather than leaving that to reviewer memory.
	AffectsMoney bool
}

// CalibrationPromotable reports whether a calibration may influence decisions,
// and why not when it may not. Every refusal is returned, not just the first:
// a promotion that fails four checks should not be able to look like it failed
// one.
func CalibrationPromotable(req CalibrationPromotionRequest) (bool, []string) {
	var refusals []string
	cal := req.Calibration

	if req.AffectsMoney {
		refusals = append(refusals,
			"calibration may not be promoted into reserve, price or settlement authority")
	}
	if !cal.Resolved() {
		refusals = append(refusals, "calibration did not resolve at any scope level")
	}
	if cal.ObservationClass != planClassPrimaryExecution {
		refusals = append(refusals, fmt.Sprintf(
			"calibration trained on %s rather than %s",
			cal.ObservationClass, planClassPrimaryExecution))
	}
	if cal.Samples < calibrationPromotionMinSamples {
		refusals = append(refusals, fmt.Sprintf(
			"%d samples is below the promotion floor of %d",
			cal.Samples, calibrationPromotionMinSamples))
	}
	if math.IsNaN(cal.MAPE) || math.IsInf(cal.MAPE, 0) || cal.MAPE > calibrationPromotionMaxMAPE {
		refusals = append(refusals, fmt.Sprintf(
			"MAPE %.1f%% exceeds the promotion bound of %.1f%%",
			cal.MAPE, calibrationPromotionMaxMAPE))
	}
	if cal.MedianRatio <= 0 || math.IsNaN(cal.MedianRatio) || math.IsInf(cal.MedianRatio, 0) {
		refusals = append(refusals, "median ratio is not a usable positive number")
	} else if spread := cal.P90Ratio / cal.MedianRatio; math.IsNaN(spread) ||
		math.IsInf(spread, 0) || spread > calibrationPromotionMaxP90Spread {
		refusals = append(refusals, fmt.Sprintf(
			"p90/median spread %.2f exceeds the stability bound of %.2f",
			spread, calibrationPromotionMaxP90Spread))
	}
	if req.DriftAlarmActive {
		refusals = append(refusals, "a drift alarm is active; the fleet is not in a calibratable state")
	}
	if req.Revision == "" {
		refusals = append(refusals, "calibration has no named revision")
	}
	if req.ShadowComparedJobs < calibrationPromotionMinSamples {
		refusals = append(refusals, fmt.Sprintf(
			"%d shadow-compared jobs is below the required %d",
			req.ShadowComparedJobs, calibrationPromotionMinSamples))
	}
	if !(req.ShadowMAPEImprovement > 0) {
		refusals = append(refusals, fmt.Sprintf(
			"shadow comparison showed no improvement (%.2f percentage points)",
			req.ShadowMAPEImprovement))
	}
	if !validSHA256(req.PromotionReceiptSHA256) {
		refusals = append(refusals, "promotion receipt digest is missing or malformed")
	}
	return len(refusals) == 0, refusals
}
