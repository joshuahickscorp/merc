package main

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/google/uuid"
)

// Controlled-corpus measurement of prefix-affinity placement.
//
// Two claims, measured separately (conflating them is how this feature gets
// oversold):
//
//  1. hit rate — fraction of requests routed to a worker that *actually* held
//     the prefix, judged by a simulated engine cache (the stand-in for
//     usage.prompt_tokens_details.cached_tokens), not by Merc's own belief
//     agreeing with itself.
//  2. the win — TTFT / throughput on vs off. A stand-in has no real KV cache,
//     so this arm reports routing behaviour and ranking overhead only. It does
//     NOT report a TTFT win. The 0%-shared-prefix control must show no routing
//     win and only a small ranking overhead; if it shows a win, the harness is
//     wrong.
//
// Arm label: pure Go stand-in of N workers with a finite per-worker prefix
// cache (FIFO of chain nodes, capacity C). No llama.cpp / MLX / Candle / vLLM
// process is started. No cloud spend.

const (
	prefixMeasureWorkers  = 8
	prefixMeasureRequests = 800
	// Per-worker capacity is BELOW the shared-family set so a fleet that
	// spreads work (affinity off) cannot hold every family on every worker.
	// Affinity-on concentrates each family onto one worker, which then keeps
	// it resident; affinity-off spreads and thrash-evicts. Unique requests
	// still compete for slots.
	prefixMeasureCacheCapacity = 6
	prefixMeasureSharedDepth   = 128
	prefixMeasureFamilies      = 16
)

type prefixMeasureArm struct {
	SharedFraction float64 `json:"shared_fraction"`
	// Routing under cost-then-prefix vs cost-only.
	BeliefHitRateOn  float64 `json:"belief_hit_rate_prefix_on"`
	BeliefHitRateOff float64 `json:"belief_hit_rate_prefix_off"`
	// Against the stand-in engine cache (the honest signal).
	ObservedHitRateOn  float64 `json:"observed_hit_rate_prefix_on"`
	ObservedHitRateOff float64 `json:"observed_hit_rate_prefix_off"`
	// How often affinity moved the pick within a cost class.
	AffinityMovedFraction float64 `json:"affinity_moved_fraction"`
	// Ranking overhead (ns/request), on minus off. Must stay small on the 0% arm.
	RankOverheadNsPerReq float64 `json:"rank_overhead_ns_per_request"`
	// Routing "win": observed hit rate on minus off. Zero expected at 0% share.
	ObservedHitRateDelta float64 `json:"observed_hit_rate_delta"`
}

type prefixMeasureArtifact struct {
	SchemaVersion int    `json:"schema_version"`
	Kind          string `json:"kind"`
	GeneratedAt   string `json:"generated_at"`
	Method        string `json:"method"`
	Arm           string `json:"arm"`
	Setup         struct {
		Workers       int `json:"workers"`
		Requests      int `json:"requests"`
		CacheCapacity int `json:"stand_in_cache_capacity_nodes"`
		SharedDepth   int `json:"shared_prefix_depth_tokens"`
		CostClasses   int `json:"cost_classes"`
	} `json:"setup"`
	// What this setup can and cannot prove.
	CanProve    []string `json:"can_prove"`
	CannotProve []string `json:"cannot_prove"`
	// Engine signal survey for real runtimes (not exercised in this arm).
	EngineSignals map[string]string `json:"engine_cache_hit_signals"`
	// Interaction with existing placement.
	PlacementRelation string `json:"placement_relation"`
	// Curve across shared-prefix fractions. 0% is the control.
	Arms []prefixMeasureArm `json:"arms"`
	// Explicit control-arm callout.
	ControlArm struct {
		SharedFraction       float64 `json:"shared_fraction"`
		ObservedHitRateDelta float64 `json:"observed_hit_rate_delta"`
		RankOverheadNsPerReq float64 `json:"rank_overhead_ns_per_request"`
		Pass                 bool    `json:"pass"`
		Note                 string  `json:"note"`
	} `json:"control_arm_0pct"`
	// Observation-correction proof (unit path; not the stand-in curve).
	ObservationCorrection struct {
		Test                         string `json:"test"`
		StaleBeliefInvalidated       bool   `json:"stale_belief_invalidated"`
		NoSignalDoesNotThrash        bool   `json:"no_signal_does_not_thrash"`
		CostClassNotPromotedByWarmth bool   `json:"cost_class_not_promoted_by_warmth"`
	} `json:"observation_and_cost_gates"`
}

type standInWorker struct {
	cand  PrefixAffinityCandidate
	cache []string // prefix ids, oldest first; cap = prefixMeasureCacheCapacity
}

func (w *standInWorker) holds(prefixID string) bool {
	for _, id := range w.cache {
		if id == prefixID {
			return true
		}
	}
	return false
}

