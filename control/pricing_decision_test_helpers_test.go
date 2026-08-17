package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

const (
	testOnlyEngineBuildHash  = "deadbeefdeadbeef"
	testOnlyHardwareIdentity = "apple_silicon_v1|brand=TEST ONLY Apple M3 Ultra|model=Test1,1|memory_bytes=103079215104|cpu_cores=28|gpu_cores=60"
)

var testOnlyCombinedTokenAuthorityPath = "TEST_ONLY/uninstalled/combined-token-throughput"

// installTestOnlyDecodeOutputTokenAuthority installs an ephemeral exact-build
// authority for candle-metal-llama1-infer whose measured unit_scope is forced
// to decode_output_tokens. Production r6 is settlement-compatible; this
// fixture preserves the decode-only vs combined-token mismatch guard without
// relabeling checked-in evidence.
func installTestOnlyDecodeOutputTokenAuthority(t *testing.T) {
	t.Helper()
	const cellID = "candle-metal-llama1-infer"
	installTestOnlyExactIdentityForLegacyBenchmark(t, cellID)
	profile, cell := cellByID(t, cellID)
	path := cell.benchmarkAuthorityFor(profile)
	summary, ok := benchmarkAuthorityManifest[path]
	if !ok {
		t.Fatalf("TEST_ONLY decode-scope authority path %q is absent", path)
	}
	summary = cloneBenchmarkReceiptSummary(summary)
	measurement, ok := summary.Throughput[profile.RuntimeID]
	if !ok {
		t.Fatalf("TEST_ONLY decode-scope authority has no %s rate", profile.RuntimeID)
	}
	measurement.Unit = "tokens"
	measurement.UnitScope = performanceUnitScopeDecodeOutputTokens
	measurement.Basis = "TEST_ONLY decode_output_tokens identity for settlement-mismatch guard; never production evidence"
	summary.Throughput[profile.RuntimeID] = measurement
	summary.Harness += "; TEST_ONLY decode_output_tokens scope"
	benchmarkAuthorityManifest[path] = summary
}

// installTestOnlyExactIdentityForLegacyBenchmark gives unit/scope regression
// tests an ephemeral exact-build authority while preserving the legacy
// receipt's measured unit, scope and rate. It repoints only an in-memory copy
// of the runtime document; checked-in evidence remains untouched and retains
// its real withdrawn/superseded status. That makes the asserted current
// refusal about settlement-unit incompatibility, not an earlier missing-build
// refusal, while historical replay still freezes the same measured semantics.
func installTestOnlyExactIdentityForLegacyBenchmark(t *testing.T, cellID string) {
	t.Helper()
	savedAuthority := runtimeAuthority
	savedActivation := activeRuntimeActivation.Load()
	edited := runtimeAuthority
	edited.Runtimes = append([]authorityRuntimeProfile(nil), runtimeAuthority.Runtimes...)
	profileIndex, cellIndex := -1, -1
	for i := range edited.Runtimes {
		edited.Runtimes[i].Cells = append(
			[]authorityCell(nil), edited.Runtimes[i].Cells...)
		for j := range edited.Runtimes[i].Cells {
			if edited.Runtimes[i].Cells[j].ID == cellID {
				profileIndex, cellIndex = i, j
			}
		}
	}
	if profileIndex < 0 || cellIndex < 0 {
		t.Fatalf("TEST_ONLY exact legacy authority cannot find cell %q", cellID)
	}
	profile := edited.Runtimes[profileIndex]
	cell := edited.Runtimes[profileIndex].Cells[cellIndex]
	sourcePath := cell.benchmarkAuthorityFor(profile)
	summary, ok := benchmarkAuthorityManifest[sourcePath]
	if !ok {
		t.Fatalf("TEST_ONLY exact legacy authority source %q is absent", sourcePath)
	}
	summary = cloneBenchmarkReceiptSummary(summary)
	summary.Validity = authorityValidityValid
	summary.BindingStatus = BindingBound
	summary.EngineBuildHash = testOnlyEngineBuildHash
	summary.EngineBuildIdentityPolicy = requiredEngineBuildIdentityPolicy(profile, cell)
	summary.HardwareIdentity = testOnlyHardwareIdentity
	summary.Harness += "; TEST_ONLY exact identity wrapper for unit/scope mechanics"
	path := filepath.Join(t.TempDir(), "TEST_ONLY-unit-scope-benchmark.json")
	edited.Runtimes[profileIndex].Cells[cellIndex].BenchmarkAuthority = path
	runtimeAuthority = edited
	benchmarkAuthorityManifest[path] = summary
	installed := currentActivation()
	activeRuntimeActivation.Store(newRuntimeActivation(
		installed.PolicyRevision, map[string]string{}, nil))
	t.Cleanup(func() {
		delete(benchmarkAuthorityManifest, path)
		runtimeAuthority = savedAuthority
		activeRuntimeActivation.Store(savedActivation)
	})
}

