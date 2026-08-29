package main

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"regexp"
	"strings"
	"time"
	"unicode"
)

const maxGovernanceApprovalBundleBytes = 1 << 20

var governanceTimestampPattern = regexp.MustCompile(
	`^[0-9]{4}-[0-9]{2}-[0-9]{2}T[0-9]{2}:[0-9]{2}:[0-9]{2}Z$`,
)

var requiredGovernanceApprovalDomains = map[string]struct{}{
	"security":         {},
	"privacy":          {},
	"legal":            {},
	"licensing":        {},
	"payments":         {},
	"operations":       {},
	"supplier_policy":  {},
	"release_approval": {},
}

var requiredGovernanceExerciseDomains = map[string]struct{}{
	"support_tabletop":           {},
	"security_tabletop":          {},
	"dsar_export_deletion":       {},
	"backup_tombstone":           {},
	"asset_and_model_provenance": {},
}

type governanceApprovalRecord struct {
	Status        string `json:"status"`
	Approver      string `json:"approver"`
	Organization  string `json:"organization"`
	ReviewedScope string `json:"reviewed_scope"`
	EvidenceURI   string `json:"evidence_uri"`
	ApprovedAt    string `json:"approved_at"`
}

type governanceExerciseRecord struct {
	Status      string `json:"status"`
	EvidenceURI string `json:"evidence_uri"`
	CompletedAt string `json:"completed_at"`
}

type governanceApprovalBundle struct {
	SchemaVersion   int                                 `json:"schema_version"`
	CandidateCommit string                              `json:"candidate_commit"`
	Scope           string                              `json:"scope"`
	Approvals       map[string]governanceApprovalRecord `json:"approvals"`
	Exercises       map[string]governanceExerciseRecord `json:"exercises"`
}

func boundedGovernanceText(value, field string, maximum int) error {
	if value != strings.TrimSpace(value) || value == "" || len(value) > maximum {
		return fmt.Errorf("%s must be non-empty, trimmed, and at most %d bytes", field, maximum)
	}
	for _, r := range value {
		if unicode.IsControl(r) {
			return fmt.Errorf("%s contains a control character", field)
		}
	}
	return nil
}

func governanceTimestamp(value, field string, now time.Time) (time.Time, error) {
	if !governanceTimestampPattern.MatchString(value) {
		return time.Time{}, fmt.Errorf("%s must be canonical second-resolution UTC", field)
	}
	parsed, err := time.Parse(time.RFC3339, value)
	if err != nil {
		return time.Time{}, fmt.Errorf("%s must be RFC3339: %w", field, err)
	}
	if parsed.After(now.Add(5 * time.Minute)) {
		return time.Time{}, fmt.Errorf("%s is in the future", field)
	}
	return parsed, nil
}

func validateGovernanceApprovalBundle(raw []byte, expectedCommit string, now time.Time) (string, error) {
	if len(raw) == 0 || len(raw) > maxGovernanceApprovalBundleBytes {
		return "", fmt.Errorf("approval bundle is empty or exceeds %d bytes", maxGovernanceApprovalBundleBytes)
	}
	if !fullCommitPattern.MatchString(expectedCommit) {
		return "", fmt.Errorf("expected candidate commit is not exact")
	}

	var bundle governanceApprovalBundle
	if err := decodeStrictJSONObject(raw, &bundle); err != nil {
		return "", fmt.Errorf("approval bundle is not closed strict JSON: %w", err)
	}
	if bundle.SchemaVersion != 1 ||
		bundle.CandidateCommit != expectedCommit ||
		bundle.Scope != "supervised_stripe_test_mode_private_canary" {
		return "", fmt.Errorf("approval bundle is not bound to this exact candidate and scope")
	}
	if len(bundle.Approvals) != len(requiredGovernanceApprovalDomains) {
		return "", fmt.Errorf("approval bundle does not contain exactly the required domains")
	}

	approvalTimes := make(map[string]time.Time, len(bundle.Approvals))
	for domain, approval := range bundle.Approvals {
		if _, ok := requiredGovernanceApprovalDomains[domain]; !ok {
			return "", fmt.Errorf("approval bundle contains unexpected domain %q", domain)
		}
		if approval.Status != "APPROVED" {
			return "", fmt.Errorf("approval %s is not APPROVED", domain)
		}
		if err := boundedGovernanceText(approval.Approver, domain+".approver", 256); err != nil {
			return "", err
		}
		if err := boundedGovernanceText(approval.Organization, domain+".organization", 256); err != nil {
			return "", err
		}
		if err := boundedGovernanceText(approval.ReviewedScope, domain+".reviewed_scope", 4096); err != nil {
			return "", err
		}
		if err := boundedGovernanceText(approval.EvidenceURI, domain+".evidence_uri", 2048); err != nil {
			return "", err
		}
		approvedAt, err := governanceTimestamp(approval.ApprovedAt, domain+".approved_at", now)
		if err != nil {
			return "", err
		}
		approvalTimes[domain] = approvedAt
	}

	if len(bundle.Exercises) != len(requiredGovernanceExerciseDomains) {
		return "", fmt.Errorf("approval bundle does not contain exactly the required exercises")
	}
	exerciseTimes := make(map[string]time.Time, len(bundle.Exercises))
	for exercise, receipt := range bundle.Exercises {
		if _, ok := requiredGovernanceExerciseDomains[exercise]; !ok {
			return "", fmt.Errorf("approval bundle contains unexpected exercise %q", exercise)
		}
		if receipt.Status != "PASS" {
			return "", fmt.Errorf("exercise %s is not PASS", exercise)
		}
		if err := boundedGovernanceText(receipt.EvidenceURI, exercise+".evidence_uri", 2048); err != nil {
			return "", err
		}
		completedAt, err := governanceTimestamp(receipt.CompletedAt, exercise+".completed_at", now)
		if err != nil {
			return "", err
		}
		exerciseTimes[exercise] = completedAt
	}

	releaseAt := approvalTimes["release_approval"]
	for domain, approvedAt := range approvalTimes {
		if domain != "release_approval" && approvedAt.After(releaseAt) {
			return "", fmt.Errorf("release approval predates %s approval", domain)
		}
	}
	for exercise, completedAt := range exerciseTimes {
		if completedAt.After(releaseAt) {
			return "", fmt.Errorf("release approval predates %s exercise", exercise)
		}
	}

	digest := sha256.Sum256(raw)
	return hex.EncodeToString(digest[:]), nil
}
