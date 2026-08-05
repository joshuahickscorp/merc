package main

import (
	"strings"
	"testing"
)

func TestPricedThroughputCitationsBind(t *testing.T) {
	mustf(t, validateAllRepricingBenchmarkCitations(), "priced throughput citations must bind: %v")
	for _, b := range repricingBenchmarks {
		if b.ModelID == "ffmpeg-transcode-v1" || b.ModelID == "svg-scene-render-v1" {
			t.Fatalf("%s must not set a catalogue price until its cited artifact binds", b.ModelID)
		}
	}
}

func TestUnpricedMediaThroughputIsRefusedUntilBound(t *testing.T) {
	if len(unpricedThroughputUntilBound) == 0 {
		t.Fatal("expected quarantined media throughput rows; empty quarantine hides the gap")
	}
	for _, b := range unpricedThroughputUntilBound {
		err := validateRepricingBenchmarkCitation(b)
		if err == nil {
			t.Fatalf("%s/%s citation unexpectedly binds; promote it to repricingBenchmarks",
				b.ModelID, b.JobType)
		}
		if !strings.Contains(err.Error(), "unbindable") {
			t.Fatalf("%s/%s refusal should name unbindable artifact, got: %v",
				b.ModelID, b.JobType, err)
		}
		t.Logf("refused as expected: %v", err)
	}
}

func TestCatalogueScheduleRefusesUnbindableCitation(t *testing.T) {
	// Inject an unbindable row into the priced set only for this test.
	saved := append([]measuredThroughput(nil), repricingBenchmarks...)
	t.Cleanup(func() { repricingBenchmarks = saved })

	repricingBenchmarks = append(repricingBenchmarks, measuredThroughput{
		ModelID:        "ffmpeg-transcode-v1",
		JobType:        "media_transcode",
		UnitsPerSec:    14423.640930216638,
		HWClass:        "apple_silicon_ultra",
		SourceCitation: "evidence/perf/runtime-benchmarks/candle-metal-ffmpeg-media-r1.json#physical_throughput",
	})
	// Also clear the quarantine list so the dual-membership check does not fire
	// first; we want the bind failure itself.
	savedUnpriced := append([]measuredThroughput(nil), unpricedThroughputUntilBound...)
	t.Cleanup(func() { unpricedThroughputUntilBound = savedUnpriced })
	unpricedThroughputUntilBound = nil

	pinBoardClockForPublication(t)
	_, err := BuildCataloguePriceSchedule()
	if err == nil {
		t.Fatal("BuildCataloguePriceSchedule accepted a price row citing an unbindable artifact")
	}
	if !strings.Contains(err.Error(), "unbindable") && !strings.Contains(err.Error(), "not a git object") {
		t.Fatalf("want unbindable/git-object refusal, got: %v", err)
	}
}

func TestEveryWattConstantDeclaresProvenance(t *testing.T) {
	must(t, validateSustainedWattsTable())
	for class, entry := range sustainedWattsByHWClass {
		if entry.Kind() != wattKindMeasured && entry.Kind() != wattKindAssumed {
			t.Fatalf("%s: kind %q is not MEASURED or ASSUMED", class, entry.Kind())
		}
		if strings.TrimSpace(entry.Provenance()) == "" {
			t.Fatalf("%s: empty provenance", class)
		}
		// CUDA on this host can only be ASSUMED.
		if strings.HasPrefix(class, "nvidia_") && entry.Kind() != wattKindAssumed {
			t.Fatalf("%s: no NVIDIA device on this host; fabricating MEASURED is refused (got %s)",
				class, entry.Kind())
		}
	}
}

func TestUnlabelledWattConstantCannotBeConstructed(t *testing.T) {
	defer func() {
		if recover() == nil {
			t.Fatal("wattsAssumed with empty provenance must panic")
		}
	}()
	_ = wattsAssumed(10, "")
}
