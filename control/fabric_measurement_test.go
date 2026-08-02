package main

import (
	"bytes"
	"context"
	"encoding/json"
	"math"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
)

func fabricReceipt() FabricLinkMeasurementReceipt {
	return FabricLinkMeasurementReceipt{
		SchemaVersion:          1,
		ReceiptID:              uuid.New(),
		Kind:                   "MERC_FABRIC_TCP_ECHO_RECEIPT",
		Status:                 "MEASURED_NOT_ADMISSIBLE",
		MeasuredAtUnixMS:       time.Now().UnixMilli(),
		DeclaredSite:           "supplier-lab-rack-a",
		PeerEndpointCommitment: strings.Repeat("a", 64),
		Transport:              "MERC_FABRIC_TCP_ECHO_V1",
		PeerAuthentication:     "HMAC_SHA256_OWNER_SHARED_PROBE_TOKEN",
		PayloadIsRandom:        true,
		P50RoundTripMicros:     1600,
		P95RoundTripMicros:     3200,
		P50PayloadGoodputMbps:  40.96,
		LocalClusterAdmissible: false,
		NonAdmissionReasons: []string{
			"the owner-shared probe token does not bind the peer to a control-plane worker identity",
		},
		Rounds: []FabricLinkMeasurementRound{
			{Round: 1, PayloadBytesEachDirection: 4096, RoundTripPayloadBytes: 8192, RoundTripMicros: 3200, PayloadGoodputMbps: 20.48, TranscriptSHA256: strings.Repeat("b", 64)},
			{Round: 2, PayloadBytesEachDirection: 4096, RoundTripPayloadBytes: 8192, RoundTripMicros: 1600, PayloadGoodputMbps: 40.96, TranscriptSHA256: strings.Repeat("c", 64)},
			{Round: 3, PayloadBytesEachDirection: 4096, RoundTripPayloadBytes: 8192, RoundTripMicros: 800, PayloadGoodputMbps: 81.92, TranscriptSHA256: strings.Repeat("d", 64)},
		},
	}
}

func newFabricMeasurementWorker(t *testing.T, ctx context.Context, store *Store) (WorkerAuth, string) {
	t.Helper()
	supplierID, workerID := uuid.New(), uuid.New()
	if _, err := store.pool.Exec(ctx, `INSERT INTO suppliers (id,email,status) VALUES ($1,$2,'active')`,
		supplierID, "fabric-"+uuid.NewString()+"@example.test"); err != nil {
		t.Fatal(err)
	}
	token, err := store.CreateWorkerToken(ctx, workerID, supplierID)
	if err != nil {
		t.Fatal(err)
	}
	return WorkerAuth{WorkerID: workerID, SupplierID: supplierID}, token
}

func requestFabricReceipt(t *testing.T, handler http.Handler, token string, raw []byte) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/v1/worker/fabric/receipts", bytes.NewReader(raw))
	req.Header.Set("X-Worker-Token", token)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	return rec
}

func requestFabricJSON(t *testing.T, handler http.Handler, token, path string, value any) *httptest.ResponseRecorder {
	t.Helper()
	body, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequest(http.MethodPost, path, bytes.NewReader(body))
	req.Header.Set("X-Worker-Token", token)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	return rec
}

func newFabricPeerWorker(t *testing.T, ctx context.Context, store *Store, supplierID uuid.UUID) (WorkerAuth, string) {
	t.Helper()
	workerID := uuid.New()
	token, err := store.CreateWorkerToken(ctx, workerID, supplierID)
	if err != nil {
		t.Fatal(err)
	}
	return WorkerAuth{WorkerID: workerID, SupplierID: supplierID}, token
}

