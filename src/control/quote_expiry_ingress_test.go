package main

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"
)

func currentQuoteForJob(job *jobRow, quoteID uuid.UUID, expiresAt time.Time) Quote {
	return Quote{
		QuoteID: "q_" + quoteID.String(), bareID: quoteID,
		etaRawP50Secs: job.ETARawSecs,
		JobType:       job.JobType,
		Model:         job.ModelRef,
		Tier:          job.Tier,
		Currency:      job.EconomicPlan.Schedule.Currency,
		Workload:      job.WorkloadDecision,
		Placement:     job.PlacementRequirement,
		ComputePlan:   job.ComputePlan,
		Pricing:       job.PricingDecision,
		Time: QuoteTime{
			P50Secs:              job.ComputePlan.ETAP50Secs,
			P90Secs:              job.ComputePlan.ETAP90Secs,
			WorstCaseSecs:        job.ComputePlan.ETAWorstCaseSecs,
			ConfidenceBandMethod: job.ComputePlan.ETAConfidenceBandMethod,
		},
		Economics:   job.EconomicPlan,
		InputSHA256: job.WorkloadDecision.Binding.InputSHA256,
		ExpiresAt:   expiresAt,
	}
}

// A quote is checked once before the buyer uploads its input, but that check
// cannot authorize the later durable write. This regression crosses the TTL
// in the existing pre-write hook and proves SubmitJobTx rechecks database time
// inside its transaction.
func TestBoundQuoteExpiryCrossingAtDurableIngressWritesNothing(t *testing.T) {
	ctx, store, pool, fixture, job, tasks := seedCurrentIdentityLifecycleJob(t)

	quoteID := uuid.New()
	// Current quote insertion revalidates the complete physical schedule and can
	// take hundreds of milliseconds on an un-warmed isolated database. Leave a
	// real preflight window, then cross it deterministically in the hook below.
	expiresAt := time.Now().UTC().Add(3 * time.Second)
	quote := currentQuoteForJob(job, quoteID, expiresAt)
	mustf(t, store.InsertQuote(ctx, fixture.BuyerID, quote), "insert short-lived bound quote: %v")
	t.Cleanup(func() {
		cleanupCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		_, _ = pool.Exec(cleanupCtx, `DELETE FROM quotes WHERE id=$1`, quoteID)
	})
	bound, err := store.GetBindableQuote(ctx, quoteID, fixture.BuyerID)
	mustf(t, err, "preflight short-lived bound quote: %v")
	if bound.Expired {
		t.Fatal("short-lived quote expired before the preflight boundary")
	}

	job.QuoteID = quoteID
	previousHook := durableAdmissionPhysicalRecheckHook
	durableAdmissionPhysicalRecheckHook = func() {
		durableAdmissionPhysicalRecheckHook = nil
		if wait := time.Until(expiresAt.Add(50 * time.Millisecond)); wait > 0 {
			time.Sleep(wait)
		}
	}
	t.Cleanup(func() { durableAdmissionPhysicalRecheckHook = previousHook })

	err = store.SubmitJobTx(ctx, job, tasks)
	if !errors.Is(err, errBoundQuoteExpired) {
		t.Fatalf("durable submit after quote TTL error=%v, want errBoundQuoteExpired", err)
	}
	for name, query := range map[string]string{
		"jobs":     `SELECT count(*) FROM jobs WHERE id=$1`,
		"tasks":    `SELECT count(*) FROM tasks WHERE job_id=$1`,
		"plans":    `SELECT count(*) FROM job_economic_plans WHERE job_id=$1`,
		"reserves": `SELECT count(*) FROM job_economic_reserves WHERE job_id=$1`,
	} {
		var rows int
		mustf(t, pool.QueryRow(ctx, query, fixture.JobID).Scan(&rows), "count %s: %v", name)
		if rows != 0 {
			t.Fatalf("expired bound quote left %d %s rows", rows, name)
		}
	}
	if rows := countBuyerLedger(t, ctx, pool, fixture.BuyerID); rows != 0 {
		t.Fatalf("expired bound quote left %d buyer ledger rows", rows)
	}
}
