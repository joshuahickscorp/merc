package main

import (
	"context"
	_ "embed"
	"errors"
	"fmt"
	"log"
	"math"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

// Split out of store.go, which had grown to 5,727 lines across roughly two
// dozen unrelated responsibilities.  Same package, same behaviour: this is a
// file move so that a reviewer can hold one subject at a time and two people
// can edit payouts and job submission without conflicting.

func (s *Store) DeleteOldTaskDurations(ctx context.Context, before time.Time) (int64, error) {
	tag, err := s.pool.Exec(ctx, `DELETE FROM task_durations WHERE created_at < $1`, before)
	if err != nil {
		return 0, err
	}
	return tag.RowsAffected(), nil
}

func (s *Store) AdminForceRequeueTask(ctx context.Context, actor AdminActor, taskID uuid.UUID, reason, correlationRef string) error {
	intent, err := prepareAdminMutation(actor, adminMutationIntent{
		Kind: adminActionTaskRequeued, TargetKind: adminTargetTask,
		TargetID: taskID, Reason: reason, CorrelationRef: correlationRef,
	})
	if err != nil {
		return err
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	if err := revalidateAdminActor(ctx, tx, actor); err != nil {
		return err
	}
	if replay, err := acquireAdminMutationReplay(ctx, tx, actor, intent); err != nil {
		return err
	} else if replay.Found {
		return tx.Commit(ctx)
	}

	var jobID uuid.UUID
	var beforeStatus string
	var beforeRetry int16
	var beforeClaimedBy, beforeWorkerID *uuid.UUID
	if err := tx.QueryRow(ctx, `
		SELECT job_id,status,COALESCE(retry_count,0),claimed_by,worker_id
		  FROM tasks WHERE id=$1 FOR UPDATE`, taskID).Scan(
		&jobID, &beforeStatus, &beforeRetry, &beforeClaimedBy, &beforeWorkerID); errors.Is(err, pgx.ErrNoRows) {
		return errNotFound
	} else if err != nil {
		return err
	}
	if beforeStatus != "running" && beforeStatus != "retrying" {
		return errNotRequeueable
	}
	retryLimit, err := canaryRetryLimit()
	if err != nil {
		return err
	}
	if int(beforeRetry) >= retryLimit {
		return fmt.Errorf("task retry limit %d reached", retryLimit)
	}
	if _, err := tx.Exec(ctx, `
		UPDATE tasks SET status='queued',claimed_by=NULL,claimed_at=NULL,
		                 worker_id=NULL,retry_count=retry_count+1,visible_at=now()
		 WHERE id=$1`, taskID); err != nil {
		return err
	}

	if err := insertAdminMutationAction(ctx, tx, actor, intent, &taskID, nil, nil,
		map[string]any{"job_id": jobID, "status": beforeStatus, "retry_count": beforeRetry,
			"claimed_by": beforeClaimedBy, "worker_id": beforeWorkerID},
		map[string]any{"job_id": jobID, "status": "queued", "retry_count": beforeRetry + 1,
			"claimed_by": nil, "worker_id": nil}); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

type taskRow struct {
	ID                    uuid.UUID
	JobID                 uuid.UUID
	IsHoneypot            bool
	IsRedundancy          bool
	InputRef              string
	ResultKey             string
	ChunkIndex            int
	ExpectedOutputRecords int64 // 0 = explicit legacy/opaque unknown, persisted NULL
}

func (s *Store) QueuedTaskCount(ctx context.Context) (int, error) {
	var n int
	err := s.pool.QueryRow(ctx,
		`SELECT count(*) FROM tasks t JOIN jobs j ON j.id = t.job_id
		 WHERE t.status IN ('queued','retrying')
		   AND t.claimed_by IS NULL
		   AND COALESCE(t.visible_at, t.created_at) <= now()
		   AND j.status NOT IN ('cancelled','failed','complete')
		   AND j.currency = $1`,
		SettlementCurrencyCode(),
	).Scan(&n)
	return n, err
}

func (s *Store) TaskEconomicAmounts(ctx context.Context, taskID uuid.UUID) (buyerCharge, supplierPayout float64, err error) {
	err = s.pool.QueryRow(ctx, `
		SELECT economic_buyer_charge_usd::float8,economic_supplier_payout_usd::float8
		  FROM tasks WHERE id=$1`, taskID).Scan(&buyerCharge, &supplierPayout)
	if errors.Is(err, pgx.ErrNoRows) {
		return 0, 0, errNotFound
	}
	if err != nil {
		return 0, 0, fmt.Errorf("task %s has no frozen economic amounts: %w", taskID, err)
	}
	return buyerCharge, supplierPayout, nil
}

func (s *Store) StartTask(ctx context.Context, taskID, workerID uuid.UUID, claimAttempt int16) error {
	if claimAttempt < 0 {
		return errNotFound
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)

	var claimSupplierID uuid.UUID
	var claimHWClass, claimEngine, claimBuildHash string
	err = tx.QueryRow(ctx, `
		SELECT supplier_id,COALESCE(hw_class,''),COALESCE(engine,''),COALESCE(build_hash,'')
		  FROM workers WHERE id=$1 FOR UPDATE`, workerID).
		Scan(&claimSupplierID, &claimHWClass, &claimEngine, &claimBuildHash)
	if errors.Is(err, pgx.ErrNoRows) {
		return errNotFound
	}
	if err != nil {
		return err
	}
	if strings.TrimSpace(claimHWClass) == "" || strings.TrimSpace(claimEngine) == "" {
		return errNotFound
	}

	var (
		jobID               uuid.UUID
		status              string
		claimedBy           *uuid.UUID
		taskWorkerID        *uuid.UUID
		executionWorkerID   *uuid.UUID
		executionSupplierID *uuid.UUID
		startedAt           *time.Time
		isRedundancy        bool
		hedgedFrom          *uuid.UUID
		attempt             int16
	)
	err = tx.QueryRow(ctx, `
		SELECT job_id,status,claimed_by,worker_id,execution_worker_id,execution_supplier_id,started_at,
		       COALESCE(is_redundancy,false),hedged_from,COALESCE(retry_count,0)
		  FROM tasks WHERE id=$1 AND retry_count=$2 FOR UPDATE`, taskID, claimAttempt).
		Scan(&jobID, &status, &claimedBy, &taskWorkerID, &executionWorkerID, &executionSupplierID, &startedAt,
			&isRedundancy, &hedgedFrom, &attempt)
	if errors.Is(err, pgx.ErrNoRows) {
		return errNotFound
	}
	if err != nil {
		return err
	}
	if claimedBy == nil || *claimedBy != workerID || (status != "queued" && status != "running") {
		return errNotFound
	}

	var parentStatus, jobCurrency string
	if err := tx.QueryRow(ctx, `SELECT status,currency FROM jobs WHERE id=$1 FOR UPDATE`, jobID).
		Scan(&parentStatus, &jobCurrency); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return errNotFound
		}
		return err
	}
	if err := RequireSettlementCurrency(jobCurrency); err != nil {
		return fmt.Errorf("job %s cannot start under this deployment: %w", jobID, err)
	}
	if parentStatus != "queued" && parentStatus != "running" && parentStatus != "verifying" {
		return errNotFound
	}

	dynamicTiebreak := isRedundancy && hedgedFrom != nil
	if status == "running" {
		if taskWorkerID == nil || *taskWorkerID != workerID ||
			executionWorkerID == nil || *executionWorkerID != workerID || executionSupplierID == nil {
			return errNotFound
		}
		if dynamicTiebreak {
			var historyExact bool
			if err := tx.QueryRow(ctx, `
				SELECT EXISTS (
				 SELECT 1 FROM task_execution_history h
				  WHERE h.task_id=$1 AND h.attempt=$2 AND h.worker_id=$3
				    AND h.supplier_id=$4
				)`, taskID, attempt, workerID, *executionSupplierID).Scan(&historyExact); err != nil {
				return err
			}
			if !historyExact {
				return errNotFound
			}
		}
		if _, err := tx.Exec(ctx, `UPDATE jobs SET status='running' WHERE id=$1 AND status='queued'`, jobID); err != nil {
			return err
		}
		return tx.Commit(ctx)
	}

	if taskWorkerID != nil && *taskWorkerID != workerID {
		return errNotFound
	}
	if dynamicTiebreak {
		if startedAt != nil {
			return errNotFound
		}
		eligible, err := tiebreakPeerClaimEligibleTx(ctx, tx, taskID, jobID, workerID)
		if err != nil {
			return err
		}
		if !eligible {
			return errNotFound
		}
		ct, err := tx.Exec(ctx, `
			INSERT INTO task_execution_history (task_id,attempt,worker_id,supplier_id)
			VALUES ($1,$2,$3,$4)
			ON CONFLICT (task_id,attempt,worker_id) DO NOTHING`,
			taskID, attempt, workerID, claimSupplierID)
		if err != nil {
			return err
		}
		if ct.RowsAffected() != 1 {
			return errNotFound
		}
	}

	ct, err := tx.Exec(ctx, `
		UPDATE tasks SET status='running',started_at=now(),worker_id=$2,
		       execution_worker_id=$2,execution_supplier_id=$3,
		       execution_hw_class=$4,execution_engine=$5,execution_build_hash=$6
		 WHERE id=$1 AND claimed_by=$2 AND retry_count=$7 AND status='queued'`,
		taskID, workerID, claimSupplierID, claimHWClass, claimEngine, claimBuildHash, claimAttempt)
	if err != nil {
		return err
	}
	if ct.RowsAffected() != 1 {
		return errNotFound
	}
	if _, err := tx.Exec(ctx, `UPDATE jobs SET status='running' WHERE id=$1 AND status='queued'`, jobID); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

type CommitTaskInfo struct {
	TaskID                   uuid.UUID
	JobID                    uuid.UUID
	WorkerID                 uuid.UUID
	SupplierID               uuid.UUID
	IsHoneypot               bool
	IsRedundancy             bool
	HWClass                  string
	engine                   string
	buildHash                string
	jobType                  string // parent job's job_type, for honeypot answer lookup
	jobMaxTokens             uint32 // bounded projection of jobs.job_type_spec.max_tokens
	resultMaxBytes           int64
	InputRef                 string // this task's input chunk key (honeypot answer lookup)
	ResultKey                string // canonical server-side result key (verification fetch)
	ModelRef                 string // parent job's model_ref (tiebreak peer selection)
	MinMemoryGB              float32
	ChunkIndex               int // this task's chunk position (tiebreak pairing + N-way vote)
	SplitSize                int
	ExpectedOutputRecords    int64
	Attempt                  int16
	DurationMS               uint64
	TokensUsed               uint64
	ResultSHA256             string
	hardwareTempC            *float32
	verificationCheckSampled *bool
	peerSupplierID           uuid.UUID
	peerEngine               string
	peerBuildHash            string
}

func (s *Store) CompleteTaskTx(ctx context.Context, taskID, workerID uuid.UUID, c TaskCommit) (*CommitTaskInfo, error) {
	return s.completeTaskTx(ctx, taskID, workerID, c, nil)
}

func (s *Store) completeTaskTx(ctx context.Context, taskID, workerID uuid.UUID, c TaskCommit, probe recoveryBoundaryProbe) (*CommitTaskInfo, error) {
	if c.Attempt < 0 || c.DurationMS > math.MaxInt64 || c.TokensUsed > math.MaxInt64 {
		return nil, fmt.Errorf("reported duration/tokens exceed durable range")
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback(ctx)

	var stagedJobID uuid.UUID
	var stagedResultKey string
	err = tx.QueryRow(ctx, `
		SELECT job_id,COALESCE(result_key,'')
		  FROM tasks
		 WHERE id=$1 AND claimed_by=$2 AND execution_worker_id=$2 AND retry_count=$3
		   AND status IN ('running','queued','verifying')
		 FOR UPDATE`, taskID, workerID, c.Attempt).Scan(&stagedJobID, &stagedResultKey)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, errNotFound
	}
	if err != nil {
		return nil, err
	}
	expectedResultKey := taskAttemptResultKey(stagedJobID, taskID, c.Attempt)
	if validateTaskAttemptResultKey(stagedJobID, taskID, c.Attempt, stagedResultKey) != nil ||
		c.ResultKey != expectedResultKey {
		return nil, errNotFound
	}

	resultSHA256 := nullSHA256Hex(c.ResultSHA256)

	ct, err := tx.Exec(ctx,
		`UPDATE tasks
		   SET status = 'verifying',
		       completed_at = CASE WHEN status='verifying' THEN completed_at ELSE NULL END,
		       result_ref = CASE WHEN status='verifying' THEN result_ref ELSE result_key END,
		       worker_id = CASE WHEN status='verifying' THEN worker_id ELSE $2 END,
		       result_sha256 = CASE WHEN status='verifying' THEN result_sha256 ELSE $4 END,
		       reported_duration_ms = CASE WHEN status='verifying' THEN reported_duration_ms ELSE $5 END,
		       reported_tokens_used = CASE WHEN status='verifying' THEN reported_tokens_used ELSE $6 END,
		       reported_hardware_temp_c = CASE WHEN status='verifying' THEN reported_hardware_temp_c ELSE $7 END,
		       verification_outcome = CASE WHEN status='verifying' THEN verification_outcome ELSE NULL END,
		       verified_at = CASE WHEN status='verifying' THEN verified_at ELSE NULL END
		 WHERE id = $1 AND claimed_by = $2 AND execution_worker_id = $2 AND retry_count = $8
		   AND result_key = $3
		   AND (status IN ('running','queued') OR (status = 'verifying' AND worker_id = $2))`,
		taskID, workerID, c.ResultKey, resultSHA256, int64(c.DurationMS), int64(c.TokensUsed), c.HardwareTempC, c.Attempt)
	if err != nil {
		return nil, err
	}
	if ct.RowsAffected() == 0 {
		return nil, errNotFound
	}
	reachRecoveryBoundary(ctx, probe, BoundaryCommitAfterTaskProjection)

	var info CommitTaskInfo
	var jobMaxTokens int64
	info.TaskID = taskID
	err = tx.QueryRow(ctx,
		`SELECT t.job_id, t.is_honeypot, t.is_redundancy,
		        COALESCE(t.input_ref,''),
		        COALESCE(t.result_key,''),
		        t.execution_worker_id,t.execution_supplier_id,t.execution_hw_class,
		        t.execution_engine,t.execution_build_hash,j.job_type,
		        COALESCE((j.job_type_spec->>'max_tokens')::bigint,0),
		        COALESCE(j.model_ref,''), COALESCE(j.min_memory_gb,0),
		        COALESCE(t.chunk_index,0), COALESCE(j.split_size,0),
		        COALESCE(t.expected_output_records,0),
		        COALESCE(t.retry_count,0), COALESCE(t.result_sha256,'')
	 FROM tasks t JOIN jobs j ON j.id = t.job_id
	 WHERE t.id = $1 AND t.execution_worker_id=$2`,
		taskID, workerID,
	).Scan(&info.JobID, &info.IsHoneypot, &info.IsRedundancy, &info.InputRef,
		&info.ResultKey, &info.WorkerID, &info.SupplierID, &info.HWClass, &info.engine, &info.buildHash, &info.jobType, &jobMaxTokens,
		&info.ModelRef, &info.MinMemoryGB, &info.ChunkIndex, &info.SplitSize,
		&info.ExpectedOutputRecords, &info.Attempt, &info.ResultSHA256)
	if err != nil {
		return nil, err
	}
	if jobMaxTokens < 0 || jobMaxTokens > int64(^uint32(0)) {
		return nil, fmt.Errorf("job max_tokens %d exceeds artifact-policy range", jobMaxTokens)
	}
	info.jobMaxTokens = uint32(jobMaxTokens)
	info.resultMaxBytes = verificationArtifactMaxBytesForRecords(
		info.jobType, info.ExpectedOutputRecords, info.SplitSize, info.jobMaxTokens,
	)

	var parentStatus string
	if err := tx.QueryRow(ctx, `SELECT status FROM jobs WHERE id=$1 FOR UPDATE`, info.JobID).Scan(&parentStatus); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, errNotFound
		}
		return nil, err
	}
	if parentStatus != "queued" && parentStatus != "running" && parentStatus != "verifying" {
		return nil, errNotFound
	}
	reachRecoveryBoundary(ctx, probe, BoundaryCommitAfterParentFence)

	if _, err := tx.Exec(ctx,
		`UPDATE jobs j SET status = 'verifying'
		 WHERE j.id = $1 AND j.status IN ('queued','running')
		   AND NOT EXISTS (
		     SELECT 1 FROM tasks t
		     WHERE t.job_id = j.id AND t.status IN ('queued','retrying','running')
		   )`, info.JobID); err != nil {
		return nil, err
	}
	reachRecoveryBoundary(ctx, probe, BoundaryCommitAfterJobProjection)

	info.DurationMS = c.DurationMS
	info.TokensUsed = c.TokensUsed
	info.hardwareTempC = c.HardwareTempC
	snapshot, err := verificationWorkSnapshotFromCommit(&info, c)
	if err != nil {
		return nil, err
	}
	if err := createVerificationWorkTx(ctx, tx, snapshot); err != nil {
		return nil, err
	}
	reachRecoveryBoundary(ctx, probe, BoundaryCommitAfterWorkInsert)

	reachRecoveryBoundary(ctx, probe, BoundaryCommitBeforeDBCommit)
	if err := tx.Commit(ctx); err != nil {
		return nil, err
	}
	reachRecoveryBoundary(ctx, probe, BoundaryCommitAfterDBCommit)
	// Warm-prefix bookkeeping is after the durable commit on purpose: it is a
	// routing hint, never part of the commit contract. A failure here must not
	// roll back a finished task, and the worker has materialised the prefix
	// whether or not we record it.
	if err := s.markWorkerWarmForJob(ctx, workerID, info.JobID); err != nil {
		log.Printf("prefix warmth: worker %s job %s: %v", workerID, info.JobID, err)
	}
	return &info, nil
}

