package main

import (
	"encoding/json"
	"math"
	"strings"
	"testing"
)

func testEconomicSchedule() EconomicSchedule {
	return EconomicSchedule{
		Version:                "test-stripe-conservative-v1",
		Currency:               "usd",
		ProcessorPercent:       0.035,
		ProcessorFixedUSD:      0.35,
		ControlPlanePerTaskUSD: 0.005,
		TargetMarginRate:       0.03,
	}
}

func TestBuildEconomicPlanSeparatesBuyerSafetyFeeFromSupplierPayout(t *testing.T) {
	plan := BuildEconomicPlan(EconomicPlanInput{
		BaseComputeUSD: 0.10, InitialTaskCount: 2, ExtraTaskReserve: 2,
		SupplierShare: 0.97, SLAPremiumUSD: 0.08,
	}, testEconomicSchedule())
	if !plan.Executable {
		t.Fatalf("plan unexpectedly blocked: %s", plan.BlockReason)
	}
	if plan.BuyerChargePerTaskUSD <= plan.BaseComputePerTaskUSD {
		t.Fatalf("tiny compute must acquire an explicit buyer safety fee: %+v", plan)
	}
	wantSupplier := roundEconomicUSD((0.10 / 2) * 0.97)
	if plan.SupplierPayoutPerTaskUSD != wantSupplier {
		t.Fatalf("supplier payout=%v want base-compute payout=%v", plan.SupplierPayoutPerTaskUSD, wantSupplier)
	}
	if plan.SupplierPayoutPerTaskUSD >= plan.BuyerChargePerTaskUSD {
		t.Fatalf("buyer safety fee leaked into supplier liability: %+v", plan)
	}
	for _, scenario := range plan.Scenarios {
		if scenario.MarginHeadroomUSD < -0.000001 {
			t.Fatalf("scenario %s misses margin floor: %+v", scenario.Name, scenario)
		}
	}
}

func TestBuildEconomicPlanReservesFullLiabilityUnderFloorCentCarryPolicy(t *testing.T) {
	plan := BuildEconomicPlan(EconomicPlanInput{
		BaseComputeUSD: 0.019999, InitialTaskCount: 1, ExtraTaskReserve: 1,
		SupplierShare: 1,
	}, testEconomicSchedule())
	if !plan.Executable {
		t.Fatal(plan.BlockReason)
	}
	if plan.Version != economicPlanVersion ||
		plan.SupplierSettlementPolicy != supplierSettlementPolicyFloorMinorUnitCarryV2 {
		t.Fatalf("plan does not freeze the minor-unit policy: %+v", plan)
	}
	liabilityMicros := int64(math.Round(plan.SupplierPayoutPerTaskUSD * 1_000_000))
	cents, remainder, err := splitSupplierLiabilityMicros(liabilityMicros)
	must(t, err)
	if cents != 1 || remainder != 9_999 {
		t.Fatalf("settlement split=(%d cents,%d microusd), want (1,9999)", cents, remainder)
	}
	for _, scenario := range plan.Scenarios {
		want := roundEconomicUSD(plan.SupplierPayoutPerTaskUSD * float64(scenario.AcceptedTasks))
		if scenario.SupplierLiabilityUSD != want {
			t.Fatalf("scenario %s reserved %.6f, want complete six-decimal liability %.6f",
				scenario.Name, scenario.SupplierLiabilityUSD, want)
		}
	}
}

func TestBuildEconomicPlanOneTaskPartialCoversWholeFixedFee(t *testing.T) {
	s := testEconomicSchedule()
	plan := BuildEconomicPlan(EconomicPlanInput{
		BaseComputeUSD: 2, InitialTaskCount: 10, SupplierShare: 0.97,
	}, s)
	if !plan.Executable {
		t.Fatal(plan.BlockReason)
	}
	partial := plan.Scenarios[0]
	if partial.Name != "one_task_partial" || partial.AcceptedTasks != 1 {
		t.Fatalf("unexpected first scenario: %+v", partial)
	}
	if partial.ProcessorFeeUSD+1e-9 < s.ProcessorFixedUSD {
		t.Fatalf("partial processor fee %v omitted fixed fee %v", partial.ProcessorFeeUSD, s.ProcessorFixedUSD)
	}
	if partial.MarginHeadroomUSD < -0.000001 {
		t.Fatalf("partial job loses money: %+v", partial)
	}
}

