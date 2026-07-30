package main

import (
	"context"
	"testing"
	"time"
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
	var etaBand, taskBand, etaConstraint, taskConstraint, etaIndex, taskIndex bool
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
		  to_regclass('eta_calibration_scope_depth_idx') IS NOT NULL,
		  to_regclass('task_durations_scope_depth_idx') IS NOT NULL`,
	).Scan(
		&etaBand, &taskBand, &etaConstraint, &taskConstraint, &etaIndex, &taskIndex,
	); err != nil {
		t.Fatalf("schema probe: %v", err)
	}
	if !etaBand || !taskBand || !etaConstraint || !taskConstraint || !etaIndex || !taskIndex {
		t.Fatalf(
			"depth-band schema incomplete: columns=%v/%v constraints=%v/%v indexes=%v/%v",
			etaBand, taskBand, etaConstraint, taskConstraint, etaIndex, taskIndex,
		)
	}
}
