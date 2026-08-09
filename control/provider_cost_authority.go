package main

import (
	"fmt"
	"strings"
)

// Provider (cloud) cost authority.
//
// For owned and community supply, Merc's provider cost is zero: Merc pays the
// supplier entitlement, and the supplier's energy and depreciation are the
// supplier's cost, not Merc's. Marking that as `unknown` permanently blocked
// true net by conflating the supplier's economics with Merc's.
//
// For cloud-backed cells, the provider cost is real and knowable: governed pod
// rate × expected seconds at acceptance, actual pod seconds × rate at
// settlement. Rates share authority with scripts/runpod-spend-guard.py: every
// rate here is the same cost_per_hr_usd the spend guard takes, with provenance
// naming the receipt or published on-demand price it was taken from.
//
// A cloud-backed cell whose rate is not resolvable stays `unknown` and
// correctly blocks true net for that job. Fabricating a rate is refused.

// governedProviderRate is one hardware class's cloud-provider hourly rate.
type governedProviderRate struct {
	// CostPerHrUSD is what RunPod (or a future governed provider) bills per
	// wall-clock hour of pod lifetime. Same unit as spend-guard --cost-per-hr.
	CostPerHrUSD float64
	// Currency is the currency of CostPerHrUSD. It is explicit even while every
	// current row is USD: a bare provider number must never be relabelled as the
	// PricingDecision's settlement currency.
	Currency string
	// AuthorityStatus decides whether this row may enter canonical pricing.
	// WITHDRAWN rows remain visible so historical evidence and diagnostics can
	// explain the defect, but they can never become a MODELED cost component.
	AuthorityStatus string
	// Provenance names the receipt or published price list. Required.
	Provenance string
}

const (
	providerRateAuthorityGoverned  = "GOVERNED"
	providerRateAuthorityWithdrawn = "WITHDRAWN"
	providerRateAuthorityUnbound   = "UNBOUND_REFERENCE"
)

// providerRatesByHWClass is the closed rate table. Keys are the same hardware
// class strings admission and the spend guard reason about. Missing classes are
// unresolvable, not free.
//
// Rates:
//   - nvidia_24gb: $0.16/hr was taken from evidence/runpod/spend-rr7b6uwmivaolh.json
//     (RTX A5000, cost_per_hr_usd). That receipt is WITHDRAWN (mutable image tag;
//     runtime unidentifiable) and citable by nothing. Keeping the rate here is a
//     known defect: a cost authority must not derive from a withdrawn receipt.
//     Re-source from a bound spend receipt or published on-demand list before
//     treating true-net figures as governed. Until then the number is retained
//     only so existing tests do not silently invent a different rate.
//   - nvidia_48gb: $0.44/hr appears in the BOUND RunPod A40 spend receipt
//     evidence/runpod/spend-yau2bzybvhkb5y.json, but nvidia_48gb is only a
//     capacity tier. Worker admission intentionally groups A40, A6000, L40 and
//     other 48GB cards under that class. Until PlacementRequirement freezes the
//     exact provider/SKU allocation, the A40 price cannot govern the whole tier.
//   - nvidia_80gb: $1.19/hr from scripts/runpod-spend-guard.py --self-test is
//     UNBOUND_REFERENCE because an arithmetic fixture is not a provider price
//     authority.
//
// nvidia_180gb has no governed rate in-tree and correctly resolves as unknown.
var providerRatesByHWClass = map[string]governedProviderRate{
	"nvidia_24gb": {
		CostPerHrUSD:    0.16,
		Currency:        catalogueReferenceCurrency,
		AuthorityStatus: providerRateAuthorityWithdrawn,
		Provenance: "DEFECT: $0.16/hr for NVIDIA RTX A5000 was recorded in withdrawn " +
			"evidence/runpod/spend-rr7b6uwmivaolh.json (mutable tag; not citable). " +
			"Must be re-sourced from a bound receipt or published price list.",
	},
	"nvidia_48gb": {
		CostPerHrUSD:    0.44,
		Currency:        catalogueReferenceCurrency,
		AuthorityStatus: providerRateAuthorityUnbound,
		Provenance: "UNBOUND_REFERENCE for generic nvidia_48gb: BOUND RunPod A40 spend receipt " +
			"evidence/runpod/spend-yau2bzybvhkb5y.json establishes cost_per_hr_usd=0.44 for an " +
			"A40 allocation, but the admitted hardware class is memory-only and does not freeze " +
			"provider or GPU SKU; an A6000/L40-class worker must not inherit the A40 price",
	},
	"nvidia_80gb": {
		CostPerHrUSD:    1.19,
		Currency:        catalogueReferenceCurrency,
		AuthorityStatus: providerRateAuthorityUnbound,
		Provenance: "UNBOUND_REFERENCE: $1.19/hr is an example input in " +
			"scripts/runpod-spend-guard.py self-tests, not a bound provider invoice or published price authority",
	},
}

