package main

import (
	"errors"
	"math"
	"testing"
)

func usd(t *testing.T) Currency { t.Helper(); return MustParseCurrency("usd") }
func cad(t *testing.T) Currency { t.Helper(); return MustParseCurrency("cad") }

func nanos(t *testing.T, c Currency, n int64) MoneyNanos {
	t.Helper()
	m, err := NewMoneyNanos(c, n)
	if err != nil {
		t.Fatalf("construct %d nanos: %v", n, err)
	}
	return m
}

// The exact defect that forced this layer, reproduced as arithmetic.
//
// Admission refused a real job because it compared an hourly ceiling derived in
// continuous dollars against an hourly gross RECONSTRUCTED from a per-task payout
// that had already been rounded to micro-USD:
//
//	modeled supplier gross 0.102978 USD/hr
//	admission ceiling      0.104733 USD/hr
//
// The gap is 1.676% and it is one lost micro-USD amplified by dividing a rounded
// amount by a sub-second duration. Compared exactly, per task, in nanos, the same
// entitlement clears the same floor.
func TestTheAdmissionDefectDisappearsUnderExactComparison(t *testing.T) {
	c := usd(t)

	// The shape that produced it: a three-record embed at the cohort's measured
	// throughput, against a supplier floor of $0.104733/hr.
	const floor = NanoUSDPerHour(104_733_000) // $0.104733/hr in nanos
	// 3 units at 1,194.46 units/sec — the candle batch-32 figure from the retained
	// bench receipt — is ~2.5ms of work.
	const throughput = NanoUnitsPerSecond(1_194_460_000_000)
	required, err := RequiredTaskNanosFromThroughput(
		c, floor, NanoWorkUnitsFromFloat(3), throughput)
	if err != nil {
		t.Fatalf("derive the exact task floor: %v", err)
	}

	// The exact entitlement the plan should freeze is the same derivation. Frozen
	// exactly, it clears its own floor — which is the property the old comparison
	// could not have, because it rounded one side and not the other.
	frozen := required
	ok, err := frozen.AtLeast(required)
	if err != nil {
		t.Fatal(err)
	}
	if !ok {
		t.Fatalf("an exactly-frozen entitlement failed its own floor: %s < %s",
			frozen, required)
	}

	// Now the old path: round the entitlement to micro-USD, then reconstruct an
	// hourly rate from it. That is what produced 0.102978 against 0.104733.
	roundedDownMicros := required.Nanos / NanosPerMicro
	rounded := nanos(t, c, roundedDownMicros*NanosPerMicro)
	if rounded.Nanos == required.Nanos {
		t.Skip("this fixture no longer loses a fraction of a micro; the amplification " +
			"case needs a smaller job to stay meaningful")
	}
	stillClears, err := rounded.AtLeast(required)
	if err != nil {
		t.Fatal(err)
	}
	if stillClears {
		t.Fatal("rounding down did not reduce the entitlement; the fixture proves nothing")
	}
	// And the loss, expressed hourly, is enormous relative to the rate — which is
	// the amplification the old comparison suffered.
	lost := required.Nanos - rounded.Nanos
	t.Logf("rounding to micro-USD lost %d nanos of a %d-nano entitlement (%.3f%%), "+
		"which is what a reverse-hourly comparison amplified into a 1.676%% shortfall",
		lost, required.Nanos, float64(lost)/float64(required.Nanos)*100)
}

