package main

import (
	"encoding/json"

	"github.com/google/uuid"
)

var validHWClasses = map[string]bool{
	"apple_silicon_base": true, "apple_silicon_pro": true,
	"apple_silicon_max": true, "apple_silicon_ultra": true,
}

var validTiers = map[string]bool{"batch": true, "priority": true, "trusted": true}

var validEngines = map[string]bool{"candle": true}

const defaultEngine = "candle"

func normalizeEngine(engine string) string {
	if engine == "" {
		return defaultEngine
	}
	return engine
}

var validJobTypes = map[string]bool{"embed": true, "batch_infer": true}

type JobType struct {
	Type        string  `json:"type"`
	BatchSize   int     `json:"batch_size,omitempty"`
	EmbedBinary bool    `json:"binary,omitempty"`
	MaxTokens   uint32  `json:"max_tokens,omitempty"`
	Temperature float32 `json:"temperature,omitempty"`
}

type ModelRef struct {
	Kind string `json:"kind"`
	Ref  string `json:"ref"`
}

type InputRef struct {
	URL   string `json:"url"`
	Bytes uint64 `json:"bytes"`
}

type OutputRef struct {
	URL string `json:"url"`
}

type JobConstraints struct {
	MinMemoryGB     float32  `json:"min_memory_gb"`
	HWClasses       []string `json:"hw_classes"` // null = any
	MaxDurationSecs uint32   `json:"max_duration_secs"`
	DataResidency   []string `json:"data_residency"` // null = unrestricted
}

type VerificationPolicy struct {
	RedundancyFrac        float32 `json:"redundancy_frac"`
	HoneypotFrac          float32 `json:"honeypot_frac"`
	PayoutHoldSecs        uint32  `json:"payout_hold_secs"`
	SkipVerificationFloor bool    `json:"skip_verification_floor,omitempty"`
}

type JobManifest struct {
	ID           uuid.UUID          `json:"id"`
	JobType      JobType            `json:"job_type"`
	Model        ModelRef           `json:"model"`
	Inputs       []InputRef         `json:"inputs"`
	Output       OutputRef          `json:"output"`
	Params       json.RawMessage    `json:"params"`
	Constraints  JobConstraints     `json:"constraints"`
	Verification VerificationPolicy `json:"verification"`
	Tier         string             `json:"tier"`
}

type BenchResult struct {
	ModelID   string  `json:"model_id"`
	JobType   string  `json:"job_type"`
	TPS       float32 `json:"tps"` // tokens/sec
	EPS       float32 `json:"eps"` // embeddings/sec
	P99MS     uint32  `json:"p99_ms"`
	ThermalOK bool    `json:"thermal_ok"`
	LoadMS    uint64  `json:"load_ms"`
}

type WorkerCapability struct {
	WorkerID        uuid.UUID     `json:"worker_id"`
	SupplierID      uuid.UUID     `json:"supplier_id"`
	HWClass         string        `json:"hw_class"`
	Engine          string        `json:"engine,omitempty"`
	BuildHash       string        `json:"build_hash,omitempty"`
	MemoryGB        float32       `json:"memory_gb"`
	MemoryBwGbps    float32       `json:"memory_bw_gbps"`
	SupportedJobs   []string      `json:"supported_jobs"`
	SupportedModels []string      `json:"supported_models"`
	MinPayoutUsdHr  float32       `json:"min_payout_usd_hr"`
	Benchmarks      []BenchResult `json:"benchmarks"`
	AgentVersion    string        `json:"agent_version"`
	OSVersion       string        `json:"os_version"`
}

type TaskDispatch struct {
	TaskID           uuid.UUID   `json:"task_id"`
	JobID            uuid.UUID   `json:"job_id"`
	RuntimeCellID    string      `json:"runtime_cell_id"`
	RuntimeID        string      `json:"runtime_id"`
	RuntimeMatrixSHA string      `json:"runtime_matrix_sha256"`
	Manifest         JobManifest `json:"manifest"`
	InputURL         string      `json:"input_url"`
	OutputURL        string      `json:"output_url"`
	PartialPutURL    string      `json:"partial_put_url,omitempty"` // presigned PUT for result_key+".partial" (checkpointable job types only)
	ResultKey        string      `json:"result_key"`                // canonical result object key (agent echoes it)
	OfferedRateUsdHr float32     `json:"offered_rate_usd_hr"`       // $/hr this task pays (matches the worker's min-payout gate)
	Deadline         uint64      `json:"deadline"`
}

type TaskCommit struct {
	TaskID        uuid.UUID `json:"task_id"`
	ResultKey     string    `json:"result_key"`
	DurationMS    uint64    `json:"duration_ms"`
	TokensUsed    uint64    `json:"tokens_used"`
	ResultSHA256  string    `json:"result_sha256,omitempty"`
	HardwareTempC *float32  `json:"hardware_temp_c"`
}

