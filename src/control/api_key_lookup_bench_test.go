package main

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"
)

func BenchmarkLookupAPIKeyCacheHitParallel(b *testing.B) {
	const rawKey = "merc-api-key-cache-benchmark"
	store := &Store{}
	keyHash := apiKeyCacheKeyFor(rawKey)
	store.apiKeys.put(keyHash, AuthResult{BuyerID: uuid.New(), APIKeyID: uuid.New()})
	store.apiKeys.mu.Lock()
	entry := store.apiKeys.entries[keyHash]
	entry.expiresAt = time.Now().Add(time.Hour)
	store.apiKeys.entries[keyHash] = entry
	store.apiKeys.mu.Unlock()

	ctx := context.Background()
	b.ReportAllocs()
	b.RunParallel(func(pb *testing.PB) {
		for pb.Next() {
			if _, err := store.LookupAPIKey(ctx, rawKey); err != nil {
				b.Fatal(err)
			}
		}
	})
}
