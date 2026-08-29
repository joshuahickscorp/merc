package main

import (
	"math"
	"testing"
)

// Two cells on one model contract, equally reliable, differing only in speed.
func tieFixture(t *testing.T) (CellEconomicsProjection, CellEconomicsProjection, CataloguePriceAuthority) {
	t.Helper()
	cat := catalogueMinilm()
	slow := ProjectCellEconomics(MeasuredSupplierLiabilityProxy{
		CellID: candleEmbedCell, RuntimeID: "candle_metal", Engine: "candle",
		HWClass: "apple_silicon_ultra", JobType: "embed", ModelRef: "all-minilm-l6-v2",
		Samples: 40, Units: 320, MedianMsPerUnit: 3.026, Measured: true,
		SupplierUSDPerUnit: miniLMSupplierUSDPerUnit, Currency: catalogueReferenceCurrency,
		SourceBinding:       BindingBound,
		VerificationSamples: 40, TerminalAttempts: 40,
	}, cat, "batch")
	fast := ProjectCellEconomics(MeasuredSupplierLiabilityProxy{
		CellID: llamaEmbedCell, RuntimeID: "llama_cpp_metal", Engine: "llama_cpp",
		HWClass: "apple_silicon_ultra", JobType: "embed", ModelRef: "all-minilm-l6-v2",
		Samples: 40, Units: 320, MedianMsPerUnit: 1.864, Measured: true,
		SupplierUSDPerUnit: miniLMSupplierUSDPerUnit, Currency: catalogueReferenceCurrency,
		SourceBinding:       BindingBound,
		VerificationSamples: 40, TerminalAttempts: 40,
	}, cat, "batch")
	return slow, fast, cat
}

// The tie is explained, not asserted: the catalogue key and the cancellation
// are named, and the margin by which the tie holds is a number.
func TestCostTieNamesTheAuthorityAndTheMargin(t *testing.T) {
	slow, fast, cat := tieFixture(t)
	got := ExplainCostTie(slow, fast, cat)

	if !got.Tied {
		t.Fatalf("equal-reliability cells on one catalogue entry did not tie: %+v", got)
	}
	if got.Verdict != costTieForcedByCatalogueKey {
		t.Fatalf("verdict = %q, want %q", got.Verdict, costTieForcedByCatalogueKey)
	}
	if got.CatalogueKey != "(model_id, job_type)" {
		t.Fatalf("catalogue key not named: %q", got.CatalogueKey)
	}
	if got.SupplierUSDPerUnit <= 0 {
		t.Fatal("tie explanation quotes no supplier entitlement to compare against")
	}
	// Nothing governed may differ, or the tie is not what the receipt claims.
	if got.LargestGovernedShare != 0 {
		t.Fatalf("a governed term differs by %g of entitlement; the tie is not forced by the key",
			got.LargestGovernedShare)
	}
	// And the margin is real and tiny, not merely absent.
	if got.LargestAnyShare <= 0 {
		t.Fatal("no differentiating term was quantified at all; the margin is unstated")
	}
	// 270 W VENDOR_WALL_UPPER_BOUND enlarges the blocked energy term vs the
	// retired 65 W understatement (~4x). It must stay a small blocked margin,
	// not a term large enough to treat as settled money.
	if got.LargestAnyShare > 5e-3 {
		t.Fatalf("a blocked term is %g of entitlement — large enough that measuring it "+
			"proplerly could change the answer, which the receipt must not treat as settled",
			got.LargestAnyShare)
	}
	if len(got.WouldRequire) == 0 {
		t.Fatal("the explanation does not say what would break the tie")
	}
}

// Reliability does not multiply payable supplier liability. A failure preserves
// the dollar tie but refuses the comparison as reliability evidence; it must
// not be relabeled as a governed monetary difference.
func TestCostTieDoesNotHideGovernedReliabilityFailure(t *testing.T) {
	slow, fast, cat := tieFixture(t)

	// Same everything, except the fast cell fails verification on 4 of 40. Those
	// rejected results are unpaid and do not change accepted supplier liability;
	// the separate governed reliability term must still expose the difference.
	unreliable := ProjectCellEconomics(MeasuredSupplierLiabilityProxy{
		CellID: llamaEmbedCell, RuntimeID: "llama_cpp_metal", Engine: "llama_cpp",
		HWClass: "apple_silicon_ultra", JobType: "embed", ModelRef: "all-minilm-l6-v2",
		Samples: 40, Units: 320, MedianMsPerUnit: 1.864, Measured: true,
		SupplierUSDPerUnit: miniLMSupplierUSDPerUnit, Currency: catalogueReferenceCurrency,
		SourceBinding:       BindingBound,
		VerificationSamples: 40, VerificationFails: 4,
		TerminalAttempts: 40,
	}, cat, "batch")

	got := ExplainCostTie(slow, unreliable, cat)
	if got.Verdict != costTieReliabilityRefused {
		t.Fatalf("a 10%% verification failure rate left the verdict at %q; want %q",
			got.Verdict, costTieReliabilityRefused)
	}
	if !got.Tied {
		t.Fatal("verification failures changed the exact accepted supplier-liability tie")
	}
	if got.ReliabilityStatus != "REFUSED" || len(got.ReliabilityRefusals) == 0 {
		t.Fatalf("reliability failure was not preserved as a refusal: %+v", got)
	}
	if got.LargestGovernedShare != 0 {
		t.Fatalf("unpaid failures manufactured a governed dollar delta: %+v", got)
	}
	_ = fast
}

