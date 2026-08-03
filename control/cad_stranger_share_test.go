package main

import (
	"math"
	"testing"
)

// strangerCADEconomicFixture is the three-record embed the public stranger path
// admits, priced under the first-complete-loop schedule in CAD.
//
// Corpus geometry (stranger_admission_test.go): 3 records, 233 bytes →
// max(3, 233/4) = 58.25 settlement units. Catalogue reference is $0.000018/1k;
// CAD settlement at FX 1.37 is $0.00002466/1k. Canary is off, so verification
// floors to one honeypot and zero redundancy → 2 economic tasks. Supplier share
// for all-minilm embed is 0.94.
//
// History: micro-ceiling each pro-rata batch fee share forced processor and
// control to one micro each on a four-micro continuous job; the headroom loop
// then inflated the buyer to ten micros and the supplier's commercial share
// collapsed to 0.20 (2 µ / 10 µ) while their exact entitlement was untouched.
// That is the defect this fixture pins.
func strangerCADEconomicFixture(t *testing.T) (EconomicPlan, EconomicScenario) {
	t.Helper()
	const (
		refPricePer1K = 0.00001800
		fx            = 1.37
		units         = 58.25
		share         = 0.94
		primaryTasks  = 1
		econTasks     = 2
	)
	settlementPer1K := math.Ceil(refPricePer1K*fx*1e8) / 1e8
	gross, err := CatalogueGrossNanos(
		MustParseCurrency("cad"),
		nanosPer1KFromFloat(settlementPer1K),
		NanoWorkUnitsFromFloat(units),
	)
	if err != nil {
		t.Fatalf("catalogue gross: %v", err)
	}
	base, err := mulDiv(gross.Nanos, int64(econTasks), int64(primaryTasks), false)
	if err != nil {
		t.Fatalf("scale base across economic tasks: %v", err)
	}
	// Primary gross on this fixture is the 1,436-nano figure the money-nanos
	// comments and pricing_authority_reconciliation_test record.
	if gross.Nanos != 1436 {
		t.Fatalf("CAD primary gross nanos=%d, want 1436 (fixture geometry drifted)", gross.Nanos)
	}
	schedule := EconomicSchedule{
		Version:                      "first-complete-loop-v1",
		Currency:                     "cad",
		ProcessorPercent:             0.035,
		ProcessorFixedUSD:            0.35,
		MinChargeBatchUSD:            5.00,
		ControlPlanePerBatchUSD:      0.005,
		ControlPlaneAllocationPolicy: controlPlaneAllocationChargeBatchV1,
		MinimumContributionUSD:       0.000001,
		TargetMarginRate:             0.03,
	}
	plan := BuildEconomicPlan(EconomicPlanInput{
		BaseComputeUSD:   float64(base) / float64(NanosPerMajorUnit),
		BaseComputeNanos: base,
		InitialTaskCount: econTasks,
		ExtraTaskReserve: primaryTasks,
		SupplierShare:    share,
	}, schedule)
	if !plan.Executable {
		t.Fatalf("CAD stranger plan blocked: %s", plan.BlockReason)
	}
	full, err := fullSuccessEconomicScenario(plan)
	if err != nil {
		t.Fatalf("full-success scenario: %v", err)
	}
	return plan, full
}

