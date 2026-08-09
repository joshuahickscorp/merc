package main

import (
	"bytes"
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
)

type acceptedServiceLeaseFixture struct {
	ctx    context.Context
	store  *Store
	pool   *pgxpool.Pool
	buyer  uuid.UUID
	worker WorkerAuth
	offer  ServiceLeaseOfferRegistration
	lease  ServiceLease
}

func newAcceptedServiceLeaseFixture(t *testing.T) acceptedServiceLeaseFixture {
	t.Helper()
	installSettlementCurrencyForTest(t, "usd")
	ctx, store, pool := openIsolatedTestStore(t)
	buyer := uuid.New()
	if _, err := pool.Exec(ctx, `INSERT INTO buyers (id,email) VALUES ($1,$2)`,
		buyer, buyer.String()+"@lease-pricing-immutability.invalid"); err != nil {
		t.Fatal(err)
	}
	must(t, store.SeedPrepaidBalance(ctx, buyer, 1_000_000, "lease-pricing-immutability-"+buyer.String()))
	profile := sortedVLLMProfiles()[0]
	worker, _ := newFabricMeasurementWorker(t, ctx, store)
	seedMeasuredWarmResidency(t, ctx, pool, worker.WorkerID, profile.ModelAlias)
	offer := serviceLeaseOffer(profile)
	offer.Region = "ca-price-lock-" + uuid.NewString()
	must(t, store.UpsertServiceLeaseOffer(ctx, worker, offer))
	lease, err := store.CreateServiceLease(ctx, buyer, ServiceLeaseRequest{
		RuntimeProfileID: profile.RuntimeProfileID, Region: offer.Region, Currency: "usd",
		MinimumReplicas: 1, MaximumReplicas: 1, TermSeconds: 60,
		MaximumP95LatencyMilliseconds: 500, BuyerDeclaredCeilingNanos: 135_000_500,
	})
	must(t, err)
	if lease.PricingAcceptanceID == nil || *lease.PricingAcceptanceID != lease.ID ||
		lease.ReservedBuyerMicros != 135_001 ||
		lease.PricingAuthoritySource != serviceLeasePricingSourceAcceptance {
		t.Fatalf("current lease did not bind append-only pricing: %+v", lease)
	}
	return acceptedServiceLeaseFixture{ctx: ctx, store: store, pool: pool, buyer: buyer, worker: worker, offer: offer, lease: lease}
}

