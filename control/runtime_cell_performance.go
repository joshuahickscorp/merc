package main

import (
	"fmt"
	"sort"
	"time"
)

// What a supplier can actually produce per second, derived from a receipt
// instead of asserted.
//
// This replaces jobTypeThroughput, a two-entry map of bare numbers that decided
// every supplier's offered hourly rate. It said batch_infer was 4 units/s while
// the receipt behind the price board recorded 138.7, and embed was 200 while the
// receipt recorded 1967. Nothing in the build could notice, because the map was
// its own authority: no receipt, no date, no hardware, no unit, and no way for a
// reader to tell a measurement from a placeholder.
//
// The concrete failure that caused: scripts/install.sh writes
// min_payout_usd_per_hr = 0.05, the scheduler filters on
// offered_rate_usd_hr >= min_payout_usd_hr, and the offered rate computed from
// those numbers came out an order of magnitude under the floor. A default
// install claimed no work, silently, forever, and reported no reason.
//
// The binding is per CELL, matching every other piece of runtime evidence in
// this package: llama_cpp_metal's embed cell and its generation cell have
// different receipts, different quality tiers and different lifecycles, so they
// have different throughput too.

const (
	// cellThroughputMeasured: a receipt measured this cell's profile on this
	// cell's model and published a rate at a batch the cell permits.
	cellThroughputMeasured = "MEASURED"
	// cellThroughputStale: the same, but the measurement is older than the
	// revalidation window. It degrades rather than silently passing, because a
	// number nobody has re-taken in six months is a claim about a machine, an
	// engine build and a model artifact that have all moved since.
	cellThroughputStale = "STALE_REQUIRES_REVALIDATION"
	// cellThroughputUnproven: no usable measurement. The rate is the named
	// fallback below, not a plausible-looking guess.
	cellThroughputUnproven = "UNPROVEN_CONSERVATIVE_FALLBACK"
)

const (
	// measuredThroughputHaircut turns a benchmark into something admission may
	// promise. The receipt was taken on one idle machine running one workload
	// with no other tenant; a supplier's box is thermally throttled, shares the
	// GPU with whatever else the owner is doing, and pays for model load and
	// task handoff that the benchmark harness does not.
	measuredThroughputHaircut = 0.70
	// staleThroughputHaircut is what "degraded" means in numbers. Half the fresh
	// haircut: still derived from the measurement, so a fast cell stays faster
	// than a slow one, but no longer good enough to carry a marginal cell over
	// an admission floor on the strength of a stale run.
	staleThroughputHaircut = 0.35
	// benchmarkRevalidationWindow is how long a runtime measurement is allowed
	// to stand unreviewed. Six months is one engine major and several model
	// artifact revisions in this ecosystem.
	benchmarkRevalidationWindow = 180 * 24 * time.Hour
	// unprovenFallbackUnitsPerSec is the ONLY rate in this file with no
	// measurement behind it, and it is deliberately too small to clear any
	// realistic payout floor. An unmeasured cell must fail admission and say so,
	// not be offered at a number that looks measured.
	unprovenFallbackUnitsPerSec = 1.0
)

// defaultInstallMinPayoutUSDHr is what scripts/install.sh writes into a fresh
// agent config. It is duplicated here so the admission arithmetic can be tested
// against the value a real default install actually carries;
// TestDefaultPayoutFloorMatchesTheInstaller keeps the two in step.
const defaultInstallMinPayoutUSDHr = 0.05

// RuntimeCellPerformance is the governed performance binding for one runtime
// cell: what it runs, on what, measured by whom, when, and what admission is
// therefore allowed to promise.
type RuntimeCellPerformance struct {
	CellID           string `json:"cell_id"`
	RuntimeProfileID string `json:"runtime_profile_id"`
	ProfileRevision  string `json:"profile_revision"`
	JobType          string `json:"job_type"`
	ModelID          string `json:"model_id"`
	ModelRevision    string `json:"model_revision"`
	WireKind         string `json:"wire_kind"`
	Precision        string `json:"precision"`
	QualityTier      string `json:"quality_tier"`
	Lifecycle        string `json:"lifecycle"`

	// MeasuredOnHWClass is the box the benchmark ran on, not the supplier's.
	MeasuredOnHWClass string   `json:"measured_on_hw_class"`
	HardwareClasses   []string `json:"hardware_classes"`

	OperatingBatch     int `json:"operating_batch"`
	CellMaxBatch       int `json:"cell_max_batch"`
	CellMaxConcurrency int `json:"cell_max_concurrency"`

	BenchmarkAuthority string `json:"benchmark_authority"`
	BenchmarkedAt      string `json:"benchmarked_at,omitempty"`
	BenchmarkBasis     string `json:"benchmark_basis,omitempty"`

	Unit                    string  `json:"unit"`
	ObservedUnitsPerSec     float64 `json:"observed_units_per_sec"`
	ObservedPeakUnitsPerSec float64 `json:"observed_peak_units_per_sec"`
	Haircut                 float64 `json:"haircut"`
	// ConservativeUnitsPerSec is the only rate any caller may use for money or
	// admission. It is a haircut lower bound, never the peak of a batch sweep.
	ConservativeUnitsPerSec float64 `json:"conservative_units_per_sec"`

	Status     string  `json:"status"`
	Confidence float64 `json:"confidence"`
	Reason     string  `json:"reason"`
}

