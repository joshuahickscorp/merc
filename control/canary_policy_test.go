package main

import (
	"encoding/json"
	"errors"
	"os"
	"strings"
	"testing"

	"github.com/google/uuid"
)

func setValidCanaryEnv(t *testing.T, workerID uuid.UUID) {
	t.Helper()
	t.Setenv("MERC_CANARY_MODE", "true")
	t.Setenv("MERC_CANARY_APPROVED_BUYER_EMAILS", "buyer@example.test, second@example.test")
	t.Setenv("MERC_CANARY_APPROVED_WORKER_IDS", workerID.String()+","+uuid.NewString())
	t.Setenv("MERC_CANARY_APPROVED_AGENT_VERSIONS", "0.1.0")
	// Extra reviewed hash plus the sealed r6 identity. Canary must name the
	// sealed production hash; a test-only extra hash is allowed beside it.
	t.Setenv("MERC_CANARY_APPROVED_BUILD_HASHES", sealedCandleMetalLlama1InferBuildHash+",0123456789abcdef")
	t.Setenv("MERC_CANARY_MAX_ACTIVE_BUYERS", "2")
	t.Setenv("MERC_CANARY_MAX_ACTIVE_WORKERS", "2")
	t.Setenv("MERC_CANARY_MAX_QUEUED_JOBS", "20")
	t.Setenv("MERC_CANARY_MAX_TASKS_PER_JOB", "64")
	t.Setenv("MERC_CANARY_MAX_ARTIFACT_BYTES", "33554432")
	t.Setenv("MERC_CANARY_MAX_INPUT_BYTES", "33554432")
	t.Setenv("MERC_CANARY_MAX_OUTPUT_TOKENS", "256")
	t.Setenv("MERC_CANARY_MAX_JOB_DURATION_SECS", "600")
	t.Setenv("MERC_CANARY_MAX_RETRIES", "3")
	t.Setenv("MERC_CANARY_MAX_DAILY_JOBS", "100")
	t.Setenv("MERC_CANARY_MAX_SHADOW_VALUE_USD", "10")
	t.Setenv("MERC_CANARY_MAX_HELD_SHADOW_PAYOUT_USD", "10")
}

func TestCanaryPolicyIsFailClosedAndBounded(t *testing.T) {
	workerID := uuid.New()
	setValidCanaryEnv(t, workerID)
	p := loadCanaryPolicyFromEnv()
	if p.configError != nil {
		t.Fatalf("valid policy: %v", p.configError)
	}
	if !p.allowsBuyerEmail(" BUYER@example.test ") || p.allowsBuyerEmail("other@example.test") {
		t.Fatal("buyer allowlist was not exact")
	}
	if !p.allowsWorker(workerID) || p.allowsWorker(uuid.New()) {
		t.Fatal("worker allowlist was not exact")
	}
	sessionID := uuid.New()
	approvedRuntime := WorkerCapability{
		AgentVersion: "0.1.0", BuildHash: "0123456789abcdef",
		AgentSessionID: &sessionID,
	}
	if !p.allowsWorkerRuntime(approvedRuntime) {
		t.Fatal("reviewed worker runtime was rejected")
	}
	approvedRuntime.AgentVersion = "0.0.9"
	if p.allowsWorkerRuntime(approvedRuntime) {
		t.Fatal("agent downgrade was accepted")
	}
	approvedRuntime.AgentVersion = "0.1.0"
	approvedRuntime.BuildHash = "fedcba9876543210"
	if p.allowsWorkerRuntime(approvedRuntime) {
		t.Fatal("unreviewed source build was accepted")
	}
	approvedRuntime.BuildHash = "0123456789abcdef"
	approvedRuntime.AgentSessionID = nil
	if p.allowsWorkerRuntime(approvedRuntime) {
		t.Fatal("worker without a process session identity was accepted")
	}
	valid := jobSubmit{
		JobType:     JobType{Type: "batch_infer", MaxTokens: 256},
		Constraints: JobConstraints{MaxDurationSecs: 600},
		MaxUSD:      10,
	}
	mustf(t, p.validateJobShape(valid), "valid shape: %v")
	for name, mutate := range map[string]func(*jobSubmit){
		"temperature": func(s *jobSubmit) { s.JobType.Temperature = 0.1 },
		"tokens":      func(s *jobSubmit) { s.JobType.MaxTokens++ },
		"duration":    func(s *jobSubmit) { s.Constraints.MaxDurationSecs++ },
		"shadow":      func(s *jobSubmit) { s.MaxUSD += 0.01 },
	} {
		t.Run(name, func(t *testing.T) {
			bad := valid
			mutate(&bad)
			if err := p.validateJobShape(bad); err == nil {
				t.Fatal("out-of-envelope job accepted")
			}
		})
	}
}

