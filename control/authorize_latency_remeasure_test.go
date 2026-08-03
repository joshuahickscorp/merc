package main

import (
	"context"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
)

// Opt-in latency cell for the deadlock fix remeasure.
func TestAuthorizeLatencyRemeasureAfterHierarchy(t *testing.T) {
	if os.Getenv("MERC_AUTH_LATENCY_REMEASURE") != "1" {
		t.Skip("set MERC_AUTH_LATENCY_REMEASURE=1")
	}
	installSettlementCurrencyForTest(t, "usd")
	ctx, store, pool := openIsolatedTestStore(t)
	t.Setenv("MERC_TOKEN_KEY", "auth-latency-remeasure-key-with-32-bytes-min!")
	profile := sortedVLLMProfiles()[0]
	buyerID, err := store.CreateBuyerAccount(ctx,
		"auth-rem-"+uuid.NewString()+"@example.test", "integration-password", 10_000)
	if err != nil {
		t.Fatal(err)
	}
	worker := newRealtimeClearingOffer(t, ctx, store, pool, profile, "HOT", 0.08, 0.30, 10_000)
	refresh := time.NewTicker(5 * time.Second)
	t.Cleanup(refresh.Stop)
	go func() {
		for range refresh.C {
			c, cancel := context.WithTimeout(context.Background(), 2*time.Second)
			_, _ = pool.Exec(c, `
				UPDATE realtime_worker_offers
				   SET last_seen_at=now(), available_sequences=max_active_sequences, status='ACTIVE'
				 WHERE worker_id=$1 AND runtime_profile_id=$2`,
				worker.WorkerID, profile.RuntimeProfileID)
			cancel()
		}
	}()

	// Warm once.
	c0, _, err := store.AuthorizeRealtimeContract(ctx, RealtimeContractAuthorization{
		RequestID: "warm-" + uuid.NewString(), BuyerID: buyerID, Profile: profile,
		InputCommitment: strings.Repeat("a", 64), RequestSHA256: strings.Repeat("b", 64),
		MaximumPriceUSD: 0.001, EstimatedPriceUSD: 0.0005, DeadlineAt: time.Now().Add(time.Minute),
		MaximumPromptTokens: 8_330, MaximumCompletionTokens: 1,
		EstimatedPromptTokens: 4_163, EstimatedCompletionTokens: 1, BuyerDeclaredCeilingUSD: 0.0011,
	})
	if err != nil {
		t.Fatal(err)
	}
	_, _ = store.FinalizeRealtimeFailure(ctx, c0.ID, uuid.New(), 500, 1, "warm", "warm", false)

	for _, c := range []int{1, 8, 32} {
		if _, err := pool.Exec(ctx, `
			UPDATE realtime_worker_offers
			   SET available_sequences=max_active_sequences, status='ACTIVE', last_seen_at=now()
			 WHERE worker_id=$1 AND runtime_profile_id=$2`,
			worker.WorkerID, profile.RuntimeProfileID); err != nil {
			t.Fatal(err)
		}
		samples := 40
		ms := measureConcurrent(t, c, samples, func() time.Duration {
			start := time.Now()
			contract, _, err := store.AuthorizeRealtimeContract(context.Background(), RealtimeContractAuthorization{
				RequestID: "rem-" + uuid.NewString(), BuyerID: buyerID, Profile: profile,
				InputCommitment: strings.Repeat("a", 64), RequestSHA256: strings.Repeat("b", 64),
				MaximumPriceUSD: 0.001, EstimatedPriceUSD: 0.0005, DeadlineAt: time.Now().Add(time.Minute),
				MaximumPromptTokens: 8_330, MaximumCompletionTokens: 1,
				EstimatedPromptTokens: 4_163, EstimatedCompletionTokens: 1, BuyerDeclaredCeilingUSD: 0.0011,
			})
			elapsed := time.Since(start)
			if err != nil {
				t.Errorf("c=%d authorize: %v", c, err)
				return elapsed
			}
			_, _ = store.FinalizeRealtimeFailure(context.Background(), contract.ID, uuid.New(), 500, 1,
				"remeasure", "teardown", false)
			return elapsed
		})
		sum := summarizeLatency(ms)
		t.Logf("HIERARCHY_FIXED c=%d n=%d p50=%.3fms p95=%.3fms min=%.3fms max=%.3fms",
			c, sum.N, sum.P50, sum.P95, sum.Min, sum.Max)
		fmt.Printf("HIERARCHY_FIXED c=%d p50_ms=%.3f p95_ms=%.3f n=%d\n", c, sum.P50, sum.P95, sum.N)
	}
}
