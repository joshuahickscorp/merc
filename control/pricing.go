package main

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"math"
	"os"
	"reflect"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/jackc/pgx/v5"
)

type measuredThroughput struct {
	ModelID                   string
	ModelArtifactDigest       string // exact sha256 of the model artifact measured by the cited receipt
	JobType                   string
	RuntimeCellID             string
	RuntimeProfileID          string
	ProfileRevision           string
	Engine                    string
	EngineRevision            string
	EngineBuildHash           string
	EngineBuildIdentityPolicy string
	HardwareIdentity          string
	Unit                      string  // exact physical unit named by the cited receipt
	UnitScope                 string  // exact semantic denominator named by the cited receipt
	UnitsPerSec               float64 // observed/near-observed Unit/UnitScope rate from the cited receipt
	HWClass                   string  // apple_silicon_pro (the only measured reference box; see GPU_CAPABILITY.md)
	SourceCitation            string
}

// repricingBenchmarks is the closed set of throughput rows considered for a
// buyer-facing catalogue price. Every entry must pass
// validateRepricingBenchmarkCitation at schedule construction: the citation
// must resolve, carry binding_status=BOUND, and contain complete producer
// identity with a real source commit. A row that cites weaker evidence remains
// useful to diagnostics below, but is refused rather than published.
//
// evidence/benchmarks/2026-07-01-m3-pro.json is binding_status=UNBOUND (missing
// all producer-identity fields). Resolvable is not the same as BOUND: the
// receipt ships for diagnostic comparison, but cannot mint a live schedule.
// Consequently the checked-in production authority currently has zero
// publishable catalogue lanes and BuildCataloguePriceSchedule fails closed.
//
// Media rows (ffmpeg-transcode-v1, svg-scene-render-v1) were removed from this
// set because their cited receipts cannot bind: the ffmpeg receipt names
// merc_source_commit "working-tree-before-media-authority" (not a git object),
// and the rendering receipt has no source commit at all. Cell routability was
// already quarantined for the same reason (fdc8eec1); catalogue pricing now
// refuses them until a bindable re-measure exists. See
// unpricedThroughputUntilBound.
var repricingBenchmarks = []measuredThroughput{
	{
		ModelID:             "all-minilm-l6-v2",
		ModelArtifactDigest: "53aa51172d142c89d9012cce15ae4d6cc0ca6895895114379cacb4fab128d9db",
		JobType:             "embed",
		RuntimeCellID:       "candle-metal-minilm-embed",
		RuntimeProfileID:    "candle_metal",
		ProfileRevision:     "r9",
		Engine:              "candle",
		EngineBuildHash:     "408db133af3c3014",
		HardwareIdentity:    "Apple M3 Pro",
		Unit:                "embeddings",
		UnitScope:           performanceUnitScopeCompletedEmbeddingRecords,
		UnitsPerSec:         1967.3141,
		HWClass:             "apple_silicon_pro",
		// diagnostic only: UNBOUND receipt, so publication refuses this row
		SourceCitation: "evidence/benchmarks/2026-07-01-m3-pro.json#embed",
	},
	{
		ModelID:             "llama-3.2-1b-instruct-q4",
		ModelArtifactDigest: "3f5a22426976ab26cfe84dba63c1d08391717abb1af893e10f1b2968d862dcc1",
		JobType:             "batch_infer",
		RuntimeCellID:       "candle-metal-llama1-infer",
		RuntimeProfileID:    "candle_metal",
		ProfileRevision:     "r9",
		Engine:              "candle",
		EngineBuildHash:     "408db133af3c3014",
		HardwareIdentity:    "Apple M3 Pro",
		Unit:                "tokens",
		UnitScope:           performanceUnitScopeDecodeOutputTokens,
		UnitsPerSec:         138.7,
		HWClass:             "apple_silicon_pro",
		// diagnostic only: UNBOUND receipt, so publication refuses this row
		SourceCitation: "evidence/benchmarks/2026-07-01-m3-pro.json#batch_infer",
	},
}

// unpricedThroughputUntilBound holds measured (model, job type) pairs that still
// have throughput receipts in-tree but must not set a buyer price until the
// cited artifact binds. They are deliberately not in repricingBenchmarks so
// BuildCataloguePriceSchedule cannot publish them. Prefer a local re-measure
// with a real merc_source_commit over inventing identity; until then refusal
// is the honest state, not a soft skip.
//
// Both citations below are unbound and unbindable for pricing (ffmpeg names a
// non-git merc_source_commit; rendering has no source commit). They are retained
// only as the measured numbers that must not become catalogue prices.
var unpricedThroughputUntilBound = []measuredThroughput{
	{
		ModelID:          "ffmpeg-transcode-v1",
		JobType:          "media_transcode",
		RuntimeCellID:    "candle-metal-ffmpeg-transcode",
		RuntimeProfileID: "candle_metal",
		ProfileRevision:  "r9",
		Engine:           "candle",
		Unit:             "media_work_units",
		UnitScope:        performanceUnitScopeSingleObjectInputByteQuarters,
		UnitsPerSec:      14423.640930216638,
		HWClass:          "apple_silicon_ultra",
		// unbound and unbindable for pricing; retained measurement only
		SourceCitation: "evidence/perf/runtime-benchmarks/candle-metal-ffmpeg-media-r1.json#physical_throughput",
	},
	{
		ModelID:          "svg-scene-render-v1",
		JobType:          "media_rendering",
		RuntimeCellID:    "candle-metal-scene-render",
		RuntimeProfileID: "candle_metal",
		ProfileRevision:  "r9",
		Engine:           "candle",
		Unit:             "pixels",
		UnitScope:        performanceUnitScopeDeclaredOutputPixelsPerScene,
		UnitsPerSec:      148271490.0,
		HWClass:          "apple_silicon_ultra",
		// unbound and unbindable for pricing; retained measurement only
		SourceCitation: "evidence/perf/runtime-benchmarks/candle-metal-rendering-r1.json#physical_throughput",
	},
}

// wattAuthorityKind is whether a sustained-power figure is MEASURED from a
// receipt or ASSUMED by a named author with a stated reason. An assumption
// honestly labelled is acceptable; an assumption presented as physics is not.
type wattAuthorityKind string

const (
	wattKindMeasured wattAuthorityKind = "MEASURED"
	wattKindAssumed  wattAuthorityKind = "ASSUMED"
)

// governedSustainedWatts is one hardware class's sustained whole-package draw
// under inference-shaped load. Fields are unexported so a bare float cannot be
// inserted into the table: construction goes through wattsMeasured / wattsAssumed,
// both of which require non-empty provenance. Startup rejects any entry that
// somehow lacks kind or provenance.
type governedSustainedWatts struct {
	watts         float64
	kind          wattAuthorityKind
	provenance    string
	receiptSHA256 string
}

// Watts is the sustained draw in watts used by contribution margins and the
// diagnostic cost floor.
func (g governedSustainedWatts) Watts() float64 { return g.watts }

// Kind is MEASURED or ASSUMED.
func (g governedSustainedWatts) Kind() wattAuthorityKind { return g.kind }

// Provenance names the receipt (MEASURED) or who assumed the figure and why
// (ASSUMED). Required for every entry.
func (g governedSustainedWatts) Provenance() string { return g.provenance }

// ReceiptSHA256 pins the exact cited receipt bytes for weight-bearing
// publication. ASSUMED diagnostic rows intentionally carry no digest.
func (g governedSustainedWatts) ReceiptSHA256() string { return g.receiptSHA256 }

// wattsMeasured constructs a MEASURED sustained-power figure. provenance must
// name the receipt that measured it. Panics on empty inputs so an unlabelled
// constant cannot be added at the call site.
func wattsMeasured(watts float64, provenance, receiptSHA256 string) governedSustainedWatts {
	provenance = strings.TrimSpace(provenance)
	if watts <= 0 || provenance == "" {
		panic("wattsMeasured requires positive watts and non-empty provenance naming the receipt")
	}
	return governedSustainedWatts{
		watts:         watts,
		kind:          wattKindMeasured,
		provenance:    provenance,
		receiptSHA256: strings.TrimSpace(receiptSHA256),
	}
}

// wattsAssumed constructs an ASSUMED sustained-power figure. provenance must
// name who assumed it and why. Panics on empty inputs so an unlabelled constant
// cannot be added at the call site. CUDA classes on hosts without NVIDIA
// hardware must use this path — fabricating MEASURED would be the failure the
// directive is trying to prevent.
func wattsAssumed(watts float64, provenance string) governedSustainedWatts {
	provenance = strings.TrimSpace(provenance)
	if watts <= 0 || provenance == "" {
		panic("wattsAssumed requires positive watts and non-empty provenance naming who assumed it and why")
	}
	return governedSustainedWatts{watts: watts, kind: wattKindAssumed, provenance: provenance}
}

