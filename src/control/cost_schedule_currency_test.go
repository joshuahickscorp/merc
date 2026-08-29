package main

import (
	"strings"
	"testing"
)

// TestCostFXSameCurrencyRequiresExactIdentity pins the same-currency cost FX
// lattice: rate 1, nano scale, and revision identity-<currency>. A same-currency
// authority that claims any other rate or revision is not conversion — it is a
// rewritten money unit, and must be refused before schedule arithmetic runs.
func TestCostFXSameCurrencyRequiresExactIdentity(t *testing.T) {
	usd := MustParseCurrency("usd")
	identity := CostFXAuthority{
		Version:                    costFXAuthorityVersion,
		ReferenceCurrency:          costReferenceCurrency,
		SettlementCurrency:         "usd",
		ReferenceToSettlementRate:  1,
		ReferenceToSettlementNanos: costFXRateScale,
		FXRevision:                 "identity-usd",
		RoundingPolicy:             costFXRoundingPolicy,
	}
	if err := validateCostFXAuthority(identity, usd); err != nil {
		t.Fatalf("exact identity cost FX was refused: %v", err)
	}

	forged := identity
	forged.ReferenceToSettlementRate = 1.25
	forged.ReferenceToSettlementNanos = 1_250_000_000
	forged.FXRevision = "forged-same-currency-rate"
	if err := validateCostFXAuthority(forged, usd); err == nil {
		t.Fatal("same-currency cost FX accepted a non-identity rate and revision")
	} else if !strings.Contains(err.Error(), "exact identity") &&
		!strings.Contains(err.Error(), "identity") {
		t.Fatalf("same-currency refusal is not explicit: %v", err)
	}
}

// A currency disagreement is the only failure in validateCostFXAuthority an
// operator can cause by configuration, so it must not read as a version bug.
// This is the exact confusion that made a USD-economics-under-CAD-process
// fixture look like a schema-version problem during step 5.
func TestCostFXCurrencyDisagreementNamesBothCurrencies(t *testing.T) {
	cad := MustParseCurrency("cad")
	usdSettling := CostFXAuthority{
		Version:                    costFXAuthorityVersion,
		ReferenceCurrency:          costReferenceCurrency,
		SettlementCurrency:         "usd",
		ReferenceToSettlementRate:  1,
		ReferenceToSettlementNanos: costFXRateScale,
		FXRevision:                 "identity-usd",
		RoundingPolicy:             costFXRoundingPolicy,
	}
	err := validateCostFXAuthority(usdSettling, cad)
	if err == nil {
		t.Fatal("a usd cost FX authority was accepted under cad settlement")
	}
	if !strings.Contains(err.Error(), "usd") || !strings.Contains(err.Error(), "cad") {
		t.Fatalf("currency disagreement does not name both currencies: %v", err)
	}
	if strings.Contains(err.Error(), "version") {
		t.Fatalf("currency disagreement still reads as a version fault: %v", err)
	}

	// A genuine version fault must still be reported as one, so the split did
	// not simply relabel every failure as a currency problem.
	badVersion := usdSettling
	badVersion.SettlementCurrency = "cad"
	badVersion.FXRevision = "cad-governed-1"
	badVersion.Version = costFXAuthorityVersion + 1
	err = validateCostFXAuthority(badVersion, cad)
	if err == nil || !strings.Contains(err.Error(), "version") {
		t.Fatalf("version fault under matching currency reported as %v", err)
	}
}
