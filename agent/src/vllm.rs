use std::path::{Path, PathBuf};
use std::process::Stdio;
use std::sync::Arc;
use std::time::{Duration, Instant};

use anyhow::{bail, Context, Result};
use base64::Engine;
use rand::RngCore;
use serde::Deserialize;
use sha2::{Digest, Sha256};
use tokio::process::{Child, Command};

use crate::protocol::ControlPlaneClient;
use crate::types::{
    RealtimeOfferHeartbeat, RealtimeOfferRegistration, ServiceLeaseAssignment,
    ServiceLeaseHeartbeat, ServiceLeaseOfferRegistration,
};

const HEALTH_INTERVAL: Duration = Duration::from_millis(500);
const HEARTBEAT_INTERVAL: Duration = Duration::from_secs(15);
const SERVICE_LEASE_MINIMUM_SAMPLES: usize = 5;
const SERVICE_LEASE_MAXIMUM_SAMPLES: usize = 20;
const SERVICE_LEASE_MEASUREMENT_KIND: &str = "DATA_PLANE_COMPLETIONS_V1";

#[derive(Debug, Clone, Deserialize)]
#[serde(deny_unknown_fields)]
pub struct VllmAgentConfig {
    pub control_url: String,
    pub worker_token: String,
    pub runtime_profile_path: PathBuf,
    pub public_base_url: String,
    pub model_cache_dir: PathBuf,
    #[serde(default = "default_container_runtime")]
    pub container_runtime: String,
    #[serde(default = "default_listen_host")]
    pub listen_host: String,
    #[serde(default = "default_listen_port")]
    pub listen_port: u16,
    #[serde(default = "default_max_active_sequences")]
    pub max_active_sequences: u32,
    #[serde(default = "default_startup_timeout_secs")]
    pub startup_timeout_secs: u64,
    pub hw_class: String,
    pub gpu_count: u32,
    pub memory_gb_per_gpu: f64,
    #[serde(default)]
    pub memory_gb_in_use: f64,
    #[serde(default)]
    pub interconnect: String,
    pub supplier_input_usd_per_million_tokens: f64,
    pub supplier_output_usd_per_million_tokens: f64,
    // An omitted section means this adapter cannot advertise reserved service
    // capacity. Opt-in is deliberate because qualifying the data-plane causes
    // bounded real completions against the advertised endpoint.
    #[serde(default)]
    pub service_lease: Option<ServiceLeaseConfig>,
}

#[derive(Debug, Clone, Deserialize)]
#[serde(deny_unknown_fields)]
pub struct ServiceLeaseConfig {
    pub region: String,
    pub maximum_warm_replicas: u32,
    pub available_warm_replicas: u32,
    pub supplier_nanos_per_replica_hour: i64,
    pub residency_nanos_per_replica_hour: i64,
    #[serde(default)]
    pub supports_rolling_upgrade: bool,
    #[serde(default = "default_service_probe_interval_secs")]
    pub probe_interval_secs: u64,
    #[serde(default = "default_service_probe_timeout_millis")]
    pub probe_timeout_millis: u64,
    #[serde(default = "default_service_probe_max_tokens")]
    pub probe_max_tokens: u32,
}

fn default_container_runtime() -> String {
    "docker".to_string()
}

fn default_listen_host() -> String {
    "127.0.0.1".to_string()
}

const fn default_listen_port() -> u16 {
    8000
}

const fn default_max_active_sequences() -> u32 {
    128
}

const fn default_startup_timeout_secs() -> u64 {
    900
}

const fn default_service_probe_interval_secs() -> u64 {
    15
}

const fn default_service_probe_timeout_millis() -> u64 {
    5_000
}

const fn default_service_probe_max_tokens() -> u32 {
    1
}

#[derive(Debug, Clone, Deserialize)]
#[serde(deny_unknown_fields)]
struct GenerationPolicy {
    version: String,
    temperature: f64,
    top_p: f64,
    maximum_output_tokens: u32,
}

#[derive(Debug, Clone, Deserialize)]
#[serde(deny_unknown_fields)]
struct RuntimeProfile {
    schema_version: u32,
    runtime_profile_id: String,
    engine: String,
    engine_version: String,
    container_image: String,
    container_platform: String,
    container_digest: String,
    model_alias: String,
    model_repository: String,
    model_revision: String,
    tokenizer_revision: String,
    architecture: String,
    dtype: String,
    #[serde(default)]
    model_weights_gb: f64,
    #[serde(default)]
    per_rank_overhead_gb: f64,
    #[serde(default)]
    attention_heads: u32,
    tensor_parallel_size: u32,
    pipeline_parallel_size: u32,
    max_model_length: u32,
    gpu_memory_utilization: f64,
    prefix_caching: bool,
    chunked_prefill: bool,
    generation_policy: GenerationPolicy,
    buyer_input_usd_per_million_tokens: f64,
    buyer_output_usd_per_million_tokens: f64,
    benchmark_status: String,
}