// installTestOnlyCombinedTokenAuthority installs one ephemeral authority for
// tests that must exercise successful *new* placement/pricing mechanics. It is
// deliberately not an evidence path and never changes the embedded manifest or
// a receipt on disk. The global document and map are restored by test cleanup.
// Production-path tests must not call this helper: the checked-in BOUND batch
// receipt measures decode-output tokens only and is correctly refused against
// combined input-plus-output settlement.
func installTestOnlyCombinedTokenAuthority(t *testing.T) {
	t.Helper()
	if _, alreadyInstalled := benchmarkAuthorityManifest[testOnlyCombinedTokenAuthorityPath]; alreadyInstalled {
		return
	}

	savedAuthority := runtimeAuthority
	savedActivation := activeRuntimeActivation.Load()
	edited := runtimeAuthority
	edited.Runtimes = append([]authorityRuntimeProfile(nil), runtimeAuthority.Runtimes...)
	for i := range edited.Runtimes {
		edited.Runtimes[i].Cells = append([]authorityCell(nil), edited.Runtimes[i].Cells...)
	}

	profileIndex, cellIndex := -1, -1
	for i := range edited.Runtimes {
		if edited.Runtimes[i].RuntimeID != "candle_metal" {
			continue
		}
		for j := range edited.Runtimes[i].Cells {
			if edited.Runtimes[i].Cells[j].ID == "candle-metal-llama1-infer" {
				profileIndex, cellIndex = i, j
				break
			}
		}
	}
	if profileIndex < 0 || cellIndex < 0 {
		t.Fatal("TEST_ONLY combined-token authority cannot find candle batch cell")
	}
	profile := edited.Runtimes[profileIndex]
	cell := edited.Runtimes[profileIndex].Cells[cellIndex]
	sourcePath := cell.benchmarkAuthorityFor(profile)
	source, ok := benchmarkAuthorityManifest[sourcePath]
	if !ok {
		t.Fatalf("TEST_ONLY combined-token authority source %q is absent", sourcePath)
	}
	synthetic := cloneBenchmarkReceiptSummary(source)
	measurement, ok := synthetic.Throughput[profile.RuntimeID]
	if !ok {
		t.Fatalf("TEST_ONLY combined-token authority source has no %s rate", profile.RuntimeID)
	}
	measurement.Unit = "tokens"
	measurement.UnitScope = performanceUnitScopeTokenLikeInputPlusOutputTokens
	measurement.Precision = "TEST_ONLY synthetic combined-token mechanics"
	measurement.OperatingBatch = 1
	measurement.UnitsPerSecAtOperatingBatch = 64
	measurement.BestObservedUnitsPerSec = 128
	measurement.BestObservedBatch = 1
	measurement.Basis = "TEST_ONLY in-memory combined token-like input plus maximum output throughput; never production evidence"
	synthetic.Throughput[profile.RuntimeID] = measurement
	synthetic.Harness = "TEST_ONLY in-memory combined-token authority; cannot write evidence"
	synthetic.Validity = authorityValidityValid
	synthetic.BindingStatus = BindingBound
	// The production receipt's build credential predates the complete agent
	// source root. A mechanics fixture must not revive or borrow it: mint a
	// clearly synthetic exact identity and rewrite the ephemeral receipt below.
	// If the publication helper is already installed, its identity is itself
	// explicitly TEST_ONLY and must remain unchanged so the selected throughput
	// cell and the synthetic power receipt keep one exact physical identity.
	if source.EngineBuildIdentityPolicy != currentEngineBuildIdentityPolicy ||
		!validCurrentHardwareIdentity(synthetic.HardwareIdentity) {
		synthetic.EngineBuildHash = testOnlyEngineBuildHash
		synthetic.HardwareIdentity = testOnlyHardwareIdentity
	}
	synthetic.EngineBuildIdentityPolicy = currentEngineBuildIdentityPolicy
	synthetic.MeasuredAt = runtimeCellPerformanceNow().UTC().Format(time.RFC3339)
	if !engineBuildHashPattern.MatchString(synthetic.EngineBuildHash) ||
		synthetic.EngineBuildIdentityPolicy != currentEngineBuildIdentityPolicy ||
		!validCurrentHardwareIdentity(synthetic.HardwareIdentity) {
		t.Fatalf("TEST_ONLY combined-token source lacks exact build/device identity: %q/%q",
			synthetic.EngineBuildHash, synthetic.HardwareIdentity)
	}

	benchmarkIndex := -1
	for i, benchmark := range repricingBenchmarks {
		if benchmark.ModelID == cell.Model && benchmark.JobType == cell.Job &&
			benchmark.RuntimeCellID == cell.ID {
			benchmarkIndex = i
			break
		}
	}
	if benchmarkIndex < 0 {
		t.Fatal("TEST_ONLY combined-token authority cannot find exact repricing benchmark row")
	}
	benchmark := repricingBenchmarks[benchmarkIndex]
	receiptPath, fragment, ok := strings.Cut(benchmark.SourceCitation, "#")
	if !ok || strings.TrimSpace(fragment) == "" {
		t.Fatalf("TEST_ONLY combined-token source citation %q has no section", benchmark.SourceCitation)
	}
	resolved, err := resolveCitedEvidencePath(receiptPath)
	mustf(t, err, "resolve TEST_ONLY combined-token source receipt: %v")
	raw, err := os.ReadFile(resolved)
	mustf(t, err, "read TEST_ONLY combined-token source receipt: %v")
	var receipt map[string]any
	mustf(t, json.Unmarshal(raw, &receipt), "decode TEST_ONLY combined-token source receipt: %v")
	section, ok := receipt[fragment].(map[string]any)
	if !ok {
		t.Fatalf("TEST_ONLY combined-token source receipt has no %q section", fragment)
	}
	measuredAt := synthetic.MeasuredAt
	receipt["kind"] = "TEST_ONLY_synthetic_combined_token_authority"
	receipt["hardware_class"] = synthetic.HWClass
	receipt["hardware"] = map[string]any{"gpu": synthetic.HardwareIdentity}
	receipt["raw_measurement"] = map[string]any{
		"build_hash":            synthetic.EngineBuildHash,
		"build_identity_policy": synthetic.EngineBuildIdentityPolicy,
	}
	receipt["measured_at"] = measuredAt
	receipt["freshness_policy"] = catalogueThroughputFreshnessPolicy
	receipt["binding_status"] = BindingBound
	section["model"] = cell.Model
	section["runtime_cell_id"] = cell.ID
	section["runtime_profile_id"] = profile.RuntimeID
	section["profile_revision"] = profile.Revision
	section["engine"] = profile.Engine
	section["engine_revision"] = profile.EngineRevision
	section["engine_build_hash"] = synthetic.EngineBuildHash
	section["engine_build_identity_policy"] = synthetic.EngineBuildIdentityPolicy
	section["hardware_identity"] = synthetic.HardwareIdentity
	section["model_artifact_digest"] = benchmark.ModelArtifactDigest
	section["unit"] = measurement.Unit
	section["unit_scope"] = measurement.UnitScope
	section["throughput_units_per_second"] = measurement.UnitsPerSecAtOperatingBatch
	receipt[fragment] = section
	receiptBytes, err := json.MarshalIndent(receipt, "", "  ")
	mustf(t, err, "encode TEST_ONLY combined-token receipt: %v")
	installedPath := filepath.Join(t.TempDir(), "combined-token-throughput.json")
	mustf(t, os.WriteFile(installedPath, append(receiptBytes, '\n'), 0o600),
		"write TEST_ONLY combined-token receipt: %v")

	savedPath := testOnlyCombinedTokenAuthorityPath
	testOnlyCombinedTokenAuthorityPath = installedPath
	savedBenchmarks := append([]measuredThroughput(nil), repricingBenchmarks...)
	repricingBenchmarks = append([]measuredThroughput(nil), repricingBenchmarks...)
	repricingBenchmarks[benchmarkIndex].Unit = measurement.Unit
	repricingBenchmarks[benchmarkIndex].UnitScope = measurement.UnitScope
	repricingBenchmarks[benchmarkIndex].UnitsPerSec = measurement.UnitsPerSecAtOperatingBatch
	repricingBenchmarks[benchmarkIndex].HWClass = synthetic.HWClass
	repricingBenchmarks[benchmarkIndex].EngineBuildHash = synthetic.EngineBuildHash
	repricingBenchmarks[benchmarkIndex].EngineBuildIdentityPolicy = synthetic.EngineBuildIdentityPolicy
	repricingBenchmarks[benchmarkIndex].HardwareIdentity = synthetic.HardwareIdentity
	repricingBenchmarks[benchmarkIndex].SourceCitation = installedPath + "#" + fragment

	edited.Runtimes[profileIndex].Cells[cellIndex].BenchmarkAuthority = installedPath
	runtimeAuthority = edited
	benchmarkAuthorityManifest[installedPath] = synthetic
	// Activation caches resolved profiles and advertised/directed projections.
	// Repointing the authority document alone is order-dependent: Store startup
	// honestly quarantines the checked-in zero-lane document, and preserving that
	// overlay would keep this explicitly synthetic success fixture quarantined as
	// well. Give the TEST_ONLY document an empty lifecycle overlay at the same
	// revision, then restore the exact prior pointer at cleanup. Production-path
	// quarantine tests never call this helper.
	if savedActivation == nil {
		activeRuntimeActivation.Store(documentActivation())
	} else {
		activeRuntimeActivation.Store(newRuntimeActivation(
			savedActivation.PolicyRevision, map[string]string{}, nil))
	}
	t.Cleanup(func() {
		runtimeAuthority = savedAuthority
		repricingBenchmarks = savedBenchmarks
		delete(benchmarkAuthorityManifest, installedPath)
		testOnlyCombinedTokenAuthorityPath = savedPath
		activeRuntimeActivation.Store(savedActivation)
	})
}