type Heartbeat struct {
	WorkerID           uuid.UUID  `json:"worker_id"`
	Timestamp          uint64     `json:"timestamp"`
	CPUPct             float32    `json:"cpu_pct"`
	GPUPct             float32    `json:"gpu_pct"`
	GPUTempC           *float32   `json:"gpu_temp_c"`
	CurrentTask        *uuid.UUID `json:"current_task"`
	AvailableMemoryGB  float32    `json:"available_memory_gb"`
	EffectiveMemoryGB  float32    `json:"effective_memory_gb"`
	ReservedHeadroomGB float32    `json:"reserved_headroom_gb"`
	Throttled          bool       `json:"throttled"`
	LoadedModels       []string   `json:"loaded_models,omitempty"` // model ids warm in the agent's pool (warm-routing re-rank)
}

type Earnings struct {
	BalanceUSD    float64  `json:"balance_usd"`
	LifetimeUSD   float64  `json:"lifetime_usd"`
	CarriedUSD    float64  `json:"carried_usd"` // exact sub-cent remainder still owed, never reported as cash
	LastPayoutUSD *float64 `json:"last_payout_usd,omitempty"`
	LastPayoutAt  *int64   `json:"last_payout_at,omitempty"` // unix seconds
	NextPayoutAt  *int64   `json:"next_payout_at,omitempty"` // unix seconds
}

type SupplierVerification struct {
	HoneypotsPassed int    `json:"honeypots_passed"`
	HoneypotsFailed int    `json:"honeypots_failed"`
	Label           string `json:"verification_label"` // reuses deriveVerificationLabel's vocabulary
}

type JobSubmitResponse struct {
	JobID               uuid.UUID `json:"job_id"`
	TaskCount           int       `json:"task_count"`
	EstimatedUSD        float64   `json:"estimated_usd"`
	ETASecs             int       `json:"eta_secs"`
	EstimatedCompletion string    `json:"estimated_completion"` // RFC3339
	TierSemantics       string    `json:"tier_semantics"`
	WebhookID           string    `json:"webhook_id,omitempty"`
	WebhookSecret       string    `json:"webhook_secret,omitempty"`
}

type JobStatus struct {
	JobID            uuid.UUID    `json:"job_id"`
	Status           string       `json:"status"`
	JobType          string       `json:"job_type"`
	Tier             string       `json:"tier"`
	TaskCount        int          `json:"task_count"`
	TasksDone        int          `json:"tasks_done"`
	EstimatedUSD     float64      `json:"estimated_usd"`
	ActualUSD        float64      `json:"actual_usd"`
	ETASecs          int          `json:"eta_secs"`
	CreatedAt        string       `json:"created_at"`
	MaxUSD           float64      `json:"max_usd"`
	BudgetState      string       `json:"budget_state"`
	ChargeStatus     string       `json:"charge_status"`
	Verification     Verification `json:"verification"`
	SLAGuaranteeSecs int          `json:"sla_guarantee_secs,omitempty"`
	SLAPremiumUSD    float64      `json:"sla_premium_usd,omitempty"`
	SLAMet           *bool        `json:"sla_met,omitempty"`
}

type Verification struct {
	Checked              int    `json:"checked"`
	HoneypotsPassed      int    `json:"honeypots_passed"`
	HoneypotsFailed      int    `json:"honeypots_failed"`
	RedundancyMatched    int    `json:"redundancy_matched"`
	RedundancyMismatched int    `json:"redundancy_mismatched"`
	Tiebreaks            int    `json:"tiebreaks"`
	SameSupplier         int    `json:"same_supplier_matches"`
	CrossClassSkipped    int    `json:"cross_class_skipped"`
	DeliveredChunks      int    `json:"delivered_chunks"`
	VerifiedChunks       int    `json:"verified_chunks"`
	UnverifiedChunks     int    `json:"unverified_chunks"`
	DisputeStatus        string `json:"dispute_status"`
	Label                string `json:"label"`
}

func deriveVerificationLabel(v Verification) string {
	switch {
	case v.DeliveredChunks > 0 && v.VerifiedChunks >= v.DeliveredChunks:
		return "fully-verified"
	case v.DeliveredChunks > 0 && v.VerifiedChunks > 0:
		return "sampled-verified"
	case v.DeliveredChunks == 0 && (v.RedundancyMatched > 0 || v.Tiebreaks > 0):
		return "verified"
	case v.Checked > 0:
		return "honeypot-checked"
	case v.SameSupplier > 0:
		return "no-independent-peer"
	case v.CrossClassSkipped > 0:
		return "cross-class-skip"
	default:
		return "unverified"
	}
}

type JobResults struct {
	JobID      uuid.UUID `json:"job_id"`
	Status     string    `json:"status"`
	ResultsURL string    `json:"results_url,omitempty"` // presigned merged output (if any)
	ResultURLs []string  `json:"result_urls"`           // presigned per-task results
}

type ModelInfo struct {
	ID            string  `json:"id"`
	Kind          string  `json:"kind"`
	MinMemoryGB   float32 `json:"min_memory_gb"`
	PricePer1KUSD float64 `json:"price_per_1k_usd,omitempty"`
	JobType       string  `json:"job_type"`
}

type PriceEstimate struct {
	Model         string  `json:"model"`
	Units         uint64  `json:"units"`
	PricePer1KUSD float64 `json:"price_per_1k_usd"`
	EstimateUSD   float64 `json:"estimate_usd"`
	Tier          string  `json:"tier"`
}

type APIError struct {
	Error string `json:"error"`
}
