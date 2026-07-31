package main

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"
)

// Turning the canary off is a decision, so in production it has to resolve to one.
//
// MERC_CANARY_MODE=false does two things at once: it stops enforcing the canary
// envelope and it opens self-serve signup. Until this file existed the only thing
// between a production deployment and that state was a non-empty environment
// variable, so MERC_CANARY_DISABLE_DECISION_REF=yes would have admitted the
// public with no record of anybody deciding to. The reference now has to name an
// artifact that says what was decided, about which candidate, by whom, from when
// and until when — the same binding MERC_LIVE_PAYMENT_ACTIVATION_FILE already
// requires, because "who may transact here" is the same class of question as "how
// much may move".
//
// Outside production the reference stays a free-text note. Development stacks and
// the money-path fixtures set it to strings like "TEST-money-path"; making them
// carry an artifact would buy nothing — there is no public to admit — and would
// bury the rule under fixture plumbing.
const (
	canaryDisableDecisionDigestEnv = "MERC_CANARY_DISABLE_DECISION_SHA256"
	maxCanaryDisableDecisionBytes  = 32 << 10
	// canaryDisableDecisionVerb is the only thing this artifact is allowed to
	// say. A governed document that authorizes something else is not permission
	// to open admission, however well signed it is.
	canaryDisableDecisionVerb = "DISABLE_CANARY_ADMISSION"
	// canaryDisableDecisionScope is the deployment the decision is about, spelled
	// exactly as the governance approval bundle spells it, so one vocabulary
	// covers both artifacts.
	canaryDisableDecisionScope = "supervised_stripe_test_mode_private_canary"
)

// Named refusals. Each one is a different operator mistake with a different fix,
// and a single "canary policy is incomplete" would send all of them to the same
// place — which is how a stale decision and an absent one came to look alike.
var (
	errCanaryDecisionMissing        = errors.New("canary disable decision reference is absent")
	errCanaryDecisionUnreadable     = errors.New("canary disable decision cannot be read")
	errCanaryDecisionUnverified     = errors.New("canary disable decision digest was never declared")
	errCanaryDecisionTampered       = errors.New("canary disable decision does not match its declared digest")
	errCanaryDecisionMalformed      = errors.New("canary disable decision is malformed")
	errCanaryDecisionWrongCandidate = errors.New("canary disable decision was taken about a different candidate")
	errCanaryDecisionNotInForce     = errors.New("canary disable decision is not in force yet")
	errCanaryDecisionExpired        = errors.New("canary disable decision has expired")
)

// canaryDisableDecisionRoles is the authority the decision must carry.
//
// Two independent humans: release_manager owns what is being deployed, security
// owns who may reach it. "payments" is deliberately absent — this decision moves
// no money, and the live payment activation, which does, carries its own payments
// approval. Requiring a payments signature here would train operators to collect
// signatures that mean nothing.
var canaryDisableDecisionRoles = []string{"release_manager", "security"}

type canaryDecisionAuthority struct {
	Role      string `json:"role"`
	Approver  string `json:"approver"`
	Reference string `json:"reference"`
}

// canaryDisableDecision is the governed record itself. ExpiresAt is optional: a
// decision to run open-signup indefinitely is a real decision, and forcing a
// fake expiry on it would only produce artifacts nobody renews.
type canaryDisableDecision struct {
	DecisionID      string                    `json:"decision_id"`
	CandidateCommit string                    `json:"candidate_commit"`
	Decision        string                    `json:"decision"`
	Scope           string                    `json:"scope"`
	Authority       []canaryDecisionAuthority `json:"authority"`
	EffectiveAt     string                    `json:"effective_at"`
	ExpiresAt       string                    `json:"expires_at,omitempty"`
}

type canaryDisableDecisionEnvelope struct {
	SchemaVersion int                   `json:"schema_version"`
	Decision      canaryDisableDecision `json:"decision"`
}