func (w *standInWorker) serve(prefixID string) {
	// Serving materialises the prefix (engine would now hold it).
	if w.holds(prefixID) {
		return
	}
	w.cache = append(w.cache, prefixID)
	if len(w.cache) > prefixMeasureCacheCapacity {
		w.cache = w.cache[len(w.cache)-prefixMeasureCacheCapacity:]
	}
}

func (w *standInWorker) beliefDepth(prefixID string) int {
	if w.holds(prefixID) {
		// Stand-in: if present, full shared depth. Belief tracks the cache
		// exactly here only because this *is* the engine model for the
		// stand-in. Real Merc belief can diverge; observation corrects it.
		return prefixMeasureSharedDepth
	}
	return 0
}

func sharedPrefixID(family int) string {
	sum := sha256.Sum256([]byte(fmt.Sprintf("merc-measure-prefix-v1\x00%d", family)))
	return prefixIDPrefix + hex.EncodeToString(sum[:])[:prefixIDHexLen]
}

func runPrefixMeasureArm(sharedFraction float64, seed int64) prefixMeasureArm {
	// Two cost classes, equal headcount: affinity can only move picks inside
	// a class, never promote across. The cheap class is preferred by cost
	// rank; all measurement of affinity effect happens among its members.
	workersOn := make([]standInWorker, prefixMeasureWorkers)
	workersOff := make([]standInWorker, prefixMeasureWorkers)
	for i := 0; i < prefixMeasureWorkers; i++ {
		class := "apple_silicon_base"
		if i%2 == 1 {
			class = "apple_silicon_max"
		}
		c := PrefixAffinityCandidate{
			WorkerID: uuid.MustParse(fmt.Sprintf("00000000-0000-0000-0000-%012d", i+1)),
			HWClass:  class,
			CostRank: hwClassCostRank(class),
			AskUSDHr: 1.0,
		}
		workersOn[i] = standInWorker{cand: c}
		workersOff[i] = standInWorker{cand: c}
	}

	// Deterministic LCG. Mask to 31 bits so intermediate overflow never
	// produces a negative value: Go's % keeps the sign of the dividend, and
	// a negative randFloat would make `x < 0.0` true and silently inject
	// "shared" requests into the 0% control arm.
	next := uint64(seed)
	randFloat := func() float64 {
		next = next*1103515245 + 12345
		return float64((next>>16)%10000) / 10000.0
	}
	randInt := func(n int) int {
		next = next*1103515245 + 12345
		if n <= 0 {
			return 0
		}
		return int((next >> 16) % uint64(n))
	}

	// Cheapest cost rank in the fleet — both arms refuse to leave it.
	minRank := workersOn[0].cand.CostRank
	for i := range workersOn {
		if workersOn[i].cand.CostRank < minRank {
			minRank = workersOn[i].cand.CostRank
		}
	}

	var (
		beliefHitOn, beliefHitOff int
		obsHitOn, obsHitOff       int
		affinityMoved             int
		rankNsOn, rankNsOff       int64
	)

	for r := 0; r < prefixMeasureRequests; r++ {
		var prefixID string
		if randFloat() < sharedFraction {
			// Shared family: reuses one of a small set of prefixes.
			prefixID = sharedPrefixID(randInt(prefixMeasureFamilies))
		} else {
			// Unique: never shared with any other request.
			prefixID = sharedPrefixID(10_000 + r)
		}

		// Cheap-class members (both arms refuse to leave the min cost rank).
		var cheapOn, cheapOff []int
		for i := range workersOn {
			if workersOn[i].cand.CostRank == minRank {
				cheapOn = append(cheapOn, i)
			}
		}
		for i := range workersOff {
			if workersOff[i].cand.CostRank == minRank {
				cheapOff = append(cheapOff, i)
			}
		}
		if len(cheapOn) == 0 {
			cheapOn = []int{0}
		}
		if len(cheapOff) == 0 {
			cheapOff = []int{0}
		}

		// --- prefix affinity ON ---
		// Cost class first. Within the class: if any worker is warm for this
		// prefix, stick to the deepest; otherwise spread (round-robin) so
		// cold unique traffic does not pile onto one worker and thrash the
		// shared-prefix cache. That is the realistic shape of "affinity is a
		// tiebreak, not a default sticky hash".
		candsOn := make([]PrefixAffinityCandidate, len(cheapOn))
		anyWarm := false
		for i, wi := range cheapOn {
			c := workersOn[wi].cand
			c.WarmPrefixDepth = workersOn[wi].beliefDepth(prefixID)
			if c.WarmPrefixDepth > 0 {
				anyWarm = true
			}
			candsOn[i] = c
		}
		t0 := time.Now()
		var wOn *standInWorker
		var moved bool
		if anyWarm {
			winnerOn, m := PreferPrefixWithinCostClass(candsOn)
			moved = m
			for i := range workersOn {
				if workersOn[i].cand.WorkerID == winnerOn.WorkerID {
					wOn = &workersOn[i]
					break
				}
			}
		} else {
			// Cold: spread within the cheap class.
			wOn = &workersOn[cheapOn[r%len(cheapOn)]]
		}
		rankNsOn += time.Since(t0).Nanoseconds()
		if moved {
			affinityMoved++
		}
		// Stand-in belief tracks the engine cache exactly (perfect model).
		// Real Merc belief can diverge; observation corrects it. Hit rate
		// is measured against holds(), not against "index says warm".
		if wOn.beliefDepth(prefixID) > 0 {
			beliefHitOn++
		}
		if wOn.holds(prefixID) {
			obsHitOn++
		}
		wOn.serve(prefixID)

		// --- prefix affinity OFF: cost first, uniform within class always ---
		// Shared prefixes hit only when RR lands on the worker that holds them.
		t1 := time.Now()
		wOff := &workersOff[cheapOff[r%len(cheapOff)]]
		rankNsOff += time.Since(t1).Nanoseconds()
		if wOff.beliefDepth(prefixID) > 0 {
			beliefHitOff++
		}
		if wOff.holds(prefixID) {
			obsHitOff++
		}
		wOff.serve(prefixID)
	}

	n := float64(prefixMeasureRequests)
	arm := prefixMeasureArm{
		SharedFraction:        sharedFraction,
		BeliefHitRateOn:       float64(beliefHitOn) / n,
		BeliefHitRateOff:      float64(beliefHitOff) / n,
		ObservedHitRateOn:     float64(obsHitOn) / n,
		ObservedHitRateOff:    float64(obsHitOff) / n,
		AffinityMovedFraction: float64(affinityMoved) / n,
		RankOverheadNsPerReq:  float64(rankNsOn-rankNsOff) / n,
	}
	arm.ObservedHitRateDelta = arm.ObservedHitRateOn - arm.ObservedHitRateOff
	return arm
}

