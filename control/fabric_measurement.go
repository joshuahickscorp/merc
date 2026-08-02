package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

const (
	fabricMeasurementBodyLimit = 128 << 10
	fabricMeasurementRetention = 30 * 24 * time.Hour
	fabricMaxPayloadBytes      = 4 * 1024 * 1024
	fabricMaxRounds            = 32
)

var errFabricMeasurementConflict = errors.New("fabric measurement receipt conflicts with existing receipt id")

// FabricLinkMeasurementReceipt is an agent's raw direct-link observation. The
// server verifies its shape and recomputes all summary metrics before persisting
// it, but this is still self-reported evidence: the currently owner-shared
// probe secret cannot establish a peer worker identity or a governed site.
type FabricLinkMeasurementReceipt struct {
	SchemaVersion          int                          `json:"schema_version"`
	ReceiptID              uuid.UUID                    `json:"receipt_id"`
	FabricSessionID        *uuid.UUID                   `json:"fabric_session_id,omitempty"`
	ExpectedPeerWorkerID   *uuid.UUID                   `json:"expected_peer_worker_id,omitempty"`
	Kind                   string                       `json:"kind"`
	Status                 string                       `json:"status"`
	MeasuredAtUnixMS       int64                        `json:"measured_at_unix_ms"`
	DeclaredSite           string                       `json:"declared_site"`
	PeerEndpointCommitment string                       `json:"peer_endpoint_commitment"`
	Transport              string                       `json:"transport"`
	PeerAuthentication     string                       `json:"peer_authentication"`
	PayloadIsRandom        bool                         `json:"payload_is_random"`
	Rounds                 []FabricLinkMeasurementRound `json:"rounds"`
	P50RoundTripMicros     int64                        `json:"p50_round_trip_micros"`
	P95RoundTripMicros     int64                        `json:"p95_round_trip_micros"`
	P50PayloadGoodputMbps  float64                      `json:"p50_payload_goodput_mbps"`
	LocalClusterAdmissible bool                         `json:"local_cluster_admissible"`
	NonAdmissionReasons    []string                     `json:"non_admission_reasons"`
}

type FabricLinkMeasurementRound struct {
	Round                     int     `json:"round"`
	PayloadBytesEachDirection int     `json:"payload_bytes_each_direction"`
	RoundTripPayloadBytes     int     `json:"round_trip_payload_bytes"`
	RoundTripMicros           int64   `json:"round_trip_micros"`
	PayloadGoodputMbps        float64 `json:"payload_goodput_mbps"`
	TranscriptSHA256          string  `json:"transcript_sha256"`
}

type FabricSessionCreateRequest struct {
	PeerWorkerID uuid.UUID `json:"peer_worker_id"`
	DeclaredSite string    `json:"declared_site"`
}

type FabricSessionCreateResponse struct {
	FabricSessionID uuid.UUID `json:"fabric_session_id"`
}

type FabricProbeObservation struct {
	SchemaVersion             int       `json:"schema_version"`
	FabricSessionID           uuid.UUID `json:"fabric_session_id"`
	TranscriptSHA256          string    `json:"transcript_sha256"`
	PayloadBytesEachDirection int       `json:"payload_bytes_each_direction"`
	ObservedAtUnixMS          int64     `json:"observed_at_unix_ms"`
}

type fabricMeasurementSummary struct {
	MeasuredAt       time.Time
	PayloadBytes     int
	SampleCount      int
	P50LatencyMicros int64
	P95LatencyMicros int64
	P50GoodputMbps   float64
}

