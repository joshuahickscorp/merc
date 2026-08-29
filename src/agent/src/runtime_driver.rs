//! The agent-side half of a runtime integration.
//!
//! The control plane's `RuntimeProfileAdapter` decides: is this profile well
//! formed, may this worker claim it, can it serve this workload, what should we
//! expect. It deliberately has no launch, health, execute, cancel, drain or
//! metrics, because the control plane never runs a model. This is where those
//! live, in the process that does.
//!
//! `execute` is spelled `embed` here on purpose. Embedding is the only verb a
//! second runtime is proven for: llama.cpp on Metal diverges from its own serial
//! output in every batched configuration
//! (`evidence/perf/runtime-benchmarks/llama-cpp-metal-determinism-sweep.json`),
//! which disqualifies it from `byte_exact` cells, and clears the cosine gate at
//! 0.999999 for the embed cell. A generic `execute` carrying one concrete verb
//! would be a wider promise than the measurements support; `batch_infer` stays on
//! [`crate::inference::InferenceBackend`] until a second runtime clears
//! byte-exactness.

use std::sync::atomic::{AtomicBool, AtomicU64, Ordering};
use std::sync::Arc;
use std::time::{Duration, Instant};

use async_trait::async_trait;
use serde::{Deserialize, Serialize};

use crate::executor::{Embedder, RunError};
use crate::pool::ModelPool;
use crate::runtime_authority::RuntimeCapability;

/// What a driver reports about itself. Counters only — no rates, because a rate
/// computed over a driver's own lifetime is not a benchmark and must never be
/// mistaken for one.
#[derive(Debug, Clone, Default, Serialize, Deserialize, PartialEq)]
pub struct DriverMetrics {
    pub runtime_id: String,
    pub requests: u64,
    pub texts: u64,
    pub failures: u64,
    pub last_latency_ms: u64,
    pub cancelled: bool,
}

/// One runtime, as the agent drives it.
#[async_trait]
// validate, cancel and drain are the boundary's shape, not its current traffic.
// They exist so a second runtime declares how it refuses a cell it cannot serve
// and how it stops mid-flight; the agent's present loop reaches neither, and
// deleting them would make the trait describe less than a driver has to answer.
#[allow(dead_code)]
pub trait RuntimeDriver: Send + Sync {
    /// The governed profile this driver implements.
    fn runtime_id(&self) -> &'static str;

    /// Refuse a cell this driver cannot serve, before any work is claimed.
    ///
    /// The check that matters is the artifact format: a driver asked to serve a
    /// cell whose wire kind it has no loader for would otherwise fail at the
    /// first task, after the claim, with the supplier already on the hook.
    fn validate(&self, cell: &RuntimeCapability) -> Result<(), RunError>;

    /// The artifact format this driver has a loader for. The dispatch path asks
    /// this instead of testing the model kind itself, so adding a runtime that
    /// serves a different format does not mean editing the runner.
    fn serves_kind(&self, kind: &str) -> bool;

    /// Bring the runtime up, or verify an externally-managed one is up.
    async fn launch(&self) -> Result<(), RunError>;

    /// Is it serving right now?
    async fn health(&self) -> Result<(), RunError>;

    /// `execute`, for the one verb this boundary carries. See the module note.
    async fn embed(
        &self,
        model_ref: &str,
        texts: &[String],
        pool: &ModelPool,
    ) -> Result<Vec<Vec<f32>>, RunError>;

    /// Refuse further work and abandon what is in flight. Idempotent.
    fn cancel(&self);

    /// Stop accepting work and wait for what is in flight to finish.
    async fn drain(&self, timeout: Duration) -> Result<(), RunError>;

    fn metrics(&self) -> DriverMetrics;
}

/// Counters shared by every driver. Split out because a driver that had to
/// implement its own bookkeeping would eventually count something different from
/// its neighbour, and the two would stop being comparable.
#[derive(Debug, Default)]
pub struct DriverCounters {
    requests: AtomicU64,
    texts: AtomicU64,
    failures: AtomicU64,
    last_latency_ms: AtomicU64,
    cancelled: AtomicBool,
}

