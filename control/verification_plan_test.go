package main

import (
	"context"
	"errors"
	"reflect"
	"testing"

	"github.com/google/uuid"
)

func TestPlanTaskResultRecordsHoneypotFailureWithoutWrites(t *testing.T) {
	taskID := uuid.New()
	supplierID := uuid.New()
	store := &verificationStoreDouble{
		honeypotAnswer:      []byte("known answer"),
		honeypotAnswerClass: "engine-a|build-a",
	}
	v := (&Verifier{store: store}).WithSamplingSecret([]byte("plan-test-secret"))
	info := &CommitTaskInfo{
		TaskID:     taskID,
		SupplierID: supplierID,
		IsHoneypot: true,
		InputRef:   "inputs/probe",
		Attempt:    3,
		jobType:    "batch_infer",
		engine:     "engine-a",
		buildHash:  "build-a",
	}

	decision, err := v.PlanTaskResult(context.Background(), info, TaskCommit{TaskID: taskID}, []byte("wrong answer"), nil)
	if err != nil {
		t.Fatalf("PlanTaskResult: %v", err)
	}
	if decision.Outcome != OutcomeFail {
		t.Fatalf("outcome = %q, want %q", decision.Outcome, OutcomeFail)
	}
	if store.mutationCalls != 0 {
		t.Fatalf("planner delegated %d mutations to the store", store.mutationCalls)
	}

	wantKinds := []VerificationEffectKind{
		VerificationEffectDockReputation,
		VerificationEffectRecordEvent,
		VerificationEffectClawbackCredit,
		VerificationEffectQuarantine,
		VerificationEffectRequeue,
	}
	gotKinds := make([]VerificationEffectKind, len(decision.Effects))
	for i, effect := range decision.Effects {
		gotKinds[i] = effect.Kind
		if effect.ID == uuid.Nil {
			t.Fatalf("effect %d has nil deterministic id", i)
		}
	}
	if !reflect.DeepEqual(gotKinds, wantKinds) {
		t.Fatalf("effect order = %#v, want %#v", gotKinds, wantKinds)
	}
	if got := decision.Effects[0]; got.SupplierID != supplierID || got.ReputationEvent != EventHoneypotFail {
		t.Fatalf("dock effect = %#v", got)
	}
	if got := decision.Effects[1]; got.TaskID != taskID || got.EventKind != "honeypot_fail" {
		t.Fatalf("event effect = %#v", got)
	}
	if got := decision.Effects[4]; got.TaskID != taskID {
		t.Fatalf("requeue task = %s, want %s", got.TaskID, taskID)
	}
}

func TestPlanTaskResultTiebreakEffectIsDeterministicPerAttempt(t *testing.T) {
	taskID := uuid.New()
	otherWorker := uuid.New()
	peerWorker := uuid.New()
	store := &verificationStoreDouble{selectedPeer: peerWorker}
	store.chunkResultsFunc = func() []ChunkResult {
		store.chunkReadCalls++
		if store.chunkReadCalls%2 == 1 {
			return nil
		}
		return []ChunkResult{{WorkerID: otherWorker, SupplierID: uuid.New()}}
	}
	v := (&Verifier{store: store, storage: &Storage{}}).WithSamplingSecret([]byte("plan-test-secret"))
	info := &CommitTaskInfo{
		TaskID:         taskID,
		JobID:          uuid.New(),
		SupplierID:     uuid.New(),
		WorkerID:       uuid.Nil,
		InputRef:       "inputs/chunk-7",
		ChunkIndex:     7,
		Attempt:        4,
		jobType:        "batch_infer",
		engine:         "engine-a",
		buildHash:      "build-a",
		peerSupplierID: uuid.New(),
		peerEngine:     "engine-a",
		peerBuildHash:  "build-a",
	}

	first, err := v.PlanTaskResult(context.Background(), info, TaskCommit{TaskID: taskID}, []byte("a"), []byte("b"))
	if err != nil {
		t.Fatalf("first plan: %v", err)
	}
	second, err := v.PlanTaskResult(context.Background(), info, TaskCommit{TaskID: taskID}, []byte("a"), []byte("b"))
	if err != nil {
		t.Fatalf("second plan: %v", err)
	}
	if !reflect.DeepEqual(first, second) {
		t.Fatalf("same attempt planned differently:\nfirst  %#v\nsecond %#v", first, second)
	}
	if first.Outcome != OutcomePassWithPenalty || len(first.Effects) != 2 {
		t.Fatalf("decision = %#v", first)
	}
	tiebreak := first.Effects[1]
	if tiebreak.Kind != VerificationEffectInsertTiebreak || tiebreak.ID == uuid.Nil || tiebreak.TaskID != tiebreak.ID {
		t.Fatalf("tiebreak effect = %#v", tiebreak)
	}
	if tiebreak.PeerWorkerID != peerWorker || tiebreak.PrimaryTaskID != taskID || tiebreak.ChunkIndex != 7 {
		t.Fatalf("tiebreak arguments = %#v", tiebreak)
	}
	if store.mutationCalls != 0 {
		t.Fatalf("planner delegated %d mutations to the store", store.mutationCalls)
	}

	nextAttempt := *info
	nextAttempt.Attempt++
	third, err := v.PlanTaskResult(context.Background(), &nextAttempt, TaskCommit{TaskID: taskID}, []byte("a"), []byte("b"))
	if err != nil {
		t.Fatalf("next-attempt plan: %v", err)
	}
	if third.Effects[1].ID == tiebreak.ID {
		t.Fatal("tiebreak effect id did not change across attempts")
	}
}

