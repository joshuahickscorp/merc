use std::path::{Path, PathBuf};

use anyhow::{Context, Result};
use serde::{Deserialize, Serialize};
use uuid::Uuid;

use crate::hardware::MemorySnapshot;

/// Canonical per-user agent state directory (bench cache, status, install config).
pub const AGENT_HOME_DIRNAME: &str = ".merc";
/// Pre-rebrand state directory. Migrated into [`AGENT_HOME_DIRNAME`] when present.
pub const LEGACY_AGENT_HOME_DIRNAME: &str = ".compute-exchange";

/// Resolve `$HOME/.merc`, migrating `$HOME/.compute-exchange` when the new path
/// is absent so an upgrade does not drop the bench cache or identity files.
pub fn agent_home_dir() -> PathBuf {
    match std::env::var("HOME") {
        Ok(home) if !home.is_empty() => agent_home_dir_for(Path::new(&home)),
        _ => PathBuf::from(AGENT_HOME_DIRNAME),
    }
}

/// Resolve the agent home under an explicit home directory (also used by tests).
pub fn agent_home_dir_for(home: &Path) -> PathBuf {
    let modern = home.join(AGENT_HOME_DIRNAME);
    let legacy = home.join(LEGACY_AGENT_HOME_DIRNAME);
    if modern.exists() {
        return modern;
    }
    if legacy.exists() {
        match std::fs::rename(&legacy, &modern) {
            Ok(()) => return modern,
            Err(_) => {
                // Cross-device or permission failure: keep reading the legacy tree
                // rather than pretending state is gone.
                return legacy;
            }
        }
    }
    modern
}

fn default_memory_headroom_gb() -> f32 {
    8.0
}

fn default_max_memory_pct() -> f32 {
    85.0
}

fn default_checkpoint_secs() -> u64 {
    30
}

fn default_inference_backend() -> String {
    "candle".to_string()
}

fn default_openai_base_url() -> String {
    "http://127.0.0.1:8099/v1".to_string()
}

fn default_embed_runtime() -> String {
    "candle_metal".to_string()
}

fn default_llama_embed_url() -> String {
    "http://127.0.0.1:8188".to_string()
}

fn default_allow_model_downloads() -> bool {
    true
}

fn default_max_model_cache_gb() -> f32 {
    4.0
}

fn default_thermal_limit() -> ThermalPressure {
    ThermalPressure::Serious
}

fn default_openai_model() -> String {
    "cx-chat-1b".to_string()
}

#[derive(Debug, Clone, Copy, PartialEq, Eq, Deserialize, Serialize)]
#[serde(rename_all = "lowercase")]
pub enum ThermalPressure {
    Nominal,
    Fair,
    Serious,
    Critical,
}

impl ThermalPressure {
    fn rank(self) -> u8 {
        match self {
            ThermalPressure::Nominal => 0,
            ThermalPressure::Fair => 1,
            ThermalPressure::Serious => 2,
            ThermalPressure::Critical => 3,
        }
    }
}

impl ThermalPressure {
    pub fn as_str(&self) -> &'static str {
        match self {
            ThermalPressure::Nominal => "nominal",
            ThermalPressure::Fair => "fair",
            ThermalPressure::Serious => "serious",
            ThermalPressure::Critical => "critical",
        }
    }
}

