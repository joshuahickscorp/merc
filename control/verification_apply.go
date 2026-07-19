package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strconv"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

var ErrVerificationReplayConflict = errors.New("verification replay conflicts with durable verdict")

type VerificationApplyResult struct {
	Applied           bool
	Rejected          bool
	TiebreaksInserted int
}

type verificationApplyFence struct {
	Lease VerificationLease
	Plan  VerificationWorkPlan
}

func (s *Store) VerifyJobTx(ctx context.Context, lease VerificationLease, plan VerificationWorkPlan, info *CommitTaskInfo, probe recoveryBoundaryProbe) (VerificationApplyResult, error) {
	if plan.WorkID != lease.WorkID {
		return VerificationApplyResult{}, ErrVerificationWorkConflict
	}
	return s.applyVerificationDecision(ctx, info, plan.Decision, plan.Settlement,
		&verificationApplyFence{Lease: lease, Plan: plan}, probe)
}

func (s *Store) applyVerificationDecision(ctx context.Context, info *CommitTaskInfo, decision VerificationDecision, entries []LedgerEntry, fence *verificationApplyFence, probe recoveryBoundaryProbe) (VerificationApplyResult, error) {
	var result VerificationApplyResult
	if info == nil {
		return result, fmt.Errorf("apply verification decision: nil task info")
	}
	switch decision.Outcome {
	case OutcomePass, OutcomePassWithPenalty, OutcomeLossNoPayout, OutcomeFail:
	default:
		return result, fmt.Errorf("apply verification decision: unsupported outcome %q", decision.Outcome)
	}
	if decision.Outcome == OutcomeFail && len(entries) != 0 {
		return result, fmt.Errorf("apply verification decision: rejected task cannot carry settlement rows")
	}
	if decision.Outcome == OutcomeLossNoPayout && len(entries) != 0 {
		return result, fmt.Errorf("apply verification decision: loss_no_payout cannot carry settlement rows")
	}
	if err := validateVerificationDecisionShape(info, decision, entries); err != nil {
		return result, err
	}
	decisionDigest, err := verificationDecisionDigest(decision, entries)
	if err != nil {
		return result, err
	}
	if fence != nil && decisionDigest != fence.Plan.DecisionSHA256 {
		return result, ErrVerificationWorkConflict
	}

	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return result, err
	}
	defer tx.Rollback(ctx)
	if fence != nil {
		if err := lockVerificationWorkFenceTx(ctx, tx, info, *fence); err != nil {
			return result, err
		}
	}
	for _, effect := range decision.Effects {
		if effect.Kind == VerificationEffectInsertTiebreak {
			var lockedJob uuid.UUID
			if err := tx.QueryRow(ctx, `
				SELECT job_id FROM job_economic_reserves WHERE job_id=$1 FOR UPDATE`, info.JobID).
				Scan(&lockedJob); err != nil {
				return result, err
			}
			break
		}
	}

	var (
		status         string
		jobID          uuid.UUID
		workerID       *uuid.UUID
		claimedBy      *uuid.UUID
		currentAttempt int16
		resultRef      string
		resultSHA      string
		reportedMS     *int64
		reportedTokens *int64
		reportedTemp   *float32
		isHoneypot     bool
		isRedundancy   bool
		inputRef       string
		chunkIndex     int
	)
	if err := tx.QueryRow(ctx, `
		SELECT status,job_id,worker_id,claimed_by,COALESCE(retry_count,0),
		       COALESCE(result_ref,''),COALESCE(result_sha256,''),
		       reported_duration_ms,reported_tokens_used,reported_hardware_temp_c,
		       is_honeypot,is_redundancy,COALESCE(input_ref,''),COALESCE(chunk_index,0)
		  FROM tasks WHERE id=$1 FOR UPDATE`, info.TaskID).
		Scan(&status, &jobID, &workerID, &claimedBy, &currentAttempt,
			&resultRef, &resultSHA, &reportedMS, &reportedTokens, &reportedTemp,
			&isHoneypot, &isRedundancy, &inputRef, &chunkIndex); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return result, errNotFound
		}
		return result, err
	}
	if jobID != info.JobID {
		return result, fmt.Errorf("apply verification decision: task %s job changed from %s to %s", info.TaskID, info.JobID, jobID)
	}
	if fence != nil && (claimedBy == nil || *claimedBy != info.WorkerID ||
		resultRef != fence.Plan.Artifact.Key || resultSHA != fence.Plan.Artifact.SHA256 ||
		reportedMS == nil || *reportedMS != int64(info.DurationMS) ||
		reportedTokens == nil || *reportedTokens != int64(info.TokensUsed) ||
		!optionalFloat32Equal(reportedTemp, info.hardwareTempC) ||
		isHoneypot != info.IsHoneypot || isRedundancy != info.IsRedundancy ||
		inputRef != info.InputRef || chunkIndex != info.ChunkIndex) {
		return result, ErrVerificationWorkConflict
	}
	if err := validateVerificationSettlementTx(ctx, tx, info, decision.Outcome, entries); err != nil {
		return result, err
	}

	if status != "verifying" || currentAttempt != info.Attempt || workerID == nil || *workerID != info.WorkerID {
		replayErr := assertExactTaskVerdictTx(ctx, tx, info, decision.Outcome, decisionDigest, fence)
		if replayErr == nil {
			result.Rejected = decision.Outcome == OutcomeFail
			return result, nil
		}
		if !errors.Is(replayErr, pgx.ErrNoRows) && !errors.Is(replayErr, ErrVerificationReplayConflict) {
			return result, replayErr
		}
		return result, fmt.Errorf("%w: task %s is %s attempt %d worker %v, expected verifying attempt %d worker %s: %v",
			ErrVerificationReplayConflict, info.TaskID, status, currentAttempt, workerID, info.Attempt, info.WorkerID, replayErr)
	}

	var parentStatus string
	if err := tx.QueryRow(ctx, `SELECT status FROM jobs WHERE id=$1 FOR UPDATE`, info.JobID).Scan(&parentStatus); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return result, errNotFound
		}
		return result, err
	}
	if parentStatus != "queued" && parentStatus != "running" && parentStatus != "verifying" {
		return result, fmt.Errorf("%w: parent job %s is %s", ErrVerificationReplayConflict, info.JobID, parentStatus)
	}

	requeueEffects := 0
	for _, effect := range decision.Effects {
		if err := validateVerificationEffectTargetTx(ctx, tx, info, effect); err != nil {
			return result, fmt.Errorf("validate effect %s: %w", effect.ID, err)
		}
		switch effect.Kind {
		case VerificationEffectDockReputation:
			if err := dockReputationTx(ctx, tx, effect.SupplierID, effect.ReputationEvent); err != nil {
				return result, fmt.Errorf("apply %s: %w", effect.ID, err)
			}
		case VerificationEffectRecordEvent:
			if err := recordVerificationEffectTx(ctx, tx, info.Attempt, effect); err != nil {
				return result, fmt.Errorf("apply %s: %w", effect.ID, err)
			}
			if effect.EventKind == "tiebreak_win" && effect.TaskID != info.TaskID {
				if err := promoteProvisionalWinnerTx(ctx, tx, effect.ID, info.TaskID, effect.TaskID); err != nil {
					return result, fmt.Errorf("apply %s winner resolution: %w", effect.ID, err)
				}
			}
		case VerificationEffectClawbackCredit:
			if err := clawbackTaskCreditTx(ctx, tx, effect.ID, info.TaskID, effect.SupplierID, effect.TaskID, effect.TaskID != info.TaskID); err != nil {
				return result, fmt.Errorf("apply %s: %w", effect.ID, err)
			}
		case VerificationEffectQuarantine:
			if _, err := tx.Exec(ctx, `
				UPDATE suppliers
				   SET status='suspended',quarantined_at=COALESCE(quarantined_at,now())
				 WHERE id=$1 AND status <> 'banned'`, effect.SupplierID); err != nil {
				return result, fmt.Errorf("apply %s: %w", effect.ID, err)
			}
		case VerificationEffectRequeue:
			if effect.TaskID != info.TaskID {
				return result, fmt.Errorf("apply %s: verifier may only requeue current task %s, got %s", effect.ID, info.TaskID, effect.TaskID)
			}
			requeueEffects++ // executed after the immutable fail verdict is inserted
		case VerificationEffectInsertTiebreak:
			inserted, err := insertPlannedTiebreakTx(ctx, tx, info, effect)
			if err != nil {
				return result, fmt.Errorf("apply %s: %w", effect.ID, err)
			}
			if inserted {
				result.TiebreaksInserted++
			}
		default:
			return result, fmt.Errorf("apply verification decision: unknown effect %q", effect.Kind)
		}
		reachRecoveryBoundary(ctx, probe, BoundaryApplyAfterEffect)
	}

	if decision.Outcome == OutcomeFail {
		if requeueEffects != 1 {
			return result, fmt.Errorf("apply verification rejection: expected one requeue effect, got %d", requeueEffects)
		}
		if err := insertExactTaskVerdictTx(ctx, tx, info, decision.Outcome, decisionDigest, fence); err != nil {
			return result, err
		}
		reachRecoveryBoundary(ctx, probe, BoundaryRejectedAfterVerdict)
		backoff := requeueBackoff(int(info.Attempt))
		ct, err := tx.Exec(ctx, `
			UPDATE tasks
			   SET status='retrying',claimed_by=NULL,claimed_at=NULL,worker_id=NULL,
			       retry_count=retry_count+1,
			       visible_at=now()+make_interval(secs => $4),
			       excluded_worker=$3,
			       excluded_until=now()+make_interval(secs => $5)
			 WHERE id=$1 AND status='verifying' AND worker_id=$2 AND retry_count=$6`,
			info.TaskID, info.WorkerID, info.WorkerID, backoff.Seconds(),
			(backoff + requeueExclusionGrace).Seconds(), info.Attempt)
		if err != nil {
			return result, err
		}
		if ct.RowsAffected() != 1 {
			return result, fmt.Errorf("apply verification rejection: task transition lost")
		}
		reachRecoveryBoundary(ctx, probe, BoundaryRejectedAfterRequeue)
		if _, err := tx.Exec(ctx, `UPDATE jobs SET status='running' WHERE id=$1 AND status='verifying'`, info.JobID); err != nil {
			return result, err
		}
		reachRecoveryBoundary(ctx, probe, BoundaryRejectedAfterParentRunning)
		if fence != nil {
			if err := terminalizeVerificationWorkTx(ctx, tx, *fence, decision.Outcome, decisionDigest); err != nil {
				return result, err
			}
			reachRecoveryBoundary(ctx, probe, BoundaryRejectedAfterWorkTerminal)
		}
		reachRecoveryBoundary(ctx, probe, BoundaryRejectedBeforeDBCommit)
		if err := tx.Commit(ctx); err != nil {
			return result, err
		}
		reachRecoveryBoundary(ctx, probe, BoundaryRejectedAfterDBCommit)
		result.Applied = true
		result.Rejected = true
		return result, nil
	}
	if requeueEffects != 0 {
		return result, fmt.Errorf("apply verification acceptance: unexpected requeue effect")
	}

	settlementEntries, countsBuyerChunk, err := fenceHedgeSettlementTx(ctx, tx, info, entries)
	if err != nil {
		return result, err
	}
	ct, err := tx.Exec(ctx, `
		UPDATE tasks
		   SET status='complete',completed_at=now(),verification_outcome=$3,verified_at=now()
		 WHERE id=$1 AND worker_id=$2 AND status='verifying' AND retry_count=$4`,
		info.TaskID, info.WorkerID, string(decision.Outcome), info.Attempt)
	if err != nil {
		return result, err
	}
	if ct.RowsAffected() != 1 {
		return result, fmt.Errorf("apply verification acceptance: task transition lost")
	}
	reachRecoveryBoundary(ctx, probe, BoundaryAcceptedAfterTask)
	if err := insertExactTaskVerdictTx(ctx, tx, info, decision.Outcome, decisionDigest, fence); err != nil {
		return result, err
	}
	reachRecoveryBoundary(ctx, probe, BoundaryAcceptedAfterVerdict)
	if countsBuyerChunk {
		if _, err := tx.Exec(ctx, `UPDATE jobs SET tasks_done=tasks_done+1 WHERE id=$1`, info.JobID); err != nil {
			return result, err
		}
		reachRecoveryBoundary(ctx, probe, BoundaryAcceptedAfterJobCounter)
	}
	if _, err := tx.Exec(ctx, `UPDATE suppliers SET completed_tasks=completed_tasks+1 WHERE id=$1`, info.SupplierID); err != nil {
		return result, err
	}
	reachRecoveryBoundary(ctx, probe, BoundaryAcceptedAfterSupplierCounter)
	reachRecoveryBoundary(ctx, probe, BoundaryAcceptedAfterCounters)
	if _, err := tx.Exec(ctx, `
		INSERT INTO task_durations
		  (task_id,job_id,job_type,model_ref,split_size,duration_ms,worker_id,engine,build_hash)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9)`,
		info.TaskID, info.JobID, info.jobType, info.ModelRef, info.SplitSize,
		int64(info.DurationMS), info.WorkerID, info.engine, info.buildHash); err != nil {
		return result, err
	}
	reachRecoveryBoundary(ctx, probe, BoundaryAcceptedAfterDuration)
	if fence != nil {
		if err := terminalizeVerificationWorkTx(ctx, tx, *fence, decision.Outcome, decisionDigest); err != nil {
			return result, err
		}
		reachRecoveryBoundary(ctx, probe, BoundaryAcceptedAfterWorkTerminal)
	}
	for _, entry := range settlementEntries {
		if err := insertNewLedgerEntryTx(ctx, tx, entry); err != nil {
			return result, err
		}
		reachRecoveryBoundary(ctx, probe, BoundaryAcceptedAfterLedger)
	}
	if fence != nil {
		if err := recordChunkArtifactResolutionTx(ctx, tx, info, decision, decisionDigest, *fence, countsBuyerChunk); err != nil {
			return result, err
		}
		reachRecoveryBoundary(ctx, probe, BoundaryAcceptedAfterArtifactResolution)
	}
	if _, err := tx.Exec(ctx, `
		UPDATE tasks
		   SET status='failed',claimed_by=NULL
		 WHERE job_id=$1 AND COALESCE(chunk_index,0)=$2 AND id<>$3
		   AND status IN ('queued','running','retrying')
		   AND is_redundancy=false AND is_honeypot=false
		   AND (hedged_from IS NOT NULL OR EXISTS (
		         SELECT 1 FROM tasks h
		          WHERE h.id=$3 AND h.hedged_from=tasks.id AND h.is_redundancy=false
		       ))`, info.JobID, info.ChunkIndex, info.TaskID); err != nil {
		return result, err
	}
	reachRecoveryBoundary(ctx, probe, BoundaryAcceptedAfterSiblingCancel)

	reachRecoveryBoundary(ctx, probe, BoundaryAcceptedBeforeDBCommit)
	if err := tx.Commit(ctx); err != nil {
		return result, err
	}
	reachRecoveryBoundary(ctx, probe, BoundaryAcceptedAfterDBCommit)
	result.Applied = true
	return result, nil
}

