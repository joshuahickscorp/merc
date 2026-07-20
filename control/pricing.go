package main

import (
	"context"
	"fmt"
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
		SourceCitation: ".artifacts/gpu-bench/metal-Apple_M3_Pro-20260701T223411Z/capability.json (eps)",
	},
	{
		ModelID:        "llama-3.2-1b-instruct-q4",
		JobType:        "batch_infer",
		UnitsPerSec:    138.7,
		HWClass:        "apple_silicon_pro",
		SourceCitation: "docs/GPU_CAPABILITY.md:43 (batch-32 peak tok/s, byte-identical to serial)",
	},
}

var sustainedWattsByHWClass = map[string]float64{
	"apple_silicon_base":  20.0,
	"apple_silicon_pro":   30.0,
	"apple_silicon_max":   45.0,
	"apple_silicon_ultra": 65.0,
	"cpu":                 25.0,
}

const defaultElectricityUSDPerKWh = 0.15

const targetSupplierUSDHr = 2.0

type RepriceResult struct {
	ModelID    string
	JobType    string
	PricePer1K float64
	Formula    string // human-readable, cites every real input (proof artifact)
}

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

func RepriceCatalogueFromSupplierEconomics(supplierShare float64) []RepriceResult {
	out := make([]RepriceResult, 0, len(repricingBenchmarks))
	for _, b := range repricingBenchmarks {
		out = append(out, repriceFromSupplierEconomics(b, supplierShare, defaultElectricityUSDPerKWh))
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
			    SET price_per_1k = $2, price_source = 'measured_supplier_economics', price_formula = $3
			  WHERE id = $1 AND (price_source IS NULL OR price_source = 'seed')`,
			r.ModelID, r.PricePer1K, r.Formula,
		)
		if uerr != nil {
			return updated, fmt.Errorf("apply repricing for %s: %w", r.ModelID, uerr)
		}
		updated += int(tag.RowsAffected())
	}
	return updated, nil
}
