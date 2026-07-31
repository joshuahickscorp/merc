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
    revision: String,
    engine: String,
    #[serde(default)]
    engine_revision: String,
    #[serde(default)]
    tokenizer_revision: String,
    #[serde(default)]
    quality_tier: String,
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

/// Whether an operator or a test may direct work to a cell in this state.
/// VALIDATED and above, terminal states excluded — the same predicate the
/// control plane applies, because a worker advertising a different set than the
/// control plane will direct to is an admission failure dressed as a mismatch.
fn lifecycle_is_directed_reachable(lifecycle: &str) -> bool {
    matches!(
        lifecycle,
        "VALIDATED" | "REAL_RUNTIME_PROVEN" | "CANARY" | "ACTIVE"
    )
}

/// Mirrors the control plane's ordering. Terminal exclusions rank below DRAFT:
/// they are not partial progress toward routability.
fn lifecycle_rank(lifecycle: &str) -> u8 {
    match lifecycle {
        "QUARANTINED" | "RETIRED" | "REJECTED_FOR_CONTRACT" => 0,
        "DRAFT" => 1,
        "VALIDATED" => 2,
        "REAL_RUNTIME_PROVEN" => 3,
        "CANARY" => 4,
        "ACTIVE" => 5,
        _ => 0,
    }
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
    /// This cell's own lifecycle; empty inherits the profile's. The control
    /// plane made the lifecycle per cell because the evidence is per cell —
    /// llama.cpp's embed cell is proven and its byte_exact generation cell is
    /// rejected — and a worker projecting the profile's state would advertise
    /// the rejected one.
    #[serde(default)]
    lifecycle: String,
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
        assert!(
            !authority.models.is_empty(),
            "runtime authority defines no models"
        );
        assert!(
            !authority.runtimes.is_empty(),
            "runtime authority defines no runtimes"
        );

        let mut capabilities: Vec<RuntimeCapability> = Vec::new();
        for profile in &authority.runtimes {
            assert!(!profile.runtime_id.is_empty());
            assert!(!profile.engine.is_empty());
            assert!(!profile.device.is_empty());
            assert!(!profile.hardware.platforms.is_empty());
            for cell in &profile.cells {
                // A cell's own lifecycle, floored by its profile's: a profile
                // cannot inflate a cell, and a cell cannot outrank its profile.
                let effective = if cell.lifecycle.is_empty() {
                    profile.lifecycle.as_str()
                } else if lifecycle_rank(&cell.lifecycle) <= lifecycle_rank(&profile.lifecycle) {
                    cell.lifecycle.as_str()
                } else {
                    profile.lifecycle.as_str()
                };
                // Directed-reachable, not routable-only, mirroring the control
                // plane's projectDirectedRuntimeCapabilities.
                //
                // A routable-only projection made a llama.cpp worker advertise
                // nothing: its embed cell is VALIDATED, so the worker had zero
                // capabilities and registration was refused. It could then never
                // execute the chain that would prove the cell. Terminal states
                // are still excluded, so the REJECTED_FOR_CONTRACT generation
                // cell remains unadvertisable.
                if !lifecycle_is_directed_reachable(effective) {
                    continue;
                }
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
            "no runtime profile projects any directed-reachable cell"
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

/// The governed identity of the named profiles, straight from the embedded
/// document.
///
/// A benchmark receipt that names a profile without its revision is evidence for
/// a name, not for a configuration: `llama_cpp_metal` has meant five different
/// things. The content digest is deliberately NOT restated here — it is derived
/// from the document by the control plane, and copying a derived value into a
/// file is how it goes stale. `bindBenchmarkReceipts` resolves (id, revision) to
/// the digest in PostgreSQL, where the binding is checked rather than asserted.
pub fn profile_identities(ids: &[&str]) -> serde_json::Value {
    let authority: Authority =
        serde_json::from_str(AUTHORITY_JSON).expect("decode embedded runtime authority");
    let mut out = serde_json::Map::new();
    for id in ids {
        if let Some(profile) = authority.runtimes.iter().find(|p| p.runtime_id == *id) {
            out.insert(
                (*id).to_string(),
                serde_json::json!({
                    "runtime_profile_id": profile.runtime_id,
                    "revision": profile.revision,
                    "engine": profile.engine,
                    "engine_revision": profile.engine_revision,
                    "lifecycle": profile.lifecycle,
                    "quality_tier": profile.quality_tier,
                    "tokenizer_revision": profile.tokenizer_revision,
                }),
            );
        }
    }
    serde_json::Value::Object(out)
}

/// The exact artifacts backing a model, per wire kind. Two runtimes serving one
/// model id are not serving one file, and a receipt that recorded only the id
/// could not tell a reader which bytes were measured.
pub fn model_artifacts(model_id: &str) -> serde_json::Value {
    let raw: serde_json::Value =
        serde_json::from_str(AUTHORITY_JSON).expect("decode embedded runtime authority");
    let Some(model) = raw["models"]
        .as_array()
        .and_then(|models| models.iter().find(|m| m["id"].as_str() == Some(model_id)))
    else {
        return serde_json::Value::Null;
    };
    let default_kind = model["wire_kind"].as_str().unwrap_or_default();
    let mut out = serde_json::Map::new();
    for artifact in model["artifacts"].as_array().into_iter().flatten() {
        let kind = artifact["wire_kind"].as_str().unwrap_or(default_kind);
        out.entry(kind.to_string())
            .or_insert_with(|| serde_json::Value::Array(Vec::new()))
            .as_array_mut()
            .expect("array")
            .push(serde_json::json!({
                "repo": artifact["repo"].as_str().unwrap_or(model["hf_repo"].as_str().unwrap_or_default()),
                "revision": artifact["revision"].as_str().unwrap_or(model["hf_revision"].as_str().unwrap_or_default()),
                "path": artifact["path"],
                "sha256": artifact["sha256"],
            }));
    }
    serde_json::json!({ "quantization": model["quant"], "by_wire_kind": out })
}
