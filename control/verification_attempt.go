package main

import (
	"encoding/json"
	"fmt"
	"math"
)

const (
	verificationAttemptSnapshotLegacyVersion int16 = 1
	verificationAttemptSnapshotPolicyVersion int16 = 2
	verificationAttemptSnapshotVersion       int16 = 4
)

type verificationAttemptInput struct {
	IsHoneypot            bool    `json:"is_honeypot"`
	IsRedundancy          bool    `json:"is_redundancy"`
	HWClass               string  `json:"hw_class"`
	Engine                string  `json:"engine"`
	BuildHash             string  `json:"build_hash"`
	JobType               string  `json:"job_type"`
	InputRef              string  `json:"input_ref"`
	ModelRef              string  `json:"model_ref"`
	MinMemoryGB           float32 `json:"min_memory_gb"`
	ChunkIndex            int     `json:"chunk_index"`
	SplitSize             int     `json:"split_size"`
	ExpectedOutputRecords int64   `json:"expected_output_records,omitempty"`
	ResultMaxBytes        int64   `json:"result_max_bytes,omitempty"`
}

func verificationWorkSnapshotFromCommit(info *CommitTaskInfo, c TaskCommit) (VerificationWorkSnapshot, error) {
	if info == nil {
		return VerificationWorkSnapshot{}, fmt.Errorf("verification attempt snapshot: nil commit info")
	}
	if info.Attempt < 0 || c.DurationMS > math.MaxInt64 || c.TokensUsed > math.MaxInt64 {
		return VerificationWorkSnapshot{}, fmt.Errorf("verification attempt snapshot: reported counters exceed durable range")
	}
	if err := validateTaskAttemptResultKey(info.JobID, info.TaskID, info.Attempt, info.ResultKey); err != nil {
		return VerificationWorkSnapshot{}, fmt.Errorf("verification attempt snapshot: %w", err)
	}
	if c.ResultKey != info.ResultKey {
		return VerificationWorkSnapshot{}, fmt.Errorf("verification attempt snapshot: commit result key %q does not match server key %q", c.ResultKey, info.ResultKey)
	}
	input := verificationAttemptInput{
		IsHoneypot: info.IsHoneypot, IsRedundancy: info.IsRedundancy,
		HWClass: info.HWClass, Engine: info.engine, BuildHash: info.buildHash,
		JobType: info.jobType, InputRef: info.InputRef, ModelRef: info.ModelRef,
		MinMemoryGB: info.MinMemoryGB, ChunkIndex: info.ChunkIndex, SplitSize: info.SplitSize,
		ExpectedOutputRecords: info.ExpectedOutputRecords,
		ResultMaxBytes:        info.resultMaxBytes,
	}
	if input.ExpectedOutputRecords < 0 {
		return VerificationWorkSnapshot{}, fmt.Errorf("verification attempt snapshot: invalid expected output records %d", input.ExpectedOutputRecords)
	}
	if input.ResultMaxBytes <= 0 {
		input.ResultMaxBytes = verificationArtifactMaxBytesForRecords(
			input.JobType, input.ExpectedOutputRecords, input.SplitSize, info.jobMaxTokens,
		)
	}
	limitedResultBytes, err := canaryArtifactLimit(input.ResultMaxBytes)
	if err != nil {
		return VerificationWorkSnapshot{}, fmt.Errorf("verification attempt snapshot: canary artifact policy: %w", err)
	}
	input.ResultMaxBytes = limitedResultBytes
	if input.ResultMaxBytes <= 0 || input.ResultMaxBytes > verificationArtifactAbsoluteMaxBytes {
		return VerificationWorkSnapshot{}, fmt.Errorf("verification attempt snapshot: invalid result limit %d", input.ResultMaxBytes)
	}
	info.resultMaxBytes = input.ResultMaxBytes
	inputBytes, err := json.Marshal(input)
	if err != nil {
		return VerificationWorkSnapshot{}, err
	}
	return VerificationWorkSnapshot{
		TaskID: info.TaskID, Attempt: int64(info.Attempt), JobID: info.JobID,
		WorkerID: info.WorkerID, SupplierID: info.SupplierID,
		SnapshotVersion: verificationAttemptSnapshotVersion, Snapshot: inputBytes,
		StagedResultKey: info.ResultKey, ReportedResultSHA256: c.ResultSHA256,
		DurationMS: int64(c.DurationMS), TokensUsed: int64(c.TokensUsed),
		HardwareTempC: c.HardwareTempC,
	}, nil
}

