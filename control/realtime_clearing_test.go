package main

import (
	"context"
	"math"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
)

func TestRealtimeVerifiedOutcomeCostUsesOnlyMeasuredTerms(t *testing.T) {
	// Base ask only when history is thin: no invented failure or refund weight.
	cost, failRate, refundRate, retry, refund := realtimeVerifiedOutcomeCostNanos(
		100, 200, 0, 0, 0, 0)
	if cost != 300 || failRate != nil || refundRate != nil || retry || refund {
		t.Fatalf("unmeasured history must leave base ask alone: cost=%d fail=%v refund=%v retry=%v risk=%v",
			cost, failRate, refundRate, retry, refund)
	}

	// Below the sample floor the rate is noise, not a measurement.
	cost, failRate, _, retry, _ = realtimeVerifiedOutcomeCostNanos(100, 200, 4, 2, 0, 0)
	if cost != 300 || failRate != nil || retry {
		t.Fatalf("under-sampled failure must not adjust cost: cost=%d fail=%v retry=%v", cost, failRate, retry)
	}

	// 50% failure doubles expected attempts: the measured "double execution"
	// term. A cheaper base ask that fails half the time costs the same as a
	// reliable ask at 2x — and more when failure is higher.
	cost, failRate, _, retry, _ = realtimeVerifiedOutcomeCostNanos(100, 100, 10, 5, 0, 0)
	if !retry || failRate == nil || math.Abs(*failRate-0.5) > 1e-9 {
		t.Fatalf("failure rate not recorded: rate=%v retry=%v", failRate, retry)
	}
	// ceil(200 * 10 / 5) = 400
	if cost != 400 {
		t.Fatalf("50%% failure should double cost: got %d", cost)
	}

	// Full failure refuses an honest verified-outcome cost (ranks last).
	cost, _, _, retry, _ = realtimeVerifiedOutcomeCostNanos(100, 100, 10, 10, 0, 0)
	if !retry || cost < math.MaxInt64/8 {
		t.Fatalf("fully failing supplier must rank last, got cost=%d retry=%v", cost, retry)
	}

	// Refund risk multiplies when measured.
	cost, _, refundRate, _, refund = realtimeVerifiedOutcomeCostNanos(100, 100, 0, 0, 10, 2)
	if !refund || refundRate == nil || math.Abs(*refundRate-0.2) > 1e-9 {
		t.Fatalf("refund rate not recorded: rate=%v applied=%v", refundRate, refund)
	}
	// ceil(200 * 10 / 8) = 250
	if cost != 250 {
		t.Fatalf("20%% refund risk: want 250 got %d", cost)
	}
}

func TestRealtimeOfferBeatsPutsCostAboveWarmth(t *testing.T) {
	// Self-declared HOT must not beat a materially cheaper verified-outcome cost.
	if !realtimeOfferBeats(100, 200, "COLD", "HOT", 1, 1, "a", "b") {
		t.Fatal("cheaper COLD must clear above expensive HOT")
	}
	if realtimeOfferBeats(200, 100, "HOT", "COLD", 1, 1, "a", "b") {
		t.Fatal("expensive HOT must not clear above cheaper COLD")
	}
	// Within the same cost class, warmth still breaks the tie.
	if !realtimeOfferBeats(100, 100, "HOT", "WARM", 1, 1, "a", "b") {
		t.Fatal("within a cost class HOT must still beat WARM")
	}
	if realtimeOfferBeats(100, 100, "WARM", "HOT", 1, 1, "a", "b") {
		t.Fatal("within a cost class WARM must not beat HOT")
	}
}

func TestRealtimeClearingRankingInputsRecordEveryTerm(t *testing.T) {
	inputs := buildRealtimeClearingRankingInputs(80_000_000, 300_000_000, 10, 5, 10, 0, "HOT")
	if inputs.BaseAskNanos != 380_000_000 {
		t.Fatalf("base ask: %d", inputs.BaseAskNanos)
	}
	if inputs.VerifiedOutcomeCostNanos != 760_000_000 {
		t.Fatalf("verified cost: %d", inputs.VerifiedOutcomeCostNanos)
	}
	if !inputs.RetryCostApplied || inputs.ObservedFailureRate == nil {
		t.Fatalf("retry term missing: %+v", inputs)
	}
	if inputs.Warmth != "HOT" || inputs.WarmthRank != 0 {
		t.Fatalf("warmth tiebreak not recorded: %+v", inputs)
	}
	if len(inputs.OmittedTerms) == 0 {
		t.Fatal("omitted terms must be named so absences are not silent")
	}
	joined := strings.Join(inputs.OmittedTerms, ",")
	if !strings.Contains(joined, "verification_redundancy") || !strings.Contains(joined, "locality") {
		t.Fatalf("omitted terms incomplete: %v", inputs.OmittedTerms)
	}
}

