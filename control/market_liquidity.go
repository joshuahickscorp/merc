package main

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

// RealtimeMarketLiquidityReceipt is a bounded operational observation, not a
// market-clearing claim. It names the time window, input coverage, and the
// still-missing region authority so operators cannot mistake a current snapshot
// for a global marketplace measurement.
type RealtimeMarketLiquidityReceipt struct {
	Version     int                            `json:"version"`
	ObservedAt  time.Time                      `json:"observed_at"`
	WindowStart time.Time                      `json:"window_start"`
	WindowEnd   time.Time                      `json:"window_end"`
	RegionScope string                         `json:"region_scope"`
	Slices      []RealtimeMarketLiquiditySlice `json:"slices"`
}

// RealtimeMarketLiquiditySlice groups only facts the current realtime lane can
// establish: a governed runtime profile and the hardware class frozen in an
// offer/contract placement plan. Region remains explicitly unspecified until a
// governed region source is introduced; IP inference is not a residency term.
type RealtimeMarketLiquiditySlice struct {
	RuntimeProfileID string `json:"runtime_profile_id"`
	ModelAlias       string `json:"model_alias,omitempty"`
	HWClass          string `json:"hw_class"`

	// Demand decisions include only decisions whose capacity outcome is known.
	// Funding refusals and authorization faults are shown separately and never
	// dilute the capacity fill rate.
	Admitted              int      `json:"admitted"`
	NoCapacity            int      `json:"no_capacity"`
	InsufficientFunds     int      `json:"insufficient_funds"`
	AuthorizationFailures int      `json:"authorization_failures"`
	CapacityFillRate      *float64 `json:"capacity_fill_rate,omitempty"`

	// Offer samples arrive from the worker's existing registration/heartbeat
	// path. Average utilization is the mean occupied-sequence fraction across
	// samples; it is not a time-weighted fleet utilization claim.
	OfferSamples            int     `json:"offer_samples"`
	MeanSampleUtilization   float64 `json:"mean_sample_utilization"`
	ActiveToInactiveWorkers int     `json:"active_to_inactive_workers"`
	CurrentActiveOffers     int     `json:"current_active_offers"`
	SupplierInputRateMin    float64 `json:"supplier_input_rate_min"`
	SupplierInputRateP50    float64 `json:"supplier_input_rate_p50"`
	SupplierInputRateP90    float64 `json:"supplier_input_rate_p90"`
	SupplierOutputRateMin   float64 `json:"supplier_output_rate_min"`
	SupplierOutputRateP50   float64 `json:"supplier_output_rate_p50"`
	SupplierOutputRateP90   float64 `json:"supplier_output_rate_p90"`
}

const (
	defaultRealtimeLiquidityWindow = 24 * time.Hour
	maxRealtimeLiquidityWindow     = 30 * 24 * time.Hour
	realtimeLiquidityRetention     = 30 * 24 * time.Hour
)

const (
	realtimeAdmissionAdmitted       = "ADMITTED"
	realtimeAdmissionNoCapacity     = "NO_CAPACITY"
	realtimeAdmissionInsufficient   = "INSUFFICIENT_FUNDS"
	realtimeAdmissionAuthorization  = "AUTHORIZATION_FAILED"
	liquidityUnmatchedHardwareClass = "UNMATCHED"
)

// RecordRealtimeAdmissionEvent persists only the capacity/funding outcome of a
// prepared request. It deliberately has no prompt, canonical request, token
// count, or upstream identifier. An admitted event verifies its contract
// binding before it can affect the fill-rate denominator.
//
// Prefer InsertRealtimeAdmissionEventTrusted from the request path when the
// caller already holds the authorize-returned placement: the verify SELECT
// here can deadlock with FinalizeRealtimeSuccess's FOR UPDATE under load.
func (s *Store) RecordRealtimeAdmissionEvent(
	ctx context.Context, buyerID uuid.UUID, runtimeProfileID, hwClass, decision string, contractID uuid.UUID,
) error {
	if buyerID == uuid.Nil || strings.TrimSpace(runtimeProfileID) == "" {
		return errors.New("realtime admission event lacks buyer or runtime profile")
	}
	switch decision {
	case realtimeAdmissionAdmitted:
		if contractID == uuid.Nil || strings.TrimSpace(hwClass) == "" {
			return errors.New("admitted realtime event lacks contract or hardware placement")
		}
		var contractHW string
		err := s.pool.QueryRow(ctx, `
			SELECT COALESCE(placement_plan->>'hw_class','')
			  FROM execution_contracts
			 WHERE id=$1 AND buyer_id=$2 AND runtime_profile_id=$3`,
			contractID, buyerID, runtimeProfileID).Scan(&contractHW)
		if errors.Is(err, pgx.ErrNoRows) {
			return errors.New("admitted realtime event contract does not bind buyer and profile")
		}
		if err != nil {
			return err
		}
		if contractHW == "" || contractHW != hwClass {
			return errors.New("admitted realtime event hardware does not match frozen contract placement")
		}
	default:
		if contractID != uuid.Nil || strings.TrimSpace(hwClass) != "" {
			return errors.New("non-admitted realtime event must not claim a contract or hardware placement")
		}
		if decision != realtimeAdmissionNoCapacity && decision != realtimeAdmissionInsufficient &&
			decision != realtimeAdmissionAuthorization {
			return fmt.Errorf("unknown realtime admission decision %q", decision)
		}
	}
	return s.insertRealtimeAdmissionEvent(ctx, buyerID, runtimeProfileID, hwClass, decision, contractID)
}

