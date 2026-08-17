use crate::quantized_llama_batched::ModelWeights as QLlama; // patched: bsz>1 batched prefill
use async_trait::async_trait;
use candle_core::quantized::gguf_file;
use candle_core::{Device, IndexOp, Tensor};
use candle_nn::VarBuilder;
use candle_transformers::models::bert::{BertModel, Config as BertConfig, DTYPE as BERT_DTYPE};
use serde::{Deserialize, Serialize};
use tokenizers::Tokenizer;

use crate::deadline::DeadlineError;
use crate::inference::{GenerateParams, InferenceBackend};
use crate::media::MediaTranscodeRunner;
use crate::models;
use crate::pool::ModelPool;
use crate::render::MediaRenderingRunner;
use crate::runtime_driver::RuntimeDriver;
use crate::token_cache::{CacheStats, TokenizationCache};
use crate::types::{JobManifest, JobType, ModelKind, WorkerCapability};

#[derive(Debug, Clone)]
pub struct JobOutput {
    pub result: Vec<u8>,
    pub binary: bool,
    /// Exact object-store content type for this produced artifact. A binary
    /// result is not necessarily opaque bytes: media needs video/mp4 so buyers
    /// and verifiers do not have to infer a type from an untrusted filename.
    pub content_type: &'static str,
    pub duration_ms: u64,
    pub tokens_used: u64,
    /// Which pluggable inference backend produced this result (`candle`, `openai_http`, …).
    /// Empty for non-inference jobs (embed).
    pub inference_backend: String,
    /// Total engine-reported prefix-cache hit size across this task's
    /// completions, when the engine reported it at all.
    ///
    /// `None` means no completion carried the signal, which the control plane
    /// must read as "no observation", not as a miss. `Some(0)` is a real
    /// observed miss and is the value that lets a stale warm belief be
    /// contradicted immediately instead of waiting out its TTL.
    pub cached_prompt_tokens: Option<u64>,
}

