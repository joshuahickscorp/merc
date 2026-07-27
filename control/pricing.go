package main

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"math"
	"os"
	"path/filepath"
	"runtime"
	"sync"
)

type measuredThroughput struct {
	ModelID        string
	JobType        string
	UnitsPerSec    float64 // tok/s or eps  -  one unit for one catalogue price, same as estimateJobUSD
	HWClass        string  // apple_silicon_pro (the only measured reference box; see GPU_CAPABILITY.md)
	SourceCitation string
}

var repricingBenchmarks = []measuredThroughput{
	{
		ModelID:        "all-minilm-l6-v2",
		JobType:        "embed",
		UnitsPerSec:    1967.3141,
		HWClass:        "apple_silicon_pro",
		SourceCitation: "evidence/benchmarks/2026-07-01-m3-pro.json#embed",
	},
	{
		ModelID:        "llama-3.2-1b-instruct-q4",
		JobType:        "batch_infer",
		UnitsPerSec:    138.7,
		HWClass:        "apple_silicon_pro",
		SourceCitation: "evidence/benchmarks/2026-07-01-m3-pro.json#batch_infer",
	},
}

var sustainedWattsByHWClass = map[string]float64{
	"apple_silicon_base":  20.0,
	"apple_silicon_pro":   30.0,
	"apple_silicon_max":   45.0,
	"apple_silicon_ultra": 65.0,
	// CUDA sustained draw under inference load, board power. These are an order
	// of magnitude above Apple Silicon, which is what makes the supplier
	// break-even arithmetic completely different on CUDA supply.
	"nvidia_24gb":  350.0,
	"nvidia_48gb":  300.0,
	"nvidia_80gb":  400.0,
	"nvidia_180gb": 1000.0,
	"cpu":          25.0,
}

const defaultElectricityUSDPerKWh = 0.15

// targetSupplierUSDHr is the cost-plus supplier revenue target used only for
// the diagnostic floor (repriceFromSupplierEconomics). Catalogue prices come
// from pricing/board.json (market board × positioning_multiplier).
const targetSupplierUSDHr = 2.0

type RepriceResult struct {
	ModelID    string
	JobType    string
	PricePer1K float64
	Formula    string // human-readable, cites every real input (proof artifact)
}

// repriceFromSupplierEconomics is the cost-plus diagnostic: target supplier
// $/hr on the slowest measured laptop class. Kept for unit tests and for the
// market-gap report; it no longer drives the live catalogue.
func repriceFromSupplierEconomics(b measuredThroughput, supplierShare, electricityUSDPerKWh float64) RepriceResult {
	watts := sustainedWattsByHWClass[b.HWClass]
	if watts <= 0 {
		watts = 30.0 // conservative apple_silicon_pro-equivalent default, never zero
	}
	electricityUSDHr := watts / 1000.0 * electricityUSDPerKWh
	unitsPerHr := b.UnitsPerSec * 3600.0

	denom := unitsPerHr / 1000.0 * supplierShare
	var price float64
	if denom > 0 {
		price = (targetSupplierUSDHr + electricityUSDHr) / denom
	}

	formula := fmt.Sprintf(
		"price_per_1k = (target_supplier_usd_hr=%.2f + electricity_usd_hr=%.4f) / (units_per_hr=%.1f/1000 * supplier_share=%.4f) = %.8f  [source: %s, hw_class=%s, %.0fW @ $%.2f/kWh, platform_take=%.2f%%]",
		targetSupplierUSDHr, electricityUSDHr, unitsPerHr, supplierShare, price,
		b.SourceCitation, b.HWClass, watts, electricityUSDPerKWh, (1-supplierShare)*100,
	)
	return RepriceResult{ModelID: b.ModelID, JobType: b.JobType, PricePer1K: price, Formula: formula}
}

// --- market board -----------------------------------------------------------

type priceBoardObservation struct {
	Provider  string  `json:"provider"`
	Model     string  `json:"model"`
	USDPer1K  float64 `json:"usd_per_1k"`
	USDPer1M  float64 `json:"usd_per_1m"`
	SourceURL string  `json:"source_url"`
	FetchedAt string  `json:"fetched_at"`
	Notes     string  `json:"notes"`
}

type priceBoardClass struct {
	Description  string                  `json:"description"`
	JobType      string                  `json:"job_type"`
	ModelIDs     []string                `json:"model_ids"`
	Observations []priceBoardObservation `json:"observations"`
}