func TestBuildEconomicPlanDoesNotRelyOnRefundableSLAPremium(t *testing.T) {
	plan := BuildEconomicPlan(EconomicPlanInput{
		BaseComputeUSD: 1, InitialTaskCount: 3, ExtraTaskReserve: 1,
		SupplierShare: 0.97, SLAPremiumUSD: 20,
	}, testEconomicSchedule())
	if !plan.Executable {
		t.Fatal(plan.BlockReason)
	}
	var met, miss EconomicScenario
	for _, scenario := range plan.Scenarios {
		switch scenario.Name {
		case "full_success_sla_met":
			met = scenario
		case "full_success_sla_miss":
			miss = scenario
		}
	}
	if miss.RefundUSD != 20 {
		t.Fatalf("SLA miss refund=%v want 20", miss.RefundUSD)
	}
	if miss.MarginHeadroomUSD < -0.000001 {
		t.Fatalf("plan depended on refundable premium: %+v", miss)
	}
	if met.ContributionMarginUSD <= miss.ContributionMarginUSD {
		t.Fatalf("met SLA should retain premium upside: met=%+v miss=%+v", met, miss)
	}
}

func TestBuildEconomicPlanFirmCapCanBlockReservedExtraWork(t *testing.T) {
	base := EconomicPlanInput{
		BaseComputeUSD: 4, InitialTaskCount: 2, ExtraTaskReserve: 4,
		SupplierShare: 0.97, SLAPremiumUSD: 0.25,
	}
	unbounded := BuildEconomicPlan(base, testEconomicSchedule())
	if !unbounded.Executable {
		t.Fatalf("unbounded plan blocked: %s", unbounded.BlockReason)
	}
	base.FirmQuoteMaxUSD = unbounded.InitialBuyerChargeUSD
	capped := BuildEconomicPlan(base, testEconomicSchedule())
	if capped.Executable {
		t.Fatalf("cap covering only initial work must not authorize reserved extra work: %+v", capped)
	}
	if capped.MinimumScenario != "max_extra_work_sla_miss" || !strings.Contains(capped.BlockReason, "margin floor") {
		t.Fatalf("wrong cap failure: %+v", capped)
	}
}

func TestBuildEconomicPlanFailsClosedOnUnknownOrInvalidInputs(t *testing.T) {
	validInput := EconomicPlanInput{BaseComputeUSD: 1, InitialTaskCount: 1, SupplierShare: 0.97}
	tests := []struct {
		name     string
		input    EconomicPlanInput
		schedule EconomicSchedule
		want     string
	}{
		{"missing schedule", validInput, EconomicSchedule{}, "version"},
		{"zero tasks", EconomicPlanInput{BaseComputeUSD: 1, SupplierShare: .97}, testEconomicSchedule(), "task_count"},
		{"nan compute", EconomicPlanInput{BaseComputeUSD: math.NaN(), InitialTaskCount: 1, SupplierShare: .97}, testEconomicSchedule(), "base_compute"},
		{"negative reserve", EconomicPlanInput{BaseComputeUSD: 1, InitialTaskCount: 1, ExtraTaskReserve: -1, SupplierShare: .97}, testEconomicSchedule(), "reserve"},
		{"missing currency", validInput, EconomicSchedule{Version: "x"}, "currency"},
		{"impossible rates", validInput, EconomicSchedule{Version: "x", Currency: "usd", ProcessorPercent: .7, TargetMarginRate: .3}, "below 1"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			plan := BuildEconomicPlan(tc.input, tc.schedule)
			if plan.Executable || !strings.Contains(plan.BlockReason, tc.want) {
				t.Fatalf("plan=%+v want blocked containing %q", plan, tc.want)
			}
		})
	}
}