func TestCanaryPolicyRejectsMalformedBuildAllowlist(t *testing.T) {
	setValidCanaryEnv(t, uuid.New())
	for _, value := range []string{"short", "0123456789ABCDEF", "0123456789abcdeg"} {
		t.Run(value, func(t *testing.T) {
			t.Setenv("MERC_CANARY_APPROVED_BUILD_HASHES", value)
			if p := loadCanaryPolicyFromEnv(); p.configError == nil {
				t.Fatal("malformed build hash allowlist was accepted")
			}
		})
	}
}

func TestCanaryPolicyRequiresSealedCatalogueBuildHash(t *testing.T) {
	setValidCanaryEnv(t, uuid.New())
	t.Setenv("MERC_CANARY_APPROVED_BUILD_HASHES", "f4303a751ca2b2af")
	p := loadCanaryPolicyFromEnv()
	if p.configError == nil {
		t.Fatal("superseded r5 measurement hash was accepted as the sole canary build identity")
	}
	if !strings.Contains(p.configError.Error(), sealedCandleMetalLlama1InferBuildHash) {
		t.Fatalf("refusal did not name the sealed hash: %v", p.configError)
	}
	t.Setenv("MERC_CANARY_APPROVED_BUILD_HASHES", sealedCandleMetalLlama1InferBuildHash)
	p = loadCanaryPolicyFromEnv()
	if p.configError != nil {
		t.Fatalf("sealed r6 hash was refused: %v", p.configError)
	}
	sessionID := uuid.New()
	if !p.allowsWorkerRuntime(WorkerCapability{
		AgentVersion: "0.1.0", BuildHash: sealedCandleMetalLlama1InferBuildHash,
		AgentSessionID: &sessionID,
	}) {
		t.Fatal("sealed r6 worker runtime was rejected")
	}
}

func TestStagingParticipantsAllowlistNamesSealedHashNotSupersededR5(t *testing.T) {
	raw, err := os.ReadFile("../ops/staging/alpha-participants.json")
	if err != nil {
		t.Fatalf("read staging allowlist: %v", err)
	}
	var doc struct {
		BuildHashes []struct {
			Hash string `json:"hash"`
		} `json:"build_hashes"`
	}
	if err := json.Unmarshal(raw, &doc); err != nil {
		t.Fatalf("parse staging allowlist: %v", err)
	}
	seen := map[string]bool{}
	for _, entry := range doc.BuildHashes {
		seen[entry.Hash] = true
	}
	if !seen[sealedCandleMetalLlama1InferBuildHash] {
		t.Fatalf("ops/staging/alpha-participants.json does not name sealed hash %s",
			sealedCandleMetalLlama1InferBuildHash)
	}
	if seen["f4303a751ca2b2af"] {
		t.Fatal("ops/staging/alpha-participants.json still allowlists superseded r5 hash f4303a751ca2b2af")
	}
}

