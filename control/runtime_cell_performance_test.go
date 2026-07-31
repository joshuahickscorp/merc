package main

import (
	"encoding/json"
	"fmt"
	"math"
	"os"
	"regexp"
	"strings"
	"testing"
	"time"
)

// benchmarkNow is an instant inside the revalidation window for every receipt in
// the tree, so "fresh" is a property of the fixture rather than of the day the
// suite happens to run.
var benchmarkNow = time.Date(2026, 8, 1, 0, 0, 0, 0, time.UTC)

func boardReferencePrice(t *testing.T, modelID, jobType string) float64 {
	t.Helper()
	board, err := loadPriceBoard()
	if err != nil {
		t.Fatalf("load price board: %v", err)
	}
	priced, ok := repriceFromMarketBoard(modelID, jobType, board)
	if !ok || priced.PricePer1K <= 0 {
		t.Fatalf("no market board price for %s/%s", modelID, jobType)
	}
	return priced.PricePer1K
}

func cellByID(t *testing.T, id string) (authorityRuntimeProfile, authorityCell) {
	t.Helper()
	for _, profile := range runtimeAuthority.Runtimes {
		for _, cell := range profile.Cells {
			if cell.ID == id {
				return profile, cell
			}
		}
	}
	t.Fatalf("no cell %q in the runtime authority document", id)
	return authorityRuntimeProfile{}, authorityCell{}
}

// The defect this lane exists for: the offered rate computed from the old
// hardcoded map came out an order of magnitude under the floor a default
// install writes, so nothing ever claimed the work.
//
// With jobTypeThroughput["embed"] = 200 this asserts $0.0126/hr against a
// $0.05/hr floor and fails.
func TestMeasuredCellClearsTheDefaultInstallPayoutFloor(t *testing.T) {
	unitsPerSec, cell, err := admissionUnitsPerSec("embed", "all-minilm-l6-v2", nil, benchmarkNow)
	if err != nil {
		t.Fatalf("admission refused the embed cell: %v", err)
	}
	if cell.Status != cellThroughputMeasured {
		t.Fatalf("embed cell resolved %s (%s), want a measured benchmark",
			cell.Status, cell.Reason)
	}
	offered := expectedSupplierUSDHr(
		unitsPerSec, boardReferencePrice(t, "all-minilm-l6-v2", "embed"),
		supplierShareRate, "batch")
	if offered <= defaultInstallMinPayoutUSDHr {
		t.Fatalf("a measured cell offers $%.5f/hr, at or below the $%.5f/hr floor a "+
			"default install writes: %.1f %s/s from %s",
			offered, defaultInstallMinPayoutUSDHr, unitsPerSec, cell.Unit,
			cell.BenchmarkAuthority)
	}
	t.Logf("cell=%s %.1f %s/s conservative (observed %.1f, best %.1f) -> $%.5f/hr",
		cell.CellID, unitsPerSec, cell.Unit, cell.ObservedUnitsPerSec,
		cell.ObservedBestUnitsPerSec, offered)
}

// The constant this file tests admission against has to be the number the
// installer actually writes, or the test proves nothing about a real install.
func TestDefaultPayoutFloorMatchesTheInstaller(t *testing.T) {
	script, err := os.ReadFile("../scripts/install.sh")
	if err != nil {
		t.Fatalf("read installer: %v", err)
	}
	match := regexp.MustCompile(`min_payout_usd_per_hr\s*=\s*([0-9.]+)`).
		FindSubmatch(script)
	if match == nil {
		t.Fatal("the installer no longer writes min_payout_usd_per_hr")
	}
	if got := string(match[1]); got != fmt.Sprintf("%g", defaultInstallMinPayoutUSDHr) {
		t.Errorf("installer writes min_payout_usd_per_hr = %s, this package tests against %g",
			got, defaultInstallMinPayoutUSDHr)
	}
}

