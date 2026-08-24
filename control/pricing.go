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
// G070 (2026-08-14): the only current-bindable production lane is
// llama-3.2-1b-instruct-q4 / batch_infer on candle-metal-llama1-infer, measured
// under settlement geometry tokens/token_like_input_plus_max_output_tokens on
// this host (candle-metal-llama1-q4-r6.json). The embed MiniLM row was moved to
// unpricedThroughputUntilBound: its receipts measure embeddings/completed_embedding_records
// while embed settlement is token_like_input_units/token_like_input_geometry, and
// no frozen conversion authority exists.
//
// Media rows remain unpriced (ffmpeg names a non-git merc_source_commit;
// rendering has no source commit). See unpricedThroughputUntilBound.

// sealedCandleMetalLlama1InferBuildHash is the r6 running-executable identity
// of the only current-bindable production infer cell. The staging canary
// allowlist must name this hash. The superseded r5 measurement hash
// f4303a751ca2b2af is not it, and neither is a later host binary that no
// longer matches the sealed receipt.
const sealedCandleMetalLlama1InferBuildHash = "f4210c0ef62e4490"

var repricingBenchmarks = []measuredThroughput{
	{
		ModelID:                   "llama-3.2-1b-instruct-q4",
		ModelArtifactDigest:       "3f5a22426976ab26cfe84dba63c1d08391717abb1af893e10f1b2968d862dcc1",
		JobType:                   "batch_infer",
		RuntimeCellID:             "candle-metal-llama1-infer",
		RuntimeProfileID:          "candle_metal",
		ProfileRevision:           "r9",
		Engine:                    "candle",
		EngineBuildHash:           sealedCandleMetalLlama1InferBuildHash,
		EngineBuildIdentityPolicy: "merc_agent_running_executable_sha256_v1",
		HardwareIdentity:          "apple_silicon_v1|brand=Apple M3 Ultra|model=Mac15,14|memory_bytes=103079215104|cpu_cores=28|gpu_cores=60",
		Unit:                      "tokens",
		UnitScope:                 performanceUnitScopeTokenLikeInputPlusOutputTokens,
		// Conservative bound: equals measured operating-batch rate (141.1353).
		// Gate requires constant <= measured and not more than 1% below.
		//
		// This replaced r6's 304.2661. r6 was a 2.2x outlier: eight-plus
		// independent Apple Silicon measurements in-tree cluster at 137-143 tok/s
		// (138.7 alone appears eight times), and r7's own batch_32 rate is
		// 314.5469 — so r6 almost certainly measured a larger batch and recorded
		// it as the operating-batch rate. r6 therefore underpriced this cell by
		// ~2.2x. r7 (141.1353, operating batch 1, thermal_ok) is the honest rate
		// and its engine_build_hash is the one a live agent actually presents.
		UnitsPerSec:    141.1353,
		HWClass:        "apple_silicon_ultra",
		SourceCitation: "evidence/perf/runtime-benchmarks/candle-metal-llama1-q4-r7.json#batch_infer",
	},
}

// unpricedThroughputUntilBound holds measured (model, job type) pairs that still
// have throughput receipts in-tree but must not set a buyer price until the
// cited artifact binds (or until a settlement-geometry match exists). They are
// deliberately not in repricingBenchmarks so BuildCataloguePriceSchedule cannot
// publish them. A pair may appear in exactly one of the two lists.
//
// Media citations remain unbound/unbindable. The MiniLM embed row is parked
// because no receipt measures token_like_input_units/token_like_input_geometry
// for that cell yet (embeddings/s cannot price that unit without a fabricated
// conversion).
var unpricedThroughputUntilBound = []measuredThroughput{
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
		// Parked: measured embeddings/completed_embedding_records; embed settlement
		// is token_like_input_units/token_like_input_geometry. No token-geometry
		// embed receipt exists yet; no frozen conversion may invent one.
		// unbound historical diagnostic; not catalogue price authority
		SourceCitation: "evidence/benchmarks/2026-07-01-m3-pro.json#embed",
	},
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

// wattAuthorityKind is the source class of a sustained-power figure.
//
// Two authorities consume this table and must not be collapsed:
//
//   - ECONOMIC_POWER_ENVELOPE: supplier-floor / admission economics and the
//     catalogue boot gate. Must never materially understate electricity cost.
//     MEASURED and a conservative VENDOR_WALL_UPPER_BOUND are both allowed.
//   - ENERGY_MEASUREMENT: joules / verified-outcome-per-joule science. Requires
//     genuine complete local or attested MEASURED energy. VENDOR_WALL_UPPER_BOUND
//     does not satisfy it. ASSUMED does not satisfy it.
//
// An assumption honestly labelled is acceptable as a diagnostic; an assumption
// presented as physics is not.
type wattAuthorityKind string

const (
	wattKindMeasured             wattAuthorityKind = "MEASURED"
	wattKindAssumed              wattAuthorityKind = "ASSUMED"
	wattKindVendorWallUpperBound wattAuthorityKind = "VENDOR_WALL_UPPER_BOUND"
)

// Economic / energy authority names. These are not wattAuthorityKind values:
// they name which question a caller is asking of the same table row.
const (
	economicPowerEnvelopeAuthority = "ECONOMIC_POWER_ENVELOPE"
	energyMeasurementAuthority     = "ENERGY_MEASUREMENT"
	measuredEnergyEvidenceKind     = "MEASURED_ENERGY"
)

