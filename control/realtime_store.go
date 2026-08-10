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
	// errRealtimeFreeCreditCurrencyMismatch separates "you are empty" from "you
	// hold credit this deployment cannot spend". Both refuse; only one is fixed
	// by topping up. No conversion is implied — free credit stays micro-USD.
	errRealtimeFreeCreditCurrencyMismatch = errors.New("free credit currency does not match the settlement currency")
	// errRealtimeTopupRequired is returned when a saved card is present but the
	// buyer lacks free credit + prepaid balance to cover the contract ceiling.
	// A card is a top-up rail, not authorization funding; the buyer must top up
	// prepaid balance before retrying. Do not auto-charge the card.
	errRealtimeTopupRequired       = errors.New("insufficient prepaid balance for the maximum request price; top up prepaid balance before retrying")
	errRealtimeNotRefundable       = errors.New("realtime settlement is not eligible for an internal refund")
	errRealtimeRefundNeedsReversal = errors.New("supplier payout crossed the internal transfer boundary; external reversal is required")
)

// Realtime money/capacity lock hierarchy (every path that touches more than
// one of these must acquire in this order; reverse order is a deadlock):
//
//  1. Buyer funding: pg_advisory_xact_lock("realtime-buyer-funding|"+buyerID)
//     then buyers.row FOR UPDATE (see evaluateRealtimeBuyerFunding).
//  2. Offer capacity: UPDATE realtime_worker_offers (claim in authorize, or
//     releaseRealtimeCapacity on settle/fail/recover).
//
// Contract rows (execution_contracts FOR UPDATE) are per-contract and may be
// taken before or after (1)/(2) only when the transaction does not also need
// the opposite resource in the reverse order. FinalizeRealtimeSuccess locks
// the contract, then the buyer (maybeDebitPrepaidForRealtimeTx), then the
// offer — buyer before offer, which matches this hierarchy.
//
// History: commit a8159ac7 took the buyer funding lock *after* the offer claim
// to shorten the buyer-serialised window. Under single-buyer authorize+settle
// concurrency that crossed the hierarchy against FinalizeRealtimeSuccess and
// produced PostgreSQL 40P01 (buyers vs realtime_worker_offers). Hierarchy
// restored: funding before offer claim. Batching of lock+funding snapshot and
// contract+event insert stays — those only cut client/server RTs.
//
// evaluateRealtimeBuyerFunding locks the buyer row and requires already-settled
// money (free-credit grant + materialised prepaid balance, net of charges,
// prepaid debits, and the singular open-exposure definition) to cover needNanos.
// Available funds are micros-granular (balance_micros, free credit), so the
// exact nano need is ceiled to whole micros for the hold — never projected
// to-nearest. A saved payment method is never treated as funding.
//
// Open exposure composition (non-overlapping — see buyer_open_exposure.go):
//
//	committed = sqlBuyerOpenExposureMicros          // prepaid residual + leases
//	            // + ACTIVE envelopes + orphan spends
//	          + sqlOpenNonPrepaidJobResidualMicros  // free-credit batch
//	          + sqlOpenNonEnvelopeExecutingCeilingMicros
//
// Envelope-backed EXECUTING work is held only by the shared envelope/orphan
// terms. Pure realtime EXECUTING (no envelope spend) is held by the ceiling
// sibling. Free-credit batch jobs are held by reserved residual, not estimated.
// This matches prepaidOpenReservationMicros for every overlapping term so the
// same prepaid cash cannot back two obligations.
//
// Serialization is two-step on purpose: FOR UPDATE on the buyer row alone is
// not enough under READ COMMITTED when the reservation itself is written to a
// different table (execution_contracts). Concurrent authorizers that only
// locked buyers could each observe realtimeReserved=0, all pass, then each
// insert an EXECUTING row — overspending a prepaid balance that funds a single
// ceiling. An advisory xact lock keyed on the buyer forces authorizers for the
// same buyer to enter the check-and-reserve critical section one at a time for
// the rest of the transaction (through the EXECUTING insert and commit).
func evaluateRealtimeBuyerFunding(ctx context.Context, tx pgx.Tx, buyerID uuid.UUID, needNanos int64) error {
	if needNanos < 0 {
		return fmt.Errorf("realtime funding need must be non-negative")
	}
	currency := SettlementCurrencyCode()
	if _, err := ParseCurrency(currency); err != nil {
		return err
	}
	// One network round-trip for lock + multi-aggregate funding snapshot.
	// pg_advisory_xact_lock is held until this transaction ends; FOR UPDATE on
	// the buyer row still serialises non-authorizer writers of that row. The
	// batch is not a correctness change — only fewer client/server RTs inside
	// the buyer-serialised window. Round-trip count must stay at one SendBatch
	// (lock statement + funding SELECT); embedding shared SQL adds no query.
	batch := &pgx.Batch{}
	batch.Queue(`SELECT pg_advisory_xact_lock(hashtextextended($1, 0))`,
		"realtime-buyer-funding|"+buyerID.String())
	batch.Queue(`
		SELECT CASE WHEN $2='usd' THEN b.free_credit_usd::float8 ELSE 0::float8 END,
		       b.free_credit_usd::float8,
		       EXISTS(SELECT 1 FROM billing_customers bc
		               WHERE bc.buyer_id=b.id AND COALESCE(bc.default_payment_method,'')<>''),
		       COALESCE((SELECT balance_micros FROM buyer_prepaid_balances bp
		                  WHERE bp.buyer_id=b.id AND bp.currency=$2),0)::bigint,
		       COALESCE((SELECT -sum(le.amount_usd) FROM ledger_entries le
		                 WHERE le.buyer_id=b.id
		                   AND le.currency=$2
		                   AND le.kind IN ('buyer_charge','buyer_refund')),0)::float8,
		       COALESCE((SELECT -sum(le.amount_usd) FROM ledger_entries le
		                 WHERE le.buyer_id=b.id AND le.currency=$2
		                   AND le.kind IN ('prepaid_debit','prepaid_restore')),0)::float8,
		       `+sqlBuyerCommittedMoneyMicros("b.id", "$2")+`::bigint
		  FROM buyers b WHERE b.id=$1 AND b.deleted_at IS NULL FOR UPDATE`, buyerID, currency)
	br := tx.SendBatch(ctx, batch)
	defer br.Close()
	if _, err := br.Exec(); err != nil {
		return err
	}
	var (
		freeCredit, freeCreditUSDRaw, spent, prepaidDebited float64
		prepaidMicros, committedMicros                      int64
		hasPaymentMethod                                    bool
	)
	err := br.QueryRow().Scan(&freeCredit, &freeCreditUSDRaw, &hasPaymentMethod, &prepaidMicros, &spent, &prepaidDebited,
		&committedMicros)
	if errors.Is(err, pgx.ErrNoRows) {
		return errNotFound
	}
	if err != nil {
		return err
	}
	// Hold ceiling: exact nanos round UP to whole micros. LedgerMicrosFromNanos
	// (to-nearest) is wrong here — it under-holds sub-half-micro remainders.
	needMicros := ceilNanosToMicros(needNanos)
	if needMicros < 0 {
		return fmt.Errorf("realtime funding need must be non-negative")
	}
	// +prepaidDebited undoes double-count: spent includes buyer_charge while
	// balance already fell on prepaid_debit. Every KindPrepaidRestore nets that
	// add-back when buyer_refund has zeroed spent (realtime refund, dispute
	// refund). SLA premium materialisation uses KindPrepaidBalanceReturn and
	// deliberately does not enter this sum — sla_refund is outside spent, so
	// the debit must keep cancelling the still-present premium charge.
	availableMicros := usdToMicros(freeCredit) + prepaidMicros -
		usdToMicros(spent) + usdToMicros(prepaidDebited) -
		committedMicros
	if availableMicros >= needMicros {
		return nil
	}
	// Free credit is frozen as micro-USD and is deliberately not converted, so on
	// a non-USD deployment it contributes nothing. Saying only "insufficient
	// authorized balance" to a buyer who is in fact holding credit sends them to
	// top up money they already have. Name the mismatch instead; still refuse,
	// and still convert nothing.
	if currency != "usd" && freeCreditUSDRaw > 0 &&
		availableMicros+usdToMicros(freeCreditUSDRaw) >= needMicros {
		return fmt.Errorf("%w: free credit is denominated in usd and cannot fund %s settlement",
			errRealtimeFreeCreditCurrencyMismatch, currency)
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
	ID                   uuid.UUID
	RequestID            string
	BuyerID              uuid.UUID
	ModelAlias           string
	RuntimeProfileID     string
	RuntimeProfileSHA256 string
	InputCommitment      string
	RequestSHA256        string
	PlacementPlan        RealtimePlacementPlan
	PlacementPlanSHA256  string
	// These two names mirror legacy database columns. Their values are
	// settlement-major projections in Currency; the truthful USD source values
	// live in Pricing.Realtime/RealtimeReuse and are the only X-Merc-Max-USD
	// authority.
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
	// MarketDecision is the canonical accepted market authority when the stored
	// market_clearing column carries a shape-discriminated decision (Step 7).
	// Historical rows store only RealtimeMarketClearingReceipt and leave this nil.
	MarketDecision *MarketDecision
	// MarketClearing is the legacy receipt projection. For new writes it is
	// projected losslessly from MarketDecision; for historical rows it is the
	// stored receipt itself.
	MarketClearing *RealtimeMarketClearingReceipt
	// WorkerPlacement is the Step 9 worker-choice binding for this authorize:
	// selected worker + MarketDecision citation + lane-discriminated fallback.
	// Derived at claim from the push book; not a second market authority.
	WorkerPlacement *WorkerPlacement
	Currency        string
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
	ReferenceCurrency           string                         `json:"reference_currency,omitempty"`
	SettlementCurrency          string                         `json:"settlement_currency,omitempty"`
	SupplierRateCurrency        string                         `json:"supplier_rate_currency,omitempty"`
	BuyerMoneyCurrency          string                         `json:"buyer_money_currency,omitempty"`
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
	RequestID                    string
	BuyerID                      uuid.UUID
	Profile                      VLLMRuntimeProfile
	InputCommitment              string
	RequestSHA256                string
	MaximumPriceUSD              float64
	EstimatedPriceUSD            float64
	MaximumPriceUSDNanos         int64
	EstimatedPriceUSDNanos       int64
	DeadlineAt                   time.Time
	IdempotencyKey               string
	MaximumPromptTokens          int64
	MaximumCompletionTokens      int64
	EstimatedPromptTokens        int64
	EstimatedCompletionTokens    int64
	BuyerDeclaredCeilingUSD      float64
	BuyerDeclaredCeilingUSDNanos int64
	FX                           RealtimeFXAuthority
	ReuseClass                   string
	// CoalescedLeaderContractID is required only for an in-flight follower. It
	// makes the physical source of a zero-physical settlement durable and lets
	// the receipt distinguish an avoided entitlement from true net contribution.
	CoalescedLeaderContractID uuid.UUID
	// EnvelopeID, when set, funds this authorization from a pre-reserved
	// execution envelope instead of re-running evaluateRealtimeBuyerFunding.
	// The envelope's cap was already reserved against the buyer at create; this
	// path only atomically spends the envelope. Supplier selection and the
	// per-request PricingDecision are unchanged.
	EnvelopeID uuid.UUID
}

func realtimeContractMaximumReferenceUSD(contract RealtimeContract) (float64, error) {
	if contract.Pricing == nil {
		if contract.Currency == realtimeReferenceCurrency {
			return contract.MaximumPriceUSD, nil
		}
		return 0, errors.New("non-USD realtime contract lacks frozen USD pricing authority")
	}
	switch contract.Pricing.ExecutionMode {
	case pricingExecutionRealtime:
		_, maximum, err := realtimePricingReferenceLegacyProjection(*contract.Pricing)
		return maximum, err
	case pricingExecutionRealtimeReuse:
		return realtimeReuseReferenceProjection(*contract.Pricing)
	default:
		return 0, errors.New("realtime contract has unsupported USD pricing projection")
	}
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
	// Shape-discriminated MarketDecision (Step 7) is the canonical stored form
	// for new realtime admits. Detect by market_shape; project the legacy receipt.
	var shapeProbe struct {
		MarketShape string `json:"market_shape"`
	}
	if err := json.Unmarshal(raw, &shapeProbe); err != nil {
		return fmt.Errorf("decode realtime market-clearing payload: %w", err)
	}
	if strings.TrimSpace(shapeProbe.MarketShape) != "" {
		var md MarketDecision
		if err := json.Unmarshal(raw, &md); err != nil {
			return fmt.Errorf("decode realtime MarketDecision: %w", err)
		}
		if err := ValidateMarketDecision(md); err != nil {
			return fmt.Errorf("realtime MarketDecision invalid: %w", err)
		}
		// Realtime lane only accepts push books. A pull body on this column is a
		// shape violation even if ValidateMarketDecision would accept a complete
		// pull snapshot on the batch path.
		if md.MarketShape != marketShapePushOrderBook {
			return refuseRealtimePullMarketDecision()
		}
		market, err := projectRealtimeMarketClearingReceipt(md)
		if err != nil {
			return err
		}
		if err := bindRealtimeMarketClearingReceipt(contract, market); err != nil {
			return err
		}
		contract.MarketDecision = &md
		contract.MarketClearing = market
		return nil
	}

	var market RealtimeMarketClearingReceipt
	if err := json.Unmarshal(raw, &market); err != nil {
		return fmt.Errorf("decode realtime market-clearing receipt: %w", err)
	}
	if err := validateRealtimeMarketClearingReceiptShape(market, contract.Currency); err != nil {
		return err
	}
	if err := bindRealtimeMarketClearingReceipt(contract, &market); err != nil {
		return err
	}
	contract.MarketClearing = &market
	return nil
}

func validateRealtimeMarketClearingReceiptShape(market RealtimeMarketClearingReceipt, settlementCurrency string) error {
	if (market.Version < 1 || market.Version > 3) || market.CandidateCount <= 0 ||
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
	if (market.Version == 2 || market.Version == 3) && (market.ReferenceCurrency != realtimeReferenceCurrency ||
		market.SettlementCurrency != settlementCurrency) {
		return errors.New("realtime market-clearing receipt lacks explicit reference/settlement currencies")
	}
	if market.Version == 1 && (market.ReferenceCurrency != "" || market.SettlementCurrency != "") {
		return errors.New("legacy realtime market-clearing receipt carries future currency fields")
	}
	if market.Version == 3 {
		if market.SupplierRateCurrency != market.ReferenceCurrency ||
			market.BuyerMoneyCurrency != market.SettlementCurrency ||
			market.RankingInputs == nil ||
			market.RankingInputs.RateCurrency != market.ReferenceCurrency {
			return errors.New("realtime market-clearing receipt does not map each money field to its currency")
		}
	} else if market.SupplierRateCurrency != "" || market.BuyerMoneyCurrency != "" ||
		(market.RankingInputs != nil && market.RankingInputs.RateCurrency != "") {
		return errors.New("historical realtime market-clearing receipt carries future per-field currency authority")
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
	// Honesty tripwire also on historical-shaped receipts that carry rank > 1.
	if market.SelectedRank > 1 && strings.Contains(market.SelectionReason, "lowest verified-outcome cost") {
		return errors.New("realtime market-clearing receipt claims lowest cost while selected_rank > 1")
	}
	return nil
}

func bindRealtimeMarketClearingReceipt(contract *RealtimeContract, market *RealtimeMarketClearingReceipt) error {
	if market == nil {
		return errors.New("realtime market-clearing receipt is nil")
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
	if contract.Pricing.Realtime != nil {
		a := contract.Pricing.Realtime
		wantInput, wantOutput := a.SupplierInputNanosPerMillion, a.SupplierOutputNanosPerMillion
		if a.Version == realtimePricingAuthorityVersion {
			wantInput = a.SupplierInputReferenceNanosPerMillion
			wantOutput = a.SupplierOutputReferenceNanosPerMillion
		}
		if market.SelectedSupplierInputNanos != wantInput ||
			market.SelectedSupplierOutputNanos != wantOutput {
			return errors.New("realtime market-clearing receipt supplier USD rates disagree with PricingDecision")
		}
	}
	return nil
}

// newRealtimeMarketClearingReceipt builds a version-3 legacy receipt for probe
// and measurement helpers that do not freeze a full MarketDecision book. The
// production authorize path builds MarketDecision first and projects via
// projectRealtimeMarketClearingReceipt.
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
	inputs.RateCurrency = pricing.Realtime.ReferenceCurrency
	if inputs.WarmthRank == 0 && inputs.Warmth != "HOT" {
		inputs.WarmthRank = warmthRank(inputs.Warmth)
	}
	market := &RealtimeMarketClearingReceipt{
		Version:                     3,
		ReferenceCurrency:           pricing.Realtime.ReferenceCurrency,
		SettlementCurrency:          pricing.Currency,
		SupplierRateCurrency:        pricing.Realtime.ReferenceCurrency,
		BuyerMoneyCurrency:          pricing.Currency,
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
		SelectionReason:             realtimeClearingSelectionReason(inputs, selectedRank, candidateCount),
		RankingInputs:               &inputs,
	}
	if market.PositiveContributionNanos <= 0 || !validSHA256(pricingSHA256) {
		return nil, errors.New("realtime market-clearing receipt lacks positive PricingDecision contribution")
	}
	if market.SelectedRank > 1 && strings.Contains(market.SelectionReason, "lowest verified-outcome cost") {
		return nil, errors.New("realtime market-clearing receipt claims lowest cost while selected_rank > 1")
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
	switch pricing.ExecutionMode {
	case pricingExecutionRealtime:
		if err := validateFrozenRealtimePricingDecision(pricing); err != nil {
			return err
		}
		a := pricing.Realtime
		if a.RuntimeProfileID != contract.RuntimeProfileID ||
			a.RuntimeProfileSHA256 != contract.RuntimeProfileSHA256 ||
			a.PlacementPlanSHA256 != contract.PlacementPlanSHA256 ||
			a.InputCommitment != contract.InputCommitment || a.RequestSHA256 != contract.RequestSHA256 ||
			a.MaximumPromptTokens != contract.MaximumPromptTokens ||
			a.MaximumCompletionTokens != contract.MaximumCompletionTokens ||
			a.EstimatedPromptTokens != contract.EstimatedPromptTokens ||
			a.EstimatedCompletionTokens != contract.EstimatedCompletionTokens ||
			a.BuyerDeclaredCeilingNanos != contract.BuyerDeclaredCeilingNanos ||
			pricing.Currency != contract.Currency {
			return errors.New("frozen realtime PricingDecision disagrees with durable contract identity or bounds")
		}
		buyerInput, inputErr := nanoRatePerMillionFromFloat(contract.BuyerInputUSDPerMillionTokens)
		buyerOutput, outputErr := nanoRatePerMillionFromFloat(contract.BuyerOutputUSDPerMillionTokens)
		supplierInput, supplierInputErr := nanoRatePerMillionFromFloat(contract.SupplierInputUSDPerMillionTokens)
		supplierOutput, supplierOutputErr := nanoRatePerMillionFromFloat(contract.SupplierOutputUSDPerMillionTokens)
		if inputErr != nil || outputErr != nil || supplierInputErr != nil || supplierOutputErr != nil {
			return errors.New("durable realtime contract has invalid USD rate projections")
		}
		if a.Version == realtimePricingAuthorityVersion {
			if a.BuyerInputReferenceNanosPerMillion != int64(buyerInput) ||
				a.BuyerOutputReferenceNanosPerMillion != int64(buyerOutput) ||
				a.SupplierInputReferenceNanosPerMillion != int64(supplierInput) ||
				a.SupplierOutputReferenceNanosPerMillion != int64(supplierOutput) {
				return errors.New("frozen realtime USD rates disagree with durable contract projections")
			}
		} else if a.BuyerInputNanosPerMillion != int64(buyerInput) ||
			a.BuyerOutputNanosPerMillion != int64(buyerOutput) ||
			a.SupplierInputNanosPerMillion != int64(supplierInput) ||
			a.SupplierOutputNanosPerMillion != int64(supplierOutput) {
			return errors.New("legacy realtime rates disagree with durable contract projections")
		}
	case pricingExecutionRealtimeReuse:
		if err := validateFrozenRealtimeReusePricingDecision(pricing); err != nil {
			return err
		}
		a := pricing.RealtimeReuse
		if a.RuntimeProfileID != contract.RuntimeProfileID ||
			a.RuntimeProfileSHA256 != contract.RuntimeProfileSHA256 ||
			a.InputCommitment != contract.InputCommitment || a.RequestSHA256 != contract.RequestSHA256 ||
			a.ResultCommitment != contract.ReuseResultCommitment || a.ReuseClass != contract.ReuseClass ||
			a.DeliveredTokens != contract.ReuseDeliveredTokens ||
			a.BuyerDeclaredCeilingNanos != contract.BuyerDeclaredCeilingNanos ||
			pricing.Currency != contract.Currency {
			return errors.New("frozen realtime reuse PricingDecision disagrees with durable contract identity")
		}
		buyerInput, inputErr := nanoRatePerMillionFromFloat(contract.BuyerInputUSDPerMillionTokens)
		buyerOutput, outputErr := nanoRatePerMillionFromFloat(contract.BuyerOutputUSDPerMillionTokens)
		if inputErr != nil || outputErr != nil {
			return errors.New("durable realtime reuse contract has invalid USD rate projections")
		}
		full := buyerInput
		if buyerOutput > full {
			full = buyerOutput
		}
		if (a.Version == realtimeReusePricingAuthorityVersion &&
			a.ReferenceFullRateNanosPerMillion != int64(full)) ||
			(a.Version == realtimeReusePricingLegacyVersion &&
				a.FullRateNanosPerMillion != int64(full)) {
			return errors.New("frozen realtime reuse USD rate disagrees with durable contract projection")
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
	currency, err := SettlementCurrency()
	if err != nil {
		return RealtimeContract{}, false, err
	}
	fx, err := realtimeFXForNewIngress(auth.FX, currency)
	if err != nil {
		return RealtimeContract{}, false, err
	}
	auth.FX = fx
	referenceExpected, referenceMaximum, settlementExpected, settlementMaximum, err :=
		realtimeBuyerPriceBounds(
			auth.Profile,
			auth.MaximumPromptTokens, auth.MaximumCompletionTokens,
			auth.EstimatedPromptTokens, auth.EstimatedCompletionTokens,
			currency, fx)
	if err != nil || referenceExpected.Nanos <= 0 || referenceMaximum.Nanos < referenceExpected.Nanos ||
		settlementExpected.Nanos <= 0 || settlementMaximum.Nanos < settlementExpected.Nanos {
		return RealtimeContract{}, false, errors.New("realtime request lacks positive ordered USD and settlement price bounds")
	}
	referenceExpectedProjection, err := projectRealtimeNanosToMajor(referenceExpected)
	if err != nil {
		return RealtimeContract{}, false, err
	}
	referenceMaximumProjection, err := projectRealtimeNanosToMajor(referenceMaximum)
	if err != nil {
		return RealtimeContract{}, false, err
	}
	if auth.EstimatedPriceUSD != referenceExpectedProjection ||
		auth.MaximumPriceUSD != referenceMaximumProjection {
		return RealtimeContract{}, false, errors.New("realtime request USD projection disagrees with governed profile and token bounds")
	}
	if auth.EstimatedPriceUSDNanos < 0 || auth.MaximumPriceUSDNanos < 0 ||
		(auth.EstimatedPriceUSDNanos > 0 && auth.EstimatedPriceUSDNanos != referenceExpected.Nanos) ||
		(auth.MaximumPriceUSDNanos > 0 && auth.MaximumPriceUSDNanos != referenceMaximum.Nanos) {
		return RealtimeContract{}, false, errors.New("realtime request exact USD nanos disagree with governed profile and token bounds")
	}
	settlementExpectedProjection, err := projectRealtimeNanosToMajor(settlementExpected)
	if err != nil {
		return RealtimeContract{}, false, err
	}
	settlementMaximumProjection, err := projectRealtimeNanosToMajor(settlementMaximum)
	if err != nil {
		return RealtimeContract{}, false, err
	}
	// Funding + capacity + durable reservation (see lock hierarchy on
	// evaluateRealtimeBuyerFunding). Order is non-negotiable:
	//   buyer funding → offer capacity claim → EXECUTING insert → commit.
	// PostgreSQL holds row locks until COMMIT, so both locks cover the
	// remainder of the transaction. Taking the offer before the buyer was
	// tried (late funding lock) and deadlocked against settlement, which
	// locks the buyer before releasing offer capacity.
	//
	// Money correctness: evaluateRealtimeBuyerFunding serialises
	// check-and-reserve through the EXECUTING insert and commit, so concurrent
	// same-buyer admits cannot each observe reserved=0 and overspend.
	// Capacity correctness: available_sequences>0 in the UPDATE WHERE clause
	// still makes decrement-and-check atomic. Funding failure before the offer
	// claim never touches capacity; after a claim (impossible on the legacy
	// path now that funding is first) a rollback would still restore it.
	//
	// Envelope path: spend is one atomic UPDATE on the envelope row; it does
	// not take the buyer advisory funding lock and must not double-count as
	// EXECUTING realtimeReserved (see evaluateRealtimeBuyerFunding). Envelope
	// create already held buyer funding; admit only claims offer capacity.
	var envelopeSpend *ExecutionEnvelopeSpend
	if auth.EnvelopeID != uuid.Nil {
		needNanos, nerr := realtimeAuthNeedNanos(auth)
		if nerr != nil {
			return RealtimeContract{}, false, nerr
		}
		reserveTokens := auth.MaximumPromptTokens + auth.MaximumCompletionTokens
		if reserveTokens < 0 {
			reserveTokens = 0
		}
		// Envelope spends are always keyed. Prefer the client Idempotency-Key;
		// fall back to request id so a header-less call cannot invent unbounded
		// double-spends on retry of the same request id.
		spendKey := strings.TrimSpace(auth.IdempotencyKey)
		if spendKey == "" {
			spendKey = "request:" + strings.TrimSpace(auth.RequestID)
		}
		if spendKey == "request:" {
			return RealtimeContract{}, false, errors.New("envelope spend requires idempotency key or request id")
		}
		spend, serr := reserveEnvelopeSpendTx(ctx, tx, auth.EnvelopeID, auth.BuyerID,
			needNanos, reserveTokens, spendKey, auth.RequestID, auth.Profile)
		if serr != nil {
			return RealtimeContract{}, false, serr
		}
		// Idempotent replay of a spend that already bound a contract: return it
		// with the upstream credential opened, matching the non-envelope path.
		if spend.ContractID != nil {
			row := tx.QueryRow(ctx, `SELECT `+realtimeContractColumns+`
				FROM execution_contracts WHERE id=$1 AND buyer_id=$2`,
				*spend.ContractID, auth.BuyerID)
			var sealedToken *string
			// scanRealtimeContract does not open the sealed token; re-read sealed
			// only when we may actually hit the upstream again.
			contract, cerr := scanRealtimeContract(row)
			if cerr == nil {
				if contract.RequestSHA256 != auth.RequestSHA256 {
					return RealtimeContract{}, false, errRealtimeIdempotencyConflict
				}
				if err := tx.QueryRow(ctx, `
					SELECT upstream_token_sealed FROM execution_contracts WHERE id=$1`,
					contract.ID).Scan(&sealedToken); err != nil {
					return RealtimeContract{}, false, err
				}
				if sealedToken == nil || *sealedToken == "" {
					return RealtimeContract{}, false, errors.New("vLLM upstream credential cannot be opened")
				}
				contract.UpstreamToken = openToken(*sealedToken)
				if contract.UpstreamToken == "" {
					return RealtimeContract{}, false, errors.New("vLLM upstream credential cannot be opened")
				}
				return contract, true, nil
			}
			if !errors.Is(cerr, pgx.ErrNoRows) {
				return RealtimeContract{}, false, cerr
			}
		}
		envelopeSpend = &spend
	} else {
		// Legacy per-request path: buyer funding *before* offer claim.
		// A saved payment method is a top-up rail only — never admission funding.
		// Same exact nano ceiling the envelope path holds (realtimeAuthNeedNanos),
		// not the to-nearest micro projection of settlementMaximumProjection.
		needNanos, nerr := realtimeAuthNeedNanos(auth)
		if nerr != nil {
			return RealtimeContract{}, false, nerr
		}
		if err := evaluateRealtimeBuyerFunding(ctx, tx, auth.BuyerID, needNanos); err != nil {
			return RealtimeContract{}, false, err
		}
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
		consideredJSON       []byte
		availabilityMode     = marketAvailabilityBlockingForUpdate
	)
	// Reserve a sequence with an atomic decrement that is also the selection.
	// Hierarchy step 2: offer capacity after buyer funding.
	//
	// Multi-offer book: FOR UPDATE SKIP LOCKED ordered by verified-outcome
	// rank so concurrent admits take distinct free offers instead of all
	// piling onto selected_rank=1. Single-offer book: blocking FOR UPDATE
	// only — trying SKIP first when there is only one candidate doubles the
	// ranking CTE under contention (SKIP miss + BLOCK wait) and made the
	// 1-offer multi-buyer p95 worse in the tail probe.
	//
	// available_sequences > 0 on the UPDATE still makes decrement-and-check
	// atomic. When SKIP returns no rows on a multi-offer book (every
	// candidate locked, or a last-sequence race), fall back to blocking so
	// we wait rather than report no-supply while capacity sits idle.
	//
	// PostgreSQL holds the offer row lock until this transaction commits or
	// rolls back — not merely for the statement. Same-buyer admits therefore
	// serialise on the funding lock (already held) and then on the claimed
	// offer row for the remainder of the transaction.
	//
	// Ranking is verified-outcome cost first (base ask adjusted by measured
	// failure and refund rates when enough samples exist), then warmth only as
	// a tiebreak inside a cost class. Self-declared HOT cannot outrank a
	// materially cheaper measured cost — the same discipline as batch claim
	// (scheduler.go: cost rank wins; warmth breaks ties within a class).
	//
	// Reputation inputs come from realtime_supplier_outcome_stats, maintained
	// incrementally by triggers when contracts reach VERIFIED/FAILED and when
	// settlements/refunds are written. Money (buyer charge / supplier payable)
	// still comes only from the selected offer rates and PricingDecision —
	// never from these counters.
	//
	// The claim SQL also freezes the eligible considered book (MarketDecision
	// PUSH_ORDER_BOOK) so a rank>1 SKIP LOCKED win records lock-skipped peers
	// rather than rewriting the receipt as "lowest cost".
	//
	// The result feeds exactly one predicate: multiOfferBookProbe > 1, which
	// chooses SKIP LOCKED vs blocking claim. Cardinality past two is never
	// read, so the probe stops after two eligible rows (LIMIT 2) instead of
	// counting the whole book.
	var multiOfferBookProbe int
	if err := tx.QueryRow(ctx, realtimeOfferBookBranchProbeSQL,
		auth.Profile.RuntimeProfileID, auth.Profile.ProfileSHA256,
		auth.Profile.BuyerInputUSDPerMillionTokens, auth.Profile.BuyerOutputUSDPerMillionTokens,
	).Scan(&multiOfferBookProbe); err != nil {
		return RealtimeContract{}, false, err
	}
	scanOffer := func(sql string) error {
		return tx.QueryRow(ctx, sql,
			auth.Profile.RuntimeProfileID, auth.Profile.ProfileSHA256,
			auth.Profile.BuyerInputUSDPerMillionTokens, auth.Profile.BuyerOutputUSDPerMillionTokens,
			minRealtimeOutcomeSamples).
			Scan(&workerID, &supplierID, &baseURL, &sealed, &supplierInput, &supplierOutput,
				&placementJSON, &placementSHA256, &selectedWarmth, &candidateCount, &selectedRank,
				&terminalAttempts, &terminalFails, &verifiedSettlements, &refundCount,
				&consideredJSON)
	}
	if multiOfferBookProbe > 1 {
		err = scanOffer(realtimeAuthorizeSelectOfferSQLSkip)
		if errors.Is(err, pgx.ErrNoRows) {
			err = scanOffer(realtimeAuthorizeSelectOfferSQLBlocking)
			availabilityMode = marketAvailabilityBlockingForUpdate
		} else if err == nil {
			availabilityMode = marketAvailabilitySkipLocked
		}
	} else {
		err = scanOffer(realtimeAuthorizeSelectOfferSQLBlocking)
		availabilityMode = marketAvailabilityBlockingForUpdate
	}
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
	pricing, err := newRealtimePricingDecision(RealtimePricingInputs{
		Profile: auth.Profile, Placement: placementPlan,
		InputCommitment: auth.InputCommitment, RequestSHA256: auth.RequestSHA256,
		MaximumPromptTokens:       auth.MaximumPromptTokens,
		MaximumCompletionTokens:   auth.MaximumCompletionTokens,
		EstimatedPromptTokens:     auth.EstimatedPromptTokens,
		EstimatedCompletionTokens: auth.EstimatedCompletionTokens,
		SupplierInputRate:         supplierInput, SupplierOutputRate: supplierOutput,
		BuyerDeclaredCeiling:               auth.BuyerDeclaredCeilingUSD,
		BuyerDeclaredCeilingReferenceNanos: auth.BuyerDeclaredCeilingUSDNanos,
		FX:                                 auth.FX, Currency: currency,
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
	marketDecision, err := newRealtimePushMarketDecision(
		availabilityMode,
		candidateCount, selectedRank, workerID, supplierID, supplierInput, supplierOutput, pricing, pricingSHA256,
		rankingInputs, consideredJSON)
	if err != nil {
		return RealtimeContract{}, false, err
	}
	market, err := projectRealtimeMarketClearingReceipt(marketDecision)
	if err != nil {
		return RealtimeContract{}, false, err
	}
	marketDigest, err := marketDecisionDigest(marketDecision)
	if err != nil {
		return RealtimeContract{}, false, err
	}
	// Soft warmth belief: record offer last_seen_at when present so a warmth
	// tiebreak is auditable. LocalityDroveSelection stays false unless a
	// later step proves affinity moved the pick inside a cost class — the
	// authorize SQL does not return peer cost equality.
	var offerLastSeen time.Time
	_ = tx.QueryRow(ctx, `
		SELECT last_seen_at FROM realtime_worker_offers
		 WHERE worker_id=$1 AND runtime_profile_id=$2 AND runtime_profile_sha256=$3
		   AND status='ACTIVE'`,
		workerID, auth.Profile.RuntimeProfileID, auth.Profile.ProfileSHA256,
	).Scan(&offerLastSeen)
	workerPlacement, err := newRealtimeWorkerPlacement(
		marketDecision, marketDigest, selectedWarmth, offerLastSeen, false)
	if err != nil {
		return RealtimeContract{}, false, fmt.Errorf("bind realtime WorkerPlacement: %w", err)
	}
	// Persist the canonical MarketDecision; attach projects the legacy receipt.
	marketJSON, err := json.Marshal(marketDecision)
	if err != nil {
		return RealtimeContract{}, false, err
	}
	expectedProjection, maximumProjection, err := realtimePricingLegacyProjection(pricing)
	if err != nil || expectedProjection != settlementExpectedProjection ||
		maximumProjection != settlementMaximumProjection {
		return RealtimeContract{}, false, errors.New("realtime PricingDecision does not match converted settlement reserve projection")
	}
	pricingReferenceExpected, pricingReferenceMaximum, err := realtimePricingReferenceLegacyProjection(pricing)
	if err != nil || pricingReferenceExpected != auth.EstimatedPriceUSD ||
		pricingReferenceMaximum != auth.MaximumPriceUSD {
		return RealtimeContract{}, false, errors.New("realtime PricingDecision does not preserve USD request projection")
	}

	contractID := uuid.New()
	// Batch contract + RESERVED event (and optional envelope bind) into one
	// client/server round-trip so the buyer-funding lock is not held across
	// three sequential network waits after the check passes.
	reserveBatch := &pgx.Batch{}
	reserveBatch.Queue(`
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
		settlementMaximumProjection,
		settlementExpectedProjection, auth.Profile.BuyerInputUSDPerMillionTokens,
		auth.Profile.BuyerOutputUSDPerMillionTokens, supplierInput, supplierOutput,
		auth.DeadlineAt, realtimeNullIfEmpty(auth.IdempotencyKey), workerID, supplierID, baseURL, sealed,
		currency.Code(), auth.MaximumPromptTokens, auth.MaximumCompletionTokens,
		auth.EstimatedPromptTokens, auth.EstimatedCompletionTokens,
		pricing.Realtime.BuyerDeclaredCeilingNanos, pricingJSON, pricingSHA256, marketJSON)
	reserveBatch.Queue(`
		INSERT INTO realtime_authorization_events (contract_id,kind,amount_usd)
		VALUES ($1,'RESERVED',$2)`, contractID, settlementMaximumProjection)
	if envelopeSpend != nil {
		reserveBatch.Queue(`
			UPDATE execution_envelope_spends
			   SET contract_id=$2
			 WHERE id=$1 AND state='RESERVED' AND contract_id IS NULL`,
			envelopeSpend.ID, contractID)
	}
	reserveBR := tx.SendBatch(ctx, reserveBatch)
	if _, err = reserveBR.Exec(); err != nil {
		_ = reserveBR.Close()
		return RealtimeContract{}, false, err
	}
	if _, err = reserveBR.Exec(); err != nil {
		_ = reserveBR.Close()
		return RealtimeContract{}, false, err
	}
	if envelopeSpend != nil {
		ct, berr := reserveBR.Exec()
		if berr != nil {
			_ = reserveBR.Close()
			return RealtimeContract{}, false, berr
		}
		if ct.RowsAffected() != 1 {
			_ = reserveBR.Close()
			return RealtimeContract{}, false, errors.New("envelope spend could not bind contract")
		}
	}
	if err = reserveBR.Close(); err != nil {
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
		MaximumPriceUSD: settlementMaximumProjection, EstimatedPriceUSD: settlementExpectedProjection,
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
		Pricing:                   &pricing, PricingDecisionSHA256: pricingSHA256,
		MarketDecision: &marketDecision, MarketClearing: market,
		WorkerPlacement: &workerPlacement,
		Currency:        currency.Code(),
	}, false, nil
}

// realtimeAuthNeedNanos is the exact settlement-currency ceiling held against
// an envelope. It derives from USD profile rates and the frozen FX authority;
// the X-Merc-Max-USD display projection is never relabeled as settlement money.
func realtimeAuthNeedNanos(auth RealtimeContractAuthorization) (int64, error) {
	settlement, err := ParseCurrency(auth.FX.SettlementCurrency)
	if err != nil {
		return 0, errors.New("realtime envelope spend lacks frozen settlement currency")
	}
	_, _, _, maximum, err := realtimeBuyerPriceBounds(
		auth.Profile,
		auth.MaximumPromptTokens, auth.MaximumCompletionTokens,
		auth.EstimatedPromptTokens, auth.EstimatedCompletionTokens,
		settlement, auth.FX)
	if err != nil || maximum.Nanos <= 0 {
		return 0, errors.New("realtime envelope spend maximum cannot be derived from frozen USD/FX authority")
	}
	return maximum.Nanos, nil
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
	// PricingDecisionSHA256 is the contract digest that authorized this settle.
	PricingDecisionSHA256 string `json:"pricing_decision_sha256,omitempty"`
	// FinalityStatus is KNOWN_COST_SETTLED for verified token economics — not
	// batch ECONOMIC_FINAL / true-net.
	FinalityStatus   string   `json:"finality_status,omitempty"`
	FinalityBlockers []string `json:"finality_blockers,omitempty"`
	// EconomicFinal is true only for ECONOMIC_FINAL with empty blockers. Realtime
	// never sets this while true-net remains refused on the lane.
	EconomicFinal bool `json:"economic_final"`
}

type RealtimeOperationalSnapshot struct {
	ExecutingContracts        int64
	OldestExecutingAgeSeconds float64
	ActiveOffers              int64
	AvailableSequences        int64
	OpenSupplierPayableUSD    float64
	ReversalRequiredUSD       float64
	InternalRefundsUSD        float64
	MoneyByCurrency           []RealtimeOperationalCurrencySnapshot
}

// RealtimeOperationalCurrencySnapshot keeps operational liabilities in their
// accepted currency. The legacy *_USD fields above remain a truthful USD-only
// compatibility projection; they never sum CAD/JPY numerics into USD.
type RealtimeOperationalCurrencySnapshot struct {
	Currency            string
	OpenSupplierPayable float64
	ReversalRequired    float64
	InternalRefunds     float64
}

func tokenChargeExact(prompt, completion int64, inputRate, outputRate float64, supplier bool) (MoneyNanos, error) {
	// Runtime profiles and worker offer rates are explicitly USD reference
	// values. Settlement conversion happens later through RealtimeFXAuthority.
	currency := MustParseCurrency(realtimeReferenceCurrency)
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

// tokenCharge is the compatibility USD projection used by request preparation.
// Settlement-major projections are derived from the frozen PricingDecision.
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

// releaseRealtimeCapacity returns one sequence to the offer. Callers that also
// hold buyer funding locks must already have acquired them (hierarchy: buyer
// funding before offer). FinalizeRealtimeSuccess does buyers FOR UPDATE in
// maybeDebitPrepaidForRealtimeTx before this; FinalizeRealtimeFailure and
// RecoverStaleRealtimeContracts touch only the contract then the offer.
func releaseRealtimeCapacity(ctx context.Context, tx pgx.Tx, workerID uuid.UUID, profileID string) error {
	_, err := tx.Exec(ctx, `
		UPDATE realtime_worker_offers
		   SET available_sequences=LEAST(max_active_sequences,available_sequences+1),updated_at=now()
		 WHERE worker_id=$1 AND runtime_profile_id=$2`, workerID, profileID)
	return err
}

func (s *Store) existingRealtimeReuseReplay(
	ctx context.Context,
	auth RealtimeContractAuthorization,
	deliveredTokens int64,
	outputCommitment string,
) (RealtimeContract, RealtimeSettlement, bool, error) {
	if auth.IdempotencyKey == "" {
		return RealtimeContract{}, RealtimeSettlement{}, false, nil
	}
	existing, err := scanRealtimeContract(s.pool.QueryRow(ctx, `SELECT `+realtimeContractColumns+`
		FROM execution_contracts WHERE buyer_id=$1 AND idempotency_key=$2`,
		auth.BuyerID, auth.IdempotencyKey))
	if errors.Is(err, pgx.ErrNoRows) {
		return RealtimeContract{}, RealtimeSettlement{}, false, nil
	}
	if err != nil {
		return RealtimeContract{}, RealtimeSettlement{}, false, err
	}
	if existing.RequestSHA256 != auth.RequestSHA256 || existing.Pricing == nil ||
		existing.Pricing.ExecutionMode != pricingExecutionRealtimeReuse ||
		existing.ReuseClass != auth.ReuseClass || existing.ReuseResultCommitment != outputCommitment ||
		existing.ReuseDeliveredTokens != deliveredTokens {
		return RealtimeContract{}, RealtimeSettlement{}, false, errRealtimeIdempotencyConflict
	}
	var settlement RealtimeSettlement
	err = s.pool.QueryRow(ctx, `
		SELECT id,buyer_charge_usd::float8,supplier_gross_usd::float8,
		       platform_margin_usd::float8,COALESCE(currency,''),
		       COALESCE(buyer_charge_nanos,0),COALESCE(supplier_gross_nanos,0),
		       COALESCE(known_cost_contribution_nanos,0)
		  FROM realtime_settlements WHERE contract_id=$1`, existing.ID).Scan(
		&settlement.ID, &settlement.BuyerChargeUSD, &settlement.SupplierPayableUSD,
		&settlement.PlatformMarginUSD, &settlement.Currency, &settlement.BuyerChargeNanos,
		&settlement.SupplierPayableNanos, &settlement.KnownCostContributionNanos)
	if err != nil {
		return RealtimeContract{}, RealtimeSettlement{}, false, err
	}
	if auth.ReuseClass == ClassCoalescedDelivery {
		var recordedLeader uuid.UUID
		if err := s.pool.QueryRow(ctx, `
			SELECT leader_contract_id FROM realtime_coalesced_deliveries
			 WHERE follower_contract_id=$1`, existing.ID).Scan(&recordedLeader); err != nil ||
			recordedLeader != auth.CoalescedLeaderContractID {
			return RealtimeContract{}, RealtimeSettlement{}, false, errRealtimeIdempotencyConflict
		}
	}
	return existing, settlement, true, nil
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
	if existing, settlement, found, err := s.existingRealtimeReuseReplay(
		ctx, auth, money.DeliveredTokens, outputCommitment,
	); err != nil || found {
		return existing, settlement, err
	}
	fx, err := realtimeFXForNewIngress(auth.FX, currency)
	if err != nil {
		return RealtimeContract{}, RealtimeSettlement{}, err
	}
	auth.FX = fx
	pricing, err := newRealtimeReusePricingDecision(RealtimeReusePricingInputs{
		Profile: auth.Profile, InputCommitment: auth.InputCommitment, RequestSHA256: auth.RequestSHA256,
		ResultCommitment: outputCommitment, ReuseClass: auth.ReuseClass, DeliveredTokens: money.DeliveredTokens,
		BuyerDeclaredCeiling:               auth.BuyerDeclaredCeilingUSD,
		BuyerDeclaredCeilingReferenceNanos: auth.BuyerDeclaredCeilingUSDNanos,
		FX:                                 auth.FX, Currency: currency,
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
	// that physical execution would have reserved. Exact nanos, ceiled to
	// micros inside evaluateRealtimeBuyerFunding.
	if err := evaluateRealtimeBuyerFunding(ctx, tx, auth.BuyerID, money.BuyerDebitNanos); err != nil {
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
	settlementID := uuid.New()
	finalityStatus, finalityBlockers := realtimeKnownCostFinality()
	liabilityAuth := liabilityAuthority{
		PricingDecisionSHA256: pricingSHA256,
		LaneSettlementID:      settlementID.String(),
	}
	if err := liabilityAuth.validate(); err != nil {
		return RealtimeContract{}, RealtimeSettlement{}, fmt.Errorf("exact reuse liability authority: %w", err)
	}
	for _, entry := range entries {
		contractRef := contractID
		li := ledgerInsert{
			Kind:                entry.kind,
			BuyerID:             entry.buyer,
			ExecutionContractID: &contractRef,
			AmountMicros:        entry.amount,
			Currency:            currency.Code(),
			CurrencyAuthority:   ledgerCurrencyAuthorityExecutionContract,
			PayoutStatus:        PayoutReleased,
		}
		if err := applyLiabilityAuthority(&li, liabilityAuth); err != nil {
			return RealtimeContract{}, RealtimeSettlement{}, err
		}
		if _, err := insertLedgerEntryTx(ctx, tx, li); err != nil {
			return RealtimeContract{}, RealtimeSettlement{}, err
		}
	}
	if err := maybeDebitPrepaidForRealtimeTx(ctx, tx, auth.BuyerID, contractID, money.BuyerDebitMicros); err != nil {
		return RealtimeContract{}, RealtimeSettlement{}, err
	}

	blockersJSON, err := json.Marshal(finalityBlockers)
	if err != nil {
		return RealtimeContract{}, RealtimeSettlement{}, err
	}
	if _, err := tx.Exec(ctx, `
		INSERT INTO realtime_settlements
		 (id,contract_id,authoritative_execution_id,receipt_id,buyer_charge_usd,
		  supplier_gross_usd,platform_margin_usd,verification_cost_usd,
		  currency,buyer_charge_nanos,supplier_gross_nanos,known_cost_contribution_nanos,
		  pricing_decision_sha256,finality_status,finality_blockers)
		VALUES ($1,$2,$3,$4,$5,0,$6,0,$7,$8,0,$8,$9,$10,$11::jsonb)`,
		settlementID, contractID, executionID, "rcp_"+executionID.String(),
		buyerCharge, platformMargin, currency.Code(), money.BuyerDebitNanos,
		pricingSHA256, finalityStatus, blockersJSON); err != nil {
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
		PricingDecisionSHA256:      pricingSHA256,
		FinalityStatus:             finalityStatus,
		FinalityBlockers:           finalityBlockers,
		EconomicFinal:              economicFinalityReportsFinal(finalityStatus, finalityBlockers),
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
		var blockersJSON []byte
		err := tx.QueryRow(ctx, `
			SELECT id,buyer_charge_usd::float8,supplier_gross_usd::float8,platform_margin_usd::float8,
			       currency,buyer_charge_nanos,supplier_gross_nanos,known_cost_contribution_nanos,
			       COALESCE(pricing_decision_sha256,''),COALESCE(finality_status,''),finality_blockers
			  FROM realtime_settlements
			 WHERE contract_id=$1 AND authoritative_execution_id=$2`, contractID, evidence.ID).
			Scan(&settlement.ID, &settlement.BuyerChargeUSD, &settlement.SupplierPayableUSD,
				&settlement.PlatformMarginUSD, &settlement.Currency, &settlement.BuyerChargeNanos,
				&settlement.SupplierPayableNanos, &settlement.KnownCostContributionNanos,
				&settlement.PricingDecisionSHA256, &settlement.FinalityStatus, &blockersJSON)
		if errors.Is(err, pgx.ErrNoRows) {
			return RealtimeSettlement{}, errRealtimeAlreadyFinalized
		}
		if err != nil {
			return RealtimeSettlement{}, err
		}
		if len(blockersJSON) > 0 {
			_ = json.Unmarshal(blockersJSON, &settlement.FinalityBlockers)
		}
		// Historical rows predate finality columns: surface the honest known-cost label.
		if settlement.FinalityStatus == "" {
			status, blockers := realtimeKnownCostFinality()
			settlement.FinalityStatus = status
			settlement.FinalityBlockers = blockers
		}
		settlement.EconomicFinal = economicFinalityReportsFinal(
			settlement.FinalityStatus, settlement.FinalityBlockers)
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
	if authority.Currency != currency.Code() {
		return RealtimeSettlement{}, errors.New("realtime settlement currency disagrees with frozen pricing authority")
	}
	buyerExact, supplierExact, err := realtimePhysicalMoneyFromAuthority(
		*authority, evidence.PromptTokens, evidence.CompletionTokens)
	if err != nil || buyerExact.Nanos <= 0 {
		return RealtimeSettlement{}, fmt.Errorf("derive buyer token charge: %w", err)
	}
	if buyerExact.Nanos > contract.Pricing.FixedPoint.AcceptedCeilingNanos {
		return RealtimeSettlement{}, fmt.Errorf("verified usage cost %d nanos exceeds contract maximum %d nanos",
			buyerExact.Nanos, contract.Pricing.FixedPoint.AcceptedCeilingNanos)
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
	settlementID := uuid.New()
	finalityStatus, finalityBlockers := realtimeKnownCostFinality()
	liabilityAuth := liabilityAuthority{
		PricingDecisionSHA256: contract.PricingDecisionSHA256,
		LaneSettlementID:      settlementID.String(),
	}
	if err := liabilityAuth.validate(); err != nil {
		return RealtimeSettlement{}, fmt.Errorf("realtime settle liability authority: %w", err)
	}
	for _, entry := range entries {
		var releaseAt *time.Time
		if t, ok := entry.release.(time.Time); ok {
			releaseAt = &t
		}
		contractRef := contractID
		li := ledgerInsert{
			Kind:                entry.kind,
			SupplierID:          entry.supplier,
			BuyerID:             entry.buyer,
			ExecutionContractID: &contractRef,
			AmountMicros:        entry.amountMicros,
			Currency:            contract.Currency,
			CurrencyAuthority:   ledgerCurrencyAuthorityExecutionContract,
			PayoutStatus:        entry.status,
			ReleaseAt:           releaseAt,
		}
		if err := applyLiabilityAuthority(&li, liabilityAuth); err != nil {
			return RealtimeSettlement{}, err
		}
		if _, err := insertLedgerEntryTx(ctx, tx, li); err != nil {
			return RealtimeSettlement{}, err
		}
	}
	// Debit materialised prepaid for the settled charge when free credit alone
	// does not cover it. Free-credit sandbox charges remain ledger-only.
	if err := maybeDebitPrepaidForRealtimeTx(ctx, tx, contract.BuyerID, contractID, buyerMicros); err != nil {
		return RealtimeSettlement{}, err
	}
	// Envelope-funded contracts convert the RESERVED hold into captured spend
	// for the exact buyer charge; the unused reservation returns to the envelope.
	if err := captureEnvelopeSpendTx(ctx, tx, contractID, buyerExact.Nanos,
		evidence.PromptTokens+evidence.CompletionTokens); err != nil {
		return RealtimeSettlement{}, err
	}
	blockersJSON, err := json.Marshal(finalityBlockers)
	if err != nil {
		return RealtimeSettlement{}, err
	}
	if _, err := tx.Exec(ctx, `
		INSERT INTO realtime_settlements
		 (id,contract_id,authoritative_execution_id,receipt_id,buyer_charge_usd,
		  supplier_gross_usd,platform_margin_usd,verification_cost_usd,
		  currency,buyer_charge_nanos,supplier_gross_nanos,known_cost_contribution_nanos,
		  pricing_decision_sha256,finality_status,finality_blockers)
		VALUES ($1,$2,$3,$4,$5,$6,$7,0,$8,$9,$10,$11,$12,$13,$14::jsonb)`, settlementID, contractID, evidence.ID,
		"rcp_"+evidence.ID.String(), buyerCharge, supplierPayable, platformMargin,
		contract.Currency, buyerExact.Nanos, supplierExact.Nanos, contributionExact.Nanos,
		contract.PricingDecisionSHA256, finalityStatus, blockersJSON); err != nil {
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
		KnownCostContributionNanos: contributionExact.Nanos,
		PricingDecisionSHA256:      contract.PricingDecisionSHA256,
		FinalityStatus:             finalityStatus,
		FinalityBlockers:           finalityBlockers,
		EconomicFinal:              economicFinalityReportsFinal(finalityStatus, finalityBlockers),
	}, nil
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
	if err := releaseEnvelopeSpendForContractTx(ctx, tx, contractID); err != nil {
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
		if err := releaseEnvelopeSpendForContractTx(ctx, tx, item.id); err != nil {
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
	Version                int       `json:"version"`
	RefundID               string    `json:"refund_id"`
	ContractID             string    `json:"contract_id"`
	SettlementID           string    `json:"settlement_id"`
	RefundMode             string    `json:"refund_mode"`
	ReasonCode             string    `json:"reason_code"`
	Reason                 string    `json:"reason"`
	CorrelationRef         string    `json:"correlation_ref"`
	Currency               string    `json:"currency,omitempty"`
	BuyerRefundAmount      float64   `json:"buyer_refund_amount,omitempty"`
	SupplierClawbackAmount float64   `json:"supplier_clawback_amount,omitempty"`
	PlatformRefundAmount   float64   `json:"platform_refund_amount,omitempty"`
	BuyerRefundUSD         float64   `json:"buyer_refund_usd,omitempty"`
	SupplierClawbackUSD    float64   `json:"supplier_clawback_usd,omitempty"`
	PlatformRefundUSD      float64   `json:"platform_refund_usd,omitempty"`
	InternalCreditState    string    `json:"internal_credit_state"`
	ExternalCashState      string    `json:"external_cash_state"`
	SupplierPayoutState    string    `json:"supplier_payout_state"`
	CreatedAt              time.Time `json:"created_at"`
}

func realtimeRefundTx(ctx context.Context, tx pgx.Tx, contractID uuid.UUID) (RealtimeRefund, error) {
	var out RealtimeRefund
	var refundID, settlementID uuid.UUID
	err := tx.QueryRow(ctx, `
		SELECT r.id,r.contract_id::text,s.id,r.refund_mode,r.reason_code,r.reason,
		       r.correlation_ref,r.buyer_refund_usd::float8,
		       r.supplier_clawback_usd::float8,r.platform_refund_usd::float8,
		       r.internal_credit_state,r.external_cash_state,le.payout_status,r.created_at,
		       COALESCE(s.currency,'')
		  FROM realtime_refunds r
		  JOIN realtime_settlements s ON s.contract_id=r.contract_id
		  JOIN ledger_entries le ON le.execution_contract_id=r.contract_id
		                         AND le.kind='supplier_credit'
		 WHERE r.contract_id=$1`, contractID).Scan(
		&refundID, &out.ContractID, &settlementID, &out.RefundMode, &out.ReasonCode,
		&out.Reason, &out.CorrelationRef, &out.BuyerRefundUSD, &out.SupplierClawbackUSD,
		&out.PlatformRefundUSD, &out.InternalCreditState, &out.ExternalCashState,
		&out.SupplierPayoutState, &out.CreatedAt, &out.Currency)
	if err != nil {
		return RealtimeRefund{}, err
	}
	out.RefundID = "rfd_" + refundID.String()
	out.SettlementID = "set_" + settlementID.String()
	if out.Currency == "" {
		out.Version = 1
	} else {
		out.Version = 2
		out.BuyerRefundAmount = out.BuyerRefundUSD
		out.SupplierClawbackAmount = out.SupplierClawbackUSD
		out.PlatformRefundAmount = out.PlatformRefundUSD
		if out.Currency != "usd" {
			out.BuyerRefundUSD = 0
			out.SupplierClawbackUSD = 0
			out.PlatformRefundUSD = 0
		}
	}
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
		state, payoutStatus, currency              string
		buyerID, supplierID, settlementID, entryID uuid.UUID
		buyerCharge, supplierGross, platformMargin float64
		fundingBound                               bool
	)
	var pricingSHA string
	err = tx.QueryRow(ctx, `
		SELECT c.state,c.buyer_id,c.supplier_id,s.id,s.buyer_charge_usd::float8,
		       s.supplier_gross_usd::float8,s.platform_margin_usd::float8,
		       le.id,le.payout_status,c.currency,
		       EXISTS(SELECT 1 FROM supplier_payout_funding f WHERE f.ledger_entry_id=le.id),
		       COALESCE(c.pricing_decision_sha256,'')
		  FROM execution_contracts c
		  JOIN realtime_settlements s ON s.contract_id=c.id
		  JOIN ledger_entries le ON le.execution_contract_id=c.id AND le.kind='supplier_credit'
		 WHERE c.id=$1
		 FOR UPDATE OF c,le`, contractID).Scan(
		&state, &buyerID, &supplierID, &settlementID, &buyerCharge,
		&supplierGross, &platformMargin, &entryID, &payoutStatus, &currency, &fundingBound,
		&pricingSHA)
	if errors.Is(err, pgx.ErrNoRows) {
		return RealtimeRefund{}, false, errNotFound
	}
	if err != nil {
		return RealtimeRefund{}, false, err
	}
	if _, err := ParseCurrency(currency); err != nil {
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
	// Amounts stay exact copies of the settlement row; citation only.
	refundAuth := liabilityAuthority{
		PricingDecisionSHA256: pricingSHA,
		LaneSettlementID:      settlementID.String(),
		LifecycleRevision:     liabilityLifecycleRevision,
	}
	if err := refundAuth.validate(); err != nil {
		return RealtimeRefund{}, false, fmt.Errorf("realtime refund liability authority: %w", err)
	}
	reversals := []ledgerInsert{
		{Kind: "buyer_refund", BuyerID: &buyerID, ExecutionContractID: &contract,
			AmountMicros: usdToMicros(refundBuyerUSD), Currency: currency,
			CurrencyAuthority: ledgerCurrencyAuthorityExecutionContract, PayoutStatus: "released"},
		{Kind: "clawback", SupplierID: &supplierID, ExecutionContractID: &contract,
			AmountMicros: -usdToMicros(refundSupplierUSD), Currency: currency,
			CurrencyAuthority: ledgerCurrencyAuthorityExecutionContract, PayoutStatus: "clawed_back"},
		{Kind: "platform_refund", ExecutionContractID: &contract,
			AmountMicros: -usdToMicros(refundPlatformUSD), Currency: currency,
			CurrencyAuthority: ledgerCurrencyAuthorityExecutionContract, PayoutStatus: "released"},
	}
	for i := range reversals {
		if err := applyLiabilityAuthority(&reversals[i], refundAuth); err != nil {
			return RealtimeRefund{}, false, err
		}
		if _, err := insertLedgerEntryTx(ctx, tx, reversals[i]); err != nil {
			return RealtimeRefund{}, false, err
		}
	}
	// buyer_refund zeros spent in evaluateRealtimeBuyerFunding while any
	// prepaid_debit for this contract still reduces balance_micros and still
	// contributes to +prepaidDebited. Restore materialisation (and a matching
	// prepaid_restore ledger row) so capacity matches cash again. No-op when
	// settle was free-credit-only. Idempotent on restore payout_ref.
	if err := restorePrepaidForExecutionContractRefundTx(ctx, tx, buyerID, contractID, currency); err != nil {
		return RealtimeRefund{}, false, err
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
		      AND currency='usd'
		      AND payout_status IN ('held','awaiting_funding','ready','sending','outcome_unknown')),0)::float8,
		  COALESCE((SELECT sum(amount_usd) FROM ledger_entries
		    WHERE execution_contract_id IS NOT NULL AND kind='supplier_credit'
		      AND currency='usd'
		      AND payout_status='reversal_required'),0)::float8,
		  COALESCE((SELECT sum(r.buyer_refund_usd)
		              FROM realtime_refunds r
		              JOIN realtime_settlements s ON s.contract_id=r.contract_id
		             WHERE COALESCE(s.currency,'usd')='usd'),0)::float8
	`).Scan(&snapshot.ExecutingContracts, &snapshot.OldestExecutingAgeSeconds,
		&snapshot.ActiveOffers, &snapshot.AvailableSequences, &snapshot.OpenSupplierPayableUSD,
		&snapshot.ReversalRequiredUSD, &snapshot.InternalRefundsUSD)
	if err != nil {
		return snapshot, err
	}
	rows, err := s.pool.Query(ctx, `
		WITH currencies AS (
			SELECT currency FROM ledger_entries
			 WHERE execution_contract_id IS NOT NULL AND currency IS NOT NULL
			UNION
			SELECT COALESCE(s.currency,'usd')
			  FROM realtime_refunds r
			  JOIN realtime_settlements s ON s.contract_id=r.contract_id
		)
		SELECT c.currency,
		       COALESCE((SELECT sum(le.amount_usd) FROM ledger_entries le
		                  WHERE le.execution_contract_id IS NOT NULL
		                    AND le.kind='supplier_credit' AND le.currency=c.currency
		                    AND le.payout_status IN
		                      ('held','awaiting_funding','ready','sending','outcome_unknown')),0)::float8,
		       COALESCE((SELECT sum(le.amount_usd) FROM ledger_entries le
		                  WHERE le.execution_contract_id IS NOT NULL
		                    AND le.kind='supplier_credit' AND le.currency=c.currency
		                    AND le.payout_status='reversal_required'),0)::float8,
		       COALESCE((SELECT sum(r.buyer_refund_usd)
		                   FROM realtime_refunds r
		                   JOIN realtime_settlements s ON s.contract_id=r.contract_id
		                  WHERE COALESCE(s.currency,'usd')=c.currency),0)::float8
		  FROM currencies c ORDER BY c.currency`)
	if err != nil {
		return snapshot, err
	}
	defer rows.Close()
	for rows.Next() {
		var item RealtimeOperationalCurrencySnapshot
		if err := rows.Scan(&item.Currency, &item.OpenSupplierPayable,
			&item.ReversalRequired, &item.InternalRefunds); err != nil {
			return snapshot, err
		}
		snapshot.MoneyByCurrency = append(snapshot.MoneyByCurrency, item)
	}
	return snapshot, rows.Err()
}

type RealtimeReceipt struct {
	Version                int                            `json:"version"`
	ReceiptID              string                         `json:"receipt_id"`
	SettlementID           string                         `json:"settlement_id,omitempty"`
	RefundID               string                         `json:"refund_id,omitempty"`
	ContractID             string                         `json:"contract_id"`
	RequestID              string                         `json:"request_id"`
	State                  string                         `json:"state"`
	Model                  string                         `json:"model"`
	RuntimeProfileID       string                         `json:"runtime_profile_id"`
	RuntimeProfileSHA256   string                         `json:"runtime_profile_sha256"`
	PlacementPlan          *RealtimePlacementPlan         `json:"placement_plan,omitempty"`
	PlacementPlanSHA256    string                         `json:"placement_plan_sha256,omitempty"`
	MarketClearing         *RealtimeMarketClearingReceipt `json:"market_clearing,omitempty"`
	PricingDecision        *PricingDecision               `json:"pricing_decision,omitempty"`
	PricingDecisionSHA256  string                         `json:"pricing_decision_sha256,omitempty"`
	PricingAuthorityStatus string                         `json:"pricing_authority_status"`
	Coalescing             *RealtimeCoalescingReceipt     `json:"coalescing,omitempty"`
	InputCommitment        string                         `json:"input_commitment"`
	StreamRootSHA256       string                         `json:"stream_root_sha256,omitempty"`
	OutputCommitment       string                         `json:"output_commitment,omitempty"`
	PromptTokens           int64                          `json:"prompt_tokens,omitempty"`
	CompletionTokens       int64                          `json:"completion_tokens,omitempty"`
	TotalTokens            int64                          `json:"total_tokens,omitempty"`
	TimeToFirstEventMS     int64                          `json:"time_to_first_event_ms,omitempty"`
	DurationMS             int64                          `json:"duration_ms,omitempty"`
	Verification           string                         `json:"verification"`
	AuthorizationState     string                         `json:"authorization_state"`
	AuthorizedAmount       float64                        `json:"authorized_amount,omitempty"`
	CapturedAmount         float64                        `json:"captured_amount,omitempty"`
	ReleasedAmount         float64                        `json:"released_amount,omitempty"`
	VoidedAmount           float64                        `json:"voided_amount,omitempty"`
	BuyerChargeAmount      float64                        `json:"buyer_charge_amount,omitempty"`
	SupplierPayableAmount  float64                        `json:"supplier_payable_amount,omitempty"`
	AuthorizedUSD          float64                        `json:"authorized_usd,omitempty"`
	CapturedUSD            float64                        `json:"captured_usd,omitempty"`
	ReleasedUSD            float64                        `json:"released_usd,omitempty"`
	VoidedUSD              float64                        `json:"voided_usd,omitempty"`
	BuyerChargeUSD         float64                        `json:"buyer_charge_usd,omitempty"`
	SupplierPayableUSD     float64                        `json:"supplier_payable_usd,omitempty"`
	// PlatformGrossSpreadUSD is the gross platform_take ledger sum: buyer charge
	// less supplier entitlement, BEFORE any Merc cost. It is not margin and it is
	// not profit. The batch path renamed the identical row for this reason (see
	// store_billing.go); this one kept the misleading name far longer. The honest
	// decomposition is in the embedded pricing_decision, which carries a per-cost
	// status and refuses a true net while any category is unknown.
	PlatformGrossSpreadAmount float64 `json:"platform_gross_spread_amount,omitempty"`
	RefundAmount              float64 `json:"refund_amount,omitempty"`
	SupplierClawbackAmount    float64 `json:"supplier_clawback_amount,omitempty"`
	PlatformRefundAmount      float64 `json:"platform_refund_amount,omitempty"`
	NetBuyerChargeAmount      float64 `json:"net_buyer_charge_amount,omitempty"`
	NetSupplierPayableAmount  float64 `json:"net_supplier_payable_amount,omitempty"`
	PlatformGrossSpreadUSD    float64 `json:"platform_gross_spread_usd,omitempty"`
	RefundUSD                 float64 `json:"refund_usd,omitempty"`
	SupplierClawbackUSD       float64 `json:"supplier_clawback_usd,omitempty"`
	PlatformRefundUSD         float64 `json:"platform_refund_usd,omitempty"`
	NetBuyerChargeUSD         float64 `json:"net_buyer_charge_usd,omitempty"`
	NetSupplierPayableUSD     float64 `json:"net_supplier_payable_usd,omitempty"`
	// NetPlatformGrossSpreadUSD is the same gross row less buyer refunds. "Net"
	// here means net of refunds only -- never net of Merc's costs.
	NetPlatformGrossSpreadAmount float64 `json:"net_platform_gross_spread_amount,omitempty"`
	NetPlatformGrossSpreadUSD    float64 `json:"net_platform_gross_spread_usd,omitempty"`
	SettlementCurrency           string  `json:"settlement_currency,omitempty"`
	BuyerChargeNanos             int64   `json:"buyer_charge_nanos,omitempty"`
	SupplierPayableNanos         int64   `json:"supplier_payable_nanos,omitempty"`
	KnownCostContributionNanos   int64   `json:"known_cost_contribution_nanos,omitempty"`
	// FinalityStatus / FinalityBlockers / EconomicFinal make realtime terminal
	// money explicit. Known-cost settle is not batch ECONOMIC_FINAL / true-net.
	// Absence of these fields is not "final by silence" — loaders fill the
	// honest known-cost label when a settlement exists without stored columns.
	FinalityStatus       string     `json:"finality_status,omitempty"`
	FinalityBlockers     []string   `json:"finality_blockers,omitempty"`
	EconomicFinal        bool       `json:"economic_final"`
	SupplierPayoutState  string     `json:"supplier_payout_state"`
	SupplierLedgerState  string     `json:"supplier_ledger_state,omitempty"`
	RefundMode           string     `json:"refund_mode,omitempty"`
	RefundReasonCode     string     `json:"refund_reason_code,omitempty"`
	RefundReason         string     `json:"refund_reason,omitempty"`
	RefundCorrelationRef string     `json:"refund_correlation_ref,omitempty"`
	InternalCreditState  string     `json:"internal_credit_state,omitempty"`
	ExternalCashState    string     `json:"external_cash_state,omitempty"`
	FailureCode          string     `json:"failure_code,omitempty"`
	CreatedAt            time.Time  `json:"created_at"`
	FinalizedAt          *time.Time `json:"finalized_at,omitempty"`
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
	var finalityStatus string
	var finalityBlockersJSON []byte
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
		       COALESCE(r.external_cash_state,''),
		       CASE WHEN c.pricing_decision IS NULL AND s.currency IS NULL
		            THEN '' ELSE c.currency END,
		       COALESCE(s.buyer_charge_nanos,0),COALESCE(s.supplier_gross_nanos,0),
		       COALESCE(s.known_cost_contribution_nanos,0),
		       COALESCE(s.finality_status,''),s.finality_blockers
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
		&receipt.SupplierPayableUSD, &receipt.PlatformGrossSpreadUSD, &receipt.RefundUSD,
		&receipt.SupplierClawbackUSD, &receipt.PlatformRefundUSD, &receipt.AuthorizedUSD,
		&receipt.CapturedUSD, &receipt.ReleasedUSD, &receipt.VoidedUSD,
		&receipt.SupplierLedgerState, &receipt.RefundMode, &receipt.RefundReasonCode,
		&receipt.RefundReason, &receipt.RefundCorrelationRef, &receipt.InternalCreditState,
		&receipt.ExternalCashState, &receipt.SettlementCurrency, &receipt.BuyerChargeNanos,
		&receipt.SupplierPayableNanos, &receipt.KnownCostContributionNanos,
		&finalityStatus, &finalityBlockersJSON)
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
		// Surface finality explicitly on the buyer receipt. A settlement without
		// stored finality columns (historical) still gets the honest known-cost
		// label so finality is never inferred from field absence.
		receipt.FinalityStatus = finalityStatus
		if len(finalityBlockersJSON) > 0 {
			_ = json.Unmarshal(finalityBlockersJSON, &receipt.FinalityBlockers)
		}
		if receipt.FinalityStatus == "" {
			status, blockers := realtimeKnownCostFinality()
			receipt.FinalityStatus = status
			receipt.FinalityBlockers = blockers
		}
		receipt.EconomicFinal = economicFinalityReportsFinal(
			receipt.FinalityStatus, receipt.FinalityBlockers)
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
	receipt.NetPlatformGrossSpreadUSD = roundRealtimeUSD(receipt.PlatformGrossSpreadUSD - receipt.PlatformRefundUSD)
	if receipt.SettlementCurrency == "" {
		receipt.Version = 1
	} else {
		receipt.Version = 2
		receipt.AuthorizedAmount = receipt.AuthorizedUSD
		receipt.CapturedAmount = receipt.CapturedUSD
		receipt.ReleasedAmount = receipt.ReleasedUSD
		receipt.VoidedAmount = receipt.VoidedUSD
		receipt.BuyerChargeAmount = receipt.BuyerChargeUSD
		receipt.SupplierPayableAmount = receipt.SupplierPayableUSD
		receipt.PlatformGrossSpreadAmount = receipt.PlatformGrossSpreadUSD
		receipt.RefundAmount = receipt.RefundUSD
		receipt.SupplierClawbackAmount = receipt.SupplierClawbackUSD
		receipt.PlatformRefundAmount = receipt.PlatformRefundUSD
		receipt.NetBuyerChargeAmount = receipt.NetBuyerChargeUSD
		receipt.NetSupplierPayableAmount = receipt.NetSupplierPayableUSD
		receipt.NetPlatformGrossSpreadAmount = receipt.NetPlatformGrossSpreadUSD
		if receipt.SettlementCurrency != "usd" {
			receipt.AuthorizedUSD = 0
			receipt.CapturedUSD = 0
			receipt.ReleasedUSD = 0
			receipt.VoidedUSD = 0
			receipt.BuyerChargeUSD = 0
			receipt.SupplierPayableUSD = 0
			receipt.PlatformGrossSpreadUSD = 0
			receipt.RefundUSD = 0
			receipt.SupplierClawbackUSD = 0
			receipt.PlatformRefundUSD = 0
			receipt.NetBuyerChargeUSD = 0
			receipt.NetSupplierPayableUSD = 0
			receipt.NetPlatformGrossSpreadUSD = 0
		}
	}
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
