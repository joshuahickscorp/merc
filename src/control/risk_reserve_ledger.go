package main

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

// Risk reserve is exact-nano money with a durable state machine. The mutable
// row is only a materialized balance; append-only causal events and immutable
// micro-ledger projections are the audit trail.
//
//	accrued_nanos = held_nanos + consumed_nanos + released_nanos
//
// Accrual uses the job's actual settled buyer-charge ledger after observed-
// output adjustment. Consumption is legal only in the same transaction as a
// specific SLA or upheld-dispute refund. Release takes the job lock used by
// dispute filing, then the reserve row lock, after the filing window and SLA
// decision are both closed.

const (
	KindRiskReserveAccrual = "risk_reserve_accrual"
	KindRiskReserveRelease = "risk_reserve_release"
	KindRiskReserveConsume = "risk_reserve_consume"

	riskReserveAccrualPolicyCostBPS     = "FROZEN_COST_POLICY_BPS_V1"
	riskReserveAccrualPolicyLegacyRatio = "LEGACY_FROZEN_ACCEPTED_RATIO_V1"
	riskReservePolicyRevision           = "job-risk-reserve-state-v1"

	// Kept as the public historical name. The actual authority is the same
	// disputeFilingWindow used by RecordDispute.
	riskReserveDisputeWindow = disputeFilingWindow
)

var (
	ErrRiskReserveCausalRefundRequired = errors.New("risk reserve consumption requires a causal refund")
	ErrRiskReserveWindowOpen           = errors.New("risk reserve filing window is still open")
	ErrRiskReserveReleaseBlocked       = errors.New("risk reserve release is blocked")
)

func riskReserveAccrualRef(jobID uuid.UUID) string {
	return "risk-reserve-accrual-" + jobID.String()
}

func riskReserveReleaseRef(jobID uuid.UUID) string {
	return "risk-reserve-release-" + jobID.String()
}

// riskReserveConsumeRef is retained for historical ledger rows written before
// causal event identity existed.
func riskReserveConsumeRef(jobID uuid.UUID) string {
	return "risk-reserve-consume-" + jobID.String()
}

func riskReserveCausalConsumeRef(jobID uuid.UUID, causalKind, causalRef string) string {
	kind := strings.ToLower(strings.ReplaceAll(causalKind, "_", "-"))
	return "risk-reserve-consume-" + jobID.String() + "-" + kind + "-" + causalRef
}

func riskReserveLedgerMicrosForAccrual(nanos int64) (int64, error) {
	if nanos <= 0 {
		return 0, errors.New("risk reserve accrual must be positive")
	}
	micros := nanos / NanosPerMicro
	if nanos%NanosPerMicro != 0 {
		micros++
	}
	if micros <= 0 || micros > maxMoneyAbsMicros {
		return 0, errors.New("risk reserve ledger projection exceeds the money domain")
	}
	return micros, nil
}

// RiskReserveSnapshot is the exact current state. Ledger fields are the
// conserved micro-major-unit projection retained for the legacy ledger.
type RiskReserveSnapshot struct {
	JobID                 uuid.UUID
	PricingDecisionSHA256 string
	Currency              string
	PolicyRevision        string
	AccrualPolicy         string
	RiskBasisPoints       *int64
	SettledChargeNanos    int64
	AccruedNanos          int64
	HeldNanos             int64
	ConsumedNanos         int64
	ReleasedNanos         int64
	LedgerAccruedMicros   int64
	LedgerHeldMicros      int64
	LedgerConsumedMicros  int64
	LedgerReleasedMicros  int64
	ReleaseEligibleAt     time.Time
	AccruedAt             time.Time
	UpdatedAt             time.Time
}

func scanRiskReserveSnapshot(row pgx.Row) (RiskReserveSnapshot, error) {
	var out RiskReserveSnapshot
	err := row.Scan(
		&out.JobID, &out.PricingDecisionSHA256, &out.Currency,
		&out.PolicyRevision, &out.AccrualPolicy, &out.RiskBasisPoints,
		&out.SettledChargeNanos, &out.AccruedNanos, &out.HeldNanos,
		&out.ConsumedNanos, &out.ReleasedNanos,
		&out.LedgerAccruedMicros, &out.LedgerHeldMicros,
		&out.LedgerConsumedMicros, &out.LedgerReleasedMicros,
		&out.ReleaseEligibleAt, &out.AccruedAt, &out.UpdatedAt,
	)
	return out, err
}