// Apple Mac Studio (2025) M3 Ultra wall-power figures, verified 2026-08-14
// against the live Apple pages. Live pages matched the expected numbers.
const (
	appleMacStudio2025M3UltraIdleWatts    = 9
	appleMacStudio2025M3UltraWallMaxWatts = 270
	appleMacStudio2025PSUCeilingWatts     = 480
	appleMacStudio2025PowerSupportURL     = "https://support.apple.com/en-us/102027"
	appleMacStudio2025SpecsURL            = "https://www.apple.com/mac-studio/specs/"
	appleMacStudio2025PowerPublishedDate  = "2025-03-12"
	appleMacStudio2025MeasuredConfig      = "32CPU/80GPU"
	appleMacStudio2025LocalConfig         = "28CPU/60GPU"
	appleVendorName                       = "apple"
	appleMacStudio2025ProductFamily       = "mac_studio_2025"
	appleM3UltraSOCFamily                 = "m3_ultra"
	acWallMeasurementScope                = "AC_WALL"
	localFailureCPUPowerSensorZero        = "cpu_power_sensor_zero"
	localFailureANEPowerSensorZero        = "ane_power_sensor_zero"
	catalogueVendorWallFreshnessPolicy    = "vendor-wall-upper-bound-v1/pinned-citation-digest"
	catalogueVendorWallAuthorityScope     = "vendor_family_chassis_conservative_upper_bound"
	catalogueVendorWallAggregation        = "vendor_published_max"
	catalogueVendorWallOperatingProtocol  = "apple-support-102027-wall-max"
	catalogueVendorWallWorkloadClass      = "not_workload_specific"
	catalogueVendorWallPublishedAt        = "2025-03-12T00:00:00Z"
)

// appleMacStudio2025WallPowerCitation is the pinned authority text whose
// sha256 is appleMacStudio2025WallPowerCitationDigest. It is the fetched
// Apple support/specs figures plus their URLs, not a local measurement.
const appleMacStudio2025WallPowerCitation = `APPLE MAC STUDIO (2025) M3 ULTRA WALL-POWER AUTHORITY
URL: https://support.apple.com/en-us/102027
PUBLISHED: 2025-03-12

Mac Studio (2025) M3 Ultra 32-Core CPU & 80-Core GPU, 512GB unified memory, 16TB SSD
Idle: 9 W
Max: 270 W

Notes:
1. Power consumption data (Watts) is measured from the wall power source and includes all power supply and system losses. Additional correction is not needed.
2. "Max" is defined as running a compute-intensive test application that maximizes processor usage and therefore power consumption. No external peripherals are attached during testing.
3. "Idle" reflects the power used with only Finder open, using the default power management settings.
4. These numbers reflect a 20.2°C (68.4° F) ambient running environment. Increased ambient temperatures require faster fan speeds which increases power consumption.

PSU CEILING (last resort; not the economic envelope):
URL: https://www.apple.com/mac-studio/specs/
Maximum continuous power: 480W
Line voltage: 100-240V AC
`

// appleMacStudio2025WallPowerCitationDigest is sha256 of
// appleMacStudio2025WallPowerCitation (UTF-8). Init verifies the pin.
const appleMacStudio2025WallPowerCitationDigest = "45b1b48412e8e24a41acf597e63ffbe66f371d3b4893231854c70eba9d6aa492"

// vendorWallProvenance is the typed source record for a VENDOR_WALL_UPPER_BOUND
// row. It is never a MEASURED receipt and is never stored as measured_watts.
type vendorWallProvenance struct {
	vendor                    string
	productFamily             string
	socFamily                 string
	wattsUpperBound           float64
	measurementScope          string
	includesPSULosses         bool
	workloadSpecific          bool
	localMeasurementAvailable bool
	localFailureReason        []string
	measuredConfig            string
	localConfig               string
	citationURL               string
	citationDigest            string
}

// vendorWallUpperBoundSpec is the required constructor input for
// wattsVendorWallUpperBound. Every field must be populated with the
// conservative-safe values; zero values are refused.
type vendorWallUpperBoundSpec struct {
	WattsUpperBound           float64
	Vendor                    string
	ProductFamily             string
	SOCFamily                 string
	MeasurementScope          string
	IncludesPSULosses         bool
	WorkloadSpecific          bool
	LocalMeasurementAvailable bool
	LocalFailureReason        []string
	MeasuredConfig            string
	LocalConfig               string
	CitationURL               string
	CitationDigest            string
}

// governedSustainedWatts is one hardware class's sustained whole-package draw
// under inference-shaped load. Fields are unexported so a bare float cannot be
// inserted into the table: construction goes through wattsMeasured /
// wattsAssumed / wattsVendorWallUpperBound, all of which require non-empty
// provenance. Startup rejects any entry that somehow lacks kind or provenance.
//
// There is no measured_watts field. MEASURED rows use watts + kind=MEASURED.
// VENDOR_WALL_UPPER_BOUND rows use watts as the conservative upper bound and
// kind=VENDOR_WALL_UPPER_BOUND.
type governedSustainedWatts struct {
	watts         float64
	kind          wattAuthorityKind
	provenance    string
	receiptSHA256 string
	vendorWall    *vendorWallProvenance
}

