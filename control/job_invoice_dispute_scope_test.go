package main

import (
	"testing"
	"time"

	"github.com/google/uuid"
)

func TestJobInvoiceDisputeRefundScopedToOwningJob(t *testing.T) {
	// Two jobs, one buyer. A dispute-sla-refund on job2 must not appear on
	// job1's invoice, and must appear exactly once across both invoices.
	installSettlementCurrencyForTest(t, "usd")
	ctx, store, pool := openIsolatedTestStore(t)

	buyerID := uuid.New()
	job1 := uuid.New()
	job2 := uuid.New()
	dispute2 := uuid.New()
	now := time.Now().UTC()

	mustf(t, pool.QueryRow(ctx, `
		INSERT INTO buyers (id,email) VALUES ($1,$2) RETURNING id`,
		buyerID, buyerID.String()+"@invoice-scope.invalid").Scan(&buyerID),
		"insert invoice buyer: %v")

	for _, jobID := range []uuid.UUID{job1, job2} {
		_, err := pool.Exec(ctx, `
			INSERT INTO jobs
			  (id,buyer_id,status,job_type,tier,input_ref,currency,created_at,terminal_at,
			   results_merged_at,actual_usd,sla_guarantee_secs,sla_premium_usd)
			VALUES ($1,$2,'complete','embed','batch','inv/input','usd',$3,$3,$3,
			        1.0,60,0.50)`,
			jobID, buyerID, now)
		mustf(t, err, "insert job %s: %v", jobID)
		// Job-level SLA premium charge (no task_id).
		_, err = insertLedgerEntryIfAbsentByRefTx(ctx, pool, ledgerInsert{
			Kind: KindBuyerCharge, BuyerID: &buyerID,
			AmountMicros: -500_000, Currency: "usd",
			PayoutStatus: PayoutReleased, PayoutRef: slaPremiumChargeRef(jobID),
		})
		mustf(t, err, "insert sla premium for %s: %v", jobID)
	}

	// Dispute on job2 only, with a job-level dispute-sla-refund row.
	_, err := pool.Exec(ctx, `
		INSERT INTO disputes
		  (id,job_id,buyer_id,reason,status,created_at,filing_deadline,resolved_at)
		VALUES ($1,$2,$3,'invoice scope','upheld',$4,$5,$4)`,
		dispute2, job2, buyerID, now, now.Add(7*24*time.Hour))
	mustf(t, err, "insert dispute on job2: %v")
	refundRef := "dispute-sla-refund-" + dispute2.String()
	_, err = insertLedgerEntryIfAbsentByRefTx(ctx, pool, ledgerInsert{
		Kind: KindBuyerRefund, BuyerID: &buyerID,
		AmountMicros: 500_000, Currency: "usd",
		PayoutStatus: PayoutReleased, PayoutRef: refundRef,
	})
	mustf(t, err, "insert dispute-sla-refund for job2: %v")

	inv1, err := store.JobInvoice(ctx, job1, buyerID)
	mustf(t, err, "JobInvoice job1: %v")
	inv2, err := store.JobInvoice(ctx, job2, buyerID)
	mustf(t, err, "JobInvoice job2: %v")

	if inv1.BuyerRefundUSD != 0 {
		t.Fatalf("job1 invoice included job2 dispute refund: BuyerRefundUSD=%v", inv1.BuyerRefundUSD)
	}
	if inv1.NetChargedUSD == nil {
		t.Fatal("job1 NetChargedUSD is nil")
	}
	// job1: only its own -0.50 charge → net charged 0.50
	if got := *inv1.NetChargedUSD; got < 0.499 || got > 0.501 {
		t.Fatalf("job1 NetChargedUSD=%v, want ~0.50 without foreign refund", got)
	}

	if inv2.BuyerRefundUSD < 0.499 || inv2.BuyerRefundUSD > 0.501 {
		t.Fatalf("job2 BuyerRefundUSD=%v, want ~0.50 dispute refund", inv2.BuyerRefundUSD)
	}
	// Exactly once across both invoices.
	totalRefund := inv1.BuyerRefundUSD + inv2.BuyerRefundUSD
	if totalRefund < 0.499 || totalRefund > 0.501 {
		t.Fatalf("dispute refund counted across invoices as %v, want exactly ~0.50 once", totalRefund)
	}
}
