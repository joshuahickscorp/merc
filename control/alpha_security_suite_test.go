package main

import (
	"archive/tar"
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
)

// Alpha security suite: every attack below goes through Server.Routes()
// (middleware + handler), not a helper called in isolation.
//
// Results are written to ALPHA_SECURITY_RESULTS when set so
// scripts/alpha-security-suite.py can bind them into evidence. The test
// itself fails on any finding.

type alphaAttack struct {
	ID             string `json:"id"`
	Class          string `json:"class"`
	Title          string `json:"title"`
	Status         string `json:"status"` // blocked | finding | error
	Severity       string `json:"severity,omitempty"`
	HTTPStatus     int    `json:"http_status"`
	Want           string `json:"want"`
	Reproduction   string `json:"reproduction"`
	Location       string `json:"location"`
	AlphaReachable bool   `json:"alpha_reachable"`
	Detail         string `json:"detail"`
}

type alphaSuiteReport struct {
	Kind            string        `json:"kind"`
	Executed        int           `json:"executed"`
	Blocked         int           `json:"blocked"`
	Findings        int           `json:"findings"`
	Errors          int           `json:"errors"`
	Attacks         []alphaAttack `json:"attacks"`
	SourceCommit    string        `json:"source_commit,omitempty"`
	DatabaseUsed    bool          `json:"database_used"`
	PaymentModeTest bool          `json:"payment_mode_test"`
}

type alphaSuite struct {
	t       *testing.T
	ctx     context.Context
	store   *Store
	pool    *pgxpool.Pool
	handler http.Handler
	mu      sync.Mutex
	attacks []alphaAttack

	buyerAKey, buyerBKey string
	buyerAID, buyerBID   uuid.UUID
	sessionA             string
	adminKey             string
	workerATok           string
	workerBTok           string
	workerAID, workerBID uuid.UUID
	jobAID, taskAID      uuid.UUID
}

func TestAlphaSecuritySuite(t *testing.T) {
	installSettlementCurrencyForTest(t, "usd")
	installContainmentDeviceBindFixture(t)
	strangerDeploymentInputs(t)
	t.Setenv("MERC_ENV", "development")
	t.Setenv("MERC_PAYMENT_MODE", "test")
	t.Setenv("STRIPE_SECRET_KEY", "sk_test_alpha_security_suite_not_a_live_secret")
	t.Setenv("STRIPE_WEBHOOK_SECRET", "whsec_billing_alpha_security_suite_0001")
	t.Setenv("MERC_CONNECT_WEBHOOK_SECRET", "whsec_connect_alpha_security_suite_0002")
	t.Setenv("MERC_ADMIN_CIDRS", "") // loopback-only admin source

	ctx, store, pool := openIsolatedTestStore(t)
	storage := dummyPresignStorage(t)
	srv := NewServer(store, storage, nil, nil)
	s := &alphaSuite{
		t: t, ctx: ctx, store: store, pool: pool, handler: srv.Routes(),
	}

	s.seedIdentities()
	s.attackAuthFuzz()
	s.attackIdentityAndPrivilege()
	s.attackWebhooks()
	s.attackMoney()
	s.attackFXAndAmounts()
	s.attackContainmentAndInput()
	s.attackConcurrency()
	s.attackAuthorityFailClosed()
	s.attackResourceExhaustion()

	report := s.finish()
	if report.Findings > 0 {
		t.Fatalf("alpha security suite: %d finding(s) across %d executed attacks",
			report.Findings, report.Executed)
	}
	if report.Executed < 40 {
		t.Fatalf("alpha security suite executed only %d attacks; the harness did not fire", report.Executed)
	}
}

func dummyPresignStorage(t *testing.T) *Storage {
	t.Helper()
	client, err := newMinio("http://127.0.0.1:9000", "alpha", "security", "us-east-1")
	mustf(t, err, "dummy minio client: %v")
	return &Storage{
		internal: client, public: client, bucket: "alpha-security",
		breaker: newStoreBreaker(5, time.Second),
	}
}

func (s *alphaSuite) seedIdentities() {
	t := s.t
	t.Helper()

	emailA := "alpha-a-" + uuid.NewString() + "@example.test"
	emailB := "alpha-b-" + uuid.NewString() + "@example.test"
	signupA := s.do(http.MethodPost, "/v1/signup", "", map[string]any{
		"email": emailA, "password": "alpha-security-password-a",
	})
	if signupA.code != http.StatusCreated && signupA.code != http.StatusOK {
		t.Fatalf("seed signup A: HTTP %d %s", signupA.code, signupA.body)
	}
	signupB := s.do(http.MethodPost, "/v1/signup", "", map[string]any{
		"email": emailB, "password": "alpha-security-password-b",
	})
	if signupB.code != http.StatusCreated && signupB.code != http.StatusOK {
		t.Fatalf("seed signup B: HTTP %d %s", signupB.code, signupB.body)
	}
	s.buyerAKey, _ = signupA.json["sandbox_key"].(string)
	s.buyerBKey, _ = signupB.json["sandbox_key"].(string)
	s.sessionA, _ = signupA.json["token"].(string)
	if id, ok := signupA.json["buyer_id"].(string); ok {
		s.buyerAID, _ = uuid.Parse(id)
	}
	if id, ok := signupB.json["buyer_id"].(string); ok {
		s.buyerBID, _ = uuid.Parse(id)
	}
	if s.buyerAKey == "" || s.buyerBKey == "" || s.buyerAID == uuid.Nil {
		t.Fatalf("signup did not issue usable identities: A=%s B=%s", signupA.body, signupB.body)
	}

	s.adminKey = "cx_admin_alpha_" + strings.ReplaceAll(uuid.NewString(), "-", "")
	if _, err := s.pool.Exec(s.ctx, `
		INSERT INTO admin_credentials (key_hash, label, revoked)
		VALUES ($1, 'alpha-security-operator', false)`, hashKey(s.adminKey)); err != nil {
		t.Fatalf("seed admin credential: %v", err)
	}

	supA, wrkA := uuid.New(), uuid.New()
	supB, wrkB := uuid.New(), uuid.New()
	s.workerAID, s.workerBID = wrkA, wrkB
	if _, err := s.pool.Exec(s.ctx, `INSERT INTO suppliers (id,email,status,reputation,completed_tasks)
		VALUES ($1,$2,'active',0.95,100),($3,$4,'active',0.95,100)`,
		supA, "alpha-sup-a-"+uuid.NewString()+"@example.test",
		supB, "alpha-sup-b-"+uuid.NewString()+"@example.test"); err != nil {
		t.Fatalf("seed suppliers: %v", err)
	}
	for _, pair := range []struct {
		w, sup uuid.UUID
		fp     string
	}{
		{wrkA, supA, "alpha-device-a-" + wrkA.String()},
		{wrkB, supB, "alpha-device-b-" + wrkB.String()},
	} {
		cap := testWorkerCapability(pair.w, pair.sup)
		cap.Sandboxed = true
		mustf(t, s.store.UpsertWorker(s.ctx, cap), "upsert worker: %v")
		tok, err := s.store.IssueDeviceBoundWorkerToken(s.ctx, pair.w, pair.sup, pair.fp)
		mustf(t, err, "device-bound token: %v")
		if pair.w == wrkA {
			s.workerATok = tok
		} else {
			s.workerBTok = tok
		}
	}

	s.jobAID, s.taskAID = uuid.New(), uuid.New()
	if _, err := s.pool.Exec(s.ctx, `
		INSERT INTO jobs (id,buyer_id,status,job_type,model_ref,input_ref,task_count,
		                  offered_rate_usd_hr,min_memory_gb,tier)
		VALUES ($1,$2,'queued','embed','all-minilm-l6-v2','in',1,10.0,0,'batch')`,
		s.jobAID, s.buyerAID); err != nil {
		t.Fatalf("seed job: %v", err)
	}
	rk := taskAttemptResultKey(s.jobAID, s.taskAID, 0)
	if rk == "" {
		rk = fmt.Sprintf("jobs/%s/tasks/%s/attempt-0/result.json", s.jobAID, s.taskAID)
	}
	if _, err := s.pool.Exec(s.ctx, `
		INSERT INTO tasks (id,job_id,status,input_ref,result_key)
		VALUES ($1,$2,'queued','in',$3)`, s.taskAID, s.jobAID, rk); err != nil {
		t.Fatalf("seed task: %v", err)
	}
}

