use std::path::{Path, PathBuf};
use std::sync::Mutex;

use serde::Serialize;
use uuid::Uuid;

use crate::config::{AgentConfig, ThrottleDecision};
use crate::types::{BenchResult, Earnings};

const SCHEMA_VERSION: u32 = 1;

const CX_PRICE_PER_1K: &[(&str, f64)] = &[
    ("all-minilm-l6-v2", 0.00100),
    ("llama-3.2-1b-instruct-q4", 0.00200),
];

const DEFAULT_PLATFORM_TAKE_PCT: f64 = 3.0;

fn estimated_sustained_watts(hw_class: crate::types::HardwareClass) -> f64 {
    use crate::types::HardwareClass::*;
    match hw_class {
        AppleSiliconBase => 20.0,
        AppleSiliconPro => 30.0,
        AppleSiliconMax => 45.0,
        AppleSiliconUltra => 65.0,
        Cpu => 25.0,
    }
}

const DEFAULT_ELECTRICITY_USD_PER_KWH: f64 = 0.15;

const HOURS_ONLINE_PER_DAY: f64 = 24.0;

pub fn projected_daily_usd(
    benchmarks: &[BenchResult],
    hw_class: crate::types::HardwareClass,
) -> Option<f64> {
    let share_rate = 1.0 - (DEFAULT_PLATFORM_TAKE_PCT.clamp(1.0, 5.0) / 100.0);
    let watts = estimated_sustained_watts(hw_class);
    let elec_usd_hr = watts / 1000.0 * DEFAULT_ELECTRICITY_USD_PER_KWH;

    let best_net_usd_hr = benchmarks
        .iter()
        .filter_map(|b| {
            let price = CX_PRICE_PER_1K
                .iter()
                .find(|(model, _)| *model == b.model_id)
                .map(|(_, p)| *p)?;
            let units_per_sec = if b.tps > 0.0 { b.tps } else { b.eps } as f64;
            if units_per_sec <= 0.0 {
                return None;
            }
            let gross_buyer_usd_hr = units_per_sec * 3600.0 / 1000.0 * price;
            let supplier_usd_hr = gross_buyer_usd_hr * share_rate;
            Some(supplier_usd_hr - elec_usd_hr)
        })
        .fold(None::<f64>, |acc, v| Some(acc.map_or(v, |a: f64| a.max(v))));

    best_net_usd_hr.map(|net_hr| (net_hr * HOURS_ONLINE_PER_DAY).max(0.0))
}

#[derive(Serialize, Clone, PartialEq)]
struct CurrentJob {
    job_id: String,
    job_type: String,
    started_at: u64,
}

struct InflightTask {
    task_id: Uuid,
    job: CurrentJob,
}

#[derive(Serialize, Clone)]
pub struct AppliedPrefs {
    pub power_only: bool,
    pub quiet_hours: Option<(u8, u8)>,
    pub min_payout_usd_per_hr: f32,
    pub memory_headroom_gb: f32,
    pub max_memory_pct: f32,
    pub max_concurrent_tasks: usize,
}

impl AppliedPrefs {
    pub fn from_config(cfg: &AgentConfig, memory_gb: f32) -> Self {
        Self {
            power_only: cfg.power_only,
            quiet_hours: cfg.quiet_hours,
            min_payout_usd_per_hr: cfg.min_payout_usd_per_hr,
            memory_headroom_gb: cfg.memory_headroom_gb,
            max_memory_pct: cfg.max_memory_pct,
            max_concurrent_tasks: cfg.concurrency(memory_gb),
        }
    }
}