// resolveCellPerformance derives the binding for one cell at one instant.
//
// It fails toward "unproven" at every step rather than toward a number: a
// receipt that names another engine, measures another model, or publishes no
// rate for this profile all land on the named fallback with a stated reason.
func resolveCellPerformance(
	profile authorityRuntimeProfile, cell authorityCell, at time.Time,
) RuntimeCellPerformance {
	out := RuntimeCellPerformance{
		CellID:                  cell.ID,
		RuntimeProfileID:        profile.RuntimeID,
		ProfileRevision:         profile.Revision,
		JobType:                 cell.Job,
		ModelID:                 cell.Model,
		QualityTier:             cell.qualityTierFor(profile),
		Lifecycle:               cell.EffectiveLifecycle(profile),
		HardwareClasses:         append([]string(nil), profile.Hardware.Platforms...),
		CellMaxBatch:            cell.MaxBatch,
		CellMaxConcurrency:      cell.MaxConcurrency,
		BenchmarkAuthority:      cell.benchmarkAuthorityFor(profile),
		ConservativeUnitsPerSec: unprovenFallbackUnitsPerSec,
		Status:                  cellThroughputUnproven,
	}
	for _, model := range runtimeAuthority.Models {
		if model.ID == cell.Model {
			out.ModelRevision = model.HFRevision
			out.WireKind = wireKindFor(cell, model.WireKind)
		}
	}

	if out.BenchmarkAuthority == "" {
		out.Reason = fmt.Sprintf("cell %q names no benchmark authority", cell.ID)
		return out
	}
	receipt, known := benchmarkAuthorityManifest[out.BenchmarkAuthority]
	switch {
	case !known:
		out.Reason = fmt.Sprintf("benchmark authority %q is not a known receipt",
			out.BenchmarkAuthority)
		return out
	case !receipt.isEvidenceFor(profile.RuntimeID):
		out.Reason = fmt.Sprintf("receipt %q is evidence for %v, not %q",
			out.BenchmarkAuthority, receipt.RuntimeProfileIDs, profile.RuntimeID)
		return out
	case len(receipt.ModelIDs) > 0 && !receipt.measures(cell.Model):
		out.Reason = fmt.Sprintf("receipt %q measures %v, not %q",
			out.BenchmarkAuthority, receipt.ModelIDs, cell.Model)
		return out
	case !receipt.ThroughputMeasured:
		out.Reason = fmt.Sprintf("receipt %q records no throughput measurement",
			out.BenchmarkAuthority)
		return out
	}

	measurement, ok := receipt.Throughput[profile.RuntimeID]
	if !ok || measurement.UnitsPerSecAtOperatingBatch <= 0 || measurement.Unit == "" {
		out.Reason = fmt.Sprintf(
			"receipt %q publishes no per-second rate for %q in a stated unit",
			out.BenchmarkAuthority, profile.RuntimeID)
		return out
	}
	// A rate taken at a batch the cell does not permit is a rate the cell cannot
	// deliver. Refusing is the point: the alternative is promising 128-wide
	// throughput from a cell capped at 8.
	if cell.MaxBatch > 0 && measurement.OperatingBatch > cell.MaxBatch {
		out.Reason = fmt.Sprintf(
			"receipt %q measured batch %d, above cell %q's declared max_batch %d",
			out.BenchmarkAuthority, measurement.OperatingBatch, cell.ID, cell.MaxBatch)
		return out
	}

	out.MeasuredOnHWClass = receipt.HWClass
	out.OperatingBatch = measurement.OperatingBatch
	out.Unit = measurement.Unit
	out.Precision = measurement.Precision
	out.BenchmarkedAt = receipt.MeasuredAt
	out.BenchmarkBasis = measurement.Basis
	out.ObservedUnitsPerSec = measurement.UnitsPerSecAtOperatingBatch
	out.ObservedPeakUnitsPerSec = measurement.PeakUnitsPerSec

	measuredAt, err := time.Parse(time.RFC3339, receipt.MeasuredAt)
	if err != nil {
		// An undated measurement can never be revalidated, so it is not treated
		// as fresh forever - it is treated as no measurement at all.
		out.Reason = fmt.Sprintf("receipt %q has no parseable measurement date",
			out.BenchmarkAuthority)
		out.ObservedUnitsPerSec, out.ObservedPeakUnitsPerSec = 0, 0
		return out
	}
	age := at.Sub(measuredAt)
	out.Status, out.Haircut, out.Confidence = cellThroughputMeasured, measuredThroughputHaircut, 0.7
	out.Reason = fmt.Sprintf(
		"measured on %s at batch %d, %s", receipt.HWClass, measurement.OperatingBatch,
		measurement.Basis)
	if age > benchmarkRevalidationWindow {
		out.Status, out.Haircut, out.Confidence = cellThroughputStale, staleThroughputHaircut, 0.3
		out.Reason = fmt.Sprintf(
			"benchmark %s is %.0f days old, past the %.0f-day revalidation window; "+
				"the rate is degraded until it is re-taken",
			receipt.MeasuredAt, age.Hours()/24, benchmarkRevalidationWindow.Hours()/24)
	}
	out.ConservativeUnitsPerSec = measurement.UnitsPerSecAtOperatingBatch * out.Haircut
	return out
}

