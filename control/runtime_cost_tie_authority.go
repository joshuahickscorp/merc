package main

import (
	"fmt"
	"math"
	"sort"
	"strings"
)

// Why two cells on one model contract cost the same, named and quantified.
//
// "The cost ties" is not an answer. It is either an arithmetic consequence of a
// specific authority, or it is an assumption nobody has checked. The overnight
// selector work established the first — supplier entitlement is
// units/1000 × price × share, the catalogue is keyed by (model, job_type), and
// unitsPerSec cancels — but a receipt that only asserts the tie leaves a reader
// unable to tell how close it was to breaking.
//
// This names the chain that forces the tie, then quantifies every term that
// could ever break it: what it is per cell, how big the difference is against
// the supplier entitlement it would have to overcome, and whether its knowledge
// state permits it to rule at all. A term that is ASSUMED cannot decide a money
// question however large it is; a term that is governed but a millionth the size
// of the entitlement does not decide one either. Both facts are worth stating,
// because "no cost winner is available here" is a much weaker claim than "no
// cost winner is available here, and here is the margin by which that holds".
//
// Nothing here adds a bias, invents a rate, or breaks a tie. It explains one.

const (
	// costTieForcedByCatalogueKey: every differentiating term is either
	// not-applicable, unmeasured, or knowledge-blocked, so the catalogue key is
	// the whole reason the cells cost the same.
	costTieForcedByCatalogueKey = "TIE_FORCED_BY_CATALOGUE_KEY_AND_CANCELLED_SETTLEMENT"
	// costTieGovernedTermDiffers: a governed term genuinely differs, so a cost
	// comparison on this contract is available and the tie claim is wrong.
	costTieGovernedTermDiffers = "COST_DIFFERS_ON_A_GOVERNED_TERM"
	// costTieNotTied: the ranking costs themselves differ, so there is no tie to
	// explain.
	costTieNotTied = "NO_TIE_TO_EXPLAIN"
	// costTieUnavailable: fewer than two measured cells.
	costTieUnavailable = "UNAVAILABLE_FEWER_THAN_TWO_MEASURED_CELLS"
	// costTieReliabilityRefused: payable liability ties, but at least one arm
	// lacks clean, sufficient reliability evidence. Failures are unpaid and
	// therefore do not create a dollar delta; they refuse the comparison.
	costTieReliabilityRefused = "RELIABILITY_EVIDENCE_REFUSES_COMPARISON"
)

// CostTieTerm is one cost term that could differ between two cells, with the
// size of the difference and whether it is allowed to rule.
type CostTieTerm struct {
	Name      string                 `json:"name"`
	Knowledge CellEconomicsKnowledge `json:"knowledge"`
	Currency  string                 `json:"currency,omitempty"`

	CellAID       string  `json:"cell_a_id"`
	CellBID       string  `json:"cell_b_id"`
	CellAUSDUnit  float64 `json:"cell_a_usd_per_unit"`
	CellBUSDUnit  float64 `json:"cell_b_usd_per_unit"`
	DeltaUSDUnit  float64 `json:"delta_usd_per_unit"`
	AbsDeltaShare float64 `json:"abs_delta_as_share_of_supplier_entitlement"`

	// MayRule is true only when the term's knowledge state permits it to decide
	// a money question. DEFAULTED and ASSUMED do not: a policy default is not a
	// measurement of this cell, and an assumption is not a measurement at all.
	MayRule bool   `json:"may_rule"`
	Why     string `json:"why"`
}

