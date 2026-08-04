package main

import (
	"fmt"
	"math"
	"strings"
)

// Per-cell verified-outcome economics as a SELECTOR projection.
//
// This is not a PricingDecision and never freezes money. Settlement continues
// to use the cancelled closed form from exactTaskEconomics:
//
//	ceiling  = unitsPerSec × 3600/1000 × price × share
//	seconds  = units / unitsPerSec
//	required = ceiling × seconds / 3600 = units/1000 × price × share
//
// so a revalidated throughput cannot move a settled receipt. Carrying
// unitsPerSec through the settlement arithmetic is the defect class that made
// a frozen money figure depend on a dated benchmark.
//
// What this file does is bind, per runtime cell, every term the selector needs
// to see — duration, throughput, catalogue supplier rate, reliability,
// provider/energy/storage knowledge, buyer price, and the structural refusal
// of true net — without becoming a second pricing authority. The selector
// ranks on VerifiedOutcomeUSDPerUnit (catalogue supplier × reliability). Any
// duration-sensitive platform partial is reported beside that figure, never
// as a rewrite of supplier entitlement.

const cellEconomicsProjectionSchemaVersion = 1
const cellEconomicsProjectionKind = "runtime_cell_economics_projection"

// supplierEntitlementPolicyCancelled is the settlement policy name: supplier
// owed equals units/1000 × price × share. Duration and throughput cancel and
// must not re-enter a settled receipt.
const supplierEntitlementPolicyCancelled = "exact_units_price_share_duration_cancelled"

// CellEconomicsModelContract pins the catalogue identity the projection quotes.
// Digests are copied from CataloguePriceAuthority when present so a reader can
// see which schedule produced the buyer price and supplier share.
type CellEconomicsModelContract struct {
	ModelID              string  `json:"model_id"`
	JobType              string  `json:"job_type"`
	ReferencePricePer1K  float64 `json:"reference_price_per_1k"`
	SettlementPricePer1K float64 `json:"settlement_price_per_1k,omitempty"`
	SupplierShare        float64 `json:"supplier_share"`
	ScheduleSHA256       string  `json:"schedule_sha256,omitempty"`
	BoardSHA256          string  `json:"board_sha256,omitempty"`
	PriceFormula         string  `json:"price_formula,omitempty"`
	SettlementCurrency   string  `json:"settlement_currency,omitempty"`
}

// CellEconomicsKnowledge is how one projection term stands: known from
// measurement or governed rate, unknown, not applicable, defaulted policy, or
// assumed with provenance. Same vocabulary as Phase 6 directive economics so
// the two surfaces do not invent parallel states.
type CellEconomicsKnowledge = CostCategoryKnowledge

// CellEconomicsTerm is one named money or non-money term with knowledge state.
// MoneyUSD is set only when Knowledge is KNOWN or DEFAULTED and a dollar amount
// is attributable. Zero with Knowledge==KNOWN is valid for a non-money base.
type CellEconomicsTerm struct {
	Name         string                 `json:"name"`
	Knowledge    CellEconomicsKnowledge `json:"knowledge"`
	MoneyUSD     float64                `json:"money_usd,omitempty"`
	NonMoney     float64                `json:"non_money,omitempty"`
	NonMoneyUnit string                 `json:"non_money_unit,omitempty"`
	Source       string                 `json:"source,omitempty"`
	Basis        string                 `json:"basis,omitempty"`
	WouldRequire string                 `json:"would_require,omitempty"`
	// Defect is set when the governing authority itself is flagged defective
	// (e.g. provider rate derived from a withdrawn receipt). The term may still
	// carry a number for test continuity; it must not be treated as governed.
	Defect string `json:"defect,omitempty"`
}

