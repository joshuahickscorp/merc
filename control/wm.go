package main

import (
	"context"
	_ "embed"

	"github.com/jackc/pgx/v5/pgxpool"
)

// World Model V2.2 foundation (directive X/XI). The world model observes,
// estimates, predicts and replays; it is NOT authority. Its schema and roles are
// bootstrapped separately from the authority Migrate() so a deployment can run
// the observation ingester as wm_writer — a role that, by construction, cannot
// accept, price, clear, route, place, verify or settle.
//
// The separation is proven at the database by control/wm_boundary_test.go:
// wm_writer/wm_reader cannot write or DDL any authority (public) object.

//go:embed wm_schema.sql
var wmSchemaSQL string

// BootstrapWorldModel applies the wm schema, roles and grants. It is idempotent
// and is an owner/admin operation, deliberately NOT part of the runtime
// authority Migrate(): the world model must never be a boot dependency of the
// acceptance/money path. Callers connect with a role that can CREATE ROLE and
// owns (or can create) the wm schema.
func BootstrapWorldModel(ctx context.Context, pool *pgxpool.Pool) error {
	_, err := pool.Exec(ctx, wmSchemaSQL)
	return err
}
