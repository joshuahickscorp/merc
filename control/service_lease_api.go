package main

import (
	"errors"
	"io"
	"net/http"
)

func (s *Server) handleServiceLeaseOffer(w http.ResponseWriter, r *http.Request) {
	auth := r.Context().Value(ctxWorker).(*WorkerAuth)
	raw, err := io.ReadAll(io.LimitReader(r.Body, 64<<10+1))
	if err != nil || len(raw) > 64<<10 {
		writeErr(w, http.StatusBadRequest, "invalid service lease offer json")
		return
	}
	var offer ServiceLeaseOfferRegistration
	if err := decodeStrictJSONObject(raw, &offer); err != nil {
		writeErr(w, http.StatusBadRequest, "invalid service lease offer json: "+err.Error())
		return
	}
	if _, err := validateServiceLeaseOffer(offer); err != nil {
		writeErr(w, http.StatusBadRequest, "service lease offer rejected: "+err.Error())
		return
	}
	if err := s.store.UpsertServiceLeaseOffer(r.Context(), *auth, offer); err != nil {
		if errors.Is(err, errServiceLeaseOfferBelowReservations) {
			writeErr(w, http.StatusConflict, "service lease offer conflicts with active reserved capacity")
			return
		}
		writeErr(w, http.StatusInternalServerError, "service lease offer registration failed")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"status": offer.Status, "runtime_profile_id": offer.RuntimeProfileID, "region": offer.Region})
}

func (s *Server) handleCreateServiceLease(w http.ResponseWriter, r *http.Request) {
	auth := r.Context().Value(ctxBuyer).(*AuthResult)
	var request ServiceLeaseRequest
	raw, err := io.ReadAll(io.LimitReader(r.Body, 32<<10+1))
	if err != nil || len(raw) > 32<<10 || decodeStrictJSONObject(raw, &request) != nil {
		writeErr(w, http.StatusBadRequest, "invalid service lease request")
		return
	}
	lease, err := s.store.CreateServiceLease(r.Context(), auth.BuyerID, request)
	if errors.Is(err, errRealtimeNoSupply) {
		writeErr(w, http.StatusServiceUnavailable, "no compatible warm service capacity is currently available")
		return
	}
	if errors.Is(err, errRealtimeInsufficientFunds) {
		writeErr(w, http.StatusPaymentRequired, "insufficient prepaid balance for maximum service reservation")
		return
	}
	if err != nil {
		writeErr(w, http.StatusBadRequest, "service lease rejected: "+err.Error())
		return
	}
	writeJSON(w, http.StatusCreated, lease)
}

func (s *Server) handleServiceLeaseHeartbeat(w http.ResponseWriter, r *http.Request) {
	auth := r.Context().Value(ctxWorker).(*WorkerAuth)
	leaseID, ok := pathUUID(w, r)
	if !ok {
		return
	}
	var heartbeat ServiceLeaseHeartbeat
	raw, err := io.ReadAll(io.LimitReader(r.Body, 32<<10+1))
	if err != nil || len(raw) > 32<<10 || decodeStrictJSONObject(raw, &heartbeat) != nil {
		writeErr(w, http.StatusBadRequest, "invalid service lease heartbeat")
		return
	}
	if err := s.store.HeartbeatServiceLease(r.Context(), *auth, leaseID, heartbeat); errors.Is(err, errNotFound) {
		writeErr(w, http.StatusNotFound, "service lease not found")
	} else if err != nil {
		writeErr(w, http.StatusConflict, "service lease heartbeat refused: "+err.Error())
	} else {
		w.WriteHeader(http.StatusNoContent)
	}
}

func (s *Server) handleWorkerServiceLeaseAssignments(w http.ResponseWriter, r *http.Request) {
	auth := r.Context().Value(ctxWorker).(*WorkerAuth)
	assignments, err := s.store.ListWorkerServiceLeaseAssignments(r.Context(), *auth)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "service lease assignments unavailable")
		return
	}
	writeJSON(w, http.StatusOK, assignments)
}

func (s *Server) handleServiceLeaseReceipt(w http.ResponseWriter, r *http.Request) {
	auth := r.Context().Value(ctxBuyer).(*AuthResult)
	leaseID, ok := pathUUID(w, r)
	if !ok {
		return
	}
	receipt, err := s.store.GetServiceLeaseReceipt(r.Context(), auth.BuyerID, leaseID)
	if errors.Is(err, errNotFound) {
		writeErr(w, http.StatusNotFound, "service lease not found")
		return
	}
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "service lease receipt unavailable")
		return
	}
	writeJSON(w, http.StatusOK, receipt)
}
