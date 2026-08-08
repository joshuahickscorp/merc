package main

import (
	"context"
	"sync"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
)

// buyerRefundFixture is a complete three-row settlement (charge + credit +
// platform take) plus optional prepaid debit / card collection, ready for an
// upheld dispute.
type buyerRefundFixture struct {
	buyerID, supplierID    uuid.UUID
	jobID, taskID, entryID uuid.UUID
	chargeMicros           int64
	supplierMicros         int64
	platformMicros         int64
	paymentIntent          string
	chargeID               string
}

type buyerRefundOpts struct {
	withCardCollection bool
	withPrepaidDebit   bool
	prepaidBalance     int64 // initial prepaid balance before debit; 0 → charge amount
	currency           string
	// secondTask adds another settled task on the same job (for multi-task caps).
	secondTask bool
}

func seedBuyerRefundFixture(
	t *testing.T,
	ctx context.Context,
	pool *pgxpool.Pool,
	opts buyerRefundOpts,
) buyerRefundFixture {
	t.Helper()
	if opts.currency == "" {
		opts.currency = "usd"
	}
	// $1.25 charge → $1.00 supplier + $0.25 platform (conserved).
	f := buyerRefundFixture{
		buyerID:        uuid.New(),
		supplierID:     uuid.New(),
		jobID:          uuid.New(),
		taskID:         uuid.New(),
		entryID:        uuid.New(),
		chargeMicros:   1_250_000,
		supplierMicros: 1_000_000,
		platformMicros: 250_000,
		paymentIntent:  "pi_refund_" + uuid.NewString(),
		chargeID:       "ch_refund_" + uuid.NewString(),
	}

	chargeStatus := "not_attempted"
	var stripePI any
	var chargeReq, chargeRecv, chargeCur any
	if opts.withCardCollection {
		chargeStatus = "charged"
		stripePI = f.paymentIntent
		cents := f.chargeMicros / microUSDPerCent
		chargeReq, chargeRecv, chargeCur = cents, cents, opts.currency
	}

	if _, err := pool.Exec(ctx, `
		INSERT INTO buyers (id, email) VALUES ($1, $2)`,
		f.buyerID, f.buyerID.String()+"@refund.invalid"); err != nil {
		t.Fatalf("seed buyer: %v", err)
	}
	if _, err := pool.Exec(ctx, `
		INSERT INTO suppliers (id, email, reputation, status)
		VALUES ($1, $2, 0.5, 'active')`,
		f.supplierID, f.supplierID.String()+"@refund.invalid"); err != nil {
		t.Fatalf("seed supplier: %v", err)
	}
	if _, err := pool.Exec(ctx, `
		INSERT INTO jobs
		  (id, buyer_id, status, job_type, input_ref, charge_status, currency,
		   stripe_pi, charge_requested_cents, charge_received_cents, charge_currency,
		   terminal_at, actual_usd)
		VALUES ($1,$2,'complete','embed','refund/input',$3,$4,$5,$6,$7,$8,now(),
		        ($9::numeric / 1000000))`,
		f.jobID, f.buyerID, chargeStatus, opts.currency, stripePI,
		chargeReq, chargeRecv, chargeCur, f.chargeMicros); err != nil {
		t.Fatalf("seed job: %v", err)
	}
	if _, err := pool.Exec(ctx, `
		INSERT INTO tasks (id, job_id, status, verification_outcome, completed_at)
		VALUES ($1, $2, 'complete', 'pass', now())`, f.taskID, f.jobID); err != nil {
		t.Fatalf("seed task: %v", err)
	}
	// Three-row settlement through the production writer shape (task/kind unique).
	for _, row := range []struct {
		kind, status    string
		buyer, supplier *uuid.UUID
		micros          int64
	}{
		{KindBuyerCharge, PayoutReleased, &f.buyerID, nil, -f.chargeMicros},
		{KindSupplierCredit, PayoutHeld, nil, &f.supplierID, f.supplierMicros},
		{KindPlatformTake, PayoutReleased, nil, nil, f.platformMicros},
	} {
		if _, err := pool.Exec(ctx, `
			INSERT INTO ledger_entries
			  (id, kind, buyer_id, supplier_id, task_id, amount_usd, currency, payout_status, release_at)
			VALUES (
			  gen_random_uuid(), $1, $2, $3, $4,
			  ($5::numeric / 1000000), $6, $7,
			  CASE WHEN $1 = 'supplier_credit' THEN now() - interval '1 minute' ELSE NULL END
			)`,
			row.kind, row.buyer, row.supplier, f.taskID, row.micros, opts.currency, row.status); err != nil {
			t.Fatalf("seed ledger %s: %v", row.kind, err)
		}
	}
	// Pin supplier credit id for payout assertions when needed.
	if err := pool.QueryRow(ctx, `
		SELECT id FROM ledger_entries WHERE task_id=$1 AND kind='supplier_credit'`,
		f.taskID).Scan(&f.entryID); err != nil {
		t.Fatalf("load supplier credit id: %v", err)
	}

	if opts.withCardCollection {
		if _, err := pool.Exec(ctx, `
			INSERT INTO buyer_cash_collections
			  (payment_intent, charge_id, buyer_id, source_kind, job_id,
			   requested_cents, received_cents, currency)
			VALUES ($1,$2,$3,'job',$4,$5,$5,$6)`,
			f.paymentIntent, f.chargeID, f.buyerID, f.jobID,
			f.chargeMicros/microUSDPerCent, opts.currency); err != nil {
			t.Fatalf("seed cash collection: %v", err)
		}
	}

	if opts.withPrepaidDebit {
		bal := opts.prepaidBalance
		if bal == 0 {
			bal = f.chargeMicros
		}
		if _, err := pool.Exec(ctx, `
			INSERT INTO buyer_prepaid_balances (buyer_id, balance_micros)
			VALUES ($1, $2)`, f.buyerID, bal); err != nil {
			t.Fatalf("seed prepaid balance: %v", err)
		}
		// Debit after seed so the materialised balance reflects spent liability.
		if _, err := pool.Exec(ctx, `
			UPDATE buyer_prepaid_balances
			   SET balance_micros = balance_micros - $2
			 WHERE buyer_id = $1`, f.buyerID, f.chargeMicros); err != nil {
			t.Fatalf("apply prepaid spend: %v", err)
		}
		if _, err := pool.Exec(ctx, `
			INSERT INTO ledger_entries
			  (kind, buyer_id, task_id, amount_usd, currency, payout_status)
			VALUES ('prepaid_debit', $1, $2, ($3::numeric / 1000000), $4, 'released')`,
			f.buyerID, f.taskID, -f.chargeMicros, opts.currency); err != nil {
			t.Fatalf("seed prepaid_debit: %v", err)
		}
	}

	if opts.secondTask {
		task2 := uuid.New()
		if _, err := pool.Exec(ctx, `
			INSERT INTO tasks (id, job_id, status, verification_outcome, completed_at)
			VALUES ($1, $2, 'complete', 'pass', now())`, task2, f.jobID); err != nil {
			t.Fatalf("seed second task: %v", err)
		}
		for _, row := range []struct {
			kind   string
			buyer  *uuid.UUID
			sup    *uuid.UUID
			micros int64
			status string
		}{
			{KindBuyerCharge, &f.buyerID, nil, -f.chargeMicros, PayoutReleased},
			{KindSupplierCredit, nil, &f.supplierID, f.supplierMicros, PayoutHeld},
			{KindPlatformTake, nil, nil, f.platformMicros, PayoutReleased},
		} {
			if _, err := pool.Exec(ctx, `
				INSERT INTO ledger_entries
				  (kind, buyer_id, supplier_id, task_id, amount_usd, currency, payout_status, release_at)
				VALUES ($1,$2,$3,$4,($5::numeric/1000000),$6,$7,
				        CASE WHEN $1='supplier_credit' THEN now()-interval '1 minute' ELSE NULL END)`,
				row.kind, row.buyer, row.sup, task2, row.micros, opts.currency, row.status); err != nil {
				t.Fatalf("seed second-task ledger %s: %v", row.kind, err)
			}
		}
	}
	return f
}

