package main

import (
	"testing"
	"time"

	"github.com/google/uuid"
)

// The decomposition has to be right about three things: the arithmetic, the
// boundary choices, and — most importantly — what it refuses to answer.
//
// A latency atlas that reports zero for an unmeasured phase is worse than one
// that reports nothing, because a zero survives averaging and a refusal does
// not. Every unknown case below exists to prove the refusal, not the number.
func TestTaskPhaseDecompositionMeasuresWhatHappenedAndRefusesTheRest(t *testing.T) {
	installSettlementCurrencyForTest(t, "usd")
	ctx, store, pool := openIsolatedTestStore(t)
	_ = store

	buyerID := uuid.New()
	if _, err := pool.Exec(ctx, `INSERT INTO buyers (id,email,free_credit_usd) VALUES ($1,$2,0)`,
		buyerID, buyerID.String()+"@phases.invalid"); err != nil {
		t.Fatal(err)
	}
	jobID := uuid.New()
	const decisionSHA = "1111111111111111111111111111111111111111111111111111111111111111"
	if _, err := pool.Exec(ctx, `
		INSERT INTO jobs (id,buyer_id,status,job_type,model_ref,input_ref,task_count,
		                  offered_rate_usd_hr,min_memory_gb,tier,currency,
		                  pricing_decision,pricing_decision_sha256)
		VALUES ($1,$2,'complete','embed','all-minilm-l6-v2','in',1,10.0,0,'batch','usd',
		        '{"version":"test"}'::jsonb,$3)`,
		jobID, buyerID, decisionSHA); err != nil {
		t.Fatal(err)
	}

	base := time.Now().UTC().Add(-time.Hour)
	newTask := func(created, visible, claimed, started, completed, verified *time.Time, reported *int64) uuid.UUID {
		t.Helper()
		id := uuid.New()
		if _, err := pool.Exec(ctx, `
			INSERT INTO tasks (id,job_id,status,input_ref,result_key,
			                   created_at,visible_at,claimed_at,started_at,completed_at,
			                   verified_at,reported_duration_ms)
			VALUES ($1,$2,'complete','in','rk',$3,$4,$5,$6,$7,$8,$9)`,
			id, jobID, created, visible, claimed, started, completed, verified, reported); err != nil {
			t.Fatalf("seed task: %v", err)
		}
		return id
	}
	at := func(d time.Duration) *time.Time { ts := base.Add(d); return &ts }

	t.Run("a fully observed task decomposes exactly", func(t *testing.T) {
		reported := int64(4_000)
		id := newTask(at(0), at(0), at(10*time.Second), at(13*time.Second),
			at(18*time.Second), at(20*time.Second), &reported)
		got, err := DecomposeTaskPhases(ctx, pool, id)
		must(t, err)

		for name, want := range map[string]struct {
			phase TaskPhase
			ms    float64
		}{
			"queue":        {got.Queue, 10_000},
			"startup":      {got.Startup, 3_000},
			"runtime":      {got.Runtime, 5_000},
			"verification": {got.Verification, 2_000},
			"total":        {got.Total, 20_000},
		} {
			if !want.phase.Known {
				t.Fatalf("%s was reported unknown on a fully timestamped task: %s", name, want.phase.Why)
			}
			if want.phase.DurationMS != want.ms {
				t.Fatalf("%s = %.0fms, want %.0fms", name, want.phase.DurationMS, want.ms)
			}
		}
		// The phases must account for the total, or the decomposition is
		// describing a different task than the one it measured.
		sum := got.Backoff.DurationMS + got.Queue.DurationMS + got.Startup.DurationMS +
			got.Runtime.DurationMS + got.Verification.DurationMS
		if sum != got.Total.DurationMS {
			t.Fatalf("phases sum to %.0fms but total is %.0fms; %.0fms of the task's life is "+
				"unaccounted for", sum, got.Total.DurationMS, got.Total.DurationMS-sum)
		}
		if got.PricingDecisionSHA256 != decisionSHA {
			t.Fatalf("actuals are not bound to the decision that chose the work: sha=%q",
				got.PricingDecisionSHA256)
		}
		// The supplier's own claim stays beside the observed runtime, not inside it.
		if !got.ReportedDurationSet || got.ReportedDurationMS != 4_000 {
			t.Fatalf("supplier reported duration lost: set=%v ms=%.0f", got.ReportedDurationSet, got.ReportedDurationMS)
		}
		if got.ReportedDurationMS == got.Runtime.DurationMS {
			t.Fatal("this fixture deliberately makes the supplier's reported duration differ from " +
				"observed runtime; if they are equal the test can no longer tell them apart")
		}
	})

	t.Run("retry backoff is its own span, not queue wait and not lost", func(t *testing.T) {
		// Created an hour ago, invisible for the first 30s of backoff, claimed
		// 10s after becoming visible. The market waited 10s, not 40s — and the
		// 30s merc itself chose to wait must still appear somewhere.
		id := newTask(at(0), at(30*time.Second), at(40*time.Second),
			at(41*time.Second), at(42*time.Second), at(43*time.Second), nil)
		got, err := DecomposeTaskPhases(ctx, pool, id)
		must(t, err)
		if !got.Queue.Known || got.Queue.DurationMS != 10_000 {
			t.Fatalf("queue = %.0fms (known=%v), want 10000ms measured from visible_at; "+
				"counting retry backoff as queue wait blames the market for the retry policy's delay",
				got.Queue.DurationMS, got.Queue.Known)
		}
		if !got.Backoff.Known || got.Backoff.DurationMS != 30_000 {
			t.Fatalf("backoff = %.0fms (known=%v), want 30000ms; the delay merc's own retry "+
				"policy imposed must be attributed, not deleted",
				got.Backoff.DurationMS, got.Backoff.Known)
		}
		// And with backoff named, the accounting closes on a retried task too —
		// which it did NOT before backoff existed, precisely on the tasks that
		// took longest.
		sum := got.Backoff.DurationMS + got.Queue.DurationMS + got.Startup.DurationMS +
			got.Runtime.DurationMS + got.Verification.DurationMS
		if sum != got.Total.DurationMS {
			t.Fatalf("a retried task's phases sum to %.0fms against a total of %.0fms; "+
				"%.0fms went unattributed", sum, got.Total.DurationMS, got.Total.DurationMS-sum)
		}
	})

	t.Run("an unstarted task has no startup duration, not a zero one", func(t *testing.T) {
		id := newTask(at(0), at(0), at(5*time.Second), nil, nil, nil, nil)
		got, err := DecomposeTaskPhases(ctx, pool, id)
		must(t, err)
		for name, p := range map[string]TaskPhase{
			"startup": got.Startup, "runtime": got.Runtime,
			"verification": got.Verification, "total": got.Total,
		} {
			if p.Known {
				t.Fatalf("%s reported %.0fms for a task that never started; an unmeasured phase "+
					"must not enter a percentile as a zero", name, p.DurationMS)
			}
			if p.Why == "" {
				t.Fatalf("%s is unknown but does not say which timestamp was missing", name)
			}
		}
		// Two known spans: the queue wait, and a zero-length backoff. A first
		// attempt genuinely had no retry delay — that is a measured fact, not an
		// absent measurement, and it is the one case where zero is the honest
		// answer rather than the dangerous one.
		if n := len(got.KnownPhases()); n != 2 {
			t.Fatalf("KnownPhases returned %d spans for a task that queued and never started; "+
				"want 2 (queue, plus a zero-length backoff) — an aggregator using this must not "+
				"average in phases that were never observed, nor drop one that was", n)
		}
		if !got.Backoff.Known || got.Backoff.DurationMS != 0 {
			t.Fatalf("first-attempt backoff = %.0fms (known=%v), want a measured 0ms",
				got.Backoff.DurationMS, got.Backoff.Known)
		}
	})

	t.Run("an unverified task still reports a total", func(t *testing.T) {
		id := newTask(at(0), at(0), at(2*time.Second), at(3*time.Second), at(9*time.Second), nil, nil)
		got, err := DecomposeTaskPhases(ctx, pool, id)
		must(t, err)
		if got.Verification.Known {
			t.Fatal("verification was reported for a task with no verdict")
		}
		if !got.Total.Known || got.Total.DurationMS != 9_000 {
			t.Fatalf("total = %.0fms (known=%v), want 9000ms to completion; refusing a total "+
				"because verification never ran would hide the work that did happen",
				got.Total.DurationMS, got.Total.Known)
		}
	})

	t.Run("a backwards span is refused, not clamped", func(t *testing.T) {
		// completed_at before started_at — clock skew or an out-of-order write.
		id := newTask(at(0), at(0), at(2*time.Second), at(9*time.Second), at(4*time.Second), nil, nil)
		got, err := DecomposeTaskPhases(ctx, pool, id)
		must(t, err)
		if got.Runtime.Known {
			t.Fatalf("a negative runtime was published as %.0fms; clamping it to zero makes "+
				"corrupt ordering indistinguishable from a fast phase", got.Runtime.DurationMS)
		}
		if got.Runtime.Why == "" {
			t.Fatal("a backwards span was refused without saying why")
		}
	})
}