// cellIsCloudBacked reports whether a runtime cell is cloud-backed from the
// runtime-cell authority. Before this change no marker existed; CloudBacked was
// added to authorityCell and set on CUDA provider cells. Community/owned metal
// cells remain false.
func cellIsCloudBacked(cellID string) (cloudBacked bool, found bool) {
	cellID = strings.TrimSpace(cellID)
	if cellID == "" {
		return false, false
	}
	for _, profile := range runtimeAuthority.Runtimes {
		for _, cell := range profile.Cells {
			if cell.ID == cellID {
				return cell.CloudBacked, true
			}
		}
	}
	return false, false
}

// resolveProviderRateUSDHr picks a governed hourly rate for a cloud-backed
// placement. When the placement freezes HW classes, every class must resolve to
// the same rate or the rate is unresolvable (a mixed-class cloud placement has
// no single pod rate). When no HW class is frozen, the cell's profile platforms
// are tried the same way.
//
// ok is false when the rate cannot be resolved — the caller must mark provider
// cost unknown, never default to zero.
func resolveProviderRateUSDHr(cellID string, hwClasses []string) (rate governedProviderRate, ok bool) {
	cloud, found := cellIsCloudBacked(cellID)
	if !found || !cloud {
		return governedProviderRate{}, false
	}
	classes := append([]string(nil), hwClasses...)
	if len(classes) == 0 {
		// Fall back to the profile's declared platforms for this cell.
		for _, profile := range runtimeAuthority.Runtimes {
			for _, cell := range profile.Cells {
				if cell.ID == cellID {
					classes = append(classes, profile.Hardware.Platforms...)
					break
				}
			}
		}
	}
	if len(classes) == 0 {
		return governedProviderRate{}, false
	}
	var chosen governedProviderRate
	var have bool
	for _, class := range classes {
		r, exists := providerRatesByHWClass[class]
		if !exists || r.CostPerHrUSD <= 0 || strings.TrimSpace(r.Currency) == "" ||
			strings.TrimSpace(r.AuthorityStatus) == "" || strings.TrimSpace(r.Provenance) == "" {
			// Any unresolvable class in the set makes the whole rate unknown.
			return governedProviderRate{}, false
		}
		if !have {
			chosen, have = r, true
			continue
		}
		if r != chosen {
			return governedProviderRate{}, false
		}
	}
	return chosen, have
}

// providerCostNanos models cloud-provider cost for expectedSeconds at the
// governed hourly rate. Round UP so acceptance never under-reserves.
//
//	nanos = ceil( costPerHrMajor * NanosPerMajorUnit * expectedSeconds / 3600 )
//
// The rate passed here has already been converted into major units of the
// PricingDecision's settlement currency by providerRateInSettlementCurrency.
// This helper performs scaling and rounding only; it is not an FX authority.
func providerCostNanos(costPerHrMajor float64, expectedSeconds float64) (int64, error) {
	if costPerHrMajor <= 0 || expectedSeconds < 0 {
		return 0, fmt.Errorf(
			"provider cost model refuses non-positive rate %.6f or negative seconds %.6f",
			costPerHrMajor, expectedSeconds)
	}
	if expectedSeconds == 0 {
		return 0, nil
	}
	// rateNanos/hour: round half-away-from-zero on the published dollar rate so
	// $0.16 becomes exactly 160_000_000 nanos/hour.
	rateNanos := int64(costPerHrMajor*float64(NanosPerMajorUnit) + 0.5)
	if rateNanos <= 0 {
		return 0, fmt.Errorf("provider rate %.6f does not produce positive nanos/hour", costPerHrMajor)
	}
	// secs may be fractional; work in integer milliseconds for stability.
	ms := int64(expectedSeconds*1000 + 0.999999) // ceil to ms
	if ms <= 0 {
		return 0, nil
	}
	// nanos = rateNanos * ms / (3600 * 1000)
	return mulDiv(rateNanos, ms, 3600*1000, true)
}

