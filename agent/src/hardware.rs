//! Hardware detection and REAL on-box measurement.
//!
//! Everything here is a genuine measurement of the machine the agent runs on:
//! - chip identity via `sysctl machdep.cpu.brand_string`
//! - physical memory via `sysctl hw.memsize` (cross-checked with `sysinfo`)
//! - a real pure-Rust memory-bandwidth microbenchmark (no fabricated numbers)
//!
//! Per-model token/embedding throughput is REAL too: `detect_and_benchmark`
//! runs each available runner on a tiny fixed workload and measures eps/tps,
//! p99 latency, and a sustained-load thermal proxy (see `runners::run_benchmarks`).

use std::path::PathBuf;
use std::process::Command;
use std::time::{Instant, SystemTime, UNIX_EPOCH};

use serde::{Deserialize, Serialize};
use sysinfo::System;
use uuid::Uuid;

use crate::types::{BenchResult, HardwareClass, WorkerCapability};

/// How long a cached startup benchmark stays valid before it's re-measured anyway,
/// even if the (agent_version, build_hash, hardware) key still matches.
const BENCH_CACHE_MAX_AGE_SECS: u64 = 7 * 24 * 60 * 60;

/// On-disk shape of a cached startup benchmark (memory bandwidth + per-model
/// tok/s-or-eps sweep). Every real launch of the agent otherwise burns ~45-60s of
/// near-full-load GPU/CPU compute running these unconditionally (the fan-spin-up
/// moment most likely to make a supplier uninstall) — this cache lets a WARM
/// relaunch on the same build + same hardware skip straight to reusing the last
/// real measurement instead of re-running it.
#[derive(Debug, Clone, Serialize, Deserialize)]
struct BenchCache {
    /// Identifies exactly what this measurement is valid for: the agent version,
    /// the verification-class build hash (so a kernel/codegen change invalidates
    /// the cache automatically, never silently reuses a stale number), and a
    /// hardware fingerprint (CPU brand string + rounded host memory) so moving
    /// the same binary to different hardware also invalidates it.
    key: String,
    measured_unix: u64,
    memory_bw_gbps: f32,
    benchmarks: Vec<BenchResult>,
}

/// Resolve the bench-cache file path: `$CX_BENCH_CACHE_PATH` (when set and
/// non-empty), else `~/.compute-exchange/bench_cache.json`, else
/// `./bench_cache.json` if `$HOME` is unset — mirrors `status::status_path()`'s
/// resolution order exactly.
fn bench_cache_path() -> PathBuf {
    if let Ok(p) = std::env::var("CX_BENCH_CACHE_PATH") {
        if !p.is_empty() {
            return PathBuf::from(p);
        }
    }
    match std::env::var("HOME") {
        Ok(home) if !home.is_empty() => PathBuf::from(home)
            .join(".compute-exchange")
            .join("bench_cache.json"),
        _ => PathBuf::from("bench_cache.json"),
    }
}

fn bench_cache_key(agent_version: &str, build_hash: &str, brand: &str, host_mem_gb: f32) -> String {
    // Round memory to the nearest GB: sysinfo/sysctl readings are exact-byte but a
    // supplier's "same Mac" reading can jitter by a few MB across boots.
    format!(
        "{agent_version}|{build_hash}|{brand}|{}",
        host_mem_gb.round() as i64
    )
}

fn now_unix() -> u64 {
    SystemTime::now()
        .duration_since(UNIX_EPOCH)
        .map(|d| d.as_secs())
        .unwrap_or(0)
}

/// Load a cached benchmark if the key matches and it isn't stale. Any read/parse
/// failure (missing file, corrupt JSON, older schema) is treated as a cache miss —
/// never a hard error — so a broken cache file can never block startup; it just
/// falls back to re-measuring, exactly like a fresh install.
fn load_bench_cache(key: &str) -> Option<(f32, Vec<BenchResult>)> {
    let path = bench_cache_path();
    let bytes = std::fs::read(&path).ok()?;
    let cache: BenchCache = serde_json::from_slice(&bytes).ok()?;
    if cache.key != key {
        tracing::info!("bench cache: hardware/build changed since last run; re-measuring");
        return None;
    }
    let age = now_unix().saturating_sub(cache.measured_unix);
    if age > BENCH_CACHE_MAX_AGE_SECS {
        tracing::info!(age_secs = age, "bench cache: stale (>7d); re-measuring");
        return None;
    }
    tracing::info!(
        age_secs = age,
        path = %path.display(),
        "bench cache: reusing measured startup benchmark (skipping ~45-60s cold re-measure)"
    );
    Some((cache.memory_bw_gbps, cache.benchmarks))
}

/// Persist a freshly-measured benchmark. A write failure is logged and swallowed
/// (matches `status.rs`'s "never fail the caller over a side-channel write") — the
/// agent already has the real numbers in hand for THIS run; only the next launch's
/// cache hit is at stake.
fn save_bench_cache(key: &str, memory_bw_gbps: f32, benchmarks: &[BenchResult]) {
    let path = bench_cache_path();
    if let Some(parent) = path.parent() {
        if let Err(e) = std::fs::create_dir_all(parent) {
            tracing::warn!(error = %e, path = %parent.display(), "bench cache: failed to create directory; skipping write");
            return;
        }
    }
    let cache = BenchCache {
        key: key.to_string(),
        measured_unix: now_unix(),
        memory_bw_gbps,
        benchmarks: benchmarks.to_vec(),
    };
    match serde_json::to_vec_pretty(&cache) {
        Ok(bytes) => {
            if let Err(e) = std::fs::write(&path, bytes) {
                tracing::warn!(error = %e, path = %path.display(), "bench cache: failed to write; next launch will re-measure");
            }
        }
        Err(e) => {
            tracing::warn!(error = %e, "bench cache: failed to serialize; next launch will re-measure")
        }
    }
}

/// Bytes touched per streaming pass of the bandwidth benchmark (~256 MiB).
const BENCH_BYTES: usize = 256 * 1024 * 1024;
/// Number of streaming passes timed; we keep the best (peak) GB/s.
const BENCH_PASSES: usize = 5;

/// Run `sysctl -n <key>` and return trimmed stdout, or `None` on any failure.
/// macOS-only path; on other platforms `sysctl` is absent and this returns None.
fn sysctl(key: &str) -> Option<String> {
    let out = Command::new("sysctl").arg("-n").arg(key).output().ok()?;
    if !out.status.success() {
        return None;
    }
    let s = String::from_utf8_lossy(&out.stdout).trim().to_string();
    if s.is_empty() {
        None
    } else {
        Some(s)
    }
}

