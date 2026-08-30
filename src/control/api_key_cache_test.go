package main

import (
	"testing"
	"time"

	"github.com/google/uuid"
)

func TestAPIKeyCacheDigestKeysPreserveExpiryAndRevocation(t *testing.T) {
	cache := &apiKeyCache{}
	buyerID, keyID := uuid.New(), uuid.New()
	digest := apiKeyCacheKeyFor("cache-boundary-key")
	cache.put(digest, AuthResult{BuyerID: buyerID, APIKeyID: keyID})
	if got, ok := cache.get(digest); !ok || got.APIKeyID != keyID || got.BuyerID != buyerID {
		t.Fatalf("cached auth = %+v, ok=%t", got, ok)
	}
	cache.invalidateID(keyID)
	if _, ok := cache.get(digest); ok {
		t.Fatal("revoked API key remained in the cache")
	}

	cache.put(digest, AuthResult{BuyerID: buyerID, APIKeyID: keyID})
	cache.invalidateBuyer(buyerID)
	if _, ok := cache.get(digest); ok {
		t.Fatal("tombstoned buyer retained an API key cache entry")
	}

	cache.put(digest, AuthResult{BuyerID: buyerID, APIKeyID: keyID})
	cache.mu.Lock()
	entry := cache.entries[digest]
	entry.expiresAt = time.Now().Add(-time.Second)
	cache.entries[digest] = entry
	cache.mu.Unlock()
	if _, ok := cache.get(digest); ok {
		t.Fatal("expired API key cache entry remained valid")
	}
}
