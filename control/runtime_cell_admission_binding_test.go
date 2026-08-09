package main

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"
)

// The freeze is the point. A projection that moves when a benchmark is
// revalidated is a selector view; a projection frozen at admission is what
// settlement may stand on. These tests pin the difference.

func TestFrozenRuntimeCellEconomicsIsBoundAtAdmission(t *testing.T) {
	_, _, placement, _, pricing := distributedPricingFixture(t)
	f := pricing.RuntimeCell
	if f == nil {
		t.Fatal("distributed pricing decision froze no runtime-cell economics")
	}
	if f.CellID != placement.RuntimeCellID || f.RuntimeID != placement.RuntimeID ||
		f.Engine != placement.Engine {
		t.Fatalf("frozen cell identity does not match the accepted placement: %+v", f)
	}
	if f.ConservativeUnitsPerSec != pricing.ExpectedSupplierUnitsPerSec {
		t.Fatalf("frozen throughput %v is not the rate admission priced on (%v)",
			f.ConservativeUnitsPerSec, pricing.ExpectedSupplierUnitsPerSec)
	}
	if f.ExpectedSeconds != pricing.ExpectedSupplierSeconds ||
		f.BillableUnits != pricing.BillableUnits ||
		f.BuyerPriceUSD != pricing.BuyerPrice {
		t.Fatalf("frozen geometry disagrees with the decision it is bound to: %+v", f)
	}
	if !validSHA256(f.Digest) {
		t.Fatalf("frozen runtime-cell economics has no digest: %q", f.Digest)
	}
	if len(f.EvidenceIdentity) == 0 {
		t.Fatal("frozen runtime-cell economics names no evidence identity")
	}
}

// A settled decision's money and its frozen cell block do not move when the
// benchmark behind the cell is revalidated to a different rate. This is the
// property the whole type exists for: the same defect class that made a settled
// receipt depend on a re-runnable benchmark.
func TestFrozenRuntimeCellEconomicsSurvivesBenchmarkRevalidation(t *testing.T) {
	workload, compute, placement, economic, settled := distributedPricingFixture(t)
	if settled.RuntimeCell == nil {
		t.Fatal("fixture froze no runtime-cell economics")
	}
	before := *settled.RuntimeCell
	beforeBuyer := settled.BuyerPrice
	beforeSupplierNanos := settled.SupplierGrossNanos

	// Revalidate: the same cell, measured faster. Admission would price a new
	// job differently; the settled decision must not move.
	faster := settled.ExpectedSupplierUnitsPerSec * 1.62
	fasterPlacement := placement
	fasterCeiling := expectedSupplierUSDHr(
		faster, settled.Catalogue.ReferencePricePer1K,
		settled.Catalogue.SupplierShare, settled.Tier)
	fasterPlacement.OfferedRateUsdHr = float32(fasterCeiling)
	revalidated, err := distributedPricingDecisionAtRate(
		workload, compute, fasterPlacement, economic, settled.Catalogue,
		settled.Tier, "", faster,
	)
	mustf(t, err, "rebuild at revalidated rate: %v")

	if !reflect.DeepEqual(before, *settled.RuntimeCell) {
		t.Fatal("revalidation mutated the settled decision's frozen runtime-cell economics")
	}
	if settled.BuyerPrice != beforeBuyer || settled.SupplierGrossNanos != beforeSupplierNanos {
		t.Fatal("revalidation moved settled money")
	}
	// And the revalidated projection really is different, or the test above
	// proves nothing.
	if revalidated.RuntimeCell == nil {
		t.Fatal("revalidated decision froze no runtime-cell economics")
	}
	if revalidated.RuntimeCell.Digest == before.Digest {
		t.Fatal("a 1.62x throughput revalidation produced an identical frozen block; " +
			"the freeze test would pass vacuously")
	}
	if revalidated.RuntimeCell.ConservativeUnitsPerSec <= before.ConservativeUnitsPerSec {
		t.Fatalf("revalidated throughput did not rise: %v -> %v",
			before.ConservativeUnitsPerSec, revalidated.RuntimeCell.ConservativeUnitsPerSec)
	}
}