// sustainedWattsByHWClass is the closed power table for contribution margins
// and supplier-viability arithmetic. Every admitted hardware class must appear
// here with MEASURED or ASSUMED provenance; validateSustainedWattsTable panics
// at init if any entry is incomplete or any admitted class is missing.
//
// None of these values are currently MEASURED against a bound receipt. They
// remain ASSUMED until a host-side power receipt exists. In particular
// apple_silicon_ultra's 65 W whole-package assumption understates GPU-alone
// prefill-shaped draw observed on this host (~131.69 W); that observation is
// not yet a bound MEASURED receipt, so the constant stays ASSUMED and the
// understatement is named in provenance rather than silently "corrected".
// CUDA figures are board-power order-of-magnitude assumptions only — there is
// no NVIDIA device on this host to measure.
var sustainedWattsByHWClass = map[string]governedSustainedWatts{
	"apple_silicon_base": wattsAssumed(20.0,
		"ASSUMED by control/pricing.go: whole-package sustained draw for apple_silicon_base under inference-shaped load; no bound host power receipt"),
	"apple_silicon_pro": wattsAssumed(30.0,
		"ASSUMED by control/pricing.go: whole-package sustained draw for apple_silicon_pro under inference-shaped load; no bound host power receipt"),
	"apple_silicon_max": wattsAssumed(45.0,
		"ASSUMED by control/pricing.go: whole-package sustained draw for apple_silicon_max under inference-shaped load; no bound host power receipt"),
	"apple_silicon_ultra": wattsAssumed(65.0,
		"ASSUMED by control/pricing.go: whole-package sustained draw for apple_silicon_ultra under inference-shaped load. "+
			"GPU-alone prefill-shaped work on this host was observed near 131.69 W (more than twice this constant) "+
			"but is not a bound MEASURED receipt; the constant remains ASSUMED and known understated until remeasured"),
	// CUDA: board-power order of magnitude under inference. Not measured here.
	"nvidia_24gb": wattsAssumed(350.0,
		"ASSUMED by control/pricing.go: ~350 W board-power class for 24GB CUDA cards under inference; no NVIDIA device on this host to measure"),
	"nvidia_48gb": wattsAssumed(300.0,
		"ASSUMED by control/pricing.go: ~300 W board-power class for 48GB CUDA cards under inference; no NVIDIA device on this host to measure"),
	"nvidia_80gb": wattsAssumed(400.0,
		"ASSUMED by control/pricing.go: ~400 W board-power class for 80GB CUDA cards under inference; no NVIDIA device on this host to measure"),
	"nvidia_180gb": wattsAssumed(1000.0,
		"ASSUMED by control/pricing.go: ~1000 W board-power class for 180GB multi-GPU / H-class nodes under inference; no NVIDIA device on this host to measure"),
	"cpu": wattsAssumed(25.0,
		"ASSUMED by control/pricing.go: ~25 W sustained for a CPU-only worker class; not an admitted registration class and not measured"),
}

// validateSustainedWattsTable is the startup gate that makes an unlabelled watt
// constant impossible: every map entry must carry positive watts, a known kind,
// and non-empty provenance, and every admitted hardware class must have an
// entry. Called from init.
func validateSustainedWattsTable() error {
	for class, entry := range sustainedWattsByHWClass {
		if entry.Watts() <= 0 {
			return fmt.Errorf("sustainedWattsByHWClass[%q]: watts must be positive, got %v", class, entry.Watts())
		}
		switch entry.Kind() {
		case wattKindMeasured, wattKindAssumed:
		default:
			return fmt.Errorf("sustainedWattsByHWClass[%q]: Kind must be MEASURED or ASSUMED, got %q",
				class, entry.Kind())
		}
		if strings.TrimSpace(entry.Provenance()) == "" {
			return fmt.Errorf("sustainedWattsByHWClass[%q]: Provenance is required (uncited watt constants are not production truth)",
				class)
		}
	}
	for class := range validHWClasses {
		if _, ok := sustainedWattsByHWClass[class]; !ok {
			return fmt.Errorf("admitted hardware class %q has no sustainedWattsByHWClass entry", class)
		}
	}
	return nil
}

// sustainedWattsForClass returns the sustained draw for a hardware class, or a
// conservative apple_silicon_pro-equivalent default when the class is unknown.
// It is diagnostic-only: catalogue publication goes through
// sustainedWattsForPublication and never accepts this fallback.
func sustainedWattsForClass(hwClass string) float64 {
	if entry, ok := sustainedWattsByHWClass[hwClass]; ok && entry.Watts() > 0 {
		return entry.Watts()
	}
	return 30.0
}

// sustainedWattsForPublication returns the exact MEASURED power authority for
// one benchmark hardware class. An ASSUMED row is useful for diagnostic
// sensitivity analysis, but it cannot prove that a public price covers the
// supply side. Unknown classes are refused rather than inheriting another
// class's default.
func sustainedWattsForPublication(b measuredThroughput) (governedSustainedWatts, error) {
	entry, err := sustainedWattsEntryForPublication(b.HWClass)
	if err != nil {
		return governedSustainedWatts{}, err
	}
	if err := validatePricingPowerCitation(b, entry, cataloguePowerNow()); err != nil {
		return governedSustainedWatts{}, err
	}
	return entry, nil
}

func sustainedWattsEntryForPublication(hwClass string) (governedSustainedWatts, error) {
	hwClass = strings.TrimSpace(hwClass)
	entry, ok := sustainedWattsByHWClass[hwClass]
	if !ok {
		return governedSustainedWatts{}, fmt.Errorf(
			"catalogue publication has no exact sustained-watts authority for hardware class %q", hwClass)
	}
	if entry.Watts() <= 0 || strings.TrimSpace(entry.Provenance()) == "" {
		return governedSustainedWatts{}, fmt.Errorf(
			"catalogue publication sustained-watts authority for hardware class %q is incomplete", hwClass)
	}
	if entry.Kind() != wattKindMeasured {
		return governedSustainedWatts{}, fmt.Errorf(
			"catalogue publication requires MEASURED sustained watts for hardware class %q; got %s",
			hwClass, entry.Kind())
	}
	return entry, nil
}

func init() {
	if err := validateSustainedWattsTable(); err != nil {
		panic("sustainedWattsByHWClass: " + err.Error())
	}
}

const defaultElectricityUSDPerKWh = 0.15

func catalogueConservativeUnitsPerSecond(b measuredThroughput) float64 {
	return b.UnitsPerSec * measuredThroughputHaircut
}

// targetSupplierUSDHr is the cost-plus supplier revenue target used only for
// the diagnostic floor (diagnosticCostFloorFromSupplierEconomics). Catalogue prices come
// from pricing/board.json (market board × positioning_multiplier).
const targetSupplierUSDHr = 2.0

type RepriceResult struct {
	ModelID             string  `json:"model_id"`
	JobType             string  `json:"job_type"`
	ReferencePricePer1K float64 `json:"reference_price_per_1k"`
	PricePer1K          float64 `json:"price_per_1k"`
	// SupplierShare is the physical workload's published market term. It is
	// stored on the result, rather than once on the schedule, so a schedule can
	// not silently apply the same take rate to every lane.
	SupplierShare     float64                          `json:"supplier_share,omitempty"`
	PhysicalAuthority CatalogueResultPhysicalAuthority `json:"physical_authority,omitempty"`
	Formula           string                           `json:"formula"` // human-readable, cites every real input (proof artifact)
}

const (
	cataloguePriceScheduleVersion                 = 3
	catalogueResultPhysicalAuthorityLegacyVersion = 1
	catalogueResultPhysicalAuthorityVersion       = 2
	catalogueScheduleCurrentUseFreshnessPolicy    = "catalogue-price-schedule-v1/min-board-throughput-power-valid-until"
)

// CatalogueThroughputAuthoritySnapshot freezes the exact receipt bytes and
// semantic throughput denominator that made one catalogue row publishable.
// Current use re-opens the cited bytes and proves they still carry this exact
// BOUND authority; accepted historical replay uses only the frozen schedule.
type CatalogueThroughputAuthoritySnapshot struct {
	Citation                   string  `json:"citation"`
	ReceiptSHA256              string  `json:"receipt_sha256"`
	BenchmarkSummarySHA256     string  `json:"benchmark_summary_sha256,omitempty"`
	EngineBuildHash            string  `json:"engine_build_hash,omitempty"`
	EngineBuildIdentityPolicy  string  `json:"engine_build_identity_policy,omitempty"`
	HardwareIdentity           string  `json:"hardware_identity,omitempty"`
	FreshnessPolicy            string  `json:"freshness_policy"`
	MeasuredAt                 string  `json:"measured_at"`
	ValidUntil                 string  `json:"valid_until"`
	ObservedUnitsPerSecond     float64 `json:"observed_units_per_second"`
	HaircutPolicyRevision      string  `json:"haircut_policy_revision"`
	Haircut                    float64 `json:"haircut"`
	ConservativeUnitsPerSecond float64 `json:"conservative_units_per_second"`
}

