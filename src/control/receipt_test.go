package main

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/google/uuid"
)

func TestAssembleClearingReceipt(t *testing.T) {
	jobID := uuid.New()
	quoted := 9.5
	processorFee := 0.25
	platformNet := -0.01
	allocationMethod := batchFeeAllocationHamiltonV1
	inv := &InvoiceView{
		JobID: jobID, Status: "complete",
		EstimatedUSD: 9.0, ActualUSD: 8.0, ChargedUSD: 8.0,
		SupplierPaidUSD: 7.76, PlatformTakeUSD: 0.24,
		PlatformGrossSpreadUSD: 0.24, QuotedUSD: &quoted,
		ProcessorFeeAllocatedUSD:     &processorFee,
		ProcessorFeeAllocationMethod: &allocationMethod,
		PlatformNetAfterProcessorUSD: &platformNet,
	}
	verif := Verification{RedundancyMatched: 2, Checked: 2, Label: "verified", DisputeStatus: "resolved"}
	classes := []string{"candle|abc123"}
	tasks := []TaskReceipt{
		taskReceiptRow(0, "complete", false, "candle", "abc123", "redundancy_match", "pass"),
		taskReceiptRow(0, "complete", true, "candle", "abc123", "honeypot_pass", "pass"),
	}
	workload, err := buildWorkloadDecision(validBatchWorkloadSubmit(t), strings.Repeat("e", 64))
	must(t, err)
	computePlan, err := newDistributedComputePlan(
		workload,
		1,
		128,
		testInputDepthProfile(1),
		1,
		1,
		0,
		0,
		quoteTimeFromETABands(10, 0, false),
		"static",
		0.1,
		0,
		QuoteConfidence{Score: 0.9, Reasons: []string{"receipt fixture"}},
		nil,
	)
	must(t, err)

	rc := assembleClearingReceipt(
		jobID, "complete", &workload, &computePlan, nil, nil,
		inv, verif, classes, tasks,
	)

	if rc.Invoice == nil || rc.Invoice.QuotedUSD == nil || *rc.Invoice.QuotedUSD != 9.5 {
		t.Fatal("receipt must carry the QUOTE")
	}
	if rc.Invoice.ActualUSD != 8.0 {
		t.Fatal("receipt must carry ACTUALS")
	}
	if rc.Invoice.SupplierPaidUSD == 0 || rc.Invoice.PlatformTakeUSD == 0 {
		t.Fatal("receipt must carry SETTLEMENT amounts")
	}
	if rc.Invoice.ProcessorFeeAllocatedUSD == nil || *rc.Invoice.ProcessorFeeAllocatedUSD != 0.25 {
		t.Fatal("receipt must carry processor-fee reconciliation attribution")
	}
	if rc.Invoice.ProcessorFeeAllocationMethod == nil ||
		*rc.Invoice.ProcessorFeeAllocationMethod != batchFeeAllocationHamiltonV1 {
		t.Fatal("receipt must identify the processor-fee allocation method")
	}
	if rc.Invoice.PlatformNetAfterProcessorUSD == nil || *rc.Invoice.PlatformNetAfterProcessorUSD != -0.01 {
		t.Fatal("receipt must carry platform net after processor fee")
	}
	if rc.Verification.Label != "verified" || rc.Verification.DisputeStatus != "resolved" {
		t.Fatal("receipt must carry VERIFICATION + DISPUTE")
	}
	if len(rc.Classes) != 1 || rc.Classes[0] != "candle|abc123" {
		t.Fatal("receipt must carry the verification CLASS")
	}
	if len(rc.Tasks) != 2 || rc.Tasks[0].WorkerClass != "candle|abc123" || rc.Tasks[0].VerificationKind != "redundancy_match" || rc.Tasks[0].Verdict != "pass" {
		t.Fatalf("receipt must carry the per-task drilldown with worker class + event; got %+v", rc.Tasks)
	}
	if rc.Workload == nil || rc.Workload.BindingSHA256 != workload.BindingSHA256 {
		t.Fatal("receipt must carry the frozen workload decision")
	}
	if rc.ComputePlan == nil || rc.ComputePlan.WorkloadDecisionSHA256 != computePlan.WorkloadDecisionSHA256 {
		t.Fatal("receipt must carry the frozen compute plan")
	}
}

func TestTaskReceiptNeverLeaksHoneypotAnswer(t *testing.T) {
	tr := taskReceiptRow(3, "complete", true, "candle", "h1", "honeypot_pass", "pass")
	if !tr.IsHoneypot || tr.VerificationKind != "honeypot_pass" || tr.WorkerClass != "candle|h1" {
		t.Fatalf("honeypot task receipt should show the probe + class + outcome; got %+v", tr)
	}
	b, _ := json.Marshal(tr)
	lower := strings.ToLower(string(b))
	if strings.Contains(lower, "answer") || strings.Contains(lower, "result") {
		t.Fatalf("a task drilldown must NOT expose any answer/result field; got %s", b)
	}
}