// TestPrivateCanaryStillRefusedByUniformTaskEconomics pins the remaining
// live-staging 503. Quote and submit call validateCurrentUniformCanaryAuthority
// with s.canary.Enabled; that function (control/task_economic_authority.go)
// refuses every canary job because private-canary policy is still defined as
// requiring a heterogeneous honeypot, which uniform v1 cannot allocate.
// Lifting this without first removing TaskReceipt.IsHoneypot from the buyer
// receipt would open a verification-evasion channel (see receipt_test.go).
func TestPrivateCanaryStillRefusedByUniformTaskEconomics(t *testing.T) {
	err := validateCurrentUniformCanaryAuthority(true)
	if err == nil {
		t.Fatal("canary admission is no longer refused; confirm TaskReceipt.IsHoneypot is gone from the buyer surface before enabling honeypot tasks")
	}
	if !errors.Is(err, errHeterogeneousTaskEconomicsUnavailable) {
		t.Fatalf("want errHeterogeneousTaskEconomicsUnavailable, got %v", err)
	}
	if !strings.Contains(err.Error(), "private canary requires a heterogeneous honeypot") {
		t.Fatalf("refusal text changed: %v", err)
	}
	if err := validateCurrentUniformCanaryAuthority(false); err != nil {
		t.Fatalf("non-canary admission was refused: %v", err)
	}
}

func TestCanaryPolicyRequiresExplicitAllowlistAndLimits(t *testing.T) {
	t.Setenv("MERC_CANARY_MODE", "true")
	p := loadCanaryPolicyFromEnv()
	if p.configError == nil || p.allowsBuyerEmail("any@example.test") || p.allowsWorker(uuid.New()) {
		t.Fatal("incomplete canary policy did not fail closed")
	}
}

func TestClampMaxDurationSecsPlatformCeiling(t *testing.T) {
	t.Setenv("MERC_MAX_JOB_DURATION_SECS", "")
	if got := clampMaxDurationSecs(0); got != defaultMaxJobDurationSecs {
		t.Fatalf("0 (unbounded) must become default ceiling, got %d", got)
	}
	if got := clampMaxDurationSecs(defaultMaxJobDurationSecs + 1); got != defaultMaxJobDurationSecs {
		t.Fatalf("over-ceiling must clamp, got %d", got)
	}
	if got := clampMaxDurationSecs(600); got != 600 {
		t.Fatalf("in-range value must pass through, got %d", got)
	}
	t.Setenv("MERC_MAX_JOB_DURATION_SECS", "120")
	if got := clampMaxDurationSecs(9999); got != 120 {
		t.Fatalf("env ceiling must apply, got %d", got)
	}
	// A rejected env value must leave the default ceiling in force.  Probe with a
	// duration ABOVE that default, otherwise the value passes through untouched
	// and the assertion says nothing about which ceiling was used.
	overDefault := defaultMaxJobDurationSecs + 1
	t.Setenv("MERC_MAX_JOB_DURATION_SECS", "0")
	if got := clampMaxDurationSecs(overDefault); got != defaultMaxJobDurationSecs {
		t.Fatalf("zero env must fall back to the default ceiling, got %d", got)
	}
	if got := clampMaxDurationSecs(600); got != 600 {
		t.Fatalf("zero env must still pass an in-range value through, got %d", got)
	}
	t.Setenv("MERC_MAX_JOB_DURATION_SECS", "nope")
	if got := clampMaxDurationSecs(overDefault); got != defaultMaxJobDurationSecs {
		t.Fatalf("malformed env must fall back to the default ceiling, got %d", got)
	}
}

func TestCanaryDisableRequiresRecordedDecision(t *testing.T) {
	// A bare reference only satisfies a development stack. Left to ambient
	// MERC_ENV this test would pass for the wrong reason on a developer's shell
	// and fail on a production-shaped one.
	t.Setenv("MERC_ENV", "development")
	t.Setenv("MERC_CANARY_MODE", "false")
	t.Setenv("MERC_CANARY_DISABLE_DECISION_REF", "")
	p := loadCanaryPolicyFromEnv()
	if !p.Enabled || p.configError == nil {
		t.Fatal("MERC_CANARY_MODE=false without a decision ref must fail closed with canary still enabled")
	}
	t.Setenv("MERC_CANARY_DISABLE_DECISION_REF", "INC-canary-exit-1")
	p = loadCanaryPolicyFromEnv()
	if p.Enabled || p.configError != nil {
		t.Fatalf("explicit false with decision ref should disable canary: enabled=%v err=%v", p.Enabled, p.configError)
	}
}