#[derive(Debug, thiserror::Error)]
pub enum RunError {
    #[error("no runner can handle job `{job_type}` with model kind `{model_kind}`")]
    NoRunner {
        job_type: String,
        model_kind: String,
    },
    #[error("model fetch from `{repo}` failed: {msg}")]
    ModelFetch { repo: String, msg: String },
    #[error("bad input for `{job}`: {msg}")]
    BadInput { job: &'static str, msg: String },
    #[error("inference error in `{backend}`: {msg}")]
    Inference { backend: &'static str, msg: String },
    #[error("memory pressure preempted `{backend}` mid-job: {msg}")]
    OomPreempt { backend: &'static str, msg: String },
    #[error("{message}")]
    DeadlineExceeded { message: String },
}

impl From<DeadlineError> for RunError {
    fn from(error: DeadlineError) -> Self {
        Self::DeadlineExceeded {
            message: error.to_string(),
        }
    }
}

fn infer_err<E: std::fmt::Display>(backend: &'static str) -> impl Fn(E) -> RunError {
    move |e| RunError::Inference {
        backend,
        msg: e.to_string(),
    }
}

pub const CHECKPOINT_RECORD_BATCH: usize = 32;

const CHECKPOINT_PUT_TIMEOUT: std::time::Duration = std::time::Duration::from_secs(60);

pub fn should_flush(elapsed: std::time::Duration, checkpoint_secs: u64) -> bool {
    checkpoint_secs > 0 && elapsed >= std::time::Duration::from_secs(checkpoint_secs)
}

pub fn partial_document<T: Serialize>(result: &T) -> Result<Vec<u8>, serde_json::Error> {
    let mut v = serde_json::to_value(result)?;
    match v {
        serde_json::Value::Object(ref mut map) => {
            map.insert("partial".to_string(), serde_json::Value::Bool(true));
        }
        _ => {
            use serde::ser::Error;
            return Err(serde_json::Error::custom(
                "partial document must serialize to a JSON object",
            ));
        }
    }
    serde_json::to_vec(&v)
}

#[derive(Clone)]
pub struct Checkpointer {
    pub partial_put_url: Option<String>,
    pub checkpoint_secs: u64,
    pub http: reqwest::Client,
    in_flight: std::sync::Arc<std::sync::atomic::AtomicBool>,
    #[allow(clippy::type_complexity)]
    preempt_check: Option<std::sync::Arc<dyn Fn() -> Option<String> + Send + Sync>>,
}

impl Checkpointer {
    pub fn new(
        partial_put_url: Option<String>,
        checkpoint_secs: u64,
        http: reqwest::Client,
    ) -> Self {
        Self {
            partial_put_url,
            checkpoint_secs,
            http,
            in_flight: std::sync::Arc::new(std::sync::atomic::AtomicBool::new(false)),
            preempt_check: None,
        }
    }

    pub fn disabled() -> Self {
        Self::new(None, 0, reqwest::Client::new())
    }

    pub fn with_preempt_check<F>(mut self, check: F) -> Self
    where
        F: Fn() -> Option<String> + Send + Sync + 'static,
    {
        self.preempt_check = Some(std::sync::Arc::new(check));
        self
    }

    pub fn check_preemption(&self) -> Option<String> {
        self.preempt_check.as_ref().and_then(|f| f())
    }

    pub fn active(&self) -> bool {
        self.partial_put_url.is_some() && self.checkpoint_secs > 0
    }

    pub async fn flush_partial<T: Serialize>(&self, result: &T) {
        let Some(url) = self.partial_put_url.clone() else {
            return;
        };
        let body = match partial_document(result) {
            Ok(b) => b,
            Err(e) => {
                tracing::warn!(error = %e, "checkpoint: partial document did not serialize; skipping this flush");
                return;
            }
        };
        use std::sync::atomic::Ordering;
        if self.in_flight.swap(true, Ordering::AcqRel) {
            tracing::debug!("checkpoint: previous flush still in flight; skipping this tick");
            return;
        }
        let http = self.http.clone();
        let flag = self.in_flight.clone();
        tokio::spawn(async move {
            let sent = http
                .put(&url)
                .header(reqwest::header::CONTENT_TYPE, "application/json")
                .timeout(CHECKPOINT_PUT_TIMEOUT)
                .body(body)
                .send()
                .await
                .and_then(|r| r.error_for_status());
            match sent {
                Ok(_) => tracing::debug!("checkpoint: partial result flushed"),
                Err(e) => tracing::warn!(
                    error = %e,
                    "checkpoint: partial flush failed; continuing (a checkpoint hiccup never fails the task)"
                ),
            }
            flag.store(false, Ordering::Release);
        });
    }
}

fn checkpoint_slice(total: usize, ckpt: &Checkpointer) -> usize {
    if ckpt.active() {
        CHECKPOINT_RECORD_BATCH.min(total.max(1))
    } else {
        total.max(1)
    }
}

#[async_trait]
pub trait JobRunner: Send + Sync {
    async fn can_run(&self, manifest: &JobManifest, cap: &WorkerCapability) -> bool;
    async fn run(
        &self,
        manifest: &JobManifest,
        input: &[u8],
        pool: &ModelPool,
    ) -> Result<JobOutput, RunError>;
    async fn run_with_checkpoints(
        &self,
        manifest: &JobManifest,
        input: &[u8],
        pool: &ModelPool,
        ckpt: &Checkpointer,
    ) -> Result<JobOutput, RunError> {
        let _ = ckpt; // the default lane has no intra-task checkpoints
        self.run(manifest, input, pool).await
    }
    fn backend_name(&self) -> &'static str;
}

fn meets_memory(manifest: &JobManifest, cap: &WorkerCapability) -> bool {
    cap.memory_gb >= manifest.constraints.min_memory_gb
}

#[derive(Debug, Deserialize)]
struct TextItem {
    #[allow(dead_code)]
    id: Option<String>,
    text: Option<String>,
    prompt: Option<String>,
}

impl TextItem {
    fn body(&self) -> Option<&str> {
        self.text.as_deref().or(self.prompt.as_deref())
    }
}

fn parse_jsonl<T: for<'de> Deserialize<'de>>(
    input: &[u8],
    job: &'static str,
) -> Result<Vec<T>, RunError> {
    let text = std::str::from_utf8(input).map_err(|e| RunError::BadInput {
        job,
        msg: format!("input is not UTF-8: {e}"),
    })?;
    let mut out = Vec::new();
    for (i, line) in text.lines().enumerate() {
        let line = line.trim();
        if line.is_empty() {
            continue;
        }
        out.push(serde_json::from_str(line).map_err(|e| RunError::BadInput {
            job,
            msg: format!("line {}: {e}", i + 1),
        })?);
    }
    if out.is_empty() {
        return Err(RunError::BadInput {
            job,
            msg: "no input items".to_string(),
        });
    }
    Ok(out)
}

#[derive(Debug, Serialize)]
pub struct EmbedResult {
    pub job_type: &'static str, // "embed"
    pub model: String,
    pub dim: usize,
    pub count: usize,
    pub vectors: Vec<Vec<f32>>,
}

pub const EMBED_BIN_MAGIC: &[u8; 4] = b"CXEM";
pub const EMBED_BIN_VERSION: u32 = 1;
pub const EMBED_BIN_HEADER: usize = 16;

pub fn encode_embeddings_binary(dim: usize, vectors: &[Vec<f32>]) -> Result<Vec<u8>, RunError> {
    let count = vectors.len();
    let mut out = Vec::with_capacity(EMBED_BIN_HEADER + count * dim * 4);
    out.extend_from_slice(EMBED_BIN_MAGIC);
    out.extend_from_slice(&EMBED_BIN_VERSION.to_le_bytes());
    out.extend_from_slice(&(dim as u32).to_le_bytes());
    out.extend_from_slice(&(count as u32).to_le_bytes());
    for (i, row) in vectors.iter().enumerate() {
        if row.len() != dim {
            return Err(RunError::Inference {
                backend: "embed",
                msg: format!(
                    "binary encode: row {i} has {} floats, expected dim {dim}",
                    row.len()
                ),
            });
        }
        for &f in row {
            out.extend_from_slice(&f.to_le_bytes());
        }
    }
    Ok(out)
}

#[derive(Debug, Clone, Serialize)]
pub struct Completion {
    pub index: usize,
    pub text: String,
    pub tokens: usize,
}

#[derive(Debug, Serialize)]
pub struct BatchInferResult {
    pub job_type: &'static str, // "batch_infer"
    pub model: String,
    /// Which pluggable backend executed this job (`candle` | `openai_http`).
    pub inference_backend: String,
    pub completions: Vec<Completion>,
}

pub const EMBED_DIM: usize = 384;

pub struct Embedder {
    model: BertModel,
    tokenizer: Tokenizer,
    tokenizer_revision: String,
    token_cache: TokenizationCache,
    device: Device,
}

impl Embedder {
    pub fn load(model_ref: &str) -> Result<Self, RunError> {
        let (_id, spec) = models::embed_spec(model_ref);
        let paths = models::fetch(&spec)?;
        let (config_p, tok_p, weights_p) = (&paths[0], &paths[1], &paths[2]);

        let cfg_bytes = std::fs::read(config_p).map_err(infer_err("embed"))?;
        let config: BertConfig = serde_json::from_slice(&cfg_bytes).map_err(infer_err("embed"))?;
        let mut tokenizer = Tokenizer::from_file(tok_p).map_err(infer_err("embed"))?;
        let pad = tokenizers::PaddingParams::default();
        tokenizer.with_padding(Some(pad));

        let device = models::device().clone();
        let vb = unsafe {
            VarBuilder::from_mmaped_safetensors(&[weights_p], BERT_DTYPE, &device)
                .map_err(infer_err("embed"))?
        };
        let model = BertModel::load(vb, &config).map_err(infer_err("embed"))?;
        Ok(Self {
            model,
            tokenizer,
            tokenizer_revision: spec.revision.to_string(),
            token_cache: TokenizationCache::default(),
            device,
        })
    }

    /// Cache diagnostics are local execution evidence only. They never alter
    /// the control-plane unit count or settlement authority.
    pub fn token_cache_stats(&self) -> CacheStats {
        self.token_cache.stats()
    }

    pub fn embed(&self, texts: &[String]) -> Result<Vec<Vec<f32>>, RunError> {
        if texts.is_empty() {
            return Ok(Vec::new());
        }
        let backend = "embed";
        let mut encs = Vec::with_capacity(texts.len());
        for text in texts {
            let key = TokenizationCache::key(&self.tokenizer_revision, text);
            if let Some(cached) = self.token_cache.get(&key) {
                encs.push(cached);
                continue;
            }
            let encoded = self
                .tokenizer
                .encode(text.as_str(), true)
                .map_err(infer_err(backend))?;
            self.token_cache.insert(key, encoded.clone());
            encs.push(encoded);
        }

        let mut buckets: std::collections::HashMap<usize, Vec<usize>> =
            std::collections::HashMap::new();
        for (i, e) in encs.iter().enumerate() {
            buckets.entry(e.get_ids().len()).or_default().push(i);
        }

        let mut out: Vec<Vec<f32>> = vec![Vec::new(); texts.len()];
        for members in buckets.values() {
            let bucket_encs: Vec<&tokenizers::Encoding> =
                members.iter().map(|&i| &encs[i]).collect();
            let vectors = self.embed_bucket(&bucket_encs)?;
            for (b, &orig) in members.iter().enumerate() {
                out[orig] = vectors[b].clone();
            }
        }
        Ok(out)
    }

    fn embed_bucket(&self, encs: &[&tokenizers::Encoding]) -> Result<Vec<Vec<f32>>, RunError> {
        let backend = "embed";
        let (bsz, seq) = (encs.len(), encs[0].len());
        let mut ids = Vec::with_capacity(bsz * seq);
        let mut mask = Vec::with_capacity(bsz * seq);
        for e in encs {
            ids.extend(e.get_ids().iter().map(|&x| x as i64));
            mask.extend(e.get_attention_mask().iter().map(|&x| x as f32));
        }

        let input_ids =
            Tensor::from_vec(ids, (bsz, seq), &self.device).map_err(infer_err(backend))?;
        let token_type = input_ids.zeros_like().map_err(infer_err(backend))?;
        let attn = Tensor::from_vec(mask, (bsz, seq), &self.device).map_err(infer_err(backend))?;

        let hidden = self
            .model
            .forward(&input_ids, &token_type, Some(&attn))
            .map_err(infer_err(backend))?;

        let mask3 = attn
            .unsqueeze(2)
            .map_err(infer_err(backend))?
            .to_dtype(hidden.dtype())
            .map_err(infer_err(backend))?;
        let summed = hidden
            .broadcast_mul(&mask3)
            .map_err(infer_err(backend))?
            .sum(1)
            .map_err(infer_err(backend))?;
        let counts = mask3.sum(1).map_err(infer_err(backend))?;
        let pooled = summed.broadcast_div(&counts).map_err(infer_err(backend))?;
        let norm = pooled
            .sqr()
            .map_err(infer_err(backend))?
            .sum_keepdim(1)
            .map_err(infer_err(backend))?
            .sqrt()
            .map_err(infer_err(backend))?;
        let normed = pooled.broadcast_div(&norm).map_err(infer_err(backend))?;
        normed.to_vec2::<f32>().map_err(infer_err(backend))
    }
}

/// The embed lane, behind the runtime driver boundary.
///
/// The runner used to call `pool.embedder()` directly, which hardwired embedding
/// to Candle: a llama.cpp worker had no way to serve the cell it is registered
/// for. It now holds whichever driver this agent was configured with, and asks
/// that driver whether it can serve the cell rather than testing the model kind
/// itself — `hf` versus `gguf` is the driver's business, not the runner's.
pub struct EmbedRunner {
    driver: std::sync::Arc<dyn RuntimeDriver>,
}

impl EmbedRunner {
    pub fn new(driver: std::sync::Arc<dyn RuntimeDriver>) -> Self {
        Self { driver }
    }
}

#[async_trait]
impl JobRunner for EmbedRunner {
    async fn can_run(&self, manifest: &JobManifest, cap: &WorkerCapability) -> bool {
        matches!(manifest.job_type, JobType::Embed { .. })
            && self.driver.serves_kind(manifest.model.kind.as_wire_str())
            && meets_memory(manifest, cap)
    }

    async fn run(
        &self,
        manifest: &JobManifest,
        input: &[u8],
        pool: &ModelPool,
    ) -> Result<JobOutput, RunError> {
        let started = std::time::Instant::now();
        let items: Vec<TextItem> = parse_jsonl(input, "embed")?;
        let texts: Vec<String> = items
            .iter()
            .map(|it| it.body().unwrap_or("").to_string())
            .collect();
        let count = texts.len();

        let vectors = self
            .driver
            .embed(&manifest.model.model_ref, &texts, pool)
            .await?;

        let binary = wants_binary(manifest);
        let (bytes, is_binary) = if binary {
            (encode_embeddings_binary(EMBED_DIM, &vectors)?, true)
        } else {
            if count >= EMBED_BINARY_ROW_HINT {
                tracing::info!(
                    rows = count,
                    hint = EMBED_BINARY_ROW_HINT,
                    "embed: large output emitted as JSON; set job_type.binary=true for the compact float32 artifact (PLANE_D D5)"
                );
            }
            let result = EmbedResult {
                job_type: "embed",
                model: short_model_id(&manifest.model.model_ref, "all-minilm-l6-v2"),
                dim: EMBED_DIM,
                count,
                vectors,
            };
            (
                serde_json::to_vec(&result).map_err(infer_err("embed"))?,
                false,
            )
        };
        Ok(JobOutput {
            result: bytes,
            binary: is_binary,
            content_type: if is_binary {
                "application/octet-stream"
            } else {
                "application/json"
            },
            duration_ms: started.elapsed().as_millis() as u64,
            tokens_used: count as u64,
            inference_backend: String::new(),
            // Embed has no prompt cache to observe.
            cached_prompt_tokens: None,
        })
    }

    fn backend_name(&self) -> &'static str {
        "embed"
    }
}

pub const EMBED_BINARY_ROW_HINT: usize = 256;

fn wants_binary(manifest: &JobManifest) -> bool {
    if let JobType::Embed { binary, .. } = manifest.job_type {
        if binary {
            return true;
        }
    }
    manifest
        .params
        .get("embed_binary")
        .and_then(|v| v.as_bool())
        .unwrap_or(false)
}

const PAD_BUCKET: usize = 16;

pub struct LlamaBackend {
    model: QLlama,
    tokenizer: Tokenizer,
    eos: u32,
    device: Device,
}

impl LlamaBackend {
    pub fn load(model_ref: &str) -> Result<Self, RunError> {
        let spec = models::llama_gguf_spec(model_ref)?;
        let paths = models::fetch(&spec)?;
        let gguf_p = &paths[0];

        let device = models::device().clone();
        let mut file = std::fs::File::open(gguf_p).map_err(infer_err("batch_infer"))?;
        let content = gguf_file::Content::read(&mut file).map_err(infer_err("batch_infer"))?;

        let eos = content
            .metadata
            .get("tokenizer.ggml.eos_token_id")
            .and_then(|v| v.to_u32().ok())
            .unwrap_or(2);
        let model =
            QLlama::from_gguf(content, &mut file, &device).map_err(infer_err("batch_infer"))?;

        let tokenizer = load_llama_tokenizer(model_ref)?;
        Ok(Self {
            model,
            tokenizer,
            eos,
            device,
        })
    }

    pub fn generate(&mut self, prompt: &str, max_tokens: u32) -> Result<(String, usize), RunError> {
        self.generate_greedy(prompt, max_tokens)
    }

    /// Greedy decode that invokes `on_token` after each decoded piece.
    ///
    /// Used by the OpenAI-HTTP shim so serving-matrix TTFT/ITL samples the real
    /// token stream rather than a full-completion wall time.
    pub fn generate_greedy_streaming<F>(
        &mut self,
        prompt: &str,
        max_tokens: u32,
        mut on_token: F,
    ) -> Result<(String, usize), RunError>
    where
        F: FnMut(&str) -> Result<(), RunError>,
    {
        let backend = "batch_infer";
        let wrapped = format!(
            "<|begin_of_text|><|start_header_id|>user<|end_header_id|>\n\n{prompt}<|eot_id|>\
             <|start_header_id|>assistant<|end_header_id|>\n\n"
        );
        let enc = self
            .tokenizer
            .encode(wrapped, true)
            .map_err(infer_err(backend))?;
        let mut tokens: Vec<u32> = enc.get_ids().to_vec();

        let mut generated: Vec<u32> = Vec::new();
        let mut index_pos = 0usize;
        for step in 0..max_tokens as usize {
            let ctx = if step == 0 {
                &tokens[..]
            } else {
                &tokens[tokens.len() - 1..]
            };
            let input = Tensor::new(ctx, &self.device)
                .map_err(infer_err(backend))?
                .unsqueeze(0)
                .map_err(infer_err(backend))?;
            let logits = self
                .model
                .forward(&input, index_pos)
                .map_err(infer_err(backend))?;
            let logits = logits.squeeze(0).map_err(infer_err(backend))?;
            let last = if logits.rank() == 2 {
                let s = logits.dim(0).map_err(infer_err(backend))?;
                logits.get(s - 1).map_err(infer_err(backend))?
            } else {
                logits
            };
            let next = last
                .argmax(0)
                .map_err(infer_err(backend))?
                .to_scalar::<u32>()
                .map_err(infer_err(backend))?;
            index_pos += ctx.len();
            if next == self.eos {
                break;
            }
            tokens.push(next);
            generated.push(next);
            // Decode only the new id so the client sees incremental pieces
            // (OpenAI SSE shape). Piece may be empty for multi-byte UTF-8 spans.
            let piece = self
                .tokenizer
                .decode(&[next], true)
                .map_err(infer_err(backend))?;
            if !piece.is_empty() {
                on_token(&piece)?;
            }
        }

        let text = self
            .tokenizer
            .decode(&generated, true)
            .map_err(infer_err(backend))?;
        Ok((text.trim().to_string(), generated.len()))
    }

    fn generate_greedy(
        &mut self,
        prompt: &str,
        max_tokens: u32,
    ) -> Result<(String, usize), RunError> {
        self.generate_greedy_streaming(prompt, max_tokens, |_| Ok(()))
    }

    pub fn generate_batch(
        &mut self,
        prompts: &[String],
        max_tokens: u32,
    ) -> Result<Vec<(String, usize)>, RunError> {
        let backend = "batch_infer";
        let mut encoded: Vec<Vec<u32>> = Vec::with_capacity(prompts.len());
        for p in prompts {
            let wrapped = format!(
                "<|begin_of_text|><|start_header_id|>user<|end_header_id|>\n\n{p}<|eot_id|>\
                 <|start_header_id|>assistant<|end_header_id|>\n\n"
            );
            let enc = self
                .tokenizer
                .encode(wrapped, true)
                .map_err(infer_err(backend))?;
            encoded.push(enc.get_ids().to_vec());
        }
        let mut buckets: std::collections::HashMap<usize, Vec<usize>> =
            std::collections::HashMap::new();
        for (i, ids) in encoded.iter().enumerate() {
            buckets.entry(ids.len()).or_default().push(i);
        }
        let mut out: Vec<(String, usize)> = vec![(String::new(), 0); prompts.len()];
        let kv_bytes_per_token = self.model.kv_bytes_per_token_per_row();
        let mut singletons: Vec<usize> = Vec::new();
        for members in buckets.values() {
            if members.len() == 1 {
                singletons.push(members[0]);
                continue;
            }
            let plen = encoded[members[0]].len();
            let available_gb = crate::hardware::read_memory_snapshot().available_gb;
            let width_cap =
                batch_width_cap(kv_bytes_per_token, plen, max_tokens as usize, available_gb);
            for members in members.chunks(width_cap) {
                if members.len() == 1 {
                    singletons.push(members[0]);
                    continue;
                }
                let bsz = members.len();
                let plen = encoded[members[0]].len();
                let seq_cap =
                    (plen + max_tokens as usize).min(crate::quantized_llama_batched::MAX_SEQ_LEN);
                self.model.set_next_seq_cap(Some(seq_cap));
                let mut gen: Vec<Vec<u32>> = vec![Vec::new(); bsz];
                let mut active: Vec<usize> = (0..bsz).collect();
                let mut active_last: Vec<u32> = vec![0u32; bsz];
                let mut index_pos = 0usize;
                for step in 0..max_tokens as usize {
                    if active.is_empty() {
                        break;
                    }
                    let abz = active.len();
                    let (rows, seq_len) = if step == 0 {
                        let mut flat = Vec::with_capacity(bsz * plen);
                        for &m in members {
                            flat.extend_from_slice(&encoded[m]);
                        }
                        (flat, plen)
                    } else {
                        (active_last.clone(), 1usize)
                    };
                    let input = Tensor::from_vec(rows, (abz, seq_len), &self.device)
                        .map_err(infer_err(backend))?;
                    let logits = self
                        .model
                        .forward(&input, index_pos)
                        .map_err(infer_err(backend))?; // (abz, vocab) · last position only
                    index_pos += seq_len;
                    let next: Vec<u32> = logits
                        .argmax(1)
                        .map_err(infer_err(backend))?
                        .to_vec1::<u32>()
                        .map_err(infer_err(backend))?;
                    let mut keep: Vec<usize> = Vec::with_capacity(abz);
                    let mut new_active: Vec<usize> = Vec::with_capacity(abz);
                    let mut new_last: Vec<u32> = Vec::with_capacity(abz);
                    for (a, &b) in active.iter().enumerate() {
                        let t = next[a];
                        if t == self.eos {
                            continue; // sequence b is finished; drop it
                        }
                        gen[b].push(t);
                        keep.push(a);
                        new_active.push(b);
                        new_last.push(t);
                    }
                    if keep.len() != abz {
                        self.model
                            .compact_kv_cache(&keep)
                            .map_err(infer_err(backend))?;
                    }
                    active = new_active;
                    active_last = new_last;
                }
                for (b, &m) in members.iter().enumerate() {
                    let text = self
                        .tokenizer
                        .decode(&gen[b], true)
                        .map_err(infer_err(backend))?;
                    out[m] = (text.trim().to_string(), gen[b].len());
                }
            }
        }
        if !singletons.is_empty() {
            singletons.sort_unstable();
            let mut bands: std::collections::BTreeMap<usize, Vec<usize>> =
                std::collections::BTreeMap::new();
            for &i in &singletons {
                let band = encoded[i].len().div_ceil(PAD_BUCKET);
                bands.entry(band).or_default().push(i);
            }
            for members in bands.values() {
                if members.len() == 1 {
                    let m = members[0];
                    let (text, n) = self.generate(&prompts[m], max_tokens)?;
                    out[m] = (text, n);
                    continue;
                }
                let pad_len = members.iter().map(|&m| encoded[m].len()).max().unwrap();
                let available_gb = crate::hardware::read_memory_snapshot().available_gb;
                let width_cap = batch_width_cap(
                    kv_bytes_per_token,
                    pad_len,
                    max_tokens as usize,
                    available_gb,
                );
                for members in members.chunks(width_cap) {
                    if members.len() == 1 {
                        let m = members[0];
                        let (text, n) = self.generate(&prompts[m], max_tokens)?;
                        out[m] = (text, n);
                        continue;
                    }
                    let results = self.generate_padded_bucket(&encoded, members, max_tokens)?;
                    for (&m, (text, n)) in members.iter().zip(results) {
                        out[m] = (text, n);
                    }
                }
            }
        }
        Ok(out)
    }

    fn generate_padded_bucket(
        &mut self,
        encoded: &[Vec<u32>],
        members: &[usize],
        max_tokens: u32,
    ) -> Result<Vec<(String, usize)>, RunError> {
        let backend = "batch_infer";
        let bsz = members.len();
        let real_len: Vec<usize> = members.iter().map(|&m| encoded[m].len()).collect();
        let pad_len = *real_len.iter().max().unwrap();
        let seq_cap =
            (pad_len + max_tokens as usize).min(crate::quantized_llama_batched::MAX_SEQ_LEN);
        self.model.set_next_seq_cap(Some(seq_cap));

        let mut flat: Vec<u32> = Vec::with_capacity(bsz * pad_len);
        for &m in members {
            let ids = &encoded[m];
            flat.extend_from_slice(ids);
            let filler = *ids.last().unwrap();
            for _ in ids.len()..pad_len {
                flat.push(filler);
            }
        }
        let input =
            Tensor::from_vec(flat, (bsz, pad_len), &self.device).map_err(infer_err(backend))?;
        let prefill_pos: Vec<Vec<usize>> = (0..bsz).map(|_| (0..pad_len).collect()).collect();
        let pos_flat: Vec<u32> = prefill_pos
            .iter()
            .flat_map(|r| r.iter().map(|&p| p as u32))
            .collect();
        let positions =
            Tensor::from_vec(pos_flat, (bsz, pad_len), &self.device).map_err(infer_err(backend))?;
        let mask = crate::quantized_llama_batched::build_padded_mask(
            &real_len,
            &prefill_pos,
            0,
            pad_len,
            &self.device,
        )
        .map_err(infer_err(backend))?;
        let logits = self
            .model
            .forward_padded(&input, &positions, &mask, true, seq_cap)
            .map_err(infer_err(backend))?; // (bsz, pad_len, vocab)

        let mut gen: Vec<Vec<u32>> = vec![Vec::new(); bsz];
        let mut active: Vec<usize> = Vec::with_capacity(bsz);
        let mut active_last: Vec<u32> = Vec::with_capacity(bsz);
        let mut active_pos: Vec<usize> = Vec::with_capacity(bsz);
        for r in 0..bsz {
            let last_row = logits
                .i((r, real_len[r] - 1, ..))
                .map_err(infer_err(backend))?;
            let t = last_row
                .argmax(0)
                .map_err(infer_err(backend))?
                .to_scalar::<u32>()
                .map_err(infer_err(backend))?;
            if t == self.eos {
                continue; // row r produced nothing
            }
            gen[r].push(t);
            active.push(r);
            active_last.push(t);
            active_pos.push(real_len[r]);
        }

        let mut active_real0: Vec<usize> = active.iter().map(|&r| real_len[r]).collect();
        for cached_len in (pad_len..).take((max_tokens as usize).saturating_sub(1)) {
            if active.is_empty() {
                break;
            }
            let abz = active.len();
            let mask = self
                .padded_decode_mask(&active_real0, cached_len, pad_len)
                .map_err(infer_err(backend))?;
            let input = Tensor::from_vec(active_last.clone(), (abz, 1), &self.device)
                .map_err(infer_err(backend))?;
            let pos_flat: Vec<u32> = active_pos.iter().map(|&p| p as u32).collect();
            let positions =
                Tensor::from_vec(pos_flat, (abz, 1), &self.device).map_err(infer_err(backend))?;
            let logits = self
                .model
                .forward_padded(&input, &positions, &mask, false, seq_cap)
                .map_err(infer_err(backend))?; // (abz, 1, vocab)
            let next: Vec<u32> = logits
                .i((.., 0, ..))
                .map_err(infer_err(backend))?
                .argmax(1)
                .map_err(infer_err(backend))?
                .to_vec1::<u32>()
                .map_err(infer_err(backend))?;
            let mut keep: Vec<usize> = Vec::with_capacity(abz);
            let mut new_active: Vec<usize> = Vec::with_capacity(abz);
            let mut new_last: Vec<u32> = Vec::with_capacity(abz);
            let mut new_pos: Vec<usize> = Vec::with_capacity(abz);
            let mut new_real0: Vec<usize> = Vec::with_capacity(abz);
            for (a, &r) in active.iter().enumerate() {
                let t = next[a];
                if t == self.eos {
                    continue;
                }
                gen[r].push(t);
                keep.push(a);
                new_active.push(r);
                new_last.push(t);
                new_pos.push(active_pos[a] + 1);
                new_real0.push(active_real0[a]);
            }
            if keep.len() != abz {
                self.model
                    .compact_kv_cache(&keep)
                    .map_err(infer_err(backend))?;
            }
            active = new_active;
            active_last = new_last;
            active_pos = new_pos;
            active_real0 = new_real0;
        }

        let mut out: Vec<(String, usize)> = Vec::with_capacity(bsz);
        for row in &gen {
            let text = self
                .tokenizer
                .decode(row, true)
                .map_err(infer_err(backend))?;
            out.push((text.trim().to_string(), row.len()));
        }
        Ok(out)
    }

    fn padded_decode_mask(
        &self,
        real0: &[usize],
        cached_len: usize,
        pad_len: usize,
    ) -> Result<Tensor, candle_core::Error> {
        let abz = real0.len();
        let kv_len = cached_len + 1;
        let mut data: Vec<u8> = Vec::with_capacity(abz * kv_len);
        for &r0 in real0 {
            for j in 0..kv_len {
                let forbidden = j >= r0 && j < pad_len;
                data.push(u8::from(forbidden));
            }
        }
        Tensor::from_vec(data, (abz, 1, 1, kv_len), &self.device)
    }
}

fn load_llama_tokenizer(_model_ref: &str) -> Result<Tokenizer, RunError> {
    let paths = models::fetch(&models::LLAMA_TOKENIZER)?;
    Tokenizer::from_file(&paths[0]).map_err(infer_err("batch_infer"))
}

pub fn short_model_id(model_ref: &str, fallback: &str) -> String {
    if model_ref.trim().is_empty() {
        fallback.to_string()
    } else {
        model_ref
            .rsplit('/')
            .next()
            .unwrap_or(model_ref)
            .to_ascii_lowercase()
    }
}

pub struct BatchInferRunner {
    pub inference: std::sync::Arc<dyn InferenceBackend>,
}

#[async_trait]
impl JobRunner for BatchInferRunner {
    async fn can_run(&self, manifest: &JobManifest, cap: &WorkerCapability) -> bool {
        matches!(manifest.job_type, JobType::BatchInfer { .. })
            && manifest.model.kind == ModelKind::Gguf
            && meets_memory(manifest, cap)
    }

    async fn run(
        &self,
        manifest: &JobManifest,
        input: &[u8],
        pool: &ModelPool,
    ) -> Result<JobOutput, RunError> {
        self.run_with_checkpoints(manifest, input, pool, &Checkpointer::disabled())
            .await
    }

    async fn run_with_checkpoints(
        &self,
        manifest: &JobManifest,
        input: &[u8],
        pool: &ModelPool,
        ckpt: &Checkpointer,
    ) -> Result<JobOutput, RunError> {
        let started = std::time::Instant::now();
        let (max_tokens, temperature) = match &manifest.job_type {
            JobType::BatchInfer {
                max_tokens,
                temperature,
            } => (*max_tokens, *temperature),
            _ => (256, 0.0),
        };
        if temperature != 0.0 {
            return Err(RunError::Inference {
                backend: "batch_infer",
                msg: format!(
                    "non-zero temperature {temperature} rejected: batch lane is greedy-only"
                ),
            });
        }
        let params = GenerateParams::greedy(max_tokens);
        let backend_name = self.inference.name().to_string();
        let items: Vec<TextItem> = parse_jsonl(input, "batch_infer")?;
        let prompts: Vec<String> = items
            .iter()
            .map(|it| it.body().unwrap_or("").to_string())
            .collect();

        let slice = checkpoint_slice(prompts.len(), ckpt);
        let mut completions: Vec<Completion> = Vec::with_capacity(prompts.len());
        let mut total_tokens: usize = 0;
        let mut cached_prompt_tokens: Option<u64> = None;
        let mut last_flush = std::time::Instant::now();
        clear_live_throttle();
        let mut live_monitor = LiveThroughputMonitor::new();
        for chunk in prompts.chunks(slice) {
            if let Some(reason) = ckpt.check_preemption() {
                if !completions.is_empty() {
                    let partial = BatchInferResult {
                        job_type: "batch_infer",
                        model: short_model_id(
                            &manifest.model.model_ref,
                            "llama-3.2-1b-instruct-q4",
                        ),
                        inference_backend: backend_name.clone(),
                        completions: completions.clone(),
                    };
                    ckpt.flush_partial(&partial).await;
                }
                tracing::warn!(
                    job_type = "batch_infer",
                    reason = %reason,
                    completed = completions.len(),
                    total = prompts.len(),
                    "memory pressure preempted job mid-run; stopping before next slice"
                );
                return Err(RunError::OomPreempt {
                    backend: "batch_infer",
                    msg: reason,
                });
            }
            let slice_started = std::time::Instant::now();
            let results = self
                .inference
                .generate_batch(&manifest.model.model_ref, chunk, params, pool)
                .await?;
            let slice_dt = slice_started.elapsed().as_secs_f64().max(1e-6);
            let mut slice_tokens: usize = 0;
            for c in results {
                total_tokens += c.tokens;
                slice_tokens += c.tokens;
                // Sum only over completions that actually carried the signal, so
                // a batch where the engine said nothing stays None rather than
                // becoming an observed zero. A mixed batch reports what was seen.
                if let Some(n) = c.cached_prompt_tokens {
                    cached_prompt_tokens = Some(cached_prompt_tokens.unwrap_or(0) + n);
                }
                completions.push(Completion {
                    index: completions.len(),
                    text: c.text,
                    tokens: c.tokens,
                });
            }
            if live_monitor.record(slice_tokens as f64 / slice_dt) {
                tracing::warn!(
                    job_type = "batch_infer",
                    tokens_per_s = slice_tokens as f64 / slice_dt,
                    "live throttle detected: sustained throughput drop mid-task"
                );
                set_live_throttle_detected();
            }
            if completions.len() < prompts.len()
                && should_flush(last_flush.elapsed(), ckpt.checkpoint_secs)
            {
                let partial = BatchInferResult {
                    job_type: "batch_infer",
                    model: short_model_id(&manifest.model.model_ref, "llama-3.2-1b-instruct-q4"),
                    inference_backend: backend_name.clone(),
                    completions: completions.clone(),
                };
                ckpt.flush_partial(&partial).await;
                last_flush = std::time::Instant::now();
            }
        }

        let result = BatchInferResult {
            job_type: "batch_infer",
            model: short_model_id(&manifest.model.model_ref, "llama-3.2-1b-instruct-q4"),
            inference_backend: backend_name.clone(),
            completions,
        };
        let bytes = serde_json::to_vec(&result).map_err(infer_err("batch_infer"))?;
        Ok(JobOutput {
            result: bytes,
            binary: false,
            content_type: "application/json",
            duration_ms: started.elapsed().as_millis() as u64,
            tokens_used: total_tokens as u64,
            inference_backend: backend_name,
            cached_prompt_tokens,
        })
    }

    fn backend_name(&self) -> &'static str {
        "batch_infer"
    }
}

pub fn default_runners(
    inference: std::sync::Arc<dyn InferenceBackend>,
    embed_driver: std::sync::Arc<dyn RuntimeDriver>,
) -> Vec<Box<dyn JobRunner>> {
    vec![
        Box::new(EmbedRunner::new(embed_driver)),
        Box::new(BatchInferRunner { inference }),
        // This runner has no advertised control-plane cell yet. It is present
        // so the agent implementation is exercised as a real process runner,
        // while runtime authority keeps ordinary buyer work out of it.
        Box::new(MediaTranscodeRunner::from_environment()),
        Box::new(MediaRenderingRunner),
    ]
}

pub async fn dispatch<'a>(
    manifest: &JobManifest,
    cap: &WorkerCapability,
    runners: &'a [Box<dyn JobRunner>],
) -> Result<&'a dyn JobRunner, RunError> {
    for r in runners {
        if r.can_run(manifest, cap).await {
            return Ok(r.as_ref());
        }
    }
    Err(RunError::NoRunner {
        job_type: manifest.job_type.tag().to_string(),
        model_kind: format!("{:?}", manifest.model.kind),
    })
}

