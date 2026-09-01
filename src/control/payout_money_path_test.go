package main

import (
	"context"
	"errors"
	"sync"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
)

func countFundingRows(t *testing.T, ctx context.Context, pool *pgxpool.Pool, paymentIntent string) (n int, reserved int64) {
	t.Helper()
	if err := pool.QueryRow(ctx, `
		SELECT count(*)::int, COALESCE(sum(amount_cents),0)::bigint
		  FROM supplier_payout_funding
		 WHERE source_kind='buyer_collection' AND collection_payment_intent=$1`,
		paymentIntent).Scan(&n, &reserved); err != nil {
		t.Fatalf("count funding: %v", err)
	}
	return n, reserved
}

func TestReservePayoutFundingUnderfundingFailsClosed(t *testing.T) {
	ctx, store, pool := openPayoutTestStore(t)
	// Disabling canary requires a recorded decision reference, because the same
	// switch also opens self-serve signup; one decision gates both.
	t.Setenv("MERC_CANARY_MODE", "false")
	t.Setenv("MERC_CANARY_DISABLE_DECISION_REF", "TEST-money-path")

	// Credit needs 200 cents; collection only holds 50.
	f := seedPayoutFixture(t, ctx, pool, payoutFixtureOpts{
		creditUSD:       2.00,
		collectionCents: 50,
	})

	claimed, ok, err := store.ClaimPayout(ctx, f.entryID)
	mustf(t, err, "ClaimPayout: %v")
	if ok {
		t.Fatalf("underfunded claim succeeded: %+v", claimed)
	}
	var status string
	if err := pool.QueryRow(ctx,
		`SELECT payout_status FROM ledger_entries WHERE id=$1`, f.entryID).Scan(&status); err != nil {
		t.Fatal(err)
	}
	if status != PayoutAwaitingFunding {
		t.Fatalf("underfunded credit status=%q, want %q", status, PayoutAwaitingFunding)
	}
	n, reserved := countFundingRows(t, ctx, pool, f.paymentIntent)
	if n != 0 || reserved != 0 {
		t.Fatalf("underfunding still reserved funding rows=%d reserved=%d", n, reserved)
	}
	due, err := store.DuePayouts(ctx, 100)
	mustf(t, err, "DuePayouts after underfunding: %v")
	if !dueContains(due, f.entryID) {
		t.Fatal("awaiting_funding payout disappeared from the retry queue")
	}
}

