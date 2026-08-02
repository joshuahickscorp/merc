#![allow(dead_code)]

use std::sync::OnceLock;

use serde::Deserialize;
use sha2::{Digest, Sha256};

const AUTHORITY_JSON: &str = include_str!("../../control/runtime-authority.json");

#[derive(Debug, Deserialize)]
struct Authority {
    schema_version: u32,
    matrix_version: String,
    #[serde(default)]
    engines: Vec<Engine>,
    models: Vec<Model>,
    runtimes: Vec<RuntimeProfile>,
}

#[derive(Debug, Deserialize)]
struct Engine {
    engine: String,
    adapter: String,
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
    adapter: String,
    #[serde(default)]
    tokenizer_revision: String,
    #[serde(default)]
    chat_template_id: String,
    #[serde(default)]
    source_identity: String,
    #[serde(default)]
    quality_tier: String,
    lifecycle: String,
    device: String,
    hardware: Hardware,
    #[serde(default)]
    parallelism: Parallelism,
    #[serde(default)]
    capabilities: Capabilities,
    cells: Vec<Cell>,
}

#[derive(Debug, Deserialize)]
struct Hardware {
    platforms: Vec<String>,
    #[serde(default)]
    device_count: DeviceCount,
}

#[derive(Debug, Default, Deserialize)]
struct DeviceCount {
    #[serde(default)]
    minimum: i64,
    #[serde(default)]
    maximum: i64,
}

#[derive(Debug, Default, Deserialize)]
struct Parallelism {
    #[serde(default)]
    continuous_batching: bool,
    #[serde(default)]
    tensor_parallel: bool,
    #[serde(default)]
    pipeline_parallel: bool,
    #[serde(default)]
    data_parallel: bool,
}

#[derive(Debug, Default, Deserialize)]
struct Capabilities {
    #[serde(default)]
    streaming: bool,
    #[serde(default)]
    prefix_cache: bool,
    #[serde(default)]
    speculation: bool,
}

fn lifecycle_is_routable(lifecycle: &str) -> bool {
    lifecycle == "CANARY" || lifecycle == "ACTIVE"
}

