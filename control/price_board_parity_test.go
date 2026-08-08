package main

import (
	"bytes"
	"encoding/json"
	"math"
	"os/exec"
	"testing"
)

// The public price board tells visitors it recomputes the confidence-weighted
// median itself, so they can check merc's arithmetic rather than trust it. That
// is only worth anything if the page's answer matches the server's.
//
// Two implementations of the same rule drift silently: someone changes a
// confidence weight in Go, the page keeps the old one, and the board publishes
// a price merc does not charge. A board that disagrees with the prices actually
// billed is worse than no board, because it invites a buyer to reconcile
// against it and conclude they were overcharged.
// pagePricesFor runs the page's own script over a board and returns what it
// would display. A nil board means the shipped one.
func pagePricesFor(t *testing.T, board *priceBoard) map[string]*float64 {
	t.Helper()
	cmd := exec.Command("node", "../scripts/price-board-page-prices.mjs")
	if board != nil {
		raw, err := json.Marshal(board)
		mustf(t, err, "encoding the constructed board: %v")
		cmd.Stdin = bytes.NewReader(raw)
	} else {
		cmd.Stdin = bytes.NewReader(nil)
	}
	out, err := cmd.Output()
	mustf(t, err, "running the page's own pricing script: %v")
	var prices map[string]*float64
	if err := json.Unmarshal(out, &prices); err != nil {
		t.Fatalf("page price output was not JSON: %v\n%s", err, out)
	}
	return prices
}

func TestPublicPriceBoardPageAgreesWithTheServer(t *testing.T) {
	out, err := exec.Command("node", "../scripts/price-board-page-prices.mjs").Output()
	mustf(t, err, "running the page's own pricing script: %v")
	var pagePrices map[string]*float64
	if err := json.Unmarshal(out, &pagePrices); err != nil {
		t.Fatalf("page price output was not JSON: %v\n%s", err, out)
	}

	board, err := loadPriceBoard()
	mustf(t, err, "loading the board: %v")
	if len(board.Classes) == 0 {
		t.Fatal("board has no classes; agreement between two empty sets proves nothing")
	}
	if len(pagePrices) != len(board.Classes) {
		t.Fatalf("page rendered %d classes, board has %d", len(pagePrices), len(board.Classes))
	}

	for name, class := range board.Classes {
		median, _, err := confidenceWeightedMedianUSDPer1K(name, class)
		got, present := pagePrices[name]
		if !present {
			t.Fatalf("class %q is on the board but the page did not render it", name)
		}
		if err != nil {
			// An ungoverned class must be shown as unpriced, not as a number the
			// server would refuse to publish.
			if got != nil {
				t.Fatalf("class %q is ungoverned server-side (%v) but the page showed $%v",
					name, err, *got)
			}
			continue
		}
		want := median * board.PositioningMultiplier
		if got == nil {
			t.Fatalf("class %q prices to $%.8f server-side but the page showed 'not priced'",
				name, want)
		}
		// The page formats to 8 decimals, so compare at that granularity rather
		// than demanding bit equality across two float implementations.
		if math.Abs(*got-want) > 5e-9 {
			t.Fatalf("class %q: page shows $%.8f, server computes $%.8f -- the board "+
				"publishes a price merc does not charge", name, *got, want)
		}
	}
}

// Every observation must name who quoted it and where, because the weighting
// that decides the price is derived from the source host. An unattributed
// observation is not merely undocumented -- it silently receives the lowest
// confidence and can still move the median.
func TestPriceBoardObservationsAreAttributed(t *testing.T) {
	board, err := loadPriceBoard()
	mustf(t, err, "loading the board: %v")
	seen := 0
	for name, class := range board.Classes {
		for i, obs := range class.Observations {
			seen++
			if obs.Provider == "" {
				t.Errorf("%s observation %d names no provider", name, i)
			}
			if obs.SourceURL == "" {
				t.Errorf("%s observation %d (%s) names no source_url", name, i, obs.Provider)
			}
			if obs.USDPer1M <= 0 && obs.USDPer1K <= 0 {
				t.Errorf("%s observation %d (%s) carries no usable price", name, i, obs.Provider)
			}
		}
	}
	if seen == 0 {
		t.Fatal("no observations on the board")
	}
}

// The shipped board has too few observations for the confidence weights to move
// the median, so agreement on it does NOT prove the two implementations weight
// observations the same way -- verified by mutation: changing either side's
// third-party weight leaves the shipped board's prices unchanged.
//
// These boards are built so the weighting decides the answer. A vendor-sourced
// observation carries weight 1.0 and a third-party one 0.35, so a cluster of
// cheap third-party quotes must NOT outvote a single vendor quote; under equal
// weighting it would, and that is the divergence this catches.
func TestPriceBoardWeightingAgreesWhereItActuallyDecides(t *testing.T) {
	obs := func(provider, host string, per1k float64) priceBoardObservation {
		return priceBoardObservation{
			Provider:  provider,
			Model:     provider + "-model",
			USDPer1K:  per1k,
			USDPer1M:  per1k * 1000,
			SourceURL: "https://" + host + "/pricing",
		}
	}
	// These shapes sit AT the crossing point, so the weight decides which
	// observation the median lands on. Verified by mutation: moving either
	// side's third-party weight 0.35 -> 0.9, or its unattributed weight
	// 0.2 -> 1.0, changes the answer and fails this test. The first draft of
	// this test used a 3-cheap-vs-1-expensive cluster, where the weighted
	// median is robust and every weight gave the same answer -- it passed
	// against a page that weighted nothing like the server.
	cases := map[string][]priceBoardObservation{
		// tp 0.35: median lands on the vendor quote (0.010).
		// tp 0.90: it lands on the second third-party quote (0.002).
		"third_party_crossing": {
			obs("acme", "blogaboutai.example", 0.001),
			obs("acme", "newsletter.example", 0.002),
			obs("acme", "acme.example", 0.010),
		},
		// unattributed 0.2: median lands on the vendor quote (0.005).
		// unattributed 1.0: it lands on an anonymous quote (0.0002).
		"unattributed_crossing": {
			{Provider: "", Model: "mystery-a", USDPer1K: 0.0001, USDPer1M: 0.1},
			{Provider: "", Model: "mystery-b", USDPer1K: 0.0002, USDPer1M: 0.2},
			obs("acme", "acme.example", 0.005),
		},
	}

	board := &priceBoard{
		SchemaVersion:         1,
		Unit:                  "usd_per_1k",
		PositioningMultiplier: 0.9,
		Classes:               map[string]priceBoardClass{},
	}
	for name, o := range cases {
		board.Classes[name] = priceBoardClass{
			Description: name, JobType: "embed",
			ModelIDs: []string{"m"}, Observations: o,
		}
	}

	pagePrices := pagePricesFor(t, board)
	checked := 0
	for name, class := range board.Classes {
		median, _, err := confidenceWeightedMedianUSDPer1K(name, class)
		got := pagePrices[name]
		if err != nil {
			if got != nil {
				t.Fatalf("class %q is ungoverned server-side (%v) but the page priced it at $%v",
					name, err, *got)
			}
			continue
		}
		want := median * board.PositioningMultiplier
		if got == nil {
			t.Fatalf("class %q prices to $%.8f server-side, page showed 'not priced'", name, want)
		}
		if math.Abs(*got-want) > 5e-9 {
			t.Fatalf("class %q: page $%.8f vs server $%.8f -- the two implementations "+
				"weight observations differently", name, *got, want)
		}
		checked++
	}
	if checked == 0 {
		t.Fatal("every constructed class was ungoverned; this proved nothing about weighting")
	}
}