func TestAwaitingFundingPayoutRetriesAfterLatePrepaidTopup(t *testing.T) {
	installSettlementCurrencyForTest(t, "usd")
	t.Setenv("MERC_CANARY_MODE", "false")
	t.Setenv("MERC_CANARY_DISABLE_DECISION_REF", "TEST-late-topup-payout-retry")
	ctx, store, pool := openIsolatedTestStore(t)

	buyerID, supplierID := uuid.New(), uuid.New()
	jobID, taskID, entryID := uuid.New(), uuid.New(), uuid.New()
	if _, err := pool.Exec(ctx, `INSERT INTO buyers (id,email) VALUES ($1,$2)`,
		buyerID, buyerID.String()+"@late-topup.invalid"); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO suppliers (id,email,status) VALUES ($1,$2,'active')`,
		supplierID, supplierID.String()+"@late-topup.invalid"); err != nil {
		t.Fatal(err)
	}

	creditTopup := func(cents int64) string {
		t.Helper()
		key := "late-topup-" + uuid.NewString()
		if _, err := store.BeginPrepaidTopup(ctx, key, buyerID, cents); err != nil {
			t.Fatalf("BeginPrepaidTopup: %v", err)
		}
		paymentIntent := "pi_late_topup_" + uuid.NewString()
		mustf(t, store.CreditPrepaidTopup(ctx, key, buyerID, ChargeResult{
			PaymentIntentID: paymentIntent,
			ChargeID:        "ch_late_topup_" + uuid.NewString(),
			RequestedCents:  cents,
			ReceivedCents:   cents,
			Currency:        "usd",
		}), "CreditPrepaidTopup: %v")
		return paymentIntent
	}
	creditTopup(50)

	if _, err := pool.Exec(ctx, `
		INSERT INTO jobs
		  (id,buyer_id,status,job_type,input_ref,prepaid_required,charge_status,currency,terminal_at)
		VALUES ($1,$2,'complete','embed','late-topup/job',true,'not_attempted','usd',now())`,
		jobID, buyerID); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `
		INSERT INTO tasks (id,job_id,status,verification_outcome,completed_at)
		VALUES ($1,$2,'complete','pass',now())`, taskID, jobID); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `
		INSERT INTO ledger_entries
		  (id,kind,supplier_id,task_id,amount_usd,currency,payout_status,release_at)
		VALUES ($1,'supplier_credit',$2,$3,1.00,'usd','held',now()-interval '1 minute')`,
		entryID, supplierID, taskID); err != nil {
		t.Fatal(err)
	}

	if _, sent, err := store.ClaimPayout(ctx, entryID); err != nil || sent {
		t.Fatalf("underfunded first claim sent=%v err=%v", sent, err)
	}
	var status string
	if err := pool.QueryRow(ctx, `SELECT payout_status FROM ledger_entries WHERE id=$1`, entryID).Scan(&status); err != nil {
		t.Fatal(err)
	}
	if status != PayoutAwaitingFunding {
		t.Fatalf("first claim status=%q, want %q", status, PayoutAwaitingFunding)
	}
	due, err := store.DuePayouts(ctx, 100)
	mustf(t, err, "DuePayouts while awaiting: %v")
	if !dueContains(due, entryID) {
		t.Fatal("awaiting_funding entry was not visible to the retry sweep")
	}

	latePaymentIntent := creditTopup(100)
	due, err = store.DuePayouts(ctx, 100)
	mustf(t, err, "DuePayouts after late top-up: %v")
	if !dueContains(due, entryID) {
		t.Fatal("late top-up did not keep the awaiting payout claimable")
	}
	claimed, sent, err := store.ClaimPayout(ctx, entryID)
	mustf(t, err, "retry ClaimPayout: %v")
	if !sent || claimed.RequestedCents != 100 {
		t.Fatalf("late top-up retry sent=%v requested=%d", sent, claimed.RequestedCents)
	}
	if err := pool.QueryRow(ctx, `SELECT payout_status FROM ledger_entries WHERE id=$1`, entryID).Scan(&status); err != nil {
		t.Fatal(err)
	}
	if status != PayoutSending {
		t.Fatalf("late top-up retry status=%q, want %q", status, PayoutSending)
	}
	var fundingPI string
	if err := pool.QueryRow(ctx, `
		SELECT collection_payment_intent FROM supplier_payout_funding WHERE ledger_entry_id=$1`, entryID).
		Scan(&fundingPI); err != nil {
		t.Fatal(err)
	}
	if fundingPI != latePaymentIntent {
		t.Fatalf("late top-up funding payment intent=%q, want %q", fundingPI, latePaymentIntent)
	}
}

func TestReservePayoutFundingIdempotentDoesNotDoubleReserve(t *testing.T) {
	ctx, _, pool := openPayoutTestStore(t)

	f := seedPayoutFixture(t, ctx, pool, payoutFixtureOpts{creditUSD: 1.00})

	tx, err := pool.Begin(ctx)
	must(t, err)
	defer tx.Rollback(ctx)

	taskID := f.taskID
	id1, funded1, err := reservePayoutFunding(ctx, tx, f.entryID, &taskID, f.creditCents, "usd")
	if err != nil || !funded1 || id1 == uuid.Nil {
		t.Fatalf("first reserve: id=%v funded=%v err=%v", id1, funded1, err)
	}
	id2, funded2, err := reservePayoutFunding(ctx, tx, f.entryID, &taskID, f.creditCents, "usd")
	if err != nil || !funded2 {
		t.Fatalf("second reserve: id=%v funded=%v err=%v", id2, funded2, err)
	}
	if id1 != id2 {
		t.Fatalf("idempotent reserve returned different funding ids %v vs %v", id1, id2)
	}
	must(t, tx.Commit(ctx))
	n, reserved := countFundingRows(t, ctx, pool, f.paymentIntent)
	if n != 1 || reserved != f.creditCents {
		t.Fatalf("double-reserve leaked rows=%d reserved=%d want 1/%d", n, reserved, f.creditCents)
	}
}

func TestReservePayoutFundingConcurrentDoesNotOversubscribe(t *testing.T) {
	ctx, store, pool := openPayoutTestStore(t)
	// Disabling canary requires a recorded decision reference, because the same
	// switch also opens self-serve signup; one decision gates both.
	t.Setenv("MERC_CANARY_MODE", "false")
	t.Setenv("MERC_CANARY_DISABLE_DECISION_REF", "TEST-money-path")

	// Collection holds 150 cents; two credits each need 100 → only one can fund.
	f := seedPayoutFixture(t, ctx, pool, payoutFixtureOpts{
		creditUSD:       1.00,
		collectionCents: 150,
	})
	siblingID, _, siblingCents := seedSiblingCredit(t, ctx, pool, f, 1.00)
	if siblingCents != 100 {
		t.Fatalf("sibling cents=%d, want 100", siblingCents)
	}

	const contenders = 2
	type result struct {
		id  uuid.UUID
		ok  bool
		err error
	}
	results := make(chan result, contenders)
	var wg sync.WaitGroup
	for _, id := range []uuid.UUID{f.entryID, siblingID} {
		wg.Add(1)
		go func(entryID uuid.UUID) {
			defer wg.Done()
			_, ok, err := store.ClaimPayout(ctx, entryID)
			results <- result{id: entryID, ok: ok, err: err}
		}(id)
	}
	wg.Wait()
	close(results)

	var claimed int
	for r := range results {
		if r.err != nil {
			t.Fatalf("concurrent claim %s: %v", r.id, r.err)
		}
		if r.ok {
			claimed++
		}
	}
	if claimed != 1 {
		t.Fatalf("concurrent claims that advanced to sending = %d, want 1", claimed)
	}
	n, reserved := countFundingRows(t, ctx, pool, f.paymentIntent)
	if reserved > 150 {
		t.Fatalf("oversubscribed collection: reserved=%d > available=150 (rows=%d)", reserved, n)
	}
	if reserved != 100 {
		t.Fatalf("reserved=%d rows=%d, want exactly 100 cents for one funded credit", reserved, n)
	}
}

func TestAuthorizePayoutSubsidyWithinBalanceRefuseBeyondIdempotent(t *testing.T) {
	ctx, store, pool := openPayoutTestStore(t)

	// Two credits of $1.00 each; fund capacity 100 cents funds only one.
	f1 := seedPayoutFixture(t, ctx, pool, payoutFixtureOpts{
		creditUSD:   1.00,
		noBuyerCash: true, // subsidy is the funding path
	})
	// Re-seed second credit with its own job/task but shared actor pattern.
	f2 := seedPayoutFixture(t, ctx, pool, payoutFixtureOpts{
		creditUSD:   1.00,
		noBuyerCash: true,
	})
	// Use f1's admin actor for both fund and authorizations.
	actor := f1.actor

	fundRef := "fund-" + uuid.NewString()
	created, err := store.CreateSubsidyFund(ctx, actor, fundRef, "treasury-"+uuid.NewString(), 100, "test subsidy capacity")
	if err != nil || !created {
		t.Fatalf("CreateSubsidyFund: created=%v err=%v", created, err)
	}

	authRef1 := "auth-" + uuid.NewString()
	ok, err := store.AuthorizePayoutSubsidy(ctx, actor, f1.entryID, fundRef, authRef1, "cover first liability")
	if err != nil || !ok {
		t.Fatalf("first subsidy: ok=%v err=%v", ok, err)
	}

	// Idempotent re-auth with same operation key (fund + authorization_ref + reason + amount).
	ok, err = store.AuthorizePayoutSubsidy(ctx, actor, f1.entryID, fundRef, authRef1, "cover first liability")
	mustf(t, err, "idempotent subsidy re-auth: %v")
	if ok {
		t.Fatal("idempotent subsidy reported created=true on replay")
	}
	var fundingRows int
	if err := pool.QueryRow(ctx, `
		SELECT count(*) FROM supplier_payout_funding
		 WHERE subsidy_fund_id=(SELECT id FROM platform_subsidy_funds WHERE fund_ref=$1)`,
		fundRef).Scan(&fundingRows); err != nil {
		t.Fatal(err)
	}
	if fundingRows != 1 {
		t.Fatalf("idempotent re-auth produced %d funding rows, want 1", fundingRows)
	}

	// Beyond balance: second liability needs another 100 cents; fund is exhausted.
	authRef2 := "auth-" + uuid.NewString()
	ok, err = store.AuthorizePayoutSubsidy(ctx, actor, f2.entryID, fundRef, authRef2, "cover second liability")
	if !errors.Is(err, errSubsidyFundUnavailable) {
		t.Fatalf("over-capacity subsidy err=%v ok=%v, want errSubsidyFundUnavailable", err, ok)
	}
	if ok {
		t.Fatal("over-capacity subsidy reported authorized")
	}
	if err := pool.QueryRow(ctx, `
		SELECT count(*) FROM supplier_payout_funding
		 WHERE subsidy_fund_id=(SELECT id FROM platform_subsidy_funds WHERE fund_ref=$1)`,
		fundRef).Scan(&fundingRows); err != nil {
		t.Fatal(err)
	}
	if fundingRows != 1 {
		t.Fatalf("over-capacity still wrote funding rows=%d", fundingRows)
	}
}

func TestFinalizePayoutTerminalStatesAndSecondIsNoOp(t *testing.T) {
	ctx, store, pool := openPayoutTestStore(t)
	// Disabling canary requires a recorded decision reference, because the same
	// switch also opens self-serve signup; one decision gates both.
	t.Setenv("MERC_CANARY_MODE", "false")
	t.Setenv("MERC_CANARY_DISABLE_DECISION_REF", "TEST-money-path")

	// --- released path ---
	released := seedPayoutFixture(t, ctx, pool, payoutFixtureOpts{creditUSD: 1.00})
	if _, ok, err := store.ClaimPayout(ctx, released.entryID); err != nil || !ok {
		t.Fatalf("claim released fixture: ok=%v err=%v", ok, err)
	}
	if _, err := store.FinalizePayout(ctx, released.entryID, PayoutResult{
		Ref: "fake", SentCents: released.creditCents, Currency: "usd", CashMoved: true,
	}); err == nil {
		t.Fatal("FinalizePayout accepted an arbitrary cash reference")
	}
	ref := "tr_released_" + uuid.NewString()
	state, err := store.FinalizePayout(ctx, released.entryID, PayoutResult{
		Ref: ref, SentCents: released.creditCents, Currency: "usd", CashMoved: true,
	})
	if err != nil || state != PayoutReleased {
		t.Fatalf("finalize released: state=%q err=%v", state, err)
	}
	// A second finalize must not produce a second payment.  The store refuses it
	// outright rather than silently no-opping, which is the stronger behaviour: a
	// double-finalize attempt is a real anomaly and should surface, not be
	// absorbed.  Assert the safety property -- no second cash row -- and accept
	// either mechanism, so this test cannot be satisfied by a silent swallow.
	state2, err := store.FinalizePayout(ctx, released.entryID, PayoutResult{
		Ref: ref, SentCents: released.creditCents, Currency: "usd", CashMoved: true,
	})
	if err == nil && state2 != PayoutReleased {
		t.Fatalf("second finalize advanced the payout to %q", state2)
	}
	var cashRows int
	var sentSum int64
	if err := pool.QueryRow(ctx, `
		SELECT count(*)::int, COALESCE(sum(sent_cents),0)::bigint
		  FROM supplier_payout_operations
		 WHERE ledger_entry_id=$1 AND cash_moved`, released.entryID).Scan(&cashRows, &sentSum); err != nil {
		t.Fatal(err)
	}
	if cashRows != 1 || sentSum != released.creditCents {
		t.Fatalf("released cash evidence rows=%d sent=%d, want 1/%d", cashRows, sentSum, released.creditCents)
	}

	// --- exported path (no cash moved) ---
	exported := seedPayoutFixture(t, ctx, pool, payoutFixtureOpts{creditUSD: 1.00})
	if _, ok, err := store.ClaimPayout(ctx, exported.entryID); err != nil || !ok {
		t.Fatalf("claim exported fixture: ok=%v err=%v", ok, err)
	}
	exportRef := "manual-export:" + uuid.NewString()
	state, err = store.FinalizePayout(ctx, exported.entryID, PayoutResult{
		Ref: exportRef, Currency: "usd", CashMoved: false,
	})
	if err != nil || state != PayoutExported {
		t.Fatalf("finalize exported: state=%q err=%v", state, err)
	}
	// Same rule as the released path: refuse or no-op, never a second export.
	state2, err = store.FinalizePayout(ctx, exported.entryID, PayoutResult{
		Ref: exportRef, Currency: "usd", CashMoved: false,
	})
	if err == nil && state2 != PayoutExported {
		t.Fatalf("second export finalize advanced the payout to %q", state2)
	}
	var exportCash bool
	if err := pool.QueryRow(ctx, `
		SELECT cash_moved FROM supplier_payout_operations WHERE ledger_entry_id=$1`,
		exported.entryID).Scan(&exportCash); err != nil {
		t.Fatal(err)
	}
	if exportCash {
		t.Fatal("exported payout incorrectly recorded cash_moved")
	}

	// --- reversal_required: dispute-clawed credit that crossed the provider boundary ---
	rev := seedPayoutFixture(t, ctx, pool, payoutFixtureOpts{creditUSD: 1.00})
	if _, ok, err := store.ClaimPayout(ctx, rev.entryID); err != nil || !ok {
		t.Fatalf("claim reversal fixture: ok=%v err=%v", ok, err)
	}
	// Mark ledger+op as reversal_required while still pre-cash (simulates dispute after claim).
	if _, err := pool.Exec(ctx, `
		UPDATE ledger_entries SET payout_status='reversal_required' WHERE id=$1`, rev.entryID); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `
		UPDATE supplier_payout_operations SET status='reversal_required' WHERE ledger_entry_id=$1`,
		rev.entryID); err != nil {
		t.Fatal(err)
	}
	revRef := "tr_rev_" + uuid.NewString()
	// Cash path with reversal_required → still terminal reversal_required (cash crossed boundary).
	state, err = store.FinalizePayout(ctx, rev.entryID, PayoutResult{
		Ref: revRef, SentCents: rev.creditCents, Currency: "usd", CashMoved: true,
	})
	if err != nil || state != PayoutReversalRequired {
		t.Fatalf("finalize reversal: state=%q err=%v", state, err)
	}
	state2, err = store.FinalizePayout(ctx, rev.entryID, PayoutResult{
		Ref: revRef, SentCents: rev.creditCents, Currency: "usd", CashMoved: true,
	})
	if err != nil || state2 != PayoutReversalRequired {
		t.Fatalf("second reversal finalize: state=%q err=%v", state2, err)
	}
}

func TestResolveDisputeBlocksPayoutTerminalControlsNoDoubleRefund(t *testing.T) {
	// Own database: DuePayouts and the dispute freeze are platform-wide, so a
	// sibling test's held credit is indistinguishable from this one's.  Under
	// -race the interleaving shifts and the collision becomes visible.
	ctx, store, pool := openIsolatedTestStore(t)
	// Disabling canary requires a recorded decision reference, because the same
	// switch also opens self-serve signup; one decision gates both.
	t.Setenv("MERC_CANARY_MODE", "false")
	t.Setenv("MERC_CANARY_DISABLE_DECISION_REF", "TEST-money-path")

	f := seedPayoutFixture(t, ctx, pool, payoutFixtureOpts{creditUSD: 1.25})

	// Open dispute blocks claim.
	disputeID, err := store.RecordDispute(ctx, f.jobID, f.buyerID, "buyer reports mismatched output")
	mustf(t, err, "RecordDispute: %v")
	if _, claimed, err := store.ClaimPayout(ctx, f.entryID); err != nil || claimed {
		t.Fatalf("open dispute still claimed: claimed=%v err=%v", claimed, err)
	}
	due, err := store.DuePayouts(ctx, 100)
	must(t, err)
	for _, e := range due {
		if e.ID == f.entryID {
			t.Fatal("open-dispute credit appeared in DuePayouts")
		}
	}

	// Rejected → may proceed.
	mustf(t, store.SetDisputeStatus(ctx, disputeID, "rejected"), "reject: %v")
	due, err = store.DuePayouts(ctx, 100)
	must(t, err)
	found := false
	for _, e := range due {
		if e.ID == f.entryID {
			found = true
		}
	}
	if !found {
		t.Fatal("rejected dispute did not re-enable DuePayouts")
	}

	// File successor and uphold → clawback once.
	upheldID, err := store.RecordDispute(ctx, f.jobID, f.buyerID, "independent review still shows a bad result")
	mustf(t, err, "successor dispute: %v")
	mustf(t, store.SetDisputeStatus(ctx, upheldID, "upheld"), "uphold: %v")
	var clawbacks int
	if err := pool.QueryRow(ctx, `
		SELECT count(*) FROM ledger_entries
		 WHERE task_id=$1 AND kind='clawback'`, f.taskID).Scan(&clawbacks); err != nil {
		t.Fatal(err)
	}
	if clawbacks != 1 {
		t.Fatalf("upheld clawbacks=%d, want 1", clawbacks)
	}
	var payoutStatus string
	if err := pool.QueryRow(ctx,
		`SELECT payout_status FROM ledger_entries WHERE id=$1`, f.entryID).Scan(&payoutStatus); err != nil {
		t.Fatal(err)
	}
	if payoutStatus != PayoutClawedBack {
		t.Fatalf("upheld credit status=%q, want %q", payoutStatus, PayoutClawedBack)
	}

	// Resolving twice does not double-refund.
	err = store.resolveDispute(ctx, upheldID, "upheld")
	if err != nil {
		// already-resolved same status is a commit no-op; other errors fail
		t.Fatalf("second resolve: %v", err)
	}
	if err := pool.QueryRow(ctx, `
		SELECT count(*) FROM ledger_entries
		 WHERE task_id=$1 AND kind='clawback'`, f.taskID).Scan(&clawbacks); err != nil {
		t.Fatal(err)
	}
	if clawbacks != 1 {
		t.Fatalf("double resolve clawbacks=%d, want 1", clawbacks)
	}

	// Terminal upheld still blocks payout.
	if _, claimed, err := store.ClaimPayout(ctx, f.entryID); err != nil || claimed {
		t.Fatalf("clawed-back claim: claimed=%v err=%v", claimed, err)
	}
}

func TestWorkerEarningsMatchesLedgerRows(t *testing.T) {
	ctx, store, pool := openPayoutTestStore(t)
	// Disabling canary requires a recorded decision reference, because the same
	// switch also opens self-serve signup; one decision gates both.
	t.Setenv("MERC_CANARY_MODE", "false")
	t.Setenv("MERC_CANARY_DISABLE_DECISION_REF", "TEST-money-path")

	// One released cash payout + one still-held credit with a sub-cent remainder story.
	released := seedPayoutFixture(t, ctx, pool, payoutFixtureOpts{creditUSD: 2.50})
	if _, ok, err := store.ClaimPayout(ctx, released.entryID); err != nil || !ok {
		t.Fatalf("claim for earnings: ok=%v err=%v", ok, err)
	}
	ref := "tr_earn_" + uuid.NewString()
	if _, err := store.FinalizePayout(ctx, released.entryID, PayoutResult{
		Ref: ref, SentCents: released.creditCents, Currency: "usd", CashMoved: true,
	}); err != nil {
		t.Fatalf("finalize for earnings: %v", err)
	}

	// Held credit with fractional micros that produce carried remainder.
	// 1.234567 USD → 1234567 micros → 123 cents + 4567 remainder micros.
	held := seedPayoutFixture(t, ctx, pool, payoutFixtureOpts{creditUSD: 1.234567})
	// Force held credit onto the same supplier so earnings aggregates them.
	if _, err := pool.Exec(ctx, `
		UPDATE ledger_entries SET supplier_id=$2 WHERE id=$1`, held.entryID, released.supplierID); err != nil {
		t.Fatal(err)
	}
	// Persist minor-unit settlement for the held row so carried remainder is visible.
	tx, err := pool.Begin(ctx)
	must(t, err)
	liability := usdToMicros(1.234567)
	if _, _, err := persistMinorUnitSettlement(ctx, tx, held.entryID, liability); err != nil {
		tx.Rollback(ctx)
		t.Fatalf("persist settlement: %v", err)
	}
	must(t, tx.Commit(ctx))

	earnings, err := store.WorkerEarnings(ctx, released.supplierID)
	mustf(t, err, "WorkerEarnings: %v")

	// Recompute from ledger the same way a support engineer would.
	var balanceUSD, lifetimeUSD, carriedUSD float64
	if err := pool.QueryRow(ctx, `
		SELECT
		  COALESCE(SUM(op.sent_cents) FILTER (
		    WHERE le.payout_status = 'released' AND op.cash_moved = true
		      AND op.sent_cents > 0), 0)::float8 / 100.0,
		  COALESCE(SUM(le.amount_usd) FILTER (WHERE le.amount_usd > 0), 0),
		  COALESCE((SELECT a.accrued_microusd FROM supplier_payout_accruals a
		             WHERE a.supplier_id = $1), 0)::float8 / 1000000.0
		 FROM ledger_entries le
		 LEFT JOIN supplier_payout_operations op ON op.ledger_entry_id=le.id
		 WHERE le.supplier_id = $1 AND le.kind = 'supplier_credit'`,
		released.supplierID).Scan(&balanceUSD, &lifetimeUSD, &carriedUSD); err != nil {
		t.Fatal(err)
	}

	if earnings.BalanceUSD != balanceUSD {
		t.Fatalf("BalanceUSD=%v ledger=%v", earnings.BalanceUSD, balanceUSD)
	}
	if earnings.LifetimeUSD != lifetimeUSD {
		t.Fatalf("LifetimeUSD=%v ledger=%v", earnings.LifetimeUSD, lifetimeUSD)
	}
	// Carry is the ACCOUNT-level accrual, not a sum over per-entry settlement
	// rows. Each settlement records the carry after that entry -- a running
	// balance -- so summing them reports the same money once per entry. The
	// previous version of this assertion replicated that sum and therefore
	// pinned the bug as correct behaviour.
	if earnings.CarriedUSD != carriedUSD {
		t.Fatalf("CarriedUSD=%v accrual=%v", earnings.CarriedUSD, carriedUSD)
	}
	if earnings.CarriedUSD >= 0.01 {
		t.Fatalf("CarriedUSD=%v is at or above one cent and should have been paid",
			earnings.CarriedUSD)
	}
	if earnings.BalanceUSD != float64(released.creditCents)/100.0 {
		t.Fatalf("BalanceUSD=%v want released cash %v", earnings.BalanceUSD, float64(released.creditCents)/100.0)
	}
	if earnings.LastPayoutUSD == nil || *earnings.LastPayoutUSD != float64(released.creditCents)/100.0 {
		t.Fatalf("LastPayoutUSD=%v", earnings.LastPayoutUSD)
	}
	if earnings.NextPayoutAt == nil {
		t.Fatal("NextPayoutAt missing for held credit")
	}

	// Sanity: lifetime is sum of positive credits (released + held).
	wantLifetime := released.creditUSD + 1.234567
	if absFloat(earnings.LifetimeUSD-wantLifetime) > 1e-9 {
		t.Fatalf("LifetimeUSD=%v want ~%v", earnings.LifetimeUSD, wantLifetime)
	}
}

func absFloat(v float64) float64 {
	if v < 0 {
		return -v
	}
	return v
}