// resolveCanaryDisableDecision reports whether the reference authorizes open
// buyer admission on this candidate, right now.
//
// Re-read on every call rather than cached, like the live payment activation, so
// that revoking a decision is deleting a file rather than restarting a fleet. The
// callers are per-job, not per-token.
//
// build is a function because resolving it is not free — it parses the embedded
// VCS stamp and touches the price-board cache — and every caller outside
// production, which is most of them, never needs it.
func resolveCanaryDisableDecision(ref string, production bool, now time.Time, build func() ControlBuildInfo) error {
	if ref == "" {
		return fmt.Errorf("%w: %s must name the decision that also opens self-serve signup",
			errCanaryDecisionMissing, canaryDisableDecisionEnv)
	}
	if !production {
		return nil
	}
	raw, err := readCanaryDisableDecision(ref)
	if err != nil {
		return err
	}
	var envelope canaryDisableDecisionEnvelope
	if err := decodeStrictJSONObject(raw, &envelope); err != nil {
		return fmt.Errorf("%w: %v", errCanaryDecisionMalformed, err)
	}
	return validateCanaryDisableDecision(envelope, now, build())
}

// readCanaryDisableDecision returns the bytes the operator declared, or refuses.
//
// The digest is required rather than optional because the file is mounted into
// the container: without it, anything that can write that volume — a restored
// backup, a drifted config repo, a sidecar — rewrites the admission decision and
// the control plane reads the new one as gospel. The digest does not stop an
// attacker who can already edit the deployment's environment; that attacker can
// declare a digest for their own file. Closing that would take a signing key held
// outside the deployment, the way the live payment activation holds one, and it
// is worth adding the day this artifact governs live money rather than a private
// canary.
func readCanaryDisableDecision(path string) ([]byte, error) {
	if !filepath.IsAbs(path) {
		return nil, fmt.Errorf("%w: %s must be an absolute path to a decision artifact in production, not the bare reference %q",
			errCanaryDecisionUnreadable, canaryDisableDecisionEnv, path)
	}
	info, err := os.Stat(path)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", errCanaryDecisionUnreadable, err)
	}
	if !info.Mode().IsRegular() || info.Mode().Perm()&0o027 != 0 {
		return nil, fmt.Errorf("%w: must be a regular file, not group-writable, and inaccessible to other users",
			errCanaryDecisionUnreadable)
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", errCanaryDecisionUnreadable, err)
	}
	if len(raw) == 0 || len(raw) > maxCanaryDisableDecisionBytes {
		return nil, fmt.Errorf("%w: file size is out of range", errCanaryDecisionUnreadable)
	}
	sum := sha256.Sum256(raw)
	actual := hex.EncodeToString(sum[:])
	expected := strings.ToLower(strings.TrimSpace(os.Getenv(canaryDisableDecisionDigestEnv)))
	if !digestPattern.MatchString(expected) {
		return nil, fmt.Errorf("%w: %s must carry the sha256 of %s",
			errCanaryDecisionUnverified, canaryDisableDecisionDigestEnv, path)
	}
	if !hmac.Equal([]byte(actual), []byte(expected)) {
		return nil, fmt.Errorf("%w: file is %s, %s declares %s",
			errCanaryDecisionTampered, actual, canaryDisableDecisionDigestEnv, expected)
	}
	return raw, nil
}