func (s *Store) JobAllTasksDone(ctx context.Context, jobID uuid.UUID) (bool, error) {
	var done bool
	err := s.pool.QueryRow(ctx,
		`SELECT j.task_count > 0
		        AND NOT EXISTS (
		          SELECT 1 FROM tasks t
		          WHERE t.job_id = j.id AND t.status NOT IN ('complete','failed')
		        )
		        AND EXISTS (SELECT 1 FROM tasks t WHERE t.job_id = j.id AND t.status = 'complete')
		 FROM jobs j WHERE j.id = $1`,
		jobID,
	).Scan(&done)
	if errors.Is(err, pgx.ErrNoRows) {
		return false, errNotFound
	}
	return done, err
}

func (s *Store) RequeueTask(ctx context.Context, taskID uuid.UUID) error {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)

	var failedWorker *uuid.UUID
	var jobID uuid.UUID
	var priorRetries int
	if err := tx.QueryRow(ctx,
		`SELECT COALESCE(worker_id, claimed_by), job_id, retry_count FROM tasks WHERE id = $1 FOR UPDATE`,
		taskID,
	).Scan(&failedWorker, &jobID, &priorRetries); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return errNotFound
		}
		return err
	}
	backoff := requeueBackoff(priorRetries)

	if _, err := tx.Exec(ctx,
		`UPDATE tasks
		   SET status = 'retrying', claimed_by = NULL, claimed_at = NULL,
		       worker_id = NULL, retry_count = retry_count + 1,
		       visible_at = now() + make_interval(secs => $2),
		       excluded_worker = $3,
		       excluded_until  = now() + make_interval(secs => $4)
		 WHERE id = $1`,
		taskID, backoff.Seconds(), failedWorker,
		(backoff + requeueExclusionGrace).Seconds(),
	); err != nil {
		return err
	}
	if _, err := tx.Exec(ctx,
		`UPDATE jobs SET status = 'running' WHERE id = $1 AND status = 'verifying'`, jobID); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