// Historical replay must use the cell/provider classification that admission
// froze. CloudBacked is activation/economics authority in the runtime document,
// and changing it after acceptance must affect new decisions without rewriting
// an old one.
func TestFrozenRuntimeCellEconomicsSurvivesCurrentCellProviderReclassification(t *testing.T) {
	workload, compute, placement, economic, settled := distributedPricingFixture(t)
	if settled.RuntimeCell == nil {
		t.Fatal("fixture froze no runtime-cell economics")
	}
	if settled.ProviderCost.Status != pricingCostNotApplicable ||
		settled.RuntimeCell.StartupResidency.Status != pricingCostNotApplicable {
		t.Fatalf("fixture is not accepted community/owned supply: provider=%+v startup=%+v",
			settled.ProviderCost, settled.RuntimeCell.StartupResidency)
	}
	before := settled

	savedAuthority := runtimeAuthority
	edited := runtimeAuthority
	edited.Runtimes = append([]authorityRuntimeProfile(nil), runtimeAuthority.Runtimes...)
	found := false
	for i := range edited.Runtimes {
		edited.Runtimes[i].Cells = append([]authorityCell(nil), edited.Runtimes[i].Cells...)
		for j := range edited.Runtimes[i].Cells {
			if edited.Runtimes[i].Cells[j].ID == placement.RuntimeCellID {
				edited.Runtimes[i].Cells[j].CloudBacked = true
				found = true
			}
		}
	}
	if !found {
		t.Fatalf("accepted cell %q is absent from runtime authority", placement.RuntimeCellID)
	}
	runtimeAuthority = edited
	savedAuthoritySHA := generatedRuntimeAuthorityFileSHA256
	generatedRuntimeAuthorityFileSHA256 = strings.Repeat("f", 64)
	if generatedRuntimeAuthorityFileSHA256 == savedAuthoritySHA {
		generatedRuntimeAuthorityFileSHA256 = strings.Repeat("e", 64)
	}
	t.Cleanup(func() {
		runtimeAuthority = savedAuthority
		generatedRuntimeAuthorityFileSHA256 = savedAuthoritySHA
	})

	if err := ValidateDistributedPricingDecisionSnapshot(
		settled, workload, compute, placement, economic,
	); err != nil {
		t.Fatalf("historical pricing inherited current provider reclassification: %v", err)
	}
	if !reflect.DeepEqual(before, settled) {
		t.Fatal("historical validation mutated the accepted pricing decision")
	}

	current, err := distributedPricingDecisionAtRate(
		workload, compute, placement, economic, settled.Catalogue,
		settled.Tier, settled.OriginQuotePricingDecisionSHA256,
		settled.ExpectedSupplierUnitsPerSec,
	)
	mustf(t, err, "build decision under current provider classification: %v")
	if reflect.DeepEqual(current.ProviderCost, settled.ProviderCost) ||
		reflect.DeepEqual(current.RuntimeCell.StartupResidency, settled.RuntimeCell.StartupResidency) {
		t.Fatalf("authority mutation did not change new economics; historical test is vacuous: current provider=%+v startup=%+v",
			current.ProviderCost, current.RuntimeCell.StartupResidency)
	}
}

