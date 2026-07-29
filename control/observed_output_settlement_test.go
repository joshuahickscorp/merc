package main

import (
	"math"
	"testing"
	"time"

	"github.com/google/uuid"
)

// Core regression: one record, max_tokens=256, observed 5 tokens.
// Buyer charge must drop by approximately the unused output share, and the
// three ledger rows must still sum to zero.
func TestObservedOutputSettlementOneRecordMaxTokens256Observed5(t *testing.T) {
	const (
		frozenCharge = 1.0
		frozenPayout = 0.70
		// 1 input token + 256 output tokens mirrors a one-record generative plan.
		estimatedIn  int64 = 1
		estimatedOut int64 = 256
		records      int64 = 1
		maxTokens          = uint32(256)
		observed     int64 = 5
	)

	got := settleObservedOutputTokens(
		frozenCharge, frozenPayout,
		estimatedIn, estimatedOut,
		records, maxTokens,
		observed, true,
	)
	if !got.Applied {
		t.Fatal("expected observed-output rebate to apply")
	}
	if got.CeilingTokens != 256 || got.ObservedTokens != 5 {
		t.Fatalf("evidence ceiling=%d observed=%d, want 256/5", got.CeilingTokens, got.ObservedTokens)
	}

	outputUnitShare := float64(estimatedOut) / float64(estimatedIn+estimatedOut)
	unusedShare := outputUnitShare * (1.0 - float64(observed)/float64(256))
	wantCharge := roundUSD(frozenCharge * (1.0 - unusedShare))
	if wantCharge < minBillableSettlementUSD {
		wantCharge = minBillableSettlementUSD
	}
	wantPayout := roundUSD(frozenPayout * wantCharge / frozenCharge)

	if got.BilledCharge != wantCharge {
		t.Fatalf("billed charge=%.6f, want %.6f (unusedShare=%.6f)",
			got.BilledCharge, wantCharge, unusedShare)
	}
	if got.SupplierPayout != wantPayout {
		t.Fatalf("supplier payout=%.6f, want %.6f", got.SupplierPayout, wantPayout)
	}
	// Approximately the unused output share of the freeze (within one micro
	// after roundUSD on the billed charge).
	if math.Abs(got.BilledCharge-wantCharge) > 0 {
		t.Fatalf("billed charge drifted from rounded unused-share arithmetic")
	}
	rebateFrac := (frozenCharge - got.BilledCharge) / frozenCharge
	if math.Abs(rebateFrac-unusedShare) > 1e-5 {
		t.Fatalf("rebate fraction=%.9f far from unusedShare=%.9f", rebateFrac, unusedShare)
	}
	// Drop is large: ~15× overbill relative to 5/256 of the output ceiling.
	if got.BilledCharge >= frozenCharge*0.5 {
		t.Fatalf("billed charge %.6f did not drop enough from freeze %.6f",
			got.BilledCharge, frozenCharge)
	}
	if got.RebateUSD != roundUSD(frozenCharge-got.BilledCharge) {
		t.Fatalf("rebate=%.6f, want %.6f", got.RebateUSD, frozenCharge-got.BilledCharge)
	}

	// Zero-sum via splitFrozenCharge (buyer negative, supplier+platform positive).
	buyer, supplier, task := uuid.New(), uuid.New(), uuid.New()
	entries := splitFrozenCharge(buyer, supplier, task, "usd",
		got.BilledCharge, got.SupplierPayout, 90, time.Unix(100, 0))
	if len(entries) != 3 {
		t.Fatalf("entries=%d, want 3", len(entries))
	}
	var sum float64
	for _, e := range entries {
		sum += e.AmountUSD
	}
	if roundUSD(sum) != 0 {
		t.Fatalf("ledger rows sum to %.9f, want 0", sum)
	}
	if entries[0].AmountUSD != -got.BilledCharge ||
		entries[1].AmountUSD != got.SupplierPayout ||
		entries[2].AmountUSD != roundUSD(got.BilledCharge-got.SupplierPayout) {
		t.Fatalf("entries=%+v do not match settled amounts", entries)
	}
}

// Invariant 2: settlement never increases relative to the freeze, even when
// the worker reports more tokens than the ceiling.
func TestObservedOutputSettlementNeverIncreasesAboveFreeze(t *testing.T) {
	frozenCharge, frozenPayout := 0.50, 0.30
	got := settleObservedOutputTokens(
		frozenCharge, frozenPayout,
		10, 100,
		1, 100,
		10_000, // well above ceiling
		true,
	)
	if got.BilledCharge > frozenCharge {
		t.Fatalf("billed charge %.6f > freeze %.6f", got.BilledCharge, frozenCharge)
	}
	if got.SupplierPayout > frozenPayout {
		t.Fatalf("payout %.6f > freeze payout %.6f", got.SupplierPayout, frozenPayout)
	}
	// Full use of the ceiling → freeze stands.
	if got.BilledCharge != frozenCharge || got.SupplierPayout != frozenPayout {
		t.Fatalf("over-report must clamp to freeze, got charge=%.6f payout=%.6f",
			got.BilledCharge, got.SupplierPayout)
	}
	if got.ObservedTokens != 100 {
		t.Fatalf("observed clamp=%d, want ceiling 100", got.ObservedTokens)
	}
}

