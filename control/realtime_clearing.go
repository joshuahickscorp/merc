package main

import (
	"fmt"
	"math"
)

// Realtime market clearing ranks live offers by the expected cost of a
// *verified successful outcome*, not by self-declared warmth.
//
// Every term must come from a measurement already in this tree. Terms without a
// measurement are omitted rather than filled with a guessed weight:
//
//   base execution ask
//     supplier_input + supplier_output rates on the live offer
//   + expected retry cost
//     observed terminal failure rate on execution_contracts for that supplier
//     and runtime profile (same arithmetic as MeasuredCellCost's delivered term)
//   + expected refund risk
//     observed full internal-refund rate on verified settlements for that
//     supplier and runtime profile, when enough samples exist
//
// Omitted for lack of a measurement on the realtime lane:
//
//   verification / redundancy / honeypot overhead
//     realtime is V0: PricingDecision records verification as not applicable
//     ("no redundant execution"). There is no per-supplier redundancy class for
//     chat completion offers.
//   locality benefit
//     realtime offers carry no governed region and no residency nanos; the
//     liquidity report states UNSPECIFIED_NO_GOVERNED_REGION_AUTHORITY. A
//     locality discount with no measured cost reduction would be fiction.
//
// Warmth is a tiebreak only inside the same verified-outcome cost class — the
// same discipline scheduler.go states for batch (cost rank first, warmth
// never outranks measured cost).

const (
	// realtimeOrderBookPolicy is frozen into every market-clearing receipt so a
	// regression in ranking discipline is visible on the buyer receipt rather
	// than silent.
	realtimeOrderBookPolicy = "verified_outcome_cost_v1"

	// minRealtimeOutcomeSamples is the fewest terminal contracts a supplier
	// needs on a runtime profile before failure or refund rates adjust ranking.
	// Below this the rates are treated as unmeasured (no adjustment), matching
	// the "refuse to rank on noise" discipline in runtime_cell_cost.go. Five is
	// the same measurement floor service leases already require before a
	// latency claim can enter the offer book.
	minRealtimeOutcomeSamples = 5
)

// RealtimeClearingRankingInputs is the full set of ranking signals frozen into
// the market-clearing receipt for the selected offer. A buyer can see why a
// supplier cleared; a regression cannot hide behind an opaque rank number.
type RealtimeClearingRankingInputs struct {
	BaseAskNanos                int64 `json:"base_supplier_ask_nanos_per_million_tokens"`
	SelectedSupplierInputNanos  int64 `json:"selected_supplier_input_nanos_per_million_tokens"`
	SelectedSupplierOutputNanos int64 `json:"selected_supplier_output_nanos_per_million_tokens"`
	VerifiedOutcomeCostNanos    int64 `json:"verified_outcome_cost_nanos_per_million_tokens"`

	TerminalAttempts    int      `json:"terminal_attempts"`
	TerminalFails       int      `json:"terminal_fails"`
	ObservedFailureRate *float64 `json:"observed_failure_rate,omitempty"`
	RetryCostApplied    bool     `json:"retry_cost_applied"`

	VerifiedSettlements int      `json:"verified_settlements"`
	RefundCount         int      `json:"refund_count"`
	ObservedRefundRate  *float64 `json:"observed_refund_rate,omitempty"`
	RefundRiskApplied   bool     `json:"refund_risk_applied"`

	Warmth     string `json:"warmth"`
	WarmthRank int    `json:"warmth_rank"`

	// OmittedTerms names every product-claim cost component this ranking
	// refused to invent. It travels with the receipt so an auditor does not
	// have to re-derive the absences from source.
	OmittedTerms []string `json:"omitted_terms"`
}

// realtimeClearingOmittedTerms is the fixed list of product-claim terms that
// have no measurement on the realtime lane today.
func realtimeClearingOmittedTerms() []string {
	return []string{
		"verification_redundancy_honeypot:realtime_v0_not_applicable",
		"locality_benefit:no_governed_region_or_residency_cost_on_realtime_offers",
	}
}

// warmthRank maps a self-declared warmth enum to a dense rank used only as a
// tiebreak. Lower is preferred. Unknown values sort last.
func warmthRank(warmth string) int {
	switch warmth {
	case "HOT":
		return 0
	case "WARM":
		return 1
	case "CACHED":
		return 2
	case "COLD":
		return 3
	default:
		return 4
	}
}