func TestLegacyV1FrozenRuntimeCellSerializedSnapshotReplaysWithoutCurrentAuthority(t *testing.T) {
	workload, compute, placement, economic, pricing := distributedPricingFixture(t)
	if pricing.RuntimeCell == nil {
		t.Fatal("fixture froze no runtime-cell economics")
	}
	legacyBlock := *pricing.RuntimeCell
	legacyBlock.Version = frozenRuntimeCellEconomicsLegacyVersion
	legacyBlock.RuntimeAuthoritySHA256 = ""
	legacyBlock.SupplyClass = ""
	legacyBlock.ProviderRateAuthority = FrozenProviderRateAuthority{}
	// V1 used the original named-cost vocabulary for the narrower delivery
	// subtotal. Preserve that historical semantic value, not the v2 projection's
	// renamed platform-delivery status.
	switch legacyBlock.PlatformDeliveryCostStatus {
	case platformDeliveryRefused:
		legacyBlock.PlatformDeliveryCostStatus = frozenVOCostRefused
	case platformDeliveryComplete:
		legacyBlock.PlatformDeliveryCostStatus = frozenVOCostComplete
	case platformDeliveryPartial:
		legacyBlock.PlatformDeliveryCostStatus = frozenVOCostPartial
	}
	legacyBlock.EvidenceIdentity = append([]string(nil), legacyBlock.EvidenceIdentity...)
	filtered := legacyBlock.EvidenceIdentity[:0]
	for _, identity := range legacyBlock.EvidenceIdentity {
		if strings.HasPrefix(identity, "runtime_authority_sha256:") ||
			strings.HasPrefix(identity, "supply_class:") ||
			strings.HasPrefix(identity, "provider_rate_") {
			continue
		}
		filtered = append(filtered, identity)
	}
	legacyBlock.EvidenceIdentity = filtered
	legacyBlock.Digest = ""
	var err error
	legacyBlock.Digest, err = digestFrozenRuntimeCellEconomics(&legacyBlock)
	must(t, err)
	legacyDecision := pricing
	legacyDecision.RuntimeCell = &legacyBlock

	// Serialize the exact pre-v2 shape: the enriched authority fields do not
	// exist in the stored JSON at all, rather than being present as zero values.
	raw, err := json.Marshal(legacyDecision)
	must(t, err)
	var decisionObject map[string]json.RawMessage
	must(t, json.Unmarshal(raw, &decisionObject))
	var cellObject map[string]json.RawMessage
	must(t, json.Unmarshal(decisionObject["runtime_cell"], &cellObject))
	delete(cellObject, "runtime_authority_sha256")
	delete(cellObject, "supply_class")
	delete(cellObject, "provider_rate_authority")
	decisionObject["runtime_cell"], err = json.Marshal(cellObject)
	must(t, err)
	raw, err = json.Marshal(decisionObject)
	must(t, err)
	if strings.Contains(string(raw), "runtime_authority_sha256") ||
		strings.Contains(string(raw), "provider_rate_authority") {
		t.Fatal("legacy wire fixture still carries v2 runtime authority fields")
	}
	var reloaded PricingDecision
	must(t, json.Unmarshal(raw, &reloaded))

	// Today's cell is now cloud-backed. V1 did not carry enough raw authority to
	// re-derive that classification, so replay must retain its digest-intact
	// accepted provider/startup components instead of borrowing this change.
	savedAuthority := runtimeAuthority
	edited := runtimeAuthority
	edited.Runtimes = append([]authorityRuntimeProfile(nil), runtimeAuthority.Runtimes...)
	for i := range edited.Runtimes {
		edited.Runtimes[i].Cells = append([]authorityCell(nil), edited.Runtimes[i].Cells...)
		for j := range edited.Runtimes[i].Cells {
			if edited.Runtimes[i].Cells[j].ID == placement.RuntimeCellID {
				edited.Runtimes[i].Cells[j].CloudBacked = true
			}
		}
	}
	runtimeAuthority = edited
	t.Cleanup(func() { runtimeAuthority = savedAuthority })

	if err := ValidateDistributedPricingDecisionSnapshot(
		reloaded, workload, compute, placement, economic,
	); err != nil {
		t.Fatalf("legacy v1 serialized economics inherited current runtime authority: %v", err)
	}
}

func TestLegacyPlacementV1RejectsEnrichedRuntimeCellV2(t *testing.T) {
	workload, compute, placement, economic, current := distributedPricingFixture(t)
	if current.RuntimeCell == nil || current.RuntimeCell.Version != frozenRuntimeCellEconomicsVersion {
		t.Fatal("current fixture lacks enriched runtime-cell economics v2")
	}
	legacyPlacement := placement
	legacyPlacement.Version = 1
	legacyPlacement.PerformanceAuthority = nil
	legacyPlacement.HWClasses = append(
		[]string(nil), workload.Binding.Constraints.HWClasses...)

	legacy, err := distributedPricingDecisionAtRate(
		workload, compute, legacyPlacement, economic, current.Catalogue,
		current.Tier, current.OriginQuotePricingDecisionSHA256,
		current.ExpectedSupplierUnitsPerSec,
	)
	mustf(t, err, "build legacy pricing decision: %v")
	if legacy.RuntimeCell != nil {
		t.Fatal("legacy placement minted runtime-cell economics from missing v2 placement authority")
	}

	impossible := legacy
	impossible.RuntimeCell = current.RuntimeCell
	err = ValidateDistributedPricingDecisionSnapshot(
		impossible, workload, compute, legacyPlacement, economic)
	if err == nil || !strings.Contains(err.Error(),
		"enriched economics requires placement-v2") {
		t.Fatalf("legacy placement accepted impossible runtime-cell v2 pairing: %v", err)
	}
}

