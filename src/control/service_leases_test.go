package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
)

func serviceLeaseOffer(profile VLLMRuntimeProfile) ServiceLeaseOfferRegistration {
	return ServiceLeaseOfferRegistration{
		RuntimeProfileID: profile.RuntimeProfileID, RuntimeProfileSHA256: profile.ProfileSHA256,
		Region: "ca-central-1", Currency: "usd", MaximumWarmReplicas: 3, AvailableWarmReplicas: 3,
		SupplierNanosPerReplicaHour: 2_000_000_000, ResidencyNanosPerReplicaHour: 200_000_000,
		SupportsRollingUpgrade: true, P95LatencyMillis: 200, P99LatencyMillis: 250,
		LatencyMeasurementCount: 5, LatencyWindowSeconds: 15,
		LatencyMeasurementKind: "DATA_PLANE_COMPLETIONS_V1", Status: "READY",
	}
}

func TestServiceLeaseUnboundLegacyOfferCannotClearUSDOrder(t *testing.T) {
	installSettlementCurrencyForTest(t, "usd")
	ctx, store, pool := openPayoutTestStore(t)
	profile := sortedVLLMProfiles()[0]
	worker, _ := newFabricMeasurementWorker(t, ctx, store)
	seedMeasuredWarmResidency(t, ctx, pool, worker.WorkerID, profile.ModelAlias)
	offer := serviceLeaseOffer(profile)
	offer.Region = "ca-unbound-" + uuid.NewString()
	must(t, store.UpsertServiceLeaseOffer(ctx, worker, offer))
	// NULL is the migration quarantine for rows whose pre-currency offer cannot
	// prove whether its nanos were USD or a deployment's former settlement unit.
	// A quarantined offer cannot remain READY or be promoted back to READY until
	// the current agent re-registers an explicit USD rate.
	if _, err := pool.Exec(ctx, `UPDATE service_lease_worker_offers
		SET status='DRAINING',currency=NULL
		WHERE worker_id=$1 AND runtime_profile_id=$2 AND region=$3`,
		worker.WorkerID, profile.RuntimeProfileID, offer.Region); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `UPDATE service_lease_worker_offers SET status='READY'
		WHERE worker_id=$1 AND runtime_profile_id=$2 AND region=$3`,
		worker.WorkerID, profile.RuntimeProfileID, offer.Region); err == nil {
		t.Fatal("unbound legacy offer was promoted to READY without explicit USD currency")
	}
	buyerID := uuid.New()
	if _, err := pool.Exec(ctx, `INSERT INTO buyers (id,email) VALUES ($1,$2)`,
		buyerID, buyerID.String()+"@service-unbound.invalid"); err != nil {
		t.Fatal(err)
	}
	_, err := store.CreateServiceLease(ctx, buyerID, ServiceLeaseRequest{
		RuntimeProfileID: profile.RuntimeProfileID, Region: offer.Region, Currency: "usd",
		MinimumReplicas: 1, MaximumReplicas: 1, TermSeconds: 60,
		MaximumP95LatencyMilliseconds: 500, BuyerDeclaredCeilingNanos: 135_000_000,
	})
	if !errors.Is(err, errRealtimeNoSupply) {
		t.Fatalf("unbound legacy offer cleared a USD buyer order: %v", err)
	}
}

// seedMeasuredWarmResidency plants a fresh measured worker_model_state row so a
// service-lease offer is allowed to advertise warm capacity. Offers fail closed
// to zero available replicas without this measurement.
func seedMeasuredWarmResidency(t *testing.T, ctx context.Context, pool *pgxpool.Pool, workerID uuid.UUID, modelID string) {
	t.Helper()
	if modelID == "" {
		t.Fatal("seedMeasuredWarmResidency requires a model id")
	}
	if _, err := pool.Exec(ctx, `
		INSERT INTO worker_model_state
		  (worker_id, model_id, last_seen_warm, rss_delta_bytes, load_ms)
		VALUES ($1, $2, now(), $3, $4)
		ON CONFLICT (worker_id, model_id) DO UPDATE SET
		  last_seen_warm = now(),
		  rss_delta_bytes = EXCLUDED.rss_delta_bytes,
		  load_ms = EXCLUDED.load_ms`,
		workerID, modelID, int64(100*1024*1024), int64(1500)); err != nil {
		t.Fatalf("seed measured warm residency for %s: %v", modelID, err)
	}
	// Remove the row when the test ends. The package shares one database, and a
	// warm row is an ordering input to the batch claim predicate: a worker left
	// warm and online here makes another test's deferral check see a cheaper
	// eligible peer that its own fixture never created. That is exactly how
	// TestClaimTasksTxDefersToACheaperAskingWorker started failing in the full
	// suite while passing alone — and because last_seen_at ages out after sixty
	// seconds, it failed on run order rather than reliably.
	t.Cleanup(func() {
		cleanupCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_, _ = pool.Exec(cleanupCtx,
			`DELETE FROM worker_model_state WHERE worker_id=$1 AND model_id=$2`,
			workerID, modelID)
	})
}

func TestServiceLeaseMarketClearingReceiptBindsLiveOfferBook(t *testing.T) {
	installSettlementCurrencyForTest(t, "usd")
	ctx, store, pool := openPayoutTestStore(t)
	profile := sortedVLLMProfiles()[0]
	buyerID := uuid.New()
	if _, err := pool.Exec(ctx, `INSERT INTO buyers (id,email) VALUES ($1,$2)`, buyerID, buyerID.String()+"@service-clearing.invalid"); err != nil {
		t.Fatal(err)
	}
	must(t, store.SeedPrepaidBalance(ctx, buyerID, 1_000_000, "service-clearing-"+buyerID.String()))
	first, _ := newFabricMeasurementWorker(t, ctx, store)
	second, _ := newFabricMeasurementWorker(t, ctx, store)
	seedMeasuredWarmResidency(t, ctx, pool, first.WorkerID, profile.ModelAlias)
	seedMeasuredWarmResidency(t, ctx, pool, second.WorkerID, profile.ModelAlias)
	region := "ca-clearing-" + uuid.NewString()
	firstOffer := serviceLeaseOffer(profile)
	firstOffer.Region = region
	secondOffer := firstOffer
	secondOffer.SupplierNanosPerReplicaHour += 100_000_000
	must(t, store.UpsertServiceLeaseOffer(ctx, first, firstOffer))
	must(t, store.UpsertServiceLeaseOffer(ctx, second, secondOffer))
	request := ServiceLeaseRequest{RuntimeProfileID: profile.RuntimeProfileID, Region: region, Currency: "usd",
		MinimumReplicas: 1, MaximumReplicas: 1, TermSeconds: 60, MaximumP95LatencyMilliseconds: 500,
		BuyerDeclaredCeilingNanos: 135_000_000}
	lease, err := store.CreateServiceLease(ctx, buyerID, request)
	must(t, err)
	if lease.WorkerID != first.WorkerID || lease.SupplierID != first.SupplierID {
		t.Fatalf("clearing did not choose the lowest measured ask: lease=%+v first=%+v", lease, first)
	}
	receipt, err := store.GetServiceLeaseReceipt(ctx, buyerID, lease.ID)
	must(t, err)
	market := receipt.MarketClearing
	if market == nil || market.Version != serviceLeaseMarketClearingVersion || market.CandidateCount != 2 ||
		market.SelectedRank != 1 || market.SelectedWorkerID != first.WorkerID ||
		market.SelectedSupplierID != first.SupplierID ||
		market.SelectedSupplierRateNanos != firstOffer.SupplierNanosPerReplicaHour ||
		market.SelectedResidencyRateNanos != firstOffer.ResidencyNanosPerReplicaHour ||
		market.BuyerCeilingNanos != request.BuyerDeclaredCeilingNanos ||
		market.AcceptedCeilingNanos != lease.Pricing.FixedPoint.AcceptedCeilingNanos ||
		market.PricingDecisionSHA256 != lease.PricingDecisionSHA256 ||
		market.PositiveContributionNanos <= 0 || market.OrderBookPolicy == "" {
		t.Fatalf("market clearing receipt lost live offer/pricing authority: %+v lease=%+v", market, lease)
	}
}

