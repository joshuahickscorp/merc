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

// api.go — all HTTP handlers, auth middleware, and job→task splitting.
//
// Routing is stdlib net/http with Go 1.22+ method+path patterns (no framework).
// Auth is real: every buyer/admin route looks the bearer key up in api_keys,
// every worker route looks the X-Worker-Token up in worker_tokens. Missing or
// invalid credentials → 401 JSON. There is no silent bypass.

// Server holds the dependencies every handler needs.
type Server struct {
	store    *Store
	storage  *Storage
	verifier *Verifier
	// verification is the durable leased processor shared by commit requests;
	// background workers construct the same processor over the same work table.
	verification *VerificationProcessor
	payout       Payout
	// Rate limiters (stdlib token buckets, see ratelimit.go). ipLimiter bounds the
	// whole surface per source IP (brute-force/flood); the credential limiters bound
	// authenticated spam per api_key / worker_token.
	ipLimiter     *rateLimiter
	buyerLimiter  *rateLimiter
	workerLimiter *rateLimiter
	// signupLimiter is a SEPARATE, much tighter cap on account creation specifically
	// (see limitSignupByIP). The generic ipLimiter (30 req/s, burst 60) is a flood
	// guard, not an abuse guard: at that rate one IP can mint dozens of buyers a
	// minute. Each signup is a fresh Sybil identity that qualifies for
	// CX_SANDBOX_CREDIT_USD (free, cardless spend) the moment an operator sets that
	// env above zero for a public launch — so signup itself needs its own daily cap,
	// independent of whatever the flood limiter allows.
	signupLimiter *rateLimiter
}

// NewServer wires the handler dependencies.
func NewServer(store *Store, storage *Storage, verifier *Verifier, payout Payout) *Server {
	return &Server{
		store: store, storage: storage, verifier: verifier,
		verification: NewVerificationProcessor(store, storage, verifier), payout: payout,
		ipLimiter:     newRateLimiter(30, 60), // 30 req/s, burst 60, per IP
		buyerLimiter:  newRateLimiter(20, 40), // 20 req/s, burst 40, per api key
		workerLimiter: newRateLimiter(30, 60), // 30 req/s, burst 60, per worker token
		// signupsPerIPPerDay accounts per IP per rolling day (burst = daily cap,
		// rate = burst/86400s so a spent bucket refills to 1 signup roughly every
		// ~4.8h). Generous enough for a household/NAT sharing one public IP across a
		// few real devices; far too slow for a credit-farming script to matter.
		signupLimiter: newRateLimiter(signupsPerIPPerDay/86400.0, signupsPerIPPerDay),
	}
}

// signupsPerIPPerDay is the daily self-serve account-creation cap per source IP.
const signupsPerIPPerDay = 5

// ctxKey is the private type for request-context values.
type ctxKey int

const (
	ctxBuyer  ctxKey = iota // *AuthResult for buyer/admin routes
	ctxWorker               // *WorkerAuth for worker routes
	ctxAdmin                // AdminActor for routes authenticated by authAdmin
)

// Routes builds the mux with every route registered. /healthz is unauthed;
// everything else is wrapped in the matching auth middleware. The whole surface is
// then wrapped in the per-IP rate limiter (health/metrics exempt).
func (s *Server) Routes() http.Handler {
	mux := http.NewServeMux()

	mux.HandleFunc("GET /healthz", s.handleHealthz)
	mux.HandleFunc("GET /readyz", s.handleReadyz)
	mux.HandleFunc("GET /version", s.handleVersion)
	mux.HandleFunc("GET /metrics", s.handleMetrics)
	mux.HandleFunc("GET /{$}", s.handleRoot)                        // the public site (web/index.html) · the operator surface stays at /admin
	mux.HandleFunc("GET /assets/site/{path...}", s.handleSiteAsset) // whitelisted site static tree (renders, foam maps, glb, self-hosted Three.js)
	mux.HandleFunc("GET /assets/{path...}", s.handleDemoAsset)      // fixed flat buyer-console image allowlist; never a general file server
	mux.HandleFunc("GET /favicon.ico", s.handleFavicon)
	mux.HandleFunc("GET /demo", s.handleDemo) // Launch/Earn product demo (monochrome, same-origin)

	// Self-serve accounts (accounts.go) · unauthed: these MINT the credential.
	mux.HandleFunc("POST /v1/signup", s.handleSignup)
	mux.HandleFunc("POST /v1/login", s.handleLogin)
	mux.HandleFunc("POST /v1/alpha-request", s.handleAlphaRequest)               // public site's alpha-access capture (alpha_request.go), unauthed lead intake
	mux.HandleFunc("POST /v1/beacon", s.handleBeacon)                            // public site's cookie-free funnel beacon (beacon.go), unauthed pageview/scroll/CTA capture
	mux.Handle("POST /v1/logout", s.authBuyer(http.HandlerFunc(s.handleLogout))) // revoke the presenting session
	mux.Handle("GET /v1/me", s.authBuyer(http.HandlerFunc(s.handleMe)))          // authenticated buyer identity + remaining sandbox credit

	// Self-serve supplier onboarding (suppliers.go). Every route is authBuyer-gated
	// and resolves suppliers exclusively through suppliers.owner_buyer_id; caller-
	// supplied email/tax identifiers are rejected and KYC stays hosted at Stripe.
	mux.Handle("POST /v1/supplier/onboard", s.authBuyer(http.HandlerFunc(s.handleSupplierOnboard)))
	mux.Handle("GET /v1/supplier/status", s.authBuyer(http.HandlerFunc(s.handleSupplierStatus)))
	mux.Handle("POST /v1/supplier/worker-tokens", s.authBuyer(http.HandlerFunc(s.handleCreateWorkerToken))) // self-serve token mint, one call per new Mac (suppliers.go)
	// Device-proofed terminal-free enrollment. The account issues a short-lived
	// code bound to one P-256 device key; the unauthenticated exchange succeeds
	// once only with a signature from that key. Credential lifecycle remains
	// account-scoped and every transition is append-only audited.
	mux.Handle("POST /v1/supplier/enrollment-approvals", s.authBuyer(http.HandlerFunc(s.handleApproveWorkerEnrollmentRequest)))
	mux.Handle("POST /v1/supplier/enrollment-codes", s.authBuyer(http.HandlerFunc(s.handleCreateWorkerEnrollmentCode)))
	mux.Handle("DELETE /v1/supplier/enrollment-codes/{id}", s.authBuyer(http.HandlerFunc(s.handleRevokeWorkerEnrollmentCode)))
	mux.Handle("GET /v1/supplier/worker-credentials", s.authBuyer(http.HandlerFunc(s.handleListWorkerCredentials)))
	mux.Handle("DELETE /v1/supplier/worker-credentials/{id}", s.authBuyer(http.HandlerFunc(s.handleRevokeWorkerCredential)))
	mux.Handle("GET /v1/supplier/credential-audit", s.authBuyer(http.HandlerFunc(s.handleWorkerCredentialAudit)))
	mux.HandleFunc("POST /v1/worker/enrollment/exchange", s.handleExchangeWorkerEnrollmentCode)

	// Buyer API (Bearer api_key OR session token).
	mux.Handle("POST /v1/jobs", s.authBuyer(http.HandlerFunc(s.handleCreateJob)))
	mux.Handle("POST /v1/audio/jobs", s.authBuyer(http.HandlerFunc(s.handleAudioCreateJob)))
	mux.Handle("POST /v1/audio/jobs/quote", s.authBuyer(http.HandlerFunc(s.handleAudioQuote)))
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
	mux.Handle("POST /v1/quote", s.authBuyer(http.HandlerFunc(s.handleQuote)))                  // Plane C: scan + price, no spend
	mux.Handle("POST /v1/quote/pipeline", s.authBuyer(http.HandlerFunc(s.handlePipelineQuote))) // composite multi-stage quote (item 4)
	mux.Handle("POST /v1/webhooks", s.authBuyer(http.HandlerFunc(s.handleRegisterWebhook)))
	mux.Handle("POST /v1/private-pool", s.authBuyer(http.HandlerFunc(s.handleAddPrivatePoolMember)))           // Private Deployment (research §3)
	mux.Handle("GET /v1/private-pool", s.authBuyer(http.HandlerFunc(s.handleListPrivatePoolMembers)))          // Buyer advantage & pricing edge 6->7
	mux.Handle("DELETE /v1/private-pool/{id}", s.authBuyer(http.HandlerFunc(s.handleRemovePrivatePoolMember))) // Buyer advantage & pricing edge 6->7

	// Concierge intake + buyer billing (intake.go, billing.go). The callback is
	// unauthed — GitHub redirects to it with no bearer; the buyer is recovered from
	// the OAuth state parameter.
	mux.Handle("GET /v1/connect/github", s.authBuyer(http.HandlerFunc(s.handleGithubConnect)))
	mux.HandleFunc("GET /v1/connect/github/callback", s.handleGithubCallback)
	mux.Handle("GET /v1/sources", s.authBuyer(http.HandlerFunc(s.handleListSources)))
	mux.Handle("POST /v1/intake", s.authBuyer(http.HandlerFunc(s.handleCreateIntake)))
	mux.Handle("POST /v1/billing/setup", s.authBuyer(http.HandlerFunc(s.handleBillingSetup)))
	mux.Handle("GET /v1/billing/status", s.authBuyer(http.HandlerFunc(s.handleBillingStatus)))
	mux.Handle("GET /v1/sources/{id}/repos", s.authBuyer(http.HandlerFunc(s.handleListRepos)))
	mux.Handle("POST /v1/intake/launch", s.authBuyer(http.HandlerFunc(s.handleLaunchIntake)))
	mux.Handle("POST /v1/pipelines", s.authBuyer(http.HandlerFunc(s.handleCreatePipeline)))
	mux.Handle("GET /v1/pipelines/{id}", s.authBuyer(http.HandlerFunc(s.handleGetPipeline)))
	mux.Handle("GET /v1/pipelines/{id}/receipt", s.authBuyer(http.HandlerFunc(s.handlePipelineReceipt))) // pipeline ClearingReceipt (item 14)
	mux.Handle("POST /v1/deliver", s.authBuyer(http.HandlerFunc(s.handleDeliver)))
	mux.HandleFunc("POST /v1/stripe/webhook", s.handleStripeWebhook)          // unauthed; verified by signature
	mux.HandleFunc("POST /v1/stripe/connect-webhook", s.handleConnectWebhook) // Connect account.updated; verified by signature

	// Buyer API-key lifecycle: mint (raw secret revealed once), list (masked), revoke.
	mux.Handle("POST /v1/keys", s.authBuyer(http.HandlerFunc(s.handleCreateKey)))
	mux.Handle("GET /v1/keys", s.authBuyer(http.HandlerFunc(s.handleListKeys)))
	mux.Handle("DELETE /v1/keys/{id}", s.authBuyer(http.HandlerFunc(s.handleRevokeKey)))

	// Worker protocol (X-Worker-Token).
	mux.Handle("POST /v1/worker/register", s.authWorker(http.HandlerFunc(s.handleWorkerRegister)))
	mux.Handle("POST /v1/worker/heartbeat", s.authWorker(http.HandlerFunc(s.handleWorkerHeartbeat)))
	mux.Handle("GET /v1/worker/poll", s.authWorker(http.HandlerFunc(s.handleWorkerPoll)))
	mux.Handle("POST /v1/worker/task/{id}/start", s.authWorker(http.HandlerFunc(s.handleWorkerStart)))
	mux.Handle("POST /v1/worker/task/{id}/commit", s.authWorker(http.HandlerFunc(s.handleWorkerCommit)))
	mux.Handle("POST /v1/worker/task/{id}/fail", s.authWorker(http.HandlerFunc(s.handleWorkerFail))) // Plane C/D: immediate typed failure
	mux.Handle("GET /v1/worker/earnings", s.authWorker(http.HandlerFunc(s.handleWorkerEarnings)))
	mux.Handle("GET /v1/worker/verification", s.authWorker(http.HandlerFunc(s.handleWorkerVerification))) // trust panel (Supplier onboarding & safety 7->8)
	mux.Handle("POST /v1/worker/connect", s.authWorker(http.HandlerFunc(s.handleWorkerConnect)))
	mux.Handle("GET /v1/worker/connect/status", s.authWorker(http.HandlerFunc(s.handleWorkerConnectStatus)))

	// Admin panel page (passkey-gated in the browser; the DATA routes below enforce
	// auth server-side, so serving the HTML shell itself is safe/public).
	mux.HandleFunc("GET /admin", s.handleAdminPage)
	mux.HandleFunc("GET /admin/{$}", s.handleAdminPage)

	// Admin passkey (WebAuthn) — webauthn.go. Register is authAdmin-gated (the bearer
	// admin key bootstraps the first passkey; a passkey session enrolls the rest).
	// Login + status + logout are public (they MINT/inspect the session).
	mux.HandleFunc("GET /admin/passkey/status", s.handleAdminPasskeyStatus)
	mux.Handle("POST /admin/passkey/register/begin", s.authAdmin(http.HandlerFunc(s.handleAdminRegisterBegin)))
	mux.Handle("POST /admin/passkey/register/finish", s.authAdmin(http.HandlerFunc(s.handleAdminRegisterFinish)))
	mux.HandleFunc("POST /admin/passkey/login/begin", s.handleAdminLoginBegin)
	mux.HandleFunc("POST /admin/passkey/login/finish", s.handleAdminLoginFinish)
	mux.HandleFunc("POST /admin/passkey/logout", s.handleAdminLogout)

	// Admin data (authAdmin: a valid cx_admin_ passkey session cookie OR the admin
	// bearer key — see authAdmin in this file).
	mux.Handle("GET /admin/summary", s.authAdmin(http.HandlerFunc(s.handleAdminSummary))) // birds-eye roll-up (summary.go)
	mux.Handle("GET /admin/workers", s.authAdmin(http.HandlerFunc(s.handleAdminWorkers)))
	mux.Handle("GET /admin/jobs", s.authAdmin(http.HandlerFunc(s.handleAdminJobs)))
	mux.Handle("GET /admin/payouts", s.authAdmin(http.HandlerFunc(s.handleAdminPayouts)))
	mux.Handle("GET /admin/fraud-flags", s.authAdmin(http.HandlerFunc(s.handleAdminFraudFlags)))
	mux.Handle("GET /admin/fraud", s.authAdmin(http.HandlerFunc(s.handleAdminFraud)))
	mux.Handle("GET /admin/drift", s.authAdmin(http.HandlerFunc(s.handleAdminDrift)))
	// GET /admin/quotes: the COST-drift twin of /admin/drift (which is ETA-only).
	// Project Detection & Quotation 6.5->7 (docs/internal/CREED_AND_PATH_TO_TEN.md,
	// "Close the cost-drift loop and start auto-tuning prices").
	mux.Handle("GET /admin/quotes", s.authAdmin(http.HandlerFunc(s.handleAdminQuoteDrift)))
	mux.Handle("POST /admin/quotes/auto-tune", s.authAdmin(http.HandlerFunc(s.handleAdminAutoTunePrices)))
	mux.Handle("GET /admin/economics/jobs", s.authAdmin(http.HandlerFunc(s.handleAdminEconomicFacts)))
	mux.Handle("GET /admin/scheduler/explain", s.authAdmin(http.HandlerFunc(s.handleAdminSchedulerExplain)))
	mux.Handle("GET /admin/moat", s.authAdmin(http.HandlerFunc(s.handleAdminMoat)))                        // data-moat tracking counters (moat.go)
	mux.Handle("GET /admin/moat/reliability", s.authAdmin(http.HandlerFunc(s.handleAdminMoatReliability))) // per-(supplier,job_type) reliability view (moat.go, Data Moat 6->7)
	mux.Handle("GET /admin/funnel", s.authAdmin(http.HandlerFunc(s.handleAdminFunnel)))                    // site funnel beacon report (beacon.go)
	mux.Handle("POST /admin/workers/{id}/suspend", s.authAdmin(http.HandlerFunc(s.handleAdminSuspend)))
	// Operator Tooling 7->8 (docs/internal/CREED_AND_PATH_TO_TEN.md, "Add write
	// actions the operator currently has to reach into the database for"): closes
	// the three highest-frequency raw-SQL procedures RUNBOOKS.md documented —
	// reinstate a quarantined supplier, force-requeue a wedged task, adjust a
	// supplier's reputation, and release a payout hold — each now a real, audited
	// admin endpoint instead of a psql one-liner.
	mux.Handle("POST /admin/workers/{id}/reinstate", s.authAdmin(http.HandlerFunc(s.handleAdminReinstate)))
	mux.Handle("POST /admin/tasks/{id}/requeue", s.authAdmin(http.HandlerFunc(s.handleAdminRequeueTask)))
	mux.Handle("POST /admin/suppliers/{id}/reputation", s.authAdmin(http.HandlerFunc(s.handleAdminAdjustReputation)))
	mux.Handle("POST /admin/payouts/{id}/release", s.authAdmin(http.HandlerFunc(s.handleAdminReleasePayout)))
	mux.Handle("POST /admin/subsidy-funds", s.authAdmin(http.HandlerFunc(s.handleAdminCreateSubsidyFund)))
	mux.Handle("POST /admin/payouts/{id}/subsidize", s.authAdmin(http.HandlerFunc(s.handleAdminSubsidizePayout)))
	mux.Handle("GET /admin/actions", s.authAdmin(http.HandlerFunc(s.handleAdminActions))) // audit log for the above

	return observe(s.ipLimiter.limitByIP(capBody(requestBodyLimit, mux)))
}

// --- middleware ---

// statusRecorder captures the response status for access logging. Unwrap lets
// http.ResponseController reach the underlying writer (Flush/Hijack) if ever needed.
type statusRecorder struct {
	http.ResponseWriter
	status int
}

