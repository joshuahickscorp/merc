package main

import (
	"encoding/hex"
	"fmt"
	"os"
	"strconv"
	"strings"

	"github.com/google/uuid"
)

// CanaryPolicy is an enforced release envelope, not a marketing description.
// It is inactive outside explicitly configured canary deployments.
type CanaryPolicy struct {
	Enabled                bool
	ApprovedBuyerEmails    map[string]struct{}
	ApprovedWorkerIDs      map[uuid.UUID]struct{}
	ApprovedAgentVersions  map[string]struct{}
	ApprovedBuildHashes    map[string]struct{}
	MaxActiveBuyers        int
	MaxActiveWorkers       int
	MaxQueuedJobs          int
	MaxTasksPerJob         int
	MaxArtifactBytes       int64
	MaxInputBytes          int64
	MaxOutputTokens        uint32
	MaxJobDurationSecs     uint32
	MaxRetries             int
	MaxDailyJobs           int
	MaxShadowValueUSD      float64
	MaxHeldShadowPayoutUSD float64
	configError            error
}

// canaryDisableDecisionEnv is required when MERC_CANARY_MODE is explicitly false.
// Disabling canary also opens self-serve signup; one recorded decision gates both.
const canaryDisableDecisionEnv = "MERC_CANARY_DISABLE_DECISION_REF"

