package main

import (
	"context"
	_ "embed"
	"encoding/json"
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

func isActiveDisputeUniqueViolation(err error) bool {
	var pgErr *pgconn.PgError
	return errors.As(err, &pgErr) && pgErr.Code == "23505" &&
		pgErr.ConstraintName == "disputes_one_active_job_uniq"
}

func appendDisputeEventTx(
	ctx context.Context,
	tx pgx.Tx,
	disputeID, jobID uuid.UUID,
	event string,
	detail []byte,
) error {
	if len(detail) == 0 {
		detail = []byte(`{}`)
	}
	_, err := tx.Exec(ctx, `
		INSERT INTO dispute_events (dispute_id,job_id,event,detail)
		VALUES ($1,$2,$3,$4)`, disputeID, jobID, event, detail)
	return err
}

// RecordDispute is the single filing boundary.  The parent job lock is shared
// with ClaimPayout, so the active-case row and every affected supplier-credit
// hold become visible atomically before any later payout claim can proceed.

// RecordDispute is the single filing boundary.  The parent job lock is shared
// with ClaimPayout, so the active-case row and every affected supplier-credit
// hold become visible atomically before any later payout claim can proceed.
func (s *Store) RecordDispute(ctx context.Context, jobID, buyerID uuid.UUID, reason string) (uuid.UUID, error) {
	reason = strings.TrimSpace(reason)
	if reason == "" {
		return uuid.Nil, errDisputeReasonRequired
	}
	if len([]rune(reason)) > maxDisputeReasonRunes {
		return uuid.Nil, errDisputeReasonTooLong
	}

	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return uuid.Nil, err
	}
	defer tx.Rollback(ctx)

	var status string
	var terminalAt *time.Time
	var now time.Time
	err = tx.QueryRow(ctx, `
		SELECT status,terminal_at,now()
		  FROM jobs WHERE id=$1 AND buyer_id=$2 FOR UPDATE`, jobID, buyerID,
	).Scan(&status, &terminalAt, &now)
	if errors.Is(err, pgx.ErrNoRows) {
		return uuid.Nil, errNotFound
	}
	if err != nil {
		return uuid.Nil, err
	}
	if status != "complete" && status != "failed" && status != "cancelled" {
		return uuid.Nil, errDisputeJobNotTerminal
	}
	if terminalAt == nil {
		return uuid.Nil, fmt.Errorf("terminal job %s has no terminal timestamp", jobID)
	}
	filingDeadline := terminalAt.Add(disputeFilingWindow)
	if now.After(filingDeadline) {
		return uuid.Nil, errDisputeWindowClosed
	}

	disputeID := uuid.New()
	_, err = tx.Exec(ctx, `
		INSERT INTO disputes
		  (id,job_id,buyer_id,reason,status,created_at,filing_deadline)
		VALUES ($1,$2,$3,$4,'open',$5,$6)`,
		disputeID, jobID, buyerID, reason, now, filingDeadline)
	if isActiveDisputeUniqueViolation(err) {
		return uuid.Nil, errDisputeAlreadyActive
	}
	if err != nil {
		return uuid.Nil, err
	}

	type frozenCredit struct {
		id     uuid.UUID
		status string
	}
	rows, err := tx.Query(ctx, `
		SELECT le.id,le.payout_status
		  FROM ledger_entries le JOIN tasks t ON t.id=le.task_id
		 WHERE t.job_id=$1 AND le.kind='supplier_credit'
		   AND le.payout_status NOT IN ('released','exported','clawed_back','reversal_required')
		 ORDER BY le.id FOR UPDATE OF le`, jobID)
	if err != nil {
		return uuid.Nil, err
	}
	var credits []frozenCredit
	for rows.Next() {
		var credit frozenCredit
		if err := rows.Scan(&credit.id, &credit.status); err != nil {
			rows.Close()
			return uuid.Nil, err
		}
		credits = append(credits, credit)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return uuid.Nil, err
	}
	for _, credit := range credits {
		if _, err := tx.Exec(ctx, `
			INSERT INTO dispute_payout_holds
			  (dispute_id,ledger_entry_id,payout_status_at_filing)
			VALUES ($1,$2,$3)`, disputeID, credit.id, credit.status); err != nil {
			return uuid.Nil, err
		}
		// A claim which linearized just before filing may already be at the
		// provider boundary.  Preserve it as recovery-required; FinalizePayout
		// will never represent resulting cash as an ordinary release.
		if credit.status == PayoutSending || credit.status == PayoutOutcomeUnknown {
			if _, err := tx.Exec(ctx, `
				UPDATE ledger_entries SET payout_status='reversal_required'
				 WHERE id=$1`, credit.id); err != nil {
				return uuid.Nil, err
			}
			if _, err := tx.Exec(ctx, `
				UPDATE supplier_payout_operations
				   SET status='reversal_required',updated_at=now(),
				       last_error='buyer dispute filed while payout was in flight'
				 WHERE ledger_entry_id=$1`, credit.id); err != nil {
				return uuid.Nil, err
			}
		}
	}

	detail, _ := json.Marshal(map[string]any{
		"dispute_id": disputeID, "frozen_supplier_credits": len(credits),
		"filing_deadline": filingDeadline,
	})
	if err := appendDisputeEventTx(ctx, tx, disputeID, jobID, "filed", detail); err != nil {
		return uuid.Nil, err
	}
	if err := insertEventTx(ctx, tx, jobID, nil, "dispute_filed",
		"Buyer filed a dispute; supplier payouts are frozen pending resolution", detail); err != nil {
		return uuid.Nil, err
	}
	if err := tx.Commit(ctx); err != nil {
		if isActiveDisputeUniqueViolation(err) {
			return uuid.Nil, errDisputeAlreadyActive
		}
		return uuid.Nil, err
	}
	return disputeID, nil
}

