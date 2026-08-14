use std::path::PathBuf;
use std::process::Command;
use std::sync::OnceLock;
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
    if let Ok(p) = std::env::var("MERC_BENCH_CACHE_PATH") {
        if !p.is_empty() {
            return PathBuf::from(p);
        }
    }
    match std::env::var("HOME") {
        Ok(home) if !home.is_empty() => crate::config::agent_home_dir().join("bench_cache.json"),
        _ => PathBuf::from("bench_cache.json"),
    }
}

fn bench_cache_key(agent_version: &str, build_hash: &str, hardware_identity: &str) -> String {
    format!("{agent_version}|{build_hash}|{hardware_identity}")
}

fn now_unix() -> u64 {
    SystemTime::now()
        .duration_since(UNIX_EPOCH)
        .map(|d| d.as_secs())
        .unwrap_or(0)
}

fn load_bench_cache(key: &str) -> Option<(f32, Vec<BenchResult>, u64)> {
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
    Some((cache.memory_bw_gbps, cache.benchmarks, cache.measured_unix))
}

fn save_bench_cache(
    key: &str,
    measured_unix: u64,
    memory_bw_gbps: f32,
    benchmarks: &[BenchResult],
) {
    let path = bench_cache_path();
    if let Some(parent) = path.parent() {
        if let Err(e) = std::fs::create_dir_all(parent) {
            tracing::warn!(error = %e, path = %parent.display(), "bench cache: failed to create directory; skipping write");
            return;
        }
    }
    let cache = BenchCache {
        key: key.to_string(),
        measured_unix,
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

/// Current meaning of the 16-lowerhex engine build credential. Keep this on
/// both the benchmark and worker wire: equality of an unversioned short hash
/// cannot prove that both sides used the executable-bound algorithm.
pub const ENGINE_BUILD_IDENTITY_POLICY: &str = "merc_agent_running_executable_sha256_v1";

// The worker-advertised build hash is an admission credential during the
// private canary. Bind it to every agent source module and the locked
// dependency graph, rather than only to the inference kernel, so a protocol,
// deadline, cache-integrity, or sandbox downgrade cannot reuse an approved
// result-class identity.
const AGENT_CONTENT_SOURCES: &[(&str, &str)] = &[
    ("config.rs", include_str!("config.rs")),
    ("deadline.rs", include_str!("deadline.rs")),
    ("enrollment.rs", include_str!("enrollment.rs")),
    ("executor.rs", include_str!("executor.rs")),
    ("fabric.rs", include_str!("fabric.rs")),
    ("failure.rs", include_str!("failure.rs")),
    ("hardware.rs", include_str!("hardware.rs")),
    ("inference.rs", include_str!("inference.rs")),
    ("main.rs", include_str!("main.rs")),
    ("media.rs", include_str!("media.rs")),
    ("models.rs", include_str!("models.rs")),
    ("openai_serve.rs", include_str!("openai_serve.rs")),
    ("pool.rs", include_str!("pool.rs")),
    ("protocol.rs", include_str!("protocol.rs")),
    (
        "quantized_llama_batched.rs",
        include_str!("quantized_llama_batched.rs"),
    ),
    ("render.rs", include_str!("render.rs")),
    ("runtime_authority.rs", include_str!("runtime_authority.rs")),
    ("runtime_driver.rs", include_str!("runtime_driver.rs")),
    ("sandbox_egress.rs", include_str!("sandbox_egress.rs")),
    ("status.rs", include_str!("status.rs")),
    ("tls.rs", include_str!("tls.rs")),
    ("token_cache.rs", include_str!("token_cache.rs")),
    ("types.rs", include_str!("types.rs")),
    ("vllm.rs", include_str!("vllm.rs")),
    ("Cargo.lock", include_str!("../Cargo.lock")),
    ("Cargo.toml", include_str!("../Cargo.toml")),
];

fn agent_content_id(sources: &[(&str, &str)]) -> String {
    use sha2::{Digest, Sha256};
    let mut h = Sha256::new();
    for (name, source) in sources {
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

pub fn infer_content_id() -> String {
    agent_content_id(AGENT_CONTENT_SOURCES)
}

// Hash the executable actually running, not just the repository inputs that
// were expected to produce it. This makes Cargo feature selection, rustc and
// linker versions, target, profile, RUSTFLAGS/codegen, and any other material
// build variation weight-bearing in the benchmark/worker build credential.
// Failure is fatal: advertising a source-only fallback would allow a different
// execution binary to inherit an approved active-hour floor.
fn execution_binary_sha256() -> &'static str {
    static DIGEST: OnceLock<String> = OnceLock::new();
    DIGEST
        .get_or_init(|| {
            use sha2::{Digest, Sha256};

            let path = std::env::current_exe().unwrap_or_else(|err| {
                panic!("cannot resolve running executable for build identity: {err}")
            });
            let bytes = std::fs::read(&path).unwrap_or_else(|err| {
                panic!(
                    "cannot read running executable {} for build identity: {err}",
                    path.display()
                )
            });
            let digest = Sha256::digest(bytes);
            digest.iter().map(|byte| format!("{byte:02x}")).collect()
        })
        .as_str()
}

fn normalized_identity_component(value: &str) -> String {
    value.split_whitespace().collect::<Vec<_>>().join(" ")
}

fn gpu_core_count() -> Option<u32> {
    let output = Command::new("ioreg")
        .args(["-r", "-c", "AGXAccelerator", "-l"])
        .output()
        .ok()
        .filter(|value| value.status.success())?;
    String::from_utf8_lossy(&output.stdout)
        .lines()
        .find_map(|line| {
            // ioreg -l tree lines look like: `  |   "gpu-core-count" = 60`
            // The pipe is structural, not part of the key. Match the quoted
            // property name anywhere on the left of '=' rather than requiring
            // the whole left side to equal the quoted key after trim.
            let (key, value) = line.split_once('=')?;
            let key = key.trim().trim_start_matches('|').trim();
            if key != "\"gpu-core-count\"" {
                return None;
            }
            value.trim().parse::<u32>().ok().filter(|count| *count > 0)
        })
}

fn hardware_identity_from_parts(
    brand: &str,
    model: &str,
    memory_bytes: u64,
    cpu_cores: u32,
    gpu_cores: u32,
) -> String {
    format!(
        "apple_silicon_v1|brand={}|model={}|memory_bytes={memory_bytes}|cpu_cores={cpu_cores}|gpu_cores={gpu_cores}",
        normalized_identity_component(brand),
        normalized_identity_component(model),
    )
}

/// Exact performance-relevant Apple hardware configuration shared by worker
/// registration and benchmark receipts. Missing topology is fatal for Apple
/// Silicon: falling back to a marketing name would allow a lower-core or
/// different-memory SKU to borrow another device's measured floor.
pub fn detected_hardware_identity() -> String {
    static IDENTITY: OnceLock<String> = OnceLock::new();
    IDENTITY
        .get_or_init(|| {
            let brand = sysctl("machdep.cpu.brand_string")
                .unwrap_or_else(|| "unknown non-Apple CPU".to_string());
            if classify(&brand) == HardwareClass::Cpu {
                return format!(
                    "non_apple_v1|brand={}|arch={}",
                    normalized_identity_component(&brand),
                    std::env::consts::ARCH
                );
            }
            let required = |key: &str| {
                sysctl(key).unwrap_or_else(|| {
                    panic!("cannot read {key} for exact Apple hardware identity")
                })
            };
            let model = required("hw.model");
            let memory_bytes = required("hw.memsize")
                .parse::<u64>()
                .unwrap_or_else(|err| panic!("invalid hw.memsize for hardware identity: {err}"));
            let cpu_cores = required("hw.ncpu")
                .parse::<u32>()
                .unwrap_or_else(|err| panic!("invalid hw.ncpu for hardware identity: {err}"));
            let gpu_cores = gpu_core_count().unwrap_or_else(|| {
                panic!("cannot read gpu-core-count for exact Apple hardware identity")
            });
            hardware_identity_from_parts(&brand, &model, memory_bytes, cpu_cores, gpu_cores)
        })
        .clone()
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
    let spec_mode = value("MERC_SPEC_DECODE", "off");
    let (spec_window, spec_order) = if matches!(spec_mode.trim(), "1" | "on" | "ngram") {
        (
            value("MERC_SPEC_DECODE_WINDOW", "32"),
            value("MERC_SPEC_DECODE_NGRAM_ORDER", "3"),
        )
    } else {
        ("inactive".to_string(), "inactive".to_string())
    };
    format!(
        "spec={spec_mode};window={spec_window};order={spec_order};q4k_splitk={};q4k_skinny_m={};dequant_f16={};fast_math={};metal_compute_per_buffer={};metal_command_pool_size={}",
        value("MERC_Q4K_SPLITK", "0"),
        value("MERC_Q4K_SKINNY_M", "0"),
        value("CANDLE_DEQUANTIZE_ALL_F16", "0"),
        value("CANDLE_METAL_ENABLE_FAST_MATH", "default"),
        value("CANDLE_METAL_COMPUTE_PER_BUFFER", "default"),
        value("CANDLE_METAL_COMMAND_POOL_SIZE", "default"),
    )
}

/// The hardware class this host advertises at registration, as it appears on the
/// wire. The verification class a worker declares is
/// engine_build_hash_for_class(engine, version, THIS) -- not device_label() --
/// so anything that must reproduce a worker's class byte-for-byte (the
/// batch_infer honeypot measurement) has to hash the same string.
pub fn detected_hw_class_wire() -> &'static str {
    let brand = sysctl("machdep.cpu.brand_string").unwrap_or_else(|| "unknown".to_string());
    classify(&brand).as_wire_str()
}

pub fn engine_build_hash(engine: &str, agent_version: &str) -> String {
    engine_build_hash_for_class(engine, agent_version, detected_hw_class_wire())
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
        execution_binary_sha256(),
        &inference_runtime_tuning_identity(engine),
    )
}

fn engine_build_hash_inner(
    engine: &str,
    agent_version: &str,
    hardware_class: &str,
    runtime_authority_sha256: &str,
    infer_content_id: &str,
    execution_binary_sha256: &str,
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
        ENGINE_BUILD_IDENTITY_POLICY,
        infer_content_id,
        execution_binary_sha256,
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
    let hardware_identity = detected_hardware_identity();
    let cache_key = bench_cache_key(agent_version, &build_hash, &hardware_identity);

    let (memory_bw_gbps, mut benchmarks, measured_unix) = match load_bench_cache(&cache_key) {
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
            let measured_unix = now_unix();
            save_bench_cache(&cache_key, measured_unix, memory_bw_gbps, &benchmarks);
            (memory_bw_gbps, benchmarks, measured_unix)
        }
    };
    for benchmark in &mut benchmarks {
        benchmark.measured_unix = measured_unix;
    }
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
        agent_session_id: Uuid::new_v4(),
        hw_class,
        engine: engine.to_string(),
        build_hash,
        build_identity_policy: ENGINE_BUILD_IDENTITY_POLICY.to_string(),
        hardware_identity,
        memory_gb,
        memory_bw_gbps,
        gpu_count: 1,
        memory_gb_per_gpu: memory_gb,
        interconnect: String::new(),
        supported_jobs,
        supported_models,
        benchmarks,
        agent_version: agent_version.to_string(),
        os_version: os_version(),
        min_payout_usd_hr,
        // Filled by the run loop after sandbox re-exec; detect_and_benchmark
        // itself does not know the process containment state.
        sandboxed: false,
        unsandboxed_opt_in: false,
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

#[cfg(test)]
mod tests {
    use super::{
        agent_content_id, engine_build_hash_inner, hardware_identity_from_parts,
        AGENT_CONTENT_SOURCES,
    };

    #[test]
    fn execution_module_change_changes_agent_content_identity() {
        let before = [
            ("inference.rs", "fn execute() { old_kernel(); }"),
            ("runtime_driver.rs", "fn dispatch() { execute(); }"),
        ];
        let after = [
            ("inference.rs", "fn execute() { substituted_kernel(); }"),
            ("runtime_driver.rs", "fn dispatch() { execute(); }"),
        ];
        assert_ne!(agent_content_id(&before), agent_content_id(&after));
    }

    #[test]
    fn agent_content_identity_covers_every_compiled_source_module() {
        let mut compiled: Vec<String> =
            std::fs::read_dir(std::path::Path::new(env!("CARGO_MANIFEST_DIR")).join("src"))
                .expect("read agent source directory")
                .filter_map(|entry| entry.ok())
                .filter_map(|entry| {
                    let path = entry.path();
                    (path.extension().and_then(|ext| ext.to_str()) == Some("rs"))
                        .then(|| path.file_name()?.to_str().map(str::to_owned))
                        .flatten()
                })
                .collect();
        compiled.sort();

        let mut covered: Vec<String> = AGENT_CONTENT_SOURCES
            .iter()
            .filter(|(name, _)| name.ends_with(".rs"))
            .map(|(name, _)| (*name).to_owned())
            .collect();
        covered.sort();
        assert_eq!(
            covered, compiled,
            "agent build identity omitted a source module"
        );
        assert!(
            AGENT_CONTENT_SOURCES
                .iter()
                .any(|(name, _)| *name == "Cargo.lock"),
            "agent build identity omitted the locked dependency graph"
        );
        assert!(
            AGENT_CONTENT_SOURCES
                .iter()
                .any(|(name, _)| *name == "Cargo.toml"),
            "agent build identity omitted features/profile/build configuration"
        );
    }

    #[test]
    fn execution_binary_change_changes_engine_build_identity() {
        let build = |binary_digest: &str| {
            engine_build_hash_inner(
                "candle",
                "test-agent",
                "apple_silicon_ultra",
                "runtime-authority",
                "source-content",
                binary_digest,
                "runtime-tuning",
            )
        };
        assert_ne!(build(&"a".repeat(64)), build(&"b".repeat(64)));
    }

    #[test]
    fn exact_hardware_identity_changes_with_performance_relevant_configuration() {
        let base = hardware_identity_from_parts("Apple M3 Ultra", "Mac15,14", 96, 28, 60);
        for changed in [
            hardware_identity_from_parts("Apple M1 Ultra", "Mac15,14", 96, 28, 60),
            hardware_identity_from_parts("Apple M3 Ultra", "Mac16,12", 96, 28, 60),
            hardware_identity_from_parts("Apple M3 Ultra", "Mac15,14", 192, 28, 60),
            hardware_identity_from_parts("Apple M3 Ultra", "Mac15,14", 96, 24, 60),
            hardware_identity_from_parts("Apple M3 Ultra", "Mac15,14", 96, 28, 76),
        ] {
            assert_ne!(base, changed);
        }
    }
}
