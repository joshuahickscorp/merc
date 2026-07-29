package main

import (
	"context"
	"math"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
)

func TestClampETABiasFactorIsOneSidedAndBounded(t *testing.T) {
	for _, tc := range []struct {
		name   string
		median float64
		want   float64
	}{
		{"faster than predicted is never used to shrink an eta", 0.4, 1},
		{"exactly on target", 1, 1},
		{"mild optimism is corrected", 1.5, 1.5},
		{"at the cap", etaBiasFactorMax, etaBiasFactorMax},
		{"a pathological window is capped", 500, etaBiasFactorMax},
		{"NaN cannot poison a quote", math.NaN(), 1},
		{"Inf cannot poison a quote", math.Inf(1), 1},
	} {
		if got := clampETABiasFactor(tc.median); got != tc.want {
			t.Errorf("%s: clampETABiasFactor(%v) = %v, want %v", tc.name, tc.median, got, tc.want)
		}
	}
}

func TestApplyETABiasNeverShortensAQuotedETA(t *testing.T) {
	for _, tc := range []struct {
		secs   int
		factor float64
		want   int
	}{
		{100, 1, 100},
		{100, 0.5, 100}, // a sub-1 factor must not shrink the promise
		{100, 1.5, 150},
		{100, 2.25, 225},
		{101, 1.005, 102}, // rounds up, never down
		{0, 2, 0},
		{100, math.NaN(), 100},
		{100, math.Inf(1), 100},
	} {
		if got := applyETABias(tc.secs, tc.factor); got != tc.want {
			t.Errorf("applyETABias(%d, %v) = %d, want %d", tc.secs, tc.factor, got, tc.want)
		}
	}
}

func TestComputePlanETASourceRanksCalibratedHighest(t *testing.T) {
	for _, tc := range []struct {
		planner, history, calibrated bool
		want                         string
	}{
		{false, false, false, "static"},
		{true, false, false, "planner"},
		{true, true, false, "historical"},
		{false, false, true, "calibrated"},
		{true, true, true, "calibrated"},
	} {
		if got := computePlanETASource(tc.planner, tc.history, tc.calibrated); got != tc.want {
			t.Errorf("computePlanETASource(%v,%v,%v) = %q, want %q",
				tc.planner, tc.history, tc.calibrated, got, tc.want)
		}
	}
}

func etaCalibrationTestStore(t *testing.T) (*Store, *pgxpool.Pool, context.Context) {
	t.Helper()
	databaseURL := requireTestDatabase(t)
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	t.Cleanup(cancel)
	pool, err := pgxpool.New(ctx, databaseURL)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	t.Cleanup(pool.Close)
	store := NewStore(pool)
	if err := store.Migrate(ctx); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	return store, pool, ctx
}

// insertETACalibrationRows writes calibration history for a job type that no
// other test uses, so the read is not contaminated by a shared database.
func insertETACalibrationRows(t *testing.T, pool *pgxpool.Pool, ctx context.Context,
	jobType, tier, modelRef string, pairs [][2]int) {
	t.Helper()
	for _, p := range pairs {
		if _, err := pool.Exec(ctx,
			`INSERT INTO eta_calibration
			   (job_id, job_type, tier, model_ref, predicted_secs, realized_secs)
			 VALUES ($1,$2,$3,$4,$5,$6)`,
			uuid.New(), jobType, tier, modelRef, p[0], p[1]); err != nil {
			t.Fatalf("insert eta_calibration: %v", err)
		}
	}
	t.Cleanup(func() {
		c, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		_, _ = pool.Exec(c, `DELETE FROM eta_calibration WHERE job_type=$1`, jobType)
	})
}

