package main

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
)

func insertTaskDurationRows(t *testing.T, pool *pgxpool.Pool, ctx context.Context,
	jobType string, modelRef *string, durationMS, count int) {
	t.Helper()
	insertTaskDurationRowsWithBand(t, pool, ctx, jobType, modelRef, "", durationMS, count)
}

func insertTaskDurationRowsWithBand(t *testing.T, pool *pgxpool.Pool, ctx context.Context,
	jobType string, modelRef *string, band string, durationMS, count int) {
	t.Helper()
	var bandArg any
	if band != "" {
		bandArg = band
	}
	for range count {
		if _, err := pool.Exec(ctx,
			`INSERT INTO task_durations (job_type,model_ref,duration_ms,input_depth_band)
			 VALUES ($1,$2,$3,$4)`,
			jobType, modelRef, durationMS, bandArg); err != nil {
			t.Fatalf("insert task duration band=%q: %v", band, err)
		}
	}
}

func stringRef(value string) *string {
	return &value
}

func TestHistoricalP90DurationMsIsExactModelScoped(t *testing.T) {
	store, pool, ctx := etaCalibrationTestStore(t)
	jobType := "etahistory_" + uuid.NewString()[:8]
	t.Cleanup(func() {
		c, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		_, _ = pool.Exec(c, `DELETE FROM task_durations WHERE job_type=$1`, jobType)
	})

	insertTaskDurationRows(t, pool, ctx, jobType, stringRef("slow-model"), 2000, driftMinSamples)
	insertTaskDurationRows(t, pool, ctx, jobType, stringRef("fast-model"), 100, driftMinSamples)
	insertTaskDurationRows(t, pool, ctx, jobType, nil, 3000, 3)
	insertTaskDurationRows(t, pool, ctx, jobType, stringRef(""), 2500, 2)
	insertTaskDurationRows(t, pool, ctx, jobType, stringRef("thin-model"), 999, driftMinSamples-1)

	for _, tc := range []struct {
		model       string
		wantP90MS   int64
		wantSamples int
	}{
		{model: "slow-model", wantP90MS: 2000, wantSamples: driftMinSamples},
		{model: "fast-model", wantP90MS: 100, wantSamples: driftMinSamples},
		// NULL and empty are one honest legacy bucket, but named models never
		// bleed into it and it never bleeds into a named model.
		{model: "", wantP90MS: 3000, wantSamples: driftMinSamples},
		{model: "thin-model", wantP90MS: 0, wantSamples: driftMinSamples - 1},
		{model: "unseen-model", wantP90MS: 0, wantSamples: 0},
	} {
		p90ms, samples, err := store.HistoricalP90DurationMs(ctx, jobType, tc.model, "")
		if err != nil {
			t.Fatalf("HistoricalP90DurationMs(%q): %v", tc.model, err)
		}
		if p90ms != tc.wantP90MS || samples != tc.wantSamples {
			t.Fatalf("HistoricalP90DurationMs(%q) = %dms/%d samples, want %dms/%d",
				tc.model, p90ms, samples, tc.wantP90MS, tc.wantSamples)
		}
	}

	rollup, err := store.DriftRollup(ctx)
	if err != nil {
		t.Fatalf("DriftRollup: %v", err)
	}
	byModel := make(map[string]DriftRow)
	rowsForJobType := 0
	for _, row := range rollup {
		if row.JobType != jobType {
			continue
		}
		rowsForJobType++
		if _, exists := byModel[row.ModelRef]; exists {
			t.Fatalf("DriftRollup emitted duplicate normalized model key %q", row.ModelRef)
		}
		byModel[row.ModelRef] = row
	}
	if rowsForJobType != 4 {
		t.Fatalf("DriftRollup rows for %s = %d, want slow/fast/legacy/thin", jobType, rowsForJobType)
	}
	legacy := byModel[""]
	if legacy.Samples != driftMinSamples || legacy.P90DurationMs != 3000 ||
		!legacy.UsingObservedP90 {
		t.Fatalf("legacy DriftRollup = %+v, want one trusted 5-sample/3000ms row", legacy)
	}
	thin := byModel["thin-model"]
	if thin.Samples != driftMinSamples-1 || thin.UsingObservedP90 {
		t.Fatalf("thin DriftRollup = %+v, want untrusted %d-sample row", thin, driftMinSamples-1)
	}
}

// TestHistoricalP90DurationMsIsInputDepthBandScoped proves short/long/legacy
// duration buckets cannot contaminate each other.
func TestHistoricalP90DurationMsIsInputDepthBandScoped(t *testing.T) {
	store, pool, ctx := etaCalibrationTestStore(t)
	jobType := "etahistdepth_" + uuid.NewString()[:8]
	model := "depth-history-model"
	t.Cleanup(func() {
		c, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		_, _ = pool.Exec(c, `DELETE FROM task_durations WHERE job_type=$1`, jobType)
	})

	insertTaskDurationRowsWithBand(t, pool, ctx, jobType, stringRef(model), inputDepthBandShort, 100, driftMinSamples)
	insertTaskDurationRowsWithBand(t, pool, ctx, jobType, stringRef(model), inputDepthBandLong, 5000, driftMinSamples)
	insertTaskDurationRowsWithBand(t, pool, ctx, jobType, stringRef(model), "", 9000, driftMinSamples)

	for _, tc := range []struct {
		band        string
		wantP90MS   int64
		wantSamples int
	}{
		{band: inputDepthBandShort, wantP90MS: 100, wantSamples: driftMinSamples},
		{band: inputDepthBandLong, wantP90MS: 5000, wantSamples: driftMinSamples},
		{band: "", wantP90MS: 9000, wantSamples: driftMinSamples},
		{band: inputDepthBandMedium, wantP90MS: 0, wantSamples: 0},
	} {
		p90ms, samples, err := store.HistoricalP90DurationMs(ctx, jobType, model, tc.band)
		if err != nil {
			t.Fatalf("HistoricalP90DurationMs(band=%q): %v", tc.band, err)
		}
		if p90ms != tc.wantP90MS || samples != tc.wantSamples {
			t.Fatalf("HistoricalP90DurationMs(band=%q) = %dms/%d samples, want %dms/%d",
				tc.band, p90ms, samples, tc.wantP90MS, tc.wantSamples)
		}
	}
}
