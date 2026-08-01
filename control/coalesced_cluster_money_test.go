package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"
)

// One execution, 128 deliveries: the money read out of the ledger rather than out
// of the pricing function.
//
// inflight_coalescing_test.go already proves the arithmetic (a follower's supplier
// liability is zero, Merc's contribution is positive, the buyer pays less than a
// fresh execution) and that 128 concurrent claims elect exactly one leader. What
// it does not do is put 128 deliveries through the store and then count what the
// ledger holds — and the arithmetic being right is not the same claim as the
// persistence being right. A settlement path that double-credited a supplier, or
// that reused one delivery authority for every follower, would pass every
// assertion in that file.
//
// What this proves, at store level:
//
//   - 128 followers produce 128 DISTINCT delivery authorities and 128 receipts;
//   - not one of them writes a supplier credit;
//   - every one of them charges the buyer less than a fresh execution;
//   - every follower has positive known/gross contribution, because storage,
//     lookup and delivery are real costs and a follower must not be free. This
//     is deliberately not called true net: processor, storage, egress, risk and
//     control-plane actuals remain separate, named cost authorities;
//   - the ledger conserves per follower — buyer debit equals platform take when
//     no supplier is owed;
//   - two tenants asking for the identical thing never share an authority.
//
// The production HTTP proof above completes the other half: one worker-backed
// leader settlement and its 127 in-flight followers. This store-level test
// remains useful because it pins the follower money and tenant-isolation
// invariants directly at the durable write boundary.
const coalescedFollowers = 128

