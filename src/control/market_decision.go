package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/google/uuid"
)

// MarketDecision is the canonical accepted market authority for one clearing
// event. It is one concept with an explicit market_shape discriminator and a
// per-shape body — not one nullable-field type spanning push and pull.
//
// Build order (Step 7 shape note):
//  1. realtime PUSH_ORDER_BOOK (this file / this step)
//  2. service lease on the same push shape (later)
//  3. batch PULL_ELIGIBILITY_SNAPSHOT at claim (later)
//
// RealtimeMarketClearingReceipt is a lossless projection of the push body for
// existing buyers and storage readers. Do not invent a second clearing
// authority beside this type.
//
// Shape note: considered + excluded-with-reason follows the evidence SHAPE of
// runtime_shadow_selections (considered_cells / excluded_cells). That table is
// the wrong authority plane (runtime selection, not market supply) — copy the
// shape only; do not extend that table.

const (
	marketDecisionVersion = 1

	marketShapePushOrderBook           = "PUSH_ORDER_BOOK"
	marketShapePullEligibilitySnapshot = "PULL_ELIGIBILITY_SNAPSHOT"

	// Availability modes record how the book was claimed. Realtime multi-offer
	// books use SKIP LOCKED so concurrent admits take distinct free offers;
	// single-offer books and SKIP misses fall back to blocking FOR UPDATE.
	// Service lease serialises with blocking FOR UPDATE on every candidate —
	// that difference is first-class, not smoothed over.
	marketAvailabilitySkipLocked        = "SKIP_LOCKED"
	marketAvailabilityBlockingForUpdate = "BLOCKING_FOR_UPDATE"

	// Exclusion reason codes on push book candidates. Empty means SELECTED.
	marketExclusionLockSkipped          = "lock_skipped_skip_locked_contention"
	marketExclusionNotSelectedWorseRank = "not_selected_worse_economic_rank"
)

// MarketDecision is the immutable accepted market object.
type MarketDecision struct {
	Version     int    `json:"version"`
	MarketShape string `json:"market_shape"`

	// Exactly one body is set, matching MarketShape.
	PushOrderBook           *MarketPushOrderBook           `json:"push_order_book,omitempty"`
	PullEligibilitySnapshot *MarketPullEligibilitySnapshot `json:"pull_eligibility_snapshot,omitempty"`
}

// MarketPushOrderBook is the body for realtime (and later service-lease) push
// clearing: a ranked eligible book, availability mode, selected offer, and
// every considered peer with an exclusion reason when not selected.
type MarketPushOrderBook struct {
	AvailabilityMode string `json:"availability_mode"`
	OrderBookPolicy  string `json:"order_book_policy"`

	CandidateCount int `json:"candidate_count"`
	SelectedRank   int `json:"selected_rank"`

	SelectedWorkerID            uuid.UUID `json:"selected_worker_id"`
	SelectedSupplierID          uuid.UUID `json:"selected_supplier_id"`
	SelectedSupplierInputNanos  int64     `json:"selected_supplier_input_nanos_per_million_tokens"`
	SelectedSupplierOutputNanos int64     `json:"selected_supplier_output_nanos_per_million_tokens"`

	BuyerCeilingNanos         int64  `json:"buyer_ceiling_nanos"`
	AcceptedCeilingNanos      int64  `json:"accepted_ceiling_nanos"`
	PricingDecisionSHA256     string `json:"pricing_decision_sha256"`
	PositiveContributionNanos int64  `json:"positive_contribution_nanos"`

	ReferenceCurrency    string `json:"reference_currency"`
	SettlementCurrency   string `json:"settlement_currency"`
	SupplierRateCurrency string `json:"supplier_rate_currency"`
	BuyerMoneyCurrency   string `json:"buyer_money_currency"`

	SelectionReason string                         `json:"selection_reason"`
	RankingInputs   *RealtimeClearingRankingInputs `json:"ranking_inputs"`

	// Considered is the eligible set that entered economic ranking at claim
	// time, ordered by selected_rank. Every non-selected entry carries an
	// exclusion reason code — including better-ranked peers skipped under
	// SKIP LOCKED contention.
	Considered []MarketBookCandidate `json:"considered"`
}

// MarketBookCandidate is one eligible offer in the push book.
type MarketBookCandidate struct {
	Rank       int       `json:"rank"`
	WorkerID   uuid.UUID `json:"worker_id"`
	SupplierID uuid.UUID `json:"supplier_id"`
	Warmth     string    `json:"warmth"`
	// VerifiedOutcomeCost is the SQL ranking key (float USD ask product with
	// measured failure/refund multipliers). Ranking currency remains float USD
	// in this step — nanos ranking is demoted (money-adjacent admission).
	VerifiedOutcomeCost float64 `json:"verified_outcome_cost"`
	// ExclusionReason is empty when this candidate is the selected offer.
	ExclusionReason string `json:"exclusion_reason,omitempty"`
}

