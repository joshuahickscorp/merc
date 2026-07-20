package main

import (
	"context"
	"encoding/json"
	"errors"
	"log"
	"net/http"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

type failurePolicy struct {
	retryable  bool
	buyerFault bool
}

var failureClasses = map[string]failurePolicy{
	"oom":                  {retryable: true, buyerFault: false}, // over-subscribed worker -> try elsewhere
	"model_load_failed":    {retryable: true, buyerFault: false}, // transient HF/download
	"thermal_throttle":     {retryable: true, buyerFault: false},
	"timeout":              {retryable: true, buyerFault: false},
	"worker_shutdown":      {retryable: true, buyerFault: false}, // graceful shutdown -> requeue now
	"transient_io":         {retryable: true, buyerFault: false},
	"object_store_error":   {retryable: true, buyerFault: false},
	"internal_error":       {retryable: true, buyerFault: false},
	"unsupported_model":    {retryable: false, buyerFault: true}, // request can't be served as specified
	"unsupported_job_type": {retryable: false, buyerFault: true},
	"bad_input":            {retryable: false, buyerFault: true}, // fail FAST  -  don't retry bad data elsewhere
	"bad_jsonl":            {retryable: false, buyerFault: true},
	"cancelled":            {retryable: false, buyerFault: false},
	"verification_failed":  {retryable: false, buyerFault: false}, // supplier fault; terminal here
}

func classifyFailure(class string) (policy failurePolicy, known bool) {
	p, ok := failureClasses[class]
	return p, ok
}

const immediateFailBackoff = 5 * time.Second

type FailureMemory struct {
	TotalGB            float32 `json:"total_gb"`
	AvailableGB        float32 `json:"available_gb"`
	EffectiveGB        float32 `json:"effective_gb"`
	ReservedHeadroomGB float32 `json:"reserved_headroom_gb"`
}

type FailureReport struct {
	Class      string         `json:"class"`
	Message    string         `json:"message"`
	DurationMS uint64         `json:"duration_ms"`
	Backend    string         `json:"backend"`
	Model      string         `json:"model"`
	Memory     *FailureMemory `json:"memory"`
}

type FailOutcome string

const (
	FailRequeued FailOutcome = "requeued" // retryable, under the retry cap -> claimable again now
	FailTerminal FailOutcome = "failed"   // terminal (bad input, or retries exhausted) -> job failed + settled at completed work
	FailNoop     FailOutcome = "noop"     // idempotent: task already resolved
)

var errNotOwner = errors.New("task is not claimed by this worker")

func (s *Store) FailTaskTx(ctx context.Context, taskID, workerID uuid.UUID, rep FailureReport) (FailOutcome, error) {
	policy, known := classifyFailure(rep.Class)
	class := rep.Class
	if !known {
		policy = failurePolicy{retryable: true, buyerFault: false} // unknown -> internal_error
		if class == "" {
			class = "internal_error"
		}
	}

	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return FailNoop, err
	}
	defer tx.Rollback(ctx)

	var jobID uuid.UUID
	var retry int16
	var status string
	var claimedBy *uuid.UUID
	err = tx.QueryRow(ctx,
		`SELECT job_id, retry_count, status, claimed_by FROM tasks WHERE id = $1 FOR UPDATE`,
		taskID).Scan(&jobID, &retry, &status, &claimedBy)
	if errors.Is(err, pgx.ErrNoRows) {
		return FailNoop, errNotFound
	}
	if err != nil {
		return FailNoop, err
	}
	if claimedBy == nil || *claimedBy != workerID {
		return FailNoop, errNotOwner
	}
	if status != "running" && status != "queued" && status != "retrying" {
		return FailNoop, nil
	}

	memBlob, _ := json.Marshal(rep.Memory) // nil -> "null"
	if _, err := tx.Exec(ctx,
		`INSERT INTO task_failures
		   (task_id, job_id, worker_id, failure_class, retryable, buyer_fault,
		    message, backend, model_ref, duration_ms, memory)
		 VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11)`,
		taskID, jobID, workerID, class, policy.retryable, policy.buyerFault,
		truncate(rep.Message, 500), rep.Backend, rep.Model, rep.DurationMS, memBlob,
	); err != nil {
		return FailNoop, err
	}
	if err := insertEventTx(ctx, tx, jobID, &taskID, "task_failed",
		"Task failed: "+class+failHint(policy), failDetail(class, policy, rep)); err != nil {
		return FailNoop, err
	}

	terminal := !policy.retryable || int(retry) >= maxTaskRetries
	if terminal {
		if _, err := tx.Exec(ctx, `UPDATE tasks SET status = 'failed' WHERE id = $1`, taskID); err != nil {
			return FailNoop, err
		}
		flipped, err := failJobAndSettleOnce(ctx, tx, jobID)
		if err != nil {
			return FailNoop, err
		}
		if flipped {
			reason := "retries exhausted"
			if policy.buyerFault {
				reason = "invalid input"
			}
			if err := insertEventTx(ctx, tx, jobID, &taskID, "job_failed",
				"Job failed ("+class+"): "+reason+". You are charged only for the chunks that "+
					"completed and were delivered; the rest was never charged.", nil); err != nil {
				return FailNoop, err
			}
		}
		if err := tx.Commit(ctx); err != nil {
			return FailNoop, err
		}
		return FailTerminal, nil
	}

	if _, err := tx.Exec(ctx,
		`UPDATE tasks
		   SET status = 'retrying', claimed_by = NULL, claimed_at = NULL, worker_id = NULL,
		       retry_count = retry_count + 1, visible_at = now() + make_interval(secs => $2)
		 WHERE id = $1`,
		taskID, immediateFailBackoff.Seconds()); err != nil {
		return FailNoop, err
	}
	if err := insertEventTx(ctx, tx, jobID, &taskID, "task_requeued",
		"Retrying chunk on another worker (was "+class+")", nil); err != nil {
		return FailNoop, err
	}
	if err := tx.Commit(ctx); err != nil {
		return FailNoop, err
	}
	return FailRequeued, nil
}

