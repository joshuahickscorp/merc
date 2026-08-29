package main

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"reflect"
	"slices"
	"sort"
	"strings"
)

// The runtime cell's economics as ACCEPTED, frozen into the PricingDecision.
//
// CellEconomicsProjection (runtime_cell_economics.go) is a selector view: it is
// re-derivable, it moves when a benchmark is revalidated, and it is deliberately
// not a pricing authority. That is the right shape for ranking and the wrong
// shape for settlement. This type is the other half: the one projection the
// admission decision actually accepted, frozen beside the money so a later
// benchmark cannot move what was already settled.
//
// Determinism is the whole constraint. ValidateDistributedPricingDecisionSnapshot
// rebuilds the decision and compares it with reflect.DeepEqual, so every term
// here must be a function of the FROZEN inputs — placement, compute plan,
// economic plan, catalogue, the governed unit rate handed in, and the compiled
// runtime authority pinned by placement.RuntimeMatrixSHA256. Nothing here may
// re-resolve a dated receipt. That is exactly the defect class that made a
// settled figure depend on a benchmark somebody could re-run.
//
// What it is not:
//
//   - not a second pricing authority: it adds no money to the conservation
//     equation, and every dollar it names is a term the decision already froze
//     or a term it explicitly marks unknown;
//   - not true net: the named-cost sum is a partial platform cost while any
//     category is unknown, and it says so in its own status field;
//   - not a promotion, and not a routing decision.

const (
	frozenRuntimeCellEconomicsLegacyVersion = 1
	frozenRuntimeCellEconomicsVersion       = 2
	frozenRuntimeCellEconomicsKind          = "frozen_runtime_cell_economics"

	// Status vocabulary for the named-cost sum. A partial sum that calls itself
	// complete is the failure this field exists to prevent.
	frozenVOCostComplete = "MODELED_ALL_NAMED_TERMS"
	frozenVOCostPartial  = "PARTIAL_MODELED_TERMS_ONLY"
	frozenVOCostRefused  = "REFUSED_NO_MODELED_TERM"

	// Platform delivery is deliberately a narrower subtotal than the complete
	// named-cost vocabulary above. Reusing frozenVOCostComplete here caused a
	// supplier+provider+verification subtotal to claim MODELED_ALL_NAMED_TERMS
	// while reliability, storage, energy, risk, and residency were still unknown.
	platformDeliveryComplete = "MODELED_ALL_PLATFORM_DELIVERY_LEGS"
	platformDeliveryPartial  = "PARTIAL_MODELED_PLATFORM_DELIVERY_LEGS_ONLY"
	platformDeliveryRefused  = "REFUSED_NO_MODELED_PLATFORM_DELIVERY_LEG"

	frozenSupplyCommunity = "COMMUNITY_OR_OWNED"
	frozenSupplyCloud     = "CLOUD_BILLED_TO_MERC"
	frozenSupplyUnknown   = "UNKNOWN_CELL_CLASSIFICATION"
)

// FrozenProviderRateAuthority carries the exact provider-rate row visible when
// the placement was accepted. Resolved means the hardware set selected one
// complete row; AuthorityStatus still decides whether it was governed enough to
// enter canonical money. An UNBOUND/WITHDRAWN row is preserved for refusal
// provenance, never promoted to a modeled cost.
type FrozenProviderRateAuthority struct {
	Resolved        bool    `json:"resolved"`
	CostPerHr       float64 `json:"cost_per_hr,omitempty"`
	Currency        string  `json:"currency,omitempty"`
	AuthorityStatus string  `json:"authority_status,omitempty"`
	Provenance      string  `json:"provenance,omitempty"`
}

// FrozenRuntimeCellEconomics is one runtime cell's accepted economics.
type FrozenRuntimeCellEconomics struct {
	Version int    `json:"version"`
	Kind    string `json:"kind"`
	// RuntimeAuthoritySHA256 is the complete runtime document whose cell
	// classification was accepted. RuntimeMatrixSHA256 alone omits activation and
	// CloudBacked fields, so it cannot identify provider economics.
	RuntimeAuthoritySHA256 string                      `json:"runtime_authority_sha256"`
	SupplyClass            string                      `json:"supply_class"`
	ProviderRateAuthority  FrozenProviderRateAuthority `json:"provider_rate_authority"`

	// ModelContract is the catalogue identity this cell was priced under.
	// Copied from the decision's catalogue so a later schedule rewrite cannot
	// re-attribute the freeze.
	ModelContract CellEconomicsModelContract `json:"model_contract"`

	// Identity. HWClass is the single hardware class this binding could resolve;
	// empty means the accepted placement admits more than one and no per-class
	// term (energy) may be modeled for it.
	CellID    string   `json:"cell_id"`
	RuntimeID string   `json:"runtime_id"`
	Engine    string   `json:"engine"`
	HWClass   string   `json:"hw_class,omitempty"`
	HWClasses []string `json:"hw_classes,omitempty"`

	// BuildIdentity binds the receipt-measured engine build, exact device and
	// model artifact snapshot for current placements. Historical placements that
	// predate those fields retain the explicit UNKNOWN legacy shape.
	BuildIdentity EconomicsFieldProvenance `json:"build_identity"`

	// EntitlementResolution names how platform economics were resolved for this
	// cell. New admissions write cell_resolved_platform_v1. Legacy decisions
	// without this block never claim cell resolution.
	EntitlementResolution string `json:"entitlement_resolution"`

	// Accepted execution geometry. ConservativeUnitsPerSec is the governed
	// haircut rate admission priced on — never an observed peak, never today's
	// receipt.
	ConservativeUnitsPerSec float64 `json:"conservative_units_per_sec"`
	ExpectedMsPerUnit       float64 `json:"expected_ms_per_unit"`
	ExpectedSeconds         float64 `json:"expected_seconds"`
	BillableUnits           float64 `json:"billable_units"`

	// Provenance for geometric and money fields that are not PricingCostComponents.
	// Cost components already carry status+basis; these are the rest.
	ThroughputProvenance EconomicsFieldProvenance `json:"throughput_provenance"`
	DurationProvenance   EconomicsFieldProvenance `json:"duration_provenance"`
	SupplierProvenance   EconomicsFieldProvenance `json:"supplier_entitlement_provenance"`
	BuyerProvenance      EconomicsFieldProvenance `json:"buyer_price_provenance"`

	// Money as accepted. These duplicate PricingDecision fields on purpose: the
	// digest below covers them, so a rewrite of the decision that leaves this
	// block alone is detectable.
	BuyerPriceUSD             float64 `json:"buyer_price_usd"`
	SupplierEntitlementUSD    float64 `json:"supplier_entitlement_usd"`
	SupplierEntitlementPolicy string  `json:"supplier_entitlement_policy"`
	SupplierAskUSDHr          float64 `json:"supplier_ask_usd_hr"`

	// PlatformDelivery is the cell-resolved platform cost of the accepted work:
	// physical supplier + modeled provider + modeled verification. Provider is
	// duration-sensitive, so two cells (or two throughputs) on one model can
	// differ here even when supplier entitlement is identical. This is the
	// economic differentiator cell resolution exists to freeze; it does not
	// rewrite supplier settlement.
	PlatformDeliveryCostUSD        float64 `json:"platform_delivery_cost_usd"`
	PlatformDeliveryCostUSDPerUnit float64 `json:"platform_delivery_cost_usd_per_unit"`
	PlatformDeliveryCostStatus     string  `json:"platform_delivery_cost_status"`
	PlatformDeliveryCostBasis      string  `json:"platform_delivery_cost_basis"`

	// The named expected verified-outcome cost terms, in the order the directive
	// names them. Each is the decision's own component or an explicit unknown.
	//
	// PhysicalCost and ProviderCost are both "what it costs to get one physical
	// execution", split by who is paid. For owned or community supply that is the
	// supplier entitlement and provider cost is not applicable; for cloud-backed
	// supply the pod rate is real money out beside it. Summing both is what the
	// decision's own conservation equation already does — provider cost is inside
	// KnownVariableCostsNanos — so this sum stays consistent with the money.
	// Energy and risk are the directive's single "energy/risk allocation" bucket,
	// split. Combining them cost real information: risk reserve is modeled money
	// on the buyer charge and energy is usually unknown, so one bucket reported
	// UNKNOWN and a known cost vanished from the sum. Two names, two states.
	PhysicalCost     PricingCostComponent `json:"physical_supplier_cost"`
	ProviderCost     PricingCostComponent `json:"provider_cost"`
	ReliabilityCost  PricingCostComponent `json:"reliability_retry_overhead"`
	VerificationCost PricingCostComponent `json:"verification_cost"`
	StorageTransfer  PricingCostComponent `json:"storage_and_transfer"`
	EnergyPartial    PricingCostComponent `json:"energy_allocation_partial"`
	RiskAllocation   PricingCostComponent `json:"risk_allocation"`
	StartupResidency PricingCostComponent `json:"startup_residency"`

	ExpectedVOCostUSD        float64  `json:"expected_verified_outcome_cost_usd"`
	ExpectedVOCostUSDPerUnit float64  `json:"expected_verified_outcome_cost_usd_per_unit"`
	ExpectedVOCostStatus     string   `json:"expected_verified_outcome_cost_status"`
	ExpectedVOCostBasis      string   `json:"expected_verified_outcome_cost_basis"`
	UnknownCategories        []string `json:"unknown_categories,omitempty"`

	// True net stays refused while any named category is unknown. A number here
	// is only ever the known-cost contribution, and the refusal says which
	// categories blocked it.
	MercTrueNetUSD    *float64 `json:"merc_true_net_contribution_usd,omitempty"`
	MercTrueNetStatus string   `json:"merc_true_net_status"`

	// Measured energy, when the accepted placement resolves to one class.
	EnergyJoules    float64 `json:"energy_joules,omitempty"`
	EnergyKnowledge string  `json:"energy_knowledge,omitempty"`
	EnergySource    string  `json:"energy_source,omitempty"`

	Confidence       float64  `json:"confidence"`
	EvidenceIdentity []string `json:"evidence_identity"`

	// Digest over this block with Digest cleared.
	Digest string `json:"digest"`
}