const riskReserveSnapshotColumns = `
	job_id,pricing_decision_sha256,currency,policy_revision,accrual_policy,
	risk_basis_points,settled_charge_nanos,accrued_nanos,held_nanos,
	consumed_nanos,released_nanos,ledger_accrued_micros,ledger_held_micros,
	ledger_consumed_micros,ledger_released_micros,release_eligible_at,
	accrued_at,updated_at`

func (s *Store) RiskReserveSnapshot(ctx context.Context, jobID uuid.UUID) (*RiskReserveSnapshot, error) {
	out, err := scanRiskReserveSnapshot(s.pool.QueryRow(ctx,
		`SELECT `+riskReserveSnapshotColumns+` FROM job_risk_reserves WHERE job_id=$1`, jobID))
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &out, nil
}

func loadRiskReserveForUpdate(ctx context.Context, tx pgx.Tx, jobID uuid.UUID) (*RiskReserveSnapshot, error) {
	out, err := scanRiskReserveSnapshot(tx.QueryRow(ctx,
		`SELECT `+riskReserveSnapshotColumns+` FROM job_risk_reserves WHERE job_id=$1 FOR UPDATE`, jobID))
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &out, nil
}

// AccrueRiskReserveAtSettlement is the standalone transactional entry point.
// FinalizeJobTx uses the Tx form so completion, actual charge and accrual commit
// together.
func (s *Store) AccrueRiskReserveAtSettlement(
	ctx context.Context, jobID uuid.UUID, pricing PricingDecision,
) error {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	if err := tx.QueryRow(ctx, `SELECT id FROM jobs WHERE id=$1 FOR UPDATE`, jobID).Scan(&jobID); err != nil {
		return err
	}
	if err := AccrueRiskReserveAtSettlementTx(ctx, tx, jobID, pricing); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

// AccrueRiskReserveAtSettlementTx derives reserve money from the actual
// settled charge. Callers must provide a real transaction: the canonical row,
// event and legacy ledger projection are one atomic fact.
func AccrueRiskReserveAtSettlementTx(
	ctx context.Context, db ledgerExec, jobID uuid.UUID, pricing PricingDecision,
) error {
	tx, ok := db.(pgx.Tx)
	if !ok {
		return errors.New("risk reserve accrual requires a database transaction")
	}

	var frozenSHA, currency string
	var terminalAt *time.Time
	if err := tx.QueryRow(ctx, `
		SELECT COALESCE(pricing_decision_sha256,''),currency,terminal_at
		  FROM jobs WHERE id=$1 FOR UPDATE`, jobID).
		Scan(&frozenSHA, &currency, &terminalAt); err != nil {
		return err
	}
	if terminalAt == nil {
		return fmt.Errorf("risk reserve accrual refused: job %s has no terminal timestamp", jobID)
	}
	gotSHA, err := pricingDecisionDigest(pricing)
	if err != nil {
		return fmt.Errorf("risk reserve pricing digest: %w", err)
	}
	if frozenSHA == "" || gotSHA != frozenSHA {
		return fmt.Errorf("risk reserve pricing digest mismatch for job %s", jobID)
	}
	if pricing.Currency != currency {
		return fmt.Errorf("risk reserve pricing currency %q does not match job currency %q", pricing.Currency, currency)
	}
	if _, err := ParseCurrency(currency); err != nil {
		return fmt.Errorf("risk reserve currency %q is unsupported", currency)
	}

	var settledChargeNanos int64
	if err := tx.QueryRow(ctx, `
		SELECT COALESCE(SUM((-le.amount_usd * 1000000000)::bigint),0)
		  FROM ledger_entries le
		 WHERE le.kind=$1 AND le.amount_usd < 0 AND le.currency=$2
		   AND (le.task_id IN (SELECT id FROM tasks WHERE job_id=$3)
		        OR le.payout_ref=$4)`,
		KindBuyerCharge, currency, jobID, slaPremiumChargeRef(jobID)).
		Scan(&settledChargeNanos); err != nil {
		return err
	}
	// A job with no settled buyer charge has no refund exposure. Do not turn an
	// accepted quote projection into money after a zero-settlement completion.
	if settledChargeNanos <= 0 {
		return nil
	}

	accrualPolicy := ""
	policyRevision := riskReservePolicyRevision
	var basisPoints *int64
	var accruedNanos int64
	if pricing.CostPolicy != nil {
		if err := validateFrozenCostPolicySnapshot(pricing.CostPolicy, currency); err != nil {
			return fmt.Errorf("risk reserve frozen cost policy: %w", err)
		}
		bps := pricing.CostPolicy.Schedule.RiskReserveBasisPoints
		if bps == 0 {
			return nil
		}
		accruedNanos, err = riskReserveNanos(pricing.CostPolicy.Schedule, settledChargeNanos)
		if err != nil {
			return err
		}
		accrualPolicy = riskReserveAccrualPolicyCostBPS
		policyRevision = pricing.CostPolicy.Schedule.Revision + ":" + pricing.CostPolicy.ScheduleSHA256
		basisPoints = &bps
	} else {
		// Historical decisions did not freeze the rate card. Never read today's
		// policy into them. Their accepted reserve/buyer pair is enough to retain
		// the exact historical ratio and scale it by the observed settlement.
		if pricing.CostScheduleSHA256 == "" || pricing.RiskReserve.Status != pricingCostModeled {
			return nil
		}
		acceptedRisk := pricing.RiskReserveAcceptedNanos
		if acceptedRisk == 0 {
			acceptedRisk = usdToMicros(pricing.RiskReserve.Amount) * NanosPerMicro
		}
		acceptedBuyer := usdToMicros(pricing.BuyerPrice) * NanosPerMicro
		if pricing.FixedPoint != nil && pricing.FixedPoint.BuyerChargeNanos > 0 {
			acceptedBuyer = pricing.FixedPoint.BuyerChargeNanos
		}
		if acceptedRisk <= 0 || acceptedBuyer <= 0 || acceptedRisk >= acceptedBuyer {
			return errors.New("historical risk reserve lacks a valid frozen accepted ratio")
		}
		accruedNanos, err = mulDiv(settledChargeNanos, acceptedRisk, acceptedBuyer, true)
		if err != nil {
			return err
		}
		accrualPolicy = riskReserveAccrualPolicyLegacyRatio
		policyRevision = "legacy-frozen-ratio:" + pricing.CostScheduleSHA256
	}
	if accruedNanos <= 0 {
		return nil
	}
	ledgerMicros, err := riskReserveLedgerMicrosForAccrual(accruedNanos)
	if err != nil {
		return err
	}
	releaseAt := terminalAt.UTC().Add(riskReserveDisputeWindow)

	tag, err := tx.Exec(ctx, `
		INSERT INTO job_risk_reserves
		  (job_id,pricing_decision_sha256,currency,policy_revision,accrual_policy,
		   risk_basis_points,settled_charge_nanos,accrued_nanos,held_nanos,
		   ledger_accrued_micros,ledger_held_micros,release_eligible_at)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$8,$9,$9,$10)
		ON CONFLICT (job_id) DO NOTHING`,
		jobID, frozenSHA, currency, policyRevision, accrualPolicy, basisPoints,
		settledChargeNanos, accruedNanos, ledgerMicros, releaseAt)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		existing, err := loadRiskReserveForUpdate(ctx, tx, jobID)
		if err != nil {
			return err
		}
		if existing == nil || existing.PricingDecisionSHA256 != frozenSHA ||
			existing.Currency != currency || existing.SettledChargeNanos != settledChargeNanos ||
			existing.AccruedNanos != accruedNanos || existing.LedgerAccruedMicros != ledgerMicros {
			return fmt.Errorf("risk reserve replay conflicts with frozen accrual for job %s", jobID)
		}
		return nil
	}

	jobAuth, boundCurrency, err := lockJobCurrencyAuthority(ctx, tx, jobID, currency)
	if err != nil {
		return err
	}
	if _, err := insertLedgerEntryIfAbsentByRefTx(ctx, tx, ledgerInsert{
		Kind: KindRiskReserveAccrual, AmountMicros: ledgerMicros, Currency: boundCurrency,
		CurrencyAuthority: jobAuth, BoundJobCurrency: boundCurrency,
		PayoutStatus: PayoutHeld, ReleaseAt: &releaseAt, PayoutRef: riskReserveAccrualRef(jobID),
	}); err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, `
		INSERT INTO job_risk_reserve_events
		  (job_id,pricing_decision_sha256,currency,kind,amount_nanos,ledger_micros,
		   causal_kind,causal_ref,ledger_payout_ref)
		VALUES ($1,$2,$3,'ACCRUED',$4,$5,'SETTLED_CHARGE',$2,$6)`,
		jobID, frozenSHA, currency, accruedNanos, ledgerMicros, riskReserveAccrualRef(jobID)); err != nil {
		return err
	}
	return nil
}

