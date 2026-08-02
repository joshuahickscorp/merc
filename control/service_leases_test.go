package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"

	"github.com/google/uuid"
)

func serviceLeaseOffer(profile VLLMRuntimeProfile) ServiceLeaseOfferRegistration {
	return ServiceLeaseOfferRegistration{
		RuntimeProfileID: profile.RuntimeProfileID, RuntimeProfileSHA256: profile.ProfileSHA256,
		Region: "ca-central-1", MaximumWarmReplicas: 3, AvailableWarmReplicas: 3,
		SupplierNanosPerReplicaHour: 2_000_000_000, ResidencyNanosPerReplicaHour: 200_000_000,
		SupportsRollingUpgrade: true, P95LatencyMillis: 200, LatencyMeasurementCount: 5,
		LatencyWindowSeconds: 15, LatencyMeasurementKind: "DATA_PLANE_COMPLETIONS_V1", Status: "READY",
	}
}

func TestServiceLeaseOfferRefreshCannotRaceCapacityReservation(t *testing.T) {
	installSettlementCurrencyForTest(t, "cad")
	ctx, store, pool := openPayoutTestStore(t)
	profile := sortedVLLMProfiles()[0]
	worker, _ := newFabricMeasurementWorker(t, ctx, store)
	offer := serviceLeaseOffer(profile)
	offer.MaximumWarmReplicas, offer.AvailableWarmReplicas = 1, 1
	if err := store.UpsertServiceLeaseOffer(ctx, worker, offer); err != nil {
		t.Fatal(err)
	}
	buyers := []uuid.UUID{uuid.New(), uuid.New()}
	for _, buyerID := range buyers {
		if _, err := pool.Exec(ctx, `INSERT INTO buyers (id,email,free_credit_usd) VALUES ($1,$2,10)`, buyerID, buyerID.String()+"@lease-race.invalid"); err != nil {
			t.Fatal(err)
		}
		// A service may not use the legacy sandbox grant as cash authority.
		// Give each racing buyer actual prepaid liability; capacity, not an
		// accidental funding shortage, is what this test is measuring.
		if err := store.SeedPrepaidBalance(ctx, buyerID, 1_000_000, "service-race-"+buyerID.String()); err != nil {
			t.Fatal(err)
		}
	}
	request := ServiceLeaseRequest{RuntimeProfileID: profile.RuntimeProfileID, Region: offer.Region,
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
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM service_leases WHERE worker_id=$1 AND state='ACTIVE'`, worker.WorkerID).Scan(&active); err != nil {
		t.Fatal(err)
	}
	if err := pool.QueryRow(ctx, `SELECT available_warm_replicas FROM service_lease_worker_offers
		WHERE worker_id=$1 AND runtime_profile_id=$2 AND region=$3`, worker.WorkerID, profile.RuntimeProfileID, offer.Region).Scan(&available); err != nil {
		t.Fatal(err)
	}
	if active != 1 || available != 0 {
		t.Fatalf("one warm replica became active=%d available=%d after concurrent refreshes", active, available)
	}
}

func TestServiceLeaseRequiresCollectedPrepaidCashAndFreezesItsMaximum(t *testing.T) {
	installSettlementCurrencyForTest(t, "cad")
	ctx, store, pool := openPayoutTestStore(t)
	buyerID := uuid.New()
	// The legacy grant is intentionally generous: if this path ever starts
	// reading free_credit_usd again, the first admission below would succeed.
	if _, err := pool.Exec(ctx, `INSERT INTO buyers (id,email,free_credit_usd) VALUES ($1,$2,1000)`, buyerID, buyerID.String()+"@service-prepaid.invalid"); err != nil {
		t.Fatal(err)
	}
	profile := sortedVLLMProfiles()[0]
	worker, _ := newFabricMeasurementWorker(t, ctx, store)
	offer := serviceLeaseOffer(profile)
	if err := store.UpsertServiceLeaseOffer(ctx, worker, offer); err != nil {
		t.Fatal(err)
	}
	request := ServiceLeaseRequest{RuntimeProfileID: profile.RuntimeProfileID, Region: offer.Region,
		MinimumReplicas: 1, MaximumReplicas: 3, TermSeconds: 60, MaximumP95LatencyMilliseconds: 500,
		BuyerDeclaredCeilingNanos: 135_000_000}
	pricing, err := newServiceLeasePricingDecision(serviceLeasePricingInputs(profile, MustParseCurrency("cad"), request,
		offer.SupplierNanosPerReplicaHour, offer.ResidencyNanosPerReplicaHour))
	if err != nil {
		t.Fatal(err)
	}
	reserve, err := LedgerMicrosFromNanos(MoneyNanos{Currency: MustParseCurrency("cad"), Nanos: pricing.FixedPoint.AcceptedCeilingNanos})
	if err != nil || reserve <= 1 {
		t.Fatalf("service reserve=%d err=%v", reserve, err)
	}
	if _, err := store.CreateServiceLease(ctx, buyerID, request); !errors.Is(err, errRealtimeInsufficientFunds) {
		t.Fatalf("lease used free credit rather than prepaid cash: %v", err)
	}
	if err := store.SeedPrepaidBalance(ctx, buyerID, reserve-1, "service-underfunded-"+buyerID.String()); err != nil {
		t.Fatal(err)
	}
	if _, err := store.CreateServiceLease(ctx, buyerID, request); !errors.Is(err, errRealtimeInsufficientFunds) {
		t.Fatalf("lease accepted one micro below its frozen maximum: %v", err)
	}
	if err := store.SeedPrepaidBalance(ctx, buyerID, 1, "service-exact-"+buyerID.String()); err != nil {
		t.Fatal(err)
	}
	lease, err := store.CreateServiceLease(ctx, buyerID, request)
	if err != nil || lease.ReservedBuyerMicros != reserve {
		t.Fatalf("exact prepaid service admission lease=%+v err=%v", lease, err)
	}
	available, err := store.BuyerPrepaidAvailableMicros(ctx, buyerID)
	if err != nil || available != 0 {
		t.Fatalf("active service left %d refundable/unreserved micros err=%v, want 0", available, err)
	}
}

func TestServiceLeaseCADBuyerAndWorkerPathUsesFrozenPricingAndCumulativeMetering(t *testing.T) {
	installSettlementCurrencyForTest(t, "cad")
	ctx, store, pool := openPayoutTestStore(t)
	buyerID := uuid.New()
	if _, err := pool.Exec(ctx, `INSERT INTO buyers (id,email,free_credit_usd) VALUES ($1,$2,10)`, buyerID, buyerID.String()+"@lease.invalid"); err != nil {
		t.Fatal(err)
	}
	if err := store.SeedPrepaidBalance(ctx, buyerID, 1_000_000, "service-path-"+buyerID.String()); err != nil {
		t.Fatal(err)
	}
	_, buyerKey, _, err := store.CreateAPIKey(ctx, buyerID, "lease-path", true)
	if err != nil {
		t.Fatal(err)
	}
	primaryWorker, workerToken := newFabricMeasurementWorker(t, ctx, store)
	profile := sortedVLLMProfiles()[0]
	handler := NewServer(store, nil, nil, nil).Routes()

	post := func(path, token string, body any) *httptest.ResponseRecorder {
		raw, err := json.Marshal(body)
		if err != nil {
			t.Fatal(err)
		}
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
	if got := post("/v1/worker/service-leases/offers", workerToken, serviceLeaseOffer(profile)).Code; got != http.StatusOK {
		t.Fatalf("offer registration status=%d", got)
	}
	request := ServiceLeaseRequest{RuntimeProfileID: profile.RuntimeProfileID, Region: "ca-central-1",
		MinimumReplicas: 1, MaximumReplicas: 3, TermSeconds: 60, MaximumP95LatencyMilliseconds: 500,
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
	if err := json.Unmarshal(created.Body.Bytes(), &lease); err != nil {
		t.Fatal(err)
	}
	if lease.Pricing.ExecutionMode != pricingExecutionServiceLease || lease.Pricing.FixedPoint == nil ||
		lease.Pricing.FixedPoint.AcceptedCeilingNanos != request.BuyerDeclaredCeilingNanos || lease.ActiveReplicas != 1 ||
		lease.ReservedBuyerMicros != 135_000 {
		t.Fatalf("lease lost frozen pricing/capacity authority: %+v", lease)
	}
	// The agent refreshes this offer continuously. Its static configuration says
	// it can warm three replicas, but all three are now reserved by this lease;
	// accepting the refresh verbatim would let a second buyer overbook the host.
	if got := post("/v1/worker/service-leases/offers", workerToken, serviceLeaseOffer(profile)).Code; got != http.StatusOK {
		t.Fatalf("periodic offer refresh status=%d", got)
	}
	var available int
	if err := pool.QueryRow(ctx, `SELECT available_warm_replicas FROM service_lease_worker_offers
		WHERE worker_id=$1 AND runtime_profile_id=$2 AND region=$3`, lease.WorkerID, profile.RuntimeProfileID, "ca-central-1").Scan(&available); err != nil {
		t.Fatal(err)
	}
	if available != 0 {
		t.Fatalf("offer refresh restored %d already-reserved replicas", available)
	}
	if got := post("/v1/service-leases", buyerKey, request).Code; got != http.StatusServiceUnavailable {
		t.Fatalf("second lease overbooked the refreshed offer: status=%d", got)
	}
	undersized := serviceLeaseOffer(profile)
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
		ServiceLeaseHeartbeat{WarmReplicas: 1, P95LatencyMillis: 200, Status: "DRAINING", UpgradeGeneration: "image-v2"}).Code; got != http.StatusNoContent {
		t.Fatalf("upgrade begin heartbeat status=%d", got)
	}
	if got := post("/v1/worker/service-leases/"+lease.ID.String()+"/heartbeat", workerToken,
		ServiceLeaseHeartbeat{WarmReplicas: 1, P95LatencyMillis: 200, Status: "READY", UpgradeGeneration: "image-v3"}).Code; got != http.StatusConflict {
		t.Fatalf("unmeasured ready heartbeat status=%d", got)
	}
	if got := post("/v1/worker/service-leases/"+lease.ID.String()+"/heartbeat", workerToken,
		ServiceLeaseHeartbeat{WarmReplicas: 1, P95LatencyMillis: 200, LatencyMeasurementCount: 5,
			LatencyWindowSeconds: 15, LatencyMeasurementKind: "DATA_PLANE_COMPLETIONS_V1", Status: "READY", UpgradeGeneration: "image-v3"}).Code; got != http.StatusNoContent {
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
	if err := json.Unmarshal(rec.Body.Bytes(), &receipt); err != nil {
		t.Fatal(err)
	}
	if receipt.Lease.State != "ACTIVE" || receipt.Lease.BuyerChargeNanos <= 0 ||
		receipt.Lease.BuyerChargeNanos != receipt.Lease.SupplierPayableNanos+receipt.Lease.KnownVariableCostNanos+receipt.Lease.KnownContributionNanos ||
		receipt.BuyerFundingState != "PREPAID_MAXIMUM_RESERVED" ||
		receipt.SupplierSettlementState != "ACCRUED_PREPAID_RESERVED_UNSETTLED" ||
		receipt.TrueNetContributionStatus != "UNKNOWN_PROCESSOR_FEE_UNALLOCATED" ||
		receipt.DataPlaneAuthorityStatus != "NOT_PROVEN_BY_CONTROL_PLANE" || receipt.LatestSLOEvidence == nil ||
		receipt.LatestSLOEvidence.P95LatencyMillis != 200 || receipt.LatestSLOEvidence.LatencyMeasurementCount != 5 ||
		receipt.LatestSLOEvidence.LatencyMeasurementKind != "DATA_PLANE_COMPLETIONS_V1" {
		t.Fatalf("receipt overclaimed or lost exact money: %+v", receipt)
	}
	var events []string
	rows, err := pool.Query(ctx, `SELECT kind FROM service_lease_events WHERE lease_id=$1 ORDER BY created_at,id`, lease.ID)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	for rows.Next() {
		var kind string
		if err := rows.Scan(&kind); err != nil {
			t.Fatal(err)
		}
		events = append(events, kind)
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
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

	// Loss recovery charges only through the last authenticated heartbeat, then
	// accepts a different supplier only when it clears the frozen rate ceilings.
	fallback, _ := newFabricMeasurementWorker(t, ctx, store)
	if err := store.UpsertServiceLeaseOffer(ctx, fallback, serviceLeaseOffer(profile)); err != nil {
		t.Fatal(err)
	}
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
		completedReceipt.Settlement.Currency != "cad" ||
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
	wantBuyerMicros, err := LedgerMicrosFromNanos(MoneyNanos{Currency: MustParseCurrency("cad"), Nanos: completedReceipt.Lease.BuyerChargeNanos})
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
	if err != nil {
		t.Fatal(err)
	}
	foreign := httptest.NewRequest(http.MethodGet, "/v1/service-leases/"+lease.ID.String(), nil)
	foreign.Header.Set("Authorization", "Bearer "+otherKey)
	foreignRec := httptest.NewRecorder()
	handler.ServeHTTP(foreignRec, foreign)
	if foreignRec.Code != http.StatusNotFound {
		t.Fatalf("foreign buyer receipt status=%d", foreignRec.Code)
	}
}