// frozenRuntimeCellEconomicsV1Digest is the exact committed v1 JSON shape.
// Keep declaration order: encoding/json emits struct fields in this order and
// the historical digest was taken over those bytes. V1 predates the explicit
// runtime-document/supply/provider-rate snapshot; its already-resolved provider,
// energy, and startup components are replayed as accepted, never reconstructed
// from current authority.
type frozenRuntimeCellEconomicsV1Digest struct {
	Version                        int                        `json:"version"`
	Kind                           string                     `json:"kind"`
	ModelContract                  CellEconomicsModelContract `json:"model_contract"`
	CellID                         string                     `json:"cell_id"`
	RuntimeID                      string                     `json:"runtime_id"`
	Engine                         string                     `json:"engine"`
	HWClass                        string                     `json:"hw_class,omitempty"`
	HWClasses                      []string                   `json:"hw_classes,omitempty"`
	BuildIdentity                  EconomicsFieldProvenance   `json:"build_identity"`
	EntitlementResolution          string                     `json:"entitlement_resolution"`
	ConservativeUnitsPerSec        float64                    `json:"conservative_units_per_sec"`
	ExpectedMsPerUnit              float64                    `json:"expected_ms_per_unit"`
	ExpectedSeconds                float64                    `json:"expected_seconds"`
	BillableUnits                  float64                    `json:"billable_units"`
	ThroughputProvenance           EconomicsFieldProvenance   `json:"throughput_provenance"`
	DurationProvenance             EconomicsFieldProvenance   `json:"duration_provenance"`
	SupplierProvenance             EconomicsFieldProvenance   `json:"supplier_entitlement_provenance"`
	BuyerProvenance                EconomicsFieldProvenance   `json:"buyer_price_provenance"`
	BuyerPriceUSD                  float64                    `json:"buyer_price_usd"`
	SupplierEntitlementUSD         float64                    `json:"supplier_entitlement_usd"`
	SupplierEntitlementPolicy      string                     `json:"supplier_entitlement_policy"`
	SupplierAskUSDHr               float64                    `json:"supplier_ask_usd_hr"`
	PlatformDeliveryCostUSD        float64                    `json:"platform_delivery_cost_usd"`
	PlatformDeliveryCostUSDPerUnit float64                    `json:"platform_delivery_cost_usd_per_unit"`
	PlatformDeliveryCostStatus     string                     `json:"platform_delivery_cost_status"`
	PlatformDeliveryCostBasis      string                     `json:"platform_delivery_cost_basis"`
	PhysicalCost                   PricingCostComponent       `json:"physical_supplier_cost"`
	ProviderCost                   PricingCostComponent       `json:"provider_cost"`
	ReliabilityCost                PricingCostComponent       `json:"reliability_retry_overhead"`
	VerificationCost               PricingCostComponent       `json:"verification_cost"`
	StorageTransfer                PricingCostComponent       `json:"storage_and_transfer"`
	EnergyPartial                  PricingCostComponent       `json:"energy_allocation_partial"`
	RiskAllocation                 PricingCostComponent       `json:"risk_allocation"`
	StartupResidency               PricingCostComponent       `json:"startup_residency"`
	ExpectedVOCostUSD              float64                    `json:"expected_verified_outcome_cost_usd"`
	ExpectedVOCostUSDPerUnit       float64                    `json:"expected_verified_outcome_cost_usd_per_unit"`
	ExpectedVOCostStatus           string                     `json:"expected_verified_outcome_cost_status"`
	ExpectedVOCostBasis            string                     `json:"expected_verified_outcome_cost_basis"`
	UnknownCategories              []string                   `json:"unknown_categories,omitempty"`
	MercTrueNetUSD                 *float64                   `json:"merc_true_net_contribution_usd,omitempty"`
	MercTrueNetStatus              string                     `json:"merc_true_net_status"`
	EnergyJoules                   float64                    `json:"energy_joules,omitempty"`
	EnergyKnowledge                string                     `json:"energy_knowledge,omitempty"`
	EnergySource                   string                     `json:"energy_source,omitempty"`
	Confidence                     float64                    `json:"confidence"`
	EvidenceIdentity               []string                   `json:"evidence_identity"`
	Digest                         string                     `json:"digest"`
}

