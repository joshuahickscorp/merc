package main

import (
	"reflect"
	"strings"
	"testing"
)

// TestParsePositiveUSDNanosRefusesMoreThanNineFractionalDigits pins the
// exact-decimal boundary of the buyer USD ceiling parser.
//
// Nanos are 1e-9 major units. A tenth fractional digit has no representable
// place in that lattice: accepting it would either truncate silently (so the
// header and the body can disagree about the same string) or invent a rounding
// policy nobody named. Scientific notation and signs are refused for the same
// reason — one string, one exact integer, nothing else.
func TestParsePositiveUSDNanosRefusesMoreThanNineFractionalDigits(t *testing.T) {
	// Nine fractional digits is the finest representable ceiling and must work.
	got, err := parsePositiveUSDNanos("1.000000001")
	if err != nil || got != 1_000_000_001 {
		t.Fatalf("nine fractional digits = (%d, %v), want 1000000001/nil", got, err)
	}
	got, err = parsePositiveUSDNanos("0.000000001")
	if err != nil || got != 1 {
		t.Fatalf("one nano = (%d, %v), want 1/nil", got, err)
	}

	// A tenth fractional digit is not a nano. Truncating it to nine digits
	// would make "0.0000000001" parse as zero-or-one and "1.0000000001" parse
	// as exactly one major unit — both are silent reinterpretations.
	for _, raw := range []string{
		"1.0000000001",
		"0.0000000001",
		"12.3456789012",
		"0.12345678901",
	} {
		if _, err := parsePositiveUSDNanos(raw); err == nil {
			t.Fatalf("accepted %q, which has more than nine fractional digits", raw)
		} else if !strings.Contains(err.Error(), "9 fractional") &&
			!strings.Contains(err.Error(), "fractional digits") {
			t.Fatalf("refusal for %q does not name the fractional-digit bound: %v", raw, err)
		}
	}
}

// TestConvertRealtimeReferenceRateRefusesCollapsedSettlementRate pins the
// post-condition of FX conversion on a per-million token rate: a settlement
// rate that is not strictly positive is not a priced authority.
//
// A zero reference rate is not a quote. Conversion must refuse it rather than
// emit a zero settlement rate that later multiplies cleanly against token
// counts and looks like free work. The same refusal covers any future
// conversion path that could round a positive reference rate down to zero.
func TestConvertRealtimeReferenceRateRefusesCollapsedSettlementRate(t *testing.T) {
	usd := MustParseCurrency(realtimeReferenceCurrency)
	fx := RealtimeFXAuthority{
		Version:                    realtimeFXAuthorityVersion,
		ReferenceCurrency:          realtimeReferenceCurrency,
		SettlementCurrency:         usd.Code(),
		ReferenceToSettlementRate:  1,
		ReferenceToSettlementNanos: realtimeFXRateScale,
		FXRevision:                 "identity-" + realtimeReferenceCurrency,
		RoundingPolicy:             realtimeFXRoundingPolicy,
	}
	if err := validateRealtimeFXAuthority(fx, usd); err != nil {
		t.Fatalf("identity FX fixture is invalid: %v", err)
	}

	// Positive unit rate survives identity conversion and stays positive.
	converted, err := convertRealtimeReferenceRate(1, usd, fx)
	if err != nil || converted != 1 {
		t.Fatalf("positive unit rate = (%d, %v), want 1/nil", converted, err)
	}

	// Zero is not a settlement rate. Refusing here is what stops a free
	// per-million token price from propagating into PricingDecision.
	if _, err := convertRealtimeReferenceRate(0, usd, fx); err == nil {
		t.Fatal("zero reference rate converted into a settlement token rate")
	} else if !strings.Contains(err.Error(), "collapsed") &&
		!strings.Contains(err.Error(), "positive") {
		t.Fatalf("zero-rate refusal is not explicit: %v", err)
	}
}

func cadRealtimeFXForTest(t *testing.T) RealtimeFXAuthority {
	t.Helper()
	// 1.37 is the governed test CAD factor used across the currency suite.
	// The nano form must leave a remainder so up/down rounding diverge.
	cad := MustParseCurrency("cad")
	fx := RealtimeFXAuthority{
		Version:                    realtimeFXAuthorityVersion,
		ReferenceCurrency:          realtimeReferenceCurrency,
		SettlementCurrency:         cad.Code(),
		ReferenceToSettlementRate:  1.37,
		ReferenceToSettlementNanos: 1_370_000_000,
		FXRevision:                 "test-fx-cad-realtime-authority",
		RoundingPolicy:             realtimeFXRoundingPolicy,
	}
	if err := validateRealtimeFXAuthority(fx, cad); err != nil {
		t.Fatalf("CAD FX fixture is invalid: %v", err)
	}
	return fx
}