impl RuntimeProfile {
    fn validate(&self) -> Result<()> {
        if self.schema_version != 1 || self.engine != "vllm" {
            bail!("runtime profile must be schema 1 for engine vllm")
        }
        if self.runtime_profile_id.trim().is_empty()
            || self.model_alias.trim().is_empty()
            || self.model_repository.trim().is_empty()
            || self.architecture.trim().is_empty()
        {
            bail!("runtime profile identity is incomplete")
        }
        if self.engine_version.trim().is_empty()
            || self.container_image.ends_with(":latest")
            || !is_sha256_digest(&self.container_digest)
        {
            bail!("engine version and container digest must be immutable")
        }
        if self.container_platform != "linux/amd64" {
            bail!("container platform must match the pinned linux/amd64 manifest")
        }
        if !is_commit(&self.model_revision) || !is_commit(&self.tokenizer_revision) {
            bail!("model and tokenizer revisions must be exact 40-character commits")
        }
        if self.tensor_parallel_size == 0
            || self.pipeline_parallel_size == 0
            || self.max_model_length == 0
            || !(0.0..=1.0).contains(&self.gpu_memory_utilization)
            || self.gpu_memory_utilization == 0.0
        {
            bail!("runtime profile resource bounds are invalid")
        }
        let placement_fields = [
            self.model_weights_gb > 0.0,
            self.per_rank_overhead_gb >= 0.0
                && (self.model_weights_gb > 0.0 || self.attention_heads > 0),
            self.attention_heads > 0,
        ];
        let has_placement = self.model_weights_gb > 0.0
            || self.per_rank_overhead_gb > 0.0
            || self.attention_heads > 0;
        if has_placement && !placement_fields.into_iter().all(|present| present) {
            bail!("runtime profile model placement authority is incomplete")
        }
        if self.tensor_parallel_size > 1 && !has_placement {
            bail!("multi-GPU runtime profile requires model placement authority")
        }
        if self.generation_policy.version.trim().is_empty()
            || self.generation_policy.maximum_output_tokens == 0
            || !self.generation_policy.temperature.is_finite()
            || !self.generation_policy.top_p.is_finite()
        {
            bail!("generation policy is invalid")
        }
        if self.buyer_input_usd_per_million_tokens <= 0.0
            || self.buyer_output_usd_per_million_tokens <= 0.0
            || !matches!(self.benchmark_status.as_str(), "UNPROVEN" | "PASSED")
        {
            bail!("runtime profile economics or benchmark state is invalid")
        }
        Ok(())
    }

    fn container_reference(&self) -> String {
        format!("{}@{}", self.container_image, self.container_digest)
    }
}

fn is_commit(value: &str) -> bool {
    value.len() == 40
        && value
            .bytes()
            .all(|b| b.is_ascii_hexdigit() && !b.is_ascii_uppercase())
}

fn is_sha256_digest(value: &str) -> bool {
    value.len() == 71
        && value.starts_with("sha256:")
        && value[7..]
            .bytes()
            .all(|b| b.is_ascii_hexdigit() && !b.is_ascii_uppercase())
}

fn sha256_hex(bytes: &[u8]) -> String {
    Sha256::digest(bytes)
        .iter()
        .map(|b| format!("{b:02x}"))
        .collect()
}

fn load_profile(path: &Path) -> Result<(RuntimeProfile, String)> {
    let raw = std::fs::read(path)
        .with_context(|| format!("reading runtime profile {}", path.display()))?;
    let profile: RuntimeProfile = serde_json::from_slice(&raw)
        .with_context(|| format!("decoding runtime profile {}", path.display()))?;
    profile.validate()?;
    Ok((profile, sha256_hex(&raw)))
}

