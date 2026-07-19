#![allow(dead_code)]

use std::sync::OnceLock;

use serde::Deserialize;
use sha2::{Digest, Sha256};

const AUTHORITY_JSON: &str = include_str!("../../control/runtime-authority.json");

#[derive(Debug, Deserialize)]
struct Authority {
    schema_version: u32,
    matrix_version: String,
    runtime: Runtime,
    models: Vec<Model>,
    cells: Vec<Cell>,
}

#[derive(Debug, Deserialize)]
struct Runtime {
    id: String,
    engine: String,
    device: String,
    hardware_classes: Vec<String>,
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
        assert_eq!(authority.schema_version, 1, "unsupported runtime authority");
        assert_eq!(
            authority.models.len(),
            2,
            "runtime authority must have two models"
        );
        assert_eq!(
            authority.cells.len(),
            2,
            "runtime authority must have two cells"
        );
        assert!(!authority.matrix_version.is_empty());
        assert!(!authority.runtime.id.is_empty());
        assert!(!authority.runtime.engine.is_empty());
        assert!(!authority.runtime.device.is_empty());
        assert!(!authority.runtime.hardware_classes.is_empty());

        let mut capabilities = Vec::with_capacity(authority.cells.len());
        for cell in authority.cells {
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
                id: cell.id,
                runtime: authority.runtime.id.clone(),
                engine: authority.runtime.engine.clone(),
                device: authority.runtime.device.clone(),
                hardware_classes: authority.runtime.hardware_classes.clone(),
                job: cell.job,
                model: cell.model,
                model_kind: model.wire_kind.clone(),
                runner: cell.runner,
                min_memory_gb: cell.min_memory_gb,
                verification: cell.verification,
            });
        }
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
