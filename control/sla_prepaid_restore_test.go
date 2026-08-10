package main

import (
	"testing"
	"time"

	"github.com/google/uuid"
)

// G070 P1 — SLA miss must return the prepaid-debited premium to materialised
// prepaid. SettleJobSLA historically wrote sla_refund on the ledger only; the
// balance stayed reduced so the platform kept the premium cash and an admin
// prepaid refund could not return it.

func TestSLAMissRestoresPrepaidPremiumMaterialisation(t *testing.T) {
	installSettlementCurrencyForTest(t, "usd")
	ctx, store, pool := openIsolatedTestStore(t)

	buyerID := uuid.New()
	jobID := uuid.New()
	const (
		premiumMicros    int64 = 150_000 // $0.15
		chargeableMicros int64 = 1_000_000
	)
	if _, err := pool.Exec(ctx, `INSERT INTO buyers (id,email,free_credit_usd) VALUES ($1,$2,0)`,
		buyerID, buyerID.String()+"@sla-prepaid-restore.invalid"); err != nil {
		t.Fatal(err)
	}
	// Materialised prepaid already reduced by the finalize-time SLA debit —
	// balance sits at 0 while the prepaid_debit ledger row still records D.
	if _, err := pool.Exec(ctx, `
		INSERT INTO buyer_prepaid_balances (buyer_id, currency, balance_micros, updated_at)
		VALUES ($1,'usd',0,now())
		ON CONFLICT (buyer_id,currency) DO UPDATE SET balance_micros=0, updated_at=now()`,
		buyerID); err != nil {
		t.Fatal(err)
	}

	createdAt := time.Now().UTC().Add(-2 * time.Minute)
	mergedAt := time.Now().UTC()
	if _, err := pool.Exec(ctx, `
		INSERT INTO jobs
		  (id,buyer_id,status,job_type,input_ref,currency,created_at,terminal_at,
		   results_merged_at,actual_usd,sla_guarantee_secs,sla_premium_usd,prepaid_required)
		VALUES ($1,$2,'complete','embed','sla/prepaid-restore','usd',$3,$4,$5,
		        ($6::numeric/1000000),1,($7::numeric/1000000),true)`,
		jobID, buyerID, createdAt, mergedAt, mergedAt, chargeableMicros, premiumMicros); err != nil {
		t.Fatal(err)
	}
	// Ledger: SLA premium buyer_charge + matching prepaid_debit (finalize shape).
	// SettleJobSLA only needs jobs.sla_* columns + the prepaid_debit row.
	buyer := buyerID
	if _, err := insertLedgerEntryIfAbsentByRefTx(ctx, pool, ledgerInsert{
		Kind: KindBuyerCharge, BuyerID: &buyer, AmountMicros: -premiumMicros, Currency: "usd",
		PayoutStatus: PayoutReleased, PayoutRef: slaPremiumChargeRef(jobID),
	}); err != nil {
		t.Fatalf("seed SLA buyer_charge: %v", err)
	}
	if _, err := insertLedgerEntryIfAbsentByRefTx(ctx, pool, ledgerInsert{
		Kind: KindPrepaidDebit, BuyerID: &buyer, AmountMicros: -premiumMicros, Currency: "usd",
		CurrencyAuthority: ledgerCurrencyAuthorityPrepaid,
		PayoutStatus:      PayoutReleased, PayoutRef: prepaidSLAPremiumDebitRef(jobID),
	}); err != nil {
		t.Fatalf("seed SLA prepaid_debit: %v", err)
	}

	balBefore, err := store.BuyerPrepaidBalanceMicros(ctx, buyerID)
	must(t, err)
	if balBefore != 0 {
		t.Fatalf("prepaid before SLA settle=%d, want 0 (premium already materialised-debited)", balBefore)
	}

	result, err := store.SettleJobSLA(ctx, jobID)
	mustf(t, err, "SettleJobSLA: %v")
	if !result.Decided || result.Met {
		t.Fatalf("SLA result=%+v, want decided miss", result)
	}
	wantRefund := slaRefundAmount(microsToUSD(premiumMicros), microsToUSD(chargeableMicros))
	if usdToMicros(result.RefundUSD) != usdToMicros(wantRefund) {
		t.Fatalf("SLA refund=%f, want %f", result.RefundUSD, wantRefund)
	}

	balAfter, err := store.BuyerPrepaidBalanceMicros(ctx, buyerID)
	must(t, err)
	wantRestore := usdToMicros(wantRefund)
	if balAfter != wantRestore {
		t.Fatalf("prepaid after SLA miss=%d, want restored premium %d — "+
			"SettleJobSLA credited ledger only and left materialised prepaid reduced",
			balAfter, wantRestore)
	}

	// Idempotent: second settle is a no-op for funding.
	result2, err := store.SettleJobSLA(ctx, jobID)
	mustf(t, err, "second SettleJobSLA: %v")
	if result2.Decided {
		t.Fatalf("second settle decided again: %+v", result2)
	}
	balFinal, err := store.BuyerPrepaidBalanceMicros(ctx, buyerID)
	must(t, err)
	if balFinal != balAfter {
		t.Fatalf("SLA restore double-credited: %d → %d", balAfter, balFinal)
	}
}
