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
	inv := &InvoiceView{
		JobID: jobID, Status: "complete",
		EstimatedUSD: 9.0, ActualUSD: 8.0, ChargedUSD: 8.0,
		SupplierPaidUSD: 7.76, PlatformTakeUSD: 0.24, QuotedUSD: &quoted,
	}
	verif := Verification{RedundancyMatched: 2, Checked: 2, Label: "verified", DisputeStatus: "resolved"}
	classes := []string{"candle|abc123"}
	tasks := []TaskReceipt{
		taskReceiptRow(0, "complete", false, "candle", "abc123", "redundancy_match", "pass"),
		taskReceiptRow(0, "complete", true, "candle", "abc123", "honeypot_pass", "pass"),
	}

	rc := assembleClearingReceipt(jobID, "complete", inv, verif, classes, tasks)

	if rc.Invoice == nil || rc.Invoice.QuotedUSD == nil || *rc.Invoice.QuotedUSD != 9.5 {
		t.Fatal("receipt must carry the QUOTE")
	}
	if rc.Invoice.ActualUSD != 8.0 {
		t.Fatal("receipt must carry ACTUALS")
	}
	if rc.Invoice.SupplierPaidUSD == 0 || rc.Invoice.PlatformTakeUSD == 0 {
		t.Fatal("receipt must carry SETTLEMENT amounts")
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