#[derive(Debug, Clone, Deserialize)]
#[allow(dead_code)]
pub struct AgentConfig {
    pub control_url: String,
    pub worker_token: String,
    pub supplier_id: Uuid,
    #[serde(default)]
    pub paused: bool,
    #[serde(default)]
    pub allowed_weekdays: Option<Vec<u8>>,
    #[serde(default)]
    pub allowed_workload_classes: Option<Vec<String>>,
    #[serde(default = "default_allow_model_downloads")]
    pub allow_model_downloads: bool,
    #[serde(default = "default_max_model_cache_gb")]
    pub max_model_cache_gb: f32,
    #[serde(default)]
    pub max_bandwidth_mbps: f32,
    #[serde(default)]
    pub max_cpu_pct: f32,
    #[serde(default = "default_thermal_limit")]
    pub thermal_limit: ThermalPressure,
    pub quiet_hours: Option<(u8, u8)>,
    pub power_only: bool,
    pub min_payout_usd_per_hr: f32,
    #[serde(default = "default_memory_headroom_gb")]
    pub memory_headroom_gb: f32,
    #[serde(default = "default_max_memory_pct")]
    pub max_memory_pct: f32,
    pub data_dir: PathBuf,
    #[serde(default)]
    pub max_concurrent_tasks: Option<usize>,
    #[serde(default = "default_checkpoint_secs")]
    pub checkpoint_secs: u64,
    /// `candle` (default, in-process) or `openai_http` (OpenAI-compatible engine).
    #[serde(default = "default_inference_backend")]
    pub inference_backend: String,
    /// Base URL including `/v1` for the OpenAI-compatible engine (openai_http only).
    #[serde(default = "default_openai_base_url")]
    pub openai_base_url: String,
    /// Model id/alias the engine advertises (openai_http only).
    #[serde(default = "default_openai_model")]
    pub openai_model: String,
    /// Optional bearer token for the engine (openai_http only).
    #[serde(default)]
    pub openai_api_key: Option<String>,
    /// Which governed runtime profile drives the embed cell: `candle_metal`
    /// (default, in-process safetensors) or `llama_cpp_metal` (GGUF via a
    /// llama-server this operator runs).
    ///
    /// Separate from `inference_backend` on purpose. That knob chooses the
    /// batch_infer engine; this one chooses the embed runtime, and the two are
    /// genuinely independent — llama.cpp is disqualified from byte_exact
    /// batch_infer on Metal and qualified for the cosine-verified embed cell, so
    /// collapsing them into one setting would force an operator to accept the
    /// engine's worst cell to get its best one.
    #[serde(default = "default_embed_runtime")]
    pub embed_runtime: String,
    /// Base URL of the llama-server serving embeddings (`llama_cpp_metal` only).
    #[serde(default = "default_llama_embed_url")]
    pub llama_embed_base_url: String,
    #[serde(skip, default)]
    pub thermal_pressure: Option<ThermalPressure>,
    #[serde(skip, default)]
    pub prefs_path: Option<PathBuf>,
}

#[derive(Debug, Clone, Default, Deserialize, PartialEq)]
pub struct OperatorPrefs {
    pub paused: Option<bool>,
    pub allowed_weekdays: Option<Vec<u8>>,
    pub allowed_workload_classes: Option<Vec<String>>,
    pub allow_model_downloads: Option<bool>,
    pub max_model_cache_gb: Option<f32>,
    pub max_bandwidth_mbps: Option<f32>,
    pub max_cpu_pct: Option<f32>,
    pub thermal_limit: Option<ThermalPressure>,
    pub power_only: Option<bool>,
    pub min_payout_usd_per_hr: Option<f32>,
    pub memory_headroom_gb: Option<f32>,
    pub max_memory_pct: Option<f32>,
    pub quiet_hours: Option<(u8, u8)>,
    pub max_concurrent_tasks: Option<usize>,
    pub thermal_pressure: Option<ThermalPressure>,
}

impl OperatorPrefs {
    pub fn load(path: &Path) -> Result<Self> {
        let text = std::fs::read_to_string(path)
            .with_context(|| format!("reading prefs file {}", path.display()))?;
        toml::from_str(&text).with_context(|| format!("parsing prefs TOML {}", path.display()))
    }

    // The vLLM adapter has its own immutable runtime configuration, but it
    // shares this live, supplier-owned policy file with the general-purpose
    // agent. Keep validation here so a malformed preference file cannot be
    // interpreted differently by the two execution paths.
    pub fn validate(&self) -> Result<()> {
        if let Some(days) = &self.allowed_weekdays {
            if days.iter().any(|day| *day > 6) {
                anyhow::bail!("allowed_weekdays values must be 0..6 (Sunday..Saturday)");
            }
        }
        if let Some(workloads) = &self.allowed_workload_classes {
            if workloads.iter().any(|workload| {
                !matches!(
                    workload.as_str(),
                    "embed" | "batch_infer" | "media_transcode" | "media_rendering"
                )
            }) {
                anyhow::bail!("allowed_workload_classes contains an unknown workload");
            }
        }
        if let Some((start, end)) = self.quiet_hours {
            if start > 23 || end > 23 {
                anyhow::bail!("quiet_hours values must be 0..23");
            }
        }
        for (name, value) in [
            ("min_payout_usd_per_hr", self.min_payout_usd_per_hr),
            ("memory_headroom_gb", self.memory_headroom_gb),
            ("max_model_cache_gb", self.max_model_cache_gb),
            ("max_bandwidth_mbps", self.max_bandwidth_mbps),
        ] {
            if let Some(value) = value {
                if !value.is_finite() || value < 0.0 {
                    anyhow::bail!("{name} must be finite and non-negative");
                }
            }
        }
        for (name, value) in [
            ("max_memory_pct", self.max_memory_pct),
            ("max_cpu_pct", self.max_cpu_pct),
        ] {
            if let Some(value) = value {
                if !value.is_finite() || !(0.0..=100.0).contains(&value) {
                    anyhow::bail!("{name} must be between 0 and 100");
                }
            }
        }
        Ok(())
    }
}