type CataloguePowerCoveredWorkload struct {
	ModelID                   string `json:"model_id"`
	JobType                   string `json:"job_type"`
	ModelArtifactDigest       string `json:"model_artifact_digest"`
	RuntimeCellID             string `json:"runtime_cell_id,omitempty"`
	RuntimeProfileID          string `json:"runtime_profile_id,omitempty"`
	Engine                    string `json:"engine,omitempty"`
	EngineBuildHash           string `json:"engine_build_hash,omitempty"`
	EngineBuildIdentityPolicy string `json:"engine_build_identity_policy,omitempty"`
	HardwareIdentity          string `json:"hardware_identity,omitempty"`
}

// CataloguePowerAuthoritySnapshot freezes a whole-package,
// inference-shaped watts measurement. GPU-domain or idle measurements are not
// interchangeable with this supply-side cost boundary.
type CataloguePowerAuthoritySnapshot struct {
	Citation                  string                          `json:"citation"`
	ReceiptSHA256             string                          `json:"receipt_sha256"`
	RuntimeCellID             string                          `json:"runtime_cell_id,omitempty"`
	RuntimeProfileID          string                          `json:"runtime_profile_id,omitempty"`
	Engine                    string                          `json:"engine,omitempty"`
	EngineBuildHash           string                          `json:"engine_build_hash,omitempty"`
	EngineBuildIdentityPolicy string                          `json:"engine_build_identity_policy,omitempty"`
	HWClass                   string                          `json:"hardware_class,omitempty"`
	HardwareIdentity          string                          `json:"hardware_identity,omitempty"`
	FreshnessPolicy           string                          `json:"freshness_policy"`
	MeasurementBoundary       string                          `json:"measurement_boundary"`
	WorkloadClass             string                          `json:"workload_class"`
	Unit                      string                          `json:"unit"`
	AuthorityScope            string                          `json:"authority_scope"`
	Aggregation               string                          `json:"aggregation"`
	OperatingProtocol         string                          `json:"operating_protocol"`
	CoveredWorkloads          []CataloguePowerCoveredWorkload `json:"covered_workloads"`
	Watts                     float64                         `json:"watts"`
	MeasuredAt                string                          `json:"measured_at"`
	ValidUntil                string                          `json:"valid_until"`
}

// CatalogueResultPhysicalAuthority is the self-contained physical basis for
// one result. Identity is intentionally repeated here (rather than inferred
// from mutable process tables) so the schedule digest binds the exact model,
// workload, hardware and settlement-unit geometry that passed publication.
type CatalogueResultPhysicalAuthority struct {
	Version                   int                                  `json:"version"`
	ModelID                   string                               `json:"model_id"`
	JobType                   string                               `json:"job_type"`
	RuntimeCellID             string                               `json:"runtime_cell_id"`
	RuntimeProfileID          string                               `json:"runtime_profile_id"`
	ProfileRevision           string                               `json:"profile_revision"`
	Engine                    string                               `json:"engine"`
	EngineRevision            string                               `json:"engine_revision,omitempty"`
	EngineBuildHash           string                               `json:"engine_build_hash,omitempty"`
	EngineBuildIdentityPolicy string                               `json:"engine_build_identity_policy,omitempty"`
	HWClass                   string                               `json:"hardware_class"`
	HardwareIdentity          string                               `json:"hardware_identity,omitempty"`
	Unit                      string                               `json:"unit"`
	UnitScope                 string                               `json:"unit_scope"`
	ModelArtifactDigest       string                               `json:"model_artifact_digest"`
	Throughput                CatalogueThroughputAuthoritySnapshot `json:"throughput"`
	Power                     CataloguePowerAuthoritySnapshot      `json:"power"`
	ValidUntil                string                               `json:"valid_until"`
}

const (
	catalogueReferenceCurrency = "usd"
	priceFXRateEnv             = "MERC_PRICE_REFERENCE_TO_SETTLEMENT_RATE"
	priceFXRevisionEnv         = "MERC_PRICE_FX_REVISION"
)

// CataloguePriceSchedule is the all-or-nothing authority applied to models.
// It binds the exact board bytes, selector policy, per-workload supplier shares
// and complete model result set. A model row may point only at a durable
// schedule record.
type CataloguePriceSchedule struct {
	Version               int     `json:"version"`
	ReferenceCurrency     string  `json:"reference_currency"`
	SettlementCurrency    string  `json:"settlement_currency"`
	ReferenceToSettlement float64 `json:"reference_to_settlement_rate"`
	FXRevision            string  `json:"fx_revision"`
	BoardSHA256           string  `json:"board_sha256"`
	BoardSchemaVersion    int     `json:"board_schema_version"`
	BoardFetchedAt        string  `json:"board_fetched_at"`
	PositioningMultiplier float64 `json:"positioning_multiplier"`
	// v3 binds the named board policy and the earliest instant at which any
	// board, throughput or power input must be revalidated before current use.
	BoardFreshnessPolicy      string `json:"board_freshness_policy,omitempty"`
	BoardValidUntil           string `json:"board_valid_until,omitempty"`
	CurrentUseFreshnessPolicy string `json:"current_use_freshness_policy,omitempty"`
	CurrentUseValidUntil      string `json:"current_use_valid_until,omitempty"`
	// SupplierShare is only populated on v1 schedules. New schedules carry a
	// per-result share under SupplierSharePolicyRevision.
	SupplierShare               float64         `json:"supplier_share,omitempty"`
	SupplierSharePolicyRevision string          `json:"supplier_share_policy_revision,omitempty"`
	Results                     []RepriceResult `json:"results"`
	SHA256                      string          `json:"sha256"`
}

// diagnosticCostFloorFromSupplierEconomics calculates a cost-plus comparison:
// target supplier $/hr on the slowest measured laptop class. It is kept only
// for unit tests and the market-gap report. It cannot publish or derive a live
// catalogue price; BuildCataloguePriceSchedule is the sole price authority.
func diagnosticCostFloorFromSupplierEconomics(b measuredThroughput, supplierShare, electricityUSDPerKWh float64) RepriceResult {
	watts := sustainedWattsForClass(b.HWClass)
	electricityUSDHr := watts / 1000.0 * electricityUSDPerKWh
	unitsPerHr := catalogueConservativeUnitsPerSecond(b) * 3600.0

	denom := unitsPerHr / 1000.0 * supplierShare
	var price float64
	if denom > 0 {
		price = (targetSupplierUSDHr + electricityUSDHr) / denom
	}

	formula := fmt.Sprintf(
		"price_per_1k = (target_supplier_usd_hr=%.2f + electricity_usd_hr=%.4f) / (units_per_hr=%.1f/1000 * supplier_share=%.4f) = %.8f  [source: %s, hw_class=%s, %.0fW @ $%.2f/kWh, platform_take=%.2f%%]",
		targetSupplierUSDHr, electricityUSDHr, unitsPerHr, supplierShare, price,
		b.SourceCitation, b.HWClass, watts, electricityUSDPerKWh, (1-supplierShare)*100,
	)
	return RepriceResult{
		ModelID: b.ModelID, JobType: b.JobType,
		ReferencePricePer1K: price, PricePer1K: price, Formula: formula,
	}
}

// --- market board -----------------------------------------------------------

type priceBoardObservation struct {
	Provider  string  `json:"provider"`
	Model     string  `json:"model"`
	USDPer1K  float64 `json:"usd_per_1k"`
	USDPer1M  float64 `json:"usd_per_1m"`
	SourceURL string  `json:"source_url"`
	FetchedAt string  `json:"fetched_at"`
	Notes     string  `json:"notes"`
}

type priceBoardClass struct {
	Description  string                  `json:"description"`
	JobType      string                  `json:"job_type"`
	ModelIDs     []string                `json:"model_ids"`
	Observations []priceBoardObservation `json:"observations"`
}

type priceBoard struct {
	SchemaVersion         int                        `json:"schema_version"`
	Unit                  string                     `json:"unit"`
	FetchedAt             string                     `json:"fetched_at"`
	PositioningMultiplier float64                    `json:"positioning_multiplier"`
	Classes               map[string]priceBoardClass `json:"classes"`
}

// The board is loaded once per process and then reused, so /version can name a
// single answer to "which prices are you serving from".
//
// A FAILURE is deliberately not cached. sync.Once cached both outcomes, which
// made the price authority a function of whoever called first: main() resolves
// at startup in production, but in any other process the first caller wins, and
// under `go test` that is whichever test the -run filter happens to order first.
// A test that sets MERC_ENV=production for its own reasons therefore poisoned
// the board permanently and every later test that priced anything failed with a
// production refusal it never asked for. Caching the failure also buys nothing
// real: main() log.Fatalf's on a load error, so the process it matters in never
// gets a second call, and every other caller would rather retry than be told
// about a resolution that is no longer the one in effect.
var (
	priceBoardMu     sync.Mutex
	priceBoardCached *priceBoard
	priceBoardSHA256 string
)

