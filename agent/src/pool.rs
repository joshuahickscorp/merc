use std::collections::HashMap;
use std::sync::atomic::{AtomicUsize, Ordering};
use std::sync::{Arc, OnceLock};
use std::time::{Duration, Instant};

use serde::Serialize;
use sysinfo::{Pid, ProcessesToUpdate, System};
use tokio::sync::{Mutex, OnceCell};

use crate::executor::{Embedder, LlamaBackend, RunError};
use crate::models;

static LOAD_COUNT: AtomicUsize = AtomicUsize::new(0);

pub fn loads() -> usize {
    LOAD_COUNT.load(Ordering::SeqCst)
}

pub(crate) fn note_load() {
    LOAD_COUNT.fetch_add(1, Ordering::SeqCst);
}

fn read_own_rss_bytes() -> u64 {
    let pid = Pid::from_u32(std::process::id());
    let mut sys = System::new();
    sys.refresh_processes(ProcessesToUpdate::Some(&[pid]));
    sys.process(pid).map(|p| p.memory()).unwrap_or(0)
}

#[derive(Debug, Clone, Copy, Serialize)]
pub struct ResidencyMeasurement {
    pub rss_delta_bytes: i64,
    pub load_ms: u64,
}

static RESIDENCY: OnceLock<std::sync::Mutex<HashMap<String, ResidencyMeasurement>>> =
    OnceLock::new();

fn residency_table() -> &'static std::sync::Mutex<HashMap<String, ResidencyMeasurement>> {
    RESIDENCY.get_or_init(|| std::sync::Mutex::new(HashMap::new()))
}

fn record_residency(key: &str, rss_delta_bytes: i64, load_ms: u64) {
    let mut table = residency_table()
        .lock()
        .expect("residency table mutex poisoned");
    table
        .entry(key.to_string())
        .or_insert(ResidencyMeasurement {
            rss_delta_bytes,
            load_ms,
        });
}

pub fn residency_snapshot() -> HashMap<String, ResidencyMeasurement> {
    residency_table()
        .lock()
        .expect("residency table mutex poisoned")
        .clone()
}

type Warm<T> = Arc<OnceCell<Arc<Mutex<T>>>>;

#[derive(Clone, Default)]
pub struct ModelPool {
    embedders: Arc<Mutex<HashMap<String, Warm<Embedder>>>>,
    llama: Arc<Mutex<HashMap<String, Warm<LlamaBackend>>>>,
    last_used: Arc<Mutex<HashMap<String, Instant>>>,
}

impl ModelPool {
    pub fn new() -> Self {
        Self::default()
    }

    pub async fn embedder(&self, model_ref: &str) -> Result<Arc<Mutex<Embedder>>, RunError> {
        let key = canonical_embed_id(model_ref);
        let cell = self.slot(&self.embedders, &key).await;
        let model_ref = model_ref.to_string();
        let measure_key = key.clone();
        let result = cell
            .get_or_try_init(|| async {
                let e = tokio::task::spawn_blocking(move || {
                    let rss_before = read_own_rss_bytes();
                    let started = Instant::now();
                    note_load();
                    let loaded = Embedder::load(&model_ref);
                    let load_ms = started.elapsed().as_millis() as u64;
                    let rss_after = read_own_rss_bytes();
                    record_residency(&measure_key, rss_after as i64 - rss_before as i64, load_ms);
                    loaded
                })
                .await
                .map_err(join_err("embed"))??;
                Ok::<_, RunError>(Arc::new(Mutex::new(e)))
            })
            .await
            .cloned();
        self.touch(&key).await;
        result
    }

    pub async fn llama(&self, model_ref: &str) -> Result<Arc<Mutex<LlamaBackend>>, RunError> {
        let key = canonical_llama_id(model_ref);
        let cell = self.slot(&self.llama, &key).await;
        let model_ref = model_ref.to_string();
        let measure_key = key.clone();
        let result = cell
            .get_or_try_init(|| async {
                let b = tokio::task::spawn_blocking(move || {
                    let rss_before = read_own_rss_bytes();
                    let started = Instant::now();
                    note_load();
                    let loaded = LlamaBackend::load(&model_ref);
                    let load_ms = started.elapsed().as_millis() as u64;
                    let rss_after = read_own_rss_bytes();
                    record_residency(&measure_key, rss_after as i64 - rss_before as i64, load_ms);
                    loaded
                })
                .await
                .map_err(join_err("batch_infer"))??;
                Ok::<_, RunError>(Arc::new(Mutex::new(b)))
            })
            .await
            .cloned();
        self.touch(&key).await;
        result
    }

    async fn slot<T>(&self, map: &Arc<Mutex<HashMap<String, Warm<T>>>>, key: &str) -> Warm<T> {
        let mut g = map.lock().await;
        g.entry(key.to_string())
            .or_insert_with(|| Arc::new(OnceCell::new()))
            .clone()
    }

    async fn touch(&self, key: &str) {
        self.last_used
            .lock()
            .await
            .insert(key.to_string(), Instant::now());
    }

    pub async fn evict_idle(&self, max_idle: Duration) -> Vec<String> {
        let now = Instant::now();
        let stale: Vec<String> = {
            let last_used = self.last_used.lock().await;
            last_used
                .iter()
                .filter(|(_, &t)| now.duration_since(t) >= max_idle)
                .map(|(k, _)| k.clone())
                .collect()
        };
        if stale.is_empty() {
            return stale;
        }
        {
            let mut embedders = self.embedders.lock().await;
            let mut llama = self.llama.lock().await;
            let mut last_used = self.last_used.lock().await;
            for key in &stale {
                embedders.remove(key);
                llama.remove(key);
                last_used.remove(key);
            }
        }
        stale
    }

    pub async fn loaded_model_ids(&self) -> Vec<String> {
        let mut ids = Vec::new();
        collect_warm_ids(&*self.embedders.lock().await, &mut ids);
        collect_warm_ids(&*self.llama.lock().await, &mut ids);
        ids.sort(); // stable order so the heartbeat payload is deterministic
        ids
    }
}

fn canonical_embed_id(model_ref: &str) -> String {
    models::embed_spec(model_ref).0.to_string()
}

fn collect_warm_ids<T>(map: &HashMap<String, Warm<T>>, ids: &mut Vec<String>) {
    for (id, cell) in map.iter() {
        if cell.get().is_some() {
            ids.push(id.clone());
        }
    }
}

fn join_err(backend: &'static str) -> impl Fn(tokio::task::JoinError) -> RunError {
    move |e| RunError::Inference {
        backend,
        msg: format!("worker thread failed: {e}"),
    }
}

fn canonical_llama_id(_model_ref: &str) -> String {
    models::INFER_LLAMA_ID.to_string()
}