#[allow(dead_code)] // cancel() is the counter half of RuntimeDriver::cancel
impl DriverCounters {
    fn record<T>(&self, texts: usize, started: Instant, out: &Result<T, RunError>) {
        self.requests.fetch_add(1, Ordering::Relaxed);
        self.texts.fetch_add(texts as u64, Ordering::Relaxed);
        self.last_latency_ms
            .store(started.elapsed().as_millis() as u64, Ordering::Relaxed);
        if out.is_err() {
            self.failures.fetch_add(1, Ordering::Relaxed);
        }
    }

    fn snapshot(&self, runtime_id: &str) -> DriverMetrics {
        DriverMetrics {
            runtime_id: runtime_id.to_string(),
            requests: self.requests.load(Ordering::Relaxed),
            texts: self.texts.load(Ordering::Relaxed),
            failures: self.failures.load(Ordering::Relaxed),
            last_latency_ms: self.last_latency_ms.load(Ordering::Relaxed),
            cancelled: self.cancelled.load(Ordering::Relaxed),
        }
    }

    fn cancel(&self) {
        self.cancelled.store(true, Ordering::Release);
    }

    fn is_cancelled(&self) -> bool {
        self.cancelled.load(Ordering::Acquire)
    }
}

/// Build the embed driver this agent was configured with.
///
/// Fails closed on an unknown name rather than falling back to Candle: silently
/// serving the wrong runtime would produce receipts naming a profile that never
/// executed anything.
pub fn build_embed_driver(
    embed_runtime: &str,
    llama_base_url: &str,
) -> Result<Arc<dyn RuntimeDriver>, RunError> {
    match embed_runtime.trim() {
        "" | "candle_metal" | "candle" => Ok(Arc::new(CandleDriver::new())),
        "llama_cpp_metal" | "llama_cpp" => Ok(Arc::new(LlamaCppDriver::new(
            LlamaServerSupervision::Attach {
                base_url: llama_base_url.to_string(),
            },
        )?)),
        other => Err(RunError::Inference {
            backend: "embed",
            msg: format!(
                "unknown embed_runtime {other:?} (expected `candle_metal` or `llama_cpp_metal`)"
            ),
        }),
    }
}

fn cancelled_err(backend: &'static str) -> RunError {
    RunError::Inference {
        backend,
        msg: "runtime driver was cancelled".into(),
    }
}

// ── candle (in-process, the routable baseline) ─────────────────────────────

/// The existing Candle/Metal embed path, unchanged algorithmically, behind the
/// driver boundary so the second runtime has something to be compared against
/// through the same interface rather than through a parallel one.
#[derive(Default)]
pub struct CandleDriver {
    counters: DriverCounters,
}

impl CandleDriver {
    pub fn new() -> Self {
        Self::default()
    }
}

#[async_trait]
impl RuntimeDriver for CandleDriver {
    fn runtime_id(&self) -> &'static str {
        "candle_metal"
    }

    fn serves_kind(&self, kind: &str) -> bool {
        kind == "hf"
    }

    fn validate(&self, cell: &RuntimeCapability) -> Result<(), RunError> {
        if cell.engine != "candle" {
            return Err(RunError::Inference {
                backend: "candle",
                msg: format!("cell {} names engine {}", cell.id, cell.engine),
            });
        }
        if cell.model_kind != "hf" {
            return Err(RunError::Inference {
                backend: "candle",
                msg: format!(
                    "cell {} serves {} as {}; candle loads safetensors",
                    cell.id, cell.model, cell.model_kind
                ),
            });
        }
        Ok(())
    }

    /// Candle runs in this process: there is nothing to launch, and saying so is
    /// more honest than spawning something to make the interface look uniform.
    async fn launch(&self) -> Result<(), RunError> {
        Ok(())
    }

    async fn health(&self) -> Result<(), RunError> {
        if self.counters.is_cancelled() {
            return Err(cancelled_err("candle"));
        }
        Ok(())
    }

    async fn embed(
        &self,
        model_ref: &str,
        texts: &[String],
        pool: &ModelPool,
    ) -> Result<Vec<Vec<f32>>, RunError> {
        if self.counters.is_cancelled() {
            return Err(cancelled_err("candle"));
        }
        let started = Instant::now();
        let out = candle_embed(pool, model_ref, texts.to_vec()).await;
        self.counters.record(texts.len(), started, &out);
        out
    }

    fn cancel(&self) {
        self.counters.cancel();
    }

    /// In-process and synchronous per call: once `embed` has returned there is
    /// nothing outstanding to wait for.
    async fn drain(&self, _timeout: Duration) -> Result<(), RunError> {
        self.counters.cancel();
        Ok(())
    }

    fn metrics(&self) -> DriverMetrics {
        self.counters.snapshot(self.runtime_id())
    }
}

