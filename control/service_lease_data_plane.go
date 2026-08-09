package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

// errServiceLeaseDataPlaneUnavailable is deliberately distinct from
// errNotFound. An owner can see that its reservation still exists, while a
// stale worker/offer is a temporary capability failure rather than an object
// ownership failure.
var errServiceLeaseDataPlaneUnavailable = errors.New("reserved service data plane is unavailable")

// serviceLeaseDataPlaneTarget is an internal, buyer-authorized projection.
// The upstream credential never leaves this process and this type is never
// serialized into a buyer response.
type serviceLeaseDataPlaneTarget struct {
	LeaseID                 uuid.UUID
	WorkerID                uuid.UUID
	SupplierID              uuid.UUID
	RuntimeProfileID        string
	RuntimeProfileSHA256    string
	ModelAlias              string
	UpstreamBaseURL         string
	UpstreamToken           string
	MaximumP95LatencyMillis int64
	State                   string
	ActiveReplicas          int
	MinimumReplicas         int
	ExpiresAt               time.Time
	LastWorkerHeartbeatAt   time.Time
	LastOfferSeenAt         time.Time
}

// ServiceLeaseDataPlaneTarget resolves only the worker that this buyer's
// current lease reserved. It does not select from the public realtime offer
// book and it does not authorize or settle a second per-request contract: the
// lease's cumulative replica-time heartbeat remains the sole money authority.
func (s *Store) ServiceLeaseDataPlaneTarget(ctx context.Context, buyerID, leaseID uuid.UUID) (serviceLeaseDataPlaneTarget, error) {
	if buyerID == uuid.Nil || leaseID == uuid.Nil {
		return serviceLeaseDataPlaneTarget{}, errNotFound
	}
	var target serviceLeaseDataPlaneTarget
	err := s.pool.QueryRow(ctx, `
		SELECT id,worker_id,supplier_id,runtime_profile_id,runtime_profile_sha256,
		       state,active_replicas,minimum_replicas,maximum_p95_latency_milliseconds,
		       expires_at,last_worker_heartbeat_at
		  FROM service_leases
		 WHERE id=$1 AND buyer_id=$2`, leaseID, buyerID).Scan(
		&target.LeaseID, &target.WorkerID, &target.SupplierID, &target.RuntimeProfileID,
		&target.RuntimeProfileSHA256, &target.State, &target.ActiveReplicas,
		&target.MinimumReplicas, &target.MaximumP95LatencyMillis, &target.ExpiresAt,
		&target.LastWorkerHeartbeatAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return serviceLeaseDataPlaneTarget{}, errNotFound
	}
	if err != nil {
		return serviceLeaseDataPlaneTarget{}, err
	}
	now := time.Now().UTC()
	if (target.State != "ACTIVE" && target.State != "UPGRADING") ||
		target.ExpiresAt.Before(now) || target.ActiveReplicas < target.MinimumReplicas ||
		now.Sub(target.LastWorkerHeartbeatAt) > serviceLeaseHeartbeatTimeout {
		return serviceLeaseDataPlaneTarget{}, errServiceLeaseDataPlaneUnavailable
	}
	profile, ok := vllmProfileByID(target.RuntimeProfileID)
	if !ok || profile.ProfileSHA256 != target.RuntimeProfileSHA256 {
		return serviceLeaseDataPlaneTarget{}, errServiceLeaseDataPlaneUnavailable
	}
	target.ModelAlias = profile.ModelAlias
	var sealed, status, profileSHA string
	err = s.pool.QueryRow(ctx, `
		SELECT upstream_base_url,upstream_token_sealed,status,runtime_profile_sha256,last_seen_at
		  FROM realtime_worker_offers
		 WHERE worker_id=$1 AND supplier_id=$2 AND runtime_profile_id=$3
		   AND status='ACTIVE' AND last_seen_at > now()-interval '45 seconds'`,
		target.WorkerID, target.SupplierID, target.RuntimeProfileID).Scan(
		&target.UpstreamBaseURL, &sealed, &status, &profileSHA, &target.LastOfferSeenAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return serviceLeaseDataPlaneTarget{}, errServiceLeaseDataPlaneUnavailable
	}
	if err != nil {
		return serviceLeaseDataPlaneTarget{}, err
	}
	if status != "ACTIVE" || profileSHA != target.RuntimeProfileSHA256 ||
		!validStoredRealtimeUpstreamURL(target.UpstreamBaseURL) {
		return serviceLeaseDataPlaneTarget{}, errServiceLeaseDataPlaneUnavailable
	}
	target.UpstreamToken = openToken(sealed)
	if target.UpstreamToken == "" {
		return serviceLeaseDataPlaneTarget{}, errServiceLeaseDataPlaneUnavailable
	}
	return target, nil
}

