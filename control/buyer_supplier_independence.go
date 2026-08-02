package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

// Buyer–supplier link signals available in this schema (read, not assumed):
//
//  1. owner_buyer_id — suppliers.owner_buyer_id = jobs.buyer_id
//     (same owning account; set by EnsureSupplierForBuyer and the email-match
//     backfill in schema.sql). Primary hard link.
//  2. email_domain — lower(split_part(buyers.email,'@',2)) equals the same for
//     suppliers.email, excluding public free-mail domains where domain match is
//     not meaningful.
//  3. enrollment_device — worker_enrollment_codes binds buyer_id + supplier_id
//     (+ device_fingerprint) at enrollment. A supplier enrolled by buyer B is
//     linked to B even if owner_buyer_id was later cleared.
//  4. payout_instrument_owner — suppliers.stripe_acct is non-null AND the
//     supplier is already owner-linked (records that the funding path was
//     considered; no separate buyer-side instrument join exists to stripe_acct).
//
// Enrollment IP is NOT in the schema (no column stores it), so it is not used.
// billing_customers.default_payment_method has no join path to suppliers, so
// it is not used as a buyer–supplier link signal.
//
// Any hard link excludes the supplier from that buyer's ordinary work AND from
// verification work (redundancy, honeypot, tiebreak). Verification is "more
// strict" in that a job requiring verification with zero unlinked suppliers is
// refused entirely rather than settled as verified.

// publicEmailDomains are free-mail providers where same-domain is not a
// meaningful collusion signal (two strangers can both be @gmail.com).
var publicEmailDomains = map[string]bool{
	"gmail.com": true, "googlemail.com": true, "yahoo.com": true,
	"hotmail.com": true, "outlook.com": true, "live.com": true,
	"icloud.com": true, "me.com": true, "aol.com": true,
	"proton.me": true, "protonmail.com": true, "mail.com": true,
	"example.com": true, "example.invalid": true, "example.test": true,
	"invalid.example": true,
}

// errBuyerSupplierLinked is the greppable claim refusal for a linked supplier.
var errBuyerSupplierLinked = errors.New("BUYER_SUPPLIER_LINKED: supplier is linked to the buyer and cannot claim this work")

// errNoIndependentSupplier is the greppable refusal when a job that requires
// independent verification has no unlinked supplier available.
var errNoIndependentSupplier = errors.New("NO_INDEPENDENT_SUPPLIER: job cannot be independently verified; refusing rather than settling as verified")

// emailDomain returns the lowercased domain part of an email, or "".
func emailDomain(email string) string {
	email = strings.ToLower(strings.TrimSpace(email))
	at := strings.LastIndexByte(email, '@')
	if at < 0 || at+1 >= len(email) {
		return ""
	}
	return email[at+1:]
}

// domainIsMeaningful is true when a shared domain is a collusion signal.
func domainIsMeaningful(domain string) bool {
	if domain == "" {
		return false
	}
	return !publicEmailDomains[domain]
}

// buyerSupplierLinkSignals returns the non-empty set of link signal names that
// connect this supplier to this buyer. Empty means unlinked.
func (s *Store) buyerSupplierLinkSignals(ctx context.Context, buyerID, supplierID uuid.UUID) ([]string, error) {
	var (
		ownerBuyer                        *uuid.UUID
		buyerEmail, supplierEmail, stripe string
		enrolled                          bool
	)
	err := s.pool.QueryRow(ctx, `
		SELECT s.owner_buyer_id,
		       lower(b.email), lower(s.email),
		       COALESCE(s.stripe_acct,''),
		       EXISTS (
		         SELECT 1 FROM worker_enrollment_codes wec
		          WHERE wec.buyer_id = $1 AND wec.supplier_id = $2
		            AND wec.consumed_at IS NOT NULL
		       )
		  FROM suppliers s
		  JOIN buyers b ON b.id = $1
		 WHERE s.id = $2`,
		buyerID, supplierID,
	).Scan(&ownerBuyer, &buyerEmail, &supplierEmail, &stripe, &enrolled)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var signals []string
	if ownerBuyer != nil && *ownerBuyer == buyerID {
		signals = append(signals, "owner_buyer_id")
	}
	bd, sd := emailDomain(buyerEmail), emailDomain(supplierEmail)
	if bd != "" && bd == sd && domainIsMeaningful(bd) {
		signals = append(signals, "email_domain")
	}
	if enrolled {
		signals = append(signals, "enrollment_device")
	}
	if stripe != "" && ownerBuyer != nil && *ownerBuyer == buyerID {
		signals = append(signals, "payout_instrument_owner")
	}
	return signals, nil
}