// testOnlyCombinedTokenSubmit is the shared successful-current-admission
// fixture. Its authority is explicitly synthetic; no checked-in BOUND lane has
// throughput matching current settlement semantics.
func testOnlyCombinedTokenSubmit(t *testing.T) jobSubmit {
	t.Helper()
	installTestOnlyCombinedTokenAuthority(t)
	sub, herr := normalizeAndValidateJobSubmit(jobSubmit{
		JobType: JobType{Type: "batch_infer", MaxTokens: 16},
		Model:   ModelRef{Kind: "gguf", Ref: "llama-3.2-1b-instruct-q4"},
		Constraints: JobConstraints{
			MaxDurationSecs: 3600,
		},
		Tier: "batch",
	})
	if herr != nil {
		t.Fatalf("normalize priced batch fixture: %s", herr.msg)
	}
	return sub
}

func TestTestOnlyCombinedTokenAuthorityRefreshesInitializedActivation(t *testing.T) {
	// Reproduce the suite/DB order that exposed the stale projection: production
	// activation is initialized and honestly quarantines the unbindable checked-in
	// cell before the test seam installs its synthetic replacement.
	previous := activeRuntimeActivation.Load()
	initialized := currentActivation()
	activeRuntimeActivation.Store(newRuntimeActivation(
		initialized.PolicyRevision,
		map[string]string{
			activationKey("candle_metal", "candle-metal-llama1-infer"): runtimeLifecycleQuarantined,
		}, nil))
	t.Cleanup(func() { activeRuntimeActivation.Store(previous) })
	before := currentActivation()
	installTestOnlyCombinedTokenAuthority(t)
	after := currentActivation()
	if after == before {
		t.Fatal("TEST_ONLY authority retained the pre-install activation snapshot")
	}
	if !advertisedRuntimeCell("candle-metal-llama1-infer") {
		t.Fatal("TEST_ONLY exact batch cell is absent from the refreshed advertised projection")
	}
	_ = testOnlyCombinedTokenSubmit(t)
}

