package main

import (
	"math"
	"testing"
)

// The three-record embed fixture, as arithmetic.
//
// These are the exact figures the public-API driver reaches on this fleet, taken
// from the deployment inputs `TestFirstCompleteLoopThroughThePublicAPI` sets and
// the governed price board:
//
//	corpus                233 bytes over 3 JSONL records
//	settlement units      max(3, 233/4) = 58.25
//	reference price       $0.0000180 USD / 1,000 units   (board median x 0.9)
//	FX                    1.37 USD -> CAD, operator declared
//	settlement price      $0.0000246600 CAD / 1,000 units
//	supplier share        0.97
//	tier                  batch (multiplier 1.0)
//	measured throughput   ~1,666 units/sec on the embed cell
//
// From those, the exact per-task figures are:
//
//	buyer gross      58.25/1000 x 0.00002466 = 1,436.445 nanoCAD  -> 1,436 (down)
//	supplier floor   1,436 x 0.97            = 1,392.92  nanoCAD  -> 1,393 (up)
//
// and the supplier's entitlement must be that same 1,393, because it IS the
// share of the same gross. Admission compares the two; they are one quantity
// computed once, so the comparison is an identity and not a negotiation.
const (
	fixtureUnitsPerTask        = 58.25
	fixtureReferencePricePer1K = 0.0000180
	fixtureFXRate              = 1.37
	fixtureSupplierShare       = 0.97
	fixtureThroughputPerSec    = 1666.2
	fixtureExactGrossNanos     = int64(1436)
	fixtureExactFloorNanos     = int64(1393)
)

func fixtureSettlementPricePer1K() float64 {
	return ceilPricePer1K(fixtureReferencePricePer1K * fixtureFXRate)
}

// This is the authority join, not another parallel pricing calculation. The
// decision must use the settlement-currency catalogue price and the exact
// fractional units once, then freeze the buyer gross and supplier floor produced
// by that same calculation. It distinguishes both an integer-unit rewrite and a
// floor silently derived from the USD reference side of a CAD decision.
func TestExactTaskEconomicsUsesOneSettlementCurrencyAuthority(t *testing.T) {
	authority := CataloguePriceAuthority{
		SettlementCurrency:        "cad",
		ReferencePricePer1K:       fixtureReferencePricePer1K,
		SettlementPricePer1K:      fixtureSettlementPricePer1K(),
		ReferenceToSettlementRate: fixtureFXRate,
		SupplierShare:             fixtureSupplierShare,
	}
	gross, floor, err := exactTaskEconomics(authority, "batch", fixtureUnitsPerTask)
	mustf(t, err, "derive exact task economics: %v")
	if gross.Currency.Code() != "cad" || floor.Currency.Code() != "cad" {
		t.Fatalf("decision crossed currency authority: gross=%s floor=%s",
			gross.Currency.Code(), floor.Currency.Code())
	}
	if gross.Nanos != fixtureExactGrossNanos || floor.Nanos != fixtureExactFloorNanos {
		t.Fatalf("one-authority decision produced gross=%d floor=%d nanos, want %d and %d",
			gross.Nanos, floor.Nanos, fixtureExactGrossNanos, fixtureExactFloorNanos)
	}
	wantFloor, err := SupplierEntitlementNanos(gross, authority.SupplierShare)
	must(t, err)
	if floor != wantFloor {
		t.Fatalf("supplier floor %+v is not the entitlement %+v from the same gross",
			floor, wantFloor)
	}
}

// TestSubSecondTaskFloorIsNotRoundedUpToAWholeSecond is the layer-3 defect.
//
// RequiredTaskNanosFromThroughput divided units by throughput BEFORE scaling to
// nanoseconds, so the intermediate was an integer count of seconds. Every task
// shorter than one second became exactly one second, and the floor it produced
// was the full hourly ceiling divided by 3,600 — independent of how little work
// the task actually contained.
//
// On the three-record fixture that is 29,093 nanos against a true 1,031: the
// "30x gap between quote and catalogue" recorded in the programme ledger was
// mostly this one integer division, not a pricing disagreement at all.
func TestSubSecondTaskFloorIsNotRoundedUpToAWholeSecond(t *testing.T) {
	cad := MustParseCurrency("cad")
	ceiling := expectedSupplierUSDHr(
		fixtureThroughputPerSec, fixtureReferencePricePer1K, fixtureSupplierShare, "batch")
	ceilingNanos, err := MoneyNanosFromUSDFloat(cad, ceiling)
	mustf(t, err, "hourly ceiling to nanos: %v")

	got, err := RequiredTaskNanosFromThroughput(
		cad, NanoUSDPerHour(ceilingNanos.Nanos),
		NanoWorkUnitsFromFloat(fixtureUnitsPerTask),
		NanoUnitsPerSecond(fixtureThroughputPerSec*float64(NanosPerMajorUnit)),
	)
	mustf(t, err, "derive task floor from throughput: %v")

	// One whole second at the ceiling. If the truncation is back, this is what
	// the floor collapses to, and it is 28x the real answer.
	wholeSecond := ceilingNanos.Nanos / 3600
	if got.Nanos >= wholeSecond {
		t.Fatalf("sub-second task floor is %d nanos, at or above the %d nanos a WHOLE "+
			"second at the ceiling costs; the duration was truncated to an integer second",
			got.Nanos, wholeSecond)
	}

	// The closed form: required = units/1000 x price x share, because the
	// throughput cancels between the ceiling and the duration.
	want := fixtureUnitsPerTask / 1000 * fixtureReferencePricePer1K * fixtureSupplierShare
	wantNanos := int64(math.Ceil(want * float64(NanosPerMajorUnit)))
	if got.Nanos < wantNanos || got.Nanos > wantNanos+2 {
		t.Fatalf("task floor %d nanos is not the closed form %d nanos (units/1000 x price x share)",
			got.Nanos, wantNanos)
	}
}