// freezeRuntimeCellEconomics builds the accepted binding from frozen inputs.
//
// Every argument is already frozen in the decision being built. New admission
// resolves cell classification once; historical replay passes the accepted
// snapshot back through the same pure derivation and never consults today's
// runtime document. A matrix digest is an identity, not access to old matrix
// content, so re-resolving a former cell from the current document would rewrite
// accepted provider, energy, and startup/residency economics.
// MercTrueNetAvailable reports whether every named cost category resolved, so a
// true net contribution may be published. False means at least one category is
// unknown and the amount must be refused rather than approximated: with no known
// variable costs subtracted, an "approximation" is just the gross spread wearing
// the name of profit.
func (c *FrozenRuntimeCellEconomics) MercTrueNetAvailable() bool {
	return c != nil && len(c.UnknownCategories) == 0 && c.MercTrueNetUSD != nil
}

func freezeRuntimeCellEconomics(
	placement PlacementRequirement,
	catalogue CataloguePriceAuthority,
	unitsPerSec float64,
	billableUnits float64,
	expectedSeconds float64,
	buyerPriceUSD float64,
	supplierEntitlementUSD float64,
	supplierEntitlementPolicy string,
	supplierAskUSDHr float64,
	physical PricingCostComponent,
	provider PricingCostComponent,
	verification PricingCostComponent,
	storage PricingCostComponent,
	egress PricingCostComponent,
	risk PricingCostComponent,
	contributionUSD float64,
	unknowns []string,
	confidence float64,
) (*FrozenRuntimeCellEconomics, error) {
	return freezeRuntimeCellEconomicsWithSnapshot(
		placement, catalogue, unitsPerSec, billableUnits, expectedSeconds,
		buyerPriceUSD, supplierEntitlementUSD, supplierEntitlementPolicy,
		supplierAskUSDHr, physical, provider, verification, storage, egress,
		risk, contributionUSD, unknowns, confidence, nil,
	)
}

// freezeRuntimeCellEconomicsWithSnapshot is also the historical reconstruction
// path. snapshot contributes only facts that cannot be derived without the old
// runtime document (accepted hardware classification, energy posture, and
// startup/residency classification). Every geometry and money field is rebuilt
// from the other frozen authorities and compared by
// validateFrozenRuntimeCellEconomics.
func freezeRuntimeCellEconomicsWithSnapshot(
	placement PlacementRequirement,
	catalogue CataloguePriceAuthority,
	unitsPerSec float64,
	billableUnits float64,
	expectedSeconds float64,
	buyerPriceUSD float64,
	supplierEntitlementUSD float64,
	supplierEntitlementPolicy string,
	supplierAskUSDHr float64,
	physical PricingCostComponent,
	provider PricingCostComponent,
	verification PricingCostComponent,
	storage PricingCostComponent,
	egress PricingCostComponent,
	risk PricingCostComponent,
	contributionUSD float64,
	unknowns []string,
	confidence float64,
	snapshot *FrozenRuntimeCellEconomics,
) (*FrozenRuntimeCellEconomics, error) {
	policy := firstNonEmpty(supplierEntitlementPolicy, supplierEntitlementPolicyCancelled)
	out := &FrozenRuntimeCellEconomics{
		Version:                   frozenRuntimeCellEconomicsVersion,
		Kind:                      frozenRuntimeCellEconomicsKind,
		ModelContract:             modelContractFromCatalogue(catalogue),
		CellID:                    placement.RuntimeCellID,
		RuntimeID:                 placement.RuntimeID,
		Engine:                    placement.Engine,
		BuildIdentity:             buildIdentityProvenance(placement),
		EntitlementResolution:     cellEntitlementResolutionForNewAdmission(),
		ConservativeUnitsPerSec:   unitsPerSec,
		ExpectedSeconds:           expectedSeconds,
		BillableUnits:             billableUnits,
		BuyerPriceUSD:             buyerPriceUSD,
		SupplierEntitlementUSD:    supplierEntitlementUSD,
		SupplierEntitlementPolicy: policy,
		SupplierAskUSDHr:          supplierAskUSDHr,
		PhysicalCost:              physical,
		ProviderCost:              provider,
		VerificationCost:          verification,
		Confidence:                confidence,
		ThroughputProvenance:      throughputProvenance(unitsPerSec),
		DurationProvenance:        durationProvenance(expectedSeconds, unitsPerSec, billableUnits),
		SupplierProvenance:        supplierEntitlementProvenance(policy, supplierEntitlementUSD),
		BuyerProvenance:           buyerPriceProvenance(buyerPriceUSD),
	}
	if snapshot != nil {
		out.Version = snapshot.Version
		out.RuntimeAuthoritySHA256 = snapshot.RuntimeAuthoritySHA256
		out.SupplyClass = snapshot.SupplyClass
		out.ProviderRateAuthority = snapshot.ProviderRateAuthority
	} else {
		out.RuntimeAuthoritySHA256 = generatedRuntimeAuthorityFileSHA256
		out.SupplyClass, out.ProviderRateAuthority =
			freezeCurrentRuntimeCellSupplyAuthority(placement.RuntimeCellID, placement.HWClasses)
	}
	if unitsPerSec > 0 {
		out.ExpectedMsPerUnit = 1000.0 / unitsPerSec
	}
	if snapshot != nil {
		out.HWClasses = append([]string(nil), snapshot.HWClasses...)
		out.HWClass = snapshot.HWClass
	} else {
		if len(placement.HWClasses) > 0 {
			out.HWClasses = append([]string(nil), placement.HWClasses...)
		}
		out.HWClass = acceptedHWClassForCell(placement.RuntimeCellID, placement.HWClasses)
		if out.HWClass == "" && placement.Version == placementRequirementVersionMultiFamily &&
			placement.PerformanceAuthority != nil {
			// Energy/catalogue still pin the preferred measured class. The
			// placement union is claim eligibility, not interchangeable watts.
			measured := strings.TrimSpace(placement.PerformanceAuthority.Performance.MeasuredOnHWClass)
			if slices.Contains(placement.HWClasses, measured) {
				out.HWClass = measured
			}
		}
	}
	if snapshot != nil && snapshot.Version == frozenRuntimeCellEconomicsLegacyVersion {
		if !reflect.DeepEqual(provider, snapshot.ProviderCost) {
			return nil, fmt.Errorf("legacy provider cost conflicts with accepted v1 snapshot")
		}
	} else {
		derivedProvider, err := providerCostFromFrozenRuntimeCellAuthority(out, expectedSeconds, catalogue)
		if err != nil {
			return nil, err
		}
		if !reflect.DeepEqual(provider, derivedProvider) {
			return nil, fmt.Errorf(
				"provider cost does not match frozen runtime-cell supply authority: decision=%+v derived=%+v",
				provider, derivedProvider)
		}
	}

	// Storage and transfer: one term, because the directive names one. Two
	// modeled components sum; any unknown makes the pair unknown rather than
	// silently reporting half of it.
	out.StorageTransfer = combineCostComponents(
		"storage and result transfer",
		[]PricingCostComponent{storage, egress},
	)

	// Energy: watts × accepted duration × the defaulted electricity rate, and
	// ASSUMED-grade whenever the watts constant is assumed. Risk reserve is real
	// policy money on the buyer charge and stands on its own — see the field
	// comment for why these are not one bucket.
	if snapshot != nil {
		out.EnergyPartial = snapshot.EnergyPartial
		out.EnergyJoules = snapshot.EnergyJoules
		out.EnergyKnowledge = snapshot.EnergyKnowledge
		out.EnergySource = snapshot.EnergySource
	} else {
		var err error
		out.EnergyPartial, out.EnergyJoules, out.EnergyKnowledge, out.EnergySource, err =
			frozenEnergyComponentFromCatalogue(
				catalogue.PhysicalAuthority, out.HWClass, expectedSeconds)
		if err != nil {
			return nil, fmt.Errorf("freeze catalogue power authority: %w", err)
		}
	}
	out.RiskAllocation = risk

	// Reliability / retry overhead. Nothing in the frozen inputs carries a
	// measured retry rate or attempt-resource attribution for this cell. Failed
	// and verification-rejected attempts receive no supplier settlement, so they
	// must not be turned into a second supplier entitlement. They can still burn
	// cloud/provider seconds, energy, verification capacity, and latency; those
	// platform burdens are unknown rather than zero. Redundancy and honeypot
	// tasks are already priced as verification and are not retries.
	out.ReliabilityCost = unknownCost(
		"no measured retry rate for cell " + placement.RuntimeCellID + " is bound into admission; " +
			"failed or rejected attempts are unpaid, but their provider, energy, verification, and latency burden is not attributed; " +
			"a zero platform overhead would be a claim, not a measurement")

	// Startup and residency.
	if snapshot != nil && snapshot.Version == frozenRuntimeCellEconomicsLegacyVersion {
		out.StartupResidency = snapshot.StartupResidency
	} else {
		out.StartupResidency = frozenStartupResidencyForSupplyClass(out.SupplyClass, placement.RuntimeCellID)
	}

	// Cell-resolved platform delivery: supplier (cancelled) + provider (duration
	// sensitive) + verification. This is the figure that can differ across cells
	// of one model without rewriting supplier settlement.
	delivery := resolveCellPlatformDelivery(
		supplierEntitlementUSD, provider, verification, billableUnits,
		out.EntitlementResolution,
	)
	out.PlatformDeliveryCostUSD = delivery.TotalUSD
	out.PlatformDeliveryCostUSDPerUnit = delivery.PerUnitUSD
	out.PlatformDeliveryCostStatus = delivery.Status
	out.PlatformDeliveryCostBasis = delivery.Basis

	// The named sum, over modeled terms only, with its status saying so.
	out.sumNamedVerifiedOutcomeCost()

	// True net: the known-cost contribution is only true net when nothing named
	// is unknown.
	//
	// Both unknown sets count. The decision's own unknowns (storage, egress,
	// provider, risk) are one half; this block's named terms are the other, and
	// they are not the same set — reliability/retry is unknown here and appears
	// nowhere on the decision. Taking only the decision's set published a true
	// net figure while a named category on the very block asserting it was
	// UNKNOWN, which is the exact refusal this type exists to make.
	out.UnknownCategories = append(out.UnknownCategories, unknowns...)
	out.UnknownCategories = append(out.UnknownCategories, out.unknownNamedTerms()...)
	sort.Strings(out.UnknownCategories)
	out.UnknownCategories = dedupeStrings(out.UnknownCategories)
	if len(out.UnknownCategories) == 0 {
		v := contributionUSD
		out.MercTrueNetUSD = &v
		out.MercTrueNetStatus = "TRUE_NET_AVAILABLE_ALL_NAMED_COSTS_MODELED_OR_NOT_APPLICABLE"
	} else {
		out.MercTrueNetStatus = "REFUSED_UNKNOWN_COST_CATEGORIES: " +
			strings.Join(out.UnknownCategories, ", ")
	}

	out.EvidenceIdentity = frozenCellEvidenceIdentity(placement, catalogue, out)

	digest, err := digestFrozenRuntimeCellEconomics(out)
	if err != nil {
		return nil, err
	}
	out.Digest = digest
	return out, nil
}