/// Map the CPU brand string to a `HardwareClass`.
///
/// Apple strings look like `"Apple M3 Max"`, `"Apple M2 Pro"`, `"Apple M1"`,
/// `"Apple M2 Ultra"`. Anything we don't recognize as Apple Silicon falls back
/// to `Cpu`.
fn classify(brand: &str) -> HardwareClass {
    let b = brand.to_ascii_lowercase();
    let is_apple = b.contains("apple") && b.contains(" m");
    if !is_apple {
        return HardwareClass::Cpu;
    }
    if b.contains("ultra") {
        HardwareClass::AppleSiliconUltra
    } else if b.contains("max") {
        HardwareClass::AppleSiliconMax
    } else if b.contains("pro") {
        HardwareClass::AppleSiliconPro
    } else {
        HardwareClass::AppleSiliconBase
    }
}

/// Query `nvidia-smi` for the first GPU's name and total VRAM (GB). Returns None
/// when nvidia-smi is absent or fails — i.e., not an NVIDIA host. A REAL reading,
/// never fabricated (BLACKHOLE: surface every failure).
fn nvidia_gpu() -> Option<(String, f32)> {
    let out = Command::new("nvidia-smi")
        .args([
            "--query-gpu=name,memory.total",
            "--format=csv,noheader,nounits",
        ])
        .output()
        .ok()?;
    if !out.status.success() {
        return None;
    }
    let stdout = String::from_utf8_lossy(&out.stdout);
    let first = stdout.lines().next()?.trim(); // e.g. "NVIDIA A100-SXM4-80GB, 81920"
    let (name, mib) = first.split_once(',')?;
    let mib: f64 = mib.trim().parse().ok()?;
    Some((name.trim().to_string(), (mib / 1024.0) as f32)) // MiB → GiB (~GB)
}

/// The REAL VRAM memory bandwidth (decimal GB/s) of a detected NVIDIA card, by its
/// `nvidia-smi` product name (CUDA Lane Performance & Parity 6→6.5).
///
/// Why this exists: `measure_memory_bandwidth_gbps()` runs a HOST-CPU streaming
/// microbenchmark (~40-60 GB/s on any box, Mac or cloud VM). On the Apple lane that
/// approximates the unified-memory bandwidth that actually gates inference, so it is
/// the right number there. On the CUDA lane it is meaningless and ~30x too low — an
/// A100's inference throughput is gated by its ~1.5-2 TB/s HBM, not the host VM's DDR.
/// Advertising the host number on an NVIDIA worker misrepresents the single most
/// decisive spec of the card by roughly 30x. Each entry below is the manufacturer's
/// published HBM/GDDR bandwidth for that EXACT SKU (the one `nvidia-smi` names) — a
/// real, verifiable spec for the detected card, not a fabricated or host-derived
/// figure. `None` for an unrecognized card, so the caller falls back to the honest
/// microbenchmark rather than inventing a number. Matched most-specific-substring
/// first (an "A100-SXM4-80GB" must not fall through to a generic "A100" 40GB entry).
fn nvidia_vram_bandwidth_gbps(gpu_name: &str) -> Option<f32> {
    let n = gpu_name.to_ascii_uppercase();
    // Order matters: more specific SKU markers precede their family fallback.
    const TABLE: &[(&str, f32)] = &[
        // Hopper / Blackwell (HBM3/HBM3e)
        ("H200", 4800.0),
        ("H100 SXM", 3350.0),
        ("H100 80GB HBM3", 3350.0),
        ("H100 PCIE", 2000.0),
        ("H100", 3350.0),
        ("GH200", 4900.0),
        ("B200", 8000.0),
        // Ampere datacenter (HBM2/HBM2e)
        ("A100-SXM4-80GB", 2039.0),
        ("A100 80GB PCIE", 1935.0),
        ("A100-PCIE-40GB", 1555.0),
        ("A100-SXM4-40GB", 1555.0),
        ("A100", 1555.0), // conservative family fallback (40GB HBM2)
        // Ampere / Ada workstation + inference (GDDR6/GDDR6X)
        ("A40", 696.0),
        ("A10G", 600.0),
        ("A10", 600.0),
        ("L40S", 864.0),
        ("L40", 864.0),
        ("L4", 300.0),
        ("RTX 6000 ADA", 960.0),
        ("RTX 4090", 1008.0),
        ("RTX 3090", 936.0),
        // Turing / Volta datacenter
        ("V100", 900.0),
        ("T4", 320.0),
    ];
    TABLE
        .iter()
        .find(|(marker, _)| n.contains(marker))
        .map(|(_, gbps)| *gbps)
}

/// The worker's advertised `memory_bw_gbps`: the detected NVIDIA card's REAL VRAM
/// bandwidth when this is an NVIDIA host (the number that actually gates GPU
/// inference), else the host streaming microbenchmark (correct for the Apple unified
/// / CPU lanes). Never fabricates: an unrecognized NVIDIA card falls back to the
/// honest microbenchmark with a warning rather than a guessed VRAM figure.
fn advertised_memory_bw_gbps() -> f32 {
    if let Some((name, _vram)) = nvidia_gpu() {
        if let Some(vram_bw) = nvidia_vram_bandwidth_gbps(&name) {
            tracing::info!(gpu = %name, vram_bw_gbps = vram_bw, "advertising REAL NVIDIA VRAM bandwidth (spec for the detected SKU), not the host microbenchmark");
            return vram_bw;
        }
        tracing::warn!(gpu = %name, "unrecognized NVIDIA card: no VRAM-bandwidth spec on file, falling back to the host streaming microbenchmark (which UNDERSTATES real GPU bandwidth) — add this SKU to nvidia_vram_bandwidth_gbps");
    }
    measure_memory_bandwidth_gbps()
}

/// Map NVIDIA VRAM (GB) to a VRAM-tiered `HardwareClass`. VRAM is the gating
/// resource on NVIDIA (what decides which models fit), as unified memory is on
/// Apple. The top tier is a catch-all for frontier / multi-GPU cards (H200/B200) so
/// a larger card is never mislabeled into a smaller tier.
fn classify_nvidia(vram_gb: f32) -> HardwareClass {
    if vram_gb <= 24.0 {
        HardwareClass::Nvidia24g
    } else if vram_gb <= 48.0 {
        HardwareClass::Nvidia48g
    } else if vram_gb <= 80.0 {
        HardwareClass::Nvidia80g
    } else {
        HardwareClass::Nvidia180g
    }
}

/// The quantization the wired (Candle) runners load the catalogue under. This is
/// the byte-output-determining weight format, and it is a fixed property of the
/// shipped catalogue (every GGUF in `models.rs` is `Q4_K_M`), not a runtime knob.
/// It is folded into the build hash so that a future requant (e.g. a sub-Q4 codec
/// lane, audit Wave 3) lands in a DIFFERENT verification class and is never
/// byte-compared against a Q4_K_M peer. A bare string keeps the seam honest: when
/// a runtime ever varies quant per model, derive this from the loaded weights.
const CATALOGUE_QUANT: &str = "q4_k_m";

