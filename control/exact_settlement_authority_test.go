package main

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
)

// testChargeBatchSchedule is the modern schedule under which FixedPoint and
// exact nanos are the settlement authority.
func testChargeBatchSchedule() EconomicSchedule {
	return EconomicSchedule{
		Version:                      "test-charge-batch-v1",
		Currency:                     "usd",
		ProcessorPercent:             0.035,
		ProcessorFixedUSD:            0.35,
		MinChargeBatchUSD:            5.0,
		ControlPlanePerBatchUSD:      0.05,
		ControlPlaneAllocationPolicy: controlPlaneAllocationChargeBatchV1,
		MinimumContributionUSD:       0.000010,
		TargetMarginRate:             0.03,
	}
}

// ---------------------------------------------------------------------------
// 1. 1436-nano / 0.97-share end-to-end: supplier credited 1393, not 2000.
// ---------------------------------------------------------------------------

func TestExactSettlementAuthority1436NanosShare097(t *testing.T) {
	const (
		catalogueGrossNanos = int64(1436)
		share               = 0.97
		wantSupplierNanos   = int64(1393) // ceil(1436 * 0.97) via SupplierEntitlementNanos
		legacyFloatNanos    = int64(2000) // ceilEconomicUSD path paid this before the fix
	)

	// Unit half: exactPerTaskNanos itself.
	base, supplier, buyer, err := exactPerTaskNanos(
		MustParseCurrency("usd"),
		float64(catalogueGrossNanos)/float64(NanosPerMajorUnit),
		share,
		float64(catalogueGrossNanos)/float64(NanosPerMajorUnit), // buyer at least base
		1,
		catalogueGrossNanos,
	)
	mustf(t, err, "exactPerTaskNanos: %v")
	if base != catalogueGrossNanos {
		t.Fatalf("base nanos=%d, want %d", base, catalogueGrossNanos)
	}
	if supplier != wantSupplierNanos {
		t.Fatalf("supplier nanos=%d, want %d (legacy float path was %d)",
			supplier, wantSupplierNanos, legacyFloatNanos)
	}
	if buyer < supplier {
		t.Fatalf("buyer nanos %d < supplier %d", buyer, supplier)
	}

	// Plan half: BuildEconomicPlan freezes the same entitlement.
	plan := BuildEconomicPlan(EconomicPlanInput{
		BaseComputeUSD:   float64(catalogueGrossNanos) / float64(NanosPerMajorUnit),
		BaseComputeNanos: catalogueGrossNanos,
		InitialTaskCount: 1,
		ExtraTaskReserve: 1,
		SupplierShare:    share,
	}, testChargeBatchSchedule())
	if !plan.Executable {
		t.Fatalf("plan blocked: %s", plan.BlockReason)
	}
	if plan.EconomicRoundingPolicy != economicRoundingPolicy {
		t.Fatalf("plan policy %q, want %q", plan.EconomicRoundingPolicy, economicRoundingPolicy)
	}
	if plan.SupplierPayoutPerTaskNanos != wantSupplierNanos {
		t.Fatalf("plan supplier nanos=%d, want %d", plan.SupplierPayoutPerTaskNanos, wantSupplierNanos)
	}
	// The float projection must NOT be the old 2000-nano ceil.
	if usdToMicros(plan.SupplierPayoutPerTaskUSD)*NanosPerMicro == legacyFloatNanos &&
		plan.SupplierPayoutPerTaskNanos == legacyFloatNanos {
		t.Fatalf("plan still freezes the legacy 2000-nano float entitlement")
	}

	// FixedPoint half: entitlements equal plan nanos × task count.
	scenario, err := fullSuccessEconomicScenario(plan)
	must(t, err)
	fixed, err := fixedPointPricingFromPlan(plan, scenario, []string{"storage cost"})
	mustf(t, err, "fixedPointPricingFromPlan: %v")
	if fixed.SupplierEntitlementsNanos != wantSupplierNanos {
		t.Fatalf("FixedPoint.SupplierEntitlementsNanos=%d, want %d",
			fixed.SupplierEntitlementsNanos, wantSupplierNanos)
	}

	// Ledger half: splitFrozenChargeNanos projects the same 1393, not 2000.
	buyerID, supplierID, taskID := uuid.New(), uuid.New(), uuid.New()
	entries, err := splitFrozenChargeNanos(
		buyerID, supplierID, taskID, "usd",
		plan.BuyerChargePerTaskNanos, plan.SupplierPayoutPerTaskNanos,
		90, time.Unix(100, 0),
	)
	mustf(t, err, "splitFrozenChargeNanos: %v")
	if len(entries) != 3 {
		t.Fatalf("entries=%d", len(entries))
	}
	// Supplier credit is the micro projection of 1393 nanos (= 1 micro = 0.000001),
	// never the 2-micro projection of the old 2000-nano float path.
	gotSupplierMicros := usdToMicros(entries[1].AmountUSD)
	wantSupplierMicros := projectNanosToMicros(wantSupplierNanos)
	if gotSupplierMicros != wantSupplierMicros {
		t.Fatalf("ledger supplier credit %d micros, want %d (from %d nanos); legacy was %d micros",
			gotSupplierMicros, wantSupplierMicros, wantSupplierNanos,
			projectNanosToMicros(legacyFloatNanos))
	}
	// Authority recorded on the settlement path is still 1393 nanos.
	if plan.SupplierPayoutPerTaskNanos != wantSupplierNanos {
		t.Fatalf("settlement authority drifted to %d", plan.SupplierPayoutPerTaskNanos)
	}
}