use crate::types::BenchResult;

const THERMAL_SECS: u64 = 20;
const LATENCY_ITERS: usize = 12;

fn batch_width_cap(
    kv_bytes_per_token: usize,
    plen: usize,
    max_tokens: usize,
    available_gb: f32,
) -> usize {
    if kv_bytes_per_token == 0 || available_gb <= 0.0 {
        return usize::MAX;
    }
    let per_row_kv_bytes =
        kv_bytes_per_token * (plen + max_tokens).min(crate::quantized_llama_batched::MAX_SEQ_LEN);
    if per_row_kv_bytes == 0 {
        return usize::MAX;
    }
    let effective_bytes = (available_gb as f64 * 0.5 * 1e9) as usize;
    (effective_bytes / per_row_kv_bytes).max(1)
}

fn percentile_ms(mut samples: Vec<f64>, pct: f64) -> u32 {
    if samples.is_empty() {
        return 0;
    }
    samples.sort_by(|a, b| a.partial_cmp(b).unwrap());
    let rank = ((pct / 100.0) * samples.len() as f64).ceil() as usize;
    let idx = rank.saturating_sub(1).min(samples.len() - 1);
    samples[idx].round() as u32
}

pub async fn run_benchmarks(pool: &crate::pool::ModelPool, _memory_gb: f32) -> Vec<BenchResult> {
    let mut out = Vec::new();
    match bench_embed(pool).await {
        Ok(b) => out.push(b),
        Err(e) => tracing::warn!(error = %e, "embed benchmark unavailable (model load failed)"),
    }
    match bench_llama(pool).await {
        Ok(b) => out.push(b),
        Err(e) => {
            tracing::warn!(error = %e, "llama (1B) benchmark unavailable (model load failed)")
        }
    }
    let media = MediaTranscodeRunner::from_environment();
    match media.benchmark().await {
        Ok(b) => out.push(b),
        Err(e) => tracing::warn!(error = %e, "media transcode benchmark unavailable"),
    }
    let rendering = MediaRenderingRunner;
    match rendering.benchmark().await {
        Ok(b) => out.push(b),
        Err(e) => tracing::warn!(error = %e, "media rendering benchmark unavailable"),
    }
    out
}