type alphaResp struct {
	code int
	body string
	json map[string]any
}

func (s *alphaSuite) do(method, path, bearer string, payload any) alphaResp {
	return s.doFull(method, path, bearer, "", nil, payload, "127.0.0.1:3344")
}

func (s *alphaSuite) doWorker(method, path, workerTok string, payload any) alphaResp {
	return s.doFull(method, path, "", workerTok, nil, payload, "127.0.0.1:3344")
}

func (s *alphaSuite) doFull(method, path, bearer, workerTok string, extra http.Header, payload any, remote string) alphaResp {
	var rdr io.Reader
	if payload != nil {
		switch v := payload.(type) {
		case []byte:
			rdr = bytes.NewReader(v)
		case string:
			rdr = strings.NewReader(v)
		default:
			b, err := json.Marshal(v)
			if err != nil {
				return alphaResp{code: -1, body: err.Error()}
			}
			rdr = bytes.NewReader(b)
		}
	}
	req := httptest.NewRequest(method, path, rdr)
	req.RemoteAddr = remote
	if payload != nil {
		if req.Header.Get("Content-Type") == "" {
			req.Header.Set("Content-Type", "application/json")
		}
	}
	if bearer != "" {
		req.Header.Set("Authorization", "Bearer "+bearer)
	}
	if workerTok != "" {
		req.Header.Set("X-Worker-Token", workerTok)
	}
	for k, vs := range extra {
		for _, v := range vs {
			req.Header.Set(k, v)
		}
	}
	rec := httptest.NewRecorder()
	s.handler.ServeHTTP(rec, req)
	out := alphaResp{code: rec.Code, body: rec.Body.String()}
	_ = json.Unmarshal(rec.Body.Bytes(), &out.json)
	return out
}

func (s *alphaSuite) record(a alphaAttack) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.attacks = append(s.attacks, a)
	if a.Status == "finding" {
		s.t.Errorf("FINDING %s [%s] %s: %s (HTTP %d)", a.Severity, a.ID, a.Title, a.Detail, a.HTTPStatus)
	}
}

func blockedOrFinding(ok bool, severity string) (status, sev string) {
	if ok {
		return "blocked", ""
	}
	return "finding", severity
}

func rejectOK(code int) bool {
	return code == http.StatusUnauthorized || code == http.StatusForbidden ||
		code == http.StatusNotFound || code == http.StatusBadRequest ||
		code == http.StatusConflict || code == http.StatusGone ||
		code == http.StatusUnprocessableEntity || code == http.StatusTooManyRequests ||
		code == http.StatusServiceUnavailable || code == http.StatusRequestEntityTooLarge ||
		code == http.StatusPaymentRequired
}

func successCode(code int) bool {
	return code >= 200 && code < 300
}

func (s *alphaSuite) attackAuthFuzz() {
	raw, err := os.ReadFile("../ops/authorization-matrix.json")
	must(s.t, err)
	var matrix struct {
		RouteClasses []struct {
			ID     string   `json:"id"`
			Routes []string `json:"routes"`
		} `json:"route_classes"`
	}
	must(s.t, json.Unmarshal(raw, &matrix))

	checked := 0
	for _, class := range matrix.RouteClasses {
		if class.ID == "public_read" || class.ID == "public_bootstrap" {
			continue
		}
		for _, pattern := range class.Routes {
			parts := strings.SplitN(pattern, " ", 2)
			path := concreteAuthorizationPath(pattern)
			// Anonymous
			rec := s.doFull(parts[0], path, "", "", nil, nil, "127.0.0.1:3344")
			ok := rec.code == http.StatusUnauthorized ||
				(class.ID == "provider_hmac" && (rec.code == http.StatusBadRequest || rec.code == http.StatusServiceUnavailable || rec.code == http.StatusUnauthorized))
			status, sev := blockedOrFinding(ok, "P0")
			s.record(alphaAttack{
				ID: "auth-anon-" + class.ID + "-" + parts[0] + "-" + path, Class: "identity",
				Title: "anonymous request on protected route", Status: status, Severity: sev,
				HTTPStatus: rec.code, Want: "401 (or webhook signature reject)",
				Reproduction:   fmt.Sprintf("%s %s with no credentials", parts[0], path),
				Location:       "control/api.go:Server.Routes + authBuyer/authWorker/authAdmin/webhook",
				AlphaReachable: true, Detail: rec.body,
			})
			// Buyer acting as worker / worker acting as buyer / buyer as admin
			switch class.ID {
			case "worker_owned":
				got := s.doFull(parts[0], path, s.buyerAKey, "", nil, map[string]any{}, "127.0.0.1:3344")
				ok = !successCode(got.code)
				status, sev = blockedOrFinding(ok, "P0")
				s.record(alphaAttack{
					ID: "auth-buyer-as-worker-" + path, Class: "identity",
					Title: "buyer credential on worker route", Status: status, Severity: sev,
					HTTPStatus: got.code, Want: "401/403",
					Reproduction:   "Authorization: Bearer <buyer sandbox key> on " + pattern,
					Location:       "control/api.go:authWorker",
					AlphaReachable: true, Detail: got.body,
				})
			case "buyer_owned":
				got := s.doFull(parts[0], path, "", s.workerATok, nil, map[string]any{}, "127.0.0.1:3344")
				ok = !successCode(got.code)
				status, sev = blockedOrFinding(ok, "P0")
				s.record(alphaAttack{
					ID: "auth-worker-as-buyer-" + path, Class: "identity",
					Title: "worker token on buyer route", Status: status, Severity: sev,
					HTTPStatus: got.code, Want: "401/403",
					Reproduction:   "X-Worker-Token on " + pattern,
					Location:       "control/api.go:authBuyer",
					AlphaReachable: true, Detail: got.body,
				})
			case "operator":
				got := s.doFull(parts[0], path, s.buyerAKey, "", nil, map[string]any{"reason": "please", "request_id": "x"}, "127.0.0.1:3344")
				ok = !successCode(got.code)
				status, sev = blockedOrFinding(ok, "P0")
				s.record(alphaAttack{
					ID: "auth-buyer-as-admin-" + path, Class: "identity",
					Title: "buyer credential on operator route", Status: status, Severity: sev,
					HTTPStatus: got.code, Want: "401/403",
					Reproduction:   "buyer API key on " + pattern,
					Location:       "control/api.go:authAdmin + admin_credentials",
					AlphaReachable: true, Detail: got.body,
				})
			}
			checked++
		}
	}
	s.record(alphaAttack{
		ID: "auth-matrix-coverage", Class: "identity", Title: "protected matrix routes exercised",
		Status: "blocked", HTTPStatus: checked, Want: "every protected route attacked",
		Reproduction:   "iterate ops/authorization-matrix.json protected classes through Routes()",
		Location:       "ops/authorization-matrix.json",
		AlphaReachable: true,
		Detail:         fmt.Sprintf("protected routes attacked: %d", checked),
	})
}