// A cell with no receipt must be priced as unproven, not as if someone had
// measured it. The old map had no way to express the difference: every job type
// got a number and an absent one got 10.
func TestCellWithoutAUsableBenchmarkIsNotOfferedAsMeasured(t *testing.T) {
	profile, cell := cellByID(t, "mlx-metal-llama1-infer")
	got := resolveCellPerformance(profile, cell, benchmarkNow)
	if got.Status != cellThroughputUnproven {
		t.Fatalf("a cell with no benchmark authority resolved %s, want %s",
			got.Status, cellThroughputUnproven)
	}
	if got.ConservativeUnitsPerSec != unprovenFallbackUnitsPerSec {
		t.Errorf("unproven cell priced at %v units/s, want the named fallback %v",
			got.ConservativeUnitsPerSec, unprovenFallbackUnitsPerSec)
	}
	if got.ObservedUnitsPerSec != 0 || got.Reason == "" {
		t.Errorf("unproven cell reports observed=%v reason=%q; it must claim no "+
			"measurement and say why", got.ObservedUnitsPerSec, got.Reason)
	}
	// And the fallback must be too small to buy admission anywhere.
	offered := expectedSupplierUSDHr(
		got.ConservativeUnitsPerSec,
		boardReferencePrice(t, "llama-3.2-1b-instruct-q4", "batch_infer"),
		supplierShareRate, "batch")
	if offered >= defaultInstallMinPayoutUSDHr {
		t.Errorf("the unproven fallback offers $%.5f/hr, enough to clear the $%.5f/hr "+
			"default floor; an unmeasured cell must not be admissible",
			offered, defaultInstallMinPayoutUSDHr)
	}
}

// Stale must degrade, not silently pass. Both halves matter: the status has to
// say revalidation is owed, and the number has to move, or "stale" is a label
// with no consequence.
func TestStaleBenchmarkDegradesRatherThanSilentlyPassing(t *testing.T) {
	profile, cell := cellByID(t, "candle-metal-minilm-embed")
	fresh := resolveCellPerformance(profile, cell, benchmarkNow)
	stale := resolveCellPerformance(profile, cell,
		benchmarkNow.Add(benchmarkRevalidationWindow+24*time.Hour))

	if fresh.Status != cellThroughputMeasured {
		t.Fatalf("inside the window the cell resolved %s, want %s",
			fresh.Status, cellThroughputMeasured)
	}
	if stale.Status != cellThroughputStale {
		t.Fatalf("past the window the cell resolved %s, want %s",
			stale.Status, cellThroughputStale)
	}
	if stale.ConservativeUnitsPerSec >= fresh.ConservativeUnitsPerSec {
		t.Errorf("stale rate %v is not below the fresh rate %v; the degradation is cosmetic",
			stale.ConservativeUnitsPerSec, fresh.ConservativeUnitsPerSec)
	}
	if stale.Confidence >= fresh.Confidence {
		t.Errorf("stale confidence %v is not below fresh %v", stale.Confidence, fresh.Confidence)
	}
	if !strings.Contains(stale.Reason, "revalidation") {
		t.Errorf("stale reason %q does not tell anyone the benchmark must be re-taken",
			stale.Reason)
	}
	// Same observation underneath: degradation is applied to the measurement,
	// not substituted for it.
	if stale.ObservedUnitsPerSec != fresh.ObservedUnitsPerSec {
		t.Errorf("stale observed %v differs from fresh observed %v",
			stale.ObservedUnitsPerSec, fresh.ObservedUnitsPerSec)
	}
}

// "Never the best number in the sweep" is the rule; this is the check. Every
// routable cell's admissible rate must sit strictly below the receipt's best
// observation - which on a comparison receipt is a median of five repetitions
// at the best batch, not a peak.
func TestNoAdmissibleRateReachesTheBestObservation(t *testing.T) {
	checked := 0
	for _, profile := range runtimeAuthority.Runtimes {
		for _, cell := range profile.Cells {
			if !cell.Routable(profile) {
				continue
			}
			got := resolveCellPerformance(profile, cell, benchmarkNow)
			if got.Status != cellThroughputMeasured {
				continue
			}
			checked++
			if got.ConservativeUnitsPerSec >= got.ObservedBestUnitsPerSec {
				t.Errorf("cell %s is admitted at %v units/s against a best observation of %v",
					got.CellID, got.ConservativeUnitsPerSec, got.ObservedBestUnitsPerSec)
			}
			if got.ConservativeUnitsPerSec >= got.ObservedUnitsPerSec {
				t.Errorf("cell %s is admitted at %v units/s with no haircut on the "+
					"observed %v", got.CellID, got.ConservativeUnitsPerSec, got.ObservedUnitsPerSec)
			}
		}
	}
	if checked == 0 {
		t.Fatal("no routable cell resolved to a measurement; this check is vacuous")
	}
}

