use std::fs::File;
use std::io::Read;
use std::path::{Path, PathBuf};
use std::sync::atomic::{AtomicBool, AtomicU64, Ordering};
use std::sync::OnceLock;

use anyhow::{Context, Result};
use candle_core::Device;
use hf_hub::api::sync::{Api, ApiBuilder};
use hf_hub::{Cache, Repo, RepoType};
use sha2::{Digest, Sha256};

use crate::executor::RunError;

static DEVICE: OnceLock<Device> = OnceLock::new();
static ALLOW_MODEL_DOWNLOADS: AtomicBool = AtomicBool::new(true);
static MAX_MODEL_CACHE_BYTES: AtomicU64 = AtomicU64::new(4 * 1024 * 1024 * 1024);

pub fn set_cache_policy(allow_downloads: bool, max_cache_gb: f32) {
    ALLOW_MODEL_DOWNLOADS.store(allow_downloads, Ordering::Release);
    let bytes =
        ((max_cache_gb.max(0.0) as f64) * 1024.0 * 1024.0 * 1024.0).min(u64::MAX as f64) as u64;
    MAX_MODEL_CACHE_BYTES.store(bytes, Ordering::Release);
}

pub fn device() -> &'static Device {
    DEVICE.get_or_init(|| {
        #[cfg(feature = "metal")]
        {
            // Candle's Metal init can panic (not just Err) when another process
            // already owns the GPU — e.g. llama-server during dual-backend bench.
            // Catch that so we fall back to CPU instead of taking down the agent.
            if std::env::var("MERC_FORCE_CPU")
                .map(|v| matches!(v.trim().to_ascii_lowercase().as_str(), "1" | "true" | "yes"))
                .unwrap_or(false)
            {
                tracing::info!("compute device: CPU (MERC_FORCE_CPU)");
                return Device::Cpu;
            }
            // Suppress the default panic printer: candle panics (does not Err)
            // when Metal is already owned by another process.
            let prev_hook = std::panic::take_hook();
            std::panic::set_hook(Box::new(|_| {}));
            let metal =
                std::panic::catch_unwind(std::panic::AssertUnwindSafe(|| Device::new_metal(0)));
            std::panic::set_hook(prev_hook);
            match metal {
                Ok(Ok(device)) => {
                    tracing::info!("compute device: Metal");
                    return device;
                }
                Ok(Err(error)) => tracing::warn!(%error, "Metal unavailable; using CPU"),
                Err(_) => tracing::warn!(
                    "Metal device init panicked (GPU likely held by another process); using CPU"
                ),
            }
        }
        #[cfg(not(feature = "metal"))]
        tracing::info!("compute device: CPU");
        Device::Cpu
    })
}

pub fn device_label() -> &'static str {
    if device().is_metal() {
        "metal"
    } else {
        "cpu"
    }
}

#[derive(Clone, Copy)]
pub struct ModelSpec {
    pub repo: &'static str,
    pub revision: &'static str,
    pub files: &'static [ModelFile],
}

#[derive(Clone, Copy)]
pub struct ModelFile {
    pub name: &'static str,
    pub sha256: &'static str,
    pub bytes: u64,
}

pub const EMBED_MINILM_ID: &str = "all-minilm-l6-v2";
pub const INFER_LLAMA_ID: &str = "llama-3.2-1b-instruct-q4";

const EMBED: ModelSpec = ModelSpec {
    repo: "sentence-transformers/all-MiniLM-L6-v2",
    revision: "1110a243fdf4706b3f48f1d95db1a4f5529b4d41",
    files: &[
        ModelFile {
            name: "config.json",
            sha256: "953f9c0d463486b10a6871cc2fd59f223b2c70184f49815e7efbcab5d8908b41",
            bytes: 612,
        },
        ModelFile {
            name: "tokenizer.json",
            sha256: "be50c3628f2bf5bb5e3a7f17b1f74611b2561a3a27eeab05e5aa30f411572037",
            bytes: 466_247,
        },
        ModelFile {
            name: "model.safetensors",
            sha256: "53aa51172d142c89d9012cce15ae4d6cc0ca6895895114379cacb4fab128d9db",
            bytes: 90_868_376,
        },
    ],
};

