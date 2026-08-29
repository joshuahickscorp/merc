package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// TestBackupAgeMetricObservation is the probe ops/scripts/test-backup-age-metric.sh
// parses. It reads MERC_BACKUP_STATUS_FILE through the same function the
// /metrics handler uses and prints one JSON object.
func TestBackupAgeMetricObservation(t *testing.T) {
	path := strings.TrimSpace(os.Getenv("MERC_BACKUP_STATUS_FILE"))
	if path == "" {
		t.Skip("MERC_BACKUP_STATUS_FILE is unset")
	}
	got := readBackupSignal(time.Now(), path)
	t.Logf("BACKUP_AGE_OBSERVATION {\"configured\":%t,\"valid\":%t,\"age_seconds\":%.3f,\"last_success\":%.0f}",
		got.configured, got.valid, got.ageSeconds, got.lastSuccess)
}

func TestReadBackupSignal(t *testing.T) {
	now := time.Unix(1_800_000_000, 0)
	path := filepath.Join(t.TempDir(), "last-success")
	must(t, os.WriteFile(path, []byte("1799999880\n"), 0o600))
	got := readBackupSignal(now, path)
	if !got.configured || !got.valid || got.lastSuccess != 1_799_999_880 || got.ageSeconds != 120 {
		t.Fatalf("unexpected valid backup signal: %+v", got)
	}

	must(t, os.WriteFile(path, []byte("not-a-timestamp\n"), 0o600))
	got = readBackupSignal(now, path)
	if !got.configured || got.valid {
		t.Fatalf("malformed signal must be configured but invalid: %+v", got)
	}

	got = readBackupSignal(now, "")
	if got.configured || got.valid {
		t.Fatalf("blank signal path must be unconfigured: %+v", got)
	}
}

func TestMetricLabelValueIsBoundedAndSanitized(t *testing.T) {
	if got := metricLabelValue(" release\n\"candidate "); got != "release__candidate" {
		t.Fatalf("unexpected sanitized label %q", got)
	}
	got := metricLabelValue(strings.Repeat("x", 200))
	if len(got) != 96 {
		t.Fatalf("label length=%d, want 96", len(got))
	}
	if got := metricLabelValue(" \t "); got != "unknown" {
		t.Fatalf("blank label=%q, want unknown", got)
	}
}

func TestTickerIntervalSnapshotIsBoundedToRegisteredTickers(t *testing.T) {
	l := &tickerLiveness{entries: map[string]*tickerStat{}}
	l.register("fast", 2*time.Second)
	l.register("slow", time.Hour)
	got := l.intervalSnapshot()
	if len(got) != 2 || got["fast"] != 2 || got["slow"] != 3600 {
		t.Fatalf("unexpected interval snapshot: %#v", got)
	}
}

func TestProgressingTickerUsesNoProgressBudgetInsteadOfScheduleInterval(t *testing.T) {
	start := time.Unix(1_800_000_000, 0)
	l := &tickerLiveness{entries: map[string]*tickerStat{}}
	l.registerWithProgressTimeout("verification", 2*time.Second, 30*time.Second)
	l.markStart("verification", start)

	if stale := l.stale(start.Add(20*time.Second), start); len(stale) != 0 {
		t.Fatalf("legitimately running sweep marked stale by schedule interval: %v", stale)
	}
	l.markProgress("verification", start.Add(20*time.Second))
	if stale := l.stale(start.Add(45*time.Second), start); len(stale) != 0 {
		t.Fatalf("recently progressing sweep marked stale: %v", stale)
	}
	if stale := l.stale(start.Add(51*time.Second), start); len(stale) != 1 || stale[0] != "verification" {
		t.Fatalf("no-progress sweep did not become stale: %v", stale)
	}
}
