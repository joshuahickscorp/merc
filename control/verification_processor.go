package main

import (
	"context"
	"crypto/sha256"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"os"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

const (
	verificationLeaseDurationDefault                = 5 * time.Minute
	verificationLeaseRenewalDefault                 = verificationLeaseDurationDefault / 3
	verificationRetryDelay                          = time.Second
	verificationArtifactUnavailableMaxLeaseAttempts = 5
)

var ErrVerificationStagingArtifactMissing = errors.New("verification staging artifact is missing")

var ErrVerificationChunkBusy = errors.New("verification chunk is already being planned")

type VerificationProcessResult struct {
	Info    *CommitTaskInfo
	Outcome VerifyOutcome
	Applied VerificationApplyResult
	Pending bool
}

type VerificationProcessor struct {
	store    *Store
	storage  *Storage
	verifier *Verifier
	owner    string
	probe    recoveryBoundaryProbe

	leaseDuration time.Duration
	leaseRenewal  time.Duration
}

func NewVerificationProcessor(store *Store, storage *Storage, verifier *Verifier) *VerificationProcessor {
	host, _ := os.Hostname()
	if host == "" {
		host = "control"
	}
	return &VerificationProcessor{
		store: store, storage: storage, verifier: verifier,
		owner:         fmt.Sprintf("%s/%d/%s", host, os.Getpid(), uuid.New()),
		leaseDuration: verificationLeaseDurationDefault,
		leaseRenewal:  verificationLeaseRenewalDefault,
	}
}

func (p *VerificationProcessor) ProcessAttempt(ctx context.Context, taskID uuid.UUID, attempt int64) (VerificationProcessResult, error) {
	if p.store == nil || p.store.verificationResources == nil || p.store.verificationResources.maxConns < 2 {
		return VerificationProcessResult{}, ErrVerificationPoolTooSmall
	}
	if !p.store.verificationResources.tryAcquireProcess() {
		return VerificationProcessResult{Pending: true}, nil
	}
	defer p.store.verificationResources.releaseProcess()

	leased, err := p.store.ClaimVerificationWorkForAttempt(ctx, taskID, attempt, p.owner, p.leaseDuration)
	if errors.Is(err, ErrVerificationWorkBusy) {
		return VerificationProcessResult{Pending: true}, nil
	}
	if errors.Is(err, ErrVerificationWorkTerminal) {
		return VerificationProcessResult{Outcome: VerifyOutcome(leased.Work.TerminalOutcome)}, nil
	}
	if err != nil {
		return VerificationProcessResult{}, err
	}
	result, err := p.processLeased(ctx, leased)
	if err != nil {
		_ = p.store.ReleaseVerificationWork(context.Background(), leased.Lease,
			time.Now().Add(verificationRetryDelay), err.Error())
		if errors.Is(err, ErrVerificationResourceBusy) || errors.Is(err, ErrVerificationChunkBusy) {
			return VerificationProcessResult{Info: result.Info, Pending: true}, nil
		}
	}
	return result, err
}

func (p *VerificationProcessor) Drain(ctx context.Context, limit int) error {
	if limit <= 0 {
		return errors.New("verification drain requires a positive limit")
	}
	if limit > verificationWorkMaxClaim {
		limit = verificationWorkMaxClaim
	}
	if p.store == nil || p.store.verificationResources == nil || p.store.verificationResources.maxConns < 2 {
		return ErrVerificationPoolTooSmall
	}
	var first error
	for processed := 0; processed < limit; processed++ {
		if !p.store.verificationResources.tryAcquireProcess() {
			break
		}
		leased, err := p.store.ClaimVerificationWork(ctx, p.owner, p.leaseDuration, 1)
		if err != nil {
			p.store.verificationResources.releaseProcess()
			if first != nil {
				return first
			}
			return err
		}
		if len(leased) == 0 {
			p.store.verificationResources.releaseProcess()
			break
		}
		item := leased[0]
		_, processErr := p.processLeased(ctx, item)
		p.store.verificationResources.releaseProcess()
		if processErr != nil {
			if first == nil && !errors.Is(processErr, ErrVerificationResourceBusy) && !errors.Is(processErr, ErrVerificationChunkBusy) {
				first = processErr
			}
			if rerr := p.store.ReleaseVerificationWork(context.Background(), item.Lease,
				time.Now().Add(verificationRetryDelay), processErr.Error()); rerr != nil && !errors.Is(rerr, ErrVerificationLeaseLost) {
				log.Printf("verification: release work %s after error: %v", item.Work.ID, rerr)
			}
		}
	}
	return first
}

func (p *VerificationProcessor) processLeased(ctx context.Context, leased LeasedVerificationWork) (VerificationProcessResult, error) {
	if p.leaseDuration <= 0 || p.leaseRenewal <= 0 || p.leaseRenewal >= p.leaseDuration {
		return VerificationProcessResult{}, errors.New("verification processor requires a renewal interval shorter than its lease")
	}
	processCtx, cancelProcess := context.WithCancel(ctx)
	defer cancelProcess()
	stopHeartbeat := make(chan struct{})
	heartbeatDone := make(chan error, 1)
	go func() {
		ticker := time.NewTicker(p.leaseRenewal)
		defer ticker.Stop()
		for {
			select {
			case <-stopHeartbeat:
				heartbeatDone <- nil
				return
			case <-processCtx.Done():
				heartbeatDone <- nil
				return
			case <-ticker.C:
				if _, err := p.store.RenewVerificationLease(processCtx, leased.Lease, p.leaseDuration); err != nil {
					cancelProcess()
					heartbeatDone <- fmt.Errorf("renew verification work %s: %w", leased.Work.ID, err)
					return
				}
			}
		}
	}()

	result, err := p.processLeasedOnce(processCtx, leased)
	close(stopHeartbeat)
	heartbeatErr := <-heartbeatDone
	if err == nil {
		return result, nil
	}
	if heartbeatErr != nil && ctx.Err() == nil {
		return result, heartbeatErr
	}
	return result, err
}

func (p *VerificationProcessor) processLeasedOnce(ctx context.Context, leased LeasedVerificationWork) (VerificationProcessResult, error) {
	result := VerificationProcessResult{}
	reachRecoveryBoundary(ctx, p.probe, BoundaryVerifyWorkClaimed)
	info, commit, err := commitInfoFromVerificationWork(leased.Work)
	if err != nil {
		return result, err
	}
	result.Info = info
	memoryCtx, releaseMemory := withVerificationMemoryTracker(ctx, p.store.verificationResources.bytes)
	defer releaseMemory()
	ctx = memoryCtx

	unlock, err := p.lockChunk(ctx, info.JobID, info.ChunkIndex)
	if err != nil {
		return result, err
	}
	defer unlock()
	ownsPlanTurn, err := p.store.OwnsVerificationChunkPlanTurn(ctx, leased.Work.ID, info.JobID, info.ChunkIndex)
	if err != nil {
		return result, err
	}
	if !ownsPlanTurn {
		return result, ErrVerificationChunkBusy
	}

	work := leased.Work
	var commitBytes []byte
	if work.SamplingProbability == nil || work.SamplingSelected == nil {
		probability := p.verifier.effectiveCheckProb(ctx, info)
		selected := p.verifier.checkSampled(info.TaskID, probability)
		if _, err := p.store.PinVerificationSampling(ctx, leased.Lease, probability, selected); err != nil {
			return result, err
		}
		reachRecoveryBoundary(ctx, p.probe, BoundaryVerifyAfterSamplingPin)
		work, err = p.store.VerificationWorkForAttempt(ctx, info.TaskID, int64(info.Attempt))
		if err != nil {
			return result, err
		}
	}
	if work.SamplingPolicy != verificationSamplingPolicy || work.SamplingProbability == nil || work.SamplingSelected == nil {
		return result, ErrVerificationWorkConflict
	}
	selected := *work.SamplingSelected
	info.verificationCheckSampled = &selected
	if work.Artifact == nil {
		exists, err := p.storage.ObjectExists(ctx, work.Snapshot.StagedResultKey)
		if err != nil {
			return result, err
		}
		if !exists {
			if work.LeaseAttempts < verificationArtifactUnavailableMaxLeaseAttempts {
				return result, ErrVerificationStagingArtifactMissing
			}
		}
		var sealed SealedVerificationArtifact
		if !exists {
			sealed, err = p.storage.sealUnavailableVerificationEvidenceWithProbe(ctx, info.TaskID, info.Attempt,
				work.Snapshot.StagedResultKey, "missing", work.LeaseAttempts, p.probe)
		} else {
			sealed, err = p.storage.sealVerificationArtifactWithLimit(ctx, info.TaskID, info.Attempt,
				work.Snapshot.StagedResultKey, info.resultMaxBytes, p.probe)
		}
		if err != nil {
			var sizeErr *VerificationArtifactTooLargeError
			switch {
			case errors.As(err, &sizeErr):
				sealed, err = p.storage.sealOversizedVerificationEvidenceWithProbe(ctx, info.TaskID, info.Attempt, sizeErr, p.probe)
			case errors.Is(err, ErrVerificationArtifactChanged) &&
				work.LeaseAttempts >= verificationArtifactUnavailableMaxLeaseAttempts:
				sealed, err = p.storage.sealUnavailableVerificationEvidenceWithProbe(ctx, info.TaskID, info.Attempt,
					work.Snapshot.StagedResultKey, "changed", work.LeaseAttempts, p.probe)
			default:
				return result, err
			}
			if err != nil {
				return result, err
			}
		}
		commitBytes = sealed.Body
		_, err = p.store.PinVerificationArtifact(ctx, leased.Lease, VerificationArtifact{
			Key: sealed.Key, SHA256: sealed.SHA256, Bytes: sealed.Bytes,
		})
		if err != nil {
			return result, err
		}
		reachRecoveryBoundary(ctx, p.probe, BoundaryVerifyAfterArtifactPin)
		work, err = p.store.VerificationWorkForAttempt(ctx, info.TaskID, int64(info.Attempt))
		if err != nil {
			return result, err
		}
		work.Artifact = &VerificationArtifact{Key: sealed.Key, SHA256: sealed.SHA256, Bytes: sealed.Bytes}
	}
	if commitBytes == nil {
		commitBytes, err = p.storage.readSealedVerificationArtifactWithLimit(ctx, *work.Artifact, info.resultMaxBytes)
		if err != nil {
			return result, err
		}
	}
	info.ResultKey, info.ResultSHA256 = work.Artifact.Key, work.Artifact.SHA256
	commit.ResultKey, commit.ResultSHA256 = work.Artifact.Key, work.Artifact.SHA256

	plan, err := p.store.VerificationWorkPlan(ctx, work.ID)
	if errors.Is(err, pgx.ErrNoRows) {
		switch {
		case isOversizedVerificationEvidenceKey(work.Artifact.Key):
			plan, err = p.createOversizedArtifactPlan(ctx, leased.Lease, work, info)
		case isUnavailableVerificationEvidenceKey(work.Artifact.Key):
			plan, err = p.createUnavailableArtifactPlan(ctx, leased.Lease, work, info)
		default:
			plan, err = p.createPlan(ctx, leased.Lease, work, info, commit, commitBytes)
		}
	}
	if err != nil {
		return result, err
	}
	if plan.Artifact != *work.Artifact || plan.SnapshotSHA256 != work.SnapshotSHA256 {
		return result, ErrVerificationWorkConflict
	}
	if plan.SamplingProbability != *work.SamplingProbability || plan.SamplingSelected != *work.SamplingSelected {
		return result, ErrVerificationWorkConflict
	}
	selected = plan.SamplingSelected
	info.verificationCheckSampled = &selected
	apply, err := p.store.VerifyJobTx(ctx, leased.Lease, plan, info, p.probe)
	if err != nil {
		return result, err
	}
	result.Outcome, result.Applied = plan.Decision.Outcome, apply
	if apply.Applied {
		for _, effect := range plan.Decision.Effects {
			switch effect.Kind {
			case VerificationEffectInsertTiebreak:
				metrics.tiebreaks.Add(1)
			case VerificationEffectQuarantine:
				metrics.quarantines.Add(1)
			}
		}
		if plan.Decision.Outcome == OutcomePassWithPenalty || plan.Decision.Outcome == OutcomeLossNoPayout {
			metrics.verificationMismatch.Add(1)
		}
		if plan.Decision.Outcome != OutcomeFail {
			metrics.tasksCompleted.Add(1)
		}
	}
	return result, nil
}

func (p *VerificationProcessor) createUnavailableArtifactPlan(ctx context.Context, lease VerificationLease, work VerificationWork, info *CommitTaskInfo) (VerificationWorkPlan, error) {
	if work.Artifact == nil || !isUnavailableVerificationEvidenceKey(work.Artifact.Key) ||
		work.SamplingProbability == nil || work.SamplingSelected == nil ||
		work.SamplingPolicy != verificationSamplingPolicy {
		return VerificationWorkPlan{}, ErrVerificationWorkConflict
	}
	effects := []VerificationEffect{
		{Kind: VerificationEffectDockReputation, SupplierID: info.SupplierID, ReputationEvent: EventTimeout},
		{Kind: VerificationEffectRecordEvent, JobID: info.JobID, TaskID: info.TaskID, SupplierID: info.SupplierID, EventKind: "artifact_unavailable"},
		{Kind: VerificationEffectRequeue, TaskID: info.TaskID},
	}
	for i := range effects {
		effects[i].ID = verificationEffectPayloadID(info.TaskID, info.Attempt, i, effects[i])
	}
	decision := VerificationDecision{
		Outcome: OutcomeFail, Effects: effects,
		Failure: &VerificationFailure{Kind: "artifact_unavailable", Code: "retry_exhausted", JobType: info.jobType},
	}
	plan, _, err := p.store.PersistVerificationWorkPlan(ctx, lease, work,
		*work.SamplingProbability, *work.SamplingSelected, decision, nil)
	if err != nil {
		return VerificationWorkPlan{}, err
	}
	reachRecoveryBoundary(ctx, p.probe, BoundaryVerifyAfterDecision)
	return plan, nil
}

func (p *VerificationProcessor) createOversizedArtifactPlan(ctx context.Context, lease VerificationLease, work VerificationWork, info *CommitTaskInfo) (VerificationWorkPlan, error) {
	if work.Artifact == nil || !isOversizedVerificationEvidenceKey(work.Artifact.Key) ||
		work.SamplingProbability == nil || work.SamplingSelected == nil ||
		work.SamplingPolicy != verificationSamplingPolicy {
		return VerificationWorkPlan{}, ErrVerificationWorkConflict
	}
	effects := []VerificationEffect{
		{Kind: VerificationEffectDockReputation, SupplierID: info.SupplierID, ReputationEvent: EventArtifactOversize},
		{Kind: VerificationEffectRecordEvent, JobID: info.JobID, TaskID: info.TaskID, SupplierID: info.SupplierID, EventKind: "artifact_oversize"},
		{Kind: VerificationEffectQuarantine, SupplierID: info.SupplierID},
		{Kind: VerificationEffectRequeue, TaskID: info.TaskID},
	}
	for i := range effects {
		effects[i].ID = verificationEffectPayloadID(info.TaskID, info.Attempt, i, effects[i])
	}
	decision := VerificationDecision{
		Outcome: OutcomeFail, Effects: effects,
		Failure: &VerificationFailure{Kind: "artifact_oversize", Code: "too_large", JobType: info.jobType},
	}
	plan, _, err := p.store.PersistVerificationWorkPlan(ctx, lease, work,
		*work.SamplingProbability, *work.SamplingSelected, decision, nil)
	if err != nil {
		return VerificationWorkPlan{}, err
	}
	reachRecoveryBoundary(ctx, p.probe, BoundaryVerifyAfterDecision)
	return plan, nil
}

func (p *VerificationProcessor) createPlan(ctx context.Context, lease VerificationLease, work VerificationWork, info *CommitTaskInfo, commit TaskCommit, commitBytes []byte) (VerificationWorkPlan, error) {
	if work.SamplingProbability == nil || work.SamplingSelected == nil || work.SamplingPolicy != verificationSamplingPolicy {
		return VerificationWorkPlan{}, ErrVerificationWorkConflict
	}
	probability := *work.SamplingProbability
	selected := *work.SamplingSelected
	info.verificationCheckSampled = &selected

	if validationErr := validateTaskResultArtifact(info, commitBytes); validationErr != nil {
		decision := invalidResultVerificationDecision(info, validationErr)
		plan, _, err := p.store.PersistVerificationWorkPlan(ctx, lease, work,
			probability, selected, decision, nil)
		if err != nil {
			return VerificationWorkPlan{}, err
		}
		reachRecoveryBoundary(ctx, p.probe, BoundaryVerifyAfterDecision)
		return plan, nil
	}

	var redundancyBytes []byte
	var err error
	peerArtifact, peerSupplier, peerEngine, peerBuild, sealedErr := p.store.PeerSealedResult(ctx, info.TaskID)
	if sealedErr == nil {
		info.peerSupplierID, info.peerEngine, info.peerBuildHash = peerSupplier, peerEngine, peerBuild
		if byteExactJobType(info.jobType) &&
			sameVerificationClass(info.engine, info.buildHash, peerEngine, peerBuild) &&
			peerArtifact.SHA256 == work.Artifact.SHA256 {
			redundancyBytes = commitBytes
			metrics.hashTrustedRedundancy.Add(1)
		} else {
			redundancyBytes, err = p.storage.readSealedVerificationArtifactWithLimit(ctx, peerArtifact, info.resultMaxBytes)
			if err != nil {
				return VerificationWorkPlan{}, fmt.Errorf("read sealed redundancy peer: %w", err)
			}
		}
	} else if !errors.Is(sealedErr, errNotFound) {
		return VerificationWorkPlan{}, sealedErr
	} else {
		peerKey, legacySupplier, legacyEngine, legacyBuild, _, legacyErr := p.store.PeerResultKey(ctx, info.TaskID)
		if legacyErr != nil && !errors.Is(legacyErr, errNotFound) {
			return VerificationWorkPlan{}, legacyErr
		}
		if peerKey != "" {
			info.peerSupplierID, info.peerEngine, info.peerBuildHash = legacySupplier, legacyEngine, legacyBuild
			redundancyBytes, err = p.storage.readVerificationObjectBounded(ctx, peerKey, info.resultMaxBytes, nil)
			if err != nil {
				return VerificationWorkPlan{}, fmt.Errorf("read legacy redundancy peer: %w", err)
			}
		}
	}
	decision, err := p.verifier.PlanTaskResult(ctx, info, commit, commitBytes, redundancyBytes)
	if err != nil {
		return VerificationWorkPlan{}, err
	}
	var settlement []LedgerEntry
	if decision.Outcome != OutcomeFail && decision.Outcome != OutcomeLossNoPayout {
		settlement, err = p.taskPayoutEntriesAt(ctx, info, time.Now().UTC())
		if err != nil {
			return VerificationWorkPlan{}, err
		}
	}
	plan, _, err := p.store.PersistVerificationWorkPlan(ctx, lease, work, probability, selected, decision, settlement)
	if err != nil {
		return VerificationWorkPlan{}, err
	}
	reachRecoveryBoundary(ctx, p.probe, BoundaryVerifyAfterDecision)
	return plan, nil
}

func (p *VerificationProcessor) taskPayoutEntriesAt(ctx context.Context, info *CommitTaskInfo, at time.Time) ([]LedgerEntry, error) {
	j, err := p.store.getJobInternal(ctx, info.JobID)
	if err != nil {
		return nil, err
	}
	buyerCharge, supplierPayout, err := p.store.TaskEconomicAmounts(ctx, info.TaskID)
	if err != nil {
		return nil, err
	}
	if buyerCharge <= 0 || supplierPayout < 0 || supplierPayout > buyerCharge {
		return nil, fmt.Errorf("task %s has invalid frozen economics", info.TaskID)
	}
	var policy VerificationPolicy
	_ = json.Unmarshal(j.VerificationPolicy, &policy)
	return splitFrozenCharge(j.BuyerID, info.SupplierID, info.TaskID,
		buyerCharge, supplierPayout, policy.PayoutHoldSecs, at), nil
}

func (p *VerificationProcessor) lockChunk(ctx context.Context, jobID uuid.UUID, chunkIndex int) (func(), error) {
	conn, err := p.store.pool.Acquire(ctx)
	if err != nil {
		return nil, err
	}
	key := verificationChunkLockKey(jobID, chunkIndex)
	var locked bool
	if err := conn.QueryRow(ctx, `SELECT pg_try_advisory_lock($1)`, key).Scan(&locked); err != nil {
		conn.Release()
		return nil, err
	}
	if !locked {
		conn.Release()
		return nil, ErrVerificationChunkBusy
	}
	return func() {
		unlockCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		var unlocked bool
		err := conn.QueryRow(unlockCtx, `SELECT pg_advisory_unlock($1)`, key).Scan(&unlocked)
		if err == nil && unlocked {
			conn.Release()
			return
		}
		raw := conn.Hijack()
		if closeErr := raw.Close(unlockCtx); closeErr != nil {
			log.Printf("verification: discard advisory-lock connection for %s/%d (unlock=%v, close=%v)",
				jobID, chunkIndex, err, closeErr)
		}
	}, nil
}

func verificationChunkLockKey(jobID uuid.UUID, chunkIndex int) int64 {
	var input [20]byte
	copy(input[:16], jobID[:])
	binary.BigEndian.PutUint32(input[16:], uint32(chunkIndex))
	sum := sha256.Sum256(input[:])
	return int64(binary.BigEndian.Uint64(sum[:8]))
}