// ---------------------------------------------------------------------------
// 2. Tampered job_economic_plans.buyer_charge_per_task_usd refused.
// ---------------------------------------------------------------------------

func TestTamperedDenormalizedPlanMoneyRefused(t *testing.T) {
	ctx, store, pool := openMoneyPathStore(t)
	f := seedMoneyPathFixture(t, ctx, store, pool, moneyPathSeedOpts{
		TaskCount: 1, SeedJob: true, SeedPlanRows: true,
	})
	// Force an exact-policy plan with nanos so the assert has something to check.
	plan := BuildEconomicPlan(EconomicPlanInput{
		BaseComputeUSD: 0.01, BaseComputeNanos: 10_000_000,
		InitialTaskCount: 1, ExtraTaskReserve: 1, SupplierShare: 0.97,
	}, testChargeBatchSchedule())
	if plan.EconomicRoundingPolicy != economicRoundingPolicy {
		t.Fatalf("fixture plan lacks exact policy: %+v", plan)
	}
	// Re-seed job with exact plan.
	reseedExactPlanJob(t, ctx, pool, f, plan)

	// Tamper the denormalized USD column directly (plan_json untouched).
	// job_economic_plans is immutable via trigger — disable trigger for the
	// attack simulation, then re-enable.
	if _, err := pool.Exec(ctx, `ALTER TABLE job_economic_plans DISABLE TRIGGER job_economic_plans_immutable`); err != nil {
		t.Fatalf("disable immutability for tamper fixture: %v", err)
	}
	t.Cleanup(func() {
		_, _ = pool.Exec(context.Background(),
			`ALTER TABLE job_economic_plans ENABLE TRIGGER job_economic_plans_immutable`)
	})
	if _, err := pool.Exec(ctx, `
		UPDATE job_economic_plans
		   SET buyer_charge_per_task_usd = buyer_charge_per_task_usd + 0.01
		 WHERE job_id=$1`, f.JobID); err != nil {
		t.Fatalf("tamper buyer_charge_per_task_usd: %v", err)
	}

	// consumeEconomicReserveTx must refuse.
	tx, err := pool.Begin(ctx)
	must(t, err)
	defer tx.Rollback(ctx)
	// Reserve needs a free slot and a live job.
	if _, err := pool.Exec(ctx, `
		UPDATE jobs SET status='running', charge_status='not_attempted' WHERE id=$1`, f.JobID); err != nil {
		t.Fatal(err)
	}
	if _, err := tx.Exec(ctx, `
		UPDATE job_economic_reserves SET reserved_tasks=2, consumed_tasks=0 WHERE job_id=$1`, f.JobID); err != nil {
		// may not exist if seed didn't create reserves with room
		t.Logf("reserve prep: %v", err)
	}
	_, err = consumeEconomicReserveTx(ctx, tx, f.JobID)
	if err == nil || !strings.Contains(err.Error(), errDenormalizedPlanMoneyMismatch) {
		t.Fatalf("consumeEconomicReserveTx accepted tampered plan: err=%v", err)
	}

	// Settlement must refuse too.
	_, err = loadObservedOutputSettlement(ctx, pool, f.TaskIDs[0])
	if err == nil || !strings.Contains(err.Error(), errDenormalizedPlanMoneyMismatch) {
		t.Fatalf("loadObservedOutputSettlement accepted tampered plan: err=%v", err)
	}
}

