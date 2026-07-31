package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// Every repricing throughput must be a conservative bound on the receipt it cites.
//
// repricingBenchmarks drives the catalogue price AND the supplier viability
// report, and its numbers are compile-time literals carrying a SourceCitation.
// A literal beside a citation is an assumption until something compares them: the
// receipt can be regenerated on new hardware, or the literal edited, and nothing
// in the tree would notice that the price is now derived from a number no
// measurement supports.
//
// This is the check that makes the citation load-bearing. It reads the cited file
// and fails if the constant and the evidence disagree — which is what merc.md
// §19.7 means by deriving supplier viability from benchmark authority rather than
// from a stale throughput constant.
func TestEveryRepricingBenchmarkMatchesItsCitedReceipt(t *testing.T) {
	repoRoot, err := filepath.Abs("..")
	if err != nil {
		t.Fatal(err)
	}
	if len(repricingBenchmarks) == 0 {
		t.Fatal("no repricing benchmark is declared, so the catalogue has no measured basis")
	}
	for _, b := range repricingBenchmarks {
		t.Run(b.ModelID+"/"+b.JobType, func(t *testing.T) {
			path, fragment, ok := strings.Cut(b.SourceCitation, "#")
			if !ok || strings.TrimSpace(fragment) == "" {
				t.Fatalf("citation %q names no fragment, so it does not say WHICH measurement",
					b.SourceCitation)
			}
			raw, err := os.ReadFile(filepath.Join(repoRoot, path))
			if err != nil {
				t.Fatalf("cited receipt is unreadable: %v", err)
			}
			var receipt map[string]any
			if err := json.Unmarshal(raw, &receipt); err != nil {
				t.Fatalf("cited receipt is not JSON: %v", err)
			}
			if got, _ := receipt["hardware_class"].(string); got != b.HWClass {
				t.Fatalf("constant claims hardware class %q, receipt measured %q",
					b.HWClass, got)
			}
			section, ok := receipt[fragment].(map[string]any)
			if !ok {
				t.Fatalf("cited receipt has no %q section", fragment)
			}
			if model, _ := section["model"].(string); model != b.ModelID {
				t.Fatalf("constant claims model %q, receipt measured %q", b.ModelID, model)
			}
			measured, ok := measuredUnitsPerSecond(section)
			if !ok {
				t.Fatalf("receipt section %q reports no throughput this test recognises: %v",
					fragment, receiptSectionKeys(section))
			}
			// The bound must be CONSERVATIVE, not equal.
			//
			// The first version of this check demanded equality and caught the
			// batch_infer constant at 138.7 against a measured 138.71389521. That
			// direction is fine and is the direction the pricing wants: a lower
			// assumed throughput derives a higher price per unit, so the supplier
			// is covered by a figure their hardware can actually beat. What is not
			// fine is a constant ABOVE the measurement, which prices supply that
			// was never demonstrated — so the two directions get different rules.
			const maxConservativeShortfall = 0.01 // 1%
			switch {
			case b.UnitsPerSec > measured+1e-4:
				t.Fatalf("constant %.6f units/sec EXCEEDS the cited measurement %.6f; the "+
					"catalogue would price throughput no receipt demonstrates",
					b.UnitsPerSec, measured)
			case b.UnitsPerSec < measured*(1-maxConservativeShortfall):
				t.Fatalf("constant %.6f units/sec is %.2f%% below the cited measurement %.6f; "+
					"a bound that conservative overcharges the buyer without saying why",
					b.UnitsPerSec, (1-b.UnitsPerSec/measured)*100, measured)
			}
			// A price derived from a throughput measured while the device was
			// thermally throttled would understate what a sustained supplier does.
			if thermalOK, present := section["thermal_ok"].(bool); present && !thermalOK {
				t.Fatal("the cited measurement was taken while thermally throttled")
			}
		})
	}
}

// receiptSectionKeys is for the failure message only: naming what the section DOES
// report is what makes an unrecognised-throughput failure actionable.
func receiptSectionKeys(m map[string]any) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}

// measuredUnitsPerSecond reads the throughput a receipt section reports, in the
// unit the repricing constant is expressed in.
//
// The two lanes report different keys because they measure different things —
// embeddings per second and tokens per second — and batch_infer additionally
// reports both a serial and a batched figure. The BATCHED one is the constant's
// basis, because that is the shape a supplier actually serves.
func measuredUnitsPerSecond(section map[string]any) (float64, bool) {
	for _, key := range []string{
		"throughput_eps", "batch_32_tokens_per_second", "throughput_tps",
	} {
		if v, ok := section[key].(float64); ok {
			return v, true
		}
	}
	return 0, false
}

// The viability report must refuse to answer for a hardware class it has no
// measurement for, rather than answering from another class's number.
func TestSupplierViabilityOnlyReportsMeasuredHardware(t *testing.T) {
	measuredClasses := map[string]bool{}
	for _, b := range repricingBenchmarks {
		measuredClasses[b.HWClass] = true
	}
	for _, row := range SupplierViabilityReport(0.97) {
		if !measuredClasses[row.HWClass] {
			t.Fatalf("viability reported for %s, which no repricing benchmark measured",
				row.HWClass)
		}
		if row.MeasuredUnitsPerSec <= 0 {
			t.Fatalf("viability for %s/%s carries no measured throughput",
				row.ModelID, row.JobType)
		}
	}
}
