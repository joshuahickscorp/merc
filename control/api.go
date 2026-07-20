package main

import (
	"bufio"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"math"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"
	"golang.org/x/sync/errgroup"
)

type Server struct {
	store         *Store
	storage       *Storage
	verifier      *Verifier
	verification  *VerificationProcessor
	payout        Payout
	ipLimiter     *rateLimiter
	buyerLimiter  *rateLimiter
	workerLimiter *rateLimiter
	signupLimiter *rateLimiter
	canary        CanaryPolicy
}

func NewServer(store *Store, storage *Storage, verifier *Verifier, payout Payout) *Server {
	return &Server{
		store: store, storage: storage, verifier: verifier,
		verification: NewVerificationProcessor(store, storage, verifier), payout: payout,
		ipLimiter:     newRateLimiter(30, 60), // 30 req/s, burst 60, per IP
		buyerLimiter:  newRateLimiter(20, 40), // 20 req/s, burst 40, per api key
		workerLimiter: newRateLimiter(30, 60), // 30 req/s, burst 60, per worker token
		signupLimiter: newRateLimiter(signupsPerIPPerDay/86400.0, signupsPerIPPerDay),
		canary:        loadCanaryPolicyFromEnv(),
	}
}

const signupsPerIPPerDay = 5

type ctxKey int

const (
	ctxBuyer  ctxKey = iota // *AuthResult for buyer/admin routes
	ctxWorker               // *WorkerAuth for worker routes
	ctxAdmin                // AdminActor for routes authenticated by authAdmin
)

func (s *Server) Routes() http.Handler {
	mux := http.NewServeMux()

	mux.HandleFunc("GET /healthz", s.handleHealthz)
	mux.HandleFunc("GET /readyz", s.handleReadyz)
	mux.HandleFunc("GET /version", s.handleVersion)
	mux.HandleFunc("GET /metrics", s.handleMetrics)
	mux.HandleFunc("GET /{$}", s.handleRoot)
	mux.HandleFunc("GET /admin", s.handleAdminRoom)
	mux.HandleFunc("GET /assets/site/{path...}", s.handleSiteAsset) // whitelisted public static assets
	mux.HandleFunc("GET /favicon.ico", s.handleFavicon)

	mux.HandleFunc("POST /v1/signup", s.handleSignup)
	mux.HandleFunc("POST /v1/login", s.handleLogin)
	mux.HandleFunc("POST /v1/alpha-request", s.handleAlphaRequest)               // public site's alpha-access capture (alpha_request.go), unauthed lead intake
	mux.Handle("POST /v1/logout", s.authBuyer(http.HandlerFunc(s.handleLogout))) // revoke the presenting session
	mux.Handle("GET /v1/me", s.authBuyer(http.HandlerFunc(s.handleMe)))          // authenticated buyer identity + remaining sandbox credit

	mux.Handle("POST /v1/supplier/onboard", s.authBuyer(http.HandlerFunc(s.handleSupplierOnboard)))
	mux.Handle("GET /v1/supplier/status", s.authBuyer(http.HandlerFunc(s.handleSupplierStatus)))
	mux.Handle("POST /v1/supplier/worker-tokens", s.authBuyer(http.HandlerFunc(s.handleCreateWorkerToken))) // self-serve token mint, one call per new Mac (suppliers.go)
	mux.Handle("POST /v1/supplier/enrollment-approvals", s.authBuyer(http.HandlerFunc(s.handleApproveWorkerEnrollmentRequest)))
	mux.Handle("POST /v1/supplier/enrollment-codes", s.authBuyer(http.HandlerFunc(s.handleCreateWorkerEnrollmentCode)))
	mux.Handle("DELETE /v1/supplier/enrollment-codes/{id}", s.authBuyer(http.HandlerFunc(s.handleRevokeWorkerEnrollmentCode)))
	mux.Handle("GET /v1/supplier/worker-credentials", s.authBuyer(http.HandlerFunc(s.handleListWorkerCredentials)))
	mux.Handle("DELETE /v1/supplier/worker-credentials/{id}", s.authBuyer(http.HandlerFunc(s.handleRevokeWorkerCredential)))
	mux.Handle("GET /v1/supplier/credential-audit", s.authBuyer(http.HandlerFunc(s.handleWorkerCredentialAudit)))
	mux.HandleFunc("POST /v1/worker/enrollment/exchange", s.handleExchangeWorkerEnrollmentCode)

	mux.Handle("POST /v1/jobs", s.authBuyer(http.HandlerFunc(s.handleCreateJob)))
	mux.Handle("GET /v1/jobs/{id}", s.authBuyer(http.HandlerFunc(s.handleGetJob)))
	mux.Handle("GET /v1/jobs/{id}/results", s.authBuyer(http.HandlerFunc(s.handleJobResults)))
	mux.Handle("GET /v1/jobs/{id}/invoice", s.authBuyer(http.HandlerFunc(s.handleJobInvoice)))
	mux.Handle("GET /v1/jobs/{id}/receipt", s.authBuyer(http.HandlerFunc(s.handleJobReceipt)))   // ClearingReceipt (items 13-15)
	mux.Handle("GET /v1/jobs/{id}/events", s.authBuyer(http.HandlerFunc(s.handleJobEvents)))     // Plane C/D: buyer timeline
	mux.Handle("GET /v1/jobs/{id}/failures", s.authBuyer(http.HandlerFunc(s.handleJobFailures))) // Plane C/D: typed failure history
	mux.Handle("POST /v1/jobs/{id}/dispute", s.authBuyer(http.HandlerFunc(s.handleFileDispute))) // buyer-dispute seam (optimistic-verification / payout-guarantee foundation)
	mux.Handle("DELETE /v1/jobs/{id}", s.authBuyer(http.HandlerFunc(s.handleCancelJob)))
	mux.Handle("GET /v1/models", s.authBuyer(http.HandlerFunc(s.handleModels)))
	mux.Handle("GET /v1/price-estimate", s.authBuyer(http.HandlerFunc(s.handlePriceEstimate)))
	mux.Handle("POST /v1/quote", s.authBuyer(http.HandlerFunc(s.handleQuote))) // Plane C: scan + price, no spend
	mux.Handle("POST /v1/webhooks", s.authBuyer(http.HandlerFunc(s.handleRegisterWebhook)))

	mux.Handle("POST /v1/billing/setup", s.authBuyer(http.HandlerFunc(s.handleBillingSetup)))
	mux.Handle("GET /v1/billing/status", s.authBuyer(http.HandlerFunc(s.handleBillingStatus)))
	mux.HandleFunc("POST /v1/stripe/webhook", s.handleStripeWebhook)          // unauthed; verified by signature
	mux.HandleFunc("POST /v1/stripe/connect-webhook", s.handleConnectWebhook) // Connect account.updated; verified by signature

	mux.Handle("POST /v1/keys", s.authBuyer(http.HandlerFunc(s.handleCreateKey)))
	mux.Handle("GET /v1/keys", s.authBuyer(http.HandlerFunc(s.handleListKeys)))
	mux.Handle("DELETE /v1/keys/{id}", s.authBuyer(http.HandlerFunc(s.handleRevokeKey)))

	mux.Handle("POST /v1/worker/register", s.authWorker(http.HandlerFunc(s.handleWorkerRegister)))
	mux.Handle("POST /v1/worker/heartbeat", s.authWorker(http.HandlerFunc(s.handleWorkerHeartbeat)))
	mux.Handle("GET /v1/worker/poll", s.authWorker(http.HandlerFunc(s.handleWorkerPoll)))
	mux.Handle("POST /v1/worker/task/{id}/start", s.authWorker(http.HandlerFunc(s.handleWorkerStart)))
	mux.Handle("POST /v1/worker/task/{id}/commit", s.authWorker(http.HandlerFunc(s.handleWorkerCommit)))
	mux.Handle("POST /v1/worker/task/{id}/fail", s.authWorker(http.HandlerFunc(s.handleWorkerFail))) // Plane C/D: immediate typed failure
	mux.Handle("GET /v1/worker/earnings", s.authWorker(http.HandlerFunc(s.handleWorkerEarnings)))
	mux.Handle("GET /v1/worker/verification", s.authWorker(http.HandlerFunc(s.handleWorkerVerification))) // trust panel (Supplier onboarding & safety 7->8)
	mux.Handle("GET /v1/worker/connect/status", s.authWorker(http.HandlerFunc(s.handleWorkerConnectStatus)))

	mux.Handle("GET /admin/workers", s.authAdmin(http.HandlerFunc(s.handleAdminWorkers)))
	mux.Handle("GET /admin/jobs", s.authAdmin(http.HandlerFunc(s.handleAdminJobs)))
	mux.Handle("GET /admin/payouts", s.authAdmin(http.HandlerFunc(s.handleAdminPayouts)))
	mux.Handle("GET /admin/fraud-flags", s.authAdmin(http.HandlerFunc(s.handleAdminFraudFlags)))
	mux.Handle("GET /admin/fraud", s.authAdmin(http.HandlerFunc(s.handleAdminFraud)))
	mux.Handle("GET /admin/drift", s.authAdmin(http.HandlerFunc(s.handleAdminDrift)))
	mux.Handle("GET /admin/quotes", s.authAdmin(http.HandlerFunc(s.handleAdminQuoteDrift)))
	mux.Handle("GET /admin/scheduler/explain", s.authAdmin(http.HandlerFunc(s.handleAdminSchedulerExplain)))
	mux.Handle("POST /admin/workers/{id}/suspend", s.authAdmin(http.HandlerFunc(s.handleAdminSuspend)))
	mux.Handle("POST /admin/workers/{id}/reinstate", s.authAdmin(http.HandlerFunc(s.handleAdminReinstate)))
	mux.Handle("POST /admin/tasks/{id}/requeue", s.authAdmin(http.HandlerFunc(s.handleAdminRequeueTask)))
	mux.Handle("POST /admin/suppliers/{id}/reputation", s.authAdmin(http.HandlerFunc(s.handleAdminAdjustReputation)))
	mux.Handle("POST /admin/payouts/{id}/release", s.authAdmin(http.HandlerFunc(s.handleAdminReleasePayout)))
	mux.Handle("POST /admin/subsidy-funds", s.authAdmin(http.HandlerFunc(s.handleAdminCreateSubsidyFund)))
	mux.Handle("POST /admin/payouts/{id}/subsidize", s.authAdmin(http.HandlerFunc(s.handleAdminSubsidizePayout)))
	mux.Handle("GET /admin/actions", s.authAdmin(http.HandlerFunc(s.handleAdminActions))) // audit log for the above
	mux.Handle("GET /admin/controls", s.authAdmin(http.HandlerFunc(s.handleAdminControls)))
	mux.Handle("POST /admin/controls/{name}", s.authAdmin(http.HandlerFunc(s.handleAdminSetControl)))

	return observe(s.ipLimiter.limitByIP(capBody(requestBodyLimit, mux)))
}

type statusRecorder struct {
	http.ResponseWriter
	status int
}

func (r *statusRecorder) WriteHeader(code int)        { r.status = code; r.ResponseWriter.WriteHeader(code) }
func (r *statusRecorder) Unwrap() http.ResponseWriter { return r.ResponseWriter }

const maxRequestBodyBytes = 72 << 20 // 72 MiB

const maxJSONRequestBodyBytes = 4 << 20 // 4 MiB

const maxJobSubmitBodyBytes = 32 << 20 // 32 MiB

const maxSynchronousInputBytes = maxJobSubmitBodyBytes

var errSynchronousInputTooLarge = errors.New("synchronous input exceeds the 32 MiB control-plane scan ceiling")

func readAndCloseBounded(r io.ReadCloser, limit int64) ([]byte, error) {
	if r == nil {
		return nil, errors.New("nil input reader")
	}
	data, readErr := io.ReadAll(io.LimitReader(r, limit+1))
	closeErr := r.Close()
	if readErr != nil {
		return nil, readErr
	}
	if int64(len(data)) > limit {
		return nil, fmt.Errorf("%w (max %d bytes)", errSynchronousInputTooLarge, limit)
	}
	if closeErr != nil {
		return nil, closeErr
	}
	return data, nil
}

func readSynchronousInput(r io.ReadCloser) ([]byte, error) {
	return readAndCloseBounded(r, maxSynchronousInputBytes)
}

func capBody(limitFor func(*http.Request) int64, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		r.Body = http.MaxBytesReader(w, r.Body, limitFor(r))
		next.ServeHTTP(w, r)
	})
}

func requestBodyLimit(r *http.Request) int64 {
	if r.Method == http.MethodPost && r.URL.Path == "/v1/jobs" {
		return maxJobSubmitBodyBytes
	}
	if r.Method == http.MethodPost && r.URL.Path == "/v1/files" {
		return maxRequestBodyBytes
	}
	return maxJSONRequestBodyBytes
}

func observe(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		rid := r.Header.Get("X-Request-ID")
		if rid == "" {
			rid = uuid.NewString()
		}
		w.Header().Set("X-Request-ID", rid)
		if r.URL.Path == "/healthz" || r.URL.Path == "/metrics" {
			next.ServeHTTP(w, r)
			return
		}
		start := time.Now()
		rec := &statusRecorder{ResponseWriter: w, status: http.StatusOK}
		next.ServeHTTP(rec, r)
		dur := time.Since(start)
		endpoint := r.Pattern
		if endpoint == "" {
			endpoint = "unmatched"
		}
		observeHTTPRequest(endpoint, dur)
		log.Print(formatRequestLog(rid, r.Method, r.URL.Path, rec.status, dur))
	})
}

func formatRequestLog(requestID, method, path string, status int, duration time.Duration) string {
	return fmt.Sprintf("req id=%q method=%q path=%q status=%d duration_ms=%d",
		requestID, method, path, status, duration.Milliseconds())
}

func (s *Server) authBuyer(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		key, ok := bearer(r)
		if !ok {
			writeErr(w, http.StatusUnauthorized, "missing or malformed Authorization bearer token")
			return
		}
		var (
			auth AuthResult
			err  error
		)
		if strings.HasPrefix(key, "cx_sess_") {
			auth, err = s.store.LookupSession(r.Context(), key)
		} else {
			auth, err = s.store.LookupAPIKey(r.Context(), key)
		}
		if err != nil {
			writeErr(w, http.StatusUnauthorized, "invalid credential")
			return
		}
		if s.canary.Enabled && !auth.IsAdmin {
			email, emailErr := s.store.BuyerEmail(r.Context(), auth.BuyerID)
			if emailErr != nil {
				writeErr(w, http.StatusServiceUnavailable, "canary participant check unavailable")
				return
			}
			if !s.canary.allowsBuyerEmail(email) {
				writeErr(w, http.StatusForbidden, "buyer is not approved for this private canary")
				return
			}
		}
		if isRemote(r) && !s.buyerLimiter.allow(auth.BuyerID.String()) {
			writeErr(w, http.StatusTooManyRequests, "rate limit exceeded")
			return
		}
		ctx := context.WithValue(r.Context(), ctxBuyer, &auth)
		next.ServeHTTP(w, r.WithContext(ctx))
	})
}