func validateFabricLinkMeasurement(receipt FabricLinkMeasurementReceipt, now time.Time) (fabricMeasurementSummary, error) {
	if receipt.SchemaVersion != 1 || receipt.ReceiptID == uuid.Nil ||
		receipt.Kind != "MERC_FABRIC_TCP_ECHO_RECEIPT" ||
		receipt.Status != "MEASURED_NOT_ADMISSIBLE" {
		return fabricMeasurementSummary{}, errors.New("unsupported fabric receipt identity or status")
	}
	if receipt.LocalClusterAdmissible {
		return fabricMeasurementSummary{}, errors.New("self-reported link measurement must not mark a local cluster admissible")
	}
	if (receipt.FabricSessionID == nil) != (receipt.ExpectedPeerWorkerID == nil) {
		return fabricMeasurementSummary{}, errors.New("fabric receipt must name both a reserved session and expected peer, or neither")
	}
	if receipt.Transport != "MERC_FABRIC_TCP_ECHO_V1" ||
		receipt.PeerAuthentication != "HMAC_SHA256_OWNER_SHARED_PROBE_TOKEN" || !receipt.PayloadIsRandom {
		return fabricMeasurementSummary{}, errors.New("fabric receipt has an unrecognized measurement protocol")
	}
	if strings.TrimSpace(receipt.DeclaredSite) == "" || len(receipt.DeclaredSite) > 128 ||
		!validSHA256(receipt.PeerEndpointCommitment) {
		return fabricMeasurementSummary{}, errors.New("fabric receipt has invalid site or endpoint commitment")
	}
	if len(receipt.Rounds) == 0 || len(receipt.Rounds) > fabricMaxRounds || len(receipt.NonAdmissionReasons) == 0 {
		return fabricMeasurementSummary{}, errors.New("fabric receipt has no bounded raw evidence or omits its admission limits")
	}
	measuredAt := time.UnixMilli(receipt.MeasuredAtUnixMS)
	if measuredAt.Before(now.Add(-48*time.Hour)) || measuredAt.After(now.Add(5*time.Minute)) {
		return fabricMeasurementSummary{}, errors.New("fabric receipt measurement time is outside the accepted evidence window")
	}

	latencies := make([]int64, 0, len(receipt.Rounds))
	goodputs := make([]float64, 0, len(receipt.Rounds))
	payloadBytes := 0
	for index, round := range receipt.Rounds {
		if round.Round != index+1 || round.PayloadBytesEachDirection <= 0 ||
			round.PayloadBytesEachDirection > fabricMaxPayloadBytes ||
			round.RoundTripPayloadBytes != round.PayloadBytesEachDirection*2 ||
			round.RoundTripMicros <= 0 || !finitePositive(round.PayloadGoodputMbps) || !validSHA256(round.TranscriptSHA256) {
			return fabricMeasurementSummary{}, fmt.Errorf("fabric receipt round %d is invalid", index+1)
		}
		observedGoodput := float64(round.RoundTripPayloadBytes) * 8 / float64(round.RoundTripMicros)
		if round.PayloadGoodputMbps != observedGoodput {
			return fabricMeasurementSummary{}, fmt.Errorf("fabric receipt round %d has a non-reproducible goodput summary", index+1)
		}
		if index == 0 {
			payloadBytes = round.PayloadBytesEachDirection
		} else if payloadBytes != round.PayloadBytesEachDirection {
			return fabricMeasurementSummary{}, errors.New("fabric receipt changes payload size between rounds")
		}
		// The client-supplied rate is a display value only. Recompute all stored
		// metrics from bytes and elapsed time so this row can never be selected
		// on an arbitrary agent-provided summary.
		latencies = append(latencies, round.RoundTripMicros)
		goodputs = append(goodputs, observedGoodput)
	}
	fabricSortInt64(latencies)
	fabricSortFloat64(goodputs)
	p50Latency := fabricPercentileInt64(latencies, 50)
	p95Latency := fabricPercentileInt64(latencies, 95)
	p50Goodput := fabricPercentileFloat64(goodputs, 50)
	if receipt.P50RoundTripMicros != p50Latency || receipt.P95RoundTripMicros != p95Latency ||
		receipt.P50PayloadGoodputMbps != p50Goodput {
		return fabricMeasurementSummary{}, errors.New("fabric receipt summary does not reproduce its raw rounds")
	}
	return fabricMeasurementSummary{
		MeasuredAt:       measuredAt,
		PayloadBytes:     payloadBytes,
		SampleCount:      len(receipt.Rounds),
		P50LatencyMicros: p50Latency,
		P95LatencyMicros: p95Latency,
		P50GoodputMbps:   p50Goodput,
	}, nil
}

