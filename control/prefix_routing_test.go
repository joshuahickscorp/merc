package main

import (
	"math"
	"strings"
	"testing"

	"github.com/google/uuid"
)

func TestPrefixIDIsStableDomainSeparatedAndShaped(t *testing.T) {
	const prompt = "You are a precise assistant. Answer in one short sentence."

	a := ComputePrefixID(prompt)
	b := ComputePrefixID(prompt)
	if a != b {
		t.Fatalf("prefix id is not stable: %s vs %s", a, b)
	}
	if !ValidPrefixID(a) {
		t.Fatalf("computed id fails its own shape check: %q", a)
	}
	if !strings.HasPrefix(a, prefixIDPrefix) || len(a) != len(prefixIDPrefix)+prefixIDHexLen {
		t.Fatalf("unexpected id shape: %q", a)
	}
	if c := ComputePrefixID(prompt + " "); c == a {
		t.Fatal("a different prefix produced the same id")
	}
	if ComputePrefixID("") != "" || ComputePrefixID("   ") != "" {
		t.Fatal("an empty prefix must not produce an id")
	}
	// Domain separation: the id must not be a bare sha256 of the prompt, or it
	// could collide with any other hash the system stores.
	bare := "pfx_" + "5e884898da28047151d0e56f8dc6292773603d0d6aabbdd6" // sha256("password") head
	if a == bare {
		t.Fatal("prefix id appears undomained")
	}
}

func TestInvalidPrefixIDsAreDroppedNotTrusted(t *testing.T) {
	for _, bad := range []string{
		"", "   ", "pfx_", "pfx_zzzz", "PFX_0123456789abcdef0123456789abcdef",
		"pfx_0123456789ABCDEF0123456789abcdef",  // uppercase hex
		"pfx_0123456789abcdef0123456789abcde",   // one short
		"pfx_0123456789abcdef0123456789abcdef0", // one long
		"'; DROP TABLE jobs; --",
	} {
		if ValidPrefixID(bad) {
			t.Fatalf("accepted a malformed prefix id: %q", bad)
		}
	}
}

// A client may send either a precomputed id or the raw prefix. A malformed id
// must fall back to hashing rather than being stored verbatim.
func TestNormalisePrefixIDPrefersValidSuppliedThenHashes(t *testing.T) {
	raw := "shared system prompt"
	want := ComputePrefixID(raw)

	if got := NormalisePrefixID(want, ""); got != want {
		t.Fatalf("valid supplied id should pass through: %q", got)
	}
	if got := NormalisePrefixID("garbage", raw); got != want {
		t.Fatalf("malformed supplied id should fall back to hashing raw prefix: %q", got)
	}
	if got := NormalisePrefixID("'; DROP TABLE jobs; --", raw); got != want {
		t.Fatalf("injection-shaped id must not be stored: %q", got)
	}
	if got := NormalisePrefixID("", ""); got != "" {
		t.Fatalf("nothing usable should yield empty, got %q", got)
	}
}

func TestPrefixWarmthIsRecordedAndScoped(t *testing.T) {
	ctx, store, pool := openPayoutTestStore(t)

	supplier := uuid.New()
	if _, err := pool.Exec(ctx,
		`INSERT INTO suppliers (id,email,reputation,status) VALUES ($1,$2,0.5,'active')
		 ON CONFLICT (id) DO NOTHING`, supplier, supplier.String()+"@pfx.invalid"); err != nil {
		t.Fatalf("seed supplier: %v", err)
	}
	warmWorker, coldWorker := uuid.New(), uuid.New()
	for _, w := range []uuid.UUID{warmWorker, coldWorker} {
		if _, err := store.CreateWorkerToken(ctx, w, supplier); err != nil {
			t.Fatalf("create worker %s: %v", w, err)
		}
	}

	// Unique per run: PrefixWarmWorkers is a global lookup by prefix id, so a
	// fixed string would collide with rows left by any earlier run in the same
	// database and make the scoping assertion non-deterministic.
	nonce := uuid.NewString()
	prefixID := ComputePrefixID("You are a precise assistant. " + nonce)
	other := ComputePrefixID("A completely different preamble. " + nonce)

	mustf(t, store.MarkPrefixWarm(ctx, warmWorker, prefixID), "MarkPrefixWarm: %v")

	warm, err := store.PrefixIsWarm(ctx, warmWorker, prefixID)
	mustf(t, err, "PrefixIsWarm: %v")
	if !warm {
		t.Fatal("worker that just served the prefix is not warm for it")
	}

	// Warmth must be per worker AND per prefix, or routing sends work to a
	// machine that has to pay the prefill anyway.
	if warm, err := store.PrefixIsWarm(ctx, coldWorker, prefixID); err != nil || warm {
		t.Fatalf("a different worker must not inherit warmth: warm=%v err=%v", warm, err)
	}
	if warm, err := store.PrefixIsWarm(ctx, warmWorker, other); err != nil || warm {
		t.Fatalf("a different prefix must not read as warm: warm=%v err=%v", warm, err)
	}

	// Repeat hits accumulate rather than duplicating rows.
	mustf(t, store.MarkPrefixWarm(ctx, warmWorker, prefixID), "second MarkPrefixWarm: %v")
	var hits int64
	if err := pool.QueryRow(ctx,
		`SELECT hits FROM worker_prefix_state WHERE worker_id=$1 AND prefix_id=$2`,
		warmWorker, prefixID).Scan(&hits); err != nil {
		t.Fatalf("read hits: %v", err)
	}
	if hits != 2 {
		t.Fatalf("hits = %d, want 2", hits)
	}

	workers, err := store.PrefixWarmWorkers(ctx, prefixID)
	mustf(t, err, "PrefixWarmWorkers: %v")
	if len(workers) != 1 || workers[0] != warmWorker {
		t.Fatalf("warm workers = %v, want [%s]", workers, warmWorker)
	}

	// A malformed id is inert, never an error and never a stored row.
	mustf(t, store.MarkPrefixWarm(ctx, warmWorker, "not-a-prefix"), "malformed id should be ignored, got %v")
	if got, err := store.PrefixWarmWorkers(ctx, "not-a-prefix"); err != nil || got != nil {
		t.Fatalf("malformed id lookup should be empty: %v %v", got, err)
	}
}