// priceBoardSource records WHERE the loaded board came from, so /version can say
// so. A deployment that cannot answer "which price board are you serving from"
// cannot be audited after the fact.
var priceBoardSource string

func loadPriceBoard() (*priceBoard, error) {
	priceBoardMu.Lock()
	defer priceBoardMu.Unlock()
	if priceBoardCached != nil {
		return priceBoardCached, nil
	}
	var priceBoardErr error
	func() {
		resolved, err := resolvePriceBoard(os.Getenv("MERC_ENV"))
		if err != nil {
			priceBoardErr = err
			return
		}
		path := resolved.Path
		raw, err := os.ReadFile(path)
		if err != nil {
			priceBoardErr = fmt.Errorf("read price board %s (%s): %w",
				path, resolved.Source, err)
			return
		}
		// Before parsing. Bytes the operator did not approve must not be given the
		// chance to be well-formed.
		digest, err := verifyPriceBoardDigest(raw, resolved.ExpectedDigest)
		if err != nil {
			priceBoardErr = err
			return
		}
		priceBoardSource = resolved.Source
		var b priceBoard
		if err := json.Unmarshal(raw, &b); err != nil {
			priceBoardErr = fmt.Errorf("parse price board %s: %w", path, err)
			return
		}
		if b.SchemaVersion != 1 {
			priceBoardErr = fmt.Errorf("price board schema_version must be 1, got %d", b.SchemaVersion)
			return
		}
		if b.Unit != "usd_per_1k_units" {
			priceBoardErr = fmt.Errorf("price board unit must be usd_per_1k_units, got %q", b.Unit)
			return
		}
		if b.PositioningMultiplier <= 0 || math.IsNaN(b.PositioningMultiplier) || math.IsInf(b.PositioningMultiplier, 0) {
			priceBoardErr = fmt.Errorf("price board positioning_multiplier must be finite and > 0, got %v", b.PositioningMultiplier)
			return
		}
		if len(b.Classes) == 0 {
			priceBoardErr = fmt.Errorf("price board has no classes")
			return
		}
		// Parse the timestamp here so a malformed date is caught at load, but do
		// not age-filter: the board is data, and the read-only price page should
		// render what it says. Staleness is enforced where new pricing authority
		// is minted, in BuildCataloguePriceSchedule.
		if _, err := parseBoardTimestamp(b.FetchedAt); err != nil {
			priceBoardErr = fmt.Errorf("price board fetched_at is unusable: %w", err)
			return
		}
		priceBoardSHA256 = digest
		priceBoardCached = &b
	}()
	return priceBoardCached, priceBoardErr
}

// repriceFromMarketBoard prices one catalogue model from the sole governed
// selector: confidence-weighted median evidence times the positioning
// multiplier. Do not reintroduce an alternate min/mean derivation here: a
// price board is authority only when every published row follows one policy.
func repriceFromMarketBoard(modelID, jobType string, board *priceBoard) (RepriceResult, bool) {
	for className, class := range board.Classes {
		match := false
		for _, id := range class.ModelIDs {
			if id == modelID {
				match = true
				break
			}
		}
		if !match {
			// Fall back to job_type match only when model_ids is empty.
			if len(class.ModelIDs) == 0 && class.JobType == jobType {
				match = true
			}
		}
		if !match {
			continue
		}
		minPx, chosen, gerr := confidenceWeightedMedianUSDPer1K(className, class)
		if gerr != nil {
			// Ungoverned class: leave the existing catalogue price alone rather
			// than replacing it with something the evidence cannot support.
			continue
		}
		provider, model, source := chosen.Provider, chosen.Model, chosen.Source
		price := minPx * board.PositioningMultiplier
		formula := fmt.Sprintf(
			"price_per_1k = confidence_weighted_median(board[%s])×positioning_multiplier = %.8f×%.4f = %.8f  [median_obs=%s/%s source=%s fetched_at=%s]",
			className, minPx, board.PositioningMultiplier, price, provider, model, source, board.FetchedAt,
		)
		jt := jobType
		if class.JobType != "" {
			jt = class.JobType
		}
		return RepriceResult{
			ModelID: modelID, JobType: jt,
			ReferencePricePer1K: price, PricePer1K: price, Formula: formula,
		}, true
	}
	return RepriceResult{}, false
}

func catalogueFXAuthority(referenceCurrency, settlementCurrency string) (float64, string, error) {
	if referenceCurrency == settlementCurrency {
		return 1, "identity-" + referenceCurrency, nil
	}
	rawRate := strings.TrimSpace(os.Getenv(priceFXRateEnv))
	rawRevision := strings.TrimSpace(os.Getenv(priceFXRevisionEnv))
	if rawRate == "" || rawRevision == "" {
		return 0, "", fmt.Errorf(
			"%s and %s are required when catalogue reference currency %s differs from settlement currency %s",
			priceFXRateEnv, priceFXRevisionEnv, referenceCurrency, settlementCurrency,
		)
	}
	rate, err := strconv.ParseFloat(rawRate, 64)
	if err != nil || rate <= 0 || math.IsNaN(rate) || math.IsInf(rate, 0) {
		return 0, "", fmt.Errorf("%s must be a finite positive number, got %q", priceFXRateEnv, rawRate)
	}
	if len(rawRevision) > 128 || strings.ContainsAny(rawRevision, "\r\n\t") {
		return 0, "", fmt.Errorf("%s must be a single non-empty line of at most 128 bytes", priceFXRevisionEnv)
	}
	return rate, rawRevision, nil
}

func ceilPricePer1K(value float64) float64 {
	const scale = 100000000
	return math.Ceil(value*scale) / scale
}

// canonicalPricePer1K gives the reference-price projection a fixed decimal
// boundary before it is persisted. The database stores eighteen fractional
// places, but float64 has about fifteen significant decimal digits; rounding
// here means a schedule result and a NUMERIC round-trip name the same decimal
// rather than differing in the last binary bit. Settlement publication still
// uses ceilPricePer1K so the buyer-facing ceiling never rounds downward.
func canonicalPricePer1K(value float64) float64 {
	const scale = 1_000_000_000_000_000.0 // 15 decimal places
	if !finiteNonNegative(value) || value <= 0 {
		return 0
	}
	return math.Round(value*scale) / scale
}

func cataloguePriceScheduleDigest(schedule CataloguePriceSchedule) (string, error) {
	payload := schedule
	payload.SHA256 = ""
	payload.Results = append([]RepriceResult(nil), schedule.Results...)
	sort.Slice(payload.Results, func(i, j int) bool {
		if payload.Results[i].ModelID == payload.Results[j].ModelID {
			return payload.Results[i].JobType < payload.Results[j].JobType
		}
		return payload.Results[i].ModelID < payload.Results[j].ModelID
	})
	raw, err := json.Marshal(payload)
	if err != nil {
		return "", fmt.Errorf("marshal catalogue price schedule: %w", err)
	}
	sum := sha256.Sum256(raw)
	return fmt.Sprintf("%x", sum[:]), nil
}

func canonicalCatalogueTimestamp(label, value string) (time.Time, error) {
	parsed, err := time.Parse(time.RFC3339, strings.TrimSpace(value))
	if err != nil {
		return time.Time{}, fmt.Errorf("%s must be an RFC3339 timestamp: %w", label, err)
	}
	parsed = parsed.UTC()
	if value != parsed.Format(time.RFC3339) {
		return time.Time{}, fmt.Errorf("%s must be canonical UTC RFC3339", label)
	}
	return parsed, nil
}