// A supplier floor rounds UP and a buyer charge rounds DOWN. Getting either
// direction wrong underpays a supplier or overcharges a buyer by a fraction that
// nobody notices per task.
func TestRoundingDirectionFavoursNeitherSideByAccident(t *testing.T) {
	c := usd(t)

	// One nanosecond at $1/hr is 1/3.6e12 of a nano — a true fraction. The floor
	// must still be at least one nano, because a supplier owed a positive amount
	// must not be owed zero.
	required, err := RequiredTaskNanosFromHourlyFloor(c, NanoUSDPerHour(NanosPerMajorUnit), 1)
	if err != nil {
		t.Fatal(err)
	}
	if required.Nanos != 1 {
		t.Fatalf("a positive-but-tiny supplier floor rounded to %d nanos, want 1",
			required.Nanos)
	}

	// The buyer side of the same fraction rounds the other way: a price of one nano
	// per 1,000 units over one unit is a thousandth of a nano, and the buyer is not
	// charged for it.
	charge, err := CatalogueGrossNanos(c, NanoUSDPerThousandUnits(1), NanoWorkUnitsFromFloat(1))
	if err != nil {
		t.Fatal(err)
	}
	if charge.Nanos != 0 {
		t.Fatalf("a sub-nano charge rounded up to %d nanos; the buyer paid for "+
			"work not delivered", charge.Nanos)
	}

	// Exactly-divisible cases must not drift in either direction.
	exact, err := CatalogueGrossNanos(c, NanoUSDPerThousandUnits(2_000), NanoWorkUnitsFromFloat(500))
	if err != nil {
		t.Fatal(err)
	}
	if exact.Nanos != 1_000 {
		t.Fatalf("exact charge = %d nanos, want 1000", exact.Nanos)
	}
}

// Boundaries around the micro-USD posting granularity, one nano at a time.
func TestRemainderCarryPostsOnWholeMicrosAndKeepsTheRest(t *testing.T) {
	c := usd(t)
	for _, tc := range []struct {
		name        string
		accrue      int64
		wantPosted  int64
		wantCarried int64
	}{
		{"less than one micro posts nothing and keeps everything", 999, 0, 999},
		{"exactly one micro posts one and keeps nothing", 1_000, 1, 0},
		{"one nano below one micro", 999, 0, 999},
		{"one nano above one micro", 1_001, 1, 1},
		{"exactly two micros", 2_000, 2, 0},
		{"zero accrues nothing", 0, 0, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			carry, err := NewRemainderCarry(c, 0)
			if err != nil {
				t.Fatal(err)
			}
			posted, err := carry.Accrue(nanos(t, c, tc.accrue))
			if err != nil {
				t.Fatal(err)
			}
			if posted != tc.wantPosted || carry.RemainderNanos != tc.wantCarried {
				t.Fatalf("posted %d carried %d, want %d and %d",
					posted, carry.RemainderNanos, tc.wantPosted, tc.wantCarried)
			}
			// The invariant, on every case.
			if got := carry.ExactAccrued(); got != tc.accrue {
				t.Fatalf("exact accrued %d, but %d was accrued: value was created or lost",
					got, tc.accrue)
			}
		})
	}
}

// The case the carry exists for: many amounts each too small to post, which under
// the old arithmetic each rounded to zero and vanished.
func TestManyTinyAccrualsLoseNothing(t *testing.T) {
	c := usd(t)
	carry, err := NewRemainderCarry(c, 0)
	if err != nil {
		t.Fatal(err)
	}
	const each = int64(17) // 17 nanos: 0.017 of a micro, invisible to the ledger
	const times = 10_000
	var totalPosted int64
	for i := 0; i < times; i++ {
		posted, err := carry.Accrue(nanos(t, c, each))
		if err != nil {
			t.Fatalf("accrual %d: %v", i, err)
		}
		totalPosted += posted
	}
	exact := each * times
	if carry.ExactAccrued() != exact {
		t.Fatalf("exact accrued %d over %d accruals of %d, want %d",
			carry.ExactAccrued(), times, each, exact)
	}
	if totalPosted != exact/NanosPerMicro {
		t.Fatalf("posted %d micros, want %d", totalPosted, exact/NanosPerMicro)
	}
	// Under micro-only arithmetic every one of these rounded to zero and the
	// supplier earned nothing at all.
	if totalPosted == 0 {
		t.Fatal("ten thousand accruals posted nothing")
	}
	t.Logf("%d accruals of %d nanos: %d micros posted, %d nanos still carried",
		times, each, totalPosted, carry.RemainderNanos)
}

