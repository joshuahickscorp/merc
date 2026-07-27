package main

import (
	"context"
	_ "embed"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

// Split out of store.go, which had grown to 5,727 lines across roughly two
// dozen unrelated responsibilities.  Same package, same behaviour: this is a
// file move so that a reviewer can hold one subject at a time and two people
// can edit payouts and job submission without conflicting.

type AdminPayout struct {
	SupplierID               uuid.UUID `json:"supplier_id"`
	PayoutStatus             string    `json:"payout_status"`
	Count                    int       `json:"count"`
	AmountUSD                float64   `json:"amount_usd"`
	CashSentUSD              float64   `json:"cash_sent_usd"`
	CarriedRemainderUSD      float64   `json:"carried_remainder_usd"`
	OutcomeUnknownCount      int       `json:"outcome_unknown_count"`
	ReleasedWithoutCashCount int       `json:"released_without_cash_count"`
}

func (s *Store) ListPayoutsAdmin(ctx context.Context) ([]AdminPayout, error) {
	rows, err := s.pool.Query(ctx,
		`SELECT COALESCE(le.supplier_id,'00000000-0000-0000-0000-000000000000'::uuid),
		        le.payout_status, COUNT(*), COALESCE(SUM(le.amount_usd),0),
		        COALESCE(SUM(op.sent_cents) FILTER (WHERE op.cash_moved),0)::float8 / 100.0,
		        COALESCE(SUM(mu.remainder_microusd),0)::float8 / 1000000.0,
		        COUNT(*) FILTER (WHERE COALESCE(op.outcome_unknown,false)),
		        COUNT(*) FILTER (
		          WHERE le.payout_status='released' AND NOT COALESCE(op.cash_moved,false))
		 FROM ledger_entries le
		 LEFT JOIN supplier_payout_operations op ON op.ledger_entry_id=le.id
		 LEFT JOIN supplier_minor_unit_settlements mu ON mu.ledger_entry_id=le.id
		 WHERE le.kind = 'supplier_credit'
		 GROUP BY le.supplier_id, le.payout_status
		 ORDER BY le.supplier_id, le.payout_status`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []AdminPayout
	for rows.Next() {
		var a AdminPayout
		if err := rows.Scan(&a.SupplierID, &a.PayoutStatus, &a.Count, &a.AmountUSD,
			&a.CashSentUSD, &a.CarriedRemainderUSD, &a.OutcomeUnknownCount,
			&a.ReleasedWithoutCashCount); err != nil {
			return nil, err
		}
		out = append(out, a)
	}
	return out, rows.Err()
}

func isPayoutFundingUniqueViolation(err error) bool {
	var pgErr *pgconn.PgError
	return errors.As(err, &pgErr) && pgErr.Code == "23505"
}

func (s *Store) CreateSubsidyFund(
	ctx context.Context,
	actor AdminActor,
	fundRef string,
	externalTreasuryRef string,
	authorizedCents int64,
	reason string,
) (bool, error) {
	fundRef = strings.TrimSpace(fundRef)
	externalTreasuryRef = strings.TrimSpace(externalTreasuryRef)
	reason = strings.TrimSpace(reason)
	if fundRef == "" || externalTreasuryRef == "" || reason == "" {
		return false, errors.New("fund_ref, external_treasury_ref, and reason are required")
	}
	if len(fundRef) > 200 || len(externalTreasuryRef) > 300 || len(reason) > 1000 {
		return false, errors.New("subsidy fund field is too long")
	}
	if authorizedCents <= 0 {
		return false, errors.New("authorized_cents must be positive")
	}
	if err := validateAdminActorShape(actor); err != nil {
		return false, err
	}

	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return false, err
	}
	defer tx.Rollback(ctx)
	if err := revalidateAdminActor(ctx, tx, actor); err != nil {
		return false, err
	}
	if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtextextended($1,0))`, fundRef); err != nil {
		return false, err
	}

	var (
		existingFundID, existingActionID                      uuid.UUID
		existingTreasuryRef, existingCurrency, existingReason string
		existingStatus                                        string
		existingAuthorizedCents                               int64
	)
	err = tx.QueryRow(ctx, `
		SELECT id,COALESCE(authorization_action_id,'00000000-0000-0000-0000-000000000000'::uuid),
		       external_treasury_ref,authorized_cents,currency,reason,status
		  FROM platform_subsidy_funds WHERE fund_ref=$1 FOR UPDATE`, fundRef).Scan(
		&existingFundID, &existingActionID, &existingTreasuryRef, &existingAuthorizedCents,
		&existingCurrency, &existingReason, &existingStatus)
	if err == nil {
		intent := moneyAuthorityIntent{
			Kind: "subsidy_fund_authorized", TargetKind: "subsidy_fund",
			TargetID: existingFundID, FundID: existingFundID, FundRef: fundRef,
			ExternalTreasuryRef: externalTreasuryRef, AmountCents: authorizedCents,
			Currency: SettlementCurrencyCode(), Reason: reason, CorrelationRef: fundRef,
		}
		if existingTreasuryRef != externalTreasuryRef || existingAuthorizedCents != authorizedCents ||
			RequireSettlementCurrency(existingCurrency) != nil || existingReason != reason || existingStatus != "active" {
			return false, errSubsidyFundConflict
		}
		if err := assertMoneyAuthorityAction(ctx, tx, actor, existingActionID, intent); err != nil {
			return false, err
		}
		if err := tx.Commit(ctx); err != nil {
			return false, err
		}
		return false, nil
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return false, err
	}

	fundID, actionID := uuid.New(), uuid.New()
	intent := moneyAuthorityIntent{
		Kind: "subsidy_fund_authorized", TargetKind: "subsidy_fund",
		TargetID: fundID, FundID: fundID, FundRef: fundRef,
		ExternalTreasuryRef: externalTreasuryRef, AmountCents: authorizedCents,
		Currency: SettlementCurrencyCode(), Reason: reason, CorrelationRef: fundRef,
	}
	if _, err := insertMoneyAuthorityAction(ctx, tx, actor, actionID, intent, nil); err != nil {
		if isPayoutFundingUniqueViolation(err) {
			return false, errSubsidyFundConflict
		}
		return false, err
	}
	if _, err := tx.Exec(ctx, `
		INSERT INTO platform_subsidy_funds
		  (id,authorization_action_id,fund_ref,external_treasury_ref,
		   authorized_cents,currency,reason,status)
		VALUES ($1,$2,$3,$4,$5,$6,$7,'active')`,
		fundID, actionID, fundRef, externalTreasuryRef, authorizedCents, SettlementCurrencyCode(), reason); err != nil {
		if isPayoutFundingUniqueViolation(err) {
			return false, errSubsidyFundConflict
		}
		return false, err
	}
	if err := tx.Commit(ctx); err != nil {
		return false, err
	}
	return true, nil
}

func (s *Store) AuthorizePayoutSubsidy(
	ctx context.Context,
	actor AdminActor,
	entryID uuid.UUID,
	fundRef string,
	authorizationRef string,
	reason string,
) (bool, error) {
	fundRef = strings.TrimSpace(fundRef)
	authorizationRef = strings.TrimSpace(authorizationRef)
	reason = strings.TrimSpace(reason)
	if fundRef == "" {
		return false, errors.New("subsidy fund_ref is required")
	}
	if authorizationRef == "" {
		return false, errors.New("subsidy authorization_ref is required")
	}
	if reason == "" {
		return false, errors.New("subsidy reason is required")
	}
	if len(fundRef) > 200 || len(authorizationRef) > 200 {
		return false, errors.New("subsidy authorization_ref is too long")
	}
	if len(reason) > 1000 {
		return false, errors.New("subsidy reason is too long")
	}
	if err := validateAdminActorShape(actor); err != nil {
		return false, err
	}

	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return false, err
	}
	defer tx.Rollback(ctx)
	if err := revalidateAdminActor(ctx, tx, actor); err != nil {
		return false, err
	}

	var (
		supplierID      uuid.UUID
		taskID          *uuid.UUID
		status          string
		liabilityMicros int64
	)
	if err := tx.QueryRow(ctx, `
		SELECT supplier_id,task_id,payout_status,(amount_usd*1000000)::bigint
		  FROM ledger_entries
		 WHERE id=$1 AND kind='supplier_credit'
		 FOR UPDATE`, entryID,
	).Scan(&supplierID, &taskID, &status, &liabilityMicros); errors.Is(err, pgx.ErrNoRows) {
		return false, errNotFound
	} else if err != nil {
		return false, err
	}
	if status != PayoutHeld && status != PayoutReady && status != PayoutAwaitingFunding {
		return false, errNotHeld
	}
	amountCents, _, err := splitSupplierLiabilityMicros(liabilityMicros)
	if err != nil {
		return false, err
	}
	if amountCents <= 0 {
		return false, fmt.Errorf("supplier credit %s is carried below one cent and needs no subsidy cash authorization", entryID)
	}

	var (
		existingSource, existingFundRef, existingRef, existingReason, existingCurrency string
		existingAmount                                                                 int64
		existingFundID, existingActionID                                               uuid.UUID
	)
	err = tx.QueryRow(ctx, `
		SELECT funding.source_kind,COALESCE(fund.id,'00000000-0000-0000-0000-000000000000'::uuid),
		       COALESCE(funding.authorization_action_id,'00000000-0000-0000-0000-000000000000'::uuid),
		       COALESCE(fund.fund_ref,''),
		       COALESCE(funding.subsidy_authorization_ref,''),
		       COALESCE(funding.subsidy_reason,''),funding.amount_cents,funding.currency
		  FROM supplier_payout_funding funding
		  LEFT JOIN platform_subsidy_funds fund ON fund.id=funding.subsidy_fund_id
		 WHERE funding.ledger_entry_id=$1
		 FOR UPDATE OF funding`, entryID,
	).Scan(&existingSource, &existingFundID, &existingActionID, &existingFundRef,
		&existingRef, &existingReason, &existingAmount, &existingCurrency)
	if err == nil {
		if existingSource == payoutFundingPlatformSubsidy && existingFundRef == fundRef &&
			existingRef == authorizationRef && existingReason == reason &&
			existingAmount == amountCents && RequireSettlementCurrency(existingCurrency) == nil {
			intent := moneyAuthorityIntent{
				Kind: "payout_subsidy_authorized", TargetKind: "supplier_liability",
				TargetID: entryID, FundID: existingFundID, FundRef: fundRef,
				AuthorizationRef: authorizationRef, AmountCents: amountCents,
				Currency: SettlementCurrencyCode(), Reason: reason, CorrelationRef: authorizationRef,
			}
			if err := assertMoneyAuthorityAction(ctx, tx, actor, existingActionID, intent); err != nil {
				return false, err
			}
			if status == PayoutAwaitingFunding {
				if _, err := tx.Exec(ctx,
					`UPDATE ledger_entries SET payout_status=$2
					  WHERE id=$1 AND payout_status=$3`,
					entryID, PayoutHeld, PayoutAwaitingFunding); err != nil {
					return false, err
				}
			}
			if err := tx.Commit(ctx); err != nil {
				return false, err
			}
			return false, nil
		}
		return false, errPayoutFundingAlreadyBound
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return false, err
	}

	var fundID, fundActionID uuid.UUID
	var capacity int64
	var fundTreasuryRef, fundCurrency, fundReason string
	if err := tx.QueryRow(ctx, `
		SELECT id,COALESCE(authorization_action_id,'00000000-0000-0000-0000-000000000000'::uuid),
		       external_treasury_ref,authorized_cents,currency,reason
		  FROM platform_subsidy_funds
		 WHERE fund_ref=$1 AND status='active' AND currency=$2
		 FOR UPDATE`, fundRef, SettlementCurrencyCode(),
	).Scan(&fundID, &fundActionID, &fundTreasuryRef, &capacity, &fundCurrency, &fundReason); errors.Is(err, pgx.ErrNoRows) {
		return false, errSubsidyFundUnavailable
	} else if err != nil {
		return false, err
	}
	fundIntent := moneyAuthorityIntent{
		Kind: "subsidy_fund_authorized", TargetKind: "subsidy_fund",
		TargetID: fundID, FundID: fundID, FundRef: fundRef,
		ExternalTreasuryRef: fundTreasuryRef, AmountCents: capacity,
		Currency: fundCurrency, Reason: fundReason, CorrelationRef: fundRef,
	}
	if err := assertMoneyAuthorityAction(ctx, tx, actor, fundActionID, fundIntent); err != nil {
		return false, err
	}
	var reserved int64
	if err := tx.QueryRow(ctx, `
		SELECT COALESCE(sum(amount_cents),0)::bigint
		  FROM supplier_payout_funding
		 WHERE source_kind='platform_subsidy' AND subsidy_fund_id=$1`, fundID,
	).Scan(&reserved); err != nil {
		return false, err
	}
	if reserved < 0 || reserved > capacity || amountCents > capacity-reserved {
		return false, errSubsidyFundUnavailable
	}

	var liabilityJobID *uuid.UUID
	if taskID != nil {
		var jobID uuid.UUID
		if err := tx.QueryRow(ctx, `SELECT job_id FROM tasks WHERE id=$1`, *taskID).Scan(&jobID); err != nil {
			return false, err
		}
		liabilityJobID = &jobID
	}
	actionID := uuid.New()
	intent := moneyAuthorityIntent{
		Kind: "payout_subsidy_authorized", TargetKind: "supplier_liability",
		TargetID: entryID, FundID: fundID, FundRef: fundRef,
		AuthorizationRef: authorizationRef, AmountCents: amountCents,
		Currency: SettlementCurrencyCode(), Reason: reason, CorrelationRef: authorizationRef,
	}
	if _, err := insertMoneyAuthorityAction(ctx, tx, actor, actionID, intent, &supplierID); err != nil {
		if isPayoutFundingUniqueViolation(err) {
			return false, errPayoutFundingAlreadyBound
		}
		return false, err
	}
	if _, err := tx.Exec(ctx, `
		INSERT INTO supplier_payout_funding
		  (authorization_action_id,ledger_entry_id,source_kind,liability_job_id,
		   subsidy_fund_id,subsidy_authorization_ref,subsidy_reason,amount_cents,currency)
		VALUES ($1,$2,'platform_subsidy',$3,$4,$5,$6,$7,$8)`,
		actionID, entryID, liabilityJobID, fundID, authorizationRef, reason, amountCents, SettlementCurrencyCode()); err != nil {
		if isPayoutFundingUniqueViolation(err) {
			return false, errPayoutFundingAlreadyBound
		}
		return false, err
	}
	if status == PayoutAwaitingFunding {
		if _, err := tx.Exec(ctx,
			`UPDATE ledger_entries SET payout_status=$2
			  WHERE id=$1 AND payout_status=$3`,
			entryID, PayoutHeld, PayoutAwaitingFunding); err != nil {
			return false, err
		}
	}
	if err := tx.Commit(ctx); err != nil {
		return false, err
	}
	return true, nil
}

func (s *Store) ReleasePayoutTx(ctx context.Context, actor AdminActor, entryID uuid.UUID, reason, correlationRef string) error {
	intent, err := prepareAdminMutation(actor, adminMutationIntent{
		Kind: adminActionPayoutReleased, TargetKind: adminTargetLedgerEntry,
		TargetID: entryID, Reason: reason, CorrelationRef: correlationRef,
	})
	if err != nil {
		return err
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	if err := revalidateAdminActor(ctx, tx, actor); err != nil {
		return err
	}
	if replay, err := acquireAdminMutationReplay(ctx, tx, actor, intent); err != nil {
		return err
	} else if replay.Found {
		return tx.Commit(ctx)
	}

	var supplierID uuid.UUID
	var kind, beforeStatus string
	var beforeReleaseAt *time.Time
	if err := tx.QueryRow(ctx, `
		SELECT supplier_id,kind,payout_status,release_at
		  FROM ledger_entries WHERE id=$1 FOR UPDATE`, entryID).Scan(
		&supplierID, &kind, &beforeStatus, &beforeReleaseAt); errors.Is(err, pgx.ErrNoRows) {
		return errNotFound
	} else if err != nil {
		return err
	}
	if kind != KindSupplierCredit || (beforeStatus != PayoutHeld && beforeStatus != PayoutReady) {
		return errNotHeld
	}
	var afterReleaseAt time.Time
	if err := tx.QueryRow(ctx, `
		UPDATE ledger_entries SET payout_status='held',release_at=now()
		 WHERE id=$1 RETURNING release_at`, entryID).Scan(&afterReleaseAt); err != nil {
		return err
	}

	if err := insertAdminMutationAction(ctx, tx, actor, intent, nil, &supplierID, &entryID,
		map[string]any{"payout_status": beforeStatus, "release_at": beforeReleaseAt},
		map[string]any{"payout_status": PayoutHeld, "release_at": afterReleaseAt}); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

func reservePayoutFunding(
	ctx context.Context,
	tx pgx.Tx,
	entryID uuid.UUID,
	taskID *uuid.UUID,
	requestedCents int64,
	currency string,
) (uuid.UUID, bool, error) {
	var (
		existingID       uuid.UUID
		existingSource   string
		existingAmount   int64
		existingCurrency string
		existingState    string
	)
	err := tx.QueryRow(ctx, `
		SELECT f.id,f.source_kind,f.amount_cents,f.currency,
		       COALESCE(fs.status,'available')
		  FROM supplier_payout_funding f
		  LEFT JOIN supplier_payout_funding_state fs ON fs.funding_id=f.id
		 WHERE f.ledger_entry_id=$1
		 FOR UPDATE OF f`, entryID,
	).Scan(&existingID, &existingSource, &existingAmount, &existingCurrency, &existingState)
	if err == nil {
		if existingAmount != requestedCents || existingCurrency != currency ||
			(existingSource != payoutFundingBuyerCollection && existingSource != payoutFundingPlatformSubsidy) {
			return uuid.Nil, false, fmt.Errorf(
				"payout funding for ledger entry %s does not match liability: source=%s amount=%d %s liability=%d %s",
				entryID, existingSource, existingAmount, existingCurrency, requestedCents, currency)
		}
		if existingState == "compromised" {
			return existingID, false, nil
		}
		return existingID, true, nil
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return uuid.Nil, false, err
	}
	if taskID == nil {
		return uuid.Nil, false, nil
	}

	var (
		jobID                                     uuid.UUID
		buyerID                                   uuid.UUID
		chargeStatus, paymentIntent, cashCurrency string
		batchID                                   *uuid.UUID
		cashRequested, cashReceived               int64
	)
	if err := tx.QueryRow(ctx, `
		SELECT j.id,j.buyer_id,j.charge_status,j.charge_batch_id,
		       COALESCE(j.stripe_pi,''),COALESCE(j.charge_requested_cents,0),
		       COALESCE(j.charge_received_cents,0),COALESCE(j.charge_currency,'')
		  FROM tasks t JOIN jobs j ON j.id=t.job_id
		 WHERE t.id=$1
		 FOR UPDATE OF j`, *taskID,
	).Scan(&jobID, &buyerID, &chargeStatus, &batchID, &paymentIntent,
		&cashRequested, &cashReceived, &cashCurrency); err != nil {
		return uuid.Nil, false, err
	}
	if chargeStatus != "charged" {
		return uuid.Nil, false, nil
	}

	sourceKind := "job"
	if batchID != nil {
		sourceKind = "batch"
		var batchBuyer uuid.UUID
		var batchStatus string
		if err := tx.QueryRow(ctx, `
			SELECT buyer_id,status,COALESCE(stripe_pi,''),
			       COALESCE(charge_requested_cents,0),COALESCE(charge_received_cents,0),
			       COALESCE(charge_currency,'')
			  FROM charge_batches WHERE id=$1 FOR UPDATE`, *batchID,
		).Scan(&batchBuyer, &batchStatus, &paymentIntent, &cashRequested, &cashReceived, &cashCurrency); err != nil {
			return uuid.Nil, false, err
		}
		if batchBuyer != buyerID || batchStatus != "charged" {
			return uuid.Nil, false, nil
		}
	}
	if strings.TrimSpace(paymentIntent) == "" || cashRequested <= 0 ||
		cashReceived != cashRequested || cashCurrency != currency {
		return uuid.Nil, false, nil
	}

	var canonicalBuyer uuid.UUID
	var canonicalRequested, canonicalReceived int64
	var canonicalCurrency, canonicalChargeID string
	if sourceKind == "job" {
		err = tx.QueryRow(ctx, `
			SELECT buyer_id,requested_cents,received_cents,currency,COALESCE(charge_id,'')
			  FROM buyer_cash_collections
			 WHERE payment_intent=$1 AND source_kind='job' AND job_id=$2
			 FOR UPDATE`, paymentIntent, jobID,
		).Scan(&canonicalBuyer, &canonicalRequested, &canonicalReceived, &canonicalCurrency, &canonicalChargeID)
	} else {
		err = tx.QueryRow(ctx, `
			SELECT buyer_id,requested_cents,received_cents,currency,COALESCE(charge_id,'')
			  FROM buyer_cash_collections
			 WHERE payment_intent=$1 AND source_kind='batch' AND charge_batch_id=$2
			 FOR UPDATE`, paymentIntent, *batchID,
		).Scan(&canonicalBuyer, &canonicalRequested, &canonicalReceived, &canonicalCurrency, &canonicalChargeID)
	}
	if errors.Is(err, pgx.ErrNoRows) {
		return uuid.Nil, false, nil
	}
	if err != nil {
		return uuid.Nil, false, err
	}
	if canonicalBuyer != buyerID || canonicalRequested != cashRequested ||
		canonicalReceived != cashReceived || canonicalCurrency != cashCurrency {
		return uuid.Nil, false, fmt.Errorf("canonical buyer cash %s disagrees with its %s source", paymentIntent, sourceKind)
	}
	if strings.TrimSpace(canonicalChargeID) == "" {
		return uuid.Nil, false, nil
	}

	unavailable, err := stripeCollectionUnavailableCents(ctx, tx, paymentIntent, cashReceived)
	if err != nil {
		return uuid.Nil, false, err
	}
	available := cashReceived - unavailable
	if available < 0 {
		return uuid.Nil, false, fmt.Errorf("stripe cash state for %s exceeds collected cash", paymentIntent)
	}

	var reserved int64
	if err := tx.QueryRow(ctx, `
		SELECT COALESCE(sum(amount_cents),0)::bigint
		  FROM supplier_payout_funding
		 WHERE source_kind='buyer_collection' AND collection_payment_intent=$1`, paymentIntent,
	).Scan(&reserved); err != nil {
		return uuid.Nil, false, err
	}
	if reserved < 0 || reserved > available || requestedCents > available-reserved {
		return uuid.Nil, false, nil
	}

	var fundingID uuid.UUID
	if err := tx.QueryRow(ctx, `
		INSERT INTO supplier_payout_funding
		  (ledger_entry_id,source_kind,liability_job_id,collection_payment_intent,
		   amount_cents,currency)
		VALUES ($1,'buyer_collection',$2,$3,$4,$5)
		RETURNING id`, entryID, jobID, paymentIntent, requestedCents, currency,
	).Scan(&fundingID); err != nil {
		return uuid.Nil, false, err
	}
	return fundingID, true, nil
}

func (s *Store) DuePayouts(ctx context.Context, limit int) ([]DueHeldEntry, error) {
	manualGate, err := canaryManualPayoutGate()
	if err != nil {
		return nil, err
	}
	rows, err := s.pool.Query(ctx,
		`SELECT le.id, le.supplier_id, le.amount_usd
		 FROM ledger_entries le LEFT JOIN tasks t ON t.id=le.task_id
		 WHERE le.kind = 'supplier_credit' AND le.payout_status = 'held'
		   AND le.release_at IS NOT NULL AND le.release_at <= now()
		   AND NOT EXISTS (
		       SELECT 1 FROM disputes d
		        WHERE d.job_id=t.job_id
		          AND d.status IN ('open','no_peer','reverifying','unresolvable'))
		   -- A provisional pass_with_penalty is visible to the buyer but cannot
		   -- leave the platform as supplier money until an unqualified durable pass.
		   AND (le.task_id IS NULL OR t.verification_outcome = 'pass')
		   AND (NOT $2 OR EXISTS (
		       SELECT 1 FROM admin_actions aa
		       WHERE aa.kind='payout_released' AND aa.ledger_entry_id=le.id))
		 ORDER BY le.release_at ASC,le.id ASC LIMIT $1`, limit, manualGate)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []DueHeldEntry
	for rows.Next() {
		var e DueHeldEntry
		if err := rows.Scan(&e.ID, &e.SupplierID, &e.AmountUSD); err != nil {
			return nil, err
		}
		out = append(out, e)
	}
	return out, rows.Err()
}

func (s *Store) ClaimPayout(ctx context.Context, entryID uuid.UUID) (DueHeldEntry, bool, error) {
	var out DueHeldEntry
	manualGate, err := canaryManualPayoutGate()
	if err != nil {
		return out, false, err
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return out, false, err
	}
	defer tx.Rollback(ctx)

	// All dispute filing and payout claiming serializes on the parent job row
	// before either path locks a liability.  Whichever transaction wins that
	// lock establishes the ordering; a claim that runs second observes the
	// active dispute and cannot advance the credit to sending.
	var jobID uuid.UUID
	err = tx.QueryRow(ctx, `
		SELECT t.job_id
		  FROM ledger_entries le JOIN tasks t ON t.id=le.task_id
		 WHERE le.id=$1 AND le.kind='supplier_credit'`, entryID).Scan(&jobID)
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return out, false, err
	}
	if err == nil {
		if err := tx.QueryRow(ctx,
			`SELECT id FROM jobs WHERE id=$1 FOR UPDATE`, jobID).Scan(&jobID); err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				return out, false, errNotFound
			}
			return out, false, err
		}
		var disputed bool
		if err := tx.QueryRow(ctx, `
			SELECT EXISTS(SELECT 1 FROM disputes
			 WHERE job_id=$1 AND status IN ('open','no_peer','reverifying','unresolvable'))`,
			jobID).Scan(&disputed); err != nil {
			return out, false, err
		}
		if disputed {
			return out, false, nil
		}
	}

	var (
		status          string
		releaseAt       *time.Time
		verdict         string
		taskID          *uuid.UUID
		liabilityMicros int64
	)
	err = tx.QueryRow(ctx, `
		SELECT le.id,le.supplier_id,le.amount_usd::float8,
		       (le.amount_usd*1000000)::bigint,le.payout_status,
		       le.release_at,COALESCE(t.verification_outcome,''),le.task_id
		  FROM ledger_entries le LEFT JOIN tasks t ON t.id=le.task_id
		 WHERE le.id=$1 AND le.kind='supplier_credit'
		 FOR UPDATE OF le`, entryID).
		Scan(&out.ID, &out.SupplierID, &out.AmountUSD, &liabilityMicros, &status, &releaseAt, &verdict, &taskID)
	if errors.Is(err, pgx.ErrNoRows) {
		return out, false, errNotFound
	}
	if err != nil {
		return out, false, err
	}
	if status != PayoutHeld || releaseAt == nil || releaseAt.After(time.Now()) ||
		(taskID != nil && verdict != string(OutcomePass)) {
		return out, false, nil
	}
	if manualGate {
		var approved bool
		if err := tx.QueryRow(ctx, `
			SELECT EXISTS(SELECT 1 FROM admin_actions
			 WHERE kind='payout_released' AND ledger_entry_id=$1)`, entryID).Scan(&approved); err != nil {
			return out, false, err
		}
		if !approved {
			return out, false, nil
		}
	}
	out.LiabilityMicros = liabilityMicros
	out.SettlementPolicy = supplierSettlementPolicyAccountAccrualV2
	out.Currency = SettlementCurrencyCode()
	// Account-level accrual: this entry's liability joins the supplier's carry,
	// and we pay whatever whole cents the combined total supports. Flooring the
	// entry on its own is what made every sub-cent credit unpayable forever.
	out.RequestedCents, out.RemainderMicros, err = accrueSupplierLiability(
		ctx, tx, out.SupplierID, entryID, liabilityMicros)
	if err != nil {
		return out, false, err
	}
	// carried == "absorbed into the supplier accrual". Its value is not lost:
	// supplier_payout_accruals holds it and a later claim pays it out.
	if out.RequestedCents == 0 {
		ct, err := tx.Exec(ctx,
			`UPDATE ledger_entries SET payout_status=$2 WHERE id=$1 AND payout_status=$3`,
			entryID, PayoutCarried, PayoutHeld)
		if err != nil {
			return out, false, err
		}
		if ct.RowsAffected() != 1 {
			return out, false, nil
		}
		if err := tx.Commit(ctx); err != nil {
			return out, false, err
		}
		return out, false, nil
	}
	out.AmountUSD = float64(out.RequestedCents) / 100
	fundingID, funded, err := reservePayoutFunding(
		ctx, tx, entryID, taskID, out.RequestedCents, out.Currency)
	if err != nil {
		return out, false, err
	}
	if !funded {
		if _, err := tx.Exec(ctx,
			`UPDATE ledger_entries SET payout_status=$2
			  WHERE id=$1 AND payout_status=$3`,
			entryID, PayoutAwaitingFunding, PayoutHeld); err != nil {
			return out, false, err
		}
		if err := tx.Commit(ctx); err != nil {
			return out, false, err
		}
		return out, false, nil
	}

	var opID uuid.UUID
	err = tx.QueryRow(ctx, `
		INSERT INTO supplier_payout_operations
		  (ledger_entry_id,funding_id,supplier_id,requested_cents,currency,status,last_error)
		VALUES ($1,$2,$3,$4,$5,'sending',NULL)
		ON CONFLICT (ledger_entry_id) DO UPDATE SET
		  funding_id=EXCLUDED.funding_id,status='sending',last_error=NULL,updated_at=now()
		WHERE supplier_payout_operations.status='ready'
		  AND (supplier_payout_operations.funding_id IS NULL
		       OR supplier_payout_operations.funding_id=EXCLUDED.funding_id)
		  AND supplier_payout_operations.supplier_id=EXCLUDED.supplier_id
		  AND supplier_payout_operations.requested_cents=EXCLUDED.requested_cents
		  AND supplier_payout_operations.currency=EXCLUDED.currency
		RETURNING id`, out.ID, fundingID, out.SupplierID, out.RequestedCents, out.Currency).Scan(&opID)
	if errors.Is(err, pgx.ErrNoRows) {
		return out, false, fmt.Errorf("payout operation for ledger entry %s is not retryable", entryID)
	}
	if err != nil {
		return out, false, err
	}
	ct, err := tx.Exec(ctx,
		`UPDATE ledger_entries SET payout_status=$2 WHERE id=$1 AND payout_status=$3`,
		entryID, PayoutSending, PayoutHeld)
	if err != nil {
		return out, false, err
	}
	if ct.RowsAffected() != 1 {
		return out, false, nil
	}
	if err := tx.Commit(ctx); err != nil {
		return out, false, err
	}
	return out, true, nil
}

func (s *Store) RecoverStalePayoutOperations(ctx context.Context, lease time.Duration, limit int) (int, error) {
	if limit <= 0 {
		return 0, nil
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return 0, err
	}
	defer tx.Rollback(ctx)

	rows, err := tx.Query(ctx, `
		SELECT op.ledger_entry_id
		  FROM supplier_payout_operations op
		  JOIN ledger_entries le ON le.id=op.ledger_entry_id
		 WHERE op.status='sending' AND le.payout_status='sending'
		   AND NOT op.cash_moved AND op.transfer_ref IS NULL
		   AND op.updated_at <= $1
		 ORDER BY op.updated_at,op.ledger_entry_id
		 FOR UPDATE OF op,le SKIP LOCKED
		 LIMIT $2`, time.Now().Add(-lease), limit)
	if err != nil {
		return 0, err
	}
	var ids []uuid.UUID
	for rows.Next() {
		var id uuid.UUID
		if err := rows.Scan(&id); err != nil {
			rows.Close()
			return 0, err
		}
		ids = append(ids, id)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return 0, err
	}
	for _, id := range ids {
		if _, err := tx.Exec(ctx, `
			UPDATE supplier_payout_operations
			   SET status='outcome_unknown',outcome_unknown=true,
			       last_error='sending lease expired; provider outcome unknown'
			 WHERE ledger_entry_id=$1 AND status='sending'
			   AND NOT cash_moved AND transfer_ref IS NULL`, id); err != nil {
			return 0, err
		}
		if _, err := tx.Exec(ctx, `
			UPDATE ledger_entries SET payout_status='outcome_unknown'
			 WHERE id=$1 AND payout_status='sending'`, id); err != nil {
			return 0, err
		}
	}
	if err := tx.Commit(ctx); err != nil {
		return 0, err
	}
	return len(ids), nil
}

func (s *Store) ClaimOutcomeUnknownPayouts(
	ctx context.Context,
	lease time.Duration,
	retryWindow time.Duration,
	limit int,
) ([]DueHeldEntry, error) {
	if limit <= 0 || lease < 0 || retryWindow <= 0 {
		return nil, nil
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback(ctx)

	now := time.Now()
	rows, err := tx.Query(ctx, `
		SELECT op.ledger_entry_id,op.supplier_id,
		       op.requested_cents::float8 / 100.0,
		       settlement.liability_microusd,op.requested_cents,
		       settlement.remainder_microusd,settlement.policy,op.currency
		  FROM supplier_payout_operations op
		  JOIN ledger_entries le ON le.id=op.ledger_entry_id
		  JOIN supplier_minor_unit_settlements settlement
		    ON settlement.ledger_entry_id=le.id
		 WHERE op.outcome_unknown=true AND NOT op.cash_moved
		   AND op.transfer_ref IS NULL
		   AND op.status IN ('outcome_unknown','reversal_required')
		   AND le.payout_status IN ('outcome_unknown','reversal_required')
		   AND op.updated_at <= $1 AND op.created_at >= $2
		 ORDER BY op.updated_at,op.ledger_entry_id
		 FOR UPDATE OF op SKIP LOCKED
		 LIMIT $3`, now.Add(-lease), now.Add(-retryWindow), limit)
	if err != nil {
		return nil, err
	}
	var out []DueHeldEntry
	for rows.Next() {
		var e DueHeldEntry
		if err := rows.Scan(&e.ID, &e.SupplierID, &e.AmountUSD,
			&e.LiabilityMicros, &e.RequestedCents, &e.RemainderMicros,
			&e.SettlementPolicy, &e.Currency); err != nil {
			rows.Close()
			return nil, err
		}
		out = append(out, e)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}
	for _, e := range out {
		if _, err := tx.Exec(ctx, `
			UPDATE supplier_payout_operations SET updated_at=$2
			 WHERE ledger_entry_id=$1 AND outcome_unknown=true
			   AND NOT cash_moved AND transfer_ref IS NULL`, e.ID, now); err != nil {
			return nil, err
		}
	}
	if err := tx.Commit(ctx); err != nil {
		return nil, err
	}
	return out, nil
}

func (s *Store) MarkPayoutOutcomeUnknown(ctx context.Context, entryID uuid.UUID, cause error) (string, error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return "", err
	}
	defer tx.Rollback(ctx)
	var ledgerStatus, opStatus string
	var cashMoved bool
	var transferRef *string
	if err := tx.QueryRow(ctx, `
		SELECT le.payout_status,op.status,op.cash_moved,op.transfer_ref
		  FROM ledger_entries le
		  JOIN supplier_payout_operations op ON op.ledger_entry_id=le.id
		 WHERE le.id=$1 FOR UPDATE OF le,op`, entryID,
	).Scan(&ledgerStatus, &opStatus, &cashMoved, &transferRef); err != nil {
		return "", err
	}
	if cashMoved || transferRef != nil {
		return "", fmt.Errorf("payout %s already has provider cash evidence", entryID)
	}
	if ledgerStatus != PayoutSending && ledgerStatus != PayoutOutcomeUnknown &&
		ledgerStatus != PayoutReversalRequired {
		return ledgerStatus, fmt.Errorf("payout %s cannot become outcome_unknown from ledger=%s operation=%s",
			entryID, ledgerStatus, opStatus)
	}
	errText := "provider outcome unknown"
	if cause != nil {
		errText = truncate(cause.Error(), 500)
	}
	next := PayoutOutcomeUnknown
	if ledgerStatus == PayoutReversalRequired || opStatus == PayoutReversalRequired {
		next = PayoutReversalRequired
	}
	if _, err := tx.Exec(ctx, `
		UPDATE supplier_payout_operations
		   SET status=$2,outcome_unknown=true,last_error=$3,updated_at=now()
		 WHERE ledger_entry_id=$1 AND NOT cash_moved AND transfer_ref IS NULL`,
		entryID, next, errText); err != nil {
		return "", err
	}
	if _, err := tx.Exec(ctx,
		`UPDATE ledger_entries SET payout_status=$2 WHERE id=$1`, entryID, next); err != nil {
		return "", err
	}
	if err := tx.Commit(ctx); err != nil {
		return "", err
	}
	return next, nil
}

func (s *Store) DeferPayout(ctx context.Context, entryID uuid.UUID, cause error) (string, error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return "", err
	}
	defer tx.Rollback(ctx)
	var status string
	var outcomeUnknown bool
	if err := tx.QueryRow(ctx, `
		SELECT le.payout_status,COALESCE(op.outcome_unknown,false)
		  FROM ledger_entries le
		  LEFT JOIN supplier_payout_operations op ON op.ledger_entry_id=le.id
		 WHERE le.id=$1 FOR UPDATE OF le`, entryID).Scan(&status, &outcomeUnknown); err != nil {
		return "", err
	}
	if outcomeUnknown {
		return status, fmt.Errorf("payout %s has an unresolved provider outcome and cannot be deferred to ready", entryID)
	}
	if status != PayoutSending {
		return status, tx.Commit(ctx)
	}
	errText := ""
	if cause != nil {
		errText = truncate(cause.Error(), 500)
	}
	if _, err := tx.Exec(ctx,
		`UPDATE supplier_payout_operations
		    SET status='ready',last_error=NULLIF($2,''),updated_at=now()
		  WHERE ledger_entry_id=$1 AND status='sending'`, entryID, errText); err != nil {
		return "", err
	}
	if _, err := tx.Exec(ctx,
		`UPDATE ledger_entries SET payout_status=$2 WHERE id=$1 AND payout_status=$3`,
		entryID, PayoutReady, PayoutSending); err != nil {
		return "", err
	}
	if err := tx.Commit(ctx); err != nil {
		return "", err
	}
	return PayoutReady, nil
}

func (s *Store) FinalizePayout(ctx context.Context, entryID uuid.UUID, result PayoutResult) (string, error) {
	if strings.TrimSpace(result.Ref) == "" {
		return "", errors.New("payout result has no durable reference")
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return "", err
	}
	defer tx.Rollback(ctx)

	var ledgerStatus, opStatus, currency, fundingCurrency, settlementPolicy string
	var requested, fundingAmount, liabilityMicros, settlementCash, remainderMicros, carryInMicros int64
	err = tx.QueryRow(ctx, `
		SELECT le.payout_status,op.status,op.requested_cents,op.currency,
		       funding.amount_cents,funding.currency,
		       (le.amount_usd*1000000)::bigint,settlement.policy,
		       settlement.cash_cents,settlement.remainder_microusd,settlement.carry_in_microusd
		  FROM ledger_entries le
		  JOIN supplier_payout_operations op ON op.ledger_entry_id=le.id
		  JOIN supplier_payout_funding funding
		    ON funding.id=op.funding_id AND funding.ledger_entry_id=le.id
		  JOIN supplier_minor_unit_settlements settlement
		    ON settlement.ledger_entry_id=le.id
		 WHERE le.id=$1
		 FOR UPDATE OF le,op,funding`, entryID).
		Scan(&ledgerStatus, &opStatus, &requested, &currency, &fundingAmount, &fundingCurrency,
			&liabilityMicros, &settlementPolicy, &settlementCash, &remainderMicros, &carryInMicros)
	if err != nil {
		return "", err
	}
	if requested != fundingAmount || currency != fundingCurrency {
		return "", fmt.Errorf(
			"payout %s operation/funding mismatch: operation=%d %s funding=%d %s",
			entryID, requested, currency, fundingAmount, fundingCurrency)
	}
	if settlementPolicy != supplierSettlementPolicyAccountAccrualV2 ||
		requested != settlementCash ||
		carryInMicros+liabilityMicros != settlementCash*microUSDPerCent+remainderMicros {
		return "", fmt.Errorf(
			"payout %s minor-unit reconciliation mismatch: policy=%s liability=%d requested=%d settlement=%d remainder=%d",
			entryID, settlementPolicy, liabilityMicros, requested, settlementCash, remainderMicros)
	}
	if result.CashMoved {
		if result.SentCents != requested || result.Currency != currency {
			return "", fmt.Errorf(
				"payout %s cash mismatch: requested=%d %s sent=%d %s",
				entryID, requested, currency, result.SentCents, result.Currency)
		}
		finalStatus := PayoutReleased
		if ledgerStatus == PayoutReversalRequired || ledgerStatus == PayoutClawedBack ||
			opStatus == PayoutReversalRequired || opStatus == PayoutClawedBack {
			finalStatus = PayoutReversalRequired
		} else if !((ledgerStatus == PayoutSending && opStatus == PayoutSending) ||
			(ledgerStatus == PayoutOutcomeUnknown && opStatus == PayoutOutcomeUnknown)) {
			return "", fmt.Errorf("payout %s cannot complete from ledger=%s operation=%s", entryID, ledgerStatus, opStatus)
		}
		if _, err := tx.Exec(ctx, `
			UPDATE supplier_payout_operations
			   SET status=$2,sent_cents=$3,currency=$4,cash_moved=true,
			       outcome_unknown=false,transfer_ref=$5,last_error=NULL,updated_at=now()
			 WHERE ledger_entry_id=$1`,
			entryID, finalStatus, result.SentCents, result.Currency, result.Ref); err != nil {
			return "", err
		}
		if _, err := tx.Exec(ctx,
			`UPDATE ledger_entries SET payout_status=$2,payout_ref=$3 WHERE id=$1`,
			entryID, finalStatus, result.Ref); err != nil {
			return "", err
		}
		if err := tx.Commit(ctx); err != nil {
			return "", err
		}
		return finalStatus, nil
	}

	if ledgerStatus == PayoutReversalRequired || opStatus == PayoutReversalRequired {
		if _, err := tx.Exec(ctx, `
			UPDATE supplier_payout_operations
			   SET status='reversal_required',outcome_unknown=false,
			       transfer_ref=$2,last_error='manual export requires external cancellation',updated_at=now()
			 WHERE ledger_entry_id=$1`, entryID, result.Ref); err != nil {
			return "", err
		}
		if _, err := tx.Exec(ctx,
			`UPDATE ledger_entries SET payout_status='reversal_required',payout_ref=$2 WHERE id=$1`,
			entryID, result.Ref); err != nil {
			return "", err
		}
		if err := tx.Commit(ctx); err != nil {
			return "", err
		}
		return PayoutReversalRequired, nil
	}
	if !((ledgerStatus == PayoutSending && opStatus == PayoutSending) ||
		(ledgerStatus == PayoutOutcomeUnknown && opStatus == PayoutOutcomeUnknown)) {
		return "", fmt.Errorf("non-cash payout %s cannot complete from ledger=%s operation=%s", entryID, ledgerStatus, opStatus)
	}
	if _, err := tx.Exec(ctx, `
		UPDATE supplier_payout_operations
		   SET status='exported',cash_moved=false,outcome_unknown=false,
		       transfer_ref=$2,last_error=NULL,updated_at=now()
		 WHERE ledger_entry_id=$1`, entryID, result.Ref); err != nil {
		return "", err
	}
	if _, err := tx.Exec(ctx,
		`UPDATE ledger_entries SET payout_status=$2,payout_ref=$3 WHERE id=$1`,
		entryID, PayoutExported, result.Ref); err != nil {
		return "", err
	}
	if err := tx.Commit(ctx); err != nil {
		return "", err
	}
	return PayoutExported, nil
}

// CountReversalRequired returns how many ledger rows still need external
// recovery (pending or in-flight). Non-zero means the platform must not send
// new supplier payouts.

func (s *Store) MarkPayout(ctx context.Context, entryID uuid.UUID, status, ref string) error {
	if status == PayoutReleased && ref == "" {
		return fmt.Errorf("refusing to mark ledger entry %s 'released' without a payout reference", entryID)
	}
	switch status {
	case PayoutReady:
		_, err := s.DeferPayout(ctx, entryID, nil)
		return err
	case PayoutReleased:
		var cents int64
		var currency string
		if err := s.pool.QueryRow(ctx,
			`SELECT requested_cents,currency FROM supplier_payout_operations WHERE ledger_entry_id=$1`,
			entryID).Scan(&cents, &currency); err != nil {
			return err
		}
		_, err := s.FinalizePayout(ctx, entryID, PayoutResult{
			Ref: ref, SentCents: cents, Currency: currency, CashMoved: true,
		})
		return err
	default:
		return fmt.Errorf("unsupported payout transition to %q; use the durable payout operation", status)
	}
}
