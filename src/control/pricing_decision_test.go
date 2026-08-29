package main

import (
	"errors"
	"reflect"
	"strings"
	"testing"
	"time"
)

func distributedPricingFixture(t *testing.T) (
	WorkloadDecision,
	ComputePlan,
	PlacementRequirement,
	EconomicPlan,
	PricingDecision,
) {
	t.Helper()
	workload, compute, economic := pricingComputePlanFixture(t)
	authority := catalogueAuthorityFixture(
		t, workload, economic.Schedule.Currency, economic.Input.SupplierShare,
	)
	placement := placementForPricingFixture(t, workload, authority)
	pricing, err := newDistributedPricingDecision(
		workload, compute, placement, economic, authority,
		workload.Binding.Tier, "",
	)
	mustf(t, err, "build distributed pricing fixture: %v")
	return workload, compute, placement, economic, pricing
}

func TestHistoricalDistributedPricingReplayAcceptsEmptyTaskEconomicPolicy(t *testing.T) {
	workload, compute, placement, economic, pricing := distributedPricingFixture(t)
	if pricing.TaskEconomicPolicy == "" {
		t.Fatal("current fixture lacks task-economic policy")
	}
	historical := pricing
	historical.TaskEconomicPolicy = ""
	if err := ValidateDistributedPricingDecisionSnapshot(
		historical, workload, compute, placement, economic,
	); err != nil {
		t.Fatalf("historical empty task-economic policy no longer replays: %v", err)
	}
}

func TestHistoricalPlacementV1DigestAndPricingReplayRemainSupported(t *testing.T) {
	workload, compute, current, economic, pricing := distributedPricingFixture(t)
	if current.PerformanceAuthority == nil {
		t.Fatal("current fixture lacks frozen performance authority")
	}
	rate := current.PerformanceAuthority.Performance.ConservativeUnitsPerSec
	legacy := current
	legacy.Version = 1
	legacy.PerformanceAuthority = nil
	legacy.HWClasses = append([]string(nil), workload.Binding.Constraints.HWClasses...)

	if err := validatePlacementRequirement(legacy, workload); err != nil {
		t.Fatalf("historical placement v1 no longer validates: %v", err)
	}
	if digest, err := placementRequirementDigest(legacy); err != nil || !validSHA256(digest) {
		t.Fatalf("historical placement v1 digest unavailable: digest=%q err=%v", digest, err)
	}
	legacyPricing, err := distributedPricingDecisionAtRate(
		workload, compute, legacy, economic, pricing.Catalogue, pricing.Tier,
		pricing.OriginQuotePricingDecisionSHA256, rate,
	)
	mustf(t, err, "build historical v1 pricing snapshot: %v")
	if err := ValidateDistributedPricingDecisionSnapshot(
		legacyPricing, workload, compute, legacy, economic,
	); err != nil {
		t.Fatalf("historical placement v1 pricing replay failed: %v", err)
	}

	// Placement v1 never carried a receipt snapshot. Its accepted rate must
	// remain readable when today's pointer is withdrawn, ages onto a different
	// haircut, or disappears entirely. New ingress rejects this placement version;
	// these mutations exercise replay only.
	path := current.PerformanceAuthority.Performance.BenchmarkAuthority
	original, ok := benchmarkAuthorityManifest[path]
	if !ok {
		t.Fatalf("current fixture receipt %q is absent", path)
	}
	assertReplay := func(t *testing.T) {
		t.Helper()
		if err := ValidateDistributedPricingDecisionSnapshot(
			legacyPricing, workload, compute, legacy, economic,
		); err != nil {
			t.Fatalf("historical placement v1 replay consulted current receipt authority: %v", err)
		}
	}
	t.Run("withdrawn current receipt", func(t *testing.T) {
		withdrawn := cloneBenchmarkReceiptSummary(original)
		withdrawn.Validity = authorityValidityWithdrawn
		benchmarkAuthorityManifest[path] = withdrawn
		t.Cleanup(func() { benchmarkAuthorityManifest[path] = original })
		assertReplay(t)
	})
	t.Run("newly stale current receipt", func(t *testing.T) {
		stale := cloneBenchmarkReceiptSummary(original)
		stale.MeasuredAt = time.Now().Add(-benchmarkRevalidationWindow - 24*time.Hour).
			UTC().Format(time.RFC3339)
		benchmarkAuthorityManifest[path] = stale
		t.Cleanup(func() { benchmarkAuthorityManifest[path] = original })
		assertReplay(t)
	})
	t.Run("removed current receipt", func(t *testing.T) {
		delete(benchmarkAuthorityManifest, path)
		t.Cleanup(func() { benchmarkAuthorityManifest[path] = original })
		assertReplay(t)
	})
}