const INFER: ModelSpec = ModelSpec {
    repo: "unsloth/Llama-3.2-1B-Instruct-GGUF",
    revision: "b69aef112e9f895e6f98d7ae0949f72ff09aa401",
    files: &[ModelFile {
        name: "Llama-3.2-1B-Instruct-Q4_K_M.gguf",
        sha256: "3f5a22426976ab26cfe84dba63c1d08391717abb1af893e10f1b2968d862dcc1",
        bytes: 807_694_368,
    }],
};

pub fn retained_model_revisions() -> Vec<(&'static str, &'static str)> {
    vec![
        (EMBED_MINILM_ID, EMBED.revision),
        (INFER_LLAMA_ID, INFER.revision),
    ]
}

pub const LLAMA_TOKENIZER: ModelSpec = ModelSpec {
    repo: "unsloth/Llama-3.2-1B-Instruct",
    revision: "5a8abab4a5d6f164389b1079fb721cfab8d7126c",
    files: &[ModelFile {
        name: "tokenizer.json",
        sha256: "6b9e4e7fb171f92fd137b777cc2714bf87d11576700a1dcd7a399e7bbe39537b",
        bytes: 17_209_920,
    }],
};

/// The same logical MiniLM, as the GGUF llama.cpp loads.
///
/// Format belongs to the (runtime, model) pair: candle serves this model from
/// safetensors and llama.cpp cannot load those at all. Both are pinned in
/// control/runtime-authority.json under the model's artifact list, the GGUF
/// tagged `wire_kind: gguf`, and `model_pins_match_runtime_authority` checks
/// every field of both against it.
// The GGUF form of the embedding model. llama.cpp loads it through llama-server
// rather than through this loader, so nothing in the agent reads the spec yet —
// but the artifact is governed and digest-pinned here, which is where a future
// in-process GGUF path will look for it.
#[allow(dead_code)]
const EMBED_GGUF: ModelSpec = ModelSpec {
    repo: "leliuga/all-MiniLM-L6-v2-GGUF",
    revision: "ddf2e25d5b8530422e7b14aa39f33a657ff9aec0",
    files: &[ModelFile {
        name: "all-MiniLM-L6-v2.F16.gguf",
        sha256: "797b70c4edf85907fe0a49eb85811256f65fa0f7bf52166b147fd16be2be4662",
        bytes: 45_949_216,
    }],
};

pub fn embed_spec(_model_ref: &str) -> (&'static str, ModelSpec) {
    (EMBED_MINILM_ID, EMBED)
}

/// The embed artifact for a given wire kind. `hf` is candle's safetensors,
/// `gguf` is llama.cpp's — the same weights, a format each engine can load.
#[allow(dead_code)] // resolves EMBED_GGUF; see the note there
pub fn embed_spec_for_kind(kind: &str) -> Result<ModelSpec, RunError> {
    match kind {
        "hf" => Ok(EMBED),
        "gguf" => Ok(EMBED_GGUF),
        other => Err(RunError::Inference {
            backend: "embed",
            msg: format!("no embed artifact for wire kind {other:?}"),
        }),
    }
}

/// Resolve a generative model reference to its governed id and GGUF pin.
///
/// Unknown refs fail closed: the pool must never alias an unrecognised name
/// onto a resident model. Today only one generative model is governed; when a
/// second is added, it gets its own entry here and its own pool slot.
pub fn llama_spec(model_ref: &str) -> Result<(&'static str, ModelSpec), RunError> {
    match model_ref {
        INFER_LLAMA_ID => Ok((INFER_LLAMA_ID, INFER)),
        other => Err(RunError::Inference {
            backend: "batch_infer",
            msg: format!("unresolvable generative model ref: {other:?}"),
        }),
    }
}

