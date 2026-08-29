package main

import (
	"math"
	"strings"
	"testing"
	"time"
)

// Fuzz targets for the invariants that cost money or leak information when
// violated. Table tests check the cases I thought of; these check the ones I
// did not.

// SelectBatch must never admit more prefill than the budget allows, never
// panic, and never lose a candidate it claimed to select.
func FuzzSelectBatchNeverExceedsBudget(f *testing.F) {
	f.Add(8192, 64, 128, 0, 5)
	f.Add(4096, 1, 100000, 99999, 1)
	f.Add(1, 200, 1, 0, 3)
	f.Add(0, 10, 10, 10, 2)

	f.Fuzz(func(t *testing.T, budget, count, promptTokens, reusable, spread int) {
		// Bound the generated shape so the fuzzer explores values rather than
		// allocating gigabytes.
		if count < 0 || count > 512 {
			count %= 513
		}
		if count < 0 {
			count = -count
		}
		if promptTokens < 0 {
			promptTokens = -promptTokens
		}
		promptTokens %= 100_000
		if reusable < 0 {
			reusable = -reusable
		}
		if spread < 0 {
			spread = -spread
		}

		cands := make([]BatchCandidate, count)
		for i := range cands {
			p := promptTokens
			if spread > 0 {
				p += (i * spread) % 4096
			}
			cands[i] = BatchCandidate{
				ID:                   "c",
				PromptTokens:         p,
				ReusablePrefixTokens: reusable,
			}
		}

		chosen, used := SelectBatch(cands, budget)

		if budget <= 0 {
			if len(chosen) != 0 || used != 0 {
				t.Fatalf("non-positive budget %d admitted %d candidates", budget, len(chosen))
			}
			return
		}
		if len(chosen) > count {
			t.Fatalf("selected %d from %d candidates", len(chosen), count)
		}

		// Recompute the cost independently: the returned total must be true.
		var recomputed int
		for _, c := range chosen {
			n := c.PromptTokens - c.ReusablePrefixTokens
			if n < 0 {
				n = 0
			}
			recomputed += n
		}
		if recomputed != used {
			t.Fatalf("reported %d prefill tokens, actually %d", used, recomputed)
		}

		// Over budget is only allowed for a lone oversized candidate, which
		// must be admitted or long prompts starve.
		if used > budget && len(chosen) != 1 {
			t.Fatalf("admitted %d tokens over budget %d across %d candidates",
				used, budget, len(chosen))
		}
	})
}

// PrefixCacheValue feeds an eviction ordering. A NaN or negative score would
// make sorting incoherent and evict arbitrary nodes.
func FuzzPrefixCacheValueIsFiniteAndNonNegative(f *testing.F) {
	f.Add(int64(10), int64(90), 128)
	f.Add(int64(0), int64(0), 0)
	f.Add(int64(-5), int64(1), -1)
	f.Add(int64(math.MaxInt32), int64(1_000_000), 2048)

	f.Fuzz(func(t *testing.T, hits, ageSeconds int64, depth int) {
		if ageSeconds < 0 {
			ageSeconds = -ageSeconds
		}
		ageSeconds %= 10_000_000
		if depth > 1<<20 {
			depth %= 1 << 20
		}

		v := PrefixCacheValue(hits, time.Duration(ageSeconds)*time.Second, depth)

		if math.IsNaN(v) {
			t.Fatalf("NaN score for hits=%d age=%ds depth=%d", hits, ageSeconds, depth)
		}
		if math.IsInf(v, 0) {
			t.Fatalf("infinite score for hits=%d age=%ds depth=%d", hits, ageSeconds, depth)
		}
		if v < 0 {
			t.Fatalf("negative score %v for hits=%d age=%ds depth=%d", v, hits, ageSeconds, depth)
		}
		if hits <= 0 || depth <= 0 {
			if v != 0 {
				t.Fatalf("degenerate input scored %v, want 0", v)
			}
		}
	})
}

// A request identity must be stable, well formed, and never produced for
// non-deterministic sampling -- a collision or a leak here serves one buyer's
// answer to another.
func FuzzRequestIdentityIsWellFormedAndStable(f *testing.F) {
	f.Add("model", "rev", "input", "tools", 0.0, 1.0, int64(1), 64)
	f.Add("", "", "", "", 0.0, 1.0, int64(0), 0)
	f.Add("m", "r", "i", "t", 0.7, 1.0, int64(1), 1)
	f.Add("m", "r", "i", "t", 0.0, 0.5, int64(1), 1)

	f.Fuzz(func(t *testing.T, model, rev, input, tools string,
		temp, topP float64, seed int64, maxTokens int) {
		if math.IsNaN(temp) || math.IsNaN(topP) {
			return // JSON cannot encode NaN; not a reachable request
		}
		r := RequestIdentity{
			ModelID: model, ModelRevision: rev, Input: input, Tools: tools,
			Temperature: temp, TopP: topP, Seed: seed, MaxTokens: maxTokens,
		}

		id, err := r.Compute()
		if err != nil {
			// Refusal is fine; producing an id anyway is not.
			if id != "" {
				t.Fatalf("returned id %q alongside error %v", id, err)
			}
			return
		}

		// Non-deterministic requests must never yield an id.
		if !r.Deterministic() {
			t.Fatalf("produced identity %q for non-deterministic sampling temp=%v top_p=%v",
				id, temp, topP)
		}
		if !ValidRequestIdentity(id) {
			t.Fatalf("computed a malformed identity %q", id)
		}
		if again, err2 := r.Compute(); err2 != nil || again != id {
			t.Fatalf("identity is unstable: %q then %q (%v)", id, again, err2)
		}

		// Changing the input must change the identity.
		r2 := r
		r2.Input = input + "\x00delta"
		if other, err := r2.Compute(); err == nil && other == id {
			t.Fatalf("distinct inputs collided on %q", id)
		}
	})
}