type ChunkResult struct {
	TaskID     uuid.UUID
	WorkerID   uuid.UUID
	SupplierID uuid.UUID
	ResultRef  string
	Artifact   *VerificationArtifact
	Engine     string
	BuildHash  string
}

func (s *Store) ChunkResults(ctx context.Context, jobID uuid.UUID, chunkIndex int) ([]ChunkResult, error) {
	rows, err := s.pool.Query(ctx,
		`SELECT t.id,COALESCE(vw.worker_id,t.execution_worker_id),
		        COALESCE(vw.supplier_id,t.execution_supplier_id),
		        t.result_ref,
		        COALESCE(vw.input_snapshot->>'engine',t.execution_engine,''),
		        COALESCE(vw.input_snapshot->>'build_hash',t.execution_build_hash,''),
		        vw.artifact_key,vw.artifact_sha256,vw.artifact_bytes
		 FROM tasks t
		 LEFT JOIN verification_work vw
		   ON vw.task_id=t.id AND vw.attempt=t.retry_count AND vw.status='terminal'
		 WHERE t.job_id = $1 AND COALESCE(t.chunk_index,0) = $2
		   AND t.status = 'complete' AND t.is_honeypot = false
		   AND t.result_ref IS NOT NULL AND t.result_ref <> ''
		   AND COALESCE(vw.worker_id,t.execution_worker_id) IS NOT NULL
		   AND COALESCE(vw.supplier_id,t.execution_supplier_id) IS NOT NULL
		 ORDER BY t.completed_at ASC NULLS LAST, t.id ASC`,
		jobID, chunkIndex)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []ChunkResult
	for rows.Next() {
		var cr ChunkResult
		var artifactKey, artifactSHA *string
		var artifactBytes *int64
		if err := rows.Scan(&cr.TaskID, &cr.WorkerID, &cr.SupplierID, &cr.ResultRef,
			&cr.Engine, &cr.BuildHash, &artifactKey, &artifactSHA, &artifactBytes); err != nil {
			return nil, err
		}
		if artifactKey != nil || artifactSHA != nil || artifactBytes != nil {
			if artifactKey == nil || artifactSHA == nil || artifactBytes == nil {
				return nil, fmt.Errorf("task %s has an incomplete terminal verification artifact tuple", cr.TaskID)
			}
			cr.Artifact = &VerificationArtifact{Key: *artifactKey, SHA256: *artifactSHA, Bytes: *artifactBytes}
		}
		out = append(out, cr)
	}
	return out, rows.Err()
}

