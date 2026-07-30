package main

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
)

// TestInputDepthSchemaAppliesTwice proves the depth-band migration is
// apply-twice-safe (IF NOT EXISTS plus idempotent constraint validation).
func TestInputDepthSchemaAppliesTwice(t *testing.T) {
	ctx, store, _ := openIsolatedTestStore(t)
	// openIsolatedTestStore already migrated once; migrate again must be a no-op.
	second, cancel := context.WithTimeout(ctx, 60*time.Second)
	defer cancel()
	if err := store.Migrate(second); err != nil {
		t.Fatalf("second schema apply failed: %v", err)
	}
	// Spot-check the new columns, constraints, and scoped indexes.
	var etaBand, taskBand, geometryBand, etaConstraint, taskConstraint, geometryConstraint, geometryTrigger, etaIndex, taskIndex bool
	if err := store.pool.QueryRow(second, `
		SELECT
		  EXISTS (
		    SELECT 1 FROM information_schema.columns
		     WHERE table_name='eta_calibration' AND column_name='input_depth_band'
		  ),
		  EXISTS (
		    SELECT 1 FROM information_schema.columns
		     WHERE table_name='task_durations' AND column_name='input_depth_band'
		  ),
		  EXISTS (
		    SELECT 1 FROM information_schema.columns
		     WHERE table_name='tasks' AND column_name='input_depth_band'
		  ),
		  EXISTS (
		    SELECT 1 FROM pg_constraint
		     WHERE conrelid='eta_calibration'::regclass
		       AND conname='eta_calibration_input_depth_band_valid'
		       AND convalidated
		  ),
		  EXISTS (
		    SELECT 1 FROM pg_constraint
		     WHERE conrelid='task_durations'::regclass
		       AND conname='task_durations_input_depth_band_valid'
		       AND convalidated
		  ),
		  EXISTS (
		    SELECT 1 FROM pg_constraint
		     WHERE conrelid='tasks'::regclass
		       AND conname='tasks_input_depth_band_valid'
		       AND convalidated
		  ),
		  EXISTS (SELECT 1 FROM pg_trigger WHERE tgname='tasks_input_depth_band_immutable' AND NOT tgisinternal),
		  to_regclass('eta_calibration_scope_depth_idx') IS NOT NULL,
		  to_regclass('task_durations_scope_depth_idx') IS NOT NULL`,
	).Scan(
		&etaBand, &taskBand, &geometryBand, &etaConstraint, &taskConstraint, &geometryConstraint, &geometryTrigger, &etaIndex, &taskIndex,
	); err != nil {
		t.Fatalf("schema probe: %v", err)
	}
	if !etaBand || !taskBand || !geometryBand || !etaConstraint || !taskConstraint || !geometryConstraint || !geometryTrigger || !etaIndex || !taskIndex {
		t.Fatalf(
			"depth-band schema incomplete: columns=%v/%v/%v constraints=%v/%v/%v trigger=%v indexes=%v/%v",
			etaBand, taskBand, geometryBand, etaConstraint, taskConstraint, geometryConstraint, geometryTrigger, etaIndex, taskIndex,
		)
	}
	// This is task geometry, not mutable telemetry: a later write must not be
	// able to rewrite which duration-history bucket a completed chunk trains.
	taskID := uuid.New()
	if _, err := store.pool.Exec(second,
		`INSERT INTO tasks (id,input_depth_band) VALUES ($1,$2)`, taskID, inputDepthBandShort); err != nil {
		t.Fatalf("insert task depth authority: %v", err)
	}
	if _, err := store.pool.Exec(second,
		`UPDATE tasks SET input_depth_band=$2 WHERE id=$1`, taskID, inputDepthBandLong); err == nil ||
		!strings.Contains(err.Error(), "input depth band") {
		t.Fatalf("task depth authority update error=%v, want immutable rejection", err)
	}
}
