package main

import (
	"context"
	"fmt"

	"github.com/google/uuid"
)

func (s *Store) FinalizeTaskVerification(ctx context.Context, info *CommitTaskInfo, outcome VerifyOutcome, entries []LedgerEntry) error {
	switch outcome {
	case OutcomePass, OutcomePassWithPenalty, OutcomeLossNoPayout:
	default:
		return fmt.Errorf("cannot finalize task %s with non-accepting outcome %q", info.TaskID, outcome)
	}

	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)

	var hedgedFrom *uuid.UUID
	var isRedundancy bool
	if err := tx.QueryRow(ctx,
		`SELECT hedged_from, is_redundancy FROM tasks WHERE id=$1`, info.TaskID,
	).Scan(&hedgedFrom, &isRedundancy); err != nil {
		return err
	}
	if !isRedundancy {
		rootTaskID := info.TaskID
		if hedgedFrom != nil {
			rootTaskID = *hedgedFrom
		}
		var lockedRoot uuid.UUID
		if err := tx.QueryRow(ctx,
			`SELECT id FROM tasks WHERE id=$1 FOR UPDATE`, rootTaskID,
		).Scan(&lockedRoot); err != nil {
			return err
		}
		var siblingAlreadySettled bool
		if err := tx.QueryRow(ctx, `
			SELECT EXISTS (
			  SELECT 1
			    FROM ledger_entries le
			    JOIN tasks sibling ON sibling.id=le.task_id
			   WHERE le.kind='buyer_charge'
			     AND sibling.is_redundancy=false
			     AND sibling.id<>$2
			     AND (sibling.id=$1 OR sibling.hedged_from=$1)
			)`, rootTaskID, info.TaskID).Scan(&siblingAlreadySettled); err != nil {
			return err
		}
		if siblingAlreadySettled {
			entries = nil
		}
	}

	ct, err := tx.Exec(ctx,
		`UPDATE tasks
		   SET status = 'complete', completed_at = now(),
		       verification_outcome = $3, verified_at = now()
		 WHERE id = $1 AND worker_id = $2 AND status = 'verifying'`,
		info.TaskID, info.WorkerID, string(outcome))
	if err != nil {
		return err
	}
	if ct.RowsAffected() == 0 {
		return errNotFound
	}

	if _, err := tx.Exec(ctx,
		`INSERT INTO task_verdicts
		   (task_id, attempt, job_id, supplier_id, outcome, result_sha256)
		 VALUES ($1,$2,$3,$4,$5,NULLIF($6,''))
		 ON CONFLICT (task_id, attempt) DO NOTHING`,
		info.TaskID, info.Attempt, info.JobID, info.SupplierID, string(outcome), info.ResultSHA256,
	); err != nil {
		return err
	}

	if _, err := tx.Exec(ctx,
		`UPDATE jobs SET tasks_done = tasks_done + 1 WHERE id = $1`, info.JobID); err != nil {
		return err
	}
	if _, err := tx.Exec(ctx,
		`UPDATE suppliers SET completed_tasks = completed_tasks + 1 WHERE id = $1`, info.SupplierID); err != nil {
		return err
	}

	if _, err := tx.Exec(ctx,
		`INSERT INTO task_durations
		   (task_id, job_id, job_type, model_ref, split_size, duration_ms, worker_id, engine, build_hash)
		 VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9)`,
		info.TaskID, info.JobID, info.jobType, info.ModelRef, info.SplitSize, int64(info.DurationMS),
		info.WorkerID, info.engine, info.buildHash,
	); err != nil {
		return err
	}

	for _, entry := range entries {
		if _, err := tx.Exec(ctx,
			`INSERT INTO ledger_entries
			   (kind, supplier_id, buyer_id, task_id, amount_usd, payout_status, release_at)
			 VALUES ($1,$2,$3,$4,$5,$6,$7)
			 ON CONFLICT (task_id, kind) DO NOTHING`,
			entry.Kind, entry.SupplierID, entry.BuyerID, entry.TaskID,
			entry.AmountUSD, entry.PayoutStatus, entry.ReleaseAt,
		); err != nil {
			return err
		}
	}

	return tx.Commit(ctx)
}

func (s *Store) RecordRejectedTaskVerdict(ctx context.Context, info *CommitTaskInfo) error {
	_, err := s.pool.Exec(ctx,
		`INSERT INTO task_verdicts
		   (task_id, attempt, job_id, supplier_id, outcome, result_sha256)
		 VALUES ($1,$2,$3,$4,'fail',NULLIF($5,''))
		 ON CONFLICT (task_id, attempt) DO NOTHING`,
		info.TaskID, info.Attempt, info.JobID, info.SupplierID, info.ResultSHA256)
	return err
}

func (s *Store) TaskVerdictOutcome(ctx context.Context, taskID uuid.UUID) (string, error) {
	var outcome string
	err := s.pool.QueryRow(ctx,
		`SELECT COALESCE(verification_outcome,'') FROM tasks WHERE id = $1`, taskID,
	).Scan(&outcome)
	return outcome, err
}