type DisputeRow struct {
	ID, JobID uuid.UUID
	Status    string
	// NoPeerAttempts is populated for no_peer rows so the sweep can promote to
	// the operator queue without a second round-trip. Zero for other statuses.
	NoPeerAttempts int
	FirstNoPeerAt  *time.Time
}

// noPeerDisputeMaxAttempts is how many failed peer searches a no_peer dispute
// may accumulate before becoming unresolvable (operator queue). At the
// disputeInterval of 20s this is ~10 minutes of continuous re-sweeping.
const noPeerDisputeMaxAttempts = 30

// noPeerDisputeMaxAge is the wall-clock bound: even if sweeps are sparse, a
// dispute that has sat with no peer for this long joins the operator queue
// rather than livelocking forever on a one- or two-worker network.
const noPeerDisputeMaxAge = 30 * time.Minute

func (s *Store) ActiveDisputes(ctx context.Context, limit int) ([]DisputeRow, error) {
	rows, err := s.pool.Query(ctx,
		`SELECT id, job_id, status, no_peer_attempts, first_no_peer_at
		   FROM disputes
		  WHERE status IN ('open','no_peer','reverifying') ORDER BY created_at LIMIT $1`, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []DisputeRow
	for rows.Next() {
		var d DisputeRow
		if err := rows.Scan(&d.ID, &d.JobID, &d.Status, &d.NoPeerAttempts, &d.FirstNoPeerAt); err != nil {
			return nil, err
		}
		out = append(out, d)
	}
	return out, rows.Err()
}

func (s *Store) SetDisputeReverifying(ctx context.Context, id, reverifyTaskID uuid.UUID) error {
	return s.setActiveDisputeStatus(ctx, id, "reverifying", &reverifyTaskID)
}

func (s *Store) SetDisputeStatus(ctx context.Context, id uuid.UUID, status string) error {
	if status == "resolved" {
		status = "upheld"
	}
	if status == "upheld" || status == "rejected" {
		return s.resolveDispute(ctx, id, status)
	}
	if status != "no_peer" && status != "unresolvable" {
		return fmt.Errorf("unsupported dispute status %q", status)
	}
	return s.setActiveDisputeStatus(ctx, id, status, nil)
}

// NoteDisputeNoPeer records one failed peer search for an active dispute.
// After noPeerDisputeMaxAttempts or noPeerDisputeMaxAge the dispute moves to
// unresolvable (operator queue) with the attempt count and reason in evidence.
// Returns the status after the note (no_peer or unresolvable).
func (s *Store) NoteDisputeNoPeer(ctx context.Context, id uuid.UUID) (string, error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return "", err
	}
	defer tx.Rollback(ctx)

	var jobID uuid.UUID
	if err := tx.QueryRow(ctx, `SELECT job_id FROM disputes WHERE id=$1`, id).Scan(&jobID); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return "", errNotFound
		}
		return "", err
	}
	if err := tx.QueryRow(ctx, `SELECT id FROM jobs WHERE id=$1 FOR UPDATE`, jobID).Scan(&jobID); err != nil {
		return "", err
	}
	var before string
	var attempts int
	var firstNoPeer *time.Time
	var createdAt time.Time
	if err := tx.QueryRow(ctx, `
		SELECT status, no_peer_attempts, first_no_peer_at, created_at
		  FROM disputes WHERE id=$1 FOR UPDATE`, id).Scan(
		&before, &attempts, &firstNoPeer, &createdAt); err != nil {
		return "", err
	}
	if before == "upheld" || before == "rejected" || before == "unresolvable" {
		return before, tx.Commit(ctx)
	}
	if before != "open" && before != "no_peer" && before != "reverifying" {
		return "", fmt.Errorf("dispute %s cannot record no_peer from %s", id, before)
	}

	attempts++
	now := time.Now().UTC()
	if firstNoPeer == nil {
		firstNoPeer = &now
	}
	ageStart := *firstNoPeer
	promote := attempts >= noPeerDisputeMaxAttempts || now.Sub(ageStart) >= noPeerDisputeMaxAge
	to := "no_peer"
	reason := "no_redundancy_peer_available"
	if promote {
		to = "unresolvable"
		if attempts >= noPeerDisputeMaxAttempts {
			reason = "no_peer_attempt_bound_exhausted"
		} else {
			reason = "no_peer_age_bound_exhausted"
		}
	}

	if _, err := tx.Exec(ctx, `
		UPDATE disputes
		   SET status=$2,
		       no_peer_attempts=$3,
		       first_no_peer_at=COALESCE(first_no_peer_at,$4)
		 WHERE id=$1`, id, to, attempts, *firstNoPeer); err != nil {
		return "", err
	}
	detail, _ := json.Marshal(map[string]any{
		"from": before, "to": to,
		"no_peer_attempts": attempts,
		"first_no_peer_at": firstNoPeer.UTC().Format(time.RFC3339Nano),
		"reason":           reason,
		"max_attempts":     noPeerDisputeMaxAttempts,
		"max_age_secs":     int(noPeerDisputeMaxAge.Seconds()),
	})
	if err := appendDisputeEventTx(ctx, tx, id, jobID, to, detail); err != nil {
		return "", err
	}
	buyerText := "Dispute remains open pending independent review: no re-verification peer is available"
	if to == "unresolvable" {
		buyerText = "Dispute moved to operator review: no re-verification peer became available within the bound"
	}
	if err := insertEventTx(ctx, tx, jobID, nil, "dispute_"+to, buyerText, detail); err != nil {
		return "", err
	}
	if err := tx.Commit(ctx); err != nil {
		return "", err
	}
	return to, nil
}

