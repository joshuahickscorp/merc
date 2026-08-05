package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
)

func TestStreamSplitAndUploadMeasuresExactFullInputDepth(t *testing.T) {
	storage := openObjectStorageForTest(t)
	server := &Server{storage: storage}
	ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
	defer cancel()

	// Put the long record beyond the submit prefix-sample size so this proves
	// the streamed authority is measured over the complete input.
	longBody := strings.Repeat("z", inputSampleBytes+257)
	input := []byte(fmt.Sprintf(
		"{\"id\":\"1\",\"text\":\"short\",\"prompt\":\"ignored\"}\r\n"+
			"{\"id\":\"2\",\"text\":null,\"prompt\":\"世界\"}\r\n"+
			"{\"id\":\"3\",\"text\":%s}\n",
		mustJSONString(longBody),
	))
	mustf(t, validateWorkloadJSONL("embed", input), "fixture rejected: %v")
	quoteScan := scanJSONL(input)
	mustf(t, validateInputDepthProfile(quoteScan.InputDepth), "quote scan depth invalid: %v")

	jobID := uuid.New()
	tasks, totalBytes, records, exactBytes, streamedDepth, sum, err :=
		server.streamSplitAndUpload(ctx, jobID, "embed", bytes.NewReader(input), 2, nil)
	defer server.discardOrphanedJobObjects(ctx, jobID, "", tasks)
	mustf(t, err, "stream split/upload: %v")
	if records != 3 || len(tasks) != 2 {
		t.Fatalf("stream geometry records=%d tasks=%d, want 3/2", records, len(tasks))
	}
	if totalBytes <= 0 || exactBytes != len(input) {
		t.Fatalf("stream bytes trimmed/exact=%d/%d, want positive/%d",
			totalBytes, exactBytes, len(input))
	}
	if want := sha256.Sum256(input); sum != want {
		t.Fatalf("stream SHA-256=%x, want %x", sum, want)
	}
	if !inputDepthProfilesEqual(streamedDepth, quoteScan.InputDepth) {
		t.Fatalf("stream depth %+v != quote depth %+v", streamedDepth, quoteScan.InputDepth)
	}
	if streamedDepth.P90DepthBand != inputDepthBandLong {
		t.Fatalf("full-stream p90 band=%q, want long", streamedDepth.P90DepthBand)
	}
	// Runtime learning consumes a task-level bucket, not this job-wide p90. The
	// first chunk is short while the second carries the long outlier.
	if tasks[0].InputDepthBand != inputDepthBandShort || tasks[1].InputDepthBand != inputDepthBandLong {
		t.Fatalf("per-task depth bands=%q/%q, want short/long",
			tasks[0].InputDepthBand, tasks[1].InputDepthBand)
	}
}
