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
	maxLoginFails    = 5
	loginLockout     = 30 * time.Second
	loginLockoutMax  = 15 * time.Minute
	loginGuardCap    = 8192
	maxLoginEmailLen = 254 // RFC 5321 path-element upper bound
)

type loginAttempt struct {
	fails       int
	lockedUntil time.Time
	touched     time.Time // oldest-first eviction under a hard cap
}

// loginGuardT is a hard-capped map of login lockout state. Shape mirrors
// rateLimiter (mutex + map + sweep): entries are reaped by a background
// sweeper and, on insert pressure, by oldest-first eviction so the cap is a
// real ceiling even when every entry is still locked.
type loginGuardT struct {
	mu  sync.Mutex
	m   map[string]*loginAttempt
	cap int
}

var loginGuard = newLoginGuard(loginGuardCap)

func newLoginGuard(cap int) *loginGuardT {
	if cap < 1 {
		cap = 1
	}
	return &loginGuardT{m: map[string]*loginAttempt{}, cap: cap}
}

// validLoginIdentifier rejects attacker-chosen strings before they become map
// keys. Length and shape only — password verification still decides existence.
func validLoginIdentifier(email string) bool {
	if email == "" || len(email) > maxLoginEmailLen {
		return false
	}
	return looksLikeEmail(email)
}

func (g *loginGuardT) allow(email string, now time.Time) (bool, time.Duration) {
	g.mu.Lock()
	defer g.mu.Unlock()
	if a := g.m[email]; a != nil && now.Before(a.lockedUntil) {
		a.touched = now
		return false, a.lockedUntil.Sub(now)
	}
	return true, 0
}