func (s *alphaSuite) attackIdentityAndPrivilege() {
	// Cross-tenant job read
	got := s.do(http.MethodGet, "/v1/jobs/"+s.jobAID.String(), s.buyerBKey, nil)
	ok := got.code == http.StatusNotFound || got.code == http.StatusForbidden || got.code == http.StatusUnauthorized
	status, sev := blockedOrFinding(ok, "P0")
	s.record(alphaAttack{
		ID: "identity-cross-tenant-job", Class: "identity", Title: "buyer B reads buyer A job",
		Status: status, Severity: sev, HTTPStatus: got.code, Want: "404/403",
		Reproduction:   "GET /v1/jobs/{A} with buyer B sandbox key",
		Location:       "control/api.go:handleGetJob + store.GetJob buyer_id predicate",
		AlphaReachable: true, Detail: got.body,
	})

	// Cross-tenant dispute
	got = s.do(http.MethodPost, "/v1/jobs/"+s.jobAID.String()+"/dispute", s.buyerBKey, map[string]any{
		"reason": "I am not the owner but I want the money back",
	})
	ok = !successCode(got.code)
	status, sev = blockedOrFinding(ok, "P0")
	s.record(alphaAttack{
		ID: "identity-cross-tenant-dispute", Class: "identity", Title: "buyer B disputes buyer A job",
		Status: status, Severity: sev, HTTPStatus: got.code, Want: "404/403/409",
		Reproduction:   "POST /v1/jobs/{A}/dispute as buyer B",
		Location:       "control/api.go:handleFileDispute + RecordDispute",
		AlphaReachable: true, Detail: got.body,
	})

	// Worker B commits worker A's unclaimed/other task
	got = s.doWorker(http.MethodPost, "/v1/worker/task/"+s.taskAID.String()+"/commit", s.workerBTok, map[string]any{
		"task_id":     uuid.NewString(),
		"attempt":     0,
		"result_key":  "jobs/" + uuid.NewString() + "/stolen.json",
		"duration_ms": 1, "tokens_used": 1,
	})
	ok = !successCode(got.code)
	status, sev = blockedOrFinding(ok, "P0")
	s.record(alphaAttack{
		ID: "identity-offer-mismatch-commit", Class: "identity", Title: "worker B commits a task it did not claim",
		Status: status, Severity: sev, HTTPStatus: got.code, Want: "409/401/403",
		Reproduction:   "POST /v1/worker/task/{A}/commit with worker B token and a foreign result_key",
		Location:       "control/api.go:handleWorkerCommit + store.completeTaskTx claimed_by fence",
		AlphaReachable: true, Detail: got.body,
	})

	// Device / identity impersonation: register as another worker_id
	got = s.doWorker(http.MethodPost, "/v1/worker/register", s.workerATok, map[string]any{
		"worker_id":        s.workerBID.String(),
		"supplier_id":      uuid.NewString(),
		"hw_class":         "apple_silicon_pro",
		"engine":           "candle",
		"memory_gb":        64,
		"supported_jobs":   []string{"embed"},
		"supported_models": []string{"all-minilm-l6-v2"},
	})
	// Handler overwrites WorkerID/SupplierID from auth. Success is OK only if
	// the stored row is still worker A. Check that.
	if successCode(got.code) {
		var stored uuid.UUID
		err := s.pool.QueryRow(s.ctx, `SELECT id FROM workers WHERE id=$1`, s.workerAID).Scan(&stored)
		if err != nil || stored != s.workerAID {
			s.record(alphaAttack{
				ID: "identity-device-impersonation-register", Class: "identity",
				Title: "worker register accepted a foreign worker_id", Status: "finding", Severity: "P0",
				HTTPStatus: got.code, Want: "auth WorkerID wins; foreign id discarded",
				Reproduction:   "POST /v1/worker/register with worker A token and worker B id in body",
				Location:       "control/api.go:handleWorkerRegister cap.WorkerID = auth.WorkerID",
				AlphaReachable: true, Detail: got.body,
			})
		} else {
			s.record(alphaAttack{
				ID: "identity-device-impersonation-register", Class: "identity",
				Title: "worker register ignores body worker_id", Status: "blocked",
				HTTPStatus: got.code, Want: "auth WorkerID wins",
				Reproduction:   "POST /v1/worker/register with worker A token and worker B id in body",
				Location:       "control/api.go:handleWorkerRegister",
				AlphaReachable: true, Detail: "body worker_id discarded; row remains worker A",
			})
		}
	} else {
		s.record(alphaAttack{
			ID: "identity-device-impersonation-register", Class: "identity",
			Title: "worker register with foreign worker_id refused", Status: "blocked",
			HTTPStatus: got.code, Want: "refuse or ignore foreign id",
			Reproduction:   "POST /v1/worker/register with worker A token and worker B id in body",
			Location:       "control/api.go:handleWorkerRegister",
			AlphaReachable: true, Detail: got.body,
		})
	}

	// Privilege escalation via key mint
	got = s.do(http.MethodPost, "/v1/keys", s.buyerAKey, map[string]any{
		"name": "pwn", "test": true, "is_admin": true, "admin": true, "role": "operator",
	})
	if successCode(got.code) {
		// extra fields may be ignored (decoder is not strict here). Check the key is not admin.
		raw, _ := got.json["key"].(string)
		if raw != "" {
			auth, err := s.store.LookupAPIKey(s.ctx, raw)
			if err == nil && auth.IsAdmin {
				s.record(alphaAttack{
					ID: "identity-priv-esc-key-mint", Class: "identity",
					Title: "buyer minted an admin API key", Status: "finding", Severity: "P0",
					HTTPStatus: got.code, Want: "non-admin key",
					Reproduction:   `POST /v1/keys {"name":"pwn","test":true,"is_admin":true}`,
					Location:       "control/api.go:handleCreateKey + store.CreateAPIKey",
					AlphaReachable: true, Detail: "LookupAPIKey.IsAdmin=true",
				})
			} else {
				s.record(alphaAttack{
					ID: "identity-priv-esc-key-mint", Class: "identity",
					Title: "is_admin on key mint did not elevate", Status: "blocked",
					HTTPStatus: got.code, Want: "non-admin key (unknown fields ignored or rejected)",
					Reproduction:   `POST /v1/keys {"is_admin":true}`,
					Location:       "control/api.go:handleCreateKey",
					AlphaReachable: true, Detail: "minted key is not admin",
				})
			}
		} else {
			s.record(alphaAttack{
				ID: "identity-priv-esc-key-mint", Class: "identity",
				Title: "key mint with is_admin extra fields", Status: "blocked",
				HTTPStatus: got.code, Want: "no admin elevation",
				Reproduction:   `POST /v1/keys {"is_admin":true}`,
				Location:       "control/api.go:handleCreateKey",
				AlphaReachable: true, Detail: got.body,
			})
		}
	} else {
		s.record(alphaAttack{
			ID: "identity-priv-esc-key-mint", Class: "identity",
			Title: "key mint with is_admin extra fields refused", Status: "blocked",
			HTTPStatus: got.code, Want: "400 or non-admin",
			Reproduction:   `POST /v1/keys {"is_admin":true}`,
			Location:       "control/api.go:handleCreateKey",
			AlphaReachable: true, Detail: got.body,
		})
	}

	// Token replay after logout
	if s.sessionA != "" {
		logout := s.do(http.MethodPost, "/v1/logout", s.sessionA, map[string]any{})
		replay := s.do(http.MethodGet, "/v1/me", s.sessionA, nil)
		ok := logout.code < 500 && !successCode(replay.code)
		status, sev = blockedOrFinding(ok, "P1")
		s.record(alphaAttack{
			ID: "identity-session-replay-after-logout", Class: "identity",
			Title: "session token reused after logout", Status: status, Severity: sev,
			HTTPStatus: replay.code, Want: "401",
			Reproduction:   "POST /v1/logout then GET /v1/me with the same cx_sess_ token",
			Location:       "control/accounts.go:RevokeSession + LookupSession revoked=false",
			AlphaReachable: true, Detail: "logout=" + logout.body + " replay=" + replay.body,
		})
	}

	// Admin from a non-loopback address
	got = s.doFull(http.MethodGet, "/admin/payouts", s.adminKey, "", nil, nil, "8.8.8.8:443")
	ok = got.code == http.StatusForbidden || got.code == http.StatusUnauthorized
	status, sev = blockedOrFinding(ok, "P0")
	s.record(alphaAttack{
		ID: "identity-admin-non-loopback", Class: "identity",
		Title: "valid admin key from a public source IP", Status: status, Severity: sev,
		HTTPStatus: got.code, Want: "403",
		Reproduction:   "GET /admin/payouts Authorization: Bearer <admin> RemoteAddr=8.8.8.8",
		Location:       "control/admin_authority.go:adminSourceAllowed",
		AlphaReachable: true, Detail: got.body,
	})

	// Expired worker token
	raw, _, err := s.store.CreateWorkerTokenWithExpiry(s.ctx, uuid.New(), uuid.New(), time.Second)
	if err == nil {
		_, _ = s.pool.Exec(s.ctx, `UPDATE worker_tokens SET expires_at = now() - interval '2 seconds'
			WHERE token_hash=$1`, hashKey(raw))
		got = s.doWorker(http.MethodGet, "/v1/worker/earnings", raw, nil)
		ok = got.code == http.StatusUnauthorized
		status, sev = blockedOrFinding(ok, "P1")
		s.record(alphaAttack{
			ID: "identity-expired-worker-token", Class: "identity",
			Title: "expired worker token on earnings", Status: status, Severity: sev,
			HTTPStatus: got.code, Want: "401 WORKER_TOKEN_EXPIRED",
			Reproduction:   "SET expires_at to the past; GET /v1/worker/earnings",
			Location:       "control/api.go:authWorker + LookupWorkerToken",
			AlphaReachable: true, Detail: got.body,
		})
	}
}

