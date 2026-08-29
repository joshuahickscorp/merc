package main

import (
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestCatalogueThroughputRefusesDifferentBoundReceiptForSameCell(t *testing.T) {
	installBoundCataloguePublicationAuthorityForTest(t)
	row := repricingBenchmarks[0]
	path, fragment, _ := strings.Cut(row.SourceCitation, "#")
	raw, err := os.ReadFile(path)
	must(t, err)
	var receipt map[string]any
	must(t, json.Unmarshal(raw, &receipt))
	section := receipt[fragment].(map[string]any)
	section["throughput_units_per_second"] = row.UnitsPerSec / 2
	mutated, err := json.MarshalIndent(receipt, "", "  ")
	must(t, err)
	alternate := filepath.Join(t.TempDir(), "separately-bound-same-cell.json")
	must(t, os.WriteFile(alternate, append(mutated, '\n'), 0o600))
	row.SourceCitation = alternate + "#" + fragment
	row.UnitsPerSec /= 2
	if err := validateRepricingBenchmarkCitation(row); err == nil ||
		!strings.Contains(err.Error(), "not exact selected cell") {
		t.Fatalf("separately BOUND same-cell receipt error=%v", err)
	}
}

func TestCataloguePowerRequiresExactRuntimeBuildAndDeviceCoverage(t *testing.T) {
	for _, tc := range []struct {
		name   string
		mutate func(map[string]any)
		want   string
	}{
		{"wrong root build", func(receipt map[string]any) {
			receipt["raw_measurement"].(map[string]any)["build_hash"] = "0000000000000000"
		}, "does not equal selected cell build"},
		{"wrong root device", func(receipt map[string]any) {
			receipt["hardware"].(map[string]any)["gpu"] = "TEST_ONLY Apple M1 Pro"
		}, "does not equal selected cell device"},
		{"wrong section build", func(receipt map[string]any) {
			receipt[testOnlyPowerReceiptFragment].(map[string]any)["engine_build_hash"] = "0000000000000000"
		}, "section engine build"},
		{"wrong covered cell", func(receipt map[string]any) {
			coverage := receipt[testOnlyPowerReceiptFragment].(map[string]any)["covered_workloads"].([]any)
			coverage[0].(map[string]any)["runtime_cell_id"] = "some-other-cell"
		}, "does not equal exact selected runtime"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			installBoundCataloguePowerAuthorityWithMutationForTest(t, tc.mutate)
			if err := governPublishedPrice(repricingBenchmarks[0], 1, 0.5); err == nil ||
				!strings.Contains(err.Error(), tc.want) {
				t.Fatalf("exact power identity error=%v, want %q", err, tc.want)
			}
		})
	}
}

func TestPersistedCatalogueRefusesDifferentValidPowerAuthority(t *testing.T) {
	installBoundCataloguePublicationAuthorityForTest(t)
	pinBoardClockForPublication(t)
	schedule, err := BuildCataloguePriceSchedule()
	must(t, err)
	power := schedule.Results[0].PhysicalAuthority.Power
	path, fragment, _ := strings.Cut(power.Citation, "#")
	raw, err := os.ReadFile(path)
	must(t, err)
	var receipt map[string]any
	must(t, json.Unmarshal(raw, &receipt))
	receipt[fragment].(map[string]any)["sustained_watts"] = 31.0
	mutated, err := json.MarshalIndent(receipt, "", "  ")
	must(t, err)
	bytes := append(mutated, '\n')
	alternate := filepath.Join(t.TempDir(), "alternate-valid-power.json")
	must(t, os.WriteFile(alternate, bytes, 0o600))
	digest := sha256.Sum256(bytes)
	sustainedWattsByHWClass[schedule.Results[0].PhysicalAuthority.HWClass] = wattsMeasured(
		31, alternate+"#"+fragment, fmt.Sprintf("%x", digest))
	if err := revalidateCataloguePriceScheduleCurrent(schedule); err == nil ||
		(!strings.Contains(err.Error(), "no longer equals current cited bytes") &&
			!strings.Contains(err.Error(), "sole current governed derivation")) {
		t.Fatalf("persisted schedule under different valid power authority error=%v", err)
	}
}