type priceBoard struct {
	SchemaVersion         int                        `json:"schema_version"`
	Unit                  string                     `json:"unit"`
	FetchedAt             string                     `json:"fetched_at"`
	PositioningMultiplier float64                    `json:"positioning_multiplier"`
	Classes               map[string]priceBoardClass `json:"classes"`
}

var (
	priceBoardOnce   sync.Once
	priceBoardCached *priceBoard
	priceBoardErr    error
)

func defaultPriceBoardPath() string {
	// Prefer env override, then repo-relative from the running binary / CWD,
	// then a path resolved from this source file (works under `go test`).
	if p := os.Getenv("MERC_PRICE_BOARD"); p != "" {
		return p
	}
	candidates := []string{
		"pricing/board.json",
		"../pricing/board.json",
	}
	if _, file, _, ok := runtime.Caller(0); ok {
		candidates = append(candidates, filepath.Join(filepath.Dir(file), "..", "pricing", "board.json"))
	}
	for _, c := range candidates {
		if st, err := os.Stat(c); err == nil && !st.IsDir() {
			return c
		}
	}
	return "pricing/board.json"
}

func loadPriceBoard() (*priceBoard, error) {
	priceBoardOnce.Do(func() {
		path := defaultPriceBoardPath()
		raw, err := os.ReadFile(path)
		if err != nil {
			priceBoardErr = fmt.Errorf("read price board %s: %w", path, err)
			return
		}
		var b priceBoard
		if err := json.Unmarshal(raw, &b); err != nil {
			priceBoardErr = fmt.Errorf("parse price board %s: %w", path, err)
			return
		}
		if b.PositioningMultiplier <= 0 || math.IsNaN(b.PositioningMultiplier) || math.IsInf(b.PositioningMultiplier, 0) {
			priceBoardErr = fmt.Errorf("price board positioning_multiplier must be finite and > 0, got %v", b.PositioningMultiplier)
			return
		}
		if len(b.Classes) == 0 {
			priceBoardErr = fmt.Errorf("price board has no classes")
			return
		}
		priceBoardCached = &b
	})
	return priceBoardCached, priceBoardErr
}

// minBoardPriceUSDPer1K returns the lowest positive observed usd_per_1k in class.
func minBoardPriceUSDPer1K(class priceBoardClass) (min float64, provider, model, source string, ok bool) {
	min = math.Inf(1)
	for _, obs := range class.Observations {
		p := obs.USDPer1K
		if p <= 0 && obs.USDPer1M > 0 {
			p = obs.USDPer1M / 1000.0
		}
		if p <= 0 || math.IsNaN(p) || math.IsInf(p, 0) {
			continue
		}
		if p < min {
			min = p
			provider = obs.Provider
			model = obs.Model
			source = obs.SourceURL
			ok = true
		}
	}
	return min, provider, model, source, ok
}

// repriceFromMarketBoard prices one catalogue model as
// min(board observations for its class) × positioning_multiplier.
func repriceFromMarketBoard(modelID, jobType string, board *priceBoard) (RepriceResult, bool) {
	for className, class := range board.Classes {
		match := false
		for _, id := range class.ModelIDs {
			if id == modelID {
				match = true
				break
			}
		}
		if !match {
			// Fall back to job_type match only when model_ids is empty.
			if len(class.ModelIDs) == 0 && class.JobType == jobType {
				match = true
			}
		}
		if !match {
			continue
		}
		minPx, chosen, gerr := confidenceWeightedMedianUSDPer1K(className, class)
		if gerr != nil {
			// Ungoverned class: leave the existing catalogue price alone rather
			// than replacing it with something the evidence cannot support.
			continue
		}
		provider, model, source := chosen.Provider, chosen.Model, chosen.Source
		price := minPx * board.PositioningMultiplier
		formula := fmt.Sprintf(
			"price_per_1k = confidence_weighted_median(board[%s])×positioning_multiplier = %.8f×%.4f = %.8f  [median_obs=%s/%s source=%s fetched_at=%s]",
			className, minPx, board.PositioningMultiplier, price, provider, model, source, board.FetchedAt,
		)
		jt := jobType
		if class.JobType != "" {
			jt = class.JobType
		}
		return RepriceResult{ModelID: modelID, JobType: jt, PricePer1K: price, Formula: formula}, true
	}
	return RepriceResult{}, false
}