// providerRateInSettlementCurrency converts a usable provider rate into the
// PricingDecision's currency. The catalogue is already the immutable FX
// authority accepted by distributed pricing: its schedule digest, FX revision,
// reference currency and reference-to-settlement rate are all frozen. A bare
// non-USD currency code or a mutable process value is not a substitute.
func providerRateInSettlementCurrency(
	rate governedProviderRate,
	catalogue CataloguePriceAuthority,
) (ratePerHr float64, basis string, err error) {
	if rate.AuthorityStatus != providerRateAuthorityGoverned {
		return 0, "", fmt.Errorf(
			"provider rate authority status is %q; only %s rates may enter canonical pricing: %s",
			rate.AuthorityStatus, providerRateAuthorityGoverned, rate.Provenance)
	}
	if err := validateCataloguePriceAuthority(catalogue); err != nil {
		return 0, "", fmt.Errorf("no frozen catalogue/FX authority: %w", err)
	}
	rateCurrency := strings.ToLower(strings.TrimSpace(rate.Currency))
	switch {
	case rateCurrency == catalogue.SettlementCurrency:
		return rate.CostPerHrUSD,
			fmt.Sprintf("provider rate already denominated in frozen settlement currency %s", catalogue.SettlementCurrency), nil
	case rateCurrency != catalogue.ReferenceCurrency:
		return 0, "", fmt.Errorf(
			"provider rate currency %q is neither frozen reference currency %q nor settlement currency %q",
			rateCurrency, catalogue.ReferenceCurrency, catalogue.SettlementCurrency)
	default:
		converted := rate.CostPerHrUSD * catalogue.ReferenceToSettlementRate
		if !finiteNonNegative(converted) || converted <= 0 {
			return 0, "", fmt.Errorf("frozen provider FX conversion produced invalid rate %.6f", converted)
		}
		return converted, fmt.Sprintf(
			"provider %.6f %s/hr × frozen reference-to-settlement rate %.9f = %.6f %s/hr (fx_revision=%s schedule_sha256=%s)",
			rate.CostPerHrUSD, rateCurrency, catalogue.ReferenceToSettlementRate,
			converted, catalogue.SettlementCurrency, catalogue.FXRevision, catalogue.ScheduleSHA256), nil
	}
}

// providerCostComponentForPlacement builds the PricingCostComponent for the
// frozen placement's cell. Community/owned → not_applicable. Cloud with a
// governed rate and frozen currency/FX authority → modeled. Withdrawn,
// unresolvable, or currency-unbound cloud rates → unknown.
func providerCostComponentForPlacement(
	cellID string,
	hwClasses []string,
	expectedSeconds float64,
	catalogue CataloguePriceAuthority,
) PricingCostComponent {
	cloud, found := cellIsCloudBacked(cellID)
	if !found {
		return unknownCost(
			"runtime cell " + cellID + " is not in the runtime-cell authority; " +
				"provider cost cannot be classified")
	}
	if !cloud {
		return notApplicableCost(
			"Merc's provider cost is not applicable for owned or community supply: " +
				"Merc pays the supplier entitlement, and the supplier's energy and " +
				"depreciation are the supplier's cost, not Merc's (cell " + cellID + ")")
	}
	rate, ok := resolveProviderRateUSDHr(cellID, hwClasses)
	if !ok {
		return unknownCost(
			"cloud-backed cell " + cellID + " has no resolvable governed provider rate " +
				"for its hardware class set; true net is blocked until the rate is " +
				"bound to the same authority scripts/runpod-spend-guard.py uses")
	}
	ratePerHr, currencyBasis, err := providerRateInSettlementCurrency(rate, catalogue)
	if err != nil {
		return unknownCost(
			"cloud-backed cell " + cellID + " provider rate cannot enter canonical pricing: " + err.Error())
	}
	nanos, err := providerCostNanos(ratePerHr, expectedSeconds)
	if err != nil {
		return unknownCost(
			"cloud-backed cell " + cellID + " provider cost could not be modeled: " + err.Error())
	}
	return modeledCost(nanosToEconomicUSD(nanos),
		fmt.Sprintf("governed cloud provider rate %.6f %s/hr × %.6fs expected; %s; %s",
			ratePerHr, catalogue.SettlementCurrency, expectedSeconds, currencyBasis, rate.Provenance))
}