async fn bench_embed(pool: &crate::pool::ModelPool) -> Result<BenchResult, RunError> {
    let load_started = std::time::Instant::now();
    let embedder = pool.embedder("").await?;
    let load_ms = load_started.elapsed().as_millis() as u64;
    let batch: Vec<String> = (0..8)
        .map(|i| format!("benchmark sentence number {i} for throughput measurement"))
        .collect();

    let embedder = embedder.lock().await;

    embedder.embed(&batch)?;

    let mut lat = Vec::with_capacity(LATENCY_ITERS);
    for _ in 0..LATENCY_ITERS {
        let t = std::time::Instant::now();
        embedder.embed(&batch)?;
        lat.push(t.elapsed().as_secs_f64() * 1000.0);
    }
    let p99_ms = percentile_ms(lat, 99.0);

    let (eps, thermal_ok) = sustained_eps(&embedder, &batch)?;
    let cache = embedder.token_cache_stats();
    tracing::info!(
        model = "all-minilm-l6-v2",
        load_ms,
        token_cache_hits = cache.hits,
        token_cache_misses = cache.misses,
        token_cache_entries = cache.entries,
        "measured cold model load"
    );
    Ok(BenchResult {
        model_id: "all-minilm-l6-v2".to_string(),
        job_type: "embed".to_string(),
        tps: 0.0,
        eps,
        p99_ms,
        thermal_ok,
        load_ms,
        unit: "embeddings".to_string(),
        unit_scope: "completed_embedding_records".to_string(),
        measured_unix: 0,
    })
}

