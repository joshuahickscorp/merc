mod config;
mod executor;
mod failure;
mod hardware;
mod models;
mod pool;
mod protocol;
mod quantized_llama_batched; // vendored + patched candle quantized_llama (bsz>1 batched prefill)
mod runtime_authority;
mod status;
mod types;

use std::path::PathBuf;
use std::sync::Arc;
use std::time::{Duration, SystemTime, UNIX_EPOCH};

use anyhow::{Context, Result};
use clap::{Parser, Subcommand};
use sysinfo::System;
use tokio::sync::Semaphore;

use config::AgentConfig;
use executor::{default_runners, dispatch, JobRunner, RunError};
use pool::ModelPool;
use protocol::ControlPlaneClient;
use status::StatusWriter;
use types::{Heartbeat, TaskCommit, TaskDispatch, WorkerCapability};

const AGENT_VERSION: &str = env!("CARGO_PKG_VERSION");

const MODEL_IDLE_EVICT_AFTER: Duration = Duration::from_secs(15 * 60);

const CX_SANDBOXED_ENV: &str = "CX_SANDBOXED";

const CX_SANDBOX_PROFILE_ENV: &str = "CX_SANDBOX_PROFILE";

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

#[cfg(target_os = "macos")]
fn reexec_under_sandbox_if_needed() {
    use std::os::unix::process::CommandExt;

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
        .env(CX_SANDBOXED_ENV, "1");

    let err = cmd.exec();
    sandbox_wrap_failed(&format!("failed to re-exec under {SANDBOX_EXEC} ({err})"));
}

#[cfg(not(target_os = "macos"))]
fn reexec_under_sandbox_if_needed() {}

#[cfg(target_os = "macos")]
fn resolve_sandbox_profile() -> Option<PathBuf> {
    let override_path = std::env::var(CX_SANDBOX_PROFILE_ENV)
        .ok()
        .filter(|p| !p.is_empty())
        .map(PathBuf::from);
    let exe_sibling = std::env::current_exe()
        .ok()
        .and_then(|exe| exe.parent().map(|d| d.join("cx-agent.sb")));
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
    Run {
        #[arg(long, default_value = "agent.toml")]
        config: PathBuf,
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
        #[arg(long, default_value = "1,2,4,8,16,32")]
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
    Version,
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

fn current_hour_local() -> u8 {
    unsafe {
        let now = libc::time(std::ptr::null_mut());
        let mut tm: libc::tm = std::mem::zeroed();
        libc::localtime_r(&now, &mut tm);
        tm.tm_hour.clamp(0, 23) as u8
    }
}

fn on_battery() -> bool {
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
        Command::Run { config } => {
            init_tracing();
            reexec_under_sandbox_if_needed();
            let cfg = AgentConfig::load(&config)
                .with_context(|| format!("loading config {}", config.display()))?;
            run_agent(cfg).await
        }
    }
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
    let reps = reps.max(1) as usize; // never 0 reps  -  that would sweep nothing and divide by zero below

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
        anyhow::bail!("no batch sizes given (e.g. --batch-sizes 1,2,4,8,16,32)");
    }

    let device = models::device_label();
    let engine = "candle";
    let build_hash = hardware::engine_build_hash(engine, AGENT_VERSION);
    eprintln!("== cx-agent bench-batch ==");
    eprintln!(
        "device={device} model={model} max_tokens={max_tokens} mode={} build_hash={build_hash}",
        mode.label()
    );

    let mut be =
        executor::LlamaBackend::load(model).map_err(|e| anyhow::anyhow!("load {model}: {e}"))?;

    let (warm_text, _) = be
        .generate(prompt, max_tokens)
        .map_err(|e| anyhow::anyhow!("warmup generate: {e}"))?;

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
        let (text, tok) = be
            .generate(p, max_tokens)
            .map_err(|e| anyhow::anyhow!("serial generate: {e}"))?;
        serial_total_dt += t.elapsed().as_secs_f64();
        serial_total_tok += tok;
        expected.insert(p.clone(), text);
    }
    if serial_total_tok == 0 {
        anyhow::bail!(
            "serial baseline produced 0 tokens for model {model:?}  -  cannot benchmark \
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
        tokens_per_s: f64,
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
            if let Ok(extra_ms) = std::env::var("CX_BENCH_SYNTHETIC_DELAY_MS") {
                if let Ok(ms) = extra_ms.parse::<u64>() {
                    std::thread::sleep(Duration::from_millis(ms));
                }
            }
            let wall = t.elapsed().as_secs_f64();
            let total_tok: usize = res.iter().map(|(_, n)| n).sum();
            let tps = total_tok as f64 / wall;
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
        "mode": mode.label(),
        "distinct_prompts": distinct_prompts.len(),
        "prompt_preview": prompt.chars().take(60).collect::<String>(),
        "warmup_ok": !warm_text.is_empty(),
        "serial_baseline_tok_s": serial_tps,
        "peak_tok_s": peak_tps,
        "peak_speedup_vs_serial": peak_tps / serial_tps,
        "batched_deterministic_vs_serial": all_deterministic,
        "diverged_batches": diverged,
        "sweep": rows,
    });
    println!("{}", serde_json::to_string_pretty(&record)?);

    if require_deterministic && !all_deterministic {
        anyhow::bail!(
            "batched decode diverged from serial at batch {diverged:?} and \
             --require-deterministic was set  -  failing the determinism gate"
        );
    }
    Ok(())
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
    eprintln!("== cx-agent bench-sustained ==");
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
    eprintln!("== cx-agent bench-concurrency ==");
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

    eprintln!("warming models (cold load happens once, not counted in any sweep point)...");
    {
        let warm_pool = ModelPool::new();
        executor::EmbedRunner
            .run(&embed_manifest, &embed_input, &warm_pool)
            .await
            .map_err(|e| anyhow::anyhow!("warmup embed: {e}"))?;
        executor::BatchInferRunner
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
        executor::EmbedRunner
            .run(&embed_manifest, &embed_input, &pool)
            .await
            .map_err(|e| anyhow::anyhow!("re-warm embed: {e}"))?;
        executor::BatchInferRunner
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
                let permit = sem.acquire_owned().await.expect("semaphore never closed");
                let t = Instant::now();
                let res = executor::EmbedRunner.run(&manifest, &input, &pool).await;
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
                let res = executor::BatchInferRunner
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

    let mut input = s3_get(s3, &task.input_url)
        .await
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

    let result_sha256 = sha256_hex(&result);

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
    let resp = req.send().await.context("GET presigned input")?;
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

    let path = std::env::temp_dir().join(format!("cx-stage-{}.bin", uuid::Uuid::new_v4()));
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
    min_payout_usd_per_hr: f32,
    memory_headroom_gb: f32,
    max_memory_pct: f32,
    checkpoint_secs: u64,
    status: Arc<StatusWriter>,
}