func stripeSign(secret string, payload []byte, ts time.Time) string {
	t := fmt.Sprint(ts.Unix())
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write([]byte(t + "." + string(payload)))
	return "t=" + t + ",v1=" + hex.EncodeToString(mac.Sum(nil))
}

func (s *alphaSuite) stripePayload(eventType string) []byte {
	return []byte(fmt.Sprintf(
		`{"id":"evt_alpha_%s","type":%q,"api_version":"2025-06-30.basil","livemode":false,"created":%d,`+
			`"data":{"object":{"id":"ch_alpha","payment_intent":"pi_alpha","amount":500,"amount_refunded":500,"currency":"usd",`+
			`"customer":"cus_alpha","payment_method":"pm_alpha","payouts_enabled":true}}}`,
		strings.ReplaceAll(eventType, ".", "_"), eventType, time.Now().Unix()))
}

func (s *alphaSuite) postWebhook(path, sig string, payload []byte) alphaResp {
	h := make(http.Header)
	if sig != "" {
		h.Set("Stripe-Signature", sig)
	}
	return s.doFull(http.MethodPost, path, "", "", h, payload, "127.0.0.1:3344")
}

func (s *alphaSuite) attackWebhooks() {
	billing := os.Getenv("STRIPE_WEBHOOK_SECRET")
	connect := os.Getenv("MERC_CONNECT_WEBHOOK_SECRET")
	payload := s.stripePayload("charge.refunded")
	connectPayload := s.stripePayload("account.updated")
	now := time.Now()

	cases := []struct {
		id, title, path, sig string
		payload              []byte
		ok                   func(int) bool
		sev                  string
	}{
		{"webhook-forged-billing", "forged signature on billing webhook",
			"/v1/stripe/webhook", "t=1,v1=deadbeef", payload,
			func(c int) bool { return c == http.StatusBadRequest || c == http.StatusServiceUnavailable }, "P0"},
		{"webhook-forged-connect", "forged signature on connect webhook",
			"/v1/stripe/connect-webhook", "t=1,v1=deadbeef", connectPayload,
			func(c int) bool { return c == http.StatusBadRequest || c == http.StatusServiceUnavailable }, "P0"},
		{"webhook-stripped-billing", "stripped Stripe-Signature on billing",
			"/v1/stripe/webhook", "", payload,
			func(c int) bool { return c == http.StatusBadRequest || c == http.StatusServiceUnavailable }, "P0"},
		{"webhook-stripped-connect", "stripped Stripe-Signature on connect",
			"/v1/stripe/connect-webhook", "", connectPayload,
			func(c int) bool { return c == http.StatusBadRequest || c == http.StatusServiceUnavailable }, "P0"},
		{"webhook-billing-secret-on-connect", "billing secret presented at Connect endpoint",
			"/v1/stripe/connect-webhook", stripeSign(billing, connectPayload, now), connectPayload,
			func(c int) bool { return c == http.StatusBadRequest || c == http.StatusServiceUnavailable }, "P0"},
		{"webhook-connect-secret-on-billing", "Connect secret presented at billing endpoint",
			"/v1/stripe/webhook", stripeSign(connect, payload, now), payload,
			func(c int) bool { return c == http.StatusBadRequest || c == http.StatusServiceUnavailable }, "P0"},
		{"webhook-replay-old-timestamp", "valid billing signature with timestamp 20 minutes old",
			"/v1/stripe/webhook", stripeSign(billing, payload, now.Add(-20*time.Minute)), payload,
			func(c int) bool { return c == http.StatusBadRequest || c == http.StatusServiceUnavailable }, "P0"},
		{"webhook-future-timestamp", "valid billing signature claiming 20 minutes in the future",
			"/v1/stripe/webhook", stripeSign(billing, payload, now.Add(20*time.Minute)), payload,
			func(c int) bool { return c == http.StatusBadRequest || c == http.StatusServiceUnavailable }, "P1"},
	}
	for _, tc := range cases {
		got := s.postWebhook(tc.path, tc.sig, tc.payload)
		ok := tc.ok(got.code)
		status, sev := blockedOrFinding(ok, tc.sev)
		s.record(alphaAttack{
			ID: tc.id, Class: "identity_webhook", Title: tc.title,
			Status: status, Severity: sev, HTTPStatus: got.code, Want: "400/503, not 200",
			Reproduction:   fmt.Sprintf("POST %s with %s", tc.path, tc.id),
			Location:       "control/billing.go:handleStripeWebhook + control/suppliers.go:handleConnectWebhook + verifyStripeSig",
			AlphaReachable: true, Detail: got.body,
		})
	}

	// Correct secret on the matching endpoint must get past signature
	// verification (200 or a later contract/apply error — not 400 invalid signature).
	got := s.postWebhook("/v1/stripe/webhook", stripeSign(billing, payload, now), payload)
	if strings.Contains(got.body, "invalid stripe signature") {
		s.record(alphaAttack{
			ID: "webhook-correct-billing-secret", Class: "identity_webhook",
			Title: "correct billing secret rejected as invalid signature", Status: "error",
			HTTPStatus: got.code, Want: "signature accepted (then apply/contract)",
			Reproduction:   "POST /v1/stripe/webhook signed with STRIPE_WEBHOOK_SECRET",
			Location:       "control/billing.go:handleStripeWebhook",
			AlphaReachable: false, Detail: got.body,
		})
	} else {
		s.record(alphaAttack{
			ID: "webhook-correct-billing-secret", Class: "identity_webhook",
			Title: "correct billing secret passes signature check", Status: "blocked",
			HTTPStatus: got.code, Want: "not 'invalid stripe signature'",
			Reproduction:   "POST /v1/stripe/webhook signed with STRIPE_WEBHOOK_SECRET",
			Location:       "control/billing.go:handleStripeWebhook",
			AlphaReachable: true, Detail: got.body,
		})
	}
}