func validateVerificationDecisionShape(info *CommitTaskInfo, decision VerificationDecision, entries []LedgerEntry) error {
	if decision.Failure != nil {
		if decision.Outcome != OutcomeFail || decision.Failure.Code == "" ||
			decision.Failure.JobType != info.jobType ||
			(decision.Failure.Kind != "artifact_invalid" && decision.Failure.Kind != "artifact_oversize" &&
				decision.Failure.Kind != "artifact_unavailable") {
			return fmt.Errorf("invalid typed verification failure %+v for outcome %q", decision.Failure, decision.Outcome)
		}
	}
	seen := make(map[uuid.UUID]struct{}, len(decision.Effects))
	failureEventSeen := false
	for i, effect := range decision.Effects {
		wantID := verificationEffectPayloadID(info.TaskID, info.Attempt, i, effect)
		if effect.ID == uuid.Nil || effect.ID != wantID {
			return fmt.Errorf("verification effect %d has non-canonical id %s (want %s)", i, effect.ID, wantID)
		}
		if _, duplicate := seen[effect.ID]; duplicate {
			return fmt.Errorf("duplicate verification effect id %s", effect.ID)
		}
		seen[effect.ID] = struct{}{}
		switch effect.Kind {
		case VerificationEffectDockReputation:
			if effect.SupplierID == uuid.Nil || !verificationReputationEventAllowed(effect.ReputationEvent) {
				return fmt.Errorf("invalid reputation effect %s/%q", effect.SupplierID, effect.ReputationEvent)
			}
		case VerificationEffectRecordEvent:
			if effect.JobID != info.JobID || effect.TaskID == uuid.Nil || effect.SupplierID == uuid.Nil || !verificationEventKindAllowed(effect.EventKind) {
				return fmt.Errorf("invalid verification event effect %+v", effect)
			}
			if decision.Failure != nil && effect.EventKind == decision.Failure.Kind {
				failureEventSeen = true
			}
		case VerificationEffectClawbackCredit:
			if effect.TaskID == uuid.Nil || effect.SupplierID == uuid.Nil {
				return fmt.Errorf("invalid clawback effect %+v", effect)
			}
		case VerificationEffectQuarantine:
			if effect.SupplierID != info.SupplierID {
				return fmt.Errorf("quarantine effect targets %s, want committing supplier %s", effect.SupplierID, info.SupplierID)
			}
		case VerificationEffectRequeue:
			if effect.TaskID != info.TaskID {
				return fmt.Errorf("requeue effect targets %s, want current task %s", effect.TaskID, info.TaskID)
			}
		case VerificationEffectInsertTiebreak:
			if effect.TaskID != effect.ID || effect.JobID != info.JobID || effect.PrimaryTaskID != info.TaskID ||
				effect.PeerWorkerID == uuid.Nil || effect.InputRef != info.InputRef || effect.ChunkIndex != info.ChunkIndex {
				return fmt.Errorf("invalid tiebreak effect %+v", effect)
			}
		default:
			return fmt.Errorf("unknown verification effect %q", effect.Kind)
		}
	}
	if decision.Failure != nil && !failureEventSeen {
		return fmt.Errorf("typed verification failure %q has no matching durable event", decision.Failure.Kind)
	}

	if decision.Outcome == OutcomeFail || decision.Outcome == OutcomeLossNoPayout {
		if len(entries) != 0 {
			return fmt.Errorf("non-payable outcome %q has %d settlement entries", decision.Outcome, len(entries))
		}
		return nil
	}
	if len(entries) != 3 {
		return fmt.Errorf("payable outcome %q requires exactly three settlement entries, got %d", decision.Outcome, len(entries))
	}
	byKind := make(map[string]LedgerEntry, 3)
	for _, entry := range entries {
		if entry.TaskID == nil || *entry.TaskID != info.TaskID {
			return fmt.Errorf("settlement %q is not bound to task %s", entry.Kind, info.TaskID)
		}
		if _, duplicate := byKind[entry.Kind]; duplicate {
			return fmt.Errorf("duplicate settlement kind %q", entry.Kind)
		}
		byKind[entry.Kind] = entry
	}
	buyer, okBuyer := byKind[KindBuyerCharge]
	supplier, okSupplier := byKind[KindSupplierCredit]
	platform, okPlatform := byKind[KindPlatformTake]
	if !okBuyer || !okSupplier || !okPlatform || buyer.BuyerID == nil || buyer.SupplierID != nil ||
		supplier.SupplierID == nil || supplier.BuyerID != nil || platform.SupplierID != nil || platform.BuyerID != nil ||
		buyer.AmountUSD >= 0 || supplier.AmountUSD < 0 || platform.AmountUSD < 0 ||
		buyer.PayoutStatus != PayoutReleased || platform.PayoutStatus != PayoutReleased ||
		supplier.PayoutStatus != PayoutHeld || supplier.ReleaseAt == nil {
		return fmt.Errorf("invalid three-row verification settlement shape")
	}
	return nil
}

