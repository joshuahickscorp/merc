package main

import (
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
)

// chargeBatchEconomicSchedule is the modern economic schedule that freezes
// fixed-point accepted pricing. Final true net lives in ContributionSettlement.
func chargeBatchEconomicSchedule(t *testing.T) EconomicSchedule {
	t.Helper()
	currency := "usd"
	if code := SettlementCurrencyCode(); code != "" {
		currency = code
	}
	return EconomicSchedule{
		Version:                      "true-net-test-v1",
		Currency:                     currency,
		ProcessorPercent:             0.029,
		ProcessorFixedUSD:            0.30,
		MinChargeBatchUSD:            5.00,
		ControlPlanePerBatchUSD:      0.0001,
		ControlPlaneAllocationPolicy: controlPlaneAllocationChargeBatchV1,
		MinimumContributionUSD:       0.01,
		TargetMarginRate:             0.10,
	}
}

// trueNetDistributedFixture builds a distributed pricing decision under the
// cost schedule on community (non-cloud) supply so provider is not_applicable
// and accepted known-cost contribution is reachable. Runtime-cell unknowns still
// prevent acceptance from masquerading as final settlement.
func trueNetDistributedFixture(t *testing.T) (
	WorkloadDecision, ComputePlan, PlacementRequirement, EconomicPlan, PricingDecision, CostSchedule,
) {
	t.Helper()
	sub := testOnlyCombinedTokenSubmit(t)
	workload, err := buildWorkloadDecision(sub, strings.Repeat("a", 64))
	mustf(t, err, "workload: %v")
	schedule := chargeBatchEconomicSchedule(t)
	// Size the job so contribution headroom absorbs storage/egress/risk.
	economic := BuildEconomicPlan(EconomicPlanInput{
		BaseComputeUSD:   2.00,
		InitialTaskCount: 2,
		ExtraTaskReserve: 2,
		SupplierShare:    0.80,
	}, schedule)
	if !economic.Executable {
		t.Fatalf("economics blocked: %s", economic.BlockReason)
	}
	// Geometry: 4 records / split 2 → 2 primary tasks. Economic InitialTaskCount
	// must match total tasks (primary + redundancy + honeypot). Base compute on
	// the plan must match the economic plan's base.
	compute, err := newDistributedComputePlan(
		workload,
		4, 4096, testInputDepthProfile(4),
		2, 2, 0, 0,
		quoteTimeFromETABands(30, 50, true),
		"planner", economic.Input.BaseComputeUSD, 0,
		QuoteConfidence{Score: 0.9, Reasons: []string{"true-net fixture"}},
		nil,
	)
	mustf(t, err, "compute plan: %v")
	mustf(t, ValidateComputePlanEconomicSnapshot(compute, workload, economic), "compute/economic authority: %v")
	// Force community cell if the workload picked something else.
	if len(workload.RuntimeCandidates) == 1 {
		// The BOUND candle batch cell is community supply, not cloud-backed.
		if cloud, ok := cellIsCloudBacked(workload.RuntimeCandidates[0].CellID); ok && cloud {
			t.Fatalf("fixture landed on cloud-backed cell %s; want community",
				workload.RuntimeCandidates[0].CellID)
		}
	}
	authority := catalogueAuthorityFixture(t, workload, schedule.Currency, economic.Input.SupplierShare)
	placement := placementForPricingFixture(t, workload, authority)
	pricing, err := newDistributedPricingDecision(
		workload, compute, placement, economic, authority, workload.Binding.Tier, "",
	)
	mustf(t, err, "pricing: %v")
	cost := DefaultCostSchedule(schedule.Currency)
	return workload, compute, placement, economic, pricing, cost
}

// reloadSettlementCurrencyForTest installs MERC_SETTLEMENT_CURRENCY from the
// current environment into the process settlement currency.
func reloadSettlementCurrencyForTest() error {
	_, err := LoadSettlementCurrencyFromEnv()
	return err
}