func TestBuildEconomicPlanIsDeterministicAndJSONFinite(t *testing.T) {
	in := EconomicPlanInput{
		BaseComputeUSD: 3.141592, InitialTaskCount: 7, ExtraTaskReserve: 3,
		SupplierShare: .97, SLAPremiumUSD: .123456,
	}
	a := BuildEconomicPlan(in, testEconomicSchedule())
	b := BuildEconomicPlan(in, testEconomicSchedule())
	ja, err := json.Marshal(a)
	must(t, err)
	jb, err := json.Marshal(b)
	must(t, err)
	if string(ja) != string(jb) {
		t.Fatalf("non-deterministic plan:\n%s\n%s", ja, jb)
	}
	if strings.Contains(string(ja), "NaN") || strings.Contains(string(ja), "Inf") {
		t.Fatalf("non-finite JSON: %s", ja)
	}
}

func TestValidateEconomicPlanSnapshotRejectsScalarAndScenarioTampering(t *testing.T) {
	plan := BuildEconomicPlan(EconomicPlanInput{
		BaseComputeUSD: 2.5, InitialTaskCount: 3, ExtraTaskReserve: 2,
		SupplierShare: .97, SLAPremiumUSD: .4,
	}, testEconomicSchedule())
	mustf(t, ValidateEconomicPlanSnapshot(plan), "untouched plan rejected: %v")

	scalar := plan
	scalar.SupplierPayoutPerTaskUSD += .01
	if err := ValidateEconomicPlanSnapshot(scalar); err == nil {
		t.Fatal("edited frozen supplier payout was accepted")
	} else if !strings.Contains(err.Error(), "SupplierPayoutPerTaskUSD") {
		t.Fatalf("scalar mismatch did not identify its field: %v", err)
	}

	scenario := plan
	scenario.Scenarios = append([]EconomicScenario(nil), plan.Scenarios...)
	scenario.Scenarios[0].ProcessorFeeUSD = 0
	if err := ValidateEconomicPlanSnapshot(scenario); err == nil {
		t.Fatal("edited economic scenario was accepted")
	}
}

func TestLoadEconomicScheduleFromEnvFailsClosedAndParsesBasisPoints(t *testing.T) {
	for _, name := range []string{
		economicScheduleVersionEnv, processorPercentBPSEnv, processorFixedUSDEnv,
		controlPerBatchUSDEnv, minimumContributionUSDEnv, targetMarginBPSEnv,
	} {
		t.Setenv(name, "")
	}
	if _, err := LoadEconomicScheduleFromEnv(); err == nil || !strings.Contains(err.Error(), economicScheduleVersionEnv) {
		t.Fatalf("missing schedule must fail closed, got %v", err)
	}
	t.Setenv(economicScheduleVersionEnv, "operator-reviewed-2026-07")
	t.Setenv(processorPercentBPSEnv, "350")
	t.Setenv(processorFixedUSDEnv, "0.35")
	t.Setenv(controlPerBatchUSDEnv, "0.005")
	t.Setenv(minimumContributionUSDEnv, "0.000001")
	t.Setenv(targetMarginBPSEnv, "300")
	s, err := LoadEconomicScheduleFromEnv()
	must(t, err)
	if s.Currency != SettlementCurrencyCode() ||
		s.ProcessorPercent != .035 || s.TargetMarginRate != .03 || s.ProcessorFixedUSD != .35 ||
		s.ControlPlanePerBatchUSD != .005 ||
		s.MinimumContributionUSD != .000001 ||
		s.ControlPlaneAllocationPolicy != controlPlaneAllocationChargeBatchV1 {
		t.Fatalf("wrong schedule: %+v", s)
	}
	t.Setenv(processorPercentBPSEnv, "not-a-number")
	if _, err := LoadEconomicScheduleFromEnv(); err == nil || !strings.Contains(err.Error(), processorPercentBPSEnv) {
		t.Fatalf("malformed processor rate must fail closed, got %v", err)
	}
}