// --- integration: live offer book -------------------------------------------

func resetRealtimeClearingState(t *testing.T, ctx context.Context, pool *pgxpool.Pool) {
	t.Helper()
	if _, err := pool.Exec(ctx, `TRUNCATE
		realtime_admission_events, realtime_offer_samples,
		realtime_authorization_events, realtime_settlements, realtime_executions,
		realtime_refunds, execution_contracts, realtime_worker_offers,
		realtime_supplier_outcome_stats
		RESTART IDENTITY CASCADE`); err != nil {
		t.Fatalf("reset realtime clearing state: %v", err)
	}
}

func newRealtimeClearingOffer(t *testing.T, ctx context.Context, store *Store, pool *pgxpool.Pool,
	profile VLLMRuntimeProfile, warmth string, input, output float64, sequences int,
) WorkerAuth {
	t.Helper()
	if sequences < 1 {
		sequences = 2
	}
	supplierID, workerID := uuid.New(), uuid.New()
	if _, err := pool.Exec(ctx, `INSERT INTO suppliers (id,email,status) VALUES ($1,$2,'active')`,
		supplierID, "clearing-supplier-"+uuid.NewString()+"@example.test"); err != nil {
		t.Fatal(err)
	}
	if _, err := store.CreateWorkerToken(ctx, workerID, supplierID); err != nil {
		t.Fatal(err)
	}
	worker := WorkerAuth{WorkerID: workerID, SupplierID: supplierID}
	if err := store.UpsertRealtimeOffer(ctx, worker, RealtimeOfferRegistration{
		RuntimeProfileID: profile.RuntimeProfileID, RuntimeProfileSHA256: profile.ProfileSHA256,
		HWClass: "nvidia_24gb", GPUCount: 1, MemoryGBPerGPU: 24,
		UpstreamBaseURL: "http://127.0.0.1:8811/v1", UpstreamToken: "cx_vllm_clearing_test_token_12345678",
		Warmth: warmth, MaxActiveSequences: sequences, AvailableSequences: sequences,
		SupplierInputUSDPerMillionTokens: input, SupplierOutputUSDPerMillionTokens: output,
	}); err != nil {
		t.Fatal(err)
	}
	// Scope cleanup to this fixture so a shared package DB cannot keep the
	// offer book (or orphan workers) alive for later tests.
	t.Cleanup(func() {
		c, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cancel()
		_, _ = pool.Exec(c, `DELETE FROM realtime_offer_samples WHERE worker_id=$1`, workerID)
		_, _ = pool.Exec(c, `DELETE FROM realtime_worker_offers WHERE worker_id=$1`, workerID)
		_, _ = pool.Exec(c, `DELETE FROM worker_tokens WHERE worker_id=$1`, workerID)
		_, _ = pool.Exec(c, `DELETE FROM workers WHERE id=$1`, workerID)
		_, _ = pool.Exec(c, `DELETE FROM suppliers WHERE id=$1`, supplierID)
	})
	return worker
}