func TestServiceLeaseOfferRefreshCannotRaceCapacityReservation(t *testing.T) {
	installSettlementCurrencyForTest(t, "usd")
	ctx, store, pool := openPayoutTestStore(t)
	profile := sortedVLLMProfiles()[0]
	worker, _ := newFabricMeasurementWorker(t, ctx, store)
	seedMeasuredWarmResidency(t, ctx, pool, worker.WorkerID, profile.ModelAlias)
	offer := serviceLeaseOffer(profile)
	offer.Region = "ca-race-" + uuid.NewString()
	offer.MaximumWarmReplicas, offer.AvailableWarmReplicas = 1, 1
	must(t, store.UpsertServiceLeaseOffer(ctx, worker, offer))
	buyers := []uuid.UUID{uuid.New(), uuid.New()}
	for _, buyerID := range buyers {
		if _, err := pool.Exec(ctx, `INSERT INTO buyers (id,email,free_credit_usd) VALUES ($1,$2,10)`, buyerID, buyerID.String()+"@lease-race.invalid"); err != nil {
			t.Fatal(err)
		}
		// A service may not use the legacy sandbox grant as cash authority.
		// Give each racing buyer actual prepaid liability; capacity, not an
		// accidental funding shortage, is what this test is measuring.
		must(t, store.SeedPrepaidBalance(ctx, buyerID, 1_000_000, "service-race-"+buyerID.String()))
	}
	request := ServiceLeaseRequest{RuntimeProfileID: profile.RuntimeProfileID, Region: offer.Region, Currency: "usd",
		MinimumReplicas: 1, MaximumReplicas: 1, TermSeconds: 60, MaximumP95LatencyMilliseconds: 500,
		BuyerDeclaredCeilingNanos: 135_000_000}

	start := make(chan struct{})
	errs := make(chan error, len(buyers)+1)
	var group sync.WaitGroup
	group.Add(1)
	go func() {
		defer group.Done()
		<-start
		for range 20 {
			if err := store.UpsertServiceLeaseOffer(ctx, worker, offer); err != nil {
				errs <- err
				return
			}
		}
	}()
	for _, buyerID := range buyers {
		group.Add(1)
		go func(buyerID uuid.UUID) {
			defer group.Done()
			<-start
			_, err := store.CreateServiceLease(ctx, buyerID, request)
			if err != nil && !errors.Is(err, errRealtimeNoSupply) {
				errs <- err
			}
		}(buyerID)
	}
	close(start)
	group.Wait()
	close(errs)
	for err := range errs {
		t.Fatalf("refresh/reservation race failed: %v", err)
	}

	var active, available int
	must(t, pool.QueryRow(ctx, `SELECT count(*) FROM service_leases WHERE worker_id=$1 AND state='ACTIVE'`, worker.WorkerID).Scan(&active))
	if err := pool.QueryRow(ctx, `SELECT available_warm_replicas FROM service_lease_worker_offers
		WHERE worker_id=$1 AND runtime_profile_id=$2 AND region=$3`, worker.WorkerID, profile.RuntimeProfileID, offer.Region).Scan(&available); err != nil {
		t.Fatal(err)
	}
	if active != 1 || available != 0 {
		t.Fatalf("one warm replica became active=%d available=%d after concurrent refreshes", active, available)
	}
}

func TestServiceLeaseRequiresCollectedPrepaidCashAndFreezesItsMaximum(t *testing.T) {
	installSettlementCurrencyForTest(t, "usd")
	ctx, store, pool := openPayoutTestStore(t)
	buyerID := uuid.New()
	// The legacy grant is intentionally generous: if this path ever starts
	// reading free_credit_usd again, the first admission below would succeed.
	if _, err := pool.Exec(ctx, `INSERT INTO buyers (id,email,free_credit_usd) VALUES ($1,$2,1000)`, buyerID, buyerID.String()+"@service-prepaid.invalid"); err != nil {
		t.Fatal(err)
	}
	profile := sortedVLLMProfiles()[0]
	worker, _ := newFabricMeasurementWorker(t, ctx, store)
	seedMeasuredWarmResidency(t, ctx, pool, worker.WorkerID, profile.ModelAlias)
	offer := serviceLeaseOffer(profile)
	offer.Region = "ca-prepaid-" + uuid.NewString()
	must(t, store.UpsertServiceLeaseOffer(ctx, worker, offer))
	// min==max is required at admission; money still freezes on the accepted
	// ceiling (replica count only shapes the priced maximum via maximum_replicas).
	request := ServiceLeaseRequest{RuntimeProfileID: profile.RuntimeProfileID, Region: offer.Region, Currency: "usd",
		MinimumReplicas: 3, MaximumReplicas: 3, TermSeconds: 60, MaximumP95LatencyMilliseconds: 500,
		BuyerDeclaredCeilingNanos: 135_000_000}
	pricing, err := newServiceLeasePricingDecision(serviceLeasePricingInputs(profile, MustParseCurrency("usd"), request,
		offer.SupplierNanosPerReplicaHour, offer.ResidencyNanosPerReplicaHour))
	must(t, err)
	reserve, err := LedgerMicrosFromNanos(MoneyNanos{Currency: MustParseCurrency("usd"), Nanos: pricing.FixedPoint.AcceptedCeilingNanos})
	if err != nil || reserve <= 1 {
		t.Fatalf("service reserve=%d err=%v", reserve, err)
	}
	if _, err := store.CreateServiceLease(ctx, buyerID, request); !errors.Is(err, errRealtimeInsufficientFunds) {
		t.Fatalf("lease used free credit rather than prepaid cash: %v", err)
	}
	must(t, store.SeedPrepaidBalance(ctx, buyerID, reserve-1, "service-underfunded-"+buyerID.String()))
	if _, err := store.CreateServiceLease(ctx, buyerID, request); !errors.Is(err, errRealtimeInsufficientFunds) {
		t.Fatalf("lease accepted one micro below its frozen maximum: %v", err)
	}
	must(t, store.SeedPrepaidBalance(ctx, buyerID, 1, "service-exact-"+buyerID.String()))
	lease, err := store.CreateServiceLease(ctx, buyerID, request)
	if err != nil || lease.ReservedBuyerMicros != reserve {
		t.Fatalf("exact prepaid service admission lease=%+v err=%v", lease, err)
	}
	available, err := store.BuyerPrepaidAvailableMicros(ctx, buyerID)
	if err != nil || available != 0 {
		t.Fatalf("active service left %d refundable/unreserved micros err=%v, want 0", available, err)
	}
}