#[derive(Debug, Clone, PartialEq)]
pub struct ThrottleDecision {
    pub throttled: bool,
    pub reason: Option<String>,
    pub total_gb: f32,
    pub available_gb: f32,
    pub reserved_headroom_gb: f32,
    pub effective_gb: f32,
    pub used_pct: f32,
}

#[derive(Debug, Clone, PartialEq, Eq)]
pub struct ThermalDecision {
    pub throttled: bool,
    pub reason: Option<String>,
    pub reading: Option<ThermalPressure>,
}

impl AgentConfig {
    pub fn evaluate_thermal_throttle(&self) -> ThermalDecision {
        match self.thermal_pressure {
            Some(reading) if reading.rank() >= self.thermal_limit.rank() => ThermalDecision {
                throttled: true,
                reason: Some(format!(
                    "thermal pressure: {} reached supplier limit {}",
                    reading.as_str(),
                    self.thermal_limit.as_str()
                )),
                reading: Some(reading),
            },
            other => ThermalDecision {
                throttled: false,
                reason: None,
                reading: other,
            },
        }
    }

    pub fn evaluate_memory_throttle(
        &self,
        snap: &MemorySnapshot,
        next_task_gb: Option<f32>,
    ) -> ThrottleDecision {
        let headroom = self.memory_headroom_gb.max(0.0);
        let effective = (snap.available_gb - headroom).max(0.0);
        let used_pct = snap.used_pct();

        let reason = if self.max_memory_pct > 0.0 && used_pct >= self.max_memory_pct {
            Some(format!(
                "memory pressure: {used_pct:.0}% used >= {:.0}% ceiling",
                self.max_memory_pct
            ))
        } else if headroom > 0.0 && snap.available_gb <= headroom {
            Some(format!(
                "reserved headroom: {:.1} GB available <= {headroom:.1} GB headroom",
                snap.available_gb
            ))
        } else if next_task_gb.is_some_and(|need| need > effective) {
            let need = next_task_gb.unwrap();
            Some(format!(
                "next task needs {need:.1} GB but only {effective:.1} GB allocatable after headroom"
            ))
        } else {
            None
        };

        ThrottleDecision {
            throttled: reason.is_some(),
            reason,
            total_gb: snap.total_gb,
            available_gb: snap.available_gb,
            reserved_headroom_gb: headroom,
            effective_gb: effective,
            used_pct,
        }
    }

    pub fn load(path: &Path) -> Result<Self> {
        let text = std::fs::read_to_string(path)
            .with_context(|| format!("reading config file {}", path.display()))?;
        let mut cfg: AgentConfig =
            toml::from_str(&text).with_context(|| format!("parsing TOML {}", path.display()))?;

        if let Ok(url) = std::env::var("MERC_CONTROL_URL") {
            if !url.is_empty() {
                cfg.control_url = url;
            }
        }
        if let Ok(token) = std::env::var("MERC_WORKER_TOKEN") {
            if !token.is_empty() {
                cfg.worker_token = token;
            }
        }
        if let Ok(secs) = std::env::var("MERC_CHECKPOINT_SECS") {
            if !secs.is_empty() {
                cfg.checkpoint_secs = secs
                    .parse()
                    .with_context(|| format!("parsing MERC_CHECKPOINT_SECS {secs:?}"))?;
            }
        }
        if let Ok(backend) = std::env::var("MERC_INFERENCE_BACKEND") {
            if !backend.is_empty() {
                cfg.inference_backend = backend;
            }
        }
        if let Ok(url) = std::env::var("MERC_OPENAI_BASE_URL") {
            if !url.is_empty() {
                cfg.openai_base_url = url;
            }
        }
        if let Ok(model) = std::env::var("MERC_OPENAI_MODEL") {
            if !model.is_empty() {
                cfg.openai_model = model;
            }
        }
        if let Ok(key) = std::env::var("MERC_OPENAI_API_KEY") {
            if !key.is_empty() {
                cfg.openai_api_key = Some(key);
            }
        }

        // Reject unknown backends at config load so a typo cannot silently fall back.
        crate::inference::BackendKind::parse(&cfg.inference_backend).map_err(anyhow::Error::msg)?;

        if let Some(pp) = Self::resolve_prefs_path(path) {
            cfg.apply_prefs(&OperatorPrefs::load(&pp)?);
        }
        cfg.prefs_path = Self::resolve_prefs_path(path);
        cfg.validate_operator_policy()?;

        Ok(cfg)
    }

