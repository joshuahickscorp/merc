package main

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"
)

// Tests below pin money invariants for AuthorizeRealtimeContract under the
// documented lock hierarchy: evaluateRealtimeBuyerFunding (buyer advisory +
// buyers FOR UPDATE) before offer capacity claim, then EXECUTING insert under
// both. The late-funding reorder (offer before buyer) was reverted after it
// deadlocked with settlement; these tests still guard the money properties
// that must hold either way — capacity restored on funding failure, free-
// credit ceiling under concurrency, and idempotent replay.

// TestAuthorizeLateLockFundingFailureRestoresCapacity is the capacity half of
// the funding contract: a buyer who fails the funding check must not permanently
// consume a sequence. With funding before offer claim this is vacuously true
// (no claim); if funding is ever moved after the claim again, this still guards
// rollback.
func TestAuthorizeLateLockFundingFailureRestoresCapacity(t *testing.T) {
	installSettlementCurrencyForTest(t, "usd")
	ctx, store, pool := openIsolatedTestStore(t)
	profile, _, workerID := realtimeFundingFixture(t, ctx, store, pool)

	// Zero free credit, no prepaid: every authorize must fail funding.
	buyerID := uuid.New()
	if _, err := pool.Exec(ctx, `INSERT INTO buyers (id,email,free_credit_usd) VALUES ($1,$2,0)`,
		buyerID, buyerID.String()+"@late-lock-broke.invalid"); err != nil {
		t.Fatal(err)
	}

	var before int
	if err := pool.QueryRow(ctx, `
		SELECT available_sequences FROM realtime_worker_offers
		 WHERE worker_id=$1 AND runtime_profile_id=$2`,
		workerID, profile.RuntimeProfileID).Scan(&before); err != nil {
		t.Fatal(err)
	}
	if before < 1 {
		t.Fatalf("fixture offer has no capacity: %d", before)
	}

	maxUSD, estUSD, maxPrompt, maxCompletion := realtimeAuthCeiling(t, profile, 7, 2)
	for i := 0; i < 5; i++ {
		_, _, err := store.AuthorizeRealtimeContract(ctx, RealtimeContractAuthorization{
			RequestID: fmt.Sprintf("broke-%d-%s", i, uuid.NewString()),
			BuyerID:   buyerID, Profile: profile,
			InputCommitment: strings.Repeat("a", 64), RequestSHA256: strings.Repeat("b", 64),
			MaximumPriceUSD: maxUSD, EstimatedPriceUSD: estUSD, DeadlineAt: time.Now().Add(time.Minute),
			MaximumPromptTokens: maxPrompt, MaximumCompletionTokens: maxCompletion,
			EstimatedPromptTokens: 7, EstimatedCompletionTokens: 2,
		})
		if !errors.Is(err, errRealtimeInsufficientFunds) && !errors.Is(err, errRealtimeTopupRequired) {
			t.Fatalf("broke buyer authorize #%d: err=%v, want insufficient/topup", i, err)
		}
	}

	var after int
	if err := pool.QueryRow(ctx, `
		SELECT available_sequences FROM realtime_worker_offers
		 WHERE worker_id=$1 AND runtime_profile_id=$2`,
		workerID, profile.RuntimeProfileID).Scan(&after); err != nil {
		t.Fatal(err)
	}
	if after != before {
		t.Fatalf("funding failure leaked capacity: available_sequences before=%d after=%d", before, after)
	}
	var execCount int
	if err := pool.QueryRow(ctx, `
		SELECT count(*) FROM execution_contracts WHERE buyer_id=$1 AND state='EXECUTING'`,
		buyerID).Scan(&execCount); err != nil {
		t.Fatal(err)
	}
	if execCount != 0 {
		t.Fatalf("broke buyer left %d EXECUTING contracts", execCount)
	}
}

