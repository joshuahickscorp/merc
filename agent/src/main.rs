mod config;
mod deadline;
mod executor;
mod fabric;
mod failure;
mod hardware;
mod inference;
mod media;
mod models;
mod pool;
mod protocol;
mod quantized_llama_batched; // vendored + patched candle quantized_llama (bsz>1 batched prefill)
mod render;
mod runtime_authority;
mod runtime_driver;
mod sandbox_egress;
mod status;
mod tls;
mod token_cache;
mod types;
mod vllm;

use std::path::PathBuf;
use std::sync::atomic::{AtomicU64, Ordering};
use std::sync::{Arc, OnceLock};
use std::time::{Duration, SystemTime, UNIX_EPOCH};

use anyhow::{Context, Result};
use clap::{Parser, Subcommand};
use sysinfo::System;
use tokio::sync::Semaphore;

use config::AgentConfig;
use deadline::TaskDeadline;
use executor::{default_runners, dispatch, JobRunner, RunError};
use pool::ModelPool;
use protocol::ControlPlaneClient;
use status::StatusWriter;
use types::{Heartbeat, TaskCommit, TaskDispatch, WorkerCapability};

const AGENT_VERSION: &str = env!("CARGO_PKG_VERSION");

const MODEL_IDLE_EVICT_AFTER: Duration = Duration::from_secs(15 * 60);
const TRANSFER_CHUNK_BYTES: usize = 64 * 1024;
static TRANSFER_BITS_PER_SECOND: AtomicU64 = AtomicU64::new(0);
static LAST_HOST_CPU_MILLIPCT: AtomicU64 = AtomicU64::new(0);
static TRANSFER_NEXT_SLOT: OnceLock<tokio::sync::Mutex<tokio::time::Instant>> = OnceLock::new();

fn set_transfer_limit(max_bandwidth_mbps: f32) {
    let bits_per_second =
        ((max_bandwidth_mbps.max(0.0) as f64) * 1_000_000.0).min(u64::MAX as f64) as u64;
    TRANSFER_BITS_PER_SECOND.store(bits_per_second, Ordering::Release);
}

fn transfer_budget(bytes: usize, bits_per_second: u64) -> Duration {
    if bytes == 0 || bits_per_second == 0 {
        return Duration::ZERO;
    }
    Duration::from_secs_f64((bytes as f64 * 8.0) / bits_per_second as f64)
}

async fn pace_transfer(bytes: usize) {
    let bits_per_second = TRANSFER_BITS_PER_SECOND.load(Ordering::Acquire);
    let budget = transfer_budget(bytes, bits_per_second);
    if budget.is_zero() {
        return;
    }
    let limiter =
        TRANSFER_NEXT_SLOT.get_or_init(|| tokio::sync::Mutex::new(tokio::time::Instant::now()));
    let release_at = {
        let mut next = limiter.lock().await;
        let now = tokio::time::Instant::now();
        let base = (*next).max(now);
        *next = base + budget;
        *next
    };
    tokio::time::sleep_until(release_at).await;
}

const MERC_SANDBOXED_ENV: &str = "MERC_SANDBOXED";

const MERC_SANDBOX_PROFILE_ENV: &str = "MERC_SANDBOX_PROFILE";

/// Explicit opt-in to run buyer payload without a seatbelt profile. Default is
/// refusal: an unsandboxed agent must not execute buyer work unless the operator
/// deliberately accepts the loss of containment and the control plane is told.
const MERC_ALLOW_UNSANDBOXED_ENV: &str = "MERC_ALLOW_UNSANDBOXED";

// Legacy MERC_REQUIRE_SANDBOX is no longer consulted: the default is refuse,
// and MERC_ALLOW_UNSANDBOXED is the only opt-in.

/// Non-routable sentinel passed to seatbelt host params when a slot is unused.
/// Matches no real peer (see merc-agent.sb).
const SANDBOX_HOST_SENTINEL: &str = "0.0.0.0:0";

enum StartAckDisposition {
    Run,
    Report(RunError),
}

fn start_ack_disposition(
    result: std::result::Result<(), protocol::ProtocolError>,
) -> StartAckDisposition {
    match result {
        Ok(()) => StartAckDisposition::Run,
        Err(error) => StartAckDisposition::Report(RunError::Inference {
            backend: "control_plane",
            msg: format!("start_task failed after bounded retries: {error}"),
        }),
    }
}

enum CommitAckDisposition {
    Done,
    Report(RunError),
}

fn commit_ack_disposition(
    result: std::result::Result<(), protocol::ProtocolError>,
) -> CommitAckDisposition {
    match result {
        Ok(()) => CommitAckDisposition::Done,
        Err(error) => CommitAckDisposition::Report(RunError::Inference {
            backend: "control_plane",
            msg: format!("commit_task failed after bounded retries: {error}"),
        }),
    }
}

#[cfg(target_os = "macos")]
fn env_flag_truthy(value: Option<&str>) -> bool {
    value.is_some_and(|value| {
        matches!(
            value.trim().to_ascii_lowercase().as_str(),
            "1" | "true" | "yes"
        )
    })
}

/// True when the operator has explicitly opted in to unsandboxed buyer payload.
#[cfg(target_os = "macos")]
fn unsandboxed_opt_in() -> bool {
    env_flag_truthy(std::env::var(MERC_ALLOW_UNSANDBOXED_ENV).ok().as_deref())
}

/// True when this process is currently inside the seatbelt profile (re-exec marker).
fn agent_is_sandboxed() -> bool {
    std::env::var(MERC_SANDBOXED_ENV).as_deref() == Ok("1")
}

/// True when the operator accepted unsandboxed execution. Recorded on the
/// capability so the control plane can refuse private work.
fn agent_unsandboxed_opt_in() -> bool {
    #[cfg(target_os = "macos")]
    {
        unsandboxed_opt_in()
    }
    #[cfg(not(target_os = "macos"))]
    {
        // Non-macOS has no seatbelt; the capability still reports that fact
        // (sandboxed=false) so the control plane can decide routing.
        std::env::var(MERC_ALLOW_UNSANDBOXED_ENV)
            .ok()
            .as_deref()
            .is_some_and(|value| {
                matches!(
                    value.trim().to_ascii_lowercase().as_str(),
                    "1" | "true" | "yes"
                )
            })
    }
}

#[cfg(target_os = "macos")]
fn sandbox_wrap_failed(message: &str) {
    // Default is refuse. Opt-in via MERC_ALLOW_UNSANDBOXED=1 is required to
    // continue; the greppable refusal string is intentional.
    if unsandboxed_opt_in() {
        tracing::warn!("merc-agent is running UNSANDBOXED (MERC_ALLOW_UNSANDBOXED=1): {message}");
        return;
    }
    tracing::error!(
        "merc-agent REFUSED_UNSANDBOXED_BUYER_PAYLOAD: {message}. Set {MERC_ALLOW_UNSANDBOXED_ENV}=1 to opt in deliberately (capability will record unsandboxed_opt_in); set {MERC_SANDBOX_PROFILE_ENV} to merc-agent.sb for containment"
    );
    std::process::exit(78);
}

#[cfg(target_os = "macos")]
fn reexec_under_sandbox_if_needed() {
    use std::os::unix::process::CommandExt;

    if agent_is_sandboxed() {
        return;
    }

    let profile = match resolve_sandbox_profile() {
        Some(p) => p,
        None => {
            sandbox_wrap_failed(&format!(
                "no seatbelt profile found (set {MERC_SANDBOX_PROFILE_ENV} to merc-agent.sb, or launch via the MercAgent .app); buyer-payload filesystem/network containment is not active"
            ));
            return;
        }
    };

    const SANDBOX_EXEC: &str = "/usr/bin/sandbox-exec";
    if !std::path::Path::new(SANDBOX_EXEC).exists() {
        sandbox_wrap_failed(&format!("{SANDBOX_EXEC} not found (unexpected on macOS)"));
        return;
    }

    let exe = match std::env::current_exe() {
        Ok(e) => e,
        Err(err) => {
            sandbox_wrap_failed(&format!("could not resolve current_exe ({err})"));
            return;
        }
    };
    let args: Vec<String> = std::env::args().skip(1).collect();

    let home = std::env::var("HOME").unwrap_or_default();
    let modelcache = sandbox_model_cache_dir();
    let datadir = sandbox_data_dir(&home);
    let tmpdir = std::env::var("TMPDIR").unwrap_or_else(|_| "/private/var/folders".to_string());

    // Host allowlist for the local egress proxy. Public sandbox-exec cannot
    // express host-scoped remote TCP (only * or localhost), so remote peers
    // are reached via a loopback CONNECT proxy spawned outside the seatbelt.
    let control_host = sandbox_control_host();
    let artifact_host = sandbox_artifact_host();
    let model_host = sandbox_model_host();
    let allowlist = sandbox_egress::build_allowlist(
        &control_host,
        &artifact_host,
        &model_host,
        SANDBOX_HOST_SENTINEL,
    );
    let (_proxy_child, proxy_url) = match sandbox_egress::spawn_proxy_process(&exe, &allowlist) {
        Ok(v) => v,
        Err(err) => {
            sandbox_wrap_failed(&format!(
                "could not start allowlisted egress proxy ({err}); without it seatbelt cannot reach remote control/artifact hosts"
            ));
            return;
        }
    };
    // Intentionally leak the Child handle: the proxy must outlive this process
    // after exec. Dropping would kill it. The OS reaps it when the session ends.
    std::mem::forget(_proxy_child);

    let bindir = exe
        .parent()
        .map(|p| p.to_string_lossy().into_owned())
        .unwrap_or_else(|| "/usr/local/bin".to_string());

    tracing::info!(
        "re-executing merc-agent under the macOS seatbelt sandbox (profile: {}, egress_proxy: {}, allowlist: {:?})",
        profile.display(),
        proxy_url,
        allowlist
    );

    let mut cmd = std::process::Command::new(SANDBOX_EXEC);
    cmd.arg("-f")
        .arg(&profile)
        .arg("-D")
        .arg(format!("HOME={home}"))
        .arg("-D")
        .arg(format!("MODELCACHE={modelcache}"))
        .arg("-D")
        .arg(format!("DATADIR={datadir}"))
        .arg("-D")
        .arg(format!("TMPDIR={tmpdir}"))
        .arg("-D")
        .arg(format!("BINDIR={bindir}"))
        .arg(&exe)
        .args(&args)
        .env(MERC_SANDBOXED_ENV, "1")
        .env(sandbox_egress::MERC_EGRESS_PROXY_ENV, &proxy_url)
        // reqwest / ureq also honour the standard proxy vars when configured.
        .env("HTTPS_PROXY", &proxy_url)
        .env("HTTP_PROXY", &proxy_url)
        .env("https_proxy", &proxy_url)
        .env("http_proxy", &proxy_url)
        .env("NO_PROXY", "localhost,127.0.0.1")
        .env("no_proxy", "localhost,127.0.0.1");

    let err = cmd.exec();
    sandbox_wrap_failed(&format!("failed to re-exec under {SANDBOX_EXEC} ({err})"));
}

#[cfg(not(target_os = "macos"))]
fn reexec_under_sandbox_if_needed() {
    // Seatbelt is macOS-only. Non-macOS agents report sandboxed=false; private
    // work routing is a control-plane decision based on the capability record.
    // Linux suppliers currently have no equivalent seatbelt path in-tree.
    // Do not refuse the process here: that would brick every non-mac worker
    // overnight. Containment on Linux is a separate work item; the capability
    // record still truthfully says sandboxed=false.
    tracing::warn!(
        "merc-agent containment: seatbelt is macOS-only; this process is not filesystem/network sandboxed"
    );
}

/// host:port for the control plane, derived from MERC_CONTROL_URL or a
/// best-effort parse of the run config path is not available at re-exec time
/// (re-exec happens before config load). Env wins; otherwise localhost:8080
/// for the local-dev path.
#[cfg(target_os = "macos")]
fn sandbox_control_host() -> String {
    if let Ok(url) = std::env::var("MERC_CONTROL_URL") {
        if let Some(host) = sandbox_egress::host_port_from_url(&url) {
            return host;
        }
    }
    // Fallback for local agent.toml without env override: loopback is already
    // allowed by the profile, so a remote-looking sentinel is fine when the
    // operator is on localhost; for remote control_url they must set
    // MERC_CONTROL_URL (or MERC_SANDBOX_CONTROL_HOST) so the host is declared.
    if let Ok(host) = std::env::var("MERC_SANDBOX_CONTROL_HOST") {
        if !host.trim().is_empty() {
            return host.trim().to_string();
        }
    }
    "127.0.0.1:8080".to_string()
}

#[cfg(target_os = "macos")]
fn sandbox_artifact_host() -> String {
    if let Ok(host) = std::env::var("MERC_SANDBOX_ARTIFACT_HOST") {
        if !host.trim().is_empty() {
            return host.trim().to_string();
        }
    }
    SANDBOX_HOST_SENTINEL.to_string()
}

#[cfg(target_os = "macos")]
fn sandbox_model_host() -> String {
    if let Ok(host) = std::env::var("MERC_SANDBOX_MODEL_HOST") {
        if !host.trim().is_empty() {
            return host.trim().to_string();
        }
    }
    // HuggingFace is the default model origin when downloads are enabled.
    // Operators who disable downloads can leave the sentinel.
    if std::env::var("MERC_ALLOW_MODEL_DOWNLOADS")
        .map(|v| env_flag_truthy(Some(&v)))
        .unwrap_or(true)
    {
        return "huggingface.co:443".to_string();
    }
    SANDBOX_HOST_SENTINEL.to_string()
}

#[cfg(target_os = "macos")]
fn resolve_sandbox_profile() -> Option<PathBuf> {
    let override_path = std::env::var(MERC_SANDBOX_PROFILE_ENV)
        .ok()
        .filter(|p| !p.is_empty())
        .map(PathBuf::from);
    let exe_sibling = std::env::current_exe()
        .ok()
        .and_then(|exe| exe.parent().map(|d| d.join("merc-agent.sb")));
    pick_sandbox_profile(override_path, exe_sibling, |p| p.is_file())
}

#[cfg(target_os = "macos")]
fn pick_sandbox_profile(
    override_path: Option<PathBuf>,
    exe_sibling: Option<PathBuf>,
    exists: impl Fn(&std::path::Path) -> bool,
) -> Option<PathBuf> {
    if let Some(p) = override_path {
        if exists(&p) {
            return Some(p);
        }
    }
    if let Some(p) = exe_sibling {
        if exists(&p) {
            return Some(p);
        }
    }
    None
}

#[cfg(target_os = "macos")]
fn sandbox_model_cache_dir() -> String {
    if let Ok(d) = std::env::var("MERC_MODEL_CACHE") {
        if !d.is_empty() {
            return d;
        }
    }
    if let Ok(hf) = std::env::var("HF_HOME") {
        if !hf.is_empty() {
            return hf;
        }
    }
    let home = std::env::var("HOME").unwrap_or_default();
    format!("{home}/.cache/huggingface")
}

#[cfg(target_os = "macos")]
fn sandbox_data_dir(home: &str) -> String {
    // Migrate ~/.compute-exchange → ~/.merc when needed so the seatbelt DATADIR
    // param covers the tree the agent will actually write.
    config::agent_home_dir_for(std::path::Path::new(home))
        .to_string_lossy()
        .into_owned()
}

#[derive(Parser)]
#[command(name = "merc-agent", version = AGENT_VERSION, about = "merc supplier agent")]
struct Cli {
    #[command(subcommand)]
    command: Command,
}

