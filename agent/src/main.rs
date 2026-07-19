//! Computexchange supplier agent.
//!
//! A signed binary that runs on idle Apple Silicon Macs: it detects and
//! benchmarks the hardware, registers with the Go control plane, polls for
//! tasks, executes them through the runner backends, and reports the result.
//! Three subcommands: `run`, `bench`, `version`.

mod cluster;
mod coalesce; // interim cross-task batching bridge (Agent Concurrency & Parallelism Model 7.5→8)
mod config;
mod continuous_batch; // Apple-Silicon continuous-batch lane skeleton (Hawking port; docs/HAWKING_PORT_PLAN.md)
mod failure;
mod hardware;
#[cfg(feature = "metal")]
mod hawking_metal_kernel; // real Metal kernel port, Hawking port week 2 (docs/HAWKING_PORT_PLAN.md); not yet wired to any runner
mod models;
mod paged_kv; // host-side paged KV and namespaced prefix-cache ownership (experimental, not routed)
mod pool;
mod protocol;
mod quantized_llama_batched; // vendored + patched candle quantized_llama (bsz>1 batched prefill)
mod resident_engine; // host-side long-lived model actor scheduler (experimental, not routed)
mod runners;
mod runtime_matrix_generated;
mod sandbox; // sandboxed BYO-container execution for the `custom` general-compute lane
mod status;
mod types;
mod whisper_decoder_kv; // vendored + patched candle whisper decoder (incremental self-attn KV cache)

use std::path::PathBuf;
use std::sync::Arc;
use std::time::{Duration, SystemTime, UNIX_EPOCH};

use anyhow::{Context, Result};
use clap::{Parser, Subcommand};
use sysinfo::System;
use tokio::sync::Semaphore;

use config::AgentConfig;
use pool::ModelPool;
use protocol::ControlPlaneClient;
use runners::{default_runners, dispatch, JobRunner, RunError};
use status::StatusWriter;
use types::{Heartbeat, TaskCommit, TaskDispatch, WorkerCapability};

const AGENT_VERSION: &str = env!("CARGO_PKG_VERSION");

/// How long a warm model may sit untouched before the pool evicts it (docs/
/// CREED_AND_PATH_TO_TEN.md, "Warm model pool" 7→8). 15 minutes is long enough
/// that a burst of same-model tasks a few minutes apart never pays a cold
/// reload, short enough that a 7B model touched once doesn't pin ~4.7GB on a
/// supplier's Mac for the rest of the process.
const MODEL_IDLE_EVICT_AFTER: Duration = Duration::from_secs(15 * 60);

/// Env marker set once the agent is running under the macOS seatbelt sandbox, by
/// whichever launch path applied it. Both the Swift menu-bar launcher
/// (`macapp/ComputeExchangeAgent/AgentController.swift`) and the Rust self-re-exec
/// (`reexec_under_sandbox_if_needed`) set it before handing control to the sandboxed
/// process, so the child can tell it is already contained and must NOT wrap itself
/// again — the guard against an infinite re-exec loop.
const CX_SANDBOXED_ENV: &str = "CX_SANDBOXED";

/// Optional operator/dev override: an explicit path to the seatbelt profile the
/// self-re-exec should apply. When unset, the self-re-exec auto-discovers the profile
/// as a sibling of the running executable (the `.app` bundle layout, where
/// `cx-agent.sb` sits next to `cx-agent` in `Contents/Resources`). A bare `cargo run`
/// dev build has neither, so it runs honestly UNSANDBOXED rather than failing to
/// launch — exactly as the `.app` path already does when it can't resolve the profile.
const CX_SANDBOX_PROFILE_ENV: &str = "CX_SANDBOX_PROFILE";

/// Fail-closed policy used by the distributed macOS app. Direct development
/// launches remain possible without it, but an installed supplier app must never
/// turn a missing/broken sandbox into permission to process buyer work.
const CX_REQUIRE_SANDBOX_ENV: &str = "CX_REQUIRE_SANDBOX";

#[cfg(target_os = "macos")]
fn sandbox_required_value(value: Option<&str>) -> bool {
    value.is_some_and(|value| {
        matches!(
            value.trim().to_ascii_lowercase().as_str(),
            "1" | "true" | "yes"
        )
    })
}

#[cfg(target_os = "macos")]
fn sandbox_required() -> bool {
    sandbox_required_value(std::env::var(CX_REQUIRE_SANDBOX_ENV).ok().as_deref())
}

#[cfg(target_os = "macos")]
fn sandbox_wrap_failed(message: &str) {
    if sandbox_required() {
        tracing::error!(
            "cx-agent refused to start: {message}. {CX_REQUIRE_SANDBOX_ENV}=1 requires the macOS seatbelt sandbox"
        );
        std::process::exit(78);
    }
    tracing::warn!("cx-agent is running UNSANDBOXED: {message}");
}

/// On macOS, if this `run` invocation is NOT already under the seatbelt sandbox and a
/// profile can be located, re-exec the process under `sandbox-exec -f <profile> …` so a
/// DIRECT binary launch (a supplier's `cargo run`, a hand-rolled LaunchAgent, `make
/// agent-run`) is contained too — not only the supported `.app` install path. This
/// closes the "the guarantee is 'the install path sandboxes the child', not 'the binary
/// sandboxes itself'" gap named in docs/SECURITY.md (Security Posture 8→9,
/// docs/internal/CREED_AND_PATH_TO_TEN.md).
///
/// It is a deliberate NO-OP (returns `Ok`, agent runs unwrapped) when:
///   - not macOS (seatbelt/`sandbox-exec` is macOS-only);
///   - already sandboxed (`CX_SANDBOXED=1`) — the loop guard; the `.app` launcher and
///     our own re-exec both set it, so we never double-wrap;
///   - no profile can be resolved (no `CX_SANDBOX_PROFILE`, none beside the binary) —
///     a bare dev build runs UNSANDBOXED with a warning, while the distributed app
///     sets `CX_REQUIRE_SANDBOX=1` and fails closed.
///
/// On success it does not return: `execv` replaces the current image with
/// `sandbox-exec`, which re-launches this same binary (now with `CX_SANDBOXED=1`) under
/// the profile. An exec failure is a hard stop under the app's required policy and
/// a loud development warning otherwise.
///
/// The `-D KEY=VALUE` params exactly mirror what `cx-agent.sb` references and what the
/// Swift launcher passes: HOME, MODELCACHE (CX_MODEL_CACHE / HF_HOME / ~/.cache/
/// huggingface), DATADIR (~/.compute-exchange), TMPDIR.
#[cfg(target_os = "macos")]
fn reexec_under_sandbox_if_needed() {
    use std::os::unix::process::CommandExt;

    // Loop guard: already inside the sandbox → nothing to do.
    if std::env::var(CX_SANDBOXED_ENV).as_deref() == Ok("1") {
        return;
    }

    let profile = match resolve_sandbox_profile() {
        Some(p) => p,
        None => {
            sandbox_wrap_failed(&format!(
                "no seatbelt profile found (set {CX_SANDBOX_PROFILE_ENV} to cx-agent.sb, or launch via the ComputeExchangeAgent .app); buyer-payload filesystem/network containment is not active"
            ));
            return;
        }
    };

    const SANDBOX_EXEC: &str = "/usr/bin/sandbox-exec";
    if !std::path::Path::new(SANDBOX_EXEC).exists() {
        sandbox_wrap_failed(&format!("{SANDBOX_EXEC} not found (unexpected on macOS)"));
        return;
    }

    // The current binary + the original args, minus argv[0].
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

    tracing::info!(
        "re-executing cx-agent under the macOS seatbelt sandbox (profile: {})",
        profile.display()
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
        .arg(&exe)
        .args(&args)
        // Mark the re-exec'd child as sandboxed so it does NOT try to wrap again.
        .env(CX_SANDBOXED_ENV, "1");

    // `exec` replaces this process image; it only returns on failure.
    let err = cmd.exec();
    sandbox_wrap_failed(&format!("failed to re-exec under {SANDBOX_EXEC} ({err})"));
}

/// Non-macOS: seatbelt does not exist, so this is a pure no-op.
#[cfg(not(target_os = "macos"))]
fn reexec_under_sandbox_if_needed() {}

/// Resolve the seatbelt profile for the self-re-exec: the explicit
/// `CX_SANDBOX_PROFILE` override first, then a `cx-agent.sb` sitting beside the running
/// executable (the `.app` `Contents/Resources` layout). `None` when neither exists — a
/// bare dev build then runs unsandboxed. Split out (and pure w.r.t. its inputs) so the
/// discovery order is unit-testable without spawning a process.
#[cfg(target_os = "macos")]
fn resolve_sandbox_profile() -> Option<PathBuf> {
    let override_path = std::env::var(CX_SANDBOX_PROFILE_ENV)
        .ok()
        .filter(|p| !p.is_empty())
        .map(PathBuf::from);
    let exe_sibling = std::env::current_exe()
        .ok()
        .and_then(|exe| exe.parent().map(|d| d.join("cx-agent.sb")));
    // `is_file` decides existence; split out as `pick_sandbox_profile` so the
    // discovery ORDER (explicit override wins, then the exe sibling) is unit-testable
    // against real temp files without mutating process-global env.
    pick_sandbox_profile(override_path, exe_sibling, |p| p.is_file())
}

/// Pure profile-selection: given an optional explicit override and an optional
/// executable-sibling candidate, return the first that `exists` reports present, in
/// that priority order. Factored out of `resolve_sandbox_profile` for testability —
/// `exists` is injected so a test can point at real temp files without env mutation.
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

/// Model-cache root for the sandbox params — mirrors `status.rs::model_cache_dir` and
/// `models.rs`: `$CX_MODEL_CACHE`, else `$HF_HOME`, else `~/.cache/huggingface`. (The
/// profile scopes writes to this subtree so hf-hub downloads stay allowed.)
#[cfg(target_os = "macos")]
fn sandbox_model_cache_dir() -> String {
    if let Ok(d) = std::env::var("CX_MODEL_CACHE") {
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

/// Agent data dir for the sandbox params — `~/.compute-exchange`, matching
/// `config.rs`'s default `data_dir` and the Swift launcher's `AgentPaths.dataDir`.
#[cfg(target_os = "macos")]
fn sandbox_data_dir(home: &str) -> String {
    format!("{home}/.compute-exchange")
}

#[derive(Parser)]
#[command(name = "cx-agent", version = AGENT_VERSION, about = "Computexchange supplier agent")]
struct Cli {
    #[command(subcommand)]
    command: Command,
}

#[derive(Subcommand)]
enum Command {
    /// Detect + benchmark hardware, then run the agent loop against the control plane.
    Run {
        /// Path to the agent TOML config.
        #[arg(long, default_value = "agent.toml")]
        config: PathBuf,
    },
    /// Detect + benchmark hardware and print the WorkerCapability as JSON. No network.
    Bench {
        /// Optional config path; only `supplier_id` is read (for the printed record).
        #[arg(long)]
        config: Option<PathBuf>,
    },
    /// Plane B (docs/PLANE_B.md): from MEASURED cluster figures, compute what a
    /// co-located Mac fabric would advertise as one `apple_silicon_cluster` worker
    /// (summed usable memory + bottleneck bandwidth), how a model's layers shard
    /// across it, and the drop-a-node → re-shard-or-offline decision. PURE math, no
    /// hardware — this only PLANS; executing the plan needs the external substrate
    /// (Exo / MLX-distributed / JACCL over Thunderbolt 5). The seam, runnable.
    ClusterPlan {
        /// Member unified memory sizes in GB, comma-separated (e.g. 512,512,512,512).
        #[arg(long)]
        members_gb: String,
        /// Bottleneck (slowest MEASURED) interconnect bandwidth, GB/s.
        #[arg(long, default_value_t = 80.0)]
        link_gbps: f32,
        /// Per-node held-back margin (OS + KV cache + activation buffers), GB.
        #[arg(long, default_value_t = 32.0)]
        margin_gb: f32,
        /// Total transformer layers of the model to plan.
        #[arg(long, default_value_t = 126)]
        model_layers: u32,
        /// The model's memory footprint, GB (must fit the summed usable memory).
        #[arg(long, default_value_t = 700.0)]
        model_gb: f32,
    },
    /// Batch-throughput benchmark: load a GGUF model and sweep batch sizes, timing
    /// batched vs serial decode. Device-agnostic — it measures whatever backend the
    /// binary was built for (Metal on macOS, CUDA when built `--features cuda`), so the
    /// SAME command produces comparable Apple-Silicon and NVIDIA numbers. Prints a
    /// human table to stderr and a machine-readable JSON record to stdout (redirect
    /// stdout to capture just the JSON). No network; downloads the GGUF once if absent.
    BenchBatch {
        /// Model ref (e.g. llama-3.2-1b-instruct-q4, qwen2.5-7b-instruct-q4).
        #[arg(long, default_value = "llama-3.2-1b-instruct-q4")]
        model: String,
        /// Max new tokens to generate per request (decode length; decode is where
        /// batching pays off, so keep this realistic, not tiny).
        #[arg(long, default_value_t = 48)]
        max_tokens: u32,
        /// Batch sizes to sweep, comma-separated (e.g. 1,2,4,8,16,32).
        #[arg(long, default_value = "1,2,4,8,16,32")]
        batch_sizes: String,
        /// The prompt every request in the batch runs (identical prompts keep the
        /// measurement about batching, not prompt variance; greedy output is then
        /// also checkable for the batched==serial invariant).
        #[arg(
            long,
            default_value = "Write a detailed paragraph about the ocean and its wonders:"
        )]
        prompt: String,
        /// Gate mode: exit non-zero if batched output ever diverges from serial. Off by
        /// default (a throughput benchmark records divergence as data, since GPU
        /// reduction-order can legitimately flip a greedy tie without invalidating the
        /// tok/s). Turn ON to use this as a byte-determinism gate.
        #[arg(long, default_value_t = false)]
        require_deterministic: bool,
        /// Repetitions per batch size (docs/CREED_AND_PATH_TO_TEN.md, "Benchmark
        /// harness validity" 6→6.5). 1 (default) reproduces the original single-run
        /// behavior exactly. >1 reports median/min/CV per sweep point instead of a
        /// single point estimate, and warns when CV exceeds 10% — cheap insurance
        /// against publishing an unexplained one-off anomaly as if it were stable.
        #[arg(long, default_value_t = 1)]
        reps: u32,
        /// Prompt-length regime (docs/internal/CREED_AND_PATH_TO_TEN.md, "Batching
        /// efficiency" 8→9). `identical` (default) runs the same `--prompt` in every
        /// row — the theoretical best case, since `generate_batch` buckets by EXACT
        /// token length and identical prompts all land in ONE full-width bucket.
        /// `mixed` gives each row a DIFFERENT-length prompt drawn from a spread of
        /// length classes, so the batch fragments into several narrower buckets —
        /// the honest real-traffic case, where a fleet sees prompts of many shapes,
        /// not one. Publishing both curves side by side shows the best case and the
        /// number real mixed traffic actually achieves. In `mixed` mode the
        /// `--prompt` string seeds the shared stem the length classes extend; the
        /// batched==serial invariant is still enforced per-row (each row's batched
        /// output must match that SAME row's own serial decode).
        #[arg(long, default_value = "identical")]
        mode: String,
    },
    /// Sustained-load thermal benchmark (docs/internal/CREED_AND_PATH_TO_TEN.md,
    /// "Thermal sustained-vs-peak throughput on fanless Apple Silicon" 3→4): drive
    /// REAL `batch_infer`-shaped decode continuously for `--minutes` (default 8,
    /// i.e. within the 5-10 minute range a real batch job actually experiences —
    /// NOT the 20-second peak-only probe `bench`'s thermal_ok proxy uses) and record
    /// throughput in rolling windows (default 30s) instead of a single peak sample.
    /// Device-agnostic (Metal on macOS, CUDA with `--features cuda`), so a fanless
    /// Mac's real throttle curve and a fanned/desktop box's flat curve are both
    /// measured with the SAME harness. Prints a human window-by-window table to
    /// stderr and a machine-readable JSON record (peak, sustained mean of the last
    /// 25% of windows, and the sustained-vs-peak gap as a percentage) to stdout.
    BenchSustained {
        /// Model ref (e.g. llama-3.2-1b-instruct-q4).
        #[arg(long, default_value = "llama-3.2-1b-instruct-q4")]
        model: String,
        /// Max new tokens generated per request in the sustained loop.
        #[arg(long, default_value_t = 48)]
        max_tokens: u32,
        /// Batch width held constant for the whole sustained run — this benchmark is
        /// about TIME (thermal decay), not the batch-width sweep `bench-batch` already
        /// covers, so one representative batch size runs continuously.
        #[arg(long, default_value_t = 8)]
        batch: usize,
        /// The prompt every request in the batch runs (identical prompts keep the
        /// measurement about sustained decode, not prompt variance).
        #[arg(
            long,
            default_value = "Write a detailed paragraph about the ocean and its wonders:"
        )]
        prompt: String,
        /// Total wall-clock duration of the sustained run, minutes. Default 8 sits
        /// inside the "what a real batch job actually experiences" 5-10 minute band
        /// the rung calls for.
        #[arg(long, default_value_t = 8)]
        minutes: u64,
        /// Width of each rolling throughput window, seconds.
        #[arg(long, default_value_t = 30)]
        window_secs: u64,
    },
    /// Concurrency-knob benchmark (docs/internal/CREED_AND_PATH_TO_TEN.md, "Agent
    /// concurrency & parallelism model" 7→7.5): drive a synthetic mixed embed +
    /// batch_infer workload through the REAL `tokio::Semaphore` + `ModelPool` +
    /// `JobRunner` dispatch path (the exact objects `run`'s main loop uses — see
    /// `poll_and_spawn`'s `sem.clone().acquire_owned()` pattern) at each of a
    /// sweep of permit counts, replacing the currently unvalidated `[2,4]`
    /// concurrency-default clamp with real measured data instead of a guess.
    /// Prints a human table to stderr and a machine-readable JSON record to
    /// stdout. No network — the model pool loads real weights (cached after
    /// first run) but nothing talks to a control plane.
    BenchConcurrency {
        /// Permit counts to sweep, comma-separated (e.g. 1,2,4).
        #[arg(long, default_value = "1,2,4")]
        permits: String,
        /// Number of embed tasks (job_type=embed) in the synthetic mixed batch,
        /// per permit level. Each is a small (8-line) JSONL chunk, matching the
        /// real embed runner's typical dispatch size.
        #[arg(long, default_value_t = 8)]
        embed_tasks: usize,
        /// Number of batch_infer tasks (job_type=batch_infer) in the synthetic
        /// mixed batch, per permit level.
        #[arg(long, default_value_t = 8)]
        llama_tasks: usize,
        /// Llama model ref for the batch_infer tasks.
        #[arg(long, default_value = "llama-3.2-1b-instruct-q4")]
        model: String,
        /// Max new tokens per batch_infer task (kept small — this benchmarks the
        /// concurrency knob, not decode length; see bench-batch/bench-sustained
        /// for that axis).
        #[arg(long, default_value_t = 24)]
        max_tokens: u32,
    },
    /// Print the agent version and exit.
    Version,
}

