package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
)

type riskReserveFixtureOptions struct {
	acceptedChargeMicros int64
	settledChargeMicros  int64
	slaPremiumMicros     int64
	terminalAt           time.Time
}

type riskReserveFixture struct {
	store      *Store
	pool       *pgxpool.Pool
	ctx        context.Context
	buyerID    uuid.UUID
	supplierID uuid.UUID
	jobID      uuid.UUID
	taskID     uuid.UUID
	pricing    PricingDecision
	wantNanos  int64
}

func seedRiskReserveFixture(t *testing.T, opts riskReserveFixtureOptions) riskReserveFixture {
	t.Helper()
	installSettlementCurrencyForTest(t, "usd")
	t.Setenv(costScheduleRevisionEnv, "")
	ctx, store, pool := openIsolatedTestStore(t)
	if opts.acceptedChargeMicros <= 0 {
		opts.acceptedChargeMicros = 1_000_000
	}
	if opts.settledChargeMicros <= 0 {
		opts.settledChargeMicros = opts.acceptedChargeMicros
	}
	if opts.terminalAt.IsZero() {
		opts.terminalAt = time.Now().UTC().Add(-time.Minute)
	}

	f := riskReserveFixture{
		store: store, pool: pool, ctx: ctx,
		buyerID: uuid.New(), supplierID: uuid.New(), jobID: uuid.New(), taskID: uuid.New(),
	}
	catalogue := CataloguePriceAuthority{
		Version: 1, ScheduleVersion: 1,
		ScheduleSHA256:            fmt.Sprintf("%064x", 1),
		BoardSHA256:               fmt.Sprintf("%064x", 2),
		ReferenceCurrency:         costReferenceCurrency,
		SettlementCurrency:        "usd",
		ReferenceToSettlementRate: 1,
		FXRevision:                "identity-usd",
	}
	costPolicy, err := freezeCurrentCostPolicySnapshot(catalogue, "usd")
	mustf(t, err, "freeze risk reserve cost policy: %v")
	acceptedNanos := opts.acceptedChargeMicros * NanosPerMicro
	acceptedRisk, err := riskReserveNanos(costPolicy.Schedule, acceptedNanos)
	mustf(t, err, "derive accepted risk reserve: %v")
	f.pricing = PricingDecision{
		Version: pricingDecisionVersion, PolicyRevision: pricingDecisionPolicyRevision,
		ExecutionMode: computeExecutionExactReuse, Currency: "usd", Tier: "batch",
		WorkloadDecisionSHA256: fmt.Sprintf("%064x", 3),
		ComputePlanSHA256:      fmt.Sprintf("%064x", 4),
		CostScheduleSHA256:     costPolicy.ScheduleSHA256,
		CostScheduleRevision:   costPolicy.Schedule.Revision,
		CostPolicy:             costPolicy,
		Catalogue:              catalogue,
		BuyerPrice:             microsToUSD(opts.acceptedChargeMicros),
		MaximumBuyerPrice:      microsToUSD(opts.acceptedChargeMicros),
		FixedPoint: &FixedPointPricingDecision{
			Currency: "usd", BuyerChargeNanos: acceptedNanos,
			AcceptedCeilingNanos: acceptedNanos,
		},
		PrimarySupplierCost:      notApplicableCost("exact-reuse fixture"),
		VerificationCost:         notApplicableCost("exact-reuse fixture"),
		RiskReserve:              modeledCost(nanosToEconomicUSD(acceptedRisk), "frozen risk reserve test policy"),
		RiskReserveAcceptedNanos: acceptedRisk,
	}
	pricingSHA, err := pricingDecisionDigest(f.pricing)
	mustf(t, err, "digest risk reserve pricing: %v")
	pricingJSON, err := json.Marshal(f.pricing)
	mustf(t, err, "marshal risk reserve pricing: %v")

	mustf(t, pool.QueryRow(ctx, `
		INSERT INTO buyers (id,email) VALUES ($1,$2) RETURNING id`,
		f.buyerID, f.buyerID.String()+"@risk-reserve.invalid").Scan(&f.buyerID),
		"insert risk reserve buyer: %v")
	mustf(t, pool.QueryRow(ctx, `
		INSERT INTO suppliers (id,email,reputation,status)
		VALUES ($1,$2,0.9,'active') RETURNING id`,
		f.supplierID, f.supplierID.String()+"@risk-reserve.invalid").Scan(&f.supplierID),
		"insert risk reserve supplier: %v")
	createdAt := opts.terminalAt.Add(-2 * time.Minute)
	mergedAt := opts.terminalAt
	actualMicros := opts.settledChargeMicros + opts.slaPremiumMicros
	_, err = pool.Exec(ctx, `
		INSERT INTO jobs
		  (id,buyer_id,status,job_type,tier,input_ref,currency,created_at,terminal_at,
		   results_merged_at,actual_usd,sla_guarantee_secs,sla_premium_usd,
		   workload_decision_sha256,compute_plan_sha256,
		   pricing_decision,pricing_decision_sha256)
		VALUES ($1,$2,'complete','embed','batch','risk/input','usd',$3,$4,$5,
		        ($6::numeric/1000000),$7,($8::numeric/1000000),$9,$10,$11::jsonb,$12)`,
		f.jobID, f.buyerID, createdAt, opts.terminalAt, mergedAt, actualMicros,
		func() any {
			if opts.slaPremiumMicros > 0 {
				return 1
			}
			return nil
		}(), opts.slaPremiumMicros,
		f.pricing.WorkloadDecisionSHA256, f.pricing.ComputePlanSHA256,
		pricingJSON, pricingSHA)
	mustf(t, err, "insert risk reserve job: %v")
	_, err = pool.Exec(ctx, `
		INSERT INTO tasks
		  (id,job_id,status,verification_outcome,completed_at)
		VALUES ($1,$2,'complete','pass',$3)`,
		f.taskID, f.jobID, opts.terminalAt)
	mustf(t, err, "insert risk reserve task: %v")

	supplierMicros := opts.settledChargeMicros * 4 / 5
	platformMicros := opts.settledChargeMicros - supplierMicros
	for _, row := range []struct {
		kind, status    string
		buyer, supplier *uuid.UUID
		micros          int64
	}{
		{KindBuyerCharge, PayoutReleased, &f.buyerID, nil, -opts.settledChargeMicros},
		{KindSupplierCredit, PayoutHeld, nil, &f.supplierID, supplierMicros},
		{KindPlatformTake, PayoutReleased, nil, nil, platformMicros},
	} {
		_, err := pool.Exec(ctx, `
			INSERT INTO ledger_entries
			  (kind,buyer_id,supplier_id,task_id,amount_usd,currency,payout_status,release_at)
			VALUES ($1,$2,$3,$4,($5::numeric/1000000),'usd',$6,
			        CASE WHEN $1='supplier_credit' THEN $7::timestamptz ELSE NULL END)`,
			row.kind, row.buyer, row.supplier, f.taskID, row.micros, row.status,
			opts.terminalAt.Add(riskReserveDisputeWindow))
		mustf(t, err, "insert risk reserve ledger %s: %v", row.kind)
	}
	if opts.slaPremiumMicros > 0 {
		_, err := insertLedgerEntryIfAbsentByRefTx(ctx, pool, ledgerInsert{
			Kind: KindBuyerCharge, BuyerID: &f.buyerID,
			AmountMicros: -opts.slaPremiumMicros, Currency: "usd",
			PayoutStatus: PayoutReleased, PayoutRef: slaPremiumChargeRef(f.jobID),
		})
		mustf(t, err, "insert risk reserve SLA premium charge: %v")
	}
	mustf(t, store.AccrueRiskReserveAtSettlement(ctx, f.jobID, f.pricing),
		"accrue canonical risk reserve: %v")
	f.wantNanos, err = riskReserveNanos(
		costPolicy.Schedule, actualMicros*NanosPerMicro,
	)
	mustf(t, err, "derive settled risk reserve: %v")
	return f
}

