package main

import (
	"testing"
	"time"
)

func TestWorkerElectionReadinessRequiresRecentObservationWhenEnabled(t *testing.T) {
	setWorkerElectionReadinessEnabled(false)
	t.Cleanup(func() { setWorkerElectionReadinessEnabled(false) })
	now := time.Unix(1_700_000_000, 0)
	if !workerElectionRecentlyObserved(now) {
		t.Fatal("explicitly disabled worker election should not make an API-only replica unready")
	}

	setWorkerElectionReadinessEnabled(true)
	if workerElectionRecentlyObserved(now) {
		t.Fatal("enabled election with no successful observation reported ready")
	}
	markWorkerElectionObserved(now)
	if !workerElectionRecentlyObserved(now.Add(workerLeaderObservationMaxAge)) {
		t.Fatal("observation at the freshness boundary reported stale")
	}
	if workerElectionRecentlyObserved(now.Add(workerLeaderObservationMaxAge + time.Nanosecond)) {
		t.Fatal("stale election observation reported ready")
	}
}
