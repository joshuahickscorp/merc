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
	jobType, tier string, pairs [][2]int) {
	t.Helper()
	for _, p := range pairs {
		if _, err := pool.Exec(ctx,
			`INSERT INTO eta_calibration (job_id, job_type, tier, predicted_secs, realized_secs)
			 VALUES ($1,$2,$3,$4,$5)`,
			uuid.New(), jobType, tier, p[0], p[1]); err != nil {
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

	// Below the sample floor there is no evidence, so the quote is untouched.
	insertETACalibrationRows(t, pool, ctx, jobType, "batch", [][2]int{
		{100, 200}, {100, 200},
	})
	factor, samples, err := store.ETABiasFactor(ctx, jobType, "batch")
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
	insertETACalibrationRows(t, pool, ctx, jobType, "batch", [][2]int{
		{100, 200}, {100, 200}, {100, 200}, {50, 100},
	})
	factor, samples, err = store.ETABiasFactor(ctx, jobType, "batch")
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
	other, otherSamples, err := store.ETABiasFactor(ctx, jobType, "priority")
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
	insertETACalibrationRows(t, pool, ctx, jobType, "batch", [][2]int{
		{100, 10}, {100, 10}, {100, 10}, {100, 10}, {100, 10}, {100, 10},
	})
	factor, samples, err := store.ETABiasFactor(ctx, jobType, "batch")
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
	insertETACalibrationRows(t, pool, ctx, jobType, "batch", [][2]int{
		{1, 86400}, {1, 86400}, {1, 86400}, {1, 86400}, {1, 86400},
	})
	factor, _, err := store.ETABiasFactor(ctx, jobType, "batch")
	if err != nil {
		t.Fatalf("ETABiasFactor: %v", err)
	}
	if factor != etaBiasFactorMax {
		t.Fatalf("factor = %v, want the cap %v", factor, etaBiasFactorMax)
	}
}
