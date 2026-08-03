package main

import (
	"context"
	"fmt"
	"math"
	"net/url"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
)

// TestAuthorizeTailCharacterize separates authorize / LookupAPIKey tails into
// pool-acquire wait vs lock contention (buyer funding vs offer row) vs query work.
//
// Opt-in:
//
//	MERC_AUTHORIZE_TAIL_PROBE=1 \
//	MERC_TEST_DATABASE_URL=postgres://cx:cx@localhost:5432/cx?sslmode=disable \
//	go test -count=1 -run TestAuthorizeTailCharacterize -timeout 45m ./control
//
// Writes evidence/perf/authorize-auth-tails-*.json (bound).
//
// Design of the factorial cells (each at c ∈ {1,8,32}):
//
//	pool_acquire          pure pgxpool.Acquire/Release under concurrency
//	lookup_cold           LookupAPIKey after cache reset (DB path)
//	lookup_warm           LookupAPIKey cache hit
//	auth_same_buyer_1off  AuthorizeRealtimeContract, 1 buyer, 1 offer
//	auth_multi_buyer_1off multi buyer, 1 offer  (offer-row serialisation alone)
//	auth_multi_buyer_Noff multi buyer, N=c equal-rank offers (rank-1 pile-on test)
//
// MaxConns is crossed at production default (20) and abundant (64) so pool
// starvation is separable from lock wait. Hierarchy (buyer funding before
// offer capacity) is not altered by this probe.
//
// Reading the 1-offer multi-buyer cell: it isolates "one capacity row is one
// capacity row" (docs/OFFER_MULTIPLICITY.md). That tail is a thin-book /
// fixture number, not a standing production defect — seed leaves 0 realtime
// offers, one local agent registers one, canary is two workers, and N-offer
// cells already show SKIP LOCKED recovering multi-supplier books. Do not
// re-architect available_sequences into slot rows because this cell is slow.
func TestAuthorizeTailCharacterize(t *testing.T) {
	if os.Getenv("MERC_AUTHORIZE_TAIL_PROBE") != "1" {
		t.Skip("set MERC_AUTHORIZE_TAIL_PROBE=1 to run authorize/auth tail characterization")
	}
	installSettlementCurrencyForTest(t, "usd")
	t.Setenv("MERC_TOKEN_KEY", "authorize-tail-probe-key-with-32-bytes-min!!")

	loadAvg, loadN := readLoadAverage()
	quiet := machineLoadQuiet()
	host, _ := os.Hostname()
	t.Logf("load before measure: avg=%v n=%d quiet=%v hostname=%s cpus=%d",
		loadAvg, loadN, quiet, host, runtime.NumCPU())

	ctx := context.Background()
	profile := sortedVLLMProfiles()[0]
	const (
		samplesPerCell = 80
		warmup         = 8
		offerCapacity  = 50_000
	)
	concurrencies := []int{1, 8, 32}
	maxConnsLevels := []int32{20, 64} // 20 = production defaultDBMaxConns

	type cell struct {
		MaxConns            int32                  `json:"max_conns"`
		Concurrency         int                    `json:"concurrency"`
		Samples             int                    `json:"samples"`
		PoolAcquireMs       segmentLatencySummary  `json:"pool_acquire_ms"`
		PoolStatDelta       map[string]any         `json:"pool_stat_delta"`
		LookupColdMs        segmentLatencySummary  `json:"lookup_api_key_cold_ms"`
		LookupWarmMs        segmentLatencySummary  `json:"lookup_api_key_warm_ms"`
		AuthSameBuyer1Off   segmentLatencySummary  `json:"authorize_same_buyer_1offer_ms"`
		AuthMultiBuyer1Off  segmentLatencySummary  `json:"authorize_multi_buyer_1offer_ms"`
		AuthMultiBuyerNOff  segmentLatencySummary  `json:"authorize_multi_buyer_Noffer_ms"`
		AuthOK              map[string]int         `json:"authorize_ok"`
		AuthFail            map[string]int         `json:"authorize_fail"`
		SlowSampleNotes     []string               `json:"slow_sample_notes,omitempty"`
		CauseHint           string                 `json:"cause_hint"`
		PgWaitEventsDuring  map[string]int         `json:"pg_wait_events_during_auth,omitempty"`
	}
	var cells []cell

	for _, maxConns := range maxConnsLevels {
		store, pool := openIsolatedTestStoreWithMaxConns(t, maxConns)
		// Buyers: one primary + 32 multi-buyer slots.
		primaryBuyer, err := store.CreateBuyerAccount(ctx,
			"tail-pri-"+uuid.NewString()+"@example.test", "integration-password", 500_000)
		if err != nil {
			t.Fatal(err)
		}
		_, primaryKey, _, err := store.CreateAPIKey(ctx, primaryBuyer, "tail primary", true)
		if err != nil {
			t.Fatal(err)
		}
		multiBuyers := make([]uuid.UUID, 32)
		multiKeys := make([]string, 32)
		multiBuyers[0], multiKeys[0] = primaryBuyer, primaryKey
		for i := 1; i < 32; i++ {
			id, cerr := store.CreateBuyerAccount(ctx,
				fmt.Sprintf("tail-mb-%d-%s@example.test", i, uuid.NewString()[:8]),
				"integration-password", 500_000)
			if cerr != nil {
				t.Fatal(cerr)
			}
			_, key, _, kerr := store.CreateAPIKey(ctx, id, "tail multi", true)
			if kerr != nil {
				t.Fatal(kerr)
			}
			multiBuyers[i], multiKeys[i] = id, key
		}

		// Offers: one primary, plus up to 32 equal-rank extras for N-offer cells.
		primaryWorker := newRealtimeClearingOffer(t, ctx, store, pool, profile, "HOT", 0.08, 0.30, offerCapacity)
		extraWorkers := make([]WorkerAuth, 32)
		extraWorkers[0] = primaryWorker
		for i := 1; i < 32; i++ {
			// Identical rates → equal verified_outcome_cost; warmth HOT matches.
			extraWorkers[i] = newRealtimeClearingOffer(t, ctx, store, pool, profile, "HOT", 0.08, 0.30, offerCapacity)
		}
		// Keep last_seen fresh.
		refresh := time.NewTicker(6 * time.Second)
		t.Cleanup(refresh.Stop)
		go func(workers []WorkerAuth) {
			for range refresh.C {
				c, cancel := context.WithTimeout(context.Background(), 3*time.Second)
				for _, w := range workers {
					_ = store.HeartbeatRealtimeOffer(c, w, RealtimeOfferHeartbeat{
						RuntimeProfileID: profile.RuntimeProfileID, Warmth: "HOT",
						AvailableSequences: offerCapacity, Status: "ACTIVE",
					})
				}
				cancel()
			}
		}(extraWorkers)

		authMaxUSD, authEstUSD, authMaxPrompt, authMaxCompletion := realtimeAuthCeiling(t, profile, 7, 2)

		replenish := func(activeWorkers []WorkerAuth) {
			t.Helper()
			// Drain non-active so 1-offer cells really have one offer.
			ids := make([]uuid.UUID, len(activeWorkers))
			for i, w := range activeWorkers {
				ids[i] = w.WorkerID
			}
			if _, err := pool.Exec(ctx, `
				UPDATE realtime_worker_offers
				   SET status='DRAINING', available_sequences=0
				 WHERE runtime_profile_id=$1 AND NOT (worker_id = ANY($2))`,
				profile.RuntimeProfileID, ids); err != nil {
				t.Fatal(err)
			}
			for _, w := range activeWorkers {
				if _, err := pool.Exec(ctx, `
					UPDATE realtime_worker_offers
					   SET available_sequences=max_active_sequences, status='ACTIVE', last_seen_at=now()
					 WHERE worker_id=$1 AND runtime_profile_id=$2`,
					w.WorkerID, profile.RuntimeProfileID); err != nil {
					t.Fatal(err)
				}
			}
		}

		for _, c := range concurrencies {
			// --- pure pool acquire ---
			statBefore := pool.Stat()
			poolSamples := measureConcurrentTail(t, c, samplesPerCell, func() time.Duration {
				start := time.Now()
				conn, err := pool.Acquire(context.Background())
				elapsed := time.Since(start)
				if err != nil {
					t.Errorf("pool acquire: %v", err)
					return elapsed
				}
				conn.Release()
				return elapsed
			})
			statAfter := pool.Stat()
			poolDelta := map[string]any{
				"empty_acquire_count_delta":     statAfter.EmptyAcquireCount() - statBefore.EmptyAcquireCount(),
				"empty_acquire_wait_ms_delta":   (statAfter.EmptyAcquireWaitTime() - statBefore.EmptyAcquireWaitTime()).Seconds() * 1000,
				"canceled_acquire_count_delta":  statAfter.CanceledAcquireCount() - statBefore.CanceledAcquireCount(),
				"max_conns":                     statAfter.MaxConns(),
				"acquired_conns_end":            statAfter.AcquiredConns(),
				"idle_conns_end":                statAfter.IdleConns(),
				"total_conns_end":               statAfter.TotalConns(),
			}

			// --- LookupAPIKey cold (cache cleared every sample; serial clear then concurrent) ---
			// Stampede-style: clear once, then fire concurrent lookups (all miss together).
			store.resetAPIKeyCacheForTest()
			// Warm the DB plan with one miss so plan-flip is not the first-sample artefact.
			if _, err := store.LookupAPIKey(ctx, primaryKey); err != nil {
				t.Fatal(err)
			}
			store.resetAPIKeyCacheForTest()
			// Barrier-style cold wave: reset, then concurrent.
			var coldStart sync.WaitGroup
			coldStart.Add(1)
			lookupCold := measureConcurrentTailBarrier(t, c, samplesPerCell, &coldStart, func() time.Duration {
				start := time.Now()
				_, err := store.LookupAPIKey(context.Background(), primaryKey)
				elapsed := time.Since(start)
				if err != nil {
					t.Errorf("lookup cold: %v", err)
				}
				return elapsed
			})
			// Second cold wave for distribution with per-sample invalidation
			// (models multi-instance / TTL expiry without full stampede).
			lookupColdPer := measureConcurrentTail(t, c, samplesPerCell, func() time.Duration {
				store.resetAPIKeyCacheForTest()
				start := time.Now()
				_, err := store.LookupAPIKey(context.Background(), primaryKey)
				elapsed := time.Since(start)
				if err != nil {
					t.Errorf("lookup cold-per: %v", err)
				}
				return elapsed
			})
			_ = lookupColdPer // retained in notes via slower of the two
			if summarizeSegmentLatency(lookupColdPer).P95 > summarizeSegmentLatency(lookupCold).P95 {
				lookupCold = lookupColdPer
			}

			// --- LookupAPIKey warm ---
			if _, err := store.LookupAPIKey(ctx, primaryKey); err != nil {
				t.Fatal(err)
			}
			lookupWarm := measureConcurrentTail(t, c, samplesPerCell, func() time.Duration {
				start := time.Now()
				_, err := store.LookupAPIKey(context.Background(), primaryKey)
				elapsed := time.Since(start)
				if err != nil {
					t.Errorf("lookup warm: %v", err)
				}
				return elapsed
			})

			// --- authorize: same buyer, 1 offer ---
			replenish([]WorkerAuth{primaryWorker})
			// Warm authorize once.
			for i := 0; i < warmup; i++ {
				contract, _, aerr := store.AuthorizeRealtimeContract(ctx, RealtimeContractAuthorization{
					RequestID: "tail-warm-" + uuid.NewString(), BuyerID: primaryBuyer, Profile: profile,
					InputCommitment: strings.Repeat("a", 64), RequestSHA256: strings.Repeat("b", 64),
					MaximumPriceUSD: authMaxUSD, EstimatedPriceUSD: authEstUSD, DeadlineAt: time.Now().Add(time.Minute),
					MaximumPromptTokens: authMaxPrompt, MaximumCompletionTokens: authMaxCompletion,
					EstimatedPromptTokens: 7, EstimatedCompletionTokens: 2,
				})
				if aerr != nil {
					t.Fatalf("warm authorize: %v", aerr)
				}
				_, _ = store.FinalizeRealtimeFailure(ctx, contract.ID, uuid.New(), 500, 1, "tail_warm", "warm", false)
			}
			replenish([]WorkerAuth{primaryWorker})

			// Sample pg wait events during the same-buyer arm (best-effort).
			waitEvents := map[string]int{}
			var waitMu sync.Mutex
			stopWait := make(chan struct{})
			var waitWG sync.WaitGroup
			waitWG.Add(1)
			go func() {
				defer waitWG.Done()
				ticker := time.NewTicker(2 * time.Millisecond)
				defer ticker.Stop()
				for {
					select {
					case <-stopWait:
						return
					case <-ticker.C:
						rows, err := pool.Query(context.Background(), `
							SELECT COALESCE(wait_event_type,'<none>') || ':' || COALESCE(wait_event,'<none>')
							  FROM pg_stat_activity
							 WHERE datname = current_database()
							   AND pid <> pg_backend_pid()
							   AND state = 'active'`)
						if err != nil {
							continue
						}
						for rows.Next() {
							var ev string
							if rows.Scan(&ev) == nil {
								waitMu.Lock()
								waitEvents[ev]++
								waitMu.Unlock()
							}
						}
						rows.Close()
					}
				}
			}()

			sameOK, sameFail := 0, 0
			// Collect contract IDs and finalize AFTER the wave so finalize lock
			// work does not contaminate authorize samples (separation of causes).
			type authRes struct {
				d  time.Duration
				id uuid.UUID
				ok bool
			}
			sameRaw := make([]authRes, samplesPerCell)
			var sameWG sync.WaitGroup
			sameJobs := make(chan int)
			for i := 0; i < c; i++ {
				sameWG.Add(1)
				go func() {
					defer sameWG.Done()
					for idx := range sameJobs {
						start := time.Now()
						contract, _, aerr := store.AuthorizeRealtimeContract(context.Background(), RealtimeContractAuthorization{
							RequestID: "tail-sb-" + uuid.NewString(), BuyerID: primaryBuyer, Profile: profile,
							InputCommitment: strings.Repeat("a", 64), RequestSHA256: strings.Repeat("b", 64),
							MaximumPriceUSD: authMaxUSD, EstimatedPriceUSD: authEstUSD, DeadlineAt: time.Now().Add(time.Minute),
							MaximumPromptTokens: authMaxPrompt, MaximumCompletionTokens: authMaxCompletion,
							EstimatedPromptTokens: 7, EstimatedCompletionTokens: 2,
						})
						elapsed := time.Since(start)
						if aerr != nil {
							sameRaw[idx] = authRes{d: elapsed, ok: false}
							continue
						}
						sameRaw[idx] = authRes{d: elapsed, id: contract.ID, ok: true}
					}
				}()
			}
			for i := 0; i < samplesPerCell; i++ {
				sameJobs <- i
			}
			close(sameJobs)
			sameWG.Wait()
			close(stopWait)
			waitWG.Wait()
			sameSamples := make([]time.Duration, 0, samplesPerCell)
			for _, s := range sameRaw {
				if s.ok {
					sameOK++
					sameSamples = append(sameSamples, s.d)
					_, _ = store.FinalizeRealtimeFailure(ctx, s.id, uuid.New(), 500, 1, "tail_sb", "teardown", false)
				} else {
					sameFail++
				}
			}

			// --- authorize: multi buyer, 1 offer ---
			replenish([]WorkerAuth{primaryWorker})
			mb1OK, mb1Fail := 0, 0
			mb1Raw := make([]authRes, samplesPerCell)
			var mb1WG sync.WaitGroup
			mb1Jobs := make(chan int)
			for i := 0; i < c; i++ {
				workerIdx := i
				mb1WG.Add(1)
				go func() {
					defer mb1WG.Done()
					buyer := multiBuyers[workerIdx%len(multiBuyers)]
					for idx := range mb1Jobs {
						start := time.Now()
						contract, _, aerr := store.AuthorizeRealtimeContract(context.Background(), RealtimeContractAuthorization{
							RequestID: "tail-mb1-" + uuid.NewString(), BuyerID: buyer, Profile: profile,
							InputCommitment: strings.Repeat("a", 64), RequestSHA256: strings.Repeat("b", 64),
							MaximumPriceUSD: authMaxUSD, EstimatedPriceUSD: authEstUSD, DeadlineAt: time.Now().Add(time.Minute),
							MaximumPromptTokens: authMaxPrompt, MaximumCompletionTokens: authMaxCompletion,
							EstimatedPromptTokens: 7, EstimatedCompletionTokens: 2,
						})
						elapsed := time.Since(start)
						if aerr != nil {
							mb1Raw[idx] = authRes{d: elapsed, ok: false}
							continue
						}
						mb1Raw[idx] = authRes{d: elapsed, id: contract.ID, ok: true}
					}
				}()
			}
			for i := 0; i < samplesPerCell; i++ {
				mb1Jobs <- i
			}
			close(mb1Jobs)
			mb1WG.Wait()
			mb1Samples := make([]time.Duration, 0, samplesPerCell)
			for _, s := range mb1Raw {
				if s.ok {
					mb1OK++
					mb1Samples = append(mb1Samples, s.d)
					_, _ = store.FinalizeRealtimeFailure(ctx, s.id, uuid.New(), 500, 1, "tail_mb1", "teardown", false)
				} else {
					mb1Fail++
				}
			}

			// --- authorize: multi buyer, N=c equal-rank offers ---
			nOff := c
			if nOff < 1 {
				nOff = 1
			}
			if nOff > len(extraWorkers) {
				nOff = len(extraWorkers)
			}
			replenish(extraWorkers[:nOff])
			mbNOK, mbNFail := 0, 0
			mbNRaw := make([]authRes, samplesPerCell)
			var mbNWG sync.WaitGroup
			mbNJobs := make(chan int)
			for i := 0; i < c; i++ {
				workerIdx := i
				mbNWG.Add(1)
				go func() {
					defer mbNWG.Done()
					buyer := multiBuyers[workerIdx%len(multiBuyers)]
					for idx := range mbNJobs {
						start := time.Now()
						contract, _, aerr := store.AuthorizeRealtimeContract(context.Background(), RealtimeContractAuthorization{
							RequestID: "tail-mbn-" + uuid.NewString(), BuyerID: buyer, Profile: profile,
							InputCommitment: strings.Repeat("a", 64), RequestSHA256: strings.Repeat("b", 64),
							MaximumPriceUSD: authMaxUSD, EstimatedPriceUSD: authEstUSD, DeadlineAt: time.Now().Add(time.Minute),
							MaximumPromptTokens: authMaxPrompt, MaximumCompletionTokens: authMaxCompletion,
							EstimatedPromptTokens: 7, EstimatedCompletionTokens: 2,
						})
						elapsed := time.Since(start)
						if aerr != nil {
							mbNRaw[idx] = authRes{d: elapsed, ok: false}
							continue
						}
						mbNRaw[idx] = authRes{d: elapsed, id: contract.ID, ok: true}
					}
				}()
			}
			for i := 0; i < samplesPerCell; i++ {
				mbNJobs <- i
			}
			close(mbNJobs)
			mbNWG.Wait()
			mbNSamples := make([]time.Duration, 0, samplesPerCell)
			for _, s := range mbNRaw {
				if s.ok {
					mbNOK++
					mbNSamples = append(mbNSamples, s.d)
					_, _ = store.FinalizeRealtimeFailure(ctx, s.id, uuid.New(), 500, 1, "tail_mbn", "teardown", false)
				} else {
					mbNFail++
				}
			}

			poolSum := summarizeSegmentLatency(poolSamples)
			sameSum := summarizeSegmentLatency(sameSamples)
			mb1Sum := summarizeSegmentLatency(mb1Samples)
			mbNSum := summarizeSegmentLatency(mbNSamples)
			coldSum := summarizeSegmentLatency(lookupCold)
			warmSum := summarizeSegmentLatency(lookupWarm)

			hint := classifyTailCause(c, int(maxConns), poolSum, sameSum, mb1Sum, mbNSum, coldSum, poolDelta)

			waitMu.Lock()
			waitCopy := make(map[string]int, len(waitEvents))
			for k, v := range waitEvents {
				waitCopy[k] = v
			}
			waitMu.Unlock()

			cellOut := cell{
				MaxConns:           maxConns,
				Concurrency:        c,
				Samples:            samplesPerCell,
				PoolAcquireMs:      poolSum,
				PoolStatDelta:      poolDelta,
				LookupColdMs:       coldSum,
				LookupWarmMs:       warmSum,
				AuthSameBuyer1Off:  sameSum,
				AuthMultiBuyer1Off: mb1Sum,
				AuthMultiBuyerNOff: mbNSum,
				AuthOK: map[string]int{
					"same_buyer_1offer":  sameOK,
					"multi_buyer_1offer": mb1OK,
					"multi_buyer_Noffer": mbNOK,
				},
				AuthFail: map[string]int{
					"same_buyer_1offer":  sameFail,
					"multi_buyer_1offer": mb1Fail,
					"multi_buyer_Noffer": mbNFail,
				},
				CauseHint:          hint,
				PgWaitEventsDuring: waitCopy,
			}
			// Note slow samples: max of same-buyer arm.
			if len(sameSamples) > 0 {
				var maxD time.Duration
				for _, d := range sameSamples {
					if d > maxD {
						maxD = d
					}
				}
				cellOut.SlowSampleNotes = append(cellOut.SlowSampleNotes,
					fmt.Sprintf("same_buyer max=%.3fms p95=%.3fms p50=%.3fms ratio_p95/p50=%.2f",
						float64(maxD)/float64(time.Millisecond), sameSum.P95, sameSum.P50,
						safeRatio(sameSum.P95, sameSum.P50)))
			}
			cells = append(cells, cellOut)
			t.Logf("max_conns=%d c=%d pool_p95=%.3f same_p95=%.3f mb1_p95=%.3f mbN_p95=%.3f cold_p95=%.3f warm_p95=%.3f | %s",
				maxConns, c, poolSum.P95, sameSum.P95, mb1Sum.P95, mbNSum.P95, coldSum.P95, warmSum.P95, hint)
			fmt.Printf("TAIL max_conns=%d c=%d pool_p95_ms=%.3f same_p95=%.3f mb1_p95=%.3f mbN_p95=%.3f cold_p95=%.3f\n",
				maxConns, c, poolSum.P95, sameSum.P95, mb1Sum.P95, mbNSum.P95, coldSum.P95)
		}
		// Close this MaxConns store/pool before opening the next (cleanup via t.Cleanup).
	}

	// Cross-cell cause synthesis from the factorial.
	var (
		poolStarvationCells  int
		offerRowLockCells    int
		rank1PileOnCells     int
		buyerFundingCells    int
		lookupColdTailCells  int
	)
	for _, cl := range cells {
		h := cl.CauseHint
		if strings.Contains(h, "POOL_ACQUIRE_WAIT") {
			poolStarvationCells++
		}
		if strings.Contains(h, "OFFER_ROW_LOCK") {
			offerRowLockCells++
		}
		if strings.Contains(h, "RANK1_PILE_ON") {
			rank1PileOnCells++
		}
		if strings.Contains(h, "BUYER_FUNDING_LOCK") {
			buyerFundingCells++
		}
		if strings.Contains(h, "LOOKUP_COLD_TAIL") {
			lookupColdTailCells++
		}
	}
	synthesis := map[string]any{
		"pool_starvation_cells":   poolStarvationCells,
		"offer_row_lock_cells":    offerRowLockCells,
		"rank1_pile_on_cells":     rank1PileOnCells,
		"buyer_funding_lock_cells": buyerFundingCells,
		"lookup_cold_tail_cells":  lookupColdTailCells,
		"how_to_read": "" +
			"If pool_acquire_ms.p95 rises when max_conns=20 and c=32 but not at max_conns=64, " +
			"the tail is connection-pool starvation. " +
			"If authorize_multi_buyer_1offer_ms tails with pool_ok, the tail is offer-row lock. " +
			"If authorize_multi_buyer_Noffer_ms ≈ 1offer, the SQL always piles onto selected_rank=1 " +
			"(FOR UPDATE without SKIP LOCKED across candidates). " +
			"If same_buyer >> multi_buyer_1offer, buyer funding advisory lock dominates.",
		"per_cell_cause_hints": func() []string {
			out := make([]string, 0, len(cells))
			for _, cl := range cells {
				out = append(out, fmt.Sprintf("max_conns=%d c=%d: %s", cl.MaxConns, cl.Concurrency, cl.CauseHint))
			}
			return out
		}(),
	}

	phase := strings.TrimSpace(os.Getenv("MERC_AUTHORIZE_TAIL_PHASE"))
	if phase == "" {
		phase = "baseline"
	}
	out := map[string]any{
		"schema_version": 1,
		"kind":           "authorize_auth_tail_characterization",
		"phase":          phase,
		"measured_at":    time.Now().UTC().Format(time.RFC3339),
		"method": map[string]any{
			"what": "Factorial separation of authorize and LookupAPIKey tails into " +
				"pool acquire wait, same-buyer funding lock, single-offer row lock, " +
				"and rank-1 pile-on (multi equal-rank offers).",
			"concurrency_levels": concurrencies,
			"max_conns_levels":   maxConnsLevels,
			"samples_per_cell":   samplesPerCell,
			"void_strategy":      "FinalizeRealtimeFailure AFTER each authorize wave (not interleaved)",
			"percentile_method":  "nearest-rank: ceil(p*n)-1",
			"lock_hierarchy":     "unchanged: buyer funding before offer capacity",
			"machine": map[string]any{
				"hostname":     host,
				"goos":         runtime.GOOS,
				"goarch":       runtime.GOARCH,
				"num_cpu":      runtime.NumCPU(),
				"load_average": loadAvg,
				"load_n":       loadN,
				"quiet":        quiet,
				"quiet_reason": quietReason(quiet, loadAvg, loadN),
			},
		},
		"cells":     cells,
		"synthesis": synthesis,
		"does_not_prove": []string{
			"engine / model inference latency",
			"end-to-end client TTFT under a real Metal/CUDA engine",
			"that multi-tenant production load matches this fixture shape",
		},
	}

	dir := filepath.Join("..", "evidence", "perf")
	name := fmt.Sprintf("authorize-auth-tails-%s.json", time.Now().UTC().Format("20060102T150405Z"))
	path := filepath.Join(dir, name)
	id, bin, err := DefaultBoundIdentity("..", "control/authorize_tail_characterize_test.go",
		"embedded method + cells", "embedded cells[].samples")
	if err != nil {
		t.Fatal(err)
	}
	if err := WriteBoundEvidenceJSON(EvidenceWriteRequest{
		RepoRoot: "..", Path: path, Payload: out,
		Identity: id, BuildBinaryPath: bin,
	}); err != nil {
		t.Fatal(err)
	}
	alias := filepath.Join(dir, "authorize-auth-tails-latest.json")
	if err := WriteBoundEvidenceJSON(EvidenceWriteRequest{
		RepoRoot: "..", Path: alias, Payload: out,
		Identity: id, BuildBinaryPath: bin,
	}); err != nil {
		t.Fatal(err)
	}
	t.Logf("wrote %s", path)
}