// MarketPullEligibilitySnapshot is the batch claim shape. It is defined so the
// discriminator cannot be faked by stuffing pull fields onto a push decision.
// Step 9 binds the worker-choice plane as WorkerPlacement; this body is the
// market-plane projection of the same claim-time eligibility facts. Realtime
// still refuses any pull body (refuseRealtimePullMarketDecision).
//
// CandidateEpoch is struck for batch: pull + SKIP LOCKED has no frozen epoch
// object. Empty or "NONE_PULL_MARKET" only.
type MarketPullEligibilitySnapshot struct {
	ClaimingWorkerID   uuid.UUID `json:"claiming_worker_id"`
	ClaimingSupplierID uuid.UUID `json:"claiming_supplier_id,omitempty"`
	ClaimedTaskID      uuid.UUID `json:"claimed_task_id,omitempty"`
	ClaimedJobID       uuid.UUID `json:"claimed_job_id,omitempty"`
	// AvailabilityMode is SKIP_LOCKED for batch claim SQL.
	AvailabilityMode string `json:"availability_mode,omitempty"`
	// CandidateEpoch must not invent a frozen book. Empty or NONE_PULL_MARKET.
	CandidateEpoch string `json:"candidate_epoch,omitempty"`
	Note           string `json:"note,omitempty"`
}

// marketBookCandidateJSON is the claim-time book row returned by the authorize
// SQL candidates CTE (uuid as text from jsonb_build_object).
type marketBookCandidateJSON struct {
	Rank                int     `json:"rank"`
	WorkerID            string  `json:"worker_id"`
	SupplierID          string  `json:"supplier_id"`
	Warmth              string  `json:"warmth"`
	VerifiedOutcomeCost float64 `json:"verified_outcome_cost"`
}

// ValidateMarketDecision checks shape/body coherence and push book integrity.
func ValidateMarketDecision(md MarketDecision) error {
	if md.Version != marketDecisionVersion {
		return fmt.Errorf("market decision has unsupported version %d", md.Version)
	}
	switch md.MarketShape {
	case marketShapePushOrderBook:
		if md.PushOrderBook == nil {
			return errors.New("PUSH_ORDER_BOOK market decision lacks push body")
		}
		if md.PullEligibilitySnapshot != nil {
			return errors.New("PUSH_ORDER_BOOK market decision must not carry a pull body")
		}
		return validateMarketPushOrderBook(*md.PushOrderBook)
	case marketShapePullEligibilitySnapshot:
		if md.PullEligibilitySnapshot == nil {
			return errors.New("PULL_ELIGIBILITY_SNAPSHOT market decision lacks pull body")
		}
		if md.PushOrderBook != nil {
			return errors.New("PULL_ELIGIBILITY_SNAPSHOT market decision must not carry a push body")
		}
		// Pull body content is validated when batch claim builds it. An empty
		// placeholder is not an accepted decision.
		if md.PullEligibilitySnapshot.ClaimingWorkerID == uuid.Nil {
			return errors.New("PULL_ELIGIBILITY_SNAPSHOT market decision is incomplete")
		}
		switch md.PullEligibilitySnapshot.CandidateEpoch {
		case "", "NONE_PULL_MARKET":
			// legal — batch has no frozen candidate epoch
		default:
			return fmt.Errorf("PULL_ELIGIBILITY_SNAPSHOT must not invent candidate epoch %q",
				md.PullEligibilitySnapshot.CandidateEpoch)
		}
		if md.PullEligibilitySnapshot.AvailabilityMode != "" &&
			md.PullEligibilitySnapshot.AvailabilityMode != marketAvailabilitySkipLocked {
			return fmt.Errorf("PULL_ELIGIBILITY_SNAPSHOT availability_mode must be %s or empty, got %q",
				marketAvailabilitySkipLocked, md.PullEligibilitySnapshot.AvailabilityMode)
		}
		return nil
	default:
		return fmt.Errorf("market decision has unknown market_shape %q", md.MarketShape)
	}
}

