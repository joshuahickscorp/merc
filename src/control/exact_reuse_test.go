package main

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"testing"

	"github.com/google/uuid"
)

// A fixed tenant, so two calls with the same input agree. Tenant separation has
// its own tests; these are about determinism and cache shape.
const detIdentityTenant = "11111111-1111-4111-8111-111111111111"

func detIdentity(input string) RequestIdentity {
	return RequestIdentity{
		TenantScope: detIdentityTenant,
		ModelID:     "llama-3.2-1b-instruct-q8", ModelRevision: "main@2026-07-27",
		Input: input, Temperature: 0, TopP: 1, Seed: 42, MaxTokens: 64,
	}
}

// Only a request whose two runs must agree may be replayed from cache.
func TestOnlyDeterministicRequestsAreCacheable(t *testing.T) {
	if _, err := detIdentity("hello").Compute(); err != nil {
		t.Fatalf("a greedy request should be cacheable: %v", err)
	}
	for _, r := range []RequestIdentity{
		{ModelID: "m", Input: "x", Temperature: 0.7, TopP: 1},
		{ModelID: "m", Input: "x", Temperature: 0, TopP: 0.9},
	} {
		if r.Deterministic() {
			t.Fatalf("sampling request reported deterministic: %+v", r)
		}
		if _, err := r.Compute(); !errors.Is(err, errNonDeterministic) {
			t.Fatalf("want errNonDeterministic, got %v", err)
		}
	}
}

// Every field that can change the output must change the identity, or the cache
// will serve one model's answer for another's request.
func TestEveryOutputAffectingFieldChangesIdentity(t *testing.T) {
	base := detIdentity("what is the capital of France?")
	baseID, err := base.Compute()
	must(t, err)
	if !ValidRequestIdentity(baseID) {
		t.Fatalf("malformed identity %q", baseID)
	}
	if again, _ := base.Compute(); again != baseID {
		t.Fatal("identity is not stable across calls")
	}

	mutations := map[string]func(*RequestIdentity){
		"model":          func(r *RequestIdentity) { r.ModelID = "other" },
		"revision":       func(r *RequestIdentity) { r.ModelRevision = "main@2026-01-01" },
		"profile_sha256": func(r *RequestIdentity) { r.ProfileSHA256 = "aaaa" + r.ProfileSHA256 },
		"adapter":        func(r *RequestIdentity) { r.Adapter = "lora-x" },
		"input":          func(r *RequestIdentity) { r.Input += "?" },
		"tools":          func(r *RequestIdentity) { r.Tools = `[{"name":"search"}]` },
		"schema":         func(r *RequestIdentity) { r.Schema = `{"type":"object"}` },
		"seed":           func(r *RequestIdentity) { r.Seed = 43 },
		"max_tokens":     func(r *RequestIdentity) { r.MaxTokens = 65 },
		"policy":         func(r *RequestIdentity) { r.Policy = "strict" },
	}
	for name, mutate := range mutations {
		r := base
		mutate(&r)
		got, err := r.Compute()
		mustf(t, err, "%s: %v", name)
		if got == baseID {
			t.Fatalf("changing %s did not change the request identity", name)
		}
	}
}

// Moving a value between fields must not collide.
func TestIdentityFieldsCannotBeConfused(t *testing.T) {
	a := RequestIdentity{TenantScope: detIdentityTenant, ModelID: "m", Input: "AB", Tools: "", TopP: 1}
	b := RequestIdentity{TenantScope: detIdentityTenant, ModelID: "m", Input: "A", Tools: "B", TopP: 1}
	ida, err := a.Compute()
	must(t, err)
	idb, err := b.Compute()
	must(t, err)
	if ida == idb {
		t.Fatal("field boundaries are not encoded; values can be shifted between fields")
	}
}

func TestExactResultRoundTripAndMissIsNotAGuess(t *testing.T) {
	ctx, store, pool := openPayoutTestStore(t)
	_ = pool

	id, err := detIdentity("unique-" + uuid.NewString()).Compute()
	must(t, err)

	// A miss must be a miss, never an approximate neighbour.
	if _, ok, err := store.LookupExactResult(ctx, id); err != nil || ok {
		t.Fatalf("unseen identity returned a hit: ok=%v err=%v", ok, err)
	}

	mustf(t, store.RecordExactResult(ctx, id, "cas/sha256/abc123", 64), "record: %v")
	hit, ok, err := store.LookupExactResult(ctx, id)
	if err != nil || !ok {
		t.Fatalf("stored result did not come back: ok=%v err=%v", ok, err)
	}
	if hit.ResultRef != "cas/sha256/abc123" || hit.OutputTokens != 64 {
		t.Fatalf("wrong payload: %+v", hit)
	}
	if hit.Hits != 1 {
		t.Fatalf("first hit count = %d, want 1", hit.Hits)
	}
	if second, _, _ := store.LookupExactResult(ctx, id); second.Hits != 2 {
		t.Fatalf("hit counter did not advance: %d", second.Hits)
	}

	// Malformed input must never reach SQL as a key.
	if err := store.RecordExactResult(ctx, "'; DROP TABLE jobs; --", "r", 1); err == nil {
		t.Fatal("a malformed identity must be refused")
	}
	if err := store.RecordExactResult(ctx, id, "   ", 1); err == nil {
		t.Fatal("an empty result reference must be refused")
	}
	if _, ok, _ := store.LookupExactResult(ctx, "not-an-identity"); ok {
		t.Fatal("malformed identity reported a hit")
	}
}

