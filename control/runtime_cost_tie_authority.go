package main

import (
	"fmt"
	"math"
	"sort"
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
)

// CostTieTerm is one cost term that could differ between two cells, with the
// size of the difference and whether it is allowed to rule.
type CostTieTerm struct {
	Name      string                 `json:"name"`
	Knowledge CellEconomicsKnowledge `json:"knowledge"`

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

	// The chain that forces it.
	CatalogueKey          string `json:"catalogue_key"`
	CatalogueKeyAuthority string `json:"catalogue_key_authority"`
	SettlementForm        string `json:"settlement_form"`
	CancellationIdentity  string `json:"cancellation_identity"`
	ScheduleSHA256        string `json:"catalogue_schedule_sha256,omitempty"`

	SupplierUSDPerUnit float64 `json:"supplier_entitlement_usd_per_unit"`
	RankingUSDPerUnitA float64 `json:"ranking_verified_outcome_usd_per_unit_a"`
	RankingUSDPerUnitB float64 `json:"ranking_verified_outcome_usd_per_unit_b"`

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

	Statement    string   `json:"statement"`
	WouldRequire []string `json:"what_would_break_the_tie"`
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
		ScheduleSHA256:     catalogue.ScheduleSHA256,
		RankingUSDPerUnitA: a.VerifiedOutcomeUSDPerUnit,
		RankingUSDPerUnitB: b.VerifiedOutcomeUSDPerUnit,
		SupplierUSDPerUnit: a.SupplierUSDPerUnit,
	}
	if a.CellID == "" || b.CellID == "" || !a.VerifiedOutcomeOK || !b.VerifiedOutcomeOK {
		out.Verdict = costTieUnavailable
		out.Statement = "fewer than two measured cells with a resolvable verified-outcome cost; " +
			"there is no tie to explain and no comparison to make"
		return out
	}

	out.Tied = VerifiedOutcomeCostsTie(a, b)
	out.DifferentiatingTerms = costTieTerms(a, b)
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
			"the two cells do not tie on verified-outcome cost: %s is $%.9f/unit and %s is $%.9f/unit",
			a.CellID, a.VerifiedOutcomeUSDPerUnit, b.CellID, b.VerifiedOutcomeUSDPerUnit)
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
		"a measured per-cell reliability difference, since retries multiply the supplier entitlement and that term is NOT cancelled",
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
		delta := ta.MoneyUSD - tb.MoneyUSD
		t := CostTieTerm{
			Name: name, Knowledge: k,
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

	// Reliability is not a projection term but it is the one multiplier that is
	// NOT cancelled by the settlement form, so it belongs in any honest account
	// of what could break the tie.
	relDelta := a.SupplierUSDPerUnit*a.ReliabilityMultiplier - b.SupplierUSDPerUnit*b.ReliabilityMultiplier
	relKnowledge := CategoryKnown
	if a.VerificationSamples == 0 || b.VerificationSamples == 0 {
		relKnowledge = CategoryUnknown
	}
	rel := CostTieTerm{
		Name: "reliability_multiplier_effect", Knowledge: relKnowledge,
		CellAID: a.CellID, CellBID: b.CellID,
		CellAUSDUnit: a.SupplierUSDPerUnit * a.ReliabilityMultiplier,
		CellBUSDUnit: b.SupplierUSDPerUnit * b.ReliabilityMultiplier,
		DeltaUSDUnit: relDelta, AbsDeltaShare: share(relDelta),
		MayRule: relKnowledge == CategoryKnown,
		Why: "retries and verification failures multiply the supplier entitlement and are " +
			"NOT cancelled by the settlement form; this is the term most likely to break a " +
			"tie on real fleet data",
	}
	terms = append(terms, rel)

	sort.SliceStable(terms, func(i, j int) bool {
		return terms[i].AbsDeltaShare > terms[j].AbsDeltaShare
	})
	return terms
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