type riskReserveRefundCause struct {
	Kind        string
	Ref         string
	RefundNanos int64
}

func loadSLARefundCauseTx(ctx context.Context, tx pgx.Tx, jobID uuid.UUID, ref string) (riskReserveRefundCause, error) {
	wantRef := slaRefundRef(jobID)
	if ref == "" {
		ref = wantRef
	}
	if ref != wantRef {
		return riskReserveRefundCause{}, ErrRiskReserveCausalRefundRequired
	}
	var nanos int64
	if err := tx.QueryRow(ctx, `
		SELECT COALESCE(SUM((le.amount_usd*1000000000)::bigint),0)
		  FROM ledger_entries le JOIN jobs j ON j.id=$1
		 WHERE le.kind=$2 AND le.payout_ref=$3 AND le.currency=j.currency
		   AND le.amount_usd > 0`, jobID, KindSLARefund, ref).Scan(&nanos); err != nil {
		return riskReserveRefundCause{}, err
	}
	if nanos <= 0 {
		return riskReserveRefundCause{}, ErrRiskReserveCausalRefundRequired
	}
	return riskReserveRefundCause{Kind: "SLA_REFUND", Ref: ref, RefundNanos: nanos}, nil
}

func loadDisputeRefundCauseTx(
	ctx context.Context, tx pgx.Tx, jobID, disputeID uuid.UUID,
) (riskReserveRefundCause, error) {
	var nanos int64
	if err := tx.QueryRow(ctx, `
		SELECT COALESCE(SUM((le.amount_usd*1000000000)::bigint),0)
		  FROM ledger_entries le
		 WHERE le.kind=$1 AND le.amount_usd > 0
		   AND le.currency=(SELECT currency FROM jobs WHERE id=$2)
		   AND (le.payout_ref LIKE 'dispute-refund-' || $3::text || '-%%'
		        OR le.payout_ref='dispute-sla-refund-' || $3::text)
		   AND EXISTS (SELECT 1 FROM disputes d
		                WHERE d.id=$3::uuid AND d.job_id=$2 AND d.status='upheld')`,
		KindBuyerRefund, jobID, disputeID).Scan(&nanos); err != nil {
		return riskReserveRefundCause{}, err
	}
	if nanos <= 0 {
		return riskReserveRefundCause{}, ErrRiskReserveCausalRefundRequired
	}
	return riskReserveRefundCause{
		Kind: "DISPUTE_REFUND", Ref: disputeID.String(), RefundNanos: nanos,
	}, nil
}