func TestHistoricalEmbedPlacementV1ReplayDoesNotInheritCurrentUnitPolicy(t *testing.T) {
	installTestOnlyExactIdentityForLegacyBenchmark(t, candleEmbedCell)
	workload, compute, economic := computePlanFixture(t)
	if workload.RuntimeJobType != "embed" {
		t.Fatalf("historical fixture job=%q, want embed", workload.RuntimeJobType)
	}
	candidate := workload.RuntimeCandidates[0]
	profile, cell := cellByID(t, candidate.CellID)
	performance := resolveCellPerformance(profile, cell, time.Now())
	if performance.Unit != "embeddings" || performance.Status == cellThroughputUnproven {
		t.Fatalf("historical embed benchmark premise changed: %+v", performance)
	}
	if err := validateCurrentPerformanceSettlementAuthority(performance); err == nil {
		t.Fatal("current embed admission unexpectedly accepts embeddings/s")
	}

	authority := catalogueAuthorityFixture(
		t, workload, economic.Schedule.Currency, economic.Input.SupplierShare)
	modelKind := candidate.ModelKind
	if modelKind == "" {
		modelKind = workload.Binding.Model.Kind
	}
	legacy := PlacementRequirement{
		Version:             1,
		JobType:             workload.RuntimeJobType,
		ModelRef:            workload.Binding.Model.Ref,
		ModelKind:           modelKind,
		RuntimeCellID:       candidate.CellID,
		RuntimeID:           candidate.RuntimeID,
		Engine:              candidate.Engine,
		RuntimeMatrixSHA256: generatedRuntimeMatrixSHA256,
		MinMemoryGB:         float32(workload.MinimumMemoryGB),
		HWClasses:           append([]string(nil), workload.Binding.Constraints.HWClasses...),
		DataResidency:       append([]string(nil), workload.Binding.Constraints.DataResidency...),
		MinReputation:       workload.Binding.MinReputation,
		TrustedOnly:         workload.Binding.Tier == "trusted",
		OfferedRateUsdHr: float32(expectedSupplierUSDHr(
			performance.ConservativeUnitsPerSec, authority.ReferencePricePer1K,
			authority.SupplierShare, workload.Binding.Tier)),
	}
	mustf(t, validatePlacementRequirement(legacy, workload),
		"historical v1 embed placement rejected: %v")
	pricing, err := distributedPricingDecisionAtRate(
		workload, compute, legacy, economic, authority, workload.Binding.Tier,
		"", performance.ConservativeUnitsPerSec,
	)
	mustf(t, err, "build historical v1 embed pricing: %v")
	if err := ValidateDistributedPricingDecisionSnapshot(
		pricing, workload, compute, legacy, economic,
	); err != nil {
		t.Fatalf("historical v1 embed pricing inherited today's unit policy: %v", err)
	}
}

func TestFixedPointPricingConservesAndRefusesFalseTrueNet(t *testing.T) {
	scenario := EconomicScenario{
		NetBilledUSD: 0.000010, SupplierLiabilityUSD: 0.000004,
		ProcessorFeeUSD: 0.000001, ControlPlaneCostUSD: 0.000001,
		ContributionMarginUSD: 0.000004,
	}
	fixed, err := fixedPointPricingFromScenario(
		"cad", 0.000010, 0.000020, scenario,
		[]string{"storage cost", "risk reserve"},
	)
	must(t, err)
	if fixed.BuyerChargeNanos != 10_000 || fixed.SupplierEntitlementsNanos != 4_000 ||
		fixed.KnownVariableCostsNanos != 2_000 ||
		fixed.KnownCostContributionNanos != 4_000 {
		t.Fatalf("fixed-point amounts drifted: %+v", fixed)
	}
	decision := PricingDecision{Currency: "cad", FixedPoint: fixed}
	mustf(t, validateFixedPointPricing(decision), "valid fixed-point decision refused: %v")
	if fixed.TrueNetContributionNanos != nil {
		t.Fatal("unknown costs became true net contribution")
	}

	mutant := *fixed
	mutant.BuyerChargeNanos++
	decision.FixedPoint = &mutant
	if err := validateFixedPointPricing(decision); err == nil {
		t.Fatal("non-conserving fixed-point buyer charge was accepted")
	}
	mutant = *fixed
	trueNet := mutant.KnownCostContributionNanos
	mutant.TrueNetContributionNanos = &trueNet
	decision.FixedPoint = &mutant
	if err := validateFixedPointPricing(decision); err == nil {
		t.Fatal("true net contribution was accepted while costs remain unknown")
	}
}

func TestMediaRenderingPricingUsesDeclaredPixelsNotSceneBytes(t *testing.T) {
	job := JobType{Type: "media_rendering", RenderWidth: 64, RenderHeight: 32}
	got := settlementInputUnitsForJobType(job, 1, 1_000_000)
	if got != 2_048 {
		t.Fatalf("rendering billable units=%v, want declared canvas pixels 2048", got)
	}
	text := settlementInputUnitsForJobType(JobType{Type: "batch_infer", MaxTokens: 1}, 1, 1_000_000)
	if text <= got {
		t.Fatalf("text geometry unexpectedly used rendering pixel authority: text=%v render=%v", text, got)
	}
	zero := settlementInputUnitsForJobType(JobType{Type: "media_rendering", RenderWidth: 0, RenderHeight: 32}, 1, 100)
	if zero != 0 {
		t.Fatalf("invalid rendering geometry produced billable units=%v", zero)
	}
}

