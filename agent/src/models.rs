use std::path::PathBuf;
use std::sync::OnceLock;

use anyhow::{Context, Result};
use candle_core::Device;
use hf_hub::api::sync::{Api, ApiBuilder};

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
    pub files: &'static [&'static str],
}

pub const EMBED_MINILM_ID: &str = "all-minilm-l6-v2";
pub const INFER_LLAMA_ID: &str = "llama-3.2-1b-instruct-q4";

const EMBED: ModelSpec = ModelSpec {
    repo: "sentence-transformers/all-MiniLM-L6-v2",
    files: &["config.json", "tokenizer.json", "model.safetensors"],
};

const INFER: ModelSpec = ModelSpec {
    repo: "unsloth/Llama-3.2-1B-Instruct-GGUF",
    files: &["Llama-3.2-1B-Instruct-Q4_K_M.gguf"],
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
    let repo = api.model(spec.repo.to_string());
    spec.files
        .iter()
        .map(|file| {
            repo.get(file).map_err(|error| RunError::ModelFetch {
                repo: spec.repo.to_string(),
                msg: format!("fetching {file}: {error}"),
            })
        })
        .collect()
}
