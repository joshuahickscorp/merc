package main

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"time"
)

var errAlphaConsentRequired = errors.New("alpha request requires consent_at")

type alphaRequestBody struct {
	Email   string `json:"email"`
	Role    string `json:"role"` // "buyer" | "supplier" | ""  -  which CTA was clicked
	Note    string `json:"note"`
	Consent bool   `json:"consent"` // required; must be true to capture contact data
}

const (
	alphaRequestNoteMaxLen = 2000
	alphaRoleMaxLen        = 32
)

func (s *Server) handleAlphaRequest(w http.ResponseWriter, r *http.Request) {
	if s.canary.Enabled {
		writeErr(w, http.StatusForbidden, "public alpha enrollment is disabled during the private canary")
		return
	}
	var req alphaRequestBody
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeErr(w, http.StatusBadRequest, "invalid alpha-request json: "+err.Error())
		return
	}
	if !req.Consent {
		writeErr(w, http.StatusBadRequest, "consent is required to submit an access request")
		return
	}
	email := normalizeEmail(req.Email)
	if !looksLikeEmail(email) {
		writeErr(w, http.StatusBadRequest, "a valid email is required")
		return
	}
	role := req.Role
	if len(role) > alphaRoleMaxLen {
		role = role[:alphaRoleMaxLen]
	}
	note := req.Note
	if len(note) > alphaRequestNoteMaxLen {
		note = note[:alphaRequestNoteMaxLen]
	}
	if err := s.store.CreateAlphaRequest(r.Context(), email, role, note, clientIP(r), time.Now().UTC()); err != nil {
		writeErr(w, http.StatusInternalServerError, "recording alpha request: "+err.Error())
		return
	}
	writeJSON(w, http.StatusCreated, map[string]bool{"ok": true})
}

func (s *Store) CreateAlphaRequest(ctx context.Context, email, role, note, sourceIP string, consentAt time.Time) error {
	if consentAt.IsZero() {
		return errAlphaConsentRequired
	}
	_, err := s.pool.Exec(ctx, `
		INSERT INTO alpha_requests (email, role, note, source_ip, consent_at, status)
		VALUES ($1, $2, $3, $4, $5, 'pending')`,
		email, role, note, sourceIP, consentAt,
	)
	return err
}
