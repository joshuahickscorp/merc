package main

import (
	"fmt"
	"testing"

	"github.com/google/uuid"
)

func canonicalRealtimeCacheBody(t *testing.T, schema any, seed int) []byte {
	t.Helper()
	payload := map[string]any{
		"model":       "cx-chat-1b",
		"temperature": 0.0,
		"top_p":       1.0,
		"messages":    []any{map[string]any{"role": "user", "content": fmt.Sprintf("hello-%d", seed)}},
		"tools": []any{map[string]any{
			"type": "function", "function": map[string]any{
				"name": "weather", "parameters": map[string]any{"type": "object", "properties": map[string]any{"city": map[string]any{"type": "string"}}},
			},
		}},
		"response_format": schema,
	}
	body, err := canonicalJSON(payload)
	must(t, err)
	return body
}

func TestRealtimeIdentityCacheScopesSemanticRequestAndEvictsBoundedly(t *testing.T) {
	resetRealtimeIdentityCacheForTest()
	t.Cleanup(resetRealtimeIdentityCacheForTest)
	must(t, validateRealtimeIdentityCacheBounds())
	profile := sortedVLLMProfiles()[0]
	buyer := uuid.New()
	body := canonicalRealtimeCacheBody(t, map[string]any{"type": "json_schema", "json_schema": map[string]any{"name": "answer"}}, 1)
	first, err := realtimeIdentityFromPreparedBody(buyer, profile, body)
	must(t, err)
	second, err := realtimeIdentityFromPreparedBody(buyer, profile, body)
	if err != nil || second != first {
		t.Fatalf("cache hit identity=%q first=%q err=%v", second, first, err)
	}
	changedSchema := canonicalRealtimeCacheBody(t, map[string]any{"type": "json_schema", "json_schema": map[string]any{"name": "different"}}, 1)
	changed, err := realtimeIdentityFromPreparedBody(buyer, profile, changedSchema)
	if err != nil || changed == first {
		t.Fatalf("tool/schema change reused semantic identity: changed=%q first=%q err=%v", changed, first, err)
	}
	otherTenant, err := realtimeIdentityFromPreparedBody(uuid.New(), profile, body)
	if err != nil || otherTenant == first {
		t.Fatalf("tenant-scoped identity leaked across cache key: other=%q first=%q err=%v", otherTenant, first, err)
	}
	changedProfile := profile
	changedProfile.ModelRevision += "-cache-invalidation"
	changedRevision, err := realtimeIdentityFromPreparedBody(buyer, changedProfile, body)
	if err != nil || changedRevision == first {
		t.Fatalf("profile revision did not invalidate identity: changed=%q first=%q err=%v", changedRevision, first, err)
	}
	oversized := make([]byte, realtimeIdentityCacheMaxBody+1)
	if _, err := realtimeIdentityFromPreparedBody(buyer, profile, oversized); err == nil {
		t.Fatal("oversized identity body entered cache")
	}
	for seed := 0; seed < realtimeIdentityCacheMaxEntries+32; seed++ {
		if _, err := realtimeIdentityFromPreparedBody(buyer, profile, canonicalRealtimeCacheBody(t, map[string]any{"type": "json_schema", "json_schema": map[string]any{"name": "bounded"}}, seed+100)); err != nil {
			t.Fatal(err)
		}
	}
	stats := realtimeIdentityCacheStatsSnapshot()
	if stats.Hits < 1 || stats.Misses < 4 || stats.Entries > realtimeIdentityCacheMaxEntries {
		t.Fatalf("identity cache stats=%+v, want hit/miss evidence and bounded entries", stats)
	}
}

func BenchmarkRealtimeIdentityCacheHit(b *testing.B) {
	resetRealtimeIdentityCacheForTest()
	defer resetRealtimeIdentityCacheForTest()
	profile := sortedVLLMProfiles()[0]
	body, err := canonicalJSON(map[string]any{
		"model": "cx-chat-1b", "temperature": 0.0, "top_p": 1.0,
		"messages":        []any{map[string]any{"role": "user", "content": "benchmark"}},
		"tools":           []any{map[string]any{"type": "function", "function": map[string]any{"name": "weather"}}},
		"response_format": map[string]any{"type": "json_object"},
	})
	if err != nil {
		b.Fatal(err)
	}
	buyer := uuid.New()
	if _, err := realtimeIdentityFromPreparedBody(buyer, profile, body); err != nil {
		b.Fatal(err)
	}
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if _, err := realtimeIdentityFromPreparedBody(buyer, profile, body); err != nil {
			b.Fatal(err)
		}
	}
}
