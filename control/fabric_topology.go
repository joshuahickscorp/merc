package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

// A link mesh is deliberately a much narrower claim than a local cluster. It
// establishes that every direction between a small, single-supplier group was
// recently observed over certificate-bound mTLS. It cannot establish a shared
// physical site, workload-data authority, collective performance, or economics.
const (
	fabricTopologyMaxWorkers       = 8
	fabricTopologyFreshness        = 15 * time.Minute
	fabricTopologyMinRoundSamples  = 12
	fabricTopologyMaxP95Micros     = int64(5_000)
	fabricTopologyMinP50GoodputMbs = 100.0
)

// FabricTopologyEvaluationRequest is submitted by one member of a proposed
// same-supplier mesh. Worker identities are not inferred from endpoint
// addresses: each listed worker must be enrolled and certificate-bound.
type FabricTopologyEvaluationRequest struct {
	SchemaVersion int         `json:"schema_version"`
	DeclaredSite  string      `json:"declared_site"`
	WorkerIDs     []uuid.UUID `json:"worker_ids"`
}

// FabricTopologyLinkEvidence is the latest acceptable direct-link evidence for
// one direction. It exposes receipt identities and derived metrics only; no
// local endpoint or private key enters the control plane.
type FabricTopologyLinkEvidence struct {
	FromWorkerID       uuid.UUID `json:"from_worker_id"`
	ToWorkerID         uuid.UUID `json:"to_worker_id"`
	ReceiptID          uuid.UUID `json:"receipt_id"`
	MeasuredAt         time.Time `json:"measured_at"`
	SampleCount        int       `json:"sample_count"`
	P95RoundTripMicros int64     `json:"p95_round_trip_micros"`
	P50GoodputMbps     float64   `json:"p50_goodput_mbps"`
}

// FabricTopologyCollectiveEvidence records one completed, two-rank synthetic
// XOR all-reduce between enrolled workers. It does not prove a customer
// workload, a multi-rank tensor collective, or permission to place a gang.
type FabricTopologyCollectiveEvidence struct {
	FromWorkerID       uuid.UUID `json:"from_worker_id"`
	ToWorkerID         uuid.UUID `json:"to_worker_id"`
	ReceiptID          uuid.UUID `json:"receipt_id"`
	MeasuredAt         time.Time `json:"measured_at"`
	SampleCount        int       `json:"sample_count"`
	P95RoundTripMicros int64     `json:"p95_round_trip_micros"`
	P50GoodputMbps     float64   `json:"p50_effective_collective_goodput_mbps"`
}

// FabricTopologyEvaluation is a short-lived evidence receipt. A complete mesh
// is useful input to a future collective benchmark, but the explicit
// LocalClusterAdmissible=false prevents it from becoming scheduler authority.
type FabricTopologyEvaluation struct {
	SchemaVersion               int                                `json:"schema_version"`
	EvaluationID                uuid.UUID                          `json:"evaluation_id"`
	DeclaredSite                string                             `json:"declared_site"`
	WorkerIDs                   []uuid.UUID                        `json:"worker_ids"`
	Status                      string                             `json:"status"`
	EvaluatedAt                 time.Time                          `json:"evaluated_at"`
	EvidenceFreshUntil          time.Time                          `json:"evidence_fresh_until"`
	RequiredDirectedLinks       int                                `json:"required_directed_links"`
	VerifiedDirectedLinks       int                                `json:"verified_directed_links"`
	Links                       []FabricTopologyLinkEvidence       `json:"links"`
	RequiredDirectedCollectives int                                `json:"required_directed_collectives"`
	VerifiedDirectedCollectives int                                `json:"verified_directed_collectives"`
	Collectives                 []FabricTopologyCollectiveEvidence `json:"collectives"`
	LocalClusterAdmissible      bool                               `json:"local_cluster_admissible"`
	NonAdmissionReasons         []string                           `json:"non_admission_reasons"`
	// TopologyPlan is an operator/supplier evidence projection. It is not
	// persisted as scheduler authority and is never used to route a buyer job.
	// Keeping it beside the evaluation makes the refusal visible at the production
	// endpoint that generated the mesh rather than only in unit tests.
	TopologyPlan *TopologyPlan `json:"topology_plan,omitempty"`
}

type fabricTopologyEvidenceRow struct {
	FabricTopologyLinkEvidence
	Classification string
}

type fabricTopologyCollectiveRow struct {
	FabricTopologyCollectiveEvidence
	EvidenceStatus string
}

