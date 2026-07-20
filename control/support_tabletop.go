package main

import (
	"context"
	"errors"
	"fmt"
	"math"

	"github.com/google/uuid"
)

type SettlementCorrectionProposal struct {
	JobID               uuid.UUID `json:"job_id"`
	DisplayedUSD        float64   `json:"displayed_usd"`
	LedgerUSD           float64   `json:"ledger_usd"`
	MismatchUSD         float64   `json:"mismatch_usd"`
	MismatchDetected    bool      `json:"mismatch_detected"`
	LedgerMutated       bool      `json:"ledger_mutated"`
	SafeCorrection      string    `json:"safe_correction"`
	CommunicationDraft  string    `json:"communication_draft"`
	PostmortemRequired  bool      `json:"postmortem_required"`
	ReconciliationBasis string    `json:"reconciliation_basis"`
}

// ProposeDisplayedSettlementCorrection compares the presentation layer with
// immutable ledger facts. It never writes money state; an attributed operator
// action is required for any actual correction.
func (s *Store) ProposeDisplayedSettlementCorrection(
	ctx context.Context, jobID uuid.UUID, displayedUSD float64,
) (SettlementCorrectionProposal, error) {
	if math.IsNaN(displayedUSD) || math.IsInf(displayedUSD, 0) || displayedUSD < 0 {
		return SettlementCorrectionProposal{}, errors.New("displayed settlement must be finite and non-negative")
	}
	var status string
	var ledgerUSD float64
	if err := s.pool.QueryRow(ctx, `
		SELECT j.status,COALESCE(sum(le.amount_usd) FILTER (WHERE le.kind='supplier_credit'),0)::float8
		  FROM jobs j
		  LEFT JOIN tasks t ON t.job_id=j.id
		  LEFT JOIN ledger_entries le ON le.task_id=t.id
		 WHERE j.id=$1 GROUP BY j.id,j.status`, jobID).Scan(&status, &ledgerUSD); err != nil {
		return SettlementCorrectionProposal{}, err
	}
	if status != "complete" {
		return SettlementCorrectionProposal{}, fmt.Errorf("job is %s, not complete", status)
	}
	delta := displayedUSD - ledgerUSD
	detected := math.Abs(delta) > 0.0000005
	proposal := SettlementCorrectionProposal{
		JobID: jobID, DisplayedUSD: displayedUSD, LedgerUSD: ledgerUSD,
		MismatchUSD: delta, MismatchDetected: detected, LedgerMutated: false,
		ReconciliationBasis: "sum of immutable supplier_credit ledger entries for completed job tasks",
		SafeCorrection:      "correct the derived display from the ledger; do not rewrite immutable money facts",
		PostmortemRequired:  detected,
	}
	if detected {
		proposal.CommunicationDraft = "We identified a settlement display discrepancy. The immutable ledger remains authoritative; payout remains held while the display and reconciliation evidence are reviewed."
	} else {
		proposal.CommunicationDraft = "The displayed settlement matches the immutable ledger."
	}
	return proposal, nil
}