// validateFrozenRuntimeCellEconomics proves that the accepted block is both
// digest-intact and a deterministic projection of the other frozen decision
// authorities. It refuses blocks whose hardware classification was never
// serialized; replay must not fill that gap from today's runtime document.
func validateFrozenRuntimeCellEconomics(
	snapshot *FrozenRuntimeCellEconomics,
	placement PlacementRequirement,
	catalogue CataloguePriceAuthority,
	unitsPerSec float64,
	billableUnits float64,
	expectedSeconds float64,
	buyerPriceUSD float64,
	supplierEntitlementUSD float64,
	supplierEntitlementPolicy string,
	supplierAskUSDHr float64,
	physical PricingCostComponent,
	provider PricingCostComponent,
	verification PricingCostComponent,
	storage PricingCostComponent,
	egress PricingCostComponent,
	risk PricingCostComponent,
	contributionUSD float64,
	unknowns []string,
	confidence float64,
) error {
	if snapshot == nil {
		return fmt.Errorf("frozen runtime-cell economics snapshot is absent")
	}
	if (snapshot.Version != frozenRuntimeCellEconomicsLegacyVersion &&
		snapshot.Version != frozenRuntimeCellEconomicsVersion) ||
		snapshot.Kind != frozenRuntimeCellEconomicsKind {
		return fmt.Errorf("unsupported frozen runtime-cell economics %d/%q",
			snapshot.Version, snapshot.Kind)
	}
	if placement.Version < 2 && snapshot.Version >= frozenRuntimeCellEconomicsVersion {
		return fmt.Errorf(
			"placement version %d cannot carry frozen runtime-cell economics version %d; enriched economics requires placement-v2 frozen performance and hardware authority",
			placement.Version, snapshot.Version)
	}
	if !validSHA256(snapshot.Digest) {
		return fmt.Errorf("frozen runtime-cell economics digest is invalid")
	}
	if snapshot.Version >= frozenRuntimeCellEconomicsVersion {
		if !validSHA256(snapshot.RuntimeAuthoritySHA256) {
			return fmt.Errorf("frozen runtime-cell economics lacks a runtime authority document digest")
		}
		switch snapshot.SupplyClass {
		case frozenSupplyCommunity, frozenSupplyCloud, frozenSupplyUnknown:
		default:
			return fmt.Errorf("frozen runtime-cell economics has unsupported supply class %q",
				snapshot.SupplyClass)
		}
	} else if snapshot.RuntimeAuthoritySHA256 != "" || snapshot.SupplyClass != "" ||
		snapshot.ProviderRateAuthority != (FrozenProviderRateAuthority{}) {
		return fmt.Errorf("legacy frozen runtime-cell economics carries future v2 authority fields")
	}
	wantDigest, err := digestFrozenRuntimeCellEconomics(snapshot)
	if err != nil || wantDigest != snapshot.Digest {
		return fmt.Errorf("frozen runtime-cell economics digest mismatch")
	}
	if snapshot.Version == frozenRuntimeCellEconomicsLegacyVersion {
		// V1 stored the resolved provider/energy/startup components but not the
		// runtime-document and raw provider-rate inputs needed to re-derive them.
		// Validate every stable cross-authority field and the original byte-shape
		// digest, then retain those accepted resolved components. Consulting
		// current authority here would be a history rewrite; retrofitting v2 fields
		// would be invented evidence.
		storageTransfer := combineCostComponents(
			"storage and result transfer", []PricingCostComponent{storage, egress})
		delivery := resolveCellPlatformDelivery(
			supplierEntitlementUSD, provider, verification, billableUnits,
			snapshot.EntitlementResolution,
		)
		namedTotal := 0.0
		for _, term := range snapshot.namedCostTerms() {
			if term.Component.Status == pricingCostModeled {
				namedTotal += term.Component.Amount
			}
		}
		namedTotal = roundEconomicUSD(namedTotal)
		namedPerUnit := 0.0
		if billableUnits > 0 {
			namedPerUnit = namedTotal / billableUnits
		}
		modeledNamed, unknownNamed := 0, 0
		for _, term := range snapshot.namedCostTerms() {
			switch term.Component.Status {
			case pricingCostModeled:
				modeledNamed++
			case pricingCostUnknown:
				unknownNamed++
			}
		}
		namedStatus := frozenVOCostPartial
		switch {
		case modeledNamed == 0:
			namedStatus = frozenVOCostRefused
		case unknownNamed == 0:
			namedStatus = frozenVOCostComplete
		}
		wantUnknowns := append([]string(nil), unknowns...)
		wantUnknowns = append(wantUnknowns, snapshot.unknownNamedTerms()...)
		sort.Strings(wantUnknowns)
		wantUnknowns = dedupeStrings(wantUnknowns)
		expectedMsPerUnit := 0.0
		if unitsPerSec > 0 {
			expectedMsPerUnit = 1000.0 / unitsPerSec
		}
		trueNetMatches := snapshot.MercTrueNetUSD == nil
		if len(wantUnknowns) == 0 {
			trueNetMatches = snapshot.MercTrueNetUSD != nil &&
				*snapshot.MercTrueNetUSD == contributionUSD
		}
		legacyDeliveryStatus := frozenVOCostPartial
		switch delivery.Status {
		case platformDeliveryRefused:
			legacyDeliveryStatus = frozenVOCostRefused
		case platformDeliveryComplete:
			legacyDeliveryStatus = frozenVOCostComplete
		}
		wantEvidence := frozenCellEvidenceIdentity(placement, catalogue, snapshot)
		if !reflect.DeepEqual(snapshot.ModelContract, modelContractFromCatalogue(catalogue)) ||
			snapshot.CellID != placement.RuntimeCellID ||
			snapshot.RuntimeID != placement.RuntimeID ||
			snapshot.Engine != placement.Engine ||
			!slices.Equal(snapshot.HWClasses, placement.HWClasses) ||
			(len(placement.HWClasses) == 1 &&
				snapshot.HWClass != strings.TrimSpace(placement.HWClasses[0])) ||
			!reflect.DeepEqual(snapshot.BuildIdentity, buildIdentityProvenance(placement)) ||
			snapshot.EntitlementResolution != cellEntitlementResolutionCellResolved ||
			snapshot.ConservativeUnitsPerSec != unitsPerSec ||
			snapshot.ExpectedMsPerUnit != expectedMsPerUnit ||
			snapshot.ExpectedSeconds != expectedSeconds ||
			snapshot.BillableUnits != billableUnits ||
			snapshot.BuyerPriceUSD != buyerPriceUSD ||
			snapshot.SupplierEntitlementUSD != supplierEntitlementUSD ||
			snapshot.SupplierEntitlementPolicy != firstNonEmpty(
				supplierEntitlementPolicy, supplierEntitlementPolicyCancelled) ||
			snapshot.SupplierAskUSDHr != supplierAskUSDHr ||
			!reflect.DeepEqual(snapshot.PhysicalCost, physical) ||
			!reflect.DeepEqual(snapshot.ProviderCost, provider) ||
			!reflect.DeepEqual(snapshot.VerificationCost, verification) ||
			!reflect.DeepEqual(snapshot.StorageTransfer, storageTransfer) ||
			!reflect.DeepEqual(snapshot.RiskAllocation, risk) ||
			snapshot.PlatformDeliveryCostUSD != delivery.TotalUSD ||
			snapshot.PlatformDeliveryCostUSDPerUnit != delivery.PerUnitUSD ||
			snapshot.PlatformDeliveryCostStatus != legacyDeliveryStatus ||
			snapshot.ExpectedVOCostUSD != namedTotal ||
			snapshot.ExpectedVOCostUSDPerUnit != namedPerUnit ||
			snapshot.ExpectedVOCostStatus != namedStatus ||
			!slices.Equal(snapshot.UnknownCategories, wantUnknowns) ||
			!trueNetMatches ||
			snapshot.Confidence != confidence ||
			!slices.Equal(snapshot.EvidenceIdentity, wantEvidence) {
			return fmt.Errorf(
				"legacy frozen runtime-cell economics conflicts with its accepted placement or pricing authority")
		}
		return nil
	}
	if placement.Version == placementRequirementVersionMultiFamily {
		if placement.PerformanceAuthority == nil {
			return fmt.Errorf("frozen runtime-cell economics lacks multi-family performance authority")
		}
		measured := strings.TrimSpace(placement.PerformanceAuthority.Performance.MeasuredOnHWClass)
		if !slices.Equal(snapshot.HWClasses, placement.HWClasses) ||
			snapshot.HWClass != measured ||
			!slices.Contains(placement.HWClasses, measured) {
			return fmt.Errorf("frozen runtime-cell economics hardware classification conflicts with multi-family placement")
		}
	} else if len(placement.HWClasses) != 1 || len(snapshot.HWClasses) != 1 {
		return fmt.Errorf(
			"frozen runtime-cell economics lacks one serialized accepted hardware class; current runtime authority cannot repair historical classification")
	} else if !slices.Equal(snapshot.HWClasses, placement.HWClasses) ||
		snapshot.HWClass != strings.TrimSpace(placement.HWClasses[0]) {
		return fmt.Errorf("frozen runtime-cell economics hardware classification conflicts with placement")
	}
	rebuilt, err := freezeRuntimeCellEconomicsWithSnapshot(
		placement, catalogue, unitsPerSec, billableUnits, expectedSeconds,
		buyerPriceUSD, supplierEntitlementUSD, supplierEntitlementPolicy,
		supplierAskUSDHr, physical, provider, verification, storage, egress,
		risk, contributionUSD, unknowns, confidence, snapshot,
	)
	if err != nil {
		return err
	}
	if !reflect.DeepEqual(snapshot, rebuilt) {
		return fmt.Errorf(
			"frozen runtime-cell economics does not match its self-contained accepted authorities")
	}
	return nil
}