func failHint(p failurePolicy) string {
	if p.buyerFault {
		return " (input problem)"
	}
	return ""
}

func failDetail(class string, p failurePolicy, rep FailureReport) []byte {
	d := map[string]any{"class": class, "retryable": p.retryable, "buyer_fault": p.buyerFault}
	if rep.Backend != "" {
		d["backend"] = rep.Backend
	}
	if rep.Memory != nil {
		d["memory"] = rep.Memory
	}
	b, _ := json.Marshal(d)
	return b
}

func truncate(s string, n int) string {
	if len(s) > n {
		return s[:n]
	}
	return s
}

func insertEventTx(ctx context.Context, tx pgx.Tx, jobID uuid.UUID, taskID *uuid.UUID, event, buyerText string, detail []byte) error {
	_, err := tx.Exec(ctx,
		`INSERT INTO job_events (job_id, task_id, event, buyer_text, detail) VALUES ($1,$2,$3,$4,$5)`,
		jobID, taskID, event, buyerText, nullJSON(detail))
	return err
}

func (s *Store) InsertJobEvent(ctx context.Context, jobID uuid.UUID, taskID *uuid.UUID, event, buyerText string, detail []byte) error {
	_, err := s.pool.Exec(ctx,
		`INSERT INTO job_events (job_id, task_id, event, buyer_text, detail) VALUES ($1,$2,$3,$4,$5)`,
		jobID, taskID, event, buyerText, nullJSON(detail))
	return err
}

type JobEvent struct {
	Event     string    `json:"event"`
	BuyerText string    `json:"buyer_text"`
	TaskID    *string   `json:"task_id,omitempty"`
	CreatedAt time.Time `json:"created_at"`
}

