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
	Currency string
	// CurrencyAuthority is set only after a caller has loaded and locked an
	// immutable historical source (currently a prepaid card operation/balance,
	// an execution contract, or a job's frozen currency). It permits preserving
	// that supported currency after a deployment cutover; it is never an
	// FX/conversion escape hatch.
	CurrencyAuthority ledgerCurrencyAuthority
	// BoundJobCurrency is jobs.currency loaded under FOR UPDATE. Required when
	// CurrencyAuthority is ledgerCurrencyAuthorityJob; the insert currency must
	// equal this value exactly.
	BoundJobCurrency string
	PayoutStatus     string
	ReleaseAt        *time.Time
	PayoutRef        string
	// PricingDecisionSHA256 is the existing PricingDecision digest that
	// authorized this liability transition. Optional for historical rows and
	// non-liability kinds; required on new work-liability production writers.
	PricingDecisionSHA256 string
	// LifecycleRevision records which refund/dispute/payout rules applied when
	// this write uses those machines. Empty for pure settle/accrual paths that
	// do not apply those rules.
	LifecycleRevision string
	// LaneSettlementID is the realtime settlement id or service-lease id when
	// that lane row is the cited authority (or a co-citation with pricing SHA).
	LaneSettlementID string
}

type ledgerCurrencyAuthority string

const (
	ledgerCurrencyAuthorityNone              ledgerCurrencyAuthority = ""
	ledgerCurrencyAuthorityPrepaid           ledgerCurrencyAuthority = "prepaid_source"
	ledgerCurrencyAuthorityExecutionContract ledgerCurrencyAuthority = "execution_contract_source"
	// ledgerCurrencyAuthorityJob authorises risk_reserve_* and sla_refund rows
	// in the job's immutable accepted currency after a process settlement cutover.
	ledgerCurrencyAuthorityJob ledgerCurrencyAuthority = "job_source"
)

// lockJobCurrencyAuthority loads jobs.currency under FOR UPDATE and returns a
// job-source currency authority bound to that immutable value. wantCurrency,
// when non-empty, must equal the locked job currency (exact match, no conversion).
func lockJobCurrencyAuthority(
	ctx context.Context, tx pgx.Tx, jobID uuid.UUID, wantCurrency string,
) (ledgerCurrencyAuthority, string, error) {
	var jobCurrency string
	if err := tx.QueryRow(ctx,
		`SELECT currency FROM jobs WHERE id=$1 FOR UPDATE`, jobID,
	).Scan(&jobCurrency); err != nil {
		return "", "", err
	}
	if _, err := ParseCurrency(jobCurrency); err != nil {
		return "", "", fmt.Errorf("job %s currency: %w", jobID, err)
	}
	if wantCurrency != "" && wantCurrency != jobCurrency {
		return "", "", fmt.Errorf(
			"%w: ledger currency %s does not match job %s currency %s",
			errCurrencyMismatch, wantCurrency, jobID, jobCurrency)
	}
	return ledgerCurrencyAuthorityJob, jobCurrency, nil
}