func TestPricingDecisionRejectsArbitraryPositiveSupplierAdmissionRate(t *testing.T) {
	workload, compute, placement, economic, pricing := distributedPricingFixture(t)
	mutantPlacement := placement
	mutantPlacement.OfferedRateUsdHr *= 100
	if _, err := newDistributedPricingDecision(
		workload, compute, mutantPlacement, economic, pricing.Catalogue,
		pricing.Tier, "",
	); err == nil {
		t.Fatal("arbitrary positive supplier admission rate survived derivation")
	}

	// Even an attacker who updates the placement digest and decision field
	// together cannot turn two independently valid siblings into a valid
	// composite decision.
	mutant := pricing
	mutant.PlacementRequirementSHA256, _ = placementRequirementDigest(mutantPlacement)
	mutant.SupplierAdmissionCeilingUSDHr = float64(mutantPlacement.OfferedRateUsdHr)
	if err := ValidateDistributedPricingDecisionSnapshot(
		mutant, workload, compute, mutantPlacement, economic,
	); err == nil {
		t.Fatal("co-mutated placement and pricing decision survived deterministic validation")
	}
}

func TestPricingDecisionDigestBindsEveryEconomicAuthorityFamily(t *testing.T) {
	_, _, _, _, base := distributedPricingFixture(t)
	original, err := pricingDecisionDigest(base)
	must(t, err)
	tests := []struct {
		name   string
		mutate func(*PricingDecision)
	}{
		{"workload", func(p *PricingDecision) { p.WorkloadDecisionSHA256 = strings.Repeat("1", 64) }},
		{"compute", func(p *PricingDecision) { p.ComputePlanSHA256 = strings.Repeat("2", 64) }},
		{"placement", func(p *PricingDecision) { p.PlacementRequirementSHA256 = strings.Repeat("3", 64) }},
		{"economic plan", func(p *PricingDecision) { p.EconomicPlanSHA256 = strings.Repeat("4", 64) }},
		{"economic schedule", func(p *PricingDecision) { p.EconomicScheduleSHA256 = strings.Repeat("5", 64) }},
		{"cost policy retention", func(p *PricingDecision) {
			frozen := *p.CostPolicy
			frozen.RetentionSeconds += int64((24 * time.Hour) / time.Second)
			p.CostPolicy = &frozen
		}},
		{"exact storage nanos", func(p *PricingDecision) { p.StorageAcceptedNanos++ }},
		{"catalogue schedule", func(p *PricingDecision) { p.Catalogue.ScheduleSHA256 = strings.Repeat("6", 64) }},
		{"market board", func(p *PricingDecision) { p.Catalogue.BoardSHA256 = strings.Repeat("7", 64) }},
		{"FX", func(p *PricingDecision) { p.Catalogue.FXRevision += "-changed" }},
		{"supplier share", func(p *PricingDecision) { p.Catalogue.SupplierShare -= 0.01 }},
		{"buyer price", func(p *PricingDecision) { p.BuyerPrice += 0.01 }},
		{"supplier cost", func(p *PricingDecision) { p.PrimarySupplierCost.Amount += 0.01 }},
		{"payment cost", func(p *PricingDecision) { p.PaymentCost.Amount += 0.01 }},
		{"storage cost status", func(p *PricingDecision) {
			// Storage is now modeled under the cost schedule; flip to unknown so
			// the digest must move.
			p.StorageCost.Status = pricingCostUnknown
			p.StorageCost.Amount = 0
		}},
		{"confidence", func(p *PricingDecision) { p.Confidence -= 0.1 }},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			mutant := base
			tc.mutate(&mutant)
			got, err := pricingDecisionDigest(mutant)
			must(t, err)
			if got == original {
				t.Fatalf("%s mutation did not change pricing digest", tc.name)
			}
		})
	}
}

