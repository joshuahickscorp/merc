use std::path::PathBuf;
use std::process::Command;
use std::time::{Instant, SystemTime, UNIX_EPOCH};

use serde::{Deserialize, Serialize};
use sysinfo::System;
use uuid::Uuid;

use crate::types::{BenchResult, HardwareClass, WorkerCapability};

const BENCH_CACHE_MAX_AGE_SECS: u64 = 7 * 24 * 60 * 60;

#[derive(Debug, Clone, Serialize, Deserialize)]
struct BenchCache {
    key: String,
    measured_unix: u64,
    memory_bw_gbps: f32,
    benchmarks: Vec<BenchResult>,
}

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

const BENCH_BYTES: usize = 256 * 1024 * 1024;
const BENCH_PASSES: usize = 5;

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

const CATALOGUE_QUANT: &str = "q4_k_m";

// The worker-advertised build hash is an admission credential during the
// private canary. Bind it to every agent source module and the locked
// dependency graph, rather than only to the inference kernel, so a protocol,
// deadline, cache-integrity, or sandbox downgrade cannot reuse an approved
// result-class identity.
const AGENT_CONTENT_SOURCES: &[(&str, &str)] = &[
    ("config.rs", include_str!("config.rs")),
    ("deadline.rs", include_str!("deadline.rs")),
    ("failure.rs", include_str!("failure.rs")),
    ("hardware.rs", include_str!("hardware.rs")),
    ("main.rs", include_str!("main.rs")),
    ("models.rs", include_str!("models.rs")),
    ("pool.rs", include_str!("pool.rs")),
    ("protocol.rs", include_str!("protocol.rs")),
    (
        "quantized_llama_batched.rs",
        include_str!("quantized_llama_batched.rs"),
    ),
    ("executor.rs", include_str!("executor.rs")),
    ("runtime_authority.rs", include_str!("runtime_authority.rs")),
    ("status.rs", include_str!("status.rs")),
    ("types.rs", include_str!("types.rs")),
    ("Cargo.lock", include_str!("../Cargo.lock")),
];