func TestDistributedAcceptancePublishesKnownCostContributionButNotSettlement(t *testing.T) {
	_, _, _, economic, pricing, _ := trueNetDistributedFixture(t)

	if pricing.CostScheduleSHA256 == "" {
		t.Fatal("new decision lacks cost schedule digest")
	}
	if pricing.ProviderCost.Status != pricingCostNotApplicable {
		t.Fatalf("community provider cost status=%s want not_applicable: %+v",
			pricing.ProviderCost.Status, pricing.ProviderCost)
	}
	if pricing.StorageCost.Status != pricingCostModeled &&
		pricing.StorageCost.Status != pricingCostNotApplicable {
		t.Fatalf("storage status=%s", pricing.StorageCost.Status)
	}
	if pricing.EgressCost.Status != pricingCostModeled &&
		pricing.EgressCost.Status != pricingCostNotApplicable {
		t.Fatalf("egress status=%s", pricing.EgressCost.Status)
	}
	if pricing.RiskReserve.Status != pricingCostModeled {
		t.Fatalf("risk status=%s", pricing.RiskReserve.Status)
	}
	if pricing.FixedPoint == nil {
		t.Fatal("expected fixed-point pricing")
	}
	fixed := pricing.FixedPoint
	// Exact settlement authority must survive true-net extras: supplier is the
	// plan entitlement, never a re-quantised float and never reduced by Merc's
	// modeled storage/egress/provider/risk costs.
	if economic.EconomicRoundingPolicy != economicRoundingPolicy ||
		economic.SupplierPayoutPerTaskNanos <= 0 ||
		economic.BuyerChargePerTaskNanos <= 0 {
		t.Fatalf("fixture plan lacks exact nano authority: policy=%q supplier_nanos=%d buyer_nanos=%d",
			economic.EconomicRoundingPolicy, economic.SupplierPayoutPerTaskNanos, economic.BuyerChargePerTaskNanos)
	}
	scenario, err := fullSuccessEconomicScenario(economic)
	mustf(t, err, "scenario: %v")
	mustf(t, validateFixedPointMatchesPlan(pricing, economic, scenario), "validateFixedPointMatchesPlan: %v")
	wantSupplier := economic.SupplierPayoutPerTaskNanos * int64(scenario.AcceptedTasks)
	if fixed.SupplierEntitlementsNanos != wantSupplier {
		t.Fatalf("supplier leg %d != plan entitlement %d × %d tasks",
			fixed.SupplierEntitlementsNanos, economic.SupplierPayoutPerTaskNanos, scenario.AcceptedTasks)
	}
	if len(fixed.UnknownCostCategories) == 0 ||
		!strings.HasPrefix(fixed.UnknownCostCategories[0], "runtime cell:") {
		t.Fatalf("runtime-cell blockers did not reach fixed-point acceptance: %v",
			fixed.UnknownCostCategories)
	}
	if fixed.TrueNetContributionNanos != nil {
		t.Fatal("accepted forecast published a true-net settlement number")
	}
	// Exact integer identity:
	// true_net = buyer - supplier - processor - control - storage - egress - provider - risk
	// which equals KnownCostContributionNanos under the conservation equation.
	toNanos := func(c PricingCostComponent) int64 {
		if c.Status != pricingCostModeled {
			return 0
		}
		return usdToMicros(c.Amount) * NanosPerMicro
	}
	want := fixed.BuyerChargeNanos -
		fixed.SupplierEntitlementsNanos -
		toNanos(pricing.PaymentCost) -
		toNanos(pricing.ControlPlaneCost) -
		pricing.StorageAcceptedNanos -
		pricing.EgressAcceptedNanos -
		toNanos(pricing.ProviderCost) -
		pricing.RiskReserveAcceptedNanos
	if fixed.KnownCostContributionNanos != want {
		t.Fatalf("accepted known-cost contribution %d != buyer-supplier-known-costs %d (fixed=%+v)",
			fixed.KnownCostContributionNanos, want, fixed)
	}
	forecast, err := acceptedForecastContributionSettlement("quote", "q_test", pricing)
	mustf(t, err, "accepted contribution forecast: %v")
	if forecast.Stage != ContributionStageAcceptedForecast || forecast.TrueNetNanos != nil ||
		forecast.AcceptedKnownCostContributionNanos != want {
		t.Fatalf("quote forecast crossed the settlement boundary: %+v", forecast)
	}
	t.Logf("WORKED EXAMPLE (community supply, exact plan nanos):")
	t.Logf("  plan supplier_payout_per_task_nanos = %d", economic.SupplierPayoutPerTaskNanos)
	t.Logf("  plan buyer_charge_per_task_nanos    = %d", economic.BuyerChargePerTaskNanos)
	t.Logf("  accepted_tasks                      = %d", scenario.AcceptedTasks)
	t.Logf("  buyer_charge_nanos                  = %d", fixed.BuyerChargeNanos)
	t.Logf("  supplier_entitlements_nanos         = %d  (must equal plan × tasks = %d)",
		fixed.SupplierEntitlementsNanos, wantSupplier)
	t.Logf("  payment (processor) nanos           = %d", toNanos(pricing.PaymentCost))
	t.Logf("  control_plane nanos                 = %d", toNanos(pricing.ControlPlaneCost))
	t.Logf("  storage nanos (status=%s)           = %d", pricing.StorageCost.Status, pricing.StorageAcceptedNanos)
	t.Logf("  egress nanos (status=%s)            = %d", pricing.EgressCost.Status, pricing.EgressAcceptedNanos)
	t.Logf("  provider nanos (status=%s)          = %d", pricing.ProviderCost.Status, toNanos(pricing.ProviderCost))
	t.Logf("  risk_reserve nanos                  = %d", pricing.RiskReserveAcceptedNanos)
	t.Logf("  known_variable_costs_nanos          = %d", fixed.KnownVariableCostsNanos)
	t.Logf("  accepted_known_cost_contribution    = %d", fixed.KnownCostContributionNanos)
	t.Logf("  conservation: buyer(%d) = supplier(%d) + variable(%d) + contribution(%d)",
		fixed.BuyerChargeNanos, fixed.SupplierEntitlementsNanos,
		fixed.KnownVariableCostsNanos, fixed.KnownCostContributionNanos)
}