func (s *Store) InsertTiebreakTask(ctx context.Context, jobID, primaryTaskID, peerWorker uuid.UUID, inputRef string, chunkIndex int) (uuid.UUID, error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return uuid.Nil, err
	}
	defer tx.Rollback(ctx)
	buyerCharge, supplierPayout, err := consumeEconomicReserveTx(ctx, tx, jobID)
	if err != nil {
		if errors.Is(err, ErrEconomicReserveExhausted) {
			var existing uuid.UUID
			qerr := tx.QueryRow(ctx, `
				SELECT id FROM tasks
				 WHERE job_id=$1 AND COALESCE(chunk_index,0)=$2
				   AND hedged_from IS NOT NULL AND is_redundancy=true
				 ORDER BY created_at LIMIT 1`, jobID, chunkIndex).Scan(&existing)
			if qerr == nil {
				return existing, nil
			}
			if !errors.Is(qerr, pgx.ErrNoRows) {
				return uuid.Nil, qerr
			}
		}
		return uuid.Nil, err
	}
	var existing uuid.UUID
	err = tx.QueryRow(ctx, `
		SELECT id FROM tasks
		 WHERE job_id=$1 AND COALESCE(chunk_index,0)=$2
		   AND hedged_from IS NOT NULL AND is_redundancy=true
		 ORDER BY created_at LIMIT 1`, jobID, chunkIndex).Scan(&existing)
	if err == nil {
		return existing, nil
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return uuid.Nil, err
	}
	frozenClass, err := frozenTiebreakClassForAnchorTx(ctx, tx, primaryTaskID)
	if err != nil {
		return uuid.Nil, err
	}

	id := uuid.New()
	resultKey := taskAttemptResultKey(jobID, id, 0)
	if _, err := tx.Exec(ctx,
		`INSERT INTO tasks
		   (id, job_id, status, is_honeypot, is_redundancy, retry_count,
		    input_ref, result_key, chunk_index, hedged_from,expected_output_records,
		    verification_hw_class,verification_engine,verification_build_hash,
		    claimed_by, claimed_at, visible_at,
		    economic_buyer_charge_usd,economic_supplier_payout_usd)
		 VALUES ($1,$2,'queued',false,true,0,$3,$4,$5,$6,
		         (SELECT expected_output_records FROM tasks WHERE id=$6),
		         $7,$8,$9,$10,now(),now(),$11,$12)`,
		id, jobID, inputRef, resultKey, chunkIndex, primaryTaskID,
		frozenClass.HWClass, frozenClass.Engine, frozenClass.BuildHash,
		peerWorker, buyerCharge, supplierPayout,
	); err != nil {
		return uuid.Nil, err
	}
	if _, err := tx.Exec(ctx,
		`UPDATE jobs
		    SET task_count = task_count + 1,
		        status = CASE WHEN status='verifying' THEN 'running' ELSE status END
		  WHERE id = $1`, jobID); err != nil {
		return uuid.Nil, err
	}
	if err := tx.Commit(ctx); err != nil {
		return uuid.Nil, err
	}
	return id, nil
}

