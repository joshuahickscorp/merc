use std::path::{Path, PathBuf};

use anyhow::{Context, Result};
use serde::Deserialize;
use uuid::Uuid;

use crate::hardware::MemorySnapshot;

fn default_memory_headroom_gb() -> f32 {
    8.0
}

fn default_max_memory_pct() -> f32 {
    85.0
}

fn default_checkpoint_secs() -> u64 {
    30
}

#[derive(Debug, Clone, Copy, PartialEq, Eq, Deserialize)]
#[serde(rename_all = "lowercase")]
pub enum ThermalPressure {
    Nominal,
    Fair,
    Serious,
    Critical,
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
    pub max_cpu_pct: f32,
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
    #[serde(skip, default)]
    pub thermal_pressure: Option<ThermalPressure>,
    #[serde(skip, default)]
    pub prefs_path: Option<PathBuf>,
}

#[derive(Debug, Clone, Default, Deserialize)]
pub struct OperatorPrefs {
    pub power_only: Option<bool>,
    pub min_payout_usd_per_hr: Option<f32>,
    pub memory_headroom_gb: Option<f32>,
    pub max_memory_pct: Option<f32>,
    pub max_cpu_pct: Option<f32>,
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
            Some(ThermalPressure::Serious) => ThermalDecision {
                throttled: true,
                reason: Some("thermal pressure: serious (macOS is throttling this Mac)".into()),
                reading: Some(ThermalPressure::Serious),
            },
            Some(ThermalPressure::Critical) => ThermalDecision {
                throttled: true,
                reason: Some("thermal pressure: critical".into()),
                reading: Some(ThermalPressure::Critical),
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

        if let Ok(url) = std::env::var("CX_CONTROL_URL") {
            if !url.is_empty() {
                cfg.control_url = url;
            }
        }
        if let Ok(token) = std::env::var("CX_WORKER_TOKEN") {
            if !token.is_empty() {
                cfg.worker_token = token;
            }
        }
        if let Ok(secs) = std::env::var("CX_CHECKPOINT_SECS") {
            if !secs.is_empty() {
                cfg.checkpoint_secs = secs
                    .parse()
                    .with_context(|| format!("parsing CX_CHECKPOINT_SECS {secs:?}"))?;
            }
        }

        if let Some(pp) = Self::resolve_prefs_path(path) {
            cfg.apply_prefs(&OperatorPrefs::load(&pp)?);
        }
        cfg.prefs_path = Self::resolve_prefs_path(path);

        Ok(cfg)
    }

    fn resolve_prefs_path(config_path: &Path) -> Option<PathBuf> {
        match std::env::var("CX_AGENT_PREFS") {
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

    pub fn apply_prefs(&mut self, prefs: &OperatorPrefs) {
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
        if let Some(v) = prefs.max_cpu_pct {
            self.max_cpu_pct = v;
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

    pub fn is_eligible_to_run(&self, now_hour: u8, on_battery: bool) -> bool {
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
}

#[cfg(test)]
mod tests {
    use super::*;

    fn cfg(quiet_hours: Option<(u8, u8)>, power_only: bool) -> AgentConfig {
        AgentConfig {
            control_url: "http://localhost".into(),
            worker_token: "t".into(),
            supplier_id: Uuid::nil(),
            max_cpu_pct: 80.0,
            quiet_hours,
            power_only,
            min_payout_usd_per_hr: 0.0,
            memory_headroom_gb: 8.0,
            max_memory_pct: 85.0,
            data_dir: PathBuf::from("/tmp"),
            max_concurrent_tasks: None,
            checkpoint_secs: default_checkpoint_secs(),
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
        assert!(!cfg(None, true).is_eligible_to_run(12, true));
        assert!(cfg(None, true).is_eligible_to_run(12, false));
        assert!(cfg(None, false).is_eligible_to_run(12, true));
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
        assert!(!c.is_eligible_to_run(9, false));
        assert!(!c.is_eligible_to_run(16, false));
        assert!(c.is_eligible_to_run(17, false));
        assert!(c.is_eligible_to_run(8, false));
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
            max_cpu_pct = 90.0
            power_only = false
            min_payout_usd_per_hr = 0.0
            data_dir = "/tmp/cx-agent"
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
    fn quiet_hours_wraps_midnight() {
        let c = cfg(Some((22, 6)), false);
        assert!(!c.is_eligible_to_run(23, false));
        assert!(!c.is_eligible_to_run(0, false));
        assert!(!c.is_eligible_to_run(5, false));
        assert!(c.is_eligible_to_run(6, false));
        assert!(c.is_eligible_to_run(12, false));
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
        assert!(c.is_eligible_to_run(2, false), "base: eligible at 02:00");
        c.apply_prefs(&OperatorPrefs {
            quiet_hours: Some((22, 6)),
            ..Default::default()
        });
        assert!(
            !c.is_eligible_to_run(2, false),
            "after prefs: quiet hours refuse 02:00"
        );
        c.apply_prefs(&OperatorPrefs {
            power_only: Some(true),
            ..Default::default()
        });
        assert!(
            !c.is_eligible_to_run(12, true),
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