// TestProductionRealtimeCoalescing128DeliveriesOnePhysicalSettlement closes
// the seam the store-level test below intentionally leaves open: the first
// request goes through Server.handleChatCompletions, FinalizeRealtimeSuccess,
// object storage, and ResolveInflightSuccess before 127 HTTP followers charge
// their own coalesced-delivery authorities.  It uses a pinned, isolated MinIO
// sidecar and a disposable database, never a shared candidate or live payment
// rail.  The upstream is an HTTP contract double, so this is proof of the
// control-plane money/receipt path, not a claim of physical vLLM performance.
func TestProductionRealtimeCoalescing128DeliveriesOnePhysicalSettlement(t *testing.T) {
	installSettlementCurrencyForTest(t, "cad")
	t.Setenv("MERC_TOKEN_KEY", "coalesced-realtime-test-key-with-at-least-32-bytes")
	t.Setenv("STRIPE_SECRET_KEY", "")

	artifacts := newArtifactHarness(t)
	ctx, store, pool := openPayoutTestStore(t)

	suffix := uuid.NewString()
	buyerID, err := store.CreateBuyerAccount(ctx,
		"coalesced-"+suffix+"@example.test", "integration-password", 5)
	if err != nil {
		t.Fatal(err)
	}
	_, buyerKey, _, err := store.CreateAPIKey(ctx, buyerID, "128 coalescing integration", true)
	if err != nil {
		t.Fatal(err)
	}
	supplierID := uuid.New()
	if _, err := pool.Exec(ctx, `INSERT INTO suppliers (id,email,status) VALUES ($1,$2,'active')`,
		supplierID, "coalesced-supplier-"+suffix+"@example.test"); err != nil {
		t.Fatalf("seed supplier: %v", err)
	}
	workerID := uuid.New()
	if _, err := store.CreateWorkerToken(ctx, workerID, supplierID); err != nil {
		t.Fatalf("create worker: %v", err)
	}

	var upstreamCalls atomic.Int64
	upstreamEntered := make(chan struct{})
	releaseUpstream := make(chan struct{})
	var releaseOnce sync.Once
	defer releaseOnce.Do(func() { close(releaseUpstream) })

	completionBody, err := json.Marshal(map[string]any{
		"id":      "chatcmpl_coalesced_" + suffix,
		"object":  "chat.completion",
		"created": 1,
		"model":   "cx-chat-1b",
		"choices": []any{map[string]any{
			"index": 0,
			"message": map[string]any{
				"role":    "assistant",
				"content": strings.Repeat("one physical completion ", 16),
			},
			"finish_reason": "stop",
		}},
		"usage": map[string]any{
			"prompt_tokens":     12,
			"completion_tokens": 64,
			"total_tokens":      76,
		},
	})
	if err != nil {
		t.Fatalf("encode upstream completion: %v", err)
	}
	upstreamToken := "coalesced-upstream-token"
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
		if upstreamCalls.Add(1) == 1 {
			close(upstreamEntered)
		}
		<-releaseUpstream
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(completionBody)
	}))
	defer upstream.Close()

	profile := sortedVLLMProfiles()[0]
	if err := store.UpsertRealtimeOffer(ctx, WorkerAuth{WorkerID: workerID, SupplierID: supplierID}, RealtimeOfferRegistration{
		RuntimeProfileID: profile.RuntimeProfileID, RuntimeProfileSHA256: profile.ProfileSHA256,
		HWClass: "nvidia_24gb", GPUCount: 1, MemoryGBPerGPU: 24,
		UpstreamBaseURL: upstream.URL + "/v1", UpstreamToken: upstreamToken,
		Warmth: "HOT", MaxActiveSequences: coalescedFollowers, AvailableSequences: coalescedFollowers,
		SupplierInputUSDPerMillionTokens: 0.08, SupplierOutputUSDPerMillionTokens: 0.30,
	}); err != nil {
		t.Fatalf("register realtime offer: %v", err)
	}

	server := NewServer(store, artifacts.storage, nil, nil)
	// The production limit protects an Internet-facing key.  This isolated proof
	// deliberately issues 128 simultaneous requests from one authenticated test
	// buyer, so make the test server's limiter large enough to test coalescing
	// rather than rate limiting.
	server.ipLimiter = newRateLimiter(10_000, coalescedFollowers*2)
	server.buyerLimiter = newRateLimiter(10_000, coalescedFollowers*2)
	control := httptest.NewServer(server.Routes())
	defer control.Close()

	requestBody := []byte(`{"model":"cx-chat-1b","messages":[{"role":"user","content":"prove one physical execution"}],"temperature":0,"top_p":1,"seed":42,"max_tokens":64}`)
	type delivery struct {
		status int
		header http.Header
		body   []byte
		err    error
	}
	deliveries := make(chan delivery, coalescedFollowers)
	deliver := func() {
		req, err := http.NewRequestWithContext(ctx, http.MethodPost,
			control.URL+"/v1/chat/completions", bytes.NewReader(requestBody))
		if err != nil {
			deliveries <- delivery{err: err}
			return
		}
		req.Header.Set("Authorization", "Bearer "+buyerKey)
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Idempotency-Key", "coalesced-"+uuid.NewString())
		response, err := http.DefaultClient.Do(req)
		if err != nil {
			deliveries <- delivery{err: err}
			return
		}
		body, readErr := io.ReadAll(response.Body)
		response.Body.Close()
		deliveries <- delivery{status: response.StatusCode, header: response.Header, body: body, err: readErr}
	}

	// Start exactly one leader, then wait until it has reached the upstream.  No
	// result exists at this point, so every request released below must take the
	// in-flight follower path rather than the later exact-result cache path.
	go deliver()
	select {
	case <-upstreamEntered:
	case <-time.After(10 * time.Second):
		t.Fatal("leader did not reach upstream")
	}

	startFollowers := make(chan struct{})
	for range coalescedFollowers - 1 {
		go func() {
			<-startFollowers
			deliver()
		}()
	}
	close(startFollowers)

	// Count the DB-backed follower claims before permitting the only physical
	// execution to return.  A sleep here would only make the test look reliable;
	// this waits for the state the production handler actually publishes.
	followersDeadline := time.Now().Add(15 * time.Second)
	for {
		var count int64
		if err := pool.QueryRow(ctx, `SELECT COALESCE(sum(followers),0) FROM inflight_executions`).Scan(&count); err != nil {
			t.Fatalf("count inflight followers: %v", err)
		}
		if count == coalescedFollowers-1 {
			break
		}
		if time.Now().After(followersDeadline) {
			t.Fatalf("only %d of %d callers joined the in-flight leader", count, coalescedFollowers-1)
		}
		time.Sleep(10 * time.Millisecond)
	}
	releaseOnce.Do(func() { close(releaseUpstream) })

	contractIDs := make(map[uuid.UUID]bool, coalescedFollowers)
	leaderContracts := 0
	followerContracts := 0
	for range coalescedFollowers {
		got := <-deliveries
		if got.err != nil {
			t.Fatalf("chat delivery failed: %v", got.err)
		}
		if got.status != http.StatusOK || !bytes.Equal(got.body, completionBody) {
			t.Fatalf("chat delivery status=%d body=%s", got.status, got.body)
		}
		if got.header.Get("X-Merc-Exact-Reuse") != "" {
			t.Fatal("overlapping request took the exact-result cache path")
		}
		contractID, err := uuid.Parse(got.header.Get("X-Merc-Contract-ID"))
		if err != nil {
			t.Fatalf("missing delivery contract id: %v", err)
		}
		if contractIDs[contractID] {
			t.Fatalf("two deliveries shared contract authority %s", contractID)
		}
		contractIDs[contractID] = true
		if got.header.Get("X-Merc-Coalesced") == "1" {
			followerContracts++
		} else {
			leaderContracts++
		}
	}
	if len(contractIDs) != coalescedFollowers || leaderContracts != 1 || followerContracts != coalescedFollowers-1 {
		t.Fatalf("delivery authorities leader=%d followers=%d distinct=%d", leaderContracts, followerContracts, len(contractIDs))
	}
	if got := upstreamCalls.Load(); got != 1 {
		t.Fatalf("%d physical upstream calls for %d deliveries", got, coalescedFollowers)
	}

	var leaderReceipts, followerReceipts int
	for contractID := range contractIDs {
		receipt, err := store.RealtimeReceipt(ctx, buyerID, contractID)
		if err != nil {
			t.Fatalf("read receipt %s: %v", contractID, err)
		}
		if receipt.ContractID != contractID.String() || receipt.State != "VERIFIED" ||
			receipt.Verification != "PASSED" || receipt.PricingAuthorityStatus != "verified" ||
			receipt.SettlementCurrency != "cad" || receipt.PricingDecision == nil {
			t.Fatalf("invalid receipt for %s: %+v", contractID, receipt)
		}
		if receipt.PricingDecision.Realtime != nil {
			leaderReceipts++
			if receipt.SupplierPayableNanos <= 0 || receipt.KnownCostContributionNanos <= 0 {
				t.Fatalf("physical receipt lacks payable or positive contribution: %+v", receipt)
			}
			continue
		}
		if receipt.PricingDecision.RealtimeReuse == nil ||
			receipt.PricingDecision.RealtimeReuse.ReuseClass != ClassCoalescedDelivery ||
			receipt.SupplierPayableNanos != 0 || receipt.KnownCostContributionNanos <= 0 {
			t.Fatalf("follower receipt lost zero-physical coalesced authority: %+v", receipt)
		}
		followerReceipts++
	}
	if leaderReceipts != 1 || followerReceipts != coalescedFollowers-1 {
		t.Fatalf("receipt modes leader=%d followers=%d", leaderReceipts, followerReceipts)
	}

	var executions, workerBackedExecutions, logicalDeliveries, settlements, supplierRows, buyerRows, platformRows int
	var buyerMicros, supplierMicros, platformMicros int64
	if err := pool.QueryRow(ctx, `
		SELECT
		  (SELECT count(*) FROM realtime_executions WHERE contract_id = ANY($1)),
		  (SELECT count(*) FROM realtime_executions WHERE contract_id = ANY($1) AND worker_id IS NOT NULL),
		  (SELECT count(*) FROM realtime_executions WHERE contract_id = ANY($1) AND worker_id IS NULL),
		  (SELECT count(*) FROM realtime_settlements WHERE contract_id = ANY($1)),
		  count(*) FILTER (WHERE kind='supplier_credit'),
		  count(*) FILTER (WHERE kind='buyer_charge'),
		  count(*) FILTER (WHERE kind='platform_take'),
		  COALESCE((-sum(amount_usd) FILTER (WHERE kind='buyer_charge')*1000000)::bigint,0),
		  COALESCE((sum(amount_usd) FILTER (WHERE kind='supplier_credit')*1000000)::bigint,0),
		  COALESCE((sum(amount_usd) FILTER (WHERE kind='platform_take')*1000000)::bigint,0)
		FROM ledger_entries WHERE execution_contract_id = ANY($1)`, authorityIDs(contractIDs)).
		Scan(&executions, &workerBackedExecutions, &logicalDeliveries, &settlements, &supplierRows, &buyerRows, &platformRows,
			&buyerMicros, &supplierMicros, &platformMicros); err != nil {
		t.Fatalf("read coalesced ledger: %v", err)
	}
	if executions != coalescedFollowers || workerBackedExecutions != 1 ||
		logicalDeliveries != coalescedFollowers-1 || settlements != coalescedFollowers {
		t.Fatalf("receipt execution modes total=%d worker_backed=%d logical=%d settlements=%d",
			executions, workerBackedExecutions, logicalDeliveries, settlements)
	}
	if supplierRows != 1 || buyerRows != coalescedFollowers || platformRows != coalescedFollowers ||
		buyerMicros != supplierMicros+platformMicros || supplierMicros <= 0 || platformMicros <= 0 {
		t.Fatalf("cluster ledger supplier_rows=%d buyer_rows=%d platform_rows=%d buyer=%d supplier=%d platform=%d",
			supplierRows, buyerRows, platformRows, buyerMicros, supplierMicros, platformMicros)
	}
	t.Logf("CAD coalescing proof: 1 upstream call, %d receipt authorities, 1 supplier payable, buyer=%d supplier=%d platform=%d micros",
		coalescedFollowers, buyerMicros, supplierMicros, platformMicros)
}