// CellEconomicsProjection is one cell's full economics view for selection.
//
// Conservation of authority:
//   - One canonical PricingDecision freezes buyer/supplier money at acceptance.
//   - This projection is re-derivable from MeasuredCellCost + catalogue + cell
//     authority. Re-deriving it never mutates a frozen PricingDecision.
//   - VerifiedOutcomeUSDPerUnit uses the cancelled supplier form × reliability.
//     Duration enters only partial platform terms (provider, energy) and the
//     admission hourly display rate — never supplier entitlement per unit.
type CellEconomicsProjection struct {
	SchemaVersion int    `json:"schema_version"`
	Kind          string `json:"kind"`

	// Identity: model contract · runtime cell · hardware class.
	ModelContract CellEconomicsModelContract `json:"model_contract"`
	CellID        string                     `json:"cell_id"`
	RuntimeID     string                     `json:"runtime_id"`
	Engine        string                     `json:"engine"`
	HWClass       string                     `json:"hw_class"`
	Tier          string                     `json:"tier,omitempty"`

	// Measured execution geometry.
	Samples             int     `json:"samples"`
	Units               int64   `json:"units"`
	MedianMsPerUnit     float64 `json:"median_ms_per_unit"`
	MeasuredUnitsPerSec float64 `json:"measured_units_per_sec"`
	Measured            bool    `json:"measured"`

	// Catalogue buyer price and the cancelled supplier entitlement per unit.
	// SupplierUSDPerUnit is what settlement pays per delivered unit: it does
	// not depend on this cell's duration or throughput.
	BuyerPricePer1KUnits                   float64 `json:"buyer_price_per_1k_units"`
	SupplierUSDPerUnit                     float64 `json:"supplier_usd_per_unit"`
	SupplierEntitlementPolicy              string  `json:"supplier_entitlement_policy"`
	DurationCancelsFromSupplierEntitlement bool    `json:"duration_cancels_from_supplier_entitlement"`

	// Admission-side duration-sensitive supplier display rate: what a supplier
	// is expected to earn per host-hour WHILE EXECUTING on this cell's measured
	// (or catalogue) throughput. Not a promise of realized hourly earnings and
	// not the settlement entitlement.
	AdmissionExpectedSupplierUSDHr float64 `json:"admission_expected_supplier_usd_hr"`
	AdmissionUnitsPerSec           float64 `json:"admission_units_per_sec"`

	// Reliability and verification overhead.
	RetryRate             float64 `json:"retry_rate"`
	VerificationPassRate  float64 `json:"verification_pass_rate"`
	TerminalDeliveredRate float64 `json:"terminal_delivered_rate"`
	ReliabilityMultiplier float64 `json:"reliability_multiplier"`
	VerificationSamples   int     `json:"verification_samples"`
	VerificationFails     int     `json:"verification_fails"`
	TerminalAttempts      int     `json:"terminal_attempts"`
	TerminalFails         int     `json:"terminal_fails"`

	// Verified-outcome cost the selector ranks on: supplier per unit adjusted
	// for retries, verification failures and terminal failures. Duration does
	// not appear in this expression under the cancelled settlement form, so two
	// cells on one model with equal reliability tie here by construction.
	VerifiedOutcomeUSDPerUnit float64 `json:"verified_outcome_usd_per_unit"`
	VerifiedOutcomeOK         bool    `json:"verified_outcome_ok"`
	VerifiedOutcomeBasis      string  `json:"verified_outcome_basis"`

	// Duration-sensitive platform partials. These MAY differ across cells of
	// the same model. They are not supplier entitlement and must not be
	// presented as full verified-outcome cost or as true net.
	ProviderCost    CellEconomicsTerm `json:"provider_cost"`
	EnergyPartial   CellEconomicsTerm `json:"energy_usd_per_unit_partial"`
	StorageTransfer CellEconomicsTerm `json:"storage_and_transfer"`
	Utilization     CellEconomicsTerm `json:"utilization"`

	// Merc contribution. True net is structurally unavailable while any cost
	// category is unknown. A gross platform residual is a different type and
	// is never aliased as net.
	MercTrueNet      MercTrueNetContribution `json:"merc_true_net_contribution"`
	GrossPlatformRow *GrossPlatformLedgerRow `json:"gross_platform_ledger_row,omitempty"`

	// Confidence and evidence authority.
	Confidence        float64  `json:"confidence"`
	EvidenceAuthority []string `json:"evidence_authority"`
	UnknownComponents []string `json:"unknown_components"`
	// DurationCanDifferentiate names the terms that can honestly differ per
	// cell without undoing the settlement cancellation.
	DurationCanDifferentiate []string `json:"duration_can_differentiate"`
}

