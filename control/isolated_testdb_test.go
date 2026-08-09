package main

import (
	"context"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
)

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
	if _, err := adminPool.Exec(ctx, `CREATE DATABASE `+name); err != nil {
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
	pool, err := pgxpool.New(ctx, own.String())
	mustf(t, err, "connect isolated database: %v")
	t.Cleanup(pool.Close)

	store := NewStore(pool)
	mustf(t, store.Migrate(ctx), "apply canonical schema to isolated database: %v")
	return ctx, store, pool
}
