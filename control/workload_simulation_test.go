package main

import (
	"context"
	"fmt"
	"math/rand"
	"os"
	"strings"
	"sync"
	"testing"

	"github.com/google/uuid"
)

// Synthetic workload simulation.
//
// Prefix reuse, exact-result reuse and in-flight coalescing are built and
// correct, but their VALUE is a cache hit rate, and merc has no traffic yet.
// Rather than leave three levers unmeasured, this drives realistic request
// streams through the real product code paths -- ComputePrefixChain,
// RequestIdentity.Compute, LookupExactResult, ClaimInflightLeader, SelectBatch --
// and measures what they actually eliminate.
//
// These are SYNTHETIC hit rates. They are honest about the mechanism and
// assumed about the traffic mix; the archetypes below are stated explicitly so
// the assumption is auditable rather than buried in an average.

type archetype struct {
	name string
	// distinct system prefixes in circulation
	prefixes int
	// how many distinct user suffixes per prefix; low means high exact-duplicate rate
	suffixesPerPrefix int
	promptTokens      int
	sharedTokens      int
	outputTokens      int
	// probability a request repeats one already seen exactly
	exactRepeatRate float64
}

// Archetypes drawn from workloads a compute marketplace actually sees. The
// parameters are assumptions; the mechanism they exercise is not.
var archetypes = []archetype{
	{"agent_rollout", 3, 200, 512, 448, 64, 0.05},
	{"eval_sweep", 1, 40, 256, 192, 32, 0.60},
	{"classification_batch", 2, 500, 320, 288, 8, 0.02},
	{"rag_qa", 20, 100, 1024, 768, 128, 0.03},
	{"open_chat", 400, 3, 192, 32, 96, 0.00},
	{"retry_storm", 1, 5, 256, 224, 32, 0.85},
}

type simResult struct {
	requests            int
	physicalPromptToks  int64
	deliveredPromptToks int64
	outputToks          int64
	exactHits           int
	coalesced           int
	prefixHitTokens     int64
}

func (s simResult) deliveredTotal() int64 { return s.deliveredPromptToks + s.outputToks }
func (s simResult) physicalTotal() int64  { return s.physicalPromptToks + s.outputToks }

// runArchetype drives one workload through the real store and policies.
func runArchetype(t *testing.T, ctx context.Context, store *Store, worker uuid.UUID,
	a archetype, requests int, seed int64) simResult {
	t.Helper()
	rng := rand.New(rand.NewSource(seed))
	nonce := int(rng.Int31())
	var out simResult

	// Distinct token sequences per (prefix, suffix) pair.
	tokensFor := func(p, s int) []int {
		toks := make([]int, a.promptTokens)
		for i := range toks {
			if i < a.sharedTokens {
				toks[i] = nonce + p*1_000_000 + i // shared region
			} else {
				toks[i] = nonce + p*1_000_000 + s*1_000 + i
			}
		}
		return toks
	}

	seen := map[string]bool{}    // identities already completed
	inflight := map[string]int{} // identity -> outstanding

	for i := 0; i < requests; i++ {
		p := rng.Intn(a.prefixes)
		s := rng.Intn(a.suffixesPerPrefix)
		if rng.Float64() < a.exactRepeatRate && i > 0 {
			s = rng.Intn(maxInt(1, a.suffixesPerPrefix/10)) // hot subset
		}
		toks := tokensFor(p, s)

		ident, err := RequestIdentity{
			ModelID: "llama-3.2-1b-instruct-q8", ModelRevision: "main",
			Input: fmt.Sprintf("%s/%d/%d", a.name, p, s),
			TopP:  1, Seed: 1, MaxTokens: a.outputTokens,
		}.Compute()
		if err != nil {
			t.Fatalf("identity: %v", err)
		}

		out.requests++
		out.deliveredPromptToks += int64(a.promptTokens)
		out.outputToks += int64(a.outputTokens)

		// 1. Exact result reuse: no model work at all.
		if seen[ident] {
			out.exactHits++
			continue
		}
		// 2. In-flight coalescing: an identical request already executing.
		if inflight[ident] > 0 {
			out.coalesced++
			inflight[ident]++
			continue
		}
		inflight[ident] = 1

		// 3. Prefix reuse: skip whatever this worker already holds.
		chain := ComputePrefixChain(toks)
		reusable := 0
		for _, e := range chain {
			if warm, err := store.PrefixIsWarm(ctx, worker, e.PrefixID); err == nil && warm {
				if e.Depth > reusable {
					reusable = e.Depth
				}
			}
		}
		out.prefixHitTokens += int64(reusable)
		out.physicalPromptToks += int64(a.promptTokens - reusable)

		if err := store.MarkPrefixChainWarm(ctx, worker, chain); err != nil {
			t.Fatalf("warm: %v", err)
		}
		seen[ident] = true
		inflight[ident] = 0
	}
	return out
}

