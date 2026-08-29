package main

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/google/uuid"
)

// Failing-before evidence (Step 7, 2026-08-09):
//
//	go test -run TestStep7FailingBeforeSelectionReasonClaimsLowestWhenRank2
//
// against pre-repair HEAD printed:
//
//	FAILING_BEFORE evidence: selected_rank=2 but SelectionReason still claims lowest cost:
//	"lowest verified-outcome cost (base supplier ask; failure and refund rates unmeasured below 5 samples); warmth HOT is tiebreak only"
//
// The permanent tripwire below replaces that probe: rank>1 must never claim
// lowest cost, and ValidateMarketDecision refuses a dishonest reason.

func TestRealtimeClearingSelectionReasonDoesNotClaimLowestWhenRankGreaterThanOne(t *testing.T) {
	inputs := buildRealtimeClearingRankingInputs(80_000_000, 300_000_000, 0, 0, 0, 0, "HOT")
	reason := realtimeClearingSelectionReason(inputs, 2, 3)
	if strings.Contains(reason, "lowest verified-outcome cost") {
		t.Fatalf("selected_rank=2 must not claim lowest cost: %q", reason)
	}
	if !strings.Contains(reason, "rank 2 of 3") {
		t.Fatalf("contended reason must name the admitted rank: %q", reason)
	}
	if !strings.Contains(reason, "lock-skipped") {
		t.Fatalf("contended reason must name lock-skipped peers: %q", reason)
	}
	// Rank 1 remains the honest lowest-cost claim.
	rank1 := realtimeClearingSelectionReason(inputs, 1, 3)
	if !strings.Contains(rank1, "lowest verified-outcome cost") {
		t.Fatalf("uncontended rank 1 should still name lowest cost: %q", rank1)
	}
}

func TestValidateMarketDecisionRefusesDishonestSelectionReason(t *testing.T) {
	md := fixturePushMarketDecision(t, 2 /*selectedRank*/, marketAvailabilitySkipLocked)
	md.PushOrderBook.SelectionReason = "lowest verified-outcome cost (base supplier ask); warmth HOT is tiebreak only"
	if err := ValidateMarketDecision(md); err == nil {
		t.Fatal("ValidateMarketDecision must refuse selection_reason that claims lowest cost when rank>1")
	}
}

func TestMarketDecisionPushProjectsLegacyReceiptLosslessly(t *testing.T) {
	md := fixturePushMarketDecision(t, 1, marketAvailabilityBlockingForUpdate)
	receipt, err := projectRealtimeMarketClearingReceipt(md)
	must(t, err)
	book := md.PushOrderBook
	if receipt.Version != 3 ||
		receipt.CandidateCount != book.CandidateCount ||
		receipt.SelectedRank != book.SelectedRank ||
		receipt.SelectedWorkerID != book.SelectedWorkerID ||
		receipt.SelectedSupplierID != book.SelectedSupplierID ||
		receipt.SelectedSupplierInputNanos != book.SelectedSupplierInputNanos ||
		receipt.SelectedSupplierOutputNanos != book.SelectedSupplierOutputNanos ||
		receipt.BuyerCeilingNanos != book.BuyerCeilingNanos ||
		receipt.AcceptedCeilingNanos != book.AcceptedCeilingNanos ||
		receipt.PricingDecisionSHA256 != book.PricingDecisionSHA256 ||
		receipt.PositiveContributionNanos != book.PositiveContributionNanos ||
		receipt.OrderBookPolicy != book.OrderBookPolicy ||
		receipt.SelectionReason != book.SelectionReason ||
		receipt.ReferenceCurrency != book.ReferenceCurrency ||
		receipt.SettlementCurrency != book.SettlementCurrency ||
		receipt.SupplierRateCurrency != book.SupplierRateCurrency ||
		receipt.BuyerMoneyCurrency != book.BuyerMoneyCurrency ||
		receipt.RankingInputs == nil ||
		receipt.RankingInputs.VerifiedOutcomeCostNanos != book.RankingInputs.VerifiedOutcomeCostNanos {
		t.Fatalf("legacy receipt projection is lossy:\n  receipt=%+v\n  book=%+v", receipt, book)
	}
}

