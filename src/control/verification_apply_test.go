package main

import (
	"testing"
	"time"

	"github.com/google/uuid"
)

func TestVerificationDecisionDigestBindsEffectsAndSettlementCanonically(t *testing.T) {
	taskID := uuid.MustParse("11111111-2222-3333-4444-555555555555")
	supplierID := uuid.MustParse("aaaaaaaa-bbbb-cccc-dddd-eeeeeeeeeeee")
	buyerID := uuid.MustParse("99999999-8888-7777-6666-555555555555")
	release := time.Date(2026, 7, 11, 12, 0, 0, 123, time.FixedZone("offset", -4*60*60))
	decision := VerificationDecision{Outcome: OutcomePass, Effects: []VerificationEffect{{
		ID:   verificationEffectID(taskID, 0, 0, VerificationEffectDockReputation),
		Kind: VerificationEffectDockReputation, SupplierID: supplierID,
		ReputationEvent: EventTaskSuccess,
	}}}
	entries := []LedgerEntry{
		{Kind: KindSupplierCredit, SupplierID: &supplierID, TaskID: &taskID, AmountUSD: 0.9, PayoutStatus: PayoutHeld, ReleaseAt: &release},
		{Kind: KindBuyerCharge, BuyerID: &buyerID, TaskID: &taskID, AmountUSD: -1, PayoutStatus: PayoutReleased},
	}

	want, err := verificationDecisionDigest(decision, entries)
	must(t, err)
	reordered := []LedgerEntry{entries[1], entries[0]}
	got, err := verificationDecisionDigest(decision, reordered)
	must(t, err)
	if got != want {
		t.Fatalf("ledger input order changed canonical digest: %s != %s", got, want)
	}

	changed := append([]LedgerEntry(nil), entries...)
	changed[0].AmountUSD = 0.8
	got, err = verificationDecisionDigest(decision, changed)
	must(t, err)
	if got == want {
		t.Fatal("changed settlement amount must change decision digest")
	}
	decision.Effects[0].ReputationEvent = EventMismatch
	got, err = verificationDecisionDigest(decision, entries)
	must(t, err)
	if got == want {
		t.Fatal("changed verification effect must change decision digest")
	}
}
