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

/// Drop measured residency for models that are no longer resident. Called when
/// the pool evicts idle warm models so the next heartbeat cannot re-advertise
/// numbers that belong to a process that already freed the weights.
pub fn clear_residency(keys: &[String]) {
    if keys.is_empty() {
        return;
    }
    let mut table = residency_table()
        .lock()
        .expect("residency table mutex poisoned");
    for key in keys {
        table.remove(key);
    }
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
        // Resolve first so an unrecognised ref fails closed before any slot is
        // created or weights are loaded under a neighbour's identity.
        let key = canonical_llama_id(model_ref)?;
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
        // Drop measurements with the weights. The control plane learns via the
        // heartbeat's evicted_models list; keeping numbers here would let a
        // subsequent loaded-model snapshot re-report a free model as resident.
        clear_residency(&stale);
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

/// Pool key for a generative model. Derived from the resolved governed id so
/// two distinct routable models never share a slot. Unresolvable refs error
/// rather than alias onto a resident model.
fn canonical_llama_id(model_ref: &str) -> Result<String, RunError> {
    Ok(models::llama_spec(model_ref)?.0.to_string())
}

#[cfg(test)]
mod tests {
    use super::*;
    use crate::models::INFER_LLAMA_ID;

    #[test]
    fn canonical_llama_id_keys_by_resolved_model_not_constant_alias() {
        let known = canonical_llama_id(INFER_LLAMA_ID).expect("governed model must resolve");
        assert_eq!(known, INFER_LLAMA_ID);

        // A second distinct ref must not collapse onto the same pool key. With
        // only one generative model governed today that means fail closed.
        let foreign_a = canonical_llama_id("foreign-model-a");
        let foreign_b = canonical_llama_id("foreign-model-b");
        match (&foreign_a, &foreign_b) {
            (Ok(a), Ok(b)) => {
                assert_ne!(
                    a, b,
                    "distinct resolvable model refs must not share a pool key"
                );
                assert_ne!(a, &known);
                assert_ne!(b, &known);
            }
            (Err(e), Err(_)) => {
                let msg = e.to_string();
                assert!(
                    msg.contains("unresolvable generative model ref"),
                    "expected fail-closed greppable error, got {msg}"
                );
            }
            _ => panic!("inconsistent resolution for foreign model refs"),
        }
    }

    #[tokio::test]
    async fn model_pool_llama_unresolvable_errors_rather_than_aliasing() {
        let pool = ModelPool::new();
        let result = pool.llama("not-a-governed-generative-model").await;
        let err = match result {
            Ok(_) => panic!("unresolvable ref must fail closed, not alias onto a resident model"),
            Err(e) => e,
        };
        let msg = err.to_string();
        assert!(
            msg.contains("unresolvable generative model ref"),
            "expected greppable fail-closed error, got {msg}"
        );
        // No slot must have been warmed under the governed id.
        assert!(
            pool.loaded_model_ids().await.is_empty(),
            "aliasing would leave the governed model resident"
        );
    }
}
