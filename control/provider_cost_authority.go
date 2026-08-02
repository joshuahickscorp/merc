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
	// Provenance names the receipt or published price list. Required.
	Provenance string
}

// providerRatesByHWClass is the closed rate table. Keys are the same hardware
// class strings admission and the spend guard reason about. Missing classes are
// unresolvable, not free.
//
// Rates:
//   - nvidia_24gb: $0.16/hr from evidence/runpod/spend-rr7b6uwmivaolh.json
//     (RTX A5000, cost_per_hr_usd). That receipt is the spend-guard output for
//     the CUDA-first pod this class maps to.
//   - nvidia_80gb: $1.19/hr from scripts/runpod-spend-guard.py --self-test
//     (A100 reference used by the guard's own arithmetic tests).
//
// nvidia_48gb and nvidia_180gb have no governed rate in-tree yet and correctly
// resolve as unknown.
var providerRatesByHWClass = map[string]governedProviderRate{
	"nvidia_24gb": {
		CostPerHrUSD: 0.16,
		Provenance: "RunPod on-demand cost_per_hr_usd=0.16 for NVIDIA RTX A5000 as " +
			"recorded by scripts/runpod-spend-guard.py in evidence/runpod/spend-rr7b6uwmivaolh.json",
	},
	"nvidia_80gb": {
		CostPerHrUSD: 1.19,
		Provenance: "RunPod on-demand cost_per_hr_usd=1.19 for NVIDIA A100 as used by " +
			"scripts/runpod-spend-guard.py --self-test (same authority the spend guard bills against)",
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
		if !exists || r.CostPerHrUSD <= 0 || strings.TrimSpace(r.Provenance) == "" {
			// Any unresolvable class in the set makes the whole rate unknown.
			return governedProviderRate{}, false
		}
		if !have {
			chosen, have = r, true
			continue
		}
		if r.CostPerHrUSD != chosen.CostPerHrUSD {
			return governedProviderRate{}, false
		}
	}
	return chosen, have
}

// providerCostNanos models cloud-provider cost for expectedSeconds at the
// governed hourly rate. Round UP so acceptance never under-reserves.
//
//	nanos = ceil( costPerHrUSD * NanosPerMajorUnit * expectedSeconds / 3600 )
//
// The rate is denominated in major units of the settlement currency per hour
// (today the spend-guard rates are USD and deployments settle in USD). The
// conversion is pure scaling, not an FX step.
func providerCostNanos(costPerHrUSD float64, expectedSeconds float64) (int64, error) {
	if costPerHrUSD <= 0 || expectedSeconds < 0 {
		return 0, fmt.Errorf(
			"provider cost model refuses non-positive rate %.6f or negative seconds %.6f",
			costPerHrUSD, expectedSeconds)
	}
	if expectedSeconds == 0 {
		return 0, nil
	}
	// rateNanos/hour: round half-away-from-zero on the published dollar rate so
	// $0.16 becomes exactly 160_000_000 nanos/hour.
	rateNanos := int64(costPerHrUSD*float64(NanosPerMajorUnit) + 0.5)
	if rateNanos <= 0 {
		return 0, fmt.Errorf("provider rate %.6f does not produce positive nanos/hour", costPerHrUSD)
	}
	// secs may be fractional; work in integer milliseconds for stability.
	ms := int64(expectedSeconds*1000 + 0.999999) // ceil to ms
	if ms <= 0 {
		return 0, nil
	}
	// nanos = rateNanos * ms / (3600 * 1000)
	return mulDiv(rateNanos, ms, 3600*1000, true)
}

// providerCostComponentForPlacement builds the PricingCostComponent for the
// frozen placement's cell. Community/owned → not_applicable. Cloud with rate →
// modeled. Cloud without rate → unknown.
func providerCostComponentForPlacement(
	cellID string,
	hwClasses []string,
	expectedSeconds float64,
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
	nanos, err := providerCostNanos(rate.CostPerHrUSD, expectedSeconds)
	if err != nil {
		return unknownCost(
			"cloud-backed cell " + cellID + " provider cost could not be modeled: " + err.Error())
	}
	return modeledCost(nanosToEconomicUSD(nanos),
		fmt.Sprintf("governed cloud provider rate $%.4f/hr × %.6fs expected; %s",
			rate.CostPerHrUSD, expectedSeconds, rate.Provenance))
}
