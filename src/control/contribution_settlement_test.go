package main

import (
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

func canonicalContributionPricing(t *testing.T, currency string) PricingDecision {
	t.Helper()
	trueNet := int64(450_000_000)
	runtimeNet := 0.45
	return PricingDecision{
		Version:              contributionPricingDecisionVersionForTest(),
		PolicyRevision:       pricingDecisionPolicyRevision,
		ExecutionMode:        computeExecutionDistributed,
		Currency:             currency,
		PrimarySupplierCost:  modeledCost(0.4, "accepted supplier entitlement"),
		VerificationCost:     notApplicableCost("no verification supplier in fixture"),
		PaymentCost:          modeledCost(0.1, "accepted processor forecast"),
		ControlPlaneCost:     modeledCost(0.05, "accepted control allocation"),
		StorageCost:          notApplicableCost("no retained object bytes"),
		EgressCost:           notApplicableCost("no result egress bytes"),
		ProviderCost:         notApplicableCost("community supplier"),
		RiskReserve:          notApplicableCost("no reserve in pure reducer fixture"),
		PlatformContribution: modeledCost(0.45, "accepted known-cost contribution"),
		FixedPoint: &FixedPointPricingDecision{
			Currency:         currency,
			BuyerChargeNanos: 1_000_000_000, AcceptedCeilingNanos: 1_000_000_000,
			SupplierEntitlementsNanos:  400_000_000,
			KnownVariableCostsNanos:    150_000_000,
			MercGrossSpreadNanos:       600_000_000,
			KnownCostContributionNanos: 450_000_000,
			// Deliberately populated to model an accepted historical body. The
			// canonical forecast reducer still refuses to publish it as settlement.
			TrueNetContributionNanos: &trueNet,
		},
		RuntimeCell: &FrozenRuntimeCellEconomics{
			MercTrueNetUSD: &runtimeNet, MercTrueNetStatus: "AVAILABLE",
		},
	}
}

// Keep fixture construction readable without baking the current integer into
// every reducer test. pricingDecisionDigest is the only validation needed here.
func contributionPricingDecisionVersionForTest() int { return pricingDecisionVersion }

func canonicalContributionFacts(t *testing.T, currency string) contributionJobFacts {
	t.Helper()
	pricing := canonicalContributionPricing(t, currency)
	pricingSHA, err := pricingDecisionDigest(pricing)
	must(t, err)
	processor := int64(100_000_000)
	return contributionJobFacts{
		Status: "complete", Pricing: pricing, PricingSHA256: pricingSHA, Currency: currency,
		HasSettlementLedger: true,
		BuyerGrossNanos:     1_000_000_000, SupplierCreditNanos: 400_000_000,
		ProcessorFeeNanos: &processor, ProcessorFeeSource: "test.processor.actual",
	}
}

func TestQuoteContributionHasOneAcceptedAuthorityAndNoTrueNet(t *testing.T) {
	pricing := canonicalContributionPricing(t, "usd")
	if pricing.FixedPoint.TrueNetContributionNanos == nil {
		t.Fatal("fixture must prove an accepted numeric field cannot escape as settlement")
	}
	current := pricing
	current.ContributionStage = ContributionStageAcceptedForecast
	if err := validateFixedPointPricing(current); err == nil ||
		!strings.Contains(err.Error(), "accepted distributed pricing forecast") {
		t.Fatalf("new accepted pricing was allowed to publish numeric true net: %v", err)
	}
	forecast, err := acceptedForecastContributionSettlement("quote", "q_dual", pricing)
	must(t, err)
	if forecast.Stage != ContributionStageAcceptedForecast || forecast.TrueNetNanos != nil {
		t.Fatalf("quote promoted accepted forecast into settlement: %+v", forecast)
	}
	if forecast.AcceptedKnownCostContributionNanos !=
		pricing.FixedPoint.KnownCostContributionNanos {
		t.Fatalf("quote lost accepted known-cost contribution: %+v", forecast)
	}
}

func TestContributionSettlementObservedOutputRebateIsNotDoubleSubtracted(t *testing.T) {
	facts := canonicalContributionFacts(t, "usd")
	// The observed-output settlement wrote a lower buyer charge than acceptance.
	// BuyerGross already contains the rebate; the informational rebate component
	// must not be subtracted a second time.
	facts.BuyerGrossNanos = 900_000_000
	facts.ObservedOutputRebateNanos = 100_000_000
	out, err := reduceContributionJobFacts(uuid.New(), facts)
	must(t, err)
	if out.Stage != ContributionStageFinalSettlement || out.TrueNetNanos == nil ||
		*out.TrueNetNanos != 350_000_000 {
		t.Fatalf("observed-output settlement = %+v, want 350000000 final nanos", out)
	}
	if out.ObservedOutputRebate.AmountNanos == nil ||
		*out.ObservedOutputRebate.AmountNanos != 100_000_000 {
		t.Fatalf("observed rebate evidence missing: %+v", out.ObservedOutputRebate)
	}
}

func TestContributionSettlementDisputeRefundAndClawbackUseNetLedgerFacts(t *testing.T) {
	facts := canonicalContributionFacts(t, "usd")
	facts.BuyerRefundNanos = 200_000_000
	facts.DisputeRefundNanos = 200_000_000
	facts.SupplierClawbackNanos = 100_000_000
	out, err := reduceContributionJobFacts(uuid.New(), facts)
	must(t, err)
	// buyer net 800 - supplier net 300 - processor 100 - control 50 = 350.
	if out.TrueNetNanos == nil || *out.TrueNetNanos != 350_000_000 {
		t.Fatalf("dispute/clawback contribution = %+v", out)
	}
	if out.DisputeSupplierClawback.AmountNanos == nil ||
		*out.DisputeSupplierClawback.AmountNanos != 100_000_000 {
		t.Fatalf("clawback source missing: %+v", out.DisputeSupplierClawback)
	}
}

func TestContributionSettlementSLARefundIsInsideBuyerNetExactlyOnce(t *testing.T) {
	facts := canonicalContributionFacts(t, "usd")
	facts.SLAGuaranteeSecs = 60
	met := false
	facts.SLAMet = &met
	facts.SLARefundNanos = 100_000_000
	out, err := reduceContributionJobFacts(uuid.New(), facts)
	must(t, err)
	if out.TrueNetNanos == nil || *out.TrueNetNanos != 350_000_000 ||
		out.BuyerNetAmount.AmountNanos == nil || *out.BuyerNetAmount.AmountNanos != 900_000_000 {
		t.Fatalf("SLA refund was omitted or counted twice: %+v", out)
	}
}

func TestContributionSettlementUsesActualProcessorFeeDivergence(t *testing.T) {
	facts := canonicalContributionFacts(t, "usd")
	actualProcessor := int64(150_000_000)
	facts.ProcessorFeeNanos = &actualProcessor
	out, err := reduceContributionJobFacts(uuid.New(), facts)
	must(t, err)
	if out.TrueNetNanos == nil || *out.TrueNetNanos != 400_000_000 ||
		!strings.Contains(out.ProcessorFee.Source, "test.processor.actual") {
		t.Fatalf("actual processor allocation did not supersede forecast: %+v", out)
	}
}

func TestContributionSettlementDeductsPlatformSubsidyExactlyOnce(t *testing.T) {
	facts := canonicalContributionFacts(t, "usd")
	facts.SubsidyNanos = 50_000_000
	out, err := reduceContributionJobFacts(uuid.New(), facts)
	must(t, err)
	if out.TrueNetNanos == nil || *out.TrueNetNanos != 400_000_000 {
		t.Fatalf("subsidy was omitted or deducted more than once: %+v", out)
	}
}

func TestContributionSettlementDigestAndPricingKeyAreImmutable(t *testing.T) {
	facts := canonicalContributionFacts(t, "usd")
	out, err := reduceContributionJobFacts(uuid.New(), facts)
	must(t, err)
	mutant := out
	changed := *mutant.ProcessorFee.AmountNanos + 1
	mutant.ProcessorFee.AmountNanos = &changed
	if err := validateContributionSettlement(mutant); err == nil ||
		!strings.Contains(err.Error(), "digest mismatch") {
		t.Fatalf("settlement component mutation escaped digest: %v", err)
	}

	facts.PricingSHA256 = strings.Repeat("a", 64)
	if _, err := reduceContributionJobFacts(uuid.New(), facts); err == nil ||
		!strings.Contains(err.Error(), "durable job authority") {
		t.Fatalf("pricing-digest substitution escaped settlement key: %v", err)
	}
}

func TestContributionSettlementRefusesUnprovedExactComponentNanos(t *testing.T) {
	for name, mutate := range map[string]func(*PricingDecision){
		"sub-micro control component": func(pricing *PricingDecision) {
			pricing.ControlPlaneCost.Amount += 0.0000005
		},
		"nonconserving component aggregate": func(pricing *PricingDecision) {
			pricing.FixedPoint.KnownVariableCostsNanos++
		},
	} {
		t.Run(name, func(t *testing.T) {
			facts := canonicalContributionFacts(t, "usd")
			mutate(&facts.Pricing)
			if err := validateModeledCostsAccountedInFixedPoint(facts.Pricing); err == nil {
				t.Fatal("fixed-point validation accepted unproved exact component nanos")
			}
			pricingSHA, err := pricingDecisionDigest(facts.Pricing)
			must(t, err)
			facts.PricingSHA256 = pricingSHA
			out, err := reduceContributionJobFacts(uuid.New(), facts)
			must(t, err)
			if out.Stage == ContributionStageFinalSettlement || out.TrueNetNanos != nil {
				t.Fatalf("unproved component nanos reached final settlement: %+v", out)
			}
			blockers := strings.Join(out.Blockers, " ")
			if !strings.Contains(blockers, "accepted exact component authority") {
				t.Fatalf("exact-authority refusal is not explicit: %+v", out.Blockers)
			}
		})
	}
}

func TestContributionSettlementRefusesUnboundStorageEgressActuals(t *testing.T) {
	facts := canonicalContributionFacts(t, "usd")
	facts.Pricing.StorageCost = modeledCost(0.01, "accepted storage byte bound")
	facts.Pricing.EgressCost = modeledCost(0.02, "accepted egress byte bound")
	facts.Pricing.FixedPoint.KnownVariableCostsNanos += 30_000_000
	facts.Pricing.FixedPoint.KnownCostContributionNanos -= 30_000_000
	accepted := facts.Pricing.FixedPoint.KnownCostContributionNanos
	facts.Pricing.FixedPoint.TrueNetContributionNanos = &accepted
	pricingSHA, err := pricingDecisionDigest(facts.Pricing)
	must(t, err)
	facts.PricingSHA256 = pricingSHA

	out, err := reduceContributionJobFacts(uuid.New(), facts)
	must(t, err)
	if out.Stage != ContributionStageProvisionalSettlement || out.TrueNetNanos != nil ||
		out.StorageCost.Status != contributionComponentUnknown ||
		out.EgressCost.Status != contributionComponentUnknown {
		t.Fatalf("accepted or caller-supplied transfer costs closed finality: %+v", out)
	}
	blockers := strings.Join(out.Blockers, " ")
	if !strings.Contains(blockers, "source-bound artifact/transfer settlement provenance") {
		t.Fatalf("storage/egress provenance refusal missing: %+v", out.Blockers)
	}
}

func TestContributionRollupBucketsCurrenciesWithoutSummingThem(t *testing.T) {
	usdFacts := canonicalContributionFacts(t, "usd")
	usd, err := reduceContributionJobFacts(uuid.New(), usdFacts)
	must(t, err)
	cadFacts := canonicalContributionFacts(t, "cad")
	cad, err := reduceContributionJobFacts(uuid.New(), cadFacts)
	must(t, err)
	buckets, err := reduceContributionRollup([]*ContributionSettlement{&cad, &usd})
	must(t, err)
	if len(buckets) != 2 || buckets[0].Currency != "cad" || buckets[1].Currency != "usd" ||
		buckets[0].TrueNetContributionNanos != 450_000_000 ||
		buckets[1].TrueNetContributionNanos != 450_000_000 {
		t.Fatalf("mixed currencies were summed or not deterministically bucketed: %+v", buckets)
	}
}

func TestContributionReceiptSnapshotCannotHybridizeConcurrentRefundAndClawback(t *testing.T) {
	ctx, store, pool, fixture, job, tasks, _ := currentUniformMoneyPathJob(t)
	mustf(t, store.SubmitJobTx(ctx, job, tasks), "submit contribution snapshot fixture: %v")

	// Establish the RR snapshot before the adjustment transaction commits.
	snapshot, err := pool.BeginTx(ctx, pgx.TxOptions{
		IsoLevel: pgx.RepeatableRead, AccessMode: pgx.ReadOnly,
	})
	must(t, err)
	defer snapshot.Rollback(ctx)
	var status string
	must(t, snapshot.QueryRow(ctx, `SELECT status FROM jobs WHERE id=$1`, fixture.JobID).Scan(&status))

	adjustment, err := pool.Begin(ctx)
	must(t, err)
	if _, err := adjustment.Exec(ctx, `
		INSERT INTO ledger_entries
		  (kind,buyer_id,task_id,amount_usd,currency,payout_status,payout_ref)
		VALUES ('buyer_refund',$1,$2,0.010000,$3,'released',$4)`,
		fixture.BuyerID, tasks[0].ID, job.PricingDecision.Currency,
		"contribution-snapshot-refund-"+uuid.NewString()); err != nil {
		_ = adjustment.Rollback(ctx)
		t.Fatalf("insert concurrent buyer refund: %v", err)
	}
	if _, err := adjustment.Exec(ctx, `
		INSERT INTO ledger_entries
		  (kind,supplier_id,task_id,amount_usd,currency,payout_status,payout_ref)
		VALUES ('clawback',$1,$2,-0.005000,$3,'clawed_back',$4)`,
		fixture.SupplierID, tasks[0].ID, job.PricingDecision.Currency,
		"contribution-snapshot-clawback-"+uuid.NewString()); err != nil {
		_ = adjustment.Rollback(ctx)
		t.Fatalf("insert concurrent supplier clawback: %v", err)
	}
	mustf(t, adjustment.Commit(ctx), "commit concurrent contribution adjustments: %v")

	before, err := loadContributionJobFacts(ctx, snapshot, fixture.JobID)
	must(t, err)
	if before.BuyerRefundNanos != 0 || before.SupplierClawbackNanos != 0 {
		t.Fatalf("repeatable-read receipt hybridized post-snapshot adjustments: %+v", before)
	}
	must(t, snapshot.Commit(ctx))

	fresh, err := pool.BeginTx(ctx, pgx.TxOptions{
		IsoLevel: pgx.RepeatableRead, AccessMode: pgx.ReadOnly,
	})
	must(t, err)
	defer fresh.Rollback(ctx)
	after, err := loadContributionJobFacts(ctx, fresh, fixture.JobID)
	must(t, err)
	if after.BuyerRefundNanos != 10_000_000 || after.SupplierClawbackNanos != 5_000_000 {
		t.Fatalf("fresh receipt did not observe the atomic adjustment pair: %+v", after)
	}
}

func TestCallerSuppliedCostSettlementRowCannotClearContributionBlockers(t *testing.T) {
	ctx, store, _, fixture, job, tasks, _ := currentUniformMoneyPathJob(t)
	mustf(t, store.SubmitJobTx(ctx, job, tasks), "submit unbound-cost fixture: %v")
	if job.PricingDecision.StorageCost.Status != pricingCostModeled &&
		job.PricingDecision.EgressCost.Status != pricingCostModeled {
		t.Fatalf("fixture lacks an applicable transfer cost: %+v", job.PricingDecision)
	}

	before, err := store.ContributionSettlementForJob(ctx, fixture.JobID)
	must(t, err)
	actuals, err := settleStorageEgressFromBytes(job.PricingDecision, 0, 0, time.Now())
	must(t, err)
	mustf(t, store.PersistCostSettlementActuals(ctx, fixture.JobID, actuals),
		"persist caller-supplied cost row: %v")
	after, err := store.ContributionSettlementForJob(ctx, fixture.JobID)
	must(t, err)

	if before.SettlementSHA256 != after.SettlementSHA256 {
		t.Fatalf("unbound job_cost_settlements row changed canonical contribution: before=%+v after=%+v",
			before, after)
	}
	if after.TrueNetNanos != nil || after.Stage == ContributionStageFinalSettlement {
		t.Fatalf("unbound cost row cleared finality: %+v", after)
	}
	for _, component := range []ContributionSettlementComponent{
		after.StorageCost, after.EgressCost,
	} {
		if component.Status == contributionComponentUnknown &&
			!strings.Contains(component.Source, "source-bound") {
			t.Fatalf("transfer-cost refusal lost its provenance requirement: %+v", component)
		}
		if strings.Contains(component.Source, "job_cost_settlements") {
			t.Fatalf("caller-supplied row masqueraded as canonical source: %+v", component)
		}
	}
}

func TestContributionBatchSettlementMatchesSingleJobAuthority(t *testing.T) {
	ctx, store, _, fixture, job, tasks, _ := currentUniformMoneyPathJob(t)
	mustf(t, store.SubmitJobTx(ctx, job, tasks), "submit batch settlement fixture: %v")

	single, err := store.ContributionSettlementForJob(ctx, fixture.JobID)
	must(t, err)
	batched, err := store.contributionSettlementsForJobs(ctx, []uuid.UUID{fixture.JobID})
	must(t, err)
	got, ok := batched[fixture.JobID]
	if !ok {
		t.Fatalf("batched settlement omitted job %s", fixture.JobID)
	}
	if !reflect.DeepEqual(*single, *got) {
		t.Fatalf("batched settlement changed canonical authority:\nsingle=%+v\nbatched=%+v", single, got)
	}
	rollup, err := store.TrueNetContributionForAccount(ctx, fixture.BuyerID)
	must(t, err)
	wantFinal := 0
	wantUnknown := 1
	if single.TrueNetNanos != nil {
		wantFinal, wantUnknown = 1, 0
	}
	if rollup.JobsWithoutDecision != 0 || rollup.JobsWithTrueNet != wantFinal ||
		rollup.JobsWithUnknownCosts != wantUnknown || rollup.Currency != single.Key.Currency {
		t.Fatalf("account rollup did not use the batched authority: %+v", rollup)
	}
	if single.TrueNetNanos != nil && rollup.TrueNetContributionNanos != *single.TrueNetNanos {
		t.Fatalf("account rollup changed true-net authority: %+v vs %+v", rollup, single)
	}
}