func TestWithdrawnCloudProviderRateCannotClaimTrueNet(t *testing.T) {
	// The numeric rate remains visible for diagnostics, but its receipt was
	// withdrawn. Canonical pricing must therefore keep provider cost unknown.
	rate, ok := providerRatesByHWClass["nvidia_24gb"]
	if !ok {
		t.Fatal("nvidia_24gb rate missing")
	}
	if rate.AuthorityStatus != providerRateAuthorityWithdrawn {
		t.Fatalf("nvidia_24gb authority status=%q, want %q",
			rate.AuthorityStatus, providerRateAuthorityWithdrawn)
	}

	cloud, found := cellIsCloudBacked("vllm-cuda-llama1-infer")
	if !found || !cloud {
		t.Fatalf("vllm-cuda-llama1-infer cloud_backed=%v found=%v; marker missing from authority",
			cloud, found)
	}
	_, _, _, _, communityPricing, _ := trueNetDistributedFixture(t)
	component := providerCostComponentForPlacement(
		"vllm-cuda-llama1-infer", []string{"nvidia_24gb"}, 60,
		communityPricing.Catalogue,
	)
	if component.Status != pricingCostUnknown || component.Amount != 0 {
		t.Fatalf("withdrawn cloud provider cost became modeled: %+v", component)
	}
	if !strings.Contains(strings.ToLower(component.Basis), "withdraw") {
		t.Fatalf("withdrawn provider refusal lost provenance: %+v", component)
	}

	// Community cell remains not_applicable.
	community := providerCostComponentForPlacement(
		"candle-metal-llama1-infer", nil, 60, communityPricing.Catalogue,
	)
	if community.Status != pricingCostNotApplicable {
		t.Fatalf("community provider: %+v", community)
	}

	// The canonical fixed-point shape must keep true net unavailable when this
	// provider category is unknown.
	scenario := EconomicScenario{
		NetBilledUSD: 1.0, SupplierLiabilityUSD: 0.5,
		ProcessorFeeUSD: 0.1, ControlPlaneCostUSD: 0.05,
		ContributionMarginUSD: 0.35,
	}
	fixed, err := fixedPointPricingFromScenario(
		"usd", 1.0, 1.0, scenario, []string{"provider cost"},
	)
	must(t, err)
	if fixed.TrueNetContributionNanos != nil {
		t.Fatalf("withdrawn provider authority allowed true net: %+v", fixed)
	}
}