// Watts is the sustained draw in watts used by contribution margins and the
// diagnostic cost floor. For VENDOR_WALL_UPPER_BOUND this is the conservative
// upper bound, never a local measurement.
func (g governedSustainedWatts) Watts() float64 { return g.watts }

// Kind is MEASURED, ASSUMED, or VENDOR_WALL_UPPER_BOUND.
func (g governedSustainedWatts) Kind() wattAuthorityKind { return g.kind }

// Provenance names the receipt (MEASURED), who assumed the figure and why
// (ASSUMED), or the vendor-wall citation (VENDOR_WALL_UPPER_BOUND). Required
// for every entry.
func (g governedSustainedWatts) Provenance() string { return g.provenance }

// ReceiptSHA256 pins the exact cited receipt bytes for MEASURED publication,
// or the vendor citation digest for VENDOR_WALL_UPPER_BOUND. ASSUMED
// diagnostic rows intentionally carry no digest.
func (g governedSustainedWatts) ReceiptSHA256() string { return g.receiptSHA256 }

// VendorWall returns a copy of the vendor-wall provenance, or nil.
func (g governedSustainedWatts) VendorWall() *vendorWallProvenance {
	if g.vendorWall == nil {
		return nil
	}
	cp := *g.vendorWall
	if len(g.vendorWall.localFailureReason) > 0 {
		cp.localFailureReason = append([]string(nil), g.vendorWall.localFailureReason...)
	}
	return &cp
}

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

// wattsVendorWallUpperBound constructs a conservative vendor whole-wall upper
// bound. Every provenance field is required. The result is never MEASURED and
// never stored as measured_watts. Panics on missing or inconsistent fields so
// an incomplete bound cannot enter the table.
func wattsVendorWallUpperBound(spec vendorWallUpperBoundSpec) governedSustainedWatts {
	if err := validateVendorWallUpperBoundSpec(spec); err != nil {
		panic("wattsVendorWallUpperBound: " + err.Error())
	}
	reasons := append([]string(nil), spec.LocalFailureReason...)
	return governedSustainedWatts{
		watts:         spec.WattsUpperBound,
		kind:          wattKindVendorWallUpperBound,
		provenance:    vendorWallProvenanceLine(spec),
		receiptSHA256: strings.TrimSpace(spec.CitationDigest),
		vendorWall: &vendorWallProvenance{
			vendor:                    strings.TrimSpace(spec.Vendor),
			productFamily:             strings.TrimSpace(spec.ProductFamily),
			socFamily:                 strings.TrimSpace(spec.SOCFamily),
			wattsUpperBound:           spec.WattsUpperBound,
			measurementScope:          strings.TrimSpace(spec.MeasurementScope),
			includesPSULosses:         spec.IncludesPSULosses,
			workloadSpecific:          spec.WorkloadSpecific,
			localMeasurementAvailable: spec.LocalMeasurementAvailable,
			localFailureReason:        reasons,
			measuredConfig:            strings.TrimSpace(spec.MeasuredConfig),
			localConfig:               strings.TrimSpace(spec.LocalConfig),
			citationURL:               strings.TrimSpace(spec.CitationURL),
			citationDigest:            strings.TrimSpace(spec.CitationDigest),
		},
	}
}

func vendorWallProvenanceLine(spec vendorWallUpperBoundSpec) string {
	return fmt.Sprintf(
		"VENDOR_WALL_UPPER_BOUND vendor=%s product_family=%s soc_family=%s watts_upper_bound=%.0f measurement_scope=%s includes_psu_losses=%t workload_specific=%t measured_config=%s local_config=%s citation=%s digest=%s (conservative family/chassis upper bound; not this config's measurement)",
		strings.TrimSpace(spec.Vendor), strings.TrimSpace(spec.ProductFamily),
		strings.TrimSpace(spec.SOCFamily), spec.WattsUpperBound,
		strings.TrimSpace(spec.MeasurementScope), spec.IncludesPSULosses, spec.WorkloadSpecific,
		strings.TrimSpace(spec.MeasuredConfig), strings.TrimSpace(spec.LocalConfig),
		strings.TrimSpace(spec.CitationURL), strings.TrimSpace(spec.CitationDigest),
	)
}