/// A stable, short identity of the ENGINE BUILD this worker runs — the finer axis
/// of the verification class below hardware (mirrors Hawking's profile identity,
/// which keys on device_name + shader_hash + tensor_layout_hash; see
/// docs/DETERMINISM_CLASS.md). It hashes the byte-output-determining build inputs:
///   - `engine`        — the runtime tag (`candle` / `mlx` / `vllm` / `hawking`);
///   - `agent_version` — the cx-agent build (a kernel/codegen change ships with a
///     new agent build, so the version stands in for "shader/kernel build" the way
///     Hawking's shader_hash does);
///   - device backend  — `metal` vs `cuda` vs `cpu` (the same engine emits DIFFERENT
///     FP bytes per backend, exactly the cross-Mac/CUDA split the audit's determinism
///     ledger calls out);
///   - `CATALOGUE_QUANT` — the weight format the catalogue is loaded under;
///   - inference content hash — a SHA-256 over the owned forward/scheduler sources,
///     the vendored Candle Metal Q4_K host+shader sources, and the Cargo lock;
///   - runtime tuning identity — the non-secret speculative/kernel environment
///     knobs that select materially different execution paths.
///
/// Together these move an output-changing kernel/forward patch (or a runtime
/// split-K/speculation selection) into a new class even WITHOUT an
/// `agent_version` bump. This closes the moat hole where a kernel patch shipped
/// into the SAME class and could dock honest old-kernel peers
/// (CANDLE_EXPANSION_RESEARCH L17).
///
/// The control plane pins BYTE-EXACT redundancy peers and honeypots to the same
/// (hw_class, engine, build_hash). Two workers in the same hw_class + engine but on
/// different agent builds (a kernel change between releases) therefore do NOT
/// auto-dock each other on a pure byte mismatch — they are a different class and
/// fall back to provisional trust, the same pattern the third-worker tiebreak uses.
/// Hawking's own research proves token-level determinism is impossible across
/// heterogeneous Apple-Silicon generations, so this boundary is the moat, not a
/// nicety. The hash is the first 16 hex chars of a SHA-256 — short, stable, and
/// collision-safe for a class tag (NOT a security primitive).
/// SHA-256 (first 8 bytes, hex) of every owned/vendored source that can alter this
/// inference lane's token bytes, plus its dependency lock. `include_str!` pins them
/// at compile time. Over-sensitive by design (a comment-only edit also moves the
/// class): the only cost is an unnecessary reseed, never a wrongful same-class
/// byte comparison.
const INFERENCE_CONTENT_SOURCES: &[(&str, &str)] = &[
    (
        "quantized_llama_batched.rs",
        include_str!("quantized_llama_batched.rs"),
    ),
    (
        "whisper_decoder_kv.rs",
        include_str!("whisper_decoder_kv.rs"),
    ),
    ("runners.rs", include_str!("runners.rs")),
    ("continuous_batch.rs", include_str!("continuous_batch.rs")),
    (
        "hawking_metal_kernel.rs",
        include_str!("hawking_metal_kernel.rs"),
    ),
    (
        "vendor/candle-metal-kernels/src/kernels/quantized.rs",
        include_str!("../vendor/candle-metal-kernels/src/kernels/quantized.rs"),
    ),
    (
        "vendor/candle-metal-kernels/src/metal_src/quantized.metal",
        include_str!("../vendor/candle-metal-kernels/src/metal_src/quantized.metal"),
    ),
    ("Cargo.lock", include_str!("../Cargo.lock")),
];

pub fn infer_content_id() -> String {
    use sha2::{Digest, Sha256};
    let mut h = Sha256::new();
    for (name, source) in INFERENCE_CONTENT_SOURCES {
        h.update((name.len() as u64).to_le_bytes());
        h.update(name.as_bytes());
        h.update((source.len() as u64).to_le_bytes());
        h.update(source.as_bytes());
    }
    h.finalize()
        .iter()
        .take(8)
        .map(|b| format!("{b:02x}"))
        .collect()
}

/// Non-secret runtime switches that select different native inference math or
/// scheduling. Raw values are intentionally retained: an invalid/unusual setting
/// is over-separated rather than accidentally sharing a class with the default.
fn inference_runtime_tuning_identity(engine: &str) -> String {
    if !matches!(engine, "candle" | "hawking") {
        return "native-tuning=not-applicable".to_string();
    }
    let value = |name: &str, default: &str| {
        std::env::var(name)
            .ok()
            .filter(|value| !value.is_empty())
            .unwrap_or_else(|| default.to_string())
    };
    let spec_mode = value("CX_SPEC_DECODE", "off");
    let (spec_window, spec_order) = if matches!(spec_mode.trim(), "1" | "on" | "ngram") {
        (
            value("CX_SPEC_DECODE_WINDOW", "32"),
            value("CX_SPEC_DECODE_NGRAM_ORDER", "3"),
        )
    } else {
        ("inactive".to_string(), "inactive".to_string())
    };
    format!(
        "spec={spec_mode};window={spec_window};order={spec_order};q4k_splitk={};q4k_skinny_m={};dequant_f16={};fast_math={};metal_compute_per_buffer={};metal_command_pool_size={}",
        value("CX_Q4K_SPLITK", "0"),
        value("CX_Q4K_SKINNY_M", "0"),
        value("CANDLE_DEQUANTIZE_ALL_F16", "0"),
        value("CANDLE_METAL_ENABLE_FAST_MATH", "default"),
        value("CANDLE_METAL_COMPUTE_PER_BUFFER", "default"),
        value("CANDLE_METAL_COMMAND_POOL_SIZE", "default"),
    )
}

pub fn engine_build_hash(engine: &str, agent_version: &str) -> String {
    engine_build_hash_inner(
        engine,
        agent_version,
        &infer_content_id(),
        &inference_runtime_tuning_identity(engine),
    )
}

/// Pure core of `engine_build_hash`, taking the inference-module content id
/// explicitly so a test can prove a content-id change moves the class WITHOUT
/// mutating a source file. The public wrapper feeds it `infer_content_id()`.
fn engine_build_hash_inner(
    engine: &str,
    agent_version: &str,
    infer_content_id: &str,
    runtime_tuning_identity: &str,
) -> String {
    use sha2::{Digest, Sha256};
    let mut h = Sha256::new();
    // Length-prefix each field so distinct field splits never collide
    // (e.g. ("ab","c") vs ("a","bc")). NUL-separate as a second guard.
    for field in [
        engine,
        agent_version,
        crate::models::device_label(),
        CATALOGUE_QUANT,
        infer_content_id,
        runtime_tuning_identity,
    ] {
        h.update((field.len() as u32).to_le_bytes());
        h.update(field.as_bytes());
        h.update([0]);
    }
    let digest = h.finalize();
    digest.iter().take(8).map(|b| format!("{b:02x}")).collect()
}