// ---------------------------------------------------------------------------
// 3. Tampered tasks.economic_* nanos refused at settlement.
// ---------------------------------------------------------------------------

func TestTamperedTaskEconomicNanosRefusedAtSettlement(t *testing.T) {
	ctx, store, pool := openMoneyPathStore(t)
	f := seedMoneyPathFixture(t, ctx, store, pool, moneyPathSeedOpts{TaskCount: 1})
	plan := BuildEconomicPlan(EconomicPlanInput{
		BaseComputeUSD: 0.01, BaseComputeNanos: 10_000_000,
		InitialTaskCount: 1, ExtraTaskReserve: 1, SupplierShare: 0.97,
	}, testChargeBatchSchedule())
	reseedExactPlanJob(t, ctx, pool, f, plan)

	// Tamper task nanos (disable immutability trigger).
	if _, err := pool.Exec(ctx, `ALTER TABLE tasks DISABLE TRIGGER tasks_frozen_economics_immutable`); err != nil {
		t.Fatalf("disable task economics trigger: %v", err)
	}
	t.Cleanup(func() {
		_, _ = pool.Exec(context.Background(),
			`ALTER TABLE tasks ENABLE TRIGGER tasks_frozen_economics_immutable`)
	})
	if _, err := pool.Exec(ctx, `
		UPDATE tasks
		   SET economic_supplier_payout_nanos = economic_supplier_payout_nanos + 1
		 WHERE id=$1`, f.TaskIDs[0]); err != nil {
		t.Fatalf("tamper task nanos: %v", err)
	}

	_, err := loadObservedOutputSettlement(ctx, pool, f.TaskIDs[0])
	if err == nil || !strings.Contains(err.Error(), errTaskEconomicNanosMismatch) {
		t.Fatalf("settlement accepted tampered task nanos: err=%v", err)
	}
}

// ---------------------------------------------------------------------------
// 4. exactTaskEconomics failure with exact policy refuses pricing decision.
// ---------------------------------------------------------------------------

