package main

import (
	"bytes"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"testing"

	"github.com/google/uuid"
)

func canonicalRealtimeCacheBody(t testing.TB, schema any, seed int) []byte {
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
	prepared := preparedRealtimeRequest{
		Body: body, Profile: profile, bodySHA256: sha256.Sum256(body),
	}
	preparedIdentity, err := realtimeIdentityFromPreparedBody(buyer, profile, body, prepared.bodySHA256)
	must(t, err)
	if preparedIdentity != first {
		t.Fatalf("prepared identity=%q differs from body identity=%q", preparedIdentity, first)
	}
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
	changedPolicy := profile
	changedPolicy.GenerationPolicy.Version += "-cache-invalidation"
	changedPolicyIdentity, err := realtimeIdentityFromPreparedBody(buyer, changedPolicy, body)
	if err != nil || changedPolicyIdentity == first {
		t.Fatalf("generation policy revision did not invalidate identity: changed=%q first=%q err=%v", changedPolicyIdentity, first, err)
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

func BenchmarkRealtimeIdentityCacheHitPrepared(b *testing.B) {
	resetRealtimeIdentityCacheForTest()
	defer resetRealtimeIdentityCacheForTest()
	profile := sortedVLLMProfiles()[0]
	body, err := canonicalJSON(map[string]any{
		"model": "cx-chat-1b", "temperature": 0.0, "top_p": 1.0,
		"messages":        []any{map[string]any{"role": "user", "content": "benchmark-prepared"}},
		"tools":           []any{map[string]any{"type": "function", "function": map[string]any{"name": "weather"}}},
		"response_format": map[string]any{"type": "json_object"},
	})
	if err != nil {
		b.Fatal(err)
	}
	prepared := preparedRealtimeRequest{
		Body: body, Profile: profile, bodySHA256: sha256.Sum256(body),
	}
	buyer := uuid.New()
	if _, err := realtimeIdentityFromPreparedBody(buyer, profile, body, prepared.bodySHA256); err != nil {
		b.Fatal(err)
	}
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if _, err := realtimeIdentityFromPreparedBody(buyer, profile, body, prepared.bodySHA256); err != nil {
			b.Fatal(err)
		}
	}
}

func TestRealtimeIdentityCanonicalProjectionMatchesNestedDecoder(t *testing.T) {
	profile := sortedVLLMProfiles()[0]
	buyer := uuid.New()
	cases := []map[string]any{
		{
			"model": "cx-chat-1b",
			"messages": []any{map[string]any{
				"role": "user", "content": "escaped <tag> \u2028 \u2029",
			}},
			"tools": []any{map[string]any{
				"type": "function", "function": map[string]any{
					"name": "weather", "parameters": map[string]any{
						"type": "object", "properties": map[string]any{
							"city": map[string]any{"type": "string"},
						},
					},
				},
			}},
			"response_format": map[string]any{
				"type": "json_schema", "json_schema": map[string]any{"name": "answer"},
			},
			"temperature": 0.0, "top_p": 1.0,
			"seed": 9007199254740993, "max_tokens": 8,
		},
		{
			"model":       "cx-chat-1b",
			"messages":    []any{map[string]any{"role": "user", "content": "plain"}},
			"temperature": 0, "top_p": 1, "max_completion_tokens": 16,
			"stream": false, "stream_options": map[string]any{"include_usage": true},
			"user": "buyer-tag",
		},
		{
			"model":                 "cx-chat-1b",
			"messages":              []any{map[string]any{"role": "user", "content": "legacy fallback"}},
			"max_completion_tokens": "not-an-integer", "max_tokens": 7,
		},
	}
	for index, payload := range cases {
		body, err := canonicalJSON(payload)
		mustf(t, err, "case %d: canonical body: %v", index)
		decoder := json.NewDecoder(bytes.NewReader(body))
		decoder.UseNumber()
		var nested map[string]any
		mustf(t, decoder.Decode(&nested), "case %d: nested decode: %v", index)
		want, wantErr := realtimeIdentityFromPayload(buyer, profile, nested)
		got, gotErr := realtimeIdentityFromCanonicalBody(buyer, profile, body)
		if (wantErr == nil) != (gotErr == nil) || want != got {
			t.Fatalf("case %d: raw identity=%q err=%v nested identity=%q err=%v", index, got, gotErr, want, wantErr)
		}
	}
}

func BenchmarkRealtimeIdentityDerivation(b *testing.B) {
	profile := sortedVLLMProfiles()[0]
	body := canonicalRealtimeCacheBody(b, map[string]any{
		"type": "json_schema", "json_schema": map[string]any{"name": "answer"},
	}, 42)
	buyer := uuid.New()
	b.Run("raw_top_level", func(b *testing.B) {
		b.ReportAllocs()
		for i := 0; i < b.N; i++ {
			if _, err := realtimeIdentityFromCanonicalBody(buyer, profile, body); err != nil {
				b.Fatal(err)
			}
		}
	})
	b.Run("nested_decode", func(b *testing.B) {
		b.ReportAllocs()
		for i := 0; i < b.N; i++ {
			decoder := json.NewDecoder(bytes.NewReader(body))
			decoder.UseNumber()
			var payload map[string]any
			if err := decoder.Decode(&payload); err != nil {
				b.Fatal(err)
			}
			if _, err := realtimeIdentityFromPayload(buyer, profile, payload); err != nil {
				b.Fatal(err)
			}
		}
	})
}
