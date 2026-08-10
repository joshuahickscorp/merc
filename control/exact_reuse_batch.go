package main

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/google/uuid"
)

// batchExactReuseEnabled must remain false until cache-hit billing is bound to
// a complete, auditable batch meter. The retired writer recorded merged output
// record counts in exact_result_cache.output_tokens, but the hit path charged
// that field as token units. Guard here, rather than only in newly built
// workload decisions, so a historical accepted job that still carries the old
// eligibility bit cannot populate another poisoned cache row during merge.
//
// Realtime exact reuse does not use this guard: it stores and bills verified
// completion-token counts through its own request identity path.
const batchExactReuseEnabled = false

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
	workloadDecision WorkloadDecision,
	computePlan ComputePlan,
	pricingDecision PricingDecision,
	quoteID uuid.UUID,
	firmQuote bool,
	firmQuoteMaxUSD float64,
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
	if err := ValidateWorkloadDecisionSnapshot(workloadDecision); err != nil {
		return fmt.Errorf("invalid exact-reuse workload decision: %w", err)
	}
	if workloadDecision.Binding.JobType.Type != jobType ||
		workloadDecision.Binding.Model.Ref != modelRef ||
		workloadDecision.Binding.Tier != tier {
		return fmt.Errorf("exact-reuse workload decision does not match the submitted job")
	}
	if err := ValidateFrozenComputePlanSnapshot(computePlan, workloadDecision); err != nil {
		return fmt.Errorf("invalid exact-reuse compute plan: %w", err)
	}
	if computePlan.ExecutionMode != computeExecutionExactReuse {
		return fmt.Errorf("exact-reuse job requires exact-result-reuse compute mode")
	}
	if err := ValidateExactReusePricingDecisionSnapshot(
		pricingDecision, workloadDecision, computePlan,
	); err != nil {
		return fmt.Errorf("invalid exact-reuse pricing decision: %w", err)
	}
	if int64(computePlan.InputRecords) != inputRecords || computePlan.InputBytes != inputBytes {
		return fmt.Errorf("exact-reuse compute plan input geometry does not match the delivered job")
	}
	workloadJSON, err := json.Marshal(workloadDecision)
	if err != nil {
		return fmt.Errorf("marshal exact-reuse workload decision: %w", err)
	}
	workloadSHA256, err := workloadDecisionDigest(workloadDecision)
	if err != nil {
		return fmt.Errorf("hash exact-reuse workload decision: %w", err)
	}
	computeJSON, err := json.Marshal(computePlan)
	if err != nil {
		return fmt.Errorf("marshal exact-reuse compute plan: %w", err)
	}
	computeSHA256, err := computePlanDigest(computePlan)
	if err != nil {
		return fmt.Errorf("hash exact-reuse compute plan: %w", err)
	}
	buyerCharge := microsToUSD(money.BuyerDebitMicros)
	if computePlan.BaseComputeUSD != roundEconomicUSD(buyerCharge) {
		return fmt.Errorf("exact-reuse compute estimate does not match buyer settlement")
	}
	if pricingDecision.BuyerPrice != roundEconomicUSD(buyerCharge) {
		return fmt.Errorf("exact-reuse pricing decision does not match buyer settlement")
	}
	if firmQuote && (firmQuoteMaxUSD <= 0 || buyerCharge > firmQuoteMaxUSD+0.000001) {
		return fmt.Errorf("exact-reuse buyer charge exceeds the firm quote maximum")
	}
	platform := microsToUSD(money.PlatformMicros)
	jobCurrency := SettlementCurrencyCode()
	if err := RequireSettlementCurrency(jobCurrency); err != nil {
		return fmt.Errorf("exact-reuse job requires settlement currency authority: %w", err)
	}
	pricingJSON, err := json.Marshal(pricingDecision)
	if err != nil {
		return fmt.Errorf("marshal exact-reuse pricing decision: %w", err)
	}
	pricingSHA256, err := pricingDecisionDigest(pricingDecision)
	if err != nil {
		return fmt.Errorf("hash exact-reuse pricing decision: %w", err)
	}
	topologyDecision, err := buildExactReuseTopologyDecision(workloadSHA256)
	if err != nil {
		return fmt.Errorf("exact-reuse topology decision: %w", err)
	}
	topologyJSON, err := json.Marshal(topologyDecision)
	if err != nil {
		return fmt.Errorf("marshal exact-reuse topology decision: %w", err)
	}
	topologySHA256, err := topologyDecisionDigest(topologyDecision)
	if err != nil {
		return fmt.Errorf("hash exact-reuse topology decision: %w", err)
	}

	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	if quoteID != uuid.Nil {
		var quoteComputeSHA256, quotePricingSHA256, quoteCurrency string
		if err := tx.QueryRow(ctx,
			`SELECT COALESCE(compute_plan_sha256,''),
			        COALESCE(pricing_decision_sha256,''),currency
			   FROM quotes
			  WHERE id=$1 AND buyer_id=$2`,
			quoteID, buyerID,
		).Scan(&quoteComputeSHA256, &quotePricingSHA256, &quoteCurrency); err != nil {
			return fmt.Errorf("load exact-reuse origin quote compute authority: %w", err)
		}
		if quoteComputeSHA256 == "" ||
			computePlan.OriginComputePlanSHA256 != quoteComputeSHA256 {
			return fmt.Errorf("exact-reuse compute plan does not match its origin quote")
		}
		if quotePricingSHA256 == "" ||
			pricingDecision.OriginPricingDecisionSHA256 != quotePricingSHA256 {
			return fmt.Errorf("exact-reuse pricing decision does not match its origin quote")
		}
		if quoteCurrency != jobCurrency {
			return fmt.Errorf("%w: exact-reuse job currency %s does not match quote currency %s",
				errCurrencyMismatch, jobCurrency, quoteCurrency)
		}
	} else if computePlan.OriginComputePlanSHA256 != "" {
		return fmt.Errorf("unquoted exact reuse cannot carry an origin compute plan")
	} else if pricingDecision.OriginPricingDecisionSHA256 != "" {
		return fmt.Errorf("unquoted exact reuse cannot carry origin pricing authority")
	}

	// Fund gate (same shape as realtime): free credit + prepaid balance only.
	// A saved card is a top-up rail, not admission funding. Exact nanos so the
	// hold ceils a sub-micro charge rather than trusting a float projection.
	if err := evaluateRealtimeBuyerFunding(ctx, tx, buyerID, money.BuyerDebitNanos); err != nil {
		return err
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
		   submit_idempotency_key, submit_request_sha256,
		   workload_decision, workload_decision_sha256,
		   compute_plan, compute_plan_sha256,
		   pricing_decision, pricing_decision_sha256,
		   topology_decision, topology_decision_sha256,
		   quote_id, firm_quote, firm_quote_max_usd, currency)
		VALUES ($1,$2,'complete',$3,$4,$5,$6,$7,'{}'::jsonb,$8,$8,0,0,
		        'tracking','charged',
		        $9,$10,'exact_result_reuse',
		        NULLIF($11,''),NULLIF($12,''),$13::jsonb,$14,
		        $15::jsonb,$16,$17::jsonb,$18,$19::jsonb,$20,$21,$22,$23,$24)`,
		jobID, buyerID, jobType, modelRef, inputRef, outputRef, tier,
		buyerCharge, inputRecords, inputBytes,
		submitIdempotencyKey, submitRequestSHA256, workloadJSON, workloadSHA256,
		computeJSON, computeSHA256, pricingJSON, pricingSHA256,
		topologyJSON, topologySHA256,
		nullUUID(quoteID), firmQuote, nullPosFloat(firmQuoteMaxUSD), jobCurrency)
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

	// Money: buyer debit + platform take. No supplier_credit. Cite the
	// PricingDecision digest already sealed for this exact-reuse job.
	reuseAuth := liabilityAuthority{PricingDecisionSHA256: pricingSHA256}
	if err := reuseAuth.validate(); err != nil {
		return fmt.Errorf("exact reuse batch liability authority: %w", err)
	}
	buyerChargeEntry := ledgerInsert{
		Kind: KindBuyerCharge, BuyerID: &buyerID, TaskID: &taskID,
		AmountMicros: -money.BuyerDebitMicros, Currency: jobCurrency, PayoutStatus: PayoutReleased,
	}
	if err := applyLiabilityAuthority(&buyerChargeEntry, reuseAuth); err != nil {
		return err
	}
	if _, err := insertLedgerEntryTx(ctx, tx, buyerChargeEntry); err != nil {
		return err
	}
	platformTakeEntry := ledgerInsert{
		Kind: KindPlatformTake, TaskID: &taskID,
		AmountMicros: money.PlatformMicros, Currency: jobCurrency, PayoutStatus: PayoutReleased,
	}
	if err := applyLiabilityAuthority(&platformTakeEntry, reuseAuth); err != nil {
		return err
	}
	if _, err := insertLedgerEntryTx(ctx, tx, platformTakeEntry); err != nil {
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

// batchRequestIdentity derives exact-reuse identity from the complete frozen
// workload decision. This binds input, normalized params, output shape, model
// kind, pinned revision and runtime authority in one versioned key.
func batchRequestIdentity(decision WorkloadDecision) string {
	if !batchExactReuseEnabled || !decision.ExactResultCacheEligible ||
		decision.Binding.JobType.Type != "batch_infer" || decision.RuntimeJobType != "batch_infer" {
		return ""
	}
	if err := ValidateFrozenWorkloadDecisionSnapshot(decision); err != nil {
		return ""
	}
	decisionSHA256, err := workloadDecisionDigest(decision)
	if err != nil {
		return ""
	}
	id, err := (RequestIdentity{
		ModelID:       decision.Binding.Model.Ref,
		ModelRevision: decision.ModelRevision,
		Input:         "workload-decision-sha256:" + decisionSHA256,
		Temperature:   float64(decision.Binding.JobType.Temperature),
		TopP:          1,
		MaxTokens:     int(decision.Binding.JobType.MaxTokens),
		Policy:        "batch-workload-v2",
	}).Compute()
	if err != nil {
		return ""
	}
	return id
}

// batchIdentityForJob uses the exact authority frozen at admission, so the
// write path and the later lookup path cannot drift under catalogue changes.
func (s *Store) batchIdentityForJob(ctx context.Context, jobID uuid.UUID) (string, error) {
	decision, err := s.JobWorkloadDecision(ctx, jobID)
	if err != nil {
		return "", err
	}
	if decision == nil {
		return "", nil
	}
	return batchRequestIdentity(*decision), nil
}