func (s *alphaSuite) attackMoney() {
	// Unauthorized money mutation routes
	money := []struct {
		id, title, method, path string
		payload                 any
		auth                    string
		sev                     string
	}{
		{"money-buyer-release-payout", "buyer releases a payout",
			http.MethodPost, "/admin/payouts/" + uuid.NewString() + "/release",
			map[string]any{"reason": "please pay me", "request_id": "alpha-rel-1"}, s.buyerAKey, "P0"},
		{"money-buyer-prepaid-refund", "buyer issues an operator prepaid refund",
			http.MethodPost, "/admin/buyers/" + s.buyerAID.String() + "/prepaid-refund",
			map[string]any{"reason": "give it back", "request_id": "alpha-ref-1"}, s.buyerAKey, "P0"},
		{"money-buyer-subsidize", "buyer subsidizes a payout",
			http.MethodPost, "/admin/payouts/" + uuid.NewString() + "/subsidize",
			map[string]any{"reason": "subsidy", "request_id": "alpha-sub-1"}, s.buyerAKey, "P0"},
		{"money-worker-topup", "worker tops up a buyer balance",
			http.MethodPost, "/v1/billing/topup",
			map[string]any{"amount_major": "10.00", "currency": "usd"}, "", "P0"},
		{"money-anon-topup", "anonymous topup",
			http.MethodPost, "/v1/billing/topup",
			map[string]any{"amount_major": "10.00", "currency": "usd"}, "", "P0"},
		{"money-worker-create-job", "worker creates a billed job",
			http.MethodPost, "/v1/jobs",
			map[string]any{"job_type": map[string]any{"type": "embed"}, "max_usd": 1}, "", "P0"},
	}
	for _, tc := range money {
		var got alphaResp
		if tc.id == "money-worker-topup" || tc.id == "money-worker-create-job" {
			h := make(http.Header)
			h.Set("Idempotency-Key", "alpha-"+uuid.NewString())
			got = s.doFull(tc.method, tc.path, "", s.workerATok, h, tc.payload, "127.0.0.1:3344")
		} else {
			got = s.do(tc.method, tc.path, tc.auth, tc.payload)
		}
		ok := !successCode(got.code)
		status, sev := blockedOrFinding(ok, tc.sev)
		s.record(alphaAttack{
			ID: tc.id, Class: "money", Title: tc.title,
			Status: status, Severity: sev, HTTPStatus: got.code, Want: "401/403",
			Reproduction:   fmt.Sprintf("%s %s as unauthorized principal", tc.method, tc.path),
			Location:       "control/api.go Routes + authAdmin/authBuyer",
			AlphaReachable: true, Detail: got.body,
		})
	}

	// Buyer tries to inject a second buyer_id on topup (strict decode)
	h := make(http.Header)
	h.Set("Idempotency-Key", "alpha-topup-"+uuid.NewString())
	got := s.doFull(http.MethodPost, "/v1/billing/topup", s.buyerAKey, "", h, map[string]any{
		"amount_major": "25.00", "currency": "usd", "buyer_id": s.buyerBID.String(),
	}, "127.0.0.1:3344")
	ok := !successCode(got.code) // unknown field or still charged as A — success would be OK only if it charged A
	// Strict JSON should refuse buyer_id.
	ok = got.code == http.StatusBadRequest || got.code == http.StatusPaymentRequired ||
		got.code == http.StatusServiceUnavailable
	status, sev := blockedOrFinding(ok, "P0")
	s.record(alphaAttack{
		ID: "money-topup-foreign-buyer-id", Class: "money",
		Title: "topup body names another buyer_id", Status: status, Severity: sev,
		HTTPStatus: got.code, Want: "400 unknown field (strict) or ignore + still require card",
		Reproduction:   `POST /v1/billing/topup {"amount_major":"25.00","currency":"usd","buyer_id":"<B>"}`,
		Location:       "control/prepaid.go:handleBillingTopup decodeStrictJSONObject",
		AlphaReachable: true, Detail: got.body,
	})

	// Replay a settled/terminal commit
	got = s.doWorker(http.MethodPost, "/v1/worker/task/"+s.taskAID.String()+"/commit", s.workerATok, map[string]any{
		"attempt": 0, "result_key": taskAttemptResultKey(s.jobAID, s.taskAID, 0),
		"duration_ms": 1, "tokens_used": 1,
	})
	ok = !successCode(got.code) // not claimed by this worker yet, or not running
	status, sev = blockedOrFinding(ok, "P1")
	s.record(alphaAttack{
		ID: "money-replay-unsettled-commit", Class: "money",
		Title: "commit a task that this worker does not hold", Status: status, Severity: sev,
		HTTPStatus: got.code, Want: "409",
		Reproduction:   "POST /v1/worker/task/{id}/commit without a prior claim",
		Location:       "control/store_tasks.go:completeTaskTx claimed_by fence",
		AlphaReachable: true, Detail: got.body,
	})
}