func TestDistributedPricingDecisionUsesExplicitUnknownCostStates(t *testing.T) {
	// After cost-schedule attribution, storage/egress/risk are modeled (or
	// not_applicable) and provider is not_applicable for community supply.
	// Unknown remains the only honest status for a cloud cell with no rate;
	// that path is covered by true_net_contribution_test.go. This test now
	// asserts every component has a non-empty basis and never carries a
	// modeled amount under an unknown/not_applicable status.
	_, _, _, _, pricing := distributedPricingFixture(t)
	for name, component := range map[string]PricingCostComponent{
		"storage":  pricing.StorageCost,
		"egress":   pricing.EgressCost,
		"provider": pricing.ProviderCost,
		"risk":     pricing.RiskReserve,
	} {
		if component.Basis == "" {
			t.Fatalf("%s cost lacks a basis: %+v", name, component)
		}
		if component.Status != pricingCostModeled &&
			component.Status != pricingCostNotApplicable &&
			component.Status != pricingCostUnknown {
			t.Fatalf("%s cost has invalid status: %+v", name, component)
		}
		if component.Status != pricingCostModeled && component.Amount != 0 {
			t.Fatalf("%s cost carries amount under non-modeled status: %+v", name, component)
		}
	}
	if pricing.CostScheduleSHA256 == "" {
		t.Fatal("distributed pricing decision lacks cost schedule digest")
	}
	if pricing.CostPolicy == nil {
		t.Fatal("distributed pricing decision lacks frozen cost policy")
	}
	if pricing.ProviderCost.Status != pricingCostNotApplicable &&
		pricing.ProviderCost.Status != pricingCostUnknown &&
		pricing.ProviderCost.Status != pricingCostModeled {
		t.Fatalf("provider cost status unexpected: %+v", pricing.ProviderCost)
	}
	if pricing.ExpectedSupplierGrossUSDHr+0.000001 <
		pricing.SupplierAdmissionCeilingUSDHr {
		t.Fatalf("modeled supplier gross %.6f is below admission ceiling %.6f",
			pricing.ExpectedSupplierGrossUSDHr,
			pricing.SupplierAdmissionCeilingUSDHr)
	}
}

func TestDistributedPricingReplayAndSettlementUseFrozenCostPolicy(t *testing.T) {
	t.Setenv(costScheduleRevisionEnv, "")
	t.Setenv("MERC_JOB_OBJECT_RETENTION_DAYS", "30")
	workload, compute, placement, economic, accepted := distributedPricingFixture(t)
	if accepted.CostPolicy == nil {
		t.Fatal("accepted decision lacks frozen cost policy")
	}
	if accepted.CostPolicy.RetentionSeconds != int64((30*24*time.Hour)/time.Second) {
		t.Fatalf("accepted retention=%d seconds, want 30 days", accepted.CostPolicy.RetentionSeconds)
	}
	acceptedDigest, err := pricingDecisionDigest(accepted)
	must(t, err)
	tamperedExact := accepted
	tamperedExact.StorageAcceptedNanos++
	if err := ValidateDistributedPricingDecisionSnapshot(
		tamperedExact, workload, compute, placement, economic,
	); err == nil {
		t.Fatalf("tampered exact storage nanos replay boundary=%v", err)
	}
	now := time.Date(2026, 8, 9, 12, 0, 0, 0, time.UTC)
	before, err := settleStorageEgressFromBytes(accepted, 17_123, 9_321, now)
	mustf(t, err, "settle accepted policy before config change: %v")

	// A future binary/config may select another schedule and retention. The old
	// accepted body must not consult either setting during replay or settlement.
	t.Setenv(costScheduleRevisionEnv, "cost-schedule-future")
	t.Setenv("MERC_JOB_OBJECT_RETENTION_DAYS", "8")
	if err := ValidateDistributedPricingDecisionSnapshot(
		accepted, workload, compute, placement, economic,
	); err != nil {
		t.Fatalf("historical replay consulted current cost configuration: %v", err)
	}
	after, err := settleStorageEgressFromBytes(accepted, 17_123, 9_321, now)
	mustf(t, err, "settle accepted policy after config change: %v")
	if !reflect.DeepEqual(before, after) {
		t.Fatalf("historical cost settlement moved under current config:\n before=%+v\n  after=%+v", before, after)
	}
	if digest, err := pricingDecisionDigest(accepted); err != nil || digest != acceptedDigest {
		t.Fatalf("accepted decision identity moved: digest=%q want=%q err=%v",
			digest, acceptedDigest, err)
	}

	// New admission is not allowed to borrow the old schedule when today's
	// explicitly selected revision is unknown to this binary.
	if _, err := newDistributedPricingDecision(
		workload, compute, placement, economic, accepted.Catalogue,
		accepted.Tier, accepted.OriginQuotePricingDecisionSHA256,
	); err == nil || !strings.Contains(err.Error(), "cost-schedule-future") {
		t.Fatalf("new admission did not fail closed on current cost revision: %v", err)
	}

	// Once the current schedule is valid, new admission freezes today's 8-day
	// retention while the old decision remains at 30 days.
	t.Setenv(costScheduleRevisionEnv, accepted.CostPolicy.Schedule.Revision)
	current, err := newDistributedPricingDecision(
		workload, compute, placement, economic, accepted.Catalogue,
		accepted.Tier, accepted.OriginQuotePricingDecisionSHA256,
	)
	mustf(t, err, "new admission under current cost policy: %v")
	if current.CostPolicy == nil ||
		current.CostPolicy.RetentionSeconds != int64((8*24*time.Hour)/time.Second) {
		t.Fatalf("new admission did not freeze current retention: %+v", current.CostPolicy)
	}
	currentDigest, err := pricingDecisionDigest(current)
	must(t, err)
	if currentDigest == acceptedDigest {
		t.Fatal("cost-policy retention change did not alter PricingDecision identity")
	}
}