func TestCanaryMoneyModeRefusesLiveAndAmbiguousRails(t *testing.T) {
	valid := func(key, cash, connect, client, payoutExport string) error {
		return validateCanaryMoneyMode("true", key, cash, connect, client, payoutExport)
	}
	mustf(t, valid("sk_test_example", "whsec_cash", "whsec_connect", "ca_test", ""), "valid test-mode configuration rejected: %v")
	for name, err := range map[string]error{
		"live":              valid("sk_live_forbidden", "whsec_cash", "whsec_connect", "ca_test", ""),
		"missing":           valid("", "whsec_cash", "whsec_connect", "ca_test", ""),
		"shared_webhook":    valid("sk_test_example", "whsec_same", "whsec_same", "ca_test", ""),
		"missing_connect":   valid("sk_test_example", "whsec_cash", "whsec_connect", "", ""),
		"payout_export":     valid("sk_test_example", "whsec_cash", "whsec_connect", "ca_test", "/tmp/export"),
		"invalid_mode_flag": validateCanaryMoneyMode("maybe", "", "", "", "", ""),
	} {
		if err == nil {
			t.Fatalf("%s unsafe configuration accepted", name)
		}
	}
}

func TestCanaryMoneyModeStagingWithoutConnectAllowsTestKeyOnly(t *testing.T) {
	t.Setenv("MERC_ENV", "staging")
	t.Setenv("MERC_CONNECT_RETURN_URL", "")
	t.Setenv("MERC_CONNECT_REFRESH_URL", "")
	if err := validateCanaryMoneyMode("true", "sk_test_staging", "", "", "", ""); err != nil {
		t.Fatalf("staging canary without Connect was refused: %v", err)
	}
	if err := validateCanaryMoneyMode("true", "sk_test_staging", "whsec_billing", "", "", ""); err != nil {
		t.Fatalf("staging canary with only a billing webhook was refused: %v", err)
	}
	if err := validateCanaryMoneyMode("true", "sk_live_nope", "", "", "", ""); err == nil {
		t.Fatal("staging canary accepted a live Stripe key")
	}
	if err := validateCanaryMoneyMode("true", "sk_test_staging", "not-a-webhook", "", "", ""); err == nil {
		t.Fatal("staging canary accepted a non-whsec billing webhook")
	}
	t.Setenv("MERC_CONNECT_RETURN_URL", "https://mercmerc.net/supplier?connect=return")
	if err := validateCanaryMoneyMode("true", "sk_test_staging", "whsec_billing", "whsec_connect", "", ""); err == nil {
		t.Fatal("staging canary with Connect URLs accepted a missing ca_*")
	}
}

// The staging-without-Connect exception may drop the ca_* platform-ID
// requirement. It may not drop secret distinctness: the Connect webhook route
// is served regardless of onboarding, so equal secrets make a cash-signed event
// verify at the Connect authority. Regression for a defect found live on the
// staging plane, where both variables held the same whsec_.
func TestStagingWithoutConnectStillRequiresDistinctWebhookSecrets(t *testing.T) {
	t.Setenv("MERC_ENV", "staging")
	t.Setenv("MERC_CONNECT_RETURN_URL", "")
	t.Setenv("MERC_CONNECT_REFRESH_URL", "")

	same := "whsec_identical_secret_for_both_authorities"
	if err := validateCanaryMoneyMode("true", "sk_test_x", same, same, "", ""); err == nil {
		t.Fatal("staging with identical cash and Connect webhook secrets must be refused")
	}

	if err := validateCanaryMoneyMode(
		"true", "sk_test_x", "whsec_cash_authority", "whsec_connect_authority", "", "",
	); err != nil {
		t.Fatalf("staging with distinct whsec_ secrets and no Connect must boot: %v", err)
	}

	// The exception itself still holds: no Connect secret at all is fine.
	if err := validateCanaryMoneyMode("true", "sk_test_x", "whsec_cash_authority", "", "", ""); err != nil {
		t.Fatalf("staging without Connect onboarding must not demand a Connect secret: %v", err)
	}
}
