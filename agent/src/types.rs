use serde::{Deserialize, Serialize};
use uuid::Uuid;

#[derive(Debug, Clone, Copy, PartialEq, Eq, Serialize, Deserialize)]
#[serde(rename_all = "snake_case")]
pub enum HardwareClass {
    AppleSiliconBase,
    AppleSiliconPro,
    AppleSiliconMax,
    AppleSiliconUltra,
    Cpu,
}

impl HardwareClass {
    pub const fn as_wire_str(self) -> &'static str {
        match self {
            Self::AppleSiliconBase => "apple_silicon_base",
            Self::AppleSiliconPro => "apple_silicon_pro",
            Self::AppleSiliconMax => "apple_silicon_max",
            Self::AppleSiliconUltra => "apple_silicon_ultra",
            Self::Cpu => "cpu",
        }
    }
}

#[derive(Debug, Clone, Copy, PartialEq, Eq, Serialize, Deserialize)]
#[serde(rename_all = "snake_case")]
pub enum ServiceTier {
    Batch,
    Priority,
    Trusted,
}

#[derive(Debug, Clone, Serialize, Deserialize)]
#[serde(tag = "type", rename_all = "snake_case")]
pub enum JobType {
    Embed {
        #[serde(default)]
        batch_size: usize,
        #[serde(default)]
        binary: bool,
    },
    BatchInfer {
        #[serde(default)]
        max_tokens: u32,
        #[serde(default)]
        temperature: f32,
    },
    // A fixed, local video-normalisation operation. This is deliberately not a
    // free-form command surface: the runner accepts only a bounded source
    // format and output geometry and invokes a pinned local FFmpeg binary with
    // a constant argument template.
    MediaTranscode {
        input_format: String,
        max_width: u32,
        max_height: u32,
        fps: u32,
        video_bitrate_kbps: u32,
    },
    /// Deterministic bounded vector-scene rasterisation. This is deliberately
    /// not image generation: the buyer supplies a closed scene document and
    /// the builtin renderer produces one byte-exact PPM artifact.
    MediaRendering {
        #[serde(rename = "render_width")]
        width: u32,
        #[serde(rename = "render_height")]
        height: u32,
    },
}

impl JobType {
    pub fn tag(&self) -> &'static str {
        match self {
            JobType::Embed { .. } => "embed",
            JobType::BatchInfer { .. } => "batch_infer",
            JobType::MediaTranscode { .. } => "media_transcode",
            JobType::MediaRendering { .. } => "media_rendering",
        }
    }
}

#[derive(Debug, Clone, Serialize, Deserialize)]
pub struct ModelRef {
    pub kind: ModelKind,
    #[serde(rename = "ref")]
    pub model_ref: String,
}

#[derive(Debug, Clone, Copy, PartialEq, Eq, Serialize, Deserialize)]
#[serde(rename_all = "snake_case")]
pub enum ModelKind {
    Gguf,
    Hf,
    Builtin,
}

impl ModelKind {
    pub const fn as_wire_str(self) -> &'static str {
        match self {
            Self::Gguf => "gguf",
            Self::Hf => "hf",
            Self::Builtin => "builtin",
        }
    }
}

#[derive(Debug, Clone, Serialize, Deserialize)]
pub struct InputRef {
    pub url: String,
    pub bytes: u64,
}

#[derive(Debug, Clone, Serialize, Deserialize)]
pub struct OutputRef {
    pub url: String,
}

#[derive(Debug, Clone, Serialize, Deserialize)]
pub struct JobConstraints {
    pub min_memory_gb: f32,
    pub hw_classes: Option<Vec<HardwareClass>>,
    pub max_duration_secs: u32,
    pub data_residency: Option<Vec<String>>,
}

#[derive(Debug, Clone, Serialize, Deserialize)]
pub struct VerificationPolicy {
    pub redundancy_frac: f32,
    pub honeypot_frac: f32,
    pub payout_hold_secs: u32,
}