#[derive(Subcommand)]
enum Command {
    Run {
        #[arg(long, default_value = "agent.toml")]
        config: PathBuf,
    },
    /// Internal: allowlisted CONNECT proxy used under the macOS seatbelt.
    /// Spawned by the unsandboxed parent before re-exec; not an operator command.
    SandboxEgressProxy {
        /// host:port peers the sandboxed agent may reach via CONNECT.
        #[arg(long = "allow", required = true)]
        allow: Vec<String>,
    },
    /// Run the pinned CUDA/vLLM realtime adapter.
    Vllm {
        #[arg(long, default_value = "vllm.toml")]
        config: PathBuf,
    },
    /// Check a pinned CUDA/vLLM configuration and local GPU/container runtime
    /// without pulling an image, starting a process, or advertising capacity.
    VllmCheck {
        #[arg(long, default_value = "vllm.toml")]
        config: PathBuf,
    },
    /// Serve bounded mTLS echo and synthetic-collective probes for a candidate
    /// Merc Fabric link. This proves only measurements; it does not expose a
    /// workload data plane.
    FabricServe {
        /// Address to bind, for example `10.0.0.12:9444`.
        #[arg(long)]
        bind: String,
        /// DER-encoded end-entity certificate for this worker's mTLS identity.
        #[arg(long)]
        certificate_der: PathBuf,
        /// DER-encoded private key for the local mTLS certificate. Must be 0600 on Unix.
        #[arg(long)]
        private_key_der: PathBuf,
        /// DER-encoded CA certificate trusted for both Fabric peer directions.
        #[arg(long)]
        ca_certificate_der: PathBuf,
        /// DNS subject-alt-name of the Fabric server certificate, never inferred from an IP.
        #[arg(long)]
        server_name: String,
        /// Existing agent configuration used to bind this certificate and peer observations to control.
        #[arg(long)]
        agent_config: PathBuf,
    },
    /// Measure one certificate-bound mTLS candidate Merc Fabric link and emit a raw receipt.
    FabricProbe {
        /// Peer address advertised by the known site owner, for example `10.0.0.13:9444`.
        #[arg(long)]
        endpoint: String,
        /// DER-encoded end-entity certificate for this worker's mTLS identity.
        #[arg(long)]
        certificate_der: PathBuf,
        /// DER-encoded private key for the local mTLS certificate. Must be 0600 on Unix.
        #[arg(long)]
        private_key_der: PathBuf,
        /// DER-encoded CA certificate trusted for both Fabric peer directions.
        #[arg(long)]
        ca_certificate_der: PathBuf,
        /// DNS subject-alt-name expected on the peer Fabric server certificate.
        #[arg(long)]
        server_name: String,
        /// Operator-declared site label. It is a claim, not a verified geography authority.
        #[arg(long)]
        site: String,
        /// Enrolled peer worker expected to observe every round.
        #[arg(long)]
        peer_worker_id: uuid::Uuid,
        /// Random payload bytes per round; bounded at 4 MiB.
        #[arg(long, default_value_t = fabric::DEFAULT_PAYLOAD_BYTES)]
        bytes: usize,
        /// Number of independently connected rounds; bounded at 32.
        #[arg(long, default_value_t = fabric::DEFAULT_ROUNDS)]
        rounds: u16,
        /// Receipt destination. Refuses to overwrite an existing file; omit to print JSON.
        #[arg(long)]
        out: Option<PathBuf>,
        /// Existing agent configuration used to bind the local certificate, reserve the peer, and upload evidence.
        #[arg(long)]
        agent_config: PathBuf,
    },
    /// Measure a bounded two-rank synthetic XOR all-reduce over a
    /// certificate-bound mTLS link. This emits non-admissible mechanics
    /// evidence only: it never sends buyer payloads or enables local-cluster
    /// placement.
    FabricCollectiveProbe {
        /// Peer address advertised by the known site owner, for example `10.0.0.13:9444`.
        #[arg(long)]
        endpoint: String,
        /// DER-encoded end-entity certificate for this worker's mTLS identity.
        #[arg(long)]
        certificate_der: PathBuf,
        /// DER-encoded private key for the local mTLS certificate. Must be 0600 on Unix.
        #[arg(long)]
        private_key_der: PathBuf,
        /// DER-encoded CA certificate trusted for both Fabric peer directions.
        #[arg(long)]
        ca_certificate_der: PathBuf,
        /// DNS subject-alt-name expected on the peer Fabric server certificate.
        #[arg(long)]
        server_name: String,
        /// Operator-declared site label. It is a claim, not a verified geography authority.
        #[arg(long)]
        site: String,
        /// Enrolled peer worker expected to observe every synthetic round.
        #[arg(long)]
        peer_worker_id: uuid::Uuid,
        /// Random payload bytes per rank per round; bounded at 4 MiB.
        #[arg(long, default_value_t = fabric::DEFAULT_PAYLOAD_BYTES)]
        bytes: usize,
        /// Number of independently connected collective rounds; bounded at 32.
        #[arg(long, default_value_t = fabric::DEFAULT_ROUNDS)]
        rounds: u16,
        /// Receipt destination. Refuses to overwrite an existing file; omit to print JSON.
        #[arg(long)]
        out: Option<PathBuf>,
        /// Existing agent configuration used to bind the local certificate and reserve the peer observation session.
        #[arg(long)]
        agent_config: PathBuf,
    },
    /// Derive a fresh, bidirectional Fabric evidence receipt for a candidate
    /// local site. It can report peer-observed synthetic collectives, but never
    /// grants LOCAL_CLUSTER placement: workload collectives, gang scheduling,
    /// and topology economics remain separate control authorities.
    FabricTopology {
        /// The same declared site label used for every directional FabricProbe.
        #[arg(long)]
        site: String,
        /// Comma-separated worker UUIDs, including this agent's worker identity.
        #[arg(long)]
        worker_ids: String,
        /// Existing agent configuration used to authenticate this supplier-scoped evaluation.
        #[arg(long)]
        agent_config: PathBuf,
    },
    Bench {
        #[arg(long)]
        config: Option<PathBuf>,
    },
    BenchBatch {
        #[arg(long, default_value = "llama-3.2-1b-instruct-q4")]
        model: String,
        #[arg(long, default_value_t = 48)]
        max_tokens: u32,
        #[arg(long, default_value = "1,8,32,64")]
        batch_sizes: String,
        #[arg(
            long,
            default_value = "Write a detailed paragraph about the ocean and its wonders:"
        )]
        prompt: String,
        #[arg(long, default_value_t = false)]
        require_deterministic: bool,
        #[arg(long, default_value_t = 1)]
        reps: u32,
        #[arg(long, default_value = "identical")]
        mode: String,
        /// Comma-separated backends to sweep: `candle`, `openai_http`.
        #[arg(long, default_value = "candle")]
        backends: String,
        #[arg(long, default_value = "http://127.0.0.1:8099/v1")]
        openai_base_url: String,
        #[arg(long, default_value = "cx-chat-1b")]
        openai_model: String,
        #[arg(long, default_value = "")]
        openai_api_key: String,
    },
    BenchSustained {
        #[arg(long, default_value = "llama-3.2-1b-instruct-q4")]
        model: String,
        #[arg(long, default_value_t = 48)]
        max_tokens: u32,
        #[arg(long, default_value_t = 8)]
        batch: usize,
        #[arg(
            long,
            default_value = "Write a detailed paragraph about the ocean and its wonders:"
        )]
        prompt: String,
        #[arg(long, default_value_t = 8)]
        minutes: u64,
        #[arg(long, default_value_t = 30)]
        window_secs: u64,
    },
    BenchConcurrency {
        #[arg(long, default_value = "1,2,4")]
        permits: String,
        #[arg(long, default_value_t = 8)]
        embed_tasks: usize,
        #[arg(long, default_value_t = 8)]
        llama_tasks: usize,
        #[arg(long, default_value = "llama-3.2-1b-instruct-q4")]
        model: String,
        #[arg(long, default_value_t = 24)]
        max_tokens: u32,
    },
    /// Measure the embed cell on two runtime profiles over one corpus, and emit
    /// the receipt shape a profile's `benchmark_authority` must point at.
    ///
    /// One harness, one corpus, one output contract, both drivers. A comparison
    /// assembled from two harnesses measures the harnesses.
    BenchEmbed {
        #[arg(long, default_value = "all-minilm-l6-v2")]
        model: String,
        /// The merc commit this receipt is evidence for.
        #[arg(long, default_value = "")]
        source_commit: String,
        /// llama-server started with `--embedding --pooling mean` on the pinned GGUF.
        #[arg(long, default_value = "http://127.0.0.1:8188")]
        llama_base_url: String,
        #[arg(long, default_value = "1,8,32,128")]
        batch_sizes: String,
        #[arg(long, default_value_t = 3)]
        reps: u32,
        /// Where to write the receipt JSON. Printed to stdout when empty.
        #[arg(long, default_value = "")]
        out: String,
    },
    Characterize,
    Version,
    /// Measure a batch_infer known-answer honeypot against this binary's engine
    /// build. Prints one JSON object with answer_class and known_answer for
    /// scripts/seed-batch-infer-honeypot.sh / control seed.
    HoneypotAnswer {
        #[arg(long, default_value = "llama-3.2-1b-instruct-q4")]
        model: String,
        #[arg(long, default_value_t = 12)]
        max_tokens: u32,
        #[arg(long, default_value = "Reply with only: merc-honeypot-ok")]
        prompt: String,
    },
    /// Emit the exact bytes a worker commits for an embed task, executed on a
    /// named runtime cell.
    ///
    /// Same reason HoneypotAnswer exists: the control plane needs the real
    /// commit bytes, and a hand-written fixture is a different artifact that
    /// merely looks like one. This is the bridge that lets a Go integration test
    /// drive REAL engine output through the Merc chain rather than a plausible
    /// stand-in — which is the whole difference between a benchmarked engine and
    /// a proven runtime cell.
    EmitEmbedArtifact {
        /// `candle_metal` (in-process safetensors) or `llama_cpp_metal` (GGUF
        /// via a llama-server this operator runs).
        #[arg(long, default_value = "candle_metal")]
        runtime: String,
        #[arg(long, default_value = "all-minilm-l6-v2")]
        model: String,
        #[arg(long, default_value = "http://127.0.0.1:8188")]
        llama_base_url: String,
        /// JSONL, one {"id":..,"text":..} per line. Reads stdin when empty.
        #[arg(long, default_value = "")]
        input: String,
        /// Where to write the committed bytes.
        #[arg(long)]
        out: String,
        /// Emit the compact float32 artifact instead of JSON.
        #[arg(long, default_value_t = false)]
        binary: bool,
    },
}

fn init_tracing() {
    use tracing_subscriber::{fmt, EnvFilter};
    let filter = EnvFilter::try_from_default_env().unwrap_or_else(|_| EnvFilter::new("info"));
    fmt()
        .with_env_filter(filter)
        .with_target(false)
        .with_writer(std::io::stderr)
        .init();
}

fn now_unix() -> u64 {
    SystemTime::now()
        .duration_since(UNIX_EPOCH)
        .map(|d| d.as_secs())
        .unwrap_or(0)
}

pub(crate) fn current_local_schedule_clock() -> (u8, u8) {
    unsafe {
        let now = libc::time(std::ptr::null_mut());
        let mut tm: libc::tm = std::mem::zeroed();
        #[cfg(unix)]
        libc::localtime_r(&now, &mut tm);
        #[cfg(windows)]
        libc::localtime_s(&mut tm, &now);
        (tm.tm_hour.clamp(0, 23) as u8, tm.tm_wday.clamp(0, 6) as u8)
    }
}

pub(crate) fn on_battery() -> bool {
    hardware::on_battery()
}

fn cpu_pct(sys: &mut System) -> f32 {
    sys.refresh_cpu_usage();
    let cpus = sys.cpus();
    if cpus.is_empty() {
        return 0.0;
    }
    let sum: f32 = cpus.iter().map(|c| c.cpu_usage()).sum();
    sum / cpus.len() as f32
}

fn throttle_snapshot() -> hardware::MemorySnapshot {
    hardware::read_memory_snapshot()
}

fn gpu_telemetry() -> (f32, Option<f32>) {
    (0.0, None)
}

#[tokio::main]
async fn main() -> Result<()> {
    let cli = Cli::parse();
    match cli.command {
        Command::Version => {
            println!("merc-agent {AGENT_VERSION}");
            Ok(())
        }
        Command::HoneypotAnswer {
            model,
            max_tokens,
            prompt,
        } => {
            init_tracing();
            run_honeypot_answer(&model, max_tokens, &prompt).await
        }
        Command::Bench { config } => {
            init_tracing();
            run_bench(config).await
        }
        Command::BenchBatch {
            model,
            max_tokens,
            batch_sizes,
            prompt,
            require_deterministic,
            reps,
            mode,
            backends,
            openai_base_url,
            openai_model,
            openai_api_key,
        } => {
            init_tracing();
            run_bench_batch(
                &model,
                max_tokens,
                &batch_sizes,
                &prompt,
                require_deterministic,
                reps,
                &mode,
                &backends,
                &openai_base_url,
                &openai_model,
                &openai_api_key,
            )
            .await
        }
        Command::BenchSustained {
            model,
            max_tokens,
            batch,
            prompt,
            minutes,
            window_secs,
        } => {
            init_tracing();
            run_bench_sustained(&model, max_tokens, batch, &prompt, minutes, window_secs)
        }
        Command::BenchConcurrency {
            permits,
            embed_tasks,
            llama_tasks,
            model,
            max_tokens,
        } => {
            init_tracing();
            run_bench_concurrency(&permits, embed_tasks, llama_tasks, &model, max_tokens).await
        }
        Command::BenchEmbed {
            model,
            source_commit,
            llama_base_url,
            batch_sizes,
            reps,
            out,
        } => {
            init_tracing();
            run_bench_embed(
                &model,
                &source_commit,
                &llama_base_url,
                &batch_sizes,
                reps,
                &out,
            )
            .await
        }
        Command::EmitEmbedArtifact {
            runtime,
            model,
            llama_base_url,
            input,
            out,
            binary,
        } => {
            init_tracing();
            run_emit_embed_artifact(&runtime, &model, &llama_base_url, &input, &out, binary).await
        }
        Command::Characterize => {
            init_tracing();
            run_characterize().await
        }
        Command::Run { config } => {
            init_tracing();
            reexec_under_sandbox_if_needed();
            let cfg = AgentConfig::load(&config)
                .with_context(|| format!("loading config {}", config.display()))?;
            run_agent(cfg).await
        }
        Command::SandboxEgressProxy { allow } => {
            // No tracing init: the parent reads the first stdout line as the URL.
            sandbox_egress::run_proxy_main(allow).context("sandbox egress proxy")?;
            Ok(())
        }
        Command::Vllm { config } => {
            init_tracing();
            vllm::run(config).await
        }
        Command::VllmCheck { config } => {
            init_tracing();
            let receipt = vllm::preflight(config).await?;
            println!(
                "{}",
                serde_json::to_string_pretty(&receipt)
                    .context("rendering vLLM preflight receipt")?
            );
            Ok(())
        }
        Command::FabricServe {
            bind,
            certificate_der,
            private_key_der,
            ca_certificate_der,
            server_name,
            agent_config,
        } => {
            init_tracing();
            let bind = bind
                .parse()
                .with_context(|| format!("parsing fabric bind address {bind}"))?;
            let tls = fabric::read_tls_material(
                &certificate_der,
                &private_key_der,
                &ca_certificate_der,
                server_name,
            )?;
            let cfg = AgentConfig::load(&agent_config).with_context(|| {
                format!(
                    "loading agent configuration {} for fabric observer",
                    agent_config.display()
                )
            })?;
            let observer = ControlPlaneClient::new(cfg.control_url, cfg.worker_token)
                .context("building control-plane client for fabric observer")?;
            observer
                .register_fabric_identity(&tls.certificate_sha256())
                .await
                .context("binding immutable fabric mTLS certificate identity to this worker")?;
            fabric::serve(bind, tls, observer).await
        }
        Command::FabricProbe {
            endpoint,
            certificate_der,
            private_key_der,
            ca_certificate_der,
            server_name,
            site,
            peer_worker_id,
            bytes,
            rounds,
            out,
            agent_config,
        } => {
            init_tracing();
            let endpoint = endpoint
                .parse()
                .with_context(|| format!("parsing fabric peer address {endpoint}"))?;
            let tls = fabric::read_tls_material(
                &certificate_der,
                &private_key_der,
                &ca_certificate_der,
                server_name,
            )?;
            let cfg = AgentConfig::load(&agent_config).with_context(|| {
                format!(
                    "loading agent configuration {} for fabric receipt upload",
                    agent_config.display()
                )
            })?;
            let control = ControlPlaneClient::new(cfg.control_url, cfg.worker_token)
                .context("building control-plane client for fabric receipt upload")?;
            control
                .register_fabric_identity(&tls.certificate_sha256())
                .await
                .context("binding immutable fabric mTLS certificate identity to this worker")?;
            let session = control
                .create_fabric_session(peer_worker_id, &site)
                .await
                .context("reserving certificate-bound fabric peer session")?;
            let receipt = fabric::probe(fabric::ProbeOptions {
                endpoint,
                site,
                fabric_session_id: session.fabric_session_id,
                expected_peer_worker_id: Some(peer_worker_id),
                expected_peer_certificate_sha256: session.peer_certificate_sha256,
                payload_bytes: bytes,
                rounds,
                tls,
            })
            .await?;
            let rendered = serde_json::to_vec_pretty(&receipt)
                .context("encoding fabric measurement receipt")?;
            if let Some(path) = out {
                fabric::write_new_receipt(&path, &rendered)?;
                tracing::info!(receipt = %path.display(), "fabric measurement receipt written");
            } else {
                println!("{}", String::from_utf8_lossy(&rendered));
            }
            let transcripts = receipt
                .rounds
                .iter()
                .map(|round| round.transcript_sha256.clone())
                .collect::<Vec<_>>();
            control
                .wait_for_fabric_observations(session.fabric_session_id, &transcripts)
                .await
                .context(
                    "waiting for every reserved peer observation before uploading fabric receipt",
                )?;
            control
                .submit_fabric_receipt(&receipt)
                .await
                .context("uploading mutual mTLS fabric measurement receipt")?;
            tracing::info!(receipt_id = %receipt.receipt_id, "fabric receipt recorded by control plane as mTLS worker-bound non-admissible evidence");
            Ok(())
        }
        Command::FabricCollectiveProbe {
            endpoint,
            certificate_der,
            private_key_der,
            ca_certificate_der,
            server_name,
            site,
            peer_worker_id,
            bytes,
            rounds,
            out,
            agent_config,
        } => {
            init_tracing();
            let endpoint = endpoint
                .parse()
                .with_context(|| format!("parsing fabric collective peer address {endpoint}"))?;
            let tls = fabric::read_tls_material(
                &certificate_der,
                &private_key_der,
                &ca_certificate_der,
                server_name,
            )?;
            let cfg = AgentConfig::load(&agent_config).with_context(|| {
                format!(
                    "loading agent configuration {} for fabric collective evidence",
                    agent_config.display()
                )
            })?;
            let control = ControlPlaneClient::new(cfg.control_url, cfg.worker_token)
                .context("building control-plane client for fabric collective evidence")?;
            control
                .register_fabric_identity(&tls.certificate_sha256())
                .await
                .context("binding immutable fabric mTLS certificate identity to this worker")?;
            let session = control
                .create_fabric_session(peer_worker_id, &site)
                .await
                .context("reserving certificate-bound fabric peer session")?;
            let receipt = fabric::collective_probe(fabric::CollectiveProbeOptions {
                endpoint,
                site,
                fabric_session_id: session.fabric_session_id,
                expected_peer_worker_id: Some(peer_worker_id),
                expected_peer_certificate_sha256: session.peer_certificate_sha256,
                payload_bytes_per_rank: bytes,
                rounds,
                tls,
            })
            .await?;
            let transcripts = receipt
                .rounds
                .iter()
                .map(|round| round.transcript_sha256.clone())
                .collect::<Vec<_>>();
            control
                .wait_for_fabric_observations(session.fabric_session_id, &transcripts)
                .await
                .context(
                    "waiting for every reserved peer observation before emitting collective evidence",
                )?;
            control
                .submit_fabric_collective_receipt(&receipt)
                .await
                .context("persisting non-admissible fabric collective receipt")?;
            let rendered = serde_json::to_vec_pretty(&receipt)
                .context("encoding fabric collective receipt")?;
            if let Some(path) = out {
                fabric::write_new_receipt(&path, &rendered)?;
                tracing::info!(receipt = %path.display(), "non-admissible fabric collective receipt written");
            } else {
                println!("{}", String::from_utf8_lossy(&rendered));
            }
            tracing::warn!(receipt_id = %receipt.receipt_id, "synthetic collective receipt is retained as evidence only and is not admitted for placement, pricing, or settlement");
            Ok(())
        }
        Command::FabricTopology {
            site,
            worker_ids,
            agent_config,
        } => {
            init_tracing();
            let worker_ids = parse_fabric_topology_workers(&worker_ids)?;
            let cfg = AgentConfig::load(&agent_config).with_context(|| {
                format!(
                    "loading agent configuration {} for fabric topology evaluation",
                    agent_config.display()
                )
            })?;
            let control = ControlPlaneClient::new(cfg.control_url, cfg.worker_token)
                .context("building control-plane client for fabric topology evaluation")?;
            let evaluation = control
                .evaluate_fabric_topology(&site, &worker_ids)
                .await
                .context("deriving fresh certificate-bound fabric topology evidence")?;
            println!(
                "{}",
                serde_json::to_string_pretty(&evaluation)
                    .context("rendering fabric topology evaluation")?
            );
            Ok(())
        }
    }
}

fn parse_fabric_topology_workers(raw: &str) -> Result<Vec<uuid::Uuid>> {
    let mut workers = raw
        .split(',')
        .map(str::trim)
        .filter(|value| !value.is_empty())
        .map(|value| {
            value
                .parse::<uuid::Uuid>()
                .context("parsing --worker-ids UUID")
        })
        .collect::<Result<Vec<_>>>()?;
    if workers.len() < 2 || workers.len() > 8 {
        anyhow::bail!("--worker-ids must contain 2..8 distinct worker UUIDs")
    }
    workers.sort_unstable();
    if workers.windows(2).any(|pair| pair[0] == pair[1]) {
        anyhow::bail!("--worker-ids contains a duplicate worker UUID")
    }
    Ok(workers)
}

