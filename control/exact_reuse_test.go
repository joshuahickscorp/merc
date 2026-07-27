package main

import (
	"errors"
	"testing"

	"github.com/google/uuid"
)

func detIdentity(input string) RequestIdentity {
	return RequestIdentity{
		ModelID: "llama-3.2-1b-instruct-q8", ModelRevision: "main@2026-07-27",
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
	if err != nil {
		t.Fatal(err)
	}
	if !ValidRequestIdentity(baseID) {
		t.Fatalf("malformed identity %q", baseID)
	}
	if again, _ := base.Compute(); again != baseID {
		t.Fatal("identity is not stable across calls")
	}

	mutations := map[string]func(*RequestIdentity){
		"model":      func(r *RequestIdentity) { r.ModelID = "other" },
		"revision":   func(r *RequestIdentity) { r.ModelRevision = "main@2026-01-01" },
		"adapter":    func(r *RequestIdentity) { r.Adapter = "lora-x" },
		"input":      func(r *RequestIdentity) { r.Input += "?" },
		"tools":      func(r *RequestIdentity) { r.Tools = `[{"name":"search"}]` },
		"schema":     func(r *RequestIdentity) { r.Schema = `{"type":"object"}` },
		"seed":       func(r *RequestIdentity) { r.Seed = 43 },
		"max_tokens": func(r *RequestIdentity) { r.MaxTokens = 65 },
		"policy":     func(r *RequestIdentity) { r.Policy = "strict" },
	}
	for name, mutate := range mutations {
		r := base
		mutate(&r)
		got, err := r.Compute()
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if got == baseID {
			t.Fatalf("changing %s did not change the request identity", name)
		}
	}
}

// Moving a value between fields must not collide.
func TestIdentityFieldsCannotBeConfused(t *testing.T) {
	a := RequestIdentity{ModelID: "m", Input: "AB", Tools: "", TopP: 1}
	b := RequestIdentity{ModelID: "m", Input: "A", Tools: "B", TopP: 1}
	ida, err := a.Compute()
	if err != nil {
		t.Fatal(err)
	}
	idb, err := b.Compute()
	if err != nil {
		t.Fatal(err)
	}
	if ida == idb {
		t.Fatal("field boundaries are not encoded; values can be shifted between fields")
	}
}

func TestExactResultRoundTripAndMissIsNotAGuess(t *testing.T) {
	ctx, store, pool := openPayoutTestStore(t)
	_ = pool

	id, err := detIdentity("unique-" + uuid.NewString()).Compute()
	if err != nil {
		t.Fatal(err)
	}

	// A miss must be a miss, never an approximate neighbour.
	if _, ok, err := store.LookupExactResult(ctx, id); err != nil || ok {
		t.Fatalf("unseen identity returned a hit: ok=%v err=%v", ok, err)
	}

	if err := store.RecordExactResult(ctx, id, "cas/sha256/abc123", 64); err != nil {
		t.Fatalf("record: %v", err)
	}
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

// Identical deterministic requests arriving together must execute once.
func TestInflightCoalescingElectsOneLeader(t *testing.T) {
	ctx, store, pool := openPayoutTestStore(t)

	id, err := detIdentity("coalesce-" + uuid.NewString()).Compute()
	if err != nil {
		t.Fatal(err)
	}
	leader := seedPayoutFixture(t, ctx, pool, payoutFixtureOpts{creditUSD: 1.00})
	follower := seedPayoutFixture(t, ctx, pool, payoutFixtureOpts{creditUSD: 1.00})

	isLeader, err := store.ClaimInflightLeader(ctx, id, leader.jobID)
	if err != nil || !isLeader {
		t.Fatalf("first caller should lead: %v %v", isLeader, err)
	}
	isLeader2, err := store.ClaimInflightLeader(ctx, id, follower.jobID)
	if err != nil {
		t.Fatal(err)
	}
	if isLeader2 {
		t.Fatal("a second identical request also became leader; the work would run twice")
	}

	followers, err := store.ReleaseInflight(ctx, id)
	if err != nil {
		t.Fatalf("release: %v", err)
	}
	if followers != 1 {
		t.Fatalf("followers = %d, want 1", followers)
	}

	// After release the next request leads again rather than waiting forever.
	third, err := store.ClaimInflightLeader(ctx, id, leader.jobID)
	if err != nil || !third {
		t.Fatalf("after release a new request must lead: %v %v", third, err)
	}
	if _, err := store.ReleaseInflight(ctx, id); err != nil {
		t.Fatal(err)
	}

	// An uncacheable request always executes rather than silently coalescing.
	if lead, err := store.ClaimInflightLeader(ctx, "not-an-identity", leader.jobID); err != nil || !lead {
		t.Fatalf("non-deterministic work must always execute: %v %v", lead, err)
	}
}

// Tenant isolation.
//
// The cache key is request identity alone -- correct, because deterministic
// inference on identical input yields identical output for every buyer. But the
// scheduler writes results to jobs/<job_id>/..., which belongs to ONE buyer.
// Caching that path under a shared key would hand a later buyer a reference
// into someone else's job namespace.
func TestExactCacheRefusesTenantScopedReferences(t *testing.T) {
	ctx, store, _ := openPayoutTestStore(t)

	id, err := detIdentity("tenant-" + uuid.NewString()).Compute()
	if err != nil {
		t.Fatal(err)
	}

	// Exactly the shape control/scheduler.go writes into tasks.result_key.
	jobPath := "jobs/" + uuid.NewString() + "/tasks/" + uuid.NewString() + "/attempt-0/result.json"
	if err := store.RecordExactResult(ctx, id, jobPath, 64); err == nil {
		t.Fatal("cached a buyer-scoped job path under a cross-tenant cache key")
	}
	if _, ok, _ := store.LookupExactResult(ctx, id); ok {
		t.Fatal("a refused entry was stored anyway")
	}

	// A content-addressed reference is tenant-neutral and acceptable.
	casPath := "cas/sha256/" + uuid.NewString()
	if err := store.RecordExactResult(ctx, id, casPath, 64); err != nil {
		t.Fatalf("content-addressed reference should be cacheable: %v", err)
	}
	hit, ok, err := store.LookupExactResult(ctx, id)
	if err != nil || !ok || hit.ResultRef != casPath {
		t.Fatalf("content-addressed round trip failed: %+v ok=%v err=%v", hit, ok, err)
	}
}
