package main

import (
	"encoding/json"

	"github.com/google/uuid"
)

// Hardware classes the control plane will register.
//
// Apple Silicon was the only admitted supply until 2026-07-27. That single fact
// is what made the realtime lane unsellable and got it deleted by [KILL-RT]: an
// NVIDIA worker was refused at registration with HTTP 400, so a latency SLA had
// no hardware to run on. RunPod supply reverses that premise, so the CUDA
// classes below are admitted and the lane is buildable again.
//
// Each class must also appear in sustainedWattsByHWClass, or supplier viability
// silently falls back to a default wattage and the economics report lies.
var validHWClasses = map[string]bool{
	"apple_silicon_base": true, "apple_silicon_pro": true,
	"apple_silicon_max": true, "apple_silicon_ultra": true,
	// CUDA. Named by capability tier rather than by exact SKU so a supplier
	// swapping one 24GB card for another does not need a catalogue change.
	"nvidia_24gb": true, "nvidia_48gb": true,
	"nvidia_80gb": true, "nvidia_180gb": true,
}

var validTiers = map[string]bool{"batch": true, "priority": true, "trusted": true}

// Engines the control plane will accept from a worker registration.
//
// `candle` is the in-process Apple Silicon path. `vllm` fronts a pinned,
// digest-addressed vLLM server on CUDA hardware. The engine is server-authorised
// at registration: a worker cannot self-declare an engine the matrix does not
// carry for its hardware class.
// validEngines is derived from the engine registry rather than hand-listed.
//
// It was {candle, vllm}, which silently excluded llama_cpp and mlx: a worker of
// either was refused at /v1/worker/register with "invalid engine" long before any
// governed profile check ran, so an engine could be fully registered in the
// authority, have a profile, cells and a benchmark receipt, and still be
// unenrollable because of a literal in a different file. The authority is the
// authority; a second hand-maintained list of engines is exactly the drift the
// registry exists to remove.
var validEngines = derivedValidEngines()

func derivedValidEngines() map[string]bool {
	out := make(map[string]bool, len(runtimeAuthority.Engines))
	for _, engine := range runtimeAuthority.Engines {
		out[engine.Engine] = true
	}
	return out
}

// cudaHWClasses is the subset that may run the vllm engine. Keeping this
// explicit stops an Apple worker from claiming vllm and a CUDA worker from
// claiming candle, either of which would route work to a runtime that cannot
// serve it.
var cudaHWClasses = map[string]bool{
	"nvidia_24gb": true, "nvidia_48gb": true,
	"nvidia_80gb": true, "nvidia_180gb": true,
}

// EngineAdmissibleFor reports whether an engine may run on a hardware class.
func EngineAdmissibleFor(engine, hwClass string) bool {
	if !validEngines[engine] || !validHWClasses[hwClass] {
		return false
	}
	if engine == "vllm" {
		return cudaHWClasses[hwClass]
	}
	// candle is Apple Silicon only; CUDA hosts must use vllm.
	return !cudaHWClasses[hwClass]
}

const defaultEngine = "candle"

func normalizeEngine(engine string) string {
	if engine == "" {
		return defaultEngine
	}
	return engine
}

var validJobTypes = map[string]bool{"embed": true, "batch_infer": true, "media_transcode": true, "media_rendering": true}