// TestETABiasFactorClosesTheCalibrationLoop is the regression that matters:
// recordEtaCalibration has always written predicted-versus-realized rows, and
// until this read existed nothing consumed them. A quote repeated the same
// optimistic estimate forever no matter how far realized time drifted.
func TestETABiasFactorClosesTheCalibrationLoop(t *testing.T) {
	store, pool, ctx := etaCalibrationTestStore(t)
	jobType := "etacal_" + uuid.NewString()[:8]
	modelRef := "eta-loop-model"

	// Below the sample floor there is no evidence, so the quote is untouched.
	insertETACalibrationRows(t, pool, ctx, jobType, "batch", modelRef, [][2]int{
		{100, 200}, {100, 200},
	})
	factor, samples, err := store.ETABiasFactor(ctx, jobType, "batch", modelRef)
	if err != nil {
		t.Fatalf("ETABiasFactor: %v", err)
	}
	if samples != 2 {
		t.Fatalf("samples = %d, want 2", samples)
	}
	if factor != 1 {
		t.Fatalf("factor = %v with only %d samples, want exactly 1 (no evidence, no correction)",
			factor, samples)
	}

	// Past the floor, a consistent 2x overrun is corrected.
	insertETACalibrationRows(t, pool, ctx, jobType, "batch", modelRef, [][2]int{
		{100, 200}, {100, 200}, {100, 200}, {50, 100},
	})
	factor, samples, err = store.ETABiasFactor(ctx, jobType, "batch", modelRef)
	if err != nil {
		t.Fatalf("ETABiasFactor: %v", err)
	}
	if samples < driftMinSamples {
		t.Fatalf("samples = %d, want at least %d", samples, driftMinSamples)
	}
	if math.Abs(factor-2) > 0.001 {
		t.Fatalf("factor = %v, want 2 (median realized/predicted)", factor)
	}
	if got, want := applyETABias(600, factor), 1200; got != want {
		t.Fatalf("a 600s quote under a 2x measured bias = %d, want %d", got, want)
	}

	// Tier is part of the key: priority history must not correct a batch quote.
	other, otherSamples, err := store.ETABiasFactor(ctx, jobType, "priority", modelRef)
	if err != nil {
		t.Fatalf("ETABiasFactor(priority): %v", err)
	}
	if otherSamples != 0 || other != 1 {
		t.Fatalf("priority tier read batch history: factor=%v samples=%d", other, otherSamples)
	}
}

// A fleet that consistently beats its own estimate must not be able to talk the
// quote into a shorter ETA, because deriveQuoteSLA turns that same number into a
// refundable promise.
func TestETABiasFactorNeverShrinksAnETA(t *testing.T) {
	store, pool, ctx := etaCalibrationTestStore(t)
	jobType := "etacal_" + uuid.NewString()[:8]
	modelRef := "eta-fast-model"
	insertETACalibrationRows(t, pool, ctx, jobType, "batch", modelRef, [][2]int{
		{100, 10}, {100, 10}, {100, 10}, {100, 10}, {100, 10}, {100, 10},
	})
	factor, samples, err := store.ETABiasFactor(ctx, jobType, "batch", modelRef)
	if err != nil {
		t.Fatalf("ETABiasFactor: %v", err)
	}
	if samples < driftMinSamples {
		t.Fatalf("samples = %d, want at least %d", samples, driftMinSamples)
	}
	if factor != 1 {
		t.Fatalf("factor = %v for a fleet 10x faster than predicted, want exactly 1", factor)
	}
}

// A single queued-for-a-day outlier must not be able to quote an absurd ETA.
func TestETABiasFactorIsCappedAgainstAPathologicalWindow(t *testing.T) {
	store, pool, ctx := etaCalibrationTestStore(t)
	jobType := "etacal_" + uuid.NewString()[:8]
	modelRef := "eta-pathological-model"
	insertETACalibrationRows(t, pool, ctx, jobType, "batch", modelRef, [][2]int{
		{1, 86400}, {1, 86400}, {1, 86400}, {1, 86400}, {1, 86400},
	})
	factor, _, err := store.ETABiasFactor(ctx, jobType, "batch", modelRef)
	if err != nil {
		t.Fatalf("ETABiasFactor: %v", err)
	}
	if factor != etaBiasFactorMax {
		t.Fatalf("factor = %v, want the cap %v", factor, etaBiasFactorMax)
	}
}