fn sustained_eps(embedder: &Embedder, batch: &[String]) -> Result<(f32, bool), RunError> {
    let n = batch.len() as f64;
    sustained_throughput(|| {
        let t = std::time::Instant::now();
        embedder.embed(batch)?;
        Ok(n / t.elapsed().as_secs_f64().max(1e-6))
    })
}

fn sustained_throughput(
    mut step: impl FnMut() -> Result<f64, RunError>,
) -> Result<(f32, bool), RunError> {
    let secs = THERMAL_SECS as f64;
    let edge = secs * 0.25;
    let start = std::time::Instant::now();
    let deadline = start + std::time::Duration::from_secs(THERMAL_SECS);
    let mut peak = 0.0f64;
    let mut samples = Vec::new();
    let (mut early_sum, mut early_n) = (0.0f64, 0u32);
    let (mut late_sum, mut late_n) = (0.0f64, 0u32);
    while std::time::Instant::now() < deadline {
        let thr = step()?;
        peak = peak.max(thr);
        samples.push(thr);
        let since = start.elapsed().as_secs_f64();
        if since < edge {
            early_sum += thr;
            early_n += 1;
        } else if since >= secs - edge {
            late_sum += thr;
            late_n += 1;
        }
    }
    let thermal_ok = if early_n == 0 || late_n == 0 {
        true
    } else {
        (late_sum / late_n as f64) >= (early_sum / early_n as f64) * 0.85
    };
    let conservative =
        conservative_sustained_rate(&samples).ok_or_else(|| RunError::Inference {
            backend: "startup_benchmark",
            msg: "sustained benchmark produced no finite positive throughput samples".into(),
        })?;
    tracing::info!(
        peak_units_per_sec = peak,
        conservative_units_per_sec = conservative,
        "startup benchmark retains peak as diagnostics and advertises the haircutted low-tail sustained rate"
    );
    Ok((conservative as f32, thermal_ok))
}