func (s *Server) authAdmin(next http.Handler) http.Handler {
	return s.authBuyer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		auth := r.Context().Value(ctxBuyer).(*AuthResult)
		if !auth.IsAdmin {
			writeErr(w, http.StatusForbidden, "admin privilege required")
			return
		}
		actor := AdminActor{
			Mode: AdminAuthBreakGlassAPIKey, PrincipalID: auth.APIKeyID,
			AttributionScope: AdminAttributionSharedCredentialOnly, Label: auth.APIKeyLabel,
		}
		ctx := context.WithValue(r.Context(), ctxAdmin, actor)
		next.ServeHTTP(w, r.WithContext(ctx))
	}))
}

func (s *Server) authWorker(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		tok := strings.TrimSpace(r.Header.Get("X-Worker-Token"))
		if tok == "" {
			writeErr(w, http.StatusUnauthorized, "missing X-Worker-Token")
			return
		}
		auth, err := s.store.LookupWorkerToken(r.Context(), tok)
		if err != nil {
			writeErr(w, http.StatusUnauthorized, "invalid worker token")
			return
		}
		if !s.canary.allowsWorker(auth.WorkerID) {
			writeErr(w, http.StatusForbidden, "worker is not approved for this private canary")
			return
		}
		if isRemote(r) && !s.workerLimiter.allow(auth.WorkerID.String()) {
			writeErr(w, http.StatusTooManyRequests, "rate limit exceeded")
			return
		}
		ctx := context.WithValue(r.Context(), ctxWorker, &auth)
		next.ServeHTTP(w, r.WithContext(ctx))
	})
}

func bearer(r *http.Request) (string, bool) {
	h := r.Header.Get("Authorization")
	const p = "Bearer "
	if len(h) <= len(p) || !strings.EqualFold(h[:len(p)], p) {
		return "", false
	}
	return strings.TrimSpace(h[len(p):]), true
}

func (s *Server) handleHealthz(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}

func (s *Server) handleVersion(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, currentControlBuildInfo())
}

func (s *Server) handleReadyz(w http.ResponseWriter, r *http.Request) {
	if s.canary.Enabled && s.canary.configError != nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]any{"status": "not_ready", "reason": "canary policy is incomplete"})
		return
	}
	if err := s.store.Ping(r.Context()); err != nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]any{"status": "not_ready", "reason": "database unreachable"})
		return
	}
	now := time.Now()
	if !workerElectionRecentlyObserved(now) {
		writeJSON(w, http.StatusServiceUnavailable, map[string]any{"status": "not_ready", "reason": "background worker election is not progressing"})
		return
	}
	if stale := liveness.stale(now, workersStarted()); len(stale) > 0 {
		writeJSON(w, http.StatusServiceUnavailable, map[string]any{"status": "not_ready", "stale_tickers": stale})
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "ready"})
}

type jobSubmit struct {
	JobType        JobType            `json:"job_type"`
	Model          ModelRef           `json:"model"`
	Params         json.RawMessage    `json:"params"`
	Constraints    JobConstraints     `json:"constraints"`
	Verification   VerificationPolicy `json:"verification"`
	Tier           string             `json:"tier"`
	Input          json.RawMessage    `json:"input"`
	WebhookURL     string             `json:"webhook_url"`
	MaxUSD         float64            `json:"max_usd,omitempty"`
	QuoteID        string             `json:"quote_id,omitempty"`
	FirmQuote      bool               `json:"firm_quote,omitempty"`
	MinReputation  float32            `json:"min_reputation,omitempty"`
	DeadlineSecs   int                `json:"deadline_secs,omitempty"`
	IdempotencyKey string             `json:"-"`
	RequestSHA256  string             `json:"-"`
}

var idempotencyKeyPattern = regexp.MustCompile(`^[A-Za-z0-9._:-]{8,128}$`)

const defaultSplitSize = 256

func (s *Server) handleCreateJob(w http.ResponseWriter, r *http.Request) {
	auth := r.Context().Value(ctxBuyer).(*AuthResult)
	idempotencyKey := strings.TrimSpace(r.Header.Get("Idempotency-Key"))
	if !idempotencyKeyPattern.MatchString(idempotencyKey) {
		writeErr(w, http.StatusBadRequest, "Idempotency-Key is required and must be 8-128 characters using letters, digits, '.', '_', ':', or '-'")
		return
	}
	raw, err := io.ReadAll(r.Body)
	if err != nil {
		var mbe *http.MaxBytesError
		if errors.As(err, &mbe) {
			writeErr(w, http.StatusRequestEntityTooLarge,
				fmt.Sprintf("request body exceeds the %d byte submission limit", mbe.Limit))
			return
		}
		writeErr(w, http.StatusBadRequest, "reading job submission: "+err.Error())
		return
	}
	var sub jobSubmit
	if err := decodeStrictJSONObject(raw, &sub); err != nil {
		writeErr(w, http.StatusBadRequest, "invalid job submission json: "+err.Error())
		return
	}
	digest := sha256.Sum256(raw)
	sub.IdempotencyKey = idempotencyKey
	sub.RequestSHA256 = hex.EncodeToString(digest[:])
	resp, herr := s.createJob(r.Context(), auth.BuyerID, sub)
	if herr != nil {
		writeErr(w, herr.status, herr.msg)
		return
	}
	if resp.WebhookSecret != "" {
		setSecretResponseHeaders(w)
	}
	if resp.IdempotentReplay {
		w.Header().Set("Idempotent-Replayed", "true")
	}
	writeJSON(w, http.StatusAccepted, resp)
}

type httpError struct {
	status int
	msg    string
}

func (e *httpError) Error() string { return e.msg }

