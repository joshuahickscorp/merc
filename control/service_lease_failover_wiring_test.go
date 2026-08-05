package main

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/google/uuid"
)

// Production sweep must fail over a dead leased worker to a replacement that
// clears the frozen ceilings, keep the buyer in service, and meter both
// suppliers for their own intervals. Before FailoverPendingServiceLeases was
// wired into recoverServiceLeases this path required a test-only direct call.
func TestRecoverServiceLeasesSweepFailsoverToReplacement(t *testing.T) {
	installSettlementCurrencyForTest(t, "cad")
	ctx, store, pool := openIsolatedTestStore(t)
	buyerID := uuid.New()
	if _, err := pool.Exec(ctx, `INSERT INTO buyers (id,email) VALUES ($1,$2)`,
		buyerID, buyerID.String()+"@failover-wire.invalid"); err != nil {
		t.Fatal(err)
	}
	must(t, store.SeedPrepaidBalance(ctx, buyerID, 1_000_000, "failover-wire-"+buyerID.String()))
	profile := sortedVLLMProfiles()[0]
	primary, _ := newFabricMeasurementWorker(t, ctx, store)
	seedMeasuredWarmResidency(t, ctx, pool, primary.WorkerID, profile.ModelAlias)
	region := "ca-fo-" + uuid.NewString()
	primaryOffer := serviceLeaseOffer(profile)
	primaryOffer.Region = region
	must(t, store.UpsertServiceLeaseOffer(ctx, primary, primaryOffer))
	lease, err := store.CreateServiceLease(ctx, buyerID, ServiceLeaseRequest{
		RuntimeProfileID: profile.RuntimeProfileID, Region: region,
		MinimumReplicas: 1, MaximumReplicas: 1, TermSeconds: 120, MaximumP95LatencyMilliseconds: 500,
		BuyerDeclaredCeilingNanos: 135_000_000,
	})
	must(t, err)
	// Replacement is live under the same frozen ceilings before the worker dies.
	fallback, _ := newFabricMeasurementWorker(t, ctx, store)
	seedMeasuredWarmResidency(t, ctx, pool, fallback.WorkerID, profile.ModelAlias)
	fallbackOffer := serviceLeaseOffer(profile)
	fallbackOffer.Region = region
	must(t, store.UpsertServiceLeaseOffer(ctx, fallback, fallbackOffer))
	// last_metered behind last_heartbeat, heartbeat past the 45s timeout:
	// RecoverServiceLeases meters the primary's authenticated interval, then
	// FailoverPendingServiceLeases moves the lease to the replacement.
	if _, err := pool.Exec(ctx, `UPDATE service_leases
		SET started_at=now()-interval '90 seconds',
		    last_metered_at=now()-interval '90 seconds',
		    last_worker_heartbeat_at=now()-interval '50 seconds'
		WHERE id=$1`, lease.ID); err != nil {
		t.Fatal(err)
	}

	// The production sweep path: recover marks FAILOVER_REQUIRED, then failover
	// or terminate. This is what Workers.recoverServiceLeases runs — not a
	// direct FailoverServiceLease call.
	wk := &Workers{store: store}
	must(t, wk.recoverServiceLeases(ctx))
	receipt, err := store.GetServiceLeaseReceipt(ctx, buyerID, lease.ID)
	must(t, err)
	if receipt.Lease.State != "ACTIVE" {
		t.Fatalf("sweep did not restore ACTIVE service: state=%s", receipt.Lease.State)
	}
	if receipt.Lease.WorkerID != fallback.WorkerID {
		t.Fatalf("sweep did not move to replacement: worker=%s want=%s",
			receipt.Lease.WorkerID, fallback.WorkerID)
	}
	var events []string
	rows, err := pool.Query(ctx, `SELECT kind FROM service_lease_events WHERE lease_id=$1 ORDER BY created_at,id`, lease.ID)
	must(t, err)
	defer rows.Close()
	for rows.Next() {
		var kind string
		must(t, rows.Scan(&kind))
		events = append(events, kind)
	}
	has := func(want string) bool {
		for _, e := range events {
			if e == want {
				return true
			}
		}
		return false
	}
	if !has("WORKER_LOSS") || !has("FAILOVER_COMPLETED") {
		t.Fatalf("sweep path not recorded: events=%v", events)
	}
	// Serve a further interval on the replacement, then finalize so both
	// suppliers appear in settlement.
	if _, err := pool.Exec(ctx, `UPDATE service_leases
		SET last_metered_at=now()-interval '2 seconds',
		    last_worker_heartbeat_at=now()-interval '1 second',
		    expires_at=now()-interval '1 second'
		WHERE id=$1`, lease.ID); err != nil {
		t.Fatal(err)
	}
	if completed, err := store.FinalizeExpiredServiceLeases(ctx, 10); err != nil || completed != 1 {
		t.Fatalf("finalize completed=%d err=%v", completed, err)
	}
	done, err := store.GetServiceLeaseReceipt(ctx, buyerID, lease.ID)
	must(t, err)
	credited := map[uuid.UUID]bool{}
	for _, credit := range done.Settlement.SupplierCredits {
		if credit.CreditMicros > 0 {
			credited[credit.SupplierID] = true
		}
	}
	if !credited[primary.SupplierID] || !credited[fallback.SupplierID] {
		t.Fatalf("both suppliers must be metered for their own intervals: credits=%+v",
			done.Settlement.SupplierCredits)
	}
}