    pub fn inference_backend_kind(&self) -> Result<crate::inference::BackendKind, String> {
        crate::inference::BackendKind::parse(&self.inference_backend)
    }

    fn resolve_prefs_path(config_path: &Path) -> Option<PathBuf> {
        match std::env::var("MERC_AGENT_PREFS") {
            Ok(p) if !p.is_empty() => Some(PathBuf::from(p)),
            _ => {
                let sidecar = config_path.with_file_name("agent.prefs.toml");
                sidecar.exists().then_some(sidecar)
            }
        }
    }

    pub fn refresh_thermal_pressure(&mut self) {
        self.thermal_pressure = crate::hardware::read_thermal_pressure().or_else(|| {
            self.prefs_path
                .as_ref()
                .and_then(|p| OperatorPrefs::load(p).ok())
                .and_then(|p| p.thermal_pressure)
        });
    }

    /// Reload the local UI's policy before a claim. A malformed preferences
    /// file is an error so the caller idles instead of silently running with an
    /// older, more permissive policy.
    pub fn refresh_operator_prefs(&mut self) -> Result<()> {
        if let Some(path) = self.prefs_path.clone() {
            self.apply_prefs(&OperatorPrefs::load(&path)?);
        }
        self.validate_operator_policy()
    }

    fn validate_operator_policy(&self) -> Result<()> {
        if let Some(days) = &self.allowed_weekdays {
            if days.iter().any(|day| *day > 6) {
                anyhow::bail!("allowed_weekdays values must be 0..6 (Sunday..Saturday)");
            }
        }
        if let Some(workloads) = &self.allowed_workload_classes {
            if workloads.iter().any(|workload| {
                !matches!(
                    workload.as_str(),
                    "embed" | "batch_infer" | "media_transcode" | "media_rendering"
                )
            }) {
                anyhow::bail!("allowed_workload_classes contains an unknown workload");
            }
        }
        if let Some((start, end)) = self.quiet_hours {
            if start > 23 || end > 23 {
                anyhow::bail!("quiet_hours values must be 0..23");
            }
        }
        if !self.min_payout_usd_per_hr.is_finite() || self.min_payout_usd_per_hr < 0.0 {
            anyhow::bail!("min_payout_usd_per_hr must be finite and non-negative");
        }
        if !self.memory_headroom_gb.is_finite() || self.memory_headroom_gb < 0.0 {
            anyhow::bail!("memory_headroom_gb must be finite and non-negative");
        }
        if !self.max_memory_pct.is_finite()
            || self.max_memory_pct < 0.0
            || self.max_memory_pct > 100.0
        {
            anyhow::bail!("max_memory_pct must be between 0 and 100");
        }
        if !self.max_model_cache_gb.is_finite() || self.max_model_cache_gb < 0.0 {
            anyhow::bail!("max_model_cache_gb must be finite and non-negative");
        }
        if !self.max_bandwidth_mbps.is_finite() || self.max_bandwidth_mbps < 0.0 {
            anyhow::bail!("max_bandwidth_mbps must be finite and non-negative");
        }
        if !self.max_cpu_pct.is_finite() || self.max_cpu_pct < 0.0 || self.max_cpu_pct > 100.0 {
            anyhow::bail!("max_cpu_pct must be between 0 and 100");
        }
        Ok(())
    }

