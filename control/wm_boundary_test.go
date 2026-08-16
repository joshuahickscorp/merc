package main

import (
	"context"
	"fmt"
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

// wmEpistemicClasses is the directive X/XI closed set. A class not in this
// list is not representable; a rewrite that raises wm.epistemic_class_rank
// is an upgrade and must be refused.
var wmEpistemicClasses = []string{
	"CONTROL_PLANE_MEASURED",
	"EXTERNALLY_ATTESTED",
	"WORKER_CLAIM_CORROBORATED",
	"WORKER_CLAIM_UNCORROBORATED",
	"DERIVED",
	"SYNTHETIC",
	"COUNTERFACTUAL",
}

// TestWorldModelMeasurementCannotBeDeletedOrLaundered proves committed
// value_num / measurement_state cannot be erased or rewritten. The connecting
// role is exercised as well as wm_writer: GRANT/REVOKE is not the guard.
func TestWorldModelMeasurementCannotBeDeletedOrLaundered(t *testing.T) {
	t.Parallel()
	ctx, _, pool := openIsolatedTestStore(t)
	if err := BootstrapWorldModel(ctx, pool); err != nil {
		t.Fatalf("bootstrap world model: %v", err)
	}
	// Apply-twice: the measurement trigger/function must be idempotent.
	if err := BootstrapWorldModel(ctx, pool); err != nil {
		t.Fatalf("bootstrap world model a second time: %v", err)
	}

	mustAllow(t, execAsRole(t, pool, ctx, "wm_writer",
		`INSERT INTO wm.execution (id,authority_ref,workload_kind)
		 VALUES ('44444444-4444-4444-4444-444444444444','job:m','embed')`),
		"seed execution")
	mustAllow(t, execAsRole(t, pool, ctx, "wm_writer",
		`INSERT INTO wm.phase_observation (id,execution_id,phase,source,measurement_state,value_num)
		 VALUES ('55555555-5555-5555-5555-555555555555','44444444-4444-4444-4444-444444444444',
		         'decode','CONTROL_PLANE_MEASURED','MEASURED',12.5)`),
		"seed measured observation")
	mustAllow(t, execAsRole(t, pool, ctx, "wm_writer",
		`INSERT INTO wm.phase_observation (id,execution_id,phase,source,measurement_state)
		 VALUES ('66666666-6666-6666-6666-666666666666','44444444-4444-4444-4444-444444444444',
		         'queue','DERIVED','UNKNOWN')`),
		"seed unknown observation")

	const (
		measuredID = "55555555-5555-5555-5555-555555555555"
		unknownID  = "66666666-6666-6666-6666-666666666666"
	)
	deleteSQL := `DELETE FROM wm.phase_observation WHERE id='` + measuredID + `'`
	launderValueSQL := `UPDATE wm.phase_observation SET value_num=0 WHERE id='` + measuredID + `'`
	eraseValueSQL := `UPDATE wm.phase_observation SET value_num=NULL WHERE id='` + measuredID + `'`
	upgradeMeasureSQL := `UPDATE wm.phase_observation SET measurement_state='MEASURED', value_num=1 WHERE id='` + unknownID + `'`
	downgradeMeasureSQL := `UPDATE wm.phase_observation SET measurement_state='UNKNOWN', value_num=NULL WHERE id='` + measuredID + `'`

	// wm_writer path.
	mustDeny(t, execAsRole(t, pool, ctx, "wm_writer", deleteSQL),
		"cannot be deleted", "wm_writer DELETE measurement")
	mustDeny(t, execAsRole(t, pool, ctx, "wm_writer", launderValueSQL),
		"value_num/measurement_state is immutable", "wm_writer rewrite value_num")
	mustDeny(t, execAsRole(t, pool, ctx, "wm_writer", eraseValueSQL),
		"value_num/measurement_state is immutable", "wm_writer erase value_num")
	mustDeny(t, execAsRole(t, pool, ctx, "wm_writer", upgradeMeasureSQL),
		"value_num/measurement_state is immutable", "wm_writer UNKNOWN -> MEASURED")
	mustDeny(t, execAsRole(t, pool, ctx, "wm_writer", downgradeMeasureSQL),
		"value_num/measurement_state is immutable", "wm_writer MEASURED -> UNKNOWN")

	// Connecting role (superuser in this environment). The trigger, not the
	// grant, is what fails closed.
	mustDeny(t, execSQL(t, pool, ctx, deleteSQL),
		"cannot be deleted", "app user DELETE measurement")
	mustDeny(t, execSQL(t, pool, ctx, launderValueSQL),
		"value_num/measurement_state is immutable", "app user rewrite value_num")
	mustDeny(t, execSQL(t, pool, ctx, eraseValueSQL),
		"value_num/measurement_state is immutable", "app user erase value_num")
	mustDeny(t, execSQL(t, pool, ctx, upgradeMeasureSQL),
		"value_num/measurement_state is immutable", "app user UNKNOWN -> MEASURED")
	mustDeny(t, execSQL(t, pool, ctx, downgradeMeasureSQL),
		"value_num/measurement_state is immutable", "app user MEASURED -> UNKNOWN")

	// trust_state may still evolve; the measurement facts stay put.
	mustAllow(t, execAsRole(t, pool, ctx, "wm_writer",
		`UPDATE wm.phase_observation SET trust_state='CORROBORATED'
		 WHERE id='`+measuredID+`'`),
		"trust escalation still allowed")
	var state string
	var value *float64
	if err := pool.QueryRow(ctx,
		`SELECT measurement_state, value_num FROM wm.phase_observation WHERE id=$1`,
		measuredID).Scan(&state, &value); err != nil {
		t.Fatalf("reread measured row: %v", err)
	}
	if state != "MEASURED" || value == nil || *value != 12.5 {
		t.Fatalf("measurement fact mutated under a legal trust update: state=%q value=%v", state, value)
	}
}

// TestWorldModelEpistemicClassesAreRepresentableAndRefuseUpgrade asserts the
// seven directive classes insert, have a total rank order, and cannot be
// rewritten into a stronger class (WORKER_CLAIM_UNCORROBORATED ->
// CONTROL_PLANE_MEASURED is the named case).
func TestWorldModelEpistemicClassesAreRepresentableAndRefuseUpgrade(t *testing.T) {
	t.Parallel()
	ctx, _, pool := openIsolatedTestStore(t)
	if err := BootstrapWorldModel(ctx, pool); err != nil {
		t.Fatalf("bootstrap world model: %v", err)
	}
	mustAllow(t, execAsRole(t, pool, ctx, "wm_writer",
		`INSERT INTO wm.execution (id,authority_ref,workload_kind)
		 VALUES ('77777777-7777-7777-7777-777777777777','job:e','embed')`),
		"seed execution")

	ranks := make(map[string]int, len(wmEpistemicClasses))
	for i, class := range wmEpistemicClasses {
		id := fmt.Sprintf("aaaaaaaa-bbbb-cccc-dddd-%012d", i+1)
		mustAllow(t, execAsRole(t, pool, ctx, "wm_writer",
			`INSERT INTO wm.phase_observation (id,execution_id,phase,source,measurement_state)
			 VALUES ('`+id+`','77777777-7777-7777-7777-777777777777','queue','`+class+`','UNKNOWN')`),
			"representable "+class)

		var rank *int
		if err := pool.QueryRow(ctx, `SELECT wm.epistemic_class_rank($1)`, class).Scan(&rank); err != nil {
			t.Fatalf("rank(%s): %v", class, err)
		}
		if rank == nil {
			t.Fatalf("rank(%s) is NULL — class is not representable in wm.epistemic_class_rank", class)
		}
		ranks[class] = *rank
	}

	// Strict total order, strongest first. A silent reorder is an upgrade path.
	for i := 0; i < len(wmEpistemicClasses)-1; i++ {
		stronger, weaker := wmEpistemicClasses[i], wmEpistemicClasses[i+1]
		if ranks[stronger] <= ranks[weaker] {
			t.Fatalf("rank order broken: %s (%d) must be stronger than %s (%d)",
				stronger, ranks[stronger], weaker, ranks[weaker])
		}
	}

	var unknownRank *int
	if err := pool.QueryRow(ctx, `SELECT wm.epistemic_class_rank('MAGIC')`).Scan(&unknownRank); err != nil {
		t.Fatalf("rank(MAGIC): %v", err)
	}
	if unknownRank != nil {
		t.Fatalf("unknown class must not have a rank, got %d", *unknownRank)
	}

	// Every stronger rewrite is refused. The named example is UNCORROBORATED -> MEASURED.
	for i, src := range wmEpistemicClasses {
		srcID := fmt.Sprintf("aaaaaaaa-bbbb-cccc-dddd-%012d", i+1)
		for _, dst := range wmEpistemicClasses[:i] {
			label := src + " -> " + dst
			mustDeny(t, execAsRole(t, pool, ctx, "wm_writer",
				`UPDATE wm.phase_observation SET source='`+dst+`' WHERE id='`+srcID+`'`),
				"immutable provenance", "upgrade "+label)
			mustDeny(t, execSQL(t, pool, ctx,
				`UPDATE wm.phase_observation SET source='`+dst+`' WHERE id='`+srcID+`'`),
				"immutable provenance", "app user upgrade "+label)
		}
	}

	mustDeny(t, execAsRole(t, pool, ctx, "wm_writer",
		`UPDATE wm.phase_observation SET source='CONTROL_PLANE_MEASURED'
		 WHERE id='aaaaaaaa-bbbb-cccc-dddd-000000000004'`),
		"immutable provenance", "UNCORROBORATED -> MEASURED")
}

// authorityWriteTargets are representative money / settlement / pricing /
// capability / trust / eligibility tables. The privilege scan below also
// refuses a write grant on ANY public table; this list is what the directive
// named, and each table must exist so a rename cannot silently drop coverage.
var authorityWriteTargets = []struct {
	category string
	table    string
}{
	{"money", "ledger_entries"},
	{"money", "buyer_prepaid_balances"},
	{"money", "buyer_charge_operations"},
	{"settlement", "job_cost_settlements"},
	{"settlement", "realtime_settlements"},
	{"settlement", "supplier_minor_unit_settlements"},
	{"pricing", "catalogue_price_schedules"},
	{"pricing", "model_price_history"},
	{"pricing", "quotes"},
	{"capability", "worker_authorized_capabilities"},
	{"capability", "worker_capability_snapshots"},
	{"capability", "runtime_profile_capabilities"},
	{"trust", "suppliers"},
	{"trust", "realtime_supplier_outcome_stats"},
	{"eligibility", "runtime_activation_policies"},
	{"eligibility", "claim_independence_exclusions"},
}

// TestWorldModelCannotWriteAuthorityTables proves no wm runtime role can write
// money, settlement, pricing, capability, trust or eligibility tables — and
// that the same is true of every other public (authority) table.
func TestWorldModelCannotWriteAuthorityTables(t *testing.T) {
	t.Parallel()
	ctx, _, pool := openIsolatedTestStore(t)
	if err := BootstrapWorldModel(ctx, pool); err != nil {
		t.Fatalf("bootstrap world model: %v", err)
	}

	for _, target := range authorityWriteTargets {
		var n int
		if err := pool.QueryRow(ctx,
			`SELECT count(*) FROM pg_class c
			 JOIN pg_namespace n ON n.oid = c.relnamespace
			 WHERE n.nspname = 'public' AND c.relkind = 'r' AND c.relname = $1`,
			target.table).Scan(&n); err != nil {
			t.Fatalf("lookup %s.%s: %v", target.category, target.table, err)
		}
		if n != 1 {
			t.Fatalf("authority table %s (%s) is missing — coverage would silently drop", target.table, target.category)
		}
	}

	rows, err := pool.Query(ctx, `
		SELECT n.nspname || '.' || c.relname AS tbl, r.rolname, p.priv
		  FROM pg_class c
		  JOIN pg_namespace n ON n.oid = c.relnamespace
		 CROSS JOIN (VALUES ('wm_writer'), ('wm_reader')) AS r(rolname)
		 CROSS JOIN (VALUES ('INSERT'), ('UPDATE'), ('DELETE'), ('TRUNCATE')) AS p(priv)
		 WHERE n.nspname = 'public'
		   AND c.relkind = 'r'
		   AND has_table_privilege(r.rolname, c.oid, p.priv)
		 ORDER BY 1, 2, 3`)
	if err != nil {
		t.Fatalf("privilege scan: %v", err)
	}
	defer rows.Close()
	var leaks []string
	for rows.Next() {
		var tbl, role, priv string
		if err := rows.Scan(&tbl, &role, &priv); err != nil {
			t.Fatalf("scan privilege: %v", err)
		}
		leaks = append(leaks, role+" "+priv+" on "+tbl)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("privilege scan rows: %v", err)
	}
	if len(leaks) > 0 {
		t.Fatalf("wm role write grant on authority: %s", strings.Join(leaks, "; "))
	}

	for _, target := range authorityWriteTargets {
		for _, role := range []string{"wm_writer", "wm_reader"} {
			label := role + " DELETE " + target.category + "." + target.table
			mustDeny(t, execAsRole(t, pool, ctx, role,
				`DELETE FROM `+target.table),
				"permission denied", label)
			mustDeny(t, execAsRole(t, pool, ctx, role,
				`INSERT INTO `+target.table+` DEFAULT VALUES`),
				"permission denied", role+" INSERT "+target.category+"."+target.table)
		}
	}
}

func execSQL(t *testing.T, pool *pgxpool.Pool, ctx context.Context, sql string) error {
	t.Helper()
	_, err := pool.Exec(ctx, sql)
	return err
}
