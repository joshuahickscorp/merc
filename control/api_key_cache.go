package main

import (
	"sync"
	"time"

	"github.com/google/uuid"
)

// API key authentication cache.
//
// Revocation argument (this is a security decision, not a latency one):
//
// A cached positive AuthResult keeps a key "valid" after the database row
// has been revoked (or the buyer tombstoned). The window of that lie is
// bounded as follows:
//
//  1. Same-process revoke / tombstone: RevokeAPIKey and TombstoneBuyer
//     invalidate the cache entry before returning success to the caller.
//     An operator who revokes via the instance that holds the hot entry
//     sees the key dead on the next request. Zero intentional lag.
//
//  2. Multi-instance: there is no cross-process invalidation bus. The
//     remaining lag is the TTL below. A request that lands on a different
//     process than the one that accepted the revoke may still authenticate
//     for up to apiKeyCacheTTL.
//
//  3. Chosen TTL: 250 ms. Why this is acceptable for Merc buyer API keys:
//     - Compromised-key response is measured in the time an operator can
//     notice and act (seconds to minutes). A 250 ms residual on other
//     processes does not change that order of magnitude.
//     - Auth is necessary but not sufficient to spend. AuthorizeRealtimeContract
//     still takes the durable funding and capacity path; a revoked key
//     cannot open new spend after the TTL, and in-flight EXECUTING work
//     is already reserved under the buyer ceiling regardless of key state.
//     - Negative results (unknown / revoked / tombstoned) are never cached,
//     so an attacker cannot pin a "denied" entry, and a re-minted key is
//     not blocked by a stale miss.
//     - Break-glass admin credentials (IsAdmin) are never cached.
//
//  4. Crash does not extend the window: the cache is process-local.
//
// If a future threat model requires zero multi-instance lag, replace the
// TTL with LISTEN/NOTIFY (or a shared generation counter) rather than
// lengthening this window. Until then, 250 ms is the accepted bound.
const apiKeyCacheTTL = 250 * time.Millisecond

const apiKeyCacheMaxEntries = 4096

type apiKeyCacheEntry struct {
	auth      AuthResult
	expiresAt time.Time
}

// apiKeyCache is embedded on Store. Nil-safe: a zero Store has no cache.
type apiKeyCache struct {
	mu      sync.Mutex
	entries map[string]apiKeyCacheEntry // key_hash -> entry
	byID    map[uuid.UUID]string        // api_key id -> key_hash (for revoke)
	byBuyer map[uuid.UUID]map[string]struct{}
}

func (c *apiKeyCache) get(keyHash string) (AuthResult, bool) {
	if c == nil {
		return AuthResult{}, false
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.entries == nil {
		return AuthResult{}, false
	}
	e, ok := c.entries[keyHash]
	if !ok {
		return AuthResult{}, false
	}
	if time.Now().After(e.expiresAt) {
		c.removeLocked(keyHash)
		return AuthResult{}, false
	}
	return e.auth, true
}

func (c *apiKeyCache) put(keyHash string, auth AuthResult) {
	if c == nil || auth.IsAdmin || auth.APIKeyID == uuid.Nil {
		// Never cache break-glass admin credentials.
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.entries == nil {
		c.entries = make(map[string]apiKeyCacheEntry)
		c.byID = make(map[uuid.UUID]string)
		c.byBuyer = make(map[uuid.UUID]map[string]struct{})
	}
	// Bound memory: drop an arbitrary entry when full (revokes are rare;
	// a forced miss is correct, never a false positive).
	if len(c.entries) >= apiKeyCacheMaxEntries {
		for h := range c.entries {
			c.removeLocked(h)
			break
		}
	}
	// Replace any prior mapping for this id.
	if prev, ok := c.byID[auth.APIKeyID]; ok && prev != keyHash {
		c.removeLocked(prev)
	}
	c.entries[keyHash] = apiKeyCacheEntry{auth: auth, expiresAt: time.Now().Add(apiKeyCacheTTL)}
	c.byID[auth.APIKeyID] = keyHash
	set := c.byBuyer[auth.BuyerID]
	if set == nil {
		set = make(map[string]struct{})
		c.byBuyer[auth.BuyerID] = set
	}
	set[keyHash] = struct{}{}
}

func (c *apiKeyCache) invalidateID(id uuid.UUID) {
	if c == nil {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.byID == nil {
		return
	}
	if h, ok := c.byID[id]; ok {
		c.removeLocked(h)
	}
}

func (c *apiKeyCache) invalidateBuyer(buyerID uuid.UUID) {
	if c == nil {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.byBuyer == nil {
		return
	}
	for h := range c.byBuyer[buyerID] {
		c.removeLocked(h)
	}
}

func (c *apiKeyCache) removeLocked(keyHash string) {
	e, ok := c.entries[keyHash]
	if !ok {
		return
	}
	delete(c.entries, keyHash)
	delete(c.byID, e.auth.APIKeyID)
	if set := c.byBuyer[e.auth.BuyerID]; set != nil {
		delete(set, keyHash)
		if len(set) == 0 {
			delete(c.byBuyer, e.auth.BuyerID)
		}
	}
}

// resetAPIKeyCacheForTest clears the cache. Test-only.
func (s *Store) resetAPIKeyCacheForTest() {
	if s == nil {
		return
	}
	s.apiKeys.mu.Lock()
	s.apiKeys.entries = nil
	s.apiKeys.byID = nil
	s.apiKeys.byBuyer = nil
	s.apiKeys.mu.Unlock()
}
