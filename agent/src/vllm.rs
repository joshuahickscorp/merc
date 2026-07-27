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
use crate::types::{RealtimeOfferHeartbeat, RealtimeOfferRegistration};

const HEALTH_INTERVAL: Duration = Duration::from_millis(500);
const HEARTBEAT_INTERVAL: Duration = Duration::from_secs(15);

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
    pub supplier_input_usd_per_million_tokens: f64,
    pub supplier_output_usd_per_million_tokens: f64,
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
    if config.supplier_input_usd_per_million_tokens < 0.0
        || config.supplier_output_usd_per_million_tokens < 0.0
        || config.supplier_input_usd_per_million_tokens > profile.buyer_input_usd_per_million_tokens
        || config.supplier_output_usd_per_million_tokens
            > profile.buyer_output_usd_per_million_tokens
    {
        bail!("supplier rates must be non-negative and no greater than buyer rates")
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
    let mut args = vec![
        "run".into(),
        "--rm".into(),
        "--gpus".into(),
        "all".into(),
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
    client
        .register_realtime(&RealtimeOfferRegistration {
            runtime_profile_id: profile.runtime_profile_id.clone(),
            runtime_profile_sha256: profile_sha256,
            upstream_base_url: config.public_base_url.clone(),
            upstream_token,
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
    loop {
        tokio::select! {
            status = child.wait() => {
                let status = status.context("waiting for vLLM container")?;
                report_terminal_state(&client, &profile.runtime_profile_id, "FAILED").await;
                bail!("vLLM container exited with {status}");
            }
            _ = tokio::signal::ctrl_c() => {
                report_terminal_state(&client, &profile.runtime_profile_id, "DRAINING").await;
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
            supplier_input_usd_per_million_tokens: 0.08,
            supplier_output_usd_per_million_tokens: 0.30,
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
    }
}