func (s *Store) setActiveDisputeStatus(
	ctx context.Context,
	id uuid.UUID,
	status string,
	reverifyTaskID *uuid.UUID,
) error {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	var jobID uuid.UUID
	if err := tx.QueryRow(ctx, `SELECT job_id FROM disputes WHERE id=$1`, id).Scan(&jobID); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return errNotFound
		}
		return err
	}
	if err := tx.QueryRow(ctx, `SELECT id FROM jobs WHERE id=$1 FOR UPDATE`, jobID).Scan(&jobID); err != nil {
		return err
	}
	var before string
	if err := tx.QueryRow(ctx, `SELECT status FROM disputes WHERE id=$1 FOR UPDATE`, id).Scan(&before); err != nil {
		return err
	}
	if before == status {
		return tx.Commit(ctx)
	}
	if before != "open" && before != "no_peer" && before != "reverifying" {
		return fmt.Errorf("dispute %s cannot transition from %s to %s", id, before, status)
	}
	if status == "reverifying" && reverifyTaskID == nil {
		return errors.New("reverifying dispute requires a task id")
	}
	if _, err := tx.Exec(ctx, `
		UPDATE disputes SET status=$2,reverify_task_id=COALESCE($3,reverify_task_id)
		 WHERE id=$1`, id, status, reverifyTaskID); err != nil {
		return err
	}
	detail, _ := json.Marshal(map[string]any{"from": before, "to": status, "reverify_task_id": reverifyTaskID})
	if err := appendDisputeEventTx(ctx, tx, id, jobID, status, detail); err != nil {
		return err
	}
	buyerText := "Dispute remains open pending independent review"
	if status == "reverifying" {
		buyerText = "Dispute: independent re-verification dispatched"
	} else if status == "unresolvable" {
		buyerText = "Dispute remains open: independent resolution is currently unavailable; an operator must decide"
	}
	if err := insertEventTx(ctx, tx, jobID, nil, "dispute_"+status, buyerText, detail); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