async fn candle_embed(
    pool: &ModelPool,
    model_ref: &str,
    texts: Vec<String>,
) -> Result<Vec<Vec<f32>>, RunError> {
    let embedder = pool.embedder(model_ref).await?;
    tokio::task::spawn_blocking(move || {
        let backend: tokio::sync::MutexGuard<'_, Embedder> = embedder.blocking_lock();
        backend.embed(&texts)
    })
    .await
    .map_err(|e| RunError::Inference {
        backend: "candle",
        msg: format!("worker thread failed: {e}"),
    })?
}

// ── llama.cpp (llama-server over HTTP) ─────────────────────────────────────

/// How the agent obtains a llama-server.
#[derive(Debug, Clone)]
// Spawn is the supervised-launch arm. The agent currently attaches to a server
// an operator started, so nothing constructs it yet; it is kept because the
// alternative is an enum that cannot express supervised launch at all.
#[allow(dead_code)]
pub enum LlamaServerSupervision {
    /// Attach to a server the operator runs. This is the shape a supplier
    /// actually deploys — llama-server owns the GPU, and having the agent fight
    /// it for ownership on every restart is how the Metal init panic in
    /// `models::device()` was earned in the first place.
    Attach { base_url: String },
    /// Spawn and own the process. `model_path` is the GGUF the cell resolves.
    Spawn {
        binary: String,
        model_path: String,
        port: u16,
    },
}

pub struct LlamaCppDriver {
    client: reqwest::Client,
    supervision: LlamaServerSupervision,
    counters: DriverCounters,
    child: tokio::sync::Mutex<Option<tokio::process::Child>>,
    inflight: Arc<AtomicU64>,
}

impl LlamaCppDriver {
    pub fn new(supervision: LlamaServerSupervision) -> Result<Self, RunError> {
        let client = reqwest::Client::builder()
            .timeout(Duration::from_secs(120))
            .connect_timeout(Duration::from_secs(5))
            .build()
            .map_err(|e| RunError::Inference {
                backend: "llama_cpp",
                msg: format!("building HTTP client: {e}"),
            })?;
        Ok(Self {
            client,
            supervision,
            counters: DriverCounters::default(),
            child: tokio::sync::Mutex::new(None),
            inflight: Arc::new(AtomicU64::new(0)),
        })
    }

    pub fn base_url(&self) -> String {
        match &self.supervision {
            LlamaServerSupervision::Attach { base_url } => {
                base_url.trim_end_matches('/').to_string()
            }
            LlamaServerSupervision::Spawn { port, .. } => format!("http://127.0.0.1:{port}"),
        }
    }

    async fn wait_until_healthy(&self, timeout: Duration) -> Result<(), RunError> {
        let deadline = Instant::now() + timeout;
        let mut last = String::from("never attempted");
        while Instant::now() < deadline {
            match self.health().await {
                Ok(()) => return Ok(()),
                Err(error) => last = error.to_string(),
            }
            tokio::time::sleep(Duration::from_millis(250)).await;
        }
        Err(RunError::Inference {
            backend: "llama_cpp",
            msg: format!("llama-server did not become healthy in {timeout:?}: {last}"),
        })
    }

