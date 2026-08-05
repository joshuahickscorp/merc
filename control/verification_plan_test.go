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
	mustf(t, err, "PlanTaskResult: %v")
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
	mustf(t, err, "first plan: %v")
	second, err := v.PlanTaskResult(context.Background(), info, TaskCommit{TaskID: taskID}, []byte("a"), []byte("b"))
	mustf(t, err, "second plan: %v")
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
	mustf(t, err, "next-attempt plan: %v")
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
	mustf(t, err, "verifyTaskResult: %v")
	if outcome != OutcomePass || store.mutationCalls != 1 {
		t.Fatalf("outcome = %q, mutation calls = %d", outcome, store.mutationCalls)
	}
}

// Honeypot known-answer checks must not be gated by verification sampling.
// Sampling only applies to expensive redundancy/tiebreak paths.
func TestHoneypotAlwaysCheckedWhenSampleDecisionFalse(t *testing.T) {
	taskID := uuid.New()
	supplierID := uuid.New()
	store := &verificationStoreDouble{
		honeypotAnswer:      []byte("known answer"),
		honeypotAnswerClass: "engine-a|build-a",
	}
	v := (&Verifier{store: store}).WithSamplingSecret([]byte("honeypot-always-secret"))
	sampled := false
	info := &CommitTaskInfo{
		TaskID:                   taskID,
		JobID:                    uuid.New(),
		SupplierID:               supplierID,
		IsHoneypot:               true,
		InputRef:                 "inputs/probe",
		Attempt:                  1,
		jobType:                  "batch_infer",
		engine:                   "engine-a",
		buildHash:                "build-a",
		verificationCheckSampled: &sampled,
	}

	decision, err := v.PlanTaskResult(context.Background(), info, TaskCommit{TaskID: taskID}, []byte("wrong answer"), nil)
	mustf(t, err, "PlanTaskResult: %v")
	if decision.Outcome != OutcomeFail {
		t.Fatalf("outcome = %q, want %q (honeypot must run even when sample is false)", decision.Outcome, OutcomeFail)
	}

	wantKinds := []VerificationEffectKind{
		VerificationEffectDockReputation,
		VerificationEffectRecordEvent,
		VerificationEffectClawbackCredit,
		VerificationEffectQuarantine,
		VerificationEffectRequeue,
	}
	if len(decision.Effects) != len(wantKinds) {
		t.Fatalf("effects = %#v, want %d effects covering dock/clawback/quarantine", decision.Effects, len(wantKinds))
	}
	for i, kind := range wantKinds {
		if decision.Effects[i].Kind != kind {
			t.Fatalf("effect %d = %q, want %q", i, decision.Effects[i].Kind, kind)
		}
	}
	if decision.Effects[0].ReputationEvent != EventHoneypotFail {
		t.Fatalf("reputation event = %q, want %q", decision.Effects[0].ReputationEvent, EventHoneypotFail)
	}
	if decision.Effects[1].EventKind != "honeypot_fail" {
		t.Fatalf("event kind = %q, want honeypot_fail", decision.Effects[1].EventKind)
	}
	if store.mutationCalls != 0 {
		t.Fatalf("planner delegated %d mutations to the store", store.mutationCalls)
	}
}

// Worker-declared engine/build_hash must not disarm a byte-exact honeypot.
// Class mismatch fails closed rather than skipping the known-answer compare.
func TestHoneypotClassMismatchFailsClosed(t *testing.T) {
	taskID := uuid.New()
	supplierID := uuid.New()
	store := &verificationStoreDouble{
		honeypotAnswer:      []byte("known answer"),
		honeypotAnswerClass: "candle|abc",
	}
	v := (&Verifier{store: store}).WithSamplingSecret([]byte("class-mismatch-secret"))
	info := &CommitTaskInfo{
		TaskID:     taskID,
		JobID:      uuid.New(),
		SupplierID: supplierID,
		IsHoneypot: true,
		InputRef:   "inputs/probe",
		Attempt:    2,
		jobType:    "batch_infer",
		engine:     "candle",
		buildHash:  "nope",
	}

	decision, err := v.PlanTaskResult(context.Background(), info, TaskCommit{TaskID: taskID}, []byte("known answer"), nil)
	mustf(t, err, "PlanTaskResult: %v")
	if decision.Outcome != OutcomeFail {
		t.Fatalf("outcome = %q, want %q (class mismatch must fail closed, not pass)", decision.Outcome, OutcomeFail)
	}
	// OutcomeFail with honeypot sanctions — never the success path that later
	// writes supplier_credit.
	if len(decision.Effects) == 0 {
		t.Fatal("expected fail effects; got none")
	}
	for _, effect := range decision.Effects {
		if effect.ReputationEvent == EventTaskSuccess || effect.ReputationEvent == EventHoneypotPass {
			t.Fatalf("success-path reputation event on class mismatch: %#v", effect)
		}
		if effect.EventKind == "honeypot_pass" || effect.EventKind == "supplier_credit" {
			t.Fatalf("must not record pass/credit on class mismatch: %#v", effect)
		}
	}
	if decision.Effects[0].ReputationEvent != EventHoneypotFail {
		t.Fatalf("dock event = %q, want %q", decision.Effects[0].ReputationEvent, EventHoneypotFail)
	}
	if decision.Effects[1].EventKind != "honeypot_class_mismatch" {
		t.Fatalf("event kind = %q, want honeypot_class_mismatch", decision.Effects[1].EventKind)
	}
	// Wrong answer with matching class still fails as honeypot_fail; matching
	// class + matching bytes would pass. Class mismatch fails even with correct bytes.
	if store.mutationCalls != 0 {
		t.Fatalf("planner delegated %d mutations to the store", store.mutationCalls)
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

// A task dispatched as a honeypot whose known answer is not stored must not
// pay.  Before the floor was made to fail closed at submit time, an unseeded
// deployment -- which is every production deployment, since InsertHoneypot has
// no production caller and `seed` is refused outside development -- silently
// fell through to OutcomePass, so the one check that distinguishes correct work
// from shape-valid garbage never ran.  The supplier is not at fault here, so
// this path must not dock reputation or quarantine.
func TestHoneypotWithoutStoredAnswerFailsClosedWithoutSanctioningSupplier(t *testing.T) {
	taskID := uuid.New()
	store := &verificationStoreDouble{} // no honeypotAnswer seeded
	v := (&Verifier{store: store}).WithSamplingSecret([]byte("honeypot-missing-secret"))
	info := &CommitTaskInfo{
		TaskID:     taskID,
		JobID:      uuid.New(),
		SupplierID: uuid.New(),
		IsHoneypot: true,
		InputRef:   "inputs/probe-with-no-answer",
		Attempt:    1,
		jobType:    "batch_infer",
		engine:     "engine-a",
		buildHash:  "build-a",
	}

	decision, err := v.PlanTaskResult(context.Background(), info, TaskCommit{TaskID: taskID}, []byte("anything"), nil)
	mustf(t, err, "PlanTaskResult: %v")
	if decision.Outcome != OutcomeFail {
		t.Fatalf("outcome = %q, want %q (an unbacked probe must not pass)", decision.Outcome, OutcomeFail)
	}
	if len(decision.Effects) != 1 || decision.Effects[0].Kind != VerificationEffectRecordEvent {
		t.Fatalf("effects = %#v, want exactly one record-event effect", decision.Effects)
	}
	if got := decision.Effects[0].EventKind; got != "honeypot_answer_missing" {
		t.Fatalf("event = %q, want honeypot_answer_missing", got)
	}
}