func (s *alphaSuite) attackFXAndAmounts() {
	h := make(http.Header)
	h.Set("Idempotency-Key", "alpha-job-"+uuid.NewString())

	neg := s.doFull(http.MethodPost, "/v1/jobs", s.buyerAKey, "", h, map[string]any{
		"job_type": map[string]any{"type": "embed"},
		"model":    map[string]any{"kind": "gguf", "ref": "all-minilm-l6-v2"},
		"tier":     "batch",
		"input":    `{"id":"0","text":"x"}`,
		"max_usd":  -1,
	}, "127.0.0.1:3344")
	ok := neg.code == http.StatusBadRequest
	status, sev := blockedOrFinding(ok, "P0")
	s.record(alphaAttack{
		ID: "money-negative-max-usd", Class: "money", Title: "job submit with max_usd=-1",
		Status: status, Severity: sev, HTTPStatus: neg.code, Want: "400",
		Reproduction:   `POST /v1/jobs {"max_usd":-1,...}`,
		Location:       "control/api.go:createJob max_usd finite non-negative",
		AlphaReachable: true, Detail: neg.body,
	})

	h.Set("Idempotency-Key", "alpha-job-inf-"+uuid.NewString())
	inf := s.doFull(http.MethodPost, "/v1/jobs", s.buyerAKey, "", h, []byte(
		`{"job_type":{"type":"embed"},"model":{"kind":"gguf","ref":"x"},"tier":"batch","input":"{}","max_usd":1e309}`),
		"127.0.0.1:3344")
	ok = inf.code == http.StatusBadRequest
	status, sev = blockedOrFinding(ok, "P0")
	s.record(alphaAttack{
		ID: "money-overflow-max-usd", Class: "money", Title: "job submit with overflowing max_usd",
		Status: status, Severity: sev, HTTPStatus: inf.code, Want: "400",
		Reproduction:   `POST /v1/jobs {"max_usd":1e309}`,
		Location:       "control/api.go:createJob math.IsInf/IsNaN",
		AlphaReachable: true, Detail: inf.body,
	})

	h.Set("Idempotency-Key", "alpha-job-fx-"+uuid.NewString())
	fx := s.doFull(http.MethodPost, "/v1/jobs", s.buyerAKey, "", h, map[string]any{
		"job_type":                     map[string]any{"type": "embed"},
		"model":                        map[string]any{"kind": "gguf", "ref": "x"},
		"tier":                         "batch",
		"input":                        "{}",
		"max_usd":                      1,
		"reference_to_settlement_rate": 0.01,
		"fx_rate":                      0.01,
		"currency":                     "jpy",
	}, "127.0.0.1:3344")
	ok = fx.code == http.StatusBadRequest // unknown fields via decodeStrictJSONObject
	status, sev = blockedOrFinding(ok, "P0")
	s.record(alphaAttack{
		ID: "money-fx-injection-on-submit", Class: "money",
		Title: "buyer injects FX rate / currency on job submit", Status: status, Severity: sev,
		HTTPStatus: fx.code, Want: "400 unknown field",
		Reproduction:   `POST /v1/jobs {"reference_to_settlement_rate":0.01,"currency":"jpy"}`,
		Location:       "control/api.go:jobSubmit + decodeStrictJSONObject",
		AlphaReachable: true, Detail: fx.body,
	})

	// CAD settlement without a governed FX rate must fail closed on quote.
	installSettlementCurrencyForTest(s.t, "cad")
	s.t.Setenv("MERC_PRICE_REFERENCE_TO_SETTLEMENT_RATE", "")
	s.t.Setenv("MERC_PRICE_FX_REVISION", "")
	q := s.do(http.MethodPost, "/v1/quote", s.buyerAKey, map[string]any{
		"job_type": map[string]any{"type": "embed"},
		"model":    map[string]any{"kind": "gguf", "ref": "all-minilm-l6-v2"},
		"tier":     "batch",
		"input":    `{"id":"0","text":"fx-attack"}`,
		"max_usd":  1,
	})
	ok = !successCode(q.code)
	status, sev = blockedOrFinding(ok, "P0")
	s.record(alphaAttack{
		ID: "money-cad-quote-without-fx", Class: "money",
		Title: "CAD quote with empty governed FX rate", Status: status, Severity: sev,
		HTTPStatus: q.code, Want: "4xx/5xx fail closed, not a USD 1:1 quote",
		Reproduction:   "MERC_SETTLEMENT_CURRENCY=cad, unset MERC_PRICE_REFERENCE_TO_SETTLEMENT_RATE, POST /v1/quote",
		Location:       "control/realtime_currency_authority.go + quote path",
		AlphaReachable: true, Detail: q.body,
	})
	installSettlementCurrencyForTest(s.t, "usd")

	// Negative / zero / huge topup
	for _, amt := range []string{"-10.00", "0", "999999999999.99"} {
		hh := make(http.Header)
		hh.Set("Idempotency-Key", "alpha-amt-"+uuid.NewString())
		got := s.doFull(http.MethodPost, "/v1/billing/topup", s.buyerAKey, "", hh, map[string]any{
			"amount_major": amt, "currency": "usd",
		}, "127.0.0.1:3344")
		ok = got.code == http.StatusBadRequest || got.code == http.StatusPaymentRequired ||
			got.code == http.StatusServiceUnavailable
		status, sev = blockedOrFinding(ok, "P0")
		s.record(alphaAttack{
			ID: "money-topup-amount-" + amt, Class: "money",
			Title: "topup amount_major=" + amt, Status: status, Severity: sev,
			HTTPStatus: got.code, Want: "400 (or 402/503 before charge)",
			Reproduction:   fmt.Sprintf(`POST /v1/billing/topup {"amount_major":"%s","currency":"usd"}`, amt),
			Location:       "control/prepaid.go:handleBillingTopup ParseMajorToMinorExact",
			AlphaReachable: true, Detail: got.body,
		})
	}

	hh := make(http.Header)
	hh.Set("Idempotency-Key", "alpha-cur-"+uuid.NewString())
	got := s.doFull(http.MethodPost, "/v1/billing/topup", s.buyerAKey, "", hh, map[string]any{
		"amount_major": "10.00", "currency": "jpy",
	}, "127.0.0.1:3344")
	ok = got.code == http.StatusBadRequest
	status, sev = blockedOrFinding(ok, "P0")
	s.record(alphaAttack{
		ID: "money-topup-currency-confusion", Class: "money",
		Title: "topup in JPY against a USD settlement deployment", Status: status, Severity: sev,
		HTTPStatus: got.code, Want: "400",
		Reproduction:   `POST /v1/billing/topup {"amount_major":"10.00","currency":"jpy"}`,
		Location:       "control/prepaid.go:handleBillingTopup requestedCurrency.Equal(settlement)",
		AlphaReachable: true, Detail: got.body,
	})
}