// CostTieAuthority is the full explanation for one comparison.
type CostTieAuthority struct {
	Verdict string `json:"verdict"`
	Tied    bool   `json:"tied"`
	// Currency governs every legacy `_usd` amount in this explanation.
	Currency string `json:"currency"`

	// The chain that forces it.
	CatalogueKey          string `json:"catalogue_key"`
	CatalogueKeyAuthority string `json:"catalogue_key_authority"`
	SettlementForm        string `json:"settlement_form"`
	CancellationIdentity  string `json:"cancellation_identity"`
	ScheduleSHA256        string `json:"catalogue_schedule_sha256,omitempty"`

	SupplierUSDPerUnit                  float64 `json:"supplier_entitlement_usd_per_unit"`
	RankingSupplierLiabilityUSDPerUnitA float64 `json:"ranking_supplier_liability_usd_per_verified_unit_a"`
	RankingSupplierLiabilityUSDPerUnitB float64 `json:"ranking_supplier_liability_usd_per_verified_unit_b"`

	// Every term that could break the tie, largest relative difference first.
	DifferentiatingTerms []CostTieTerm `json:"differentiating_terms"`

	// LargestGovernedShare is the biggest |delta| / supplier entitlement among
	// terms that are actually allowed to rule. Zero means nothing governed
	// differs at all.
	LargestGovernedShare float64 `json:"largest_governed_delta_share"`
	// LargestAnyShare is the same over ALL terms, including the ones that may
	// not rule. Reported so a reader can see that even taking a blocked term at
	// face value would not change the outcome — or that it would, which is a
	// reason to go and measure it properly.
	LargestAnyShare float64 `json:"largest_any_delta_share"`

	ReliabilityStatus   string   `json:"reliability_status"`
	ReliabilityRefusals []string `json:"reliability_refusals,omitempty"`
	Statement           string   `json:"statement"`
	WouldRequire        []string `json:"what_would_break_the_tie"`
}

// ExplainCostTie builds the explanation for two projected cells.
//
// a and b are the two cells being compared; catalogue is the authority both
// were priced under. The function does not decide anything — the caller has
// already ranked — it explains what the ranking could and could not see.
func ExplainCostTie(
	a, b CellEconomicsProjection, catalogue CataloguePriceAuthority,
) CostTieAuthority {
	out := CostTieAuthority{
		Currency:     a.Currency,
		CatalogueKey: "(model_id, job_type)",
		CatalogueKeyAuthority: "control/pricing_decision.go#LoadCataloguePriceAuthorityAtSchedule" +
			"(schedule_sha256, schedule_version, model_id, job_type) — the lookup takes no " +
			"runtime cell, so no price in this system can depend on which cell ran the work",
		SettlementForm: supplierEntitlementPolicyCancelled +
			": supplier_owed = units/1000 × price × share",
		CancellationIdentity: "ceiling($/hr) = unitsPerSec × 3600/1000 × price × share; " +
			"seconds = units / unitsPerSec; required = ceiling × seconds / 3600 = " +
			"units/1000 × price × share. unitsPerSec appears twice and cancels, so a cell " +
			"that is N× faster earns exactly the same payout for the same units",
		ScheduleSHA256:                      catalogue.ScheduleSHA256,
		RankingSupplierLiabilityUSDPerUnitA: a.SupplierLiabilityUSDPerVerifiedUnit,
		RankingSupplierLiabilityUSDPerUnitB: b.SupplierLiabilityUSDPerVerifiedUnit,
		SupplierUSDPerUnit:                  a.SupplierUSDPerUnit,
	}
	if a.Currency == "" || b.Currency == "" || !strings.EqualFold(a.Currency, b.Currency) {
		clearCostTieMoney(&out)
		out.Verdict = costTieUnavailable
		out.Statement = fmt.Sprintf(
			"cost tie refused: cell money currencies are %q and %q; no cross-currency arithmetic is permitted",
			a.Currency, b.Currency)
		return out
	}
	_, catalogueCurrency, _ := cellEconomicsSettlementPrice(catalogue)
	if catalogueCurrency != "" && !sameCellEconomicsCurrency(a.Currency, catalogueCurrency) {
		clearCostTieMoney(&out)
		out.Verdict = costTieUnavailable
		out.Statement = fmt.Sprintf(
			"cost tie refused: cell money currency %q does not match catalogue settlement currency %q",
			a.Currency, catalogueCurrency)
		return out
	}
	out.ReliabilityRefusals = append(
		projectionReliabilityRefusals(a),
		projectionReliabilityRefusals(b)...,
	)
	if a.CellID == "" || b.CellID == "" {
		out.Verdict = costTieUnavailable
		out.Statement = "fewer than two measured cells with a resolvable supplier-liability proxy; " +
			"there is no tie to explain and no comparison to make"
		return out
	}
	if !a.SupplierLiabilityAvailable || !b.SupplierLiabilityAvailable {
		if len(out.ReliabilityRefusals) > 0 {
			out.Tied = supplierLiabilitiesTieUSD(a.SupplierUSDPerUnit, b.SupplierUSDPerUnit)
			out.ReliabilityStatus = "REFUSED"
			out.Verdict = costTieReliabilityRefused
			out.Statement = fmt.Sprintf(
				"payable supplier liability remains observable, but the comparison is refused by reliability evidence: %v. "+
					"Rejected, retried and terminally failed attempts are unpaid; they do not manufacture a dollar cost delta",
				out.ReliabilityRefusals)
			return out
		}
		out.Verdict = costTieUnavailable
		out.Statement = "fewer than two cells have strict measured supplier-liability authority; " +
			"runtime/build or exact-geometry refusal leaves no comparison to make"
		return out
	}

	out.Tied = SupplierLiabilityProxiesTie(a, b)
	out.DifferentiatingTerms = costTieTerms(a, b)
	if len(out.ReliabilityRefusals) == 0 {
		out.ReliabilityStatus = "CLEAN_SUFFICIENT"
	} else {
		out.ReliabilityStatus = "REFUSED"
	}
	for _, t := range out.DifferentiatingTerms {
		if t.AbsDeltaShare > out.LargestAnyShare {
			out.LargestAnyShare = t.AbsDeltaShare
		}
		if t.MayRule && t.AbsDeltaShare > out.LargestGovernedShare {
			out.LargestGovernedShare = t.AbsDeltaShare
		}
	}

	switch {
	case !out.Tied:
		out.Verdict = costTieNotTied
		out.Statement = fmt.Sprintf(
			"the two cells do not tie on measured supplier liability: %s is $%.9f/verified unit and %s is $%.9f/verified unit; this is not a total-cost verdict",
			a.CellID, a.SupplierLiabilityUSDPerVerifiedUnit,
			b.CellID, b.SupplierLiabilityUSDPerVerifiedUnit)
	case len(out.ReliabilityRefusals) > 0:
		out.Verdict = costTieReliabilityRefused
		out.Statement = fmt.Sprintf(
			"payable supplier liability ties, but the comparison is refused by reliability evidence: %v. "+
				"Rejected, retried and terminally failed attempts are unpaid; they do not manufacture a dollar cost delta",
			out.ReliabilityRefusals)
	case out.LargestGovernedShare > 0:
		out.Verdict = costTieGovernedTermDiffers
		out.Statement = fmt.Sprintf(
			"the ranking cost ties, but a governed term differs by %.3g of the supplier "+
				"entitlement; the ranking basis does not see it and that is a gap, not a tie",
			out.LargestGovernedShare)
	default:
		out.Verdict = costTieForcedByCatalogueKey
		out.Statement = fmt.Sprintf(
			"supplier entitlement is $%.7f/unit for both cells because the catalogue is keyed "+
				"by (model_id, job_type) and duration cancels from the settlement form. Every "+
				"term that could differ is either not applicable, unmeasured, or knowledge-blocked. "+
				"Taking even the blocked terms at face value, the largest difference available is "+
				"%.3g of the supplier entitlement — so no cost winner exists on this contract, and "+
				"it is not close",
			out.SupplierUSDPerUnit, out.LargestAnyShare)
	}

	out.WouldRequire = []string{
		"rekeying the catalogue price schedule by runtime cell, which is a pricing decision and not a code change",
		"a cloud-backed cell on one side, so a governed provider pod rate × duration becomes a real per-cell cost",
		"a MEASURED sustained-watts receipt for the hardware class, so the energy term stops being ASSUMED and may rule",
		"clean sufficient reliability evidence on both cells; retries and failures refuse comparison but do not multiply supplier liability",
	}
	return out
}