func (s *Server) createJob(ctx context.Context, buyerID uuid.UUID, sub jobSubmit) (JobSubmitResponse, *httpError) {
	if sub.IdempotencyKey != "" {
		if replay, found, err := s.store.JobSubmissionReplay(ctx, buyerID, sub.IdempotencyKey, sub.RequestSHA256); err != nil {
			if errors.Is(err, errIdempotencyConflict) {
				return JobSubmitResponse{}, &httpError{http.StatusConflict, err.Error()}
			}
			return JobSubmitResponse{}, &httpError{http.StatusServiceUnavailable, "idempotency lookup failed: " + err.Error()}
		} else if found {
			replay.IdempotentReplay = true
			return replay, nil
		}
	}
	if math.IsNaN(sub.MaxUSD) || math.IsInf(sub.MaxUSD, 0) || sub.MaxUSD < 0 {
		return JobSubmitResponse{}, &httpError{http.StatusBadRequest, "max_usd must be a finite non-negative number"}
	}
	if err := s.canary.validateJobShape(sub); err != nil {
		return JobSubmitResponse{}, &httpError{http.StatusForbidden, "outside private-canary limits: " + err.Error()}
	}
	if s.canary.Enabled {
		allowed, err := s.store.CanaryBuyerAdmissionAllowed(ctx, buyerID, s.canary.MaxActiveBuyers)
		if err != nil {
			return JobSubmitResponse{}, &httpError{http.StatusServiceUnavailable, "canary buyer-admission check unavailable"}
		}
		if !allowed {
			return JobSubmitResponse{}, &httpError{http.StatusTooManyRequests, "private-canary active-buyer limit reached"}
		}
		counts, err := s.store.CanaryAdmissionCounts(ctx)
		if err != nil {
			return JobSubmitResponse{}, &httpError{http.StatusServiceUnavailable, "canary admission counters unavailable"}
		}
		if counts.QueuedJobs >= s.canary.MaxQueuedJobs {
			return JobSubmitResponse{}, &httpError{http.StatusTooManyRequests, "private-canary queued-job limit reached"}
		}
		if counts.JobsToday >= s.canary.MaxDailyJobs {
			return JobSubmitResponse{}, &httpError{http.StatusTooManyRequests, "private-canary daily-job limit reached"}
		}
		if counts.HeldShadowPayoutUSD >= s.canary.MaxHeldShadowPayoutUSD {
			return JobSubmitResponse{}, &httpError{http.StatusTooManyRequests, "private-canary held-shadow-payout limit reached"}
		}
		// Buyers cannot weaken verification or accelerate payout release in the
		// supervised canary. Every primary receives a same-shape redundancy run,
		// at least one honeypot is requested, and payout remains manually gated.
		sub.Verification.SkipVerificationFloor = false
		if sub.Verification.RedundancyFrac < 1 {
			sub.Verification.RedundancyFrac = 1
		}
		if sub.Verification.HoneypotFrac <= 0 {
			sub.Verification.HoneypotFrac = 0.1
		}
		if sub.Verification.PayoutHoldSecs < 7*24*60*60 {
			sub.Verification.PayoutHoldSecs = 7 * 24 * 60 * 60
		}
	}
	if sub.JobType.Type == "" {
		return JobSubmitResponse{}, &httpError{http.StatusBadRequest, "job_type.type is required"}
	}
	if !validJobTypes[sub.JobType.Type] {
		return JobSubmitResponse{}, &httpError{http.StatusBadRequest, "invalid job_type.type: " + sub.JobType.Type}
	}
	if sub.Tier == "" {
		sub.Tier = "batch"
	}
	if !validTiers[sub.Tier] {
		return JobSubmitResponse{}, &httpError{http.StatusBadRequest, "invalid tier: " + sub.Tier}
	}
	canonicalModel, err := normalizeAdvertisedRuntimeModelRef(sub.JobType.Type, sub.Model)
	if err != nil {
		return JobSubmitResponse{}, &httpError{http.StatusBadRequest, err.Error()}
	}
	sub.Model = canonicalModel
	paused, err := s.store.OperationalControlPaused(ctx, controlIntake)
	if err != nil {
		return JobSubmitResponse{}, &httpError{http.StatusServiceUnavailable, "intake control unavailable"}
	}
	if paused {
		return JobSubmitResponse{}, &httpError{http.StatusServiceUnavailable, "job intake is paused by the operator"}
	}
	economicSchedule, err := LoadEconomicScheduleFromEnv()
	if err != nil {
		return JobSubmitResponse{}, &httpError{http.StatusServiceUnavailable, "economic schedule unavailable: " + err.Error()}
	}
	for _, c := range sub.Constraints.HWClasses {
		if !validHWClasses[c] {
			return JobSubmitResponse{}, &httpError{http.StatusBadRequest, "invalid hw_class: " + c}
		}
	}
	if sub.WebhookURL != "" {
		sub.WebhookURL = strings.TrimSpace(sub.WebhookURL)
		if _, err := validateWebhookURLSyntax(sub.WebhookURL, false); err != nil {
			return JobSubmitResponse{}, &httpError{http.StatusBadRequest, err.Error()}
		}
		if err := requireWebhookSigningKey(); err != nil {
			return JobSubmitResponse{}, &httpError{http.StatusServiceUnavailable,
				"webhook registration unavailable: encrypted signing-secret storage is not configured"}
		}
	}
	if sub.DeadlineSecs != 0 && sub.DeadlineSecs != -1 &&
		(sub.DeadlineSecs < 60 || sub.DeadlineSecs > 604800) {
		return JobSubmitResponse{}, &httpError{http.StatusBadRequest,
			"deadline_secs must be -1 (run to completion), 0 (default watchdog), or 60..604800 seconds"}
	}
	wantVerificationFloor := !sub.Verification.SkipVerificationFloor &&
		sub.Verification.RedundancyFrac <= 0 && sub.Verification.HoneypotFrac <= 0

	if stripeKey() != "" {
		_, pm, berr := s.store.GetBillingCustomer(ctx, buyerID)
		switch {
		case berr != nil && !errors.Is(berr, errNotFound):
			return JobSubmitResponse{}, &httpError{http.StatusServiceUnavailable, "billing lookup failed: " + berr.Error()}
		case errors.Is(berr, errNotFound), pm == "":
			free, ferr := s.store.BuyerFreeCreditRemaining(ctx, buyerID)
			if ferr != nil {
				return JobSubmitResponse{}, &httpError{http.StatusServiceUnavailable, "free-credit lookup failed: " + ferr.Error()}
			}
			if free <= 0 {
				return JobSubmitResponse{}, &httpError{http.StatusPaymentRequired, "no payment method on file and sandbox free credit is exhausted · save a card via POST /v1/billing/setup before submitting a job"}
			}
			if sub.MaxUSD <= 0 || sub.MaxUSD > free {
				sub.MaxUSD = free
			}
		}
	}
	var qBind *boundQuote
	var firmQuoteMaxUSD float64
	var slaGuaranteeSecs int  // wave 2A: bound time guarantee (0 = none)
	var slaPremiumUSD float64 // wave 2A: the bound guarantee's premium (the miss remedy)
	if sub.FirmQuote && sub.QuoteID == "" {
		return JobSubmitResponse{}, &httpError{http.StatusBadRequest, "firm_quote requires a quote_id: a firm price commitment needs a quote to be firm about"}
	}
	if sub.QuoteID != "" {
		qid, err := quoteIDToUUID(sub.QuoteID)
		if err != nil {
			return JobSubmitResponse{}, &httpError{http.StatusBadRequest, err.Error()}
		}
		q, err := s.store.GetBindableQuote(ctx, qid, buyerID)
		if errors.Is(err, errNotFound) {
			return JobSubmitResponse{}, &httpError{http.StatusNotFound, "quote not found"}
		}
		if err != nil {
			return JobSubmitResponse{}, &httpError{http.StatusInternalServerError, "loading quote: " + err.Error()}
		}
		if q.Expired {
			return JobSubmitResponse{}, &httpError{http.StatusConflict, "quote expired"}
		}
		if q.JobType != sub.JobType.Type || q.ModelRef != sub.Model.Ref || q.Tier != sub.Tier {
			return JobSubmitResponse{}, &httpError{http.StatusConflict, "quote does not match this submission"}
		}
		if !q.EconomicExecutable || q.EconomicScheduleVersion == "" {
			return JobSubmitResponse{}, &httpError{http.StatusConflict, "quote has no executable economic plan; request a new quote"}
		}
		if err := ValidateEconomicPlanSnapshot(q.EconomicPlan); err != nil {
			return JobSubmitResponse{}, &httpError{http.StatusConflict, "quote economic plan is invalid or was altered: " + err.Error()}
		}
		if q.EconomicPlan.Schedule != economicSchedule || q.EconomicScheduleVersion != economicSchedule.Version {
			return JobSubmitResponse{}, &httpError{http.StatusConflict, "economic schedule changed since quote; request a new quote"}
		}
		if sub.FirmQuote {
			if q.CostMaxUSD <= 0 {
				return JobSubmitResponse{}, &httpError{http.StatusConflict, "quote has no positive cost_max_usd to firm-commit to"}
			}
			firmQuoteMaxUSD = q.EconomicPlan.ReservedBuyerChargeUSD
			if q.SLAGuaranteedSecs > 0 && q.SLAPremiumUSD > 0 {
				slaGuaranteeSecs = q.SLAGuaranteedSecs
				slaPremiumUSD = q.SLAPremiumUSD
				firmQuoteMaxUSD = q.EconomicPlan.ReservedBuyerChargeUSD
			}
		}
		qBind = q
	}

	inputReader, srcKey, err := s.resolveInput(ctx, buyerID, sub.Input)
	if err != nil {
		return JobSubmitResponse{}, &httpError{http.StatusBadRequest, "resolving input: " + err.Error()}
	}
	defer inputReader.Close()

	splitSize := splitSizeOf(sub.Params)
	if splitSize == defaultSplitSize && !hasExplicitSplitSize(sub.Params) {
		sample, rest, serr := peekInputSample(inputReader, inputSampleBytes)
		if serr != nil {
			return JobSubmitResponse{}, &httpError{http.StatusBadRequest, "reading input: " + serr.Error()}
		}
		inputReader = rest
		avgLineBytes := 0.0
		totalRecords := 0
		if scan := scanJSONL(sample); scan.Records > 0 {
			avgLineBytes = float64(scan.Bytes) / float64(scan.Records)
			if len(sample) < inputSampleBytes {
				totalRecords = scan.Records
			}
		}
		splitSize = adaptiveSplitSize(sub.JobType.Type, sub.Params, avgLineBytes)
		splitSize = s.adaptiveSplitSizeLive(ctx, sub.JobType.Type, sub.Model.Ref,
			sub.Constraints.MinMemoryGB, sub.JobType.MaxTokens, avgLineBytes, splitSize, totalRecords)
	}

	jobID := uuid.New()
	if sub.IdempotencyKey != "" {
		jobID = uuid.NewSHA1(buyerID, []byte(sub.IdempotencyKey))
	}
	inputKey := srcKey
	var canonicalWriter io.Writer
	var canonicalPut *streamingPut
	if inputKey == "" {
		inputKey = fmt.Sprintf("jobs/%s/input.jsonl", jobID)
		canonicalPut = newStreamingPut(ctx, s.storage, inputKey, "application/x-ndjson")
		canonicalWriter = canonicalPut.writer
	}

	tasks, _, totalRecords, exactInputBytes, sum256, serr := s.streamSplitAndUpload(ctx, jobID, inputReader, splitSize, canonicalWriter)
	if canonicalPut != nil {
		canonicalPut.writer.Close() // signal EOF to the tee goroutine regardless of serr
		if perr := canonicalPut.wait(); perr != nil && serr == nil {
			serr = perr
		}
	}
	if serr != nil {
		return JobSubmitResponse{}, &httpError{http.StatusInternalServerError, "splitting/uploading input: " + serr.Error()}
	}
	if s.canary.Enabled && int64(exactInputBytes) > s.canary.MaxInputBytes {
		return JobSubmitResponse{}, &httpError{http.StatusRequestEntityTooLarge,
			fmt.Sprintf("private-canary input limit is %d bytes", s.canary.MaxInputBytes)}
	}
	if len(tasks) == 0 {
		return JobSubmitResponse{}, &httpError{http.StatusBadRequest, "input is empty: at least one JSONL line is required"}
	}
	nPrimary := len(tasks) // primaries precede any redundancy/honeypot clones
	inputSHA256 := hex.EncodeToString(sum256[:])
	var boundQuoteID uuid.UUID
	if qBind != nil {
		if qBind.InputSHA256 != "" && qBind.InputSHA256 != inputSHA256 {
			return JobSubmitResponse{}, &httpError{http.StatusConflict, "quote does not match this submission: input changed since the quote"}
		}
		boundQuoteID = qBind.ID
	}

	outputKey := fmt.Sprintf("jobs/%s/output.jsonl", jobID)

	nRedundancy := fracCount(nPrimary, sub.Verification.RedundancyFrac)
	redundancyPeers := append([]taskRow(nil), tasks[:nPrimary]...)
	sort.Slice(redundancyPeers, func(i, j int) bool {
		return redundancySelectionHash(jobID, redundancyPeers[i].ID) < redundancySelectionHash(jobID, redundancyPeers[j].ID)
	})
	for i := 0; i < nRedundancy; i++ {
		p := redundancyPeers[i]
		taskID := uuid.New()
		tasks = append(tasks, taskRow{
			ID:                    taskID,
			JobID:                 jobID,
			IsRedundancy:          true,
			InputRef:              p.InputRef,
			ResultKey:             taskAttemptResultKey(jobID, taskID, 0),
			ChunkIndex:            p.ChunkIndex,
			ExpectedOutputRecords: p.ExpectedOutputRecords,
		})
	}

	nHoneypot := fracCount(nPrimary, sub.Verification.HoneypotFrac)
	if (wantVerificationFloor || s.canary.Enabled) && nHoneypot == 0 {
		nHoneypot = 1
	}
	if nHoneypot > 0 {
		hps, herr := s.store.AvailableSeedHoneypots(ctx, sub.JobType.Type, sub.Model.Ref, sub.JobType.MaxTokens, nHoneypot)
		if herr != nil {
			return JobSubmitResponse{}, &httpError{http.StatusInternalServerError, "loading honeypots: " + herr.Error()}
		}
		for i, hp := range hps {
			taskID := uuid.New()
			opaqueKey := fmt.Sprintf("jobs/%s/tasks/%s/input.jsonl", jobID, taskID)
			inputBytes, gerr := s.storage.GetObject(ctx, hp.InputRef)
			if gerr != nil {
				log.Printf("createJob: honeypot input %q unreadable, skipping this probe: %v", hp.InputRef, gerr)
				continue
			}
			expectedRecords := countNonBlankJSONLRecords(inputBytes)
			if expectedRecords == 0 {
				log.Printf("createJob: honeypot input %q has no records, skipping this probe", hp.InputRef)
				continue
			}
			if perr := s.storage.PutObject(ctx, opaqueKey, inputBytes, "application/x-ndjson"); perr != nil {
				return JobSubmitResponse{}, &httpError{http.StatusInternalServerError, "copying honeypot input to opaque key: " + perr.Error()}
			}
			if aerr := s.store.RegisterHoneypotAlias(ctx, sub.JobType.Type, opaqueKey, hp.KnownAnswer, hp.AnswerClass); aerr != nil {
				return JobSubmitResponse{}, &httpError{http.StatusInternalServerError, "registering honeypot alias: " + aerr.Error()}
			}
			tasks = append(tasks, taskRow{
				ID:                    taskID,
				JobID:                 jobID,
				IsHoneypot:            true,
				InputRef:              opaqueKey,
				ResultKey:             taskAttemptResultKey(jobID, taskID, 0),
				ChunkIndex:            i % nPrimary,
				ExpectedOutputRecords: int64(expectedRecords),
			})
		}
	}
	if s.canary.Enabled {
		actualHoneypots := 0
		for _, task := range tasks {
			if task.IsHoneypot {
				actualHoneypots++
			}
		}
		if actualHoneypots == 0 {
			return JobSubmitResponse{}, &httpError{http.StatusServiceUnavailable,
				"private-canary verification floor unavailable: no usable honeypot"}
		}
	}
	if s.canary.Enabled && len(tasks) > s.canary.MaxTasksPerJob {
		return JobSubmitResponse{}, &httpError{http.StatusRequestEntityTooLarge,
			fmt.Sprintf("private-canary task limit is %d after verification expansion", s.canary.MaxTasksPerJob)}
	}

	basePrimaryCompute := s.estimateJobUSD(ctx, sub.JobType.Type, sub.Model.Ref, exactInputBytes, totalRecords, sub.JobType.MaxTokens, sub.Tier)
	baseComputeUSD := basePrimaryCompute
	if nPrimary > 0 && len(tasks) > nPrimary {
		baseComputeUSD = roundEconomicUSD(basePrimaryCompute * float64(len(tasks)) / float64(nPrimary))
	}
	quoteComparableInput := EconomicPlanInput{
		BaseComputeUSD:   baseComputeUSD,
		InitialTaskCount: len(tasks),
		ExtraTaskReserve: economicExtraTaskReserve(nPrimary),
		SupplierShare:    supplierShareRate,
		SLAPremiumUSD:    slaPremiumUSD,
	}
	if qBind != nil {
		quoteComparableInput.SLAPremiumUSD = qBind.EconomicPlan.Input.SLAPremiumUSD
		quoteComparable := BuildEconomicPlan(quoteComparableInput, economicSchedule)
		if !EconomicPlansEqual(quoteComparable, qBind.EconomicPlan) {
			return JobSubmitResponse{}, &httpError{http.StatusConflict,
				"quote economics no longer match exact submit shape; request a new quote"}
		}
	}
	executionInput := quoteComparableInput
	executionInput.SLAPremiumUSD = slaPremiumUSD
	executionInput.FirmQuoteMaxUSD = firmQuoteMaxUSD
	economicPlan := BuildEconomicPlan(executionInput, economicSchedule)
	if !economicPlan.Executable {
		return JobSubmitResponse{}, &httpError{http.StatusConflict, "job is not economically executable: " + economicPlan.BlockReason}
	}
	estimate := economicPlan.InitialBuyerChargeUSD
	vp, _ := json.Marshal(sub.Verification)
	spec, _ := json.Marshal(sub.JobType)
	offeredRate := s.offeredRateUsdHrForSubmission(ctx, sub)
	effectiveMinMem := sub.Constraints.MinMemoryGB
	if m, merr := s.store.GetModel(ctx, sub.Model.Ref); merr == nil {
		if m.MinMemoryGB > effectiveMinMem {
			effectiveMinMem = m.MinMemoryGB
		}
	}
	etaSecs, _, _ := s.etaBandSecs(
		ctx, sub.JobType.Type, sub.Model.Ref, effectiveMinMem, len(tasks),
	)

	var webhookRegistration WebhookRegistration
	var webhookSecretSealed string
	if sub.WebhookURL != "" {
		secret, sealed, err := newWebhookSigningSecret()
		if err != nil {
			status := http.StatusInternalServerError
			if errors.Is(err, errWebhookSigningKeyUnavailable) {
				status = http.StatusServiceUnavailable
			}
			return JobSubmitResponse{}, &httpError{status, "registering webhook: signing secret unavailable"}
		}
		webhookRegistration = WebhookRegistration{ID: uuid.New(), Secret: secret}
		webhookSecretSealed = sealed
	}

	jr := &jobRow{
		ID:                         jobID,
		BuyerID:                    buyerID,
		JobType:                    sub.JobType.Type,
		ModelRef:                   sub.Model.Ref,
		InputRef:                   inputKey,
		OutputRef:                  outputKey,
		Tier:                       sub.Tier,
		VerificationPolicy:         vp,
		EstimatedUSD:               estimate,
		TaskCount:                  len(tasks),
		MinMemoryGB:                effectiveMinMem,
		MaxDurationSecs:            sub.Constraints.MaxDurationSecs,
		MinReputation:              sub.MinReputation,
		HWClasses:                  sub.Constraints.HWClasses,
		DataResidency:              sub.Constraints.DataResidency,
		JobTypeSpec:                spec,
		SplitSize:                  splitSize,
		OfferedRateUsdHr:           offeredRate,
		ETASecs:                    etaSecs,
		MaxUSD:                     sub.MaxUSD,   // Budget Governor cap (0 = none -> persisted NULL)
		QuoteID:                    boundQuoteID, // D7 quote binding (zero = none -> persisted NULL)
		DeadlineSecs:               sub.DeadlineSecs,
		FirmQuote:                  sub.FirmQuote,
		FirmQuoteMaxUSD:            firmQuoteMaxUSD,  // the real charge ceiling (0 = not firm -> persisted NULL)
		SLAGuaranteeSecs:           slaGuaranteeSecs, // wave 2A time guarantee (0 = none -> persisted NULL)
		SLAPremiumUSD:              slaPremiumUSD,    // wave 2A premium = the miss remedy (0 = none -> NULL)
		EconomicInputRecords:       int64(totalRecords),
		EconomicInputBytes:         int64(exactInputBytes),
		EconomicInputSource:        economicInputSourceSubmitStream,
		EconomicPlan:               economicPlan,
		WebhookID:                  webhookRegistration.ID,
		WebhookURL:                 sub.WebhookURL,
		WebhookSigningSecretSealed: webhookSecretSealed,
		SubmitIdempotencyKey:       sub.IdempotencyKey,
		SubmitRequestSHA256:        sub.RequestSHA256,
	}
	if err := s.store.SubmitJobTx(ctx, jr, tasks); err != nil {
		if sub.IdempotencyKey != "" && isUniqueViolation(err) {
			if replay, found, replayErr := s.store.JobSubmissionReplay(ctx, buyerID, sub.IdempotencyKey, sub.RequestSHA256); replayErr == nil && found {
				replay.IdempotentReplay = true
				return replay, nil
			} else if errors.Is(replayErr, errIdempotencyConflict) {
				return JobSubmitResponse{}, &httpError{http.StatusConflict, replayErr.Error()}
			}
		}
		return JobSubmitResponse{}, &httpError{http.StatusInternalServerError, "failed to create job: " + err.Error()}
	}

	metrics.jobsSubmitted.Add(1)

	_ = s.store.InsertJobEvent(ctx, jobID, nil, "job_created",
		fmt.Sprintf("Job created: %d tasks, model %s, %s tier", len(tasks), jr.ModelRef, sub.Tier), nil)

	if sub.MaxUSD > 0 {
		_ = s.store.InsertJobEvent(ctx, jobID, nil, "budget_set",
			fmt.Sprintf("budget set: $%.2f cap", sub.MaxUSD), nil)
	}

	if boundQuoteID != (uuid.UUID{}) {
		_ = s.store.InsertJobEvent(ctx, jobID, nil, "quote_bound",
			fmt.Sprintf("bound to quote q_%s", boundQuoteID), nil)
	}

	if slaGuaranteeSecs > 0 {
		_ = s.store.InsertJobEvent(ctx, jobID, nil, "sla_bound",
			fmt.Sprintf("Speed SLA bound: results guaranteed within %ds of submission · premium $%.6f is refunded automatically on a miss", slaGuaranteeSecs, slaPremiumUSD), nil)
	}

	dur := time.Duration(etaSecs) * time.Second
	if min := tierMinCompletion(sub.Tier); dur < min {
		dur = min
	}
	response := JobSubmitResponse{
		JobID:               jobID,
		TaskCount:           len(tasks),
		EstimatedUSD:        estimate,
		ETASecs:             etaSecs,
		EstimatedCompletion: time.Now().Add(dur).UTC().Format(time.RFC3339),
		TierSemantics:       serviceTierSemantics(sub.Tier),
	}
	if webhookRegistration.ID != uuid.Nil {
		response.WebhookID = webhookRegistration.ID.String()
		response.WebhookSecret = webhookRegistration.Secret
	}
	return response, nil
}

