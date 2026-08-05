package main

import (
	"encoding/json"
	"math"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestPhase6MetalLocalEconomicsStructuralHonesty(t *testing.T) {
	r := BuildMetalLocalPhase6Economics()
	mustf(t, ValidatePhase6Receipt(r), "receipt invalid: %v")

	// Joules: bound pin.
	if r.JoulesPerVerifiedOutcome == nil || r.JoulesPerVerifiedOutcome.Status != "known" {
		t.Fatal("joules must be known")
	}
	if math.Abs(r.JoulesPerVerifiedOutcome.Value-0.121724572) > 1e-12 {
		t.Fatalf("joules = %v", r.JoulesPerVerifiedOutcome.Value)
	}
	if !strings.Contains(r.JoulesPerVerifiedOutcome.Source, boundEnergyAuthorityPath) {
		t.Fatalf("joules source must cite energy authority: %s", r.JoulesPerVerifiedOutcome.Source)
	}

	// Full cost: structurally unavailable (no amount field to quote).
	if r.CostPerVerifiedOutcome == nil || r.CostPerVerifiedOutcome.Status != "unavailable" {
		t.Fatal("full cost must be unavailable")
	}
	rawCost, _ := json.Marshal(r.CostPerVerifiedOutcome)
	if strings.Contains(string(rawCost), `"value"`) || strings.Contains(string(rawCost), `"nanos"`) {
		t.Fatalf("unavailable cost must not encode a number field: %s", rawCost)
	}

	// Partial energy USD is defaulted conversion, not full cost.
	if r.EnergyUSDPerVerifiedOutcome == nil || r.EnergyUSDPerVerifiedOutcome.Knowledge != CategoryDefaulted {
		t.Fatalf("energy USD partial must be DEFAULTED: %+v", r.EnergyUSDPerVerifiedOutcome)
	}
	wantEnergyUSD := 0.121724572 / 3.6e6 * defaultElectricityUSDPerKWh
	if math.Abs(r.EnergyUSDPerVerifiedOutcome.Value-wantEnergyUSD) > 1e-18 {
		t.Fatalf("energy USD = %v want %v", r.EnergyUSDPerVerifiedOutcome.Value, wantEnergyUSD)
	}

	// True net: no Available, Unavailable present, not computable today.
	if r.MercTrueNet.IsAvailable() || r.MercTrueNetComputableToday {
		t.Fatal("true net must not be available/computable")
	}
	rawNet, _ := json.Marshal(r.MercTrueNet)
	if strings.Contains(string(rawNet), `"value"`) {
		t.Fatalf("true net JSON must not encode a value: %s", rawNet)
	}
	if len(r.MercTrueNet.Unavailable.BlockingCategories) == 0 {
		t.Fatal("true net must list blocking categories")
	}

	// Buyer savings refused without counterfactual.
	if r.BuyerSavings.Unavailable == nil {
		t.Fatal("buyer savings must be unavailable")
	}

	// Supplier contribution known as ledger credit only.
	if r.SupplierContribution.Available == nil {
		t.Fatal("supplier ledger contribution should be known from money path")
	}
	if r.SupplierContribution.PhysicalCostOfSupplier.Status != "unavailable" {
		t.Fatal("supplier physical cost must remain unavailable")
	}

	// Gross is not true net.
	if r.GrossPlatformRow == nil || r.GrossPlatformRow.Nanos != boundMoneyKnownCostContributionNanosTotal {
		t.Fatalf("gross row: %+v", r.GrossPlatformRow)
	}
	if r.GrossPlatformRow.Label != "gross_platform_ledger_row_not_true_net" {
		t.Fatal("gross label")
	}

	// Primary metric blocked; term status names the blockers.
	if r.PrimaryMetricQuotableToday || r.PrimaryMetric.Available != nil {
		t.Fatal("primary metric must not be quotable")
	}
	if !strings.Contains(r.PrimaryMetric.TermStatus.DollarCost, "UNAVAILABLE") {
		t.Fatalf("dollar term: %s", r.PrimaryMetric.TermStatus.DollarCost)
	}
	if !strings.Contains(r.PrimaryMetric.TermStatus.InsideSLA, "UNAVAILABLE") {
		t.Fatalf("SLA term: %s", r.PrimaryMetric.TermStatus.InsideSLA)
	}
	if !strings.Contains(r.PrimaryMetric.TermStatus.Joules, "KNOWN") {
		t.Fatalf("joules term: %s", r.PrimaryMetric.TermStatus.Joules)
	}

	// No CUDA invention.
	if !r.Scope.NoCUDAInvented {
		t.Fatal("must refuse CUDA invention")
	}
}

func TestPhase6CategoryInventoryMarksKnownAndUnknown(t *testing.T) {
	r := BuildMetalLocalPhase6Economics()
	byName := map[string]DirectiveCostCategory{}
	for _, c := range r.Categories {
		byName[c.Name] = c
	}

	// KNOWN today on Metal energy path.
	for _, name := range []string{dirCatActualDuration, dirCatEnergy} {
		c := byName[name]
		if c.Knowledge != CategoryKnown {
			t.Fatalf("%s knowledge=%s want KNOWN", name, c.Knowledge)
		}
		if c.Source == "" {
			t.Fatalf("%s KNOWN without source", name)
		}
	}
	// NOT_APPLICABLE: cloud provider rate on owned Metal.
	if byName[dirCatProviderSupplierRate].Knowledge != CategoryNotApplicable {
		t.Fatalf("provider rate: %+v", byName[dirCatProviderSupplierRate])
	}
	// UNKNOWN with would_require.
	for _, name := range []string{
		dirCatUtilization, dirCatStartupResidency, dirCatVerification,
		dirCatRetries, dirCatStorageEgress, dirCatPaymentAllocation, dirCatRefundRisk,
	} {
		c := byName[name]
		if c.Knowledge != CategoryUnknown {
			t.Fatalf("%s knowledge=%s want UNKNOWN", name, c.Knowledge)
		}
		if strings.TrimSpace(c.WouldRequire) == "" {
			t.Fatalf("%s UNKNOWN without would_require", name)
		}
	}
}

func TestPhase6TrueNetCannotBeReadAsNumber(t *testing.T) {
	r := BuildMetalLocalPhase6Economics()
	// The only legal numeric read path is IsAvailable() → false.
	if r.MercTrueNet.IsAvailable() {
		t.Fatal("IsAvailable true")
	}
	// Encoding must be an object with status unavailable, not a bare number.
	b, err := json.Marshal(r.MercTrueNet)
	must(t, err)
	var probe map[string]any
	must(t, json.Unmarshal(b, &probe))
	un, ok := probe["unavailable"].(map[string]any)
	if !ok {
		t.Fatalf("want unavailable object, got %s", b)
	}
	if un["status"] != "unavailable" {
		t.Fatalf("status=%v", un["status"])
	}
	if _, has := probe["available"]; has {
		t.Fatalf("available key must be omitted when blocked: %s", b)
	}
}

func TestPhase6GrossRowIsNotTrueNetType(t *testing.T) {
	r := BuildMetalLocalPhase6Economics()
	// Compile-time / JSON-time separation: gross has nanos; true net does not
	// when blocked. Cross-wiring labels is refused by ValidatePhase6Receipt.
	grossJSON, _ := json.Marshal(r.GrossPlatformRow)
	if !strings.Contains(string(grossJSON), "gross_platform_ledger_row_not_true_net") {
		t.Fatalf("gross JSON: %s", grossJSON)
	}
	if strings.Contains(string(grossJSON), "true_net_contribution") {
		t.Fatalf("gross must not use true_net field names: %s", grossJSON)
	}
	// True net must not silently equal gross nanos via any Available value.
	if r.MercTrueNet.Available != nil &&
		int64(r.MercTrueNet.Available.Value) == r.GrossPlatformRow.Nanos {
		t.Fatal("true net must not be aliased to gross nanos")
	}
}

func TestPhase6BoundEnergyAuthorityOnDisk(t *testing.T) {
	root := repoRootForPhase6Test(t)
	joules, outcomes, err := LoadBoundMetalEnergyAuthority(root)
	mustf(t, err, "load energy authority: %v")
	if outcomes != 512 {
		t.Fatalf("outcomes=%d", outcomes)
	}
	if math.Abs(joules-boundJoulesPerVerifiedOutcome) > 1e-12 {
		t.Fatalf("joules=%v", joules)
	}
}

func TestPhase6MoneyConservationSourceExists(t *testing.T) {
	root := repoRootForPhase6Test(t)
	path := filepath.Join(root, boundMoneyConservationPath)
	raw, err := os.ReadFile(path)
	mustf(t, err, "money conservation receipt: %v")
	var doc struct {
		BindingStatus string `json:"binding_status"`
		Cluster       struct {
			BuyerChargeNanosTotal           int64 `json:"buyer_charge_nanos_total"`
			SupplierPayableNanosTotal       int64 `json:"supplier_payable_nanos_total"`
			KnownCostContributionNanosTotal int64 `json:"known_cost_contribution_nanos_total"`
			PhysicalExecutions              int   `json:"physical_executions"`
			IndependentDeliveries           int   `json:"independent_deliveries"`
		} `json:"cluster"`
		Assertions map[string]bool `json:"assertions"`
	}
	must(t, json.Unmarshal(raw, &doc))
	if strings.ToUpper(doc.BindingStatus) != BindingBound {
		t.Fatalf("binding_status=%q", doc.BindingStatus)
	}
	// Conservation: buyer = supplier + platform (known contribution).
	if doc.Cluster.BuyerChargeNanosTotal !=
		doc.Cluster.SupplierPayableNanosTotal+doc.Cluster.KnownCostContributionNanosTotal {
		t.Fatalf("money conservation broken: buyer=%d supplier=%d known=%d",
			doc.Cluster.BuyerChargeNanosTotal,
			doc.Cluster.SupplierPayableNanosTotal,
			doc.Cluster.KnownCostContributionNanosTotal)
	}
	if doc.Cluster.BuyerChargeNanosTotal != boundMoneyBuyerChargeNanosTotal ||
		doc.Cluster.SupplierPayableNanosTotal != boundMoneySupplierPayableNanosTotal ||
		doc.Cluster.KnownCostContributionNanosTotal != boundMoneyKnownCostContributionNanosTotal {
		t.Fatalf("money pins drifted vs phase6 constants: cluster=%+v", doc.Cluster)
	}
	if doc.Cluster.PhysicalExecutions != 1 || doc.Cluster.IndependentDeliveries != 128 {
		t.Fatalf("128→1 shape drifted: %+v", doc.Cluster)
	}
	// The proof may assert positive_merc_net_contribution on the GROSS residual;
	// Phase 6 still refuses to call that true net.
	if doc.Assertions["positive_merc_net_contribution"] && rGrossIsTrueNetAlias(t) {
		t.Fatal("unreachable")
	}
}

// rGrossIsTrueNetAlias is a named false: gross contribution assertions on money
// receipts are not Phase 6 true net.
func rGrossIsTrueNetAlias(t *testing.T) bool {
	t.Helper()
	return false
}

func TestPhase6PrimaryMetricNamesBlockingTerms(t *testing.T) {
	r := BuildMetalLocalPhase6Economics()
	u := r.PrimaryMetric.Unavailable
	if u == nil {
		t.Fatal("nil unavailable")
	}
	joined := strings.Join(u.BlockingCategories, ",")
	for _, need := range []string{
		dirCatUtilization, dirCatStartupResidency, dirCatRefundRisk,
		"full_dollar_cost_per_verified_outcome", "inside_sla",
	} {
		if !strings.Contains(joined, need) {
			t.Fatalf("primary blocking missing %q in %v", need, u.BlockingCategories)
		}
	}
}

func TestPhase6ReceiptJSONRoundTrip(t *testing.T) {
	r := BuildMetalLocalPhase6Economics()
	b, err := Phase6ReceiptJSON(r)
	must(t, err)
	var again Phase6EconomicsReceipt
	must(t, json.Unmarshal(b, &again))
	mustf(t, ValidatePhase6Receipt(again), "round-trip invalid: %v")
}

// TestPhase6WriteEvidencePayload is opt-in: MERC_PHASE6_ECONOMICS_WRITE=1 writes
// the un-bound payload for scripts/write-bound-evidence.py to seal.
func TestPhase6WriteEvidencePayload(t *testing.T) {
	if os.Getenv("MERC_PHASE6_ECONOMICS_WRITE") != "1" {
		t.Skip("set MERC_PHASE6_ECONOMICS_WRITE=1 to emit evidence payload")
	}
	r := BuildMetalLocalPhase6Economics()
	b, err := Phase6ReceiptJSON(r)
	must(t, err)
	out := filepath.Join(repoRootForPhase6Test(t), "evidence/perf/phase6-directive-economics.payload.json")
	must(t, os.WriteFile(out, b, 0o644))
	t.Logf("wrote %s (%d bytes)", out, len(b))
}

func repoRootForPhase6Test(t *testing.T) string {
	t.Helper()
	// Tests run with cwd = control/.
	wd, err := os.Getwd()
	must(t, err)
	root := filepath.Clean(filepath.Join(wd, ".."))
	if _, err := os.Stat(filepath.Join(root, boundEnergyAuthorityPath)); err != nil {
		// Fallback: walk up looking for evidence/perf/ioreport-gpu-energy-authority.json.
		dir := wd
		for i := 0; i < 6; i++ {
			cand := filepath.Join(dir, boundEnergyAuthorityPath)
			if _, err := os.Stat(cand); err == nil {
				return dir
			}
			dir = filepath.Dir(dir)
		}
		t.Fatalf("cannot locate repo root from %s: %v", wd, err)
	}
	return root
}
