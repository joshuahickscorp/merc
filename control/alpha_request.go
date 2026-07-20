package main

import (
	"context"
	"encoding/json"
	"net/http"
)

type alphaRequestBody struct {
	Email string `json:"email"`
	Role  string `json:"role"` // "buyer" | "supplier" | ""  -  which CTA was clicked
	Note  string `json:"note"`
}

const (
	alphaRequestNoteMaxLen = 2000
	alphaRoleMaxLen        = 32
)

func (s *Server) handleAlphaRequest(w http.ResponseWriter, r *http.Request) {
	var req alphaRequestBody
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeErr(w, http.StatusBadRequest, "invalid alpha-request json: "+err.Error())
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
	if err := s.store.CreateAlphaRequest(r.Context(), email, role, note, clientIP(r)); err != nil {
		writeErr(w, http.StatusInternalServerError, "recording alpha request: "+err.Error())
		return
	}
	writeJSON(w, http.StatusCreated, map[string]bool{"ok": true})
}

func (s *Store) CreateAlphaRequest(ctx context.Context, email, role, note, sourceIP string) error {
	_, err := s.pool.Exec(ctx,
		`INSERT INTO alpha_requests (email, role, note, source_ip) VALUES ($1, $2, $3, $4)`,
		email, role, note, sourceIP,
	)
	return err
}