// The same inputs produce the same decision, byte for byte, every time.
//
// This is the property the whole freeze rests on: the validator rebuilds and
// DeepEquals, so a decision that differs from itself is refused. It was worth a
// test of its own because the first version of this block summed its cost terms
// over a Go map, and randomised map order changed the float addition order
// enough to move the per-unit figure by one ulp between two builds of the same
// job. Nothing about that failure looks like a money bug until you see it.
func TestFrozenRuntimeCellIsDeterministicAcrossBuilds(t *testing.T) {
	workload, compute, placement, economic, first := distributedPricingFixture(t)
	for i := 0; i < 32; i++ {
		again, err := distributedPricingDecisionAtRate(
			workload, compute, placement, economic, first.Catalogue, first.Tier,
			first.OriginQuotePricingDecisionSHA256, first.ExpectedSupplierUnitsPerSec,
		)
		mustf(t, err, "rebuild %d: %v", i)
		if !reflect.DeepEqual(first, again) {
			a, _ := json.Marshal(first.RuntimeCell)
			b, _ := json.Marshal(again.RuntimeCell)
			t.Fatalf("rebuild %d differs from the original decision\n first: %s\n again: %s", i, a, b)
		}
	}

	// The loop above is a general guard and is NOT sufficient on its own. Whether
	// it fires depends on whether a fixture's amounts happen to sum
	// order-invariantly and round to themselves — this one's do, on both counts,
	// which is exactly why the defect reached the catalogue-anchor suite instead
	// of being caught here. The arithmetic is pinned directly below.
}

// The published per-unit figure is the published total over the units, exactly.
//
// Tested on the arithmetic rather than through a fixture, deliberately: the
// distributed fixture's cost terms sum to a value that is already clean at six
// decimals, so it cannot distinguish "per unit from the raw accumulator" from
// "per unit from the published total". These amounts can — 0.1+0.2+0.3 is
// 0.6000000000000001 in binary, and dividing that instead of 0.6 is what put a
// money figure one ulp away from reproducing.
func TestFrozenRuntimeCellPerUnitDividesThePublishedTotal(t *testing.T) {
	f := &FrozenRuntimeCellEconomics{
		BillableUnits:   128,
		PhysicalCost:    modeledCost(0.1, "test"),
		ProviderCost:    modeledCost(0.2, "test"),
		StorageTransfer: modeledCost(0.3, "test"),
		ReliabilityCost: unknownCost("test"),
	}
	f.sumNamedVerifiedOutcomeCost()

	if f.ExpectedVOCostUSD != 0.6 {
		t.Fatalf("published total %v is not the rounded sum 0.6", f.ExpectedVOCostUSD)
	}
	want := f.ExpectedVOCostUSD / f.BillableUnits
	if f.ExpectedVOCostUSDPerUnit != want {
		t.Fatalf("per-unit %v is not the published total %v over %v units (%v); "+
			"it was derived from something other than the figure this block publishes",
			f.ExpectedVOCostUSDPerUnit, f.ExpectedVOCostUSD, f.BillableUnits, want)
	}
	if f.ExpectedVOCostStatus != frozenVOCostPartial {
		t.Fatalf("an unknown term left the status at %q", f.ExpectedVOCostStatus)
	}
}