// CreateFabricProbeSession reserves a short, explicit connection between two
// enrolled workers of the SAME supplier. It does not grant either worker a
// cluster placement or any workload-data authority; it merely gives the peer's
// independently authenticated observer a session it may attest to.
func (s *Store) CreateFabricProbeSession(ctx context.Context, auth WorkerAuth, request FabricSessionCreateRequest) (FabricSessionCreateResponse, error) {
	if request.PeerWorkerID == uuid.Nil || request.PeerWorkerID == auth.WorkerID ||
		strings.TrimSpace(request.DeclaredSite) == "" || len(request.DeclaredSite) > 128 {
		return FabricSessionCreateResponse{}, errors.New("fabric session has invalid peer or declared site")
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return FabricSessionCreateResponse{}, err
	}
	defer tx.Rollback(ctx)
	var peerSupplierID uuid.UUID
	if err := tx.QueryRow(ctx, `SELECT supplier_id FROM workers WHERE id=$1`, request.PeerWorkerID).Scan(&peerSupplierID); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return FabricSessionCreateResponse{}, errNotFound
		}
		return FabricSessionCreateResponse{}, err
	}
	if peerSupplierID != auth.SupplierID {
		return FabricSessionCreateResponse{}, errors.New("fabric peer must belong to the same supplier until a governed multi-owner site authority exists")
	}
	id := uuid.New()
	if _, err := tx.Exec(ctx, `INSERT INTO fabric_probe_sessions
		(id,initiator_worker_id,peer_worker_id,supplier_id,declared_site,expires_at)
		VALUES ($1,$2,$3,$4,$5,now()+interval '10 minutes')`,
		id, auth.WorkerID, request.PeerWorkerID, auth.SupplierID, strings.TrimSpace(request.DeclaredSite)); err != nil {
		return FabricSessionCreateResponse{}, err
	}
	if err := tx.Commit(ctx); err != nil {
		return FabricSessionCreateResponse{}, err
	}
	return FabricSessionCreateResponse{FabricSessionID: id}, nil
}

func validateFabricProbeObservation(observation FabricProbeObservation, now time.Time) (time.Time, error) {
	if observation.SchemaVersion != 1 || observation.FabricSessionID == uuid.Nil ||
		!validSHA256(observation.TranscriptSHA256) || observation.PayloadBytesEachDirection <= 0 ||
		observation.PayloadBytesEachDirection > fabricMaxPayloadBytes {
		return time.Time{}, errors.New("fabric peer observation has invalid identity or bounds")
	}
	observedAt := time.UnixMilli(observation.ObservedAtUnixMS)
	if observedAt.Before(now.Add(-48*time.Hour)) || observedAt.After(now.Add(5*time.Minute)) {
		return time.Time{}, errors.New("fabric peer observation time is outside the accepted evidence window")
	}
	return observedAt, nil
}

func (s *Store) RecordFabricProbeObservation(ctx context.Context, auth WorkerAuth, observation FabricProbeObservation) error {
	observedAt, err := validateFabricProbeObservation(observation, time.Now())
	if err != nil {
		return err
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	var sessionSupplier, peerWorker uuid.UUID
	var expiresAt time.Time
	if err := tx.QueryRow(ctx, `SELECT supplier_id,peer_worker_id,expires_at FROM fabric_probe_sessions WHERE id=$1`, observation.FabricSessionID).
		Scan(&sessionSupplier, &peerWorker, &expiresAt); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return errNotFound
		}
		return err
	}
	if auth.SupplierID != sessionSupplier || auth.WorkerID != peerWorker {
		return errors.New("only the reserved peer worker may attest to this fabric session")
	}
	if !expiresAt.After(time.Now()) {
		return errors.New("fabric session has expired")
	}
	tag, err := tx.Exec(ctx, `INSERT INTO fabric_probe_observations
		(fabric_session_id,transcript_sha256,observer_worker_id,observer_supplier_id,
		 payload_bytes_each_direction,observed_at)
		VALUES ($1,$2,$3,$4,$5,$6)
		ON CONFLICT (fabric_session_id,transcript_sha256) DO NOTHING`,
		observation.FabricSessionID, observation.TranscriptSHA256, auth.WorkerID, auth.SupplierID,
		observation.PayloadBytesEachDirection, observedAt)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		var workerID uuid.UUID
		var payloadBytes int
		if err := tx.QueryRow(ctx, `SELECT observer_worker_id,payload_bytes_each_direction
			FROM fabric_probe_observations WHERE fabric_session_id=$1 AND transcript_sha256=$2`,
			observation.FabricSessionID, observation.TranscriptSHA256).Scan(&workerID, &payloadBytes); err != nil {
			return err
		}
		if workerID != auth.WorkerID || payloadBytes != observation.PayloadBytesEachDirection {
			return errFabricMeasurementConflict
		}
	}
	return tx.Commit(ctx)
}