func validateFabricTopologyRequest(auth WorkerAuth, request *FabricTopologyEvaluationRequest) error {
	if request.SchemaVersion != 1 || strings.TrimSpace(request.DeclaredSite) == "" || len(request.DeclaredSite) > 128 {
		return errors.New("fabric topology request has invalid schema or site")
	}
	if len(request.WorkerIDs) < 2 || len(request.WorkerIDs) > fabricTopologyMaxWorkers {
		return fmt.Errorf("fabric topology requires 2..%d workers", fabricTopologyMaxWorkers)
	}
	seen := make(map[uuid.UUID]struct{}, len(request.WorkerIDs))
	containsRequester := false
	for _, workerID := range request.WorkerIDs {
		if workerID == uuid.Nil {
			return errors.New("fabric topology contains an empty worker identity")
		}
		if _, exists := seen[workerID]; exists {
			return errors.New("fabric topology repeats a worker identity")
		}
		seen[workerID] = struct{}{}
		containsRequester = containsRequester || workerID == auth.WorkerID
	}
	if !containsRequester {
		return errors.New("fabric topology requester must be one of the measured workers")
	}
	request.DeclaredSite = strings.TrimSpace(request.DeclaredSite)
	sort.Slice(request.WorkerIDs, func(i, j int) bool { return request.WorkerIDs[i].String() < request.WorkerIDs[j].String() })
	return nil
}