type JobType struct {
	Type             string  `json:"type"`
	BatchSize        int     `json:"batch_size,omitempty"`
	EmbedBinary      bool    `json:"binary,omitempty"`
	MaxTokens        uint32  `json:"max_tokens,omitempty"`
	Temperature      float32 `json:"temperature,omitempty"`
	InputFormat      string  `json:"input_format,omitempty"`
	MaxWidth         uint32  `json:"max_width,omitempty"`
	MaxHeight        uint32  `json:"max_height,omitempty"`
	FPS              uint32  `json:"fps,omitempty"`
	VideoBitrateKbps uint32  `json:"video_bitrate_kbps,omitempty"`
	RenderWidth      uint32  `json:"render_width,omitempty"`
	RenderHeight     uint32  `json:"render_height,omitempty"`
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
	RedundancyFrac float32 `json:"redundancy_frac"`
	HoneypotFrac   float32 `json:"honeypot_frac"`
	PayoutHoldSecs uint32  `json:"payout_hold_secs"`
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
	WorkerID       uuid.UUID  `json:"worker_id"`
	SupplierID     uuid.UUID  `json:"supplier_id"`
	AgentSessionID *uuid.UUID `json:"agent_session_id,omitempty"`
	HWClass        string     `json:"hw_class"`
	Engine         string     `json:"engine,omitempty"`
	BuildHash      string     `json:"build_hash,omitempty"`
	MemoryGB       float32    `json:"memory_gb"`
	MemoryBwGbps   float32    `json:"memory_bw_gbps"`
	// Single-host topology. Absent means one GPU: an agent that predates these
	// fields is a single-GPU host as far as admission is concerned, which is
	// what it was before the fields existed. A worker claiming more than one
	// GPU must also declare its interconnect -- see validateHostTopology, which
	// refuses to guess.
	GPUCount        int           `json:"gpu_count,omitempty"`
	MemoryGBPerGPU  float32       `json:"memory_gb_per_gpu,omitempty"`
	Interconnect    string        `json:"interconnect,omitempty"`
	SupportedJobs   []string      `json:"supported_jobs"`
	SupportedModels []string      `json:"supported_models"`
	MinPayoutUsdHr  float32       `json:"min_payout_usd_hr"`
	Benchmarks      []BenchResult `json:"benchmarks"`
	AgentVersion    string        `json:"agent_version"`
	OSVersion       string        `json:"os_version"`
}

type TaskDispatch struct {
	TaskID           uuid.UUID   `json:"task_id"`
	Attempt          int16       `json:"attempt"`
	JobID            uuid.UUID   `json:"job_id"`
	RuntimeCellID    string      `json:"runtime_cell_id"`
	RuntimeID        string      `json:"runtime_id"`
	RuntimeMatrixSHA string      `json:"runtime_matrix_sha256"`
	Manifest         JobManifest `json:"manifest"`
	InputURL         string      `json:"input_url"`
	OutputURL        string      `json:"output_url"`
	PartialPutURL    string      `json:"partial_put_url,omitempty"` // presigned PUT for result_key+".partial" (checkpointable job types only)
	ResultKey        string      `json:"result_key"`                // canonical result object key (agent echoes it)
	// Compatibility name: this is the modeled supplier ask admission ceiling
	// in USD/hour. Settlement remains frozen per accepted task.
	OfferedRateUsdHr float32 `json:"offered_rate_usd_hr"`
	Deadline         uint64  `json:"deadline"`
}

type TaskCommit struct {
	TaskID        uuid.UUID `json:"task_id"`
	Attempt       int16     `json:"attempt"`
	ResultKey     string    `json:"result_key"`
	DurationMS    uint64    `json:"duration_ms"`
	TokensUsed    uint64    `json:"tokens_used"`
	ResultSHA256  string    `json:"result_sha256,omitempty"`
	HardwareTempC *float32  `json:"hardware_temp_c"`
	// InferenceBackend records which pluggable batch_infer path produced the
	// result (candle | openai_http). Empty for embed / legacy agents.
	InferenceBackend string `json:"inference_backend,omitempty"`
}

type Heartbeat struct {
	WorkerID           uuid.UUID   `json:"worker_id"`
	Timestamp          uint64      `json:"timestamp"`
	CPUPct             float32     `json:"cpu_pct"`
	GPUPct             float32     `json:"gpu_pct"`
	GPUTempC           *float32    `json:"gpu_temp_c"`
	CurrentTask        *uuid.UUID  `json:"current_task"`
	ActiveTasks        []TaskLease `json:"active_tasks,omitempty"`
	AvailableMemoryGB  float32     `json:"available_memory_gb"`
	EffectiveMemoryGB  float32     `json:"effective_memory_gb"`
	ReservedHeadroomGB float32     `json:"reserved_headroom_gb"`
	Throttled          bool        `json:"throttled"`
	LoadedModels       []string    `json:"loaded_models,omitempty"` // model ids warm in the agent's pool (warm-routing re-rank)
}

// TaskLease identifies the exact execution epoch an agent is still running.
// The retry counter is an attempt fence: an old process cannot renew a task
// after the control plane has requeued and reassigned it.
type TaskLease struct {
	TaskID  uuid.UUID `json:"task_id"`
	Attempt int16     `json:"attempt"`
}