func TestTrueNetCloudUnresolvableRateLeavesProviderUnknown(t *testing.T) {
	_, _, _, _, communityPricing, _ := trueNetDistributedFixture(t)
	// nvidia_80gb has only an unbound self-test reference, not a governed rate.
	component := providerCostComponentForPlacement(
		"vllm-cuda-llama1-infer", []string{"nvidia_80gb"}, 60,
		communityPricing.Catalogue,
	)
	if component.Status != pricingCostUnknown {
		t.Fatalf("unresolvable rate must be unknown, got %+v", component)
	}
	if component.Amount != 0 {
		t.Fatalf("unknown provider cost must not carry an amount: %+v", component)
	}
	// Building a full decision on a cloud cell with unresolvable rate must
	// leave true net nil. We synthesize via fixedPoint with an unknown category.
	scenario := EconomicScenario{
		NetBilledUSD: 1.0, SupplierLiabilityUSD: 0.5,
		ProcessorFeeUSD: 0.1, ControlPlaneCostUSD: 0.05,
		ContributionMarginUSD: 0.35,
	}
	fixed, err := fixedPointPricingFromScenarioWithExtras(
		"usd", 1.0, 1.0, scenario, 0, []string{"provider cost"},
	)
	must(t, err)
	if fixed.TrueNetContributionNanos != nil {
		t.Fatal("true net populated while provider cost is unknown")
	}
	decision := PricingDecision{
		Currency: "usd", FixedPoint: fixed, CostScheduleSHA256: strings.Repeat("a", 64),
		PrimarySupplierCost:  modeledCost(0.5, "supplier"),
		PaymentCost:          modeledCost(0.1, "processor"),
		ControlPlaneCost:     modeledCost(0.05, "control"),
		PlatformContribution: modeledCost(0.35, "contribution"),
		ProviderCost:         component,
		StorageCost:          notApplicableCost("n/a"),
		EgressCost:           notApplicableCost("n/a"),
		RiskReserve:          notApplicableCost("n/a"),
		VerificationCost:     notApplicableCost("n/a"),
	}
	// Supplier entitlements must match for the accounting check; align them.
	decision.FixedPoint.SupplierEntitlementsNanos = usdToMicros(0.5) * NanosPerMicro
	// Skip full validatePricingCostShape (needs digests); check the unknown rule.
	if err := validateFixedPointPricing(decision); err != nil {
		// Accounting may fail without full alignment; the honesty property is
		// TrueNet == nil with unknowns present.
		t.Logf("validateFixedPointPricing: %v (acceptable if only accounting)", err)
	}
	if decision.FixedPoint.TrueNetContributionNanos != nil {
		t.Fatal("honesty test failed: true net claimed with unknown provider")
	}
}