func pricingComputePlanFixture(t *testing.T) (WorkloadDecision, ComputePlan, EconomicPlan) {
	t.Helper()
	sub := testOnlyCombinedTokenSubmit(t)
	decision, err := buildWorkloadDecision(sub, strings.Repeat("a", 64))
	mustf(t, err, "build priced batch workload: %v")
	share := supplierShareForTest(t, decision.RuntimeJobType, decision.Binding.Model.Ref)
	economic := BuildEconomicPlan(EconomicPlanInput{
		BaseComputeUSD:   0.40,
		InitialTaskCount: 4,
		ExtraTaskReserve: 2,
		SupplierShare:    share,
	}, testEconomicSchedule())
	if !economic.Executable {
		t.Fatalf("priced batch fixture economics blocked: %s", economic.BlockReason)
	}
	plan, err := newDistributedComputePlan(
		decision,
		4,
		512,
		testInputDepthProfile(4),
		2,
		2,
		1,
		1,
		quoteTimeFromETABands(30, 50, true),
		"planner",
		0.20,
		0.20,
		QuoteConfidence{Score: 0.8, Reasons: []string{"fixture planner evidence"}},
		[]string{"fixture unknown"},
	)
	mustf(t, err, "build priced batch compute plan: %v")
	mustf(t, ValidateComputePlanEconomicSnapshot(plan, decision, economic),
		"valid priced batch compute/economic authority rejected: %v")
	return decision, plan, economic
}

