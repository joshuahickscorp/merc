package main

import (
	"math"
	"testing"
)

// Small-job economics: a minimum billable job size floors base compute so a
// supplier who performed work is never reserved $0 while the buyer is charged.
//
// History: catalogue prices from a real M3 Ultra (1,980 emb/s → $0.000018/1k)
// made sub-100-unit jobs either unbuyable (base rounded to $0) or charge the
// buyer while SupplierPayoutPerTaskUSD was exactly zero. That characterisation
// lived here until the floor landed; these tests now assert the corrected
// behaviour.

func economicScheduleForTest(t *testing.T) EconomicSchedule {
	t.Helper()
	t.Setenv("MERC_ECON_SCHEDULE_VERSION", "2026-07-19")
	t.Setenv("MERC_PROCESSOR_PERCENT_BPS", "290")
	t.Setenv("MERC_PROCESSOR_FIXED_USD", "0.30")
	t.Setenv("MERC_CONTROL_PLANE_PER_TASK_USD", "0.0001")
	t.Setenv("MERC_TARGET_MARGIN_BPS", "1000")
	schedule, err := LoadEconomicScheduleFromEnv()
	if err != nil {
		t.Fatalf("loading the economic schedule: %v", err)
	}
	return schedule
}

// Small jobs are priced at the minimum billable floor, not rejected.
func TestSmallJobsArePricedAtAFloor(t *testing.T) {
	schedule := economicScheduleForTest(t)

	// $0.000018 per 1,000 units is the catalogue price after repricing from a
	// real M3 Ultra's measured throughput.
	const pricePer1K = 0.000018

	for _, units := range []int{1, 2, 3, 4, 5, 10, 100} {
		base := float64(units) / 1000.0 * pricePer1K
		plan := BuildEconomicPlan(EconomicPlanInput{
			BaseComputeUSD:   base,
			InitialTaskCount: 1,
			SupplierShare:    0.8,
		}, schedule)

		if base <= 0 || math.IsNaN(base) {
			t.Fatalf("%d units produced a non-positive base compute (%v) before the plan "+
				"was even built", units, base)
		}
		if !plan.Executable {
			t.Fatalf("%d units: not executable after the minimum-billable floor: %s",
				units, plan.BlockReason)
		}
		if plan.SupplierPayoutPerTaskUSD <= 0 {
			t.Fatalf("%d units: supplier payout is $0 while the plan is executable", units)
		}
		if plan.BuyerChargePerTaskUSD <= 0 {
			t.Fatalf("%d units: buyer is not charged", units)
		}
		t.Logf("units=%-6d raw_base=$%.12f floored_base=$%.9f buyer=$%.9f supplier=$%.9f",
			units, base, plan.Input.BaseComputeUSD,
			plan.BuyerChargePerTaskUSD, plan.SupplierPayoutPerTaskUSD)
	}
}

// The buyer charge must never be less than what merc pays the supplier plus
// what it costs merc to run the task. If it can be, merc loses money per task
// and does so more the more it sells.
func TestBuyerChargeCoversSupplierAndControlPlaneAtEverySize(t *testing.T) {
	schedule := economicScheduleForTest(t)
	const pricePer1K = 0.000018

	for _, units := range []int{5, 10, 50, 100, 1000, 100000} {
		base := float64(units) / 1000.0 * pricePer1K
		plan := BuildEconomicPlan(EconomicPlanInput{
			BaseComputeUSD:   base,
			InitialTaskCount: 1,
			SupplierShare:    0.8,
		}, schedule)
		if !plan.Executable {
			continue
		}
		floor := plan.SupplierPayoutPerTaskUSD + schedule.ControlPlanePerTaskUSD
		if plan.BuyerChargePerTaskUSD < floor {
			t.Fatalf("units=%d: buyer charged $%.9f per task against a floor of $%.9f "+
				"(supplier $%.9f + control plane $%.9f) -- merc loses money on every task "+
				"and loses more the more it sells",
				units, plan.BuyerChargePerTaskUSD, floor,
				plan.SupplierPayoutPerTaskUSD, schedule.ControlPlanePerTaskUSD)
		}
	}
}

// The supplier's share of what the buyer pays collapses at small sizes because
// the control-plane cost per task is FIXED. Measured against a real worker: a
// 3-row job gave merc 99.2% and a 400-row job gave 51%.
//
// This asserts the collapse exists and is bounded, so that a minimum billable
// job size -- when someone adds one -- has a number to be checked against.
func TestSupplierShareCollapsesAtSmallJobSizes(t *testing.T) {
	schedule := economicScheduleForTest(t)
	const pricePer1K = 0.000018

	shareAt := func(units int) (float64, bool) {
		base := float64(units) / 1000.0 * pricePer1K
		plan := BuildEconomicPlan(EconomicPlanInput{
			BaseComputeUSD:   base,
			InitialTaskCount: 1,
			SupplierShare:    0.8,
		}, schedule)
		if !plan.Executable || plan.BuyerChargePerTaskUSD <= 0 {
			return 0, false
		}
		return plan.SupplierPayoutPerTaskUSD / plan.BuyerChargePerTaskUSD, true
	}

	small, okSmall := shareAt(10)
	large, okLarge := shareAt(1000000)
	if !okSmall || !okLarge {
		t.Skip("both sizes must price for the comparison to mean anything")
	}
	if small >= large {
		t.Fatalf("supplier share does not collapse at small sizes (small %.4f, large %.4f); "+
			"if this changed, the fixed per-task control-plane cost was removed or "+
			"amortised and this test should be rewritten rather than deleted", small, large)
	}
	t.Logf("supplier share: 10 units %.4f%%, 1e6 units %.4f%% -- the gap is the fixed "+
		"per-task control-plane cost", small*100, large*100)
}

