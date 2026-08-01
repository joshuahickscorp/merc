package main

import (
	"context"
	"testing"
)

// Applying a catalogue schedule records one model_price_history row per model,
// keyed (schedule_sha256, model_id). The skip that makes re-application a no-op
// is keyed on models.price_schedule_sha256 instead. Anything that resets model
// price state without clearing the history - a re-seed, or a restore that brings
// back one table and not the other - made the skip miss while the row still
// existed, so ApplyRepricing failed on a duplicate key and the control plane
// could not start at all.
//
// Recovery is the moment that must not happen, so this pins it.

func TestRepricingSurvivesModelPriceStateBeingReset(t *testing.T) {
	pinBoardClockForPublication(t)
	ctx, store, _ := openAdminMutationTestStore(t)

	schedule, err := BuildCataloguePriceSchedule()
	if err != nil {
		t.Fatalf("build schedule: %v", err)
	}
	if _, err := store.ApplyRepricing(ctx, schedule); err != nil {
		t.Fatalf("first apply: %v", err)
	}

	// Re-seeding clears the schedule pointer on models but leaves the history.
	// That is exactly the state a restore or `make seed` leaves behind.
	if _, err := store.pool.Exec(ctx, `
		UPDATE models SET price_schedule_sha256=NULL, price_source='seed'`); err != nil {
		t.Fatalf("simulate model price state reset: %v", err)
	}

	if _, err := store.ApplyRepricing(ctx, schedule); err != nil {
		t.Fatalf("re-apply after a model price reset must succeed, got: %v", err)
	}

	// The history is a record of the transition, so it must not be duplicated.
	var rows int
	if err := store.pool.QueryRow(ctx, `
		SELECT count(*) FROM model_price_history WHERE schedule_sha256=$1`,
		schedule.SHA256).Scan(&rows); err != nil {
		t.Fatalf("count history: %v", err)
	}
	if rows != len(schedule.Results) {
		t.Fatalf("history rows = %d, want one per model (%d)", rows, len(schedule.Results))
	}
}

// Tolerating a re-apply must not become tolerating a rewritten history. What
// actually guarantees that is stronger than the comparison in ApplyRepricing:
// the table is append-only at the database level, so a recorded outcome cannot
// be edited to match a schedule that claims something different. The comparison
// remains as defence for the case the trigger is ever relaxed.
func TestRecordedPriceHistoryCannotBeRewritten(t *testing.T) {
	pinBoardClockForPublication(t)
	ctx, store, _ := openAdminMutationTestStore(t)

	schedule, err := BuildCataloguePriceSchedule()
	if err != nil {
		t.Fatalf("build schedule: %v", err)
	}
	if _, err := store.ApplyRepricing(ctx, schedule); err != nil {
		t.Fatalf("first apply: %v", err)
	}

	if _, err := store.pool.Exec(ctx, `
		UPDATE model_price_history SET price_per_1k = price_per_1k * 2
		 WHERE schedule_sha256=$1`, schedule.SHA256); err == nil {
		t.Fatal("model_price_history accepted an UPDATE; the price record is not append-only")
	}
	if _, err := store.pool.Exec(ctx, `
		DELETE FROM model_price_history WHERE schedule_sha256=$1`, schedule.SHA256); err == nil {
		t.Fatal("model_price_history accepted a DELETE; the price record is not append-only")
	}

	// And the recorded outcome still matches what the schedule says, so the
	// re-apply path above is comparing against an untampered row.
	var recorded float64
	if err := store.pool.QueryRow(ctx, `
		SELECT price_per_1k FROM model_price_history
		 WHERE schedule_sha256=$1 AND model_id=$2`,
		schedule.SHA256, schedule.Results[0].ModelID).Scan(&recorded); err != nil {
		t.Fatalf("read recorded price: %v", err)
	}
	if recorded != schedule.Results[0].PricePer1K {
		t.Fatalf("recorded price %v does not match the schedule %v",
			recorded, schedule.Results[0].PricePer1K)
	}
}

var _ = context.Background