// ProjectCellEconomics binds one MeasuredCellCost into a full projection.
//
// catalogue may be zero-valued: when SupplierUSDPerUnit is already measured
// from frozen task payouts, the catalogue is only needed for buyer price and
// the admission hourly display. When both measured supplier and catalogue are
// absent, verified-outcome cost is refused (ok=false), never invented as zero.
func ProjectCellEconomics(
	cost MeasuredCellCost,
	catalogue CataloguePriceAuthority,
	tier string,
) CellEconomicsProjection {
	if tier == "" {
		tier = "batch"
	}
	p := CellEconomicsProjection{
		SchemaVersion: cellEconomicsProjectionSchemaVersion,
		Kind:          cellEconomicsProjectionKind,
		ModelContract: CellEconomicsModelContract{
			ModelID:              firstNonEmpty(catalogue.ModelID, cost.ModelRef),
			JobType:              firstNonEmpty(catalogue.JobType, cost.JobType),
			ReferencePricePer1K:  catalogue.ReferencePricePer1K,
			SettlementPricePer1K: catalogue.SettlementPricePer1K,
			SupplierShare:        catalogue.SupplierShare,
			ScheduleSHA256:       catalogue.ScheduleSHA256,
			BoardSHA256:          catalogue.BoardSHA256,
			PriceFormula:         catalogue.PriceFormula,
			SettlementCurrency:   catalogue.SettlementCurrency,
		},
		CellID:    cost.CellID,
		RuntimeID: cost.RuntimeID,
		Engine:    cost.Engine,
		HWClass:   cost.HWClass,
		Tier:      tier,

		Samples:         cost.Samples,
		Units:           cost.Units,
		MedianMsPerUnit: cost.MedianMsPerUnit,
		Measured:        cost.Measured,

		BuyerPricePer1KUnits:                   catalogue.ReferencePricePer1K,
		SupplierEntitlementPolicy:              supplierEntitlementPolicyCancelled,
		DurationCancelsFromSupplierEntitlement: true,

		RetryRate:           cost.RetryRate,
		VerificationSamples: cost.VerificationSamples,
		VerificationFails:   cost.VerificationFails,
		TerminalAttempts:    cost.TerminalAttempts,
		TerminalFails:       cost.TerminalFails,
		UnknownComponents:   append([]string(nil), cost.Unknown...),
		DurationCanDifferentiate: []string{
			"admission_expected_supplier_usd_hr",
			"median_ms_per_unit",
			"measured_units_per_sec",
			"provider_cost_when_cloud_backed",
			"energy_usd_per_unit_partial",
			"capacity_more_throughput_at_equal_price",
		},
	}
	if p.UnknownComponents == nil {
		p.UnknownComponents = unknownCostComponents()
	}

	// Measured throughput from median ms/unit. Zero median is not infinite rate.
	if cost.MedianMsPerUnit > 0 && !math.IsNaN(cost.MedianMsPerUnit) && !math.IsInf(cost.MedianMsPerUnit, 0) {
		p.MeasuredUnitsPerSec = 1000.0 / cost.MedianMsPerUnit
		p.AdmissionUnitsPerSec = p.MeasuredUnitsPerSec
	}

	// Supplier per unit: prefer the frozen money-path measurement (already the
	// cancelled form). Fall back to the catalogue closed form only when the
	// measurement is empty and the catalogue is complete — still cancelled.
	p.SupplierUSDPerUnit = cost.SupplierUSDPerUnit
	if p.SupplierUSDPerUnit <= 0 &&
		catalogue.ReferencePricePer1K > 0 && catalogue.SupplierShare > 0 {
		// units/1000 × price × share — no unitsPerSec.
		p.SupplierUSDPerUnit = catalogue.ReferencePricePer1K / 1000.0 *
			catalogue.SupplierShare * tierMultiplier(tier)
		p.BuyerPricePer1KUnits = catalogue.ReferencePricePer1K
	}

	// Admission hourly display: duration-sensitive, not settlement.
	if p.AdmissionUnitsPerSec > 0 && p.BuyerPricePer1KUnits > 0 && catalogue.SupplierShare > 0 {
		p.AdmissionExpectedSupplierUSDHr = expectedSupplierUSDHr(
			p.AdmissionUnitsPerSec, p.BuyerPricePer1KUnits, catalogue.SupplierShare, tier)
	}

	// Reliability rates.
	p.VerificationPassRate = 1.0
	if cost.VerificationSamples > 0 {
		p.VerificationPassRate = 1 - float64(cost.VerificationFails)/float64(cost.VerificationSamples)
	}
	p.TerminalDeliveredRate = 1.0
	if cost.TerminalAttempts > 0 {
		p.TerminalDeliveredRate = 1 - float64(cost.TerminalFails)/float64(cost.TerminalAttempts)
	}
	if p.VerificationPassRate > 0 && p.TerminalDeliveredRate > 0 {
		p.ReliabilityMultiplier = (1 + cost.RetryRate) / p.VerificationPassRate / p.TerminalDeliveredRate
	}

	// Selector ranking cost: same expression as MeasuredCellCost.ExpectedVerifiedOutcomeUSDPerUnit.
	if vo, ok := cost.ExpectedVerifiedOutcomeUSDPerUnit(); ok {
		p.VerifiedOutcomeUSDPerUnit = vo
		p.VerifiedOutcomeOK = true
	} else if p.SupplierUSDPerUnit > 0 && p.VerificationPassRate > 0 && p.TerminalDeliveredRate > 0 {
		vo := p.SupplierUSDPerUnit * p.ReliabilityMultiplier
		if !math.IsNaN(vo) && !math.IsInf(vo, 0) {
			p.VerifiedOutcomeUSDPerUnit = vo
			p.VerifiedOutcomeOK = true
		}
	}
	p.VerifiedOutcomeBasis = "supplier_usd_per_unit × (1+retry_rate) / verification_pass_rate / terminal_delivered_rate; " +
		"supplier_usd_per_unit is catalogue units×price×share (duration cancelled)"

	// Provider cost: duration-sensitive for cloud-backed cells. Never invent a
	// rate; surface the withdrawn-receipt DEFECT when that is the governing row.
	p.ProviderCost = projectProviderCostTerm(cost.CellID, cost.HWClass, cost.MedianMsPerUnit)

	// Energy partial from measured duration × governed watts × defaulted kWh.
	// ASSUMED watts stay ASSUMED; never presented as full verified-outcome cost.
	p.EnergyPartial = projectEnergyPartialTerm(cost.HWClass, cost.MedianMsPerUnit)

	// Storage/transfer and utilization: unknown without attributed bytes / meter.
	p.StorageTransfer = CellEconomicsTerm{
		Name:         "storage_and_transfer",
		Knowledge:    CategoryUnknown,
		WouldRequire: "accepted/actual storage and egress bytes bound into a PricingDecision CostSchedule model for this cell's unit of work",
		Basis:        "CostSchedule publishes defaulted rates with provenance, but rates without bytes are not a cost",
	}
	p.Utilization = CellEconomicsTerm{
		Name:         "utilization",
		Knowledge:    CategoryUnknown,
		WouldRequire: "a production utilization signal (busy fraction of the placement over the billable window), not a default of 1.0",
		Basis:        "absent on MeasuredCellCost; admission display rates are while-executing only",
	}

	// True net: structurally unavailable while any named category is unknown.
	blocking := projectionBlockingCategories(p)
	p.MercTrueNet = MercTrueNetContribution{
		Unavailable: ptrUnavailable(unavailable(
			"merc_true_net_contribution",
			"true net contribution remains structurally unavailable while any named cost category is UNKNOWN or ASSUMED; "+
				"a gross platform ledger residual must never be presented as net",
			blocking,
		)),
	}
	// No gross platform row is invented here: without a frozen PricingDecision
	// there is no buyer-minus-supplier residual to quote. Leaving it nil is the
	// honest state, not a zero.

	p.Confidence = cellEconomicsConfidence(cost)
	p.EvidenceAuthority = cellEconomicsEvidenceAuthority(cost, catalogue, p)
	return p
}

