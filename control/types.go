package main

import (
	"encoding/json"

	"github.com/google/uuid"
)

// types.go — wire contract (the project "horizon"). These structs mirror the
// Rust agent's serde types in agent/src/types.rs EXACTLY: snake_case JSON,
// snake_case string enums, and a tagged JobType. Do not let the representation
// drift from the agent side; this is the single source of truth on Go's side.

// --- string-enum value domains (validated, not separate Go types) ---
//
// hw_class:  apple_silicon_base | apple_silicon_pro | apple_silicon_max |
//            apple_silicon_ultra | cpu
// tier:      batch | priority | trusted
// job type:  embed | batch_infer | audio_transcribe | image_gen | eval | lora_finetune |
//            batch_classification | json_extraction | rerank | custom
// task status: queued | running | complete | failed | retrying
// job status:  queued | running | verifying | complete | failed | cancelled

// validHWClasses is the closed set of hardware classes (matches HardwareClass).
// apple_silicon_cluster (Plane B, docs/PLANE_B.md) is a co-located Mac cluster
// advertising SUMMED member memory as one high-memory worker; the existing claim
// filter routes summed-memory jobs to it with no scheduler change.
var validHWClasses = map[string]bool{
	"apple_silicon_base": true, "apple_silicon_pro": true,
	"apple_silicon_max": true, "apple_silicon_ultra": true,
	"apple_silicon_cluster": true,
	// NVIDIA/CUDA lane, VRAM-tiered. A DISTINCT family from Apple so within-class
	// verification never compares results across architectures (FP kernels differ).
	"nvidia_24g": true, "nvidia_48g": true,
	"nvidia_80g": true, "nvidia_180g": true,
	"cpu": true,
}

// validTiers is the closed set of service tiers.
var validTiers = map[string]bool{"batch": true, "priority": true, "trusted": true}

// validEngines is the closed set of on-device inference ENGINES a worker may
// advertise (WorkerCapability.Engine). The engine is the SECOND axis of the
// verification class alongside hw_class: byte-exact redundancy peers and
// honeypots are drawn from the same (hw_class, engine), because two engines'
// floating-point kernels differ even on identical hardware, so a future
// mlx/vllm/hawking worker must never be byte-compared against a Candle one.
//   - candle  — the wired default every existing worker advertises (Metal+CUDA)
//   - mlx     — the Apple MLX serving-lane seam (agent/src/runners.rs MlxRunner)
//   - vllm    — the NVIDIA/CUDA vLLM serving lane (audit Wave 2)
//   - hawking — the founder's Apple-Silicon continuous-batch engine (audit Wave 2)
//
// An empty/absent engine normalizes to "candle" at registration, so an older
// agent that does not send the field is unchanged.
var validEngines = map[string]bool{
	"candle": true, "mlx": true, "vllm": true, "hawking": true,
}

// defaultEngine is the engine an absent/blank advertisement normalizes to: the
// wired Candle path every existing worker runs. Keeping a single-engine Candle
// fleet on this default means the (hw_class, engine) class collapses back to
// hw_class alone — exactly today's behavior.
const defaultEngine = "candle"

// normalizeEngine maps a blank/absent advertised engine to defaultEngine (the wired
// Candle path every older agent runs), leaving any non-blank value untouched for the
// caller's validEngines check. Centralizing the "blank → candle" rule keeps the
// (hw_class, engine) class collapse to today's behavior in exactly one place.
func normalizeEngine(engine string) string {
	if engine == "" {
		return defaultEngine
	}
	return engine
}

// validJobTypes is the closed set of job-type tags. The three Turbo workloads
// (batch_classification | json_extraction | rerank) join the original set; each
// has a real result verifier (see verification.go resultsAgree). `custom` is the
// general-compute LANE (ACCRETION.md §7-8): an opaque BYO-container job for the
// metered NVIDIA GPU-second market. It is a VALID contract variant a buyer can
// submit; it has no output verifier (arbitrary compute has no known answer) so it is
// metered per GPU-second + reputation-trusted, and the agent runs it on the GPU in a
// locked-down sandbox (agent/src/sandbox.rs) — an incapable worker errors honestly,
// never a fake result.
var validJobTypes = map[string]bool{
	"embed": true, "batch_infer": true, "audio_transcribe": true,
	"image_gen": true, "eval": true, "lora_finetune": true,
	"batch_classification": true, "json_extraction": true, "rerank": true,
	"custom": true,
}