type disputeResolveResult struct {
	Before               string
	Resolution           string
	JobID                uuid.UUID
	ClawedCredits        int64
	BuyerRefundMicros    int64
	PlatformRefundMicros int64
	TasksRefunded        int64
	FundingDestination   string
	Currency             string
	MoneyEffect          string
}

func (s *Store) resolveDispute(ctx context.Context, id uuid.UUID, resolution string) error {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	if _, err := resolveDisputeInTx(ctx, tx, id, resolution); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

// ResolveDisputeTx is the operator exit for a terminal (unresolvable) dispute.
// It records who decided, on what basis, and what happened to the money in
// admin_actions, then applies the same upheld/rejected money effects as the
// automatic re-verification path. It never chooses a side without the human
// resolution argument.
func (s *Store) ResolveDisputeTx(
	ctx context.Context,
	actor AdminActor,
	disputeID uuid.UUID,
	resolution, reason, correlationRef string,
) error {
	resolution = strings.TrimSpace(resolution)
	if resolution != "upheld" && resolution != "rejected" {
		return errDisputeResolutionInvalid
	}
	intent, err := prepareAdminMutation(actor, adminMutationIntent{
		Kind: adminActionDisputeResolved, TargetKind: adminTargetDispute,
		TargetID: disputeID, Reason: reason, CorrelationRef: correlationRef,
		Resolution: resolution,
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

	var before string
	var jobID uuid.UUID
	if err := tx.QueryRow(ctx, `
		SELECT status, job_id FROM disputes WHERE id=$1`, disputeID).Scan(&before, &jobID); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return errNotFound
		}
		return err
	}
	if before == "upheld" || before == "rejected" {
		return errDisputeAlreadyResolved
	}
	// Operator queue only. Automatic re-verification still owns the active
	// sweep states; unresolvable is exactly the case where a human chooses.
	if before != "unresolvable" {
		return errDisputeNotOperatorQueue
	}

	result, err := resolveDisputeInTx(ctx, tx, disputeID, resolution)
	if err != nil {
		return err
	}
	moneyEffect := "held_supplier_credits_eligible_again"
	if resolution == "upheld" {
		moneyEffect = "supplier_credits_clawed_back_buyer_refunded"
	}
	beforeState := map[string]any{
		"status": before, "job_id": jobID.String(),
	}
	afterState := map[string]any{
		"status":                 result.Resolution,
		"job_id":                 result.JobID.String(),
		"money_effect":           moneyEffect,
		"new_clawback_entries":   result.ClawedCredits,
		"buyer_refund_micros":    result.BuyerRefundMicros,
		"platform_refund_micros": result.PlatformRefundMicros,
		"tasks_refunded":         result.TasksRefunded,
		"funding_destination":    result.FundingDestination,
		"currency":               result.Currency,
		"basis":                  reason,
	}
	if err := insertAdminMutationAction(ctx, tx, actor, intent, nil, nil, nil,
		beforeState, afterState); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

func resolveDisputeInTx(
	ctx context.Context,
	tx pgx.Tx,
	id uuid.UUID,
	resolution string,
) (disputeResolveResult, error) {
	var out disputeResolveResult
	out.Resolution = resolution
	if resolution != "upheld" && resolution != "rejected" {
		return out, errDisputeResolutionInvalid
	}

	var jobID uuid.UUID
	if err := tx.QueryRow(ctx, `SELECT job_id FROM disputes WHERE id=$1`, id).Scan(&jobID); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return out, errNotFound
		}
		return out, err
	}
	if err := tx.QueryRow(ctx, `SELECT id FROM jobs WHERE id=$1 FOR UPDATE`, jobID).Scan(&jobID); err != nil {
		return out, err
	}
	var before string
	if err := tx.QueryRow(ctx, `SELECT status FROM disputes WHERE id=$1 FOR UPDATE`, id).Scan(&before); err != nil {
		return out, err
	}
	out.Before = before
	out.JobID = jobID
	if before == resolution {
		// Idempotent commit path for automatic retries.
		return out, nil
	}
	if before == "upheld" || before == "rejected" {
		return out, fmt.Errorf("%w: dispute %s is already resolved as %s", errDisputeAlreadyResolved, id, before)
	}

	if _, err := tx.Exec(ctx, `
		UPDATE disputes SET status=$2,resolved_at=now() WHERE id=$1`, id, resolution); err != nil {
		return out, err
	}
	clawedCredits := int64(0)
	var refundResult disputeBuyerRefundResult
	var fundingDestination string
	if resolution == "upheld" {
		// Append a balancing liability effect for every task credit.  Existing
		// verification clawbacks are idempotently retained by the task/kind key.
		ct, err := insertJobDisputeClawbacksTx(ctx, tx, jobID)
		if err != nil {
			return out, err
		}
		clawedCredits = ct.RowsAffected()
		if _, err := tx.Exec(ctx, `
			UPDATE ledger_entries le
			   SET payout_status=CASE
			       WHEN le.payout_ref IS NOT NULL
			         OR le.payout_status IN ('sending','outcome_unknown','released','exported','reversal_required')
			         OR EXISTS (SELECT 1 FROM supplier_payout_operations op
			                      WHERE op.ledger_entry_id=le.id AND op.outcome_unknown)
			       THEN 'reversal_required' ELSE 'clawed_back' END
			  FROM tasks t
			 WHERE le.task_id=t.id AND t.job_id=$1 AND le.kind='supplier_credit'
			   AND le.payout_status NOT IN ('clawed_back','reversal_required')`, jobID); err != nil {
			return out, err
		}
		if _, err := tx.Exec(ctx, `
			UPDATE supplier_payout_operations op
			   SET status=le.payout_status,updated_at=now(),
			       last_error=CASE WHEN le.payout_status='reversal_required'
			         THEN 'upheld buyer dispute requires external recovery' ELSE op.last_error END
			  FROM ledger_entries le JOIN tasks t ON t.id=le.task_id
			 WHERE op.ledger_entry_id=le.id AND t.job_id=$1`, jobID); err != nil {
			return out, err
		}
		if _, err := tx.Exec(ctx, `
			UPDATE tasks SET verification_outcome='clawed_back',verified_at=now()
			 WHERE job_id=$1 AND id IN (
			   SELECT task_id FROM ledger_entries
			    WHERE kind='supplier_credit' AND task_id IS NOT NULL)`, jobID); err != nil {
			return out, err
		}

		// Credit the buyer. Ledger buyer_refund rows feed every balance formula
		// that already subtracts kind IN ('buyer_charge','buyer_refund'). Funding
		// destination decides whether prepaid liability is restored or a card
		// refund is left as an external settlement step.
		refundResult, err = insertJobDisputeBuyerRefundsTx(ctx, tx, jobID, id)
		if err != nil {
			return out, err
		}
		fundingDestination, err = applyDisputeBuyerRefundFundingTx(ctx, tx, jobID, id, refundResult)
		if err != nil {
			return out, err
		}
		if refundResult.BuyerRefundMicros > 0 {
			// Bind reserve consumption to the exact dispute-keyed refund rows while
			// the job/dispute locks are still held. Neither side can commit alone.
			if err := consumeRiskReserveForDisputeRefundTx(ctx, tx, jobID, id); err != nil {
				return out, fmt.Errorf("consume risk reserve for upheld dispute %s: %w", id, err)
			}
		}
	}
	if _, err := tx.Exec(ctx, `
		UPDATE dispute_payout_holds
		   SET resolution=$2,resolved_at=now()
		 WHERE dispute_id=$1 AND resolution IS NULL`, id, resolution); err != nil {
		return out, err
	}
	detail, _ := json.Marshal(map[string]any{
		"from": before, "resolution": resolution, "new_clawback_entries": clawedCredits,
		"buyer_refund_micros":    refundResult.BuyerRefundMicros,
		"platform_refund_micros": refundResult.PlatformRefundMicros,
		"tasks_refunded":         refundResult.TasksRefunded,
		"funding_destination":    fundingDestination,
		"currency":               refundResult.Currency,
	})
	if err := appendDisputeEventTx(ctx, tx, id, jobID, resolution, detail); err != nil {
		return out, err
	}
	buyerText := "Dispute rejected: independent re-verification agreed with the original result; held payouts are eligible again"
	if resolution == "upheld" {
		buyerText = disputeUpheldBuyerText(refundResult, fundingDestination)
	}
	if err := insertEventTx(ctx, tx, jobID, nil, "dispute_"+resolution, buyerText, detail); err != nil {
		return out, err
	}
	out.ClawedCredits = clawedCredits
	out.BuyerRefundMicros = refundResult.BuyerRefundMicros
	out.PlatformRefundMicros = refundResult.PlatformRefundMicros
	out.TasksRefunded = refundResult.TasksRefunded
	out.FundingDestination = fundingDestination
	out.Currency = refundResult.Currency
	if resolution == "upheld" {
		out.MoneyEffect = "supplier_credits_clawed_back_buyer_refunded"
	} else {
		out.MoneyEffect = "held_supplier_credits_eligible_again"
	}
	return out, nil
}

