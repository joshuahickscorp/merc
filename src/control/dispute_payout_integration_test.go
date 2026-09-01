package main

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

func disputeRequest(jobID, buyerID uuid.UUID, body string) (*http.Request, *httptest.ResponseRecorder) {
	req := httptest.NewRequest(http.MethodPost, "/v1/jobs/"+jobID.String()+"/dispute", strings.NewReader(body))
	req.SetPathValue("id", jobID.String())
	req = req.WithContext(context.WithValue(req.Context(), ctxBuyer, &AuthResult{BuyerID: buyerID}))
	return req, httptest.NewRecorder()
}

type disputePayoutFixture struct {
	buyerID, otherBuyerID  uuid.UUID
	supplierID             uuid.UUID
	jobID, taskID, entryID uuid.UUID
}

func seedDisputePayoutFixture(
	t *testing.T,
	ctx context.Context,
	pool *pgxpool.Pool,
	jobStatus string,
) disputePayoutFixture {
	t.Helper()
	f := disputePayoutFixture{
		buyerID: uuid.New(), otherBuyerID: uuid.New(), supplierID: uuid.New(),
		jobID: uuid.New(), taskID: uuid.New(), entryID: uuid.New(),
	}
	statements := []struct {
		query string
		args  []any
	}{
		{`INSERT INTO suppliers (id,email,reputation,status) VALUES ($1,$2,0.5,'active')`,
			[]any{f.supplierID, f.supplierID.String() + "@dispute.invalid"}},
		{`INSERT INTO jobs (id,buyer_id,status,job_type,input_ref)
		  VALUES ($1,$2,$3,'embed','dispute/input')`, []any{f.jobID, f.buyerID, jobStatus}},
		{`INSERT INTO tasks
		    (id,job_id,status,verification_outcome,completed_at)
		  VALUES ($1,$2,'complete','pass',now())`, []any{f.taskID, f.jobID}},
		{`INSERT INTO ledger_entries
		    (id,kind,supplier_id,task_id,amount_usd,payout_status,release_at)
		  VALUES ($1,'supplier_credit',$2,$3,1.25,'held',now()-interval '1 minute')`,
			[]any{f.entryID, f.supplierID, f.taskID}},
	}
	for _, statement := range statements {
		if _, err := pool.Exec(ctx, statement.query, statement.args...); err != nil {
			t.Fatalf("seed dispute payout fixture: %v", err)
		}
	}
	return f
}

func dueContains(entries []DueHeldEntry, id uuid.UUID) bool {
	for _, entry := range entries {
		if entry.ID == id {
			return true
		}
	}
	return false
}

