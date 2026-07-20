use std::fs::File;
use std::io::Read;
use std::path::{Path, PathBuf};
use std::sync::OnceLock;

use anyhow::{Context, Result};
use candle_core::Device;
use hf_hub::api::sync::{Api, ApiBuilder};
use hf_hub::{Repo, RepoType};
use sha2::{Digest, Sha256};

use crate::executor::RunError;

static DEVICE: OnceLock<Device> = OnceLock::new();

pub fn device() -> &'static Device {
    DEVICE.get_or_init(|| {
        #[cfg(feature = "metal")]
        match Device::new_metal(0) {
            Ok(device) => {
                tracing::info!("compute device: Metal");
                return device;
            }
            Err(error) => tracing::warn!(%error, "Metal unavailable; using CPU"),
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

pub const LLAMA_TOKENIZER: ModelSpec = ModelSpec {
    repo: "unsloth/Llama-3.2-1B-Instruct",
    revision: "5a8abab4a5d6f164389b1079fb721cfab8d7126c",
    files: &[ModelFile {
        name: "tokenizer.json",
        sha256: "6b9e4e7fb171f92fd137b777cc2714bf87d11576700a1dcd7a399e7bbe39537b",
        bytes: 17_209_920,
    }],
};

pub fn embed_spec(_model_ref: &str) -> (&'static str, ModelSpec) {
    (EMBED_MINILM_ID, EMBED)
}

pub fn llama_gguf_spec(_model_ref: &str) -> ModelSpec {
    INFER
}

fn api() -> Result<Api> {
    let mut builder = ApiBuilder::new();
    if let Ok(dir) = std::env::var("CX_MODEL_CACHE") {
        if !dir.is_empty() {
            builder = builder.with_cache_dir(PathBuf::from(dir));
        }
    }
    builder.build().context("building hf-hub API")
}

pub fn fetch(spec: &ModelSpec) -> Result<Vec<PathBuf>, RunError> {
    let api = api().map_err(|error| RunError::ModelFetch {
        repo: spec.repo.to_string(),
        msg: format!("{error:#}"),
    })?;
    let repo = api.repo(Repo::with_revision(
        spec.repo.to_string(),
        RepoType::Model,
        spec.revision.to_string(),
    ));
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
        let path = std::env::temp_dir().join(format!("cx-model-check-{}", std::process::id()));
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