func verificationReputationEventAllowed(event ReputationEvent) bool {
	switch event {
	case EventTaskSuccess, EventHoneypotPass, EventRedundancyMatch, EventMismatch, EventHoneypotFail, EventTimeout,
		EventResultCorrupt, EventArtifactOversize:
		return true
	default:
		return false
	}
}

func verificationEventKindAllowed(kind string) bool {
	switch kind {
	case "honeypot_pass", "honeypot_fail", "redundancy_match", "redundancy_mismatch",
		"redundancy_cross_class", "redundancy_same_supplier", "tiebreak_win",
		"tiebreak_loss", "tiebreak_cross_class", "artifact_invalid", "artifact_oversize", "artifact_unavailable":
		return true
	default:
		return false
	}
}

func validateVerificationSettlementTx(ctx context.Context, tx pgx.Tx, info *CommitTaskInfo, outcome VerifyOutcome, entries []LedgerEntry) error {
	if outcome == OutcomeFail || outcome == OutcomeLossNoPayout {
		return nil
	}
	var buyerID uuid.UUID
	var buyerCharge, supplierPayout float64
	if err := tx.QueryRow(ctx, `
		SELECT j.buyer_id,t.economic_buyer_charge_usd::float8,t.economic_supplier_payout_usd::float8
		  FROM tasks t JOIN jobs j ON j.id=t.job_id WHERE t.id=$1`, info.TaskID).
		Scan(&buyerID, &buyerCharge, &supplierPayout); err != nil {
		return err
	}
	want := map[string]struct {
		amount   string
		buyer    *uuid.UUID
		supplier *uuid.UUID
	}{
		KindBuyerCharge:    {amount: fmt.Sprintf("%.6f", -buyerCharge), buyer: &buyerID},
		KindSupplierCredit: {amount: fmt.Sprintf("%.6f", supplierPayout), supplier: &info.SupplierID},
		KindPlatformTake:   {amount: fmt.Sprintf("%.6f", buyerCharge-supplierPayout)},
	}
	for _, entry := range entries {
		expected, ok := want[entry.Kind]
		if !ok || fmt.Sprintf("%.6f", entry.AmountUSD) != expected.amount ||
			!sameOptionalUUID(entry.BuyerID, expected.buyer) || !sameOptionalUUID(entry.SupplierID, expected.supplier) {
			return fmt.Errorf("settlement %q conflicts with frozen task economics", entry.Kind)
		}
	}
	return nil
}

