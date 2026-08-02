package main

import (
	"testing"

	"github.com/google/uuid"
)

// TestServiceLeaseSupplierPayoutFundingUsesCollectedTopup proves the missing
// terminal bridge: a service lease has no job/task, but its supplier liability
// is still funded from the buyer's actual CAD top-up and names the exact lease
// in the immutable funding fact. It deliberately stops before an external
// payout provider call; ClaimPayout is the authority boundary that creates the
// provider operation and remains fail-closed when the payout rail is absent.
func TestServiceLeaseSupplierPayoutFundingUsesCollectedTopup(t *testing.T) {
	installSettlementCurrencyForTest(t, "cad")
	t.Setenv("MERC_CANARY_MODE", "false")
	t.Setenv("MERC_CANARY_DISABLE_DECISION_REF", "TEST-service-lease-payout-funding")
	// Finalization is a platform-wide sweep. Under -race the shared suite can
	// legitimately leave another lease expired while this fixture is waiting;
	// isolate the database so the returned count names this test's one lease,
	// not an unrelated sibling's terminal transition.
	ctx, store, pool := openIsolatedTestStore(t)

	buyerID := uuid.New()
	if _, err := pool.Exec(ctx, `INSERT INTO buyers (id,email) VALUES ($1,$2)`, buyerID, buyerID.String()+"@service-payout.invalid"); err != nil {
		t.Fatal(err)
	}
	topupKey := "service-lease-payout-topup-" + uuid.NewString()
	if _, err := store.BeginPrepaidTopup(ctx, topupKey, buyerID, 100); err != nil {
		t.Fatal(err)
	}
	paymentIntent := "pi_service_lease_topup_" + uuid.NewString()
	chargeID := "ch_service_lease_topup_" + uuid.NewString()
	if err := store.CreditPrepaidTopup(ctx, topupKey, buyerID, ChargeResult{
		PaymentIntentID: paymentIntent, ChargeID: chargeID,
		RequestedCents: 100, ReceivedCents: 100, Currency: "cad",
	}); err != nil {
		t.Fatal(err)
	}

	profile := sortedVLLMProfiles()[0]
	worker, _ := newFabricMeasurementWorker(t, ctx, store)
	offer := serviceLeaseOffer(profile)
	offer.Region = "ca-service-payout-" + uuid.NewString()
	if err := store.UpsertServiceLeaseOffer(ctx, worker, offer); err != nil {
		t.Fatal(err)
	}
	lease, err := store.CreateServiceLease(ctx, buyerID, ServiceLeaseRequest{
		RuntimeProfileID: profile.RuntimeProfileID, Region: offer.Region,
		MinimumReplicas: 1, MaximumReplicas: 1, TermSeconds: 60,
		MaximumP95LatencyMilliseconds: 500, BuyerDeclaredCeilingNanos: 135_000_000,
	})
	if err != nil {
		t.Fatal(err)
	}
	// Make a short but billable terminal interval. The supplier rate is fixed
	// point, so ~19 seconds is enough to cross one CAD cent after accrual while
	// remaining well below the frozen buyer ceiling.
	if _, err := pool.Exec(ctx, `UPDATE service_leases
		SET last_metered_at=now()-interval '20 seconds',expires_at=now()-interval '1 second'
		WHERE id=$1`, lease.ID); err != nil {
		t.Fatal(err)
	}
	if completed, err := store.FinalizeExpiredServiceLeases(ctx, 10); err != nil || completed != 1 {
		t.Fatalf("finalize service lease completed=%d err=%v", completed, err)
	}

	var entryID uuid.UUID
	var payoutRef string
	if err := pool.QueryRow(ctx, `SELECT id,payout_ref FROM ledger_entries
		WHERE kind='supplier_credit' AND payout_ref LIKE $1`, serviceLeaseLedgerRef(lease.ID, KindSupplierCredit)+":%").Scan(&entryID, &payoutRef); err != nil {
		t.Fatal(err)
	}
	if parsed, ok := serviceLeaseIDFromSupplierCreditRef(payoutRef); !ok || parsed != lease.ID {
		t.Fatalf("supplier credit reference did not bind lease: ref=%q parsed=%v ok=%v", payoutRef, parsed, ok)
	}
	if _, err := pool.Exec(ctx, `UPDATE ledger_entries SET release_at=now()-interval '1 minute' WHERE id=$1`, entryID); err != nil {
		t.Fatal(err)
	}
	claimed, sent, err := store.ClaimPayout(ctx, entryID)
	if err != nil {
		t.Fatal(err)
	}
	if !sent || claimed.RequestedCents <= 0 || claimed.Currency != "cad" {
		t.Fatalf("service lease payout was not funded from collected topup: sent=%v claim=%+v", sent, claimed)
	}

	var (
		fundingLease    *uuid.UUID
		fundingJob      *uuid.UUID
		fundingPI       string
		fundingCents    int64
		fundingCurrency string
	)
	if err := pool.QueryRow(ctx, `SELECT liability_service_lease_id,liability_job_id,
		collection_payment_intent,amount_cents,currency
		FROM supplier_payout_funding WHERE ledger_entry_id=$1`, entryID).
		Scan(&fundingLease, &fundingJob, &fundingPI, &fundingCents, &fundingCurrency); err != nil {
		t.Fatal(err)
	}
	if fundingLease == nil || *fundingLease != lease.ID || fundingJob != nil || fundingPI != paymentIntent ||
		fundingCents != claimed.RequestedCents || fundingCurrency != "cad" {
		t.Fatalf("funding fact lost service/topup identity: lease=%v job=%v pi=%q cents=%d currency=%q claim=%+v",
			fundingLease, fundingJob, fundingPI, fundingCents, fundingCurrency, claimed)
	}

	receipt, err := store.GetServiceLeaseReceipt(ctx, buyerID, lease.ID)
	if err != nil {
		t.Fatal(err)
	}
	if receipt.Settlement == nil || receipt.Settlement.FundingAuthorityState != "PREPAID_CASH_ALLOCATED_TO_SUPPLIER_LIABILITIES" ||
		receipt.SupplierSettlementState != "SUPPLIER_CREDIT_FUNDED_PAYOUT_SENDING" {
		t.Fatalf("service receipt did not expose allocated supplier funding: %+v", receipt)
	}

	// A replay cannot create a second allocation or provider operation.
	if _, sentAgain, err := store.ClaimPayout(ctx, entryID); err != nil || sentAgain {
		t.Fatalf("service payout replay sent=%v err=%v", sentAgain, err)
	}
	var fundingRows, operationRows int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM supplier_payout_funding WHERE ledger_entry_id=$1`, entryID).Scan(&fundingRows); err != nil {
		t.Fatal(err)
	}
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM supplier_payout_operations WHERE ledger_entry_id=$1`, entryID).Scan(&operationRows); err != nil {
		t.Fatal(err)
	}
	if fundingRows != 1 || operationRows != 1 {
		t.Fatalf("service payout replay duplicated immutable rows: funding=%d operations=%d", fundingRows, operationRows)
	}
}