pub fn llama_gguf_spec(model_ref: &str) -> Result<ModelSpec, RunError> {
    Ok(llama_spec(model_ref)?.1)
}

fn model_cache() -> Cache {
    if let Ok(dir) = std::env::var("MERC_MODEL_CACHE") {
        if !dir.is_empty() {
            return Cache::new(PathBuf::from(dir));
        }
    }
    Cache::default()
}

fn api(cache: Cache) -> Result<Api> {
    ApiBuilder::from_cache(cache)
        .build()
        .context("building hf-hub API")
}

pub fn fetch(spec: &ModelSpec) -> Result<Vec<PathBuf>, RunError> {
    fetch_with_policy(
        spec,
        model_cache(),
        ALLOW_MODEL_DOWNLOADS.load(Ordering::Acquire),
        MAX_MODEL_CACHE_BYTES.load(Ordering::Acquire),
    )
}

fn fetch_with_policy(
    spec: &ModelSpec,
    cache: Cache,
    allow_downloads: bool,
    max_cache_bytes: u64,
) -> Result<Vec<PathBuf>, RunError> {
    let repo_id = Repo::with_revision(
        spec.repo.to_string(),
        RepoType::Model,
        spec.revision.to_string(),
    );
    let cached_repo = cache.repo(repo_id.clone());
    let mut cached = Vec::with_capacity(spec.files.len());
    let mut missing_bytes = 0_u64;
    for file in spec.files {
        if let Some(path) = cached_repo.get(file.name) {
            verify_file(&path, file).map_err(|error| RunError::ModelFetch {
                repo: spec.repo.to_string(),
                msg: format!(
                    "verifying cached {} at {}: {error:#}",
                    file.name, spec.revision
                ),
            })?;
            cached.push(Some(path));
        } else {
            cached.push(None);
            missing_bytes = missing_bytes.saturating_add(file.bytes);
        }
    }
    let cache_bytes = dir_size(cache.path());
    if cache_bytes > max_cache_bytes {
        return Err(RunError::ModelFetch {
            repo: spec.repo.to_string(),
            msg: format!(
                "model cache ceiling: {} existing > {} bytes",
                cache_bytes, max_cache_bytes
            ),
        });
    }
    if missing_bytes == 0 {
        return Ok(cached.into_iter().flatten().collect());
    }
    if !allow_downloads {
        return Err(RunError::ModelFetch {
            repo: spec.repo.to_string(),
            msg: format!(
                "{} pinned byte(s) are not cached and model downloads are disabled by the supplier",
                missing_bytes
            ),
        });
    }
    // hf-hub adds refs/pointers alongside the pinned blobs. Reserve one MiB per
    // missing file so the configured ceiling includes metadata, not just model
    // payload bytes.
    let metadata_reserve = (spec.files.len() as u64).saturating_mul(1024 * 1024);
    let required_bytes = missing_bytes.saturating_add(metadata_reserve);
    if cache_bytes.saturating_add(required_bytes) > max_cache_bytes {
        return Err(RunError::ModelFetch {
            repo: spec.repo.to_string(),
            msg: format!(
                "model cache ceiling: {} existing + {} required > {} bytes",
                cache_bytes, required_bytes, max_cache_bytes
            ),
        });
    }

    let api = api(cache).map_err(|error| RunError::ModelFetch {
        repo: spec.repo.to_string(),
        msg: format!("{error:#}"),
    })?;
    let repo = api.repo(repo_id);
    spec.files
        .iter()
        .map(|file| {
            let path = repo.get(file.name).map_err(|error| RunError::ModelFetch {
                repo: spec.repo.to_string(),
                msg: format!("fetching {} at {}: {error}", file.name, spec.revision),
            })?;
            verify_file(&path, file).map_err(|error| RunError::ModelFetch {
                repo: spec.repo.to_string(),
                msg: format!("verifying {} at {}: {error:#}", file.name, spec.revision),
            })?;
            Ok(path)
        })
        .collect()
}

