package main

import (
	"strings"
	"testing"

	"github.com/google/uuid"
)

// A retried meter event must not charge the buyer twice.
//
// Step 28's required proof names "duplicate/retry meter events, idempotent
// settlement, no lost liability/double charge" explicitly, and a worker
// heartbeat is the least reliable message in the system: it crosses a network
// from a machine the platform does not control, and any client that retries on
// a timeout will send the same observation again. The response may have been
// lost after the charge landed.
//
// What "not twice" means here needs stating precisely, because the first
// version of this test asserted the wrong thing and failed against correct
// code. A lease meter is not an event ledger — it is a clock. Each heartbeat
// charges for the wall time since last_metered_at, so a retry arriving a
// moment later legitimately adds a moment's charge. That is continuous
// metering of a lease that really was held, not a double charge.
//
// The defect this guards is re-metering an INTERVAL. Age the lease by five
// seconds, heartbeat (charging that interval), then heartbeat again
// immediately without moving the clock back. Correct behaviour adds only the
// microseconds that actually elapsed between the two calls. A delta-based
// meter — one that charged for the observation rather than for the time —
// would add the five seconds again.
//
// So the assertion is that the replay's addition is a small fraction of the
// metered interval, not that it is zero. Demanding zero would be demanding the
// clock stop, and it is what made the first attempt fail at +2,275 nanos on a
// lease that was genuinely still running.
//
// It was untested at the store either way: TestServiceLeaseCumulativeMetering
// DoesNotChangeEconomicsAtHeartbeatBoundaries exercises the pure money helper,
// not HeartbeatServiceLease against a real lease row.
func TestReplayedServiceLeaseHeartbeatDoesNotChargeTheBuyerTwice(t *testing.T) {
	installSettlementCurrencyForTest(t, "usd")
	ctx, store, pool := openPayoutTestStore(t)
	profile := sortedVLLMProfiles()[0]

	worker, _ := newFabricMeasurementWorker(t, ctx, store)
	seedMeasuredWarmResidency(t, ctx, pool, worker.WorkerID, profile.ModelAlias)
	offer := serviceLeaseOffer(profile)
	offer.Region = "ca-meter-" + uuid.NewString()
	offer.MaximumWarmReplicas, offer.AvailableWarmReplicas = 1, 1
	must(t, store.UpsertServiceLeaseOffer(ctx, worker, offer))

	buyerID := uuid.New()
	if _, err := pool.Exec(ctx,
		`INSERT INTO buyers (id,email,free_credit_usd) VALUES ($1,$2,0)`,
		buyerID, buyerID.String()+"@lease-meter.invalid"); err != nil {
		t.Fatal(err)
	}
	must(t, store.SeedPrepaidBalance(ctx, buyerID, 1_000_000, "service-meter-"+buyerID.String()))

	lease, err := store.CreateServiceLease(ctx, buyerID, ServiceLeaseRequest{
		RuntimeProfileID: profile.RuntimeProfileID, Region: offer.Region, Currency: "usd",
		MinimumReplicas: 1, MaximumReplicas: 1, TermSeconds: 60,
		MaximumP95LatencyMilliseconds: 500, BuyerDeclaredCeilingNanos: 135_000_000,
	})
	must(t, err)

	charge := func(label string) (int64, int64) {
		t.Helper()
		var buyerNanos, replicaNanos int64
		if err := pool.QueryRow(ctx,
			`SELECT buyer_charge_nanos, cumulative_replica_nanoseconds
			   FROM service_leases WHERE id=$1`, lease.ID,
		).Scan(&buyerNanos, &replicaNanos); err != nil {
			t.Fatalf("%s: read lease money: %v", label, err)
		}
		return buyerNanos, replicaNanos
	}

	beat := ServiceLeaseHeartbeat{
		WarmReplicas: 1, P95LatencyMillis: 200,
		LatencyMeasurementCount: 5, LatencyWindowSeconds: 15,
		LatencyMeasurementKind: "DATA_PLANE_COMPLETIONS_V1",
		Status:                 "READY",
		// A READY heartbeat must cite the bounded data-plane probe receipt;
		// without it the store refuses before any metering happens.
		DataPlaneProbeReceiptSHA256: strings.Repeat("a", 64),
	}

	// Age the lease so the first heartbeat meters a real interval; without this
	// the charge could be zero and the comparison would prove nothing.
	if _, err := pool.Exec(ctx,
		`UPDATE service_leases SET last_metered_at=now()-interval '5 seconds' WHERE id=$1`,
		lease.ID); err != nil {
		t.Fatal(err)
	}
	if err := store.HeartbeatServiceLease(ctx, worker, lease.ID, beat); err != nil {
		t.Fatalf("first heartbeat: %v", err)
	}
	firstCharge, firstReplica := charge("after first heartbeat")
	if firstCharge <= 0 || firstReplica <= 0 {
		t.Fatalf("the first heartbeat metered nothing (charge=%d replica_nanos=%d), so a "+
			"doubled charge would be indistinguishable from a correct one",
			firstCharge, firstReplica)
	}

	// The retry: same worker, same lease, same observation, no time advanced.
	if err := store.HeartbeatServiceLease(ctx, worker, lease.ID, beat); err != nil {
		t.Fatalf("replayed heartbeat: %v", err)
	}
	secondCharge, secondReplica := charge("after replayed heartbeat")

	// The replay may add only the time that really passed between the two
	// calls — microseconds — never the five-second interval again. Half the
	// first interval is a generous ceiling: a delta-based meter would add
	// approximately all of it.
	replayAdded := secondCharge - firstCharge
	if replayAdded < 0 {
		t.Fatalf("a replayed meter event REDUCED the buyer charge %d -> %d nanos; "+
			"metered liability must never go backwards", firstCharge, secondCharge)
	}
	if replayAdded*2 >= firstCharge {
		t.Fatalf("a replayed meter event re-charged the metered interval: %d -> %d nanos "+
			"(+%d against a first interval of %d). The meter is charging for the "+
			"observation rather than for elapsed time, so every network retry is a charge",
			firstCharge, secondCharge, replayAdded, firstCharge)
	}
	if replayReplica := secondReplica - firstReplica; replayReplica*2 >= firstReplica {
		t.Fatalf("a replayed meter event re-accrued the metered replica interval: "+
			"%d -> %d nanos (+%d against %d); liability is being manufactured from a retry",
			firstReplica, secondReplica, replayReplica, firstReplica)
	}
	t.Logf("first interval charged %d nanos; the replay added %d nanos of genuinely "+
		"elapsed time (%.4f%% of the interval)", firstCharge, replayAdded,
		100*float64(replayAdded)/float64(firstCharge))
}