// A supplier that claims nothing must be able to read why. Every branch that
// can exclude a cell has to name itself.
func TestViabilityReportNamesTheReasonForIneligibility(t *testing.T) {
	price := func(modelID string) (float64, error) {
		switch modelID {
		case "all-minilm-l6-v2":
			return boardReferencePrice(t, modelID, "embed"), nil
		case "llama-3.2-1b-instruct-q4":
			return boardReferencePrice(t, modelID, "batch_infer"), nil
		}
		return 0, fmt.Errorf("no board class for %s", modelID)
	}

	rows := SupplierAdmissionViability(
		"apple_silicon_pro", defaultInstallMinPayoutUSDHr, supplierShareRate,
		"batch", benchmarkNow, price)
	if len(rows) == 0 {
		t.Fatal("the report is empty; a supplier learns nothing from it")
	}
	eligible := 0
	for _, row := range rows {
		if row.Reason == "" {
			t.Errorf("cell %s reports eligible=%v with no reason",
				row.Performance.CellID, row.Eligible)
		}
		if row.ExpectedUtilization != 1 || row.UtilizationBasis == "" {
			t.Errorf("cell %s does not state its utilization assumption",
				row.Performance.CellID)
		}
		if row.Performance.BenchmarkAuthority == "" && row.Eligible {
			t.Errorf("cell %s is eligible with no benchmark authority named",
				row.Performance.CellID)
		}
		if !row.Eligible && row.ExpectedSupplierUSDHr >= row.MinimumPayoutUSDHr &&
			!strings.Contains(row.Reason, "hardware class") &&
			!strings.Contains(row.Reason, "no usable benchmark") {
			t.Errorf("cell %s is ineligible at $%.5f/hr against a $%.5f/hr floor for "+
				"an unstated reason: %s", row.Performance.CellID,
				row.ExpectedSupplierUSDHr, row.MinimumPayoutUSDHr, row.Reason)
		}
		if row.Eligible {
			eligible++
		}
		t.Logf("%-30s eligible=%-5v $%.5f/hr vs $%.5f/hr floor  %s",
			row.Performance.CellID, row.Eligible, row.ExpectedSupplierUSDHr,
			row.MinimumPayoutUSDHr, row.Reason)
	}
	if eligible == 0 {
		t.Fatal("no cell is viable for a default install on Apple Silicon; " +
			"the marketplace has no supply side")
	}

	// A shortfall must quote both numbers, not just say no.
	shortfall := SupplierAdmissionViability(
		"apple_silicon_pro", 1000, supplierShareRate, "batch", benchmarkNow, price)
	for _, row := range shortfall {
		if row.Eligible {
			t.Fatalf("cell %s cleared a $1000/hr floor", row.Performance.CellID)
		}
		if !strings.Contains(row.Reason, "minimum payout") {
			t.Errorf("cell %s refuses a $1000/hr floor without naming the payout "+
				"comparison: %s", row.Performance.CellID, row.Reason)
		}
	}

	// Hardware the runtime does not serve is a different refusal, and must not
	// be reported as an economics problem.
	wrongHardware := SupplierAdmissionViability(
		"nvidia_80gb", 0, supplierShareRate, "batch", benchmarkNow, price)
	for _, row := range wrongHardware {
		if row.Eligible || !strings.Contains(row.Reason, "hardware class") {
			t.Errorf("cell %s on hardware it does not serve: eligible=%v reason=%q",
				row.Performance.CellID, row.Eligible, row.Reason)
		}
	}
}

