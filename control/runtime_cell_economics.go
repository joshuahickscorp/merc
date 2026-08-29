package main

import (
	"fmt"
	"math"
	"strings"
)

// Per-cell supplier-liability and partial platform economics as a selector projection.
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
// exposes SupplierLiabilityUSDPerVerifiedUnit (the eventual accepted supplier
// payout) beside separate reliability observations and duration-sensitive
// platform partials. It is explicitly a supplier-liability proxy and never a
// complete cost or a rewrite of settlement.

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
	Name      string                 `json:"name"`
	Knowledge CellEconomicsKnowledge `json:"knowledge"`
	Currency  string                 `json:"currency,omitempty"`
	// MoneyUSD is a legacy wire name for a settlement-major-unit amount. Currency
	// is authoritative and must be present whenever money is populated.
	MoneyUSD     float64 `json:"money_usd,omitempty"`
	NonMoney     float64 `json:"non_money,omitempty"`
	NonMoneyUnit string  `json:"non_money_unit,omitempty"`
	Source       string  `json:"source,omitempty"`
	Basis        string  `json:"basis,omitempty"`
	WouldRequire string  `json:"would_require,omitempty"`
	// Defect is set when the governing authority itself is flagged defective
	// (e.g. provider rate derived from a withdrawn receipt). A defective term
	// remains UNKNOWN and carries provenance, never canonical money.
	Defect string `json:"defect,omitempty"`
}

