package main

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

// ServiceLeaseMarketLiquidityReceipt is an operational observation for the
// warm-service lane, not a claim that Merc has a liquid public market. It keeps
// supply and demand coverage explicit by model, worker-declared hardware, and
// supplier-declared operational region.
type ServiceLeaseMarketLiquidityReceipt struct {
	Version     int                                `json:"version"`
	ObservedAt  time.Time                          `json:"observed_at"`
	WindowStart time.Time                          `json:"window_start"`
	WindowEnd   time.Time                          `json:"window_end"`
	RegionScope string                             `json:"region_scope"`
	Slices      []ServiceLeaseMarketLiquiditySlice `json:"slices"`
}

// ServiceLeaseMarketLiquiditySlice keeps all price observations in their
// original fixed-point nanos-per-replica-hour unit. Ratios are numerator and
// denominator pairs so no float is mistaken for money or a time-weighted fact.
type ServiceLeaseMarketLiquiditySlice struct {
	RuntimeProfileID        string `json:"runtime_profile_id"`
	ModelAlias              string `json:"model_alias,omitempty"`
	Region                  string `json:"region"`
	WorkerDeclaredHWClass   string `json:"worker_declared_hw_class"`
	Admitted                int    `json:"admitted"`
	NoCapacity              int    `json:"no_capacity"`
	InsufficientFunds       int    `json:"insufficient_funds"`
	RequestRejected         int    `json:"request_rejected"`
	CapacityFillNumerator   int    `json:"capacity_fill_numerator"`
	CapacityFillDenominator int    `json:"capacity_fill_denominator"`
	OfferSamples            int    `json:"offer_samples"`
	OccupiedReplicaSamples  int64  `json:"occupied_replica_samples"`
	MaximumReplicaSamples   int64  `json:"maximum_replica_samples"`
	ActiveToInactiveWorkers int    `json:"active_to_inactive_workers"`
	CurrentReadyOffers      int    `json:"current_ready_offers"`
	SupplierFloorMinNanos   int64  `json:"supplier_floor_min_nanos_per_replica_hour"`
	SupplierFloorP50Nanos   int64  `json:"supplier_floor_p50_nanos_per_replica_hour"`
	SupplierFloorP90Nanos   int64  `json:"supplier_floor_p90_nanos_per_replica_hour"`
	ResidencyCostMinNanos   int64  `json:"residency_cost_min_nanos_per_replica_hour"`
	ResidencyCostP50Nanos   int64  `json:"residency_cost_p50_nanos_per_replica_hour"`
	ResidencyCostP90Nanos   int64  `json:"residency_cost_p90_nanos_per_replica_hour"`
}

const (
	serviceLeaseAdmissionAdmitted        = "ADMITTED"
	serviceLeaseAdmissionNoCapacity      = "NO_CAPACITY"
	serviceLeaseAdmissionInsufficient    = "INSUFFICIENT_FUNDS"
	serviceLeaseAdmissionRequestRejected = "REQUEST_REJECTED"
	serviceLiquidityUnmatchedHardware    = "UNMATCHED"
)

const serviceLeaseMarketClearingVersion = 1

// serviceLeaseMarketCandidate is the authority-side offer book row considered
// for one buyer order. It is read under the same transaction lock as the
// reservation, so the candidate count and selected rank describe the market
// that actually cleared rather than a best-effort snapshot taken beforehand.
type serviceLeaseMarketCandidate struct {
	WorkerID                     uuid.UUID
	SupplierID                   uuid.UUID
	SupplierNanosPerReplicaHour  int64
	ResidencyNanosPerReplicaHour int64
	AvailableWarmReplicas        int
}

