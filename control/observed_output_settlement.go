package main

import "math"

// minBillableSettlementUSD is the smallest positive buyer charge the ledger can
// represent. Settlement may rebate unused generative output down to this floor
// but never to zero when frozen economics were positive.
const minBillableSettlementUSD = 1.0 / float64(microUSDPerUSD)

// observedOutputSettlement is the ledger-time adjustment of frozen per-task
// economics by the unused generative output share. Frozen task columns are
// never written; only the amounts passed to splitFrozenCharge change.
type observedOutputSettlement struct {
	BilledCharge   float64
	SupplierPayout float64
	// Applied is true when the observed-output formula adjusted the freeze.
	// False means settle exactly at the frozen pair (non-generative, missing
	// plan/observation, or zero ceiling).
	Applied bool
	// Evidence for buyer receipts. Zero when Applied is false.
	CeilingTokens  int64
	ObservedTokens int64
	UnusedShare    float64
	RebateUSD      float64
}

// settleObservedOutputTokens bounds generative batch settlement by tokens the
// worker actually reported, relative to the frozen output ceiling.
//
// Arithmetic (invariants win on conflict):
//
//	outputUnitShare = estimatedOut / (estimatedIn + estimatedOut)
//	ceilingTokens   = expectedOutputRecords * maxTokens
//	observed        = clamp(reportedTokens, 0, ceilingTokens)
//	unusedShare     = outputUnitShare * (1 - observed/ceilingTokens)
//	billedCharge    = round(frozenCharge * (1 - unusedShare))
//	billedCharge    = max(billedCharge, minBillable)   // still never above freeze
//	supplierPayout' = round(frozenPayout * billedCharge / frozenCharge)
//
// Missing plan inputs, non-generative work (estimatedOut == 0), a zero
// ceiling, or a missing reported-token observation settle at the freeze.
// Settlement may only reduce relative to the freeze.
func settleObservedOutputTokens(
	frozenCharge, frozenPayout float64,
	estimatedIn, estimatedOut int64,
	expectedOutputRecords int64,
	maxTokens uint32,
	reportedTokens int64,
	hasReported bool,
) observedOutputSettlement {
	out := observedOutputSettlement{
		BilledCharge:   frozenCharge,
		SupplierPayout: frozenPayout,
	}
	if !moneyUSDInDomain(frozenCharge) || !moneyUSDInDomain(frozenPayout) ||
		frozenCharge <= 0 || frozenPayout < 0 || frozenPayout > frozenCharge {
		return out
	}
	if !hasReported || estimatedOut <= 0 || estimatedIn < 0 {
		return out
	}
	totalUnits := estimatedIn + estimatedOut
	if totalUnits <= 0 {
		return out
	}
	if expectedOutputRecords <= 0 || maxTokens == 0 {
		return out
	}
	// expectedOutputRecords * maxTokens can overflow int64 for absurd inputs;
	// refuse to invent a ceiling and settle at the freeze.
	if expectedOutputRecords > math.MaxInt64/int64(maxTokens) {
		return out
	}
	ceiling := expectedOutputRecords * int64(maxTokens)
	if ceiling <= 0 {
		return out
	}

	observed := reportedTokens
	if observed < 0 {
		observed = 0
	}
	if observed > ceiling {
		observed = ceiling
	}

	outputUnitShare := float64(estimatedOut) / float64(totalUnits)
	unusedShare := outputUnitShare * (1.0 - float64(observed)/float64(ceiling))
	if unusedShare < 0 {
		unusedShare = 0
	}
	if unusedShare > outputUnitShare {
		unusedShare = outputUnitShare
	}
	// No unused output → freeze stands (including the over-report clamp).
	if unusedShare == 0 {
		out.CeilingTokens = ceiling
		out.ObservedTokens = observed
		return out
	}

	billed := roundUSD(frozenCharge * (1.0 - unusedShare))
	if billed < minBillableSettlementUSD {
		billed = minBillableSettlementUSD
	}
	// Invariant 2: never increase relative to the freeze.
	if billed > frozenCharge {
		billed = frozenCharge
	}
	// Scaling from freeze must keep supplier within [0, billed] and
	// [0, frozenPayout].
	payout := roundUSD(frozenPayout * billed / frozenCharge)
	if payout < 0 {
		payout = 0
	}
	if payout > frozenPayout {
		payout = frozenPayout
	}
	if payout > billed {
		payout = billed
	}

	rebate := roundUSD(frozenCharge - billed)
	if rebate < 0 {
		rebate = 0
	}
	out.BilledCharge = billed
	out.SupplierPayout = payout
	out.Applied = billed < frozenCharge || payout < frozenPayout
	out.CeilingTokens = ceiling
	out.ObservedTokens = observed
	out.UnusedShare = unusedShare
	out.RebateUSD = rebate
	// Always surface ceiling/observed once we had a generative ceiling, even
	// when the floor pinned the charge (buyer can still audit the observation).
	if out.CeilingTokens == 0 {
		out.CeilingTokens = ceiling
		out.ObservedTokens = observed
	}
	return out
}
