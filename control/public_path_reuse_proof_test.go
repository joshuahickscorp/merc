package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"
)

// Public-path work-elimination proof.
//
// The 128-to-1 arithmetic already lives in coalesced_cluster_money_test.go and
// the store-level follower loop. What this file closes is the unreceipted claim:
// a production HTTP caller (real routes, real auth, real money, no store
// shortcuts) must produce a sealed evidence artifact bound to a source commit,
// with every assertion listed separately so a run that delivers 128 bodies but
// cannot show one supplier payable has not proved the thing.
//
// Path under test: non-streaming POST /v1/chat/completions with deterministic
// sampling. Request 1 executes live; requests 2..128 hit the exact-result cache
// after the first result is stored. That is the production-wired exact reuse
// lane (realtime.go tryRealtimeExactReuse), not the disabled batch path that
// evidence/reuse/live-proof.json incorrectly claimed.

const publicPathReuseClusterSize = 128

// TestPublicPathExactReuse128To1WritesSealedReceipt drives 128 authenticated
// chat-completion requests through the public HTTP surface and writes
// evidence/reuse/public-path-128-to-1.json with integer-nanos money, counter
// deltas, and per-delivery receipt identifiers.
func TestPublicPathExactReuse128To1WritesSealedReceipt(t *testing.T) {
	installSettlementCurrencyForTest(t, "cad")
	installRealtimeCADFXForTest(t)
	t.Setenv("MERC_TOKEN_KEY", "public-path-reuse-proof-key-with-at-least-32-bytes")
	t.Setenv("STRIPE_SECRET_KEY", "")

	artifacts := newArtifactHarness(t)
	// Disposable database: the shared suite DB accumulates offers, contracts and
	// cache rows that deadlock or starve admission under sequential reuse load.
	ctx, store, pool := openIsolatedTestStore(t)

	suffix := uuid.NewString()
	buyerID, err := store.CreateBuyerAccount(ctx,
		"public-reuse-"+suffix+"@example.test", "integration-password", 50)
	must(t, err)
	_, buyerKey, _, err := store.CreateAPIKey(ctx, buyerID, "public-path reuse proof", true)
	must(t, err)
	supplierID := uuid.New()
	if _, err := pool.Exec(ctx, `INSERT INTO suppliers (id,email,status) VALUES ($1,$2,'active')`,
		supplierID, "public-reuse-supplier-"+suffix+"@example.test"); err != nil {
		t.Fatalf("seed supplier: %v", err)
	}
	workerID := uuid.New()
	if _, err := store.CreateWorkerToken(ctx, workerID, supplierID); err != nil {
		t.Fatalf("create worker: %v", err)
	}

	var upstreamCalls atomic.Int64
	completionBody, err := json.Marshal(map[string]any{
		"id":      "chatcmpl_public_reuse_" + suffix,
		"object":  "chat.completion",
		"created": 1,
		"model":   "cx-chat-1b",
		"choices": []any{map[string]any{
			"index": 0,
			"message": map[string]any{
				"role":    "assistant",
				"content": strings.Repeat("exact reuse public path ", 16),
			},
			"finish_reason": "stop",
		}},
		"usage": map[string]any{
			"prompt_tokens":     12,
			"completion_tokens": 64,
			"total_tokens":      76,
		},
	})
	mustf(t, err, "encode upstream completion: %v")
	upstreamToken := "public-reuse-upstream-token"
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/chat/completions" {
			t.Errorf("upstream path=%q", r.URL.Path)
			w.WriteHeader(http.StatusNotFound)
			return
		}
		if r.Header.Get("Authorization") != "Bearer "+upstreamToken {
			t.Errorf("upstream authorization was not forwarded")
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		upstreamCalls.Add(1)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(completionBody)
	}))
	defer upstream.Close()

	profile := sortedVLLMProfiles()[0]
	if err := store.UpsertRealtimeOffer(ctx, WorkerAuth{WorkerID: workerID, SupplierID: supplierID}, RealtimeOfferRegistration{
		RuntimeProfileID: profile.RuntimeProfileID, RuntimeProfileSHA256: profile.ProfileSHA256,
		HWClass: "nvidia_24gb", GPUCount: 1, MemoryGBPerGPU: 24,
		UpstreamBaseURL: upstream.URL + "/v1", UpstreamToken: upstreamToken,
		Warmth: "HOT", MaxActiveSequences: 8, AvailableSequences: 8,
		SupplierInputUSDPerMillionTokens: 0.08, SupplierOutputUSDPerMillionTokens: 0.30,
	}); err != nil {
		t.Fatalf("register realtime offer: %v", err)
	}

	server := NewServer(store, artifacts.storage, nil, nil)
	// One buyer, 128 sequential authenticated requests: raise the limiter so the
	// proof measures reuse rather than rate limiting.
	server.ipLimiter = newRateLimiter(10_000, publicPathReuseClusterSize*2)
	server.buyerLimiter = newRateLimiter(10_000, publicPathReuseClusterSize*2)
	control := httptest.NewServer(server.Routes())
	defer control.Close()

	requestBody := []byte(`{"model":"cx-chat-1b","messages":[{"role":"user","content":"public path exact reuse 128-to-1"}],"temperature":0,"top_p":1,"seed":42,"max_tokens":64}`)

	// Counter snapshot before the cluster so other suite activity is excluded
	// from the deltas this receipt records.
	beforeVerified := metrics.realtimeVerified.Load()
	beforeReused := metrics.realtimeReuseDeliveries.Load()
	beforeCoalesced := metrics.realtimeCoalescedDeliveries.Load()

	type delivery struct {
		status     int
		header     http.Header
		body       []byte
		contractID uuid.UUID
		reuseHit   bool
		coalesced  bool
	}
	deliveries := make([]delivery, 0, publicPathReuseClusterSize)

	deliver := func(i int) delivery {
		t.Helper()
		req, err := http.NewRequestWithContext(ctx, http.MethodPost,
			control.URL+"/v1/chat/completions", bytes.NewReader(requestBody))
		mustf(t, err, "request %d: build: %v", i)
		req.Header.Set("Authorization", "Bearer "+buyerKey)
		req.Header.Set("Content-Type", "application/json")
		// Distinct idempotency keys: reuse is not idempotency replay.
		req.Header.Set("Idempotency-Key", fmt.Sprintf("public-reuse-%03d-%s", i, uuid.NewString()))
		response, err := http.DefaultClient.Do(req)
		mustf(t, err, "request %d: do: %v", i)
		body, readErr := io.ReadAll(response.Body)
		response.Body.Close()
		if readErr != nil {
			t.Fatalf("request %d: read body: %v", i, readErr)
		}
		if response.StatusCode != http.StatusOK {
			t.Fatalf("request %d: status=%d body=%s", i, response.StatusCode, body)
		}
		if !bytes.Equal(body, completionBody) {
			t.Fatalf("request %d: body mismatch", i)
		}
		contractID, err := uuid.Parse(response.Header.Get("X-Merc-Contract-ID"))
		mustf(t, err, "request %d: missing contract id: %v", i)
		return delivery{
			status:     response.StatusCode,
			header:     response.Header.Clone(),
			body:       body,
			contractID: contractID,
			reuseHit:   response.Header.Get("X-Merc-Exact-Reuse") == "1",
			coalesced:  response.Header.Get("X-Merc-Coalesced") == "1",
		}
	}

	// Sequential: first physical, then cache hits. Concurrent arrivals would
	// exercise coalescing instead; that path already has its own production
	// proof. This receipt is specifically about exact-result reuse after a
	// durable cache entry exists.
	first := deliver(0)
	if first.reuseHit || first.coalesced {
		t.Fatalf("first request must be a physical execution; reuse=%v coalesced=%v",
			first.reuseHit, first.coalesced)
	}
	deliveries = append(deliveries, first)

	// Derive the same tenant-scoped identity the handler uses, then poll until
	// that row is durable. A raw table count would accept debris from other tests.
	preparedForID, err := prepareRealtimeRequest(requestBody, "")
	mustf(t, err, "prepare identity body: %v")
	clusterIdentity, err := realtimeIdentityFromPreparedBody(buyerID, preparedForID.Profile, preparedForID.Body)
	if err != nil || !ValidRequestIdentity(clusterIdentity) {
		t.Fatalf("tenant-scoped identity: id=%q err=%v", clusterIdentity, err)
	}
	// SELECT only — LookupExactResult bumps hits under a row lock and is the
	// production path under test, not the readiness probe.
	cacheDeadline := time.Now().Add(10 * time.Second)
	for {
		var ref string
		err := pool.QueryRow(ctx,
			`SELECT result_ref FROM exact_result_cache WHERE request_identity=$1`,
			clusterIdentity).Scan(&ref)
		if err == nil && ref != "" {
			break
		}
		if time.Now().After(cacheDeadline) {
			t.Fatal("physical execution did not populate exact_result_cache for this tenant identity")
		}
		time.Sleep(5 * time.Millisecond)
	}

	for i := 1; i < publicPathReuseClusterSize; i++ {
		got := deliver(i)
		if !got.reuseHit {
			t.Fatalf("request %d expected X-Merc-Exact-Reuse=1; coalesced=%v headers=%v",
				i, got.coalesced, got.header)
		}
		if got.coalesced {
			t.Fatalf("request %d took coalescing path; want sequential exact reuse", i)
		}
		deliveries = append(deliveries, got)
	}

	// Assertion: exactly 1 physical execution (upstream call).
	if got := upstreamCalls.Load(); got != 1 {
		t.Fatalf("physical executions (upstream calls)=%d want 1", got)
	}

	// Assertion: counter deltas — verified +1, reused +127, coalesced +0.
	deltaVerified := metrics.realtimeVerified.Load() - beforeVerified
	deltaReused := metrics.realtimeReuseDeliveries.Load() - beforeReused
	deltaCoalesced := metrics.realtimeCoalescedDeliveries.Load() - beforeCoalesced
	if deltaVerified != 1 {
		t.Fatalf("merc_realtime_contracts_verified_total delta=%d want 1", deltaVerified)
	}
	if deltaReused != publicPathReuseClusterSize-1 {
		t.Fatalf("merc_realtime_deliveries_reused_total delta=%d want %d",
			deltaReused, publicPathReuseClusterSize-1)
	}
	if deltaCoalesced != 0 {
		t.Fatalf("merc_realtime_deliveries_coalesced_total delta=%d want 0", deltaCoalesced)
	}

	// Metrics HTTP surface agrees with the process counters for this server.
	metricsResp, err := http.Get(control.URL + "/metrics")
	must(t, err)
	metricsBody, err := io.ReadAll(metricsResp.Body)
	metricsResp.Body.Close()
	if err != nil || metricsResp.StatusCode != http.StatusOK {
		t.Fatalf("metrics: status=%d err=%v", metricsResp.StatusCode, err)
	}
	for _, series := range []string{
		"merc_realtime_contracts_verified_total",
		"merc_realtime_deliveries_reused_total",
		"merc_realtime_deliveries_coalesced_total",
	} {
		if !bytes.Contains(metricsBody, []byte(series)) {
			t.Fatalf("/metrics missing %s", series)
		}
	}

	// 128 distinct delivery authorities via public receipt route.
	contractIDs := make(map[uuid.UUID]bool, publicPathReuseClusterSize)
	type deliveryReceipt struct {
		ContractID                 string `json:"contract_id"`
		ReceiptID                  string `json:"receipt_id"`
		State                      string `json:"state"`
		ExactReuse                 bool   `json:"exact_reuse"`
		BuyerChargeNanos           int64  `json:"buyer_charge_nanos"`
		SupplierPayableNanos       int64  `json:"supplier_payable_nanos"`
		KnownCostContributionNanos int64  `json:"known_cost_contribution_nanos"`
		SettlementCurrency         string `json:"settlement_currency"`
	}
	receipts := make([]deliveryReceipt, 0, publicPathReuseClusterSize)
	var physicalReceipts, reuseReceipts int
	var physicalSupplierPayableNanos int64
	var totalBuyerChargeNanos, totalSupplierPayableNanos, totalKnownContributionNanos int64
	var physicalBuyerChargeNanos int64

	for i, d := range deliveries {
		if contractIDs[d.contractID] {
			t.Fatalf("delivery %d reused contract authority %s", i, d.contractID)
		}
		contractIDs[d.contractID] = true

		// Public path only: fetch the receipt through the authenticated HTTP route.
		req, err := http.NewRequestWithContext(ctx, http.MethodGet,
			control.URL+"/v1/realtime/requests/"+d.contractID.String()+"/receipt", nil)
		mustf(t, err, "receipt %d: build: %v", i)
		req.Header.Set("Authorization", "Bearer "+buyerKey)
		resp, err := http.DefaultClient.Do(req)
		mustf(t, err, "receipt %d: do: %v", i)
		raw, readErr := io.ReadAll(resp.Body)
		resp.Body.Close()
		if readErr != nil {
			t.Fatalf("receipt %d: read: %v", i, readErr)
		}
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("receipt %d: status=%d body=%s", i, resp.StatusCode, raw)
		}
		var receipt RealtimeReceipt
		mustf(t, json.Unmarshal(raw, &receipt), "receipt %d: decode: %v", i)
		if receipt.ContractID != d.contractID.String() || receipt.State != "VERIFIED" ||
			receipt.Verification != "PASSED" || receipt.PricingAuthorityStatus != "verified" ||
			receipt.SettlementCurrency != "cad" || receipt.PricingDecision == nil {
			t.Fatalf("receipt %d invalid: %+v", i, receipt)
		}
		// Existence-leak surface on the receipt itself: no field may name another
		// buyer's request identity or assert a shared global cache hit.
		rawLower := strings.ToLower(string(raw))
		for _, banned := range []string{
			"other_buyer", "cross_tenant", "global_cache_hit", "shared_cache",
		} {
			if strings.Contains(rawLower, banned) {
				t.Fatalf("receipt %d leaks existence language %q", i, banned)
			}
		}

		entry := deliveryReceipt{
			ContractID:                 receipt.ContractID,
			ReceiptID:                  receipt.ReceiptID,
			State:                      receipt.State,
			ExactReuse:                 d.reuseHit,
			BuyerChargeNanos:           receipt.BuyerChargeNanos,
			SupplierPayableNanos:       receipt.SupplierPayableNanos,
			KnownCostContributionNanos: receipt.KnownCostContributionNanos,
			SettlementCurrency:         receipt.SettlementCurrency,
		}
		receipts = append(receipts, entry)
		totalBuyerChargeNanos += receipt.BuyerChargeNanos
		totalSupplierPayableNanos += receipt.SupplierPayableNanos
		totalKnownContributionNanos += receipt.KnownCostContributionNanos

		if d.reuseHit {
			// Assertion: reused delivery — buyer discount path, zero supplier payable.
			if receipt.PricingDecision.RealtimeReuse == nil ||
				receipt.PricingDecision.RealtimeReuse.ReuseClass != ClassExactResultReuse {
				t.Fatalf("reuse receipt %d lost exact-result authority: %+v", i, receipt.PricingDecision)
			}
			if receipt.SupplierPayableNanos != 0 {
				t.Fatalf("reuse receipt %d supplier payable=%d want 0", i, receipt.SupplierPayableNanos)
			}
			if receipt.BuyerChargeNanos <= 0 || receipt.KnownCostContributionNanos <= 0 {
				t.Fatalf("reuse receipt %d must charge buyer and contribute: %+v", i, receipt)
			}
			reuseReceipts++
			continue
		}
		if receipt.PricingDecision.Realtime == nil {
			t.Fatalf("physical receipt %d missing realtime pricing: %+v", i, receipt.PricingDecision)
		}
		if receipt.SupplierPayableNanos <= 0 || receipt.KnownCostContributionNanos <= 0 {
			t.Fatalf("physical receipt %d lacks payable or contribution: %+v", i, receipt)
		}
		physicalReceipts++
		physicalSupplierPayableNanos = receipt.SupplierPayableNanos
		physicalBuyerChargeNanos = receipt.BuyerChargeNanos
	}

	// Assertion: 128 independent deliveries / receipts.
	if len(contractIDs) != publicPathReuseClusterSize ||
		physicalReceipts != 1 || reuseReceipts != publicPathReuseClusterSize-1 {
		t.Fatalf("receipts physical=%d reuse=%d distinct=%d",
			physicalReceipts, reuseReceipts, len(contractIDs))
	}

	// Assertion: buyer discount on reused deliveries (strictly less than physical).
	for i, r := range receipts {
		if !r.ExactReuse {
			continue
		}
		if r.BuyerChargeNanos >= physicalBuyerChargeNanos {
			t.Fatalf("reuse receipt %d charge %d not less than physical %d",
				i, r.BuyerChargeNanos, physicalBuyerChargeNanos)
		}
	}

	// Assertion: exactly 1 supplier payable (ledger rows, not only receipt fields).
	var supplierRows, buyerRows, platformRows int
	var buyerMicros, supplierMicros, platformMicros int64
	if err := pool.QueryRow(ctx, `
		SELECT
		  count(*) FILTER (WHERE kind='supplier_credit'),
		  count(*) FILTER (WHERE kind='buyer_charge'),
		  count(*) FILTER (WHERE kind='platform_take'),
		  COALESCE((-sum(amount_usd) FILTER (WHERE kind='buyer_charge')*1000000)::bigint,0),
		  COALESCE((sum(amount_usd) FILTER (WHERE kind='supplier_credit')*1000000)::bigint,0),
		  COALESCE((sum(amount_usd) FILTER (WHERE kind='platform_take')*1000000)::bigint,0)
		FROM ledger_entries WHERE execution_contract_id = ANY($1)`, authorityIDs(contractIDs)).
		Scan(&supplierRows, &buyerRows, &platformRows, &buyerMicros, &supplierMicros, &platformMicros); err != nil {
		t.Fatalf("read cluster ledger: %v", err)
	}
	if supplierRows != 1 || totalSupplierPayableNanos != physicalSupplierPayableNanos {
		t.Fatalf("supplier payable rows=%d total_nanos=%d physical_nanos=%d",
			supplierRows, totalSupplierPayableNanos, physicalSupplierPayableNanos)
	}
	if buyerRows != publicPathReuseClusterSize || platformRows != publicPathReuseClusterSize {
		t.Fatalf("ledger buyer_rows=%d platform_rows=%d want %d each",
			buyerRows, platformRows, publicPathReuseClusterSize)
	}
	if buyerMicros != supplierMicros+platformMicros || supplierMicros <= 0 || platformMicros <= 0 {
		t.Fatalf("cluster ledger not conserved: buyer=%d supplier=%d platform=%d micros",
			buyerMicros, supplierMicros, platformMicros)
	}
	// Assertion: positive merc net contribution across the whole cluster.
	if totalKnownContributionNanos <= 0 || platformMicros <= 0 {
		t.Fatalf("merc contribution not positive: nanos=%d micros=%d",
			totalKnownContributionNanos, platformMicros)
	}

	// Worker-backed executions: exactly one of the 128 contract rows has a worker.
	var workerBacked, logicalDeliveries int
	if err := pool.QueryRow(ctx, `
		SELECT
		  (SELECT count(*) FROM realtime_executions WHERE contract_id = ANY($1) AND worker_id IS NOT NULL),
		  (SELECT count(*) FROM realtime_executions WHERE contract_id = ANY($1) AND worker_id IS NULL)`,
		authorityIDs(contractIDs)).Scan(&workerBacked, &logicalDeliveries); err != nil {
		t.Fatalf("count execution modes: %v", err)
	}
	if workerBacked != 1 || logicalDeliveries != publicPathReuseClusterSize-1 {
		t.Fatalf("execution modes worker_backed=%d logical=%d", workerBacked, logicalDeliveries)
	}

	identity := clusterIdentity

	// ReuseRatio on pure reuse: physical is zero, ratio must be 0 (never +Inf).
	currency := MustParseCurrency("cad")
	money, err := SettleRealtimeReuseHitMoney(currency, 64,
		profile.BuyerInputUSDPerMillionTokens, profile.BuyerOutputUSDPerMillionTokens)
	must(t, err)
	if money.PhysicalTokens != 0 || money.Accounting.ReuseRatio() != 0 {
		t.Fatalf("reuse settlement must report physical=0 and reuse_ratio=0: %+v ratio=%v",
			money, money.Accounting.ReuseRatio())
	}

	t.Logf("public-path exact reuse: 1 physical upstream call : %d deliveries; verified_delta=%d reused_delta=%d coalesced_delta=%d; buyer=%d supplier=%d platform=%d micros",
		publicPathReuseClusterSize, deltaVerified, deltaReused, deltaCoalesced, buyerMicros, supplierMicros, platformMicros)

	// Opt-in sealed receipt. A verification suite run must not dirty tracked evidence.
	// Set MERC_PUBLIC_PATH_REUSE_PROOF=1 to seal evidence/reuse/public-path-128-to-1.json.
	if os.Getenv("MERC_PUBLIC_PATH_REUSE_PROOF") != "1" {
		t.Logf("skipping evidence write (set MERC_PUBLIC_PATH_REUSE_PROOF=1 to seal)")
		return
	}
	measuredAt := time.Now().UTC().Format(time.RFC3339)
	out := map[string]any{
		"schema_version": 1,
		"kind":           "public_path_exact_reuse_128_to_1",
		"measured_at":    measuredAt,
		"headline":       fmt.Sprintf("1 physical upstream call : %d deliveries", publicPathReuseClusterSize),
		"path": map[string]any{
			"route":           "POST /v1/chat/completions",
			"receipt_route":   "GET /v1/realtime/requests/{id}/receipt",
			"metrics_route":   "GET /metrics",
			"authentication":  "buyer API key (Authorization: Bearer)",
			"stream":          false,
			"mechanism":       "exact_result_cache after one physical execution",
			"not_the_path":    "control/exact_reuse_batch.go (batchExactReuseEnabled=false)",
			"upstream_double": true,
			"stripe":          "disabled (no live payment rail)",
		},
		"cluster": map[string]any{
			"public_path_requests":                publicPathReuseClusterSize,
			"physical_executions":                 1,
			"upstream_calls":                      upstreamCalls.Load(),
			"supplier_payable_count":              1,
			"supplier_payable_nanos":              physicalSupplierPayableNanos,
			"independent_deliveries":              publicPathReuseClusterSize,
			"valid_receipts":                      len(receipts),
			"exact_reuse_deliveries":              reuseReceipts,
			"physical_deliveries":                 physicalReceipts,
			"buyer_charge_nanos_total":            totalBuyerChargeNanos,
			"supplier_payable_nanos_total":        totalSupplierPayableNanos,
			"known_cost_contribution_nanos_total": totalKnownContributionNanos,
			"ledger_buyer_micros":                 buyerMicros,
			"ledger_supplier_micros":              supplierMicros,
			"ledger_platform_micros":              platformMicros,
			"settlement_currency":                 "cad",
			"physical_buyer_charge_nanos":         physicalBuyerChargeNanos,
			"request_identity_prefix":             identity[:min(12, len(identity))],
			"tenant_scoped_identity":              true,
		},
		"counter_deltas": map[string]any{
			"merc_realtime_contracts_verified_total":   deltaVerified,
			"merc_realtime_deliveries_reused_total":    deltaReused,
			"merc_realtime_deliveries_coalesced_total": deltaCoalesced,
		},
		"assertions": map[string]bool{
			"public_path_requests_128":                   true,
			"exactly_one_physical_execution":             upstreamCalls.Load() == 1,
			"exactly_one_supplier_payable":               supplierRows == 1 && totalSupplierPayableNanos == physicalSupplierPayableNanos,
			"128_independent_deliveries":                 len(contractIDs) == publicPathReuseClusterSize,
			"128_valid_individually_verifiable_receipts": len(receipts) == publicPathReuseClusterSize,
			"buyer_discount_on_reused_deliveries":        true,
			"positive_merc_net_contribution":             totalKnownContributionNanos > 0 && platformMicros > 0,
			"tenant_scoped_request_identity":             true,
			"no_private_input_existence_leak":            true, // headers/receipts; see existence_leak
			"verified_counter_plus_one":                  deltaVerified == 1,
			"reused_counter_plus_127":                    deltaReused == publicPathReuseClusterSize-1,
			"coalesced_counter_unchanged":                deltaCoalesced == 0,
			"reuse_ratio_zero_on_zero_physical":          money.Accounting.ReuseRatio() == 0,
		},
		"existence_leak": map[string]any{
			"design": "RequestIdentity.TenantScope is hashed into the durable cache key " +
				"(refused when empty). A second buyer submitting byte-identical input " +
				"computes a different key and therefore cannot observe a hit, a reuse " +
				"header, a reuse-class receipt, or a reuse price from another tenant's " +
				"prior submission. See TestPublicPathExactReuseDoesNotCrossTenants.",
			"checked_in_this_run": []string{
				"no X-Merc-Exact-Reuse on physical delivery",
				"reuse receipts carry ClassExactResultReuse only for this buyer",
				"receipt JSON has no cross-tenant cache-existence language",
			},
			"timing": "Same-tenant cache hits are faster than physical execution by design; " +
				"that is not a cross-tenant leak. Cross-tenant identical input always " +
				"misses and executes live (asserted in the tenant isolation test). " +
				"No constant-time guarantee is claimed for same-tenant hit vs miss.",
		},
		"deliveries": receipts,
		"proves": []string{
			"128 authenticated public-path chat completions collapse to one physical upstream execution",
			fmt.Sprintf("headline ratio is 1 physical upstream call : %d deliveries (not a price-book cost ratio)", publicPathReuseClusterSize),
			"exactly one supplier_credit ledger row and one positive supplier payable in nanos",
			"128 distinct VERIFIED receipts fetchable via GET /v1/realtime/requests/{id}/receipt",
			"127 exact-reuse deliveries charge the buyer less than the physical delivery",
			"cluster platform take / known-cost contribution is strictly positive",
			"merc_realtime_contracts_verified_total moves only for the physical execution",
			"merc_realtime_deliveries_reused_total moves for each cache hit; coalesced does not",
			"TokenAccounting.ReuseRatio returns 0 on pure reuse (never infinite throughput)",
		},
		"does_not_prove": []string{
			"physical vLLM / GPU performance or TTFT against a real engine",
			"live Stripe or external payment rail behaviour",
			"batch exact reuse (batchExactReuseEnabled remains false)",
			"cross-tenant timing side channels under adversarial RTT measurement",
			"streaming (stream:true) exact reuse — deliberately excluded from this path",
			"a cost ratio derived from Merc's price book (price/price is not a measurement)",
			"in-flight coalescing of concurrent arrivals (see public-path-coalescing-128-to-1)",
		},
		"supersedes": map[string]any{
			"artifact": "evidence/reuse/live-proof.json",
			"reasons": []string{
				"batchExactReuseEnabled is compile-time false in control/exact_reuse_batch.go:21, so the batch path cannot settle a live cache hit",
				"SubmitExactReuseBatchJob requires workload batch_infer while the old artifact described embed jobs",
				"the historical batch identity build omitted TenantScope, which RequestIdentity.Compute refuses (control/exact_reuse.go:110-112), so the key was empty even if the flag were flipped",
			},
		},
	}

	id, bin, err := DefaultBoundIdentity("..",
		"control/public_path_reuse_proof_test.go#TestPublicPathExactReuse128To1WritesSealedReceipt",
		"temperature=0 top_p=1 seed=42 max_tokens=64 stream=false sequential 128 same body",
		"embedded deliveries[] + counter_deltas + ledger micros")
	must(t, err)
	outPath := filepath.Join("..", "evidence", "reuse", "public-path-128-to-1.json")
	if err := WriteBoundEvidenceJSON(EvidenceWriteRequest{
		RepoRoot: "..", Path: outPath, Payload: out,
		Identity: id, BuildBinaryPath: bin,
	}); err != nil {
		t.Fatalf("bound evidence write refused: %v", err)
	}
	t.Logf("wrote %s (verified_delta=%d reused_delta=%d coalesced_delta=%d physical_supplier_nanos=%d)",
		outPath, deltaVerified, deltaReused, deltaCoalesced, physicalSupplierPayableNanos)
}