func validateCatalogueResultPhysicalAuthority(
	result RepriceResult,
) (time.Time, error) {
	physical := result.PhysicalAuthority
	if (physical.Version != catalogueResultPhysicalAuthorityLegacyVersion &&
		physical.Version != catalogueResultPhysicalAuthorityVersion) ||
		physical.ModelID != result.ModelID || physical.JobType != result.JobType ||
		strings.TrimSpace(physical.RuntimeCellID) == "" ||
		strings.TrimSpace(physical.RuntimeProfileID) == "" ||
		strings.TrimSpace(physical.ProfileRevision) == "" ||
		strings.TrimSpace(physical.Engine) == "" ||
		strings.TrimSpace(physical.HWClass) == "" ||
		strings.TrimSpace(physical.Unit) == "" || strings.TrimSpace(physical.UnitScope) == "" ||
		!digestPattern.MatchString(physical.ModelArtifactDigest) {
		return time.Time{}, fmt.Errorf(
			"catalogue result %s/%s lacks complete physical identity", result.ModelID, result.JobType)
	}
	throughput := physical.Throughput
	if physical.Version == catalogueResultPhysicalAuthorityVersion {
		if !engineBuildHashPattern.MatchString(physical.EngineBuildHash) ||
			!historicalEngineBuildIdentityPolicyMatches(
				physical.EngineBuildIdentityPolicy,
				throughput.EngineBuildIdentityPolicy,
			) ||
			!validCanonicalHardwareIdentity(physical.HardwareIdentity) ||
			throughput.EngineBuildHash != physical.EngineBuildHash ||
			throughput.HardwareIdentity != physical.HardwareIdentity {
			return time.Time{}, fmt.Errorf(
				"catalogue result %s/%s lacks exact execution-build/device identity",
				result.ModelID, result.JobType)
		}
	} else if physical.EngineBuildHash != "" || physical.EngineBuildIdentityPolicy != "" ||
		physical.HardwareIdentity != "" || throughput.EngineBuildHash != "" ||
		throughput.EngineBuildIdentityPolicy != "" || throughput.HardwareIdentity != "" {
		return time.Time{}, fmt.Errorf("legacy catalogue physical authority carries future build/device identity")
	}
	if strings.TrimSpace(throughput.Citation) == "" ||
		!digestPattern.MatchString(throughput.ReceiptSHA256) ||
		throughput.FreshnessPolicy != catalogueThroughputFreshnessPolicy ||
		!finiteNonNegative(throughput.ObservedUnitsPerSecond) ||
		throughput.ObservedUnitsPerSecond <= 0 ||
		throughput.HaircutPolicyRevision != runtimeCellPerformancePolicyRevision ||
		throughput.Haircut != measuredThroughputHaircut ||
		!finiteNonNegative(throughput.ConservativeUnitsPerSecond) ||
		throughput.ConservativeUnitsPerSecond <= 0 {
		return time.Time{}, fmt.Errorf(
			"catalogue result %s/%s lacks complete throughput authority", result.ModelID, result.JobType)
	}
	wantConservative := throughput.ObservedUnitsPerSecond * throughput.Haircut
	if math.Abs(throughput.ConservativeUnitsPerSecond-wantConservative) > math.Abs(wantConservative)*1e-12 {
		return time.Time{}, fmt.Errorf(
			"catalogue result %s/%s conservative throughput does not equal observed rate times governed haircut",
			result.ModelID, result.JobType)
	}
	throughputAt, err := canonicalCatalogueTimestamp("throughput measured_at", throughput.MeasuredAt)
	if err != nil {
		return time.Time{}, err
	}
	throughputUntil, err := canonicalCatalogueTimestamp("throughput valid_until", throughput.ValidUntil)
	if err != nil {
		return time.Time{}, err
	}
	if !throughputUntil.Equal(throughputAt.Add(catalogueThroughputMaxAge)) {
		return time.Time{}, fmt.Errorf("catalogue throughput valid_until does not follow %s",
			catalogueThroughputFreshnessPolicy)
	}
	power := physical.Power
	if physical.Version == catalogueResultPhysicalAuthorityVersion {
		if power.RuntimeCellID != physical.RuntimeCellID ||
			power.RuntimeProfileID != physical.RuntimeProfileID ||
			power.Engine != physical.Engine ||
			power.EngineBuildHash != physical.EngineBuildHash ||
			!historicalEngineBuildIdentityPolicyMatches(
				physical.EngineBuildIdentityPolicy,
				power.EngineBuildIdentityPolicy,
			) ||
			power.HWClass != physical.HWClass ||
			power.HardwareIdentity != physical.HardwareIdentity {
			return time.Time{}, fmt.Errorf(
				"catalogue result %s/%s power authority does not bind exact runtime/build/device identity",
				result.ModelID, result.JobType)
		}
	} else if power.RuntimeCellID != "" || power.RuntimeProfileID != "" ||
		power.Engine != "" || power.EngineBuildHash != "" ||
		power.EngineBuildIdentityPolicy != "" || power.HWClass != "" ||
		power.HardwareIdentity != "" {
		return time.Time{}, fmt.Errorf("legacy catalogue power authority carries future exact identity")
	}
	if strings.TrimSpace(power.Citation) == "" ||
		!digestPattern.MatchString(power.ReceiptSHA256) ||
		power.FreshnessPolicy != cataloguePowerFreshnessPolicy ||
		power.MeasurementBoundary != "whole_package" ||
		power.WorkloadClass != "inference_shaped" || power.Unit != "watts" ||
		power.AuthorityScope != cataloguePowerAuthorityScope ||
		power.Aggregation != cataloguePowerAggregation ||
		power.OperatingProtocol != cataloguePowerOperatingProtocol ||
		len(power.CoveredWorkloads) == 0 ||
		!finiteNonNegative(power.Watts) || power.Watts <= 0 {
		return time.Time{}, fmt.Errorf(
			"catalogue result %s/%s lacks complete whole-package inference-shaped power authority",
			result.ModelID, result.JobType)
	}
	coveredTarget := false
	seenCoverage := map[string]bool{}
	for _, covered := range power.CoveredWorkloads {
		if strings.TrimSpace(covered.ModelID) == "" || strings.TrimSpace(covered.JobType) == "" ||
			!digestPattern.MatchString(covered.ModelArtifactDigest) {
			return time.Time{}, fmt.Errorf("catalogue power coverage identity is incomplete")
		}
		key := covered.ModelID + "\x00" + covered.JobType
		if seenCoverage[key] {
			return time.Time{}, fmt.Errorf("catalogue power coverage repeats %s/%s", covered.ModelID, covered.JobType)
		}
		seenCoverage[key] = true
		if physical.Version == catalogueResultPhysicalAuthorityVersion {
			if strings.TrimSpace(covered.RuntimeCellID) == "" ||
				strings.TrimSpace(covered.RuntimeProfileID) == "" ||
				strings.TrimSpace(covered.Engine) == "" ||
				!engineBuildHashPattern.MatchString(covered.EngineBuildHash) ||
				!historicalEngineBuildIdentityPolicyMatches(
					covered.EngineBuildIdentityPolicy,
				) ||
				!validCanonicalHardwareIdentity(covered.HardwareIdentity) {
				return time.Time{}, fmt.Errorf(
					"catalogue power coverage %s/%s lacks exact runtime/build/device identity",
					covered.ModelID, covered.JobType)
			}
		} else if covered.RuntimeCellID != "" || covered.RuntimeProfileID != "" ||
			covered.Engine != "" || covered.EngineBuildHash != "" ||
			covered.EngineBuildIdentityPolicy != "" || covered.HardwareIdentity != "" {
			return time.Time{}, fmt.Errorf("legacy catalogue power coverage carries future exact identity")
		}
		if covered.ModelID == result.ModelID && covered.JobType == result.JobType &&
			covered.ModelArtifactDigest == physical.ModelArtifactDigest &&
			(physical.Version == catalogueResultPhysicalAuthorityLegacyVersion ||
				(covered.RuntimeCellID == physical.RuntimeCellID &&
					covered.RuntimeProfileID == physical.RuntimeProfileID &&
					covered.Engine == physical.Engine &&
					covered.EngineBuildHash == physical.EngineBuildHash &&
					covered.EngineBuildIdentityPolicy == physical.EngineBuildIdentityPolicy &&
					covered.HardwareIdentity == physical.HardwareIdentity)) {
			coveredTarget = true
		}
	}
	if !coveredTarget {
		return time.Time{}, fmt.Errorf(
			"catalogue power envelope does not cover exact result %s/%s", result.ModelID, result.JobType)
	}
	powerAt, err := canonicalCatalogueTimestamp("power measured_at", power.MeasuredAt)
	if err != nil {
		return time.Time{}, err
	}
	powerUntil, err := canonicalCatalogueTimestamp("power valid_until", power.ValidUntil)
	if err != nil {
		return time.Time{}, err
	}
	if !powerUntil.Equal(powerAt.Add(cataloguePowerMaxAge)) {
		return time.Time{}, fmt.Errorf("catalogue power valid_until does not follow %s",
			cataloguePowerFreshnessPolicy)
	}
	wantValidUntil := throughputUntil
	if powerUntil.Before(wantValidUntil) {
		wantValidUntil = powerUntil
	}
	physicalUntil, err := canonicalCatalogueTimestamp("physical authority valid_until", physical.ValidUntil)
	if err != nil {
		return time.Time{}, err
	}
	if !physicalUntil.Equal(wantValidUntil) {
		return time.Time{}, fmt.Errorf(
			"catalogue result %s/%s physical valid_until is not the earliest receipt boundary",
			result.ModelID, result.JobType)
	}
	return physicalUntil, nil
}

