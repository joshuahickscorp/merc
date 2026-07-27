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
}

func (s *Store) ActiveDisputes(ctx context.Context, limit int) ([]DisputeRow, error) {
	rows, err := s.pool.Query(ctx,
		`SELECT id, job_id, status FROM disputes
		  WHERE status IN ('open','no_peer','reverifying') ORDER BY created_at LIMIT $1`, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []DisputeRow
	for rows.Next() {
		var d DisputeRow
		if err := rows.Scan(&d.ID, &d.JobID, &d.Status); err != nil {
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
		buyerText = "Dispute remains open: independent resolution is currently unavailable"
	}
	if err := insertEventTx(ctx, tx, jobID, nil, "dispute_"+status, buyerText, detail); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

func (s *Store) resolveDispute(ctx context.Context, id uuid.UUID, resolution string) error {
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
	if before == resolution {
		return tx.Commit(ctx)
	}
	if before == "upheld" || before == "rejected" {
		return fmt.Errorf("dispute %s is already resolved as %s", id, before)
	}

	if _, err := tx.Exec(ctx, `
		UPDATE disputes SET status=$2,resolved_at=now() WHERE id=$1`, id, resolution); err != nil {
		return err
	}
	clawedCredits := int64(0)
	if resolution == "upheld" {
		// Append a balancing liability effect for every task credit.  Existing
		// verification clawbacks are idempotently retained by the task/kind key.
		ct, err := insertJobDisputeClawbacksTx(ctx, tx, jobID)
		if err != nil {
			return err
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
			return err
		}
		if _, err := tx.Exec(ctx, `
			UPDATE supplier_payout_operations op
			   SET status=le.payout_status,updated_at=now(),
			       last_error=CASE WHEN le.payout_status='reversal_required'
			         THEN 'upheld buyer dispute requires external recovery' ELSE op.last_error END
			  FROM ledger_entries le JOIN tasks t ON t.id=le.task_id
			 WHERE op.ledger_entry_id=le.id AND t.job_id=$1`, jobID); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `
			UPDATE tasks SET verification_outcome='clawed_back',verified_at=now()
			 WHERE job_id=$1 AND id IN (
			   SELECT task_id FROM ledger_entries
			    WHERE kind='supplier_credit' AND task_id IS NOT NULL)`, jobID); err != nil {
			return err
		}
	}
	if _, err := tx.Exec(ctx, `
		UPDATE dispute_payout_holds
		   SET resolution=$2,resolved_at=now()
		 WHERE dispute_id=$1 AND resolution IS NULL`, id, resolution); err != nil {
		return err
	}
	detail, _ := json.Marshal(map[string]any{
		"from": before, "resolution": resolution, "new_clawback_entries": clawedCredits,
	})
	if err := appendDisputeEventTx(ctx, tx, id, jobID, resolution, detail); err != nil {
		return err
	}
	buyerText := "Dispute rejected: independent re-verification agreed with the original result; held payouts are eligible again"
	if resolution == "upheld" {
		buyerText = "Dispute upheld: supplier liabilities were clawed back"
	}
	if err := insertEventTx(ctx, tx, jobID, nil, "dispute_"+resolution, buyerText, detail); err != nil {
		return err
	}
	return tx.Commit(ctx)
}
