package main

import (
	"math"
	"testing"
)

// merc's money arithmetic degrades at small magnitudes, and it does so in three
// different places for three different reasons. This file pins the third one,
// which is the only one that makes a job outright unbuyable.
//
// Found by running the Python SDK against a live merc: a job of fewer than
// about five units is rejected with "base_compute_usd must be finite and
// positive". Not throttled, not priced at a minimum -- rejected.
//
// The causal chain is worth stating because it is perverse:
//
//	a real supplier benchmarks at 1,980 embeddings/sec on an M3 Ultra
//	  -> merc reprices the catalogue from measured supplier throughput
//	  -> the per-1k price falls to $0.000018
//	  -> a 1-unit job's base compute rounds to zero at micro-USD granularity
//	  -> BuildEconomicPlan rejects it as not economically executable
//
// So a FASTER supplier makes small jobs impossible to buy. Nobody chose that;
// it falls out of rounding.
//
// These tests do not assert the current boundary is correct -- it is not. They
// assert it is where it is, so that changing it is a deliberate act with a
// visible diff, and so the day someone adds a minimum billable job size this
// file is what tells them what they changed.

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

// A base compute cost that rounds to zero blocks the job. This is the exact
// condition a small job hits after repricing.
func TestSmallJobsAreRejectedNotPricedAtAFloor(t *testing.T) {
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
		if !plan.Executable && plan.BlockReason == "" {
			t.Fatalf("%d units: not executable with no reason given", units)
		}
		t.Logf("units=%-6d base_compute=$%.12f executable=%v %s",
			units, base, plan.Executable, plan.BlockReason)
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

// KNOWN DEFECT, characterised not fixed.
//
// Between roughly 5 and 99 units the economic plan is executable, the buyer IS
// charged, and the supplier payout is EXACTLY ZERO:
//
//	units=10   buyer=$0.000124000  supplier=$0.000000000
//	units=100  buyer=$0.000125000  supplier=$0.000001000
//
// This is not the sub-cent carry the accrual path handles. SupplierPayoutPerTaskUSD
// is 0, so nothing is accrued at all -- merc bills a buyer for work and records
// no obligation to whoever performed it. roundEconomicUSD(computePerTask *
// SupplierShare) rounds 0.0000000144 to zero.
//
// This test asserts the CURRENT behaviour so the defect cannot widen unnoticed
// and so fixing it produces a visible, deliberate diff. When a minimum billable
// job size or a supplier payout floor lands, INVERT this test -- do not delete
// it.
func TestKnownDefectSupplierPaidZeroWhileBuyerIsCharged(t *testing.T) {
	schedule := economicScheduleForTest(t)
	const pricePer1K = 0.000018

	plan := BuildEconomicPlan(EconomicPlanInput{
		BaseComputeUSD:   10.0 / 1000.0 * pricePer1K,
		InitialTaskCount: 1,
		SupplierShare:    0.8,
	}, schedule)

	if !plan.Executable {
		t.Fatalf("a 10-unit job is no longer executable; the defect window moved and this "+
			"characterisation is stale: %s", plan.BlockReason)
	}
	if plan.BuyerChargePerTaskUSD <= 0 {
		t.Fatal("a 10-unit job no longer charges the buyer; re-characterise")
	}
	if plan.SupplierPayoutPerTaskUSD > 0 {
		t.Fatalf("GOOD NEWS, ACTION REQUIRED: a 10-unit job now pays the supplier $%.9f. "+
			"The zero-payout defect is fixed. Invert this test to assert the supplier is "+
			"always paid something whenever the buyer is charged.",
			plan.SupplierPayoutPerTaskUSD)
	}

	// And the first size at which the supplier is paid anything.
	paid := BuildEconomicPlan(EconomicPlanInput{
		BaseComputeUSD:   100.0 / 1000.0 * pricePer1K,
		InitialTaskCount: 1,
		SupplierShare:    0.8,
	}, schedule)
	if paid.SupplierPayoutPerTaskUSD <= 0 {
		t.Fatal("even a 100-unit job pays the supplier nothing; the defect is WIDER than " +
			"characterised")
	}
	t.Logf("defect window characterised: 10 units pays the supplier $0.000000000 while "+
		"charging the buyer $%.9f; 100 units pays $%.9f",
		plan.BuyerChargePerTaskUSD, paid.SupplierPayoutPerTaskUSD)
}