// The worker rate is an admission credential, not a dashboard peak. Use the
// lower decile across the full thermal window and retain an additional 5%
// measurement margin. One fast iteration therefore cannot make a slower
// steady-state worker eligible for a centrally governed active-hour floor.
fn conservative_sustained_rate(samples: &[f64]) -> Option<f64> {
    if samples.is_empty()
        || samples
            .iter()
            .any(|sample| !sample.is_finite() || *sample <= 0.0)
    {
        return None;
    }
    let mut ordered = samples.to_vec();
    ordered.sort_by(f64::total_cmp);
    let low_decile_index = (ordered.len() - 1) / 10;
    Some(ordered[low_decile_index] * 0.95)
}

const LIVE_BASELINE_SLICES: usize = 2;

const LIVE_THROTTLE_RATIO: f64 = 0.75;

const LIVE_MIN_DROP_SLICES: usize = 3;

#[derive(Debug, Default, Clone)]
pub struct LiveThroughputMonitor {
    baseline: Option<f64>,
    baseline_samples: Vec<f64>,
    consecutive_low: usize,
}

impl LiveThroughputMonitor {
    pub fn new() -> Self {
        Self::default()
    }

    pub fn record(&mut self, tokens_per_sec: f64) -> bool {
        if tokens_per_sec <= 0.0 || !tokens_per_sec.is_finite() {
            return false;
        }
        let Some(baseline) = self.baseline else {
            self.baseline_samples.push(tokens_per_sec);
            if self.baseline_samples.len() >= LIVE_BASELINE_SLICES {
                let mean =
                    self.baseline_samples.iter().sum::<f64>() / self.baseline_samples.len() as f64;
                self.baseline = Some(mean);
            }
            return false;
        };
        if tokens_per_sec < baseline * LIVE_THROTTLE_RATIO {
            self.consecutive_low += 1;
        } else {
            self.consecutive_low = 0;
        }
        self.consecutive_low >= LIVE_MIN_DROP_SLICES
    }