// recordServiceLeaseDataPlaneDiagnostic persists only application payload
// lengths. It intentionally accepts the resolved worker/supplier pair even if a
// failover commits after target resolution: that old pair is the truthful
// interval that served this request. The observation never feeds pricing or
// settlement and carries an explicit non-authority status in storage.
func (s *Store) recordServiceLeaseDataPlaneDiagnostic(
	ctx context.Context,
	target serviceLeaseDataPlaneTarget,
	buyerID uuid.UUID,
	requestApplicationBytes, responseApplicationBytes int64,
) error {
	if target.LeaseID == uuid.Nil || target.WorkerID == uuid.Nil || target.SupplierID == uuid.Nil ||
		buyerID == uuid.Nil || requestApplicationBytes < 0 || responseApplicationBytes < 0 {
		return errors.New("invalid service lease data-plane diagnostic")
	}
	ct, err := s.pool.Exec(ctx, `INSERT INTO service_lease_data_plane_diagnostics
		(lease_id,buyer_id,worker_id,supplier_id,request_application_bytes,
		 response_application_bytes,authority_status)
		SELECT id,buyer_id,$3,$4,$5,$6,'APPLICATION_BYTES_DIAGNOSTIC_NOT_PROVIDER_BILLING'
		  FROM service_leases WHERE id=$1 AND buyer_id=$2`,
		target.LeaseID, buyerID, target.WorkerID, target.SupplierID,
		requestApplicationBytes, responseApplicationBytes)
	if err != nil {
		return err
	}
	if ct.RowsAffected() != 1 {
		return errNotFound
	}
	return nil
}

func validStoredRealtimeUpstreamURL(raw string) bool {
	u, err := url.Parse(strings.TrimSpace(raw))
	return err == nil && u.Host != "" && u.User == nil && u.RawQuery == "" &&
		u.Fragment == "" && (u.Scheme == "http" || u.Scheme == "https") && u.Path == "/v1"
}

func serviceLeaseRequestTimeout(p95Millis int64) time.Duration {
	// The worker-attested p95 is an SLO, not a buyer-controlled deadline. Give a
	// bounded multiple of it room for tail latency while retaining the global
	// realtime ceiling and avoiding duration overflow on legacy rows.
	const multiplier int64 = 5
	maxMillis := int64(defaultRealtimeTimeout / time.Millisecond)
	if p95Millis <= 0 || p95Millis > maxMillis/multiplier {
		return defaultRealtimeTimeout
	}
	d := time.Duration(p95Millis*multiplier) * time.Millisecond
	if d < time.Second {
		return time.Second
	}
	return d
}

