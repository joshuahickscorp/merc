package main

import (
	"errors"
	"fmt"
	"testing"

	"github.com/google/uuid"
)

func TestStripeCashOutcomeProvesOutOfOrderNonRegressionAndReplay(t *testing.T) {
	ctx, store, pool := openPayoutTestStore(t)
	suffix := uuid.NewString()
	disputeID := "dp_outcome_" + suffix
	chargeID := "ch_outcome_" + suffix
	paymentIntent := "pi_outcome_" + suffix

	makeEvent := func(eventID, eventType, status string, created int64) stripeCashEvent {
		t.Helper()
		object := []byte(fmt.Sprintf(
			`{"object":"dispute","id":%q,"charge":%q,"payment_intent":%q,"amount":500,"currency":"cad","status":%q}`,
			disputeID, chargeID, paymentIntent, status,
		))
		payload := []byte(fmt.Sprintf(
			`{"id":%q,"type":%q,"created":%d,"data":{"object":%s}}`,
			eventID, eventType, created, object,
		))
		event, err := parseStripeCashEvent(eventID, eventType, created, object, payload)
		mustf(t, err, "parse %s: %v", eventType)
		return event
	}

	closedID := "evt_closed_" + suffix
	createdID := "evt_created_" + suffix
	closed := makeEvent(closedID, stripeEventDisputeClosed, "lost", 1_700_000_002)
	created := makeEvent(createdID, stripeEventDisputeCreated, "needs_response", 1_700_000_001)

	first, err := store.ApplyPaymentEventTx(ctx, closed)
	must(t, err)
	if !first.CashEffectApplied || first.CurrentCashEffectRank != 30 || first.Duplicate {
		t.Fatalf("terminal-first outcome=%+v, want applied rank 30", first)
	}

	stale, err := store.ApplyPaymentEventTx(ctx, created)
	must(t, err)
	if stale.CashEffectApplied || stale.CurrentCashEffectRank != 30 || stale.Duplicate {
		t.Fatalf("older opening outcome=%+v, want stale ignored behind rank 30", stale)
	}

	replay, err := store.ApplyPaymentEventTx(ctx, closed)
	must(t, err)
	if !replay.Duplicate || replay.CashEffectApplied {
		t.Fatalf("terminal replay outcome=%+v, want duplicate", replay)
	}

	var status, lastEventID string
	var rank int
	var lastCreated int64
	if err := pool.QueryRow(ctx, `
		SELECT status,cash_effect_rank,last_event_id,last_event_created
		  FROM stripe_dispute_cash_state WHERE dispute_id=$1`, disputeID,
	).Scan(&status, &rank, &lastEventID, &lastCreated); err != nil {
		t.Fatal(err)
	}
	if status != "lost" || rank != 30 || lastEventID != closedID || lastCreated != closed.EventCreated {
		t.Fatalf("durable state status/rank/event/created=%s/%d/%s/%d, want lost/30/%s/%d",
			status, rank, lastEventID, lastCreated, closedID, closed.EventCreated)
	}
}