func TestProductionThroughputCitationsBindCurrentSettlementGeometry(t *testing.T) {
	// G070: production carries one BOUND batch_infer lane under
	// tokens/token_like_input_plus_max_output_tokens. Media and embed remain
	// unpriced, not in repricingBenchmarks.
	if err := validateAllRepricingBenchmarkCitations(); err != nil {
		t.Fatalf("production throughput citations must bind: %v", err)
	}
	if len(repricingBenchmarks) != 1 {
		t.Fatalf("want exactly one production priced lane, got %d", len(repricingBenchmarks))
	}
	b := repricingBenchmarks[0]
	if b.ModelID != "llama-3.2-1b-instruct-q4" || b.JobType != "batch_infer" {
		t.Fatalf("unexpected production priced lane %+v", b)
	}
	settlement, ok := currentSettlementAuthorityForJobType(b.JobType)
	if !ok || b.Unit != settlement.Unit || b.UnitScope != settlement.Scope {
		t.Fatalf("production lane unit/scope %q/%q != settlement %q/%q",
			b.Unit, b.UnitScope, settlement.Unit, settlement.Scope)
	}
	for _, b := range repricingBenchmarks {
		if b.ModelID == "ffmpeg-transcode-v1" || b.ModelID == "svg-scene-render-v1" ||
			b.ModelID == "all-minilm-l6-v2" {
			t.Fatalf("%s must not set a catalogue price until its cited artifact binds under settlement geometry", b.ModelID)
		}
	}
}

func TestProductionCatalogueScheduleAcceptsVendorWallUpperBound(t *testing.T) {
	// Throughput is BOUND; apple_silicon_ultra power is a conservative
	// VENDOR_WALL_UPPER_BOUND (270 W), which satisfies ECONOMIC_POWER_ENVELOPE
	// and unblocks catalogue publication. It is not MEASURED.
	schedule, err := BuildCataloguePriceSchedule()
	if err != nil {
		t.Fatalf("production catalogue publication error=%v", err)
	}
	if len(schedule.Results) < 1 {
		t.Fatal("production catalogue published no lanes")
	}
	power := schedule.Results[0].PhysicalAuthority.Power
	if power.SourceClass != string(wattKindVendorWallUpperBound) {
		t.Fatalf("source_class=%q, want VENDOR_WALL_UPPER_BOUND", power.SourceClass)
	}
	if power.Watts != appleMacStudio2025M3UltraWallMaxWatts || power.WattsUpperBound != appleMacStudio2025M3UltraWallMaxWatts {
		t.Fatalf("vendor-wall watts=%v upper=%v, want 270", power.Watts, power.WattsUpperBound)
	}
}

func TestProductionCatalogueScheduleRefusesAssumedPower(t *testing.T) {
	// The production ultra row is a vendor-wall bound. Overwrite it with
	// ASSUMED: catalogue publication must still fail closed.
	previous := sustainedWattsByHWClass["apple_silicon_ultra"]
	t.Cleanup(func() { sustainedWattsByHWClass["apple_silicon_ultra"] = previous })
	sustainedWattsByHWClass["apple_silicon_ultra"] = wattsAssumed(
		65,
		"ASSUMED only to prove that catalogue publication refuses it",
	)
	_, err := BuildCataloguePriceSchedule()
	if err == nil || !strings.Contains(err.Error(), "requires MEASURED or VENDOR_WALL_UPPER_BOUND sustained watts") ||
		!strings.Contains(err.Error(), "apple_silicon_ultra") {
		t.Fatalf("production catalogue publication error=%v", err)
	}
}

func TestBoundTestPublicationAuthorityCitationsBind(t *testing.T) {
	installBoundCataloguePublicationAuthorityForTest(t)
	mustf(t, validateAllRepricingBenchmarkCitations(), "BOUND test throughput citations must bind: %v")
	for _, b := range repricingBenchmarks {
		settlement, ok := currentSettlementAuthorityForJobType(b.JobType)
		if !ok || b.Unit != settlement.Unit || b.UnitScope != settlement.Scope {
			t.Fatalf("%s/%s TEST_ONLY throughput authority=%q/%q settlement=%q/%q",
				b.ModelID, b.JobType, b.Unit, b.UnitScope, settlement.Unit, settlement.Scope)
		}
	}
}

func TestPricingThroughputReceiptRequiresExplicitModelIdentity(t *testing.T) {
	installBoundCataloguePublicationAuthorityWithMutationForTest(t, func(jobType string, receipt map[string]any) {
		if jobType == "embed" {
			delete(receipt["embed"].(map[string]any), "model")
		}
	})
	err := validateRepricingBenchmarkCitation(repricingBenchmarks[0])
	if err == nil || !strings.Contains(err.Error(), "requires an explicit model identity") {
		t.Fatalf("pricing receipt without model identity error=%v", err)
	}
}

func TestPricingThroughputReceiptRequiresExactCurrentRuntimeCellIdentity(t *testing.T) {
	tests := []struct {
		field string
		value string
	}{
		{field: "runtime_cell_id", value: "candle-metal-llama1-infer"},
		{field: "runtime_profile_id", value: "llama_cpp_metal"},
		{field: "profile_revision", value: "r8"},
		{field: "engine", value: "llama.cpp"},
		{field: "engine_revision", value: "forged"},
	}
	for _, tc := range tests {
		t.Run(tc.field, func(t *testing.T) {
			installBoundCataloguePublicationAuthorityWithMutationForTest(t, func(jobType string, receipt map[string]any) {
				if jobType == "embed" {
					receipt["embed"].(map[string]any)[tc.field] = tc.value
				}
			})
			err := validateRepricingBenchmarkCitation(repricingBenchmarks[0])
			if err == nil || !strings.Contains(err.Error(), "pricing throughput "+tc.field) {
				t.Fatalf("pricing receipt with wrong %s error=%v", tc.field, err)
			}
		})
	}
}

