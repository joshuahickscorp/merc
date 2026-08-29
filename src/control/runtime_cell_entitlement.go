package main

import (
	"fmt"
	"math"
	"strings"
)

// Cell-level entitlement resolution at admission.
//
// The catalogue keys prices by (model_id, job_type). Supplier unit entitlement
// is therefore model-level under the cancelled closed form:
//
//	supplier_owed = units/1000 × price × share
//
// Throughput cancels, so two cells on one model produce identical supplier
// payouts for the same units. That is correct for supplier settlement and is
// NOT undone here — undoing it would make a frozen money figure depend on a
// revalidatable benchmark.
//
// What WAS blind is the platform's cost of delivery. Provider cost is
// duration-sensitive (pod rate × seconds) and does not cancel with throughput:
// a faster cell burns fewer provider seconds for the same units, so platform
// delivery cost and true net contribution genuinely differ. Resolving that
// stack against the accepted runtime cell is what this file does.
//
// Policy gate:
//
//   - model_catalogue_cancelled — legacy. No cell block, or a decision frozen
//     before cell resolution existed. Settlement uses the cancelled supplier
//     form only; no platform-delivery figure is claimed.
//   - cell_resolved_platform_v1 — new admissions. Supplier unit payout remains
//     the cancelled form (frozen amount). Platform delivery cost binds the
//     accepted cell's provider and verification terms so true net can differ
//     across cells. Existing settled receipts never see this policy and never
//     recompute under it.

const (
	// cellEntitlementResolutionModelLevel is the pre-cell-resolution settlement
	// form. Historical decisions and any path that freezes no cell block stay
	// here. Settlement arithmetic is identical to economic-nanos-v1 cancelled.
	cellEntitlementResolutionModelLevel = "model_catalogue_cancelled"

	// cellEntitlementResolutionCellResolved is written on every new admission
	// that freezes a runtime-cell block. Supplier settlement stays cancelled;
	// platform delivery and true-net inputs resolve against the accepted cell.
	cellEntitlementResolutionCellResolved = "cell_resolved_platform_v1"

	// Provenance vocabulary for non-money geometric fields. Same words as
	// CostCategoryKnowledge where they overlap, plus "measured" for a quantity
	// taken from a governed benchmark binding rather than a cost schedule.
	fieldProvenanceMeasured      = "measured"
	fieldProvenanceModeled       = "modeled"
	fieldProvenanceDefaulted     = "defaulted"
	fieldProvenanceAssumed       = "assumed"
	fieldProvenanceUnknown       = "unknown"
	fieldProvenanceNotApplicable = "not_applicable"
)

// EconomicsFieldProvenance states whether one frozen field is measured,
// modeled, defaulted, assumed, unknown, or not applicable. A projection that
// cannot tell a measurement from a guess is the thing that made the selector
// economically blind.
type EconomicsFieldProvenance struct {
	Knowledge    string `json:"knowledge"`
	Source       string `json:"source,omitempty"`
	Basis        string `json:"basis,omitempty"`
	WouldRequire string `json:"would_require,omitempty"`
}

// CellPlatformDelivery is a narrow accepted-execution subtotal on one cell:
// physical supplier + modeled provider + modeled verification. It is not the
// complete cost of delivery. Reliability/retry is excluded when unknown (never
// summed as zero); energy, storage, risk and startup stand outside this sum and
// are named on the frozen block separately. Status therefore uses the explicit
// PLATFORM_DELIVERY_LEGS vocabulary rather than claiming every named term was
// modeled.
type CellPlatformDelivery struct {
	TotalUSD   float64
	PerUnitUSD float64
	Status     string
	Basis      string
	Resolution string
	// SupplierUSD is the cancelled-form physical supplier leg. Identical for
	// equal units on one catalogue entry, by construction.
	SupplierUSD float64
	// ProviderUSD is zero when provider is not modeled (N/A or unknown).
	ProviderUSD float64
	// VerificationUSD is zero when verification is not modeled.
	VerificationUSD float64
}

