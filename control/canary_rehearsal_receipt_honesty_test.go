package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// The L11 live rehearsal is operator-controlled. These receipts must never
// be readable as EXTERNAL_ALPHA_PROVEN or as independent external use.
func TestCanaryRehearsalReceiptsCannotSatisfyExternalAlpha(t *testing.T) {
	root := alphaHonestyRepoRoot(t)
	dir := filepath.Join(root, "evidence", "canary")
	matches, err := filepath.Glob(filepath.Join(dir, "l11-p1-canary-rehearsal-*.json"))
	if err != nil {
		t.Fatal(err)
	}
	if len(matches) == 0 {
		t.Fatal("expected l11-p1-canary-rehearsal-*.json receipts")
	}
	for _, path := range matches {
		raw, err := os.ReadFile(path)
		if err != nil {
			t.Fatalf("read %s: %v", path, err)
		}
		var doc map[string]any
		if err := json.Unmarshal(raw, &doc); err != nil {
			t.Fatalf("parse %s: %v", path, err)
		}
		if got := strings.TrimSpace(strField(doc, "does_not_satisfy")); got != "EXTERNAL_ALPHA_PROVEN" {
			t.Errorf("%s: does_not_satisfy=%q", filepath.Base(path), got)
		}
		if doc["external_alpha_proven"] != false {
			t.Errorf("%s: external_alpha_proven must be false", filepath.Base(path))
		}
		if doc["synthetic"] != true {
			t.Errorf("%s: synthetic must be true", filepath.Base(path))
		}
		if got := strings.TrimSpace(strField(doc, "participant_class")); got != "operator_controlled" {
			t.Errorf("%s: participant_class=%q", filepath.Base(path), got)
		}
		if doc["controlled_by_operator"] != true {
			t.Errorf("%s: controlled_by_operator must be true", filepath.Base(path))
		}
		if claim := strings.TrimSpace(strField(doc, "claim")); claim == "EXTERNAL_ALPHA_PROVEN" {
			t.Errorf("%s: must not carry claim EXTERNAL_ALPHA_PROVEN", filepath.Base(path))
		}
	}
}

func strField(doc map[string]any, key string) string {
	v, _ := doc[key].(string)
	return v
}