func TestPricingThroughputReceiptModelMustExactlyMatchRow(t *testing.T) {
	installBoundCataloguePublicationAuthorityWithMutationForTest(t, func(jobType string, receipt map[string]any) {
		if jobType == "embed" {
			receipt["embed"].(map[string]any)["model"] = "llama-3.2-1b-instruct-q4"
		}
	})
	err := validateRepricingBenchmarkCitation(repricingBenchmarks[0])
	if err == nil || !strings.Contains(err.Error(), "constant claims model") {
		t.Fatalf("pricing receipt with mismatched model identity error=%v", err)
	}
}

func TestPricingThroughputProducerModelDigestCannotBeNA(t *testing.T) {
	installBoundCataloguePublicationAuthorityWithMutationForTest(t, func(jobType string, receipt map[string]any) {
		if jobType != "embed" {
			return
		}
		identity := receipt["producer_identity"].(ReceiptIdentity)
		identity.ModelArtifactDigest = IdentitySlotNA("TEST_ONLY adversarial N/A")
		receipt["producer_identity"] = identity
	})
	err := validateRepricingBenchmarkCitation(repricingBenchmarks[0])
	if err == nil || !strings.Contains(err.Error(), "requires an actual lowercase sha256 value") {
		t.Fatalf("pricing receipt with N/A producer model digest error=%v", err)
	}
}

func TestPricingThroughputReceiptRequiresPinnedSectionModelDigest(t *testing.T) {
	installBoundCataloguePublicationAuthorityWithMutationForTest(t, func(jobType string, receipt map[string]any) {
		if jobType == "embed" {
			delete(receipt["embed"].(map[string]any), "model_artifact_digest")
		}
	})
	err := validateRepricingBenchmarkCitation(repricingBenchmarks[0])
	if err == nil || !strings.Contains(err.Error(), "receipt section \"embed\" requires an exact lowercase sha256 model_artifact_digest") {
		t.Fatalf("pricing receipt without section model digest error=%v", err)
	}
}

func TestPricingThroughputModelDigestMustMatchAcrossRowSectionAndProducer(t *testing.T) {
	installBoundCataloguePublicationAuthorityWithMutationForTest(t, func(jobType string, receipt map[string]any) {
		if jobType == "embed" {
			receipt["embed"].(map[string]any)["model_artifact_digest"] = strings.Repeat("b", 64)
		}
	})
	err := validateRepricingBenchmarkCitation(repricingBenchmarks[0])
	if err == nil || !strings.Contains(err.Error(), "model_artifact_digest mismatch") {
		t.Fatalf("pricing receipt with mismatched model digest error=%v", err)
	}
}

func TestPricingThroughputModelDigestMustMatchCanonicalRuntimeAuthority(t *testing.T) {
	arbitraryDigest := strings.Repeat("a", 64)
	installBoundCataloguePublicationAuthorityWithMutationForTest(t, func(jobType string, receipt map[string]any) {
		if jobType != "embed" {
			return
		}
		identity := receipt["producer_identity"].(ReceiptIdentity)
		identity.ModelArtifactDigest = IdentitySlotValue(arbitraryDigest)
		receipt["producer_identity"] = identity
		receipt["embed"].(map[string]any)["model_artifact_digest"] = arbitraryDigest
	})
	repricingBenchmarks[0].ModelArtifactDigest = arbitraryDigest
	err := validateRepricingBenchmarkCitation(repricingBenchmarks[0])
	if err == nil || !strings.Contains(err.Error(), "does not match a canonical weight artifact pinned by runtime authority") {
		t.Fatalf("pricing receipt with self-consistent but arbitrary model digest error=%v", err)
	}
}

func TestPricingThroughputModelDigestMustMatchExactCellWireKind(t *testing.T) {
	const siblingGGUFDigest = "797b70c4edf85907fe0a49eb85811256f65fa0f7bf52166b147fd16be2be4662"
	installBoundCataloguePublicationAuthorityWithMutationForTest(t, func(jobType string, receipt map[string]any) {
		if jobType != "embed" {
			return
		}
		identity := receipt["producer_identity"].(ReceiptIdentity)
		identity.ModelArtifactDigest = IdentitySlotValue(siblingGGUFDigest)
		receipt["producer_identity"] = identity
		receipt["embed"].(map[string]any)["model_artifact_digest"] = siblingGGUFDigest
	})
	repricingBenchmarks[0].ModelArtifactDigest = siblingGGUFDigest
	err := validateRepricingBenchmarkCitation(repricingBenchmarks[0])
	if err == nil || !strings.Contains(err.Error(), "exact cell candle-metal-minilm-embed wire kind hf") {
		t.Fatalf("Candle pricing receipt accepted sibling canonical GGUF weights: %v", err)
	}
}