// resolveCellPlatformDelivery folds the modeled legs of platform delivery for
// the accepted cell. Unknown legs are excluded and named in Status — never
// added as zero.
//
// supplierUSD is the frozen physical supplier entitlement for the job (already
// cancelled form). provider and verification are the decision's own components.
func resolveCellPlatformDelivery(
	supplierUSD float64,
	provider, verification PricingCostComponent,
	billableUnits float64,
	resolution string,
) CellPlatformDelivery {
	out := CellPlatformDelivery{
		SupplierUSD: supplierUSD,
		Resolution:  firstNonEmpty(resolution, cellEntitlementResolutionCellResolved),
		Basis: "physical supplier (cancelled units×price×share) + modeled provider " +
			"(pod rate × accepted duration when cloud-backed) + modeled verification; " +
			"unknown legs excluded, never zeroed",
	}
	total := 0.0
	modeled, unknown := 0, 0
	if supplierUSD > 0 && !math.IsNaN(supplierUSD) && !math.IsInf(supplierUSD, 0) {
		total += supplierUSD
		modeled++
	}
	switch provider.Status {
	case pricingCostModeled:
		total += provider.Amount
		out.ProviderUSD = provider.Amount
		modeled++
	case pricingCostUnknown:
		unknown++
	}
	switch verification.Status {
	case pricingCostModeled:
		total += verification.Amount
		out.VerificationUSD = verification.Amount
		modeled++
	case pricingCostUnknown:
		unknown++
	}
	out.TotalUSD = roundEconomicUSD(total)
	if billableUnits > 0 {
		out.PerUnitUSD = out.TotalUSD / billableUnits
	}
	switch {
	case modeled == 0:
		out.Status = platformDeliveryRefused
		out.TotalUSD = 0
		out.PerUnitUSD = 0
	case unknown == 0:
		out.Status = platformDeliveryComplete
	default:
		out.Status = platformDeliveryPartial
	}
	return out
}

// modelContractFromCatalogue copies the catalogue identity into the frozen
// block so a reader does not have to re-resolve a schedule to learn which
// model contract the cell was priced under.
func modelContractFromCatalogue(catalogue CataloguePriceAuthority) CellEconomicsModelContract {
	return CellEconomicsModelContract{
		ModelID:              catalogue.ModelID,
		JobType:              catalogue.JobType,
		ReferencePricePer1K:  catalogue.ReferencePricePer1K,
		SettlementPricePer1K: catalogue.SettlementPricePer1K,
		SupplierShare:        catalogue.SupplierShare,
		ScheduleSHA256:       catalogue.ScheduleSHA256,
		BoardSHA256:          catalogue.BoardSHA256,
		PriceFormula:         catalogue.PriceFormula,
		SettlementCurrency:   catalogue.SettlementCurrency,
	}
}

// buildIdentityProvenance reports the exact execution-build, device and model
// artifact authority frozen by current placement. Older placements remain
// readable, but their missing policy tag is honestly reported as unknown.
func buildIdentityProvenance(placement PlacementRequirement) EconomicsFieldProvenance {
	if placement.Version >= placementRequirementVersion &&
		engineBuildHashPattern.MatchString(placement.EngineBuildHash) &&
		validCurrentEngineBuildIdentityPolicy(placement.EngineBuildIdentityPolicy) &&
		validCanonicalHardwareIdentity(placement.HardwareIdentity) &&
		placement.PerformanceAuthority != nil &&
		placement.PerformanceAuthority.Version == frozenRuntimeCellPerformanceVersion {
		frozen := placement.PerformanceAuthority
		return EconomicsFieldProvenance{
			Knowledge: fieldProvenanceMeasured,
			Source: fmt.Sprintf("%s@benchmark-summary-sha256:%s",
				frozen.Performance.BenchmarkAuthority, frozen.BenchmarkSnapshotSHA256),
			Basis: fmt.Sprintf(
				"accepted engine=%q engine_build_hash=%q engine_build_identity_policy=%q hardware_identity=%q runtime_id=%q cell=%q "+
					"with exact model artifact pin(s) %v under frozen benchmark authority",
				placement.Engine, placement.EngineBuildHash, placement.EngineBuildIdentityPolicy,
				placement.HardwareIdentity,
				placement.RuntimeID, placement.RuntimeCellID, frozen.ModelArtifactPins),
		}
	}
	return EconomicsFieldProvenance{
		Knowledge: fieldProvenanceUnknown,
		Basis: fmt.Sprintf(
			"placement freezes engine=%q engine_build_hash=%q engine_build_identity_policy=%q "+
				"hardware_identity=%q runtime_id=%q cell=%q and runtime_matrix_sha256=%q, but it lacks "+
				"a complete current exact-build/device/artifact authority",
			placement.Engine, placement.EngineBuildHash, placement.EngineBuildIdentityPolicy,
			placement.HardwareIdentity, placement.RuntimeID, placement.RuntimeCellID,
			placement.RuntimeMatrixSHA256),
		WouldRequire: "a versioned exact execution-build hash, canonical hardware fingerprint, and exact " +
			"model artifact pins bound into the accepted PlacementRequirement and rechecked at claim time",
	}
}

