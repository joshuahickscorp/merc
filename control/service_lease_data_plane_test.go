package main

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/google/uuid"
)

func TestServiceLeaseDataPlaneUsesReservedWorkerAndFailsClosed(t *testing.T) {
	installSettlementCurrencyForTest(t, "cad")
	t.Setenv("MERC_TOKEN_KEY", "service-lease-data-plane-test-key-with-at-least-32-bytes")
	ctx, store, pool := openPayoutTestStore(t)
	profile := sortedVLLMProfiles()[0]
	buyerID := uuid.New()
	if _, err := pool.Exec(ctx, `INSERT INTO buyers (id,email) VALUES ($1,$2)`, buyerID, buyerID.String()+"@service-data-plane.invalid"); err != nil {
		t.Fatal(err)
	}
	if err := store.SeedPrepaidBalance(ctx, buyerID, 1_000_000, "service-data-plane-"+buyerID.String()); err != nil {
		t.Fatal(err)
	}
	_, buyerKey, _, err := store.CreateAPIKey(ctx, buyerID, "service-data-plane", true)
	if err != nil {
		t.Fatal(err)
	}
	worker, workerToken := newFabricMeasurementWorker(t, ctx, store)

	var upstreamCalls atomic.Int32
	upstreamToken := "cx_vllm_service_lease_test_token_123456"
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		upstreamCalls.Add(1)
		if r.Method != http.MethodPost || r.URL.Path != "/v1/chat/completions" {
			t.Errorf("unexpected upstream request: %s %s", r.Method, r.URL.Path)
		}
		if got := r.Header.Get("Authorization"); got != "Bearer "+upstreamToken {
			t.Errorf("upstream bearer=%q", got)
		}
		body, readErr := io.ReadAll(r.Body)
		if readErr != nil {
			t.Errorf("read upstream body: %v", readErr)
		}
		var payload map[string]any
		if err := json.Unmarshal(body, &payload); err != nil || payload["model"] != profile.ModelAlias {
			t.Errorf("upstream body=%s err=%v", body, err)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"id":"lease-chat-1","choices":[{"message":{"role":"assistant","content":"hello"}}]}`)
	}))
	defer upstream.Close()

	handler := NewServer(store, nil, nil, nil).Routes()
	post := func(path, token string, body any, workerAuth bool) *httptest.ResponseRecorder {
		raw, marshalErr := json.Marshal(body)
		if marshalErr != nil {
			t.Fatal(marshalErr)
		}
		req := httptest.NewRequest(http.MethodPost, path, bytes.NewReader(raw))
		if workerAuth {
			req.Header.Set("X-Worker-Token", token)
		} else {
			req.Header.Set("Authorization", "Bearer "+token)
		}
		if strings.Contains(path, "/worker/realtime/register") {
			// The registration validator permits loopback HTTP only when the
			// authenticated worker request itself is loopback.
			req.RemoteAddr = "127.0.0.1:12345"
		}
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, req)
		return rec
	}

	seedMeasuredWarmResidency(t, ctx, pool, worker.WorkerID, profile.ModelAlias)
	serviceOffer := serviceLeaseOffer(profile)
	serviceOffer.Region = "ca-data-plane-" + uuid.NewString()
	serviceOffer.MaximumWarmReplicas = 1
	serviceOffer.AvailableWarmReplicas = 1
	if got := post("/v1/worker/service-leases/offers", workerToken, serviceOffer, true).Code; got != http.StatusOK {
		t.Fatalf("service offer status=%d", got)
	}
	realtimeOffer := RealtimeOfferRegistration{
		RuntimeProfileID: profile.RuntimeProfileID, RuntimeProfileSHA256: profile.ProfileSHA256,
		HWClass: "nvidia_24gb", GPUCount: 1, MemoryGBPerGPU: 24,
		UpstreamBaseURL: upstream.URL + "/v1", UpstreamToken: upstreamToken,
		Warmth: "HOT", MaxActiveSequences: 2, AvailableSequences: 2,
		SupplierInputUSDPerMillionTokens: 0.08, SupplierOutputUSDPerMillionTokens: 0.30,
	}
	if got := post("/v1/worker/realtime/register", workerToken, realtimeOffer, true).Code; got != http.StatusOK {
		t.Fatalf("realtime offer status=%d", got)
	}
	leaseRequest := ServiceLeaseRequest{
		RuntimeProfileID: profile.RuntimeProfileID, Region: serviceOffer.Region,
		MinimumReplicas: 1, MaximumReplicas: 1, TermSeconds: 60,
		MaximumP95LatencyMilliseconds: 500, BuyerDeclaredCeilingNanos: 135_000_000,
	}
	created := post("/v1/service-leases", buyerKey, leaseRequest, false)
	if created.Code != http.StatusCreated {
		t.Fatalf("service lease status=%d body=%s", created.Code, created.Body.String())
	}
	var lease ServiceLease
	if err := json.Unmarshal(created.Body.Bytes(), &lease); err != nil {
		t.Fatal(err)
	}
	if lease.WorkerID != worker.WorkerID || lease.State != "ACTIVE" {
		t.Fatalf("lease did not bind reserved worker: %+v", lease)
	}

	requestBody := map[string]any{
		"model":      profile.ModelAlias,
		"messages":   []any{map[string]any{"role": "user", "content": "hello"}},
		"max_tokens": 1,
	}
	raw, err := json.Marshal(requestBody)
	if err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequest(http.MethodPost, "/v1/service-leases/"+lease.ID.String()+"/chat/completions", bytes.NewReader(raw))
	req.Header.Set("Authorization", "Bearer "+buyerKey)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK || rec.Body.String() == "" {
		t.Fatalf("service data-plane status=%d body=%s", rec.Code, rec.Body.String())
	}
	if rec.Header().Get("X-Merc-Service-Lease-ID") != lease.ID.String() ||
		rec.Header().Get("X-Merc-Service-Lease-Metering") != "replica_nanoseconds" ||
		rec.Header().Get("X-Merc-Receipt") != "/v1/service-leases/"+lease.ID.String()+"/receipt" {
		t.Fatalf("service data-plane receipt headers=%v", rec.Header())
	}
	if got := upstreamCalls.Load(); got != 1 {
		t.Fatalf("upstream calls=%d want 1", got)
	}

	foreignID := uuid.New()
	if _, err := pool.Exec(ctx, `INSERT INTO buyers (id,email) VALUES ($1,$2)`, foreignID, foreignID.String()+"@service-data-plane-foreign.invalid"); err != nil {
		t.Fatal(err)
	}
	_, foreignKey, _, err := store.CreateAPIKey(ctx, foreignID, "foreign", true)
	if err != nil {
		t.Fatal(err)
	}
	foreignReq := httptest.NewRequest(http.MethodPost, "/v1/service-leases/"+lease.ID.String()+"/chat/completions", bytes.NewReader(raw))
	foreignReq.Header.Set("Authorization", "Bearer "+foreignKey)
	foreignRec := httptest.NewRecorder()
	handler.ServeHTTP(foreignRec, foreignReq)
	if foreignRec.Code != http.StatusNotFound || upstreamCalls.Load() != 1 {
		t.Fatalf("foreign lease access status=%d upstream_calls=%d", foreignRec.Code, upstreamCalls.Load())
	}

	streamBody := map[string]any{"model": profile.ModelAlias, "messages": requestBody["messages"], "stream": true}
	streamRec := post("/v1/service-leases/"+lease.ID.String()+"/chat/completions", buyerKey, streamBody, false)
	if streamRec.Code != http.StatusBadRequest || upstreamCalls.Load() != 1 {
		t.Fatalf("stream service request status=%d upstream_calls=%d", streamRec.Code, upstreamCalls.Load())
	}

	if got := post("/v1/worker/realtime/heartbeat", workerToken, RealtimeOfferHeartbeat{
		RuntimeProfileID: profile.RuntimeProfileID, Warmth: "COLD", AvailableSequences: 0, Status: "FAILED",
	}, true).Code; got != http.StatusNoContent {
		t.Fatalf("failed realtime heartbeat status=%d", got)
	}
	staleReq := httptest.NewRequest(http.MethodPost, "/v1/service-leases/"+lease.ID.String()+"/chat/completions", bytes.NewReader(raw))
	staleReq.Header.Set("Authorization", "Bearer "+buyerKey)
	staleRec := httptest.NewRecorder()
	handler.ServeHTTP(staleRec, staleReq)
	if staleRec.Code != http.StatusServiceUnavailable || upstreamCalls.Load() != 1 {
		t.Fatalf("stale service target status=%d upstream_calls=%d", staleRec.Code, upstreamCalls.Load())
	}

}