// Coalescing moved to inflight_coalescing_test.go when it gained a lease, a
// state machine and a production caller. The test that stood here exercised
// ClaimInflightLeader/ReleaseInflight, which are deleted: they proved that a
// second caller did not become leader, and could not prove what the second
// caller then DID, because there was nothing for it to wait on.

func TestExactCacheRefusesTenantScopedReferences(t *testing.T) {
	ctx, store, _ := openPayoutTestStore(t)

	id, err := detIdentity("tenant-" + uuid.NewString()).Compute()
	must(t, err)

	// Exactly the shape src/control/scheduler.go writes into tasks.result_key.
	jobPath := "jobs/" + uuid.NewString() + "/tasks/" + uuid.NewString() + "/attempt-0/result.json"
	if err := store.RecordExactResult(ctx, id, jobPath, 64); err == nil {
		t.Fatal("cached a buyer-scoped job path under a cross-tenant cache key")
	}
	if _, ok, _ := store.LookupExactResult(ctx, id); ok {
		t.Fatal("a refused entry was stored anyway")
	}

	// A content-addressed reference is tenant-neutral and acceptable.
	casPath := "cas/sha256/" + uuid.NewString()
	mustf(t, store.RecordExactResult(ctx, id, casPath, 64), "content-addressed reference should be cacheable: %v")
	hit, ok, err := store.LookupExactResult(ctx, id)
	if err != nil || !ok || hit.ResultRef != casPath {
		t.Fatalf("content-addressed round trip failed: %+v ok=%v err=%v", hit, ok, err)
	}
}

func TestRequestIdentityEncodingRemainsByteCompatible(t *testing.T) {
	identity := detIdentity("identity-compute-benchmark")
	identity.Tools = `[{"type":"function","function":{"name":"weather"}}]`
	identity.Schema = `{"type":"json_schema","json_schema":{"name":"answer"}}`
	got, err := identity.Compute()
	must(t, err)
	const want = "req_4ecef7ebbcb2b1dd966926804e420576a0b351b04f9e7aba805e184dfc93fc06"
	if got != want {
		t.Fatalf("request identity changed: got %q want %q", got, want)
	}
}

func TestRequestIdentityStreamingEncodingMatchesMarshalForEscapedValues(t *testing.T) {
	identity := detIdentity("quotes \" angle <tag> ampersand & line\u2028separator\u2029")
	identity.ModelID = "model/<&>"
	identity.ModelRevision = "revision\nwith\tescapes"
	identity.ProfileSHA256 = "profile"
	identity.Adapter = "adapter"
	identity.Tools = "tools"
	identity.Schema = "schema"
	identity.Policy = "policy"
	identity.TenantScope = "tenant/<&>"
	got, err := identity.Compute()
	must(t, err)

	fields := [...]struct {
		name  string
		value any
	}{
		{name: "adapter", value: identity.Adapter},
		{name: "input", value: identity.Input},
		{name: "max_tokens", value: identity.MaxTokens},
		{name: "model", value: identity.ModelID},
		{name: "policy", value: identity.Policy},
		{name: "profile_sha256", value: identity.ProfileSHA256},
		{name: "revision", value: identity.ModelRevision},
		{name: "schema", value: identity.Schema},
		{name: "seed", value: identity.Seed},
		{name: "temperature", value: identity.Temperature},
		{name: "tenant", value: identity.TenantScope},
		{name: "top_p", value: identity.TopP},
		{name: "tools", value: identity.Tools},
	}
	h := sha256.New()
	_, _ = h.Write([]byte(requestIdentityDomain + "\x00"))
	for _, field := range fields {
		_, _ = h.Write([]byte(field.name))
		_, _ = h.Write([]byte{0})
		blob, marshalErr := json.Marshal(field.value)
		mustf(t, marshalErr, "marshal %s: %v", field.name)
		_, _ = h.Write(blob)
		_, _ = h.Write([]byte{0})
	}
	want := "req_" + hex.EncodeToString(h.Sum(nil))
	if got != want {
		t.Fatalf("streaming request identity changed escaped-value bytes: got %q want %q", got, want)
	}
}

func BenchmarkRequestIdentityCompute(b *testing.B) {
	identity := detIdentity("identity-compute-benchmark")
	identity.Tools = `[{"type":"function","function":{"name":"weather"}}]`
	identity.Schema = `{"type":"json_schema","json_schema":{"name":"answer"}}`
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if _, err := identity.Compute(); err != nil {
			b.Fatal(err)
		}
	}
}
