//! Pluggable batch inference backends.
//!
//! `batch_infer` talks to this trait only. Candle is the in-process baseline;
//! `openai_http` fronts any OpenAI-compatible engine (llama-server, vLLM,
//! SGLang). The engine does continuous batching — that is the entire point.

use std::sync::Arc;
use std::time::Duration;

use async_trait::async_trait;
use serde::{Deserialize, Serialize};

use crate::executor::RunError;
use crate::pool::ModelPool;

/// Which concrete backend will execute batch_infer work.
#[derive(Debug, Clone, Copy, PartialEq, Eq, Serialize, Deserialize, Default)]
#[serde(rename_all = "snake_case")]
pub enum BackendKind {
    #[default]
    Candle,
    OpenAiHttp,
}

impl BackendKind {
    pub fn as_str(self) -> &'static str {
        match self {
            BackendKind::Candle => "candle",
            BackendKind::OpenAiHttp => "openai_http",
        }
    }

    pub fn parse(s: &str) -> Result<Self, String> {
        match s.trim().to_ascii_lowercase().as_str() {
            "" | "candle" => Ok(BackendKind::Candle),
            "openai_http" | "openai" | "http" | "llama_server" | "llama-server" => {
                Ok(BackendKind::OpenAiHttp)
            }
            other => Err(format!(
                "unknown inference_backend {other:?} (expected `candle` or `openai_http`)"
            )),
        }
    }

    /// Byte-exact redundancy needs greedy, engine-level determinism.
    /// Candle's argmax path is the verified baseline. OpenAI-compatible
    /// engines accept temperature=0/seed but continuous batching / kernel
    /// reductions often still flip logits — mark unusable for verified work.
    pub fn supports_verified_work(self) -> bool {
        matches!(self, BackendKind::Candle)
    }
}

impl std::fmt::Display for BackendKind {
    fn fmt(&self, f: &mut std::fmt::Formatter<'_>) -> std::fmt::Result {
        f.write_str(self.as_str())
    }
}

/// Shared generation knobs. Batch lane is greedy (`temperature == 0`).
#[derive(Debug, Clone, Copy)]
pub struct GenerateParams {
    pub max_tokens: u32,
    /// Must be 0.0 for verified / production batch work.
    pub temperature: f32,
    /// Fixed seed for engines that honour it (llama-server, vLLM).
    pub seed: u64,
}

impl GenerateParams {
    pub fn greedy(max_tokens: u32) -> Self {
        Self {
            max_tokens,
            temperature: 0.0,
            seed: 0,
        }
    }

    pub fn validate_deterministic(&self) -> Result<(), RunError> {
        if self.temperature != 0.0 {
            return Err(RunError::Inference {
                backend: "inference",
                msg: format!(
                    "non-zero temperature {} rejected: batch lane is greedy-only for redundancy verification",
                    self.temperature
                ),
            });
        }
        Ok(())
    }
}

/// One completion from a batch generate call.
#[derive(Debug, Clone, Serialize, Deserialize)]
pub struct BackendCompletion {
    pub text: String,
    pub tokens: usize,
}

/// Batch-oriented inference engine. The task/checkpoint/commit contract above
/// this trait stays byte-identical regardless of implementation.
#[async_trait]
pub trait InferenceBackend: Send + Sync {
    fn kind(&self) -> BackendKind;

    fn name(&self) -> &'static str {
        self.kind().as_str()
    }

    fn supports_verified_work(&self) -> bool {
        self.kind().supports_verified_work()
    }

    /// Generate completions for `prompts` with the given params.
    /// Order of results matches order of prompts.
    async fn generate_batch(
        &self,
        model_ref: &str,
        prompts: &[String],
        params: GenerateParams,
        pool: &ModelPool,
    ) -> Result<Vec<BackendCompletion>, RunError>;

    /// Single-prompt convenience used by benches and warmup.
    async fn generate(
        &self,
        model_ref: &str,
        prompt: &str,
        params: GenerateParams,
        pool: &ModelPool,
    ) -> Result<BackendCompletion, RunError> {
        let mut out = self
            .generate_batch(model_ref, &[prompt.to_string()], params, pool)
            .await?;
        out.pop().ok_or_else(|| RunError::Inference {
            backend: self.name(),
            msg: "generate returned empty batch".into(),
        })
    }
}

// ── Candle (in-process baseline) ───────────────────────────────────────────

/// Existing Candle/Metal path, unchanged algorithmically, now behind the trait.
pub struct CandleBackend;

#[async_trait]
impl InferenceBackend for CandleBackend {
    fn kind(&self) -> BackendKind {
        BackendKind::Candle
    }

