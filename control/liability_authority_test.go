package main

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
)

// productionLiabilityWriters are the named liability transitions that must cite
// an existing PricingDecision digest or lane settlement id. A new writer that
// invents work liability without using applyLiabilityAuthority / loadJobLiability
// / liabilityAuthority will fail this scan.
var productionLiabilityWriters = []string{
	"taskPayoutEntriesAt",
	"insertJobSLAPremiumChargeTx",
	"SettleJobSLA",
	"insertJobDisputeClawbacksTx",
	"insertJobDisputeBuyerRefundsTx",
	"accrueSupplierLiability",
	"FinalizePayout",
	"FinalizeRealtimeSuccess",
	"SettleRealtimeExactReuse",
	"RefundRealtimeContract",
	"settleFinalServiceLeaseTx",
	"clawbackTaskCreditTx",
}

func TestLiabilityLifecycleRevisionIdentity(t *testing.T) {
	if liabilityLifecycleRevision != "refund-dispute-payout-lifecycle-v1" {
		t.Fatalf("liability lifecycle revision drifted: %q", liabilityLifecycleRevision)
	}
	// Replay recovery: the constant is the durable identity cited by writers.
	// Risk-reserve keeps its own policy revision; this one is for refund /
	// dispute / payout rules only.
	if riskReservePolicyRevision == liabilityLifecycleRevision {
		t.Fatal("liability lifecycle revision must not collide with risk reserve policy revision")
	}
	auth := liabilityAuthority{
		PricingDecisionSHA256: strings.Repeat("ab", 32),
		LifecycleRevision:     liabilityLifecycleRevision,
	}
	if err := auth.validate(); err != nil {
		t.Fatalf("valid authority rejected: %v", err)
	}
	bad := liabilityAuthority{
		PricingDecisionSHA256: strings.Repeat("ab", 32),
		LifecycleRevision:     "unknown-rules-v99",
	}
	if err := bad.validate(); err == nil {
		t.Fatal("unknown lifecycle revision must be refused")
	}
}

func TestEconomicFinalityRefusesBlockers(t *testing.T) {
	status, blockers := realtimeKnownCostFinality()
	if status != laneFinalityKnownCostSettled {
		t.Fatalf("realtime finality status = %q", status)
	}
	if economicFinalityReportsFinal(status, blockers) {
		t.Fatal("realtime known-cost settle must not report economic FINAL")
	}
	if economicFinalityReportsFinal(laneFinalityEconomicFinal, []string{"X"}) {
		t.Fatal("ECONOMIC_FINAL with blockers must not report final")
	}
	if !economicFinalityReportsFinal(laneFinalityEconomicFinal, nil) {
		t.Fatal("ECONOMIC_FINAL with empty blockers is final")
	}
	leaseStatus, leaseFinal := serviceLeaseMoneyTerminalFinality(serviceLeaseEconomicFinalityBlockers())
	if leaseStatus != laneFinalityMoneyTerminalNotEconomicFinal || leaseFinal {
		t.Fatalf("lease money terminal with blockers: status=%q final=%v", leaseStatus, leaseFinal)
	}
}

func TestLiabilityAuthorityValidateRequiresDigest(t *testing.T) {
	if err := (liabilityAuthority{}).validate(); err == nil {
		t.Fatal("empty authority must fail")
	}
	if err := (liabilityAuthority{LaneSettlementID: "lane-1"}).validate(); err != nil {
		t.Fatalf("lane settlement id alone is enough: %v", err)
	}
	if err := (liabilityAuthority{PricingDecisionSHA256: "not-hex"}).validate(); err == nil {
		t.Fatal("non-hex pricing sha must fail")
	}
}

func TestApplyLiabilityAuthorityDoesNotTouchAmounts(t *testing.T) {
	auth := liabilityAuthority{
		PricingDecisionSHA256: strings.Repeat("cd", 32),
		LifecycleRevision:     liabilityLifecycleRevision,
	}
	entry := ledgerInsert{Kind: KindBuyerCharge, AmountMicros: -1_234_567}
	if err := applyLiabilityAuthority(&entry, auth); err != nil {
		t.Fatal(err)
	}
	if entry.AmountMicros != -1_234_567 {
		t.Fatalf("amount changed: %d", entry.AmountMicros)
	}
	if entry.PricingDecisionSHA256 != auth.PricingDecisionSHA256 {
		t.Fatalf("sha not applied: %q", entry.PricingDecisionSHA256)
	}
	if entry.LifecycleRevision != liabilityLifecycleRevision {
		t.Fatalf("lifecycle not applied: %q", entry.LifecycleRevision)
	}
}

