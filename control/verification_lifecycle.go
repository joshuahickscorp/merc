package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

var ErrJobVerificationPending = errors.New("job has unresolved verification work")

func jobTerminalTransitionStateTx(ctx context.Context, tx pgx.Tx, jobID uuid.UUID) (status string, pending bool, err error) {
	err = tx.QueryRow(ctx, `SELECT status FROM jobs WHERE id=$1 FOR UPDATE`, jobID).Scan(&status)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", false, errNotFound
	}
	if err != nil {
		return "", false, err
	}
	err = tx.QueryRow(ctx, `
		SELECT EXISTS (
		         SELECT 1 FROM tasks t
		          WHERE t.job_id=$1 AND t.status='verifying'
		       ) OR EXISTS (
		         SELECT 1 FROM verification_work w
		          WHERE w.job_id=$1 AND w.status<>'terminal'
		       )`, jobID).Scan(&pending)
	return status, pending, err
}

func (s *Store) jobTerminalTransitionState(ctx context.Context, jobID uuid.UUID) (status string, pending bool, err error) {
	err = s.pool.QueryRow(ctx, `
		SELECT j.status,
		       EXISTS (
		         SELECT 1 FROM tasks t
		          WHERE t.job_id=j.id AND t.status='verifying'
		       ) OR EXISTS (
		         SELECT 1 FROM verification_work w
		          WHERE w.job_id=j.id AND w.status<>'terminal'
		       )
		  FROM jobs j WHERE j.id=$1`, jobID).Scan(&status, &pending)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", false, errNotFound
	}
	return status, pending, err
}

func lockUnfinishedJobTasksTx(ctx context.Context, tx pgx.Tx, jobID uuid.UUID) error {
	rows, err := tx.Query(ctx, `
		SELECT id FROM tasks
		 WHERE job_id=$1 AND status NOT IN ('complete','failed','cancelled')
		 ORDER BY id FOR UPDATE`, jobID)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var id uuid.UUID
		if err := rows.Scan(&id); err != nil {
			return err
		}
	}
	return rows.Err()
}

func (s *Store) ReconcileLegacyVerifyingTasks(ctx context.Context) (int64, error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return 0, err
	}
	defer tx.Rollback(ctx)

	rows, err := tx.Query(ctx, `
		WITH candidates AS (
		  SELECT t.id,t.job_id,COALESCE(t.retry_count,0)::int AS abandoned_attempt
		    FROM tasks t JOIN jobs j ON j.id=t.job_id
		   WHERE t.status='verifying'
		     AND j.status NOT IN ('complete','failed','cancelled')
		     AND NOT EXISTS (
		       SELECT 1 FROM verification_work w
		        WHERE w.task_id=t.id AND w.attempt=COALESCE(t.retry_count,0)
		     )
		   FOR UPDATE OF t SKIP LOCKED
		), requeued AS (
		  UPDATE tasks t
		     SET status='retrying',claimed_by=NULL,claimed_at=NULL,worker_id=NULL,
		         started_at=NULL,completed_at=NULL,
		         retry_count=LEAST(COALESCE(t.retry_count,0)::int+1,32767)::smallint,
		         visible_at=now(),excluded_worker=NULL,excluded_until=NULL,
		         result_ref=NULL,result_sha256=NULL,
		         reported_duration_ms=NULL,reported_tokens_used=NULL,
		         reported_hardware_temp_c=NULL,
		         verification_outcome=NULL,verified_at=NULL
		    FROM candidates c
		   WHERE t.id=c.id AND t.status='verifying'
		     AND NOT EXISTS (
		       SELECT 1 FROM verification_work w
		        WHERE w.task_id=t.id AND w.attempt=COALESCE(t.retry_count,0)
		     )
		  RETURNING t.id,t.job_id,c.abandoned_attempt
		)
		SELECT id,job_id,abandoned_attempt FROM requeued
		 ORDER BY job_id,id`)
	if err != nil {
		return 0, err
	}
	type recovered struct {
		taskID  uuid.UUID
		jobID   uuid.UUID
		attempt int
	}
	var recoveredRows []recovered
	for rows.Next() {
		var r recovered
		if err := rows.Scan(&r.taskID, &r.jobID, &r.attempt); err != nil {
			rows.Close()
			return 0, err
		}
		recoveredRows = append(recoveredRows, r)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return 0, err
	}
	rows.Close()

	jobs := make([]uuid.UUID, 0, len(recoveredRows))
	for _, r := range recoveredRows {
		jobs = append(jobs, r.jobID)
		detail, _ := json.Marshal(map[string]any{
			"policy":            "retry_without_settlement_v1",
			"reason":            "missing_verification_work",
			"recovered_attempt": r.attempt,
		})
		if err := insertEventTx(ctx, tx, r.jobID, &r.taskID, "verification_recovered",
			"Verification recovery retried this chunk; no result was accepted and no charge was created.", detail); err != nil {
			return 0, err
		}
	}
	if len(jobs) > 0 {
		if _, err := tx.Exec(ctx, `
			UPDATE jobs SET status='running'
			 WHERE id=ANY($1) AND status IN ('queued','verifying')`, jobs); err != nil {
			return 0, err
		}
	}
	if err := tx.Commit(ctx); err != nil {
		return 0, err
	}
	return int64(len(recoveredRows)), nil
}

