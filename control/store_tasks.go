package main

import (
	"context"
	_ "embed"
	"errors"
	"fmt"
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
	ID           uuid.UUID
	JobID        uuid.UUID
	IsHoneypot   bool
	IsRedundancy bool
	InputRef     string
	// InputSHA256 is the submit-time digest of the exact immutable object this
	// task will execute. Durable current admission compares it to the workload's
	// priced input digest; InputRef remains the stored execution handle.
	InputSHA256 string
	// InputDepthBand is the immutable depth bucket for this exact input chunk.
	// It is deliberately task-scoped: a job-wide p90 loses the mixed-input
	// geometry that task-duration learning needs.
	InputDepthBand        string
	ResultKey             string
	ChunkIndex            int
	ExpectedOutputRecords int64 // 0 = explicit legacy/opaque unknown, persisted NULL
	// VerificationClass is the governed class this task is verified under. Empty
	// is derived from the probe/redundancy flags and the job's class at insert,
	// so a caller that does not care does not have to know.
	VerificationClass string
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

func (s *Store) TaskEconomicAmounts(ctx context.Context, taskID uuid.UUID) (frozen frozenTaskEconomics, err error) {
	err = s.pool.QueryRow(ctx, `
		SELECT economic_buyer_charge_usd::float8,economic_supplier_payout_usd::float8,
		       economic_buyer_charge_nanos,economic_supplier_payout_nanos
		  FROM tasks WHERE id=$1`, taskID).
		Scan(&frozen.BuyerChargeUSD, &frozen.SupplierPayoutUSD,
			&frozen.BuyerChargeNanos, &frozen.SupplierPayoutNanos)
	if errors.Is(err, pgx.ErrNoRows) {
		return frozenTaskEconomics{}, errNotFound
	}
	if err != nil {
		return frozenTaskEconomics{}, fmt.Errorf("task %s has no frozen economic amounts: %w", taskID, err)
	}
	return frozen, nil
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
	var claimHWClass, claimHardwareIdentity, claimEngine, claimBuildHash, claimBuildIdentityPolicy, claimRuntimeProfileID string
	err = tx.QueryRow(ctx, `
		SELECT supplier_id,COALESCE(hw_class,''),COALESCE(hardware_identity,''),COALESCE(engine,''),
		       COALESCE(build_hash,''),COALESCE(build_identity_policy,''),COALESCE(runtime_profile_id,'')
		  FROM workers WHERE id=$1 FOR UPDATE`, workerID).
		Scan(&claimSupplierID, &claimHWClass, &claimHardwareIdentity, &claimEngine, &claimBuildHash,
			&claimBuildIdentityPolicy, &claimRuntimeProfileID)
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
		frozenHWClass       string
		frozenHardware      string
		frozenEngine        string
		frozenBuildHash     string
		frozenBuildPolicy   string
	)
	err = tx.QueryRow(ctx, `
		SELECT job_id,status,claimed_by,worker_id,execution_worker_id,execution_supplier_id,started_at,
		       COALESCE(is_redundancy,false),hedged_from,COALESCE(retry_count,0),
		       COALESCE(execution_hw_class,''),COALESCE(execution_hardware_identity,''),
		       COALESCE(execution_engine,''),COALESCE(execution_build_hash,''),
		       COALESCE(execution_build_identity_policy,'')
		  FROM tasks WHERE id=$1 AND retry_count=$2 FOR UPDATE`, taskID, claimAttempt).
		Scan(&jobID, &status, &claimedBy, &taskWorkerID, &executionWorkerID, &executionSupplierID, &startedAt,
			&isRedundancy, &hedgedFrom, &attempt, &frozenHWClass, &frozenHardware, &frozenEngine, &frozenBuildHash,
			&frozenBuildPolicy)
	if errors.Is(err, pgx.ErrNoRows) {
		return errNotFound
	}
	if err != nil {
		return err
	}
	if claimedBy == nil || *claimedBy != workerID || (status != "queued" && status != "running") {
		return errNotFound
	}

	var parentStatus, jobCurrency, placementRuntimeID, placementEngine, placementBuild, placementBuildPolicy, placementHardware string
	var placementVersion int
	if err := tx.QueryRow(ctx, `
		SELECT status,currency,
		       COALESCE(NULLIF(placement_requirement->>'version','')::int,1),
		       COALESCE(placement_requirement->>'runtime_id',''),
		       COALESCE(placement_requirement->>'engine',''),
		       COALESCE(placement_requirement->>'engine_build_hash',''),
		       COALESCE(placement_requirement->>'engine_build_identity_policy',''),
		       COALESCE(placement_requirement->>'hardware_identity','')
		  FROM jobs WHERE id=$1 FOR UPDATE`, jobID).
		Scan(&parentStatus, &jobCurrency, &placementVersion, &placementRuntimeID,
			&placementEngine, &placementBuild, &placementBuildPolicy, &placementHardware); err != nil {
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
	if placementVersion >= placementRequirementVersion &&
		(claimRuntimeProfileID != placementRuntimeID || claimEngine != placementEngine ||
			claimBuildHash != placementBuild || claimBuildIdentityPolicy != placementBuildPolicy ||
			claimHardwareIdentity != placementHardware) {
		return errNotFound
	}
	if placementVersion >= placementRequirementVersion && (status == "running" || !dynamicTiebreak) &&
		(frozenHWClass != claimHWClass || frozenHardware != claimHardwareIdentity ||
			frozenEngine != claimEngine || frozenBuildHash != claimBuildHash ||
			frozenBuildPolicy != claimBuildIdentityPolicy ||
			executionSupplierID == nil || *executionSupplierID != claimSupplierID) {
		return errNotFound
	}
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
		       execution_hw_class=$4,execution_hardware_identity=NULLIF($5,''),
		       execution_engine=$6,execution_build_hash=$7,execution_build_identity_policy=$8
		 WHERE id=$1 AND claimed_by=$2 AND retry_count=$9 AND status='queued'`,
		taskID, workerID, claimSupplierID, claimHWClass, claimHardwareIdentity, claimEngine, claimBuildHash,
		claimBuildIdentityPolicy, claimAttempt)
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
	TaskID       uuid.UUID
	JobID        uuid.UUID
	WorkerID     uuid.UUID
	SupplierID   uuid.UUID
	IsHoneypot   bool
	IsRedundancy bool
	// VerificationClass decides whether this task's check is a coin flip. Read
	// from the task row rather than recomputed, because the row is what the
	// receipt and the verification work plan will be reconciled against.
	VerificationClass        string
	HWClass                  string
	hardwareIdentity         string
	engine                   string
	buildHash                string
	buildIdentityPolicy      string
	jobType                  string // parent job's job_type, for honeypot answer lookup
	jobMaxTokens             uint32 // bounded projection of frozen workload job_type.max_tokens
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
	peerBuildIdentityPolicy  string
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
		SELECT t.job_id,COALESCE(t.result_key,'')
		  FROM tasks t
		  JOIN jobs j ON j.id=t.job_id
		  JOIN workers w ON w.id=$2
		 WHERE t.id=$1 AND t.claimed_by=$2 AND t.execution_worker_id=$2 AND t.retry_count=$3
		   AND t.status IN ('running','queued','verifying')
		   AND (
		     COALESCE(NULLIF(j.placement_requirement->>'version','')::int,1)<3
		     OR (
		       COALESCE(w.runtime_profile_id,'')=COALESCE(j.placement_requirement->>'runtime_id','')
		       AND COALESCE(w.engine,'')=COALESCE(j.placement_requirement->>'engine','')
		       AND COALESCE(w.build_hash,'')=COALESCE(j.placement_requirement->>'engine_build_hash','')
		       AND COALESCE(w.build_identity_policy,'')=COALESCE(j.placement_requirement->>'engine_build_identity_policy','')
		       AND COALESCE(w.hardware_identity,'')=COALESCE(j.placement_requirement->>'hardware_identity','')
		       AND w.supplier_id=t.execution_supplier_id
		       AND COALESCE(w.hw_class,'')=COALESCE(t.execution_hw_class,'')
		       AND COALESCE(w.engine,'')=COALESCE(t.execution_engine,'')
		       AND COALESCE(w.build_hash,'')=COALESCE(t.execution_build_hash,'')
		       AND COALESCE(w.build_identity_policy,'')=COALESCE(t.execution_build_identity_policy,'')
		       AND COALESCE(w.hardware_identity,'')=COALESCE(t.execution_hardware_identity,'')
		     )
		   )
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
		        COALESCE(t.verification_class,'SAMPLED'),
		        COALESCE(t.input_ref,''),
		        COALESCE(t.result_key,''),
		        t.execution_worker_id,t.execution_supplier_id,COALESCE(t.execution_hw_class,''),
		        COALESCE(t.execution_hardware_identity,''),
		        COALESCE(t.execution_engine,''),COALESCE(t.execution_build_hash,''),
		        COALESCE(t.execution_build_identity_policy,''),j.job_type,
		        COALESCE((j.workload_decision #>> '{binding,job_type,max_tokens}')::bigint,0),
		        COALESCE(j.model_ref,''), COALESCE(j.min_memory_gb,0),
		        COALESCE(t.chunk_index,0), COALESCE(j.split_size,0),
		        COALESCE(t.expected_output_records,0),
		        COALESCE(t.retry_count,0), COALESCE(t.result_sha256,'')
	 FROM tasks t JOIN jobs j ON j.id = t.job_id
	 WHERE t.id = $1 AND t.execution_worker_id=$2`,
		taskID, workerID,
	).Scan(&info.JobID, &info.IsHoneypot, &info.IsRedundancy, &info.VerificationClass, &info.InputRef,
		&info.ResultKey, &info.WorkerID, &info.SupplierID, &info.HWClass, &info.hardwareIdentity, &info.engine, &info.buildHash,
		&info.buildIdentityPolicy, &info.jobType, &jobMaxTokens,
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
	// whether or not we record it. When the agent/engine reports
	// cached_prompt_tokens, observation corrects stale belief before the
	// post-serve mark (see observeAndMarkPrefixForCommit).
	s.observeAndMarkPrefixForCommit(ctx, workerID, info.JobID, c.CachedPromptTokens)
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
	TaskID              uuid.UUID
	WorkerID            uuid.UUID
	SupplierID          uuid.UUID
	ResultRef           string
	Artifact            *VerificationArtifact
	Engine              string
	BuildHash           string
	BuildIdentityPolicy string
}

func (s *Store) ChunkResults(ctx context.Context, jobID uuid.UUID, chunkIndex int) ([]ChunkResult, error) {
	rows, err := s.pool.Query(ctx,
		`SELECT t.id,COALESCE(vw.worker_id,t.execution_worker_id),
		        COALESCE(vw.supplier_id,t.execution_supplier_id),
		        t.result_ref,
		        COALESCE(vw.input_snapshot->>'engine',t.execution_engine,''),
		        COALESCE(vw.input_snapshot->>'build_hash',t.execution_build_hash,''),
		        COALESCE(vw.input_snapshot->>'build_identity_policy',t.execution_build_identity_policy,''),
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
			&cr.Engine, &cr.BuildHash, &cr.BuildIdentityPolicy,
			&artifactKey, &artifactSHA, &artifactBytes); err != nil {
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

func existingTiebreakTaskTx(
	ctx context.Context,
	tx pgx.Tx,
	jobID uuid.UUID,
	chunkIndex int,
) (uuid.UUID, error) {
	var existing uuid.UUID
	err := tx.QueryRow(ctx, `
		SELECT id FROM tasks
		 WHERE job_id=$1 AND COALESCE(chunk_index,0)=$2
		   AND hedged_from IS NOT NULL AND is_redundancy=true
		 ORDER BY created_at,id LIMIT 1`, jobID, chunkIndex).Scan(&existing)
	return existing, err
}

func existingHedgeTaskTx(
	ctx context.Context,
	tx pgx.Tx,
	jobID, primaryTaskID uuid.UUID,
) (uuid.UUID, error) {
	var existing uuid.UUID
	err := tx.QueryRow(ctx, `
		SELECT id FROM tasks
		 WHERE job_id=$1 AND hedged_from=$2 AND is_redundancy=false
		 ORDER BY created_at,id LIMIT 1`, jobID, primaryTaskID).Scan(&existing)
	return existing, err
}

func lockLiveJobForDynamicTaskTx(ctx context.Context, tx pgx.Tx, jobID uuid.UUID) error {
	var status string
	err := tx.QueryRow(ctx, `SELECT status FROM jobs WHERE id=$1 FOR UPDATE`, jobID).Scan(&status)
	if errors.Is(err, pgx.ErrNoRows) {
		return ErrEconomicReserveExhausted
	}
	if err != nil {
		return err
	}
	if status != "queued" && status != "running" && status != "verifying" {
		return ErrEconomicReserveExhausted
	}
	return nil
}

// lockDynamicPeerWorkerTx establishes the process-wide dynamic dispatch lock
// order: candidate worker, then reserve/task/job. It must be called before any
// dynamic-task writer takes a reserve, task, or job row lock. Claim and Start
// already begin with the worker row, so the shared order avoids worker<->job
// deadlocks while serializing registration with pin publication.
func lockDynamicPeerWorkerTx(ctx context.Context, tx pgx.Tx, peer uuid.UUID) error {
	var locked uuid.UUID
	if err := tx.QueryRow(ctx, `SELECT id FROM workers WHERE id=$1 FOR UPDATE`, peer).Scan(&locked); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return ErrNoSupply
		}
		return err
	}
	return nil
}

// dynamicPeerClaimEligibleTx is the last, transactional eligibility boundary
// for a task that will be born pinned to peer. Selection happens before this
// transaction and is necessarily advisory: either the anchor or the proposed
// peer can re-register between selection and insertion. Every writer locks the
// peer before taking reserve/task/job locks, then calls this helper while those
// facts are stable. Most importantly, this runs before
// consumeEconomicReserveTx, so an identity/cell mismatch cannot consume the
// one frozen hedge/tiebreak reserve and leave behind an unclaimable task.
func dynamicPeerClaimEligibleTx(
	ctx context.Context,
	tx pgx.Tx,
	jobID, anchorTaskID, peer uuid.UUID,
) (bool, error) {
	var eligible bool
	err := tx.QueryRow(ctx, `
		SELECT EXISTS (
		 SELECT 1
		   FROM jobs j
		   JOIN tasks anchor ON anchor.id=$2 AND anchor.job_id=j.id
		   JOIN workers nw ON nw.id=$3
		   JOIN suppliers ns ON ns.id=nw.supplier_id
		  WHERE j.id=$1
		    AND j.status IN ('queued','running','verifying')
		    AND nw.last_seen_at>now()-interval '60 seconds'
		    AND ns.status='active'
		    AND NOT COALESCE(nw.throttled,false)
`+workerJobContainmentSQL("nw", "j")+`
		    AND COALESCE(nw.effective_memory_gb,nw.memory_gb,0)>=COALESCE(j.min_memory_gb,0)
		    AND (j.hw_classes IS NULL OR nw.hw_class=ANY(j.hw_classes))
		    AND (j.data_residency IS NULL OR ns.data_country=ANY(j.data_residency))
		    AND COALESCE(j.min_reputation,0)<=ns.reputation
		    AND (j.tier<>'trusted' OR (ns.reputation>=0.80 AND ns.completed_tasks>=500))
		    AND COALESCE(j.offered_rate_usd_hr,1e9)>=COALESCE(nw.min_payout_usd_hr,0)
		    AND nw.id IS DISTINCT FROM anchor.execution_worker_id
		    AND nw.supplier_id IS DISTINCT FROM anchor.execution_supplier_id
		    AND (
		      COALESCE(NULLIF(j.placement_requirement->>'version','')::int,1)<3
		      OR (
		        j.placement_requirement->>'version'='3'
		        -- Bind both sides to the accepted placement. The anchor tuple is
		        -- immutable after claim; the peer tuple is locked above.
		        AND COALESCE(anchor.execution_hw_class,'')=COALESCE(nw.hw_class,'')
		        AND COALESCE(anchor.execution_engine,'')=COALESCE(j.placement_requirement->>'engine','')
		        AND COALESCE(anchor.execution_engine,'')=COALESCE(nw.engine,'')
		        AND COALESCE(anchor.execution_build_hash,'')=COALESCE(j.placement_requirement->>'engine_build_hash','')
		        AND COALESCE(anchor.execution_build_hash,'')=COALESCE(nw.build_hash,'')
		        AND COALESCE(anchor.execution_build_identity_policy,'')=COALESCE(j.placement_requirement->>'engine_build_identity_policy','')
		        AND COALESCE(anchor.execution_build_identity_policy,'')=COALESCE(nw.build_identity_policy,'')
		        AND COALESCE(anchor.execution_hardware_identity,'')=COALESCE(j.placement_requirement->>'hardware_identity','')
		        AND COALESCE(anchor.execution_hardware_identity,'')=COALESCE(nw.hardware_identity,'')
		        AND COALESCE(anchor.runtime_cell_id,'')=COALESCE(j.placement_requirement->>'runtime_cell_id','')
		        AND COALESCE(anchor.runtime_id,'')=COALESCE(j.placement_requirement->>'runtime_id','')
		        AND COALESCE(anchor.model_kind,'')=COALESCE(j.placement_requirement->>'model_kind','')
		        AND COALESCE(nw.runtime_profile_id,'')=COALESCE(j.placement_requirement->>'runtime_id','')
		        AND COALESCE(j.placement_requirement->>'runtime_matrix_sha256','')=$4
		        AND EXISTS (
		          SELECT 1 FROM worker_authorized_capabilities wac
		           WHERE wac.worker_id=nw.id
		             AND wac.job_type=j.job_type
		             AND wac.model_ref=COALESCE(j.model_ref,'')
		             AND wac.matrix_sha256=$4
		             AND wac.authorized_at>=now()-interval '7 days'
		             AND wac.cell_id=j.placement_requirement->>'runtime_cell_id'
		             AND wac.runtime_id=j.placement_requirement->>'runtime_id'
		             AND wac.model_kind=j.placement_requirement->>'model_kind'
		        )
		      )
		    )
`+supplierNotLinkedToBuyerSQL("ns")+`
		)`, jobID, anchorTaskID, peer, generatedRuntimeMatrixSHA256).Scan(&eligible)
	return eligible, err
}

func (s *Store) InsertTiebreakTask(ctx context.Context, jobID, primaryTaskID, peerWorker uuid.UUID, inputRef string, chunkIndex int) (uuid.UUID, error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return uuid.Nil, err
	}
	defer tx.Rollback(ctx)
	if err := lockDynamicPeerWorkerTx(ctx, tx, peerWorker); err != nil {
		return uuid.Nil, err
	}

	if err := lockEconomicReserveTx(ctx, tx, jobID); err != nil {
		if errors.Is(err, ErrEconomicReserveExhausted) {
			existing, qerr := existingTiebreakTaskTx(ctx, tx, jobID, chunkIndex)
			if qerr == nil {
				return existing, nil
			}
			if !errors.Is(qerr, pgx.ErrNoRows) {
				return uuid.Nil, qerr
			}
		}
		return uuid.Nil, err
	}
	existing, err := existingTiebreakTaskTx(ctx, tx, jobID, chunkIndex)
	if err == nil {
		return existing, nil
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return uuid.Nil, err
	}

	var lockedAnchor uuid.UUID
	err = tx.QueryRow(ctx,
		`SELECT id FROM tasks WHERE id=$1 AND job_id=$2 FOR UPDATE`,
		primaryTaskID, jobID).Scan(&lockedAnchor)
	if errors.Is(err, pgx.ErrNoRows) {
		return uuid.Nil, ErrEconomicReserveExhausted
	}
	if err != nil {
		return uuid.Nil, err
	}
	if err := lockLiveJobForDynamicTaskTx(ctx, tx, jobID); err != nil {
		return uuid.Nil, err
	}
	cloneFrozen, currentUniform, err := currentUniformCloneTaskEconomics(
		ctx, tx, jobID, primaryTaskID, inputRef, chunkIndex)
	if err != nil {
		return uuid.Nil, err
	}
	eligible, err := dynamicPeerClaimEligibleTx(ctx, tx, jobID, primaryTaskID, peerWorker)
	if err != nil {
		return uuid.Nil, err
	}
	if !eligible {
		return uuid.Nil, ErrNoSupply
	}
	frozen, err := consumeEconomicReserveTx(ctx, tx, jobID)
	if err != nil {
		return uuid.Nil, err
	}
	if currentUniform {
		frozen = cloneFrozen
	}
	frozenClass, err := frozenTiebreakClassForAnchorTx(ctx, tx, primaryTaskID)
	if err != nil {
		return uuid.Nil, err
	}

	id := uuid.New()
	resultKey := taskAttemptResultKey(jobID, id, 0)
	if _, err := tx.Exec(ctx,
		`INSERT INTO tasks
		   (id, job_id, status, is_honeypot, is_redundancy, verification_class, retry_count,
		    input_ref, input_sha256, input_depth_band, result_key, chunk_index, hedged_from,expected_output_records,
		    verification_hw_class,verification_engine,verification_build_hash,verification_build_identity_policy,
		    claimed_by, claimed_at, visible_at,
		    economic_buyer_charge_usd,economic_supplier_payout_usd,
		    economic_buyer_charge_nanos,economic_supplier_payout_nanos)
		 VALUES ($1,$2,'queued',false,true,'REDUNDANT',0,$3,
		         (SELECT input_sha256 FROM tasks WHERE id=$6),
		         (SELECT input_depth_band FROM tasks WHERE id=$6),$4,$5,$6,
		         (SELECT expected_output_records FROM tasks WHERE id=$6),
		         $7,$8,$9,$10,$11,now(),now(),$12,$13,$14,$15)`,
		id, jobID, inputRef, resultKey, chunkIndex, primaryTaskID,
		frozenClass.HWClass, frozenClass.Engine, frozenClass.BuildHash, frozenClass.BuildIdentityPolicy,
		peerWorker, frozen.BuyerChargeUSD, frozen.SupplierPayoutUSD,
		frozen.BuyerChargeNanos, frozen.SupplierPayoutNanos,
	); err != nil {
		return uuid.Nil, err
	}
	tag, err := tx.Exec(ctx,
		`UPDATE jobs
		    SET task_count = task_count + 1,
		        status = CASE WHEN status='verifying' THEN 'running' ELSE status END
		  WHERE id = $1 AND status IN ('queued','running','verifying')`, jobID)
	if err != nil {
		return uuid.Nil, err
	}
	if tag.RowsAffected() != 1 {
		return uuid.Nil, ErrEconomicReserveExhausted
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
		var jobID uuid.UUID
		if err := tx.QueryRow(ctx, `SELECT job_id FROM tasks WHERE id=$1`, taskID).Scan(&jobID); err != nil {
			return err
		}
		pricingSHA, aerr := loadJobPricingDecisionSHA(ctx, tx, jobID)
		if aerr != nil {
			return aerr
		}
		cb.PricingDecisionSHA256 = pricingSHA
		cb.LifecycleRevision = liabilityLifecycleRevision
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
	// Load frozen compute plan once so generative tasks can show ceiling /
	// observed / rebate without inventing a parallel receipt path.
	plan, planErr := s.JobComputePlan(ctx, jobID)
	if planErr != nil {
		return nil, planErr
	}
	workload, workloadErr := s.JobWorkloadDecision(ctx, jobID)
	if workloadErr != nil {
		return nil, workloadErr
	}
	var maxTokens uint32
	if plan != nil && workload != nil {
		maxTokens = effectiveObservedOutputMaxTokens(*workload, *plan)
	}
	rows, err := s.pool.Query(ctx,
		`SELECT t.id, COALESCE(t.chunk_index,0), t.status, t.is_honeypot,
		        COALESCE(vw.input_snapshot->>'engine',t.execution_engine,''),
		        COALESCE(vw.input_snapshot->>'build_hash',t.execution_build_hash,''),
		        COALESCE(vw.input_snapshot->>'build_identity_policy',t.execution_build_identity_policy,''),
		        COALESCE((SELECT ve.kind FROM verification_events ve
		                  WHERE ve.task_id = t.id ORDER BY ve.created_at DESC LIMIT 1), ''),
		        COALESCE(t.verification_outcome,''),
		        COALESCE(t.runtime_cell_id,''), COALESCE(t.runtime_id,''),
		        COALESCE(t.runtime_matrix_sha256,''), COALESCE(t.model_kind,''),
		        COALESCE(t.verification_class,'SAMPLED'), vw.sampling_selected,
		        COALESCE(t.expected_output_records,0),
		        t.reported_tokens_used,
		        t.economic_buyer_charge_usd::float8,
		        COALESCE((
		          SELECT -le.amount_usd::float8 FROM ledger_entries le
		           WHERE le.task_id = t.id AND le.kind = 'buyer_charge'
		           LIMIT 1
		        ),0)
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
			taskID                       uuid.UUID
			chunk                        int
			status                       string
			isHoneypot                   bool
			engine, build, buildPolicy   string
			kind, verdict                string
			cellID, runtimeID, matrixSHA string
			modelKind                    string
			verificationClass            string
			verificationSelected         *bool
			expectedRecords              int64
			reportedTokens               *int64
			frozenCharge                 *float64
			billedCharge                 float64
		)
		if err := rows.Scan(&taskID, &chunk, &status, &isHoneypot, &engine, &build, &buildPolicy, &kind, &verdict,
			&cellID, &runtimeID, &matrixSHA, &modelKind,
			&verificationClass, &verificationSelected,
			&expectedRecords, &reportedTokens, &frozenCharge, &billedCharge); err != nil {
			return nil, err
		}
		tr := taskReceiptRowWithRuntimePolicy(chunk, status, isHoneypot, engine, build, buildPolicy,
			kind, verdict, cellID, runtimeID, matrixSHA, modelKind)
		tr.VerificationClass = verificationClass
		tr.VerificationSelected = verificationSelected
		// Surface observed-output evidence only when the frozen plan priced
		// generative output and this task had a positive ceiling.
		if plan != nil && plan.EstimatedOutputTokens > 0 &&
			expectedRecords > 0 && maxTokens > 0 &&
			expectedRecords <= math.MaxInt64/int64(maxTokens) {
			ceiling := expectedRecords * int64(maxTokens)
			tr.OutputTokenCeiling = &ceiling
			if reportedTokens != nil {
				observed := *reportedTokens
				if observed >= 0 && observed > ceiling {
					observed = ceiling
				}
				if observed >= 0 {
					tr.ObservedOutputTokens = &observed
				}
			}
			if frozenCharge != nil && *frozenCharge > 0 {
				fc := *frozenCharge
				tr.FrozenBuyerChargeUSD = &fc
				// Prefer the ledger (what was actually settled). Fall back to
				// the same loader settlement uses so floor clamp and nano
				// authority cannot disagree with the receipt.
				billed := billedCharge
				rebate := roundUSD(0)
				if billed <= 0 {
					settled, serr := loadObservedOutputSettlement(ctx, s.pool, taskID)
					if serr != nil {
						return nil, serr
					}
					billed = settled.BilledCharge
					rebate = settled.RebateUSD
					if settled.FloorClamped {
						clamped := true
						tr.ContributionFloorClamped = &clamped
						if settled.UnclampedRebateUSD > 0 {
							u := settled.UnclampedRebateUSD
							tr.UnclampedRebateUSD = &u
						}
					}
				} else {
					rebate = roundUSD(fc - billed)
					if rebate < 0 {
						rebate = 0
					}
				}
				tr.BilledBuyerChargeUSD = &billed
				tr.OutputTokenRebateUSD = &rebate
			}
		}
		out = append(out, tr)
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
		`SELECT t.id, t.job_id, t.retry_count
		   FROM tasks t
		   JOIN jobs j ON j.id = t.job_id
		  WHERE t.status = 'running' AND t.claimed_at IS NOT NULL
		    AND t.claimed_at < now() - make_interval(secs => $1)
		    AND j.status NOT IN ('complete','cancelled','failed')
		  ORDER BY t.claimed_at ASC LIMIT $2`,
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
	if err := lockDynamicPeerWorkerTx(ctx, tx, peerWorker); err != nil {
		return uuid.Nil, err
	}

	if err := lockEconomicReserveTx(ctx, tx, jobID); err != nil {
		if errors.Is(err, ErrEconomicReserveExhausted) {
			existing, qerr := existingHedgeTaskTx(ctx, tx, jobID, primaryTaskID)
			if qerr == nil {
				return existing, nil
			}
			if !errors.Is(qerr, pgx.ErrNoRows) {
				return uuid.Nil, qerr
			}
		}
		return uuid.Nil, err
	}
	existing, err := existingHedgeTaskTx(ctx, tx, jobID, primaryTaskID)
	if err == nil {
		return existing, nil
	}
	if !errors.Is(err, pgx.ErrNoRows) {
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
	if err := lockLiveJobForDynamicTaskTx(ctx, tx, jobID); err != nil {
		return uuid.Nil, err
	}
	cloneFrozen, currentUniform, err := currentUniformCloneTaskEconomics(
		ctx, tx, jobID, primaryTaskID, inputRef, chunkIndex)
	if err != nil {
		return uuid.Nil, err
	}
	eligible, err := dynamicPeerClaimEligibleTx(ctx, tx, jobID, primaryTaskID, peerWorker)
	if err != nil {
		return uuid.Nil, err
	}
	if !eligible {
		return uuid.Nil, ErrNoSupply
	}
	frozen, err := consumeEconomicReserveTx(ctx, tx, jobID)
	if err != nil {
		return uuid.Nil, err
	}
	if currentUniform {
		frozen = cloneFrozen
	}

	id := uuid.New()
	resultKey := taskAttemptResultKey(jobID, id, 0)
	_, err = tx.Exec(ctx,
		`INSERT INTO tasks
		   (id, job_id, status, is_honeypot, is_redundancy, verification_class, retry_count,
		    input_ref, input_sha256, input_depth_band, result_key, chunk_index, hedged_from,expected_output_records,
		    claimed_by, claimed_at, visible_at,
		    economic_buyer_charge_usd,economic_supplier_payout_usd,
		    economic_buyer_charge_nanos,economic_supplier_payout_nanos)
		 VALUES ($1,$2,'queued',false,false,'SAMPLED',0,$3,
		         (SELECT input_sha256 FROM tasks WHERE id=$6),
		         (SELECT input_depth_band FROM tasks WHERE id=$6),$4,$5,$6,
		         (SELECT expected_output_records FROM tasks WHERE id=$6),
		         $7, now(), now(),$8,$9,$10,$11)`,
		id, jobID, inputRef, resultKey, chunkIndex, primaryTaskID, peerWorker,
		frozen.BuyerChargeUSD, frozen.SupplierPayoutUSD,
		frozen.BuyerChargeNanos, frozen.SupplierPayoutNanos)
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
		 UPDATE tasks t
		   SET status = 'queued', claimed_by = NULL, claimed_at = NULL,
		       worker_id = NULL, retry_count = retry_count + 1,
		       visible_at = now() + make_interval(secs => $2)
		 WHERE t.id = $1 AND t.status = 'running'
		   AND EXISTS (
		     SELECT 1 FROM jobs j
		      WHERE j.id = t.job_id
		        AND j.status NOT IN ('complete','cancelled','failed')
		   )
		 RETURNING t.job_id
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
		`UPDATE tasks
		    SET status = 'failed',
		        claimed_by = NULL,
		        claimed_at = NULL,
		        worker_id = NULL
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