// TestNamedLiabilityWritersCiteAuthority fails if a production liability writer
// no longer references the citation helpers. This is the Step 13 guard against
// a new writer landing without an authority digest.
func TestNamedLiabilityWritersCiteAuthority(t *testing.T) {
	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatal(err)
	}
	bodies := map[string]string{}
	for _, entry := range entries {
		name := entry.Name()
		if entry.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		raw, err := os.ReadFile(name)
		if err != nil {
			t.Fatal(err)
		}
		bodies[name] = string(raw)
	}
	// Helpers that prove citation is attached.
	const markers = "applyLiabilityAuthority|loadJobLiabilityAuthority|loadJobPricingDecisionSHA|loadLedgerEntryLiabilityAuthority|liabilityAuthority|liabilityLifecycleRevision|realtimeKnownCostFinality|pricing_decision_sha256"
	markerList := strings.Split(markers, "|")
	for _, writer := range productionLiabilityWriters {
		found := false
		for file, body := range bodies {
			if !strings.Contains(body, "func "+writer) && !strings.Contains(body, "func (") {
				// cheap filter below
			}
			idx := indexFuncDecl(body, writer)
			if idx < 0 {
				continue
			}
			// Take a window after the function declaration.
			window := body[idx:]
			if end := strings.Index(window[1:], "\nfunc "); end > 0 {
				window = window[:end+1]
			}
			if len(window) > 12000 {
				window = window[:12000]
			}
			for _, m := range markerList {
				if strings.Contains(window, m) {
					found = true
					break
				}
			}
			if found {
				t.Logf("%s cites authority via helpers in %s", writer, file)
				break
			}
			// FinalizePayout validates stored citation rather than re-applying.
			if writer == "FinalizePayout" && strings.Contains(window, "settlement.pricing_decision_sha256") {
				found = true
				t.Logf("%s validates stored settlement authority in %s", writer, file)
				break
			}
		}
		if !found {
			t.Errorf("liability writer %s does not cite pricing/lane authority helpers", writer)
		}
	}
}

func indexFuncDecl(body, name string) int {
	for _, prefix := range []string{
		"func " + name + "(",
		"func (s *Store) " + name + "(",
		"func (p *VerificationProcessor) " + name + "(",
	} {
		if i := strings.Index(body, prefix); i >= 0 {
			return i
		}
	}
	return -1
}

// TestWorkLiabilityLedgerInsertsInProductionSourceUseCitationHelpers scans
// production composite literals for work-liability kinds and requires that the
// surrounding function body also mentions a citation helper. Pure validation
// tests and prepaid/risk-reserve paths are excluded by kind filter.
func TestWorkLiabilityLedgerInsertsInProductionSourceUseCitationHelpers(t *testing.T) {
	fset := token.NewFileSet()
	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatal(err)
	}
	var offenders []string
	for _, entry := range entries {
		name := entry.Name()
		if entry.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		// prepaid funding and risk reserve are out of the work-liability set.
		if name == "store_prepaid.go" || name == "risk_reserve_ledger.go" {
			continue
		}
		file, err := parser.ParseFile(fset, name, nil, 0)
		if err != nil {
			t.Fatalf("parse %s: %v", name, err)
		}
		raw, _ := os.ReadFile(name)
		body := string(raw)
		ast.Inspect(file, func(n ast.Node) bool {
			cl, ok := n.(*ast.CompositeLit)
			if !ok {
				return true
			}
			// ledgerInsert{ Kind: KindBuyerCharge, ... } or Kind: "buyer_charge"
			ident, ok := cl.Type.(*ast.Ident)
			if !ok || ident.Name != "ledgerInsert" {
				return true
			}
			kind := compositeLitKind(cl)
			if !isWorkLiabilityKind(kind) && !isWorkLiabilityKindConst(kind) {
				return true
			}
			// Find enclosing function window in source.
			pos := fset.Position(cl.Pos())
			// Search backward for func keyword line.
			lines := strings.Split(body, "\n")
			start := pos.Line - 1
			for start > 0 && !strings.HasPrefix(strings.TrimSpace(lines[start]), "func ") {
				start--
			}
			end := pos.Line - 1
			for end < len(lines)-1 {
				end++
				if end > pos.Line && strings.HasPrefix(lines[end], "func ") {
					break
				}
			}
			window := strings.Join(lines[start:min(end+1, len(lines))], "\n")
			if !strings.Contains(window, "applyLiabilityAuthority") &&
				!strings.Contains(window, "PricingDecisionSHA256:") &&
				!strings.Contains(window, "loadJobLiabilityAuthority") &&
				!strings.Contains(window, "liabilityAuth") &&
				!strings.Contains(window, "refundAuth") &&
				!strings.Contains(window, "reuseAuth") {
				offenders = append(offenders, filepath.Base(name)+":"+lineNum(pos.Line)+":"+kind)
			}
			return true
		})
	}
	if len(offenders) > 0 {
		t.Fatalf("work-liability ledgerInsert without citation in production: %v", offenders)
	}
}