var jobsKeyPattern = regexp.MustCompile(`^jobs/([0-9a-fA-F-]{36})/`)

func (s *Server) resolveInput(ctx context.Context, buyerID uuid.UUID, raw json.RawMessage) (r io.ReadCloser, fromKey string, err error) {
	raw = bytes.TrimSpace(raw)
	if len(raw) == 0 || string(raw) == "null" {
		return nil, "", errors.New("input is required (inline JSONL string or {\"s3_key\":\"...\"})")
	}
	if raw[0] == '"' {
		var inline string
		if err := json.Unmarshal(raw, &inline); err != nil {
			return nil, "", fmt.Errorf("invalid inline input string: %w", err)
		}
		return io.NopCloser(strings.NewReader(inline)), "", nil
	}
	var ref struct {
		S3Key string `json:"s3_key"`
	}
	if err := json.Unmarshal(raw, &ref); err != nil || ref.S3Key == "" {
		return nil, "", errors.New("input must be a JSONL string or an object with a non-empty s3_key")
	}
	m := jobsKeyPattern.FindStringSubmatch(ref.S3Key)
	if m == nil {
		return nil, "", errors.New("s3_key must reference an object under a job you submitted (jobs/<job_id>/...)")
	}
	refJobID, perr := uuid.Parse(m[1])
	if perr != nil {
		return nil, "", errors.New("s3_key contains an invalid job id")
	}
	ownerID, oerr := s.store.JobBuyerID(ctx, refJobID)
	if oerr != nil || ownerID != buyerID {
		return nil, "", errors.New("s3_key does not reference a job you submitted")
	}
	rc, err := s.storage.GetObjectReader(ctx, ref.S3Key)
	if err != nil {
		return nil, "", fmt.Errorf("fetching input %q: %w", ref.S3Key, err)
	}
	return rc, ref.S3Key, nil
}

func (s *Server) handleGetJob(w http.ResponseWriter, r *http.Request) {
	auth := r.Context().Value(ctxBuyer).(*AuthResult)
	id, ok := pathUUID(w, r)
	if !ok {
		return
	}
	j, err := s.store.GetJob(r.Context(), id, auth.BuyerID)
	if errors.Is(err, errNotFound) {
		writeErr(w, http.StatusNotFound, "job not found")
		return
	}
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, JobStatus{
		JobID:            j.ID,
		Status:           j.Status,
		JobType:          j.JobType,
		Tier:             j.Tier,
		TaskCount:        j.TaskCount,
		TasksDone:        j.TasksDone,
		EstimatedUSD:     j.EstimatedUSD,
		ActualUSD:        j.ActualUSD,
		ETASecs:          j.ETASecs,
		CreatedAt:        j.CreatedAt.UTC().Format(time.RFC3339),
		MaxUSD:           j.MaxUSD,
		BudgetState:      j.BudgetState,
		ChargeStatus:     j.ChargeStatus,
		Verification:     j.Verification,
		SLAGuaranteeSecs: j.SLAGuaranteeSecs, // wave 2A: the bound guarantee (0 = none, omitted)
		SLAPremiumUSD:    j.SLAPremiumUSD,    // wave 2A: its premium (the miss remedy)
		SLAMet:           j.SLAMet,           // wave 2A: outcome (absent until decided)
	})
}

func (s *Server) handleJobResults(w http.ResponseWriter, r *http.Request) {
	auth := r.Context().Value(ctxBuyer).(*AuthResult)
	ctx := r.Context()
	id, ok := pathUUID(w, r)
	if !ok {
		return
	}
	j, err := s.store.GetJob(ctx, id, auth.BuyerID)
	if errors.Is(err, errNotFound) {
		writeErr(w, http.StatusNotFound, "job not found")
		return
	}
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	if j.Status != "complete" {
		writeErr(w, http.StatusConflict, "job not complete (status="+j.Status+")")
		return
	}

	keys, err := s.store.JobResultKeys(ctx, j.ID)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "loading result keys: "+err.Error())
		return
	}
	urls := make([]string, 0, len(keys))
	for _, k := range keys {
		u, perr := s.storage.PresignGet(ctx, k, time.Hour)
		if perr != nil {
			writeErr(w, http.StatusInternalServerError, "presign result: "+perr.Error())
			return
		}
		urls = append(urls, u)
	}

	res := JobResults{JobID: j.ID, Status: j.Status, ResultURLs: urls}
	if j.OutputRef != "" {
		if j.ResultsMergedAt == nil {
			if _, merr := s.MergeJobResults(ctx, j.ID); merr != nil {
				writeErr(w, http.StatusInternalServerError, "merging results: "+merr.Error())
				return
			}
		}
		if u, perr := s.storage.PresignGet(ctx, j.OutputRef, time.Hour); perr == nil {
			res.ResultsURL = u
		}
	}
	writeJSON(w, http.StatusOK, res)
}

func (s *Server) MergeJobResults(ctx context.Context, jobID uuid.UUID) (int, error) {
	return mergeJobResults(ctx, s.store, s.storage, jobID)
}

var embedBinMagic = []byte("CXEM")

const embedBinHeaderLen = 16

const embedBinVersion = uint32(1)

func isEmbedBinary(obj []byte) bool {
	return len(obj) >= 4 && bytes.Equal(obj[:4], embedBinMagic)
}

func mergeJobResults(ctx context.Context, store *Store, storage *Storage, jobID uuid.UUID) (int, error) {
	return mergeJobResultsWithProbe(ctx, store, storage, jobID, nil)
}

func mergeJobResultsWithProbe(
	ctx context.Context,
	store *Store,
	storage *Storage,
	jobID uuid.UUID,
	probe recoveryBoundaryProbe,
) (int, error) {
	if store == nil || store.verificationResources == nil {
		return 0, errors.New("merge: verification resource budget is unavailable")
	}
	budgetCtx, releaseMemory := withVerificationMemoryTracker(ctx, store.verificationResources.bytes)
	defer releaseMemory()
	ctx = budgetCtx

	info, err := store.JobMergeInputs(ctx, jobID)
	if err != nil {
		return 0, err
	}
	if info.OutputRef == "" {
		return 0, fmt.Errorf("job %s has no output_ref to merge into", jobID)
	}

	tmp, err := os.CreateTemp("", "computexchange-result-merge-*.tmp")
	if err != nil {
		return 0, fmt.Errorf("merge: create temporary output: %w", err)
	}
	tmpName := tmp.Name()
	defer func() {
		_ = tmp.Close()
		_ = os.Remove(tmpName)
	}()

	contentType := "application/x-ndjson"
	var outputRecords int64
	if len(info.Results) > 0 {
		firstMark := verificationMemoryMark(ctx)
		first, ferr := readMergeResult(ctx, storage, info.Results[0])
		if ferr != nil {
			return 0, ferr
		}
		if info.JobType == "embed" && isEmbedBinary(first) {
			contentType = "application/octet-stream"
			outputRecords, err = mergeEmbedBinaryToFile(ctx, storage, tmp, info.Results, first, firstMark)
		} else {
			outputRecords, err = mergeJSONResultsToFile(ctx, storage, tmp, info.JobType, info.Results, first, firstMark)
		}
		if err != nil {
			return 0, err
		}
	}
	outputBytes, err := tmp.Seek(0, io.SeekEnd)
	if err != nil {
		return 0, fmt.Errorf("merge: size temporary output: %w", err)
	}
	if uint64(outputBytes) > uint64(^uint(0)>>1) {
		return 0, fmt.Errorf("merge: output %d bytes cannot be represented on this platform", outputBytes)
	}
	reachRecoveryBoundary(ctx, probe, BoundaryMergeBeforePut)
	if err := storage.PutObjectReadSeeker(ctx, info.OutputRef, tmp, outputBytes, contentType); err != nil {
		return 0, fmt.Errorf("merge: writing output %q: %w", info.OutputRef, err)
	}
	reachRecoveryBoundary(ctx, probe, BoundaryMergeAfterPut)
	if err := verifyMergedObject(ctx, storage, info.OutputRef, tmp, outputBytes); err != nil {
		return 0, err
	}
	reachRecoveryBoundary(ctx, probe, BoundaryMergeAfterVerify)
	metrics.resultMerges.Add(1)
	reachRecoveryBoundary(ctx, probe, BoundaryMergeBeforePublish)
	if err := store.MarkResultsMerged(ctx, jobID, outputRecords, outputBytes); err != nil {
		return int(outputBytes), fmt.Errorf("merge: writing output %q succeeded but marking results_merged_at failed: %w", info.OutputRef, err)
	}
	reachRecoveryBoundary(ctx, probe, BoundaryMergeAfterPublish)
	return int(outputBytes), nil
}

func verifyMergedObject(
	ctx context.Context,
	storage *Storage,
	key string,
	expected io.ReadSeeker,
	expectedBytes int64,
) error {
	if _, err := expected.Seek(0, io.SeekStart); err != nil {
		return fmt.Errorf("merge: rewind expected output %q for verification: %w", key, err)
	}
	expectedHash := sha256.New()
	n, err := io.Copy(expectedHash, expected)
	if err != nil {
		return fmt.Errorf("merge: hash expected output %q: %w", key, err)
	}
	if n != expectedBytes {
		return fmt.Errorf("merge: expected output %q changed size on disk: assembled=%d hashed=%d", key, expectedBytes, n)
	}

	actual, err := storage.GetObjectReader(ctx, key)
	if err != nil {
		return fmt.Errorf("merge: reading back output %q: %w", key, err)
	}
	defer actual.Close()
	actualHash := sha256.New()
	actualBytes, err := io.Copy(actualHash, actual)
	if err != nil {
		return fmt.Errorf("merge: reading back output %q: %w", key, err)
	}
	if actualBytes != expectedBytes || !bytes.Equal(actualHash.Sum(nil), expectedHash.Sum(nil)) {
		return fmt.Errorf(
			"merge: output %q failed readback verification: expected_bytes=%d actual_bytes=%d expected_sha256=%s actual_sha256=%s",
			key, expectedBytes, actualBytes,
			hex.EncodeToString(expectedHash.Sum(nil)), hex.EncodeToString(actualHash.Sum(nil)),
		)
	}
	return nil
}

func readMergeResult(ctx context.Context, storage *Storage, pr PrimaryResult) ([]byte, error) {
	var (
		obj []byte
		err error
	)
	if pr.Artifact != nil {
		if pr.Artifact.Key != pr.ResultRef {
			return nil, fmt.Errorf("merge: chunk %d resolved key %q disagrees with authority key %q", pr.ChunkIndex, pr.ResultRef, pr.Artifact.Key)
		}
		obj, err = storage.ReadSealedVerificationArtifact(ctx, *pr.Artifact)
	} else {
		obj, err = storage.readVerificationObjectBounded(ctx, pr.ResultRef, verificationArtifactAbsoluteMaxBytes, nil)
	}
	if err != nil {
		return nil, fmt.Errorf("merge: fetching result %q: %w", pr.ResultRef, err)
	}
	return obj, nil
}

func mergeJSONResultsToFile(ctx context.Context, storage *Storage, out io.Writer, jobType string, results []PrimaryResult, first []byte, firstMark int64) (int64, error) {
	var total int64
	for i, pr := range results {
		mark := verificationMemoryMark(ctx)
		obj := first
		if i == 0 {
			mark = firstMark
		} else {
			var err error
			obj, err = readMergeResult(ctx, storage, pr)
			if err != nil {
				return 0, err
			}
		}
		if uint64(total) > uint64(^uint(0)>>1) {
			return 0, fmt.Errorf("merge: record index %d cannot be represented on this platform", total)
		}
		n, err := mergeResultObjectTo(out, jobType, obj, int(total))
		if err != nil {
			return 0, fmt.Errorf("merge: chunk %d (%s): %w", pr.ChunkIndex, pr.ResultRef, err)
		}
		total += int64(n)
		obj = nil
		releaseVerificationMemoryToMark(ctx, mark)
	}
	return total, nil
}

func mergeEmbedBinaryToFile(ctx context.Context, storage *Storage, out io.WriteSeeker, results []PrimaryResult, first []byte, firstMark int64) (int64, error) {
	if n, err := out.Write(make([]byte, embedBinHeaderLen)); err != nil || n != embedBinHeaderLen {
		if err == nil {
			err = io.ErrShortWrite
		}
		return 0, fmt.Errorf("merge: write binary header placeholder: %w", err)
	}
	var dim uint32
	var total uint64
	for i, pr := range results {
		mark := verificationMemoryMark(ctx)
		obj := first
		if i == 0 {
			mark = firstMark
		} else {
			var err error
			obj, err = readMergeResult(ctx, storage, pr)
			if err != nil {
				return 0, err
			}
		}
		cdim, count, body, err := validateEmbedBinaryChunk(obj, i, pr.ResultRef, dim)
		if err != nil {
			return 0, err
		}
		if i == 0 {
			dim = cdim
		}
		if total > math.MaxUint32-uint64(count) {
			return 0, fmt.Errorf("merge: more than %d embedding rows exceeds the binary format's uint32 count", uint64(math.MaxUint32))
		}
		total += uint64(count)
		if n, err := out.Write(body); err != nil || n != len(body) {
			if err == nil {
				err = io.ErrShortWrite
			}
			return 0, fmt.Errorf("merge: write binary chunk %d: %w", pr.ChunkIndex, err)
		}
		obj = nil
		releaseVerificationMemoryToMark(ctx, mark)
	}
	if _, err := out.Seek(0, io.SeekStart); err != nil {
		return 0, fmt.Errorf("merge: seek binary header: %w", err)
	}
	var header [embedBinHeaderLen]byte
	copy(header[:4], embedBinMagic)
	binary.LittleEndian.PutUint32(header[4:8], embedBinVersion)
	binary.LittleEndian.PutUint32(header[8:12], dim)
	binary.LittleEndian.PutUint32(header[12:16], uint32(total))
	if n, err := out.Write(header[:]); err != nil || n != len(header) {
		if err == nil {
			err = io.ErrShortWrite
		}
		return 0, fmt.Errorf("merge: finalize binary header: %w", err)
	}
	return int64(total), nil
}