fn init_tracing() {
    use tracing_subscriber::{fmt, EnvFilter};
    let filter = EnvFilter::try_from_default_env().unwrap_or_else(|_| EnvFilter::new("info"));
    // Logs to STDERR so stdout stays clean for the machine-readable JSON the `bench`
    // and `bench-batch` subcommands print (a harness redirects stdout to capture just
    // the record). stderr is the conventional stream for diagnostics; under
    // systemd/launchd/docker both streams are still captured.
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

/// Current LOCAL hour (0..=23) for quiet-hours checks, via the real OS
/// timezone/DST database.
///
/// PATCH (P-real-platform-signals, docs/internal/CREED_AND_PATH_TO_TEN.md, "Agent
/// idle footprint & startup overhead" 7→8): this used to be a coarse UTC hour
/// (`(now_unix() / 3600) % 24`), documented honestly as a known bug — "operators
/// set quiet_hours in the agent's local timezone === UTC on a server", silently
/// shifting a US supplier's configured quiet window by 5-8 hours (more with DST).
/// `libc::localtime_r` asks the OS itself (its tz database, which already knows
/// this host's real zone and whether DST is active today) for the real local-time
/// breakdown — the POSIX-standard way to do this without vendoring a date/tz crate.
fn current_hour_local() -> u8 {
    // SAFETY: `time(NULL)` cannot fail. `localtime_r` writes into our own stack
    // `tm` (thread-safe, unlike `localtime`) and can only fail by leaving fields
    // at their zeroed default, which still yields a valid (if wrong) 0..=23 hour —
    // never UB, never a panic.
    unsafe {
        let now = libc::time(std::ptr::null_mut());
        let mut tm: libc::tm = std::mem::zeroed();
        libc::localtime_r(&now, &mut tm);
        tm.tm_hour.clamp(0, 23) as u8
    }
}

/// Real battery-vs-AC detection (docs/internal/CREED_AND_PATH_TO_TEN.md, "Agent
/// idle footprint & startup overhead" 7→8): delegates to `hardware::on_battery`,
/// which reads macOS's IOKit power-source registry in-process via
/// `IOPSGetProvidingPowerSourceType` — replacing a `pmset -g batt` subprocess
/// spawned on every poll cycle (the audit's own count: "~6,500 times a day").
/// Off macOS (the CUDA/Linux lane) `hardware::on_battery` returns the honest
/// constant `false` (a rented GPU box is never on battery) rather than spawning
/// a binary that doesn't exist on that platform.
fn on_battery() -> bool {
    hardware::on_battery()
}

/// Sample coarse CPU utilization (0..=100) for the heartbeat.
fn cpu_pct(sys: &mut System) -> f32 {
    sys.refresh_cpu_usage();
    let cpus = sys.cpus();
    if cpus.is_empty() {
        return 0.0;
    }
    let sum: f32 = cpus.iter().map(|c| c.cpu_usage()).sum();
    sum / cpus.len() as f32
}

/// REAL memory reading for the throttle gate, against the resource that actually
/// limits THIS box. On the CUDA lane (`device_label() == "cuda"`) that is VRAM,
/// not host RAM — a GPU box has plenty of host RAM, so the host-RAM throttle would
/// never trip even when the card is nearly full (e.g. a 24 GB card handed a ~20 GB
/// job). We read free+total VRAM via nvidia-smi and feed it through the SAME
/// `evaluate_memory_throttle` thresholds. If CUDA is active but VRAM can't be read
/// we surface that and fall back to the host-RAM snapshot (mirrors
/// `detect_and_benchmark`) rather than fabricating headroom. On Apple/CPU the
/// gating resource is unified/host memory, so we keep the host-RAM path.
fn throttle_snapshot() -> hardware::MemorySnapshot {
    if models::device_label() == "cuda" {
        match hardware::read_vram_snapshot() {
            Some(vram) => return vram,
            None => tracing::warn!(
                "CUDA lane active but nvidia-smi VRAM read failed; gating on host RAM this cycle"
            ),
        }
    }
    hardware::read_memory_snapshot()
}

/// GPU utilization (%) + temperature (°C) for the heartbeat, CUDA lane only. Off the
/// NVIDIA lane (Apple/CPU) there is no discrete GPU to query, so report an honest
/// 0.0/None rather than a fabricated number; on CUDA a failed nvidia-smi read also
/// degrades to 0.0/None (never faked).
fn gpu_telemetry() -> (f32, Option<f32>) {
    if models::device_label() == "cuda" {
        if let Some((util, temp)) = hardware::read_gpu_telemetry() {
            return (util, temp);
        }
    }
    (0.0, None)
}

#[tokio::main]
async fn main() -> Result<()> {
    let cli = Cli::parse();
    match cli.command {
        Command::Version => {
            println!("cx-agent {AGENT_VERSION}");
            Ok(())
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
            )
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
        Command::ClusterPlan {
            members_gb,
            link_gbps,
            margin_gb,
            model_layers,
            model_gb,
        } => run_cluster_plan(&members_gb, link_gbps, margin_gb, model_layers, model_gb),
        Command::Run { config } => {
            init_tracing();
            // Contain a DIRECT binary launch too, not only the .app install path: on
            // macOS, if we are not already under the seatbelt sandbox and a profile can
            // be found, re-exec ourselves under `sandbox-exec` (Security Posture 8→9,
            // docs/SECURITY.md). This does not return on success (execv replaces the
            // image); it is a no-op on non-macOS, when already sandboxed, or when no
            // profile is found — in which case the agent runs honestly unsandboxed and
            // says so in the log rather than refusing to start.
            reexec_under_sandbox_if_needed();
            let cfg = AgentConfig::load(&config)
                .with_context(|| format!("loading config {}", config.display()))?;
            run_agent(cfg).await
        }
    }
}

/// `bench` subcommand: detect + benchmark, print WorkerCapability JSON.
async fn run_bench(config: Option<PathBuf>) -> Result<()> {
    // Read supplier id + the configured engine tag from the config when present, so
    // `bench` prints the SAME engine the agent will advertise; with no config it is the
    // default Candle path (engine "candle").
    let (supplier_id, engine) = match config {
        Some(path) => {
            let cfg = AgentConfig::load(&path)
                .with_context(|| format!("loading config {}", path.display()))?;
            (cfg.supplier_id, cfg.inference_backend.engine_tag())
        }
        None => (
            uuid::Uuid::nil(),
            config::InferenceBackend::default().engine_tag(),
        ),
    };
    // `bench` is informational only — reservation price is 0.0 (not advertised).
    // A fresh, throwaway pool: this subcommand prints a JSON record and exits, so
    // there is no later real dispatch to keep the load warm for.
    let pool = ModelPool::new();
    let cap = hardware::detect_and_benchmark(supplier_id, AGENT_VERSION, 0.0, engine, &pool).await;
    println!("{}", serde_json::to_string_pretty(&cap)?);
    Ok(())
}

/// Population stddev / mean, as a percentage (docs/CREED_AND_PATH_TO_TEN.md,
/// "Benchmark harness validity" 6→6.5). 0.0 for a single sample (no dispersion to
/// report, not a divide-by-zero) and 0.0 for an all-zero sample (nothing to
/// normalize by, not NaN/inf).
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

/// The middle value of a sorted-in-place copy (nearest-rank; even counts take the
/// lower-middle element — fine for a small rep count, not meant to be textbook-exact).
fn median(xs: &mut [f64]) -> f64 {
    xs.sort_by(|a, b| a.partial_cmp(b).unwrap());
    xs[xs.len() / 2]
}

/// Prompt-length regime for `bench-batch` (docs/internal/CREED_AND_PATH_TO_TEN.md,
/// "Batching efficiency" 8→9).
#[derive(Clone, Copy, PartialEq, Eq, Debug)]
enum BenchMode {
    /// Every row runs the SAME prompt — the theoretical best case, since
    /// `generate_batch` buckets by exact token length and identical prompts all
    /// land in one full-width bucket.
    Identical,
    /// Each row runs a DIFFERENT-length prompt — the honest real-traffic case,
    /// where a fleet sees prompts of many shapes and `generate_batch`'s
    /// exact-length bucketing fragments the batch into several narrower buckets.
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

/// Build a batch of `b` prompts for one sweep point, per the chosen regime.
///
/// `Identical`: `b` copies of `stem` — the existing best-case behavior, byte-for-byte.
///
/// `Mixed`: `b` prompts of DELIBERATELY VARIED length. Each row `i` extends `stem`
/// with `i % LEN_CLASSES` filler clauses drawn from a fixed pool, so the batch spans
/// a spread of distinct token lengths. Because `generate_batch` buckets by EXACT
/// token length, this fragments the batch into several narrower buckets — exactly what
/// real mixed traffic does, and the reason its measured speedup sits below the
/// identical-prompt ceiling. Deterministic (no RNG) so a published mixed curve is
/// reproducible and so this generator is unit-testable without a model.
fn build_bench_prompts(stem: &str, b: usize, mode: BenchMode) -> Vec<String> {
    match mode {
        BenchMode::Identical => std::iter::repeat_n(stem.to_string(), b).collect(),
        BenchMode::Mixed => {
            // A fixed pool of filler clauses. Row i appends `i % LEN_CLASSES` of them,
            // producing LEN_CLASSES distinct lengths cycled across the batch. Chosen so
            // that even a small batch (b>=2) already fragments into multiple buckets.
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

/// `bench-batch` subcommand: sweep batch sizes on a real GGUF model, timing batched
/// vs serial decode. The batched path (generate_batch) shares the decode step across
/// the batch — the core throughput lever CX relies on — so this quantifies the win on
/// whatever backend the binary was built for. Emits a JSON record on stdout; a human
/// table on stderr. Also asserts the batched==serial greedy invariant so a throughput
/// number can never be reported over an INCORRECT (diverged) batched decode.
///
/// Two regimes (`--mode`, docs/internal/CREED_AND_PATH_TO_TEN.md "Batching efficiency"
/// 8→9): `identical` (default) runs one prompt in every row — the best case; `mixed`
/// gives each row a different-length prompt — the honest real-traffic case, where
/// exact-length bucketing fragments the batch into narrower buckets.
fn run_bench_batch(
    model: &str,
    max_tokens: u32,
    batch_sizes: &str,
    prompt: &str,
    require_deterministic: bool,
    reps: u32,
    mode: &str,
) -> Result<()> {
    use std::time::Instant;

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
            // Reject 0: a zero-size batch runs zero sequences, so tok/s and
            // per-request throughput divide by zero → NaN → serializes as JSON null,
            // breaking the stdout contract AND vacuously "passing" the batched==serial
            // check (all() over an empty vec is true). Never let it into the sweep.
            if n == 0 {
                anyhow::bail!("batch size must be >= 1 (got 0 in --batch-sizes)");
            }
            Ok(n)
        })
        .collect::<Result<Vec<_>>>()?;
    if sizes.is_empty() {
        anyhow::bail!("no batch sizes given (e.g. --batch-sizes 1,2,4,8,16,32)");
    }

    let device = models::device_label();
    // Report the same class axis the scheduler pins on, so a bench record is
    // attributable to an exact (device, engine, build_hash). The bench binary is the
    // default Candle path; the engine tag matches what `run` would advertise.
    let engine = config::InferenceBackend::default().engine_tag();
    let build_hash = hardware::engine_build_hash(engine, AGENT_VERSION);
    eprintln!("== cx-agent bench-batch ==");
    eprintln!(
        "device={device} model={model} max_tokens={max_tokens} mode={} build_hash={build_hash}",
        mode.label()
    );

    let mut be =
        runners::LlamaBackend::load(model).map_err(|e| anyhow::anyhow!("load {model}: {e}"))?;

    // Warm-up: one generate outside the timed loop so weight upload / first-kernel
    // JIT / autotune costs land here, not in the measured serial baseline.
    let (warm_text, _) = be
        .generate(prompt, max_tokens)
        .map_err(|e| anyhow::anyhow!("warmup generate: {e}"))?;

    // The set of DISTINCT prompts the sweep will run. In identical mode that is just
    // `{prompt}`; in mixed mode it is every distinct-length prompt that appears in the
    // widest batch (row i's prompt depends only on i, so the largest batch size covers
    // every shorter one's prompts too). The serial baseline runs EACH distinct prompt
    // once — a fair "no batching" reference for the SAME varied traffic the batched
    // path handles — and its per-prompt output seeds the per-row batched==serial gate.
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

    // Serial baseline: run each distinct prompt once through the scalar path, summing
    // tokens and wall time. serial_tps is the aggregate no-batching throughput; the
    // per-prompt greedy output is remembered so each batched row is checked against
    // ITS OWN prompt's serial decode (identical mode reduces to the original single
    // reference, since there is exactly one distinct prompt).
    let mut expected: std::collections::HashMap<String, String> =
        std::collections::HashMap::with_capacity(distinct_prompts.len());
    let mut serial_total_tok = 0usize;
    let mut serial_total_dt = 0.0f64;
    for p in &distinct_prompts {
        let t = Instant::now();
        let (text, tok) = be
            .generate(p, max_tokens)
            .map_err(|e| anyhow::anyhow!("serial generate: {e}"))?;
        serial_total_dt += t.elapsed().as_secs_f64();
        serial_total_tok += tok;
        expected.insert(p.clone(), text);
    }
    // A zero-token serial baseline (model emitted EOS immediately for every distinct
    // prompt) would make every speedup = tps/0 = ±inf/NaN → JSON null downstream. That
    // is a degenerate input (bad model/prompt), not a measurable run — fail loudly.
    if serial_total_tok == 0 {
        anyhow::bail!(
            "serial baseline produced 0 tokens for model {model:?} — cannot benchmark \
             (check the model ref and prompt)"
        );
    }
    let serial_tps = serial_total_tok as f64 / serial_total_dt;
    eprintln!(
        "serial baseline ({} distinct prompt(s)): {serial_total_tok} tok in {serial_total_dt:.2}s = {serial_tps:.1} tok/s",
        distinct_prompts.len()
    );

    #[derive(serde::Serialize)]
    struct SweepRow {
        batch: usize,
        reps: usize,
        wall_s: f64,
        total_tokens: usize,
        // Median across `reps` timed runs (== the single measurement when reps=1).
        tokens_per_s: f64,
        // Dispersion across `reps` timed runs. min_tok_s == tokens_per_s and
        // cv_pct == 0.0 when reps=1 — there is nothing to disperse over one run.
        min_tok_s: f64,
        cv_pct: f64,
        per_request_tok_s: f64,
        speedup_vs_serial: f64,
        batched_equals_serial: bool,
    }

    let mut rows: Vec<SweepRow> = Vec::with_capacity(sizes.len());
    let mut peak_tps = serial_tps;
    for &b in &sizes {
        let prompts: Vec<String> = build_bench_prompts(prompt, b, mode);
        let mut tps_samples = Vec::with_capacity(reps);
        let mut last_wall = 0.0;
        let mut last_total_tok = 0usize;
        let mut all_equal_serial = true;
        for rep in 0..reps {
            let t = Instant::now();
            let res = be
                .generate_batch(&prompts, max_tokens)
                .map_err(|e| anyhow::anyhow!("generate_batch b={b} rep={rep}: {e}"))?;
            // TEST-ONLY synthetic-slowdown hook for proving the regression gate
            // (docs/internal/CREED_AND_PATH_TO_TEN.md, "Benchmark harness validity"
            // 7→8) actually catches a real throughput drop, without touching the
            // determinism-sensitive Candle kernel path itself. Off by default (the
            // env var is unset in every real run, so `extra_ms` is 0 and this is a
            // no-op sleep(0) — never affects a real published number). Only a
            // deliberate `CX_BENCH_SYNTHETIC_DELAY_MS` in the environment activates
            // it, which is exactly how the gate's own proof run exercises it.
            if let Ok(extra_ms) = std::env::var("CX_BENCH_SYNTHETIC_DELAY_MS") {
                if let Ok(ms) = extra_ms.parse::<u64>() {
                    std::thread::sleep(Duration::from_millis(ms));
                }
            }
            let wall = t.elapsed().as_secs_f64();
            let total_tok: usize = res.iter().map(|(_, n)| n).sum();
            let tps = total_tok as f64 / wall;
            // Byte-determinism vs serial: does batched greedy output match
            // one-at-a-time? This is a SEPARATE property from throughput. On
            // Apple/Metal it holds at every batch size (mask-cache +
            // active-set-shrink determinism). On CUDA it can FLIP a greedy
            // argmax TIE because GPU float reductions vary with batch
            // composition — the batched tokens are still a valid greedy decode,
            // just not byte-identical to serial. So a divergence does NOT
            // invalidate the tok/s; it is recorded as a determinism data point,
            // and only a gate (--require-deterministic) treats it as failure.
            // With reps>1, ANY rep diverging marks the whole batch size as
            // non-deterministic — a single lucky repeat must not hide real flakiness.
            // `generate_batch` returns results in input order, so row j's output is
            // checked against ITS OWN prompt's serial decode (in mixed mode different
            // rows have different prompts; in identical mode every prompt maps to the
            // one reference, exactly as before).
            all_equal_serial &= res
                .iter()
                .zip(&prompts)
                .all(|((text, _), p)| expected.get(p).is_some_and(|exp| exp == text));
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
            "batch={b:>3}: {:>5} tok in {:>6.2}s = {:>7.1} tok/s (median of {reps}, min={tps_min:.1}, CV={cv_pct:.1}%)  ({:.2}x serial){}{}",
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
        "peak {peak_tps:.1} tok/s = {:.2}x serial · byte-determinism vs serial: {}",
        peak_tps / serial_tps,
        if all_deterministic {
            "IDENTICAL at every batch size".to_string()
        } else {
            format!("DIVERGES at batch {diverged:?} (GPU reduction-order tie-flip; throughput still valid)")
        }
    );

    let record = serde_json::json!({
        "kind": "bench_batch",
        "device": device,
        "build_hash": build_hash,
        "model": model,
        "max_tokens": max_tokens,
        // Prompt-length regime this record was measured under (identical = best case,
        // mixed = honest real-traffic case). Lets a published record be attributed to
        // its regime, so the two curves are never confused.
        "mode": mode.label(),
        "distinct_prompts": distinct_prompts.len(),
        "prompt_preview": prompt.chars().take(60).collect::<String>(),
        "warmup_ok": !warm_text.is_empty(),
        "serial_baseline_tok_s": serial_tps,
        "peak_tok_s": peak_tps,
        "peak_speedup_vs_serial": peak_tps / serial_tps,
        // Byte-determinism vs serial (NOT a throughput validity flag). true = batched
        // output byte-identical to serial at every batch size; false = at least one
        // batch diverged (see `diverged_batches`).
        "batched_deterministic_vs_serial": all_deterministic,
        "diverged_batches": diverged,
        "sweep": rows,
    });
    println!("{}", serde_json::to_string_pretty(&record)?);

    // Divergence is a data point, not a failure — the tok/s are real. Only a caller that
    // explicitly demands byte-determinism (--require-deterministic, e.g. a verification
    // gate) treats a divergence as a hard, non-zero-exit failure.
    if require_deterministic && !all_deterministic {
        anyhow::bail!(
            "batched decode diverged from serial at batch {diverged:?} and \
             --require-deterministic was set — failing the determinism gate"
        );
    }
    Ok(())
}

/// PURE arithmetic over a rolling-window throughput curve (docs/internal/
/// CREED_AND_PATH_TO_TEN.md, "Thermal sustained-vs-peak throughput on fanless
/// Apple Silicon" 3→4). `windows` is tok/s per fixed-width rolling window, in time
/// order. Returns (peak, sustained_mean, gap_pct) where `sustained_mean` is the
/// mean of the LAST 25% of windows (the steady-state regime after any thermal
/// ramp) and `gap_pct` is how much lower that is than the peak, as a percentage —
/// 0.0 when the run is flat, positive when it throttles. Pulled out as a pure
/// function (mirrors `coefficient_of_variation_pct`/`median` above) so the exact
/// sustained-vs-peak math is unit-testable without a 5-10 minute real run.
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

/// `bench-sustained` subcommand (docs/internal/CREED_AND_PATH_TO_TEN.md, "Thermal
/// sustained-vs-peak throughput on fanless Apple Silicon" 3→4): the REAL 5-10
/// minute sustained-load measurement the rung calls for, as a new invocation mode
/// on the existing harness (not new infrastructure) — `bench`'s THERMAL_SECS=20
/// probe and `bench-batch`'s single-shot sweep both stay unchanged; this is a
/// third, explicitly long-running mode for whoever wants the real curve. Drives
/// `generate_batch` continuously at a fixed batch width for `minutes` wall-clock
/// minutes, sampling tok/s in `window_secs`-wide rolling windows (default 30s) so
/// the actual sustained-vs-peak curve is visible, not collapsed into one number.
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
    let engine = config::InferenceBackend::default().engine_tag();
    let build_hash = hardware::engine_build_hash(engine, AGENT_VERSION);
    eprintln!("== cx-agent bench-sustained ==");
    eprintln!(
        "device={device} model={model} batch={batch} max_tokens={max_tokens} \
         duration={minutes}min window={window_secs}s build_hash={build_hash}"
    );
    eprintln!(
        "this is a REAL {minutes}-minute run, not a spot measurement — it will take \
         {minutes} real minutes."
    );

    let mut be =
        runners::LlamaBackend::load(model).map_err(|e| anyhow::anyhow!("load {model}: {e}"))?;
    let prompts: Vec<String> = std::iter::repeat_n(prompt.to_string(), batch).collect();

    // Warm-up outside the timed windows: first-kernel JIT / weight upload costs
    // must not land in window 0, or an unrelated one-time cost would be
    // misread as thermal throttling in the very first sample.
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
            "!! sustained throughput is {gap_pct:.1}% below peak — this box is throttling \
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

/// `bench-concurrency` subcommand (docs/internal/CREED_AND_PATH_TO_TEN.md, "Agent
/// concurrency & parallelism model" 7→7.5): sweep the REAL semaphore-plus-pool
/// concurrency knob against a synthetic mixed embed + batch_infer workload,
/// mirroring the exact dispatch shape `run`'s main loop uses (`sem.clone()
/// .acquire_owned()` then spawn — see `poll_and_spawn`) so the measured numbers
/// are about the real object graph, not a stand-in. For each permit count in the
/// sweep: build a FRESH `ModelPool` (so warm-load costs don't leak between sweep
/// points and skew the comparison) and a `Semaphore::new(permits)`, spawn every
/// embed + batch_infer task up front (all of them immediately try to acquire a
/// permit, exactly like real concurrent dispatch), and measure aggregate
/// wall-clock time and per-workload-type wall time. Prints a human table to
/// stderr and a machine-readable JSON record to stdout.
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
        anyhow::bail!("--embed-tasks and --llama-tasks are both 0 — nothing to benchmark");
    }

    let device = models::device_label();
    let engine = config::InferenceBackend::default().engine_tag();
    let build_hash = hardware::engine_build_hash(engine, AGENT_VERSION);
    eprintln!("== cx-agent bench-concurrency ==");
    eprintln!(
        "device={device} model={model} embed_tasks={embed_tasks} llama_tasks={llama_tasks} \
         max_tokens={max_tokens} build_hash={build_hash}"
    );
    eprintln!(
        "sweeping permits={permit_sweep:?} — this replaces the unvalidated [2,4] \
         concurrency-default clamp (config.rs::AgentConfig::concurrency) with real data"
    );

    // Fixed synthetic manifests (job_type is the only field the two runners
    // actually branch on; model_ref/constraints are set to real, representative
    // values). Built once, cloned per task below.
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

    // Warm both models once, OUTSIDE every timed sweep point, on a throwaway
    // pool — the sweep below always builds its OWN fresh pool per permit level
    // (see the loop), so this warm-up only pays the model-download / first-JIT
    // cost up front and never pollutes a measured point.
    eprintln!("warming models (cold load happens once, not counted in any sweep point)...");
    {
        let warm_pool = ModelPool::new();
        runners::EmbedRunner
            .run(&embed_manifest, &embed_input, &warm_pool)
            .await
            .map_err(|e| anyhow::anyhow!("warmup embed: {e}"))?;
        runners::BatchInferRunner
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
        // Fresh pool per sweep point: each permit level gets its own warm-once
        // load (paid here, not in the timed region below, since the model was
        // already warmed above and pool state doesn't carry across `ModelPool`
        // instances) so no sweep point's timing is contaminated by another
        // point's in-flight state or eviction behavior.
        let pool = Arc::new(ModelPool::new());
        // Re-warm this fresh pool's slot (cheap: hf-hub cache hit, no re-download)
        // so the timed region below measures dispatch/compute, not cold load.
        runners::EmbedRunner
            .run(&embed_manifest, &embed_input, &pool)
            .await
            .map_err(|e| anyhow::anyhow!("re-warm embed: {e}"))?;
        runners::BatchInferRunner
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
            set.spawn(async move {
                // Mirrors the real loop's `sem.clone().acquire_owned()` then
                // spawn pattern exactly (main's `poll_and_spawn` call site).
                let permit = sem.acquire_owned().await.expect("semaphore never closed");
                let t = Instant::now();
                let res = runners::EmbedRunner.run(&manifest, &input, &pool).await;
                drop(permit);
                ("embed", t.elapsed().as_secs_f64(), res.is_ok())
            });
        }
        for _ in 0..llama_tasks {
            let sem = sem.clone();
            let pool = pool.clone();
            let manifest = llama_manifest.clone();
            let input = llama_input.clone();
            set.spawn(async move {
                let permit = sem.acquire_owned().await.expect("semaphore never closed");
                let t = Instant::now();
                let res = runners::BatchInferRunner
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

    // Honest summary: report where added permits stopped meaningfully helping
    // (the rung's own proof-artifact language), rather than just dumping numbers.
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

/// `cluster-plan` subcommand (Plane B): turn MEASURED member memories + a measured
/// bottleneck link into the cluster's one-worker advertisement and shard layout,
/// then show what happens if a node drops. Pure (uses `cluster.rs`); no hardware,
/// no network. Honest by construction: the summed memory subtracts a real per-node
/// margin, the bandwidth is the bottleneck link, and EXECUTING the plan is flagged
/// as the external substrate's job — this only plans it.
fn run_cluster_plan(
    members_gb: &str,
    link_gbps: f32,
    margin_gb: f32,
    model_layers: u32,
    model_gb: f32,
) -> Result<()> {
    use cluster::{ClusterNode, ClusterTopology, Link, ReshardDecision};

    let mems: Vec<f32> = members_gb
        .split(',')
        .map(|s| s.trim())
        .filter(|s| !s.is_empty())
        .map(|s| s.parse::<f32>())
        .collect::<Result<_, _>>()
        .context("parsing --members-gb (expected comma-separated GB, e.g. 512,512,512,512)")?;
    if mems.len() < 2 {
        anyhow::bail!(
            "a cluster needs at least 2 members (got {}); a single Mac is a normal Plane-A worker",
            mems.len()
        );
    }

    let nodes: Vec<ClusterNode> = mems
        .iter()
        .map(|&m| ClusterNode {
            unified_memory_gb: m,
        })
        .collect();
    // Model the fabric as a ring at the supplied (measured) bottleneck bandwidth.
    let links: Vec<Link> = (0..nodes.len())
        .map(|i| Link {
            a: i,
            b: (i + 1) % nodes.len(),
            gbps: link_gbps,
            latency_us: 0.0,
        })
        .collect();
    let topo = ClusterTopology { nodes, links };

    let advert = topo
        .advertise(margin_gb)
        .context("topology does not form a cluster (need ≥2 members + a fabric)")?;
    println!("Plane B cluster plan (docs/PLANE_B.md) — MEASURED inputs, pure math");
    println!("  members          : {} Macs {:?} GB", mems.len(), mems);
    println!(
        "  advertises as    : apple_silicon_cluster, memory_gb={:.1} (summed usable, −{:.0} GB/node margin), memory_bw_gbps={:.1} (bottleneck link)",
        advert.memory_gb, margin_gb, advert.memory_bw_gbps
    );
    println!(
        "  model            : {} layers, {:.1} GB → fits: {}",
        model_layers,
        model_gb,
        topo.fits_model(margin_gb, model_gb)
    );

    if !topo.fits_model(margin_gb, model_gb) {
        println!(
            "  RESULT           : model does NOT fit summed usable memory ({:.1} GB) — this cluster would not advertise it.",
            topo.summed_usable_memory_gb(margin_gb)
        );
        return Ok(());
    }

    println!("  shard layout     : pipeline stages, contiguous layers per node");
    for s in topo.assign_shards(model_layers, margin_gb) {
        println!(
            "    node {:>2}        : layers [{:>3}, {:>3})  ({} layers)",
            s.node,
            s.first_layer,
            s.first_layer + s.layer_count,
            s.layer_count
        );
    }

    // Illustrate fault handling: drop the last member and re-decide.
    let mut survivors = topo.clone();
    survivors.nodes.pop();
    let n = survivors.nodes.len();
    survivors.links = if n >= 2 {
        (0..n)
            .map(|i| Link {
                a: i,
                b: (i + 1) % n,
                gbps: link_gbps,
                latency_us: 0.0,
            })
            .collect()
    } else {
        Vec::new()
    };
    match survivors.on_membership_change(model_layers, model_gb, margin_gb) {
        ReshardDecision::Reshard(plan) => println!(
            "  if a node drops  : RE-SHARD across {} survivors ({} layers still covered)",
            survivors.nodes.len(),
            plan.iter().map(|s| s.layer_count).sum::<u32>()
        ),
        ReshardDecision::Offline => println!(
            "  if a node drops  : go OFFLINE — survivors ({:.1} GB usable) can't hold the model; never run a partial shard",
            survivors.summed_usable_memory_gb(margin_gb)
        ),
    }

    println!(
        "  EXECUTION        : planning only. Running this shard layout needs the EXTERNAL co-located substrate"
    );
    println!(
        "                     (Exo / MLX-distributed / JACCL over Thunderbolt 5, macOS 26.2) — see PLANE_B.md §3,§5."
    );
    Ok(())
}

/// Execute one dispatched task end-to-end and build the commit to submit.
///
/// Real path (action plan §K): GET the presigned `input_url` → run the backend →
/// PUT the result JSON to the presigned `output_url` → WIPE the in-memory input
/// and result buffers. Job bytes live in memory only; nothing is written to disk
/// unencrypted, and the buffers are zeroized the moment the commit is built.
///
/// Eight narrow, independently-meaningful parameters (the single real call site
/// unpacks them straight from `WorkCtx`) — not a design smell worth a bundling
/// struct for one caller.
#[allow(clippy::too_many_arguments)]
async fn execute_task(
    task: &TaskDispatch,
    cap: &WorkerCapability,
    runners: &[Box<dyn JobRunner>],
    pool: &ModelPool,
    s3: &reqwest::Client,
    checkpoint_secs: u64,
    memory_headroom_gb: f32,
    max_memory_pct: f32,
) -> Result<TaskCommit, RunError> {
    let manifest = &task.manifest;
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
    let runner = dispatch(manifest, cap, runners).await?;
    tracing::info!(task = %task.task_id, backend = runner.backend_name(), "executing task");

    // 1. GET the presigned input (the task's JSONL chunk) into memory.
    let mut input = s3_get(s3, &task.input_url)
        .await
        .map_err(|e| RunError::Inference {
            backend: runner.backend_name(),
            msg: format!("fetching input_url: {e:#}"),
        })?;

    // Intra-task checkpointing (additive contract delta): when the dispatch
    // carries `partial_put_url` and the operator's cadence is on, the generative
    // runners may periodically PUT the rows completed so far to it, so the
    // stuck-run watchdog can hand the buyer mid-chunk progress if it kills the
    // job. Absent URL or cadence 0 → inert; every other runner ignores it via
    // the trait default. The final result PUT below is byte-for-byte unchanged.
    //
    // PATCH (P-mid-job-preempt, docs/internal/CREED_AND_PATH_TO_TEN.md, "Memory
    // management & dynamic throttling internals" 7→8): `with_preempt_check`
    // attaches a REAL memory-pressure probe re-evaluated between every
    // generation slice — a real `hardware::read_memory_snapshot()` fed through
    // the SAME `evaluate_memory_throttle` governor the pre-claim gate already
    // uses (never a second, divergent threshold). This is the WARN-level signal
    // the rung asks for: the moment this box would ALREADY refuse a new claim
    // (headroom breached or the utilization ceiling crossed), an in-progress job
    // also stops starting new slices instead of running to a real OOM.
    let mem_headroom_gb = memory_headroom_gb;
    let mem_max_pct = max_memory_pct;
    let ckpt =
        runners::Checkpointer::new(task.partial_put_url.clone(), checkpoint_secs, s3.clone())
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

    // 2. Run the model through the WARM pool. On failure, still wipe input first.
    let output = match runner
        .run_with_checkpoints(manifest, &input, pool, &ckpt)
        .await
    {
        Ok(o) => o,
        Err(e) => {
            wipe(&mut input);
            return Err(e);
        }
    };
    wipe(&mut input);

    let (duration_ms, tokens_used) = (output.duration_ms, output.tokens_used);

    // 3. PUT the result to the presigned output URL. Large results (>16 MiB) are
    // staged to a temp file ENCRYPTED with an ephemeral AES-GCM key, uploaded from
    // there, then wiped + removed — nothing large sits on disk in the clear (action
    // plan §K). Small results stay entirely in memory. The content type follows the
    // payload: JSON for every job by default, octet-stream for the opt-in binary
    // embedding artifact (PLANE_D D5/D15) so the object is stored as opaque bytes.
    let mut result = output.result;
    let content_type = if output.binary {
        "application/octet-stream"
    } else {
        "application/json"
    };
    let put = if result.len() > STAGING_THRESHOLD {
        put_via_encrypted_staging(s3, &task.output_url, &result, content_type).await
    } else {
        s3_put_bytes(s3, &task.output_url, &result, content_type).await
    };
    put.map_err(|e| RunError::Inference {
        backend: runner.backend_name(),
        msg: format!("putting output_url: {e:#}"),
    })?;

    // Control Plane Hot Path 8->9 (docs/internal/CREED_AND_PATH_TO_TEN.md, "Get
    // result-commit off the S3 critical path"): hash the EXACT bytes just PUT to
    // output_url, so the control plane can trust a hash-to-hash redundancy
    // compare for byte-exact job types instead of re-downloading this same
    // object synchronously inside the commit request. Computed AFTER the PUT
    // succeeds (so a failed upload never reports a hash for bytes the server
    // doesn't have) and over `result` itself — never a re-read of the uploaded
    // object — so this is provably the hash of what actually landed.
    let result_sha256 = sha256_hex(&result);

    let commit = TaskCommit {
        task_id: task.task_id,
        // Echo the object key the control plane told us to write to. Prefer the
        // explicit `result_key`; fall back to a job/task-derived key if absent.
        result_key: if task.result_key.is_empty() {
            format!("results/{}/{}.json", task.job_id, task.task_id)
        } else {
            task.result_key.clone()
        },
        duration_ms,
        tokens_used,
        result_sha256,
        hardware_temp_c: None,
    };

    // 4. Wipe the result buffer now that it is durably PUT, hashed, and the
    // commit built.
    wipe(&mut result);
    Ok(commit)
}

/// Fail-closed mirror of the server's exact-cell claim authority. This consumes
/// only generated production cells and the worker capability already registered
/// with the control plane; compatibility arrays and local runner availability are
/// deliberately insufficient. A stale matrix therefore stops before input bytes
/// are fetched or model work begins.
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
    matrix_sha256 == runtime_matrix_generated::RUNTIME_MATRIX_SHA256
        && runtime_matrix_generated::ADVERTISED_RUNTIME_CAPABILITIES
            .iter()
            .any(|cell| {
                cell.id == cell_id
                    && cell.runtime == runtime_id
                    && cell.engine == engine
                    && cell.device == device
                    && cell.hardware_classes.contains(&hw_class.as_wire_str())
                    && cell.job == job
                    && cell.model == Some(model)
                    && cell.model_kind == Some(model_kind.as_wire_str())
            })
}

/// Overwrite a buffer's bytes with zeros and drop its capacity. Best-effort
/// in-memory wipe of decrypted job data per the security mandate.
fn wipe(buf: &mut Vec<u8>) {
    for b in buf.iter_mut() {
        *b = 0;
    }
    buf.clear();
    buf.shrink_to_fit();
}

/// Lowercase-hex SHA-256 of `data`, in the exact shape the control plane's
/// `nullSHA256Hex` validator requires (64 lowercase hex chars) — Control Plane
/// Hot Path 8->9, docs/internal/CREED_AND_PATH_TO_TEN.md "Get result-commit off
/// the S3 critical path". Pure and directly testable so a formatting slip
/// (wrong case, wrong digest) fails fast in `cargo test`, not only in a real
/// end-to-end run against the control plane.
fn sha256_hex(data: &[u8]) -> String {
    use sha2::{Digest, Sha256};
    let mut h = Sha256::new();
    h.update(data);
    h.finalize().iter().map(|b| format!("{b:02x}")).collect()
}

/// Hard ceiling on a single downloaded task input. Bounds the memory a
/// malformed/oversized object (or a compromised store) can force this agent
/// to buffer for one task — a supplier's Mac has no business ever downloading
/// a multi-GB single-task chunk given the control plane's own adaptive
/// chunking targets ~45s-of-work objects. 512 MiB is generous headroom above
/// any real chunk while still being a real, finite bound instead of none.
const MAX_INPUT_DOWNLOAD_BYTES: u64 = 512 * 1024 * 1024;

/// Data Transfer & Artifact I/O 4.5->5 (docs/internal/CREED_AND_PATH_TO_TEN.md):
/// a single transient network blip (a reset connection, a momentary 5xx from the
/// storage backend, a connect timeout) used to fail the WHOLE task immediately —
/// discarding any compute already spent on it — with zero retry. `TRANSFER_RETRIES`
/// is the number of ADDITIONAL attempts after the first; backoff doubles each time
/// starting at `TRANSFER_RETRY_BASE_DELAY`. Deliberately narrow: only retries
/// connect/timeout errors and 5xx responses (a genuinely transient failure) — a
/// 4xx (e.g. an expired or malformed presigned URL) is never transient and fails
/// fast on the first attempt, exactly as before.
const TRANSFER_RETRIES: u32 = 3;
const TRANSFER_RETRY_BASE_DELAY: Duration = Duration::from_millis(250);

/// True if `status` is worth retrying (server-side, possibly-transient) rather
/// than treated as a permanent rejection of this specific request.
fn is_retryable_status(status: reqwest::StatusCode) -> bool {
    status.is_server_error() || status == reqwest::StatusCode::TOO_MANY_REQUESTS
}

/// True if a request-level error (never got a response at all) is worth retrying:
/// a connect failure or a timeout, not e.g. a URL-parse error.
fn is_retryable_reqwest_err(e: &reqwest::Error) -> bool {
    e.is_connect() || e.is_timeout()
}

/// GET a presigned URL into memory (no auth header — the signature is in the
/// URL). Bounded by `MAX_INPUT_DOWNLOAD_BYTES`: an honestly-reported
/// `Content-Length` over the cap is rejected before any body bytes are read;
/// a body that turns out to exceed the cap despite an absent or understated
/// `Content-Length` is caught (and its memory dropped) immediately after the
/// read completes. Neither path lets an oversized object become an
/// unbounded-memory task on a supplier's machine. Transient failures (connect/
/// timeout/5xx) are retried with exponential backoff; the oversized-body checks
/// are never retried (retrying would not change the object's real size).
///
/// Range-resume (Data Transfer & Artifact I/O 5->6): if a prior attempt got far
/// enough to read SOME body bytes before failing (a body-read error mid-stream,
/// distinct from a connect/status failure which has zero bytes to resume from),
/// the next attempt sends `Range: bytes=<n>-` for the bytes already in hand and
/// appends a `206 Partial Content` response's body to them, instead of
/// re-downloading the whole object from byte 0. This is real, load-bearing
/// plumbing (proven in the tests below against a mock server that truncates the
/// first response and serves a 206 on retry) — but note the bookkeeping it
/// drives is the MINIMAL correct version, not a fully streaming rewrite: this
/// function still reads the body with one shot via `resp.bytes()`, so a body
/// read failure is only observable as "the whole read failed" (zero bytes
/// captured from THAT attempt) rather than "N of M bytes arrived before the
/// connection dropped". True byte-level resume (capturing exactly how far a
/// dropped connection got) requires streaming the body incrementally (e.g.
/// `bytes_stream()`, tracking bytes consumed as each chunk arrives) rather than
/// one whole-body `.bytes()` call — left as a follow-up if a real dropped-mid-
/// transfer case in the field ever needs plumbing beyond the Range mechanism
/// itself, which this pass proves works.
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
                // Only a body-read failure carries resumable partial bytes; a
                // connect/status failure means nothing was read yet, so `partial`
                // stays whatever it already was (empty on a first-attempt connect
                // failure — same behavior as before this change, a fresh GET).
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

/// A 5xx/429 status from a presigned URL — a distinct error type from a generic
/// `error_for_status` so the retry loop above can recognize "this is worth
/// retrying" without re-parsing string text.
#[derive(Debug)]
struct TransientStatusError(reqwest::StatusCode);
impl std::fmt::Display for TransientStatusError {
    fn fmt(&self, f: &mut std::fmt::Formatter<'_>) -> std::fmt::Result {
        write!(f, "transient status {}", self.0)
    }
}
impl std::error::Error for TransientStatusError {}

/// A body-read failure that occurred AFTER a successful (200/206) response
/// status — distinct from a connect/status failure so `s3_get`'s retry loop
/// knows there may be resumable bytes. `bytes_read` carries forward whatever
/// had already been accumulated into this attempt (the Range-resumed prefix
/// from an earlier attempt, if any); this implementation's own single-shot
/// `resp.bytes()` read never itself contributes partial bytes on failure (see
/// the doc comment on `s3_get`), so in practice this is only non-empty when a
/// prior successful Range/206 read is retried again after ITS retry-worthy
/// follow-on failure — the mechanism is real and tested, the bookkeeping is
/// intentionally minimal.
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
    let resp = req.send().await.context("GET presigned input")?;
    let status = resp.status();
    if !already_read.is_empty() && status == reqwest::StatusCode::PARTIAL_CONTENT {
        // Resumed request, server honored the Range: append rather than replace.
    } else if !status.is_success() {
        if is_retryable_status(status) {
            return Err(TransientStatusError(status).into());
        }
        return Err(resp
            .error_for_status()
            .context("input_url returned error status")
            .unwrap_err());
    } else if !already_read.is_empty() {
        // We asked for a Range resume but got a full 200 back (server doesn't
        // support Range on this object) — restart from scratch rather than
        // silently double-counting bytes.
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
    match resp.bytes().await {
        Ok(bytes) => out.extend_from_slice(&bytes),
        Err(e) => {
            return Err(PartialBodyError {
                bytes_read: out,
                source: e,
            }
            .into())
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

/// The plain (non-Range) whole-body read path, factored out so the "server
/// ignored our Range request and sent 200" fallback in `s3_get_once` can reuse
/// it instead of duplicating the cap checks.
async fn s3_get_full(resp: reqwest::Response) -> Result<Vec<u8>> {
    if let Some(len) = resp.content_length() {
        if len > MAX_INPUT_DOWNLOAD_BYTES {
            anyhow::bail!(
                "input_url reports {len} bytes, exceeding the {MAX_INPUT_DOWNLOAD_BYTES}-byte task download cap"
            );
        }
    }
    let bytes = resp.bytes().await.context("reading input body")?;
    if bytes.len() as u64 > MAX_INPUT_DOWNLOAD_BYTES {
        anyhow::bail!(
            "input body was {} bytes, exceeding the {MAX_INPUT_DOWNLOAD_BYTES}-byte task download cap",
            bytes.len()
        );
    }
    Ok(bytes.to_vec())
}

/// PUT bytes to a presigned URL with the given `Content-Type`. `application/json`
/// for normal results; `application/octet-stream` for the opt-in binary embedding
/// artifact (PLANE_D D5/D15). The bytes are uploaded verbatim either way.
/// Transient failures (connect/timeout/5xx) are retried with exponential backoff
/// (Data Transfer & Artifact I/O 4.5->5) — a presigned PUT is idempotent (it
/// simply overwrites the same object key), so replaying it on a transient failure
/// is always safe, never a risk of double-applying an effect.
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

/// Deliberately uploads `body` VERBATIM, uncompressed (Data Transfer & Artifact
/// I/O 6->7 covers the result-PUT path only in name, not in code): reqwest's
/// gzip feature auto-decodes an incoming response's gzip body but does NOT
/// auto-compress an outgoing request body, so gzip-compressing this PUT would
/// require this function to gzip-encode `body` itself and set
/// `Content-Encoding: gzip` before sending. That was scoped OUT after checking
/// real consumers of these exact result objects: control/api.go's
/// `mergeJobResults` (control/api.go:1048, `storage.GetObject(ctx, pr.ResultRef)`)
/// and control/verification.go's redundancy vote path
/// (control/verification.go:465, `v.storage.GetObject(ctx, cr.ResultRef)` feeding
/// `resultsAgree`'s byte/JSON comparison) both read the object's raw bytes
/// straight off `Storage.GetObject` (control/storage.go:248, a plain
/// `io.ReadAll` over the MinIO object with no Content-Encoding awareness at
/// all) and parse/compare them directly as JSON or the binary embed format. If
/// this PUT compressed its body, every one of those reads would silently
/// operate on gzip bytes instead of the real result — a merge/verification
/// corruption, not a decode error, since nothing there ever checks
/// Content-Encoding before use. Compressing this body is unsafe until those
/// Go-side readers are made Content-Encoding-aware; only the GET-side (input
/// download) compression above was implemented, since the agent is the sole
/// reader of its own downloaded input and no other consumer is affected.
async fn s3_put_bytes_once(
    client: &reqwest::Client,
    url: &str,
    body: &[u8],
    content_type: &str,
) -> Result<()> {
    let resp = client
        .put(url)
        .header(reqwest::header::CONTENT_TYPE, content_type)
        .body(body.to_vec())
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

/// Results larger than this are staged through an encrypted temp file rather than
/// held only in memory (action plan §K / STRONG I).
const STAGING_THRESHOLD: usize = 16 * 1024 * 1024;

/// Upload a large result without ever leaving it unencrypted on disk.
///
/// We encrypt the plaintext with a one-shot AES-256-GCM key (random key+nonce,
/// generated here and dropped at the end), write only the CIPHERTEXT to a temp
/// file, then read it back, decrypt in memory, and PUT the plaintext over TLS to
/// the presigned URL. The transient plaintext copy and the temp file are both
/// wiped/removed before returning. The presigned endpoint still receives the
/// plaintext payload with `content_type` (the control plane verifies plaintext);
/// the encryption guards the *disk* spill only, which is the whole point of staging.
async fn put_via_encrypted_staging(
    client: &reqwest::Client,
    url: &str,
    plaintext: &[u8],
    content_type: &str,
) -> Result<()> {
    use aes_gcm::aead::{Aead, KeyInit};
    use aes_gcm::{Aes256Gcm, Key, Nonce};
    use rand::RngCore;

    // Ephemeral key + nonce: live only for this upload, never persisted.
    let mut key_bytes = [0u8; 32];
    let mut nonce_bytes = [0u8; 12];
    rand::thread_rng().fill_bytes(&mut key_bytes);
    rand::thread_rng().fill_bytes(&mut nonce_bytes);
    let cipher = Aes256Gcm::new(Key::<Aes256Gcm>::from_slice(&key_bytes));
    let nonce = Nonce::from_slice(&nonce_bytes);

    let ciphertext = cipher
        .encrypt(nonce, plaintext)
        .map_err(|e| anyhow::anyhow!("staging encryption failed: {e}"))?;

    // Write CIPHERTEXT to a uniquely-named temp file.
    let path = std::env::temp_dir().join(format!("cx-stage-{}.bin", uuid::Uuid::new_v4()));
    tokio::fs::write(&path, &ciphertext)
        .await
        .with_context(|| format!("writing encrypted staging file {}", path.display()))?;

    // Read it back, decrypt in memory, upload plaintext, then wipe + remove —
    // even if the PUT fails, so no decrypted bytes linger on disk.
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

    // Best-effort secure cleanup: overwrite the ciphertext file with zeros, then
    // remove it. (Overwrite-in-place is best-effort on modern FS, but we never
    // leave the staged bytes readable as a normal file.)
    let _ = tokio::fs::write(&path, vec![0u8; ciphertext.len()]).await;
    let _ = tokio::fs::remove_file(&path).await;
    result
}

/// Shared, cheaply-cloned context every in-flight task needs. Bundling it keeps
/// the spawn sites tidy and the warm `pool` shared across all tasks.
#[derive(Clone)]
struct WorkCtx {
    client: Arc<ControlPlaneClient>,
    cap: Arc<WorkerCapability>,
    runners: Arc<Vec<Box<dyn JobRunner>>>,
    pool: ModelPool,
    s3: reqwest::Client,
    min_payout_usd_per_hr: f32,
    /// Operator's reserved memory headroom (GB), for the memory snapshot attached
    /// to a typed failure report (Plane C/D D0).
    memory_headroom_gb: f32,
    /// Ceiling on physical-memory utilization (%) — the SAME knob
    /// `evaluate_memory_throttle` gates pre-claim work on. Carried into `WorkCtx`
    /// (docs/internal/CREED_AND_PATH_TO_TEN.md, "Memory management & dynamic
    /// throttling internals" 7→8) so the mid-job preemption probe re-checks
    /// pressure against the SAME governor thresholds as the pre-claim gate,
    /// rather than inventing a second, divergent notion of "too much pressure".
    max_memory_pct: f32,
    /// Seconds between intra-task partial-result checkpoint flushes (0 = off);
    /// combined per task with the dispatch's `partial_put_url`.
    checkpoint_secs: u64,
    status: Arc<StatusWriter>,
}

/// The agent loop: register once, then run a BOUNDED-CONCURRENCY pipeline —
/// prefetch up to `max_concurrent_tasks` tasks, execute them concurrently
/// through the warm model pool, and commit each as it finishes. A 30s heartbeat
/// and SIGINT stay responsive via `tokio::select!`. Heavy GPU compute serializes
/// behind each model's mutex (in the pool); the wins are no per-task model reload
/// and overlapping S3 GET/PUT with compute.
async fn run_agent(mut cfg: AgentConfig) -> Result<()> {
    // Constructed BEFORE benchmarking (not after, as `WorkCtx` used to do) and
    // threaded through detect_and_benchmark so the benchmark's model loads land
    // in the SAME pool real task dispatch reuses afterward — Warm Model Pool
    // 6->6.5 (docs/internal/CREED_AND_PATH_TO_TEN.md): the benchmark used to load
    // into a local variable that was simply dropped, so the agent's first real
    // task paid the exact cold load the benchmark had just measured and thrown
    // away. `ModelPool` is cheap to clone (Arc-backed); reused, not recreated,
    // below.
    let pool = ModelPool::new();
    let mut cap = hardware::detect_and_benchmark(
        cfg.supplier_id,
        AGENT_VERSION,
        cfg.min_payout_usd_per_hr,
        // The configured on-device engine (candle default). It becomes the second
        // axis of the worker's verification class on the control plane.
        cfg.inference_backend.engine_tag(),
        &pool,
    )
    .await;
    let advertised_worker_id = cap.worker_id;
    let permits = cfg.concurrency(cap.memory_gb);

    let client = ControlPlaneClient::new(cfg.control_url.clone(), cfg.worker_token.clone())
        .context("building control-plane client (is worker_token set?)")?;
    // Separate client for presigned S3 object I/O (no auth header; longer body
    // timeout for large chunks). Two DISTINCT timeouts (Data Transfer & Artifact
    // I/O 5->6): `connect_timeout` bounds only the TCP+TLS handshake, `timeout`
    // remains the total-request ceiling (unchanged at 120s — large-object
    // transfers still get the same overall budget). A presigned URL's connect
    // should be near-instant on any real network; if a full 10s connect doesn't
    // succeed, that's a real network/DNS problem, not a slow transfer, and
    // should fail fast so the existing retry loop (is_retryable_reqwest_err
    // already treats e.is_connect() as retryable) can retry sooner instead of
    // burning the whole 120s budget on one dead attempt.
    let s3 = reqwest::Client::builder()
        .connect_timeout(Duration::from_secs(10))
        .timeout(Duration::from_secs(120))
        .build()
        .context("building S3 client")?;

    tracing::info!(worker_id = %advertised_worker_id, control = %cfg.control_url, max_concurrent_tasks = permits, "registering with control plane");
    let confirmed = client.register(&cap).await.context("registration failed")?;
    // The control plane binds worker/supplier identity from the token and echoes the
    // authoritative ids. Use those ids for every receipt/status/heartbeat after
    // registration; the locally generated id is only a pre-registration placeholder.
    cap.worker_id = confirmed.worker_id;
    cap.supplier_id = confirmed.supplier_id;
    let worker_id = cap.worker_id;
    tracing::info!(worker_id = %worker_id, supplier_id = %cap.supplier_id, "registered");

    // Menu-bar status surface: write the status file now (idle), then on every
    // heartbeat and task transition. The macOS app reads it (see macapp/).
    let status = Arc::new(StatusWriter::new(
        AGENT_VERSION,
        worker_id,
        &cap.benchmarks,
        cap.hw_class,
    ));
    // Echo the APPLIED operator prefs (the effective config after the agent.prefs.toml
    // overlay) so the app shows agent truth, not just its local toggle state (item 26).
    status.set_applied_prefs(status::AppliedPrefs::from_config(&cfg, cap.memory_gb));
    status.registered();

    let ctx = WorkCtx {
        client: Arc::new(client),
        cap: Arc::new(cap),
        runners: Arc::new({
            let mut rs = default_runners();
            // Serving-lane seams: only when the operator opts in. Each is inserted right
            // AFTER ClusterRunner (index 0) so a giant cluster model still routes to the
            // Plane B seam, but BEFORE the Candle generative runners so lane jobs route
            // here. (Each seam's can_run also yields cluster models, defense-in-depth.)
            // The backends are mutually exclusive (one inference_backend), and the default
            // Candle backend leaves dispatch byte-for-byte unchanged.
            match cfg.inference_backend {
                config::InferenceBackend::Candle => {}
                config::InferenceBackend::Mlx => {
                    rs.insert(1, Box::new(runners::MlxRunner));
                    tracing::info!("inference_backend=mlx: MLX serving-lane seam active (generative LLM jobs route to the MLX boundary until the runtime is wired)");
                }
                config::InferenceBackend::Vllm => {
                    rs.insert(1, Box::new(runners::VllmRunner));
                    tracing::info!("inference_backend=vllm: vLLM CUDA serving-lane seam active (generative LLM jobs route to the vLLM boundary until a pinned server is configured + the determinism soak passes — docs/VLLM_LANE.md)");
                }
                config::InferenceBackend::Hawking => {
                    // Week 6 (docs/HAWKING_PORT_PLAN.md): the Hawking continuous-batch lane
                    // is WIRED for live dispatch. HawkingRunner::run drives the real,
                    // Metal-hardware-proven churn driver (LlamaBackend::hawking_generate_churn
                    // — scheduler admission + slot churn + KV region reuse over
                    // hawking_decode_step) for `batch_infer` on the small GGUF family, and
                    // its can_run declines everything else so unwired job types (batch_
                    // classification / json_extraction) and the big 7B GGUF fall through to
                    // the proven Candle runners unchanged (see HawkingRunner's doc comment
                    // for the honest wired-vs-not list). Inserted the same way as the
                    // MLX/vLLM seams: right AFTER ClusterRunner, BEFORE the Candle
                    // generative runners. The pool size is the operator knob, HARD-CLAMPED
                    // to 1..=8 (B=16 is explicitly unvalidated).
                    #[cfg(feature = "metal")]
                    {
                        rs.insert(
                            1,
                            Box::new(runners::HawkingRunner::new(cfg.hawking_pool_size_clamped())),
                        );
                    }
                    tracing::info!(
                        pool_size = cfg.hawking_pool_size_clamped(),
                        "inference_backend=hawking: continuous-batch lane WIRED for dispatch (engine=hawking) — batch_infer on the small GGUF family runs through the proven churn scheduler; batch_classification/json_extraction and the 7B GGUF stay on Candle (not yet dispatch-gated on this lane); on a non-Metal build no runner is inserted and this is a log line only — docs/HAWKING_PORT_PLAN.md Week 6"
                    );
                }
            }
            rs
        }),
        pool,
        s3,
        min_payout_usd_per_hr: cfg.min_payout_usd_per_hr,
        memory_headroom_gb: cfg.memory_headroom_gb,
        max_memory_pct: cfg.max_memory_pct,
        checkpoint_secs: cfg.checkpoint_secs,
        status: status.clone(),
    };
    let sem = Arc::new(Semaphore::new(permits));
    // In-flight tasks; drained as they finish so commits/errors surface promptly.
    let mut inflight = tokio::task::JoinSet::new();

    // PATCH (P-heartbeat-decouple, docs/CREED_AND_PATH_TO_TEN.md "Agent
    // concurrency & parallelism model" 6.5->7): the heartbeat used to be a
    // `select!` arm sharing this loop with the permit-acquire+long-poll arm.
    // `tokio::select!` runs whichever arm fires to COMPLETION before it can pick
    // another — and poll_and_spawn's own long-poll blocks for up to
    // wait_ms=25000 (protocol.rs) even in the ENTIRELY NORMAL case (never mind
    // the 60s ineligible / 30s throttled idle sleeps also inside that same arm),
    // so the 30s heartbeat could slip to 55-90s worst case, staling exactly the
    // warm-routing/throttle data the scheduler's safe-dispatch filter reads.
    // Spawning heartbeat as its own task makes its 30s cadence structurally
    // independent of whatever the poll loop is doing — `ctx`/`cfg`/`status` are
    // already designed to be cheaply cloned for exactly this kind of use
    // (WorkCtx's own doc comment: "shared, cheaply-cloned context").
    {
        let ctx = ctx.clone();
        let mut cfg = cfg.clone();
        let status = status.clone();
        tokio::spawn(async move {
            let mut sys = System::new();
            let mut heartbeat = tokio::time::interval(Duration::from_secs(30));
            // Don't fire a burst if a tick is missed while the runtime was busy.
            heartbeat.set_missed_tick_behavior(tokio::time::MissedTickBehavior::Delay);
            loop {
                heartbeat.tick().await;
                let ts = now_unix();
                let cpu = cpu_pct(&mut sys);
                // Real memory reading → throttle decision. Sent to the control
                // plane (effective memory + throttled gate the safe-dispatch
                // filter) and surfaced to the menu bar. On the CUDA lane this is
                // VRAM (the real limit on a GPU box); host/unified RAM otherwise.
                let throttle = cfg.evaluate_memory_throttle(&throttle_snapshot(), None);
                // Real thermal reading (Supplier onboarding & safety 6→7): on the Mac
                // agent this is the menu-bar app's `ProcessInfo.thermalState`, re-read
                // fresh from the prefs overlay every heartbeat — never cached from
                // process start, since the whole point is a hot Mac mid-run.
                cfg.refresh_thermal_pressure();
                // Idle-LRU eviction (docs/CREED_AND_PATH_TO_TEN.md, "Warm model
                // pool" 7→8): piggybacks on this same 30s cadence rather than a
                // separate timer. A 7B model touched once and then left idle for
                // MODEL_IDLE_EVICT_AFTER is dropped, reclaiming its ~4.7GB instead
                // of pinning it for the rest of the process.
                let evicted = ctx.pool.evict_idle(MODEL_IDLE_EVICT_AFTER).await;
                if !evicted.is_empty() {
                    // PATCH (P-measured-residency, docs/CREED_AND_PATH_TO_TEN.md "Memory
                    // management & dynamic throttling internals" 8→9 / "Warm model pool &
                    // load mechanics" 7→8): report the REAL measured RSS bytes each evicted
                    // model actually occupied (from `pool::residency_snapshot()`, populated
                    // by a genuine before/after process-RSS reading around that model's
                    // load), not just its name — so this log line is the measured residency
                    // table surfacing in practice, not a table nobody reads.
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
                // Warm-routing (D3): report the models actually warm in the pool so the
                // control plane can prefer this worker for those models. Real ids only —
                // `loaded_model_ids` gates on a resolved OnceCell, never a load in flight.
                let loaded_models = ctx.pool.loaded_model_ids().await;
                // Real GPU telemetry on the CUDA lane (nvidia-smi); honest 0.0/None off it.
                let (gpu, gpu_temp) = gpu_telemetry();
                // Live throttle detection (docs/internal/CREED_AND_PATH_TO_TEN.md,
                // "Thermal sustained-vs-peak throughput on fanless Apple Silicon"
                // 7→8): a generative runner's LiveThroughputMonitor sets this
                // process-wide flag the instant it observes a REAL sustained tok/s
                // drop mid-task — folded into the SAME `throttled` wire field the
                // memory governor already uses, so the control plane's existing
                // ClaimTask/CandidateWorkers exclusions (never dispatch to, never
                // pick as a redundancy/hedge peer, a worker pausing for pressure)
                // apply to a live thermal throttle too, with no new scheduler-side
                // plumbing. OR'd with the memory-pressure throttle, never replacing
                // it — either real signal alone is sufficient reason to pause.
                let live_throttling = runners::live_throttle_detected();
                let hb = Heartbeat {
                    worker_id,
                    timestamp: ts,
                    cpu_pct: cpu,
                    gpu_pct: gpu,
                    gpu_temp_c: gpu_temp,
                    current_task: None,
                    available_memory_gb: throttle.available_gb,
                    effective_memory_gb: throttle.effective_gb,
                    reserved_headroom_gb: throttle.reserved_headroom_gb,
                    throttled: throttle.throttled || live_throttling,
                    loaded_models,
                };
                if let Err(e) = ctx.client.heartbeat(&hb).await {
                    tracing::warn!(error = %e, "heartbeat failed");
                }
                // Refresh the menu-bar status: telemetry + eligibility + earnings +
                // trust surface (Supplier onboarding & safety 7->8: "Populate the
                // trust panel with real data") — all best-effort; a failed call keeps
                // the last-known values rather than blanking the panel.
                let eligible = cfg.is_eligible_to_run(current_hour_local(), on_battery());
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

            // Reap a finished task. `join_next` is None only when nothing is
            // in-flight, in which case it returns immediately and we fall through
            // to polling — so this arm never starves the others.
            Some(joined) = inflight.join_next() => {
                if let Err(e) = joined {
                    tracing::error!(error = %e, "in-flight task panicked");
                }
            }

            // Acquire a permit (waits when all `permits` slots are busy → this IS
            // the concurrency bound), then long-poll. A returned task is spawned
            // with the permit moved into it; no work releases the permit at once.
            // Reaping finished tasks (the arm above) keeps working while we hold a
            // permit here, so an idle poll never blocks commits.
            permit = sem.clone().acquire_owned() => {
                let permit = permit.expect("semaphore is never closed");
                if let Err(e) = poll_and_spawn(&mut cfg, &ctx, permit, &mut inflight).await {
                    tracing::warn!(error = %e, "poll cycle error");
                    // Back off briefly so a hard failure doesn't hot-loop.
                    tokio::time::sleep(Duration::from_secs(5)).await;
                }
            }
        }
    }
}

/// Gate on eligibility, long-poll once, and on a task: start it and spawn the
/// execute→commit pipeline (the `permit` rides along, freeing the slot when the
/// task ends). Returns promptly so heartbeat/SIGINT stay responsive.
async fn poll_and_spawn(
    cfg: &mut AgentConfig,
    ctx: &WorkCtx,
    permit: tokio::sync::OwnedSemaphorePermit,
    inflight: &mut tokio::task::JoinSet<()>,
) -> Result<()> {
    if !cfg.is_eligible_to_run(current_hour_local(), on_battery()) {
        tracing::debug!("not eligible to run (quiet hours / battery); idling 60s");
        drop(permit); // release the slot while we idle
        tokio::time::sleep(Duration::from_secs(60)).await;
        return Ok(());
    }

    // Real thermal gate (Supplier onboarding & safety 6→7): re-read the live
    // thermal-pressure overlay fresh (never the value from process start — a
    // Mac can heat up mid-run) and pause new claims on `serious`/`critical`,
    // closing the gap between the consent copy's "pauses ... under ... high
    // thermals" and a `gpu_temp` that used to be permanently `None` off CUDA.
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

    // Dynamic provider throttling: before claiming work, re-read REAL memory and
    // pause new claims if taking a job would breach the supplier's reserved
    // headroom or drive the box past its memory ceiling. Enforced here (before the
    // claim) and re-evaluated every cycle — so finishing a task and looping back
    // re-checks before the next claim, and a pressured Mac is never handed more
    // work. The surfaced reason tells the operator exactly why work paused. On the
    // CUDA lane the gating resource is VRAM (a GPU box has ample host RAM, so the
    // host-RAM gate would never trip even with the card nearly full); host/unified
    // RAM on Apple/CPU. `throttle_snapshot()` picks the right one per active device.
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
    tracing::info!(task = %task.task_id, job = %task.job_id, "received task");

    // Per-task fit gate (docs/internal/CREED_AND_PATH_TO_TEN.md "Memory management &
    // dynamic throttling internals" 5.5->6): the pre-claim throttle check above
    // (`evaluate_memory_throttle(..., None)`) can only test GENERAL capacity — it runs
    // before poll_task returns, so it cannot yet know THIS task's own memory need.
    // `evaluate_memory_throttle`'s `next_task_gb` parameter has existed since the
    // governor was built but every call site passed `None`, so the check it implements
    // never actually ran. Now that we have the real claimed task in hand, re-check with
    // its real declared `min_memory_gb` against currently effective memory — a task
    // that would not fit is declined HONESTLY before `start_task`, so the control plane
    // reassigns it to a worker with room, instead of this worker blindly starting it and
    // risking an OOM the pre-claim check had no way to see coming.
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
        return Ok(()); // never started → control plane reassigns it
    }

    // Min-payout gate (STRONG H): the server already filters by reservation
    // price, but if it ever offers a rate below ours, refuse honestly rather than
    // running for less than the operator's floor. 0.0 = "no rate advertised".
    let floor = ctx.min_payout_usd_per_hr;
    if floor > 0.0 && task.offered_rate_usd_hr > 0.0 && task.offered_rate_usd_hr < floor {
        tracing::warn!(
            task = %task.task_id,
            offered = task.offered_rate_usd_hr,
            floor,
            "offered rate below reservation price; skipping task (not started)"
        );
        return Ok(()); // never started → control plane reassigns it
    }

    ctx.client
        .start_task(task.task_id)
        .await
        .context("start_task")?;
    // Surface the running job to the menu bar (the operator sees the job_id; the
    // task_id keys the in-flight set so concurrent tasks don't collide).
    ctx.status.job_started(
        task.task_id,
        task.job_id,
        task.manifest.job_type.tag(),
        now_unix(),
    );

    // Spawn the heavy work so the loop returns to its `select!` immediately:
    // the heartbeat keeps firing and new tasks prefetch while this one runs.
    let ctx = ctx.clone();
    inflight.spawn(async move {
        // `permit` is held for the lifetime of this task; dropping it here frees
        // the slot for the next poll.
        let _permit = permit;
        let task_id = task.task_id;
        let started = std::time::Instant::now();
        match execute_task(
            &task,
            &ctx.cap,
            &ctx.runners,
            &ctx.pool,
            &ctx.s3,
            ctx.checkpoint_secs,
            ctx.memory_headroom_gb,
            ctx.max_memory_pct,
        )
        .await
        {
            Ok(commit) => {
                match ctx.client.commit_task(task_id, &commit).await {
                    Ok(()) => tracing::info!(task = %task_id, "committed result"),
                    Err(e) => tracing::error!(task = %task_id, error = %e, "commit_task failed"),
                }
                ctx.status.job_finished(task_id, None);
            }
            Err(e) => {
                // Honest failure: no result produced (model download / inference
                // error, or no runner matched). Do NOT commit a fake result. Plane
                // C/D D0: report a TYPED failure immediately so the control plane
                // requeues (retryable) or fails+refunds (terminal) in seconds —
                // instead of stranding the task for the 30-min stale reaper.
                tracing::error!(task = %task_id, error = %e, "task execution failed; reporting typed failure");
                let snap = hardware::read_memory_snapshot();
                let report = failure::build_report(
                    &e,
                    task.manifest.job_type.tag(),
                    &task.manifest.model.model_ref,
                    started.elapsed().as_millis() as u64,
                    &snap,
                    ctx.memory_headroom_gb,
                );
                if let Err(fe) = ctx.client.fail_task(task_id, &report).await {
                    tracing::warn!(task = %task_id, error = %fe, "fail_task report failed (stale reaper remains the fallback)");
                }
                ctx.status.job_finished(task_id, Some(e.to_string()));
            }
        }
    });
    Ok(())
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn dispatch_runtime_authority_is_exact_and_fail_closed() {
        let sha = runtime_matrix_generated::RUNTIME_MATRIX_SHA256;
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
                "candle-cuda-minilm-embed",
                "candle_cuda",
                sha,
                "candle",
                types::HardwareClass::Nvidia80g,
                "cuda",
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

    // ── self-re-exec sandbox profile discovery (macOS) ──────────────────────
    // The self-re-exec (reexec_under_sandbox_if_needed) re-launches a DIRECT binary
    // start under `sandbox-exec` so a `cargo run`/hand-rolled launch is contained too,
    // not only the .app path (Security Posture 8→9, docs/SECURITY.md). These pin the
    // pure profile-DISCOVERY logic; the real end-to-end containment (a direct launch
    // ends up sandboxed) is proven separately against real sandbox-exec by
    // macapp/ComputeExchangeAgent/sandbox-profile-test.sh + the reexec harness.
    #[cfg(target_os = "macos")]
    #[test]
    fn pick_sandbox_profile_prefers_override_then_sibling_then_none() {
        use std::path::Path;
        let over = PathBuf::from("/opt/cx/override.sb");
        let sib = PathBuf::from("/Applications/CX.app/Contents/Resources/cx-agent.sb");

        // Override present → it wins even when the sibling also exists.
        let all_exist = |_: &Path| true;
        assert_eq!(
            pick_sandbox_profile(Some(over.clone()), Some(sib.clone()), all_exist),
            Some(over.clone()),
            "an existing explicit override must take priority over the exe sibling"
        );

        // Override MISSING on disk → fall through to the sibling.
        let only_sibling = |p: &Path| p == sib.as_path();
        assert_eq!(
            pick_sandbox_profile(Some(over.clone()), Some(sib.clone()), only_sibling),
            Some(sib.clone()),
            "a non-existent override must be skipped and the sibling used"
        );

        // Neither exists → None (a bare dev build runs honestly unsandboxed).
        let none_exist = |_: &Path| false;
        assert_eq!(
            pick_sandbox_profile(Some(over.clone()), Some(sib.clone()), none_exist),
            None,
            "when no profile exists the discovery must return None, not a phantom path"
        );

        // No override supplied at all → sibling is the only candidate.
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
    fn sandbox_data_dir_is_compute_exchange_under_home() {
        // Must match config.rs's default data_dir and the Swift launcher's
        // AgentPaths.dataDir so the profile's DATADIR param scopes the same directory
        // the agent actually writes status.json/prefs into.
        assert_eq!(
            sandbox_data_dir("/Users/alice"),
            "/Users/alice/.compute-exchange"
        );
    }

    #[cfg(target_os = "macos")]
    #[test]
    fn sandbox_requirement_parser_is_explicit() {
        for enabled in ["1", "true", "TRUE", " yes "] {
            assert!(sandbox_required_value(Some(enabled)), "{enabled:?}");
        }
        for disabled in [None, Some(""), Some("0"), Some("false"), Some("maybe")] {
            assert!(!sandbox_required_value(disabled));
        }
    }

    #[test]
    fn bench_mode_parse_accepts_known_and_rejects_unknown() {
        assert_eq!(BenchMode::parse("identical").unwrap(), BenchMode::Identical);
        assert_eq!(BenchMode::parse("mixed").unwrap(), BenchMode::Mixed);
        // Case/whitespace-insensitive so a CLI typo in casing still works.
        assert_eq!(BenchMode::parse("  MiXeD ").unwrap(), BenchMode::Mixed);
        assert!(
            BenchMode::parse("random").is_err(),
            "unknown mode must be a hard error, not silently defaulted"
        );
    }

    #[test]
    fn identical_mode_produces_one_distinct_prompt() {
        // The best-case regime: every row is the same prompt, so the whole batch
        // lands in ONE exact-token-length bucket inside generate_batch.
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
        // The honest real-traffic regime: rows have DIFFERENT lengths, so
        // generate_batch's exact-length bucketing splits the batch into several
        // narrower buckets — the mechanism that pulls the mixed curve below the
        // identical ceiling. Prove the generator actually produces that spread.
        let b = 8;
        let prompts = build_bench_prompts("Write about the ocean:", b, BenchMode::Mixed);
        assert_eq!(prompts.len(), b);
        // Distinct CHARACTER lengths is a model-free proxy for distinct token
        // lengths (more chars → more tokens for this filler): a real spread here
        // guarantees the batch cannot collapse into a single bucket.
        let distinct_char_lens: std::collections::HashSet<usize> =
            prompts.iter().map(|p| p.len()).collect();
        assert!(
            distinct_char_lens.len() >= 3,
            "mixed mode must span multiple distinct lengths (got {} for b={b}: {:?})",
            distinct_char_lens.len(),
            prompts.iter().map(|p| p.len()).collect::<Vec<_>>()
        );
        // Every mixed prompt must still START with the stem — the length classes
        // EXTEND a shared stem, they don't replace it, so serial and batched decode
        // the same underlying request, just longer or shorter.
        assert!(
            prompts
                .iter()
                .all(|p| p.starts_with("Write about the ocean:")),
            "every mixed prompt must extend the shared stem"
        );
    }

    #[test]
    fn mixed_mode_is_deterministic_across_calls() {
        // A published mixed curve must be reproducible: the same (stem, b) always
        // yields the same prompts (no RNG, no clock, no env).
        let a = build_bench_prompts("stem", 16, BenchMode::Mixed);
        let b = build_bench_prompts("stem", 16, BenchMode::Mixed);
        assert_eq!(a, b, "mixed prompt generation must be deterministic");
    }

    #[test]
    fn mixed_mode_prompt_for_a_row_is_stable_as_batch_grows() {
        // Row i's prompt depends only on i, so a wider batch is a strict superset of
        // a narrower one's prompts. This is what lets the serial baseline run the
        // widest batch's distinct prompts and cover every narrower sweep point.
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
        // Mean=100, values deviate by ±10 -> population stddev = 10 -> CV = 10%.
        let cv = coefficient_of_variation_pct(&[90.0, 110.0]);
        assert!((cv - 10.0).abs() < 1e-9, "expected CV=10%, got {cv}");
    }

    /// Control Plane Hot Path 8->9 (docs/internal/CREED_AND_PATH_TO_TEN.md, "Get
    /// result-commit off the S3 critical path"): sha256_hex must produce the
    /// well-known SHA-256("abc") test vector, in lowercase hex, exactly matching
    /// the shape the control plane's nullSHA256Hex validator requires (64
    /// lowercase hex chars) — a formatting bug here (wrong case, truncated
    /// digest, byte-order slip) would silently disable the hash-trust fast path
    /// on every commit (it always falls back to a real GetObject on a shape
    /// mismatch, so the failure mode is "never faster", not "wrong verdict" —
    /// but this test still pins the exact well-known vector so the win is real).
    #[test]
    fn sha256_hex_matches_known_test_vector() {
        let got = sha256_hex(b"abc");
        assert_eq!(
            got, "ba7816bf8f01cfea414140de5dae2223b00361a396177a9cb410ff61f20015ad",
            "sha256_hex(\"abc\") must match the well-known NIST test vector"
        );
        assert_eq!(got.len(), 64, "must be 64 lowercase hex chars — the exact shape control/store.go's nullSHA256Hex requires");
    }

    /// The empty-input digest is another well-known fixed vector, and pins that
    /// an empty (but non-None) result still produces a real, valid 64-char hex
    /// digest rather than an empty string that could be mistaken for "no hash".
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
        // Mirrors the real published sweep's own unexplained anomaly (a 693 vs
        // 1087.9 tok/s dip at one batch size) — this magnitude of spread must
        // clear the 10% warning threshold the bench-batch CLI now checks.
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
        // Upper-middle for an even count (index len/2 = 2 of [10,20,30,40] -> 30),
        // matching this function's actual, documented "not textbook-exact" convention.
        assert_eq!(median(&mut even), 30.0);
        let mut single = vec![7.5];
        assert_eq!(median(&mut single), 7.5);
    }

    #[test]
    fn median_is_robust_to_a_single_outlier_unlike_a_mean() {
        // The whole point of reporting median (not mean) alongside CV: one wild
        // rep should not move the headline number nearly as much as it would a
        // plain average.
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
        // No thermal decay at all: every window identical -> peak == sustained,
        // gap 0%.
        let (peak, sustained, gap) = sustained_summary(&[100.0, 100.0, 100.0, 100.0]);
        assert_eq!(peak, 100.0);
        assert_eq!(sustained, 100.0);
        assert_eq!(gap, 0.0);
    }

    #[test]
    fn sustained_summary_detects_real_throttling_decay() {
        // A realistic fanless-Mac decay shape: peak early, decaying to a lower
        // steady state and staying there — exactly what THERMAL_SECS=20's
        // early/late-window comparison in runners.rs looks for, but over a much
        // longer curve. 12 windows, last 25% (ceil(12*0.25) = 3 windows) = the
        // throttled tail.
        let windows = vec![
            140.0, 138.0, 130.0, 120.0, 110.0, 105.0, 100.0, 98.0, 97.0, 96.0, 95.0, 95.0,
        ];
        let (peak, sustained, gap) = sustained_summary(&windows);
        assert_eq!(peak, 140.0);
        assert!(
            (sustained - (96.0 + 95.0 + 95.0) / 3.0).abs() < 1e-9,
            "sustained should be the mean of the last 25% (3 of 12 windows), got {sustained}"
        );
        // 140 -> ~95.33 is a real, substantial gap; must land well above 20%.
        assert!(
            gap > 25.0,
            "expected a real double-digit throttling gap, got {gap}%"
        );
    }

    #[test]
    fn sustained_summary_never_reports_negative_gap_for_a_rising_curve() {
        // A warm-up ramp (throughput RISING, not falling) must never be reported
        // as negative "throttling" — the sustained (tail) mean is the new peak in
        // this case, so gap should be ~0, not negative.
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

    /// A minimal, real (no mocking framework) HTTP/1.1 server on 127.0.0.1:0 that
    /// hands out `responses` in order, one per accepted connection — the Nth
    /// connection gets responses[N]. Panics if more connections arrive than
    /// responses were provided (a test bug, not a retry-loop bug). Returns the
    /// `http://127.0.0.1:PORT` base URL.
    async fn spawn_sequenced_mock_server(responses: Vec<(u16, &'static str)>) -> String {
        let listener = tokio::net::TcpListener::bind("127.0.0.1:0").await.unwrap();
        let addr = listener.local_addr().unwrap();
        tokio::spawn(async move {
            use tokio::io::{AsyncReadExt, AsyncWriteExt};
            for (status, body) in responses {
                let (mut socket, _) = listener.accept().await.unwrap();
                // Drain the request (headers + any body) before responding, so the
                // client's write doesn't block on a full socket buffer.
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

    /// s3_get must retry a transient 503 and succeed on the next attempt, not fail
    /// the whole task on one blip (Data Transfer & Artifact I/O 4.5->5).
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

    /// A 404 (e.g. an expired or malformed presigned URL) is never transient — it
    /// must fail on the FIRST attempt, not burn the retry budget on a request that
    /// will never succeed.
    #[tokio::test]
    async fn s3_get_does_not_retry_client_error() {
        let base = spawn_sequenced_mock_server(vec![(404, "not found")]).await;
        let client = reqwest::Client::new();
        let err = s3_get(&client, &base)
            .await
            .expect_err("404 must not be retried into a success");
        assert!(err.to_string().contains("error status") || err.to_string().contains("404"));
    }

    /// s3_put_bytes must retry a transient 503 and succeed on the next attempt.
    #[tokio::test]
    async fn s3_put_bytes_retries_transient_5xx_then_succeeds() {
        let base = spawn_sequenced_mock_server(vec![(503, "try again"), (204, "")]).await;
        let client = reqwest::Client::new();
        s3_put_bytes(&client, &base, b"payload", "application/json")
            .await
            .expect("should succeed after one retry");
    }

    /// A connect_timeout must fail MUCH faster than the total-request timeout —
    /// proving the two are genuinely distinct failure modes (Data Transfer &
    /// Artifact I/O 5->6), not the same 120s ceiling wearing two names. We point
    /// at a black-holed address (a non-routable TEST-NET-adjacent IP that never
    /// answers a SYN, confirmed above to reliably time out client-side rather
    /// than fast-RST) with a deliberately short connect_timeout and a much
    /// longer total timeout, and assert the failure lands close to the connect
    /// bound, not the total one.
    #[tokio::test]
    async fn connect_timeout_fails_fast_and_distinct_from_total_timeout() {
        let client = reqwest::Client::builder()
            .connect_timeout(Duration::from_millis(500))
            .timeout(Duration::from_secs(60))
            .build()
            .unwrap();
        let start = std::time::Instant::now();
        // 10.255.255.1 is routed nowhere on this host/CI network and silently
        // drops the SYN rather than answering — the same black-hole behavior a
        // dead/unreachable presigned-URL host would exhibit.
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

    /// A minimal, real (no mocking framework) HTTP/1.1 server that accepts ONE
    /// connection, records the raw request headers it received, and writes back
    /// exactly `raw_response` verbatim (caller controls status line, headers,
    /// and how much — or little — of the declared body actually gets sent,
    /// so it can simulate a connection dropped mid-body). Returns the captured
    /// headers via the shared `captured` slot once the connection closes.
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

    /// Range-resume plumbing (Data Transfer & Artifact I/O 5->6): a first
    /// attempt whose connection is severed after only PART of the declared
    /// `Content-Length` arrives must be recognized as a body-read failure
    /// (not a connect/status failure), and the retry must send a real
    /// `Range: bytes=N-` header for the bytes already read; when the server
    /// honors it with a genuine `206 Partial Content`, the resumed bytes are
    /// appended to what was already read rather than the object being
    /// re-fetched from byte 0.
    #[tokio::test]
    async fn s3_get_sends_range_header_on_resume_and_appends_206_body() {
        // "hello world" (11 bytes) declared, but only "hello " (6 bytes) actually
        // written before the connection closes — a truncated body, not a clean
        // 200. Content-Length lies on purpose to trigger a body-read error out
        // of resp.bytes() rather than a clean short read.
        let (base1, _headers1) = spawn_raw_mock_server(
            b"HTTP/1.1 200 OK\r\nContent-Type: text/plain\r\nContent-Length: 11\r\nConnection: close\r\n\r\nhello ",
        )
        .await;
        // First attempt: connect straight to the truncating server.
        let client = reqwest::Client::new();
        let first = s3_get_once(&client, &base1, &[]).await;
        let partial_err = first.expect_err("a truncated body must surface as a read failure");
        let partial = partial_err
            .downcast_ref::<PartialBodyError>()
            .expect("truncated body must be a PartialBodyError, not some other failure");
        assert_eq!(
            partial.bytes_read.len(),
            0,
            "this pass's bookkeeping is the documented minimal version: a whole-body \
             resp.bytes() failure carries forward only what the caller already had \
             (zero on a first attempt), not true streamed byte-accounting"
        );

        // Second attempt (simulating the retry loop's resume): server serves a
        // real 206 Partial Content for the remaining bytes, and we seed
        // `already_read` with what a prior successful partial read would have
        // captured, to prove the header + append mechanism itself.
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

    /// gzip Accept-Encoding negotiation (Data Transfer & Artifact I/O 6->7):
    /// a server that declares `Content-Encoding: gzip` and sends a real
    /// gzip-compressed body must be transparently decoded by s3_get — proving
    /// reqwest's gzip feature (enabled in Cargo.toml) actually does the work,
    /// rather than assuming it from the feature flag alone.
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
        // The gzip feature is a client-level default (Accept-Encoding is added
        // and the response transparently inflated) — reqwest::Client::new()
        // already has it since the crate feature is compiled in.
        let client = reqwest::Client::new();
        let bytes = s3_get(&client, &base)
            .await
            .expect("a real gzip response must be transparently decoded");
        assert_eq!(
            bytes, plaintext,
            "s3_get must hand back the DECODED plaintext body, not raw gzip bytes"
        );
    }

    /// Tiny in-test-only gzip encoder so the gzip-decoding test above can
    /// construct a REAL compressed body without adding a production dependency
    /// just for a test fixture. Implemented directly against the DEFLATE-via-
    /// miniz_oxide primitives already vendored by this workspace's dependency
    /// tree would be its own small maintenance burden, so instead this uses the
    /// `flate2` crate, which is already resolved in Cargo.lock as a transitive
    /// dependency of reqwest's own gzip feature — adding it as a `dev-dependency`
    /// fetches nothing new.
    mod flate2_test_helper {
        pub use flate2::write::GzEncoder;
        pub use flate2::Compression;
    }
}