// realtimeVerifiedOutcomeCostNanos computes the ranking key for one offer.
//
//	base = input_nanos + output_nanos
//	if terminal_attempts >= minRealtimeOutcomeSamples and some succeeded:
//	  base = base * attempts / (attempts - fails)     // expected retry cost
//	if verified_settlements >= minRealtimeOutcomeSamples and some not refunded:
//	  base = base * verified / (verified - refunds)   // expected refund risk
//
// Rates are applied only when measured. Division is integer and rounds up so a
// fractional extra attempt cannot understate cost.
func realtimeVerifiedOutcomeCostNanos(
	inputNanos, outputNanos int64,
	terminalAttempts, terminalFails int,
	verifiedSettlements, refundCount int,
) (cost int64, failureRate, refundRate *float64, retryApplied, refundApplied bool) {
	if inputNanos < 0 || outputNanos < 0 {
		return 0, nil, nil, false, false
	}
	cost = inputNanos + outputNanos
	if cost < 0 { // overflow guard; rates are validated far below MaxInt64/2
		return math.MaxInt64, nil, nil, false, false
	}

	if terminalAttempts >= minRealtimeOutcomeSamples && terminalFails >= 0 {
		rate := float64(terminalFails) / float64(terminalAttempts)
		if terminalFails > terminalAttempts {
			rate = 1
		}
		failureRate = &rate
		if terminalFails >= terminalAttempts {
			// Every observed attempt failed: no verified-outcome cost exists.
			// Rank last rather than treat the base ask as honest.
			cost = math.MaxInt64 / 4
			retryApplied = true
		} else {
			delivered := terminalAttempts - terminalFails
			cost = divRoundUp(cost*int64(terminalAttempts), int64(delivered))
			retryApplied = terminalFails > 0
		}
	}

	if verifiedSettlements >= minRealtimeOutcomeSamples && refundCount >= 0 {
		rate := float64(refundCount) / float64(verifiedSettlements)
		if refundCount > verifiedSettlements {
			rate = 1
		}
		refundRate = &rate
		if refundCount >= verifiedSettlements {
			cost = math.MaxInt64 / 4
			refundApplied = true
		} else {
			kept := verifiedSettlements - refundCount
			cost = divRoundUp(cost*int64(verifiedSettlements), int64(kept))
			refundApplied = refundCount > 0
		}
	}

	return cost, failureRate, refundRate, retryApplied, refundApplied
}

// divRoundUp returns ceil(num/den) for positive integers. den must be > 0.
func divRoundUp(num, den int64) int64 {
	if den <= 0 {
		return math.MaxInt64
	}
	if num <= 0 {
		return 0
	}
	// (num + den - 1) / den can overflow; use checked form.
	q := num / den
	if num%den != 0 {
		if q == math.MaxInt64 {
			return math.MaxInt64
		}
		return q + 1
	}
	return q
}

// buildRealtimeClearingRankingInputs freezes the selected offer's ranking
// signals for the market-clearing receipt.
func buildRealtimeClearingRankingInputs(
	inputNanos, outputNanos int64,
	terminalAttempts, terminalFails int,
	verifiedSettlements, refundCount int,
	warmth string,
) RealtimeClearingRankingInputs {
	cost, failRate, refundRate, retryApplied, refundApplied := realtimeVerifiedOutcomeCostNanos(
		inputNanos, outputNanos, terminalAttempts, terminalFails,
		verifiedSettlements, refundCount,
	)
	return RealtimeClearingRankingInputs{
		BaseAskNanos:                inputNanos + outputNanos,
		SelectedSupplierInputNanos:  inputNanos,
		SelectedSupplierOutputNanos: outputNanos,
		VerifiedOutcomeCostNanos:    cost,
		TerminalAttempts:            terminalAttempts,
		TerminalFails:               terminalFails,
		ObservedFailureRate:         failRate,
		RetryCostApplied:            retryApplied,
		VerifiedSettlements:         verifiedSettlements,
		RefundCount:                 refundCount,
		ObservedRefundRate:          refundRate,
		RefundRiskApplied:           refundApplied,
		Warmth:                      warmth,
		WarmthRank:                  warmthRank(warmth),
		OmittedTerms:                realtimeClearingOmittedTerms(),
	}
}

// realtimeClearingSelectionReason is the human-readable selection line frozen
// into the receipt. It names the ranking discipline, not the winner's warmth.
func realtimeClearingSelectionReason(inputs RealtimeClearingRankingInputs) string {
	switch {
	case inputs.RetryCostApplied && inputs.RefundRiskApplied:
		return fmt.Sprintf(
			"lowest verified-outcome cost (base ask adjusted by measured failure rate %.4f and refund rate %.4f); warmth %s is tiebreak only",
			*inputs.ObservedFailureRate, *inputs.ObservedRefundRate, inputs.Warmth)
	case inputs.RetryCostApplied:
		return fmt.Sprintf(
			"lowest verified-outcome cost (base ask adjusted by measured failure rate %.4f); warmth %s is tiebreak only",
			*inputs.ObservedFailureRate, inputs.Warmth)
	case inputs.RefundRiskApplied:
		return fmt.Sprintf(
			"lowest verified-outcome cost (base ask adjusted by measured refund rate %.4f); warmth %s is tiebreak only",
			*inputs.ObservedRefundRate, inputs.Warmth)
	default:
		return fmt.Sprintf(
			"lowest verified-outcome cost (base supplier ask; failure and refund rates unmeasured below %d samples); warmth %s is tiebreak only",
			minRealtimeOutcomeSamples, inputs.Warmth)
	}
}

// realtimeOfferBeats reports whether offer a should clear above offer b under
// the verified-outcome ranking. Used by pure unit tests; production ranking is
// the SQL twin of this comparison so the atomic reservation cannot diverge.
func realtimeOfferBeats(
	aCost, bCost int64,
	aWarmth, bWarmth string,
	aAvailable, bAvailable int,
	aWorkerID, bWorkerID string,
) bool {
	if aCost != bCost {
		return aCost < bCost
	}
	if wa, wb := warmthRank(aWarmth), warmthRank(bWarmth); wa != wb {
		return wa < wb
	}
	if aAvailable != bAvailable {
		return aAvailable > bAvailable
	}
	return aWorkerID < bWorkerID
}