// When no replacement clears the frozen ceiling, the sweep must terminate the
// lease and release the prepaid reservation rather than holding it to expires_at.
func TestRecoverServiceLeasesSweepTerminatesWhenNoReplacement(t *testing.T) {
	installSettlementCurrencyForTest(t, "cad")
	ctx, store, pool := openIsolatedTestStore(t)
	buyerID := uuid.New()
	if _, err := pool.Exec(ctx, `INSERT INTO buyers (id,email) VALUES ($1,$2)`,
		buyerID, buyerID.String()+"@failover-none.invalid"); err != nil {
		t.Fatal(err)
	}
	const topup int64 = 1_000_000
	must(t, store.SeedPrepaidBalance(ctx, buyerID, topup, "failover-none-"+buyerID.String()))
	profile := sortedVLLMProfiles()[0]
	primary, _ := newFabricMeasurementWorker(t, ctx, store)
	seedMeasuredWarmResidency(t, ctx, pool, primary.WorkerID, profile.ModelAlias)
	region := "ca-none-" + uuid.NewString()
	offer := serviceLeaseOffer(profile)
	offer.Region = region
	must(t, store.UpsertServiceLeaseOffer(ctx, primary, offer))
	// Term is short enough to clear the frozen ceiling; expires_at is then
	// pushed forward so the bug under test (holding reserve until term end)
	// would still have days of residual reservation without termination.
	lease, err := store.CreateServiceLease(ctx, buyerID, ServiceLeaseRequest{
		RuntimeProfileID: profile.RuntimeProfileID, Region: region,
		MinimumReplicas: 1, MaximumReplicas: 1, TermSeconds: 120, MaximumP95LatencyMilliseconds: 500,
		BuyerDeclaredCeilingNanos: 135_000_000,
	})
	must(t, err)
	if _, err := pool.Exec(ctx, `UPDATE service_leases SET expires_at=now()+interval '6 days' WHERE id=$1`, lease.ID); err != nil {
		t.Fatal(err)
	}
	reservedBefore, err := store.BuyerPrepaidAvailableMicros(ctx, buyerID)
	must(t, err)
	// Accrue authenticated usage on the primary, then lose the worker with no
	// replacement. last_metered < last_heartbeat so recovery records real usage
	// before FAILOVER_REQUIRED; heartbeat is past the 45s timeout.
	if _, err := pool.Exec(ctx, `UPDATE service_leases
		SET started_at=now()-interval '90 seconds',
		    last_metered_at=now()-interval '90 seconds',
		    last_worker_heartbeat_at=now()-interval '50 seconds'
		WHERE id=$1`, lease.ID); err != nil {
		t.Fatal(err)
	}
	// Lease still has a long expires_at; without termination the reservation
	// would sit in FAILOVER_REQUIRED for the remainder of the term.
	wk := &Workers{store: store}
	must(t, wk.recoverServiceLeases(ctx))
	receipt, err := store.GetServiceLeaseReceipt(ctx, buyerID, lease.ID)
	must(t, err)
	if receipt.Lease.State != "CANCELLED" {
		t.Fatalf("no-replacement path must terminate, got state=%s (would hold reserve to expires_at=%s)",
			receipt.Lease.State, receipt.Lease.ExpiresAt.Format(time.RFC3339))
	}
	if receipt.Lease.FinalizedAt == nil {
		t.Fatal("terminated lease has no finalized_at")
	}
	var detail []byte
	if err := pool.QueryRow(ctx, `SELECT detail FROM service_lease_events
		WHERE lease_id=$1 AND kind='FAILOVER_TERMINATED' ORDER BY created_at DESC LIMIT 1`, lease.ID).Scan(&detail); err != nil {
		t.Fatalf("FAILOVER_TERMINATED event missing: %v", err)
	}
	var payload map[string]any
	must(t, json.Unmarshal(detail, &payload))
	if payload["path"] != "no_replacement_under_frozen_ceiling" {
		t.Fatalf("termination path not recorded: %+v", payload)
	}
	available, err := store.BuyerPrepaidAvailableMicros(ctx, buyerID)
	must(t, err)
	// Reservation released: available should be topup minus any settled usage,
	// not topup minus the full reserved ceiling.
	if available <= reservedBefore {
		// reservedBefore was after create (ceiling held). After terminate, unused
		// reserve returns so available must rise unless the entire ceiling was spent.
		t.Fatalf("prepaid reservation was not released: available=%d after create available=%d",
			available, reservedBefore)
	}
	if available > topup {
		t.Fatalf("available=%d exceeds topup=%d", available, topup)
	}
	// FAILOVER_REQUIRED must no longer count in the open-reservation sum.
	var openCount int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM service_leases
		WHERE id=$1 AND state IN ('ACTIVE','UPGRADING','FAILOVER_REQUIRED')`, lease.ID).Scan(&openCount); err != nil {
		t.Fatal(err)
	}
	if openCount != 0 {
		t.Fatalf("terminated lease still counts as open reservation")
	}
}
