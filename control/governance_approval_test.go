package main

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"strings"
	"testing"
	"time"
)

func validGovernanceApprovalBundle(t *testing.T, commit string, now time.Time) []byte {
	t.Helper()
	approvals := make(map[string]governanceApprovalRecord, len(requiredGovernanceApprovalDomains))
	for domain := range requiredGovernanceApprovalDomains {
		approvedAt := now.Add(-2 * time.Hour)
		if domain == "release_approval" {
			approvedAt = now.Add(-time.Hour)
		}
		approvals[domain] = governanceApprovalRecord{
			Status:        "APPROVED",
			Approver:      "Qualified reviewer for " + domain,
			Organization:  "Independent Review Organization",
			ReviewedScope: "Exact supervised test-mode private-canary candidate",
			EvidenceURI:   "s3://governance-evidence/" + domain + ".json",
			ApprovedAt:    approvedAt.UTC().Format(time.RFC3339),
		}
	}
	exercises := make(map[string]governanceExerciseRecord, len(requiredGovernanceExerciseDomains))
	for exercise := range requiredGovernanceExerciseDomains {
		exercises[exercise] = governanceExerciseRecord{
			Status:      "PASS",
			EvidenceURI: "s3://governance-evidence/" + exercise + ".json",
			CompletedAt: now.Add(-3 * time.Hour).UTC().Format(time.RFC3339),
		}
	}
	raw, err := json.Marshal(governanceApprovalBundle{
		SchemaVersion:   1,
		CandidateCommit: commit,
		Scope:           "supervised_stripe_test_mode_private_canary",
		Approvals:       approvals,
		Exercises:       exercises,
	})
	must(t, err)
	return raw
}

func TestGovernanceApprovalBundleRequiresCompleteExactAuthority(t *testing.T) {
	now := time.Date(2026, 7, 28, 12, 0, 0, 0, time.UTC)
	commit := strings.Repeat("a", 40)
	valid := validGovernanceApprovalBundle(t, commit, now)
	digest, err := validateGovernanceApprovalBundle(valid, commit, now)
	mustf(t, err, "valid governance approval bundle rejected: %v")
	want := sha256.Sum256(valid)
	if digest != hex.EncodeToString(want[:]) {
		t.Fatalf("approval digest=%s, want %x", digest, want)
	}

	var base governanceApprovalBundle
	must(t, json.Unmarshal(valid, &base))
	mutate := func(t *testing.T, change func(*governanceApprovalBundle)) {
		t.Helper()
		var candidate governanceApprovalBundle
		must(t, json.Unmarshal(valid, &candidate))
		change(&candidate)
		raw, err := json.Marshal(candidate)
		must(t, err)
		if _, err := validateGovernanceApprovalBundle(raw, commit, now); err == nil {
			t.Fatal("hostile governance approval mutation was accepted")
		}
	}

	t.Run("wrong candidate", func(t *testing.T) {
		mutate(t, func(bundle *governanceApprovalBundle) {
			bundle.CandidateCommit = strings.Repeat("b", 40)
		})
	})
	t.Run("wrong scope", func(t *testing.T) {
		mutate(t, func(bundle *governanceApprovalBundle) { bundle.Scope = "public_live_money" })
	})
	t.Run("missing domain", func(t *testing.T) {
		mutate(t, func(bundle *governanceApprovalBundle) { delete(bundle.Approvals, "security") })
	})
	t.Run("legacy wrong domain set", func(t *testing.T) {
		mutate(t, func(bundle *governanceApprovalBundle) {
			delete(bundle.Approvals, "licensing")
			bundle.Approvals["license"] = base.Approvals["licensing"]
		})
	})
	t.Run("pending approval", func(t *testing.T) {
		mutate(t, func(bundle *governanceApprovalBundle) {
			record := bundle.Approvals["payments"]
			record.Status = "PENDING"
			bundle.Approvals["payments"] = record
		})
	})
	t.Run("missing identity", func(t *testing.T) {
		mutate(t, func(bundle *governanceApprovalBundle) {
			record := bundle.Approvals["legal"]
			record.Approver = ""
			bundle.Approvals["legal"] = record
		})
	})
	t.Run("whitespace identity", func(t *testing.T) {
		mutate(t, func(bundle *governanceApprovalBundle) {
			record := bundle.Approvals["legal"]
			record.Approver = "   "
			bundle.Approvals["legal"] = record
		})
	})
	t.Run("control character identity", func(t *testing.T) {
		mutate(t, func(bundle *governanceApprovalBundle) {
			record := bundle.Approvals["legal"]
			record.Approver = "reviewer\ninjected"
			bundle.Approvals["legal"] = record
		})
	})
	t.Run("future approval", func(t *testing.T) {
		mutate(t, func(bundle *governanceApprovalBundle) {
			record := bundle.Approvals["security"]
			record.ApprovedAt = now.Add(time.Hour).Format(time.RFC3339)
			bundle.Approvals["security"] = record
		})
	})
	t.Run("noncanonical timestamp", func(t *testing.T) {
		mutate(t, func(bundle *governanceApprovalBundle) {
			record := bundle.Approvals["security"]
			record.ApprovedAt = "2026-07-28T10:00:00.000Z"
			bundle.Approvals["security"] = record
		})
	})
	t.Run("premature release approval", func(t *testing.T) {
		mutate(t, func(bundle *governanceApprovalBundle) {
			record := bundle.Approvals["release_approval"]
			record.ApprovedAt = now.Add(-4 * time.Hour).Format(time.RFC3339)
			bundle.Approvals["release_approval"] = record
		})
	})
	t.Run("exercise after release", func(t *testing.T) {
		mutate(t, func(bundle *governanceApprovalBundle) {
			record := bundle.Exercises["security_tabletop"]
			record.CompletedAt = now.Add(-30 * time.Minute).Format(time.RFC3339)
			bundle.Exercises["security_tabletop"] = record
		})
	})
	t.Run("missing exercise evidence", func(t *testing.T) {
		mutate(t, func(bundle *governanceApprovalBundle) {
			record := bundle.Exercises["backup_tombstone"]
			record.EvidenceURI = ""
			bundle.Exercises["backup_tombstone"] = record
		})
	})

	duplicate := []byte(`{"schema_version":1,"schema_version":1}`)
	if _, err := validateGovernanceApprovalBundle(duplicate, commit, now); err == nil {
		t.Fatal("duplicate governance JSON keys were accepted")
	}
	unknown := append(valid[:len(valid)-1], []byte(`,"invented_authority":true}`)...)
	if _, err := validateGovernanceApprovalBundle(unknown, commit, now); err == nil {
		t.Fatal("unknown governance authority field was accepted")
	}
	oversized := make([]byte, maxGovernanceApprovalBundleBytes+1)
	if _, err := validateGovernanceApprovalBundle(oversized, commit, now); err == nil {
		t.Fatal("oversized governance approval bundle was accepted")
	}
}