func disputeUpheldBuyerText(refund disputeBuyerRefundResult, funding string) string {
	if refund.BuyerRefundMicros <= 0 {
		return "Dispute upheld: supplier liabilities were clawed back"
	}
	amount := microsToUSD(refund.BuyerRefundMicros)
	switch funding {
	case refundFundingPrepaidBalance:
		return fmt.Sprintf(
			"Dispute upheld: $%.6f refunded to your prepaid balance (supplier liabilities clawed back)",
			amount)
	case refundFundingExternalCardPending:
		return fmt.Sprintf(
			"Dispute upheld: $%.6f buyer refund recorded; card refund is pending external settlement (supplier liabilities clawed back)",
			amount)
	default:
		return fmt.Sprintf(
			"Dispute upheld: $%.6f buyer refund recorded on the ledger (supplier liabilities clawed back)",
			amount)
	}
}

// applyDisputeBuyerRefundFundingTx materialises the funding side-effect of a
// dispute buyer refund and records an append-only job_dispute_refunds receipt.
//
// Rules (explicit, receipted):
//   - prepaid_debit present on any refunded task → restore that liability to
//     the buyer's prepaid balance (internal; no Stripe).
//   - else job has a charged card collection → ledger credit only;
//     external_cash_state=NOT_REQUESTED (Stripe refund is a separate step).
//   - else ledger_only (free credit / never card-charged).
//
// Idempotent on dispute_id: a concurrent or retried resolution that already
// wrote the receipt is a no-op for funding.
func applyDisputeBuyerRefundFundingTx(
	ctx context.Context,
	tx pgx.Tx,
	jobID, disputeID uuid.UUID,
	refund disputeBuyerRefundResult,
) (string, error) {
	if refund.BuyerRefundMicros < 0 {
		return "", fmt.Errorf("job %s dispute refund amount is negative", jobID)
	}

	var buyerID uuid.UUID
	var chargeStatus string
	var stripePI *string
	if err := tx.QueryRow(ctx, `
		SELECT buyer_id, charge_status, stripe_pi FROM jobs WHERE id=$1`, jobID).
		Scan(&buyerID, &chargeStatus, &stripePI); err != nil {
		return "", err
	}

	// Prepaid restore amount: sum of prepaid_debit abs amounts on tasks that
	// received a buyer_refund. Fail closed if debit and refund disagree.
	var prepaidRestoreMicros int64
	var prepaidMismatch bool
	if err := tx.QueryRow(ctx, `
		SELECT COALESCE(SUM((-d.amount_usd) * 1000000)::bigint, 0),
		       EXISTS (
		         SELECT 1
		           FROM ledger_entries d
		           JOIN tasks t ON t.id = d.task_id
		           JOIN ledger_entries r ON r.task_id = d.task_id AND r.kind = 'buyer_refund'
		          WHERE t.job_id = $1 AND d.kind = 'prepaid_debit'
		            AND (-d.amount_usd) IS DISTINCT FROM r.amount_usd
		       )
		  FROM ledger_entries d
		  JOIN tasks t ON t.id = d.task_id
		  JOIN ledger_entries r ON r.task_id = d.task_id AND r.kind = 'buyer_refund'
		 WHERE t.job_id = $1 AND d.kind = 'prepaid_debit'`, jobID).
		Scan(&prepaidRestoreMicros, &prepaidMismatch); err != nil {
		return "", err
	}
	if prepaidMismatch {
		return "", fmt.Errorf("job %s prepaid_debit does not match buyer_refund; refusing funding restore", jobID)
	}

	var hasCardCollection bool
	if err := tx.QueryRow(ctx, `
		SELECT EXISTS (
		  SELECT 1 FROM buyer_cash_collections
		   WHERE job_id = $1 AND source_kind IN ('job','batch') AND received_cents > 0
		) OR (
		  SELECT charge_status = 'charged' AND COALESCE(stripe_pi,'') <> ''
		    FROM jobs WHERE id = $1
		)`, jobID).Scan(&hasCardCollection); err != nil {
		return "", err
	}

	funding := refundFundingLedgerOnly
	externalCash := "NOT_APPLICABLE"
	switch {
	case prepaidRestoreMicros > 0:
		funding = refundFundingPrepaidBalance
		externalCash = "NOT_APPLICABLE"
		// KindPrepaidRestore per refunded task (idempotent on task debit). Bare
		// creditPrepaidBalanceTx without a restore receipt leaves prepaidDebited
		// counting the original debit after buyer_refund zeros spent — phantom
		// realtime capacity of exactly D, mintable again on re-entry.
		if err := restorePrepaidForDisputeJobTasksTx(ctx, tx, buyerID, jobID, refund.Currency); err != nil {
			return "", err
		}
	case refund.BuyerRefundMicros > 0 && hasCardCollection:
		funding = refundFundingExternalCardPending
		externalCash = "NOT_REQUESTED"
	case refund.BuyerRefundMicros > 0:
		funding = refundFundingLedgerOnly
		externalCash = "NOT_APPLICABLE"
	}

	// Append-only receipt. UNIQUE(dispute_id) makes concurrent resolution of
	// the same dispute write funding effects at most once (second insert is a
	// no-op after the first committed under the dispute row lock).
	currency := refund.Currency
	if currency == "" {
		currency = "usd"
	}
	// Only receipt a funding row when there is a buyer-visible refund. Pure
	// clawback-without-charge cases (legacy fixtures) keep the clawback path
	// but do not invent a zero-dollar refund receipt.
	if refund.BuyerRefundMicros > 0 || refund.PlatformRefundMicros > 0 {
		if _, err := tx.Exec(ctx, `
			INSERT INTO job_dispute_refunds
			  (dispute_id, job_id, buyer_id, currency,
			   buyer_refund_usd, platform_refund_usd, supplier_clawback_usd,
			   funding_destination, external_cash_state, reason_code)
			VALUES (
			  $1, $2, $3, $4,
			  ($5::numeric / 1000000),
			  ($6::numeric / 1000000),
			  COALESCE((
			    SELECT SUM(-le.amount_usd)
			      FROM ledger_entries le JOIN tasks t ON t.id = le.task_id
			     WHERE t.job_id = $2 AND le.kind = 'clawback'
			  ), 0),
			  $7, $8, 'DISPUTE_UPHELD'
			)
			ON CONFLICT (dispute_id) DO NOTHING`,
			disputeID, jobID, buyerID, currency,
			refund.BuyerRefundMicros, refund.PlatformRefundMicros,
			funding, externalCash); err != nil {
			return "", err
		}
	}
	_ = chargeStatus
	_ = stripePI
	return funding, nil
}