func validateVerificationEffectTargetTx(ctx context.Context, tx pgx.Tx, info *CommitTaskInfo, effect VerificationEffect) error {
	switch effect.Kind {
	case VerificationEffectDockReputation:
		if effect.SupplierID == info.SupplierID {
			return nil
		}
		var exists bool
		if err := tx.QueryRow(ctx, `
			SELECT EXISTS (
			 SELECT 1 FROM tasks t
			 LEFT JOIN verification_work vw ON vw.task_id=t.id AND vw.attempt=t.retry_count
			  WHERE t.job_id=$1 AND COALESCE(t.chunk_index,0)=$2
			    AND COALESCE(vw.supplier_id,t.execution_supplier_id)=$3
			)`, info.JobID, info.ChunkIndex, effect.SupplierID).Scan(&exists); err != nil {
			return err
		}
		if !exists {
			return fmt.Errorf("supplier %s did not execute this chunk", effect.SupplierID)
		}
	case VerificationEffectRecordEvent, VerificationEffectClawbackCredit:
		var jobID uuid.UUID
		var chunk int
		var supplierID *uuid.UUID
		if err := tx.QueryRow(ctx, `
			SELECT t.job_id,COALESCE(t.chunk_index,0),
			       COALESCE(vw.supplier_id,t.execution_supplier_id)
			  FROM tasks t
			  LEFT JOIN verification_work vw ON vw.task_id=t.id AND vw.attempt=t.retry_count
			 WHERE t.id=$1`, effect.TaskID).
			Scan(&jobID, &chunk, &supplierID); err != nil {
			return err
		}
		if jobID != info.JobID || chunk != info.ChunkIndex || supplierID == nil || *supplierID != effect.SupplierID {
			return fmt.Errorf("effect target task %s is outside committing chunk/supplier", effect.TaskID)
		}
	case VerificationEffectInsertTiebreak:
	}
	return nil
}

func insertExactTaskVerdictTx(ctx context.Context, tx pgx.Tx, info *CommitTaskInfo, outcome VerifyOutcome, decisionDigest string, fence *verificationApplyFence) error {
	var workID any
	var artifactKey, artifactSHA any
	if fence != nil {
		workID = fence.Lease.WorkID
		artifactKey = fence.Plan.Artifact.Key
		artifactSHA = fence.Plan.Artifact.SHA256
	}
	ct, err := tx.Exec(ctx, `
		INSERT INTO task_verdicts
		  (task_id,attempt,job_id,supplier_id,outcome,result_sha256,decision_version,decision_sha256,
		   verification_work_id,artifact_key,artifact_sha256)
		VALUES ($1,$2,$3,$4,$5,NULLIF($6,''),1,$7,$8,$9,$10)
		ON CONFLICT (task_id,attempt) DO NOTHING`,
		info.TaskID, info.Attempt, info.JobID, info.SupplierID, string(outcome), info.ResultSHA256, decisionDigest,
		workID, artifactKey, artifactSHA)
	if err != nil {
		return err
	}
	if ct.RowsAffected() != 1 {
		if err := assertExactTaskVerdictTx(ctx, tx, info, outcome, decisionDigest, fence); err != nil {
			return fmt.Errorf("conflicting task verdict for %s attempt %d: %w", info.TaskID, info.Attempt, err)
		}
		return fmt.Errorf("task verdict for %s attempt %d already existed before terminal transition", info.TaskID, info.Attempt)
	}
	return nil
}