func (s *Store) FabricProbeObservationStatus(ctx context.Context, auth WorkerAuth, sessionID uuid.UUID) ([]string, error) {
	var initiatorWorker, peerWorker, supplierID uuid.UUID
	if err := s.pool.QueryRow(ctx, `SELECT initiator_worker_id,peer_worker_id,supplier_id
		FROM fabric_probe_sessions WHERE id=$1`, sessionID).Scan(&initiatorWorker, &peerWorker, &supplierID); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, errNotFound
		}
		return nil, err
	}
	if initiatorWorker != auth.WorkerID || supplierID != auth.SupplierID {
		return nil, errors.New("only the fabric-session initiator may read peer observations")
	}
	rows, err := s.pool.Query(ctx, `SELECT transcript_sha256 FROM fabric_probe_observations
		WHERE fabric_session_id=$1 AND observer_worker_id=$2 ORDER BY transcript_sha256`, sessionID, peerWorker)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var transcripts []string
	for rows.Next() {
		var transcript string
		if err := rows.Scan(&transcript); err != nil {
			return nil, err
		}
		transcripts = append(transcripts, transcript)
	}
	return transcripts, rows.Err()
}

func (s *Store) RecordFabricLinkMeasurement(ctx context.Context, auth WorkerAuth, raw []byte) error {
	if len(raw) == 0 || len(raw) > fabricMeasurementBodyLimit {
		return errors.New("fabric receipt body has invalid size")
	}
	var receipt FabricLinkMeasurementReceipt
	if !json.Valid(raw) || decodeStrictJSONObject(raw, &receipt) != nil {
		return errors.New("fabric receipt body is not a strict fabric receipt json object")
	}
	summary, err := validateFabricLinkMeasurement(receipt, time.Now())
	if err != nil {
		return err
	}
	sum := sha256.Sum256(raw)
	receiptSHA256 := hex.EncodeToString(sum[:])
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	if tag, err := tx.Exec(ctx, `UPDATE workers SET last_seen_at=now()
		WHERE id=$1 AND supplier_id=$2`, auth.WorkerID, auth.SupplierID); err != nil {
		return err
	} else if tag.RowsAffected() != 1 {
		return errNotFound
	}
	classification := "SELF_REPORTED_UNQUALIFIED"
	if receipt.FabricSessionID != nil {
		var initiatorWorker, peerWorker, supplierID uuid.UUID
		var declaredSite string
		var expiresAt time.Time
		if err := tx.QueryRow(ctx, `SELECT initiator_worker_id,peer_worker_id,supplier_id,declared_site,expires_at
			FROM fabric_probe_sessions WHERE id=$1`, *receipt.FabricSessionID).
			Scan(&initiatorWorker, &peerWorker, &supplierID, &declaredSite, &expiresAt); err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				return errNotFound
			}
			return err
		}
		if initiatorWorker != auth.WorkerID || supplierID != auth.SupplierID ||
			receipt.ExpectedPeerWorkerID == nil || peerWorker != *receipt.ExpectedPeerWorkerID ||
			declaredSite != strings.TrimSpace(receipt.DeclaredSite) || !expiresAt.After(time.Now()) {
			return errors.New("fabric receipt does not match its reserved peer session")
		}
		allObserved := true
		for _, round := range receipt.Rounds {
			var observerWorker uuid.UUID
			var payloadBytes int
			err := tx.QueryRow(ctx, `SELECT observer_worker_id,payload_bytes_each_direction
				FROM fabric_probe_observations
				WHERE fabric_session_id=$1 AND transcript_sha256=$2`, *receipt.FabricSessionID, round.TranscriptSHA256).
				Scan(&observerWorker, &payloadBytes)
			if errors.Is(err, pgx.ErrNoRows) {
				allObserved = false
				break
			}
			if err != nil {
				return err
			}
			if observerWorker != peerWorker || payloadBytes != round.PayloadBytesEachDirection {
				return errors.New("fabric peer observation does not match the receipt round")
			}
		}
		if allObserved {
			classification = "MUTUAL_WORKER_OBSERVED_NOT_ADMISSIBLE"
		}
	}
	tag, err := tx.Exec(ctx, `
		INSERT INTO fabric_link_measurements
		  (receipt_id, reporting_worker_id, reporting_supplier_id, declared_site,
		   peer_endpoint_commitment, transport, peer_authentication, measured_at,
		   payload_bytes_each_direction, sample_count, p50_round_trip_micros,
		   p95_round_trip_micros, p50_payload_goodput_mbps, fabric_session_id,
		   expected_peer_worker_id, classification, receipt_sha256, raw_receipt)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,$15,$16,$17,$18::jsonb)
		ON CONFLICT (receipt_id) DO NOTHING`,
		receipt.ReceiptID, auth.WorkerID, auth.SupplierID, strings.TrimSpace(receipt.DeclaredSite),
		receipt.PeerEndpointCommitment, receipt.Transport, receipt.PeerAuthentication, summary.MeasuredAt,
		summary.PayloadBytes, summary.SampleCount, summary.P50LatencyMicros, summary.P95LatencyMicros,
		summary.P50GoodputMbps, receipt.FabricSessionID, receipt.ExpectedPeerWorkerID, classification, receiptSHA256, raw)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		var existingSHA string
		var workerID, supplierID uuid.UUID
		if err := tx.QueryRow(ctx, `SELECT receipt_sha256,reporting_worker_id,reporting_supplier_id
			FROM fabric_link_measurements WHERE receipt_id=$1`, receipt.ReceiptID).Scan(&existingSHA, &workerID, &supplierID); err != nil {
			return err
		}
		if existingSHA != receiptSHA256 || workerID != auth.WorkerID || supplierID != auth.SupplierID {
			return errFabricMeasurementConflict
		}
	}
	return tx.Commit(ctx)
}