func (s *Store) ClawbackTaskCredit(ctx context.Context, supplierID, taskID uuid.UUID) error {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)

	var (
		creditID       uuid.UUID
		credited       float64
		creditState    string
		payoutRef      string
		creditCurrency string
		outcomeUnknown bool
	)
	err = tx.QueryRow(ctx, `
		SELECT le.id,le.amount_usd::float8,le.payout_status,
		       COALESCE(le.payout_ref,''),le.currency,COALESCE(op.outcome_unknown,false)
		  FROM ledger_entries le
		  LEFT JOIN supplier_payout_operations op ON op.ledger_entry_id=le.id
		 WHERE le.supplier_id=$1 AND le.task_id=$2
		   AND le.kind='supplier_credit' AND le.amount_usd>0
		 ORDER BY le.created_at,le.id LIMIT 1
		 FOR UPDATE OF le`, supplierID, taskID).
		Scan(&creditID, &credited, &creditState, &payoutRef, &creditCurrency, &outcomeUnknown)
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return err
	}
	if err == nil && credited > 0 {
		cb := clawbackEntry(supplierID, taskID, credited)
		cb.Currency = creditCurrency
		if _, err := insertLedgerEntryOnTaskConflictDoNothingTx(ctx, tx, ledgerInsertFromEntry(cb)); err != nil {
			return err
		}
		reversalRequired := payoutRef != "" || outcomeUnknown ||
			creditState == PayoutSending || creditState == PayoutOutcomeUnknown ||
			creditState == PayoutReleased || creditState == PayoutExported ||
			creditState == PayoutReversalRequired
		nextState := PayoutClawedBack
		if reversalRequired {
			nextState = PayoutReversalRequired
		}
		if _, err := tx.Exec(ctx,
			`UPDATE ledger_entries SET payout_status=$2 WHERE id=$1`, creditID, nextState); err != nil {
			return err
		}
		opState := nextState
		if _, err := tx.Exec(ctx, `
			UPDATE supplier_payout_operations
			   SET status=$2,updated_at=now(),
			       last_error=CASE WHEN $2='reversal_required'
			                       THEN 'confirmed clawback requires external recovery'
			                       ELSE last_error END
			 WHERE ledger_entry_id=$1`, creditID, opState); err != nil {
			return err
		}
	}
	if _, err := tx.Exec(ctx,
		`UPDATE tasks SET verification_outcome='clawed_back', verified_at=now() WHERE id=$1`, taskID); err != nil {
		return err
	}
	if _, err := tx.Exec(ctx,
		`INSERT INTO task_verdict_resolutions (effect_id,task_id,source_task_id,kind)
		 VALUES ($1,$2,$2,'clawed_back') ON CONFLICT (effect_id) DO NOTHING`,
		verificationResolutionID(taskID, "clawed_back", taskID), taskID); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

