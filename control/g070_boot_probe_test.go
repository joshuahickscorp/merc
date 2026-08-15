package main

import (
	"testing"
)

// G070 probes: throughput citation binds; catalogue publication uses the
// conservative Apple wall-power VENDOR_WALL_UPPER_BOUND (270 W) as the
// ECONOMIC_POWER_ENVELOPE so the llama lane can boot. Local GPU-only
// telemetry remains refused and does not satisfy ENERGY_MEASUREMENT.
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
		t.Fatalf("BuildCataloguePriceSchedule unexpected error: %v", err)
	}
	if len(schedule.Results) < 1 {
		t.Fatalf("want >=1 published lane, got len(Results)=%d", len(schedule.Results))
	}
	t.Logf("OK lanes=%d", len(schedule.Results))
	for _, r := range schedule.Results {
		t.Logf("lane %s/%s price_per_1k=%.6f source_class=%s watts=%.0f",
			r.ModelID, r.JobType, r.PricePer1K,
			r.PhysicalAuthority.Power.SourceClass, r.PhysicalAuthority.Power.Watts)
		if r.PhysicalAuthority.Power.SourceClass != string(wattKindVendorWallUpperBound) {
			t.Fatalf("production llama power source_class=%q, want VENDOR_WALL_UPPER_BOUND",
				r.PhysicalAuthority.Power.SourceClass)
		}
		if r.PhysicalAuthority.Power.Watts != appleMacStudio2025M3UltraWallMaxWatts {
			t.Fatalf("production llama watts=%.0f, want 270", r.PhysicalAuthority.Power.Watts)
		}
		if r.PhysicalAuthority.Power.SourceClass == string(wattKindMeasured) {
			t.Fatal("VENDOR_WALL_UPPER_BOUND must never be stored as MEASURED")
		}
	}
}
