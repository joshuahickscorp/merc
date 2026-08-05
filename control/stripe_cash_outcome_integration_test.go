package main

import (
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
			`{"id":%q,"charge":%q,"payment_intent":%q,"amount":500,"currency":"cad","status":%q}`,
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