func TestFabricReceiptIsWorkerAuthenticatedRecomputedImmutableAndNonAdmissible(t *testing.T) {
	ctx, store, pool := openPayoutTestStore(t)
	worker, token := newFabricMeasurementWorker(t, ctx, store)
	receipt := fabricReceipt()
	raw, err := json.Marshal(receipt)
	if err != nil {
		t.Fatal(err)
	}
	handler := NewServer(store, nil, nil, nil).Routes()
	if got := requestFabricReceipt(t, handler, token, raw).Code; got != http.StatusNoContent {
		t.Fatalf("fabric receipt status=%d", got)
	}
	// Retry exactly the same body: agent transport retries are bounded but may
	// occur after an ambiguous acknowledgement, so the receipt id is idempotent.
	if got := requestFabricReceipt(t, handler, token, raw).Code; got != http.StatusNoContent {
		t.Fatalf("fabric receipt retry status=%d", got)
	}

	var (
		classification string
		p50Latency     int64
		p95Latency     int64
		p50Goodput     float64
		storedWorker   uuid.UUID
	)
	if err := pool.QueryRow(ctx, `SELECT classification,p50_round_trip_micros,p95_round_trip_micros,
		p50_payload_goodput_mbps,reporting_worker_id FROM fabric_link_measurements WHERE receipt_id=$1`,
		receipt.ReceiptID).Scan(&classification, &p50Latency, &p95Latency, &p50Goodput, &storedWorker); err != nil {
		t.Fatal(err)
	}
	if classification != "SELF_REPORTED_UNQUALIFIED" || storedWorker != worker.WorkerID ||
		p50Latency != 1600 || p95Latency != 3200 || math.Abs(p50Goodput-40.96) > 0.000001 {
		t.Fatalf("fabric receipt was not stored as derived non-admissible evidence: class=%s worker=%s p50=%d p95=%d goodput=%f",
			classification, storedWorker, p50Latency, p95Latency, p50Goodput)
	}
	if _, err := pool.Exec(ctx, `UPDATE fabric_link_measurements SET p50_round_trip_micros=1 WHERE receipt_id=$1`, receipt.ReceiptID); err == nil {
		t.Fatal("database allowed immutable fabric evidence to be rewritten")
	}

	// A different credential cannot adopt an existing receipt id, even if it has
	// the exact body. Ownership is the submitting worker, not a JSON field.
	_, anotherToken := newFabricMeasurementWorker(t, ctx, store)
	if got := requestFabricReceipt(t, handler, anotherToken, raw).Code; got != http.StatusConflict {
		t.Fatalf("cross-worker receipt replay status=%d, want 409", got)
	}

	// Nor can a worker mutate the receipt after a successful upload.
	receipt.DeclaredSite = "different-site"
	mutated, err := json.Marshal(receipt)
	if err != nil {
		t.Fatal(err)
	}
	if got := requestFabricReceipt(t, handler, token, mutated).Code; got != http.StatusConflict {
		t.Fatalf("mutated receipt replay status=%d, want 409", got)
	}
}

func TestFabricReceiptRefusesSelfPromotionAndMalformedEvidence(t *testing.T) {
	ctx, store, _ := openPayoutTestStore(t)
	_, token := newFabricMeasurementWorker(t, ctx, store)
	handler := NewServer(store, nil, nil, nil).Routes()

	receipt := fabricReceipt()
	receipt.LocalClusterAdmissible = true
	raw, err := json.Marshal(receipt)
	if err != nil {
		t.Fatal(err)
	}
	if got := requestFabricReceipt(t, handler, token, raw).Code; got != http.StatusBadRequest {
		t.Fatalf("self-promoting receipt status=%d, want 400", got)
	}
	receipt = fabricReceipt()
	receipt.Rounds[0].PayloadGoodputMbps = 20.49
	raw, err = json.Marshal(receipt)
	if err != nil {
		t.Fatal(err)
	}
	if got := requestFabricReceipt(t, handler, token, raw).Code; got != http.StatusBadRequest {
		t.Fatalf("non-reproducible rate status=%d, want 400", got)
	}
	if got := requestFabricReceipt(t, handler, token, []byte(`{"receipt_id":"`+uuid.NewString()+`"}`)).Code; got != http.StatusBadRequest {
		t.Fatalf("incomplete receipt status=%d, want 400", got)
	}
}