type namedCostTerm struct {
	Name      string
	Component PricingCostComponent
}

// namedCostTerms is the directive's cost decomposition, in its own order.
//
// A SLICE, not a map, and that is load-bearing. Summing floats over a Go map
// adds them in a randomised order, so two builds of the same decision produced
// per-unit figures differing in the last ulp — 0.0013474296874999999 against
// 0.0013474296875 — and the deterministic-rebuild validator refused a decision
// that was in every other respect identical to itself. A money figure that does
// not reproduce is not a money figure. One ordered list, so the sum, the
// unknown set and any future reader cannot drift apart either.
func (f *FrozenRuntimeCellEconomics) namedCostTerms() []namedCostTerm {
	return []namedCostTerm{
		{"physical_supplier_cost", f.PhysicalCost},
		{"provider_cost", f.ProviderCost},
		{"reliability_retry_overhead", f.ReliabilityCost},
		{"verification_cost", f.VerificationCost},
		{"storage_and_transfer", f.StorageTransfer},
		{"energy_allocation_partial", f.EnergyPartial},
		{"risk_allocation", f.RiskAllocation},
		{"startup_residency", f.StartupResidency},
	}
}

// unknownNamedTerms names every term this block could not model.
func (f *FrozenRuntimeCellEconomics) unknownNamedTerms() []string {
	var out []string
	for _, t := range f.namedCostTerms() {
		if t.Component.Status == pricingCostUnknown {
			out = append(out, t.Name)
		}
	}
	sort.Strings(out)
	return out
}