func supplierShareForTest(t *testing.T, jobType, modelID string) float64 {
	t.Helper()
	share, err := supplierShareForWorkload(jobType, modelID)
	mustf(t, err, "supplier share policy for %s/%s: %v", jobType, modelID)
	return share
}

func catalogueAuthorityFixture(t *testing.T, workload WorkloadDecision, currency string, supplierShare float64) CataloguePriceAuthority {
	return catalogueAuthorityFixtureAtReferencePrice(
		t, workload, currency, supplierShare, 0.01,
	)
}

// catalogueAuthorityFixtureAtReferencePrice makes the catalogue rate explicit
// for tests whose economic geometry is itself under test. Most money-path tests
// deliberately use the generic one-cent fixture above; a measured workload must
// not inherit that unrelated price and then repair the result with arithmetic at
// the call site.
func catalogueAuthorityFixtureAtReferencePrice(
	t *testing.T,
	workload WorkloadDecision,
	currency string,
	supplierShare float64,
	referencePrice float64,
) CataloguePriceAuthority {
	t.Helper()
	rate := 1.0
	fxRevision := "identity-usd"
	if currency != "usd" {
		rate = 1.35
		fxRevision = "test-fx-" + currency
	}
	// Digest is unique per fixture content so USD and CAD (and share variants)
	// can each exist as distinct append-only schedule rows under store-backed
	// validation. A fixed all-c digest made two different authorities claim the
	// same schedule and cannot be seeded.
	digestInput := fmt.Sprintf(
		"fixture-catalogue|%s|%s|%s|%.12g|%.12g|%s|%.12g|%s",
		workload.Binding.Model.Ref, workload.RuntimeJobType, currency,
		referencePrice, rate, fxRevision, supplierShare, supplierSharePolicyRevision,
	)
	sum := sha256.Sum256([]byte(digestInput))
	boardSum := sha256.Sum256([]byte("fixture-board|" + digestInput))
	authority := CataloguePriceAuthority{
		Version:                     cataloguePriceScheduleVersion,
		ModelID:                     workload.Binding.Model.Ref,
		JobType:                     workload.RuntimeJobType,
		PriceSource:                 "market_board",
		ScheduleSHA256:              hex.EncodeToString(sum[:]),
		ScheduleVersion:             cataloguePriceScheduleVersion,
		ReferenceCurrency:           catalogueReferenceCurrency,
		ReferencePricePer1K:         referencePrice,
		SettlementCurrency:          currency,
		SettlementPricePer1K:        ceilPricePer1K(referencePrice * rate),
		ReferenceToSettlementRate:   rate,
		FXRevision:                  fxRevision,
		BoardSHA256:                 hex.EncodeToString(boardSum[:]),
		CurrentUseValidUntil:        "2099-01-01T00:00:00Z",
		PriceFormula:                "test market-board authority",
		SupplierShare:               supplierShare,
		SupplierSharePolicyRevision: supplierSharePolicyRevision,
	}
	if physical, ok := testOnlyCataloguePhysicalAuthority(workload); ok {
		authority.PhysicalAuthority = physical
	}
	mustf(t, validateCataloguePriceAuthority(authority), "catalogue authority fixture: %v")
	return authority
}