func TestPricingThroughputReceiptRequiresTimestamp(t *testing.T) {
	installBoundCataloguePublicationAuthorityWithMutationForTest(t, func(jobType string, receipt map[string]any) {
		if jobType == "embed" {
			delete(receipt, "measured_at")
		}
	})
	err := validateRepricingBenchmarkCitation(repricingBenchmarks[0])
	if err == nil || !strings.Contains(err.Error(), "requires an explicit RFC3339 measured_at") {
		t.Fatalf("pricing throughput receipt without timestamp error=%v", err)
	}
}

func TestPricingThroughputReceiptRefusesFutureTimestamp(t *testing.T) {
	installBoundCataloguePublicationAuthorityWithMutationForTest(t, func(jobType string, receipt map[string]any) {
		if jobType == "embed" {
			receipt["measured_at"] = catalogueThroughputNow().Add(time.Hour).UTC().Format(time.RFC3339)
		}
	})
	err := validateRepricingBenchmarkCitation(repricingBenchmarks[0])
	if err == nil || !strings.Contains(err.Error(), "is future-dated under "+catalogueThroughputFreshnessPolicy) {
		t.Fatalf("pricing throughput receipt with future timestamp error=%v", err)
	}
}

func TestPricingThroughputReceiptRefusesStaleTimestamp(t *testing.T) {
	installBoundCataloguePublicationAuthorityWithMutationForTest(t, func(jobType string, receipt map[string]any) {
		if jobType == "embed" {
			receipt["measured_at"] = catalogueThroughputNow().
				Add(-catalogueThroughputMaxAge - time.Hour).UTC().Format(time.RFC3339)
		}
	})
	err := validateRepricingBenchmarkCitation(repricingBenchmarks[0])
	if err == nil || !strings.Contains(err.Error(), "is stale under "+catalogueThroughputFreshnessPolicy) {
		t.Fatalf("pricing throughput receipt with stale timestamp error=%v", err)
	}
}

func TestPricingThroughputFreshnessReturnsExactValidityBoundary(t *testing.T) {
	measuredAt := time.Date(2026, 8, 9, 12, 0, 0, 0, time.UTC)
	validUntil, err := pricingThroughputValidUntil(map[string]any{
		"freshness_policy": catalogueThroughputFreshnessPolicy,
		"measured_at":      measuredAt.Format(time.RFC3339),
	}, measuredAt.Add(time.Hour))
	mustf(t, err, "validate throughput freshness: %v")
	want := measuredAt.Add(benchmarkRevalidationWindow)
	if !validUntil.Equal(want) {
		t.Fatalf("throughput valid_until=%s want=%s", validUntil, want)
	}
}

func TestPricingThroughputAuthorityRequiresExplicitUnitScope(t *testing.T) {
	installBoundCataloguePublicationAuthorityForTest(t)
	b := repricingBenchmarks[0]
	b.UnitScope = ""
	err := validateRepricingBenchmarkCitation(b)
	if err == nil || !strings.Contains(err.Error(), "requires explicit unit and unit_scope") {
		t.Fatalf("pricing throughput without declared unit_scope error=%v", err)
	}
}

func TestPricingThroughputReceiptRequiresExplicitUnitScope(t *testing.T) {
	installBoundCataloguePublicationAuthorityWithMutationForTest(t, func(jobType string, receipt map[string]any) {
		if jobType == "embed" {
			delete(receipt["embed"].(map[string]any), "unit_scope")
		}
	})
	err := validateRepricingBenchmarkCitation(repricingBenchmarks[0])
	if err == nil || !strings.Contains(err.Error(), "receipt section \"embed\" requires explicit unit and unit_scope") {
		t.Fatalf("pricing receipt without unit_scope error=%v", err)
	}
}

func TestPricingThroughputReceiptMustMatchDeclaredUnitScope(t *testing.T) {
	installBoundCataloguePublicationAuthorityWithMutationForTest(t, func(jobType string, receipt map[string]any) {
		if jobType == "embed" {
			receipt["embed"].(map[string]any)["unit_scope"] = performanceUnitScopeCompletedEmbeddingRecords
		}
	})
	err := validateRepricingBenchmarkCitation(repricingBenchmarks[0])
	if err == nil || !strings.Contains(err.Error(), "receipt measured") ||
		!strings.Contains(err.Error(), performanceUnitScopeCompletedEmbeddingRecords) {
		t.Fatalf("pricing receipt/declaration scope mismatch error=%v", err)
	}
}

