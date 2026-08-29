package main

import (
	"math"
	"strings"
	"testing"
)

func TestProviderCostRefusesNonUSDCurrencyWithoutFrozenFXAuthority(t *testing.T) {
	_, _, _, _, pricing := distributedPricingFixture(t)
	broken := pricing.Catalogue
	broken.SettlementCurrency = "cad"
	broken.ReferenceToSettlementRate = 0
	broken.FXRevision = ""
	rate := providerRatesByHWClass["nvidia_48gb"]
	rate.AuthorityStatus = providerRateAuthorityGoverned
	rate.Provenance = "test-only exact A40 allocation authority"

	if _, _, err := providerRateInSettlementCurrency(rate, broken); err == nil ||
		!strings.Contains(strings.ToLower(err.Error()), "frozen") {
		t.Fatalf("provider USD rate was relabelled as CAD without frozen FX: %v", err)
	}
}

func TestProviderCostUsesFrozenReferenceToSettlementFXAuthority(t *testing.T) {
	workload, _, _, economic, _, _ := trueNetDistributedFixture(t)
	catalogue := catalogueAuthorityFixture(
		t, workload, "cad", economic.Input.SupplierShare,
	)
	rate := providerRatesByHWClass["nvidia_48gb"]
	rate.AuthorityStatus = providerRateAuthorityGoverned
	rate.Provenance = "test-only exact A40 allocation authority"
	converted, basis, err := providerRateInSettlementCurrency(rate, catalogue)
	must(t, err)
	if converted <= 0 {
		t.Fatalf("provider rate did not use valid frozen CAD FX authority: %v", converted)
	}
	wantNanos, err := providerCostNanos(
		rate.CostPerHrUSD*catalogue.ReferenceToSettlementRate, 60,
	)
	must(t, err)
	want := nanosToEconomicUSD(wantNanos)
	gotNanos, err := providerCostNanos(converted, 60)
	must(t, err)
	if math.Abs(nanosToEconomicUSD(gotNanos)-want) > 1e-12 {
		t.Fatalf("converted provider cost=%v, want %v CAD", nanosToEconomicUSD(gotNanos), want)
	}
	if !strings.Contains(basis, catalogue.FXRevision) ||
		!strings.Contains(basis, catalogue.ScheduleSHA256) {
		t.Fatalf("converted provider rate does not bind frozen FX provenance: %s", basis)
	}
}

func TestGeneric48GBClassCannotInheritA40ProviderPrice(t *testing.T) {
	_, _, _, _, pricing := distributedPricingFixture(t)
	component := providerCostComponentForPlacement(
		"vllm-cuda-llama1-infer", []string{"nvidia_48gb"}, 60, pricing.Catalogue,
	)
	if component.Status != pricingCostUnknown || component.Amount != 0 {
		t.Fatalf("generic 48GB placement inherited an A40 provider price: %+v", component)
	}
	for _, phrase := range []string{"unbound", "A40", "GPU SKU"} {
		if !strings.Contains(strings.ToLower(component.Basis), strings.ToLower(phrase)) {
			t.Fatalf("provider refusal does not name %q: %s", phrase, component.Basis)
		}
	}
}
