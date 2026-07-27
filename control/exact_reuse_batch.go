package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

// SubmitExactReuseBatchJob records a batch job that is served entirely from the
// exact-result cache: the buyer is billed at the reuse class, no tasks are
// queued, and no supplier is credited.
func (s *Store) SubmitExactReuseBatchJob(
	ctx context.Context,
	buyerID, jobID uuid.UUID,
	jobType, modelRef, inputRef, outputRef, tier string,
	hit ExactCacheHit,
	money ReuseHitSettlement,
	inputRecords, inputBytes int64,
	submitIdempotencyKey, submitRequestSHA256 string,
) error {
	if !money.Conserved() {
		return fmt.Errorf("reuse settlement not conserved")
	}
	if money.SupplierLiabilityMicros != 0 {
		return fmt.Errorf("exact reuse batch job must not credit a supplier")
	}
	if money.BuyerDebitMicros <= 0 {
		return fmt.Errorf("exact reuse batch job must charge the buyer")
	}
	buyerCharge := microsToUSD(money.BuyerDebitMicros)
	platform := microsToUSD(money.PlatformMicros)

	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)

	// Fund gate (same shape as realtime): free credit or a payment method.
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
			  FROM buyers b WHERE b.id=$1 AND b.deleted_at IS NULL FOR UPDATE`, buyerID).
			Scan(&freeCredit, &hasPaymentMethod, &spent, &batchReserved, &realtimeReserved)
		if errorsIsNotFound(err) {
			return errNotFound
		}
		if err != nil {
			return err
		}
		providerFunded := stripeKey() != "" && hasPaymentMethod
		if !providerFunded && freeCredit-spent-batchReserved-realtimeReserved < buyerCharge {
			return errRealtimeInsufficientFunds
		}
	}

	// Copy the cached result to the job's output key so the buyer downloads
	// from their own job namespace; the cache continues to hold the cas/ ref.
	// Callers that already placed the body at outputRef may pass that key.

	_, err = tx.Exec(ctx, `
		INSERT INTO jobs
		  (id, buyer_id, status, job_type, model_ref, input_ref, output_ref,
		   tier, verification_policy, estimated_usd, actual_usd, task_count, tasks_done,
		   budget_state, charge_status,
		   economic_input_records, economic_input_bytes, economic_input_source,
		   submit_idempotency_key, submit_request_sha256)
		VALUES ($1,$2,'complete',$3,$4,$5,$6,$7,'{}'::jsonb,$8,$8,0,0,
		        'tracking','charged',
		        $9,$10,'exact_result_reuse',
		        NULLIF($11,''),NULLIF($12,''))`,
		jobID, buyerID, jobType, modelRef, inputRef, outputRef, tier,
		buyerCharge, inputRecords, inputBytes,
		submitIdempotencyKey, submitRequestSHA256)
	if err != nil {
		return err
	}

	// Synthetic task so ledger rows can key on task_id (existing unique index).
	// Status complete, no worker, no claim — nothing for the scheduler to run.
	taskID := uuid.New()
	if _, err := tx.Exec(ctx, `
		INSERT INTO tasks
		  (id, job_id, status, input_ref, result_key, result_ref, chunk_index,
		   expected_output_records, economic_buyer_charge_usd, economic_supplier_payout_usd,
		   completed_at, verification_outcome, verified_at)
		VALUES ($1,$2,'complete',$3,$4,$5,0,$6,$7,0,now(),'pass',now())`,
		taskID, jobID, inputRef, outputRef, hit.ResultRef, inputRecords,
		buyerCharge); err != nil {
		return err
	}

	// Money: buyer debit + platform take. No supplier_credit.
	if _, err := insertLedgerEntryTx(ctx, tx, ledgerInsert{
		Kind: KindBuyerCharge, BuyerID: &buyerID, TaskID: &taskID,
		AmountMicros: -money.BuyerDebitMicros, PayoutStatus: PayoutReleased,
	}); err != nil {
		return err
	}
	if _, err := insertLedgerEntryTx(ctx, tx, ledgerInsert{
		Kind: KindPlatformTake, TaskID: &taskID,
		AmountMicros: money.PlatformMicros, PayoutStatus: PayoutReleased,
	}); err != nil {
		return err
	}
	_ = platform

	// Token accounting: all delivered tokens are exact_result_reuse; physical=0.
	for class, tokens := range money.Accounting {
		if _, err := tx.Exec(ctx, `
			INSERT INTO job_token_accounting (job_id, class, tokens)
			VALUES ($1,$2,$3)
			ON CONFLICT (job_id, class) DO UPDATE SET tokens = EXCLUDED.tokens`,
			jobID, class, tokens); err != nil {
			return err
		}
	}

	return tx.Commit(ctx)
}

func errorsIsNotFound(err error) bool {
	return err != nil && (err == errNotFound || err == pgx.ErrNoRows)
}

// CopyExactReuseResultToJobOutput materialises a cas/ cache hit into the buyer's
// job output key so subsequent downloads stay tenant-scoped while the cache
// itself never stores a jobs/ path.
func (s *Store) CopyExactReuseResultToJobOutput(ctx context.Context, storage *Storage, hit ExactCacheHit, outputKey string) error {
	if storage == nil {
		return fmt.Errorf("storage required")
	}
	body, err := s.LoadExactResultBytes(ctx, storage, hit.ResultRef)
	if err != nil {
		return err
	}
	return storage.PutObject(ctx, outputKey, body, "application/x-ndjson")
}

// maybeCacheCompletedBatchJob stores a finished job's primary output under a
// content-addressed ref when the request identity is deterministic. The
// scheduler's jobs/ result path is never written into the shared cache.
func (s *Store) maybeCacheCompletedBatchJob(
	ctx context.Context,
	storage *Storage,
	identity string,
	resultBody []byte,
	outputTokens int64,
) {
	if identity == "" || len(resultBody) == 0 || storage == nil {
		return
	}
	if _, err := s.StoreExactResultBytes(ctx, storage, identity, resultBody, outputTokens); err != nil {
		// Best-effort: cache population must not fail settlement.
		_ = err
	}
}

// batchRequestIdentity derives the cache key for a batch submission. Empty
// string means not cacheable.
func batchRequestIdentity(modelRef, modelRevision, jobType, inputSHA256 string, maxTokens uint32, temperature float64) string {
	id, err := batchIdentityFromJob(modelRef, modelRevision, jobType, inputSHA256, int(maxTokens), temperature, 0)
	if err != nil {
		return ""
	}
	return id
}

// batchIdentityForJob re-derives the request identity for a completed job so
// its output can be cached without the client ever supplying a key. The input
// SHA is taken from the stored input object so it matches createJob's stream
// digest.
func (s *Store) batchIdentityForJob(ctx context.Context, storage *Storage, jobID uuid.UUID) (string, error) {
	var modelRef, jobType, inputRef string
	var maxTokens int
	err := s.pool.QueryRow(ctx, `
		SELECT COALESCE(model_ref,''), COALESCE(job_type,''), COALESCE(input_ref,''),
		       COALESCE((job_type_spec->>'max_tokens')::int, 0)
		  FROM jobs WHERE id=$1`, jobID).Scan(&modelRef, &jobType, &inputRef, &maxTokens)
	if err != nil {
		return "", err
	}
	if storage == nil || inputRef == "" {
		return "", fmt.Errorf("cannot derive batch identity without stored input")
	}
	body, err := storage.GetObject(ctx, inputRef)
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(body)
	inputSHA := hex.EncodeToString(sum[:])
	return batchRequestIdentity(modelRef, modelRef, jobType, inputSHA, uint32(maxTokens), 0), nil
}
