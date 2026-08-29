package main

import (
	"strings"
	"testing"
)

func calibrationCohortFixture() ProjectCalibrationCohort {
	rows := make([]ProjectCalibrationObservation, 100)
	for i := range rows {
		rows[i] = ProjectCalibrationObservation{
			PredictedTrueNetCostNanos: 1_000_000, ActualTrueNetCostNanos: 1_050_000,
			MaximumTrueNetCostNanos: 1_200_000, PredictedDurationMS: 1000, ActualDurationMS: 1100,
		}
	}
	return ProjectCalibrationCohort{
		Version: 1, IRSHA256: strings.Repeat("a", 64), Revision: "project-calibration-r1",
		Currency: "cad", ObservationClass: planClassPrimaryExecution,
		CostCompleteness: projectCostCompleteTrueNet, SourceReceiptSHA256: strings.Repeat("b", 64),
		Observations: rows,
	}
}

func TestProjectCalibrationClearsExactTargets(t *testing.T) {
	result := EvaluateProjectCalibration(calibrationCohortFixture())
	if !result.PromotableForEstimation || len(result.RefusalReasons) != 0 {
		t.Fatalf("sound calibration refused: %+v", result)
	}
	if result.MedianCostErrorPct != 5 || result.P90CostErrorPct != 5 || result.P90DurationErrorPct != 10 || result.WithinCeilingPct != 100 {
		t.Fatalf("wrong calibration metrics: %+v", result)
	}
	if len(result.CalibrationCohortSHA256) != 64 || result.CostConfidence != .95 || result.DurationConfidence != .9 {
		t.Fatalf("confidence/provenance missing: %+v", result)
	}
}

func TestProjectCalibrationRefusesTailAndCeilingFailures(t *testing.T) {
	cohort := calibrationCohortFixture()
	for i := 0; i < 11; i++ {
		cohort.Observations[i].ActualTrueNetCostNanos = 1_500_000
		cohort.Observations[i].ActualDurationMS = 1500
	}
	result := EvaluateProjectCalibration(cohort)
	joined := strings.Join(result.RefusalReasons, "\n")
	for _, want := range []string{"p90 cost error", "p90 duration error", "within ceiling"} {
		if !strings.Contains(joined, want) {
			t.Errorf("missing %q refusal: %s", want, joined)
		}
	}
	if result.PromotableForEstimation {
		t.Fatal("tail failures were promoted")
	}
}

func TestProjectCalibrationRefusesIncompleteCostAndFixtures(t *testing.T) {
	cohort := calibrationCohortFixture()
	cohort.CostCompleteness = "SUPPLIER_GROSS_ONLY"
	cohort.ObservationClass = planClassSyntheticTest
	cohort.Observations = cohort.Observations[:99]
	result := EvaluateProjectCalibration(cohort)
	joined := strings.Join(result.RefusalReasons, "\n")
	for _, want := range []string{"not complete true-net", "not PRIMARY_EXECUTION", "below the calibration floor"} {
		if !strings.Contains(joined, want) {
			t.Errorf("missing %q refusal: %s", want, joined)
		}
	}
}

func TestProjectCalibrationThresholdsHaveNoHiddenTolerance(t *testing.T) {
	cohort := calibrationCohortFixture()
	for i := range cohort.Observations {
		cohort.Observations[i].ActualTrueNetCostNanos = 1_100_001
	}
	result := EvaluateProjectCalibration(cohort)
	if result.PromotableForEstimation || result.MedianCostErrorPct <= 10 {
		t.Fatalf("cost error above exact target passed: %+v", result)
	}
}
