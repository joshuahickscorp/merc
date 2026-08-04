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
	if err != nil {
		t.Fatalf("rebuild at revalidated rate: %v", err)
	}

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
		if err != nil {
			t.Fatalf("rebuild %d: %v", i, err)
		}
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

// The digest covers the block: any edit to a bound term changes it.
func TestFrozenRuntimeCellDigestCoversEveryTerm(t *testing.T) {
	_, _, _, _, pricing := distributedPricingFixture(t)
	f := pricing.RuntimeCell
	if f == nil {
		t.Fatal("fixture froze no runtime-cell economics")
	}
	recomputed, err := digestFrozenRuntimeCellEconomics(f)
	if err != nil {
		t.Fatal(err)
	}
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
		if err != nil {
			t.Fatal(err)
		}
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
	if err != nil {
		t.Fatal(err)
	}
	var reloaded PricingDecision
	if err := json.Unmarshal(raw, &reloaded); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(pricing, reloaded) {
		t.Fatal("pricing decision did not survive a JSON round trip unchanged")
	}
	if err := ValidateDistributedPricingDecisionSnapshot(
		reloaded, workload, compute, placement, economic,
	); err != nil {
		t.Fatalf("reloaded decision failed snapshot validation: %v", err)
	}
}