func (s *Store) JobTaskReceipts(ctx context.Context, jobID uuid.UUID) ([]TaskReceipt, error) {
	rows, err := s.pool.Query(ctx,
		`SELECT COALESCE(t.chunk_index,0), t.status, t.is_honeypot,
		        COALESCE(vw.input_snapshot->>'engine',t.execution_engine,''),
		        COALESCE(vw.input_snapshot->>'build_hash',t.execution_build_hash,''),
		        COALESCE((SELECT ve.kind FROM verification_events ve
		                  WHERE ve.task_id = t.id ORDER BY ve.created_at DESC LIMIT 1), ''),
		        COALESCE(t.verification_outcome,''),
		        COALESCE(t.runtime_cell_id,''), COALESCE(t.runtime_id,''),
		        COALESCE(t.runtime_matrix_sha256,''), COALESCE(t.model_kind,'')
		 FROM tasks t
		 LEFT JOIN verification_work vw ON vw.task_id=t.id AND vw.attempt=t.retry_count
		 WHERE t.job_id = $1
		 ORDER BY COALESCE(t.chunk_index,0), t.id`,
		jobID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []TaskReceipt
	for rows.Next() {
		var (
			chunk                        int
			status                       string
			isHoneypot                   bool
			engine, build                string
			kind, verdict                string
			cellID, runtimeID, matrixSHA string
			modelKind                    string
		)
		if err := rows.Scan(&chunk, &status, &isHoneypot, &engine, &build, &kind, &verdict,
			&cellID, &runtimeID, &matrixSHA, &modelKind); err != nil {
			return nil, err
		}
		out = append(out, taskReceiptRowWithRuntime(chunk, status, isHoneypot, engine, build,
			kind, verdict, cellID, runtimeID, matrixSHA, modelKind))
	}
	return out, rows.Err()
}

func (s *Store) JobHasPendingTasks(ctx context.Context, jobID uuid.UUID) (bool, error) {
	var n int
	err := s.pool.QueryRow(ctx,
		`SELECT count(*) FROM tasks WHERE job_id = $1 AND status IN ('queued','retrying','running')`,
		jobID).Scan(&n)
	return n > 0, err
}

func (s *Store) TaskHasClawback(ctx context.Context, taskID uuid.UUID) (bool, error) {
	var exists bool
	err := s.pool.QueryRow(ctx,
		`SELECT EXISTS(SELECT 1 FROM ledger_entries WHERE kind = 'clawback' AND task_id = $1)`,
		taskID).Scan(&exists)
	return exists, err
}

type StaleTask struct {
	ID         uuid.UUID
	JobID      uuid.UUID
	RetryCount int16
}

func (s *Store) StaleRunningTasks(ctx context.Context, timeout time.Duration, limit int) ([]StaleTask, error) {
	rows, err := s.pool.Query(ctx,
		`SELECT id, job_id, retry_count FROM tasks
		 WHERE status = 'running' AND claimed_at IS NOT NULL
		   AND claimed_at < now() - make_interval(secs => $1)
		 ORDER BY claimed_at ASC LIMIT $2`,
		timeout.Seconds(), limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []StaleTask
	for rows.Next() {
		var t StaleTask
		if err := rows.Scan(&t.ID, &t.JobID, &t.RetryCount); err != nil {
			return nil, err
		}
		out = append(out, t)
	}
	return out, rows.Err()
}

func (s *Store) StragglerTasks(ctx context.Context, after, throttledAfter time.Duration, maxInFlight, limit int) ([]Straggler, error) {
	rows, err := s.pool.Query(ctx,
		`SELECT t.id, t.job_id, t.worker_id, j.job_type, COALESCE(j.model_ref,''),
		        COALESCE(t.input_ref,''), COALESCE(t.chunk_index,0), COALESCE(j.min_memory_gb,0),
		        COALESCE(w.throttled, false)
		 FROM tasks t JOIN jobs j ON j.id = t.job_id
		 LEFT JOIN workers w ON w.id = t.worker_id
		 WHERE t.status = 'running'
		   AND t.is_honeypot = false AND t.is_redundancy = false
		   AND t.hedged_from IS NULL
		   AND t.started_at IS NOT NULL
		   AND (
		     t.started_at < now() - make_interval(secs => $1)
		     OR (
		       COALESCE(w.throttled, false)
		       AND t.started_at < now() - make_interval(secs => $2)
		     )
		   )
		   AND j.status = 'running'
		   -- this chunk is not already hedged:
		   AND NOT EXISTS (
		     SELECT 1 FROM tasks h
		     WHERE h.job_id = t.job_id AND COALESCE(h.chunk_index,0) = COALESCE(t.chunk_index,0)
		       AND h.hedged_from IS NOT NULL AND h.is_redundancy = false
		       AND h.status NOT IN ('failed','cancelled')
		   )
		   -- and the job is under its in-flight hedge cap:
		   AND (
		     SELECT count(*) FROM tasks h2
		     WHERE h2.job_id = t.job_id AND h2.hedged_from IS NOT NULL
		       AND h2.is_redundancy = false AND h2.status IN ('queued','running')
		   ) < $3
		 ORDER BY t.started_at ASC
		 LIMIT $4`,
		after.Seconds(), throttledAfter.Seconds(), maxInFlight, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Straggler
	for rows.Next() {
		var s Straggler
		var throttled bool
		if err := rows.Scan(&s.TaskID, &s.JobID, &s.WorkerID, &s.JobType, &s.ModelRef,
			&s.InputRef, &s.ChunkIndex, &s.MinMemGB, &throttled); err != nil {
			return nil, err
		}
		s.ThrottledHedge = throttled
		out = append(out, s)
	}
	return out, rows.Err()
}

func (s *Store) EndgameTailTasks(ctx context.Context, minRun time.Duration, maxInFlight, limit int) ([]Straggler, error) {
	rows, err := s.pool.Query(ctx,
		`SELECT t.id, t.job_id, t.worker_id, j.job_type, COALESCE(j.model_ref,''),
		        COALESCE(t.input_ref,''), COALESCE(t.chunk_index,0), COALESCE(j.min_memory_gb,0)
		 FROM tasks t JOIN jobs j ON j.id = t.job_id
		 WHERE t.status = 'running'
		   AND t.is_honeypot = false AND t.is_redundancy = false
		   AND t.hedged_from IS NULL
		   AND t.started_at IS NOT NULL
		   AND t.started_at < now() - make_interval(secs => $1)
		   AND j.status = 'running'
		   -- ENDGAME: no unclaimed queued/retrying work remains on this job.
		   AND NOT EXISTS (
		     SELECT 1 FROM tasks q
		     WHERE q.job_id = t.job_id AND q.status IN ('queued','retrying')
		       AND q.claimed_by IS NULL
		   )
		   -- this chunk is not already hedged (same guard as StragglerTasks):
		   AND NOT EXISTS (
		     SELECT 1 FROM tasks h
		     WHERE h.job_id = t.job_id AND COALESCE(h.chunk_index,0) = COALESCE(t.chunk_index,0)
		       AND h.hedged_from IS NOT NULL AND h.is_redundancy = false
		       AND h.status NOT IN ('failed','cancelled')
		   )
		   -- and the job is under its in-flight hedge cap (same as StragglerTasks):
		   AND (
		     SELECT count(*) FROM tasks h2
		     WHERE h2.job_id = t.job_id AND h2.hedged_from IS NOT NULL
		       AND h2.is_redundancy = false AND h2.status IN ('queued','running')
		   ) < $2
		 ORDER BY t.started_at ASC
		 LIMIT $3`,
		minRun.Seconds(), maxInFlight, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Straggler
	for rows.Next() {
		var s Straggler
		if err := rows.Scan(&s.TaskID, &s.JobID, &s.WorkerID, &s.JobType, &s.ModelRef,
			&s.InputRef, &s.ChunkIndex, &s.MinMemGB); err != nil {
			return nil, err
		}
		out = append(out, s)
	}
	return out, rows.Err()
}

func (s *Store) InsertHedgeTask(ctx context.Context, jobID, primaryTaskID, peerWorker uuid.UUID, inputRef string, chunkIndex int) (uuid.UUID, error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return uuid.Nil, err
	}
	defer tx.Rollback(ctx)

	buyerCharge, supplierPayout, err := consumeEconomicReserveTx(ctx, tx, jobID)
	if err != nil {
		if errors.Is(err, ErrEconomicReserveExhausted) {
			var existing uuid.UUID
			qerr := tx.QueryRow(ctx, `
				SELECT id FROM tasks
				 WHERE job_id=$1 AND hedged_from=$2 AND is_redundancy=false
				 ORDER BY created_at LIMIT 1`, jobID, primaryTaskID).Scan(&existing)
			if qerr == nil {
				return existing, nil
			}
			if !errors.Is(qerr, pgx.ErrNoRows) {
				return uuid.Nil, qerr
			}
		}
		return uuid.Nil, err
	}

	var primaryStatus string
	err = tx.QueryRow(ctx, `
		SELECT status FROM tasks
		 WHERE id=$1 AND job_id=$2 AND is_redundancy=false
		 FOR UPDATE`, primaryTaskID, jobID).Scan(&primaryStatus)
	if errors.Is(err, pgx.ErrNoRows) {
		return uuid.Nil, ErrEconomicReserveExhausted
	}
	if err != nil {
		return uuid.Nil, err
	}
	if primaryStatus != "running" && primaryStatus != "verifying" {
		return uuid.Nil, ErrEconomicReserveExhausted
	}

	var existing uuid.UUID
	err = tx.QueryRow(ctx, `
		SELECT id FROM tasks
		 WHERE job_id=$1 AND hedged_from=$2 AND is_redundancy=false
		 ORDER BY created_at LIMIT 1`, jobID, primaryTaskID).Scan(&existing)
	if err == nil {
		return existing, nil
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return uuid.Nil, err
	}

	id := uuid.New()
	resultKey := taskAttemptResultKey(jobID, id, 0)
	_, err = tx.Exec(ctx,
		`INSERT INTO tasks
		   (id, job_id, status, is_honeypot, is_redundancy, retry_count,
		    input_ref, result_key, chunk_index, hedged_from,expected_output_records,
		    claimed_by, claimed_at, visible_at,
		    economic_buyer_charge_usd,economic_supplier_payout_usd)
		 VALUES ($1,$2,'queued',false,false,0,$3,$4,$5,$6,
		         (SELECT expected_output_records FROM tasks WHERE id=$6),
		         $7, now(), now(),$8,$9)`,
		id, jobID, inputRef, resultKey, chunkIndex, primaryTaskID, peerWorker,
		buyerCharge, supplierPayout)
	if err != nil {
		return uuid.Nil, err
	}
	if err := tx.Commit(ctx); err != nil {
		return uuid.Nil, err
	}
	return id, nil
}

func (s *Store) RequeueStaleTask(ctx context.Context, taskID uuid.UUID, backoff time.Duration) error {
	_, err := s.pool.Exec(ctx,
		`WITH requeued AS (
		 UPDATE tasks
		   SET status = 'queued', claimed_by = NULL, claimed_at = NULL,
		       worker_id = NULL, retry_count = retry_count + 1,
		       visible_at = now() + make_interval(secs => $2)
		 WHERE id = $1 AND status = 'running'
		 RETURNING job_id
		)
		UPDATE jobs SET status = 'running'
		 WHERE id IN (SELECT job_id FROM requeued) AND status = 'verifying'`,
		taskID, backoff.Seconds())
	return err
}

func (s *Store) FailTaskAndSettleJob(ctx context.Context, taskID, jobID uuid.UUID) error {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)

	ct, err := tx.Exec(ctx,
		`UPDATE tasks SET status = 'failed'
		  WHERE id = $1 AND job_id=$2 AND status='running'`, taskID, jobID)
	if err != nil {
		return err
	}
	if ct.RowsAffected() == 0 {
		return nil // the upload/verdict path or another reaper won the task race
	}
	if _, err := failJobAndSettleOnce(ctx, tx, jobID); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

func (s *Store) TaskJobID(ctx context.Context, taskID uuid.UUID) (uuid.UUID, error) {
	var jobID uuid.UUID
	err := s.pool.QueryRow(ctx, `SELECT job_id FROM tasks WHERE id = $1`, taskID).Scan(&jobID)
	if errors.Is(err, pgx.ErrNoRows) {
		return uuid.Nil, errNotFound
	}
	return jobID, err
}

func (s *Store) DeadClaimedTasks(ctx context.Context, olderThan time.Duration, limit int) ([]DeadClaim, error) {
	rows, err := s.pool.Query(ctx,
		`SELECT t.id,t.job_id,t.execution_worker_id,t.execution_supplier_id,j.job_type
		 FROM tasks t
		 JOIN workers w ON w.id = t.claimed_by
		 JOIN jobs j ON j.id = t.job_id
		 WHERE t.status = 'running'
		   AND t.claimed_at IS NOT NULL
		   AND t.claimed_at < now() - make_interval(secs => $1)
		   AND (w.last_seen_at IS NULL OR w.last_seen_at < now() - make_interval(secs => $1))
		   AND t.execution_worker_id IS NOT NULL
		   AND t.execution_supplier_id IS NOT NULL
		   AND j.status = 'running'
		 ORDER BY t.claimed_at ASC
		 LIMIT $2`,
		olderThan.Seconds(), limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []DeadClaim
	for rows.Next() {
		var d DeadClaim
		var sup *uuid.UUID
		if err := rows.Scan(&d.TaskID, &d.JobID, &d.WorkerID, &sup, &d.JobType); err != nil {
			return nil, err
		}
		if sup != nil {
			d.SupplierID = *sup
		}
		out = append(out, d)
	}
	return out, rows.Err()
}

func (s *Store) RescueRunningTask(ctx context.Context, taskID uuid.UUID, backoff time.Duration) (rescued bool, err error) {
	ct, err := s.pool.Exec(ctx,
		`UPDATE tasks
		   SET status = 'queued', claimed_by = NULL, claimed_at = NULL,
		       started_at = NULL, worker_id = NULL, retry_count = retry_count + 1,
		       visible_at = now() + make_interval(secs => $2)
		 WHERE id = $1 AND status = 'running'`,
		taskID, backoff.Seconds())
	if err != nil {
		return false, err
	}
	return ct.RowsAffected() > 0, nil
}

func (s *Store) CancelledTaskResultKeys(ctx context.Context, jobID uuid.UUID) ([]string, error) {
	rows, err := s.pool.Query(ctx,
		`SELECT result_key FROM tasks
		 WHERE job_id = $1 AND status = 'cancelled'
		   AND is_honeypot = false AND is_redundancy = false
		   AND result_key IS NOT NULL AND result_key <> ''
		 ORDER BY chunk_index ASC, id ASC`,
		jobID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var k string
		if err := rows.Scan(&k); err != nil {
			return nil, err
		}
		out = append(out, k)
	}
	return out, rows.Err()
}

var taskDurationBucketsMs = []float64{100, 500, 1000, 2500, 5000, 15000, 30000, 60000, 120000}

type TaskDurationHistogramRow struct {
	JobType string
	Buckets []int64 // cumulative, same order/length as taskDurationBucketsMs
	Count   int64
	SumMs   int64
}

func (s *Store) TaskDurationHistogram(ctx context.Context) ([]TaskDurationHistogramRow, error) {
	selectCols := make([]string, 0, len(taskDurationBucketsMs))
	args := make([]any, 0, len(taskDurationBucketsMs))
	for i, b := range taskDurationBucketsMs {
		args = append(args, b)
		selectCols = append(selectCols, fmt.Sprintf("count(*) FILTER (WHERE duration_ms <= $%d)", i+1))
	}
	query := fmt.Sprintf(
		`SELECT job_type, count(*), COALESCE(sum(duration_ms),0), %s
		 FROM task_durations
		 GROUP BY job_type
		 ORDER BY job_type`,
		strings.Join(selectCols, ", "),
	)
	rows, err := s.pool.Query(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []TaskDurationHistogramRow
	for rows.Next() {
		var r TaskDurationHistogramRow
		r.Buckets = make([]int64, len(taskDurationBucketsMs))
		scanArgs := make([]any, 0, 3+len(r.Buckets))
		scanArgs = append(scanArgs, &r.JobType, &r.Count, &r.SumMs)
		for i := range r.Buckets {
			scanArgs = append(scanArgs, &r.Buckets[i])
		}
		if err := rows.Scan(scanArgs...); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}
