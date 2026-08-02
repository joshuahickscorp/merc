package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

var (
	errRealtimeNoSupply            = errors.New("no active compatible realtime supply")
	errRealtimeIdempotencyConflict = errors.New("idempotency key was already used for a different realtime request")
	errRealtimeAlreadyFinalized    = errors.New("realtime contract is already finalized")
	errRealtimeInsufficientFunds   = errors.New("insufficient authorized balance for the maximum request price")
	// errRealtimeTopupRequired is returned when a saved card is present but the
	// buyer lacks free credit + prepaid balance to cover the contract ceiling.
	// A card is a top-up rail, not authorization funding; the buyer must top up
	// prepaid balance before retrying. Do not auto-charge the card.
	errRealtimeTopupRequired       = errors.New("insufficient prepaid balance for the maximum request price; top up prepaid balance before retrying")
	errRealtimeNotRefundable       = errors.New("realtime settlement is not eligible for an internal refund")
	errRealtimeRefundNeedsReversal = errors.New("supplier payout crossed the internal transfer boundary; external reversal is required")
)

// evaluateRealtimeBuyerFunding locks the buyer row and requires already-settled
// money (free-credit grant + materialised prepaid balance, net of charges,
// prepaid debits, open batch estimates, and EXECUTING realtime ceilings) to
// cover needUSD. A saved payment method is never treated as funding.
//
// Serialization is two-step on purpose: FOR UPDATE on the buyer row alone is
// not enough under READ COMMITTED when the reservation itself is written to a
// different table (execution_contracts). Concurrent authorizers that only
// locked buyers could each observe realtimeReserved=0, all pass, then each
// insert an EXECUTING row — overspending a prepaid balance that funds a single
// ceiling. An advisory xact lock keyed on the buyer forces authorizers for the
// same buyer to enter the check-and-reserve critical section one at a time for
// the rest of the transaction (through the EXECUTING insert and commit).
func evaluateRealtimeBuyerFunding(ctx context.Context, tx pgx.Tx, buyerID uuid.UUID, needUSD float64) error {
	if needUSD < 0 {
		return fmt.Errorf("realtime funding need must be non-negative")
	}
	if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtextextended($1, 0))`,
		"realtime-buyer-funding|"+buyerID.String()); err != nil {
		return err
	}
	var (
		freeCredit, spent, prepaidDebited, batchReserved, realtimeReserved float64
		prepaidMicros                                                      int64
		hasPaymentMethod                                                   bool
	)
	err := tx.QueryRow(ctx, `
		SELECT b.free_credit_usd::float8,
		       EXISTS(SELECT 1 FROM billing_customers bc
		               WHERE bc.buyer_id=b.id AND COALESCE(bc.default_payment_method,'')<>''),
		       COALESCE((SELECT balance_micros FROM buyer_prepaid_balances bp
		                  WHERE bp.buyer_id=b.id),0)::bigint,
		       COALESCE((SELECT -sum(le.amount_usd) FROM ledger_entries le
		                 WHERE le.buyer_id=b.id
		                   AND le.kind IN ('buyer_charge','buyer_refund')),0)::float8,
		       COALESCE((SELECT -sum(le.amount_usd) FROM ledger_entries le
		                 WHERE le.buyer_id=b.id AND le.kind='prepaid_debit'),0)::float8,
		       COALESCE((SELECT sum(j.estimated_usd) FROM jobs j
		                 WHERE j.buyer_id=b.id AND j.status IN ('queued','running','verifying')),0)::float8,
		       COALESCE((SELECT sum(c.maximum_price_usd) FROM execution_contracts c
		                 WHERE c.buyer_id=b.id AND c.state='EXECUTING'),0)::float8
		  FROM buyers b WHERE b.id=$1 AND b.deleted_at IS NULL FOR UPDATE`, buyerID).
		Scan(&freeCredit, &hasPaymentMethod, &prepaidMicros, &spent, &prepaidDebited, &batchReserved, &realtimeReserved)
	if errors.Is(err, pgx.ErrNoRows) {
		return errNotFound
	}
	if err != nil {
		return err
	}
	// Compare in integer micros so a sub-cent ceiling cannot be over-admitted
	// by float residual after several concurrent reservations.
	needMicros := usdToMicros(needUSD)
	if needMicros < 0 {
		return fmt.Errorf("realtime funding need must be non-negative")
	}
	availableMicros := usdToMicros(freeCredit) + prepaidMicros -
		usdToMicros(spent) + usdToMicros(prepaidDebited) -
		usdToMicros(batchReserved) - usdToMicros(realtimeReserved)
	if availableMicros >= needMicros {
		return nil
	}
	if hasPaymentMethod {
		return errRealtimeTopupRequired
	}
	return errRealtimeInsufficientFunds
}

type RealtimeOfferRegistration struct {
	RuntimeProfileID                  string  `json:"runtime_profile_id"`
	RuntimeProfileSHA256              string  `json:"runtime_profile_sha256"`
	HWClass                           string  `json:"hw_class"`
	GPUCount                          int     `json:"gpu_count"`
	MemoryGBPerGPU                    float64 `json:"memory_gb_per_gpu"`
	MemoryGBInUse                     float64 `json:"memory_gb_in_use"`
	Interconnect                      string  `json:"interconnect,omitempty"`
	UpstreamBaseURL                   string  `json:"upstream_base_url"`
	UpstreamToken                     string  `json:"upstream_token"`
	Warmth                            string  `json:"warmth"`
	MaxActiveSequences                int     `json:"max_active_sequences"`
	AvailableSequences                int     `json:"available_sequences"`
	SupplierInputUSDPerMillionTokens  float64 `json:"supplier_input_usd_per_million_tokens"`
	SupplierOutputUSDPerMillionTokens float64 `json:"supplier_output_usd_per_million_tokens"`
}

type RealtimeOfferHeartbeat struct {
	RuntimeProfileID   string `json:"runtime_profile_id"`
	Warmth             string `json:"warmth"`
	AvailableSequences int    `json:"available_sequences"`
	Status             string `json:"status"`
}

type RealtimeContract struct {
	ID                                uuid.UUID
	RequestID                         string
	BuyerID                           uuid.UUID
	ModelAlias                        string
	RuntimeProfileID                  string
	RuntimeProfileSHA256              string
	InputCommitment                   string
	RequestSHA256                     string
	PlacementPlan                     RealtimePlacementPlan
	PlacementPlanSHA256               string
	MaximumPriceUSD                   float64
	EstimatedPriceUSD                 float64
	BuyerInputUSDPerMillionTokens     float64
	BuyerOutputUSDPerMillionTokens    float64
	SupplierInputUSDPerMillionTokens  float64
	SupplierOutputUSDPerMillionTokens float64
	DeadlineAt                        time.Time
	State                             string
	WorkerID                          uuid.UUID
	SupplierID                        uuid.UUID
	UpstreamBaseURL                   string
	UpstreamToken                     string
	MaximumPromptTokens               int64
	MaximumCompletionTokens           int64
	EstimatedPromptTokens             int64
	EstimatedCompletionTokens         int64
	BuyerDeclaredCeilingNanos         int64
	ReuseClass                        string
	ReuseResultCommitment             string
	ReuseDeliveredTokens              int64
	Pricing                           *PricingDecision
	PricingDecisionSHA256             string
	MarketClearing                    *RealtimeMarketClearingReceipt
	Currency                          string
}

// RealtimeMarketClearingReceipt is the immutable result of crossing one
// realtime buyer order against the live supplier offer book. It is an
// observation of the candidates considered by the atomic reservation, not a
// second pricing authority: all money fields are copied from the frozen
// PricingDecision and retained in fixed-point nanos.
//
// RankingInputs freezes every signal that ordered the offer book so a buyer
// can see why a supplier cleared and a ranking regression is visible rather
// than silent.
type RealtimeMarketClearingReceipt struct {
	Version                     int                            `json:"version"`
	CandidateCount              int                            `json:"candidate_count"`
	SelectedRank                int                            `json:"selected_rank"`
	SelectedWorkerID            uuid.UUID                      `json:"selected_worker_id"`
	SelectedSupplierID          uuid.UUID                      `json:"selected_supplier_id"`
	SelectedSupplierInputNanos  int64                          `json:"selected_supplier_input_nanos_per_million_tokens"`
	SelectedSupplierOutputNanos int64                          `json:"selected_supplier_output_nanos_per_million_tokens"`
	BuyerCeilingNanos           int64                          `json:"buyer_ceiling_nanos"`
	AcceptedCeilingNanos        int64                          `json:"accepted_ceiling_nanos"`
	PricingDecisionSHA256       string                         `json:"pricing_decision_sha256"`
	PositiveContributionNanos   int64                          `json:"positive_contribution_nanos"`
	OrderBookPolicy             string                         `json:"order_book_policy"`
	SelectionReason             string                         `json:"selection_reason"`
	RankingInputs               *RealtimeClearingRankingInputs `json:"ranking_inputs,omitempty"`
}

type RealtimeContractAuthorization struct {
	RequestID                 string
	BuyerID                   uuid.UUID
	Profile                   VLLMRuntimeProfile
	InputCommitment           string
	RequestSHA256             string
	MaximumPriceUSD           float64
	EstimatedPriceUSD         float64
	DeadlineAt                time.Time
	IdempotencyKey            string
	MaximumPromptTokens       int64
	MaximumCompletionTokens   int64
	EstimatedPromptTokens     int64
	EstimatedCompletionTokens int64
	BuyerDeclaredCeilingUSD   float64
	ReuseClass                string
	// CoalescedLeaderContractID is required only for an in-flight follower. It
	// makes the physical source of a zero-physical settlement durable and lets
	// the receipt distinguish an avoided entitlement from true net contribution.
	CoalescedLeaderContractID uuid.UUID
}

func (s *Store) UpsertRealtimeOffer(ctx context.Context, worker WorkerAuth, registration RealtimeOfferRegistration) error {
	profile, ok := vllmProfileByID(registration.RuntimeProfileID)
	if !ok || registration.RuntimeProfileSHA256 != profile.ProfileSHA256 {
		return errors.New("runtime profile does not match control-plane authority")
	}
	if err := validateRealtimeOfferRates(profile, registration); err != nil {
		return err
	}
	plan, err := newRealtimePlacementPlan(profile, registration)
	if err != nil {
		return fmt.Errorf("build realtime placement plan: %w", err)
	}
	placementJSON, placementSHA256, err := encodeRealtimePlacementPlan(plan)
	if err != nil {
		return fmt.Errorf("encode realtime placement plan: %w", err)
	}
	sealed := sealToken(registration.UpstreamToken)
	if !strings.HasPrefix(sealed, "enc:") {
		return errors.New("MERC_TOKEN_KEY is required to seal the vLLM upstream credential")
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	tag, err := tx.Exec(ctx, `
		UPDATE workers SET last_seen_at=now()
		 WHERE id=$1 AND supplier_id=$2`, worker.WorkerID, worker.SupplierID)
	if err != nil {
		return err
	}
	if tag.RowsAffected() != 1 {
		return errNotFound
	}
	_, err = tx.Exec(ctx, `
		INSERT INTO realtime_worker_offers
		  (worker_id,supplier_id,runtime_profile_id,runtime_profile_sha256,
		   placement_plan,placement_plan_sha256,
		   upstream_base_url,upstream_token_sealed,warmth,max_active_sequences,
		   available_sequences,supplier_input_usd_per_million_tokens,
		   supplier_output_usd_per_million_tokens,status,last_seen_at,updated_at)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,'ACTIVE',now(),now())
		ON CONFLICT (worker_id,runtime_profile_id) DO UPDATE SET
		  supplier_id=EXCLUDED.supplier_id,
		  runtime_profile_sha256=EXCLUDED.runtime_profile_sha256,
		  placement_plan=EXCLUDED.placement_plan,
		  placement_plan_sha256=EXCLUDED.placement_plan_sha256,
		  upstream_base_url=EXCLUDED.upstream_base_url,
		  upstream_token_sealed=EXCLUDED.upstream_token_sealed,
		  warmth=EXCLUDED.warmth,
		  max_active_sequences=EXCLUDED.max_active_sequences,
		  available_sequences=EXCLUDED.available_sequences,
		  supplier_input_usd_per_million_tokens=EXCLUDED.supplier_input_usd_per_million_tokens,
		  supplier_output_usd_per_million_tokens=EXCLUDED.supplier_output_usd_per_million_tokens,
		  status='ACTIVE',last_seen_at=now(),updated_at=now()`,
		worker.WorkerID, worker.SupplierID, registration.RuntimeProfileID,
		registration.RuntimeProfileSHA256, placementJSON, placementSHA256,
		registration.UpstreamBaseURL, sealed,
		registration.Warmth, registration.MaxActiveSequences, registration.AvailableSequences,
		registration.SupplierInputUSDPerMillionTokens, registration.SupplierOutputUSDPerMillionTokens)
	if err != nil {
		return err
	}
	// The mutable offer answers "what is available now". A sample answers the
	// separate, time-bounded operations question: did demand find capacity, and
	// how much of it stayed occupied? Keep the latter prompt-free and outside
	// every admission and money decision.
	if _, err := tx.Exec(ctx, `
		INSERT INTO realtime_offer_samples
		  (worker_id,supplier_id,runtime_profile_id,runtime_profile_sha256,hw_class,
		   status,max_active_sequences,available_sequences,
		   supplier_input_usd_per_million_tokens,supplier_output_usd_per_million_tokens)
		VALUES ($1,$2,$3,$4,$5,'ACTIVE',$6,$7,$8,$9)`,
		worker.WorkerID, worker.SupplierID, registration.RuntimeProfileID,
		registration.RuntimeProfileSHA256, plan.HWClass, registration.MaxActiveSequences,
		registration.AvailableSequences, registration.SupplierInputUSDPerMillionTokens,
		registration.SupplierOutputUSDPerMillionTokens); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

func (s *Store) HeartbeatRealtimeOffer(ctx context.Context, worker WorkerAuth, hb RealtimeOfferHeartbeat) error {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	var (
		profileSHA string
		hwClass    string
		maxActive  int
		available  int
		inputRate  float64
		outputRate float64
	)
	err = tx.QueryRow(ctx, `
		UPDATE realtime_worker_offers AS o
		   SET warmth=$4,
		       available_sequences=GREATEST(0,LEAST($5,o.max_active_sequences-(
		           SELECT count(*)::int FROM execution_contracts c
		            WHERE c.worker_id=$1 AND c.runtime_profile_id=$3
		              AND c.state='EXECUTING'))),
		       status=$6,last_seen_at=now(),updated_at=now()
		  FROM workers AS w
		 WHERE o.worker_id=$1 AND o.supplier_id=$2 AND o.runtime_profile_id=$3
		   AND w.id=o.worker_id
		   AND $5 BETWEEN 0 AND o.max_active_sequences
		RETURNING o.runtime_profile_sha256,
	          COALESCE(NULLIF(o.placement_plan->>'hw_class',''),w.hw_class),o.max_active_sequences,
	          o.available_sequences,o.supplier_input_usd_per_million_tokens,
	          o.supplier_output_usd_per_million_tokens`,
		worker.WorkerID, worker.SupplierID, hb.RuntimeProfileID, hb.Warmth,
		hb.AvailableSequences, hb.Status).Scan(&profileSHA, &hwClass, &maxActive,
		&available, &inputRate, &outputRate)
	if errors.Is(err, pgx.ErrNoRows) {
		return errNotFound
	}
	if err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, `
		INSERT INTO realtime_offer_samples
		  (worker_id,supplier_id,runtime_profile_id,runtime_profile_sha256,hw_class,
		   status,max_active_sequences,available_sequences,
		   supplier_input_usd_per_million_tokens,supplier_output_usd_per_million_tokens)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10)`,
		worker.WorkerID, worker.SupplierID, hb.RuntimeProfileID, profileSHA, hwClass,
		hb.Status, maxActive, available, inputRate, outputRate); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

func scanRealtimeContract(row pgx.Row) (RealtimeContract, error) {
	var contract RealtimeContract
	var sealed *string
	var workerID, supplierID *uuid.UUID
	var upstream *string
	var placementJSON []byte
	var placementSHA256 *string
	var pricingJSON []byte
	var marketJSON []byte
	err := row.Scan(
		&contract.ID, &contract.RequestID, &contract.BuyerID, &contract.ModelAlias,
		&contract.RuntimeProfileID, &contract.RuntimeProfileSHA256,
		&contract.InputCommitment, &contract.RequestSHA256,
		&placementJSON, &placementSHA256,
		&contract.MaximumPriceUSD, &contract.EstimatedPriceUSD,
		&contract.BuyerInputUSDPerMillionTokens, &contract.BuyerOutputUSDPerMillionTokens,
		&contract.SupplierInputUSDPerMillionTokens, &contract.SupplierOutputUSDPerMillionTokens,
		&contract.DeadlineAt, &contract.State, &workerID, &supplierID,
		&upstream, &sealed, &contract.Currency,
		&contract.MaximumPromptTokens, &contract.MaximumCompletionTokens,
		&contract.EstimatedPromptTokens, &contract.EstimatedCompletionTokens,
		&contract.BuyerDeclaredCeilingNanos, &contract.ReuseClass,
		&contract.ReuseResultCommitment, &contract.ReuseDeliveredTokens,
		&pricingJSON, &contract.PricingDecisionSHA256, &marketJSON)
	if err != nil {
		return RealtimeContract{}, err
	}
	if workerID != nil {
		contract.WorkerID = *workerID
	}
	if supplierID != nil {
		contract.SupplierID = *supplierID
	}
	if upstream != nil {
		contract.UpstreamBaseURL = *upstream
	}
	if len(placementJSON) > 0 && placementSHA256 != nil {
		plan, err := decodeRealtimePlacementPlan(placementJSON, *placementSHA256)
		if err != nil {
			return RealtimeContract{}, err
		}
		if plan.RuntimeProfileID != contract.RuntimeProfileID ||
			plan.RuntimeProfileSHA256 != contract.RuntimeProfileSHA256 {
			return RealtimeContract{}, errors.New("realtime placement plan does not match contract profile")
		}
		contract.PlacementPlan = plan
		contract.PlacementPlanSHA256 = *placementSHA256
	}
	if err := attachRealtimeContractPricing(&contract, pricingJSON); err != nil {
		return RealtimeContract{}, err
	}
	if err := attachRealtimeMarketClearing(&contract, marketJSON); err != nil {
		return RealtimeContract{}, err
	}
	// Settlement and receipt reads never need to decrypt an upstream bearer
	// token. The binding-shape constraint guarantees physical rows carry one;
	// only the idempotent execution replay path opens it when another upstream
	// call may actually be made.
	return contract, nil
}

func attachRealtimeMarketClearing(contract *RealtimeContract, raw []byte) error {
	if len(raw) == 0 {
		return nil
	}
	var market RealtimeMarketClearingReceipt
	if err := json.Unmarshal(raw, &market); err != nil {
		return fmt.Errorf("decode realtime market-clearing receipt: %w", err)
	}
	if market.Version != 1 || market.CandidateCount <= 0 ||
		market.SelectedRank <= 0 || market.SelectedRank > market.CandidateCount ||
		market.SelectedWorkerID == uuid.Nil || market.SelectedSupplierID == uuid.Nil ||
		market.SelectedSupplierInputNanos <= 0 || market.SelectedSupplierOutputNanos <= 0 ||
		market.BuyerCeilingNanos < 0 || market.AcceptedCeilingNanos <= 0 ||
		(market.BuyerCeilingNanos > 0 && market.AcceptedCeilingNanos > market.BuyerCeilingNanos) ||
		market.PositiveContributionNanos <= 0 ||
		!validSHA256(market.PricingDecisionSHA256) ||
		(market.OrderBookPolicy != realtimeOrderBookPolicy &&
			market.OrderBookPolicy != "lowest_warmth_then_supplier_rate_v1") ||
		strings.TrimSpace(market.SelectionReason) == "" {
		return errors.New("realtime market-clearing receipt has invalid bounded authority")
	}
	// New receipts must carry ranking inputs so a buyer can see why a supplier
	// cleared. Historical rows written under the warmth-first policy predate
	// that field and are still attachable (read path must not break).
	if market.OrderBookPolicy == realtimeOrderBookPolicy {
		if market.RankingInputs == nil ||
			market.RankingInputs.BaseAskNanos <= 0 ||
			market.RankingInputs.VerifiedOutcomeCostNanos <= 0 ||
			market.RankingInputs.SelectedSupplierInputNanos != market.SelectedSupplierInputNanos ||
			market.RankingInputs.SelectedSupplierOutputNanos != market.SelectedSupplierOutputNanos ||
			len(market.RankingInputs.OmittedTerms) == 0 {
			return errors.New("realtime market-clearing receipt lacks ranking inputs")
		}
	}
	if contract.WorkerID == uuid.Nil || contract.SupplierID == uuid.Nil ||
		market.SelectedWorkerID != contract.WorkerID || market.SelectedSupplierID != contract.SupplierID {
		return errors.New("realtime market-clearing receipt does not bind selected offer to contract")
	}
	if contract.PricingDecisionSHA256 == "" || market.PricingDecisionSHA256 != contract.PricingDecisionSHA256 {
		return errors.New("realtime market-clearing receipt does not bind PricingDecision")
	}
	if contract.Pricing == nil || contract.Pricing.FixedPoint == nil ||
		market.AcceptedCeilingNanos != contract.Pricing.FixedPoint.AcceptedCeilingNanos ||
		market.PositiveContributionNanos != contract.Pricing.FixedPoint.KnownCostContributionNanos {
		return errors.New("realtime market-clearing receipt disagrees with PricingDecision")
	}
	contract.MarketClearing = &market
	return nil
}

func newRealtimeMarketClearingReceipt(
	candidateCount, selectedRank int, workerID, supplierID uuid.UUID,
	supplierInput, supplierOutput float64, pricing PricingDecision, pricingSHA256 string,
	inputs RealtimeClearingRankingInputs,
) (*RealtimeMarketClearingReceipt, error) {
	if candidateCount <= 0 || selectedRank <= 0 || selectedRank > candidateCount ||
		workerID == uuid.Nil || supplierID == uuid.Nil || pricing.Realtime == nil || pricing.FixedPoint == nil {
		return nil, errors.New("realtime market-clearing candidate evidence is invalid")
	}
	supplierInputNanos, err := nanoRatePerMillionFromFloat(supplierInput)
	if err != nil {
		return nil, err
	}
	supplierOutputNanos, err := nanoRatePerMillionFromFloat(supplierOutput)
	if err != nil {
		return nil, err
	}
	// Ranking inputs are authoritative for the selected offer's rates; the
	// float path above is only the PricingDecision hand-off. They must agree.
	if inputs.SelectedSupplierInputNanos != int64(supplierInputNanos) ||
		inputs.SelectedSupplierOutputNanos != int64(supplierOutputNanos) ||
		inputs.BaseAskNanos <= 0 || inputs.VerifiedOutcomeCostNanos <= 0 {
		return nil, errors.New("realtime market-clearing ranking inputs disagree with selected offer rates")
	}
	inputs.OmittedTerms = realtimeClearingOmittedTerms()
	if inputs.WarmthRank == 0 && inputs.Warmth != "HOT" {
		inputs.WarmthRank = warmthRank(inputs.Warmth)
	}
	market := &RealtimeMarketClearingReceipt{
		Version:                     1,
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
		OrderBookPolicy:             realtimeOrderBookPolicy,
		SelectionReason:             realtimeClearingSelectionReason(inputs),
		RankingInputs:               &inputs,
	}
	if market.PositiveContributionNanos <= 0 || !validSHA256(pricingSHA256) {
		return nil, errors.New("realtime market-clearing receipt lacks positive PricingDecision contribution")
	}
	return market, nil
}

const realtimeContractColumns = `
	 id,request_id,buyer_id,model_alias,runtime_profile_id,runtime_profile_sha256,
	 input_commitment,request_sha256,
	 placement_plan,placement_plan_sha256,
	 maximum_price_usd::float8,estimated_price_usd::float8,
	 buyer_input_usd_per_million_tokens::float8,buyer_output_usd_per_million_tokens::float8,
	 supplier_input_usd_per_million_tokens::float8,supplier_output_usd_per_million_tokens::float8,
	 deadline_at,state,worker_id,supplier_id,upstream_base_url,upstream_token_sealed,currency,
	 COALESCE(maximum_prompt_tokens,0),COALESCE(maximum_completion_tokens,0),
	 COALESCE(estimated_prompt_tokens,0),COALESCE(estimated_completion_tokens,0),
	 COALESCE(buyer_declared_ceiling_nanos,0),COALESCE(reuse_class,''),
	 COALESCE(reuse_result_commitment,''),COALESCE(reuse_delivered_tokens,0),pricing_decision,
	 COALESCE(pricing_decision_sha256,''),market_clearing`

func attachRealtimeContractPricing(contract *RealtimeContract, raw []byte) error {
	if len(raw) == 0 {
		return nil
	}
	var pricing PricingDecision
	if err := json.Unmarshal(raw, &pricing); err != nil {
		return fmt.Errorf("decode realtime PricingDecision: %w", err)
	}
	digest, err := pricingDecisionDigest(pricing)
	if err != nil || digest != contract.PricingDecisionSHA256 {
		return errors.New("realtime PricingDecision digest mismatch")
	}
	profile, ok := vllmProfileByID(contract.RuntimeProfileID)
	if !ok || profile.ProfileSHA256 != contract.RuntimeProfileSHA256 {
		return errors.New("realtime contract profile authority disappeared")
	}
	currency, err := ParseCurrency(contract.Currency)
	if err != nil {
		return err
	}
	declaredCeiling := float64(contract.BuyerDeclaredCeilingNanos) / float64(NanosPerMajorUnit)
	switch pricing.ExecutionMode {
	case pricingExecutionRealtime:
		if err := ValidateRealtimePricingDecisionSnapshot(pricing, RealtimePricingInputs{
			Profile: profile, Placement: contract.PlacementPlan,
			InputCommitment: contract.InputCommitment, RequestSHA256: contract.RequestSHA256,
			MaximumPromptTokens: contract.MaximumPromptTokens, MaximumCompletionTokens: contract.MaximumCompletionTokens,
			EstimatedPromptTokens: contract.EstimatedPromptTokens, EstimatedCompletionTokens: contract.EstimatedCompletionTokens,
			SupplierInputRate:    contract.SupplierInputUSDPerMillionTokens,
			SupplierOutputRate:   contract.SupplierOutputUSDPerMillionTokens,
			BuyerDeclaredCeiling: declaredCeiling, Currency: currency,
		}); err != nil {
			return err
		}
	case pricingExecutionRealtimeReuse:
		if err := ValidateRealtimeReusePricingDecisionSnapshot(pricing, RealtimeReusePricingInputs{
			Profile: profile, InputCommitment: contract.InputCommitment, RequestSHA256: contract.RequestSHA256,
			ResultCommitment: contract.ReuseResultCommitment, ReuseClass: contract.ReuseClass,
			DeliveredTokens: contract.ReuseDeliveredTokens, BuyerDeclaredCeiling: declaredCeiling, Currency: currency,
		}); err != nil {
			return err
		}
	default:
		return errors.New("realtime contract PricingDecision has unsupported execution mode")
	}
	expected, maximum, err := realtimePricingLegacyProjection(pricing)
	if err != nil || expected != contract.EstimatedPriceUSD || maximum != contract.MaximumPriceUSD {
		return errors.New("realtime PricingDecision legacy projection mismatch")
	}
	contract.Pricing = &pricing
	return nil
}

func (s *Store) AuthorizeRealtimeContract(ctx context.Context, auth RealtimeContractAuthorization) (RealtimeContract, bool, error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return RealtimeContract{}, false, err
	}
	defer tx.Rollback(ctx)

	if auth.IdempotencyKey != "" {
		if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtextextended($1,0))`, auth.BuyerID.String()+":"+auth.IdempotencyKey); err != nil {
			return RealtimeContract{}, false, err
		}
		row := tx.QueryRow(ctx, `SELECT `+realtimeContractColumns+`
			FROM execution_contracts WHERE buyer_id=$1 AND idempotency_key=$2`,
			auth.BuyerID, auth.IdempotencyKey)
		var contract RealtimeContract
		var sealed *string
		var workerID, supplierID *uuid.UUID
		var upstream *string
		var placementJSON []byte
		var placementSHA256 *string
		var pricingJSON []byte
		var marketJSON []byte
		err := row.Scan(
			&contract.ID, &contract.RequestID, &contract.BuyerID, &contract.ModelAlias,
			&contract.RuntimeProfileID, &contract.RuntimeProfileSHA256,
			&contract.InputCommitment, &contract.RequestSHA256,
			&placementJSON, &placementSHA256,
			&contract.MaximumPriceUSD, &contract.EstimatedPriceUSD,
			&contract.BuyerInputUSDPerMillionTokens, &contract.BuyerOutputUSDPerMillionTokens,
			&contract.SupplierInputUSDPerMillionTokens, &contract.SupplierOutputUSDPerMillionTokens,
			&contract.DeadlineAt, &contract.State, &workerID, &supplierID,
			&upstream, &sealed, &contract.Currency,
			&contract.MaximumPromptTokens, &contract.MaximumCompletionTokens,
			&contract.EstimatedPromptTokens, &contract.EstimatedCompletionTokens,
			&contract.BuyerDeclaredCeilingNanos, &contract.ReuseClass,
			&contract.ReuseResultCommitment, &contract.ReuseDeliveredTokens,
			&pricingJSON, &contract.PricingDecisionSHA256, &marketJSON)
		if err == nil {
			if contract.RequestSHA256 != auth.RequestSHA256 {
				return RealtimeContract{}, false, errRealtimeIdempotencyConflict
			}
			if workerID != nil {
				contract.WorkerID = *workerID
			}
			if supplierID != nil {
				contract.SupplierID = *supplierID
			}
			if upstream != nil {
				contract.UpstreamBaseURL = *upstream
			}
			if len(placementJSON) > 0 && placementSHA256 != nil {
				plan, decodeErr := decodeRealtimePlacementPlan(placementJSON, *placementSHA256)
				if decodeErr != nil {
					return RealtimeContract{}, false, decodeErr
				}
				if plan.RuntimeProfileID != contract.RuntimeProfileID ||
					plan.RuntimeProfileSHA256 != contract.RuntimeProfileSHA256 {
					return RealtimeContract{}, false, errors.New("realtime placement plan does not match contract profile")
				}
				contract.PlacementPlan = plan
				contract.PlacementPlanSHA256 = *placementSHA256
			}
			if err := attachRealtimeContractPricing(&contract, pricingJSON); err != nil {
				return RealtimeContract{}, false, err
			}
			if err := attachRealtimeMarketClearing(&contract, marketJSON); err != nil {
				return RealtimeContract{}, false, err
			}
			// Exact-reuse contracts have no upstream credential.
			if sealed == nil || *sealed == "" {
				if contract.WorkerID == uuid.Nil && contract.SupplierID == uuid.Nil {
					return contract, true, nil
				}
				return RealtimeContract{}, false, errors.New("vLLM upstream credential cannot be opened")
			}
			contract.UpstreamToken = openToken(*sealed)
			if contract.UpstreamToken == "" {
				return RealtimeContract{}, false, errors.New("vLLM upstream credential cannot be opened")
			}
			return contract, true, nil
		}
		if !errors.Is(err, pgx.ErrNoRows) {
			return RealtimeContract{}, false, err
		}
	}
	// Serialize balance authorization on the buyer row. The execution contract
	// and RESERVED event are the maximum-cost reservation, so concurrent
	// requests cannot each spend the same free-credit/prepaid pool. A saved
	// payment method is a top-up rail only — never admission funding.
	if err := evaluateRealtimeBuyerFunding(ctx, tx, auth.BuyerID, auth.MaximumPriceUSD); err != nil {
		return RealtimeContract{}, false, err
	}

	var (
		workerID, supplierID uuid.UUID
		baseURL, sealed      string
		supplierInput        float64
		supplierOutput       float64
		placementJSON        []byte
		placementSHA256      string
		candidateCount       int
		selectedRank         int
		selectedWarmth       string
		terminalAttempts     int
		terminalFails        int
		verifiedSettlements  int
		refundCount          int
	)
	// Reserve a sequence with a single atomic decrement that is also the
	// selection.
	//
	// This was a SELECT ... FOR UPDATE OF o SKIP LOCKED followed by a separate
	// UPDATE, which held the offer row locked through both INSERTs and the
	// COMMIT below.  Because there is one row per (worker, runtime profile),
	// concurrent authorizations did not queue on that lock -- SKIP LOCKED made
	// them step over the only candidate row, get pgx.ErrNoRows, and return
	// errRealtimeNoSupply, which the handler surfaces as HTTP 503 "no
	// compatible realtime capacity is currently available".  A worker
	// advertising 128 free sequences would refuse most concurrent requests
	// while sitting idle: measured at 50 concurrent authorizations against one
	// offer, 40 succeeded and 10 were told there was no capacity.
	//
	// Writing it as one UPDATE ... RETURNING serialises only on the row's own
	// write lock for the duration of that statement, and contending writers
	// block and retry against the new value instead of skipping the row.
	// available_sequences > 0 in the WHERE clause is what makes it safe: the
	// decrement and the capacity check are the same atomic operation, so the
	// counter cannot go negative under any interleaving.
	//
	// Ranking is verified-outcome cost first (base ask adjusted by measured
	// failure and refund rates when enough samples exist), then warmth only as
	// a tiebreak inside a cost class. Self-declared HOT cannot outrank a
	// materially cheaper measured cost — the same discipline as batch claim
	// (scheduler.go: cost rank wins; warmth breaks ties within a class).
	err = tx.QueryRow(ctx, `
		WITH supplier_outcomes AS (
			SELECT supplier_id,
			       count(*) FILTER (WHERE state IN ('VERIFIED','FAILED'))::int AS terminal_attempts,
			       count(*) FILTER (WHERE state = 'FAILED')::int AS terminal_fails
			  FROM execution_contracts
			 WHERE runtime_profile_id = $1
			 GROUP BY supplier_id
		), supplier_refunds AS (
			SELECT c.supplier_id,
			       count(*)::int AS verified_settlements,
			       count(r.id)::int AS refund_count
			  FROM execution_contracts c
			  JOIN realtime_settlements s ON s.contract_id = c.id
			  LEFT JOIN realtime_refunds r ON r.contract_id = c.id
			 WHERE c.runtime_profile_id = $1 AND c.state = 'VERIFIED'
			 GROUP BY c.supplier_id
		), candidates AS (
			SELECT c.worker_id,c.runtime_profile_id,c.supplier_id,c.upstream_base_url,
			       c.upstream_token_sealed,c.supplier_input_usd_per_million_tokens::float8 AS supplier_input,
			       c.supplier_output_usd_per_million_tokens::float8 AS supplier_output,
			       c.placement_plan,c.placement_plan_sha256,c.warmth,
			       COALESCE(o.terminal_attempts,0)::int AS terminal_attempts,
			       COALESCE(o.terminal_fails,0)::int AS terminal_fails,
			       COALESCE(rf.verified_settlements,0)::int AS verified_settlements,
			       COALESCE(rf.refund_count,0)::int AS refund_count,
			       -- verified_outcome_cost: base ask, then divide by delivered and
			       -- kept rates when measured (same arithmetic as
			       -- ExpectedVerifiedOutcomeUSDPerUnit). Unmeasured rates leave the
			       -- base ask unchanged rather than inventing a coefficient.
			       (
			         (c.supplier_input_usd_per_million_tokens + c.supplier_output_usd_per_million_tokens)
			         * CASE
			             WHEN COALESCE(o.terminal_attempts,0) >= $5
			              AND COALESCE(o.terminal_fails,0) >= o.terminal_attempts
			             THEN 1e12::numeric
			             WHEN COALESCE(o.terminal_attempts,0) >= $5
			              AND COALESCE(o.terminal_fails,0) < o.terminal_attempts
			             THEN o.terminal_attempts::numeric
			                  / (o.terminal_attempts - o.terminal_fails)::numeric
			             ELSE 1::numeric
			           END
			         * CASE
			             WHEN COALESCE(rf.verified_settlements,0) >= $5
			              AND COALESCE(rf.refund_count,0) >= rf.verified_settlements
			             THEN 1e12::numeric
			             WHEN COALESCE(rf.verified_settlements,0) >= $5
			              AND COALESCE(rf.refund_count,0) < rf.verified_settlements
			             THEN rf.verified_settlements::numeric
			                  / (rf.verified_settlements - rf.refund_count)::numeric
			             ELSE 1::numeric
			           END
			       ) AS verified_outcome_cost,
			       count(*) OVER ()::int AS candidate_count,
			       row_number() OVER (ORDER BY
			         (
			           (c.supplier_input_usd_per_million_tokens + c.supplier_output_usd_per_million_tokens)
			           * CASE
			               WHEN COALESCE(o.terminal_attempts,0) >= $5
			                AND COALESCE(o.terminal_fails,0) >= o.terminal_attempts
			               THEN 1e12::numeric
			               WHEN COALESCE(o.terminal_attempts,0) >= $5
			                AND COALESCE(o.terminal_fails,0) < o.terminal_attempts
			               THEN o.terminal_attempts::numeric
			                    / (o.terminal_attempts - o.terminal_fails)::numeric
			               ELSE 1::numeric
			             END
			           * CASE
			               WHEN COALESCE(rf.verified_settlements,0) >= $5
			                AND COALESCE(rf.refund_count,0) >= rf.verified_settlements
			               THEN 1e12::numeric
			               WHEN COALESCE(rf.verified_settlements,0) >= $5
			                AND COALESCE(rf.refund_count,0) < rf.verified_settlements
			               THEN rf.verified_settlements::numeric
			                    / (rf.verified_settlements - rf.refund_count)::numeric
			               ELSE 1::numeric
			             END
			         ) ASC,
			         CASE c.warmth WHEN 'HOT' THEN 0 WHEN 'WARM' THEN 1 WHEN 'CACHED' THEN 2 ELSE 3 END,
			         c.available_sequences DESC, c.last_seen_at DESC, c.worker_id ASC)::int AS selected_rank
			  FROM realtime_worker_offers c
			  JOIN suppliers s ON s.id = c.supplier_id
			  LEFT JOIN supplier_outcomes o ON o.supplier_id = c.supplier_id
			  LEFT JOIN supplier_refunds rf ON rf.supplier_id = c.supplier_id
			 WHERE c.runtime_profile_id=$1 AND c.runtime_profile_sha256=$2
			   AND c.status='ACTIVE' AND c.available_sequences > 0
			   AND c.last_seen_at > now()-interval '45 seconds'
			   AND s.status='active' AND s.quarantined_at IS NULL
			   AND c.supplier_input_usd_per_million_tokens <= $3
			   AND c.supplier_output_usd_per_million_tokens <= $4
		), chosen AS (
			SELECT * FROM candidates WHERE selected_rank=1
		), updated AS (
			UPDATE realtime_worker_offers o
			   SET available_sequences = o.available_sequences - 1, updated_at = now()
			  FROM chosen c
			 WHERE o.worker_id = c.worker_id AND o.runtime_profile_id = c.runtime_profile_id
			   AND o.available_sequences > 0
			 RETURNING o.worker_id,o.supplier_id,o.upstream_base_url,o.upstream_token_sealed,
			           o.supplier_input_usd_per_million_tokens::float8,
			           o.supplier_output_usd_per_million_tokens::float8,
			           o.placement_plan,o.placement_plan_sha256,o.warmth,
			           c.candidate_count,c.selected_rank,
			           c.terminal_attempts,c.terminal_fails,
			           c.verified_settlements,c.refund_count
		)
		SELECT worker_id,supplier_id,upstream_base_url,upstream_token_sealed,
		       supplier_input_usd_per_million_tokens::float8,
		       supplier_output_usd_per_million_tokens::float8,
		       placement_plan,placement_plan_sha256,warmth,
		       candidate_count,selected_rank,
		       terminal_attempts,terminal_fails,verified_settlements,refund_count
		  FROM updated`,
		auth.Profile.RuntimeProfileID, auth.Profile.ProfileSHA256,
		auth.Profile.BuyerInputUSDPerMillionTokens, auth.Profile.BuyerOutputUSDPerMillionTokens,
		minRealtimeOutcomeSamples).
		Scan(&workerID, &supplierID, &baseURL, &sealed, &supplierInput, &supplierOutput,
			&placementJSON, &placementSHA256, &selectedWarmth, &candidateCount, &selectedRank,
			&terminalAttempts, &terminalFails, &verifiedSettlements, &refundCount)
	if errors.Is(err, pgx.ErrNoRows) {
		return RealtimeContract{}, false, errRealtimeNoSupply
	}
	if err != nil {
		return RealtimeContract{}, false, err
	}
	placementPlan, err := decodeRealtimePlacementPlan(placementJSON, placementSHA256)
	if err != nil {
		return RealtimeContract{}, false, fmt.Errorf("selected offer has invalid placement authority: %w", err)
	}
	if placementPlan.RuntimeProfileID != auth.Profile.RuntimeProfileID ||
		placementPlan.RuntimeProfileSHA256 != auth.Profile.ProfileSHA256 ||
		placementPlan.ConfiguredTensorParallel != auth.Profile.TensorParallelSize {
		return RealtimeContract{}, false, errors.New("selected offer placement does not match authorized runtime profile")
	}
	currency, err := SettlementCurrency()
	if err != nil {
		return RealtimeContract{}, false, err
	}
	pricing, err := newRealtimePricingDecision(RealtimePricingInputs{
		Profile: auth.Profile, Placement: placementPlan,
		InputCommitment: auth.InputCommitment, RequestSHA256: auth.RequestSHA256,
		MaximumPromptTokens:       auth.MaximumPromptTokens,
		MaximumCompletionTokens:   auth.MaximumCompletionTokens,
		EstimatedPromptTokens:     auth.EstimatedPromptTokens,
		EstimatedCompletionTokens: auth.EstimatedCompletionTokens,
		SupplierInputRate:         supplierInput, SupplierOutputRate: supplierOutput,
		BuyerDeclaredCeiling: auth.BuyerDeclaredCeilingUSD, Currency: currency,
	})
	if err != nil {
		return RealtimeContract{}, false, fmt.Errorf("build realtime PricingDecision: %w", err)
	}
	pricingJSON, err := json.Marshal(pricing)
	if err != nil {
		return RealtimeContract{}, false, err
	}
	pricingSHA256, err := pricingDecisionDigest(pricing)
	if err != nil {
		return RealtimeContract{}, false, err
	}
	inputNanos, err := nanoRatePerMillionFromFloat(supplierInput)
	if err != nil {
		return RealtimeContract{}, false, err
	}
	outputNanos, err := nanoRatePerMillionFromFloat(supplierOutput)
	if err != nil {
		return RealtimeContract{}, false, err
	}
	rankingInputs := buildRealtimeClearingRankingInputs(
		int64(inputNanos), int64(outputNanos),
		terminalAttempts, terminalFails, verifiedSettlements, refundCount,
		selectedWarmth,
	)
	market, err := newRealtimeMarketClearingReceipt(
		candidateCount, selectedRank, workerID, supplierID, supplierInput, supplierOutput, pricing, pricingSHA256,
		rankingInputs)
	if err != nil {
		return RealtimeContract{}, false, err
	}
	marketJSON, err := json.Marshal(market)
	if err != nil {
		return RealtimeContract{}, false, err
	}
	expectedProjection, maximumProjection, err := realtimePricingLegacyProjection(pricing)
	if err != nil || expectedProjection != auth.EstimatedPriceUSD || maximumProjection != auth.MaximumPriceUSD {
		return RealtimeContract{}, false, errors.New("realtime PricingDecision does not match legacy reserve projection")
	}

	contractID := uuid.New()
	_, err = tx.Exec(ctx, `
		INSERT INTO execution_contracts
		 (id,request_id,buyer_id,workload_type,route,model_alias,runtime_profile_id,
		  runtime_profile_sha256,input_commitment,request_sha256,
		  placement_plan,placement_plan_sha256,maximum_price_usd,
		  estimated_price_usd,buyer_input_usd_per_million_tokens,
		  buyer_output_usd_per_million_tokens,supplier_input_usd_per_million_tokens,
		  supplier_output_usd_per_million_tokens,deadline_at,verification_tier,
		  idempotency_key,state,worker_id,supplier_id,upstream_base_url,upstream_token_sealed,
			 currency,maximum_prompt_tokens,maximum_completion_tokens,
			 estimated_prompt_tokens,estimated_completion_tokens,buyer_declared_ceiling_nanos,
			 pricing_decision,pricing_decision_sha256,market_clearing)
		VALUES ($1,$2,$3,'CHAT_COMPLETION','/v1/chat/completions',$4,$5,$6,$7,$8,$9,$10,
		        $11,$12,$13,$14,$15,$16,$17,'V0',$18,'EXECUTING',$19,$20,$21,$22,$23,
		        $24,$25,$26,$27,$28,$29,$30,$31)`,
		contractID, auth.RequestID, auth.BuyerID, auth.Profile.ModelAlias,
		auth.Profile.RuntimeProfileID, auth.Profile.ProfileSHA256,
		auth.InputCommitment, auth.RequestSHA256, placementJSON, placementSHA256,
		auth.MaximumPriceUSD,
		auth.EstimatedPriceUSD, auth.Profile.BuyerInputUSDPerMillionTokens,
		auth.Profile.BuyerOutputUSDPerMillionTokens, supplierInput, supplierOutput,
		auth.DeadlineAt, realtimeNullIfEmpty(auth.IdempotencyKey), workerID, supplierID, baseURL, sealed,
		SettlementCurrencyCode(), auth.MaximumPromptTokens, auth.MaximumCompletionTokens,
		auth.EstimatedPromptTokens, auth.EstimatedCompletionTokens,
		pricing.Realtime.BuyerDeclaredCeilingNanos, pricingJSON, pricingSHA256, marketJSON)
	if err != nil {
		return RealtimeContract{}, false, err
	}
	if _, err := tx.Exec(ctx, `
		INSERT INTO realtime_authorization_events (contract_id,kind,amount_usd)
		VALUES ($1,'RESERVED',$2)`, contractID, auth.MaximumPriceUSD); err != nil {
		return RealtimeContract{}, false, err
	}
	if err := tx.Commit(ctx); err != nil {
		return RealtimeContract{}, false, err
	}
	upstreamToken := openToken(sealed)
	if upstreamToken == "" {
		return RealtimeContract{}, false, errors.New("vLLM upstream credential cannot be opened")
	}
	return RealtimeContract{
		ID: contractID, RequestID: auth.RequestID, BuyerID: auth.BuyerID,
		ModelAlias: auth.Profile.ModelAlias, RuntimeProfileID: auth.Profile.RuntimeProfileID,
		RuntimeProfileSHA256: auth.Profile.ProfileSHA256,
		InputCommitment:      auth.InputCommitment, RequestSHA256: auth.RequestSHA256,
		PlacementPlan: placementPlan, PlacementPlanSHA256: placementSHA256,
		MaximumPriceUSD: auth.MaximumPriceUSD, EstimatedPriceUSD: auth.EstimatedPriceUSD,
		BuyerInputUSDPerMillionTokens:     auth.Profile.BuyerInputUSDPerMillionTokens,
		BuyerOutputUSDPerMillionTokens:    auth.Profile.BuyerOutputUSDPerMillionTokens,
		SupplierInputUSDPerMillionTokens:  supplierInput,
		SupplierOutputUSDPerMillionTokens: supplierOutput,
		DeadlineAt:                        auth.DeadlineAt, State: "EXECUTING", WorkerID: workerID,
		SupplierID: supplierID, UpstreamBaseURL: baseURL, UpstreamToken: upstreamToken,
		MaximumPromptTokens:       auth.MaximumPromptTokens,
		MaximumCompletionTokens:   auth.MaximumCompletionTokens,
		EstimatedPromptTokens:     auth.EstimatedPromptTokens,
		EstimatedCompletionTokens: auth.EstimatedCompletionTokens,
		BuyerDeclaredCeilingNanos: pricing.Realtime.BuyerDeclaredCeilingNanos,
		Pricing:                   &pricing, PricingDecisionSHA256: pricingSHA256, MarketClearing: market,
		Currency: currency.Code(),
	}, false, nil
}