// TestPublicPathExactReuseDoesNotCrossTenants proves two buyers with
// byte-identical input do not share a cache entry, through the public HTTP
// path only. This is the existence-leak close: buyer B must not learn that
// buyer A already ran the request via a reuse header, reuse price, or receipt.
func TestPublicPathExactReuseDoesNotCrossTenants(t *testing.T) {
	installSettlementCurrencyForTest(t, "cad")
	installRealtimeCADFXForTest(t)
	t.Setenv("MERC_TOKEN_KEY", "public-path-tenant-isolation-key-with-32b!")
	t.Setenv("STRIPE_SECRET_KEY", "")

	artifacts := newArtifactHarness(t)
	ctx, store, pool := openIsolatedTestStore(t)

	suffix := uuid.NewString()
	makeBuyer := func(label string) (uuid.UUID, string) {
		t.Helper()
		id, err := store.CreateBuyerAccount(ctx,
			label+"-"+suffix+"@example.test", "integration-password", 20)
		must(t, err)
		_, key, _, err := store.CreateAPIKey(ctx, id, label+" key", true)
		must(t, err)
		return id, key
	}
	buyerA, keyA := makeBuyer("tenant-a")
	buyerB, keyB := makeBuyer("tenant-b")

	supplierID := uuid.New()
	if _, err := pool.Exec(ctx, `INSERT INTO suppliers (id,email,status) VALUES ($1,$2,'active')`,
		supplierID, "tenant-isolation-supplier-"+suffix+"@example.test"); err != nil {
		t.Fatalf("seed supplier: %v", err)
	}
	workerID := uuid.New()
	if _, err := store.CreateWorkerToken(ctx, workerID, supplierID); err != nil {
		t.Fatalf("create worker: %v", err)
	}

	var upstreamCalls atomic.Int64
	completionBody, err := json.Marshal(map[string]any{
		"id":      "chatcmpl_tenant_" + suffix,
		"object":  "chat.completion",
		"created": 1,
		"model":   "cx-chat-1b",
		"choices": []any{map[string]any{
			"index":         0,
			"message":       map[string]any{"role": "assistant", "content": "tenant isolation body"},
			"finish_reason": "stop",
		}},
		"usage": map[string]any{"prompt_tokens": 8, "completion_tokens": 16, "total_tokens": 24},
	})
	must(t, err)
	upstreamToken := "tenant-isolation-upstream"
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		upstreamCalls.Add(1)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(completionBody)
	}))
	defer upstream.Close()

	profile := sortedVLLMProfiles()[0]
	if err := store.UpsertRealtimeOffer(ctx, WorkerAuth{WorkerID: workerID, SupplierID: supplierID}, RealtimeOfferRegistration{
		RuntimeProfileID: profile.RuntimeProfileID, RuntimeProfileSHA256: profile.ProfileSHA256,
		HWClass: "nvidia_24gb", GPUCount: 1, MemoryGBPerGPU: 24,
		UpstreamBaseURL: upstream.URL + "/v1", UpstreamToken: upstreamToken,
		Warmth: "HOT", MaxActiveSequences: 4, AvailableSequences: 4,
		SupplierInputUSDPerMillionTokens: 0.08, SupplierOutputUSDPerMillionTokens: 0.30,
	}); err != nil {
		t.Fatalf("register offer: %v", err)
	}

	server := NewServer(store, artifacts.storage, nil, nil)
	server.ipLimiter = newRateLimiter(10_000, 64)
	server.buyerLimiter = newRateLimiter(10_000, 64)
	control := httptest.NewServer(server.Routes())
	defer control.Close()

	// Byte-identical input from both tenants — the existence-leak probe.
	sharedBody := []byte(`{"model":"cx-chat-1b","messages":[{"role":"user","content":"secret probe shared by two tenants"}],"temperature":0,"top_p":1,"seed":7,"max_tokens":32}`)

	type chatResult struct {
		status    int
		reuse     bool
		coalesced bool
		contract  uuid.UUID
		body      []byte
	}
	chat := func(key string, idem string) chatResult {
		t.Helper()
		req, err := http.NewRequestWithContext(ctx, http.MethodPost,
			control.URL+"/v1/chat/completions", bytes.NewReader(sharedBody))
		must(t, err)
		req.Header.Set("Authorization", "Bearer "+key)
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Idempotency-Key", idem)
		resp, err := http.DefaultClient.Do(req)
		must(t, err)
		body, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("chat status=%d body=%s", resp.StatusCode, body)
		}
		id, err := uuid.Parse(resp.Header.Get("X-Merc-Contract-ID"))
		mustf(t, err, "contract id: %v")
		return chatResult{
			status:    resp.StatusCode,
			reuse:     resp.Header.Get("X-Merc-Exact-Reuse") == "1",
			coalesced: resp.Header.Get("X-Merc-Coalesced") == "1",
			contract:  id,
			body:      body,
		}
	}

	// Buyer A executes and populates the cache under A's tenant scope.
	a1 := chat(keyA, "tenant-a-1-"+uuid.NewString())
	if a1.reuse || a1.coalesced {
		t.Fatalf("buyer A first request must be physical; reuse=%v coalesced=%v", a1.reuse, a1.coalesced)
	}
	preparedShared, err := prepareRealtimeRequest(sharedBody, "")
	must(t, err)
	idA, err := realtimeIdentityFromPreparedBody(buyerA, preparedShared.Profile, preparedShared.Body)
	must(t, err)
	idB, err := realtimeIdentityFromPreparedBody(buyerB, preparedShared.Profile, preparedShared.Body)
	must(t, err)
	if idA == "" || idB == "" || idA == idB {
		t.Fatalf("tenant-scoped identities must differ: a=%q b=%q", idA, idB)
	}
	cacheDeadline := time.Now().Add(10 * time.Second)
	for {
		var ref string
		err := pool.QueryRow(ctx,
			`SELECT result_ref FROM exact_result_cache WHERE request_identity=$1`, idA).Scan(&ref)
		if err == nil && ref != "" {
			break
		}
		if time.Now().After(cacheDeadline) {
			t.Fatal("buyer A did not populate exact_result_cache under A's identity")
		}
		time.Sleep(5 * time.Millisecond)
	}
	// Buyer B must still miss on B's identity before B runs (prove isolation at
	// the store boundary as well as on the HTTP header surface).
	var bRef string
	err = pool.QueryRow(ctx,
		`SELECT result_ref FROM exact_result_cache WHERE request_identity=$1`, idB).Scan(&bRef)
	if err == nil {
		t.Fatalf("buyer B identity unexpectedly present before B ran: ref=%s", bRef)
	}
	// Same tenant, same input → reuse hit (proves the cache works).
	a2 := chat(keyA, "tenant-a-2-"+uuid.NewString())
	if !a2.reuse {
		t.Fatal("buyer A second identical request did not hit exact reuse")
	}

	callsAfterA := upstreamCalls.Load()
	if callsAfterA != 1 {
		t.Fatalf("buyer A cluster upstream calls=%d want 1", callsAfterA)
	}

	// Buyer B, byte-identical input: must NOT observe A's cache.
	b1 := chat(keyB, "tenant-b-1-"+uuid.NewString())
	if b1.reuse {
		t.Fatal("existence leak: buyer B received X-Merc-Exact-Reuse for buyer A's prior input")
	}
	if b1.coalesced {
		t.Fatal("existence leak: buyer B coalesced onto buyer A's execution")
	}
	if upstreamCalls.Load() != 2 {
		t.Fatalf("buyer B must force a second physical execution; upstream=%d", upstreamCalls.Load())
	}

	// B's public receipt must show a physical supplier payable, not reuse class.
	req, err := http.NewRequestWithContext(ctx, http.MethodGet,
		control.URL+"/v1/realtime/requests/"+b1.contract.String()+"/receipt", nil)
	must(t, err)
	req.Header.Set("Authorization", "Bearer "+keyB)
	resp, err := http.DefaultClient.Do(req)
	must(t, err)
	raw, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("buyer B receipt status=%d body=%s", resp.StatusCode, raw)
	}
	var bReceipt RealtimeReceipt
	must(t, json.Unmarshal(raw, &bReceipt))
	if bReceipt.PricingDecision == nil || bReceipt.PricingDecision.Realtime == nil {
		t.Fatalf("buyer B first receipt must be physical realtime, not reuse: %+v", bReceipt.PricingDecision)
	}
	if bReceipt.PricingDecision.RealtimeReuse != nil {
		t.Fatal("existence leak: buyer B receipt carries RealtimeReuse after A's prior input")
	}
	if bReceipt.SupplierPayableNanos <= 0 {
		t.Fatalf("buyer B physical receipt has no supplier payable: %+v", bReceipt)
	}

	// B cannot read A's receipt (cross-tenant read refuse).
	reqA, err := http.NewRequestWithContext(ctx, http.MethodGet,
		control.URL+"/v1/realtime/requests/"+a1.contract.String()+"/receipt", nil)
	must(t, err)
	reqA.Header.Set("Authorization", "Bearer "+keyB)
	respA, err := http.DefaultClient.Do(reqA)
	must(t, err)
	bodyA, _ := io.ReadAll(respA.Body)
	respA.Body.Close()
	if respA.StatusCode == http.StatusOK {
		t.Fatalf("existence leak: buyer B read buyer A's receipt: %s", bodyA)
	}

	// After B's own physical run, B's second identical request reuses B's cache.
	cacheDeadline = time.Now().Add(10 * time.Second)
	for {
		var ref string
		err := pool.QueryRow(ctx,
			`SELECT result_ref FROM exact_result_cache WHERE request_identity=$1`, idB).Scan(&ref)
		if err == nil && ref != "" {
			break
		}
		if time.Now().After(cacheDeadline) {
			t.Fatal("buyer B did not populate exact_result_cache under B's identity")
		}
		time.Sleep(5 * time.Millisecond)
	}
	b2 := chat(keyB, "tenant-b-2-"+uuid.NewString())
	if !b2.reuse {
		t.Fatal("buyer B second identical request should hit B's own exact-reuse cache")
	}
	if upstreamCalls.Load() != 2 {
		t.Fatalf("buyer B reuse must not call upstream again; upstream=%d", upstreamCalls.Load())
	}
}
