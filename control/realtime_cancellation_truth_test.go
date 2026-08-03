package main

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
)

// TestRealtimeJSONBodyReadCancelRecordsCancelled covers the non-stream
// upstream_response_invalid site: a buyer disconnect while the JSON body is
// still being read must finalize CANCELLED (not FAILED with a hard-coded
// cancelled=false).
func TestRealtimeJSONBodyReadCancelRecordsCancelled(t *testing.T) {
	installSettlementCurrencyForTest(t, "usd")
	t.Setenv("MERC_TOKEN_KEY", "realtime-json-cancel-body-key-with-at-least-32b")
	ctx, store, pool := openIsolatedTestStore(t)
	profile, supplierID, workerID := realtimeFundingFixture(t, ctx, store, pool)

	buyerID, err := store.CreateBuyerAccount(ctx, "json-body-cancel-"+uuid.NewString()+"@example.test", "pw", 5)
	if err != nil {
		t.Fatal(err)
	}
	_, buyerKey, _, err := store.CreateAPIKey(ctx, buyerID, "json-body-cancel", true)
	if err != nil {
		t.Fatal(err)
	}

	upstreamStarted := make(chan struct{}, 1)
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		// Partial body only — hold the connection open so the gateway's
		// ReadAll is in flight when the buyer disconnects.
		if f, ok := w.(http.Flusher); ok {
			_, _ = io.WriteString(w, `{"id":"chatcmpl_partial","object":"chat.completion","choices":[`)
			f.Flush()
		}
		select {
		case upstreamStarted <- struct{}{}:
		default:
		}
		<-r.Context().Done()
	}))
	defer upstream.Close()
	if err := store.UpsertRealtimeOffer(ctx, WorkerAuth{WorkerID: workerID, SupplierID: supplierID}, RealtimeOfferRegistration{
		RuntimeProfileID: profile.RuntimeProfileID, RuntimeProfileSHA256: profile.ProfileSHA256,
		HWClass: "nvidia_24gb", GPUCount: 1, MemoryGBPerGPU: 24,
		UpstreamBaseURL: upstream.URL + "/v1", UpstreamToken: "rt-json-body-cancel",
		Warmth: "HOT", MaxActiveSequences: 8, AvailableSequences: 8,
		SupplierInputUSDPerMillionTokens: 0.08, SupplierOutputUSDPerMillionTokens: 0.30,
	}); err != nil {
		t.Fatal(err)
	}

	server := httptest.NewServer(NewServer(store, nil, nil, nil).Routes())
	defer server.Close()

	cancelCtx, cancelRequest := context.WithCancel(ctx)
	req, err := http.NewRequestWithContext(cancelCtx, http.MethodPost, server.URL+"/v1/chat/completions",
		bytes.NewReader([]byte(`{"model":"cx-chat-1b","messages":[{"role":"user","content":"hi"}],"stream":false,"max_tokens":8}`)))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer "+buyerKey)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Idempotency-Key", "json-body-cancel-"+uuid.NewString())

	done := make(chan error, 1)
	go func() {
		resp, err := http.DefaultClient.Do(req)
		if resp != nil {
			_, _ = io.Copy(io.Discard, resp.Body)
			resp.Body.Close()
		}
		done <- err
	}()

	select {
	case <-upstreamStarted:
	case <-time.After(3 * time.Second):
		t.Fatal("upstream never saw the non-stream request")
	}
	cancelRequest()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("request did not return after buyer cancel")
	}

	contractID, state := waitRealtimeTerminalState(t, ctx, pool, buyerID, 3*time.Second)
	if state != "CANCELLED" {
		t.Fatalf("buyer disconnect during JSON body read recorded state=%q contract=%s, want CANCELLED", state, contractID)
	}
	receipt, err := store.RealtimeReceipt(ctx, buyerID, contractID)
	if err != nil {
		t.Fatal(err)
	}
	if receipt.FailureCode != "client_cancelled" {
		t.Fatalf("failure_code=%q, want client_cancelled (cancellation truth, not a hard-coded non-cancel failure)", receipt.FailureCode)
	}
	var ledgerRows int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM ledger_entries WHERE execution_contract_id=$1`, contractID).Scan(&ledgerRows); err != nil {
		t.Fatal(err)
	}
	if ledgerRows != 0 {
		t.Fatalf("incomplete cancelled JSON body must not settle money: ledger_rows=%d", ledgerRows)
	}
}

// TestRealtimeJSONSettlementSurvivesBuyerCancel covers the non-stream
// settlement_failed site: once the upstream JSON body is fully in hand, a
// buyer disconnect must not abandon settlement. The streaming path already
// detaches settlement from the request context; non-stream must match.
//
// Mechanism: wait for EXECUTING, hold FOR UPDATE on that row, then release
// the upstream body. Settlement blocks on our lock; cancel the buyer while
// blocked; release. With r.Context() settlement the cancelled context voids
// the work; with a detached settlement context the delivered work settles
// and money conserves.
func TestRealtimeJSONSettlementSurvivesBuyerCancel(t *testing.T) {
	installSettlementCurrencyForTest(t, "usd")
	t.Setenv("MERC_TOKEN_KEY", "realtime-json-settle-cancel-key-with-32b!!!!")
	ctx, store, pool := openIsolatedTestStore(t)
	profile, supplierID, workerID := realtimeFundingFixture(t, ctx, store, pool)

	buyerID, err := store.CreateBuyerAccount(ctx, "json-settle-cancel-"+uuid.NewString()+"@example.test", "pw", 5)
	if err != nil {
		t.Fatal(err)
	}
	_, buyerKey, _, err := store.CreateAPIKey(ctx, buyerID, "json-settle-cancel", true)
	if err != nil {
		t.Fatal(err)
	}

	const jsonBody = `{"id":"chatcmpl_settle_cancel","object":"chat.completion","created":1,"model":"cx-chat-1b","choices":[{"index":0,"message":{"role":"assistant","content":"hello"},"finish_reason":"stop"}],"usage":{"prompt_tokens":7,"completion_tokens":2,"total_tokens":9}}`
	// Gate the upstream body until the test holds the contract row lock, so
	// settlement cannot race past us before the buyer is cancelled.
	releaseUpstream := make(chan struct{})
	upstreamSeen := make(chan struct{}, 1)
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case upstreamSeen <- struct{}{}:
		default:
		}
		select {
		case <-releaseUpstream:
		case <-time.After(10 * time.Second):
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, jsonBody)
	}))
	defer upstream.Close()
	if err := store.UpsertRealtimeOffer(ctx, WorkerAuth{WorkerID: workerID, SupplierID: supplierID}, RealtimeOfferRegistration{
		RuntimeProfileID: profile.RuntimeProfileID, RuntimeProfileSHA256: profile.ProfileSHA256,
		HWClass: "nvidia_24gb", GPUCount: 1, MemoryGBPerGPU: 24,
		UpstreamBaseURL: upstream.URL + "/v1", UpstreamToken: "rt-json-settle-cancel",
		Warmth: "HOT", MaxActiveSequences: 8, AvailableSequences: 8,
		SupplierInputUSDPerMillionTokens: 0.08, SupplierOutputUSDPerMillionTokens: 0.30,
	}); err != nil {
		t.Fatal(err)
	}

	server := httptest.NewServer(NewServer(store, nil, nil, nil).Routes())
	defer server.Close()

	cancelCtx, cancelRequest := context.WithCancel(ctx)
	req, err := http.NewRequestWithContext(cancelCtx, http.MethodPost, server.URL+"/v1/chat/completions",
		bytes.NewReader([]byte(`{"model":"cx-chat-1b","messages":[{"role":"user","content":"hi"}],"stream":false,"max_tokens":8}`)))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer "+buyerKey)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Idempotency-Key", "json-settle-cancel-"+uuid.NewString())

	done := make(chan error, 1)
	go func() {
		resp, err := http.DefaultClient.Do(req)
		if resp != nil {
			_, _ = io.Copy(io.Discard, resp.Body)
			resp.Body.Close()
		}
		done <- err
	}()

	select {
	case <-upstreamSeen:
	case <-time.After(5 * time.Second):
		t.Fatal("upstream never saw the request")
	}

	// Contract is EXECUTING (authorized) while upstream holds. Lock the row so
	// FinalizeRealtimeSuccess blocks after the body is released.
	var contractID uuid.UUID
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		err := pool.QueryRow(ctx, `
			SELECT id FROM execution_contracts
			 WHERE buyer_id=$1 AND state='EXECUTING'
			 ORDER BY created_at DESC LIMIT 1`, buyerID).Scan(&contractID)
		if err == nil {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	if contractID == uuid.Nil {
		t.Fatal("no EXECUTING contract while upstream held")
	}
	lockTx, err := pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := lockTx.Exec(ctx, `SELECT id FROM execution_contracts WHERE id=$1 FOR UPDATE`, contractID); err != nil {
		_ = lockTx.Rollback(ctx)
		t.Fatal(err)
	}

	// Upstream returns full body; gateway reads it and enters settlement, which
	// blocks on our FOR UPDATE. Then cancel the buyer so r.Context() is dead.
	close(releaseUpstream)
	time.Sleep(30 * time.Millisecond)
	cancelRequest()
	// Settlement is either blocked on the row or already failed Begin(ctx) on
	// the unmodified tree. Release the row either way.
	time.Sleep(30 * time.Millisecond)
	if err := lockTx.Rollback(ctx); err != nil {
		t.Fatal(err)
	}

	select {
	case <-done:
	case <-time.After(8 * time.Second):
		t.Fatal("request did not return after settlement lock release")
	}

	_, state := waitRealtimeTerminalState(t, ctx, pool, buyerID, 5*time.Second)
	if state != "VERIFIED" {
		// Unmodified tree voids as FAILED/settlement_failed when r.Context() is dead.
		t.Fatalf("delivered non-stream work after buyer cancel recorded state=%q contract=%s, want VERIFIED (settled)", state, contractID)
	}
	receipt, err := store.RealtimeReceipt(ctx, buyerID, contractID)
	if err != nil {
		t.Fatal(err)
	}
	if receipt.AuthorizationState != "CAPTURED" || receipt.BuyerChargeUSD <= 0 {
		t.Fatalf("settled receipt missing capture: %+v", receipt)
	}
	if receipt.SupplierPayableUSD <= 0 || receipt.SupplierPayableUSD > receipt.BuyerChargeUSD {
		t.Fatalf("supplier not paid for delivered work: %+v", receipt)
	}
	// Money conserves: buyer charge = supplier credit + platform take (ledger net zero).
	var rows int
	var net float64
	if err := pool.QueryRow(ctx, `
		SELECT count(*),COALESCE(sum(amount_usd),0)::float8
		  FROM ledger_entries WHERE execution_contract_id=$1`, contractID).Scan(&rows, &net); err != nil {
		t.Fatal(err)
	}
	if rows != 3 || net < -0.0000001 || net > 0.0000001 {
		t.Fatalf("ledger does not conserve money after cancel+settle: rows=%d net=%f", rows, net)
	}
	var buyerCharge, supplierCredit, platformTake float64
	if err := pool.QueryRow(ctx, `
		SELECT COALESCE(sum(-amount_usd) FILTER (WHERE kind='buyer_charge'),0)::float8,
		       COALESCE(sum(amount_usd) FILTER (WHERE kind='supplier_credit'),0)::float8,
		       COALESCE(sum(amount_usd) FILTER (WHERE kind='platform_take'),0)::float8
		  FROM ledger_entries WHERE execution_contract_id=$1`, contractID).
		Scan(&buyerCharge, &supplierCredit, &platformTake); err != nil {
		t.Fatal(err)
	}
	if diff := buyerCharge - (supplierCredit + platformTake); diff < -0.0000001 || diff > 0.0000001 {
		t.Fatalf("buyer charge %f != supplier %f + platform %f", buyerCharge, supplierCredit, platformTake)
	}
}

func waitRealtimeTerminalState(t *testing.T, ctx context.Context, pool *pgxpool.Pool, buyerID uuid.UUID, timeout time.Duration) (uuid.UUID, string) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	var id uuid.UUID
	var state string
	var err error
	for time.Now().Before(deadline) {
		err = pool.QueryRow(ctx, `
			SELECT id,state FROM execution_contracts
			 WHERE buyer_id=$1 AND state IN ('FAILED','CANCELLED','VERIFIED')
			 ORDER BY created_at DESC LIMIT 1`, buyerID).Scan(&id, &state)
		if err == nil {
			return id, state
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("no terminal realtime contract for buyer %s within %s (last err=%v)", buyerID, timeout, err)
	return uuid.Nil, ""
}
