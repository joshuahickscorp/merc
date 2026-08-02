package main

import (
	"bytes"
	"encoding/json"
	"math"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
)

func TestRealtimeMarketLiquidityRetainsOfferAndCapacityEvidence(t *testing.T) {
	installSettlementCurrencyForTest(t, "cad")
	ctx, store, pool := openPayoutTestStore(t)
	t.Setenv("MERC_TOKEN_KEY", "liquidity-test-key-with-at-least-32-bytes")
	// This report is intentionally a current fleet snapshot. Reset only the
	// realtime lane so an earlier integration test's live offer cannot masquerade
	// as liquidity belonging to this fixture when the suite shares a database.
	if _, err := pool.Exec(ctx, `TRUNCATE
		realtime_admission_events, realtime_offer_samples,
		realtime_authorization_events, realtime_settlements, realtime_executions,
		realtime_refunds, execution_contracts, realtime_worker_offers
		RESTART IDENTITY CASCADE`); err != nil {
		t.Fatalf("reset realtime liquidity state: %v", err)
	}
	if _, err := pool.Exec(ctx, `DELETE FROM ledger_entries WHERE execution_contract_id IS NOT NULL`); err != nil {
		t.Fatalf("reset realtime liquidity ledger rows: %v", err)
	}

	buyerID, err := store.CreateBuyerAccount(ctx,
		"liquidity-"+uuid.NewString()+"@example.test", "integration-password", 5)
	if err != nil {
		t.Fatal(err)
	}
	supplierID, workerID := uuid.New(), uuid.New()
	if _, err := pool.Exec(ctx, `INSERT INTO suppliers (id,email,status) VALUES ($1,$2,'active')`,
		supplierID, "liquidity-supplier-"+uuid.NewString()+"@example.test"); err != nil {
		t.Fatal(err)
	}
	if _, err := store.CreateWorkerToken(ctx, workerID, supplierID); err != nil {
		t.Fatal(err)
	}
	profile := sortedVLLMProfiles()[0]
	worker := WorkerAuth{WorkerID: workerID, SupplierID: supplierID}
	if err := store.UpsertRealtimeOffer(ctx, worker, RealtimeOfferRegistration{
		RuntimeProfileID: profile.RuntimeProfileID, RuntimeProfileSHA256: profile.ProfileSHA256,
		HWClass: "nvidia_24gb", GPUCount: 1, MemoryGBPerGPU: 24,
		UpstreamBaseURL: "http://127.0.0.1:8811/v1", UpstreamToken: "cx_vllm_liquidity_test_token_123456",
		Warmth: "HOT", MaxActiveSequences: 2, AvailableSequences: 2,
		SupplierInputUSDPerMillionTokens: 0.08, SupplierOutputUSDPerMillionTokens: 0.30,
	}); err != nil {
		t.Fatal(err)
	}

	contract, _, err := store.AuthorizeRealtimeContract(ctx, RealtimeContractAuthorization{
		RequestID: "req-liquidity-" + uuid.NewString(), BuyerID: buyerID, Profile: profile,
		InputCommitment: strings.Repeat("a", 64), RequestSHA256: strings.Repeat("b", 64),
		MaximumPriceUSD: 0.001, EstimatedPriceUSD: 0.0005, DeadlineAt: time.Now().Add(time.Minute),
		MaximumPromptTokens: 8_330, MaximumCompletionTokens: 1,
		EstimatedPromptTokens: 4_163, EstimatedCompletionTokens: 1,
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := store.RecordRealtimeAdmissionEvent(ctx, buyerID, profile.RuntimeProfileID,
		contract.PlacementPlan.HWClass, realtimeAdmissionAdmitted, contract.ID); err != nil {
		t.Fatal(err)
	}
	if err := store.RecordRealtimeAdmissionEvent(ctx, buyerID, profile.RuntimeProfileID,
		"", realtimeAdmissionNoCapacity, uuid.Nil); err != nil {
		t.Fatal(err)
	}
	if err := store.RecordRealtimeAdmissionEvent(ctx, buyerID, profile.RuntimeProfileID,
		"", realtimeAdmissionInsufficient, uuid.Nil); err != nil {
		t.Fatal(err)
	}
	// A status transition is retained as evidence of offer churn. Finalizing the
	// contract then returns the advertised slot, so price depth below is current
	// rather than a historic rate from a draining offer.
	if err := store.HeartbeatRealtimeOffer(ctx, worker, RealtimeOfferHeartbeat{
		RuntimeProfileID: profile.RuntimeProfileID, Warmth: "WARM", AvailableSequences: 1, Status: "DRAINING",
	}); err != nil {
		t.Fatal(err)
	}
	if finalized, err := store.FinalizeRealtimeFailure(ctx, contract.ID, uuid.New(), 0, 1,
		"liquidity_test", "release test capacity", false); err != nil || !finalized {
		t.Fatalf("finalize test contract: finalized=%v err=%v", finalized, err)
	}
	if err := store.HeartbeatRealtimeOffer(ctx, worker, RealtimeOfferHeartbeat{
		RuntimeProfileID: profile.RuntimeProfileID, Warmth: "HOT", AvailableSequences: 2, Status: "ACTIVE",
	}); err != nil {
		t.Fatal(err)
	}

	receipt, err := store.RealtimeMarketLiquidity(ctx, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	if receipt.RegionScope != "UNSPECIFIED_NO_GOVERNED_REGION_AUTHORITY" {
		t.Fatalf("region scope=%q; report must not infer residency", receipt.RegionScope)
	}
	var hardware, unmatched *RealtimeMarketLiquiditySlice
	for i := range receipt.Slices {
		slice := &receipt.Slices[i]
		if slice.RuntimeProfileID != profile.RuntimeProfileID {
			continue
		}
		switch slice.HWClass {
		case "nvidia_24gb":
			hardware = slice
		case liquidityUnmatchedHardwareClass:
			unmatched = slice
		}
	}
	if hardware == nil {
		t.Fatalf("missing hardware slice in %+v", receipt.Slices)
	}
	if hardware.ModelAlias != profile.ModelAlias || hardware.Admitted != 1 ||
		hardware.OfferSamples < 3 || hardware.ActiveToInactiveWorkers != 1 ||
		hardware.CurrentActiveOffers != 1 {
		t.Fatalf("unexpected hardware liquidity slice: %+v", *hardware)
	}
	if math.Abs(hardware.SupplierInputRateP50-0.08) > 0.000001 ||
		math.Abs(hardware.SupplierOutputRateP90-0.30) > 0.000001 {
		t.Fatalf("current supplier rate depth was not retained: %+v", *hardware)
	}
	if unmatched == nil || unmatched.NoCapacity != 1 || unmatched.InsufficientFunds != 1 ||
		unmatched.CapacityFillRate == nil || *unmatched.CapacityFillRate != 0 {
		t.Fatalf("capacity/funding outcomes were not separated honestly: %+v", unmatched)
	}
}

func TestRealtimeMarketClearingReceiptBindsOfferBookAndPricing(t *testing.T) {
	installSettlementCurrencyForTest(t, "usd")
	ctx, store, pool := openPayoutTestStore(t)
	t.Setenv("MERC_TOKEN_KEY", "market-clearing-test-key-with-at-least-32-bytes")
	if _, err := pool.Exec(ctx, `TRUNCATE
		realtime_admission_events, realtime_offer_samples,
		realtime_authorization_events, realtime_settlements, realtime_executions,
		realtime_refunds, execution_contracts, realtime_worker_offers
		RESTART IDENTITY CASCADE`); err != nil {
		t.Fatalf("reset realtime market state: %v", err)
	}

	buyerID, err := store.CreateBuyerAccount(ctx,
		"market-clearing-"+uuid.NewString()+"@example.test", "integration-password", 5)
	if err != nil {
		t.Fatal(err)
	}
	profile := sortedVLLMProfiles()[0]
	newOffer := func(warmth string, input, output float64) WorkerAuth {
		supplierID, workerID := uuid.New(), uuid.New()
		if _, err := pool.Exec(ctx, `INSERT INTO suppliers (id,email,status) VALUES ($1,$2,'active')`,
			supplierID, "market-supplier-"+uuid.NewString()+"@example.test"); err != nil {
			t.Fatal(err)
		}
		if _, err := store.CreateWorkerToken(ctx, workerID, supplierID); err != nil {
			t.Fatal(err)
		}
		worker := WorkerAuth{WorkerID: workerID, SupplierID: supplierID}
		if err := store.UpsertRealtimeOffer(ctx, worker, RealtimeOfferRegistration{
			RuntimeProfileID: profile.RuntimeProfileID, RuntimeProfileSHA256: profile.ProfileSHA256,
			HWClass: "nvidia_24gb", GPUCount: 1, MemoryGBPerGPU: 24,
			UpstreamBaseURL: "http://127.0.0.1:8811/v1", UpstreamToken: "cx_vllm_market_clearing_test_token_123456",
			Warmth: warmth, MaxActiveSequences: 1, AvailableSequences: 1,
			SupplierInputUSDPerMillionTokens: input, SupplierOutputUSDPerMillionTokens: output,
		}); err != nil {
			t.Fatal(err)
		}
		return worker
	}
	// Failing-before: warmth-first ranking selected the HOT 0.08/0.30 offer over
	// the cheaper WARM 0.05/0.20 offer. Verified-outcome cost ranking selects
	// the cheaper ask; warmth is tiebreak only inside a cost class.
	hotWorker := newOffer("HOT", 0.08, 0.30)
	cheapWorker := newOffer("WARM", 0.05, 0.20)

	contract, _, err := store.AuthorizeRealtimeContract(ctx, RealtimeContractAuthorization{
		RequestID: "req-market-clearing-" + uuid.NewString(), BuyerID: buyerID, Profile: profile,
		InputCommitment: strings.Repeat("a", 64), RequestSHA256: strings.Repeat("b", 64),
		MaximumPriceUSD: 0.001, EstimatedPriceUSD: 0.0005, DeadlineAt: time.Now().Add(time.Minute),
		MaximumPromptTokens: 8_330, MaximumCompletionTokens: 1,
		EstimatedPromptTokens: 4_163, EstimatedCompletionTokens: 1, BuyerDeclaredCeilingUSD: 0.0011,
	})
	if err != nil {
		t.Fatal(err)
	}
	market := contract.MarketClearing
	if market == nil || market.Version != 1 || market.CandidateCount != 2 || market.SelectedRank != 1 ||
		market.SelectedWorkerID != cheapWorker.WorkerID || market.SelectedSupplierID != cheapWorker.SupplierID ||
		market.PricingDecisionSHA256 != contract.PricingDecisionSHA256 || market.BuyerCeilingNanos <= 0 ||
		market.PositiveContributionNanos <= 0 ||
		market.AcceptedCeilingNanos != contract.Pricing.FixedPoint.AcceptedCeilingNanos ||
		market.OrderBookPolicy != realtimeOrderBookPolicy ||
		market.RankingInputs == nil || market.RankingInputs.VerifiedOutcomeCostNanos <= 0 ||
		market.RankingInputs.Warmth != "WARM" || len(market.RankingInputs.OmittedTerms) == 0 {
		t.Fatalf("realtime market receipt did not bind live offer book under verified-outcome ranking: %+v contract=%+v hot=%+v",
			market, contract, hotWorker)
	}
	receipt, err := store.RealtimeReceipt(ctx, buyerID, contract.ID)
	if err != nil {
		t.Fatal(err)
	}
	if receipt.MarketClearing == nil || receipt.MarketClearing.SelectedWorkerID != cheapWorker.WorkerID ||
		receipt.MarketClearing.CandidateCount != 2 || receipt.MarketClearing.RankingInputs == nil {
		t.Fatalf("buyer receipt omitted market clearing authority: %+v", receipt.MarketClearing)
	}
	if _, err := pool.Exec(ctx, `UPDATE execution_contracts SET market_clearing='{}' WHERE id=$1`, contract.ID); err == nil {
		t.Fatal("database allowed frozen market-clearing evidence to mutate")
	}
}

func TestRealtimeAdmissionEventRejectsFabricatedPlacement(t *testing.T) {
	ctx, store, _ := openPayoutTestStore(t)
	err := store.RecordRealtimeAdmissionEvent(ctx, uuid.New(), "profile", "nvidia_24gb",
		realtimeAdmissionAdmitted, uuid.New())
	if err == nil || !strings.Contains(err.Error(), "contract") {
		t.Fatalf("fabricated admitted placement accepted: %v", err)
	}
}

func TestServiceLeaseMarketLiquidityUsesRealOfferAndBuyerAdmissionPaths(t *testing.T) {
	installSettlementCurrencyForTest(t, "cad")
	ctx, store, pool := openPayoutTestStore(t)
	buyerID := uuid.New()
	if _, err := pool.Exec(ctx, `INSERT INTO buyers (id,email) VALUES ($1,$2)`, buyerID, buyerID.String()+"@service-liquidity.invalid"); err != nil {
		t.Fatal(err)
	}
	if err := store.SeedPrepaidBalance(ctx, buyerID, 1_000_000, "service-liquidity-"+buyerID.String()); err != nil {
		t.Fatal(err)
	}
	_, buyerKey, _, err := store.CreateAPIKey(ctx, buyerID, "service-liquidity", true)
	if err != nil {
		t.Fatal(err)
	}
	worker, workerToken := newFabricMeasurementWorker(t, ctx, store)
	profile := sortedVLLMProfiles()[0]
	offer := serviceLeaseOffer(profile)
	offer.Region = "ca-liquidity-" + uuid.NewString()
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
	if got := post("/v1/worker/service-leases/offers", workerToken, offer).Code; got != http.StatusOK {
		t.Fatalf("service offer status=%d", got)
	}
	request := ServiceLeaseRequest{RuntimeProfileID: profile.RuntimeProfileID, Region: offer.Region,
		MinimumReplicas: 1, MaximumReplicas: 3, TermSeconds: 60, MaximumP95LatencyMilliseconds: 500,
		BuyerDeclaredCeilingNanos: 135_000_000}
	if created := post("/v1/service-leases", buyerKey, request); created.Code != http.StatusCreated {
		t.Fatalf("service admission status=%d body=%s", created.Code, created.Body.String())
	}
	if denied := post("/v1/service-leases", buyerKey, request); denied.Code != http.StatusServiceUnavailable {
		t.Fatalf("second service admission status=%d body=%s", denied.Code, denied.Body.String())
	}
	receipt, err := store.ServiceLeaseMarketLiquidity(ctx, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	if receipt.RegionScope != "SUPPLIER_DECLARED_OPERATIONAL_REGION_ONLY" {
		t.Fatalf("service liquidity promoted supplier region: %q", receipt.RegionScope)
	}
	var matched, unmatched *ServiceLeaseMarketLiquiditySlice
	for i := range receipt.Slices {
		slice := &receipt.Slices[i]
		if slice.RuntimeProfileID != profile.RuntimeProfileID || slice.Region != request.Region {
			continue
		}
		if slice.WorkerDeclaredHWClass == serviceLiquidityUnmatchedHardware {
			unmatched = slice
		} else {
			matched = slice
		}
	}
	if matched == nil || matched.ModelAlias != profile.ModelAlias || matched.Admitted != 1 ||
		matched.NoCapacity != 0 || matched.CapacityFillNumerator != 1 ||
		matched.CapacityFillDenominator != 1 || matched.OfferSamples < 2 ||
		matched.OccupiedReplicaSamples < 3 || matched.MaximumReplicaSamples < 6 {
		t.Fatalf("matched service liquidity did not retain real capacity movement: %+v", matched)
	}
	if unmatched == nil || unmatched.Admitted != 0 || unmatched.NoCapacity != 1 ||
		unmatched.CapacityFillNumerator != 0 || unmatched.CapacityFillDenominator != 1 {
		t.Fatalf("capacity refusal was not kept separate from matched supply: %+v", unmatched)
	}
	var sampleID uuid.UUID
	if err := pool.QueryRow(ctx, `SELECT id FROM service_lease_offer_samples
		WHERE worker_id=$1 ORDER BY observed_at,id LIMIT 1`, worker.WorkerID).Scan(&sampleID); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `UPDATE service_lease_offer_samples
		SET available_warm_replicas=available_warm_replicas WHERE id=$1`, sampleID); err == nil {
		t.Fatal("service liquidity evidence row was mutable")
	}
	if err := store.RecordServiceLeaseAdmissionEvent(ctx, buyerID, request, serviceLeaseAdmissionAdmitted, uuid.New()); err == nil {
		t.Fatal("fabricated admitted service lease entered the liquidity denominator")
	}
	if _, _, err := store.DeleteOldServiceLeaseLiquidityTelemetry(ctx, time.Now().Add(time.Second)); err != nil {
		t.Fatal(err)
	}
	pruned, err := store.ServiceLeaseMarketLiquidity(ctx, time.Hour)
	if err != nil || len(pruned.Slices) != 0 {
		t.Fatalf("service liquidity retention left stale evidence: slices=%+v err=%v", pruned.Slices, err)
	}
	_ = worker
}

func TestNetworkMarketLiquidityComposesBoundedLaneReceipts(t *testing.T) {
	installSettlementCurrencyForTest(t, "cad")
	ctx, store, _ := openPayoutTestStore(t)
	receipt, err := store.NetworkMarketLiquidity(ctx, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	if receipt.Version != 1 || receipt.Window != time.Hour.String() ||
		receipt.MarketScope != "MERC_RETAINED_REALTIME_AND_WARM_SERVICE_LANES_ONLY_NO_GLOBAL_OR_LEGAL_REGION_CLAIM" ||
		receipt.Realtime.Version != 1 || receipt.Services.Version != 1 ||
		receipt.WindowStart.IsZero() || receipt.WindowEnd.Before(receipt.WindowStart) {
		t.Fatalf("network liquidity receipt lost bounded lane authority: %+v", receipt)
	}
}