    pub fn apply_prefs(&mut self, prefs: &OperatorPrefs) {
        if let Some(v) = prefs.paused {
            self.paused = v;
        }
        if let Some(v) = &prefs.allowed_weekdays {
            self.allowed_weekdays = Some(v.clone());
        }
        if let Some(v) = &prefs.allowed_workload_classes {
            self.allowed_workload_classes = Some(v.clone());
        }
        if let Some(v) = prefs.allow_model_downloads {
            self.allow_model_downloads = v;
        }
        if let Some(v) = prefs.max_model_cache_gb {
            self.max_model_cache_gb = v;
        }
        if let Some(v) = prefs.max_bandwidth_mbps {
            self.max_bandwidth_mbps = v;
        }
        if let Some(v) = prefs.max_cpu_pct {
            self.max_cpu_pct = v;
        }
        if let Some(v) = prefs.thermal_limit {
            self.thermal_limit = v;
        }
        if let Some(v) = prefs.power_only {
            self.power_only = v;
        }
        if let Some(v) = prefs.min_payout_usd_per_hr {
            self.min_payout_usd_per_hr = v;
        }
        if let Some(v) = prefs.memory_headroom_gb {
            self.memory_headroom_gb = v;
        }
        if let Some(v) = prefs.max_memory_pct {
            self.max_memory_pct = v;
        }
        if let Some(v) = prefs.quiet_hours {
            self.quiet_hours = Some(v);
        }
        if let Some(v) = prefs.max_concurrent_tasks {
            self.max_concurrent_tasks = if v == 0 { None } else { Some(v) };
        }
        if let Some(v) = prefs.thermal_pressure {
            self.thermal_pressure = Some(v);
        }
    }