fn dir_size(path: &Path) -> u64 {
    let Ok(entries) = std::fs::read_dir(path) else {
        return 0;
    };
    entries
        .flatten()
        .map(|entry| match entry.metadata() {
            Ok(metadata) if metadata.is_dir() => dir_size(&entry.path()),
            Ok(metadata) => metadata.len(),
            Err(_) => 0,
        })
        .fold(0_u64, u64::saturating_add)
}

fn verify_file(path: &Path, expected: &ModelFile) -> Result<()> {
    let metadata = path
        .metadata()
        .with_context(|| format!("reading metadata for {}", path.display()))?;
    if metadata.len() != expected.bytes {
        anyhow::bail!(
            "size mismatch: got {} bytes, expected {}",
            metadata.len(),
            expected.bytes
        );
    }
    let mut source = File::open(path).with_context(|| format!("opening {}", path.display()))?;
    let mut hasher = Sha256::new();
    let mut buffer = vec![0_u8; 1024 * 1024];
    loop {
        let read = source
            .read(&mut buffer)
            .with_context(|| format!("hashing {}", path.display()))?;
        if read == 0 {
            break;
        }
        hasher.update(&buffer[..read]);
    }
    let actual = format!("{:x}", hasher.finalize());
    if actual != expected.sha256 {
        anyhow::bail!(
            "sha256 mismatch: got {actual}, expected {}",
            expected.sha256
        );
    }
    Ok(())
}

#[cfg(test)]
mod tests {
    use super::*;

    const POLICY_FILE: ModelFile = ModelFile {
        name: "weights.bin",
        sha256: "0000000000000000000000000000000000000000000000000000000000000000",
        bytes: 64,
    };
    const POLICY_SPEC: ModelSpec = ModelSpec {
        repo: "merc/policy-fixture",
        revision: "0123456789abcdef0123456789abcdef01234567",
        files: &[POLICY_FILE],
    };

    fn assert_spec_matches_authority(spec: ModelSpec) {
        let authority: serde_json::Value =
            serde_json::from_str(include_str!("../../control/runtime-authority.json"))
                .expect("runtime authority json");
        let models = authority["models"].as_array().expect("models array");
        let model = models
            .iter()
            .find(|entry| entry["hf_repo"].as_str() == Some(spec.repo))
            .expect("spec repo in runtime authority");
        assert_eq!(model["hf_revision"].as_str(), Some(spec.revision));
        let artifacts = model["artifacts"].as_array().expect("artifacts array");
        for expected in spec.files {
            let actual = artifacts
                .iter()
                .find(|entry| entry["path"].as_str() == Some(expected.name))
                .expect("model artifact in runtime authority");
            assert_eq!(actual["sha256"].as_str(), Some(expected.sha256));
            assert_eq!(actual["bytes"].as_u64(), Some(expected.bytes));
        }
    }

    #[test]
    fn model_file_verification_fails_closed() {
        let path = std::env::temp_dir().join(format!("merc-model-check-{}", std::process::id()));
        std::fs::write(&path, b"pinned model bytes").expect("write fixture");
        let valid = ModelFile {
            name: "fixture",
            sha256: "7826512858078d3d070641414fc406bd3d4f82b60d83968427eb3e5dd7d1377e",
            bytes: 18,
        };
        assert!(verify_file(&path, &valid).is_ok());
        let wrong_digest = ModelFile {
            sha256: "0000000000000000000000000000000000000000000000000000000000000000",
            ..valid
        };
        assert!(verify_file(&path, &wrong_digest).is_err());
        let wrong_size = ModelFile { bytes: 17, ..valid };
        assert!(verify_file(&path, &wrong_size).is_err());
        let _ = std::fs::remove_file(path);
    }