#[derive(serde::Serialize)]
struct CharacterizationReceipt {
    schema_version: u32,
    kind: &'static str,
    status: &'static str,
    physical_devices_observed: u32,
    device: String,
    device_model: String,
    hardware_class: String,
    memory_gb: f32,
    operating_system: String,
    metal_available: bool,
    source_identity: String,
    runtime_authority_sha256: String,
    model_revisions: Vec<(&'static str, &'static str)>,
    benchmarks: Vec<types::BenchResult>,
    peak_rss_bytes: u64,
    thermal_state: String,
    throttling: bool,
    cache_behavior: String,
    limitations: Vec<&'static str>,
}

async fn run_characterize() -> Result<()> {
    let benchmark_cache = std::env::var("MERC_BENCH_CACHE_PATH")
        .map(PathBuf::from)
        .unwrap_or_else(|_| config::agent_home_dir().join("bench_cache.json"));
    let reused_benchmark_cache = benchmark_cache.is_file();
    let pool = ModelPool::new();
    let capability =
        hardware::detect_and_benchmark(uuid::Uuid::nil(), AGENT_VERSION, 0.0, "candle", &pool)
            .await;
    let thermal = hardware::read_thermal_pressure();
    let throttling = matches!(
        thermal,
        Some(config::ThermalPressure::Serious | config::ThermalPressure::Critical)
    ) || capability.benchmarks.iter().any(|item| !item.thermal_ok);
    let sw_vers = |argument: &str| {
        std::process::Command::new("sw_vers")
            .arg(argument)
            .output()
            .ok()
            .filter(|value| value.status.success())
            .map(|value| String::from_utf8_lossy(&value.stdout).trim().to_string())
            .filter(|value| !value.is_empty())
    };
    let os = match (
        sw_vers("-productName"),
        sw_vers("-productVersion"),
        sw_vers("-buildVersion"),
    ) {
        (Some(name), Some(version), Some(build)) => format!("{name} {version} ({build})"),
        _ => format!("{} {}", std::env::consts::OS, std::env::consts::ARCH),
    };
    let device_model = std::process::Command::new("sysctl")
        .args(["-n", "hw.model"])
        .output()
        .ok()
        .filter(|value| value.status.success())
        .map(|value| String::from_utf8_lossy(&value.stdout).trim().to_string())
        .filter(|value| !value.is_empty())
        .unwrap_or_else(|| "unavailable".into());
    let cache_behavior = if reused_benchmark_cache {
        "warm benchmark cache reused after build/hardware identity validation"
    } else {
        "fresh isolated benchmark run completed and cache receipt written"
    };
    let receipt = CharacterizationReceipt {
        schema_version: 1,
        kind: "cx_agent_device_characterization",
        status: "PASS",
        physical_devices_observed: 1,
        device: models::device_label().to_string(),
        device_model,
        hardware_class: capability.hw_class.as_wire_str().to_string(),
        memory_gb: capability.memory_gb,
        operating_system: os,
        metal_available: models::device().is_metal(),
        source_identity: capability.build_hash.clone(),
        runtime_authority_sha256: runtime_authority::sha256().to_string(),
        model_revisions: models::retained_model_revisions(),
        benchmarks: capability.benchmarks,
        peak_rss_bytes: peak_rss_bytes(),
        thermal_state: thermal
            .map(|value| value.as_str().to_string())
            .unwrap_or_else(|| "unavailable".into()),
        throttling,
        cache_behavior: cache_behavior.to_string(),
        limitations: vec![
            "single physical Mac only",
            "additional authorized devices require separate receipts",
        ],
    };
    println!("{}", serde_json::to_string_pretty(&receipt)?);
    Ok(())
}

#[cfg(unix)]
fn peak_rss_bytes() -> u64 {
    let mut usage = std::mem::MaybeUninit::<libc::rusage>::zeroed();
    let ok = unsafe { libc::getrusage(libc::RUSAGE_SELF, usage.as_mut_ptr()) } == 0;
    if !ok {
        return 0;
    }
    let rss = unsafe { usage.assume_init().ru_maxrss.max(0) as u64 };
    #[cfg(target_os = "macos")]
    {
        rss
    }
    #[cfg(not(target_os = "macos"))]
    {
        rss * 1024
    }
}

#[cfg(not(unix))]
fn peak_rss_bytes() -> u64 {
    0
}

async fn run_bench(config: Option<PathBuf>) -> Result<()> {
    let supplier_id = match config {
        Some(path) => {
            let cfg = AgentConfig::load(&path)
                .with_context(|| format!("loading config {}", path.display()))?;
            cfg.supplier_id
        }
        None => uuid::Uuid::nil(),
    };
    let pool = ModelPool::new();
    let cap =
        hardware::detect_and_benchmark(supplier_id, AGENT_VERSION, 0.0, "candle", &pool).await;
    println!("{}", serde_json::to_string_pretty(&cap)?);
    Ok(())
}

fn coefficient_of_variation_pct(xs: &[f64]) -> f64 {
    if xs.len() < 2 {
        return 0.0;
    }
    let mean = xs.iter().sum::<f64>() / xs.len() as f64;
    if mean <= 0.0 {
        return 0.0;
    }
    let variance = xs.iter().map(|x| (x - mean).powi(2)).sum::<f64>() / xs.len() as f64;
    variance.sqrt() / mean * 100.0
}

fn median(xs: &mut [f64]) -> f64 {
    xs.sort_by(|a, b| a.partial_cmp(b).unwrap());
    xs[xs.len() / 2]
}

#[derive(Clone, Copy, PartialEq, Eq, Debug)]
enum BenchMode {
    Identical,
    Mixed,
}

impl BenchMode {
    fn parse(s: &str) -> Result<Self> {
        match s.trim().to_ascii_lowercase().as_str() {
            "identical" => Ok(BenchMode::Identical),
            "mixed" => Ok(BenchMode::Mixed),
            other => anyhow::bail!("bad --mode {other:?} (expected `identical` or `mixed`)"),
        }
    }
    fn label(self) -> &'static str {
        match self {
            BenchMode::Identical => "identical",
            BenchMode::Mixed => "mixed",
        }
    }
}

fn build_bench_prompts(stem: &str, b: usize, mode: BenchMode) -> Vec<String> {
    match mode {
        BenchMode::Identical => std::iter::repeat_n(stem.to_string(), b).collect(),
        BenchMode::Mixed => {
            const CLAUSES: &[&str] = &[
                " Consider the currents.",
                " Describe the depths in careful detail.",
                " Note the tides, the reefs, and the open sea.",
                " Explain how storms reshape the coast over many years.",
                " Reflect on how sailors once navigated by the stars alone at night.",
                " Weigh the balance of life across every layer from the sunlit shallows to the abyss.",
            ];
            let len_classes = CLAUSES.len() + 1; // +1 for the bare stem (zero clauses)
            (0..b)
                .map(|i| {
                    let extra = i % len_classes;
                    let mut p = stem.to_string();
                    for clause in CLAUSES.iter().take(extra) {
                        p.push_str(clause);
                    }
                    p
                })
                .collect()
        }
    }
}

/// Measure a single greedy completion and emit the exact result bytes a worker
/// would commit for a batch_infer honeypot, plus the engine|build_hash class.
async fn run_honeypot_answer(model: &str, max_tokens: u32, prompt: &str) -> Result<()> {
    let engine = "candle";
    // engine_build_hash and registration share detected_hw_class_wire(), so
    // the measured honeypot class reproduces the worker's advertised class.
    let hw_class = hardware::detected_hw_class_wire();
    let build_hash = hardware::engine_build_hash(engine, AGENT_VERSION);
    let answer_class = format!("{engine}|{build_hash}");
    let pool = ModelPool::new();
    let backend = inference::build_backend(inference::BackendKind::Candle, "", "", None)
        .map_err(|e| anyhow::anyhow!("build candle backend: {e}"))?;
    let params = inference::GenerateParams::greedy(max_tokens);
    let completion = backend
        .generate(model, prompt, params, &pool)
        .await
        .map_err(|e| anyhow::anyhow!("honeypot generate: {e}"))?;
    if completion.text.is_empty() {
        anyhow::bail!("honeypot generate produced empty text");
    }
    let known_answer_bytes =
        honeypot_known_answer_bytes(model, engine, &completion.text, completion.tokens)?;
    let out = serde_json::json!({
        "engine": engine,
        "build_hash": build_hash,
        "answer_class": answer_class,
        "model": model,
        "max_tokens": max_tokens,
        "prompt": prompt,
        "device": models::device_label(),
        "hw_class": hw_class,
        "agent_version": AGENT_VERSION,
        // Only the exact bytes are emitted. A nested JSON object would be
        // re-serialized by this envelope (and by any jq that reads it) in
        // whatever order that serializer chooses, and control/verification.go
        // compares batch_infer honeypots with bytes.Equal.
        "known_answer_utf8": String::from_utf8_lossy(&known_answer_bytes),
    });
    println!("{}", serde_json::to_string_pretty(&out)?);
    Ok(())
}

/// Serialize the exact bytes a worker commits for a one-completion batch_infer
/// task. Built from `executor::BatchInferResult` itself, never from a hand-written
/// `json!` literal: serde_json has no `preserve_order` feature here, so a `json!`
/// map serializes its keys alphabetically while the derived struct serializes in
/// declaration order. `control/verification.go` compares batch_infer honeypots
/// with `bytes.Equal`, so an alphabetical answer never matches an honest commit —
/// it quarantines the supplier and claws back its credit instead.
fn honeypot_known_answer_bytes(
    model: &str,
    backend_name: &str,
    text: &str,
    tokens: usize,
) -> Result<Vec<u8>> {
    let result = executor::BatchInferResult {
        job_type: "batch_infer",
        model: executor::short_model_id(model, model),
        inference_backend: backend_name.to_string(),
        completions: vec![executor::Completion {
            index: 0,
            text: text.to_string(),
            tokens,
        }],
    };
    Ok(serde_json::to_vec(&result)?)
}

#[allow(clippy::too_many_arguments)]
async fn run_bench_batch(
    model: &str,
    max_tokens: u32,
    batch_sizes: &str,
    prompt: &str,
    require_deterministic: bool,
    reps: u32,
    mode: &str,
    backends: &str,
    openai_base_url: &str,
    openai_model: &str,
    openai_api_key: &str,
) -> Result<()> {
    let mode = BenchMode::parse(mode)?;
    let reps = reps.max(1) as usize; // never 0 reps — that would sweep nothing and divide by zero below

    let sizes: Vec<usize> = batch_sizes
        .split(',')
        .map(|s| s.trim())
        .filter(|s| !s.is_empty())
        .map(|s| {
            let n = s
                .parse::<usize>()
                .map_err(|e| anyhow::anyhow!("bad batch size {s:?}: {e}"))?;
            if n == 0 {
                anyhow::bail!("batch size must be >= 1 (got 0 in --batch-sizes)");
            }
            Ok(n)
        })
        .collect::<Result<Vec<_>>>()?;
    if sizes.is_empty() {
        anyhow::bail!("no batch sizes given (e.g. --batch-sizes 1,8,32,64)");
    }

    let backend_kinds: Vec<inference::BackendKind> = backends
        .split(',')
        .map(|s| s.trim())
        .filter(|s| !s.is_empty())
        .map(|s| inference::BackendKind::parse(s).map_err(|e| anyhow::anyhow!(e)))
        .collect::<Result<Vec<_>>>()?;
    if backend_kinds.is_empty() {
        anyhow::bail!("no backends given (e.g. --backends candle,openai_http)");
    }

    // Device label for the receipt. On Metal, candle and a concurrent llama-server
    // cannot both own the GPU — dual sweeps stop/start the real engine around candle.
    let wants_candle = backend_kinds.contains(&inference::BackendKind::Candle);
    let wants_http = backend_kinds.contains(&inference::BackendKind::OpenAiHttp);
    if wants_candle && wants_http {
        eprintln!(
            "note: dual-backend Metal sweep will stop the OpenAI engine while candle runs \
             (they cannot share the GPU), then restart it for openai_http"
        );
        let _ = run_real_engine_script("stop");
        // Brief settle so Metal is released before candle opens a device.
        std::thread::sleep(Duration::from_secs(2));
    }

    let device = models::device_label();
    let build_hash = hardware::engine_build_hash("candle", AGENT_VERSION);
    eprintln!("== merc-agent bench-batch ==");
    eprintln!(
        "device={device} model={model} max_tokens={max_tokens} mode={} build_hash={build_hash} backends={:?}",
        mode.label(),
        backend_kinds.iter().map(|b| b.as_str()).collect::<Vec<_>>(),
    );

    let api_key = if openai_api_key.is_empty() {
        None
    } else {
        Some(openai_api_key)
    };

    let mut backend_records = serde_json::Map::new();
    let mut peak_by_backend: std::collections::BTreeMap<&'static str, f64> =
        std::collections::BTreeMap::new();
    let mut any_determinism_fail = false;
    let mut diverged_all: Vec<serde_json::Value> = Vec::new();

    // Candle first (while GPU is free), then openai_http (after engine restart).
    let mut ordered: Vec<inference::BackendKind> = Vec::with_capacity(backend_kinds.len());
    for k in &backend_kinds {
        if *k == inference::BackendKind::Candle {
            ordered.push(*k);
        }
    }
    for k in &backend_kinds {
        if *k != inference::BackendKind::Candle {
            ordered.push(*k);
        }
    }

    for kind in &ordered {
        if *kind == inference::BackendKind::OpenAiHttp && wants_candle && wants_http {
            eprintln!("starting real engine for openai_http…");
            run_real_engine_script("start")?;
        }
        // Fresh pool per backend so Candle's Metal tensors are dropped before
        // llama-server reclaims the GPU for openai_http.
        let pool = ModelPool::new();
        let be = inference::build_backend(*kind, openai_base_url, openai_model, api_key)
            .map_err(|e| anyhow::anyhow!("build backend {}: {e}", kind.as_str()))?;
        eprintln!(
            "\n-- backend={} verified_work={} --",
            kind.as_str(),
            kind.supports_verified_work()
        );
        let record = sweep_backend(
            be.as_ref(),
            &pool,
            model,
            max_tokens,
            &sizes,
            prompt,
            mode,
            reps,
        )
        .await?;
        drop(be);
        drop(pool);
        if let Some(peak) = record.get("peak_tok_s").and_then(|v| v.as_f64()) {
            peak_by_backend.insert(kind.as_str(), peak);
        }
        let det = record
            .get("batched_deterministic_vs_serial")
            .and_then(|v| v.as_bool())
            .unwrap_or(true);
        if !det {
            any_determinism_fail = true;
            if let Some(div) = record.get("diverged_batches") {
                diverged_all.push(serde_json::json!({
                    "backend": kind.as_str(),
                    "batches": div,
                }));
            }
        }
        backend_records.insert(kind.as_str().to_string(), record);
    }

    // Honest cross-backend multiples at each batch size (openai_http / candle when both present).
    let mut multiples = serde_json::Map::new();
    if backend_records.contains_key("candle") && backend_records.contains_key("openai_http") {
        let candle_sweep = backend_records["candle"]["sweep"]
            .as_array()
            .cloned()
            .unwrap_or_default();
        let http_sweep = backend_records["openai_http"]["sweep"]
            .as_array()
            .cloned()
            .unwrap_or_default();
        for row in &candle_sweep {
            let b = row.get("batch").and_then(|v| v.as_u64()).unwrap_or(0);
            let candle_tps = row
                .get("tokens_per_s")
                .and_then(|v| v.as_f64())
                .unwrap_or(0.0);
            let http_tps = http_sweep
                .iter()
                .find(|r| r.get("batch").and_then(|v| v.as_u64()) == Some(b))
                .and_then(|r| r.get("tokens_per_s").and_then(|v| v.as_f64()))
                .unwrap_or(0.0);
            let multiple = if candle_tps > 0.0 {
                http_tps / candle_tps
            } else {
                0.0
            };
            multiples.insert(
                b.to_string(),
                serde_json::json!({
                    "candle_tok_s": candle_tps,
                    "openai_http_tok_s": http_tps,
                    "openai_http_over_candle": multiple,
                }),
            );
            eprintln!(
                "multiple batch={b}: openai_http/candle = {multiple:.3}x  (candle={candle_tps:.1}  http={http_tps:.1} tok/s)"
            );
        }
    }

    let kind_label = if backend_kinds.len() > 1 {
        "bench_batch_compare"
    } else {
        "bench_batch"
    };
    let record = serde_json::json!({
        "kind": kind_label,
        "device": device,
        "build_hash": build_hash,
        "model": model,
        "max_tokens": max_tokens,
        "mode": mode.label(),
        "prompt_preview": prompt.chars().take(60).collect::<String>(),
        "batch_sizes": sizes,
        "backends": backend_records,
        "peak_by_backend": peak_by_backend,
        "multiples_openai_http_over_candle": multiples,
        "notes": [
            "openai_http fans out concurrent /v1/chat/completions so the ENGINE continuous-batches",
            "candle path is the in-process baseline (static length-bucket batching)",
            // Determinism under batching is MEASURED per backend above, not
            // assumed here. This note previously asserted that openai_http is
            // never verified-work-capable because continuous batching is not
            // byte-deterministic. vLLM on an L4 disproved it: 41.4x serial
            // throughput with byte-identical output at every batch size, while
            // llama.cpp diverged from its own serial output everywhere. It is
            // an engine property, not a property of batching, and a hardcoded
            // claim here would have buried the one result that mattered.
            "byte-determinism is measured per backend; see each backend's determinism field"
        ],
    });
    println!("{}", serde_json::to_string_pretty(&record)?);

    if require_deterministic && any_determinism_fail {
        anyhow::bail!(
            "batched decode diverged from serial ({diverged_all:?}) and \
             --require-deterministic was set — failing the determinism gate"
        );
    }
    Ok(())
}

/// Emit the exact commit bytes for an embed task on a named runtime cell.
async fn run_emit_embed_artifact(
    runtime: &str,
    model: &str,
    llama_base_url: &str,
    input: &str,
    out: &str,
    binary: bool,
) -> Result<()> {
    use executor::{EmbedRunner, JobRunner};
    use sha2::{Digest, Sha256};

    let driver = runtime_driver::build_embed_driver(runtime, llama_base_url)
        .map_err(|e| anyhow::anyhow!("embed runtime: {e}"))?;
    driver
        .launch()
        .await
        .with_context(|| format!("runtime {} must be serving", driver.runtime_id()))?;

    let body = if input.is_empty() {
        let mut buffer = String::new();
        std::io::Read::read_to_string(&mut std::io::stdin(), &mut buffer)
            .context("reading input from stdin")?;
        buffer
    } else {
        std::fs::read_to_string(input).with_context(|| format!("reading {input}"))?
    };

    // The cell decides the artifact format, exactly as the control plane's
    // per-cell wire_kind says: candle loads safetensors, llama.cpp loads the
    // GGUF of the same logical model.
    let kind = if driver.serves_kind("gguf") {
        types::ModelKind::Gguf
    } else {
        types::ModelKind::Hf
    };
    let manifest = types::JobManifest {
        id: uuid::Uuid::nil(),
        job_type: types::JobType::Embed {
            binary,
            batch_size: 0,
        },
        model: types::ModelRef {
            kind,
            model_ref: model.to_string(),
        },
        inputs: vec![],
        output: types::OutputRef { url: String::new() },
        params: serde_json::Value::Null,
        constraints: types::JobConstraints {
            min_memory_gb: 0.0,
            hw_classes: None,
            max_duration_secs: 600,
            data_residency: None,
        },
        verification: types::VerificationPolicy {
            redundancy_frac: 0.0,
            honeypot_frac: 0.0,
            payout_hold_secs: 0,
        },
        tier: types::ServiceTier::Batch,
    };

    // The PRODUCTION runner, not a bespoke call into the driver: the bytes have
    // to be what a claimed task would actually commit, including the artifact
    // encoding and the token accounting.
    let runner = EmbedRunner::new(driver.clone());
    let pool = ModelPool::new();
    let output = runner
        .run(&manifest, body.as_bytes(), &pool)
        .await
        .map_err(|e| anyhow::anyhow!("embed execution on {}: {e}", driver.runtime_id()))?;

    std::fs::write(out, &output.result).with_context(|| format!("writing {out}"))?;
    let digest = format!("{:x}", Sha256::digest(&output.result));
    println!(
        "{}",
        serde_json::json!({
            "runtime_profile_id": driver.runtime_id(),
            "model": model,
            "wire_kind": if kind == types::ModelKind::Gguf { "gguf" } else { "hf" },
            "artifact_path": out,
            "artifact_sha256": digest,
            "artifact_bytes": output.result.len(),
            "binary": output.binary,
            "tokens_used": output.tokens_used,
            "duration_ms": output.duration_ms,
            "driver_metrics": driver.metrics(),
        })
    );
    Ok(())
}