// JobType is the tagged job descriptor. The wire form is the serde-tagged enum
// the Rust side emits: {"type":"embed","batch_size":64},
// {"type":"batch_infer","max_tokens":512,"temperature":0.0}, etc. omitempty
// keeps the irrelevant variant fields off the wire so the shape matches.
type JobType struct {
	Type      string `json:"type"`
	BatchSize int    `json:"batch_size,omitempty"`
	// EmbedBinary (embed only): opt-in compact float32 output (PLANE_D §11 D5 /
	// §21 D15). When true the agent emits a binary embedding artifact instead of
	// the JSON `vectors` array. omitempty keeps it off the wire for every other
	// job type and for JSON-default embed jobs; a zero-value (false) decodes
	// against an older agent that never sends it. Persisted in job_type_spec so it
	// round-trips to the agent on dispatch (manifest.params does not).
	EmbedBinary     bool            `json:"binary,omitempty"`
	MaxTokens       uint32          `json:"max_tokens,omitempty"`
	Temperature     float32         `json:"temperature,omitempty"`
	Language        *string         `json:"language,omitempty"`
	Timestamps      bool            `json:"timestamps,omitempty"`
	Resolution      [2]uint32       `json:"resolution,omitempty"`
	Steps           uint32          `json:"steps,omitempty"`
	Rubric          json.RawMessage `json:"rubric,omitempty"`
	Epochs          uint32          `json:"epochs,omitempty"`
	Lr              float32         `json:"lr,omitempty"`
	CheckpointEvery uint32          `json:"checkpoint_every,omitempty"`
	// Turbo workload params (match the Rust agent's matching side):
	//   batch_classification → Labels (the closed label set the model picks from),
	//   json_extraction      → Schema (the JSON schema each item must conform to),
	//   rerank               → TopK   (cut the ranking to the top-K documents).
	Labels []string        `json:"labels,omitempty"`
	Schema json.RawMessage `json:"schema,omitempty"`
	TopK   uint32          `json:"top_k,omitempty"`
	// General-compute SEAM (custom only; ACCRETION.md §7-8). An opaque
	// bring-your-own-container job for the metered NVIDIA GPU-second lane:
	//   Image   → OCI container image ref the job runs in (nullable: a pointer so a
	//             null on the wire round-trips, matching the agent's Option<String>),
	//   Command → argv the sandbox executes inside the image (empty = the image's
	//             own entrypoint).
	// omitempty keeps both off the wire for every other job type. The control plane
	// has no output verifier for custom (arbitrary compute has no known answer), so the
	// lane is metered per GPU-second + reputation-trusted; the agent runs the container
	// on the GPU in a locked-down sandbox (agent/src/sandbox.rs).
	Image   *string  `json:"image,omitempty"`
	Command []string `json:"command,omitempty"`
}

// ModelRef references a model. Wire: {"kind":"gguf"|"hf"|"mlx","ref":"..."}.
type ModelRef struct {
	Kind string `json:"kind"`
	Ref  string `json:"ref"`
}

// InputRef is one input object: a URL plus its size in bytes.
type InputRef struct {
	URL   string `json:"url"`
	Bytes uint64 `json:"bytes"`
}

// OutputRef is where the merged result is written.
type OutputRef struct {
	URL string `json:"url"`
}

// JobConstraints narrows which workers may run a job.
type JobConstraints struct {
	MinMemoryGB     float32  `json:"min_memory_gb"`
	HWClasses       []string `json:"hw_classes"` // null = any
	MaxDurationSecs uint32   `json:"max_duration_secs"`
	DataResidency   []string `json:"data_residency"` // null = unrestricted
}