// Half a cost is never reported as the whole cost.
//
// combineCostComponents folds storage and egress into the one "storage and
// transfer" term the directive names. In the fixture both halves are modeled, so
// the branch that matters — one half unknown — is never exercised through it.
// Exercised directly here, because a pair that quietly reports the modeled half
// as the total is a cost understatement, not a rounding question.
func TestCombinedCostRefusesToReportHalfATotal(t *testing.T) {
	got := combineCostComponents("storage and result transfer", []PricingCostComponent{
		modeledCost(0.25, "storage modeled"),
		unknownCost("egress bytes are not attributable"),
	})
	if got.Status != pricingCostUnknown {
		t.Fatalf("a pair with an unknown half reported %q amount %v; the modeled half is not the total",
			got.Status, got.Amount)
	}
	if got.Amount != 0 {
		t.Fatalf("unknown combined cost carries amount %v", got.Amount)
	}

	// Both modeled: sum, and say what it is made of.
	sum := combineCostComponents("storage and result transfer", []PricingCostComponent{
		modeledCost(0.25, "storage modeled"),
		modeledCost(0.5, "egress modeled"),
	})
	if sum.Status != pricingCostModeled || sum.Amount != 0.75 {
		t.Fatalf("two modeled halves gave %q %v, want modeled 0.75", sum.Status, sum.Amount)
	}
	// Both not-applicable: not applicable, not a modeled zero.
	na := combineCostComponents("storage and result transfer", []PricingCostComponent{
		notApplicableCost("no retained bytes"),
		notApplicableCost("no result bytes"),
	})
	if na.Status != pricingCostNotApplicable || na.Amount != 0 {
		t.Fatalf("two not-applicable halves gave %q %v", na.Status, na.Amount)
	}
}

// An unknown term is never summed as zero, and a partial sum never calls itself
// complete.
func TestFrozenRuntimeCellCostSumRefusesUnknownAsZero(t *testing.T) {
	_, _, _, _, pricing := distributedPricingFixture(t)
	f := pricing.RuntimeCell
	if f == nil {
		t.Fatal("fixture froze no runtime-cell economics")
	}

	named := map[string]PricingCostComponent{
		"physical":          f.PhysicalCost,
		"provider":          f.ProviderCost,
		"reliability":       f.ReliabilityCost,
		"verification":      f.VerificationCost,
		"storage_transfer":  f.StorageTransfer,
		"energy_partial":    f.EnergyPartial,
		"risk_allocation":   f.RiskAllocation,
		"startup_residency": f.StartupResidency,
	}
	sum := 0.0
	unknown := 0
	for name, c := range named {
		switch c.Status {
		case pricingCostModeled:
			sum += c.Amount
		case pricingCostUnknown:
			unknown++
			if c.Amount != 0 {
				t.Fatalf("unknown term %q carries a modeled amount %v", name, c.Amount)
			}
		case pricingCostNotApplicable:
			if c.Amount != 0 {
				t.Fatalf("not-applicable term %q carries an amount %v", name, c.Amount)
			}
		default:
			t.Fatalf("term %q has invalid status %q", name, c.Status)
		}
		if strings.TrimSpace(c.Basis) == "" {
			t.Fatalf("term %q has no basis", name)
		}
	}
	if roundEconomicUSD(sum) != f.ExpectedVOCostUSD {
		t.Fatalf("named sum %v does not match the frozen figure %v",
			roundEconomicUSD(sum), f.ExpectedVOCostUSD)
	}
	if unknown > 0 && f.ExpectedVOCostStatus != frozenVOCostPartial {
		t.Fatalf("%d unknown terms but status is %q", unknown, f.ExpectedVOCostStatus)
	}
	if unknown == 0 && f.ExpectedVOCostStatus != frozenVOCostComplete {
		t.Fatalf("no unknown terms but status is %q", f.ExpectedVOCostStatus)
	}
	// Reliability is the term nothing in the frozen inputs can measure. If it
	// ever becomes modeled, that is a real change and this test should be the
	// thing that notices.
	if f.ReliabilityCost.Status != pricingCostUnknown {
		t.Fatalf("reliability/retry overhead claims status %q; nothing at admission measures it",
			f.ReliabilityCost.Status)
	}
	// True net is refused while anything is unknown.
	if len(f.UnknownCategories) > 0 && f.MercTrueNetUSD != nil {
		t.Fatalf("true net was published with unknown categories %v", f.UnknownCategories)
	}
}

func TestCloudStartupResidencyRemainsUnknownWithoutFrozenDuration(t *testing.T) {
	component := frozenStartupResidencyComponent("vllm-cuda-llama1-infer")
	if component.Status != pricingCostUnknown {
		t.Fatalf("cloud startup/residency status = %q, want unknown", component.Status)
	}
	for _, required := range []string{"execution seconds only", "pod-startup", "minimum-billing"} {
		if !strings.Contains(component.Basis, required) {
			t.Fatalf("cloud startup/residency refusal does not name %q: %s", required, component.Basis)
		}
	}
}

