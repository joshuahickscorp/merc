package main

import (
	"context"
	"encoding/binary"
	"encoding/json"
	"fmt"

	"github.com/google/uuid"
)

type verificationStore interface {
	GetHoneypotAnswer(context.Context, string, string) ([]byte, string, error)
	CandidateWorkers(context.Context, string, string, float32) ([]MatchWorker, error)
	ChunkResults(context.Context, uuid.UUID, int) ([]ChunkResult, error)
	TiebreakExists(context.Context, uuid.UUID, int) (bool, error)
	SelectRedundancyPeerExcluding(context.Context, string, string, float32, uuid.UUID, []uuid.UUID, []uuid.UUID) (uuid.UUID, error)

	DockReputation(context.Context, uuid.UUID, ReputationEvent) error
	RecordVerificationEvent(context.Context, uuid.UUID, uuid.UUID, uuid.UUID, string) error
	ClawbackTaskCredit(context.Context, uuid.UUID, uuid.UUID) error
	QuarantineSupplier(context.Context, uuid.UUID) error
	RequeueTask(context.Context, uuid.UUID) error
	InsertTiebreakTask(context.Context, uuid.UUID, uuid.UUID, uuid.UUID, string, int) (uuid.UUID, error)
}

var _ verificationStore = (*Store)(nil)

type VerificationEffectKind string

const (
	VerificationEffectDockReputation VerificationEffectKind = "dock_reputation"
	VerificationEffectRecordEvent    VerificationEffectKind = "record_verification_event"
	VerificationEffectClawbackCredit VerificationEffectKind = "clawback_task_credit"
	VerificationEffectQuarantine     VerificationEffectKind = "quarantine_supplier"
	VerificationEffectRequeue        VerificationEffectKind = "requeue_task"
	VerificationEffectInsertTiebreak VerificationEffectKind = "insert_tiebreak_task"
)

type VerificationEffect struct {
	ID              uuid.UUID              `json:"id"`
	Kind            VerificationEffectKind `json:"kind"`
	JobID           uuid.UUID              `json:"job_id,omitempty"`
	TaskID          uuid.UUID              `json:"task_id,omitempty"`
	SupplierID      uuid.UUID              `json:"supplier_id,omitempty"`
	ReputationEvent ReputationEvent        `json:"reputation_event,omitempty"`
	EventKind       string                 `json:"event_kind,omitempty"`
	PeerWorkerID    uuid.UUID              `json:"peer_worker_id,omitempty"`
	PrimaryTaskID   uuid.UUID              `json:"primary_task_id,omitempty"`
	InputRef        string                 `json:"input_ref,omitempty"`
	ChunkIndex      int                    `json:"chunk_index,omitempty"`
}

type VerificationDecision struct {
	Outcome VerifyOutcome        `json:"outcome"`
	Effects []VerificationEffect `json:"effects"`
	Failure *VerificationFailure `json:"failure,omitempty"`
}

type VerificationFailure struct {
	Kind    string `json:"kind"`
	Code    string `json:"code"`
	JobType string `json:"job_type"`
}

func (v *Verifier) PlanTaskResult(ctx context.Context, info *CommitTaskInfo, commit TaskCommit, commitBytes, redundancyBytes []byte) (VerificationDecision, error) {
	if info == nil {
		return VerificationDecision{}, fmt.Errorf("plan verification: nil commit task info")
	}
	recorder := &recordingVerificationStore{
		reads:   v.store,
		taskID:  info.TaskID,
		attempt: info.Attempt,
	}
	planner := *v
	planner.store = recorder
	planner.planning = true

	outcome, err := planner.verifyTaskResult(ctx, info, commit, commitBytes, redundancyBytes)
	if err != nil {
		return VerificationDecision{Outcome: outcome}, err
	}
	effects := append([]VerificationEffect(nil), recorder.effects...)
	return VerificationDecision{Outcome: outcome, Effects: effects}, nil
}

type recordingVerificationStore struct {
	reads   verificationStore
	taskID  uuid.UUID
	attempt int16
	effects []VerificationEffect
}

func (r *recordingVerificationStore) append(effect VerificationEffect) uuid.UUID {
	effect.ID = verificationEffectPayloadID(r.taskID, r.attempt, len(r.effects), effect)
	r.effects = append(r.effects, effect)
	return effect.ID
}

func verificationEffectID(taskID uuid.UUID, attempt int16, ordinal int, kind VerificationEffectKind) uuid.UUID {
	return verificationEffectPayloadID(taskID, attempt, ordinal, VerificationEffect{Kind: kind})
}