func validateEmbedBinaryChunk(obj []byte, index int, ref string, expectedDim uint32) (dim, count uint32, body []byte, err error) {
	envelope, parseErr := parseEmbeddingBinaryEnvelope(obj, 0)
	if parseErr != nil {
		return 0, 0, nil, fmt.Errorf("merge: chunk %d (%s): %w", index, ref, parseErr)
	}
	dim, count, body = envelope.Dim, envelope.Count, envelope.Body
	if index > 0 && dim != expectedDim {
		return 0, 0, nil, fmt.Errorf("merge: chunk %d (%s): dim %d != job dim %d (cannot merge embeddings of different width)", index, ref, dim, expectedDim)
	}
	return dim, count, body, nil
}

func mergeEmbedBinary(objs [][]byte, results []PrimaryResult) ([]byte, error) {
	if len(objs) == 0 {
		return nil, fmt.Errorf("merge: binary embedding job has no chunk artifacts")
	}
	var dim uint32
	var total uint64
	bodies := make([][]byte, 0, len(objs))
	for i, obj := range objs {
		ref := ""
		if i < len(results) {
			ref = results[i].ResultRef
		}
		envelope, err := parseEmbeddingBinaryEnvelope(obj, 0)
		if err != nil {
			return nil, fmt.Errorf("merge: chunk %d (%s): %w", i, ref, err)
		}
		cdim, ccount := envelope.Dim, envelope.Count
		if i == 0 {
			dim = cdim
		} else if cdim != dim {
			return nil, fmt.Errorf("merge: chunk %d (%s): dim %d != job dim %d (cannot merge embeddings of different width)", i, ref, cdim, dim)
		}
		var ok bool
		total, ok = checkedAddUint64(total, uint64(ccount))
		if !ok || total > math.MaxUint32 {
			return nil, fmt.Errorf("merge: total embedding row count exceeds the binary format's uint32 count")
		}
		bodies = append(bodies, envelope.Body)
	}

	elements, ok := checkedMulUint64(uint64(dim), total)
	if !ok {
		return nil, fmt.Errorf("merge: combined embedding element count overflows uint64")
	}
	bodyBytes, ok := checkedMulUint64(elements, 4)
	if !ok {
		return nil, fmt.Errorf("merge: combined embedding byte count overflows uint64")
	}
	outputBytes, ok := checkedAddUint64(embedBinHeaderLen, bodyBytes)
	if !ok || outputBytes > uint64(verificationArtifactAbsoluteMaxBytes) || outputBytes > uint64(^uint(0)>>1) {
		return nil, fmt.Errorf("merge: combined binary embedding artifact is %d bytes, above the safe in-memory merge bound %d",
			outputBytes, verificationArtifactAbsoluteMaxBytes)
	}

	out := make([]byte, int(outputBytes))
	copy(out[:4], embedBinMagic)
	binary.LittleEndian.PutUint32(out[4:8], embedBinVersion)
	binary.LittleEndian.PutUint32(out[8:12], dim)
	binary.LittleEndian.PutUint32(out[12:16], uint32(total))
	offset := embedBinHeaderLen
	for _, b := range bodies {
		offset += copy(out[offset:], b)
	}
	if offset != len(out) {
		return nil, fmt.Errorf("merge: copied %d bytes, expected %d", offset, len(out))
	}
	return out, nil
}

func mergeResultObjectTo(out io.Writer, jobType string, obj []byte, base int) (int, error) {
	writeBytes := func(b []byte) error {
		n, err := out.Write(b)
		if err == nil && n != len(b) {
			return io.ErrShortWrite
		}
		return err
	}
	writeLine := func(v any) error {
		b, err := json.Marshal(v)
		if err != nil {
			return err
		}
		if err := writeBytes(b); err != nil {
			return err
		}
		return writeBytes([]byte{'\n'})
	}
	switch jobType {
	case "embed":
		var r struct {
			Vectors [][]float64 `json:"vectors"`
		}
		if err := json.Unmarshal(obj, &r); err != nil || len(r.Vectors) == 0 {
			return 0, fmt.Errorf("not a valid embed result")
		}
		for i, v := range r.Vectors {
			if err := writeLine(map[string]any{"index": base + i, "vector": v}); err != nil {
				return 0, err
			}
		}
		return len(r.Vectors), nil
	case "batch_infer":
		var r struct {
			Completions []json.RawMessage `json:"completions"`
		}
		if err := json.Unmarshal(obj, &r); err == nil {
			if len(r.Completions) > 0 {
				for _, c := range r.Completions {
					if err := writeBytes(bytes.TrimSpace(c)); err != nil {
						return 0, err
					}
					if err := writeBytes([]byte{'\n'}); err != nil {
						return 0, err
					}
				}
				return len(r.Completions), nil
			}
		}
		if err := writeBytes(bytes.TrimSpace(obj)); err != nil {
			return 0, err
		}
		if err := writeBytes([]byte{'\n'}); err != nil {
			return 0, err
		}
		return 1, nil
	default:
		return 0, fmt.Errorf("unsupported job type %q", jobType)
	}
}

func (s *Server) handleCancelJob(w http.ResponseWriter, r *http.Request) {
	auth := r.Context().Value(ctxBuyer).(*AuthResult)
	id, ok := pathUUID(w, r)
	if !ok {
		return
	}
	err := s.store.CancelJob(r.Context(), id, auth.BuyerID)
	if errors.Is(err, errNotFound) {
		writeErr(w, http.StatusNotFound, "job not found")
		return
	}
	if errors.Is(err, errJobNotCancellable) {
		writeErr(w, http.StatusConflict, "job already started or has pending verification work")
		return
	}
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "cancelled"})
}

func (s *Server) handleModels(w http.ResponseWriter, r *http.Request) {
	rows, err := s.store.ListModels(r.Context())
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	out := make([]ModelInfo, 0, len(rows))
	for _, m := range rows {
		if !advertisedRuntimeModel(m.ID) {
			continue
		}
		info := ModelInfo{
			ID:          m.ID,
			Kind:        m.Kind,
			MinMemoryGB: m.MinMemoryGB,
			JobType:     m.JobType,
		}
		info.PricePer1KUSD = modelPrice(m)
		out = append(out, info)
	}
	writeJSON(w, http.StatusOK, out)
}

func (s *Server) handlePriceEstimate(w http.ResponseWriter, r *http.Request) {
	model := r.URL.Query().Get("model")
	tier := r.URL.Query().Get("tier")
	if tier == "" {
		tier = "batch"
	}
	units, err := strconv.ParseUint(r.URL.Query().Get("units"), 10, 64)
	if err != nil {
		writeErr(w, http.StatusBadRequest, "units must be a positive integer")
		return
	}
	if !advertisedRuntimeModel(model) {
		writeErr(w, http.StatusBadRequest, "model is not advertised by the production runtime matrix: "+model)
		return
	}
	m, err := s.store.GetModel(r.Context(), model)
	if errors.Is(err, errNotFound) {
		writeErr(w, http.StatusBadRequest, "unknown model: "+model)
		return
	}
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	price := modelPrice(*m)
	est := float64(units) / 1000.0 * price * tierMultiplier(tier)
	writeJSON(w, http.StatusOK, PriceEstimate{
		Model:         m.ID,
		Units:         units,
		PricePer1KUSD: price,
		EstimateUSD:   roundUSD(est),
		Tier:          tier,
	})
}