func TestDisputeFilingAtomicallyFreezesAndTerminalResolutionControlsPayout(t *testing.T) {
	// Own database: DuePayouts and the dispute freeze are platform-wide, so a
	// sibling test's held credit is indistinguishable from this one's. Under
	// -race the interleaving shifts and the collision becomes visible.
	ctx, store, pool := openIsolatedTestStore(t)
	t.Setenv("MERC_CANARY_MODE", "false")
	t.Setenv("MERC_CANARY_DISABLE_DECISION_REF", "test:dispute-payout-fixture")
	f := seedDisputePayoutFixture(t, ctx, pool, "complete")

	due, err := store.DuePayouts(ctx, 100)
	if err != nil || !dueContains(due, f.entryID) {
		t.Fatalf("credit was not initially due: present=%v err=%v", dueContains(due, f.entryID), err)
	}
	disputeID, err := store.RecordDispute(ctx, f.jobID, f.buyerID, " output does not match the submitted input ")
	mustf(t, err, "file dispute: %v")
	due, err = store.DuePayouts(ctx, 100)
	if err != nil || dueContains(due, f.entryID) {
		t.Fatalf("actively disputed credit remained due: present=%v err=%v", dueContains(due, f.entryID), err)
	}
	if _, claimed, err := store.ClaimPayout(ctx, f.entryID); err != nil || claimed {
		t.Fatalf("actively disputed credit claim = %v, %v", claimed, err)
	}
	if _, err := pool.Exec(ctx,
		`UPDATE ledger_entries SET payout_status='sending' WHERE id=$1`, f.entryID); err == nil {
		t.Fatal("database lifecycle guard allowed an active-dispute credit to cross into sending")
	}

	var holds, disputeEvents, jobEvents int
	if err := pool.QueryRow(ctx,
		`SELECT count(*) FROM dispute_payout_holds WHERE dispute_id=$1`, disputeID).Scan(&holds); err != nil {
		t.Fatal(err)
	}
	if err := pool.QueryRow(ctx,
		`SELECT count(*) FROM dispute_events WHERE dispute_id=$1 AND event='filed'`, disputeID).Scan(&disputeEvents); err != nil {
		t.Fatal(err)
	}
	if err := pool.QueryRow(ctx,
		`SELECT count(*) FROM job_events WHERE job_id=$1 AND event='dispute_filed'`, f.jobID).Scan(&jobEvents); err != nil {
		t.Fatal(err)
	}
	if holds != 1 || disputeEvents != 1 || jobEvents != 1 {
		t.Fatalf("atomic filing evidence = holds:%d dispute_events:%d job_events:%d", holds, disputeEvents, jobEvents)
	}

	mustf(t, store.SetDisputeStatus(ctx, disputeID, "rejected"), "reject dispute: %v")
	due, err = store.DuePayouts(ctx, 100)
	if err != nil || !dueContains(due, f.entryID) {
		t.Fatalf("rejected dispute did not re-enable held payout: present=%v err=%v", dueContains(due, f.entryID), err)
	}

	upheldID, err := store.RecordDispute(ctx, f.jobID, f.buyerID, "independent review still shows a bad result")
	mustf(t, err, "file successor dispute: %v")
	mustf(t, store.SetDisputeStatus(ctx, upheldID, "upheld"), "uphold dispute: %v")
	var payoutStatus, holdResolution string
	var clawbacks int
	if err := pool.QueryRow(ctx,
		`SELECT payout_status FROM ledger_entries WHERE id=$1`, f.entryID).Scan(&payoutStatus); err != nil {
		t.Fatal(err)
	}
	if err := pool.QueryRow(ctx,
		`SELECT resolution FROM dispute_payout_holds WHERE dispute_id=$1 AND ledger_entry_id=$2`,
		upheldID, f.entryID).Scan(&holdResolution); err != nil {
		t.Fatal(err)
	}
	if err := pool.QueryRow(ctx,
		`SELECT count(*) FROM ledger_entries WHERE task_id=$1 AND kind='clawback' AND amount_usd=-1.25`,
		f.taskID).Scan(&clawbacks); err != nil {
		t.Fatal(err)
	}
	if payoutStatus != PayoutClawedBack || holdResolution != "upheld" || clawbacks != 1 {
		t.Fatalf("upheld liability = status:%q resolution:%q clawbacks:%d", payoutStatus, holdResolution, clawbacks)
	}
	due, err = store.DuePayouts(ctx, 100)
	if err != nil || dueContains(due, f.entryID) {
		t.Fatalf("upheld liability became due: present=%v err=%v", dueContains(due, f.entryID), err)
	}
}