// sumNamedVerifiedOutcomeCost adds the modeled named terms and records how
// complete that sum is. Unknown terms never contribute zero.
func (f *FrozenRuntimeCellEconomics) sumNamedVerifiedOutcomeCost() {
	total := 0.0
	modeled, unknown := 0, 0
	for _, t := range f.namedCostTerms() {
		switch t.Component.Status {
		case pricingCostModeled:
			total += t.Component.Amount
			modeled++
		case pricingCostUnknown:
			unknown++
		}
	}
	f.ExpectedVOCostUSD = roundEconomicUSD(total)
	// Per unit is derived from the PUBLISHED total, not from the raw accumulator.
	// A reader dividing the figure this block states must get the figure this
	// block states.
	if f.BillableUnits > 0 {
		f.ExpectedVOCostUSDPerUnit = f.ExpectedVOCostUSD / f.BillableUnits
	}
	switch {
	case modeled == 0:
		f.ExpectedVOCostStatus = frozenVOCostRefused
		f.ExpectedVOCostUSD = 0
		f.ExpectedVOCostUSDPerUnit = 0
	case unknown == 0:
		f.ExpectedVOCostStatus = frozenVOCostComplete
	default:
		f.ExpectedVOCostStatus = frozenVOCostPartial
	}
	f.ExpectedVOCostBasis = "physical/provider + reliability/retry + verification + " +
		"storage/transfer + energy + risk allocation + startup/residency, over MODELED " +
		"terms only; an unknown term is excluded from the sum and named in " +
		"unknown_categories, never added as zero"
}

// acceptedHWClassForCell resolves the single hardware class this placement can
// be charged energy against, or "" when it admits more than one.
//
// The placement's frozen set is the only accepted one. A missing set is unknown:
// consulting the cell's currently compiled platforms would rewrite historical
// energy classification after a runtime-authority revision.
func acceptedHWClassForCell(cellID string, frozen []string) string {
	_ = cellID // retained in the signature for clear call-site semantics
	if len(frozen) != 1 {
		return ""
	}
	return strings.TrimSpace(frozen[0])
}

func freezeCurrentRuntimeCellSupplyAuthority(
	cellID string,
	hwClasses []string,
) (string, FrozenProviderRateAuthority) {
	cloud, found := cellIsCloudBacked(cellID)
	if !found {
		return frozenSupplyUnknown, FrozenProviderRateAuthority{}
	}
	if !cloud {
		return frozenSupplyCommunity, FrozenProviderRateAuthority{}
	}
	rate, resolved := resolveProviderRateUSDHr(cellID, hwClasses)
	if !resolved {
		return frozenSupplyCloud, FrozenProviderRateAuthority{}
	}
	return frozenSupplyCloud, FrozenProviderRateAuthority{
		Resolved:        true,
		CostPerHr:       rate.CostPerHrUSD,
		Currency:        rate.Currency,
		AuthorityStatus: rate.AuthorityStatus,
		Provenance:      rate.Provenance,
	}
}

func providerCostFromFrozenRuntimeCellAuthority(
	frozen *FrozenRuntimeCellEconomics,
	expectedSeconds float64,
	catalogue CataloguePriceAuthority,
) (PricingCostComponent, error) {
	if frozen == nil {
		return PricingCostComponent{}, fmt.Errorf("provider cost lacks frozen runtime-cell authority")
	}
	switch frozen.SupplyClass {
	case frozenSupplyCommunity:
		if frozen.ProviderRateAuthority.Resolved {
			return PricingCostComponent{}, fmt.Errorf(
				"community/owned supply carries a cloud provider-rate authority")
		}
		return notApplicableCost(
			"Merc's provider cost is not applicable for owned or community supply: " +
				"Merc pays the supplier entitlement, and the supplier's energy and " +
				"depreciation are the supplier's cost, not Merc's (cell " + frozen.CellID + ")"), nil
	case frozenSupplyUnknown:
		if frozen.ProviderRateAuthority.Resolved {
			return PricingCostComponent{}, fmt.Errorf(
				"unknown cell classification carries a resolved provider-rate authority")
		}
		return unknownCost(
			"runtime cell " + frozen.CellID + " is not in the runtime-cell authority; " +
				"provider cost cannot be classified"), nil
	case frozenSupplyCloud:
		if !frozen.ProviderRateAuthority.Resolved {
			return unknownCost(
				"cloud-backed cell " + frozen.CellID + " has no resolvable governed provider rate " +
					"for its hardware class set; true net is blocked until the rate is " +
					"bound to the same authority ops/scripts/runpod-spend-guard.py uses"), nil
		}
	default:
		return PricingCostComponent{}, fmt.Errorf(
			"unsupported frozen runtime-cell supply class %q", frozen.SupplyClass)
	}

	rateAuthority := frozen.ProviderRateAuthority
	if !finiteNonNegative(rateAuthority.CostPerHr) || rateAuthority.CostPerHr <= 0 ||
		strings.TrimSpace(rateAuthority.Currency) == "" ||
		strings.TrimSpace(rateAuthority.AuthorityStatus) == "" ||
		strings.TrimSpace(rateAuthority.Provenance) == "" {
		return PricingCostComponent{}, fmt.Errorf("frozen provider-rate authority is incomplete")
	}
	rate := governedProviderRate{
		CostPerHrUSD:    rateAuthority.CostPerHr,
		Currency:        rateAuthority.Currency,
		AuthorityStatus: rateAuthority.AuthorityStatus,
		Provenance:      rateAuthority.Provenance,
	}
	ratePerHr, currencyBasis, err := providerRateInSettlementCurrency(rate, catalogue)
	if err != nil {
		return unknownCost(
			"cloud-backed cell " + frozen.CellID +
				" provider rate cannot enter canonical pricing: " + err.Error()), nil
	}
	nanos, err := providerCostNanos(ratePerHr, expectedSeconds)
	if err != nil {
		return unknownCost(
			"cloud-backed cell " + frozen.CellID +
				" provider cost could not be modeled: " + err.Error()), nil
	}
	return modeledCost(nanosToEconomicUSD(nanos),
		fmt.Sprintf("governed cloud provider rate %.6f %s/hr × %.6fs expected; %s; %s",
			ratePerHr, catalogue.SettlementCurrency, expectedSeconds,
			currencyBasis, rate.Provenance)), nil
}