// TestCADStrangerSubMicroJobKeepsCommercialSupplierShare is the pure-arithmetic
// twin of TestAStrangerCanBeAdmittedForASubMicroJob/cad. It needs no object
// storage and fails closed on the 2 µ / 10 µ shape that micro-ceil of pro-rata
// batch fees produced.
func TestCADStrangerSubMicroJobKeepsCommercialSupplierShare(t *testing.T) {
	plan, full := strangerCADEconomicFixture(t)

	if full.NetBilledUSD <= 0 {
		t.Fatalf("buyer charge is not positive: %+v", full)
	}
	share := full.SupplierLiabilityUSD / full.NetBilledUSD
	if share < 0.25 {
		t.Fatalf("tiny-job supplier share is still commercially meaningless: supplier %.9f / buyer %.9f (share %.4f)",
			full.SupplierLiabilityUSD, full.NetBilledUSD, share)
	}
	// Supplier entitlement is the exact share of catalogue compute, not a test
	// subsidy. On this fixture that is 1,350 nanos/task (ceil of 1,436 × 0.94),
	// which projects to one micro each → two micros across the job.
	const wantSupplierNanosPerTask int64 = 1350
	if plan.SupplierPayoutPerTaskNanos != wantSupplierNanosPerTask {
		t.Fatalf("supplier entitlement %d nanos/task, want %d (do not move money to pass)",
			plan.SupplierPayoutPerTaskNanos, wantSupplierNanosPerTask)
	}
	if usdToMicros(full.SupplierLiabilityUSD) != 2 {
		t.Fatalf("supplier liability micros=%d, want 2 (projection of exact entitlement)",
			usdToMicros(full.SupplierLiabilityUSD))
	}
	// Continuous pro-rata fees on a four-micro job are sub-micro; they must not
	// each micro-ceil to one micro and force the buyer to ten.
	if usdToMicros(full.NetBilledUSD) >= 10 {
		t.Fatalf("buyer still inflated to %d micros by phantom fee floors; full=%+v",
			usdToMicros(full.NetBilledUSD), full)
	}
	// Money conserves exactly in micro-units.
	got := usdToMicros(full.NetBilledUSD)
	want := usdToMicros(full.SupplierLiabilityUSD) + usdToMicros(full.ProcessorFeeUSD) +
		usdToMicros(full.ControlPlaneCostUSD) + usdToMicros(full.ContributionMarginUSD)
	if got != want {
		t.Fatalf("commercial split does not conserve: buyer=%d parts=%d", got, want)
	}
	// Buyer never below supplier + required absolute contribution on the full job.
	if full.ContributionMarginUSD < full.RequiredMarginUSD-1e-12 {
		t.Fatalf("contribution below required: margin=%.9f required=%.9f",
			full.ContributionMarginUSD, full.RequiredMarginUSD)
	}
	t.Logf("CAD stranger: buyer=%dµ supplier=%dµ proc=%dµ ctrl=%dµ margin=%dµ share=%.4f entitlement=%dn/task",
		usdToMicros(full.NetBilledUSD), usdToMicros(full.SupplierLiabilityUSD),
		usdToMicros(full.ProcessorFeeUSD), usdToMicros(full.ControlPlaneCostUSD),
		usdToMicros(full.ContributionMarginUSD), share, plan.SupplierPayoutPerTaskNanos)
}

// TestProRataBatchFeeDoesNotMicroCeilSubMicroShare pins the fee projection
// itself: a continuous share well below half a micro rounds to zero, not one.
func TestProRataBatchFeeDoesNotMicroCeilSubMicroShare(t *testing.T) {
	// 4 micros of a $5 batch: fixedShare = 4e-6/5 = 8e-7
	// control 0.005 * 8e-7 = 4e-9 → half-away micro = 0
	// processor 4e-6*0.035 + 0.35*8e-7 = 1.4e-7 + 2.8e-7 = 4.2e-7 → 0
	schedule := EconomicSchedule{
		Version: "v", Currency: "cad",
		ProcessorPercent: 0.035, ProcessorFixedUSD: 0.35, MinChargeBatchUSD: 5,
		ControlPlanePerBatchUSD: 0.005, ControlPlaneAllocationPolicy: controlPlaneAllocationChargeBatchV1,
		MinimumContributionUSD: 0.000001, TargetMarginRate: 0.03,
	}
	const net = 0.000004
	if got := schedule.processorFeeFor(net); got != 0 {
		t.Fatalf("pro-rata processor fee on 4µ charge = %.9f, want 0 (continuous ≈0.42µ)", got)
	}
	if got := schedule.controlPlaneCostFor(net, 2); got != 0 {
		t.Fatalf("pro-rata control fee on 4µ charge = %.9f, want 0 (continuous ≈0.004µ)", got)
	}
	// A full-batch charge still ceils the whole fixed fee.
	if got := schedule.processorFeeFor(5.0); got < 0.35 {
		t.Fatalf("standalone processor fee %.9f omitted the $0.35 fixed fee", got)
	}
}