    #[allow(dead_code)]
    pub fn is_throttling(&self) -> bool {
        self.consecutive_low >= LIVE_MIN_DROP_SLICES
    }
}

static LIVE_THROTTLE_DETECTED: std::sync::atomic::AtomicBool =
    std::sync::atomic::AtomicBool::new(false);

pub fn live_throttle_detected() -> bool {
    LIVE_THROTTLE_DETECTED.load(std::sync::atomic::Ordering::SeqCst)
}

fn set_live_throttle_detected() {
    LIVE_THROTTLE_DETECTED.store(true, std::sync::atomic::Ordering::SeqCst);
}

pub fn clear_live_throttle() {
    LIVE_THROTTLE_DETECTED.store(false, std::sync::atomic::Ordering::SeqCst);
}

async fn bench_llama(pool: &crate::pool::ModelPool) -> Result<BenchResult, RunError> {
    bench_llama_ref(pool, "llama-3.2-1b-instruct-q4", "llama-3.2-1b-instruct-q4").await
}

async fn bench_llama_ref(
    pool: &crate::pool::ModelPool,
    model_ref: &str,
    model_id: &str,
) -> Result<BenchResult, RunError> {
    let load_started = std::time::Instant::now();
    let model_handle = pool.llama(model_ref).await?;
    let mut model = model_handle.lock().await;
    let load_ms = load_started.elapsed().as_millis() as u64;
    // Same prompt and decode length as the candle-metal-llama1-infer
    // settlement-geometry authority, so the advertised startup bench can
    // clear that cell's conservative floor instead of measuring a shorter
    // decode-only toy prompt.
    let prompt = "Write a detailed paragraph about the ocean and its wonders:";

    let (_t, _n) = model.generate(prompt, 48)?; // warmup

    let mut lat = Vec::with_capacity(LATENCY_ITERS / 2);
    for _ in 0..(LATENCY_ITERS / 2) {
        let t = std::time::Instant::now();
        let (_txt, n) = model.generate(prompt, 48)?;
        let per_tok = t.elapsed().as_secs_f64() * 1000.0 / (n.max(1) as f64);
        lat.push(per_tok);
    }
    let p99_ms = percentile_ms(lat, 99.0);

    let (tps, thermal_ok) = sustained_tps(&mut model, prompt)?;
    tracing::info!(model = model_id, load_ms, "measured cold model load");
    // Advertised infer cells settle token_like_input_plus_max_output_tokens.
    // decode_output_tokens cannot satisfy that floor, so convert under the
    // same wall: billable = max(1, prompt_bytes/4) + generated_tokens.
    const GEN_TOKENS: f64 = 48.0;
    let prompt_bytes = prompt.len() as f64;
    let billable = prompt_bytes.max(4.0) / 4.0 + GEN_TOKENS;
    let combined = tps * (billable / GEN_TOKENS) as f32;
    Ok(BenchResult {
        model_id: model_id.to_string(),
        job_type: "batch_infer".to_string(),
        tps: combined,
        eps: 0.0,
        p99_ms,
        thermal_ok,
        load_ms,
        unit: "tokens".to_string(),
        unit_scope: "token_like_input_plus_max_output_tokens".to_string(),
        measured_unix: 0,
    })
}

fn sustained_tps(model: &mut LlamaBackend, prompt: &str) -> Result<(f32, bool), RunError> {
    sustained_throughput(|| {
        let t = std::time::Instant::now();
        let (_txt, n) = model.generate(prompt, 48)?;
        Ok(n as f64 / t.elapsed().as_secs_f64().max(1e-6))
    })
}

#[cfg(test)]
mod sustained_benchmark_tests {
    use super::conservative_sustained_rate;

    #[test]
    fn one_spike_cannot_authorize_a_higher_active_hour_floor() {
        let mut spiky = vec![200.0];
        spiky.extend(std::iter::repeat_n(100.0, 99));
        let advertised = conservative_sustained_rate(&spiky).expect("valid samples");
        assert_eq!(advertised, 95.0);
        assert!(advertised < 158.0, "one peak authorized a higher floor");
    }

    #[test]
    fn invalid_sustained_samples_fail_closed() {
        assert_eq!(conservative_sustained_rate(&[]), None);
        assert_eq!(conservative_sustained_rate(&[100.0, f64::NAN]), None);
        assert_eq!(conservative_sustained_rate(&[100.0, 0.0]), None);
    }
}