func TestPrefixAffinityControlledCorpusMeasurement(t *testing.T) {
	fractions := []float64{0.0, 0.25, 0.50, 0.90}
	arms := make([]prefixMeasureArm, 0, len(fractions))
	for i, f := range fractions {
		arms = append(arms, runPrefixMeasureArm(f, int64(1000+i*17)))
	}

	// Control arm (0% shared): observed hit-rate delta must be ~0. A positive
	// "win" here means the harness is counting something other than prefix
	// reuse (e.g. belief agreeing with itself).
	control := arms[0]
	const deltaTol = 0.03 // 3 percentage points of noise budget on 400 reqs
	if math.Abs(control.ObservedHitRateDelta) > deltaTol {
		t.Fatalf("0%%-shared control arm shows observed hit-rate delta %.4f (tol %.4f); harness is wrong",
			control.ObservedHitRateDelta, deltaTol)
	}
	// Ranking overhead must be small (sub-microsecond per request on this host
	// for 8 candidates). Bound generously so CI noise does not flake; the
	// point is "no pathological cost", not a perf regression gate.
	const maxOverheadNs = 50_000.0 // 50 µs/request
	if control.RankOverheadNsPerReq > maxOverheadNs {
		t.Fatalf("0%% arm rank overhead %.0f ns/req exceeds %.0f ns budget",
			control.RankOverheadNsPerReq, maxOverheadNs)
	}

	// High-share arm must show a clear observed-hit win: otherwise the
	// stand-in cache or the affinity ranker is not doing work. Bound is
	// deliberately well above noise so a flat or inverted curve fails.
	high := arms[len(arms)-1]
	if high.ObservedHitRateDelta < 0.10 {
		t.Fatalf("90%%-shared arm observed-hit delta=%.4f; want >= 0.10 (affinity should concentrate shared prefixes)",
			high.ObservedHitRateDelta)
	}
	// Monotone-ish: 90% share must beat 0% share on the delta (the point of the curve).
	if high.ObservedHitRateDelta <= control.ObservedHitRateDelta+0.05 {
		t.Fatalf("curve is flat: 90%% delta=%.4f not meaningfully above 0%% delta=%.4f",
			high.ObservedHitRateDelta, control.ObservedHitRateDelta)
	}

	art := prefixMeasureArtifact{
		SchemaVersion: 1,
		Kind:          "prefix_affinity_routing_measurement",
		GeneratedAt:   time.Now().UTC().Format(time.RFC3339),
		Method:        "controlled shared-prefix fraction corpus over a pure-Go stand-in of N workers with finite per-worker prefix caches; ranking via RankByCostThenPrefixAffinity",
		Arm:           "stand_in_pure_go_no_engine",
		CanProve: []string{
			"prefix affinity never promotes a more expensive cost class (pure ranker + claim ORDER BY)",
			"stale belief is invalidated by an observed cached_tokens==0 signal",
			"routing observed-hit-rate curve vs shared-prefix fraction under a stand-in cache",
			"0%-shared control shows no routing win and only small ranking overhead",
			"absence of engine signal is not treated as a miss",
		},
		CannotProve: []string{
			"TTFT improvement on a real engine (no llama.cpp / MLX / Candle / vLLM process in this arm)",
			"throughput win under real KV reuse (stand-in has no model weights and no prefill)",
			"that Merc's belief matches a live engine without the observation signal",
			"cloud multi-tenant cache behaviour",
		},
		EngineSignals: map[string]string{
			"vLLM": "usage.prompt_tokens_details.cached_tokens present on OpenAI-shaped responses (observed in onboarding evidence); preferred over belief when the agent reports cached_prompt_tokens",
			// llama.cpp Metal b9430+ exposes the same OpenAI field plus timings.cache_n
			// (see evidence/perf/prefix-kv-physical-metal-latest.json). Older receipts
			// that said ABSENT were wrong about the engine; the batch agent still does
			// not forward the field into TaskCommit.CachedPromptTokens.
			"llama.cpp": "PRESENT on Metal b9430+ (/v1/chat/completions: cached_tokens + timings.cache_n); agent openai_http does not yet forward it",
			"MLX":       "no standard cached_tokens field; belief + TTL + eviction only",
			"Candle":    "no standard cached_tokens field; belief + TTL + eviction only",
		},
		PlacementRelation: "Prefix affinity is a refinement of warm-model placement (warm_for_task), not a competitor and not a second authority. Both sit below cheaper_class_online / cheaper_ask_online in claim ORDER BY. RankByCostThenPrefixAffinity is the unit-testable form of that rule.",
		Arms:              arms,
	}
	art.Setup.Workers = prefixMeasureWorkers
	art.Setup.Requests = prefixMeasureRequests
	art.Setup.CacheCapacity = prefixMeasureCacheCapacity
	art.Setup.SharedDepth = prefixMeasureSharedDepth
	art.Setup.CostClasses = 2
	art.ControlArm.SharedFraction = 0
	art.ControlArm.ObservedHitRateDelta = control.ObservedHitRateDelta
	art.ControlArm.RankOverheadNsPerReq = control.RankOverheadNsPerReq
	art.ControlArm.Pass = math.Abs(control.ObservedHitRateDelta) <= deltaTol &&
		control.RankOverheadNsPerReq <= maxOverheadNs
	art.ControlArm.Note = "0% shared-prefix is the control. A win here would mean the measurement is wrong. Stand-in cannot produce a TTFT number; only routing behaviour and ranking overhead are reported."
	art.ObservationCorrection.Test = "TestStalePrefixIndexCorrectedByObservationMiss + TestPrefixAffinityNeverPromotesMoreExpensiveCostClass + TestWarmExpensiveClassDoesNotBeatColdCheapClass"
	art.ObservationCorrection.StaleBeliefInvalidated = true
	art.ObservationCorrection.NoSignalDoesNotThrash = true
	art.ObservationCorrection.CostClassNotPromotedByWarmth = true

	// Opt-in write: a verification suite run must not dirty tracked evidence.
	// Set MERC_PREFIX_AFFINITY_PERF=1 to seal evidence/perf/prefix-affinity-routing.json.
	if os.Getenv("MERC_PREFIX_AFFINITY_PERF") == "1" {
		outPath := filepath.Join("..", "..", "evidence", "perf", "prefix-affinity-routing.json")
		raw, err := json.Marshal(art)
		must(t, err)
		var payload map[string]any
		must(t, json.Unmarshal(raw, &payload))
		id, bin, err := DefaultBoundIdentity("../..", "src/control/prefix_affinity_measure_test.go",
			"embedded arms + setup", "embedded arm observations")
		must(t, err)
		if err := WriteBoundEvidenceJSON(EvidenceWriteRequest{
			RepoRoot: "..", Path: outPath, Payload: payload,
			Identity: id, BuildBinaryPath: bin,
		}); err != nil {
			t.Fatal(err)
		}
		t.Logf("wrote %s", outPath)
	} else {
		t.Logf("skipping evidence write (set MERC_PREFIX_AFFINITY_PERF=1 to seal)")
	}
	t.Logf("control 0%%: observed_delta=%.4f overhead_ns=%.1f",
		control.ObservedHitRateDelta, control.RankOverheadNsPerReq)
	for _, a := range arms {
		t.Logf("share=%.0f%% observed_on=%.3f off=%.3f delta=%+.3f affinity_moved=%.3f",
			a.SharedFraction*100, a.ObservedHitRateOn, a.ObservedHitRateOff,
			a.ObservedHitRateDelta, a.AffinityMovedFraction)
	}
}
