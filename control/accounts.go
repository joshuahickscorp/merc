package main

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"golang.org/x/crypto/bcrypt"
)

const bcryptCost = 12

const sandboxFreeCreditUSD = 0.0

const sessionTTL = 30 * 24 * time.Hour

const (
	maxLoginFails   = 5
	loginLockout    = 30 * time.Second
	loginLockoutMax = 15 * time.Minute
	loginGuardCap   = 8192
)

type loginAttempt struct {
	fails       int
	lockedUntil time.Time
}

type loginGuardT struct {
	mu sync.Mutex
	m  map[string]*loginAttempt
}

var loginGuard = &loginGuardT{m: map[string]*loginAttempt{}}

func (g *loginGuardT) allow(email string, now time.Time) (bool, time.Duration) {
	g.mu.Lock()
	defer g.mu.Unlock()
	if a := g.m[email]; a != nil && now.Before(a.lockedUntil) {
		return false, a.lockedUntil.Sub(now)
	}
	return true, 0
}

func (g *loginGuardT) fail(email string, now time.Time) {
	g.mu.Lock()
	defer g.mu.Unlock()
	if len(g.m) > loginGuardCap {
		for k, a := range g.m {
			if now.After(a.lockedUntil) {
				delete(g.m, k)
			}
		}
	}
	a := g.m[email]
	if a == nil {
		a = &loginAttempt{}
		g.m[email] = a
	}
	a.fails++
	if a.fails >= maxLoginFails {
		d := loginLockout << (a.fails - maxLoginFails)
		if d <= 0 || d > loginLockoutMax {
			d = loginLockoutMax
		}
		a.lockedUntil = now.Add(d)
	}
}

func (g *loginGuardT) success(email string) {
	g.mu.Lock()
	delete(g.m, email)
	g.mu.Unlock()
}

func (s *Store) CreateBuyerAccount(ctx context.Context, email, password string, freeCreditUSD float64) (uuid.UUID, error) {
	hash, err := bcrypt.GenerateFromPassword([]byte(password), bcryptCost)
	if err != nil {
		return uuid.Nil, err
	}
	var id uuid.UUID
	err = s.pool.QueryRow(ctx,
		`INSERT INTO buyers (email, password_hash, free_credit_usd)
		 VALUES (lower($1), $2, $3)
		 RETURNING id`,
		email, string(hash), freeCreditUSD,
	).Scan(&id)
	if err != nil {
		if isUniqueViolation(err) {
			return uuid.Nil, errEmailTaken
		}
		return uuid.Nil, err
	}
	return id, nil
}

func (s *Store) VerifyBuyerPassword(ctx context.Context, email, password string) (uuid.UUID, error) {
	var (
		id   uuid.UUID
		hash *string
	)
	err := s.pool.QueryRow(ctx,
		`SELECT id, password_hash FROM buyers WHERE email = lower($1)`, email,
	).Scan(&id, &hash)
	if errors.Is(err, pgx.ErrNoRows) {
		_ = bcrypt.CompareHashAndPassword([]byte("$2a$12$"+strings.Repeat("x", 53)), []byte(password))
		return uuid.Nil, errNotFound
	}
	if err != nil {
		return uuid.Nil, err
	}
	if hash == nil {
		_ = bcrypt.CompareHashAndPassword([]byte("$2a$12$"+strings.Repeat("x", 53)), []byte(password))
		return uuid.Nil, errNotFound
	}
	if err := bcrypt.CompareHashAndPassword([]byte(*hash), []byte(password)); err != nil {
		return uuid.Nil, errNotFound
	}
	return id, nil
}

func (s *Store) CreateSession(ctx context.Context, buyerID uuid.UUID, ttl time.Duration) (string, error) {
	raw := newSecret("cx_sess_")
	if raw == "" {
		return "", errors.New("session token: entropy failure")
	}
	_, err := s.pool.Exec(ctx,
		`INSERT INTO sessions (token_hash, buyer_id, expires_at, revoked)
		 VALUES ($1, $2, $3, false)`,
		hashKey(raw), buyerID, time.Now().Add(ttl),
	)
	if err != nil {
		return "", err
	}
	return raw, nil
}

func (s *Store) LookupSession(ctx context.Context, rawToken string) (AuthResult, error) {
	var r AuthResult
	err := s.pool.QueryRow(ctx,
		`SELECT buyer_id FROM sessions
		 WHERE token_hash = $1 AND revoked = false AND expires_at > now()`,
		hashKey(rawToken),
	).Scan(&r.BuyerID)
	if errors.Is(err, pgx.ErrNoRows) {
		return r, errNotFound
	}
	return r, err
}

func (s *Store) RevokeSession(ctx context.Context, rawToken string) error {
	_, err := s.pool.Exec(ctx,
		`UPDATE sessions SET revoked = true WHERE token_hash = $1`, hashKey(rawToken))
	return err
}

func (s *Store) BuyerFreeCreditRemaining(ctx context.Context, buyerID uuid.UUID) (float64, error) {
	var remaining float64
	err := s.pool.QueryRow(ctx,
		`SELECT GREATEST(
		          b.free_credit_usd
		          - COALESCE((SELECT -SUM(amount_usd) FROM ledger_entries
		                       WHERE buyer_id = b.id AND kind = 'buyer_charge'), 0)
		          - COALESCE((SELECT SUM(estimated_usd) FROM jobs
		                       WHERE buyer_id = b.id AND status IN ('queued','running','verifying')), 0),
		          0)::float8
		   FROM buyers b WHERE b.id = $1`, buyerID,
	).Scan(&remaining)
	if errors.Is(err, pgx.ErrNoRows) {
		return 0, nil
	}
	return remaining, err
}