func (g *loginGuardT) fail(email string, now time.Time) {
	g.mu.Lock()
	defer g.mu.Unlock()
	if _, exists := g.m[email]; !exists {
		// Leave room for the new key: expired first, then oldest-first so a
		// flood of still-locked entries cannot grow the map without bound.
		g.evictToLocked(now, g.cap-1)
	}
	a := g.m[email]
	if a == nil {
		a = &loginAttempt{}
		g.m[email] = a
	}
	a.fails++
	a.touched = now
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

// sweep removes idle unlocked entries. Called from the rate-limit sweeper so
// the guard does not need its own ticker. Locked entries stay until they expire
// or oldest-first eviction under cap pressure reclaims them.
func (g *loginGuardT) sweep(now time.Time) {
	g.mu.Lock()
	defer g.mu.Unlock()
	for k, a := range g.m {
		if now.Before(a.lockedUntil) {
			continue
		}
		// Drop only when the lock has lapsed and the entry has been idle for a
		// full max lockout window — progressive fail counts are no longer
		// useful for an address nobody is probing.
		if now.Sub(a.touched) >= loginLockoutMax {
			delete(g.m, k)
		}
	}
}

// evictToLocked shrinks the map to at most target entries. Prefer expired
// lockouts; if still over target, drop the oldest touched entry regardless of
// lock state so the cap cannot be defeated by sustained lockouts.
func (g *loginGuardT) evictToLocked(now time.Time, target int) {
	if target < 0 {
		target = 0
	}
	for k, a := range g.m {
		if len(g.m) <= target {
			return
		}
		if !now.Before(a.lockedUntil) {
			delete(g.m, k)
		}
	}
	for len(g.m) > target {
		var oldestKey string
		var oldestTime time.Time
		first := true
		for k, a := range g.m {
			if first || a.touched.Before(oldestTime) {
				oldestKey = k
				oldestTime = a.touched
				first = false
			}
		}
		if oldestKey == "" {
			return
		}
		delete(g.m, oldestKey)
	}
}

func (g *loginGuardT) len() int {
	g.mu.Lock()
	defer g.mu.Unlock()
	return len(g.m)
}

func (s *Store) CreateBuyerAccount(ctx context.Context, email, password string, freeCreditUSD float64) (uuid.UUID, error) {
	email = strings.ToLower(strings.TrimSpace(email))
	hash, err := bcrypt.GenerateFromPassword([]byte(password), bcryptCost)
	if err != nil {
		return uuid.Nil, err
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return uuid.Nil, err
	}
	defer tx.Rollback(ctx)
	if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtextextended($1,0))`,
		"buyer-email|"+email); err != nil {
		return uuid.Nil, err
	}
	var tombstoned bool
	if err := tx.QueryRow(ctx, `
		SELECT EXISTS (
		  SELECT 1 FROM buyer_identity_tombstones
		   WHERE email_sha256=encode(digest($1,'sha256'),'hex'))`, email).Scan(&tombstoned); err != nil {
		return uuid.Nil, err
	}
	if tombstoned {
		return uuid.Nil, errEmailTaken
	}
	var id uuid.UUID
	err = tx.QueryRow(ctx,
		`INSERT INTO buyers (email, password_hash, free_credit_usd)
		 VALUES ($1, $2, $3)
		 RETURNING id`,
		email, string(hash), freeCreditUSD,
	).Scan(&id)
	if err != nil {
		if isUniqueViolation(err) {
			return uuid.Nil, errEmailTaken
		}
		return uuid.Nil, err
	}
	if err := tx.Commit(ctx); err != nil {
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
		`SELECT id, password_hash FROM buyers WHERE email = lower($1) AND deleted_at IS NULL`, email,
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
	tag, err := s.pool.Exec(ctx,
		`INSERT INTO sessions (token_hash, buyer_id, expires_at, revoked)
		 SELECT $1, id, $3, false FROM buyers WHERE id=$2 AND deleted_at IS NULL`,
		hashKey(raw), buyerID, time.Now().Add(ttl),
	)
	if err != nil {
		return "", err
	}
	if tag.RowsAffected() != 1 {
		return "", errNotFound
	}
	return raw, nil
}

func (s *Store) LookupSession(ctx context.Context, rawToken string) (AuthResult, error) {
	var r AuthResult
	err := s.pool.QueryRow(ctx,
		`SELECT s.buyer_id FROM sessions s JOIN buyers b ON b.id=s.buyer_id
		 WHERE s.token_hash = $1 AND s.revoked = false AND s.expires_at > now()
		   AND b.deleted_at IS NULL`,
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

// BuyerFreeCreditRemaining is the free-credit leg of committed-money accounting.
// It is not advisory-only: job intake clamps MaxUSD and refuses submission when
// this figure is exhausted and no payment method is on file (see api.go).
//
// Committed-money shape matches evaluateRealtimeBuyerFunding and
// prepaidOpenReservationMicros so the three rails cannot drift:
//   - open batch estimates
//   - EXECUTING realtime maxima, excluding contracts already held by an ACTIVE
//     envelope (counting both would double-hold)
//   - ACTIVE envelope residual (cap − spent), ceil-nanos → micros → USD
//
// When an envelope leaves ACTIVE, the exclusion falls off and still-EXECUTING
// work is held by the contract term again — the same expiry fallback the other
// two rails apply.
func (s *Store) BuyerFreeCreditRemaining(ctx context.Context, buyerID uuid.UUID) (float64, error) {
	settlement, err := SettlementCurrency()
	if err != nil {
		return 0, err
	}
	// The grant is explicitly free_credit_usd. It is never converted or
	// relabelled into another settlement currency; non-USD deployments require
	// collected prepaid cash or a payment method.
	if settlement.Code() != "usd" {
		return 0, nil
	}
	var remaining float64
	err = s.pool.QueryRow(ctx,
		`SELECT GREATEST(
		          b.free_credit_usd
		          - COALESCE((SELECT -SUM(amount_usd) FROM ledger_entries
		                       WHERE buyer_id = b.id
		                         AND currency = 'usd'
		                         AND kind IN ('buyer_charge','buyer_refund')), 0)
		          - COALESCE((SELECT SUM(estimated_usd) FROM jobs
		                       WHERE buyer_id = b.id AND currency='usd'
		                         AND status IN ('queued','running','verifying')), 0)
		          - COALESCE((SELECT SUM(c.maximum_price_usd) FROM execution_contracts c
		                       WHERE c.buyer_id = b.id AND c.currency='usd' AND c.state = 'EXECUTING'
		                         AND NOT EXISTS (
		                           SELECT 1 FROM execution_envelope_spends s
		                             JOIN execution_envelopes e ON e.id = s.envelope_id
		                            WHERE s.contract_id = c.id AND e.state = 'ACTIVE'
		                         )), 0)
		          - COALESCE((SELECT SUM(((e.cap_nanos - e.spent_nanos) + 999) / 1000)
		                        FROM execution_envelopes e
		                       WHERE e.buyer_id = b.id AND e.currency='usd' AND e.state = 'ACTIVE'), 0)::float8 / 1000000.0,
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
		`SELECT email FROM buyers WHERE id = $1 AND deleted_at IS NULL`, buyerID,
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
	if !s.canary.allowsBuyerEmail(email) {
		writeErr(w, http.StatusForbidden, "email is not approved for this private canary")
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
	if v := envFloat("MERC_SANDBOX_CREDIT_USD", grant); v >= 0 {
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
	w.Header().Set("Cache-Control", "no-store, private")
	w.Header().Set("Pragma", "no-cache")
	writeJSON(w, http.StatusCreated, resp)
}

func (s *Server) handleLogin(w http.ResponseWriter, r *http.Request) {
	var req signupRequest // same shape: {email, password}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeErr(w, http.StatusBadRequest, "invalid login json: "+err.Error())
		return
	}
	email := normalizeEmail(req.Email)
	// Bound and shape-check before the string is ever used as a loginGuard key.
	// Signup already enforces looksLikeEmail; login must not accept free-form
	// attacker text that would otherwise inflate the guard map.
	if !validLoginIdentifier(email) {
		writeErr(w, http.StatusBadRequest, "login identifier rejected: well-formed email at most 254 characters required")
		return
	}
	if !s.canary.allowsBuyerEmail(email) {
		writeErr(w, http.StatusForbidden, "email is not approved for this private canary")
		return
	}
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
	w.Header().Set("Cache-Control", "no-store, private")
	w.Header().Set("Pragma", "no-cache")
	writeJSON(w, http.StatusOK, map[string]any{"buyer_id": buyerID, "token": token})
}

func (s *Server) handleLogout(w http.ResponseWriter, r *http.Request) {
	raw, ok := bearer(r)
	if ok && strings.HasPrefix(raw, "cx_sess_") {
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