// The ranking basis is not the whole cost, and when a governed term differs
// underneath a ranking tie the explanation must say so rather than report a
// forced tie.
//
// This is the shape the Metal-versus-CUDA placement question will arrive in: two
// cells can tie on accepted supplier entitlement — because the catalogue is
// keyed by model and duration cancels — while one of them costs Merc a real
// governed pod rate per second and the other costs nothing. Calling that a tie
// would hide the only cost difference that is actually allowed to rule.
//
// Built by hand rather than from a live cloud cell on purpose: the one
// cloud-backed embed cell in the authority resolves its rate from a withdrawn
// receipt, so its provider term is ASSUMED and cannot rule. That is the correct
// state for that cell and the wrong fixture for this branch.
func TestCostTieDoesNotSwallowAGovernedProviderDifference(t *testing.T) {
	slow, fast, cat := tieFixture(t)
	// Same ranking cost on both sides — only the platform-side provider term
	// differs, and it is governed on both.
	slow.ProviderCost = CellEconomicsTerm{
		Name: "provider_cost", Knowledge: CategoryKnown, Currency: slow.Currency, MoneyUSD: 0,
		Basis: "owned supply", Source: "test",
	}
	fast.ProviderCost = CellEconomicsTerm{
		Name: "provider_cost", Knowledge: CategoryKnown, Currency: fast.Currency, MoneyUSD: 0.0004,
		Basis: "governed pod rate × duration", Source: "test",
	}

	got := ExplainCostTie(slow, fast, cat)
	if !got.Tied {
		t.Fatal("fixture no longer ties on the ranking basis; the branch under test is unreachable")
	}
	if got.Verdict != costTieGovernedTermDiffers {
		t.Fatalf("verdict = %q, want %q: a governed provider difference is not a tie",
			got.Verdict, costTieGovernedTermDiffers)
	}
	if got.LargestGovernedShare <= 0 {
		t.Fatal("a governed term differs but the governed margin is reported as zero")
	}
}

// A real difference in the ranking supplier-liability authority is not a tie
// merely because every secondary platform-cost term happens to match. This is
// the first branch in ExplainCostTie: the later governed-term explanation must
// never mask or overwrite it.
func TestCostTieBreaksWhenAGovernedTermDiffers(t *testing.T) {
	slow, fast, cat := tieFixture(t)
	fast.SupplierLiabilityUSDPerVerifiedUnit = slow.SupplierLiabilityUSDPerVerifiedUnit * 1.2

	got := ExplainCostTie(slow, fast, cat)
	if got.Tied {
		t.Fatalf("different measured supplier liabilities were reported as tied: %+v", got)
	}
	if got.Verdict != costTieNotTied {
		t.Fatalf("verdict = %q, want %q for different measured supplier liabilities",
			got.Verdict, costTieNotTied)
	}
	if got.RankingSupplierLiabilityUSDPerUnitA == got.RankingSupplierLiabilityUSDPerUnitB {
		t.Fatalf("receipt erased the governed ranking difference: %+v", got)
	}
}

