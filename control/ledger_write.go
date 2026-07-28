package main

import (
	"context"
	"fmt"
	"math"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

// Money columns use NUMERIC(12,6): absolute value must fit in 999999.999999.
const (
	microUSDPerUSD    int64   = 1_000_000
	maxMoneyUSD       float64 = 999_999.999999
	maxMoneyAbsMicros int64   = 999_999_999_999
)

// roundUSD is the single USD rounding helper (six decimal places / micro-USD).
func roundUSD(v float64) float64 {
	return math.Round(v*float64(microUSDPerUSD)) / float64(microUSDPerUSD)
}

// usdToMicros converts a USD float to integer micro-USD with banker's-safe
// round-half-away-from-zero via math.Round (matches the historical helpers).
func usdToMicros(v float64) int64 {
	return int64(math.Round(v * float64(microUSDPerUSD)))
}

func microsToUSD(m int64) float64 {
	return float64(m) / float64(microUSDPerUSD)
}

func moneyUSDInDomain(v float64) bool {
	if math.IsNaN(v) || math.IsInf(v, 0) {
		return false
	}
	return math.Abs(v) <= maxMoneyUSD
}

// ledgerExec is the narrow DB surface used by the sole ledger insert writer.
// Both pgx.Tx and *pgxpool.Pool satisfy it.
type ledgerExec interface {
	Exec(ctx context.Context, sql string, arguments ...any) (pgconn.CommandTag, error)
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
}

// ledgerInsert is the internal shape for production ledger writes. Amounts are
// integer micro-units of the row's currency (historically micro-USD); conversion
// to NUMERIC happens only at the SQL boundary. Currency is required and must be
// a supported code — cross-currency mixing is refused at the write boundary.
type ledgerInsert struct {
	Kind       string
	SupplierID *uuid.UUID
	BuyerID    *uuid.UUID
	TaskID     *uuid.UUID
	// ExecutionContractID keys realtime rows, which have no task. Both are
	// nullable and at most one is ever set.
	ExecutionContractID *uuid.UUID
	AmountMicros        int64
	// Currency is the ISO code this row settles in. Empty means "use the
	// process settlement currency" (new money). Explicit values allow
	// historical clawbacks to preserve the source row's currency.
	Currency     string
	PayoutStatus string
	ReleaseAt    *time.Time
	PayoutRef    string
}

func ledgerInsertFromEntry(e LedgerEntry) ledgerInsert {
	cur := e.Currency
	if cur == "" {
		cur = SettlementCurrencyCode()
	}
	return ledgerInsert{
		Kind:         e.Kind,
		SupplierID:   e.SupplierID,
		BuyerID:      e.BuyerID,
		TaskID:       e.TaskID,
		AmountMicros: usdToMicros(e.AmountUSD),
		Currency:     cur,
		PayoutStatus: e.PayoutStatus,
		ReleaseAt:    e.ReleaseAt,
	}
}

// insertLedgerEntryTx is the sole production path that issues
// INSERT INTO ledger_entries. Callers must not embed that SQL elsewhere.
//
// amount_usd is bound as ($micros::numeric / 1000000) so the Go float never
// touches the SQL parameter for the money column.
func insertLedgerEntryTx(ctx context.Context, db ledgerExec, e ledgerInsert) (pgconn.CommandTag, error) {
	e, err := resolveLedgerInsert(e)
	if err != nil {
		return pgconn.CommandTag{}, err
	}
	return db.Exec(ctx, `
		INSERT INTO ledger_entries
		  (kind, supplier_id, buyer_id, task_id, execution_contract_id,
		   amount_usd, currency, payout_status, release_at, payout_ref)
		VALUES (
		  $1, $2, $3, $4, $5,
		  ($6::numeric / 1000000),
		  $7, $8, $9, NULLIF($10, '')
		)`,
		e.Kind, e.SupplierID, e.BuyerID, e.TaskID, e.ExecutionContractID,
		e.AmountMicros, e.Currency, e.PayoutStatus, e.ReleaseAt, e.PayoutRef,
	)
}

// insertLedgerEntryOnTaskConflictDoNothingTx inserts with the task/kind unique
// index conflict target used by settlement and clawback paths.
func insertLedgerEntryOnTaskConflictDoNothingTx(ctx context.Context, db ledgerExec, e ledgerInsert) (pgconn.CommandTag, error) {
	e, err := resolveLedgerInsert(e)
	if err != nil {
		return pgconn.CommandTag{}, err
	}
	return db.Exec(ctx, `
		INSERT INTO ledger_entries
		  (kind, supplier_id, buyer_id, task_id, amount_usd, currency, payout_status, release_at, payout_ref)
		VALUES (
		  $1, $2, $3, $4,
		  ($5::numeric / 1000000),
		  $6, $7, $8, NULLIF($9, '')
		)
		ON CONFLICT (task_id, kind) DO NOTHING`,
		e.Kind, e.SupplierID, e.BuyerID, e.TaskID,
		e.AmountMicros, e.Currency, e.PayoutStatus, e.ReleaseAt, e.PayoutRef,
	)
}

// insertLedgerEntryIfAbsentByRefTx inserts only when no row already carries the
// same (kind, payout_ref). Used for stripe_fee and sla_refund once-only rows.
func insertLedgerEntryIfAbsentByRefTx(ctx context.Context, db ledgerExec, e ledgerInsert) (pgconn.CommandTag, error) {
	e, err := resolveLedgerInsert(e)
	if err != nil {
		return pgconn.CommandTag{}, err
	}
	if e.PayoutRef == "" {
		return pgconn.CommandTag{}, fmt.Errorf("ledger insert by ref requires payout_ref")
	}
	return db.Exec(ctx, `
		INSERT INTO ledger_entries
		  (kind, supplier_id, buyer_id, task_id, amount_usd, currency, payout_status, release_at, payout_ref)
		SELECT $1, $2, $3, $4, ($5::numeric / 1000000), $6, $7, $8, $9
		 WHERE NOT EXISTS (
		   SELECT 1 FROM ledger_entries WHERE kind = $1 AND payout_ref = $9
		 )`,
		e.Kind, e.SupplierID, e.BuyerID, e.TaskID,
		e.AmountMicros, e.Currency, e.PayoutStatus, e.ReleaseAt, e.PayoutRef,
	)
}

// insertJobDisputeClawbacksTx appends balancing clawback rows for every
// supplier credit on a job. Amounts are copied from existing NUMERIC values
// (no float path). SQL still lives here so the CI raw-insert guard stays clean.
func insertJobDisputeClawbacksTx(ctx context.Context, db ledgerExec, jobID uuid.UUID) (pgconn.CommandTag, error) {
	// Currency is copied from the credit being clawed back — historical USD
	// credits stay USD even when the deployment now settles CAD.
	return db.Exec(ctx, `
		INSERT INTO ledger_entries
		  (kind, supplier_id, task_id, amount_usd, currency, payout_status)
		SELECT 'clawback', le.supplier_id, le.task_id, -le.amount_usd, le.currency, 'clawed_back'
		  FROM ledger_entries le JOIN tasks t ON t.id = le.task_id
		 WHERE t.job_id = $1 AND le.kind = 'supplier_credit'
		   AND le.amount_usd > 0 AND le.task_id IS NOT NULL
		ON CONFLICT (task_id, kind) DO NOTHING`, jobID)
}

// insertJobSLAPremiumChargeTx records the once-only SLA premium buyer_charge
// from the frozen economic plan (NUMERIC → NUMERIC, no float path).
func insertJobSLAPremiumChargeTx(ctx context.Context, db ledgerExec, jobID uuid.UUID, payoutRef string) (pgconn.CommandTag, error) {
	var jobCurrency, planCurrency string
	if err := db.QueryRow(ctx, `
		SELECT j.currency,p.currency
		  FROM jobs j JOIN job_economic_plans p ON p.job_id=j.id
		 WHERE j.id=$1`, jobID).Scan(&jobCurrency, &planCurrency); err != nil {
		return pgconn.CommandTag{}, err
	}
	if jobCurrency != planCurrency {
		return pgconn.CommandTag{}, fmt.Errorf(
			"%w: job %s currency %s differs from economic plan %s",
			errCurrencyMismatch, jobID, jobCurrency, planCurrency,
		)
	}
	if err := RequireSettlementCurrency(jobCurrency); err != nil {
		return pgconn.CommandTag{}, fmt.Errorf("job %s cannot settle SLA premium: %w", jobID, err)
	}
	return db.Exec(ctx, `
		INSERT INTO ledger_entries (kind, buyer_id, amount_usd, currency, payout_status, payout_ref)
		SELECT 'buyer_charge', j.buyer_id, -p.sla_premium_usd, $3, 'released', $2
		  FROM jobs j JOIN job_economic_plans p ON p.job_id = j.id
		 WHERE j.id = $1 AND j.status = 'complete' AND p.sla_premium_usd > 0
		ON CONFLICT DO NOTHING`, jobID, payoutRef, jobCurrency)
}

// resolveLedgerInsert validates and fills currency on a ledger write. Empty
// Currency resolves to the process settlement currency. Non-settlement currency
// is refused for new money; clawbacks may preserve a historical supported code.
func resolveLedgerInsert(e ledgerInsert) (ledgerInsert, error) {
	if e.Kind == "" {
		return ledgerInsert{}, fmt.Errorf("ledger insert requires kind")
	}
	if e.AmountMicros > maxMoneyAbsMicros || e.AmountMicros < -maxMoneyAbsMicros {
		return ledgerInsert{}, fmt.Errorf("ledger amount %d micro-units exceeds NUMERIC(12,6) domain", e.AmountMicros)
	}
	cur := strings.TrimSpace(e.Currency)
	if cur == "" {
		code := SettlementCurrencyCode()
		if code == "" {
			return ledgerInsert{}, fmt.Errorf("ledger insert requires currency (settlement currency is not configured)")
		}
		cur = code
	}
	got, err := ParseCurrency(cur)
	if err != nil {
		return ledgerInsert{}, fmt.Errorf("ledger insert currency: %w", err)
	}
	settle, serr := SettlementCurrency()
	if serr != nil {
		return ledgerInsert{}, serr
	}
	if !got.Equal(settle) && e.Kind != KindClawback {
		return ledgerInsert{}, fmt.Errorf("%w: ledger insert currency %s does not match settlement %s",
			errCurrencyMismatch, got, settle)
	}
	e.Currency = got.Code()
	return e, nil
}

// validateLedgerInsert is the exported-shape check used by tests.
func validateLedgerInsert(e ledgerInsert) error {
	_, err := resolveLedgerInsert(e)
	return err
}

// insertNewLedgerEntryTx inserts and requires a new row (conflict is an error).
func insertNewLedgerEntryTx(ctx context.Context, tx pgx.Tx, entry LedgerEntry) error {
	ct, err := insertLedgerEntryOnTaskConflictDoNothingTx(ctx, tx, ledgerInsertFromEntry(entry))
	if err != nil {
		return err
	}
	if ct.RowsAffected() != 1 {
		return fmt.Errorf("ledger row %s/%v existed before terminal transition", entry.Kind, entry.TaskID)
	}
	return nil
}

// insertLedgerEntryIfAbsentExactTx inserts if absent; if present, requires an
// exact amount/party match (used by verification clawback idempotency).
func insertLedgerEntryIfAbsentExactTx(ctx context.Context, tx pgx.Tx, entry LedgerEntry) error {
	want := ledgerInsertFromEntry(entry)
	ct, err := insertLedgerEntryOnTaskConflictDoNothingTx(ctx, tx, want)
	if err != nil || ct.RowsAffected() == 1 {
		return err
	}
	var amountMicros int64
	var supplierID, taskID *uuid.UUID
	var kind string
	if err := tx.QueryRow(ctx, `
		SELECT kind, supplier_id, task_id, (amount_usd * 1000000)::bigint
		  FROM ledger_entries
		 WHERE task_id = $1 AND kind = $2`, entry.TaskID, entry.Kind).
		Scan(&kind, &supplierID, &taskID, &amountMicros); err != nil {
		return err
	}
	if kind != want.Kind || amountMicros != want.AmountMicros ||
		!sameOptionalUUID(supplierID, want.SupplierID) || !sameOptionalUUID(taskID, want.TaskID) {
		return fmt.Errorf("conflicting existing ledger row %s/%v", entry.Kind, entry.TaskID)
	}
	return nil
}