// TestRealtimeCeilingConversionRoundsDownNotUp pins the buyer-declared
// ceiling conversion direction. Buyer charges round up so a positive liability
// cannot disappear; ceilings round down so conversion cannot spend past the
// USD cap the buyer actually typed. The two directions must stay opposite.
func TestRealtimeCeilingConversionRoundsDownNotUp(t *testing.T) {
	cad := MustParseCurrency("cad")
	fx := cadRealtimeFXForTest(t)
	// 10001 * 1.37e9 leaves a remainder against 1e9, so floor and ceil differ.
	const referenceCeiling int64 = 10_001
	down, err := convertRealtimeReferenceNanos(referenceCeiling, cad, fx, false)
	must(t, err)
	up, err := convertRealtimeReferenceNanos(referenceCeiling, cad, fx, true)
	must(t, err)
	if down.Nanos >= up.Nanos {
		t.Fatalf("fixture no longer distinguishes ceiling floor/ceil: down=%d up=%d",
			down.Nanos, up.Nanos)
	}
	// The production ceiling path must use the lower bound.
	if down.Nanos != 13_701 || up.Nanos != 13_702 {
		t.Fatalf("unexpected CAD ceiling conversion: down=%d up=%d", down.Nanos, up.Nanos)
	}
}

// TestRealtimeCrossCurrencyFXRefusesIdentityRevision pins the falsehood that
// a non-USD settlement currency can still claim identity FX. Identity is only
// exact when reference and settlement are the same currency at rate 1.
func TestRealtimeCrossCurrencyFXRefusesIdentityRevision(t *testing.T) {
	cad := MustParseCurrency("cad")
	fx := RealtimeFXAuthority{
		Version:                    realtimeFXAuthorityVersion,
		ReferenceCurrency:          realtimeReferenceCurrency,
		SettlementCurrency:         cad.Code(),
		ReferenceToSettlementRate:  1.37,
		ReferenceToSettlementNanos: 1_370_000_000,
		FXRevision:                 "identity-usd",
		RoundingPolicy:             realtimeFXRoundingPolicy,
	}
	err := validateRealtimeFXAuthority(fx, cad)
	if err == nil {
		t.Fatal("cross-currency FX authority was allowed to claim identity")
	}
	if !strings.Contains(err.Error(), "identity") {
		t.Fatalf("refusal does not name the identity falsehood: %v", err)
	}
}

// TestRealtimeIngressRefusesDriftedFrozenFXAuthority pins the new-work
// boundary: a caller-supplied FX snapshot is accepted only when it is exactly
// the current governed authority. Historical replay uses the frozen body and
// never calls this function; new ingress must not silently adopt a stale rate.
func TestRealtimeIngressRefusesDriftedFrozenFXAuthority(t *testing.T) {
	installSettlementCurrencyForTest(t, "cad")
	installRealtimeCADFXForTest(t)
	cad := MustParseCurrency("cad")
	current, err := loadRealtimeFXAuthority(cad)
	must(t, err)

	// Matching the current authority is the only legitimate new-ingress path.
	got, err := realtimeFXForNewIngress(current, cad)
	if err != nil || !reflect.DeepEqual(got, current) {
		t.Fatalf("current governed FX was refused on ingress: got=%+v err=%v", got, err)
	}
	// Empty frozen means "load current" and is how first-time callers attach.
	got, err = realtimeFXForNewIngress(RealtimeFXAuthority{}, cad)
	if err != nil || !reflect.DeepEqual(got, current) {
		t.Fatalf("empty frozen FX did not resolve to current: got=%+v err=%v", got, err)
	}

	drifted := current
	drifted.ReferenceToSettlementRate = 1.99
	drifted.ReferenceToSettlementNanos = 1_990_000_000
	drifted.FXRevision = "later-fx-must-not-reprice-ingress"
	if _, err := realtimeFXForNewIngress(drifted, cad); err == nil {
		t.Fatal("drifted frozen FX authority was accepted for new realtime ingress")
	} else if !strings.Contains(err.Error(), "current governed") {
		t.Fatalf("drift refusal is not explicit: %v", err)
	}
}