func consumeRiskReserveForCauseTx(
	ctx context.Context, tx pgx.Tx, jobID uuid.UUID, cause riskReserveRefundCause,
) error {
	reserve, err := loadRiskReserveForUpdate(ctx, tx, jobID)
	if err != nil {
		return err
	}
	if reserve == nil {
		return consumeLegacyRiskReserveForCauseTx(ctx, tx, jobID, cause)
	}
	var exists bool
	if err := tx.QueryRow(ctx, `
		SELECT EXISTS (SELECT 1 FROM job_risk_reserve_events
		 WHERE job_id=$1 AND pricing_decision_sha256=$2 AND currency=$3
		   AND kind='CONSUMED' AND causal_kind=$4 AND causal_ref=$5)`,
		jobID, reserve.PricingDecisionSHA256, reserve.Currency, cause.Kind, cause.Ref).
		Scan(&exists); err != nil {
		return err
	}
	if exists {
		return nil
	}
	if reserve.HeldNanos == 0 {
		if reserve.ReleasedNanos > 0 {
			return fmt.Errorf("%w: job %s reserve was released before refund %s",
				ErrRiskReserveReleaseBlocked, jobID, cause.Ref)
		}
		return nil
	}
	consumeNanos := cause.RefundNanos
	if consumeNanos > reserve.HeldNanos {
		consumeNanos = reserve.HeldNanos
	}
	newConsumed := reserve.ConsumedNanos + consumeNanos
	ledgerConsumedTarget, err := mulDiv(
		newConsumed, reserve.LedgerAccruedMicros, reserve.AccruedNanos, false,
	)
	if err != nil {
		return err
	}
	ledgerDelta := ledgerConsumedTarget - reserve.LedgerConsumedMicros
	if ledgerDelta < 0 || ledgerDelta > reserve.LedgerHeldMicros {
		return errors.New("risk reserve consume ledger projection is out of bounds")
	}
	payoutRef := ""
	if ledgerDelta > 0 {
		jobAuth, boundCurrency, err := lockJobCurrencyAuthority(ctx, tx, jobID, reserve.Currency)
		if err != nil {
			return err
		}
		payoutRef = riskReserveCausalConsumeRef(jobID, cause.Kind, cause.Ref)
		if _, err := insertLedgerEntryIfAbsentByRefTx(ctx, tx, ledgerInsert{
			Kind: KindRiskReserveConsume, AmountMicros: -ledgerDelta,
			Currency: boundCurrency, CurrencyAuthority: jobAuth, BoundJobCurrency: boundCurrency,
			PayoutStatus: PayoutReleased, PayoutRef: payoutRef,
		}); err != nil {
			return err
		}
	}
	if _, err := tx.Exec(ctx, `
		INSERT INTO job_risk_reserve_events
		  (job_id,pricing_decision_sha256,currency,kind,amount_nanos,ledger_micros,
		   causal_kind,causal_ref,causal_refund_nanos,ledger_payout_ref)
		VALUES ($1,$2,$3,'CONSUMED',$4,$5,$6,$7,$8,NULLIF($9,''))`,
		jobID, reserve.PricingDecisionSHA256, reserve.Currency,
		consumeNanos, ledgerDelta, cause.Kind, cause.Ref, cause.RefundNanos, payoutRef); err != nil {
		return err
	}
	_, err = tx.Exec(ctx, `
		UPDATE job_risk_reserves
		   SET held_nanos=held_nanos-$4,consumed_nanos=consumed_nanos+$4,
		       ledger_held_micros=ledger_held_micros-$5,
		       ledger_consumed_micros=ledger_consumed_micros+$5
		 WHERE job_id=$1 AND pricing_decision_sha256=$2 AND currency=$3`,
		jobID, reserve.PricingDecisionSHA256, reserve.Currency, consumeNanos, ledgerDelta)
	return err
}