func TestServiceLeaseUSDBuyerAndWorkerPathUsesFrozenPricingAndCumulativeMetering(t *testing.T) {
	installSettlementCurrencyForTest(t, "usd")
	// RecoverServiceLeases and FinalizeExpiredServiceLeases are platform-wide
	// sweeps. A shared suite database can contain another expired fixture under
	// -race, which would make the returned count unrelated to this test's one
	// lease. Keep the complete buyer/worker path isolated so its count is exact.
	ctx, store, pool := openIsolatedTestStore(t)
	buyerID := uuid.New()
	if _, err := pool.Exec(ctx, `INSERT INTO buyers (id,email,free_credit_usd) VALUES ($1,$2,10)`, buyerID, buyerID.String()+"@lease.invalid"); err != nil {
		t.Fatal(err)
	}
	must(t, store.SeedPrepaidBalance(ctx, buyerID, 1_000_000, "service-path-"+buyerID.String()))
	_, buyerKey, _, err := store.CreateAPIKey(ctx, buyerID, "lease-path", true)
	must(t, err)
	primaryWorker, workerToken := newFabricMeasurementWorker(t, ctx, store)
	profile := sortedVLLMProfiles()[0]
	seedMeasuredWarmResidency(t, ctx, pool, primaryWorker.WorkerID, profile.ModelAlias)
	offer := serviceLeaseOffer(profile)
	offer.Region = "ca-path-" + uuid.NewString()
	handler := NewServer(store, nil, nil, nil).Routes()

	post := func(path, token string, body any) *httptest.ResponseRecorder {
		raw, err := json.Marshal(body)
		must(t, err)
		req := httptest.NewRequest(http.MethodPost, path, bytes.NewReader(raw))
		if token == workerToken {
			req.Header.Set("X-Worker-Token", token)
		} else {
			req.Header.Set("Authorization", "Bearer "+token)
		}
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, req)
		return rec
	}
	if got := post("/v1/worker/service-leases/offers", workerToken, offer).Code; got != http.StatusOK {
		t.Fatalf("offer registration status=%d", got)
	}
	// Admission requires min==max; hold all three advertised warm replicas so
	// overbooking and under-max offer refresh stay covered.
	request := ServiceLeaseRequest{RuntimeProfileID: profile.RuntimeProfileID, Region: offer.Region, Currency: "usd",
		MinimumReplicas: 3, MaximumReplicas: 3, TermSeconds: 60, MaximumP95LatencyMilliseconds: 500,
		BuyerDeclaredCeilingNanos: 135_000_000}
	tooStrict := request
	tooStrict.MaximumP95LatencyMilliseconds = 199
	if got := post("/v1/service-leases", buyerKey, tooStrict).Code; got != http.StatusServiceUnavailable {
		t.Fatalf("lease admitted an offer that missed its measured p95: status=%d", got)
	}
	created := post("/v1/service-leases", buyerKey, request)
	if created.Code != http.StatusCreated {
		t.Fatalf("create lease status=%d body=%s", created.Code, created.Body.String())
	}
	var lease ServiceLease
	must(t, json.Unmarshal(created.Body.Bytes(), &lease))
	if lease.Pricing.ExecutionMode != pricingExecutionServiceLease || lease.Pricing.FixedPoint == nil ||
		lease.Pricing.FixedPoint.AcceptedCeilingNanos != request.BuyerDeclaredCeilingNanos || lease.ActiveReplicas != 3 ||
		lease.ReservedBuyerMicros != 135_000 || lease.PricingAcceptanceID == nil ||
		*lease.PricingAcceptanceID != lease.ID || lease.PricingAuthoritySource != serviceLeasePricingSourceAcceptance {
		t.Fatalf("lease lost frozen src/pricing/capacity authority: %+v", lease)
	}
	// The agent refreshes this offer continuously. Its static configuration says
	// it can warm three replicas, but all three are now reserved by this lease;
	// accepting the refresh verbatim would let a second buyer overbook the host.
	if got := post("/v1/worker/service-leases/offers", workerToken, offer).Code; got != http.StatusOK {
		t.Fatalf("periodic offer refresh status=%d", got)
	}
	var available int
	if err := pool.QueryRow(ctx, `SELECT available_warm_replicas FROM service_lease_worker_offers
		WHERE worker_id=$1 AND runtime_profile_id=$2 AND region=$3`, lease.WorkerID, profile.RuntimeProfileID, offer.Region).Scan(&available); err != nil {
		t.Fatal(err)
	}
	if available != 0 {
		t.Fatalf("offer refresh restored %d already-reserved replicas", available)
	}
	if got := post("/v1/service-leases", buyerKey, request).Code; got != http.StatusServiceUnavailable {
		t.Fatalf("second lease overbooked the refreshed offer: status=%d", got)
	}
	undersized := offer
	undersized.MaximumWarmReplicas = 2
	undersized.AvailableWarmReplicas = 2
	if got := post("/v1/worker/service-leases/offers", workerToken, undersized).Code; got != http.StatusConflict {
		t.Fatalf("offer reduced below active reservation: status=%d", got)
	}
	assignmentReq := httptest.NewRequest(http.MethodGet, "/v1/worker/service-leases/active", nil)
	assignmentReq.Header.Set("X-Worker-Token", workerToken)
	assignmentRec := httptest.NewRecorder()
	handler.ServeHTTP(assignmentRec, assignmentReq)
	if assignmentRec.Code != http.StatusOK {
		t.Fatalf("worker assignment status=%d body=%s", assignmentRec.Code, assignmentRec.Body.String())
	}
	var assignments []ServiceLeaseAssignment
	if err := json.Unmarshal(assignmentRec.Body.Bytes(), &assignments); err != nil || len(assignments) != 1 ||
		assignments[0].ID != lease.ID || assignments[0].RuntimeProfileID != profile.RuntimeProfileID ||
		assignments[0].MaximumP95LatencyMillis != request.MaximumP95LatencyMilliseconds {
		t.Fatalf("worker assignment leaked or lost lease authority: assignments=%+v err=%v", assignments, err)
	}
	_, unrelatedWorkerToken := newFabricMeasurementWorker(t, ctx, store)
	unrelatedReq := httptest.NewRequest(http.MethodGet, "/v1/worker/service-leases/active", nil)
	unrelatedReq.Header.Set("X-Worker-Token", unrelatedWorkerToken)
	unrelatedRec := httptest.NewRecorder()
	handler.ServeHTTP(unrelatedRec, unrelatedReq)
	var unrelatedAssignments []ServiceLeaseAssignment
	if unrelatedRec.Code != http.StatusOK || json.Unmarshal(unrelatedRec.Body.Bytes(), &unrelatedAssignments) != nil || len(unrelatedAssignments) != 0 {
		t.Fatalf("unrelated worker observed lease assignment: status=%d assignments=%+v", unrelatedRec.Code, unrelatedAssignments)
	}
	if _, err := pool.Exec(ctx, `UPDATE service_leases SET last_metered_at=now()-interval '2 seconds' WHERE id=$1`, lease.ID); err != nil {
		t.Fatal(err)
	}
	if got := post("/v1/worker/service-leases/"+lease.ID.String()+"/heartbeat", workerToken,
		ServiceLeaseHeartbeat{WarmReplicas: 3, P95LatencyMillis: 200, Status: "DRAINING", UpgradeGeneration: "image-v2"}).Code; got != http.StatusNoContent {
		t.Fatalf("upgrade begin heartbeat status=%d", got)
	}
	if got := post("/v1/worker/service-leases/"+lease.ID.String()+"/heartbeat", workerToken,
		ServiceLeaseHeartbeat{WarmReplicas: 3, P95LatencyMillis: 200, Status: "READY", UpgradeGeneration: "image-v3"}).Code; got != http.StatusConflict {
		t.Fatalf("unmeasured ready heartbeat status=%d", got)
	}
	if got := post("/v1/worker/service-leases/"+lease.ID.String()+"/heartbeat", workerToken,
		ServiceLeaseHeartbeat{WarmReplicas: 3, P95LatencyMillis: 200, LatencyMeasurementCount: 5,
			LatencyWindowSeconds: 15, LatencyMeasurementKind: "DATA_PLANE_COMPLETIONS_V1", Status: "READY",
			UpgradeGeneration: "image-v3"}).Code; got != http.StatusConflict {
		t.Fatalf("ready heartbeat without probe receipt status=%d", got)
	}
	if got := post("/v1/worker/service-leases/"+lease.ID.String()+"/heartbeat", workerToken,
		ServiceLeaseHeartbeat{WarmReplicas: 3, P95LatencyMillis: 200, LatencyMeasurementCount: 5,
			LatencyWindowSeconds: 15, LatencyMeasurementKind: "DATA_PLANE_COMPLETIONS_V1",
			DataPlaneProbeReceiptSHA256: strings.Repeat("a", 64), Status: "READY", UpgradeGeneration: "image-v3"}).Code; got != http.StatusNoContent {
		t.Fatalf("upgrade complete heartbeat status=%d", got)
	}
	req := httptest.NewRequest(http.MethodGet, "/v1/service-leases/"+lease.ID.String()+"/receipt", nil)
	req.Header.Set("Authorization", "Bearer "+buyerKey)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("receipt status=%d body=%s", rec.Code, rec.Body.String())
	}
	var receipt ServiceLeaseReceipt
	must(t, json.Unmarshal(rec.Body.Bytes(), &receipt))
	blockers := make(map[string]bool, len(receipt.ReceiptBlockers))
	for _, blocker := range receipt.ReceiptBlockers {
		blockers[blocker] = true
	}
	if receipt.Lease.State != "ACTIVE" || receipt.Lease.BuyerChargeNanos <= 0 ||
		receipt.Lease.BuyerChargeNanos != receipt.Lease.SupplierPayableNanos+receipt.Lease.KnownVariableCostNanos+receipt.Lease.KnownContributionNanos ||
		receipt.BuyerFundingState != "PREPAID_MAXIMUM_RESERVED" ||
		receipt.SupplierSettlementState != "ACCRUED_PREPAID_RESERVED_UNSETTLED" ||
		receipt.TrueNetContributionStatus != "UNKNOWN_ECONOMIC_FINALITY_BLOCKERS" ||
		receipt.Lease.Pricing.ServiceLease.RiskReserveNanosPerReplicaHour != 0 ||
		receipt.Lease.Pricing.RiskReserve.Status != pricingCostUnknown ||
		receipt.Lease.Pricing.EgressCost.Status != pricingCostUnknown ||
		receipt.EgressAuthorityStatus != "APPLICATION_BYTES_DIAGNOSTIC_ONLY_PROVIDER_BILLING_UNKNOWN" ||
		len(blockers) != 4 || !blockers[serviceLeaseBlockerProcessor] || !blockers[serviceLeaseBlockerEgress] ||
		!blockers[serviceLeaseBlockerResidency] || !blockers[serviceLeaseBlockerReserve] ||
		receipt.DataPlaneAuthorityStatus != "WORKER_ATTESTED_PROBE_NOT_BUYER_REQUEST" || receipt.LatestSLOEvidence == nil ||
		receipt.LatestSLOEvidence.P95LatencyMillis != 200 || receipt.LatestSLOEvidence.LatencyMeasurementCount != 5 ||
		receipt.LatestSLOEvidence.LatencyMeasurementKind != "DATA_PLANE_COMPLETIONS_V1" ||
		receipt.LatestSLOEvidence.DataPlaneProbeReceiptSHA256 != strings.Repeat("a", 64) {
		t.Fatalf("receipt overclaimed or lost exact money: %+v", receipt)
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
	must(t, rows.Err())
	has := func(want string) bool {
		for _, event := range events {
			if event == want {
				return true
			}
		}
		return false
	}
	if len(events) < 5 || !has("ACTIVATED") || !has("METERED") || !has("SLO_MEASURED") || !has("ROLLING_UPDATE_STARTED") || !has("ROLLING_UPDATE_COMPLETED") {
		t.Fatalf("rolling upgrade receipt events=%v", events)
	}
	var activationRaw []byte
	must(t, pool.QueryRow(ctx, `SELECT detail FROM service_lease_events WHERE lease_id=$1 AND kind='ACTIVATED' ORDER BY created_at,id LIMIT 1`, lease.ID).Scan(&activationRaw))
	var activation serviceLeaseActivationDetail
	must(t, json.Unmarshal(activationRaw, &activation))
	if activation.PricingDecisionSHA256 != lease.PricingDecisionSHA256 ||
		activation.Currency != "usd" || activation.ReservedCeilingNanos != request.BuyerDeclaredCeilingNanos ||
		activation.ReservedBuyerMicros != lease.ReservedBuyerMicros ||
		activation.SupplierFloorNanosPerReplicaHour != lease.Pricing.ServiceLease.SupplierNanosPerReplicaHour ||
		activation.ResidencyNanosPerReplicaHour != lease.Pricing.ServiceLease.ResidencyNanosPerReplicaHour ||
		activation.ControlNanosPerReplicaHour != lease.Pricing.ServiceLease.ControlPlaneNanosPerReplicaHour ||
		activation.RiskReserveNanosPerReplicaHour != lease.Pricing.ServiceLease.RiskReserveNanosPerReplicaHour ||
		activation.ContributionNanosPerReplicaHour != lease.Pricing.ServiceLease.ContributionNanosPerReplicaHour ||
		activation.BuyerChargeNanos != lease.Pricing.FixedPoint.BuyerChargeNanos ||
		activation.SupplierEntitlementsNanos != lease.Pricing.FixedPoint.SupplierEntitlementsNanos ||
		activation.KnownVariableCostsNanos != lease.Pricing.FixedPoint.KnownVariableCostsNanos ||
		activation.KnownCostContributionNanos != lease.Pricing.FixedPoint.KnownCostContributionNanos ||
		activation.RiskReserveNanosPerReplicaHour != 0 ||
		activation.ResidencyLiabilityPolicy != "SELECTED_SUPPLIER_ALL_IN_WARM_CAPACITY_ENTITLEMENT_V2" ||
		activation.TrueNetContributionStatus != "UNKNOWN_ECONOMIC_FINALITY_BLOCKERS" {
		t.Fatalf("activation event lost frozen economic authority: %+v lease=%+v", activation, lease.Pricing)
	}

	// Loss recovery charges only through the last authenticated heartbeat, then
	// accepts a different supplier only when it clears the frozen rate ceilings.
	fallback, _ := newFabricMeasurementWorker(t, ctx, store)
	seedMeasuredWarmResidency(t, ctx, pool, fallback.WorkerID, profile.ModelAlias)
	fallbackOffer := serviceLeaseOffer(profile)
	fallbackOffer.Region = request.Region
	must(t, store.UpsertServiceLeaseOffer(ctx, fallback, fallbackOffer))
	if _, err := pool.Exec(ctx, `UPDATE service_leases SET last_metered_at=now()-interval '60 seconds',last_worker_heartbeat_at=now()-interval '60 seconds' WHERE id=$1`, lease.ID); err != nil {
		t.Fatal(err)
	}
	if lost, err := store.RecoverServiceLeases(ctx, 10); err != nil || lost != 1 {
		t.Fatalf("worker loss recovery lost=%d err=%v", lost, err)
	}
	if moved, err := store.FailoverServiceLease(ctx, lease.ID); err != nil || !moved {
		t.Fatalf("failover moved=%v err=%v", moved, err)
	}
	recovered, err := store.GetServiceLeaseReceipt(ctx, buyerID, lease.ID)
	if err != nil || recovered.Lease.State != "ACTIVE" || recovered.Lease.WorkerID != fallback.WorkerID {
		t.Fatalf("failover receipt=%+v err=%v", recovered, err)
	}
	if _, err := pool.Exec(ctx, `UPDATE service_leases SET last_metered_at=now()-interval '2 seconds',expires_at=now()-interval '1 second' WHERE id=$1`, lease.ID); err != nil {
		t.Fatal(err)
	}
	if completed, err := store.FinalizeExpiredServiceLeases(ctx, 10); err != nil || completed != 1 {
		t.Fatalf("expiry finalization completed=%d err=%v", completed, err)
	}
	completedReceipt, err := store.GetServiceLeaseReceipt(ctx, buyerID, lease.ID)
	if err != nil || completedReceipt.Lease.State != "COMPLETED" || completedReceipt.Lease.FinalizedAt == nil {
		t.Fatalf("completed lease receipt=%+v err=%v", completedReceipt, err)
	}
	if completedReceipt.Settlement == nil ||
		completedReceipt.BuyerFundingState != "PREPAID_FINAL_DEBIT_RECORDED" ||
		completedReceipt.SupplierSettlementState != "SUPPLIER_CREDIT_HELD_PREPAID_COLLECTION_ALLOCATION_REQUIRED" ||
		completedReceipt.Settlement.Currency != "usd" ||
		completedReceipt.Settlement.BuyerChargeMicros != completedReceipt.Settlement.PrepaidDebitMicros ||
		completedReceipt.Settlement.BuyerChargeMicros != completedReceipt.Settlement.SupplierCreditMicros+completedReceipt.Settlement.PlatformGrossMicros ||
		completedReceipt.Settlement.SupplierPayoutStatus != PayoutHeld ||
		completedReceipt.Settlement.FundingAuthorityState != "PREPAID_CASH_COLLECTED_BUT_PAYOUT_COLLECTION_ALLOCATION_NOT_IMPLEMENTED" {
		t.Fatalf("completed service receipt overclaimed or lost terminal money: %+v", completedReceipt)
	}
	var supplierPayableNanos int64
	creditedSuppliers := map[uuid.UUID]bool{}
	for _, credit := range completedReceipt.Settlement.SupplierCredits {
		supplierPayableNanos += credit.PayableNanos
		creditedSuppliers[credit.SupplierID] = credit.CreditMicros > 0 && credit.PayoutStatus == PayoutHeld
	}
	if supplierPayableNanos != completedReceipt.Lease.SupplierPayableNanos ||
		len(creditedSuppliers) != 2 || !creditedSuppliers[primaryWorker.SupplierID] || !creditedSuppliers[fallback.SupplierID] {
		t.Fatalf("service failover payout was not attributed to each metered supplier: %+v", completedReceipt.Settlement.SupplierCredits)
	}
	// Every immutable failover interval credits the supplier's complete frozen
	// all-in ask. Residency is not left behind as platform gross merely because
	// the current worker changed between cumulative meter snapshots.
	meterRows, err := pool.Query(ctx, `SELECT m.cumulative_replica_nanoseconds,
		sm.supplier_id,sm.supplier_payable_delta_nanos
		FROM service_lease_meterings m
		JOIN service_lease_supplier_meterings sm USING (lease_id,sequence)
		WHERE m.lease_id=$1 ORDER BY m.sequence`, lease.ID)
	must(t, err)
	defer meterRows.Close()
	var previousCumulative, previousAllIn int64
	intervalSuppliers := map[uuid.UUID]bool{}
	for meterRows.Next() {
		var cumulative, payableDelta int64
		var supplierID uuid.UUID
		must(t, meterRows.Scan(&cumulative, &supplierID, &payableDelta))
		allIn, moneyErr := ServiceLeaseMoneyForReplicaDuration(MustParseCurrency("usd"),
			*completedReceipt.Lease.Pricing.ServiceLease, cumulative)
		must(t, moneyErr)
		if payableDelta != allIn.SupplierPayable.Nanos-previousAllIn {
			t.Fatalf("supplier %s interval [%d,%d] payable=%d, want all-in delta=%d",
				supplierID, previousCumulative, cumulative, payableDelta,
				allIn.SupplierPayable.Nanos-previousAllIn)
		}
		if payableDelta > 0 {
			intervalSuppliers[supplierID] = true
		}
		previousCumulative, previousAllIn = cumulative, allIn.SupplierPayable.Nanos
	}
	must(t, meterRows.Err())
	if !intervalSuppliers[primaryWorker.SupplierID] || !intervalSuppliers[fallback.SupplierID] {
		t.Fatalf("failover intervals did not carry all-in liability for both suppliers: %v", intervalSuppliers)
	}
	wantBuyerMicros, err := LedgerMicrosFromNanos(MoneyNanos{Currency: MustParseCurrency("usd"), Nanos: completedReceipt.Lease.BuyerChargeNanos})
	if err != nil || completedReceipt.Settlement.BuyerChargeMicros != wantBuyerMicros || wantBuyerMicros <= 0 {
		t.Fatalf("terminal buyer projection=%d receipt=%+v err=%v", wantBuyerMicros, completedReceipt.Settlement, err)
	}
	terminalRefs := []string{
		serviceLeaseLedgerRef(lease.ID, KindBuyerCharge), prepaidServiceLeaseDebitRef(lease.ID),
		serviceLeaseLedgerRef(lease.ID, KindPlatformTake),
	}
	for _, credit := range completedReceipt.Settlement.SupplierCredits {
		terminalRefs = append(terminalRefs, serviceLeaseSupplierCreditLedgerRef(lease.ID, credit.SupplierID))
	}
	var terminalLedgerRows int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM ledger_entries WHERE payout_ref=ANY($1)`, terminalRefs).Scan(&terminalLedgerRows); err != nil || terminalLedgerRows != len(terminalRefs) {
		t.Fatalf("terminal service ledger rows=%d err=%v, want %d exact rows", terminalLedgerRows, err, len(terminalRefs))
	}
	var phantomReserveRows int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM ledger_entries
		WHERE payout_ref LIKE $1 AND kind IN ('risk_reserve_accrual','risk_reserve_release','risk_reserve_consume')`,
		"service-lease-ledger-"+lease.ID.String()+":%").Scan(&phantomReserveRows); err != nil || phantomReserveRows != 0 {
		t.Fatalf("service lease acquired %d phantom risk-reserve ledger rows: %v", phantomReserveRows, err)
	}
	if completed, err := store.FinalizeExpiredServiceLeases(ctx, 10); err != nil || completed != 0 {
		t.Fatalf("terminal service settlement replay completed=%d err=%v", completed, err)
	}
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM ledger_entries WHERE payout_ref=ANY($1)`, terminalRefs).Scan(&terminalLedgerRows); err != nil || terminalLedgerRows != len(terminalRefs) {
		t.Fatalf("terminal service replay created duplicate ledger rows=%d err=%v", terminalLedgerRows, err)
	}
	availableAfterSettlement, err := store.BuyerPrepaidAvailableMicros(ctx, buyerID)
	if err != nil || availableAfterSettlement != 1_000_000-wantBuyerMicros {
		t.Fatalf("terminal service balance=%d err=%v, want %d", availableAfterSettlement, err, 1_000_000-wantBuyerMicros)
	}

	// A different buyer cannot inspect the lease, even though all receipt data is
	// non-prompt operational metadata.
	otherID := uuid.New()
	if _, err := pool.Exec(ctx, `INSERT INTO buyers (id,email) VALUES ($1,$2)`, otherID, otherID.String()+"@lease.invalid"); err != nil {
		t.Fatal(err)
	}
	_, otherKey, _, err := store.CreateAPIKey(ctx, otherID, "other", true)
	must(t, err)
	foreign := httptest.NewRequest(http.MethodGet, "/v1/service-leases/"+lease.ID.String(), nil)
	foreign.Header.Set("Authorization", "Bearer "+otherKey)
	foreignRec := httptest.NewRecorder()
	handler.ServeHTTP(foreignRec, foreign)
	if foreignRec.Code != http.StatusNotFound {
		t.Fatalf("foreign buyer receipt status=%d", foreignRec.Code)
	}
}

func TestBuyerCancelsServiceLeaseAtLastAuthenticatedMeterAndReleasesUnusedReserve(t *testing.T) {
	installSettlementCurrencyForTest(t, "usd")
	ctx, store, pool := openPayoutTestStore(t)
	buyerID := uuid.New()
	if _, err := pool.Exec(ctx, `INSERT INTO buyers (id,email) VALUES ($1,$2)`, buyerID, buyerID.String()+"@lease-cancel.invalid"); err != nil {
		t.Fatal(err)
	}
	must(t, store.SeedPrepaidBalance(ctx, buyerID, 1_000_000, "service-cancel-"+buyerID.String()))
	_, buyerKey, _, err := store.CreateAPIKey(ctx, buyerID, "lease-cancel", true)
	must(t, err)
	profile := sortedVLLMProfiles()[0]
	worker, workerToken := newFabricMeasurementWorker(t, ctx, store)
	seedMeasuredWarmResidency(t, ctx, pool, worker.WorkerID, profile.ModelAlias)
	offer := serviceLeaseOffer(profile)
	offer.Region = "ca-cancel-" + uuid.NewString()
	must(t, store.UpsertServiceLeaseOffer(ctx, worker, offer))
	lease, err := store.CreateServiceLease(ctx, buyerID, ServiceLeaseRequest{
		RuntimeProfileID: profile.RuntimeProfileID, Region: offer.Region, Currency: "usd",
		MinimumReplicas: 1, MaximumReplicas: 1, TermSeconds: 90, MaximumP95LatencyMilliseconds: 500,
		BuyerDeclaredCeilingNanos: 135_000_000,
	})
	must(t, err)
	// Model a verified worker observation that is older than the buyer's stop
	// request. Cancellation must meter through this point, not through now.
	observedAt := time.Now().UTC().Add(-2 * time.Second)
	if _, err := pool.Exec(ctx, `UPDATE service_leases
		SET started_at=$2::timestamptz-interval '2 seconds',last_metered_at=$2::timestamptz-interval '2 seconds',
		    last_worker_heartbeat_at=$2,expires_at=$2::timestamptz+interval '88 seconds' WHERE id=$1`, lease.ID, observedAt); err != nil {
		t.Fatal(err)
	}
	handler := NewServer(store, nil, nil, nil).Routes()
	cancel := func(token string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodPost, "/v1/service-leases/"+lease.ID.String()+"/cancel", nil)
		req.Header.Set("Authorization", "Bearer "+token)
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, req)
		return rec
	}
	rec := cancel(buyerKey)
	if rec.Code != http.StatusOK {
		t.Fatalf("cancel status=%d body=%s", rec.Code, rec.Body.String())
	}
	var receipt ServiceLeaseReceipt
	must(t, json.Unmarshal(rec.Body.Bytes(), &receipt))
	if receipt.Lease.State != "CANCELLED" || receipt.Lease.FinalizedAt == nil || receipt.Settlement == nil ||
		receipt.Settlement.BuyerChargeMicros <= 0 ||
		receipt.Settlement.BuyerChargeMicros != receipt.Settlement.PrepaidDebitMicros ||
		receipt.Settlement.BuyerChargeMicros != receipt.Settlement.SupplierCreditMicros+receipt.Settlement.PlatformGrossMicros ||
		receipt.BuyerFundingState != "PREPAID_FINAL_DEBIT_RECORDED" ||
		receipt.SupplierSettlementState != "SUPPLIER_CREDIT_HELD_PREPAID_COLLECTION_ALLOCATION_REQUIRED" {
		t.Fatalf("cancel receipt lost terminal money or authority: %+v", receipt)
	}
	if receipt.Lease.LastMeteredAt.After(observedAt.Add(250 * time.Millisecond)) {
		t.Fatalf("buyer cancellation billed beyond authenticated worker observation: metered=%s observed=%s", receipt.Lease.LastMeteredAt, observedAt)
	}
	available, err := store.BuyerPrepaidAvailableMicros(ctx, buyerID)
	if err != nil || available != 1_000_000-receipt.Settlement.BuyerChargeMicros {
		t.Fatalf("cancelled prepaid balance=%d err=%v, want %d", available, err, 1_000_000-receipt.Settlement.BuyerChargeMicros)
	}
	var offerAvailable int
	if err := pool.QueryRow(ctx, `SELECT available_warm_replicas FROM service_lease_worker_offers
		WHERE worker_id=$1 AND runtime_profile_id=$2 AND region=$3`, worker.WorkerID, profile.RuntimeProfileID, offer.Region).Scan(&offerAvailable); err != nil || offerAvailable != 3 {
		t.Fatalf("cancel did not release warm capacity: available=%d err=%v", offerAvailable, err)
	}
	assignmentsReq := httptest.NewRequest(http.MethodGet, "/v1/worker/service-leases/active", nil)
	assignmentsReq.Header.Set("X-Worker-Token", workerToken)
	assignmentsRec := httptest.NewRecorder()
	handler.ServeHTTP(assignmentsRec, assignmentsReq)
	if assignmentsRec.Code != http.StatusOK || strings.TrimSpace(assignmentsRec.Body.String()) != "[]" {
		t.Fatalf("cancelled lease remained an agent assignment: status=%d body=%s", assignmentsRec.Code, assignmentsRec.Body.String())
	}
	var terminalRows int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM ledger_entries WHERE payout_ref=ANY($1)`, []string{
		serviceLeaseLedgerRef(lease.ID, KindBuyerCharge), prepaidServiceLeaseDebitRef(lease.ID),
		serviceLeaseLedgerRef(lease.ID, KindPlatformTake), serviceLeaseSupplierCreditLedgerRef(lease.ID, worker.SupplierID),
	}).Scan(&terminalRows); err != nil || terminalRows != 4 {
		t.Fatalf("cancel terminal ledger rows=%d err=%v, want 4", terminalRows, err)
	}
	// Cancellation is idempotent: a retry exposes the same immutable receipt and
	// cannot create another debit, supplier credit, or capacity release.
	if retry := cancel(buyerKey); retry.Code != http.StatusOK {
		t.Fatalf("cancel retry status=%d body=%s", retry.Code, retry.Body.String())
	}
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM ledger_entries WHERE payout_ref=ANY($1)`, []string{
		serviceLeaseLedgerRef(lease.ID, KindBuyerCharge), prepaidServiceLeaseDebitRef(lease.ID),
		serviceLeaseLedgerRef(lease.ID, KindPlatformTake), serviceLeaseSupplierCreditLedgerRef(lease.ID, worker.SupplierID),
	}).Scan(&terminalRows); err != nil || terminalRows != 4 {
		t.Fatalf("cancel retry duplicated terminal ledger: rows=%d err=%v", terminalRows, err)
	}
	workerHeartbeat := httptest.NewRequest(http.MethodPost, "/v1/worker/service-leases/"+lease.ID.String()+"/heartbeat", bytes.NewReader([]byte(`{"warm_replicas":1,"p95_latency_milliseconds":200,"latency_measurement_count":5,"latency_window_seconds":15,"latency_measurement_kind":"DATA_PLANE_COMPLETIONS_V1","data_plane_probe_receipt_sha256":"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa","status":"READY"}`)))
	workerHeartbeat.Header.Set("X-Worker-Token", workerToken)
	workerHeartbeatRec := httptest.NewRecorder()
	handler.ServeHTTP(workerHeartbeatRec, workerHeartbeat)
	if workerHeartbeatRec.Code != http.StatusConflict {
		t.Fatalf("cancelled lease accepted a later worker heartbeat: status=%d", workerHeartbeatRec.Code)
	}
}

func TestServiceLeaseAdmissionRefusesReplicaRangeWithoutHoldingSupplyOrMoney(t *testing.T) {
	installSettlementCurrencyForTest(t, "usd")
	ctx, store, pool := openIsolatedTestStore(t)
	buyerID := uuid.New()
	if _, err := pool.Exec(ctx, `INSERT INTO buyers (id,email) VALUES ($1,$2)`, buyerID, buyerID.String()+"@lease-range.invalid"); err != nil {
		t.Fatal(err)
	}
	must(t, store.SeedPrepaidBalance(ctx, buyerID, 1_000_000, "service-range-"+buyerID.String()))
	profile := sortedVLLMProfiles()[0]
	worker, _ := newFabricMeasurementWorker(t, ctx, store)
	seedMeasuredWarmResidency(t, ctx, pool, worker.WorkerID, profile.ModelAlias)
	offer := serviceLeaseOffer(profile)
	offer.Region = "ca-range-" + uuid.NewString()
	must(t, store.UpsertServiceLeaseOffer(ctx, worker, offer))

	var availableBefore int
	must(t, pool.QueryRow(ctx, `SELECT available_warm_replicas FROM service_lease_worker_offers
		WHERE worker_id=$1 AND runtime_profile_id=$2 AND region=$3`,
		worker.WorkerID, profile.RuntimeProfileID, offer.Region).Scan(&availableBefore))
	prepaidBefore, err := store.BuyerPrepaidAvailableMicros(ctx, buyerID)
	must(t, err)

	// min < max is the undeliverable elasticity shape.
	_, err = store.CreateServiceLease(ctx, buyerID, ServiceLeaseRequest{
		RuntimeProfileID: profile.RuntimeProfileID, Region: offer.Region, Currency: "usd",
		MinimumReplicas: 1, MaximumReplicas: 10, TermSeconds: 60, MaximumP95LatencyMilliseconds: 500,
		BuyerDeclaredCeilingNanos: 135_000_000,
	})
	if err == nil || !strings.Contains(err.Error(), "minimum_replicas != maximum_replicas") {
		t.Fatalf("min<max admission error=%v, want named replica-range refusal", err)
	}

	var availableAfter int
	must(t, pool.QueryRow(ctx, `SELECT available_warm_replicas FROM service_lease_worker_offers
		WHERE worker_id=$1 AND runtime_profile_id=$2 AND region=$3`,
		worker.WorkerID, profile.RuntimeProfileID, offer.Region).Scan(&availableAfter))
	prepaidAfter, err := store.BuyerPrepaidAvailableMicros(ctx, buyerID)
	must(t, err)
	var leaseRows int
	must(t, pool.QueryRow(ctx, `SELECT count(*) FROM service_leases WHERE buyer_id=$1`, buyerID).Scan(&leaseRows))
	if availableAfter != availableBefore || prepaidAfter != prepaidBefore || leaseRows != 0 {
		t.Fatalf("refusal held durable state: available %d→%d prepaid %d→%d leases=%d",
			availableBefore, availableAfter, prepaidBefore, prepaidAfter, leaseRows)
	}

	// Equal bounds still admit at 1 and at N (separate capacity so holds do not collide).
	for _, n := range []int{1, 3} {
		equalOffer := serviceLeaseOffer(profile)
		equalOffer.Region = fmt.Sprintf("ca-range-eq-%d-%s", n, uuid.NewString())
		equalOffer.MaximumWarmReplicas, equalOffer.AvailableWarmReplicas = n, n
		must(t, store.UpsertServiceLeaseOffer(ctx, worker, equalOffer))
		lease, createErr := store.CreateServiceLease(ctx, buyerID, ServiceLeaseRequest{
			RuntimeProfileID: profile.RuntimeProfileID, Region: equalOffer.Region, Currency: "usd",
			MinimumReplicas: n, MaximumReplicas: n, TermSeconds: 60, MaximumP95LatencyMilliseconds: 500,
			BuyerDeclaredCeilingNanos: 135_000_000,
		})
		if createErr != nil || lease.MinimumReplicas != n || lease.MaximumReplicas != n || lease.ActiveReplicas != n {
			t.Fatalf("min==max=%d admission lease=%+v err=%v", n, lease, createErr)
		}
	}
}

func TestServiceLeaseAdmissionReplicaRangeRefusalIsClientVisible(t *testing.T) {
	installSettlementCurrencyForTest(t, "usd")
	ctx, store, pool := openIsolatedTestStore(t)
	buyerID := uuid.New()
	if _, err := pool.Exec(ctx, `INSERT INTO buyers (id,email) VALUES ($1,$2)`, buyerID, buyerID.String()+"@lease-range-api.invalid"); err != nil {
		t.Fatal(err)
	}
	must(t, store.SeedPrepaidBalance(ctx, buyerID, 1_000_000, "service-range-api-"+buyerID.String()))
	_, buyerKey, _, err := store.CreateAPIKey(ctx, buyerID, "lease-range-api", true)
	must(t, err)
	profile := sortedVLLMProfiles()[0]
	worker, _ := newFabricMeasurementWorker(t, ctx, store)
	seedMeasuredWarmResidency(t, ctx, pool, worker.WorkerID, profile.ModelAlias)
	offer := serviceLeaseOffer(profile)
	offer.Region = "ca-range-api-" + uuid.NewString()
	must(t, store.UpsertServiceLeaseOffer(ctx, worker, offer))

	handler := NewServer(store, nil, nil, nil).Routes()
	raw, _ := json.Marshal(ServiceLeaseRequest{
		RuntimeProfileID: profile.RuntimeProfileID, Region: offer.Region, Currency: "usd",
		MinimumReplicas: 1, MaximumReplicas: 5, TermSeconds: 60, MaximumP95LatencyMilliseconds: 500,
		BuyerDeclaredCeilingNanos: 135_000_000,
	})
	req := httptest.NewRequest(http.MethodPost, "/v1/service-leases", bytes.NewReader(raw))
	req.Header.Set("Authorization", "Bearer "+buyerKey)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusBadRequest || !strings.Contains(rec.Body.String(), "minimum_replicas != maximum_replicas") {
		t.Fatalf("API refusal status=%d body=%s", rec.Code, rec.Body.String())
	}
}

func TestServiceLeaseHistoricalReplicaRangeStillHeartbeatsUpgradesAndCancels(t *testing.T) {
	installSettlementCurrencyForTest(t, "usd")
	ctx, store, pool := openIsolatedTestStore(t)
	buyerID := uuid.New()
	if _, err := pool.Exec(ctx, `INSERT INTO buyers (id,email) VALUES ($1,$2)`, buyerID, buyerID.String()+"@lease-legacy-range.invalid"); err != nil {
		t.Fatal(err)
	}
	must(t, store.SeedPrepaidBalance(ctx, buyerID, 100_000_000, "service-legacy-range-"+buyerID.String()))
	profile := sortedVLLMProfiles()[0]
	worker, _ := newFabricMeasurementWorker(t, ctx, store)
	seedMeasuredWarmResidency(t, ctx, pool, worker.WorkerID, profile.ModelAlias)
	offer := serviceLeaseOffer(profile)
	offer.Region = "ca-legacy-range-" + uuid.NewString()
	offer.MaximumWarmReplicas, offer.AvailableWarmReplicas = 4, 4
	must(t, store.UpsertServiceLeaseOffer(ctx, worker, offer))

	// Directly seed a pre-refusal durable row (min < max). CreateServiceLease
	// refuses this shape; the acceptance+lease pair is what a pre-change
	// admission would have frozen.
	request := ServiceLeaseRequest{
		RuntimeProfileID: profile.RuntimeProfileID, Region: offer.Region, Currency: "usd",
		MinimumReplicas: 1, MaximumReplicas: 4, TermSeconds: 120, MaximumP95LatencyMilliseconds: 500,
		BuyerDeclaredCeilingNanos: 10_000_000_000,
	}
	pricing, err := newServiceLeasePricingDecision(serviceLeasePricingInputs(profile, MustParseCurrency("usd"), request,
		offer.SupplierNanosPerReplicaHour, offer.ResidencyNanosPerReplicaHour))
	must(t, err)
	pricingJSON, err := json.Marshal(pricing)
	must(t, err)
	pricingSHA, err := pricingDecisionDigest(pricing)
	must(t, err)
	reservedMicros, err := LedgerMicrosFromNanos(MoneyNanos{Currency: MustParseCurrency("usd"), Nanos: pricing.FixedPoint.AcceptedCeilingNanos})
	must(t, err)
	leaseID := uuid.New()
	now := time.Now().UTC()
	expires := now.Add(120 * time.Second)
	tx, err := pool.Begin(ctx)
	must(t, err)
	defer tx.Rollback(ctx)
	must(t, insertServiceLeasePricingAcceptanceTx(ctx, tx, leaseID, pricingJSON, pricingSHA))
	if _, err := tx.Exec(ctx, `
		INSERT INTO service_leases
		 (id,buyer_id,worker_id,supplier_id,runtime_profile_id,runtime_profile_sha256,region,
		  minimum_replicas,maximum_replicas,maximum_p95_latency_milliseconds,term_seconds,state,
		  active_replicas,reserved_buyer_micros,pricing_acceptance_id,pricing_decision,pricing_decision_sha256,
		  started_at,expires_at,last_metered_at,last_worker_heartbeat_at)
		VALUES ($1,$2,$3,$4,$5,$6,$7,1,4,500,120,'ACTIVE',1,$8,$1,$9::jsonb,$10,$11,$12,$11,$11)`,
		leaseID, buyerID, worker.WorkerID, worker.SupplierID, profile.RuntimeProfileID, profile.ProfileSHA256,
		offer.Region, reservedMicros, pricingJSON, pricingSHA, now, expires); err != nil {
		t.Fatal(err)
	}
	if _, err := tx.Exec(ctx, `UPDATE service_lease_worker_offers
		SET available_warm_replicas=available_warm_replicas-4,updated_at=now()
		WHERE worker_id=$1 AND runtime_profile_id=$2 AND region=$3`,
		worker.WorkerID, profile.RuntimeProfileID, offer.Region); err != nil {
		t.Fatal(err)
	}
	must(t, reservePrepaidForServiceLeaseTx(ctx, tx, buyerID, reservedMicros))
	must(t, tx.Commit(ctx))

	must(t, store.HeartbeatServiceLease(ctx, worker, leaseID, ServiceLeaseHeartbeat{
		WarmReplicas: 1, P95LatencyMillis: 200, Status: "DRAINING", UpgradeGeneration: "legacy-v2",
	}))
	must(t, store.HeartbeatServiceLease(ctx, worker, leaseID, ServiceLeaseHeartbeat{
		WarmReplicas: 1, P95LatencyMillis: 200, LatencyMeasurementCount: 5, LatencyWindowSeconds: 15,
		LatencyMeasurementKind:      "DATA_PLANE_COMPLETIONS_V1",
		DataPlaneProbeReceiptSHA256: strings.Repeat("b", 64), Status: "READY", UpgradeGeneration: "legacy-v3",
	}))
	if _, _, cancelErr := store.CancelServiceLease(ctx, buyerID, leaseID); cancelErr != nil {
		t.Fatalf("historical min<max cancel: %v", cancelErr)
	}
}

func TestServiceLeaseP99BoundEnforcedOnReadyHeartbeat(t *testing.T) {
	installSettlementCurrencyForTest(t, "usd")
	ctx, store, pool := openIsolatedTestStore(t)
	buyerID := uuid.New()
	if _, err := pool.Exec(ctx, `INSERT INTO buyers (id,email) VALUES ($1,$2)`, buyerID, buyerID.String()+"@lease-p99.invalid"); err != nil {
		t.Fatal(err)
	}
	must(t, store.SeedPrepaidBalance(ctx, buyerID, 1_000_000, "service-p99-"+buyerID.String()))
	profile := sortedVLLMProfiles()[0]
	worker, _ := newFabricMeasurementWorker(t, ctx, store)
	seedMeasuredWarmResidency(t, ctx, pool, worker.WorkerID, profile.ModelAlias)
	offer := serviceLeaseOffer(profile)
	offer.Region = "ca-p99-" + uuid.NewString()
	offer.P99LatencyMillis = 300
	must(t, store.UpsertServiceLeaseOffer(ctx, worker, offer))

	lease, err := store.CreateServiceLease(ctx, buyerID, ServiceLeaseRequest{
		RuntimeProfileID: profile.RuntimeProfileID, Region: offer.Region, Currency: "usd",
		MinimumReplicas: 1, MaximumReplicas: 1, TermSeconds: 60, MaximumP95LatencyMilliseconds: 500,
		MaximumP99LatencyMilliseconds: 400, BuyerDeclaredCeilingNanos: 135_000_000,
	})
	must(t, err)
	if lease.MaximumP99LatencyMillis != 400 {
		t.Fatalf("lease lost p99 bound: %+v", lease)
	}

	// p95 within bound but p99 over bound must fail naming p99.
	err = store.HeartbeatServiceLease(ctx, worker, lease.ID, ServiceLeaseHeartbeat{
		WarmReplicas: 1, P95LatencyMillis: 200, P99LatencyMillis: 401,
		LatencyMeasurementCount: 5, LatencyWindowSeconds: 15,
		LatencyMeasurementKind:      "DATA_PLANE_COMPLETIONS_V1",
		DataPlaneProbeReceiptSHA256: strings.Repeat("c", 64), Status: "READY",
	})
	if err == nil || !strings.Contains(err.Error(), "p99") {
		t.Fatalf("p99 breach error=%v, want p99 named refusal", err)
	}

	// Measured p99 within bound reaches READY and is durable on the receipt.
	must(t, store.HeartbeatServiceLease(ctx, worker, lease.ID, ServiceLeaseHeartbeat{
		WarmReplicas: 1, P95LatencyMillis: 200, P99LatencyMillis: 350,
		LatencyMeasurementCount: 5, LatencyWindowSeconds: 15,
		LatencyMeasurementKind:      "DATA_PLANE_COMPLETIONS_V1",
		DataPlaneProbeReceiptSHA256: strings.Repeat("d", 64), Status: "READY",
	}))
	receipt, err := store.GetServiceLeaseReceipt(ctx, buyerID, lease.ID)
	must(t, err)
	if receipt.LatestSLOEvidence == nil || receipt.LatestSLOEvidence.P99LatencyMillis != 350 ||
		receipt.LatestSLOEvidence.P95LatencyMillis != 200 {
		t.Fatalf("receipt lost measured p99 evidence: %+v", receipt.LatestSLOEvidence)
	}
}

func TestServiceLeaseWithoutP99BoundIgnoresReportedP99(t *testing.T) {
	installSettlementCurrencyForTest(t, "usd")
	ctx, store, pool := openIsolatedTestStore(t)
	buyerID := uuid.New()
	if _, err := pool.Exec(ctx, `INSERT INTO buyers (id,email) VALUES ($1,$2)`, buyerID, buyerID.String()+"@lease-no-p99.invalid"); err != nil {
		t.Fatal(err)
	}
	must(t, store.SeedPrepaidBalance(ctx, buyerID, 1_000_000, "service-no-p99-"+buyerID.String()))
	profile := sortedVLLMProfiles()[0]
	worker, _ := newFabricMeasurementWorker(t, ctx, store)
	seedMeasuredWarmResidency(t, ctx, pool, worker.WorkerID, profile.ModelAlias)
	offer := serviceLeaseOffer(profile)
	offer.Region = "ca-no-p99-" + uuid.NewString()
	must(t, store.UpsertServiceLeaseOffer(ctx, worker, offer))

	lease, err := store.CreateServiceLease(ctx, buyerID, ServiceLeaseRequest{
		RuntimeProfileID: profile.RuntimeProfileID, Region: offer.Region, Currency: "usd",
		MinimumReplicas: 1, MaximumReplicas: 1, TermSeconds: 60, MaximumP95LatencyMilliseconds: 500,
		BuyerDeclaredCeilingNanos: 135_000_000,
	})
	must(t, err)
	if lease.MaximumP99LatencyMillis != 0 {
		t.Fatalf("unexpected p99 bound on unbound lease: %+v", lease)
	}
	// High p99 with no declared bound must not fail READY (p95 still within bound).
	must(t, store.HeartbeatServiceLease(ctx, worker, lease.ID, ServiceLeaseHeartbeat{
		WarmReplicas: 1, P95LatencyMillis: 200, P99LatencyMillis: 9_999,
		LatencyMeasurementCount: 5, LatencyWindowSeconds: 15,
		LatencyMeasurementKind:      "DATA_PLANE_COMPLETIONS_V1",
		DataPlaneProbeReceiptSHA256: strings.Repeat("e", 64), Status: "READY",
	}))
}
