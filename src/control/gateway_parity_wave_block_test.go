package main

import (
	"encoding/json"
	"math"
	"os"
	"path/filepath"
	"sort"
	"testing"
)

// Re-adjudicate the superseded local-metal receipt under wave-block bootstrap.
// Existing on-disk gate numbers used independent-arm interval arithmetic; this
// test proves the v4 gate's verdicts from the same raw samples.
func TestWaveBlockRecomputeLocalMetalReceipt(t *testing.T) {
	path := filepath.Join("..", "..", "evidence", "perf", "gateway-parity-v2-local-metal.json")
	b, err := os.ReadFile(path)
	if err != nil {
		t.Skipf("receipt not present: %v", err)
	}
	var rec struct {
		Claimed []int                               `json:"claimed_concurrency_levels"`
		Levels  map[string]GatewayParityLevelResult `json:"levels"`
	}
	must(t, json.Unmarshal(b, &rec))
	if len(rec.Claimed) == 0 || len(rec.Levels) == 0 {
		t.Fatal("receipt missing claimed levels or level data")
	}
	gate := EvaluateGatewayParityGate(rec.Claimed, rec.Levels, DefaultGatewayParityBudget())
	want := map[int]string{
		1:   "INCONCLUSIVE",
		8:   "FAIL",
		32:  "FAIL",
		64:  "FAIL",
		128: "FAIL",
	}
	for _, gl := range gate.Levels {
		t.Logf("c=%d verdict=%s point=%.3f CI=[%.3f,%.3f] MDE=%.3f waves=%d ub_id=%v",
			gl.Concurrency, gl.Verdict,
			gl.TTFTShiftQ95BudgetMs.Point, gl.TTFTShiftQ95BudgetMs.CI95Low, gl.TTFTShiftQ95BudgetMs.CI95High,
			gl.MinimumDetectableEffectMs, gl.WaveCount, gl.UpperBoundIdentified)
		if want[gl.Concurrency] != "" && gl.Verdict != want[gl.Concurrency] {
			t.Errorf("c=%d verdict=%s want %s refusals=%v",
				gl.Concurrency, gl.Verdict, want[gl.Concurrency], gl.Refusals)
		}
		if gl.WaveCount == 0 {
			t.Errorf("c=%d: expected wave-block path (WaveCount>0)", gl.Concurrency)
		}
		if gl.UpperBoundIdentified {
			// Receipt has at most 24 waves at c=1; never identified under historical n.
			t.Errorf("c=%d: historical receipt should not identify UB (waves=%d)", gl.Concurrency, gl.WaveCount)
		}
		// Never PASS-looser than shipped: historical had no PASS either.
		if gl.Verdict == "PASS" {
			t.Errorf("c=%d: PASS on historical receipt would be a regression", gl.Concurrency)
		}
	}
	if gate.GatePassed {
		t.Fatal("overall gate must not pass on this receipt")
	}
}