func validateVendorWallUpperBoundSpec(spec vendorWallUpperBoundSpec) error {
	if spec.WattsUpperBound <= 0 {
		return fmt.Errorf("watts_upper_bound must be positive, got %v", spec.WattsUpperBound)
	}
	if strings.TrimSpace(spec.Vendor) == "" {
		return fmt.Errorf("vendor is required")
	}
	if strings.TrimSpace(spec.ProductFamily) == "" {
		return fmt.Errorf("product_family is required")
	}
	if strings.TrimSpace(spec.SOCFamily) == "" {
		return fmt.Errorf("soc_family is required")
	}
	if strings.TrimSpace(spec.MeasurementScope) == "" {
		return fmt.Errorf("measurement_scope is required")
	}
	if strings.TrimSpace(spec.MeasuredConfig) == "" {
		return fmt.Errorf("measured_config is required")
	}
	if strings.TrimSpace(spec.LocalConfig) == "" {
		return fmt.Errorf("local_config is required")
	}
	if strings.TrimSpace(spec.CitationURL) == "" {
		return fmt.Errorf("citation URL is required")
	}
	if !digestPattern.MatchString(strings.TrimSpace(spec.CitationDigest)) {
		return fmt.Errorf("citation_digest must be a lowercase sha256")
	}
	if len(spec.LocalFailureReason) == 0 {
		return fmt.Errorf("local_failure_reason is required when local measurement is unavailable")
	}
	for i, reason := range spec.LocalFailureReason {
		if strings.TrimSpace(reason) == "" {
			return fmt.Errorf("local_failure_reason[%d] is empty", i)
		}
	}
	// Bool fields cannot be "missing" in Go; the conservative-safe values are
	// part of the contract for this source class.
	if !spec.IncludesPSULosses {
		return fmt.Errorf("includes_psu_losses must be true for an AC_WALL vendor bound")
	}
	if spec.WorkloadSpecific {
		return fmt.Errorf("workload_specific must be false for a family/chassis vendor bound")
	}
	if spec.LocalMeasurementAvailable {
		return fmt.Errorf("local_measurement_available must be false when recording a vendor-wall fallback")
	}
	if strings.TrimSpace(spec.MeasurementScope) != acWallMeasurementScope {
		return fmt.Errorf("measurement_scope must be %s, got %q", acWallMeasurementScope, spec.MeasurementScope)
	}
	wantDigest := fmt.Sprintf("%x", sha256.Sum256([]byte(appleMacStudio2025WallPowerCitation)))
	if strings.TrimSpace(spec.CitationDigest) != wantDigest {
		return fmt.Errorf("citation_digest does not match pinned Apple authority text: got %s want %s",
			spec.CitationDigest, wantDigest)
	}
	if wantDigest != appleMacStudio2025WallPowerCitationDigest {
		return fmt.Errorf("pinned Apple citation digest constant drifted from hashed authority text")
	}
	return nil
}

func appleSiliconUltraVendorWallSpec() vendorWallUpperBoundSpec {
	return vendorWallUpperBoundSpec{
		WattsUpperBound:           appleMacStudio2025M3UltraWallMaxWatts,
		Vendor:                    appleVendorName,
		ProductFamily:             appleMacStudio2025ProductFamily,
		SOCFamily:                 appleM3UltraSOCFamily,
		MeasurementScope:          acWallMeasurementScope,
		IncludesPSULosses:         true,
		WorkloadSpecific:          false,
		LocalMeasurementAvailable: false,
		LocalFailureReason:        []string{localFailureCPUPowerSensorZero, localFailureANEPowerSensorZero},
		MeasuredConfig:            appleMacStudio2025MeasuredConfig,
		LocalConfig:               appleMacStudio2025LocalConfig,
		CitationURL:               appleMacStudio2025PowerSupportURL,
		CitationDigest:            appleMacStudio2025WallPowerCitationDigest,
	}
}

// sustainedWattsByHWClass is the closed power table for contribution margins
// and supplier-viability arithmetic. Every admitted hardware class must appear
// here with MEASURED, ASSUMED, or VENDOR_WALL_UPPER_BOUND provenance;
// validateSustainedWattsTable panics at init if any entry is incomplete or any
// admitted class is missing.
//
// apple_silicon_ultra is a CONSERVATIVE FAMILY/CHASSIS VENDOR_WALL_UPPER_BOUND
// of 270 W: Apple measured Mac Studio (2025) M3 Ultra 32-core CPU / 80-core GPU
// at the wall (including PSU/system losses). This host is 28-core CPU / 60-core
// GPU, so 270 W is an upper bound (28/60 draws ≤ 32/80), not this config's
// measurement. Local package telemetry is incomplete (CPU and ANE sensors read
// zero; only GPU moves) and is refused, not promoted to MEASURED.
//
// CUDA figures remain board-power order-of-magnitude ASSUMED values — there is
// no NVIDIA device on this host to measure. An Apple vendor bound cannot cover
// a CUDA class.
var sustainedWattsByHWClass = map[string]governedSustainedWatts{
	"apple_silicon_base": wattsAssumed(20.0,
		"ASSUMED by control/pricing.go: whole-package sustained draw for apple_silicon_base under inference-shaped load; no bound host power receipt"),
	"apple_silicon_pro": wattsAssumed(30.0,
		"ASSUMED by control/pricing.go: whole-package sustained draw for apple_silicon_pro under inference-shaped load; no bound host power receipt"),
	"apple_silicon_max": wattsAssumed(45.0,
		"ASSUMED by control/pricing.go: whole-package sustained draw for apple_silicon_max under inference-shaped load; no bound host power receipt"),
	"apple_silicon_ultra": wattsVendorWallUpperBound(appleSiliconUltraVendorWallSpec()),
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
	wantDigest := fmt.Sprintf("%x", sha256.Sum256([]byte(appleMacStudio2025WallPowerCitation)))
	if wantDigest != appleMacStudio2025WallPowerCitationDigest {
		return fmt.Errorf("apple Mac Studio wall-power citation digest drifted: hashed=%s pinned=%s",
			wantDigest, appleMacStudio2025WallPowerCitationDigest)
	}
	for class, entry := range sustainedWattsByHWClass {
		if entry.Watts() <= 0 {
			return fmt.Errorf("sustainedWattsByHWClass[%q]: watts must be positive, got %v", class, entry.Watts())
		}
		switch entry.Kind() {
		case wattKindMeasured, wattKindAssumed, wattKindVendorWallUpperBound:
		default:
			return fmt.Errorf("sustainedWattsByHWClass[%q]: Kind must be MEASURED, ASSUMED, or VENDOR_WALL_UPPER_BOUND, got %q",
				class, entry.Kind())
		}
		if strings.TrimSpace(entry.Provenance()) == "" {
			return fmt.Errorf("sustainedWattsByHWClass[%q]: Provenance is required (uncited watt constants are not production truth)",
				class)
		}
		if entry.Kind() == wattKindVendorWallUpperBound {
			if err := validateVendorWallTableEntry(class, entry); err != nil {
				return fmt.Errorf("sustainedWattsByHWClass[%q]: %w", class, err)
			}
		}
		if entry.Kind() == wattKindMeasured && entry.vendorWall != nil {
			return fmt.Errorf("sustainedWattsByHWClass[%q]: MEASURED row must not carry vendor-wall provenance", class)
		}
	}
	for class := range validHWClasses {
		if _, ok := sustainedWattsByHWClass[class]; !ok {
			return fmt.Errorf("admitted hardware class %q has no sustainedWattsByHWClass entry", class)
		}
	}
	return nil
}