/// Read the OS version string for the capability record.
/// Uses `sw_vers -productVersion` on macOS, falling back to `sysinfo`.
fn os_version() -> String {
    if let Some(v) = Command::new("sw_vers")
        .arg("-productVersion")
        .output()
        .ok()
        .filter(|o| o.status.success())
        .map(|o| String::from_utf8_lossy(&o.stdout).trim().to_string())
        .filter(|s| !s.is_empty())
    {
        return format!("macOS {v}");
    }
    System::long_os_version().unwrap_or_else(|| "unknown".to_string())
}

/// REAL streaming-read memory-bandwidth microbenchmark, in pure Rust.
///
/// Allocates a ~256 MiB buffer and runs several timed passes that read and
/// lightly transform every 8-byte word, accumulating into a checksum the
/// compiler cannot elide. We report the *peak* throughput across passes as
/// GB/s (decimal gigabytes, to match how memory bandwidth is usually quoted).
///
/// This measures achievable single-thread streaming read bandwidth, which is a
/// genuine and reproducible figure — not a spec-sheet number.
pub fn measure_memory_bandwidth_gbps() -> f32 {
    let len = BENCH_BYTES / std::mem::size_of::<u64>();
    let mut buf: Vec<u64> = (0..len as u64).collect();
    // Touch once to ensure pages are resident before timing.
    let mut warm: u64 = 0;
    for &x in &buf {
        warm = warm.wrapping_add(x);
    }
    std::hint::black_box(warm);

    let mut best_gbps = 0.0f32;
    for pass in 0..BENCH_PASSES {
        let salt = pass as u64;
        let start = Instant::now();
        let mut acc: u64 = 0;
        for v in buf.iter_mut() {
            // Read + cheap transform + write-back: exercises the bus both ways.
            let x = v.wrapping_mul(6364136223846793005).wrapping_add(salt);
            acc = acc.wrapping_add(x);
            *v = x;
        }
        let elapsed = start.elapsed().as_secs_f64();
        std::hint::black_box(acc);
        if elapsed > 0.0 {
            // Count both the read and the write-back traffic.
            let bytes = (BENCH_BYTES as f64) * 2.0;
            let gbps = (bytes / 1e9) / elapsed;
            best_gbps = best_gbps.max(gbps as f32);
        }
    }
    best_gbps
}

/// A REAL, point-in-time reading of physical memory (bytes → decimal GB).
///
/// `available_gb` is the kernel's estimate of memory obtainable *without*
/// swapping (free + reclaimable), not just free RAM — that is the figure that
/// decides whether the box can take another job without being pushed into swap.
/// Total memory alone is never enough (a 64 GB Mac with 2 GB free must NOT be
/// handed a 4 GB job), which is the whole point of dynamic throttling.
#[derive(Debug, Clone, Copy, PartialEq)]
pub struct MemorySnapshot {
    pub total_gb: f32,
    pub available_gb: f32,
}

impl MemorySnapshot {
    /// Used fraction of physical memory, in percent (0..=100). Pure.
    pub fn used_pct(&self) -> f32 {
        if self.total_gb <= 0.0 {
            return 0.0;
        }
        (((self.total_gb - self.available_gb) / self.total_gb) * 100.0).clamp(0.0, 100.0)
    }
}

/// Take a REAL memory reading from the OS via `sysinfo` (host statistics on
/// macOS). Self-contained — refreshes only memory on a throwaway `System` — so it
/// is cheap enough to call on every poll/heartbeat and never fabricates a number.
pub fn read_memory_snapshot() -> MemorySnapshot {
    let mut sys = System::new();
    sys.refresh_memory();
    // sysinfo 0.31 reports memory in BYTES (matches `hw.memsize` used above).
    MemorySnapshot {
        total_gb: (sys.total_memory() as f64 / 1e9) as f32,
        available_gb: (sys.available_memory() as f64 / 1e9) as f32,
    }
}

/// A REAL, point-in-time reading of the first NVIDIA GPU's VRAM via `nvidia-smi`,
/// shaped as the same `MemorySnapshot` the host-RAM throttle consumes so the
/// gating logic is identical against VRAM. `total_gb` = total VRAM, `available_gb`
/// = free VRAM (`memory.free`), both in decimal GB.
///
/// On a GPU box the gating resource is VRAM, not host RAM: a 24 GB card with most
/// of its VRAM already resident must NOT be handed a ~20 GB-VRAM job, exactly as a
/// pressured Mac must not take a job that would push it into swap. Returns `None`
/// when nvidia-smi is absent or fails — i.e. no readable NVIDIA GPU — so the caller
/// surfaces the failure honestly (BLACKHOLE) rather than fabricating headroom.
pub fn read_vram_snapshot() -> Option<MemorySnapshot> {
    let out = Command::new("nvidia-smi")
        .args([
            "--query-gpu=memory.free,memory.total",
            "--format=csv,noheader,nounits",
        ])
        .output()
        .ok()?;
    if !out.status.success() {
        return None;
    }
    let stdout = String::from_utf8_lossy(&out.stdout);
    let first = stdout.lines().next()?.trim(); // e.g. "11096, 24576" (MiB, nounits)
    let (free_mib, total_mib) = first.split_once(',')?;
    let free_mib: f64 = free_mib.trim().parse().ok()?;
    let total_mib: f64 = total_mib.trim().parse().ok()?;
    if total_mib <= 0.0 {
        return None; // a zero/garbage total is not a usable reading
    }
    // MiB → decimal GB, matching read_memory_snapshot's units so the throttle
    // thresholds (headroom_gb, max_memory_pct) carry over unchanged.
    let mib_to_gb = |mib: f64| (mib * 1024.0 * 1024.0 / 1e9) as f32;
    Some(MemorySnapshot {
        total_gb: mib_to_gb(total_mib),
        available_gb: mib_to_gb(free_mib),
    })
}