func TestPricingThroughputAuthorityMustMatchCurrentSettlementUnitScope(t *testing.T) {
	installBoundCataloguePublicationAuthorityWithMutationForTest(t, func(jobType string, receipt map[string]any) {
		if jobType != "embed" {
			return
		}
		section := receipt["embed"].(map[string]any)
		section["unit"] = "embeddings"
		section["unit_scope"] = performanceUnitScopeCompletedEmbeddingRecords
	})
	repricingBenchmarks[0].Unit = "embeddings"
	repricingBenchmarks[0].UnitScope = performanceUnitScopeCompletedEmbeddingRecords
	_, err := BuildCataloguePriceSchedule()
	if err == nil || !strings.Contains(err.Error(), "incompatible with current settlement authority") ||
		!strings.Contains(err.Error(), performanceUnitScopeCompletedEmbeddingRecords) ||
		!strings.Contains(err.Error(), performanceUnitScopeTokenLikeInputGeometry) {
		t.Fatalf("pricing/settlement semantic mismatch error=%v", err)
	}
}

func TestPricingCitationBoundLabelDoesNotReplaceCompleteIdentity(t *testing.T) {
	err := ensurePricingCitationBindable("test.json", map[string]any{
		"binding_status": BindingBound,
	})
	if err == nil || !strings.Contains(err.Error(), "producer_identity is missing") {
		t.Fatalf("BOUND label without producer identity error=%v", err)
	}
}

func TestPricingCitationRequiresRealProducerSourceCommit(t *testing.T) {
	identity := map[string]any{
		"source_commit":         map[string]any{"value": "not-a-real-commit"},
		"build_digest":          map[string]any{"na": "test"},
		"model_artifact_digest": map[string]any{"na": "test"},
		"image_digest":          map[string]any{"na": "test"},
		"harness_revision":      map[string]any{"value": "test"},
		"corpus_digest":         map[string]any{"na": "test"},
		"exact_config":          map[string]any{"value": "test"},
		"raw_samples":           map[string]any{"value": "test"},
	}
	err := ensurePricingCitationBindable("test.json", map[string]any{
		"binding_status":    BindingBound,
		"producer_identity": identity,
	})
	if err == nil || !strings.Contains(err.Error(), "not a git object") {
		t.Fatalf("BOUND citation with free-string source commit error=%v", err)
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
		// The `err == nil` check above is the assertion that matters: a quarantined
		// row must still be refused. This list only guards that it is refused for a
		// reason about authority rather than by accident. "is not exact selected
		// cell" joined the list when the embed cell gained a real sealed identity
		// (r3, engine_build_hash 2939a8e26ffe6fd2): the citation now points at a
		// different artifact than the selected cell's authority, which is a
		// stricter refusal than "unbindable", not a weaker one.
		if !strings.Contains(err.Error(), "unbindable") &&
			!strings.Contains(err.Error(), "not current-bindable") &&
			!strings.Contains(err.Error(), "not a git object") &&
			!strings.Contains(err.Error(), "is not exact selected cell") &&
			!strings.Contains(err.Error(), "lacks exact canonical build/device identity") {
			t.Fatalf("%s/%s refusal should name unbindable artifact or current identity refusal, got: %v",
				b.ModelID, b.JobType, err)
		}
		t.Logf("refused as expected: %v", err)
	}
}

func TestCatalogueScheduleRefusesUnbindableCitation(t *testing.T) {
	// Capture the production ffmpeg quarantine row BEFORE the synthetic
	// publication helper clears unpricedThroughputUntilBound for its install.
	var unbindable measuredThroughput
	for _, candidate := range unpricedThroughputUntilBound {
		if candidate.ModelID == "ffmpeg-transcode-v1" && candidate.JobType == "media_transcode" {
			unbindable = candidate
			break
		}
	}
	if unbindable.RuntimeCellID == "" {
		t.Fatal("missing quarantined ffmpeg throughput fixture")
	}

	installBoundCataloguePublicationAuthorityForTest(t)
	// Inject an unbindable row into the priced set only for this test.
	saved := append([]measuredThroughput(nil), repricingBenchmarks...)
	t.Cleanup(func() { repricingBenchmarks = saved })
	repricingBenchmarks = append(repricingBenchmarks, unbindable)
	// Keep quarantine empty for this test so dual-membership does not fire
	// first; we want the bind failure itself. The publication helper already
	// nil'd unpriced and restores production quarantine on cleanup.
	unpricedThroughputUntilBound = nil

	pinBoardClockForPublication(t)
	_, err := BuildCataloguePriceSchedule()
	if err == nil {
		t.Fatal("BuildCataloguePriceSchedule accepted a price row citing an unbindable artifact")
	}
	if !strings.Contains(err.Error(), "unbindable") &&
		!strings.Contains(err.Error(), "not a git object") &&
		!strings.Contains(err.Error(), "lacks exact canonical build/device identity") {
		t.Fatalf("want unbindable/git-object/identity refusal, got: %v", err)
	}
}