// serviceLeaseMarketClearingDetail is embedded in the activation receipt. It
// is a matching receipt, not a second price authority: the only amount frozen
// here is the digest and fixed-point result of the canonical PricingDecision.
// Candidate rates are retained so an operator can distinguish a genuine
// order-book match from a hard-coded single-worker route.
type serviceLeaseMarketClearingDetail struct {
	Version                    int       `json:"version"`
	CandidateCount             int       `json:"candidate_count"`
	SelectedRank               int       `json:"selected_rank"`
	SelectedWorkerID           uuid.UUID `json:"selected_worker_id"`
	SelectedSupplierID         uuid.UUID `json:"selected_supplier_id"`
	SelectedSupplierRateNanos  int64     `json:"selected_supplier_nanos_per_replica_hour"`
	SelectedResidencyRateNanos int64     `json:"selected_residency_nanos_per_replica_hour"`
	BuyerCeilingNanos          int64     `json:"buyer_ceiling_nanos"`
	AcceptedCeilingNanos       int64     `json:"accepted_ceiling_nanos"`
	PricingDecisionSHA256      string    `json:"pricing_decision_sha256"`
	PositiveContributionNanos  int64     `json:"positive_contribution_nanos"`
	OrderBookPolicy            string    `json:"order_book_policy"`
	SelectionReason            string    `json:"selection_reason"`
}

// RecordServiceLeaseAdmissionEvent accepts only a lease-bound admission as a
// capacity success. It contains no buyer ceiling or request body and therefore
// cannot become a second pricing or customer-data store.
func (s *Store) RecordServiceLeaseAdmissionEvent(
	ctx context.Context, buyerID uuid.UUID, request ServiceLeaseRequest, decision string, leaseID uuid.UUID,
) error {
	if buyerID == uuid.Nil || strings.TrimSpace(request.RuntimeProfileID) == "" ||
		!serviceLeaseRegionPattern.MatchString(request.Region) {
		return errors.New("service lease admission telemetry lacks buyer, runtime profile, or region")
	}
	switch decision {
	case serviceLeaseAdmissionAdmitted:
		if leaseID == uuid.Nil {
			return errors.New("admitted service lease telemetry lacks lease identity")
		}
		var boundBuyer uuid.UUID
		var boundProfile, boundRegion, hwClass string
		if err := s.pool.QueryRow(ctx, `SELECT l.buyer_id,l.runtime_profile_id,l.region,
			COALESCE(NULLIF(btrim(w.hw_class),''),'UNDECLARED')
			FROM service_leases l JOIN workers w ON w.id=l.worker_id WHERE l.id=$1`, leaseID).Scan(&boundBuyer, &boundProfile, &boundRegion, &hwClass); err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				return errors.New("admitted service lease telemetry names no lease")
			}
			return err
		}
		if boundBuyer != buyerID || boundProfile != request.RuntimeProfileID || boundRegion != request.Region {
			return errors.New("admitted service lease telemetry does not match frozen lease authority")
		}
		_, err := s.pool.Exec(ctx, `INSERT INTO service_lease_admission_events
			(buyer_id,runtime_profile_id,region,worker_declared_hw_class,decision,lease_id)
			VALUES ($1,$2,$3,$4,$5,$6)`, buyerID, request.RuntimeProfileID, request.Region, hwClass, decision, leaseID)
		return err
	case serviceLeaseAdmissionNoCapacity, serviceLeaseAdmissionInsufficient, serviceLeaseAdmissionRequestRejected:
		if leaseID != uuid.Nil {
			return errors.New("non-admitted service lease telemetry must not name a lease")
		}
	default:
		return fmt.Errorf("unknown service lease admission decision %q", decision)
	}
	_, err := s.pool.Exec(ctx, `INSERT INTO service_lease_admission_events
		(buyer_id,runtime_profile_id,region,worker_declared_hw_class,decision,lease_id)
		VALUES ($1,$2,$3,$4,$5,NULLIF($6::uuid,'00000000-0000-0000-0000-000000000000'::uuid))`,
		buyerID, request.RuntimeProfileID, request.Region, serviceLiquidityUnmatchedHardware, decision, leaseID)
	return err
}

