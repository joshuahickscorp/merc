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
	ctx, store, pool := openAdminMutationTestStore(t)
	t.Setenv("CX_CANARY_MODE", "false")
	f := seedDisputePayoutFixture(t, ctx, pool, "complete")

	due, err := store.DuePayouts(ctx, 100)
	if err != nil || !dueContains(due, f.entryID) {
		t.Fatalf("credit was not initially due: present=%v err=%v", dueContains(due, f.entryID), err)
	}
	disputeID, err := store.RecordDispute(ctx, f.jobID, f.buyerID, " output does not match the submitted input ")
	if err != nil {
		t.Fatalf("file dispute: %v", err)
	}
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

	if err := store.SetDisputeStatus(ctx, disputeID, "rejected"); err != nil {
		t.Fatalf("reject dispute: %v", err)
	}
	due, err = store.DuePayouts(ctx, 100)
	if err != nil || !dueContains(due, f.entryID) {
		t.Fatalf("rejected dispute did not re-enable held payout: present=%v err=%v", dueContains(due, f.entryID), err)
	}

	upheldID, err := store.RecordDispute(ctx, f.jobID, f.buyerID, "independent review still shows a bad result")
	if err != nil {
		t.Fatalf("file successor dispute: %v", err)
	}
	if err := store.SetDisputeStatus(ctx, upheldID, "upheld"); err != nil {
		t.Fatalf("uphold dispute: %v", err)
	}
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
	t.Setenv("CX_CANARY_MODE", "false")
	f := seedDisputePayoutFixture(t, ctx, pool, "complete")

	blocker, err := pool.BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		t.Fatal(err)
	}
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
	if err := blocker.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	if err := <-filed; err != nil {
		t.Fatalf("filing side of race: %v", err)
	}
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
