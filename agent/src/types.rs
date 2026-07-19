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
}

impl JobType {
    pub fn tag(&self) -> &'static str {
        match self {
            JobType::Embed { .. } => "embed",
            JobType::BatchInfer { .. } => "batch_infer",
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
}

impl ModelKind {
    pub const fn as_wire_str(self) -> &'static str {
        match self {
            Self::Gguf => "gguf",
            Self::Hf => "hf",
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
    pub hw_class: HardwareClass,
    #[serde(default = "default_engine")]
    pub engine: String,
    #[serde(default = "default_build_hash")]
    pub build_hash: String,
    pub memory_gb: f32,
    pub memory_bw_gbps: f32,
    pub supported_jobs: Vec<String>,
    pub supported_models: Vec<String>,
    pub benchmarks: Vec<BenchResult>,
    pub agent_version: String,
    pub os_version: String,
    #[serde(default)]
    pub min_payout_usd_hr: f32,
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

#[derive(Debug, Clone, Serialize, Deserialize)]
pub struct Earnings {
    pub balance_usd: f64,
    pub lifetime_usd: f64,
    #[serde(default)]
    pub last_payout_usd: Option<f64>,
    #[serde(default)]
    pub last_payout_at: Option<u64>,
    #[serde(default)]
    pub next_payout_at: Option<u64>,
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