func (s *Store) ListJobEvents(ctx context.Context, jobID, buyerID uuid.UUID) ([]JobEvent, error) {
	rows, err := s.pool.Query(ctx,
		`SELECT e.event, COALESCE(e.buyer_text,''), e.task_id, e.created_at
		   FROM job_events e JOIN jobs j ON j.id = e.job_id
		  WHERE e.job_id = $1 AND j.buyer_id = $2
		  ORDER BY e.created_at ASC, e.id ASC`,
		jobID, buyerID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []JobEvent
	for rows.Next() {
		var e JobEvent
		var tid *uuid.UUID
		if err := rows.Scan(&e.Event, &e.BuyerText, &tid, &e.CreatedAt); err != nil {
			return nil, err
		}
		if tid != nil {
			s := tid.String()
			e.TaskID = &s
		}
		out = append(out, e)
	}
	return out, rows.Err()
}

type TaskFailureView struct {
	FailureClass string    `json:"failure_class"`
	Retryable    bool      `json:"retryable"`
	BuyerFault   bool      `json:"buyer_fault"`
	Message      string    `json:"message"`
	Backend      string    `json:"backend"`
	Model        string    `json:"model_ref"`
	CreatedAt    time.Time `json:"created_at"`
}

func (s *Store) ListTaskFailuresByJob(ctx context.Context, jobID, buyerID uuid.UUID) ([]TaskFailureView, error) {
	rows, err := s.pool.Query(ctx,
		`SELECT f.failure_class, f.retryable, f.buyer_fault, COALESCE(f.message,''),
		        COALESCE(f.backend,''), COALESCE(f.model_ref,''), f.created_at
		   FROM task_failures f JOIN jobs j ON j.id = f.job_id
		  WHERE f.job_id = $1 AND j.buyer_id = $2
		  ORDER BY f.created_at DESC, f.id DESC`,
		jobID, buyerID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []TaskFailureView
	for rows.Next() {
		var f TaskFailureView
		if err := rows.Scan(&f.FailureClass, &f.Retryable, &f.BuyerFault, &f.Message,
			&f.Backend, &f.Model, &f.CreatedAt); err != nil {
			return nil, err
		}
		out = append(out, f)
	}
	return out, rows.Err()
}

func (s *Server) handleWorkerFail(w http.ResponseWriter, r *http.Request) {
	auth := r.Context().Value(ctxWorker).(*WorkerAuth)
	taskID, err := uuid.Parse(r.PathValue("id"))
	if err != nil {
		writeErr(w, http.StatusBadRequest, "invalid task id")
		return
	}
	var rep FailureReport
	if err := json.NewDecoder(r.Body).Decode(&rep); err != nil {
		writeErr(w, http.StatusBadRequest, "invalid fail report json")
		return
	}
	outcome, err := s.store.FailTaskTx(r.Context(), taskID, auth.WorkerID, rep)
	switch {
	case errors.Is(err, errNotFound):
		writeErr(w, http.StatusNotFound, "task not found")
		return
	case errors.Is(err, errNotOwner):
		writeErr(w, http.StatusConflict, "task is not claimed by this worker")
		return
	case err != nil:
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	if outcome == FailRequeued || outcome == FailTerminal {
		metrics.taskFailures.Add(1)
	}
	if outcome == FailTerminal {
		if jobID, jerr := s.store.TaskJobID(r.Context(), taskID); jerr != nil {
			log.Printf("fail: job lookup for checkpoint of task %s: %v", taskID, jerr)
		} else {
			checkpointBeforeFail(r.Context(), s.store, s.storage, jobID)
		}
	}
	writeJSON(w, http.StatusOK, map[string]string{"outcome": string(outcome)})
}

func (s *Server) handleJobEvents(w http.ResponseWriter, r *http.Request) {
	auth := r.Context().Value(ctxBuyer).(*AuthResult)
	jobID, err := uuid.Parse(r.PathValue("id"))
	if err != nil {
		writeErr(w, http.StatusBadRequest, "invalid job id")
		return
	}
	events, err := s.store.ListJobEvents(r.Context(), jobID, auth.BuyerID)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	if events == nil {
		events = []JobEvent{}
	}
	writeJSON(w, http.StatusOK, events)
}

func (s *Server) handleJobFailures(w http.ResponseWriter, r *http.Request) {
	auth := r.Context().Value(ctxBuyer).(*AuthResult)
	jobID, err := uuid.Parse(r.PathValue("id"))
	if err != nil {
		writeErr(w, http.StatusBadRequest, "invalid job id")
		return
	}
	fails, err := s.store.ListTaskFailuresByJob(r.Context(), jobID, auth.BuyerID)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	if fails == nil {
		fails = []TaskFailureView{}
	}
	writeJSON(w, http.StatusOK, fails)
}