// ServiceLeaseMarketLiquidity reports only retained service-offer samples and
// real buyer route outcomes. A period beyond retention is rejected rather than
// silently reported as a complete market history.
func (s *Store) ServiceLeaseMarketLiquidity(ctx context.Context, window time.Duration) (ServiceLeaseMarketLiquidityReceipt, error) {
	if window <= 0 || window > maxRealtimeLiquidityWindow {
		return ServiceLeaseMarketLiquidityReceipt{}, fmt.Errorf("service liquidity window must be in (0,%s]", maxRealtimeLiquidityWindow)
	}
	now := time.Now().UTC()
	since := now.Add(-window)
	out := ServiceLeaseMarketLiquidityReceipt{
		Version: 1, ObservedAt: now, WindowStart: since, WindowEnd: now,
		RegionScope: "SUPPLIER_DECLARED_OPERATIONAL_REGION_ONLY",
	}
	rows, err := s.pool.Query(ctx, `SELECT runtime_profile_id,region,worker_declared_hw_class FROM (
			SELECT runtime_profile_id,region,worker_declared_hw_class
			  FROM service_lease_offer_samples WHERE observed_at >= $1
			UNION
			SELECT runtime_profile_id,region,worker_declared_hw_class
			  FROM service_lease_admission_events WHERE observed_at >= $1
		) keys ORDER BY runtime_profile_id,region,worker_declared_hw_class`, since)
	if err != nil {
		return out, err
	}
	defer rows.Close()
	for rows.Next() {
		var slice ServiceLeaseMarketLiquiditySlice
		if err := rows.Scan(&slice.RuntimeProfileID, &slice.Region, &slice.WorkerDeclaredHWClass); err != nil {
			return out, err
		}
		if profile, ok := vllmProfileByID(slice.RuntimeProfileID); ok {
			slice.ModelAlias = profile.ModelAlias
		}
		if err := s.populateServiceLeaseLiquiditySlice(ctx, since, &slice); err != nil {
			return out, err
		}
		out.Slices = append(out.Slices, slice)
	}
	if err := rows.Err(); err != nil {
		return out, err
	}
	return out, nil
}