// TestFractionalUnitsAreNotInflatedToWholeUnits pins the second defect.
//
// WorkUnits was an integer, so 58.25 units of input had to be ceiled to 59
// before the floor could be derived — charging the buyer's supplier floor for
// three quarters of a unit nobody bought. It is 1.3% on this fixture and 100% on
// any job below one unit.
func TestFractionalUnitsAreNotInflatedToWholeUnits(t *testing.T) {
	cad := MustParseCurrency("cad")
	price := nanosPer1KFromFloat(fixtureSettlementPricePer1K())

	fractional, err := CatalogueGrossNanos(cad, price, NanoWorkUnitsFromFloat(fixtureUnitsPerTask))
	mustf(t, err, "exact gross for fractional units: %v")
	ceiled, err := CatalogueGrossNanos(cad, price, NanoWorkUnitsFromFloat(math.Ceil(fixtureUnitsPerTask)))
	mustf(t, err, "exact gross for ceiled units: %v")
	if fractional.Nanos != fixtureExactGrossNanos {
		t.Fatalf("exact buyer gross is %d nanos, want %d", fractional.Nanos, fixtureExactGrossNanos)
	}
	if ceiled.Nanos <= fractional.Nanos {
		t.Fatalf("ceiling the units did not inflate the gross (%d vs %d), so this test "+
			"no longer distinguishes the defect", ceiled.Nanos, fractional.Nanos)
	}
}

// TestBuyerGrossIsNotQuantisedToMicrosBeforeTheSupplierShare pins the third.
//
// The quote froze base compute as a micro-USD float. 1,436.445 nanos became
// $0.000001 — 1,000 nanos — a 30.4% haircut taken before the supplier's 97% was
// applied. The supplier was then entitled to 970 nanos against a 1,393 nano
// floor derived from the unrounded catalogue, and admission refused, correctly,
// for a reason nobody had written down.
func TestBuyerGrossIsNotQuantisedToMicrosBeforeTheSupplierShare(t *testing.T) {
	cad := MustParseCurrency("cad")
	price := nanosPer1KFromFloat(fixtureSettlementPricePer1K())

	gross, err := CatalogueGrossNanos(cad, price, NanoWorkUnitsFromFloat(fixtureUnitsPerTask))
	mustf(t, err, "exact gross: %v")
	entitlement, err := SupplierEntitlementNanos(gross, fixtureSupplierShare)
	mustf(t, err, "exact entitlement: %v")
	if entitlement.Nanos != fixtureExactFloorNanos {
		t.Fatalf("exact supplier entitlement is %d nanos, want %d",
			entitlement.Nanos, fixtureExactFloorNanos)
	}

	// What the micro-quantised path produced, for the record: the round-trip
	// through roundUSD costs the supplier 30% of a job this size.
	quantised := roundUSD(gross.USDFloat())
	quantisedNanos, err := MoneyNanosFromUSDFloat(cad, quantised)
	mustf(t, err, "quantised gross: %v")
	quantisedEntitlement, err := SupplierEntitlementNanos(quantisedNanos, fixtureSupplierShare)
	mustf(t, err, "quantised entitlement: %v")
	if quantisedEntitlement.Nanos >= entitlement.Nanos {
		t.Fatalf("micro quantisation no longer loses value (%d vs %d); this fixture has "+
			"stopped exercising the defect and needs a smaller job",
			quantisedEntitlement.Nanos, entitlement.Nanos)
	}
	lossFraction := float64(entitlement.Nanos-quantisedEntitlement.Nanos) / float64(entitlement.Nanos)
	if lossFraction < 0.25 {
		t.Fatalf("expected the recorded ~30%% haircut, measured %.1f%%", lossFraction*100)
	}
}

// TestTheEntitlementAndItsFloorAreOneQuantity is the property the whole layer
// exists to establish, and the reason admission can be an identity rather than a
// tolerance.
//
// Both sides derive from the same catalogue price, the same units and the same
// currency. There is no rounding between them that runs in the supplier's
// disfavour, so the entitlement clears its own floor at every size — including
// the sizes where every previous derivation failed.
func TestTheEntitlementAndItsFloorAreOneQuantity(t *testing.T) {
	cad := MustParseCurrency("cad")
	price := nanosPer1KFromFloat(fixtureSettlementPricePer1K())

	for _, units := range []float64{
		0.25, 1, 3, 58.25, 59, 100.5, 1000, 12345.75, 1e6,
	} {
		gross, err := CatalogueGrossNanos(cad, price, NanoWorkUnitsFromFloat(units))
		mustf(t, err, "%g units: exact gross: %v", units)
		entitlement, err := SupplierEntitlementNanos(gross, fixtureSupplierShare)
		mustf(t, err, "%g units: exact entitlement: %v", units)
		floor, err := SupplierEntitlementNanos(gross, fixtureSupplierShare)
		mustf(t, err, "%g units: exact floor: %v", units)
		ok, err := entitlement.AtLeast(floor)
		mustf(t, err, "%g units: compare: %v", units)
		if !ok {
			t.Fatalf("%g units: entitlement %d nanos is below its own floor %d nanos",
				units, entitlement.Nanos, floor.Nanos)
		}
		if entitlement.Nanos > gross.Nanos {
			t.Fatalf("%g units: supplier entitlement %d exceeds the buyer gross %d it is a share of",
				units, entitlement.Nanos, gross.Nanos)
		}
	}
}