/// Detect hardware class and take real measurements, producing the
/// `WorkerCapability` advertised at registration.
///
/// `worker_id` is freshly minted per process start; `supplier_id` comes from
/// config. `min_payout_usd_hr` is the operator reservation price (0.0 from
/// `bench`). `engine` is the on-device inference engine tag this worker advertises
/// (`candle` default, from `config.inference_backend.engine_tag()`); it is the
/// second axis of the verification class so a future mlx/vllm/hawking worker is
/// never byte-compared against a Candle one. The advertised
/// `supported_models`/`supported_jobs` are compatibility roll-ups derived from
/// the generated production cells for this exact engine/hardware pair, not from
/// whatever happened to benchmark. The control plane persists the underlying
/// exact cells and never treats the arrays as Cartesian authority.
/// `pool` is the SAME `ModelPool` the agent reuses for real task dispatch
/// afterward (Warm Model Pool 6->6.5, docs/internal/CREED_AND_PATH_TO_TEN.md):
/// the benchmark load below is routed through it so it becomes the agent's one
/// real cold load per model, not a rehearsal that gets dropped and re-paid on
/// the first real task.
pub async fn detect_and_benchmark(
    supplier_id: Uuid,
    agent_version: &str,
    min_payout_usd_hr: f32,
    engine: &str,
    pool: &crate::pool::ModelPool,
) -> WorkerCapability {
    let mut sys = System::new();
    sys.refresh_memory();

    // Physical (host) memory: prefer sysctl hw.memsize (exact bytes), fall back to sysinfo.
    let mem_bytes = sysctl("hw.memsize")
        .and_then(|s| s.parse::<u64>().ok())
        .unwrap_or_else(|| sys.total_memory());
    let host_mem_gb = (mem_bytes as f64 / 1e9) as f32;

    // Class + the gating-memory figure the scheduler filters on. On the CUDA lane the
    // gating resource is VRAM and the class is nvidia_* — NOT host RAM / an Apple
    // class; we detect it via nvidia-smi. Off the CUDA lane it's the Apple/CPU class
    // + host unified memory. device_label() reflects the actually-selected backend.
    let brand = sysctl("machdep.cpu.brand_string").unwrap_or_else(|| "unknown".to_string());
    let (hw_class, memory_gb) = if crate::models::device_label() == "cuda" {
        match nvidia_gpu() {
            Some((name, vram_gb)) => {
                let class = classify_nvidia(vram_gb);
                tracing::info!(gpu = %name, ?class, vram_gb, "detected NVIDIA GPU (CUDA lane)");
                (class, vram_gb)
            }
            None => {
                tracing::warn!("CUDA device active but nvidia-smi unavailable; advertising `cpu`");
                (HardwareClass::Cpu, host_mem_gb)
            }
        }
    } else {
        let class = classify(&brand);
        if class == HardwareClass::Cpu {
            tracing::warn!(cpu = %brand, "could not map to an Apple Silicon class; advertising as `cpu`");
        } else {
            tracing::info!(cpu = %brand, ?class, memory_gb = host_mem_gb, "detected hardware");
        }
        (class, host_mem_gb)
    };

    // Cache key: same agent build (build_hash folds in the verification-class
    // content id, so a kernel/codegen change invalidates it automatically) on the
    // same physical hardware. A hit skips ~45-60s of near-full-load GPU/CPU
    // benchmarking on every relaunch — the exact fan-spin-up moment that makes a
    // supplier notice and consider uninstalling.
    let build_hash = engine_build_hash(engine, agent_version);
    let cache_key = bench_cache_key(agent_version, &build_hash, &brand, host_mem_gb);

    let (memory_bw_gbps, benchmarks) = match load_bench_cache(&cache_key) {
        Some(cached) => cached,
        None => {
            tracing::info!("resolving advertised memory bandwidth (real NVIDIA VRAM spec on a CUDA host; host streaming microbenchmark on Apple/CPU)...");
            let memory_bw_gbps = advertised_memory_bw_gbps();
            tracing::info!(memory_bw_gbps, "resolved advertised memory bandwidth");

            // REAL per-model benchmarks: load each backend and measure it on a tiny
            // workload. Models that fail to load (e.g. HF unreachable) are skipped with
            // a warning inside `run_benchmarks` — never replaced by fabricated numbers.
            tracing::info!(
                device = crate::models::device_label(),
                "running real model benchmarks (embed, llama 1B, llama 7B, whisper, rerank; ~20s each)…"
            );
            let benchmarks = crate::runners::run_benchmarks(pool, memory_gb).await;
            for b in &benchmarks {
                tracing::info!(
                    model = %b.model_id, eps = b.eps, tps = b.tps, p99_ms = b.p99_ms,
                    thermal_ok = b.thermal_ok, "benchmark"
                );
            }
            save_bench_cache(&cache_key, memory_bw_gbps, &benchmarks);
            (memory_bw_gbps, benchmarks)
        }
    };
    // Production advertisement comes from the SAME generated exact-cell projection
    // the control plane consumes. The arrays remain compatibility roll-ups on the
    // wire, but they are no longer a separately maintained catalogue that can drift
    // or accidentally light a hardware-pending runner. Benchmark rows are filtered
    // to those exact tuples as well: measuring a soak/pending model is useful local
    // research, never authority to register it as production supply.
    let authorized = generated_authorized_capabilities(engine, hw_class);
    let supported_jobs: Vec<String> = authorized
        .iter()
        .map(|cell| cell.job)
        .collect::<std::collections::BTreeSet<_>>()
        .into_iter()
        .map(str::to_string)
        .collect();
    let supported_models: Vec<String> = authorized
        .iter()
        .filter_map(|cell| cell.model)
        .collect::<std::collections::BTreeSet<_>>()
        .into_iter()
        .map(str::to_string)
        .collect();
    let benchmarks: Vec<BenchResult> = benchmarks
        .into_iter()
        .filter(|bench| {
            authorized.iter().any(|cell| {
                cell.job == bench.job_type && cell.model == Some(bench.model_id.as_str())
            })
        })
        .collect();

    WorkerCapability {
        worker_id: Uuid::new_v4(),
        supplier_id,
        hw_class,
        // The on-device inference engine this worker runs (`candle` default). It is
        // the second axis of the verification class: the control plane pins byte-exact
        // redundancy peers + honeypots to the SAME (hw_class, engine), so a future
        // mlx/vllm/hawking worker is never byte-compared against a Candle one.
        engine: engine.to_string(),
        // The finer axis of the verification class BELOW (hw_class, engine): a stable
        // hash of the byte-output-determining build inputs (engine + agent build +
        // device backend + catalogue quant). The control plane pins byte-exact
        // redundancy peers + honeypots to the same (hw_class, engine, build_hash), so a
        // kernel/codegen change shipped in a NEW agent build lands in a new class and is
        // never byte-docked against an old-build peer (docs/DETERMINISM_CLASS.md).
        build_hash,
        memory_gb,
        memory_bw_gbps,
        supported_jobs,
        supported_models,
        benchmarks,
        agent_version: agent_version.to_string(),
        os_version: os_version(),
        min_payout_usd_hr,
    }
}

fn generated_authorized_capabilities(
    engine: &str,
    hw_class: HardwareClass,
) -> Vec<&'static crate::runtime_matrix_generated::GeneratedRuntimeCapability> {
    generated_authorized_capabilities_for(engine, hw_class, crate::models::device_label())
}