func TestRejectedDisputeRearmsInFlightPayoutWithoutCash(t *testing.T) {
	ctx, store, pool := openIsolatedTestStore(t)
	t.Setenv("MERC_CANARY_MODE", "false")
	t.Setenv("MERC_CANARY_DISABLE_DECISION_REF", "test:dispute-inflight-reject")
	f := seedPayoutFixture(t, ctx, pool, payoutFixtureOpts{creditUSD: 1.25})
	if _, sent, err := store.ClaimPayout(ctx, f.entryID); err != nil || !sent {
		t.Fatalf("claim in-flight fixture: sent=%v err=%v", sent, err)
	}
	disputeID, err := store.RecordDispute(ctx, f.jobID, f.buyerID, "provider outcome must survive a rejected dispute")
	mustf(t, err, "file in-flight dispute: %v")

	var heldStatus, heldOperation string
	var outcomeUnknown bool
	mustf(t, pool.QueryRow(ctx, `
		SELECT le.payout_status,op.status,op.outcome_unknown
		  FROM ledger_entries le JOIN supplier_payout_operations op ON op.ledger_entry_id=le.id
		 WHERE le.id=$1`, f.entryID).Scan(&heldStatus, &heldOperation, &outcomeUnknown),
		"inspect filed in-flight payout: %v")
	if heldStatus != PayoutReversalRequired || heldOperation != PayoutReversalRequired || !outcomeUnknown {
		t.Fatalf("filed in-flight payout=%s/%s unknown=%v, want reversal_required/reversal_required/true",
			heldStatus, heldOperation, outcomeUnknown)
	}

	mustf(t, store.SetDisputeStatus(ctx, disputeID, "rejected"), "reject in-flight dispute: %v")
	var payoutStatus, operationStatus string
	var retryable bool
	mustf(t, pool.QueryRow(ctx, `
		SELECT le.payout_status,op.status,op.outcome_unknown
		  FROM ledger_entries le JOIN supplier_payout_operations op ON op.ledger_entry_id=le.id
		 WHERE le.id=$1`, f.entryID).Scan(&payoutStatus, &operationStatus, &retryable),
		"inspect rejected in-flight payout: %v")
	if payoutStatus != PayoutOutcomeUnknown || operationStatus != PayoutOutcomeUnknown || !retryable {
		t.Fatalf("rejected in-flight payout=%s/%s unknown=%v, want outcome_unknown/outcome_unknown/true",
			payoutStatus, operationStatus, retryable)
	}
	if n, err := store.CountReversalRequired(ctx); err != nil || n != 0 {
		t.Fatalf("rejected no-cash payout left reversal pause count=%d err=%v", n, err)
	}
	unknown, err := store.ClaimOutcomeUnknownPayouts(ctx, 0, 24*time.Hour, 10)
	mustf(t, err, "claim rearmed outcome-unknown payout: %v")
	if !dueContains(unknown, f.entryID) {
		t.Fatalf("rejected in-flight payout was not rearmed for idempotent retry: %+v", unknown)
	}
}

func TestActiveDisputeDefersCashReversalUntilResolution(t *testing.T) {
	ctx, store, pool := openIsolatedTestStore(t)
	t.Setenv("MERC_CANARY_MODE", "false")
	t.Setenv("MERC_CANARY_DISABLE_DECISION_REF", "test:dispute-inflight-cash")
	f := seedPayoutFixture(t, ctx, pool, payoutFixtureOpts{creditUSD: 1.25})
	if _, sent, err := store.ClaimPayout(ctx, f.entryID); err != nil || !sent {
		t.Fatalf("claim cash fixture: sent=%v err=%v", sent, err)
	}
	disputeID, err := store.RecordDispute(ctx, f.jobID, f.buyerID, "hold cash until the dispute is resolved")
	mustf(t, err, "file cash dispute: %v")
	transferRef := "tr_dispute_" + uuid.NewString()
	state, err := store.FinalizePayout(ctx, f.entryID, PayoutResult{
		Ref: transferRef, SentCents: f.creditCents, Currency: f.currency, CashMoved: true,
	})
	if err != nil || state != PayoutReversalRequired {
		t.Fatalf("finalize cash during active dispute: state=%q err=%v", state, err)
	}

	claimed, err := store.ClaimReversals(ctx, time.Minute, 10)
	mustf(t, err, "claim active-dispute reversal: %v")
	if dueContainsReversal(claimed, f.entryID) {
		t.Fatalf("active dispute exposed cash reversal before resolution: %+v", claimed)
	}

	mustf(t, store.SetDisputeStatus(ctx, disputeID, "rejected"), "reject cash dispute: %v")
	var payoutStatus, operationStatus, storedTransfer string
	var cashMoved bool
	mustf(t, pool.QueryRow(ctx, `
		SELECT le.payout_status,op.status,op.cash_moved,op.transfer_ref
		  FROM ledger_entries le JOIN supplier_payout_operations op ON op.ledger_entry_id=le.id
		 WHERE le.id=$1`, f.entryID).Scan(&payoutStatus, &operationStatus, &cashMoved, &storedTransfer),
		"inspect released cash payout: %v")
	if payoutStatus != PayoutReleased || operationStatus != PayoutReleased || !cashMoved || storedTransfer != transferRef {
		t.Fatalf("rejected cash payout=%s/%s cash=%v transfer=%q, want released/released/true/%q",
			payoutStatus, operationStatus, cashMoved, storedTransfer, transferRef)
	}
}

