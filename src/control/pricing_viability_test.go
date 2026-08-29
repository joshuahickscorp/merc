package main

import "testing"

// A marketplace whose suppliers pay to participate has no supply side.
//
// The move from cost-plus to a market board corrected a price that was ~460x
// above market and cut supplier gross by the same factor. This test exists so
// that the resulting viability -- or lack of it -- is an asserted, visible fact
// rather than something a supplier discovers from their electricity bill.
func TestSupplierViabilityAtMarketIsReportedHonestly(t *testing.T) {
	rows := SupplierViabilityReport()
	if len(rows) == 0 {
		t.Fatal("no measured model could be evaluated against the board")
	}
	for _, r := range rows {
		if r.MeasuredUnitsPerSec <= 0 || r.CataloguePricePer1K <= 0 {
			t.Fatalf("%s: incomplete inputs %+v", r.ModelID, r)
		}
		// The arithmetic must be self-consistent: at exactly the break-even
		// throughput, gross equals electricity.
		if r.BreakEvenUnitsPerSec <= 0 {
			t.Fatalf("%s: break-even not computed", r.ModelID)
		}
		wantViable := r.MeasuredUnitsPerSec > r.BreakEvenUnitsPerSec
		if r.Viable != wantViable {
			t.Fatalf("%s: Viable=%v but measured %.1f vs break-even %.1f units/s",
				r.ModelID, r.Viable, r.MeasuredUnitsPerSec, r.BreakEvenUnitsPerSec)
		}
		if r.NetUSDHr != roundUSD(r.SupplierGrossUSDHr-r.ElectricityUSDHr) {
			t.Fatalf("%s: net %.6f does not reconcile gross %.6f minus power %.6f",
				r.ModelID, r.NetUSDHr, r.SupplierGrossUSDHr, r.ElectricityUSDHr)
		}
		t.Logf("%-28s %8.1f u/s  price $%.8f/1K  gross $%.5f/hr  power $%.5f/hr  net $%+.5f/hr  break-even %.0f u/s  viable=%v",
			r.ModelID, r.MeasuredUnitsPerSec, r.CataloguePricePer1K,
			r.SupplierGrossUSDHr, r.ElectricityUSDHr, r.NetUSDHr,
			r.BreakEvenUnitsPerSec, r.Viable)
	}
}