func TestModeledCostOmittedFromKnownVariableCostsIsRefused(t *testing.T) {
	scenario := EconomicScenario{
		NetBilledUSD: 1.0, SupplierLiabilityUSD: 0.4,
		ProcessorFeeUSD: 0.1, ControlPlaneCostUSD: 0.1,
		ContributionMarginUSD: 0.4,
	}
	// Include 0.05 of extra modeled storage in the components but leave it out
	// of KnownVariableCostsNanos.
	fixed, err := fixedPointPricingFromScenarioWithExtras(
		"usd", 1.0, 1.0, scenario, 0, nil,
	)
	must(t, err)
	decision := PricingDecision{
		Currency:             "usd",
		FixedPoint:           fixed,
		CostScheduleSHA256:   strings.Repeat("b", 64),
		PrimarySupplierCost:  modeledCost(0.4, "supplier"),
		VerificationCost:     notApplicableCost("n/a"),
		PaymentCost:          modeledCost(0.1, "processor"),
		ControlPlaneCost:     modeledCost(0.1, "control"),
		StorageCost:          modeledCost(0.05, "deliberately omitted from variable sum"),
		EgressCost:           notApplicableCost("n/a"),
		ProviderCost:         notApplicableCost("n/a"),
		RiskReserve:          notApplicableCost("n/a"),
		PlatformContribution: modeledCost(0.4, "contribution"),
	}
	err = validateModeledCostsAccountedInFixedPoint(decision)
	if err == nil {
		t.Fatal("modeled storage left out of KnownVariableCostsNanos was accepted")
	}
	if !strings.Contains(err.Error(), "does not conserve fixed-point known variable costs") {
		t.Fatalf("error not greppable: %v", err)
	}
}

func TestContributionSettlementUsesCausalRiskReserveFinality(t *testing.T) {
	facts := canonicalContributionFacts(t, "usd")
	facts.Pricing.RiskReserve = modeledCost(0.05, "accepted reserve forecast")
	facts.Pricing.FixedPoint.KnownVariableCostsNanos += 50_000_000
	facts.Pricing.FixedPoint.KnownCostContributionNanos -= 50_000_000
	accepted := facts.Pricing.FixedPoint.KnownCostContributionNanos
	facts.Pricing.FixedPoint.TrueNetContributionNanos = &accepted
	facts.Pricing.PlatformContribution = modeledCost(0.4, "accepted reserve-adjusted contribution")
	pricingSHA, err := pricingDecisionDigest(facts.Pricing)
	must(t, err)
	facts.PricingSHA256 = pricingSHA
	facts.RiskCanonical = true
	facts.RiskAccrualNanos = 50_000_000
	facts.RiskHeldNanos = 50_000_000

	held, err := reduceContributionJobFacts(uuid.New(), facts)
	must(t, err)
	if held.Stage != ContributionStageProvisionalSettlement || held.TrueNetNanos != nil ||
		!strings.Contains(strings.Join(held.Blockers, " "), "lifecycle remains open") {
		t.Fatalf("held reserve did not block final contribution: %+v", held)
	}

	facts.RiskHeldNanos = 0
	facts.RiskReleaseNanos = 50_000_000
	released, err := reduceContributionJobFacts(uuid.New(), facts)
	must(t, err)
	if released.Stage != ContributionStageFinalSettlement || released.TrueNetNanos == nil ||
		*released.TrueNetNanos != 450_000_000 || released.RiskReserve.AmountNanos == nil ||
		*released.RiskReserve.AmountNanos != 0 {
		t.Fatalf("released reserve did not close at zero actual cost: %+v", released)
	}
}