func (r *statusRecorder) WriteHeader(code int)        { r.status = code; r.ResponseWriter.WriteHeader(code) }
func (r *statusRecorder) Unwrap() http.ResponseWriter { return r.ResponseWriter }

// observe assigns each request a correlation id (propagated from X-Request-ID or
// generated), echoes it on the response, and emits one structured access-log line
// (method · path · status · latency · id) so a buyer/supplier request can be traced
// end to end. High-frequency /healthz + /metrics are skipped to keep logs signal-dense.
// maxRequestBodyBytes is the exceptional artifact/upload ceiling. It leaves room
// for the explicitly supported 64 MiB OpenAI multipart upload plus framing and is
// also reused as the absolute verification-artifact ceiling. Ordinary JSON routes
// use maxJSONRequestBodyBytes below; giving every decoder this much headroom would
// let a small authenticated request shape consume most of the production
// container after JSON expansion.
const maxRequestBodyBytes = 72 << 20 // 72 MiB

// maxJSONRequestBodyBytes is the blanket ceiling for ordinary API JSON. Payloads
// that legitimately carry bulk data have explicit routes below: /v1/jobs has its
// audited inline limit plus the streaming s3_key path, /v1/files is the 64 MiB
// upload path, and audio has its own strict multipart limit.
const maxJSONRequestBodyBytes = 4 << 20 // 4 MiB

// maxJobSubmitBodyBytes is intentionally tighter than the ordinary upload cap.
// An inline JSONL submit is a JSON string decoded into json.RawMessage before the
// downstream splitter can stream it, so its peak heap cost is several copies of
// the wire body. Thirty-two MiB stays useful inside the production memory budget.
// Larger inputs use the existing content-addressed {"s3_key":...} path, which
// streams through storage.GetObjectReader and is not carried in this request body.
const maxJobSubmitBodyBytes = 32 << 20 // 32 MiB

const maxSynchronousInputBytes = maxJobSubmitBodyBytes

var errSynchronousInputTooLarge = errors.New("synchronous input exceeds the 32 MiB control-plane scan ceiling")

// readAndCloseBounded is the one whole-buffer boundary for synchronous quote,
// planning, and workflow-chaining paths. Large async job submission still streams;
// synchronous control-plane work reads limit+1 so overflow is detected explicitly
// instead of truncating or risking an unbounded allocation. The reader is closed on
// every outcome, including read failure and overflow.
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

// capBody wraps every request's Body in http.MaxBytesReader so a body past the
// route's limit fails cleanly (the json decoder / io.ReadAll returns an error,
// and MaxBytesReader itself closes the connection rather than reading further)
// instead of buffering an unbounded amount of memory — an oversized body 413s,
// it never OOMs or crashes the process. This is deliberately the
// OUTERMOST-of-the-innermost wrap — applied ONCE, around the whole mux, so no
// individual handler can forget it AND no per-route wrap could accidentally
// layer a LARGER http.MaxBytesReader on top of a smaller one (Go's
// MaxBytesReader nests to the SMALLEST limit seen, not the most recent one — a
// second, bigger call never widens an outer, smaller one). limitFor picks the
// limit per-request so exceptional upload routes can carry their own deliberate
// larger or smaller ceiling without relying on a second, ineffective wrap.
func capBody(limitFor func(*http.Request) int64, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		r.Body = http.MaxBytesReader(w, r.Body, limitFor(r))
		next.ServeHTTP(w, r)
	})
}

// requestBodyLimit is capBody's limitFor: the decoder-safe inline limit for POST
// /v1/jobs; raw-WAV-plus-64-KiB for the two strict audio multipart routes; the
// upload-compatible ceiling only for POST /v1/files; and the small decoder-safe
// JSON ceiling for everything else.
func requestBodyLimit(r *http.Request) int64 {
	if r.Method == http.MethodPost &&
		(r.URL.Path == "/v1/audio/jobs" || r.URL.Path == "/v1/audio/jobs/quote") {
		return audioUploadMaxRawBytes + audioUploadBodyOverhead
	}
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
		// Per-endpoint HTTP request-duration histogram (Performance Observability &
		// Regression Tracking, docs/internal/CREED_AND_PATH_TO_TEN.md: the exposition
		// had task- and claim-duration histograms but "no HTTP request duration ...
		// per-endpoint p99"). The LABEL is the ServeMux-matched route PATTERN (r.Pattern,
		// populated by the mux during next.ServeHTTP — this middleware wraps the whole
		// mux, so it is set by the time we read it here), e.g. "GET /v1/jobs/{id}", NOT
		// the raw path — so a route with a path variable stays ONE bounded series instead
		// of exploding into one per job id. An unmatched request (404, no pattern) is
		// bucketed under a single "unmatched" label so a flood of bogus paths can never
		// blow up label cardinality.
		endpoint := r.Pattern
		if endpoint == "" {
			endpoint = "unmatched"
		}
		observeHTTPRequest(endpoint, dur)
		log.Print(formatRequestLog(rid, r.Method, r.URL.Path, rec.status, dur))
	})
}

func formatRequestLog(requestID, method, path string, status int, duration time.Duration) string {
	// Quote every request-derived string so control characters (including an
	// encoded newline decoded into URL.Path) cannot forge a second log record.
	return fmt.Sprintf("req id=%q method=%q path=%q status=%d duration_ms=%d",
		requestID, method, path, status, duration.Milliseconds())
}

// authBuyer authenticates a Bearer credential · either an api_key (cx_live_/cx_test_)
// or a self-serve session token (cx_sess_) · and stashes the AuthResult. The token's
// prefix selects the lookup: a cx_sess_ bearer goes through the sessions table (hashed
// at rest, expiry- and revocation-checked), everything else through api_keys. Both
// resolve to the same buyer scope, so every buyer route works with either credential;
// a session is never admin (admin is api-key-only). Honest 401 on any miss.
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
		if isRemote(r) && !s.buyerLimiter.allow(auth.BuyerID.String()) {
			writeErr(w, http.StatusTooManyRequests, "rate limit exceeded")
			return
		}
		ctx := context.WithValue(r.Context(), ctxBuyer, &auth)
		next.ServeHTTP(w, r.WithContext(ctx))
	})
}

// authAdmin gates admin routes on EITHER of two credentials:
//  1. a valid cx_admin_ passkey session cookie (the operator signed in with a passkey
//     at /admin — see webauthn.go), or
//  2. the admin bearer key (authBuyer + is_admin) — kept as BREAK-GLASS so a lost or
//     not-yet-registered passkey can never lock the operator out.
//
// The passkey path is checked first and, on success, synthesizes an admin AuthResult
// (admin reads are cross-buyer and never dereference BuyerID, verified in api.go).
// Both paths also attach a secret-free AdminActor under ctxAdmin. Passkey-first is
// intentional: when both credentials are present, the phishing-resistant session is
// the authority the downstream audit records.
func (s *Server) authAdmin(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if actor, ok := s.lookupAdminSessionActor(r); ok {
			ctx := context.WithValue(r.Context(), ctxBuyer, &AuthResult{IsAdmin: true})
			ctx = context.WithValue(ctx, ctxAdmin, actor)
			next.ServeHTTP(w, r.WithContext(ctx))
			return
		}
		s.authBuyer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			auth := r.Context().Value(ctxBuyer).(*AuthResult)
			if !auth.IsAdmin {
				writeErr(w, http.StatusForbidden, "admin privilege required")
				return
			}
			actor := AdminActor{
				Mode:             AdminAuthBreakGlassAPIKey,
				PrincipalID:      auth.APIKeyID,
				AttributionScope: AdminAttributionSharedCredentialOnly,
				Label:            auth.APIKeyLabel,
			}
			ctx := context.WithValue(r.Context(), ctxAdmin, actor)
			next.ServeHTTP(w, r.WithContext(ctx))
		})).ServeHTTP(w, r)
	})
}

// authWorker authenticates an X-Worker-Token and stashes the WorkerAuth.
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
		if isRemote(r) && !s.workerLimiter.allow(auth.WorkerID.String()) {
			writeErr(w, http.StatusTooManyRequests, "rate limit exceeded")
			return
		}
		ctx := context.WithValue(r.Context(), ctxWorker, &auth)
		next.ServeHTTP(w, r.WithContext(ctx))
	})
}

// bearer extracts a Bearer token from the Authorization header.
func bearer(r *http.Request) (string, bool) {
	h := r.Header.Get("Authorization")
	const p = "Bearer "
	if len(h) <= len(p) || !strings.EqualFold(h[:len(p)], p) {
		return "", false
	}
	return strings.TrimSpace(h[len(p):]), true
}

// --- health ---

func (s *Server) handleHealthz(w http.ResponseWriter, r *http.Request) {
	if err := s.store.Ping(r.Context()); err != nil {
		writeErr(w, http.StatusServiceUnavailable, "database unreachable")
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}

// handleVersion exposes immutable build provenance without requiring database or
// auth. Deploy automation checks this after the edge is live, so a healthy service
// running the wrong source revision cannot be reported as a successful deploy.
func (s *Server) handleVersion(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, currentControlBuildInfo())
}