func TestCataloguePublicationRefusesAssumedWatts(t *testing.T) {
	installBoundCataloguePublicationAuthorityForTest(t)
	sustainedWattsByHWClass["apple_silicon_pro"] = wattsAssumed(
		30,
		"ASSUMED only to prove that catalogue publication refuses it",
	)
	pinBoardClockForPublication(t)
	_, err := BuildCataloguePriceSchedule()
	if err == nil || !strings.Contains(err.Error(), "requires MEASURED or VENDOR_WALL_UPPER_BOUND sustained watts") {
		t.Fatalf("catalogue publication with ASSUMED watts error=%v", err)
	}
}

func TestCataloguePublicationRefusesArbitraryMeasuredPowerProvenance(t *testing.T) {
	installBoundCataloguePublicationAuthorityForTest(t)
	sustainedWattsByHWClass["apple_silicon_pro"] = wattsMeasured(
		30,
		"operator says this is measured",
		strings.Repeat("a", 64),
	)
	err := governPublishedPrice(repricingBenchmarks[0], 1, 0.5)
	if err == nil || !strings.Contains(err.Error(), "requires a cited receipt section") {
		t.Fatalf("catalogue publication with arbitrary MEASURED provenance error=%v", err)
	}
}

func TestCataloguePublicationRefusesMeasuredPowerWithoutPinnedReceiptDigest(t *testing.T) {
	installBoundCataloguePublicationAuthorityForTest(t)
	valid := sustainedWattsByHWClass["apple_silicon_pro"]
	sustainedWattsByHWClass["apple_silicon_pro"] = wattsMeasured(30, valid.Provenance(), "")
	err := governPublishedPrice(repricingBenchmarks[0], 1, 0.5)
	if err == nil || !strings.Contains(err.Error(), "requires a pinned lowercase sha256 receipt digest") {
		t.Fatalf("catalogue publication with unpinned MEASURED power receipt error=%v", err)
	}
}

func TestCataloguePublicationRefusesMeasuredPowerReceiptDigestMismatch(t *testing.T) {
	installBoundCataloguePublicationAuthorityForTest(t)
	valid := sustainedWattsByHWClass["apple_silicon_pro"]
	sustainedWattsByHWClass["apple_silicon_pro"] = wattsMeasured(
		valid.Watts(), valid.Provenance(), strings.Repeat("a", 64),
	)
	err := governPublishedPrice(repricingBenchmarks[0], 1, 0.5)
	if err == nil || !strings.Contains(err.Error(), "power receipt digest mismatch") {
		t.Fatalf("catalogue publication with mismatched power receipt digest error=%v", err)
	}
}

func TestCataloguePublicationRefusesUnboundPowerReceipt(t *testing.T) {
	installBoundCataloguePowerAuthorityWithMutationForTest(t, func(receipt map[string]any) {
		receipt["binding_status"] = BindingUnbound
	})
	err := governPublishedPrice(repricingBenchmarks[0], 1, 0.5)
	if err == nil || !strings.Contains(err.Error(), "binding_status=UNBOUND") {
		t.Fatalf("catalogue publication with UNBOUND power receipt error=%v", err)
	}
}

func TestCataloguePublicationRefusesPowerReceiptWithWrongHardware(t *testing.T) {
	installBoundCataloguePowerAuthorityWithMutationForTest(t, func(receipt map[string]any) {
		receipt[testOnlyPowerReceiptFragment].(map[string]any)["hardware_class"] = "apple_silicon_ultra"
	})
	err := governPublishedPrice(repricingBenchmarks[0], 1, 0.5)
	if err == nil || !strings.Contains(err.Error(), "receipt section measured \"apple_silicon_ultra\"") {
		t.Fatalf("catalogue publication with wrong-HW power receipt error=%v", err)
	}
}

func TestCataloguePublicationRefusesPowerReceiptWithWrongWatts(t *testing.T) {
	installBoundCataloguePowerAuthorityWithMutationForTest(t, func(receipt map[string]any) {
		receipt[testOnlyPowerReceiptFragment].(map[string]any)["sustained_watts"] = 31.0
	})
	err := governPublishedPrice(repricingBenchmarks[0], 1, 0.5)
	if err == nil || !strings.Contains(err.Error(), "claims 30.000000 watts, receipt measured 31") {
		t.Fatalf("catalogue publication with wrong-watts power receipt error=%v", err)
	}
}