func consumeRiskReserveForSLARefundTx(
	ctx context.Context, tx pgx.Tx, jobID uuid.UUID, refundRef string,
) error {
	cause, err := loadSLARefundCauseTx(ctx, tx, jobID, refundRef)
	if err != nil {
		return err
	}
	return consumeRiskReserveForCauseTx(ctx, tx, jobID, cause)
}

func consumeRiskReserveForDisputeRefundTx(
	ctx context.Context, tx pgx.Tx, jobID, disputeID uuid.UUID,
) error {
	cause, err := loadDisputeRefundCauseTx(ctx, tx, jobID, disputeID)
	if err != nil {
		return err
	}
	return consumeRiskReserveForCauseTx(ctx, tx, jobID, cause)
}

// ConsumeRiskReserveOnRefund is a recovery/idempotency entry point. Normal SLA
// and dispute writers call the typed Tx helpers in the refund transaction. This
// method will only consume already-durable causal refunds and explicitly refuses
// a standalone call.
func (s *Store) ConsumeRiskReserveOnRefund(ctx context.Context, jobID uuid.UUID) error {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	if err := tx.QueryRow(ctx, `SELECT id FROM jobs WHERE id=$1 FOR UPDATE`, jobID).Scan(&jobID); err != nil {
		return err
	}

	var causes []riskReserveRefundCause
	if cause, err := loadSLARefundCauseTx(ctx, tx, jobID, slaRefundRef(jobID)); err == nil {
		causes = append(causes, cause)
	} else if !errors.Is(err, ErrRiskReserveCausalRefundRequired) {
		return err
	}
	rows, err := tx.Query(ctx, `
		SELECT id FROM disputes
		 WHERE job_id=$1 AND status='upheld'
		 ORDER BY resolved_at,id`, jobID)
	if err != nil {
		return err
	}
	var disputeIDs []uuid.UUID
	for rows.Next() {
		var id uuid.UUID
		if err := rows.Scan(&id); err != nil {
			rows.Close()
			return err
		}
		disputeIDs = append(disputeIDs, id)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return err
	}
	for _, id := range disputeIDs {
		cause, err := loadDisputeRefundCauseTx(ctx, tx, jobID, id)
		if errors.Is(err, ErrRiskReserveCausalRefundRequired) {
			continue
		}
		if err != nil {
			return err
		}
		causes = append(causes, cause)
	}
	if len(causes) == 0 {
		return fmt.Errorf("%w for job %s", ErrRiskReserveCausalRefundRequired, jobID)
	}
	for _, cause := range causes {
		if err := consumeRiskReserveForCauseTx(ctx, tx, jobID, cause); err != nil {
			return err
		}
	}
	return tx.Commit(ctx)
}