func TestBuyerRefundNeverExceedsChargeUnderInterleavedRetries(t *testing.T) {
	ctx, store, pool := openIsolatedTestStore(t)
	t.Setenv("MERC_CANARY_MODE", "false")
	t.Setenv("MERC_CANARY_DISABLE_DECISION_REF", "test:buyer-refund-cap")
	f := seedBuyerRefundFixture(t, ctx, pool, buyerRefundOpts{withCardCollection: true})

	disputeID, err := store.RecordDispute(ctx, f.jobID, f.buyerID, "output does not match submitted input")
	mustf(t, err, "RecordDispute: %v")
	// Interleave: resolve, then re-resolve, then attempt a raw extra refund insert
	// that would over-refund if the writer did not fail closed / unique-key.
	for i := 0; i < 5; i++ {
		mustf(t, store.resolveDispute(ctx, disputeID, "upheld"), "resolve attempt %d: %v", i)
	}
	var refunds, charges int
	var refundMicros, chargeMicros int64
	if err := pool.QueryRow(ctx, `
		SELECT
		  COUNT(*) FILTER (WHERE kind='buyer_refund'),
		  COUNT(*) FILTER (WHERE kind='buyer_charge'),
		  COALESCE((SUM(amount_usd) FILTER (WHERE kind='buyer_refund')*1000000)::bigint,0),
		  COALESCE((-SUM(amount_usd) FILTER (WHERE kind='buyer_charge')*1000000)::bigint,0)
		  FROM ledger_entries WHERE task_id=$1`, f.taskID).
		Scan(&refunds, &charges, &refundMicros, &chargeMicros); err != nil {
		t.Fatal(err)
	}
	if refunds != 1 || charges != 1 {
		t.Fatalf("refunds=%d charges=%d, want 1 each", refunds, charges)
	}
	if refundMicros != chargeMicros || refundMicros != f.chargeMicros {
		t.Fatalf("refund=%d charge=%d want %d", refundMicros, chargeMicros, f.chargeMicros)
	}
	// Direct over-refund attempt must not succeed: unique (task_id, kind) rejects
	// a second buyer_refund, and the post-condition would refuse a larger amount.
	_, err = pool.Exec(ctx, `
		INSERT INTO ledger_entries
		  (kind, buyer_id, task_id, amount_usd, currency, payout_status)
		VALUES ('buyer_refund', $1, $2, 0.01, 'usd', 'released')`,
		f.buyerID, f.taskID)
	if err == nil {
		t.Fatal("second buyer_refund row was accepted; uniqueness failed")
	}
	var net float64
	if err := pool.QueryRow(ctx, `
		SELECT COALESCE(SUM(amount_usd),0)::float8 FROM ledger_entries
		 WHERE task_id=$1 AND kind IN ('buyer_charge','buyer_refund')`, f.taskID).
		Scan(&net); err != nil {
		t.Fatal(err)
	}
	if net > 0 {
		t.Fatalf("net charge+refund = %v > 0 (over-refunded)", net)
	}
}