func FuzzBuildEconomicPlanNeverAuthorizesNegativeMargin(f *testing.F) {
	f.Add(0.01, 1, 0, 0.97, 0.0, 0.0)
	f.Add(100.0, 64, 64, 0.95, 5.0, 0.0)
	f.Add(4.0, 2, 4, 0.97, 0.25, 3.0)
	f.Fuzz(func(t *testing.T, compute float64, tasks, reserve int, share, premium, cap float64) {
		if tasks > 10_000 || tasks < -10_000 || reserve > 10_000 || reserve < -10_000 {
			t.Skip()
		}
		plan := BuildEconomicPlan(EconomicPlanInput{
			BaseComputeUSD: compute, InitialTaskCount: tasks, ExtraTaskReserve: reserve,
			SupplierShare: share, SLAPremiumUSD: premium, FirmQuoteMaxUSD: cap,
		}, testEconomicSchedule())
		if !plan.Executable {
			return
		}
		if len(plan.Scenarios) != 4 {
			t.Fatalf("executable plan missing scenarios: %+v", plan)
		}
		for _, scenario := range plan.Scenarios {
			if math.IsNaN(scenario.MarginHeadroomUSD) || math.IsInf(scenario.MarginHeadroomUSD, 0) || scenario.MarginHeadroomUSD < -0.000001 {
				t.Fatalf("executable plan authorizes negative/invalid margin: plan=%+v scenario=%+v", plan, scenario)
			}
		}
	})
}

// The per-task price floor must not be made of a card-network fee.
//
// Charges are batched -- chargeOrDeferJob defers a job until the buyer's
// outstanding total reaches chargeMinUSD and FormChargeBatch then settles the
// group on one PaymentIntent -- so a task is never charged alone.  Loading a
// whole ProcessorFixedUSD into every task's floor produced a hard $0.344547
// minimum per task no matter how cheap the compute, which is a floor that
// cannot fall as compute gets cheaper and which priced small workloads roughly
// two orders of magnitude above market.
func TestBuildEconomicPlanAmortisesProcessorFixedFeeOverChargeBatch(t *testing.T) {
	schedule := EconomicSchedule{
		Version: "test", Currency: "usd", ProcessorPercent: 0.029, ProcessorFixedUSD: 0.30,
		MinChargeBatchUSD: 5.00, ControlPlanePerTaskUSD: 0.0001, TargetMarginRate: 0.10,
	}
	in := EconomicPlanInput{
		BaseComputeUSD: 0.01, InitialTaskCount: 1000, SupplierShare: 0.97,
	}
	plan := BuildEconomicPlan(in, schedule)
	if !plan.Executable {
		t.Fatalf("plan blocked: %s", plan.BlockReason)
	}

	// The old model floored every task at (0.30 + 0.0001) / (1 - 0.029 - 0.10).
	const oldFloor = 0.344547
	if plan.BuyerChargePerTaskUSD >= oldFloor {
		t.Fatalf("buyer charge per task = %.6f, still at or above the old fixed-fee floor %.6f",
			plan.BuyerChargePerTaskUSD, oldFloor)
	}

	// A 1000-task job costing a cent in total should be priced from compute plus
	// margin, not from a $0.30 fee.  Anything above a cent a task means the fee
	// is still dominating.
	if plan.BuyerChargePerTaskUSD > 0.01 {
		t.Fatalf("buyer charge per task = %.6f, want <= 0.01 for a $0.01 job", plan.BuyerChargePerTaskUSD)
	}

	// And the whole job must still clear the margin target in every scenario.
	if plan.MinimumMarginHeadroomUSD < -0.000001 {
		t.Fatalf("scenario %s misses the margin floor by %.6f",
			plan.MinimumScenario, -plan.MinimumMarginHeadroomUSD)
	}
}

// With no batching floor configured the conservative original behaviour must
// survive: each task is assumed to trigger its own charge and must cover a whole
// fixed fee.
func TestBuildEconomicPlanKeepsStandaloneFixedFeeWhenNoBatchFloor(t *testing.T) {
	schedule := EconomicSchedule{
		Version: "test", Currency: "usd", ProcessorPercent: 0.029, ProcessorFixedUSD: 0.30,
		MinChargeBatchUSD: 0, ControlPlanePerTaskUSD: 0.0001, TargetMarginRate: 0.10,
	}
	plan := BuildEconomicPlan(EconomicPlanInput{
		BaseComputeUSD: 0.01, InitialTaskCount: 1000, SupplierShare: 0.97,
	}, schedule)
	if plan.BuyerChargePerTaskUSD < 0.344547 {
		t.Fatalf("buyer charge per task = %.6f, want the standalone floor when no batch size is known",
			plan.BuyerChargePerTaskUSD)
	}
}
