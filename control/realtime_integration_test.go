package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
)

func TestRealtimeStreamContractVerificationSettlementAndReceipt(t *testing.T) {
	databaseURL := requireTestDatabase(t)
	t.Setenv("MERC_TOKEN_KEY", "realtime-integration-test-key-with-at-least-32-bytes")
	t.Setenv("STRIPE_SECRET_KEY", "")
	t.Setenv("MERC_CANARY_ENABLED", "false")

	ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
	defer cancel()
	pool, err := pgxpool.New(ctx, databaseURL)
	if err != nil {
		t.Fatal(err)
	}
	defer pool.Close()
	store := NewStore(pool)
	if err := store.Migrate(ctx); err != nil {
		t.Fatal(err)
	}

	// This test asserts on RealtimeOperationalSnapshot and on the /metrics
	// gauges, both of which count platform-wide.  Those assertions only hold if
	// the realtime tables start empty, so take ownership of them rather than
	// assuming a database nobody else has touched -- the suite shares one, and
	// re-running it must not depend on the previous run's leftovers.
	//
	// Restricted to realtime state on purpose: nothing else in the suite writes
	// these tables, and truncating more would make this test destructive to its
	// neighbours instead of merely self-sufficient.
	if _, err := pool.Exec(ctx, `TRUNCATE
		realtime_authorization_events, realtime_settlements, realtime_executions,
		realtime_refunds, execution_contracts, realtime_worker_offers
		RESTART IDENTITY CASCADE`); err != nil {
		t.Fatalf("reset realtime state: %v", err)
	}
	if _, err := pool.Exec(ctx,
		`DELETE FROM ledger_entries WHERE execution_contract_id IS NOT NULL`); err != nil {
		t.Fatalf("reset realtime ledger rows: %v", err)
	}

	suffix := uuid.NewString()
	buyerID, err := store.CreateBuyerAccount(ctx, "realtime-"+suffix+"@example.test", "integration-password", 5)
	if err != nil {
		t.Fatal(err)
	}
	_, buyerKey, _, err := store.CreateAPIKey(ctx, buyerID, "realtime integration", true)
	if err != nil {
		t.Fatal(err)
	}
	supplierID := uuid.New()
	if _, err := pool.Exec(ctx, `INSERT INTO suppliers (id,email,status) VALUES ($1,$2,'pending')`,
		supplierID, "supplier-"+suffix+"@example.test"); err != nil {
		t.Fatal(err)
	}
	workerID := uuid.New()
	if _, err := store.CreateWorkerToken(ctx, workerID, supplierID); err != nil {
		t.Fatal(err)
	}

	upstreamToken := "cx_vllm_integration_secret_123456789"
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/chat/completions" {
			t.Errorf("upstream path = %s", r.URL.Path)
		}
		if r.Header.Get("Authorization") != "Bearer "+upstreamToken {
			t.Error("upstream bearer credential was not forwarded")
		}
		body, readErr := io.ReadAll(r.Body)
		if readErr != nil {
			t.Error(readErr)
		}
		var request map[string]any
		if json.Unmarshal(body, &request) != nil {
			t.Error("upstream body was not JSON")
		}
		benchmarkRequest := request["user"] == "merc-benchmark-v1"
		if benchmarkRequest {
			time.Sleep(150 * time.Millisecond)
		}
		if request["stream"] != true {
			w.Header().Set("Content-Type", "application/json")
			if tools, ok := request["tools"].([]any); ok && len(tools) == 2 && request["parallel_tool_calls"] == true {
				_, _ = io.WriteString(w, `{"id":"chatcmpl_tools","object":"chat.completion","created":1,"model":"cx-chat-1b","choices":[{"index":0,"message":{"role":"assistant","content":null,"tool_calls":[{"id":"call_weather","type":"function","function":{"name":"weather","arguments":"{\"city\":\"Toronto\"}"}},{"id":"call_time","type":"function","function":{"name":"time","arguments":"{\"zone\":\"UTC\"}"}}]},"finish_reason":"tool_calls","logprobs":null}],"usage":{"prompt_tokens":7,"completion_tokens":2,"total_tokens":9}}`)
				return
			}
			if _, ok := request["response_format"].(map[string]any); ok {
				_, _ = io.WriteString(w, `{"id":"chatcmpl_structured","object":"chat.completion","created":1,"model":"cx-chat-1b","choices":[{"index":0,"message":{"role":"assistant","content":"{\"status\":\"ready\",\"count\":3}"},"finish_reason":"stop","logprobs":null}],"usage":{"prompt_tokens":7,"completion_tokens":2,"total_tokens":9}}`)
				return
			}
			_, _ = io.WriteString(w, `{"id":"chatcmpl_integration_json","object":"chat.completion","created":1,"model":"cx-chat-1b","choices":[{"index":0,"message":{"role":"assistant","content":"hello"},"finish_reason":"stop","logprobs":null}],"usage":{"prompt_tokens":7,"completion_tokens":2,"total_tokens":9}}`)
			return
		}
		streamOptions, _ := request["stream_options"].(map[string]any)
		if streamOptions["include_usage"] != true {
			t.Error("gateway did not request final usage")
		}
		w.Header().Set("Content-Type", "text/event-stream")
		flusher := w.(http.Flusher)
		for _, event := range []string{
			`data: {"id":"chatcmpl_integration","object":"chat.completion.chunk","created":1,"model":"cx-chat-1b","choices":[{"index":0,"delta":{"content":"hello"},"finish_reason":null}],"usage":null}` + "\n\n",
			`data: {"id":"chatcmpl_integration","object":"chat.completion.chunk","created":1,"model":"cx-chat-1b","choices":[],"usage":{"prompt_tokens":7,"completion_tokens":2,"total_tokens":9}}` + "\n\n",
			"data: [DONE]\n\n",
		} {
			_, _ = io.WriteString(w, event)
			flusher.Flush()
			if benchmarkRequest {
				time.Sleep(150 * time.Millisecond)
			}
		}
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
		t.Fatal(err)
	}
	poorBuyerID, err := store.CreateBuyerAccount(ctx, "realtime-no-credit-"+suffix+"@example.test", "integration-password", 0)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := store.AuthorizeRealtimeContract(ctx, RealtimeContractAuthorization{
		RequestID: "req-no-credit-" + suffix, BuyerID: poorBuyerID, Profile: profile,
		InputCommitment: strings.Repeat("c", 64), RequestSHA256: strings.Repeat("d", 64),
		MaximumPriceUSD: 0.001, EstimatedPriceUSD: 0.0005, DeadlineAt: time.Now().Add(time.Minute),
		MaximumPromptTokens: 8330, MaximumCompletionTokens: 1,
		EstimatedPromptTokens: 4163, EstimatedCompletionTokens: 1,
	}); !errors.Is(err, errRealtimeInsufficientFunds) {
		t.Fatalf("unfunded buyer authorization returned %v", err)
	}
	var poorContracts int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM execution_contracts WHERE buyer_id=$1`, poorBuyerID).Scan(&poorContracts); err != nil || poorContracts != 0 {
		t.Fatalf("unfunded authorization created contracts=%d err=%v", poorContracts, err)
	}

	bounded, _, err := store.AuthorizeRealtimeContract(ctx, RealtimeContractAuthorization{
		RequestID: "req-over-bound-" + suffix, BuyerID: buyerID, Profile: profile,
		InputCommitment: strings.Repeat("e", 64), RequestSHA256: strings.Repeat("f", 64),
		MaximumPriceUSD: 0.001, EstimatedPriceUSD: 0.0005, DeadlineAt: time.Now().Add(time.Minute),
		MaximumPromptTokens: 8330, MaximumCompletionTokens: 1,
		EstimatedPromptTokens: 4163, EstimatedCompletionTokens: 1,
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.FinalizeRealtimeSuccess(ctx, bounded.ID, RealtimeExecutionEvidence{
		ID: uuid.New(), HTTPStatus: http.StatusOK, StreamRootSHA256: strings.Repeat("1", 64),
		OutputCommitment: strings.Repeat("2", 64), PromptTokens: 8331, CompletionTokens: 1,
		TotalTokens: 8332,
	}); err == nil || !strings.Contains(err.Error(), "exceeds frozen PricingDecision token bounds") {
		t.Fatalf("over-bound verified usage was accepted: %v", err)
	}
	var boundedEffects int
	if err := pool.QueryRow(ctx, `
		SELECT (SELECT count(*) FROM realtime_executions WHERE contract_id=$1) +
		       (SELECT count(*) FROM realtime_settlements WHERE contract_id=$1)`,
		bounded.ID).Scan(&boundedEffects); err != nil || boundedEffects != 0 {
		t.Fatalf("rejected over-bound usage created effects=%d err=%v", boundedEffects, err)
	}
	if finalized, err := store.FinalizeRealtimeFailure(ctx, bounded.ID, uuid.New(), 0, 1,
		"bounds_test", "bounds test cleanup", false); err != nil || !finalized {
		t.Fatalf("bounds-test cleanup failed: finalized=%v err=%v", finalized, err)
	}

	server := httptest.NewServer(NewServer(store, nil, nil, nil).Routes())
	defer server.Close()
	requestBody := []byte(`{"model":"cx-chat-1b","messages":[{"role":"user","content":"say hello"}],"stream":true,"max_tokens":8}`)
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, server.URL+"/v1/chat/completions", bytes.NewReader(requestBody))
	if err != nil {
		t.Fatal(err)
	}
	request.Header.Set("Authorization", "Bearer "+buyerKey)
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Idempotency-Key", "realtime-integration-"+suffix)
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	streamBody, err := io.ReadAll(response.Body)
	response.Body.Close()
	if err != nil {
		t.Fatal(err)
	}
	if response.StatusCode != http.StatusOK {
		t.Fatalf("chat status=%d body=%s", response.StatusCode, streamBody)
	}
	if !strings.Contains(string(streamBody), "hello") || !strings.Contains(string(streamBody), "[DONE]") {
		t.Fatalf("compatible stream was not relayed: %s", streamBody)
	}
	contractID, err := uuid.Parse(response.Header.Get("X-Merc-Contract-ID"))
	if err != nil {
		t.Fatalf("missing contract header: %v", err)
	}
	if _, err := store.RealtimeReceipt(ctx, buyerID, contractID); err != nil {
		t.Fatalf("direct realtime receipt read failed: %v", err)
	}

	receiptRequest, _ := http.NewRequestWithContext(ctx, http.MethodGet,
		server.URL+"/v1/realtime/requests/"+contractID.String()+"/receipt", nil)
	receiptRequest.Header.Set("Authorization", "Bearer "+buyerKey)
	receiptResponse, err := http.DefaultClient.Do(receiptRequest)
	if err != nil {
		t.Fatal(err)
	}
	defer receiptResponse.Body.Close()
	var receipt RealtimeReceipt
	if err := json.NewDecoder(receiptResponse.Body).Decode(&receipt); err != nil {
		t.Fatal(err)
	}
	if receiptResponse.StatusCode != http.StatusOK || receipt.State != "VERIFIED" || receipt.Verification != "PASSED" {
		t.Fatalf("unexpected receipt: status=%d receipt=%+v", receiptResponse.StatusCode, receipt)
	}
	if receipt.PlacementPlan == nil || !validSHA256(receipt.PlacementPlanSHA256) ||
		receipt.PlacementPlan.RuntimeProfileID != profile.RuntimeProfileID ||
		receipt.PlacementPlan.RuntimeProfileSHA256 != profile.ProfileSHA256 ||
		receipt.PlacementPlan.HWClass != "nvidia_24gb" ||
		receipt.PlacementPlan.AdmittedTensorParallel != 1 {
		t.Fatalf("receipt omitted or changed frozen placement authority: %+v", receipt)
	}
	if receipt.PricingAuthorityStatus != "verified" || receipt.PricingDecision == nil ||
		!validSHA256(receipt.PricingDecisionSHA256) || receipt.PricingDecision.Realtime == nil ||
		receipt.PricingDecision.FixedPoint == nil ||
		receipt.PricingDecision.FixedPoint.Currency != "usd" ||
		receipt.PricingDecision.FixedPoint.TrueNetContributionNanos != nil {
		t.Fatalf("receipt omitted exact realtime PricingDecision or mislabeled true net: %+v", receipt)
	}
	if receipt.SettlementCurrency != "usd" || receipt.BuyerChargeNanos <= 0 ||
		receipt.SupplierPayableNanos <= 0 || receipt.KnownCostContributionNanos <= 0 ||
		receipt.BuyerChargeNanos != receipt.SupplierPayableNanos+receipt.KnownCostContributionNanos {
		t.Fatalf("receipt exact realtime settlement does not conserve: %+v", receipt)
	}
	pricingAuthority := receipt.PricingDecision.Realtime
	expectedBuyer, err := BuyerRealtimeTokenChargeNanos(usd(t), receipt.PromptTokens, receipt.CompletionTokens,
		NanoMajorPerMillionTokens(pricingAuthority.BuyerInputNanosPerMillion),
		NanoMajorPerMillionTokens(pricingAuthority.BuyerOutputNanosPerMillion))
	if err != nil {
		t.Fatal(err)
	}
	expectedSupplier, err := SupplierRealtimeTokenEntitlementNanos(usd(t), receipt.PromptTokens, receipt.CompletionTokens,
		NanoMajorPerMillionTokens(pricingAuthority.SupplierInputNanosPerMillion),
		NanoMajorPerMillionTokens(pricingAuthority.SupplierOutputNanosPerMillion))
	if err != nil {
		t.Fatal(err)
	}
	if receipt.BuyerChargeNanos != expectedBuyer.Nanos ||
		receipt.SupplierPayableNanos != expectedSupplier.Nanos ||
		receipt.KnownCostContributionNanos != expectedBuyer.Nanos-expectedSupplier.Nanos {
		t.Fatalf("settlement did not derive from frozen PricingDecision rates: receipt=%+v buyer=%d supplier=%d",
			receipt, expectedBuyer.Nanos, expectedSupplier.Nanos)
	}
	var offerPlacementSHA256, contractPlacementSHA256 string
	if err := pool.QueryRow(ctx, `
		SELECT o.placement_plan_sha256,c.placement_plan_sha256
		  FROM realtime_worker_offers o
		  JOIN execution_contracts c ON c.worker_id=o.worker_id
		 WHERE c.id=$1 AND o.runtime_profile_id=c.runtime_profile_id`,
		contractID).Scan(&offerPlacementSHA256, &contractPlacementSHA256); err != nil {
		t.Fatal(err)
	}
	if offerPlacementSHA256 != contractPlacementSHA256 ||
		contractPlacementSHA256 != receipt.PlacementPlanSHA256 {
		t.Fatalf("offer->contract->receipt placement digest drifted: offer=%s contract=%s receipt=%s",
			offerPlacementSHA256, contractPlacementSHA256, receipt.PlacementPlanSHA256)
	}
	if _, err := pool.Exec(ctx, `
		UPDATE execution_contracts
		   SET placement_plan=jsonb_set(placement_plan,'{gpu_count}','2'::jsonb)
		 WHERE id=$1`, contractID); err == nil {
		t.Fatal("database allowed frozen contract placement authority to mutate")
	}
	if _, err := pool.Exec(ctx, `
		UPDATE execution_contracts
		   SET pricing_decision=jsonb_set(pricing_decision,'{fixed_point,buyer_charge_nanos}','999'::jsonb)
		 WHERE id=$1`, contractID); err == nil {
		t.Fatal("database allowed frozen realtime PricingDecision to mutate")
	}
	if _, err := pool.Exec(ctx, `
		UPDATE execution_contracts SET maximum_prompt_tokens=maximum_prompt_tokens+1 WHERE id=$1`,
		contractID); err == nil {
		t.Fatal("database allowed frozen realtime token bounds to mutate")
	}
	if _, err := pool.Exec(ctx, `
		UPDATE execution_contracts
		   SET supplier_input_usd_per_million_tokens=supplier_input_usd_per_million_tokens+0.001
		 WHERE id=$1`, contractID); err == nil {
		t.Fatal("database allowed legacy supplier-rate projection to diverge from PricingDecision")
	}
	if receipt.TotalTokens != 9 || receipt.PromptTokens != 7 || receipt.CompletionTokens != 2 {
		t.Fatalf("usage did not reconcile: %+v", receipt)
	}
	if receipt.BuyerChargeUSD <= 0 || receipt.SupplierPayableUSD <= 0 ||
		receipt.SupplierPayableUSD > receipt.BuyerChargeUSD {
		t.Fatalf("invalid settlement: %+v", receipt)
	}
	if receipt.SettlementID == "" || receipt.AuthorizationState != "CAPTURED" ||
		receipt.AuthorizedUSD <= 0 || receipt.CapturedUSD != receipt.BuyerChargeUSD ||
		receipt.AuthorizedUSD-receipt.CapturedUSD-receipt.ReleasedUSD < -0.0000001 ||
		receipt.AuthorizedUSD-receipt.CapturedUSD-receipt.ReleasedUSD > 0.0000001 ||
		receipt.VoidedUSD != 0 || receipt.RefundUSD != 0 ||
		receipt.SupplierPayoutState != "VERIFICATION_HOLD" {
		t.Fatalf("authorization/settlement receipt did not reconcile: %+v", receipt)
	}
	var rows int
	var net float64
	if err := pool.QueryRow(ctx, `
		SELECT count(*),COALESCE(sum(amount_usd),0)::float8
		  FROM ledger_entries WHERE execution_contract_id=$1`, contractID).Scan(&rows, &net); err != nil {
		t.Fatal(err)
	}
	if rows != 3 || net < -0.0000001 || net > 0.0000001 {
		t.Fatalf("ledger is not one zero-sum three-entry settlement: rows=%d net=%f", rows, net)
	}
	var settlementRows, authorizationEvents int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM realtime_settlements WHERE contract_id=$1`, contractID).Scan(&settlementRows); err != nil || settlementRows != 1 {
		t.Fatalf("settlement authority rows=%d err=%v", settlementRows, err)
	}
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM realtime_authorization_events WHERE contract_id=$1`, contractID).Scan(&authorizationEvents); err != nil || authorizationEvents != 3 {
		t.Fatalf("authorization event rows=%d err=%v", authorizationEvents, err)
	}

	replay, _ := http.NewRequestWithContext(ctx, http.MethodPost, server.URL+"/v1/chat/completions", bytes.NewReader(requestBody))
	replay.Header.Set("Authorization", "Bearer "+buyerKey)
	replay.Header.Set("Content-Type", "application/json")
	replay.Header.Set("Idempotency-Key", "realtime-integration-"+suffix)
	replayResponse, err := http.DefaultClient.Do(replay)
	if err != nil {
		t.Fatal(err)
	}
	replayResponse.Body.Close()
	if replayResponse.StatusCode != http.StatusConflict || replayResponse.Header.Get("X-Merc-Contract-ID") != contractID.String() {
		t.Fatalf("idempotent replay status=%d contract=%q", replayResponse.StatusCode, replayResponse.Header.Get("X-Merc-Contract-ID"))
	}
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM ledger_entries WHERE execution_contract_id=$1`, contractID).Scan(&rows); err != nil || rows != 3 {
		t.Fatalf("idempotent replay changed money effects: rows=%d err=%v", rows, err)
	}

	// A named operator can record one full internal credit only while the
	// supplier payable is still inside the verification hold. The action,
	// buyer credit, supplier clawback, platform correction, and receipt update
	// are one transaction and do not claim that Stripe cash moved.
	adminID := uuid.New()
	adminKey := "cx_admin_realtime_refund_" + strings.ReplaceAll(uuid.NewString(), "-", "")
	if _, err := pool.Exec(ctx, `
		INSERT INTO api_keys (id,buyer_id,key_hash,is_admin,revoked,name)
		VALUES ($1,$2,$3,true,false,'realtime-refund-operator')`,
		adminID, buyerID, hashKey(adminKey)); err != nil {
		t.Fatal(err)
	}
	refundRef := "INC-realtime-refund-" + suffix
	refundBody := []byte(`{"reason":"confirmed platform delivery fault","request_id":"` + refundRef + `"}`)
	var supplierEntryID uuid.UUID
	if err := pool.QueryRow(ctx, `
		UPDATE ledger_entries SET release_at=now()
		 WHERE execution_contract_id=$1 AND kind='supplier_credit'
		 RETURNING id`, contractID).Scan(&supplierEntryID); err != nil {
		t.Fatal(err)
	}
	type refundCallResult struct {
		status int
		refund RealtimeRefund
		err    error
	}
	const refundCallers = 8
	refundResults := make(chan refundCallResult, refundCallers)
	startRefunds := make(chan struct{})
	type payoutRaceResult struct {
		claimed bool
		err     error
	}
	payoutRace := make(chan payoutRaceResult, 1)
	go func() {
		<-startRefunds
		_, claimed, err := store.ClaimPayout(ctx, supplierEntryID)
		payoutRace <- payoutRaceResult{claimed: claimed, err: err}
	}()
	for range refundCallers {
		go func() {
			<-startRefunds
			request, _ := http.NewRequestWithContext(ctx, http.MethodPost,
				server.URL+"/admin/realtime/contracts/"+contractID.String()+"/refund", bytes.NewReader(refundBody))
			request.Header.Set("Authorization", "Bearer "+adminKey)
			request.Header.Set("Content-Type", "application/json")
			response, err := http.DefaultClient.Do(request)
			if err != nil {
				refundResults <- refundCallResult{err: err}
				return
			}
			defer response.Body.Close()
			var refund RealtimeRefund
			err = json.NewDecoder(response.Body).Decode(&refund)
			refundResults <- refundCallResult{status: response.StatusCode, refund: refund, err: err}
		}()
	}
	close(startRefunds)
	var refund RealtimeRefund
	createdRefunds := 0
	for range refundCallers {
		result := <-refundResults
		if result.err != nil || (result.status != http.StatusCreated && result.status != http.StatusOK) {
			t.Fatalf("concurrent refund failed: status=%d refund=%+v err=%v", result.status, result.refund, result.err)
		}
		if result.status == http.StatusCreated {
			createdRefunds++
		}
		if refund.RefundID == "" {
			refund = result.refund
		} else if result.refund.RefundID != refund.RefundID {
			t.Fatalf("concurrent refund returned different identities: %s != %s", result.refund.RefundID, refund.RefundID)
		}
	}
	if createdRefunds != 1 || refund.RefundID == "" || refund.BuyerRefundUSD != receipt.BuyerChargeUSD ||
		refund.SupplierClawbackUSD != receipt.SupplierPayableUSD ||
		refund.InternalCreditState != "RECORDED" || refund.ExternalCashState != "NOT_REQUESTED" ||
		refund.SupplierPayoutState != PayoutClawedBack {
		t.Fatalf("concurrent realtime refund did not resolve once: created=%d refund=%+v", createdRefunds, refund)
	}
	payoutResult := <-payoutRace
	if payoutResult.err != nil || payoutResult.claimed {
		t.Fatalf("refund/payout race crossed the cash boundary: claimed=%t err=%v", payoutResult.claimed, payoutResult.err)
	}
	var payoutOperations, payoutFunding int
	if err := pool.QueryRow(ctx, `
		SELECT (SELECT count(*) FROM supplier_payout_operations WHERE ledger_entry_id=$1),
		       (SELECT count(*) FROM supplier_payout_funding WHERE ledger_entry_id=$1)`,
		supplierEntryID).Scan(&payoutOperations, &payoutFunding); err != nil {
		t.Fatal(err)
	}
	if payoutOperations != 0 || payoutFunding != 0 {
		t.Fatalf("refund/payout race created provider boundary rows: operations=%d funding=%d", payoutOperations, payoutFunding)
	}
	conflictingRefundBody := []byte(`{"reason":"second correction attempt","request_id":"INC-conflicting-` + suffix + `"}`)
	conflictingRefundRequest, _ := http.NewRequestWithContext(ctx, http.MethodPost,
		server.URL+"/admin/realtime/contracts/"+contractID.String()+"/refund", bytes.NewReader(conflictingRefundBody))
	conflictingRefundRequest.Header.Set("Authorization", "Bearer "+adminKey)
	conflictingRefundRequest.Header.Set("Content-Type", "application/json")
	conflictingRefundResponse, err := http.DefaultClient.Do(conflictingRefundRequest)
	if err != nil {
		t.Fatal(err)
	}
	_, _ = io.Copy(io.Discard, conflictingRefundResponse.Body)
	conflictingRefundResponse.Body.Close()
	if conflictingRefundResponse.StatusCode != http.StatusConflict {
		t.Fatalf("second refund correlation returned %d, want 409", conflictingRefundResponse.StatusCode)
	}
	refundedReceipt, err := store.RealtimeReceipt(ctx, buyerID, contractID)
	if err != nil {
		t.Fatal(err)
	}
	if refundedReceipt.AuthorizationState != "REFUNDED" || refundedReceipt.RefundID != refund.RefundID ||
		refundedReceipt.RefundUSD != refundedReceipt.BuyerChargeUSD ||
		refundedReceipt.SupplierClawbackUSD != refundedReceipt.SupplierPayableUSD ||
		refundedReceipt.PlatformRefundUSD != refundedReceipt.PlatformMarginUSD ||
		refundedReceipt.NetBuyerChargeUSD != 0 || refundedReceipt.NetSupplierPayableUSD != 0 ||
		refundedReceipt.NetPlatformMarginUSD != 0 || refundedReceipt.SupplierPayoutState != "REVERSED" ||
		refundedReceipt.ExternalCashState != "NOT_REQUESTED" {
		t.Fatalf("refunded receipt did not reconcile: %+v", refundedReceipt)
	}
	remainingCredit, err := store.BuyerFreeCreditRemaining(ctx, buyerID)
	if err != nil || remainingCredit != 5 {
		t.Fatalf("full internal refund did not restore buyer credit: remaining=%f err=%v", remainingCredit, err)
	}
	if err := pool.QueryRow(ctx, `
		SELECT count(*),COALESCE(sum(amount_usd),0)::float8
		  FROM ledger_entries WHERE execution_contract_id=$1`, contractID).Scan(&rows, &net); err != nil {
		t.Fatal(err)
	}
	if rows != 6 || net < -0.0000001 || net > 0.0000001 {
		t.Fatalf("refund ledger is not a zero-sum six-entry history: rows=%d net=%f", rows, net)
	}
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM realtime_authorization_events WHERE contract_id=$1`, contractID).Scan(&authorizationEvents); err != nil || authorizationEvents != 4 {
		t.Fatalf("refunded authorization event rows=%d err=%v", authorizationEvents, err)
	}
	var refundActions int
	if err := pool.QueryRow(ctx, `
		SELECT count(*) FROM admin_actions
		 WHERE kind='realtime_refunded' AND target_id=$1 AND correlation_ref=$2`,
		contractID, refundRef).Scan(&refundActions); err != nil || refundActions != 1 {
		t.Fatalf("refund audit actions=%d err=%v", refundActions, err)
	}
	if _, err := pool.Exec(ctx, `UPDATE realtime_refunds SET reason='mutated' WHERE contract_id=$1`, contractID); err == nil {
		t.Fatal("append-only realtime refund accepted mutation")
	}

	if python := strings.TrimSpace(os.Getenv("MERC_TEST_OPENAI_PYTHON")); python != "" {
		command := exec.CommandContext(ctx, python, "../scripts/realtime-openai-python-conformance.py")
		command.Env = append(os.Environ(),
			"MERC_CONFORMANCE_ORIGIN="+server.URL,
			"MERC_CONFORMANCE_API_KEY="+buyerKey,
		)
		output, err := command.CombinedOutput()
		if err != nil {
			t.Fatalf("official OpenAI Python SDK conformance failed: %v\n%s", err, output)
		}
		var result struct {
			Status               string `json:"status"`
			OpenAIPythonVersion  string `json:"openai_python_version"`
			JSONCompletion       bool   `json:"json_completion"`
			StreamingCompletion  bool   `json:"streaming_completion"`
			ReceiptVerified      bool   `json:"receipt_verified"`
			AuthorizationReceipt bool   `json:"authorization_receipt"`
			ModelsList           bool   `json:"models_list"`
			ParallelToolCalls    bool   `json:"parallel_tool_calls"`
			StructuredOutput     bool   `json:"structured_output"`
		}
		if err := json.Unmarshal(bytes.TrimSpace(output), &result); err != nil {
			t.Fatalf("invalid official SDK conformance evidence: %v\n%s", err, output)
		}
		if result.Status != "PASS" || result.OpenAIPythonVersion == "" ||
			!result.JSONCompletion || !result.StreamingCompletion || !result.ReceiptVerified || !result.AuthorizationReceipt || !result.ModelsList ||
			!result.ParallelToolCalls || !result.StructuredOutput {
			t.Fatalf("incomplete official SDK conformance result: %+v", result)
		}
		t.Logf("official OpenAI Python SDK %s conformance: PASS", result.OpenAIPythonVersion)
	}
	if node := strings.TrimSpace(os.Getenv("MERC_TEST_OPENAI_NODE")); node != "" {
		modulePath := strings.TrimSpace(os.Getenv("MERC_TEST_OPENAI_NODE_MODULE"))
		version := strings.TrimSpace(os.Getenv("MERC_TEST_OPENAI_NODE_VERSION"))
		if modulePath == "" || version == "" {
			t.Fatal("MERC_TEST_OPENAI_NODE_MODULE and MERC_TEST_OPENAI_NODE_VERSION are required with MERC_TEST_OPENAI_NODE")
		}
		command := exec.CommandContext(ctx, node, "../scripts/realtime-openai-node-conformance.mjs")
		command.Env = append(os.Environ(),
			"MERC_CONFORMANCE_ORIGIN="+server.URL,
			"MERC_CONFORMANCE_API_KEY="+buyerKey,
			"MERC_OPENAI_NODE_MODULE="+modulePath,
			"MERC_OPENAI_NODE_VERSION="+version,
		)
		output, err := command.CombinedOutput()
		if err != nil {
			t.Fatalf("official OpenAI JavaScript SDK conformance failed: %v\n%s", err, output)
		}
		var result struct {
			Status               string `json:"status"`
			OpenAINodeVersion    string `json:"openai_node_version"`
			JSONCompletion       bool   `json:"json_completion"`
			StreamingCompletion  bool   `json:"streaming_completion"`
			ReceiptVerified      bool   `json:"receipt_verified"`
			AuthorizationReceipt bool   `json:"authorization_receipt"`
			ModelsList           bool   `json:"models_list"`
			ParallelToolCalls    bool   `json:"parallel_tool_calls"`
			StructuredOutput     bool   `json:"structured_output"`
		}
		if err := json.Unmarshal(bytes.TrimSpace(output), &result); err != nil {
			t.Fatalf("invalid official JavaScript SDK conformance evidence: %v\n%s", err, output)
		}
		if result.Status != "PASS" || result.OpenAINodeVersion != version ||
			!result.JSONCompletion || !result.StreamingCompletion || !result.ReceiptVerified || !result.AuthorizationReceipt || !result.ModelsList ||
			!result.ParallelToolCalls || !result.StructuredOutput {
			t.Fatalf("incomplete official JavaScript SDK conformance result: %+v", result)
		}
		t.Logf("official OpenAI JavaScript SDK %s conformance: PASS", result.OpenAINodeVersion)
	}
	benchmarkOutput := filepath.Join(t.TempDir(), "fake-upstream-parity.json")
	benchmark := exec.CommandContext(ctx, "python3", "../scripts/realtime-parity-benchmark.py",
		"--merc-base-url", server.URL+"/v1",
		"--direct-base-url", upstream.URL+"/v1",
		"--samples", "5", "--warmups", "1", "--concurrency", "1",
		"--max-completion-tokens", "8", "--out", benchmarkOutput)
	benchmark.Env = append(os.Environ(),
		"MERC_BENCHMARK_API_KEY="+buyerKey,
		"MERC_DIRECT_VLLM_API_KEY="+upstreamToken,
	)
	if output, err := benchmark.CombinedOutput(); err != nil {
		t.Fatalf("parity benchmark harness integration failed: %v\n%s", err, output)
	}
	benchmarkRaw, err := os.ReadFile(benchmarkOutput)
	if err != nil {
		t.Fatal(err)
	}
	var benchmarkEvidence struct {
		EvidenceLevel       string `json:"evidence_level"`
		GatePassed          bool   `json:"gate_passed"`
		RealRuntimeAttested bool   `json:"real_runtime_attested"`
		PublicClaimAllowed  bool   `json:"public_claim_allowed"`
	}
	if err := json.Unmarshal(benchmarkRaw, &benchmarkEvidence); err != nil {
		t.Fatal(err)
	}
	if !benchmarkEvidence.GatePassed || benchmarkEvidence.RealRuntimeAttested ||
		benchmarkEvidence.PublicClaimAllowed || benchmarkEvidence.EvidenceLevel != "UNATTESTED_HARNESS_RUN" {
		t.Fatalf("benchmark harness mislabeled fake-upstream evidence: %+v", benchmarkEvidence)
	}

	failedUpstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, `data: {"id":"chatcmpl_worker_died","choices":[{"delta":{"content":"partial"}}],"usage":null}`+"\n\n")
		w.(http.Flusher).Flush()
		// A clean socket close without final usage simulates engine death after
		// the buyer has already received a prefix of the stream.
	}))
	defer failedUpstream.Close()
	if err := store.UpsertRealtimeOffer(ctx, WorkerAuth{WorkerID: workerID, SupplierID: supplierID}, RealtimeOfferRegistration{
		RuntimeProfileID: profile.RuntimeProfileID, RuntimeProfileSHA256: profile.ProfileSHA256,
		HWClass: "nvidia_24gb", GPUCount: 1, MemoryGBPerGPU: 24,
		UpstreamBaseURL: failedUpstream.URL + "/v1", UpstreamToken: upstreamToken,
		Warmth: "HOT", MaxActiveSequences: 8, AvailableSequences: 8,
		SupplierInputUSDPerMillionTokens: 0.08, SupplierOutputUSDPerMillionTokens: 0.30,
	}); err != nil {
		t.Fatal(err)
	}
	failedRequest, _ := http.NewRequestWithContext(ctx, http.MethodPost, server.URL+"/v1/chat/completions", bytes.NewReader(requestBody))
	failedRequest.Header.Set("Authorization", "Bearer "+buyerKey)
	failedRequest.Header.Set("Content-Type", "application/json")
	failedResponse, err := http.DefaultClient.Do(failedRequest)
	if err != nil {
		t.Fatal(err)
	}
	_, _ = io.ReadAll(failedResponse.Body)
	failedResponse.Body.Close()
	failedContractID, err := uuid.Parse(failedResponse.Header.Get("X-Merc-Contract-ID"))
	if err != nil {
		t.Fatalf("worker-death request did not create a contract: %v", err)
	}
	failedReceipt, err := store.RealtimeReceipt(ctx, buyerID, failedContractID)
	if err != nil {
		t.Fatal(err)
	}
	if failedReceipt.State != "FAILED" || failedReceipt.Verification != "FAILED" || failedReceipt.FailureCode != "usage_reconciliation_failed" {
		t.Fatalf("worker death was not failed and receipted: %+v", failedReceipt)
	}
	if failedReceipt.AuthorizationState != "VOIDED" || failedReceipt.AuthorizedUSD <= 0 ||
		failedReceipt.VoidedUSD != failedReceipt.AuthorizedUSD || failedReceipt.CapturedUSD != 0 {
		t.Fatalf("worker-death reservation was not fully voided: %+v", failedReceipt)
	}
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM ledger_entries WHERE execution_contract_id=$1`, failedContractID).Scan(&rows); err != nil || rows != 0 {
		t.Fatalf("failed worker stream created money effects: rows=%d err=%v", rows, err)
	}

	// A heartbeat reports the worker's raw capacity, but the control plane must
	// subtract its own executing reservations instead of reopening a busy slot.
	if err := store.UpsertRealtimeOffer(ctx, WorkerAuth{WorkerID: workerID, SupplierID: supplierID}, RealtimeOfferRegistration{
		RuntimeProfileID: profile.RuntimeProfileID, RuntimeProfileSHA256: profile.ProfileSHA256,
		HWClass: "nvidia_24gb", GPUCount: 1, MemoryGBPerGPU: 24,
		UpstreamBaseURL: upstream.URL + "/v1", UpstreamToken: upstreamToken,
		Warmth: "HOT", MaxActiveSequences: 1, AvailableSequences: 1,
		SupplierInputUSDPerMillionTokens: 0.08, SupplierOutputUSDPerMillionTokens: 0.30,
	}); err != nil {
		t.Fatal(err)
	}
	authorize := func(label string) (RealtimeContract, error) {
		contract, _, err := store.AuthorizeRealtimeContract(ctx, RealtimeContractAuthorization{
			RequestID: "req-capacity-" + label + "-" + uuid.NewString(), BuyerID: buyerID,
			Profile: profile, InputCommitment: strings.Repeat("a", 64),
			RequestSHA256: strings.Repeat("b", 64), MaximumPriceUSD: 0.001,
			EstimatedPriceUSD: 0.0005, DeadlineAt: time.Now().Add(time.Minute),
			MaximumPromptTokens: 8330, MaximumCompletionTokens: 1,
			EstimatedPromptTokens: 4163, EstimatedCompletionTokens: 1,
		})
		return contract, err
	}
	reserved, err := authorize("first")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `
		INSERT INTO realtime_authorization_events (contract_id,kind,amount_usd)
		VALUES ($1,'CAPTURED',0.000001)`, reserved.ID); err == nil {
		t.Fatal("database accepted a capture without verified settlement authority")
	}
	if err := store.HeartbeatRealtimeOffer(ctx, WorkerAuth{WorkerID: workerID, SupplierID: supplierID}, RealtimeOfferHeartbeat{
		RuntimeProfileID: profile.RuntimeProfileID, Warmth: "HOT", AvailableSequences: 1, Status: "ACTIVE",
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := authorize("overbook"); !errors.Is(err, errRealtimeNoSupply) {
		t.Fatalf("heartbeat reopened an executing slot: %v", err)
	}
	if finalized, err := store.FinalizeRealtimeFailure(ctx, reserved.ID, uuid.New(), 0, 1, "capacity_test", "capacity test cleanup", false); err != nil || !finalized {
		t.Fatal(err)
	}
	released, err := authorize("released")
	if err != nil {
		t.Fatalf("finalization did not release the reserved slot: %v", err)
	}
	if finalized, err := store.FinalizeRealtimeFailure(ctx, released.ID, uuid.New(), 0, 1, "capacity_test", "capacity test cleanup", false); err != nil || !finalized {
		t.Fatal(err)
	}

	// A control-process crash can leave an EXECUTING row after its request
	// context is gone. Recovery must create failure evidence once, restore the
	// slot, and never create a financial effect.
	if err := store.UpsertRealtimeOffer(ctx, WorkerAuth{WorkerID: workerID, SupplierID: supplierID}, RealtimeOfferRegistration{
		RuntimeProfileID: profile.RuntimeProfileID, RuntimeProfileSHA256: profile.ProfileSHA256,
		HWClass: "nvidia_24gb", GPUCount: 1, MemoryGBPerGPU: 24,
		UpstreamBaseURL: upstream.URL + "/v1", UpstreamToken: upstreamToken,
		Warmth: "HOT", MaxActiveSequences: 1, AvailableSequences: 1,
		SupplierInputUSDPerMillionTokens: 0.08, SupplierOutputUSDPerMillionTokens: 0.30,
	}); err != nil {
		t.Fatal(err)
	}
	// RealtimeOperationalSnapshot counts platform-wide, and the earlier phases of
	// this test leave their own EXECUTING contracts behind, so assert on the
	// delta this step produces rather than on an absolute count.  Asserting the
	// absolute value only held on a database no other test had touched, which is
	// not a property a shared suite can offer.
	baseline, err := store.RealtimeOperationalSnapshot(ctx)
	if err != nil {
		t.Fatalf("baseline snapshot: %v", err)
	}
	stale, err := authorize("stale")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `UPDATE execution_contracts
		SET deadline_at=now()-make_interval(secs=>$2::double precision)
		WHERE id=$1`, stale.ID, (realtimeRecoveryGrace + time.Second).Seconds()); err != nil {
		t.Fatal(err)
	}
	beforeRecovery, err := store.RealtimeOperationalSnapshot(ctx)
	if err != nil {
		t.Fatalf("pre-recovery snapshot: %v err=%v", beforeRecovery, err)
	}
	if got := beforeRecovery.ExecutingContracts - baseline.ExecutingContracts; got != 1 {
		t.Fatalf("stale authorize added %d executing contracts, want 1 (baseline %+v, now %+v)",
			got, baseline, beforeRecovery)
	}
	if beforeRecovery.AvailableSequences != 0 {
		t.Fatalf("offer should be fully reserved, got %d available", beforeRecovery.AvailableSequences)
	}
	startRecovery := make(chan struct{})
	recoveryCounts := make([]int, 2)
	recoveryErrors := make([]error, 2)
	var recoveryCalls sync.WaitGroup
	for i := range recoveryCounts {
		recoveryCalls.Add(1)
		go func(index int) {
			defer recoveryCalls.Done()
			<-startRecovery
			recoveryCounts[index], recoveryErrors[index] = store.RecoverStaleRealtimeContracts(ctx, realtimeRecoveryGrace, sweepBatch)
		}(i)
	}
	close(startRecovery)
	recoveryCalls.Wait()
	if recoveryErrors[0] != nil || recoveryErrors[1] != nil || recoveryCounts[0]+recoveryCounts[1] != 1 {
		t.Fatalf("concurrent stale recovery counts=%v errors=%v", recoveryCounts, recoveryErrors)
	}
	if recovered, err := store.RecoverStaleRealtimeContracts(ctx, realtimeRecoveryGrace, sweepBatch); err != nil || recovered != 0 {
		t.Fatalf("realtime recovery was not idempotent: count=%d err=%v", recovered, err)
	}
	staleReceipt, err := store.RealtimeReceipt(ctx, buyerID, stale.ID)
	if err != nil {
		t.Fatal(err)
	}
	if staleReceipt.State != "FAILED" || staleReceipt.FailureCode != "control_recovery_timeout" || staleReceipt.Verification != "FAILED" {
		t.Fatalf("stale contract did not receive recovery evidence: %+v", staleReceipt)
	}
	if staleReceipt.AuthorizationState != "VOIDED" || staleReceipt.VoidedUSD != staleReceipt.AuthorizedUSD {
		t.Fatalf("recovered contract reservation was not voided: %+v", staleReceipt)
	}
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM ledger_entries WHERE execution_contract_id=$1`, stale.ID).Scan(&rows); err != nil || rows != 0 {
		t.Fatalf("recovered stale contract created money effects: rows=%d err=%v", rows, err)
	}
	// Delta again, for the same reason as the pre-recovery assertion: recovery
	// must return this contract's own reservation, which is a change of one
	// against whatever else the suite has left EXECUTING.
	afterRecovery, err := store.RealtimeOperationalSnapshot(ctx)
	if err != nil {
		t.Fatalf("post-recovery snapshot: %v", err)
	}
	if got := beforeRecovery.ExecutingContracts - afterRecovery.ExecutingContracts; got != 1 {
		t.Fatalf("recovery cleared %d executing contracts, want exactly the stale one (before %+v, after %+v)",
			got, beforeRecovery, afterRecovery)
	}
	if afterRecovery.AvailableSequences != 1 {
		t.Fatalf("recovery must return the reserved sequence, got %d available", afterRecovery.AvailableSequences)
	}

	// A buyer disconnect must cancel the upstream request and finalize the
	// contract without a debit or supplier payable.
	upstreamStarted := make(chan struct{}, 1)
	upstreamCancelled := make(chan struct{}, 1)
	releaseUpstream := make(chan struct{})
	cancelledUpstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, `data: {"id":"chatcmpl_cancel","choices":[{"delta":{"content":"partial"}}],"usage":null}`+"\n\n")
		w.(http.Flusher).Flush()
		upstreamStarted <- struct{}{}
		ticker := time.NewTicker(10 * time.Millisecond)
		defer ticker.Stop()
		for {
			select {
			case <-r.Context().Done():
				upstreamCancelled <- struct{}{}
				return
			case <-releaseUpstream:
				return
			case <-ticker.C:
				if _, err := io.WriteString(w, ": keepalive\n\n"); err != nil {
					upstreamCancelled <- struct{}{}
					return
				}
				w.(http.Flusher).Flush()
			}
		}
	}))
	defer cancelledUpstream.Close()
	defer close(releaseUpstream)
	if err := store.UpsertRealtimeOffer(ctx, WorkerAuth{WorkerID: workerID, SupplierID: supplierID}, RealtimeOfferRegistration{
		RuntimeProfileID: profile.RuntimeProfileID, RuntimeProfileSHA256: profile.ProfileSHA256,
		HWClass: "nvidia_24gb", GPUCount: 1, MemoryGBPerGPU: 24,
		UpstreamBaseURL: cancelledUpstream.URL + "/v1", UpstreamToken: upstreamToken,
		Warmth: "HOT", MaxActiveSequences: 1, AvailableSequences: 1,
		SupplierInputUSDPerMillionTokens: 0.08, SupplierOutputUSDPerMillionTokens: 0.30,
	}); err != nil {
		t.Fatal(err)
	}
	cancelKey := "realtime-cancel-" + suffix
	cancelContext, cancelRequest := context.WithCancel(ctx)
	disconnectRequest, _ := http.NewRequestWithContext(cancelContext, http.MethodPost,
		server.URL+"/v1/chat/completions", bytes.NewReader(requestBody))
	disconnectRequest.Header.Set("Authorization", "Bearer "+buyerKey)
	disconnectRequest.Header.Set("Content-Type", "application/json")
	disconnectRequest.Header.Set("Idempotency-Key", cancelKey)
	requestResult := make(chan error, 1)
	go func() {
		response, err := http.DefaultClient.Do(disconnectRequest)
		if response != nil {
			_, readErr := io.Copy(io.Discard, response.Body)
			response.Body.Close()
			if err == nil {
				err = readErr
			}
		}
		requestResult <- err
	}()
	select {
	case <-upstreamStarted:
	case <-time.After(2 * time.Second):
		t.Fatal("cancellation upstream was never reached")
	}
	cancelRequest()
	if err := <-requestResult; err == nil {
		t.Fatal("disconnected client unexpectedly received a complete response")
	}
	select {
	case <-upstreamCancelled:
	case <-time.After(2 * time.Second):
		t.Fatal("buyer disconnect did not cancel the in-flight upstream request")
	}
	var cancelledID uuid.UUID
	cancelledState := ""
	pollDeadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(pollDeadline) {
		err = pool.QueryRow(ctx, `SELECT id,state FROM execution_contracts
			WHERE buyer_id=$1 AND idempotency_key=$2`, buyerID, cancelKey).Scan(&cancelledID, &cancelledState)
		if err == nil && cancelledState == "CANCELLED" {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if cancelledState != "CANCELLED" {
		t.Fatalf("buyer disconnect did not cancel its contract: id=%s state=%q err=%v", cancelledID, cancelledState, err)
	}
	cancelledReceipt, err := store.RealtimeReceipt(ctx, buyerID, cancelledID)
	if err != nil {
		t.Fatal(err)
	}
	if cancelledReceipt.FailureCode != "client_cancelled" || cancelledReceipt.Verification != "FAILED" {
		t.Fatalf("buyer disconnect did not receive cancellation evidence: %+v", cancelledReceipt)
	}
	if cancelledReceipt.AuthorizationState != "VOIDED" || cancelledReceipt.VoidedUSD != cancelledReceipt.AuthorizedUSD {
		t.Fatalf("cancelled contract reservation was not voided: %+v", cancelledReceipt)
	}
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM ledger_entries WHERE execution_contract_id=$1`, cancelledID).Scan(&rows); err != nil || rows != 0 {
		t.Fatalf("cancelled contract created money effects: rows=%d err=%v", rows, err)
	}
	metricsResponse, err := http.Get(server.URL + "/metrics")
	if err != nil {
		t.Fatal(err)
	}
	metricsBody, err := io.ReadAll(metricsResponse.Body)
	metricsResponse.Body.Close()
	if err != nil || metricsResponse.StatusCode != http.StatusOK {
		t.Fatalf("realtime metrics unavailable: status=%d err=%v", metricsResponse.StatusCode, err)
	}
	for _, required := range []string{
		"merc_realtime_contracts_authorized_total ",
		"merc_realtime_contracts_verified_total ",
		"merc_realtime_contracts_failed_total ",
		"merc_realtime_contracts_cancelled_total ",
		"merc_realtime_finalization_errors_total ",
		"merc_realtime_refunds_total 1",
		"merc_realtime_executing_contracts 0",
		"merc_realtime_available_sequences 1",
		"merc_realtime_internal_refunds_usd ",
	} {
		if !bytes.Contains(metricsBody, []byte(required)) {
			t.Fatalf("realtime metrics output is missing %q", required)
		}
	}
}