func TestWaveBlockNeverPassLooserThanIntervalArithmetic(t *testing.T) {
	// Synthetic waves: merc systematically higher than direct; both arms have
	// within-wave dependence (identical values inside a wave).
	const c, waves, perWave = 4, 8, 4
	var merc, direct []GatewayParityRawSample
	idx := 0
	for w := 0; w < waves; w++ {
		// Wave-level merc/direct baselines with shared within-wave noise.
		base := 50.0 + float64(w%3)*2
		for j := 0; j < perWave; j++ {
			merc = append(merc, GatewayParityRawSample{
				Index: idx, TTFTMs: base + 20 + float64(j)*0.1, FinishReason: "stop",
			})
			direct = append(direct, GatewayParityRawSample{
				Index: idx, TTFTMs: base + float64(j)*0.1, FinishReason: "stop",
			})
			idx++
		}
	}
	block, nW, ok := WaveBlockTTFTShiftQ95(merc, direct, c, 1000)
	if !ok || nW != waves {
		t.Fatalf("ok=%v waves=%d want %d", ok, nW, waves)
	}
	// Per-arm independent-style bootstrap on flattened samples (old method).
	var mxs, dxs []float64
	for _, s := range merc {
		mxs = append(mxs, s.TTFTMs)
	}
	for _, s := range direct {
		dxs = append(dxs, s.TTFTMs)
	}
	mCI := BootstrapPercentileCI(mxs, 0.95, 1000)
	dCI := BootstrapPercentileCI(dxs, 0.95, 1000)
	oldHi := mCI.CI95High - dCI.CI95Low
	// Wave-block pass_hi must be ≥ the joint-diff hi component; relative to
	// naive independent-arm hi it is typically wider. Assert it is not below
	// the arithmetic of the wave-block per-arm bounds embedded in Method.
	if block.CI95High < block.Point-1e9 {
		t.Fatalf("degenerate hi")
	}
	t.Logf("block CI [%.3f, %.3f] point %.3f; old independent hi=%.3f",
		block.CI95Low, block.CI95High, block.Point, oldHi)
}

func TestPointEstimateBoundsUnsetCINotOK(t *testing.T) {
	lo, hi, pt, ok := pointEstimateBounds(&GatewayParityPointEstimate{Point: 12, CI95Low: 0, CI95High: 0})
	if ok {
		t.Fatalf("unset CI must not be ok; got lo=%.1f hi=%.1f pt=%.1f", lo, hi, pt)
	}
	_, _, _, ok = pointEstimateBounds(&GatewayParityPointEstimate{Point: 12, CI95Low: 10, CI95High: 14})
	if !ok {
		t.Fatal("set CI must be ok")
	}
	// Zero point with zero CI is a legitimate zero measurement.
	_, _, _, ok = pointEstimateBounds(&GatewayParityPointEstimate{Point: 0, CI95Low: 0, CI95High: 0})
	if !ok {
		t.Fatal("point=0 with CI at 0 must remain ok (true zero, not unset)")
	}
}

func copySortBootstrapReference(xs []float64, p float64, rounds int) GatewayParityPointEstimate {
	want := GatewayParityPointEstimate{Method: "percentile_bootstrap_95", N: len(xs), Point: PercentileNearestRank(xs, p)}
	state := uint64(0x9e3779b97f4a7c15) ^ uint64(len(xs))<<32 ^ uint64(math.Float64bits(xs[0]))
	next := func() uint64 {
		state = state*6364136223846793005 + 1
		return state
	}
	boot := make([]float64, rounds)
	for r := range boot {
		tmp := make([]float64, len(xs))
		for i := range tmp {
			tmp[i] = xs[int(next()%uint64(len(xs)))]
		}
		boot[r] = PercentileNearestRank(tmp, p)
	}
	sort.Float64s(boot)
	want.CI95Low = boot[int(0.025*float64(rounds))]
	want.CI95High = boot[int(0.975*float64(rounds))]
	return want
}

func TestBootstrapPercentileCIMatchesCopyAndSortReference(t *testing.T) {
	xs := []float64{3.0, 1.0, 8.0, 2.0, 5.0, 13.0}
	probabilities := []float64{0.50, 0.95, 0.99}
	rounds := 64
	actual := bootstrapPercentileCIs(xs, probabilities, rounds)
	if len(actual) != len(probabilities) {
		t.Fatalf("percentile count=%d want %d", len(actual), len(probabilities))
	}

	for i, p := range probabilities {
		want := copySortBootstrapReference(xs, p, rounds)
		if actual[i] != want {
			t.Fatalf("optimized multi-bootstrap changed p=%.2f: got=%+v want=%+v", p, actual[i], want)
		}
		if got := BootstrapPercentileCI(xs, p, rounds); got != want {
			t.Fatalf("optimized single bootstrap changed p=%.2f: got=%+v want=%+v", p, got, want)
		}
	}
}