#[derive(Serialize)]
struct StatusDoc<'a> {
    schema_version: u32,
    state: &'static str,
    agent_version: &'a str,
    worker_id: Option<&'a str>,
    current_job: Option<CurrentJob>,
    current_task_id: Option<String>,
    today_earnings_usd: f64,
    balance_usd: f64,
    lifetime_usd: f64,
    projected_daily_usd: Option<f64>,
    thermal_state: &'static str,
    gpu_temp_c: Option<f32>,
    cpu_pct: f32,
    model_cache_bytes: u64,
    total_memory_gb: f32,
    available_memory_gb: f32,
    reserved_headroom_gb: f32,
    effective_memory_gb: f32,
    throttled: bool,
    throttle_reason: Option<&'a str>,
    active: bool,
    eligible_now: bool,
    last_heartbeat: u64,
    last_error: Option<&'a str>,
    applied_prefs: Option<&'a AppliedPrefs>,

    payouts_configured: Option<bool>,
    payouts_connected: Option<bool>,
    payouts_enabled: Option<bool>,
    last_payout_usd: Option<f64>,
    last_payout_at: Option<u64>,
    next_payout_at: Option<u64>,
    honeypots_passed: Option<i64>,
    honeypots_failed: Option<i64>,
    verification_label: Option<&'a str>,
}

struct Inner {
    worker_id: Option<String>,
    inflight: Vec<InflightTask>,
    today_earnings_usd: f64,
    balance_usd: f64,
    lifetime_usd: f64,
    projected_daily_usd: Option<f64>,
    lifetime_baseline: Option<(u64, f64)>,
    thermal_state: &'static str,
    gpu_temp_c: Option<f32>,
    cpu_pct: f32,
    model_cache_bytes: u64,
    total_memory_gb: f32,
    available_memory_gb: f32,
    reserved_headroom_gb: f32,
    effective_memory_gb: f32,
    throttled: bool,
    throttle_reason: Option<String>,
    eligible_now: bool,
    last_heartbeat: u64,
    last_error: Option<String>,
    applied_prefs: Option<AppliedPrefs>,

    payouts_configured: Option<bool>,
    payouts_connected: Option<bool>,
    payouts_enabled: Option<bool>,
    last_payout_usd: Option<f64>,
    last_payout_at: Option<u64>,
    next_payout_at: Option<u64>,
    honeypots_passed: Option<i64>,
    honeypots_failed: Option<i64>,
    verification_label: Option<String>,
}

pub struct StatusWriter {
    path: PathBuf,
    agent_version: String,
    inner: Mutex<Inner>,
}

impl StatusWriter {
    pub fn new(
        agent_version: &str,
        worker_id: Uuid,
        benchmarks: &[BenchResult],
        hw_class: crate::types::HardwareClass,
    ) -> Self {
        Self {
            path: status_path(),
            agent_version: agent_version.to_string(),
            inner: Mutex::new(Inner {
                worker_id: Some(worker_id.to_string()),
                inflight: Vec::new(),
                today_earnings_usd: 0.0,
                balance_usd: 0.0,
                lifetime_usd: 0.0,
                projected_daily_usd: projected_daily_usd(benchmarks, hw_class),
                lifetime_baseline: None,
                thermal_state: "unknown",
                gpu_temp_c: None,
                cpu_pct: 0.0,
                model_cache_bytes: dir_size(&model_cache_dir()),
                total_memory_gb: 0.0,
                available_memory_gb: 0.0,
                reserved_headroom_gb: 0.0,
                effective_memory_gb: 0.0,
                throttled: false,
                throttle_reason: None,
                eligible_now: true,
                last_heartbeat: 0,
                last_error: None,
                applied_prefs: None,
                payouts_configured: None,
                payouts_connected: None,
                payouts_enabled: None,
                last_payout_usd: None,
                last_payout_at: None,
                next_payout_at: None,
                honeypots_passed: None,
                honeypots_failed: None,
                verification_label: None,
            }),
        }
    }

    pub fn set_applied_prefs(&self, prefs: AppliedPrefs) {
        {
            let mut i = self.inner.lock().unwrap();
            i.applied_prefs = Some(prefs);
        }
        self.write();
    }

    pub fn registered(&self) {
        self.write();
    }

    pub fn job_started(&self, task_id: Uuid, job_id: Uuid, job_type: &str, started_at: u64) {
        {
            let mut i = self.inner.lock().unwrap();
            i.inflight.push(InflightTask {
                task_id,
                job: CurrentJob {
                    job_id: job_id.to_string(),
                    job_type: job_type.to_string(),
                    started_at,
                },
            });
        }
        self.write();
    }