func realtimeNullIfEmpty(value string) any {
	if strings.TrimSpace(value) == "" {
		return nil
	}
	return value
}

type RealtimeExecutionEvidence struct {
	ID                 uuid.UUID
	UpstreamRequestID  string
	HTTPStatus         int
	StreamEventCount   int64
	StreamRootSHA256   string
	OutputCommitment   string
	PromptTokens       int64
	CompletionTokens   int64
	TotalTokens        int64
	TimeToFirstEventMS int64
	DurationMS         int64
}

type RealtimeSettlement struct {
	ID                         uuid.UUID `json:"-"`
	BuyerChargeUSD             float64   `json:"buyer_charge_usd"`
	SupplierPayableUSD         float64   `json:"supplier_payable_usd"`
	PlatformMarginUSD          float64   `json:"platform_margin_usd"`
	Currency                   string    `json:"currency,omitempty"`
	BuyerChargeNanos           int64     `json:"buyer_charge_nanos,omitempty"`
	SupplierPayableNanos       int64     `json:"supplier_payable_nanos,omitempty"`
	KnownCostContributionNanos int64     `json:"known_cost_contribution_nanos,omitempty"`
}

type RealtimeOperationalSnapshot struct {
	ExecutingContracts        int64
	OldestExecutingAgeSeconds float64
	ActiveOffers              int64
	AvailableSequences        int64
	OpenSupplierPayableUSD    float64
	ReversalRequiredUSD       float64
	InternalRefundsUSD        float64
}

