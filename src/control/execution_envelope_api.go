package main

import (
	"errors"
	"io"
	"net/http"
	"strings"

	"github.com/google/uuid"
)

// handleCreateExecutionEnvelope creates a pre-authorized spend grant. The full
// buyer-funding transaction runs once here; subsequent realtime requests spend
// the envelope without re-entering that critical section.
func (s *Server) handleCreateExecutionEnvelope(w http.ResponseWriter, r *http.Request) {
	auth := r.Context().Value(ctxBuyer).(*AuthResult)
	raw, err := io.ReadAll(io.LimitReader(r.Body, 32<<10+1))
	if err != nil || len(raw) > 32<<10 {
		writeErr(w, http.StatusBadRequest, "invalid execution envelope request")
		return
	}
	var req ExecutionEnvelopeCreateRequest
	if err := decodeStrictJSONObject(raw, &req); err != nil {
		writeErr(w, http.StatusBadRequest, "invalid execution envelope request: "+err.Error())
		return
	}
	env, err := s.store.CreateExecutionEnvelope(r.Context(), auth.BuyerID, req)
	if errors.Is(err, errRealtimeInsufficientFunds) || errors.Is(err, errRealtimeTopupRequired) {
		writeErr(w, http.StatusPaymentRequired, err.Error())
		return
	}
	if err != nil {
		writeErr(w, http.StatusBadRequest, "execution envelope rejected: "+err.Error())
		return
	}
	writeJSON(w, http.StatusCreated, env)
}

func (s *Server) handleGetExecutionEnvelope(w http.ResponseWriter, r *http.Request) {
	auth := r.Context().Value(ctxBuyer).(*AuthResult)
	id, err := uuid.Parse(strings.TrimSpace(r.PathValue("id")))
	if err != nil {
		writeErr(w, http.StatusBadRequest, "invalid execution envelope id")
		return
	}
	env, err := s.store.GetExecutionEnvelope(r.Context(), auth.BuyerID, id)
	if errors.Is(err, errEnvelopeNotFound) {
		writeErr(w, http.StatusNotFound, "execution envelope not found")
		return
	}
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "execution envelope lookup failed")
		return
	}
	writeJSON(w, http.StatusOK, env)
}

// parseEnvelopeIDHeader reads X-Merc-Envelope-Id from a realtime request.
// Empty means the legacy per-request funding path.
func parseEnvelopeIDHeader(r *http.Request) (uuid.UUID, error) {
	raw := strings.TrimSpace(r.Header.Get("X-Merc-Envelope-Id"))
	if raw == "" {
		return uuid.Nil, nil
	}
	id, err := uuid.Parse(raw)
	if err != nil {
		return uuid.Nil, errors.New("X-Merc-Envelope-Id must be a UUID")
	}
	return id, nil
}