func TestBuyerRefundDoubleResolutionOnce(t *testing.T) {
	ctx, store, pool := openIsolatedTestStore(t)
	t.Setenv("MERC_CANARY_MODE", "false")
	t.Setenv("MERC_CANARY_DISABLE_DECISION_REF", "test:buyer-refund-double")
	f := seedBuyerRefundFixture(t, ctx, pool, buyerRefundOpts{withCardCollection: true})

	disputeID, err := store.RecordDispute(ctx, f.jobID, f.buyerID, "independent review shows a bad result")
	mustf(t, err, "RecordDispute: %v")
	mustf(t, store.SetDisputeStatus(ctx, disputeID, "upheld"), "uphold: %v")
	mustf(t, store.resolveDispute(ctx, disputeID, "upheld"), "second resolve: %v")
	var refundRows, receiptRows int
	if err := pool.QueryRow(ctx, `
		SELECT
		  (SELECT count(*) FROM ledger_entries WHERE task_id=$1 AND kind='buyer_refund'),
		  (SELECT count(*) FROM job_dispute_refunds WHERE dispute_id=$2)`,
		f.taskID, disputeID).Scan(&refundRows, &receiptRows); err != nil {
		t.Fatal(err)
	}
	if refundRows != 1 || receiptRows != 1 {
		t.Fatalf("double resolve refund_rows=%d receipt_rows=%d, want 1/1", refundRows, receiptRows)
	}
}