func tokenChargeExact(prompt, completion int64, inputRate, outputRate float64, supplier bool) (MoneyNanos, error) {
	currency, err := SettlementCurrency()
	if err != nil {
		return MoneyNanos{}, err
	}
	input, err := nanoRatePerMillionFromFloat(inputRate)
	if err != nil {
		return MoneyNanos{}, err
	}
	output, err := nanoRatePerMillionFromFloat(outputRate)
	if err != nil {
		return MoneyNanos{}, err
	}
	if supplier {
		return SupplierRealtimeTokenEntitlementNanos(currency, prompt, completion, input, output)
	}
	return BuyerRealtimeTokenChargeNanos(currency, prompt, completion, input, output)
}

// tokenCharge is a compatibility projection for stored decimal columns and API
// fields. Authority is derived in nano-major-units above, then projected once.
func tokenCharge(prompt, completion int64, inputRate, outputRate float64) (float64, error) {
	exact, err := tokenChargeExact(prompt, completion, inputRate, outputRate, false)
	if err != nil {
		return 0, err
	}
	micros, err := LedgerMicrosFromNanos(exact)
	if err != nil {
		return 0, err
	}
	return microsToUSD(micros), nil
}

func supplierTokenCharge(prompt, completion int64, inputRate, outputRate float64) (float64, error) {
	exact, err := tokenChargeExact(prompt, completion, inputRate, outputRate, true)
	if err != nil {
		return 0, err
	}
	micros, err := LedgerMicrosFromNanos(exact)
	if err != nil {
		return 0, err
	}
	return microsToUSD(micros), nil
}