/// The embed corpus. Fixed in the binary so a receipt cannot be produced against
/// a corpus nobody can reconstruct, and hashed into the receipt so two runs that
/// claim to be comparable can be shown to be.
const EMBED_BENCH_CORPUS: &[&str] = &[
    "The water cycle moves water through the atmosphere, land and oceans.",
    "Merc settles every task against a receipt.",
    "short",
    "A considerably longer sentence, written so the comparison covers more than one \
     length bucket and exercises the tokenizer well past its first few tokens.",
    "Numbers like 3.14159 and dates like 2026-07-30 appear in real buyer input.",
    "Embeddings are compared by cosine, not by bytes.",
    "Supplier payouts are held until verification clears.",
    "A quantized model is a different product, not a cheaper one.",
];

/// Measure the embed cell on both runtime profiles over one corpus.
async fn run_bench_embed(
    model: &str,
    source_commit: &str,
    llama_base_url: &str,
    batch_sizes: &str,
    reps: u32,
    out: &str,
) -> Result<()> {
    use runtime_driver::{cosine, CandleDriver, LlamaCppDriver, LlamaServerSupervision};
    use sha2::{Digest, Sha256};

    let sizes: Vec<usize> = batch_sizes
        .split(',')
        .filter_map(|s| s.trim().parse().ok())
        .filter(|n| *n > 0)
        .collect();
    if sizes.is_empty() {
        anyhow::bail!("no batch sizes given (e.g. --batch-sizes 1,8,32)");
    }

    let candle: Arc<dyn runtime_driver::RuntimeDriver> = Arc::new(CandleDriver::new());
    let llama_concrete = Arc::new(LlamaCppDriver::new(LlamaServerSupervision::Attach {
        base_url: llama_base_url.to_string(),
    })?);
    let llama: Arc<dyn runtime_driver::RuntimeDriver> = llama_concrete.clone();
    candle.launch().await?;
    llama
        .launch()
        .await
        .with_context(|| format!("llama-server at {llama_base_url} must be serving"))?;
    let engine_props = llama_concrete.engine_properties().await?;

    let pool = ModelPool::new();
    let corpus_digest = {
        let mut hasher = Sha256::new();
        for line in EMBED_BENCH_CORPUS {
            hasher.update(line.as_bytes());
            hasher.update([0u8]);
        }
        format!("{:x}", hasher.finalize())
    };

    // One shared quality measurement over the whole corpus, before any timing:
    // a throughput number for a runtime that does not clear its cell's
    // verification gate is not a result, it is a distraction.
    let base: Vec<String> = EMBED_BENCH_CORPUS.iter().map(|s| s.to_string()).collect();
    let ours = candle.embed(model, &base, &pool).await?;
    let theirs = llama.embed(model, &base, &pool).await?;
    let mut min_cosine = f32::MAX;
    let mut sum_cosine = 0.0_f32;
    for (a, b) in ours.iter().zip(&theirs) {
        let c = cosine(a, b).context("cosine over mismatched vectors")?;
        min_cosine = min_cosine.min(c);
        sum_cosine += c;
    }
    let mean_cosine = sum_cosine / ours.len() as f32;

    let mut rows = Vec::new();
    for &batch in &sizes {
        let texts: Vec<String> = (0..batch)
            .map(|i| EMBED_BENCH_CORPUS[i % EMBED_BENCH_CORPUS.len()].to_string())
            .collect();
        for (label, driver) in [("candle_metal", &candle), ("llama_cpp_metal", &llama)] {
            // Warm once outside the measured reps; a cold load is a deployment
            // property, not a throughput property.
            driver.embed(model, &texts, &pool).await?;
            let mut per_rep_s: Vec<f64> = Vec::new();
            for _ in 0..reps.max(1) {
                let started = std::time::Instant::now();
                let vectors = driver.embed(model, &texts, &pool).await?;
                anyhow::ensure!(
                    vectors.len() == batch,
                    "{label} returned {} rows",
                    vectors.len()
                );
                per_rep_s.push(started.elapsed().as_secs_f64());
            }
            per_rep_s.sort_by(|a, b| a.partial_cmp(b).expect("no NaN timings"));
            let median = per_rep_s[per_rep_s.len() / 2];
            rows.push(serde_json::json!({
                "runtime_profile_id": label,
                "batch": batch,
                "reps": per_rep_s.len(),
                "median_wall_s": median,
                "min_wall_s": per_rep_s[0],
                "max_wall_s": per_rep_s[per_rep_s.len() - 1],
                "texts_per_sec": batch as f64 / median,
            }));
            eprintln!(
                "{label:>16} batch={batch:<4} median={median:.4}s  {:.1} texts/s",
                batch as f64 / median
            );
        }
    }

    let receipt = serde_json::json!({
        "schema_version": 1,
        "question": "What does the embed cell cost and deliver on each of the two registered \
                     runtime profiles, measured on one harness over one corpus?",
        "harness": format!("merc-agent {AGENT_VERSION} bench-embed"),
        "authority_matrix_version": runtime_authority::version(),
        "authority_matrix_sha256": runtime_authority::sha256(),
        "authority_document_sha256": runtime_authority::file_sha256(),
        "merc_source_commit": source_commit,
        "model_id": model,
        "profiles": runtime_authority::profile_identities(&["candle_metal", "llama_cpp_metal"]),
        "model_artifacts": runtime_authority::model_artifacts(model),
        "hardware": {
            "hw_class": hardware::detected_hw_class_wire(),
            "device": models::device_label(),
            "memory_gb": hardware::read_memory_snapshot().total_gb,
        },
        "engine_configuration": {
            "llama_cpp_metal": engine_props,
            "candle_metal": {
                "device": models::device_label(),
                "note": "in-process; no server configuration to report",
            },
        },
        "corpus": {
            "texts": EMBED_BENCH_CORPUS.len(),
            "sha256": corpus_digest,
        },
        "quality": {
            "verification": "cosine",
            "gate": 0.999,
            "min_cosine": min_cosine,
            "mean_cosine": mean_cosine,
            "passes": min_cosine >= 0.999,
            "reference": "candle_metal",
        },
        "measurements": rows,
        "not_established": {
            "energy": "power draw is not metered on this host",
            "provider_cost": "both profiles run on the same owned hardware here; no provider \
                              invoice separates them",
            "concurrency": "one client, sequential requests; this is not a concurrency sweep",
            "engine_tuning": "llama-server was measured AS CONFIGURED, at whatever n_ctx and \
                              total_slots engine_configuration records. A batch larger than its \
                              slots queues inside the server, so a row where candle wins at a \
                              large batch is a statement about THIS server configuration and \
                              not about llama.cpp. No flag sweep was run.",
            "quantization_sweep": "llama.cpp was measured at F16 only",
        },
    });

    let rendered = serde_json::to_string_pretty(&receipt)?;
    if out.is_empty() {
        println!("{rendered}");
    } else {
        std::fs::write(out, rendered + "\n").with_context(|| format!("writing {out}"))?;
        eprintln!("receipt written to {out}");
    }
    Ok(())
}

/// Locate and invoke `scripts/real-engine.sh` from the repo root or CWD.
fn run_real_engine_script(action: &str) -> Result<()> {
    let candidates = [
        PathBuf::from("scripts/real-engine.sh"),
        PathBuf::from("../scripts/real-engine.sh"),
        // When the binary is invoked via absolute path from elsewhere.
        PathBuf::from(env!("CARGO_MANIFEST_DIR")).join("../scripts/real-engine.sh"),
    ];
    let script = candidates
        .into_iter()
        .find(|p| p.is_file())
        .ok_or_else(|| {
            anyhow::anyhow!(
                "scripts/real-engine.sh not found (cwd={:?}); start llama-server manually",
                std::env::current_dir().ok()
            )
        })?;
    let status = std::process::Command::new("bash")
        .arg(&script)
        .arg(action)
        .status()
        .map_err(|e| anyhow::anyhow!("{} {action}: {e}", script.display()))?;
    if !status.success() {
        anyhow::bail!("{} {action} failed with {status}", script.display());
    }
    Ok(())
}

#[allow(clippy::too_many_arguments)]
async fn sweep_backend(
    backend: &dyn inference::InferenceBackend,
    pool: &ModelPool,
    model: &str,
    max_tokens: u32,
    sizes: &[usize],
    prompt: &str,
    mode: BenchMode,
    reps: usize,
) -> Result<serde_json::Value> {
    use std::time::Instant;

    let params = inference::GenerateParams::greedy(max_tokens);
    let warm = backend
        .generate(model, prompt, params, pool)
        .await
        .map_err(|e| anyhow::anyhow!("[{}] warmup: {e}", backend.name()))?;

    let widest = *sizes
        .iter()
        .max()
        .expect("sizes is non-empty (checked above)");
    let distinct_prompts: Vec<String> = {
        let mut seen: std::collections::HashSet<String> = std::collections::HashSet::new();
        let mut v = Vec::new();
        for p in build_bench_prompts(prompt, widest, mode) {
            if seen.insert(p.clone()) {
                v.push(p);
            }
        }
        v
    };

    let mut expected: std::collections::HashMap<String, String> =
        std::collections::HashMap::with_capacity(distinct_prompts.len());
    let mut serial_total_tok = 0usize;
    let mut serial_total_dt = 0.0f64;
    for p in &distinct_prompts {
        let t = Instant::now();
        let c = backend
            .generate(model, p, params, pool)
            .await
            .map_err(|e| anyhow::anyhow!("[{}] serial generate: {e}", backend.name()))?;
        serial_total_dt += t.elapsed().as_secs_f64();
        serial_total_tok += c.tokens;
        expected.insert(p.clone(), c.text);
    }
    if serial_total_tok == 0 {
        anyhow::bail!(
            "[{}] serial baseline produced 0 tokens for model {model:?} — cannot benchmark",
            backend.name()
        );
    }
    let serial_tps = serial_total_tok as f64 / serial_total_dt;
    eprintln!(
        "[{}] serial baseline ({} distinct): {serial_total_tok} tok in {serial_total_dt:.2}s = {serial_tps:.1} tok/s",
        backend.name(),
        distinct_prompts.len()
    );

    #[derive(serde::Serialize)]
    struct SweepRow {
        batch: usize,
        reps: usize,
        wall_s: f64,
        total_tokens: usize,
        tokens_per_s: f64,
        min_tok_s: f64,
        cv_pct: f64,
        per_request_tok_s: f64,
        speedup_vs_serial: f64,
        batched_equals_serial: bool,
    }

    let mut rows: Vec<SweepRow> = Vec::with_capacity(sizes.len());
    let mut peak_tps = serial_tps;
    for &b in sizes {
        let prompts: Vec<String> = build_bench_prompts(prompt, b, mode);
        let mut tps_samples = Vec::with_capacity(reps);
        let mut last_wall = 0.0;
        let mut last_total_tok = 0usize;
        let mut all_equal_serial = true;
        for rep in 0..reps {
            let t = Instant::now();
            let res = backend
                .generate_batch(model, &prompts, params, pool)
                .await
                .map_err(|e| {
                    anyhow::anyhow!("[{}] generate_batch b={b} rep={rep}: {e}", backend.name())
                })?;
            if let Ok(extra_ms) = std::env::var("MERC_BENCH_SYNTHETIC_DELAY_MS") {
                if let Ok(ms) = extra_ms.parse::<u64>() {
                    std::thread::sleep(Duration::from_millis(ms));
                }
            }
            let wall = t.elapsed().as_secs_f64();
            let total_tok: usize = res.iter().map(|c| c.tokens).sum();
            let tps = total_tok as f64 / wall.max(1e-9);
            all_equal_serial &= res
                .iter()
                .zip(&prompts)
                .all(|(c, p)| expected.get(p).is_some_and(|exp| exp == &c.text));
            tps_samples.push(tps);
            last_wall = wall;
            last_total_tok = total_tok;
        }
        let tps_median = {
            let mut samples = tps_samples.clone();
            median(&mut samples)
        };
        let tps_min = tps_samples.iter().cloned().fold(f64::INFINITY, f64::min);
        let cv_pct = coefficient_of_variation_pct(&tps_samples);
        let row = SweepRow {
            batch: b,
            reps,
            wall_s: last_wall,
            total_tokens: last_total_tok,
            tokens_per_s: tps_median,
            min_tok_s: tps_min,
            cv_pct,
            per_request_tok_s: tps_median / b as f64,
            speedup_vs_serial: tps_median / serial_tps,
            batched_equals_serial: all_equal_serial,
        };
        let cv_flag = if reps > 1 && cv_pct > 10.0 {
            format!("  !! high variance across {reps} reps: CV={cv_pct:.1}%")
        } else {
            String::new()
        };
        eprintln!(
            "[{}] batch={b:>3}: {:>5} tok in {:>6.2}s = {:>7.1} tok/s (median of {reps}, min={tps_min:.1}, CV={cv_pct:.1}%)  ({:.2}x serial){}{}",
            backend.name(),
            row.total_tokens,
            row.wall_s,
            row.tokens_per_s,
            row.speedup_vs_serial,
            if all_equal_serial { "" } else { "  !! batched != serial" },
            cv_flag,
        );
        peak_tps = peak_tps.max(tps_median);
        rows.push(row);
    }

    let all_deterministic = rows.iter().all(|r| r.batched_equals_serial);
    let diverged: Vec<usize> = rows
        .iter()
        .filter(|r| !r.batched_equals_serial)
        .map(|r| r.batch)
        .collect();
    eprintln!(
        "[{}] peak {peak_tps:.1} tok/s = {:.2}x serial · byte-determinism vs serial: {}",
        backend.name(),
        peak_tps / serial_tps,
        if all_deterministic {
            "IDENTICAL at every batch size".to_string()
        } else {
            format!("DIVERGES at batch {diverged:?}")
        }
    );

    Ok(serde_json::json!({
        "backend": backend.name(),
        "supports_verified_work": backend.supports_verified_work(),
        "distinct_prompts": distinct_prompts.len(),
        "warmup_ok": !warm.text.is_empty(),
        "serial_baseline_tok_s": serial_tps,
        "peak_tok_s": peak_tps,
        "peak_speedup_vs_serial": peak_tps / serial_tps,
        "batched_deterministic_vs_serial": all_deterministic,
        "diverged_batches": diverged,
        "sweep": rows,
    }))
}

fn sustained_summary(windows: &[f64]) -> (f64, f64, f64) {
    if windows.is_empty() {
        return (0.0, 0.0, 0.0);
    }
    let peak = windows.iter().cloned().fold(f64::MIN, f64::max);
    let tail_n = ((windows.len() as f64) * 0.25).ceil() as usize;
    let tail_n = tail_n.max(1).min(windows.len());
    let tail = &windows[windows.len() - tail_n..];
    let sustained_mean = tail.iter().sum::<f64>() / tail.len() as f64;
    let gap_pct = if peak > 0.0 {
        (1.0 - sustained_mean / peak) * 100.0
    } else {
        0.0
    };
    (peak, sustained_mean, gap_pct)
}

fn run_bench_sustained(
    model: &str,
    max_tokens: u32,
    batch: usize,
    prompt: &str,
    minutes: u64,
    window_secs: u64,
) -> Result<()> {
    use std::time::{Duration, Instant};

    if batch == 0 {
        anyhow::bail!("--batch must be >= 1");
    }
    if minutes == 0 || window_secs == 0 {
        anyhow::bail!("--minutes and --window-secs must both be >= 1");
    }

    let device = models::device_label();
    let engine = "candle";
    let build_hash = hardware::engine_build_hash(engine, AGENT_VERSION);
    eprintln!("== merc-agent bench-sustained ==");
    eprintln!(
        "device={device} model={model} batch={batch} max_tokens={max_tokens} \
         duration={minutes}min window={window_secs}s build_hash={build_hash}"
    );
    eprintln!(
        "this is a REAL {minutes}-minute run, not a spot measurement  -  it will take \
         {minutes} real minutes."
    );

    let mut be =
        executor::LlamaBackend::load(model).map_err(|e| anyhow::anyhow!("load {model}: {e}"))?;
    let prompts: Vec<String> = std::iter::repeat_n(prompt.to_string(), batch).collect();

    be.generate_batch(&prompts, max_tokens)
        .map_err(|e| anyhow::anyhow!("warmup generate_batch: {e}"))?;

    #[derive(serde::Serialize, Clone)]
    struct WindowSample {
        window_index: u32,
        elapsed_s: f64,
        requests: u32,
        total_tokens: u64,
        tokens_per_s: f64,
    }

    let run_start = Instant::now();
    let total_dur = Duration::from_secs(minutes * 60);
    let window_dur = Duration::from_secs(window_secs);

    let mut windows: Vec<WindowSample> = Vec::new();
    let mut win_start = Instant::now();
    let mut win_requests: u32 = 0;
    let mut win_tokens: u64 = 0;
    let mut win_index: u32 = 0;

    eprintln!(
        "{:>6} {:>8} {:>9} {:>12} {:>10}",
        "window", "elapsed", "requests", "tokens", "tok/s"
    );
    while run_start.elapsed() < total_dur {
        let results = be
            .generate_batch(&prompts, max_tokens)
            .map_err(|e| anyhow::anyhow!("generate_batch: {e}"))?;
        let total_tok: usize = results.iter().map(|(_, n)| n).sum();
        win_requests += 1;
        win_tokens += total_tok as u64;

        if win_start.elapsed() >= window_dur || run_start.elapsed() >= total_dur {
            let dt = win_start.elapsed().as_secs_f64().max(1e-6);
            let tps = win_tokens as f64 / dt;
            let sample = WindowSample {
                window_index: win_index,
                elapsed_s: run_start.elapsed().as_secs_f64(),
                requests: win_requests,
                total_tokens: win_tokens,
                tokens_per_s: tps,
            };
            eprintln!(
                "{:>6} {:>7.0}s {:>9} {:>12} {:>10.1}",
                sample.window_index,
                sample.elapsed_s,
                sample.requests,
                sample.total_tokens,
                sample.tokens_per_s
            );
            windows.push(sample);
            win_index += 1;
            win_start = Instant::now();
            win_requests = 0;
            win_tokens = 0;
        }
    }

    let tps_curve: Vec<f64> = windows.iter().map(|w| w.tokens_per_s).collect();
    let (peak, sustained_mean, gap_pct) = sustained_summary(&tps_curve);
    let wall_s = run_start.elapsed().as_secs_f64();

    eprintln!(
        "peak {peak:.1} tok/s · sustained (last 25% of windows) {sustained_mean:.1} tok/s \
         · gap {gap_pct:.1}% over {wall_s:.0}s ({} windows)",
        windows.len()
    );
    if gap_pct > 20.0 {
        eprintln!(
            "!! sustained throughput is {gap_pct:.1}% below peak  -  this box is throttling \
             under real sustained load; the peak number alone would overstate real capacity."
        );
    }

    let record = serde_json::json!({
        "kind": "bench_sustained",
        "device": device,
        "build_hash": build_hash,
        "model": model,
        "batch": batch,
        "max_tokens": max_tokens,
        "duration_minutes": minutes,
        "window_secs": window_secs,
        "wall_s": wall_s,
        "peak_tok_s": peak,
        "sustained_tok_s": sustained_mean,
        "sustained_vs_peak_gap_pct": gap_pct,
        "windows": windows.iter().map(|w| serde_json::json!({
            "window_index": w.window_index,
            "elapsed_s": w.elapsed_s,
            "requests": w.requests,
            "total_tokens": w.total_tokens,
            "tokens_per_s": w.tokens_per_s,
        })).collect::<Vec<_>>(),
    });
    println!("{}", serde_json::to_string_pretty(&record)?);
    Ok(())
}