// EvaluateFabricTopology derives a single-supplier evidence mesh from raw,
// append-only receipts. It always returns a non-admissible result: no caller
// may use connectivity measurements as permission to transfer buyer data or
// select a LOCAL_CLUSTER execution mode.
func (s *Store) EvaluateFabricTopology(ctx context.Context, auth WorkerAuth, request FabricTopologyEvaluationRequest) (FabricTopologyEvaluation, error) {
	if err := validateFabricTopologyRequest(auth, &request); err != nil {
		return FabricTopologyEvaluation{}, err
	}
	now := time.Now().UTC()
	evaluation := FabricTopologyEvaluation{
		SchemaVersion:               1,
		EvaluationID:                uuid.New(),
		DeclaredSite:                request.DeclaredSite,
		WorkerIDs:                   append([]uuid.UUID(nil), request.WorkerIDs...),
		EvaluatedAt:                 now,
		EvidenceFreshUntil:          now.Add(fabricTopologyFreshness),
		RequiredDirectedLinks:       len(request.WorkerIDs) * (len(request.WorkerIDs) - 1),
		RequiredDirectedCollectives: len(request.WorkerIDs) * (len(request.WorkerIDs) - 1),
		Links:                       make([]FabricTopologyLinkEvidence, 0),
		Collectives:                 make([]FabricTopologyCollectiveEvidence, 0),
		LocalClusterAdmissible:      false,
	}

	// First prove that every requested worker is still controlled by the same
	// supplier and has a frozen mTLS identity. A matching declared-site string is
	// not an ownership or residency authority.
	rows, err := s.pool.Query(ctx, `SELECT w.id, w.supplier_id, f.certificate_sha256
		FROM workers w LEFT JOIN worker_fabric_identities f ON f.worker_id=w.id
		WHERE w.id=ANY($1)`, request.WorkerIDs)
	if err != nil {
		return FabricTopologyEvaluation{}, err
	}
	defer rows.Close()
	found := make(map[uuid.UUID]bool, len(request.WorkerIDs))
	for rows.Next() {
		var workerID, supplierID uuid.UUID
		var certificate *string
		if err := rows.Scan(&workerID, &supplierID, &certificate); err != nil {
			return FabricTopologyEvaluation{}, err
		}
		found[workerID] = true
		if supplierID != auth.SupplierID {
			return FabricTopologyEvaluation{}, errors.New("fabric topology may only contain workers of the requesting supplier")
		}
		if certificate == nil || !validSHA256(*certificate) {
			return FabricTopologyEvaluation{}, errors.New("fabric topology worker has no immutable mTLS identity")
		}
	}
	if err := rows.Err(); err != nil {
		return FabricTopologyEvaluation{}, err
	}
	if len(found) != len(request.WorkerIDs) {
		return FabricTopologyEvaluation{}, errors.New("fabric topology names an unknown worker")
	}

	// DISTINCT ON chooses the newest receipt for every requested direction. An
	// old good receipt may not mask a newer degraded path.
	measurementRows, err := s.pool.Query(ctx, `SELECT DISTINCT ON (reporting_worker_id,expected_peer_worker_id)
		reporting_worker_id,expected_peer_worker_id,receipt_id,measured_at,sample_count,
		p95_round_trip_micros,p50_payload_goodput_mbps,classification
		FROM fabric_link_measurements
		WHERE reporting_supplier_id=$1 AND declared_site=$2
		  AND reporting_worker_id=ANY($3) AND expected_peer_worker_id=ANY($3)
		  AND reporting_worker_id<>expected_peer_worker_id
		  AND measured_at >= $4
		  AND classification='MUTUAL_MTLS_WORKER_BOUND_NOT_ADMISSIBLE'
		ORDER BY reporting_worker_id,expected_peer_worker_id,measured_at DESC,ingested_at DESC`,
		auth.SupplierID, request.DeclaredSite, request.WorkerIDs, now.Add(-fabricTopologyFreshness))
	if err != nil {
		return FabricTopologyEvaluation{}, err
	}
	defer measurementRows.Close()
	links := make(map[string]fabricTopologyEvidenceRow, evaluation.RequiredDirectedLinks)
	for measurementRows.Next() {
		var row fabricTopologyEvidenceRow
		if err := measurementRows.Scan(&row.FromWorkerID, &row.ToWorkerID, &row.ReceiptID, &row.MeasuredAt,
			&row.SampleCount, &row.P95RoundTripMicros, &row.P50GoodputMbps, &row.Classification); err != nil {
			return FabricTopologyEvaluation{}, err
		}
		links[fabricTopologyLinkKey(row.FromWorkerID, row.ToWorkerID)] = row
	}
	if err := measurementRows.Err(); err != nil {
		return FabricTopologyEvaluation{}, err
	}

	// The newest synthetic collective per direction wins for the same reason as
	// direct links: an old result may not mask a newly degraded path.
	collectiveRows, err := s.pool.Query(ctx, `SELECT DISTINCT ON (reporting_worker_id,expected_peer_worker_id)
		reporting_worker_id,expected_peer_worker_id,receipt_id,measured_at,sample_count,
		p95_round_trip_micros,p50_effective_collective_goodput_mbps,evidence_status
		FROM fabric_collective_measurements
		WHERE reporting_supplier_id=$1 AND declared_site=$2
		  AND reporting_worker_id=ANY($3) AND expected_peer_worker_id=ANY($3)
		  AND reporting_worker_id<>expected_peer_worker_id
		  AND measured_at >= $4
		  AND evidence_status='MUTUAL_MTLS_WORKER_BOUND_NOT_ADMISSIBLE'
		ORDER BY reporting_worker_id,expected_peer_worker_id,measured_at DESC,ingested_at DESC`,
		auth.SupplierID, request.DeclaredSite, request.WorkerIDs, now.Add(-fabricTopologyFreshness))
	if err != nil {
		return FabricTopologyEvaluation{}, err
	}
	defer collectiveRows.Close()
	collectives := make(map[string]fabricTopologyCollectiveRow, evaluation.RequiredDirectedCollectives)
	for collectiveRows.Next() {
		var row fabricTopologyCollectiveRow
		if err := collectiveRows.Scan(&row.FromWorkerID, &row.ToWorkerID, &row.ReceiptID, &row.MeasuredAt,
			&row.SampleCount, &row.P95RoundTripMicros, &row.P50GoodputMbps, &row.EvidenceStatus); err != nil {
			return FabricTopologyEvaluation{}, err
		}
		collectives[fabricTopologyLinkKey(row.FromWorkerID, row.ToWorkerID)] = row
	}
	if err := collectiveRows.Err(); err != nil {
		return FabricTopologyEvaluation{}, err
	}

	for _, from := range request.WorkerIDs {
		for _, to := range request.WorkerIDs {
			if from == to {
				continue
			}
			row, ok := links[fabricTopologyLinkKey(from, to)]
			if !ok {
				evaluation.NonAdmissionReasons = append(evaluation.NonAdmissionReasons,
					fmt.Sprintf("no fresh mutual-mTLS direct-link receipt from %s to %s", from, to))
				continue
			}
			evaluation.Links = append(evaluation.Links, row.FabricTopologyLinkEvidence)
			if row.SampleCount < fabricTopologyMinRoundSamples || row.P95RoundTripMicros > fabricTopologyMaxP95Micros || row.P50GoodputMbps < fabricTopologyMinP50GoodputMbs {
				evaluation.NonAdmissionReasons = append(evaluation.NonAdmissionReasons, fmt.Sprintf(
					"link %s to %s misses mesh threshold (samples=%d p95_us=%d p50_mbps=%.6f)",
					from, to, row.SampleCount, row.P95RoundTripMicros, row.P50GoodputMbps))
				continue
			}
			evaluation.VerifiedDirectedLinks++
		}
	}
	for _, from := range request.WorkerIDs {
		for _, to := range request.WorkerIDs {
			if from == to {
				continue
			}
			row, ok := collectives[fabricTopologyLinkKey(from, to)]
			if !ok {
				evaluation.NonAdmissionReasons = append(evaluation.NonAdmissionReasons,
					fmt.Sprintf("no fresh peer-observed synthetic collective receipt from %s to %s", from, to))
				continue
			}
			evaluation.Collectives = append(evaluation.Collectives, row.FabricTopologyCollectiveEvidence)
			if row.SampleCount < fabricTopologyMinRoundSamples || row.P95RoundTripMicros > fabricTopologyMaxP95Micros || row.P50GoodputMbps < fabricTopologyMinP50GoodputMbs {
				evaluation.NonAdmissionReasons = append(evaluation.NonAdmissionReasons, fmt.Sprintf(
					"synthetic collective %s to %s misses threshold (samples=%d p95_us=%d p50_mbps=%.6f)",
					from, to, row.SampleCount, row.P95RoundTripMicros, row.P50GoodputMbps))
				continue
			}
			evaluation.VerifiedDirectedCollectives++
		}
	}
	if evaluation.VerifiedDirectedLinks == evaluation.RequiredDirectedLinks &&
		evaluation.VerifiedDirectedCollectives == evaluation.RequiredDirectedCollectives {
		evaluation.Status = "SYNTHETIC_COLLECTIVES_MEASURED_GANG_SCHEDULER_REQUIRED"
	} else if evaluation.VerifiedDirectedLinks == evaluation.RequiredDirectedLinks {
		evaluation.Status = "LINK_MESH_MEASURED_COLLECTIVE_REQUIRED"
	} else {
		evaluation.Status = "LINK_MESH_REFUSED"
	}
	// These unresolved authorities are always retained, even for a complete link
	// mesh. That keeps a future caller from treating a status string as a hidden
	// admission bit.
	evaluation.NonAdmissionReasons = append(evaluation.NonAdmissionReasons,
		"a declared site label is not independent site or residency governance",
		"synthetic two-rank random XOR collectives are not customer-workload or tensor/pipeline/expert/rendering collective benchmarks",
		"no gang scheduler or workload admission path consumes the measured topology",
		"no topology-specific pricing decision binds buyer ceiling, supplier floor, costs, and positive true Merc contribution",
	)
	sort.Slice(evaluation.Links, func(i, j int) bool {
		if evaluation.Links[i].FromWorkerID == evaluation.Links[j].FromWorkerID {
			return evaluation.Links[i].ToWorkerID.String() < evaluation.Links[j].ToWorkerID.String()
		}
		return evaluation.Links[i].FromWorkerID.String() < evaluation.Links[j].FromWorkerID.String()
	})
	sort.Slice(evaluation.Collectives, func(i, j int) bool {
		if evaluation.Collectives[i].FromWorkerID == evaluation.Collectives[j].FromWorkerID {
			return evaluation.Collectives[i].ToWorkerID.String() < evaluation.Collectives[j].ToWorkerID.String()
		}
		return evaluation.Collectives[i].FromWorkerID.String() < evaluation.Collectives[j].FromWorkerID.String()
	})
	linksJSON, err := json.Marshal(evaluation.Links)
	if err != nil {
		return FabricTopologyEvaluation{}, err
	}
	collectivesJSON, err := json.Marshal(evaluation.Collectives)
	if err != nil {
		return FabricTopologyEvaluation{}, err
	}
	reasonsJSON, err := json.Marshal(evaluation.NonAdmissionReasons)
	if err != nil {
		return FabricTopologyEvaluation{}, err
	}
	// Persist the derived evidence snapshot, not merely a mutable query result.
	// Links can subsequently age out of the current mesh query, while this
	// append-only receipt still says exactly which observations were considered.
	if _, err := s.pool.Exec(ctx, `INSERT INTO fabric_topology_evaluations
		(id,requesting_worker_id,requesting_supplier_id,declared_site,worker_ids,status,
		 required_directed_links,verified_directed_links,evidence_fresh_until,links,
		 required_directed_collectives,verified_directed_collectives,collectives,
		 local_cluster_admissible,non_admission_reasons)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10::jsonb,$11,$12,$13::jsonb,false,$14::jsonb)`,
		evaluation.EvaluationID, auth.WorkerID, auth.SupplierID, evaluation.DeclaredSite,
		evaluation.WorkerIDs, evaluation.Status, evaluation.RequiredDirectedLinks,
		evaluation.VerifiedDirectedLinks, evaluation.EvidenceFreshUntil, linksJSON,
		evaluation.RequiredDirectedCollectives, evaluation.VerifiedDirectedCollectives, collectivesJSON, reasonsJSON); err != nil {
		return FabricTopologyEvaluation{}, err
	}
	return evaluation, nil
}