// VerificationPolicy controls honeypot/redundancy rates and payout hold.
type VerificationPolicy struct {
	RedundancyFrac float32 `json:"redundancy_frac"`
	HoneypotFrac   float32 `json:"honeypot_frac"`
	PayoutHoldSecs uint32  `json:"payout_hold_secs"`
	// SkipVerificationFloor explicitly opts a job out of the server-side minimum
	// combined honeypot+redundancy coverage (Verification & Result Trust 6->7,
	// docs/internal/CREED_AND_PATH_TO_TEN.md). Without this, redundancy_frac=0
	// AND honeypot_frac=0 meant a job could run with ZERO real verification —
	// a buyer and a colluding/careless supplier had no anti-fraud check on that
	// job's result at all. Default false (the floor applies); an explicit,
	// auditable true is required to run genuinely unverified, never a silent
	// side-effect of just leaving both fractions unset.
	SkipVerificationFloor bool `json:"skip_verification_floor,omitempty"`
}

// JobManifest is the full job description submitted by a buyer.
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

// BenchResult is one benchmark line for a (model, job_type) pair.
type BenchResult struct {
	ModelID   string  `json:"model_id"`
	JobType   string  `json:"job_type"`
	TPS       float32 `json:"tps"` // tokens/sec
	EPS       float32 `json:"eps"` // embeddings/sec
	P99MS     uint32  `json:"p99_ms"`
	ThermalOK bool    `json:"thermal_ok"`
	// LoadMS is the wall-clock milliseconds this model's COLD load took, measured
	// by the agent at its own unavoidably-cold first load during the startup
	// benchmark (docs/CREED_AND_PATH_TO_TEN.md, "Warm model pool" 6.5->7). Zero
	// from an older agent build that doesn't send it yet — never fabricated.
	LoadMS uint64 `json:"load_ms"`
}