    /// The server's own account of how it is configured.
    ///
    /// A throughput receipt that does not record `n_ctx` and `total_slots` is not
    /// authority for anything: this engine's texts/s at batch 128 is decided
    /// almost entirely by whether 128 fits in its slots and context, and a reader
    /// comparing two receipts has no way to know they were taken under the same
    /// server. Reported verbatim rather than from the flags the agent thinks it
    /// passed, because an attached server was configured by someone else.
    pub async fn engine_properties(&self) -> Result<serde_json::Value, RunError> {
        let url = format!("{}/props", self.base_url());
        let resp = self
            .client
            .get(&url)
            .timeout(Duration::from_secs(5))
            .send()
            .await
            .map_err(|e| RunError::Inference {
                backend: "llama_cpp",
                msg: format!("GET {url}: {e}"),
            })?;
        let props: serde_json::Value = resp.json().await.map_err(|e| RunError::Inference {
            backend: "llama_cpp",
            msg: format!("decoding /props: {e}"),
        })?;
        Ok(serde_json::json!({
            "build_info": props.get("build_info"),
            "model_path": props.get("model_path"),
            "total_slots": props.get("total_slots"),
            "n_ctx": props
                .get("default_generation_settings")
                .and_then(|g| g.get("n_ctx")),
        }))
    }
}

#[async_trait]
impl RuntimeDriver for LlamaCppDriver {
    fn runtime_id(&self) -> &'static str {
        "llama_cpp_metal"
    }

    fn serves_kind(&self, kind: &str) -> bool {
        kind == "gguf"
    }

    fn validate(&self, cell: &RuntimeCapability) -> Result<(), RunError> {
        if cell.engine != "llama_cpp" {
            return Err(RunError::Inference {
                backend: "llama_cpp",
                msg: format!("cell {} names engine {}", cell.id, cell.engine),
            });
        }
        if cell.model_kind != "gguf" {
            return Err(RunError::Inference {
                backend: "llama_cpp",
                msg: format!(
                    "cell {} serves {} as {}; llama.cpp loads GGUF",
                    cell.id, cell.model, cell.model_kind
                ),
            });
        }
        // The measured disqualification, enforced rather than documented.
        // llama.cpp on Metal reproduces its own serial output only when
        // serialised to one slot, so it may not take a cell whose verification
        // strategy compares bytes across workers.
        if cell.verification == "byte_exact" {
            return Err(RunError::Inference {
                backend: "llama_cpp",
                msg: format!(
                    "cell {} is verified byte_exact; llama.cpp on Metal diverges from its own \
                     serial output in every batched configuration measured",
                    cell.id
                ),
            });
        }
        Ok(())
    }

    async fn launch(&self) -> Result<(), RunError> {
        match &self.supervision {
            // Attaching still verifies. "Configured" is not "serving", and a
            // driver that reported success for an unreachable endpoint would
            // move the failure to the first claimed task.
            LlamaServerSupervision::Attach { .. } => {
                self.wait_until_healthy(Duration::from_secs(5)).await
            }
            LlamaServerSupervision::Spawn {
                binary,
                model_path,
                port,
            } => {
                let mut guard = self.child.lock().await;
                if guard.is_some() {
                    return self.wait_until_healthy(Duration::from_secs(60)).await;
                }
                let child = tokio::process::Command::new(binary)
                    .args([
                        "-m",
                        model_path,
                        "--embedding",
                        "--pooling",
                        "mean",
                        "--port",
                        &port.to_string(),
                        "--host",
                        "127.0.0.1",
                    ])
                    .stdout(std::process::Stdio::null())
                    .stderr(std::process::Stdio::null())
                    .kill_on_drop(true)
                    .spawn()
                    .map_err(|e| RunError::Inference {
                        backend: "llama_cpp",
                        msg: format!("spawning {binary}: {e}"),
                    })?;
                *guard = Some(child);
                drop(guard);
                self.wait_until_healthy(Duration::from_secs(120)).await
            }
        }
    }

    async fn health(&self) -> Result<(), RunError> {
        if self.counters.is_cancelled() {
            return Err(cancelled_err("llama_cpp"));
        }
        let url = format!("{}/health", self.base_url());
        let resp = self
            .client
            .get(&url)
            .timeout(Duration::from_secs(3))
            .send()
            .await
            .map_err(|e| RunError::Inference {
                backend: "llama_cpp",
                msg: format!("GET {url}: {e}"),
            })?;
        if !resp.status().is_success() {
            return Err(RunError::Inference {
                backend: "llama_cpp",
                msg: format!("GET {url} returned HTTP {}", resp.status()),
            });
        }
        Ok(())
    }

    async fn embed(
        &self,
        model_ref: &str,
        texts: &[String],
        _pool: &ModelPool,
    ) -> Result<Vec<Vec<f32>>, RunError> {
        if self.counters.is_cancelled() {
            return Err(cancelled_err("llama_cpp"));
        }
        if texts.is_empty() {
            return Ok(Vec::new());
        }
        let started = Instant::now();
        self.inflight.fetch_add(1, Ordering::AcqRel);
        let out = self.embed_inner(model_ref, texts).await;
        self.inflight.fetch_sub(1, Ordering::AcqRel);
        self.counters.record(texts.len(), started, &out);
        out
    }

    fn cancel(&self) {
        self.counters.cancel();
    }

    async fn drain(&self, timeout: Duration) -> Result<(), RunError> {
        self.counters.cancel();
        let deadline = Instant::now() + timeout;
        while self.inflight.load(Ordering::Acquire) > 0 {
            if Instant::now() >= deadline {
                return Err(RunError::Inference {
                    backend: "llama_cpp",
                    msg: format!(
                        "{} requests still in flight after {timeout:?}",
                        self.inflight.load(Ordering::Acquire)
                    ),
                });
            }
            tokio::time::sleep(Duration::from_millis(50)).await;
        }
        if let Some(mut child) = self.child.lock().await.take() {
            let _ = child.kill().await;
        }
        Ok(())
    }

    fn metrics(&self) -> DriverMetrics {
        self.counters.snapshot(self.runtime_id())
    }
}