func verificationEffectPayloadID(taskID uuid.UUID, attempt int16, ordinal int, effect VerificationEffect) uuid.UUID {
	data := make([]byte, 0, len(taskID)+2+8+len(effect.Kind)+128)
	data = append(data, taskID[:]...)
	var n [8]byte
	binary.BigEndian.PutUint16(n[:2], uint16(attempt))
	data = append(data, n[:2]...)
	binary.BigEndian.PutUint64(n[:], uint64(ordinal))
	data = append(data, n[:]...)
	effect.ID = uuid.Nil
	if effect.Kind == VerificationEffectInsertTiebreak {
		effect.TaskID = uuid.Nil
	}
	payload, err := json.Marshal(effect)
	if err != nil {
		panic(fmt.Sprintf("marshal verification effect identity: %v", err))
	}
	data = append(data, payload...)
	return uuid.NewSHA1(verificationEffectNamespace, data)
}

var verificationEffectNamespace = uuid.MustParse("d63f7f13-0178-5dc4-90fd-81caf1074a6f")

func (r *recordingVerificationStore) GetHoneypotAnswer(ctx context.Context, jobType, inputRef string) ([]byte, string, error) {
	if r.reads == nil {
		return nil, "", fmt.Errorf("verification plan requires store read: GetHoneypotAnswer")
	}
	return r.reads.GetHoneypotAnswer(ctx, jobType, inputRef)
}

func (r *recordingVerificationStore) CandidateWorkers(ctx context.Context, jobType, modelRef string, minMemoryGB float32) ([]MatchWorker, error) {
	if r.reads == nil {
		return nil, fmt.Errorf("verification plan requires store read: CandidateWorkers")
	}
	return r.reads.CandidateWorkers(ctx, jobType, modelRef, minMemoryGB)
}

func (r *recordingVerificationStore) ChunkResults(ctx context.Context, jobID uuid.UUID, chunkIndex int) ([]ChunkResult, error) {
	if r.reads == nil {
		return nil, fmt.Errorf("verification plan requires store read: ChunkResults")
	}
	return r.reads.ChunkResults(ctx, jobID, chunkIndex)
}

func (r *recordingVerificationStore) TiebreakExists(ctx context.Context, jobID uuid.UUID, chunkIndex int) (bool, error) {
	if r.reads == nil {
		return false, fmt.Errorf("verification plan requires store read: TiebreakExists")
	}
	return r.reads.TiebreakExists(ctx, jobID, chunkIndex)
}

func (r *recordingVerificationStore) SelectRedundancyPeerExcluding(ctx context.Context, jobType, modelRef string, minMemoryGB float32, anchor uuid.UUID, also, alsoSuppliers []uuid.UUID) (uuid.UUID, error) {
	if r.reads == nil {
		return uuid.Nil, fmt.Errorf("verification plan requires store read: SelectRedundancyPeerExcluding")
	}
	return r.reads.SelectRedundancyPeerExcluding(ctx, jobType, modelRef, minMemoryGB, anchor, also, alsoSuppliers)
}

func (r *recordingVerificationStore) DockReputation(_ context.Context, supplierID uuid.UUID, event ReputationEvent) error {
	r.append(VerificationEffect{Kind: VerificationEffectDockReputation, SupplierID: supplierID, ReputationEvent: event})
	return nil
}

func (r *recordingVerificationStore) RecordVerificationEvent(_ context.Context, jobID, taskID, supplierID uuid.UUID, kind string) error {
	r.append(VerificationEffect{Kind: VerificationEffectRecordEvent, JobID: jobID, TaskID: taskID, SupplierID: supplierID, EventKind: kind})
	return nil
}

func (r *recordingVerificationStore) ClawbackTaskCredit(_ context.Context, supplierID, taskID uuid.UUID) error {
	r.append(VerificationEffect{Kind: VerificationEffectClawbackCredit, SupplierID: supplierID, TaskID: taskID})
	return nil
}

func (r *recordingVerificationStore) QuarantineSupplier(_ context.Context, supplierID uuid.UUID) error {
	r.append(VerificationEffect{Kind: VerificationEffectQuarantine, SupplierID: supplierID})
	return nil
}

func (r *recordingVerificationStore) RequeueTask(_ context.Context, taskID uuid.UUID) error {
	r.append(VerificationEffect{Kind: VerificationEffectRequeue, TaskID: taskID})
	return nil
}

func (r *recordingVerificationStore) InsertTiebreakTask(_ context.Context, jobID, primaryTaskID, peerWorker uuid.UUID, inputRef string, chunkIndex int) (uuid.UUID, error) {
	id := r.append(VerificationEffect{
		Kind:          VerificationEffectInsertTiebreak,
		JobID:         jobID,
		PrimaryTaskID: primaryTaskID,
		PeerWorkerID:  peerWorker,
		InputRef:      inputRef,
		ChunkIndex:    chunkIndex,
	})
	r.effects[len(r.effects)-1].TaskID = id
	return id, nil
}