func clearCostTieMoney(out *CostTieAuthority) {
	out.Currency = ""
	out.SupplierUSDPerUnit = 0
	out.RankingSupplierLiabilityUSDPerUnitA = 0
	out.RankingSupplierLiabilityUSDPerUnitB = 0
}

func projectionReliabilityRefusals(p CellEconomicsProjection) []string {
	var out []string
	add := func(reason string) {
		out = append(out, fmt.Sprintf("%s: %s", p.CellID, reason))
	}
	if p.Samples < minSupplierLiabilitySamples {
		add(fmt.Sprintf("completed samples %d < %d", p.Samples, minSupplierLiabilitySamples))
	}
	if math.IsNaN(p.RetryRate) || math.IsInf(p.RetryRate, 0) || p.RetryRate < 0 {
		add("retry rate is invalid")
	} else if p.RetryRate != 0 {
		add(fmt.Sprintf("retry rate %.6g is nonzero", p.RetryRate))
	}
	if p.VerificationSamples < minSupplierLiabilitySamples {
		add(fmt.Sprintf("verification samples %d < %d",
			p.VerificationSamples, minSupplierLiabilitySamples))
	}
	if p.VerificationFails != 0 {
		add(fmt.Sprintf("verification failures %d are nonzero", p.VerificationFails))
	}
	if p.TerminalAttempts < minSupplierLiabilitySamples {
		add(fmt.Sprintf("terminal attempts %d < %d",
			p.TerminalAttempts, minSupplierLiabilitySamples))
	}
	if p.TerminalFails != 0 {
		add(fmt.Sprintf("terminal failures %d are nonzero", p.TerminalFails))
	}
	return out
}

