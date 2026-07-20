package main

import (
	"encoding/json"
	"testing"

	"github.com/google/uuid"
)

func TestVerificationAttemptFreezesExactCardinalityAndNarrowCap(t *testing.T) {
	info := &CommitTaskInfo{
		TaskID: uuid.New(), JobID: uuid.New(), WorkerID: uuid.New(), SupplierID: uuid.New(),
		jobType: "embed", ModelRef: "all-minilm-l6-v2", SplitSize: 4096,
		ExpectedOutputRecords: 1,
	}
	info.ResultKey = taskAttemptResultKey(info.JobID, info.TaskID, info.Attempt)
	snapshot, err := verificationWorkSnapshotFromCommit(info, TaskCommit{TaskID: info.TaskID, ResultKey: info.ResultKey})
	if err != nil {
		t.Fatal(err)
	}
	if snapshot.SnapshotVersion != verificationAttemptSnapshotVersion {
		t.Fatalf("snapshot version=%d, want %d", snapshot.SnapshotVersion, verificationAttemptSnapshotVersion)
	}
	var frozen verificationAttemptInput
	if err := json.Unmarshal(snapshot.Snapshot, &frozen); err != nil {
		t.Fatal(err)
	}
	wantCap := verificationArtifactMaxBytesForRecords("embed", 1, 4096, 0)
	if frozen.ExpectedOutputRecords != 1 || frozen.ResultMaxBytes != wantCap ||
		frozen.ResultMaxBytes >= verificationArtifactMaxBytes("embed", 4096, 0) {
		t.Fatalf("frozen attempt cardinality/cap = %+v, want records=1 cap=%d", frozen, wantCap)
	}
	recovered, _, err := commitInfoFromVerificationWork(VerificationWork{Snapshot: snapshot})
	if err != nil {
		t.Fatal(err)
	}
	if recovered.ExpectedOutputRecords != 1 || recovered.resultMaxBytes != wantCap {
		t.Fatalf("recovered attempt lost exact cardinality: %+v", recovered)
	}
}