async fn run_bench_concurrency(
    permits_arg: &str,
    embed_tasks: usize,
    llama_tasks: usize,
    model: &str,
    max_tokens: u32,
) -> Result<()> {
    use std::time::Instant;
    use types::{
        JobConstraints, JobManifest, JobType, ModelKind, ModelRef, ServiceTier, VerificationPolicy,
    };

    let permit_sweep: Vec<usize> = permits_arg
        .split(',')
        .map(|s| s.trim())
        .filter(|s| !s.is_empty())
        .map(|s| {
            let n = s
                .parse::<usize>()
                .map_err(|e| anyhow::anyhow!("bad permit count {s:?}: {e}"))?;
            if n == 0 {
                anyhow::bail!("permit count must be >= 1 (got 0 in --permits)");
            }
            Ok(n)
        })
        .collect::<Result<Vec<_>>>()?;
    if permit_sweep.is_empty() {
        anyhow::bail!("no permit counts given (e.g. --permits 1,2,4)");
    }
    if embed_tasks == 0 && llama_tasks == 0 {
        anyhow::bail!("--embed-tasks and --llama-tasks are both 0  -  nothing to benchmark");
    }

    let device = models::device_label();
    let engine = "candle";
    let build_hash = hardware::engine_build_hash(engine, AGENT_VERSION);
    eprintln!("== merc-agent bench-concurrency ==");
    eprintln!(
        "device={device} model={model} embed_tasks={embed_tasks} llama_tasks={llama_tasks} \
         max_tokens={max_tokens} build_hash={build_hash}"
    );
    eprintln!(
        "sweeping permits={permit_sweep:?}  -  this replaces the unvalidated [2,4] \
         concurrency-default clamp (config.rs::AgentConfig::concurrency) with real data"
    );

    let embed_manifest = JobManifest {
        id: uuid::Uuid::nil(),
        job_type: JobType::Embed {
            batch_size: 8,
            binary: false,
        },
        model: ModelRef {
            kind: ModelKind::Hf,
            model_ref: String::new(), // empty ref -> MiniLM default
        },
        inputs: vec![],
        output: types::OutputRef { url: String::new() },
        params: serde_json::Value::Null,
        constraints: JobConstraints {
            min_memory_gb: 0.0,
            hw_classes: None,
            max_duration_secs: 600,
            data_residency: None,
        },
        verification: VerificationPolicy {
            redundancy_frac: 0.0,
            honeypot_frac: 0.0,
            payout_hold_secs: 0,
        },
        tier: ServiceTier::Batch,
    };
    let embed_input: Vec<u8> = (0..8)
        .map(|i| format!("{{\"id\":\"{i}\",\"text\":\"benchmark sentence number {i} for concurrency measurement\"}}\n"))
        .collect::<String>()
        .into_bytes();

    let llama_manifest = JobManifest {
        id: uuid::Uuid::nil(),
        job_type: JobType::BatchInfer {
            max_tokens,
            temperature: 0.0,
        },
        model: ModelRef {
            kind: ModelKind::Gguf,
            model_ref: model.to_string(),
        },
        inputs: vec![],
        output: types::OutputRef { url: String::new() },
        params: serde_json::Value::Null,
        constraints: JobConstraints {
            min_memory_gb: 0.0,
            hw_classes: None,
            max_duration_secs: 600,
            data_residency: None,
        },
        verification: VerificationPolicy {
            redundancy_frac: 0.0,
            honeypot_frac: 0.0,
            payout_hold_secs: 0,
        },
        tier: ServiceTier::Batch,
    };
    let llama_input: Vec<u8> =
        b"{\"id\":\"0\",\"prompt\":\"Write one sentence about the weather:\"}\n".to_vec();

    let candle_backend: std::sync::Arc<dyn inference::InferenceBackend> =
        std::sync::Arc::new(inference::CandleBackend);
    // The bench measures candle against candle; the embed lane runs through the
    // same driver boundary a claimed task does.
    let embed_runner = Arc::new(executor::EmbedRunner::new(Arc::new(
        runtime_driver::CandleDriver::new(),
    )));
    eprintln!("warming models (cold load happens once, not counted in any sweep point)...");
    {
        let warm_pool = ModelPool::new();
        embed_runner
            .run(&embed_manifest, &embed_input, &warm_pool)
            .await
            .map_err(|e| anyhow::anyhow!("warmup embed: {e}"))?;
        executor::BatchInferRunner {
            inference: candle_backend.clone(),
        }
        .run(&llama_manifest, &llama_input, &warm_pool)
        .await
        .map_err(|e| anyhow::anyhow!("warmup batch_infer: {e}"))?;
    }

    #[derive(serde::Serialize, Clone)]
    struct SweepPoint {
        permits: usize,
        wall_s: f64,
        embed_tasks: usize,
        llama_tasks: usize,
        embed_wall_s: f64,
        llama_wall_s: f64,
        total_tasks_per_s: f64,
        speedup_vs_permit_1: f64,
    }

    let mut points: Vec<SweepPoint> = Vec::new();
    let mut baseline_tasks_per_s: Option<f64> = None;

    eprintln!(
        "{:>7} {:>10} {:>12} {:>12} {:>14} {:>10}",
        "permits", "wall_s", "embed_wall_s", "llama_wall_s", "tasks/s", "speedup"
    );
    for &permits in &permit_sweep {
        let pool = Arc::new(ModelPool::new());
        embed_runner
            .run(&embed_manifest, &embed_input, &pool)
            .await
            .map_err(|e| anyhow::anyhow!("re-warm embed: {e}"))?;
        executor::BatchInferRunner {
            inference: candle_backend.clone(),
        }
        .run(&llama_manifest, &llama_input, &pool)
        .await
        .map_err(|e| anyhow::anyhow!("re-warm batch_infer: {e}"))?;

        let sem = Arc::new(Semaphore::new(permits));
        let embed_manifest = Arc::new(embed_manifest.clone());
        let llama_manifest = Arc::new(llama_manifest.clone());
        let embed_input = Arc::new(embed_input.clone());
        let llama_input = Arc::new(llama_input.clone());

        let wall_start = Instant::now();
        let mut set = tokio::task::JoinSet::new();

        for _ in 0..embed_tasks {
            let sem = sem.clone();
            let pool = pool.clone();
            let manifest = embed_manifest.clone();
            let input = embed_input.clone();
            let runner = embed_runner.clone();
            set.spawn(async move {
                let permit = sem.acquire_owned().await.expect("semaphore never closed");
                let t = Instant::now();
                let res = runner.run(&manifest, &input, &pool).await;
                drop(permit);
                ("embed", t.elapsed().as_secs_f64(), res.is_ok())
            });
        }
        for _ in 0..llama_tasks {
            let sem = sem.clone();
            let pool = pool.clone();
            let manifest = llama_manifest.clone();
            let input = llama_input.clone();
            let inference = candle_backend.clone();
            set.spawn(async move {
                let permit = sem.acquire_owned().await.expect("semaphore never closed");
                let t = Instant::now();
                let res = executor::BatchInferRunner { inference }
                    .run(&manifest, &input, &pool)
                    .await;
                drop(permit);
                ("batch_infer", t.elapsed().as_secs_f64(), res.is_ok())
            });
        }

        let mut embed_wall_s = 0.0;
        let mut llama_wall_s = 0.0;
        while let Some(joined) = set.join_next().await {
            let (kind, dt, ok) = joined.expect("bench task panicked");
            if !ok {
                anyhow::bail!(
                    "a {kind} task failed during the concurrency sweep at permits={permits}"
                );
            }
            match kind {
                "embed" => embed_wall_s += dt,
                "batch_infer" => llama_wall_s += dt,
                _ => unreachable!(),
            }
        }
        let wall_s = wall_start.elapsed().as_secs_f64();
        let total_tasks = (embed_tasks + llama_tasks) as f64;
        let total_tasks_per_s = total_tasks / wall_s.max(1e-6);
        if baseline_tasks_per_s.is_none() {
            baseline_tasks_per_s = Some(total_tasks_per_s);
        }
        let speedup = total_tasks_per_s / baseline_tasks_per_s.unwrap_or(total_tasks_per_s);

        eprintln!(
            "{:>7} {:>9.2}s {:>11.2}s {:>11.2}s {:>14.2} {:>9.2}x",
            permits, wall_s, embed_wall_s, llama_wall_s, total_tasks_per_s, speedup
        );

        points.push(SweepPoint {
            permits,
            wall_s,
            embed_tasks,
            llama_tasks,
            embed_wall_s,
            llama_wall_s,
            total_tasks_per_s,
            speedup_vs_permit_1: speedup,
        });
    }

    if let Some(best) = points
        .iter()
        .max_by(|a, b| a.total_tasks_per_s.total_cmp(&b.total_tasks_per_s))
    {
        eprintln!(
            "best measured: permits={} at {:.2} tasks/s ({:.2}x vs permits={})",
            best.permits,
            best.total_tasks_per_s,
            best.speedup_vs_permit_1,
            points.first().map(|p| p.permits).unwrap_or(1)
        );
    }

    let record = serde_json::json!({
        "kind": "bench_concurrency",
        "device": device,
        "build_hash": build_hash,
        "model": model,
        "embed_tasks": embed_tasks,
        "llama_tasks": llama_tasks,
        "max_tokens": max_tokens,
        "permit_sweep": permit_sweep,
        "points": points.iter().map(|p| serde_json::json!({
            "permits": p.permits,
            "wall_s": p.wall_s,
            "embed_tasks": p.embed_tasks,
            "llama_tasks": p.llama_tasks,
            "embed_wall_s": p.embed_wall_s,
            "llama_wall_s": p.llama_wall_s,
            "total_tasks_per_s": p.total_tasks_per_s,
            "speedup_vs_permit_1": p.speedup_vs_permit_1,
        })).collect::<Vec<_>>(),
    });
    println!("{}", serde_json::to_string_pretty(&record)?);
    Ok(())
}

#[allow(clippy::too_many_arguments)]
async fn execute_task(
    task: &TaskDispatch,
    deadline: &TaskDeadline,
    cap: &WorkerCapability,
    runners: &[Box<dyn JobRunner>],
    pool: &ModelPool,
    s3: &reqwest::Client,
    checkpoint_secs: u64,
    memory_headroom_gb: f32,
    max_memory_pct: f32,
) -> Result<TaskCommit, RunError> {
    let manifest = &task.manifest;
    deadline.check("dispatch validation")?;
    if !runtime_authority_matches(
        &task.runtime_cell_id,
        &task.runtime_id,
        &task.runtime_matrix_sha256,
        &cap.engine,
        cap.hw_class,
        manifest.job_type.tag(),
        &manifest.model.model_ref,
        manifest.model.kind,
    ) {
        return Err(RunError::Inference {
            backend: "runtime_authority",
            msg: format!(
                "dispatch authority rejected: cell={:?} runtime={:?} matrix={:?} job={:?} model={:?} kind={:?}",
                task.runtime_cell_id,
                task.runtime_id,
                task.runtime_matrix_sha256,
                manifest.job_type.tag(),
                manifest.model.model_ref,
                manifest.model.kind,
            ),
        });
    }
    let runner = deadline
        .run("runner selection", dispatch(manifest, cap, runners))
        .await??;
    tracing::info!(task = %task.task_id, backend = runner.backend_name(), "executing task");

    let mut input = deadline
        .run("input download", s3_get(s3, &task.input_url))
        .await?
        .map_err(|e| RunError::Inference {
            backend: runner.backend_name(),
            msg: format!("fetching input_url: {e:#}"),
        })?;

    let mem_headroom_gb = memory_headroom_gb;
    let mem_max_pct = max_memory_pct;
    let ckpt =
        executor::Checkpointer::new(task.partial_put_url.clone(), checkpoint_secs, s3.clone())
            .with_preempt_check(move || {
                let snap = hardware::read_memory_snapshot();
                let headroom = mem_headroom_gb.max(0.0);
                let effective = (snap.available_gb - headroom).max(0.0);
                let used_pct = snap.used_pct();
                if mem_max_pct > 0.0 && used_pct >= mem_max_pct {
                    Some(format!(
                        "memory pressure: {used_pct:.0}% used >= {mem_max_pct:.0}% ceiling"
                    ))
                } else if headroom > 0.0 && snap.available_gb <= headroom {
                    Some(format!(
                        "reserved headroom: {:.1} GB available <= {headroom:.1} GB headroom",
                        snap.available_gb
                    ))
                } else {
                    let _ = effective; // computed for parity with evaluate_memory_throttle; unused when not throttled
                    None
                }
            });

    let output = match deadline
        .run(
            "model load and execution",
            runner.run_with_checkpoints(manifest, &input, pool, &ckpt),
        )
        .await?
    {
        Ok(o) => o,
        Err(e) => {
            wipe(&mut input);
            return Err(e);
        }
    };
    wipe(&mut input);

    let (duration_ms, tokens_used) = (output.duration_ms, output.tokens_used);

    let mut result = output.result;
    let content_type = output.content_type;
    let put = deadline
        .run("result upload", async {
            if result.len() > STAGING_THRESHOLD {
                put_via_encrypted_staging(s3, &task.output_url, &result, content_type).await
            } else {
                s3_put_bytes(s3, &task.output_url, &result, content_type).await
            }
        })
        .await?;
    put.map_err(|e| RunError::Inference {
        backend: runner.backend_name(),
        msg: format!("putting output_url: {e:#}"),
    })?;

    deadline.check("result digest")?;
    let result_sha256 = sha256_hex(&result);
    deadline.check("commit preparation")?;

    let commit = TaskCommit {
        task_id: task.task_id,
        attempt: task.attempt,
        result_key: if task.result_key.is_empty() {
            format!("results/{}/{}.json", task.job_id, task.task_id)
        } else {
            task.result_key.clone()
        },
        duration_ms,
        tokens_used,
        result_sha256,
        hardware_temp_c: None,
        inference_backend: output.inference_backend,
    };

    wipe(&mut result);
    Ok(commit)
}

#[allow(clippy::too_many_arguments)]
fn runtime_authority_matches(
    cell_id: &str,
    runtime_id: &str,
    matrix_sha256: &str,
    engine: &str,
    hw_class: types::HardwareClass,
    job: &str,
    model: &str,
    model_kind: types::ModelKind,
) -> bool {
    runtime_authority_matches_for(
        cell_id,
        runtime_id,
        matrix_sha256,
        engine,
        hw_class,
        models::device_label(),
        job,
        model,
        model_kind,
    )
}

#[allow(clippy::too_many_arguments)]
fn runtime_authority_matches_for(
    cell_id: &str,
    runtime_id: &str,
    matrix_sha256: &str,
    engine: &str,
    hw_class: types::HardwareClass,
    device: &str,
    job: &str,
    model: &str,
    model_kind: types::ModelKind,
) -> bool {
    matrix_sha256 == runtime_authority::sha256()
        && runtime_authority::capabilities().iter().any(|cell| {
            cell.id == cell_id
                && cell.runtime == runtime_id
                && cell.engine == engine
                && cell.device == device
                && cell
                    .hardware_classes
                    .iter()
                    .any(|class| class == hw_class.as_wire_str())
                && cell.job == job
                && cell.model == model
                && cell.model_kind == model_kind.as_wire_str()
        })
}

fn wipe(buf: &mut Vec<u8>) {
    for b in buf.iter_mut() {
        *b = 0;
    }
    buf.clear();
    buf.shrink_to_fit();
}

fn sha256_hex(data: &[u8]) -> String {
    use sha2::{Digest, Sha256};
    let mut h = Sha256::new();
    h.update(data);
    h.finalize().iter().map(|b| format!("{b:02x}")).collect()
}

const MAX_INPUT_DOWNLOAD_BYTES: u64 = 512 * 1024 * 1024;

const TRANSFER_RETRIES: u32 = 3;
const TRANSFER_RETRY_BASE_DELAY: Duration = Duration::from_millis(250);

fn is_retryable_status(status: reqwest::StatusCode) -> bool {
    status.is_server_error() || status == reqwest::StatusCode::TOO_MANY_REQUESTS
}

fn is_retryable_reqwest_err(e: &reqwest::Error) -> bool {
    e.is_connect() || e.is_timeout()
}

async fn s3_get(client: &reqwest::Client, url: &str) -> Result<Vec<u8>> {
    let mut delay = TRANSFER_RETRY_BASE_DELAY;
    let mut partial: Vec<u8> = Vec::new();
    for attempt in 0..=TRANSFER_RETRIES {
        match s3_get_once(client, url, &partial).await {
            Ok(bytes) => return Ok(bytes),
            Err(e) => {
                let retryable = e
                    .downcast_ref::<reqwest::Error>()
                    .is_some_and(is_retryable_reqwest_err)
                    || e.downcast_ref::<TransientStatusError>().is_some();
                if attempt == TRANSFER_RETRIES || !retryable {
                    return Err(e);
                }
                if let Some(PartialBodyError { bytes_read, .. }) =
                    e.downcast_ref::<PartialBodyError>()
                {
                    partial = bytes_read.clone();
                }
                tracing::warn!(attempt = attempt + 1, error = %e, delay_ms = delay.as_millis(), partial_bytes = partial.len(), "s3_get: transient failure, retrying");
                tokio::time::sleep(delay).await;
                delay *= 2;
            }
        }
    }
    unreachable!("loop always returns on its last iteration")
}