// throughputProvenance describes the conservative units/sec admission priced on.
// The rate is governed (benchmark binding + haircut), not a live measurement of
// this job, so it is modeled from measured evidence — never presented as a
// metered sample of the accepted task.
func throughputProvenance(unitsPerSec float64) EconomicsFieldProvenance {
	if unitsPerSec <= 0 {
		return EconomicsFieldProvenance{
			Knowledge:    fieldProvenanceUnknown,
			WouldRequire: "a positive governed admission units/sec from the runtime-cell performance binding",
		}
	}
	return EconomicsFieldProvenance{
		Knowledge: fieldProvenanceModeled,
		Source:    "src/control/runtime_cell_performance.go#admissionUnitsPerSec",
		Basis: fmt.Sprintf(
			"conservative governed admission rate %.6f units/sec (benchmark binding haircut to a "+
				"lower bound); not a live measurement of this job's wall-clock",
			unitsPerSec),
	}
}

// durationProvenance describes expected seconds = billable_units / units_per_sec
// scaled by the compute plan's task geometry. Modeled from the frozen rate.
func durationProvenance(expectedSeconds, unitsPerSec, billableUnits float64) EconomicsFieldProvenance {
	if expectedSeconds <= 0 || unitsPerSec <= 0 || billableUnits <= 0 {
		return EconomicsFieldProvenance{
			Knowledge:    fieldProvenanceUnknown,
			WouldRequire: "positive billable units and governed units/sec so expected duration can be modeled",
		}
	}
	return EconomicsFieldProvenance{
		Knowledge: fieldProvenanceModeled,
		Source:    "src/control/pricing_decision.go#distributedPricingDecisionAtRate",
		Basis: fmt.Sprintf(
			"expected_seconds=%.6f from billable_units=%.4f / conservative_units_per_sec=%.6f "+
				"(scaled by total/primary task geometry); modeled, not measured wall-clock",
			expectedSeconds, billableUnits, unitsPerSec),
	}
}

// supplierEntitlementProvenance names the cancelled settlement form.
func supplierEntitlementProvenance(policy string, amountUSD float64) EconomicsFieldProvenance {
	p := firstNonEmpty(policy, supplierEntitlementPolicyCancelled)
	return EconomicsFieldProvenance{
		Knowledge: fieldProvenanceModeled,
		Source:    "src/control/pricing_decision.go#exactTaskEconomics",
		Basis: fmt.Sprintf(
			"supplier entitlement $%.8f under policy %q: units/1000 × price × share "+
				"(duration cancelled). Model-level catalogue key; cell identity does not "+
				"enter the supplier payout arithmetic",
			amountUSD, p),
	}
}

// buyerPriceProvenance names the frozen buyer charge from the economic plan.
func buyerPriceProvenance(buyerUSD float64) EconomicsFieldProvenance {
	return EconomicsFieldProvenance{
		Knowledge: fieldProvenanceModeled,
		Source:    "src/control/economic_plan.go#BuildEconomicPlan",
		Basis: fmt.Sprintf(
			"buyer price $%.8f frozen from the accepted economic plan (initial buyer charge); "+
				"not re-derived from a later catalogue or benchmark",
			buyerUSD),
	}
}