func TestETABiasFactorIsModelScopedWithoutLegacyBleed(t *testing.T) {
	store, pool, ctx := etaCalibrationTestStore(t)
	jobType := "etamodelscope_" + uuid.NewString()[:8]
	modelA, modelB := "slow-model", "fast-model"
	buyerID := uuid.New()
	if _, err := pool.Exec(ctx,
		`INSERT INTO buyers (id,email) VALUES ($1,$2)`,
		buyerID, buyerID.String()+"@eta.invalid"); err != nil {
		t.Fatalf("insert buyer: %v", err)
	}
	t.Cleanup(func() {
		c, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		_, _ = pool.Exec(c, `DELETE FROM eta_calibration WHERE job_type=$1`, jobType)
		_, _ = pool.Exec(c, `DELETE FROM jobs WHERE buyer_id=$1`, buyerID)
		_, _ = pool.Exec(c, `DELETE FROM buyers WHERE id=$1`, buyerID)
	})

	// Exercise the production writer for both named models. If it ever drops or
	// rewrites model_ref, this read-side isolation test must fail.
	for _, model := range []struct {
		ref          string
		realizedSecs int
	}{
		{modelA, 200},
		{modelB, 100},
	} {
		for range driftMinSamples {
			jobID := uuid.New()
			if _, err := pool.Exec(ctx,
				`INSERT INTO jobs
				   (id,buyer_id,status,job_type,model_ref,input_ref,tier,
				    eta_secs,eta_secs_raw,created_at)
				 VALUES ($1,$2,'complete',$3,$4,$5,'batch',100,100,
				         now() - make_interval(secs => $6))`,
				jobID, buyerID, jobType, model.ref, "eta/"+jobID.String(),
				model.realizedSecs); err != nil {
				t.Fatalf("insert %s job: %v", model.ref, err)
			}
			raw, _, realized, err := store.RecordEtaCalibration(ctx, jobID)
			if err != nil {
				t.Fatalf("record %s job: %v", model.ref, err)
			}
			if raw != 100 || realized < model.realizedSecs || realized > model.realizedSecs+2 {
				t.Fatalf("%s calibration raw/realized=%d/%d, want 100/about %d",
					model.ref, raw, realized, model.realizedSecs)
			}
		}
	}

	for range driftMinSamples {
		if _, err := pool.Exec(ctx,
			`INSERT INTO eta_calibration
			   (job_id,job_type,tier,predicted_secs,realized_secs)
			 VALUES ($1,$2,'batch',100,300)`,
			uuid.New(), jobType); err != nil {
			t.Fatalf("insert legacy calibration: %v", err)
		}
	}

	factorA, samplesA, err := store.ETABiasFactor(ctx, jobType, "batch", modelA)
	if err != nil {
		t.Fatal(err)
	}
	factorB, samplesB, err := store.ETABiasFactor(ctx, jobType, "batch", modelB)
	if err != nil {
		t.Fatal(err)
	}
	legacyFactor, legacySamples, err := store.ETABiasFactor(ctx, jobType, "batch", "")
	if err != nil {
		t.Fatal(err)
	}
	missingFactor, missingSamples, err := store.ETABiasFactor(ctx, jobType, "batch", "unseen-model")
	if err != nil {
		t.Fatal(err)
	}
	if samplesA != driftMinSamples || math.Abs(factorA-2) > 0.001 {
		t.Fatalf("slow model factor=%v samples=%d, want 2/%d", factorA, samplesA, driftMinSamples)
	}
	if samplesB != driftMinSamples || math.Abs(factorB-1) > 0.02 {
		t.Fatalf("fast model factor=%v samples=%d, want about 1/%d",
			factorB, samplesB, driftMinSamples)
	}
	if legacySamples != driftMinSamples || legacyFactor != etaBiasFactorMax {
		t.Fatalf("legacy factor=%v samples=%d, want cap %v/%d",
			legacyFactor, legacySamples, etaBiasFactorMax, driftMinSamples)
	}
	if missingSamples != 0 || missingFactor != 1 {
		t.Fatalf("unseen model inherited factor=%v samples=%d", missingFactor, missingSamples)
	}
}