func assertExactTaskVerdictTx(ctx context.Context, tx pgx.Tx, info *CommitTaskInfo, outcome VerifyOutcome, decisionDigest string, fence *verificationApplyFence) error {
	var (
		jobID              uuid.UUID
		supplierID         *uuid.UUID
		gotOutcome         string
		gotSHA             string
		gotDecisionVersion int
		gotDecisionSHA     string
		gotWorkID          *uuid.UUID
		gotArtifactKey     string
		gotArtifactSHA     string
	)
	err := tx.QueryRow(ctx, `
		SELECT job_id,supplier_id,outcome,COALESCE(result_sha256,''),
		       COALESCE(decision_version,0),COALESCE(decision_sha256,''),verification_work_id,
		       COALESCE(artifact_key,''),COALESCE(artifact_sha256,'')
		  FROM task_verdicts WHERE task_id=$1 AND attempt=$2`, info.TaskID, info.Attempt).
		Scan(&jobID, &supplierID, &gotOutcome, &gotSHA, &gotDecisionVersion, &gotDecisionSHA,
			&gotWorkID, &gotArtifactKey, &gotArtifactSHA)
	if err != nil {
		return err
	}
	if jobID != info.JobID || supplierID == nil || *supplierID != info.SupplierID ||
		gotOutcome != string(outcome) || gotSHA != info.ResultSHA256 ||
		gotDecisionVersion != 1 || gotDecisionSHA != decisionDigest {
		return fmt.Errorf("%w: durable verdict mismatch job=%s supplier=%v outcome=%q sha=%q decision=v%d/%q",
			ErrVerificationReplayConflict,
			jobID, supplierID, gotOutcome, gotSHA, gotDecisionVersion, gotDecisionSHA)
	}
	if fence == nil {
		if gotWorkID != nil || gotArtifactKey != "" || gotArtifactSHA != "" {
			return fmt.Errorf("%w: durable verdict unexpectedly has verification-work authority", ErrVerificationReplayConflict)
		}
	} else if gotWorkID == nil || *gotWorkID != fence.Lease.WorkID ||
		gotArtifactKey != fence.Plan.Artifact.Key || gotArtifactSHA != fence.Plan.Artifact.SHA256 {
		return fmt.Errorf("%w: durable verdict verification-work authority mismatch", ErrVerificationReplayConflict)
	}
	return nil
}

func lockVerificationWorkFenceTx(ctx context.Context, tx pgx.Tx, info *CommitTaskInfo, fence verificationApplyFence) error {
	if err := normalizeVerificationLease(fence.Lease); err != nil {
		return err
	}
	var (
		taskID, jobID, workerID, supplierID uuid.UUID
		attempt                             int64
		status, owner, snapshotSHA          string
		token                               uuid.UUID
		leaseLive                           bool
		artifactKey, artifactSHA            string
		artifactBytes                       int64
		samplingPolicy, samplingProbability string
		samplingSelected                    bool
		planDigest                          string
	)
	err := tx.QueryRow(ctx, `
		SELECT w.task_id,w.attempt,w.job_id,w.worker_id,w.supplier_id,w.status,
		       COALESCE(w.lease_owner,''),w.lease_token,w.lease_expires_at>now(),w.snapshot_sha256,
		       COALESCE(w.artifact_key,''),COALESCE(w.artifact_sha256,''),COALESCE(w.artifact_bytes,-1),
		       COALESCE(w.sampling_policy,''),COALESCE(w.sampling_probability,''),COALESCE(w.sampling_selected,false),
		       p.decision_sha256
		  FROM verification_work w JOIN verification_work_plans p ON p.work_id=w.id
		 WHERE w.id=$1 FOR UPDATE OF w`, fence.Lease.WorkID).
		Scan(&taskID, &attempt, &jobID, &workerID, &supplierID, &status, &owner, &token,
			&leaseLive, &snapshotSHA, &artifactKey, &artifactSHA, &artifactBytes,
			&samplingPolicy, &samplingProbability, &samplingSelected, &planDigest)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return ErrVerificationLeaseLost
		}
		return err
	}
	if status != VerificationWorkLeased || owner != fence.Lease.Owner || token != fence.Lease.Token || !leaseLive ||
		taskID != info.TaskID || attempt != int64(info.Attempt) || jobID != info.JobID || workerID != info.WorkerID ||
		supplierID != info.SupplierID || snapshotSHA != fence.Plan.SnapshotSHA256 || planDigest != fence.Plan.DecisionSHA256 ||
		artifactKey != fence.Plan.Artifact.Key || artifactSHA != fence.Plan.Artifact.SHA256 || artifactBytes != fence.Plan.Artifact.Bytes ||
		samplingPolicy != fence.Plan.SamplingPolicy ||
		samplingProbability != strconv.FormatFloat(fence.Plan.SamplingProbability, 'g', 17, 64) ||
		samplingSelected != fence.Plan.SamplingSelected ||
		info.ResultKey != artifactKey || info.ResultSHA256 != artifactSHA {
		return ErrVerificationWorkConflict
	}
	return nil
}

func terminalizeVerificationWorkTx(ctx context.Context, tx pgx.Tx, fence verificationApplyFence, outcome VerifyOutcome, digest string) error {
	ct, err := tx.Exec(ctx, `
		UPDATE verification_work
		   SET status='terminal',terminal_outcome=$4,decision_sha256=$5,terminal_at=now(),
		       lease_owner=NULL,lease_token=NULL,lease_expires_at=NULL,updated_at=now()
		 WHERE id=$1 AND status='leased' AND lease_owner=$2 AND lease_token=$3
		   AND lease_expires_at>now() AND artifact_key=$6 AND artifact_sha256=$7 AND artifact_bytes=$8`,
		fence.Lease.WorkID, fence.Lease.Owner, fence.Lease.Token, string(outcome), digest,
		fence.Plan.Artifact.Key, fence.Plan.Artifact.SHA256, fence.Plan.Artifact.Bytes)
	if err != nil {
		return err
	}
	if ct.RowsAffected() != 1 {
		return ErrVerificationLeaseLost
	}
	return nil
}

type chunkWinnerCandidate struct {
	taskID       uuid.UUID
	workID       uuid.UUID
	artifact     VerificationArtifact
	isRedundancy bool
	completedAt  time.Time
}