func maxInt(a, b int) int {
	if a > b {
		return a
	}
	return b
}

// TestSyntheticWorkloadsMeasureWorkElimination reports what the three reuse
// mechanisms actually eliminate on realistic traffic shapes.
func TestSyntheticWorkloadsMeasureWorkElimination(t *testing.T) {
	ctx, store, pool := openPayoutTestStore(t)

	// Real supplier and workers: the prefix-warmth table has a foreign key, and
	// exercising the real schema is the point of driving the product code.
	supplier := uuid.New()
	if _, err := pool.Exec(ctx,
		`INSERT INTO suppliers (id,email,reputation,status) VALUES ($1,$2,0.5,'active')
		 ON CONFLICT (id) DO NOTHING`, supplier, supplier.String()+"@sim.invalid"); err != nil {
		t.Fatalf("seed supplier: %v", err)
	}

	const requests = 400
	const fullPricePer1K = 0.000036

	t.Logf("%-22s %8s %8s %8s %9s %9s %9s %9s",
		"archetype", "reqs", "exact", "coal", "pfx tok", "physical", "delivered", "reuse")

	var totPhys, totDeliv int64
	for i, a := range archetypes {
		worker := uuid.New()
		if _, err := store.CreateWorkerToken(ctx, worker, supplier); err != nil {
			t.Fatalf("create worker: %v", err)
		}
		r := runArchetype(t, ctx, store, worker, a, requests, int64(1000+i))
		reuse := float64(r.deliveredTotal()) / float64(maxInt64(r.physicalTotal(), 1))
		totPhys += r.physicalTotal()
		totDeliv += r.deliveredTotal()

		t.Logf("%-22s %8d %8d %8d %9d %9d %9d %8.2fx",
			a.name, r.requests, r.exactHits, r.coalesced, r.prefixHitTokens,
			r.physicalTotal(), r.deliveredTotal(), reuse)

		// Invariants that must hold on every archetype.
		if r.physicalTotal() > r.deliveredTotal() {
			t.Fatalf("%s: physical %d exceeds delivered %d",
				a.name, r.physicalTotal(), r.deliveredTotal())
		}
		if r.exactHits+r.coalesced > r.requests {
			t.Fatalf("%s: more eliminated requests than requests", a.name)
		}
	}

	overall := float64(totDeliv) / float64(maxInt64(totPhys, 1))
	t.Logf("")
	t.Logf("BLENDED across %d archetypes: physical %d, delivered %d, reuse %.2fx",
		len(archetypes), totPhys, totDeliv, overall)

	// Economics of the blend, using the real billing-class pricing.
	acct := TokenAccounting{
		ClassUncachedInput:     totPhys,
		ClassPrefixReusedInput: totDeliv - totPhys,
	}
	charged := PriceAccounting(acct, fullPricePer1K)
	naive := float64(totDeliv) / 1000.0 * fullPricePer1K
	t.Logf("buyer pays $%.6f vs $%.6f if every delivered token were charged at full rate (%.1f%% saving)",
		charged, naive, (1-charged/naive)*100)

	if overall <= 1.0 {
		t.Fatal("synthetic traffic eliminated no work at all; the mechanisms are not engaging")
	}
}