func releaseRealtimeCapacity(ctx context.Context, tx pgx.Tx, workerID uuid.UUID, profileID string) error {
	_, err := tx.Exec(ctx, `
		UPDATE realtime_worker_offers
		   SET available_sequences=LEAST(max_active_sequences,available_sequences+1),updated_at=now()
		 WHERE worker_id=$1 AND runtime_profile_id=$2`, workerID, profileID)
	return err
}

// SettleRealtimeExactReuse records a cache-hit settlement: buyer pays the reuse
// class price, no supplier is credited, no worker capacity is reserved. Money
// conservation is buyer = platform exactly (supplier = 0).
func (s *Store) SettleRealtimeExactReuse(
	ctx context.Context,
	auth RealtimeContractAuthorization,
	hit ExactCacheHit,
	money ReuseHitSettlement,
	outputCommitment string,
) (RealtimeContract, RealtimeSettlement, error) {
	if !money.Conserved() || !money.ConservedExact() {
		return RealtimeContract{}, RealtimeSettlement{}, fmt.Errorf(
			"reuse settlement not conserved: buyer=%d supplier=%d platform=%d",
			money.BuyerDebitMicros, money.SupplierLiabilityMicros, money.PlatformMicros)
	}
	if money.SupplierLiabilityMicros != 0 {
		return RealtimeContract{}, RealtimeSettlement{}, errors.New("exact reuse must not credit a supplier")
	}
	if money.BuyerDebitMicros <= 0 {
		return RealtimeContract{}, RealtimeSettlement{}, errors.New("exact reuse must bill the buyer a positive reuse charge")
	}
	currency, err := SettlementCurrency()
	if err != nil {
		return RealtimeContract{}, RealtimeSettlement{}, err
	}
	if money.Currency != currency.Code() || auth.ReuseClass == "" {
		return RealtimeContract{}, RealtimeSettlement{}, errors.New("exact reuse lacks currency-bound reuse-class authority")
	}
	if auth.ReuseClass == ClassCoalescedDelivery && auth.CoalescedLeaderContractID == uuid.Nil {
		return RealtimeContract{}, RealtimeSettlement{}, errors.New("coalesced delivery lacks its physical leader contract")
	}
	if auth.ReuseClass != ClassCoalescedDelivery && auth.CoalescedLeaderContractID != uuid.Nil {
		return RealtimeContract{}, RealtimeSettlement{}, errors.New("non-coalesced reuse may not name a physical leader contract")
	}
	pricing, err := newRealtimeReusePricingDecision(RealtimeReusePricingInputs{
		Profile: auth.Profile, InputCommitment: auth.InputCommitment, RequestSHA256: auth.RequestSHA256,
		ResultCommitment: outputCommitment, ReuseClass: auth.ReuseClass, DeliveredTokens: money.DeliveredTokens,
		BuyerDeclaredCeiling: auth.BuyerDeclaredCeilingUSD, Currency: currency,
	})
	if err != nil {
		return RealtimeContract{}, RealtimeSettlement{}, err
	}
	pricingMicros, err := LedgerMicrosFromNanos(MoneyNanos{Currency: currency, Nanos: pricing.FixedPoint.BuyerChargeNanos})
	if err != nil || pricing.FixedPoint.BuyerChargeNanos != money.BuyerDebitNanos ||
		pricingMicros != money.BuyerDebitMicros || pricing.FixedPoint.SupplierEntitlementsNanos != 0 {
		return RealtimeContract{}, RealtimeSettlement{}, errors.New("exact reuse settlement disagrees with PricingDecision")
	}
	pricingJSON, err := json.Marshal(pricing)
	if err != nil {
		return RealtimeContract{}, RealtimeSettlement{}, err
	}
	pricingSHA256, err := pricingDecisionDigest(pricing)
	if err != nil {
		return RealtimeContract{}, RealtimeSettlement{}, err
	}
	buyerCharge := microsToUSD(money.BuyerDebitMicros)
	platformMargin := microsToUSD(money.PlatformMicros)

	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return RealtimeContract{}, RealtimeSettlement{}, err
	}
	defer tx.Rollback(ctx)
	if auth.IdempotencyKey != "" {
		if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtextextended($1,0))`, auth.BuyerID.String()+":"+auth.IdempotencyKey); err != nil {
			return RealtimeContract{}, RealtimeSettlement{}, err
		}
		existing, err := scanRealtimeContract(tx.QueryRow(ctx, `SELECT `+realtimeContractColumns+`
			FROM execution_contracts WHERE buyer_id=$1 AND idempotency_key=$2`, auth.BuyerID, auth.IdempotencyKey))
		if err == nil {
			if existing.RequestSHA256 != auth.RequestSHA256 || existing.Pricing == nil ||
				existing.Pricing.ExecutionMode != pricingExecutionRealtimeReuse ||
				existing.ReuseClass != auth.ReuseClass || existing.ReuseResultCommitment != outputCommitment ||
				existing.ReuseDeliveredTokens != money.DeliveredTokens {
				return RealtimeContract{}, RealtimeSettlement{}, errRealtimeIdempotencyConflict
			}
			var settlement RealtimeSettlement
			err = tx.QueryRow(ctx, `
				SELECT id,buyer_charge_usd::float8,supplier_gross_usd::float8,
				       platform_margin_usd::float8,COALESCE(currency,''),
				       COALESCE(buyer_charge_nanos,0),COALESCE(supplier_gross_nanos,0),
				       COALESCE(known_cost_contribution_nanos,0)
				  FROM realtime_settlements WHERE contract_id=$1`, existing.ID).Scan(
				&settlement.ID, &settlement.BuyerChargeUSD, &settlement.SupplierPayableUSD,
				&settlement.PlatformMarginUSD, &settlement.Currency, &settlement.BuyerChargeNanos,
				&settlement.SupplierPayableNanos, &settlement.KnownCostContributionNanos)
			if err != nil {
				return RealtimeContract{}, RealtimeSettlement{}, err
			}
			if auth.ReuseClass == ClassCoalescedDelivery {
				var recordedLeader uuid.UUID
				if err := tx.QueryRow(ctx, `
					SELECT leader_contract_id FROM realtime_coalesced_deliveries
					 WHERE follower_contract_id=$1`, existing.ID).Scan(&recordedLeader); err != nil ||
					recordedLeader != auth.CoalescedLeaderContractID {
					return RealtimeContract{}, RealtimeSettlement{}, errRealtimeIdempotencyConflict
				}
			}
			return existing, settlement, nil
		}
		if !errors.Is(err, pgx.ErrNoRows) {
			return RealtimeContract{}, RealtimeSettlement{}, err
		}
	}

	// Same fund gate as live authorization, against the reuse charge (not the
	// full-rate maximum) so a cache hit cannot fail for want of capacity money
	// that physical execution would have reserved.
	if err := evaluateRealtimeBuyerFunding(ctx, tx, auth.BuyerID, buyerCharge); err != nil {
		return RealtimeContract{}, RealtimeSettlement{}, err
	}

	var counterfactualSupplierEntitlementNanos int64
	if auth.ReuseClass == ClassCoalescedDelivery {
		err := tx.QueryRow(ctx, `
			SELECT s.supplier_gross_nanos
			  FROM execution_contracts c
			  JOIN realtime_settlements s ON s.contract_id=c.id
			  JOIN realtime_executions e ON e.contract_id=c.id
			 WHERE c.id=$1 AND c.buyer_id=$2 AND c.state='VERIFIED'
			   AND c.worker_id IS NOT NULL AND c.supplier_id IS NOT NULL
			   AND c.reuse_class IS NULL AND c.currency=$3 AND s.currency=$3
			   AND s.supplier_gross_nanos > 0 AND e.output_commitment=$4`,
			auth.CoalescedLeaderContractID, auth.BuyerID, currency.Code(), outputCommitment).
			Scan(&counterfactualSupplierEntitlementNanos)
		if errors.Is(err, pgx.ErrNoRows) {
			return RealtimeContract{}, RealtimeSettlement{}, errors.New("coalesced delivery leader is not a matching finalized physical settlement")
		}
		if err != nil {
			return RealtimeContract{}, RealtimeSettlement{}, err
		}
	}

	contractID := uuid.New()
	executionID := uuid.New()
	// No worker, no supplier, no upstream: capacity is not reserved.
	_, err = tx.Exec(ctx, `
		INSERT INTO execution_contracts
		 (id,request_id,buyer_id,workload_type,route,model_alias,runtime_profile_id,
		  runtime_profile_sha256,input_commitment,request_sha256,maximum_price_usd,
		  estimated_price_usd,buyer_input_usd_per_million_tokens,
		  buyer_output_usd_per_million_tokens,supplier_input_usd_per_million_tokens,
		  supplier_output_usd_per_million_tokens,deadline_at,verification_tier,
		  idempotency_key,state,worker_id,supplier_id,upstream_base_url,upstream_token_sealed,
		  finalized_at,currency,buyer_declared_ceiling_nanos,reuse_class,
		  reuse_result_commitment,reuse_delivered_tokens,pricing_decision,pricing_decision_sha256)
		VALUES ($1,$2,$3,'CHAT_COMPLETION','/v1/chat/completions',$4,$5,$6,$7,$8,
		        $9,$9,$10,$11,0,0,$12,'V0',$13,'VERIFIED',NULL,NULL,NULL,NULL,now(),$14,
		        $15,$16,$17,$18,$19,$20)`,
		contractID, auth.RequestID, auth.BuyerID, auth.Profile.ModelAlias,
		auth.Profile.RuntimeProfileID, auth.Profile.ProfileSHA256,
		auth.InputCommitment, auth.RequestSHA256, buyerCharge,
		auth.Profile.BuyerInputUSDPerMillionTokens, auth.Profile.BuyerOutputUSDPerMillionTokens,
		auth.DeadlineAt, realtimeNullIfEmpty(auth.IdempotencyKey), currency.Code(),
		pricing.RealtimeReuse.BuyerDeclaredCeilingNanos, auth.ReuseClass, outputCommitment,
		money.DeliveredTokens, pricingJSON, pricingSHA256)
	if err != nil {
		return RealtimeContract{}, RealtimeSettlement{}, err
	}

	// Delivered tokens are all logical reuse; physical is zero. We still record
	// the cached completion size on the execution so the receipt can separate
	// physical from delivered via the billing-class accounting.
	delivered := money.DeliveredTokens
	if delivered <= 0 {
		delivered = hit.OutputTokens
	}
	_, err = tx.Exec(ctx, `
		INSERT INTO realtime_executions
		 (id,contract_id,worker_id,supplier_id,upstream_request_id,http_status,
		  stream_event_count,stream_root_sha256,output_commitment,prompt_tokens,
		  completion_tokens,total_tokens,time_to_first_event_ms,duration_ms,
		  verification_state)
		VALUES ($1,$2,NULL,NULL,$3,200,0,$4,$5,0,$6,$6,0,0,'PASSED')`,
		executionID, contractID, "exact_reuse:"+hit.ResultRef,
		outputCommitment, outputCommitment, delivered)
	if err != nil {
		return RealtimeContract{}, RealtimeSettlement{}, err
	}

	// Buyer debit + platform take only. No supplier_credit row at all.
	entries := []struct {
		kind   string
		buyer  *uuid.UUID
		amount int64
	}{
		{KindBuyerCharge, &auth.BuyerID, -money.BuyerDebitMicros},
		{KindPlatformTake, nil, money.PlatformMicros},
	}
	for _, entry := range entries {
		contract := contractID
		if _, err := insertLedgerEntryTx(ctx, tx, ledgerInsert{
			Kind:                entry.kind,
			BuyerID:             entry.buyer,
			ExecutionContractID: &contract,
			AmountMicros:        entry.amount,
			PayoutStatus:        PayoutReleased,
		}); err != nil {
			return RealtimeContract{}, RealtimeSettlement{}, err
		}
	}
	if err := maybeDebitPrepaidForRealtimeTx(ctx, tx, auth.BuyerID, contractID, money.BuyerDebitMicros); err != nil {
		return RealtimeContract{}, RealtimeSettlement{}, err
	}

	settlementID := uuid.New()
	if _, err := tx.Exec(ctx, `
		INSERT INTO realtime_settlements
		 (id,contract_id,authoritative_execution_id,receipt_id,buyer_charge_usd,
		  supplier_gross_usd,platform_margin_usd,verification_cost_usd,
		  currency,buyer_charge_nanos,supplier_gross_nanos,known_cost_contribution_nanos)
		VALUES ($1,$2,$3,$4,$5,0,$6,0,$7,$8,0,$8)`,
		settlementID, contractID, executionID, "rcp_"+executionID.String(),
		buyerCharge, platformMargin, currency.Code(), money.BuyerDebitNanos); err != nil {
		return RealtimeContract{}, RealtimeSettlement{}, err
	}
	if auth.ReuseClass == ClassCoalescedDelivery {
		if _, err := tx.Exec(ctx, `
			INSERT INTO realtime_coalesced_deliveries
			 (follower_contract_id,leader_contract_id,buyer_id,currency,
			  counterfactual_supplier_entitlement_nanos)
			VALUES ($1,$2,$3,$4,$5)`,
			contractID, auth.CoalescedLeaderContractID, auth.BuyerID, currency.Code(),
			counterfactualSupplierEntitlementNanos); err != nil {
			return RealtimeContract{}, RealtimeSettlement{}, err
		}
	}
	if _, err := tx.Exec(ctx, `
		INSERT INTO realtime_authorization_events (contract_id,kind,amount_usd)
		VALUES ($1,'RESERVED',$2),($1,'CAPTURED',$2),($1,'RELEASED',0)`,
		contractID, buyerCharge); err != nil {
		return RealtimeContract{}, RealtimeSettlement{}, err
	}
	if err := tx.Commit(ctx); err != nil {
		return RealtimeContract{}, RealtimeSettlement{}, err
	}
	contract := RealtimeContract{
		ID: contractID, RequestID: auth.RequestID, BuyerID: auth.BuyerID,
		ModelAlias: auth.Profile.ModelAlias, RuntimeProfileID: auth.Profile.RuntimeProfileID,
		RuntimeProfileSHA256: auth.Profile.ProfileSHA256,
		InputCommitment:      auth.InputCommitment, RequestSHA256: auth.RequestSHA256,
		MaximumPriceUSD: buyerCharge, EstimatedPriceUSD: buyerCharge,
		BuyerInputUSDPerMillionTokens:  auth.Profile.BuyerInputUSDPerMillionTokens,
		BuyerOutputUSDPerMillionTokens: auth.Profile.BuyerOutputUSDPerMillionTokens,
		DeadlineAt:                     auth.DeadlineAt, State: "VERIFIED", Currency: currency.Code(),
		BuyerDeclaredCeilingNanos: pricing.RealtimeReuse.BuyerDeclaredCeilingNanos,
		ReuseClass:                auth.ReuseClass, ReuseResultCommitment: outputCommitment,
		ReuseDeliveredTokens: money.DeliveredTokens, Pricing: &pricing, PricingDecisionSHA256: pricingSHA256,
	}
	return contract, RealtimeSettlement{
		ID: settlementID, BuyerChargeUSD: buyerCharge,
		SupplierPayableUSD: 0, PlatformMarginUSD: platformMargin, Currency: currency.Code(),
		BuyerChargeNanos: money.BuyerDebitNanos, SupplierPayableNanos: 0,
		KnownCostContributionNanos: money.BuyerDebitNanos,
	}, nil
}

func (s *Store) FinalizeRealtimeSuccess(ctx context.Context, contractID uuid.UUID, evidence RealtimeExecutionEvidence) (RealtimeSettlement, error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return RealtimeSettlement{}, err
	}
	defer tx.Rollback(ctx)
	contract, err := scanRealtimeContract(tx.QueryRow(ctx,
		`SELECT `+realtimeContractColumns+` FROM execution_contracts WHERE id=$1 FOR UPDATE`, contractID))
	if errors.Is(err, pgx.ErrNoRows) {
		return RealtimeSettlement{}, errNotFound
	}
	if err != nil {
		return RealtimeSettlement{}, err
	}
	// Idempotent settle: a retried settlement intent must not double-charge.
	if contract.State == "VERIFIED" {
		var settlement RealtimeSettlement
		err := tx.QueryRow(ctx, `
			SELECT id,buyer_charge_usd::float8,supplier_gross_usd::float8,platform_margin_usd::float8,
			       currency,buyer_charge_nanos,supplier_gross_nanos,known_cost_contribution_nanos
			  FROM realtime_settlements
			 WHERE contract_id=$1 AND authoritative_execution_id=$2`, contractID, evidence.ID).
			Scan(&settlement.ID, &settlement.BuyerChargeUSD, &settlement.SupplierPayableUSD,
				&settlement.PlatformMarginUSD, &settlement.Currency, &settlement.BuyerChargeNanos,
				&settlement.SupplierPayableNanos, &settlement.KnownCostContributionNanos)
		if errors.Is(err, pgx.ErrNoRows) {
			return RealtimeSettlement{}, errRealtimeAlreadyFinalized
		}
		if err != nil {
			return RealtimeSettlement{}, err
		}
		if _, err := tx.Exec(ctx, `
			UPDATE realtime_settlement_intents
			   SET state='settled', updated_at=now(), last_error=NULL
			 WHERE contract_id=$1 AND execution_id=$2 AND state IN ('pending','escalated')`,
			contractID, evidence.ID); err != nil {
			return RealtimeSettlement{}, err
		}
		if err := tx.Commit(ctx); err != nil {
			return RealtimeSettlement{}, err
		}
		return settlement, nil
	}
	if contract.State != "EXECUTING" {
		return RealtimeSettlement{}, errRealtimeAlreadyFinalized
	}
	if contract.Pricing == nil || contract.Pricing.Realtime == nil || contract.Pricing.FixedPoint == nil {
		return RealtimeSettlement{}, errors.New("physical realtime contract lacks PricingDecision authority")
	}
	if evidence.ID == uuid.Nil || evidence.HTTPStatus < 200 || evidence.HTTPStatus > 299 ||
		evidence.StreamRootSHA256 == "" || evidence.OutputCommitment == "" ||
		evidence.PromptTokens < 0 || evidence.CompletionTokens < 0 ||
		evidence.TotalTokens != evidence.PromptTokens+evidence.CompletionTokens {
		return RealtimeSettlement{}, errors.New("incomplete V0 execution evidence")
	}
	if evidence.PromptTokens > contract.MaximumPromptTokens ||
		evidence.CompletionTokens > contract.MaximumCompletionTokens {
		return RealtimeSettlement{}, errors.New("verified usage exceeds frozen PricingDecision token bounds")
	}
	currency, err := ParseCurrency(contract.Currency)
	if err != nil {
		return RealtimeSettlement{}, err
	}
	authority := contract.Pricing.Realtime
	buyerExact, err := BuyerRealtimeTokenChargeNanos(currency, evidence.PromptTokens, evidence.CompletionTokens,
		NanoMajorPerMillionTokens(authority.BuyerInputNanosPerMillion),
		NanoMajorPerMillionTokens(authority.BuyerOutputNanosPerMillion))
	if err != nil || buyerExact.Nanos <= 0 {
		return RealtimeSettlement{}, fmt.Errorf("derive buyer token charge: %w", err)
	}
	if buyerExact.Nanos > contract.Pricing.FixedPoint.AcceptedCeilingNanos {
		return RealtimeSettlement{}, fmt.Errorf("verified usage cost %d nanos exceeds contract maximum %d nanos",
			buyerExact.Nanos, contract.Pricing.FixedPoint.AcceptedCeilingNanos)
	}
	supplierExact, err := SupplierRealtimeTokenEntitlementNanos(currency, evidence.PromptTokens, evidence.CompletionTokens,
		NanoMajorPerMillionTokens(authority.SupplierInputNanosPerMillion),
		NanoMajorPerMillionTokens(authority.SupplierOutputNanosPerMillion))
	if err != nil {
		return RealtimeSettlement{}, fmt.Errorf("derive supplier token entitlement: %w", err)
	}
	contributionExact, err := buyerExact.Sub(supplierExact)
	if err != nil || contributionExact.Nanos <= 0 {
		return RealtimeSettlement{}, errors.New("supplier entitlement leaves no positive exact Merc contribution")
	}
	buyerMicros, err := LedgerMicrosFromNanos(buyerExact)
	if err != nil {
		return RealtimeSettlement{}, err
	}
	supplierMicros, err := LedgerMicrosFromNanos(supplierExact)
	if err != nil || supplierMicros > buyerMicros {
		return RealtimeSettlement{}, errors.New("supplier ledger projection exceeds buyer charge")
	}
	platformMicros := buyerMicros - supplierMicros
	buyerCharge := microsToUSD(buyerMicros)
	supplierPayable := microsToUSD(supplierMicros)
	platformMargin := microsToUSD(platformMicros)
	_, err = tx.Exec(ctx, `
		INSERT INTO realtime_executions
		 (id,contract_id,worker_id,supplier_id,upstream_request_id,http_status,
		  stream_event_count,stream_root_sha256,output_commitment,prompt_tokens,
		  completion_tokens,total_tokens,time_to_first_event_ms,duration_ms,
		  verification_state)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,'PASSED')`,
		evidence.ID, contractID, contract.WorkerID, contract.SupplierID, realtimeNullIfEmpty(evidence.UpstreamRequestID),
		evidence.HTTPStatus, evidence.StreamEventCount, evidence.StreamRootSHA256,
		evidence.OutputCommitment, evidence.PromptTokens, evidence.CompletionTokens,
		evidence.TotalTokens, evidence.TimeToFirstEventMS, evidence.DurationMS)
	if err != nil {
		return RealtimeSettlement{}, err
	}
	releaseAt := payoutReleaseAt(time.Now(), 0)
	entries := []struct {
		kind, status    string
		buyer, supplier *uuid.UUID
		amountMicros    int64
		release         any
	}{
		{KindBuyerCharge, PayoutReleased, &contract.BuyerID, nil, -buyerMicros, nil},
		{KindSupplierCredit, PayoutHeld, nil, &contract.SupplierID, supplierMicros, releaseAt},
		{KindPlatformTake, PayoutReleased, nil, nil, platformMicros, nil},
	}
	for _, entry := range entries {
		var releaseAt *time.Time
		if t, ok := entry.release.(time.Time); ok {
			releaseAt = &t
		}
		contract := contractID
		if _, err := insertLedgerEntryTx(ctx, tx, ledgerInsert{
			Kind:                entry.kind,
			SupplierID:          entry.supplier,
			BuyerID:             entry.buyer,
			ExecutionContractID: &contract,
			AmountMicros:        entry.amountMicros,
			PayoutStatus:        entry.status,
			ReleaseAt:           releaseAt,
		}); err != nil {
			return RealtimeSettlement{}, err
		}
	}
	// Debit materialised prepaid for the settled charge when free credit alone
	// does not cover it. Free-credit sandbox charges remain ledger-only.
	if err := maybeDebitPrepaidForRealtimeTx(ctx, tx, contract.BuyerID, contractID, buyerMicros); err != nil {
		return RealtimeSettlement{}, err
	}
	settlementID := uuid.New()
	if _, err := tx.Exec(ctx, `
		INSERT INTO realtime_settlements
		 (id,contract_id,authoritative_execution_id,receipt_id,buyer_charge_usd,
		  supplier_gross_usd,platform_margin_usd,verification_cost_usd,
		  currency,buyer_charge_nanos,supplier_gross_nanos,known_cost_contribution_nanos)
		VALUES ($1,$2,$3,$4,$5,$6,$7,0,$8,$9,$10,$11)`, settlementID, contractID, evidence.ID,
		"rcp_"+evidence.ID.String(), buyerCharge, supplierPayable, platformMargin,
		contract.Currency, buyerExact.Nanos, supplierExact.Nanos, contributionExact.Nanos); err != nil {
		return RealtimeSettlement{}, err
	}
	if _, err := tx.Exec(ctx, `UPDATE execution_contracts SET state='VERIFIED',finalized_at=now() WHERE id=$1`, contractID); err != nil {
		return RealtimeSettlement{}, err
	}
	if _, err := tx.Exec(ctx, `
		INSERT INTO realtime_authorization_events (contract_id,kind,amount_usd)
		VALUES ($1,'CAPTURED',$2),
		       ($1,'RELEASED',(SELECT maximum_price_usd-$2 FROM execution_contracts WHERE id=$1))`,
		contractID, buyerCharge); err != nil {
		return RealtimeSettlement{}, err
	}
	if err := releaseRealtimeCapacity(ctx, tx, contract.WorkerID, contract.RuntimeProfileID); err != nil {
		return RealtimeSettlement{}, err
	}
	if _, err := tx.Exec(ctx, `
		UPDATE realtime_settlement_intents
		   SET state='settled', updated_at=now(), last_error=NULL
		 WHERE contract_id=$1 AND execution_id=$2 AND state IN ('pending','escalated')`,
		contractID, evidence.ID); err != nil {
		return RealtimeSettlement{}, err
	}
	if err := tx.Commit(ctx); err != nil {
		return RealtimeSettlement{}, err
	}
	return RealtimeSettlement{ID: settlementID, BuyerChargeUSD: buyerCharge, SupplierPayableUSD: supplierPayable,
		PlatformMarginUSD: platformMargin, Currency: contract.Currency,
		BuyerChargeNanos: buyerExact.Nanos, SupplierPayableNanos: supplierExact.Nanos,
		KnownCostContributionNanos: contributionExact.Nanos}, nil
}

func (s *Store) FinalizeRealtimeFailure(ctx context.Context, contractID uuid.UUID, executionID uuid.UUID, httpStatus int, durationMS int64, code, detail string, cancelled bool) (bool, error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return false, err
	}
	defer tx.Rollback(ctx)
	var state, profileID string
	var workerID, supplierID uuid.UUID
	err = tx.QueryRow(ctx, `
		SELECT state,worker_id,supplier_id,runtime_profile_id
		  FROM execution_contracts WHERE id=$1 FOR UPDATE`, contractID).
		Scan(&state, &workerID, &supplierID, &profileID)
	if errors.Is(err, pgx.ErrNoRows) {
		return false, errNotFound
	}
	if err != nil {
		return false, err
	}
	if state != "EXECUTING" {
		return false, nil
	}
	if len(detail) > 1000 {
		detail = detail[:1000]
	}
	var status any
	if httpStatus > 0 {
		status = httpStatus
	}
	_, err = tx.Exec(ctx, `
		INSERT INTO realtime_executions
		 (id,contract_id,worker_id,supplier_id,http_status,duration_ms,
		  verification_state,failure_code,failure_detail)
		VALUES ($1,$2,$3,$4,$5,$6,'FAILED',$7,$8)`,
		executionID, contractID, workerID, supplierID, status, durationMS, code, detail)
	if err != nil {
		return false, err
	}
	next := "FAILED"
	if cancelled {
		next = "CANCELLED"
	}
	if _, err := tx.Exec(ctx, `UPDATE execution_contracts SET state=$2,finalized_at=now() WHERE id=$1`, contractID, next); err != nil {
		return false, err
	}
	if _, err := tx.Exec(ctx, `
		INSERT INTO realtime_authorization_events (contract_id,kind,amount_usd)
		SELECT id,'VOIDED',maximum_price_usd FROM execution_contracts WHERE id=$1`, contractID); err != nil {
		return false, err
	}
	if err := releaseRealtimeCapacity(ctx, tx, workerID, profileID); err != nil {
		return false, err
	}
	// Interrupted or undelivered work cancels any pending settlement intent so
	// the sweep never bills for a stream that did not complete.
	if _, err := tx.Exec(ctx, `
		UPDATE realtime_settlement_intents
		   SET state='cancelled', updated_at=now(),
		       last_error=LEFT($3,1000)
		 WHERE contract_id=$1 AND execution_id=$2 AND state='pending'`,
		contractID, executionID, "cancelled:"+code+":"+detail); err != nil {
		return false, err
	}
	if err := tx.Commit(ctx); err != nil {
		return false, err
	}
	return true, nil
}

const (
	realtimeSettlementIntentMaxAttempts = 20
	realtimeSettlementIntentBaseBackoff = 2 * time.Second
	realtimeSettlementIntentMaxBackoff  = 5 * time.Minute
)

// InsertRealtimeSettlementIntent records a durable pending settle before the
// first stream byte is written. Own transaction so a later finalize failure
// cannot roll back the obligation to bill delivered work.
func (s *Store) InsertRealtimeSettlementIntent(ctx context.Context, contractID, executionID uuid.UUID) error {
	if contractID == uuid.Nil || executionID == uuid.Nil {
		return errors.New("settlement intent requires contract and execution ids")
	}
	_, err := s.pool.Exec(ctx, `
		INSERT INTO realtime_settlement_intents (contract_id,execution_id,state)
		VALUES ($1,$2,'pending')
		ON CONFLICT (contract_id,execution_id) DO NOTHING`, contractID, executionID)
	return err
}

// RecordRealtimeSettlementIntentFailure keeps a delivered-stream intent pending
// with the observed evidence and error so the worker sweep can retry. It does
// not void the contract or write failure ledger rows.
func (s *Store) RecordRealtimeSettlementIntentFailure(ctx context.Context, contractID, executionID uuid.UUID, evidence RealtimeExecutionEvidence, settleErr error) error {
	if contractID == uuid.Nil || executionID == uuid.Nil {
		return errors.New("settlement intent failure requires contract and execution ids")
	}
	detail := ""
	if settleErr != nil {
		detail = settleErr.Error()
	}
	if len(detail) > 1000 {
		detail = detail[:1000]
	}
	evidenceJSON, err := json.Marshal(evidence)
	if err != nil {
		return err
	}
	ct, err := s.pool.Exec(ctx, `
		UPDATE realtime_settlement_intents
		   SET evidence=$3::jsonb,
		       last_error=$4,
		       next_attempt_at=now(),
		       updated_at=now()
		 WHERE contract_id=$1 AND execution_id=$2 AND state='pending'`,
		contractID, executionID, evidenceJSON, detail)
	if err != nil {
		return err
	}
	if ct.RowsAffected() == 0 {
		return errNotFound
	}
	return nil
}

// SettlePendingRealtimeIntents retries pending stream settlements from recorded
// evidence. Bounded attempts escalate rather than silently dropping the bill.
// Returns settled count and escalated count.
func (s *Store) SettlePendingRealtimeIntents(ctx context.Context, limit int) (settled, escalated int, err error) {
	if limit < 1 || limit > 1000 {
		return 0, 0, errors.New("invalid realtime settlement intent bounds")
	}
	// No long-lived row locks: FinalizeRealtimeSuccess is idempotent per
	// (contract, execution), so concurrent sweeps cannot double-charge.
	rows, err := s.pool.Query(ctx, `
		SELECT id,contract_id,execution_id,attempt_count,evidence
		  FROM realtime_settlement_intents
		 WHERE state='pending' AND next_attempt_at <= now() AND evidence IS NOT NULL
		 ORDER BY next_attempt_at,created_at
		 LIMIT $1`, limit)
	if err != nil {
		return 0, 0, err
	}
	type pendingIntent struct {
		id          uuid.UUID
		contractID  uuid.UUID
		executionID uuid.UUID
		attempts    int
		evidence    []byte
	}
	var pending []pendingIntent
	for rows.Next() {
		var item pendingIntent
		if err := rows.Scan(&item.id, &item.contractID, &item.executionID, &item.attempts, &item.evidence); err != nil {
			rows.Close()
			return 0, 0, err
		}
		pending = append(pending, item)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return 0, 0, err
	}
	rows.Close()

	for _, item := range pending {
		var evidence RealtimeExecutionEvidence
		if err := json.Unmarshal(item.evidence, &evidence); err != nil {
			if markErr := s.markRealtimeSettlementIntentAttempt(ctx, item.id, item.attempts, err); markErr != nil {
				return settled, escalated, markErr
			}
			continue
		}
		if evidence.ID == uuid.Nil {
			evidence.ID = item.executionID
		}
		_, settleErr := s.FinalizeRealtimeSuccess(ctx, item.contractID, evidence)
		if settleErr == nil || errors.Is(settleErr, errRealtimeAlreadyFinalized) {
			if _, err := s.pool.Exec(ctx, `
				UPDATE realtime_settlement_intents
				   SET state='settled', updated_at=now(), last_error=NULL
				 WHERE id=$1 AND state IN ('pending','escalated')`, item.id); err != nil {
				return settled, escalated, err
			}
			settled++
			continue
		}
		nextAttempts := item.attempts + 1
		if nextAttempts >= realtimeSettlementIntentMaxAttempts {
			detail := settleErr.Error()
			if len(detail) > 1000 {
				detail = detail[:1000]
			}
			if _, err := s.pool.Exec(ctx, `
				UPDATE realtime_settlement_intents
				   SET state='escalated', attempt_count=$2, last_error=$3, updated_at=now()
				 WHERE id=$1 AND state='pending'`, item.id, nextAttempts, detail); err != nil {
				return settled, escalated, err
			}
			escalated++
			continue
		}
		if err := s.markRealtimeSettlementIntentAttempt(ctx, item.id, item.attempts, settleErr); err != nil {
			return settled, escalated, err
		}
	}
	return settled, escalated, nil
}

func (s *Store) markRealtimeSettlementIntentAttempt(ctx context.Context, intentID uuid.UUID, priorAttempts int, settleErr error) error {
	detail := ""
	if settleErr != nil {
		detail = settleErr.Error()
	}
	if len(detail) > 1000 {
		detail = detail[:1000]
	}
	nextAttempts := priorAttempts + 1
	backoff := realtimeSettlementIntentBaseBackoff << nextAttempts
	if backoff > realtimeSettlementIntentMaxBackoff || backoff <= 0 {
		backoff = realtimeSettlementIntentMaxBackoff
	}
	_, err := s.pool.Exec(ctx, `
		UPDATE realtime_settlement_intents
		   SET attempt_count=$2, last_error=$3,
		       next_attempt_at=now()+make_interval(secs=>$4::double precision),
		       updated_at=now()
		 WHERE id=$1 AND state='pending'`,
		intentID, nextAttempts, detail, backoff.Seconds())
	return err
}

func (s *Store) RecoverStaleRealtimeContracts(ctx context.Context, grace time.Duration, limit int) (int, error) {
	if grace < 0 || limit < 1 || limit > 1000 {
		return 0, errors.New("invalid realtime recovery bounds")
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return 0, err
	}
	defer tx.Rollback(ctx)
	type staleContract struct {
		id                   uuid.UUID
		workerID, supplierID uuid.UUID
		runtimeProfileID     string
		createdAt            time.Time
	}
	rows, err := tx.Query(ctx, `
		SELECT id,worker_id,supplier_id,runtime_profile_id,created_at
		  FROM execution_contracts
		 WHERE state='EXECUTING'
		   AND deadline_at < now()-make_interval(secs=>$1::double precision)
		 ORDER BY deadline_at,id
		 FOR UPDATE SKIP LOCKED
		 LIMIT $2`, grace.Seconds(), limit)
	if err != nil {
		return 0, err
	}
	var stale []staleContract
	for rows.Next() {
		var item staleContract
		if err := rows.Scan(&item.id, &item.workerID, &item.supplierID, &item.runtimeProfileID, &item.createdAt); err != nil {
			rows.Close()
			return 0, err
		}
		stale = append(stale, item)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return 0, err
	}
	rows.Close()
	for _, item := range stale {
		durationMS := time.Since(item.createdAt).Milliseconds()
		if durationMS < 0 {
			durationMS = 0
		}
		if _, err := tx.Exec(ctx, `
			INSERT INTO realtime_executions
			 (id,contract_id,worker_id,supplier_id,duration_ms,verification_state,
			  failure_code,failure_detail)
			VALUES ($1,$2,$3,$4,$5,'FAILED','control_recovery_timeout',
			        'control recovered an execution contract past its deadline and recovery grace')`,
			uuid.New(), item.id, item.workerID, item.supplierID, durationMS); err != nil {
			return 0, err
		}
		if _, err := tx.Exec(ctx, `
			UPDATE execution_contracts
			   SET state='FAILED',finalized_at=now()
			 WHERE id=$1 AND state='EXECUTING'`, item.id); err != nil {
			return 0, err
		}
		if _, err := tx.Exec(ctx, `
			INSERT INTO realtime_authorization_events (contract_id,kind,amount_usd)
			SELECT id,'VOIDED',maximum_price_usd FROM execution_contracts WHERE id=$1`, item.id); err != nil {
			return 0, err
		}
		if err := releaseRealtimeCapacity(ctx, tx, item.workerID, item.runtimeProfileID); err != nil {
			return 0, err
		}
	}
	if err := tx.Commit(ctx); err != nil {
		return 0, err
	}
	return len(stale), nil
}

type RealtimeRefund struct {
	RefundID            string    `json:"refund_id"`
	ContractID          string    `json:"contract_id"`
	SettlementID        string    `json:"settlement_id"`
	RefundMode          string    `json:"refund_mode"`
	ReasonCode          string    `json:"reason_code"`
	Reason              string    `json:"reason"`
	CorrelationRef      string    `json:"correlation_ref"`
	BuyerRefundUSD      float64   `json:"buyer_refund_usd"`
	SupplierClawbackUSD float64   `json:"supplier_clawback_usd"`
	PlatformRefundUSD   float64   `json:"platform_refund_usd"`
	InternalCreditState string    `json:"internal_credit_state"`
	ExternalCashState   string    `json:"external_cash_state"`
	SupplierPayoutState string    `json:"supplier_payout_state"`
	CreatedAt           time.Time `json:"created_at"`
}

func realtimeRefundTx(ctx context.Context, tx pgx.Tx, contractID uuid.UUID) (RealtimeRefund, error) {
	var out RealtimeRefund
	var refundID, settlementID uuid.UUID
	err := tx.QueryRow(ctx, `
		SELECT r.id,r.contract_id::text,s.id,r.refund_mode,r.reason_code,r.reason,
		       r.correlation_ref,r.buyer_refund_usd::float8,
		       r.supplier_clawback_usd::float8,r.platform_refund_usd::float8,
		       r.internal_credit_state,r.external_cash_state,le.payout_status,r.created_at
		  FROM realtime_refunds r
		  JOIN realtime_settlements s ON s.contract_id=r.contract_id
		  JOIN ledger_entries le ON le.execution_contract_id=r.contract_id
		                         AND le.kind='supplier_credit'
		 WHERE r.contract_id=$1`, contractID).Scan(
		&refundID, &out.ContractID, &settlementID, &out.RefundMode, &out.ReasonCode,
		&out.Reason, &out.CorrelationRef, &out.BuyerRefundUSD, &out.SupplierClawbackUSD,
		&out.PlatformRefundUSD, &out.InternalCreditState, &out.ExternalCashState,
		&out.SupplierPayoutState, &out.CreatedAt)
	if err != nil {
		return RealtimeRefund{}, err
	}
	out.RefundID = "rfd_" + refundID.String()
	out.SettlementID = "set_" + settlementID.String()
	return out, nil
}

// RefundRealtimeContract records a full internal credit while the supplier
// liability is still inside the platform boundary. It deliberately refuses to
// imply a Stripe refund or transfer reversal; those require separate provider
// evidence and remain fail-closed once payout funding or sending begins.
func (s *Store) RefundRealtimeContract(
	ctx context.Context,
	actor AdminActor,
	contractID uuid.UUID,
	reason, correlationRef string,
) (RealtimeRefund, bool, error) {
	intent, err := prepareAdminMutation(actor, adminMutationIntent{
		Kind: adminActionRealtimeRefunded, TargetKind: adminTargetContract,
		TargetID: contractID, Reason: reason, CorrelationRef: correlationRef,
	})
	if err != nil {
		return RealtimeRefund{}, false, err
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return RealtimeRefund{}, false, err
	}
	defer tx.Rollback(ctx)
	if err := revalidateAdminActor(ctx, tx, actor); err != nil {
		return RealtimeRefund{}, false, err
	}
	if replay, err := acquireAdminMutationReplay(ctx, tx, actor, intent); err != nil {
		return RealtimeRefund{}, false, err
	} else if replay.Found {
		refund, err := realtimeRefundTx(ctx, tx, contractID)
		if err != nil {
			return RealtimeRefund{}, false, err
		}
		if err := tx.Commit(ctx); err != nil {
			return RealtimeRefund{}, false, err
		}
		return refund, false, nil
	}

	var (
		state, payoutStatus                        string
		buyerID, supplierID, settlementID, entryID uuid.UUID
		buyerCharge, supplierGross, platformMargin float64
		fundingBound                               bool
	)
	err = tx.QueryRow(ctx, `
		SELECT c.state,c.buyer_id,c.supplier_id,s.id,s.buyer_charge_usd::float8,
		       s.supplier_gross_usd::float8,s.platform_margin_usd::float8,
		       le.id,le.payout_status,
		       EXISTS(SELECT 1 FROM supplier_payout_funding f WHERE f.ledger_entry_id=le.id)
		  FROM execution_contracts c
		  JOIN realtime_settlements s ON s.contract_id=c.id
		  JOIN ledger_entries le ON le.execution_contract_id=c.id AND le.kind='supplier_credit'
		 WHERE c.id=$1
		 FOR UPDATE OF c,le`, contractID).Scan(
		&state, &buyerID, &supplierID, &settlementID, &buyerCharge,
		&supplierGross, &platformMargin, &entryID, &payoutStatus, &fundingBound)
	if errors.Is(err, pgx.ErrNoRows) {
		return RealtimeRefund{}, false, errNotFound
	}
	if err != nil {
		return RealtimeRefund{}, false, err
	}
	if state != "VERIFIED" {
		return RealtimeRefund{}, false, errRealtimeNotRefundable
	}
	var alreadyRefunded bool
	if err := tx.QueryRow(ctx,
		`SELECT EXISTS(SELECT 1 FROM realtime_refunds WHERE contract_id=$1)`, contractID,
	).Scan(&alreadyRefunded); err != nil {
		return RealtimeRefund{}, false, err
	}
	if alreadyRefunded {
		return RealtimeRefund{}, false, errRealtimeNotRefundable
	}
	if fundingBound || (payoutStatus != PayoutHeld && payoutStatus != PayoutAwaitingFunding && payoutStatus != PayoutCarried) {
		return RealtimeRefund{}, false, errRealtimeRefundNeedsReversal
	}

	before := map[string]any{
		"buyer_charge_usd": buyerCharge, "supplier_payable_usd": supplierGross,
		"platform_margin_usd": platformMargin, "refund_usd": 0,
		"supplier_payout_state": payoutStatus,
	}
	// Read the settled amounts, then write the three reversal rows through the
	// single ledger writer.  These were INSERT ... SELECT statements, which is why
	// they survived the writer refactor -- but it also meant the realtime lane
	// wrote money rows that nothing else could audit.
	var refundBuyerUSD, refundSupplierUSD, refundPlatformUSD float64
	if err := tx.QueryRow(ctx, `
		SELECT buyer_charge_usd::float8, supplier_gross_usd::float8, platform_margin_usd::float8
		  FROM realtime_settlements WHERE contract_id=$1`, contractID).
		Scan(&refundBuyerUSD, &refundSupplierUSD, &refundPlatformUSD); err != nil {
		return RealtimeRefund{}, false, err
	}
	contract := contractID
	reversals := []ledgerInsert{
		{Kind: "buyer_refund", BuyerID: &buyerID, ExecutionContractID: &contract,
			AmountMicros: usdToMicros(refundBuyerUSD), PayoutStatus: "released"},
		{Kind: "clawback", SupplierID: &supplierID, ExecutionContractID: &contract,
			AmountMicros: -usdToMicros(refundSupplierUSD), PayoutStatus: "clawed_back"},
		{Kind: "platform_refund", ExecutionContractID: &contract,
			AmountMicros: -usdToMicros(refundPlatformUSD), PayoutStatus: "released"},
	}
	for _, entry := range reversals {
		if _, err := insertLedgerEntryTx(ctx, tx, entry); err != nil {
			return RealtimeRefund{}, false, err
		}
	}
	tag, err := tx.Exec(ctx, `
		UPDATE ledger_entries SET payout_status='clawed_back',release_at=NULL
		 WHERE id=$1 AND payout_status=$2`, entryID, payoutStatus)
	if err != nil {
		return RealtimeRefund{}, false, err
	}
	if tag.RowsAffected() != 1 {
		return RealtimeRefund{}, false, errRealtimeRefundNeedsReversal
	}
	after := map[string]any{
		"buyer_charge_usd": buyerCharge, "buyer_refund_usd": buyerCharge,
		"net_buyer_charge_usd": 0, "supplier_payable_usd": supplierGross,
		"supplier_clawback_usd": supplierGross, "net_supplier_payable_usd": 0,
		"platform_margin_usd": platformMargin, "platform_refund_usd": platformMargin,
		"net_platform_margin_usd": 0, "supplier_payout_state": PayoutClawedBack,
	}
	actionID, err := insertAdminMutationActionWithID(ctx, tx, actor, intent, nil,
		&supplierID, nil, before, after)
	if err != nil {
		return RealtimeRefund{}, false, err
	}
	refundID := uuid.New()
	if _, err := tx.Exec(ctx, `
		INSERT INTO realtime_refunds
		 (id,contract_id,admin_action_id,refund_mode,reason_code,reason,correlation_ref,
		  buyer_refund_usd,supplier_clawback_usd,platform_refund_usd,
		  internal_credit_state,external_cash_state)
		SELECT $1,$2,$3,'FULL_INTERNAL_CREDIT','OPERATOR_CONFIRMED_PLATFORM_FAULT',$4,$5,
		       buyer_charge_usd,supplier_gross_usd,platform_margin_usd,'RECORDED','NOT_REQUESTED'
		  FROM realtime_settlements WHERE id=$6 AND contract_id=$2`, refundID, contractID,
		actionID, intent.Reason, intent.CorrelationRef, settlementID); err != nil {
		return RealtimeRefund{}, false, err
	}
	if _, err := tx.Exec(ctx, `
		INSERT INTO realtime_authorization_events (contract_id,kind,amount_usd)
		SELECT contract_id,'REFUNDED',buyer_refund_usd FROM realtime_refunds WHERE id=$1`,
		refundID); err != nil {
		return RealtimeRefund{}, false, err
	}
	refund, err := realtimeRefundTx(ctx, tx, contractID)
	if err != nil {
		return RealtimeRefund{}, false, err
	}
	if err := tx.Commit(ctx); err != nil {
		return RealtimeRefund{}, false, err
	}
	return refund, true, nil
}

func (s *Store) RealtimeOperationalSnapshot(ctx context.Context) (RealtimeOperationalSnapshot, error) {
	var snapshot RealtimeOperationalSnapshot
	err := s.pool.QueryRow(ctx, `
		SELECT
		  (SELECT count(*) FROM execution_contracts WHERE state='EXECUTING'),
		  COALESCE((SELECT extract(epoch FROM now()-min(created_at))
		              FROM execution_contracts WHERE state='EXECUTING'),0)::float8,
		  (SELECT count(*) FROM realtime_worker_offers
		    WHERE status='ACTIVE' AND last_seen_at > now()-interval '45 seconds'),
		  COALESCE((SELECT sum(available_sequences) FROM realtime_worker_offers
		    WHERE status='ACTIVE' AND last_seen_at > now()-interval '45 seconds'),0),
		  COALESCE((SELECT sum(amount_usd) FROM ledger_entries
		    WHERE execution_contract_id IS NOT NULL AND kind='supplier_credit'
		      AND payout_status IN ('held','awaiting_funding','ready','sending','outcome_unknown')),0)::float8,
		  COALESCE((SELECT sum(amount_usd) FROM ledger_entries
		    WHERE execution_contract_id IS NOT NULL AND kind='supplier_credit'
		      AND payout_status='reversal_required'),0)::float8,
		  COALESCE((SELECT sum(buyer_refund_usd) FROM realtime_refunds),0)::float8
	`).Scan(&snapshot.ExecutingContracts, &snapshot.OldestExecutingAgeSeconds,
		&snapshot.ActiveOffers, &snapshot.AvailableSequences, &snapshot.OpenSupplierPayableUSD,
		&snapshot.ReversalRequiredUSD, &snapshot.InternalRefundsUSD)
	return snapshot, err
}

type RealtimeReceipt struct {
	ReceiptID                  string                         `json:"receipt_id"`
	SettlementID               string                         `json:"settlement_id,omitempty"`
	RefundID                   string                         `json:"refund_id,omitempty"`
	ContractID                 string                         `json:"contract_id"`
	RequestID                  string                         `json:"request_id"`
	State                      string                         `json:"state"`
	Model                      string                         `json:"model"`
	RuntimeProfileID           string                         `json:"runtime_profile_id"`
	RuntimeProfileSHA256       string                         `json:"runtime_profile_sha256"`
	PlacementPlan              *RealtimePlacementPlan         `json:"placement_plan,omitempty"`
	PlacementPlanSHA256        string                         `json:"placement_plan_sha256,omitempty"`
	MarketClearing             *RealtimeMarketClearingReceipt `json:"market_clearing,omitempty"`
	PricingDecision            *PricingDecision               `json:"pricing_decision,omitempty"`
	PricingDecisionSHA256      string                         `json:"pricing_decision_sha256,omitempty"`
	PricingAuthorityStatus     string                         `json:"pricing_authority_status"`
	Coalescing                 *RealtimeCoalescingReceipt     `json:"coalescing,omitempty"`
	InputCommitment            string                         `json:"input_commitment"`
	StreamRootSHA256           string                         `json:"stream_root_sha256,omitempty"`
	OutputCommitment           string                         `json:"output_commitment,omitempty"`
	PromptTokens               int64                          `json:"prompt_tokens,omitempty"`
	CompletionTokens           int64                          `json:"completion_tokens,omitempty"`
	TotalTokens                int64                          `json:"total_tokens,omitempty"`
	TimeToFirstEventMS         int64                          `json:"time_to_first_event_ms,omitempty"`
	DurationMS                 int64                          `json:"duration_ms,omitempty"`
	Verification               string                         `json:"verification"`
	AuthorizationState         string                         `json:"authorization_state"`
	AuthorizedUSD              float64                        `json:"authorized_usd"`
	CapturedUSD                float64                        `json:"captured_usd"`
	ReleasedUSD                float64                        `json:"released_usd"`
	VoidedUSD                  float64                        `json:"voided_usd"`
	BuyerChargeUSD             float64                        `json:"buyer_charge_usd"`
	SupplierPayableUSD         float64                        `json:"supplier_payable_usd"`
	PlatformMarginUSD          float64                        `json:"platform_margin_usd"`
	RefundUSD                  float64                        `json:"refund_usd"`
	SupplierClawbackUSD        float64                        `json:"supplier_clawback_usd"`
	PlatformRefundUSD          float64                        `json:"platform_refund_usd"`
	NetBuyerChargeUSD          float64                        `json:"net_buyer_charge_usd"`
	NetSupplierPayableUSD      float64                        `json:"net_supplier_payable_usd"`
	NetPlatformMarginUSD       float64                        `json:"net_platform_margin_usd"`
	SettlementCurrency         string                         `json:"settlement_currency,omitempty"`
	BuyerChargeNanos           int64                          `json:"buyer_charge_nanos,omitempty"`
	SupplierPayableNanos       int64                          `json:"supplier_payable_nanos,omitempty"`
	KnownCostContributionNanos int64                          `json:"known_cost_contribution_nanos,omitempty"`
	SupplierPayoutState        string                         `json:"supplier_payout_state"`
	SupplierLedgerState        string                         `json:"supplier_ledger_state,omitempty"`
	RefundMode                 string                         `json:"refund_mode,omitempty"`
	RefundReasonCode           string                         `json:"refund_reason_code,omitempty"`
	RefundReason               string                         `json:"refund_reason,omitempty"`
	RefundCorrelationRef       string                         `json:"refund_correlation_ref,omitempty"`
	InternalCreditState        string                         `json:"internal_credit_state,omitempty"`
	ExternalCashState          string                         `json:"external_cash_state,omitempty"`
	FailureCode                string                         `json:"failure_code,omitempty"`
	CreatedAt                  time.Time                      `json:"created_at"`
	FinalizedAt                *time.Time                     `json:"finalized_at,omitempty"`
}

// RealtimeCoalescingReceipt makes the physical source of an in-flight follower
// inspectable by its buyer. Counterfactual values answer only "what would this
// same selected supplier entitlement have been if this follower ran again?";
// they never report provider cash, allocated costs, or true net contribution.
type RealtimeCoalescingReceipt struct {
	Role                                    string `json:"role"`
	LeaderContractID                        string `json:"leader_contract_id"`
	CoalescedFollowerDeliveries             int64  `json:"coalesced_follower_deliveries"`
	CounterfactualPhysicalExecutionsAvoided int64  `json:"counterfactual_physical_executions_avoided"`
	CounterfactualSupplierEntitlementNanos  int64  `json:"counterfactual_supplier_entitlement_nanos"`
	Currency                                string `json:"currency"`
}

func (s *Store) realtimeCoalescingReceipt(
	ctx context.Context, buyerID, contractID uuid.UUID,
) (*RealtimeCoalescingReceipt, error) {
	var follower RealtimeCoalescingReceipt
	err := s.pool.QueryRow(ctx, `
		SELECT leader_contract_id::text, counterfactual_supplier_entitlement_nanos,
		       currency
		  FROM realtime_coalesced_deliveries
		 WHERE follower_contract_id=$1 AND buyer_id=$2`, contractID, buyerID).
		Scan(&follower.LeaderContractID, &follower.CounterfactualSupplierEntitlementNanos,
			&follower.Currency)
	if err == nil {
		follower.Role = "FOLLOWER"
		follower.CoalescedFollowerDeliveries = 1
		follower.CounterfactualPhysicalExecutionsAvoided = 1
		return &follower, nil
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return nil, err
	}

	var leader RealtimeCoalescingReceipt
	err = s.pool.QueryRow(ctx, `
		SELECT count(*)::bigint,
		       COALESCE(sum(counterfactual_supplier_entitlement_nanos),0)::bigint,
		       COALESCE(min(currency),'')
		  FROM realtime_coalesced_deliveries
		 WHERE leader_contract_id=$1 AND buyer_id=$2`, contractID, buyerID).
		Scan(&leader.CoalescedFollowerDeliveries,
			&leader.CounterfactualSupplierEntitlementNanos, &leader.Currency)
	if err != nil {
		return nil, err
	}
	if leader.CoalescedFollowerDeliveries == 0 {
		return nil, nil
	}
	leader.Role = "LEADER"
	leader.LeaderContractID = contractID.String()
	leader.CounterfactualPhysicalExecutionsAvoided = leader.CoalescedFollowerDeliveries
	return &leader, nil
}

func (s *Store) RealtimeReceipt(ctx context.Context, buyerID, contractID uuid.UUID) (RealtimeReceipt, error) {
	var receipt RealtimeReceipt
	var executionID, settlementID, refundID *uuid.UUID
	var finalized *time.Time
	var placementJSON []byte
	var placementSHA256 *string
	contract, contractErr := scanRealtimeContract(s.pool.QueryRow(ctx,
		`SELECT `+realtimeContractColumns+` FROM execution_contracts WHERE id=$1 AND buyer_id=$2`,
		contractID, buyerID))
	if errors.Is(contractErr, pgx.ErrNoRows) {
		return RealtimeReceipt{}, errNotFound
	}
	if contractErr != nil {
		return RealtimeReceipt{}, contractErr
	}
	err := s.pool.QueryRow(ctx, `
		SELECT c.id::text,c.request_id,c.state,c.model_alias,c.runtime_profile_id,
		       c.runtime_profile_sha256,c.placement_plan,c.placement_plan_sha256,
		       c.input_commitment,c.created_at,c.finalized_at,
		       e.id,s.id,r.id,COALESCE(s.receipt_id,''),
		       COALESCE(e.stream_root_sha256,''),COALESCE(e.output_commitment,''),
		       COALESCE(e.prompt_tokens,0),COALESCE(e.completion_tokens,0),
		       COALESCE(e.total_tokens,0),COALESCE(e.time_to_first_event_ms,0),
		       COALESCE(e.duration_ms,0),COALESCE(e.verification_state,'PENDING'),
		       COALESCE(e.failure_code,''),
		       money.buyer_charge,money.supplier_credit,money.platform_take,
		       money.buyer_refund,money.supplier_clawback,money.platform_refund,
		       authz.reserved,authz.captured,authz.released,
		       authz.voided,COALESCE(money.supplier_ledger_state,''),
		       COALESCE(r.refund_mode,''),COALESCE(r.reason_code,''),COALESCE(r.reason,''),
		       COALESCE(r.correlation_ref,''),COALESCE(r.internal_credit_state,''),
		       COALESCE(r.external_cash_state,''),COALESCE(s.currency,''),
		       COALESCE(s.buyer_charge_nanos,0),COALESCE(s.supplier_gross_nanos,0),
		       COALESCE(s.known_cost_contribution_nanos,0)
		  FROM execution_contracts c
		  LEFT JOIN realtime_executions e ON e.contract_id=c.id
		  LEFT JOIN realtime_settlements s ON s.contract_id=c.id
		  LEFT JOIN realtime_refunds r ON r.contract_id=c.id
		  LEFT JOIN LATERAL (
		    SELECT COALESCE(-sum(amount_usd) FILTER (WHERE kind='buyer_charge'),0)::float8 buyer_charge,
		           COALESCE(sum(amount_usd) FILTER (WHERE kind='supplier_credit'),0)::float8 supplier_credit,
		           COALESCE(sum(amount_usd) FILTER (WHERE kind='platform_take'),0)::float8 platform_take,
		           COALESCE(sum(amount_usd) FILTER (WHERE kind='buyer_refund'),0)::float8 buyer_refund,
		           COALESCE(-sum(amount_usd) FILTER (WHERE kind='clawback'),0)::float8 supplier_clawback,
		           COALESCE(-sum(amount_usd) FILTER (WHERE kind='platform_refund'),0)::float8 platform_refund,
		           COALESCE(max(payout_status) FILTER (WHERE kind='supplier_credit'),'') supplier_ledger_state
		      FROM ledger_entries WHERE execution_contract_id=c.id
		  ) money ON true
		  LEFT JOIN LATERAL (
		    SELECT COALESCE(sum(amount_usd) FILTER (WHERE kind='RESERVED'),0)::float8 reserved,
		           COALESCE(sum(amount_usd) FILTER (WHERE kind='CAPTURED'),0)::float8 captured,
		           COALESCE(sum(amount_usd) FILTER (WHERE kind='RELEASED'),0)::float8 released,
		           COALESCE(sum(amount_usd) FILTER (WHERE kind='VOIDED'),0)::float8 voided
		      FROM realtime_authorization_events WHERE contract_id=c.id
		  ) authz ON true
		 WHERE c.id=$1 AND c.buyer_id=$2
	`, contractID, buyerID).Scan(
		&receipt.ContractID, &receipt.RequestID, &receipt.State, &receipt.Model,
		&receipt.RuntimeProfileID, &receipt.RuntimeProfileSHA256,
		&placementJSON, &placementSHA256, &receipt.InputCommitment,
		&receipt.CreatedAt, &finalized, &executionID, &settlementID, &refundID, &receipt.ReceiptID,
		&receipt.StreamRootSHA256,
		&receipt.OutputCommitment, &receipt.PromptTokens, &receipt.CompletionTokens,
		&receipt.TotalTokens, &receipt.TimeToFirstEventMS, &receipt.DurationMS,
		&receipt.Verification, &receipt.FailureCode, &receipt.BuyerChargeUSD,
		&receipt.SupplierPayableUSD, &receipt.PlatformMarginUSD, &receipt.RefundUSD,
		&receipt.SupplierClawbackUSD, &receipt.PlatformRefundUSD, &receipt.AuthorizedUSD,
		&receipt.CapturedUSD, &receipt.ReleasedUSD, &receipt.VoidedUSD,
		&receipt.SupplierLedgerState, &receipt.RefundMode, &receipt.RefundReasonCode,
		&receipt.RefundReason, &receipt.RefundCorrelationRef, &receipt.InternalCreditState,
		&receipt.ExternalCashState, &receipt.SettlementCurrency, &receipt.BuyerChargeNanos,
		&receipt.SupplierPayableNanos, &receipt.KnownCostContributionNanos)
	if errors.Is(err, pgx.ErrNoRows) {
		return RealtimeReceipt{}, errNotFound
	}
	if err != nil {
		return RealtimeReceipt{}, err
	}
	if len(placementJSON) > 0 && placementSHA256 != nil {
		plan, decodeErr := decodeRealtimePlacementPlan(placementJSON, *placementSHA256)
		if decodeErr != nil {
			return RealtimeReceipt{}, decodeErr
		}
		if plan.RuntimeProfileID != receipt.RuntimeProfileID ||
			plan.RuntimeProfileSHA256 != receipt.RuntimeProfileSHA256 {
			return RealtimeReceipt{}, errors.New("receipt placement plan does not match runtime profile")
		}
		receipt.PlacementPlan = &plan
		receipt.PlacementPlanSHA256 = *placementSHA256
	}
	receipt.PricingAuthorityStatus = "legacy_unverifiable"
	if contract.Pricing != nil {
		receipt.PricingDecision = contract.Pricing
		receipt.PricingDecisionSHA256 = contract.PricingDecisionSHA256
		receipt.PricingAuthorityStatus = "verified"
	}
	receipt.MarketClearing = contract.MarketClearing
	coalescing, err := s.realtimeCoalescingReceipt(ctx, buyerID, contractID)
	if err != nil {
		return RealtimeReceipt{}, err
	}
	receipt.Coalescing = coalescing
	receipt.FinalizedAt = finalized
	if receipt.ReceiptID == "" && executionID != nil {
		receipt.ReceiptID = "rcp_" + executionID.String()
	}
	if settlementID != nil {
		receipt.SettlementID = "set_" + settlementID.String()
	}
	if refundID != nil {
		receipt.RefundID = "rfd_" + refundID.String()
	}
	switch {
	case receipt.RefundUSD > 0:
		receipt.AuthorizationState = "REFUNDED"
	case receipt.VoidedUSD > 0:
		receipt.AuthorizationState = "VOIDED"
	case receipt.CapturedUSD > 0:
		receipt.AuthorizationState = "CAPTURED"
	case receipt.AuthorizedUSD > 0:
		receipt.AuthorizationState = "RESERVED"
	default:
		receipt.AuthorizationState = "UNKNOWN"
	}
	receipt.NetBuyerChargeUSD = roundRealtimeUSD(receipt.BuyerChargeUSD - receipt.RefundUSD)
	receipt.NetSupplierPayableUSD = roundRealtimeUSD(receipt.SupplierPayableUSD - receipt.SupplierClawbackUSD)
	receipt.NetPlatformMarginUSD = roundRealtimeUSD(receipt.PlatformMarginUSD - receipt.PlatformRefundUSD)
	receipt.SupplierPayoutState = realtimePayoutState(receipt.SupplierLedgerState)
	return receipt, nil
}

func roundRealtimeUSD(value float64) float64 {
	return math.Round(value*1_000_000) / 1_000_000
}

func realtimePayoutState(state string) string {
	switch state {
	case PayoutHeld:
		return "VERIFICATION_HOLD"
	case PayoutAwaitingFunding, PayoutCarried:
		return "ACCRUED"
	case PayoutReady, PayoutSending, PayoutOutcomeUnknown:
		return "TRANSFER_PENDING"
	case PayoutReleased:
		return "TRANSFERRED"
	case PayoutExported:
		return "PAYOUT_PENDING"
	case PayoutClawedBack:
		return "REVERSED"
	case PayoutReversalRequired:
		return "DISPUTED"
	default:
		return "UNACCRUED"
	}
}