func TestFrozenCostPolicyV1RetentionRevisionSurvivesFutureAdmissionPolicy(t *testing.T) {
	_, _, _, _, accepted := distributedPricingFixture(t)
	if accepted.CostPolicy == nil {
		t.Fatal("fixture lacks frozen cost policy")
	}
	historical := *accepted.CostPolicy
	if historical.Version != frozenCostPolicySnapshotVersionV1 ||
		historical.RetentionPolicyRevision != frozenCostPolicyV1RetentionPolicyRevision {
		t.Fatalf("fixture did not freeze permanent v1 retention authority: %+v", historical)
	}

	// Simulate the live retention policy advancing in a future binary. New
	// admission must refuse to keep emitting snapshot v1, while the already
	// accepted v1 body remains valid without consulting that live revision.
	future := currentJobObjectRetentionPolicy()
	future.Revision = "job-object-retention-v2"
	if err := validateCurrentRetentionPolicyForFrozenCostVersion(
		frozenCostPolicySnapshotVersionV1, future,
	); err == nil || !strings.Contains(err.Error(), "bump the frozen snapshot version") {
		t.Fatalf("future retention revision remained compatible with snapshot v1: %v", err)
	}
	if err := validateFrozenCostPolicySnapshot(&historical, accepted.Currency); err != nil {
		t.Fatalf("historical v1 replay consulted future admission revision: %v", err)
	}

	tampered := historical
	tampered.RetentionPolicyRevision = future.Revision
	if err := validateFrozenCostPolicySnapshot(&tampered, accepted.Currency); err == nil ||
		!strings.Contains(err.Error(), "unsupported retention policy revision") {
		t.Fatalf("v1 snapshot accepted future retention semantics: %v", err)
	}
}

func TestLegacyCostScheduleDigestWithoutBodyIsExplicitlyUnreplayable(t *testing.T) {
	workload, compute, placement, economic, pricing := distributedPricingFixture(t)
	legacy := pricing
	legacy.CostPolicy = nil
	if err := ValidateDistributedPricingDecisionSnapshot(
		legacy, workload, compute, placement, economic,
	); err == nil || !strings.Contains(err.Error(), "legacy pricing decision binds only") {
		t.Fatalf("legacy digest-only decision replay boundary=%v", err)
	}
	if _, err := settleStorageEgressFromBytes(legacy, 1, 1, time.Now()); err == nil || !strings.Contains(err.Error(), "full cost policy was not frozen") {
		t.Fatalf("legacy digest-only decision settlement boundary=%v", err)
	}
}

func TestDefaultCADCostScheduleRequiresExactFrozenFX(t *testing.T) {
	installSettlementCurrencyForTest(t, "cad")
	t.Setenv(costScheduleRevisionEnv, "")
	unresolved := DefaultCostSchedule("cad")
	if unresolved.ReferenceCurrency != "usd" || unresolved.SettlementCurrency != "cad" ||
		unresolved.StorageReferenceNanosPerGiBMonth != defaultStorageNanosPerGiBMonth ||
		unresolved.EgressReferenceNanosPerGiB != defaultEgressNanosPerGiB {
		t.Fatalf("CAD default lost explicit USD reference authority: %+v", unresolved)
	}
	if unresolved.StorageNanosPerGiBMonth != 0 || unresolved.EgressNanosPerGiB != 0 {
		t.Fatalf("CAD default numerically relabeled USD rates before FX: %+v", unresolved)
	}
	if reason := validateCostSchedule(unresolved); reason == "" {
		t.Fatal("unconverted cross-currency cost schedule validated")
	}

	catalogue := CataloguePriceAuthority{
		ReferenceCurrency: costReferenceCurrency, SettlementCurrency: "cad",
		ReferenceToSettlementRate: 1.375, FXRevision: "test-cad-cost-fx-1.375",
	}
	fx, err := costFXAuthorityFromCatalogue(catalogue)
	mustf(t, err, "freeze CAD cost FX: %v")
	schedule, err := LoadCostScheduleFromEnv(fx)
	mustf(t, err, "load CAD cost schedule: %v")
	if fx.ReferenceToSettlementNanos != 1_375_000_000 {
		t.Fatalf("exact CAD FX nanos=%d, want 1375000000", fx.ReferenceToSettlementNanos)
	}
	wantStorage, err := mulDiv(
		defaultStorageNanosPerGiBMonth, fx.ReferenceToSettlementNanos,
		costFXRateScale, true,
	)
	must(t, err)
	wantEgress, err := mulDiv(
		defaultEgressNanosPerGiB, fx.ReferenceToSettlementNanos,
		costFXRateScale, true,
	)
	must(t, err)
	if schedule.StorageNanosPerGiBMonth != wantStorage ||
		schedule.EgressNanosPerGiB != wantEgress ||
		schedule.StorageNanosPerGiBMonth == schedule.StorageReferenceNanosPerGiBMonth ||
		schedule.EgressNanosPerGiB == schedule.EgressReferenceNanosPerGiB {
		t.Fatalf("CAD cost conversion=%+v want storage=%d egress=%d",
			schedule, wantStorage, wantEgress)
	}

	badFX := fx
	badFX.ReferenceToSettlementNanos++
	if _, err := LoadCostScheduleFromEnv(badFX); err == nil ||
		!strings.Contains(err.Error(), "display rate disagrees") {
		t.Fatalf("CAD schedule accepted incoherent exact FX: %v", err)
	}
	if _, err := LoadCostScheduleFromEnv(CostFXAuthority{}); err == nil {
		t.Fatal("CAD schedule loaded without frozen FX authority")
	}
}