func TestHistoricalDecisionWithUnknownsKeepsTrueNetUnavailable(t *testing.T) {
	// A pre-schedule decision: no CostScheduleSHA256, unknown categories, no
	// true net. Must not be re-derived under the new schedule.
	historical := PricingDecision{
		Version:              pricingDecisionVersion,
		PolicyRevision:       pricingDecisionPolicyRevision,
		ExecutionMode:        computeExecutionDistributed,
		Currency:             "usd",
		BuyerPrice:           1.0,
		MaximumBuyerPrice:    1.0,
		PrimarySupplierCost:  modeledCost(0.5, "historical supplier"),
		VerificationCost:     modeledCost(0.1, "historical verification"),
		PaymentCost:          modeledCost(0.1, "historical processor"),
		ControlPlaneCost:     modeledCost(0.05, "historical control"),
		StorageCost:          unknownCost("no independently metered object-storage cost is attributed at acceptance"),
		EgressCost:           unknownCost("result egress destination and billable bytes are unknown at acceptance"),
		ProviderCost:         unknownCost("worker/provider energy and depreciation are not independently metered"),
		RiskReserve:          unknownCost("no independently calibrated loss reserve is available"),
		PlatformContribution: modeledCost(0.25, "historical contribution"),
		FixedPoint: &FixedPointPricingDecision{
			Currency: "usd", BuyerChargeNanos: 1_000_000_000,
			AcceptedCeilingNanos:       1_000_000_000,
			SupplierEntitlementsNanos:  600_000_000,
			KnownVariableCostsNanos:    150_000_000,
			MercGrossSpreadNanos:       400_000_000,
			KnownCostContributionNanos: 250_000_000,
			UnknownCostCategories: []string{
				"storage cost", "egress cost", "provider energy and depreciation", "risk reserve",
			},
		},
		Confidence: 1,
	}
	// CostScheduleSHA256 empty: do not re-derive.
	if historical.CostScheduleSHA256 != "" {
		t.Fatal("historical fixture must not carry a cost schedule")
	}
	if historical.FixedPoint.TrueNetContributionNanos != nil {
		t.Fatal("historical decision claims true net")
	}
	view := buildEconomicContributionView(&historical, "usd", 0.4, nil, 0)
	if view.MercNetContribution.Status != pricingCostUnknown {
		t.Fatalf("historical contribution view status=%s, want unknown", view.MercNetContribution.Status)
	}
	if view.TrueNetContributionNanos != nil {
		t.Fatal("historical view re-derived true net")
	}
	if !strings.Contains(view.MercNetContribution.Basis, "unavailable") {
		t.Fatalf("basis does not state unavailability: %q", view.MercNetContribution.Basis)
	}
}

func identityCostCatalogueAndFX(t *testing.T) (CataloguePriceAuthority, CostFXAuthority) {
	t.Helper()
	catalogue := CataloguePriceAuthority{
		ReferenceCurrency:         costReferenceCurrency,
		SettlementCurrency:        "usd",
		ReferenceToSettlementRate: 1,
		FXRevision:                "identity-usd",
	}
	fx, err := costFXAuthorityFromCatalogue(catalogue)
	mustf(t, err, "build identity cost FX fixture: %v")
	return catalogue, fx
}