pub fn infer_content_id() -> String {
    use sha2::{Digest, Sha256};
    let mut h = Sha256::new();
    for (name, source) in AGENT_CONTENT_SOURCES {
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

fn inference_runtime_tuning_identity(engine: &str) -> String {
    if engine != "candle" {
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
    engine_build_hash_for_class(engine, agent_version, crate::models::device_label())
}

pub fn engine_build_hash_for_class(
    engine: &str,
    agent_version: &str,
    hardware_class: &str,
) -> String {
    engine_build_hash_inner(
        engine,
        agent_version,
        hardware_class,
        crate::runtime_authority::sha256(),
        &infer_content_id(),
        &inference_runtime_tuning_identity(engine),
    )
}

fn engine_build_hash_inner(
    engine: &str,
    agent_version: &str,
    hardware_class: &str,
    runtime_authority_sha256: &str,
    infer_content_id: &str,
    runtime_tuning_identity: &str,
) -> String {
    use sha2::{Digest, Sha256};
    let mut h = Sha256::new();
    for field in [
        engine,
        agent_version,
        hardware_class,
        runtime_authority_sha256,
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

pub fn measure_memory_bandwidth_gbps() -> f32 {
    let len = BENCH_BYTES / std::mem::size_of::<u64>();
    let mut buf: Vec<u64> = (0..len as u64).collect();
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
            let x = v.wrapping_mul(6364136223846793005).wrapping_add(salt);
            acc = acc.wrapping_add(x);
            *v = x;
        }
        let elapsed = start.elapsed().as_secs_f64();
        std::hint::black_box(acc);
        if elapsed > 0.0 {
            let bytes = (BENCH_BYTES as f64) * 2.0;
            let gbps = (bytes / 1e9) / elapsed;
            best_gbps = best_gbps.max(gbps as f32);
        }
    }
    best_gbps
}

#[derive(Debug, Clone, Copy, PartialEq)]
pub struct MemorySnapshot {
    pub total_gb: f32,
    pub available_gb: f32,
}

impl MemorySnapshot {
    pub fn used_pct(&self) -> f32 {
        if self.total_gb <= 0.0 {
            return 0.0;
        }
        (((self.total_gb - self.available_gb) / self.total_gb) * 100.0).clamp(0.0, 100.0)
    }
}

pub(crate) fn resolved_available_memory(total: u64, available: u64, used: u64) -> u64 {
    if available == 0 && used > 0 && total > used {
        total - used
    } else {
        available
    }
}

pub fn read_memory_snapshot() -> MemorySnapshot {
    let mut sys = System::new();
    sys.refresh_memory();
    let total = sys.total_memory();
    let available = resolved_available_memory(total, sys.available_memory(), sys.used_memory());
    MemorySnapshot {
        total_gb: (total as f64 / 1e9) as f32,
        available_gb: (available as f64 / 1e9) as f32,
    }
}

pub async fn detect_and_benchmark(
    supplier_id: Uuid,
    agent_version: &str,
    min_payout_usd_hr: f32,
    engine: &str,
    pool: &crate::pool::ModelPool,
) -> WorkerCapability {
    let mut sys = System::new();
    sys.refresh_memory();

    let mem_bytes = sysctl("hw.memsize")
        .and_then(|s| s.parse::<u64>().ok())
        .unwrap_or_else(|| sys.total_memory());
    let host_mem_gb = (mem_bytes as f64 / 1e9) as f32;

    let brand = sysctl("machdep.cpu.brand_string").unwrap_or_else(|| "unknown".to_string());
    let hw_class = classify(&brand);
    if hw_class == HardwareClass::Cpu {
        tracing::warn!(cpu = %brand, "could not map to an Apple Silicon class; CPU is for tests only");
    } else {
        tracing::info!(cpu = %brand, ?hw_class, memory_gb = host_mem_gb, "detected hardware");
    }
    let memory_gb = host_mem_gb;

    let build_hash = engine_build_hash_for_class(engine, agent_version, hw_class.as_wire_str());
    let cache_key = bench_cache_key(agent_version, &build_hash, &brand, host_mem_gb);

    let (memory_bw_gbps, benchmarks) = match load_bench_cache(&cache_key) {
        Some(cached) => cached,
        None => {
            tracing::info!("measuring unified-memory bandwidth");
            let memory_bw_gbps = measure_memory_bandwidth_gbps();
            tracing::info!(memory_bw_gbps, "resolved advertised memory bandwidth");

            tracing::info!(
                device = crate::models::device_label(),
                "running retained model benchmarks (embed and batch_infer)"
            );
            let benchmarks = crate::executor::run_benchmarks(pool, memory_gb).await;
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
    let authorized = generated_authorized_capabilities(engine, hw_class);
    let supported_jobs: Vec<String> = authorized
        .iter()
        .map(|cell| cell.job.as_str())
        .collect::<std::collections::BTreeSet<_>>()
        .into_iter()
        .map(str::to_string)
        .collect();
    let supported_models: Vec<String> = authorized
        .iter()
        .map(|cell| cell.model.as_str())
        .collect::<std::collections::BTreeSet<_>>()
        .into_iter()
        .map(str::to_string)
        .collect();
    let benchmarks: Vec<BenchResult> = benchmarks
        .into_iter()
        .filter(|bench| {
            authorized
                .iter()
                .any(|cell| cell.job == bench.job_type && cell.model == bench.model_id)
        })
        .collect();

    WorkerCapability {
        worker_id: Uuid::new_v4(),
        supplier_id,
        hw_class,
        engine: engine.to_string(),
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
) -> Vec<&'static crate::runtime_authority::RuntimeCapability> {
    generated_authorized_capabilities_for(engine, hw_class, crate::models::device_label())
}

fn generated_authorized_capabilities_for(
    engine: &str,
    hw_class: HardwareClass,
    device: &str,
) -> Vec<&'static crate::runtime_authority::RuntimeCapability> {
    let hw_tag = hw_class.as_wire_str();
    crate::runtime_authority::capabilities()
        .iter()
        .filter(|cell| {
            cell.engine == engine
                && cell.device == device
                && cell.hardware_classes.iter().any(|class| class == hw_tag)
        })
        .collect()
}

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

#[cfg(not(target_os = "macos"))]
pub fn on_battery() -> bool {
    false
}

#[cfg(target_os = "macos")]
pub fn read_thermal_pressure() -> Option<crate::config::ThermalPressure> {
    use crate::config::ThermalPressure;
    use objc2_foundation::{NSProcessInfo, NSProcessInfoThermalState};

    let info = NSProcessInfo::processInfo();
    let state = info.thermalState();
    Some(match state {
        NSProcessInfoThermalState::Nominal => ThermalPressure::Nominal,
        NSProcessInfoThermalState::Fair => ThermalPressure::Fair,
        NSProcessInfoThermalState::Serious => ThermalPressure::Serious,
        _ => ThermalPressure::Critical, // Critical, or any future/unknown state  -  fail safe
    })
}

#[cfg(not(target_os = "macos"))]
pub fn read_thermal_pressure() -> Option<crate::config::ThermalPressure> {
    None
}
