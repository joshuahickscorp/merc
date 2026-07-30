#![allow(dead_code)]

use std::sync::OnceLock;

use serde::Deserialize;
use sha2::{Digest, Sha256};

const AUTHORITY_JSON: &str = include_str!("../../control/runtime-authority.json");

#[derive(Debug, Deserialize)]
struct Authority {
    schema_version: u32,
    matrix_version: String,
    models: Vec<Model>,
    runtimes: Vec<RuntimeProfile>,
}

/// Schema v2 replaced the single `runtime` object with a list of profiles, each
/// carrying its own cells and a lifecycle that decides routability.
///
/// The agent projects ONLY routable profiles, exactly as the control plane does.
/// A worker that advertised a VALIDATED or DRAFT profile's cells would be
/// offering to serve work the control plane will never dispatch, and the
/// mismatch would surface as an unexplained admission failure rather than as the
/// governance decision it actually is.
#[derive(Debug, Deserialize)]
struct RuntimeProfile {
    runtime_id: String,
    engine: String,
    lifecycle: String,
    device: String,
    hardware: Hardware,
    cells: Vec<Cell>,
}

#[derive(Debug, Deserialize)]
struct Hardware {
    platforms: Vec<String>,
}

fn lifecycle_is_routable(lifecycle: &str) -> bool {
    lifecycle == "CANARY" || lifecycle == "ACTIVE"
}

#[derive(Debug, Deserialize)]
struct Model {
    id: String,
    wire_kind: String,
    job_type: String,
    min_memory_gb: f64,
}

#[derive(Debug, Deserialize)]
struct Cell {
    id: String,
    job: String,
    model: String,
    runner: String,
    /// Artifact format THIS runtime loads the model from; empty inherits the
    /// model's. Format belongs to the (runtime, model) pair — candle serves
    /// MiniLM from safetensors and llama.cpp serves the same logical model from
    /// a GGUF — and the control plane made it per-cell. Reading only
    /// `model.wire_kind` here would have a worker advertise `hf` for a cell that
    /// serves GGUF the moment a non-candle profile became routable.
    #[serde(default)]
    wire_kind: String,
    min_memory_gb: f64,
    verification: String,
}

#[derive(Debug)]
pub struct RuntimeCapability {
    pub id: String,
    pub runtime: String,
    pub engine: String,
    pub device: String,
    pub hardware_classes: Vec<String>,
    pub job: String,
    pub model: String,
    pub model_kind: String,
    pub runner: String,
    pub min_memory_gb: f64,
    pub verification: String,
}

struct Projection {
    version: String,
    sha256: String,
    capabilities: Vec<RuntimeCapability>,
}

static PROJECTION: OnceLock<Projection> = OnceLock::new();

fn projection() -> &'static Projection {
    PROJECTION.get_or_init(|| {
        let authority: Authority =
            serde_json::from_str(AUTHORITY_JSON).expect("decode embedded runtime authority");
        assert_eq!(authority.schema_version, 2, "unsupported runtime authority");
        assert!(!authority.matrix_version.is_empty());
        assert!(!authority.models.is_empty(), "runtime authority defines no models");
        assert!(!authority.runtimes.is_empty(), "runtime authority defines no runtimes");

        let mut capabilities: Vec<RuntimeCapability> = Vec::new();
        for profile in &authority.runtimes {
            assert!(!profile.runtime_id.is_empty());
            assert!(!profile.engine.is_empty());
            assert!(!profile.device.is_empty());
            assert!(!profile.hardware.platforms.is_empty());
            if !lifecycle_is_routable(&profile.lifecycle) {
                continue;
            }
            for cell in &profile.cells {
                let model = authority
                    .models
                    .iter()
                    .find(|model| model.id == cell.model)
                    .expect("runtime cell model must exist");
                assert_eq!(
                    cell.job, model.job_type,
                    "runtime cell job must match model"
                );
                assert_eq!(cell.runner, cell.job, "runtime runner must match job");
                assert!(cell.min_memory_gb >= model.min_memory_gb);
                assert!(!cell.id.is_empty() && !cell.verification.is_empty());
                assert!(!capabilities
                    .iter()
                    .any(|known: &RuntimeCapability| known.id == cell.id));
                capabilities.push(RuntimeCapability {
                    id: cell.id.clone(),
                    runtime: profile.runtime_id.clone(),
                    engine: profile.engine.clone(),
                    device: profile.device.clone(),
                    hardware_classes: profile.hardware.platforms.clone(),
                    job: cell.job.clone(),
                    model: cell.model.clone(),
                    model_kind: if cell.wire_kind.is_empty() {
                        model.wire_kind.clone()
                    } else {
                        cell.wire_kind.clone()
                    },
                    runner: cell.runner.clone(),
                    min_memory_gb: cell.min_memory_gb,
                    verification: cell.verification.clone(),
                });
            }
        }
        assert!(
            !capabilities.is_empty(),
            "no routable runtime profile projects any cell"
        );
        Projection {
            version: authority.matrix_version,
            sha256: format!("{:x}", Sha256::digest(AUTHORITY_JSON.as_bytes())),
            capabilities,
        }
    })
}

pub fn version() -> &'static str {
    &projection().version
}

pub fn sha256() -> &'static str {
    &projection().sha256
}

pub fn capabilities() -> &'static [RuntimeCapability] {
    &projection().capabilities
}