// testOnlyCataloguePhysicalAuthority projects the current synthetic benchmark
// fixture into the same immutable shape as a published schedule result. It is
// intentionally in-memory test authority; current store/publication tests use
// installBoundCataloguePublicationAuthorityForTest and real ephemeral receipt
// bytes instead.
func testOnlyCataloguePhysicalAuthority(
	workload WorkloadDecision,
) (CatalogueResultPhysicalAuthority, bool) {
	_, performance, err := admissionUnitsPerSec(
		workload.RuntimeJobType, workload.Binding.Model.Ref,
		admissionCellsForWorkload(workload), runtimeCellPerformanceNow(),
	)
	if err != nil {
		return CatalogueResultPhysicalAuthority{}, false
	}
	frozen, err := freezeRuntimeCellPerformance(performance)
	if err != nil || len(frozen.ModelArtifactPins) != 1 || len(workload.RuntimeCandidates) != 1 {
		return CatalogueResultPhysicalAuthority{}, false
	}
	candidate := workload.RuntimeCandidates[0]
	engineRevision := ""
	for _, profile := range runtimeAuthority.Runtimes {
		if profile.RuntimeID == performance.RuntimeProfileID &&
			profile.Revision == performance.ProfileRevision {
			engineRevision = profile.EngineRevision
			break
		}
	}
	measuredAt, err := time.Parse(time.RFC3339, performance.BenchmarkedAt)
	if err != nil {
		return CatalogueResultPhysicalAuthority{}, false
	}
	throughputUntil := measuredAt.UTC().Add(catalogueThroughputMaxAge)
	powerUntil := measuredAt.UTC().Add(cataloguePowerMaxAge)
	validUntil := throughputUntil
	if powerUntil.Before(validUntil) {
		validUntil = powerUntil
	}
	receiptDigest := sha256.Sum256([]byte(performance.BenchmarkAuthority + "\x00TEST_ONLY physical"))
	powerDigest := sha256.Sum256([]byte(performance.MeasuredOnHWClass + "\x00TEST_ONLY power"))
	physical := CatalogueResultPhysicalAuthority{
		Version: catalogueResultPhysicalAuthorityVersion,
		ModelID: workload.Binding.Model.Ref, JobType: workload.RuntimeJobType,
		RuntimeCellID: candidate.CellID, RuntimeProfileID: performance.RuntimeProfileID,
		ProfileRevision: performance.ProfileRevision, Engine: candidate.Engine,
		EngineRevision: engineRevision, EngineBuildHash: performance.EngineBuildHash,
		EngineBuildIdentityPolicy: performance.EngineBuildIdentityPolicy,
		HWClass:                   performance.MeasuredOnHWClass, HardwareIdentity: performance.HardwareIdentity,
		Unit: performance.Unit, UnitScope: performance.UnitScope,
		ModelArtifactDigest: frozen.ModelArtifactPins[0],
		Throughput: CatalogueThroughputAuthoritySnapshot{
			Citation:                   performance.BenchmarkAuthority + "#TEST_ONLY",
			ReceiptSHA256:              hex.EncodeToString(receiptDigest[:]),
			BenchmarkSummarySHA256:     frozen.BenchmarkSnapshotSHA256,
			EngineBuildHash:            performance.EngineBuildHash,
			EngineBuildIdentityPolicy:  performance.EngineBuildIdentityPolicy,
			HardwareIdentity:           performance.HardwareIdentity,
			FreshnessPolicy:            catalogueThroughputFreshnessPolicy,
			MeasuredAt:                 measuredAt.UTC().Format(time.RFC3339),
			ValidUntil:                 throughputUntil.Format(time.RFC3339),
			ObservedUnitsPerSecond:     performance.ObservedUnitsPerSec,
			HaircutPolicyRevision:      frozen.PolicyRevision,
			Haircut:                    performance.Haircut,
			ConservativeUnitsPerSecond: performance.ConservativeUnitsPerSec,
		},
		Power: CataloguePowerAuthoritySnapshot{
			Citation:                  "TEST_ONLY/in-memory/whole-package-power#measurement",
			ReceiptSHA256:             hex.EncodeToString(powerDigest[:]),
			RuntimeCellID:             candidate.CellID,
			RuntimeProfileID:          performance.RuntimeProfileID,
			Engine:                    candidate.Engine,
			EngineBuildHash:           performance.EngineBuildHash,
			EngineBuildIdentityPolicy: performance.EngineBuildIdentityPolicy,
			HWClass:                   performance.MeasuredOnHWClass,
			HardwareIdentity:          performance.HardwareIdentity,
			FreshnessPolicy:           cataloguePowerFreshnessPolicy,
			MeasurementBoundary:       "whole_package", WorkloadClass: "inference_shaped",
			Unit: "watts", AuthorityScope: cataloguePowerAuthorityScope,
			Aggregation:       cataloguePowerAggregation,
			OperatingProtocol: cataloguePowerOperatingProtocol,
			CoveredWorkloads: []CataloguePowerCoveredWorkload{{
				ModelID: workload.Binding.Model.Ref, JobType: workload.RuntimeJobType,
				ModelArtifactDigest: frozen.ModelArtifactPins[0],
				RuntimeCellID:       candidate.CellID, RuntimeProfileID: performance.RuntimeProfileID,
				Engine: candidate.Engine, EngineBuildHash: performance.EngineBuildHash,
				EngineBuildIdentityPolicy: performance.EngineBuildIdentityPolicy,
				HardwareIdentity:          performance.HardwareIdentity,
			}},
			Watts: 30, MeasuredAt: measuredAt.UTC().Format(time.RFC3339),
			ValidUntil: powerUntil.Format(time.RFC3339),
		},
		ValidUntil: validUntil.Format(time.RFC3339),
	}
	if _, err := validateCatalogueResultPhysicalAuthority(RepriceResult{
		ModelID: physical.ModelID, JobType: physical.JobType, PhysicalAuthority: physical,
	}); err != nil {
		return CatalogueResultPhysicalAuthority{}, false
	}
	return physical, true
}