func recordChunkArtifactResolutionTx(ctx context.Context, tx pgx.Tx, info *CommitTaskInfo, decision VerificationDecision, digest string, fence verificationApplyFence, countsBuyerChunk bool) error {
	winnerIDs := make(map[uuid.UUID]struct{})
	for _, effect := range decision.Effects {
		if effect.Kind == VerificationEffectRecordEvent && effect.EventKind == "tiebreak_win" {
			winnerIDs[effect.TaskID] = struct{}{}
		}
	}
	basis := "majority"
	if len(winnerIDs) == 0 {
		if !countsBuyerChunk {
			return nil
		}
		var isRedundancy, isHoneypot bool
		if err := tx.QueryRow(ctx, `SELECT is_redundancy,is_honeypot FROM tasks WHERE id=$1`, info.TaskID).
			Scan(&isRedundancy, &isHoneypot); err != nil {
			return err
		}
		if isRedundancy || isHoneypot {
			return nil
		}
		basis = "provisional"
		winnerIDs[info.TaskID] = struct{}{}
	}

	candidates := make([]chunkWinnerCandidate, 0, len(winnerIDs))
	for taskID := range winnerIDs {
		if taskID == info.TaskID {
			var isRedundancy bool
			if err := tx.QueryRow(ctx, `SELECT is_redundancy FROM tasks WHERE id=$1`, taskID).Scan(&isRedundancy); err != nil {
				return err
			}
			candidates = append(candidates, chunkWinnerCandidate{
				taskID: taskID, workID: fence.Lease.WorkID, artifact: fence.Plan.Artifact,
				isRedundancy: isRedundancy, completedAt: time.Date(9999, 1, 1, 0, 0, 0, 0, time.UTC),
			})
			continue
		}
		var c chunkWinnerCandidate
		c.taskID = taskID
		if err := tx.QueryRow(ctx, `
			SELECT vw.id,vw.artifact_key,vw.artifact_sha256,vw.artifact_bytes,
			       t.is_redundancy,COALESCE(t.completed_at,now())
			  FROM tasks t JOIN verification_work vw
			    ON vw.task_id=t.id AND vw.attempt=t.retry_count AND vw.status='terminal'
			 WHERE t.id=$1 AND t.job_id=$2 AND COALESCE(t.chunk_index,0)=$3
			   AND t.verification_outcome IN ('pass','pass_with_penalty')`, taskID, info.JobID, info.ChunkIndex).
			Scan(&c.workID, &c.artifact.Key, &c.artifact.SHA256, &c.artifact.Bytes,
				&c.isRedundancy, &c.completedAt); err != nil {
			return fmt.Errorf("load majority winner %s: %w", taskID, err)
		}
		candidates = append(candidates, c)
	}
	if len(candidates) == 0 {
		return fmt.Errorf("verification decision had no deliverable winner")
	}
	sort.Slice(candidates, func(i, j int) bool {
		if candidates[i].isRedundancy != candidates[j].isRedundancy {
			return !candidates[i].isRedundancy
		}
		if !candidates[i].completedAt.Equal(candidates[j].completedAt) {
			return candidates[i].completedAt.Before(candidates[j].completedAt)
		}
		return candidates[i].taskID.String() < candidates[j].taskID.String()
	})
	winner := candidates[0]
	effectID := uuid.NewSHA1(verificationResolutionNamespace, []byte(fmt.Sprintf(
		"chunk:%s:%d:%s:%s:%s", info.JobID, info.ChunkIndex, basis, winner.taskID, digest)))
	ct, err := tx.Exec(ctx, `
		INSERT INTO chunk_artifact_resolutions
		 (effect_id,job_id,chunk_index,winner_task_id,verification_work_id,
		  artifact_key,artifact_sha256,artifact_bytes,basis)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9)
		ON CONFLICT (job_id,chunk_index,basis) DO NOTHING`,
		effectID, info.JobID, info.ChunkIndex, winner.taskID, winner.workID,
		winner.artifact.Key, winner.artifact.SHA256, winner.artifact.Bytes, basis)
	if err != nil {
		return err
	}
	if ct.RowsAffected() == 1 {
		return nil
	}
	if basis == "provisional" {
		return nil
	}
	var existingTask, existingWork uuid.UUID
	var key, sha string
	var size int64
	if err := tx.QueryRow(ctx, `
		SELECT winner_task_id,verification_work_id,artifact_key,artifact_sha256,artifact_bytes
		  FROM chunk_artifact_resolutions WHERE job_id=$1 AND chunk_index=$2 AND basis=$3`,
		info.JobID, info.ChunkIndex, basis).
		Scan(&existingTask, &existingWork, &key, &sha, &size); err != nil {
		return err
	}
	if existingTask != winner.taskID || existingWork != winner.workID || key != winner.artifact.Key ||
		sha != winner.artifact.SHA256 || size != winner.artifact.Bytes {
		return ErrVerificationWorkConflict
	}
	return nil
}

type canonicalVerificationSettlement struct {
	Kind         string `json:"kind"`
	SupplierID   string `json:"supplier_id,omitempty"`
	BuyerID      string `json:"buyer_id,omitempty"`
	TaskID       string `json:"task_id,omitempty"`
	AmountUSD    string `json:"amount_usd"`
	PayoutStatus string `json:"payout_status"`
	ReleaseAt    string `json:"release_at,omitempty"`
}

func verificationDecisionDigest(decision VerificationDecision, entries []LedgerEntry) (string, error) {
	settlement := canonicalVerificationSettlements(entries)
	envelope := struct {
		Version    int                               `json:"version"`
		Decision   VerificationDecision              `json:"decision"`
		Settlement []canonicalVerificationSettlement `json:"settlement"`
	}{Version: 1, Decision: decision, Settlement: settlement}
	b, err := json.Marshal(envelope)
	if err != nil {
		return "", fmt.Errorf("marshal verification decision: %w", err)
	}
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:]), nil
}

func canonicalVerificationSettlements(entries []LedgerEntry) []canonicalVerificationSettlement {
	settlement := make([]canonicalVerificationSettlement, 0, len(entries))
	for _, entry := range entries {
		row := canonicalVerificationSettlement{
			Kind: entry.Kind, SupplierID: optionalUUIDString(entry.SupplierID),
			BuyerID: optionalUUIDString(entry.BuyerID), TaskID: optionalUUIDString(entry.TaskID),
			AmountUSD: fmt.Sprintf("%.6f", entry.AmountUSD), PayoutStatus: entry.PayoutStatus,
		}
		if entry.ReleaseAt != nil {
			row.ReleaseAt = entry.ReleaseAt.UTC().Format(time.RFC3339Nano)
		}
		settlement = append(settlement, row)
	}
	sort.Slice(settlement, func(i, j int) bool {
		if settlement[i].Kind != settlement[j].Kind {
			return settlement[i].Kind < settlement[j].Kind
		}
		return settlement[i].TaskID < settlement[j].TaskID
	})
	return settlement
}

