package main

import (
	"errors"
	"strings"
	"testing"
)

// The board shipped with a $0.01/1M observation attributed to Fireworks but
// sourced from a competitor's marketing blog. Under min() that one row set the
// price for every supplier in the class and put the supply side underwater.

func TestVendorOwnedSourcesOutweighHearsay(t *testing.T) {
	vendor := observationConfidence("together", "https://www.together.ai/pricing")
	docs := observationConfidence("voyage", "https://docs.voyageai.com/docs/pricing")
	blog := observationConfidence("fireworks", "https://www.generalcompute.com/blog/inference-api-pricing-guide")
	none := observationConfidence("fireworks", "")

	if vendor <= blog {
		t.Fatalf("a vendor's own price sheet (%.2f) must outweigh a third-party blog (%.2f)", vendor, blog)
	}
	if docs <= blog {
		t.Fatalf("vendor docs subdomain (%.2f) must outweigh a blog (%.2f)", docs, blog)
	}
	if none > blog {
		t.Fatalf("an unattributed observation (%.2f) must not outweigh an attributed one (%.2f)", none, blog)
	}
}

// The core regression: one cheap outlier must not set the class price.
func TestSingleCheapOutlierCannotSetThePrice(t *testing.T) {
	class := priceBoardClass{Observations: []priceBoardObservation{
		{Provider: "fireworks", Model: "a", USDPer1M: 0.01,
			SourceURL: "https://www.generalcompute.com/blog/inference-api-pricing-guide"},
		{Provider: "generalcompute", Model: "b", USDPer1M: 0.04,
			SourceURL: "https://www.generalcompute.com/pricing"},
		{Provider: "together", Model: "c", USDPer1M: 0.06,
			SourceURL: "https://www.together.ai/pricing"},
	}}

	price, chosen, err := confidenceWeightedMedianUSDPer1K("infer_small", class)
	if err != nil {
		t.Fatalf("governed selector refused a well-populated class: %v", err)
	}
	cheapest := 0.01 / 1000.0
	if price <= cheapest {
		t.Fatalf("selector still anchored to the cheap outlier: got %.8f, outlier %.8f", price, cheapest)
	}
	if strings.Contains(chosen.Source, "/blog/") {
		t.Fatalf("a blog post was chosen as the price-setting observation: %s", chosen.Source)
	}
}

// Fail closed: too little evidence means no repricing at all, not a guess.
func TestUngovernedClassRefusesToPrice(t *testing.T) {
	thin := priceBoardClass{Observations: []priceBoardObservation{
		{Provider: "someone", Model: "x", USDPer1M: 0.01, SourceURL: "https://blog.example.com/post"},
	}}
	_, _, err := confidenceWeightedMedianUSDPer1K("thin", thin)
	if err == nil {
		t.Fatal("a single hearsay observation must not be enough to set a public price")
	}
	var ungoverned errBoardUngoverned
	if !errors.As(err, &ungoverned) {
		t.Fatalf("want errBoardUngoverned, got %T", err)
	}

	// Three rows that are all unattributed still fail the confidence floor.
	hearsay := priceBoardClass{Observations: []priceBoardObservation{
		{Provider: "a", Model: "x", USDPer1M: 0.01},
		{Provider: "b", Model: "y", USDPer1M: 0.02},
		{Provider: "c", Model: "z", USDPer1M: 0.03},
	}}
	if _, _, err := confidenceWeightedMedianUSDPer1K("hearsay", hearsay); err == nil {
		t.Fatal("three unattributed observations should not clear the confidence floor")
	}
}

// Item 13: a price that cannot pay its supply side is refused unless an
// operator records a subsidy receipt naming the authorising decision.
func TestNegativeContributionIsRefusedWithoutAReceipt(t *testing.T) {
	t.Setenv(subsidyReceiptEnv, "")
	b := repricingBenchmarks[0]

	// A price low enough that the supplier cannot cover electricity.
	starve := 0.0000000001
	err := governPublishedPrice(b, starve, 0.97)
	if err == nil {
		t.Fatal("a price with negative supplier contribution was allowed to publish")
	}
	var neg errNegativeContribution
	if !errors.As(err, &neg) {
		t.Fatalf("want errNegativeContribution, got %T: %v", err, err)
	}
	if !strings.Contains(err.Error(), subsidyReceiptEnv) {
		t.Fatalf("refusal should name the override mechanism: %v", err)
	}

	// With an explicit receipt the same price is permitted.
	t.Setenv(subsidyReceiptEnv, "DECISION-2026-07-27-canary-subsidy")
	if err := governPublishedPrice(b, starve, 0.97); err != nil {
		t.Fatalf("a receipted subsidy should permit the price: %v", err)
	}

	// A blank-ish receipt is not a receipt.
	t.Setenv(subsidyReceiptEnv, "   ")
	if err := governPublishedPrice(b, starve, 0.97); err == nil {
		t.Fatal("whitespace must not count as a subsidy authorisation")
	}
}

// The shipped board must actually clear the gate: if it does not, the catalogue
// silently stops repricing and nobody notices.
func TestShippedBoardPublishesAViablePrice(t *testing.T) {
	t.Setenv(subsidyReceiptEnv, "")
	results := RepriceCatalogueFromSupplierEconomics(0.97)
	if len(results) == 0 {
		t.Fatal("the shipped price board publishes no prices at all")
	}
	for _, r := range results {
		var b measuredThroughput
		found := false
		for _, cand := range repricingBenchmarks {
			if cand.ModelID == r.ModelID {
				b, found = cand, true
				break
			}
		}
		if !found {
			continue
		}
		m := marginsForPrice(b, r.PricePer1K, 0.97)
		if m.SupplierNetUSDHr <= 0 {
			t.Fatalf("%s publishes at $%.8f/1k leaving the supplier $%.6f/hr — below electricity",
				r.ModelID, r.PricePer1K, m.SupplierNetUSDHr)
		}
		if m.CXGrossUSDHr <= 0 {
			t.Fatalf("%s publishes with non-positive platform contribution $%.6f/hr",
				r.ModelID, m.CXGrossUSDHr)
		}
	}
}