func TestCataloguePublicationRefusesPowerReceiptWithoutMeasuredStatus(t *testing.T) {
	installBoundCataloguePowerAuthorityWithMutationForTest(t, func(receipt map[string]any) {
		delete(receipt[testOnlyPowerReceiptFragment].(map[string]any), "measurement_status")
	})
	err := governPublishedPrice(repricingBenchmarks[0], 1, 0.5)
	if err == nil || !strings.Contains(err.Error(), "requires measurement_status=MEASURED") {
		t.Fatalf("catalogue publication with missing power measurement status error=%v", err)
	}
}

func TestCataloguePublicationRefusesPowerReceiptWithoutWholePackageBoundary(t *testing.T) {
	tests := []struct {
		name  string
		value any
	}{
		{name: "missing", value: nil},
		{name: "gpu domain", value: "gpu_device"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			installBoundCataloguePowerAuthorityWithMutationForTest(t, func(receipt map[string]any) {
				section := receipt[testOnlyPowerReceiptFragment].(map[string]any)
				if tc.value == nil {
					delete(section, "measurement_boundary")
				} else {
					section["measurement_boundary"] = tc.value
				}
			})
			err := governPublishedPrice(repricingBenchmarks[0], 1, 0.5)
			if err == nil || !strings.Contains(err.Error(), "requires measurement_boundary=\"whole_package\"") {
				t.Fatalf("catalogue publication with %s boundary error=%v", tc.name, err)
			}
		})
	}
}

func TestCataloguePublicationRefusesPowerReceiptWithoutInferenceShapedLoad(t *testing.T) {
	tests := []struct {
		name  string
		value any
	}{
		{name: "missing", value: nil},
		{name: "idle", value: "idle"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			installBoundCataloguePowerAuthorityWithMutationForTest(t, func(receipt map[string]any) {
				section := receipt[testOnlyPowerReceiptFragment].(map[string]any)
				if tc.value == nil {
					delete(section, "workload_class")
				} else {
					section["workload_class"] = tc.value
				}
			})
			err := governPublishedPrice(repricingBenchmarks[0], 1, 0.5)
			if err == nil || !strings.Contains(err.Error(), "requires workload_class=\"inference_shaped\"") {
				t.Fatalf("catalogue publication with %s workload error=%v", tc.name, err)
			}
		})
	}
}

func TestCataloguePublicationRefusesPowerReceiptWithoutWattsUnit(t *testing.T) {
	tests := []struct {
		name  string
		value any
	}{
		{name: "missing", value: nil},
		{name: "kilowatts", value: "kilowatts"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			installBoundCataloguePowerAuthorityWithMutationForTest(t, func(receipt map[string]any) {
				section := receipt[testOnlyPowerReceiptFragment].(map[string]any)
				if tc.value == nil {
					delete(section, "unit")
				} else {
					section["unit"] = tc.value
				}
			})
			err := governPublishedPrice(repricingBenchmarks[0], 1, 0.5)
			if err == nil || !strings.Contains(err.Error(), "requires unit=\"watts\"") {
				t.Fatalf("catalogue publication with %s unit error=%v", tc.name, err)
			}
		})
	}
}

func TestCataloguePublicationRefusesPowerEnvelopeWithoutGovernedScopeAggregationOrProtocol(t *testing.T) {
	tests := []struct {
		field string
		want  string
	}{
		{field: "authority_scope", want: "requires authority_scope="},
		{field: "aggregation", want: "requires aggregation="},
		{field: "operating_protocol", want: "requires operating_protocol="},
	}
	for _, tc := range tests {
		t.Run(tc.field, func(t *testing.T) {
			installBoundCataloguePowerAuthorityWithMutationForTest(t, func(receipt map[string]any) {
				delete(receipt[testOnlyPowerReceiptFragment].(map[string]any), tc.field)
			})
			err := governPublishedPrice(repricingBenchmarks[0], 1, 0.5)
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("catalogue publication without %s error=%v", tc.field, err)
			}
		})
	}
}

func TestCataloguePublicationPowerEnvelopeMustCoverExactModelJobAndArtifact(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(map[string]any)
		want   string
	}{
		{
			name: "missing target",
			mutate: func(section map[string]any) {
				section["covered_workloads"] = []any{
					map[string]any{
						"model_id": "llama-3.2-1b-instruct-q4", "job_type": "batch_infer",
						"model_artifact_digest": testOnlyBatchModelArtifactDigest,
						"runtime_cell_id":       "candle-metal-llama1-infer", "runtime_profile_id": "candle_metal",
						"engine": "candle", "engine_build_hash": testOnlyPublicationBuildHash,
						"engine_build_identity_policy": currentEngineBuildIdentityPolicy,
						"hardware_identity":            testOnlyPublicationHardware,
					},
				}
			},
			want: "does not cover exact workload",
		},
		{
			name: "co-mutated artifact",
			mutate: func(section map[string]any) {
				coverage := section["covered_workloads"].([]any)
				coverage[0].(map[string]any)["model_artifact_digest"] = strings.Repeat("a", 64)
			},
			want: "outside canonical runtime authority",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			installBoundCataloguePowerAuthorityWithMutationForTest(t, func(receipt map[string]any) {
				tc.mutate(receipt[testOnlyPowerReceiptFragment].(map[string]any))
			})
			err := governPublishedPrice(repricingBenchmarks[0], 1, 0.5)
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("catalogue publication with %s error=%v", tc.name, err)
			}
		})
	}
}