func optionalUUIDString(id *uuid.UUID) string {
	if id == nil {
		return ""
	}
	return id.String()
}

func dockReputationTx(ctx context.Context, tx pgx.Tx, supplierID uuid.UUID, event ReputationEvent) error {
	var current float32
	if err := tx.QueryRow(ctx, `SELECT reputation FROM suppliers WHERE id=$1 FOR UPDATE`, supplierID).Scan(&current); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return errNotFound
		}
		return err
	}
	next := updateReputation(current, event)
	switch {
	case next <= 0:
		_, err := tx.Exec(ctx, `UPDATE suppliers SET reputation=0.0,status='banned' WHERE id=$1`, supplierID)
		return err
	case next < quarantineRepFloor:
		_, err := tx.Exec(ctx, `
			UPDATE suppliers
			   SET reputation=$2,
			       status=CASE WHEN status='active' THEN 'suspended' ELSE status END,
			       quarantined_at=CASE WHEN status='active' THEN now() ELSE quarantined_at END
			 WHERE id=$1`, supplierID, next)
		return err
	default:
		_, err := tx.Exec(ctx, `UPDATE suppliers SET reputation=$2 WHERE id=$1`, supplierID, next)
		return err
	}
}

func recordVerificationEffectTx(ctx context.Context, tx pgx.Tx, attempt int16, effect VerificationEffect) error {
	ct, err := tx.Exec(ctx, `
		INSERT INTO verification_events
		  (id,effect_id,attempt,job_id,task_id,supplier_id,kind)
		VALUES ($1,$1,$2,$3,NULLIF($4,'00000000-0000-0000-0000-000000000000'::uuid),
		        NULLIF($5,'00000000-0000-0000-0000-000000000000'::uuid),$6)
		ON CONFLICT (effect_id) DO NOTHING`,
		effect.ID, attempt, effect.JobID, effect.TaskID, effect.SupplierID, effect.EventKind)
	if err != nil {
		return err
	}
	if ct.RowsAffected() != 1 {
		return fmt.Errorf("verification effect %s existed before terminal transition", effect.ID)
	}
	return nil
}

func promoteProvisionalWinnerTx(ctx context.Context, tx pgx.Tx, effectID, sourceTaskID, taskID uuid.UUID) error {
	ct, err := tx.Exec(ctx, `
		UPDATE tasks SET verification_outcome='pass',verified_at=now()
		 WHERE id=$1 AND status='complete' AND verification_outcome='pass_with_penalty'`, taskID)
	if err != nil {
		return err
	}
	if ct.RowsAffected() == 0 {
		var outcome string
		if err := tx.QueryRow(ctx, `SELECT COALESCE(verification_outcome,'') FROM tasks WHERE id=$1`, taskID).Scan(&outcome); err != nil {
			return err
		}
		if outcome == "pass" {
			return nil
		}
		return fmt.Errorf("cannot promote task %s from outcome %q", taskID, outcome)
	}
	_, err = tx.Exec(ctx, `
		INSERT INTO task_verdict_resolutions (effect_id,task_id,source_task_id,kind)
		VALUES ($1,$2,$3,'promoted_pass') ON CONFLICT (effect_id) DO NOTHING`,
		effectID, taskID, sourceTaskID)
	return err
}

func clawbackTaskCreditTx(ctx context.Context, tx pgx.Tx, effectID, sourceTaskID, supplierID, taskID uuid.UUID, correctProjection bool) error {
	var (
		creditID       uuid.UUID
		credited       float64
		creditState    string
		payoutRef      string
		outcomeUnknown bool
	)
	err := tx.QueryRow(ctx, `
		SELECT le.id,le.amount_usd::float8,le.payout_status,
		       COALESCE(le.payout_ref,''),COALESCE(op.outcome_unknown,false)
		  FROM ledger_entries le
		  LEFT JOIN supplier_payout_operations op ON op.ledger_entry_id=le.id
		 WHERE le.supplier_id=$1 AND le.task_id=$2
		   AND le.kind='supplier_credit' AND le.amount_usd>0
		 ORDER BY le.created_at,le.id LIMIT 1 FOR UPDATE OF le`, supplierID, taskID).
		Scan(&creditID, &credited, &creditState, &payoutRef, &outcomeUnknown)
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return err
	}
	if err == nil && credited > 0 {
		cb := clawbackEntry(supplierID, taskID, credited)
		if err := insertLedgerEntryIfAbsentExactTx(ctx, tx, cb); err != nil {
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
		if _, err := tx.Exec(ctx, `UPDATE ledger_entries SET payout_status=$2 WHERE id=$1`, creditID, nextState); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `
			UPDATE supplier_payout_operations
			   SET status=$2,updated_at=now(),
			       last_error=CASE WHEN $2='reversal_required'
			                       THEN 'confirmed clawback requires external recovery'
			                       ELSE last_error END
			 WHERE ledger_entry_id=$1`, creditID, nextState); err != nil {
			return err
		}
	}
	if correctProjection {
		if _, err := tx.Exec(ctx, `UPDATE tasks SET verification_outcome='clawed_back',verified_at=now() WHERE id=$1`, taskID); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `
			INSERT INTO task_verdict_resolutions (effect_id,task_id,source_task_id,kind)
			VALUES ($1,$2,$3,'clawed_back') ON CONFLICT (effect_id) DO NOTHING`,
			effectID, taskID, sourceTaskID); err != nil {
			return err
		}
	}
	return nil
}

var verificationResolutionNamespace = uuid.MustParse("75565216-8f40-54c6-8dca-f056f434c03b")

func verificationResolutionID(taskID uuid.UUID, kind string, sourceTaskID uuid.UUID) uuid.UUID {
	data := append([]byte(kind+":"), taskID[:]...)
	data = append(data, sourceTaskID[:]...)
	return uuid.NewSHA1(verificationResolutionNamespace, data)
}