// seedCataloguePriceAuthority inserts the append-only schedule and history rows
// that store-backed pricing validation resolves. Idempotent on the fixture
// digest: a second call with the same authority is a no-op.
func seedCataloguePriceAuthority(t *testing.T, ctx context.Context, pool *pgxpool.Pool, a CataloguePriceAuthority) {
	t.Helper()
	if pool == nil {
		t.Fatal("seedCataloguePriceAuthority requires a pool")
	}
	mustf(t, validateCataloguePriceAuthority(a), "seed catalogue authority: %v")
	scheduleJSON, err := json.Marshal(map[string]any{
		"sha256":                         a.ScheduleSHA256,
		"version":                        a.ScheduleVersion,
		"reference_currency":             a.ReferenceCurrency,
		"settlement_currency":            a.SettlementCurrency,
		"fx_revision":                    a.FXRevision,
		"board_sha256":                   a.BoardSHA256,
		"current_use_valid_until":        a.CurrentUseValidUntil,
		"supplier_share_policy_revision": a.SupplierSharePolicyRevision,
		"results": []RepriceResult{{
			ModelID: a.ModelID, JobType: a.JobType,
			ReferencePricePer1K: a.ReferencePricePer1K,
			PricePer1K:          a.SettlementPricePer1K,
			SupplierShare:       a.SupplierShare, Formula: a.PriceFormula,
			PhysicalAuthority: a.PhysicalAuthority,
		}},
	})
	mustf(t, err, "marshal fixture schedule: %v")
	if _, err := pool.Exec(ctx, `
		INSERT INTO catalogue_price_schedules (
		  sha256,version,reference_currency,settlement_currency,
		  reference_to_settlement_rate,fx_revision,board_sha256,board_schema_version,
		  board_fetched_at,positioning_multiplier,supplier_share,schedule_json
		) VALUES ($1,$2,$3,$4,$5,$6,$7,1,'1970-01-01T00:00:00Z',1.0,NULL,$8)
		ON CONFLICT (sha256) DO NOTHING`,
		a.ScheduleSHA256, a.ScheduleVersion, a.ReferenceCurrency, a.SettlementCurrency,
		a.ReferenceToSettlementRate, a.FXRevision, a.BoardSHA256, scheduleJSON,
	); err != nil {
		t.Fatalf("seed catalogue_price_schedules: %v", err)
	}
	// Ensure the model row exists for the history FK when tests use non-seed models.
	if _, err := pool.Exec(ctx, `
		INSERT INTO models (id, job_type, kind)
		VALUES ($1,$2,'hf')
		ON CONFLICT (id) DO NOTHING`,
		a.ModelID, a.JobType,
	); err != nil {
		t.Fatalf("ensure model %s for catalogue seed: %v", a.ModelID, err)
	}
	if _, err := pool.Exec(ctx, `
		INSERT INTO model_price_history (
		  schedule_sha256,model_id,job_type,prior_price_per_1k,prior_price_source,
		  reference_price_per_1k,reference_currency,price_per_1k,
		  price_currency,price_formula,supplier_share
		) VALUES ($1,$2,$3,0,'seed',$4,$5,$6,$7,$8,$9)
		ON CONFLICT (schedule_sha256, model_id) DO NOTHING`,
		a.ScheduleSHA256, a.ModelID, a.JobType, a.ReferencePricePer1K, a.ReferenceCurrency,
		a.SettlementPricePer1K, a.SettlementCurrency, a.PriceFormula, a.SupplierShare,
	); err != nil {
		t.Fatalf("seed model_price_history for %s: %v", a.ModelID, err)
	}
}