func TestMarketDecisionRealtimeLaneRefusesPullBody(t *testing.T) {
	if err := refuseRealtimePullMarketDecision(); err == nil {
		t.Fatal("realtime lane must refuse PULL_ELIGIBILITY_SNAPSHOT")
	}
	// A coherent pull snapshot may validate as a type for a later batch step,
	// but it must never project onto the realtime receipt.
	md := MarketDecision{
		Version:     marketDecisionVersion,
		MarketShape: marketShapePullEligibilitySnapshot,
		PullEligibilitySnapshot: &MarketPullEligibilitySnapshot{
			ClaimingWorkerID: uuid.New(),
		},
	}
	if _, err := projectRealtimeMarketClearingReceipt(md); err == nil {
		t.Fatal("PULL body must not project to a realtime market-clearing receipt")
	}
	// Dual-body and missing-body push are invalid.
	if err := ValidateMarketDecision(MarketDecision{
		Version:                 marketDecisionVersion,
		MarketShape:             marketShapePushOrderBook,
		PushOrderBook:           &MarketPushOrderBook{},
		PullEligibilitySnapshot: &MarketPullEligibilitySnapshot{ClaimingWorkerID: uuid.New()},
	}); err == nil {
		t.Fatal("must refuse push shape that also carries a pull body")
	}
	if err := ValidateMarketDecision(MarketDecision{
		Version:     marketDecisionVersion,
		MarketShape: marketShapePushOrderBook,
	}); err == nil {
		t.Fatal("must refuse push shape without push body")
	}
}

func TestNewRealtimePushMarketDecisionRecordsLockSkippedPeers(t *testing.T) {
	worker1, supplier1 := uuid.New(), uuid.New()
	worker2, supplier2 := uuid.New(), uuid.New()
	// Rank 1 locked, rank 2 admitted under SKIP LOCKED.
	bookJSON, err := json.Marshal([]marketBookCandidateJSON{
		{Rank: 1, WorkerID: worker1.String(), SupplierID: supplier1.String(), Warmth: "HOT", VerifiedOutcomeCost: 0.25},
		{Rank: 2, WorkerID: worker2.String(), SupplierID: supplier2.String(), Warmth: "WARM", VerifiedOutcomeCost: 0.38},
	})
	must(t, err)
	pricing, pricingSHA := fixtureRealtimePricingForMarketDecision(t, 0.08, 0.30)
	inputs := buildRealtimeClearingRankingInputs(
		mustNanoRate(t, 0.08), mustNanoRate(t, 0.30), 0, 0, 0, 0, "WARM")
	md, err := newRealtimePushMarketDecision(
		marketAvailabilitySkipLocked,
		2, 2, worker2, supplier2, 0.08, 0.30, pricing, pricingSHA, inputs, bookJSON)
	must(t, err)
	if md.MarketShape != marketShapePushOrderBook || md.PushOrderBook == nil {
		t.Fatalf("expected PUSH_ORDER_BOOK: %+v", md)
	}
	book := md.PushOrderBook
	if book.SelectedRank != 2 || book.AvailabilityMode != marketAvailabilitySkipLocked {
		t.Fatalf("selected/mode: %+v", book)
	}
	if len(book.Considered) != 2 {
		t.Fatalf("considered: %+v", book.Considered)
	}
	if book.Considered[0].ExclusionReason != marketExclusionLockSkipped ||
		book.Considered[0].WorkerID != worker1 {
		t.Fatalf("rank-1 peer must be lock-skipped: %+v", book.Considered[0])
	}
	if book.Considered[1].ExclusionReason != "" || book.Considered[1].WorkerID != worker2 {
		t.Fatalf("rank-2 selected must carry no exclusion: %+v", book.Considered[1])
	}
	if strings.Contains(book.SelectionReason, "lowest verified-outcome cost") {
		t.Fatalf("SelectionReason dishonest under contention: %q", book.SelectionReason)
	}
	receipt, err := projectRealtimeMarketClearingReceipt(md)
	must(t, err)
	if receipt.SelectedRank != 2 || receipt.SelectionReason != book.SelectionReason {
		t.Fatalf("projection lost contention truth: %+v", receipt)
	}
}