impl LlamaCppDriver {
    async fn embed_inner(
        &self,
        model_ref: &str,
        texts: &[String],
    ) -> Result<Vec<Vec<f32>>, RunError> {
        #[derive(Serialize)]
        struct Request<'a> {
            model: &'a str,
            input: &'a [String],
        }
        #[derive(Deserialize)]
        struct Row {
            index: usize,
            embedding: Vec<f32>,
        }
        #[derive(Deserialize)]
        struct Response {
            data: Vec<Row>,
        }

        let url = format!("{}/v1/embeddings", self.base_url());
        let resp = self
            .client
            .post(&url)
            .json(&Request {
                model: model_ref,
                input: texts,
            })
            .send()
            .await
            .map_err(|e| RunError::Inference {
                backend: "llama_cpp",
                msg: format!("POST {url}: {e}"),
            })?;
        let status = resp.status();
        let bytes = resp.bytes().await.map_err(|e| RunError::Inference {
            backend: "llama_cpp",
            msg: format!("reading embeddings body: {e}"),
        })?;
        if !status.is_success() {
            let snippet: String = String::from_utf8_lossy(&bytes).chars().take(400).collect();
            return Err(RunError::Inference {
                backend: "llama_cpp",
                msg: format!("embeddings returned HTTP {status}: {snippet}"),
            });
        }
        let parsed: Response = serde_json::from_slice(&bytes).map_err(|e| RunError::Inference {
            backend: "llama_cpp",
            msg: format!("decoding embeddings JSON: {e}"),
        })?;
        if parsed.data.len() != texts.len() {
            return Err(RunError::Inference {
                backend: "llama_cpp",
                msg: format!(
                    "asked for {} embeddings, got {}",
                    texts.len(),
                    parsed.data.len()
                ),
            });
        }

        // Order by the server's own index rather than trusting arrival order.
        // A row landing in the wrong slot would silently attach one buyer's
        // vector to another's text, and every downstream check is on the bytes,
        // not on the pairing.
        let mut slots: Vec<Option<Vec<f32>>> = (0..texts.len()).map(|_| None).collect();
        for row in parsed.data {
            let slot = slots
                .get_mut(row.index)
                .ok_or_else(|| RunError::Inference {
                    backend: "llama_cpp",
                    msg: format!("embedding index {} is out of range", row.index),
                })?;
            if slot.is_some() {
                return Err(RunError::Inference {
                    backend: "llama_cpp",
                    msg: format!("embedding index {} was returned twice", row.index),
                });
            }
            *slot = Some(normalize(row.embedding));
        }
        slots
            .into_iter()
            .enumerate()
            .map(|(i, row)| {
                row.ok_or_else(|| RunError::Inference {
                    backend: "llama_cpp",
                    msg: format!("no embedding for index {i}"),
                })
            })
            .collect()
    }
}

