package main

import (
	"bytes"
	"encoding/json"
	"os/exec"
	"strings"
	"testing"
)

type publicPricePageResult struct {
	FetchedPath   string             `json:"fetched_path"`
	Prices        map[string]float64 `json:"prices"`
	Meta          string             `json:"meta"`
	CatalogueRows [][]string         `json:"catalogue_rows"`
}

func publicPricePagePublication(price float64, board *priceBoard) map[string]any {
	return map[string]any{
		"schema_version": 2,
		"price_authority": map[string]any{
			"status":                  "current_revalidated",
			"schedule_sha256":         strings.Repeat("a", 64),
			"schedule_version":        cataloguePriceScheduleVersion,
			"current_use_valid_until": "2099-01-01T00:00:00Z",
			"settlement_currency":     "usd",
			"catalogue": []map[string]any{{
				"model_id":                "all-minilm-l6-v2",
				"job_type":                "embed",
				"schedule_sha256":         strings.Repeat("a", 64),
				"schedule_version":        cataloguePriceScheduleVersion,
				"current_use_valid_until": "2099-01-01T00:00:00Z",
				"settlement_currency":     "usd",
				"settlement_price_per_1k": price,
			}},
		},
		"market_evidence": map[string]any{
			"status":                  "evidence_only_not_buyer_authority",
			"authorizes_buyer_prices": false,
			"sha256":                  strings.Repeat("b", 64),
			"source":                  "test",
			"board":                   board,
		},
	}
}

func renderPublicPricePage(t *testing.T, publication any) publicPricePageResult {
	t.Helper()
	raw, err := json.Marshal(publication)
	mustf(t, err, "encode public price publication: %v")
	cmd := exec.Command("node", "../scripts/price-board-page-prices.mjs")
	cmd.Stdin = bytes.NewReader(raw)
	out, err := cmd.Output()
	mustf(t, err, "run price page script: %v")
	var rendered publicPricePageResult
	if err := json.Unmarshal(out, &rendered); err != nil {
		t.Fatalf("page harness output was not JSON: %v\n%s", err, out)
	}
	return rendered
}

func TestPublicPriceBoardPageAgreesWithTheServerAuthority(t *testing.T) {
	// The evidence would price to a wildly different number if the browser
	// retained its old weighted-median implementation. Only the schedule value
	// may be displayed.
	board := &priceBoard{
		SchemaVersion: 1, Unit: "usd_per_1k_units", FetchedAt: "2026-08-02",
		PositioningMultiplier: 0.9,
		Classes: map[string]priceBoardClass{
			"embed_small": {
				JobType: "embed", ModelIDs: []string{"all-minilm-l6-v2"},
				Observations: []priceBoardObservation{{
					Provider: "evidence", Model: "not-authority", USDPer1K: 99,
					USDPer1M: 99000, SourceURL: "https://evidence.invalid/pricing",
				}},
			},
		},
	}
	const governed = 0.000018
	rendered := renderPublicPricePage(t, publicPricePagePublication(governed, board))
	if rendered.FetchedPath != "/pricing/board.json" {
		t.Fatalf("page fetched %q", rendered.FetchedPath)
	}
	if got := rendered.Prices["all-minilm-l6-v2"]; got != governed {
		t.Fatalf("page price=%v want governed schedule price=%v", got, governed)
	}
	if strings.Contains(strings.Join(flattenStrings(rendered.CatalogueRows), " "), "99000") {
		t.Fatalf("raw evidence price escaped into catalogue rows: %+v", rendered.CatalogueRows)
	}
	if !strings.Contains(rendered.Meta, "observations do not authorize buyer prices") {
		t.Fatalf("page metadata does not label evidence boundary: %q", rendered.Meta)
	}
}

func TestPublicPriceBoardPageShowsUnavailableWithoutCurrentAuthority(t *testing.T) {
	board := &priceBoard{SchemaVersion: 1, Classes: map[string]priceBoardClass{
		"stale": {Observations: []priceBoardObservation{{USDPer1K: 777}}},
	}}
	publication := publicPricePagePublication(0.5, board)
	publication["price_authority"].(map[string]any)["status"] = "expired"
	rendered := renderPublicPricePage(t, publication)
	if len(rendered.Prices) != 0 {
		t.Fatalf("page exposed prices without current authority: %+v", rendered.Prices)
	}
	rows := strings.Join(flattenStrings(rendered.CatalogueRows), " ")
	if !strings.Contains(rows, "current catalogue price authority unavailable") || strings.Contains(rows, "777") {
		t.Fatalf("page did not fail closed, rows=%q", rows)
	}
}

// Every observation must still name who quoted it and where. Observations are
// no longer buyer-price authority, but unattributed evidence is not auditable.
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

func TestPriceBoardWeightingCannotOverrideGovernedSchedule(t *testing.T) {
	governed := 0.000042
	low := &priceBoard{SchemaVersion: 1, Classes: map[string]priceBoardClass{
		"x": {Observations: []priceBoardObservation{{USDPer1K: 0.000001}}},
	}}
	high := &priceBoard{SchemaVersion: 1, Classes: map[string]priceBoardClass{
		"x": {Observations: []priceBoardObservation{{USDPer1K: 1000}}},
	}}
	lowPage := renderPublicPricePage(t, publicPricePagePublication(governed, low))
	highPage := renderPublicPricePage(t, publicPricePagePublication(governed, high))
	if lowPage.Prices["all-minilm-l6-v2"] != governed ||
		highPage.Prices["all-minilm-l6-v2"] != governed {
		t.Fatalf("evidence weighting moved governed price: low=%v high=%v",
			lowPage.Prices, highPage.Prices)
	}
}

func flattenStrings(rows [][]string) []string {
	var out []string
	for _, row := range rows {
		out = append(out, row...)
	}
	return out
}
