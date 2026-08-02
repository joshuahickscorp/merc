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
			{Round: 1, PayloadBytesEachDirection: 4096, RoundTripPayloadBytes: 8192, RoundTripMicros: 3200, PayloadGoodputMbps: 20.48},
			{Round: 2, PayloadBytesEachDirection: 4096, RoundTripPayloadBytes: 8192, RoundTripMicros: 1600, PayloadGoodputMbps: 40.96},
			{Round: 3, PayloadBytesEachDirection: 4096, RoundTripPayloadBytes: 8192, RoundTripMicros: 800, PayloadGoodputMbps: 81.92},
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
