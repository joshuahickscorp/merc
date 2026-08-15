package main

import (
	"context"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"
)

// execAsRole runs one statement on a single pooled connection under SET ROLE and
// resets the role before returning the connection to the pool. It returns the
// statement's error (nil on success) so callers can assert allow/deny.
func execAsRole(t *testing.T, pool *pgxpool.Pool, ctx context.Context, role, sql string) error {
	t.Helper()
	conn, err := pool.Acquire(ctx)
	if err != nil {
		t.Fatalf("acquire conn: %v", err)
	}
	defer conn.Release()
	if _, err := conn.Exec(ctx, "SET ROLE "+role); err != nil {
		t.Fatalf("SET ROLE %s: %v", role, err)
	}
	defer conn.Exec(ctx, "RESET ROLE")
	_, err = conn.Exec(ctx, sql)
	return err
}

func mustDeny(t *testing.T, err error, want, label string) {
	t.Helper()
	if err == nil {
		t.Fatalf("%s: expected refusal %q, got success", label, want)
	}
	if !strings.Contains(err.Error(), want) {
		t.Fatalf("%s: expected error containing %q, got %v", label, want, err)
	}
}

func mustAllow(t *testing.T, err error, label string) {
	t.Helper()
	if err != nil {
		t.Fatalf("%s: expected success, got %v", label, err)
	}
}

// TestWorldModelBoundaryFailsClosedAtTheDatabase is the security proof for the
// World Model V2.2 separation law: the world-model runtime roles cannot touch
// authority. If this ever passes for a forbidden write, the separation is a lie.
func TestWorldModelBoundaryFailsClosedAtTheDatabase(t *testing.T) {
	t.Parallel()
	ctx, _, pool := openIsolatedTestStore(t)
	if err := BootstrapWorldModel(ctx, pool); err != nil {
		t.Fatalf("bootstrap world model: %v", err)
	}

	// wm_writer must not reach authority (public) at all, and must not DDL.
	mustDeny(t, execAsRole(t, pool, ctx, "wm_writer",
		`INSERT INTO buyers (id,email) VALUES (gen_random_uuid(),'x@y.z')`),
		"permission denied", "wm_writer INSERT authority.buyers")
	mustDeny(t, execAsRole(t, pool, ctx, "wm_writer",
		`UPDATE jobs SET status='x'`),
		"permission denied", "wm_writer UPDATE authority.jobs")
	mustDeny(t, execAsRole(t, pool, ctx, "wm_writer",
		`CREATE TABLE public.evil (x int)`),
		"permission denied", "wm_writer CREATE TABLE public")
	mustDeny(t, execAsRole(t, pool, ctx, "wm_writer",
		`CREATE TABLE wm.evil (x int)`),
		"permission denied", "wm_writer DDL in wm")

	// wm_writer may append world-model observations.
	mustAllow(t, execAsRole(t, pool, ctx, "wm_writer",
		`INSERT INTO wm.execution (id,authority_ref,workload_kind)
		 VALUES ('11111111-1111-1111-1111-111111111111','job:abc','embed')`),
		"wm_writer INSERT wm.execution")

	// wm_reader is read-only, everywhere.
	mustDeny(t, execAsRole(t, pool, ctx, "wm_reader",
		`INSERT INTO wm.execution (authority_ref,workload_kind) VALUES ('job:x','embed')`),
		"permission denied", "wm_reader INSERT wm.execution")
	mustDeny(t, execAsRole(t, pool, ctx, "wm_reader",
		`INSERT INTO buyers (id,email) VALUES (gen_random_uuid(),'x@y.z')`),
		"permission denied", "wm_reader INSERT authority.buyers")
	mustAllow(t, execAsRole(t, pool, ctx, "wm_reader",
		`SELECT count(*) FROM wm.execution`),
		"wm_reader SELECT wm.execution")
}

// TestWorldModelEpistemicInvariants proves UNKNOWN is never silently 0, bad
// provenance is rejected, and a source can never be laundered into a stronger
// class (a WORKER_CLAIM cannot become CONTROL_PLANE_MEASURED). trust_state may
// still escalate.
func TestWorldModelEpistemicInvariants(t *testing.T) {
	t.Parallel()
	ctx, _, pool := openIsolatedTestStore(t)
	if err := BootstrapWorldModel(ctx, pool); err != nil {
		t.Fatalf("bootstrap world model: %v", err)
	}
	mustAllow(t, execAsRole(t, pool, ctx, "wm_writer",
		`INSERT INTO wm.execution (id,authority_ref,workload_kind)
		 VALUES ('22222222-2222-2222-2222-222222222222','job:e','embed')`),
		"seed execution")

	// UNKNOWN with a numeric value is a lie (UNKNOWN=0). Refuse it.
	mustDeny(t, execAsRole(t, pool, ctx, "wm_writer",
		`INSERT INTO wm.phase_observation (execution_id,phase,source,measurement_state,value_num)
		 VALUES ('22222222-2222-2222-2222-222222222222','queue','DERIVED','UNKNOWN',5)`),
		"wm_phase_value_only_when_measured", "UNKNOWN carries a value")

	// Unknown provenance class is refused.
	mustDeny(t, execAsRole(t, pool, ctx, "wm_writer",
		`INSERT INTO wm.phase_observation (execution_id,phase,source,measurement_state)
		 VALUES ('22222222-2222-2222-2222-222222222222','queue','MAGIC','MEASURED')`),
		"wm_phase_source_valid", "bad provenance class")

	// A committed WORKER_CLAIM observation.
	mustAllow(t, execAsRole(t, pool, ctx, "wm_writer",
		`INSERT INTO wm.phase_observation (id,execution_id,phase,source,measurement_state)
		 VALUES ('33333333-3333-3333-3333-333333333333','22222222-2222-2222-2222-222222222222',
		         'decode','WORKER_CLAIM_UNCORROBORATED','MEASURED')`),
		"seed worker claim")

	// trust_state may escalate.
	mustAllow(t, execAsRole(t, pool, ctx, "wm_writer",
		`UPDATE wm.phase_observation SET trust_state='CORROBORATED'
		 WHERE id='33333333-3333-3333-3333-333333333333'`),
		"trust escalation allowed")

	// source may NOT escalate — immutable provenance.
	mustDeny(t, execAsRole(t, pool, ctx, "wm_writer",
		`UPDATE wm.phase_observation SET source='CONTROL_PLANE_MEASURED'
		 WHERE id='33333333-3333-3333-3333-333333333333'`),
		"immutable provenance", "source laundering blocked")
}