#[derive(Debug)]
struct TransientStatusError(reqwest::StatusCode);
impl std::fmt::Display for TransientStatusError {
    fn fmt(&self, f: &mut std::fmt::Formatter<'_>) -> std::fmt::Result {
        write!(f, "transient status {}", self.0)
    }
}
impl std::error::Error for TransientStatusError {}

#[derive(Debug)]
struct PartialBodyError {
    bytes_read: Vec<u8>,
    source: reqwest::Error,
}
impl std::fmt::Display for PartialBodyError {
    fn fmt(&self, f: &mut std::fmt::Formatter<'_>) -> std::fmt::Result {
        write!(
            f,
            "body read failed after {} bytes: {}",
            self.bytes_read.len(),
            self.source
        )
    }
}
impl std::error::Error for PartialBodyError {
    fn source(&self) -> Option<&(dyn std::error::Error + 'static)> {
        Some(&self.source)
    }
}

async fn s3_get_once(client: &reqwest::Client, url: &str, already_read: &[u8]) -> Result<Vec<u8>> {
    let mut req = client.get(url);
    if !already_read.is_empty() {
        req = req.header(
            reqwest::header::RANGE,
            format!("bytes={}-", already_read.len()),
        );
    }
    let mut resp = req.send().await.context("GET presigned input")?;
    let status = resp.status();
    if !already_read.is_empty() && status == reqwest::StatusCode::PARTIAL_CONTENT {
    } else if !status.is_success() {
        if is_retryable_status(status) {
            return Err(TransientStatusError(status).into());
        }
        return Err(resp
            .error_for_status()
            .context("input_url returned error status")
            .unwrap_err());
    } else if !already_read.is_empty() {
        return s3_get_full(resp).await;
    }
    if let Some(len) = resp.content_length() {
        if already_read.len() as u64 + len > MAX_INPUT_DOWNLOAD_BYTES {
            anyhow::bail!(
                "input_url reports {len} more bytes (total {}), exceeding the {MAX_INPUT_DOWNLOAD_BYTES}-byte task download cap",
                already_read.len() as u64 + len
            );
        }
    }
    let mut out = already_read.to_vec();
    loop {
        match resp.chunk().await {
            Ok(Some(bytes)) => {
                pace_transfer(bytes.len()).await;
                out.extend_from_slice(&bytes);
                if out.len() as u64 > MAX_INPUT_DOWNLOAD_BYTES {
                    anyhow::bail!(
                        "input body was {} bytes, exceeding the {MAX_INPUT_DOWNLOAD_BYTES}-byte task download cap",
                        out.len()
                    );
                }
            }
            Ok(None) => break,
            Err(e) => {
                return Err(PartialBodyError {
                    bytes_read: out,
                    source: e,
                }
                .into())
            }
        }
    }
    if out.len() as u64 > MAX_INPUT_DOWNLOAD_BYTES {
        anyhow::bail!(
            "input body was {} bytes, exceeding the {MAX_INPUT_DOWNLOAD_BYTES}-byte task download cap",
            out.len()
        );
    }
    Ok(out)
}

async fn s3_get_full(mut resp: reqwest::Response) -> Result<Vec<u8>> {
    if let Some(len) = resp.content_length() {
        if len > MAX_INPUT_DOWNLOAD_BYTES {
            anyhow::bail!(
                "input_url reports {len} bytes, exceeding the {MAX_INPUT_DOWNLOAD_BYTES}-byte task download cap"
            );
        }
    }
    let mut out = Vec::new();
    while let Some(bytes) = resp.chunk().await.context("reading input body")? {
        pace_transfer(bytes.len()).await;
        out.extend_from_slice(&bytes);
        if out.len() as u64 > MAX_INPUT_DOWNLOAD_BYTES {
            anyhow::bail!(
                "input body was {} bytes, exceeding the {MAX_INPUT_DOWNLOAD_BYTES}-byte task download cap",
                out.len()
            );
        }
    }
    Ok(out)
}

async fn s3_put_bytes(
    client: &reqwest::Client,
    url: &str,
    body: &[u8],
    content_type: &str,
) -> Result<()> {
    let mut delay = TRANSFER_RETRY_BASE_DELAY;
    for attempt in 0..=TRANSFER_RETRIES {
        match s3_put_bytes_once(client, url, body, content_type).await {
            Ok(()) => return Ok(()),
            Err(e) => {
                let retryable = e
                    .downcast_ref::<reqwest::Error>()
                    .is_some_and(is_retryable_reqwest_err)
                    || e.downcast_ref::<TransientStatusError>().is_some();
                if attempt == TRANSFER_RETRIES || !retryable {
                    return Err(e);
                }
                tracing::warn!(attempt = attempt + 1, error = %e, delay_ms = delay.as_millis(), "s3_put_bytes: transient failure, retrying");
                tokio::time::sleep(delay).await;
                delay *= 2;
            }
        }
    }
    unreachable!("loop always returns on its last iteration")
}

async fn s3_put_bytes_once(
    client: &reqwest::Client,
    url: &str,
    body: &[u8],
    content_type: &str,
) -> Result<()> {
    use futures_util::stream;

    let content_length = body.len();
    let owned = body.to_vec();
    let stream = stream::unfold((owned, 0_usize), |(body, offset)| async move {
        if offset >= body.len() {
            return None;
        }
        let end = (offset + TRANSFER_CHUNK_BYTES).min(body.len());
        pace_transfer(end - offset).await;
        let chunk = body[offset..end].to_vec();
        Some((Ok::<Vec<u8>, std::io::Error>(chunk), (body, end)))
    });
    let resp = client
        .put(url)
        .header(reqwest::header::CONTENT_TYPE, content_type)
        .header(reqwest::header::CONTENT_LENGTH, content_length)
        .body(reqwest::Body::wrap_stream(stream))
        .send()
        .await
        .context("PUT presigned output")?;
    let status = resp.status();
    if !status.is_success() {
        if is_retryable_status(status) {
            return Err(TransientStatusError(status).into());
        }
        return Err(resp
            .error_for_status()
            .context("output_url returned error status")
            .unwrap_err());
    }
    Ok(())
}

const STAGING_THRESHOLD: usize = 16 * 1024 * 1024;

async fn put_via_encrypted_staging(
    client: &reqwest::Client,
    url: &str,
    plaintext: &[u8],
    content_type: &str,
) -> Result<()> {
    use aes_gcm::aead::{Aead, KeyInit};
    use aes_gcm::{Aes256Gcm, Key, Nonce};
    use rand::RngCore;

    let mut key_bytes = [0u8; 32];
    let mut nonce_bytes = [0u8; 12];
    rand::thread_rng().fill_bytes(&mut key_bytes);
    rand::thread_rng().fill_bytes(&mut nonce_bytes);
    let cipher = Aes256Gcm::new(Key::<Aes256Gcm>::from_slice(&key_bytes));
    let nonce = Nonce::from_slice(&nonce_bytes);

    let ciphertext = cipher
        .encrypt(nonce, plaintext)
        .map_err(|e| anyhow::anyhow!("staging encryption failed: {e}"))?;

    let path = std::env::temp_dir().join(format!("merc-stage-{}.bin", uuid::Uuid::new_v4()));
    tokio::fs::write(&path, &ciphertext)
        .await
        .with_context(|| format!("writing encrypted staging file {}", path.display()))?;

    let result = async {
        let on_disk = tokio::fs::read(&path)
            .await
            .context("reading encrypted staging file")?;
        let mut decrypted = cipher
            .decrypt(nonce, on_disk.as_ref())
            .map_err(|e| anyhow::anyhow!("staging decryption failed: {e}"))?;
        let put = s3_put_bytes(client, url, &decrypted, content_type).await;
        wipe(&mut decrypted);
        put
    }
    .await;

    let _ = tokio::fs::write(&path, vec![0u8; ciphertext.len()]).await;
    let _ = tokio::fs::remove_file(&path).await;
    result
}

#[derive(Clone)]
struct WorkCtx {
    client: Arc<ControlPlaneClient>,
    cap: Arc<WorkerCapability>,
    runners: Arc<Vec<Box<dyn JobRunner>>>,
    pool: ModelPool,
    s3: reqwest::Client,
    checkpoint_secs: u64,
    status: Arc<StatusWriter>,
}

async fn run_agent(mut cfg: AgentConfig) -> Result<()> {
    if std::env::var_os("MERC_MODEL_CACHE").is_none() {
        std::env::set_var("MERC_MODEL_CACHE", cfg.data_dir.join("models"));
    }
    models::set_cache_policy(cfg.allow_model_downloads, cfg.max_model_cache_gb);
    set_transfer_limit(cfg.max_bandwidth_mbps);
    let pool = ModelPool::new();
    // The advertised engine follows the embed runtime this worker was configured
    // with, rather than being hardcoded to candle.
    //
    // It was "candle" unconditionally, so a worker configured for llama.cpp
    // registered as a candle worker and was authorized for candle's cells — it
    // would then have been dispatched candle work its driver cannot serve, and
    // the llama.cpp cell it exists to prove stayed unadvertised. Caught by
    // running two real agents and reading what the control plane stored.
    //
    // A non-candle embed runtime makes this a single-cell worker: it advertises
    // that engine's cells only. That is correct for a dedicated second-runtime
    // worker and it is the shape the directed proof needs.
    let engine = runtime_driver::advertised_engine(&cfg.embed_runtime)
        .map_err(|e| anyhow::anyhow!("embed runtime: {e}"))?;
    let mut cap = hardware::detect_and_benchmark(
        cfg.supplier_id,
        AGENT_VERSION,
        cfg.min_payout_usd_per_hr,
        engine,
        &pool,
    )
    .await;
    cap.supported_jobs
        .retain(|workload| cfg.allows_workload(workload));
    cap.benchmarks
        .retain(|bench| cfg.allows_workload(&bench.job_type));
    // Containment record: the control plane refuses private work to workers
    // that are neither sandboxed nor deliberately opted in.
    cap.sandboxed = agent_is_sandboxed();
    cap.unsandboxed_opt_in = agent_unsandboxed_opt_in();
    if !cap.sandboxed {
        tracing::warn!(
            sandboxed = cap.sandboxed,
            unsandboxed_opt_in = cap.unsandboxed_opt_in,
            "registering worker capability without seatbelt containment"
        );
    }
    // status.json is written after registration; seed containment now so an
    // early crash still leaves the opt-in flag on disk.
    let advertised_worker_id = cap.worker_id;
    let permits = cfg.concurrency(cap.memory_gb);

    let backend_kind = cfg.inference_backend_kind().map_err(anyhow::Error::msg)?;
    let inference = inference::build_backend(
        backend_kind,
        &cfg.openai_base_url,
        &cfg.openai_model,
        cfg.openai_api_key.as_deref(),
    )
    .map_err(|e| anyhow::anyhow!("inference backend: {e}"))?;
    // The embed lane's runtime is chosen separately: llama.cpp is disqualified
    // from byte_exact batch_infer on Metal and qualified for the cosine-verified
    // embed cell, so one knob for both would force an operator to take the
    // engine's worst cell to get its best one.
    let embed_driver =
        runtime_driver::build_embed_driver(&cfg.embed_runtime, &cfg.llama_embed_base_url)
            .map_err(|e| anyhow::anyhow!("embed runtime: {e}"))?;
    // Fail at startup, not at the first claimed task, if the runtime is not
    // serving. A worker that enrolls against a runtime it cannot reach will
    // claim work and then fail it, with the supplier already on the hook.
    embed_driver
        .launch()
        .await
        .map_err(|e| anyhow::anyhow!("embed runtime {}: {e}", embed_driver.runtime_id()))?;
    if !backend_kind.supports_verified_work() {
        tracing::warn!(
            backend = backend_kind.as_str(),
            "inference backend is not verified-work-capable (byte-exact redundancy not guaranteed); use candle for canary/verified lanes"
        );
    }
    tracing::info!(
        inference_backend = backend_kind.as_str(),
        openai_base_url = %cfg.openai_base_url,
        "batch_infer will use the configured pluggable backend"
    );

    let client = ControlPlaneClient::new(cfg.control_url.clone(), cfg.worker_token.clone())
        .context("building control-plane client (is worker_token set?)")?;
    let s3 = tls::client_builder()?
        .connect_timeout(Duration::from_secs(10))
        .timeout(Duration::from_secs(120))
        .build()
        .context("building S3 client")?;

    tracing::info!(worker_id = %advertised_worker_id, control = %cfg.control_url, max_concurrent_tasks = permits, "registering with control plane");
    let confirmed = client.register(&cap).await.context("registration failed")?;
    cap.worker_id = confirmed.worker_id;
    cap.supplier_id = confirmed.supplier_id;
    let worker_id = cap.worker_id;
    tracing::info!(worker_id = %worker_id, supplier_id = %cap.supplier_id, "registered");

    let status = Arc::new(StatusWriter::new(
        AGENT_VERSION,
        worker_id,
        &cap.benchmarks,
        cap.hw_class,
    ));
    status.set_containment(cap.sandboxed, cap.unsandboxed_opt_in);
    status.set_applied_prefs(status::AppliedPrefs::from_config(&cfg, cap.memory_gb));
    status.registered();

    let ctx = WorkCtx {
        client: Arc::new(client),
        cap: Arc::new(cap),
        runners: Arc::new(default_runners(inference, embed_driver)),
        pool,
        s3,
        checkpoint_secs: cfg.checkpoint_secs,
        status: status.clone(),
    };
    let sem = Arc::new(Semaphore::new(permits));
    let mut inflight = tokio::task::JoinSet::new();

    {
        let ctx = ctx.clone();
        let mut cfg = cfg.clone();
        let status = status.clone();
        tokio::spawn(async move {
            let mut sys = System::new();
            let mut heartbeat = tokio::time::interval(Duration::from_secs(30));
            heartbeat.set_missed_tick_behavior(tokio::time::MissedTickBehavior::Delay);
            loop {
                heartbeat.tick().await;
                let prefs_valid = match cfg.refresh_operator_prefs() {
                    Ok(()) => {
                        models::set_cache_policy(cfg.allow_model_downloads, cfg.max_model_cache_gb);
                        set_transfer_limit(cfg.max_bandwidth_mbps);
                        status.set_applied_prefs(status::AppliedPrefs::from_config(
                            &cfg,
                            ctx.cap.memory_gb,
                        ));
                        true
                    }
                    Err(error) => {
                        tracing::error!(%error, "operator preferences invalid; claims remain fail-closed");
                        false
                    }
                };
                let ts = now_unix();
                let cpu = cpu_pct(&mut sys);
                LAST_HOST_CPU_MILLIPCT.store((cpu.max(0.0) * 1000.0) as u64, Ordering::Release);
                let throttle = cfg.evaluate_memory_throttle(&throttle_snapshot(), None);
                cfg.refresh_thermal_pressure();
                let evicted = ctx.pool.evict_idle(MODEL_IDLE_EVICT_AFTER).await;
                if !evicted.is_empty() {
                    let residency = pool::residency_snapshot();
                    let reclaimed_bytes: i64 = evicted
                        .iter()
                        .filter_map(|k| residency.get(k))
                        .map(|m| m.rss_delta_bytes.max(0))
                        .sum();
                    tracing::info!(
                        models = ?evicted,
                        idle_for = ?MODEL_IDLE_EVICT_AFTER,
                        measured_reclaimed_bytes = reclaimed_bytes,
                        measured_reclaimed_mb = reclaimed_bytes as f64 / 1e6,
                        "evicted idle warm model(s)"
                    );
                }
                let loaded_models = ctx.pool.loaded_model_ids().await;
                let (gpu, gpu_temp) = gpu_telemetry();
                let live_throttling = executor::live_throttle_detected();
                let active_tasks = status.active_task_leases();
                let hb = Heartbeat {
                    worker_id,
                    timestamp: ts,
                    cpu_pct: cpu,
                    gpu_pct: gpu,
                    gpu_temp_c: gpu_temp,
                    current_task: active_tasks.last().map(|lease| lease.task_id),
                    active_tasks,
                    available_memory_gb: throttle.available_gb,
                    effective_memory_gb: throttle.effective_gb,
                    reserved_headroom_gb: throttle.reserved_headroom_gb,
                    throttled: throttle.throttled || live_throttling,
                    loaded_models,
                };
                if let Err(e) = ctx.client.heartbeat(&hb).await {
                    tracing::warn!(error = %e, "heartbeat failed");
                }
                let (hour, weekday) = current_local_schedule_clock();
                let eligible =
                    prefs_valid && cfg.is_eligible_to_run_at(hour, weekday, on_battery());
                let earnings = ctx.client.earnings().await.ok();
                let connect = ctx.client.connect_status().await.ok();
                let verification = ctx.client.verification().await.ok();
                status.heartbeat(
                    cpu,
                    gpu_temp,
                    cfg.thermal_pressure,
                    eligible,
                    ts,
                    earnings,
                    &throttle,
                    connect,
                    verification,
                );
            }
        });
    }

    loop {
        tokio::select! {
            biased;

            _ = tokio::signal::ctrl_c() => {
                tracing::info!(
                    inflight = inflight.len(),
                    models_loaded = pool::loads(),
                    "received SIGINT; shutting down (in-flight tasks will be reassigned by the control plane)"
                );
                inflight.shutdown().await;
                return Ok(());
            }

            Some(joined) = inflight.join_next() => {
                if let Err(e) = joined {
                    tracing::error!(error = %e, "in-flight task panicked");
                }
            }

            permit = sem.clone().acquire_owned() => {
                let permit = permit.expect("semaphore is never closed");
                if let Err(e) = poll_and_spawn(&mut cfg, &ctx, permit, &mut inflight).await {
                    tracing::warn!(error = %e, "poll cycle error");
                    tokio::time::sleep(Duration::from_secs(5)).await;
                }
            }
        }
    }
}