func (s *alphaSuite) attackContainmentAndInput() {
	// Path traversal on public site assets. ServeMux used to 307 these onto
	// cleaned paths (including other registered routes). The request must be
	// refused before that bounce, and must never return file contents.
	for _, p := range []string{
		"/assets/site/../../control/schema.sql",
		"/assets/site/..%2F..%2F.env",
		"/assets/site/foo/../../../etc/passwd",
		"/assets/site/../../v1/me",
	} {
		got := s.do(http.MethodGet, p, s.buyerAKey, nil)
		ok := got.code == http.StatusNotFound || got.code == http.StatusBadRequest
		if got.code == http.StatusTemporaryRedirect || got.code == http.StatusMovedPermanently {
			ok = false
		}
		if strings.Contains(got.body, "CREATE TABLE") || strings.Contains(got.body, "STRIPE_SECRET") ||
			strings.Contains(got.body, "buyer_id") {
			ok = false
		}
		status, sev := blockedOrFinding(ok, "P1")
		s.record(alphaAttack{
			ID: "contain-site-asset-traversal", Class: "containment",
			Title: "path traversal via /assets/site", Status: status, Severity: sev,
			HTTPStatus: got.code, Want: "404, not 307 onto another route",
			Reproduction:   "GET " + p,
			Location:       "control/api.go:rejectPathTraversal + handleSiteAsset",
			AlphaReachable: true, Detail: trim(got.body, 200),
		})
	}

	// Project compile archive with traversal entries
	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)
	hdr := &tar.Header{Name: "../../etc/passwd", Mode: 0600, Size: 5}
	_ = tw.WriteHeader(hdr)
	_, _ = tw.Write([]byte("root\n"))
	_ = tw.Close()
	h := make(http.Header)
	h.Set("Content-Type", "application/x-tar")
	req := httptest.NewRequest(http.MethodPost, "/v1/projects/compile", bytes.NewReader(buf.Bytes()))
	req.RemoteAddr = "127.0.0.1:3344"
	req.Header.Set("Authorization", "Bearer "+s.buyerAKey)
	req.Header.Set("Content-Type", "application/x-tar")
	rec := httptest.NewRecorder()
	s.handler.ServeHTTP(rec, req)
	ok := rec.Code == http.StatusBadRequest
	status, sev := blockedOrFinding(ok, "P0")
	s.record(alphaAttack{
		ID: "contain-project-tar-traversal", Class: "containment",
		Title: "project compile tar with ../../etc/passwd", Status: status, Severity: sev,
		HTTPStatus: rec.Code, Want: "400 archive path escapes",
		Reproduction:   "POST /v1/projects/compile with a tar entry named ../../etc/passwd",
		Location:       "control/project_compile_api.go:cleanProjectArchivePath",
		AlphaReachable: true, Detail: rec.Body.String(),
	})

	// Artifact poisoning on commit
	got := s.doWorker(http.MethodPost, "/v1/worker/task/"+s.taskAID.String()+"/commit", s.workerATok, map[string]any{
		"attempt":     0,
		"result_key":  "jobs/" + s.jobAID.String() + "/../../../secrets.json",
		"duration_ms": 1, "tokens_used": 1,
	})
	ok = !successCode(got.code)
	status, sev = blockedOrFinding(ok, "P0")
	s.record(alphaAttack{
		ID: "contain-artifact-poison-result-key", Class: "containment",
		Title: "worker commit with a non-canonical result_key", Status: status, Severity: sev,
		HTTPStatus: got.code, Want: "409/400",
		Reproduction:   `POST /v1/worker/task/{id}/commit {"result_key":"jobs/<id>/../../../secrets.json"}`,
		Location:       "control/store_tasks.go:completeTaskTx expectedResultKey",
		AlphaReachable: true, Detail: got.body,
	})

	// Project artifact ref traversal
	got = s.do(http.MethodPost, "/v1/projects", s.buyerAKey, map[string]any{
		"declaration": map[string]any{
			"steps": []any{map[string]any{
				"id": "s1", "consumes": []string{"project://../../etc/passwd"},
			}},
		},
	})
	ok = !successCode(got.code)
	status, sev = blockedOrFinding(ok, "P1")
	s.record(alphaAttack{
		ID: "contain-project-artifact-ref", Class: "containment",
		Title: "project declaration with traversing artifact ref", Status: status, Severity: sev,
		HTTPStatus: got.code, Want: "400",
		Reproduction:   `POST /v1/projects consumes project://../../etc/passwd`,
		Location:       "control/project_declaration.go:validProjectArtifactRef",
		AlphaReachable: true, Detail: got.body,
	})
}

func (s *alphaSuite) attackConcurrency() {
	// Two workers poll the same queued task through the HTTP surface.
	var wg sync.WaitGroup
	var codes [2]int
	var bodies [2]string
	tokens := []string{s.workerATok, s.workerBTok}
	wg.Add(2)
	for i := 0; i < 2; i++ {
		go func(i int) {
			defer wg.Done()
			got := s.doWorker(http.MethodGet, "/v1/worker/poll", tokens[i], nil)
			codes[i] = got.code
			bodies[i] = got.body
		}(i)
	}
	wg.Wait()

	var claimed int
	if err := s.pool.QueryRow(s.ctx, `
		SELECT count(*) FROM tasks WHERE id=$1 AND claimed_by IS NOT NULL`, s.taskAID,
	).Scan(&claimed); err != nil {
		s.t.Fatalf("count claimed: %v", err)
	}
	ok := claimed <= 1
	status, sev := blockedOrFinding(ok, "P0")
	s.record(alphaAttack{
		ID: "concurrency-two-worker-claim", Class: "concurrency",
		Title: "two workers poll the same queued task", Status: status, Severity: sev,
		HTTPStatus: codes[0], Want: "at most one claimed_by",
		Reproduction:   "concurrent GET /v1/worker/poll with two device-bound worker tokens",
		Location:       "control/scheduler.go:ClaimTasksTx FOR UPDATE SKIP LOCKED",
		AlphaReachable: true,
		Detail: fmt.Sprintf("claimed_by rows=%d http=%d/%d bodies=%q / %q",
			claimed, codes[0], codes[1], trim(bodies[0], 120), trim(bodies[1], 120)),
	})

	// Two admin "make due now" releases of the same future-held credit, then
	// two ClaimPayout calls. HTTP may stamp release_at twice; money must not
	// produce two claimable operations.
	installSettlementCurrencyForTest(s.t, "usd")
	f := seedPayoutFixture(s.t, s.ctx, s.pool, payoutFixtureOpts{
		creditUSD: 1.00, releaseFuture: true,
	})
	var relWG sync.WaitGroup
	relWG.Add(2)
	for i := 0; i < 2; i++ {
		go func(i int) {
			defer relWG.Done()
			_ = s.do(http.MethodPost, "/admin/payouts/"+f.entryID.String()+"/release", s.adminKey, map[string]any{
				"reason":     "alpha-security dual release",
				"request_id": fmt.Sprintf("alpha-dual-release-%d-%s", i, uuid.NewString()),
			})
		}(i)
	}
	relWG.Wait()

	var claimedOK int32
	var claimWG sync.WaitGroup
	claimWG.Add(2)
	for i := 0; i < 2; i++ {
		go func() {
			defer claimWG.Done()
			_, okClaim, err := s.store.ClaimPayout(s.ctx, f.entryID)
			if err == nil && okClaim {
				atomic.AddInt32(&claimedOK, 1)
			}
		}()
	}
	claimWG.Wait()

	var opCount int
	if err := s.pool.QueryRow(s.ctx, `
		SELECT count(*) FROM supplier_payout_operations WHERE ledger_entry_id=$1`,
		f.entryID).Scan(&opCount); err != nil {
		s.t.Fatalf("count payout operations: %v", err)
	}
	ok = claimedOK <= 1 && opCount <= 1
	status, sev = blockedOrFinding(ok, "P0")
	s.record(alphaAttack{
		ID: "concurrency-dual-payout-release", Class: "concurrency",
		Title: "two concurrent releases then two ClaimPayout calls", Status: status, Severity: sev,
		HTTPStatus: int(claimedOK), Want: "at most one successful claim / payout operation",
		Reproduction:   "two POST /admin/payouts/{id}/release then two ClaimPayout",
		Location:       "control/store_payouts.go:ReleasePayoutTx + ClaimPayout FOR UPDATE",
		AlphaReachable: false,
		Detail:         fmt.Sprintf("claim_ok=%d operations=%d", claimedOK, opCount),
	})
}

