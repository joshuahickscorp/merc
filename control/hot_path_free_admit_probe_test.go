package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// TestHotPathFreeAdmitProbe answers: how cheap can durable admission get when
// funding is pre-authorized (envelope) and capacity selection is O(1) (lease
// stand-in: direct offer claim by worker primary key), versus today's legacy
// multi-aggregate path.
//
// It does NOT wire a production path that skips the durable contract insert —
// that design dies under concurrent ceiling and crash recovery (see
// docs/HOT_PATH_DURABLE_ADMISSION.md). It measures the surviving floor:
// amortized O(1) durable holds still synchronous before dispatch.
//
// Opt-in:
//
//	MERC_HOT_PATH_FREE_PROBE=1 \
//	MERC_TEST_DATABASE_URL=postgres://cx:cx@localhost:5432/cx?sslmode=disable \
//	go test -count=1 -run TestHotPathFreeAdmitProbe -timeout 30m ./control
//
// Writes evidence/perf/hot-path-free-admit-*.json (unbound probe receipt).
func TestHotPathFreeAdmitProbe(t *testing.T) {
	if os.Getenv("MERC_HOT_PATH_FREE_PROBE") != "1" {
		t.Skip("set MERC_HOT_PATH_FREE_PROBE=1 to measure hot-path-free admit floors")
	}
	installSettlementCurrencyForTest(t, "usd")
	t.Setenv("MERC_TOKEN_KEY", "hot-path-free-probe-key-with-32-bytes-min!!!!")

	loadAvg, loadN := readLoadAverage()
	quiet := machineLoadQuiet()
	host, _ := os.Hostname()
	t.Logf("load before measure: avg=%v n=%d quiet=%v hostname=%s cpus=%d",
		loadAvg, loadN, quiet, host, runtime.NumCPU())

	ctx := context.Background()
	store, pool := openIsolatedTestStoreWithMaxConns(t, 20)
	profile := sortedVLLMProfiles()[0]
	const (
		samplesPerCell = 80
		warmup         = 10
		offerCapacity  = 100_000
	)
	concurrencies := []int{1, 8, 32}

	maxUSD, estUSD, maxPrompt, maxCompletion := realtimeAuthCeiling(t, profile, 7, 2)
	needNanos := usdToMicros(maxUSD) * NanosPerMicro
	if needNanos <= 0 {
		t.Fatal("needNanos must be positive")
	}

	// One buyer with large prepaid so envelope caps never starve the sample set.
	buyerID := envelopeTestBuyer(t, ctx, store, pool, 500_000_000) // $500
	worker := newRealtimeClearingOffer(t, ctx, store, pool, profile, "HOT", 0.08, 0.30, offerCapacity)

	refresh := time.NewTicker(5 * time.Second)
	t.Cleanup(refresh.Stop)
	go func() {
		for range refresh.C {
			c, cancel := context.WithTimeout(context.Background(), 2*time.Second)
			_ = store.HeartbeatRealtimeOffer(c, worker, RealtimeOfferHeartbeat{
				RuntimeProfileID: profile.RuntimeProfileID, Warmth: "HOT",
				AvailableSequences: offerCapacity, Status: "ACTIVE",
			})
			cancel()
		}
	}()

	replenishOffer := func() {
		t.Helper()
		if _, err := pool.Exec(ctx, `
			UPDATE realtime_worker_offers
			   SET available_sequences=$1, status='ACTIVE', last_seen_at=now()
			 WHERE worker_id=$2 AND runtime_profile_id=$3`,
			offerCapacity, worker.WorkerID, profile.RuntimeProfileID); err != nil {
			t.Fatal(err)
		}
	}

	var envMu sync.Mutex
	var env ExecutionEnvelope
	ensureEnvelope := func() {
		t.Helper()
		envMu.Lock()
		defer envMu.Unlock()
		capN := needNanos * int64(samplesPerCell*64)
		maxReq := int64(samplesPerCell * 128)
		if env.ID != uuid.Nil {
			cur, err := store.GetExecutionEnvelope(ctx, buyerID, env.ID)
			if err == nil && cur.State == "ACTIVE" &&
				cur.CapNanos-cur.SpentNanos-cur.ReservedNanos > needNanos*int64(samplesPerCell) &&
				cur.RequestCount+int64(samplesPerCell) <= cur.MaxRequests {
				return
			}
		}
		created, err := store.CreateExecutionEnvelope(ctx, buyerID, ExecutionEnvelopeCreateRequest{
			RuntimeProfileID:       profile.RuntimeProfileID,
			CapNanos:               capN,
			MaxRequests:            maxReq,
			MaxTokens:              0,
			PerRequestCeilingNanos: needNanos,
			TTLSeconds:             3600,
		})
		if err != nil {
			t.Fatalf("create envelope: %v", err)
		}
		env = created
	}

	type pathCell struct {
		Path        string                `json:"path"`
		Concurrency int                   `json:"concurrency"`
		Samples     int                   `json:"samples"`
		OK          int                   `json:"ok"`
		Fail        int                   `json:"fail"`
		AuthorizeMs segmentLatencySummary `json:"authorize_ms"`
		Notes       []string              `json:"notes,omitempty"`
	}
	var cells []pathCell

	appendCell := func(c pathCell) {
		cells = append(cells, c)
		t.Logf("%s c=%d p50=%.3fms p95=%.3fms ok=%d fail=%d",
			c.Path, c.Concurrency, c.AuthorizeMs.P50, c.AuthorizeMs.P95, c.OK, c.Fail)
	}

	// --- Path A: legacy AuthorizeRealtimeContract ---
	runLegacy := func(c int) {
		replenishOffer()
		for i := 0; i < warmup; i++ {
			contract, _, err := store.AuthorizeRealtimeContract(ctx, RealtimeContractAuthorization{
				RequestID: "warm-leg-" + uuid.NewString(), BuyerID: buyerID, Profile: profile,
				InputCommitment: strings.Repeat("a", 64), RequestSHA256: strings.Repeat("b", 64),
				MaximumPriceUSD: maxUSD, EstimatedPriceUSD: estUSD,
				DeadlineAt: time.Now().Add(time.Minute),
				MaximumPromptTokens: maxPrompt, MaximumCompletionTokens: maxCompletion,
				EstimatedPromptTokens: 7, EstimatedCompletionTokens: 2,
				BuyerDeclaredCeilingUSD: maxUSD * 1.1,
			})
			if err == nil {
				_, _ = store.FinalizeRealtimeFailure(ctx, contract.ID, uuid.New(), 500, 1, "warm", "warm", false)
			}
		}
		replenishOffer()
		var ok, fail atomic.Int64
		ms := measureConcurrent(t, c, samplesPerCell, func() time.Duration {
			start := time.Now()
			contract, _, err := store.AuthorizeRealtimeContract(context.Background(), RealtimeContractAuthorization{
				RequestID: "leg-" + uuid.NewString(), BuyerID: buyerID, Profile: profile,
				InputCommitment: strings.Repeat("a", 64), RequestSHA256: strings.Repeat("b", 64),
				MaximumPriceUSD: maxUSD, EstimatedPriceUSD: estUSD,
				DeadlineAt: time.Now().Add(time.Minute),
				MaximumPromptTokens: maxPrompt, MaximumCompletionTokens: maxCompletion,
				EstimatedPromptTokens: 7, EstimatedCompletionTokens: 2,
				BuyerDeclaredCeilingUSD: maxUSD * 1.1,
			})
			elapsed := time.Since(start)
			if err != nil {
				fail.Add(1)
				return elapsed
			}
			ok.Add(1)
			_, _ = store.FinalizeRealtimeFailure(context.Background(), contract.ID, uuid.New(), 500, 1,
				"probe", "teardown", false)
			return elapsed
		})
		appendCell(pathCell{
			Path: "legacy_multi_aggregate", Concurrency: c, Samples: samplesPerCell,
			OK: int(ok.Load()), Fail: int(fail.Load()),
			AuthorizeMs: summarizeSegmentLatency(ms),
			Notes: []string{
				"full evaluateRealtimeBuyerFunding under advisory lock + offer claim + contract insert",
			},
		})
	}

	// --- Path B: envelope AuthorizeRealtimeContract ---
	runEnvelope := func(c int) {
		ensureEnvelope()
		replenishOffer()
		for i := 0; i < warmup; i++ {
			ensureEnvelope()
			contract, _, err := store.AuthorizeRealtimeContract(ctx, RealtimeContractAuthorization{
				RequestID: "warm-env-" + uuid.NewString(), BuyerID: buyerID, Profile: profile,
				InputCommitment: strings.Repeat("a", 64), RequestSHA256: strings.Repeat("b", 64),
				MaximumPriceUSD: maxUSD, EstimatedPriceUSD: estUSD,
				DeadlineAt: time.Now().Add(time.Minute),
				IdempotencyKey: "warm-env-key-" + uuid.NewString(),
				MaximumPromptTokens: maxPrompt, MaximumCompletionTokens: maxCompletion,
				EstimatedPromptTokens: 7, EstimatedCompletionTokens: 2,
				EnvelopeID: env.ID,
			})
			if err == nil {
				_, _ = store.FinalizeRealtimeFailure(ctx, contract.ID, uuid.New(), 500, 1, "warm", "warm", false)
			}
		}
		replenishOffer()
		ensureEnvelope()
		var ok, fail atomic.Int64
		ms := measureConcurrent(t, c, samplesPerCell, func() time.Duration {
			ensureEnvelope()
			start := time.Now()
			contract, _, err := store.AuthorizeRealtimeContract(context.Background(), RealtimeContractAuthorization{
				RequestID: "env-" + uuid.NewString(), BuyerID: buyerID, Profile: profile,
				InputCommitment: strings.Repeat("a", 64), RequestSHA256: strings.Repeat("b", 64),
				MaximumPriceUSD: maxUSD, EstimatedPriceUSD: estUSD,
				DeadlineAt: time.Now().Add(time.Minute),
				IdempotencyKey: "env-key-" + uuid.NewString(),
				MaximumPromptTokens: maxPrompt, MaximumCompletionTokens: maxCompletion,
				EstimatedPromptTokens: 7, EstimatedCompletionTokens: 2,
				EnvelopeID: env.ID,
			})
			elapsed := time.Since(start)
			if err != nil {
				fail.Add(1)
				return elapsed
			}
			ok.Add(1)
			_, _ = store.FinalizeRealtimeFailure(context.Background(), contract.ID, uuid.New(), 500, 1,
				"probe", "teardown", false)
			return elapsed
		})
		appendCell(pathCell{
			Path: "envelope_active", Concurrency: c, Samples: samplesPerCell,
			OK: int(ok.Load()), Fail: int(fail.Load()),
			AuthorizeMs: summarizeSegmentLatency(ms),
			Notes: []string{
				"reserveEnvelopeSpendTx (O(1) UPDATE) + full offer ranking claim + contract insert",
				"no buyer funding advisory lock on admit",
			},
		})
	}

	// --- Path C: envelope + direct offer claim (capacity-lease stand-in) ---
	runEnvelopeDirectClaim := func(c int) {
		ensureEnvelope()
		replenishOffer()
		for i := 0; i < warmup; i++ {
			ensureEnvelope()
			if _, err := authorizeEnvelopeDirectClaim(ctx, store, pool, buyerID, env.ID, worker, profile,
				maxUSD, estUSD, maxPrompt, maxCompletion, needNanos); err != nil {
				t.Logf("warm direct-claim: %v", err)
			}
		}
		replenishOffer()
		ensureEnvelope()
		var ok, fail atomic.Int64
		ms := measureConcurrent(t, c, samplesPerCell, func() time.Duration {
			ensureEnvelope()
			start := time.Now()
			contractID, err := authorizeEnvelopeDirectClaim(context.Background(), store, pool,
				buyerID, env.ID, worker, profile, maxUSD, estUSD, maxPrompt, maxCompletion, needNanos)
			elapsed := time.Since(start)
			if err != nil {
				fail.Add(1)
				return elapsed
			}
			ok.Add(1)
			_, _ = store.FinalizeRealtimeFailure(context.Background(), contractID, uuid.New(), 500, 1,
				"probe", "teardown", false)
			return elapsed
		})
		appendCell(pathCell{
			Path: "envelope_plus_direct_offer_claim", Concurrency: c, Samples: samplesPerCell,
			OK: int(ok.Load()), Fail: int(fail.Load()),
			AuthorizeMs: summarizeSegmentLatency(ms),
			Notes: []string{
				"stand-in for envelope + capacity lease: O(1) envelope spend + O(1) offer UPDATE by worker PK + contract/event insert",
				"skips ranking CTE; not production-wired; hierarchy preserved (buyer KEY SHARE before offer)",
			},
		})
	}

	type decomp struct {
		BeginMs         segmentLatencySummary `json:"begin_ms"`
		EnvelopeSpendMs segmentLatencySummary `json:"envelope_spend_ms"`
		DirectOfferMs   segmentLatencySummary `json:"direct_offer_claim_ms"`
		PricingMs       segmentLatencySummary `json:"pricing_ms"`
		ContractBatchMs segmentLatencySummary `json:"contract_event_batch_ms"`
		CommitMs        segmentLatencySummary `json:"commit_ms"`
		TotalMs         segmentLatencySummary `json:"total_ms"`
	}
	var decompOut decomp
	{
		ensureEnvelope()
		replenishOffer()
		n := samplesPerCell
		begins := make([]time.Duration, 0, n)
		spends := make([]time.Duration, 0, n)
		offers := make([]time.Duration, 0, n)
		pricings := make([]time.Duration, 0, n)
		batches := make([]time.Duration, 0, n)
		commits := make([]time.Duration, 0, n)
		totals := make([]time.Duration, 0, n)
		for i := 0; i < n; i++ {
			ensureEnvelope()
			parts, contractID, err := authorizeEnvelopeDirectClaimTimed(ctx, store, pool, buyerID, env.ID,
				worker, profile, maxUSD, estUSD, maxPrompt, maxCompletion, needNanos)
			if err != nil {
				t.Fatalf("decomp sample %d: %v", i, err)
			}
			begins = append(begins, parts.begin)
			spends = append(spends, parts.envelopeSpend)
			offers = append(offers, parts.directOffer)
			pricings = append(pricings, parts.pricing)
			batches = append(batches, parts.contractBatch)
			commits = append(commits, parts.commit)
			totals = append(totals, parts.total)
			_, _ = store.FinalizeRealtimeFailure(ctx, contractID, uuid.New(), 500, 1, "decomp", "teardown", false)
		}
		decompOut = decomp{
			BeginMs:         summarizeSegmentLatency(begins),
			EnvelopeSpendMs: summarizeSegmentLatency(spends),
			DirectOfferMs:   summarizeSegmentLatency(offers),
			PricingMs:       summarizeSegmentLatency(pricings),
			ContractBatchMs: summarizeSegmentLatency(batches),
			CommitMs:        summarizeSegmentLatency(commits),
			TotalMs:         summarizeSegmentLatency(totals),
		}
		t.Logf("decomp c=1 envelope_spend p50=%.3f direct_offer p50=%.3f contract_batch p50=%.3f commit p50=%.3f total p50=%.3f",
			decompOut.EnvelopeSpendMs.P50, decompOut.DirectOfferMs.P50,
			decompOut.ContractBatchMs.P50, decompOut.CommitMs.P50, decompOut.TotalMs.P50)
	}

	for _, c := range concurrencies {
		runLegacy(c)
		runEnvelope(c)
		runEnvelopeDirectClaim(c)
	}

	find := func(path string, c int) *pathCell {
		for i := range cells {
			if cells[i].Path == path && cells[i].Concurrency == c {
				return &cells[i]
			}
		}
		return nil
	}
	p50 := func(c *pathCell) float64 {
		if c == nil {
			return 0
		}
		return c.AuthorizeMs.P50
	}
	p95 := func(c *pathCell) float64 {
		if c == nil {
			return 0
		}
		return c.AuthorizeMs.P95
	}

	table := map[string]any{}
	for _, path := range []string{"legacy_multi_aggregate", "envelope_active", "envelope_plus_direct_offer_claim"} {
		row := map[string]any{}
		for _, c := range concurrencies {
			cell := find(path, c)
			row[fmt.Sprintf("c=%d", c)] = map[string]float64{"p50_ms": p50(cell), "p95_ms": p95(cell)}
		}
		table[path] = row
	}

	verdict := map[string]any{
		"question": "Can admission be durable without being synchronous on the first-token path?",
		"strict_zero_durable_before_dispatch": map[string]any{
			"survives": false,
			"why": "concurrent admits without a durable budget decrement overspend the buyer ceiling; " +
				"crash without a durable intent breaks supplier liability or recreates the TTL-hold P0 shape",
		},
		"amortized_o1_durable_before_dispatch": map[string]any{
			"survives": true,
			"why": "envelope (or O(1) committed counter) + capacity lease make per-request durable work " +
				"two UPDATEs + contract insert + commit; money holds are durable before dispatch",
			"still_synchronous": true,
			"exposure_window":   "none beyond today's EXECUTING hold — durable before dispatch",
		},
		"measured_authorize_p50_c1_ms": map[string]float64{
			"legacy":                 p50(find("legacy_multi_aggregate", 1)),
			"envelope":               p50(find("envelope_active", 1)),
			"envelope_plus_direct":   p50(find("envelope_plus_direct_offer_claim", 1)),
			"direct_claim_decomp":    decompOut.TotalMs.P50,
		},
		"prior_context_ms": map[string]float64{
			"merc_owned_metal_p50":  2.994,
			"authorize_share_metal": 2.6,
			"legacy_floor_claimed":  1.0,
		},
		"target_le_1ms_merc_added": map[string]any{
			"reachable_by_optimising_legacy_path": false,
			"reachable_with_zero_durable_sync":    false,
			"reachable_with_amortized_o1_sync": "borderline — authorize can drop below 1 ms, but merc-added " +
				"includes intake/admission/proxy; Metal path was ~3 ms merc-owned. Renegotiate or prove e2e.",
		},
	}

	payload := map[string]any{
		"kind":             "hot_path_free_admit_probe",
		"schema_version":   1,
		"binding_status":   "UNBOUND_PROBE",
		"measured_at":      time.Now().UTC().Format(time.RFC3339Nano),
		"hostname":         host,
		"cpus":             runtime.NumCPU(),
		"load_average":     loadAvg,
		"load_n":           loadN,
		"machine_quiet":    quiet,
		"samples_per_cell": samplesPerCell,
		"warmup":           warmup,
		"profile_id":       profile.RuntimeProfileID,
		"cells":            cells,
		"summary_table":    table,
		"direct_claim_decomp_c1": decompOut,
		"verdict":          verdict,
		"can_prove": []string{
			"envelope path authorize latency vs legacy at c=1,8,32",
			"envelope + direct offer claim (lease stand-in) authorize latency",
			"serial decomposition of the lease-stand-in durable statements",
		},
		"does_not_prove": []string{
			"end-to-end Merc-added TTFT against a real Metal/CUDA engine",
			"production capacity leases (not implemented; stand-in only)",
			"safety of zero-durable-before-dispatch (disqualified by argument, not measured)",
			"O(1) materialised committed-micros counter (named next step on short-reserve branch; not built here)",
		},
		"design_doc": "docs/HOT_PATH_DURABLE_ADMISSION.md",
	}

	dir := filepath.Join("..", "evidence", "perf")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	ts := time.Now().UTC().Format("20060102T150405Z")
	outPath := filepath.Join(dir, "hot-path-free-admit-"+ts+".json")
	latest := filepath.Join(dir, "hot-path-free-admit-latest.json")
	raw, err := json.MarshalIndent(payload, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(outPath, raw, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(latest, raw, 0o644); err != nil {
		t.Fatal(err)
	}
	t.Logf("wrote %s and %s", outPath, latest)
	fmt.Printf("HOT_PATH_FREE summary_table=%s\n", hotPathMustJSON(table))
	fmt.Printf("HOT_PATH_FREE decomp_total_p50=%.3f envelope_spend_p50=%.3f direct_offer_p50=%.3f contract_batch_p50=%.3f commit_p50=%.3f\n",
		decompOut.TotalMs.P50, decompOut.EnvelopeSpendMs.P50, decompOut.DirectOfferMs.P50,
		decompOut.ContractBatchMs.P50, decompOut.CommitMs.P50)
}

func hotPathMustJSON(v any) string {
	b, err := json.Marshal(v)
	if err != nil {
		return "{}"
	}
	return string(b)
}

// --- direct-claim helpers (probe only; not production admit) ---

type directClaimParts struct {
	begin, envelopeSpend, directOffer, pricing, contractBatch, commit, total time.Duration
}

func authorizeEnvelopeDirectClaim(
	ctx context.Context, store *Store, pool *pgxpool.Pool,
	buyerID, envelopeID uuid.UUID, worker WorkerAuth, profile VLLMRuntimeProfile,
	maxUSD, estUSD float64, maxPrompt, maxCompletion, needNanos int64,
) (uuid.UUID, error) {
	_, id, err := authorizeEnvelopeDirectClaimTimed(ctx, store, pool, buyerID, envelopeID,
		worker, profile, maxUSD, estUSD, maxPrompt, maxCompletion, needNanos)
	return id, err
}

func authorizeEnvelopeDirectClaimTimed(
	ctx context.Context, store *Store, pool *pgxpool.Pool,
	buyerID, envelopeID uuid.UUID, worker WorkerAuth, profile VLLMRuntimeProfile,
	maxUSD, estUSD float64, maxPrompt, maxCompletion, needNanos int64,
) (directClaimParts, uuid.UUID, error) {
	var parts directClaimParts
	t0 := time.Now()
	tx, err := pool.Begin(ctx)
	if err != nil {
		return parts, uuid.Nil, err
	}
	defer tx.Rollback(ctx)
	parts.begin = time.Since(t0)

	// 1) O(1) envelope spend (same production helper).
	t1 := time.Now()
	spendKey := "direct-" + uuid.NewString()
	reqID := "direct-req-" + uuid.NewString()
	inputCommitment := strings.Repeat("c", 64)
	requestSHA := strings.Repeat("d", 64)
	deadline := time.Now().Add(time.Minute)
	reserveTokens := maxPrompt + maxCompletion
	if reserveTokens < 0 {
		reserveTokens = 0
	}
	spend, err := reserveEnvelopeSpendTx(ctx, tx, envelopeID, buyerID, needNanos, reserveTokens,
		spendKey, reqID, profile)
	parts.envelopeSpend = time.Since(t1)
	if err != nil {
		return parts, uuid.Nil, fmt.Errorf("envelope spend: %w", err)
	}

	// 2) Buyer FOR KEY SHARE then O(1) direct offer claim by worker PK.
	// Hierarchy: money hold already taken via envelope; KEY SHARE before offer
	// prevents 40P01 against settlement (buyer then offer).
	t2 := time.Now()
	var buyerAlive bool
	if err := tx.QueryRow(ctx, `
		SELECT deleted_at IS NULL FROM buyers WHERE id=$1 FOR KEY SHARE`, buyerID).
		Scan(&buyerAlive); err != nil {
		return parts, uuid.Nil, err
	}
	if !buyerAlive {
		return parts, uuid.Nil, errNotFound
	}
	var (
		baseURL, sealed               string
		supplierInput, supplierOutput float64
		placementJSON                 []byte
		placementSHA256, warmth       string
		availableAfter                int
	)
	err = tx.QueryRow(ctx, `
		UPDATE realtime_worker_offers o
		   SET available_sequences = available_sequences - 1,
		       last_seen_at = now()
		 WHERE o.worker_id = $1
		   AND o.runtime_profile_id = $2
		   AND o.runtime_profile_sha256 = $3
		   AND o.status = 'ACTIVE'
		   AND o.available_sequences > 0
		   AND o.last_seen_at > now() - interval '45 seconds'
		 RETURNING o.upstream_base_url, o.upstream_token_sealed,
		           o.supplier_input_usd_per_million_tokens::float8,
		           o.supplier_output_usd_per_million_tokens::float8,
		           o.placement_plan, o.placement_plan_sha256, o.warmth,
		           o.available_sequences`,
		worker.WorkerID, profile.RuntimeProfileID, profile.ProfileSHA256,
	).Scan(&baseURL, &sealed, &supplierInput, &supplierOutput,
		&placementJSON, &placementSHA256, &warmth, &availableAfter)
	parts.directOffer = time.Since(t2)
	if err != nil {
		return parts, uuid.Nil, fmt.Errorf("direct offer claim: %w", err)
	}
	_ = availableAfter
	placementPlan, err := decodeRealtimePlacementPlan(placementJSON, placementSHA256)
	if err != nil {
		return parts, uuid.Nil, err
	}

	// 3) Pricing (CPU; not durable).
	t3 := time.Now()
	currency, err := SettlementCurrency()
	if err != nil {
		return parts, uuid.Nil, err
	}
	pricing, err := newRealtimePricingDecision(RealtimePricingInputs{
		Profile: profile, Placement: placementPlan,
		InputCommitment: inputCommitment, RequestSHA256: requestSHA,
		MaximumPromptTokens: maxPrompt, MaximumCompletionTokens: maxCompletion,
		EstimatedPromptTokens: 7, EstimatedCompletionTokens: 2,
		SupplierInputRate: supplierInput, SupplierOutputRate: supplierOutput,
		BuyerDeclaredCeiling: maxUSD * 1.1, Currency: currency,
	})
	parts.pricing = time.Since(t3)
	if err != nil {
		return parts, uuid.Nil, err
	}
	pricingJSON, err := json.Marshal(pricing)
	if err != nil {
		return parts, uuid.Nil, err
	}
	pricingSHA256, err := pricingDecisionDigest(pricing)
	if err != nil {
		return parts, uuid.Nil, err
	}
	inputNanos, err := nanoRatePerMillionFromFloat(supplierInput)
	if err != nil {
		return parts, uuid.Nil, err
	}
	outputNanos, err := nanoRatePerMillionFromFloat(supplierOutput)
	if err != nil {
		return parts, uuid.Nil, err
	}
	market, err := newRealtimeMarketClearingReceipt(
		1, 1, worker.WorkerID, worker.SupplierID, supplierInput, supplierOutput, pricing, pricingSHA256,
		buildRealtimeClearingRankingInputs(int64(inputNanos), int64(outputNanos), 0, 0, 0, 0, warmth))
	if err != nil {
		return parts, uuid.Nil, err
	}
	marketJSON, err := json.Marshal(market)
	if err != nil {
		return parts, uuid.Nil, err
	}

	// 4) Contract + event + bind spend.
	t4 := time.Now()
	contractID := uuid.New()
	batch := &pgx.Batch{}
	batch.Queue(`
		INSERT INTO execution_contracts
		 (id,request_id,buyer_id,workload_type,route,model_alias,runtime_profile_id,
		  runtime_profile_sha256,input_commitment,request_sha256,
		  placement_plan,placement_plan_sha256,maximum_price_usd,
		  estimated_price_usd,buyer_input_usd_per_million_tokens,
		  buyer_output_usd_per_million_tokens,supplier_input_usd_per_million_tokens,
		  supplier_output_usd_per_million_tokens,deadline_at,verification_tier,
		  idempotency_key,state,worker_id,supplier_id,upstream_base_url,upstream_token_sealed,
		  currency,maximum_prompt_tokens,maximum_completion_tokens,
		  estimated_prompt_tokens,estimated_completion_tokens,buyer_declared_ceiling_nanos,
		  pricing_decision,pricing_decision_sha256,market_clearing)
		VALUES ($1,$2,$3,'CHAT_COMPLETION','/v1/chat/completions',$4,$5,$6,$7,$8,$9,$10,
		        $11,$12,$13,$14,$15,$16,$17,'V0',$18,'EXECUTING',$19,$20,$21,$22,$23,
		        $24,$25,$26,$27,$28,$29,$30,$31)`,
		contractID, reqID, buyerID, profile.ModelAlias,
		profile.RuntimeProfileID, profile.ProfileSHA256,
		inputCommitment, requestSHA, placementJSON, placementSHA256,
		maxUSD, estUSD, profile.BuyerInputUSDPerMillionTokens, profile.BuyerOutputUSDPerMillionTokens,
		supplierInput, supplierOutput, deadline, spendKey,
		worker.WorkerID, worker.SupplierID, baseURL, sealed,
		SettlementCurrencyCode(), maxPrompt, maxCompletion, int64(7), int64(2),
		pricing.Realtime.BuyerDeclaredCeilingNanos, pricingJSON, pricingSHA256, marketJSON)
	batch.Queue(`
		INSERT INTO realtime_authorization_events (contract_id,kind,amount_usd)
		VALUES ($1,'RESERVED',$2)`, contractID, maxUSD)
	batch.Queue(`
		UPDATE execution_envelope_spends
		   SET contract_id=$2
		 WHERE id=$1 AND state='RESERVED' AND contract_id IS NULL`,
		spend.ID, contractID)
	br := tx.SendBatch(ctx, batch)
	if _, err = br.Exec(); err != nil {
		_ = br.Close()
		return parts, uuid.Nil, err
	}
	if _, err = br.Exec(); err != nil {
		_ = br.Close()
		return parts, uuid.Nil, err
	}
	ct, err := br.Exec()
	if err != nil {
		_ = br.Close()
		return parts, uuid.Nil, err
	}
	if ct.RowsAffected() != 1 {
		_ = br.Close()
		return parts, uuid.Nil, fmt.Errorf("bind spend rows=%d", ct.RowsAffected())
	}
	if err = br.Close(); err != nil {
		return parts, uuid.Nil, err
	}
	parts.contractBatch = time.Since(t4)

	t5 := time.Now()
	if err := tx.Commit(ctx); err != nil {
		return parts, uuid.Nil, err
	}
	parts.commit = time.Since(t5)
	parts.total = time.Since(t0)
	_ = store
	return parts, contractID, nil
}