func TestOneExecutionWith128FollowersWritesNoSupplierCreditAndOneAuthorityEach(t *testing.T) {
	ctx, store, pool := openPayoutTestStore(t)

	buyerID := uuid.New()
	otherBuyerID := uuid.New()
	for _, id := range []uuid.UUID{buyerID, otherBuyerID} {
		if _, err := pool.Exec(ctx, `
			INSERT INTO buyers (id,email,password_hash,free_credit_usd)
			VALUES ($1,$2,'x',100.0)`, id, id.String()+"@coalesced.invalid"); err != nil {
			t.Fatalf("seed buyer: %v", err)
		}
	}

	profile := sortedVLLMProfiles()[0]
	fullPer1K := fullPricePer1KFromRealtime(
		profile.BuyerInputUSDPerMillionTokens, profile.BuyerOutputUSDPerMillionTokens)

	// The one physical execution: one result artifact, one token count. Every
	// follower is delivered THIS, which is what makes them followers.
	const deliveredTokens int64 = 64
	leaderResultRef := "cas/sha256/" + uuid.NewString()
	leaderResultSHA := "eeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeee"

	// What a fresh execution would have cost the buyer, for the comparison the
	// whole mechanism exists to make.
	fresh := PriceAccounting(TokenAccounting{
		ClassUncachedInput: 100, ClassGeneratedOutput: deliveredTokens,
	}, fullPer1K)

	currency, err := SettlementCurrency()
	if err != nil {
		t.Fatal(err)
	}
	money, err := SettleRealtimeReuseHitMoney(currency, deliveredTokens,
		profile.BuyerInputUSDPerMillionTokens, profile.BuyerOutputUSDPerMillionTokens)
	if err != nil || !money.Conserved() || !money.ConservedExact() || money.SupplierLiabilityMicros != 0 {
		t.Fatalf("follower money invariant broken before any write: %+v", money)
	}

	authorities := map[uuid.UUID]bool{}
	requestIDs := map[string]bool{}
	for i := 0; i < coalescedFollowers; i++ {
		// A distinct request identity per follower, because each is a different
		// buyer request that happened to collapse onto one execution. Reusing one
		// identity would be testing idempotency, not coalescing.
		requestID := fmt.Sprintf("req_follower_%03d_%s", i, uuid.NewString())
		hit := ExactCacheHit{ResultRef: leaderResultRef, OutputTokens: deliveredTokens}
		contract, settlement, err := store.SettleRealtimeExactReuse(ctx,
			RealtimeContractAuthorization{
				RequestID: requestID, BuyerID: buyerID, Profile: profile,
				InputCommitment:   "cccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccc",
				RequestSHA256:     "dddddddddddddddddddddddddddddddddddddddddddddddddddddddddddddddd",
				MaximumPriceUSD:   microsToUSD(money.BuyerDebitMicros),
				EstimatedPriceUSD: microsToUSD(money.BuyerDebitMicros),
				ReuseClass:        ClassCoalescedDelivery,
			}, hit, money, leaderResultSHA)
		if err != nil {
			t.Fatalf("follower %d: settle: %v", i, err)
		}
		if contract.Pricing == nil || contract.Pricing.RealtimeReuse == nil ||
			contract.Pricing.RealtimeReuse.ReuseClass != ClassCoalescedDelivery ||
			settlement.SupplierPayableNanos != 0 ||
			settlement.BuyerChargeNanos != settlement.KnownCostContributionNanos {
			t.Fatalf("follower %d lost coalesced exact authority: contract=%+v settlement=%+v", i, contract, settlement)
		}

		if authorities[contract.ID] {
			t.Fatalf("follower %d reused delivery authority %s; 128 buyers would share "+
				"one receipt", i, contract.ID)
		}
		authorities[contract.ID] = true
		if requestIDs[requestID] {
			t.Fatalf("follower %d reused request id %s", i, requestID)
		}
		requestIDs[requestID] = true

		if contract.WorkerID != uuid.Nil || contract.SupplierID != uuid.Nil {
			t.Fatalf("follower %d scheduled a worker: worker=%s supplier=%s",
				i, contract.WorkerID, contract.SupplierID)
		}
		if settlement.SupplierPayableUSD != 0 {
			t.Fatalf("follower %d minted a supplier payable of %v", i, settlement.SupplierPayableUSD)
		}
		if settlement.BuyerChargeUSD <= 0 {
			t.Fatalf("follower %d was delivered free; storage, lookup and delivery are "+
				"real costs", i)
		}
		if settlement.BuyerChargeUSD >= fresh {
			t.Fatalf("follower %d paid %.9f, not less than a fresh execution's %.9f",
				i, settlement.BuyerChargeUSD, fresh)
		}
	}

	if len(authorities) != coalescedFollowers {
		t.Fatalf("%d distinct delivery authorities for %d followers",
			len(authorities), coalescedFollowers)
	}

	// The ledger, read once across the whole cluster.
	var supplierRows, buyerRows, platformRows int
	var buyerMicros, platformMicros, supplierMicros int64
	if err := pool.QueryRow(ctx, `
		SELECT count(*) FILTER (WHERE kind='supplier_credit'),
		       count(*) FILTER (WHERE kind='buyer_charge'),
		       count(*) FILTER (WHERE kind='platform_take'),
		       COALESCE((-sum(amount_usd) FILTER (WHERE kind='buyer_charge')*1000000)::bigint,0),
		       COALESCE((sum(amount_usd) FILTER (WHERE kind='platform_take')*1000000)::bigint,0),
		       COALESCE((sum(amount_usd) FILTER (WHERE kind='supplier_credit')*1000000)::bigint,0)
		  FROM ledger_entries
		 WHERE execution_contract_id = ANY($1)`, authorityIDs(authorities)).
		Scan(&supplierRows, &buyerRows, &platformRows,
			&buyerMicros, &platformMicros, &supplierMicros); err != nil {
		t.Fatalf("read cluster ledger: %v", err)
	}
	if supplierRows != 0 || supplierMicros != 0 {
		t.Fatalf("%d supplier credits worth %d micros across %d followers; the supplier "+
			"is paid once by the leader for the one execution",
			supplierRows, supplierMicros, coalescedFollowers)
	}
	if buyerRows != coalescedFollowers {
		t.Fatalf("%d buyer charges for %d followers", buyerRows, coalescedFollowers)
	}
	if platformRows != coalescedFollowers {
		t.Fatalf("%d platform takes for %d followers", platformRows, coalescedFollowers)
	}
	// With no supplier owed, every micro the buyer paid is the platform ledger
	// take and the reuse decision's known contribution. It is not true net until
	// the separately measured delivery costs have been allocated.
	if buyerMicros != platformMicros {
		t.Fatalf("cluster ledger not conserved: buyer %d micros, platform %d micros",
			buyerMicros, platformMicros)
	}
	if platformMicros <= 0 {
		t.Fatalf("known coalesced contribution across the cluster is %d micros", platformMicros)
	}

	// Every follower can fetch its own receipt, and only its own.
	fetched := 0
	for id := range authorities {
		receipt, err := store.RealtimeReceipt(ctx, buyerID, id)
		if err != nil {
			t.Fatalf("receipt for %s: %v", id, err)
		}
		if receipt.ContractID != id.String() {
			t.Fatalf("receipt for %s reported contract %s", id, receipt.ContractID)
		}
		fetched++
		// The other tenant must not be able to read it. This is the cross-tenant
		// disclosure the reuse key's tenant scope exists to prevent, checked at the
		// read rather than only at the lookup.
		if _, err := store.RealtimeReceipt(ctx, otherBuyerID, id); err == nil {
			t.Fatalf("a second tenant read follower receipt %s", id)
		}
	}
	if fetched != coalescedFollowers {
		t.Fatalf("fetched %d receipts for %d followers", fetched, coalescedFollowers)
	}

	t.Logf("128 followers: %d authorities, %d receipts, %d supplier credits, "+
		"buyer %d micros == platform %d micros, fresh execution would have cost %.9f",
		len(authorities), fetched, supplierRows, buyerMicros, platformMicros, fresh)
}

func authorityIDs(set map[uuid.UUID]bool) []uuid.UUID {
	out := make([]uuid.UUID, 0, len(set))
	for id := range set {
		out = append(out, id)
	}
	return out
}