func validateMarketPushOrderBook(book MarketPushOrderBook) error {
	switch book.AvailabilityMode {
	case marketAvailabilitySkipLocked, marketAvailabilityBlockingForUpdate:
	default:
		return fmt.Errorf("push order book has unknown availability_mode %q", book.AvailabilityMode)
	}
	if book.OrderBookPolicy != realtimeOrderBookPolicy &&
		book.OrderBookPolicy != "lowest_warmth_then_supplier_rate_v1" {
		return fmt.Errorf("push order book has unknown order_book_policy %q", book.OrderBookPolicy)
	}
	if book.CandidateCount <= 0 || book.SelectedRank <= 0 ||
		book.SelectedRank > book.CandidateCount {
		return errors.New("push order book has invalid candidate_count/selected_rank")
	}
	if book.SelectedWorkerID == uuid.Nil || book.SelectedSupplierID == uuid.Nil {
		return errors.New("push order book lacks selected offer identity")
	}
	if book.SelectedSupplierInputNanos <= 0 || book.SelectedSupplierOutputNanos <= 0 {
		return errors.New("push order book lacks selected supplier rates")
	}
	if book.BuyerCeilingNanos < 0 || book.AcceptedCeilingNanos <= 0 ||
		(book.BuyerCeilingNanos > 0 && book.AcceptedCeilingNanos > book.BuyerCeilingNanos) {
		return errors.New("push order book has invalid ceiling authority")
	}
	if book.PositiveContributionNanos <= 0 || !validSHA256(book.PricingDecisionSHA256) {
		return errors.New("push order book lacks positive PricingDecision contribution")
	}
	if strings.TrimSpace(book.SelectionReason) == "" {
		return errors.New("push order book lacks selection_reason")
	}
	// Honesty: rank > 1 must not claim the winner was the lowest cost.
	if book.SelectedRank > 1 && strings.Contains(book.SelectionReason, "lowest verified-outcome cost") {
		return errors.New("push order book selection_reason claims lowest cost while selected_rank > 1")
	}
	if book.RankingInputs == nil ||
		book.RankingInputs.BaseAskNanos <= 0 ||
		book.RankingInputs.VerifiedOutcomeCostNanos <= 0 ||
		book.RankingInputs.SelectedSupplierInputNanos != book.SelectedSupplierInputNanos ||
		book.RankingInputs.SelectedSupplierOutputNanos != book.SelectedSupplierOutputNanos ||
		len(book.RankingInputs.OmittedTerms) == 0 {
		return errors.New("push order book lacks ranking inputs")
	}
	if len(book.Considered) != book.CandidateCount {
		return errors.New("push order book considered set size disagrees with candidate_count")
	}
	var sawSelected bool
	var lockSkippedBetter int
	for i, c := range book.Considered {
		if c.Rank != i+1 {
			return errors.New("push order book considered set is not dense rank order")
		}
		if c.WorkerID == uuid.Nil || c.SupplierID == uuid.Nil {
			return errors.New("push order book candidate lacks identity")
		}
		if c.Rank == book.SelectedRank {
			if c.WorkerID != book.SelectedWorkerID || c.SupplierID != book.SelectedSupplierID {
				return errors.New("push order book selected identity disagrees with considered set")
			}
			if c.ExclusionReason != "" {
				return errors.New("push order book selected candidate must not carry exclusion_reason")
			}
			sawSelected = true
			continue
		}
		if c.ExclusionReason == "" {
			return errors.New("push order book non-selected candidate lacks exclusion_reason")
		}
		if c.Rank < book.SelectedRank {
			if c.ExclusionReason != marketExclusionLockSkipped {
				return errors.New("push order book better-ranked peer must be lock_skipped under contention")
			}
			lockSkippedBetter++
		} else if c.ExclusionReason != marketExclusionNotSelectedWorseRank {
			return errors.New("push order book worse-ranked peer has unexpected exclusion_reason")
		}
	}
	if !sawSelected {
		return errors.New("push order book considered set omits the selected offer")
	}
	// Under SKIP LOCKED, selected_rank > 1 implies at least one better peer was
	// lock-skipped. Blocking FOR UPDATE never admits rank > 1 when capacity
	// exists, so a rank>1 book under blocking is refused as incoherent.
	if book.SelectedRank > 1 {
		if book.AvailabilityMode != marketAvailabilitySkipLocked {
			return errors.New("push order book selected_rank > 1 requires SKIP_LOCKED availability mode")
		}
		if lockSkippedBetter != book.SelectedRank-1 {
			return errors.New("push order book lock-skipped count disagrees with selected_rank")
		}
	}
	if book.ReferenceCurrency == "" || book.SettlementCurrency == "" ||
		book.SupplierRateCurrency != book.ReferenceCurrency ||
		book.BuyerMoneyCurrency != book.SettlementCurrency ||
		(book.RankingInputs.RateCurrency != "" && book.RankingInputs.RateCurrency != book.ReferenceCurrency) {
		return errors.New("push order book currency map is incomplete or inconsistent")
	}
	return nil
}