func ledgerInsertFromEntry(e LedgerEntry) ledgerInsert {
	cur := e.Currency
	if cur == "" {
		cur = SettlementCurrencyCode()
	}
	return ledgerInsert{
		Kind:                  e.Kind,
		SupplierID:            e.SupplierID,
		BuyerID:               e.BuyerID,
		TaskID:                e.TaskID,
		AmountMicros:          usdToMicros(e.AmountUSD),
		Currency:              cur,
		PayoutStatus:          e.PayoutStatus,
		ReleaseAt:             e.ReleaseAt,
		PricingDecisionSHA256: e.PricingDecisionSHA256,
		LifecycleRevision:     e.LifecycleRevision,
		LaneSettlementID:      e.LaneSettlementID,
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
		   amount_usd, currency, payout_status, release_at, payout_ref,
		   pricing_decision_sha256, lifecycle_revision, lane_settlement_id)
		VALUES (
		  $1, $2, $3, $4, $5,
		  ($6::numeric / 1000000),
		  $7, $8, $9, NULLIF($10, ''),
		  NULLIF($11, ''), NULLIF($12, ''), NULLIF($13, '')
		)`,
		e.Kind, e.SupplierID, e.BuyerID, e.TaskID, e.ExecutionContractID,
		e.AmountMicros, e.Currency, e.PayoutStatus, e.ReleaseAt, e.PayoutRef,
		e.PricingDecisionSHA256, e.LifecycleRevision, e.LaneSettlementID,
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
		  (kind, supplier_id, buyer_id, task_id, amount_usd, currency, payout_status, release_at, payout_ref,
		   pricing_decision_sha256, lifecycle_revision, lane_settlement_id)
		VALUES (
		  $1, $2, $3, $4,
		  ($5::numeric / 1000000),
		  $6, $7, $8, NULLIF($9, ''),
		  NULLIF($10, ''), NULLIF($11, ''), NULLIF($12, '')
		)
		ON CONFLICT (task_id, kind) DO NOTHING`,
		e.Kind, e.SupplierID, e.BuyerID, e.TaskID,
		e.AmountMicros, e.Currency, e.PayoutStatus, e.ReleaseAt, e.PayoutRef,
		e.PricingDecisionSHA256, e.LifecycleRevision, e.LaneSettlementID,
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
		  (kind, supplier_id, buyer_id, task_id, execution_contract_id,
		   amount_usd, currency, payout_status, release_at, payout_ref,
		   pricing_decision_sha256, lifecycle_revision, lane_settlement_id)
		SELECT $1, $2, $3, $4, $5, ($6::numeric / 1000000), $7, $8, $9, $10,
		       NULLIF($11, ''), NULLIF($12, ''), NULLIF($13, '')
		 WHERE NOT EXISTS (
		   SELECT 1 FROM ledger_entries WHERE kind = $1 AND payout_ref = $10
		 )`,
		e.Kind, e.SupplierID, e.BuyerID, e.TaskID, e.ExecutionContractID,
		e.AmountMicros, e.Currency, e.PayoutStatus, e.ReleaseAt, e.PayoutRef,
		e.PricingDecisionSHA256, e.LifecycleRevision, e.LaneSettlementID,
	)
}

// insertJobDisputeClawbacksTx appends balancing clawback rows for every
// supplier credit on a job. Amounts are copied from existing NUMERIC values
// (no float path). SQL still lives here so the CI raw-insert guard stays clean.
// Each clawback cites the job's PricingDecision digest when present, and always
// cites the refund/dispute lifecycle revision; amounts are unchanged copies.
func insertJobDisputeClawbacksTx(ctx context.Context, db ledgerExec, jobID uuid.UUID) (pgconn.CommandTag, error) {
	pricingSHA, err := loadJobPricingDecisionSHA(ctx, db, jobID)
	if err != nil {
		return pgconn.CommandTag{}, err
	}
	// Currency is copied from the credit being clawed back — historical USD
	// credits stay USD even when the deployment now settles CAD.
	return db.Exec(ctx, `
		INSERT INTO ledger_entries
		  (kind, supplier_id, task_id, amount_usd, currency, payout_status,
		   pricing_decision_sha256, lifecycle_revision)
		SELECT 'clawback', le.supplier_id, le.task_id, -le.amount_usd, le.currency, 'clawed_back',
		       NULLIF($2, ''), $3
		  FROM ledger_entries le JOIN tasks t ON t.id = le.task_id
		 WHERE t.job_id = $1 AND le.kind = 'supplier_credit'
		   AND le.amount_usd > 0 AND le.task_id IS NOT NULL
		ON CONFLICT (task_id, kind) DO NOTHING`,
		jobID, pricingSHA, liabilityLifecycleRevision)
}

// disputeBuyerRefundResult is the NUMERIC-domain outcome of recording dispute
// buyer/platform refunds for one job. Amounts are micro-units of the job
// settlement currency.
type disputeBuyerRefundResult struct {
	BuyerRefundMicros    int64
	PlatformRefundMicros int64
	TasksRefunded        int64
	SLARefundMicros      int64
	Currency             string
}

// insertJobDisputeBuyerRefundsTx credits the buyer for every task-scoped
// buyer_charge on the job, and reverses each matching platform_take.
//
// Design (and why the existing (task_id, kind) unique index works):
//
//   - Settlement writes at most one buyer_charge and one platform_take per
//     task (ledger_task_kind_uniq). A full dispute refund is therefore also
//     one buyer_refund and one platform_refund per task — the same uniqueness
//     shape as clawback. Retries and concurrent resolvers hit ON CONFLICT DO
//     NOTHING and cannot double-refund.
//   - Amounts are NUMERIC copies of the charge/take rows (no float path). A
//     refund row is therefore exactly equal to the charge it reverses; the
//     post-condition check refuses any task whose net charge+refund is
//     positive (refund > charge).
//   - Partial multi-row refunds of the same kind on one task are not
//     representable under (task_id, kind). That is intentional for dispute
//     resolution, which is a full job-scope reversal. Future partial-refund
//     products would need a different uniqueness key and must not weaken this
//     one.
//   - Job-level SLA premium charges (no task_id) are refunded once via
//     payout_ref uniqueness keyed by dispute id.
//
// Currency is copied from the charge/take being reversed so historical USD
// jobs stay USD under a CAD deployment.
func insertJobDisputeBuyerRefundsTx(
	ctx context.Context,
	db ledgerExec,
	jobID, disputeID uuid.UUID,
) (disputeBuyerRefundResult, error) {
	var out disputeBuyerRefundResult
	if err := db.QueryRow(ctx, `SELECT currency FROM jobs WHERE id=$1`, jobID).
		Scan(&out.Currency); err != nil {
		return out, err
	}
	if err := RequireSettlementCurrency(out.Currency); err != nil {
		// Historical jobs may still settle a supported non-process currency
		// that was the authority when the job was accepted. Refunds preserve
		// that authority rather than converting; only refuse unknown codes.
		if _, perr := ParseCurrency(out.Currency); perr != nil {
			return out, fmt.Errorf("job %s refund refused: unsupported currency %q", jobID, out.Currency)
		}
	}
	pricingSHA, err := loadJobPricingDecisionSHA(ctx, db, jobID)
	if err != nil {
		return out, err
	}

	// Task-scoped buyer refunds: exact reverse of each buyer_charge.
	ct, err := db.Exec(ctx, `
		INSERT INTO ledger_entries
		  (kind, buyer_id, task_id, amount_usd, currency, payout_status, payout_ref,
		   pricing_decision_sha256, lifecycle_revision)
		SELECT 'buyer_refund', le.buyer_id, le.task_id, -le.amount_usd, le.currency, 'released',
		       'dispute-refund-' || $2::text || '-' || le.task_id::text,
		       NULLIF($3, ''), $4
		  FROM ledger_entries le
		  JOIN tasks t ON t.id = le.task_id
		 WHERE t.job_id = $1
		   AND le.kind = 'buyer_charge'
		   AND le.amount_usd < 0
		   AND le.task_id IS NOT NULL
		   AND le.buyer_id IS NOT NULL
		   AND le.currency = (SELECT currency FROM jobs WHERE id = $1)
		ON CONFLICT (task_id, kind) DO NOTHING`,
		jobID, disputeID, pricingSHA, liabilityLifecycleRevision)
	if err != nil {
		return out, err
	}
	out.TasksRefunded = ct.RowsAffected()

	// Reverse platform take so conservation holds after clawback + refund.
	if _, err := db.Exec(ctx, `
		INSERT INTO ledger_entries
		  (kind, task_id, amount_usd, currency, payout_status, payout_ref,
		   pricing_decision_sha256, lifecycle_revision)
		SELECT 'platform_refund', le.task_id, -le.amount_usd, le.currency, 'released',
		       'dispute-platform-refund-' || $2::text || '-' || le.task_id::text,
		       NULLIF($3, ''), $4
		  FROM ledger_entries le
		  JOIN tasks t ON t.id = le.task_id
		 WHERE t.job_id = $1
		   AND le.kind = 'platform_take'
		   AND le.amount_usd > 0
		   AND le.task_id IS NOT NULL
		   AND le.currency = (SELECT currency FROM jobs WHERE id = $1)
		ON CONFLICT (task_id, kind) DO NOTHING`,
		jobID, disputeID, pricingSHA, liabilityLifecycleRevision); err != nil {
		return out, err
	}

	// Job-level SLA premium charge (no task_id): once per dispute via payout_ref.
	slaRef := "dispute-sla-refund-" + disputeID.String()
	if _, err := db.Exec(ctx, `
		INSERT INTO ledger_entries
		  (kind, buyer_id, amount_usd, currency, payout_status, payout_ref,
		   pricing_decision_sha256, lifecycle_revision)
		SELECT 'buyer_refund', le.buyer_id, -le.amount_usd, le.currency, 'released', $2,
		       NULLIF($4, ''), $5
		  FROM ledger_entries le
		 WHERE le.kind = 'buyer_charge'
		   AND le.task_id IS NULL
		   AND le.payout_ref = $3
		   AND le.amount_usd < 0
		   AND le.buyer_id IS NOT NULL
		   AND le.currency = (SELECT currency FROM jobs WHERE id = $1)
		   AND NOT EXISTS (
		         SELECT 1 FROM ledger_entries r
		          WHERE r.kind = 'buyer_refund' AND r.payout_ref = $2
		       )`, jobID, slaRef, slaPremiumChargeRef(jobID),
		pricingSHA, liabilityLifecycleRevision); err != nil {
		return out, err
	}

	// Fail closed: no task may have refunds exceeding its charges.
	var overRefunded bool
	if err := db.QueryRow(ctx, `
		SELECT EXISTS (
		  SELECT 1 FROM (
		    SELECT le.task_id, COALESCE(SUM(le.amount_usd), 0) AS net
		      FROM ledger_entries le
		      JOIN tasks t ON t.id = le.task_id
		     WHERE t.job_id = $1
		       AND le.kind IN ('buyer_charge', 'buyer_refund')
		       AND le.task_id IS NOT NULL
		     GROUP BY le.task_id
		  ) x WHERE x.net > 0
		)`, jobID).Scan(&overRefunded); err != nil {
		return out, err
	}
	if overRefunded {
		return out, fmt.Errorf("job %s buyer refund would exceed charged amount on at least one task", jobID)
	}

	// Fail closed: currency mix on refund rows is refused (should be impossible
	// given the currency predicates above, but prove it).
	var mixed int
	if err := db.QueryRow(ctx, `
		SELECT COUNT(DISTINCT le.currency)
		  FROM ledger_entries le
		  JOIN tasks t ON t.id = le.task_id
		 WHERE t.job_id = $1
		   AND le.kind IN ('buyer_charge','buyer_refund','platform_take','platform_refund',
		                   'supplier_credit','clawback')`, jobID).Scan(&mixed); err != nil {
		return out, err
	}
	if mixed > 1 {
		return out, fmt.Errorf("%w: job %s ledger mixes currencies across charge/refund rows",
			errCurrencyMismatch, jobID)
	}

	// Totals in micro-units for the dispute receipt / prepaid restore.
	if err := db.QueryRow(ctx, `
		SELECT
		  COALESCE((SELECT (SUM(le.amount_usd) * 1000000)::bigint
		              FROM ledger_entries le JOIN tasks t ON t.id = le.task_id
		             WHERE t.job_id = $1 AND le.kind = 'buyer_refund'), 0)
		  + COALESCE((SELECT (SUM(le.amount_usd) * 1000000)::bigint
		              FROM ledger_entries le
		             WHERE le.kind = 'buyer_refund'
		               AND le.payout_ref = $2), 0),
		  COALESCE((SELECT (-SUM(le.amount_usd) * 1000000)::bigint
		              FROM ledger_entries le JOIN tasks t ON t.id = le.task_id
		             WHERE t.job_id = $1 AND le.kind = 'platform_refund'), 0),
		  COALESCE((SELECT (SUM(le.amount_usd) * 1000000)::bigint
		              FROM ledger_entries le
		             WHERE le.kind = 'buyer_refund'
		               AND le.payout_ref = $2), 0)
		`, jobID, slaRef).
		Scan(&out.BuyerRefundMicros, &out.PlatformRefundMicros, &out.SLARefundMicros); err != nil {
		return out, err
	}
	return out, nil
}

// insertJobSLAPremiumChargeTx records the once-only SLA premium buyer_charge
// from the frozen economic plan (NUMERIC → NUMERIC, no float path). Cites the
// job's PricingDecision digest; amount still comes only from sla_premium_usd.
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
	pricingSHA, err := loadJobPricingDecisionSHA(ctx, db, jobID)
	if err != nil {
		return pgconn.CommandTag{}, err
	}
	return db.Exec(ctx, `
		INSERT INTO ledger_entries
		  (kind, buyer_id, amount_usd, currency, payout_status, payout_ref, pricing_decision_sha256)
		SELECT 'buyer_charge', j.buyer_id, -p.sla_premium_usd, $3, 'released', $2, NULLIF($4, '')
		  FROM jobs j JOIN job_economic_plans p ON p.job_id = j.id
		 WHERE j.id = $1 AND j.status = 'complete' AND p.sla_premium_usd > 0
		ON CONFLICT DO NOTHING`, jobID, payoutRef, jobCurrency, pricingSHA)
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
	// Reversal kinds preserve the historical currency of the row they reverse.
	// Source-bound prepaid, execution-contract, and job writers may also finish
	// an already accepted historical obligation after a deployment cutover. The
	// caller proves the immutable source (locked prepaid balance, execution
	// contract, or jobs.currency); arbitrary new money continues to require the
	// current process currency. Job authority is exact: insert currency must
	// equal the locked jobs.currency, not merely any non-settlement code.
	sourceBound := false
	switch e.CurrencyAuthority {
	case ledgerCurrencyAuthorityNone:
	case ledgerCurrencyAuthorityPrepaid:
		sourceBound = e.Kind == KindPrepaidTopup || e.Kind == KindPrepaidDebit ||
			e.Kind == KindPrepaidRestore || e.Kind == KindPrepaidRefund
	case ledgerCurrencyAuthorityExecutionContract:
		sourceBound = e.ExecutionContractID != nil
	case ledgerCurrencyAuthorityJob:
		if strings.TrimSpace(e.BoundJobCurrency) == "" {
			return ledgerInsert{}, fmt.Errorf("ledger insert job currency authority requires bound job currency")
		}
		bound, berr := ParseCurrency(e.BoundJobCurrency)
		if berr != nil {
			return ledgerInsert{}, fmt.Errorf("ledger insert bound job currency: %w", berr)
		}
		if !got.Equal(bound) {
			return ledgerInsert{}, fmt.Errorf(
				"%w: ledger insert currency %s does not match job currency %s",
				errCurrencyMismatch, got, bound)
		}
		switch e.Kind {
		case KindRiskReserveAccrual, KindRiskReserveRelease, KindRiskReserveConsume, KindSLARefund:
			sourceBound = true
		default:
			sourceBound = false
		}
	default:
		return ledgerInsert{}, fmt.Errorf("ledger insert has unknown currency authority %q", e.CurrencyAuthority)
	}
	if e.CurrencyAuthority != ledgerCurrencyAuthorityNone && !sourceBound {
		return ledgerInsert{}, fmt.Errorf("ledger insert currency authority %q does not bind this row", e.CurrencyAuthority)
	}
	if !got.Equal(settle) &&
		e.Kind != KindClawback &&
		e.Kind != KindBuyerRefund &&
		e.Kind != KindPlatformRefund &&
		!sourceBound {
		return ledgerInsert{}, fmt.Errorf("%w: ledger insert currency %s does not match settlement %s",
			errCurrencyMismatch, got, settle)
	}
	e.Currency = got.Code()
	// Citation format only — empty remains legal for historical rows and for
	// non-liability kinds. Production liability writers must populate these
	// before insert; resolve does not invent digests or change amounts.
	if sha := strings.TrimSpace(e.PricingDecisionSHA256); sha != "" {
		if !validSHA256(sha) {
			return ledgerInsert{}, fmt.Errorf("ledger insert pricing_decision_sha256 %q is not a sha256 hex digest", sha)
		}
		e.PricingDecisionSHA256 = sha
	}
	if rev := strings.TrimSpace(e.LifecycleRevision); rev != "" {
		if rev != liabilityLifecycleRevision {
			return ledgerInsert{}, fmt.Errorf("ledger insert unknown lifecycle revision %q", rev)
		}
		e.LifecycleRevision = rev
	}
	e.LaneSettlementID = strings.TrimSpace(e.LaneSettlementID)
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