func loadCanaryPolicyFromEnv() CanaryPolicy {
	raw := strings.TrimSpace(os.Getenv("MERC_CANARY_MODE"))
	if raw == "" {
		// Unset is off for local tooling. Production compose defaults the env to
		// true; turning it off requires an explicit false plus a decision ref.
		return CanaryPolicy{}
	}
	enabled, err := strconv.ParseBool(raw)
	if err != nil {
		return CanaryPolicy{Enabled: true, configError: fmt.Errorf("MERC_CANARY_MODE: %w", err)}
	}
	if !enabled {
		decision := strings.TrimSpace(os.Getenv(canaryDisableDecisionEnv))
		if decision == "" {
			// Fail closed: keep canary enforcement on until the disable decision
			// is recorded. Self-serve signup stays gated by the same flag.
			return CanaryPolicy{
				Enabled: true,
				configError: fmt.Errorf(
					"MERC_CANARY_MODE=false requires %s (recorded decision that also opens self-serve signup)",
					canaryDisableDecisionEnv,
				),
			}
		}
		return CanaryPolicy{}
	}

	p := CanaryPolicy{
		Enabled:               true,
		ApprovedBuyerEmails:   make(map[string]struct{}),
		ApprovedWorkerIDs:     make(map[uuid.UUID]struct{}),
		ApprovedAgentVersions: make(map[string]struct{}),
		ApprovedBuildHashes:   make(map[string]struct{}),
	}
	for _, email := range splitCanaryCSV(os.Getenv("MERC_CANARY_APPROVED_BUYER_EMAILS")) {
		email = normalizeEmail(email)
		if email != "" {
			p.ApprovedBuyerEmails[email] = struct{}{}
		}
	}
	for _, raw := range splitCanaryCSV(os.Getenv("MERC_CANARY_APPROVED_WORKER_IDS")) {
		id, parseErr := uuid.Parse(raw)
		if parseErr != nil {
			p.configError = fmt.Errorf("MERC_CANARY_APPROVED_WORKER_IDS contains %q: %w", raw, parseErr)
			return p
		}
		p.ApprovedWorkerIDs[id] = struct{}{}
	}
	for _, version := range splitCanaryCSV(os.Getenv("MERC_CANARY_APPROVED_AGENT_VERSIONS")) {
		if err := validateWorkerTextField("approved agent version", version, maxWorkerVersionBytes); err != nil {
			p.configError = fmt.Errorf("MERC_CANARY_APPROVED_AGENT_VERSIONS contains an invalid value: %w", err)
			return p
		}
		p.ApprovedAgentVersions[version] = struct{}{}
	}
	for _, buildHash := range splitCanaryCSV(os.Getenv("MERC_CANARY_APPROVED_BUILD_HASHES")) {
		if len(buildHash) != 16 {
			p.configError = fmt.Errorf("MERC_CANARY_APPROVED_BUILD_HASHES contains %q: expected the agent's 16-character lowercase hex build hash", buildHash)
			return p
		}
		if _, err := hex.DecodeString(buildHash); err != nil || strings.ToLower(buildHash) != buildHash {
			p.configError = fmt.Errorf("MERC_CANARY_APPROVED_BUILD_HASHES contains %q: expected lowercase hex", buildHash)
			return p
		}
		p.ApprovedBuildHashes[buildHash] = struct{}{}
	}

	var fieldErr error
	p.MaxActiveBuyers, fieldErr = requiredPositiveInt("MERC_CANARY_MAX_ACTIVE_BUYERS")
	if fieldErr == nil {
		p.MaxActiveWorkers, fieldErr = requiredPositiveInt("MERC_CANARY_MAX_ACTIVE_WORKERS")
	}
	if fieldErr == nil {
		p.MaxQueuedJobs, fieldErr = requiredPositiveInt("MERC_CANARY_MAX_QUEUED_JOBS")
	}
	if fieldErr == nil {
		p.MaxTasksPerJob, fieldErr = requiredPositiveInt("MERC_CANARY_MAX_TASKS_PER_JOB")
	}
	if fieldErr == nil {
		p.MaxArtifactBytes, fieldErr = requiredPositiveInt64("MERC_CANARY_MAX_ARTIFACT_BYTES")
	}
	if fieldErr == nil {
		p.MaxInputBytes, fieldErr = requiredPositiveInt64("MERC_CANARY_MAX_INPUT_BYTES")
	}
	if fieldErr == nil {
		v, err := requiredPositiveInt("MERC_CANARY_MAX_OUTPUT_TOKENS")
		fieldErr = err
		p.MaxOutputTokens = uint32(v)
	}
	if fieldErr == nil {
		v, err := requiredPositiveInt("MERC_CANARY_MAX_JOB_DURATION_SECS")
		fieldErr = err
		p.MaxJobDurationSecs = uint32(v)
	}
	if fieldErr == nil {
		p.MaxRetries, fieldErr = requiredPositiveInt("MERC_CANARY_MAX_RETRIES")
	}
	if fieldErr == nil {
		p.MaxDailyJobs, fieldErr = requiredPositiveInt("MERC_CANARY_MAX_DAILY_JOBS")
	}
	if fieldErr == nil {
		p.MaxShadowValueUSD, fieldErr = requiredPositiveFloat("MERC_CANARY_MAX_SHADOW_VALUE_USD")
	}
	if fieldErr == nil {
		p.MaxHeldShadowPayoutUSD, fieldErr = requiredPositiveFloat("MERC_CANARY_MAX_HELD_SHADOW_PAYOUT_USD")
	}
	if fieldErr != nil {
		p.configError = fieldErr
	} else if len(p.ApprovedBuyerEmails) == 0 {
		p.configError = fmt.Errorf("MERC_CANARY_APPROVED_BUYER_EMAILS must contain at least one approved participant")
	} else if len(p.ApprovedWorkerIDs) == 0 {
		p.configError = fmt.Errorf("MERC_CANARY_APPROVED_WORKER_IDS must contain at least one approved worker")
	} else if len(p.ApprovedAgentVersions) == 0 {
		p.configError = fmt.Errorf("MERC_CANARY_APPROVED_AGENT_VERSIONS must contain at least one reviewed agent version")
	} else if len(p.ApprovedBuildHashes) == 0 {
		p.configError = fmt.Errorf("MERC_CANARY_APPROVED_BUILD_HASHES must contain at least one reviewed source-bound build hash")
	} else if p.MaxActiveBuyers > len(p.ApprovedBuyerEmails) {
		p.configError = fmt.Errorf("MERC_CANARY_MAX_ACTIVE_BUYERS exceeds the buyer allowlist")
	} else if p.MaxActiveWorkers > len(p.ApprovedWorkerIDs) {
		p.configError = fmt.Errorf("MERC_CANARY_MAX_ACTIVE_WORKERS exceeds the worker allowlist")
	} else if p.MaxRetries > maxTaskRetries {
		p.configError = fmt.Errorf("MERC_CANARY_MAX_RETRIES cannot exceed the server safety ceiling %d", maxTaskRetries)
	}
	return p
}

func splitCanaryCSV(raw string) []string {
	var out []string
	for _, value := range strings.Split(raw, ",") {
		if value = strings.TrimSpace(value); value != "" {
			out = append(out, value)
		}
	}
	return out
}

func requiredPositiveInt(name string) (int, error) {
	v, err := strconv.Atoi(strings.TrimSpace(os.Getenv(name)))
	if err != nil || v <= 0 {
		return 0, fmt.Errorf("%s must be a positive integer", name)
	}
	return v, nil
}