    async fn generate_batch(
        &self,
        model_ref: &str,
        prompts: &[String],
        params: GenerateParams,
        pool: &ModelPool,
    ) -> Result<Vec<BackendCompletion>, RunError> {
        params.validate_deterministic()?;
        if prompts.is_empty() {
            return Ok(Vec::new());
        }
        let model = pool.llama(model_ref).await?;
        let chunk = prompts.to_vec();
        let max_tokens = params.max_tokens;
        let results = tokio::task::spawn_blocking(move || -> Result<_, RunError> {
            let mut backend = model.blocking_lock();
            backend.generate_batch(&chunk, max_tokens)
        })
        .await
        .map_err(|e| RunError::Inference {
            backend: "candle",
            msg: format!("worker thread failed: {e}"),
        })??;
        Ok(results
            .into_iter()
            .map(|(text, tokens)| BackendCompletion { text, tokens })
            .collect())
    }

    async fn generate(
        &self,
        model_ref: &str,
        prompt: &str,
        params: GenerateParams,
        pool: &ModelPool,
    ) -> Result<BackendCompletion, RunError> {
        params.validate_deterministic()?;
        let model = pool.llama(model_ref).await?;
        let prompt = prompt.to_string();
        let max_tokens = params.max_tokens;
        let (text, tokens) = tokio::task::spawn_blocking(move || -> Result<_, RunError> {
            let mut backend = model.blocking_lock();
            backend.generate(&prompt, max_tokens)
        })
        .await
        .map_err(|e| RunError::Inference {
            backend: "candle",
            msg: format!("worker thread failed: {e}"),
        })??;
        Ok(BackendCompletion { text, tokens })
    }
}

// ── OpenAI-compatible HTTP engine ──────────────────────────────────────────

/// Fronts llama-server / vLLM / SGLang via `/v1/chat/completions`.
///
/// Concurrent requests are issued so the *engine* can continuous-batch; this
/// agent does not materialise a dense KV cache.
///
/// Determinism: requests force `temperature=0`, `top_p=1`, fixed `seed`.
/// Continuous-batching engines still do not generally guarantee byte-identical
/// greedy decode across batch sizes or hosts — see
/// [`BackendKind::supports_verified_work`].
#[derive(Clone)]
pub struct OpenAiHttpBackend {
    client: reqwest::Client,
    base_url: String,
    model: String,
    api_key: Option<String>,
}

impl OpenAiHttpBackend {
    pub fn new(
        base_url: impl Into<String>,
        model: impl Into<String>,
        api_key: Option<String>,
    ) -> Result<Self, RunError> {
        let base_url = base_url.into().trim_end_matches('/').to_string();
        if base_url.is_empty() {
            return Err(RunError::Inference {
                backend: "openai_http",
                msg: "openai_base_url is empty".into(),
            });
        }
        let client = reqwest::Client::builder()
            .timeout(Duration::from_secs(600))
            .connect_timeout(Duration::from_secs(10))
            .build()
            .map_err(|e| RunError::Inference {
                backend: "openai_http",
                msg: format!("building HTTP client: {e}"),
            })?;
        Ok(Self {
            client,
            base_url,
            model: model.into(),
            api_key,
        })
    }

    fn chat_url(&self) -> String {
        format!("{}/chat/completions", self.base_url)
    }

    async fn complete_one(
        &self,
        prompt: &str,
        params: GenerateParams,
    ) -> Result<BackendCompletion, RunError> {
        params.validate_deterministic()?;

        #[derive(Serialize)]
        struct Message<'a> {
            role: &'a str,
            content: &'a str,
        }
        #[derive(Serialize)]
        struct RequestBody<'a> {
            model: &'a str,
            messages: Vec<Message<'a>>,
            max_tokens: u32,
            temperature: f32,
            top_p: f32,
            seed: u64,
            stream: bool,
        }
        #[derive(Deserialize)]
        struct ChoiceMsg {
            content: Option<String>,
        }
        #[derive(Deserialize)]
        struct Choice {
            message: ChoiceMsg,
        }
        #[derive(Deserialize)]
        struct Usage {
            completion_tokens: Option<u64>,
        }
        #[derive(Deserialize)]
        struct Response {
            choices: Vec<Choice>,
            usage: Option<Usage>,
        }

        let body = RequestBody {
            model: &self.model,
            messages: vec![Message {
                role: "user",
                content: prompt,
            }],
            max_tokens: params.max_tokens,
            temperature: 0.0,
            top_p: 1.0,
            seed: params.seed,
            stream: false,
        };

        let mut req = self
            .client
            .post(self.chat_url())
            .header(reqwest::header::CONTENT_TYPE, "application/json")
            .json(&body);
        if let Some(key) = &self.api_key {
            if !key.is_empty() {
                req = req.bearer_auth(key);
            }
        }

        let resp = req.send().await.map_err(|e| RunError::Inference {
            backend: "openai_http",
            msg: format!("POST {}: {e}", self.chat_url()),
        })?;
        let status = resp.status();
        let bytes = resp.bytes().await.map_err(|e| RunError::Inference {
            backend: "openai_http",
            msg: format!("reading response body: {e}"),
        })?;
        if !status.is_success() {
            let snippet = String::from_utf8_lossy(&bytes);
            let snippet: String = snippet.chars().take(400).collect();
            return Err(RunError::Inference {
                backend: "openai_http",
                msg: format!("engine returned HTTP {status}: {snippet}"),
            });
        }
        let parsed: Response = serde_json::from_slice(&bytes).map_err(|e| RunError::Inference {
            backend: "openai_http",
            msg: format!("decoding chat.completions JSON: {e}"),
        })?;
        let text = parsed
            .choices
            .first()
            .and_then(|c| c.message.content.clone())
            .unwrap_or_default()
            .trim()
            .to_string();
        let tokens = parsed
            .usage
            .and_then(|u| u.completion_tokens)
            .map(|n| n as usize)
            .unwrap_or_else(|| estimate_tokens(&text));
        Ok(BackendCompletion { text, tokens })
    }
}

