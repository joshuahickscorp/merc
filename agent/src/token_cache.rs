//! Bounded semantic tokenization cache used by the production embedding lane.
//!
//! The cache is deliberately narrower than a model/output cache: it stores only
//! the tokenizer's deterministic encoding for an exact model revision and input
//! text.  A revisioned key, byte/entry ceilings, and LRU eviction make reuse
//! observable without allowing an old tokenizer or an unbounded buyer corpus to
//! become execution authority.

use std::collections::{HashMap, VecDeque};
use std::sync::Mutex;

use sha2::{Digest, Sha256};
use tokenizers::Encoding;

const CACHE_REVISION: &str = "tokenization-cache-v2";
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

/// Domain-separated SHA-256 identity for a (revision, model_revision, text)
/// triple. Fixed-size so the map key cannot retain buyer text, and collision
/// probability is cryptographic rather than birthday-on-64-bits.
#[derive(Debug, Clone, PartialEq, Eq, Hash)]
pub struct TokenCacheKey([u8; 32]);

#[derive(Debug)]
struct State {
    entries: HashMap<TokenCacheKey, Entry>,
    order: VecDeque<TokenCacheKey>,
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

    /// Exact identity for a model revision and input text. Domain-separated
    /// SHA-256 matches every other identity in this tree (request identity,
    /// prefix routing): a 64-bit non-cryptographic hash is not acceptable here
    /// because a silent wrong tokenization is worse than a miss.
    pub fn key(model_revision: &str, text: &str) -> TokenCacheKey {
        let mut h = Sha256::new();
        h.update(CACHE_REVISION.as_bytes());
        h.update([0]);
        h.update(model_revision.as_bytes());
        h.update([0]);
        h.update(text.as_bytes());
        let digest = h.finalize();
        let mut out = [0_u8; 32];
        out.copy_from_slice(&digest);
        TokenCacheKey(out)
    }

    pub fn get(&self, key: &TokenCacheKey) -> Option<Encoding> {
        let mut state = self
            .state
            .lock()
            .expect("tokenization cache mutex poisoned");
        let entry = state.entries.get(key).cloned();
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

    pub fn insert(&self, key: TokenCacheKey, encoding: Encoding) {
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
            remove_key(&mut state.order, &key);
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
        state.entries.insert(key.clone(), Entry { encoding, bytes });
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

fn touch(order: &mut VecDeque<TokenCacheKey>, key: &TokenCacheKey) {
    remove_key(order, key);
    order.push_back(key.clone());
}

// O(n) scan over the LRU order on every get/insert. Fine at the 2048-entry
// ceiling (DEFAULT_MAX_ENTRIES); raise that cap only with a better index.
fn remove_key(order: &mut VecDeque<TokenCacheKey>, key: &TokenCacheKey) {
    if let Some(pos) = order.iter().position(|candidate| candidate == key) {
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
    fn get_never_returns_encoding_for_text_not_stored() {
        let cache = TokenizationCache::new(8, 1024);
        let stored = encoding(1);
        let key_one = TokenizationCache::key("rev", "text-one");
        let key_two = TokenizationCache::key("rev", "text-two");
        assert_ne!(
            key_one, key_two,
            "distinct texts must not share a cache identity"
        );
        cache.insert(key_one.clone(), stored);
        assert!(
            cache.get(&key_one).is_some(),
            "stored text must hit"
        );
        assert!(
            cache.get(&key_two).is_none(),
            "a text that was never stored must miss — silent wrong tokenization is forbidden"
        );
        // A fabricated key that never went through insert must also miss.
        let forged = TokenCacheKey([0xAB; 32]);
        assert!(cache.get(&forged).is_none());
    }

    #[test]
    fn bounded_lru_reports_hits_misses_and_evictions() {
        let cache = TokenizationCache::new(1, 1024);
        let first = encoding(1);
        let second = encoding(2);
        let key1 = TokenizationCache::key("rev", "one");
        let key2 = TokenizationCache::key("rev", "two");
        assert!(cache.get(&key1).is_none());
        cache.insert(key1.clone(), first);
        assert!(cache.get(&key1).is_some());
        cache.insert(key2, second);
        assert!(cache.get(&key1).is_none());
        let stats = cache.stats();
        assert_eq!(stats.hits, 1);
        assert_eq!(stats.misses, 2);
        assert_eq!(stats.evictions, 1);
        assert_eq!(stats.entries, 1);
    }
}