// seedSupplierFailureRate authorizes and fails `fails` contracts against the
// only bookable offer (worker), then authorizes and succeeds `verified` more
// so the clearing CTE sees a known failure rate. Other competing offers must
// be absent or exhausted.
func seedSupplierFailureRate(t *testing.T, ctx context.Context, store *Store, pool *pgxpool.Pool,
	buyerID uuid.UUID, worker WorkerAuth, profile VLLMRuntimeProfile,
	verified, failed int,
) {
	t.Helper()
	// Temporarily remove every other active offer so seeds land on worker.
	// Capture who we drained so cleanup only restores those rows — never a
	// blanket reactivation of every offer on the shared profile (that left
	// foreign fixtures ACTIVE for later tests).
	drained, err := pool.Query(ctx, `
		UPDATE realtime_worker_offers
		   SET status='DRAINING', available_sequences=0
		 WHERE NOT (worker_id=$1 AND runtime_profile_id=$2)
		   AND status='ACTIVE'
		 RETURNING worker_id`,
		worker.WorkerID, profile.RuntimeProfileID)
	if err != nil {
		t.Fatal(err)
	}
	var drainedIDs []uuid.UUID
	for drained.Next() {
		var id uuid.UUID
		if err := drained.Scan(&id); err != nil {
			drained.Close()
			t.Fatal(err)
		}
		drainedIDs = append(drainedIDs, id)
	}
	drained.Close()
	if err := drained.Err(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		c, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cancel()
		for _, id := range drainedIDs {
			_, _ = pool.Exec(c, `
				UPDATE realtime_worker_offers
				   SET status='ACTIVE', available_sequences=GREATEST(available_sequences,1), last_seen_at=now()
				 WHERE worker_id=$1 AND runtime_profile_id=$2`,
				id, profile.RuntimeProfileID)
		}
	})
	// Ensure the target offer has enough capacity for the seed loop.
	if _, err := pool.Exec(ctx, `
		UPDATE realtime_worker_offers
		   SET max_active_sequences=$3, available_sequences=$3, status='ACTIVE', last_seen_at=now()
		 WHERE worker_id=$1 AND runtime_profile_id=$2`,
		worker.WorkerID, profile.RuntimeProfileID, verified+failed+2); err != nil {
		t.Fatal(err)
	}

	for i := 0; i < failed; i++ {
		contract, _, err := store.AuthorizeRealtimeContract(ctx, RealtimeContractAuthorization{
			RequestID: "seed-fail-" + uuid.NewString(), BuyerID: buyerID, Profile: profile,
			InputCommitment: strings.Repeat("c", 64), RequestSHA256: strings.Repeat("d", 64),
			MaximumPriceUSD: 0.001, EstimatedPriceUSD: 0.0005, DeadlineAt: time.Now().Add(time.Minute),
			MaximumPromptTokens: 8_330, MaximumCompletionTokens: 1,
			EstimatedPromptTokens: 4_163, EstimatedCompletionTokens: 1, BuyerDeclaredCeilingUSD: 0.0011,
		})
		if err != nil {
			t.Fatalf("seed fail authorize: %v", err)
		}
		if contract.SupplierID != worker.SupplierID {
			t.Fatalf("seed fail cleared wrong supplier %s", contract.SupplierID)
		}
		ok, err := store.FinalizeRealtimeFailure(ctx, contract.ID, uuid.New(), 500, 1,
			"seed_failure", "seed terminal failure for clearing rank", false)
		if err != nil || !ok {
			t.Fatalf("seed finalize failure: ok=%v err=%v", ok, err)
		}
	}
	for i := 0; i < verified; i++ {
		contract, _, err := store.AuthorizeRealtimeContract(ctx, RealtimeContractAuthorization{
			RequestID: "seed-ok-" + uuid.NewString(), BuyerID: buyerID, Profile: profile,
			InputCommitment: strings.Repeat("e", 64), RequestSHA256: strings.Repeat("f", 64),
			MaximumPriceUSD: 0.001, EstimatedPriceUSD: 0.0005, DeadlineAt: time.Now().Add(time.Minute),
			MaximumPromptTokens: 8_330, MaximumCompletionTokens: 1,
			EstimatedPromptTokens: 4_163, EstimatedCompletionTokens: 1, BuyerDeclaredCeilingUSD: 0.0011,
		})
		if err != nil {
			t.Fatalf("seed ok authorize: %v", err)
		}
		if contract.SupplierID != worker.SupplierID {
			t.Fatalf("seed ok cleared wrong supplier %s", contract.SupplierID)
		}
		// A success finalize needs V0 evidence. Mark VERIFIED with a null
		// settlement path by updating state after a capacity-preserving failure
		// would be dishonest. Use FinalizeRealtimeSuccess with minimal evidence.
		evidence := RealtimeExecutionEvidence{
			ID: uuid.New(), HTTPStatus: 200, StreamEventCount: 1,
			StreamRootSHA256: strings.Repeat("1", 64), OutputCommitment: strings.Repeat("2", 64),
			PromptTokens: 1, CompletionTokens: 1, TotalTokens: 2,
			TimeToFirstEventMS: 1, DurationMS: 1,
		}
		if _, err := store.FinalizeRealtimeSuccess(ctx, contract.ID, evidence); err != nil {
			t.Fatalf("seed finalize success: %v", err)
		}
	}
	// Restore bookable capacity for the subject authorization.
	if _, err := pool.Exec(ctx, `
		UPDATE realtime_worker_offers
		   SET available_sequences=max_active_sequences, status='ACTIVE', last_seen_at=now()
		 WHERE worker_id=$1 AND runtime_profile_id=$2`,
		worker.WorkerID, profile.RuntimeProfileID); err != nil {
		t.Fatal(err)
	}
}