func TestExactPolicyExactTaskEconomicsFailureRefusesPricing(t *testing.T) {
	supplierShare := supplierShareForTest(
		t, "batch_infer", "llama-3.2-1b-instruct-q4")
	// Build a real exact-policy plan so ValidateEconomicPlanSnapshot accepts it.
	economic := BuildEconomicPlan(EconomicPlanInput{
		BaseComputeUSD: 0.01, BaseComputeNanos: 10_000_000,
		InitialTaskCount: 4, ExtraTaskReserve: 2, SupplierShare: supplierShare,
	}, testChargeBatchSchedule())
	if economic.EconomicRoundingPolicy != economicRoundingPolicy ||
		economic.SupplierPayoutPerTaskNanos <= 0 {
		t.Fatalf("fixture plan is not exact: policy=%q supplier=%d",
			economic.EconomicRoundingPolicy, economic.SupplierPayoutPerTaskNanos)
	}

	workload, compute, _ := pricingComputePlanFixture(t)
	// Align compute total tasks with the economic plan for the composite check.
	// computePlanFixture uses InitialTaskCount 4 with ExtraTaskReserve 2 for its
	// own economic; the compute plan total is independent of our economic above
	// as long as TotalInitialTasks matches economic.Input.InitialTaskCount.
	if compute.TotalInitialTasks != economic.Input.InitialTaskCount {
		// rebuild economic to match the fixture compute geometry
		economic = BuildEconomicPlan(EconomicPlanInput{
			BaseComputeUSD: compute.BaseComputeUSD + compute.VerificationOverheadUSD,
			BaseComputeNanos: int64(math.Round((compute.BaseComputeUSD + compute.VerificationOverheadUSD) *
				float64(NanosPerMajorUnit))),
			InitialTaskCount: compute.TotalInitialTasks,
			ExtraTaskReserve: economicExtraTaskReserve(compute.PrimaryTasks),
			SupplierShare:    supplierShare,
		}, testChargeBatchSchedule())
	}
	// Money-sum agreement for ValidateComputePlanEconomicSnapshot.
	economic.Input.BaseComputeUSD = settlementBaseFromComputeEstimate(
		roundEconomicUSD(compute.BaseComputeUSD+compute.VerificationOverheadUSD),
		economic.Input.SupplierShare, economic.Input.InitialTaskCount,
	)
	// Re-build so snapshot validation round-trips.
	economic = BuildEconomicPlan(economic.Input, economic.Schedule)
	if economic.EconomicRoundingPolicy != economicRoundingPolicy {
		t.Fatalf("rebuilt plan lost exact policy")
	}

	// Catalogue priced so low that exactTaskEconomics yields a zero gross for
	// the fixture's per-task units — "cannot price exactly".
	authority := catalogueAuthorityFixture(
		t, workload, economic.Schedule.Currency, economic.Input.SupplierShare,
	)
	authority.ReferencePricePer1K = 1e-8
	authority.SettlementPricePer1K = ceilPricePer1K(authority.ReferencePricePer1K * authority.ReferenceToSettlementRate)
	// Placement ceiling derivation uses the catalogue; rebuild with the tiny price.
	placement := placementForPricingFixture(t, workload, authority)

	// Sanity: exactTaskEconomics itself refuses at this price for the plan's units.
	unitsPerTask := pricingBillableUnitsForComputePlan(compute) / float64(compute.PrimaryTasks)
	if _, _, eerr := exactTaskEconomics(authority, workload.Binding.Tier, unitsPerTask); eerr == nil {
		t.Fatalf("precondition: exactTaskEconomics unexpectedly succeeded at units=%g price=%g",
			unitsPerTask, authority.SettlementPricePer1K)
	}

	unitsPerSec, _, err := admissionUnitsPerSec(
		workload.RuntimeJobType, authority.ModelID,
		admissionCellsForWorkload(workload), time.Now(),
	)
	mustf(t, err, "admission units/sec: %v")
	_, err = distributedPricingDecisionAtRate(
		workload, compute, placement, economic, authority,
		workload.Binding.Tier, "", unitsPerSec,
	)
	if err == nil {
		t.Fatal("pricing decision accepted exact policy with failed exactTaskEconomics")
	}
	if !strings.Contains(err.Error(), "exactTaskEconomics failed") &&
		!strings.Contains(err.Error(), economicRoundingPolicy) {
		t.Fatalf("error does not identify exact-policy failure: %v", err)
	}
}

// ---------------------------------------------------------------------------
// 5. Plan whose exactPerTaskNanos fails does not carry EconomicRoundingPolicy.
// ---------------------------------------------------------------------------

func TestExactPerTaskNanosFailureDoesNotStampPolicy(t *testing.T) {
	// Negative task count is rejected before exact; use a share that breaks
	// MoneyNanosFromUSDFloat / SupplierEntitlementNanos by being out of range
	// after the schedule validation? Supplier share is validated in (0,1].
	//
	// exactPerTaskNanos fails when tasks <= 0, but BuildEconomicPlan blocks
	// that earlier. The remaining failure mode is an absurd base that overflows
	// — use a baseComputeNanos of 0 with a non-finite float base blocked too.
	//
	// Practical approach: call exactPerTaskNanos directly with bad inputs, and
	// assert BuildEconomicPlan with a normal plan that has BaseComputeNanos=0
	// and a float base that converts fine STILL stamps policy (success).
	// For failure-to-stamp: when base compute is so large it overflows.
	_, _, _, err := exactPerTaskNanos(
		MustParseCurrency("usd"), 1, 0.97, 1, 0, 0, // tasks=0
	)
	if err == nil {
		t.Fatal("exactPerTaskNanos accepted zero tasks")
	}

	// A plan built without any base that can yield positive nanos must not
	// claim the exact policy. Blocked plans leave policy empty.
	blocked := BuildEconomicPlan(EconomicPlanInput{
		BaseComputeUSD: 0, InitialTaskCount: 1, SupplierShare: 0.97,
	}, testChargeBatchSchedule())
	if blocked.EconomicRoundingPolicy != "" {
		t.Fatalf("blocked plan stamped policy %q", blocked.EconomicRoundingPolicy)
	}
	if blocked.SupplierPayoutPerTaskNanos != 0 || blocked.BuyerChargePerTaskNanos != 0 {
		t.Fatalf("blocked plan carried nanos: %+v", blocked)
	}

	// Successful exact derivation DOES stamp the policy.
	ok := BuildEconomicPlan(EconomicPlanInput{
		BaseComputeUSD: 0.01, BaseComputeNanos: 10_000_000,
		InitialTaskCount: 1, SupplierShare: 0.97,
	}, testChargeBatchSchedule())
	if ok.EconomicRoundingPolicy != economicRoundingPolicy {
		t.Fatalf("successful exact plan missing policy: %q", ok.EconomicRoundingPolicy)
	}
	if ok.SupplierPayoutPerTaskNanos <= 0 {
		t.Fatalf("successful exact plan has no supplier nanos")
	}
}