func TestServiceLeaseAcceptedPricingAndReferenceAreAppendOnly(t *testing.T) {
	f := newAcceptedServiceLeaseFixture(t)

	var originalRaw []byte
	var originalSHA string
	var originalMeteredAt time.Time
	if err := f.pool.QueryRow(f.ctx, `SELECT pricing_decision,pricing_decision_sha256,last_metered_at
		FROM service_leases WHERE id=$1`, f.lease.ID).Scan(&originalRaw, &originalSHA, &originalMeteredAt); err != nil {
		t.Fatal(err)
	}
	tampered := f.lease.Pricing
	tampered.ServiceLease.SupplierNanosPerReplicaHour++
	tamperedRaw, err := json.Marshal(tampered)
	must(t, err)
	tamperedSHA, err := pricingDecisionDigest(tampered)
	must(t, err)

	// Replacing JSON and its matching digest in the same statement used to
	// bypass the read-side digest check. The state write in this statement must
	// roll back too, proving refusal is atomic rather than a partial repair.
	if _, err := f.pool.Exec(f.ctx, `UPDATE service_leases
		SET pricing_decision=$2::jsonb,pricing_decision_sha256=$3,last_metered_at=last_metered_at+interval '1 second'
		WHERE id=$1`, f.lease.ID, tamperedRaw, tamperedSHA); err == nil || !strings.Contains(err.Error(), "immutable") {
		t.Fatalf("accepted inline pricing mutation error=%v, want immutable refusal", err)
	}
	assertServiceLeaseInlinePricingUnchanged(t, f, originalRaw, originalSHA, originalMeteredAt)

	// The canonical acceptance itself is append-only, even when the attacker
	// supplies a self-consistent replacement pair.
	if _, err := f.pool.Exec(f.ctx, `UPDATE service_lease_pricing_acceptances
		SET pricing_decision=$2::jsonb,pricing_decision_sha256=$3 WHERE id=$1`,
		f.lease.ID, tamperedRaw, tamperedSHA); err == nil || !strings.Contains(err.Error(), "append-only") {
		t.Fatalf("pricing acceptance rewrite error=%v, want append-only refusal", err)
	}
	if _, err := f.pool.Exec(f.ctx, `DELETE FROM service_lease_pricing_acceptances WHERE id=$1`, f.lease.ID); err == nil || !strings.Contains(err.Error(), "append-only") {
		t.Fatalf("pricing acceptance delete error=%v, want append-only refusal", err)
	}

	// A valid second acceptance cannot be swapped under an existing lease. The
	// unrelated state update proves reference replacement also fails atomically.
	alternate := uuid.New()
	if _, err := f.pool.Exec(f.ctx, `INSERT INTO service_lease_pricing_acceptances
		(id,pricing_decision,pricing_decision_sha256) VALUES ($1,$2::jsonb,$3)`,
		alternate, originalRaw, originalSHA); err != nil {
		t.Fatal(err)
	}
	if _, err := f.pool.Exec(f.ctx, `UPDATE service_leases
		SET pricing_acceptance_id=$2,last_metered_at=last_metered_at+interval '1 second' WHERE id=$1`,
		f.lease.ID, alternate); err == nil || !strings.Contains(err.Error(), "immutable") {
		t.Fatalf("pricing acceptance reference replacement error=%v, want immutable refusal", err)
	}
	assertServiceLeaseInlinePricingUnchanged(t, f, originalRaw, originalSHA, originalMeteredAt)
	var currentRef *uuid.UUID
	if err := f.pool.QueryRow(f.ctx, `SELECT pricing_acceptance_id FROM service_leases WHERE id=$1`, f.lease.ID).Scan(&currentRef); err != nil ||
		currentRef == nil || *currentRef != f.lease.ID {
		t.Fatalf("pricing reference changed after refused replacement: ref=%v err=%v", currentRef, err)
	}
}

func TestServiceLeaseAcceptedReservationCannotBeZeroedOrRepriced(t *testing.T) {
	f := newAcceptedServiceLeaseFixture(t)
	original := f.lease.ReservedBuyerMicros
	if original <= 0 {
		t.Fatalf("fixture has non-positive accepted reservation %d", original)
	}
	if _, err := f.pool.Exec(f.ctx, `UPDATE service_leases
		SET reserved_buyer_micros=0 WHERE id=$1`, f.lease.ID); err == nil ||
		(!strings.Contains(err.Error(), "immutable") && !strings.Contains(err.Error(), "reservation")) {
		t.Fatalf("zeroing accepted reservation error=%v, want authority refusal", err)
	}
	if _, err := f.pool.Exec(f.ctx, `UPDATE service_leases
		SET reserved_buyer_micros=reserved_buyer_micros+1 WHERE id=$1`, f.lease.ID); err == nil ||
		(!strings.Contains(err.Error(), "immutable") && !strings.Contains(err.Error(), "reservation")) {
		t.Fatalf("repricing accepted reservation error=%v, want authority refusal", err)
	}
	var stored int64
	if err := f.pool.QueryRow(f.ctx, `SELECT reserved_buyer_micros FROM service_leases WHERE id=$1`,
		f.lease.ID).Scan(&stored); err != nil || stored != original {
		t.Fatalf("refused reservation writes changed value: got=%d want=%d err=%v", stored, original, err)
	}
}