// The embedded manifest is what ships in the container, and the receipts are the
// evidence. A number typed into one and not present in the other is exactly the
// ungoverned constant this lane removed, wearing a citation.
func TestManifestThroughputIsDerivableFromTheReceipts(t *testing.T) {
	for path, summary := range benchmarkAuthorityManifest {
		if len(summary.Throughput) == 0 {
			continue
		}
		raw, err := os.ReadFile("../" + path)
		if err != nil {
			t.Errorf("manifest names %s, which does not exist: %v", path, err)
			continue
		}
		var receipt struct {
			MeasuredAt         string `json:"measured_at"`
			PhysicalThroughput struct {
				SerialTokensPerSec float64 `json:"serial_tokens_per_sec"`
				PeakTokensPerSec   float64 `json:"peak_tokens_per_sec"`
				PeakBatch          int     `json:"peak_batch"`
			} `json:"physical_throughput"`
			Measurements []struct {
				Batch       int     `json:"batch"`
				MaxWallS    float64 `json:"max_wall_s"`
				TextsPerSec float64 `json:"texts_per_sec"`
				RuntimeID   string  `json:"runtime_profile_id"`
			} `json:"measurements"`
		}
		if err := json.Unmarshal(raw, &receipt); err != nil {
			t.Errorf("%s is not JSON: %v", path, err)
			continue
		}
		if summary.HWClass == "" {
			t.Errorf("%s publishes a rate but no hardware class", path)
		}
		if _, err := time.Parse(time.RFC3339, summary.MeasuredAt); err != nil {
			t.Errorf("%s publishes a rate with no parseable measurement date %q",
				path, summary.MeasuredAt)
		}
		for profileID, throughput := range summary.Throughput {
			want, best := 0.0, 0.0
			if len(receipt.Measurements) > 0 {
				// A comparison receipt: the slowest repetition at the quoted batch.
				for _, m := range receipt.Measurements {
					if m.RuntimeID != profileID || m.Batch != throughput.OperatingBatch {
						continue
					}
					want = float64(m.Batch) / m.MaxWallS
				}
				// texts_per_sec is batch/median_wall_s, so the best of them is a
				// MEDIAN and not a peak. The manifest field is named for what this
				// number is, and the basis has to say so out loud.
				for _, m := range receipt.Measurements {
					if m.RuntimeID == profileID && m.TextsPerSec > best {
						best = m.TextsPerSec
					}
				}
				if !strings.Contains(throughput.Basis, "MEDIAN") {
					t.Errorf("%s: %s publishes a median best observation without saying so: %q",
						path, profileID, throughput.Basis)
				}
			} else {
				// A single-profile sweep: the un-batched serial rate.
				want = receipt.PhysicalThroughput.SerialTokensPerSec
				best = receipt.PhysicalThroughput.PeakTokensPerSec
			}
			if want <= 0 {
				t.Errorf("%s: nothing in the receipt backs %s at batch %d",
					path, profileID, throughput.OperatingBatch)
				continue
			}
			if math.Abs(throughput.UnitsPerSecAtOperatingBatch-want) > 0.0001 {
				t.Errorf("%s: manifest says %s does %v units/s, the receipt says %v",
					path, profileID, throughput.UnitsPerSecAtOperatingBatch, want)
			}
			if math.Abs(throughput.BestObservedUnitsPerSec-best) > 0.0001 {
				t.Errorf("%s: manifest says %s tops out at %v, the receipt says %v",
					path, profileID, throughput.BestObservedUnitsPerSec, best)
			}
			if throughput.Unit == "" || throughput.Basis == "" || throughput.Precision == "" {
				t.Errorf("%s: %s publishes a rate with no unit, basis or precision",
					path, profileID)
			}
		}
	}
}

// mutableRuntimeAuthority swaps in a copy of the compiled runtime document for
// the duration of one test. The profile and cell slices are copied rather than
// aliased, or a mutation would reach through the copy into the package-level
// document and outlive the test.
func mutableRuntimeAuthority(t *testing.T) *runtimeAuthorityDocument {
	t.Helper()
	saved := runtimeAuthority
	t.Cleanup(func() { runtimeAuthority = saved })
	edited := runtimeAuthority
	edited.Runtimes = append([]authorityRuntimeProfile(nil), runtimeAuthority.Runtimes...)
	for i := range edited.Runtimes {
		edited.Runtimes[i].Cells = append([]authorityCell(nil), edited.Runtimes[i].Cells...)
	}
	runtimeAuthority = edited
	return &runtimeAuthority
}

func mutableCell(t *testing.T, doc *runtimeAuthorityDocument, cellID string) *authorityCell {
	t.Helper()
	for i := range doc.Runtimes {
		for j := range doc.Runtimes[i].Cells {
			if doc.Runtimes[i].Cells[j].ID == cellID {
				return &doc.Runtimes[i].Cells[j]
			}
		}
	}
	t.Fatalf("no cell %q in the runtime authority document", cellID)
	return nil
}