// InsertRealtimeAdmissionEventTrusted records an admission outcome without the
// contract-binding re-verify. Use only when the caller already holds the
// authorize-returned contract and placement (the realtime handler). Avoids the
// verify SELECT that can deadlock with finalize under concurrent load.
// Shape checks still apply so a buggy caller cannot insert a nonsense row.
func (s *Store) InsertRealtimeAdmissionEventTrusted(
	ctx context.Context, buyerID uuid.UUID, runtimeProfileID, hwClass, decision string, contractID uuid.UUID,
) error {
	if buyerID == uuid.Nil || strings.TrimSpace(runtimeProfileID) == "" {
		return errors.New("realtime admission event lacks buyer or runtime profile")
	}
	switch decision {
	case realtimeAdmissionAdmitted:
		if contractID == uuid.Nil || strings.TrimSpace(hwClass) == "" {
			return errors.New("admitted realtime event lacks contract or hardware placement")
		}
	default:
		if contractID != uuid.Nil || strings.TrimSpace(hwClass) != "" {
			return errors.New("non-admitted realtime event must not claim a contract or hardware placement")
		}
		if decision != realtimeAdmissionNoCapacity && decision != realtimeAdmissionInsufficient &&
			decision != realtimeAdmissionAuthorization {
			return fmt.Errorf("unknown realtime admission decision %q", decision)
		}
	}
	return s.insertRealtimeAdmissionEvent(ctx, buyerID, runtimeProfileID, hwClass, decision, contractID)
}

func (s *Store) insertRealtimeAdmissionEvent(
	ctx context.Context, buyerID uuid.UUID, runtimeProfileID, hwClass, decision string, contractID uuid.UUID,
) error {
	_, err := s.pool.Exec(ctx, `
		INSERT INTO realtime_admission_events
		  (buyer_id,runtime_profile_id,hw_class,decision,contract_id)
		VALUES ($1,$2,$3,$4,NULLIF($5::uuid,'00000000-0000-0000-0000-000000000000'::uuid))`,
		buyerID, runtimeProfileID, hwClass, decision, contractID)
	return err
}

// RealtimeMarketLiquidity reports the evidence retained by the real offer and
// admission paths. It intentionally refuses windows beyond retention: silently
// treating a partial historical sample as a month-long market would be worse
// than returning no report.
func (s *Store) RealtimeMarketLiquidity(ctx context.Context, window time.Duration) (RealtimeMarketLiquidityReceipt, error) {
	if window <= 0 || window > maxRealtimeLiquidityWindow {
		return RealtimeMarketLiquidityReceipt{}, fmt.Errorf("liquidity window must be in (0,%s]", maxRealtimeLiquidityWindow)
	}
	now := time.Now().UTC()
	since := now.Add(-window)
	out := RealtimeMarketLiquidityReceipt{
		Version: 1, ObservedAt: now, WindowStart: since, WindowEnd: now,
		RegionScope: "UNSPECIFIED_NO_GOVERNED_REGION_AUTHORITY",
	}
	rows, err := s.pool.Query(ctx, `
		SELECT runtime_profile_id,hw_class FROM (
			SELECT runtime_profile_id,hw_class
			  FROM realtime_offer_samples WHERE observed_at >= $1
			UNION
			SELECT runtime_profile_id,
			       CASE WHEN hw_class='' THEN $2 ELSE hw_class END
			  FROM realtime_admission_events WHERE observed_at >= $1
		) keys
		ORDER BY runtime_profile_id,hw_class`, since, liquidityUnmatchedHardwareClass)
	if err != nil {
		return out, err
	}
	defer rows.Close()
	for rows.Next() {
		var slice RealtimeMarketLiquiditySlice
		if err := rows.Scan(&slice.RuntimeProfileID, &slice.HWClass); err != nil {
			return out, err
		}
		if profile, ok := vllmProfileByID(slice.RuntimeProfileID); ok {
			slice.ModelAlias = profile.ModelAlias
		}
		if err := s.populateRealtimeLiquiditySlice(ctx, since, &slice); err != nil {
			return out, err
		}
		out.Slices = append(out.Slices, slice)
	}
	if err := rows.Err(); err != nil {
		return out, err
	}
	return out, nil
}