fn validate_config(config: &VllmAgentConfig, profile: &RuntimeProfile) -> Result<()> {
    if config.control_url.trim().is_empty()
        || config.worker_token.trim().is_empty()
        || config.public_base_url.trim().is_empty()
        || config.container_runtime.trim().is_empty()
    {
        bail!("control_url, worker_token, public_base_url, and container_runtime are required")
    }
    if config.listen_port == 0 || config.max_active_sequences == 0 {
        bail!("listen_port and max_active_sequences must be positive")
    }
    if !matches!(
        config.hw_class.as_str(),
        "nvidia_24gb" | "nvidia_48gb" | "nvidia_80gb" | "nvidia_180gb"
    ) {
        bail!("hw_class must be an admitted NVIDIA capability class")
    }
    if config.gpu_count == 0
        || config.memory_gb_per_gpu <= 0.0
        || !config.memory_gb_per_gpu.is_finite()
        || config.memory_gb_in_use < 0.0
        || !config.memory_gb_in_use.is_finite()
        || config.memory_gb_in_use > config.memory_gb_per_gpu
        || profile.tensor_parallel_size > config.gpu_count
    {
        bail!("declared GPU topology cannot host the runtime profile")
    }
    if config.gpu_count > 1 && !matches!(config.interconnect.as_str(), "nvlink" | "pcie") {
        bail!("multi-GPU hosts must declare interconnect as nvlink or pcie")
    }
    if config.gpu_count == 1
        && !config.interconnect.is_empty()
        && !matches!(config.interconnect.as_str(), "nvlink" | "pcie")
    {
        bail!("interconnect must be empty, nvlink, or pcie")
    }
    if config.supplier_input_usd_per_million_tokens < 0.0
        || config.supplier_output_usd_per_million_tokens < 0.0
        || config.supplier_input_usd_per_million_tokens > profile.buyer_input_usd_per_million_tokens
        || config.supplier_output_usd_per_million_tokens
            > profile.buyer_output_usd_per_million_tokens
    {
        bail!("supplier rates must be non-negative and no greater than buyer rates")
    }
    if let Some(service) = &config.service_lease {
        let region_valid = service.region.len() >= 2
            && service.region.len() <= 64
            && service
                .region
                .bytes()
                .all(|byte| byte.is_ascii_lowercase() || byte.is_ascii_digit() || byte == b'-');
        if !region_valid
            || service.maximum_warm_replicas == 0
            || service.available_warm_replicas == 0
            || service.available_warm_replicas > service.maximum_warm_replicas
            || service.supplier_nanos_per_replica_hour <= 0
            || service.residency_nanos_per_replica_hour <= 0
            || !(5..=300).contains(&service.probe_interval_secs)
            || !(100..=30_000).contains(&service.probe_timeout_millis)
            || !(1..=4).contains(&service.probe_max_tokens)
        {
            bail!("service lease config has invalid capacity, fixed-point floor, or bounded probe settings")
        }
    }
    Ok(())
}

fn generate_upstream_token() -> String {
    let mut bytes = [0_u8; 32];
    rand::thread_rng().fill_bytes(&mut bytes);
    format!(
        "cx_vllm_{}",
        base64::engine::general_purpose::URL_SAFE_NO_PAD.encode(bytes)
    )
}

fn container_args(
    config: &VllmAgentConfig,
    profile: &RuntimeProfile,
    upstream_token: &str,
) -> Vec<String> {
    let selected_devices = (0..profile.tensor_parallel_size)
        .map(|device| device.to_string())
        .collect::<Vec<_>>()
        .join(",");
    let mut args = vec![
        "run".into(),
        "--rm".into(),
        "--gpus".into(),
        format!("device={selected_devices}"),
        "--network".into(),
        "host".into(),
        "--read-only".into(),
        "--cap-drop".into(),
        "ALL".into(),
        "--security-opt".into(),
        "no-new-privileges".into(),
        "--tmpfs".into(),
        "/tmp:rw,nosuid,size=4g".into(),
        "--shm-size".into(),
        "4g".into(),
        "-e".into(),
        "HF_HUB_DISABLE_TELEMETRY=1".into(),
        "-v".into(),
        format!("{}:/root/.cache", config.model_cache_dir.display()),
        profile.container_reference(),
        profile.model_repository.clone(),
        "--revision".into(),
        profile.model_revision.clone(),
        "--tokenizer-revision".into(),
        profile.tokenizer_revision.clone(),
        "--served-model-name".into(),
        profile.model_alias.clone(),
        "--dtype".into(),
        profile.dtype.clone(),
        "--tensor-parallel-size".into(),
        profile.tensor_parallel_size.to_string(),
        "--pipeline-parallel-size".into(),
        profile.pipeline_parallel_size.to_string(),
        "--max-model-len".into(),
        profile.max_model_length.to_string(),
        "--gpu-memory-utilization".into(),
        profile.gpu_memory_utilization.to_string(),
        "--max-num-seqs".into(),
        config.max_active_sequences.to_string(),
        "--generation-config".into(),
        "vllm".into(),
        "--host".into(),
        config.listen_host.clone(),
        "--port".into(),
        config.listen_port.to_string(),
        "--api-key".into(),
        upstream_token.to_string(),
    ];
    if profile.prefix_caching {
        args.push("--enable-prefix-caching".into());
    }
    if profile.chunked_prefill {
        args.push("--enable-chunked-prefill".into());
    }
    args
}