func authorizeClearingContract(t *testing.T, ctx context.Context, store *Store,
	buyerID uuid.UUID, profile VLLMRuntimeProfile,
) RealtimeContract {
	t.Helper()
	contract, _, err := store.AuthorizeRealtimeContract(ctx, RealtimeContractAuthorization{
		RequestID: "req-clearing-" + uuid.NewString(), BuyerID: buyerID, Profile: profile,
		InputCommitment: strings.Repeat("a", 64), RequestSHA256: strings.Repeat("b", 64),
		MaximumPriceUSD: 0.001, EstimatedPriceUSD: 0.0005, DeadlineAt: time.Now().Add(time.Minute),
		MaximumPromptTokens: 8_330, MaximumCompletionTokens: 1,
		EstimatedPromptTokens: 4_163, EstimatedCompletionTokens: 1, BuyerDeclaredCeilingUSD: 0.0011,
	})
	if err != nil {
		t.Fatal(err)
	}
	return contract
}

// Failing-before evidence lives in TestRealtimeMarketClearingReceiptBindsOfferBookAndPricing:
// under lowest_warmth_then_supplier_rate_v1 a HOT offer at 0.08/0.30 beat a WARM
// offer at 0.05/0.20. After this change the cheaper verified-outcome cost wins.

func TestRealtimeClearingSelfDeclaredHOTDoesNotBeatCheaperVerifiedCost(t *testing.T) {
	installSettlementCurrencyForTest(t, "usd")
	ctx, store, pool := openPayoutTestStore(t)
	t.Setenv("MERC_TOKEN_KEY", "clearing-hot-test-key-with-at-least-32-bytes!!")
	resetRealtimeClearingState(t, ctx, pool)

	buyerID, err := store.CreateBuyerAccount(ctx,
		"clearing-hot-"+uuid.NewString()+"@example.test", "integration-password", 5)
	if err != nil {
		t.Fatal(err)
	}
	profile := sortedVLLMProfiles()[0]
	// Expensive HOT vs cheap COLD: warmth-first would pick HOT; cost-first picks COLD.
	hot := newRealtimeClearingOffer(t, ctx, store, pool, profile, "HOT", 0.08, 0.30, 2)
	cold := newRealtimeClearingOffer(t, ctx, store, pool, profile, "COLD", 0.05, 0.20, 2)

	contract := authorizeClearingContract(t, ctx, store, buyerID, profile)
	if contract.WorkerID != cold.WorkerID || contract.SupplierID != cold.SupplierID {
		t.Fatalf("HOT outranked cheaper verified-outcome cost: chose worker=%s want cold=%s (hot=%s) market=%+v",
			contract.WorkerID, cold.WorkerID, hot.WorkerID, contract.MarketClearing)
	}
	market := contract.MarketClearing
	if market == nil || market.OrderBookPolicy != realtimeOrderBookPolicy {
		t.Fatalf("order book policy: %+v", market)
	}
	if market.RankingInputs == nil || market.RankingInputs.Warmth != "COLD" {
		t.Fatalf("ranking inputs must record the selected warmth as a tiebreak signal: %+v", market.RankingInputs)
	}
}