func (s *Store) populateRealtimeLiquiditySlice(ctx context.Context, since time.Time, slice *RealtimeMarketLiquiditySlice) error {
	eventHW := slice.HWClass
	if eventHW == liquidityUnmatchedHardwareClass {
		eventHW = ""
	}
	if err := s.pool.QueryRow(ctx, `
		SELECT count(*) FILTER (WHERE decision='ADMITTED'),
		       count(*) FILTER (WHERE decision='NO_CAPACITY'),
		       count(*) FILTER (WHERE decision='INSUFFICIENT_FUNDS'),
		       count(*) FILTER (WHERE decision='AUTHORIZATION_FAILED')
		  FROM realtime_admission_events
		 WHERE runtime_profile_id=$1 AND hw_class=$2 AND observed_at >= $3`,
		slice.RuntimeProfileID, eventHW, since).Scan(
		&slice.Admitted, &slice.NoCapacity, &slice.InsufficientFunds, &slice.AuthorizationFailures); err != nil {
		return err
	}
	if total := slice.Admitted + slice.NoCapacity; total > 0 {
		fill := float64(slice.Admitted) / float64(total)
		slice.CapacityFillRate = &fill
	}
	if slice.HWClass == liquidityUnmatchedHardwareClass {
		return nil
	}
	if err := s.pool.QueryRow(ctx, `
		WITH per_worker AS (
			SELECT worker_id,
			       bool_or(status='ACTIVE') AS observed_active,
			       bool_or(status IN ('DRAINING','FAILED','QUARANTINED')) AS observed_inactive
			  FROM realtime_offer_samples
			 WHERE runtime_profile_id=$1 AND hw_class=$2 AND observed_at >= $3
			 GROUP BY worker_id
		)
		SELECT count(*)::int,
		       COALESCE(avg((max_active_sequences-available_sequences)::float8 /
		                    max_active_sequences),0)::float8,
		       COALESCE((SELECT count(*) FROM per_worker
		                  WHERE observed_active AND observed_inactive),0)::int
		  FROM realtime_offer_samples
		 WHERE runtime_profile_id=$1 AND hw_class=$2 AND observed_at >= $3`,
		slice.RuntimeProfileID, slice.HWClass, since).Scan(
		&slice.OfferSamples, &slice.MeanSampleUtilization, &slice.ActiveToInactiveWorkers); err != nil {
		return err
	}
	// Price depth is a current offer-book observation. It uses the live offers,
	// not historical samples, because a withdrawn rate is not depth available to
	// a buyer now. Rates are supplier asks, not a buyer catalogue price.
	return s.pool.QueryRow(ctx, `
		SELECT count(*)::int,
		       COALESCE(min(o.supplier_input_usd_per_million_tokens),0)::float8,
		       COALESCE(percentile_cont(0.5) WITHIN GROUP (ORDER BY o.supplier_input_usd_per_million_tokens),0)::float8,
		       COALESCE(percentile_cont(0.9) WITHIN GROUP (ORDER BY o.supplier_input_usd_per_million_tokens),0)::float8,
		       COALESCE(min(o.supplier_output_usd_per_million_tokens),0)::float8,
		       COALESCE(percentile_cont(0.5) WITHIN GROUP (ORDER BY o.supplier_output_usd_per_million_tokens),0)::float8,
		       COALESCE(percentile_cont(0.9) WITHIN GROUP (ORDER BY o.supplier_output_usd_per_million_tokens),0)::float8
		  FROM realtime_worker_offers o
		  JOIN workers w ON w.id=o.worker_id
		 WHERE o.runtime_profile_id=$1
		   AND COALESCE(NULLIF(o.placement_plan->>'hw_class',''),w.hw_class)=$2
		   AND o.status='ACTIVE' AND o.last_seen_at > now()-interval '45 seconds'
		   AND o.available_sequences > 0`, slice.RuntimeProfileID, slice.HWClass).Scan(
		&slice.CurrentActiveOffers, &slice.SupplierInputRateMin, &slice.SupplierInputRateP50,
		&slice.SupplierInputRateP90, &slice.SupplierOutputRateMin, &slice.SupplierOutputRateP50,
		&slice.SupplierOutputRateP90)
}

func (s *Store) DeleteOldRealtimeLiquidityTelemetry(ctx context.Context, before time.Time) (offerSamples, admissionEvents int64, err error) {
	if tag, err := s.pool.Exec(ctx, `DELETE FROM realtime_offer_samples WHERE observed_at < $1`, before); err != nil {
		return 0, 0, err
	} else {
		offerSamples = tag.RowsAffected()
	}
	if tag, err := s.pool.Exec(ctx, `DELETE FROM realtime_admission_events WHERE observed_at < $1`, before); err != nil {
		return offerSamples, 0, err
	} else {
		admissionEvents = tag.RowsAffected()
	}
	return offerSamples, admissionEvents, nil
}

func (s *Server) handleAdminRealtimeMarketLiquidity(w http.ResponseWriter, r *http.Request) {
	window := defaultRealtimeLiquidityWindow
	if raw := strings.TrimSpace(r.URL.Query().Get("window_seconds")); raw != "" {
		seconds, err := strconv.ParseInt(raw, 10, 64)
		if err != nil || seconds <= 0 {
			writeErr(w, http.StatusBadRequest, "window_seconds must be a positive integer")
			return
		}
		window = time.Duration(seconds) * time.Second
	}
	receipt, err := s.store.RealtimeMarketLiquidity(r.Context(), window)
	if err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, receipt)
}