type StalePinnedTiebreak struct {
	TaskID       uuid.UUID
	JobID        uuid.UUID
	AnchorTaskID uuid.UUID
	PinnedWorker uuid.UUID
	HWClass      string
	Engine       string
	BuildHash    string
	JobType      string
	ModelRef     string
	MinMemoryGB  float32
	ChunkIndex   int
	ClaimedAt    time.Time
}

func (s *Store) StalePinnedTiebreaks(ctx context.Context, olderThan time.Duration, limit int) ([]StalePinnedTiebreak, error) {
	if olderThan <= 0 || limit <= 0 {
		return nil, fmt.Errorf("stale pinned tiebreak query requires positive age and limit")
	}
	rows, err := s.pool.Query(ctx, `
		SELECT t.id,t.job_id,t.hedged_from,t.claimed_by,
		       COALESCE(t.verification_hw_class,''),COALESCE(t.verification_engine,''),
		       COALESCE(t.verification_build_hash,''),
		       j.job_type,COALESCE(j.model_ref,''),COALESCE(j.min_memory_gb,0),
		       COALESCE(t.chunk_index,0),t.claimed_at
		  FROM tasks t JOIN jobs j ON j.id=t.job_id
		 WHERE t.status IN ('queued','retrying')
		   AND t.is_redundancy=true AND t.hedged_from IS NOT NULL
		   AND t.claimed_by IS NOT NULL AND t.claimed_at IS NOT NULL
		   AND t.started_at IS NULL
		   AND t.claimed_at < now()-make_interval(secs=>$1::double precision)
		   AND j.status IN ('running','verifying')
		 ORDER BY t.claimed_at,t.id LIMIT $2`, olderThan.Seconds(), limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []StalePinnedTiebreak
	for rows.Next() {
		var item StalePinnedTiebreak
		if err := rows.Scan(&item.TaskID, &item.JobID, &item.AnchorTaskID, &item.PinnedWorker,
			&item.HWClass, &item.Engine, &item.BuildHash,
			&item.JobType, &item.ModelRef, &item.MinMemoryGB, &item.ChunkIndex, &item.ClaimedAt); err != nil {
			return nil, err
		}
		out = append(out, item)
	}
	return out, rows.Err()
}

type tiebreakVerificationClass struct {
	HWClass   string
	Engine    string
	BuildHash string
}

func frozenTiebreakClassForAnchorTx(ctx context.Context, tx pgx.Tx, anchorTaskID uuid.UUID) (tiebreakVerificationClass, error) {
	var out tiebreakVerificationClass
	err := tx.QueryRow(ctx, `
		SELECT COALESCE((
		         SELECT NULLIF(vw.input_snapshot->>'hw_class','')
		           FROM verification_work vw WHERE vw.task_id=anchor.id
		          ORDER BY vw.attempt DESC LIMIT 1
		       ),NULLIF(COALESCE(anchor.execution_hw_class,''),''),''),
		       COALESCE((
		         SELECT vw.input_snapshot->>'engine'
		           FROM verification_work vw WHERE vw.task_id=anchor.id
		          ORDER BY vw.attempt DESC LIMIT 1
		       ),COALESCE(anchor.execution_engine,'')),
		       COALESCE((
		         SELECT vw.input_snapshot->>'build_hash'
		           FROM verification_work vw WHERE vw.task_id=anchor.id
		          ORDER BY vw.attempt DESC LIMIT 1
		       ),COALESCE(anchor.execution_build_hash,''))
		  FROM tasks anchor
		 WHERE anchor.id=$1`, anchorTaskID).
		Scan(&out.HWClass, &out.Engine, &out.BuildHash)
	if err != nil {
		return out, err
	}
	if out.HWClass == "" {
		return out, errors.New("tiebreak anchor has no frozen hardware class")
	}
	return out, nil
}

func tiebreakPeerClaimEligibleTx(ctx context.Context, tx pgx.Tx, taskID, jobID, peer uuid.UUID) (bool, error) {
	var eligible bool
	err := tx.QueryRow(ctx, `
		SELECT EXISTS (
		 SELECT 1
		   FROM tasks t
		   JOIN jobs j ON j.id=t.job_id
		   JOIN workers nw ON nw.id=$3
		   JOIN suppliers ns ON ns.id=nw.supplier_id
		  WHERE t.id=$1 AND t.job_id=$2
		    AND t.status IN ('queued','retrying') AND t.started_at IS NULL
		    AND t.is_redundancy=true AND t.hedged_from IS NOT NULL
		    AND j.status IN ('running','verifying')
		    AND NULLIF(COALESCE(t.verification_hw_class,''),'') IS NOT NULL
		    AND nw.hw_class=t.verification_hw_class
		    AND (COALESCE(t.verification_engine,'')=''
		         OR COALESCE(nw.engine,'')=t.verification_engine)
		    AND (COALESCE(t.verification_build_hash,'')=''
		         OR COALESCE(nw.build_hash,'')=t.verification_build_hash)
		    AND nw.last_seen_at>now()-interval '60 seconds'
		    AND ns.status='active' AND NOT COALESCE(nw.throttled,false)
		    AND COALESCE(nw.effective_memory_gb,nw.memory_gb,0)>=COALESCE(j.min_memory_gb,0)
		    AND (j.hw_classes IS NULL OR nw.hw_class=ANY(j.hw_classes))
		    AND EXISTS (
		      SELECT 1 FROM worker_authorized_capabilities wac
		       WHERE wac.worker_id=nw.id AND wac.job_type=j.job_type
		         AND wac.model_ref=COALESCE(j.model_ref,'') AND wac.matrix_sha256=$4
		    )
		    AND (j.data_residency IS NULL OR ns.data_country=ANY(j.data_residency))
		    AND COALESCE(j.min_reputation,0)<=ns.reputation
		    AND (j.tier<>'trusted' OR (ns.reputation>=0.80 AND ns.completed_tasks>=500))
		    AND COALESCE(j.offered_rate_usd_hr,1e9)>=COALESCE(nw.min_payout_usd_hr,0)
		    AND (j.max_usd IS NULL OR (
		      (SELECT COALESCE(SUM(-le.amount_usd),0) FROM ledger_entries le
		        WHERE le.kind='buyer_charge'
		          AND le.task_id IN (SELECT id FROM tasks WHERE job_id=j.id))
		      + (SELECT COUNT(*) FROM tasks inflight
		          WHERE inflight.job_id=j.id AND inflight.status IN ('running','verifying'))
		        * (SELECT p.buyer_charge_per_task_usd FROM job_economic_plans p WHERE p.job_id=j.id)
		      + (SELECT p.buyer_charge_per_task_usd+p.sla_premium_usd
		           FROM job_economic_plans p WHERE p.job_id=j.id)
		    )<=j.max_usd)
		    AND NOT EXISTS (
		      SELECT 1
		        FROM (
		          SELECT prior.execution_worker_id AS worker_id,
		                 prior.execution_supplier_id AS supplier_id
		            FROM tasks prior
		           WHERE prior.job_id=t.job_id
		             AND COALESCE(prior.chunk_index,0)=COALESCE(t.chunk_index,0)
		             AND prior.id<>t.id AND prior.execution_worker_id IS NOT NULL
		          UNION ALL
		          SELECT work.worker_id,work.supplier_id
		            FROM verification_work work
		            JOIN tasks committed ON committed.id=work.task_id
		           WHERE committed.job_id=t.job_id
		             AND COALESCE(committed.chunk_index,0)=COALESCE(t.chunk_index,0)
		          UNION ALL
		          SELECT history.worker_id,history.supplier_id
		            FROM task_execution_history history
		            JOIN tasks attempted ON attempted.id=history.task_id
		           WHERE attempted.job_id=t.job_id
		             AND COALESCE(attempted.chunk_index,0)=COALESCE(t.chunk_index,0)
		        ) executed
		       WHERE executed.worker_id=nw.id OR executed.supplier_id=nw.supplier_id
		    )
		)`, taskID, jobID, peer, generatedRuntimeMatrixSHA256).Scan(&eligible)
	return eligible, err
}

func (s *Store) PinnedTiebreakExclusions(ctx context.Context, item StalePinnedTiebreak) (anchor uuid.UUID, workers, suppliers []uuid.UUID, err error) {
	err = s.pool.QueryRow(ctx, `
		SELECT COALESCE(vw.worker_id,t.execution_worker_id)
		  FROM tasks t
		  LEFT JOIN verification_work vw ON vw.task_id=t.id AND vw.attempt=t.retry_count
		 WHERE t.id=$1 AND t.job_id=$2
		   AND COALESCE(vw.worker_id,t.execution_worker_id) IS NOT NULL`, item.AnchorTaskID, item.JobID).Scan(&anchor)
	if err != nil {
		return uuid.Nil, nil, nil, err
	}
	rows, err := s.pool.Query(ctx, `
		SELECT DISTINCT worker_id,supplier_id
		  FROM (
		    SELECT t.execution_worker_id AS worker_id,t.execution_supplier_id AS supplier_id
		      FROM tasks t
		     WHERE t.job_id=$1 AND COALESCE(t.chunk_index,0)=$2
		       AND t.id<>$3 AND t.execution_worker_id IS NOT NULL
		    UNION ALL
		    SELECT vw.worker_id,vw.supplier_id
		      FROM verification_work vw JOIN tasks t ON t.id=vw.task_id
		     WHERE t.job_id=$1 AND COALESCE(t.chunk_index,0)=$2 AND t.id<>$3
		    UNION ALL
		    SELECT h.worker_id,h.supplier_id
		      FROM task_execution_history h JOIN tasks t ON t.id=h.task_id
		     WHERE t.job_id=$1 AND COALESCE(t.chunk_index,0)=$2 AND t.id<>$3
		  ) executed
		 WHERE worker_id IS NOT NULL AND supplier_id IS NOT NULL
		 ORDER BY worker_id,supplier_id`, item.JobID, item.ChunkIndex, item.TaskID)
	if err != nil {
		return uuid.Nil, nil, nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var workerID, supplierID uuid.UUID
		if err := rows.Scan(&workerID, &supplierID); err != nil {
			return uuid.Nil, nil, nil, err
		}
		workers = append(workers, workerID)
		suppliers = append(suppliers, supplierID)
	}
	if err := rows.Err(); err != nil {
		return uuid.Nil, nil, nil, err
	}
	workers = append(workers, item.PinnedWorker)
	return anchor, workers, suppliers, nil
}

func (s *Store) ReassignPinnedTiebreak(ctx context.Context, item StalePinnedTiebreak, peer uuid.UUID, olderThan time.Duration) (bool, error) {
	if peer == uuid.Nil || olderThan <= 0 {
		return false, errors.New("pinned tiebreak reassignment requires peer and positive age")
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return false, err
	}
	defer tx.Rollback(ctx)

	var locked uuid.UUID
	err = tx.QueryRow(ctx, `
		SELECT id FROM tasks
		 WHERE id=$1 AND job_id=$2 AND status IN ('queued','retrying')
		   AND is_redundancy=true AND hedged_from=$3
		   AND claimed_by=$4 AND claimed_at=$5 AND started_at IS NULL
		   AND claimed_at < now()-make_interval(secs=>$6::double precision)
		 FOR UPDATE`, item.TaskID, item.JobID, item.AnchorTaskID, item.PinnedWorker,
		item.ClaimedAt, olderThan.Seconds()).Scan(&locked)
	if errors.Is(err, pgx.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	var parentStatus string
	if err := tx.QueryRow(ctx, `SELECT status FROM jobs WHERE id=$1 FOR UPDATE`, item.JobID).Scan(&parentStatus); err != nil {
		return false, err
	}
	if parentStatus != "running" && parentStatus != "verifying" {
		return false, nil
	}

	if peer == item.PinnedWorker {
		return false, ErrNoSupply
	}
	eligible, err := tiebreakPeerClaimEligibleTx(ctx, tx, item.TaskID, item.JobID, peer)
	if err != nil {
		return false, err
	}
	if !eligible {
		return false, ErrNoSupply
	}

	ct, err := tx.Exec(ctx, `
		UPDATE tasks
		   SET claimed_by=$2,claimed_at=now(),worker_id=NULL,visible_at=now()
		 WHERE id=$1 AND claimed_by=$3 AND started_at IS NULL
		   AND status IN ('queued','retrying')`, item.TaskID, peer, item.PinnedWorker)
	if err != nil {
		return false, err
	}
	if ct.RowsAffected() != 1 {
		return false, nil
	}
	if _, err := tx.Exec(ctx, `
		UPDATE jobs SET status='running'
		 WHERE id=$1 AND status='verifying'`, item.JobID); err != nil {
		return false, err
	}
	detail, _ := json.Marshal(map[string]any{
		"policy":          "independent_same_class_reselect_v1",
		"previous_worker": item.PinnedWorker,
		"replacement":     peer,
	})
	if err := insertEventTx(ctx, tx, item.JobID, &item.TaskID, "tiebreak_reassigned",
		"A third-opinion worker did not start; verification was reassigned to another independent compatible worker.", detail); err != nil {
		return false, err
	}
	if err := tx.Commit(ctx); err != nil {
		return false, err
	}
	return true, nil
}
