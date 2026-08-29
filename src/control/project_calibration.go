package main

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"sort"
	"strings"
)

const (
	projectCalibrationVersion         = 1
	projectCalibrationMinSamples      = 100
	projectMedianCostErrorMaxPct      = 10.0
	projectP90CostErrorMaxPct         = 20.0
	projectP90DurationErrorMaxPct     = 20.0
	projectWithinCeilingMinPct        = 95.0
	projectCostCompleteTrueNet        = "TRUE_NET_COMPLETE"
	projectCalibrationMaxObservations = 100_000
	projectCalibrationMaxBytes        = 16 << 20
)

type ProjectCalibrationObservation struct {
	PredictedTrueNetCostNanos int64 `json:"predicted_true_net_cost_nanos"`
	ActualTrueNetCostNanos    int64 `json:"actual_true_net_cost_nanos"`
	MaximumTrueNetCostNanos   int64 `json:"maximum_true_net_cost_nanos"`
	PredictedDurationMS       int64 `json:"predicted_duration_ms"`
	ActualDurationMS          int64 `json:"actual_duration_ms"`
}

// ProjectCalibrationCohort is outcome evidence, never pricing authority. Cost
// completeness is explicit because gross platform rows, supplier payout alone,
// or known-cost contribution are not true net cost observations.
type ProjectCalibrationCohort struct {
	Version             int                             `json:"version"`
	IRSHA256            string                          `json:"ir_sha256"`
	Revision            string                          `json:"revision"`
	Currency            string                          `json:"currency"`
	ObservationClass    string                          `json:"observation_class"`
	CostCompleteness    string                          `json:"cost_completeness"`
	SourceReceiptSHA256 string                          `json:"source_receipt_sha256"`
	Observations        []ProjectCalibrationObservation `json:"observations"`
}

type ProjectCalibrationResult struct {
	Version                 int      `json:"version"`
	IRSHA256                string   `json:"ir_sha256"`
	Revision                string   `json:"revision"`
	Currency                string   `json:"currency"`
	Samples                 int      `json:"samples"`
	MedianCostErrorPct      float64  `json:"median_cost_error_pct"`
	P90CostErrorPct         float64  `json:"p90_cost_error_pct"`
	P90DurationErrorPct     float64  `json:"p90_duration_error_pct"`
	WithinCeilingPct        float64  `json:"within_ceiling_pct"`
	CostConfidence          float64  `json:"cost_confidence"`
	DurationConfidence      float64  `json:"duration_confidence"`
	PromotableForEstimation bool     `json:"promotable_for_estimation"`
	RefusalReasons          []string `json:"refusal_reasons,omitempty"`
	SourceReceiptSHA256     string   `json:"source_receipt_sha256"`
	CalibrationCohortSHA256 string   `json:"calibration_cohort_sha256"`
}

