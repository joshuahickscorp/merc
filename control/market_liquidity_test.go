package main

import (
	"math"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
)

func TestRealtimeMarketLiquidityRetainsOfferAndCapacityEvidence(t *testing.T) {
	installSettlementCurrencyForTest(t, "cad")
	ctx, store, pool := openPayoutTestStore(t)
	t.Setenv("MERC_TOKEN_KEY", "liquidity-test-key-with-at-least-32-bytes")

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

func TestRealtimeAdmissionEventRejectsFabricatedPlacement(t *testing.T) {
	ctx, store, _ := openPayoutTestStore(t)
	err := store.RecordRealtimeAdmissionEvent(ctx, uuid.New(), "profile", "nvidia_24gb",
		realtimeAdmissionAdmitted, uuid.New())
	if err == nil || !strings.Contains(err.Error(), "contract") {
		t.Fatalf("fabricated admitted placement accepted: %v", err)
	}
}