func (s *Store) BuyerEmail(ctx context.Context, buyerID uuid.UUID) (string, error) {
	var email string
	err := s.pool.QueryRow(ctx,
		`SELECT email FROM buyers WHERE id = $1`, buyerID,
	).Scan(&email)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", nil
	}
	if err != nil {
		return "", err
	}
	return email, nil
}

var errEmailTaken = errors.New("email already registered")

func isUniqueViolation(err error) bool {
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) {
		return pgErr.Code == "23505"
	}
	return false
}

type signupRequest struct {
	Email    string `json:"email"`
	Password string `json:"password"`
}

func (s *Server) handleSignup(w http.ResponseWriter, r *http.Request) {
	if isRemote(r) && !s.signupLimiter.allow(clientIP(r)) {
		writeErr(w, http.StatusTooManyRequests, "too many accounts created from this address today")
		return
	}

	var req signupRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeErr(w, http.StatusBadRequest, "invalid signup json: "+err.Error())
		return
	}
	email := normalizeEmail(req.Email)
	if !looksLikeEmail(email) {
		writeErr(w, http.StatusBadRequest, "a valid email is required")
		return
	}
	if len(req.Password) < 8 {
		writeErr(w, http.StatusBadRequest, "password must be at least 8 characters")
		return
	}
	if len(req.Password) > 72 {
		writeErr(w, http.StatusBadRequest, "password must be at most 72 bytes")
		return
	}

	grant := sandboxFreeCreditUSD
	if v := envFloat("CX_SANDBOX_CREDIT_USD", grant); v >= 0 {
		grant = v
	}
	buyerID, err := s.store.CreateBuyerAccount(r.Context(), email, req.Password, grant)
	if errors.Is(err, errEmailTaken) {
		writeErr(w, http.StatusConflict, "email already registered")
		return
	}
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "creating account: "+err.Error())
		return
	}

	_, sandboxKey, _, kerr := s.store.CreateAPIKey(r.Context(), buyerID, "sandbox", true)

	token, err := s.store.CreateSession(r.Context(), buyerID, sessionTTL)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "issuing session: "+err.Error())
		return
	}

	resp := map[string]any{
		"buyer_id":        buyerID,
		"token":           token,
		"email":           email,
		"free_credit_usd": grant,
	}
	if kerr == nil {
		resp["sandbox_key"] = sandboxKey // cx_test_… · revealed once, for CLI/SDK use
	} else {
		resp["sandbox_key_error"] = kerr.Error() // honest: the grant stands, the key mint did not
	}
	writeJSON(w, http.StatusCreated, resp)
}

func (s *Server) handleLogin(w http.ResponseWriter, r *http.Request) {
	var req signupRequest // same shape: {email, password}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeErr(w, http.StatusBadRequest, "invalid login json: "+err.Error())
		return
	}
	email := normalizeEmail(req.Email)
	now := time.Now()
	if ok, retry := loginGuard.allow(email, now); !ok {
		w.Header().Set("Retry-After", strconv.Itoa(int(retry.Seconds())+1))
		writeErr(w, http.StatusTooManyRequests, "too many failed login attempts · try again later")
		return
	}
	buyerID, err := s.store.VerifyBuyerPassword(r.Context(), email, req.Password)
	if errors.Is(err, errNotFound) {
		loginGuard.fail(email, now)
		writeErr(w, http.StatusUnauthorized, "invalid email or password")
		return
	}
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "verifying credentials: "+err.Error())
		return
	}
	loginGuard.success(email)
	token, err := s.store.CreateSession(r.Context(), buyerID, sessionTTL)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "issuing session: "+err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"buyer_id": buyerID, "token": token})
}

func (s *Server) handleLogout(w http.ResponseWriter, r *http.Request) {
	raw := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
	if strings.HasPrefix(raw, "cx_sess_") {
		if err := s.store.RevokeSession(r.Context(), raw); err != nil {
			writeErr(w, http.StatusInternalServerError, "logout: "+err.Error())
			return
		}
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) handleMe(w http.ResponseWriter, r *http.Request) {
	auth := r.Context().Value(ctxBuyer).(*AuthResult)
	email, err := s.store.BuyerEmail(r.Context(), auth.BuyerID)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "reading account: "+err.Error())
		return
	}
	free, err := s.store.BuyerFreeCreditRemaining(r.Context(), auth.BuyerID)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "reading credit: "+err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"buyer_id":                  auth.BuyerID,
		"email":                     email,
		"is_admin":                  auth.IsAdmin,
		"free_credit_remaining_usd": free,
	})
}

func normalizeEmail(s string) string { return strings.ToLower(strings.TrimSpace(s)) }

func envFloat(name string, def float64) float64 {
	if v := strings.TrimSpace(os.Getenv(name)); v != "" {
		if f, err := strconv.ParseFloat(v, 64); err == nil {
			return f
		}
	}
	return def
}

func looksLikeEmail(s string) bool {
	at := strings.IndexByte(s, '@')
	if at <= 0 || at != strings.LastIndexByte(s, '@') {
		return false
	}
	domain := s[at+1:]
	return strings.Contains(domain, ".") && !strings.HasPrefix(domain, ".") && !strings.HasSuffix(domain, ".")
}
