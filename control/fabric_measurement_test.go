package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
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

func fabricCollectiveReceipt() FabricCollectiveMeasurementReceipt {
	return FabricCollectiveMeasurementReceipt{
		SchemaVersion:                     1,
		ReceiptID:                         uuid.New(),
		Kind:                              "MERC_FABRIC_SYNTHETIC_XOR_ALL_REDUCE_RECEIPT",
		Status:                            "MEASURED_NOT_ADMISSIBLE",
		MeasuredAtUnixMS:                  time.Now().UnixMilli(),
		DeclaredSite:                      "supplier-lab-rack-a",
		PeerEndpointCommitment:            strings.Repeat("a", 64),
		Transport:                         "MERC_FABRIC_MTLS_SYNTHETIC_COLLECTIVE_V1",
		PeerAuthentication:                "MUTUAL_TLS_WORKER_CERTIFICATE_BOUND",
		PayloadIsRandom:                   true,
		Collective:                        "XOR_ALL_REDUCE_TWO_RANKS_V1",
		P50RoundTripMicros:                1600,
		P95RoundTripMicros:                3200,
		P50EffectiveCollectiveGoodputMbps: 40.96,
		LocalClusterAdmissible:            false,
		NonAdmissionReasons: []string{
			"synthetic random bytes are not a customer workload data plane",
			"no gang scheduler, workload admission, result verification, or settlement path consumes this probe",
		},
		Rounds: []FabricCollectiveMeasurementRound{
			{Round: 1, Ranks: 2, PayloadBytesPerRank: 4096, TransportBytes: 12288, RoundTripMicros: 3200, EffectiveCollectiveGoodputMbps: 20.48, LocalPayloadSHA256: strings.Repeat("b", 64), PeerPayloadSHA256: strings.Repeat("c", 64), ReducedPayloadSHA256: strings.Repeat("d", 64), TranscriptSHA256: strings.Repeat("e", 64)},
			{Round: 2, Ranks: 2, PayloadBytesPerRank: 4096, TransportBytes: 12288, RoundTripMicros: 1600, EffectiveCollectiveGoodputMbps: 40.96, LocalPayloadSHA256: strings.Repeat("f", 64), PeerPayloadSHA256: strings.Repeat("0", 64), ReducedPayloadSHA256: strings.Repeat("1", 64), TranscriptSHA256: strings.Repeat("2", 64)},
			{Round: 3, Ranks: 2, PayloadBytesPerRank: 4096, TransportBytes: 12288, RoundTripMicros: 800, EffectiveCollectiveGoodputMbps: 81.92, LocalPayloadSHA256: strings.Repeat("3", 64), PeerPayloadSHA256: strings.Repeat("4", 64), ReducedPayloadSHA256: strings.Repeat("5", 64), TranscriptSHA256: strings.Repeat("6", 64)},
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

func requestFabricCollectiveReceipt(t *testing.T, handler http.Handler, token string, raw []byte) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/v1/worker/fabric/collective-receipts", bytes.NewReader(raw))
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

func registerFabricIdentity(t *testing.T, ctx context.Context, store *Store, worker WorkerAuth) string {
	t.Helper()
	sum := sha256.Sum256([]byte("fabric-test-worker-certificate:" + worker.WorkerID.String()))
	fingerprint := hex.EncodeToString(sum[:])
	if err := store.RegisterFabricWorkerIdentity(ctx, worker, FabricWorkerIdentity{CertificateSHA256: fingerprint}); err != nil {
		t.Fatal(err)
	}
	return fingerprint
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

func TestFabricWorkerCertificateIdentityIsAuthenticatedUniqueAndImmutable(t *testing.T) {
	ctx, store, _ := openPayoutTestStore(t)
	worker, token := newFabricMeasurementWorker(t, ctx, store)
	handler := NewServer(store, nil, nil, nil).Routes()
	fingerprint := strings.Repeat("e", 64)
	if got := requestFabricJSON(t, handler, token, "/v1/worker/fabric/identity", FabricWorkerIdentity{CertificateSHA256: fingerprint}).Code; got != http.StatusNoContent {
		t.Fatalf("fabric identity status=%d", got)
	}
	// Exact retries are safe. A changed certificate identity is not: certificate
	// rotation needs a separately governed operation rather than a token call.
	if got := requestFabricJSON(t, handler, token, "/v1/worker/fabric/identity", FabricWorkerIdentity{CertificateSHA256: fingerprint}).Code; got != http.StatusNoContent {
		t.Fatalf("fabric identity retry status=%d", got)
	}
	if got := requestFabricJSON(t, handler, token, "/v1/worker/fabric/identity", FabricWorkerIdentity{CertificateSHA256: strings.Repeat("f", 64)}).Code; got != http.StatusConflict {
		t.Fatalf("fabric identity mutation status=%d, want 409", got)
	}
	peer, peerToken := newFabricPeerWorker(t, ctx, store, worker.SupplierID)
	if got := requestFabricJSON(t, handler, peerToken, "/v1/worker/fabric/identity", FabricWorkerIdentity{CertificateSHA256: fingerprint}).Code; got != http.StatusConflict {
		t.Fatalf("cross-worker fabric certificate reuse status=%d, want 409", got)
	}
	_ = peer
}

func TestFabricSessionRequiresEveryRoundFromTheReservedPeerAndStaysNonAdmissible(t *testing.T) {
	ctx, store, pool := openPayoutTestStore(t)
	initiator, initiatorToken := newFabricMeasurementWorker(t, ctx, store)
	peer, peerToken := newFabricPeerWorker(t, ctx, store, initiator.SupplierID)
	initiatorCertificate := registerFabricIdentity(t, ctx, store, initiator)
	peerCertificate := registerFabricIdentity(t, ctx, store, peer)
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
	if session.PeerCertificateSHA256 != peerCertificate {
		t.Fatalf("fabric session returned peer certificate %q, want %q", session.PeerCertificateSHA256, peerCertificate)
	}

	receipt := fabricReceipt()
	receipt.Transport = "MERC_FABRIC_MTLS_ECHO_V1"
	receipt.PeerAuthentication = "MUTUAL_TLS_WORKER_CERTIFICATE_BOUND"
	receipt.LocalCertificateSHA256 = initiatorCertificate
	receipt.PeerCertificateSHA256 = peerCertificate
	receipt.FabricSessionID = &session.FabricSessionID
	receipt.ExpectedPeerWorkerID = &peer.WorkerID
	wrongObserved := requestFabricJSON(t, handler, peerToken, "/v1/worker/fabric/observations", FabricProbeObservation{
		SchemaVersion: 1, FabricSessionID: session.FabricSessionID,
		TranscriptSHA256: receipt.Rounds[0].TranscriptSHA256, PayloadBytesEachDirection: receipt.Rounds[0].PayloadBytesEachDirection,
		ObservedAtUnixMS: time.Now().UnixMilli(), ObservedPeerCertificateSHA256: strings.Repeat("0", 64),
	})
	if wrongObserved.Code != http.StatusBadRequest {
		t.Fatalf("wrong mTLS client certificate observation status=%d body=%s", wrongObserved.Code, wrongObserved.Body.String())
	}
	for _, round := range receipt.Rounds {
		observed := requestFabricJSON(t, handler, peerToken, "/v1/worker/fabric/observations", FabricProbeObservation{
			SchemaVersion: 1, FabricSessionID: session.FabricSessionID,
			TranscriptSHA256: round.TranscriptSHA256, PayloadBytesEachDirection: round.PayloadBytesEachDirection,
			ObservedAtUnixMS: time.Now().UnixMilli(), ObservedPeerCertificateSHA256: initiatorCertificate,
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
	var classification, storedLocalCertificate, storedPeerCertificate string
	if err := pool.QueryRow(ctx, `SELECT classification,local_certificate_sha256,peer_certificate_sha256
		FROM fabric_link_measurements WHERE receipt_id=$1`, receipt.ReceiptID).
		Scan(&classification, &storedLocalCertificate, &storedPeerCertificate); err != nil {
		t.Fatal(err)
	}
	if classification != "MUTUAL_MTLS_WORKER_BOUND_NOT_ADMISSIBLE" ||
		storedLocalCertificate != initiatorCertificate || storedPeerCertificate != peerCertificate {
		t.Fatalf("mTLS evidence was not bound and retained: class=%q local=%q peer=%q", classification, storedLocalCertificate, storedPeerCertificate)
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
	second.Transport = "MERC_FABRIC_MTLS_ECHO_V1"
	second.PeerAuthentication = "MUTUAL_TLS_WORKER_CERTIFICATE_BOUND"
	second.LocalCertificateSHA256 = initiatorCertificate
	second.PeerCertificateSHA256 = peerCertificate
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

func TestFabricCollectiveReceiptRequiresPeerObservationAndStaysEvidenceOnly(t *testing.T) {
	ctx, store, pool := openPayoutTestStore(t)
	initiator, initiatorToken := newFabricMeasurementWorker(t, ctx, store)
	peer, peerToken := newFabricPeerWorker(t, ctx, store, initiator.SupplierID)
	initiatorCertificate := registerFabricIdentity(t, ctx, store, initiator)
	peerCertificate := registerFabricIdentity(t, ctx, store, peer)
	handler := NewServer(store, nil, nil, nil).Routes()

	created := requestFabricJSON(t, handler, initiatorToken, "/v1/worker/fabric/sessions", FabricSessionCreateRequest{
		PeerWorkerID: peer.WorkerID, DeclaredSite: "supplier-lab-rack-a",
	})
	if created.Code != http.StatusCreated {
		t.Fatalf("fabric collective session status=%d body=%s", created.Code, created.Body.String())
	}
	var session FabricSessionCreateResponse
	if err := json.NewDecoder(created.Body).Decode(&session); err != nil {
		t.Fatal(err)
	}
	receipt := fabricCollectiveReceipt()
	receipt.FabricSessionID = &session.FabricSessionID
	receipt.ExpectedPeerWorkerID = &peer.WorkerID
	receipt.LocalCertificateSHA256 = initiatorCertificate
	receipt.PeerCertificateSHA256 = peerCertificate
	raw, err := json.Marshal(receipt)
	if err != nil {
		t.Fatal(err)
	}
	if got := requestFabricCollectiveReceipt(t, handler, initiatorToken, raw).Code; got != http.StatusBadRequest {
		t.Fatalf("unobserved fabric collective receipt status=%d, want 400", got)
	}
	partialHashObservation := requestFabricJSON(t, handler, peerToken, "/v1/worker/fabric/observations", FabricProbeObservation{
		SchemaVersion: 1, FabricSessionID: session.FabricSessionID,
		TranscriptSHA256: receipt.Rounds[0].TranscriptSHA256, PayloadBytesEachDirection: receipt.Rounds[0].PayloadBytesPerRank,
		ObservedAtUnixMS: time.Now().UnixMilli(), ObservedPeerCertificateSHA256: initiatorCertificate,
		CollectiveLocalPayloadSHA256: receipt.Rounds[0].LocalPayloadSHA256,
	})
	if partialHashObservation.Code != http.StatusBadRequest {
		t.Fatalf("partial fabric collective hash observation status=%d, want 400", partialHashObservation.Code)
	}
	for _, round := range receipt.Rounds {
		observed := requestFabricJSON(t, handler, peerToken, "/v1/worker/fabric/observations", FabricProbeObservation{
			SchemaVersion: 1, FabricSessionID: session.FabricSessionID,
			TranscriptSHA256: round.TranscriptSHA256, PayloadBytesEachDirection: round.PayloadBytesPerRank,
			ObservedAtUnixMS: time.Now().UnixMilli(), ObservedPeerCertificateSHA256: initiatorCertificate,
			CollectiveLocalPayloadSHA256: round.LocalPayloadSHA256, CollectivePeerPayloadSHA256: round.PeerPayloadSHA256,
			CollectiveReducedPayloadSHA256: round.ReducedPayloadSHA256,
		})
		if observed.Code != http.StatusNoContent {
			t.Fatalf("fabric collective observation status=%d body=%s", observed.Code, observed.Body.String())
		}
	}
	if got := requestFabricCollectiveReceipt(t, handler, initiatorToken, raw).Code; got != http.StatusNoContent {
		t.Fatalf("fabric collective receipt status=%d", got)
	}
	if got := requestFabricCollectiveReceipt(t, handler, initiatorToken, raw).Code; got != http.StatusNoContent {
		t.Fatalf("fabric collective receipt retry status=%d", got)
	}

	var evidenceStatus string
	var p50Latency, p95Latency int64
	var p50Goodput float64
	if err := pool.QueryRow(ctx, `SELECT evidence_status,p50_round_trip_micros,p95_round_trip_micros,
		p50_effective_collective_goodput_mbps FROM fabric_collective_measurements WHERE receipt_id=$1`, receipt.ReceiptID).
		Scan(&evidenceStatus, &p50Latency, &p95Latency, &p50Goodput); err != nil {
		t.Fatal(err)
	}
	if evidenceStatus != "MUTUAL_MTLS_WORKER_BOUND_NOT_ADMISSIBLE" ||
		p50Latency != 1600 || p95Latency != 3200 || math.Abs(p50Goodput-40.96) > 0.000001 {
		t.Fatalf("collective evidence was not retained as derived non-admissible data: status=%s p50=%d p95=%d goodput=%f",
			evidenceStatus, p50Latency, p95Latency, p50Goodput)
	}
	if _, err := pool.Exec(ctx, `UPDATE fabric_collective_measurements SET p50_round_trip_micros=1 WHERE receipt_id=$1`, receipt.ReceiptID); err == nil {
		t.Fatal("database allowed immutable fabric collective evidence to be rewritten")
	}
	// The peer's recorded payload digests bind the raw receipt to the actual
	// collective exchange; a client cannot retain the transcript and rewrite a
	// contribution or reduction hash after the fact.
	tampered := receipt
	tampered.ReceiptID = uuid.New()
	tampered.Rounds = append([]FabricCollectiveMeasurementRound(nil), receipt.Rounds...)
	tampered.Rounds[0].ReducedPayloadSHA256 = strings.Repeat("7", 64)
	tamperedRaw, err := json.Marshal(tampered)
	if err != nil {
		t.Fatal(err)
	}
	if got := requestFabricCollectiveReceipt(t, handler, initiatorToken, tamperedRaw).Code; got != http.StatusBadRequest {
		t.Fatalf("tampered collective payload digest status=%d, want 400", got)
	}
	// An otherwise valid mutation must not adopt the original immutable receipt id.
	receipt.NonAdmissionReasons[0] = "mutated but still non-empty"
	mutated, err := json.Marshal(receipt)
	if err != nil {
		t.Fatal(err)
	}
	if got := requestFabricCollectiveReceipt(t, handler, initiatorToken, mutated).Code; got != http.StatusConflict {
		t.Fatalf("mutated collective receipt retry status=%d, want 409", got)
	}
}

func recordQualifiedMutualFabricLink(t *testing.T, handler http.Handler, from WorkerAuth, fromToken, fromCertificate string, to WorkerAuth, toToken, toCertificate, site string) {
	t.Helper()
	created := requestFabricJSON(t, handler, fromToken, "/v1/worker/fabric/sessions", FabricSessionCreateRequest{
		PeerWorkerID: to.WorkerID, DeclaredSite: site,
	})
	if created.Code != http.StatusCreated {
		t.Fatalf("create qualified fabric session status=%d body=%s", created.Code, created.Body.String())
	}
	var session FabricSessionCreateResponse
	if err := json.NewDecoder(created.Body).Decode(&session); err != nil {
		t.Fatal(err)
	}
	if session.PeerCertificateSHA256 != toCertificate {
		t.Fatalf("reserved peer certificate=%s want=%s", session.PeerCertificateSHA256, toCertificate)
	}

	const payloadBytes = 256 * 1024
	const roundTripMicros = int64(1_000)
	goodput := float64(payloadBytes*2*8) / float64(roundTripMicros)
	receipt := fabricReceipt()
	receipt.DeclaredSite = site
	receipt.Transport = "MERC_FABRIC_MTLS_ECHO_V1"
	receipt.PeerAuthentication = "MUTUAL_TLS_WORKER_CERTIFICATE_BOUND"
	receipt.LocalCertificateSHA256 = fromCertificate
	receipt.PeerCertificateSHA256 = toCertificate
	receipt.FabricSessionID = &session.FabricSessionID
	receipt.ExpectedPeerWorkerID = &to.WorkerID
	receipt.Rounds = make([]FabricLinkMeasurementRound, 0, fabricTopologyMinRoundSamples)
	for round := 1; round <= fabricTopologyMinRoundSamples; round++ {
		sum := sha256.Sum256([]byte(fmt.Sprintf("fabric-topology-test:%s:%s:%d", from.WorkerID, to.WorkerID, round)))
		receipt.Rounds = append(receipt.Rounds, FabricLinkMeasurementRound{
			Round: round, PayloadBytesEachDirection: payloadBytes, RoundTripPayloadBytes: payloadBytes * 2,
			RoundTripMicros: roundTripMicros, PayloadGoodputMbps: goodput, TranscriptSHA256: hex.EncodeToString(sum[:]),
		})
	}
	receipt.P50RoundTripMicros = roundTripMicros
	receipt.P95RoundTripMicros = roundTripMicros
	receipt.P50PayloadGoodputMbps = goodput
	for _, round := range receipt.Rounds {
		observed := requestFabricJSON(t, handler, toToken, "/v1/worker/fabric/observations", FabricProbeObservation{
			SchemaVersion: 1, FabricSessionID: session.FabricSessionID,
			TranscriptSHA256: round.TranscriptSHA256, PayloadBytesEachDirection: payloadBytes,
			ObservedAtUnixMS: time.Now().UnixMilli(), ObservedPeerCertificateSHA256: fromCertificate,
		})
		if observed.Code != http.StatusNoContent {
			t.Fatalf("qualified fabric observation status=%d body=%s", observed.Code, observed.Body.String())
		}
	}
	raw, err := json.Marshal(receipt)
	if err != nil {
		t.Fatal(err)
	}
	if got := requestFabricReceipt(t, handler, fromToken, raw).Code; got != http.StatusNoContent {
		t.Fatalf("qualified fabric receipt status=%d", got)
	}
}

func TestFabricTopologyRequiresFreshBidirectionalMTLSMeshAndRefusesClusterPromotion(t *testing.T) {
	ctx, store, pool := openPayoutTestStore(t)
	first, firstToken := newFabricMeasurementWorker(t, ctx, store)
	second, secondToken := newFabricPeerWorker(t, ctx, store, first.SupplierID)
	firstCertificate := registerFabricIdentity(t, ctx, store, first)
	secondCertificate := registerFabricIdentity(t, ctx, store, second)
	handler := NewServer(store, nil, nil, nil).Routes()
	const site = "supplier-lab-rack-a"
	recordQualifiedMutualFabricLink(t, handler, first, firstToken, firstCertificate, second, secondToken, secondCertificate, site)
	recordQualifiedMutualFabricLink(t, handler, second, secondToken, secondCertificate, first, firstToken, firstCertificate, site)

	response := requestFabricJSON(t, handler, firstToken, "/v1/worker/fabric/topologies/evaluate", FabricTopologyEvaluationRequest{
		SchemaVersion: 1, DeclaredSite: site, WorkerIDs: []uuid.UUID{second.WorkerID, first.WorkerID},
	})
	if response.Code != http.StatusOK {
		t.Fatalf("fabric topology evaluation status=%d body=%s", response.Code, response.Body.String())
	}
	var evaluation FabricTopologyEvaluation
	if err := json.NewDecoder(response.Body).Decode(&evaluation); err != nil {
		t.Fatal(err)
	}
	if evaluation.Status != "LINK_MESH_MEASURED_COLLECTIVE_REQUIRED" ||
		evaluation.RequiredDirectedLinks != 2 || evaluation.VerifiedDirectedLinks != 2 || len(evaluation.Links) != 2 {
		t.Fatalf("fabric topology did not derive the complete mesh: %+v", evaluation)
	}
	if evaluation.LocalClusterAdmissible {
		t.Fatal("a link mesh promoted itself to LOCAL_CLUSTER without collective/economic authority")
	}
	if len(evaluation.NonAdmissionReasons) < 3 || !strings.Contains(strings.Join(evaluation.NonAdmissionReasons, " "), "collective benchmark") {
		t.Fatalf("fabric topology did not preserve its collective refusal: %+v", evaluation.NonAdmissionReasons)
	}
	var storedStatus string
	var storedLinks int
	if err := pool.QueryRow(ctx, `SELECT status,jsonb_array_length(links) FROM fabric_topology_evaluations WHERE id=$1`, evaluation.EvaluationID).
		Scan(&storedStatus, &storedLinks); err != nil {
		t.Fatal(err)
	}
	if storedStatus != evaluation.Status || storedLinks != 2 {
		t.Fatalf("fabric topology receipt was not persisted exactly: status=%q links=%d", storedStatus, storedLinks)
	}
	if _, err := pool.Exec(ctx, `UPDATE fabric_topology_evaluations SET status='LINK_MESH_REFUSED' WHERE id=$1`, evaluation.EvaluationID); err == nil {
		t.Fatal("database allowed an immutable fabric topology receipt to be rewritten")
	}

	// The endpoint is a supplier-scoped discovery aid, not an inventory oracle.
	other, _ := newFabricMeasurementWorker(t, ctx, store)
	forbidden := requestFabricJSON(t, handler, firstToken, "/v1/worker/fabric/topologies/evaluate", FabricTopologyEvaluationRequest{
		SchemaVersion: 1, DeclaredSite: site, WorkerIDs: []uuid.UUID{first.WorkerID, other.WorkerID},
	})
	if forbidden.Code != http.StatusBadRequest {
		t.Fatalf("cross-supplier topology status=%d, want 400", forbidden.Code)
	}
}