// GetFabricTopologyEvaluation replays one immutable topology evidence receipt
// for the worker that requested it. The read reconstructs the same
// fail-closed planner projection; it never treats the stored status or boolean
// as scheduler authority and never exposes another worker's evaluation.
func (s *Store) GetFabricTopologyEvaluation(ctx context.Context, auth WorkerAuth, id uuid.UUID) (FabricTopologyEvaluation, error) {
	if auth.WorkerID == uuid.Nil || auth.SupplierID == uuid.Nil || id == uuid.Nil {
		return FabricTopologyEvaluation{}, errNotFound
	}
	var evaluation FabricTopologyEvaluation
	var linksRaw, collectivesRaw, reasonsRaw []byte
	err := s.pool.QueryRow(ctx, `SELECT declared_site,worker_ids,status,created_at,evidence_fresh_until,
		required_directed_links,verified_directed_links,links,required_directed_collectives,
		verified_directed_collectives,collectives,local_cluster_admissible,non_admission_reasons
		FROM fabric_topology_evaluations
		WHERE id=$1 AND requesting_worker_id=$2 AND requesting_supplier_id=$3`,
		id, auth.WorkerID, auth.SupplierID).Scan(
		&evaluation.DeclaredSite, &evaluation.WorkerIDs, &evaluation.Status, &evaluation.EvaluatedAt,
		&evaluation.EvidenceFreshUntil, &evaluation.RequiredDirectedLinks,
		&evaluation.VerifiedDirectedLinks, &linksRaw, &evaluation.RequiredDirectedCollectives,
		&evaluation.VerifiedDirectedCollectives, &collectivesRaw, &evaluation.LocalClusterAdmissible,
		&reasonsRaw)
	if errors.Is(err, pgx.ErrNoRows) {
		return FabricTopologyEvaluation{}, errNotFound
	}
	if err != nil {
		return FabricTopologyEvaluation{}, err
	}
	evaluation.SchemaVersion = 1
	evaluation.EvaluationID = id
	if err := json.Unmarshal(linksRaw, &evaluation.Links); err != nil {
		return FabricTopologyEvaluation{}, fmt.Errorf("decode stored fabric topology links: %w", err)
	}
	if err := json.Unmarshal(collectivesRaw, &evaluation.Collectives); err != nil {
		return FabricTopologyEvaluation{}, fmt.Errorf("decode stored fabric topology collectives: %w", err)
	}
	if err := json.Unmarshal(reasonsRaw, &evaluation.NonAdmissionReasons); err != nil {
		return FabricTopologyEvaluation{}, fmt.Errorf("decode stored fabric topology refusals: %w", err)
	}
	plan, err := PlanTopologyFromFabricEvaluation(evaluation, TopologyRequest{
		WorkloadClass: "fabric_topology_evaluation",
		Parallelism: WorkloadParallelism{
			Mode: "tensor_parallel", TensorParallelDegree: len(evaluation.WorkerIDs),
		},
		CommunityCapacityAvailable: true,
		CandidateDeviceCount:       len(evaluation.WorkerIDs),
	}, time.Now().UTC())
	if err != nil {
		return FabricTopologyEvaluation{}, err
	}
	evaluation.TopologyPlan = &plan
	return evaluation, nil
}

func fabricTopologyLinkKey(from, to uuid.UUID) string {
	return from.String() + "\x00" + to.String()
}

func (s *Store) DeleteOldFabricTopologyEvaluations(ctx context.Context, before time.Time) (int64, error) {
	tag, err := s.pool.Exec(ctx, `DELETE FROM fabric_topology_evaluations WHERE created_at < $1`, before)
	if err != nil {
		return 0, err
	}
	return tag.RowsAffected(), nil
}
