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
		SupplierPaidUSD: 7.76, PlatformTakeUSD: 0.24, QuotedUSD: &quoted,
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
	if err != nil {
		t.Fatal(err)
	}
	computePlan, err := newDistributedComputePlan(
		workload,
		1,
		128,
		1,
		1,
		0,
		0,
		QuoteTime{P50Secs: 10, P90Secs: 20, WorstCaseSecs: 40},
		"static",
		0.1,
		0,
		QuoteConfidence{Score: 0.9, Reasons: []string{"receipt fixture"}},
		nil,
	)
	if err != nil {
		t.Fatal(err)
	}

	rc := assembleClearingReceipt(jobID, "complete", &workload, &computePlan, inv, verif, classes, tasks)

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