func TestFabricSessionRequiresEveryRoundFromTheReservedPeerAndStaysNonAdmissible(t *testing.T) {
	ctx, store, pool := openPayoutTestStore(t)
	initiator, initiatorToken := newFabricMeasurementWorker(t, ctx, store)
	peer, peerToken := newFabricPeerWorker(t, ctx, store, initiator.SupplierID)
	handler := NewServer(store, nil, nil, nil).Routes()

	created := requestFabricJSON(t, handler, initiatorToken, "/v1/worker/fabric/sessions", FabricSessionCreateRequest{
		PeerWorkerID: peer.WorkerID, DeclaredSite: "supplier-lab-rack-a",
	})
	if created.Code != http.StatusCreated {
		t.Fatalf("fabric session status=%d body=%s", created.Code, created.Body.String())
	}
	var session FabricSessionCreateResponse
	if err := json.NewDecoder(created.Body).Decode(&session); err != nil || session.FabricSessionID == uuid.Nil {
		t.Fatalf("decode fabric session: session=%+v err=%v", session, err)
	}

	receipt := fabricReceipt()
	receipt.FabricSessionID = &session.FabricSessionID
	receipt.ExpectedPeerWorkerID = &peer.WorkerID
	for _, round := range receipt.Rounds {
		observed := requestFabricJSON(t, handler, peerToken, "/v1/worker/fabric/observations", FabricProbeObservation{
			SchemaVersion: 1, FabricSessionID: session.FabricSessionID,
			TranscriptSHA256: round.TranscriptSHA256, PayloadBytesEachDirection: round.PayloadBytesEachDirection,
			ObservedAtUnixMS: time.Now().UnixMilli(),
		})
		if observed.Code != http.StatusNoContent {
			t.Fatalf("fabric observation status=%d body=%s", observed.Code, observed.Body.String())
		}
	}
	statusReq := httptest.NewRequest(http.MethodGet,
		"/v1/worker/fabric/sessions/"+session.FabricSessionID.String()+"/observations", nil)
	statusReq.Header.Set("X-Worker-Token", initiatorToken)
	statusRec := httptest.NewRecorder()
	handler.ServeHTTP(statusRec, statusReq)
	if statusRec.Code != http.StatusOK {
		t.Fatalf("fabric observation status=%d body=%s", statusRec.Code, statusRec.Body.String())
	}
	var status struct {
		Observed []string `json:"observed_transcript_sha256"`
	}
	if err := json.NewDecoder(statusRec.Body).Decode(&status); err != nil || len(status.Observed) != len(receipt.Rounds) {
		t.Fatalf("fabric observation status body=%+v err=%v", status, err)
	}
	raw, err := json.Marshal(receipt)
	if err != nil {
		t.Fatal(err)
	}
	if got := requestFabricReceipt(t, handler, initiatorToken, raw).Code; got != http.StatusNoContent {
		t.Fatalf("mutual fabric receipt status=%d", got)
	}
	var classification string
	if err := pool.QueryRow(ctx, `SELECT classification FROM fabric_link_measurements WHERE receipt_id=$1`, receipt.ReceiptID).Scan(&classification); err != nil {
		t.Fatal(err)
	}
	if classification != "MUTUAL_WORKER_OBSERVED_NOT_ADMISSIBLE" {
		t.Fatalf("classification=%q, want mutual non-admissible evidence", classification)
	}

	// A missing observation cannot self-promote: it remains self-reported rather
	// than being treated as a partly proven local cluster.
	created = requestFabricJSON(t, handler, initiatorToken, "/v1/worker/fabric/sessions", FabricSessionCreateRequest{
		PeerWorkerID: peer.WorkerID, DeclaredSite: "supplier-lab-rack-a",
	})
	if created.Code != http.StatusCreated {
		t.Fatalf("second fabric session status=%d", created.Code)
	}
	var unobservedSession FabricSessionCreateResponse
	if err := json.NewDecoder(created.Body).Decode(&unobservedSession); err != nil {
		t.Fatal(err)
	}
	second := fabricReceipt()
	second.FabricSessionID = &unobservedSession.FabricSessionID
	second.ExpectedPeerWorkerID = &peer.WorkerID
	second.ReceiptID = uuid.New()
	secondRaw, err := json.Marshal(second)
	if err != nil {
		t.Fatal(err)
	}
	if got := requestFabricReceipt(t, handler, initiatorToken, secondRaw).Code; got != http.StatusNoContent {
		t.Fatalf("partially observed receipt status=%d", got)
	}
	if err := pool.QueryRow(ctx, `SELECT classification FROM fabric_link_measurements WHERE receipt_id=$1`, second.ReceiptID).Scan(&classification); err != nil {
		t.Fatal(err)
	}
	if classification != "SELF_REPORTED_UNQUALIFIED" {
		t.Fatalf("partial observation classification=%q", classification)
	}
}