/// L2-normalise, because the cosine gate compares against candle's `embed`,
/// which normalises, and llama-server does not.
///
/// A zero vector is returned unchanged rather than producing NaNs: the gate is
/// then free to fail it as a mismatch, which is the correct outcome, instead of
/// the comparison silently evaluating to false everywhere.
fn normalize(mut v: Vec<f32>) -> Vec<f32> {
    let norm = v.iter().map(|x| x * x).sum::<f32>().sqrt();
    if norm > 0.0 {
        for x in &mut v {
            *x /= norm;
        }
    }
    v
}

/// Cosine similarity of two equal-length vectors, or None if they disagree in
/// length. Used by the cross-runtime equivalence check.
pub fn cosine(a: &[f32], b: &[f32]) -> Option<f32> {
    if a.len() != b.len() || a.is_empty() {
        return None;
    }
    let dot: f32 = a.iter().zip(b).map(|(x, y)| x * y).sum();
    let na = a.iter().map(|x| x * x).sum::<f32>().sqrt();
    let nb = b.iter().map(|x| x * x).sum::<f32>().sqrt();
    if na == 0.0 || nb == 0.0 {
        return None;
    }
    Some(dot / (na * nb))
}

/// The engine a worker advertises, derived from its configured embed runtime.
///
/// Hardcoding "candle" made a llama.cpp-configured worker register as a candle
/// worker: it was authorized for candle's cells, would have been dispatched work
/// its driver cannot serve, and never advertised the cell it exists to prove.
pub fn advertised_engine(embed_runtime: &str) -> Result<&'static str, RunError> {
    match embed_runtime.trim() {
        "" | "candle_metal" | "candle" => Ok("candle"),
        "llama_cpp_metal" | "llama_cpp" => Ok("llama_cpp"),
        other => Err(RunError::Inference {
            backend: "embed",
            msg: format!("unknown embed_runtime {other:?}"),
        }),
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    fn cell(id: &str, engine: &str, kind: &str, verification: &str) -> RuntimeCapability {
        RuntimeCapability {
            id: id.into(),
            runtime: "test".into(),
            engine: engine.into(),
            device: "metal".into(),
            hardware_classes: vec!["apple_silicon_max".into()],
            job: "embed".into(),
            model: "all-minilm-l6-v2".into(),
            model_kind: kind.into(),
            runner: "embed".into(),
            min_memory_gb: 2.0,
            verification: verification.into(),
        }
    }

    #[test]
    fn each_driver_refuses_the_other_engines_cell() {
        let candle = CandleDriver::new();
        let llama = LlamaCppDriver::new(LlamaServerSupervision::Attach {
            base_url: "http://127.0.0.1:1".into(),
        })
        .unwrap();

        let candle_cell = cell("c", "candle", "hf", "cosine");
        let llama_cell = cell("l", "llama_cpp", "gguf", "cosine");

        assert!(candle.validate(&candle_cell).is_ok());
        assert!(llama.validate(&llama_cell).is_ok());
        assert!(candle.validate(&llama_cell).is_err());
        assert!(llama.validate(&candle_cell).is_err());
    }

    // The determinism sweep is the reason this profile is not simply "a second
    // runtime". A byte_exact cell must be refused at validate, not discovered
    // after a supplier has claimed the task.
    #[test]
    fn llama_cpp_refuses_a_byte_exact_cell() {
        let llama = LlamaCppDriver::new(LlamaServerSupervision::Attach {
            base_url: "http://127.0.0.1:1".into(),
        })
        .unwrap();
        let err = llama
            .validate(&cell("l", "llama_cpp", "gguf", "byte_exact"))
            .unwrap_err()
            .to_string();
        assert!(err.contains("byte_exact"), "refusal said {err}");
    }

    #[tokio::test]
    async fn launch_refuses_an_endpoint_that_is_not_serving() {
        let llama = LlamaCppDriver::new(LlamaServerSupervision::Attach {
            // Port 1 is reserved and nothing listens on it.
            base_url: "http://127.0.0.1:1".into(),
        })
        .unwrap();
        assert!(llama.launch().await.is_err());
    }

    #[tokio::test]
    async fn cancel_stops_further_work_and_shows_in_metrics() {
        let llama = LlamaCppDriver::new(LlamaServerSupervision::Attach {
            base_url: "http://127.0.0.1:1".into(),
        })
        .unwrap();
        llama.cancel();
        assert!(llama.metrics().cancelled);
        assert!(llama.health().await.is_err());
        let pool = ModelPool::new();
        let err = llama
            .embed("all-minilm-l6-v2", &["x".to_string()], &pool)
            .await
            .unwrap_err()
            .to_string();
        assert!(err.contains("cancelled"), "error said {err}");
    }

    #[tokio::test]
    async fn drain_returns_when_nothing_is_in_flight() {
        let llama = LlamaCppDriver::new(LlamaServerSupervision::Attach {
            base_url: "http://127.0.0.1:1".into(),
        })
        .unwrap();
        llama.drain(Duration::from_secs(1)).await.unwrap();
    }

    #[test]
    fn normalize_leaves_a_zero_vector_alone_rather_than_producing_nans() {
        assert_eq!(normalize(vec![0.0, 0.0]), vec![0.0, 0.0]);
        let unit = normalize(vec![3.0, 4.0]);
        assert!((unit[0] - 0.6).abs() < 1e-6 && (unit[1] - 0.8).abs() < 1e-6);
    }

    #[test]
    fn cosine_of_identical_vectors_is_one_and_length_mismatch_is_none() {
        let a = vec![1.0, 2.0, 3.0];
        assert!((cosine(&a, &a).unwrap() - 1.0).abs() < 1e-6);
        assert_eq!(cosine(&a, &[1.0, 2.0]), None);
        assert_eq!(cosine(&[], &[]), None);
    }

    fn embed_manifest(kind: crate::types::ModelKind) -> crate::types::JobManifest {
        use crate::types::*;
        JobManifest {
            id: uuid::Uuid::nil(),
            job_type: JobType::Embed {
                binary: true,
                batch_size: 0,
            },
            model: ModelRef {
                kind,
                model_ref: "all-minilm-l6-v2".into(),
            },
            inputs: vec![],
            output: OutputRef { url: String::new() },
            params: serde_json::Value::Null,
            constraints: JobConstraints {
                min_memory_gb: 0.0,
                hw_classes: None,
                max_duration_secs: 600,
                data_residency: None,
            },
            verification: VerificationPolicy {
                redundancy_frac: 0.0,
                honeypot_frac: 0.0,
                payout_hold_secs: 0,
            },
            tier: ServiceTier::Batch,
        }
    }

    fn worker_cap() -> crate::types::WorkerCapability {
        crate::types::WorkerCapability {
            worker_id: uuid::Uuid::nil(),
            supplier_id: uuid::Uuid::nil(),
            agent_session_id: uuid::Uuid::nil(),
            hw_class: crate::types::HardwareClass::AppleSiliconMax,
            engine: "candle".into(),
            build_hash: String::new(),
            build_identity_policy: String::new(),
            hardware_identity: "Apple M2 Max".into(),
            memory_gb: 64.0,
            memory_bw_gbps: 400.0,
            gpu_count: 1,
            memory_gb_per_gpu: 64.0,
            interconnect: String::new(),
            supported_jobs: vec!["embed".into()],
            supported_models: vec!["all-minilm-l6-v2".into()],
            benchmarks: vec![],
            agent_version: String::new(),
            os_version: String::new(),
            min_payout_usd_hr: 0.0,
            sandboxed: false,
            unsandboxed_opt_in: false,
        }
    }

    /// Dispatch must follow the driver, not a hardcoded model kind.
    ///
    /// Before the driver boundary, `EmbedRunner::can_run` tested
    /// `matches!(manifest.model.kind, ModelKind::Hf)` directly, so a llama.cpp
    /// worker could be dispatched a cell it was registered for and refuse it.
    #[tokio::test]
    async fn dispatch_follows_the_configured_embed_runtime() {
        use crate::executor::{EmbedRunner, JobRunner};
        use crate::types::ModelKind;

        let candle_runner = EmbedRunner::new(Arc::new(CandleDriver::new()));
        let llama_runner = EmbedRunner::new(Arc::new(
            LlamaCppDriver::new(LlamaServerSupervision::Attach {
                base_url: "http://127.0.0.1:1".into(),
            })
            .unwrap(),
        ));
        let cap = worker_cap();
        let hf = embed_manifest(ModelKind::Hf);
        let gguf = embed_manifest(ModelKind::Gguf);

        assert!(candle_runner.can_run(&hf, &cap).await);
        assert!(!candle_runner.can_run(&gguf, &cap).await);
        assert!(llama_runner.can_run(&gguf, &cap).await);
        assert!(!llama_runner.can_run(&hf, &cap).await);
    }

    #[test]
    fn build_embed_driver_fails_closed_on_an_unknown_runtime() {
        assert_eq!(
            build_embed_driver("", "http://x").unwrap().runtime_id(),
            "candle_metal"
        );
        assert_eq!(
            build_embed_driver("llama_cpp_metal", "http://x")
                .unwrap()
                .runtime_id(),
            "llama_cpp_metal"
        );
        // Not a silent fall back to candle: a receipt naming a profile that
        // never executed anything is worse than a refusal at startup.
        assert!(build_embed_driver("tensorrt", "http://x").is_err());
    }

    /// The gate the control plane applies to a `cosine`-verified cell.
    /// `src/control/verification.go` — two runs of the same implementation were what
    /// it was calibrated for; whether a different engine clears it is the whole
    /// question this test answers.
    const EMBEDDING_COSINE_THRESHOLD: f32 = 0.999;

    /// Both runtimes, same texts, same gate.
    ///
    /// Skipped unless MERC_LLAMA_EMBED_URL points at a llama-server started with
    /// `--embedding --pooling mean` on the pinned GGUF. Skipping rather than
    /// failing is deliberate: a developer without the engine running should not
    /// be told the runtimes disagree, which is a different and much louder claim
    /// than "not measured here".
    #[tokio::test]
    async fn candle_and_llama_cpp_agree_on_the_same_embed_cell() {
        let Ok(base_url) = std::env::var("MERC_LLAMA_EMBED_URL") else {
            eprintln!("skipping: MERC_LLAMA_EMBED_URL is not set");
            return;
        };

        let texts: Vec<String> = [
            "The water cycle moves water through the atmosphere, land and oceans.",
            "Merc settles every task against a receipt.",
            "short",
            "A considerably longer sentence, written so the comparison covers more \
             than one length bucket and exercises the tokenizer past its first few tokens.",
            "Numbers like 3.14159 and 2026-07-30 appear in real buyer input.",
            "",
        ]
        .iter()
        .map(|s| s.to_string())
        .collect();

        let candle = CandleDriver::new();
        let llama = LlamaCppDriver::new(LlamaServerSupervision::Attach { base_url }).unwrap();
        candle.launch().await.expect("candle launch");
        llama.launch().await.expect("llama-server must be serving");

        let pool = ModelPool::new();
        let theirs = llama
            .embed("all-minilm-l6-v2", &texts, &pool)
            .await
            .expect("llama.cpp embed");
        let ours = candle
            .embed("all-minilm-l6-v2", &texts, &pool)
            .await
            .expect("candle embed");

        assert_eq!(ours.len(), texts.len());
        assert_eq!(theirs.len(), texts.len());

        let mut min = f32::MAX;
        let mut total = 0.0_f32;
        for (i, (a, b)) in ours.iter().zip(&theirs).enumerate() {
            assert_eq!(a.len(), 384, "candle row {i} has {} dims", a.len());
            assert_eq!(b.len(), 384, "llama.cpp row {i} has {} dims", b.len());
            let c = cosine(a, b).unwrap_or_else(|| panic!("row {i} has no cosine"));
            min = min.min(c);
            total += c;
        }
        let mean = total / ours.len() as f32;
        eprintln!("cross-runtime embed cosine: min={min:.6} mean={mean:.6}");
        assert!(
            min >= EMBEDDING_COSINE_THRESHOLD,
            "min cosine {min:.6} is below the {EMBEDDING_COSINE_THRESHOLD} gate the control \
             plane applies to a cosine-verified cell"
        );

        // Both drivers counted the same work through the same boundary.
        assert_eq!(candle.metrics().texts, texts.len() as u64);
        assert_eq!(llama.metrics().texts, texts.len() as u64);
        assert_eq!(candle.metrics().failures, 0);
        assert_eq!(llama.metrics().failures, 0);
    }
}