// --------------------------------------------------------------------------
// Prefix trie

func TestPrefixChainMatchesAtSharedDepthNotOnlyExact(t *testing.T) {
	// Two requests share a 100-token system prompt then diverge. Exact matching
	// reuses nothing; the chain must still match at depth 64.
	shared := make([]int, 100)
	for i := range shared {
		shared[i] = 1000 + i
	}
	a := append(append([]int{}, shared...), 7, 7, 7, 7)
	b := append(append([]int{}, shared...), 9, 9, 9, 9)

	ca, cb := ComputePrefixChain(a), ComputePrefixChain(b)
	if len(ca) == 0 || len(cb) == 0 {
		t.Fatal("chains should not be empty for a 104-token prompt")
	}

	var deepestShared int
	for _, ea := range ca {
		for _, eb := range cb {
			if ea.Depth == eb.Depth && ea.PrefixID == eb.PrefixID && ea.Depth > deepestShared {
				deepestShared = ea.Depth
			}
		}
	}
	if deepestShared != 64 {
		t.Fatalf("deepest shared depth = %d, want 64 (both share 100 tokens)", deepestShared)
	}
	// And they must NOT match at a depth beyond the divergence.
	for _, ea := range ca {
		for _, eb := range cb {
			if ea.Depth == eb.Depth && ea.Depth > 100 && ea.PrefixID == eb.PrefixID {
				t.Fatalf("matched at depth %d beyond the shared region", ea.Depth)
			}
		}
	}
}

func TestPrefixChainIsTokenIdentityNotTextSimilarity(t *testing.T) {
	base := make([]int, 64)
	for i := range base {
		base[i] = i
	}
	same := append([]int{}, base...)
	diff := append([]int{}, base...)
	diff[0] = 999 // one token different at the very start

	if ComputePrefixChain(same)[0].PrefixID != ComputePrefixChain(base)[0].PrefixID {
		t.Fatal("identical token sequences must share a prefix id")
	}
	if ComputePrefixChain(diff)[0].PrefixID == ComputePrefixChain(base)[0].PrefixID {
		t.Fatal("a single differing token must produce a different prefix id")
	}
	// Token-space ids must not collide with text-space ids.
	if ComputePrefixChain(base)[0].PrefixID == ComputePrefixID("whatever") {
		t.Fatal("token and text prefix id spaces collide")
	}
	// Short prompts yield no chain rather than a bogus shallow node.
	if got := ComputePrefixChain([]int{1, 2, 3}); len(got) != 0 {
		t.Fatalf("a 3-token prompt should produce no chain node, got %v", got)
	}
}