func openIsolatedTestStoreWithMaxConns(t *testing.T, maxConns int32) (*Store, *pgxpool.Pool) {
	t.Helper()
	base := requireTestDatabase(t)
	parsed, err := url.Parse(base)
	if err != nil {
		t.Fatalf("parse MERC_TEST_DATABASE_URL: %v", err)
	}
	name := "cx_iso_" + strings.ReplaceAll(uuid.NewString(), "-", "")[:24]
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	t.Cleanup(cancel)

	admin := *parsed
	admin.Path = "/postgres"
	adminPool, err := pgxpool.New(ctx, admin.String())
	if err != nil {
		t.Fatalf("connect to postgres for database creation: %v", err)
	}
	if _, err := adminPool.Exec(ctx, `CREATE DATABASE `+name); err != nil {
		adminPool.Close()
		t.Fatalf("create isolated database: %v", err)
	}
	adminPool.Close()
	t.Cleanup(func() {
		c, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		p, err := pgxpool.New(c, admin.String())
		if err != nil {
			return
		}
		defer p.Close()
		_, _ = p.Exec(c, `DROP DATABASE IF EXISTS `+name+` WITH (FORCE)`)
	})

	own := *parsed
	own.Path = "/" + name
	cfg, err := pgxpool.ParseConfig(own.String())
	if err != nil {
		t.Fatal(err)
	}
	cfg.MaxConns = maxConns
	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		t.Fatalf("connect isolated database: %v", err)
	}
	t.Cleanup(pool.Close)
	store := NewStore(pool)
	if err := store.Migrate(ctx); err != nil {
		t.Fatalf("apply canonical schema: %v", err)
	}
	return store, pool
}