#[derive(Debug, Clone, Serialize, Deserialize)]
pub struct JobManifest {
    pub id: Uuid,
    pub job_type: JobType,
    pub model: ModelRef,
    pub inputs: Vec<InputRef>,
    pub output: OutputRef,
    pub params: serde_json::Value,
    pub constraints: JobConstraints,
    pub verification: VerificationPolicy,
    pub tier: ServiceTier,
}

#[derive(Debug, Clone, Serialize, Deserialize)]
pub struct BenchResult {
    pub model_id: String,
    pub job_type: String,
    // tokens/sec for generation and media_work_units/sec for media_transcode;
    // the job type and runtime authority bind the unit before admission.
    pub tps: f32,
    pub eps: f32,
    pub p99_ms: u32,
    pub thermal_ok: bool,
    #[serde(default)]
    pub load_ms: u64,
}

fn default_engine() -> String {
    "candle".to_string()
}

fn default_build_hash() -> String {
    String::new()
}

#[derive(Debug, Clone, Serialize, Deserialize)]
pub struct WorkerCapability {
    pub worker_id: Uuid,
    pub supplier_id: Uuid,
    /// Fresh for each merc-agent process. The control plane persists transitions
    /// so restart rehearsals can prove a process generation changed instead of
    /// trusting a supervisor command's exit status.
    pub agent_session_id: Uuid,
    pub hw_class: HardwareClass,
    #[serde(default = "default_engine")]
    pub engine: String,
    #[serde(default = "default_build_hash")]
    pub build_hash: String,
    pub memory_gb: f32,
    pub memory_bw_gbps: f32,
    #[serde(default)]
    pub gpu_count: u32,
    #[serde(default)]
    pub memory_gb_per_gpu: f32,
    #[serde(default)]
    pub interconnect: String,
    pub supported_jobs: Vec<String>,
    pub supported_models: Vec<String>,
    pub benchmarks: Vec<BenchResult>,
    pub agent_version: String,
    pub os_version: String,
    #[serde(default)]
    pub min_payout_usd_hr: f32,
    /// True when this process is running under the macOS seatbelt profile.
    /// The control plane uses this to refuse private work to uncontained workers.
    #[serde(default)]
    pub sandboxed: bool,
    /// True when the operator set MERC_ALLOW_UNSANDBOXED=1. Distinct from
    /// sandboxed=false (which also covers non-macOS and missing profiles): this
    /// flag is the deliberate opt-in the control plane greps for.
    #[serde(default)]
    pub unsandboxed_opt_in: bool,
}

#[derive(Debug, Clone, Serialize, Deserialize)]
pub struct RealtimeOfferRegistration {
    pub runtime_profile_id: String,
    pub runtime_profile_sha256: String,
    pub hw_class: String,
    pub gpu_count: u32,
    pub memory_gb_per_gpu: f64,
    pub memory_gb_in_use: f64,
    #[serde(default, skip_serializing_if = "String::is_empty")]
    pub interconnect: String,
    pub upstream_base_url: String,
    pub upstream_token: String,
    pub warmth: String,
    pub max_active_sequences: u32,
    pub available_sequences: u32,
    pub supplier_input_usd_per_million_tokens: f64,
    pub supplier_output_usd_per_million_tokens: f64,
}

#[derive(Debug, Clone, Serialize, Deserialize)]
pub struct RealtimeOfferHeartbeat {
    pub runtime_profile_id: String,
    pub warmth: String,
    pub available_sequences: u32,
    pub status: String,
}

#[derive(Debug, Clone, Serialize, Deserialize)]
pub struct ServiceLeaseOfferRegistration {
    pub runtime_profile_id: String,
    pub runtime_profile_sha256: String,
    pub region: String,
    pub maximum_warm_replicas: u32,
    pub available_warm_replicas: u32,
    pub supplier_nanos_per_replica_hour: i64,
    pub residency_nanos_per_replica_hour: i64,
    pub supports_rolling_upgrade: bool,
    pub p95_latency_milliseconds: i64,
    pub latency_measurement_count: u32,
    pub latency_window_seconds: i64,
    pub latency_measurement_kind: String,
    pub status: String,
}