// RepriceCatalogueFromSupplierEconomics keeps its historical name (call sites
// and tests) but now drives catalogue prices from pricing/board.json:
// min(observed competitor price) × positioning_multiplier. Unlisted models are
// omitted. The supplierShare argument is ignored for pricing (retained so
// existing call signatures and tests compile); cost-plus math lives in
// repriceFromSupplierEconomics for diagnostics.
func RepriceCatalogueFromSupplierEconomics(supplierShare float64) []RepriceResult {
	board, err := loadPriceBoard()
	if err != nil {
		// Fail closed to empty catalogue update rather than inventing prices.
		return nil
	}
	out := make([]RepriceResult, 0, len(repricingBenchmarks))
	for _, b := range repricingBenchmarks {
		r, ok := repriceFromMarketBoard(b.ModelID, b.JobType, board)
		if !ok {
			continue
		}
		// A price that cannot pay its own supply side is not published without
		// a receipted subsidy. Dropping it leaves the previous catalogue price
		// in place, which is the conservative outcome.
		if gerr := governPublishedPrice(b, r.PricePer1K, supplierShare); gerr != nil {
			log.Printf("catalogue price rejected: %v", gerr)
			continue
		}
		out = append(out, r)
	}
	return out
}

// CostFloorVsMarket reports, for each measured model, the cost-plus floor
// versus the market-board catalogue price. Used in tests and operator reports.
type CostFloorVsMarket struct {
	ModelID          string
	JobType          string
	CostPlusPer1K    float64
	MarketBoardPer1K float64
	GapRatio         float64 // cost_plus / market; >1 means cost basis above market
}

func CompareCostFloorToMarketBoard(supplierShare float64) []CostFloorVsMarket {
	board, err := loadPriceBoard()
	if err != nil {
		return nil
	}
	var out []CostFloorVsMarket
	for _, b := range repricingBenchmarks {
		cost := repriceFromSupplierEconomics(b, supplierShare, defaultElectricityUSDPerKWh)
		mkt, ok := repriceFromMarketBoard(b.ModelID, b.JobType, board)
		if !ok || mkt.PricePer1K <= 0 {
			continue
		}
		out = append(out, CostFloorVsMarket{
			ModelID:          b.ModelID,
			JobType:          b.JobType,
			CostPlusPer1K:    cost.PricePer1K,
			MarketBoardPer1K: mkt.PricePer1K,
			GapRatio:         cost.PricePer1K / mkt.PricePer1K,
		})
	}
	return out
}

// SupplierViabilityAtMarket answers the question the price board cannot: at the
// catalogue price we actually charge, does the supplier clear their own power
// bill?
//
// Moving from cost-plus to a market board fixed a price that was ~460x above the
// market and, in the same stroke, cut supplier gross by the same factor. On the
// hardware the control plane currently admits (Apple Silicon, 138.7 tok/s
// measured) the two numbers land within a rounding error of each other: about
// $0.00436/hr of gross against $0.0045/hr of electricity. A marketplace whose
// suppliers pay to participate has no supply side, and that fact must be visible
// in an operator report rather than discovered by a supplier reading their
// electricity bill.
//
// This is a report, not a guard. Refusing to price at market would simply hide
// the problem behind an uncompetitive catalogue; the honest response is to make
// both facts legible at once and let the operator decide which side to fix.
type SupplierViabilityAtMarket struct {
	ModelID              string  `json:"model_id"`
	JobType              string  `json:"job_type"`
	HWClass              string  `json:"hw_class"`
	MeasuredUnitsPerSec  float64 `json:"measured_units_per_sec"`
	CataloguePricePer1K  float64 `json:"catalogue_price_per_1k"`
	SupplierGrossUSDHr   float64 `json:"supplier_gross_usd_hr"`
	ElectricityUSDHr     float64 `json:"electricity_usd_hr"`
	NetUSDHr             float64 `json:"net_usd_hr"`
	BreakEvenUnitsPerSec float64 `json:"break_even_units_per_sec"`
	Viable               bool    `json:"viable"`
}