/// Whether this agent may DECLARE a cell at all.
///
/// The agent declares capability; the control plane decides activation. So the
/// filter here is only the terminal states — QUARANTINED, RETIRED and
/// REJECTED_FOR_CONTRACT are decisions that policy alone will not reverse, and a
/// worker offering to sell a contract measurement has ruled out is a claim it
/// cannot honour.
///
/// DRAFT and VALIDATED are deliberately included. The previous filter was
/// "VALIDATED and above", which put lifecycle on the AGENT BUILD: an agent whose
/// embedded document called a cell DRAFT could not declare it, so promoting that
/// cell by policy still required rebuilding and redeploying the fleet — exactly
/// the coupling the capability/activation split exists to remove. The control
/// plane authorizes only what its activation policy allows, so declaring more
/// here widens nothing.
fn lifecycle_is_capability_offerable(lifecycle: &str) -> bool {
    !matches!(
        lifecycle,
        "QUARANTINED" | "RETIRED" | "REJECTED_FOR_CONTRACT"
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
    #[serde(default)]
    hf_repo: String,
    #[serde(default)]
    hf_revision: String,
    #[serde(default)]
    artifacts: Vec<Artifact>,
}

#[derive(Debug, Deserialize)]
struct Artifact {
    #[serde(default)]
    repo: String,
    #[serde(default)]
    revision: String,
    path: String,
    sha256: String,
    #[serde(default)]
    bytes: i64,
    #[serde(default)]
    wire_kind: String,
}

impl Model {
    fn artifact_wire_kind<'a>(&'a self, artifact: &'a Artifact) -> &'a str {
        if artifact.wire_kind.is_empty() {
            &self.wire_kind
        } else {
            &artifact.wire_kind
        }
    }

    /// `repo@revision/path:sha256` — the identity is the bytes, the rest is the
    /// provenance that names them. Mirrors resolvedArtifactRef in Go.
    fn artifact_ref(&self, artifact: &Artifact) -> String {
        let (repo, revision) = if artifact.repo.is_empty() {
            (&self.hf_repo, &self.hf_revision)
        } else {
            (&artifact.repo, &artifact.revision)
        };
        format!(
            "{}@{}/{}:{}",
            repo, revision, artifact.path, artifact.sha256
        )
    }
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
    /// Measured parallelism limits. Zero means unstated, never unlimited.
    #[serde(default)]
    max_batch: i64,
    #[serde(default)]
    max_concurrency: i64,
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

/// The capability manifest digest, mirroring control/capability_manifest.go.
///
/// The agent used to hash the raw bytes of the embedded document, and the control
/// plane did the same, so the two agreed only while the file was byte-identical.
/// Promoting a cell's lifecycle rewrites those bytes, which meant every agent had
/// to be rebuilt before it could enrol against a promoted matrix — a lifecycle
/// change had become a coordinated binary deploy.
///
/// This digest covers CAPABILITY only: engines and adapters, models and their
/// exact artifact bytes, and per profile the engine, adapter, tokenizer, chat
/// template, source identity, device, hardware, parallelism, runtime features and
/// cells. Lifecycle, quality tier, benchmark authority, evidence, rejection reason
/// and superseded_by are activation policy, owned by the control plane at runtime,
/// and are all absent here.
///
/// The canonical form is line-oriented rather than JSON precisely because the two
/// implementations are in different languages: Go serialises `float64(2)` as `2`
/// and Rust serialises `2.0f64` as `2.0`, so a JSON canonicalisation would produce
/// two different digests for one document and the disagreement would surface as
/// every agent being refused. Numbers are formatted explicitly on both sides.
const CAPABILITY_MANIFEST_VERSION: u32 = 2;

/// ASCII unit separator: cannot occur in an identifier, path, digest or platform
/// name, so no value can forge a field boundary.
const FIELD_SEP: char = '\u{1f}';

fn cap_line(fields: &[&str]) -> String {
    fields.join(&FIELD_SEP.to_string())
}

fn cap_float(v: f64) -> String {
    format!("{v:.6}")
}

fn cap_bool(v: bool) -> &'static str {
    if v {
        "1"
    } else {
        "0"
    }
}

fn cap_version_line() -> String {
    cap_line(&[
        "capability-manifest-version",
        &CAPABILITY_MANIFEST_VERSION.to_string(),
    ])
}

fn cap_digest_of(mut lines: Vec<String>) -> String {
    lines.sort();
    let mut buf = String::new();
    for line in lines {
        buf.push_str(&line);
        buf.push('\n');
    }
    format!("{:x}", Sha256::digest(buf.as_bytes()))
}

fn profile_capability_lines(authority: &Authority, profile: &RuntimeProfile) -> Vec<String> {
    let mut lines = vec![cap_line(&[
        "profile",
        &profile.runtime_id,
        &profile.revision,
        &profile.engine,
        &profile.engine_revision,
        &profile.adapter,
        &profile.tokenizer_revision,
        &profile.chat_template_id,
        &profile.source_identity,
        &profile.device,
        &profile.hardware.device_count.minimum.to_string(),
        &profile.hardware.device_count.maximum.to_string(),
    ])];
    for platform in &profile.hardware.platforms {
        lines.push(cap_line(&[
            "profile-platform",
            &profile.runtime_id,
            platform,
        ]));
    }
    // Every flag, including the false ones: turning one off must move the digest.
    for (name, on) in [
        (
            "capabilities.prefix_cache",
            profile.capabilities.prefix_cache,
        ),
        ("capabilities.speculation", profile.capabilities.speculation),
        ("capabilities.streaming", profile.capabilities.streaming),
        (
            "parallelism.continuous_batching",
            profile.parallelism.continuous_batching,
        ),
        (
            "parallelism.data_parallel",
            profile.parallelism.data_parallel,
        ),
        (
            "parallelism.pipeline_parallel",
            profile.parallelism.pipeline_parallel,
        ),
        (
            "parallelism.tensor_parallel",
            profile.parallelism.tensor_parallel,
        ),
    ] {
        lines.push(cap_line(&[
            "profile-flag",
            &profile.runtime_id,
            name,
            cap_bool(on),
        ]));
    }
    for cell in &profile.cells {
        let model = authority
            .models
            .iter()
            .find(|model| model.id == cell.model)
            .expect("runtime cell model must exist");
        let kind = if cell.wire_kind.is_empty() {
            model.wire_kind.as_str()
        } else {
            cell.wire_kind.as_str()
        };
        lines.push(cap_line(&[
            "cell",
            &profile.runtime_id,
            &cell.id,
            &cell.job,
            &cell.model,
            &cell.runner,
            kind,
            &cap_float(cell.min_memory_gb),
            &cell.verification,
            &cell.max_batch.to_string(),
            &cell.max_concurrency.to_string(),
        ]));
        for artifact in &model.artifacts {
            if model.artifact_wire_kind(artifact) != kind {
                continue;
            }
            lines.push(cap_line(&[
                "cell-artifact",
                &profile.runtime_id,
                &cell.id,
                &model.artifact_ref(artifact),
            ]));
        }
    }
    lines
}

fn capability_matrix_digest(authority: &Authority) -> String {
    let mut lines = vec![cap_version_line()];
    for engine in &authority.engines {
        lines.push(cap_line(&["engine", &engine.engine, &engine.adapter]));
    }
    for model in &authority.models {
        lines.push(cap_line(&[
            "model",
            &model.id,
            &model.wire_kind,
            &model.job_type,
            &cap_float(model.min_memory_gb),
            &model.hf_repo,
            &model.hf_revision,
        ]));
        for artifact in &model.artifacts {
            lines.push(cap_line(&[
                "model-artifact",
                &model.id,
                model.artifact_wire_kind(artifact),
                &model.artifact_ref(artifact),
                &artifact.bytes.to_string(),
            ]));
        }
    }
    for profile in &authority.runtimes {
        lines.extend(profile_capability_lines(authority, profile));
    }
    cap_digest_of(lines)
}

struct Projection {
    version: String,
    sha256: String,
    file_sha256: String,
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
                // Capability, not activation.
                //
                // A routable-only projection made a llama.cpp worker advertise
                // nothing: its embed cell was VALIDATED, so the worker had zero
                // capabilities and registration was refused, so it could never
                // execute the chain that would prove the cell. Terminal states
                // are still excluded, so the REJECTED_FOR_CONTRACT generation
                // cell remains undeclarable.
                if !lifecycle_is_capability_offerable(effective) {
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
            "no runtime profile projects any declarable cell"
        );
        Projection {
            sha256: capability_matrix_digest(&authority),
            version: authority.matrix_version,
            file_sha256: format!("{:x}", Sha256::digest(AUTHORITY_JSON.as_bytes())),
            capabilities,
        }
    })
}

