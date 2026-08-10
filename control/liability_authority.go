package main

import (
	"context"
	"fmt"
	"strings"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

// Liability transitions cite an existing authority digest — PricingDecision
// SHA-256 and/or a lane settlement id — rather than inventing a second money
// body (SettlementPlan). Risk reserve already does this for its own machine;
// these helpers extend the same discipline to task settle, SLA, dispute
// reverse, payout claim/finalize, and the refund paths that share those rules.
//
// Amounts are never derived here. Callers only attach the identity that already
// authorized the money move.

const (
	// liabilityLifecycleRevision freezes the refund / dispute / payout hold
	// rules that live as code-time constants today (filing window, reverse
	// shape, hold release). Writers that apply those rules record this string
	// so a replay can recover which revision applied. It is not a policy engine.
	liabilityLifecycleRevision = "refund-dispute-payout-lifecycle-v1"
)

// Lane finality labels. Realtime and service-lease may reach a terminal money
// position without batch ContributionSettlement FINAL / true-net. These statuses
// make that explicit without promoting either lane to true-net.
const (
	// Known token / metered money is on the ledger. True-net is not claimed.
	laneFinalityKnownCostSettled = "KNOWN_COST_SETTLED"
	// Terminal prepaid/supplier ledger rows exist; economic contribution finality
	// blockers remain. Not batch FINAL.
	laneFinalityMoneyTerminalNotEconomicFinal = "MONEY_TERMINAL_NOT_ECONOMIC_FINAL"
	// Reserved for batch ContributionSettlement FINAL with empty blockers.
	// Realtime and lease writers must not emit this while true-net is refused.
	laneFinalityEconomicFinal = "ECONOMIC_FINAL"
)

// liabilityAuthority is the digest a liability write cites. At least one of
// PricingDecisionSHA256 or LaneSettlementID must be non-empty for a new write
// that invents or reverses work liability.
type liabilityAuthority struct {
	PricingDecisionSHA256 string
	LaneSettlementID      string
	LifecycleRevision     string
}

func (a liabilityAuthority) ok() bool {
	return strings.TrimSpace(a.PricingDecisionSHA256) != "" ||
		strings.TrimSpace(a.LaneSettlementID) != ""
}

func (a liabilityAuthority) validate() error {
	if !a.ok() {
		return fmt.Errorf("liability authority requires pricing_decision_sha256 or lane_settlement_id")
	}
	if sha := strings.TrimSpace(a.PricingDecisionSHA256); sha != "" && !validSHA256(sha) {
		return fmt.Errorf("liability pricing_decision_sha256 %q is not a sha256 hex digest", sha)
	}
	if rev := strings.TrimSpace(a.LifecycleRevision); rev != "" && rev != liabilityLifecycleRevision {
		return fmt.Errorf("unknown liability lifecycle revision %q", rev)
	}
	return nil
}

// isWorkLiabilityKind reports ledger kinds that invent or reverse delivered-work
// liability. Funding rails (prepaid top-up/refund) and collection (stripe_fee)
// are out of scope. Risk-reserve kinds already cite pricing SHA on their own
// state machine and are enforced there.
func isWorkLiabilityKind(kind string) bool {
	switch kind {
	case KindBuyerCharge, KindSupplierCredit, KindPlatformTake,
		KindClawback, KindBuyerRefund, KindPlatformRefund, KindSLARefund:
		return true
	default:
		return false
	}
}

// applyLiabilityAuthority copies a validated citation onto a ledger insert.
// Amounts are untouched.
func applyLiabilityAuthority(e *ledgerInsert, auth liabilityAuthority) error {
	if e == nil {
		return fmt.Errorf("ledger insert is required")
	}
	if err := auth.validate(); err != nil {
		return err
	}
	e.PricingDecisionSHA256 = strings.TrimSpace(auth.PricingDecisionSHA256)
	e.LaneSettlementID = strings.TrimSpace(auth.LaneSettlementID)
	e.LifecycleRevision = strings.TrimSpace(auth.LifecycleRevision)
	return nil
}

// applyLiabilityAuthorityToEntry copies a validated citation onto a LedgerEntry
// used by task-settlement helpers before they convert to ledgerInsert.
func applyLiabilityAuthorityToEntry(e *LedgerEntry, auth liabilityAuthority) error {
	if e == nil {
		return fmt.Errorf("ledger entry is required")
	}
	if err := auth.validate(); err != nil {
		return err
	}
	e.PricingDecisionSHA256 = strings.TrimSpace(auth.PricingDecisionSHA256)
	e.LaneSettlementID = strings.TrimSpace(auth.LaneSettlementID)
	e.LifecycleRevision = strings.TrimSpace(auth.LifecycleRevision)
	return nil
}

// loadJobPricingDecisionSHA loads the durable accept-time digest for a job.
// Empty is legal for pre-PricingDecision history; modern writers refuse empty
// when they invent new liability against that job.
func loadJobPricingDecisionSHA(ctx context.Context, db ledgerExec, jobID uuid.UUID) (string, error) {
	var sha string
	if err := db.QueryRow(ctx,
		`SELECT COALESCE(pricing_decision_sha256,'') FROM jobs WHERE id=$1`, jobID,
	).Scan(&sha); err != nil {
		return "", err
	}
	return sha, nil
}

// loadJobLiabilityAuthority returns the pricing digest that authorizes job-scoped
// liability. Lifecycle revision is set when the caller is applying refund /
// dispute / payout rules (pass withLifecycle=true).
func loadJobLiabilityAuthority(
	ctx context.Context, db ledgerExec, jobID uuid.UUID, withLifecycle bool,
) (liabilityAuthority, error) {
	sha, err := loadJobPricingDecisionSHA(ctx, db, jobID)
	if err != nil {
		return liabilityAuthority{}, err
	}
	if sha == "" {
		return liabilityAuthority{}, fmt.Errorf(
			"job %s has no pricing_decision_sha256; cannot authorize liability write", jobID)
	}
	auth := liabilityAuthority{PricingDecisionSHA256: sha}
	if withLifecycle {
		auth.LifecycleRevision = liabilityLifecycleRevision
	}
	return auth, nil
}

// loadLedgerEntryLiabilityAuthority recovers the origin authority for a
// supplier_credit that is being claimed or finalized for payout. Source is one of:
// task → job.pricing_decision_sha256, execution_contract → contract digest +
// realtime settlement id, or service-lease payout_ref → lease digest + lease id.
//
// legacyMissing is true when the liability is readable history that predates
// PricingDecision digests (no citation on the row and no origin digest). Claim
// must not fail closed on that history; new settles write the citation first.
func loadLedgerEntryLiabilityAuthority(
	ctx context.Context, tx pgx.Tx, entryID uuid.UUID,
) (auth liabilityAuthority, legacyMissing bool, err error) {
	var (
		taskID              *uuid.UUID
		executionContractID *uuid.UUID
		payoutRef           string
		existingSHA         string
		existingLane        string
		existingRev         string
	)
	if err := tx.QueryRow(ctx, `
		SELECT task_id, execution_contract_id, COALESCE(payout_ref,''),
		       COALESCE(pricing_decision_sha256,''), COALESCE(lane_settlement_id,''),
		       COALESCE(lifecycle_revision,'')
		  FROM ledger_entries WHERE id=$1`, entryID).
		Scan(&taskID, &executionContractID, &payoutRef,
			&existingSHA, &existingLane, &existingRev); err != nil {
		return liabilityAuthority{}, false, err
	}
	// Prefer the citation already frozen on the liability row when present.
	if existingSHA != "" || existingLane != "" {
		auth := liabilityAuthority{
			PricingDecisionSHA256: existingSHA,
			LaneSettlementID:      existingLane,
			LifecycleRevision:     liabilityLifecycleRevision,
		}
		if existingRev != "" {
			auth.LifecycleRevision = existingRev
		}
		return auth, false, auth.validate()
	}
	if taskID != nil {
		var jobID uuid.UUID
		var pricingSHA string
		if err := tx.QueryRow(ctx, `
			SELECT t.job_id, COALESCE(j.pricing_decision_sha256,'')
			  FROM tasks t JOIN jobs j ON j.id=t.job_id
			 WHERE t.id=$1`, *taskID).Scan(&jobID, &pricingSHA); err != nil {
			return liabilityAuthority{}, false, err
		}
		if pricingSHA == "" {
			return liabilityAuthority{LifecycleRevision: liabilityLifecycleRevision}, true, nil
		}
		auth := liabilityAuthority{
			PricingDecisionSHA256: pricingSHA,
			LifecycleRevision:     liabilityLifecycleRevision,
		}
		return auth, false, auth.validate()
	}
	if executionContractID != nil {
		var pricingSHA string
		var settlementID *uuid.UUID
		if err := tx.QueryRow(ctx, `
			SELECT COALESCE(c.pricing_decision_sha256,''), s.id
			  FROM execution_contracts c
			  LEFT JOIN realtime_settlements s ON s.contract_id=c.id
			 WHERE c.id=$1`, *executionContractID).
			Scan(&pricingSHA, &settlementID); err != nil {
			return liabilityAuthority{}, false, err
		}
		auth := liabilityAuthority{
			PricingDecisionSHA256: pricingSHA,
			LifecycleRevision:     liabilityLifecycleRevision,
		}
		if settlementID != nil {
			auth.LaneSettlementID = settlementID.String()
		}
		if !auth.ok() {
			return liabilityAuthority{LifecycleRevision: liabilityLifecycleRevision}, true, nil
		}
		return auth, false, auth.validate()
	}
	if leaseID, ok := serviceLeaseIDFromSupplierCreditRef(payoutRef); ok {
		var pricingSHA string
		if err := tx.QueryRow(ctx, `
			SELECT COALESCE(pricing_decision_sha256,'') FROM service_leases WHERE id=$1`,
			leaseID).Scan(&pricingSHA); err != nil {
			return liabilityAuthority{}, false, err
		}
		auth := liabilityAuthority{
			PricingDecisionSHA256: pricingSHA,
			LaneSettlementID:      leaseID.String(),
			LifecycleRevision:     liabilityLifecycleRevision,
		}
		if pricingSHA == "" {
			// Lease id alone is a durable lane settlement identity.
			return auth, false, nil
		}
		return auth, false, auth.validate()
	}
	return liabilityAuthority{}, false, fmt.Errorf(
		"ledger entry %s has no task, execution contract, or service-lease origin for liability authority",
		entryID)
}

// economicFinalityReportsFinal is true only for ECONOMIC_FINAL with no blockers.
// Realtime and lease must never report final while blockers remain.
func economicFinalityReportsFinal(status string, blockers []string) bool {
	return status == laneFinalityEconomicFinal && len(blockers) == 0
}

// realtimeKnownCostFinality is the honest label for a VERIFIED realtime
// settlement: known token economics are settled; true-net / batch FINAL is not
// claimed. Blockers list the refused claims so a receipt cannot be read as
// economic FINAL by silence.
func realtimeKnownCostFinality() (status string, blockers []string) {
	return laneFinalityKnownCostSettled, []string{
		"TRUE_NET_NOT_CLAIMED_ON_REALTIME_LANE",
		"BATCH_FINAL_STAGE_NOT_APPLICABLE",
	}
}

// serviceLeaseMoneyTerminalFinality labels COMPLETED/CANCELLED lease money that
// has terminal ledger rows while contribution true-net blockers remain.
func serviceLeaseMoneyTerminalFinality(blockers []string) (status string, economicFinal bool) {
	if economicFinalityReportsFinal(laneFinalityEconomicFinal, blockers) {
		return laneFinalityEconomicFinal, true
	}
	return laneFinalityMoneyTerminalNotEconomicFinal, false
}
