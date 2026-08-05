package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
)

func TestCreateAPIKeySQLHardcodesIsAdminFalse(t *testing.T) {
	// Regression: ordinary buyer mint paths must never be able to set is_admin.
	if !strings.Contains(createAPIKeySQL, "is_admin, revoked") {
		t.Fatal("createAPIKeySQL must insert is_admin explicitly")
	}
	if !strings.Contains(createAPIKeySQL, "false, false") {
		t.Fatal("createAPIKeySQL must hardcode is_admin=false")
	}
	if strings.Contains(strings.ToLower(createAPIKeySQL), "$5") {
		t.Fatal("createAPIKeySQL must not accept is_admin as a bind parameter")
	}
}

func TestCreateAPIKeyNeverMintsAdmin(t *testing.T) {
	ctx, store, pool := openAdminMutationTestStore(t)
	buyerID := uuid.New()
	if _, err := pool.Exec(ctx, `INSERT INTO buyers (id, email) VALUES ($1, $2)`,
		buyerID, buyerID.String()+"@create-key.invalid"); err != nil {
		t.Fatal(err)
	}
	id, raw, _, err := store.CreateAPIKey(ctx, buyerID, "ordinary", true)
	must(t, err)
	if raw == "" || id == uuid.Nil {
		t.Fatal("expected a minted buyer key")
	}
	var isAdmin bool
	must(t, pool.QueryRow(ctx, `SELECT is_admin FROM api_keys WHERE id=$1`, id).Scan(&isAdmin))
	if isAdmin {
		t.Fatal("CreateAPIKey minted an admin key through the ordinary buyer path")
	}
	// AuthenticateAdmin must reject the ordinary buyer key.
	if _, err := store.AuthenticateAdmin(ctx, raw); err == nil {
		t.Fatal("buyer API key was accepted as an admin principal")
	}
}

func TestAuthenticateAdminPrefersOperatorCredentialsOverBreakGlass(t *testing.T) {
	ctx, store, pool := openAdminMutationTestStore(t)
	// Unique per run: admin_credentials.key_hash is UNIQUE, so a fixed literal
	// makes the test pass once and then collide forever on a reused database.
	raw := "cx_admin_" + strings.ReplaceAll(uuid.NewString(), "-", "") + strings.Repeat("ab", 8)
	sum := sha256.Sum256([]byte(raw))
	hash := hex.EncodeToString(sum[:])
	opID := uuid.New()
	breakID := uuid.New()
	if _, err := pool.Exec(ctx, `
		INSERT INTO admin_credentials (id, key_hash, label, revoked)
		VALUES ($1, $2, 'primary-operator', false)`, opID, hash); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `
		INSERT INTO api_keys (id, key_hash, is_admin, revoked, name)
		VALUES ($1, $2, true, false, 'legacy-break-glass')`, breakID, hash); err != nil {
		t.Fatal(err)
	}
	actor, err := store.AuthenticateAdmin(ctx, raw)
	must(t, err)
	if actor.Mode != AdminAuthOperatorKey || actor.PrincipalID != opID {
		t.Fatalf("expected operator_key principal %s, got mode=%s id=%s", opID, actor.Mode, actor.PrincipalID)
	}
	if actor.Label != "primary-operator" {
		t.Fatalf("label = %q", actor.Label)
	}
}

func TestAuthenticateAdminBreakGlassMigrationPath(t *testing.T) {
	ctx, store, pool := openAdminMutationTestStore(t)
	raw := "cx_admin_breakglass_" + uuid.NewString()
	sum := sha256.Sum256([]byte(raw))
	hash := hex.EncodeToString(sum[:])
	id := uuid.New()
	if _, err := pool.Exec(ctx, `
		INSERT INTO api_keys (id, key_hash, is_admin, revoked, name)
		VALUES ($1, $2, true, false, 'migration-break-glass')`, id, hash); err != nil {
		t.Fatal(err)
	}
	actor, err := store.AuthenticateAdmin(ctx, raw)
	must(t, err)
	if actor.Mode != AdminAuthBreakGlassAPIKey || actor.PrincipalID != id {
		t.Fatalf("expected break-glass principal %s, got mode=%s id=%s", id, actor.Mode, actor.PrincipalID)
	}
}

func TestConstantTimeHashEqual(t *testing.T) {
	a := hashKey("same-credential-value")
	if !constantTimeHashEqual(a, a) {
		t.Fatal("identical digests must match")
	}
	if constantTimeHashEqual(a, hashKey("different")) {
		t.Fatal("different digests must not match")
	}
	if constantTimeHashEqual(a, a[:len(a)-1]) {
		t.Fatal("length mismatch must not match")
	}
}