func TestStripeCashImpairmentDefinitePayoutFailureRearmsWithoutCash(t *testing.T) {
	ctx, store, pool := openPayoutTestStore(t)
	t.Setenv("MERC_CANARY_MODE", "false")
	t.Setenv("MERC_CANARY_DISABLE_DECISION_REF", "TEST-stripe-cash-rearm")
	f := seedPayoutFixture(t, ctx, pool, payoutFixtureOpts{creditUSD: 1.00})
	if _, claimed, err := store.ClaimPayout(ctx, f.entryID); err != nil || !claimed {
		t.Fatalf("claim payout: claimed=%v err=%v", claimed, err)
	}

	object := []byte(fmt.Sprintf(
		`{"object":"dispute","id":"dp_rearm_%s","charge":%q,"payment_intent":%q,"amount":%d,"currency":%q,"status":"needs_response"}`,
		f.entryID, f.chargeID, f.paymentIntent, f.collectionCents, f.currency,
	))
	event, err := parseStripeCashEvent(
		"evt_rearm_"+f.entryID.String(), stripeEventDisputeCreated, 1_700_002_000,
		object, []byte(fmt.Sprintf(`{"event":"rearm","id":%q}`, f.entryID.String())),
	)
	mustf(t, err, "parse impairment event: %v")
	result, err := store.ApplyPaymentEventTx(ctx, event)
	mustf(t, err, "apply impairment event: %v")
	if result.CompromisedFundingRows != 1 || result.ReversalRequiredRows != 1 {
		t.Fatalf("impairment result=%+v, want one compromised/reversal row", result)
	}

	state, err := store.DeferPayout(ctx, f.entryID, errors.New("stripe rejected payout before transfer"))
	mustf(t, err, "defer definite payout failure: %v")
	if state != PayoutReady {
		t.Fatalf("deferred impaired payout state=%q, want %q", state, PayoutReady)
	}

	var ledgerStatus, operationStatus string
	var cashMoved, outcomeUnknown bool
	mustf(t, pool.QueryRow(ctx, `
		SELECT le.payout_status,op.status,op.cash_moved,op.outcome_unknown
		  FROM ledger_entries le JOIN supplier_payout_operations op ON op.ledger_entry_id=le.id
		 WHERE le.id=$1`, f.entryID).Scan(&ledgerStatus, &operationStatus, &cashMoved, &outcomeUnknown),
		"inspect rearmed payout: %v")
	if ledgerStatus != PayoutReady || operationStatus != PayoutReady || cashMoved || outcomeUnknown {
		t.Fatalf("rearmed payout=%s/%s cash=%v unknown=%v, want ready/ready/false/false",
			ledgerStatus, operationStatus, cashMoved, outcomeUnknown)
	}
	if n, err := store.CountReversalRequired(ctx); err != nil || n != 0 {
		t.Fatalf("no-cash impairment left reversal pause count=%d err=%v", n, err)
	}
	due, err := store.DuePayouts(ctx, 10)
	mustf(t, err, "due after rearm: %v")
	if !dueContains(due, f.entryID) {
		t.Fatal("rearmed definite-failure payout disappeared from retry queue")
	}

	// The impaired funding source remains unusable, so the retry must not send;
	// it becomes an ordinary awaiting_funding hold rather than a global reversal.
	if _, claimed, err := store.ClaimPayout(ctx, f.entryID); err != nil || claimed {
		t.Fatalf("claim with impaired funding: claimed=%v err=%v", claimed, err)
	}
	mustf(t, pool.QueryRow(ctx, `SELECT payout_status FROM ledger_entries WHERE id=$1`, f.entryID).
		Scan(&ledgerStatus), "inspect impaired retry status: %v")
	if ledgerStatus != PayoutAwaitingFunding {
		t.Fatalf("impaired retry status=%q, want %q", ledgerStatus, PayoutAwaitingFunding)
	}
}

func TestStripeCashImpairmentRepairsLedgerForExistingReversalOperation(t *testing.T) {
	ctx, store, pool := openPayoutTestStore(t)
	t.Setenv("MERC_CANARY_MODE", "false")
	t.Setenv("MERC_CANARY_DISABLE_DECISION_REF", "TEST-stripe-cash-existing-reversal")
	f := seedPayoutFixture(t, ctx, pool, payoutFixtureOpts{creditUSD: 1.00})
	if _, claimed, err := store.ClaimPayout(ctx, f.entryID); err != nil || !claimed {
		t.Fatalf("claim payout: claimed=%v err=%v", claimed, err)
	}
	// Simulate an earlier impairment transition whose ledger update was
	// interrupted after the append-only payout operation changed state. The
	// next event must repair the ledger even though this operation no longer
	// matches the fresh-transition predicate.
	if _, err := pool.Exec(ctx, `
		UPDATE supplier_payout_operations
		   SET status='reversal_required'
		 WHERE ledger_entry_id=$1`, f.entryID); err != nil {
		t.Fatalf("seed existing reversal operation: %v", err)
	}

	object := []byte(fmt.Sprintf(
		`{"object":"dispute","id":"dp_existing_%s","charge":%q,"payment_intent":%q,"amount":%d,"currency":%q,"status":"needs_response"}`,
		f.entryID, f.chargeID, f.paymentIntent, f.collectionCents, f.currency,
	))
	event, err := parseStripeCashEvent(
		"evt_existing_"+f.entryID.String(), stripeEventDisputeCreated, 1_700_003_000,
		object, []byte(fmt.Sprintf(`{"event":"existing-reversal","id":%q}`, f.entryID.String())),
	)
	mustf(t, err, "parse impairment event: %v")
	if _, err := store.ApplyPaymentEventTx(ctx, event); err != nil {
		t.Fatalf("apply impairment event: %v", err)
	}

	var ledgerStatus, operationStatus string
	mustf(t, pool.QueryRow(ctx, `
		SELECT le.payout_status,op.status
		  FROM ledger_entries le JOIN supplier_payout_operations op ON op.ledger_entry_id=le.id
		 WHERE le.id=$1`, f.entryID).Scan(&ledgerStatus, &operationStatus),
		"inspect repaired reversal operation: %v")
	if ledgerStatus != PayoutReversalRequired || operationStatus != PayoutReversalRequired {
		t.Fatalf("repaired payout=%s/%s, want reversal_required/reversal_required", ledgerStatus, operationStatus)
	}
}