// An unproven cell resolves to unprovenFallbackUnitsPerSec, which is below every
// realistic payout floor by construction. Letting that number into the minimum
// drives the offered rate for the WHOLE (job, model) to nothing, so no supplier
// clears its floor, nothing claims, and nothing says why - the silent no-claim
// failure this file exists to remove, reintroduced one level down.
//
// schema.sql's runtime_profile_models_evidenced CHECK only forbids an ACTIVE
// cell with an EMPTY benchmark_authority. A cell whose named receipt does not
// measure it satisfies the CHECK and lands here, which is what this test builds.
func TestUnprovenRoutableCellRefusesAdmissionRatherThanCollapsingIt(t *testing.T) {
	doc := mutableRuntimeAuthority(t)
	cell := mutableCell(t, doc, "candle-metal-minilm-embed")
	cell.BenchmarkAuthority = "evidence/perf/runtime-benchmarks/candle-metal-llama1-q4-r3.json"

	rate, resolved, err := admissionUnitsPerSec("embed", "all-minilm-l6-v2", nil, benchmarkNow)
	if err == nil {
		t.Fatalf("a routable cell with no usable benchmark priced admission at %v units/s "+
			"on cell %q (%s)", rate, resolved.CellID, resolved.Status)
	}
	if !strings.Contains(err.Error(), "candle-metal-minilm-embed") {
		t.Errorf("the refusal does not name the cell an operator has to fix: %v", err)
	}

	// The premise, stated rather than assumed: had the fallback been allowed to
	// participate, this is the hourly rate the market would have been offered.
	collapsed := expectedSupplierUSDHr(unprovenFallbackUnitsPerSec,
		boardReferencePrice(t, "all-minilm-l6-v2", "embed"), supplierShareRate, "batch")
	if collapsed >= defaultInstallMinPayoutUSDHr {
		t.Fatalf("the fallback offers $%.5f/hr, which clears the $%.5f/hr default floor; "+
			"this test no longer describes a collapse",
			collapsed, defaultInstallMinPayoutUSDHr)
	}

	// And the same refusal has to reach the frozen-snapshot path, or a stored
	// decision could be verified against an authority admission itself refuses.
	if _, err := governedAdmissionUnitRates(
		"embed", "all-minilm-l6-v2", nil, benchmarkNow,
	); err == nil {
		t.Error("the governed rate set accepted a cell admission refuses")
	}
}

// A job is pinned to one runtime candidate before it is priced, but admission
// took the minimum over every routable cell serving the model. With two routable
// cells of different speed, a job pinned to the fast one is offered the slow
// one's rate and its supplier is underpaid for work it can actually do.
func TestAdmissionPricesTheCellsTheJobCanReach(t *testing.T) {
	doc := mutableRuntimeAuthority(t)
	// llama.cpp's embed cell is measured faster than candle's at every batch in
	// the same receipt, and is held back by lifecycle rather than by evidence.
	// Promoting it here gives the model two routable cells that disagree.
	for i := range doc.Runtimes {
		if doc.Runtimes[i].RuntimeID == "llama_cpp_metal" {
			doc.Runtimes[i].Lifecycle = runtimeLifecycleActive
		}
	}
	mutableCell(t, doc, "llama-cpp-metal-minilm-embed").Lifecycle = runtimeLifecycleActive

	catalogueWide, slowCell, err := admissionUnitsPerSec(
		"embed", "all-minilm-l6-v2", nil, benchmarkNow)
	if err != nil {
		t.Fatalf("catalogue-wide admission: %v", err)
	}
	pinned, fastCell, err := admissionUnitsPerSec(
		"embed", "all-minilm-l6-v2", []string{"llama-cpp-metal-minilm-embed"}, benchmarkNow)
	if err != nil {
		t.Fatalf("pinned admission: %v", err)
	}
	if fastCell.CellID != "llama-cpp-metal-minilm-embed" {
		t.Fatalf("pinning to one candidate resolved cell %q", fastCell.CellID)
	}
	if pinned <= catalogueWide {
		t.Fatalf("a job pinned to %s is priced at %v units/s, no better than the "+
			"catalogue-wide minimum %v units/s taken from %s",
			fastCell.CellID, pinned, catalogueWide, slowCell.CellID)
	}
}