fn generated_authorized_capabilities_for(
    engine: &str,
    hw_class: HardwareClass,
    device: &str,
) -> Vec<&'static crate::runtime_matrix_generated::GeneratedRuntimeCapability> {
    let hw_tag = hw_class.as_wire_str();
    crate::runtime_matrix_generated::ADVERTISED_RUNTIME_CAPABILITIES
        .iter()
        .filter(|cell| {
            cell.engine == engine
                && cell.device == device
                && cell.hardware_classes.contains(&hw_tag)
        })
        .collect()
}

/// Sample the active GPU's utilization (%) and temperature (°C) via nvidia-smi for the
/// heartbeat. `Some((util_pct, temp_c))` on the NVIDIA lane; `None` if nvidia-smi is
/// absent or fails — the caller then reports an honest 0.0/None, never a fabricated
/// load. Mirrors read_vram_snapshot's nvidia-smi parsing.
pub fn read_gpu_telemetry() -> Option<(f32, Option<f32>)> {
    let out = Command::new("nvidia-smi")
        .args([
            "--query-gpu=utilization.gpu,temperature.gpu",
            "--format=csv,noheader,nounits",
        ])
        .output()
        .ok()?;
    if !out.status.success() {
        return None;
    }
    let stdout = String::from_utf8_lossy(&out.stdout);
    let first = stdout.lines().next()?.trim(); // e.g. "37, 58" (%, °C)
    let (util, temp) = first.split_once(',')?;
    let util_pct: f32 = util.trim().parse().ok()?;
    // Temperature reads "N/A" on some virtualized GPUs — keep utilization, drop temp.
    let temp_c: Option<f32> = temp.trim().parse().ok();
    Some((util_pct, temp_c))
}

/// PATCH (P-real-platform-signals, docs/internal/CREED_AND_PATH_TO_TEN.md, "Agent
/// idle footprint & startup overhead" 7→8): "Replace subprocess polling with real
/// platform signals". Battery state used to be read by spawning `pmset -g batt`
/// (main.rs's old `on_battery`) — a subprocess fork+exec+parse on every poll cycle,
/// documented in this same audit as "~6,500 times a day". `IOPSGetProvidingPowerSourceType`
/// is the real macOS/IOKit API for the exact same fact (AC vs battery vs UPS),
/// read in-process via the `IOKit.framework` C ABI — no fork, no shell, no text
/// parsing. Returns `false` (not on battery / can't tell) when the info blob or
/// the power-source-type string can't be read, matching the old subprocess path's
/// own fail-open behavior (a failed pmset call also returned "not on battery").
#[cfg(target_os = "macos")]
pub fn on_battery() -> bool {
    use objc2_core_foundation::{CFRetained, CFString, CFType};
    use std::ffi::c_void;
    use std::ptr::NonNull;

    #[link(name = "IOKit", kind = "framework")]
    extern "C" {
        fn IOPSCopyPowerSourcesInfo() -> *const c_void;
        fn IOPSGetProvidingPowerSourceType(snapshot: *const c_void) -> *const c_void;
    }

    // SAFETY: both functions are read-only C queries into IOKit's power-source
    // registry. `IOPSCopyPowerSourcesInfo` follows CF "Copy" semantics (caller
    // owns one reference, released via `CFRetained::from_raw` + drop below).
    // `IOPSGetProvidingPowerSourceType` follows CF "Get" semantics — the
    // returned `CFStringRef` is owned by (and lives at least as long as) the
    // `blob`, so we read it BEFORE releasing `blob_ref`, never after.
    unsafe {
        let blob = IOPSCopyPowerSourcesInfo();
        let Some(blob_ptr) = NonNull::new(blob as *mut CFType) else {
            return false; // no power-source info available; fail open (not on battery)
        };
        let blob_ref: CFRetained<CFType> = CFRetained::from_raw(blob_ptr);
        let type_ref = IOPSGetProvidingPowerSourceType(blob);
        let on_battery = if type_ref.is_null() {
            false
        } else {
            (*(type_ref as *const CFString))
                .to_string()
                .contains("Battery")
        };
        drop(blob_ref);
        on_battery
    }
}

/// Non-macOS builds (the CUDA/Linux lane) have no IOKit power-source registry —
/// a rented GPU box is never running on battery, so report the honest constant
/// rather than fabricating a subprocess call that doesn't exist on that platform.
#[cfg(not(target_os = "macos"))]
pub fn on_battery() -> bool {
    false
}

/// PATCH (P-real-platform-signals, docs/internal/CREED_AND_PATH_TO_TEN.md, "Agent
/// idle footprint & startup overhead" 7→8): real thermal-pressure + low-power-mode
/// reading via `NSProcessInfo` (Cocoa/Foundation), in-process — no subprocess, no
/// `powermetrics`/`pmset` text scraping. This is the SAME signal
/// `config::ThermalPressure`/`evaluate_thermal_throttle` already model and unit-test
/// (that machinery was built ahead of a real reader; this IS the real reader) — Apple's
/// own definition of "this device is measurably hot", the identical enum the OS uses
/// internally to throttle itself. Maps `NSProcessInfoThermalState` (0..=3) onto our
/// `config::ThermalPressure` 1:1; any future OS thermal state we don't recognize
/// degrades to `Critical` (fail SAFE — pause work — never silently treated as nominal).
#[cfg(target_os = "macos")]
pub fn read_thermal_pressure() -> Option<crate::config::ThermalPressure> {
    use crate::config::ThermalPressure;
    use objc2_foundation::{NSProcessInfo, NSProcessInfoThermalState};

    // Both are safe-in-this-binding read-only calls: `processInfo()` returns the
    // shared process-info singleton and `thermalState()` is a plain property getter.
    let info = NSProcessInfo::processInfo();
    let state = info.thermalState();
    Some(match state {
        NSProcessInfoThermalState::Nominal => ThermalPressure::Nominal,
        NSProcessInfoThermalState::Fair => ThermalPressure::Fair,
        NSProcessInfoThermalState::Serious => ThermalPressure::Serious,
        _ => ThermalPressure::Critical, // Critical, or any future/unknown state — fail safe
    })
}