// SupplierLinkedToBuyer is true when any link signal fires.
func (s *Store) SupplierLinkedToBuyer(ctx context.Context, buyerID, supplierID uuid.UUID) (bool, []string, error) {
	signals, err := s.buyerSupplierLinkSignals(ctx, buyerID, supplierID)
	if err != nil {
		return false, nil, err
	}
	return len(signals) > 0, signals, nil
}

// RecordClaimIndependenceExclusion persists a placement decision that a linked
// supplier was refused. Receipts and audits read this table.
func (s *Store) RecordClaimIndependenceExclusion(
	ctx context.Context,
	jobID, buyerID, supplierID uuid.UUID,
	workerID *uuid.UUID,
	taskID *uuid.UUID,
	kind string,
	signals []string,
	detail map[string]any,
) error {
	if kind == "" {
		kind = "buyer_work"
	}
	if detail == nil {
		detail = map[string]any{}
	}
	detailJSON, err := json.Marshal(detail)
	if err != nil {
		return err
	}
	if signals == nil {
		signals = []string{}
	}
	_, err = s.pool.Exec(ctx, `
		INSERT INTO claim_independence_exclusions
		  (job_id, task_id, buyer_id, supplier_id, worker_id, link_signals, exclusion_kind, detail)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8)`,
		jobID, taskID, buyerID, supplierID, workerID, signals, kind, detailJSON,
	)
	return err
}

// claimIndependenceSQL is the predicate spliced into ClaimTaskSQL: the claiming
// supplier must not be linked to the job's buyer. Applied to ALL tasks
// (ordinary + verification). Verification tasks get an additional permanent
// exclusion of prior executors already present in the claim SQL.
//
// Link signals evaluated in SQL (must match buyerSupplierLinkSignals):
//   - owner_buyer_id
//   - email_domain (non-public)
//   - enrollment_device (consumed enrollment code for this buyer+supplier)
const claimIndependenceSQL = `
	     -- Buyer–supplier independence (D6): a supplier owned by, enrolled by,
	     -- or sharing a meaningful email domain with the buyer cannot claim
	     -- that buyer's tasks — including redundancy/honeypot verification.
	     AND NOT (
	       me.supplier_id_s IN (
	         SELECT s_link.id FROM suppliers s_link
	          WHERE s_link.owner_buyer_id = j.buyer_id
	       )
	       OR EXISTS (
	         SELECT 1 FROM suppliers s_link
	           JOIN buyers b_link ON b_link.id = j.buyer_id
	          WHERE s_link.id = me.supplier_id_s
	            AND lower(split_part(s_link.email,'@',2)) = lower(split_part(b_link.email,'@',2))
	            AND lower(split_part(b_link.email,'@',2)) <> ''
	            AND lower(split_part(b_link.email,'@',2)) NOT IN (
	              'gmail.com','googlemail.com','yahoo.com','hotmail.com','outlook.com',
	              'live.com','icloud.com','me.com','aol.com','proton.me','protonmail.com',
	              'mail.com','example.com','example.invalid','example.test','invalid.example'
	            )
	       )
	       OR EXISTS (
	         SELECT 1 FROM worker_enrollment_codes wec
	          WHERE wec.buyer_id = j.buyer_id
	            AND wec.supplier_id = me.supplier_id_s
	            AND wec.consumed_at IS NOT NULL
	       )
	     )`

// CountIndependentSuppliersForBuyer returns how many active, online suppliers
// are not linked to this buyer. Used at submit and settlement to refuse rather
// than silently downgrade verification. Does not require a jobs row to exist
// (submit runs this before INSERT).
func (s *Store) CountIndependentSuppliersForBuyer(ctx context.Context, buyerID uuid.UUID) (int, error) {
	var n int
	err := s.pool.QueryRow(ctx, `
		SELECT count(DISTINCT w.supplier_id)
		  FROM workers w
		  JOIN suppliers s ON s.id = w.supplier_id
		 WHERE s.status = 'active'
		   AND w.last_seen_at IS NOT NULL
		   AND w.last_seen_at > now() - interval '60 seconds'
		   AND NOT COALESCE(w.throttled, false)
		   AND s.owner_buyer_id IS DISTINCT FROM $1
		   AND NOT EXISTS (
		     SELECT 1 FROM worker_enrollment_codes wec
		      WHERE wec.buyer_id = $1 AND wec.supplier_id = s.id
		        AND wec.consumed_at IS NOT NULL
		   )
		   AND NOT (
		     lower(split_part(s.email,'@',2)) = (
		       SELECT lower(split_part(b.email,'@',2)) FROM buyers b WHERE b.id = $1
		     )
		     AND lower(split_part(s.email,'@',2)) NOT IN (
		       'gmail.com','googlemail.com','yahoo.com','hotmail.com','outlook.com',
		       'live.com','icloud.com','me.com','aol.com','proton.me','protonmail.com',
		       'mail.com','example.com','example.invalid','example.test','invalid.example'
		     )
		     AND lower(split_part(s.email,'@',2)) <> ''
		   )`,
		buyerID,
	).Scan(&n)
	return n, err
}