func frozenStartupResidencyForSupplyClass(
	supplyClass string,
	cellID string,
) PricingCostComponent {
	switch supplyClass {
	case frozenSupplyCommunity:
		return notApplicableCost(
			"owned or community supply: the worker is already resident and its startup and " +
				"idle time are the supplier's cost, not Merc's (cell " + cellID + ")")
	case frozenSupplyCloud:
		return unknownCost(
			"cloud-backed supply: governed provider cost covers benchmark-derived execution seconds only; " +
				"no frozen pod-startup, model-load, minimum-billing, or idle-residency duration is attributed " +
				"to cell " + cellID + ", so treating those seconds as zero would understate provider cost")
	default:
		return unknownCost(
			"runtime cell " + cellID + " is not in the runtime-cell authority; " +
				"startup and residency cannot be classified")
	}
}

// cellPlatformsFromAuthority reads the compiled runtime authority for the cell's
// hardware platforms. Same source and same pinning as cellIsCloudBacked.
func cellPlatformsFromAuthority(cellID string) []string {
	cellID = strings.TrimSpace(cellID)
	if cellID == "" {
		return nil
	}
	for _, profile := range runtimeAuthority.Runtimes {
		for _, cell := range profile.Cells {
			if cell.ID == cellID {
				return append([]string(nil), profile.Hardware.Platforms...)
			}
		}
	}
	return nil
}

// frozenEnergyJoules is watts × accepted seconds for the whole accepted job.
func frozenEnergyJoules(hwClass string, expectedSeconds float64) float64 {
	entry, ok := sustainedWattsByHWClass[hwClass]
	if !ok || entry.Watts() <= 0 || expectedSeconds <= 0 {
		return 0
	}
	return entry.Watts() * expectedSeconds
}

// frozenEnergyComponent models energy for the accepted job at the accepted
// duration. Unknown without a single hardware class or a positive duration.
func frozenEnergyComponent(hwClass string, msPerUnit, billableUnits float64) PricingCostComponent {
	if strings.TrimSpace(hwClass) == "" {
		return unknownCost(
			"accepted placement admits more than one hardware class (or none), so no " +
				"per-class sustained-watts constant applies to this job's energy")
	}
	entry, ok := sustainedWattsByHWClass[hwClass]
	if !ok || entry.Watts() <= 0 {
		return unknownCost("hardware class " + hwClass + " has no governed sustained-watts entry")
	}
	if msPerUnit <= 0 || billableUnits <= 0 {
		return unknownCost("accepted duration is not positive, so energy cannot be modeled as watts × seconds")
	}
	seconds := msPerUnit / 1000.0 * billableUnits
	joules := entry.Watts() * seconds
	usd := joules / 3.6e6 * defaultElectricityUSDPerKWh
	if math.IsNaN(usd) || math.IsInf(usd, 0) {
		return unknownCost("energy model produced a non-finite figure")
	}
	if err := acceptEnergyMeasurement(entry); err != nil {
		return unknownCost(fmt.Sprintf(
			"%.1f W is %s, not MEASURED energy (%s); over %.6fs it would imply %.6f J and $%.9f at the default $%.2f/kWh, but %s / %s cannot enter canonical platform cost or true net: %v",
			entry.Watts(), entry.Kind(), entry.Provenance(), seconds, joules, usd,
			defaultElectricityUSDPerKWh, energyMeasurementAuthority, measuredEnergyEvidenceKind, err))
	}
	return modeledCost(roundEconomicUSD(usd), fmt.Sprintf(
		"%.1f W (%s) × %.6fs accepted duration × $%.2f/kWh = %.6f J; "+
			"electricity is a policy default, so this is a defaulted-grade figure, not a metered invoice; %s",
		entry.Watts(), entry.Kind(), seconds, defaultElectricityUSDPerKWh, joules,
		entry.Provenance()))
}

// frozenEnergyComponentFromCatalogue uses the exact power receipt retained by
// the selected schedule result. Reading sustainedWattsByHWClass here would let
// a later global table edit rewrite accepted energy, or let a different valid
// power receipt for the same coarse class authorize this price.
func frozenEnergyComponentFromCatalogue(
	physical CatalogueResultPhysicalAuthority,
	hwClass string,
	expectedSeconds float64,
) (PricingCostComponent, float64, string, string, error) {
	if physical.Version != catalogueResultPhysicalAuthorityVersion ||
		physical.HWClass != hwClass {
		return PricingCostComponent{}, 0, "", "", errors.New(
			"catalogue physical authority does not match accepted hardware class")
	}
	if _, err := validateCatalogueResultPhysicalAuthority(RepriceResult{
		ModelID: physical.ModelID, JobType: physical.JobType, PhysicalAuthority: physical,
	}); err != nil {
		return PricingCostComponent{}, 0, "", "", err
	}
	power := physical.Power
	if power.SourceClass == string(wattKindVendorWallUpperBound) ||
		power.SourceClass == string(wattKindAssumed) {
		source := fmt.Sprintf("%s@sha256:%s", power.Citation, power.ReceiptSHA256)
		return unknownCost(fmt.Sprintf(
				"%.1f W is %s, not MEASURED energy (%s); %s / %s requires MEASURED energy and this envelope cannot enter canonical platform cost or true net",
				power.Watts, power.SourceClass, source,
				energyMeasurementAuthority, measuredEnergyEvidenceKind)),
			0, power.SourceClass, source, nil
	}
	if expectedSeconds <= 0 || math.IsNaN(expectedSeconds) || math.IsInf(expectedSeconds, 0) {
		return PricingCostComponent{}, 0, "", "", errors.New(
			"accepted duration is not positive for frozen catalogue power")
	}
	joules := power.Watts * expectedSeconds
	usd := joules / 3.6e6 * defaultElectricityUSDPerKWh
	if math.IsNaN(usd) || math.IsInf(usd, 0) {
		return PricingCostComponent{}, 0, "", "", errors.New(
			"catalogue power energy model produced a non-finite figure")
	}
	source := fmt.Sprintf("%s@sha256:%s + src/control/pricing.go:defaultElectricityUSDPerKWh",
		power.Citation, power.ReceiptSHA256)
	basis := fmt.Sprintf(
		"%.1f W (MEASURED; %s) × %.6fs accepted duration × $%.2f/kWh = %.6f J; "+
			"electricity is a policy default, so this is a defaulted-grade figure, not a metered invoice",
		power.Watts, source, expectedSeconds, defaultElectricityUSDPerKWh, joules)
	return modeledCost(roundEconomicUSD(usd), basis), joules,
		string(wattKindMeasured), source, nil
}

// frozenStartupResidencyComponent states who pays for having the cell resident.
//
// For owned or community supply the worker's startup and idle time are the
// supplier's cost, exactly as provider cost is not applicable there. For
// cloud-backed supply, the currently frozen provider component covers only
// benchmark-derived execution seconds. No accepted field carries pod boot,
// model load, minimum billing, or idle residency seconds, so those costs remain
// unknown rather than being silently folded into execution duration.
func frozenStartupResidencyComponent(cellID string) PricingCostComponent {
	cloud, found := cellIsCloudBacked(cellID)
	if !found {
		return unknownCost(
			"runtime cell " + cellID + " is not in the runtime-cell authority; " +
				"startup and residency cannot be classified")
	}
	if !cloud {
		return notApplicableCost(
			"owned or community supply: the worker is already resident and its startup and " +
				"idle time are the supplier's cost, not Merc's (cell " + cellID + ")")
	}
	return unknownCost(
		"cloud-backed supply: governed provider cost covers benchmark-derived execution seconds only; " +
			"no frozen pod-startup, model-load, minimum-billing, or idle-residency duration is attributed " +
			"to cell " + cellID + ", so treating those seconds as zero would understate provider cost")
}

