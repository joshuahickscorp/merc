package main

import (
	"testing"

	"github.com/google/uuid"
)

// WorkerPayoutLedger is the line-item half of the supplier earnings surface.
// Earnings aggregates without a trail leave a stranger unable to reconcile
// "what job paid me". This proves the trail is supplier-scoped, ordered newest
// first, and includes clawbacks alongside credits.
func TestWorkerPayoutLedgerListsCreditsAndClawbacksNewestFirst(t *testing.T) {
	ctx, store, pool := openPayoutTestStore(t)

	first := seedPayoutFixture(t, ctx, pool, payoutFixtureOpts{creditUSD: 0.001234})
	// Second credit on the same supplier, later insert order.
	siblingID, siblingTask, _ := seedSiblingCredit(t, ctx, pool, first, 0.002000)
	_ = siblingID

	// Clawback the first credit (newest of the three once inserted after).
	clawbackID := uuid.New()
	if _, err := pool.Exec(ctx, `
		INSERT INTO ledger_entries
		  (id,kind,supplier_id,task_id,amount_usd,currency,payout_status)
		VALUES ($1,'clawback',$2,$3,($4::numeric / 1000000),$5,'clawed_back')`,
		clawbackID, first.supplierID, first.taskID, usdToMicros(-0.001234), first.currency); err != nil {
		t.Fatalf("seed clawback: %v", err)
	}

	// Foreign supplier credit must not appear.
	foreign := seedPayoutFixture(t, ctx, pool, payoutFixtureOpts{creditUSD: 9.99})
	if foreign.supplierID == first.supplierID {
		t.Fatal("foreign fixture reused supplier")
	}

	ledger, err := store.WorkerPayoutLedger(ctx, first.supplierID, 50)
	if err != nil {
		t.Fatalf("WorkerPayoutLedger: %v", err)
	}
	if ledger.Currency == "" {
		t.Error("ledger carries no currency")
	}
	if len(ledger.Entries) < 3 {
		t.Fatalf("got %d entries, want at least 3 (2 credits + clawback)", len(ledger.Entries))
	}
	// Newest first: clawback should lead.
	if ledger.Entries[0].Kind != "clawback" {
		t.Errorf("newest row kind=%s, want clawback", ledger.Entries[0].Kind)
	}
	if ledger.Entries[0].AmountUSD >= 0 {
		t.Errorf("clawback amount %.9f is not negative", ledger.Entries[0].AmountUSD)
	}
	// Foreign supplier must not leak in.
	for _, e := range ledger.Entries {
		if e.ID == foreign.entryID {
			t.Error("foreign supplier credit appeared in this supplier's ledger")
		}
		if e.Currency == "" {
			t.Errorf("entry %s has empty currency", e.ID)
		}
	}
	// Job projection from task join.
	foundJob, foundSiblingTask := false, false
	for _, e := range ledger.Entries {
		if e.JobID != nil && *e.JobID == first.jobID {
			foundJob = true
		}
		if e.TaskID != nil && *e.TaskID == siblingTask {
			foundSiblingTask = true
		}
	}
	if !foundJob {
		t.Error("no ledger row projected the job_id from its task")
	}
	if !foundSiblingTask {
		t.Error("sibling credit task missing from ledger")
	}

	short, err := store.WorkerPayoutLedger(ctx, first.supplierID, 1)
	if err != nil {
		t.Fatal(err)
	}
	if len(short.Entries) != 1 {
		t.Errorf("limit=1 returned %d rows", len(short.Entries))
	}
}