// This worker-local view intentionally has no buyer, pricing, prompt, or
// payment fields. The agent needs only the operational SLO it was assigned.
#[derive(Debug, Clone, Serialize, Deserialize)]
pub struct ServiceLeaseAssignment {
    pub id: Uuid,
    pub runtime_profile_id: String,
    pub region: String,
    pub minimum_replicas: u32,
    pub maximum_replicas: u32,
    pub maximum_p95_latency_milliseconds: i64,
    pub state: String,
    #[serde(default)]
    pub upgrade_generation: String,
}

#[derive(Debug, Clone, Serialize, Deserialize)]
pub struct ServiceLeaseHeartbeat {
    pub warm_replicas: u32,
    pub p95_latency_milliseconds: i64,
    pub latency_measurement_count: u32,
    pub latency_window_seconds: i64,
    pub latency_measurement_kind: String,
    #[serde(skip_serializing_if = "String::is_empty")]
    pub data_plane_probe_receipt_sha256: String,
    pub status: String,
    #[serde(skip_serializing_if = "String::is_empty")]
    pub upgrade_generation: String,
}

#[derive(Debug, Clone, Serialize, Deserialize)]
pub struct TaskDispatch {
    pub task_id: Uuid,
    #[serde(default)]
    pub attempt: i16,
    pub job_id: Uuid,
    #[serde(default)]
    pub runtime_cell_id: String,
    #[serde(default)]
    pub runtime_id: String,
    #[serde(default)]
    pub runtime_matrix_sha256: String,
    pub manifest: JobManifest,
    pub input_url: String,
    pub output_url: String,
    #[serde(default)]
    pub result_key: String,
    #[serde(default)]
    pub partial_put_url: Option<String>,
    pub deadline: u64,
    #[serde(default)]
    pub offered_rate_usd_hr: f32,
}

#[derive(Debug, Clone, Serialize, Deserialize)]
pub struct TaskCommit {
    pub task_id: Uuid,
    pub attempt: i16,
    pub result_key: String,
    pub duration_ms: u64,
    pub tokens_used: u64,
    #[serde(default)]
    pub result_sha256: String,
    pub hardware_temp_c: Option<f32>,
    /// Pluggable batch_infer backend that produced this result (`candle`, `openai_http`).
    /// Empty for embed / non-inference jobs. A receipt that cannot say what executed
    /// it is not evidence.
    #[serde(default, skip_serializing_if = "String::is_empty")]
    pub inference_backend: String,
    /// Engine-reported prefix-cache hit size for this task, when the engine
    /// exposed it (`usage.prompt_tokens_details.cached_tokens`).
    ///
    /// Omitted means "no signal", and the control plane must not read that as a
    /// miss. Present-and-zero is an observed miss, which is what lets
    /// CorrectPrefixBeliefFromObservation contradict a stale warm belief with
    /// what the engine just did rather than waiting out a TTL.
    #[serde(default, skip_serializing_if = "Option::is_none")]
    pub cached_prompt_tokens: Option<u64>,
}

/// Per-model residency measurement carried on the worker heartbeat. These are
/// the numbers the agent recorded when it loaded the weights (`rss_delta_bytes`
/// and `load_ms`); the control plane range-checks them before they can affect
/// warm-capacity economics.
#[derive(Debug, Clone, Serialize, Deserialize)]
pub struct ResidentModel {
    pub model_id: String,
    pub rss_delta_bytes: i64,
    pub load_ms: u64,
}