func validateCataloguePriceSchedule(schedule CataloguePriceSchedule) error {
	if schedule.Version != 1 && schedule.Version != 2 && schedule.Version != cataloguePriceScheduleVersion {
		return fmt.Errorf("unsupported catalogue price schedule version %d", schedule.Version)
	}
	if schedule.ReferenceCurrency != catalogueReferenceCurrency ||
		!finiteNonNegative(schedule.ReferenceToSettlement) || schedule.ReferenceToSettlement <= 0 ||
		strings.TrimSpace(schedule.FXRevision) == "" || len(schedule.FXRevision) > 128 ||
		strings.ContainsAny(schedule.FXRevision, "\r\n\t") ||
		!digestPattern.MatchString(schedule.BoardSHA256) ||
		schedule.BoardSchemaVersion != 1 ||
		strings.TrimSpace(schedule.BoardFetchedAt) == "" ||
		!finiteNonNegative(schedule.PositioningMultiplier) || schedule.PositioningMultiplier <= 0 ||
		len(schedule.Results) != len(repricingBenchmarks) {
		return fmt.Errorf("catalogue price schedule metadata is incomplete or invalid")
	}
	switch schedule.Version {
	case 1:
		if !finiteNonNegative(schedule.SupplierShare) || schedule.SupplierShare <= 0 || schedule.SupplierShare > 1 ||
			schedule.SupplierSharePolicyRevision != "" {
			return fmt.Errorf("legacy catalogue price schedule has invalid supplier share authority")
		}
	case 2, cataloguePriceScheduleVersion:
		if schedule.SupplierShare != 0 || schedule.SupplierSharePolicyRevision != supplierSharePolicyRevision {
			return fmt.Errorf("catalogue price schedule must bind %s per-workload supplier shares", supplierSharePolicyRevision)
		}
	}
	if schedule.Version < cataloguePriceScheduleVersion {
		if schedule.BoardFreshnessPolicy != "" || schedule.BoardValidUntil != "" ||
			schedule.CurrentUseFreshnessPolicy != "" || schedule.CurrentUseValidUntil != "" {
			return fmt.Errorf("historical catalogue price schedule carries unsupported current-use authority")
		}
	} else {
		if schedule.BoardFreshnessPolicy != catalogueBoardFreshnessPolicy ||
			schedule.CurrentUseFreshnessPolicy != catalogueScheduleCurrentUseFreshnessPolicy {
			return fmt.Errorf("catalogue price schedule lacks named current-use freshness policies")
		}
	}
	settlement, err := ParseCurrency(schedule.SettlementCurrency)
	if err != nil || settlement.Code() != schedule.SettlementCurrency {
		return fmt.Errorf("catalogue price schedule settlement currency is invalid")
	}
	if schedule.ReferenceCurrency == schedule.SettlementCurrency {
		if schedule.ReferenceToSettlement != 1 || schedule.FXRevision != "identity-"+schedule.ReferenceCurrency {
			return fmt.Errorf("identity catalogue FX authority is invalid")
		}
	} else if strings.HasPrefix(schedule.FXRevision, "identity-") {
		return fmt.Errorf("cross-currency catalogue schedule cannot use identity FX authority")
	}
	expected := make(map[string]string, len(repricingBenchmarks))
	for _, b := range repricingBenchmarks {
		expected[b.ModelID] = b.JobType
	}
	seen := make(map[string]bool, len(schedule.Results))
	var earliestPhysical time.Time
	for _, result := range schedule.Results {
		if seen[result.ModelID] || expected[result.ModelID] != result.JobType ||
			!finiteNonNegative(result.ReferencePricePer1K) || result.ReferencePricePer1K <= 0 ||
			!finiteNonNegative(result.PricePer1K) || result.PricePer1K <= 0 ||
			strings.TrimSpace(result.Formula) == "" ||
			!strings.Contains(result.Formula, "board_sha256="+schedule.BoardSHA256) ||
			!strings.Contains(result.Formula, "fx_revision="+schedule.FXRevision) {
			return fmt.Errorf("invalid catalogue price result for model %q", result.ModelID)
		}
		if schedule.Version == 1 {
			if result.SupplierShare != 0 {
				return fmt.Errorf("legacy catalogue price result for model %q carries a per-workload share", result.ModelID)
			}
		} else if err := validateSupplierSharePolicy(result.JobType, result.ModelID, result.SupplierShare); err != nil {
			return fmt.Errorf("invalid catalogue supplier share for model %q: %w", result.ModelID, err)
		} else if !strings.Contains(result.Formula, "supplier_share_policy="+supplierSharePolicyRevision) ||
			!strings.Contains(result.Formula, fmt.Sprintf("supplier_share=%.8f", result.SupplierShare)) {
			return fmt.Errorf("catalogue price result for model %q lacks supplier-share provenance", result.ModelID)
		}
		wantPrice := ceilPricePer1K(result.ReferencePricePer1K * schedule.ReferenceToSettlement)
		if math.Abs(result.PricePer1K-wantPrice) > 0.0000000001 {
			return fmt.Errorf("catalogue price result for model %q is inconsistent with FX authority", result.ModelID)
		}
		if schedule.Version < cataloguePriceScheduleVersion {
			if !reflect.DeepEqual(result.PhysicalAuthority, CatalogueResultPhysicalAuthority{}) {
				return fmt.Errorf("historical catalogue result for model %q carries unsupported physical authority", result.ModelID)
			}
		} else {
			physicalUntil, err := validateCatalogueResultPhysicalAuthority(result)
			if err != nil {
				return err
			}
			if earliestPhysical.IsZero() || physicalUntil.Before(earliestPhysical) {
				earliestPhysical = physicalUntil
			}
		}
		seen[result.ModelID] = true
	}
	if schedule.Version == cataloguePriceScheduleVersion {
		boardUntil, err := canonicalCatalogueTimestamp("board valid_until", schedule.BoardValidUntil)
		if err != nil {
			return err
		}
		currentUseUntil, err := canonicalCatalogueTimestamp("schedule current_use_valid_until", schedule.CurrentUseValidUntil)
		if err != nil {
			return err
		}
		wantCurrentUseUntil := boardUntil
		if !earliestPhysical.IsZero() && earliestPhysical.Before(wantCurrentUseUntil) {
			wantCurrentUseUntil = earliestPhysical
		}
		if !currentUseUntil.Equal(wantCurrentUseUntil) {
			return fmt.Errorf("catalogue schedule current_use_valid_until is not the earliest board or physical boundary")
		}
	}
	digest, err := cataloguePriceScheduleDigest(schedule)
	if err != nil {
		return err
	}
	if schedule.SHA256 == "" || schedule.SHA256 != digest {
		return fmt.Errorf("catalogue price schedule digest mismatch")
	}
	return nil
}

// BuildCataloguePriceSchedule derives one complete, canonical schedule from the
// exact governed board bytes. Any unpriced or underwater measured model blocks
// the entire schedule; boot never applies a profitable subset and leaves the
// rest on stale terms. Each result receives its own reviewed physical-workload
// share; there is intentionally no process-wide take-rate input.
//
// Every repricingBenchmark citation is re-validated here so a price row that
// cites an unbindable artifact cannot reach buyers even if a future edit
// bypasses the unit test.
func BuildCataloguePriceSchedule() (CataloguePriceSchedule, error) {
	if err := validateAllRepricingBenchmarkCitations(); err != nil {
		return CataloguePriceSchedule{}, err
	}
	loaded, err := loadPriceBoard()
	if err != nil {
		return CataloguePriceSchedule{}, err
	}
	// Publication is where market evidence becomes pricing authority, so it is
	// where age is enforced. Work on a copy: dropping stale rows must not mutate
	// the cached board that the read-only price page renders.
	board, err := boardAsOfPublication(loaded, priceBoardNow())
	if err != nil {
		return CataloguePriceSchedule{}, err
	}
	boardValidUntil, err := catalogueBoardValidUntil(board)
	if err != nil {
		return CataloguePriceSchedule{}, err
	}
	settlement, err := SettlementCurrency()
	if err != nil {
		return CataloguePriceSchedule{}, fmt.Errorf("build catalogue price schedule: %w", err)
	}
	settlementCurrency := settlement.Code()
	fxRate, fxRevision, err := catalogueFXAuthority(catalogueReferenceCurrency, settlementCurrency)
	if err != nil {
		return CataloguePriceSchedule{}, err
	}
	out := make([]RepriceResult, 0, len(repricingBenchmarks))
	currentUseValidUntil := boardValidUntil
	for _, b := range repricingBenchmarks {
		r, ok := repriceFromMarketBoard(b.ModelID, b.JobType, board)
		if !ok {
			return CataloguePriceSchedule{}, fmt.Errorf(
				"price board has no governed price for measured model %s", b.ModelID,
			)
		}
		referencePrice := canonicalPricePer1K(r.PricePer1K)
		if referencePrice <= 0 {
			return CataloguePriceSchedule{}, fmt.Errorf("model %s has no finite canonical reference price", b.ModelID)
		}
		supplierShare, serr := supplierShareForWorkload(b.JobType, b.ModelID)
		if serr != nil {
			return CataloguePriceSchedule{}, serr
		}
		physical, perr := buildCatalogueResultPhysicalAuthority(b)
		if perr != nil {
			return CataloguePriceSchedule{}, perr
		}
		if gerr := governPublishedPriceAtWatts(
			b, referencePrice, supplierShare, physical.Power.Watts,
		); gerr != nil {
			return CataloguePriceSchedule{}, gerr
		}
		r.ReferencePricePer1K = referencePrice
		r.PricePer1K = ceilPricePer1K(referencePrice * fxRate)
		r.SupplierShare = supplierShare
		r.PhysicalAuthority = physical
		r.Formula += fmt.Sprintf(
			" board_sha256=%s reference_price_per_1k=%.15f reference_currency=%s reference_to_settlement_rate=%.12g fx_revision=%s settlement_price_per_1k=%.8f settlement_currency=%s supplier_share_policy=%s supplier_share=%.8f",
			priceBoardSHA256, r.ReferencePricePer1K, catalogueReferenceCurrency,
			fxRate, fxRevision, r.PricePer1K, settlementCurrency,
			supplierSharePolicyRevision, supplierShare,
		)
		out = append(out, r)
		physicalValidUntil, _ := time.Parse(time.RFC3339, physical.ValidUntil)
		if physicalValidUntil.Before(currentUseValidUntil) {
			currentUseValidUntil = physicalValidUntil
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ModelID < out[j].ModelID })
	schedule := CataloguePriceSchedule{
		Version:                     cataloguePriceScheduleVersion,
		ReferenceCurrency:           catalogueReferenceCurrency,
		SettlementCurrency:          settlementCurrency,
		ReferenceToSettlement:       fxRate,
		FXRevision:                  fxRevision,
		BoardSHA256:                 priceBoardSHA256,
		BoardSchemaVersion:          board.SchemaVersion,
		BoardFetchedAt:              board.FetchedAt,
		PositioningMultiplier:       board.PositioningMultiplier,
		BoardFreshnessPolicy:        catalogueBoardFreshnessPolicy,
		BoardValidUntil:             boardValidUntil.UTC().Format(time.RFC3339),
		CurrentUseFreshnessPolicy:   catalogueScheduleCurrentUseFreshnessPolicy,
		CurrentUseValidUntil:        currentUseValidUntil.UTC().Format(time.RFC3339),
		SupplierSharePolicyRevision: supplierSharePolicyRevision,
		Results:                     out,
	}
	schedule.SHA256, err = cataloguePriceScheduleDigest(schedule)
	if err != nil {
		return CataloguePriceSchedule{}, err
	}
	if err := validateCataloguePriceSchedule(schedule); err != nil {
		return CataloguePriceSchedule{}, err
	}
	return schedule, nil
}

