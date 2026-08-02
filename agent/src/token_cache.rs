//! Bounded semantic tokenization cache used by the production embedding lane.
//!
//! The cache is deliberately narrower than a model/output cache: it stores only
//! the tokenizer's deterministic encoding for an exact model revision and input
//! text.  A revisioned key, byte/entry ceilings, and LRU eviction make reuse
//! observable without allowing an old tokenizer or an unbounded buyer corpus to
//! become execution authority.

use std::collections::{HashMap, VecDeque};
use std::hash::{Hash, Hasher};
use std::sync::Mutex;

use tokenizers::Encoding;

const CACHE_REVISION: &str = "tokenization-cache-v1";
const DEFAULT_MAX_ENTRIES: usize = 2048;
const DEFAULT_MAX_BYTES: usize = 16 * 1024 * 1024;

#[derive(Debug, Clone, Copy, Default, PartialEq, Eq)]
pub struct CacheStats {
    pub hits: u64,
    pub misses: u64,
    pub evictions: u64,
    pub bytes: usize,
    pub entries: usize,
}

#[derive(Debug, Clone)]
struct Entry {
    encoding: Encoding,
    bytes: usize,
}

#[derive(Debug)]
struct State {
    entries: HashMap<u64, Entry>,
    order: VecDeque<u64>,
    bytes: usize,
    hits: u64,
    misses: u64,
    evictions: u64,
}

/// A bounded, thread-safe cache. The lock is held only for map operations; the
/// tokenizer itself runs outside it so a slow encode cannot block readers of a
/// completed entry.
#[derive(Debug)]
pub struct TokenizationCache {
    max_entries: usize,
    max_bytes: usize,
    state: Mutex<State>,
}

impl Default for TokenizationCache {
    fn default() -> Self {
        Self::new(DEFAULT_MAX_ENTRIES, DEFAULT_MAX_BYTES)
    }
}

impl TokenizationCache {
    pub fn new(max_entries: usize, max_bytes: usize) -> Self {
        Self {
            max_entries: max_entries.max(1),
            max_bytes: max_bytes.max(1),
            state: Mutex::new(State {
                entries: HashMap::new(),
                order: VecDeque::new(),
                bytes: 0,
                hits: 0,
                misses: 0,
                evictions: 0,
            }),
        }
    }

    /// Return the exact key for a model revision and normalized input. Hashing
    /// keeps buyer text out of diagnostic state while still separating models,
    /// tokenizer revisions, and the cache schema.
    pub fn key(model_revision: &str, text: &str) -> u64 {
        let mut h = std::collections::hash_map::DefaultHasher::new();
        CACHE_REVISION.hash(&mut h);
        model_revision.hash(&mut h);
        text.hash(&mut h);
        h.finish()
    }

    pub fn get(&self, key: u64) -> Option<Encoding> {
        let mut state = self
            .state
            .lock()
            .expect("tokenization cache mutex poisoned");
        let entry = state.entries.get(&key).cloned();
        match entry {
            Some(entry) => {
                state.hits = state.hits.saturating_add(1);
                touch(&mut state.order, key);
                Some(entry.encoding)
            }
            None => {
                state.misses = state.misses.saturating_add(1);
                None
            }
        }
    }

    pub fn insert(&self, key: u64, encoding: Encoding) {
        let bytes = encoding_bytes(&encoding);
        if bytes > self.max_bytes {
            return;
        }
        let mut state = self
            .state
            .lock()
            .expect("tokenization cache mutex poisoned");
        if let Some(previous) = state.entries.remove(&key) {
            state.bytes = state.bytes.saturating_sub(previous.bytes);
            remove_key(&mut state.order, key);
        }
        while state.entries.len() >= self.max_entries
            || state.bytes.saturating_add(bytes) > self.max_bytes
        {
            let Some(oldest) = state.order.pop_front() else {
                break;
            };
            if let Some(old) = state.entries.remove(&oldest) {
                state.bytes = state.bytes.saturating_sub(old.bytes);
                state.evictions = state.evictions.saturating_add(1);
            }
        }
        state.bytes = state.bytes.saturating_add(bytes);
        state.entries.insert(key, Entry { encoding, bytes });
        state.order.push_back(key);
    }

    pub fn stats(&self) -> CacheStats {
        let state = self
            .state
            .lock()
            .expect("tokenization cache mutex poisoned");
        CacheStats {
            hits: state.hits,
            misses: state.misses,
            evictions: state.evictions,
            bytes: state.bytes,
            entries: state.entries.len(),
        }
    }
}

fn touch(order: &mut VecDeque<u64>, key: u64) {
    remove_key(order, key);
    order.push_back(key);
}

fn remove_key(order: &mut VecDeque<u64>, key: u64) {
    if let Some(pos) = order.iter().position(|candidate| *candidate == key) {
        order.remove(pos);
    }
}

fn encoding_bytes(encoding: &Encoding) -> usize {
    encoding
        .get_ids()
        .len()
        .saturating_mul(std::mem::size_of::<u32>())
        + encoding
            .get_attention_mask()
            .len()
            .saturating_mul(std::mem::size_of::<u32>())
        + encoding
            .get_type_ids()
            .len()
            .saturating_mul(std::mem::size_of::<u32>())
}

#[cfg(test)]
mod tests {
    use super::*;
    use tokenizers::Encoding;

    fn encoding(_id: u32) -> Encoding {
        Encoding::with_capacity(1)
    }

    #[test]
    fn semantic_key_is_revision_and_text_bound() {
        assert_eq!(
            TokenizationCache::key("rev", "x"),
            TokenizationCache::key("rev", "x")
        );
        assert_ne!(
            TokenizationCache::key("rev", "x"),
            TokenizationCache::key("rev2", "x")
        );
        assert_ne!(
            TokenizationCache::key("rev", "x"),
            TokenizationCache::key("rev", "y")
        );
    }

    #[test]
    fn bounded_lru_reports_hits_misses_and_evictions() {
        let cache = TokenizationCache::new(1, 1024);
        let first = encoding(1);
        let second = encoding(2);
        let key1 = TokenizationCache::key("rev", "one");
        let key2 = TokenizationCache::key("rev", "two");
        assert!(cache.get(key1).is_none());
        cache.insert(key1, first);
        assert!(cache.get(key1).is_some());
        cache.insert(key2, second);
        assert!(cache.get(key1).is_none());
        let stats = cache.stats();
        assert_eq!(stats.hits, 1);
        assert_eq!(stats.misses, 2);
        assert_eq!(stats.evictions, 1);
        assert_eq!(stats.entries, 1);
    }
}