async fn pull_pinned_container(config: &VllmAgentConfig, profile: &RuntimeProfile) -> Result<()> {
    let status = Command::new(&config.container_runtime)
        .arg("pull")
        .arg(profile.container_reference())
        .stdin(Stdio::null())
        .status()
        .await
        .context("launching pinned vLLM container pull")?;
    if !status.success() {
        bail!("pinned vLLM container pull failed with {status}")
    }
    Ok(())
}

fn start_container(
    config: &VllmAgentConfig,
    profile: &RuntimeProfile,
    upstream_token: &str,
) -> Result<Child> {
    Command::new(&config.container_runtime)
        .args(container_args(config, profile, upstream_token))
        .stdin(Stdio::null())
        .stdout(Stdio::inherit())
        .stderr(Stdio::inherit())
        .kill_on_drop(true)
        .spawn()
        .context("launching pinned vLLM container")
}

async fn wait_until_healthy(
    child: &mut Child,
    config: &VllmAgentConfig,
    upstream_token: &str,
) -> Result<()> {
    let client = reqwest::Client::builder()
        .connect_timeout(Duration::from_secs(2))
        .timeout(Duration::from_secs(5))
        .build()
        .context("building local vLLM health client")?;
    let health_url = format!("http://127.0.0.1:{}/health", config.listen_port);
    let deadline = Instant::now() + Duration::from_secs(config.startup_timeout_secs);
    loop {
        if let Some(status) = child.try_wait().context("checking vLLM container")? {
            bail!("vLLM container exited before readiness with {status}")
        }
        if Instant::now() >= deadline {
            bail!("vLLM did not become healthy before the startup timeout")
        }
        if let Ok(response) = client
            .get(&health_url)
            .bearer_auth(upstream_token)
            .send()
            .await
        {
            if response.status().is_success() {
                return Ok(());
            }
        }
        tokio::time::sleep(HEALTH_INTERVAL).await;
    }
}

#[derive(Debug, Deserialize)]
struct VllmProbeResponse {
    choices: Vec<serde_json::Value>,
}

// A health endpoint only proves that a process answered HTTP. Reserved-service
// SLOs instead use a fixed, tiny completion through the exact public endpoint
// registered with Merc. The bounded body, max_tokens, timeout, and cadence are
// all operator configuration, never buyer input.
async fn measure_public_data_plane(
    config: &VllmAgentConfig,
    profile: &RuntimeProfile,
    upstream_token: &str,
    service: &ServiceLeaseConfig,
) -> Result<i64> {
    let client = crate::tls::client_builder()?
        .connect_timeout(Duration::from_secs(2))
        .timeout(Duration::from_millis(service.probe_timeout_millis))
        .build()
        .context("building service lease data-plane probe client")?;
    let endpoint = format!(
        "{}/completions",
        config.public_base_url.trim_end_matches('/')
    );
    let started = Instant::now();
    let response = client
        .post(&endpoint)
        .bearer_auth(upstream_token)
        .json(&serde_json::json!({
            "model": profile.model_alias,
            "prompt": "Merc reserved-service probe. Reply READY.",
            "max_tokens": service.probe_max_tokens,
            "temperature": 0,
        }))
        .send()
        .await
        .with_context(|| format!("calling bounded service lease probe {endpoint}"))?;
    if !response.status().is_success() {
        bail!(
            "service lease data-plane probe returned {}",
            response.status()
        )
    }
    let response = response
        .json::<VllmProbeResponse>()
        .await
        .context("decoding service lease data-plane completion")?;
    if response.choices.is_empty() {
        bail!("service lease data-plane probe returned no completion choices")
    }
    let milliseconds = started.elapsed().as_millis().max(1);
    i64::try_from(milliseconds).context("service lease probe latency overflow")
}

#[derive(Debug)]
struct ServiceLatencySamples {
    started_at: Instant,
    samples_millis: Vec<i64>,
}

impl ServiceLatencySamples {
    fn new() -> Self {
        Self {
            started_at: Instant::now(),
            samples_millis: Vec::with_capacity(SERVICE_LEASE_MAXIMUM_SAMPLES),
        }
    }