func TestDeepestWarmPrefixReportsReusableTokens(t *testing.T) {
	ctx, store, pool := openPayoutTestStore(t)

	supplier := uuid.New()
	if _, err := pool.Exec(ctx,
		`INSERT INTO suppliers (id,email,reputation,status) VALUES ($1,$2,0.5,'active')
		 ON CONFLICT (id) DO NOTHING`, supplier, supplier.String()+"@trie.invalid"); err != nil {
		t.Fatalf("seed supplier: %v", err)
	}
	deep, shallow, cold := uuid.New(), uuid.New(), uuid.New()
	for _, w := range []uuid.UUID{deep, shallow, cold} {
		if _, err := store.CreateWorkerToken(ctx, w, supplier); err != nil {
			t.Fatalf("worker: %v", err)
		}
	}

	// Unique per run so global prefix lookups cannot collide across runs.
	nonce := int(uuid.New().ID())
	tokens := make([]int, 300)
	for i := range tokens {
		tokens[i] = nonce + i
	}
	chain := ComputePrefixChain(tokens)
	if len(chain) < 3 {
		t.Fatalf("expected several chain depths for 300 tokens, got %d", len(chain))
	}

	f := seedPayoutFixture(t, ctx, pool, payoutFixtureOpts{creditUSD: 1.00, supplierID: supplier})
	mustf(t, store.RecordJobPrefixChain(ctx, f.jobID, chain), "record chain: %v")

	// deep holds everything; shallow only the first node.
	mustf(t, store.MarkPrefixChainWarm(ctx, deep, chain), "warm deep: %v")
	mustf(t, store.MarkPrefixChainWarm(ctx, shallow, chain[:1]), "warm shallow: %v")

	deepest, err := store.DeepestWarmPrefix(ctx, deep, f.jobID)
	mustf(t, err, "DeepestWarmPrefix(deep): %v")
	if want := chain[len(chain)-1].Depth; deepest != want {
		t.Fatalf("deep worker reusable tokens = %d, want %d", deepest, want)
	}

	shallowDepth, err := store.DeepestWarmPrefix(ctx, shallow, f.jobID)
	mustf(t, err, "DeepestWarmPrefix(shallow): %v")
	if shallowDepth != chain[0].Depth {
		t.Fatalf("shallow worker reusable tokens = %d, want %d", shallowDepth, chain[0].Depth)
	}
	if shallowDepth >= deepest {
		t.Fatal("routing cannot distinguish a deep cache from a shallow one")
	}

	coldDepth, err := store.DeepestWarmPrefix(ctx, cold, f.jobID)
	mustf(t, err, "DeepestWarmPrefix(cold): %v")
	if coldDepth != 0 {
		t.Fatalf("a worker holding nothing reports %d reusable tokens", coldDepth)
	}
}

// --------------------------------------------------------------------------
// Value-aware eviction

func TestCacheValueIsDrivenByReuseNotDepth(t *testing.T) {
	// Measured: prefill is O(n^1.01) and KV bytes are linear in depth, so depth
	// very nearly cancels. A deep node must NOT outrank a shallow one on size.
	shallowHot := PrefixCacheValue(20, 0, 32)
	deepCold := PrefixCacheValue(1, 0, 2048)
	if shallowHot <= deepCold {
		t.Fatalf("a frequently reused 32-token node (%.6g) must outrank a "+
			"rarely used 2048-token node (%.6g)", shallowHot, deepCold)
	}

	// Equal hits and age: depth should barely matter.
	a := PrefixCacheValue(5, 0, 64)
	b := PrefixCacheValue(5, 0, 1024)
	ratio := b / a
	if ratio < 0.9 || ratio > 1.2 {
		t.Fatalf("depth should nearly cancel at equal reuse, got ratio %.3f", ratio)
	}
}

func TestCacheValueDecaysWithAge(t *testing.T) {
	fresh := PrefixCacheValue(10, 0, 128)
	oneTTL := PrefixCacheValue(10, prefixWarmTTL, 128)
	twoTTL := PrefixCacheValue(10, 2*prefixWarmTTL, 128)

	if !(fresh > oneTTL && oneTTL > twoTTL) {
		t.Fatalf("value must decay with age: %.6g %.6g %.6g", fresh, oneTTL, twoTTL)
	}
	if r := oneTTL / fresh; math.Abs(r-0.5) > 1e-9 {
		t.Fatalf("one TTL should halve the value, got %.4f", r)
	}
	// Frequency alone must not beat a recently used node forever: LFU's failure.
	if PrefixCacheValue(100, 10*prefixWarmTTL, 128) >= PrefixCacheValue(1, 0, 128) {
		t.Fatal("a long-stale hot node still outranks a fresh one; decay is too weak")
	}
}

func TestCacheValueRejectsDegenerateInput(t *testing.T) {
	for _, tc := range []struct {
		hits  int64
		depth int
	}{{0, 0}, {-1, 128}, {5, 0}, {5, -1}} {
		if v := PrefixCacheValue(tc.hits, 0, tc.depth); v != 0 {
			t.Fatalf("hits=%d depth=%d should score 0, got %v", tc.hits, tc.depth, v)
		}
	}
}