// SupplierViabilityReport evaluates every measured model against the catalogue
// price the buyer is actually charged.
func SupplierViabilityReport(supplierShare float64) []SupplierViabilityAtMarket {
	board, err := loadPriceBoard()
	if err != nil {
		return nil
	}
	var out []SupplierViabilityAtMarket
	for _, b := range repricingBenchmarks {
		mkt, ok := repriceFromMarketBoard(b.ModelID, b.JobType, board)
		if !ok || mkt.PricePer1K <= 0 {
			continue
		}
		watts := sustainedWattsByHWClass[b.HWClass]
		if watts <= 0 {
			watts = 30.0
		}
		elec := watts / 1000.0 * defaultElectricityUSDPerKWh
		unitsPerHr := b.UnitsPerSec * 3600.0
		gross := unitsPerHr / 1000.0 * mkt.PricePer1K * supplierShare
		var breakEven float64
		if perUnitHr := mkt.PricePer1K / 1000.0 * supplierShare; perUnitHr > 0 {
			breakEven = elec / perUnitHr / 3600.0
		}
		out = append(out, SupplierViabilityAtMarket{
			ModelID: b.ModelID, JobType: b.JobType, HWClass: b.HWClass,
			MeasuredUnitsPerSec:  b.UnitsPerSec,
			CataloguePricePer1K:  mkt.PricePer1K,
			SupplierGrossUSDHr:   roundUSD(gross),
			ElectricityUSDHr:     roundUSD(elec),
			NetUSDHr:             roundUSD(gross - elec),
			BreakEvenUnitsPerSec: breakEven,
			Viable:               gross > elec,
		})
	}
	return out
}

const actualUSDBasisQuoteDerivedSettlement = "quote_derived_per_task_buyer_charge_settlement"

type PriceTuningBlockReason string

const priceTuningBlockedNoIndependentTelemetry PriceTuningBlockReason = "independent_execution_cost_telemetry_unavailable"

type CostDriftRow struct {
	JobType           string                 `json:"job_type"`
	ModelRef          string                 `json:"model_ref"`
	Samples           int                    `json:"samples"`             // quote-bound, terminal jobs behind this rollup
	AvgQuotedUSD      float64                `json:"avg_quoted_usd"`      // mean quotes.cost_expected_usd
	AvgActualUSD      float64                `json:"avg_actual_usd"`      // mean quote-derived jobs.actual_usd settlement
	DriftRatio        float64                `json:"drift_ratio"`         // settled charge / quoted charge; not a cost-overrun ratio
	DriftPct          float64                `json:"drift_pct"`           // (drift_ratio - 1) * 100, signed charge-realization difference
	ActualUSDBasis    string                 `json:"actual_usd_basis"`    // explicitly names the source semantics of AvgActualUSD
	UsingForTuning    bool                   `json:"using_for_tuning"`    // always false until independent economic telemetry exists
	TuningBlockReason PriceTuningBlockReason `json:"tuning_block_reason"` // machine-readable fail-closed reason
}

func finalizeCostDriftRow(d CostDriftRow) CostDriftRow {
	if d.AvgQuotedUSD > 0 {
		d.DriftRatio = d.AvgActualUSD / d.AvgQuotedUSD
		d.DriftPct = (d.DriftRatio - 1) * 100
	}
	d.ActualUSDBasis = actualUSDBasisQuoteDerivedSettlement
	d.UsingForTuning = false
	d.TuningBlockReason = priceTuningBlockedNoIndependentTelemetry
	return d
}

func (s *Store) CostDriftRollup(ctx context.Context) ([]CostDriftRow, error) {
	rows, err := s.pool.Query(ctx,
		`SELECT j.job_type,
		        COALESCE(j.model_ref,''),
		        COUNT(*),
		        COALESCE(AVG(q.cost_expected_usd),0),
		        COALESCE(AVG(j.actual_usd),0)
		   FROM jobs j
		   JOIN quotes q ON q.id = j.quote_id
		  WHERE j.quote_id IS NOT NULL
		    AND j.status IN ('complete','failed')
		    AND q.cost_expected_usd > 0
		  GROUP BY j.job_type, j.model_ref
		  ORDER BY COUNT(*) DESC, j.job_type, j.model_ref`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []CostDriftRow
	for rows.Next() {
		var d CostDriftRow
		if err := rows.Scan(&d.JobType, &d.ModelRef, &d.Samples, &d.AvgQuotedUSD, &d.AvgActualUSD); err != nil {
			return nil, err
		}
		out = append(out, finalizeCostDriftRow(d))
	}
	return out, rows.Err()
}

func (s *Store) ApplyRepricing(ctx context.Context, results []RepriceResult) (updated int, err error) {
	for _, r := range results {
		tag, uerr := s.pool.Exec(ctx,
			`UPDATE models
			    SET price_per_1k = $2, price_source = 'market_board', price_formula = $3
			  WHERE id = $1 AND (price_source IS NULL OR price_source = 'seed' OR price_source = 'measured_supplier_economics' OR price_source = 'market_board')`,
			r.ModelID, r.PricePer1K, r.Formula,
		)
		if uerr != nil {
			return updated, fmt.Errorf("apply repricing for %s: %w", r.ModelID, uerr)
		}
		updated += int(tag.RowsAffected())
	}
	return updated, nil
}