// handleReadyz is the readiness probe: DB reachability, progress of an enabled
// worker-election loop, and liveness of any background tickers this replica owns.
// A process whose DB is up but whose election goroutine died, or whose payout-release
// / stale-requeue / job-finalize / webhook-sweep loop wedged, is NOT ready to serve:
// money would stop moving while /healthz stayed green. We fail (503) with the exact
// class of failure so the condition is observable and actionable. A load balancer
// can route on /readyz to drain a degraded node.
func (s *Server) handleReadyz(w http.ResponseWriter, r *http.Request) {
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

// --- buyer handlers ---

// jobSubmit is the POST /v1/jobs request shape: the buyer-facing submission
// (not a raw JobManifest). input is polymorphic — a JSON string IS the inline
// JSONL, or {"s3_key":"..."} points at an already-uploaded object.
type jobSubmit struct {
	JobType      JobType            `json:"job_type"`
	Model        ModelRef           `json:"model"`
	Params       json.RawMessage    `json:"params"`
	Constraints  JobConstraints     `json:"constraints"`
	Verification VerificationPolicy `json:"verification"`
	Tier         string             `json:"tier"`
	Input        json.RawMessage    `json:"input"`
	WebhookURL   string             `json:"webhook_url"`
	// MaxUSD is the optional buyer hard spend cap (Budget Governor, Plane C §12 /
	// Plane D §14 D8). When > 0 the claim refuses to dispatch a new task once the
	// job's projected charge would breach it. omitempty/pointer-free zero (0) means
	// no cap — unchanged behavior for buyers who do not set one.
	MaxUSD float64 `json:"max_usd,omitempty"`
	// QuoteID optionally binds this submission to an advisory quote (Plane D D7): the
	// "q_<uuid>" (or bare uuid) returned by POST /v1/quote. When set, createJob checks
	// the quote is the buyer's, not expired, and matches this job's type/model/tier
	// (and best-effort input bytes) before persisting the binding; a mismatch is 409.
	// Empty (the default) keeps the unbound submission path unchanged.
	QuoteID string `json:"quote_id,omitempty"`
	// FirmQuote opts into the firm-quote tier (Project Detection & Quotation 7->8,
	// docs/internal/CREED_AND_PATH_TO_TEN.md, "Ship a firm-quote tier: a real
	// commitment, not just an estimate"): the bound quote's cost_max_usd becomes a
	// REAL CEILING on the buyer's charge — if the job's actual cost exceeds it, the
	// buyer is still only ever charged the quoted maximum, and the platform absorbs
	// the difference. Requires QuoteID (a firm price commitment needs something to
	// be firm ABOUT); false (the default) leaves the existing advisory-only
	// behavior — quoted-vs-actual is shown on the invoice, but the buyer pays
	// actuals — completely unchanged.
	FirmQuote bool `json:"firm_quote,omitempty"`
	// MinReputation routes this job only to suppliers whose reputation is >= this (0..1).
	// The Elite-supplier moat (DEEP_RESEARCH_V2 §6.4 anti-defection): high-margin /
	// enterprise work is reachable only by suppliers who earned a high reputation ON the
	// platform, an asset they cannot port to a direct deal. 0 (default) = any supplier.
	MinReputation float32 `json:"min_reputation,omitempty"`
	// PrivatePool routes this job ONLY to the buyer's bound suppliers (Private Deployment
	// tier, research §3): their dedicated fleet, so the data never touches a shared pool.
	PrivatePool bool `json:"private_pool,omitempty"`
	// DeadlineSecs is the buyer's stuck-run watchdog policy knob: -1 opts OUT of the
	// watchdog entirely (run to completion — never judged, not even by the 24h cap),
	// 0 (the default) keeps the ETA-derived deadline, and 60..604800 (1 minute to
	// 7 days) sets an explicit wall-clock deadline. Anything else is a 400.
	DeadlineSecs int `json:"deadline_secs,omitempty"`

	// audioAdmission is server-only authority. JSON decoding cannot populate it;
	// only the strict WAV upload route can. Its price is overwritten from the
	// catalogue before quote/job economics are built.
	audioAdmission *audioAdmission
}

// defaultSplitSize is the JSONL chunk size (lines per task) when params omits it.
const defaultSplitSize = 256

// handleCreateJob is the real submission path: it resolves the input JSONL
// (inline or from object storage), uploads the canonical input, splits it into
// per-task chunks (each its own object), creates the job + task rows carrying
// the chunk keys, sizes honeypot/redundancy tasks from the verification policy,
// registers a webhook if given, and returns 202 with the estimate.
func (s *Server) handleCreateJob(w http.ResponseWriter, r *http.Request) {
	auth := r.Context().Value(ctxBuyer).(*AuthResult)
	var sub jobSubmit
	if err := json.NewDecoder(r.Body).Decode(&sub); err != nil {
		// Data Transfer & Artifact I/O 8->9 / Scalability Headroom 7->8, "Remove the
		// artifact-size ceiling": a body past maxJobSubmitBodyBytes must 413 CLEANLY,
		// not surface as an ordinary 400 "invalid json" (what a bare decode error
		// looks like otherwise). http.MaxBytesReader's *http.MaxBytesError is exactly
		// this case, distinguishable from a genuinely malformed body.
		var mbe *http.MaxBytesError
		if errors.As(err, &mbe) {
			writeErr(w, http.StatusRequestEntityTooLarge,
				fmt.Sprintf("request body exceeds the %d byte submission limit", mbe.Limit))
			return
		}
		writeErr(w, http.StatusBadRequest, "invalid job submission json: "+err.Error())
		return
	}
	resp, herr := s.createJob(r.Context(), auth.BuyerID, sub)
	if herr != nil {
		writeErr(w, herr.status, herr.msg)
		return
	}
	if resp.WebhookSecret != "" {
		setSecretResponseHeaders(w)
	}
	writeJSON(w, http.StatusAccepted, resp)
}

// httpError carries an HTTP status + message from an internal helper back to the
// handler that writes the response, so job creation can be shared by POST /v1/jobs
// and the OpenAI batch endpoint (openai.go) without either duplicating the pipeline.
type httpError struct {
	status int
	msg    string
}

func (e *httpError) Error() string { return e.msg }

// createJob runs the full submission pipeline for an already-decoded, buyer-scoped
// jobSubmit and returns the 202 payload (or an httpError). The single source of
// truth for turning a submission into a job + tasks: it resolves the input JSONL,
// uploads the canonical input, splits it into per-task chunks, sizes
// honeypot/redundancy tasks from the verification policy, registers a webhook if
// given, and persists the job. Both the native API and the OpenAI batch endpoint
// go through here.
func (s *Server) createJob(ctx context.Context, buyerID uuid.UUID, sub jobSubmit) (JobSubmitResponse, *httpError) {
	// Reject a malformed buyer cap before model lookup, billing lookup, object
	// storage, or any other side effect. Zero is the documented "no cap" sentinel;
	// a negative/non-finite value must never flow to nullPosFloat, where it would be
	// persisted as NULL and silently turn an intended hard cap into an uncapped job.
	if math.IsNaN(sub.MaxUSD) || math.IsInf(sub.MaxUSD, 0) || sub.MaxUSD < 0 {
		return JobSubmitResponse{}, &httpError{http.StatusBadRequest, "max_usd must be a finite non-negative number"}
	}
	if sub.JobType.Type == "" {
		return JobSubmitResponse{}, &httpError{http.StatusBadRequest, "job_type.type is required"}
	}
	if !validJobTypes[sub.JobType.Type] {
		return JobSubmitResponse{}, &httpError{http.StatusBadRequest, "invalid job_type.type: " + sub.JobType.Type}
	}
	if herr := rejectUntrustedAudioSubmission(sub); herr != nil {
		return JobSubmitResponse{}, herr
	}
	if sub.Tier == "" {
		sub.Tier = "batch"
	}
	if !validTiers[sub.Tier] {
		return JobSubmitResponse{}, &httpError{http.StatusBadRequest, "invalid tier: " + sub.Tier}
	}
	// The generated runtime matrix is the production job/model authority. Reject an
	// unknown, incompatible, hardware-pending, soak-only, stub, or wire-only cell
	// before input resolution/storage can create any side effect. A model's mere
	// presence in the DB pricing catalog is not permission to execute it.
	canonicalModel, err := normalizeAdvertisedRuntimeModelRef(sub.JobType.Type, sub.Model)
	if err != nil {
		return JobSubmitResponse{}, &httpError{http.StatusBadRequest, err.Error()}
	}
	sub.Model = canonicalModel
	if herr := s.prepareAudioPricing(ctx, &sub); herr != nil {
		return JobSubmitResponse{}, herr
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
	// Validate the webhook URL up front (before any S3 work) so a bad URL never
	// leaves a job created with no webhook.
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
	// Watchdog policy knob: only -1 (opt out), 0 (default), or an explicit
	// 60..604800 wall-clock deadline are meaningful; anything else is a typo the
	// buyer should hear about now, not a silently-misread deadline.
	if sub.DeadlineSecs != 0 && sub.DeadlineSecs != -1 &&
		(sub.DeadlineSecs < 60 || sub.DeadlineSecs > 604800) {
		return JobSubmitResponse{}, &httpError{http.StatusBadRequest,
			"deadline_secs must be -1 (run to completion), 0 (default watchdog), or 60..604800 seconds"}
	}
	// Private pool guard (Buyer advantage & pricing edge 6->7,
	// docs/internal/CREED_AND_PATH_TO_TEN.md: "Productize the privacy premium
	// instead of leaving it a sentence"): private_pool was previously accepted with
	// ZERO validation that the buyer had bound ANY supplier — the dispatch filter
	// (control/scheduler.go's private_pool_members EXISTS clause) would then
	// silently refuse to hand the job to anyone, ever, with no error at submit
	// time and no way for the buyer to learn why their job was stuck at 0%. Refuse
	// loudly here instead, before any storage write, so the failure is visible at
	// submit rather than discovered later as an inexplicably stalled job.
	if sub.PrivatePool {
		n, err := s.store.PrivatePoolMemberCount(ctx, buyerID)
		if err != nil {
			return JobSubmitResponse{}, &httpError{http.StatusInternalServerError, "checking private pool membership: " + err.Error()}
		}
		if n == 0 {
			return JobSubmitResponse{}, &httpError{http.StatusBadRequest,
				"private_pool is set but you have zero bound suppliers: POST /v1/private-pool {\"supplier_id\":\"<uuid>\"} first, or this job could never be claimed"}
		}
	}
	// Verification floor (Verification & Result Trust 6->7,
	// docs/internal/CREED_AND_PATH_TO_TEN.md): redundancy_frac=0 AND
	// honeypot_frac=0 used to be silently accepted — a job with ZERO real
	// anti-fraud coverage. `wantVerificationFloor` records that this job needs
	// at least one honeypot task injected below (after nPrimary/nHoneypot are
	// computed from the real chunk count) UNLESS the buyer explicitly opted
	// out. A buyer who set EITHER fraction above zero already asked for real
	// coverage and is left untouched. Deliberately NOT done by bumping
	// HoneypotFrac to a small constant and trusting fracCount's rounding: for a
	// job with few chunks (the common case), a percentage-based floor can
	// itself round back down to zero — the same bug this rung fixes, just
	// moved one step over. The floor is enforced as a real minimum COUNT below.
	wantVerificationFloor := !sub.Verification.SkipVerificationFloor &&
		sub.Verification.RedundancyFrac <= 0 && sub.Verification.HoneypotFrac <= 0

	// Require a saved payment method before accepting billable work WHEN billing is
	// configured · UNLESS the buyer still has sandbox free credit. Without this gate a
	// cardless buyer's completed job is owed forever (the ledger records it but
	// chargeForJob can never collect off-session). We surface it at submit (402) rather
	// than silently accruing uncollectable debt. With no Stripe key (local/dev/test) the
	// gate is skipped · behavior unchanged.
	//
	// SANDBOX EXEMPTION: a new buyer is granted a small free credit (buyers.free_credit_usd)
	// so they can run jobs before adding a card. While unspent credit remains, a cardless
	// submit is allowed; once the realized spend (buyer_charge debits) reaches the grant,
	// the gate requires a card honestly. This is an HONEST boundary · the credit is real
	// ledger spend, not a faked bypass · and the gate re-asserts the instant it is exhausted.
	if stripeKey() != "" {
		_, pm, berr := s.store.GetBillingCustomer(ctx, buyerID)
		switch {
		case berr != nil && !errors.Is(berr, errNotFound):
			return JobSubmitResponse{}, &httpError{http.StatusServiceUnavailable, "billing lookup failed: " + berr.Error()}
		case errors.Is(berr, errNotFound), pm == "":
			// No card on file: allow only while sandbox free credit remains.
			free, ferr := s.store.BuyerFreeCreditRemaining(ctx, buyerID)
			if ferr != nil {
				return JobSubmitResponse{}, &httpError{http.StatusServiceUnavailable, "free-credit lookup failed: " + ferr.Error()}
			}
			if free <= 0 {
				return JobSubmitResponse{}, &httpError{http.StatusPaymentRequired, "no payment method on file and sandbox free credit is exhausted · save a card via POST /v1/billing/setup before submitting a job"}
			}
			// Hard-bound this cardless sandbox job's spend to the remaining grant: the
			// budget governor enforces MaxUSD during task claims, so a single job cannot
			// exceed the free credit. With the in-flight reservation in
			// BuyerFreeCreditRemaining, the per-buyer free pool cannot be overspent across
			// concurrent submits. Without this, free<=0 was a pure boolean and one job
			// could run unbounded supplier-paid compute on a tiny grant.
			if sub.MaxUSD <= 0 || sub.MaxUSD > free {
				sub.MaxUSD = free
			}
		}
	}
	if sub.JobType.Type == audioUploadJobType && sub.MaxUSD > 0 {
		plan := audioInitialEconomicPlan(sub, economicSchedule)
		if !plan.Executable {
			return JobSubmitResponse{}, &httpError{http.StatusConflict, "job is not economically executable: " + plan.BlockReason}
		}
		if sub.MaxUSD < plan.InitialBuyerChargeUSD {
			return JobSubmitResponse{}, &httpError{http.StatusBadRequest,
				fmt.Sprintf("max_usd %.6f is below the audio job's initial guarded charge %.6f", sub.MaxUSD, plan.InitialBuyerChargeUSD)}
		}
	}

	// Quote-to-submit binding (Plane D D7), CHEAP checks only: if the buyer passed a
	// quote_id, load it and check everything that needs no input bytes (existence,
	// expiry, job_type/model/tier match) BEFORE any storage writes, so a stale/
	// mismatched quote_id rejects cleanly with no orphaned objects — same as
	// before this rung. The one check that DOES need the input (the sha256
	// fingerprint match) cannot run yet: on the new streamed path, the hash is
	// only known after the single pass over the input completes below, so it is
	// checked AFTER streaming, see the qBind.InputSHA256 comparison further
	// down. That is a genuine (small, honest) trade of this rung: a buyer who
	// passes a valid, non-expired, type-matching quote_id but a DIFFERENT input
	// than the one quoted now has its chunks uploaded before the mismatch is
	// caught and the submission rejected — those chunk objects are orphaned
	// (unreferenced garbage, cheap, and no job/task DB row is ever created to
	// point at them) rather than the previous "reject before writing anything"
	// guarantee. Whole-buffer verify-then-write was the only way to avoid this
	// entirely, and whole-buffer is exactly what streaming exists to remove.
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
		// The quote must describe THIS submission: same job type, model, and tier. A
		// mismatch means the buyer is acting on a price they were not given — refuse it.
		if q.JobType != sub.JobType.Type || q.ModelRef != sub.Model.Ref || q.Tier != sub.Tier {
			return JobSubmitResponse{}, &httpError{http.StatusConflict, "quote does not match this submission"}
		}
		if sub.JobType.Type == audioUploadJobType {
			if q.InputSHA256 == "" {
				return JobSubmitResponse{}, &httpError{http.StatusConflict,
					"audio quote has no normalized input binding; request a new audio quote"}
			}
			if q.InputSHA256 != hex.EncodeToString(sub.audioAdmission.normalizedJSONLSHA256[:]) {
				return JobSubmitResponse{}, &httpError{http.StatusConflict,
					"quote does not match this audio submission: normalized input changed since quote"}
			}
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
		// Firm-quote tier (Project Detection & Quotation 7->8): the quote's OWN
		// cost_max_usd — the conservative top of the band the buyer was already
		// shown at quote time (buildQuote's Budget.SuggestedMaxUSD) — becomes a real
		// ceiling, not just a suggestion. A quote with no positive max (a pre-D7 row,
		// or a degenerate zero-cost quote) cannot back a real commitment; refuse
		// rather than silently firm-committing to a meaningless $0 cap.
		if sub.FirmQuote {
			if q.CostMaxUSD <= 0 {
				return JobSubmitResponse{}, &httpError{http.StatusConflict, "quote has no positive cost_max_usd to firm-commit to"}
			}
			firmQuoteMaxUSD = q.EconomicPlan.ReservedBuyerChargeUSD
			// Speed-SLA binding (Speed Lane wave 2A): a firm submission against an
			// SLA-bearing quote binds the TIME guarantee alongside the price cap —
			// one commitment package, exactly as offered (the quote's sla block
			// showed both the guarantee and the premium). The committed price
			// ceiling grows by exactly the quoted premium: the cap the buyer was
			// shown covered the work; the SLA surcharge is priced on top of it,
			// never squeezed out of it. A quote without an SLA offer binds
			// price-only, byte-identical to before this wave.
			//
			// Deliberately NOT mapped onto deadline_secs: the deadline drives the
			// stuck-run watchdog's rescue→KILL ladder (workers.go reapStuckJobs),
			// and a missed SLA must COMPLETE and REFUND — killing a late job would
			// destroy the buyer's results to punish lateness. The guarantee is a
			// money remedy (collect.go settleSLAOutcome), enforced at finalize; the
			// watchdog keeps its own independent ETA-derived geometry.
			if q.SLAGuaranteedSecs > 0 && q.SLAPremiumUSD > 0 {
				slaGuaranteeSecs = q.SLAGuaranteedSecs
				slaPremiumUSD = q.SLAPremiumUSD
				// The reserved plan maximum already includes the once-only premium.
				firmQuoteMaxUSD = q.EconomicPlan.ReservedBuyerChargeUSD
			}
		}
		qBind = q
	}

	// Resolve the input as a STREAM (Data Transfer & Artifact I/O 7->8): fromKey is
	// non-empty when the input already lives in object storage (we then skip
	// re-uploading the canonical copy). The caller (this function) owns closing it.
	inputReader, srcKey, err := s.resolveInput(ctx, buyerID, sub.Input)
	if err != nil {
		return JobSubmitResponse{}, &httpError{http.StatusBadRequest, "resolving input: " + err.Error()}
	}
	defer inputReader.Close()

	// splitSize: an explicit params.split_size always wins. Otherwise adaptiveSplitSize
	// wants an avgLineBytes estimate, which for a STREAMED input we can only get from a
	// bounded look-ahead sample (peekInputSample), not the whole object — the whole
	// point of streaming is to never require the full size up front. This is a sizing
	// HEURISTIC only (adaptiveSplitSize just targets ~45s/task), never a correctness
	// requirement, so a sample-based estimate is an honest trade, not a silent
	// regression: see peekInputSample.
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
				// The sample consumed the WHOLE stream, so the record count is
				// EXACT — only then may the planner's width floor below be
				// applied (a partial sample cannot honestly bound chunk count).
				totalRecords = scan.Records
			}
		}
		splitSize = adaptiveSplitSize(sub.JobType.Type, sub.Params, avgLineBytes)
		// Speed Lane wave 1B (planner.go): refine the static-map size with the
		// LIVE fleet's measured rates, and floor the chunk count at the
		// planner's recommended fan-out width so the width is actually
		// achievable. Falls back to the static size untouched whenever the
		// rate cache is thin, the type is non-generative, or the planner is
		// disabled — never a silent guess.
		splitSize = s.adaptiveSplitSizeLive(ctx, sub.JobType.Type, sub.Model.Ref,
			sub.Constraints.MinMemoryGB, sub.JobType.MaxTokens, avgLineBytes, splitSize, totalRecords)
	}

	jobID := uuid.New()
	inputKey := srcKey
	var canonicalWriter io.Writer
	var canonicalPut *streamingPut
	if inputKey == "" {
		// Upload the canonical job input only when it came inline. Streamed via an
		// io.Pipe alongside the chunk split below (streamSplitAndUpload tees into
		// this writer) so the canonical copy is never separately buffered either.
		inputKey = fmt.Sprintf("jobs/%s/input.jsonl", jobID)
		canonicalPut = newStreamingPut(ctx, s.storage, inputKey, "application/x-ndjson")
		canonicalWriter = canonicalPut.writer
	}

	// Stream-split the input into per-task chunk objects, uploading them
	// CONCURRENTLY through a bounded errgroup (~16 in flight) instead of one
	// whole-buffer read + serial PutObject per chunk (Data Transfer & Artifact I/O
	// 7->8 / Scalability Headroom 7->8, "Stream the control-plane storage layer end
	// to end" / "Remove the artifact-size ceiling"). See streamSplitAndUpload.
	tasks, totalBytes, totalRecords, exactInputBytes, sum256, serr := s.streamSplitAndUpload(ctx, jobID, inputReader, splitSize, canonicalWriter)
	if canonicalPut != nil {
		canonicalPut.writer.Close() // signal EOF to the tee goroutine regardless of serr
		if perr := canonicalPut.wait(); perr != nil && serr == nil {
			serr = perr
		}
	}
	if serr != nil {
		return JobSubmitResponse{}, &httpError{http.StatusInternalServerError, "splitting/uploading input: " + serr.Error()}
	}
	if len(tasks) == 0 {
		return JobSubmitResponse{}, &httpError{http.StatusBadRequest, "input is empty: at least one JSONL line is required"}
	}
	nPrimary := len(tasks) // primaries precede any redundancy/honeypot clones
	inputSHA256 := hex.EncodeToString(sum256[:])
	if sub.JobType.Type == audioUploadJobType {
		if totalRecords != 1 || nPrimary != 1 || sub.audioAdmission == nil || sum256 != sub.audioAdmission.normalizedJSONLSHA256 {
			return JobSubmitResponse{}, &httpError{http.StatusBadRequest,
				"normalized audio input no longer matches its single-record server admission"}
		}
	}

	var boundQuoteID uuid.UUID
	if qBind != nil {
		// Best-effort: confirm the bytes match what was scanned at quote time. We only
		// reject when BOTH sides have a fingerprint and they differ (a pre-D7 quote with
		// no stored sha still binds, leaning permissive rather than blocking older quotes).
		// See the long comment above qBind's cheap pre-checks for why this ONE check
		// unavoidably runs after the chunks are already written on the streamed path.
		if qBind.InputSHA256 != "" && qBind.InputSHA256 != inputSHA256 {
			return JobSubmitResponse{}, &httpError{http.StatusConflict, "quote does not match this submission: input changed since the quote"}
		}
		boundQuoteID = qBind.ID
	}

	outputKey := fmt.Sprintf("jobs/%s/output.jsonl", jobID)

	// tasks/nPrimary/inputKey/inputSHA256 were already produced above by
	// streamSplitAndUpload — one object per chunk, task.InputRef the chunk key,
	// ResultKey its result target — uploaded CONCURRENTLY through a bounded
	// errgroup instead of the old whole-buffer-then-serial-PutObject loop.
	//
	// SECURITY: every task's result_key is keyed by that task's own opaque UUID
	// (jobs/{job}/tasks/{taskID}/result.json), with NO "honeypots/" or
	// "redundancy/" path segment and no revealing sequential index. A worker only
	// ever sees its own presigned GET/PUT URLs (PresignGet(c.InputRef) /
	// PresignPut(c.ResultKey) below in pollDispatch), so if the key shape or
	// substrings differed by task kind, a worker could fingerprint honeypot/
	// redundancy tasks from the URL alone and pass every probe while cheating
	// elsewhere — the exact hole a prior audit found here. Primary, redundancy,
	// and honeypot tasks must stay byte-for-byte indistinguishable in their
	// storage addressing; only the DB's is_honeypot/is_redundancy columns (never
	// sent to the worker, see the pollDispatch NOTE below) may know the type.

	// Redundancy tasks: a same-class peer for redundancy_frac of the primaries.
	// Each clones a primary's input chunk so PeerResultKey can pair them by
	// shared input_ref, and reuses that primary's chunk_index. The result_key is
	// a fresh opaque task UUID — same shape as a primary's, never "redundancy/".
	//
	// WHICH primaries get a peer is chosen by a keyed hash of (jobID, that
	// primary's own task UUID) — NOT "the first nRedundancy primaries in chunk
	// order". The old ordinal selection was deterministic in a way nothing about
	// task addressing hides: chunk 0..k of every job always got a peer and the
	// rest never did, so a pattern-watching supplier could infer redundancy
	// assignment from submission order alone even though the URL/key shape
	// itself is opaque (the prior audit's fix). Both jobID and every task's UUID
	// are freshly random per submission, so this hash is unpredictable ahead of
	// time to anyone who does not already know the task IDs — which workers
	// never do until they are dispatched one. Deterministic given those inputs
	// (so tests/replays are reproducible), not ordinal.
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
			ResultKey:             fmt.Sprintf("jobs/%s/tasks/%s/result.json", jobID, taskID),
			ChunkIndex:            p.ChunkIndex,
			ExpectedOutputRecords: p.ExpectedOutputRecords,
		})
	}

	// Honeypot tasks: pull available known-answer SEED honeypots for this job type
	// and inject them as probes (not buyer output — they reuse the matching
	// primary's chunk_index and are excluded from the merge by is_honeypot).
	//
	// SECURITY — the honeypot input-GET leak (Verification & Result Trust 5->5.5,
	// docs/internal/CREED_AND_PATH_TO_TEN.md). The result-PUT path was already
	// opaque (result_key is a per-task UUID; see the SECURITY note above). But the
	// GET/input side still leaked: a seed honeypot's object key is "honeypots/..."
	// (seed.go, Store.InsertHoneypot), and pollDispatch presigns the task's
	// input_ref verbatim into input_url — so a worker inspecting its OWN presigned
	// input URL would find the literal substring "honeypots/" and could ace every
	// probe while cheating on real chunks, defeating the entire trust engine.
	//
	// Fix: copy each seed honeypot's input BYTES to a per-task, job-namespaced
	// OPAQUE key — "jobs/{job}/tasks/{taskID}/input.jsonl", byte-for-byte the same
	// shape streamSplitAndUpload gives a primary chunk (and a redundancy clone
	// reuses) — and point the task's input_ref at THAT. The worker's presigned GET
	// is now indistinguishable from a normal chunk's on the wire. To keep the
	// verifier's GetHoneypotAnswer(job_type, input_ref) lookup working — it keys on
	// the task's input_ref, now the opaque key — we register the opaque key as a
	// honeypots-table ALIAS carrying the same known answer + class
	// (RegisterHoneypotAlias). AvailableSeedHoneypots deliberately reads only
	// "honeypots/..."-keyed seeds, never these "jobs/..."-keyed aliases, so an
	// alias is never re-dispatched as a honeypot for a future job.
	nHoneypot := fracCount(nPrimary, sub.Verification.HoneypotFrac)
	if wantVerificationFloor && nHoneypot == 0 {
		// A real minimum COUNT, not a fraction — guarantees at least one
		// honeypot task even for a single-chunk job, where any small
		// percentage floor would itself round back down to zero.
		nHoneypot = 1
	}
	if nHoneypot > 0 {
		// Pass the job's model + max_tokens so the injection-time param/model guard
		// (AvailableSeedHoneypots) only draws a byte-exact seed honeypot for a job it
		// is actually byte-valid for: a batch_infer job on a DIFFERENT model, or with
		// max_tokens below the seed's captured floor, would make an HONEST same-class
		// worker produce different bytes and get wrongly quarantined. A tolerant seed
		// (NULL bounds) is unaffected — it still matches on job_type alone.
		hps, herr := s.store.AvailableSeedHoneypots(ctx, sub.JobType.Type, sub.Model.Ref, sub.JobType.MaxTokens, nHoneypot)
		if herr != nil {
			return JobSubmitResponse{}, &httpError{http.StatusInternalServerError, "loading honeypots: " + herr.Error()}
		}
		for i, hp := range hps {
			taskID := uuid.New()
			// Opaque per-task input key, identical in shape to a primary chunk's.
			opaqueKey := fmt.Sprintf("jobs/%s/tasks/%s/input.jsonl", jobID, taskID)
			// Copy the seed honeypot's real input bytes to the opaque key so the
			// worker's presigned GET serves the same probe content under a
			// non-revealing address.
			inputBytes, gerr := s.storage.GetObject(ctx, hp.InputRef)
			if gerr != nil {
				// A honeypot whose input object is missing cannot be dispatched
				// safely (the worker's GET would 404 and it would retry forever —
				// the exact real bug seed.go's storage upload closed). Skip this one
				// rather than inject a broken probe; coverage is best-effort.
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
			// Register the opaque key as an alias so verification's answer lookup by
			// input_ref still resolves — same answer + class as the seed honeypot.
			if aerr := s.store.RegisterHoneypotAlias(ctx, sub.JobType.Type, opaqueKey, hp.KnownAnswer, hp.AnswerClass); aerr != nil {
				return JobSubmitResponse{}, &httpError{http.StatusInternalServerError, "registering honeypot alias: " + aerr.Error()}
			}
			tasks = append(tasks, taskRow{
				ID:                    taskID,
				JobID:                 jobID,
				IsHoneypot:            true,
				InputRef:              opaqueKey,
				ResultKey:             fmt.Sprintf("jobs/%s/tasks/%s/result.json", jobID, taskID),
				ChunkIndex:            i % nPrimary,
				ExpectedOutputRecords: int64(expectedRecords),
			})
		}
	}

	// Estimate cost from DB-backed model pricing × unit count. totalRecords (not
	// the chunk count nPrimary) is the real per-record unit count the generative
	// output-token term prices against; sub.JobType.MaxTokens drives that term for
	// batch_infer/json_extraction (Project Detection & Quotation 6->6.5).
	// Independent margin guard. Use exact raw streamed bytes (the quote scans the
	// same raw bytes), then scale base compute across the exact initial task set.
	// The refundable SLA premium is deliberately NOT distributed through task
	// payout math; it is a once-only job charge at successful completion.
	basePrimaryCompute := s.estimateSubmissionUSD(ctx, sub, exactInputBytes, totalRecords)
	if sub.PrivatePool {
		basePrimaryCompute = roundEconomicUSD(basePrimaryCompute + roundUSD(basePrimaryCompute*privatePoolPremiumRate))
	}
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
		// A non-firm submit does not buy the optional SLA, but the quote snapshot did
		// price its offer. Rebuild that exact offered plan solely for tamper/parity
		// validation, then build the execution plan from what this submit bound.
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
	// Persist the FULL submitted JobType (tag + labels/schema/max_tokens/...) so the
	// poll dispatch can reconstruct manifest.job_type for the agent, not just the tag.
	spec, _ := json.Marshal(sub.JobType)
	// offered_rate_usd_hr: a price-derived $/hr a worker earns running this job —
	// model price_per_1k × representative units/hr (see offeredRateUsdHr). The
	// claim's min-payout gate compares it to the worker's reservation price.
	offeredRate := s.offeredRateUsdHrForSubmission(ctx, sub)
	// eta_secs: a simple queue-depth/throughput estimate, model-aware so a
	// (job_type, model) with enough committed history uses its observed p90
	// per-task duration instead of the static target (Plane D D6). We call
	// etaBandSecs (the source estimateETASecs wraps) instead of estimateETASecs so
	// the SAME single call ALSO yields the planner's conservative band +
	// plannerBacked flag for the substrate-routing decision below — the p50 (eta)
	// is byte-identical to what estimateETASecs would have returned, so eta_secs is
	// unchanged.
	//
	// NOTE (quote-vs-submit ETA parity, deferred): the quote path additionally
	// derates this p50 to the sustained pace for long batch jobs
	// (quote.go: sustainedBatchETASecs), which createJob does NOT — so the quote
	// reads a longer ETA than the submit for the same input. Applying the derating
	// here is NOT the fix: the two paths ALSO compute different task counts (the
	// quote's scanned-sample split vs the submit's exact-stream split), so their
	// base p50 differs BEFORE any derating (measured: quote base ~137s vs submit
	// base ~182s for the same 500-record job). Deracting submit alone pushed it
	// FURTHER from the quote and into the wrong direction (submit slower than
	// quoted). Real parity requires unifying the split/task-count computation AND
	// the derating together — tracked as a separate task, not bolted on here.
	// Use the same effective memory floor as the exact supply/routing queries: a
	// buyer cannot lower a model's catalogue requirement by omitting the optional
	// constraint. The ETA fallback must count only workers that could claim this
	// exact tuple now.
	effectiveMinMem := sub.Constraints.MinMemoryGB
	var modelMinMem float32
	if m, merr := s.store.GetModel(ctx, sub.Model.Ref); merr == nil {
		modelMinMem = m.MinMemoryGB
		if modelMinMem > effectiveMinMem {
			effectiveMinMem = modelMinMem
		}
	}
	etaSecs, conservativeSecs, plannerBacked := s.etaBandSecs(
		ctx, sub.JobType.Type, sub.Model.Ref, effectiveMinMem, len(tasks),
	)

	// Substrate routing (Speed Lane road-to-ten rubric dimension 5, routing.go +
	// quote.go): read the job's SHAPE and say which substrate runs it fastest —
	// fleet, a lit GPU lane, or an honest GPU recommendation — grounded in the
	// measured 2026-07-06 A100 sweep. This MIRRORS quote.go's buildQuote routing
	// block EXACTLY: same generativeJobType + records>0 honesty guard (the sweep
	// measured generative decode only, so every other shape gets NO routing block
	// rather than an unmeasured guess), same DecideSubstrate inputs, same
	// QuoteRouting mapping and [MODELED] Basis label. avgLineBytes is EXACT here —
	// derived from the full-stream totalBytes/totalRecords the split pass already
	// produced (not the quote path's whole-input scan, but the same exact
	// per-record byte average) — so the routing block on the submitted job agrees
	// with what the quote showed for the same input. A nil routing block (the
	// non-generative / empty-input boundary) persists no columns and returns no
	// block, exactly as the quote omits it.
	var routing *QuoteRouting
	if generativeJobType(sub.JobType.Type) && totalRecords > 0 {
		// totalRecords > 0 is guaranteed by the guard above, so this is the exact
		// per-record byte average with no divide-by-zero guard needed.
		avgLineBytes := float64(totalBytes) / float64(totalRecords)
		// LIVE lit-lane supply (see quote.go's EligibleVLLMWorkerCount): real
		// online vLLM workers eligible for this job. Error → 0 (honest "no lit
		// lane"). Makes the submit path's routing block agree with the quote's.
		litGPU, _ := s.store.EligibleVLLMWorkerCount(ctx, sub.JobType.Type, sub.Model.Ref, effectiveMinMem)
		dec := DecideSubstrate(totalRecords, sub.Tier,
			routingModelClass(sub.Model.Ref, modelMinMem),
			tokensPerItemEstimate(sub.JobType.MaxTokens, avgLineBytes),
			etaSecs, conservativeSecs, plannerBacked, litGPU)
		routing = &QuoteRouting{
			Substrate:      dec.Substrate,
			Reason:         dec.Reason,
			FleetETASecs:   dec.FleetSecs,
			GPUModeledSecs: dec.GPUModeledSecs,
			Basis:          quoteRoutingBasis,
		}
	}

	// Generate the callback credential only after input processing/planning, then
	// pass only its sealed form into the atomic job transaction. A failed webhook
	// insert therefore cannot leave a buyer with a created job but no recoverable
	// callback credential.
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
		ID:                 jobID,
		BuyerID:            buyerID,
		JobType:            sub.JobType.Type,
		ModelRef:           sub.Model.Ref,
		InputRef:           inputKey,
		OutputRef:          outputKey,
		Tier:               sub.Tier,
		VerificationPolicy: vp,
		EstimatedUSD:       estimate,
		TaskCount:          len(tasks),
		// Persist the effective floor used by quote/supply routing, not merely the
		// buyer's optional override: omitting min_memory_gb cannot erase the model
		// catalogue requirement on the actual scheduler or agent dispatch.
		MinMemoryGB:                effectiveMinMem,
		MaxDurationSecs:            sub.Constraints.MaxDurationSecs,
		MinReputation:              sub.MinReputation,
		PrivatePool:                sub.PrivatePool,
		HWClasses:                  sub.Constraints.HWClasses,
		DataResidency:              sub.Constraints.DataResidency,
		JobTypeSpec:                spec,
		SplitSize:                  splitSize,
		OfferedRateUsdHr:           offeredRate,
		ETASecs:                    etaSecs,
		MaxUSD:                     sub.MaxUSD,   // Budget Governor cap (0 = none → persisted NULL)
		QuoteID:                    boundQuoteID, // D7 quote binding (zero = none → persisted NULL)
		DeadlineSecs:               sub.DeadlineSecs,
		FirmQuote:                  sub.FirmQuote,
		FirmQuoteMaxUSD:            firmQuoteMaxUSD,  // the real charge ceiling (0 = not firm → persisted NULL)
		SLAGuaranteeSecs:           slaGuaranteeSecs, // wave 2A time guarantee (0 = none → persisted NULL)
		SLAPremiumUSD:              slaPremiumUSD,    // wave 2A premium = the miss remedy (0 = none → NULL)
		EconomicInputRecords:       int64(totalRecords),
		EconomicInputBytes:         int64(exactInputBytes),
		EconomicInputSource:        economicInputSourceSubmitStream,
		EconomicPlan:               economicPlan,
		WebhookID:                  webhookRegistration.ID,
		WebhookURL:                 sub.WebhookURL,
		WebhookSigningSecretSealed: webhookSecretSealed,
	}
	// Persist the substrate-routing decision (rubric dimension 5) on the job row so
	// the clearing receipt can project it deterministically. All-or-nothing: a nil
	// routing block leaves every routing column NULL (the honesty boundary).
	if routing != nil {
		jr.RoutingSubstrate = routing.Substrate
		jr.RoutingReason = routing.Reason
		jr.RoutingFleetETASecs = routing.FleetETASecs
		jr.RoutingGPUModeledSecs = routing.GPUModeledSecs
	}
	if err := s.store.CreateJobWithTasks(ctx, jr, tasks); err != nil {
		return JobSubmitResponse{}, &httpError{http.StatusInternalServerError, "failed to create job: " + err.Error()}
	}

	metrics.jobsSubmitted.Add(1)

	// Open the buyer-visible event timeline (Plane C/D). Best-effort: a timeline
	// write must never fail an accepted job — log via the error return being ignored.
	_ = s.store.InsertJobEvent(ctx, jobID, nil, "job_created",
		fmt.Sprintf("Job created: %d tasks, model %s, %s tier", len(tasks), jr.ModelRef, sub.Tier), audioJobEventDetail(sub.audioAdmission))

	// Record the buyer's hard spend cap on the timeline (Budget Governor). Only when
	// a cap was set; best-effort like the job_created event.
	if sub.MaxUSD > 0 {
		_ = s.store.InsertJobEvent(ctx, jobID, nil, "budget_set",
			fmt.Sprintf("budget set: $%.2f cap", sub.MaxUSD), nil)
	}

	// Record the quote binding on the timeline (Plane D D7). Only when a quote was
	// bound; best-effort like the events above. The bare uuid is enough to trace the
	// invoice's quoted-vs-actual back to the originating quote.
	if boundQuoteID != (uuid.UUID{}) {
		_ = s.store.InsertJobEvent(ctx, jobID, nil, "quote_bound",
			fmt.Sprintf("bound to quote q_%s", boundQuoteID), nil)
	}

	// Record the speed-SLA binding on the timeline (wave 2A): the buyer sees the
	// guarantee clock and the remedy in their own event stream, not just in a
	// column. Best-effort like the events above.
	if slaGuaranteeSecs > 0 {
		_ = s.store.InsertJobEvent(ctx, jobID, nil, "sla_bound",
			fmt.Sprintf("Speed SLA bound: results guaranteed within %ds of submission · premium $%.6f is refunded automatically on a miss", slaGuaranteeSecs, slaPremiumUSD), nil)
	}

	// Record the substrate-routing decision on the timeline (rubric dimension 5):
	// the buyer sees "we ran it on X because Y" in their own event stream at
	// submit, not only on the receipt. Best-effort like the events above; only
	// when a routing block was actually decided (the generative + records>0
	// honesty boundary). The Reason carries the measured basis + [MODELED] label.
	if routing != nil {
		_ = s.store.InsertJobEvent(ctx, jobID, nil, "routed",
			fmt.Sprintf("routed to %s: %s", routing.Substrate, routing.Reason), nil)
	}

	// Estimated completion from the queue-depth/throughput ETA (priority work is
	// estimated to clear faster — see estimateETASecs), with a tier floor so the
	// human-facing RFC3339 timestamp stays sane.
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
		Routing:             routing, // nil for non-generative / empty-input (honesty boundary)
		AudioInput:          audioUploadMetadata(sub.audioAdmission),
	}
	if webhookRegistration.ID != uuid.Nil {
		response.WebhookID = webhookRegistration.ID.String()
		response.WebhookSecret = webhookRegistration.Secret
	}
	return response, nil
}