func TestCADCostPolicyReplaySurvivesFXDriftAndNewAdmissionUsesNewCatalogue(t *testing.T) {
	installSettlementCurrencyForTest(t, "cad")
	t.Setenv(costScheduleRevisionEnv, "")
	t.Setenv(priceFXRateEnv, "1.35")
	t.Setenv(priceFXRevisionEnv, "test-fx-cad")
	workload, compute, economic := pricingComputePlanFixture(t)
	economicSchedule := economic.Schedule
	economicSchedule.Currency = "cad"
	economic = BuildEconomicPlan(economic.Input, economicSchedule)
	mustf(t, ValidateComputePlanEconomicSnapshot(compute, workload, economic),
		"CAD compute/economic fixture: %v")
	catalogue := catalogueAuthorityFixture(
		t, workload, "cad", economic.Input.SupplierShare,
	)
	placement := placementForPricingFixture(t, workload, catalogue)
	accepted, err := newDistributedPricingDecision(
		workload, compute, placement, economic, catalogue,
		workload.Binding.Tier, "",
	)
	mustf(t, err, "build accepted CAD cost policy: %v")
	if accepted.CostPolicy == nil || accepted.CostPolicy.FX.ReferenceToSettlementNanos != 1_350_000_000 ||
		accepted.CostPolicy.Schedule.ReferenceCurrency != "usd" ||
		accepted.CostPolicy.Schedule.SettlementCurrency != "cad" {
		t.Fatalf("accepted CAD cost policy lacks exact currency authority: %+v", accepted.CostPolicy)
	}
	acceptedDigest, err := pricingDecisionDigest(accepted)
	must(t, err)
	now := time.Date(2026, 8, 9, 12, 0, 0, 0, time.UTC)
	before, err := settleStorageEgressFromBytes(accepted, 17_123, 9_321, now)
	mustf(t, err, "settle accepted CAD cost policy: %v")

	// Today's operator FX can move without rewriting accepted CAD work.
	t.Setenv(priceFXRateEnv, "1.36")
	t.Setenv(priceFXRevisionEnv, "test-fx-cad-next")
	if err := ValidateDistributedPricingDecisionSnapshot(
		accepted, workload, compute, placement, economic,
	); err != nil {
		t.Fatalf("historical CAD pricing consulted current FX: %v", err)
	}
	after, err := settleStorageEgressFromBytes(accepted, 17_123, 9_321, now)
	mustf(t, err, "replay accepted CAD cost settlement: %v")
	if !reflect.DeepEqual(before, after) {
		t.Fatalf("historical CAD settlement moved under FX drift:\n before=%+v\n  after=%+v",
			before, after)
	}

	// A newly published catalogue carries the new governed FX. New admission
	// freezes it and produces a different cost-policy and PricingDecision digest.
	currentCatalogue := catalogue
	currentCatalogue.ReferenceToSettlementRate = 1.36
	currentCatalogue.FXRevision = "test-fx-cad-next"
	currentCatalogue.SettlementPricePer1K = ceilPricePer1K(
		currentCatalogue.ReferencePricePer1K * currentCatalogue.ReferenceToSettlementRate)
	currentCatalogue.ScheduleSHA256 = strings.Repeat("9", 64)
	current, err := newDistributedPricingDecision(
		workload, compute, placement, economic, currentCatalogue,
		workload.Binding.Tier, "",
	)
	mustf(t, err, "build current CAD cost policy: %v")
	if current.CostPolicy == nil || current.CostPolicy.FX.ReferenceToSettlementNanos != 1_360_000_000 {
		t.Fatalf("new admission did not freeze current CAD FX: %+v", current.CostPolicy)
	}
	currentDigest, err := pricingDecisionDigest(current)
	must(t, err)
	if currentDigest == acceptedDigest {
		t.Fatal("new catalogue FX did not change PricingDecision identity")
	}

	missingFX := catalogue
	missingFX.ReferenceToSettlementRate = 0
	missingFX.FXRevision = ""
	missingFX.SettlementPricePer1K = 0
	if _, err := newDistributedPricingDecision(
		workload, compute, placement, economic, missingFX,
		workload.Binding.Tier, "",
	); err == nil {
		t.Fatal("new CAD admission accepted a catalogue without frozen FX")
	}
}