func assertRiskReserveConserves(t *testing.T, got *RiskReserveSnapshot) {
	t.Helper()
	if got == nil {
		t.Fatal("risk reserve snapshot is nil")
	}
	if got.AccruedNanos != got.HeldNanos+got.ConsumedNanos+got.ReleasedNanos {
		t.Fatalf("nano reserve does not conserve: %+v", *got)
	}
	if got.LedgerAccruedMicros != got.LedgerHeldMicros+got.LedgerConsumedMicros+got.LedgerReleasedMicros {
		t.Fatalf("micro reserve projection does not conserve: %+v", *got)
	}
}

func TestRiskReserveObservedOutputAdjustedAccrual(t *testing.T) {
	f := seedRiskReserveFixture(t, riskReserveFixtureOptions{
		acceptedChargeMicros: 2_000_000,
		settledChargeMicros:  1_000_000, // observed-output rebate halved the accepted ceiling
	})
	got, err := f.store.RiskReserveSnapshot(f.ctx, f.jobID)
	mustf(t, err, "load observed-output reserve: %v")
	assertRiskReserveConserves(t, got)
	if got.SettledChargeNanos != 1_000_000*NanosPerMicro || got.AccruedNanos != f.wantNanos {
		t.Fatalf("observed-output accrual=%+v want settled=%d reserve=%d",
			got, 1_000_000*NanosPerMicro, f.wantNanos)
	}
	if got.AccruedNanos >= f.pricing.RiskReserveAcceptedNanos {
		t.Fatalf("actual reserve %d did not move below accepted ceiling %d",
			got.AccruedNanos, f.pricing.RiskReserveAcceptedNanos)
	}
}