// providerCostDiffersByDuration reports whether two provider components at
// different accepted durations would produce different modeled amounts under
// the same rate — the identity that makes cell-level resolution economically
// meaningful for cloud-backed supply.
func providerCostDiffersByDuration(rateUSDHr, secondsA, secondsB float64) (deltaUSD float64, differs bool, err error) {
	if rateUSDHr <= 0 {
		return 0, false, fmt.Errorf("provider rate must be positive, got %v", rateUSDHr)
	}
	a, err := providerCostNanos(rateUSDHr, secondsA)
	if err != nil {
		return 0, false, err
	}
	b, err := providerCostNanos(rateUSDHr, secondsB)
	if err != nil {
		return 0, false, err
	}
	delta := nanosToEconomicUSD(a) - nanosToEconomicUSD(b)
	return delta, a != b, nil
}

// settledAmountsFromFrozenDecision returns the buyer and supplier amounts
// settlement must use. It reads only fields frozen into the PricingDecision —
// never a live benchmark, never a re-resolved catalogue rate, never a later
// cell performance row. A path that recomputed historical amounts from a
// revalidated benchmark would be a defect; this function is the positive
// statement of what settlement is allowed to see.
func settledAmountsFromFrozenDecision(d PricingDecision) (
	buyerNanos, supplierNanos int64, evidence []string, err error,
) {
	if d.FixedPoint != nil {
		buyerNanos = d.FixedPoint.BuyerChargeNanos
		supplierNanos = d.FixedPoint.SupplierEntitlementsNanos
	} else {
		// Legacy float path: project the frozen buyer/supplier floats to nanos.
		// Still frozen — the floats are on the decision, not re-derived.
		if d.BuyerPrice <= 0 {
			return 0, 0, nil, fmt.Errorf("frozen decision has no buyer price")
		}
		buyerNanos = usdToMicros(d.BuyerPrice) * NanosPerMicro
		if d.PrimarySupplierCost.Status == pricingCostModeled {
			supplierNanos = usdToMicros(d.PrimarySupplierCost.Amount) * NanosPerMicro
		}
		if d.SupplierGrossNanos > 0 {
			// Prefer the exact nano entitlement when present even without FixedPoint.
			supplierNanos = d.SupplierGrossNanos
		}
	}
	evidence = []string{
		fmt.Sprintf("pricing_decision_supplier_entitlement_policy:%s",
			firstNonEmpty(d.SupplierEntitlementPolicy, cellEntitlementResolutionModelLevel)),
	}
	if d.RuntimeCell != nil {
		evidence = append(evidence, d.RuntimeCell.EvidenceIdentity...)
		evidence = append(evidence,
			"frozen_runtime_cell_digest:"+d.RuntimeCell.Digest,
			"cell_entitlement_resolution:"+d.RuntimeCell.EntitlementResolution,
		)
	} else {
		evidence = append(evidence, "runtime_cell:absent_legacy_decision")
	}
	if d.Catalogue.ScheduleSHA256 != "" {
		evidence = append(evidence, "catalogue_schedule:"+d.Catalogue.ScheduleSHA256)
	}
	return buyerNanos, supplierNanos, evidence, nil
}

// cellEntitlementResolutionForNewAdmission is the policy written on every new
// freeze. Legacy decisions never call freezeRuntimeCellEconomics with a nil
// block retrofit, so they stay model_catalogue_cancelled by absence.
func cellEntitlementResolutionForNewAdmission() string {
	return cellEntitlementResolutionCellResolved
}

// isLegacyModelLevelEntitlement reports whether a decision settles under the
// pre-cell-resolution form. Absence of a cell block, or an explicit model-level
// resolution, both count.
func isLegacyModelLevelEntitlement(d PricingDecision) bool {
	if d.RuntimeCell == nil {
		return true
	}
	r := strings.TrimSpace(d.RuntimeCell.EntitlementResolution)
	return r == "" || r == cellEntitlementResolutionModelLevel
}