func assertServiceLeaseInlinePricingUnchanged(t *testing.T, f acceptedServiceLeaseFixture, wantRaw []byte, wantSHA string, wantMeteredAt time.Time) {
	t.Helper()
	var gotRaw []byte
	var gotSHA string
	var gotMeteredAt time.Time
	if err := f.pool.QueryRow(f.ctx, `SELECT pricing_decision,pricing_decision_sha256,last_metered_at
		FROM service_leases WHERE id=$1`, f.lease.ID).Scan(&gotRaw, &gotSHA, &gotMeteredAt); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(gotRaw, wantRaw) || gotSHA != wantSHA || !gotMeteredAt.Equal(wantMeteredAt) {
		t.Fatalf("refused authority write changed row: sha=%q/%q metered=%s/%s json_equal=%v",
			gotSHA, wantSHA, gotMeteredAt, wantMeteredAt, bytes.Equal(gotRaw, wantRaw))
	}
}

func TestServiceLeaseSettlementReplaysAcceptedPricingAndLegacyMigrationIsExplicit(t *testing.T) {
	f := newAcceptedServiceLeaseFixture(t)

	var acceptedRaw []byte
	var acceptedSHA string
	if err := f.pool.QueryRow(f.ctx, `SELECT pricing_decision,pricing_decision_sha256
		FROM service_lease_pricing_acceptances WHERE id=$1`, f.lease.ID).Scan(&acceptedRaw, &acceptedSHA); err != nil {
		t.Fatal(err)
	}
	accepted, err := decodeServiceLeasePricing(acceptedRaw, acceptedSHA)
	must(t, err)

	// The mutable offer and current policy inputs can move after acceptance.
	// Settlement must still use the acceptance row, not reinterpret live rates.
	repricedOffer := f.offer
	repricedOffer.SupplierNanosPerReplicaHour *= 7
	repricedOffer.ResidencyNanosPerReplicaHour *= 5
	must(t, f.store.UpsertServiceLeaseOffer(f.ctx, f.worker, repricedOffer))
	if _, err := f.pool.Exec(f.ctx, `UPDATE service_leases
		SET last_metered_at=now()-interval '20 seconds',expires_at=now()-interval '1 second'
		WHERE id=$1`, f.lease.ID); err != nil {
		t.Fatal(err)
	}
	if completed, err := f.store.FinalizeExpiredServiceLeases(f.ctx, 10); err != nil || completed != 1 {
		t.Fatalf("finalize accepted-price lease completed=%d err=%v", completed, err)
	}
	var replicaNanos, buyerNanos, supplierNanos, variableNanos, contributionNanos int64
	if err := f.pool.QueryRow(f.ctx, `SELECT cumulative_replica_nanoseconds,buyer_charge_nanos,
		supplier_payable_nanos,known_variable_cost_nanos,known_contribution_nanos
		FROM service_lease_meterings WHERE lease_id=$1 ORDER BY sequence DESC LIMIT 1`, f.lease.ID).
		Scan(&replicaNanos, &buyerNanos, &supplierNanos, &variableNanos, &contributionNanos); err != nil {
		t.Fatal(err)
	}
	expected, err := ServiceLeaseMoneyForReplicaDuration(MustParseCurrency(accepted.Currency), *accepted.ServiceLease, replicaNanos)
	must(t, err)
	expectedVariable, err := serviceLeaseKnownVariableNanos(*accepted.ServiceLease, expected)
	must(t, err)
	if buyerNanos != expected.BuyerCharge.Nanos || supplierNanos != expected.SupplierPayable.Nanos ||
		variableNanos != expectedVariable || contributionNanos != expected.MercContribution.Nanos {
		t.Fatalf("meter did not replay accepted authority: got buyer=%d supplier=%d variable=%d contribution=%d expected=%+v",
			buyerNanos, supplierNanos, variableNanos, contributionNanos, expected)
	}
	mutatedAuthority := *accepted.ServiceLease
	mutatedAuthority.SupplierNanosPerReplicaHour = repricedOffer.SupplierNanosPerReplicaHour
	mutatedAuthority.ResidencyNanosPerReplicaHour = repricedOffer.ResidencyNanosPerReplicaHour
	if repriced, err := ServiceLeaseMoneyForReplicaDuration(MustParseCurrency(accepted.Currency), mutatedAuthority, replicaNanos); err == nil &&
		repriced.BuyerCharge.Nanos == buyerNanos {
		t.Fatal("test repricing did not distinguish live offer from accepted settlement authority")
	}
	receipt, err := f.store.GetServiceLeaseReceipt(f.ctx, f.buyer, f.lease.ID)
	must(t, err)
	if receipt.Lease.PricingAuthoritySource != serviceLeasePricingSourceAcceptance ||
		receipt.Lease.PricingAcceptanceID == nil || receipt.Settlement == nil {
		t.Fatalf("terminal receipt lost accepted authority provenance or settlement: %+v", receipt)
	}
	buyerMicros, err := LedgerMicrosFromNanos(MoneyNanos{Currency: MustParseCurrency(accepted.Currency), Nanos: expected.BuyerCharge.Nanos})
	must(t, err)
	supplierMicros, err := LedgerMicrosFromNanos(MoneyNanos{Currency: MustParseCurrency(accepted.Currency), Nanos: expected.SupplierPayable.Nanos})
	must(t, err)
	baseSupplier, err := serviceLeaseComponent(MustParseCurrency(accepted.Currency),
		accepted.ServiceLease.SupplierNanosPerReplicaHour, replicaNanos)
	must(t, err)
	if expected.SupplierPayable.Nanos != baseSupplier.Nanos+expected.ResidencyCost.Nanos ||
		receipt.Settlement.BuyerChargeMicros != buyerMicros ||
		receipt.Settlement.SupplierCreditMicros != supplierMicros ||
		receipt.Settlement.PlatformGrossMicros != buyerMicros-supplierMicros {
		t.Fatalf("terminal ledger did not project accepted nanos: settlement=%+v want buyer=%d supplier=%d",
			receipt.Settlement, buyerMicros, supplierMicros)
	}

	// Simulate the only permitted NULL-reference population: a row that existed
	// before acceptance records were deployed. Reapplying the canonical schema
	// twice must not fabricate a reference; it freezes and labels the inline pair
	// for historical replay, and protects it from then on.
	if _, err := f.pool.Exec(f.ctx, `ALTER TABLE service_leases DISABLE TRIGGER service_leases_pricing_authority_immutable`); err != nil {
		t.Fatal(err)
	}
	if _, err := f.pool.Exec(f.ctx, `UPDATE service_leases SET pricing_acceptance_id=NULL WHERE id=$1`, f.lease.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := f.pool.Exec(f.ctx, `ALTER TABLE service_leases ENABLE TRIGGER service_leases_pricing_authority_immutable`); err != nil {
		t.Fatal(err)
	}
	must(t, f.store.Migrate(f.ctx))
	must(t, f.store.Migrate(f.ctx))
	legacyReceipt, err := f.store.GetServiceLeaseReceipt(f.ctx, f.buyer, f.lease.ID)
	must(t, err)
	if legacyReceipt.Lease.PricingAcceptanceID != nil ||
		legacyReceipt.Lease.PricingAuthoritySource != serviceLeasePricingSourceLegacy ||
		legacyReceipt.Lease.PricingDecisionSHA256 != acceptedSHA {
		t.Fatalf("historical lease migration invented or lost pricing authority: %+v", legacyReceipt.Lease)
	}
	if _, err := f.pool.Exec(f.ctx, `UPDATE service_leases
		SET pricing_decision_sha256=$2 WHERE id=$1`, f.lease.ID, strings.Repeat("f", 64)); err == nil || !strings.Contains(err.Error(), "immutable") {
		t.Fatalf("migration-frozen inline authority mutation error=%v, want immutable refusal", err)
	}
}