func TestRealtimeClearingHigherAskWithNoFailuresBeatsCheaperThatNeedsDoubleExecution(t *testing.T) {
	// "Double execution" is measured as a 50% terminal failure rate: expected
	// attempts per verified outcome is 2, so verified-outcome cost is 2× ask.
	// A reliable supplier at a higher ask clears when that product is lower.
	// (Realtime V0 has no per-supplier redundancy class; failure-driven retry
	// is the measured term that multiplies executions.)
	installSettlementCurrencyForTest(t, "usd")
	ctx, store, pool := openPayoutTestStore(t)
	t.Setenv("MERC_TOKEN_KEY", "clearing-retry-test-key-with-at-least-32-bytes!")
	resetRealtimeClearingState(t, ctx, pool)

	buyerID, err := store.CreateBuyerAccount(ctx,
		"clearing-retry-"+uuid.NewString()+"@example.test", "integration-password", 5)
	if err != nil {
		t.Fatal(err)
	}
	profile := sortedVLLMProfiles()[0]

	// Cheap base ask 0.05+0.20=0.25 with 50% failure → verified cost 0.50.
	// Reliable ask 0.08+0.30=0.38 → verified cost 0.38. Reliable clears.
	// Create reliable first but drain it while seeding the cheap offer's history.
	reliable := newRealtimeClearingOffer(t, ctx, store, pool, profile, "COLD", 0.08, 0.30, 4)
	cheapUnreliable := newRealtimeClearingOffer(t, ctx, store, pool, profile, "HOT", 0.05, 0.20, 16)

	// 5 verified + 5 failed (≥ minRealtimeOutcomeSamples) → exact 50% failure.
	seedSupplierFailureRate(t, ctx, store, pool, buyerID, cheapUnreliable, profile, 5, 5)

	// Re-activate both offers for the subject clearing.
	if _, err := pool.Exec(ctx, `
		UPDATE realtime_worker_offers
		   SET status='ACTIVE', available_sequences=max_active_sequences, last_seen_at=now()
		 WHERE runtime_profile_id=$1`, profile.RuntimeProfileID); err != nil {
		t.Fatal(err)
	}

	contract := authorizeClearingContract(t, ctx, store, buyerID, profile)
	if contract.WorkerID != reliable.WorkerID {
		t.Fatalf("cheap double-execution (50%% fail) outranked reliable higher ask: chose=%s want reliable=%s cheap=%s market=%+v",
			contract.WorkerID, reliable.WorkerID, cheapUnreliable.WorkerID, contract.MarketClearing)
	}
	inputs := contract.MarketClearing.RankingInputs
	if inputs == nil || inputs.VerifiedOutcomeCostNanos <= 0 {
		t.Fatalf("winner must freeze verified-outcome cost: %+v", inputs)
	}
	if inputs.RetryCostApplied {
		t.Fatalf("reliable winner should have no failure history: %+v", inputs)
	}
}

func TestRealtimeClearingReceiptRecordsEveryRankingInput(t *testing.T) {
	installSettlementCurrencyForTest(t, "usd")
	ctx, store, pool := openPayoutTestStore(t)
	t.Setenv("MERC_TOKEN_KEY", "clearing-receipt-test-key-with-at-least-32-bytes")
	resetRealtimeClearingState(t, ctx, pool)

	buyerID, err := store.CreateBuyerAccount(ctx,
		"clearing-receipt-"+uuid.NewString()+"@example.test", "integration-password", 5)
	if err != nil {
		t.Fatal(err)
	}
	profile := sortedVLLMProfiles()[0]
	worker := newRealtimeClearingOffer(t, ctx, store, pool, profile, "WARM", 0.08, 0.30, 16)
	seedSupplierFailureRate(t, ctx, store, pool, buyerID, worker, profile, 8, 2)

	contract := authorizeClearingContract(t, ctx, store, buyerID, profile)
	market := contract.MarketClearing
	if market == nil || market.RankingInputs == nil {
		t.Fatalf("missing ranking inputs: %+v", market)
	}
	in := market.RankingInputs
	if in.BaseAskNanos <= 0 || in.VerifiedOutcomeCostNanos <= 0 {
		t.Fatalf("base/verified cost missing: %+v", in)
	}
	if in.SelectedSupplierInputNanos != market.SelectedSupplierInputNanos ||
		in.SelectedSupplierOutputNanos != market.SelectedSupplierOutputNanos {
		t.Fatalf("ranking rates disagree with selected offer: inputs=%+v market=%+v", in, market)
	}
	if in.TerminalAttempts != 10 || in.TerminalFails != 2 || !in.RetryCostApplied || in.ObservedFailureRate == nil {
		t.Fatalf("failure measurement not frozen: %+v", in)
	}
	if in.Warmth != "WARM" || in.WarmthRank != 1 {
		t.Fatalf("warmth tiebreak not frozen: %+v", in)
	}
	if len(in.OmittedTerms) < 2 {
		t.Fatalf("omitted terms not frozen: %+v", in)
	}
	// Buyer receipt path must carry the same authority.
	receipt, err := store.RealtimeReceipt(ctx, buyerID, contract.ID)
	if err != nil {
		t.Fatal(err)
	}
	if receipt.MarketClearing == nil || receipt.MarketClearing.RankingInputs == nil ||
		receipt.MarketClearing.RankingInputs.VerifiedOutcomeCostNanos != in.VerifiedOutcomeCostNanos {
		t.Fatalf("buyer receipt lost ranking inputs: %+v", receipt.MarketClearing)
	}
}