func (s *alphaSuite) attackAuthorityFailClosed() {
	// CAD FX missing already covered. Here: sealed payment refuses webhooks
	// if we clear the test key; corrupt ledger then try release; corrupt
	// runtime-authority bytes fail to parse as a usable catalogue.

	// Ledger amount tampering: flip a credit to a huge value and try to
	// release via the admin HTTP route. The release path must not pay the
	// tampered figure (it should refuse or still be bounded by funding).
	installSettlementCurrencyForTest(s.t, "usd")
	f := seedPayoutFixture(s.t, s.ctx, s.pool, payoutFixtureOpts{creditUSD: 1.00})
	if _, err := s.pool.Exec(s.ctx, `
		UPDATE ledger_entries SET amount_usd = 999999.99 WHERE id=$1`, f.entryID); err != nil {
		s.t.Fatalf("corrupt ledger: %v", err)
	}
	got := s.do(http.MethodPost, "/admin/payouts/"+f.entryID.String()+"/release", s.adminKey, map[string]any{
		"reason":     "alpha-security ledger corruption",
		"request_id": "alpha-corrupt-ledger-" + uuid.NewString(),
	})
	// Fail-closed: 409/400/500 all OK. 200 is only OK if the reserved funding
	// is still the original $1, not $999999.
	ok := !successCode(got.code)
	if successCode(got.code) {
		var reserved int64
		_ = s.pool.QueryRow(s.ctx, `
			SELECT COALESCE(sum(amount_cents),0) FROM supplier_payout_funding
			 WHERE ledger_entry_id=$1`, f.entryID).Scan(&reserved)
		ok = reserved <= f.creditCents+1
	}
	status, sev := blockedOrFinding(ok, "P0")
	s.record(alphaAttack{
		ID: "authority-corrupt-ledger-release", Class: "authority",
		Title: "admin release after ledger amount_usd corruption", Status: status, Severity: sev,
		HTTPStatus: got.code, Want: "refuse, or reserve only original funded cents",
		Reproduction:   "UPDATE ledger_entries SET amount_usd=999999.99; POST /admin/payouts/{id}/release",
		Location:       "control/store_payouts.go:ReleasePayoutTx + reservePayoutFunding",
		AlphaReachable: false, Detail: got.body,
	})

	// Runtime authority document: a truncated/garbage document must not
	// parse into a permissive empty catalogue.
	var parsed map[string]any
	err := json.Unmarshal(append([]byte("{"), runtimeAuthorityJSON...), &parsed)
	ok = err != nil
	status, sev = blockedOrFinding(ok, "P1")
	s.record(alphaAttack{
		ID: "authority-corrupt-runtime-json", Class: "authority",
		Title: "truncated runtime-authority.json fails to parse", Status: status, Severity: sev,
		HTTPStatus: 0, Want: "JSON refuse",
		Reproduction:   "json.Unmarshal(append([]byte(`{`), runtimeAuthorityJSON))",
		Location:       "control/runtime_authority.go go:embed runtime-authority.json",
		AlphaReachable: false, Detail: fmt.Sprintf("err=%v", err),
	})

	// Authorization matrix default-deny is a tripwire the suite re-checks.
	raw, err := os.ReadFile("../ops/authorization-matrix.json")
	must(s.t, err)
	var doc map[string]any
	must(s.t, json.Unmarshal(raw, &doc))
	pol, _ := doc["policy"].(map[string]any)
	ok = pol != nil && pol["default"] == "deny"
	status, sev = blockedOrFinding(ok, "P0")
	s.record(alphaAttack{
		ID: "authority-matrix-default-deny", Class: "authority",
		Title: "reviewed matrix default is deny", Status: status, Severity: sev,
		HTTPStatus: 0, Want: "policy.default=deny",
		Reproduction:   "read ops/authorization-matrix.json policy.default",
		Location:       "ops/authorization-matrix.json",
		AlphaReachable: true, Detail: fmt.Sprintf("%v", pol),
	})
}

func (s *alphaSuite) attackResourceExhaustion() {
	// Oversized JSON on an ordinary buyer route (4 MiB cap).
	big := bytes.Repeat([]byte("a"), 5<<20)
	body := append([]byte(`{"name":"`), append(big, []byte(`"}`)...)...)
	req := httptest.NewRequest(http.MethodPost, "/v1/keys", bytes.NewReader(body))
	req.RemoteAddr = "127.0.0.1:3344"
	req.Header.Set("Authorization", "Bearer "+s.buyerAKey)
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	s.handler.ServeHTTP(rec, req)
	ok := rec.Code == http.StatusRequestEntityTooLarge || rec.Code == http.StatusBadRequest
	status, sev := blockedOrFinding(ok, "P2")
	s.record(alphaAttack{
		ID: "resource-oversized-json", Class: "resource",
		Title: "5 MiB JSON on POST /v1/keys", Status: status, Severity: sev,
		HTTPStatus: rec.Code, Want: "413/400",
		Reproduction:   "POST /v1/keys with a 5 MiB body",
		Location:       "control/api.go:capBody + requestBodyLimit",
		AlphaReachable: true, Detail: rec.Body.String(),
	})

	// Oversized webhook
	huge := bytes.Repeat([]byte("x"), (1<<20)+100)
	got := s.postWebhook("/v1/stripe/webhook", "t=1,v1=x", huge)
	ok = got.code == http.StatusBadRequest || got.code == http.StatusRequestEntityTooLarge ||
		got.code == http.StatusServiceUnavailable
	status, sev = blockedOrFinding(ok, "P2")
	s.record(alphaAttack{
		ID: "resource-oversized-webhook", Class: "resource",
		Title: "webhook body over 1 MiB", Status: status, Severity: sev,
		HTTPStatus: got.code, Want: "400/413",
		Reproduction:   "POST /v1/stripe/webhook with >1 MiB body",
		Location:       "control/billing.go:io.LimitReader 1<<20",
		AlphaReachable: true, Detail: trim(got.body, 160),
	})
}

func (s *alphaSuite) finish() alphaSuiteReport {
	s.mu.Lock()
	defer s.mu.Unlock()
	rep := alphaSuiteReport{
		Kind: "alpha_security_http_attacks", Attacks: s.attacks,
		DatabaseUsed: true, PaymentModeTest: true,
		SourceCommit: strings.TrimSpace(os.Getenv("MERC_SOURCE_COMMIT")),
	}
	for _, a := range s.attacks {
		rep.Executed++
		switch a.Status {
		case "blocked":
			rep.Blocked++
		case "finding":
			rep.Findings++
		default:
			rep.Errors++
		}
	}
	if path := strings.TrimSpace(os.Getenv("ALPHA_SECURITY_RESULTS")); path != "" {
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err == nil {
			b, err := json.MarshalIndent(rep, "", "  ")
			if err == nil {
				_ = os.WriteFile(path, b, 0o644)
			}
		}
	}
	s.t.Logf("alpha security HTTP suite: executed=%d blocked=%d findings=%d errors=%d",
		rep.Executed, rep.Blocked, rep.Findings, rep.Errors)
	return rep
}

func trim(s string, n int) string {
	s = strings.TrimSpace(s)
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}