// Conservation across a whole cohort of small jobs: buyer = supplier + Merc.
func TestConservationHoldsExactlyAcrossManySmallJobs(t *testing.T) {
	c := usd(t)
	supplier, err := NewRemainderCarry(c, 0)
	if err != nil {
		t.Fatal(err)
	}
	platform, err := NewRemainderCarry(c, 0)
	if err != nil {
		t.Fatal(err)
	}
	var buyerExact int64
	for i := 1; i <= 5_000; i++ {
		// A charge that is deliberately not divisible by anything convenient.
		charge := nanos(t, c, int64(i)*7+13)
		// 97% to the supplier, exactly, with the remainder to the platform — split
		// by integer arithmetic so nothing is invented.
		supplierNanos, err := mulDiv(charge.Nanos, 97, 100, false)
		if err != nil {
			t.Fatal(err)
		}
		platformNanos := charge.Nanos - supplierNanos
		if _, err := supplier.Accrue(nanos(t, c, supplierNanos)); err != nil {
			t.Fatal(err)
		}
		if _, err := platform.Accrue(nanos(t, c, platformNanos)); err != nil {
			t.Fatal(err)
		}
		buyerExact += charge.Nanos
	}
	if got := supplier.ExactAccrued() + platform.ExactAccrued(); got != buyerExact {
		t.Fatalf("conservation broken: buyer %d != supplier %d + platform %d",
			buyerExact, supplier.ExactAccrued(), platform.ExactAccrued())
	}
	t.Logf("5,000 jobs: buyer %d nanos = supplier %d + platform %d, "+
		"posted %d/%d micros with %d/%d nanos carried",
		buyerExact, supplier.ExactAccrued(), platform.ExactAccrued(),
		supplier.PostedMicros, platform.PostedMicros,
		supplier.RemainderNanos, platform.RemainderNanos)
}

// Currencies never mix, in any operation.
func TestExactMoneyRefusesToMixCurrencies(t *testing.T) {
	u, ca := nanos(t, usd(t), 1_000), nanos(t, cad(t), 1_000)
	if _, err := u.Add(ca); !errors.Is(err, errMoneyCurrencyMismatch) {
		t.Fatalf("Add across currencies: %v", err)
	}
	if _, err := u.Sub(ca); !errors.Is(err, errMoneyCurrencyMismatch) {
		t.Fatalf("Sub across currencies: %v", err)
	}
	if _, err := u.AtLeast(ca); !errors.Is(err, errMoneyCurrencyMismatch) {
		t.Fatalf("AtLeast across currencies: %v", err)
	}
	if _, err := u.AtMost(ca); !errors.Is(err, errMoneyCurrencyMismatch) {
		t.Fatalf("AtMost across currencies: %v", err)
	}
	carry, err := NewRemainderCarry(usd(t), 0)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := carry.Accrue(ca); !errors.Is(err, errMoneyCurrencyMismatch) {
		t.Fatalf("accruing CAD into a USD carry: %v", err)
	}
	// And an amount with no currency at all is not constructible.
	if _, err := NewMoneyNanos(Currency{}, 1); err == nil {
		t.Fatal("constructed an amount with no currency")
	}
}

// Overflow is an error, never a wrap. A wrapped int64 reports that value was
// created, which is the one thing a conservation invariant must never say.
func TestExactMoneyRefusesToOverflowInsteadOfWrapping(t *testing.T) {
	c := usd(t)
	big1 := nanos(t, c, math.MaxInt64)
	if _, err := big1.Add(nanos(t, c, 1)); !errors.Is(err, errMoneyOverflow) {
		t.Fatalf("addition overflow: %v", err)
	}
	small := nanos(t, c, math.MinInt64)
	if _, err := small.Sub(nanos(t, c, 1)); !errors.Is(err, errMoneyOverflow) {
		t.Fatalf("subtraction overflow: %v", err)
	}
	// A rate times a duration is where the real overflow risk lives: this product
	// exceeds int64 before the division brings it back, which is why mulDiv works
	// in big.Int.
	if _, err := RequiredTaskNanosFromHourlyFloor(
		c, NanoUSDPerHour(math.MaxInt64), DurationNanos(math.MaxInt64),
	); !errors.Is(err, errMoneyOverflow) {
		t.Fatalf("rate x duration overflow: %v", err)
	}
	// But a large-yet-real rate over a long duration must still compute: $1,000/hr
	// for a full day is $24,000, nowhere near the int64 ceiling.
	day := DurationNanos(24 * 3600 * NanosPerMajorUnit)
	got, err := RequiredTaskNanosFromHourlyFloor(c, NanoUSDPerHour(1_000*NanosPerMajorUnit), day)
	if err != nil {
		t.Fatalf("a realistic large amount overflowed: %v", err)
	}
	if want := int64(24_000) * NanosPerMajorUnit; got.Nanos != want {
		t.Fatalf("$1000/hr for a day = %d nanos, want %d", got.Nanos, want)
	}
}

