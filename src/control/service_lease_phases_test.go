package main

import (
	"testing"
	"time"

	"github.com/google/uuid"
)

func TestLeasePhaseDefinitionMatchesStateMachine(t *testing.T) {
	// Documented mapping (service_lease_phases.go). A definition that invents
	// states production does not have, or ignores ones it does, is wrong.
	wantStates := []string{"ACTIVE", "UPGRADING", "FAILOVER_REQUIRED", "COMPLETED", "CANCELLED"}
	wantEvents := []string{
		"ACTIVATED", "METERED", "SLO_MEASURED",
		"ROLLING_UPDATE_STARTED", "ROLLING_UPDATE_COMPLETED",
		"WORKER_LOSS", "FAILOVER_COMPLETED", "FAILOVER_TERMINATED",
		"EXPIRED", "CANCELLED",
	}
	phases := []string{
		"reservation", "provisioning_to_ready", "steady_serving",
		"upgrade_drain", "failover", "termination",
	}
	if len(wantStates) != 5 {
		t.Fatalf("lease state set size = %d, want 5", len(wantStates))
	}
	if len(wantEvents) != 10 {
		t.Fatalf("lease event set size = %d, want 10", len(wantEvents))
	}
	if len(phases) != 6 {
		t.Fatalf("lease phase set size = %d, want 6", len(phases))
	}
}