// routableCellPerformance is every cell that may take ordinary buyer work for
// this job type and model, slowest first. Slowest first because the callers
// that pick one element pick the conservative one.
func routableCellPerformance(jobType, modelID string, at time.Time) []RuntimeCellPerformance {
	var out []RuntimeCellPerformance
	for _, profile := range runtimeAuthority.Runtimes {
		for _, cell := range profile.Cells {
			if cell.Job != jobType || cell.Model != modelID || !cell.Routable(profile) {
				continue
			}
			out = append(out, resolveCellPerformance(profile, cell, at))
		}
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].ConservativeUnitsPerSec != out[j].ConservativeUnitsPerSec {
			return out[i].ConservativeUnitsPerSec < out[j].ConservativeUnitsPerSec
		}
		return out[i].CellID < out[j].CellID
	})
	return out
}

// admissionUnitsPerSec is the rate the control plane may offer a supplier for
// this workload, and the cell that rate came from.
//
// It is the SLOWEST routable cell, because a posted job carries one offered
// rate and may land on any of them. Offering the fastest cell's rate would
// admit suppliers whose floor the cell they actually get cannot clear, which
// reintroduces the silent no-claim failure one level down.
//
// No routable cell means the fallback, not an error: refusing to price a
// workload nothing can execute would turn a routing gap into a 5xx on submit.
// The rate is small enough that admission rejects it, which is the honest
// outcome.
func admissionUnitsPerSec(jobType, modelID string, at time.Time) (float64, RuntimeCellPerformance) {
	cells := routableCellPerformance(jobType, modelID, at)
	if len(cells) == 0 {
		return unprovenFallbackUnitsPerSec, RuntimeCellPerformance{
			JobType: jobType, ModelID: modelID,
			ConservativeUnitsPerSec: unprovenFallbackUnitsPerSec,
			Status:                  cellThroughputUnproven,
			Reason: fmt.Sprintf(
				"no routable runtime cell serves job %q on model %q", jobType, modelID),
		}
	}
	return cells[0].ConservativeUnitsPerSec, cells[0]
}

// SupplierCellViability is one row of the answer to "why am I not being offered
// any work". Every field a supplier would need to argue with the number is
// present, including the ones that make the answer unflattering.
type SupplierCellViability struct {
	Performance RuntimeCellPerformance `json:"performance"`

	SupplierHWClass       string  `json:"supplier_hw_class"`
	BuyerPricePer1KUnits  float64 `json:"buyer_price_per_1k_units"`
	SupplierShare         float64 `json:"supplier_share"`
	Tier                  string  `json:"tier"`
	ExpectedUtilization   float64 `json:"expected_utilization"`
	UtilizationBasis      string  `json:"utilization_basis"`
	ExpectedSupplierUSDHr float64 `json:"expected_supplier_usd_hr"`
	MinimumPayoutUSDHr    float64 `json:"minimum_payout_usd_hr"`

	Eligible bool `json:"eligible"`
	// Reason is populated whether or not the row is eligible. An eligible row
	// still says what it is eligible ON, because "measured on hardware you do
	// not have" is something a supplier should read before buying a machine.
	Reason string `json:"reason"`
}

