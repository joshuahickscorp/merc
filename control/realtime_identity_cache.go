package main

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/google/uuid"
)

// The prepared realtime body is canonical JSON. Caching its derived identity
// avoids re-decoding and re-canonicalising the same tools/response-schema
// payload across the exact-reuse, coalescing, and cache-population callers.
// This is deliberately an identity cache, not a tokenizer: the control plane
// has no model tokenizer and must not manufacture token IDs or price from one.
const (
	realtimeIdentityCacheMaxEntries = 256
	realtimeIdentityCacheTTL        = 2 * time.Minute
	realtimeIdentityCacheMaxBody    = 1 << 20
)

type realtimeIdentityCacheEntry struct {
	identity  string
	expiresAt time.Time
	lastUsed  uint64
}

var realtimeIdentityCache = struct {
	sync.Mutex
	entries map[string]realtimeIdentityCacheEntry
	clock   uint64
	hits    uint64
	misses  uint64
}{entries: make(map[string]realtimeIdentityCacheEntry)}

func realtimeIdentityCacheKey(buyerID uuid.UUID, profile VLLMRuntimeProfile, body []byte) string {
	sum := sha256.Sum256(body)
	// Tenant, profile digest/revision, and generation-policy revision are part of
	// the cache key. A buyer can therefore never observe another tenant's
	// identity, and a runtime/policy update naturally invalidates old entries.
	return buyerID.String() + "\x00" + profile.RuntimeProfileID + "\x00" +
		profile.ProfileSHA256 + "\x00" + profile.ModelRevision + "\x00" +
		profile.GenerationPolicy.Version + "\x00" + hex.EncodeToString(sum[:])
}

// realtimeIdentityFromPreparedBody is the production caller. `body` is the
// already bounded, canonical body produced by prepareRealtimeRequest, so a
// cache hit has a semantic request key without trusting any buyer-supplied
// identity header. Non-deterministic requests still refuse identity creation
// exactly as realtimeIdentityFromPayload does.
func realtimeIdentityFromPreparedBody(
	buyerID uuid.UUID, profile VLLMRuntimeProfile, body []byte,
) (string, error) {
	if buyerID == uuid.Nil || len(body) == 0 || len(body) > realtimeIdentityCacheMaxBody {
		return "", errors.New("realtime identity body is outside bounded cache input")
	}
	key := realtimeIdentityCacheKey(buyerID, profile, body)
	now := time.Now().UTC()
	realtimeIdentityCache.Lock()
	if entry, ok := realtimeIdentityCache.entries[key]; ok {
		if now.Before(entry.expiresAt) {
			realtimeIdentityCache.clock++
			entry.lastUsed = realtimeIdentityCache.clock
			realtimeIdentityCache.entries[key] = entry
			realtimeIdentityCache.hits++
			realtimeIdentityCache.Unlock()
			return entry.identity, nil
		}
		delete(realtimeIdentityCache.entries, key)
	}
	realtimeIdentityCache.misses++
	realtimeIdentityCache.Unlock()

	payload, err := decodeRealtimePayload(body)
	if err != nil {
		return "", err
	}
	identity, err := realtimeIdentityFromPayload(buyerID, profile, payload)
	if err != nil {
		return "", err
	}

	realtimeIdentityCache.Lock()
	realtimeIdentityCache.clock++
	if len(realtimeIdentityCache.entries) >= realtimeIdentityCacheMaxEntries {
		var oldestKey string
		var oldest uint64
		for candidateKey, candidate := range realtimeIdentityCache.entries {
			if oldestKey == "" || candidate.lastUsed < oldest {
				oldestKey, oldest = candidateKey, candidate.lastUsed
			}
		}
		if oldestKey != "" {
			delete(realtimeIdentityCache.entries, oldestKey)
		}
	}
	realtimeIdentityCache.entries[key] = realtimeIdentityCacheEntry{
		identity: identity, expiresAt: now.Add(realtimeIdentityCacheTTL), lastUsed: realtimeIdentityCache.clock,
	}
	realtimeIdentityCache.Unlock()
	return identity, nil
}

type realtimeIdentityCacheStats struct {
	Hits    uint64
	Misses  uint64
	Entries int
}

func realtimeIdentityCacheStatsSnapshot() realtimeIdentityCacheStats {
	realtimeIdentityCache.Lock()
	defer realtimeIdentityCache.Unlock()
	return realtimeIdentityCacheStats{
		Hits: realtimeIdentityCache.hits, Misses: realtimeIdentityCache.misses, Entries: len(realtimeIdentityCache.entries),
	}
}

func resetRealtimeIdentityCacheForTest() {
	realtimeIdentityCache.Lock()
	realtimeIdentityCache.entries = make(map[string]realtimeIdentityCacheEntry)
	realtimeIdentityCache.clock = 0
	realtimeIdentityCache.hits = 0
	realtimeIdentityCache.misses = 0
	realtimeIdentityCache.Unlock()
}

func validateRealtimeIdentityCacheBounds() error {
	if realtimeIdentityCacheMaxEntries < 1 || realtimeIdentityCacheTTL <= 0 || realtimeIdentityCacheMaxBody < 1 {
		return fmt.Errorf("realtime identity cache bounds are invalid")
	}
	return nil
}
