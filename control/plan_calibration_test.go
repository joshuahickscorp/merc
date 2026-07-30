package main

import (
	"context"
	"math"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
)

// insertPlanActualRows writes PRIMARY_EXECUTION observations directly. Going
// through RecordPlanActuals would need a whole fleet per sample; the resolver's
// job is to read buckets, and this builds buckets.
func insertPlanActualRows(
	t *testing.T, pool *pgxpool.Pool, ctx context.Context,
	class, metric string, scope PlanCalibrationScope,
	pairs [][2]float64,
) {
	t.Helper()
	for _, pair := range pairs {
		if _, err := pool.Exec(ctx,
			`INSERT INTO plan_actuals
			   (job_id, metric, observation_class, job_type, workload_class, tier,
			    model_ref, input_depth_band, runtime_id, hw_class, predicted, realized)
			 VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12)`,
			uuid.New(), metric, class, scope.JobType, scope.WorkloadClass, scope.Tier,
			scope.ModelRef, nullableBand(scope.InputDepthBand), scope.RuntimeID,
			scope.HWClass, pair[0], pair[1]); err != nil {
			t.Fatalf("insert plan_actuals: %v", err)
		}
	}
	t.Cleanup(func() {
		c, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		_, _ = pool.Exec(c, `DELETE FROM plan_actuals WHERE model_ref=$1`, scope.ModelRef)
	})
}

func nullableBand(band string) any {
	if band == "" {
		return nil
	}
	return band
}

// A distinct model_ref per test keeps buckets from bleeding across a shared
// database, so a resolver result is attributable to the rows the test wrote.
func calibrationScope(modelRef string) PlanCalibrationScope {
	return PlanCalibrationScope{
		JobType:        "batch_infer",
		WorkloadClass:  "batch_generation",
		Tier:           "batch",
		ModelRef:       modelRef,
		InputDepthBand: "medium",
		RuntimeID:      "candle_metal",
		HWClass:        "apple_silicon_ultra",
	}
}

// The point of the hierarchy: an exact bucket goes sparse the moment a new
// hardware class appears, and a two-sample bucket is noise wearing the costume
// of evidence. Resolution must widen rather than return nothing — and must say
// how far it widened.
func TestResolvePlanCalibrationWidensAndNamesTheLevelItLandedOn(t *testing.T) {
	store, pool, ctx := planActualsTestStore(t)
	modelRef := "calib-" + uuid.NewString()[:8]
	scope := calibrationScope(modelRef)

	// Nothing recorded yet: no level resolves, and the result says so rather
	// than returning a fabricated 1.0.
	cal, err := store.ResolvePlanCalibration(ctx, planMetricOutputTokens, scope)
	if err != nil {
		t.Fatalf("ResolvePlanCalibration: %v", err)
	}
	if cal.Resolved() || cal.Level != calibrationLevelNone || cal.Samples != 0 {
		t.Fatalf("empty table resolved to %+v, want an unresolved result", cal)
	}

	// Enough samples at model+depth, but on a DIFFERENT hardware class, so the
	// exact level cannot see them.
	wide := scope
	wide.HWClass = "apple_silicon_base"
	pairs := make([][2]float64, driftMinSamples)
	for i := range pairs {
		pairs[i] = [2]float64{1000, 500}
	}
	insertPlanActualRows(t, pool, ctx, planClassPrimaryExecution,
		planMetricOutputTokens, wide, pairs)

	cal, err = store.ResolvePlanCalibration(ctx, planMetricOutputTokens, scope)
	if err != nil {
		t.Fatalf("ResolvePlanCalibration: %v", err)
	}
	if cal.Level != calibrationLevelRuntimeModelDepth {
		t.Fatalf("level = %q, want %q (exact bucket is empty, runtime+model+depth is not)",
			cal.Level, calibrationLevelRuntimeModelDepth)
	}
	if cal.Samples != driftMinSamples {
		t.Fatalf("samples = %d, want %d", cal.Samples, driftMinSamples)
	}
	if math.Abs(cal.MedianRatio-0.5) > 0.000001 {
		t.Fatalf("median ratio = %v, want 0.5", cal.MedianRatio)
	}

	// Now fill the exact bucket. The narrower level must win.
	insertPlanActualRows(t, pool, ctx, planClassPrimaryExecution,
		planMetricOutputTokens, scope, pairs)
	cal, err = store.ResolvePlanCalibration(ctx, planMetricOutputTokens, scope)
	if err != nil {
		t.Fatalf("ResolvePlanCalibration: %v", err)
	}
	if cal.Level != calibrationLevelExact {
		t.Fatalf("level = %q, want %q once the exact bucket is populated",
			cal.Level, calibrationLevelExact)
	}
}