// ProjectCellEconomicsMap projects every measured cell in a hardware class map.
func ProjectCellEconomicsMap(
	costs map[string]MeasuredCellCost,
	catalogue CataloguePriceAuthority,
	tier string,
) map[string]CellEconomicsProjection {
	out := make(map[string]CellEconomicsProjection, len(costs))
	for id, c := range costs {
		out[id] = ProjectCellEconomics(c, catalogue, tier)
	}
	return out
}

// VerifiedOutcomeCostsTie reports whether two projections have the same
// selector ranking cost within pricesTieWithin. Used to pin the census
// prediction: without undoing cancellation, equal-reliability cells on one
// model still tie on verified-outcome cost even when duration differs.
func VerifiedOutcomeCostsTie(a, b CellEconomicsProjection) bool {
	if !a.VerifiedOutcomeOK || !b.VerifiedOutcomeOK {
		return false
	}
	if a.VerifiedOutcomeUSDPerUnit <= 0 || b.VerifiedOutcomeUSDPerUnit <= 0 {
		return false
	}
	mid := (a.VerifiedOutcomeUSDPerUnit + b.VerifiedOutcomeUSDPerUnit) / 2
	if mid <= 0 {
		return false
	}
	frac := math.Abs(a.VerifiedOutcomeUSDPerUnit-b.VerifiedOutcomeUSDPerUnit) / mid
	return frac < pricesTieWithin
}