func TestBuyerRefundConcurrentResolutionOnce(t *testing.T) {
	ctx, store, pool := openIsolatedTestStore(t)
	t.Setenv("MERC_CANARY_MODE", "false")
	t.Setenv("MERC_CANARY_DISABLE_DECISION_REF", "test:buyer-refund-concurrent")
	f := seedBuyerRefundFixture(t, ctx, pool, buyerRefundOpts{withCardCollection: true})

	disputeID, err := store.RecordDispute(ctx, f.jobID, f.buyerID, "concurrent resolution must refund once")
	mustf(t, err, "RecordDispute: %v")

	const workers = 8
	var wg sync.WaitGroup
	errs := make(chan error, workers)
	wg.Add(workers)
	for i := 0; i < workers; i++ {
		go func() {
			defer wg.Done()
			errs <- store.resolveDispute(ctx, disputeID, "upheld")
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		mustf(t, err, "concurrent resolve: %v")
	}
	var refundRows int
	var refundMicros int64
	if err := pool.QueryRow(ctx, `
		SELECT count(*),
		       COALESCE((SUM(amount_usd)*1000000)::bigint,0)
		  FROM ledger_entries WHERE task_id=$1 AND kind='buyer_refund'`, f.taskID).
		Scan(&refundRows, &refundMicros); err != nil {
		t.Fatal(err)
	}
	if refundRows != 1 || refundMicros != f.chargeMicros {
		t.Fatalf("concurrent refund rows=%d micros=%d want 1/%d", refundRows, refundMicros, f.chargeMicros)
	}
	var receipts int
	if err := pool.QueryRow(ctx,
		`SELECT count(*) FROM job_dispute_refunds WHERE dispute_id=$1`, disputeID).
		Scan(&receipts); err != nil {
		t.Fatal(err)
	}
	if receipts != 1 {
		t.Fatalf("concurrent receipts=%d, want 1", receipts)
	}
}

func TestBuyerRefundConservesMoneyChargeClawbackRefund(t *testing.T) {
	ctx, store, pool := openIsolatedTestStore(t)
	t.Setenv("MERC_CANARY_MODE", "false")
	t.Setenv("MERC_CANARY_DISABLE_DECISION_REF", "test:buyer-refund-conserve")
	f := seedBuyerRefundFixture(t, ctx, pool, buyerRefundOpts{withCardCollection: true})

	// Pre-dispute: charge + credit + take = 0.
	var sumBefore float64
	if err := pool.QueryRow(ctx, `
		SELECT COALESCE(SUM(amount_usd),0)::float8 FROM ledger_entries WHERE task_id=$1`,
		f.taskID).Scan(&sumBefore); err != nil {
		t.Fatal(err)
	}
	if sumBefore != 0 {
		t.Fatalf("pre-dispute ledger sum=%v, want 0", sumBefore)
	}

	disputeID, err := store.RecordDispute(ctx, f.jobID, f.buyerID, "bad result, full money path")
	mustf(t, err, "RecordDispute: %v")
	mustf(t, store.SetDisputeStatus(ctx, disputeID, "upheld"), "uphold: %v")

	// After charge → clawback → buyer_refund → platform_refund, sum is 0.
	var sumAfter float64
	if err := pool.QueryRow(ctx, `
		SELECT COALESCE(SUM(amount_usd),0)::float8 FROM ledger_entries WHERE task_id=$1`,
		f.taskID).Scan(&sumAfter); err != nil {
		t.Fatal(err)
	}
	if sumAfter != 0 {
		t.Fatalf("post-refund ledger sum=%v, want 0 (not conserved)", sumAfter)
	}

	var kinds map[string]float64
	rows, err := pool.Query(ctx, `
		SELECT kind, amount_usd::float8 FROM ledger_entries WHERE task_id=$1 ORDER BY kind`, f.taskID)
	must(t, err)
	defer rows.Close()
	kinds = map[string]float64{}
	for rows.Next() {
		var k string
		var a float64
		must(t, rows.Scan(&k, &a))
		kinds[k] = a
	}
	if kinds[KindBuyerCharge] != -1.25 || kinds[KindBuyerRefund] != 1.25 ||
		kinds[KindSupplierCredit] != 1.0 || kinds[KindClawback] != -1.0 ||
		kinds[KindPlatformTake] != 0.25 || kinds[KindPlatformRefund] != -0.25 {
		t.Fatalf("unexpected kind amounts: %+v", kinds)
	}
}

func TestBuyerRefundVisibleOnReceipt(t *testing.T) {
	ctx, store, pool := openIsolatedTestStore(t)
	t.Setenv("MERC_CANARY_MODE", "false")
	t.Setenv("MERC_CANARY_DISABLE_DECISION_REF", "test:buyer-refund-receipt")
	f := seedBuyerRefundFixture(t, ctx, pool, buyerRefundOpts{withCardCollection: true})

	disputeID, err := store.RecordDispute(ctx, f.jobID, f.buyerID, "receipt must show the refund")
	mustf(t, err, "RecordDispute: %v")
	mustf(t, store.SetDisputeStatus(ctx, disputeID, "upheld"), "uphold: %v")

	inv, err := store.JobInvoice(ctx, f.jobID, f.buyerID)
	mustf(t, err, "JobInvoice: %v")
	if inv.BuyerRefundUSD != 1.25 {
		t.Fatalf("BuyerRefundUSD=%v, want 1.25", inv.BuyerRefundUSD)
	}
	if inv.NetChargedUSD == nil || *inv.NetChargedUSD != 0 {
		t.Fatalf("NetChargedUSD=%v, want 0", inv.NetChargedUSD)
	}
	if inv.RefundCause != "dispute_upheld" {
		t.Fatalf("RefundCause=%q, want dispute_upheld", inv.RefundCause)
	}
	if inv.RefundFundingDestination != refundFundingExternalCardPending {
		t.Fatalf("funding=%q, want %q", inv.RefundFundingDestination, refundFundingExternalCardPending)
	}

	// Job event text is buyer-visible on the timeline.
	var eventText string
	if err := pool.QueryRow(ctx, `
		SELECT buyer_text FROM job_events
		 WHERE job_id=$1 AND event='dispute_upheld'
		 ORDER BY created_at DESC LIMIT 1`, f.jobID).Scan(&eventText); err != nil {
		t.Fatal(err)
	}
	if eventText == "" || !containsAll(eventText, "refund", "pending external settlement") {
		t.Fatalf("buyer event text missing refund cause: %q", eventText)
	}

	// Receipt assembly carries the invoice fields.
	rc := assembleClearingReceipt(f.jobID, inv.Status, nil, nil, nil, nil, inv, Verification{}, nil, nil)
	if rc.Invoice == nil || rc.Invoice.BuyerRefundUSD != 1.25 || rc.Invoice.RefundCause != "dispute_upheld" {
		t.Fatalf("clearing receipt lost refund: %+v", rc.Invoice)
	}
}

func TestBuyerRefundCurrencyCannotMix(t *testing.T) {
	ctx, store, pool := openIsolatedTestStore(t)
	t.Setenv("MERC_CANARY_MODE", "false")
	t.Setenv("MERC_CANARY_DISABLE_DECISION_REF", "test:buyer-refund-currency")
	f := seedBuyerRefundFixture(t, ctx, pool, buyerRefundOpts{
		withCardCollection: true,
		currency:           "usd",
	})

	// Plant a CAD charge on the same task — should be refused by the currency
	// predicate and/or the mixed-currency post-condition. The task currency
	// trigger binds ledger rows to job currency, so a direct CAD insert must fail.
	_, err := pool.Exec(ctx, `
		INSERT INTO ledger_entries
		  (kind, buyer_id, task_id, amount_usd, currency, payout_status)
		VALUES ('buyer_charge', $1, $2, -0.50, 'cad', 'released')`,
		f.buyerID, f.taskID)
	if err == nil {
		t.Fatal("CAD buyer_charge on a USD job was accepted; currency authority broken")
	}

	disputeID, err := store.RecordDispute(ctx, f.jobID, f.buyerID, "currency authority must hold on refund")
	mustf(t, err, "RecordDispute: %v")
	mustf(t, store.SetDisputeStatus(ctx, disputeID, "upheld"), "uphold: %v")
	var refundCurrency string
	if err := pool.QueryRow(ctx, `
		SELECT currency FROM ledger_entries
		 WHERE task_id=$1 AND kind='buyer_refund'`, f.taskID).Scan(&refundCurrency); err != nil {
		t.Fatal(err)
	}
	if refundCurrency != "usd" {
		t.Fatalf("refund currency=%q, want usd (job authority)", refundCurrency)
	}
}

func TestBuyerRefundRestoresPrepaidBalance(t *testing.T) {
	ctx, store, pool := openIsolatedTestStore(t)
	t.Setenv("MERC_CANARY_MODE", "false")
	t.Setenv("MERC_CANARY_DISABLE_DECISION_REF", "test:buyer-refund-prepaid")
	f := seedBuyerRefundFixture(t, ctx, pool, buyerRefundOpts{
		withPrepaidDebit: true,
		prepaidBalance:   1_250_000,
	})

	var balBefore int64
	if err := pool.QueryRow(ctx,
		`SELECT balance_micros FROM buyer_prepaid_balances WHERE buyer_id=$1`, f.buyerID).
		Scan(&balBefore); err != nil {
		t.Fatal(err)
	}
	if balBefore != 0 {
		t.Fatalf("prepaid before refund=%d, want 0 (fully spent)", balBefore)
	}

	disputeID, err := store.RecordDispute(ctx, f.jobID, f.buyerID, "prepaid path must restore liability")
	mustf(t, err, "RecordDispute: %v")
	mustf(t, store.SetDisputeStatus(ctx, disputeID, "upheld"), "uphold: %v")

	var balAfter int64
	if err := pool.QueryRow(ctx,
		`SELECT balance_micros FROM buyer_prepaid_balances WHERE buyer_id=$1`, f.buyerID).
		Scan(&balAfter); err != nil {
		t.Fatal(err)
	}
	if balAfter != f.chargeMicros {
		t.Fatalf("prepaid after refund=%d, want %d", balAfter, f.chargeMicros)
	}
	inv, err := store.JobInvoice(ctx, f.jobID, f.buyerID)
	must(t, err)
	if inv.RefundFundingDestination != refundFundingPrepaidBalance {
		t.Fatalf("funding=%q, want prepaid_balance", inv.RefundFundingDestination)
	}
	// No second restore on re-resolve.
	mustf(t, store.resolveDispute(ctx, disputeID, "upheld"), "second resolve: %v")
	if err := pool.QueryRow(ctx,
		`SELECT balance_micros FROM buyer_prepaid_balances WHERE buyer_id=$1`, f.buyerID).
		Scan(&balAfter); err != nil {
		t.Fatal(err)
	}
	if balAfter != f.chargeMicros {
		t.Fatalf("prepaid double-credited: %d", balAfter)
	}
}

func TestBuyerRefundAppearsInFreeCreditBalanceMath(t *testing.T) {
	// Free credit remaining already subtracts buyer_charge and buyer_refund.
	// Prove a refund restores remaining credit.
	ctx, store, pool := openIsolatedTestStore(t)
	t.Setenv("MERC_CANARY_MODE", "false")
	t.Setenv("MERC_CANARY_DISABLE_DECISION_REF", "test:buyer-refund-free-credit")
	f := seedBuyerRefundFixture(t, ctx, pool, buyerRefundOpts{})

	if _, err := pool.Exec(ctx, `
		UPDATE buyers SET free_credit_usd = 5.00 WHERE id=$1`, f.buyerID); err != nil {
		t.Fatal(err)
	}
	before, err := store.BuyerFreeCreditRemaining(ctx, f.buyerID)
	must(t, err)
	// 5.00 - 1.25 charge = 3.75
	if before != 3.75 {
		t.Fatalf("free credit before refund=%v, want 3.75", before)
	}

	disputeID, err := store.RecordDispute(ctx, f.jobID, f.buyerID, "free credit must see the refund")
	mustf(t, err, "RecordDispute: %v")
	mustf(t, store.SetDisputeStatus(ctx, disputeID, "upheld"), "uphold: %v")
	after, err := store.BuyerFreeCreditRemaining(ctx, f.buyerID)
	must(t, err)
	if after != 5.00 {
		t.Fatalf("free credit after refund=%v, want 5.00", after)
	}
}

func containsAll(s string, parts ...string) bool {
	for _, p := range parts {
		if !containsFold(s, p) {
			return false
		}
	}
	return true
}

func containsFold(s, sub string) bool {
	return len(s) >= len(sub) && (s == sub ||
		len(sub) == 0 ||
		(len(s) > 0 && containsFoldIndex(s, sub)))
}

func containsFoldIndex(s, sub string) bool {
	// small local helper to avoid strings import churn in assertions
	sl, subl := []rune(s), []rune(sub)
	for i := 0; i+len(subl) <= len(sl); i++ {
		ok := true
		for j := 0; j < len(subl); j++ {
			a, b := sl[i+j], subl[j]
			if a >= 'A' && a <= 'Z' {
				a += 'a' - 'A'
			}
			if b >= 'A' && b <= 'Z' {
				b += 'a' - 'A'
			}
			if a != b {
				ok = false
				break
			}
		}
		if ok {
			return true
		}
	}
	return false
}