/// Rough fallback when an engine omits usage — not used for billing, only for
/// throughput bookkeeping in benches / checkpoints.
fn estimate_tokens(text: &str) -> usize {
    // ~4 chars/token English heuristic; floor at 1 if non-empty so tok/s is defined.
    let n = text.chars().count().div_ceil(4);
    if text.is_empty() {
        0
    } else {
        n.max(1)
    }
}

#[async_trait]
impl InferenceBackend for OpenAiHttpBackend {
    fn kind(&self) -> BackendKind {
        BackendKind::OpenAiHttp
    }

    async fn generate_batch(
        &self,
        _model_ref: &str,
        prompts: &[String],
        params: GenerateParams,
        _pool: &ModelPool,
    ) -> Result<Vec<BackendCompletion>, RunError> {
        params.validate_deterministic()?;
        if prompts.is_empty() {
            return Ok(Vec::new());
        }
        // Fan out concurrently so the engine can continuous-batch. Preserve order.
        let mut join = tokio::task::JoinSet::new();
        for (i, prompt) in prompts.iter().cloned().enumerate() {
            let this = self.clone();
            join.spawn(async move {
                let completion = this.complete_one(&prompt, params).await?;
                Ok::<_, RunError>((i, completion))
            });
        }
        let mut slots: Vec<Option<BackendCompletion>> = (0..prompts.len()).map(|_| None).collect();
        while let Some(joined) = join.join_next().await {
            let (i, completion) = joined.map_err(|e| RunError::Inference {
                backend: "openai_http",
                msg: format!("request task failed: {e}"),
            })??;
            slots[i] = Some(completion);
        }
        slots
            .into_iter()
            .enumerate()
            .map(|(i, c)| {
                c.ok_or_else(|| RunError::Inference {
                    backend: "openai_http",
                    msg: format!("missing completion for prompt index {i}"),
                })
            })
            .collect()
    }
}

/// Build the configured backend. Candle needs no network; openai_http needs a live engine.
pub fn build_backend(
    kind: BackendKind,
    openai_base_url: &str,
    openai_model: &str,
    openai_api_key: Option<&str>,
) -> Result<Arc<dyn InferenceBackend>, RunError> {
    match kind {
        BackendKind::Candle => Ok(Arc::new(CandleBackend)),
        BackendKind::OpenAiHttp => {
            let key = openai_api_key
                .map(|s| s.to_string())
                .filter(|s| !s.is_empty());
            Ok(Arc::new(OpenAiHttpBackend::new(
                openai_base_url,
                openai_model,
                key,
            )?))
        }
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn backend_kind_parse_aliases() {
        assert_eq!(BackendKind::parse("candle").unwrap(), BackendKind::Candle);
        assert_eq!(BackendKind::parse("").unwrap(), BackendKind::Candle);
        assert_eq!(
            BackendKind::parse("openai_http").unwrap(),
            BackendKind::OpenAiHttp
        );
        assert_eq!(
            BackendKind::parse("llama-server").unwrap(),
            BackendKind::OpenAiHttp
        );
        assert!(BackendKind::parse("vllm-native").is_err());
    }

    #[test]
    fn only_candle_supports_verified_work() {
        assert!(BackendKind::Candle.supports_verified_work());
        assert!(!BackendKind::OpenAiHttp.supports_verified_work());
    }

    #[test]
    fn greedy_params_reject_nonzero_temperature() {
        let p = GenerateParams {
            max_tokens: 16,
            temperature: 0.7,
            seed: 0,
        };
        let err = p.validate_deterministic().unwrap_err();
        assert!(err.to_string().contains("temperature"));
    }

    #[test]
    fn estimate_tokens_empty_and_nonempty() {
        assert_eq!(estimate_tokens(""), 0);
        assert!(estimate_tokens("hello world") >= 1);
    }
}
