package main

import (
	"context"
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
	errRealtimeNotRefundable       = errors.New("realtime settlement is not eligible for an internal refund")
	errRealtimeRefundNeedsReversal = errors.New("supplier payout crossed the internal transfer boundary; external reversal is required")
)

type RealtimeOfferRegistration struct {
	RuntimeProfileID                  string  `json:"runtime_profile_id"`
	RuntimeProfileSHA256              string  `json:"runtime_profile_sha256"`
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
}

type RealtimeContractAuthorization struct {
	RequestID         string
	BuyerID           uuid.UUID
	Profile           VLLMRuntimeProfile
	InputCommitment   string
	RequestSHA256     string
	MaximumPriceUSD   float64
	EstimatedPriceUSD float64
	DeadlineAt        time.Time
	IdempotencyKey    string
}

func (s *Store) UpsertRealtimeOffer(ctx context.Context, worker WorkerAuth, registration RealtimeOfferRegistration) error {
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
		   upstream_base_url,upstream_token_sealed,warmth,max_active_sequences,
		   available_sequences,supplier_input_usd_per_million_tokens,
		   supplier_output_usd_per_million_tokens,status,last_seen_at,updated_at)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,'ACTIVE',now(),now())
		ON CONFLICT (worker_id,runtime_profile_id) DO UPDATE SET
		  supplier_id=EXCLUDED.supplier_id,
		  runtime_profile_sha256=EXCLUDED.runtime_profile_sha256,
		  upstream_base_url=EXCLUDED.upstream_base_url,
		  upstream_token_sealed=EXCLUDED.upstream_token_sealed,
		  warmth=EXCLUDED.warmth,
		  max_active_sequences=EXCLUDED.max_active_sequences,
		  available_sequences=EXCLUDED.available_sequences,
		  supplier_input_usd_per_million_tokens=EXCLUDED.supplier_input_usd_per_million_tokens,
		  supplier_output_usd_per_million_tokens=EXCLUDED.supplier_output_usd_per_million_tokens,
		  status='ACTIVE',last_seen_at=now(),updated_at=now()`,
		worker.WorkerID, worker.SupplierID, registration.RuntimeProfileID,
		registration.RuntimeProfileSHA256, registration.UpstreamBaseURL, sealed,
		registration.Warmth, registration.MaxActiveSequences, registration.AvailableSequences,
		registration.SupplierInputUSDPerMillionTokens, registration.SupplierOutputUSDPerMillionTokens)
	if err != nil {
		return err
	}
	return tx.Commit(ctx)
}

func (s *Store) HeartbeatRealtimeOffer(ctx context.Context, worker WorkerAuth, hb RealtimeOfferHeartbeat) error {
	tag, err := s.pool.Exec(ctx, `
		UPDATE realtime_worker_offers
		   SET warmth=$4,
		       available_sequences=GREATEST(0,LEAST($5,max_active_sequences-(
		           SELECT count(*)::int FROM execution_contracts c
		            WHERE c.worker_id=$1 AND c.runtime_profile_id=$3
		              AND c.state='EXECUTING'))),
		       status=$6,last_seen_at=now(),updated_at=now()
		 WHERE worker_id=$1 AND supplier_id=$2 AND runtime_profile_id=$3
		   AND $5 BETWEEN 0 AND max_active_sequences`,
		worker.WorkerID, worker.SupplierID, hb.RuntimeProfileID, hb.Warmth,
		hb.AvailableSequences, hb.Status)
	if err != nil {
		return err
	}
	if tag.RowsAffected() != 1 {
		return errNotFound
	}
	return nil
}

func scanRealtimeContract(row pgx.Row) (RealtimeContract, error) {
	var contract RealtimeContract
	var sealed string
	err := row.Scan(
		&contract.ID, &contract.RequestID, &contract.BuyerID, &contract.ModelAlias,
		&contract.RuntimeProfileID, &contract.RuntimeProfileSHA256,
		&contract.MaximumPriceUSD, &contract.EstimatedPriceUSD,
		&contract.BuyerInputUSDPerMillionTokens, &contract.BuyerOutputUSDPerMillionTokens,
		&contract.SupplierInputUSDPerMillionTokens, &contract.SupplierOutputUSDPerMillionTokens,
		&contract.DeadlineAt, &contract.State, &contract.WorkerID, &contract.SupplierID,
		&contract.UpstreamBaseURL, &sealed)
	if err != nil {
		return RealtimeContract{}, err
	}
	contract.UpstreamToken = openToken(sealed)
	if contract.UpstreamToken == "" {
		return RealtimeContract{}, errors.New("vLLM upstream credential cannot be opened")
	}
	return contract, nil
}

const realtimeContractColumns = `
	 id,request_id,buyer_id,model_alias,runtime_profile_id,runtime_profile_sha256,
	 maximum_price_usd::float8,estimated_price_usd::float8,
	 buyer_input_usd_per_million_tokens::float8,buyer_output_usd_per_million_tokens::float8,
	 supplier_input_usd_per_million_tokens::float8,supplier_output_usd_per_million_tokens::float8,
	 deadline_at,state,worker_id,supplier_id,upstream_base_url,upstream_token_sealed`

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
		row := tx.QueryRow(ctx, `SELECT `+realtimeContractColumns+`,request_sha256
			FROM execution_contracts WHERE buyer_id=$1 AND idempotency_key=$2`,
			auth.BuyerID, auth.IdempotencyKey)
		var requestSHA string
		var contract RealtimeContract
		var sealed string
		err := row.Scan(
			&contract.ID, &contract.RequestID, &contract.BuyerID, &contract.ModelAlias,
			&contract.RuntimeProfileID, &contract.RuntimeProfileSHA256,
			&contract.MaximumPriceUSD, &contract.EstimatedPriceUSD,
			&contract.BuyerInputUSDPerMillionTokens, &contract.BuyerOutputUSDPerMillionTokens,
			&contract.SupplierInputUSDPerMillionTokens, &contract.SupplierOutputUSDPerMillionTokens,
			&contract.DeadlineAt, &contract.State, &contract.WorkerID, &contract.SupplierID,
			&contract.UpstreamBaseURL, &sealed, &requestSHA)
		if err == nil {
			if requestSHA != auth.RequestSHA256 {
				return RealtimeContract{}, false, errRealtimeIdempotencyConflict
			}
			contract.UpstreamToken = openToken(sealed)
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
	// requests cannot each spend the same sandbox credit. A saved payment method
	// is authority only while the Stripe rail itself is configured.
	{
		var freeCredit, spent, batchReserved, realtimeReserved float64
		var hasPaymentMethod bool
		err := tx.QueryRow(ctx, `
			SELECT b.free_credit_usd::float8,
			       EXISTS(SELECT 1 FROM billing_customers bc
			               WHERE bc.buyer_id=b.id AND COALESCE(bc.default_payment_method,'')<>''),
			       COALESCE((SELECT -sum(le.amount_usd) FROM ledger_entries le
			                 WHERE le.buyer_id=b.id
			                   AND le.kind IN ('buyer_charge','buyer_refund')),0)::float8,
			       COALESCE((SELECT sum(j.estimated_usd) FROM jobs j
			                 WHERE j.buyer_id=b.id AND j.status IN ('queued','running','verifying')),0)::float8,
			       COALESCE((SELECT sum(c.maximum_price_usd) FROM execution_contracts c
			                 WHERE c.buyer_id=b.id AND c.state='EXECUTING'),0)::float8
			  FROM buyers b WHERE b.id=$1 AND b.deleted_at IS NULL FOR UPDATE`, auth.BuyerID).
			Scan(&freeCredit, &hasPaymentMethod, &spent, &batchReserved, &realtimeReserved)
		if errors.Is(err, pgx.ErrNoRows) {
			return RealtimeContract{}, false, errNotFound
		}
		if err != nil {
			return RealtimeContract{}, false, err
		}
		providerFunded := stripeKey() != "" && hasPaymentMethod
		if !providerFunded && freeCredit-spent-batchReserved-realtimeReserved < auth.MaximumPriceUSD {
			return RealtimeContract{}, false, errRealtimeInsufficientFunds
		}
	}

	var (
		workerID, supplierID uuid.UUID
		baseURL, sealed      string
		supplierInput        float64
		supplierOutput       float64
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
	err = tx.QueryRow(ctx, `
		UPDATE realtime_worker_offers o
		   SET available_sequences = o.available_sequences - 1, updated_at = now()
		 WHERE (o.worker_id, o.runtime_profile_id) = (
		       SELECT c.worker_id, c.runtime_profile_id
		         FROM realtime_worker_offers c
		         JOIN suppliers s ON s.id = c.supplier_id
		        WHERE c.runtime_profile_id=$1 AND c.runtime_profile_sha256=$2
		          AND c.status='ACTIVE' AND c.available_sequences > 0
		          AND c.last_seen_at > now()-interval '45 seconds'
		          AND s.status='active' AND s.quarantined_at IS NULL
		          AND c.supplier_input_usd_per_million_tokens <= $3
		          AND c.supplier_output_usd_per_million_tokens <= $4
		        ORDER BY CASE c.warmth WHEN 'HOT' THEN 0 WHEN 'WARM' THEN 1 WHEN 'CACHED' THEN 2 ELSE 3 END,
		                 (c.supplier_input_usd_per_million_tokens+c.supplier_output_usd_per_million_tokens),
		                 c.available_sequences DESC, c.last_seen_at DESC
		        LIMIT 1)
		   AND o.available_sequences > 0
	 RETURNING o.worker_id,o.supplier_id,o.upstream_base_url,o.upstream_token_sealed,
		           o.supplier_input_usd_per_million_tokens::float8,
		           o.supplier_output_usd_per_million_tokens::float8`,
		auth.Profile.RuntimeProfileID, auth.Profile.ProfileSHA256,
		auth.Profile.BuyerInputUSDPerMillionTokens, auth.Profile.BuyerOutputUSDPerMillionTokens).
		Scan(&workerID, &supplierID, &baseURL, &sealed, &supplierInput, &supplierOutput)
	if errors.Is(err, pgx.ErrNoRows) {
		return RealtimeContract{}, false, errRealtimeNoSupply
	}
	if err != nil {
		return RealtimeContract{}, false, err
	}

	contractID := uuid.New()
	_, err = tx.Exec(ctx, `
		INSERT INTO execution_contracts
		 (id,request_id,buyer_id,workload_type,route,model_alias,runtime_profile_id,
		  runtime_profile_sha256,input_commitment,request_sha256,maximum_price_usd,
		  estimated_price_usd,buyer_input_usd_per_million_tokens,
		  buyer_output_usd_per_million_tokens,supplier_input_usd_per_million_tokens,
		  supplier_output_usd_per_million_tokens,deadline_at,verification_tier,
		  idempotency_key,state,worker_id,supplier_id,upstream_base_url,upstream_token_sealed)
		VALUES ($1,$2,$3,'CHAT_COMPLETION','/v1/chat/completions',$4,$5,$6,$7,$8,
		        $9,$10,$11,$12,$13,$14,$15,'V0',$16,'EXECUTING',$17,$18,$19,$20)`,
		contractID, auth.RequestID, auth.BuyerID, auth.Profile.ModelAlias,
		auth.Profile.RuntimeProfileID, auth.Profile.ProfileSHA256,
		auth.InputCommitment, auth.RequestSHA256, auth.MaximumPriceUSD,
		auth.EstimatedPriceUSD, auth.Profile.BuyerInputUSDPerMillionTokens,
		auth.Profile.BuyerOutputUSDPerMillionTokens, supplierInput, supplierOutput,
		auth.DeadlineAt, realtimeNullIfEmpty(auth.IdempotencyKey), workerID, supplierID, baseURL, sealed)
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
		MaximumPriceUSD:      auth.MaximumPriceUSD, EstimatedPriceUSD: auth.EstimatedPriceUSD,
		BuyerInputUSDPerMillionTokens:     auth.Profile.BuyerInputUSDPerMillionTokens,
		BuyerOutputUSDPerMillionTokens:    auth.Profile.BuyerOutputUSDPerMillionTokens,
		SupplierInputUSDPerMillionTokens:  supplierInput,
		SupplierOutputUSDPerMillionTokens: supplierOutput,
		DeadlineAt:                        auth.DeadlineAt, State: "EXECUTING", WorkerID: workerID,
		SupplierID: supplierID, UpstreamBaseURL: baseURL, UpstreamToken: upstreamToken,
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
	ID                 uuid.UUID `json:"-"`
	BuyerChargeUSD     float64   `json:"buyer_charge_usd"`
	SupplierPayableUSD float64   `json:"supplier_payable_usd"`
	PlatformMarginUSD  float64   `json:"platform_margin_usd"`
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

func tokenCharge(prompt, completion int64, inputRate, outputRate float64) float64 {
	amount := float64(prompt)/1_000_000*inputRate + float64(completion)/1_000_000*outputRate
	amount = math.Round(amount*1_000_000) / 1_000_000
	if amount == 0 && prompt+completion > 0 && (inputRate > 0 || outputRate > 0) {
		amount = 0.000001
	}
	return amount
}

func releaseRealtimeCapacity(ctx context.Context, tx pgx.Tx, workerID uuid.UUID, profileID string) error {
	_, err := tx.Exec(ctx, `
		UPDATE realtime_worker_offers
		   SET available_sequences=LEAST(max_active_sequences,available_sequences+1),updated_at=now()
		 WHERE worker_id=$1 AND runtime_profile_id=$2`, workerID, profileID)
	return err
}

func (s *Store) FinalizeRealtimeSuccess(ctx context.Context, contractID uuid.UUID, evidence RealtimeExecutionEvidence) (RealtimeSettlement, error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return RealtimeSettlement{}, err
	}
	defer tx.Rollback(ctx)
	var (
		state, profileID                 string
		buyerID, workerID, supplierID    uuid.UUID
		maximum, buyerInput, buyerOutput float64
		supplierInput, supplierOutput    float64
	)
	err = tx.QueryRow(ctx, `
		SELECT state,buyer_id,worker_id,supplier_id,runtime_profile_id,
		       maximum_price_usd::float8,buyer_input_usd_per_million_tokens::float8,
		       buyer_output_usd_per_million_tokens::float8,
		       supplier_input_usd_per_million_tokens::float8,
		       supplier_output_usd_per_million_tokens::float8
		  FROM execution_contracts WHERE id=$1 FOR UPDATE`, contractID).
		Scan(&state, &buyerID, &workerID, &supplierID, &profileID, &maximum,
			&buyerInput, &buyerOutput, &supplierInput, &supplierOutput)
	if errors.Is(err, pgx.ErrNoRows) {
		return RealtimeSettlement{}, errNotFound
	}
	if err != nil {
		return RealtimeSettlement{}, err
	}
	if state != "EXECUTING" {
		return RealtimeSettlement{}, errRealtimeAlreadyFinalized
	}
	if evidence.ID == uuid.Nil || evidence.HTTPStatus < 200 || evidence.HTTPStatus > 299 ||
		evidence.StreamRootSHA256 == "" || evidence.OutputCommitment == "" ||
		evidence.PromptTokens < 0 || evidence.CompletionTokens < 0 ||
		evidence.TotalTokens != evidence.PromptTokens+evidence.CompletionTokens {
		return RealtimeSettlement{}, errors.New("incomplete V0 execution evidence")
	}
	buyerCharge := tokenCharge(evidence.PromptTokens, evidence.CompletionTokens, buyerInput, buyerOutput)
	if buyerCharge > maximum {
		return RealtimeSettlement{}, fmt.Errorf("verified usage cost %.6f exceeds contract maximum %.6f", buyerCharge, maximum)
	}
	supplierPayable := tokenCharge(evidence.PromptTokens, evidence.CompletionTokens, supplierInput, supplierOutput)
	if supplierPayable > buyerCharge {
		return RealtimeSettlement{}, errors.New("supplier payable exceeds buyer charge")
	}
	platformMargin := math.Round((buyerCharge-supplierPayable)*1_000_000) / 1_000_000
	_, err = tx.Exec(ctx, `
		INSERT INTO realtime_executions
		 (id,contract_id,worker_id,supplier_id,upstream_request_id,http_status,
		  stream_event_count,stream_root_sha256,output_commitment,prompt_tokens,
		  completion_tokens,total_tokens,time_to_first_event_ms,duration_ms,
		  verification_state)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,'PASSED')`,
		evidence.ID, contractID, workerID, supplierID, realtimeNullIfEmpty(evidence.UpstreamRequestID),
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
		amount          float64
		release         any
	}{
		{KindBuyerCharge, PayoutReleased, &buyerID, nil, -buyerCharge, nil},
		{KindSupplierCredit, PayoutHeld, nil, &supplierID, supplierPayable, releaseAt},
		{KindPlatformTake, PayoutReleased, nil, nil, platformMargin, nil},
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
			AmountMicros:        usdToMicros(entry.amount),
			PayoutStatus:        entry.status,
			ReleaseAt:           releaseAt,
		}); err != nil {
			return RealtimeSettlement{}, err
		}
	}
	settlementID := uuid.New()
	if _, err := tx.Exec(ctx, `
		INSERT INTO realtime_settlements
		 (id,contract_id,authoritative_execution_id,receipt_id,buyer_charge_usd,
		  supplier_gross_usd,platform_margin_usd,verification_cost_usd)
		VALUES ($1,$2,$3,$4,$5,$6,$7,0)`, settlementID, contractID, evidence.ID,
		"rcp_"+evidence.ID.String(), buyerCharge, supplierPayable, platformMargin); err != nil {
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
	if err := releaseRealtimeCapacity(ctx, tx, workerID, profileID); err != nil {
		return RealtimeSettlement{}, err
	}
	if err := tx.Commit(ctx); err != nil {
		return RealtimeSettlement{}, err
	}
	return RealtimeSettlement{ID: settlementID, BuyerChargeUSD: buyerCharge, SupplierPayableUSD: supplierPayable, PlatformMarginUSD: platformMargin}, nil
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
	if err := tx.Commit(ctx); err != nil {
		return false, err
	}
	return true, nil
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
	ReceiptID             string     `json:"receipt_id"`
	SettlementID          string     `json:"settlement_id,omitempty"`
	RefundID              string     `json:"refund_id,omitempty"`
	ContractID            string     `json:"contract_id"`
	RequestID             string     `json:"request_id"`
	State                 string     `json:"state"`
	Model                 string     `json:"model"`
	RuntimeProfileID      string     `json:"runtime_profile_id"`
	RuntimeProfileSHA256  string     `json:"runtime_profile_sha256"`
	InputCommitment       string     `json:"input_commitment"`
	StreamRootSHA256      string     `json:"stream_root_sha256,omitempty"`
	OutputCommitment      string     `json:"output_commitment,omitempty"`
	PromptTokens          int64      `json:"prompt_tokens,omitempty"`
	CompletionTokens      int64      `json:"completion_tokens,omitempty"`
	TotalTokens           int64      `json:"total_tokens,omitempty"`
	TimeToFirstEventMS    int64      `json:"time_to_first_event_ms,omitempty"`
	DurationMS            int64      `json:"duration_ms,omitempty"`
	Verification          string     `json:"verification"`
	AuthorizationState    string     `json:"authorization_state"`
	AuthorizedUSD         float64    `json:"authorized_usd"`
	CapturedUSD           float64    `json:"captured_usd"`
	ReleasedUSD           float64    `json:"released_usd"`
	VoidedUSD             float64    `json:"voided_usd"`
	BuyerChargeUSD        float64    `json:"buyer_charge_usd"`
	SupplierPayableUSD    float64    `json:"supplier_payable_usd"`
	PlatformMarginUSD     float64    `json:"platform_margin_usd"`
	RefundUSD             float64    `json:"refund_usd"`
	SupplierClawbackUSD   float64    `json:"supplier_clawback_usd"`
	PlatformRefundUSD     float64    `json:"platform_refund_usd"`
	NetBuyerChargeUSD     float64    `json:"net_buyer_charge_usd"`
	NetSupplierPayableUSD float64    `json:"net_supplier_payable_usd"`
	NetPlatformMarginUSD  float64    `json:"net_platform_margin_usd"`
	SupplierPayoutState   string     `json:"supplier_payout_state"`
	SupplierLedgerState   string     `json:"supplier_ledger_state,omitempty"`
	RefundMode            string     `json:"refund_mode,omitempty"`
	RefundReasonCode      string     `json:"refund_reason_code,omitempty"`
	RefundReason          string     `json:"refund_reason,omitempty"`
	RefundCorrelationRef  string     `json:"refund_correlation_ref,omitempty"`
	InternalCreditState   string     `json:"internal_credit_state,omitempty"`
	ExternalCashState     string     `json:"external_cash_state,omitempty"`
	FailureCode           string     `json:"failure_code,omitempty"`
	CreatedAt             time.Time  `json:"created_at"`
	FinalizedAt           *time.Time `json:"finalized_at,omitempty"`
}

func (s *Store) RealtimeReceipt(ctx context.Context, buyerID, contractID uuid.UUID) (RealtimeReceipt, error) {
	var receipt RealtimeReceipt
	var executionID, settlementID, refundID *uuid.UUID
	var finalized *time.Time
	err := s.pool.QueryRow(ctx, `
		SELECT c.id::text,c.request_id,c.state,c.model_alias,c.runtime_profile_id,
		       c.runtime_profile_sha256,c.input_commitment,c.created_at,c.finalized_at,
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
		       COALESCE(r.external_cash_state,'')
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
		&receipt.RuntimeProfileID, &receipt.RuntimeProfileSHA256, &receipt.InputCommitment,
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
		&receipt.ExternalCashState)
	if errors.Is(err, pgx.ErrNoRows) {
		return RealtimeReceipt{}, errNotFound
	}
	if err != nil {
		return RealtimeReceipt{}, err
	}
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