// The billing split must never report more physical work than delivered work,
// and pricing must never exceed charging everything at the full rate.
func FuzzBillingAccountingNeverInflatesPhysical(f *testing.F) {
	f.Add(int64(100), int64(200), int64(50), int64(10), 0.000036)
	f.Add(int64(0), int64(0), int64(0), int64(0), 0.0)
	f.Add(int64(1), int64(0), int64(0), int64(0), 1.0)

	f.Fuzz(func(t *testing.T, uncached, prefixReused, output, exactReuse int64, price float64) {
		for _, v := range []*int64{&uncached, &prefixReused, &output, &exactReuse} {
			if *v < 0 {
				*v = -*v
			}
			*v %= 1 << 40
		}
		if math.IsNaN(price) || math.IsInf(price, 0) || price < 0 {
			return
		}
		price = math.Mod(price, 1000)

		acct := TokenAccounting{
			ClassUncachedInput:     uncached,
			ClassPrefixReusedInput: prefixReused,
			ClassGeneratedOutput:   output,
			ClassExactResultReuse:  exactReuse,
		}

		phys, deliv := acct.PhysicalTokens(), acct.DeliveredTokens()
		if phys > deliv {
			t.Fatalf("physical %d exceeds delivered %d", phys, deliv)
		}
		if phys != uncached+output {
			t.Fatalf("physical %d should be uncached+output = %d", phys, uncached+output)
		}

		charged := PriceAccounting(acct, price)
		if math.IsNaN(charged) || charged < 0 {
			t.Fatalf("charged %v for %+v at %v", charged, acct, price)
		}
		// Compare at ledger granularity. Both figures are subject to the same
		// micro-USD rounding, so comparing a rounded charge against an
		// unrounded ceiling would flag rounding as an overcharge. The property
		// that matters is that reuse never costs MORE than no reuse.
		naive := roundUSD(float64(deliv) / 1000.0 * price)
		if charged > naive {
			t.Fatalf("charged %.10f above the full-rate ceiling %.10f (reuse made the job dearer)",
				charged, naive)
		}
	})
}

// The exact-result cache is shared across buyers. A reference that leaks one
// tenant's namespace must be refused for EVERY input shape, not just the ones
// I thought to write down.
func FuzzExactCacheNeverStoresTenantScopedRef(f *testing.F) {
	f.Add("cas/sha256/abc")
	f.Add("jobs/x/result.json")
	f.Add("")
	f.Add("JOBS/upper")
	f.Add("./jobs/sneaky")

	f.Fuzz(func(t *testing.T, ref string) {
		refused := tenantScopedRefPattern.MatchString(ref)
		// Anything beginning with the scheduler's job namespace must be refused.
		if strings.HasPrefix(ref, "jobs/") && !refused {
			t.Fatalf("tenant-scoped reference %q was not detected", ref)
		}
		// The detector must not be so broad it refuses legitimate content
		// addresses, or the cache silently never works.
		if strings.HasPrefix(ref, "cas/") && refused {
			t.Fatalf("content-addressed reference %q was wrongly refused", ref)
		}
	})
}

// Prefix chain construction must never panic, never emit a malformed id, and
// never claim a depth deeper than the token sequence it was given.
func FuzzPrefixChainIsWellFormed(f *testing.F) {
	f.Add(0, 0)
	f.Add(1, 7)
	f.Add(3000, 42)
	f.Add(31, -1)

	f.Fuzz(func(t *testing.T, length, seed int) {
		if length < 0 {
			length = -length
		}
		length %= 5000

		toks := make([]int, length)
		for i := range toks {
			toks[i] = seed + i
		}
		chain := ComputePrefixChain(toks)

		var prev int
		for _, e := range chain {
			if !ValidPrefixID(e.PrefixID) {
				t.Fatalf("malformed prefix id %q at depth %d", e.PrefixID, e.Depth)
			}
			if e.Depth > length {
				t.Fatalf("chain claims depth %d for a %d-token prompt", e.Depth, length)
			}
			if e.Depth <= prev {
				t.Fatalf("chain depths are not strictly increasing: %d then %d", prev, e.Depth)
			}
			prev = e.Depth
		}
		// A prompt shorter than the shallowest boundary yields nothing rather
		// than a bogus node.
		if length < prefixChainDepths[0] && len(chain) != 0 {
			t.Fatalf("%d-token prompt produced %d chain nodes", length, len(chain))
		}
	})
}