func commitInfoFromVerificationWork(work VerificationWork) (*CommitTaskInfo, TaskCommit, error) {
	if work.Snapshot.SnapshotVersion < verificationAttemptSnapshotLegacyVersion ||
		work.Snapshot.SnapshotVersion > verificationAttemptSnapshotVersion ||
		work.Snapshot.Attempt < 0 || work.Snapshot.Attempt > math.MaxInt16 ||
		work.Snapshot.DurationMS < 0 || work.Snapshot.TokensUsed < 0 {
		return nil, TaskCommit{}, fmt.Errorf("unsupported verification attempt snapshot v%d/attempt %d",
			work.Snapshot.SnapshotVersion, work.Snapshot.Attempt)
	}
	if err := validateTaskAttemptResultKey(work.Snapshot.JobID, work.Snapshot.TaskID,
		int16(work.Snapshot.Attempt), work.Snapshot.StagedResultKey); err != nil {
		return nil, TaskCommit{}, fmt.Errorf("verification attempt snapshot: %w", err)
	}
	var input verificationAttemptInput
	if err := json.Unmarshal(work.Snapshot.Snapshot, &input); err != nil {
		return nil, TaskCommit{}, fmt.Errorf("decode verification attempt snapshot: %w", err)
	}
	if work.Snapshot.SnapshotVersion == verificationAttemptSnapshotLegacyVersion && input.ResultMaxBytes <= 0 {
		input.ResultMaxBytes = verificationArtifactMaxBytes(input.JobType, input.SplitSize, verificationMaxGenerationTokens)
	}
	limitedResultBytes, err := canaryArtifactLimit(input.ResultMaxBytes)
	if err != nil {
		return nil, TaskCommit{}, fmt.Errorf("verification attempt snapshot has invalid canary artifact policy: %w", err)
	}
	input.ResultMaxBytes = limitedResultBytes
	if input.ExpectedOutputRecords < 0 {
		return nil, TaskCommit{}, fmt.Errorf("verification attempt snapshot has invalid expected output records %d", input.ExpectedOutputRecords)
	}
	if input.ResultMaxBytes <= 0 || input.ResultMaxBytes > verificationArtifactAbsoluteMaxBytes {
		return nil, TaskCommit{}, fmt.Errorf("verification attempt snapshot has invalid result limit %d", input.ResultMaxBytes)
	}
	info := &CommitTaskInfo{
		TaskID: work.Snapshot.TaskID, JobID: work.Snapshot.JobID,
		WorkerID: work.Snapshot.WorkerID, SupplierID: work.Snapshot.SupplierID,
		IsHoneypot: input.IsHoneypot, IsRedundancy: input.IsRedundancy,
		HWClass: input.HWClass, engine: input.Engine, buildHash: input.BuildHash,
		jobType: input.JobType, InputRef: input.InputRef, ModelRef: input.ModelRef,
		MinMemoryGB: input.MinMemoryGB, ChunkIndex: input.ChunkIndex, SplitSize: input.SplitSize,
		ExpectedOutputRecords: input.ExpectedOutputRecords,
		resultMaxBytes:        input.ResultMaxBytes,
		Attempt:               int16(work.Snapshot.Attempt), DurationMS: uint64(work.Snapshot.DurationMS),
		TokensUsed: uint64(work.Snapshot.TokensUsed), hardwareTempC: work.Snapshot.HardwareTempC,
		ResultKey: work.Snapshot.StagedResultKey,
	}
	commit := TaskCommit{
		TaskID: info.TaskID, Attempt: info.Attempt, ResultKey: work.Snapshot.StagedResultKey,
		DurationMS: info.DurationMS, TokensUsed: info.TokensUsed,
		ResultSHA256:  work.Snapshot.ReportedResultSHA256,
		HardwareTempC: work.Snapshot.HardwareTempC,
	}
	return info, commit, nil
}
