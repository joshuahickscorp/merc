package main

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"math"
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
	frozenRuntimeCellEconomicsVersion = 1
	frozenRuntimeCellEconomicsKind    = "frozen_runtime_cell_economics"

	// Status vocabulary for the named-cost sum. A partial sum that calls itself
	// complete is the failure this field exists to prevent.
	frozenVOCostComplete = "MODELED_ALL_NAMED_TERMS"
	frozenVOCostPartial  = "PARTIAL_MODELED_TERMS_ONLY"
	frozenVOCostRefused  = "REFUSED_NO_MODELED_TERM"
)

// FrozenRuntimeCellEconomics is one runtime cell's accepted economics.
type FrozenRuntimeCellEconomics struct {
	Version int    `json:"version"`
	Kind    string `json:"kind"`

	// Identity. HWClass is the single hardware class this binding could resolve;
	// empty means the accepted placement admits more than one and no per-class
	// term (energy) may be modeled for it.
	CellID    string   `json:"cell_id"`
	RuntimeID string   `json:"runtime_id"`
	Engine    string   `json:"engine"`
	HWClass   string   `json:"hw_class,omitempty"`
	HWClasses []string `json:"hw_classes,omitempty"`

	// Accepted execution geometry. ConservativeUnitsPerSec is the governed
	// haircut rate admission priced on — never an observed peak, never today's
	// receipt.
	ConservativeUnitsPerSec float64 `json:"conservative_units_per_sec"`
	ExpectedMsPerUnit       float64 `json:"expected_ms_per_unit"`
	ExpectedSeconds         float64 `json:"expected_seconds"`
	BillableUnits           float64 `json:"billable_units"`

	// Money as accepted. These duplicate PricingDecision fields on purpose: the
	// digest below covers them, so a rewrite of the decision that leaves this
	// block alone is detectable.
	BuyerPriceUSD             float64 `json:"buyer_price_usd"`
	SupplierEntitlementUSD    float64 `json:"supplier_entitlement_usd"`
	SupplierEntitlementPolicy string  `json:"supplier_entitlement_policy"`
	SupplierAskUSDHr          float64 `json:"supplier_ask_usd_hr"`

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

// freezeRuntimeCellEconomics builds the accepted binding from frozen inputs.
//
// Every argument is already frozen in the decision being built. The compiled
// runtime authority is read for cell classification and hardware platforms; it
// is pinned by placement.RuntimeMatrixSHA256, which validatePlacementRequirement
// compares against the binary's own generatedRuntimeMatrixSHA256, so reading it
// cannot make this binding time-dependent.
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
	out := &FrozenRuntimeCellEconomics{
		Version:                   frozenRuntimeCellEconomicsVersion,
		Kind:                      frozenRuntimeCellEconomicsKind,
		CellID:                    placement.RuntimeCellID,
		RuntimeID:                 placement.RuntimeID,
		Engine:                    placement.Engine,
		ConservativeUnitsPerSec:   unitsPerSec,
		ExpectedSeconds:           expectedSeconds,
		BillableUnits:             billableUnits,
		BuyerPriceUSD:             buyerPriceUSD,
		SupplierEntitlementUSD:    supplierEntitlementUSD,
		SupplierEntitlementPolicy: firstNonEmpty(supplierEntitlementPolicy, supplierEntitlementPolicyCancelled),
		SupplierAskUSDHr:          supplierAskUSDHr,
		PhysicalCost:              physical,
		ProviderCost:              provider,
		VerificationCost:          verification,
		Confidence:                confidence,
	}
	if unitsPerSec > 0 {
		out.ExpectedMsPerUnit = 1000.0 / unitsPerSec
	}
	if len(placement.HWClasses) > 0 {
		out.HWClasses = append([]string(nil), placement.HWClasses...)
	}
	out.HWClass = acceptedHWClassForCell(placement.RuntimeCellID, placement.HWClasses)

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
	out.EnergyPartial = frozenEnergyComponent(out.HWClass, out.ExpectedMsPerUnit, billableUnits)
	if out.EnergyPartial.Status == pricingCostModeled {
		out.EnergyJoules = frozenEnergyJoules(out.HWClass, expectedSeconds)
		if entry, ok := sustainedWattsByHWClass[out.HWClass]; ok {
			out.EnergyKnowledge = string(entry.Kind())
			out.EnergySource = entry.Provenance() + " + control/pricing.go:defaultElectricityUSDPerKWh"
		}
	}
	out.RiskAllocation = risk

	// Reliability / retry overhead. Nothing in the frozen inputs carries a
	// measured retry rate for this cell, and a retry costs another supplier
	// entitlement, so this is unknown rather than zero. Redundancy and honeypot
	// tasks are already priced as verification and are not retries.
	out.ReliabilityCost = unknownCost(
		"no measured retry rate for cell " + placement.RuntimeCellID + " is bound into admission; " +
			"a retried task costs a further supplier entitlement, so a zero here would be a claim, not a measurement")

	// Startup and residency.
	out.StartupResidency = frozenStartupResidencyComponent(placement.RuntimeCellID)

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
// When the buyer froze a constraint set, that set is the accepted one. When it
// did not, the cell's own compiled platforms are. Either way a set of size other
// than one is not a class, and pretending otherwise would attribute one card's
// watts to whatever machine happened to claim the work.
func acceptedHWClassForCell(cellID string, frozen []string) string {
	classes := frozen
	if len(classes) == 0 {
		classes = cellPlatformsFromAuthority(cellID)
	}
	if len(classes) != 1 {
		return ""
	}
	return strings.TrimSpace(classes[0])
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
	return modeledCost(roundEconomicUSD(usd), fmt.Sprintf(
		"%.1f W (%s) × %.6fs accepted duration × $%.2f/kWh = %.6f J; "+
			"electricity is a policy default and the watts constant is %s, so this is a "+
			"defaulted-grade figure, not a metered invoice; %s",
		entry.Watts(), entry.Kind(), seconds, defaultElectricityUSDPerKWh, joules,
		entry.Kind(), entry.Provenance()))
}

// frozenStartupResidencyComponent states who pays for having the cell resident.
//
// For owned or community supply the worker is already resident and its idle time
// is the supplier's cost, exactly as provider cost is not applicable there. For
// cloud-backed supply the pod-seconds Merc pays for already include startup, so
// naming a second startup charge would double-count it — the honest statement is
// that it is inside provider cost, not that it is zero.
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
	return notApplicableCost(
		"cloud-backed supply: startup and residency seconds are already inside the governed " +
			"pod-rate × seconds provider cost for cell " + cellID + "; charging them again would double-count")
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
		"control/pricing_decision.go#exactTaskEconomics",
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
	cp := *f
	cp.Digest = ""
	raw, err := json.Marshal(cp)
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(raw)
	return hex.EncodeToString(sum[:]), nil
}