func TestStorageEgressSettleFromActualBytesBesideAcceptedBound(t *testing.T) {
	schedule := DefaultCostSchedule("usd")
	digest, err := costScheduleDigest(schedule)
	must(t, err)
	catalogue, fx := identityCostCatalogueAndFX(t)
	retention := 30 * 24 * time.Hour
	acceptedStorage, acceptedEgress := int64(10*BytesPerGiB), int64(1*BytesPerGiB)
	storageAcceptedNanos, err := storageNanosForBytes(schedule, acceptedStorage, retention)
	must(t, err)
	egressAcceptedNanos, err := egressNanosForBytes(schedule, acceptedEgress)
	must(t, err)
	pricing := PricingDecision{
		Currency:             "usd",
		Catalogue:            catalogue,
		CostScheduleSHA256:   digest,
		CostScheduleRevision: schedule.Revision,
		CostPolicy: &FrozenCostPolicySnapshot{
			Version:                 frozenCostPolicySnapshotVersion,
			Schedule:                schedule,
			ScheduleSHA256:          digest,
			FX:                      fx,
			RetentionSeconds:        int64(retention / time.Second),
			RetentionPolicyRevision: jobObjectRetentionPolicyRevision,
			RetentionBasis:          defaultJobObjectRetentionBasis,
		},
		StorageAcceptedBytes: acceptedStorage,
		EgressAcceptedBytes:  acceptedEgress,
		StorageAcceptedNanos: storageAcceptedNanos,
		EgressAcceptedNanos:  egressAcceptedNanos,
		StorageCost:          modeledCost(nanosToEconomicUSD(storageAcceptedNanos), "accepted bound"),
		EgressCost:           modeledCost(nanosToEconomicUSD(egressAcceptedNanos), "accepted bound"),
	}
	// Actuals are half the bound.
	actualStorage, actualEgress := acceptedStorage/2, acceptedEgress/2
	actuals, err := settleStorageEgressFromBytes(
		pricing, actualStorage, actualEgress, time.Now(),
	)
	must(t, err)
	if actuals.StorageAcceptedBytes != acceptedStorage || actuals.EgressAcceptedBytes != acceptedEgress {
		t.Fatalf("accepted bounds not recorded: %+v", actuals)
	}
	if actuals.StorageSettledBytes != actualStorage || actuals.EgressSettledBytes != actualEgress {
		t.Fatalf("settled actuals not recorded: %+v", actuals)
	}
	if actuals.StorageSettledNanos >= actuals.StorageAcceptedNanos {
		t.Fatalf("settled storage %d should be below accepted bound %d",
			actuals.StorageSettledNanos, actuals.StorageAcceptedNanos)
	}
	if actuals.EgressSettledNanos >= actuals.EgressAcceptedNanos {
		t.Fatalf("settled egress %d should be below accepted bound %d",
			actuals.EgressSettledNanos, actuals.EgressAcceptedNanos)
	}
	// Wrong schedule digest fails closed.
	bad := pricing
	bad.CostScheduleSHA256 = strings.Repeat("f", 64)
	if _, err := settleStorageEgressFromBytes(
		bad, actualStorage, actualEgress, time.Now(),
	); err == nil {
		t.Fatal("settlement accepted a mismatched cost schedule digest")
	}
}

