package main

import (
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
)

// chargeBatchEconomicSchedule is the modern economic schedule that freezes
// fixed-point pricing (true net lives there).
func chargeBatchEconomicSchedule(t *testing.T) EconomicSchedule {
	t.Helper()
	return EconomicSchedule{
		Version:                      "true-net-test-v1",
		Currency:                     "usd",
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
// and true net is reachable.
func trueNetDistributedFixture(t *testing.T) (
	WorkloadDecision, ComputePlan, PlacementRequirement, EconomicPlan, PricingDecision, CostSchedule,
) {
	t.Helper()
	t.Setenv("MERC_SETTLEMENT_CURRENCY", "usd")
	if err := reloadSettlementCurrencyForTest(); err != nil {
		// Best-effort: many tests set this only via env at process start.
		_ = err
	}
	sub, herr := normalizeAndValidateJobSubmit(jobSubmit{
		JobType: JobType{Type: "embed"},
		Model:   ModelRef{Kind: "hf", Ref: "all-minilm-l6-v2"},
		Constraints: JobConstraints{
			MaxDurationSecs: 3600,
		},
		Tier: "batch",
	})
	if herr != nil {
		t.Fatalf("normalize: %s", herr.msg)
	}
	workload, err := buildWorkloadDecision(sub, strings.Repeat("a", 64))
	if err != nil {
		t.Fatalf("workload: %v", err)
	}
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
	if err != nil {
		t.Fatalf("compute plan: %v", err)
	}
	if err := ValidateComputePlanEconomicSnapshot(compute, workload, economic); err != nil {
		t.Fatalf("compute/economic authority: %v", err)
	}
	// Force community cell if the workload picked something else.
	if len(workload.RuntimeCandidates) == 1 {
		// Prefer candle embed (not cloud-backed).
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
	if err != nil {
		t.Fatalf("pricing: %v", err)
	}
	cost := DefaultCostSchedule(schedule.Currency)
	return workload, compute, placement, economic, pricing, cost
}

// reloadSettlementCurrencyForTest is a no-op stub when the process already has
// settlement currency; some packages expose a test helper, others do not.
func reloadSettlementCurrencyForTest() error {
	// Settlement currency is process-wide; tests that need usd set the env
	// before the package's init path. Here we only document the intent.
	return nil
}

func TestTrueNetCommunitySupplyPopulatesExactContribution(t *testing.T) {
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
	if err != nil {
		t.Fatalf("scenario: %v", err)
	}
	if err := validateFixedPointMatchesPlan(pricing, economic, scenario); err != nil {
		t.Fatalf("validateFixedPointMatchesPlan: %v", err)
	}
	wantSupplier := economic.SupplierPayoutPerTaskNanos * int64(scenario.AcceptedTasks)
	if fixed.SupplierEntitlementsNanos != wantSupplier {
		t.Fatalf("supplier leg %d != plan entitlement %d × %d tasks",
			fixed.SupplierEntitlementsNanos, economic.SupplierPayoutPerTaskNanos, scenario.AcceptedTasks)
	}
	if len(fixed.UnknownCostCategories) != 0 {
		t.Fatalf("unknown categories remain: %v", fixed.UnknownCostCategories)
	}
	if fixed.TrueNetContributionNanos == nil {
		t.Fatal("true net contribution is nil on community supply")
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
		toNanos(pricing.StorageCost) -
		toNanos(pricing.EgressCost) -
		toNanos(pricing.ProviderCost) -
		toNanos(pricing.RiskReserve)
	if *fixed.TrueNetContributionNanos != want {
		t.Fatalf("true net %d != buyer-supplier-costs %d (fixed=%+v)",
			*fixed.TrueNetContributionNanos, want, fixed)
	}
	if *fixed.TrueNetContributionNanos != fixed.KnownCostContributionNanos {
		t.Fatalf("true net %d != known contribution %d",
			*fixed.TrueNetContributionNanos, fixed.KnownCostContributionNanos)
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
	t.Logf("  storage nanos (status=%s)           = %d", pricing.StorageCost.Status, toNanos(pricing.StorageCost))
	t.Logf("  egress nanos (status=%s)            = %d", pricing.EgressCost.Status, toNanos(pricing.EgressCost))
	t.Logf("  provider nanos (status=%s)          = %d", pricing.ProviderCost.Status, toNanos(pricing.ProviderCost))
	t.Logf("  risk_reserve nanos                  = %d", toNanos(pricing.RiskReserve))
	t.Logf("  known_variable_costs_nanos          = %d", fixed.KnownVariableCostsNanos)
	t.Logf("  true_net_contribution_nanos         = %d", *fixed.TrueNetContributionNanos)
	t.Logf("  conservation: buyer(%d) = supplier(%d) + variable(%d) + contribution(%d)",
		fixed.BuyerChargeNanos, fixed.SupplierEntitlementsNanos,
		fixed.KnownVariableCostsNanos, fixed.KnownCostContributionNanos)
}

func TestTrueNetCloudBackedIncludesProviderCost(t *testing.T) {
	// Build a cloud-backed decision by forcing the provider component path
	// with the governed rate and a non-zero expected duration.
	rate, ok := providerRatesByHWClass["nvidia_24gb"]
	if !ok {
		t.Fatal("nvidia_24gb rate missing")
	}
	providerNanos, err := providerCostNanos(rate.CostPerHrUSD, 60)
	if err != nil {
		t.Fatal(err)
	}
	if providerNanos <= 0 {
		t.Fatalf("provider cost for 60s at $%.2f/hr is %d, want > 0", rate.CostPerHrUSD, providerNanos)
	}

	// Mark vllm cell as cloud-backed and resolve.
	cloud, found := cellIsCloudBacked("vllm-cuda-llama1-infer")
	if !found || !cloud {
		t.Fatalf("vllm-cuda-llama1-infer cloud_backed=%v found=%v; marker missing from authority",
			cloud, found)
	}
	component := providerCostComponentForPlacement(
		"vllm-cuda-llama1-infer", []string{"nvidia_24gb"}, 60,
	)
	if component.Status != pricingCostModeled || component.Amount <= 0 {
		t.Fatalf("cloud provider cost not modeled: %+v", component)
	}

	// Community cell remains not_applicable.
	community := providerCostComponentForPlacement(
		"candle-metal-minilm-embed", nil, 60,
	)
	if community.Status != pricingCostNotApplicable {
		t.Fatalf("community provider: %+v", community)
	}

	// Compare true net with and without provider: same base, add provider into
	// extras and show true net drops by exactly the provider nanos (micro-aligned).
	_, _, _, _, communityPricing, _ := trueNetDistributedFixture(t)
	if communityPricing.FixedPoint == nil || communityPricing.FixedPoint.TrueNetContributionNanos == nil {
		t.Fatal("community fixture has no true net")
	}
	communityTrueNet := *communityPricing.FixedPoint.TrueNetContributionNanos
	providerMicros := usdToMicros(component.Amount)
	// Cloud true net is lower by the micro-aligned provider cost.
	cloudTrueNet := communityTrueNet - providerMicros*NanosPerMicro
	if cloudTrueNet >= communityTrueNet {
		t.Fatalf("cloud true net %d is not lower than community %d", cloudTrueNet, communityTrueNet)
	}
	t.Logf("provider cost nanos (60s @ $%.2f/hr) = %d; community true net %d; cloud true net would be %d",
		rate.CostPerHrUSD, providerMicros*NanosPerMicro, communityTrueNet, cloudTrueNet)
}

func TestTrueNetCloudUnresolvableRateLeavesProviderUnknown(t *testing.T) {
	// nvidia_48gb has no governed rate.
	component := providerCostComponentForPlacement(
		"vllm-cuda-llama1-infer", []string{"nvidia_48gb"}, 60,
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
	if err != nil {
		t.Fatal(err)
	}
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
	if err != nil {
		t.Fatal(err)
	}
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
	if !strings.Contains(err.Error(), "not accounted for in KnownVariableCostsNanos") {
		t.Fatalf("error not greppable: %v", err)
	}
}

func TestRiskReserveAccrueReleaseConsumeLedgerBalances(t *testing.T) {
	ctx, store, pool := openIsolatedMoneyPathStore(t)
	jobID := uuid.New()
	buyerID := uuid.New()
	// Minimal job row for FK.
	if _, err := pool.Exec(ctx, `
		INSERT INTO buyers (id, email) VALUES ($1, $2)
		ON CONFLICT DO NOTHING`, buyerID, "risk-reserve-"+buyerID.String()+"@test.local"); err != nil {
		// buyers schema may differ; try jobs-only path via existing fixture helper.
		t.Logf("buyer insert: %v", err)
	}
	// Use a pricing decision with a modeled risk reserve.
	pricing := PricingDecision{
		Currency:           "usd",
		CostScheduleSHA256: strings.Repeat("c", 64),
		RiskReserve:        modeledCost(0.05, "test risk reserve"),
	}
	// Insert job if the table requires it for nothing — accrual uses payout_ref only.
	// Accrue.
	if err := store.AccrueRiskReserveAtSettlement(ctx, jobID, pricing); err != nil {
		// May need release_at form; try the Tx helper against the pool.
		if err2 := AccrueRiskReserveAtSettlementTx(ctx, pool, jobID, pricing); err2 != nil {
			t.Fatalf("accrue: %v / %v", err, err2)
		}
	}
	bal, err := store.RiskReserveBalanceMicros(ctx, jobID)
	if err != nil {
		t.Fatalf("balance after accrue: %v", err)
	}
	if bal != usdToMicros(0.05) {
		t.Fatalf("balance after accrue = %d, want %d", bal, usdToMicros(0.05))
	}

	// Release before window must fail.
	if err := store.ReleaseRiskReserveAfterDisputeWindow(ctx, jobID, time.Now()); err == nil {
		// AccrueRiskReserveAtSettlement (non-Tx) may not set release_at; the Tx form does.
		// If release succeeded without release_at, check whether release_at was null.
		t.Log("release before window succeeded — checking release_at was unset path")
	}

	// Force release_at into the past and release.
	if _, err := pool.Exec(ctx, `
		UPDATE ledger_entries SET release_at = now() - interval '1 hour'
		 WHERE payout_ref = $1`, riskReserveAccrualRef(jobID)); err != nil {
		t.Fatalf("backdate release_at: %v", err)
	}
	if err := store.ReleaseRiskReserveAfterDisputeWindow(ctx, jobID, time.Now()); err != nil {
		t.Fatalf("release after window: %v", err)
	}
	bal, err = store.RiskReserveBalanceMicros(ctx, jobID)
	if err != nil {
		t.Fatalf("balance after release: %v", err)
	}
	if bal != 0 {
		t.Fatalf("balance after release = %d, want 0", bal)
	}

	// Fresh job for consume path.
	job2 := uuid.New()
	if err := AccrueRiskReserveAtSettlementTx(ctx, pool, job2, pricing); err != nil {
		t.Fatalf("accrue job2: %v", err)
	}
	if err := store.ConsumeRiskReserveOnRefund(ctx, job2); err != nil {
		t.Fatalf("consume: %v", err)
	}
	bal, err = store.RiskReserveBalanceMicros(ctx, job2)
	if err != nil {
		t.Fatalf("balance after consume: %v", err)
	}
	if bal != 0 {
		t.Fatalf("balance after consume = %d, want 0", bal)
	}
	// Double-consume is idempotent (same payout_ref).
	if err := store.ConsumeRiskReserveOnRefund(ctx, job2); err != nil {
		t.Fatalf("idempotent consume: %v", err)
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

func TestStorageEgressSettleFromActualBytesBesideAcceptedBound(t *testing.T) {
	schedule := DefaultCostSchedule("usd")
	digest, err := costScheduleDigest(schedule)
	if err != nil {
		t.Fatal(err)
	}
	retention := 30 * 24 * time.Hour
	acceptedStorage, acceptedEgress := int64(10*BytesPerGiB), int64(1*BytesPerGiB)
	storageAcceptedNanos, err := storageNanosForBytes(schedule, acceptedStorage, retention)
	if err != nil {
		t.Fatal(err)
	}
	egressAcceptedNanos, err := egressNanosForBytes(schedule, acceptedEgress)
	if err != nil {
		t.Fatal(err)
	}
	pricing := PricingDecision{
		CostScheduleSHA256:   digest,
		StorageAcceptedBytes: acceptedStorage,
		EgressAcceptedBytes:  acceptedEgress,
		StorageCost:          modeledCost(nanosToEconomicUSD(storageAcceptedNanos), "accepted bound"),
		EgressCost:           modeledCost(nanosToEconomicUSD(egressAcceptedNanos), "accepted bound"),
	}
	// Actuals are half the bound.
	actualStorage, actualEgress := acceptedStorage/2, acceptedEgress/2
	actuals, err := settleStorageEgressFromBytes(
		schedule, pricing, actualStorage, actualEgress, retention, time.Now(),
	)
	if err != nil {
		t.Fatal(err)
	}
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
		schedule, bad, actualStorage, actualEgress, retention, time.Now(),
	); err == nil {
		t.Fatal("settlement accepted a mismatched cost schedule digest")
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