// costTieTerms builds the per-term comparison, largest relative gap first.
func costTieTerms(a, b CellEconomicsProjection) []CostTieTerm {
	supplier := a.SupplierUSDPerUnit
	if supplier <= 0 {
		supplier = b.SupplierUSDPerUnit
	}

	share := func(delta float64) float64 {
		if supplier <= 0 {
			return 0
		}
		return math.Abs(delta) / supplier
	}

	mk := func(name string, ta, tb CellEconomicsTerm) CostTieTerm {
		// The pair's knowledge is the weaker of the two: a term known on one cell
		// and assumed on the other cannot rule on the difference between them.
		k := weakerKnowledge(ta.Knowledge, tb.Knowledge)
		if costTieTermNeedsCurrency(ta) || costTieTermNeedsCurrency(tb) {
			if !sameCellEconomicsCurrency(ta.Currency, a.Currency) ||
				!sameCellEconomicsCurrency(tb.Currency, b.Currency) ||
				!sameCellEconomicsCurrency(ta.Currency, tb.Currency) {
				return CostTieTerm{
					Name: name, Knowledge: CategoryUnknown,
					CellAID: a.CellID, CellBID: b.CellID,
					Why: fmt.Sprintf(
						"money comparison refused: term currencies %q and %q do not both match projection currency %q",
						ta.Currency, tb.Currency, a.Currency),
				}
			}
		}
		delta := ta.MoneyUSD - tb.MoneyUSD
		t := CostTieTerm{
			Name: name, Knowledge: k, Currency: a.Currency,
			CellAID: a.CellID, CellBID: b.CellID,
			CellAUSDUnit: ta.MoneyUSD, CellBUSDUnit: tb.MoneyUSD,
			DeltaUSDUnit: delta, AbsDeltaShare: share(delta),
			MayRule: k == CategoryKnown,
		}
		switch k {
		case CategoryKnown:
			t.Why = "governed on both cells; this term may decide a cost comparison"
		case CategoryDefaulted:
			t.Why = "a published policy rate with provenance, not a meter on this cell; " +
				"it may be reported but must not rule on money"
		case CategoryAssumed:
			t.Why = "an estimate with no measurement path on this hardware; it must never " +
				"present as measured and must never rule"
		case CategoryNotApplicable:
			t.Why = "the category does not apply to either placement, so it cannot differ"
		default:
			t.Why = "no attributable value exists on at least one cell, so the difference is unknown rather than zero"
		}
		return t
	}

	terms := []CostTieTerm{
		mk("provider_cost", a.ProviderCost, b.ProviderCost),
		mk("energy_usd_per_unit_partial", a.EnergyPartial, b.EnergyPartial),
		mk("storage_and_transfer", a.StorageTransfer, b.StorageTransfer),
		mk("utilization", a.Utilization, b.Utilization),
	}

	sort.SliceStable(terms, func(i, j int) bool {
		return terms[i].AbsDeltaShare > terms[j].AbsDeltaShare
	})
	return terms
}

func costTieTermNeedsCurrency(t CellEconomicsTerm) bool {
	if t.MoneyUSD != 0 {
		return true
	}
	switch t.Knowledge {
	case CategoryKnown, CategoryDefaulted, CategoryAssumed:
		return true
	default:
		return false
	}
}

// weakerKnowledge returns the less authoritative of two knowledge states.
// A comparison is only as governed as its weaker side.
func weakerKnowledge(a, b CellEconomicsKnowledge) CellEconomicsKnowledge {
	rank := func(k CellEconomicsKnowledge) int {
		switch k {
		case CategoryKnown:
			return 4
		case CategoryDefaulted:
			return 3
		case CategoryAssumed:
			return 2
		case CategoryNotApplicable:
			return 1
		default: // UNKNOWN and anything unrecognised
			return 0
		}
	}
	if rank(a) <= rank(b) {
		return a
	}
	return b
}