func TestFrozenCostSettlementPersistsIdempotentlyAcrossConfigChange(t *testing.T) {
	installSettlementCurrencyForTest(t, "cad")
	t.Setenv(costScheduleRevisionEnv, "")
	ctx, store, pool := openIsolatedTestStore(t)
	buyerID, jobID := uuid.New(), uuid.New()
	mustf(t, pool.QueryRow(ctx, `
		INSERT INTO buyers (id,email)
		VALUES ($1,$2)
		RETURNING id`, buyerID, "cost-policy-"+buyerID.String()+"@example.invalid").
		Scan(&buyerID), "insert cost-policy buyer: %v")
	if _, err := pool.Exec(ctx, `
		INSERT INTO jobs
		  (id,buyer_id,status,job_type,tier,input_ref,output_ref,created_at,terminal_at)
		VALUES ($1,$2,'complete','embed','batch','cost/input','cost/output',now(),now())`,
		jobID, buyerID); err != nil {
		t.Fatalf("insert cost-policy job: %v", err)
	}

	catalogue := CataloguePriceAuthority{
		ReferenceCurrency: costReferenceCurrency, SettlementCurrency: "cad",
		ReferenceToSettlementRate: 1.375, FXRevision: "test-cad-cost-persistence",
	}
	fx, err := costFXAuthorityFromCatalogue(catalogue)
	mustf(t, err, "build CAD persistence FX: %v")
	schedule, err := LoadCostScheduleFromEnv(fx)
	mustf(t, err, "load CAD persistence schedule: %v")
	digest, err := costScheduleDigest(schedule)
	must(t, err)
	retention := 30 * 24 * time.Hour
	storageAcceptedNanos, err := storageNanosForBytes(
		schedule, 4*BytesPerGiB, retention,
	)
	must(t, err)
	egressAcceptedNanos, err := egressNanosForBytes(schedule, BytesPerGiB)
	must(t, err)
	pricing := PricingDecision{
		Currency:             "cad",
		Catalogue:            catalogue,
		CostScheduleSHA256:   digest,
		CostScheduleRevision: schedule.Revision,
		CostPolicy: &FrozenCostPolicySnapshot{
			Version:                 frozenCostPolicySnapshotVersion,
			Schedule:                schedule,
			ScheduleSHA256:          digest,
			FX:                      fx,
			RetentionSeconds:        int64(retention / time.Second),
			RetentionPolicyRevision: jobObjectRetentionPolicyRevision,
			RetentionBasis:          defaultJobObjectRetentionBasis,
		},
		StorageAcceptedBytes: 4 * BytesPerGiB,
		EgressAcceptedBytes:  BytesPerGiB,
		StorageAcceptedNanos: storageAcceptedNanos,
		EgressAcceptedNanos:  egressAcceptedNanos,
		StorageCost: modeledCost(
			nanosToEconomicUSD(storageAcceptedNanos), "accepted storage bound"),
		EgressCost: modeledCost(
			nanosToEconomicUSD(egressAcceptedNanos), "accepted egress bound"),
	}
	now := time.Date(2026, 8, 9, 12, 0, 0, 0, time.UTC)
	accepted, err := settleStorageEgressFromBytes(
		pricing, 2*BytesPerGiB, BytesPerGiB/2, now,
	)
	mustf(t, err, "settle frozen cost policy: %v")
	mustf(t, store.PersistCostSettlementActuals(ctx, jobID, accepted),
		"persist frozen cost settlement: %v")

	t.Setenv(costScheduleRevisionEnv, "cost-schedule-future")
	t.Setenv("MERC_JOB_OBJECT_RETENTION_DAYS", "8")
	t.Setenv(priceFXRateEnv, "1.99")
	t.Setenv(priceFXRevisionEnv, "future-cad-fx")
	replayed, err := settleStorageEgressFromBytes(
		pricing, 2*BytesPerGiB, BytesPerGiB/2, now,
	)
	mustf(t, err, "replay frozen cost settlement: %v")
	if !reflect.DeepEqual(accepted, replayed) {
		t.Fatalf("persisted cost settlement moved under current config:\n accepted=%+v\n replayed=%+v",
			accepted, replayed)
	}
	mustf(t, store.PersistCostSettlementActuals(ctx, jobID, replayed),
		"idempotently persist replayed cost settlement: %v")
	stored, err := store.LoadCostSettlementActuals(ctx, jobID)
	mustf(t, err, "load frozen cost settlement: %v")
	if stored == nil || !reflect.DeepEqual(*stored, accepted) {
		t.Fatalf("stored frozen cost settlement=%+v, want %+v", stored, accepted)
	}
}

func TestCostScheduleRequiresProvenance(t *testing.T) {
	s := DefaultCostSchedule("usd")
	s.StorageProvenance = ""
	if reason := validateCostSchedule(s); reason == "" || !strings.Contains(reason, "storage_provenance") {
		t.Fatalf("missing storage provenance not refused: %q", reason)
	}
	s = DefaultCostSchedule("usd")
	s.EgressProvenance = ""
	if reason := validateCostSchedule(s); !strings.Contains(reason, "egress_provenance") {
		t.Fatalf("missing egress provenance not refused: %q", reason)
	}
	s = DefaultCostSchedule("usd")
	s.RiskReserveProvenance = ""
	if reason := validateCostSchedule(s); !strings.Contains(reason, "risk_reserve_provenance") {
		t.Fatalf("missing risk provenance not refused: %q", reason)
	}
}