func TestV4PricingBillableUnitsUseFrozenSettlementAuthority(t *testing.T) {
	workload, compute, placement, economic, pricing := distributedPricingFixture(t)
	if compute.Version != computePlanVersion {
		t.Fatalf("fixture plan version=%d, want current v4", compute.Version)
	}
	if compute.EstimatedInputTokens == int64(compute.SettlementInputUnits) {
		t.Fatal("fixture does not distinguish selected-body planning tokens from settlement units")
	}
	want := compute.SettlementInputUnits + float64(compute.EstimatedOutputTokens)
	if pricing.BillableUnits != want {
		t.Fatalf("pricing billable_units=%v, want frozen settlement units %v", pricing.BillableUnits, want)
	}
	if pricing.BillableUnits == float64(compute.EstimatedInputTokens+compute.EstimatedOutputTokens) {
		t.Fatal("pricing still presents selected-body planning tokens as money units")
	}

	// Historical v2 decisions retain their original computed presentation and
	// remain verifiable; version 3 and later plans carry the reconciled field.
	historical := compute
	historical.Version = computePlanVersionV2
	historical.SettlementInputUnits = 0
	historical.ETAConfidenceBandMethod = ""
	historicalPricing, err := newDistributedPricingDecision(
		workload, historical, placement, economic, pricing.Catalogue,
		workload.Binding.Tier, "",
	)
	mustf(t, err, "rebuild historical v2 pricing: %v")
	wantHistorical := float64(historical.EstimatedInputTokens + historical.EstimatedOutputTokens)
	if historicalPricing.BillableUnits != wantHistorical {
		t.Fatalf("historical v2 billable_units=%v, want preserved %v", historicalPricing.BillableUnits, wantHistorical)
	}
}

func TestExactReusePricingHasNoPhysicalSupplierOrPlacement(t *testing.T) {
	workload, origin, _, _, originPricing := distributedPricingFixture(t)
	originSHA, err := pricingDecisionDigest(originPricing)
	must(t, err)
	reuseCompute, err := newExactReuseComputePlan(
		workload, origin.InputRecords, origin.InputBytes, testInputDepthProfile(origin.InputRecords), 0.01, &origin,
	)
	must(t, err)
	reuse, err := newExactReusePricingDecision(
		workload, reuseCompute, originPricing.Catalogue,
		workload.Binding.Tier, 0.01, originSHA,
	)
	must(t, err)
	if reuse.PlacementRequirementSHA256 != "" ||
		reuse.PrimarySupplierCost.Status != pricingCostNotApplicable ||
		reuse.VerificationCost.Status != pricingCostNotApplicable ||
		reuse.PrimarySupplierCost.Amount != 0 ||
		reuse.VerificationCost.Amount != 0 {
		t.Fatalf("exact reuse attributes physical work: %+v", reuse)
	}
	mutant := reuse
	mutant.PrimarySupplierCost = modeledCost(0.001, "forged physical work")
	if reflect.DeepEqual(mutant, reuse) {
		t.Fatal("test mutation failed")
	}
	if err := ValidateExactReusePricingDecisionSnapshot(
		mutant, workload, reuseCompute,
	); err == nil {
		t.Fatal("exact reuse accepted forged physical supplier work")
	}
}

func TestBoundQuoteCatalogueSelectionNeverReadsCurrentModelPrice(t *testing.T) {
	_, _, _, _, pricing := distributedPricingFixture(t)
	called := false
	got, err := selectCataloguePriceAuthority(
		&boundQuote{Pricing: pricing},
		func() (CataloguePriceAuthority, error) {
			called = true
			mutant := pricing.Catalogue
			mutant.SettlementPricePer1K *= 100
			return mutant, nil
		},
	)
	must(t, err)
	if called {
		t.Fatal("bound quote consulted the current model price")
	}
	if !reflect.DeepEqual(got, pricing.Catalogue) {
		t.Fatalf("bound quote catalogue changed: got %+v want %+v",
			got, pricing.Catalogue)
	}
}

func TestBoundVersionTwoQuoteCannotMintANewJob(t *testing.T) {
	_, _, _, _, pricing := distributedPricingFixture(t)
	pricing.Catalogue.Version = 2
	pricing.Catalogue.ScheduleVersion = 2
	pricing.Catalogue.PhysicalAuthority = CatalogueResultPhysicalAuthority{}
	_, err := selectCataloguePriceAuthority(
		&boundQuote{Pricing: pricing},
		func() (CataloguePriceAuthority, error) {
			t.Fatal("historical bound quote consulted mutable current price")
			return CataloguePriceAuthority{}, nil
		},
	)
	if err == nil || !strings.Contains(err.Error(), "historical-only") {
		t.Fatalf("bound v2 quote selection error=%v", err)
	}
}

