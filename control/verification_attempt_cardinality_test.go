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
		HWClass:               "apple_silicon_ultra", hardwareIdentity: "Apple M3 Ultra",
		engine: "candle", buildHash: "0123456789abcdef",
	}
	info.ResultKey = taskAttemptResultKey(info.JobID, info.TaskID, info.Attempt)
	snapshot, err := verificationWorkSnapshotFromCommit(info, TaskCommit{TaskID: info.TaskID, ResultKey: info.ResultKey})
	must(t, err)
	if snapshot.SnapshotVersion != verificationAttemptSnapshotVersion {
		t.Fatalf("snapshot version=%d, want %d", snapshot.SnapshotVersion, verificationAttemptSnapshotVersion)
	}
	var frozen verificationAttemptInput
	must(t, json.Unmarshal(snapshot.Snapshot, &frozen))
	wantCap := verificationArtifactMaxBytesForRecords("embed", 1, 4096, 0)
	if frozen.ExpectedOutputRecords != 1 || frozen.ResultMaxBytes != wantCap ||
		frozen.HWClass != info.HWClass || frozen.HardwareIdentity != info.hardwareIdentity ||
		frozen.Engine != info.engine || frozen.BuildHash != info.buildHash ||
		frozen.ResultMaxBytes >= verificationArtifactMaxBytes("embed", 4096, 0) {
		t.Fatalf("frozen attempt identity/cardinality/cap = %+v, want records=1 cap=%d", frozen, wantCap)
	}
	recovered, _, err := commitInfoFromVerificationWork(VerificationWork{Snapshot: snapshot})
	must(t, err)
	if recovered.ExpectedOutputRecords != 1 || recovered.resultMaxBytes != wantCap ||
		recovered.HWClass != info.HWClass || recovered.hardwareIdentity != info.hardwareIdentity ||
		recovered.engine != info.engine || recovered.buildHash != info.buildHash {
		t.Fatalf("recovered attempt lost exact cardinality: %+v", recovered)
	}
}