// PublishedCatalogueResults returns the all-or-nothing result set from the
// market-board schedule. It does not calculate prices from supplier costs.
// Startup uses the error-returning schedule builder and therefore cannot
// silently retain a stale partial catalogue.
func PublishedCatalogueResults() []RepriceResult {
	schedule, err := BuildCataloguePriceSchedule()
	if err != nil {
		log.Printf("catalogue price schedule rejected: %v", err)
		return nil
	}
	return append([]RepriceResult(nil), schedule.Results...)
}

// CostFloorVsMarket reports, for each measured model, the cost-plus floor
// versus the market-board catalogue price. Used in tests and operator reports.
type CostFloorVsMarket struct {
	ModelID          string
	JobType          string
	CostPlusPer1K    float64
	MarketBoardPer1K float64
	GapRatio         float64 // cost_plus / market; >1 means cost basis above market
}

func CompareCostFloorToMarketBoard(supplierShare float64) []CostFloorVsMarket {
	board, err := loadPriceBoard()
	if err != nil {
		return nil
	}
	var out []CostFloorVsMarket
	for _, b := range repricingBenchmarks {
		cost := diagnosticCostFloorFromSupplierEconomics(b, supplierShare, defaultElectricityUSDPerKWh)
		mkt, ok := repriceFromMarketBoard(b.ModelID, b.JobType, board)
		if !ok || mkt.PricePer1K <= 0 {
			continue
		}
		out = append(out, CostFloorVsMarket{
			ModelID:          b.ModelID,
			JobType:          b.JobType,
			CostPlusPer1K:    cost.PricePer1K,
			MarketBoardPer1K: mkt.PricePer1K,
			GapRatio:         cost.PricePer1K / mkt.PricePer1K,
		})
	}
	return out
}

// SupplierViabilityAtMarket answers the question the price board cannot: at the
// catalogue price we actually charge, does the supplier clear their own power
// bill?
//
// Moving from cost-plus to a market board fixed a price that was ~460x above the
// market and, in the same stroke, cut supplier gross by the same factor. On the
// hardware the control plane currently admits (Apple Silicon, 138.7 tok/s
// measured) the two numbers land within a rounding error of each other: about
// $0.00436/hr of gross against $0.0045/hr of electricity. A marketplace whose
// suppliers pay to participate has no supply side, and that fact must be visible
// in an operator report rather than discovered by a supplier reading their
// electricity bill.
//
// This is a report, not a guard. Refusing to price at market would simply hide
// the problem behind an uncompetitive catalogue; the honest response is to make
// both facts legible at once and let the operator decide which side to fix.
type SupplierViabilityAtMarket struct {
	ModelID              string  `json:"model_id"`
	JobType              string  `json:"job_type"`
	SupplierShare        float64 `json:"supplier_share"`
	HWClass              string  `json:"hw_class"`
	MeasuredUnitsPerSec  float64 `json:"measured_units_per_sec"`
	CataloguePricePer1K  float64 `json:"catalogue_price_per_1k"`
	SupplierGrossUSDHr   float64 `json:"supplier_gross_usd_hr"`
	ElectricityUSDHr     float64 `json:"electricity_usd_hr"`
	NetUSDHr             float64 `json:"net_usd_hr"`
	BreakEvenUnitsPerSec float64 `json:"break_even_units_per_sec"`
	Viable               bool    `json:"viable"`
}

// SupplierViabilityReport evaluates every measured model against the catalogue
// price the buyer is actually charged, under the same physical-workload share
// policy that publication binds into the catalogue.
func SupplierViabilityReport() []SupplierViabilityAtMarket {
	board, err := loadPriceBoard()
	if err != nil {
		return nil
	}
	var out []SupplierViabilityAtMarket
	for _, b := range repricingBenchmarks {
		mkt, ok := repriceFromMarketBoard(b.ModelID, b.JobType, board)
		if !ok || mkt.PricePer1K <= 0 {
			continue
		}
		supplierShare, err := supplierShareForWorkload(b.JobType, b.ModelID)
		if err != nil {
			continue
		}
		watts := sustainedWattsForClass(b.HWClass)
		elec := watts / 1000.0 * defaultElectricityUSDPerKWh
		conservativeUnitsPerSec := catalogueConservativeUnitsPerSecond(b)
		unitsPerHr := conservativeUnitsPerSec * 3600.0
		gross := unitsPerHr / 1000.0 * mkt.PricePer1K * supplierShare
		var breakEven float64
		if perUnitHr := mkt.PricePer1K / 1000.0 * supplierShare; perUnitHr > 0 {
			breakEven = elec / perUnitHr / 3600.0
		}
		out = append(out, SupplierViabilityAtMarket{
			ModelID: b.ModelID, JobType: b.JobType, SupplierShare: supplierShare, HWClass: b.HWClass,
			MeasuredUnitsPerSec:  conservativeUnitsPerSec,
			CataloguePricePer1K:  mkt.PricePer1K,
			SupplierGrossUSDHr:   roundUSD(gross),
			ElectricityUSDHr:     roundUSD(elec),
			NetUSDHr:             roundUSD(gross - elec),
			BreakEvenUnitsPerSec: breakEven,
			Viable:               gross > elec,
		})
	}
	return out
}

const actualUSDBasisQuoteDerivedSettlement = "quote_derived_per_task_buyer_charge_settlement"

type PriceTuningBlockReason string

const priceTuningBlockedNoIndependentTelemetry PriceTuningBlockReason = "independent_execution_cost_telemetry_unavailable"

type CostDriftRow struct {
	JobType           string                 `json:"job_type"`
	ModelRef          string                 `json:"model_ref"`
	Samples           int                    `json:"samples"`             // quote-bound, terminal jobs behind this rollup
	AvgQuotedUSD      float64                `json:"avg_quoted_usd"`      // mean quotes.cost_expected_usd
	AvgActualUSD      float64                `json:"avg_actual_usd"`      // mean quote-derived jobs.actual_usd settlement
	DriftRatio        float64                `json:"drift_ratio"`         // settled charge / quoted charge; not a cost-overrun ratio
	DriftPct          float64                `json:"drift_pct"`           // (drift_ratio - 1) * 100, signed charge-realization difference
	ActualUSDBasis    string                 `json:"actual_usd_basis"`    // explicitly names the source semantics of AvgActualUSD
	UsingForTuning    bool                   `json:"using_for_tuning"`    // always false until independent economic telemetry exists
	TuningBlockReason PriceTuningBlockReason `json:"tuning_block_reason"` // machine-readable fail-closed reason
}

