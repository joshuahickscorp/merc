package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
)

type recordingTaskResultPresigner struct {
	keys []string
}

func (p *recordingTaskResultPresigner) PresignPut(_ context.Context, key string, _ time.Duration) (string, error) {
	p.keys = append(p.keys, key)
	return "presigned-put://" + key, nil
}

func resultDigest(body []byte) string {
	sum := sha256.Sum256(body)
	return hex.EncodeToString(sum[:])
}

func TestAttemptSpecificPresignedResultRejectsStaleLateWrite(t *testing.T) {
	jobID := uuid.MustParse("11111111-1111-4111-8111-111111111111")
	taskID := uuid.MustParse("22222222-2222-4222-8222-222222222222")
	workerID := uuid.MustParse("33333333-3333-4333-8333-333333333333")
	supplierID := uuid.MustParse("44444444-4444-4444-8444-444444444444")
	presigner := new(recordingTaskResultPresigner)

	attempt0 := &ClaimedTask{JobID: jobID, TaskID: taskID, Attempt: 0}
	attempt0.ResultKey = taskAttemptResultKey(jobID, taskID, attempt0.Attempt)
	staleURL, err := presignTaskAttemptResult(context.Background(), presigner, attempt0)
	if err != nil {
		t.Fatal(err)
	}
	attempt1 := &ClaimedTask{JobID: jobID, TaskID: taskID, Attempt: 1}
	attempt1.ResultKey = taskAttemptResultKey(jobID, taskID, attempt1.Attempt)
	currentURL, err := presignTaskAttemptResult(context.Background(), presigner, attempt1)
	if err != nil {
		t.Fatal(err)
	}
	if staleURL == currentURL || attempt0.ResultKey == attempt1.ResultKey {
		t.Fatalf("retry reused staging capability: attempt0=%q attempt1=%q", staleURL, currentURL)
	}

	// Model two still-valid presigned capabilities. A late attempt-0 upload can
	// overwrite only the abandoned attempt-0 object, never the attempt-1 object
	// selected by the durable attempt snapshot for sealing.
	objects := map[string][]byte{}
	objects[staleURL] = []byte(`{"attempt":0,"state":"first"}`)
	currentBody := []byte(`{"attempt":1,"state":"current"}`)
	objects[currentURL] = currentBody
	objects[staleURL] = []byte(`{"attempt":0,"state":"late-overwrite"}`)
	if got := string(objects[currentURL]); got != string(currentBody) {
		t.Fatalf("stale capability changed current attempt object: got %q", got)
	}

	currentDigest := resultDigest(objects[currentURL])
	snapshot := VerificationWorkSnapshot{
		TaskID: taskID, Attempt: 1, JobID: jobID, WorkerID: workerID, SupplierID: supplierID,
		SnapshotVersion: verificationAttemptSnapshotVersion,
		Snapshot:        []byte(`{"job_type":"embed"}`),
		StagedResultKey: attempt1.ResultKey, ReportedResultSHA256: currentDigest,
	}
	if _, _, _, err := prepareVerificationSnapshot(snapshot); err != nil {
		t.Fatalf("current attempt snapshot was rejected: %v", err)
	}
	staleSnapshot := snapshot
	staleSnapshot.StagedResultKey = attempt0.ResultKey
	if _, _, _, err := prepareVerificationSnapshot(staleSnapshot); err == nil {
		t.Fatal("attempt-1 snapshot accepted attempt-0 staging key")
	}
	if err := validateReportedResultDigest("embed", currentDigest, currentDigest); err != nil {
		t.Fatalf("current attempt digest did not match its sealed artifact: %v", err)
	}

	staleDigest := resultDigest(objects[staleURL])
	validationErr := validateReportedResultDigest("embed", staleDigest, currentDigest)
	if validationErr == nil {
		t.Fatal("stale worker digest matched the current sealed artifact")
	}
	var typed *ResultArtifactValidationError
	if !errors.As(validationErr, &typed) || typed.Code != resultValidationDigest {
		t.Fatalf("digest mismatch was not typed as %q: %v", resultValidationDigest, validationErr)
	}
	info := &CommitTaskInfo{TaskID: taskID, JobID: jobID, SupplierID: supplierID, Attempt: 1, jobType: "embed"}
	decision := invalidResultVerificationDecision(info, validationErr)
	if err := validateVerificationDecisionShape(info, decision, nil); err != nil {
		t.Fatalf("digest mismatch did not produce a valid no-settlement decision: %v", err)
	}
	if decision.Outcome != OutcomeFail || decision.Failure == nil || decision.Failure.Code != resultValidationDigest {
		t.Fatalf("digest mismatch decision = %+v", decision)
	}
	requeuesCurrentAttempt := false
	for _, effect := range decision.Effects {
		if effect.Kind == VerificationEffectRequeue && effect.TaskID == taskID {
			requeuesCurrentAttempt = true
		}
	}
	if !requeuesCurrentAttempt {
		t.Fatalf("digest mismatch decision does not requeue current task: %+v", decision.Effects)
	}
}

func TestPresignTaskAttemptResultRequiresExactCurrentKey(t *testing.T) {
	jobID := uuid.New()
	taskID := uuid.New()
	presigner := new(recordingTaskResultPresigner)
	task := &ClaimedTask{
		JobID: jobID, TaskID: taskID, Attempt: 2,
		ResultKey: taskAttemptResultKey(jobID, taskID, 1),
	}
	if _, err := presignTaskAttemptResult(context.Background(), presigner, task); err == nil {
		t.Fatal("presigned a stale-attempt result key")
	}
	if len(presigner.keys) != 0 {
		t.Fatalf("presigner received invalid key(s): %v", presigner.keys)
	}
}

func TestClaimTaskSQLPersistsAttemptSpecificResultKey(t *testing.T) {
	query := ClaimTaskSQL("t.claimed_by IS NULL")
	for _, fragment := range []string{
		"result_key = 'jobs/' || tasks.job_id::text || '/tasks/' || tasks.id::text",
		"'/attempt-' || COALESCE(tasks.retry_count,0)::text || '/result.json'",
	} {
		if !strings.Contains(query, fragment) {
			t.Fatalf("claim SQL does not persist attempt-specific staging key; missing %q", fragment)
		}
	}
}