// CellEconomicsProjection is one cell's full economics view for selection.
//
// Conservation of authority:
//   - One canonical PricingDecision freezes buyer/supplier money at acceptance.
//   - This projection is re-derivable from MeasuredSupplierLiabilityProxy +
//     catalogue + cell authority. Re-deriving it never mutates a frozen
//     PricingDecision.
//   - SupplierLiabilityUSDPerVerifiedUnit is the eventual accepted payout only.
//     Reliability gates eligibility but never multiplies money for unpaid
//     attempts. Duration enters only partial platform terms (provider, energy)
//     and the admission hourly display rate — never supplier entitlement/unit.
type CellEconomicsProjection struct {
	SchemaVersion int    `json:"schema_version"`
	Kind          string `json:"kind"`
	// Currency is the settlement-major-unit authority for every legacy `_usd`
	// money field below. The wire names are retained for compatibility; they do
	// not authorize relabelling a CAD amount as USD.
	Currency string `json:"currency"`

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

	// Measured supplier-liability proxy: the payable entitlement for the one
	// eventual accepted unit. Rejected verification attempts and terminal
	// failures receive no supplier settlement, so their reliability burden is
	// reported separately above and never capitalized into payable liability.
	// This contains no platform-side costs and cannot authorize a total-cost
	// selection while those costs are unknown.
	SupplierLiabilityUSDPerVerifiedUnit float64 `json:"supplier_liability_proxy_usd_per_verified_unit"`
	SupplierLiabilityAvailable          bool    `json:"supplier_liability_proxy_available"`
	SupplierLiabilityBasis              string  `json:"supplier_liability_proxy_basis"`

	// Duration-sensitive platform partials. These MAY differ across cells of
	// the same model. They are not supplier entitlement and must not be
	// presented as complete platform cost or as true net.
	ProviderCost    CellEconomicsTerm `json:"provider_cost"`
	EnergyPartial   CellEconomicsTerm `json:"energy_usd_per_unit_partial"`
	StorageTransfer CellEconomicsTerm `json:"storage_and_transfer"`
	Utilization     CellEconomicsTerm `json:"utilization"`

	// PlatformDeliveryUSDPerUnit is only the known cell-resolved partial for one
	// verified unit: supplier liability plus provider per unit when provider is
	// KNOWN. PlatformDeliveryOK is true only when every named platform component
	// is known or not applicable. The partial may still be populated when OK is
	// false so the known money is not lost, but it must not be presented as a
	// complete platform-delivery cost. It never rewrites supplier entitlement.
	PlatformDeliveryUSDPerUnit float64 `json:"platform_delivery_usd_per_unit,omitempty"`
	PlatformDeliveryOK         bool    `json:"platform_delivery_ok"`
	PlatformDeliveryBasis      string  `json:"platform_delivery_basis,omitempty"`
	EntitlementResolution      string  `json:"entitlement_resolution"`

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

// ProjectCellEconomics binds one MeasuredSupplierLiabilityProxy into a full projection.
//
// catalogue may be zero-valued: supplier liability comes only from frozen task
// payouts. A catalogue price is denominated in settlement input units, while a
// measured proxy is denominated in accepted output records; without the frozen
// job geometry there is no honest conversion between them. The catalogue is
// therefore descriptive here and never a fallback liability authority.
func ProjectCellEconomics(
	cost MeasuredSupplierLiabilityProxy,
	catalogue CataloguePriceAuthority,
	tier string,
) CellEconomicsProjection {
	if tier == "" {
		tier = "batch"
	}
	buyerPricePer1K, settlementCurrency, buyerPriceKnown :=
		cellEconomicsSettlementPrice(catalogue)
	p := CellEconomicsProjection{
		SchemaVersion: cellEconomicsProjectionSchemaVersion,
		Kind:          cellEconomicsProjectionKind,
		Currency:      settlementCurrency,
		ModelContract: CellEconomicsModelContract{
			ModelID:              firstNonEmpty(catalogue.ModelID, cost.ModelRef),
			JobType:              firstNonEmpty(catalogue.JobType, cost.JobType),
			ReferencePricePer1K:  catalogue.ReferencePricePer1K,
			SettlementPricePer1K: catalogue.SettlementPricePer1K,
			SupplierShare:        catalogue.SupplierShare,
			ScheduleSHA256:       catalogue.ScheduleSHA256,
			BoardSHA256:          catalogue.BoardSHA256,
			PriceFormula:         catalogue.PriceFormula,
			SettlementCurrency:   settlementCurrency,
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

		BuyerPricePer1KUnits:                   buyerPricePer1K,
		SupplierEntitlementPolicy:              supplierEntitlementPolicyCancelled,
		DurationCancelsFromSupplierEntitlement: true,

		RetryRate:           cost.RetryRate,
		VerificationSamples: cost.VerificationSamples,
		VerificationFails:   cost.VerificationFails,
		TerminalAttempts:    cost.TerminalAttempts,
		TerminalFails:       cost.TerminalFails,
		UnknownComponents:   append([]string(nil), cost.UnknownPlatformCostComponents...),
		DurationCanDifferentiate: []string{
			"admission_expected_supplier_usd_hr",
			"median_ms_per_unit",
			"measured_units_per_sec",
			"provider_cost_when_cloud_backed",
			"platform_delivery_usd_per_unit_when_provider_known",
			"energy_usd_per_unit_partial",
			"capacity_more_throughput_at_equal_supplier_liability",
		},
	}
	if p.UnknownComponents == nil {
		p.UnknownComponents = unknownPlatformCostComponents()
	}

	// Measured throughput from median ms/unit. Zero median is not infinite rate.
	if cost.MedianMsPerUnit > 0 && !math.IsNaN(cost.MedianMsPerUnit) && !math.IsInf(cost.MedianMsPerUnit, 0) {
		p.MeasuredUnitsPerSec = 1000.0 / cost.MedianMsPerUnit
		p.AdmissionUnitsPerSec = p.MeasuredUnitsPerSec
	}

	// Supplier per accepted output: only the frozen money-path measurement has
	// the matching denominator. Do not substitute catalogue price/1k here: text
	// catalogue units are token-like input units, not output records.
	p.SupplierUSDPerUnit = cost.SupplierUSDPerUnit

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

	// Supplier-liability proxy: the settlement liability for the eventual
	// accepted unit. Retry, rejection and terminal-failure rates remain explicit
	// reliability evidence above; those attempts are unpaid and do not multiply
	// supplier liability.
	currencyMatches := cellEconomicsSupplierCurrencyMatches(
		cost.Currency, settlementCurrency, catalogue)
	if !currencyMatches {
		// This projection has exactly one money unit. Preserve the refusal reason
		// below, but do not retain a numeric payout that belongs to another unit:
		// downstream gross arithmetic must have no mismatched operands to reach for.
		p.SupplierUSDPerUnit = 0
	}
	if liability, ok := eligibleMeasuredSupplierLiability(cost); ok && currencyMatches {
		p.SupplierLiabilityUSDPerVerifiedUnit = liability
		p.SupplierLiabilityAvailable = true
	}
	p.Measured = p.SupplierLiabilityAvailable
	// Active-hour payout projection: both factors share the accepted-output
	// denominator measured on this cell. It is descriptive, not settlement and
	// not the quote admission ceiling. Refused/under-sampled reliability never
	// gets an earnings display merely because it carried a raw duration.
	if p.SupplierLiabilityAvailable && p.AdmissionUnitsPerSec > 0 && p.SupplierUSDPerUnit > 0 {
		p.AdmissionExpectedSupplierUSDHr =
			p.AdmissionUnitsPerSec * p.SupplierUSDPerUnit * 3600
	}
	p.SupplierLiabilityBasis = "eventual accepted supplier payout per verified unit in model_contract.settlement_currency; rejected verification attempts and terminal failures are unpaid; " +
		"supplier_usd_per_unit is frozen task payout divided by accepted output records (duration cancelled); " +
		"catalogue input-unit geometry remains in the frozen PricingDecision; availability requires the same strict sample, reliability, runtime/build, and exact-geometry authority as selector/promotion; " +
		"reliability burden and platform costs excluded"
	if !currencyMatches {
		p.SupplierLiabilityBasis = fmt.Sprintf(
			"REFUSED: measured payout currency %q conflicts with catalogue settlement currency %q",
			cost.Currency, catalogue.SettlementCurrency)
	}
	if !buyerPriceKnown {
		p.UnknownComponents = append(p.UnknownComponents,
			"buyer_settlement_price_per_1k")
	}

	// Provider cost: duration-sensitive for cloud-backed cells. Never invent a
	// rate; surface the withdrawn-receipt DEFECT when that is the governing row.
	p.ProviderCost = projectProviderCostTerm(
		cost.CellID, cost.HWClass, cost.MedianMsPerUnit, catalogue)

	// Energy partial from measured duration × governed watts × defaulted kWh.
	// ASSUMED watts stay ASSUMED; never presented as complete platform cost.
	p.EnergyPartial = projectEnergyPartialTerm(cost.HWClass, cost.MedianMsPerUnit, catalogue)

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
		Basis:        "absent on MeasuredSupplierLiabilityProxy; admission display rates are while-executing only",
	}

	// Cell-resolved known platform partial. Supplier settlement stays cancelled;
	// this figure adds provider when it is KNOWN. Completeness is decided below
	// only after every named platform component has been classified.
	p.EntitlementResolution = cellEntitlementResolutionCellResolved
	if p.SupplierLiabilityAvailable {
		switch p.ProviderCost.Knowledge {
		case CategoryKnown:
			if !sameCellEconomicsCurrency(p.Currency, p.ProviderCost.Currency) {
				p.PlatformDeliveryOK = false
				p.PlatformDeliveryBasis = fmt.Sprintf(
					"provider cost currency %q does not match supplier-liability currency %q; platform delivery refused",
					p.ProviderCost.Currency, p.Currency)
				break
			}
			p.PlatformDeliveryUSDPerUnit = p.SupplierLiabilityUSDPerVerifiedUnit + p.ProviderCost.MoneyUSD
			p.PlatformDeliveryOK = true
			p.PlatformDeliveryBasis = "supplier_liability_proxy_usd_per_verified_unit + provider_cost per unit " +
				"(cell_resolved_platform_v1); provider is duration-sensitive and does not cancel"
		case CategoryNotApplicable:
			p.PlatformDeliveryUSDPerUnit = p.SupplierLiabilityUSDPerVerifiedUnit
			p.PlatformDeliveryOK = true
			p.PlatformDeliveryBasis = "supplier_liability_proxy_usd_per_verified_unit only; provider not applicable " +
				"on owned/community supply; other platform components remain separately unknown"
		default:
			// Unknown/assumed provider: refuse to invent a complete platform delivery.
			p.PlatformDeliveryOK = false
			p.PlatformDeliveryBasis = "provider cost is " + string(p.ProviderCost.Knowledge) +
				"; platform delivery refused rather than treating it as zero"
		}
	}

	// True net: structurally unavailable while any named category is unknown.
	blocking := projectionBlockingCategories(p)
	if p.PlatformDeliveryOK && len(blocking) > 0 {
		p.PlatformDeliveryOK = false
		p.PlatformDeliveryBasis += "; known partial only: complete platform delivery refused while " +
			strings.Join(blocking, ", ") + " remain UNKNOWN or ASSUMED"
	}
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

// cellEconomicsSettlementPrice returns the buyer price in its settlement
// currency. A reference price may stand in only for a same-currency legacy
// authority. Once the schedule names a different settlement currency, a
// missing settlement price is an unknown, never an invitation to subtract USD
// reference money from CAD supplier payouts.
func cellEconomicsSettlementPrice(
	catalogue CataloguePriceAuthority,
) (price float64, currency string, ok bool) {
	referenceCurrency := strings.ToLower(strings.TrimSpace(catalogue.ReferenceCurrency))
	if referenceCurrency == "" {
		referenceCurrency = catalogueReferenceCurrency
	}
	currency = strings.ToLower(strings.TrimSpace(catalogue.SettlementCurrency))
	if currency == "" {
		currency = referenceCurrency
	}
	// The catalogue reference unit is a system-wide USD authority. Accepting a
	// caller-provided alternative here would let an incomplete projection invent
	// a second reference currency that canonical pricing can never publish.
	if referenceCurrency != catalogueReferenceCurrency {
		return 0, currency, false
	}
	parsed, err := ParseCurrency(currency)
	if err != nil || parsed.Code() != currency ||
		catalogue.ReferencePricePer1K <= 0 ||
		math.IsNaN(catalogue.ReferencePricePer1K) ||
		math.IsInf(catalogue.ReferencePricePer1K, 0) {
		return 0, currency, false
	}

	if currency == referenceCurrency {
		// Legacy same-currency projection fixtures predate the append-only catalogue
		// and name only the USD reference price. Preserve that narrow compatibility
		// path. If a settlement price or FX rate is supplied, however, it must agree
		// with identity FX; an explicit mismatch is an authority refusal, not a
		// reason to fall back to the reference number.
		if catalogue.ReferenceToSettlementRate != 0 &&
			(!finiteNonNegative(catalogue.ReferenceToSettlementRate) ||
				math.Abs(catalogue.ReferenceToSettlementRate-1) > 1e-12) {
			return 0, currency, false
		}
		if catalogue.SettlementPricePer1K == 0 {
			return catalogue.ReferencePricePer1K, currency, true
		}
		want := ceilPricePer1K(catalogue.ReferencePricePer1K)
		if !finiteNonNegative(catalogue.SettlementPricePer1K) ||
			catalogue.SettlementPricePer1K <= 0 ||
			math.Abs(catalogue.SettlementPricePer1K-want) > 1e-10 {
			return 0, currency, false
		}
		return catalogue.SettlementPricePer1K, currency, true
	}

	// Cross-currency money exists only under the complete append-only catalogue
	// and frozen FX authority. A positive CAD number supplied by a caller is not
	// provenance and must not become a buyer-minus-supplier operand.
	if err := validateCataloguePriceAuthority(catalogue); err != nil {
		return 0, currency, false
	}
	return catalogue.SettlementPricePer1K, currency, true
}

// cellEconomicsSupplierCurrencyMatches binds the legacy `_usd` payout field to
// the projection's actual settlement currency. Blank currency survives only on
// the historical same-currency USD fixture path. It can never be inferred as a
// cross-currency payout.
func cellEconomicsSupplierCurrencyMatches(
	rawCurrency, settlementCurrency string,
	catalogue CataloguePriceAuthority,
) bool {
	settlement, err := ParseCurrency(settlementCurrency)
	if err != nil || settlement.Code() != settlementCurrency {
		return false
	}
	rawCurrency = strings.TrimSpace(rawCurrency)
	if rawCurrency != "" {
		measured, err := ParseCurrency(rawCurrency)
		return err == nil && measured.Equal(settlement)
	}
	referenceCurrency := strings.ToLower(strings.TrimSpace(catalogue.ReferenceCurrency))
	if referenceCurrency == "" {
		referenceCurrency = catalogueReferenceCurrency
	}
	return settlementCurrency == referenceCurrency &&
		referenceCurrency == catalogueReferenceCurrency
}

func sameCellEconomicsCurrency(a, b string) bool {
	ca, err := ParseCurrency(a)
	if err != nil {
		return false
	}
	cb, err := ParseCurrency(b)
	return err == nil && ca.Equal(cb)
}

// ProjectCellEconomicsMap projects every measured cell in a hardware class map.
func ProjectCellEconomicsMap(
	costs map[string]MeasuredSupplierLiabilityProxy,
	catalogue CataloguePriceAuthority,
	tier string,
) map[string]CellEconomicsProjection {
	out := make(map[string]CellEconomicsProjection, len(costs))
	for id, c := range costs {
		out[id] = ProjectCellEconomics(c, catalogue, tier)
	}
	return out
}

// SupplierLiabilityProxiesTie reports whether two projections have the same
// supplier-liability proxy within pricesTieWithin. This permits only an
// equal-liability throughput comparison, not a total-cost claim.
func SupplierLiabilityProxiesTie(a, b CellEconomicsProjection) bool {
	if !a.SupplierLiabilityAvailable || !b.SupplierLiabilityAvailable {
		return false
	}
	if !sameCellEconomicsCurrency(a.Currency, b.Currency) {
		return false
	}
	if a.SupplierLiabilityUSDPerVerifiedUnit <= 0 || b.SupplierLiabilityUSDPerVerifiedUnit <= 0 {
		return false
	}
	mid := (a.SupplierLiabilityUSDPerVerifiedUnit + b.SupplierLiabilityUSDPerVerifiedUnit) / 2
	if mid <= 0 {
		return false
	}
	frac := math.Abs(a.SupplierLiabilityUSDPerVerifiedUnit-b.SupplierLiabilityUSDPerVerifiedUnit) / mid
	return frac < pricesTieWithin
}

// projectProviderCostTerm maps provider_cost_authority into a projection term.
// A withdrawn-receipt rate is surfaced as Defect, never silently as governed.
func projectProviderCostTerm(
	cellID, hwClass string,
	medianMsPerUnit float64,
	catalogue CataloguePriceAuthority,
) CellEconomicsTerm {
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
	if rate.AuthorityStatus != providerRateAuthorityGoverned {
		term.Knowledge = CategoryUnknown
		term.WouldRequire = "a governed provider rate bound to a citable provider price or invoice receipt"
		term.Basis = fmt.Sprintf("provider rate authority status is %q; only %s may enter the selector projection",
			rate.AuthorityStatus, providerRateAuthorityGoverned)
		term.Defect = rate.Provenance
		term.Source = rate.Provenance
		return term
	}
	if expectedSeconds <= 0 {
		term.Knowledge = CategoryUnknown
		term.WouldRequire = "positive median_ms_per_unit so provider cost can be modeled as rate × duration"
		term.Basis = fmt.Sprintf("governed rate $%.4f/hr available but duration is unmeasured", rate.CostPerHrUSD)
		term.Source = rate.Provenance
		return term
	}
	ratePerHr, currencyBasis, err := providerRateInSettlementCurrency(rate, catalogue)
	if err != nil {
		term.Knowledge = CategoryUnknown
		term.WouldRequire = "a provider rate and frozen catalogue FX authority in the projection's settlement currency"
		term.Basis = "provider currency conversion refused: " + err.Error()
		term.Source = rate.Provenance
		return term
	}
	nanos, err := providerCostNanos(ratePerHr, expectedSeconds)
	if err != nil {
		term.Knowledge = CategoryUnknown
		term.Basis = "provider cost model refused: " + err.Error()
		term.Source = rate.Provenance
		return term
	}
	term.Knowledge = CategoryKnown
	term.Currency = catalogue.SettlementCurrency
	// This is a re-derivable per-unit selector projection, not a NUMERIC(12,6)
	// ledger column. Preserve nano-major-unit precision so sub-micro provider
	// differences remain visible instead of collapsing distinct durations to
	// the same six-decimal amount.
	term.MoneyUSD = float64(nanos) / float64(NanosPerMajorUnit)
	term.Source = rate.Provenance
	term.Basis = fmt.Sprintf(
		"cloud provider %.6f %s/hr × %.6fs per unit (median_ms_per_unit=%.4f); %s; duration-sensitive platform cost, not supplier entitlement",
		ratePerHr, catalogue.SettlementCurrency, expectedSeconds, medianMsPerUnit, currencyBasis)
	return term
}

// projectEnergyPartialTerm models watts × duration × defaulted electricity.
// ASSUMED watts stay ASSUMED. The dollar figure is always at most DEFAULTED
// because electricity is defaultElectricityUSDPerKWh, not a metered invoice.
func projectEnergyPartialTerm(
	hwClass string,
	medianMsPerUnit float64,
	catalogue CataloguePriceAuthority,
) CellEconomicsTerm {
	term := CellEconomicsTerm{Name: "energy_usd_per_unit_partial"}
	if medianMsPerUnit <= 0 {
		term.Knowledge = CategoryUnknown
		term.WouldRequire = "positive median_ms_per_unit from MeasuredSupplierLiabilityProxy"
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
	currency := firstNonEmpty(catalogue.SettlementCurrency, catalogueReferenceCurrency)
	moneyMajor := usd
	fxBasis := "USD reference electricity policy"
	if currency != catalogueReferenceCurrency {
		if err := validateCataloguePriceAuthority(catalogue); err != nil ||
			catalogue.ReferenceCurrency != catalogueReferenceCurrency {
			term.Knowledge = CategoryUnknown
			term.NonMoney = joules
			term.NonMoneyUnit = "joules_per_unit"
			term.WouldRequire = "a valid frozen USD-reference-to-settlement FX authority"
			term.Basis = fmt.Sprintf(
				"%.6f J is attributable, but USD electricity cannot be relabelled as %s without frozen FX: %v",
				joules, currency, err)
			return term
		}
		moneyMajor = usd * catalogue.ReferenceToSettlementRate
		fxBasis = fmt.Sprintf("frozen fx_revision=%s schedule_sha256=%s",
			catalogue.FXRevision, catalogue.ScheduleSHA256)
	}
	knowledge := CategoryDefaulted
	if entry.Kind() != wattKindMeasured {
		knowledge = CategoryAssumed
	}
	term.Knowledge = knowledge
	term.Currency = currency
	term.MoneyUSD = moneyMajor
	term.NonMoney = joules
	term.NonMoneyUnit = "joules_per_unit"
	term.Source = entry.Provenance() + " + control/pricing.go:defaultElectricityUSDPerKWh"
	term.Basis = fmt.Sprintf(
		"%.1f W (%s) × %.6fs/unit × $%.2f USD/kWh converted to %.12g %s/unit using %s; PARTIAL energy only — not complete platform cost; "+
			"package boundary and electricity are not a metered invoice",
		entry.Watts(), entry.Kind(), seconds, defaultElectricityUSDPerKWh,
		moneyMajor, currency, fxBasis)
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
// minSupplierLiabilitySamples. Under-sampled cells stay low so a reader cannot treat
// three tasks as a fleet claim.
func cellEconomicsConfidence(cost MeasuredSupplierLiabilityProxy) float64 {
	if cost.Samples <= 0 {
		return 0
	}
	c := float64(cost.Samples) / float64(minSupplierLiabilitySamples)
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
	cost MeasuredSupplierLiabilityProxy, catalogue CataloguePriceAuthority, p CellEconomicsProjection,
) []string {
	auth := []string{
		"control/runtime_cell_cost.go#MeasuredSupplierLiabilityProxy",
		"control/pricing_decision.go#exactTaskEconomics",
		"docs/archive/engineering/PROGRAMME.md#throughput-cancels",
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