func insertPlannedTiebreakTx(ctx context.Context, tx pgx.Tx, info *CommitTaskInfo, effect VerificationEffect) (bool, error) {
	var reserved, consumed int
	if err := tx.QueryRow(ctx, `
		SELECT reserved_tasks,consumed_tasks FROM job_economic_reserves
		 WHERE job_id=$1 FOR UPDATE`, effect.JobID).Scan(&reserved, &consumed); err != nil {
		return false, err
	}
	var existing uuid.UUID
	err := tx.QueryRow(ctx, `
		SELECT id FROM tasks
		 WHERE job_id=$1 AND COALESCE(chunk_index,0)=$2
		   AND hedged_from IS NOT NULL AND is_redundancy=true
		 ORDER BY created_at,id LIMIT 1`, effect.JobID, effect.ChunkIndex).Scan(&existing)
	if err == nil {
		return false, nil
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return false, err
	}
	if consumed >= reserved {
		return false, ErrEconomicReserveExhausted
	}
	buyerCharge, supplierPayout, err := consumeEconomicReserveTx(ctx, tx, effect.JobID)
	if err != nil {
		return false, err
	}
	if info == nil || info.HWClass == "" {
		return false, errors.New("planned tiebreak requires frozen verification class")
	}
	resultKey := fmt.Sprintf("jobs/%s/tiebreak/%s/result.json", effect.JobID, effect.TaskID)
	if _, err := tx.Exec(ctx, `
		INSERT INTO tasks
		  (id,job_id,status,is_honeypot,is_redundancy,retry_count,input_ref,result_key,
		   chunk_index,hedged_from,expected_output_records,
		   verification_hw_class,verification_engine,verification_build_hash,
		   claimed_by,claimed_at,visible_at,
		   economic_buyer_charge_usd,economic_supplier_payout_usd)
		VALUES ($1,$2,'queued',false,true,0,$3,$4,$5,$6,
		        (SELECT expected_output_records FROM tasks WHERE id=$6),
		        $7,$8,$9,NULL,NULL,now(),$10,$11)`,
		effect.TaskID, effect.JobID, effect.InputRef, resultKey, effect.ChunkIndex,
		effect.PrimaryTaskID, info.HWClass, info.engine, info.buildHash,
		buyerCharge, supplierPayout); err != nil {
		return false, err
	}
	eligible, err := tiebreakPeerClaimEligibleTx(ctx, tx, effect.TaskID, effect.JobID, effect.PeerWorkerID)
	if err != nil {
		return false, err
	}
	if eligible {
		if _, err := tx.Exec(ctx, `
			UPDATE tasks SET claimed_by=$2,claimed_at=now()
			 WHERE id=$1 AND claimed_by IS NULL AND started_at IS NULL`, effect.TaskID, effect.PeerWorkerID); err != nil {
			return false, err
		}
	}
	if _, err := tx.Exec(ctx, `
		UPDATE jobs
		   SET task_count=task_count+1,
		       status=CASE WHEN status='verifying' THEN 'running' ELSE status END
		 WHERE id=$1`, effect.JobID); err != nil {
		return false, err
	}
	return true, nil
}

func fenceHedgeSettlementTx(ctx context.Context, tx pgx.Tx, info *CommitTaskInfo, entries []LedgerEntry) ([]LedgerEntry, bool, error) {
	var hedgedFrom *uuid.UUID
	var isRedundancy bool
	if err := tx.QueryRow(ctx, `SELECT hedged_from,is_redundancy FROM tasks WHERE id=$1`, info.TaskID).
		Scan(&hedgedFrom, &isRedundancy); err != nil {
		return nil, false, err
	}
	if isRedundancy {
		return entries, true, nil
	}
	rootTaskID := info.TaskID
	if hedgedFrom != nil {
		rootTaskID = *hedgedFrom
	}
	var locked uuid.UUID
	if err := tx.QueryRow(ctx, `SELECT id FROM tasks WHERE id=$1 FOR UPDATE`, rootTaskID).Scan(&locked); err != nil {
		return nil, false, err
	}
	var siblingSettled bool
	if err := tx.QueryRow(ctx, `
		SELECT EXISTS (
		 SELECT 1 FROM ledger_entries le JOIN tasks sibling ON sibling.id=le.task_id
		  WHERE le.kind='buyer_charge' AND sibling.is_redundancy=false
		    AND sibling.id<>$2 AND (sibling.id=$1 OR sibling.hedged_from=$1)
		)`, rootTaskID, info.TaskID).Scan(&siblingSettled); err != nil {
		return nil, false, err
	}
	if siblingSettled {
		return nil, false, nil
	}
	return entries, true, nil
}

func insertNewLedgerEntryTx(ctx context.Context, tx pgx.Tx, entry LedgerEntry) error {
	ct, err := tx.Exec(ctx, `
		INSERT INTO ledger_entries
		  (kind,supplier_id,buyer_id,task_id,amount_usd,payout_status,release_at)
		VALUES ($1,$2,$3,$4,$5,$6,$7) ON CONFLICT (task_id,kind) DO NOTHING`,
		entry.Kind, entry.SupplierID, entry.BuyerID, entry.TaskID,
		entry.AmountUSD, entry.PayoutStatus, entry.ReleaseAt)
	if err != nil {
		return err
	}
	if ct.RowsAffected() != 1 {
		return fmt.Errorf("ledger row %s/%v existed before terminal transition", entry.Kind, entry.TaskID)
	}
	return nil
}

func insertLedgerEntryIfAbsentExactTx(ctx context.Context, tx pgx.Tx, entry LedgerEntry) error {
	ct, err := tx.Exec(ctx, `
		INSERT INTO ledger_entries
		  (kind,supplier_id,buyer_id,task_id,amount_usd,payout_status,release_at)
		VALUES ($1,$2,$3,$4,$5,$6,$7) ON CONFLICT (task_id,kind) DO NOTHING`,
		entry.Kind, entry.SupplierID, entry.BuyerID, entry.TaskID,
		entry.AmountUSD, entry.PayoutStatus, entry.ReleaseAt)
	if err != nil || ct.RowsAffected() == 1 {
		return err
	}
	var amount float64
	var supplierID, taskID *uuid.UUID
	var kind string
	if err := tx.QueryRow(ctx, `
		SELECT kind,supplier_id,task_id,amount_usd::float8 FROM ledger_entries
		 WHERE task_id=$1 AND kind=$2`, entry.TaskID, entry.Kind).
		Scan(&kind, &supplierID, &taskID, &amount); err != nil {
		return err
	}
	if kind != entry.Kind || amount != entry.AmountUSD || !sameOptionalUUID(supplierID, entry.SupplierID) || !sameOptionalUUID(taskID, entry.TaskID) {
		return fmt.Errorf("conflicting existing ledger row %s/%v", entry.Kind, entry.TaskID)
	}
	return nil
}

func sameOptionalUUID(a, b *uuid.UUID) bool {
	if a == nil || b == nil {
		return a == nil && b == nil
	}
	return *a == *b
}