    fn record(&mut self, latency_millis: i64) -> Result<()> {
        if latency_millis < 1 {
            bail!("service lease latency measurement must be positive")
        }
        if self.samples_millis.len() == SERVICE_LEASE_MAXIMUM_SAMPLES {
            self.samples_millis.remove(0);
        }
        self.samples_millis.push(latency_millis);
        Ok(())
    }

    fn p95_millis(&self) -> Result<i64> {
        if self.samples_millis.len() < SERVICE_LEASE_MINIMUM_SAMPLES {
            bail!("service lease requires five actual data-plane measurements before advertisement")
        }
        let mut sorted = self.samples_millis.clone();
        sorted.sort_unstable();
        let rank = (sorted.len() * 95).div_ceil(100).saturating_sub(1);
        Ok(sorted[rank])
    }

    fn measurement_count(&self) -> u32 {
        self.samples_millis.len() as u32
    }

    fn window_seconds(&self) -> i64 {
        i64::try_from(self.started_at.elapsed().as_secs().max(1)).unwrap_or(i64::MAX)
    }
}

fn service_offer(
    profile: &RuntimeProfile,
    profile_sha256: &str,
    service: &ServiceLeaseConfig,
    samples: &ServiceLatencySamples,
    status: &str,
) -> Result<ServiceLeaseOfferRegistration> {
    let p95_latency_milliseconds = samples.p95_millis()?;
    Ok(ServiceLeaseOfferRegistration {
        runtime_profile_id: profile.runtime_profile_id.clone(),
        runtime_profile_sha256: profile_sha256.to_string(),
        region: service.region.clone(),
        maximum_warm_replicas: service.maximum_warm_replicas,
        available_warm_replicas: service.available_warm_replicas,
        supplier_nanos_per_replica_hour: service.supplier_nanos_per_replica_hour,
        residency_nanos_per_replica_hour: service.residency_nanos_per_replica_hour,
        supports_rolling_upgrade: service.supports_rolling_upgrade,
        p95_latency_milliseconds,
        latency_measurement_count: samples.measurement_count(),
        latency_window_seconds: samples.window_seconds(),
        latency_measurement_kind: SERVICE_LEASE_MEASUREMENT_KIND.to_string(),
        status: status.to_string(),
    })
}

fn lease_matches_runtime(
    lease: &ServiceLeaseAssignment,
    profile: &RuntimeProfile,
    service: &ServiceLeaseConfig,
) -> bool {
    lease.runtime_profile_id == profile.runtime_profile_id && lease.region == service.region
}

async fn report_service_lease_failure(
    client: &ControlPlaneClient,
    profile: &RuntimeProfile,
    service: &ServiceLeaseConfig,
) {
    match client.service_lease_assignments().await {
        Ok(assignments) => {
            for lease in assignments
                .iter()
                .filter(|lease| lease_matches_runtime(lease, profile, service))
            {
                let heartbeat = ServiceLeaseHeartbeat {
                    warm_replicas: 0,
                    p95_latency_milliseconds: 0,
                    latency_measurement_count: 0,
                    latency_window_seconds: 0,
                    latency_measurement_kind: String::new(),
                    status: "FAILED".to_string(),
                    upgrade_generation: String::new(),
                };
                if let Err(error) = client.heartbeat_service_lease(lease.id, &heartbeat).await {
                    tracing::warn!(%error, lease_id = %lease.id, "could not report failed service lease")
                }
            }
        }
        Err(error) => {
            tracing::warn!(%error, "could not read service lease assignments for failure report")
        }
    }
}

