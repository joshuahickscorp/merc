package main

import (
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

const (
	testOnlyEmbedModelArtifactDigest = "53aa51172d142c89d9012cce15ae4d6cc0ca6895895114379cacb4fab128d9db"
	testOnlyBatchModelArtifactDigest = "3f5a22426976ab26cfe84dba63c1d08391717abb1af893e10f1b2968d862dcc1"
	testOnlyPowerReceiptFragment     = "whole_package_power"
	testOnlyPublicationBuildHash     = "feedfacefeedface"
	testOnlyPublicationHardware      = "apple_silicon_v1|brand=TEST ONLY Apple M3 Pro|model=Test2,1|memory_bytes=38654705664|cpu_cores=12|gpu_cores=18"
)

// The synthetic publication describes a TEST ONLY machine by default. A test
// that runs a live `merc-agent run` must instead describe the machine the agent
// is really on: control requires worker identity to equal the advertised cell's
// benchmark identity exactly, so a fabricated M3 Pro cell can never be enrolled
// by a real M3 Ultra worker.
var (
	publicationIdentityBuildHash = testOnlyPublicationBuildHash
	publicationIdentityHardware  = testOnlyPublicationHardware
	publicationIdentityHWClass   = "apple_silicon_pro"
)

// useLiveAgentPublicationIdentityForTest points the synthetic publication at the
// identity this machine's agent actually presents at register time. It asks the
// agent binary rather than hardcoding, so the fixture cannot drift from the
// worker: same source, same answer.
func useLiveAgentPublicationIdentityForTest(t *testing.T) {
	t.Helper()
	out, err := exec.Command(agentBinaryPath(t), "characterize").Output()
	mustf(t, err, "characterize live agent identity: %v")
	var characterization struct {
		HardwareClass    string `json:"hardware_class"`
		HardwareIdentity string `json:"hardware_identity"`
		SourceIdentity   string `json:"source_identity"`
	}
	mustf(t, json.Unmarshal(out, &characterization), "decode agent characterization: %v")
	if characterization.HardwareClass == "" || characterization.HardwareIdentity == "" ||
		characterization.SourceIdentity == "" {
		t.Fatalf("agent characterization is missing identity: %+v", characterization)
	}
	previousHash := publicationIdentityBuildHash
	previousHardware := publicationIdentityHardware
	previousClass := publicationIdentityHWClass
	t.Cleanup(func() {
		publicationIdentityBuildHash = previousHash
		publicationIdentityHardware = previousHardware
		publicationIdentityHWClass = previousClass
	})
	publicationIdentityBuildHash = characterization.SourceIdentity
	publicationIdentityHardware = characterization.HardwareIdentity
	publicationIdentityHWClass = characterization.HardwareClass
}

type pricingThroughputReceiptMutation func(jobType string, receipt map[string]any)
type pricingPowerReceiptMutation func(receipt map[string]any)

// installBoundCataloguePublicationAuthorityForTest is the single test-only
// seam for tests that exercise fresh schedule mechanics. Production evidence
// remains UNBOUND and production ultra watts remain a VENDOR_WALL_UPPER_BOUND;
// this helper writes ephemeral, explicitly synthetic BOUND throughput and
// MEASURED power receipts. It never edits or relabels checked-in evidence.
func installBoundCataloguePublicationAuthorityForTest(t *testing.T) {
	installBoundCataloguePublicationAuthorityWithMutationsForTest(t, nil, nil)
}

// installBoundCataloguePublicationAuthorityWithMutationForTest exposes each
// synthetic per-model throughput receipt only to adversarial tests. Keeping one
// model per receipt makes producer_identity.model_artifact_digest unambiguous.
func installBoundCataloguePublicationAuthorityWithMutationForTest(
	t *testing.T,
	mutate pricingThroughputReceiptMutation,
) {
	installBoundCataloguePublicationAuthorityWithMutationsForTest(t, mutate, nil)
}

// installBoundCataloguePowerAuthorityWithMutationForTest exposes the synthetic
// power receipt to adversarial tests while leaving throughput authority valid.
func installBoundCataloguePowerAuthorityWithMutationForTest(
	t *testing.T,
	mutate pricingPowerReceiptMutation,
) {
	installBoundCataloguePublicationAuthorityWithMutationsForTest(t, nil, mutate)
}

// ensureCrossCurrencyCatalogueFXForTest declares the USD→settlement FX the
// catalogue and cost schedule require when the process does not settle USD.
// It never installs identity-usd under CAD (that would relabel published USD
// rates). Tests that already set both env vars, or that pin USD settlement,
// are left alone.
func ensureCrossCurrencyCatalogueFXForTest(t *testing.T) {
	t.Helper()
	settlement := SettlementCurrencyCode()
	if settlement == "" || settlement == costReferenceCurrency {
		return
	}
	if strings.TrimSpace(os.Getenv(priceFXRateEnv)) != "" &&
		strings.TrimSpace(os.Getenv(priceFXRevisionEnv)) != "" {
		return
	}
	t.Setenv(priceFXRateEnv, "1.37")
	t.Setenv(priceFXRevisionEnv, "first-complete-loop-operator-declared")
}

func installBoundCataloguePublicationAuthorityWithMutationsForTest(
	t *testing.T,
	mutateThroughput pricingThroughputReceiptMutation,
	mutatePower pricingPowerReceiptMutation,
) {
	t.Helper()
	ensureCrossCurrencyCatalogueFXForTest(t)
	commitRaw, err := gitBytes(".", "rev-parse", "HEAD")
	mustf(t, err, "resolve test source commit: %v")
	commit := strings.TrimSpace(string(commitRaw))

	previousBenchmarks := append([]measuredThroughput(nil), repricingBenchmarks...)
	previousUnpriced := append([]measuredThroughput(nil), unpricedThroughputUntilBound...)
	previousWatts, hadPreviousWatts := sustainedWattsByHWClass[publicationIdentityHWClass]
	previousRuntimeAuthority := runtimeAuthority
	previousActivation := activeRuntimeActivation.Load()
	editedRuntimeAuthority := runtimeAuthority
	editedRuntimeAuthority.Runtimes = append(
		[]authorityRuntimeProfile(nil), runtimeAuthority.Runtimes...)
	for i := range editedRuntimeAuthority.Runtimes {
		editedRuntimeAuthority.Runtimes[i].Cells = append(
			[]authorityCell(nil), editedRuntimeAuthority.Runtimes[i].Cells...)
	}
	var installedBenchmarkAuthorities []string
	t.Cleanup(func() {
		repricingBenchmarks = previousBenchmarks
		unpricedThroughputUntilBound = previousUnpriced
		runtimeAuthority = previousRuntimeAuthority
		for _, path := range installedBenchmarkAuthorities {
			delete(benchmarkAuthorityManifest, path)
		}
		if hadPreviousWatts {
			sustainedWattsByHWClass[publicationIdentityHWClass] = previousWatts
		} else {
			delete(sustainedWattsByHWClass, publicationIdentityHWClass)
		}
		activeRuntimeActivation.Store(previousActivation)
	})

	type throughputFixture struct {
		modelID       string
		modelDigest   string
		jobType       string
		runtimeCellID string
		unit          string
		unitScope     string
		unitsPerSec   float64
		receiptValue  float64
	}
	fixtures := []throughputFixture{
		{
			modelID:       "all-minilm-l6-v2",
			modelDigest:   testOnlyEmbedModelArtifactDigest,
			jobType:       "embed",
			runtimeCellID: "candle-metal-minilm-embed",
			unit:          "token_like_input_units",
			unitScope:     performanceUnitScopeTokenLikeInputGeometry,
			unitsPerSec:   1967.3141,
			receiptValue:  1967.3141,
		},
		{
			modelID:       "llama-3.2-1b-instruct-q4",
			modelDigest:   testOnlyBatchModelArtifactDigest,
			jobType:       "batch_infer",
			runtimeCellID: "candle-metal-llama1-infer",
			unit:          "tokens",
			unitScope:     performanceUnitScopeTokenLikeInputPlusOutputTokens,
			unitsPerSec:   138.71389521174524,
			receiptValue:  138.71389521174524,
		},
	}
	repricingBenchmarks = make([]measuredThroughput, 0, len(fixtures))
	// Synthetic publication fixtures temporarily price models that production may
	// honestly park in unpricedThroughputUntilBound (e.g. embed). Clear the
	// quarantine for this install so the both-lists-forbidden invariant holds;
	// cleanup restores the production parking list.
	unpricedThroughputUntilBound = nil
	for _, fixture := range fixtures {
		profileIndex, cellIndex := -1, -1
		for i := range editedRuntimeAuthority.Runtimes {
			if editedRuntimeAuthority.Runtimes[i].RuntimeID != "candle_metal" {
				continue
			}
			for j := range editedRuntimeAuthority.Runtimes[i].Cells {
				if editedRuntimeAuthority.Runtimes[i].Cells[j].ID == fixture.runtimeCellID {
					profileIndex, cellIndex = i, j
					break
				}
			}
		}
		if profileIndex < 0 || cellIndex < 0 {
			t.Fatalf("synthetic publication authority cannot find runtime cell %s", fixture.runtimeCellID)
		}
		identity := ReceiptIdentity{
			SourceCommit:        IdentitySlotValue(commit),
			BuildDigest:         IdentitySlotNA("synthetic pricing publication unit-test fixture; no release binary"),
			ModelArtifactDigest: IdentitySlotValue(fixture.modelDigest),
			ImageDigest:         IdentitySlotNA("in-process Go unit-test fixture; no container image"),
			HarnessRevision:     IdentitySlotValue("pricing_publication_test_helpers_test.go/v2"),
			CorpusDigest:        IdentitySlotNA("fixed synthetic throughput values; no external corpus"),
			ExactConfig:         IdentitySlotValue("test-only exact settlement-unit throughput authority"),
			RawSamples:          IdentitySlotValue(fmt.Sprintf("%s_per_sec=%.14g", fixture.unitScope, fixture.receiptValue)),
		}
		measuredAt := catalogueThroughputNow().UTC().Format(time.RFC3339)
		receipt := map[string]any{
			"schema_version": 1,
			"kind":           "synthetic_catalogue_publication_throughput_authority_test_fixture",
			"hardware_class": publicationIdentityHWClass,
			"hardware":       map[string]any{"gpu": publicationIdentityHardware},
			"raw_measurement": map[string]any{
				"build_hash":            publicationIdentityBuildHash,
				"build_identity_policy": currentEngineBuildIdentityPolicy,
			},
			"measured_at":        measuredAt,
			"freshness_policy":   catalogueThroughputFreshnessPolicy,
			"merc_source_commit": commit,
			"binding_status":     BindingBound,
			"producer_identity":  identity,
			fixture.jobType: map[string]any{
				"model":                        fixture.modelID,
				"runtime_cell_id":              fixture.runtimeCellID,
				"runtime_profile_id":           "candle_metal",
				"profile_revision":             "r9",
				"engine":                       "candle",
				"engine_revision":              "",
				"engine_build_hash":            publicationIdentityBuildHash,
				"engine_build_identity_policy": currentEngineBuildIdentityPolicy,
				"hardware_identity":            publicationIdentityHardware,
				"model_artifact_digest":        fixture.modelDigest,
				"unit":                         fixture.unit,
				"unit_scope":                   fixture.unitScope,
				"throughput_units_per_second":  fixture.receiptValue,
				"thermal_ok":                   true,
			},
		}
		if mutateThroughput != nil {
			mutateThroughput(fixture.jobType, receipt)
		}
		raw, err := json.MarshalIndent(receipt, "", "  ")
		mustf(t, err, "marshal synthetic %s pricing authority: %v", fixture.jobType, err)
		path := filepath.Join(t.TempDir(), "bound-catalogue-throughput-"+fixture.jobType+".json")
		mustf(t, os.WriteFile(path, append(raw, '\n'), 0o600),
			"write synthetic %s pricing authority: %v", fixture.jobType)
		syntheticSummary := benchmarkReceiptSummary{
			RuntimeProfileIDs: []string{"candle_metal"},
			ModelIDs:          []string{fixture.modelID}, ThroughputMeasured: true,
			ByteDeterministic: true, MeasuredAt: measuredAt,
			HWClass: publicationIdentityHWClass,
			Throughput: map[string]benchmarkThroughput{"candle_metal": {
				Unit: fixture.unit, UnitScope: fixture.unitScope,
				Precision: "TEST_ONLY exact publication mechanics", OperatingBatch: 1,
				UnitsPerSecAtOperatingBatch: fixture.receiptValue,
				BestObservedUnitsPerSec:     fixture.receiptValue, BestObservedBatch: 1,
				Basis: "TEST_ONLY exact selected-cell publication authority",
			}},
			Validity: authorityValidityValid, BindingStatus: BindingBound,
			MercSourceCommit: commit, ProfileRevision: "r9",
			Harness:                   "pricing_publication_test_helpers_test.go/v3 TEST_ONLY",
			ModelArtifactSHA256s:      []string{fixture.modelDigest},
			EngineBuildHash:           publicationIdentityBuildHash,
			EngineBuildIdentityPolicy: currentEngineBuildIdentityPolicy,
			HardwareIdentity:          publicationIdentityHardware,
		}
		benchmarkAuthorityManifest[path] = syntheticSummary
		installedBenchmarkAuthorities = append(installedBenchmarkAuthorities, path)
		editedRuntimeAuthority.Runtimes[profileIndex].Cells[cellIndex].BenchmarkAuthority = path
		// Production keeps embed CANARY so the sold surface stays infer-only
		// (different execution identity than r6). Mechanism tests that install
		// this fixture need the cell ACTIVE to exercise ordinary admission.
		editedRuntimeAuthority.Runtimes[profileIndex].Cells[cellIndex].Lifecycle = runtimeLifecycleActive
		repricingBenchmarks = append(repricingBenchmarks, measuredThroughput{
			ModelID:                   fixture.modelID,
			ModelArtifactDigest:       fixture.modelDigest,
			JobType:                   fixture.jobType,
			RuntimeCellID:             fixture.runtimeCellID,
			RuntimeProfileID:          "candle_metal",
			ProfileRevision:           "r9",
			Engine:                    "candle",
			EngineBuildHash:           publicationIdentityBuildHash,
			EngineBuildIdentityPolicy: currentEngineBuildIdentityPolicy,
			HardwareIdentity:          publicationIdentityHardware,
			Unit:                      fixture.unit,
			UnitScope:                 fixture.unitScope,
			UnitsPerSec:               fixture.unitsPerSec,
			HWClass:                   publicationIdentityHWClass,
			SourceCitation:            path + "#" + fixture.jobType,
		})
	}
	runtimeAuthority = editedRuntimeAuthority

	powerIdentity := ReceiptIdentity{
		SourceCommit:        IdentitySlotValue(commit),
		BuildDigest:         IdentitySlotNA("synthetic pricing power unit-test fixture; no release binary"),
		ModelArtifactDigest: IdentitySlotNA("whole-package power measurement is hardware authority, not model throughput authority"),
		ImageDigest:         IdentitySlotNA("in-process Go unit-test fixture; no container image"),
		HarnessRevision:     IdentitySlotValue("pricing_publication_test_helpers_test.go/power-v1"),
		CorpusDigest:        IdentitySlotNA("fixed synthetic power sample; no external corpus"),
		ExactConfig:         IdentitySlotValue("TEST_ONLY apple_silicon_pro whole-package sustained power"),
		RawSamples:          IdentitySlotValue("TEST_ONLY sustained_watts=30.0"),
	}
	powerReceipt := map[string]any{
		"schema_version": 1,
		"kind":           "synthetic_catalogue_publication_power_authority_test_fixture",
		"hardware_class": publicationIdentityHWClass,
		"hardware":       map[string]any{"gpu": publicationIdentityHardware},
		"raw_measurement": map[string]any{
			"build_hash":            publicationIdentityBuildHash,
			"build_identity_policy": currentEngineBuildIdentityPolicy,
		},
		"merc_source_commit": commit,
		"binding_status":     BindingBound,
		"producer_identity":  powerIdentity,
		testOnlyPowerReceiptFragment: map[string]any{
			"hardware_class":               publicationIdentityHWClass,
			"engine_build_hash":            publicationIdentityBuildHash,
			"engine_build_identity_policy": currentEngineBuildIdentityPolicy,
			"hardware_identity":            publicationIdentityHardware,
			"measurement_status":           string(wattKindMeasured),
			"measurement_boundary":         "whole_package",
			"workload_class":               "inference_shaped",
			"unit":                         "watts",
			"authority_scope":              cataloguePowerAuthorityScope,
			"aggregation":                  cataloguePowerAggregation,
			"operating_protocol":           cataloguePowerOperatingProtocol,
			"covered_workloads": []any{
				map[string]any{
					"model_id": "all-minilm-l6-v2", "job_type": "embed",
					"model_artifact_digest": testOnlyEmbedModelArtifactDigest,
					"runtime_cell_id":       "candle-metal-minilm-embed", "runtime_profile_id": "candle_metal",
					"engine": "candle", "engine_build_hash": publicationIdentityBuildHash,
					"engine_build_identity_policy": currentEngineBuildIdentityPolicy,
					"hardware_identity":            publicationIdentityHardware,
				},
				map[string]any{
					"model_id": "llama-3.2-1b-instruct-q4", "job_type": "batch_infer",
					"model_artifact_digest": testOnlyBatchModelArtifactDigest,
					"runtime_cell_id":       "candle-metal-llama1-infer", "runtime_profile_id": "candle_metal",
					"engine": "candle", "engine_build_hash": publicationIdentityBuildHash,
					"engine_build_identity_policy": currentEngineBuildIdentityPolicy,
					"hardware_identity":            publicationIdentityHardware,
				},
			},
			"sustained_watts":  30.0,
			"measured_at":      cataloguePowerNow().UTC().Format(time.RFC3339),
			"freshness_policy": cataloguePowerFreshnessPolicy,
		},
	}
	if mutatePower != nil {
		mutatePower(powerReceipt)
	}
	powerRaw, err := json.MarshalIndent(powerReceipt, "", "  ")
	mustf(t, err, "marshal synthetic power authority: %v")
	powerBytes := append(powerRaw, '\n')
	powerPath := filepath.Join(t.TempDir(), "bound-catalogue-power.json")
	mustf(t, os.WriteFile(powerPath, powerBytes, 0o600), "write synthetic power authority: %v")
	powerDigest := sha256.Sum256(powerBytes)
	sustainedWattsByHWClass[publicationIdentityHWClass] = wattsMeasured(
		30,
		powerPath+"#"+testOnlyPowerReceiptFragment,
		fmt.Sprintf("%x", powerDigest),
	)
	// Activation caches resolved advertised/directed projections. Store startup
	// honestly quarantines the checked-in zero-lane document; preserving that
	// overlay would keep these explicitly synthetic publication cells
	// quarantined as well. Give the TEST_ONLY document an empty lifecycle
	// overlay at the same revision (or documentActivation when none was loaded),
	// then restore the exact prior pointer at cleanup — matching
	// installTestOnlyCombinedTokenAuthority. Production-path quarantine tests
	// never call this helper.
	if previousActivation == nil {
		activeRuntimeActivation.Store(documentActivation())
	} else {
		activeRuntimeActivation.Store(newRuntimeActivation(
			previousActivation.PolicyRevision, map[string]string{}, nil))
	}
}
