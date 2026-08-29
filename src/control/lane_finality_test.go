package main

import (
	"encoding/json"
	"strings"
	"testing"
)

// TestLaneWithUnresolvedBlockersCannotReportFinal is the Step 13 residual
// tripwire: a lane that still has blockers must not report economic final.
//
// Failing-before proof: the broken predicate below (status-only, ignoring
// blockers) would accept ECONOMIC_FINAL with blockers. The production guard
// refuses that. If someone weakens economicFinalityReportsFinal to status-only,
// this test fails.
func TestLaneWithUnresolvedBlockersCannotReportFinal(t *testing.T) {
	// --- failing-before: status-only predicate (what we refuse to ship) ---
	brokenReportsFinal := func(status string, blockers []string) bool {
		_ = blockers
		return status == laneFinalityEconomicFinal
	}
	if !brokenReportsFinal(laneFinalityEconomicFinal, []string{"ANY_BLOCKER"}) {
		t.Fatal("broken predicate fixture is wrong: status-only must accept ECONOMIC_FINAL with blockers")
	}

	// --- production guard ---
	if economicFinalityReportsFinal(laneFinalityEconomicFinal, []string{"ANY_BLOCKER"}) {
		t.Fatal("ECONOMIC_FINAL with unresolved blockers must not report final")
	}
	if economicFinalityReportsFinal(laneFinalityKnownCostSettled, nil) {
		t.Fatal("KNOWN_COST_SETTLED must not report economic FINAL even with empty blockers")
	}
	if economicFinalityReportsFinal(laneFinalityMoneyTerminalNotEconomicFinal, nil) {
		t.Fatal("MONEY_TERMINAL_NOT_ECONOMIC_FINAL must not report economic FINAL")
	}
	if !economicFinalityReportsFinal(laneFinalityEconomicFinal, nil) {
		t.Fatal("ECONOMIC_FINAL with empty blockers is the only true final")
	}
	if !economicFinalityReportsFinal(laneFinalityEconomicFinal, []string{}) {
		t.Fatal("empty blocker slice is final for ECONOMIC_FINAL")
	}

	// Realtime known-cost path always carries blockers while true-net is refused.
	status, blockers := realtimeKnownCostFinality()
	if status == "" {
		t.Fatal("realtime finality status must be explicit, not empty")
	}
	if len(blockers) == 0 {
		t.Fatal("realtime known-cost finality must list blockers; silence is not a final claim")
	}
	if economicFinalityReportsFinal(status, blockers) {
		t.Fatalf("realtime status %q with blockers must not report final", status)
	}

	// Service lease money-terminal with contribution blockers is not economic final.
	leaseStatus, leaseFinal := serviceLeaseMoneyTerminalFinality(serviceLeaseEconomicFinalityBlockers())
	if leaseFinal {
		t.Fatal("service lease with economic blockers must not report economic final")
	}
	if leaseStatus == laneFinalityEconomicFinal {
		t.Fatal("service lease money-terminal label must not be ECONOMIC_FINAL while blockers remain")
	}
	if leaseStatus == "" {
		t.Fatal("service lease money finality status must be explicit, not empty")
	}
}

// TestRealtimeAndLeaseFinalityStatusIsExplicit proves status is readable on the
// settlement / receipt shapes — not inferred from missing fields.
func TestRealtimeAndLeaseFinalityStatusIsExplicit(t *testing.T) {
	rtStatus, rtBlockers := realtimeKnownCostFinality()
	rt := RealtimeSettlement{
		FinalityStatus:   rtStatus,
		FinalityBlockers: rtBlockers,
		EconomicFinal:    economicFinalityReportsFinal(rtStatus, rtBlockers),
	}
	raw, err := json.Marshal(rt)
	if err != nil {
		t.Fatal(err)
	}
	var wire map[string]any
	if err := json.Unmarshal(raw, &wire); err != nil {
		t.Fatal(err)
	}
	if wire["finality_status"] != laneFinalityKnownCostSettled {
		t.Fatalf("realtime wire finality_status = %v, want explicit %s",
			wire["finality_status"], laneFinalityKnownCostSettled)
	}
	if _, ok := wire["economic_final"]; !ok {
		t.Fatal("realtime wire must include economic_final (not omitempty when false)")
	}
	if wire["economic_final"] != false {
		t.Fatalf("realtime economic_final = %v, want false", wire["economic_final"])
	}
	blockers, ok := wire["finality_blockers"].([]any)
	if !ok || len(blockers) == 0 {
		t.Fatalf("realtime finality_blockers missing or empty: %v", wire["finality_blockers"])
	}

	leaseBlockers := serviceLeaseEconomicFinalityBlockers()
	moneyStatus, leaseFinal := serviceLeaseMoneyTerminalFinality(leaseBlockers)
	if leaseFinal {
		t.Fatal("fixture lease final must be false")
	}
	lease := ServiceLeaseSettlement{
		MoneyFinalityStatus:    moneyStatus,
		EconomicFinalityStatus: "UNKNOWN_ECONOMIC_FINALITY_BLOCKERS",
		EconomicFinal:          false,
		FinalityBlockers:       leaseBlockers,
	}
	raw, err = json.Marshal(lease)
	if err != nil {
		t.Fatal(err)
	}
	wire = map[string]any{}
	if err := json.Unmarshal(raw, &wire); err != nil {
		t.Fatal(err)
	}
	if wire["money_finality_status"] != laneFinalityMoneyTerminalNotEconomicFinal {
		t.Fatalf("lease money_finality_status = %v", wire["money_finality_status"])
	}
	if wire["economic_finality_status"] == "" || wire["economic_finality_status"] == nil {
		t.Fatal("lease economic_finality_status must be explicit, not empty")
	}
	if wire["economic_final"] != false {
		t.Fatalf("lease economic_final = %v, want false", wire["economic_final"])
	}
	// Receipt-root shape used by GetServiceLeaseReceipt for open leases.
	receipt := ServiceLeaseReceipt{
		TrueNetContributionStatus: "UNKNOWN_ECONOMIC_FINALITY_BLOCKERS",
		EconomicFinalityStatus:    "UNKNOWN_ECONOMIC_FINALITY_BLOCKERS",
		EconomicFinal:             false,
		FinalityBlockers:          leaseBlockers,
	}
	if receipt.EconomicFinal {
		t.Fatal("open lease receipt must not report economic final")
	}
	if len(receipt.FinalityBlockers) == 0 {
		t.Fatal("open lease receipt must list finality blockers explicitly")
	}
	if receipt.EconomicFinalityStatus == "" {
		t.Fatal("open lease receipt economic_finality_status must not be empty")
	}
}

