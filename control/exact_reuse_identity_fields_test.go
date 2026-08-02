package main

import (
	"errors"
	"testing"

	"github.com/google/uuid"
)

// Fail-before (unmodified code at d646da80): two payloads differing only in
// stop hashed to the same req_… identity, so request 2 would be served
// request 1's truncated completion:
//
//	--- FAIL: TestProbeStopDoesNotCollide (0.00s)
//	    stop sequences collided on identity "req_092d384d1f8570f1c9f3f66e977b88f8537a7f62fc5cd215a899909fa7c36232"
//
// Closed-set cacheability refuses any generation-affecting key not folded into
// RequestIdentity, so these requests are not cacheable at all and cannot collide.

func identityProbeBuyer() uuid.UUID {
	return uuid.MustParse(detIdentityTenant)
}

func baseCacheablePayload() map[string]any {
	return map[string]any{
		"model":       "cx-chat-1b",
		"temperature": 0.0,
		"top_p":       1.0,
		"messages":    []any{map[string]any{"role": "user", "content": "Count to 20"}},
	}
}

func clonePayload(base map[string]any) map[string]any {
	out := make(map[string]any, len(base)+1)
	for k, v := range base {
		out[k] = v
	}
	return out
}

func TestStopSequencesDoNotCollideOnIdentity(t *testing.T) {
	profile := sortedVLLMProfiles()[0]
	buyer := identityProbeBuyer()
	p1 := clonePayload(baseCacheablePayload())
	p1["stop"] = []any{"5"}
	p2 := clonePayload(baseCacheablePayload())
	p2["stop"] = []any{"20"}

	id1, err1 := realtimeIdentityFromPayload(buyer, profile, p1)
	id2, err2 := realtimeIdentityFromPayload(buyer, profile, p2)
	if err1 == nil && err2 == nil && id1 == id2 {
		t.Fatalf("stop sequences collided on identity %q", id1)
	}
	// Closed-set policy: stop is generation-affecting and not in the identity
	// field list, so both refuse rather than hash a partial key.
	if !errors.Is(err1, errNotExactCacheable) {
		t.Fatalf("stop=[5]: want errNotExactCacheable, got id=%q err=%v", id1, err1)
	}
	if !errors.Is(err2, errNotExactCacheable) {
		t.Fatalf("stop=[20]: want errNotExactCacheable, got id=%q err=%v", id2, err2)
	}
}

func TestGenerationFieldsOutsideIdentityRefuseCacheability(t *testing.T) {
	profile := sortedVLLMProfiles()[0]
	buyer := identityProbeBuyer()
	fields := map[string]any{
		"tool_choice":       "required",
		"frequency_penalty": 0.5,
		"presence_penalty":  0.2,
		"logit_bias":        map[string]any{"42": 1.0},
		"n":                 2,
	}
	for name, value := range fields {
		p1 := clonePayload(baseCacheablePayload())
		p2 := clonePayload(baseCacheablePayload())
		// Two different values of the same field must not share an identity.
		p1[name] = value
		switch name {
		case "tool_choice":
			p2[name] = "none"
		case "frequency_penalty", "presence_penalty":
			p2[name] = 0.9
		case "logit_bias":
			p2[name] = map[string]any{"99": -1.0}
		case "n":
			p2[name] = 3
		default:
			p2[name] = value
		}
		id1, err1 := realtimeIdentityFromPayload(buyer, profile, p1)
		id2, err2 := realtimeIdentityFromPayload(buyer, profile, p2)
		if err1 == nil && err2 == nil && id1 == id2 {
			t.Fatalf("%s: distinct values collided on identity %q", name, id1)
		}
		if !errors.Is(err1, errNotExactCacheable) {
			t.Fatalf("%s value A: want errNotExactCacheable, got id=%q err=%v", name, id1, err1)
		}
		if !errors.Is(err2, errNotExactCacheable) {
			t.Fatalf("%s value B: want errNotExactCacheable, got id=%q err=%v", name, id2, err2)
		}
	}
}