async fn heartbeat_service_leases(
    client: &ControlPlaneClient,
    config: &VllmAgentConfig,
    profile: &RuntimeProfile,
    profile_sha256: &str,
    upstream_token: &str,
    service: &ServiceLeaseConfig,
    samples: &mut ServiceLatencySamples,
) -> Result<()> {
    let assignments = client
        .service_lease_assignments()
        .await
        .context("reading service lease assignments")?;
    let assignments: Vec<_> = assignments
        .into_iter()
        .filter(|lease| lease_matches_runtime(lease, profile, service))
        .collect();
    let measurement = measure_public_data_plane(config, profile, upstream_token, service).await;
    match measurement {
        Ok(latency) => samples.record(latency)?,
        Err(error) => {
            tracing::warn!(%error, "service lease data-plane probe failed; failing closed");
            if let Ok(offer) = service_offer(profile, profile_sha256, service, samples, "FAILED") {
                if let Err(error) = client.register_service_lease_offer(&offer).await {
                    tracing::warn!(%error, "could not withdraw failed service lease offer")
                }
            }
            report_service_lease_failure(client, profile, service).await;
            return Ok(());
        }
    }
    client
        .register_service_lease_offer(&service_offer(
            profile,
            profile_sha256,
            service,
            samples,
            "READY",
        )?)
        .await
        .context("refreshing measured service lease offer")?;
    if assignments.is_empty() {
        return Ok(());
    }
    let p95 = samples.p95_millis()?;
    for lease in assignments {
        let status = if p95 <= lease.maximum_p95_latency_milliseconds {
            "READY"
        } else {
            "FAILED"
        };
        let heartbeat = ServiceLeaseHeartbeat {
            warm_replicas: if status == "READY" {
                service.available_warm_replicas
            } else {
                0
            },
            p95_latency_milliseconds: p95,
            latency_measurement_count: samples.measurement_count(),
            latency_window_seconds: samples.window_seconds(),
            latency_measurement_kind: SERVICE_LEASE_MEASUREMENT_KIND.to_string(),
            status: status.to_string(),
            upgrade_generation: String::new(),
        };
        client
            .heartbeat_service_lease(lease.id, &heartbeat)
            .await
            .with_context(|| format!("heartbeating service lease {}", lease.id))?;
    }
    Ok(())
}

async fn report_terminal_state(client: &ControlPlaneClient, profile_id: &str, status: &str) {
    let heartbeat = RealtimeOfferHeartbeat {
        runtime_profile_id: profile_id.to_string(),
        warmth: "HOT".to_string(),
        available_sequences: 0,
        status: status.to_string(),
    };
    if let Err(error) = client.heartbeat_realtime(&heartbeat).await {
        tracing::warn!(%error, %status, "could not report terminal vLLM state");
    }
}