// ---------------------------------------------------------------------------
// 6. Observed-output rebate that would breach contribution floor is clamped.
// ---------------------------------------------------------------------------

func TestObservedOutputRebateClampedToContributionFloor(t *testing.T) {
	schedule := testChargeBatchSchedule()
	// Freeze large enough that floor is meaningful; unused share near 100%
	// would otherwise drop to minBillable.
	frozenCharge := 1.0
	frozenPayout := 0.50
	// Huge unused output share → token-proportional billed near minBillable.
	got := settleObservedOutputTokensWithSchedule(
		frozenCharge, frozenPayout,
		0, 0, false,
		1, 1_000_000, // estimatedIn, estimatedOut
		1, 1_000_000, // records, maxTokens
		0, true, // reported 0 tokens
		schedule, 1,
	)
	if !got.FloorClamped {
		t.Fatalf("expected contribution-floor clamp, got %+v", got)
	}
	if got.UnclampedRebateUSD <= 0 {
		t.Fatalf("clamp fired but unclamped rebate not recorded: %+v", got)
	}
	if got.RebateUSD >= got.UnclampedRebateUSD {
		t.Fatalf("clamped rebate %.6f should be < unclamped %.6f",
			got.RebateUSD, got.UnclampedRebateUSD)
	}
	// Settled charge must still cover supplier + processor + control + required.
	processor := schedule.processorFeeFor(got.BilledCharge)
	control := schedule.controlPlaneCostFor(got.BilledCharge, 1)
	required := roundEconomicUSD(math.Max(
		got.BilledCharge*schedule.TargetMarginRate,
		schedule.MinimumContributionUSD,
	))
	margin := roundEconomicUSD(got.BilledCharge - got.SupplierPayout - processor - control)
	if margin+1e-12 < required {
		t.Fatalf("clamped settlement still breaches floor: charge=%.6f supplier=%.6f "+
			"processor=%.6f control=%.6f margin=%.6f required=%.6f",
			got.BilledCharge, got.SupplierPayout, processor, control, margin, required)
	}
	if got.BilledCharge > frozenCharge {
		t.Fatalf("clamp raised charge above freeze")
	}
}

// ---------------------------------------------------------------------------
// 7. Legacy row (nanos NULL) settles exactly as today.
// ---------------------------------------------------------------------------

func TestLegacyNullNanosSettlementUnchanged(t *testing.T) {
	// Pin current float numbers first, then assert settleObservedOutputTokens
	// (no schedule, no nanos) still produces them.
	const (
		frozenCharge = 1.0
		frozenPayout = 0.70
		estimatedIn  = 1.0
		estimatedOut = int64(256)
		records      = int64(1)
		maxTokens    = uint32(256)
		observed     = int64(5)
	)
	// Pre-computed under the historical formula (no floor clamp, no nanos).
	outputUnitShare := float64(estimatedOut) / (estimatedIn + float64(estimatedOut))
	unusedShare := outputUnitShare * (1.0 - float64(observed)/float64(256))
	wantCharge := roundUSD(frozenCharge * (1.0 - unusedShare))
	if wantCharge < minBillableSettlementUSD {
		wantCharge = minBillableSettlementUSD
	}
	wantPayout := roundUSD(frozenPayout * wantCharge / frozenCharge)

	got := settleObservedOutputTokens(
		frozenCharge, frozenPayout,
		estimatedIn, estimatedOut,
		records, maxTokens,
		observed, true,
	)
	if got.HasNanos {
		t.Fatal("legacy path must not set HasNanos")
	}
	if got.BilledCharge != wantCharge || got.SupplierPayout != wantPayout {
		t.Fatalf("legacy settlement drifted: charge=%.6f/%.6f payout=%.6f/%.6f",
			got.BilledCharge, wantCharge, got.SupplierPayout, wantPayout)
	}
	if got.FloorClamped {
		t.Fatal("legacy path without schedule must not floor-clamp")
	}

	// splitFrozenCharge (float form) still builds the same three rows.
	buyer, supplier, task := uuid.New(), uuid.New(), uuid.New()
	entries := splitFrozenCharge(buyer, supplier, task, "usd",
		got.BilledCharge, got.SupplierPayout, 90, time.Unix(100, 0))
	if len(entries) != 3 {
		t.Fatalf("entries=%d", len(entries))
	}
	if entries[0].AmountUSD != -got.BilledCharge ||
		entries[1].AmountUSD != got.SupplierPayout ||
		entries[2].AmountUSD != got.BilledCharge-got.SupplierPayout {
		t.Fatalf("legacy ledger split drifted: %+v", entries)
	}
}