func fixturePushMarketDecision(t *testing.T, selectedRank int, mode string) MarketDecision {
	t.Helper()
	worker1, supplier1 := uuid.New(), uuid.New()
	worker2, supplier2 := uuid.New(), uuid.New()
	selectedWorker, selectedSupplier := worker1, supplier1
	input, output := 0.05, 0.20
	if selectedRank == 2 {
		selectedWorker, selectedSupplier = worker2, supplier2
		input, output = 0.08, 0.30
	}
	rows := []marketBookCandidateJSON{
		{Rank: 1, WorkerID: worker1.String(), SupplierID: supplier1.String(), Warmth: "HOT", VerifiedOutcomeCost: 0.25},
		{Rank: 2, WorkerID: worker2.String(), SupplierID: supplier2.String(), Warmth: "WARM", VerifiedOutcomeCost: 0.38},
	}
	if selectedRank == 1 {
		// Single-offer blocking book when rank is 1 with count 1 is also valid;
		// use a two-offer uncontended win for projection coverage.
		mode = marketAvailabilitySkipLocked
	}
	bookJSON, err := json.Marshal(rows)
	must(t, err)
	pricing, pricingSHA := fixtureRealtimePricingForMarketDecision(t, input, output)
	inputs := buildRealtimeClearingRankingInputs(
		mustNanoRate(t, input), mustNanoRate(t, output), 0, 0, 0, 0,
		rows[selectedRank-1].Warmth)
	md, err := newRealtimePushMarketDecision(
		mode, 2, selectedRank, selectedWorker, selectedSupplier, input, output,
		pricing, pricingSHA, inputs, bookJSON)
	must(t, err)
	return md
}

func mustNanoRate(t *testing.T, rate float64) int64 {
	t.Helper()
	n, err := nanoRatePerMillionFromFloat(rate)
	must(t, err)
	return int64(n)
}

// fixtureRealtimePricingForMarketDecision builds a minimal realtime PricingDecision
// with positive contribution so MarketDecision validation can bind money fields.
func fixtureRealtimePricingForMarketDecision(t *testing.T, supplierInput, supplierOutput float64) (PricingDecision, string) {
	t.Helper()
	installSettlementCurrencyForTest(t, "usd")
	profile := sortedVLLMProfiles()[0]
	placement, err := newRealtimePlacementPlan(profile, RealtimeOfferRegistration{
		RuntimeProfileID: profile.RuntimeProfileID, RuntimeProfileSHA256: profile.ProfileSHA256,
		HWClass: "nvidia_24gb", GPUCount: 1, MemoryGBPerGPU: 24,
		UpstreamBaseURL: "http://127.0.0.1:8811/v1", UpstreamToken: "cx_vllm_market_decision_test_token_12345",
		Warmth: "HOT", MaxActiveSequences: 1, AvailableSequences: 1,
		SupplierInputUSDPerMillionTokens: supplierInput, SupplierOutputUSDPerMillionTokens: supplierOutput,
	})
	must(t, err)
	currency, err := SettlementCurrency()
	must(t, err)
	pricing, err := newRealtimePricingDecision(RealtimePricingInputs{
		Profile: profile, Placement: placement,
		InputCommitment: strings.Repeat("a", 64), RequestSHA256: strings.Repeat("b", 64),
		MaximumPromptTokens: 8_330, MaximumCompletionTokens: 1,
		EstimatedPromptTokens: 4_163, EstimatedCompletionTokens: 1,
		SupplierInputRate: supplierInput, SupplierOutputRate: supplierOutput,
		BuyerDeclaredCeiling: 0.0011, Currency: currency,
	})
	must(t, err)
	sha, err := pricingDecisionDigest(pricing)
	must(t, err)
	return pricing, sha
}
