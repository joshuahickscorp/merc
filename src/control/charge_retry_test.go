package main

import (
	"testing"
	"time"

	"github.com/google/uuid"
)

// Before the cap, a permanently dead card retried forever: chargeRetryBackoff
// bounded the interval at 6h but nothing bounded the attempt count, so the
// sweep re-selected the job four times a day indefinitely.

func TestChargeRetryBackoffIsBoundedAndMonotonic(t *testing.T) {
	var prev time.Duration
	for attempts := 1; attempts <= 100; attempts++ {
		d := chargeRetryBackoff(attempts)
		if d <= 0 {
			t.Fatalf("attempt %d produced a non-positive delay %v", attempts, d)
		}
		if d > chargeRetryMax {
			t.Fatalf("attempt %d exceeded the cap: %v > %v", attempts, d, chargeRetryMax)
		}
		if d < prev {
			t.Fatalf("backoff went backwards at attempt %d: %v then %v", attempts, prev, d)
		}
		prev = d
	}
	// Degenerate inputs must not produce a zero or negative delay, which would
	// make the retry loop spin.
	for _, a := range []int{0, -1, -1000} {
		if d := chargeRetryBackoff(a); d <= 0 {
			t.Fatalf("attempts=%d produced %v; a non-positive delay spins the sweep", a, d)
		}
	}
}

// The cap must be finite and small enough that a dead card cannot generate an
// unbounded number of Stripe attempts.
func TestChargeMaxAttemptsIsFinite(t *testing.T) {
	if chargeMaxAttempts <= 0 {
		t.Fatalf("chargeMaxAttempts must be positive, got %d", chargeMaxAttempts)
	}
	if chargeMaxAttempts > 24 {
		t.Fatalf("chargeMaxAttempts=%d is high enough to look like no cap at all", chargeMaxAttempts)
	}
	// Worst case wall-clock spent retrying before a human is involved.
	var total time.Duration
	for i := 1; i <= chargeMaxAttempts; i++ {
		total += chargeRetryBackoff(i)
	}
	if total > 7*24*time.Hour {
		t.Fatalf("retry schedule spans %v before manual review; that is too long to sit uncollected", total)
	}
}

func TestManualReviewEndsTheAutomaticLoop(t *testing.T) {
	ctx, store, pool := openPayoutTestStore(t)

	// noBuyerCash leaves the job at 'not_attempted', from which 'failed' is a
	// legal transition ('charged' is terminal, so the funded fixture cannot be
	// walked into a retry state).
	f := seedPayoutFixture(t, ctx, pool, payoutFixtureOpts{creditUSD: 1.00, noBuyerCash: true})
	if _, err := pool.Exec(ctx, `UPDATE jobs SET charge_status='failed', charge_next_at=NULL WHERE id=$1`,
		f.jobID); err != nil {
		t.Fatalf("seed failed charge: %v", err)
	}

	due, err := store.FailedChargesDue(ctx, 100)
	mustf(t, err, "FailedChargesDue: %v")
	if !containsJob(due, f.jobID) {
		t.Fatal("a failed job should be selected for retry before the cap")
	}

	mustf(t, store.MarkChargeManualReview(ctx, f.jobID), "MarkChargeManualReview: %v")

	status, err := store.JobChargeStatus(ctx, f.jobID)
	mustf(t, err, "JobChargeStatus: %v")
	if status != chargeStatusManualReview {
		t.Fatalf("charge_status = %q, want %q", status, chargeStatusManualReview)
	}

	// The whole point: it must no longer be selected for automatic retry.
	due, err = store.FailedChargesDue(ctx, 100)
	mustf(t, err, "FailedChargesDue after manual review: %v")
	if containsJob(due, f.jobID) {
		t.Fatal("job in manual_review is still being retried automatically")
	}

	// Idempotent: a second call cannot re-transition it.
	if err := store.MarkChargeManualReview(ctx, f.jobID); err == nil {
		t.Fatal("second MarkChargeManualReview should refuse; the job is no longer 'failed'")
	}
}

func containsJob(ids []uuid.UUID, want uuid.UUID) bool {
	for _, id := range ids {
		if id == want {
			return true
		}
	}
	return false
}