// ---------------------------------------------------------------------------
// 8. Conservation property: buyer = supplier + variable + contribution in nanos.
// ---------------------------------------------------------------------------

func TestExactSettlementConservationProperty(t *testing.T) {
	schedule := testChargeBatchSchedule()
	cataloguePrices := []int64{1436, 10_000, 100_000, 1_000_000, 50_000_000}
	shares := []float64{0.90, 0.97, 1.0}
	taskCounts := []int{1, 2, 5, 10}

	for _, gross := range cataloguePrices {
		for _, share := range shares {
			for _, tasks := range taskCounts {
				t.Run(fmt.Sprintf("g%d_s%.2f_t%d", gross, share, tasks), func(t *testing.T) {
					plan := BuildEconomicPlan(EconomicPlanInput{
						BaseComputeUSD:   float64(gross) / float64(NanosPerMajorUnit),
						BaseComputeNanos: gross,
						InitialTaskCount: tasks,
						ExtraTaskReserve: tasks,
						SupplierShare:    share,
					}, schedule)
					if !plan.Executable {
						t.Skipf("plan not executable: %s", plan.BlockReason)
					}
					if plan.EconomicRoundingPolicy != economicRoundingPolicy {
						t.Fatalf("missing exact policy")
					}
					if plan.SupplierPayoutPerTaskNanos <= 0 || plan.BuyerChargePerTaskNanos <= 0 {
						t.Fatalf("missing nanos: supplier=%d buyer=%d",
							plan.SupplierPayoutPerTaskNanos, plan.BuyerChargePerTaskNanos)
					}
					if plan.SupplierPayoutPerTaskNanos > plan.BuyerChargePerTaskNanos {
						t.Fatalf("supplier %d > buyer %d",
							plan.SupplierPayoutPerTaskNanos, plan.BuyerChargePerTaskNanos)
					}

					scenario, err := fullSuccessEconomicScenario(plan)
					must(t, err)
					fixed, err := fixedPointPricingFromPlan(plan, scenario, []string{"x"})
					mustf(t, err, "fixed point: %v")
					// Conservation in integers.
					if fixed.BuyerChargeNanos != fixed.SupplierEntitlementsNanos+
						fixed.KnownVariableCostsNanos+fixed.KnownCostContributionNanos {
						t.Fatalf("conservation broken: buyer %d != supplier %d + var %d + contrib %d",
							fixed.BuyerChargeNanos, fixed.SupplierEntitlementsNanos,
							fixed.KnownVariableCostsNanos, fixed.KnownCostContributionNanos)
					}
					// Exact entitlement equals ledger credit authority.
					if fixed.SupplierEntitlementsNanos != plan.SupplierPayoutPerTaskNanos*int64(tasks) {
						t.Fatalf("FixedPoint supplier %d != plan %d × %d",
							fixed.SupplierEntitlementsNanos, plan.SupplierPayoutPerTaskNanos, tasks)
					}
					if err := validateFixedPointMatchesPlan(
						PricingDecision{FixedPoint: fixed, Currency: plan.Schedule.Currency},
						plan, scenario,
					); err != nil {
						t.Fatal(err)
					}

					// Ledger credits the projection of the same nanos.
					entries, err := splitFrozenChargeNanos(
						uuid.New(), uuid.New(), uuid.New(), "usd",
						plan.BuyerChargePerTaskNanos, plan.SupplierPayoutPerTaskNanos,
						0, time.Unix(1, 0),
					)
					must(t, err)
					if usdToMicros(entries[1].AmountUSD) != projectNanosToMicros(plan.SupplierPayoutPerTaskNanos) {
						t.Fatalf("ledger supplier micros != projection of plan nanos")
					}
				})
			}
		}
	}
}