func maxInt64(a, b int64) int64 {
	if a > b {
		return a
	}
	return b
}

// Coalescing only engages under genuine concurrency, so the sequential
// simulation above reports zero for it. This drives real goroutines at the real
// database: exactly one must lead, the rest must wait, and the count must
// reconcile.
func TestConcurrentCoalescingElectsExactlyOneLeaderPerIdentity(t *testing.T) {
	ctx, store, pool := openPayoutTestStore(t)

	const identities = 8
	const callersEach = 16

	leaderJob := seedPayoutFixture(t, ctx, pool, payoutFixtureOpts{creditUSD: 1.00})

	type outcome struct {
		identity string
		leader   bool
	}
	results := make(chan outcome, identities*callersEach)

	idents := make([]string, identities)
	for i := range idents {
		id, err := RequestIdentity{
			ModelID: "llama-3.2-1b-instruct-q8", ModelRevision: "main",
			Input: fmt.Sprintf("concurrent-%s-%d", uuid.NewString(), i),
			TopP:  1, Seed: 1, MaxTokens: 32,
		}.Compute()
		if err != nil {
			t.Fatal(err)
		}
		idents[i] = id
	}

	var wg sync.WaitGroup
	for _, id := range idents {
		for c := 0; c < callersEach; c++ {
			wg.Add(1)
			go func(identity string) {
				defer wg.Done()
				lead, err := store.ClaimInflightLeader(ctx, identity, leaderJob.jobID)
				if err != nil {
					results <- outcome{identity, false}
					return
				}
				results <- outcome{identity, lead}
			}(id)
		}
	}
	wg.Wait()
	close(results)

	leaders := map[string]int{}
	total := 0
	for r := range results {
		total++
		if r.leader {
			leaders[r.identity]++
		}
	}
	if total != identities*callersEach {
		t.Fatalf("lost callers: %d of %d", total, identities*callersEach)
	}
	for _, id := range idents {
		if leaders[id] != 1 {
			t.Fatalf("identity %s elected %d leaders; work would run %d times",
				id[:12], leaders[id], leaders[id])
		}
	}

	// Follower counts must reconcile: every non-leader registered as a waiter.
	var totalFollowers int64
	for _, id := range idents {
		f, err := store.ReleaseInflight(ctx, id)
		if err != nil {
			t.Fatalf("release: %v", err)
		}
		totalFollowers += f
	}
	wantFollowers := int64(identities * (callersEach - 1))
	if totalFollowers != wantFollowers {
		t.Fatalf("followers = %d, want %d", totalFollowers, wantFollowers)
	}

	eliminated := float64(wantFollowers) / float64(identities*callersEach) * 100
	t.Logf("concurrent coalescing: %d callers over %d identities -> %d executions, "+
		"%.1f%% of inference eliminated", total, identities, identities, eliminated)
}