    pub fn is_eligible_to_run_at(
        &self,
        now_hour: u8,
        weekday_sunday_zero: u8,
        on_battery: bool,
    ) -> bool {
        if self.paused {
            return false;
        }
        if self
            .allowed_weekdays
            .as_ref()
            .is_some_and(|days| !days.contains(&weekday_sunday_zero))
        {
            return false;
        }
        if self.power_only && on_battery {
            return false;
        }
        if let Some((start, end)) = self.quiet_hours {
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

    pub fn concurrency(&self, memory_gb: f32) -> usize {
        match self.max_concurrent_tasks {
            Some(n) => n.max(1),
            None => ((memory_gb / 8.0) as usize).clamp(2, 4),
        }
    }

    pub fn allows_workload(&self, workload: &str) -> bool {
        self.allowed_workload_classes
            .as_ref()
            .is_none_or(|allowed| allowed.iter().any(|item| item == workload))
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    fn cfg(quiet_hours: Option<(u8, u8)>, power_only: bool) -> AgentConfig {
        AgentConfig {
            control_url: "http://localhost".into(),
            worker_token: "t".into(),
            supplier_id: Uuid::nil(),
            paused: false,
            allowed_weekdays: None,
            allowed_workload_classes: None,
            allow_model_downloads: true,
            max_model_cache_gb: default_max_model_cache_gb(),
            max_bandwidth_mbps: 0.0,
            max_cpu_pct: 0.0,
            thermal_limit: default_thermal_limit(),
            quiet_hours,
            power_only,
            min_payout_usd_per_hr: 0.0,
            memory_headroom_gb: 8.0,
            max_memory_pct: 85.0,
            data_dir: PathBuf::from("/tmp"),
            max_concurrent_tasks: None,
            checkpoint_secs: default_checkpoint_secs(),
            inference_backend: default_inference_backend(),
            openai_base_url: default_openai_base_url(),
            openai_model: default_openai_model(),
            openai_api_key: None,
            embed_runtime: default_embed_runtime(),
            llama_embed_base_url: default_llama_embed_url(),
            thermal_pressure: None,
            prefs_path: None,
        }
    }

    fn snap(total: f32, available: f32) -> MemorySnapshot {
        MemorySnapshot {
            total_gb: total,
            available_gb: available,
        }
    }

    #[test]
    fn throttle_clears_when_memory_is_ample() {
        let d = cfg(None, false).evaluate_memory_throttle(&snap(64.0, 40.0), None);
        assert!(!d.throttled, "reason: {:?}", d.reason);
        assert_eq!(d.reserved_headroom_gb, 8.0);
        assert!((d.effective_gb - 32.0).abs() < 0.01);
    }

    #[test]
    fn throttle_trips_when_headroom_would_be_breached() {
        let d = cfg(None, false).evaluate_memory_throttle(&snap(16.0, 6.0), None);
        assert!(d.throttled);
        assert_eq!(d.effective_gb, 0.0);
        assert!(d.reason.unwrap().contains("headroom"));
    }

    #[test]
    fn throttle_trips_on_utilization_ceiling() {
        let mut c = cfg(None, false);
        c.memory_headroom_gb = 1.0; // headroom alone would NOT trip
        let d = c.evaluate_memory_throttle(&snap(100.0, 10.0), None);
        assert!(d.throttled);
        assert!(d.reason.unwrap().contains("pressure"));
    }

    #[test]
    fn governor_off_when_both_knobs_zero() {
        let mut c = cfg(None, false);
        c.memory_headroom_gb = 0.0;
        c.max_memory_pct = 0.0;
        assert!(!c.evaluate_memory_throttle(&snap(16.0, 0.0), None).throttled);
        assert!(
            !c.evaluate_memory_throttle(&snap(16.0, 16.0), None)
                .throttled
        );
        c.max_memory_pct = 90.0;
        assert!(c.evaluate_memory_throttle(&snap(16.0, 0.5), None).throttled);
    }

    #[test]
    fn throttle_respects_known_next_task_estimate() {
        let c = cfg(None, false);
        assert!(
            c.evaluate_memory_throttle(&snap(32.0, 20.0), Some(16.0))
                .throttled
        );
        assert!(
            !c.evaluate_memory_throttle(&snap(32.0, 20.0), Some(4.0))
                .throttled
        );
    }

    #[test]
    fn vram_tier_gating_uses_same_throttle_against_vram() {
        let mut c = cfg(None, false);
        c.memory_headroom_gb = 0.0; // dedicated GPU box: no host-RAM-style reserve
        c.max_memory_pct = 90.0; // pause once VRAM is >=90% resident

        let near_full = snap(24.0, 3.5);
        let d = c.evaluate_memory_throttle(&near_full, Some(16.0));
        assert!(
            d.throttled,
            "20GB-resident 24GB card must throttle a 16GB job"
        );
        assert!(d.reason.unwrap().contains("16.0 GB"));

        let d = c.evaluate_memory_throttle(&snap(24.0, 2.0), None);
        assert!(d.throttled);
        assert!(d.reason.unwrap().contains("pressure"));

        let d = c.evaluate_memory_throttle(&snap(24.0, 22.0), Some(16.0));
        assert!(
            !d.throttled,
            "idle 24GB card must accept a 16GB job: {:?}",
            d.reason
        );
        assert!((d.effective_gb - 22.0).abs() < 0.01);
    }

    #[test]
    fn battery_blocks_only_when_power_only() {
        assert!(!cfg(None, true).is_eligible_to_run_at(12, 0, true));
        assert!(cfg(None, true).is_eligible_to_run_at(12, 0, false));
        assert!(cfg(None, false).is_eligible_to_run_at(12, 0, true));
    }

    #[test]
    fn no_thermal_reading_never_throttles() {
        let d = cfg(None, false).evaluate_thermal_throttle();
        assert!(!d.throttled);
        assert_eq!(d.reading, None);
    }

    #[test]
    fn nominal_and_fair_do_not_throttle() {
        let mut c = cfg(None, false);
        c.thermal_pressure = Some(ThermalPressure::Nominal);
        assert!(!c.evaluate_thermal_throttle().throttled);
        c.thermal_pressure = Some(ThermalPressure::Fair);
        assert!(!c.evaluate_thermal_throttle().throttled);
    }

    #[test]
    fn serious_and_critical_thermal_pressure_throttle() {
        let mut c = cfg(None, false);
        c.thermal_pressure = Some(ThermalPressure::Serious);
        let d = c.evaluate_thermal_throttle();
        assert!(d.throttled);
        assert!(d.reason.unwrap().contains("serious"));
        assert_eq!(d.reading, Some(ThermalPressure::Serious));

        c.thermal_pressure = Some(ThermalPressure::Critical);
        let d = c.evaluate_thermal_throttle();
        assert!(d.throttled);
        assert!(d.reason.unwrap().contains("critical"));
    }

    #[test]
    fn supplier_can_choose_a_more_conservative_thermal_limit() {
        let mut c = cfg(None, false);
        c.thermal_limit = ThermalPressure::Fair;
        c.thermal_pressure = Some(ThermalPressure::Fair);
        let decision = c.evaluate_thermal_throttle();
        assert!(decision.throttled);
        assert!(decision.reason.unwrap().contains("supplier limit fair"));
    }

    #[test]
    fn thermal_pressure_overlay_merges_via_apply_prefs() {
        let mut c = cfg(None, false);
        assert!(!c.evaluate_thermal_throttle().throttled);

        let prefs = OperatorPrefs {
            thermal_pressure: Some(ThermalPressure::Serious),
            ..Default::default()
        };
        c.apply_prefs(&prefs);
        assert!(c.evaluate_thermal_throttle().throttled);
    }

    #[test]
    fn quiet_hours_same_day_window() {
        let c = cfg(Some((9, 17)), false);
        assert!(!c.is_eligible_to_run_at(9, 0, false));
        assert!(!c.is_eligible_to_run_at(16, 0, false));
        assert!(c.is_eligible_to_run_at(17, 0, false));
        assert!(c.is_eligible_to_run_at(8, 0, false));
    }

    #[test]
    fn concurrency_default_is_memory_aware_and_clamped() {
        let mut c = cfg(None, false);
        assert_eq!(c.concurrency(4.0), 2); // tiny box still gets 2
        assert_eq!(c.concurrency(16.0), 2);
        assert_eq!(c.concurrency(64.0), 4); // big box caps at 4
        c.max_concurrent_tasks = Some(8);
        assert_eq!(c.concurrency(8.0), 8);
        c.max_concurrent_tasks = Some(0);
        assert_eq!(c.concurrency(8.0), 1);
    }

    #[test]
    fn checkpoint_secs_defaults_and_zero_disables() {
        let base = r#"
            control_url = "http://localhost:8080"
            worker_token = "t"
            supplier_id = "00000000-0000-0000-0000-000000000000"
            power_only = false
            min_payout_usd_per_hr = 0.0
            data_dir = "/tmp/merc-agent"
        "#;
        let c: AgentConfig = toml::from_str(base).unwrap();
        assert_eq!(c.checkpoint_secs, 30, "omitted -> the 30s default");
        let c: AgentConfig = toml::from_str(&format!("{base}\ncheckpoint_secs = 7")).unwrap();
        assert_eq!(c.checkpoint_secs, 7);
        let c: AgentConfig = toml::from_str(&format!("{base}\ncheckpoint_secs = 0")).unwrap();
        assert_eq!(
            c.checkpoint_secs, 0,
            "explicit 0 disables, not defaulted over"
        );
    }

    #[test]
    fn inference_backend_defaults_to_candle() {
        let base = r#"
            control_url = "http://localhost:8080"
            worker_token = "t"
            supplier_id = "00000000-0000-0000-0000-000000000000"
            max_cpu_pct = 90.0
            power_only = false
            min_payout_usd_per_hr = 0.0
            data_dir = "/tmp/merc-agent"
        "#;
        let c: AgentConfig = toml::from_str(base).unwrap();
        assert_eq!(c.inference_backend, "candle");
        assert_eq!(c.openai_base_url, "http://127.0.0.1:8099/v1");
        assert_eq!(c.openai_model, "cx-chat-1b");
        let c: AgentConfig =
            toml::from_str(&format!("{base}\ninference_backend = \"openai_http\"\nopenai_base_url = \"http://127.0.0.1:9000/v1\"\nopenai_model = \"my-alias\"\n")).unwrap();
        assert_eq!(c.inference_backend, "openai_http");
        assert_eq!(c.openai_base_url, "http://127.0.0.1:9000/v1");
        assert_eq!(c.openai_model, "my-alias");
    }

    #[test]
    fn quiet_hours_wraps_midnight() {
        let c = cfg(Some((22, 6)), false);
        assert!(!c.is_eligible_to_run_at(23, 0, false));
        assert!(!c.is_eligible_to_run_at(0, 0, false));
        assert!(!c.is_eligible_to_run_at(5, 0, false));
        assert!(c.is_eligible_to_run_at(6, 0, false));
        assert!(c.is_eligible_to_run_at(12, 0, false));
    }

    #[test]
    fn pause_and_weekday_schedule_are_enforced() {
        let mut c = cfg(None, false);
        c.allowed_weekdays = Some(vec![1, 2, 3, 4, 5]);
        assert!(c.is_eligible_to_run_at(12, 1, false));
        assert!(!c.is_eligible_to_run_at(12, 0, false));
        c.paused = true;
        assert!(!c.is_eligible_to_run_at(12, 1, false));
    }

    #[test]
    fn workload_allowlist_is_exact() {
        let mut c = cfg(None, false);
        assert!(c.allows_workload("embed"));
        c.allowed_workload_classes = Some(vec!["media_transcode".into()]);
        assert!(c.allows_workload("media_transcode"));
        assert!(!c.allows_workload("embed"));
        c.allowed_workload_classes = Some(vec!["embed".into()]);
        assert!(c.allows_workload("embed"));
        assert!(!c.allows_workload("batch_infer"));
        c.allowed_workload_classes = Some(vec!["unknown".into()]);
        assert!(c.validate_operator_policy().is_err());
    }

    #[test]
    fn invalid_operator_policy_is_refused() {
        let mut c = cfg(None, false);
        c.allowed_weekdays = Some(vec![7]);
        assert!(c.validate_operator_policy().is_err());
        c.allowed_weekdays = None;
        c.min_payout_usd_per_hr = f32::NAN;
        assert!(c.validate_operator_policy().is_err());
        c.min_payout_usd_per_hr = 0.0;
        c.max_memory_pct = 101.0;
        assert!(c.validate_operator_policy().is_err());
        c.max_memory_pct = 85.0;
        c.max_cpu_pct = 100.1;
        assert!(c.validate_operator_policy().is_err());
    }

    #[test]
    fn preferences_reload_changes_claim_policy_without_restart() {
        let dir = std::env::temp_dir().join(format!("merc-prefs-{}", Uuid::new_v4()));
        std::fs::create_dir_all(&dir).unwrap();
        let path = dir.join("agent.prefs.toml");
        std::fs::write(
            &path,
            "paused = false\nallowed_weekdays = [1]\nmin_payout_usd_per_hr = 1.25\n",
        )
        .unwrap();
        let mut c = cfg(None, false);
        c.prefs_path = Some(path.clone());
        c.refresh_operator_prefs().unwrap();
        assert!(c.is_eligible_to_run_at(12, 1, false));
        assert!(!c.is_eligible_to_run_at(12, 2, false));
        assert_eq!(c.min_payout_usd_per_hr, 1.25);

        std::fs::write(&path, "paused = true\n").unwrap();
        c.refresh_operator_prefs().unwrap();
        assert!(!c.is_eligible_to_run_at(12, 1, false));

        std::fs::write(&path, "max_memory_pct = 101\n").unwrap();
        assert!(c.refresh_operator_prefs().is_err());
        let _ = std::fs::remove_dir_all(dir);
    }

    #[test]
    fn operator_prefs_overlay_overrides_present_fields_only() {
        let mut c = cfg(None, false); // base: no quiet hours, not power-only, headroom 8
        c.min_payout_usd_per_hr = 1.0;
        let prefs = OperatorPrefs {
            power_only: Some(true),
            quiet_hours: Some((22, 6)),
            memory_headroom_gb: Some(2.0),
            min_payout_usd_per_hr: None, // absent -> base value (1.0) survives
            ..Default::default()
        };
        c.apply_prefs(&prefs);
        assert!(c.power_only, "present pref overrides");
        assert_eq!(c.quiet_hours, Some((22, 6)));
        assert_eq!(c.memory_headroom_gb, 2.0);
        assert_eq!(
            c.min_payout_usd_per_hr, 1.0,
            "absent pref leaves the base value untouched"
        );
    }

    #[test]
    fn operator_prefs_actually_control_eligibility() {
        let mut c = cfg(None, false);
        assert!(
            c.is_eligible_to_run_at(2, 0, false),
            "base: eligible at 02:00"
        );
        c.apply_prefs(&OperatorPrefs {
            quiet_hours: Some((22, 6)),
            ..Default::default()
        });
        assert!(
            !c.is_eligible_to_run_at(2, 0, false),
            "after prefs: quiet hours refuse 02:00"
        );
        c.apply_prefs(&OperatorPrefs {
            power_only: Some(true),
            ..Default::default()
        });
        assert!(
            !c.is_eligible_to_run_at(12, 0, true),
            "after prefs: power-only refuses on battery"
        );
    }

    #[test]
    fn operator_prefs_parse_partial_toml_ignoring_unknown_keys() {
        let toml = "power_only = true\nmin_payout_usd_per_hr = 3.5\nquiet_hours = [22, 6]\nunknown_future_key = 7\n";
        let prefs: OperatorPrefs = toml::from_str(toml).unwrap();
        assert_eq!(prefs.power_only, Some(true));
        assert_eq!(prefs.min_payout_usd_per_hr, Some(3.5));
        assert_eq!(prefs.quiet_hours, Some((22, 6)));
        assert_eq!(prefs.memory_headroom_gb, None, "absent key -> None");
    }

    #[test]
    fn operator_prefs_zero_concurrency_means_derive() {
        let mut c = cfg(None, false);
        c.max_concurrent_tasks = Some(7); // base had an explicit pin
        c.apply_prefs(&OperatorPrefs {
            max_concurrent_tasks: Some(0),
            ..Default::default()
        });
        assert_eq!(c.max_concurrent_tasks, None, "0 from the app means derive");
        assert_eq!(c.concurrency(64.0), 4, "derive path: big box caps at 4");
        c.apply_prefs(&OperatorPrefs {
            max_concurrent_tasks: Some(3),
            ..Default::default()
        });
        assert_eq!(c.max_concurrent_tasks, Some(3));
    }
}