// WorkerCapability is what a worker advertises on registration. MinPayoutUsdHr is
// the operator's reservation price ($/hr): the claim's hard filter excludes any
// job whose offered_rate_usd_hr is below it, so a worker never runs work that
// pays under its floor (mirrors the Rust agent's matching side).
type WorkerCapability struct {
	WorkerID   uuid.UUID `json:"worker_id"`
	SupplierID uuid.UUID `json:"supplier_id"`
	HWClass    string    `json:"hw_class"`
	// Engine is the on-device inference engine this worker runs (validEngines).
	// It is the SECOND axis of the verification class: the redundancy matcher and
	// honeypot seeding pin byte-exact peers to the same (hw_class, engine). The
	// agent sends "candle" by default; an absent value (an older agent) normalizes
	// to defaultEngine at registration, so a single-engine fleet is unchanged.
	Engine string `json:"engine,omitempty"`
	// BuildHash is the FINER axis of the verification class BELOW (hw_class, engine):
	// a stable hash of the byte-output-determining build inputs (engine + agent build +
	// device backend + catalogue quant — agent hardware::engine_build_hash). Byte-exact
	// redundancy peers and honeypots are pinned to the same (hw_class, engine,
	// build_hash), because a kernel/codegen change between agent builds can shift bytes
	// even on identical hardware running the same engine. An absent/blank value (an
	// older agent) is "unknown build" — never drawn as a byte-exact peer and never
	// auto-docked on a pure byte mismatch (provisional trust), so a single-build fleet
	// reporting the same hash collapses the class to today's behavior.
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

// TaskDispatch is handed to a worker in response to a poll. result_key is the
// canonical server-side object key the worker PUTs its result to (and echoes in
// the commit); output_url is that same key presigned for upload. The Rust agent
// reads result_key (serde default) and uploads to output_url. partial_put_url is
// the OPTIONAL intra-task checkpoint seam (shared wire contract): result_key +
// ".partial" presigned for upload, sent only for checkpointable job types — the
// agent MAY periodically PUT a partial result document there (final-result JSON
// shape plus a top-level "partial": true marker); the final commit is unchanged,
// and old agents ignore the field.
type TaskDispatch struct {
	TaskID uuid.UUID `json:"task_id"`
	JobID  uuid.UUID `json:"job_id"`
	// Runtime authority is selected by ClaimTask from the server-generated exact
	// capability projection and frozen on the task attempt. The agent verifies all
	// three values before executing, so a stale/malformed control-plane dispatch
	// cannot silently fall through to a merely compatible local runner.
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

// TaskCommit is the worker's result submission.
type TaskCommit struct {
	TaskID     uuid.UUID `json:"task_id"`
	ResultKey  string    `json:"result_key"`
	DurationMS uint64    `json:"duration_ms"`
	TokensUsed uint64    `json:"tokens_used"`
	// ResultSHA256 is the worker's own SHA-256 (lowercase hex) of the exact bytes
	// it just PUT to ResultKey (Control Plane Hot Path 8->9,
	// docs/internal/CREED_AND_PATH_TO_TEN.md "trust a buyer/worker-supplied
	// SHA-256 for redundancy/honeypot comparison where safe, instead of
	// re-downloading bytes the worker just uploaded synchronously inside the
	// commit transaction"). Persisted verbatim to tasks.result_sha256 so a LATER
	// commit's redundancy compare can trust a hash-to-hash match for byte-exact
	// job types without a second S3 GetObject. Optional: "" (an older agent, or
	// one that failed to hash) always falls back to a real GetObject — the hash
	// is a speed optimization, never a trust requirement the commit can fail on.
	ResultSHA256  string   `json:"result_sha256,omitempty"`
	HardwareTempC *float32 `json:"hardware_temp_c"`
}

// Heartbeat is the periodic liveness + telemetry signal (~30s). The resource
// fields (AvailableMemoryGB … Throttled) are the supplier-throttling delta: the
// agent reports its live effective memory and whether it is throttled, and the
// claim's hard filter uses both so a memory-pressured worker is never dispatched
// work. They are optional on the wire (a pre-throttling agent omits them → zero
// values; EffectiveMemoryGB 0 makes the claim fall back to total memory_gb).
//
// LoadedModels is the warm-routing delta (docs/PLANE_D.md §9 D3): the model ids
// currently WARM in the agent's pool. HeartbeatWorker upserts a worker_model_state
// row per id, and the scheduler gives a small re-rank bonus to a worker that has the
// job's model warm (warm only re-ranks — the claim hard filter is unchanged). It is
// optional on the wire (omitempty / a pre-warm agent omits it → nil), so older peers
// still decode; the agent reports real warm ids only.
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

// Earnings is returned by GET /v1/worker/earnings. LastPayout*/NextPayoutAt are
// Supplier onboarding & safety 7->8 (docs/internal/CREED_AND_PATH_TO_TEN.md,
// "Populate the trust panel with real data"): real payout proof for the menu-bar
// trust panel, sourced from this supplier's own ledger_entries — never fabricated.
// Pointers so an absent value (no released payout yet / no held credit pending)
// round-trips as JSON null, matching the app's optional trust-surface contract.
type Earnings struct {
	BalanceUSD    float64  `json:"balance_usd"`
	LifetimeUSD   float64  `json:"lifetime_usd"`
	CarriedUSD    float64  `json:"carried_usd"` // exact sub-cent remainder still owed, never reported as cash
	LastPayoutUSD *float64 `json:"last_payout_usd,omitempty"`
	LastPayoutAt  *int64   `json:"last_payout_at,omitempty"` // unix seconds
	NextPayoutAt  *int64   `json:"next_payout_at,omitempty"` // unix seconds
}

// SupplierVerification is the per-supplier verification (honeypot) aggregate for
// GET /v1/worker/verification — the trust-panel data source distinct from
// JobVerification (which is per-JOB, buyer-facing). Sourced from this supplier's
// own verification_events rows across every job they have ever worked, so the
// menu-bar app can show a real "N honeypots passed" figure instead of leaving the
// trust panel permanently empty (Supplier onboarding & safety 7->8).
type SupplierVerification struct {
	HoneypotsPassed int    `json:"honeypots_passed"`
	HoneypotsFailed int    `json:"honeypots_failed"`
	Label           string `json:"verification_label"` // reuses deriveVerificationLabel's vocabulary
}

// --- control-plane-local response types (not part of the agent contract) ---

// JobSubmitResponse is the 202 body for POST /v1/jobs. ETASecs is a queue-depth
// + throughput estimate of seconds to completion (also persisted to jobs.eta_secs).
type JobSubmitResponse struct {
	JobID               uuid.UUID `json:"job_id"`
	TaskCount           int       `json:"task_count"`
	EstimatedUSD        float64   `json:"estimated_usd"`
	ETASecs             int       `json:"eta_secs"`
	EstimatedCompletion string    `json:"estimated_completion"` // RFC3339
	TierSemantics       string    `json:"tier_semantics"`
	// Routing is the SUBSTRATE-ROUTING decision (Speed Lane road-to-ten rubric
	// dimension 5, control/routing.go + quote.go): which substrate this job's
	// SHAPE favors — fleet, a lit GPU lane, or an honest GPU recommendation — with
	// the measured basis and the compared numbers stated plainly, MIRRORING the
	// quote's routing block for the same input. Present only for GENERATIVE jobs
	// with records > 0 (the A100 sweep measured generative decode only, so every
	// other shape gets NO block rather than an unmeasured guess). It is a routing
	// STATEMENT, never a refusal: a gpu_recommend job still runs on the fleet at
	// the quoted eta_secs — the eta_secs, pricing, and SLA above are unchanged.
	Routing    *QuoteRouting        `json:"routing,omitempty"`
	AudioInput *AudioUploadMetadata `json:"audio_input,omitempty"`
	// WebhookSecret is returned only when this submission registered a webhook.
	// It is a one-time bearer verifier value; only an AES-GCM sealed copy persists.
	WebhookID     string `json:"webhook_id,omitempty"`
	WebhookSecret string `json:"webhook_secret,omitempty"`
}

// JobStatus is the GET /v1/jobs/{id} body. ETASecs is the submit-time
// queue-depth/throughput estimate persisted at creation (jobs.eta_secs).
type JobStatus struct {
	JobID        uuid.UUID `json:"job_id"`
	Status       string    `json:"status"`
	JobType      string    `json:"job_type"`
	Tier         string    `json:"tier"`
	TaskCount    int       `json:"task_count"`
	TasksDone    int       `json:"tasks_done"`
	EstimatedUSD float64   `json:"estimated_usd"`
	ActualUSD    float64   `json:"actual_usd"`
	ETASecs      int       `json:"eta_secs"`
	CreatedAt    string    `json:"created_at"`
	// MaxUSD is the buyer's hard spend cap (Budget Governor); 0 when none was set.
	// BudgetState is the governor state machine
	// (tracking|near_limit|paused_for_budget|cancelled_by_budget) — the buyer-facing
	// signal that Computexchange STOPS before a cap (Plane C §12 / Plane D §14 D8).
	MaxUSD      float64 `json:"max_usd"`
	BudgetState string  `json:"budget_state"`
	// ChargeStatus is the buyer-facing billing state for this job, mirroring the
	// queryable jobs.charge_status column:
	//   not_attempted | charged | failed | no_payment_method
	// It makes a silent-debt charge failure VISIBLE in the status body (a "failed"
	// or "no_payment_method" here pairs with a charge_failed job event), never hidden.
	ChargeStatus string `json:"charge_status"`
	// Verification is the honest, derived verification receipt for the job, assembled
	// from the append-only verification_events log plus the latest dispute. Counts are
	// real (only outcomes that actually occurred are logged); label is derived, never
	// asserted beyond what the counts support.
	Verification Verification `json:"verification"`
	// Wall-clock speed-SLA (Speed Lane wave 2A). SLAGuaranteeSecs > 0 means this
	// job carries a bound time guarantee (results merged within that many seconds
	// of submission) priced at SLAPremiumUSD. SLAMet is the recorded outcome:
	// absent until decided at finalize (or absent forever without an SLA),
	// true = met, false = missed — a miss auto-recorded an sla_refund ledger
	// credit for the premium (visible on the invoice/receipt) plus an sla_missed
	// timeline event. All three are omitted entirely for a job with no SLA.
	SLAGuaranteeSecs int     `json:"sla_guarantee_secs,omitempty"`
	SLAPremiumUSD    float64 `json:"sla_premium_usd,omitempty"`
	SLAMet           *bool   `json:"sla_met,omitempty"`
}

// Verification is the buyer-facing verification receipt block of JobStatus. Every
// count is sourced from the append-only verification_events log (control/store.go
// JobVerification), so it reflects what actually happened · a skipped/sampled-out
// check is simply absent, never reported as a pass. dispute_status is the latest
// dispute's status for the job (empty when none). label is DERIVED from delivered
// chunk coverage, not merely from the existence of one event:
//   - "fully-verified"    when every delivered primary chunk has an independent check
//   - "sampled-verified"  when some, but not all, delivered chunks were checked
//   - "honeypot-checked"  when only known-answer probes ran
//   - "unverified"        when no delivered chunk was independently checked
//
// Legacy event-only projections with no delivered-task rows may use "verified".
type Verification struct {
	Checked              int `json:"checked"`
	HoneypotsPassed      int `json:"honeypots_passed"`
	HoneypotsFailed      int `json:"honeypots_failed"`
	RedundancyMatched    int `json:"redundancy_matched"`
	RedundancyMismatched int `json:"redundancy_mismatched"`
	Tiebreaks            int `json:"tiebreaks"`
	// SameSupplier counts chunks whose only agreeing peer shared the committing
	// supplier, so the match was NOT counted as independent verification (items 7, 9).
	SameSupplier int `json:"same_supplier_matches"`
	// CrossClassSkipped counts chunks whose peer was in a DIFFERENT verification class,
	// so a byte-exact comparison could not run (a coverage gap, not a defect) (item 9).
	CrossClassSkipped int `json:"cross_class_skipped"`
	// Coverage is per delivered primary chunk, not per event. This prevents one
	// independently checked chunk from labeling an otherwise unchecked job fully
	// verified.
	DeliveredChunks  int    `json:"delivered_chunks"`
	VerifiedChunks   int    `json:"verified_chunks"`
	UnverifiedChunks int    `json:"unverified_chunks"`
	DisputeStatus    string `json:"dispute_status"`
	Label            string `json:"label"`
}

// deriveVerificationLabel returns the honest label for a verification aggregate,
// applying the contract's rule. It is the single place the label is computed so the
// store and any future caller agree.
func deriveVerificationLabel(v Verification) string {
	switch {
	case v.DeliveredChunks > 0 && v.VerifiedChunks >= v.DeliveredChunks:
		return "fully-verified"
	case v.DeliveredChunks > 0 && v.VerifiedChunks > 0:
		return "sampled-verified"
	case v.DeliveredChunks == 0 && (v.RedundancyMatched > 0 || v.Tiebreaks > 0):
		// A real INDEPENDENT cross-check settled at least one chunk (a different
		// supplier's matching result, or a tiebreak vote) in a legacy/event-only
		// projection that predates per-chunk task coverage.
		return "verified"
	case v.Checked > 0:
		// Known-answer (honeypot) probes ran, or a mismatch was detected, but no
		// independent redundancy match — honest middle state.
		return "honeypot-checked"
	case v.SameSupplier > 0:
		// A peer ran but shared the committing supplier, so it was NOT independent
		// (items 7, 9). The work is provisionally accepted, not independently verified.
		return "no-independent-peer"
	case v.CrossClassSkipped > 0:
		// A peer existed but in a different verification class, so a byte-exact
		// comparison could not run (coverage gap, surfaced honestly, never inflated).
		return "cross-class-skip"
	default:
		return "unverified"
	}
}

// JobResults is the GET /v1/jobs/{id}/results body. results_url is a real
// time-limited presigned GET for the merged job output when one exists;
// result_urls is the per-task list of presigned result URLs (the V1 outputs are
// per-task, since the control plane does not merge). Both are real signed URLs
// minted by storage.PresignGet, never a fabricated stub.
type JobResults struct {
	JobID      uuid.UUID `json:"job_id"`
	Status     string    `json:"status"`
	ResultsURL string    `json:"results_url,omitempty"` // presigned merged output (if any)
	ResultURLs []string  `json:"result_urls"`           // presigned per-task results
}

// ModelInfo is one entry in GET /v1/models.
type ModelInfo struct {
	ID                     string  `json:"id"`
	Kind                   string  `json:"kind"`
	MinMemoryGB            float32 `json:"min_memory_gb"`
	PricePer1KUSD          float64 `json:"price_per_1k_usd,omitempty"`
	PricePerAudioMinuteUSD float64 `json:"price_per_audio_minute_usd,omitempty"`
	JobType                string  `json:"job_type"`
}

// PriceEstimate is the GET /v1/price-estimate body.
type PriceEstimate struct {
	Model         string  `json:"model"`
	Units         uint64  `json:"units"`
	PricePer1KUSD float64 `json:"price_per_1k_usd"`
	EstimateUSD   float64 `json:"estimate_usd"`
	Tier          string  `json:"tier"`
}

// APIError is the uniform JSON error body.
type APIError struct {
	Error string `json:"error"`
}