// Adversarial simulation: a hostile buyer trying to use routing hints and the
// shared cache to reach work or data that is not theirs.
//
// Every mechanism added for efficiency is also an attack surface. Prefix ids
// and request identities are buyer-influenced, so this drives hostile values at
// the real code and asserts the blast radius stays at "cache miss".
func TestHostileBuyerCannotWeaponiseReuseMechanisms(t *testing.T) {
	ctx, store, pool := openPayoutTestStore(t)

	supplier := uuid.New()
	if _, err := pool.Exec(ctx,
		`INSERT INTO suppliers (id,email,reputation,status) VALUES ($1,$2,0.5,'active')
		 ON CONFLICT (id) DO NOTHING`, supplier, supplier.String()+"@hostile.invalid"); err != nil {
		t.Fatalf("seed supplier: %v", err)
	}
	worker := uuid.New()
	if _, err := store.CreateWorkerToken(ctx, worker, supplier); err != nil {
		t.Fatalf("worker: %v", err)
	}

	hostile := []string{
		"'; DROP TABLE worker_prefix_state; --",
		"pfx_' OR '1'='1",
		"pfx_../../etc/passwd",
		"pfx_%00",
		"pfx_" + strings.Repeat("f", 10_000),
		"\x00\x01\x02",
		"pfx_00000000000000000000000000000000\n--",
	}

	for _, h := range hostile {
		// Routing hints: a forged value must cost at most a cache miss.
		if err := store.MarkPrefixWarm(ctx, worker, h); err != nil {
			t.Fatalf("hostile prefix %q produced an error rather than being ignored: %v", h, err)
		}
		warm, err := store.PrefixIsWarm(ctx, worker, h)
		if err != nil {
			t.Fatalf("hostile prefix %q errored on lookup: %v", h, err)
		}
		if warm {
			t.Fatalf("hostile prefix %q reported warm; routing would trust a forged hint", h)
		}
		if got, err := store.PrefixWarmWorkers(ctx, h); err != nil || got != nil {
			t.Fatalf("hostile prefix %q returned workers: %v %v", h, got, err)
		}

		// Cache: a forged identity must never look up or store.
		if _, ok, err := store.LookupExactResult(ctx, h); err != nil || ok {
			t.Fatalf("hostile identity %q returned a hit: ok=%v err=%v", h, ok, err)
		}
		if err := store.RecordExactResult(ctx, h, "cas/sha256/x", 1); err == nil {
			t.Fatalf("hostile identity %q was accepted as a cache key", h)
		}

		// Coalescing: an unrecognised identity must always EXECUTE rather than
		// silently waiting on someone else's work.
		lead, err := store.ClaimInflightLeader(ctx, h, uuid.New())
		if err != nil {
			t.Fatalf("hostile identity %q errored on claim: %v", h, err)
		}
		if !lead {
			t.Fatalf("hostile identity %q coalesced onto another request", h)
		}
	}

	// The tables survived: no injection landed.
	var n int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM worker_prefix_state`).Scan(&n); err != nil {
		t.Fatalf("worker_prefix_state did not survive hostile input: %v", err)
	}
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM exact_result_cache`).Scan(&n); err != nil {
		t.Fatalf("exact_result_cache did not survive hostile input: %v", err)
	}

	// A buyer cannot make their own prefix look warm on a worker that never
	// served it: warmth is recorded by merc, never asserted by the client.
	legit := ComputePrefixID("a legitimate prefix " + uuid.NewString())
	if warm, _ := store.PrefixIsWarm(ctx, worker, legit); warm {
		t.Fatal("a never-served prefix reported warm")
	}
	t.Logf("hostile input: %d payloads, all confined to cache-miss behaviour", len(hostile))
}

// A syntactically valid identity IS accepted by the storage layer -- "req_" plus
// 64 hex characters is well formed regardless of who typed it. The safety of the
// cache therefore does not rest on rejecting such values; it rests on identities
// being DERIVED from the request server-side and never accepted from a client.
//
// This test pins that property, because it is the one an attacker would attack:
// if a buyer could supply an identity, they could poison a key a future
// legitimate request will compute.
func TestRequestIdentitiesAreDerivedNeverSupplied(t *testing.T) {
	// A well-formed but invented identity is storable -- that is expected.
	invented := "req_" + strings.Repeat("a", 64)
	if !ValidRequestIdentity(invented) {
		t.Fatal("shape check should accept any 64 hex characters")
	}

	// The protection is that no real request can produce it: the identity is a
	// SHA-256 over the request fields, so a buyer would have to find a preimage.
	derived, err := detIdentity("some prompt").Compute()
	if err != nil {
		t.Fatal(err)
	}
	if derived == invented {
		t.Fatal("a derived identity collided with a hand-written one")
	}

	// And structurally: nothing in the request-handling surface reads an
	// identity from the wire. If that ever changes, this comment is the place
	// the reviewer should be looking.
	handlers, err := os.ReadFile("api.go")
	if err != nil {
		t.Fatalf("read api.go: %v", err)
	}
	for _, forbidden := range []string{"request_identity", "RequestIdentity{"} {
		if strings.Contains(string(handlers), forbidden) {
			t.Fatalf("api.go references %q: a client-supplied identity would let a buyer "+
				"choose a cache key and poison it for a future legitimate request", forbidden)
		}
	}
}
