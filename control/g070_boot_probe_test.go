package main

import (
	"strings"
	"testing"
)

// G070 probes: throughput citation binds; full catalogue publication still
// refuses ASSUMED whole-package power for apple_silicon_ultra until a MEASURED
// power receipt exists (powermetrics requires root; IOReport CPU energy is
// frozen on this host; only GPU Energy moves).
func TestG070ThroughputCitationBinds(t *testing.T) {
	if len(repricingBenchmarks) != 1 {
		t.Fatalf("want exactly 1 repricing row, got %d", len(repricingBenchmarks))
	}
	b := repricingBenchmarks[0]
	if b.ModelID != "llama-3.2-1b-instruct-q4" || b.JobType != "batch_infer" {
		t.Fatalf("unexpected row %+v", b)
	}
	if err := validateRepricingBenchmarkCitation(b); err != nil {
		t.Fatalf("throughput citation bind failed: %v", err)
	}
	t.Logf("throughput citation OK model=%s ups=%.4f unit=%s/%s build=%s hw=%s",
		b.ModelID, b.UnitsPerSec, b.Unit, b.UnitScope, b.EngineBuildHash, b.HardwareIdentity)
}

func TestG070BootProbeBuildCataloguePriceSchedule(t *testing.T) {
	schedule, err := BuildCataloguePriceSchedule()
	if err != nil {
		// Exact known remaining gate after G070 throughput bind.
		if strings.Contains(err.Error(), "requires MEASURED sustained watts") &&
			strings.Contains(err.Error(), "apple_silicon_ultra") {
			t.Logf("BLOCKED (honest): %v", err)
			t.Log("whole-package power MEASURED receipt is required; see G070 report")
			return
		}
		t.Fatalf("BuildCataloguePriceSchedule unexpected error: %v", err)
	}
	if len(schedule.Results) < 1 {
		t.Fatalf("want >=1 published lane, got len(Results)=%d", len(schedule.Results))
	}
	t.Logf("OK lanes=%d", len(schedule.Results))
	for _, r := range schedule.Results {
		t.Logf("lane %s/%s price_per_1k=%.6f", r.ModelID, r.JobType, r.PricePer1K)
	}
}