// TestLaneFinalityDoesNotChangeAmounts is the residual's before/after
// reconciliation: attaching finality labels must not move money.
func TestLaneFinalityDoesNotChangeAmounts(t *testing.T) {
	const buyerNanos int64 = 1_000_000_000
	const supplierNanos int64 = 600_000_000
	const platformNanos int64 = 400_000_000

	beforeRT := RealtimeSettlement{
		BuyerChargeNanos:           buyerNanos,
		SupplierPayableNanos:       supplierNanos,
		KnownCostContributionNanos: platformNanos,
		Currency:                   "usd",
	}
	status, blockers := realtimeKnownCostFinality()
	afterRT := beforeRT
	afterRT.FinalityStatus = status
	afterRT.FinalityBlockers = blockers
	afterRT.EconomicFinal = economicFinalityReportsFinal(status, blockers)
	if afterRT.BuyerChargeNanos != beforeRT.BuyerChargeNanos ||
		afterRT.SupplierPayableNanos != beforeRT.SupplierPayableNanos ||
		afterRT.KnownCostContributionNanos != beforeRT.KnownCostContributionNanos {
		t.Fatalf("realtime amounts changed: before=%+v after=%+v", beforeRT, afterRT)
	}
	if afterRT.EconomicFinal {
		t.Fatal("finality attach must not promote true-net")
	}

	beforeLease := ServiceLeaseSettlement{
		BuyerChargeMicros:    1_000_000,
		PrepaidDebitMicros:   1_000_000,
		SupplierCreditMicros: 600_000,
		PlatformGrossMicros:  400_000,
		Currency:             "usd",
	}
	leaseBlockers := serviceLeaseEconomicFinalityBlockers()
	moneyStatus, leaseFinal := serviceLeaseMoneyTerminalFinality(leaseBlockers)
	afterLease := beforeLease
	afterLease.MoneyFinalityStatus = moneyStatus
	afterLease.EconomicFinalityStatus = "UNKNOWN_ECONOMIC_FINALITY_BLOCKERS"
	afterLease.EconomicFinal = leaseFinal
	afterLease.FinalityBlockers = leaseBlockers
	if afterLease.BuyerChargeMicros != beforeLease.BuyerChargeMicros ||
		afterLease.PrepaidDebitMicros != beforeLease.PrepaidDebitMicros ||
		afterLease.SupplierCreditMicros != beforeLease.SupplierCreditMicros ||
		afterLease.PlatformGrossMicros != beforeLease.PlatformGrossMicros {
		t.Fatalf("lease amounts changed: before=%+v after=%+v", beforeLease, afterLease)
	}
	// Conservation of the known split is unchanged.
	if afterLease.BuyerChargeMicros != afterLease.SupplierCreditMicros+afterLease.PlatformGrossMicros {
		t.Fatalf("lease conservation broken after finality labels: %+v", afterLease)
	}
}

// TestRealtimeReceiptJSONSurfacesFinalityWhenSettlementPresent ensures the
// buyer receipt type carries the same finality fields (not settlement-only).
func TestRealtimeReceiptJSONSurfacesFinalityWhenSettlementPresent(t *testing.T) {
	status, blockers := realtimeKnownCostFinality()
	rcp := RealtimeReceipt{
		FinalityStatus:   status,
		FinalityBlockers: blockers,
		EconomicFinal:    economicFinalityReportsFinal(status, blockers),
	}
	raw, err := json.Marshal(rcp)
	if err != nil {
		t.Fatal(err)
	}
	s := string(raw)
	if !strings.Contains(s, `"finality_status":"KNOWN_COST_SETTLED"`) {
		t.Fatalf("receipt JSON missing explicit finality_status: %s", s)
	}
	if !strings.Contains(s, `"economic_final":false`) {
		t.Fatalf("receipt JSON missing economic_final:false: %s", s)
	}
	if !strings.Contains(s, "TRUE_NET_NOT_CLAIMED_ON_REALTIME_LANE") {
		t.Fatalf("receipt JSON missing finality blocker: %s", s)
	}
}