func TestUnknownGenerationFieldIsNotCacheable(t *testing.T) {
	profile := sortedVLLMProfiles()[0]
	buyer := identityProbeBuyer()
	p := clonePayload(baseCacheablePayload())
	// A field that does not exist on today's OpenAI surface must still refuse:
	// tomorrow's addition must fall out of the cache by default, not collide.
	p["min_p"] = 0.05
	id, err := realtimeIdentityFromPayload(buyer, profile, p)
	if !errors.Is(err, errNotExactCacheable) || id != "" {
		t.Fatalf("unknown field must refuse cacheability, got id=%q err=%v", id, err)
	}
}

func TestBareCacheablePayloadStillHashes(t *testing.T) {
	// Migration rule: no behaviour change for a request that carries none of
	// the newly covered fields (beyond the intended identity version bump and
	// ProfileSHA256 binding, which apply uniformly).
	profile := sortedVLLMProfiles()[0]
	buyer := identityProbeBuyer()
	p := baseCacheablePayload()
	// stream is non-semantic and must not refuse.
	p["stream"] = false
	p["user"] = "buyer-tag"
	id, err := realtimeIdentityFromPayload(buyer, profile, p)
	if err != nil || !ValidRequestIdentity(id) {
		t.Fatalf("closed-set allowlist rejected a bare payload: id=%q err=%v", id, err)
	}
	again, err := realtimeIdentityFromPayload(buyer, profile, p)
	if err != nil || again != id {
		t.Fatalf("identity unstable: %q then %q (%v)", id, again, err)
	}
}

func TestProfileSHA256ChangesDurableIdentity(t *testing.T) {
	profile := sortedVLLMProfiles()[0]
	buyer := identityProbeBuyer()
	p := baseCacheablePayload()
	id1, err := realtimeIdentityFromPayload(buyer, profile, p)
	if err != nil {
		t.Fatal(err)
	}
	changed := profile
	// Same model revision, different profile digest (engine/container/dtype).
	changed.ProfileSHA256 = "ffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffff"
	if changed.ProfileSHA256 == profile.ProfileSHA256 {
		t.Fatal("test setup: need a distinct profile digest")
	}
	id2, err := realtimeIdentityFromPayload(buyer, changed, p)
	if err != nil {
		t.Fatal(err)
	}
	if id1 == id2 {
		t.Fatalf("ProfileSHA256 change did not change durable identity: %q", id1)
	}
}

func TestIdentityVersionBumpMissesOldRows(t *testing.T) {
	// An entry written under a prior domain separator must never be served
	// under the current one. Changing the identity definition invalidates
	// existing rows by design; versioning the domain separator is how.
	base := detIdentity("version-bump-fixed-input")
	base.ProfileSHA256 = "deadbeef"
	current, err := base.Compute()
	if err != nil {
		t.Fatal(err)
	}
	legacy, err := base.computeRequestIdentityWithDomain("merc-request-identity-v2")
	if err != nil {
		t.Fatal(err)
	}
	if current == legacy {
		t.Fatalf("v2 and v3 domain separators collided: %q", current)
	}
	// Current Compute never produces the legacy key, so a row stored under the
	// old domain cannot be looked up by any live caller.
	again, err := base.Compute()
	if err != nil || again != current || again == legacy {
		t.Fatalf("live Compute drifted toward legacy: current=%q again=%q legacy=%q err=%v",
			current, again, legacy, err)
	}

	ctx, store, _ := openPayoutTestStore(t)
	if err := store.RecordExactResult(ctx, legacy, "cas/sha256/legacy-version-row", 8); err != nil {
		t.Fatalf("store legacy: %v", err)
	}
	// Lookup under the current identity must miss the legacy row.
	if _, ok, err := store.LookupExactResult(ctx, current); err != nil || ok {
		t.Fatalf("current identity served a legacy-version row: ok=%v err=%v", ok, err)
	}
	// The legacy key still finds its own row (shape valid); it is simply never
	// produced by Compute() any more.
	if hit, ok, err := store.LookupExactResult(ctx, legacy); err != nil || !ok || hit.OutputTokens != 8 {
		t.Fatalf("legacy row not stored under legacy key: ok=%v err=%v hit=%+v", ok, err, hit)
	}
}
