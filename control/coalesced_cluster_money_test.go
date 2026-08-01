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
	var leaderContractID uuid.UUID
	var leaderSupplierEntitlementNanos int64
	var followerReceiptAuthorities []RealtimeReceipt
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
			leaderContractID = contractID
			leaderSupplierEntitlementNanos = receipt.SupplierPayableNanos
			continue
		}
		if receipt.PricingDecision.RealtimeReuse == nil ||
			receipt.PricingDecision.RealtimeReuse.ReuseClass != ClassCoalescedDelivery ||
			receipt.SupplierPayableNanos != 0 || receipt.KnownCostContributionNanos <= 0 {
			t.Fatalf("follower receipt lost zero-physical coalesced authority: %+v", receipt)
		}
		followerReceipts++
		followerReceiptAuthorities = append(followerReceiptAuthorities, receipt)
	}
	if leaderReceipts != 1 || followerReceipts != coalescedFollowers-1 {
		t.Fatalf("receipt modes leader=%d followers=%d", leaderReceipts, followerReceipts)
	}
	if leaderContractID == uuid.Nil || leaderSupplierEntitlementNanos <= 0 {
		t.Fatalf("missing physical leader receipt identity=%s supplier=%d", leaderContractID, leaderSupplierEntitlementNanos)
	}
	for _, receipt := range followerReceiptAuthorities {
		if receipt.Coalescing == nil || receipt.Coalescing.Role != "FOLLOWER" ||
			receipt.Coalescing.LeaderContractID != leaderContractID.String() ||
			receipt.Coalescing.CoalescedFollowerDeliveries != 1 ||
			receipt.Coalescing.CounterfactualPhysicalExecutionsAvoided != 1 ||
			receipt.Coalescing.CounterfactualSupplierEntitlementNanos != leaderSupplierEntitlementNanos ||
			receipt.Coalescing.Currency != "cad" {
			t.Fatalf("follower receipt lost physical source provenance: %+v", receipt.Coalescing)
		}
	}
	leaderReceipt, err := store.RealtimeReceipt(ctx, buyerID, leaderContractID)
	if err != nil || leaderReceipt.Coalescing == nil || leaderReceipt.Coalescing.Role != "LEADER" ||
		leaderReceipt.Coalescing.CoalescedFollowerDeliveries != coalescedFollowers-1 ||
		leaderReceipt.Coalescing.CounterfactualPhysicalExecutionsAvoided != coalescedFollowers-1 ||
		leaderReceipt.Coalescing.CounterfactualSupplierEntitlementNanos != int64(coalescedFollowers-1)*leaderSupplierEntitlementNanos ||
		leaderReceipt.Coalescing.Currency != "cad" {
		t.Fatalf("leader receipt lost coalescing aggregate: receipt=%+v err=%v", leaderReceipt.Coalescing, err)
	}
	var provenanceRows, provenanceLeaders int
	var provenanceEntitlementNanos int64
	if err := pool.QueryRow(ctx, `
		SELECT count(*), count(DISTINCT leader_contract_id),
		       COALESCE(sum(counterfactual_supplier_entitlement_nanos),0)::bigint
		  FROM realtime_coalesced_deliveries
		 WHERE follower_contract_id = ANY($1)`, authorityIDs(contractIDs)).
		Scan(&provenanceRows, &provenanceLeaders, &provenanceEntitlementNanos); err != nil {
		t.Fatalf("read coalescing provenance: %v", err)
	}
	if provenanceRows != coalescedFollowers-1 || provenanceLeaders != 1 ||
		provenanceEntitlementNanos != int64(coalescedFollowers-1)*leaderSupplierEntitlementNanos {
		t.Fatalf("coalescing provenance rows=%d leaders=%d entitlement=%d", provenanceRows, provenanceLeaders, provenanceEntitlementNanos)
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
	installSettlementCurrencyForTest(t, "cad")
	t.Setenv("MERC_TOKEN_KEY", "coalesced-realtime-test-key-with-at-least-32-bytes")
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
	supplierID := uuid.New()
	if _, err := pool.Exec(ctx, `INSERT INTO suppliers (id,email,status) VALUES ($1,$2,'active')`,
		supplierID, "cluster-leader-"+uuid.NewString()+"@example.test"); err != nil {
		t.Fatalf("seed physical leader supplier: %v", err)
	}
	workerID := uuid.New()
	if _, err := store.CreateWorkerToken(ctx, workerID, supplierID); err != nil {
		t.Fatalf("seed physical leader worker: %v", err)
	}
	if err := store.UpsertRealtimeOffer(ctx, WorkerAuth{WorkerID: workerID, SupplierID: supplierID}, RealtimeOfferRegistration{
		RuntimeProfileID: profile.RuntimeProfileID, RuntimeProfileSHA256: profile.ProfileSHA256,
		HWClass: "nvidia_24gb", GPUCount: 1, MemoryGBPerGPU: 24,
		UpstreamBaseURL: "http://coalesced-leader.invalid/v1", UpstreamToken: "coalesced-leader-token",
		Warmth: "HOT", MaxActiveSequences: 1, AvailableSequences: 1,
		SupplierInputUSDPerMillionTokens: 0.08, SupplierOutputUSDPerMillionTokens: 0.30,
	}); err != nil {
		t.Fatalf("register physical leader offer: %v", err)
	}

	// This is a store-level proof, but its source execution is no longer an
	// invented reference: create and settle the one physical leader that every
	// follower is allowed to cite. The handler-level test above proves the same
	// relationship through HTTP; this one pins its durable write boundary.
	const promptTokens, deliveredTokens int64 = 12, 64
	leaderResultRef := "cas/sha256/" + uuid.NewString()
	leaderResultSHA := "eeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeee"
	currency := MustParseCurrency("cad")
	buyerInput, err := nanoRatePerMillionFromFloat(profile.BuyerInputUSDPerMillionTokens)
	if err != nil {
		t.Fatal(err)
	}
	buyerOutput, err := nanoRatePerMillionFromFloat(profile.BuyerOutputUSDPerMillionTokens)
	if err != nil {
		t.Fatal(err)
	}
	leaderExpected, err := BuyerRealtimeTokenChargeNanos(currency, promptTokens, deliveredTokens, buyerInput, buyerOutput)
	if err != nil {
		t.Fatal(err)
	}
	leaderMaximum, err := BuyerRealtimeTokenChargeNanos(currency, 100, deliveredTokens, buyerInput, buyerOutput)
	if err != nil {
		t.Fatal(err)
	}
	leaderExpectedMicros, err := LedgerMicrosFromNanos(leaderExpected)
	if err != nil {
		t.Fatal(err)
	}
	leaderMaximumMicros, err := LedgerMicrosFromNanos(leaderMaximum)
	if err != nil {
		t.Fatal(err)
	}
	leader, _, err := store.AuthorizeRealtimeContract(ctx, RealtimeContractAuthorization{
		RequestID: "req-coalesced-leader-" + uuid.NewString(), BuyerID: buyerID, Profile: profile,
		InputCommitment: strings.Repeat("c", 64), RequestSHA256: strings.Repeat("d", 64),
		MaximumPriceUSD: microsToUSD(leaderMaximumMicros), EstimatedPriceUSD: microsToUSD(leaderExpectedMicros),
		DeadlineAt:          time.Now().Add(time.Minute),
		MaximumPromptTokens: 100, MaximumCompletionTokens: deliveredTokens,
		EstimatedPromptTokens: promptTokens, EstimatedCompletionTokens: deliveredTokens,
	})
	if err != nil {
		t.Fatalf("authorize physical leader: %v", err)
	}
	leaderSettlement, err := store.FinalizeRealtimeSuccess(ctx, leader.ID, RealtimeExecutionEvidence{
		ID: uuid.New(), HTTPStatus: http.StatusOK, StreamRootSHA256: strings.Repeat("a", 64),
		OutputCommitment: leaderResultSHA, PromptTokens: promptTokens, CompletionTokens: deliveredTokens,
		TotalTokens: promptTokens + deliveredTokens,
	})
	if err != nil || leaderSettlement.SupplierPayableNanos <= 0 {
		t.Fatalf("finalize physical leader settlement=%+v err=%v", leaderSettlement, err)
	}
	fullPer1K := fullPricePer1KFromRealtime(
		profile.BuyerInputUSDPerMillionTokens, profile.BuyerOutputUSDPerMillionTokens)

	// What a fresh execution would have cost the buyer, for the comparison the
	// whole mechanism exists to make.
	fresh := PriceAccounting(TokenAccounting{
		ClassUncachedInput: 100, ClassGeneratedOutput: deliveredTokens,
	}, fullPer1K)

	currency, err = SettlementCurrency()
	if err != nil {
		t.Fatal(err)
	}
	money, err := SettleRealtimeReuseHitMoney(currency, deliveredTokens,
		profile.BuyerInputUSDPerMillionTokens, profile.BuyerOutputUSDPerMillionTokens)
	if err != nil || !money.Conserved() || !money.ConservedExact() || money.SupplierLiabilityMicros != 0 {
		t.Fatalf("follower money invariant broken before any write: %+v", money)
	}
	// The source relationship is an authority, not a display field. A follower
	// may settle only against this buyer's finalized physical execution and the
	// exact delivered output it actually committed. These negatives must fail
	// before a follower contract, ledger entry, or provenance row is written.
	rejectCoalescedSource := func(label string, buyer, source uuid.UUID, commitment string) {
		t.Helper()
		_, _, err := store.SettleRealtimeExactReuse(ctx, RealtimeContractAuthorization{
			RequestID: "req-coalesced-rejected-" + label + "-" + uuid.NewString(),
			BuyerID:   buyer, Profile: profile,
			InputCommitment: strings.Repeat("c", 64), RequestSHA256: strings.Repeat("d", 64),
			MaximumPriceUSD: microsToUSD(money.BuyerDebitMicros), EstimatedPriceUSD: microsToUSD(money.BuyerDebitMicros),
			ReuseClass: ClassCoalescedDelivery, CoalescedLeaderContractID: source,
		}, ExactCacheHit{ResultRef: leaderResultRef, OutputTokens: deliveredTokens}, money, commitment)
		if err == nil {
			t.Fatalf("%s coalesced source was accepted", label)
		}
	}
	rejectCoalescedSource("missing", buyerID, uuid.Nil, leaderResultSHA)
	rejectCoalescedSource("cross-buyer", otherBuyerID, leader.ID, leaderResultSHA)
	rejectCoalescedSource("wrong-output", buyerID, leader.ID, strings.Repeat("f", 64))
	reuseBackedSource, _, err := store.SettleRealtimeExactReuse(ctx, RealtimeContractAuthorization{
		RequestID: "req-coalesced-reuse-source-" + uuid.NewString(), BuyerID: buyerID, Profile: profile,
		InputCommitment: strings.Repeat("c", 64), RequestSHA256: strings.Repeat("d", 64),
		MaximumPriceUSD: microsToUSD(money.BuyerDebitMicros), EstimatedPriceUSD: microsToUSD(money.BuyerDebitMicros),
		ReuseClass: ClassExactResultReuse,
	}, ExactCacheHit{ResultRef: leaderResultRef, OutputTokens: deliveredTokens}, money, leaderResultSHA)
	if err != nil {
		t.Fatalf("create reuse-backed negative source: %v", err)
	}
	rejectCoalescedSource("reuse-backed", buyerID, reuseBackedSource.ID, leaderResultSHA)
	var rejectedContracts int
	if err := pool.QueryRow(ctx, `
		SELECT count(*) FROM execution_contracts WHERE request_id LIKE 'req-coalesced-rejected-%'`).
		Scan(&rejectedContracts); err != nil || rejectedContracts != 0 {
		t.Fatalf("rejected sources wrote contracts=%d err=%v", rejectedContracts, err)
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
				InputCommitment:           "cccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccc",
				RequestSHA256:             "dddddddddddddddddddddddddddddddddddddddddddddddddddddddddddddddd",
				MaximumPriceUSD:           microsToUSD(money.BuyerDebitMicros),
				EstimatedPriceUSD:         microsToUSD(money.BuyerDebitMicros),
				ReuseClass:                ClassCoalescedDelivery,
				CoalescedLeaderContractID: leader.ID,
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

	// The ledger, read once across the physical leader and every follower.
	clusterAuthorities := append(authorityIDs(authorities), leader.ID)
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
		 WHERE execution_contract_id = ANY($1)`, clusterAuthorities).
		Scan(&supplierRows, &buyerRows, &platformRows,
			&buyerMicros, &platformMicros, &supplierMicros); err != nil {
		t.Fatalf("read cluster ledger: %v", err)
	}
	leaderSupplierMicros, err := LedgerMicrosFromNanos(MoneyNanos{Currency: currency, Nanos: leaderSettlement.SupplierPayableNanos})
	if err != nil {
		t.Fatal(err)
	}
	if supplierRows != 1 || supplierMicros != leaderSupplierMicros {
		t.Fatalf("%d supplier credits worth %d micros across %d followers; the supplier "+
			"must be paid once by the leader for the one execution",
			supplierRows, supplierMicros, coalescedFollowers)
	}
	if buyerRows != coalescedFollowers+1 {
		t.Fatalf("%d buyer charges for one leader and %d followers", buyerRows, coalescedFollowers)
	}
	if platformRows != coalescedFollowers+1 {
		t.Fatalf("%d platform takes for one leader and %d followers", platformRows, coalescedFollowers)
	}
	// Across the physical leader plus its followers, the ledger still conserves.
	if buyerMicros != supplierMicros+platformMicros {
		t.Fatalf("cluster ledger not conserved: buyer %d supplier %d platform %d micros",
			buyerMicros, supplierMicros, platformMicros)
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
		if receipt.Coalescing == nil || receipt.Coalescing.Role != "FOLLOWER" ||
			receipt.Coalescing.LeaderContractID != leader.ID.String() ||
			receipt.Coalescing.CounterfactualSupplierEntitlementNanos != leaderSettlement.SupplierPayableNanos {
			t.Fatalf("follower receipt lost physical leader provenance: %+v", receipt.Coalescing)
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
	leaderReceipt, err := store.RealtimeReceipt(ctx, buyerID, leader.ID)
	if err != nil {
		t.Fatalf("leader receipt: %v", err)
	}
	if leaderReceipt.Coalescing == nil || leaderReceipt.Coalescing.Role != "LEADER" ||
		leaderReceipt.Coalescing.LeaderContractID != leader.ID.String() ||
		leaderReceipt.Coalescing.CoalescedFollowerDeliveries != coalescedFollowers ||
		leaderReceipt.Coalescing.CounterfactualPhysicalExecutionsAvoided != coalescedFollowers ||
		leaderReceipt.Coalescing.CounterfactualSupplierEntitlementNanos != int64(coalescedFollowers)*leaderSettlement.SupplierPayableNanos {
		t.Fatalf("leader receipt lost coalescing provenance: %+v", leaderReceipt.Coalescing)
	}

	t.Logf("128 followers: %d authorities, %d receipts, %d supplier credits, "+
		"buyer %d micros == supplier %d + platform %d, fresh execution would have cost %.9f",
		len(authorities), fetched, supplierRows, buyerMicros, supplierMicros, platformMicros, fresh)
}

func authorityIDs(set map[uuid.UUID]bool) []uuid.UUID {
	out := make([]uuid.UUID, 0, len(set))
	for id := range set {
		out = append(out, id)
	}
	return out
}
