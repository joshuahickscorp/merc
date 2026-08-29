package main

import "testing"

// The two arrangements of one unstable cell, taken from real artifacts.
func TestUnstablePopulationCatchesBothRegimes(t *testing.T) {
	cases := map[string]struct {
		lat  selectorScaleLatency
		want bool
	}{
		"slow tail over fast bulk (isolated diagnostic)": {
			selectorScaleLatency{N: 40, Min: 30.1, P50: 64.6, P95: 4000, P99: 8037.9, Max: 8037.9}, true},
		"slow bulk with a fast outlier (full curve)": {
			selectorScaleLatency{N: 40, Min: 31.4, P50: 2613.5, P95: 2836.5, P99: 3260.8, Max: 3260.8}, true},
		"a genuinely stable cell must not be flagged": {
			selectorScaleLatency{N: 40, Min: 42.2, P50: 48.8, P95: 62.2, P99: 66.7, Max: 66.7}, false},
	}
	for name, c := range cases {
		got := detectSelectorScaleUnstablePopulation("batch", 1000, "concurrency_1", c.lat) != nil
		if got != c.want {
			t.Fatalf("%s: flagged=%v want=%v (p99/p50=%.1f p50/min=%.1f)",
				name, got, c.want, c.lat.P99/c.lat.P50, c.lat.P50/c.lat.Min)
		}
	}
}