// CountIndependentSuppliersForJob is a convenience wrapper that resolves the
// buyer from an existing job row.
func (s *Store) CountIndependentSuppliersForJob(ctx context.Context, jobID uuid.UUID) (int, error) {
	var buyerID uuid.UUID
	if err := s.pool.QueryRow(ctx, `SELECT buyer_id FROM jobs WHERE id=$1`, jobID).Scan(&buyerID); err != nil {
		return 0, err
	}
	return s.CountIndependentSuppliersForBuyer(ctx, buyerID)
}

// RefuseIfNoIndependentSupplier fails closed when a job requires verification
// (redundancy or honeypot tasks) and no unlinked supplier is online. Records
// the exclusion and returns errNoIndependentSupplier. jobID may be the
// pre-allocated job UUID even before the jobs row is inserted.
func (s *Store) RefuseIfNoIndependentSupplier(ctx context.Context, jobID, buyerID uuid.UUID, requiresVerification bool) error {
	if !requiresVerification {
		return nil
	}
	n, err := s.CountIndependentSuppliersForBuyer(ctx, buyerID)
	if err != nil {
		return err
	}
	if n > 0 {
		return nil
	}
	_ = s.RecordClaimIndependenceExclusion(ctx, jobID, buyerID, uuid.Nil, nil, nil,
		"no_independent_supplier", nil,
		map[string]any{"reason": "no unlinked supplier online for independent verification"})
	return fmt.Errorf("%w", errNoIndependentSupplier)
}

// recordLinkedClaimExclusions writes one placement-decision row per ready job
// this supplier is linked out of (deduped per calendar day).
func (s *Store) recordLinkedClaimExclusions(ctx context.Context, w WorkerAuth) error {
	rows, err := s.pool.Query(ctx, `
		SELECT DISTINCT j.id, j.buyer_id,
		       EXISTS (
		         SELECT 1 FROM tasks t
		          WHERE t.job_id = j.id
		            AND t.status IN ('queued','retrying')
		            AND (t.is_honeypot OR t.is_redundancy)
		       ) AS is_verification
		  FROM jobs j
		  JOIN tasks t ON t.job_id = j.id
		 WHERE j.status NOT IN ('cancelled','failed','complete')
		   AND t.status IN ('queued','retrying')
		   AND COALESCE(t.visible_at, t.created_at) <= now()
		   AND (
		     EXISTS (SELECT 1 FROM suppliers s WHERE s.id = $1 AND s.owner_buyer_id = j.buyer_id)
		     OR EXISTS (
		       SELECT 1 FROM suppliers s
		         JOIN buyers b ON b.id = j.buyer_id
		        WHERE s.id = $1
		          AND lower(split_part(s.email,'@',2)) = lower(split_part(b.email,'@',2))
		          AND lower(split_part(b.email,'@',2)) <> ''
		          AND lower(split_part(b.email,'@',2)) NOT IN (
		            'gmail.com','googlemail.com','yahoo.com','hotmail.com','outlook.com',
		            'live.com','icloud.com','me.com','aol.com','proton.me','protonmail.com',
		            'mail.com','example.com','example.invalid','example.test','invalid.example'
		          )
		     )
		     OR EXISTS (
		       SELECT 1 FROM worker_enrollment_codes wec
		        WHERE wec.buyer_id = j.buyer_id AND wec.supplier_id = $1
		          AND wec.consumed_at IS NOT NULL
		     )
		   )
		   AND NOT EXISTS (
		     SELECT 1 FROM claim_independence_exclusions e
		      WHERE e.job_id = j.id AND e.supplier_id = $1
		        AND e.created_at > now() - interval '1 day'
		   )
		 LIMIT 20`,
		w.SupplierID,
	)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var jobID, buyerID uuid.UUID
		var isVerification bool
		if err := rows.Scan(&jobID, &buyerID, &isVerification); err != nil {
			return err
		}
		signals, _ := s.buyerSupplierLinkSignals(ctx, buyerID, w.SupplierID)
		kind := "buyer_work"
		if isVerification {
			kind = "verification_work"
		}
		workerID := w.WorkerID
		if err := s.RecordClaimIndependenceExclusion(ctx, jobID, buyerID, w.SupplierID,
			&workerID, nil, kind, signals,
			map[string]any{"policy": "buyer_supplier_independence_v1"}); err != nil {
			return err
		}
	}
	return rows.Err()
}