func (s *Store) populateServiceLeaseLiquiditySlice(ctx context.Context, since time.Time, slice *ServiceLeaseMarketLiquiditySlice) error {
	if err := s.pool.QueryRow(ctx, `SELECT
		count(*) FILTER (WHERE decision='ADMITTED'),
		count(*) FILTER (WHERE decision='NO_CAPACITY'),
		count(*) FILTER (WHERE decision='INSUFFICIENT_FUNDS'),
		count(*) FILTER (WHERE decision='REQUEST_REJECTED')
		FROM service_lease_admission_events
		WHERE runtime_profile_id=$1 AND region=$2 AND worker_declared_hw_class=$3 AND observed_at >= $4`,
		slice.RuntimeProfileID, slice.Region, slice.WorkerDeclaredHWClass, since).Scan(&slice.Admitted, &slice.NoCapacity,
		&slice.InsufficientFunds, &slice.RequestRejected); err != nil {
		return err
	}
	slice.CapacityFillNumerator = slice.Admitted
	slice.CapacityFillDenominator = slice.Admitted + slice.NoCapacity
	if slice.WorkerDeclaredHWClass == serviceLiquidityUnmatchedHardware {
		return nil
	}
	if err := s.pool.QueryRow(ctx, `WITH per_worker AS (
			SELECT worker_id,bool_or(status='READY') AS observed_ready,
			       bool_or(status IN ('DRAINING','FAILED')) AS observed_inactive
			  FROM service_lease_offer_samples
			 WHERE runtime_profile_id=$1 AND region=$2 AND worker_declared_hw_class=$3 AND observed_at >= $4
			 GROUP BY worker_id
		)
		SELECT count(*)::int,
		       COALESCE(sum(maximum_warm_replicas-available_warm_replicas),0)::bigint,
		       COALESCE(sum(maximum_warm_replicas),0)::bigint,
		       COALESCE((SELECT count(*) FROM per_worker WHERE observed_ready AND observed_inactive),0)::int
		FROM service_lease_offer_samples
		WHERE runtime_profile_id=$1 AND region=$2 AND worker_declared_hw_class=$3 AND observed_at >= $4`,
		slice.RuntimeProfileID, slice.Region, slice.WorkerDeclaredHWClass, since).Scan(
		&slice.OfferSamples, &slice.OccupiedReplicaSamples, &slice.MaximumReplicaSamples,
		&slice.ActiveToInactiveWorkers); err != nil {
		return err
	}
	rows, err := s.pool.Query(ctx, `SELECT supplier_nanos_per_replica_hour,residency_nanos_per_replica_hour
		FROM service_lease_worker_offers o JOIN workers w ON w.id=o.worker_id
		WHERE o.runtime_profile_id=$1 AND o.region=$2
		  AND COALESCE(NULLIF(btrim(w.hw_class),''),'UNDECLARED')=$3
		  AND o.status='READY' AND o.last_seen_at > now()-interval '45 seconds'
		  AND o.available_warm_replicas > 0
		ORDER BY o.supplier_nanos_per_replica_hour,o.residency_nanos_per_replica_hour,o.worker_id`,
		slice.RuntimeProfileID, slice.Region, slice.WorkerDeclaredHWClass)
	if err != nil {
		return err
	}
	defer rows.Close()
	var supplier, residency []int64
	for rows.Next() {
		var floor, cost int64
		if err := rows.Scan(&floor, &cost); err != nil {
			return err
		}
		supplier = append(supplier, floor)
		residency = append(residency, cost)
	}
	if err := rows.Err(); err != nil {
		return err
	}
	slice.CurrentReadyOffers = len(supplier)
	sort.Slice(residency, func(i, j int) bool { return residency[i] < residency[j] })
	if len(supplier) > 0 {
		slice.SupplierFloorMinNanos = supplier[0]
		slice.SupplierFloorP50Nanos = nearestRankInt64(supplier, 50, 100)
		slice.SupplierFloorP90Nanos = nearestRankInt64(supplier, 90, 100)
		slice.ResidencyCostMinNanos = residency[0]
		slice.ResidencyCostP50Nanos = nearestRankInt64(residency, 50, 100)
		slice.ResidencyCostP90Nanos = nearestRankInt64(residency, 90, 100)
	}
	return nil
}

func nearestRankInt64(sortedValues []int64, numerator, denominator int) int64 {
	if len(sortedValues) == 0 || numerator <= 0 || denominator <= 0 {
		return 0
	}
	index := (len(sortedValues)*numerator + denominator - 1) / denominator
	if index < 1 {
		index = 1
	}
	if index > len(sortedValues) {
		index = len(sortedValues)
	}
	return sortedValues[index-1]
}

func (s *Store) DeleteOldServiceLeaseLiquidityTelemetry(ctx context.Context, before time.Time) (offerSamples, admissionEvents int64, err error) {
	if tag, err := s.pool.Exec(ctx, `DELETE FROM service_lease_offer_samples WHERE observed_at < $1`, before); err != nil {
		return 0, 0, err
	} else {
		offerSamples = tag.RowsAffected()
	}
	if tag, err := s.pool.Exec(ctx, `DELETE FROM service_lease_admission_events WHERE observed_at < $1`, before); err != nil {
		return offerSamples, 0, err
	} else {
		admissionEvents = tag.RowsAffected()
	}
	return offerSamples, admissionEvents, nil
}

func (s *Server) handleAdminServiceLeaseMarketLiquidity(w http.ResponseWriter, r *http.Request) {
	window := defaultRealtimeLiquidityWindow
	if raw := strings.TrimSpace(r.URL.Query().Get("window_seconds")); raw != "" {
		seconds, err := strconv.ParseInt(raw, 10, 64)
		if err != nil || seconds <= 0 {
			writeErr(w, http.StatusBadRequest, "window_seconds must be a positive integer")
			return
		}
		window = time.Duration(seconds) * time.Second
	}
	receipt, err := s.store.ServiceLeaseMarketLiquidity(r.Context(), window)
	if err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, receipt)
}