// EarningsHoldReason is one bucket of supplier money that is earned but not
// yet cash-out. Reasons are exclusive per credit (priority order in
// WorkerEarnings): dispute_freeze > verification > manual_gate > hold_window
// > awaiting_funding > in_flight.
type EarningsHoldReason struct {
	// Reason is a greppable token the supplier (and support) can match to policy.
	Reason string `json:"reason"`
	// AmountUSD is the sum of supplier_credit rows in this bucket.
	AmountUSD float64 `json:"amount_usd"`
	// EntryCount is how many ledger credits contribute to AmountUSD.
	EntryCount int `json:"entry_count"`
	// EarliestReleaseAt is the earliest wall-clock moment any credit in this
	// bucket could become eligible under its non-manual gates (unix seconds).
	// Nil when the gate is not time-based (dispute, verification, manual).
	EarliestReleaseAt *int64 `json:"earliest_release_at,omitempty"`
	// Detail is a plain-language statement of why the money is held.
	Detail string `json:"detail"`
}

type Earnings struct {
	// Currency governs every major-unit field below. The historical USD field
	// names describe the ledger's scale, not a USD-only settlement authority.
	Currency    string  `json:"currency"`
	BalanceUSD  float64 `json:"balance_usd"`
	LifetimeUSD float64 `json:"lifetime_usd"`
	CarriedUSD  float64 `json:"carried_usd"` // exact sub-cent remainder still owed, never reported as cash
	// HeldUSD is the sum of supplier credits that are not yet cash (held, ready,
	// awaiting_funding, sending, outcome_unknown). Reconcile as approximately
	// lifetime − balance − carried when every credit is still positive.
	HeldUSD float64 `json:"held_usd"`
	// HeldByReason breaks HeldUSD into exclusive why-buckets so a supplier does
	// not have to reverse-engineer the 24h hold vs a dispute freeze vs canary.
	HeldByReason []EarningsHoldReason `json:"held_by_reason"`
	// ManualPayoutGate is true when canary policy requires an operator to POST
	// /admin/payouts/{ledger_entry_id}/release before any held credit can leave.
	// NextPayoutAt in that mode is eligibility only, not a promise of cash.
	ManualPayoutGate     bool     `json:"manual_payout_gate"`
	ManualPayoutGateNote string   `json:"manual_payout_gate_note,omitempty"`
	LastPayoutUSD        *float64 `json:"last_payout_usd,omitempty"`
	LastPayoutAt         *int64   `json:"last_payout_at,omitempty"` // unix seconds
	// NextPayoutAt is the earliest release_at among held credits that are not
	// dispute-frozen and have a durable verification pass. Under a manual gate
	// this is eligibility only; cash still needs the operator release.
	NextPayoutAt *int64 `json:"next_payout_at,omitempty"` // unix seconds
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
	IdempotentReplay    bool      `json:"-"`
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
	// Set when payload objects have passed retention and been removed. Without
	// it a caller sees an empty results_url and cannot tell "not ready yet"
	// from "deleted", and would poll forever for bytes that no longer exist.
	ResultsExpired bool   `json:"results_expired,omitempty"`
	RetentionNote  string `json:"retention_note,omitempty"`
}

type ModelInfo struct {
	ID            string  `json:"id"`
	Kind          string  `json:"kind"`
	MinMemoryGB   float32 `json:"min_memory_gb"`
	PricePer1K    float64 `json:"price_per_1k"`
	Currency      string  `json:"currency"`
	PricePer1KUSD float64 `json:"price_per_1k_usd,omitempty"`
	JobType       string  `json:"job_type"`
}

type PriceEstimate struct {
	Model         string  `json:"model"`
	Units         uint64  `json:"units"`
	PricePer1K    float64 `json:"price_per_1k"`
	Estimate      float64 `json:"estimate"`
	Currency      string  `json:"currency"`
	PricePer1KUSD float64 `json:"price_per_1k_usd,omitempty"`
	EstimateUSD   float64 `json:"estimate_usd,omitempty"`
	Tier          string  `json:"tier"`
}

// APIError is the machine-readable buyer/supplier error envelope.
// Error stays human prose; Code is a closed enum (see api_error.go).
type APIError struct {
	Error  string       `json:"error"`
	Code   APIErrorCode `json:"code"`
	Action BuyerAction  `json:"action,omitempty"`
}