pub fn version() -> &'static str {
    &projection().version
}

/// The CAPABILITY matrix digest — what the control plane and this agent must
/// agree on before a task may be executed. Stable across lifecycle promotions.
pub fn sha256() -> &'static str {
    &projection().sha256
}

/// The embedded document's byte identity. Provenance for receipts only; never a
/// dispatch check, because it moves on whitespace and on any policy edit.
pub fn file_sha256() -> &'static str {
    &projection().file_sha256
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

#[cfg(test)]
mod tests {
    /// The capability matrix digest is the one value the agent and the control
    /// plane MUST compute identically: the agent refuses any dispatched task whose
    /// matrix digest is not its own, so a disagreement grounds the whole fleet.
    ///
    /// Pinning it on both sides is the cheapest way to make a divergence a test
    /// failure rather than a production refusal. control/capability_manifest_test.go
    /// pins the same constant. A capability change is expected to move it — and to
    /// move it in both places, in one commit.
    #[test]
    fn capability_matrix_digest_matches_the_control_plane() {
        assert_eq!(
            super::sha256(),
            "c9d4f7f8041e0706a564409d07dc71aa60ff58576ed535427ff3f99c2bc7237b",
            "the agent's capability matrix digest no longer matches the pinned \
             control-plane value; if this is a deliberate capability change, update \
             the pin in BOTH control/capability_manifest_test.go and here"
        );
    }

    /// The document digest and the capability digest are different questions, and
    /// conflating them is what made a lifecycle promotion a fleet rebuild.
    #[test]
    fn capability_digest_is_not_the_document_digest() {
        assert_ne!(super::sha256(), super::file_sha256());
    }
}