    #[test]
    fn disabled_downloads_and_cache_ceiling_fail_before_network() {
        let path = std::env::temp_dir().join(format!("merc-model-policy-{}", uuid::Uuid::new_v4()));
        let cache = Cache::new(path.clone());
        let disabled = fetch_with_policy(&POLICY_SPEC, cache.clone(), false, u64::MAX)
            .expect_err("missing model must not download");
        assert!(disabled.to_string().contains("downloads are disabled"));

        std::fs::create_dir_all(&path).unwrap();
        std::fs::write(path.join("unrelated-cache-entry"), vec![0_u8; 32]).unwrap();
        let capped = fetch_with_policy(&POLICY_SPEC, cache, true, 95)
            .expect_err("32 existing + 64 required exceeds 95");
        assert!(capped.to_string().contains("model cache ceiling"));
        let _ = std::fs::remove_dir_all(path);
    }

    // The GGUF is pinned on the MiniLM model, not on a repo of its own, so it
    // cannot be located by hf_repo like the others. It is the artifact the
    // llama.cpp embed cell resolves, and swapping it would change every byte
    // that runtime executes.
    #[test]
    fn gguf_embed_pin_matches_runtime_authority() {
        let authority: serde_json::Value =
            serde_json::from_str(include_str!("../../control/runtime-authority.json"))
                .expect("runtime authority json");
        let artifact = authority["models"]
            .as_array()
            .expect("models array")
            .iter()
            .find(|entry| entry["id"].as_str() == Some(EMBED_MINILM_ID))
            .expect("minilm authority")["artifacts"]
            .as_array()
            .expect("artifacts")
            .iter()
            .find(|entry| entry["wire_kind"].as_str() == Some("gguf"))
            .expect("a gguf embed artifact");
        let file = EMBED_GGUF.files[0];
        assert_eq!(artifact["repo"].as_str(), Some(EMBED_GGUF.repo));
        assert_eq!(artifact["revision"].as_str(), Some(EMBED_GGUF.revision));
        assert_eq!(artifact["path"].as_str(), Some(file.name));
        assert_eq!(artifact["sha256"].as_str(), Some(file.sha256));
        assert_eq!(artifact["bytes"].as_u64(), Some(file.bytes));
    }

    #[test]
    fn embed_spec_for_kind_refuses_a_format_with_no_artifact() {
        assert_eq!(embed_spec_for_kind("hf").unwrap().repo, EMBED.repo);
        assert_eq!(embed_spec_for_kind("gguf").unwrap().repo, EMBED_GGUF.repo);
        assert!(embed_spec_for_kind("onnx").is_err());
    }

    #[test]
    fn model_pins_match_runtime_authority() {
        assert_spec_matches_authority(EMBED);
        assert_spec_matches_authority(INFER);

        let authority: serde_json::Value =
            serde_json::from_str(include_str!("../../control/runtime-authority.json"))
                .expect("runtime authority json");
        let llama = authority["models"]
            .as_array()
            .expect("models array")
            .iter()
            .find(|entry| entry["id"].as_str() == Some(INFER_LLAMA_ID))
            .expect("llama authority");
        let token = llama["artifacts"]
            .as_array()
            .expect("artifacts")
            .iter()
            .find(|entry| entry["repo"].as_str() == Some(LLAMA_TOKENIZER.repo))
            .expect("llama tokenizer artifact");
        assert_eq!(token["revision"].as_str(), Some(LLAMA_TOKENIZER.revision));
        assert_eq!(
            token["sha256"].as_str(),
            Some(LLAMA_TOKENIZER.files[0].sha256)
        );
        assert_eq!(
            token["bytes"].as_u64(),
            Some(LLAMA_TOKENIZER.files[0].bytes)
        );
    }
}