func validateCanaryDisableDecision(envelope canaryDisableDecisionEnvelope, now time.Time, build ControlBuildInfo) error {
	if envelope.SchemaVersion != 1 {
		return fmt.Errorf("%w: unsupported schema version %d", errCanaryDecisionMalformed, envelope.SchemaVersion)
	}
	d := envelope.Decision
	if !activationIDPattern.MatchString(d.DecisionID) {
		return fmt.Errorf("%w: decision_id is not a usable receipt identity", errCanaryDecisionMalformed)
	}
	if d.Decision != canaryDisableDecisionVerb || d.Scope != canaryDisableDecisionScope {
		return fmt.Errorf("%w: decision must be %q in scope %q",
			errCanaryDecisionMalformed, canaryDisableDecisionVerb, canaryDisableDecisionScope)
	}
	// Bound to the running binary, not to the repository. A decision taken about
	// last week's candidate says nothing about the code that would serve the
	// buyers it admits, and a modified build is not any candidate at all.
	if !fullCommitPattern.MatchString(d.CandidateCommit) {
		return fmt.Errorf("%w: candidate_commit must be a full lowercase Git SHA-1", errCanaryDecisionMalformed)
	}
	if build.Modified || !fullCommitPattern.MatchString(build.Commit) || build.Commit != d.CandidateCommit {
		return fmt.Errorf("%w: decision=%s build=%s modified=%t",
			errCanaryDecisionWrongCandidate, d.CandidateCommit, build.Commit, build.Modified)
	}
	if err := validateCanaryDecisionAuthority(d.Authority); err != nil {
		return err
	}

	effectiveAt, err := canaryDecisionTime(d.EffectiveAt, "effective_at")
	if err != nil {
		return err
	}
	if now.Before(effectiveAt) {
		return fmt.Errorf("%w: effective_at is %s", errCanaryDecisionNotInForce, d.EffectiveAt)
	}
	if d.ExpiresAt == "" {
		return nil
	}
	expiresAt, err := canaryDecisionTime(d.ExpiresAt, "expires_at")
	if err != nil {
		return err
	}
	if !effectiveAt.Before(expiresAt) {
		return fmt.Errorf("%w: expires_at does not follow effective_at", errCanaryDecisionMalformed)
	}
	if !now.Before(expiresAt) {
		return fmt.Errorf("%w: expires_at was %s", errCanaryDecisionExpired, d.ExpiresAt)
	}
	return nil
}

func validateCanaryDecisionAuthority(records []canaryDecisionAuthority) error {
	seen := make(map[string]bool, len(records))
	for _, record := range records {
		role := strings.ToLower(strings.TrimSpace(record.Role))
		if seen[role] {
			return fmt.Errorf("%w: duplicate authority role %q", errCanaryDecisionMalformed, role)
		}
		// An unknown role is refused rather than ignored: a decision signed by
		// three people, none of whom hold the two roles that matter, must not read
		// as better-approved than one signed by the two who do.
		if !slices.Contains(canaryDisableDecisionRoles, role) {
			return fmt.Errorf("%w: unsupported authority role %q", errCanaryDecisionMalformed, record.Role)
		}
		if err := boundedGovernanceText(record.Approver, role+".approver", 256); err != nil {
			return fmt.Errorf("%w: %v", errCanaryDecisionMalformed, err)
		}
		if err := boundedGovernanceText(record.Reference, role+".reference", 512); err != nil {
			return fmt.Errorf("%w: %v", errCanaryDecisionMalformed, err)
		}
		seen[role] = true
	}
	for _, role := range canaryDisableDecisionRoles {
		if !seen[role] {
			return fmt.Errorf("%w: missing %s authority", errCanaryDecisionMalformed, role)
		}
	}
	return nil
}

// canaryDecisionTime reads a governed timestamp in the one format the governance
// artifacts already use, so "when was this decided" cannot depend on which
// timezone the operator's shell was in.
func canaryDecisionTime(value, field string) (time.Time, error) {
	if !governanceTimestampPattern.MatchString(value) {
		return time.Time{}, fmt.Errorf("%w: %s must be canonical second-resolution UTC",
			errCanaryDecisionMalformed, field)
	}
	parsed, err := time.Parse(time.RFC3339, value)
	if err != nil {
		return time.Time{}, fmt.Errorf("%w: %s: %v", errCanaryDecisionMalformed, field, err)
	}
	return parsed, nil
}