// TestHoneypotIdentityExposureStaysUnreachable is a tripwire, not a behaviour
// test.
//
// TaskReceipt.IsHoneypot tells the buyer which task was the probe, and the test
// directly above deliberately pins that exposure. Worker dispatch correctly omits
// the flag, so today only the buyer can see it — which is safe for exactly one
// reason: no honeypot task can be admitted at all. An independent-supplier
// canary still requires a heterogeneous honeypot that uniform v1 cannot
// allocate, and validateCurrentUniformTaskCounts refuses any honeypot task
// even on the operator-controlled path that may quote without one. The field
// is therefore always false on accepted work.
//
// If those refusals are ever lifted while the buyer surface still carries the
// flag, a buyer colluding with a supplier can tell it exactly which task is the
// probe, and honeypot verification stops proving anything. The buyer surface
// must change FIRST. This test fails at that moment and names the change.
func TestHoneypotIdentityExposureStaysUnreachable(t *testing.T) {
	if err := validateCurrentUniformCanaryAuthority(CanaryPolicy{Enabled: true}); err == nil {
		t.Fatal("honeypot admission is no longer refused for an independent-supplier canary, so " +
			"TaskReceipt.IsHoneypot is now a live verification-evasion channel: stop exposing " +
			"is_honeypot on the buyer receipt (src/control/receipt.go TaskReceipt) before enabling " +
			"honeypot tasks")
	}
	if err := validateCurrentUniformCanaryAuthority(singleOperatorControlledCanaryForTest()); err == nil {
		t.Fatal("one-supplier operator-controlled canary no longer refuses honeypot admission, so " +
			"the skip is trading a real control for a redundancy vote that cannot be independent")
	}
	if err := validateCurrentUniformTaskCounts(1, 1, 1); err == nil {
		t.Fatal("honeypot tasks are now admitted, so TaskReceipt.IsHoneypot is now a " +
			"live verification-evasion channel: stop exposing is_honeypot on the buyer receipt " +
			"(src/control/receipt.go TaskReceipt) before enabling honeypot tasks")
	}
}

func TestClearingReceiptExposesExactCompositePricingAndReconciliation(t *testing.T) {
	workload, compute, placement, _, pricing := distributedPricingFixture(t)
	jobID := uuid.New()
	processor := 0.01
	invoice := &InvoiceView{
		JobID: jobID, Currency: pricing.Currency,
		ActualUSD: pricing.BuyerPrice,
		SupplierPaidUSD: roundEconomicUSD(
			pricing.PrimarySupplierCost.Amount + pricing.VerificationCost.Amount,
		),
		PlatformTakeUSD:          pricing.PlatformContribution.Amount,
		PlatformGrossSpreadUSD:   pricing.PlatformContribution.Amount,
		ProcessorFeeAllocatedUSD: &processor,
	}
	receipt := assembleClearingReceipt(
		jobID, "complete", &workload, &compute, &placement, &pricing,
		invoice, Verification{}, nil, nil,
	)
	wantPricingSHA, err := pricingDecisionDigest(pricing)
	must(t, err)
	if receipt.AuthorityStatus != "verified" ||
		receipt.Authority.PricingDecisionSHA256 != wantPricingSHA ||
		receipt.Authority.PlacementRequirementSHA256 !=
			pricing.PlacementRequirementSHA256 {
		t.Fatalf("receipt lost composite authority: %+v", receipt.Authority)
	}
	if receipt.Reconciliation == nil ||
		receipt.Reconciliation.CatalogueScheduleSHA256 !=
			pricing.Catalogue.ScheduleSHA256 ||
		receipt.Reconciliation.FXRevision != pricing.Catalogue.FXRevision ||
		receipt.Reconciliation.AcceptedBuyerPrice != pricing.BuyerPrice ||
		receipt.Reconciliation.SettledSupplierCost != invoice.SupplierPaidUSD {
		t.Fatalf("receipt lost pricing reconciliation: %+v", receipt.Reconciliation)
	}
}

func TestContributionViewNeverCallsGrossSpreadTrueNetWhileCostsAreUnknown(t *testing.T) {
	// Historical decision shape: no cost schedule, named costs still unknown.
	// The live distributed fixture now models storage/egress/risk under the
	// cost schedule, so true net is reachable there; this test guards the
	// historical reporting path.
	pricing := PricingDecision{
		Currency:             "usd",
		PrimarySupplierCost:  modeledCost(0.01, "supplier"),
		VerificationCost:     modeledCost(0.001, "verification"),
		PaymentCost:          modeledCost(0.000002, "processor"),
		ControlPlaneCost:     modeledCost(0.000001, "control"),
		StorageCost:          unknownCost("no independently metered object-storage cost"),
		EgressCost:           unknownCost("result egress unknown"),
		ProviderCost:         unknownCost("provider energy unmetered"),
		RiskReserve:          unknownCost("no calibrated reserve"),
		PlatformContribution: modeledCost(0.01, "gross before unknown costs"),
	}
	processor := 0.000002
	view := buildEconomicContributionView(&pricing, pricing.Currency, 0.011562, &processor, 0)
	if view.MercGrossSpread.AmountUSD == nil || *view.MercGrossSpread.AmountUSD != 0.011562 {
		t.Fatalf("gross spread missing: %+v", view.MercGrossSpread)
	}
	if view.MercNetContribution.Status != pricingCostUnknown ||
		view.MercNetContribution.AmountUSD != nil {
		t.Fatalf("unknown costs were presented as true net contribution: %+v",
			view.MercNetContribution)
	}

	known := pricing
	known.StorageCost = modeledCost(0.000001, "metered storage allocation")
	known.EgressCost = modeledCost(0.000001, "metered egress allocation")
	known.ProviderCost = notApplicableCost("supplier entitlement covers external provider")
	known.RiskReserve = modeledCost(0.000001, "calibrated refund reserve")
	view = buildEconomicContributionView(&known, known.Currency, 0.011562, &processor, 0)
	if view.MercNetContribution.Status != pricingCostUnknown ||
		view.MercNetContribution.AmountUSD != nil ||
		view.TrueNetContributionNanos != nil {
		t.Fatalf("accepted pricing masqueraded as final contribution: %+v",
			view.MercNetContribution)
	}
	if !strings.Contains(view.MercNetContribution.Basis, "FINAL ContributionSettlement") {
		t.Fatalf("contribution refusal does not name final settlement authority: %+v",
			view.MercNetContribution)
	}
}