// Degenerate inputs are refused rather than producing a plausible number.
func TestExactDerivationsRefuseDegenerateInputs(t *testing.T) {
	c := usd(t)
	if _, err := RequiredTaskNanosFromThroughput(c, 1_000, NanoWorkUnitsFromFloat(10), 0); err == nil {
		t.Fatal("derived a floor from zero throughput")
	}
	if _, err := RequiredTaskNanosFromThroughput(c, 1_000, NanoWorkUnitsFromFloat(10), -5); err == nil {
		t.Fatal("derived a floor from negative throughput")
	}
	if _, err := RequiredTaskNanosFromHourlyFloor(c, -1, 1_000); err == nil {
		t.Fatal("derived a floor from a negative hourly minimum")
	}
	if _, err := RequiredTaskNanosFromHourlyFloor(c, 1_000, -1); err == nil {
		t.Fatal("derived a floor from a negative duration")
	}
	if _, err := CatalogueGrossNanos(c, -1, NanoWorkUnitsFromFloat(10)); err == nil {
		t.Fatal("charged a negative catalogue price")
	}
	// Zero work is not an error — it is zero money.
	zero, err := RequiredTaskNanosFromHourlyFloor(c, 1_000, 0)
	if err != nil {
		t.Fatalf("zero duration: %v", err)
	}
	if !zero.IsZero() {
		t.Fatalf("zero work owes %s", zero)
	}
	// A remainder at or above one whole micro is a micro that was never posted.
	if _, err := NewRemainderCarry(c, NanosPerMicro); err == nil {
		t.Fatal("accepted an opening remainder of a whole micro")
	}
	if _, err := NewRemainderCarry(c, -1); err == nil {
		t.Fatal("accepted a negative opening remainder")
	}
}

// The two derivations of a task floor must agree when handed the same physics.
// Maintaining two authorities that disagree is precisely the defect this replaces.
func TestBothTaskFloorDerivationsAgree(t *testing.T) {
	c := usd(t)
	const floor = NanoUSDPerHour(500_000_000) // $0.50/hr
	const units = NanoWorkUnits(1_000 * NanosPerMajorUnit)
	const throughput = NanoUnitsPerSecond(250 * NanosPerMajorUnit) // 250 units/sec

	viaThroughput, err := RequiredTaskNanosFromThroughput(c, floor, units, throughput)
	if err != nil {
		t.Fatal(err)
	}
	// 1,000 units at 250/sec is exactly 4 seconds.
	viaDuration, err := RequiredTaskNanosFromHourlyFloor(
		c, floor, DurationNanos(4*NanosPerMajorUnit))
	if err != nil {
		t.Fatal(err)
	}
	if viaThroughput.Nanos != viaDuration.Nanos {
		t.Fatalf("the two derivations disagree: %s via throughput, %s via duration",
			viaThroughput, viaDuration)
	}
	// $0.50/hr for 4 seconds is $0.000555…, and the floor rounds up.
	if want := int64(555_556); viaDuration.Nanos != want {
		t.Fatalf("4 seconds at $0.50/hr = %d nanos, want %d", viaDuration.Nanos, want)
	}
}