// utilizationBasisWhileExecuting states, rather than hides, what the admission
// comparison actually models. offered_rate_usd_hr and min_payout_usd_hr are
// both rates WHILE EXECUTING; neither side models idle time, and no fleet
// utilization measurement exists to model it with. Publishing a made-up
// utilization factor here would be exactly the defect this lane removed.
const utilizationBasisWhileExecuting = "rate while executing; queue idle time is not measured and is not modeled on either side of the comparison"

// SupplierAdmissionViability evaluates a supplier's payout floor against what
// each routable cell it can actually execute is expected to earn.
//
// referencePricePer1K is injected rather than read here so the report quotes the
// same catalogue authority admission used. A report computed off a second price
// source would eventually disagree with the gate it is explaining.
func SupplierAdmissionViability(
	hwClass string,
	minPayoutUSDHr, supplierShare float64,
	tier string,
	at time.Time,
	referencePricePer1K func(modelID string) (float64, error),
) []SupplierCellViability {
	var out []SupplierCellViability
	for _, profile := range runtimeAuthority.Runtimes {
		for _, cell := range profile.Cells {
			if !cell.Routable(profile) {
				continue
			}
			row := SupplierCellViability{
				Performance:         resolveCellPerformance(profile, cell, at),
				SupplierHWClass:     hwClass,
				SupplierShare:       supplierShare,
				Tier:                tier,
				ExpectedUtilization: 1,
				UtilizationBasis:    utilizationBasisWhileExecuting,
				MinimumPayoutUSDHr:  minPayoutUSDHr,
			}
			price, err := referencePricePer1K(cell.Model)
			if err != nil {
				row.Reason = fmt.Sprintf("model %q has no catalogue price authority: %v",
					cell.Model, err)
				out = append(out, row)
				continue
			}
			row.BuyerPricePer1KUnits = price
			row.ExpectedSupplierUSDHr = expectedSupplierUSDHr(
				row.Performance.ConservativeUnitsPerSec, price, supplierShare, tier)

			switch {
			case !hardwareClassServed(profile, hwClass):
				row.Reason = fmt.Sprintf(
					"runtime %q does not run on hardware class %q; it serves %v",
					profile.RuntimeID, hwClass, profile.Hardware.Platforms)
			case row.Performance.Status == cellThroughputUnproven:
				row.Reason = fmt.Sprintf(
					"cell %q has no usable benchmark, so it is priced at the %.0f unit/s "+
						"conservative fallback rather than as if measured: %s",
					cell.ID, unprovenFallbackUnitsPerSec, row.Performance.Reason)
			case row.ExpectedSupplierUSDHr < minPayoutUSDHr:
				row.Reason = fmt.Sprintf(
					"expected $%.5f/hr on cell %q is below your minimum payout of $%.5f/hr "+
						"(%.1f %s/s conservative x $%.8f per 1k units x %.2f supplier share)",
					row.ExpectedSupplierUSDHr, cell.ID, minPayoutUSDHr,
					row.Performance.ConservativeUnitsPerSec, row.Performance.Unit,
					price, supplierShare)
			default:
				row.Eligible = true
				row.Reason = fmt.Sprintf(
					"expected $%.5f/hr on cell %q clears your $%.5f/hr floor; %s",
					row.ExpectedSupplierUSDHr, cell.ID, minPayoutUSDHr, row.Performance.Reason)
				if row.Performance.MeasuredOnHWClass != "" &&
					row.Performance.MeasuredOnHWClass != hwClass {
					row.Reason += fmt.Sprintf(
						" (measured on %s, not your %s; your rate will differ)",
						row.Performance.MeasuredOnHWClass, hwClass)
				}
			}
			out = append(out, row)
		}
	}
	sort.Slice(out, func(i, j int) bool {
		return out[i].Performance.CellID < out[j].Performance.CellID
	})
	return out
}

// expectedSupplierUSDHr converts a conservative unit rate into the hourly gross
// a supplier is offered. It is the same arithmetic supplierAdmissionCeilingUSDHr
// performs, kept in one place so the report cannot drift from the gate.
func expectedSupplierUSDHr(unitsPerSec, referencePricePer1K, supplierShare float64, tier string) float64 {
	if !finiteNonNegative(unitsPerSec) || !finiteNonNegative(referencePricePer1K) {
		return 0
	}
	return unitsPerSec * 3600 / 1000 * referencePricePer1K * supplierShare * tierMultiplier(tier)
}

// hardwareClassServed reports whether this profile runs on the supplier's box.
// An unknown class is not assumed to be served: admission that guesses here
// offers CUDA work to a Mac.
func hardwareClassServed(profile authorityRuntimeProfile, hwClass string) bool {
	if hwClass == "" {
		return false
	}
	for _, platform := range profile.Hardware.Platforms {
		if platform == hwClass {
			return true
		}
	}
	return false
}
