package main

import (
	"context"
	"os"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

// TestMutationTemplateSchema deliberately has no product assertion.  The
// mutation supervisor invokes it only with MERC_MUTATION_TEMPLATE_DB=1 to make
// a schema-only, exact-source PostgreSQL template.  Every contract still gets
// a fresh PostgreSQL clone of that template and calls Store.Migrate itself.
// Keeping this opt-in avoids changing ordinary test-suite database semantics.
func TestMutationTemplateSchema(t *testing.T) {
	if os.Getenv("MERC_MUTATION_TEMPLATE_DB") != "1" {
		t.Skip("mutation database template preparation is supervisor-only")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	pool, err := pgxpool.New(ctx, requireTestDatabase(t))
	mustf(t, err, "connect mutation template database: %v")
	defer pool.Close()
	mustf(t, NewStore(pool).Migrate(ctx), "migrate mutation template database: %v")
	if os.Getenv("MERC_SCHEMA_TEMPLATE_APPLY_TWICE") == "1" {
		mustf(t, NewStore(pool).Migrate(ctx), "re-apply canonical schema for template stamp: %v")
	}
}