func TestPlanTaskResultReadErrorDiscardsPartialEffects(t *testing.T) {
	readErr := errors.New("read unavailable")
	store := &verificationStoreDouble{tiebreakExistsErr: readErr}
	v := (&Verifier{store: store, storage: &Storage{}}).WithSamplingSecret([]byte("plan-test-secret"))
	taskID := uuid.New()
	info := &CommitTaskInfo{
		TaskID:         taskID,
		JobID:          uuid.New(),
		SupplierID:     uuid.New(),
		InputRef:       "inputs/chunk",
		Attempt:        1,
		jobType:        "batch_infer",
		engine:         "engine-a",
		buildHash:      "build-a",
		peerSupplierID: uuid.New(),
		peerEngine:     "engine-a",
		peerBuildHash:  "build-a",
	}

	decision, err := v.PlanTaskResult(context.Background(), info, TaskCommit{TaskID: taskID}, []byte("a"), []byte("b"))
	if !errors.Is(err, readErr) {
		t.Fatalf("error = %v, want %v", err, readErr)
	}
	if len(decision.Effects) != 0 {
		t.Fatalf("invalid partial plan exposed effects: %#v", decision.Effects)
	}
	if store.mutationCalls != 0 {
		t.Fatalf("planner delegated %d mutations to the store", store.mutationCalls)
	}
}

func TestVerifyTaskResultStillWritesThrough(t *testing.T) {
	store := &verificationStoreDouble{}
	v := &Verifier{store: store}
	info := &CommitTaskInfo{TaskID: uuid.New(), SupplierID: uuid.New()}

	outcome, err := v.verifyTaskResult(context.Background(), info, TaskCommit{TaskID: info.TaskID}, []byte("result"), nil)
	if err != nil {
		t.Fatalf("verifyTaskResult: %v", err)
	}
	if outcome != OutcomePass || store.mutationCalls != 1 {
		t.Fatalf("outcome = %q, mutation calls = %d", outcome, store.mutationCalls)
	}
}

type verificationStoreDouble struct {
	honeypotAnswer      []byte
	honeypotAnswerClass string
	chunkResultsFunc    func() []ChunkResult
	chunkReadCalls      int
	tiebreakExistsErr   error
	selectedPeer        uuid.UUID
	mutationCalls       int
}

func (s *verificationStoreDouble) GetHoneypotAnswer(context.Context, string, string) ([]byte, string, error) {
	return append([]byte(nil), s.honeypotAnswer...), s.honeypotAnswerClass, nil
}

func (s *verificationStoreDouble) CandidateWorkers(context.Context, string, string, float32) ([]MatchWorker, error) {
	return nil, nil
}

func (s *verificationStoreDouble) ChunkResults(context.Context, uuid.UUID, int) ([]ChunkResult, error) {
	if s.chunkResultsFunc == nil {
		return nil, nil
	}
	return s.chunkResultsFunc(), nil
}

func (s *verificationStoreDouble) TiebreakExists(context.Context, uuid.UUID, int) (bool, error) {
	return false, s.tiebreakExistsErr
}

func (s *verificationStoreDouble) SelectRedundancyPeerExcluding(context.Context, string, string, float32, uuid.UUID, []uuid.UUID, []uuid.UUID) (uuid.UUID, error) {
	return s.selectedPeer, nil
}

func (s *verificationStoreDouble) DockReputation(context.Context, uuid.UUID, ReputationEvent) error {
	s.mutationCalls++
	return nil
}

func (s *verificationStoreDouble) RecordVerificationEvent(context.Context, uuid.UUID, uuid.UUID, uuid.UUID, string) error {
	s.mutationCalls++
	return nil
}

func (s *verificationStoreDouble) ClawbackTaskCredit(context.Context, uuid.UUID, uuid.UUID) error {
	s.mutationCalls++
	return nil
}

func (s *verificationStoreDouble) QuarantineSupplier(context.Context, uuid.UUID) error {
	s.mutationCalls++
	return nil
}

func (s *verificationStoreDouble) RequeueTask(context.Context, uuid.UUID) error {
	s.mutationCalls++
	return nil
}

func (s *verificationStoreDouble) InsertTiebreakTask(context.Context, uuid.UUID, uuid.UUID, uuid.UUID, string, int) (uuid.UUID, error) {
	s.mutationCalls++
	return uuid.New(), nil
}
