package main

import (
	"net/http/httptest"
	"testing"
)

func TestSubmissionIdempotencyKeyContract(t *testing.T) {
	for _, key := range []string{"submit-1", "buyer.retry:2026_07_19", "12345678"} {
		if !idempotencyKeyPattern.MatchString(key) {
			t.Errorf("valid idempotency key %q was rejected", key)
		}
	}
	for _, key := range []string{"", "short", "contains space", "contains/slash"} {
		if idempotencyKeyPattern.MatchString(key) {
			t.Errorf("invalid idempotency key %q was accepted", key)
		}
	}
}

func TestTaskAttemptHeaderFencesExecutionEpoch(t *testing.T) {
	r := httptest.NewRequest("POST", "/v1/worker/task/ignored/start", nil)
	if _, err := taskAttemptHeader(r); err == nil {
		t.Fatal("missing attempt header was accepted")
	}
	r.Header.Set(taskAttemptHeaderName, "-1")
	if _, err := taskAttemptHeader(r); err == nil {
		t.Fatal("negative attempt header was accepted")
	}
	r.Header.Set(taskAttemptHeaderName, "7")
	if got, err := taskAttemptHeader(r); err != nil || got != 7 {
		t.Fatalf("attempt header = %d, %v; want 7, nil", got, err)
	}
}