// Whenever the buyer is charged for work a supplier performed, supplier
// liability is strictly positive. Property-style across the sizes that used to
// round the supplier share to exactly zero.
func TestSupplierLiabilityStrictlyPositiveWhenBuyerCharged(t *testing.T) {
	schedule := economicScheduleForTest(t)
	const pricePer1K = 0.000018

	for units := 1; units <= 200; units++ {
		base := float64(units) / 1000.0 * pricePer1K
		plan := BuildEconomicPlan(EconomicPlanInput{
			BaseComputeUSD:   base,
			InitialTaskCount: 1,
			SupplierShare:    0.8,
		}, schedule)
		if !plan.Executable {
			t.Fatalf("units=%d: plan not executable: %s", units, plan.BlockReason)
		}
		if plan.BuyerChargePerTaskUSD <= 0 {
			t.Fatalf("units=%d: buyer charge is not positive", units)
		}
		if plan.SupplierPayoutPerTaskUSD <= 0 {
			t.Fatalf("units=%d: buyer charged $%.9f but supplier liability is $0",
				units, plan.BuyerChargePerTaskUSD)
		}
		// Micro-USD conservation on the per-task floor: buyer covers supplier +
		// control plane (processor is modelled at scenario level).
		if plan.BuyerChargePerTaskUSD+1e-12 < plan.SupplierPayoutPerTaskUSD+schedule.ControlPlanePerTaskUSD {
			t.Fatalf("units=%d: buyer $%.9f does not cover supplier $%.9f + control $%.9f",
				units, plan.BuyerChargePerTaskUSD, plan.SupplierPayoutPerTaskUSD,
				schedule.ControlPlanePerTaskUSD)
		}
	}

	// Also over multi-task and multi-share shapes that used to hit the hole.
	for _, share := range []float64{0.1, 0.5, 0.8, 0.97, 1.0} {
		for _, tasks := range []int{1, 2, 7, 64} {
			for _, micros := range []int64{1, 2, 5, 10, 100, 1000} {
				plan := BuildEconomicPlan(EconomicPlanInput{
					BaseComputeUSD:   microsToUSD(micros),
					InitialTaskCount: tasks,
					SupplierShare:    share,
				}, schedule)
				if !plan.Executable {
					continue
				}
				if plan.BuyerChargePerTaskUSD > 0 && plan.SupplierPayoutPerTaskUSD <= 0 {
					t.Fatalf("share=%.2f tasks=%d base_micros=%d: buyer $%.9f supplier $0",
						share, tasks, micros, plan.BuyerChargePerTaskUSD)
				}
			}
		}
	}
}

// Scenario-level micro-USD conservation: for every modelled scenario,
// net billed equals supplier + processor + control + contribution margin.
func TestEconomicPlanScenarioConservation(t *testing.T) {
	schedule := economicScheduleForTest(t)
	const pricePer1K = 0.000018

	for _, units := range []int{1, 10, 100, 1000, 100000} {
		plan := BuildEconomicPlan(EconomicPlanInput{
			BaseComputeUSD:   float64(units) / 1000.0 * pricePer1K,
			InitialTaskCount: 1,
			SupplierShare:    0.8,
		}, schedule)
		if !plan.Executable {
			t.Fatalf("units=%d blocked: %s", units, plan.BlockReason)
		}
		for _, sc := range plan.Scenarios {
			// net = supplier + processor + control + margin, in micro-USD.
			lhs := usdToMicros(sc.NetBilledUSD)
			rhs := usdToMicros(sc.SupplierLiabilityUSD) +
				usdToMicros(sc.ProcessorFeeUSD) +
				usdToMicros(sc.ControlPlaneCostUSD) +
				usdToMicros(sc.ContributionMarginUSD)
			if lhs != rhs {
				// Allow 1 micro of residual from independent rounding of terms.
				if d := lhs - rhs; d > 1 || d < -1 {
					t.Fatalf("units=%d scenario %s: net %d != parts %d (supplier %d + proc %d + ctrl %d + margin %d)",
						units, sc.Name, lhs, rhs,
						usdToMicros(sc.SupplierLiabilityUSD),
						usdToMicros(sc.ProcessorFeeUSD),
						usdToMicros(sc.ControlPlaneCostUSD),
						usdToMicros(sc.ContributionMarginUSD))
				}
			}
		}
	}
}