func TestRiskReserveRefusesConsumptionWithoutCausalRefund(t *testing.T) {
	f := seedRiskReserveFixture(t, riskReserveFixtureOptions{})
	err := f.store.ConsumeRiskReserveOnRefund(f.ctx, f.jobID)
	if !errors.Is(err, ErrRiskReserveCausalRefundRequired) {
		t.Fatalf("standalone consume error=%v, want ErrRiskReserveCausalRefundRequired", err)
	}
	got, err := f.store.RiskReserveSnapshot(f.ctx, f.jobID)
	mustf(t, err, "load refused standalone consume: %v")
	assertRiskReserveConserves(t, got)
	if got.HeldNanos != got.AccruedNanos || got.ConsumedNanos != 0 {
		t.Fatalf("standalone consume moved reserve: %+v", got)
	}
}

func TestRiskReservePartialSLARefundConsumption(t *testing.T) {
	f := seedRiskReserveFixture(t, riskReserveFixtureOptions{
		settledChargeMicros: 1_000_000,
		slaPremiumMicros:    1_000, // below the 50-bps reserve: a partial consume
	})
	result, err := f.store.SettleJobSLA(f.ctx, f.jobID)
	mustf(t, err, "settle missed SLA with reserve: %v")
	if !result.Decided || result.Met || usdToMicros(result.RefundUSD) != 1_000 {
		t.Fatalf("SLA result=%+v, want a 1000-micro miss refund", result)
	}
	got, err := f.store.RiskReserveSnapshot(f.ctx, f.jobID)
	mustf(t, err, "load partial SLA reserve: %v")
	assertRiskReserveConserves(t, got)
	wantConsumed := int64(1_000) * NanosPerMicro
	if got.ConsumedNanos != wantConsumed || got.HeldNanos != got.AccruedNanos-wantConsumed {
		t.Fatalf("partial SLA reserve=%+v want consumed=%d", got, wantConsumed)
	}
	var events int
	mustf(t, f.pool.QueryRow(f.ctx, `
		SELECT count(*) FROM job_risk_reserve_events
		 WHERE job_id=$1 AND kind='CONSUMED' AND causal_kind='SLA_REFUND'
		   AND causal_ref=$2`, f.jobID, slaRefundRef(f.jobID)).Scan(&events),
		"count SLA reserve event: %v")
	if events != 1 {
		t.Fatalf("SLA consume events=%d, want 1", events)
	}
}

func TestRiskReserveUpheldDisputeConsumesAtomically(t *testing.T) {
	f := seedRiskReserveFixture(t, riskReserveFixtureOptions{})
	disputeID, err := f.store.RecordDispute(
		f.ctx, f.jobID, f.buyerID, "risk reserve must follow an upheld refund",
	)
	mustf(t, err, "record risk reserve dispute: %v")
	mustf(t, f.store.resolveDispute(f.ctx, disputeID, "upheld"),
		"resolve risk reserve dispute: %v")
	got, err := f.store.RiskReserveSnapshot(f.ctx, f.jobID)
	mustf(t, err, "load upheld-dispute reserve: %v")
	assertRiskReserveConserves(t, got)
	if got.HeldNanos != 0 || got.ConsumedNanos != got.AccruedNanos || got.ReleasedNanos != 0 {
		t.Fatalf("upheld dispute did not consume reserve: %+v", got)
	}
	var refunds, events int
	mustf(t, f.pool.QueryRow(f.ctx, `
		SELECT
		  (SELECT count(*) FROM ledger_entries
		    WHERE kind='buyer_refund' AND payout_ref LIKE 'dispute-refund-' || $1::text || '-%'),
		  (SELECT count(*) FROM job_risk_reserve_events
		    WHERE job_id=$2 AND kind='CONSUMED' AND causal_kind='DISPUTE_REFUND'
		      AND causal_ref=$1::text)`, disputeID, f.jobID).Scan(&refunds, &events),
		"load dispute refund/reserve evidence: %v")
	if refunds == 0 || events != 1 {
		t.Fatalf("dispute refund rows/events=%d/%d, want positive/1", refunds, events)
	}
}