// Only PRIMARY_EXECUTION trains ordinary planning. A cache hit realized zero
// physical output by definition; a resolver that read it would conclude the
// estimator over-predicts by 100%.
func TestResolvePlanCalibrationIgnoresNonPrimaryObservations(t *testing.T) {
	store, pool, ctx := planActualsTestStore(t)
	modelRef := "calib-" + uuid.NewString()[:8]
	scope := calibrationScope(modelRef)

	pairs := make([][2]float64, driftMinSamples*3)
	for i := range pairs {
		pairs[i] = [2]float64{1000, 0}
	}
	insertPlanActualRows(t, pool, ctx, planClassCacheHit,
		planMetricOutputTokens, scope, pairs)
	insertPlanActualRows(t, pool, ctx, planClassSyntheticTest,
		planMetricOutputTokens, scope, pairs)

	cal, err := store.ResolvePlanCalibration(ctx, planMetricOutputTokens, scope)
	if err != nil {
		t.Fatalf("ResolvePlanCalibration: %v", err)
	}
	if cal.Resolved() {
		t.Fatalf("resolved to %+v from cache-hit and fixture rows alone, want unresolved", cal)
	}
}

// An unknown scope field must not silently address a narrow level as if it were
// a wildcard. Resolving "every row whose model_ref is empty" is not the same
// question as "every row".
func TestResolvePlanCalibrationSkipsLevelsItCannotAddress(t *testing.T) {
	store, pool, ctx := planActualsTestStore(t)
	modelRef := "calib-" + uuid.NewString()[:8]
	populated := calibrationScope(modelRef)
	pairs := make([][2]float64, driftMinSamples)
	for i := range pairs {
		pairs[i] = [2]float64{100, 100}
	}
	insertPlanActualRows(t, pool, ctx, planClassPrimaryExecution,
		planMetricComputeUSD, populated, pairs)

	// A caller that does not know the model cannot address model-scoped levels.
	// It must fall through to workload_class, not match the empty-model bucket.
	unknownModel := populated
	unknownModel.ModelRef = ""
	cal, err := store.ResolvePlanCalibration(ctx, planMetricComputeUSD, unknownModel)
	if err != nil {
		t.Fatalf("ResolvePlanCalibration: %v", err)
	}
	for _, forbidden := range []string{
		calibrationLevelExact, calibrationLevelRuntimeModelDepth,
		calibrationLevelModelDepth, calibrationLevelModel,
	} {
		if cal.Level == forbidden {
			t.Fatalf("level = %q for a caller with no model_ref; an empty field is not a wildcard",
				cal.Level)
		}
	}
}

func promotableCalibration() PlanCalibration {
	return PlanCalibration{
		Metric:           planMetricOutputTokens,
		Level:            calibrationLevelExact,
		Samples:          calibrationPromotionMinSamples,
		MedianRatio:      0.5,
		P90Ratio:         0.8,
		MAPE:             12,
		ObservationClass: planClassPrimaryExecution,
	}
}

func promotableRequest() CalibrationPromotionRequest {
	return CalibrationPromotionRequest{
		Calibration:            promotableCalibration(),
		Revision:               "calib-2026-07-30.1",
		ShadowComparedJobs:     calibrationPromotionMinSamples,
		ShadowMAPEImprovement:  4.5,
		PromotionReceiptSHA256: strings.Repeat("a", 64),
	}
}

