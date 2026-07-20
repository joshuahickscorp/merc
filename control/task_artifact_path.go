package main

import (
	"fmt"
	"strings"

	"github.com/google/uuid"
)

// taskAttemptResultKey is the sole staging-object namespace for worker output.
// An attempt epoch is part of the key so a presigned URL from an abandoned
// execution can never overwrite the object consumed by a later execution.
func taskAttemptResultKey(jobID, taskID uuid.UUID, attempt int16) string {
	if jobID == uuid.Nil || taskID == uuid.Nil || attempt < 0 {
		return ""
	}
	return fmt.Sprintf("jobs/%s/tasks/%s/attempt-%d/result.json", jobID, taskID, attempt)
}

func validateTaskAttemptResultKey(jobID, taskID uuid.UUID, attempt int16, key string) error {
	want := taskAttemptResultKey(jobID, taskID, attempt)
	if want == "" || key != want {
		return fmt.Errorf("task %s attempt %d result key is %q, want %q", taskID, attempt, key, want)
	}
	return nil
}

func reportedResultDigestMatches(reported, sealed string) bool {
	reported = strings.TrimSpace(reported)
	return reported == "" || reported == sealed
}
