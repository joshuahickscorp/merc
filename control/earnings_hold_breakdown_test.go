package main

import (
	"testing"
	"time"

	"github.com/google/uuid"
)

// TestEarningsReportsHeldByReasonAndManualGate covers defect 3: a supplier must
// see held amounts broken down by reason, and an honest statement when the
// canary manual payout gate is in force.
func TestEarningsReportsHeldByReasonAndManualGate(t *testing.T) {
	ctx, store, pool := openIsolatedTestStore(t)
	// Canary ON → manual gate. Use the same complete envelope other canary tests use.
	setValidCanaryEnv(t, uuid.New())

	// Future hold window credit (same supplier).
	hold := seedPayoutFixture(t, ctx, pool, payoutFixtureOpts{
		creditUSD: 2.00, releaseFuture: true,
	})
	// Dispute-frozen credit.
	disputed := seedPayoutFixture(t, ctx, pool, payoutFixtureOpts{
		creditUSD: 3.00, supplierID: hold.supplierID,
	})
	if _, err := store.RecordDispute(ctx, disputed.jobID, disputed.buyerID, "buyer rejects the output quality"); err != nil {
		t.Fatalf("file dispute: %v", err)
	}
	// Verification-held credit (provisional / non-pass).
	_ = seedPayoutFixture(t, ctx, pool, payoutFixtureOpts{
		creditUSD: 1.50, supplierID: hold.supplierID, verificationFail: true,
	})

	earnings, err := store.WorkerEarnings(ctx, hold.supplierID)
	if err != nil {
		t.Fatalf("WorkerEarnings: %v", err)
	}
	if !earnings.ManualPayoutGate {
		t.Fatal("manual_payout_gate must be true under canary")
	}
	if earnings.ManualPayoutGateNote == "" {
		t.Fatal("manual_payout_gate_note must state the gate plainly")
	}
	if earnings.HeldUSD < 6.4 {
		t.Fatalf("held_usd=%.4f want at least 6.5 (2+3+1.5)", earnings.HeldUSD)
	}

	byReason := map[string]EarningsHoldReason{}
	for _, hr := range earnings.HeldByReason {
		byReason[hr.Reason] = hr
	}
	if _, ok := byReason["dispute_freeze"]; !ok {
		t.Fatalf("missing dispute_freeze bucket: %+v", earnings.HeldByReason)
	}
	if byReason["dispute_freeze"].AmountUSD < 2.99 {
		t.Fatalf("dispute_freeze amount=%.4f", byReason["dispute_freeze"].AmountUSD)
	}
	if _, ok := byReason["verification"]; !ok {
		t.Fatalf("missing verification bucket: %+v", earnings.HeldByReason)
	}
	// Future release_at under canary → manual_gate (held, not operator-released).
	if _, ok := byReason["manual_gate"]; !ok {
		t.Fatalf("expected manual_gate for canary-held credit: %+v", earnings.HeldByReason)
	}

	// Turn canary off and re-check: hold_window for future release, no manual note.
	t.Setenv("MERC_CANARY_MODE", "false")
	t.Setenv("MERC_CANARY_DISABLE_DECISION_REF", "test:earnings-hold")
	// Force a fresh future-held credit without dispute/verification.
	plain := seedPayoutFixture(t, ctx, pool, payoutFixtureOpts{
		creditUSD: 4.00, releaseFuture: true,
	})
	// Push release further so hold_window is unambiguous.
	if _, err := pool.Exec(ctx, `
		UPDATE ledger_entries SET release_at=now()+interval '12 hours' WHERE id=$1`,
		plain.entryID); err != nil {
		t.Fatal(err)
	}
	e2, err := store.WorkerEarnings(ctx, plain.supplierID)
	if err != nil {
		t.Fatal(err)
	}
	if e2.ManualPayoutGate {
		t.Fatal("manual gate must be off outside canary")
	}
	if e2.ManualPayoutGateNote != "" {
		t.Fatalf("unexpected manual note: %q", e2.ManualPayoutGateNote)
	}
	foundHold := false
	for _, hr := range e2.HeldByReason {
		if hr.Reason == "hold_window" && hr.AmountUSD >= 3.99 {
			foundHold = true
			if hr.EarliestReleaseAt == nil {
				t.Fatal("hold_window must report earliest_release_at")
			}
			if *hr.EarliestReleaseAt < time.Now().Unix() {
				t.Fatalf("earliest_release_at is in the past: %d", *hr.EarliestReleaseAt)
			}
		}
	}
	if !foundHold {
		t.Fatalf("missing hold_window for future credit: %+v", e2.HeldByReason)
	}
	if e2.NextPayoutAt == nil {
		t.Fatal("next_payout_at should surface earliest eligibility")
	}
}