func measureConcurrentTail(t *testing.T, concurrency, samples int, fn func() time.Duration) []time.Duration {
	t.Helper()
	if concurrency < 1 {
		concurrency = 1
	}
	out := make([]time.Duration, samples)
	var wg sync.WaitGroup
	jobs := make(chan int)
	for i := 0; i < concurrency; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for idx := range jobs {
				out[idx] = fn()
			}
		}()
	}
	for i := 0; i < samples; i++ {
		jobs <- i
	}
	close(jobs)
	wg.Wait()
	return out
}

// measureConcurrentTailBarrier starts all workers blocked on start.Done() so the
// first wave is a true concurrent stampede (cache cold-start).
func measureConcurrentTailBarrier(t *testing.T, concurrency, samples int, start *sync.WaitGroup, fn func() time.Duration) []time.Duration {
	t.Helper()
	if concurrency < 1 {
		concurrency = 1
	}
	out := make([]time.Duration, samples)
	var wg sync.WaitGroup
	var ready atomic.Int32
	jobs := make(chan int, samples)
	for i := 0; i < samples; i++ {
		jobs <- i
	}
	close(jobs)
	for i := 0; i < concurrency; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			ready.Add(1)
			start.Wait()
			for idx := range jobs {
				out[idx] = fn()
			}
		}()
	}
	// Wait until all workers are parked on the barrier.
	for ready.Load() < int32(concurrency) {
		time.Sleep(100 * time.Microsecond)
	}
	start.Done()
	wg.Wait()
	return out
}