/// Off macOS there is no `NSProcessInfo.thermalState` (Foundation isn't linked at
/// all on the CUDA/Linux lane) — `None` is the honest "no reading available"
/// value `evaluate_thermal_throttle` already treats as "unknown, never assumed
/// nominal", matching this function's macOS counterpart's own failure semantics.
#[cfg(not(target_os = "macos"))]
pub fn read_thermal_pressure() -> Option<crate::config::ThermalPressure> {
    None
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn classify_apple_variants() {
        assert_eq!(classify("Apple M3 Max"), HardwareClass::AppleSiliconMax);
        assert_eq!(classify("Apple M2 Pro"), HardwareClass::AppleSiliconPro);
        assert_eq!(classify("Apple M2 Ultra"), HardwareClass::AppleSiliconUltra);
        assert_eq!(classify("Apple M1"), HardwareClass::AppleSiliconBase);
        assert_eq!(classify("Apple M4"), HardwareClass::AppleSiliconBase);
        assert_eq!(classify("Intel(R) Core(TM) i9-9980HK"), HardwareClass::Cpu);
    }

    #[test]
    fn production_advertisement_is_generated_and_hardware_exact() {
        let metal = generated_authorized_capabilities_for(
            "candle",
            HardwareClass::AppleSiliconPro,
            "metal",
        );
        assert_eq!(metal.len(), 7);
        assert!(metal.iter().all(|cell| cell.runtime == "candle_metal"));
        assert!(metal
            .iter()
            .any(|cell| cell.id == "candle-metal-minilm-embed"));

        // CUDA cells exist in the truth matrix but remain hardware_pending, so a
        // CUDA worker gets zero production authority instead of advertising a
        // runner merely because its code compiled locally.
        assert!(
            generated_authorized_capabilities_for("candle", HardwareClass::Nvidia80g, "cuda")
                .is_empty()
        );
        assert!(
            generated_authorized_capabilities_for("vllm", HardwareClass::Nvidia80g, "cuda")
                .is_empty()
        );
        assert!(generated_authorized_capabilities_for(
            "candle",
            HardwareClass::AppleSiliconPro,
            "cpu"
        )
        .is_empty());
    }

    #[test]
    fn classify_nvidia_tiers() {
        assert_eq!(classify_nvidia(16.0), HardwareClass::Nvidia24g); // RTX 4090 etc.
        assert_eq!(classify_nvidia(24.0), HardwareClass::Nvidia24g);
        assert_eq!(classify_nvidia(48.0), HardwareClass::Nvidia48g); // A6000/L40
        assert_eq!(classify_nvidia(80.0), HardwareClass::Nvidia80g); // A100/H100 80G
        assert_eq!(classify_nvidia(141.0), HardwareClass::Nvidia180g); // H200 / frontier
    }

    #[test]
    fn bandwidth_is_positive() {
        // A real run on any machine must report > 0 GB/s.
        assert!(measure_memory_bandwidth_gbps() > 0.0);
    }

    #[test]
    fn nvidia_vram_bandwidth_uses_real_per_sku_spec_not_host_number() {
        // The exact product strings `nvidia-smi --query-gpu=name` emits.
        // Each must resolve to its real HBM/GDDR spec — an order of magnitude
        // above any host-CPU streaming microbenchmark (~40-60 GB/s), which is the
        // whole point of the CUDA 6->6.5 fix.
        assert_eq!(
            nvidia_vram_bandwidth_gbps("NVIDIA A100-SXM4-80GB"),
            Some(2039.0)
        );
        assert_eq!(
            nvidia_vram_bandwidth_gbps("NVIDIA A100-PCIE-40GB"),
            Some(1555.0)
        );
        assert_eq!(
            nvidia_vram_bandwidth_gbps("NVIDIA H100 80GB HBM3"),
            Some(3350.0)
        );
        assert_eq!(nvidia_vram_bandwidth_gbps("NVIDIA L4"), Some(300.0));
        // Most-specific-substring wins: the 80GB SXM SKU must NOT fall through to
        // the generic "A100" 40GB family entry.
        assert!(nvidia_vram_bandwidth_gbps("NVIDIA A100-SXM4-80GB").unwrap() > 2000.0);
        // Every known datacenter card is >> any plausible host streaming number.
        for name in [
            "NVIDIA A100-SXM4-80GB",
            "NVIDIA H100 PCIe",
            "Tesla V100-SXM2-16GB",
        ] {
            assert!(
                nvidia_vram_bandwidth_gbps(name).unwrap() > 500.0,
                "{name} must resolve to a real GPU bandwidth, not a host figure"
            );
        }
        // An unknown card returns None so the caller falls back honestly, never a guess.
        assert_eq!(nvidia_vram_bandwidth_gbps("NVIDIA MADE-UP-9000"), None);
        assert_eq!(nvidia_vram_bandwidth_gbps("Apple M3 Pro"), None);
    }

    /// A cold cache (no file yet), a matching-key hit, a build/hardware-mismatch
    /// miss, and a stale (>7d) miss — the four cache states `detect_and_benchmark`
    /// actually relies on. Uses a dedicated temp path via `CX_BENCH_CACHE_PATH` so
    /// it never touches a real `~/.compute-exchange/bench_cache.json`.
    #[test]
    fn bench_cache_hit_miss_and_staleness() {
        let dir = std::env::temp_dir().join(format!("cx-bench-cache-test-{}", std::process::id()));
        let _ = std::fs::remove_dir_all(&dir);
        let path = dir.join("bench_cache.json");
        std::env::set_var("CX_BENCH_CACHE_PATH", &path);

        // 1. Nothing written yet: a miss, not a panic or a fabricated value.
        assert!(load_bench_cache("k1").is_none());

        // 2. Save, then load with the SAME key: a hit, with the exact values back.
        let sample = vec![BenchResult {
            model_id: "all-minilm-l6-v2".to_string(),
            job_type: "embed".to_string(),
            tps: 0.0,
            eps: 812.5,
            p99_ms: 12,
            thermal_ok: true,
            load_ms: 340,
        }];
        save_bench_cache("k1", 123.75, &sample);
        let (bw, benches) = load_bench_cache("k1").expect("fresh matching-key save must hit");
        assert_eq!(bw, 123.75);
        assert_eq!(benches.len(), 1);
        assert_eq!(benches[0].model_id, "all-minilm-l6-v2");

        // 3. Same file, DIFFERENT key (e.g. a new agent build_hash or different
        // hardware): must miss even though the file is fresh and well-formed.
        assert!(load_bench_cache("k2-different-build").is_none());

        // 4. Same key, but measured far enough in the past to exceed the 7-day
        // cap: must miss even though the key matches exactly.
        let stale = BenchCache {
            key: "k1".to_string(),
            measured_unix: now_unix().saturating_sub(BENCH_CACHE_MAX_AGE_SECS + 3600),
            memory_bw_gbps: 999.0,
            benchmarks: vec![],
        };
        std::fs::write(&path, serde_json::to_vec(&stale).unwrap()).unwrap();
        assert!(
            load_bench_cache("k1").is_none(),
            "a >7d-old cache must be treated as a miss"
        );

        std::env::remove_var("CX_BENCH_CACHE_PATH");
        let _ = std::fs::remove_dir_all(&dir);
    }

    #[test]
    fn used_pct_is_total_minus_available() {
        let s = MemorySnapshot {
            total_gb: 16.0,
            available_gb: 4.0,
        };
        assert!((s.used_pct() - 75.0).abs() < 0.01);
        // Degenerate total → 0%, never NaN/divide-by-zero.
        assert_eq!(
            MemorySnapshot {
                total_gb: 0.0,
                available_gb: 0.0
            }
            .used_pct(),
            0.0
        );
    }

    #[test]
    fn engine_build_hash_is_stable_and_sensitive() {
        // Stable: same inputs → same hash, every call (a class tag must not drift).
        let a = engine_build_hash("candle", "0.1.0");
        let b = engine_build_hash("candle", "0.1.0");
        assert_eq!(a, b, "build hash must be deterministic");

        // Short, hex, non-empty: a 16-char (8-byte) tag the wire carries cheaply.
        assert_eq!(a.len(), 16, "build hash is the first 8 bytes as hex");
        assert!(
            a.chars().all(|c| c.is_ascii_hexdigit()),
            "build hash must be hex"
        );

        // Sensitive: a different engine OR a different agent build → a different
        // class. A kernel/codegen change ships in a new agent build, so an
        // agent-version bump MUST move the class (this is the whole point — a
        // silent byte-shift between builds is caught by class divergence, not by a
        // false auto-dock against an old-build peer).
        assert_ne!(a, engine_build_hash("mlx", "0.1.0"), "engine changes class");
        assert_ne!(
            a,
            engine_build_hash("candle", "0.2.0"),
            "agent build changes class"
        );

        // Content/kernel identity: an output-changing engine patch (a different
        // inference-module source) MUST move the class even at the SAME engine +
        // agent_version — the moat hole the content id closes (CANDLE_EXPANSION L17).
        assert_ne!(
            engine_build_hash_inner("candle", "0.1.0", "aaaaaaaaaaaaaaaa", "spec=off"),
            engine_build_hash_inner("candle", "0.1.0", "bbbbbbbbbbbbbbbb", "spec=off"),
            "a content/kernel identity change must move the verification class"
        );
        assert_ne!(
            engine_build_hash_inner("candle", "0.1.0", "aaaaaaaaaaaaaaaa", "spec=off"),
            engine_build_hash_inner(
                "candle",
                "0.1.0",
                "aaaaaaaaaaaaaaaa",
                "spec=ngram;window=32;order=3;q4k_splitk=1"
            ),
            "runtime inference tuning must move the verification class"
        );
        // The real inference-module content id is deterministic and a 16-char hex tag.
        let cid = infer_content_id();
        assert_eq!(cid, infer_content_id(), "content id must be deterministic");
        assert_eq!(cid.len(), 16, "content id is the first 8 bytes as hex");
        assert!(
            cid.chars().all(|c| c.is_ascii_hexdigit()),
            "content id must be hex"
        );
    }

    #[test]
    fn inference_content_identity_covers_whisper_decoder_math() {
        assert!(
            INFERENCE_CONTENT_SOURCES
                .iter()
                .any(|(name, _)| *name == "whisper_decoder_kv.rs"),
            "byte-exact audio verification requires whisper_decoder_kv.rs in the worker build identity"
        );
    }

    #[test]
    fn read_memory_snapshot_is_real() {
        // A real reading on any machine: positive total, available ≤ total.
        let s = read_memory_snapshot();
        assert!(s.total_gb > 0.0, "total memory must be positive");
        assert!(
            s.available_gb >= 0.0 && s.available_gb <= s.total_gb,
            "available ({}) must be within [0, total ({})]",
            s.available_gb,
            s.total_gb
        );
    }

    /// PATCH (P-real-platform-signals, docs/internal/CREED_AND_PATH_TO_TEN.md,
    /// "Agent idle footprint & startup overhead" 7→8): a real, in-process
    /// platform-signal call — no subprocess, so this must never panic, hang, or
    /// require any process spawn permission. Not asserting a specific value (this
    /// box's real battery/thermal state at test time is whatever it is) — the
    /// proof is that the call completes and returns a value of the right shape,
    /// exactly mirroring `read_memory_snapshot_is_real`'s own "real, not
    /// fabricated" discipline.
    #[test]
    fn on_battery_reads_real_platform_state_without_a_subprocess() {
        // Must simply return without panicking; both true/false are valid depending
        // on whether this test runs on AC or battery power.
        let _ = on_battery();
    }

    #[test]
    fn read_thermal_pressure_reads_real_nsprocessinfo_without_a_subprocess() {
        // On macOS this must be Some(..) — a live machine always has SOME thermal
        // reading. Off macOS (not exercised in this CI, but documented) it is None.
        #[cfg(target_os = "macos")]
        assert!(
            read_thermal_pressure().is_some(),
            "a real macOS process always has a thermalState reading"
        );
        #[cfg(not(target_os = "macos"))]
        assert_eq!(read_thermal_pressure(), None);
    }

    /// Real proof of the Warm Model Pool 6->6.5 fix
    /// (docs/internal/CREED_AND_PATH_TO_TEN.md): `detect_and_benchmark`'s model
    /// loads must land in the SAME pool a real task reuses afterward, so the
    /// benchmark's cold load is the agent's ONLY cold load for that model — not a
    /// rehearsal that gets thrown away and re-paid on the first real dispatch.
    /// Downloads real weights (MiniLM + the 1B GGUF), so gated behind `#[ignore]`.
    /// Run with:
    ///   cargo test --release benchmark_load_stays_warm_for_real_dispatch -- --ignored --nocapture
    #[tokio::test]
    #[ignore = "downloads real MiniLM + Llama-3.2-1B weights and runs the full benchmark"]
    async fn benchmark_load_stays_warm_for_real_dispatch() {
        let pool = crate::pool::ModelPool::new();
        let before = crate::pool::loads();
        let cap = detect_and_benchmark(uuid::Uuid::nil(), "test", 0.0, "candle", &pool).await;
        assert!(
            !cap.benchmarks.is_empty(),
            "benchmark must have produced at least one real result"
        );
        let after_bench = crate::pool::loads();
        let real_loads_during_bench = after_bench - before;
        assert!(
            real_loads_during_bench >= 1,
            "benchmarking must have caused at least one real model load, got {real_loads_during_bench}"
        );

        // A "real task" touching the SAME models afterward, through the SAME pool,
        // must NOT cause any further load — this is the whole point of the fix.
        let _ = pool
            .embedder("")
            .await
            .expect("embedder must already be warm");
        let _ = pool
            .llama("")
            .await
            .expect("llama backend must already be warm");
        let after_reuse = crate::pool::loads();
        assert_eq!(
            after_reuse, after_bench,
            "reusing the benchmarked models via the same pool must cause ZERO additional loads (got {} more)",
            after_reuse - after_bench
        );
    }
}