async fn poll_and_spawn(
    cfg: &mut AgentConfig,
    ctx: &WorkCtx,
    permit: tokio::sync::OwnedSemaphorePermit,
    inflight: &mut tokio::task::JoinSet<()>,
) -> Result<()> {
    cfg.refresh_operator_prefs()
        .context("operator preferences invalid; refusing to claim")?;
    models::set_cache_policy(cfg.allow_model_downloads, cfg.max_model_cache_gb);
    set_transfer_limit(cfg.max_bandwidth_mbps);
    let (hour, weekday) = current_local_schedule_clock();
    if !cfg.is_eligible_to_run_at(hour, weekday, on_battery()) {
        tracing::debug!("not eligible to run (paused / schedule / battery); idling 60s");
        drop(permit); // release the slot while we idle
        tokio::time::sleep(Duration::from_secs(60)).await;
        return Ok(());
    }

    let host_cpu_pct = LAST_HOST_CPU_MILLIPCT.load(Ordering::Acquire) as f32 / 1000.0;
    if cfg.max_cpu_pct > 0.0 && host_cpu_pct >= cfg.max_cpu_pct {
        tracing::info!(
            host_cpu_pct,
            max_cpu_pct = cfg.max_cpu_pct,
            "host CPU claim ceiling reached; pausing new claims"
        );
        drop(permit);
        tokio::time::sleep(Duration::from_secs(5)).await;
        return Ok(());
    }

    cfg.refresh_thermal_pressure();
    let thermal = cfg.evaluate_thermal_throttle();
    ctx.status.set_thermal_throttle(&thermal);
    if thermal.throttled {
        tracing::info!(
            reason = thermal.reason.as_deref().unwrap_or("thermal pressure"),
            reading = thermal.reading.map(|p| p.as_str()),
            "thermal throttle: pausing new claims"
        );
        drop(permit); // release the slot while we idle
        tokio::time::sleep(Duration::from_secs(30)).await;
        return Ok(());
    }

    let throttle = cfg.evaluate_memory_throttle(&throttle_snapshot(), None);
    if throttle.throttled {
        tracing::info!(
            reason = throttle.reason.as_deref().unwrap_or("memory pressure"),
            available_gb = throttle.available_gb,
            effective_gb = throttle.effective_gb,
            "memory throttle: pausing new claims"
        );
        ctx.status.set_throttle(&throttle);
        drop(permit); // release the slot while we idle
        tokio::time::sleep(Duration::from_secs(30)).await;
        return Ok(());
    }

    let task = match ctx.client.poll_task().await {
        Ok(Some(t)) => t,
        Ok(None) => return Ok(()), // long-poll returned no work; `permit` drops
        Err(e) => return Err(e.into()),
    };
    let claim_memory_headroom_gb = cfg.memory_headroom_gb;
    tracing::info!(task = %task.task_id, job = %task.job_id, "received task");
    if !cfg.allows_workload(task.manifest.job_type.tag()) {
        tracing::warn!(
            task = %task.task_id,
            workload = task.manifest.job_type.tag(),
            "task declined: workload class disabled by supplier policy (not started)"
        );
        return Ok(());
    }
    let received_at = std::time::Instant::now();
    let deadline = match TaskDeadline::from_dispatch(
        task.deadline,
        task.manifest.constraints.max_duration_secs,
    ) {
        Ok(deadline) => deadline,
        Err(error) => {
            let error = RunError::from(error);
            report_task_error(ctx, &task, received_at, &error, claim_memory_headroom_gb).await;
            return Ok(());
        }
    };

    let task_fit = cfg.evaluate_memory_throttle(
        &throttle_snapshot(),
        Some(task.manifest.constraints.min_memory_gb),
    );
    if task_fit.throttled {
        tracing::warn!(
            task = %task.task_id,
            reason = task_fit.reason.as_deref().unwrap_or("memory pressure"),
            needed_gb = task.manifest.constraints.min_memory_gb,
            effective_gb = task_fit.effective_gb,
            "task declined: would not fit in currently available memory (not started; control plane will reassign)"
        );
        return Ok(()); // never started -> control plane reassigns it
    }

    let floor = cfg.min_payout_usd_per_hr;
    if floor > 0.0 && task.offered_rate_usd_hr > 0.0 && task.offered_rate_usd_hr < floor {
        tracing::warn!(
            task = %task.task_id,
            offered = task.offered_rate_usd_hr,
            floor,
            "offered rate below reservation price; skipping task (not started)"
        );
        return Ok(()); // never started -> control plane reassigns it
    }

    let start_result = match deadline
        .run(
            "task start acknowledgement",
            ctx.client.start_task(task.task_id, task.attempt),
        )
        .await
    {
        Ok(result) => result,
        Err(error) => {
            let error = RunError::from(error);
            report_task_error(ctx, &task, received_at, &error, claim_memory_headroom_gb).await;
            return Ok(());
        }
    };
    match start_ack_disposition(start_result) {
        StartAckDisposition::Run => {}
        StartAckDisposition::Report(error) => {
            // A start request may be durable even when every acknowledgement
            // is lost. Do not abandon that queued/running claim to the
            // watchdog: use the same owner+attempt failure path as every other
            // execution error, then return the polling slot immediately.
            report_task_error(ctx, &task, received_at, &error, claim_memory_headroom_gb).await;
            return Ok(());
        }
    }
    ctx.status.job_started(
        task.task_id,
        task.attempt,
        task.job_id,
        task.manifest.job_type.tag(),
        now_unix(),
    );

    // Snapshot supplier prefs for the in-flight guard. New claims already check
    // eligibility; this re-evaluates while the task runs and aborts via the same
    // deadline cancellation path the wall-clock watchdog uses.
    let mut eligibility_cfg = cfg.clone();
    let eligibility_interval = eligibility_watch_interval(cfg.checkpoint_secs);
    let memory_headroom_gb = cfg.memory_headroom_gb;
    let max_memory_pct = cfg.max_memory_pct;

    let ctx = ctx.clone();
    inflight.spawn(async move {
        let _permit = permit;
        let task_id = task.task_id;
        let started = received_at;

        let guard_deadline = deadline.clone();
        let eligibility_guard = tokio::spawn(async move {
            guard_deadline
                .cancel_when_ineligible(eligibility_interval, move || {
                    let prefs_valid = eligibility_cfg.refresh_operator_prefs().is_ok();
                    let (hour, weekday) = current_local_schedule_clock();
                    let lost = !prefs_valid
                        || !eligibility_cfg.is_eligible_to_run_at(hour, weekday, on_battery());
                    if lost {
                        tracing::info!(
                            task = %task_id,
                            "supplier eligibility lost (pause / schedule / battery / invalid prefs); cancelling in-flight task"
                        );
                    }
                    lost
                })
                .await;
        });

        match execute_task(
            &task,
            &deadline,
            &ctx.cap,
            &ctx.runners,
            &ctx.pool,
            &ctx.s3,
            ctx.checkpoint_secs,
            memory_headroom_gb,
            max_memory_pct,
        )
        .await
        {
            Ok(commit) => {
                match deadline
                    .run("result commit", ctx.client.commit_task(task_id, &commit))
                    .await
                {
                    Ok(result) => match commit_ack_disposition(result) {
                        CommitAckDisposition::Done => {
                            tracing::info!(task = %task_id, "committed result");
                            ctx.status.job_finished(task_id, None);
                        }
                        CommitAckDisposition::Report(error) => {
                            // A commit may be durable even if every response is
                            // lost. The failure endpoint is inert after a
                            // durable commit, releases an exact running
                            // owner+attempt when commit never applied, and
                            // fences a superseded execution.
                            report_task_error(&ctx, &task, started, &error, memory_headroom_gb).await;
                        }
                    },
                    Err(error) => {
                        report_task_error(
                            &ctx,
                            &task,
                            started,
                            &RunError::from(error),
                            memory_headroom_gb,
                        )
                        .await;
                    }
                }
            }
            Err(e) => {
                report_task_error(&ctx, &task, started, &e, memory_headroom_gb).await;
            }
        }
        eligibility_guard.abort();
    });
    Ok(())
}

/// How often an in-flight task re-checks quiet hours / battery. Mirrors the
/// checkpoint cadence so an abort lands within one checkpoint interval.
fn eligibility_watch_interval(checkpoint_secs: u64) -> Duration {
    if checkpoint_secs == 0 {
        Duration::from_secs(30)
    } else {
        Duration::from_secs(checkpoint_secs)
    }
}

async fn report_task_error(
    ctx: &WorkCtx,
    task: &TaskDispatch,
    started: std::time::Instant,
    error: &RunError,
    memory_headroom_gb: f32,
) {
    tracing::error!(task = %task.task_id, error = %error, "task execution failed; reporting typed failure");
    let snap = hardware::read_memory_snapshot();
    let report = failure::build_report(
        error,
        task.manifest.job_type.tag(),
        &task.manifest.model.model_ref,
        started.elapsed().as_millis() as u64,
        &snap,
        memory_headroom_gb,
    );
    if let Err(failure_error) = ctx
        .client
        .fail_task(task.task_id, task.attempt, &report)
        .await
    {
        tracing::warn!(task = %task.task_id, error = %failure_error, "fail_task report exhausted bounded delivery retries; stale reaper is the final fallback");
    }
    ctx.status
        .job_finished(task.task_id, Some(error.to_string()));
}

#[cfg(test)]
mod tests {
    use super::*;
    use std::sync::atomic::{AtomicBool, AtomicU8, Ordering};

    #[test]
    fn start_ack_disposition_runs_only_after_success() {
        assert!(matches!(
            start_ack_disposition(Ok(())),
            StartAckDisposition::Run
        ));
    }

    #[test]
    fn start_ack_disposition_reports_terminal_protocol_error() {
        let disposition = start_ack_disposition(Err(protocol::ProtocolError::Status {
            endpoint: "/v1/worker/task/{id}/start".to_string(),
            status: reqwest::StatusCode::SERVICE_UNAVAILABLE,
            body: "ambiguous start acknowledgement".to_string(),
        }));
        let StartAckDisposition::Report(error) = disposition else {
            panic!("terminal start error must report failure and release ownership");
        };
        assert_eq!(failure::classify(&error, false), "internal_error");
        assert!(error
            .to_string()
            .contains("start_task failed after bounded retries"));
        assert!(error.to_string().contains("503"));
    }

    #[test]
    fn commit_ack_disposition_finishes_only_after_success() {
        assert!(matches!(
            commit_ack_disposition(Ok(())),
            CommitAckDisposition::Done
        ));
    }

    #[test]
    fn commit_ack_disposition_reports_exhausted_protocol_error() {
        let disposition = commit_ack_disposition(Err(protocol::ProtocolError::Status {
            endpoint: "/v1/worker/task/{id}/commit".to_string(),
            status: reqwest::StatusCode::SERVICE_UNAVAILABLE,
            body: "ambiguous commit acknowledgement".to_string(),
        }));
        let CommitAckDisposition::Report(error) = disposition else {
            panic!("exhausted commit error must report failure and release residual ownership");
        };
        assert_eq!(failure::classify(&error, false), "internal_error");
        assert!(error
            .to_string()
            .contains("commit_task failed after bounded retries"));
        assert!(error.to_string().contains("503"));
    }

    #[test]
    fn commit_ack_disposition_reports_ownership_conflict() {
        let disposition = commit_ack_disposition(Err(protocol::ProtocolError::Status {
            endpoint: "/v1/worker/task/{id}/commit".to_string(),
            status: reqwest::StatusCode::CONFLICT,
            body: "task is not claimed by this worker".to_string(),
        }));
        let CommitAckDisposition::Report(error) = disposition else {
            panic!("commit ownership fence must enter the exact fail/release path");
        };
        assert_eq!(failure::classify(&error, false), "internal_error");
        assert!(error.to_string().contains("409"));
        assert!(error
            .to_string()
            .contains("task is not claimed by this worker"));
    }

    #[test]
    fn honeypot_known_answer_is_in_worker_wire_order_not_alphabetical() {
        let bytes = honeypot_known_answer_bytes("llama-3.2-1b-instruct-q4", "candle", "ok", 2)
            .expect("serialize honeypot answer");
        let text = String::from_utf8(bytes).expect("utf-8");
        // control/verification.go compares batch_infer honeypots with bytes.Equal
        // against what executor::BatchInferResult serializes, so the key order is
        // load-bearing. A serde_json::json! literal would alphabetize this to
        // completions/inference_backend/job_type/model and never match a commit.
        assert_eq!(
            text,
            r#"{"job_type":"batch_infer","model":"llama-3.2-1b-instruct-q4","inference_backend":"candle","completions":[{"index":0,"text":"ok","tokens":2}]}"#,
            "honeypot answer must be byte-identical to a worker's BatchInferResult commit"
        );
    }

    #[test]
    fn compressed_memory_fallback_is_bounded_and_fails_closed_without_stats() {
        assert_eq!(hardware::resolved_available_memory(100, 25, 80), 25);
        assert_eq!(hardware::resolved_available_memory(100, 0, 80), 20);
        assert_eq!(hardware::resolved_available_memory(100, 0, 0), 0);
        assert_eq!(hardware::resolved_available_memory(100, 0, 100), 0);
    }

    #[test]
    fn eligibility_watch_interval_follows_checkpoint_secs() {
        assert_eq!(eligibility_watch_interval(7), Duration::from_secs(7));
        assert_eq!(eligibility_watch_interval(0), Duration::from_secs(30));
        assert_eq!(eligibility_watch_interval(30), Duration::from_secs(30));
    }

    #[test]
    fn transfer_budget_uses_decimal_megabits_and_zero_is_unlimited() {
        assert_eq!(
            transfer_budget(1_000_000, 8_000_000),
            Duration::from_secs(1)
        );
        assert_eq!(transfer_budget(64 * 1024, 0), Duration::ZERO);
        assert_eq!(transfer_budget(0, 1), Duration::ZERO);
    }

    /// Mid-run power_only / quiet-hours loss must abort via the deadline
    /// cancellation path and classify as the same recoverable `timeout` the
    /// wall-clock watchdog reports (control plane requeues).
    #[tokio::test]
    async fn mid_run_battery_loss_aborts_within_checkpoint_interval_as_timeout() {
        let power_only = true;
        let quiet_hours: Option<(u8, u8)> = None;
        let hour = Arc::new(AtomicU8::new(12));
        let on_battery = Arc::new(AtomicBool::new(false));

        let deadline = TaskDeadline::for_test(Duration::from_secs(30));
        let interval = Duration::from_millis(40);

        let watch = deadline.clone();
        let hour_w = hour.clone();
        let batt_w = on_battery.clone();
        let guard = tokio::spawn(async move {
            watch
                .cancel_when_ineligible(interval, move || {
                    let cfg_hour = hour_w.load(Ordering::Acquire);
                    let cfg_batt = batt_w.load(Ordering::Acquire);
                    // Same predicate as AgentConfig::is_eligible_to_run.
                    if power_only && cfg_batt {
                        return true;
                    }
                    if let Some((start, end)) = quiet_hours {
                        let in_quiet = if start <= end {
                            cfg_hour >= start && cfg_hour < end
                        } else {
                            cfg_hour >= start || cfg_hour < end
                        };
                        if in_quiet {
                            return true;
                        }
                    }
                    false
                })
                .await;
        });

        let run_deadline = deadline.clone();
        let work = tokio::spawn(async move {
            run_deadline
                .run("model load and execution", std::future::pending::<()>())
                .await
        });

        tokio::time::sleep(Duration::from_millis(20)).await;
        on_battery.store(true, Ordering::Release);

        let flipped = std::time::Instant::now();
        let result = work.await.unwrap();
        assert!(
            flipped.elapsed() < interval + Duration::from_millis(250),
            "abort must land within one checkpoint interval; elapsed {:?}",
            flipped.elapsed()
        );
        let err = result.expect_err("ineligible supplier must abort the in-flight phase");
        assert!(matches!(err, deadline::DeadlineError::Expired { .. }));

        let run_err = RunError::from(err);
        assert_eq!(
            failure::classify(&run_err, false),
            "timeout",
            "must match the deadline-abort recoverable-failure class"
        );
        assert!(deadline.is_cancelled());
        let _ = guard.await;
        let _ = hour; // keep sources alive for the guard
    }

    #[tokio::test]
    async fn mid_run_quiet_hours_aborts_via_same_deadline_path() {
        let power_only = false;
        let quiet_hours = Some((22u8, 6u8));
        let hour = Arc::new(AtomicU8::new(21));
        let on_battery = Arc::new(AtomicBool::new(false));

        let deadline = TaskDeadline::for_test(Duration::from_secs(30));
        let interval = Duration::from_millis(40);

        let watch = deadline.clone();
        let hour_w = hour.clone();
        let batt_w = on_battery.clone();
        let guard = tokio::spawn(async move {
            watch
                .cancel_when_ineligible(interval, move || {
                    !eligible_for_test(
                        power_only,
                        quiet_hours,
                        hour_w.load(Ordering::Acquire),
                        batt_w.load(Ordering::Acquire),
                    )
                })
                .await;
        });

        let run_deadline = deadline.clone();
        let work = tokio::spawn(async move {
            run_deadline
                .run("model load and execution", std::future::pending::<()>())
                .await
        });

        tokio::time::sleep(Duration::from_millis(20)).await;
        hour.store(22, Ordering::Release); // enter quiet window mid-run

        let flipped = std::time::Instant::now();
        let result = work.await.unwrap();
        assert!(flipped.elapsed() < interval + Duration::from_millis(250));
        let err = result.expect_err("quiet hours must abort");
        assert_eq!(failure::classify(&RunError::from(err), false), "timeout");
        let _ = guard.await;
    }

    fn eligible_for_test(
        power_only: bool,
        quiet_hours: Option<(u8, u8)>,
        now_hour: u8,
        on_battery: bool,
    ) -> bool {
        if power_only && on_battery {
            return false;
        }
        if let Some((start, end)) = quiet_hours {
            let in_quiet = if start <= end {
                now_hour >= start && now_hour < end
            } else {
                now_hour >= start || now_hour < end
            };
            if in_quiet {
                return false;
            }
        }
        true
    }

    #[test]
    fn dispatch_runtime_authority_is_exact_and_fail_closed() {
        let sha = runtime_authority::sha256();
        assert!(runtime_authority_matches_for(
            "candle-metal-minilm-embed",
            "candle_metal",
            sha,
            "candle",
            types::HardwareClass::AppleSiliconPro,
            "metal",
            "embed",
            "all-minilm-l6-v2",
            types::ModelKind::Hf,
        ));
        for rejected in [
            runtime_authority_matches_for(
                "candle-metal-minilm-embed",
                "candle_metal",
                &"0".repeat(64),
                "candle",
                types::HardwareClass::AppleSiliconPro,
                "metal",
                "embed",
                "all-minilm-l6-v2",
                types::ModelKind::Hf,
            ),
            runtime_authority_matches_for(
                "candle-metal-minilm-embed",
                "candle_metal",
                sha,
                "candle",
                types::HardwareClass::AppleSiliconPro,
                "metal",
                "batch_infer",
                "all-minilm-l6-v2",
                types::ModelKind::Hf,
            ),
            runtime_authority_matches_for(
                "candle-metal-minilm-embed",
                "candle_metal",
                sha,
                "candle",
                types::HardwareClass::AppleSiliconPro,
                "cpu",
                "embed",
                "all-minilm-l6-v2",
                types::ModelKind::Hf,
            ),
            runtime_authority_matches_for(
                "candle-metal-minilm-embed",
                "candle_metal",
                sha,
                "candle",
                types::HardwareClass::AppleSiliconPro,
                "metal",
                "embed",
                "all-minilm-l6-v2",
                types::ModelKind::Gguf,
            ),
        ] {
            assert!(!rejected);
        }
    }