func TestDecomposeLeasePhasesMeasuresClosedSpansAndRefusesOpenOnes(t *testing.T) {
	installSettlementCurrencyForTest(t, "usd")
	// openPayoutTestStore matches the established service-lease fixtures
	// (prepaid + fabric workers + offer book) used by service_leases_test.go.
	ctx, store, pool := openPayoutTestStore(t)
	profile := sortedVLLMProfiles()[0]
	buyerID := uuid.New()
	if _, err := pool.Exec(ctx, `INSERT INTO buyers (id,email) VALUES ($1,$2)`,
		buyerID, buyerID.String()+"@lease-phases.invalid"); err != nil {
		t.Fatal(err)
	}
	must(t, store.SeedPrepaidBalance(ctx, buyerID, 1_000_000, "lease-phases-"+buyerID.String()))
	worker, _ := newFabricMeasurementWorker(t, ctx, store)
	// Ordinary supply eligibility requires sandboxed=true AND an active
	// device-bound credential (containment work on main). CreateWorkerToken
	// alone is unbound; bind the same shape enrolment produces.
	if _, err := pool.Exec(ctx,
		`UPDATE workers SET sandboxed=true WHERE id=$1`, worker.WorkerID); err != nil {
		t.Fatal(err)
	}
	bindWorkerDeviceCredential(t, pool, ctx, worker.WorkerID)
	seedMeasuredWarmResidency(t, ctx, pool, worker.WorkerID, profile.ModelAlias)
	region := "ca-phases-" + uuid.NewString()
	offer := serviceLeaseOffer(profile)
	offer.Region = region
	must(t, store.UpsertServiceLeaseOffer(ctx, worker, offer))
	// TermSeconds=60 matches working service_leases fixtures under the same
	// ceiling (135e6 nanos). 600s exceeds that ceiling and surfaces as
	// errRealtimeNoSupply via pricing refusal, not a missing offer.
	lease, err := store.CreateServiceLease(ctx, buyerID, ServiceLeaseRequest{
		RuntimeProfileID: profile.RuntimeProfileID, Region: region, Currency: "usd",
		MinimumReplicas: 1, MaximumReplicas: 1, TermSeconds: 60, MaximumP95LatencyMilliseconds: 500,
		BuyerDeclaredCeilingNanos: 135_000_000,
	})
	must(t, err)

	base := time.Now().UTC().Add(-time.Hour)
	if _, err := pool.Exec(ctx, `
		UPDATE service_leases
		   SET created_at=$2, started_at=$2, last_metered_at=$2, last_worker_heartbeat_at=$2
		 WHERE id=$1`, lease.ID, base); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `DELETE FROM service_lease_events WHERE lease_id=$1`, lease.ID); err != nil {
		t.Fatal(err)
	}
	insertEv := func(kind string, at time.Time) {
		t.Helper()
		if _, err := pool.Exec(ctx, `
			INSERT INTO service_lease_events (lease_id,kind,detail,created_at)
			VALUES ($1,$2,'{}'::jsonb,$3)`, lease.ID, kind, at); err != nil {
			t.Fatalf("event %s: %v", kind, err)
		}
	}
	insertEv("ACTIVATED", base.Add(1*time.Second))
	insertEv("SLO_MEASURED", base.Add(6*time.Second))
	insertEv("ROLLING_UPDATE_STARTED", base.Add(30*time.Second))
	insertEv("ROLLING_UPDATE_COMPLETED", base.Add(40*time.Second))
	insertEv("SLO_MEASURED", base.Add(41*time.Second))
	insertEv("WORKER_LOSS", base.Add(50*time.Second))
	insertEv("FAILOVER_COMPLETED", base.Add(55*time.Second))
	insertEv("SLO_MEASURED", base.Add(56*time.Second))
	insertEv("EXPIRED", base.Add(100*time.Second))
	final := base.Add(100 * time.Second)
	if _, err := pool.Exec(ctx, `
		UPDATE service_leases SET state='COMPLETED', finalized_at=$2 WHERE id=$1`,
		lease.ID, final); err != nil {
		t.Fatal(err)
	}

	got, err := DecomposeLeasePhases(ctx, pool, lease.ID)
	must(t, err)

	assertPhaseMS := func(name string, p TaskPhase, want float64) {
		t.Helper()
		if !p.Known {
			t.Fatalf("%s unknown: %s", name, p.Why)
		}
		if p.DurationMS != want {
			t.Fatalf("%s = %.0fms, want %.0fms", name, p.DurationMS, want)
		}
	}
	assertPhaseMS("reservation", got.Reservation, 1000)
	assertPhaseMS("provisioning_to_ready", got.ProvisioningToReady, 5000)
	assertPhaseMS("upgrade_drain", got.UpgradeDrain, 10000)
	assertPhaseMS("failover", got.Failover, 5000)
	assertPhaseMS("steady_serving", got.SteadyServing, 77000)
	if !got.Termination.Known {
		t.Fatalf("termination unknown: %s", got.Termination.Why)
	}

	// Record phase calibrations (actual-only).
	if err := store.RecordLeasePhaseCalibrations(ctx, lease.ID); err != nil {
		t.Fatal(err)
	}
	var n int
	if err := pool.QueryRow(ctx, `
		SELECT COUNT(*) FROM eta_calibration
		 WHERE subject_kind='service_lease' AND subject_id=$1
		   AND predicted_ms IS NULL AND realized_ms IS NOT NULL`,
		lease.ID).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n < 4 {
		t.Fatalf("want at least reservation/provision/upgrade/failover/steady rows, got %d", n)
	}

	// Open upgrade must not mint a complete duration.
	// First lease held the only warm replica; re-offer so the second can clear.
	must(t, store.UpsertServiceLeaseOffer(ctx, worker, offer))
	lease2, err := store.CreateServiceLease(ctx, buyerID, ServiceLeaseRequest{
		RuntimeProfileID: profile.RuntimeProfileID, Region: region, Currency: "usd",
		MinimumReplicas: 1, MaximumReplicas: 1, TermSeconds: 60, MaximumP95LatencyMilliseconds: 500,
		BuyerDeclaredCeilingNanos: 135_000_000,
	})
	must(t, err)
	if _, err := pool.Exec(ctx, `DELETE FROM service_lease_events WHERE lease_id=$1`, lease2.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `
		INSERT INTO service_lease_events (lease_id,kind,detail,created_at)
		VALUES ($1,'ACTIVATED','{}'::jsonb,$2),
		       ($1,'SLO_MEASURED','{}'::jsonb,$3),
		       ($1,'ROLLING_UPDATE_STARTED','{}'::jsonb,$4)`,
		lease2.ID, base, base.Add(time.Second), base.Add(2*time.Second)); err != nil {
		t.Fatal(err)
	}
	open, err := DecomposeLeasePhases(ctx, pool, lease2.ID)
	must(t, err)
	if open.UpgradeDrain.Known {
		t.Fatalf("open upgrade must be unknown, got %+v", open.UpgradeDrain)
	}
	if open.UpgradeDrain.Why == "" {
		t.Fatal("open upgrade refusal must explain the missing close")
	}
	if open.Termination.Known {
		t.Fatal("live lease must not invent termination")
	}
}