// projectProviderCostTerm maps provider_cost_authority into a projection term.
// A withdrawn-receipt rate is surfaced as Defect, never silently as governed.
func projectProviderCostTerm(cellID, hwClass string, medianMsPerUnit float64) CellEconomicsTerm {
	term := CellEconomicsTerm{Name: "provider_cost"}
	if strings.TrimSpace(cellID) == "" {
		term.Knowledge = CategoryUnknown
		term.WouldRequire = "a runtime_cell_id on the measured task so cloud-backed classification can run"
		return term
	}
	cloud, found := cellIsCloudBacked(cellID)
	if !found {
		term.Knowledge = CategoryUnknown
		term.WouldRequire = "cell " + cellID + " present in the runtime-cell authority"
		term.Basis = "cell not in runtime-cell authority; provider cost cannot be classified"
		return term
	}
	if !cloud {
		term.Knowledge = CategoryNotApplicable
		term.Source = "control/provider_cost_authority.go"
		term.Basis = "owned or community supply: Merc pays catalogue supplier entitlement; " +
			"provider pod rate is not applicable"
		return term
	}
	classes := []string{}
	if hwClass != "" {
		classes = []string{hwClass}
	}
	// Expected seconds for ONE unit at the measured median. Zero median refuses
	// rather than modeling a free cloud pod.
	expectedSeconds := 0.0
	if medianMsPerUnit > 0 {
		expectedSeconds = medianMsPerUnit / 1000.0
	}
	rate, ok := resolveProviderRateUSDHr(cellID, classes)
	if !ok {
		term.Knowledge = CategoryUnknown
		term.WouldRequire = "a governed provider rate for the cell's hardware class, " +
			"bound to the same authority scripts/runpod-spend-guard.py uses"
		term.Basis = "cloud-backed cell has no resolvable governed provider rate"
		return term
	}
	defect := ""
	if strings.Contains(rate.Provenance, "DEFECT") || strings.Contains(rate.Provenance, "withdrawn") {
		defect = rate.Provenance
	}
	if expectedSeconds <= 0 {
		term.Knowledge = CategoryUnknown
		term.WouldRequire = "positive median_ms_per_unit so provider cost can be modeled as rate × duration"
		term.Basis = fmt.Sprintf("governed rate $%.4f/hr available but duration is unmeasured", rate.CostPerHrUSD)
		term.Defect = defect
		term.Source = rate.Provenance
		return term
	}
	nanos, err := providerCostNanos(rate.CostPerHrUSD, expectedSeconds)
	if err != nil {
		term.Knowledge = CategoryUnknown
		term.Basis = "provider cost model refused: " + err.Error()
		term.Defect = defect
		term.Source = rate.Provenance
		return term
	}
	// Knowledge is KNOWN only when the rate is not DEFECT-flagged. A defective
	// authority still emits the number for continuity but must not be read as
	// governed true-net input.
	knowledge := CategoryKnown
	if defect != "" {
		knowledge = CategoryAssumed
	}
	term.Knowledge = knowledge
	term.MoneyUSD = nanosToEconomicUSD(nanos)
	term.Source = rate.Provenance
	term.Basis = fmt.Sprintf(
		"cloud provider $%.4f/hr × %.6fs per unit (median_ms_per_unit=%.4f); duration-sensitive platform cost, not supplier entitlement",
		rate.CostPerHrUSD, expectedSeconds, medianMsPerUnit)
	term.Defect = defect
	return term
}

// projectEnergyPartialTerm models watts × duration × defaulted electricity.
// ASSUMED watts stay ASSUMED. The dollar figure is always at most DEFAULTED
// because electricity is defaultElectricityUSDPerKWh, not a metered invoice.
func projectEnergyPartialTerm(hwClass string, medianMsPerUnit float64) CellEconomicsTerm {
	term := CellEconomicsTerm{Name: "energy_usd_per_unit_partial"}
	if medianMsPerUnit <= 0 {
		term.Knowledge = CategoryUnknown
		term.WouldRequire = "positive median_ms_per_unit from MeasuredCellCost"
		return term
	}
	entry, ok := sustainedWattsByHWClass[hwClass]
	if !ok || entry.Watts() <= 0 {
		term.Knowledge = CategoryUnknown
		term.WouldRequire = "hardware class present in sustainedWattsByHWClass with provenance"
		return term
	}
	// joules = watts × seconds; USD = joules / 3.6e6 × $/kWh
	seconds := medianMsPerUnit / 1000.0
	joules := entry.Watts() * seconds
	usd := joules / 3.6e6 * defaultElectricityUSDPerKWh
	knowledge := CategoryDefaulted
	if entry.Kind() == wattKindAssumed {
		knowledge = CategoryAssumed
	}
	term.Knowledge = knowledge
	term.MoneyUSD = usd
	term.NonMoney = joules
	term.NonMoneyUnit = "joules_per_unit"
	term.Source = entry.Provenance() + " + control/pricing.go:defaultElectricityUSDPerKWh"
	term.Basis = fmt.Sprintf(
		"%.1f W (%s) × %.6fs/unit × $%.2f/kWh; PARTIAL energy only — not full verified-outcome cost; "+
			"package boundary and electricity are not a metered invoice",
		entry.Watts(), entry.Kind(), seconds, defaultElectricityUSDPerKWh)
	return term
}

