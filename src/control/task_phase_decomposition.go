package main

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
)

// Where a batch task's time actually went, decomposed from the timestamps
// production already writes.
//
// Replay regret needs predicted-versus-actual PER PHASE, and the standing
// assumption was that none of it had been captured — that queue wait, startup
// and cold start had no column anywhere and replay therefore had to begin with
// a schema migration. That is true of columns with those names and false about
// the data. `tasks` already carries created_at, visible_at, claimed_at,
// started_at, completed_at and verified_at, and every one of them is written by
// a production path. The decomposition was derivable the whole time; nothing
// derived it.
//
// This is the actuals half only. Regret needs a prediction to subtract from,
// and eta_calibration holds predicted-versus-realized for the TOTAL alone, so
// there is nothing per-phase to regret against yet. Inventing per-phase
// predictions to make the arithmetic work would be manufacturing the very
// evidence the step exists to gather.
//
// UNKNOWN IS A VALUE HERE. Each timestamp is nullable, and a task that has not
// started has no startup duration — it does not have a startup duration of
// zero. Reporting zero would put a real number into a regret calculation that
// nothing measured, which is exactly how a latency atlas ends up confidently
// wrong. Every phase therefore reports Known alongside its duration, and
// callers that ignore Known get a zero they were warned about.

// TaskPhase names one measured span of a task's life.
type TaskPhase struct {
	Name string `json:"name"`
	// Known is false when either endpoint is absent. Duration is then zero and
	// carries no meaning.
	Known      bool          `json:"known"`
	Duration   time.Duration `json:"-"`
	DurationMS float64       `json:"duration_ms"`
	// Why records what was missing, so an unknown phase sends the reader to the
	// timestamp rather than to a guess.
	Why string `json:"unknown_reason,omitempty"`
}

// TaskPhaseDecomposition is one task's time, split into the spans the control
// plane can actually observe, bound to the decision that chose the work.
type TaskPhaseDecomposition struct {
	TaskID uuid.UUID `json:"task_id"`
	JobID  uuid.UUID `json:"job_id"`
	// PricingDecisionSHA256 binds these actuals to the accepted decision, so a
	// replay can ask what a decision produced rather than what a task did.
	// Empty when the job predates the decision chain.
	PricingDecisionSHA256 string `json:"pricing_decision_sha256,omitempty"`

	// Backoff is created_at to visible_at — the deliberate invisibility a retry
	// policy imposes before the task may be claimed again. Zero-length for a
	// first attempt.
	//
	// It exists as its own span rather than being folded into Queue because the
	// two have different owners: backoff is merc's own retry policy choosing to
	// wait, queue is the market failing to pick the work up. Blaming the market
	// for a delay the platform chose is exactly the misattribution a latency
	// atlas is supposed to prevent. But leaving the gap unnamed is no better —
	// it would simply vanish from the accounting, and the phases would quietly
	// fail to sum to the total on precisely the tasks that took longest.
	Backoff TaskPhase `json:"backoff"`
	// Queue is the wait between becoming claimable and being claimed.
	Queue TaskPhase `json:"queue"`
	// Startup is claim to execution start — dispatch, artifact and model load.
	Startup TaskPhase `json:"startup"`
	// Runtime is execution start to completion.
	Runtime TaskPhase `json:"runtime"`
	// Verification is completion to verdict.
	Verification TaskPhase `json:"verification"`
	// Total is created to verdict, or to completion when unverified.
	Total TaskPhase `json:"total"`

	// ReportedDurationMS is the supplier's own claim about physical execution.
	// It is kept beside Runtime rather than folded into it: Runtime is what the
	// control plane observed, this is what the supplier said, and a divergence
	// between them is a finding rather than something to average away.
	ReportedDurationMS  float64 `json:"reported_duration_ms,omitempty"`
	ReportedDurationSet bool    `json:"reported_duration_set"`
}

// KnownPhases returns only the spans that were actually measured, so a caller
// aggregating percentiles cannot silently include a zero that means "no data".
func (d TaskPhaseDecomposition) KnownPhases() []TaskPhase {
	var out []TaskPhase
	for _, p := range []TaskPhase{d.Backoff, d.Queue, d.Startup, d.Runtime, d.Verification, d.Total} {
		if p.Known {
			out = append(out, p)
		}
	}
	return out
}