// jobsKeyPattern matches every object key this codebase ever generates under a
// job (jobs/{jobID}/input.jsonl, jobs/{jobID}/output.jsonl,
// jobs/{jobID}/tasks/{taskID}/...). Any s3_key a buyer legitimately owns has this
// shape — it is how resolveInput recovers which job (and therefore which buyer)
// produced the referenced object.
var jobsKeyPattern = regexp.MustCompile(`^jobs/([0-9a-fA-F-]{36})/`)

// resolveInput turns the polymorphic `input` field into a STREAM (Data Transfer &
// Artifact I/O 7->8, "Stream the control-plane storage layer end to end"). A JSON
// string IS the inline JSONL, wrapped in a reader with no extra copy beyond the
// []byte json.Unmarshal already produced; an object {"s3_key":"..."} opens a
// streaming GetObjectReader on the object and returns its key (so the caller
// skips re-uploading) WITHOUT ever buffering the whole object — the only thing
// that can be multi-GB on this path. Anything else is an error. The caller MUST
// Close the returned reader.
//
// SECURITY (Security Posture 6.5->7): an {"s3_key":...} reference is fetched only
// after confirming the key belongs to a job THIS buyerID submitted. Without this,
// any authenticated buyer could pass any other buyer's job_id (leaked via a
// webhook payload, a support ticket, a shared log line — job IDs are unguessable
// UUIDs, but "unguessable" is not the same as "never learned") in an s3_key and
// read that buyer's private input/output bytes — a real IDOR, not a theoretical
// one, since resolveInput previously fetched whatever key it was given with no
// ownership check at all. The legitimate use this preserves: a buyer chaining
// their OWN completed job's output into a new job's input.
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
		// Same message whether the job doesn't exist or belongs to someone else —
		// never confirm/deny another buyer's job_id to an unauthorized caller.
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