func projectionBlockingCategories(p CellEconomicsProjection) []string {
	var blocking []string
	add := func(t CellEconomicsTerm) {
		switch t.Knowledge {
		case CategoryUnknown, CategoryAssumed:
			blocking = append(blocking, t.Name)
		}
	}
	add(p.ProviderCost)
	add(p.EnergyPartial)
	add(p.StorageTransfer)
	add(p.Utilization)
	for _, u := range p.UnknownComponents {
		blocking = append(blocking, u)
	}
	return blocking
}

// cellEconomicsConfidence scales with sample count toward 1.0 at and above
// minCellCostSamples. Under-sampled cells stay low so a reader cannot treat
// three tasks as a fleet claim.
func cellEconomicsConfidence(cost MeasuredCellCost) float64 {
	if cost.Samples <= 0 {
		return 0
	}
	c := float64(cost.Samples) / float64(minCellCostSamples)
	if c > 1 {
		c = 1
	}
	// Verification failures and terminal failures cut confidence: a cheap unit
	// that often fails is not a confident measurement of verified cost.
	if cost.VerificationSamples > 0 && cost.VerificationFails > 0 {
		c *= 1 - float64(cost.VerificationFails)/float64(cost.VerificationSamples)
	}
	if cost.TerminalAttempts > 0 && cost.TerminalFails > 0 {
		c *= 1 - float64(cost.TerminalFails)/float64(cost.TerminalAttempts)
	}
	if c < 0 {
		return 0
	}
	return c
}

func cellEconomicsEvidenceAuthority(
	cost MeasuredCellCost, catalogue CataloguePriceAuthority, p CellEconomicsProjection,
) []string {
	auth := []string{
		"control/runtime_cell_cost.go#MeasuredCellCost",
		"control/pricing_decision.go#exactTaskEconomics",
		"docs/MASTER_PROGRAMME_LEDGER.md#throughput-cancels",
	}
	if catalogue.ScheduleSHA256 != "" {
		auth = append(auth, "catalogue_schedule:"+catalogue.ScheduleSHA256)
	}
	if catalogue.BoardSHA256 != "" {
		auth = append(auth, "price_board:"+catalogue.BoardSHA256)
	}
	if cost.CellID != "" {
		auth = append(auth, "runtime_cell:"+cost.CellID)
	}
	if p.ProviderCost.Source != "" {
		auth = append(auth, "provider_cost_authority:"+p.ProviderCost.Source)
	}
	if p.EnergyPartial.Source != "" {
		auth = append(auth, "energy:"+p.EnergyPartial.Source)
	}
	return auth
}

func firstNonEmpty(values ...string) string {
	for _, v := range values {
		if strings.TrimSpace(v) != "" {
			return v
		}
	}
	return ""
}

// censusRequiredUSD is the closed-form supplier entitlement the ledger records:
// units/1000 × price × share. unitsPerSec must not appear.
func censusRequiredUSD(units, pricePer1K, share float64) float64 {
	return units / 1000.0 * pricePer1K * share
}

// censusRequiredViaThroughput is the expanded form that cancels:
//
//	ceiling($/hr) = unitsPerSec × 3600/1000 × price × share
//	seconds       = units / unitsPerSec
//	required      = ceiling × seconds / 3600
func censusRequiredViaThroughput(units, unitsPerSec, pricePer1K, share float64) float64 {
	if unitsPerSec <= 0 {
		return math.NaN()
	}
	ceiling := unitsPerSec * 3600.0 / 1000.0 * pricePer1K * share
	seconds := units / unitsPerSec
	return ceiling * seconds / 3600.0
}