func compositeLitKind(cl *ast.CompositeLit) string {
	for _, elt := range cl.Elts {
		kv, ok := elt.(*ast.KeyValueExpr)
		if !ok {
			continue
		}
		key, ok := kv.Key.(*ast.Ident)
		if !ok || key.Name != "Kind" {
			continue
		}
		switch v := kv.Value.(type) {
		case *ast.Ident:
			return v.Name
		case *ast.BasicLit:
			return strings.Trim(v.Value, `"`)
		case *ast.SelectorExpr:
			return v.Sel.Name
		}
	}
	return ""
}

func isWorkLiabilityKindConst(name string) bool {
	switch name {
	case "KindBuyerCharge", "KindSupplierCredit", "KindPlatformTake",
		"KindClawback", "KindBuyerRefund", "KindPlatformRefund", "KindSLARefund",
		"buyer_charge", "supplier_credit", "platform_take",
		"clawback", "buyer_refund", "platform_refund", "sla_refund":
		return true
	default:
		return false
	}
}

func lineNum(n int) string {
	if n == 0 {
		return "0"
	}
	var b [12]byte
	i := len(b)
	for n > 0 {
		i--
		b[i] = byte('0' + n%10)
		n /= 10
	}
	return string(b[i:])
}

// TestLiabilityCitationDoesNotChangeSplitAmounts proves the citation attach
// path leaves frozen charge amounts byte-identical (no number moved).
func TestLiabilityCitationDoesNotChangeSplitAmounts(t *testing.T) {
	buyer := mustParseUUID(t, "11111111-1111-1111-1111-111111111111")
	supplier := mustParseUUID(t, "22222222-2222-2222-2222-222222222222")
	task := mustParseUUID(t, "33333333-3333-3333-3333-333333333333")
	before, err := splitFrozenChargeNanos(buyer, supplier, task, "usd",
		1_000_000_000, 600_000_000, 0, mustParseTime(t, "2026-01-01T00:00:00Z"))
	if err != nil {
		t.Fatal(err)
	}
	auth := liabilityAuthority{PricingDecisionSHA256: strings.Repeat("ef", 32)}
	after := make([]LedgerEntry, len(before))
	copy(after, before)
	for i := range after {
		if err := applyLiabilityAuthorityToEntry(&after[i], auth); err != nil {
			t.Fatal(err)
		}
	}
	if len(before) != len(after) {
		t.Fatal("entry count changed")
	}
	for i := range before {
		if before[i].AmountUSD != after[i].AmountUSD ||
			before[i].Kind != after[i].Kind ||
			before[i].Currency != after[i].Currency ||
			before[i].PayoutStatus != after[i].PayoutStatus {
			t.Fatalf("entry %d money fields changed: before=%+v after=%+v", i, before[i], after[i])
		}
		if after[i].PricingDecisionSHA256 != auth.PricingDecisionSHA256 {
			t.Fatalf("entry %d missing citation", i)
		}
	}
	// Platform residual conservation unchanged.
	sum := after[0].AmountUSD + after[1].AmountUSD + after[2].AmountUSD
	if sum != 0 {
		t.Fatalf("conservation broken after citation: sum=%v", sum)
	}
}

func mustParseUUID(t *testing.T, s string) uuid.UUID {
	t.Helper()
	id, err := uuid.Parse(s)
	if err != nil {
		t.Fatal(err)
	}
	return id
}

func mustParseTime(t *testing.T, s string) time.Time {
	t.Helper()
	ts, err := time.Parse(time.RFC3339, s)
	if err != nil {
		t.Fatal(err)
	}
	return ts
}
