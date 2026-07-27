package main

import (
	"context"
	"fmt"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

// Supplier payouts used to floor each ledger entry to whole cents on its own.
// At catalogue prices a supplier credit is worth a fraction of a cent, so every
// entry floored to zero and moved to payout_status='carried' -- a state with
// exactly one writer and no path back. Supplier lifetime cash was $0.00 by
// construction, which is the real reason supplier net read -$0.00014/hr.
//
// The residue is now accrued against the supplier account. `carried` means
// "absorbed into the accrual", and the next entry that pushes the accrual over
// a cent pays out everything accumulated.

const supplierSettlementPolicyAccountAccrualV2 = "account_accrual_v2"

// supplierAccrual is the account-level carry for one supplier.
type supplierAccrual struct {
	AccruedMicros    int64
	LifetimeAbsorbed int64
	LifetimePaidCent int64
}

// lockSupplierAccrual reads the supplier's accrual FOR UPDATE, creating the row
// on first use. The lock is what makes concurrent claims for the same supplier
// serialise: without it two claims could each read the same carry and both
// spend it.
func lockSupplierAccrual(ctx context.Context, tx pgx.Tx, supplierID uuid.UUID) (supplierAccrual, error) {
	var a supplierAccrual
	if _, err := tx.Exec(ctx, `
		INSERT INTO supplier_payout_accruals (supplier_id) VALUES ($1)
		ON CONFLICT (supplier_id) DO NOTHING`, supplierID); err != nil {
		return a, fmt.Errorf("creating supplier accrual %s: %w", supplierID, err)
	}
	if err := tx.QueryRow(ctx, `
		SELECT accrued_microusd, lifetime_absorbed_microusd, lifetime_paid_cents
		  FROM supplier_payout_accruals WHERE supplier_id=$1
		 FOR UPDATE`, supplierID,
	).Scan(&a.AccruedMicros, &a.LifetimeAbsorbed, &a.LifetimePaidCent); err != nil {
		return a, fmt.Errorf("locking supplier accrual %s: %w", supplierID, err)
	}
	return a, nil
}

// accrueSupplierLiability folds one entry's liability into the supplier's
// accrual and reports how many whole cents are now payable.
//
// carryIn + liability == cashCents*10000 + carryOut, always. The database
// enforces that same equation on supplier_accrual_events, so a lost or invented
// micro-USD cannot be written even if this function is wrong.
func accrueSupplierLiability(
	ctx context.Context,
	tx pgx.Tx,
	supplierID, entryID uuid.UUID,
	liabilityMicros int64,
) (cashCents, carryOut int64, err error) {
	if liabilityMicros < 0 {
		return 0, 0, fmt.Errorf("supplier liability must be non-negative, got %d microusd", liabilityMicros)
	}
	accrual, err := lockSupplierAccrual(ctx, tx, supplierID)
	if err != nil {
		return 0, 0, err
	}

	// Replaying the same entry must be a no-op, not a second absorption.
	var (
		haveCarryIn, haveLiability, haveCash, haveCarryOut int64
		replay                                             bool
	)
	if err := tx.QueryRow(ctx, `
		SELECT carry_in_microusd, liability_microusd, cash_cents, remainder_microusd, true
		  FROM supplier_minor_unit_settlements WHERE ledger_entry_id=$1`, entryID,
	).Scan(&haveCarryIn, &haveLiability, &haveCash, &haveCarryOut, &replay); err == nil {
		if haveLiability != liabilityMicros {
			return 0, 0, fmt.Errorf(
				"accrual event for ledger entry %s changed: recorded liability %d, now %d",
				entryID, haveLiability, liabilityMicros)
		}
		return haveCash, haveCarryOut, nil
	} else if err != pgx.ErrNoRows {
		return 0, 0, err
	}

	effective := accrual.AccruedMicros + liabilityMicros
	cashCents = effective / microUSDPerCent
	carryOut = effective % microUSDPerCent

	if _, err := tx.Exec(ctx, `
		INSERT INTO supplier_minor_unit_settlements
		  (ledger_entry_id,policy,carry_in_microusd,liability_microusd,cash_cents,remainder_microusd,currency)
		VALUES ($1,$2,$3,$4,$5,$6,$7)`,
		entryID, supplierSettlementPolicyAccountAccrualV2,
		accrual.AccruedMicros, liabilityMicros, cashCents, carryOut, SettlementCurrencyCode(),
	); err != nil {
		return 0, 0, fmt.Errorf("recording accrual settlement for %s: %w", entryID, err)
	}
	if _, err := tx.Exec(ctx, `
		UPDATE supplier_payout_accruals
		   SET accrued_microusd = $2,
		       lifetime_absorbed_microusd = lifetime_absorbed_microusd + $3,
		       lifetime_paid_cents = lifetime_paid_cents + $4,
		       updated_at = now()
		 WHERE supplier_id = $1`,
		supplierID, carryOut, liabilityMicros, cashCents,
	); err != nil {
		return 0, 0, fmt.Errorf("updating supplier accrual %s: %w", supplierID, err)
	}
	return cashCents, carryOut, nil
}

// SupplierAccrual reports the account-level carry, for the earnings view and
// for the reconciliation invariant.
func (s *Store) SupplierAccrual(ctx context.Context, supplierID uuid.UUID) (supplierAccrual, error) {
	var a supplierAccrual
	err := s.pool.QueryRow(ctx, `
		SELECT accrued_microusd, lifetime_absorbed_microusd, lifetime_paid_cents
		  FROM supplier_payout_accruals WHERE supplier_id=$1`, supplierID,
	).Scan(&a.AccruedMicros, &a.LifetimeAbsorbed, &a.LifetimePaidCent)
	if err == pgx.ErrNoRows {
		return supplierAccrual{}, nil
	}
	return a, err
}