// TestAuthorizeLateLockConcurrentFreeCreditCeiling holds the buyer-ceiling
// invariant under the late lock order when funding is free credit (not
// prepaid). evaluateRealtimeBuyerFunding must still serialise check+reserve.
func TestAuthorizeLateLockConcurrentFreeCreditCeiling(t *testing.T) {
	installSettlementCurrencyForTest(t, "usd")
	ctx, store, pool := openIsolatedTestStore(t)
	profile, _, _ := realtimeFundingFixture(t, ctx, store, pool)

	maxUSD, estUSD, maxPrompt, maxCompletion := realtimeAuthCeiling(t, profile, 7, 2)
	// Grant free credit that covers exactly one ceiling (micro slack for rounding).
	grant := maxUSD + 0.000001
	buyerID, err := store.CreateBuyerAccount(ctx,
		"late-lock-fc-"+uuid.NewString()+"@example.test", "integration-password", grant)
	must(t, err)

	const n = 12
	var (
		wg        sync.WaitGroup
		start     = make(chan struct{})
		okCount   atomic.Int64
		failCount atomic.Int64
	)
	wg.Add(n)
	for i := 0; i < n; i++ {
		go func(i int) {
			defer wg.Done()
			<-start
			_, _, err := store.AuthorizeRealtimeContract(context.Background(), RealtimeContractAuthorization{
				RequestID: fmt.Sprintf("fc-concurrent-%d-%s", i, uuid.NewString()),
				BuyerID:   buyerID, Profile: profile,
				InputCommitment: strings.Repeat(fmt.Sprintf("%x", i%16), 64)[:64],
				RequestSHA256:   strings.Repeat(fmt.Sprintf("%x", (i+3)%16), 64)[:64],
				MaximumPriceUSD: maxUSD, EstimatedPriceUSD: estUSD, DeadlineAt: time.Now().Add(time.Minute),
				MaximumPromptTokens: maxPrompt, MaximumCompletionTokens: maxCompletion,
				EstimatedPromptTokens: 7, EstimatedCompletionTokens: 2,
			})
			if err == nil {
				okCount.Add(1)
				return
			}
			if errors.Is(err, errRealtimeInsufficientFunds) || errors.Is(err, errRealtimeTopupRequired) ||
				errors.Is(err, errRealtimeNoSupply) {
				failCount.Add(1)
				return
			}
			t.Errorf("unexpected auth error: %v", err)
			failCount.Add(1)
		}(i)
	}
	close(start)
	wg.Wait()
	if okCount.Load() != 1 {
		t.Fatalf("free-credit concurrent auth succeeded=%d fail=%d, want exactly 1 success",
			okCount.Load(), failCount.Load())
	}
	var reserved float64
	if err := pool.QueryRow(ctx, `
		SELECT COALESCE(sum(maximum_price_usd),0)::float8
		  FROM execution_contracts WHERE buyer_id=$1 AND state='EXECUTING'`, buyerID).Scan(&reserved); err != nil {
		t.Fatal(err)
	}
	if reserved > maxUSD+1e-9 {
		t.Fatalf("reserved EXECUTING ceilings %f exceed free-credit single max %f", reserved, maxUSD)
	}
}

// TestAuthorizeLateLockIdempotentReplayDoesNotDoubleReserve covers the
// idempotency invariant under the reordered path: a replay with the same key
// returns the original contract and must not open a second EXECUTING row or
// consume a second sequence.
func TestAuthorizeLateLockIdempotentReplayDoesNotDoubleReserve(t *testing.T) {
	installSettlementCurrencyForTest(t, "usd")
	ctx, store, pool := openIsolatedTestStore(t)
	profile, _, workerID := realtimeFundingFixture(t, ctx, store, pool)

	buyerID, err := store.CreateBuyerAccount(ctx,
		"late-lock-idem-"+uuid.NewString()+"@example.test", "integration-password", 100)
	must(t, err)
	maxUSD, estUSD, maxPrompt, maxCompletion := realtimeAuthCeiling(t, profile, 7, 2)
	var before int
	if err := pool.QueryRow(ctx, `
		SELECT available_sequences FROM realtime_worker_offers
		 WHERE worker_id=$1 AND runtime_profile_id=$2`,
		workerID, profile.RuntimeProfileID).Scan(&before); err != nil {
		t.Fatal(err)
	}

	auth := RealtimeContractAuthorization{
		RequestID: "idem-req-" + uuid.NewString(), BuyerID: buyerID, Profile: profile,
		InputCommitment: strings.Repeat("c", 64), RequestSHA256: strings.Repeat("d", 64),
		MaximumPriceUSD: maxUSD, EstimatedPriceUSD: estUSD, DeadlineAt: time.Now().Add(time.Minute),
		MaximumPromptTokens: maxPrompt, MaximumCompletionTokens: maxCompletion,
		EstimatedPromptTokens: 7, EstimatedCompletionTokens: 2,
		IdempotencyKey: "late-lock-idem-" + uuid.NewString(),
	}
	first, replay1, err := store.AuthorizeRealtimeContract(ctx, auth)
	if err != nil || replay1 {
		t.Fatalf("first authorize: contract err=%v replay=%v", err, replay1)
	}
	second, replay2, err := store.AuthorizeRealtimeContract(ctx, auth)
	if err != nil || !replay2 {
		t.Fatalf("replay authorize: err=%v replay=%v, want replay", err, replay2)
	}
	if first.ID != second.ID {
		t.Fatalf("replay returned different contract %s vs %s", first.ID, second.ID)
	}

	var execCount int
	if err := pool.QueryRow(ctx, `
		SELECT count(*) FROM execution_contracts
		 WHERE buyer_id=$1 AND state='EXECUTING'`, buyerID).Scan(&execCount); err != nil {
		t.Fatal(err)
	}
	if execCount != 1 {
		t.Fatalf("idempotent replay left %d EXECUTING rows, want 1", execCount)
	}
	var after int
	if err := pool.QueryRow(ctx, `
		SELECT available_sequences FROM realtime_worker_offers
		 WHERE worker_id=$1 AND runtime_profile_id=$2`,
		workerID, profile.RuntimeProfileID).Scan(&after); err != nil {
		t.Fatal(err)
	}
	if after != before-1 {
		t.Fatalf("idempotent replay capacity delta: before=%d after=%d, want before-1", before, after)
	}
}