func requiredPositiveInt64(name string) (int64, error) {
	v, err := strconv.ParseInt(strings.TrimSpace(os.Getenv(name)), 10, 64)
	if err != nil || v <= 0 {
		return 0, fmt.Errorf("%s must be a positive integer", name)
	}
	return v, nil
}

func requiredPositiveFloat(name string) (float64, error) {
	v, err := strconv.ParseFloat(strings.TrimSpace(os.Getenv(name)), 64)
	if err != nil || v <= 0 {
		return 0, fmt.Errorf("%s must be a positive number", name)
	}
	return v, nil
}

func (p CanaryPolicy) allowsBuyerEmail(email string) bool {
	if !p.Enabled || p.configError != nil {
		return !p.Enabled
	}
	_, ok := p.ApprovedBuyerEmails[normalizeEmail(email)]
	return ok
}

func (p CanaryPolicy) allowsWorker(workerID uuid.UUID) bool {
	if !p.Enabled || p.configError != nil {
		return !p.Enabled
	}
	_, ok := p.ApprovedWorkerIDs[workerID]
	return ok
}

func (p CanaryPolicy) allowsWorkerRuntime(cap WorkerCapability) bool {
	if !p.Enabled || p.configError != nil {
		return !p.Enabled
	}
	_, versionAllowed := p.ApprovedAgentVersions[cap.AgentVersion]
	_, buildAllowed := p.ApprovedBuildHashes[cap.BuildHash]
	return versionAllowed && buildAllowed
}

func (p CanaryPolicy) validateJobShape(sub jobSubmit) error {
	if !p.Enabled {
		return nil
	}
	if p.configError != nil {
		return p.configError
	}
	if sub.JobType.Type == "batch_infer" {
		if sub.JobType.Temperature != 0 {
			return fmt.Errorf("temperature must be zero in the deterministic canary")
		}
		if sub.JobType.MaxTokens == 0 || sub.JobType.MaxTokens > p.MaxOutputTokens {
			return fmt.Errorf("max_tokens must be 1..%d in the canary", p.MaxOutputTokens)
		}
	}
	if sub.Constraints.MaxDurationSecs == 0 || sub.Constraints.MaxDurationSecs > p.MaxJobDurationSecs {
		return fmt.Errorf("max_duration_secs must be 1..%d in the canary", p.MaxJobDurationSecs)
	}
	if sub.MaxUSD <= 0 || sub.MaxUSD > p.MaxShadowValueUSD {
		return fmt.Errorf("max_usd must be positive and no greater than %.6f in the canary", p.MaxShadowValueUSD)
	}
	return nil
}

// defaultMaxJobDurationSecs is the unconditional platform ceiling for a job's
// max_duration_secs (canary may be stricter). Four hours bounds wall-clock
// exposure on a supplier Mac; override with MERC_MAX_JOB_DURATION_SECS.
const defaultMaxJobDurationSecs uint32 = 4 * 60 * 60

func maxJobDurationSecsFromEnv() uint32 {
	raw := strings.TrimSpace(os.Getenv("MERC_MAX_JOB_DURATION_SECS"))
	if raw == "" {
		return defaultMaxJobDurationSecs
	}
	v, err := strconv.ParseUint(raw, 10, 32)
	if err != nil || v == 0 {
		return defaultMaxJobDurationSecs
	}
	return uint32(v)
}

// clampMaxDurationSecs enforces the platform ceiling outside (and under) canary.
// 0 ("runner default" / unbounded) becomes the ceiling so a task always has a
// finite wall-clock budget on the supplier device.
func clampMaxDurationSecs(secs uint32) uint32 {
	ceiling := maxJobDurationSecsFromEnv()
	if secs == 0 || secs > ceiling {
		return ceiling
	}
	return secs
}

func canaryArtifactLimit(computed int64) (int64, error) {
	p := loadCanaryPolicyFromEnv()
	if !p.Enabled {
		return computed, nil
	}
	if p.configError != nil {
		return 0, p.configError
	}
	if computed <= 0 || p.MaxArtifactBytes < computed {
		return p.MaxArtifactBytes, nil
	}
	return computed, nil
}

func canaryRetryLimit() (int, error) {
	p := loadCanaryPolicyFromEnv()
	if !p.Enabled {
		return maxTaskRetries, nil
	}
	if p.configError != nil {
		return 0, p.configError
	}
	return p.MaxRetries, nil
}

func canaryManualPayoutGate() (bool, error) {
	p := loadCanaryPolicyFromEnv()
	if !p.Enabled {
		return false, nil
	}
	if p.configError != nil {
		return true, p.configError
	}
	return true, nil
}