// marketDecisionDigest is the immutable identity of an accepted MarketDecision.
func marketDecisionDigest(md MarketDecision) (string, error) {
	if err := ValidateMarketDecision(md); err != nil {
		return "", err
	}
	blob, err := json.Marshal(md)
	if err != nil {
		return "", fmt.Errorf("marshal market decision: %w", err)
	}
	return canonicalDigestBytes(blob), nil
}

func marketDecisionDigestFromJSON(md MarketDecision, blob []byte) (string, error) {
	if err := ValidateMarketDecision(md); err != nil {
		return "", err
	}
	return canonicalDigestBytes(blob), nil
}

// projectRealtimeMarketClearingReceipt projects the legacy realtime receipt
// losslessly from a PUSH_ORDER_BOOK MarketDecision. Every money field, rank,
// ranking input, and selection reason is copied — not recomputed.
func projectRealtimeMarketClearingReceipt(md MarketDecision) (*RealtimeMarketClearingReceipt, error) {
	if err := ValidateMarketDecision(md); err != nil {
		return nil, err
	}
	if md.MarketShape != marketShapePushOrderBook || md.PushOrderBook == nil {
		return nil, errors.New("realtime market-clearing receipt projects only from PUSH_ORDER_BOOK")
	}
	book := md.PushOrderBook
	return &RealtimeMarketClearingReceipt{
		Version:                     3,
		ReferenceCurrency:           book.ReferenceCurrency,
		SettlementCurrency:          book.SettlementCurrency,
		SupplierRateCurrency:        book.SupplierRateCurrency,
		BuyerMoneyCurrency:          book.BuyerMoneyCurrency,
		CandidateCount:              book.CandidateCount,
		SelectedRank:                book.SelectedRank,
		SelectedWorkerID:            book.SelectedWorkerID,
		SelectedSupplierID:          book.SelectedSupplierID,
		SelectedSupplierInputNanos:  book.SelectedSupplierInputNanos,
		SelectedSupplierOutputNanos: book.SelectedSupplierOutputNanos,
		BuyerCeilingNanos:           book.BuyerCeilingNanos,
		AcceptedCeilingNanos:        book.AcceptedCeilingNanos,
		PricingDecisionSHA256:       book.PricingDecisionSHA256,
		PositiveContributionNanos:   book.PositiveContributionNanos,
		OrderBookPolicy:             book.OrderBookPolicy,
		SelectionReason:             book.SelectionReason,
		RankingInputs:               book.RankingInputs,
	}, nil
}

