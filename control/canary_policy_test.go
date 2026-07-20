package main

import (
	"testing"

	"github.com/google/uuid"
)

func setValidCanaryEnv(t *testing.T, workerID uuid.UUID) {
	t.Helper()
	t.Setenv("CX_CANARY_MODE", "true")
	t.Setenv("CX_CANARY_APPROVED_BUYER_EMAILS", "buyer@example.test, second@example.test")
	t.Setenv("CX_CANARY_APPROVED_WORKER_IDS", workerID.String()+","+uuid.NewString())
	t.Setenv("CX_CANARY_APPROVED_AGENT_VERSIONS", "0.1.0")
	t.Setenv("CX_CANARY_APPROVED_BUILD_HASHES", "0123456789abcdef")
	t.Setenv("CX_CANARY_MAX_ACTIVE_BUYERS", "2")
	t.Setenv("CX_CANARY_MAX_ACTIVE_WORKERS", "2")
	t.Setenv("CX_CANARY_MAX_QUEUED_JOBS", "20")
	t.Setenv("CX_CANARY_MAX_TASKS_PER_JOB", "64")
	t.Setenv("CX_CANARY_MAX_ARTIFACT_BYTES", "33554432")
	t.Setenv("CX_CANARY_MAX_INPUT_BYTES", "33554432")
	t.Setenv("CX_CANARY_MAX_OUTPUT_TOKENS", "256")
	t.Setenv("CX_CANARY_MAX_JOB_DURATION_SECS", "600")
	t.Setenv("CX_CANARY_MAX_RETRIES", "3")
	t.Setenv("CX_CANARY_MAX_DAILY_JOBS", "100")
	t.Setenv("CX_CANARY_MAX_SHADOW_VALUE_USD", "10")
	t.Setenv("CX_CANARY_MAX_HELD_SHADOW_PAYOUT_USD", "10")
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
	approvedRuntime := WorkerCapability{AgentVersion: "0.1.0", BuildHash: "0123456789abcdef"}
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
	valid := jobSubmit{
		JobType:     JobType{Type: "batch_infer", MaxTokens: 256},
		Constraints: JobConstraints{MaxDurationSecs: 600},
		MaxUSD:      10,
	}
	if err := p.validateJobShape(valid); err != nil {
		t.Fatalf("valid shape: %v", err)
	}
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
			t.Setenv("CX_CANARY_APPROVED_BUILD_HASHES", value)
			if p := loadCanaryPolicyFromEnv(); p.configError == nil {
				t.Fatal("malformed build hash allowlist was accepted")
			}
		})
	}
}

func TestCanaryPolicyRequiresExplicitAllowlistAndLimits(t *testing.T) {
	t.Setenv("CX_CANARY_MODE", "true")
	p := loadCanaryPolicyFromEnv()
	if p.configError == nil || p.allowsBuyerEmail("any@example.test") || p.allowsWorker(uuid.New()) {
		t.Fatal("incomplete canary policy did not fail closed")
	}
}

func TestCanaryMoneyModeRefusesLiveAndAmbiguousRails(t *testing.T) {
	valid := func(key, cash, connect, client, payoutExport string) error {
		return validateCanaryMoneyMode("true", key, cash, connect, client, payoutExport)
	}
	if err := valid("sk_test_example", "whsec_cash", "whsec_connect", "ca_test", ""); err != nil {
		t.Fatalf("valid test-mode configuration rejected: %v", err)
	}
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