#[derive(Debug, Clone, Serialize, Deserialize)]
pub struct Heartbeat {
    pub worker_id: Uuid,
    pub timestamp: u64,
    pub cpu_pct: f32,
    pub gpu_pct: f32,
    pub gpu_temp_c: Option<f32>,
    pub current_task: Option<Uuid>,
    #[serde(default)]
    pub active_tasks: Vec<TaskLease>,
    #[serde(default)]
    pub available_memory_gb: f32,
    #[serde(default)]
    pub effective_memory_gb: f32,
    #[serde(default)]
    pub reserved_headroom_gb: f32,
    #[serde(default)]
    pub throttled: bool,
    #[serde(default)]
    pub loaded_models: Vec<String>,
    /// Measured residency for each currently warm model. Supersedes bare
    /// `loaded_models` for economic decisions once the control plane has it.
    #[serde(default, skip_serializing_if = "Vec::is_empty")]
    pub resident_models: Vec<ResidentModel>,
    /// Models dropped since the previous heartbeat. Control deletes the
    /// corresponding `worker_model_state` row immediately so a supplier is not
    /// treated as warm (and later, paid for residency) after a silent eviction.
    #[serde(default, skip_serializing_if = "Vec::is_empty")]
    pub evicted_models: Vec<String>,
}

#[derive(Debug, Clone, Copy, Serialize, Deserialize)]
pub struct TaskLease {
    pub task_id: Uuid,
    pub attempt: i16,
}

#[derive(Debug, Clone, Serialize, Deserialize)]
pub struct FailMemory {
    pub total_gb: f32,
    pub available_gb: f32,
    pub effective_gb: f32,
    pub reserved_headroom_gb: f32,
}

#[derive(Debug, Clone, Serialize, Deserialize)]
pub struct FailReport {
    pub class: String,
    pub message: String,
    pub duration_ms: u64,
    pub backend: String,
    pub model: String,
    pub memory: Option<FailMemory>,
}

/// One exclusive hold bucket from WorkerEarnings.HeldByReason.
#[derive(Debug, Clone, Serialize, Deserialize)]
pub struct EarningsHoldReason {
    pub reason: String,
    pub amount_usd: f64,
    #[serde(default)]
    pub entry_count: i64,
    #[serde(default)]
    pub earliest_release_at: Option<i64>,
    #[serde(default)]
    pub detail: String,
}

/// Mirrors control/types.go Earnings. Historical `_usd` field names are ledger
/// scale labels, not a USD-only authority — see `currency`.
#[derive(Debug, Clone, Serialize, Deserialize)]
pub struct Earnings {
    #[serde(default)]
    pub currency: String,
    pub balance_usd: f64,
    pub lifetime_usd: f64,
    #[serde(default)]
    pub carried_usd: f64,
    #[serde(default)]
    pub held_usd: f64,
    #[serde(default)]
    pub held_by_reason: Vec<EarningsHoldReason>,
    #[serde(default)]
    pub manual_payout_gate: bool,
    #[serde(default)]
    pub manual_payout_gate_note: String,
    #[serde(default)]
    pub last_payout_usd: Option<f64>,
    #[serde(default)]
    pub last_payout_at: Option<u64>,
    #[serde(default)]
    pub next_payout_at: Option<u64>,
}

/// One supplier-visible ledger credit / clawback row.
#[derive(Debug, Clone, Serialize, Deserialize)]
pub struct PayoutLedgerEntry {
    pub id: Uuid,
    pub kind: String,
    pub amount_usd: f64,
    #[serde(default)]
    pub currency: String,
    pub payout_status: String,
    #[serde(default)]
    pub task_id: Option<Uuid>,
    #[serde(default)]
    pub job_id: Option<Uuid>,
    #[serde(default)]
    pub release_at: Option<i64>,
    pub created_at: String,
}

#[derive(Debug, Clone, Serialize, Deserialize)]
pub struct PayoutLedger {
    #[serde(default)]
    pub currency: String,
    #[serde(default)]
    pub entries: Vec<PayoutLedgerEntry>,
}

#[derive(Debug, Clone, Serialize, Deserialize)]
pub struct SupplierVerification {
    pub honeypots_passed: i64,
    pub honeypots_failed: i64,
    pub verification_label: String,
}

#[derive(Debug, Clone, Serialize, Deserialize)]
pub struct ConnectStatus {
    pub configured: bool,
    pub connected: bool,
    #[serde(rename = "payouts_enabled")]
    pub enabled: bool,
}
