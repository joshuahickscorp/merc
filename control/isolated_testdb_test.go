package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"net/url"
	"os"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
)

// isolatedTestDBTemplateEnv is the same opt-in the suite wrapper already
// understands (scripts/with-isolated-test-db.sh). When set to a schema-stamped
// template, every isolated database is a PostgreSQL clone instead of an
// 8164-line schema apply.
const isolatedTestDBTemplateEnv = "MERC_ISOLATED_TEST_DB_TEMPLATE"

// schemaTemplateNamePrefix is the Makefile/ensure-schema-template cache.
// Names are merc_schema_<first 16 hex chars of sha256(schema.sql)>. A stale
// cache from an older schema.sql cannot silently satisfy current tests.
const schemaTemplateNamePrefix = "merc_schema_"

var isolatedTemplateNameRe = regexp.MustCompile(`^[a-z][a-z0-9_]{2,60}$`)

// defaultIsolatedTestMaxConns keeps 16 parallel isolated tests inside a
// default PostgreSQL max_connections=100 budget while leaving enough
// connections for one verification process (3) plus headroom (2).
const defaultIsolatedTestMaxConns int32 = 5

// openIsolatedTestStore gives one test its own database.
//
// Most integration tests share a database and assert on their own rows, which is
// fine. A few assert on genuinely PLATFORM-WIDE state -- the payout pause engages
// while ANY ledger row anywhere is reversal_required -- and those cannot be made
// correct on a shared database: sibling rows keep the platform legitimately
// paused, and supplier_payout_operations is deliberately append-only so the
// leftovers cannot be cleaned up either.
//
// Rather than weaken the assertion until it passes, give the test the isolation
// its subject actually requires.
func openIsolatedTestStore(t *testing.T) (context.Context, *Store, *pgxpool.Pool) {
	t.Helper()
	return openIsolatedDatabase(t, 0)
}

func openIsolatedTestStoreWithMaxConns(t *testing.T, maxConns int32) (*Store, *pgxpool.Pool) {
	t.Helper()
	_, store, pool := openIsolatedDatabase(t, maxConns)
	return store, pool
}

func openIsolatedDatabase(t *testing.T, maxConns int32) (context.Context, *Store, *pgxpool.Pool) {
	t.Helper()
	template := strings.TrimSpace(os.Getenv(isolatedTestDBTemplateEnv))
	if err := validateIsolatedTestDBTemplate(template); err != nil {
		t.Fatal(err)
	}
	// Migrate/loadActivationAtStartup adopts a process-wide activation snapshot.
	// Restore the caller's snapshot so one DB test cannot quarantine later pure
	// unit tests that read currentActivation().
	previousActivation := activeRuntimeActivation.Load()
	t.Cleanup(func() { activeRuntimeActivation.Store(previousActivation) })

	base := requireTestDatabase(t)

	parsed, err := url.Parse(base)
	mustf(t, err, "parse MERC_TEST_DATABASE_URL: %v")
	name := "cx_iso_" + strings.ReplaceAll(uuid.NewString(), "-", "")[:24]

	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	t.Cleanup(cancel)

	admin := *parsed
	admin.Path = "/postgres"
	adminPool, err := pgxpool.New(ctx, admin.String())
	mustf(t, err, "connect to postgres for database creation: %v")
	createSQL := `CREATE DATABASE ` + name
	if template != "" {
		// STRATEGY file_copy skips the wal_log copy PostgreSQL 15+ does by default.
		// These are ephemeral per-test clones dropped at cleanup, so crash-safe WAL
		// logging of the copy buys nothing and costs ~2x the clone time (0.29s ->
		// 0.15s here) plus WAL/fsync pressure on the single checkpointer — which is
		// the isolated-DB suite's real serialization tax, not the copy itself.
		createSQL += ` TEMPLATE ` + template + ` STRATEGY file_copy`
	}
	if _, err := adminPool.Exec(ctx, createSQL); err != nil {
		adminPool.Close()
		t.Fatalf("create isolated database: %v", err)
	}
	adminPool.Close()

	t.Cleanup(func() {
		c, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		p, err := pgxpool.New(c, admin.String())
		if err != nil {
			return
		}
		defer p.Close()
		_, _ = p.Exec(c, `DROP DATABASE IF EXISTS `+name+` WITH (FORCE)`)
	})

	own := *parsed
	own.Path = "/" + name
	cfg, err := pgxpool.ParseConfig(own.String())
	mustf(t, err, "parse isolated database config: %v")
	if maxConns <= 0 {
		maxConns = defaultIsolatedTestMaxConns
	}
	cfg.MaxConns = maxConns
	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	mustf(t, err, "connect isolated database: %v")
	t.Cleanup(pool.Close)

	store := NewStore(pool)
	if template == "" {
		mustf(t, store.Migrate(ctx), "apply canonical schema to isolated database: %v")
	} else {
		// Template clone already has schema.sql. Re-apply only catalog/profile
		// sync and activation load so in-memory TEST_ONLY authority installed
		// before this helper still seeds the isolated database.
		mustf(t, store.adoptMigratedSchema(ctx), "adopt template clone: %v")
	}
	return ctx, store, pool
}

func canonicalSchemaSHA256() string {
	sum := sha256.Sum256([]byte(canonicalSchema))
	return hex.EncodeToString(sum[:])
}

func schemaTemplateDatabaseName(sha256hex string) string {
	if len(sha256hex) < 16 {
		return ""
	}
	return schemaTemplateNamePrefix + "ddl_" + sha256hex[:16]
}

func validateIsolatedTestDBTemplate(name string) error {
	if name == "" {
		return nil
	}
	if !isolatedTemplateNameRe.MatchString(name) {
		return fmt.Errorf("%s: invalid template database name %q", isolatedTestDBTemplateEnv, name)
	}
	if strings.HasPrefix(name, schemaTemplateNamePrefix) {
		want := schemaTemplateDatabaseName(canonicalSchemaSHA256())
		if name != want {
			return fmt.Errorf("%s=%q is stale for the embedded control/schema.sql (want %s); refusing to clone an old schema",
				isolatedTestDBTemplateEnv, name, want)
		}
	}
	return nil
}