// The float64 seam is explicitly a seam: it exists for legacy values and it says
// so, and it must not silently accept a non-finite one.
func TestLegacyFloatConversionIsGuarded(t *testing.T) {
	c := usd(t)
	got, err := MoneyNanosFromUSDFloat(c, 0.000194)
	if err != nil {
		t.Fatal(err)
	}
	if got.Nanos != 194_000 {
		t.Fatalf("0.000194 USD = %d nanos, want 194000", got.Nanos)
	}
	for _, bad := range []float64{math.NaN(), math.Inf(1), math.Inf(-1)} {
		if _, err := MoneyNanosFromUSDFloat(c, bad); err == nil {
			t.Fatalf("converted %v into an exact amount", bad)
		}
	}
	// Round-tripping a legacy micro-USD amount must land on a whole nano boundary,
	// or the migration would introduce drift of its own.
	for _, micro := range []int64{1, 194, 999, 1_000, 123_456} {
		usdFloat := float64(micro) / 1e6
		exact, err := MoneyNanosFromUSDFloat(c, usdFloat)
		if err != nil {
			t.Fatal(err)
		}
		if exact.Nanos != micro*NanosPerMicro {
			t.Fatalf("legacy %d micro-USD converted to %d nanos, want %d",
				micro, exact.Nanos, micro*NanosPerMicro)
		}
	}
}

// Every plan and receipt written by this authority names the arithmetic it used,
// so a historical row is never re-read under later rules.
func TestRoundingPolicyIsNamed(t *testing.T) {
	carry, err := NewRemainderCarry(usd(t), 0)
	if err != nil {
		t.Fatal(err)
	}
	if carry.RoundingPolicy() != economicRoundingPolicy {
		t.Fatalf("carry policy %q, want %q", carry.RoundingPolicy(), economicRoundingPolicy)
	}
	if economicRoundingPolicy != "economic-nanos-v1" {
		t.Fatalf("the policy name changed to %q; historical receipts reference the old "+
			"one and a rename silently reinterprets them", economicRoundingPolicy)
	}
}

func TestRealtimeTokenMoneyMultipliesBeforeDivisionInCurrencyBoundNanos(t *testing.T) {
	c := cad(t)
	input, err := nanoRatePerMillionFromFloat(0.50)
	if err != nil {
		t.Fatal(err)
	}
	output, err := nanoRatePerMillionFromFloat(1.25)
	if err != nil {
		t.Fatal(err)
	}
	charge, err := BuyerRealtimeTokenChargeNanos(c, 3, 5, input, output)
	if err != nil {
		t.Fatal(err)
	}
	if charge.Currency.Code() != "cad" || charge.Nanos != 7_750 {
		t.Fatalf("realtime charge = %s, want 7750 nano-cad", charge)
	}
}

func TestRealtimeBuyerAndSupplierRoundingDirectionsAreOpposite(t *testing.T) {
	c := cad(t)
	oneNanoPerMillion, err := nanoRatePerMillionFromFloat(0.000000001)
	if err != nil {
		t.Fatal(err)
	}
	buyer, err := BuyerRealtimeTokenChargeNanos(c, 1, 0, oneNanoPerMillion, 0)
	if err != nil {
		t.Fatal(err)
	}
	supplier, err := SupplierRealtimeTokenEntitlementNanos(c, 1, 0, oneNanoPerMillion, 0)
	if err != nil {
		t.Fatal(err)
	}
	if buyer.Nanos != 0 || supplier.Nanos != 1 {
		t.Fatalf("buyer/supplier directions = %d/%d nanos, want 0/1", buyer.Nanos, supplier.Nanos)
	}
}

func TestRealtimeLedgerProjectionOccursAfterTokenClassesAreCombined(t *testing.T) {
	c := cad(t)
	amount, err := NewMoneyNanos(c, 800)
	if err != nil {
		t.Fatal(err)
	}
	micros, err := LedgerMicrosFromNanos(amount)
	if err != nil {
		t.Fatal(err)
	}
	if micros != 1 {
		t.Fatalf("800 combined nanos projected to %d micros, want 1", micros)
	}
	zero, err := LedgerMicrosFromNanos(nanos(t, c, 0))
	if err != nil || zero != 0 {
		t.Fatalf("zero projected to (%d,%v), want 0", zero, err)
	}
}

func TestRealtimeTokenMoneyRejectsInvalidShape(t *testing.T) {
	if _, err := nanoRatePerMillionFromFloat(math.NaN()); err == nil {
		t.Fatal("NaN realtime rate passed")
	}
	if _, err := BuyerRealtimeTokenChargeNanos(cad(t), -1, 0, 1, 1); err == nil {
		t.Fatal("negative token count passed")
	}
}