// The validator must re-derive the supplier unit rate from the governed cell
// authority, never from the record being checked.
//
// Rebuilding with decision.ExpectedSupplierUnitsPerSec made this self-certifying.
// The rebuild derives the admission ceiling from that rate and compares it to the
// placement's offered rate, so an attacker who can rewrite a stored pricing
// decision - and can therefore rewrite the stored placement beside it - gets a
// composite that verifies at any rate at all. Here the whole set is internally
// consistent and every digest matches; the only thing wrong with it is that no
// benchmark in the tree produces the rate.
func TestStoredPricingDecisionCannotCertifyItsOwnSupplierRate(t *testing.T) {
	workload, compute, placement, economic, pricing := distributedPricingFixture(t)

	forgedRate := pricing.ExpectedSupplierUnitsPerSec * 10
	forgedPlacement := placement
	forgedPlacement.OfferedRateUsdHr = float32(expectedSupplierUSDHr(
		forgedRate, pricing.Catalogue.ReferencePricePer1K,
		pricing.Catalogue.SupplierShare, pricing.Tier,
	))
	forged, err := distributedPricingDecisionAtRate(
		workload, compute, forgedPlacement, economic, pricing.Catalogue,
		pricing.Tier, "", forgedRate,
	)
	mustf(t, err, "the forgery is meant to be internally consistent: %v")
	if err := ValidateDistributedPricingDecisionSnapshot(
		forged, workload, compute, forgedPlacement, economic,
	); err == nil {
		t.Fatalf("a decision claiming %v units/s validated against itself at a "+
			"$%.5f/hr admission ceiling", forgedRate, forged.SupplierAdmissionCeilingUSDHr)
	}
}

// A quote freezes a supplier unit rate; the receipt behind it keeps ageing.
// Rebuilding a bound submission from live evidence compared today's posture
// against the quote's frozen offered rate, so a receipt crossing its 180-day
// revalidation window between quote and submit must be classified as temporary
// physical-authority unavailability, never a buyer-caused 4xx.
func TestQuoteFrozenSupplierRateSurvivesTheRevalidationBoundary(t *testing.T) {
	workload, compute, placement, economic, pricing := distributedPricingFixture(t)
	if placement.PerformanceAuthority == nil {
		t.Fatal("v2 fixture has no frozen performance authority")
	}

	// Freeze what the SAME receipt yields once it is past its window. A v2
	// placement carries that posture inside its accepted performance snapshot;
	// changing only OfferedRateUsdHr would create inconsistent sibling fields
	// and should be rejected.
	stalePlacement := placement
	stalePerformance := *placement.PerformanceAuthority
	stalePerformance.Performance.Status = cellThroughputStale
	stalePerformance.Performance.Haircut = staleThroughputHaircut
	stalePostureRate := stalePerformance.Performance.ObservedUnitsPerSec *
		staleThroughputHaircut
	stalePerformance.Performance.ConservativeUnitsPerSec = stalePostureRate
	stalePerformance.Performance.Reason = "receipt exceeded revalidation window when this placement was accepted"
	stalePerformance.Digest = ""
	var err error
	stalePerformance.Digest, err = frozenRuntimeCellPerformanceDigest(stalePerformance)
	mustf(t, err, "digest stale performance authority: %v")
	stalePlacement.PerformanceAuthority = &stalePerformance
	stalePlacement.OfferedRateUsdHr = float32(expectedSupplierUSDHr(
		stalePostureRate, pricing.Catalogue.ReferencePricePer1K,
		pricing.Catalogue.SupplierShare, pricing.Tier,
	))

	// Live resolution refuses a quote frozen on the other side of the boundary,
	// in either direction, and preserves the sentinel the API maps to 503.
	if _, err := newDistributedPricingDecision(
		workload, compute, stalePlacement, economic, pricing.Catalogue, pricing.Tier, "",
	); err == nil {
		t.Fatal("live re-resolution accepted a rate from the other posture; " +
			"this test no longer describes the boundary")
	} else if !errors.Is(err, errQuotePhysicalAuthorityUnavailable) {
		t.Fatalf("performance posture crossing lost its public 503 sentinel: %v", err)
	}

	// Binding the quote's own frozen rate accepts it, and the stored snapshot
	// still verifies, because both governed postures of the same measurement are
	// admissible and a rate from neither is not.
	bound, err := distributedPricingDecisionAtRate(
		workload, compute, stalePlacement, economic, pricing.Catalogue,
		pricing.Tier, "", stalePostureRate,
	)
	mustf(t, err, "binding the quote's frozen rate: %v")
	if err := ValidateDistributedPricingDecisionSnapshot(
		bound, workload, compute, stalePlacement, economic,
	); err != nil {
		t.Fatalf("a decision at a governed stale-posture rate failed its own "+
			"snapshot check: %v", err)
	}
}