func dueContainsReversal(entries []DueReversal, id uuid.UUID) bool {
	for _, entry := range entries {
		if entry.ID == id {
			return true
		}
	}
	return false
}

func TestDisputeFilingOwnershipTerminalReasonAndWindowBoundaries(t *testing.T) {
	ctx, store, pool := openAdminMutationTestStore(t)
	f := seedDisputePayoutFixture(t, ctx, pool, "running")

	if _, err := store.RecordDispute(ctx, f.jobID, f.otherBuyerID, "wrong owner"); !errors.Is(err, errNotFound) {
		t.Fatalf("cross-buyer filing error = %v", err)
	}
	if _, err := store.RecordDispute(ctx, f.jobID, f.buyerID, "not done"); !errors.Is(err, errDisputeJobNotTerminal) {
		t.Fatalf("nonterminal filing error = %v", err)
	}
	if _, err := pool.Exec(ctx, `UPDATE jobs SET status='complete' WHERE id=$1`, f.jobID); err != nil {
		t.Fatal(err)
	}
	if _, err := store.RecordDispute(ctx, f.jobID, f.buyerID, "   "); !errors.Is(err, errDisputeReasonRequired) {
		t.Fatalf("blank reason error = %v", err)
	}
	if _, err := store.RecordDispute(ctx, f.jobID, f.buyerID,
		string(make([]rune, maxDisputeReasonRunes+1))); !errors.Is(err, errDisputeReasonTooLong) {
		t.Fatalf("oversized reason error = %v", err)
	}
	if _, err := pool.Exec(ctx,
		`UPDATE jobs SET terminal_at=now()-interval '8 days' WHERE id=$1`, f.jobID); err != nil {
		t.Fatal(err)
	}
	if _, err := store.RecordDispute(ctx, f.jobID, f.buyerID, "filed too late"); !errors.Is(err, errDisputeWindowClosed) {
		t.Fatalf("expired filing error = %v", err)
	}
}

func TestDisputeAPIUsesAuthenticatedOwnerAndStrictBoundedReason(t *testing.T) {
	ctx, store, pool := openAdminMutationTestStore(t)
	f := seedDisputePayoutFixture(t, ctx, pool, "complete")
	server := &Server{store: store}

	for name, body := range map[string]string{
		"empty":     "",
		"unknown":   `{"reason":"review","extra":true}`,
		"duplicate": `{"reason":"one","reason":"two"}`,
		"blank":     `{"reason":"   "}`,
		"oversized": `{"reason":"` + strings.Repeat("x", maxDisputeReasonRunes+1) + `"}`,
	} {
		t.Run(name, func(t *testing.T) {
			req, rec := disputeRequest(f.jobID, f.buyerID, body)
			server.handleFileDispute(rec, req)
			if rec.Code != http.StatusBadRequest {
				t.Fatalf("status = %d, body=%s", rec.Code, rec.Body.String())
			}
		})
	}
	req, rec := disputeRequest(f.jobID, f.otherBuyerID, `{"reason":"cross-account attempt"}`)
	server.handleFileDispute(rec, req)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("cross-owner status = %d, body=%s", rec.Code, rec.Body.String())
	}
	req, rec = disputeRequest(f.jobID, f.buyerID, `{"reason":"durable buyer report"}`)
	server.handleFileDispute(rec, req)
	if rec.Code != http.StatusAccepted {
		t.Fatalf("valid filing status = %d, body=%s", rec.Code, rec.Body.String())
	}
	var frozen int
	if err := pool.QueryRow(ctx, `
		SELECT count(*) FROM dispute_payout_holds h
		JOIN disputes d ON d.id=h.dispute_id
		WHERE d.job_id=$1 AND d.buyer_id=$2 AND d.status='open'`,
		f.jobID, f.buyerID).Scan(&frozen); err != nil {
		t.Fatal(err)
	}
	if frozen != 1 {
		t.Fatalf("accepted API filing froze %d credits, want 1", frozen)
	}
}

