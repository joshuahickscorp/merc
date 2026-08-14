package main

import (
	"encoding/json"
	"fmt"
	"math"
	"os"
	"regexp"
	"slices"
	"strings"
	"testing"
	"time"
)

// benchmarkNow is an instant inside the revalidation window for every receipt in
// the tree, so "fresh" is a property of the fixture rather than of the day the
// suite happens to run.
//
// It is derived from the newest receipt rather than typed as a constant. A typed
// date silently stops meaning "inside the window for every receipt" the moment a
// receipt is re-measured later than it: the staleness tests then resolve against
// a receipt in their own future, never trip, and a degradation guard passes by
// doing nothing. That is exactly what happened when the embed cell's authority
// was re-sealed.
var benchmarkNow = newestBenchmarkMeasuredAt()

// newestBenchmarkMeasuredAt returns the latest MeasuredAt across every benchmark
// receipt the runtime authority resolves, so the fixture clock is at or after all
// of them by construction.
func newestBenchmarkMeasuredAt() time.Time {
	newest := time.Date(2026, 8, 1, 0, 0, 0, 0, time.UTC)
	for _, receipt := range benchmarkAuthorityManifest {
		at, err := time.Parse(time.RFC3339, receipt.MeasuredAt)
		if err != nil {
			continue
		}
		if at.After(newest) {
			newest = at
		}
	}
	return newest
}

func boardReferencePrice(t *testing.T, modelID, jobType string) float64 {
	t.Helper()
	board, err := loadPriceBoard()
	mustf(t, err, "load price board: %v")
	priced, ok := repriceFromMarketBoard(modelID, jobType, board)
	if !ok || priced.PricePer1K <= 0 {
		t.Fatalf("no market board price for %s/%s", modelID, jobType)
	}
	return priced.PricePer1K
}