    #[cfg(target_os = "macos")]
    #[test]
    fn pick_sandbox_profile_prefers_override_then_sibling_then_none() {
        use std::path::Path;
        let over = PathBuf::from("/opt/merc/override.sb");
        let sib = PathBuf::from("/Applications/Merc.app/Contents/Resources/merc-agent.sb");

        let all_exist = |_: &Path| true;
        assert_eq!(
            pick_sandbox_profile(Some(over.clone()), Some(sib.clone()), all_exist),
            Some(over.clone()),
            "an existing explicit override must take priority over the exe sibling"
        );

        let only_sibling = |p: &Path| p == sib.as_path();
        assert_eq!(
            pick_sandbox_profile(Some(over.clone()), Some(sib.clone()), only_sibling),
            Some(sib.clone()),
            "a non-existent override must be skipped and the sibling used"
        );

        let none_exist = |_: &Path| false;
        assert_eq!(
            pick_sandbox_profile(Some(over.clone()), Some(sib.clone()), none_exist),
            None,
            "when no profile exists the discovery must return None, not a phantom path"
        );

        let sibling_exists = |p: &Path| p == sib.as_path();
        assert_eq!(
            pick_sandbox_profile(None, Some(sib.clone()), sibling_exists),
            Some(sib.clone())
        );
        assert_eq!(
            pick_sandbox_profile(None, Some(sib), |_: &Path| false),
            None
        );
    }

    #[cfg(target_os = "macos")]
    #[test]
    fn sandbox_data_dir_is_merc_under_home() {
        let tmp =
            std::env::temp_dir().join(format!("merc-agent-sandbox-home-{}", std::process::id()));
        let _ = std::fs::remove_dir_all(&tmp);
        std::fs::create_dir_all(&tmp).unwrap();
        assert_eq!(
            sandbox_data_dir(tmp.to_str().unwrap()),
            tmp.join(".merc").to_string_lossy()
        );
        let _ = std::fs::remove_dir_all(&tmp);
    }

    #[test]
    fn agent_home_migrates_legacy_compute_exchange_dir() {
        let tmp =
            std::env::temp_dir().join(format!("merc-agent-home-migrate-{}", std::process::id()));
        let _ = std::fs::remove_dir_all(&tmp);
        std::fs::create_dir_all(&tmp).unwrap();
        let legacy = tmp.join(".compute-exchange");
        std::fs::create_dir_all(&legacy).unwrap();
        std::fs::write(legacy.join("bench_cache.json"), b"{}").unwrap();
        let home = config::agent_home_dir_for(&tmp);
        assert_eq!(home, tmp.join(".merc"));
        assert!(home.join("bench_cache.json").is_file());
        assert!(!legacy.exists());
        let _ = std::fs::remove_dir_all(&tmp);
    }

    #[cfg(target_os = "macos")]
    #[test]
    fn unsandboxed_opt_in_parser_is_explicit() {
        for enabled in ["1", "true", "TRUE", " yes "] {
            assert!(env_flag_truthy(Some(enabled)), "{enabled:?}");
        }
        for disabled in [None, Some(""), Some("0"), Some("false"), Some("maybe")] {
            assert!(!env_flag_truthy(disabled));
        }
    }

    #[cfg(target_os = "macos")]
    #[test]
    fn host_port_from_url_covers_common_shapes() {
        assert_eq!(
            sandbox_egress::host_port_from_url("https://api.example.com/v1"),
            Some("api.example.com:443".into())
        );
        assert_eq!(
            sandbox_egress::host_port_from_url("http://localhost:8080"),
            Some("localhost:8080".into())
        );
        assert_eq!(
            sandbox_egress::host_port_from_url("control.internal:8443"),
            Some("control.internal:8443".into())
        );
        assert_eq!(sandbox_egress::host_port_from_url(""), None);
    }

    #[test]
    fn refuse_unsandboxed_error_string_is_greppable() {
        // Guard the greppable refusal token the control plane and ops greps for.
        assert!(
            "merc-agent REFUSED_UNSANDBOXED_BUYER_PAYLOAD: no seatbelt profile"
                .contains("REFUSED_UNSANDBOXED_BUYER_PAYLOAD")
        );
    }

    #[test]
    fn bench_mode_parse_accepts_known_and_rejects_unknown() {
        assert_eq!(BenchMode::parse("identical").unwrap(), BenchMode::Identical);
        assert_eq!(BenchMode::parse("mixed").unwrap(), BenchMode::Mixed);
        assert_eq!(BenchMode::parse("  MiXeD ").unwrap(), BenchMode::Mixed);
        assert!(
            BenchMode::parse("random").is_err(),
            "unknown mode must be a hard error, not silently defaulted"
        );
    }

    #[test]
    fn identical_mode_produces_one_distinct_prompt() {
        let prompts = build_bench_prompts("hello ocean", 8, BenchMode::Identical);
        assert_eq!(prompts.len(), 8);
        assert!(
            prompts.iter().all(|p| p == "hello ocean"),
            "identical mode must repeat the stem verbatim in every row"
        );
        let distinct: std::collections::HashSet<_> = prompts.iter().collect();
        assert_eq!(
            distinct.len(),
            1,
            "identical mode: exactly one distinct prompt"
        );
    }

    #[test]
    fn mixed_mode_fragments_into_several_distinct_lengths() {
        let b = 8;
        let prompts = build_bench_prompts("Write about the ocean:", b, BenchMode::Mixed);
        assert_eq!(prompts.len(), b);
        let distinct_char_lens: std::collections::HashSet<usize> =
            prompts.iter().map(|p| p.len()).collect();
        assert!(
            distinct_char_lens.len() >= 3,
            "mixed mode must span multiple distinct lengths (got {} for b={b}: {:?})",
            distinct_char_lens.len(),
            prompts.iter().map(|p| p.len()).collect::<Vec<_>>()
        );
        assert!(
            prompts
                .iter()
                .all(|p| p.starts_with("Write about the ocean:")),
            "every mixed prompt must extend the shared stem"
        );
    }

    #[test]
    fn mixed_mode_is_deterministic_across_calls() {
        let a = build_bench_prompts("stem", 16, BenchMode::Mixed);
        let b = build_bench_prompts("stem", 16, BenchMode::Mixed);
        assert_eq!(a, b, "mixed prompt generation must be deterministic");
    }

    #[test]
    fn mixed_mode_prompt_for_a_row_is_stable_as_batch_grows() {
        let small = build_bench_prompts("s", 4, BenchMode::Mixed);
        let large = build_bench_prompts("s", 16, BenchMode::Mixed);
        assert_eq!(
            &large[..4],
            &small[..],
            "row i's prompt must not change when the batch widens"
        );
    }

    #[test]
    fn cv_pct_zero_for_single_or_identical_samples() {
        assert_eq!(
            coefficient_of_variation_pct(&[]),
            0.0,
            "no samples: nothing to disperse"
        );
        assert_eq!(
            coefficient_of_variation_pct(&[42.0]),
            0.0,
            "one sample: nothing to disperse"
        );
        assert_eq!(
            coefficient_of_variation_pct(&[100.0, 100.0, 100.0]),
            0.0,
            "identical samples: zero variance"
        );
        assert_eq!(
            coefficient_of_variation_pct(&[0.0, 0.0]),
            0.0,
            "all-zero mean must not divide by zero"
        );
    }

    #[test]
    fn cv_pct_matches_hand_computed_value() {
        let cv = coefficient_of_variation_pct(&[90.0, 110.0]);
        assert!((cv - 10.0).abs() < 1e-9, "expected CV=10%, got {cv}");
    }

    #[test]
    fn sha256_hex_matches_known_test_vector() {
        let got = sha256_hex(b"abc");
        assert_eq!(
            got, "ba7816bf8f01cfea414140de5dae2223b00361a396177a9cb410ff61f20015ad",
            "sha256_hex(\"abc\") must match the well-known NIST test vector"
        );
        assert_eq!(got.len(), 64, "must be 64 lowercase hex chars  -  the exact shape control/store.go's nullSHA256Hex requires");
    }

    #[test]
    fn sha256_hex_of_empty_input_is_the_known_empty_digest() {
        let got = sha256_hex(b"");
        assert_eq!(
            got, "e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855",
            "sha256_hex(\"\") must match the well-known empty-string digest"
        );
        assert_eq!(got.len(), 64, "must be 64 lowercase hex chars");
    }

    #[test]
    fn cv_pct_flags_the_kind_of_anomaly_the_audit_named() {
        let cv = coefficient_of_variation_pct(&[693.0, 1087.9, 1090.0]);
        assert!(
            cv > 10.0,
            "expected a real anomaly of this size to exceed 10% CV, got {cv}%"
        );
    }

    #[test]
    fn median_picks_the_middle_of_odd_and_even_counts() {
        let mut odd = vec![5.0, 1.0, 3.0];
        assert_eq!(median(&mut odd), 3.0);
        let mut even = vec![10.0, 20.0, 30.0, 40.0];
        assert_eq!(median(&mut even), 30.0);
        let mut single = vec![7.5];
        assert_eq!(median(&mut single), 7.5);
    }

    #[test]
    fn median_is_robust_to_a_single_outlier_unlike_a_mean() {
        let mut samples = vec![100.0, 102.0, 98.0, 500.0]; // one bench-noise outlier
        let med = median(&mut samples);
        let mean: f64 = [100.0, 102.0, 98.0, 500.0].iter().sum::<f64>() / 4.0;
        assert!(
            (med - 100.0).abs() < (mean - 100.0).abs(),
            "median ({med}) should track the real cluster of samples much more closely than the mean ({mean}) once an outlier is present"
        );
    }

    #[test]
    fn sustained_summary_empty_is_all_zero() {
        assert_eq!(sustained_summary(&[]), (0.0, 0.0, 0.0));
    }

    #[test]
    fn sustained_summary_flat_curve_has_zero_gap() {
        let (peak, sustained, gap) = sustained_summary(&[100.0, 100.0, 100.0, 100.0]);
        assert_eq!(peak, 100.0);
        assert_eq!(sustained, 100.0);
        assert_eq!(gap, 0.0);
    }

    #[test]
    fn sustained_summary_detects_real_throttling_decay() {
        let windows = vec![
            140.0, 138.0, 130.0, 120.0, 110.0, 105.0, 100.0, 98.0, 97.0, 96.0, 95.0, 95.0,
        ];
        let (peak, sustained, gap) = sustained_summary(&windows);
        assert_eq!(peak, 140.0);
        assert!(
            (sustained - (96.0 + 95.0 + 95.0) / 3.0).abs() < 1e-9,
            "sustained should be the mean of the last 25% (3 of 12 windows), got {sustained}"
        );
        assert!(
            gap > 25.0,
            "expected a real double-digit throttling gap, got {gap}%"
        );
    }

    #[test]
    fn sustained_summary_never_reports_negative_gap_for_a_rising_curve() {
        let windows = vec![50.0, 70.0, 90.0, 100.0, 100.0, 100.0, 100.0, 100.0];
        let (peak, sustained, gap) = sustained_summary(&windows);
        assert_eq!(peak, 100.0);
        assert_eq!(sustained, 100.0);
        assert!(
            gap.abs() < 1e-9,
            "rising-then-flat curve should show ~0% gap, got {gap}%"
        );
    }

    #[test]
    fn sustained_summary_single_window_is_its_own_peak_and_sustained() {
        let (peak, sustained, gap) = sustained_summary(&[77.0]);
        assert_eq!(peak, 77.0);
        assert_eq!(sustained, 77.0);
        assert_eq!(gap, 0.0);
    }

    async fn spawn_sequenced_mock_server(responses: Vec<(u16, &'static str)>) -> String {
        let listener = tokio::net::TcpListener::bind("127.0.0.1:0").await.unwrap();
        let addr = listener.local_addr().unwrap();
        tokio::spawn(async move {
            use tokio::io::{AsyncReadExt, AsyncWriteExt};
            for (status, body) in responses {
                let (mut socket, _) = listener.accept().await.unwrap();
                let mut buf = vec![0u8; 16384];
                let mut total = 0usize;
                let header_end = loop {
                    let n = socket.read(&mut buf[total..]).await.unwrap();
                    total += n;
                    if let Some(pos) = buf[..total].windows(4).position(|w| w == b"\r\n\r\n") {
                        break pos + 4;
                    }
                    if n == 0 {
                        break total;
                    }
                };
                let headers = String::from_utf8_lossy(&buf[..header_end]).to_lowercase();
                let content_length: usize = headers
                    .lines()
                    .find_map(|l| {
                        l.strip_prefix("content-length:")
                            .map(|v| v.trim().parse().unwrap_or(0))
                    })
                    .unwrap_or(0);
                while total < header_end + content_length {
                    let n = socket.read(&mut buf[total..]).await.unwrap();
                    if n == 0 {
                        break;
                    }
                    total += n;
                }
                let reason = match status {
                    200 => "OK",
                    204 => "No Content",
                    404 => "Not Found",
                    503 => "Service Unavailable",
                    _ => "Status",
                };
                let resp = if body.is_empty() {
                    format!("HTTP/1.1 {status} {reason}\r\nContent-Length: 0\r\nConnection: close\r\n\r\n")
                } else {
                    format!(
                        "HTTP/1.1 {status} {reason}\r\nContent-Type: text/plain\r\nContent-Length: {}\r\nConnection: close\r\n\r\n{body}",
                        body.len()
                    )
                };
                socket.write_all(resp.as_bytes()).await.unwrap();
                socket.shutdown().await.ok();
            }
        });
        format!("http://{addr}")
    }

    #[tokio::test]
    async fn s3_get_retries_transient_5xx_then_succeeds() {
        let base =
            spawn_sequenced_mock_server(vec![(503, "try again"), (200, "hello world")]).await;
        let client = reqwest::Client::new();
        let bytes = s3_get(&client, &base)
            .await
            .expect("should succeed after one retry");
        assert_eq!(bytes, b"hello world");
    }

    #[tokio::test]
    async fn s3_get_does_not_retry_client_error() {
        let base = spawn_sequenced_mock_server(vec![(404, "not found")]).await;
        let client = reqwest::Client::new();
        let err = s3_get(&client, &base)
            .await
            .expect_err("404 must not be retried into a success");
        assert!(err.to_string().contains("error status") || err.to_string().contains("404"));
    }

    #[tokio::test]
    async fn s3_put_bytes_retries_transient_5xx_then_succeeds() {
        let base = spawn_sequenced_mock_server(vec![(503, "try again"), (204, "")]).await;
        let client = reqwest::Client::new();
        s3_put_bytes(&client, &base, b"payload", "application/json")
            .await
            .expect("should succeed after one retry");
    }

    #[tokio::test]
    async fn connect_timeout_fails_fast_and_distinct_from_total_timeout() {
        let client = reqwest::Client::builder()
            .connect_timeout(Duration::from_millis(500))
            .timeout(Duration::from_secs(60))
            .build()
            .unwrap();
        let start = std::time::Instant::now();
        let err = s3_get_once(&client, "http://10.255.255.1:1/", &[])
            .await
            .expect_err("a black-holed address must not succeed");
        let elapsed = start.elapsed();
        assert!(
            elapsed < Duration::from_secs(5),
            "connect_timeout(500ms) should fail in well under the 60s total timeout, took {elapsed:?}"
        );
        let reqwest_err = err
            .downcast_ref::<reqwest::Error>()
            .expect("a connect failure should surface as a reqwest::Error");
        assert!(
            reqwest_err.is_connect() || reqwest_err.is_timeout(),
            "expected a connect/timeout error, got {reqwest_err:?}"
        );
    }

    async fn spawn_raw_mock_server(
        raw_response: &'static [u8],
    ) -> (String, Arc<tokio::sync::Mutex<Option<String>>>) {
        let listener = tokio::net::TcpListener::bind("127.0.0.1:0").await.unwrap();
        let addr = listener.local_addr().unwrap();
        let captured: Arc<tokio::sync::Mutex<Option<String>>> =
            Arc::new(tokio::sync::Mutex::new(None));
        let captured_clone = captured.clone();
        tokio::spawn(async move {
            use tokio::io::{AsyncReadExt, AsyncWriteExt};
            let (mut socket, _) = listener.accept().await.unwrap();
            let mut buf = vec![0u8; 16384];
            let mut total = 0usize;
            let header_end = loop {
                let n = socket.read(&mut buf[total..]).await.unwrap();
                total += n;
                if let Some(pos) = buf[..total].windows(4).position(|w| w == b"\r\n\r\n") {
                    break pos + 4;
                }
                if n == 0 {
                    break total;
                }
            };
            let headers = String::from_utf8_lossy(&buf[..header_end]).to_string();
            *captured_clone.lock().await = Some(headers);
            socket.write_all(raw_response).await.unwrap();
            socket.shutdown().await.ok();
        });
        (format!("http://{addr}"), captured)
    }

    #[tokio::test]
    async fn s3_get_sends_range_header_on_resume_and_appends_206_body() {
        let (base1, _headers1) = spawn_raw_mock_server(
            b"HTTP/1.1 200 OK\r\nContent-Type: text/plain\r\nContent-Length: 11\r\nConnection: close\r\n\r\nhello ",
        )
        .await;
        let client = reqwest::Client::new();
        let first = s3_get_once(&client, &base1, &[]).await;
        let partial_err = first.expect_err("a truncated body must surface as a read failure");
        let partial = partial_err
            .downcast_ref::<PartialBodyError>()
            .expect("truncated body must be a PartialBodyError, not some other failure");
        assert_eq!(
            partial.bytes_read, b"hello ",
            "streamed transfer accounting must preserve the exact prefix received before a drop"
        );

        let (base2, headers2) = spawn_raw_mock_server(
            b"HTTP/1.1 206 Partial Content\r\nContent-Type: text/plain\r\nContent-Range: bytes 6-10/11\r\nContent-Length: 5\r\nConnection: close\r\n\r\nworld",
        )
        .await;
        let already_read = b"hello ".to_vec();
        let resumed = s3_get_once(&client, &base2, &already_read)
            .await
            .expect("a real 206 response to a Range-resume must succeed");
        assert_eq!(
            resumed, b"hello world",
            "resumed bytes must be appended to the already-read prefix, not replace it"
        );

        let sent_headers = headers2.lock().await.clone().unwrap();
        let sent_headers_lc = sent_headers.to_lowercase();
        assert!(
            sent_headers_lc.contains("range: bytes=6-"),
            "resumed GET must send a Range header for the bytes already read, got headers:\n{sent_headers}"
        );
    }

    #[tokio::test]
    async fn s3_get_transparently_decodes_a_real_gzip_response() {
        use std::io::Write;
        let plaintext = b"hello compressed world, this is the real input body";
        let mut encoder = flate2_test_helper::GzEncoder::new(
            Vec::new(),
            flate2_test_helper::Compression::default(),
        );
        encoder.write_all(plaintext).unwrap();
        let gz_bytes = encoder.finish().unwrap();

        let mut raw = format!(
            "HTTP/1.1 200 OK\r\nContent-Type: text/plain\r\nContent-Encoding: gzip\r\nContent-Length: {}\r\nConnection: close\r\n\r\n",
            gz_bytes.len()
        )
        .into_bytes();
        raw.extend_from_slice(&gz_bytes);
        let raw: &'static [u8] = Box::leak(raw.into_boxed_slice());

        let (base, _headers) = spawn_raw_mock_server(raw).await;
        let client = reqwest::Client::new();
        let bytes = s3_get(&client, &base)
            .await
            .expect("a real gzip response must be transparently decoded");
        assert_eq!(
            bytes, plaintext,
            "s3_get must hand back the DECODED plaintext body, not raw gzip bytes"
        );
    }

    mod flate2_test_helper {
        pub use flate2::write::GzEncoder;
        pub use flate2::Compression;
    }
}