// handleAddPrivatePoolMember binds a supplier to the buyer's Private Deployment pool
// (research §3): thereafter only bound suppliers may claim that buyer's private_pool
// jobs. POST /v1/private-pool {"supplier_id":"<uuid>"} → 204. Idempotent.
func (s *Server) handleAddPrivatePoolMember(w http.ResponseWriter, r *http.Request) {
	auth := r.Context().Value(ctxBuyer).(*AuthResult)
	var body struct {
		SupplierID string `json:"supplier_id"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeErr(w, http.StatusBadRequest, "invalid json")
		return
	}
	sid, err := uuid.Parse(body.SupplierID)
	if err != nil {
		writeErr(w, http.StatusBadRequest, "supplier_id must be a uuid")
		return
	}
	if err := s.store.AddPrivatePoolMember(r.Context(), auth.BuyerID, sid); err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// handleListPrivatePoolMembers (GET /v1/private-pool) returns the buyer's own
// private-pool members (Buyer advantage & pricing edge 6->7: "Productize the
// privacy premium instead of leaving it a sentence") — the buyer-facing read side
// of the pool a quote's private_pool_member_count and a submission's dispatch
// filter both depend on, so a buyer can see WHO they are actually paying the
// premium to run on, not just an opaque database row.
func (s *Server) handleListPrivatePoolMembers(w http.ResponseWriter, r *http.Request) {
	auth := r.Context().Value(ctxBuyer).(*AuthResult)
	members, err := s.store.ListPrivatePoolMembers(r.Context(), auth.BuyerID)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, members)
}

// handleRemovePrivatePoolMember (DELETE /v1/private-pool/{id}) unbinds a supplier
// from the buyer's Private Deployment pool (Buyer advantage & pricing edge 6->7)
// — the real add/remove/list flow the rung asks for, not a one-way bind. 204,
// idempotent (removing a non-member is a no-op, matching Add's own idempotency).
func (s *Server) handleRemovePrivatePoolMember(w http.ResponseWriter, r *http.Request) {
	auth := r.Context().Value(ctxBuyer).(*AuthResult)
	sid, ok := pathUUID(w, r)
	if !ok {
		return
	}
	if err := s.store.RemovePrivatePoolMember(r.Context(), auth.BuyerID, sid); err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// handleJobResults returns a presigned GET of the merged buyer-ready artifact for
// a completed job, with the per-task result URLs as a fallback list. The merge is
// normally written by the completion sweep before the job is marked complete; if
// the merged object is not yet present (e.g. the buyer polls in the gap between
// the last commit and the next sweep), it is merged on read so the buyer always
// gets the single artifact. Every URL is a real time-limited signed URL.
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
	// Merge into the single buyer-ready artifact and presign it. Once
	// results_merged_at is set, a real successful merge has already run since
	// completion (either finalizeJobIfDone synchronously before marking the job
	// complete, or a prior read here) and output_ref already holds the current
	// buyer-ready artifact, so skip re-merging (Data Transfer & Artifact I/O
	// 4.5->5, "Stop paying for every poll twice" — a buyer polling repeatedly
	// after completion no longer re-fetches and re-writes every primary result on
	// every single poll). When it is NOT set (a legacy job from before this
	// migration, or the rare gap where completion raced ahead of the merge),
	// fall back to merging on read exactly as before: the sweep/finalize path is
	// best-effort timing, this stays the correctness guarantee.
	if j.OutputRef != "" {
		if j.ResultsMergedAt == nil {
			if _, merr := s.MergeJobResults(ctx, j.ID); merr != nil {
				// Surface a merge failure (e.g. a malformed result object) rather than
				// hand back a fallback that hides the problem.
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

// MergeJobResults assembles a job's completed PRIMARY task results into ONE
// buyer-ready JSONL artifact and writes it to the job's output_ref. Each primary
// task's result object is fetched in chunk_index order (so the merged file reads
// in the buyer's original input order) and flattened to one JSON line per input
// item, with the per-job-type shape:
//
//	embed                → {"index":<global>,"vector":[...]}
//	batch_classification → {"index":<global>,"label":"..."}
//	rerank               → {"index":<global>,"order":[...]}
//	json_extraction      → the extracted object, with an "index" field stamped in
//	batch_infer / other  → the task's per-item record passed through (or, when the
//	                       result is not the documented JSONL-of-items shape, the
//	                       raw result object as a single line — never dropped)
//
// A primary whose object is missing or malformed is surfaced as an error (the
// merge is the deliverable; a silent gap would hand the buyer a short file).
// Returns the byte count written. Called by the completion sweep BEFORE the job
// is marked complete, and on read as the correctness fallback.
func (s *Server) MergeJobResults(ctx context.Context, jobID uuid.UUID) (int, error) {
	return mergeJobResults(ctx, s.store, s.storage, jobID)
}

// embedBinMagic marks a Computexchange binary embedding artifact (PLANE_D D5/D15).
// The agent writes it (agent/src/runners.rs EMBED_BIN_MAGIC) when an embed job opts
// into compact float32 output; the merge detects these 4 bytes to keep the chunk on
// the binary path instead of JSON-parsing it. Layout: [magic|version u32|dim u32|
// count u32| count*dim packed little-endian f32], all little-endian.
var embedBinMagic = []byte("CXEM")

// embedBinHeaderLen is the fixed binary embedding header size (magic+version+dim+count).
const embedBinHeaderLen = 16

// embedBinVersion is the binary embedding format version this build reads/writes.
const embedBinVersion = uint32(1)

// isEmbedBinary reports whether obj is a binary embedding artifact (magic prefix).
func isEmbedBinary(obj []byte) bool {
	return len(obj) >= 4 && bytes.Equal(obj[:4], embedBinMagic)
}

// mergeJobResults is the shared merge used by both the results handler (*Server)
// and the completion sweep (*Workers): it fetches the job's ordered primary
// results and writes ONE buyer-ready artifact to the job's output_ref. For every
// JSON job (the default) it flattens results to a JSONL file exactly as before. For
// an embed job that opted into the binary float32 artifact (PLANE_D D5/D15) it
// detects the magic prefix and does a real BINARY merge (one combined header +
// concatenated row bodies, row order preserved by chunk_index) so the output is a
// single valid binary embedding file the SDK reader decodes — never a binary blob
// spliced into a JSONL file. Surfaces any missing/malformed result loudly.
func mergeJobResults(ctx context.Context, store *Store, storage *Storage, jobID uuid.UUID) (int, error) {
	return mergeJobResultsWithProbe(ctx, store, storage, jobID, nil)
}

// mergeJobResultsWithProbe is the crash-testable production implementation.
// The ordinary server and worker paths always pass a nil probe through the
// wrapper above; integration recovery tests stop the process only at the stable
// object-write/readback/database-publication edges below.
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

	// Assemble the buyer artifact on disk, retaining only one bounded source chunk
	// in memory.  The previous [][]byte + bytes.Buffer shape multiplied memory by
	// chunk count and allowed concurrent completion sweeps to exhaust the process.
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
	// A successful PUT response is not publication authority. Read the object
	// back from storage and compare its exact byte count and digest with the
	// disk-backed artifact we just assembled. This catches truncated, stale, or
	// otherwise changed bytes before results_merged_at tells a buyer they exist.
	if err := verifyMergedObject(ctx, storage, info.OutputRef, tmp, outputBytes); err != nil {
		return 0, err
	}
	reachRecoveryBoundary(ctx, probe, BoundaryMergeAfterVerify)
	metrics.resultMerges.Add(1)
	// Stamp the watermark now that the bytes are durably written: a later
	// GET /v1/jobs/{id}/results poll can trust results_merged_at and skip
	// re-merging (Data Transfer & Artifact I/O 4.5->5). The merge itself
	// already succeeded, so a watermark write failure is still surfaced (never
	// silently swallowed) but never hides that the artifact is already good.
	reachRecoveryBoundary(ctx, probe, BoundaryMergeBeforePublish)
	if err := store.MarkResultsMerged(ctx, jobID, outputRecords, outputBytes); err != nil {
		return int(outputBytes), fmt.Errorf("merge: writing output %q succeeded but marking results_merged_at failed: %w", info.OutputRef, err)
	}
	reachRecoveryBoundary(ctx, probe, BoundaryMergeAfterPublish)
	return int(outputBytes), nil
}

// verifyMergedObject proves that the fixed output key contains exactly the file
// we intended to publish. Both sides are streamed through SHA-256, so the check
// remains constant-memory even for a large buyer artifact.
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
		// Explicit pre-verification-work compatibility only.  It is bounded by the
		// same absolute cap and participates in the global byte budget, but remains
		// non-authoritative: new commits always merge a server-sealed tuple.
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

// mergeEmbedBinary concatenates per-chunk binary embedding artifacts (PLANE_D
// D5/D15) into ONE binary file: a single header carrying the summed row count and
// the shared dim, followed by every chunk's float body in chunk order (so rows read
// in the buyer's original input order). Every chunk must be a valid binary artifact
// of the SAME dim; a missing magic, a version we do not read, a size that disagrees
// with its header, or a dim mismatch is surfaced as an error — the merge is the
// deliverable, so a corrupt chunk must fail loudly, never silently shorten the file.
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

// mergeResultObject flattens one task result object into per-item JSONL lines on
// buf, returning the number of items emitted (so the caller can keep a running
// global index). It rejects a malformed object loudly rather than skipping it.
func mergeResultObject(buf *bytes.Buffer, jobType string, obj []byte, base int) (int, error) {
	return mergeResultObjectTo(buf, jobType, obj, base)
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
	case "batch_classification":
		var r classificationResult
		if err := json.Unmarshal(obj, &r); err != nil || len(r.Labels) == 0 {
			return 0, fmt.Errorf("not a valid batch_classification result")
		}
		for i, it := range r.Labels {
			if err := writeLine(map[string]any{"index": base + i, "label": it.Label}); err != nil {
				return 0, err
			}
		}
		return len(r.Labels), nil
	case "rerank":
		var r rerankResult
		if err := json.Unmarshal(obj, &r); err != nil || len(r.Rankings) == 0 {
			return 0, fmt.Errorf("not a valid rerank result")
		}
		for i, it := range r.Rankings {
			if err := writeLine(map[string]any{"index": base + i, "order": it.Order}); err != nil {
				return 0, err
			}
		}
		return len(r.Rankings), nil
	case "json_extraction":
		var r jsonExtractionResult
		if err := json.Unmarshal(obj, &r); err != nil || len(r.Items) == 0 {
			return 0, fmt.Errorf("not a valid json_extraction result")
		}
		for i, it := range r.Items {
			// Stamp the global index into the extracted object so the buyer can
			// realign lines with their input even after chunk concatenation.
			var m map[string]json.RawMessage
			if err := json.Unmarshal(it.JSON, &m); err != nil {
				return 0, fmt.Errorf("item %d not a JSON object", it.Index)
			}
			ib, _ := json.Marshal(base + i)
			m["index"] = ib
			if err := writeLine(m); err != nil {
				return 0, err
			}
		}
		return len(r.Items), nil
	default:
		// batch_infer and any other type: the agent writes one completion per input
		// item. The documented shape is {"completions":[...]} (or {"items":[...]});
		// when present we flatten it, otherwise we pass the whole object through as a
		// single line so nothing is ever silently dropped.
		var r struct {
			Completions []json.RawMessage `json:"completions"`
			Items       []json.RawMessage `json:"items"`
		}
		if err := json.Unmarshal(obj, &r); err == nil {
			list := r.Completions
			if list == nil {
				list = r.Items
			}
			if len(list) > 0 {
				for _, c := range list {
					if err := writeBytes(bytes.TrimSpace(c)); err != nil {
						return 0, err
					}
					if err := writeBytes([]byte{'\n'}); err != nil {
						return 0, err
					}
				}
				return len(list), nil
			}
		}
		if err := writeBytes(bytes.TrimSpace(obj)); err != nil {
			return 0, err
		}
		if err := writeBytes([]byte{'\n'}); err != nil {
			return 0, err
		}
		return 1, nil
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
		writeErr(w, http.StatusConflict, "job not found or already started")
		return
	}
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "cancelled"})
}

// handleModels serves the DB-backed model + pricing catalogue (the single
// source of truth — no static Go list). Prices come straight from the models
// table.
func (s *Server) handleModels(w http.ResponseWriter, r *http.Request) {
	rows, err := s.store.ListModels(r.Context())
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	out := make([]ModelInfo, 0, len(rows))
	for _, m := range rows {
		// Postgres may carry priced rows for hardware-pending work. The public
		// catalog advertises only models that participate in a generated production
		// runtime cell; a seed row cannot promote itself into buyer-visible supply.
		if !advertisedRuntimeModel(m.ID) {
			continue
		}
		info := ModelInfo{
			ID:          m.ID,
			Kind:        m.Kind,
			MinMemoryGB: m.MinMemoryGB,
			JobType:     m.JobType,
		}
		if m.JobType == audioUploadJobType {
			info.PricePerAudioMinuteUSD = m.PricePerUnit
		} else {
			info.PricePer1KUSD = modelPrice(m)
		}
		out = append(out, info)
	}
	writeJSON(w, http.StatusOK, out)
}

// handlePriceEstimate estimates cost from query params: model, units, tier. The
// price comes from the DB models table, not a static list.
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
	if m.JobType == audioUploadJobType {
		writeErr(w, http.StatusBadRequest, "audio estimates require server-derived WAV duration; use POST /v1/audio/jobs/quote")
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

// handleRegisterWebhook persists a completion-webhook registration. The webhooks
// table is part of the schema, so this is real: it validates the URL, stores the
// row, and returns 201 truthfully (delivery is done by the background sweep).
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
		// Do not distinguish a missing job from another buyer's job.
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

// handleFileDispute records a buyer dispute against a completed job's result — the
// anti-defection / optimistic-verification primitive ROADMAP_STATUS flags as missing.
// It records the dispute (scoped to the buyer's OWN job; another buyer's job → 404)
// and emits a buyer-visible event. RESOLUTION by optimistic recompute (operator-level
// bisection / tolerance-aware FP verification — docs/PRODUCTION_AUDIT.md §2.3) is the
// frontier seam: surfaced here in the response, never faked.
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
	_ = json.NewDecoder(r.Body).Decode(&body) // reason is optional
	id, err := s.store.RecordDispute(r.Context(), jobID, auth.BuyerID, body.Reason)
	if errors.Is(err, errNotFound) {
		writeErr(w, http.StatusNotFound, "job not found")
		return
	}
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "recording dispute: "+err.Error())
		return
	}
	_ = s.store.InsertJobEvent(r.Context(), jobID, nil, "dispute_filed",
		"Buyer filed a dispute on this job's result", nil)
	writeJSON(w, http.StatusAccepted, map[string]any{
		"dispute_id": id,
		"status":     "open",
		"note": "Dispute recorded. Resolution by optimistic recompute (operator-level bisection / " +
			"tolerance-aware FP verification) is the frontier seam (docs/PRODUCTION_AUDIT.md §2.3) and is not yet auto-resolved.",
	})
}

// --- worker handlers ---

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
	// Normalize + validate the engine (the second verification-class axis). An
	// absent/blank engine (an older agent that does not advertise it) normalizes to
	// the wired Candle default, so a single-engine fleet's (hw_class, engine) class
	// collapses to hw_class — exactly today's behavior. An UNKNOWN non-blank engine
	// is rejected rather than silently stored, so the closed set stays meaningful and
	// a typo never opens a one-worker verification class by accident.
	cap.Engine = normalizeEngine(cap.Engine)
	if !validEngines[cap.Engine] {
		writeErr(w, http.StatusBadRequest, "invalid engine: "+cap.Engine)
		return
	}
	if err := validateWorkerRuntimeProjection(cap); err != nil {
		writeErr(w, http.StatusBadRequest, "runtime capability rejected: "+err.Error())
		return
	}
	// build_hash remains an opaque agent-computed class tag rather than a closed-set
	// server value. Runtime admission nevertheless bounds its byte length, UTF-8,
	// and control characters so it cannot become an unbounded/log-forging identity.
	// Blank remains "unknown build" and receives only provisional trust.
	// Bind the capability to the authenticated worker/supplier — never trust
	// the body's ids over the token's.
	cap.WorkerID = auth.WorkerID
	cap.SupplierID = auth.SupplierID
	if err := s.store.UpsertWorker(r.Context(), cap); err != nil {
		writeErr(w, http.StatusInternalServerError, "register failed: "+err.Error())
		return
	}
	// Echo the capability back with the server-bound worker_id/supplier_id. The
	// agent deserializes this response into its WorkerCapability to confirm the
	// binding, so it must be the full object, not a status envelope.
	writeJSON(w, http.StatusOK, cap)
}

func (s *Server) handleWorkerHeartbeat(w http.ResponseWriter, r *http.Request) {
	auth := r.Context().Value(ctxWorker).(*WorkerAuth)
	var hb Heartbeat
	if err := json.NewDecoder(r.Body).Decode(&hb); err != nil {
		writeErr(w, http.StatusBadRequest, "invalid heartbeat json")
		return
	}
	if err := validateHeartbeatRuntimeModels(hb.LoadedModels); err != nil {
		writeErr(w, http.StatusBadRequest, "runtime heartbeat rejected: "+err.Error())
		return
	}
	// Refresh liveness + the live resource state the safe-dispatch filter reads.
	if err := s.store.HeartbeatWorker(r.Context(), auth.WorkerID, WorkerResources{
		AvailableMemoryGB:  hb.AvailableMemoryGB,
		EffectiveMemoryGB:  hb.EffectiveMemoryGB,
		ReservedHeadroomGB: hb.ReservedHeadroomGB,
		Throttled:          hb.Throttled,
		LoadedModels:       hb.LoadedModels,
	}); err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// longPollCap bounds the server-side wait so a misbehaving (or hostile) wait_ms
// can never pin a request goroutine indefinitely. The agent's transport ceiling
// is 35s (protocol.rs POLL_TIMEOUT), so 25s leaves headroom for the final claim +
// response to land before the client times out (Plane D §7 D1).
const longPollCap = 25 * time.Second

// longPollInterval is the FALLBACK re-attempt cadence while waiting — a safety net
// for a missed pg_notify (a brief LISTEN connection drop; see notify.go), not the
// primary wake mechanism. It used to be the only mechanism (every idle long-poll
// re-attempted ClaimTask every 250ms regardless of whether work existed); now the
// real wake is taskWake's broadcast on the tasks table's notify trigger, so this
// interval is deliberately generous — a real notify almost always fires first.
const longPollInterval = 5 * time.Second

// parseWaitMs reads the optional ?wait_ms long-poll budget, clamped to
// [0, longPollCap]. A missing, empty, malformed, or non-positive value yields 0 —
// the original single-shot poll (no wait) — so the param is purely additive and an
// older client that never sends it is unaffected.
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

// claimWithWait is ClaimTask with an optional long-poll wait (Plane D §7 D1). With
// wait<=0 it is a single ClaimTask — identical to the pre-long-poll behavior. With
// wait>0 and nothing immediately claimable, it re-attempts ClaimTask on every real
// wake (taskWake's broadcast — see notify.go — fired by a Postgres trigger the
// instant a task is inserted, requeued, hedged, or rescued) or, as a fallback safety
// net for a missed notification, every longPollInterval, until a task is found or
// the wait elapses, then returns (nil, nil) for the caller's 204. Each attempt is
// its own short-lived transaction (the wait never holds a DB transaction open). ctx
// (the request context) is honored throughout, so a client disconnect aborts the
// wait immediately. A timed-out empty return bumps metrics.longPollTimeouts;
// errNotFound (unregistered worker) and any real claim error surface at once
// without waiting.
func (s *Server) claimWithWait(ctx context.Context, auth WorkerAuth, wait time.Duration) (*ClaimedTask, error) {
	c, err := s.store.ClaimTask(ctx, auth)
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
			// Client hung up (or the server is shutting down): stop waiting. Not a
			// timeout — no task was withheld, so do not count it as one.
			return nil, ctx.Err()
		case <-deadline.C:
			metrics.longPollTimeouts.Add(1)
			return nil, nil
		case <-taskWake.Wait():
			c, err := s.store.ClaimTask(ctx, auth)
			if err != nil || c != nil {
				return c, err
			}
		case <-tick.C:
			c, err := s.store.ClaimTask(ctx, auth)
			if err != nil || c != nil {
				return c, err
			}
		}
	}
}

// checkpointableJobTypes are the job types whose dispatch carries a
// partial_put_url (shared wire contract): per-record batch work where a mid-chunk
// checkpoint document is meaningful. Other types (embeddings finish in seconds;
// custom runtimes are opaque) are deliberately excluded — old agents ignore the
// field either way, so the contract stays fully backward compatible.
var checkpointableJobTypes = map[string]bool{
	"batch_infer":          true,
	"batch_classification": true,
	"json_extraction":      true,
}

// claimedTaskConstraints reconstructs the buyer-authored scheduling/runtime
// contract from the columns ClaimTask returned. Keeping this as one explicit
// projection prevents the dispatch manifest from silently falling back to the
// zero-value constraints after the scheduler already enforced the real values.
func claimedTaskConstraints(c *ClaimedTask) JobConstraints {
	return JobConstraints{
		MinMemoryGB:     c.MinMemoryGB,
		HWClasses:       append([]string(nil), c.HWClasses...),
		MaxDurationSecs: c.MaxDurationSecs,
		DataResidency:   append([]string(nil), c.DataResidency...),
	}
}

// handleWorkerPoll claims the next eligible task via the SKIP-LOCKED queue and
// returns it as a TaskDispatch, or 204 when nothing is available.
//
// Long-poll (Plane D §7 D1): an optional ?wait_ms=N turns an otherwise-empty poll
// into a server-side wait (see claimWithWait) — re-attempting the claim until a
// task appears or wait_ms (capped at longPollCap) elapses. No wait_ms keeps the
// original single-shot behavior, so an older agent is unaffected.
func (s *Server) handleWorkerPoll(w http.ResponseWriter, r *http.Request) {
	auth := r.Context().Value(ctxWorker).(*WorkerAuth)
	ctx := r.Context()
	c, err := s.claimWithWait(ctx, *auth, parseWaitMs(r))
	if errors.Is(err, errNotFound) {
		writeErr(w, http.StatusForbidden, "worker not registered — call /v1/worker/register first")
		return
	}
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		// The client disconnected (or its deadline passed) mid-wait. There is no one
		// left to answer; just stop. Writing a body would race a closed connection.
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

	// NOTE: a honeypot is NOT signalled to the worker, and its expected answer is
	// never sent. The server knows the task is a honeypot (from the DB) and verifies
	// the worker's uploaded result against the stored known answer in verifyTaskResult.
	// Leaking is_honeypot/honeypot_ans would let a hostile worker ace every probe.

	// Presign the input for download and the result key for upload. The agent
	// fetches input_url, runs the task, and PUTs its result to output_url (the
	// result_key presigned). result_key is the canonical key it echoes in commit.
	inputURL, err := s.storage.PresignGet(ctx, c.InputRef, 15*time.Minute)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "presign input: "+err.Error())
		return
	}
	outputURL, err := s.storage.PresignPut(ctx, c.ResultKey, time.Hour)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "presign result: "+err.Error())
		return
	}
	// Intra-task checkpointing (shared wire contract with the agent): for the
	// checkpointable batch job types, also presign a PUT for result_key+".partial"
	// (same expiry as the result presign) so the agent MAY periodically upload a
	// mid-chunk partial result document. The final commit is UNCHANGED (full result
	// to output_url; byte-determinism unaffected); partials are never merged and
	// never affect money — the watchdog's kill path merely hands the buyer GET URLs
	// to the ones that exist. Best-effort: a presign failure here degrades only the
	// optional checkpoint, so it is logged and the dispatch proceeds without it.
	partialPutURL := ""
	if checkpointableJobTypes[c.JobType] {
		if u, perr := s.storage.PresignPut(ctx, c.ResultKey+".partial", time.Hour); perr == nil {
			partialPutURL = u
		} else {
			log.Printf("poll: presign partial for task %s: %v (dispatched without checkpoint URL)", c.TaskID, perr)
		}
	}

	// Reconstruct a minimal manifest for the dispatch from the stored job
	// fields. The verification policy round-trips from jsonb.
	var vp VerificationPolicy
	_ = json.Unmarshal(c.VerifPolicy, &vp)
	// Reconstruct the FULL job_type from the stored spec so the agent receives the
	// buyer's params (labels/schema/max_tokens/temperature), not just the bare tag.
	// Falls back to {"type":tag} when no spec was persisted (older jobs / null).
	jt := JobType{Type: c.JobType}
	if len(c.JobTypeSpec) > 0 && string(c.JobTypeSpec) != "null" {
		var parsed JobType
		if err := json.Unmarshal(c.JobTypeSpec, &parsed); err == nil && parsed.Type != "" {
			jt = parsed
		}
	}
	disp := TaskDispatch{
		TaskID:           c.TaskID,
		JobID:            c.JobID,
		RuntimeCellID:    c.RuntimeCellID,
		RuntimeID:        c.RuntimeID,
		RuntimeMatrixSHA: c.RuntimeMatrixSHA,
		Manifest: JobManifest{
			ID:      c.JobID,
			JobType: jt,
			// The kind comes from the catalog row frozen by ClaimTask, never a
			// hardcoded backend guess. The exact runtime tuple above is the stronger
			// execution gate; kind remains the agent runner's coarse wire guard.
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
	err := s.store.StartTask(r.Context(), id, auth.WorkerID)
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

// handleWorkerCommit stores the result, fetches the committed result bytes from
// object storage, runs real verification (honeypot / redundancy with
// embedding-aware comparison), and on a clean pass writes the ledger entries with
// the payout hold.
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
	c.TaskID = id // trust the path, not the body

	info, err := s.store.commitTask(ctx, id, auth.WorkerID, c, s.verification.probe)
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
		// Job merge/completion is itself restartable via the completion sweep. Keep
		// the low-latency synchronous attempt, but correctness no longer depends on
		// this request surviving after the task transaction commits.
		if err := s.finalizeJobIfDone(ctx, info.JobID); err != nil {
			writeErr(w, http.StatusInternalServerError, "finalizing job: "+err.Error())
			return
		}
	}
	w.WriteHeader(http.StatusNoContent)
}

// finalizeJobIfDone finalizes a job synchronously when the just-committed task
// was its last: merge the buyer-ready artifact, THEN mark the job complete and
// settle actual_usd. Merge-before-mark guarantees a buyer never sees
// status=complete with a missing or short merged output. A not-yet-done job is a
// clean no-op (the background sweep finalizes it later); a merge failure is
// returned so the commit surfaces it (never marked complete on bad output).
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
	if err := s.store.CompleteJobEconomics(ctx, jobID); err != nil {
		return err
	}
	// Speed-SLA outcome (wave 2A): decided HERE — after the merge stamped
	// results_merged_at (the guarantee clock's stop) and actual_usd settled (the
	// refund cap), and BEFORE the charge decision below so a miss's refund nets
	// the very first collection. Idempotent (SettleJobSLA stamps sla_met once);
	// a no-op for jobs without a bound SLA. Best-effort: a settle error is
	// logged and retried by the collect sweep, never fails the finalize.
	settleSLAOutcome(ctx, s.store, jobID)
	// Feed the ETA calibration loop (predicted vs realized; best-effort).
	recordEtaCalibration(ctx, s.store, jobID)
	// Best-effort external charge: gated on Stripe + a saved card, idempotent by
	// job id. A no-op (and unchanged lifecycle) when billing isn't configured.
	s.chargeForJob(ctx, jobID)
	s.advanceIntake(ctx, jobID)   // multi-stage chain: no-op unless this job is an intake stage
	s.advancePipeline(ctx, jobID) // user-defined pipeline chain: no-op unless this job is a pipeline stage
	return nil
}

// scheduleTaskPayout writes the buyer_charge / supplier_credit / platform_take
// ledger rows for a completed task, using the immutable task-level buyer charge
// and supplier payout plus the policy's payout hold.
func (s *Server) scheduleTaskPayout(ctx context.Context, info *CommitTaskInfo) error {
	entries, err := s.taskPayoutEntries(ctx, info)
	if err != nil {
		return err
	}
	return s.store.InsertLedgerEntries(ctx, entries)
}

// taskPayoutEntries computes, but does not write, the task's money rows. The
// normal commit path hands these to FinalizeTaskVerification so verdict,
// completion, telemetry, and settlement are one transaction. scheduleTaskPayout
// remains for dispute/test repair paths that intentionally settle an existing task.
func (s *Server) taskPayoutEntries(ctx context.Context, info *CommitTaskInfo) ([]LedgerEntry, error) {
	return s.taskPayoutEntriesAt(ctx, info, time.Now())
}

// taskPayoutEntriesAt freezes the payout-hold clock to a durable attempt time.
// Recovery workers pass verification_work.created_at, so a crash before the
// terminal transaction cannot re-plan a different release_at and therefore a
// different settlement digest. Legacy callers retain time.Now through the
// wrapper above.
func (s *Server) taskPayoutEntriesAt(ctx context.Context, info *CommitTaskInfo, now time.Time) ([]LedgerEntry, error) {
	j, err := s.store.getJobInternal(ctx, info.JobID)
	if err != nil {
		return nil, err
	}
	buyerCharge, supplierPayout, err := s.store.TaskEconomicAmounts(ctx, info.TaskID)
	if err != nil {
		return nil, err
	}
	if buyerCharge <= 0 || supplierPayout < 0 || supplierPayout > buyerCharge {
		return nil, fmt.Errorf("task %s has invalid frozen economics: buyer %.6f supplier %.6f", info.TaskID, buyerCharge, supplierPayout)
	}
	var vp VerificationPolicy
	_ = json.Unmarshal(j.VerificationPolicy, &vp)
	entries := splitFrozenCharge(j.BuyerID, info.SupplierID, info.TaskID, buyerCharge, supplierPayout, vp.PayoutHoldSecs, now)
	return entries, nil
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

// handleWorkerVerification (GET /v1/worker/verification) reports THIS supplier's
// own real honeypot pass/fail counts + derived label — the trust-panel data
// source (Supplier onboarding & safety 7->8: "Populate the trust panel with real
// data"). Polled each heartbeat by the agent alongside earnings + connect/status
// so agent/src/status.rs can populate honeypots_passed/failed/verification_label
// instead of leaving them permanently absent.
func (s *Server) handleWorkerVerification(w http.ResponseWriter, r *http.Request) {
	auth := r.Context().Value(ctxWorker).(*WorkerAuth)
	v, err := s.store.SupplierVerification(r.Context(), auth.SupplierID)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, v)
}

// --- admin handlers ---

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

// handleAdminFraud surfaces the richer fraud report: per-supplier reputation,
// tier, status, quarantine timestamp, and confirmed-fraud clawback/mismatch
// counts — the auto-quarantine (Verification V2) review queue.
func (s *Server) handleAdminFraud(w http.ResponseWriter, r *http.Request) {
	f, err := s.store.ListFraud(r.Context())
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, f)
}

// handleAdminDrift surfaces the per-(job_type, model) quoted-vs-actual rollup
// (Plane D D6 / errata C-Errata-6): observed avg + p90 committed-task duration, the
// sample count behind it, and the average quoted whole-job eta_secs — so an operator
// can see how the static quote tracks reality and which slices have enough history
// that their observed p90 is now driving the ETA.
func (s *Server) handleAdminDrift(w http.ResponseWriter, r *http.Request) {
	d, err := s.store.DriftRollup(r.Context())
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, d)
}

// handleAdminQuoteDrift (GET /admin/quotes) is the COST-drift twin of
// handleAdminDrift, which is ETA-only. Project Detection & Quotation 6.5->7
// (docs/internal/CREED_AND_PATH_TO_TEN.md): "the quoted-vs-charged learning loop
// is ETA-only: the drift data lands in Postgres but is never rolled up or fed back
// into prices, and the specced GET /admin/quotes surface doesn't exist." This
// rolls up real quotes.cost_expected_usd vs real jobs.actual_usd per (job_type,
// model), so an operator can see exactly which slices of the catalogue are
// under- or over-priced relative to what jobs actually cost to run.
func (s *Server) handleAdminQuoteDrift(w http.ResponseWriter, r *http.Request) {
	d, err := s.store.CostDriftRollup(r.Context())
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, d)
}

// handleAdminAutoTunePrices (POST /admin/quotes/auto-tune) is the "use it to
// auto-adjust catalogue prices" half of the same rung: it reads the real cost
// drift and nudges each sufficiently-sampled (job_type, model)'s catalogue price
// toward the drift-corrected value (clamped, see autoTuneMaxAdjustmentFrac). An
// explicit admin-triggered action, not a silent background loop — a price change
// this consequential gets an operator's deliberate act and a real response body
// naming exactly what changed, not an invisible cron.
func (s *Server) handleAdminAutoTunePrices(w http.ResponseWriter, r *http.Request) {
	applied, err := s.store.AutoTunePrices(r.Context())
	if err != nil {
		var unavailable *PriceTuningUnavailableError
		if errors.As(err, &unavailable) {
			writeJSON(w, http.StatusConflict, map[string]any{
				"error":              unavailable.Error(),
				"reason":             unavailable.Reason,
				"actual_usd_basis":   unavailable.ActualUSDBasis,
				"required_telemetry": unavailable.RequiredTelemetry,
			})
			return
		}
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"tuned": applied, "count": len(applied)})
}

// handleAdminSchedulerExplain answers "why is this worker getting no work?" (Plane D
// §17 D11). Many "slow" workers are not slow at all — there is simply nothing
// ELIGIBLE for them in the queue. For the worker in ?worker_id=, it runs the SAME
// hard-filter predicates as ClaimTask against every currently-claimable task and
// returns per-reason COUNTS (memory/model/job-type/hw-class/residency/throttle/payout/
// supplier gates) plus how many tasks it COULD claim (eligible). Read-only: it never
// claims and never mutates. 400 on a missing/malformed worker_id, 404 when the worker
// is unregistered.
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

// handleAdminJobs lists recent jobs across all buyers (admin-only).
func (s *Server) handleAdminJobs(w http.ResponseWriter, r *http.Request) {
	j, err := s.store.ListJobsAdmin(r.Context())
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, j)
}

// handleAdminPayouts surfaces the per-supplier payout rollup across the complete
// lifecycle, including awaiting/ready/sending/unknown/carried/export/reversal
// states and operation-backed cash/anomaly counts.
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
	err := s.store.SuspendWorker(r.Context(), id)
	if errors.Is(err, errNotFound) {
		writeErr(w, http.StatusNotFound, "worker not found")
		return
	}
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "suspended"})
}

// adminActionBody is the shared request shape for the audited admin write
// endpoints below: an optional free-text reason, and (adjust-reputation only) a
// numeric delta. reason is optional (an operator working a real incident should
// never be BLOCKED from acting because they didn't type a sentence first) but is
// always recorded — empty if omitted — so the audit trail is honest about what
// was and wasn't explained.
type adminActionBody struct {
	Reason string  `json:"reason,omitempty"`
	Delta  float32 `json:"delta,omitempty"`
}

// decodeAdminActionBody reads an optional JSON body; a missing/empty body is NOT
// an error (reason is optional), but malformed JSON in a present body is a real
// 400 rather than being silently ignored.
func decodeAdminActionBody(r *http.Request) (adminActionBody, error) {
	var b adminActionBody
	if r.ContentLength == 0 {
		return b, nil
	}
	if err := json.NewDecoder(r.Body).Decode(&b); err != nil && !errors.Is(err, io.EOF) {
		return b, err
	}
	return b, nil
}

// handleAdminReinstate (POST /admin/workers/{id}/reinstate) closes the "reinstate
// after review" half of RUNBOOKS.md's Bad/fraudulent worker procedure (Operator
// Tooling 7->8): previously a raw
// `psql -c "UPDATE suppliers SET status='active', quarantined_at=NULL ..."`.
// 409 (not 500) when the worker's supplier isn't currently suspended — reinstating
// an already-active supplier is a no-op an operator should be told about, not a
// silent success that hides a typo'd worker id.
func (s *Server) handleAdminReinstate(w http.ResponseWriter, r *http.Request) {
	id, ok := pathUUID(w, r)
	if !ok {
		return
	}
	if _, err := decodeAdminActionBody(r); err != nil {
		writeErr(w, http.StatusBadRequest, "invalid request json: "+err.Error())
		return
	}
	err := s.store.ReinstateWorker(r.Context(), id)
	if errors.Is(err, errNotFound) {
		writeErr(w, http.StatusNotFound, "worker not found")
		return
	}
	if errors.Is(err, errNotSuspended) {
		writeErr(w, http.StatusConflict, "worker's supplier is not currently suspended")
		return
	}
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "active"})
}

// handleAdminRequeueTask (POST /admin/tasks/{id}/requeue) closes the "Stuck job"
// runbook's manual fix (Operator Tooling 7->8): previously a raw
// `psql -c "UPDATE tasks SET status='queued', claimed_by=NULL, visible_at=now() ..."`.
// 409 when the task isn't in a requeueable state (running/retrying) — matching the
// runbook's own scope, so this can never resurrect a complete/failed/cancelled task.
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
	err = s.store.AdminForceRequeueTask(r.Context(), id, body.Reason)
	if errors.Is(err, errNotFound) {
		writeErr(w, http.StatusNotFound, "task not found")
		return
	}
	if errors.Is(err, errNotRequeueable) {
		writeErr(w, http.StatusConflict, "task is not running/retrying — nothing to requeue")
		return
	}
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "queued"})
}

// handleAdminAdjustReputation (POST /admin/suppliers/{id}/reputation) closes the
// "manually adjust a supplier's reputation with an audit trail" gap named
// directly in the backlog rung (Operator Tooling 7->8). Body: {"delta": ±float,
// "reason": "..."}. delta=0 is rejected (a no-op reputation "adjustment" is
// almost certainly a caller mistake, and would otherwise write a confusing
// before==after audit row); reason is optional but recommended.
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
	before, after, err := s.store.AdminAdjustReputation(r.Context(), id, body.Delta, body.Reason)
	if errors.Is(err, errNotFound) {
		writeErr(w, http.StatusNotFound, "supplier not found")
		return
	}
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"before": before, "after": after})
}

// handleAdminReleasePayout (POST /admin/payouts/{id}/release) closes the
// "manually trigger a payout-hold release" gap named directly in the backlog
// rung (Operator Tooling 7->8): previously the operator would reach for a raw
// UPDATE against ledger_entries. Accepts an entry that is currently 'held' OR
// 'ready' (409 for any other status — pending/released/clawed_back) and always
// leaves it 'held' with release_at advanced to now() — it never marks the entry
// 'released' itself (that requires a real payout_ref, per MarkPayout's own
// invariant); the existing release-worker sweep (DuePayouts) picks it up on its
// very next cycle.
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
	err = s.store.AdminReleasePayoutHold(r.Context(), id, body.Reason)
	if errors.Is(err, errNotFound) {
		writeErr(w, http.StatusNotFound, "ledger entry not found")
		return
	}
	if errors.Is(err, errNotHeld) {
		writeErr(w, http.StatusConflict, "ledger entry is not currently held or ready")
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

// decodeMoneyAuthorityJSON is intentionally stricter and much smaller than the
// ordinary API body cap: these requests mint authority over platform money. It
// rejects oversized input, unknown fields, malformed JSON, and any second/trailing
// JSON value before a store method can run.
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

// handleAdminCreateSubsidyFund records a finite operator-declared treasury pool.
// The external reference is not independent bank reconciliation, but capacity is
// immutable and every later reservation is transactionally capped by it.
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

// handleAdminSubsidizePayout creates an audited, exact-cent platform-fund
// authorization for one supplier liability. It is deliberately separate from
// hold release: authorizing a subsidy neither shortens the 24h floor nor claims
// that supplier cash moved. ClaimPayout still performs the sending CAS and the
// rail result still must prove exact cash movement.
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

// handleAdminActions (GET /admin/actions) is the audit-log review surface for
// every admin write action above: who did what, when, and (if given) why.
func (s *Server) handleAdminActions(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Pragma", "no-cache")
	limit := 0
	if raw := strings.TrimSpace(r.URL.Query().Get("limit")); raw != "" {
		parsed, err := strconv.Atoi(raw)
		if err != nil {
			writeErr(w, http.StatusBadRequest, "invalid admin action limit")
			return
		}
		limit = parsed
	}
	actions, err := s.store.ListAdminActionsPage(
		r.Context(), limit, strings.TrimSpace(r.URL.Query().Get("cursor")))
	if errors.Is(err, errAdminActionReviewLimit) || errors.Is(err, errAdminActionReviewCursor) {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, actions)
}

// handleJobInvoice returns the buyer-facing invoice for one job (scoped to the
// authenticated buyer): estimated vs actual charge plus the realized ledger
// breakdown (supplier credit, platform take).
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

// handleJobReceipt returns the ClearingReceipt (items 13-15): ONE buyer-scoped projection
// joining the invoice (quote + actuals + settlement), the verification receipt (counts +
// label + dispute), and the verification classes that produced the results. JobInvoice is
// buyer-scoped, so it doubles as the ownership gate (a buyer sees only their own jobs).
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
	writeJSON(w, http.StatusOK, assembleClearingReceipt(id, inv.Status, inv, verif, classes, tasks, receiptRouting(inv)))
}

// secureHTMLHeaders sets defensive headers on same-origin HTML responses:
// anti-MIME-sniff, anti-clickjacking (same-origin framing only), no referrer
// leakage. Deliberately NO Content-Security-Policy — the /admin console and /demo
// use inline <script> and fetch presigned object-store URLs, which a strict CSP
// would break.
func secureHTMLHeaders(w http.ResponseWriter) {
	h := w.Header()
	h.Set("X-Content-Type-Options", "nosniff")
	h.Set("X-Frame-Options", "SAMEORIGIN")
	h.Set("Referrer-Policy", "no-referrer")
}

// handleRoot serves the public informational site at the bare domain. One page,
// no signup, no email capture · every claim on it is receipted in
// docs/SITE-CLAIMS.md. Path is web/index.html, overridable via SITE_PATH; a
// missing file is a clear 404, never a faked page. The operator surface stays at
// /admin; buyers still submit via the API/CLI/SDK, the page only informs.
func (s *Server) handleRoot(w http.ResponseWriter, r *http.Request) {
	path := os.Getenv("SITE_PATH")
	if path == "" {
		path = "web/index.html"
	}
	b, err := os.ReadFile(path)
	if err != nil {
		writeErr(w, http.StatusNotFound, "site not found at "+path+" (set SITE_PATH)")
		return
	}
	secureHTMLHeaders(w)
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	_, _ = w.Write(b)
}

// handleSiteAsset serves the static assets the public site references: the oracle
// renders and their srcset variants, the foam maps, the glb of the two devices, and
// the self-hosted Three.js vendor bundle. It is deliberately NOT a general file
// server: it is scoped to one directory tree (web/assets/site, overridable via
// SITE_ASSETS_PATH), rejects any traversal, and only serves a fixed extension
// whitelist. Anything else is 404.
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
	// Resolve inside root and confirm the cleaned absolute path stays within it,
	// so a crafted `path` can never escape the asset tree.
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
	// Content-hashed asset names (name-<8+ hex>.ext) never change · cache them a year,
	// immutable. Everything else gets a modest day so an unhashed swap propagates.
	if siteAssetHashed(rel) {
		h.Set("Cache-Control", "public, max-age=31536000, immutable")
	} else {
		h.Set("Cache-Control", "public, max-age=86400")
	}
	// Serve a brotli-precompressed sibling (built by render/site/precompress.sh) when
	// the client accepts it and the .br exists · text assets shrink a lot, and the glb
	// is already entropy-dense so it is left uncompressed (measured, not assumed).
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

// brotliOK reports whether the client accepts brotli.
func brotliOK(r *http.Request) bool {
	for _, tok := range strings.Split(r.Header.Get("Accept-Encoding"), ",") {
		if strings.TrimSpace(strings.SplitN(tok, ";", 2)[0]) == "br" {
			return true
		}
	}
	return false
}

// siteAssetHashed reports whether a filename carries a content hash (stem ends in
// -<8 or more hex digits>), so it can be cached immutably for a year.
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

// siteAssetType validates a site-asset request path and returns its content type.
// The path may contain forward-slash subdirs (vendor/addons/...), letters (the
// vendored GLTFLoader.js has capitals), digits, dash, underscore, @, dot, and slash · no "..", no leading slash, no
// backslash, no other characters. Only the listed extensions are served.
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

// demoAssetTypes is the complete public asset contract for /demo. The console
// intentionally doesn't get a general /web or /assets file server: only the
// files referenced by the shipped HTML are reachable, and Dockerfile.control
// copies this exact set into the production image.
var demoAssetTypes = map[string]string{
	"btn-add-payment-shell@3x.png": "image/png",
	"btn-launch-shell@3x.png":      "image/png",
	"cx-mark-white.png":            "image/png",
	"dot-ring@3x.png":              "image/png",
	"knob-off@3x.png":              "image/png",
	"knob-on@3x.png":               "image/png",
	"knob-pressed@3x.png":          "image/png",
	"knob-red@3x.png":              "image/png",
}

// handleDemoAsset serves the fixed, flat image allowlist used by /demo. A path
// containing a slash, traversal, or a name outside demoAssetTypes is always 404.
func (s *Server) handleDemoAsset(w http.ResponseWriter, r *http.Request) {
	rel := r.PathValue("path")
	ctype, ok := demoAssetTypes[rel]
	if !ok || strings.ContainsAny(rel, `/\`) {
		writeErr(w, http.StatusNotFound, "no such demo asset")
		return
	}
	root := os.Getenv("DEMO_ASSETS_PATH")
	if root == "" {
		root = "web/assets"
	}
	b, err := os.ReadFile(filepath.Join(root, rel))
	if err != nil {
		writeErr(w, http.StatusNotFound, "no such demo asset")
		return
	}
	h := w.Header()
	h.Set("X-Content-Type-Options", "nosniff")
	h.Set("Content-Type", ctype)
	h.Set("Cache-Control", "public, max-age=86400")
	_, _ = w.Write(b)
}

// handleFavicon serves the existing favicon so the site and console do not 404
// the browser's automatic request. Same read-from-disk mechanism as the pages.
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

// handleAdminPage serves the passkey-gated operator panel at /admin. The HTML shell
// itself is public (it just renders the passkey sign-in and, once authed, fetches the
// admin data routes, which enforce auth server-side). Path is web/admin.html,
// overridable via ADMIN_PATH; a missing file is a clear 404, never a faked page.
func (s *Server) handleAdminPage(w http.ResponseWriter, r *http.Request) {
	path := os.Getenv("ADMIN_PATH")
	if path == "" {
		path = "web/admin.html"
	}
	b, err := os.ReadFile(path)
	if err != nil {
		writeErr(w, http.StatusNotFound, "admin panel not found at "+path+" (set ADMIN_PATH)")
		return
	}
	secureHTMLHeaders(w)
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	_, _ = w.Write(b)
}

// handleDemo serves the Launch/Earn product demo at /demo, same-origin with the
// API so its fetches need no CORS. Path is web/demo.html, overridable via
// DEMO_PATH; a missing file is a clear 404, never a faked page. It is a navigable
// monochrome prototype of the two windows (buyer Launch + seller Earn): it pings
// /healthz live and simulates the auditor / job / earnings flows.
func (s *Server) handleDemo(w http.ResponseWriter, r *http.Request) {
	path := os.Getenv("DEMO_PATH")
	if path == "" {
		path = "web/demo.html"
	}
	b, err := os.ReadFile(path)
	if err != nil {
		writeErr(w, http.StatusNotFound, "demo not found at "+path+" (set DEMO_PATH)")
		return
	}
	secureHTMLHeaders(w)
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	_, _ = w.Write(b)
}

// --- pricing + split helpers ---

// modelPrice returns a model's effective per-1K price. Most models price per
// 1,000 units (tokens/embeddings); some (e.g. audio transcription) price per
// discrete unit, in which case price_per_unit is scaled to a per-1K figure so a
// single number drives both the catalogue and the estimate.
func modelPrice(m ModelRow) float64 {
	if m.PricePer1K > 0 {
		return m.PricePer1K
	}
	return m.PricePerUnit * 1000.0
}

// tierMultiplier prices the service tiers: priority +50%, trusted +100%.
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

// generativeJobType reports whether a job type produces model-generated COMPLETION
// tokens whose length (max_tokens) is a real, separately-priced cost — as opposed
// to embed/classification/rerank, which emit a bounded, input-shaped result (a
// vector, a label, a ranking) whose size max_tokens does not drive. Only these two
// types get an expected-output-token cost term in estimateJobUSD (Project Detection
// & Quotation 6->6.5, docs/internal/CREED_AND_PATH_TO_TEN.md). audio_transcribe and
// image_gen are generative too but priced on a different basis (audio seconds /
// image steps), not JSONL max_tokens, so they are deliberately excluded here.
func generativeJobType(jobType string) bool {
	return jobType == "batch_infer" || jobType == "json_extraction"
}

// defaultQuoteMaxTokens is the completion length the quote ASSUMES for a
// generative job when the buyer sets no explicit max_tokens (the wire zero-value
// after omitempty). It mirrors the Rust agent's own fallback for a batch_infer
// job with no max_tokens (agent/src/runners.rs: `_ => 256`), so the quote's
// assumed output length matches what the worker would actually generate rather
// than pricing zero output for a job that will really emit ~256 tokens/record.
const defaultQuoteMaxTokens = 256

// estimateJobUSD estimates a job's pre-execution cost from the DB-backed model
// price and the unit count. Base units = JSONL lines (one record per line) for
// the per-1K pricing, with a byte-derived floor (~4 bytes/token) so a few very
// large records are not under-counted. actual_usd is set from the real ledger on
// completion; this is the honest up-front estimate.
//
// EXPECTED-OUTPUT-TOKEN COST (Project Detection & Quotation 6->6.5): for a
// GENERATIVE job type (batch_infer/json_extraction) the completion the model
// writes is real work the buyer pays for, and its length is driven by max_tokens.
// The old estimate ignored completion length entirely — max_tokens did not move
// the price at all, so a 16-token extraction and a 2048-token generation quoted
// identically. We now add an expected-output-token term: each of the nLines
// records is assumed to generate up to max_tokens completion tokens (falling back
// to defaultQuoteMaxTokens when the buyer omits it), and those output tokens are
// priced on the SAME per-1K catalogue basis as the input units — because for a
// generative model the catalogue's price_per_1k IS a per-1K-token price
// (control/pricing.go's repricing derives it from tok/s throughput). A tolerant
// (non-generative) job type is unchanged: maxTokens does not apply and adds
// nothing, so embed/classification/rerank quotes are byte-for-byte identical to
// before this rung.
func (s *Server) estimateJobUSD(ctx context.Context, jobType, modelRef string, inputBytesLen int, nLines int, maxTokens uint32, tier string) float64 {
	price := 0.002 // default per-1K when the model is unknown to the catalogue
	if m, err := s.store.GetModel(ctx, modelRef); err == nil {
		price = modelPrice(*m)
	}
	units := float64(nLines)
	if byteUnits := float64(inputBytesLen) / 4.0; byteUnits > units {
		units = byteUnits
	}
	// Expected output tokens for generative jobs: nLines records × the per-record
	// completion length (max_tokens, or the agent's own default when unset). These
	// are ADDED to the priced units so a longer max_tokens measurably raises the
	// quote — the real cost of a longer completion the old estimate dropped.
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

// splitSizeOf reads params.split_size (lines per task chunk), defaulting to
// defaultSplitSize when absent or non-positive.
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

// hasExplicitSplitSize reports whether params carries a positive split_size (the
// buyer override adaptiveSplitSize always defers to). Used by the streaming
// submit path to decide whether the avgLineBytes heuristic — which for a
// streamed input can only come from a bounded look-ahead sample, see
// peekInputSample — is even needed: an explicit split_size makes it moot.
func hasExplicitSplitSize(params json.RawMessage) bool {
	if len(params) == 0 {
		return false
	}
	var p struct {
		SplitSize int `json:"split_size"`
	}
	return json.Unmarshal(params, &p) == nil && p.SplitSize > 0
}

// jobTypeThroughput is the representative items processed per SECOND for one
// task of a job type on a typical worker. It drives both adaptive chunk sizing
// (items/chunk = throughput × targetTaskSecs) and the offered-rate estimate
// (units/hr = throughput × 3600). These are deliberate, documented sane defaults
// — not measured per-worker numbers — chosen so each task lands in ~30–60s.
// Embeddings are cheap per item (hundreds/s); token generation is dear (a handful
// of completions/s). Unknown types fall back to a conservative middle figure.
var jobTypeThroughput = map[string]float64{
	"embed":                200, // short texts → embeddings, very fast
	"batch_classification": 80,  // one classification per item, cheap
	"rerank":               40,  // a scored pass over a candidate list per item
	"json_extraction":      8,   // structured generation per item, dearer
	"batch_infer":          4,   // free-form completion per item, dearest
	"audio_transcribe":     2,   // realtime-ish per clip
	"image_gen":            0.2, // multi-step diffusion per image
}

// throughputOf returns a job type's items/sec, defaulting to a conservative 10.
func throughputOf(jobType string) float64 {
	if v, ok := jobTypeThroughput[jobType]; ok && v > 0 {
		return v
	}
	return 10
}

// effectiveThroughput scales a GENERATION job's items/sec down for long inputs.
// Classification / extraction / completion are prefill-bound: the model reads the
// whole prompt before emitting a token, so a 1.2 KB posting is far slower than a
// one-line tweet — but the base jobTypeThroughput figures assume short texts (the
// jobscraper real-project run estimated 90s vs ~108s actual for exactly this reason).
// We scale items/sec by the prompt-length ratio past a short-text reference. Embed /
// transcribe / rerank are not prompt-length-bound this way. avgLineBytes <= 0 means
// "unknown" → no scaling. Drift feedback (observed p90) refines further once history
// exists; this fixes the COLD-START estimate before any history is recorded.
func effectiveThroughput(jobType string, avgLineBytes float64) float64 {
	base := throughputOf(jobType)
	switch jobType {
	case "batch_classification", "json_extraction", "batch_infer":
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

// targetTaskSecs is the per-task duration adaptive chunk sizing aims for, so a
// task is big enough to amortize dispatch overhead but small enough to hedge and
// retry cheaply (~30–60s; we target the midpoint).
const targetTaskSecs = 45

// adaptiveSplitSize picks the JSONL lines-per-task. An explicit, positive
// params.split_size always wins (the buyer override). Otherwise the size is
// throughput(jobType) × targetTaskSecs, clamped to a sane [1,4096] band, so an
// embed job packs far more items per chunk than a generation job for the same
// ~45s target.
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

// adaptiveSplitSizeLive is the LIVE-FLEET-aware refinement of adaptiveSplitSize
// (Speed Lane wave 1B, planner.go): instead of the static jobTypeThroughput
// map's "typical worker", it sizes a chunk at ~targetTaskSecs of work for the
// MEDIAN live eligible worker's measured rate (worker_tps_cache), and then
// floors the resulting CHUNK COUNT at the fan-out planner's recommended width
// so the plan's parallelism is actually achievable (a job split into fewer
// chunks than the fleet can usefully run in parallel wastes idle capacity —
// the pre-wave failure mode for small generative jobs).
//
// staticSize (the adaptiveSplitSize result) is returned UNCHANGED — the honest
// fallback — whenever: the planner is disabled; the job type is not generative
// (worker_tps_cache tps is tokens/sec, which converts to items/sec via
// tokensPerItemEstimate only for token-generating types; embed/rerank rates
// live on a different unit axis and keep the static path — a documented
// boundary, not a silent mismatch); fewer than plannerMinFleetSamples live
// workers have a real measured rate; or the snapshot query fails (a sizing
// heuristic must never fail a submission). totalRecords is 0 when the input
// was streamed past the look-ahead sample (record count unknown) — the median
// sizing still applies but the width floor is skipped, honestly.
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
		return staticSize // rate cache too thin — static map remains in force
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

// plannerETASecs is the planner-backed ETA (Speed Lane wave 1B): the modeled
// makespan of (queuedAhead + nTasks) task-units across the live fleet's
// MEASURED heterogeneous rates, with a per-worker COLD-LOAD term
// (benchmark_results.load_ms where measured, the documented default
// otherwise) — the two things the blunt ceil(queue/workers)×perTaskSecs
// formula structurally cannot see (a mixed fleet's tail and a cold fleet's
// first minutes were systematically underestimated). Rates are RELATIVE:
// perTaskSecs (the drift-fed p90 or the static target) anchors the absolute
// scale — the median-rate worker completes one task-unit per perTaskSecs; a
// worker at 2× the median tps completes it in half that.
//
// Returns ok=false — and the caller keeps the pre-wave formula — when the
// planner is disabled, the snapshot fails, or fewer than
// plannerMinFleetSamples live workers have a measured rate for this job type.
//
// Speed Lane wave 2A: alongside the p50 it now also returns the plan's
// CONSERVATIVE band (the same assignment re-costed with every rate degraded to
// plannerConservativeRateFactor of measured — planner.go) so the speed-SLA
// quote can build its guarantee on the band, never on the p50. The band is
// floored at the p50 (defensive; by construction it is ≥).
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

// offeredRateUsdHr derives the $/hr a worker earns running this job from the
// DB-backed model price and the job type's representative throughput:
//
//	units/hr      = throughput(jobType) × 3600
//	$/hr          = (units/hr / 1000) × price_per_1k_usd
//
// This is the rate the claim's min-payout gate compares to the worker's
// reservation price. When the model is unknown to the catalogue we fall back to
// the same default per-1K price estimateJobUSD uses, so the gate is never zeroed.
func (s *Server) offeredRateUsdHr(ctx context.Context, jobType, modelRef string) float32 {
	price := 0.002
	if m, err := s.store.GetModel(ctx, modelRef); err == nil {
		if jobType == audioUploadJobType {
			// Duration is required to translate the clip-throughput target into
			// source-audio minutes/hour. The submission-aware wrapper below has
			// that authority; this duration-free fallback must not invent it.
			return 0
		}
		price = modelPrice(*m)
	}
	unitsPerHr := throughputOf(jobType) * 3600.0
	return float32(unitsPerHr / 1000.0 * price)
}

func (s *Server) offeredRateUsdHrForSubmission(ctx context.Context, sub jobSubmit) float32 {
	if sub.JobType.Type == audioUploadJobType {
		if sub.audioAdmission == nil {
			return 0
		}
		return audioOfferedRateUSDHr(
			sub.audioAdmission.facts,
			sub.audioAdmission.pricePerAudioMinute,
			throughputOf(audioUploadJobType),
		)
	}
	return s.offeredRateUsdHr(ctx, sub.JobType.Type, sub.Model.Ref)
}

// perTaskSecsFromP90 converts an observed p90 committed-task duration (ms) into the
// per-task seconds the ETA uses. p90ms <= 0 means "no trustworthy history" (the store
// returns 0 below the sample floor or on error), so we fall back to the static
// throughput target. A positive p90 rounds UP to whole seconds (a sub-second p90 still
// counts as one second of work). Pure — unit-tested without a DB.
func perTaskSecsFromP90(p90ms int64) int {
	if p90ms <= 0 {
		return targetTaskSecs // thin/empty history → static target (Plane D D6 fallback)
	}
	secs := int((p90ms + 999) / 1000)
	if secs < 1 {
		secs = 1
	}
	return secs
}

// estimateETASecs is a simple queue-depth/throughput estimate: the time for this
// job's tasks plus everything already queued ahead of them to drain across the
// live fleet. The p50-only wrapper over etaBandSecs below — its byte-identical p50
// is the conceptual anchor the surrounding comments (and quote.go's
// sustained-derating notes) refer to by name. createJob itself now calls
// etaBandSecs directly (it needs the conservative band + plannerBacked for the
// substrate-routing decision), so this accessor currently has no in-tree caller;
// it is retained as the documented single-value entry point rather than inlining
// the wrap at every future p50-only site.
func (s *Server) estimateETASecs(ctx context.Context, jobType, modelRef string, minMemGB float32, nTasks int) int {
	eta, _, _ := s.etaBandSecs(ctx, jobType, modelRef, minMemGB, nTasks)
	return eta
}

// etaBandSecs is estimateETASecs plus the planner's CONSERVATIVE band (Speed
// Lane wave 2A — the guarantee basis of the speed-SLA quote). perTaskSecs is
// the per-task duration: we use the OBSERVED p90 of past committed tasks for
// this (job_type, model_ref) once enough have been recorded (Plane D D6 drift
// feedback — the Exchange Brain learns reality), and fall back to the static
// throughput target when history is too thin (or a DB error). The queue depth
// and exact current-matrix eligible-worker count come from the store (a DB error
// degrades conservatively to one worker rather than failing the submission).
// Never returns eta < perTaskSecs (a job always takes at least one task's
// worth of time).
//
// plannerBacked=true iff the ETA is the planner's modeled makespan over REAL
// measured heterogeneous rates (wave 1B); only then is `conservative` a real
// band (rates degraded to 75% of measured, planner.go) a guarantee may be
// built on. plannerBacked=false → conservative is 0 and NO caller may promise
// anything: the pre-wave blunt formula is an aggregate, not a model of this
// fleet, and the speed-SLA never offers a guarantee on it (honest degradation).
func (s *Server) etaBandSecs(ctx context.Context, jobType, modelRef string, minMemGB float32, nTasks int) (eta, conservative int, plannerBacked bool) {
	// Drift feedback: prefer the observed p90 per-task duration once history is thick
	// enough (HistoricalP90DurationMs returns 0 below the sample floor, so a thin or
	// empty history — or a DB error — cleanly falls back to the static target).
	p90ms, _, err := s.store.HistoricalP90DurationMs(ctx, jobType, modelRef)
	if err != nil {
		p90ms = 0
	}
	perTaskSecs := perTaskSecsFromP90(p90ms)
	queued, _ := s.store.QueuedTaskCount(ctx)
	// Speed Lane wave 1B (planner.go): when the live fleet has enough measured
	// rates, the ETA is the planner's modeled makespan over HETEROGENEOUS
	// per-worker rates plus a real COLD-LOAD term — the two effects the wave
	// formula below cannot represent. Same perTaskSecs anchor, same queue term;
	// ok=false (thin cache / disabled / DB error) falls through to the
	// unchanged pre-wave formula.
	if eta, cons, ok := s.plannerETASecs(ctx, jobType, modelRef, minMemGB, nTasks, queued, perTaskSecs); ok {
		return eta, cons, true
	}
	// The blunt fallback still needs honest supply. A global liveness count admits
	// legacy array-only workers, stale-matrix registrations, and workers below this
	// job's memory floor into the divisor even though none can claim the task. Use
	// the same exact tuple + current matrix SHA + live resource predicate as quote
	// eligibility. Zero/error intentionally floors to one theoretical worker: it
	// yields a conservative serial ETA without pretending inert supply is capacity.
	workers, _ := s.store.EligibleWorkerCount(ctx, jobType, modelRef, minMemGB)
	if workers < 1 {
		workers = 1
	}
	// This job's tasks land behind whatever is already queued; spread across the
	// fleet. round up so a partial wave still counts as a full pass.
	ahead := queued // tasks already waiting
	totalTasks := ahead + nTasks
	waves := (totalTasks + workers - 1) / workers
	eta = waves * perTaskSecs
	if eta < perTaskSecs {
		eta = perTaskSecs
	}
	return eta, 0, false
}

// tierMinCompletion is the human-facing completion floor per tier, so the RFC3339
// estimated_completion is never absurdly near even when the queue is empty.
func tierMinCompletion(tier string) time.Duration {
	if tier == "priority" {
		return 5 * time.Minute
	}
	return 15 * time.Minute
}

// splitJSONL breaks JSONL bytes into chunks of at most n lines each, preserving
// line content (newline-terminated). Blank lines are dropped (they carry no
// record). The remainder forms a final, smaller chunk. Returns nil for empty
// input. n<=0 is treated as defaultSplitSize.
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

// inputSampleBytes bounds the look-ahead peekInputSample reads to estimate
// avgLineBytes for adaptiveSplitSize on a STREAMED input, when the buyer gave no
// explicit split_size. Large enough to see a representative mix of record sizes,
// small enough that it is never the memory cost driver even for a multi-GB input.
const inputSampleBytes = 1 << 20 // 1 MiB

// peekInputSample reads up to max bytes from r for a look-ahead sample, then
// returns that sample PLUS a reader that reproduces the full original stream
// (sample followed by whatever remains of r, via io.MultiReader) — so the
// caller can inspect the sample without losing a single byte of the real input.
// r is not closed; the returned rest wraps it in a NopCloser paired with the
// prepended sample, matching resolveInput's io.ReadCloser contract.
func peekInputSample(r io.ReadCloser, max int) (sample []byte, rest io.ReadCloser, err error) {
	buf := make([]byte, max)
	n, rerr := io.ReadFull(r, buf)
	if rerr != nil && rerr != io.ErrUnexpectedEOF && rerr != io.EOF {
		return nil, nil, rerr
	}
	sample = buf[:n]
	return sample, io.NopCloser(io.MultiReader(bytes.NewReader(sample), r)), nil
}

// streamingPut uploads an unknown-size stream to storage.PutObject via an
// io.Pipe, run on its own goroutine so createJob can tee the canonical input
// upload alongside the chunk split below without ever buffering it separately.
// size=-1 tells minio-go to use its own streaming multipart upload internally
// (see minio-go's PutObject), so this never buffers the whole canonical input
// in control-plane memory either.
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

// wait blocks for the upload goroutine to finish and returns its error.
func (sp *streamingPut) wait() error { return <-sp.done }

// streamSplitAndUpload reads input once, splitting it into <=splitSize-line
// JSONL chunks and uploading each as its own object CONCURRENTLY through a
// bounded errgroup (Data Transfer & Artifact I/O 7->8 / Scalability Headroom
// 7->8: "write submission chunks concurrently through a bounded errgroup (~16 in
// flight) instead of serially"). If canonicalTee is non-nil, every byte read is
// also written there verbatim (before line-splitting/trimming) so the canonical
// jobs/{jobID}/input.jsonl upload — when the input came inline rather than from
// an existing s3_key — reproduces the buyer's exact original bytes, the same
// contract PutObject(inputBytes) gave before this streamed. Returns the primary
// task rows (chunk_index 0-based, matching the input's line order), exact
// non-blank record count, record-payload bytes used by pricing, exact raw bytes
// consumed from the stream (including line endings/blank lines), and the sha256
// of those same exact bytes (for D7 quote-hash binding). Keeping the two byte
// counters distinct prevents the economics record from relabelling the pricing
// heuristic as an exact transport fact.
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

	// chunkIndex assigns each task its 0-based input position HERE, in the single
	// reader goroutine below — NOT by goroutine launch/completion order (errgroup's
	// bounded workers can finish out-of-order). tasks itself is only ever appended
	// to from this same reader goroutine (the grp.Go closures only PutObject; they
	// never touch tasks), so no lock is needed for it — the concurrency this
	// function bounds is entirely on the object-store WRITE side, not on this
	// bookkeeping.
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
			ResultKey:             fmt.Sprintf("jobs/%s/tasks/%s/result.json", jobID, taskID),
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
	// Order tasks by ChunkIndex: errgroup workers can finish (and append) out of
	// launch order even though chunkIndex assignment above was already correct —
	// the DB rows/keys are right regardless, but a stable, predictable ORDER in
	// the slice (matching the pre-streaming code's guarantee) costs one sort of an
	// already-small (task-count, not byte-size) slice.
	sort.Slice(tasks, func(i, j int) bool { return tasks[i].ChunkIndex < tasks[j].ChunkIndex })
	copy(sum256[:], hasher.Sum(nil))
	return tasks, totalBytes, totalRecords, exactInputBytes, sum256, nil
}

// countNonBlankJSONLRecords derives the exact record count from bytes already
// fetched for an injected honeypot. It mirrors streamSplitAndUpload's definition:
// each non-blank line is one input/output record, including a final line without
// a trailing newline.
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

// fracCount returns round(n * frac), clamped to [0, n]. Used to size honeypot
// and redundancy task counts from the policy fractions.
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

// redundancySelectionHash gives each primary task a stable, unpredictable-ahead-
// of-time rank for "does this primary get a redundancy peer" — sha256(jobID +
// taskID) truncated to a uint64. Deterministic given those two freshly-random
// UUIDs (so a fixed job replays identically), but NOT ordinal — it does not
// correlate with chunk position, so a supplier watching submission order alone
// cannot infer which chunks are more likely to get redundancy-checked.
func redundancySelectionHash(jobID, taskID uuid.UUID) uint64 {
	h := sha256.New()
	h.Write(jobID[:])
	h.Write(taskID[:])
	sum := h.Sum(nil)
	return binary.BigEndian.Uint64(sum[:8])
}

// pathUUID parses the {id} path value as a UUID, writing a 400 on failure.
func pathUUID(w http.ResponseWriter, r *http.Request) (uuid.UUID, bool) {
	id, err := uuid.Parse(r.PathValue("id"))
	if err != nil {
		writeErr(w, http.StatusBadRequest, "invalid id: must be a uuid")
		return uuid.Nil, false
	}
	return id, true
}

// writeJSON serializes v as a JSON response with the given status.
// --- buyer API-key lifecycle handlers (POST/GET/DELETE /v1/keys) ---

// handleCreateKey mints a new buyer API key and reveals the raw secret EXACTLY
// once (it is never stored · only its hash + a masked hint are). Body:
// {"name": string, "test": bool?}. The returned `key` is the only chance to copy
// the secret; subsequent GETs show only the masked form.
func (s *Server) handleCreateKey(w http.ResponseWriter, r *http.Request) {
	auth := r.Context().Value(ctxBuyer).(*AuthResult)
	var body struct {
		Name string `json:"name"`
		Test bool   `json:"test"`
	}
	// An empty/absent body is allowed (unnamed live key); only reject malformed JSON.
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

// handleListKeys returns the caller's keys as masked rows (never the raw secret).
func (s *Server) handleListKeys(w http.ResponseWriter, r *http.Request) {
	auth := r.Context().Value(ctxBuyer).(*AuthResult)
	keys, err := s.store.ListAPIKeys(r.Context(), auth.BuyerID)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "listing api keys: "+err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"keys": keys})
}

// handleRevokeKey revokes one of the caller's keys. Idempotent: returns 204
// whether or not the key existed for this buyer (revoking twice is a no-op),
// matching the DELETE contract. Scoped to the buyer so one cannot revoke another's.
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
		// Best-effort: the status is already written; log to stderr via the
		// caller is not possible here, so surface nothing more than the failed
		// encode (the client will see a truncated body).
		_ = err
	}
}

// writeErr writes a uniform JSON error body.
func writeErr(w http.ResponseWriter, status int, msg string) {
	writeJSON(w, status, APIError{Error: msg})
}