// newRealtimePushMarketDecision freezes the realtime push book at claim time.
// consideredJSON is the candidates CTE snapshot returned with the claim row.
// A PULL body cannot be written through this builder.
func newRealtimePushMarketDecision(
	availabilityMode string,
	candidateCount, selectedRank int,
	workerID, supplierID uuid.UUID,
	supplierInput, supplierOutput float64,
	pricing PricingDecision, pricingSHA256 string,
	inputs RealtimeClearingRankingInputs,
	consideredJSON []byte,
) (MarketDecision, error) {
	if candidateCount <= 0 || selectedRank <= 0 || selectedRank > candidateCount ||
		workerID == uuid.Nil || supplierID == uuid.Nil || pricing.Realtime == nil || pricing.FixedPoint == nil {
		return MarketDecision{}, errors.New("realtime push market decision candidate evidence is invalid")
	}
	switch availabilityMode {
	case marketAvailabilitySkipLocked, marketAvailabilityBlockingForUpdate:
	default:
		return MarketDecision{}, fmt.Errorf("realtime push market decision has unknown availability_mode %q", availabilityMode)
	}
	supplierInputNanos, err := nanoRatePerMillionFromFloat(supplierInput)
	if err != nil {
		return MarketDecision{}, err
	}
	supplierOutputNanos, err := nanoRatePerMillionFromFloat(supplierOutput)
	if err != nil {
		return MarketDecision{}, err
	}
	if inputs.SelectedSupplierInputNanos != int64(supplierInputNanos) ||
		inputs.SelectedSupplierOutputNanos != int64(supplierOutputNanos) ||
		inputs.BaseAskNanos <= 0 || inputs.VerifiedOutcomeCostNanos <= 0 {
		return MarketDecision{}, errors.New("realtime push market decision ranking inputs disagree with selected offer rates")
	}
	inputs.OmittedTerms = realtimeClearingOmittedTerms()
	inputs.RateCurrency = pricing.Realtime.ReferenceCurrency
	if inputs.WarmthRank == 0 && inputs.Warmth != "HOT" {
		inputs.WarmthRank = warmthRank(inputs.Warmth)
	}

	considered, err := decodeAndAnnotateMarketBook(
		consideredJSON, candidateCount, selectedRank, workerID, supplierID, availabilityMode)
	if err != nil {
		return MarketDecision{}, err
	}

	book := &MarketPushOrderBook{
		AvailabilityMode:            availabilityMode,
		OrderBookPolicy:             realtimeOrderBookPolicy,
		CandidateCount:              candidateCount,
		SelectedRank:                selectedRank,
		SelectedWorkerID:            workerID,
		SelectedSupplierID:          supplierID,
		SelectedSupplierInputNanos:  int64(supplierInputNanos),
		SelectedSupplierOutputNanos: int64(supplierOutputNanos),
		BuyerCeilingNanos:           pricing.Realtime.BuyerDeclaredCeilingNanos,
		AcceptedCeilingNanos:        pricing.FixedPoint.AcceptedCeilingNanos,
		PricingDecisionSHA256:       pricingSHA256,
		PositiveContributionNanos:   pricing.FixedPoint.KnownCostContributionNanos,
		ReferenceCurrency:           pricing.Realtime.ReferenceCurrency,
		SettlementCurrency:          pricing.Currency,
		SupplierRateCurrency:        pricing.Realtime.ReferenceCurrency,
		BuyerMoneyCurrency:          pricing.Currency,
		SelectionReason:             realtimeClearingSelectionReason(inputs, selectedRank, candidateCount),
		RankingInputs:               &inputs,
		Considered:                  considered,
	}
	md := MarketDecision{
		Version:       marketDecisionVersion,
		MarketShape:   marketShapePushOrderBook,
		PushOrderBook: book,
	}
	if err := ValidateMarketDecision(md); err != nil {
		return MarketDecision{}, err
	}
	if book.PositiveContributionNanos <= 0 || !validSHA256(pricingSHA256) {
		return MarketDecision{}, errors.New("realtime push market decision lacks positive PricingDecision contribution")
	}
	return md, nil
}

func decodeAndAnnotateMarketBook(
	raw []byte,
	candidateCount, selectedRank int,
	selectedWorker, selectedSupplier uuid.UUID,
	availabilityMode string,
) ([]MarketBookCandidate, error) {
	if len(raw) == 0 {
		return nil, errors.New("realtime push market decision lacks considered book snapshot")
	}
	var rows []marketBookCandidateJSON
	if err := json.Unmarshal(raw, &rows); err != nil {
		return nil, fmt.Errorf("decode realtime push considered book: %w", err)
	}
	if len(rows) != candidateCount {
		return nil, fmt.Errorf("considered book size %d disagrees with candidate_count %d", len(rows), candidateCount)
	}
	out := make([]MarketBookCandidate, 0, len(rows))
	for _, row := range rows {
		workerID, err := uuid.Parse(strings.TrimSpace(row.WorkerID))
		if err != nil || workerID == uuid.Nil {
			return nil, errors.New("considered book candidate has invalid worker_id")
		}
		supplierID, err := uuid.Parse(strings.TrimSpace(row.SupplierID))
		if err != nil || supplierID == uuid.Nil {
			return nil, errors.New("considered book candidate has invalid supplier_id")
		}
		c := MarketBookCandidate{
			Rank:                row.Rank,
			WorkerID:            workerID,
			SupplierID:          supplierID,
			Warmth:              row.Warmth,
			VerifiedOutcomeCost: row.VerifiedOutcomeCost,
		}
		switch {
		case c.Rank == selectedRank && c.WorkerID == selectedWorker && c.SupplierID == selectedSupplier:
			// Selected: no exclusion reason.
		case c.Rank < selectedRank:
			if availabilityMode != marketAvailabilitySkipLocked {
				return nil, errors.New("better-ranked peer under non-SKIP mode is incoherent")
			}
			c.ExclusionReason = marketExclusionLockSkipped
		default:
			c.ExclusionReason = marketExclusionNotSelectedWorseRank
		}
		out = append(out, c)
	}
	return out, nil
}

// refuseRealtimePullMarketDecision is the explicit shape guard: the realtime
// authorize path cannot write a PULL_ELIGIBILITY_SNAPSHOT body.
func refuseRealtimePullMarketDecision() error {
	return errors.New("realtime lane refuses PULL_ELIGIBILITY_SNAPSHOT market_shape; use PUSH_ORDER_BOOK")
}