func TestRiskReserveCleanSweepReleasesOnce(t *testing.T) {
	f := seedRiskReserveFixture(t, riskReserveFixtureOptions{
		terminalAt: time.Now().UTC().Add(-8 * 24 * time.Hour),
	})
	wk := NewWorkers(f.store, nil, nil)
	mustf(t, wk.releaseMaturedRiskReserves(f.ctx), "run production reserve sweep: %v")
	mustf(t, wk.releaseMaturedRiskReserves(f.ctx), "repeat production reserve sweep: %v")
	got, err := f.store.RiskReserveSnapshot(f.ctx, f.jobID)
	mustf(t, err, "load released reserve: %v")
	assertRiskReserveConserves(t, got)
	if got.HeldNanos != 0 || got.ReleasedNanos != got.AccruedNanos || got.ConsumedNanos != 0 {
		t.Fatalf("clean sweep did not release reserve: %+v", got)
	}
	var events, ledgerRows int
	mustf(t, f.pool.QueryRow(f.ctx, `
		SELECT
		 (SELECT count(*) FROM job_risk_reserve_events WHERE job_id=$1 AND kind='RELEASED'),
		 (SELECT count(*) FROM ledger_entries WHERE kind=$2 AND payout_ref=$3)`,
		f.jobID, KindRiskReserveRelease, riskReserveReleaseRef(f.jobID)).
		Scan(&events, &ledgerRows), "count clean release facts: %v")
	if events != 1 || ledgerRows != 1 {
		t.Fatalf("release events/ledger rows=%d/%d, want 1/1", events, ledgerRows)
	}
}

func TestRiskReserveConcurrentReleaseConsumeAndDuplicateCalls(t *testing.T) {
	f := seedRiskReserveFixture(t, riskReserveFixtureOptions{
		terminalAt: time.Now().UTC().Add(-8 * 24 * time.Hour),
	})
	// Durable causal fact exists before recovery calls race. Production creates
	// this row and consumes in one transaction; this setup targets the recovery
	// entry point's row-lock/idempotency behavior.
	_, err := insertLedgerEntryIfAbsentByRefTx(f.ctx, f.pool, ledgerInsert{
		Kind: KindSLARefund, BuyerID: &f.buyerID,
		AmountMicros: 1_000_000, Currency: "usd",
		PayoutStatus: PayoutReleased, PayoutRef: slaRefundRef(f.jobID),
	})
	mustf(t, err, "seed causal refund for release/consume race: %v")

	const callers = 12
	errs := make(chan error, callers)
	var wg sync.WaitGroup
	for i := 0; i < callers; i++ {
		wg.Add(1)
		go func(consume bool) {
			defer wg.Done()
			if consume {
				errs <- f.store.ConsumeRiskReserveOnRefund(f.ctx, f.jobID)
				return
			}
			err := f.store.ReleaseRiskReserveAfterDisputeWindow(f.ctx, f.jobID, time.Now().UTC())
			if errors.Is(err, ErrRiskReserveReleaseBlocked) {
				err = nil
			}
			errs <- err
		}(i%2 == 0)
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatalf("concurrent release/consume: %v", err)
		}
	}
	got, err := f.store.RiskReserveSnapshot(f.ctx, f.jobID)
	mustf(t, err, "load raced reserve: %v")
	assertRiskReserveConserves(t, got)
	if got.HeldNanos != 0 || got.ConsumedNanos != got.AccruedNanos || got.ReleasedNanos != 0 {
		t.Fatalf("release won despite causal refund, or duplicate consumed twice: %+v", got)
	}
	var consumeEvents, releaseEvents int
	mustf(t, f.pool.QueryRow(f.ctx, `
		SELECT count(*) FILTER (WHERE kind='CONSUMED'),
		       count(*) FILTER (WHERE kind='RELEASED')
		  FROM job_risk_reserve_events WHERE job_id=$1`, f.jobID).
		Scan(&consumeEvents, &releaseEvents), "count raced events: %v")
	if consumeEvents != 1 || releaseEvents != 0 {
		t.Fatalf("raced consume/release events=%d/%d, want 1/0", consumeEvents, releaseEvents)
	}
}