func boardCatalogueAuthority(t *testing.T) func(string) (CataloguePriceAuthority, error) {
	t.Helper()
	return func(modelID string) (CataloguePriceAuthority, error) {
		for _, benchmark := range repricingBenchmarks {
			if benchmark.ModelID != modelID {
				continue
			}
			return CataloguePriceAuthority{
				ModelID:             modelID,
				JobType:             benchmark.JobType,
				ReferencePricePer1K: boardReferencePrice(t, modelID, benchmark.JobType),
				SupplierShare:       supplierShareForTest(t, benchmark.JobType, modelID),
			}, nil
		}
		return CataloguePriceAuthority{}, fmt.Errorf("no board authority for %s", modelID)
	}
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

// historicalFrozenPerformanceForTest encodes a snapshot as an older admission
// would have accepted it. Production creation must use freezeRuntimeCellPerformance,
// whose current-policy checks are intentionally stronger; historical replay
// validates only this self-contained block.
func historicalFrozenPerformanceForTest(
	t *testing.T, performance RuntimeCellPerformance,
) *FrozenRuntimeCellPerformance {
	t.Helper()
	summary, ok := benchmarkAuthorityManifest[performance.BenchmarkAuthority]
	if !ok {
		t.Fatalf("benchmark authority %q is absent", performance.BenchmarkAuthority)
	}
	summary = cloneBenchmarkReceiptSummary(summary)
	exactPins, err := exactWeightDigestsForCurrentPerformance(performance)
	must(t, err)
	summary = projectBenchmarkSummaryToExactCell(summary, exactPins)
	canonicalCommit, err := canonicalFrozenMercSourceCommit(summary.MercSourceCommit)
	must(t, err)
	summary.MercSourceCommit = canonicalCommit
	summarySHA, err := benchmarkReceiptSummarySHA256(summary)
	must(t, err)
	out := FrozenRuntimeCellPerformance{
		Version:                 frozenRuntimeCellPerformanceVersion,
		PolicyRevision:          runtimeCellPerformancePolicyRevision,
		Performance:             performance,
		BenchmarkSnapshot:       summary,
		BenchmarkSnapshotSHA256: summarySHA,
		ModelArtifactWireKind:   performance.WireKind,
		ModelArtifactPins:       append([]string(nil), exactPins...),
	}
	out.Digest, err = frozenRuntimeCellPerformanceDigest(out)
	must(t, err)
	mustf(t, validateFrozenRuntimeCellPerformance(&out),
		"historical frozen performance fixture was not self-contained: %v")
	return &out
}

func historicalLegacyFrozenPerformanceForTest(
	t *testing.T, performance RuntimeCellPerformance,
) *FrozenRuntimeCellPerformance {
	t.Helper()
	legacy := *historicalFrozenPerformanceForTest(t, performance)
	legacy.Version = frozenRuntimeCellPerformanceLegacyVersion
	legacy.PolicyRevision = runtimeCellPerformanceLegacyPolicyRevision
	legacy.Performance.EngineBuildHash = ""
	legacy.Performance.EngineBuildIdentityPolicy = ""
	legacy.Performance.HardwareIdentity = ""
	legacy.BenchmarkSnapshot = cloneBenchmarkReceiptSummary(legacy.BenchmarkSnapshot)
	legacy.BenchmarkSnapshot.EngineBuildHash = ""
	legacy.BenchmarkSnapshot.EngineBuildIdentityPolicy = ""
	legacy.BenchmarkSnapshot.HardwareIdentity = ""
	var err error
	legacy.BenchmarkSnapshotSHA256, err = benchmarkReceiptSummarySHA256(legacy.BenchmarkSnapshot)
	must(t, err)
	legacy.Digest, err = frozenRuntimeCellPerformanceDigest(legacy)
	must(t, err)
	mustf(t, validateFrozenRuntimeCellPerformance(&legacy),
		"legacy frozen performance fixture was not self-contained: %v")
	return &legacy
}

// historicalLegacyDecodeOnlyFrozenPerformanceForTest builds a self-contained
// legacy freeze under decode_output_tokens so historical-replay tests keep
// their "frozen decode receipt is not rewritten" guarantee after production
// r5 moved the live cell onto settlement geometry.
func historicalLegacyDecodeOnlyFrozenPerformanceForTest(
	t *testing.T, performance RuntimeCellPerformance,
) *FrozenRuntimeCellPerformance {
	t.Helper()
	decode := performance
	decode.UnitScope = performanceUnitScopeDecodeOutputTokens
	decode.Reason = "historical decode_output_tokens freeze; not current settlement authority"
	decode.EngineBuildHash = ""
	decode.EngineBuildIdentityPolicy = ""
	decode.HardwareIdentity = ""

	summary, ok := benchmarkAuthorityManifest[performance.BenchmarkAuthority]
	if !ok {
		t.Fatalf("benchmark authority %q is absent", performance.BenchmarkAuthority)
	}
	summary = cloneBenchmarkReceiptSummary(summary)
	exactPins, err := exactWeightDigestsForCurrentPerformance(performance)
	must(t, err)
	summary = projectBenchmarkSummaryToExactCell(summary, exactPins)
	canonicalCommit, err := canonicalFrozenMercSourceCommit(summary.MercSourceCommit)
	must(t, err)
	summary.MercSourceCommit = canonicalCommit
	summary.EngineBuildHash = ""
	summary.EngineBuildIdentityPolicy = ""
	summary.HardwareIdentity = ""
	measurement, ok := summary.Throughput[performance.RuntimeProfileID]
	if !ok {
		t.Fatalf("benchmark authority %q has no %s rate",
			performance.BenchmarkAuthority, performance.RuntimeProfileID)
	}
	measurement.Unit = "tokens"
	measurement.UnitScope = performanceUnitScopeDecodeOutputTokens
	measurement.Basis = "historical decode_output_tokens freeze for replay-only tests"
	// Keep rates consistent with the accepted performance projection.
	measurement.UnitsPerSecAtOperatingBatch = decode.ObservedUnitsPerSec
	measurement.BestObservedUnitsPerSec = decode.ObservedBestUnitsPerSec
	measurement.OperatingBatch = decode.OperatingBatch
	measurement.Precision = decode.Precision
	summary.Throughput[performance.RuntimeProfileID] = measurement
	decode.BenchmarkBasis = measurement.Basis

	summarySHA, err := benchmarkReceiptSummarySHA256(summary)
	must(t, err)
	out := FrozenRuntimeCellPerformance{
		Version:                 frozenRuntimeCellPerformanceLegacyVersion,
		PolicyRevision:          runtimeCellPerformanceLegacyPolicyRevision,
		Performance:             decode,
		BenchmarkSnapshot:       summary,
		BenchmarkSnapshotSHA256: summarySHA,
		ModelArtifactWireKind:   decode.WireKind,
		ModelArtifactPins:       append([]string(nil), exactPins...),
	}
	out.Digest, err = frozenRuntimeCellPerformanceDigest(out)
	must(t, err)
	mustf(t, validateFrozenRuntimeCellPerformance(&out),
		"legacy decode-only frozen performance fixture was not self-contained: %v")
	return &out
}

// The embed receipt measures completed embeddings/s, while ComputePlan settles
// max(records, raw input bytes/4). Multiplying the first by the price of the
// second silently treats one long input as both one unit and many units. New
// quote/admission must refuse until a conversion is frozen; an already accepted
// v2 placement remains replayable from its own historical snapshot.
func TestEmbedUnitMismatchRefusesNewQuoteButDoesNotRewriteHistoricalReplay(t *testing.T) {
	installTestOnlyExactIdentityForLegacyBenchmark(t, candleEmbedCell)
	profile, cell := cellByID(t, candleEmbedCell)
	performance := resolveCellPerformance(profile, cell, benchmarkNow)
	if performance.Status != cellThroughputMeasured || performance.Unit != "embeddings" ||
		performance.UnitScope != performanceUnitScopeCompletedEmbeddingRecords {
		t.Fatalf("embed evidence premise changed: status=%s authority=%q/%q reason=%q",
			performance.Status, performance.Unit, performance.UnitScope, performance.Reason)
	}

	workload, err := buildWorkloadDecision(embedSubmit(), strings.Repeat("6", 64))
	must(t, err)
	authority := catalogueAuthorityFixture(
		t, workload, SettlementCurrencyCode(),
		supplierShareForTest(t, workload.RuntimeJobType, workload.Binding.Model.Ref),
	)
	assertMismatch := func(stage string, err error) {
		t.Helper()
		if err == nil || !strings.Contains(err.Error(), `"embeddings"`) ||
			!strings.Contains(err.Error(), `"token_like_input_units"`) ||
			!strings.Contains(err.Error(), performanceUnitScopeCompletedEmbeddingRecords) ||
			!strings.Contains(err.Error(), performanceUnitScopeTokenLikeInputGeometry) ||
			!strings.Contains(err.Error(), "no frozen unit conversion") {
			t.Fatalf("%s did not refuse the incompatible throughput/settlement units: %v",
				stage, err)
		}
	}
	_, err = supplierAdmissionCeilingUSDHr(
		authority, workload.RuntimeJobType, workload.Binding.Tier,
		admissionCellsForWorkload(workload), workload.Binding.Constraints.HWClasses,
	)
	assertMismatch("quote ceiling", err)
	_, err = placementRequirementFor(embedSubmit(), workload, 1)
	assertMismatch("placement admission", err)

	// Encode what an older admission accepted before the cross-unit guard. The
	// frozen validator must not consult today's compatibility policy or manifest.
	frozen := historicalLegacyFrozenPerformanceForTest(t, performance)
	candidate := workload.RuntimeCandidates[0]
	modelKind := candidate.ModelKind
	if modelKind == "" {
		modelKind = workload.Binding.Model.Kind
	}
	historical := PlacementRequirement{
		Version:              2,
		JobType:              workload.RuntimeJobType,
		ModelRef:             workload.Binding.Model.Ref,
		ModelKind:            modelKind,
		RuntimeCellID:        candidate.CellID,
		RuntimeID:            candidate.RuntimeID,
		Engine:               candidate.Engine,
		RuntimeMatrixSHA256:  generatedRuntimeMatrixSHA256,
		MinMemoryGB:          float32(workload.MinimumMemoryGB),
		HWClasses:            []string{performance.MeasuredOnHWClass},
		DataResidency:        append([]string(nil), workload.Binding.Constraints.DataResidency...),
		MinReputation:        workload.Binding.MinReputation,
		TrustedOnly:          workload.Binding.Tier == "trusted",
		PerformanceAuthority: frozen,
		OfferedRateUsdHr: float32(expectedSupplierUSDHr(
			performance.ConservativeUnitsPerSec, authority.ReferencePricePer1K,
			authority.SupplierShare, workload.Binding.Tier)),
	}
	mustf(t, validatePlacementRequirement(historical, workload),
		"historical frozen embed placement was rewritten by current unit policy: %v")
	if historical.PerformanceAuthority.Performance.Unit != "embeddings" {
		t.Fatalf("historical replay rewrote frozen unit to %q",
			historical.PerformanceAuthority.Performance.Unit)
	}
	if historical.PerformanceAuthority.Performance.UnitScope != performanceUnitScopeCompletedEmbeddingRecords {
		t.Fatalf("historical replay rewrote frozen unit scope to %q",
			historical.PerformanceAuthority.Performance.UnitScope)
	}
}

// G070 r5 measures batch_infer under the settlement geometry directly
// (tokens/token_like_input_plus_max_output_tokens). New quote/admission must
// accept that current authority. A frozen historical decode-only snapshot still
// replays on its own terms and must not be rewritten by current scope policy.
func TestBatchSettlementScopeAdmitsNewQuoteAndDoesNotRewriteHistoricalReplay(t *testing.T) {
	profile, cell := cellByID(t, "candle-metal-llama1-infer")
	performance := resolveCellPerformance(profile, cell, benchmarkNow)
	if performance.Status != cellThroughputMeasured || performance.Unit != "tokens" ||
		performance.UnitScope != performanceUnitScopeTokenLikeInputPlusOutputTokens {
		t.Fatalf("batch evidence premise changed: status=%s authority=%q/%q reason=%q",
			performance.Status, performance.Unit, performance.UnitScope, performance.Reason)
	}
	if err := validateCurrentPerformanceSettlementAuthority(performance); err != nil {
		t.Fatalf("current settlement-geometry batch performance refused: %v", err)
	}

	sub, herr := normalizeAndValidateJobSubmit(jobSubmit{
		JobType: JobType{Type: "batch_infer", MaxTokens: 16},
		Model:   ModelRef{Kind: "gguf", Ref: "llama-3.2-1b-instruct-q4"},
		Constraints: JobConstraints{
			MaxDurationSecs: 3600,
		},
		Tier: "batch",
	})
	if herr != nil {
		t.Fatalf("normalize production batch fixture: %s", herr.msg)
	}
	workload, err := buildWorkloadDecision(sub, strings.Repeat("7", 64))
	must(t, err)
	authority := catalogueAuthorityFixture(
		t, workload, SettlementCurrencyCode(),
		supplierShareForTest(t, workload.RuntimeJobType, workload.Binding.Model.Ref),
	)
	if _, err = supplierAdmissionCeilingUSDHr(
		authority, workload.RuntimeJobType, workload.Binding.Tier,
		admissionCellsForWorkload(workload), workload.Binding.Constraints.HWClasses,
	); err != nil {
		t.Fatalf("quote ceiling refused settlement-compatible batch performance: %v", err)
	}
	if _, err = placementRequirementFor(sub, workload, 1); err != nil {
		t.Fatalf("placement admission refused settlement-compatible batch performance: %v", err)
	}

	// Historical decode-only freeze: build a self-contained legacy snapshot
	// under decode_output_tokens (the r4 measured geometry) and prove replay
	// still accepts that frozen block on its own terms without rewriting scope.
	frozen := historicalLegacyDecodeOnlyFrozenPerformanceForTest(t, performance)
	if frozen.Performance.UnitScope != performanceUnitScopeDecodeOutputTokens {
		t.Fatalf("historical freeze rewrote decode scope to %q", frozen.Performance.UnitScope)
	}
	candidate := workload.RuntimeCandidates[0]
	historical := PlacementRequirement{
		Version:              2,
		JobType:              workload.RuntimeJobType,
		ModelRef:             workload.Binding.Model.Ref,
		ModelKind:            candidate.ModelKind,
		RuntimeCellID:        candidate.CellID,
		RuntimeID:            candidate.RuntimeID,
		Engine:               candidate.Engine,
		RuntimeMatrixSHA256:  generatedRuntimeMatrixSHA256,
		MinMemoryGB:          float32(workload.MinimumMemoryGB),
		HWClasses:            []string{performance.MeasuredOnHWClass},
		DataResidency:        append([]string(nil), workload.Binding.Constraints.DataResidency...),
		MinReputation:        workload.Binding.MinReputation,
		TrustedOnly:          workload.Binding.Tier == "trusted",
		PerformanceAuthority: frozen,
		OfferedRateUsdHr: float32(expectedSupplierUSDHr(
			performance.ConservativeUnitsPerSec, authority.ReferencePricePer1K,
			authority.SupplierShare, workload.Binding.Tier)),
	}
	mustf(t, validatePlacementRequirement(historical, workload),
		"historical frozen decode-only placement was rewritten by current scope policy: %v")
	if historical.PerformanceAuthority.Performance.UnitScope != performanceUnitScopeDecodeOutputTokens {
		t.Fatalf("historical replay rewrote frozen batch scope to %q",
			historical.PerformanceAuthority.Performance.UnitScope)
	}
	// Current admission must still refuse that decode-only block if presented as
	// *current* authority (historical freeze remains the only allowed path).
	if err := validateCurrentPerformanceSettlementAuthority(frozen.Performance); err == nil ||
		!strings.Contains(err.Error(), performanceUnitScopeDecodeOutputTokens) ||
		!strings.Contains(err.Error(), performanceUnitScopeTokenLikeInputPlusOutputTokens) {
		t.Fatalf("decode-only historical performance was accepted as current settlement authority: %v", err)
	}
}

// Version-1 frozen performance snapshots predate UnitScope. Historical replay
// accepts an internally consistent scope-less snapshot, while every new freeze
// and durable write requires the explicit current field.
func TestHistoricalScopeLessFrozenPerformanceReplayRemainsSelfContained(t *testing.T) {
	installTestOnlyCombinedTokenAuthority(t)
	profile, cell := cellByID(t, "candle-metal-llama1-infer")
	performance := resolveCellPerformance(profile, cell, runtimeCellPerformanceNow())
	frozen := *historicalFrozenPerformanceForTest(t, performance)
	frozen.Performance.UnitScope = ""
	measurement := frozen.BenchmarkSnapshot.Throughput[performance.RuntimeProfileID]
	measurement.UnitScope = ""
	frozen.BenchmarkSnapshot.Throughput[performance.RuntimeProfileID] = measurement
	var err error
	frozen.BenchmarkSnapshotSHA256, err = benchmarkReceiptSummarySHA256(frozen.BenchmarkSnapshot)
	must(t, err)
	frozen.Digest, err = frozenRuntimeCellPerformanceDigest(frozen)
	must(t, err)
	mustf(t, validateFrozenRuntimeCellPerformance(&frozen),
		"scope-less historical performance replay inherited current policy: %v")
	if err := validateCurrentPerformanceSettlementAuthority(frozen.Performance); err == nil ||
		!strings.Contains(err.Error(), "no explicit performance-unit scope") {
		t.Fatalf("new admission accepted a scope-less historical performance block: %v", err)
	}
}

func TestPolicylessExactBuildSnapshotRemainsHistoricalOnly(t *testing.T) {
	sub := testOnlyCombinedTokenSubmit(t)
	workload, err := buildWorkloadDecision(sub, strings.Repeat("9", 64))
	must(t, err)
	placement, err := placementRequirementFor(sub, workload, 1)
	must(t, err)

	// Re-encode the already-frozen block as it existed after exact build/device
	// fields were introduced but before the short-hash algorithm acquired an
	// explicit policy tag. Its own digests remain authoritative for replay.
	legacy := placement
	legacy.EngineBuildIdentityPolicy = ""
	frozen := *placement.PerformanceAuthority
	frozen.Performance.EngineBuildIdentityPolicy = ""
	frozen.BenchmarkSnapshot = cloneBenchmarkReceiptSummary(frozen.BenchmarkSnapshot)
	frozen.BenchmarkSnapshot.EngineBuildIdentityPolicy = ""
	frozen.BenchmarkSnapshotSHA256, err = benchmarkReceiptSummarySHA256(frozen.BenchmarkSnapshot)
	must(t, err)
	frozen.Digest, err = frozenRuntimeCellPerformanceDigest(frozen)
	must(t, err)
	legacy.PerformanceAuthority = &frozen

	mustf(t, validateFrozenRuntimeCellPerformance(&frozen),
		"policy-less frozen exact-build authority was not historically readable: %v")
	mustf(t, validatePlacementRequirement(legacy, workload),
		"policy-less exact-build placement was not historically readable: %v")
	if err := validateCurrentPlacementRequirement(legacy, workload); err == nil ||
		!strings.Contains(err.Error(), "engine_build_identity_policy") {
		t.Fatalf("policy-less historical placement became current-admissible: %v", err)
	}
}

func TestFrozenMiniLMPerformanceCarriesOnlyTheSelectedCellArtifact(t *testing.T) {
	_, candleCell := cellByID(t, "candle-metal-minilm-embed")
	_, llamaCell := cellByID(t, "llama-cpp-metal-minilm-embed")
	candlePins, err := exactWeightDigestsForCell(candleCell, runtimeAuthorityModels)
	must(t, err)
	llamaPins, err := exactWeightDigestsForCell(llamaCell, runtimeAuthorityModels)
	must(t, err)
	if len(candlePins) != 1 || len(llamaPins) != 1 || candlePins[0] == llamaPins[0] {
		t.Fatalf("MiniLM exact-artifact premise changed: candle=%v llama.cpp=%v",
			candlePins, llamaPins)
	}

	t.Run("candle selected artifact", func(t *testing.T) {
		installTestOnlyExactIdentityForLegacyBenchmark(t, candleEmbedCell)
		candleProfile, exactCandleCell := cellByID(t, candleEmbedCell)
		performance := resolveCellPerformance(candleProfile, exactCandleCell, benchmarkNow)
		frozen := historicalFrozenPerformanceForTest(t, performance)
		if frozen.ModelArtifactWireKind != wireKindFor(exactCandleCell, runtimeAuthorityModels[exactCandleCell.Model].WireKind) {
			t.Fatalf("frozen wire kind=%q, want Candle selected kind %q",
				frozen.ModelArtifactWireKind, performance.WireKind)
		}
		if !slices.Equal(frozen.ModelArtifactPins, candlePins) ||
			!slices.Equal(frozen.BenchmarkSnapshot.ModelArtifactSHA256s, candlePins) {
			t.Fatalf("frozen Candle performance retained non-selected MiniLM artifacts: pins=%v snapshot=%v want=%v",
				frozen.ModelArtifactPins, frozen.BenchmarkSnapshot.ModelArtifactSHA256s, candlePins)
		}

		// Historical replay has no access to today's runtime catalogue. It still
		// rejects a sibling GGUF substituted on only one side of the frozen
		// cross-binding, even after the outer digest is honestly recomputed.
		tampered := *frozen
		tampered.ModelArtifactPins = append([]string(nil), llamaPins...)
		tampered.Digest, err = frozenRuntimeCellPerformanceDigest(tampered)
		must(t, err)
		if err := validateFrozenRuntimeCellPerformance(&tampered); err == nil ||
			!strings.Contains(err.Error(), "does not exactly cross-bind") {
			t.Fatalf("historical replay accepted sibling GGUF against frozen Candle snapshot: %v", err)
		}
	})

	t.Run("llama.cpp selected artifact", func(t *testing.T) {
		// The sibling is nevertheless the correct exact authority for llama.cpp's
		// cell. This proves the refusal is cell/wire scoped, not a GGUF blacklist.
		installTestOnlyExactIdentityForLegacyBenchmark(t, llamaEmbedCell)
		llamaProfile, exactLlamaCell := cellByID(t, llamaEmbedCell)
		llamaPerformance := resolveCellPerformance(llamaProfile, exactLlamaCell, benchmarkNow)
		llamaFrozen := historicalFrozenPerformanceForTest(t, llamaPerformance)
		if !slices.Equal(llamaFrozen.ModelArtifactPins, llamaPins) ||
			!slices.Equal(llamaFrozen.BenchmarkSnapshot.ModelArtifactSHA256s, llamaPins) ||
			llamaFrozen.ModelArtifactWireKind != "gguf" {
			t.Fatalf("llama.cpp frozen performance did not select its exact GGUF: %+v",
				llamaFrozen)
		}
	})
}

// G070: exactly candle-metal-llama1-infer admits under settlement geometry
// tokens/token_like_input_plus_max_output_tokens. No other routable production
// cell may admit (embed parked, media unbound). A wrong extra binding fails.
func TestExactlyLlamaLaneHasSettlementCompatibleAdmission(t *testing.T) {
	const wantCell = "candle-metal-llama1-infer"
	admitted := map[string]RuntimeCellPerformance{}
	routable := 0
	for _, profile := range runtimeAuthority.Runtimes {
		for _, cell := range profile.Cells {
			if !cell.Routable(profile) {
				continue
			}
			routable++
			_, performance, err := admissionUnitsPerSec(
				cell.Job, cell.Model, []string{cell.ID}, benchmarkNow)
			if err != nil {
				if cell.ID == wantCell {
					t.Fatalf("production cell %s refused settlement-compatible admission: %v",
						cell.ID, err)
				}
				continue
			}
			admitted[cell.ID] = performance
		}
	}
	if routable != 1 {
		t.Fatalf("production routable cell count=%d, want exactly 1 (%s)", routable, wantCell)
	}
	performance, ok := admitted[wantCell]
	if !ok || len(admitted) != 1 {
		t.Fatalf("admitted cells=%v, want exactly [%s]", admitted, wantCell)
	}
	if performance.Unit != "tokens" ||
		performance.UnitScope != performanceUnitScopeTokenLikeInputPlusOutputTokens {
		t.Fatalf("llama admission unit/scope=%q/%q, want tokens/%s",
			performance.Unit, performance.UnitScope,
			performanceUnitScopeTokenLikeInputPlusOutputTokens)
	}
}

func TestCurrentStalePerformanceIgnoresDiagnosticAgeProseOnly(t *testing.T) {
	testOnlyCombinedTokenSubmit(t)
	profile, cell := cellByID(t, "candle-metal-llama1-infer")
	receipt := benchmarkAuthorityManifest[cell.benchmarkAuthorityFor(profile)]
	measuredAt, err := time.Parse(time.RFC3339, receipt.MeasuredAt)
	must(t, err)
	firstAt := measuredAt.Add(benchmarkRevalidationWindow + 24*time.Hour)
	laterAt := firstAt.Add(24 * time.Hour)
	frozen := resolveCellPerformance(profile, cell, firstAt)
	later := resolveCellPerformance(profile, cell, laterAt)
	if frozen.Status != cellThroughputStale || later.Status != cellThroughputStale ||
		frozen.Reason == later.Reason {
		t.Fatalf("stale diagnostic-age premise failed: first=%+v later=%+v", frozen, later)
	}
	if err := validateCurrentRuntimeCellPerformanceAuthorityAt(frozen, laterAt); err != nil {
		t.Fatalf("unchanged stale authority failed solely because age prose moved: %v", err)
	}
}

// The constant this file tests admission against has to be the number the
// installer actually writes, or the test proves nothing about a real install.
func TestDefaultPayoutFloorMatchesTheInstaller(t *testing.T) {
	script, err := os.ReadFile("../scripts/install.sh")
	mustf(t, err, "read installer: %v")
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
		supplierShareForTest(t, "batch_infer", "llama-3.2-1b-instruct-q4"), "batch")
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
	installTestOnlyExactIdentityForLegacyBenchmark(t, candleEmbedCell)
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

func TestFutureDatedBenchmarkRefusesCurrentAdmission(t *testing.T) {
	installTestOnlyExactIdentityForLegacyBenchmark(t, candleEmbedCell)
	profile, cell := cellByID(t, "candle-metal-minilm-embed")
	receipt := benchmarkAuthorityManifest[cell.benchmarkAuthorityFor(profile)]
	measuredAt, err := time.Parse(time.RFC3339, receipt.MeasuredAt)
	mustf(t, err, "parse benchmark time: %v")
	authorityTime := measuredAt.Add(-time.Nanosecond)

	got := resolveCellPerformance(profile, cell, authorityTime)
	if got.Status != cellThroughputUnproven ||
		got.ConservativeUnitsPerSec != unprovenFallbackUnitsPerSec ||
		got.ObservedUnitsPerSec != 0 || got.ObservedBestUnitsPerSec != 0 ||
		!strings.Contains(got.Reason, "future-dated") {
		t.Fatalf("future-dated benchmark resolved as current authority: %+v", got)
	}
	if _, _, err := admissionUnitsPerSec(
		cell.Job, cell.Model, []string{cell.ID}, authorityTime,
	); err == nil || !strings.Contains(err.Error(), "future-dated") {
		t.Fatalf("future-dated benchmark admitted current work: %v", err)
	}
}

// "Never the best number in the sweep" is the rule; this is the check. Every
// routable cell's admissible rate must sit strictly below the receipt's best
// observation - which on a comparison receipt is a median of five repetitions
// at the best batch, not a peak.
func TestNoAdmissibleRateReachesTheBestObservation(t *testing.T) {
	installTestOnlyCombinedTokenAuthority(t)
	at := runtimeCellPerformanceNow()
	checked := 0
	for _, profile := range runtimeAuthority.Runtimes {
		for _, cell := range profile.Cells {
			if !cell.Routable(profile) {
				continue
			}
			got := resolveCellPerformance(profile, cell, at)
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
	// Parked embed needs a TEST_ONLY exact identity to reach the viability
	// surface at all; its refusal is then catalogue authority, not a missing
	// build hash. Production llama is already routable under r5.
	installTestOnlyExactIdentityForLegacyBenchmark(t, candleEmbedCell)
	authority := boardCatalogueAuthority(t)

	rows := SupplierAdmissionViability(
		"apple_silicon_pro", defaultInstallMinPayoutUSDHr,
		"batch", benchmarkNow, authority)
	if len(rows) == 0 {
		t.Fatal("the report is empty; a supplier learns nothing from it")
	}
	eligible := 0
	embedCatalogueRefusal := 0
	llamaUnderwater := 0
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
			!strings.Contains(row.Reason, "no usable benchmark") &&
			!strings.Contains(row.Reason, "no catalogue price authority") &&
			!strings.Contains(row.Reason, "no board authority") {
			t.Errorf("cell %s is ineligible at $%.5f/hr against a $%.5f/hr floor for "+
				"an unstated reason: %s", row.Performance.CellID,
				row.ExpectedSupplierUSDHr, row.MinimumPayoutUSDHr, row.Reason)
		}
		if row.Eligible {
			eligible++
		}
		if row.Performance.CellID == candleEmbedCell {
			if row.Eligible {
				t.Errorf("parked embed cell must not be eligible: %s", row.Reason)
			}
			if strings.Contains(row.Reason, "no catalogue price authority") ||
				strings.Contains(row.Reason, "no board authority") {
				embedCatalogueRefusal++
			} else {
				t.Errorf("embed refusal must name missing catalogue/board authority, got: %s",
					row.Reason)
			}
		}
		if row.Performance.CellID == "candle-metal-llama1-infer" {
			// Honest underwater signal: measured lane earns below the install floor.
			if row.Eligible {
				t.Errorf("llama lane must remain underwater vs $%.5f/hr floor, got eligible: %s",
					defaultInstallMinPayoutUSDHr, row.Reason)
			}
			if !strings.Contains(row.Reason, "minimum payout") ||
				!strings.Contains(row.Reason, "below your minimum payout") {
				t.Errorf("llama underwater refusal must name the payout floor comparison: %s",
					row.Reason)
			}
			if row.ExpectedSupplierUSDHr <= 0 ||
				row.ExpectedSupplierUSDHr >= row.MinimumPayoutUSDHr {
				t.Errorf("llama expected $%.5f/hr vs floor $%.5f/hr; want positive underwater shortfall",
					row.ExpectedSupplierUSDHr, row.MinimumPayoutUSDHr)
			}
			llamaUnderwater++
		}
		t.Logf("%-30s eligible=%-5v $%.5f/hr vs $%.5f/hr floor  %s",
			row.Performance.CellID, row.Eligible, row.ExpectedSupplierUSDHr,
			row.MinimumPayoutUSDHr, row.Reason)
	}
	if embedCatalogueRefusal == 0 {
		t.Fatal("viability report omitted the parked-embed catalogue-authority refusal")
	}
	if llamaUnderwater == 0 {
		t.Fatal("viability report omitted the honest llama underwater (below min payout) signal")
	}
	if eligible != 0 {
		t.Fatalf("eligible current cells=%d, want 0 (llama underwater, embed unpriced)", eligible)
	}
	t.Logf("eligible current cells=%d; embed catalogue refusals=%d; llama underwater=%d",
		eligible, embedCatalogueRefusal, llamaUnderwater)

	// A shortfall must quote both numbers, not just say no.
	shortfall := SupplierAdmissionViability(
		"apple_silicon_pro", 1000, "batch", benchmarkNow, authority)
	for _, row := range shortfall {
		// The bounded media lanes are fixed-contract canaries with positive
		// contribution at the reference price. A $1000/hr supplier floor is a
		// diagnostic for the model lanes, not a reason to reject those measured
		// media cells; their viability is checked by the shared economic plan.
		if row.Performance.CellID == "candle-metal-ffmpeg-transcode" ||
			row.Performance.CellID == "candle-metal-scene-render" {
			continue
		}
		if strings.Contains(row.Reason, "no market") ||
			strings.Contains(row.Reason, "no catalogue price authority") ||
			strings.Contains(row.Reason, "no board authority") {
			// Unpriced / unit-incompatible lanes are neither a low price nor a
			// payout-floor shortfall. Admission correctly reports no market /
			// no catalogue authority until the conversion or price binds.
			continue
		}
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
		"nvidia_80gb", 0, "batch", benchmarkNow, authority)
	hardwareNamed := 0
	for _, row := range wrongHardware {
		// Parked/unpriced models fail earlier on catalogue authority; that is
		// not a hardware-class report and is checked above.
		if strings.Contains(row.Reason, "no catalogue price authority") ||
			strings.Contains(row.Reason, "no board authority") {
			continue
		}
		if row.Eligible || !strings.Contains(row.Reason, "hardware class") {
			t.Errorf("cell %s on hardware it does not serve: eligible=%v reason=%q",
				row.Performance.CellID, row.Eligible, row.Reason)
		}
		hardwareNamed++
	}
	if hardwareNamed == 0 {
		t.Fatal("wrong-hardware viability omitted the hardware-class refusal for priced lanes")
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
			BatchInfer struct {
				ThroughputUnitsPerSecond float64 `json:"throughput_units_per_second"`
				UnitScope                string  `json:"unit_scope"`
			} `json:"batch_infer"`
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
			} else if throughput.UnitScope == performanceUnitScopeTokenLikeInputPlusOutputTokens {
				// Settlement-geometry receipts publish the BILLABLE rate under
				// that scope (batch_infer.throughput_units_per_second), not the
				// diagnostic physical serial decode rate. Manifest + scope are
				// correct; derivation must learn the billable geometry.
				want = receipt.BatchInfer.ThroughputUnitsPerSecond
				best = receipt.PhysicalThroughput.PeakTokensPerSec
				if want <= 0 {
					t.Errorf("%s: settlement-geometry receipt lacks batch_infer.throughput_units_per_second",
						path)
					continue
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
			if throughput.Unit == "" || throughput.UnitScope == "" ||
				throughput.Basis == "" || throughput.Precision == "" {
				t.Errorf("%s: %s publishes a rate with no unit, scope, basis or precision",
					path, profileID)
			}
			switch throughput.UnitScope {
			case performanceUnitScopeDecodeOutputTokens,
				performanceUnitScopeTokenLikeInputPlusOutputTokens,
				performanceUnitScopeCompletedEmbeddingRecords,
				performanceUnitScopeSingleObjectInputByteQuarters,
				performanceUnitScopeDeclaredOutputPixelsPerScene:
			default:
				t.Errorf("%s: %s publishes unknown performance-unit scope %q",
					path, profileID, throughput.UnitScope)
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
//
// The receipt must still BIND (identity + validity): a mismatched authority
// demotes the cell from the routable set entirely, which is a different branch
// (no routable cell at all). This test strips throughput from the cell's own
// bound receipt so Routable stays true and the unproven-rate refusal fires.
func TestUnprovenRoutableCellRefusesAdmissionRatherThanCollapsingIt(t *testing.T) {
	installTestOnlyExactIdentityForLegacyBenchmark(t, candleEmbedCell)
	profile, cell := cellByID(t, candleEmbedCell)
	path := cell.benchmarkAuthorityFor(profile)
	saved := benchmarkAuthorityManifest[path]
	t.Cleanup(func() { benchmarkAuthorityManifest[path] = saved })
	stripped := saved
	stripped.ThroughputMeasured = false
	stripped.Throughput = nil
	benchmarkAuthorityManifest[path] = stripped

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
		boardReferencePrice(t, "all-minilm-l6-v2", "embed"), supplierShareForTest(t, "embed", "all-minilm-l6-v2"), "batch")
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

// Candidate resolution still narrows to the cells the frozen workload can
// reach. This is intentionally below admission: embed rates are measured in an
// incompatible unit and therefore cannot be priced until conversion authority
// exists, but routing must still resolve the correct measured cell.
func TestPerformanceResolutionUsesOnlyTheCellsTheJobCanReach(t *testing.T) {
	installTestOnlyExactIdentityForLegacyBenchmark(t, candleEmbedCell)
	installTestOnlyExactIdentityForLegacyBenchmark(t, llamaEmbedCell)
	doc := mutableRuntimeAuthority(t)
	// Promoting llama.cpp's embed cell gives the model two routable cells whose
	// measured rates disagree, which is the situation this test is about.
	//
	// Which of the two is faster is deliberately NOT hardcoded. It used to be,
	// on the strength of an unbound receipt claiming llama.cpp was the quicker
	// embed engine; the re-sealed bound r2 measurement puts candle ahead. A test
	// that pins the winner by name asserts a measurement rather than a property,
	// and fails the moment the measurement is redone honestly.
	for i := range doc.Runtimes {
		if doc.Runtimes[i].RuntimeID == "llama_cpp_metal" {
			doc.Runtimes[i].Lifecycle = runtimeLifecycleActive
		}
	}
	mutableCell(t, doc, "llama-cpp-metal-minilm-embed").Lifecycle = runtimeLifecycleActive

	catalogue := routableCellPerformance("embed", "all-minilm-l6-v2", nil, benchmarkNow)
	if len(catalogue) != 2 {
		t.Fatalf("catalogue-wide performance resolved %d cells, want 2", len(catalogue))
	}
	slowCell := catalogue[0]
	// Pin to whichever cell is not the catalogue-wide slowest.
	faster := "llama-cpp-metal-minilm-embed"
	if slowCell.CellID == faster {
		faster = "candle-metal-minilm-embed"
	}
	pinned := routableCellPerformance(
		"embed", "all-minilm-l6-v2", []string{faster}, benchmarkNow)
	if len(pinned) != 1 {
		t.Fatalf("pinned performance resolved %d cells, want 1", len(pinned))
	}
	fastCell := pinned[0]
	if fastCell.CellID != faster {
		t.Fatalf("pinning to one candidate resolved cell %q", fastCell.CellID)
	}
	if fastCell.ConservativeUnitsPerSec <= slowCell.ConservativeUnitsPerSec {
		t.Fatalf("pinned cell %s resolves at %v units/s, no better than the "+
			"catalogue-wide minimum %v units/s taken from %s",
			fastCell.CellID, fastCell.ConservativeUnitsPerSec,
			slowCell.ConservativeUnitsPerSec, slowCell.CellID)
	}
}

// A directed workload must resolve performance from the cell it was directed
// to, but directed routing cannot bypass settlement-unit compatibility.
//
// Directed routing exists to send real buyer work to a cell that is PROVEN and
// deliberately kept out of the advertised catalogue -- llama.cpp embedding is
// exactly that cell. The first cut of routableCellPerformance filtered on
// Routable(), which drops precisely those cells, so a directed workload found no
// candidates at all, fell into the no-routable-cell branch, and was offered
// unprovenFallbackUnitsPerSec with the reason "no routable runtime cell serves
// job X on model Y". Both halves were wrong: a cell does serve it, and that cell
// is measured. A supplier would have been offered roughly a thousandth of the
// rate the cell it actually runs on can produce.
func TestDirectedWorkloadResolvesItsCellButCannotBypassUnitCompatibility(t *testing.T) {
	const authorityPath = "evidence/perf/runtime-benchmarks/embed-cell-candle-vs-llama-cpp-r2.json"
	saved := benchmarkAuthorityManifest[authorityPath]
	synthetic := cloneBenchmarkReceiptSummary(saved)
	synthetic.EngineBuildHash = testOnlyEngineBuildHash
	synthetic.EngineBuildIdentityPolicy = externalRunnerBuildIdentityPolicy
	synthetic.HardwareIdentity = testOnlyHardwareIdentity
	_, llamaCell := cellByID(t, llamaEmbedCell)
	pins, err := exactWeightDigestsForCell(llamaCell, runtimeAuthorityModels)
	must(t, err)
	synthetic.ModelArtifactSHA256s = pins
	benchmarkAuthorityManifest[authorityPath] = synthetic
	t.Cleanup(func() { benchmarkAuthorityManifest[authorityPath] = saved })

	at := time.Now()
	resolved := routableCellPerformance("embed", "all-minilm-l6-v2",
		[]string{llamaEmbedCell}, at)
	if len(resolved) != 1 {
		t.Fatalf("directed performance resolved %d cells, want one", len(resolved))
	}
	cell := resolved[0]
	if cell.CellID != llamaEmbedCell {
		t.Fatalf("directed to %s, priced from %q", llamaEmbedCell, cell.CellID)
	}
	if cell.ConservativeUnitsPerSec == unprovenFallbackUnitsPerSec {
		t.Fatalf("the directed cell was priced at the unproven fallback (%.1f). "+
			"Reason given: %q", cell.ConservativeUnitsPerSec, cell.Reason)
	}
	if cell.Status == cellThroughputUnproven {
		t.Errorf("a REAL_RUNTIME_PROVEN cell resolved as unproven: %s", cell.Reason)
	}
	if strings.Contains(cell.Reason, "no routable runtime cell") {
		t.Errorf("the reason denies that any cell serves this workload: %q", cell.Reason)
	}
	t.Logf("directed %s -> %.2f %s/s (%s)", llamaEmbedCell,
		cell.ConservativeUnitsPerSec, cell.Unit, cell.Reason)
	if _, _, err := admissionUnitsPerSec("embed", "all-minilm-l6-v2",
		[]string{llamaEmbedCell}, at); err == nil ||
		!strings.Contains(err.Error(), "token_like_input_units") {
		t.Fatalf("directed routing bypassed unit compatibility: %v", err)
	}

	// The TEST_ONLY directed identity does not widen ordinary production. Every
	// checked-in current credential remains historical-only, so no undirected
	// performance cell is available.
	undirectedCells := routableCellPerformance("embed", "all-minilm-l6-v2", nil, at)
	if len(undirectedCells) != 0 {
		t.Fatalf("directed TEST_ONLY identity widened undirected production to %+v",
			undirectedCells)
	}

	// And a pin at a cell that is not even directed-reachable must NOT be priced
	// as if it were. Otherwise the pin would have become a way to price off any
	// cell in the document.
	if _, resolved, err := admissionUnitsPerSec("embed", "all-minilm-l6-v2",
		[]string{"no-such-cell"}, at); err != nil {
		t.Fatalf("unexpected error: %v", err)
	} else if resolved.Status != cellThroughputUnproven {
		t.Errorf("a pin at an unknown cell resolved to %q on cell %q",
			resolved.Status, resolved.CellID)
	}
}

// The supplier-facing report must never disagree with admission.
//
// It used to carry its own sentence -- an unproven cell "is priced at the 1
// unit/s conservative fallback rather than as if measured" -- which was true
// while the fallback was the answer. Admission now refuses such a cell outright
// and the report went on promising a low rate on work that will never be posted.
// A supplier reading "priced very low" tunes their floor and waits forever.
//
// The report now quotes admission rather than restating it, so this checks the
// property on EVERY routable cell rather than only on the unproven ones the live
// document happens to contain -- an assertion that skips when the document is
// healthy is not a guard.
func TestViabilityReportNeverDisagreesWithAdmission(t *testing.T) {
	installTestOnlyCombinedTokenAuthority(t)
	authority := func(string) (CataloguePriceAuthority, error) {
		return CataloguePriceAuthority{ReferencePricePer1K: 0.0002, SupplierShare: 0.8}, nil
	}
	at := time.Now()
	rows := SupplierAdmissionViability(
		"apple_silicon_ultra", 0.01, "batch", at, authority)
	if len(rows) == 0 {
		t.Fatal("no routable cells at all; this report guards nothing")
	}

	for _, row := range rows {
		perf := row.Performance
		_, _, refusal := admissionUnitsPerSec(perf.JobType, perf.ModelID,
			[]string{perf.CellID}, at)

		switch {
		case refusal != nil && row.Eligible:
			t.Errorf("cell %q is reported eligible while admission refuses it: %v",
				perf.CellID, refusal)
		case refusal != nil && row.ExpectedSupplierUSDHr != 0:
			t.Errorf("cell %q quotes $%.5f/hr while admission refuses it",
				perf.CellID, row.ExpectedSupplierUSDHr)
		case refusal != nil && !strings.Contains(row.Reason, "no market"):
			t.Errorf("cell %q is refused by admission but the report does not say "+
				"so: %s", perf.CellID, row.Reason)
		}
		// The stale sentence, in either direction. A cell admission refuses must
		// not be described as merely cheap, and one it prices must not be
		// described as priced at a fallback it is not priced at.
		if strings.Contains(row.Reason, "conservative fallback rather than as if measured") {
			t.Errorf("cell %q still carries the report's own copy of a pricing rule "+
				"admission owns: %s", perf.CellID, row.Reason)
		}
		t.Logf("%s eligible=%v $%.5f/hr", perf.CellID, row.Eligible, row.ExpectedSupplierUSDHr)
	}
}