// The gate exists so the argument for letting measurement drive a decision is a
// checkable structure rather than a paragraph in a pull request. Every one of
// these refusals is a way a plausible-looking calibration gets it wrong.
func TestCalibrationPromotableRefusesEveryUnsafeShape(t *testing.T) {
	if ok, refusals := CalibrationPromotable(promotableRequest()); !ok {
		t.Fatalf("a fully-evidenced promotion was refused: %v", refusals)
	}

	for _, tc := range []struct {
		name   string
		mutate func(*CalibrationPromotionRequest)
	}{
		{"money authority", func(r *CalibrationPromotionRequest) { r.AffectsMoney = true }},
		{"unresolved calibration", func(r *CalibrationPromotionRequest) {
			r.Calibration.Level = calibrationLevelNone
		}},
		{"trained on cache hits", func(r *CalibrationPromotionRequest) {
			r.Calibration.ObservationClass = planClassCacheHit
		}},
		{"below the sample floor", func(r *CalibrationPromotionRequest) {
			r.Calibration.Samples = calibrationPromotionMinSamples - 1
		}},
		{"noisy bucket", func(r *CalibrationPromotionRequest) {
			r.Calibration.MAPE = calibrationPromotionMaxMAPE + 0.1
		}},
		{"NaN MAPE", func(r *CalibrationPromotionRequest) {
			r.Calibration.MAPE = math.NaN()
		}},
		{"long right tail", func(r *CalibrationPromotionRequest) {
			r.Calibration.MedianRatio, r.Calibration.P90Ratio = 1.0, 2.5
		}},
		{"zero median", func(r *CalibrationPromotionRequest) {
			r.Calibration.MedianRatio = 0
		}},
		{"active drift alarm", func(r *CalibrationPromotionRequest) {
			r.DriftAlarmActive = true
		}},
		{"unnamed revision", func(r *CalibrationPromotionRequest) { r.Revision = "" }},
		{"thin shadow window", func(r *CalibrationPromotionRequest) {
			r.ShadowComparedJobs = calibrationPromotionMinSamples - 1
		}},
		{"shadow showed no improvement", func(r *CalibrationPromotionRequest) {
			r.ShadowMAPEImprovement = 0
		}},
		{"shadow got worse", func(r *CalibrationPromotionRequest) {
			r.ShadowMAPEImprovement = -3
		}},
		{"missing receipt", func(r *CalibrationPromotionRequest) {
			r.PromotionReceiptSHA256 = ""
		}},
		{"malformed receipt", func(r *CalibrationPromotionRequest) {
			r.PromotionReceiptSHA256 = "not-a-digest"
		}},
	} {
		req := promotableRequest()
		tc.mutate(&req)
		ok, refusals := CalibrationPromotable(req)
		if ok {
			t.Errorf("%s was promoted", tc.name)
		}
		if len(refusals) == 0 {
			t.Errorf("%s was refused without saying why", tc.name)
		}
	}
}

// Every refusal is returned, not just the first. A promotion that fails four
// checks must not be able to look like it failed one.
func TestCalibrationPromotableReportsEveryRefusal(t *testing.T) {
	req := promotableRequest()
	req.AffectsMoney = true
	req.Revision = ""
	req.DriftAlarmActive = true
	req.PromotionReceiptSHA256 = ""
	ok, refusals := CalibrationPromotable(req)
	if ok {
		t.Fatal("a request failing four checks was promoted")
	}
	if len(refusals) < 4 {
		t.Fatalf("got %d refusals for four distinct failures: %v", len(refusals), refusals)
	}
}

// The load-bearing invariant of the whole lane: measurement must not reach money.
// Comments claiming it are not enforcement — this is. If a money, pricing,
// settlement or admission file starts reading plan_actuals or a calibration, the
// build fails and someone has to argue for it in the open.
func TestCalibrationIsUnreachableFromMoneyAndAdmissionPaths(t *testing.T) {
	needles := []string{
		"plan_actuals", "PlanAccuracy", "ResolvePlanCalibration",
		"PlanCalibration", "RecordPlanActuals", "recordPlanActuals",
	}
	// The only files allowed to mention calibration at all. api.go and workers.go
	// carry the finalize hook, which writes observations and reads nothing.
	allowed := map[string]bool{
		"plan_actuals.go": true, "plan_actuals_test.go": true,
		"plan_calibration.go": true, "plan_calibration_test.go": true,
		"api.go": true, "workers.go": true,
	}
	// Files that own money, price, reserve, settlement or admission authority.
	// A hit in any of these is the failure this test exists to catch.
	guarded := []string{
		"billing.go", "buyer.go", "buyer_charge_operations.go", "collect.go",
		"economic_plan.go", "economic_facts.go", "ledger_write.go", "payment.go",
		"payment_authority.go", "prepaid.go", "pricing.go", "pricing_decision.go",
		"pricing_governance.go", "quote.go", "compute_plan.go", "scheduler.go",
		"realtime_placement.go", "shape_routing.go", "supplier_accrual.go",
		"stripe_settlement.go", "observed_output_settlement.go", "store_billing.go",
	}
	for _, name := range guarded {
		if allowed[name] {
			t.Fatalf("%s is in both the guarded and allowed lists", name)
		}
		if refs := codeReferences(t, name, needles); len(refs) != 0 {
			t.Errorf("%s references %v: calibration must not reach money, price, "+
				"reserve, settlement or admission authority", name, refs)
		}
	}

	// And nothing outside the allowlist may read it either, so a new money file
	// cannot quietly appear without this test noticing.
	entries, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range entries {
		// Test files cannot be a decision path, and excluding them keeps the
		// allowlist about production code rather than about fixtures.
		if allowed[name] || strings.HasSuffix(name, "_test.go") {
			continue
		}
		if refs := codeReferences(t, name, needles); len(refs) != 0 {
			t.Errorf("%s references %v but is not on the calibration allowlist; "+
				"add it deliberately or move the read", name, refs)
		}
	}
}