func TestEvictionKeepsTheValuableNodes(t *testing.T) {
	ctx, store, pool := openPayoutTestStore(t)

	supplier := uuid.New()
	if _, err := pool.Exec(ctx,
		`INSERT INTO suppliers (id,email,reputation,status) VALUES ($1,$2,0.5,'active')
		 ON CONFLICT (id) DO NOTHING`, supplier, supplier.String()+"@evict.invalid"); err != nil {
		t.Fatalf("seed supplier: %v", err)
	}
	worker := uuid.New()
	if _, err := store.CreateWorkerToken(ctx, worker, supplier); err != nil {
		t.Fatalf("worker: %v", err)
	}

	nonce := int(uuid.New().ID())
	mk := func(seed, depth int) string {
		toks := make([]int, depth)
		for i := range toks {
			toks[i] = nonce + seed*100000 + i
		}
		return prefixIDForTokens(toks)
	}

	hot := mk(1, 128)   // reused often
	cold := mk(2, 2048) // huge, used once
	warm := mk(3, 128)  // moderate

	for id, spec := range map[string]struct {
		depth int
		hits  int64
	}{hot: {128, 40}, cold: {2048, 1}, warm: {128, 5}} {
		if _, err := pool.Exec(ctx, `
			INSERT INTO worker_prefix_state (worker_id, prefix_id, depth, hits, last_seen_warm)
			VALUES ($1,$2,$3,$4,now())`, worker, id, spec.depth, spec.hits); err != nil {
			t.Fatalf("seed node: %v", err)
		}
	}

	// Budget fits the two 128-token nodes but not the 2048-token one.
	budget := int64(3 * 128 * kvBytesPerToken)
	evicted, err := store.EvictPrefixCacheToBudget(ctx, worker, budget)
	mustf(t, err, "evict: %v")
	if evicted == 0 {
		t.Fatal("over budget but nothing was evicted")
	}

	survived := map[string]bool{}
	rows, err := pool.Query(ctx,
		`SELECT prefix_id FROM worker_prefix_state WHERE worker_id=$1`, worker)
	mustf(t, err, "read back: %v")
	defer rows.Close()
	for rows.Next() {
		var id string
		must(t, rows.Scan(&id))
		survived[id] = true
	}

	if !survived[hot] {
		t.Fatal("evicted the most-reused node; that is the one worth keeping")
	}
	if survived[cold] {
		t.Fatal("kept a 2048-token node used once while evicting cheaper reusable ones")
	}

	// Idempotent once inside budget.
	again, err := store.EvictPrefixCacheToBudget(ctx, worker, budget)
	mustf(t, err, "second evict: %v")
	if again != 0 {
		t.Fatalf("evicted %d more rows while already within budget", again)
	}
}

func TestEnforcePrefixRoutingStateBudgetsCoversEveryWorker(t *testing.T) {
	ctx, store, pool := openPayoutTestStore(t)
	for workerIndex := 0; workerIndex < 2; workerIndex++ {
		supplier := uuid.New()
		if _, err := pool.Exec(ctx, `INSERT INTO suppliers (id,email,reputation,status) VALUES ($1,$2,0.5,'active')`, supplier, supplier.String()+"@routing-budget.invalid"); err != nil {
			t.Fatal(err)
		}
		worker := uuid.New()
		if _, err := store.CreateWorkerToken(ctx, worker, supplier); err != nil {
			t.Fatal(err)
		}
		for seed := 0; seed < 3; seed++ {
			id := prefixIDForTokens([]int{workerIndex + 1, seed + 1, 7})
			if _, err := pool.Exec(ctx, `
				INSERT INTO worker_prefix_state (worker_id,prefix_id,depth,hits,last_seen_warm)
				VALUES ($1,$2,$3,1,now())`, worker, id, 2048); err != nil {
				t.Fatal(err)
			}
		}
	}
	evicted, err := store.EnforcePrefixRoutingStateBudgets(ctx)
	must(t, err)
	if evicted < 2 {
		t.Fatalf("evicted=%d, want low-value state removed for both workers", evicted)
	}
	var overBudget int
	if err := pool.QueryRow(ctx, `
		SELECT count(*) FROM (
			SELECT worker_id, COALESCE(sum(depth),0) * $1 > $2 AS over_budget
			  FROM worker_prefix_state GROUP BY worker_id
		) workers WHERE over_budget`, kvBytesPerToken, prefixRoutingStateBudgetBytes).Scan(&overBudget); err != nil {
		t.Fatal(err)
	}
	if overBudget != 0 {
		t.Fatalf("%d workers retained over-budget advisory prefix state", overBudget)
	}
}