func safeRatio(a, b float64) float64 {
	if b <= 0 {
		return math.Inf(1)
	}
	return a / b
}

func classifyTailCause(
	c int, maxConns int,
	pool, same, mb1, mbN, cold segmentLatencySummary,
	poolDelta map[string]any,
) string {
	var parts []string
	emptyWait, _ := poolDelta["empty_acquire_wait_ms_delta"].(float64)
	emptyCount, _ := poolDelta["empty_acquire_count_delta"].(int64)
	if emptyCount > 0 || emptyWait > 1.0 || (c > maxConns && pool.P95 > 0.5) {
		parts = append(parts, fmt.Sprintf(
			"POOL_ACQUIRE_WAIT: empty_acquire_count_delta=%v empty_wait_ms_delta=%.3f pool_p95=%.3fms (c=%d max_conns=%d)",
			emptyCount, emptyWait, pool.P95, c, maxConns))
	} else {
		parts = append(parts, fmt.Sprintf("pool_ok: pool_p95=%.3fms empty_count=%v", pool.P95, emptyCount))
	}

	// Offer-row lock if multi-buyer 1-offer still has heavy tail.
	if mb1.P95 > 5 && mb1.P95 > 3*mb1.P50 {
		parts = append(parts, fmt.Sprintf(
			"OFFER_ROW_LOCK: multi_buyer_1offer p95=%.3f p50=%.3f (same-buyer funding excluded)",
			mb1.P95, mb1.P50))
	}
	// Rank-1 pile-on: multi-offer does NOT shrink the 1-offer tail.
	if mb1.P95 > 5 && mbN.P95 > 0.7*mb1.P95 {
		parts = append(parts, fmt.Sprintf(
			"RANK1_PILE_ON: multi_buyer_Noffer p95=%.3f still ~ multi_buyer_1offer p95=%.3f — concurrent claims all target selected_rank=1",
			mbN.P95, mb1.P95))
	} else if mb1.P95 > 5 && mbN.P95 < 0.5*mb1.P95 {
		parts = append(parts, fmt.Sprintf(
			"MULTI_OFFER_HELPS: N-offer p95=%.3f << 1-offer p95=%.3f (unexpected under current rank-1 SQL)",
			mbN.P95, mb1.P95))
	}
	// Funding lock: same-buyer worse than multi-buyer 1-offer.
	if same.P95 > 5 && same.P95 > 1.3*mb1.P95 {
		parts = append(parts, fmt.Sprintf(
			"BUYER_FUNDING_LOCK: same_buyer p95=%.3f > multi_buyer_1offer p95=%.3f",
			same.P95, mb1.P95))
	} else if same.P95 > 5 {
		parts = append(parts, fmt.Sprintf(
			"same_buyer_tail: p95=%.3f (not clearly worse than offer-only mb1 p95=%.3f)",
			same.P95, mb1.P95))
	}
	if cold.P95 > 5 {
		parts = append(parts, fmt.Sprintf("LOOKUP_COLD_TAIL: p95=%.3fms", cold.P95))
	} else {
		parts = append(parts, fmt.Sprintf("lookup_cold_ok: p95=%.3fms", cold.P95))
	}
	return strings.Join(parts, " | ")
}