func TestCostTieRefusesProxyAndTermCurrencyMismatch(t *testing.T) {
	slow, fast, cat := tieFixture(t)

	crossCurrency := fast
	crossCurrency.Currency = "cad"
	if SupplierLiabilityProxiesTie(slow, crossCurrency) {
		t.Fatal("supplier-liability proxy tie compared USD and CAD")
	}
	got := ExplainCostTie(slow, crossCurrency, cat)
	if got.Verdict != costTieUnavailable || got.Tied {
		t.Fatalf("cross-currency projections produced a tie: %+v", got)
	}
	if got.Currency != "" || got.SupplierUSDPerUnit != 0 ||
		got.RankingSupplierLiabilityUSDPerUnitA != 0 ||
		got.RankingSupplierLiabilityUSDPerUnitB != 0 {
		t.Fatalf("cross-currency values survived under one legacy `_usd` label: %+v", got)
	}

	// Top-level liability currency matches, but the provider operands do not.
	// Their raw numbers must not be copied into a USD-labelled delta or allowed to
	// govern the comparison.
	slow.ProviderCost = CellEconomicsTerm{
		Name: "provider_cost", Knowledge: CategoryKnown,
		Currency: "usd", MoneyUSD: 0.0001,
	}
	fast.ProviderCost = CellEconomicsTerm{
		Name: "provider_cost", Knowledge: CategoryKnown,
		Currency: "cad", MoneyUSD: 0.0002,
	}
	got = ExplainCostTie(slow, fast, cat)
	var provider *CostTieTerm
	for i := range got.DifferentiatingTerms {
		if got.DifferentiatingTerms[i].Name == "provider_cost" {
			provider = &got.DifferentiatingTerms[i]
			break
		}
	}
	if provider == nil || provider.Knowledge != CategoryUnknown || provider.MayRule ||
		provider.Currency != "" || provider.CellAUSDUnit != 0 ||
		provider.CellBUSDUnit != 0 || provider.DeltaUSDUnit != 0 {
		t.Fatalf("mismatched provider currencies were relabelled or allowed to rule: %+v", provider)
	}
	if got.LargestGovernedShare != 0 {
		t.Fatalf("cross-currency provider delta became governed: %+v", got)
	}
}

// A term that may not rule is still reported, with its knowledge state and the
// reason it may not rule. Silence about a blocked term is how an assumption
// becomes a finding later.
func TestCostTieReportsBlockedTermsWithTheirReason(t *testing.T) {
	slow, fast, cat := tieFixture(t)
	got := ExplainCostTie(slow, fast, cat)

	var energy *CostTieTerm
	for i := range got.DifferentiatingTerms {
		if got.DifferentiatingTerms[i].Name == "energy_usd_per_unit_partial" {
			energy = &got.DifferentiatingTerms[i]
		}
	}
	if energy == nil {
		t.Fatal("energy is the only term that differs between these cells and it is not reported")
	}
	if energy.MayRule {
		t.Fatalf("energy is %s on this hardware class and must not be allowed to rule on money",
			energy.Knowledge)
	}
	if energy.Knowledge != CategoryAssumed {
		t.Fatalf("energy knowledge = %s; apple_silicon_ultra watts are VENDOR_WALL_UPPER_BOUND in "+
			"src/control/pricing.go and this must not silently upgrade to MEASURED energy", energy.Knowledge)
	}
	// The faster cell really does use less energy, so the delta has a sign and
	// is not merely a rounding artefact.
	if math.Abs(energy.DeltaUSDUnit) <= 0 {
		t.Fatal("a 1.62x duration difference produced no energy delta at all")
	}
	if energy.DeltaUSDUnit <= 0 {
		t.Fatalf("the slower cell should cost MORE energy per unit, got delta %g",
			energy.DeltaUSDUnit)
	}
	if energy.Why == "" {
		t.Fatal("a blocked term is reported with no reason it is blocked")
	}
	// Every term carries both cells' identities so a reader is never guessing
	// which side is which.
	for _, term := range got.DifferentiatingTerms {
		if term.CellAID == "" || term.CellBID == "" {
			t.Fatalf("term %q does not name the cells it compares", term.Name)
		}
	}
}

// A comparison is only as governed as its weaker side.
func TestWeakerKnowledgeTakesTheLessAuthoritativeSide(t *testing.T) {
	cases := []struct {
		a, b, want CellEconomicsKnowledge
	}{
		{CategoryKnown, CategoryAssumed, CategoryAssumed},
		{CategoryAssumed, CategoryKnown, CategoryAssumed},
		{CategoryKnown, CategoryDefaulted, CategoryDefaulted},
		{CategoryKnown, CategoryKnown, CategoryKnown},
		{CategoryDefaulted, CategoryUnknown, CategoryUnknown},
		{CategoryNotApplicable, CategoryUnknown, CategoryUnknown},
	}
	for _, c := range cases {
		if got := weakerKnowledge(c.a, c.b); got != c.want {
			t.Fatalf("weakerKnowledge(%s, %s) = %s, want %s", c.a, c.b, got, c.want)
		}
	}
}

// Fewer than two measured cells is a refusal, not a tie.
func TestCostTieRefusesWithoutTwoMeasuredCells(t *testing.T) {
	slow, _, cat := tieFixture(t)
	got := ExplainCostTie(slow, CellEconomicsProjection{}, cat)
	if got.Verdict != costTieUnavailable {
		t.Fatalf("one measured cell produced verdict %q, want %q", got.Verdict, costTieUnavailable)
	}
	if got.Tied {
		t.Fatal("a missing second cell was reported as a tie")
	}
}
