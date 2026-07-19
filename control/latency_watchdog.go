package main

import (
	"context"
	"errors"
	"log"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

const (
	noPeerWatchdogInterval = 30 * time.Second
	noPeerWatchdogAfter    = 5 * time.Minute
	coldModelLoadAllowance = 6 * time.Minute
)

func (s *Store) isColdModelStraggler(ctx context.Context, taskID uuid.UUID) (bool, error) {
	var cold bool
	err := s.pool.QueryRow(ctx, `
		SELECT
			COALESCE(j.model_ref,'') <> ''
			AND t.started_at IS NOT NULL
			AND t.started_at > now() - make_interval(secs => $2)
			AND NOT EXISTS (
				SELECT 1 FROM worker_model_state wms
				WHERE wms.worker_id = t.worker_id
				  AND wms.model_id = j.model_ref
				  AND wms.last_seen_warm > now() - interval '60 seconds'
			)
		FROM tasks t JOIN jobs j ON j.id = t.job_id
		WHERE t.id = $1`,
		taskID, coldModelLoadAllowance.Seconds()).Scan(&cold)
	if errors.Is(err, pgx.ErrNoRows) {
		return false, nil // task vanished (committed/cancelled between find and check)  -  not cold, not an error
	}
	return cold, err
}

type NoPeerWedged struct {
	TaskID   uuid.UUID
	JobID    uuid.UUID
	WorkerID uuid.UUID
	JobType  string
	ModelRef string
	MinMemGB float32
}

func (s *Store) NoPeerWedgedCandidates(ctx context.Context, after, deadAfter time.Duration, limit int) ([]NoPeerWedged, error) {
	rows, err := s.pool.Query(ctx,
		`SELECT t.id, t.job_id, t.worker_id, j.job_type, COALESCE(j.model_ref,''),
		        COALESCE(j.min_memory_gb,0)
		 FROM tasks t
		 JOIN jobs j    ON j.id = t.job_id
		 JOIN workers w ON w.id = t.worker_id
		 WHERE t.status = 'running'
		   AND t.is_honeypot = false AND t.is_redundancy = false
		   AND t.hedged_from IS NULL
		   AND t.started_at IS NOT NULL
		   AND t.started_at < now() - make_interval(secs => $1)
		   AND j.status = 'running'
		   -- worker is STILL heartbeating: a dead worker's task belongs to the
		   -- dead-claim rescue, not this watchdog (that path docks + requeues faster).
		   AND w.last_seen_at IS NOT NULL
		   AND w.last_seen_at > now() - make_interval(secs => $2)
		 ORDER BY t.started_at ASC
		 LIMIT $3`,
		after.Seconds(), deadAfter.Seconds(), limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []NoPeerWedged
	for rows.Next() {
		var n NoPeerWedged
		if err := rows.Scan(&n.TaskID, &n.JobID, &n.WorkerID, &n.JobType, &n.ModelRef, &n.MinMemGB); err != nil {
			return nil, err
		}
		out = append(out, n)
	}
	return out, rows.Err()
}

func (wk *Workers) reapNoPeerWedged(ctx context.Context) error {
	if started := workersStarted(); !started.IsZero() && time.Since(started) < deadWorkerAfter {
		return nil
	}
	candidates, err := wk.store.NoPeerWedgedCandidates(ctx, noPeerWatchdogAfter, deadWorkerAfter, sweepBatch)
	if err != nil {
		return err
	}
	for _, c := range candidates {
		_, perr := wk.store.SelectRedundancyPeerExcluding(ctx, c.JobType, c.ModelRef, c.MinMemGB, c.WorkerID, nil, nil)
		if perr == nil {
			continue // a peer exists -> hedging handles it, not this watchdog
		}
		if !errors.Is(perr, ErrNoSupply) {
			return perr // a real error probing supply  -  surface it, don't misread as no-peer
		}
		if rerr := wk.store.RequeueTask(ctx, c.TaskID); rerr != nil {
			log.Printf("workers: no-peer watchdog: requeue task %s (chunk of job %s): %v", c.TaskID, c.JobID, rerr)
			continue
		}
		metrics.noPeerRequeues.Add(1)
		log.Printf("workers: no-peer watchdog: requeued wedged task %s (job %s, type %s)  -  held by heartbeating worker %s past %s with no eligible same-class peer; escaped ahead of the %s stale reaper",
			c.TaskID, c.JobID, c.JobType, c.WorkerID, noPeerWatchdogAfter, staleTaskTimeout)
	}
	return nil
}