// catalogueAuthorityFixtureInStore is the store-backed counterpart of
// catalogueAuthorityFixture: same numbers, and the append-only rows exist.
func catalogueAuthorityFixtureInStore(
	t *testing.T,
	ctx context.Context,
	pool *pgxpool.Pool,
	workload WorkloadDecision,
	currency string,
	supplierShare float64,
) CataloguePriceAuthority {
	t.Helper()
	authority := catalogueAuthorityFixture(t, workload, currency, supplierShare)
	seedCataloguePriceAuthority(t, ctx, pool, authority)
	return authority
}

func placementForPricingFixture(
	t *testing.T,
	workload WorkloadDecision,
	authority CataloguePriceAuthority,
) PlacementRequirement {
	t.Helper()
	binding := workload.Binding
	ceiling, err := supplierAdmissionCeilingUSDHr(
		authority, workload.RuntimeJobType, binding.Tier,
		admissionCellsForWorkload(workload),
		binding.Constraints.HWClasses,
	)
	mustf(t, err, "derive placement ceiling fixture: %v")
	placement, err := placementRequirementFor(jobSubmit{
		JobType: binding.JobType, Model: binding.Model, Constraints: binding.Constraints,
		Tier: binding.Tier, MinReputation: binding.MinReputation,
	}, workload, float32(ceiling))
	mustf(t, err, "build placement fixture: %v")
	return placement
}