func (s *Server) handleServiceLeaseChatCompletions(w http.ResponseWriter, r *http.Request) {
	auth := r.Context().Value(ctxBuyer).(*AuthResult)
	leaseID, ok := pathUUID(w, r)
	if !ok {
		return
	}
	target, err := s.store.ServiceLeaseDataPlaneTarget(r.Context(), auth.BuyerID, leaseID)
	if errors.Is(err, errNotFound) {
		writeOpenAIError(w, http.StatusNotFound, "service lease not found", "invalid_request_error", "not_found")
		return
	}
	if errors.Is(err, errServiceLeaseDataPlaneUnavailable) {
		writeOpenAIError(w, http.StatusServiceUnavailable, "reserved service data plane is unavailable", "server_error", "service_unavailable")
		return
	}
	if err != nil {
		writeOpenAIError(w, http.StatusInternalServerError, "reserved service data plane lookup failed", "server_error", "lookup_failed")
		return
	}
	raw, err := io.ReadAll(io.LimitReader(r.Body, maxRealtimeRequestBytes+1))
	if err != nil || len(raw) > maxRealtimeRequestBytes {
		writeOpenAIError(w, http.StatusRequestEntityTooLarge, "request body exceeds the realtime limit", "invalid_request_error", "request_too_large")
		return
	}
	prepared, err := prepareRealtimeRequest(raw, "")
	if err != nil {
		writeOpenAIError(w, http.StatusBadRequest, err.Error(), "invalid_request_error", "invalid_request")
		return
	}
	if prepared.Stream {
		writeOpenAIError(w, http.StatusBadRequest, "streaming is not supported on a reserved service lease data plane", "invalid_request_error", "stream_not_supported")
		return
	}
	if prepared.Profile.RuntimeProfileID != target.RuntimeProfileID ||
		prepared.Profile.ProfileSHA256 != target.RuntimeProfileSHA256 ||
		prepared.Profile.ModelAlias != target.ModelAlias {
		writeOpenAIError(w, http.StatusBadRequest, "model is not bound to this service lease", "invalid_request_error", "model_not_bound")
		return
	}

	requestContext, cancel := context.WithTimeout(r.Context(), serviceLeaseRequestTimeout(target.MaximumP95LatencyMillis))
	defer cancel()
	upstreamRequest, err := http.NewRequestWithContext(requestContext, http.MethodPost,
		realtimeUpstreamURL(target.UpstreamBaseURL), bytes.NewReader(prepared.Body))
	if err != nil {
		writeOpenAIError(w, http.StatusBadGateway, "could not construct the reserved service request", "server_error", "upstream_request_invalid")
		return
	}
	upstreamRequest.Header.Set("Authorization", "Bearer "+target.UpstreamToken)
	upstreamRequest.Header.Set("Content-Type", "application/json")
	upstreamRequest.Header.Set("X-Request-ID", w.Header().Get("X-Request-ID"))
	client := s.realtimeHTTPClient
	if client == nil {
		client = newRealtimeHTTPClient()
	}
	response, err := client.Do(upstreamRequest)
	if err != nil {
		status := http.StatusBadGateway
		code := "upstream_unavailable"
		if errors.Is(requestContext.Err(), context.DeadlineExceeded) {
			status = http.StatusGatewayTimeout
			code = "upstream_timeout"
		}
		writeOpenAIError(w, status, "reserved service worker is unavailable", "server_error", code)
		return
	}
	defer response.Body.Close()
	if response.StatusCode < 200 || response.StatusCode > 299 {
		_, _ = io.Copy(io.Discard, io.LimitReader(response.Body, 64<<10))
		writeOpenAIError(w, http.StatusBadGateway, "reserved service worker rejected the request", "server_error", "upstream_rejected")
		return
	}
	body, err := io.ReadAll(io.LimitReader(response.Body, maxRealtimeResponseBytes+1))
	if err != nil || len(body) > maxRealtimeResponseBytes || !json.Valid(body) {
		writeOpenAIError(w, http.StatusBadGateway, "reserved service worker returned an invalid JSON response", "server_error", "upstream_response_invalid")
		return
	}
	// Diagnostics are not economic authority and must not turn a successfully
	// served reserved request into a buyer-visible failure. Persist when the
	// database is available; the receipt remains EGRESS UNKNOWN either way.
	if err := s.store.recordServiceLeaseDataPlaneDiagnostic(r.Context(), target, auth.BuyerID,
		int64(len(prepared.Body)), int64(len(body))); err != nil {
		log.Printf("service lease %s application-byte diagnostic was not persisted: %v", leaseID, err)
	}
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("X-Merc-Service-Lease-ID", leaseID.String())
	w.Header().Set("X-Merc-Service-Lease-Metering", "replica_nanoseconds")
	w.Header().Set("X-Merc-Receipt", "/v1/service-leases/"+leaseID.String()+"/receipt")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(body)
}