// The digest covers the block: any edit to a bound term changes it.
func TestFrozenRuntimeCellDigestCoversEveryTerm(t *testing.T) {
	_, _, _, _, pricing := distributedPricingFixture(t)
	f := pricing.RuntimeCell
	if f == nil {
		t.Fatal("fixture froze no runtime-cell economics")
	}
	recomputed, err := digestFrozenRuntimeCellEconomics(f)
	must(t, err)
	if recomputed != f.Digest {
		t.Fatalf("digest does not reproduce: stored %s recomputed %s", f.Digest, recomputed)
	}
	for name, mutate := range map[string]func(*FrozenRuntimeCellEconomics){
		"cell_id":        func(m *FrozenRuntimeCellEconomics) { m.CellID += "-tampered" },
		"throughput":     func(m *FrozenRuntimeCellEconomics) { m.ConservativeUnitsPerSec *= 2 },
		"supplier":       func(m *FrozenRuntimeCellEconomics) { m.SupplierEntitlementUSD += 0.01 },
		"physical_cost":  func(m *FrozenRuntimeCellEconomics) { m.PhysicalCost.Amount += 0.01 },
		"vo_cost":        func(m *FrozenRuntimeCellEconomics) { m.ExpectedVOCostUSD += 0.01 },
		"vo_status":      func(m *FrozenRuntimeCellEconomics) { m.ExpectedVOCostStatus = frozenVOCostComplete },
		"energy_joules":  func(m *FrozenRuntimeCellEconomics) { m.EnergyJoules += 1 },
		"unknown_set":    func(m *FrozenRuntimeCellEconomics) { m.UnknownCategories = nil },
		"true_net_state": func(m *FrozenRuntimeCellEconomics) { m.MercTrueNetStatus = "TRUE_NET_AVAILABLE" },
	} {
		mutant := *f
		mutate(&mutant)
		got, err := digestFrozenRuntimeCellEconomics(&mutant)
		must(t, err)
		if got == f.Digest {
			t.Fatalf("digest is blind to %s", name)
		}
	}
}

// A decision frozen before runtime-cell economics existed replays against its
// own composite authority. It is never retro-fitted with a binding it did not
// accept.
func TestLegacyPricingDecisionWithoutFrozenCellStillReplays(t *testing.T) {
	workload, compute, placement, economic, pricing := distributedPricingFixture(t)
	legacy := pricing
	legacy.RuntimeCell = nil
	if err := ValidateDistributedPricingDecisionSnapshot(
		legacy, workload, compute, placement, economic,
	); err != nil {
		t.Fatalf("legacy decision without a frozen cell block was refused: %v", err)
	}
	// The modern decision still validates, and a TAMPERED block does not.
	if err := ValidateDistributedPricingDecisionSnapshot(
		pricing, workload, compute, placement, economic,
	); err != nil {
		t.Fatalf("frozen-cell decision was refused: %v", err)
	}
	tampered := pricing
	block := *pricing.RuntimeCell
	block.ExpectedVOCostUSD += 0.01
	tampered.RuntimeCell = &block
	if err := ValidateDistributedPricingDecisionSnapshot(
		tampered, workload, compute, placement, economic,
	); err == nil {
		t.Fatal("a rewritten frozen runtime-cell block passed snapshot validation")
	}

	// Stored decisions make the trip through JSON. omitempty turns an empty
	// slice into nil on the way back, and the validator is a DeepEqual, so a
	// block that is correct in memory and different after a round trip would
	// fail every reload. This is the check that catches that, not a formality.
	raw, err := json.Marshal(pricing)
	must(t, err)
	var reloaded PricingDecision
	must(t, json.Unmarshal(raw, &reloaded))
	if !reflect.DeepEqual(pricing, reloaded) {
		t.Fatal("pricing decision did not survive a JSON round trip unchanged")
	}
	if err := ValidateDistributedPricingDecisionSnapshot(
		reloaded, workload, compute, placement, economic,
	); err != nil {
		t.Fatalf("reloaded decision failed snapshot validation: %v", err)
	}
}