func consumeLegacyRiskReserveForCauseTx(
	ctx context.Context, tx pgx.Tx, jobID uuid.UUID, cause riskReserveRefundCause,
) error {
	var accruedMicros int64
	var currency string
	err := tx.QueryRow(ctx, `
		SELECT (amount_usd*1000000)::bigint,currency FROM ledger_entries
		 WHERE kind=$1 AND payout_ref=$2 FOR UPDATE`,
		KindRiskReserveAccrual, riskReserveAccrualRef(jobID)).Scan(&accruedMicros, &currency)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil
	}
	if err != nil {
		return err
	}
	var released bool
	if err := tx.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM ledger_entries
		 WHERE kind=$1 AND payout_ref=$2)`, KindRiskReserveRelease, riskReserveReleaseRef(jobID)).
		Scan(&released); err != nil {
		return err
	}
	if released {
		return fmt.Errorf("%w: legacy reserve for job %s was released", ErrRiskReserveReleaseBlocked, jobID)
	}
	// Causality was validated before entering this compatibility path. Historical
	// rows had only an all-or-nothing consume identity, so preserve that fact.
	jobAuth, boundCurrency, err := lockJobCurrencyAuthority(ctx, tx, jobID, currency)
	if err != nil {
		return err
	}
	_, err = insertLedgerEntryIfAbsentByRefTx(ctx, tx, ledgerInsert{
		Kind: KindRiskReserveConsume, AmountMicros: -accruedMicros, Currency: boundCurrency,
		CurrencyAuthority: jobAuth, BoundJobCurrency: boundCurrency,
		PayoutStatus: PayoutReleased, PayoutRef: riskReserveConsumeRef(jobID),
	})
	_ = cause
	return err
}

func unconsumedRiskReserveRefundExistsTx(
	ctx context.Context, tx pgx.Tx, reserve RiskReserveSnapshot,
) (bool, error) {
	var exists bool
	err := tx.QueryRow(ctx, `
		SELECT EXISTS (
		  SELECT 1 FROM ledger_entries le
		   WHERE le.kind='sla_refund' AND le.payout_ref='sla-' || $1::text
		     AND le.amount_usd>0 AND le.currency=$3
		     AND NOT EXISTS (SELECT 1 FROM job_risk_reserve_events e
		          WHERE e.job_id=$1::uuid AND e.pricing_decision_sha256=$2 AND e.currency=$3
		            AND e.kind='CONSUMED' AND e.causal_kind='SLA_REFUND'
		            AND e.causal_ref=le.payout_ref)
		) OR EXISTS (
		  SELECT 1 FROM disputes d
		   WHERE d.job_id=$1::uuid AND d.status='upheld'
		     AND EXISTS (SELECT 1 FROM ledger_entries le
		          WHERE le.kind='buyer_refund' AND le.amount_usd>0 AND le.currency=$3
		            AND (le.payout_ref LIKE 'dispute-refund-' || d.id::text || '-%%'
		                 OR le.payout_ref='dispute-sla-refund-' || d.id::text))
		     AND NOT EXISTS (SELECT 1 FROM job_risk_reserve_events e
		          WHERE e.job_id=$1::uuid AND e.pricing_decision_sha256=$2 AND e.currency=$3
		            AND e.kind='CONSUMED' AND e.causal_kind='DISPUTE_REFUND'
		            AND e.causal_ref=d.id::text)
		)`, reserve.JobID, reserve.PricingDecisionSHA256, reserve.Currency).Scan(&exists)
	return exists, err
}

func releaseRiskReserveTx(
	ctx context.Context, tx pgx.Tx, jobID uuid.UUID, now time.Time,
) (bool, error) {
	reserve, err := loadRiskReserveForUpdate(ctx, tx, jobID)
	if err != nil {
		return false, err
	}
	if reserve == nil {
		return releaseLegacyRiskReserveTx(ctx, tx, jobID, now)
	}
	if reserve.HeldNanos == 0 {
		return false, nil
	}
	if !now.After(reserve.ReleaseEligibleAt) {
		return false, fmt.Errorf("%w until %s for job %s", ErrRiskReserveWindowOpen,
			reserve.ReleaseEligibleAt.UTC().Format(time.RFC3339), jobID)
	}
	var activeDispute, slaUndecided bool
	if err := tx.QueryRow(ctx, `
		SELECT EXISTS (SELECT 1 FROM disputes WHERE job_id=$1
		                AND status IN ('open','no_peer','reverifying','unresolvable')),
		       EXISTS (SELECT 1 FROM jobs WHERE id=$1
		                AND COALESCE(sla_guarantee_secs,0)>0 AND sla_met IS NULL)`, jobID).
		Scan(&activeDispute, &slaUndecided); err != nil {
		return false, err
	}
	if activeDispute || slaUndecided {
		return false, fmt.Errorf("%w: job %s has an open dispute or undecided SLA", ErrRiskReserveReleaseBlocked, jobID)
	}
	unconsumed, err := unconsumedRiskReserveRefundExistsTx(ctx, tx, *reserve)
	if err != nil {
		return false, err
	}
	if unconsumed {
		return false, fmt.Errorf("%w: job %s has an unconsumed causal refund", ErrRiskReserveReleaseBlocked, jobID)
	}

	ledgerDelta := reserve.LedgerHeldMicros
	payoutRef := ""
	if ledgerDelta > 0 {
		jobAuth, boundCurrency, err := lockJobCurrencyAuthority(ctx, tx, jobID, reserve.Currency)
		if err != nil {
			return false, err
		}
		payoutRef = riskReserveReleaseRef(jobID)
		if _, err := insertLedgerEntryIfAbsentByRefTx(ctx, tx, ledgerInsert{
			Kind: KindRiskReserveRelease, AmountMicros: -ledgerDelta,
			Currency: boundCurrency, CurrencyAuthority: jobAuth, BoundJobCurrency: boundCurrency,
			PayoutStatus: PayoutReleased, PayoutRef: payoutRef,
		}); err != nil {
			return false, err
		}
	}
	causeRef := reserve.ReleaseEligibleAt.UTC().Format(time.RFC3339Nano)
	if _, err := tx.Exec(ctx, `
		INSERT INTO job_risk_reserve_events
		  (job_id,pricing_decision_sha256,currency,kind,amount_nanos,ledger_micros,
		   causal_kind,causal_ref,ledger_payout_ref)
		VALUES ($1,$2,$3,'RELEASED',$4,$5,'FILING_WINDOW_ELAPSED',$6,NULLIF($7,''))`,
		jobID, reserve.PricingDecisionSHA256, reserve.Currency,
		reserve.HeldNanos, ledgerDelta, causeRef, payoutRef); err != nil {
		return false, err
	}
	if _, err := tx.Exec(ctx, `
		UPDATE job_risk_reserves
		   SET held_nanos=0,released_nanos=released_nanos+$4,
		       ledger_held_micros=0,ledger_released_micros=ledger_released_micros+$5
		 WHERE job_id=$1 AND pricing_decision_sha256=$2 AND currency=$3`,
		jobID, reserve.PricingDecisionSHA256, reserve.Currency,
		reserve.HeldNanos, ledgerDelta); err != nil {
		return false, err
	}
	return true, nil
}

// ReleaseRiskReserveAfterDisputeWindow serializes with dispute filing by
// taking the same job row lock before the reserve lock.
func (s *Store) ReleaseRiskReserveAfterDisputeWindow(
	ctx context.Context, jobID uuid.UUID, now time.Time,
) error {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	if err := tx.QueryRow(ctx, `SELECT id FROM jobs WHERE id=$1 FOR UPDATE`, jobID).Scan(&jobID); err != nil {
		return err
	}
	if _, err := releaseRiskReserveTx(ctx, tx, jobID, now.UTC()); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

func releaseLegacyRiskReserveTx(
	ctx context.Context, tx pgx.Tx, jobID uuid.UUID, now time.Time,
) (bool, error) {
	var accruedMicros int64
	var currency string
	var releaseAt *time.Time
	err := tx.QueryRow(ctx, `
		SELECT (amount_usd*1000000)::bigint,currency,release_at
		  FROM ledger_entries WHERE kind=$1 AND payout_ref=$2 FOR UPDATE`,
		KindRiskReserveAccrual, riskReserveAccrualRef(jobID)).
		Scan(&accruedMicros, &currency, &releaseAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	if releaseAt != nil && !now.After(*releaseAt) {
		return false, ErrRiskReserveWindowOpen
	}
	var blocked, alreadyClosed bool
	if err := tx.QueryRow(ctx, `
		SELECT EXISTS (SELECT 1 FROM disputes WHERE job_id=$1
		                AND status IN ('open','no_peer','reverifying','unresolvable'))
		       OR EXISTS (SELECT 1 FROM jobs WHERE id=$1
		                  AND COALESCE(sla_guarantee_secs,0)>0 AND sla_met IS NULL)
		       OR EXISTS (SELECT 1 FROM ledger_entries le
		                  WHERE le.kind IN ('buyer_refund','sla_refund')
		                    AND (le.task_id IN (SELECT id FROM tasks WHERE job_id=$1)
		                         OR le.payout_ref LIKE 'dispute-%%' || $1::text || '%%'
		                         OR le.payout_ref='sla-' || $1::text)),
		       EXISTS (SELECT 1 FROM ledger_entries WHERE
		                  (kind=$2 AND payout_ref=$3) OR (kind=$4 AND payout_ref=$5))`,
		jobID, KindRiskReserveRelease, riskReserveReleaseRef(jobID),
		KindRiskReserveConsume, riskReserveConsumeRef(jobID)).Scan(&blocked, &alreadyClosed); err != nil {
		return false, err
	}
	if blocked {
		return false, ErrRiskReserveReleaseBlocked
	}
	if alreadyClosed {
		return false, nil
	}
	jobAuth, boundCurrency, err := lockJobCurrencyAuthority(ctx, tx, jobID, currency)
	if err != nil {
		return false, err
	}
	_, err = insertLedgerEntryIfAbsentByRefTx(ctx, tx, ledgerInsert{
		Kind: KindRiskReserveRelease, AmountMicros: -accruedMicros, Currency: boundCurrency,
		CurrencyAuthority: jobAuth, BoundJobCurrency: boundCurrency,
		PayoutStatus: PayoutReleased, PayoutRef: riskReserveReleaseRef(jobID),
	})
	return err == nil, err
}

// ReleaseEligibleRiskReserves is the production sweep entry point. Candidate
// discovery is advisory; each candidate rechecks under job then reserve row
// locks. A concurrent refund/dispute wins or observes the completed release,
// never a split state.
func (s *Store) ReleaseEligibleRiskReserves(
	ctx context.Context, now time.Time, limit int,
) (int, error) {
	if limit <= 0 {
		return 0, nil
	}
	rows, err := s.pool.Query(ctx, `
		SELECT r.job_id
		  FROM job_risk_reserves r JOIN jobs j ON j.id=r.job_id
		 WHERE r.held_nanos>0 AND r.release_eligible_at < $1
		   AND NOT EXISTS (SELECT 1 FROM disputes d WHERE d.job_id=r.job_id
		                   AND d.status IN ('open','no_peer','reverifying','unresolvable'))
		   AND (COALESCE(j.sla_guarantee_secs,0)=0 OR j.sla_met IS NOT NULL)
		 ORDER BY r.release_eligible_at,r.job_id LIMIT $2`, now.UTC(), limit)
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
	released := 0
	for _, id := range ids {
		tx, err := s.pool.Begin(ctx)
		if err != nil {
			return released, err
		}
		if err := tx.QueryRow(ctx, `SELECT id FROM jobs WHERE id=$1 FOR UPDATE`, id).Scan(&id); err != nil {
			tx.Rollback(ctx)
			return released, err
		}
		didRelease, err := releaseRiskReserveTx(ctx, tx, id, now.UTC())
		if err != nil {
			tx.Rollback(ctx)
			if errors.Is(err, ErrRiskReserveWindowOpen) || errors.Is(err, ErrRiskReserveReleaseBlocked) {
				continue
			}
			return released, err
		}
		if err := tx.Commit(ctx); err != nil {
			return released, err
		}
		if didRelease {
			released++
		}
	}
	return released, nil
}

// RiskReserveBalanceMicros returns the conserved ledger projection. Historical
// pre-state-machine rows retain their original ledger-only read path.
func (s *Store) RiskReserveBalanceMicros(ctx context.Context, jobID uuid.UUID) (int64, error) {
	var balance int64
	err := s.pool.QueryRow(ctx, `SELECT ledger_held_micros FROM job_risk_reserves WHERE job_id=$1`, jobID).
		Scan(&balance)
	if err == nil {
		return balance, nil
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return 0, err
	}
	err = s.pool.QueryRow(ctx, `
		SELECT COALESCE(SUM((amount_usd*1000000)::bigint),0)
		  FROM ledger_entries
		 WHERE kind IN ($1,$2,$3) AND payout_ref IN ($4,$5,$6)`,
		KindRiskReserveAccrual, KindRiskReserveRelease, KindRiskReserveConsume,
		riskReserveAccrualRef(jobID), riskReserveReleaseRef(jobID), riskReserveConsumeRef(jobID)).
		Scan(&balance)
	return balance, err
}

func (s *Store) RiskReserveBalanceNanos(ctx context.Context, jobID uuid.UUID) (int64, error) {
	var balance int64
	err := s.pool.QueryRow(ctx, `SELECT held_nanos FROM job_risk_reserves WHERE job_id=$1`, jobID).
		Scan(&balance)
	if errors.Is(err, pgx.ErrNoRows) {
		return 0, nil
	}
	return balance, err
}