func validateVendorWallTableEntry(hwClass string, entry governedSustainedWatts) error {
	if entry.Kind() != wattKindVendorWallUpperBound {
		return fmt.Errorf("kind is %s, not VENDOR_WALL_UPPER_BOUND", entry.Kind())
	}
	if entry.vendorWall == nil {
		return fmt.Errorf("VENDOR_WALL_UPPER_BOUND requires typed provenance fields")
	}
	p := entry.vendorWall
	spec := vendorWallUpperBoundSpec{
		WattsUpperBound:           p.wattsUpperBound,
		Vendor:                    p.vendor,
		ProductFamily:             p.productFamily,
		SOCFamily:                 p.socFamily,
		MeasurementScope:          p.measurementScope,
		IncludesPSULosses:         p.includesPSULosses,
		WorkloadSpecific:          p.workloadSpecific,
		LocalMeasurementAvailable: p.localMeasurementAvailable,
		LocalFailureReason:        p.localFailureReason,
		MeasuredConfig:            p.measuredConfig,
		LocalConfig:               p.localConfig,
		CitationURL:               p.citationURL,
		CitationDigest:            p.citationDigest,
	}
	if err := validateVendorWallUpperBoundSpec(spec); err != nil {
		return err
	}
	if entry.Watts() != p.wattsUpperBound {
		return fmt.Errorf("watts %v != watts_upper_bound %v (VENDOR_WALL must not be stored as a separate measured_watts)",
			entry.Watts(), p.wattsUpperBound)
	}
	if strings.TrimSpace(entry.ReceiptSHA256()) != strings.TrimSpace(p.citationDigest) {
		return fmt.Errorf("receipt/citation digest mismatch")
	}
	if err := vendorWallCoversHardware(p, hwClass, ""); err != nil {
		return err
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

// sustainedWattsForPublication returns the ECONOMIC_POWER_ENVELOPE for one
// benchmark hardware class. MEASURED whole-package receipts and a conservative
// VENDOR_WALL_UPPER_BOUND are both acceptable. ASSUMED rows remain diagnostic
// only. Unknown classes are refused rather than inheriting another class's
// default.
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
	if err := acceptEconomicPowerEnvelope(entry, hwClass, ""); err != nil {
		return governedSustainedWatts{}, err
	}
	return entry, nil
}

// acceptEconomicPowerEnvelope is the ECONOMIC_POWER_ENVELOPE gate: MEASURED or
// VENDOR_WALL_UPPER_BOUND. ASSUMED, GPU-only, and incomplete rows are refused.
func acceptEconomicPowerEnvelope(entry governedSustainedWatts, hwClass, hardwareIdentity string) error {
	switch entry.Kind() {
	case wattKindMeasured:
		if entry.vendorWall != nil {
			return fmt.Errorf("ECONOMIC_POWER_ENVELOPE refuses a MEASURED row that also carries vendor-wall provenance")
		}
		return nil
	case wattKindVendorWallUpperBound:
		if err := validateVendorWallTableEntry(hwClass, entry); err != nil {
			return fmt.Errorf("catalogue publication VENDOR_WALL_UPPER_BOUND for hardware class %q: %w", hwClass, err)
		}
		if err := vendorWallCoversHardware(entry.vendorWall, hwClass, hardwareIdentity); err != nil {
			return err
		}
		if vendorWallFamilyIsAppleStudioM3Ultra(entry.vendorWall) &&
			entry.Watts() != appleMacStudio2025M3UltraWallMaxWatts {
			return fmt.Errorf(
				"480W PSU ceiling must not replace applicable 270W VENDOR_WALL_UPPER_BOUND (got %.0fW)",
				entry.Watts())
		}
		return nil
	case wattKindAssumed:
		return fmt.Errorf(
			"catalogue publication requires MEASURED or VENDOR_WALL_UPPER_BOUND sustained watts for hardware class %q; got %s",
			hwClass, entry.Kind())
	default:
		return fmt.Errorf(
			"catalogue publication requires MEASURED or VENDOR_WALL_UPPER_BOUND sustained watts for hardware class %q; got %s",
			hwClass, entry.Kind())
	}
}

// acceptEnergyMeasurement is the ENERGY_MEASUREMENT / MEASURED_ENERGY gate.
// Only genuine complete MEASURED energy satisfies joules science.
// VENDOR_WALL_UPPER_BOUND, ASSUMED, and GPU-only telemetry are refused.
func acceptEnergyMeasurement(entry governedSustainedWatts) error {
	if entry.Kind() != wattKindMeasured || entry.vendorWall != nil {
		return fmt.Errorf(
			"%s / %s requires MEASURED energy; %s does not satisfy it",
			energyMeasurementAuthority, measuredEnergyEvidenceKind, entry.Kind())
	}
	return nil
}

func vendorWallCoversHardware(p *vendorWallProvenance, hwClass, hardwareIdentity string) error {
	if p == nil {
		return fmt.Errorf("VENDOR_WALL_UPPER_BOUND provenance is missing")
	}
	hwClass = strings.TrimSpace(hwClass)
	if strings.HasPrefix(hwClass, "nvidia_") || hwClass == "cpu" {
		return fmt.Errorf(
			"VENDOR_WALL_UPPER_BOUND vendor=%s product_family=%s soc_family=%s cannot cover hardware class %q",
			p.vendor, p.productFamily, p.socFamily, hwClass)
	}
	id := strings.ToLower(hardwareIdentity)
	if strings.Contains(id, "nvidia") || strings.Contains(id, "cuda") {
		return fmt.Errorf(
			"VENDOR_WALL_UPPER_BOUND vendor=%s soc_family=%s cannot cover hardware identity %q",
			p.vendor, p.socFamily, hardwareIdentity)
	}
	if p.vendor != appleVendorName ||
		p.productFamily != appleMacStudio2025ProductFamily ||
		p.socFamily != appleM3UltraSOCFamily {
		return fmt.Errorf(
			"VENDOR_WALL_UPPER_BOUND vendor=%s product_family=%s soc_family=%s is not the Apple Mac Studio 2025 M3 Ultra bound",
			p.vendor, p.productFamily, p.socFamily)
	}
	if hwClass != "" && hwClass != "apple_silicon_ultra" {
		return fmt.Errorf(
			"VENDOR_WALL_UPPER_BOUND vendor=apple product_family=mac_studio_2025 soc_family=m3_ultra cannot cover hardware class %q",
			hwClass)
	}
	if hardwareIdentity != "" &&
		!strings.Contains(hardwareIdentity, "M3 Ultra") &&
		!strings.Contains(hardwareIdentity, "Mac15,14") &&
		!strings.Contains(hardwareIdentity, "apple_silicon_ultra") {
		return fmt.Errorf(
			"VENDOR_WALL_UPPER_BOUND soc_family=m3_ultra does not match hardware identity %q",
			hardwareIdentity)
	}
	return nil
}

func vendorWallFamilyIsAppleStudioM3Ultra(p *vendorWallProvenance) bool {
	if p == nil {
		return false
	}
	return p.vendor == appleVendorName &&
		p.productFamily == appleMacStudio2025ProductFamily &&
		p.socFamily == appleM3UltraSOCFamily
}

func vendorWall270Applicable(p *vendorWallProvenance, hwClass string) bool {
	return vendorWallFamilyIsAppleStudioM3Ultra(p) &&
		p.wattsUpperBound == appleMacStudio2025M3UltraWallMaxWatts &&
		(hwClass == "" || hwClass == "apple_silicon_ultra")
}

// economicPowerSourceRank is the fallback hierarchy. Higher values supersede.
// GPU-only is invalid and must never be selected.
type economicPowerSourceRank int

const (
	economicPowerRankInvalid                      economicPowerSourceRank = 0
	economicPowerRankGPUOnly                      economicPowerSourceRank = 1 // INVALID
	economicPowerRankPSUCeiling                   economicPowerSourceRank = 2 // LAST RESORT 480W
	economicPowerRankVendorWallUpperBound         economicPowerSourceRank = 3 // SAFE FALLBACK 270W
	economicPowerRankMatchedExternalWallMeasured  economicPowerSourceRank = 4
	economicPowerRankCompleteLocalPackageMeasured economicPowerSourceRank = 5
	economicPowerRankLocalWallMeasured            economicPowerSourceRank = 6 // BEST
)

// economicPowerCandidate is one candidate for selectEconomicPowerEnvelope.
type economicPowerCandidate struct {
	Name       string
	Rank       economicPowerSourceRank
	Watts      float64
	Applicable bool
	Entry      governedSustainedWatts
}

// selectEconomicPowerEnvelope picks the highest-quality applicable source.
// GPU-only is never selected. The 480W PSU ceiling is used only when the 270W
// vendor-wall bound is inapplicable.
func selectEconomicPowerEnvelope(candidates []economicPowerCandidate) (economicPowerCandidate, error) {
	var best economicPowerCandidate
	bestRank := economicPowerRankInvalid
	vendorWallApplicable := false
	for _, c := range candidates {
		if !c.Applicable {
			continue
		}
		if c.Rank == economicPowerRankGPUOnly || c.Rank == economicPowerRankInvalid {
			continue
		}
		if c.Rank == economicPowerRankVendorWallUpperBound &&
			c.Watts == appleMacStudio2025M3UltraWallMaxWatts {
			vendorWallApplicable = true
		}
		if c.Rank > bestRank {
			best = c
			bestRank = c.Rank
		}
	}
	if bestRank == economicPowerRankInvalid {
		return economicPowerCandidate{}, fmt.Errorf("no applicable economic power envelope (GPU-only is invalid)")
	}
	if best.Rank == economicPowerRankPSUCeiling && vendorWallApplicable {
		return economicPowerCandidate{}, fmt.Errorf(
			"480W PSU ceiling must not replace applicable 270W VENDOR_WALL_UPPER_BOUND")
	}
	if best.Watts == appleMacStudio2025PSUCeilingWatts && vendorWallApplicable {
		return economicPowerCandidate{}, fmt.Errorf(
			"480W PSU ceiling must not replace applicable 270W VENDOR_WALL_UPPER_BOUND")
	}
	return best, nil
}

// localPackagePowerTelemetry is a local package-power probe result. GPU-only
// (CPU and/or ANE sensors at zero while GPU moves) is incomplete and refused.
type localPackagePowerTelemetry struct {
	CPUWatts float64
	ANEWatts float64
	GPUWatts float64
}

// classifyLocalPackagePowerTelemetry refuses GPU-only / incomplete local
// package telemetry. This is the Go-side counterpart of the seal script's
// CPU-package refusal; it is not weakened.
func classifyLocalPackagePowerTelemetry(t localPackagePowerTelemetry) error {
	if t.GPUWatts > 0 && t.CPUWatts <= 0 {
		return fmt.Errorf(
			"CPU package component is %v; refusing GPU-only envelope (ANE=%v GPU=%v); local_failure_reason=[%s, %s]",
			t.CPUWatts, t.ANEWatts, t.GPUWatts, localFailureCPUPowerSensorZero, localFailureANEPowerSensorZero)
	}
	if t.CPUWatts <= 0 && t.ANEWatts <= 0 {
		return fmt.Errorf(
			"incomplete local package telemetry: %s, %s; refusing GPU-only / incomplete envelope",
			localFailureCPUPowerSensorZero, localFailureANEPowerSensorZero)
	}
	return nil
}

func localFailureReasonsFromTelemetry(t localPackagePowerTelemetry) []string {
	var reasons []string
	if t.CPUWatts <= 0 {
		reasons = append(reasons, localFailureCPUPowerSensorZero)
	}
	if t.ANEWatts <= 0 {
		reasons = append(reasons, localFailureANEPowerSensorZero)
	}
	return reasons
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
// inference-shaped watts measurement or a conservative vendor-wall upper bound.
// GPU-domain or idle measurements are not interchangeable with this
// supply-side cost boundary. SourceClass distinguishes MEASURED receipts from
// VENDOR_WALL_UPPER_BOUND; the latter is never stored as MEASURED.
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
	// Vendor-wall provenance. Omitempty keeps MEASURED snapshots compact.
	// There is no measured_watts field.
	SourceClass               string   `json:"source_class,omitempty"`
	Vendor                    string   `json:"vendor,omitempty"`
	ProductFamily             string   `json:"product_family,omitempty"`
	SOCFamily                 string   `json:"soc_family,omitempty"`
	WattsUpperBound           float64  `json:"watts_upper_bound,omitempty"`
	MeasurementScope          string   `json:"measurement_scope,omitempty"`
	IncludesPSULosses         *bool    `json:"includes_psu_losses,omitempty"`
	WorkloadSpecific          *bool    `json:"workload_specific,omitempty"`
	LocalMeasurementAvailable *bool    `json:"local_measurement_available,omitempty"`
	LocalFailureReason        []string `json:"local_failure_reason,omitempty"`
	MeasuredConfig            string   `json:"measured_config,omitempty"`
	LocalConfig               string   `json:"local_config,omitempty"`
	CitationDigest            string   `json:"citation_digest,omitempty"`
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
	if power.SourceClass == string(wattKindVendorWallUpperBound) {
		if err := validateVendorWallCataloguePower(result, physical, power); err != nil {
			return time.Time{}, err
		}
		// Vendor-wall citations are pinned by digest, not a 30-day measurement
		// clock. Physical current-use is gated by throughput freshness only.
		physicalUntil, err := canonicalCatalogueTimestamp("physical authority valid_until", physical.ValidUntil)
		if err != nil {
			return time.Time{}, err
		}
		if !physicalUntil.Equal(throughputUntil) {
			return time.Time{}, fmt.Errorf(
				"catalogue result %s/%s physical valid_until is not the throughput boundary under VENDOR_WALL_UPPER_BOUND",
				result.ModelID, result.JobType)
		}
		return physicalUntil, nil
	}
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

func validateVendorWallCataloguePower(
	result RepriceResult,
	physical CatalogueResultPhysicalAuthority,
	power CataloguePowerAuthoritySnapshot,
) error {
	if power.SourceClass != string(wattKindVendorWallUpperBound) {
		return fmt.Errorf("catalogue vendor-wall power for %s/%s has source_class %q",
			result.ModelID, result.JobType, power.SourceClass)
	}
	if strings.EqualFold(power.SourceClass, string(wattKindMeasured)) {
		return fmt.Errorf("VENDOR_WALL_UPPER_BOUND must never be stored as MEASURED")
	}
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
			return fmt.Errorf(
				"catalogue result %s/%s vendor-wall power authority does not bind exact runtime/build/device identity",
				result.ModelID, result.JobType)
		}
	}
	trueVal, falseVal := true, false
	if strings.TrimSpace(power.Citation) == "" ||
		!digestPattern.MatchString(power.ReceiptSHA256) ||
		power.FreshnessPolicy != catalogueVendorWallFreshnessPolicy ||
		power.MeasurementBoundary != acWallMeasurementScope ||
		power.MeasurementScope != acWallMeasurementScope ||
		power.WorkloadClass != catalogueVendorWallWorkloadClass ||
		power.Unit != "watts" ||
		power.AuthorityScope != catalogueVendorWallAuthorityScope ||
		power.Aggregation != catalogueVendorWallAggregation ||
		power.OperatingProtocol != catalogueVendorWallOperatingProtocol ||
		len(power.CoveredWorkloads) != 0 ||
		!finiteNonNegative(power.Watts) || power.Watts <= 0 ||
		power.Watts != appleMacStudio2025M3UltraWallMaxWatts ||
		power.WattsUpperBound != appleMacStudio2025M3UltraWallMaxWatts ||
		power.Watts == appleMacStudio2025PSUCeilingWatts ||
		strings.TrimSpace(power.Vendor) == "" ||
		strings.TrimSpace(power.ProductFamily) == "" ||
		strings.TrimSpace(power.SOCFamily) == "" ||
		strings.TrimSpace(power.MeasuredConfig) == "" ||
		strings.TrimSpace(power.LocalConfig) == "" ||
		!digestPattern.MatchString(power.CitationDigest) ||
		power.CitationDigest != power.ReceiptSHA256 ||
		power.IncludesPSULosses == nil || *power.IncludesPSULosses != trueVal ||
		power.WorkloadSpecific == nil || *power.WorkloadSpecific != falseVal ||
		power.LocalMeasurementAvailable == nil || *power.LocalMeasurementAvailable != falseVal ||
		len(power.LocalFailureReason) == 0 {
		return fmt.Errorf(
			"catalogue result %s/%s lacks complete VENDOR_WALL_UPPER_BOUND provenance",
			result.ModelID, result.JobType)
	}
	wantDigest := fmt.Sprintf("%x", sha256.Sum256([]byte(appleMacStudio2025WallPowerCitation)))
	if power.CitationDigest != wantDigest || power.ReceiptSHA256 != wantDigest {
		return fmt.Errorf(
			"catalogue result %s/%s vendor-wall citation digest mismatch: pinned=%s want=%s",
			result.ModelID, result.JobType, power.CitationDigest, wantDigest)
	}
	p := &vendorWallProvenance{
		vendor:                    power.Vendor,
		productFamily:             power.ProductFamily,
		socFamily:                 power.SOCFamily,
		wattsUpperBound:           power.WattsUpperBound,
		measurementScope:          power.MeasurementScope,
		includesPSULosses:         *power.IncludesPSULosses,
		workloadSpecific:          *power.WorkloadSpecific,
		localMeasurementAvailable: *power.LocalMeasurementAvailable,
		localFailureReason:        power.LocalFailureReason,
		measuredConfig:            power.MeasuredConfig,
		localConfig:               power.LocalConfig,
		citationURL:               power.Citation,
		citationDigest:            power.CitationDigest,
	}
	if err := vendorWallCoversHardware(p, physical.HWClass, physical.HardwareIdentity); err != nil {
		return err
	}
	hasCPU, hasANE := false, false
	for _, reason := range power.LocalFailureReason {
		switch reason {
		case localFailureCPUPowerSensorZero:
			hasCPU = true
		case localFailureANEPowerSensorZero:
			hasANE = true
		}
	}
	if !hasCPU || !hasANE {
		return fmt.Errorf(
			"catalogue result %s/%s vendor-wall local_failure_reason must record cpu and ane sensor zeros",
			result.ModelID, result.JobType)
	}
	if power.MeasuredAt != catalogueVendorWallPublishedAt {
		return fmt.Errorf(
			"catalogue result %s/%s vendor-wall published-at must be Apple's %s, got %q",
			result.ModelID, result.JobType, catalogueVendorWallPublishedAt, power.MeasuredAt)
	}
	return nil
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
// exact governed board bytes. Unpriced measured models still block the entire
// schedule. A conservative VENDOR_WALL_UPPER_BOUND that makes a market price
// look underwater is a viability WARNING (see SupplierViabilityReport / main
// boot), not a publication veto — the bound errs high by design. MEASURED
// power still refuses a negative-contribution price. Each result receives its
// own reviewed physical-workload share; there is intentionally no process-wide
// take-rate input.
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
		if gerr := governPublishedPriceAtWattsKind(
			b, referencePrice, supplierShare, physical.Power.Watts,
			wattAuthorityKind(physical.Power.SourceClass),
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
// hardware the control plane currently admits (Apple Silicon) gross and
// electricity land within a rounding error of each other. The worked figures
// that used to sit here — 138.7 tok/s, $0.00436/hr against $0.0045/hr — came
// from evidence/benchmarks/2026-07-01-m3-pro.json and are close to what the
// catalogue now prices: the bound throughput is 141.1353 tok/s from the candle
// r7 receipt (r6's 304.2661 was a 2.2x outlier and was retired). The
// illustration is left unnumbered rather than restated, because
// a hardcoded number in a comment beside live pricing code is exactly the thing
// that goes stale without anyone noticing. A marketplace whose
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