func TestRealtimeClearingSameCostClassKeepsWarmthTiebreak(t *testing.T) {
	// Existing behaviour when all candidates share a cost class: warmth, then
	// available sequences, then recency. Two offers with no failure history and
	// equal rates → HOT still wins.
	installSettlementCurrencyForTest(t, "usd")
	ctx, store, pool := openPayoutTestStore(t)
	t.Setenv("MERC_TOKEN_KEY", "clearing-tie-test-key-with-at-least-32-bytes!!!")
	resetRealtimeClearingState(t, ctx, pool)

	buyerID, err := store.CreateBuyerAccount(ctx,
		"clearing-tie-"+uuid.NewString()+"@example.test", "integration-password", 5)
	if err != nil {
		t.Fatal(err)
	}
	profile := sortedVLLMProfiles()[0]
	hot := newRealtimeClearingOffer(t, ctx, store, pool, profile, "HOT", 0.08, 0.30, 2)
	_ = newRealtimeClearingOffer(t, ctx, store, pool, profile, "WARM", 0.08, 0.30, 2)

	contract := authorizeClearingContract(t, ctx, store, buyerID, profile)
	if contract.WorkerID != hot.WorkerID {
		t.Fatalf("same cost class should still prefer HOT: chose=%s want=%s market=%+v",
			contract.WorkerID, hot.WorkerID, contract.MarketClearing)
	}
	if contract.MarketClearing == nil || contract.MarketClearing.RankingInputs == nil ||
		contract.MarketClearing.RankingInputs.Warmth != "HOT" ||
		contract.MarketClearing.RankingInputs.RetryCostApplied {
		t.Fatalf("tiebreak receipt: %+v", contract.MarketClearing)
	}
}