func (s *Server) handleRegisterWebhook(w http.ResponseWriter, r *http.Request) {
	auth := r.Context().Value(ctxBuyer).(*AuthResult)
	var body struct {
		URL   string `json:"url"`
		JobID string `json:"job_id"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeErr(w, http.StatusBadRequest, "invalid webhook json")
		return
	}
	body.URL = strings.TrimSpace(body.URL)
	if _, err := validateWebhookURLSyntax(body.URL, false); err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	if strings.TrimSpace(body.JobID) == "" {
		writeErr(w, http.StatusBadRequest, "job_id is required")
		return
	}
	id, perr := uuid.Parse(body.JobID)
	if perr != nil {
		writeErr(w, http.StatusBadRequest, "job_id must be a uuid")
		return
	}
	registration, err := s.store.InsertWebhook(r.Context(), auth.BuyerID, &id, body.URL)
	if errors.Is(err, errNotFound) {
		writeErr(w, http.StatusNotFound, "job not found")
		return
	}
	if errors.Is(err, errWebhookLimit) {
		writeErr(w, http.StatusTooManyRequests, err.Error())
		return
	}
	if errors.Is(err, errWebhookSigningKeyUnavailable) {
		writeErr(w, http.StatusServiceUnavailable,
			"webhook registration unavailable: encrypted signing-secret storage is not configured")
		return
	}
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "registering webhook: "+err.Error())
		return
	}
	setSecretResponseHeaders(w)
	writeJSON(w, http.StatusCreated, map[string]any{
		"status":         "registered",
		"webhook_id":     registration.ID,
		"webhook_secret": registration.Secret,
	})
}

func setSecretResponseHeaders(w http.ResponseWriter) {
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Pragma", "no-cache")
}

func (s *Server) handleFileDispute(w http.ResponseWriter, r *http.Request) {
	auth := r.Context().Value(ctxBuyer).(*AuthResult)
	jobID, err := uuid.Parse(r.PathValue("id"))
	if err != nil {
		writeErr(w, http.StatusBadRequest, "invalid job id")
		return
	}
	var body struct {
		Reason string `json:"reason"`
	}
	raw, err := io.ReadAll(r.Body)
	if err != nil {
		writeErr(w, http.StatusBadRequest, "reading dispute request: "+err.Error())
		return
	}
	if err := decodeStrictJSONObject(raw, &body); err != nil {
		writeErr(w, http.StatusBadRequest, "invalid dispute json: "+err.Error())
		return
	}
	id, err := s.store.RecordDispute(r.Context(), jobID, auth.BuyerID, body.Reason)
	if errors.Is(err, errNotFound) {
		writeErr(w, http.StatusNotFound, "job not found")
		return
	}
	if errors.Is(err, errDisputeReasonRequired) || errors.Is(err, errDisputeReasonTooLong) {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	if errors.Is(err, errDisputeJobNotTerminal) || errors.Is(err, errDisputeAlreadyActive) {
		writeErr(w, http.StatusConflict, err.Error())
		return
	}
	if errors.Is(err, errDisputeWindowClosed) {
		writeErr(w, http.StatusGone, err.Error())
		return
	}
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "recording dispute: "+err.Error())
		return
	}
	writeJSON(w, http.StatusAccepted, map[string]any{
		"dispute_id": id,
		"status":     "open",
		"note":       "Dispute recorded; unreleased supplier credits are frozen pending independent resolution.",
	})
}

func (s *Server) handleWorkerRegister(w http.ResponseWriter, r *http.Request) {
	auth := r.Context().Value(ctxWorker).(*WorkerAuth)
	var cap WorkerCapability
	if err := json.NewDecoder(r.Body).Decode(&cap); err != nil {
		writeErr(w, http.StatusBadRequest, "invalid capability json: "+err.Error())
		return
	}
	if !validHWClasses[cap.HWClass] {
		writeErr(w, http.StatusBadRequest, "invalid hw_class: "+cap.HWClass)
		return
	}
	cap.Engine = normalizeEngine(cap.Engine)
	if !validEngines[cap.Engine] {
		writeErr(w, http.StatusBadRequest, "invalid engine: "+cap.Engine)
		return
	}
	if err := validateWorkerRuntimeProjection(cap); err != nil {
		writeErr(w, http.StatusBadRequest, "runtime capability rejected: "+err.Error())
		return
	}
	if s.canary.Enabled {
		if !s.canary.allowsWorkerRuntime(cap) {
			writeErr(w, http.StatusForbidden, "worker agent version or source-bound build hash is outside the private-canary allowlist")
			return
		}
		allowed, err := s.store.CanaryWorkerAdmissionAllowed(r.Context(), auth.WorkerID, s.canary.MaxActiveWorkers)
		if err != nil {
			writeErr(w, http.StatusServiceUnavailable, "canary worker-admission check unavailable")
			return
		}
		if !allowed {
			writeErr(w, http.StatusTooManyRequests, "private-canary active-worker limit reached")
			return
		}
	}
	cap.WorkerID = auth.WorkerID
	cap.SupplierID = auth.SupplierID
	if err := s.store.UpsertWorker(r.Context(), cap); err != nil {
		writeErr(w, http.StatusInternalServerError, "register failed: "+err.Error())
		return
	}
	writeJSON(w, http.StatusOK, cap)
}

func (s *Server) handleWorkerHeartbeat(w http.ResponseWriter, r *http.Request) {
	auth := r.Context().Value(ctxWorker).(*WorkerAuth)
	var hb Heartbeat
	raw, err := io.ReadAll(io.LimitReader(r.Body, 64<<10+1))
	if err != nil || len(raw) > 64<<10 || decodeStrictJSONObject(raw, &hb) != nil {
		writeErr(w, http.StatusBadRequest, "invalid heartbeat json")
		return
	}
	if hb.WorkerID != uuid.Nil && hb.WorkerID != auth.WorkerID {
		writeErr(w, http.StatusBadRequest, "heartbeat worker_id does not match credential")
		return
	}
	if len(hb.ActiveTasks) > 32 {
		writeErr(w, http.StatusBadRequest, "too many active task leases")
		return
	}
	seen := make(map[uuid.UUID]struct{}, len(hb.ActiveTasks))
	for _, lease := range hb.ActiveTasks {
		if lease.TaskID == uuid.Nil || lease.Attempt < 0 {
			writeErr(w, http.StatusBadRequest, "invalid active task lease")
			return
		}
		if _, duplicate := seen[lease.TaskID]; duplicate {
			writeErr(w, http.StatusBadRequest, "duplicate active task lease")
			return
		}
		seen[lease.TaskID] = struct{}{}
	}
	if err := validateHeartbeatRuntimeModels(hb.LoadedModels); err != nil {
		writeErr(w, http.StatusBadRequest, "runtime heartbeat rejected: "+err.Error())
		return
	}
	if err := s.store.HeartbeatTx(r.Context(), auth.WorkerID, WorkerResources{
		AvailableMemoryGB:  hb.AvailableMemoryGB,
		EffectiveMemoryGB:  hb.EffectiveMemoryGB,
		ReservedHeadroomGB: hb.ReservedHeadroomGB,
		Throttled:          hb.Throttled,
		LoadedModels:       hb.LoadedModels,
		ActiveTasks:        hb.ActiveTasks,
	}); err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

const longPollCap = 25 * time.Second

const longPollInterval = 5 * time.Second

const taskAttemptHeaderName = "X-Task-Attempt"

func taskAttemptHeader(r *http.Request) (int16, error) {
	raw := strings.TrimSpace(r.Header.Get(taskAttemptHeaderName))
	if raw == "" {
		return 0, fmt.Errorf("%s is required", taskAttemptHeaderName)
	}
	n, err := strconv.ParseInt(raw, 10, 16)
	if err != nil || n < 0 {
		return 0, fmt.Errorf("%s must be a non-negative 16-bit integer", taskAttemptHeaderName)
	}
	return int16(n), nil
}

func parseWaitMs(r *http.Request) time.Duration {
	raw := r.URL.Query().Get("wait_ms")
	if raw == "" {
		return 0
	}
	n, err := strconv.Atoi(raw)
	if err != nil || n <= 0 {
		return 0
	}
	d := time.Duration(n) * time.Millisecond
	if d > longPollCap {
		d = longPollCap
	}
	return d
}

func (s *Server) claimWithWait(ctx context.Context, auth WorkerAuth, wait time.Duration) (*ClaimedTask, error) {
	paused, err := s.store.OperationalControlPaused(ctx, controlDispatch)
	if err != nil || paused {
		return nil, err
	}
	c, err := s.store.ClaimTasksTx(ctx, auth)
	if err != nil || c != nil || wait <= 0 {
		return c, err
	}

	deadline := time.NewTimer(wait)
	defer deadline.Stop()
	tick := time.NewTicker(longPollInterval)
	defer tick.Stop()

	for {
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-deadline.C:
			metrics.longPollTimeouts.Add(1)
			return nil, nil
		case <-taskWake.Wait():
			paused, err := s.store.OperationalControlPaused(ctx, controlDispatch)
			if err != nil || paused {
				return nil, err
			}
			c, err := s.store.ClaimTasksTx(ctx, auth)
			if err != nil || c != nil {
				return c, err
			}
		case <-tick.C:
			paused, err := s.store.OperationalControlPaused(ctx, controlDispatch)
			if err != nil || paused {
				return nil, err
			}
			c, err := s.store.ClaimTasksTx(ctx, auth)
			if err != nil || c != nil {
				return c, err
			}
		}
	}
}

var checkpointableJobTypes = map[string]bool{"batch_infer": true}

func claimedTaskConstraints(c *ClaimedTask) JobConstraints {
	return JobConstraints{
		MinMemoryGB:     c.MinMemoryGB,
		HWClasses:       append([]string(nil), c.HWClasses...),
		MaxDurationSecs: c.MaxDurationSecs,
		DataResidency:   append([]string(nil), c.DataResidency...),
	}
}

type taskResultPutPresigner interface {
	PresignPut(context.Context, string, time.Duration) (string, error)
}

func presignTaskAttemptResult(ctx context.Context, presigner taskResultPutPresigner, c *ClaimedTask) (string, error) {
	if c == nil {
		return "", errors.New("cannot presign result for a nil task")
	}
	if err := validateTaskAttemptResultKey(c.JobID, c.TaskID, c.Attempt, c.ResultKey); err != nil {
		return "", err
	}
	return presigner.PresignPut(ctx, c.ResultKey, time.Hour)
}

func (s *Server) handleWorkerPoll(w http.ResponseWriter, r *http.Request) {
	auth := r.Context().Value(ctxWorker).(*WorkerAuth)
	ctx := r.Context()
	c, err := s.claimWithWait(ctx, *auth, parseWaitMs(r))
	if errors.Is(err, errNotFound) {
		writeErr(w, http.StatusForbidden, "worker not registered  -  call /v1/worker/register first")
		return
	}
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return
	}
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	if c == nil {
		w.WriteHeader(http.StatusNoContent) // no work (immediately, or after the long-poll wait elapsed)
		return
	}

	inputURL, err := s.storage.PresignGet(ctx, c.InputRef, 15*time.Minute)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "presign input: "+err.Error())
		return
	}
	outputURL, err := presignTaskAttemptResult(ctx, s.storage, c)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "presign result: "+err.Error())
		return
	}
	partialPutURL := ""
	if checkpointableJobTypes[c.JobType] {
		if u, perr := s.storage.PresignPut(ctx, c.ResultKey+".partial", time.Hour); perr == nil {
			partialPutURL = u
		} else {
			log.Printf("poll: presign partial for task %s: %v (dispatched without checkpoint URL)", c.TaskID, perr)
		}
	}

	var vp VerificationPolicy
	_ = json.Unmarshal(c.VerifPolicy, &vp)
	jt := JobType{Type: c.JobType}
	if len(c.JobTypeSpec) > 0 && string(c.JobTypeSpec) != "null" {
		var parsed JobType
		if err := json.Unmarshal(c.JobTypeSpec, &parsed); err == nil && parsed.Type != "" {
			jt = parsed
		}
	}
	disp := TaskDispatch{
		TaskID:           c.TaskID,
		Attempt:          c.Attempt,
		JobID:            c.JobID,
		RuntimeCellID:    c.RuntimeCellID,
		RuntimeID:        c.RuntimeID,
		RuntimeMatrixSHA: c.RuntimeMatrixSHA,
		Manifest: JobManifest{
			ID:           c.JobID,
			JobType:      jt,
			Model:        ModelRef{Kind: c.ModelKind, Ref: c.ModelRef},
			Inputs:       []InputRef{}, // real inputs travel via the presigned input_url, not the manifest
			Constraints:  claimedTaskConstraints(c),
			Verification: vp,
			Tier:         c.Tier,
		},
		InputURL:         inputURL,
		OutputURL:        outputURL,
		PartialPutURL:    partialPutURL,
		ResultKey:        c.ResultKey,
		OfferedRateUsdHr: c.OfferedRateUsdHr,
		Deadline:         uint64(time.Now().Add(time.Hour).Unix()),
	}
	metrics.tasksDispatched.Add(1)
	writeJSON(w, http.StatusOK, disp)
}

func (s *Server) handleWorkerStart(w http.ResponseWriter, r *http.Request) {
	auth := r.Context().Value(ctxWorker).(*WorkerAuth)
	id, ok := pathUUID(w, r)
	if !ok {
		return
	}
	attempt, err := taskAttemptHeader(r)
	if err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	err = s.store.StartTask(r.Context(), id, auth.WorkerID, attempt)
	if errors.Is(err, errNotFound) {
		writeErr(w, http.StatusConflict, "task not claimed by this worker or not startable")
		return
	}
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) handleWorkerCommit(w http.ResponseWriter, r *http.Request) {
	auth := r.Context().Value(ctxWorker).(*WorkerAuth)
	ctx := r.Context()
	id, ok := pathUUID(w, r)
	if !ok {
		return
	}
	var c TaskCommit
	if err := json.NewDecoder(r.Body).Decode(&c); err != nil {
		writeErr(w, http.StatusBadRequest, "invalid commit json: "+err.Error())
		return
	}
	if c.Attempt < 0 {
		writeErr(w, http.StatusBadRequest, "attempt must be non-negative")
		return
	}
	c.TaskID = id // trust the path, not the body

	info, err := s.store.completeTaskTx(ctx, id, auth.WorkerID, c, s.verification.probe)
	if errors.Is(err, errNotFound) {
		exact, replayErr := s.store.ExactTerminalVerificationCommit(ctx, id, auth.WorkerID, c)
		if replayErr != nil {
			writeErr(w, http.StatusInternalServerError, replayErr.Error())
			return
		}
		if exact {
			w.WriteHeader(http.StatusNoContent)
			return
		}
		writeErr(w, http.StatusConflict, "task not claimed by this worker or not committable")
		return
	}
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}

	processed, err := s.verification.ProcessAttempt(ctx, info.TaskID, int64(info.Attempt))
	if errors.Is(err, ErrVerificationStagingArtifactMissing) {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "verification recovery: "+err.Error())
		return
	}
	if processed.Pending {
		w.WriteHeader(http.StatusAccepted)
		return
	}
	if processed.Outcome != OutcomeFail {
		if err := s.finalizeJobIfDone(ctx, info.JobID); err != nil {
			writeErr(w, http.StatusInternalServerError, "finalizing job: "+err.Error())
			return
		}
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) finalizeJobIfDone(ctx context.Context, jobID uuid.UUID) error {
	done, err := s.store.JobAllTasksDone(ctx, jobID)
	if err != nil {
		return err
	}
	if !done {
		return nil
	}
	if _, err := s.MergeJobResults(ctx, jobID); err != nil {
		return err
	}
	if err := s.store.FinalizeJobTx(ctx, jobID); err != nil {
		return err
	}
	settleSLAOutcome(ctx, s.store, jobID)
	recordEtaCalibration(ctx, s.store, jobID)
	s.chargeForJob(ctx, jobID)
	return nil
}

func (s *Server) handleWorkerEarnings(w http.ResponseWriter, r *http.Request) {
	auth := r.Context().Value(ctxWorker).(*WorkerAuth)
	e, err := s.store.WorkerEarnings(r.Context(), auth.SupplierID)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, e)
}

func (s *Server) handleWorkerVerification(w http.ResponseWriter, r *http.Request) {
	auth := r.Context().Value(ctxWorker).(*WorkerAuth)
	v, err := s.store.SupplierVerification(r.Context(), auth.SupplierID)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, v)
}

func (s *Server) handleAdminWorkers(w http.ResponseWriter, r *http.Request) {
	ws, err := s.store.ListWorkers(r.Context())
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, ws)
}

func (s *Server) handleAdminFraudFlags(w http.ResponseWriter, r *http.Request) {
	f, err := s.store.ListFraudFlags(r.Context())
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, f)
}

func (s *Server) handleAdminFraud(w http.ResponseWriter, r *http.Request) {
	f, err := s.store.ListFraud(r.Context())
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, f)
}

func (s *Server) handleAdminDrift(w http.ResponseWriter, r *http.Request) {
	d, err := s.store.DriftRollup(r.Context())
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, d)
}

func (s *Server) handleAdminQuoteDrift(w http.ResponseWriter, r *http.Request) {
	d, err := s.store.CostDriftRollup(r.Context())
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, d)
}

func (s *Server) handleAdminSchedulerExplain(w http.ResponseWriter, r *http.Request) {
	raw := r.URL.Query().Get("worker_id")
	if raw == "" {
		writeErr(w, http.StatusBadRequest, "worker_id query parameter required")
		return
	}
	workerID, err := uuid.Parse(raw)
	if err != nil {
		writeErr(w, http.StatusBadRequest, "invalid worker_id: must be a uuid")
		return
	}
	exp, err := s.store.SchedulerExplain(r.Context(), workerID)
	if errors.Is(err, errNotFound) {
		writeErr(w, http.StatusNotFound, "worker not found")
		return
	}
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, exp)
}

func (s *Server) handleAdminJobs(w http.ResponseWriter, r *http.Request) {
	j, err := s.store.ListJobsAdmin(r.Context())
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, j)
}

func (s *Server) handleAdminPayouts(w http.ResponseWriter, r *http.Request) {
	p, err := s.store.ListPayoutsAdmin(r.Context())
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, p)
}

func (s *Server) handleAdminSuspend(w http.ResponseWriter, r *http.Request) {
	id, ok := pathUUID(w, r)
	if !ok {
		return
	}
	body, err := decodeAdminActionBody(r)
	if err != nil {
		writeErr(w, http.StatusBadRequest, "invalid request json: "+err.Error())
		return
	}
	actor, ok := adminActorFromContext(r.Context())
	if !ok {
		writeErr(w, http.StatusUnauthorized, "authenticated admin identity is required")
		return
	}
	err = s.store.SuspendWorker(r.Context(), actor, id, body.Reason, body.RequestID)
	if errors.Is(err, errNotFound) {
		writeErr(w, http.StatusNotFound, "worker not found")
		return
	}
	if writeAdminMutationInputOrAuthError(w, err) {
		return
	}
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "suspended"})
}

type adminActionBody struct {
	Reason    string  `json:"reason"`
	Delta     float32 `json:"delta,omitempty"`
	RequestID string  `json:"request_id,omitempty"`
}

const adminActionBodyLimit = 4 << 10

func decodeAdminActionBody(r *http.Request) (adminActionBody, error) {
	var b adminActionBody
	raw, err := io.ReadAll(io.LimitReader(r.Body, adminActionBodyLimit+1))
	if err != nil {
		return b, err
	}
	if len(raw) > adminActionBodyLimit {
		return b, fmt.Errorf("request body exceeds %d bytes", adminActionBodyLimit)
	}
	if err := decodeStrictJSONObject(raw, &b); err != nil {
		return b, err
	}
	return b, nil
}

func writeAdminMutationInputOrAuthError(w http.ResponseWriter, err error) bool {
	switch {
	case errors.Is(err, errAdminActorUnauthorized):
		writeErr(w, http.StatusUnauthorized, err.Error())
		return true
	case errors.Is(err, errAdminMutationInvalid):
		writeErr(w, http.StatusBadRequest, err.Error())
		return true
	default:
		return false
	}
}

func (s *Server) handleAdminReinstate(w http.ResponseWriter, r *http.Request) {
	id, ok := pathUUID(w, r)
	if !ok {
		return
	}
	body, err := decodeAdminActionBody(r)
	if err != nil {
		writeErr(w, http.StatusBadRequest, "invalid request json: "+err.Error())
		return
	}
	actor, ok := adminActorFromContext(r.Context())
	if !ok {
		writeErr(w, http.StatusUnauthorized, "authenticated admin identity is required")
		return
	}
	err = s.store.ReinstateWorker(r.Context(), actor, id, body.Reason, body.RequestID)
	if errors.Is(err, errNotFound) {
		writeErr(w, http.StatusNotFound, "worker not found")
		return
	}
	if errors.Is(err, errNotSuspended) {
		writeErr(w, http.StatusConflict, "worker's supplier is not currently suspended")
		return
	}
	if writeAdminMutationInputOrAuthError(w, err) {
		return
	}
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "active"})
}

func (s *Server) handleAdminRequeueTask(w http.ResponseWriter, r *http.Request) {
	id, ok := pathUUID(w, r)
	if !ok {
		return
	}
	body, err := decodeAdminActionBody(r)
	if err != nil {
		writeErr(w, http.StatusBadRequest, "invalid request json: "+err.Error())
		return
	}
	actor, ok := adminActorFromContext(r.Context())
	if !ok {
		writeErr(w, http.StatusUnauthorized, "authenticated admin identity is required")
		return
	}
	err = s.store.AdminForceRequeueTask(r.Context(), actor, id, body.Reason, body.RequestID)
	if errors.Is(err, errNotFound) {
		writeErr(w, http.StatusNotFound, "task not found")
		return
	}
	if errors.Is(err, errNotRequeueable) {
		writeErr(w, http.StatusConflict, "task is not running/retrying  -  nothing to requeue")
		return
	}
	if writeAdminMutationInputOrAuthError(w, err) {
		return
	}
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "queued"})
}

func (s *Server) handleAdminAdjustReputation(w http.ResponseWriter, r *http.Request) {
	id, ok := pathUUID(w, r)
	if !ok {
		return
	}
	body, err := decodeAdminActionBody(r)
	if err != nil {
		writeErr(w, http.StatusBadRequest, "invalid request json: "+err.Error())
		return
	}
	if body.Delta == 0 {
		writeErr(w, http.StatusBadRequest, "delta must be non-zero")
		return
	}
	actor, ok := adminActorFromContext(r.Context())
	if !ok {
		writeErr(w, http.StatusUnauthorized, "authenticated admin identity is required")
		return
	}
	before, after, err := s.store.AdminAdjustReputation(
		r.Context(), actor, id, body.Delta, body.Reason, body.RequestID)
	if errors.Is(err, errNotFound) {
		writeErr(w, http.StatusNotFound, "supplier not found")
		return
	}
	if writeAdminMutationInputOrAuthError(w, err) {
		return
	}
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"before": before, "after": after})
}

func (s *Server) handleAdminReleasePayout(w http.ResponseWriter, r *http.Request) {
	id, ok := pathUUID(w, r)
	if !ok {
		return
	}
	body, err := decodeAdminActionBody(r)
	if err != nil {
		writeErr(w, http.StatusBadRequest, "invalid request json: "+err.Error())
		return
	}
	actor, ok := adminActorFromContext(r.Context())
	if !ok {
		writeErr(w, http.StatusUnauthorized, "authenticated admin identity is required")
		return
	}
	err = s.store.ReleasePayoutTx(r.Context(), actor, id, body.Reason, body.RequestID)
	if errors.Is(err, errNotFound) {
		writeErr(w, http.StatusNotFound, "ledger entry not found")
		return
	}
	if errors.Is(err, errNotHeld) {
		writeErr(w, http.StatusConflict, "ledger entry is not currently held or ready")
		return
	}
	if writeAdminMutationInputOrAuthError(w, err) {
		return
	}
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "release_scheduled"})
}

type payoutSubsidyBody struct {
	FundRef          string `json:"fund_ref"`
	AuthorizationRef string `json:"authorization_ref"`
	Reason           string `json:"reason"`
}

type subsidyFundBody struct {
	FundRef             string `json:"fund_ref"`
	ExternalTreasuryRef string `json:"external_treasury_ref"`
	AuthorizedCents     int64  `json:"authorized_cents"`
	Reason              string `json:"reason"`
}

const moneyAuthorityBodyLimit = 4 << 10

func decodeMoneyAuthorityJSON(r *http.Request, dst any) error {
	body, err := io.ReadAll(io.LimitReader(r.Body, moneyAuthorityBodyLimit+1))
	if err != nil {
		return err
	}
	if len(body) > moneyAuthorityBodyLimit {
		return fmt.Errorf("request body exceeds %d bytes", moneyAuthorityBodyLimit)
	}
	return decodeStrictJSONObject(body, dst)
}

func (s *Server) handleAdminCreateSubsidyFund(w http.ResponseWriter, r *http.Request) {
	var body subsidyFundBody
	if err := decodeMoneyAuthorityJSON(r, &body); err != nil {
		writeErr(w, http.StatusBadRequest, "invalid request json: "+err.Error())
		return
	}
	actor, ok := adminActorFromContext(r.Context())
	if !ok {
		writeErr(w, http.StatusUnauthorized, "authenticated admin identity is required")
		return
	}
	created, err := s.store.CreateSubsidyFund(
		r.Context(), actor, body.FundRef, body.ExternalTreasuryRef, body.AuthorizedCents, body.Reason)
	if errors.Is(err, errSubsidyFundConflict) {
		writeErr(w, http.StatusConflict, err.Error())
		return
	}
	if errors.Is(err, errAdminActorUnauthorized) {
		writeErr(w, http.StatusUnauthorized, err.Error())
		return
	}
	if errors.Is(err, errMoneyAuthorityAuditInvariant) {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	if err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"status":  "subsidy_fund_authorized",
		"created": created,
	})
}

func (s *Server) handleAdminSubsidizePayout(w http.ResponseWriter, r *http.Request) {
	id, ok := pathUUID(w, r)
	if !ok {
		return
	}
	var body payoutSubsidyBody
	if err := decodeMoneyAuthorityJSON(r, &body); err != nil {
		writeErr(w, http.StatusBadRequest, "invalid request json: "+err.Error())
		return
	}
	actor, ok := adminActorFromContext(r.Context())
	if !ok {
		writeErr(w, http.StatusUnauthorized, "authenticated admin identity is required")
		return
	}
	created, err := s.store.AuthorizePayoutSubsidy(
		r.Context(), actor, id, body.FundRef, body.AuthorizationRef, body.Reason)
	if errors.Is(err, errNotFound) {
		writeErr(w, http.StatusNotFound, "ledger entry not found")
		return
	}
	if errors.Is(err, errNotHeld) || errors.Is(err, errPayoutFundingAlreadyBound) ||
		errors.Is(err, errSubsidyFundUnavailable) {
		writeErr(w, http.StatusConflict, err.Error())
		return
	}
	if errors.Is(err, errAdminActorUnauthorized) {
		writeErr(w, http.StatusUnauthorized, err.Error())
		return
	}
	if errors.Is(err, errMoneyAuthorityAuditInvariant) {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	if err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"status":  "subsidy_authorized",
		"created": created,
	})
}

func (s *Server) handleAdminActions(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	actions, err := s.store.ListAdminActions(r.Context())
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, actions)
}

func (s *Server) handleJobInvoice(w http.ResponseWriter, r *http.Request) {
	auth := r.Context().Value(ctxBuyer).(*AuthResult)
	id, ok := pathUUID(w, r)
	if !ok {
		return
	}
	inv, err := s.store.JobInvoice(r.Context(), id, auth.BuyerID)
	if errors.Is(err, errNotFound) {
		writeErr(w, http.StatusNotFound, "job not found")
		return
	}
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, inv)
}

func (s *Server) handleJobReceipt(w http.ResponseWriter, r *http.Request) {
	auth := r.Context().Value(ctxBuyer).(*AuthResult)
	id, ok := pathUUID(w, r)
	if !ok {
		return
	}
	inv, err := s.store.JobInvoice(r.Context(), id, auth.BuyerID)
	if errors.Is(err, errNotFound) {
		writeErr(w, http.StatusNotFound, "job not found")
		return
	}
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	verif, verr := s.store.JobVerification(r.Context(), id)
	if verr != nil {
		writeErr(w, http.StatusInternalServerError, verr.Error())
		return
	}
	classes, cerr := s.store.JobVerificationClasses(r.Context(), id)
	if cerr != nil {
		writeErr(w, http.StatusInternalServerError, cerr.Error())
		return
	}
	tasks, terr := s.store.JobTaskReceipts(r.Context(), id)
	if terr != nil {
		writeErr(w, http.StatusInternalServerError, terr.Error())
		return
	}
	writeJSON(w, http.StatusOK, assembleClearingReceipt(id, inv.Status, inv, verif, classes, tasks))
}

func secureHTMLHeaders(w http.ResponseWriter) {
	h := w.Header()
	h.Set("X-Content-Type-Options", "nosniff")
	h.Set("X-Frame-Options", "SAMEORIGIN")
	h.Set("Referrer-Policy", "no-referrer")
}

func (s *Server) handleRoot(w http.ResponseWriter, r *http.Request) {
	path := os.Getenv("SITE_PATH")
	if path == "" {
		path = "web/index.html"
	}
	serveHTML(w, path)
}

func (s *Server) handleAdminRoom(w http.ResponseWriter, r *http.Request) {
	path := os.Getenv("ADMIN_PATH")
	if path == "" {
		path = "web/admin.html"
	}
	serveHTML(w, path)
}

func serveHTML(w http.ResponseWriter, path string) {
	b, err := os.ReadFile(path)
	if err != nil {
		writeErr(w, http.StatusNotFound, "page not found")
		return
	}
	secureHTMLHeaders(w)
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	_, _ = w.Write(b)
}

func (s *Server) handleSiteAsset(w http.ResponseWriter, r *http.Request) {
	rel := r.PathValue("path")
	ctype, ok := siteAssetType(rel)
	if !ok {
		writeErr(w, http.StatusNotFound, "no such site asset")
		return
	}
	root := os.Getenv("SITE_ASSETS_PATH")
	if root == "" {
		root = "web/assets/site"
	}
	full := filepath.Join(root, filepath.FromSlash(rel))
	absRoot, err1 := filepath.Abs(root)
	absFull, err2 := filepath.Abs(full)
	if err1 != nil || err2 != nil || !strings.HasPrefix(absFull, absRoot+string(os.PathSeparator)) {
		writeErr(w, http.StatusNotFound, "no such site asset")
		return
	}
	h := w.Header()
	h.Set("X-Content-Type-Options", "nosniff")
	h.Set("Content-Type", ctype)
	if siteAssetHashed(rel) {
		h.Set("Cache-Control", "public, max-age=31536000, immutable")
	} else {
		h.Set("Cache-Control", "public, max-age=86400")
	}
	if brotliOK(r) {
		if bz, err := os.ReadFile(absFull + ".br"); err == nil {
			h.Set("Content-Encoding", "br")
			h.Set("Vary", "Accept-Encoding")
			_, _ = w.Write(bz)
			return
		}
	}
	b, err := os.ReadFile(absFull)
	if err != nil {
		writeErr(w, http.StatusNotFound, "no such site asset")
		return
	}
	if strings.HasSuffix(rel, ".glb") {
		h.Set("Accept-Ranges", "bytes") // let the loader range-request the glb
	}
	_, _ = w.Write(b)
}

func brotliOK(r *http.Request) bool {
	for _, tok := range strings.Split(r.Header.Get("Accept-Encoding"), ",") {
		if strings.TrimSpace(strings.SplitN(tok, ";", 2)[0]) == "br" {
			return true
		}
	}
	return false
}

func siteAssetHashed(rel string) bool {
	base := rel
	if i := strings.LastIndex(base, "/"); i >= 0 {
		base = base[i+1:]
	}
	dot := strings.LastIndex(base, ".")
	if dot <= 0 {
		return false
	}
	stem := base[:dot]
	dash := strings.LastIndex(stem, "-")
	if dash < 0 || len(stem)-dash-1 < 8 {
		return false
	}
	for _, c := range stem[dash+1:] {
		if (c < '0' || c > '9') && (c < 'a' || c > 'f') {
			return false
		}
	}
	return true
}

func siteAssetType(rel string) (string, bool) {
	if rel == "" || len(rel) > 128 || strings.HasPrefix(rel, "/") || strings.Contains(rel, "..") || strings.Contains(rel, "\\") {
		return "", false
	}
	for _, c := range rel {
		if (c < 'a' || c > 'z') && (c < 'A' || c > 'Z') && (c < '0' || c > '9') && c != '-' && c != '_' && c != '@' && c != '.' && c != '/' {
			return "", false
		}
	}
	switch {
	case strings.HasSuffix(rel, ".png"):
		return "image/png", true
	case strings.HasSuffix(rel, ".js"):
		return "text/javascript; charset=utf-8", true
	case strings.HasSuffix(rel, ".glb"):
		return "model/gltf-binary", true
	case strings.HasSuffix(rel, ".wasm"):
		return "application/wasm", true
	case strings.HasSuffix(rel, ".json"):
		return "application/json", true
	case strings.HasSuffix(rel, ".woff2"):
		return "font/woff2", true
	case strings.HasSuffix(rel, ".ktx2"):
		return "image/ktx2", true
	}
	return "", false
}

func (s *Server) handleFavicon(w http.ResponseWriter, r *http.Request) {
	b, err := os.ReadFile("web/favicon.ico")
	if err != nil {
		writeErr(w, http.StatusNotFound, "no favicon")
		return
	}
	h := w.Header()
	h.Set("Content-Type", "image/x-icon")
	h.Set("Cache-Control", "public, max-age=86400")
	_, _ = w.Write(b)
}

func modelPrice(m ModelRow) float64 {
	return m.PricePer1K
}

func tierMultiplier(tier string) float64 {
	switch tier {
	case "priority":
		return 1.5
	case "trusted":
		return 2.0
	default:
		return 1.0
	}
}

func generativeJobType(jobType string) bool {
	return jobType == "batch_infer"
}

const defaultQuoteMaxTokens = 256

func (s *Server) estimateJobUSD(ctx context.Context, jobType, modelRef string, inputBytesLen int, nLines int, maxTokens uint32, tier string) float64 {
	price := 0.002 // default per-1K when the model is unknown to the catalogue
	if m, err := s.store.GetModel(ctx, modelRef); err == nil {
		price = modelPrice(*m)
	}
	units := float64(nLines)
	if byteUnits := float64(inputBytesLen) / 4.0; byteUnits > units {
		units = byteUnits
	}
	if generativeJobType(jobType) && nLines > 0 {
		outTokensPerRecord := maxTokens
		if outTokensPerRecord == 0 {
			outTokensPerRecord = defaultQuoteMaxTokens
		}
		units += float64(nLines) * float64(outTokensPerRecord)
	}
	est := units / 1000.0 * price * tierMultiplier(tier)
	return roundUSD(est)
}

func splitSizeOf(params json.RawMessage) int {
	if len(params) == 0 {
		return defaultSplitSize
	}
	var p struct {
		SplitSize int `json:"split_size"`
	}
	if err := json.Unmarshal(params, &p); err != nil || p.SplitSize <= 0 {
		return defaultSplitSize
	}
	return p.SplitSize
}

func hasExplicitSplitSize(params json.RawMessage) bool {
	if len(params) == 0 {
		return false
	}
	var p struct {
		SplitSize int `json:"split_size"`
	}
	return json.Unmarshal(params, &p) == nil && p.SplitSize > 0
}

var jobTypeThroughput = map[string]float64{
	"embed":       200,
	"batch_infer": 4,
}

func throughputOf(jobType string) float64 {
	if v, ok := jobTypeThroughput[jobType]; ok && v > 0 {
		return v
	}
	return 10
}

func effectiveThroughput(jobType string, avgLineBytes float64) float64 {
	base := throughputOf(jobType)
	switch jobType {
	case "batch_infer":
		const refBytes = 140.0
		if avgLineBytes > refBytes {
			base *= refBytes / avgLineBytes
		}
	}
	if base < 0.2 {
		base = 0.2
	}
	return base
}

const targetTaskSecs = 45

func adaptiveSplitSize(jobType string, params json.RawMessage, avgLineBytes float64) int {
	if len(params) > 0 {
		var p struct {
			SplitSize int `json:"split_size"`
		}
		if err := json.Unmarshal(params, &p); err == nil && p.SplitSize > 0 {
			return p.SplitSize
		}
	}
	n := int(effectiveThroughput(jobType, avgLineBytes) * targetTaskSecs)
	if n < 1 {
		n = 1
	}
	if n > 4096 {
		n = 4096
	}
	return n
}

func (s *Server) adaptiveSplitSizeLive(ctx context.Context, jobType, modelRef string, minMemGB float32, maxTokens uint32, avgLineBytes float64, staticSize, totalRecords int) int {
	if !fanoutPlannerEnabled.Load() || !generativeJobType(jobType) {
		return staticSize
	}
	rows, err := s.store.FleetRateSnapshot(ctx, jobType, modelRef, minMemGB)
	if err != nil {
		return staticSize // degraded sizing, never a failed submit
	}
	var rates []float64
	for _, r := range rows {
		if r.TPS > 0 && !r.Throttled {
			rates = append(rates, float64(r.TPS))
		}
	}
	if len(rates) < plannerMinFleetSamples {
		return staticSize // rate cache too thin  -  static map remains in force
	}
	tokensPerItem := tokensPerItemEstimate(maxTokens, avgLineBytes)
	medianItemsPerSec := medianRate(rates) / tokensPerItem
	size := int(medianItemsPerSec * targetTaskSecs)
	if size < 1 {
		size = 1
	}
	if size > 4096 {
		size = 4096
	}
	width := 0
	if totalRecords > 0 {
		fleet := plannerFleetFromRows(rows, modelRef, func(tps float32) float64 {
			return float64(tps) / tokensPerItem
		})
		plan := PlanFanout(PlannerJob{Items: totalRecords, JobType: jobType, ModelRef: modelRef}, fleet)
		width = plan.Width
		if width > 0 {
			if chunks := (totalRecords + size - 1) / size; chunks < width {
				size = (totalRecords + width - 1) / width // ceil: chunk count >= planner width
				if size < 1 {
					size = 1
				}
			}
		}
		log.Printf("planner: split jobType=%s model=%s live_fleet=%d median_tps=%.1f tok/item=%.0f records=%d width=%d split=%d (static %d) modeled_wallclock_p50=%.1fs conservative=%.1fs single_node=%.1fs speedup_vs_single=%.2fx [MODELED]",
			jobType, modelRef, len(rates), medianRate(rates), tokensPerItem, totalRecords,
			plan.Width, size, staticSize, plan.WallClockP50Secs, plan.WallClockConservativeSecs,
			plan.SingleNodeSecs, plan.ModeledSpeedupVsSingle)
	} else {
		log.Printf("planner: split jobType=%s model=%s live_fleet=%d median_tps=%.1f tok/item=%.0f records=unknown(streamed) split=%d (static %d) width_floor=skipped [MODELED]",
			jobType, modelRef, len(rates), medianRate(rates), tokensPerItem, size, staticSize)
	}
	return size
}

func (s *Server) plannerETASecs(ctx context.Context, jobType, modelRef string, minMemGB float32, nTasks, queuedAhead, perTaskSecs int) (eta, conservative int, ok bool) {
	if !fanoutPlannerEnabled.Load() {
		return 0, 0, false
	}
	rows, err := s.store.FleetRateSnapshot(ctx, jobType, modelRef, minMemGB)
	if err != nil {
		return 0, 0, false
	}
	var rates []float64
	for _, r := range rows {
		if r.TPS > 0 && !r.Throttled {
			rates = append(rates, float64(r.TPS))
		}
	}
	median := medianRate(rates)
	if len(rates) < plannerMinFleetSamples || median <= 0 || perTaskSecs <= 0 {
		return 0, 0, false
	}
	fleet := plannerFleetFromRows(rows, modelRef, func(tps float32) float64 {
		return (float64(tps) / median) / float64(perTaskSecs) // task-units per second
	})
	plan := PlanFanout(PlannerJob{Items: queuedAhead + nTasks, JobType: jobType, ModelRef: modelRef}, fleet)
	if plan.Width == 0 {
		return 0, 0, false
	}
	eta = int(math.Ceil(plan.WallClockP50Secs))
	if eta < perTaskSecs {
		eta = perTaskSecs // a job always takes at least one task's worth of time
	}
	conservative = int(math.Ceil(plan.WallClockConservativeSecs))
	if conservative < eta {
		conservative = eta
	}
	log.Printf("planner: eta jobType=%s model=%s tasks=%d queued_ahead=%d live_fleet=%d width=%d per_task=%ds eta=%ds (conservative %ds) [MODELED]",
		jobType, modelRef, nTasks, queuedAhead, len(rates), plan.Width, perTaskSecs, eta, conservative)
	return eta, conservative, true
}

func (s *Server) offeredRateUsdHr(ctx context.Context, jobType, modelRef string) float32 {
	price := 0.002
	if m, err := s.store.GetModel(ctx, modelRef); err == nil {
		price = modelPrice(*m)
	}
	unitsPerHr := throughputOf(jobType) * 3600.0
	return float32(unitsPerHr / 1000.0 * price)
}

func (s *Server) offeredRateUsdHrForSubmission(ctx context.Context, sub jobSubmit) float32 {
	return s.offeredRateUsdHr(ctx, sub.JobType.Type, sub.Model.Ref)
}

func perTaskSecsFromP90(p90ms int64) int {
	if p90ms <= 0 {
		return targetTaskSecs // thin/empty history -> static target (Plane D D6 fallback)
	}
	secs := int((p90ms + 999) / 1000)
	if secs < 1 {
		secs = 1
	}
	return secs
}

func (s *Server) etaBandSecs(ctx context.Context, jobType, modelRef string, minMemGB float32, nTasks int) (eta, conservative int, plannerBacked bool) {
	p90ms, _, err := s.store.HistoricalP90DurationMs(ctx, jobType, modelRef)
	if err != nil {
		p90ms = 0
	}
	perTaskSecs := perTaskSecsFromP90(p90ms)
	queued, _ := s.store.QueuedTaskCount(ctx)
	if eta, cons, ok := s.plannerETASecs(ctx, jobType, modelRef, minMemGB, nTasks, queued, perTaskSecs); ok {
		return eta, cons, true
	}
	workers, _ := s.store.EligibleWorkerCount(ctx, jobType, modelRef, minMemGB)
	if workers < 1 {
		workers = 1
	}
	ahead := queued // tasks already waiting
	totalTasks := ahead + nTasks
	waves := (totalTasks + workers - 1) / workers
	eta = waves * perTaskSecs
	if eta < perTaskSecs {
		eta = perTaskSecs
	}
	return eta, 0, false
}

func tierMinCompletion(tier string) time.Duration {
	if tier == "priority" {
		return 5 * time.Minute
	}
	return 15 * time.Minute
}

func splitJSONL(data []byte, n int) [][]byte {
	if n <= 0 {
		n = defaultSplitSize
	}
	var lines [][]byte
	for _, ln := range bytes.Split(data, []byte("\n")) {
		ln = bytes.TrimRight(ln, "\r")
		if len(bytes.TrimSpace(ln)) == 0 {
			continue
		}
		lines = append(lines, ln)
	}
	if len(lines) == 0 {
		return nil
	}
	var chunks [][]byte
	for i := 0; i < len(lines); i += n {
		end := i + n
		if end > len(lines) {
			end = len(lines)
		}
		var buf bytes.Buffer
		for _, ln := range lines[i:end] {
			buf.Write(ln)
			buf.WriteByte('\n')
		}
		chunks = append(chunks, buf.Bytes())
	}
	return chunks
}

const inputSampleBytes = 1 << 20 // 1 MiB

func peekInputSample(r io.ReadCloser, max int) (sample []byte, rest io.ReadCloser, err error) {
	buf := make([]byte, max)
	n, rerr := io.ReadFull(r, buf)
	if rerr != nil && rerr != io.ErrUnexpectedEOF && rerr != io.EOF {
		return nil, nil, rerr
	}
	sample = buf[:n]
	return sample, io.NopCloser(io.MultiReader(bytes.NewReader(sample), r)), nil
}

type streamingPut struct {
	writer *io.PipeWriter
	done   chan error
}

func newStreamingPut(ctx context.Context, storage *Storage, key, contentType string) *streamingPut {
	pr, pw := io.Pipe()
	sp := &streamingPut{writer: pw, done: make(chan error, 1)}
	go func() {
		err := storage.PutObjectStream(ctx, key, pr, contentType)
		pr.CloseWithError(err) // unblock the writer side if PutObject gave up early
		sp.done <- err
	}()
	return sp
}

func (sp *streamingPut) wait() error { return <-sp.done }

func (s *Server) streamSplitAndUpload(ctx context.Context, jobID uuid.UUID, input io.Reader, splitSize int, canonicalTee io.Writer) (tasks []taskRow, totalBytes int, totalRecords int, exactInputBytes int, sum256 [32]byte, err error) {
	if splitSize <= 0 {
		splitSize = defaultSplitSize
	}
	if canonicalTee != nil {
		input = io.TeeReader(input, canonicalTee)
	}
	hasher := sha256.New()
	input = io.TeeReader(input, hasher)

	const uploadConcurrency = 16
	grp, gctx := errgroup.WithContext(ctx)
	grp.SetLimit(uploadConcurrency)

	nextIndex := 0

	br := bufio.NewReaderSize(input, 64<<10)
	var lineBuf bytes.Buffer
	linesInChunk := 0
	flush := func() {
		if linesInChunk == 0 {
			return
		}
		expectedRecords := linesInChunk
		idx := nextIndex
		nextIndex++
		chunk := append([]byte(nil), lineBuf.Bytes()...) // copy: lineBuf is reused next iteration
		lineBuf.Reset()
		linesInChunk = 0
		taskID := uuid.New()
		chunkKey := fmt.Sprintf("jobs/%s/tasks/%s/input.jsonl", jobID, taskID)
		tasks = append(tasks, taskRow{
			ID:                    taskID,
			JobID:                 jobID,
			InputRef:              chunkKey,
			ResultKey:             taskAttemptResultKey(jobID, taskID, 0),
			ChunkIndex:            idx,
			ExpectedOutputRecords: int64(expectedRecords),
		})
		grp.Go(func() error {
			return s.storage.PutObject(gctx, chunkKey, chunk, "application/x-ndjson")
		})
	}

	for {
		line, rerr := br.ReadBytes('\n')
		if len(line) > 0 {
			exactInputBytes += len(line) // every byte consumed, including newline/blank bytes
			trimmed := bytes.TrimRight(line, "\r\n")
			totalBytes += len(trimmed)
			if len(bytes.TrimSpace(trimmed)) != 0 {
				lineBuf.Write(trimmed)
				lineBuf.WriteByte('\n')
				linesInChunk++
				totalRecords++ // one non-blank JSONL line = one record (per-record output-token pricing)
				if linesInChunk >= splitSize {
					flush()
				}
			}
		}
		if rerr != nil {
			if rerr != io.EOF {
				_ = grp.Wait()
				return nil, 0, 0, 0, sum256, rerr
			}
			break
		}
	}
	flush() // the final, possibly-short chunk
	if werr := grp.Wait(); werr != nil {
		return nil, 0, 0, 0, sum256, werr
	}
	sort.Slice(tasks, func(i, j int) bool { return tasks[i].ChunkIndex < tasks[j].ChunkIndex })
	copy(sum256[:], hasher.Sum(nil))
	return tasks, totalBytes, totalRecords, exactInputBytes, sum256, nil
}

func countNonBlankJSONLRecords(data []byte) int {
	count := 0
	for len(data) > 0 {
		line := data
		if newline := bytes.IndexByte(data, '\n'); newline >= 0 {
			line = data[:newline]
			data = data[newline+1:]
		} else {
			data = nil
		}
		if len(bytes.TrimSpace(line)) > 0 {
			count++
		}
	}
	return count
}

func fracCount(n int, frac float32) int {
	if frac <= 0 || n <= 0 {
		return 0
	}
	c := int(math.Round(float64(n) * float64(frac)))
	if c > n {
		c = n
	}
	return c
}

func roundUSD(v float64) float64 { return math.Round(v*1e6) / 1e6 }

func redundancySelectionHash(jobID, taskID uuid.UUID) uint64 {
	h := sha256.New()
	h.Write(jobID[:])
	h.Write(taskID[:])
	sum := h.Sum(nil)
	return binary.BigEndian.Uint64(sum[:8])
}

func pathUUID(w http.ResponseWriter, r *http.Request) (uuid.UUID, bool) {
	id, err := uuid.Parse(r.PathValue("id"))
	if err != nil {
		writeErr(w, http.StatusBadRequest, "invalid id: must be a uuid")
		return uuid.Nil, false
	}
	return id, true
}

func (s *Server) handleCreateKey(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store, private")
	w.Header().Set("Pragma", "no-cache")
	auth := r.Context().Value(ctxBuyer).(*AuthResult)
	var body struct {
		Name string `json:"name"`
		Test bool   `json:"test"`
	}
	if r.Body != nil {
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil && err != io.EOF {
			writeErr(w, http.StatusBadRequest, "invalid request json: "+err.Error())
			return
		}
	}
	id, raw, masked, err := s.store.CreateAPIKey(r.Context(), auth.BuyerID, body.Name, body.Test)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "minting api key: "+err.Error())
		return
	}
	prefix := "cx_live_"
	if body.Test {
		prefix = "cx_test_"
	}
	writeJSON(w, http.StatusCreated, map[string]any{
		"id":         id,
		"name":       body.Name,
		"key":        raw, // revealed ONCE · never returned again
		"prefix":     prefix,
		"masked":     masked,
		"created_at": time.Now().UTC(),
	})
}

func (s *Server) handleListKeys(w http.ResponseWriter, r *http.Request) {
	auth := r.Context().Value(ctxBuyer).(*AuthResult)
	keys, err := s.store.ListAPIKeys(r.Context(), auth.BuyerID)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "listing api keys: "+err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"keys": keys})
}

func (s *Server) handleRevokeKey(w http.ResponseWriter, r *http.Request) {
	auth := r.Context().Value(ctxBuyer).(*AuthResult)
	id, ok := pathUUID(w, r)
	if !ok {
		return
	}
	if _, err := s.store.RevokeAPIKey(r.Context(), auth.BuyerID, id); err != nil {
		writeErr(w, http.StatusInternalServerError, "revoking api key: "+err.Error())
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	if err := json.NewEncoder(w).Encode(v); err != nil {
		_ = err
	}
}

func writeErr(w http.ResponseWriter, status int, msg string) {
	writeJSON(w, status, APIError{Error: msg})
}