func TestConcurrentDisputeFilingsCreateOnlyOneActiveCase(t *testing.T) {
	ctx, store, pool := openAdminMutationTestStore(t)
	f := seedDisputePayoutFixture(t, ctx, pool, "complete")

	const contenders = 8
	errs := make(chan error, contenders)
	var wg sync.WaitGroup
	for i := 0; i < contenders; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			_, err := store.RecordDispute(ctx, f.jobID, f.buyerID, fmt.Sprintf("concurrent reason %d", i))
			errs <- err
		}(i)
	}
	wg.Wait()
	close(errs)
	succeeded, conflicted := 0, 0
	for err := range errs {
		switch {
		case err == nil:
			succeeded++
		case errors.Is(err, errDisputeAlreadyActive):
			conflicted++
		default:
			t.Fatalf("unexpected concurrent filing error: %v", err)
		}
	}
	var active int
	if err := pool.QueryRow(ctx, `
		SELECT count(*) FROM disputes WHERE job_id=$1
		 AND status IN ('open','no_peer','reverifying','unresolvable')`, f.jobID).Scan(&active); err != nil {
		t.Fatal(err)
	}
	if succeeded != 1 || conflicted != contenders-1 || active != 1 {
		t.Fatalf("concurrent filings = success:%d conflict:%d active:%d", succeeded, conflicted, active)
	}
}

func TestDisputeFilingWinsQueuedPayoutClaimRace(t *testing.T) {
	ctx, store, pool := openAdminMutationTestStore(t)
	t.Setenv("MERC_CANARY_MODE", "false")
	t.Setenv("MERC_CANARY_DISABLE_DECISION_REF", "test:dispute-payout-claim-race")
	f := seedDisputePayoutFixture(t, ctx, pool, "complete")

	blocker, err := pool.BeginTx(ctx, pgx.TxOptions{})
	must(t, err)
	defer blocker.Rollback(ctx)
	if _, err := blocker.Exec(ctx, `SELECT id FROM jobs WHERE id=$1 FOR UPDATE`, f.jobID); err != nil {
		t.Fatal(err)
	}

	filed := make(chan error, 1)
	go func() {
		_, err := store.RecordDispute(ctx, f.jobID, f.buyerID, "race-safe freeze")
		filed <- err
	}()
	// Queue filing first on the canonical job lock, then queue the payout claim.
	time.Sleep(100 * time.Millisecond)
	claimed := make(chan struct {
		ok  bool
		err error
	}, 1)
	go func() {
		_, ok, err := store.ClaimPayout(ctx, f.entryID)
		claimed <- struct {
			ok  bool
			err error
		}{ok, err}
	}()
	time.Sleep(100 * time.Millisecond)
	must(t, blocker.Commit(ctx))
	mustf(t, <-filed, "filing side of race: %v")
	claim := <-claimed
	if claim.err != nil || claim.ok {
		t.Fatalf("claim side of race = claimed:%v err:%v", claim.ok, claim.err)
	}
	var status string
	if err := pool.QueryRow(ctx,
		`SELECT payout_status FROM ledger_entries WHERE id=$1`, f.entryID).Scan(&status); err != nil {
		t.Fatal(err)
	}
	if status != PayoutHeld {
		t.Fatalf("queued claim advanced disputed credit to %q", status)
	}
}