    pub fn job_finished(&self, task_id: Uuid, error: Option<String>) {
        {
            let mut i = self.inner.lock().unwrap();
            i.inflight.retain(|t| t.task_id != task_id);
            if error.is_some() {
                i.last_error = error;
            }
            i.model_cache_bytes = dir_size(&model_cache_dir());
        }
        self.write();
    }

    #[allow(clippy::too_many_arguments)]
    pub fn heartbeat(
        &self,
        cpu_pct: f32,
        gpu_temp_c: Option<f32>,
        thermal_pressure: Option<crate::config::ThermalPressure>,
        eligible_now: bool,
        ts: u64,
        earnings: Option<Earnings>,
        throttle: &ThrottleDecision,
        connect: Option<crate::types::ConnectStatus>,
        verification: Option<crate::types::SupplierVerification>,
    ) {
        {
            let mut i = self.inner.lock().unwrap();
            i.cpu_pct = cpu_pct;
            i.gpu_temp_c = gpu_temp_c;
            i.thermal_state = match thermal_pressure {
                Some(p) => thermal_from_pressure(p),
                None => thermal_from_temp(gpu_temp_c),
            };
            i.eligible_now = eligible_now;
            i.last_heartbeat = ts;
            apply_throttle(&mut i, throttle);
            if let Some(e) = earnings {
                i.balance_usd = e.balance_usd;
                i.lifetime_usd = e.lifetime_usd;
                let day = ts / 86_400;
                match i.lifetime_baseline {
                    Some((d, base)) if d == day => {
                        i.today_earnings_usd = (e.lifetime_usd - base).max(0.0);
                    }
                    _ => {
                        i.lifetime_baseline = Some((day, e.lifetime_usd));
                        i.today_earnings_usd = 0.0;
                    }
                }
                i.last_payout_usd = e.last_payout_usd;
                i.last_payout_at = e.last_payout_at;
                i.next_payout_at = e.next_payout_at;
            }
            if let Some(c) = connect {
                i.payouts_configured = Some(c.configured);
                i.payouts_connected = Some(c.connected);
                i.payouts_enabled = Some(c.enabled);
            }
            if let Some(v) = verification {
                i.honeypots_passed = Some(v.honeypots_passed);
                i.honeypots_failed = Some(v.honeypots_failed);
                i.verification_label = Some(v.verification_label);
            }
        }
        self.write();
    }

    pub fn set_throttle(&self, throttle: &ThrottleDecision) {
        {
            let mut i = self.inner.lock().unwrap();
            apply_throttle(&mut i, throttle);
        }
        self.write();
    }

    pub fn set_thermal_throttle(&self, decision: &crate::config::ThermalDecision) {
        {
            let mut i = self.inner.lock().unwrap();
            if let Some(p) = decision.reading {
                i.thermal_state = thermal_from_pressure(p);
            }
            if decision.throttled {
                i.throttled = true;
                i.throttle_reason = decision.reason.clone();
            }
        }
        self.write();
    }

    fn write(&self) {
        let json = {
            let i = self.inner.lock().unwrap();
            let state = if !i.inflight.is_empty() {
                "running"
            } else if i.eligible_now && !i.throttled {
                "idle"
            } else {
                "paused"
            };
            let doc = StatusDoc {
                schema_version: SCHEMA_VERSION,
                state,
                agent_version: &self.agent_version,
                worker_id: i.worker_id.as_deref(),
                current_job: i.inflight.last().map(|t| t.job.clone()),
                current_task_id: i.inflight.last().map(|t| t.task_id.to_string()),
                today_earnings_usd: i.today_earnings_usd,
                balance_usd: i.balance_usd,
                lifetime_usd: i.lifetime_usd,
                projected_daily_usd: i.projected_daily_usd,
                thermal_state: i.thermal_state,
                gpu_temp_c: i.gpu_temp_c,
                cpu_pct: i.cpu_pct,
                model_cache_bytes: i.model_cache_bytes,
                total_memory_gb: i.total_memory_gb,
                available_memory_gb: i.available_memory_gb,
                reserved_headroom_gb: i.reserved_headroom_gb,
                effective_memory_gb: i.effective_memory_gb,
                throttled: i.throttled,
                throttle_reason: i.throttle_reason.as_deref(),
                active: true,
                eligible_now: i.eligible_now,
                last_heartbeat: i.last_heartbeat,
                last_error: i.last_error.as_deref(),
                applied_prefs: i.applied_prefs.as_ref(),
                payouts_configured: i.payouts_configured,
                payouts_connected: i.payouts_connected,
                payouts_enabled: i.payouts_enabled,
                last_payout_usd: i.last_payout_usd,
                last_payout_at: i.last_payout_at,
                next_payout_at: i.next_payout_at,
                honeypots_passed: i.honeypots_passed,
                honeypots_failed: i.honeypots_failed,
                verification_label: i.verification_label.as_deref(),
            };
            serde_json::to_vec_pretty(&doc)
        };
        match json {
            Ok(bytes) => {
                if let Err(e) = write_atomic(&self.path, &bytes) {
                    tracing::warn!(error = %e, path = %self.path.display(), "failed to write status.json");
                }
            }
            Err(e) => tracing::warn!(error = %e, "failed to serialize status.json"),
        }
    }
}