// Invariant 4: 0 <= supplierPayout' <= billedCharge, platformTake' >= 0.
func TestObservedOutputSettlementSupplierWithinBilledCharge(t *testing.T) {
	cases := []struct {
		name                   string
		charge, payout         float64
		inTok, outTok, records int64
		maxTokens              uint32
		observed               int64
	}{
		{"partial", 1.0, 0.7, 1, 256, 1, 256, 5},
		{"zero observed", 2.0, 1.0, 50, 200, 2, 100, 0},
		{"full ceiling", 0.25, 0.10, 8, 32, 1, 32, 32},
		{"tiny freeze", 0.000010, 0.000005, 1, 10, 1, 10, 1},
		{"high share payout", 0.10, 0.10, 4, 100, 1, 100, 1},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := settleObservedOutputTokens(
				tc.charge, tc.payout,
				tc.inTok, tc.outTok,
				tc.records, tc.maxTokens,
				tc.observed, true,
			)
			if got.SupplierPayout < 0 {
				t.Fatalf("supplier payout negative: %.9f", got.SupplierPayout)
			}
			if got.SupplierPayout > got.BilledCharge {
				t.Fatalf("supplier %.9f > billed %.9f", got.SupplierPayout, got.BilledCharge)
			}
			platform := roundUSD(got.BilledCharge - got.SupplierPayout)
			if platform < 0 {
				t.Fatalf("platform take negative: %.9f", platform)
			}
			if got.BilledCharge > tc.charge || got.SupplierPayout > tc.payout {
				t.Fatalf("increased above freeze: got=%+v freeze charge=%.6f payout=%.6f",
					got, tc.charge, tc.payout)
			}
		})
	}
}

// Invariant 6: non-generative (embed) jobs are unchanged — zero ceiling, zero rebate.
func TestObservedOutputSettlementEmbedUnchanged(t *testing.T) {
	frozenCharge, frozenPayout := 0.42, 0.21
	// Embed plans freeze EstimatedOutputTokens at 0.
	got := settleObservedOutputTokens(
		frozenCharge, frozenPayout,
		128, 0, // no generative output
		10, 0, // no max_tokens
		999, true,
	)
	if got.Applied || got.BilledCharge != frozenCharge || got.SupplierPayout != frozenPayout {
		t.Fatalf("embed settled %+v, want exact freeze", got)
	}
	if got.RebateUSD != 0 || got.CeilingTokens != 0 {
		t.Fatalf("embed must not invent ceiling/rebate: %+v", got)
	}
}

// Invariant 7: missing compute-plan token estimates or missing reported tokens
// settle at the freeze — no silent zeroing, no crash.
func TestObservedOutputSettlementMissingInputsSettleAtFreeze(t *testing.T) {
	frozenCharge, frozenPayout := 0.88, 0.44
	cases := []struct {
		name                   string
		inTok, outTok, records int64
		maxTokens              uint32
		observed               int64
		hasReported            bool
	}{
		{"no report", 10, 100, 1, 100, 0, false},
		{"zero total units", 0, 0, 1, 100, 5, true},
		{"zero ceiling records", 10, 100, 0, 100, 5, true},
		{"zero max tokens", 10, 100, 1, 0, 5, true},
		{"negative estimated out treated as missing", 10, -1, 1, 100, 5, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := settleObservedOutputTokens(
				frozenCharge, frozenPayout,
				tc.inTok, tc.outTok,
				tc.records, tc.maxTokens,
				tc.observed, tc.hasReported,
			)
			if got.Applied {
				t.Fatalf("missing inputs must not apply rebate: %+v", got)
			}
			if got.BilledCharge != frozenCharge || got.SupplierPayout != frozenPayout {
				t.Fatalf("got charge=%.6f payout=%.6f, want freeze", got.BilledCharge, got.SupplierPayout)
			}
			if got.BilledCharge == 0 && frozenCharge != 0 {
				t.Fatal("silent zeroing of a positive freeze")
			}
		})
	}
}

func TestObservedOutputSettlementDeterministic(t *testing.T) {
	a := settleObservedOutputTokens(1.23, 0.80, 40, 200, 2, 128, 17, true)
	b := settleObservedOutputTokens(1.23, 0.80, 40, 200, 2, 128, 17, true)
	if a != b {
		t.Fatalf("non-deterministic settlement: %+v vs %+v", a, b)
	}
}

func TestObservedOutputSettlementMinBillableFloor(t *testing.T) {
	// Extreme unused share on a tiny freeze still floors at one micro-USD.
	got := settleObservedOutputTokens(
		0.000002, 0.000001,
		1, 1_000_000,
		1, 1_000_000,
		0, true,
	)
	if got.BilledCharge < minBillableSettlementUSD {
		t.Fatalf("billed %.9f below minBillable", got.BilledCharge)
	}
	if got.BilledCharge > 0.000002 {
		t.Fatalf("floor raised above freeze: %.9f", got.BilledCharge)
	}
}
