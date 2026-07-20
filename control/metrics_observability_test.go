package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestReadBackupSignal(t *testing.T) {
	now := time.Unix(1_800_000_000, 0)
	path := filepath.Join(t.TempDir(), "last-success")
	if err := os.WriteFile(path, []byte("1799999880\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	got := readBackupSignal(now, path)
	if !got.configured || !got.valid || got.lastSuccess != 1_799_999_880 || got.ageSeconds != 120 {
		t.Fatalf("unexpected valid backup signal: %+v", got)
	}

	if err := os.WriteFile(path, []byte("not-a-timestamp\n"), 0o600); err != nil {
		t.Fatal(err)
	}
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