func TestAdminSourceAllowlist(t *testing.T) {
	t.Setenv("MERC_ADMIN_CIDRS", "")
	if !adminSourceAllowed("127.0.0.1") || !adminSourceAllowed("::1") {
		t.Fatal("unset MERC_ADMIN_CIDRS must still allow loopback")
	}
	if adminSourceAllowed("8.8.8.8") {
		t.Fatal("unset MERC_ADMIN_CIDRS must refuse non-loopback")
	}

	t.Setenv("MERC_ADMIN_CIDRS", "10.0.0.0/8,192.0.2.10/32")
	if !adminSourceAllowed("10.1.2.3") || !adminSourceAllowed("192.0.2.10") {
		t.Fatal("allowlisted addresses were refused")
	}
	if adminSourceAllowed("11.0.0.1") || adminSourceAllowed("127.0.0.1") {
		t.Fatal("addresses outside MERC_ADMIN_CIDRS were accepted")
	}

	t.Setenv("MERC_ADMIN_CIDRS", "not-a-cidr")
	if adminSourceAllowed("10.0.0.1") {
		t.Fatal("malformed allowlist must fail closed")
	}
}

func TestValidateAdminAccessConfigRequiresCIDRsInProduction(t *testing.T) {
	if err := validateAdminAccessConfig("production", ""); err == nil {
		t.Fatal("production without MERC_ADMIN_CIDRS must refuse to start")
	}
	must(t, validateAdminAccessConfig("production", "10.0.0.0/8"))
	must(t, validateAdminAccessConfig("development", ""))
	if err := validateAdminAccessConfig("development", "bad"); err == nil {
		t.Fatal("malformed CIDRs must be refused in any env")
	}
}

func TestAuthAdminEnforcesIPAllowlistAndSeparatePrincipal(t *testing.T) {
	databaseURL := os.Getenv("MERC_TEST_DATABASE_URL")
	if databaseURL == "" {
		t.Skip("MERC_TEST_DATABASE_URL is not set")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
	t.Cleanup(cancel)
	pool, err := pgxpool.New(ctx, databaseURL)
	must(t, err)
	t.Cleanup(pool.Close)
	store := NewStore(pool)
	must(t, store.Migrate(ctx))

	raw := "cx_admin_http_" + uuid.NewString()
	sum := sha256.Sum256([]byte(raw))
	hash := hex.EncodeToString(sum[:])
	if _, err := pool.Exec(ctx, `
		INSERT INTO admin_credentials (key_hash, label, revoked)
		VALUES ($1, 'http-operator', false)`, hash); err != nil {
		t.Fatal(err)
	}

	t.Setenv("MERC_ADMIN_CIDRS", "10.0.0.0/8")
	srv := NewServer(store, nil, nil, nil)
	handler := srv.Routes()

	// Non-allowlisted source with valid key → 403
	req := httptest.NewRequest(http.MethodGet, "/admin/controls", nil)
	req.RemoteAddr = "8.8.8.8:12345"
	req.Header.Set("Authorization", "Bearer "+raw)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("non-allowlisted admin source: status %d, want 403", rec.Code)
	}

	// Allowlisted source with valid operator key → not 401/403 from auth
	// (handler may 500 without full wiring; auth must pass).
	req = httptest.NewRequest(http.MethodGet, "/admin/controls", nil)
	req.RemoteAddr = "10.1.2.3:12345"
	req.Header.Set("Authorization", "Bearer "+raw)
	rec = httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code == http.StatusUnauthorized || rec.Code == http.StatusForbidden {
		t.Fatalf("allowlisted operator key rejected by auth: status %d body %s", rec.Code, rec.Body.String())
	}

	// Buyer-shaped key is not an admin principal.
	buyerRaw := "cx_test_not_admin_" + uuid.NewString()
	buyerID := uuid.New()
	if _, err := pool.Exec(ctx, `INSERT INTO buyers (id, email) VALUES ($1,$2)`,
		buyerID, buyerID.String()+"@admin-auth.invalid"); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `
		INSERT INTO api_keys (buyer_id, key_hash, is_admin, revoked, name)
		VALUES ($1, $2, false, false, 'buyer')`, buyerID, hashKey(buyerRaw)); err != nil {
		t.Fatal(err)
	}
	req = httptest.NewRequest(http.MethodGet, "/admin/controls", nil)
	req.RemoteAddr = "10.1.2.3:12345"
	req.Header.Set("Authorization", "Bearer "+buyerRaw)
	rec = httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("buyer key on admin route: status %d, want 401", rec.Code)
	}
}