func EvaluateProjectCalibration(cohort ProjectCalibrationCohort) ProjectCalibrationResult {
	result := ProjectCalibrationResult{
		Version: projectCalibrationVersion, IRSHA256: cohort.IRSHA256,
		Revision: strings.TrimSpace(cohort.Revision), Currency: strings.ToLower(strings.TrimSpace(cohort.Currency)),
		Samples: len(cohort.Observations), SourceReceiptSHA256: cohort.SourceReceiptSHA256,
	}
	result.CalibrationCohortSHA256, _ = projectCalibrationCohortDigest(cohort)
	if cohort.Version != projectCalibrationVersion {
		result.RefusalReasons = append(result.RefusalReasons, fmt.Sprintf("unsupported cohort version %d", cohort.Version))
	}
	if !validSHA256(cohort.IRSHA256) {
		result.RefusalReasons = append(result.RefusalReasons, "IR digest is missing or malformed")
	}
	if result.Revision == "" || len(result.Revision) > 128 || strings.ContainsAny(result.Revision, "\r\n\t") {
		result.RefusalReasons = append(result.RefusalReasons, "calibration revision is missing or malformed")
	}
	if _, err := ParseCurrency(result.Currency); err != nil {
		result.RefusalReasons = append(result.RefusalReasons, "calibration currency is unsupported")
	}
	if cohort.ObservationClass != planClassPrimaryExecution {
		result.RefusalReasons = append(result.RefusalReasons, "cohort is not PRIMARY_EXECUTION outcome evidence")
	}
	if cohort.CostCompleteness != projectCostCompleteTrueNet {
		result.RefusalReasons = append(result.RefusalReasons, "cost observations are not complete true-net costs")
	}
	if !validSHA256(cohort.SourceReceiptSHA256) {
		result.RefusalReasons = append(result.RefusalReasons, "source receipt digest is missing or malformed")
	}
	if len(cohort.Observations) < projectCalibrationMinSamples {
		result.RefusalReasons = append(result.RefusalReasons, fmt.Sprintf(
			"%d samples is below the calibration floor of %d", len(cohort.Observations), projectCalibrationMinSamples))
	}
	if len(cohort.Observations) > projectCalibrationMaxObservations {
		result.RefusalReasons = append(result.RefusalReasons, fmt.Sprintf(
			"%d samples exceeds the bounded cohort maximum of %d", len(cohort.Observations), projectCalibrationMaxObservations))
	}

	costErrors := make([]float64, 0, len(cohort.Observations))
	durationErrors := make([]float64, 0, len(cohort.Observations))
	within := 0
	for i, row := range cohort.Observations {
		if row.PredictedTrueNetCostNanos <= 0 || row.ActualTrueNetCostNanos < 0 ||
			row.MaximumTrueNetCostNanos <= 0 || row.PredictedDurationMS <= 0 || row.ActualDurationMS < 0 {
			result.RefusalReasons = append(result.RefusalReasons, fmt.Sprintf("observation %d has non-positive authority", i))
			continue
		}
		costErrors = append(costErrors, absolutePercentError(row.PredictedTrueNetCostNanos, row.ActualTrueNetCostNanos))
		durationErrors = append(durationErrors, absolutePercentError(row.PredictedDurationMS, row.ActualDurationMS))
		if row.ActualTrueNetCostNanos <= row.MaximumTrueNetCostNanos {
			within++
		}
	}
	if len(costErrors) == len(cohort.Observations) && len(costErrors) > 0 {
		result.MedianCostErrorPct = percentile(costErrors, 0.5)
		result.P90CostErrorPct = percentile(costErrors, 0.9)
		result.P90DurationErrorPct = percentile(durationErrors, 0.9)
		result.WithinCeilingPct = float64(within) * 100 / float64(len(costErrors))
		result.CostConfidence = boundedConfidence(result.P90CostErrorPct)
		result.DurationConfidence = boundedConfidence(result.P90DurationErrorPct)
		if result.MedianCostErrorPct > projectMedianCostErrorMaxPct {
			result.RefusalReasons = append(result.RefusalReasons, fmt.Sprintf("median cost error %.3f%% exceeds %.1f%%", result.MedianCostErrorPct, projectMedianCostErrorMaxPct))
		}
		if result.P90CostErrorPct > projectP90CostErrorMaxPct {
			result.RefusalReasons = append(result.RefusalReasons, fmt.Sprintf("p90 cost error %.3f%% exceeds %.1f%%", result.P90CostErrorPct, projectP90CostErrorMaxPct))
		}
		if result.P90DurationErrorPct > projectP90DurationErrorMaxPct {
			result.RefusalReasons = append(result.RefusalReasons, fmt.Sprintf("p90 duration error %.3f%% exceeds %.1f%%", result.P90DurationErrorPct, projectP90DurationErrorMaxPct))
		}
		if result.WithinCeilingPct < projectWithinCeilingMinPct {
			result.RefusalReasons = append(result.RefusalReasons, fmt.Sprintf("%.3f%% within ceiling is below %.1f%%", result.WithinCeilingPct, projectWithinCeilingMinPct))
		}
	}
	result.RefusalReasons = normalizeUniqueStrings(result.RefusalReasons, strings.TrimSpace)
	result.PromotableForEstimation = len(result.RefusalReasons) == 0
	return result
}

func absolutePercentError(predicted, actual int64) float64 {
	return math.Abs(float64(actual)-float64(predicted)) / float64(predicted) * 100
}

func percentile(values []float64, quantile float64) float64 {
	if len(values) == 0 {
		return 0
	}
	ordered := append([]float64(nil), values...)
	sort.Float64s(ordered)
	position := quantile * float64(len(ordered)-1)
	lower := int(math.Floor(position))
	upper := int(math.Ceil(position))
	if lower == upper {
		return ordered[lower]
	}
	weight := position - float64(lower)
	return ordered[lower]*(1-weight) + ordered[upper]*weight
}

func boundedConfidence(p90Error float64) float64 {
	confidence := 1 - p90Error/100
	if confidence < 0 {
		return 0
	}
	if confidence > 1 {
		return 1
	}
	return confidence
}

func projectCalibrationCohortDigest(cohort ProjectCalibrationCohort) (string, error) {
	blob, err := json.Marshal(cohort)
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(blob)
	return hex.EncodeToString(sum[:]), nil
}

func decodeProjectCalibrationCohort(raw []byte) (ProjectCalibrationCohort, error) {
	var cohort ProjectCalibrationCohort
	if len(raw) == 0 {
		return cohort, errors.New("empty calibration cohort")
	}
	if err := decodeStrictJSONObject(raw, &cohort); err != nil {
		return cohort, err
	}
	return cohort, nil
}

func writeProjectCalibrationResult(w io.Writer, result ProjectCalibrationResult) error {
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	return enc.Encode(result)
}