func finalizeCostDriftRow(d CostDriftRow) CostDriftRow {
	if d.AvgQuotedUSD > 0 {
		d.DriftRatio = d.AvgActualUSD / d.AvgQuotedUSD
		d.DriftPct = (d.DriftRatio - 1) * 100
	}
	d.ActualUSDBasis = actualUSDBasisQuoteDerivedSettlement
	d.UsingForTuning = false
	d.TuningBlockReason = priceTuningBlockedNoIndependentTelemetry
	return d
}

func (s *Store) CostDriftRollup(ctx context.Context) ([]CostDriftRow, error) {
	rows, err := s.pool.Query(ctx,
		`SELECT j.job_type,
		        COALESCE(j.model_ref,''),
		        COUNT(*),
		        COALESCE(AVG(q.cost_expected_usd),0),
		        COALESCE(AVG(j.actual_usd),0)
		   FROM jobs j
		   JOIN quotes q ON q.id = j.quote_id
		  WHERE j.quote_id IS NOT NULL
		    AND j.status IN ('complete','failed')
		    AND q.cost_expected_usd > 0
		  GROUP BY j.job_type, j.model_ref
		  ORDER BY COUNT(*) DESC, j.job_type, j.model_ref`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []CostDriftRow
	for rows.Next() {
		var d CostDriftRow
		if err := rows.Scan(&d.JobType, &d.ModelRef, &d.Samples, &d.AvgQuotedUSD, &d.AvgActualUSD); err != nil {
			return nil, err
		}
		out = append(out, finalizeCostDriftRow(d))
	}
	return out, rows.Err()
}

func (s *Store) ApplyRepricing(ctx context.Context, schedule CataloguePriceSchedule) (updated int, err error) {
	if err := revalidateCataloguePriceScheduleCurrent(schedule); err != nil {
		return 0, fmt.Errorf("refusing catalogue price schedule without current physical authority: %w", err)
	}
	if err := RequireSettlementCurrency(schedule.SettlementCurrency); err != nil {
		return 0, fmt.Errorf("refusing catalogue schedule for another settlement authority: %w", err)
	}
	scheduleJSON, err := json.Marshal(schedule)
	if err != nil {
		return 0, fmt.Errorf("marshal catalogue price schedule: %w", err)
	}
	tx, err := s.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.Serializable})
	if err != nil {
		return 0, err
	}
	defer tx.Rollback(ctx)
	if _, err := tx.Exec(ctx,
		`SELECT pg_advisory_xact_lock(hashtextextended('merc-catalogue-price-schedule-v1',0))`); err != nil {
		return 0, fmt.Errorf("lock catalogue price authority: %w", err)
	}
	// Publication can wait on another reprice while receipt or board authority
	// crosses its exact freshness boundary. Recheck after the serialization lock
	// and immediately before the first append-only write so the durable current
	// pointer is never minted from only the earlier, pre-lock observation.
	if err := revalidateCataloguePriceScheduleCurrent(schedule); err != nil {
		return 0, fmt.Errorf(
			"refusing catalogue price schedule without current physical authority under publication lock: %w",
			err)
	}
	if _, err := tx.Exec(ctx, `
		INSERT INTO catalogue_price_schedules (
		  sha256,version,reference_currency,settlement_currency,
		  reference_to_settlement_rate,fx_revision,board_sha256,board_schema_version,
		  board_fetched_at,positioning_multiplier,supplier_share,schedule_json
		) VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12)
		ON CONFLICT (sha256) DO NOTHING`,
		schedule.SHA256, schedule.Version, schedule.ReferenceCurrency, schedule.SettlementCurrency,
		schedule.ReferenceToSettlement, schedule.FXRevision,
		schedule.BoardSHA256, schedule.BoardSchemaVersion, schedule.BoardFetchedAt,
		schedule.PositioningMultiplier, nullIfZero(schedule.SupplierShare), scheduleJSON,
	); err != nil {
		return 0, fmt.Errorf("record catalogue price schedule: %w", err)
	}

	for _, result := range schedule.Results {
		var priorPrice, priorReferencePrice float64
		var priorSource, priorFormula, priorSchedule, priorCurrency string
		if err := tx.QueryRow(ctx, `
			SELECT COALESCE(price_per_1k,0),COALESCE(price_source,''),
			       COALESCE(price_formula,''),COALESCE(price_schedule_sha256,''),
			       COALESCE(price_currency,''),COALESCE(price_reference_per_1k,0)
			  FROM models WHERE id=$1 FOR UPDATE`,
			result.ModelID,
		).Scan(
			&priorPrice, &priorSource, &priorFormula, &priorSchedule,
			&priorCurrency, &priorReferencePrice,
		); err != nil {
			return 0, fmt.Errorf("lock catalogue model %s: %w", result.ModelID, err)
		}
		switch priorSource {
		case "", "seed", "measured_supplier_economics", "market_board":
		default:
			return 0, fmt.Errorf("model %s has operator-owned price source %q", result.ModelID, priorSource)
		}
		if priorSchedule == schedule.SHA256 {
			if math.Abs(priorPrice-result.PricePer1K) > 0.0000000001 ||
				math.Abs(priorReferencePrice-result.ReferencePricePer1K) > 0.0000000001 ||
				priorSource != "market_board" || priorFormula != result.Formula ||
				priorCurrency != schedule.SettlementCurrency {
				return 0, fmt.Errorf("model %s conflicts with its recorded price schedule", result.ModelID)
			}
			continue
		}
		// The skip above is keyed on models.price_schedule_sha256, but this row is
		// keyed on (schedule_sha256, model_id). Anything that resets model price
		// state without clearing the history - a re-seed, or a restore that brings
		// back one table and not the other - makes the skip miss while the row
		// still exists, and the control plane then cannot start at all. Recovery
		// is exactly when that must not happen.
		//
		// An existing row describing the same transition is therefore not an
		// error; one describing a different outcome for the same schedule still is.
		var recordedPrice, recordedReference, recordedSupplierShare float64
		var recordedCurrency, recordedFormula string
		switch err := tx.QueryRow(ctx, `
			SELECT price_per_1k,reference_price_per_1k,price_currency,price_formula,
			       COALESCE(supplier_share,0)
			  FROM model_price_history WHERE schedule_sha256=$1 AND model_id=$2`,
			schedule.SHA256, result.ModelID,
		).Scan(&recordedPrice, &recordedReference, &recordedCurrency, &recordedFormula, &recordedSupplierShare); {
		case errors.Is(err, pgx.ErrNoRows):
			if _, err := tx.Exec(ctx, `
				INSERT INTO model_price_history (
				  schedule_sha256,model_id,job_type,prior_price_per_1k,prior_price_source,
				  reference_price_per_1k,reference_currency,price_per_1k,
				  price_currency,price_formula,supplier_share
				) VALUES ($1,$2,$3,$4,NULLIF($5,''),$6,$7,$8,$9,$10,$11)`,
				schedule.SHA256, result.ModelID, result.JobType, priorPrice, priorSource,
				result.ReferencePricePer1K, schedule.ReferenceCurrency,
				result.PricePer1K, schedule.SettlementCurrency, result.Formula,
				nullIfZero(result.SupplierShare),
			); err != nil {
				return 0, fmt.Errorf("record price history for %s: %w", result.ModelID, err)
			}
		case err != nil:
			return 0, fmt.Errorf("read recorded price history for %s: %w", result.ModelID, err)
		default:
			if math.Abs(recordedPrice-result.PricePer1K) > 0.0000000001 ||
				math.Abs(recordedReference-result.ReferencePricePer1K) > 0.0000000001 ||
				recordedCurrency != schedule.SettlementCurrency ||
				recordedFormula != result.Formula ||
				math.Abs(recordedSupplierShare-result.SupplierShare) > 0.0000000001 {
				return 0, fmt.Errorf(
					"model %s already has a different recorded outcome for schedule %s",
					result.ModelID, schedule.SHA256)
			}
		}
		tag, err := tx.Exec(ctx, `
			UPDATE models
			   SET price_per_1k=$2,price_source='market_board',price_formula=$3,
			       price_reference_currency=$4,price_schedule_sha256=$5,
			       price_schedule_version=$6,price_currency=$7,
			       price_reference_per_1k=$8
			 WHERE id=$1`,
			result.ModelID, result.PricePer1K, result.Formula,
			schedule.ReferenceCurrency, schedule.SHA256, schedule.Version,
			schedule.SettlementCurrency, result.ReferencePricePer1K,
		)
		if err != nil {
			return 0, fmt.Errorf("apply repricing for %s: %w", result.ModelID, err)
		}
		if tag.RowsAffected() != 1 {
			return 0, fmt.Errorf("apply repricing for %s changed %d rows", result.ModelID, tag.RowsAffected())
		}
		updated++
	}
	if err := tx.Commit(ctx); err != nil {
		return 0, err
	}
	return updated, nil
}

// nullIfZero keeps v1 append-only schedule rows readable while ensuring a v2
// schedule never records a made-up process-wide supplier share in the legacy
// column. The v2 authority lives on each model_price_history result.
func nullIfZero(value float64) any {
	if value == 0 {
		return nil
	}
	return value
}