func measuredPhase(name string, from, to *time.Time, fromName, toName string) TaskPhase {
	switch {
	case from == nil && to == nil:
		return TaskPhase{Name: name, Why: fromName + " and " + toName + " are both unset"}
	case from == nil:
		return TaskPhase{Name: name, Why: fromName + " is unset"}
	case to == nil:
		return TaskPhase{Name: name, Why: toName + " is unset"}
	}
	d := to.Sub(*from)
	if d < 0 {
		// Clock skew or an out-of-order write. Refuse rather than publish a
		// negative span or clamp it to zero, because a clamped negative is
		// indistinguishable from a fast phase.
		return TaskPhase{Name: name, Why: fmt.Sprintf(
			"%s is before %s by %s; the span is not measurable", toName, fromName, -d)}
	}
	return TaskPhase{Name: name, Known: true, Duration: d,
		DurationMS: float64(d) / float64(time.Millisecond)}
}

// later returns the later of two timestamps, or the one that is set.
func later(a, b *time.Time) (*time.Time, string) {
	switch {
	case a == nil && b == nil:
		return nil, "created_at and visible_at"
	case a == nil:
		return b, "visible_at"
	case b == nil:
		return a, "created_at"
	case b.After(*a):
		return b, "visible_at"
	default:
		return a, "created_at"
	}
}

// DecomposeTaskPhases reads one task's observable spans. It never writes, and
// it never invents a duration for a phase whose endpoints are not both present.
func DecomposeTaskPhases(ctx context.Context, db ledgerExec, taskID uuid.UUID) (TaskPhaseDecomposition, error) {
	if taskID == uuid.Nil {
		return TaskPhaseDecomposition{}, errors.New("task phase decomposition requires a task id")
	}
	var (
		out                                TaskPhaseDecomposition
		createdAt, visibleAt, claimedAt    *time.Time
		startedAt, completedAt, verifiedAt *time.Time
		reportedDurationMS                 *int64
		jobID                              *uuid.UUID
		pricingSHA                         *string
	)
	if err := db.QueryRow(ctx, `
		SELECT t.job_id, t.created_at, t.visible_at, t.claimed_at, t.started_at,
		       t.completed_at, t.verified_at, t.reported_duration_ms,
		       j.pricing_decision_sha256
		  FROM tasks t
		  LEFT JOIN jobs j ON j.id = t.job_id
		 WHERE t.id = $1`, taskID,
	).Scan(&jobID, &createdAt, &visibleAt, &claimedAt, &startedAt,
		&completedAt, &verifiedAt, &reportedDurationMS, &pricingSHA); err != nil {
		return TaskPhaseDecomposition{}, fmt.Errorf("read task %s phases: %w", taskID, err)
	}

	out.TaskID = taskID
	if jobID != nil {
		out.JobID = *jobID
	}
	if pricingSHA != nil {
		out.PricingDecisionSHA256 = *pricingSHA
	}

	claimable, claimableName := later(createdAt, visibleAt)
	// Backoff is only a real span when the task was held invisible; when
	// visible_at is at or before created_at there was no retry delay, and a
	// zero-length measured span is the truthful answer rather than an unknown.
	if createdAt != nil && visibleAt != nil && visibleAt.After(*createdAt) {
		out.Backoff = measuredPhase("backoff", createdAt, visibleAt, "created_at", "visible_at")
	} else if createdAt != nil {
		out.Backoff = TaskPhase{Name: "backoff", Known: true}
	} else {
		out.Backoff = TaskPhase{Name: "backoff", Why: "created_at is unset"}
	}
	out.Queue = measuredPhase("queue", claimable, claimedAt, claimableName, "claimed_at")
	out.Startup = measuredPhase("startup", claimedAt, startedAt, "claimed_at", "started_at")
	out.Runtime = measuredPhase("runtime", startedAt, completedAt, "started_at", "completed_at")
	out.Verification = measuredPhase("verification", completedAt, verifiedAt, "completed_at", "verified_at")

	// Total ends at the verdict when there is one, and at completion otherwise —
	// an unverified task has a real end, and refusing to report its total
	// because verification never ran would hide the work that did happen.
	end, endName := verifiedAt, "verified_at"
	if end == nil {
		end, endName = completedAt, "completed_at"
	}
	out.Total = measuredPhase("total", createdAt, end, "created_at", endName)

	if reportedDurationMS != nil {
		out.ReportedDurationMS, out.ReportedDurationSet = float64(*reportedDurationMS), true
	}
	return out, nil
}