async fn run_agent(mut cfg: AgentConfig) -> Result<()> {
    let pool = ModelPool::new();
    let mut cap = hardware::detect_and_benchmark(
        cfg.supplier_id,
        AGENT_VERSION,
        cfg.min_payout_usd_per_hr,
        "candle",
        &pool,
    )
    .await;
    let advertised_worker_id = cap.worker_id;
    let permits = cfg.concurrency(cap.memory_gb);

    let client = ControlPlaneClient::new(cfg.control_url.clone(), cfg.worker_token.clone())
        .context("building control-plane client (is worker_token set?)")?;
    let s3 = reqwest::Client::builder()
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
    status.set_applied_prefs(status::AppliedPrefs::from_config(&cfg, cap.memory_gb));
    status.registered();

    let ctx = WorkCtx {
        client: Arc::new(client),
        cap: Arc::new(cap),
        runners: Arc::new(default_runners()),
        pool,
        s3,
        min_payout_usd_per_hr: cfg.min_payout_usd_per_hr,
        memory_headroom_gb: cfg.memory_headroom_gb,
        max_memory_pct: cfg.max_memory_pct,
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
                let ts = now_unix();
                let cpu = cpu_pct(&mut sys);
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
    if !cfg.is_eligible_to_run(current_hour_local(), on_battery()) {
        tracing::debug!("not eligible to run (quiet hours / battery); idling 60s");
        drop(permit); // release the slot while we idle
        tokio::time::sleep(Duration::from_secs(60)).await;
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
    tracing::info!(task = %task.task_id, job = %task.job_id, "received task");

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

    let floor = ctx.min_payout_usd_per_hr;
    if floor > 0.0 && task.offered_rate_usd_hr > 0.0 && task.offered_rate_usd_hr < floor {
        tracing::warn!(
            task = %task.task_id,
            offered = task.offered_rate_usd_hr,
            floor,
            "offered rate below reservation price; skipping task (not started)"
        );
        return Ok(()); // never started -> control plane reassigns it
    }

    ctx.client
        .start_task(task.task_id, task.attempt)
        .await
        .context("start_task")?;
    ctx.status.job_started(
        task.task_id,
        task.attempt,
        task.job_id,
        task.manifest.job_type.tag(),
        now_unix(),
    );

    let ctx = ctx.clone();
    inflight.spawn(async move {
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
                if let Err(fe) = ctx.client.fail_task(task_id, task.attempt, &report).await {
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
    fn compressed_memory_fallback_is_bounded_and_fails_closed_without_stats() {
        assert_eq!(hardware::resolved_available_memory(100, 25, 80), 25);
        assert_eq!(hardware::resolved_available_memory(100, 0, 80), 20);
        assert_eq!(hardware::resolved_available_memory(100, 0, 0), 0);
        assert_eq!(hardware::resolved_available_memory(100, 0, 100), 0);
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
        let over = PathBuf::from("/opt/cx/override.sb");
        let sib = PathBuf::from("/Applications/CX.app/Contents/Resources/cx-agent.sb");

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
    fn sandbox_data_dir_is_compute_exchange_under_home() {
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
            partial.bytes_read.len(),
            0,
            "this pass's bookkeeping is the documented minimal version: a whole-body \
             resp.bytes() failure carries forward only what the caller already had \
             (zero on a first attempt), not true streamed byte-accounting"
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