// combineCostComponents sums modeled components under one directive name.
// Any unknown makes the pair unknown: half a cost reported as the whole cost is
// the specific dishonesty this refuses.
func combineCostComponents(name string, parts []PricingCostComponent) PricingCostComponent {
	total := 0.0
	modeled, na := 0, 0
	var reasons []string
	for _, c := range parts {
		switch c.Status {
		case pricingCostModeled:
			total += c.Amount
			modeled++
			reasons = append(reasons, c.Basis)
		case pricingCostNotApplicable:
			na++
			reasons = append(reasons, c.Basis)
		default:
			return unknownCost(name + " is unknown because a component is unknown: " + c.Basis)
		}
	}
	if modeled == 0 {
		return notApplicableCost(name + ": " + strings.Join(reasons, "; "))
	}
	return modeledCost(roundEconomicUSD(total), name+": "+strings.Join(reasons, "; "))
}

func frozenCellEvidenceIdentity(
	placement PlacementRequirement,
	catalogue CataloguePriceAuthority,
	f *FrozenRuntimeCellEconomics,
) []string {
	out := []string{
		"runtime_cell:" + placement.RuntimeCellID,
		"runtime_matrix_sha256:" + placement.RuntimeMatrixSHA256,
		"supplier_entitlement_policy:" + f.SupplierEntitlementPolicy,
		"cell_entitlement_resolution:" + f.EntitlementResolution,
		"src/control/pricing_decision.go#exactTaskEconomics",
		"src/control/runtime_cell_entitlement.go#resolveCellPlatformDelivery",
	}
	if f.Version >= frozenRuntimeCellEconomicsVersion {
		out = append(out,
			"runtime_authority_sha256:"+f.RuntimeAuthoritySHA256,
			"supply_class:"+f.SupplyClass,
		)
	}
	if placement.PerformanceAuthority != nil {
		out = append(out,
			"runtime_performance_authority:"+placement.PerformanceAuthority.Digest,
			"benchmark_snapshot:"+placement.PerformanceAuthority.BenchmarkSnapshotSHA256,
		)
	}
	if f.ModelContract.ModelID != "" {
		out = append(out, "model_contract:"+f.ModelContract.ModelID+"/"+f.ModelContract.JobType)
	}
	if catalogue.ScheduleSHA256 != "" {
		out = append(out, "catalogue_schedule:"+catalogue.ScheduleSHA256)
	}
	if catalogue.BoardSHA256 != "" {
		out = append(out, "price_board:"+catalogue.BoardSHA256)
	}
	if f.EnergySource != "" {
		out = append(out, "energy:"+f.EnergySource)
	}
	if f.ProviderRateAuthority.Resolved {
		out = append(out,
			"provider_rate_status:"+f.ProviderRateAuthority.AuthorityStatus,
			"provider_rate_provenance:"+f.ProviderRateAuthority.Provenance,
		)
	}
	if f.BuildIdentity.Knowledge == fieldProvenanceUnknown {
		out = append(out, "build_identity:absent")
	}
	return out
}

func dedupeStrings(in []string) []string {
	if len(in) == 0 {
		return nil
	}
	seen := make(map[string]struct{}, len(in))
	out := make([]string, 0, len(in))
	for _, s := range in {
		if _, dup := seen[s]; dup {
			continue
		}
		seen[s] = struct{}{}
		out = append(out, s)
	}
	return out
}

// digestFrozenRuntimeCellEconomics hashes the block with Digest cleared so the
// digest is not self-referential.
func digestFrozenRuntimeCellEconomics(f *FrozenRuntimeCellEconomics) (string, error) {
	if f == nil {
		return "", fmt.Errorf("frozen runtime-cell economics is nil")
	}
	if f.Version == frozenRuntimeCellEconomicsLegacyVersion {
		legacy := frozenRuntimeCellEconomicsV1Digest{
			Version: f.Version, Kind: f.Kind, ModelContract: f.ModelContract,
			CellID: f.CellID, RuntimeID: f.RuntimeID, Engine: f.Engine,
			HWClass: f.HWClass, HWClasses: append([]string(nil), f.HWClasses...),
			BuildIdentity: f.BuildIdentity, EntitlementResolution: f.EntitlementResolution,
			ConservativeUnitsPerSec: f.ConservativeUnitsPerSec,
			ExpectedMsPerUnit:       f.ExpectedMsPerUnit, ExpectedSeconds: f.ExpectedSeconds,
			BillableUnits:        f.BillableUnits,
			ThroughputProvenance: f.ThroughputProvenance,
			DurationProvenance:   f.DurationProvenance,
			SupplierProvenance:   f.SupplierProvenance, BuyerProvenance: f.BuyerProvenance,
			BuyerPriceUSD: f.BuyerPriceUSD, SupplierEntitlementUSD: f.SupplierEntitlementUSD,
			SupplierEntitlementPolicy: f.SupplierEntitlementPolicy, SupplierAskUSDHr: f.SupplierAskUSDHr,
			PlatformDeliveryCostUSD:        f.PlatformDeliveryCostUSD,
			PlatformDeliveryCostUSDPerUnit: f.PlatformDeliveryCostUSDPerUnit,
			PlatformDeliveryCostStatus:     f.PlatformDeliveryCostStatus,
			PlatformDeliveryCostBasis:      f.PlatformDeliveryCostBasis,
			PhysicalCost:                   f.PhysicalCost, ProviderCost: f.ProviderCost,
			ReliabilityCost: f.ReliabilityCost, VerificationCost: f.VerificationCost,
			StorageTransfer: f.StorageTransfer, EnergyPartial: f.EnergyPartial,
			RiskAllocation: f.RiskAllocation, StartupResidency: f.StartupResidency,
			ExpectedVOCostUSD:        f.ExpectedVOCostUSD,
			ExpectedVOCostUSDPerUnit: f.ExpectedVOCostUSDPerUnit,
			ExpectedVOCostStatus:     f.ExpectedVOCostStatus,
			ExpectedVOCostBasis:      f.ExpectedVOCostBasis,
			UnknownCategories:        append([]string(nil), f.UnknownCategories...),
			MercTrueNetUSD:           f.MercTrueNetUSD, MercTrueNetStatus: f.MercTrueNetStatus,
			EnergyJoules: f.EnergyJoules, EnergyKnowledge: f.EnergyKnowledge,
			EnergySource: f.EnergySource, Confidence: f.Confidence,
			EvidenceIdentity: append([]string(nil), f.EvidenceIdentity...),
		}
		raw, err := json.Marshal(legacy)
		if err != nil {
			return "", err
		}
		sum := sha256.Sum256(raw)
		return hex.EncodeToString(sum[:]), nil
	}
	cp := *f
	cp.Digest = ""
	raw, err := json.Marshal(cp)
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(raw)
	return hex.EncodeToString(sum[:]), nil
}