// TestRecordEtaCalibrationLearnsFromRawAndConverges proves the production
// writer cannot feed its already-corrected buyer promise back into the learner.
// A stable raw estimate of 100s that realizes in 200s must keep producing a 2x
// bias even when every buyer-facing quote was already stretched to 200s.
func TestRecordEtaCalibrationLearnsFromRawAndConverges(t *testing.T) {
	store, pool, ctx := etaCalibrationTestStore(t)
	jobType := "etaconverge_" + uuid.NewString()[:8]
	modelRef := "eta-converge-model"
	buyerID := uuid.New()
	if _, err := pool.Exec(ctx,
		`INSERT INTO buyers (id,email) VALUES ($1,$2)`,
		buyerID, buyerID.String()+"@eta.invalid"); err != nil {
		t.Fatalf("insert buyer: %v", err)
	}
	t.Cleanup(func() {
		c, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		_, _ = pool.Exec(c, `DELETE FROM eta_calibration WHERE job_type=$1`, jobType)
		_, _ = pool.Exec(c, `DELETE FROM jobs WHERE buyer_id=$1`, buyerID)
		_, _ = pool.Exec(c, `DELETE FROM buyers WHERE id=$1`, buyerID)
	})

	for i := 0; i < driftMinSamples; i++ {
		jobID := uuid.New()
		if _, err := pool.Exec(ctx,
			`INSERT INTO jobs
			   (id,buyer_id,status,job_type,model_ref,input_ref,tier,eta_secs,eta_secs_raw,created_at)
			 VALUES ($1,$2,'complete',$3,$4,$5,'batch',200,100,
			         now() - interval '200 seconds')`,
			jobID, buyerID, jobType, modelRef, "eta/"+jobID.String()); err != nil {
			t.Fatalf("insert job %d: %v", i, err)
		}
		raw, quoted, realized, err := store.RecordEtaCalibration(ctx, jobID)
		if err != nil {
			t.Fatalf("record job %d: %v", i, err)
		}
		if raw != 100 || quoted != 200 || realized < 200 || realized > 202 {
			t.Fatalf("job %d calibration = raw %d quoted %d realized %d, want 100/200/about 200",
				i, raw, quoted, realized)
		}
		againRaw, againQuoted, againRealized, err := store.RecordEtaCalibration(ctx, jobID)
		if err != nil || againRaw != 0 || againQuoted != 0 || againRealized != 0 {
			t.Fatalf("job %d duplicate calibration = %d/%d/%d err=%v, want inert",
				i, againRaw, againQuoted, againRealized, err)
		}
	}

	factor, samples, err := store.ETABiasFactor(ctx, jobType, "batch", modelRef)
	if err != nil {
		t.Fatalf("ETABiasFactor: %v", err)
	}
	if samples != driftMinSamples || math.Abs(factor-2) > 0.02 {
		t.Fatalf("converged factor = %v from %d samples, want about 2 from %d",
			factor, samples, driftMinSamples)
	}
}

func TestRecordEtaCalibrationKeepsLegacyJobsHonest(t *testing.T) {
	store, pool, ctx := etaCalibrationTestStore(t)
	jobType := "etalegacy_" + uuid.NewString()[:8]
	buyerID, jobID := uuid.New(), uuid.New()
	if _, err := pool.Exec(ctx,
		`INSERT INTO buyers (id,email) VALUES ($1,$2)`,
		buyerID, buyerID.String()+"@eta.invalid"); err != nil {
		t.Fatalf("insert buyer: %v", err)
	}
	t.Cleanup(func() {
		c, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		_, _ = pool.Exec(c, `DELETE FROM eta_calibration WHERE job_id=$1`, jobID)
		_, _ = pool.Exec(c, `DELETE FROM jobs WHERE id=$1`, jobID)
		_, _ = pool.Exec(c, `DELETE FROM buyers WHERE id=$1`, buyerID)
	})
	if _, err := pool.Exec(ctx,
		`INSERT INTO jobs
		   (id,buyer_id,status,job_type,input_ref,tier,eta_secs,created_at)
		 VALUES ($1,$2,'complete',$3,$4,'batch',123,now() - interval '10 seconds')`,
		jobID, buyerID, jobType, "eta/"+jobID.String()); err != nil {
		t.Fatalf("insert legacy job: %v", err)
	}
	raw, quoted, realized, err := store.RecordEtaCalibration(ctx, jobID)
	if err != nil {
		t.Fatalf("record legacy job: %v", err)
	}
	if raw != 123 || quoted != 123 || realized < 10 {
		t.Fatalf("legacy calibration = raw %d quoted %d realized %d, want 123/123/at least 10",
			raw, quoted, realized)
	}
}