func TestCataloguePublicationPowerCoverageUsesExactCellWireKindArtifact(t *testing.T) {
	const siblingGGUFDigest = "797b70c4edf85907fe0a49eb85811256f65fa0f7bf52166b147fd16be2be4662"
	installBoundCataloguePowerAuthorityWithMutationForTest(t, func(receipt map[string]any) {
		coverage := receipt[testOnlyPowerReceiptFragment].(map[string]any)["covered_workloads"].([]any)
		coverage[0].(map[string]any)["model_artifact_digest"] = siblingGGUFDigest
	})
	repricingBenchmarks[0].ModelArtifactDigest = siblingGGUFDigest
	err := governPublishedPrice(repricingBenchmarks[0], 1, 0.5)
	if err == nil || !strings.Contains(err.Error(), "exact cell candle-metal-minilm-embed wire kind hf") {
		t.Fatalf("power envelope accepted sibling canonical GGUF weights for Candle workload: %v", err)
	}
}

func TestCataloguePublicationRefusesPowerReceiptWithoutTimestamp(t *testing.T) {
	installBoundCataloguePowerAuthorityWithMutationForTest(t, func(receipt map[string]any) {
		delete(receipt[testOnlyPowerReceiptFragment].(map[string]any), "measured_at")
	})
	err := governPublishedPrice(repricingBenchmarks[0], 1, 0.5)
	if err == nil || !strings.Contains(err.Error(), "requires an explicit RFC3339 measured_at") {
		t.Fatalf("catalogue publication with missing power timestamp error=%v", err)
	}
}

func TestCataloguePublicationRefusesStalePowerReceipt(t *testing.T) {
	installBoundCataloguePowerAuthorityWithMutationForTest(t, func(receipt map[string]any) {
		receipt[testOnlyPowerReceiptFragment].(map[string]any)["measured_at"] = cataloguePowerNow().
			Add(-cataloguePowerMaxAge - time.Hour).UTC().Format(time.RFC3339)
	})
	err := governPublishedPrice(repricingBenchmarks[0], 1, 0.5)
	if err == nil || !strings.Contains(err.Error(), "is stale under "+cataloguePowerFreshnessPolicy) {
		t.Fatalf("catalogue publication with stale power receipt error=%v", err)
	}
}

func TestCataloguePublicationRefusesFutureDatedPowerReceipt(t *testing.T) {
	installBoundCataloguePowerAuthorityWithMutationForTest(t, func(receipt map[string]any) {
		receipt[testOnlyPowerReceiptFragment].(map[string]any)["measured_at"] = cataloguePowerNow().
			Add(time.Hour).UTC().Format(time.RFC3339)
	})
	err := governPublishedPrice(repricingBenchmarks[0], 1, 0.5)
	if err == nil || !strings.Contains(err.Error(), "is future-dated under "+cataloguePowerFreshnessPolicy) {
		t.Fatalf("catalogue publication with future-dated power receipt error=%v", err)
	}
}

func TestCataloguePublicationRefusesUnknownWattsClass(t *testing.T) {
	b := measuredThroughput{
		ModelID: "test-model", JobType: "embed",
		Unit: "token_like_input_units", UnitScope: performanceUnitScopeTokenLikeInputGeometry,
		UnitsPerSec: 100,
		HWClass:     "unknown-future-hardware",
	}
	err := governPublishedPrice(b, 1, 0.5)
	if err == nil || !strings.Contains(err.Error(), "no exact sustained-watts authority") {
		t.Fatalf("catalogue publication with unknown hardware class error=%v", err)
	}
}

func TestEveryWattConstantDeclaresProvenance(t *testing.T) {
	must(t, validateSustainedWattsTable())
	for class, entry := range sustainedWattsByHWClass {
		if entry.Kind() != wattKindMeasured && entry.Kind() != wattKindAssumed &&
			entry.Kind() != wattKindVendorWallUpperBound {
			t.Fatalf("%s: kind %q is not MEASURED, ASSUMED, or VENDOR_WALL_UPPER_BOUND", class, entry.Kind())
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