pub async fn run(config_path: PathBuf) -> Result<()> {
    if !cfg!(target_os = "linux") {
        bail!("the production vLLM adapter requires a Linux CUDA host")
    }
    if std::env::consts::ARCH != "x86_64" {
        bail!("this runtime profile pins a linux/amd64 vLLM container manifest")
    }
    let text = std::fs::read_to_string(&config_path)
        .with_context(|| format!("reading vLLM agent config {}", config_path.display()))?;
    let config: VllmAgentConfig = toml::from_str(&text)
        .with_context(|| format!("decoding vLLM agent config {}", config_path.display()))?;
    let (profile, profile_sha256) = load_profile(&config.runtime_profile_path)?;
    validate_config(&config, &profile)?;
    std::fs::create_dir_all(&config.model_cache_dir)
        .with_context(|| format!("creating model cache {}", config.model_cache_dir.display()))?;

    pull_pinned_container(&config, &profile).await?;
    let upstream_token = generate_upstream_token();
    let mut child = start_container(&config, &profile, &upstream_token)?;
    if let Err(error) = wait_until_healthy(&mut child, &config, &upstream_token).await {
        let _ = child.start_kill();
        let _ = child.wait().await;
        return Err(error);
    }

    let client = Arc::new(
        ControlPlaneClient::new(config.control_url.clone(), config.worker_token.clone())
            .context("building vLLM control-plane client")?,
    );
    let mut service_samples = if let Some(service) = &config.service_lease {
        let mut samples = ServiceLatencySamples::new();
        for sample in 0..SERVICE_LEASE_MINIMUM_SAMPLES {
            let latency = measure_public_data_plane(&config, &profile, &upstream_token, service)
                .await
                .context("qualifying reserved service data plane before advertisement")?;
            samples.record(latency)?;
            if sample + 1 < SERVICE_LEASE_MINIMUM_SAMPLES {
                tokio::time::sleep(Duration::from_secs(service.probe_interval_secs)).await;
            }
        }
        client
            .register_service_lease_offer(&service_offer(
                &profile,
                &profile_sha256,
                service,
                &samples,
                "READY",
            )?)
            .await
            .context("registering measured vLLM service lease offer")?;
        Some(samples)
    } else {
        None
    };
    client
        .register_realtime(&RealtimeOfferRegistration {
            runtime_profile_id: profile.runtime_profile_id.clone(),
            runtime_profile_sha256: profile_sha256.clone(),
            hw_class: config.hw_class.clone(),
            gpu_count: config.gpu_count,
            memory_gb_per_gpu: config.memory_gb_per_gpu,
            memory_gb_in_use: config.memory_gb_in_use,
            interconnect: config.interconnect.clone(),
            upstream_base_url: config.public_base_url.clone(),
            upstream_token: upstream_token.clone(),
            warmth: "HOT".to_string(),
            max_active_sequences: config.max_active_sequences,
            available_sequences: config.max_active_sequences,
            supplier_input_usd_per_million_tokens: config.supplier_input_usd_per_million_tokens,
            supplier_output_usd_per_million_tokens: config.supplier_output_usd_per_million_tokens,
        })
        .await
        .context("registering vLLM offer")?;

    tracing::info!(
        runtime_profile_id = %profile.runtime_profile_id,
        model = %profile.model_alias,
        engine_version = %profile.engine_version,
        container_digest = %profile.container_digest,
        "pinned vLLM lane is healthy and registered"
    );
    let mut heartbeat = tokio::time::interval(HEARTBEAT_INTERVAL);
    heartbeat.set_missed_tick_behavior(tokio::time::MissedTickBehavior::Delay);
    let mut service_heartbeat = config.service_lease.as_ref().map(|service| {
        let mut heartbeat = tokio::time::interval(Duration::from_secs(service.probe_interval_secs));
        heartbeat.set_missed_tick_behavior(tokio::time::MissedTickBehavior::Delay);
        heartbeat
    });
    loop {
        tokio::select! {
            status = child.wait() => {
                let status = status.context("waiting for vLLM container")?;
                report_terminal_state(&client, &profile.runtime_profile_id, "FAILED").await;
                if let Some(service) = &config.service_lease {
                    report_service_lease_failure(&client, &profile, service).await;
                }
                bail!("vLLM container exited with {status}");
            }
            _ = tokio::signal::ctrl_c() => {
                report_terminal_state(&client, &profile.runtime_profile_id, "DRAINING").await;
                if let Some(service) = &config.service_lease {
                    report_service_lease_failure(&client, &profile, service).await;
                }
                let _ = child.start_kill();
                let _ = child.wait().await;
                return Ok(());
            }
            _ = heartbeat.tick() => {
                let hb = RealtimeOfferHeartbeat {
                    runtime_profile_id: profile.runtime_profile_id.clone(),
                    warmth: "HOT".to_string(),
                    available_sequences: config.max_active_sequences,
                    status: "ACTIVE".to_string(),
                };
                if let Err(error) = client.heartbeat_realtime(&hb).await {
                    tracing::warn!(%error, "vLLM offer heartbeat failed");
                }
            }
            _ = async {
                if let Some(heartbeat) = &mut service_heartbeat {
                    heartbeat.tick().await;
                } else {
                    std::future::pending::<()>().await;
                }
            } => {
                if let (Some(service), Some(samples)) = (&config.service_lease, &mut service_samples) {
                    if let Err(error) = heartbeat_service_leases(&client, &config, &profile, &profile_sha256, &upstream_token, service, samples).await {
                        tracing::warn!(%error, "vLLM service lease heartbeat failed");
                    }
                }
            }
        }
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    fn profile() -> RuntimeProfile {
        RuntimeProfile {
            schema_version: 1,
            runtime_profile_id: "rtp_test".into(),
            engine: "vllm".into(),
            engine_version: "0.23.0".into(),
            container_image: "docker.io/vllm/vllm-openai:v0.23.0".into(),
            container_platform: "linux/amd64".into(),
            container_digest: format!("sha256:{}", "a".repeat(64)),
            model_alias: "merc-test".into(),
            model_repository: "org/model".into(),
            model_revision: "b".repeat(40),
            tokenizer_revision: "c".repeat(40),
            architecture: "ForCausalLM".into(),
            dtype: "bfloat16".into(),
            model_weights_gb: 60.0,
            per_rank_overhead_gb: 8.0,
            attention_heads: 32,
            tensor_parallel_size: 2,
            pipeline_parallel_size: 1,
            max_model_length: 32768,
            gpu_memory_utilization: 0.9,
            prefix_caching: true,
            chunked_prefill: true,
            generation_policy: GenerationPolicy {
                version: "v1".into(),
                temperature: 0.7,
                top_p: 0.95,
                maximum_output_tokens: 1024,
            },
            buyer_input_usd_per_million_tokens: 0.12,
            buyer_output_usd_per_million_tokens: 0.45,
            benchmark_status: "UNPROVEN".into(),
        }
    }

    fn config() -> VllmAgentConfig {
        VllmAgentConfig {
            control_url: "https://control.example".into(),
            worker_token: "cxw_test".into(),
            runtime_profile_path: PathBuf::from("profile.json"),
            public_base_url: "https://worker.example/v1".into(),
            model_cache_dir: PathBuf::from("/var/lib/cx/models"),
            container_runtime: "docker".into(),
            listen_host: "127.0.0.1".into(),
            listen_port: 8000,
            max_active_sequences: 128,
            startup_timeout_secs: 900,
            hw_class: "nvidia_48gb".into(),
            gpu_count: 2,
            memory_gb_per_gpu: 48.0,
            memory_gb_in_use: 0.0,
            interconnect: "nvlink".into(),
            supplier_input_usd_per_million_tokens: 0.08,
            supplier_output_usd_per_million_tokens: 0.30,
            service_lease: None,
        }
    }

    #[test]
    fn command_is_digest_pinned_and_has_no_buyer_controlled_flags() {
        let profile = profile();
        let args = container_args(&config(), &profile, "cx_vllm_secret");
        let joined = args.join(" ");
        assert!(joined.contains(&profile.container_reference()));
        assert!(joined.contains("--revision bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"));
        assert!(joined.contains("--tokenizer-revision cccccccccccccccccccccccccccccccccccccccc"));
        assert!(joined.contains("--generation-config vllm"));
        assert!(joined.contains("--max-num-seqs 128"));
        assert!(joined.contains("--gpus device=0,1"));
        assert!(!joined.contains("--gpus all"));
        assert!(joined.contains("--enable-prefix-caching"));
        assert!(joined.contains("--enable-chunked-prefill"));
        assert!(!joined.contains(":latest"));
    }

    #[test]
    fn invalid_or_floating_profiles_fail_closed() {
        let mut bad = profile();
        bad.model_revision = "main".into();
        assert!(bad.validate().is_err());
        let mut bad = profile();
        bad.container_digest = "sha256:short".into();
        assert!(bad.validate().is_err());
        let mut bad = profile();
        bad.container_image = "vllm/vllm-openai:latest".into();
        assert!(bad.validate().is_err());
        let mut bad = profile();
        bad.model_weights_gb = 0.0;
        bad.per_rank_overhead_gb = 0.0;
        bad.attention_heads = 0;
        assert!(bad.validate().is_err());
    }

    #[test]
    fn multi_gpu_topology_is_explicit_and_bounded() {
        let profile = profile();
        assert!(validate_config(&config(), &profile).is_ok());
        let mut bad = config();
        bad.interconnect.clear();
        assert!(validate_config(&bad, &profile).is_err());
        let mut bad = config();
        bad.gpu_count = 1;
        assert!(validate_config(&bad, &profile).is_err());
    }

    #[test]
    fn service_lease_requires_real_capacity_and_bounded_probes() {
        let profile = profile();
        let mut configured = config();
        configured.service_lease = Some(ServiceLeaseConfig {
            region: "ca-central-1".into(),
            maximum_warm_replicas: 2,
            available_warm_replicas: 2,
            supplier_nanos_per_replica_hour: 2_000_000_000,
            residency_nanos_per_replica_hour: 200_000_000,
            supports_rolling_upgrade: true,
            probe_interval_secs: 15,
            probe_timeout_millis: 5_000,
            probe_max_tokens: 1,
        });
        assert!(validate_config(&configured, &profile).is_ok());
        configured.service_lease.as_mut().unwrap().probe_max_tokens = 5;
        assert!(validate_config(&configured, &profile).is_err());
    }

    #[test]
    fn service_p95_is_exact_order_statistic_and_requires_five_samples() {
        let mut samples = ServiceLatencySamples::new();
        for sample in [9, 4, 7, 2] {
            samples.record(sample).unwrap();
        }
        assert!(samples.p95_millis().is_err());
        samples.record(6).unwrap();
        assert_eq!(samples.p95_millis().unwrap(), 9);
        for sample in 10..=30 {
            samples.record(sample).unwrap();
        }
        assert_eq!(
            samples.measurement_count(),
            SERVICE_LEASE_MAXIMUM_SAMPLES as u32
        );
        assert_eq!(samples.p95_millis().unwrap(), 29);
    }

    #[test]
    fn documented_reserved_service_config_is_parseable_and_opt_in() {
        let config: VllmAgentConfig = toml::from_str(include_str!("../vllm.example.toml")).unwrap();
        let service = config
            .service_lease
            .expect("the documented service lane must remain an explicit opt-in section");
        assert_eq!(service.region, "ca-central-1");
        assert_eq!(service.probe_max_tokens, 1);
    }
}