fn apply_throttle(i: &mut Inner, t: &ThrottleDecision) {
    i.total_memory_gb = t.total_gb;
    i.available_memory_gb = t.available_gb;
    i.reserved_headroom_gb = t.reserved_headroom_gb;
    i.effective_memory_gb = t.effective_gb;
    i.throttled = t.throttled;
    i.throttle_reason = t.reason.clone();
}

fn status_path() -> PathBuf {
    if let Ok(p) = std::env::var("CX_STATUS_PATH") {
        if !p.is_empty() {
            return PathBuf::from(p);
        }
    }
    match std::env::var("HOME") {
        Ok(home) if !home.is_empty() => PathBuf::from(home)
            .join(".compute-exchange")
            .join("status.json"),
        _ => PathBuf::from("status.json"),
    }
}

fn model_cache_dir() -> PathBuf {
    if let Ok(d) = std::env::var("CX_MODEL_CACHE") {
        if !d.is_empty() {
            return PathBuf::from(d);
        }
    }
    if let Ok(hf) = std::env::var("HF_HOME") {
        if !hf.is_empty() {
            return PathBuf::from(hf).join("hub");
        }
    }
    match std::env::var("HOME") {
        Ok(home) if !home.is_empty() => PathBuf::from(home)
            .join(".cache")
            .join("huggingface")
            .join("hub"),
        _ => PathBuf::from(".cache/huggingface/hub"),
    }
}

fn dir_size(root: &Path) -> u64 {
    fn walk(dir: &Path, acc: &mut u64) {
        let entries = match std::fs::read_dir(dir) {
            Ok(e) => e,
            Err(_) => return,
        };
        for entry in entries.flatten() {
            let Ok(ft) = entry.file_type() else { continue };
            if ft.is_symlink() {
                continue;
            }
            if ft.is_dir() {
                walk(&entry.path(), acc);
            } else if ft.is_file() {
                if let Ok(md) = entry.metadata() {
                    *acc += md.len();
                }
            }
        }
    }
    let mut total = 0;
    walk(root, &mut total);
    total
}

fn thermal_from_temp(gpu_temp_c: Option<f32>) -> &'static str {
    match gpu_temp_c {
        None => "unknown",
        Some(t) if t < 70.0 => "nominal",
        Some(t) if t < 85.0 => "fair",
        Some(t) if t < 95.0 => "serious",
        Some(_) => "critical",
    }
}

fn thermal_from_pressure(p: crate::config::ThermalPressure) -> &'static str {
    use crate::config::ThermalPressure::*;
    match p {
        Nominal => "nominal",
        Fair => "fair",
        Serious => "serious",
        Critical => "critical",
    }
}

fn write_atomic(path: &Path, json: &[u8]) -> std::io::Result<()> {
    if let Some(parent) = path.parent() {
        if !parent.as_os_str().is_empty() {
            std::fs::create_dir_all(parent)?;
        }
    }
    let tmp = path.with_extension("json.tmp");
    std::fs::write(&tmp, json)?;
    std::fs::rename(&tmp, path)
}