func TestServiceLeaseClearingRanksByTotalSupplierPlusResidency(t *testing.T) {
	// Region stays a hard filter. Residency nanos are a measured cost component
	// of the lease price, so ranking by supplier ask alone would let a cheap
	// supplier with expensive residency clear above a higher supplier ask whose
	// total (supplier+residency) is lower. Total cost is the verified ranking.
	installSettlementCurrencyForTest(t, "cad")
	ctx, store, pool := openPayoutTestStore(t)
	profile := sortedVLLMProfiles()[0]
	buyerID := uuid.New()
	if _, err := pool.Exec(ctx, `INSERT INTO buyers (id,email) VALUES ($1,$2)`,
		buyerID, buyerID.String()+"@residency-rank.invalid"); err != nil {
		t.Fatal(err)
	}
	if err := store.SeedPrepaidBalance(ctx, buyerID, 2_000_000, "residency-rank-"+buyerID.String()); err != nil {
		t.Fatal(err)
	}
	cheapSupplier, _ := newFabricMeasurementWorker(t, ctx, store)
	cheaperTotal, _ := newFabricMeasurementWorker(t, ctx, store)
	// Available warm capacity is measured and fail-closed: without a fresh
	// worker_model_state row (rss_delta_bytes + load_ms, last_seen_warm within
	// 60s) UpsertServiceLeaseOffer advertises available=0 and clearing finds
	// no supply. Seed the same measurement a real worker heartbeat would.
	seedMeasuredWarmResidency(t, ctx, pool, cheapSupplier.WorkerID, profile.ModelAlias)
	seedMeasuredWarmResidency(t, ctx, pool, cheaperTotal.WorkerID, profile.ModelAlias)
	region := "ca-residency-rank-" + uuid.NewString()

	// supplier 1_000M + residency 2_000M = total 3_000M
	// supplier 1_500M + residency 200M  = total 1_700M  ← must win
	lowSupplierHighRes := serviceLeaseOffer(profile)
	lowSupplierHighRes.Region = region
	lowSupplierHighRes.SupplierNanosPerReplicaHour = 1_000_000_000
	lowSupplierHighRes.ResidencyNanosPerReplicaHour = 2_000_000_000

	higherSupplierLowRes := serviceLeaseOffer(profile)
	higherSupplierLowRes.Region = region
	higherSupplierLowRes.SupplierNanosPerReplicaHour = 1_500_000_000
	higherSupplierLowRes.ResidencyNanosPerReplicaHour = 200_000_000

	if err := store.UpsertServiceLeaseOffer(ctx, cheapSupplier, lowSupplierHighRes); err != nil {
		t.Fatal(err)
	}
	if err := store.UpsertServiceLeaseOffer(ctx, cheaperTotal, higherSupplierLowRes); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		c, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cancel()
		for _, w := range []WorkerAuth{cheapSupplier, cheaperTotal} {
			_, _ = pool.Exec(c, `DELETE FROM service_lease_offer_samples WHERE worker_id=$1`, w.WorkerID)
			_, _ = pool.Exec(c, `DELETE FROM service_lease_worker_offers WHERE worker_id=$1`, w.WorkerID)
			_, _ = pool.Exec(c, `DELETE FROM worker_model_state WHERE worker_id=$1`, w.WorkerID)
			_, _ = pool.Exec(c, `DELETE FROM worker_tokens WHERE worker_id=$1`, w.WorkerID)
			_, _ = pool.Exec(c, `DELETE FROM workers WHERE id=$1`, w.WorkerID)
			_, _ = pool.Exec(c, `DELETE FROM suppliers WHERE id=$1`, w.SupplierID)
		}
		_, _ = pool.Exec(c, `DELETE FROM service_leases WHERE buyer_id=$1`, buyerID)
		_, _ = pool.Exec(c, `DELETE FROM buyer_prepaid_balances WHERE buyer_id=$1`, buyerID)
		_, _ = pool.Exec(c, `DELETE FROM ledger_entries WHERE buyer_id=$1`, buyerID)
		_, _ = pool.Exec(c, `DELETE FROM buyers WHERE id=$1`, buyerID)
	})
	// Ceiling must admit the winner's total pricing path.
	request := ServiceLeaseRequest{
		RuntimeProfileID: profile.RuntimeProfileID, Region: region,
		MinimumReplicas: 1, MaximumReplicas: 1, TermSeconds: 60, MaximumP95LatencyMilliseconds: 500,
		BuyerDeclaredCeilingNanos: 500_000_000,
	}
	lease, err := store.CreateServiceLease(ctx, buyerID, request)
	if err != nil {
		t.Fatal(err)
	}
	if lease.WorkerID != cheaperTotal.WorkerID {
		t.Fatalf("residency not honoured in ranking: chose worker=%s (supplier-only cheap) want total-cheap=%s",
			lease.WorkerID, cheaperTotal.WorkerID)
	}
	receipt, err := store.GetServiceLeaseReceipt(ctx, buyerID, lease.ID)
	if err != nil {
		t.Fatal(err)
	}
	if receipt.MarketClearing == nil ||
		receipt.MarketClearing.OrderBookPolicy != "lowest_total_supplier_plus_residency_ask_v1" ||
		receipt.MarketClearing.SelectedResidencyRateNanos != higherSupplierLowRes.ResidencyNanosPerReplicaHour {
		t.Fatalf("residency ranking receipt: %+v", receipt.MarketClearing)
	}
}