// reseedExactPlanJob replaces the fixture's plan/job/task money freeze with an
// exact-policy plan (used by tamper tests).
func reseedExactPlanJob(t *testing.T, ctx context.Context, pool *pgxpool.Pool, f moneyPathFixture, plan EconomicPlan) {
	t.Helper()
	f.Plan = plan
	// Clean any prior seed rows for this job id.
	_, _ = pool.Exec(ctx, `DELETE FROM tasks WHERE job_id=$1`, f.JobID)
	_, _ = pool.Exec(ctx, `DELETE FROM job_economic_reserves WHERE job_id=$1`, f.JobID)
	_, _ = pool.Exec(ctx, `DELETE FROM job_economic_plans WHERE job_id=$1`, f.JobID)
	_, _ = pool.Exec(ctx, `DELETE FROM jobs WHERE id=$1`, f.JobID)

	if _, err := pool.Exec(ctx, `
		INSERT INTO jobs (id,buyer_id,status,job_type,model_ref,input_ref,task_count,tasks_done,
		                  offered_rate_usd_hr,min_memory_gb,tier,estimated_usd,actual_usd,
		                  firm_quote,sla_premium_usd,charge_status)
		VALUES ($1,$2,'running','embed','all-minilm-l6-v2','money/input',1,0,
		        10.0,0,'batch',$3,0,false,0,'not_attempted')`,
		f.JobID, f.BuyerID, plan.InitialBuyerChargeUSD); err != nil {
		t.Fatalf("insert job: %v", err)
	}
	planJSON, err := json.Marshal(plan)
	must(t, err)
	if _, err := pool.Exec(ctx, `
		INSERT INTO job_economic_plans (
		  job_id,plan_version,schedule_version,plan_json,initial_task_count,
		  buyer_charge_per_task_usd,supplier_payout_per_task_usd,
		  initial_buyer_charge_usd,reserved_buyer_charge_usd,sla_premium_usd,
		  buyer_charge_per_task_nanos,supplier_payout_per_task_nanos
		) VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12)`,
		f.JobID, plan.Version, plan.Schedule.Version, planJSON,
		plan.Input.InitialTaskCount, plan.BuyerChargePerTaskUSD, plan.SupplierPayoutPerTaskUSD,
		plan.InitialBuyerChargeUSD, plan.ReservedBuyerChargeUSD, plan.Input.SLAPremiumUSD,
		plan.BuyerChargePerTaskNanos, plan.SupplierPayoutPerTaskNanos); err != nil {
		t.Fatalf("insert plan: %v", err)
	}
	if _, err := pool.Exec(ctx, `
		INSERT INTO job_economic_reserves (job_id,reserved_tasks,consumed_tasks)
		VALUES ($1,$2,0)`, f.JobID, plan.Input.ExtraTaskReserve); err != nil {
		t.Fatalf("insert reserve: %v", err)
	}
	taskID := f.TaskIDs[0]
	if taskID == uuid.Nil {
		taskID = uuid.New()
		f.TaskIDs = []uuid.UUID{taskID}
	}
	resultKey := taskAttemptResultKey(f.JobID, taskID, 0)
	if _, err := pool.Exec(ctx, `
		INSERT INTO tasks
		  (id,job_id,status,input_ref,result_key,chunk_index,retry_count,
		   economic_buyer_charge_usd,economic_supplier_payout_usd,
		   economic_buyer_charge_nanos,economic_supplier_payout_nanos)
		VALUES ($1,$2,'running','money/input',$3,0,0,$4,$5,$6,$7)`,
		taskID, f.JobID, resultKey,
		plan.BuyerChargePerTaskUSD, plan.SupplierPayoutPerTaskUSD,
		plan.BuyerChargePerTaskNanos, plan.SupplierPayoutPerTaskNanos); err != nil {
		t.Fatalf("insert task: %v", err)
	}
}