func (s *Store) DeleteOldFabricLinkMeasurements(ctx context.Context, before time.Time) (int64, error) {
	tag, err := s.pool.Exec(ctx, `DELETE FROM fabric_link_measurements WHERE ingested_at < $1`, before)
	if err != nil {
		return 0, err
	}
	return tag.RowsAffected(), nil
}

func (s *Store) DeleteOldFabricProbeSessions(ctx context.Context, before time.Time) (int64, error) {
	tag, err := s.pool.Exec(ctx, `DELETE FROM fabric_probe_sessions WHERE created_at < $1`, before)
	if err != nil {
		return 0, err
	}
	return tag.RowsAffected(), nil
}

func finitePositive(value float64) bool {
	return value > 0 && !math.IsNaN(value) && !math.IsInf(value, 0)
}

func fabricSortInt64(values []int64) {
	for i := 1; i < len(values); i++ {
		for j := i; j > 0 && values[j] < values[j-1]; j-- {
			values[j], values[j-1] = values[j-1], values[j]
		}
	}
}

func fabricSortFloat64(values []float64) {
	for i := 1; i < len(values); i++ {
		for j := i; j > 0 && values[j] < values[j-1]; j-- {
			values[j], values[j-1] = values[j-1], values[j]
		}
	}
}

func fabricPercentileInt64(sorted []int64, percentile int) int64 {
	return sorted[fabricPercentileIndex(len(sorted), percentile)]
}

func fabricPercentileFloat64(sorted []float64, percentile int) float64 {
	return sorted[fabricPercentileIndex(len(sorted), percentile)]
}

func fabricPercentileIndex(length, percentile int) int {
	return (length*percentile+99)/100 - 1 // nearest-rank: choose a measured round
}
