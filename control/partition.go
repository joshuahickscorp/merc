package main

import (
	"context"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

// partition.go — Postgres Data Lifecycle 6->7 (docs/internal/CREED_AND_PATH_TO_TEN.md):
// move the three high-churn telemetry tables (worker_memory_samples, task_durations,
// job_events — control/workers.go telemetryTables) from delete-based pruning to
// PARTITION-based lifecycle management.
//
// Why. The hourly retention sweep (sweepTelemetryRetention) prunes these tables with
// a row-by-row `DELETE ... WHERE created_at < cutoff`. On a busy queue that is a
// churn-heavy, bloat-prone O(rows) operation that competes with autovacuum (the
// sizing doc docs/POSTGRES_TELEMETRY_SIZING.md quantifies ~72K wms + ~21K td rows
// deleted PER HOUR at the 600-worker target). Declarative RANGE partitioning by the
// existing created_at column turns "drop everything older than N days" into an O(1)
// metadata `DROP TABLE partition` — the whole month's heap + indexes vanish at once,
// no dead tuples, no vacuum debt.
//
// Safety model — this is a real, risky schema migration, and every choice here is
// made to be provably row-preserving and idempotent on an EXISTING populated DB:
//
//   - Conversion runs in ONE transaction per table (PostgreSQL DDL is transactional):
//     rename old -> create partitioned parent -> pre-create month partitions covering
//     every existing row's month -> copy all rows -> drop old -> assert count. Any
//     failure rolls the whole thing back to the original table untouched. There is no
//     window in which rows are half-migrated.
//
//   - Month partitions are PRE-CREATED across the full historical span BEFORE the row
//     copy, so every historical row routes into a proper (droppable) month partition.
//     A catch-all DEFAULT partition exists only as a safety net for an edge row
//     outside the pre-created window — real inserts never land there because the
//     rotation job always keeps months created ahead of now().
//
//   - The composite PRIMARY KEY (created_at, id) is required by Postgres (a partition
//     key column must be in every unique constraint). No code looks these rows up by
//     id, no FK references them, and gen_random_uuid() collision is astronomically
//     improbable, so global id-uniqueness is preserved in practice while satisfying
//     the partitioning constraint. The old single-column PK on id is dropped as part
//     of the swap.
//
//   - The existing DELETE-based sweep (Store.DeleteOld*, sweepTelemetryRetention) keeps
//     working UNCHANGED against the partitioned parent (a DELETE cascades to leaves), so
//     it is retained as defense-in-depth: partition-drop removes whole expired MONTHS in
//     O(1), and the row-level DELETE trims the sub-month tail (rows older than the exact
//     cutoff but still inside a live boundary partition). Exact retention semantics are
//     therefore preserved to the row while the bulk of the work becomes O(1).
//
//   - Autovacuum storage params (db/schema.sql / store.go's Migrate) cannot sit on a
//     partitioned PARENT — Postgres requires them on the leaf partitions — so they are
//     applied to every partition at creation time (both here and in the rotation job).

// telemetryPartitionSpec describes one partitioned telemetry table: its name, the
// per-partition autovacuum storage params (mirroring the pre-partition tuning in
// db/schema.sql, now applied per leaf), the retention window that drives drop-expired,
// and the exact column list used to rebuild the parent. Column lists are this
// codebase's own fixed schema, never request input, so the interpolation below carries
// no injection risk — same discipline as TelemetryTableCounts.
type telemetryPartitionSpec struct {
	table string
	// columns is the exact CREATE-parent column body (everything except the PK line),
	// matching db/schema.sql. created_at is forced NOT NULL here (a RANGE partition key
	// row cannot be NULL); every existing row already has a non-NULL created_at (the
	// column has defaulted to now() since creation), so tightening the constraint on
	// copy never rejects a real row.
	columns string
	// secondaryIndex is the one non-PK index each table carries, recreated on the parent
	// (Postgres propagates it to every leaf). Name matches db/schema.sql exactly.
	secondaryIndexName string
	secondaryIndexBody string // the "(cols...)" part
	// autovacuum is the leaf-level storage-param clause (WITH (...)), mirroring the
	// per-table tuning db/schema.sql applied when these were plain tables.
	autovacuum string
	// retention is how far back rows are kept; the rotation job drops any partition
	// whose entire range is older than now()-retention.
	retention time.Duration
}

// telemetryPartitionSpecs is the single source of truth for the three partitioned
// telemetry tables. The columns/indexes/autovacuum values MUST match db/schema.sql.
// Retention windows mirror control/workers.go (workerMemorySampleRetention etc.).
func telemetryPartitionSpecs() []telemetryPartitionSpec {
	return []telemetryPartitionSpec{
		{
			table: "worker_memory_samples",
			columns: `id           UUID NOT NULL DEFAULT gen_random_uuid(),
			 worker_id    UUID,
			 available_gb REAL,
			 effective_gb REAL,
			 throttled    BOOLEAN,
			 created_at   TIMESTAMPTZ NOT NULL DEFAULT now()`,
			secondaryIndexName: "worker_memory_samples_worker_time_idx",
			secondaryIndexBody: "(worker_id, created_at DESC)",
			autovacuum: `autovacuum_vacuum_scale_factor  = 0.02,
			 autovacuum_vacuum_threshold     = 200,
			 autovacuum_analyze_scale_factor = 0.02,
			 autovacuum_analyze_threshold    = 200,
			 autovacuum_vacuum_cost_limit    = 1000`,
			retention: workerMemorySampleRetention,
		},
		{
			table: "task_durations",
			columns: `id          UUID NOT NULL DEFAULT gen_random_uuid(),
			 created_at  TIMESTAMPTZ NOT NULL DEFAULT now(),
			 job_id      UUID,
			 job_type    TEXT,
			 model_ref   TEXT,
			 split_size  INT,
			 duration_ms BIGINT,
			 worker_id   UUID,
			 engine      TEXT,
			 build_hash  TEXT,
			 task_id     UUID`,
			secondaryIndexName: "task_durations_type_model_idx",
			secondaryIndexBody: "(job_type, model_ref)",
			autovacuum: `autovacuum_vacuum_scale_factor  = 0.05,
			 autovacuum_vacuum_threshold     = 100,
			 autovacuum_analyze_scale_factor = 0.05,
			 autovacuum_analyze_threshold    = 100,
			 autovacuum_vacuum_cost_limit    = 500`,
			retention: taskDurationRetention,
		},
		{
			table: "job_events",
			columns: `id          UUID NOT NULL DEFAULT gen_random_uuid(),
			 created_at  TIMESTAMPTZ NOT NULL DEFAULT now(),
			 job_id      UUID NOT NULL,
			 task_id     UUID,
			 event       TEXT NOT NULL,
			 buyer_text  TEXT,
			 detail      JSONB`,
			secondaryIndexName: "job_events_job_idx",
			secondaryIndexBody: "(job_id, created_at)",
			autovacuum: `autovacuum_vacuum_scale_factor  = 0.1,
			 autovacuum_vacuum_threshold     = 100,
			 autovacuum_analyze_scale_factor = 0.1,
			 autovacuum_analyze_threshold    = 100,
			 autovacuum_vacuum_cost_limit    = 500`,
			retention: jobEventRetention,
		},
	}
}

// partitionCreateAheadMonths is how many whole months of empty partitions the rotation
// job keeps created in ADVANCE of the current month, so a real insert always finds a
// concrete month partition waiting and never falls back to DEFAULT. Two is generous
// headroom against a rotation tick that is briefly delayed.
const partitionCreateAheadMonths = 2

// monthStartUTC truncates t to the first instant of its month in UTC. Partition bounds
// are pinned to UTC (not the session timezone) so a month boundary is unambiguous
// across DST transitions — the bound is a fixed timestamptz either way, and UTC keeps
// the naming (_pYYYY_MM) and the FROM/TO literals consistent regardless of where the
// server runs.
func monthStartUTC(t time.Time) time.Time {
	t = t.UTC()
	return time.Date(t.Year(), t.Month(), 1, 0, 0, 0, 0, time.UTC)
}

// addMonth returns the first instant of the month after m (m must be a month start).
func addMonth(m time.Time) time.Time { return m.AddDate(0, 1, 0) }

// partitionName is the deterministic leaf name for a table's month partition:
// "<table>_pYYYY_MM". Deterministic naming lets the rotation job compute bounds from
// the name (never parsing pg_get_expr text) and makes "CREATE ... IF NOT EXISTS"
// naturally idempotent.
func partitionName(table string, monthStart time.Time) string {
	m := monthStart.UTC()
	return fmt.Sprintf("%s_p%04d_%02d", table, m.Year(), int(m.Month()))
}

// pgxExecer is the minimal Exec surface shared by *pgxpool.Pool and pgx.Tx, so the
// partition helpers work both inside the migration transaction and against the pool
// without a wrapper type.
type pgxExecer interface {
	Exec(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error)
}

// createMonthPartition creates one month partition (idempotent) with leaf-level
// autovacuum params. Bounds are the UTC month [monthStart, monthStart+1month).
func createMonthPartition(ctx context.Context, q pgxExecer, spec telemetryPartitionSpec, monthStart time.Time) error {
	name := partitionName(spec.table, monthStart)
	lo := monthStart.UTC().Format("2006-01-02 15:04:05-07")
	hi := addMonth(monthStart).UTC().Format("2006-01-02 15:04:05-07")
	stmt := fmt.Sprintf(
		`CREATE TABLE IF NOT EXISTS %s PARTITION OF %s FOR VALUES FROM ('%s') TO ('%s') WITH (%s)`,
		name, spec.table, lo, hi, spec.autovacuum)
	if _, err := q.Exec(ctx, stmt); err != nil {
		return fmt.Errorf("create month partition %s: %w", name, err)
	}
	return nil
}

// pgxQuerier is the minimal QueryRow surface shared by *pgxpool.Pool and pgx.Tx.
type pgxQuerier interface {
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
}

// pgxPartitionDB is the transaction + query surface shared by a pool and one
// acquired pool connection. Store.Migrate uses the acquired-connection form so
// it can hold one session advisory lock across every per-table transaction;
// ordinary rotation and direct migration tests continue to use the pool.
type pgxPartitionDB interface {
	pgxQuerier
	Begin(ctx context.Context) (pgx.Tx, error)
}

// isPartitioned reports whether `table` is already a partitioned parent (relkind 'p'),
// so the migration is a no-op on a second run (idempotency).
func isPartitioned(ctx context.Context, q pgxQuerier, table string) (bool, error) {
	var kind string
	err := q.QueryRow(ctx,
		`SELECT relkind FROM pg_class WHERE relname = $1 AND relnamespace = 'public'::regnamespace`,
		table).Scan(&kind)
	if err != nil {
		if err == pgx.ErrNoRows {
			return false, nil // table absent (a control plane that only ran Migrate before the base tables existed) — caller creates it fresh
		}
		return false, err
	}
	return kind == "p", nil
}

// MigrateTelemetryPartitions converts the three telemetry tables from plain tables to
// declarative-RANGE-partitioned tables, in place, preserving every existing row. Safe
// to run on an existing populated database and idempotent (a second run is a no-op once
// the tables are already partitioned). Called from Store.Migrate AFTER the base tables +
// their autovacuum tuning exist, so it always finds a real table to convert (or, on a
// brand-new install, creates the partitioned shape directly).
//
// The whole conversion of each table runs in its own transaction: either the table is
// fully partitioned with all rows copied, or it is left exactly as it was.
func (s *Store) MigrateTelemetryPartitions(ctx context.Context) error {
	return migrateTelemetryPartitions(ctx, s.pool)
}

func migrateTelemetryPartitions(ctx context.Context, db pgxPartitionDB) error {
	for _, spec := range telemetryPartitionSpecs() {
		if err := migrateOneTelemetryPartition(ctx, db, spec); err != nil {
			return fmt.Errorf("partition-migrate %s: %w", spec.table, err)
		}
	}
	return nil
}

func migrateOneTelemetryPartition(ctx context.Context, db pgxPartitionDB, spec telemetryPartitionSpec) error {
	already, err := isPartitioned(ctx, db, spec.table)
	if err != nil {
		return err
	}
	if already {
		// Already converted on a prior startup — nothing to do. The rotation job keeps
		// partitions current; this migration never runs twice on the same table.
		return nil
	}

	tx, err := db.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx) //nolint:errcheck // rollback-on-early-return; the happy path Commits

	// Does the plain table exist yet? On a fresh install where the base schema was
	// applied, it does (plain). On a control plane that only ever ran Migrate before
	// the base tables were introduced, it may not — then we create the partitioned
	// shape directly with no rows to copy.
	var exists bool
	if err := tx.QueryRow(ctx,
		`SELECT EXISTS (SELECT 1 FROM pg_class WHERE relname = $1 AND relnamespace = 'public'::regnamespace)`,
		spec.table).Scan(&exists); err != nil {
		return err
	}

	// Row-count check bookends: capture the pre-count (0 if the table is absent) so we
	// can assert exact row preservation before committing.
	var preCount int64
	if exists {
		if err := tx.QueryRow(ctx, `SELECT count(*) FROM `+spec.table).Scan(&preCount); err != nil {
			return err
		}
		// Rename the plain table + its indexes out of the way so the new partitioned
		// parent can claim the canonical names. (Renaming a table does NOT rename its
		// indexes — they must be moved explicitly or the CREATE INDEX below collides.)
		if _, err := tx.Exec(ctx, fmt.Sprintf(`ALTER TABLE %s RENAME TO %s_old`, spec.table, spec.table)); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, fmt.Sprintf(`ALTER INDEX %s RENAME TO %s_old`, spec.secondaryIndexName, spec.secondaryIndexName)); err != nil {
			return err
		}
		// The single-column PK index is named <table>_pkey by convention; move it aside
		// too. IF EXISTS not available for ALTER INDEX RENAME, but every base table here
		// carries this PK (db/schema.sql), so it is always present when exists is true.
		if _, err := tx.Exec(ctx, fmt.Sprintf(`ALTER INDEX %s_pkey RENAME TO %s_pkey_old`, spec.table, spec.table)); err != nil {
			return err
		}
	}

	// Create the partitioned parent with the composite (created_at, id) PK.
	createParent := fmt.Sprintf(
		`CREATE TABLE %s (%s, PRIMARY KEY (created_at, id)) PARTITION BY RANGE (created_at)`,
		spec.table, spec.columns)
	if _, err := tx.Exec(ctx, createParent); err != nil {
		return err
	}
	// Recreate the secondary index on the parent (propagates to every leaf).
	if _, err := tx.Exec(ctx, fmt.Sprintf(`CREATE INDEX %s ON %s %s`,
		spec.secondaryIndexName, spec.table, spec.secondaryIndexBody)); err != nil {
		return err
	}
	// DEFAULT partition: a safety net for an edge row outside the pre-created month
	// window. Real inserts never land here (rotation keeps months created ahead), but
	// its existence guarantees no INSERT is ever rejected for lack of a partition.
	if _, err := tx.Exec(ctx, fmt.Sprintf(
		`CREATE TABLE %s_default PARTITION OF %s DEFAULT WITH (%s)`,
		spec.table, spec.table, spec.autovacuum)); err != nil {
		return err
	}

	// Defensive: the new parent forces created_at NOT NULL, so any legacy row with a
	// NULL created_at (impossible via the production insert path — every INSERT omits the
	// column and takes DEFAULT now() — but a hand-inserted row could in theory) would be
	// rejected by the row copy and abort the whole migration. Backfill such rows to now()
	// on the RENAMED old table first, so the migration is self-healing rather than stuck.
	// This touches only genuinely-NULL rows (a no-op on every real DB) and cannot change
	// a real timestamp.
	if exists && preCount > 0 {
		if _, err := tx.Exec(ctx, fmt.Sprintf(`UPDATE %s_old SET created_at = now() WHERE created_at IS NULL`, spec.table)); err != nil {
			return err
		}
	}

	// Determine the historical span so we can pre-create a month partition for every
	// month that has existing rows — this routes all historical rows into proper month
	// partitions (droppable) instead of piling into DEFAULT (which cannot later be
	// carved up, per Postgres' overlapping-DEFAULT rule).
	loMonth := monthStartUTC(time.Now())
	if exists && preCount > 0 {
		var minCreated time.Time
		if err := tx.QueryRow(ctx, fmt.Sprintf(`SELECT min(created_at) FROM %s_old`, spec.table)).Scan(&minCreated); err != nil {
			return err
		}
		loMonth = monthStartUTC(minCreated)
	}
	// Upper edge: create ahead of now() so post-migration inserts have a home too.
	hiMonth := monthStartUTC(time.Now().AddDate(0, partitionCreateAheadMonths, 0))
	for m := loMonth; !m.After(hiMonth); m = addMonth(m) {
		if err := createMonthPartition(ctx, tx, spec, m); err != nil {
			return err
		}
	}

	if exists {
		// Copy every row (id + created_at + all columns preserved) then drop the old
		// table. INSERT ... SELECT * relies on identical column ORDER, which the explicit
		// column body above guarantees matches db/schema.sql.
		if _, err := tx.Exec(ctx, fmt.Sprintf(`INSERT INTO %s SELECT * FROM %s_old`, spec.table, spec.table)); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, fmt.Sprintf(`DROP TABLE %s_old`, spec.table)); err != nil {
			return err
		}
		// Assert exact row preservation BEFORE committing — a mismatch aborts (rolls
		// back) rather than committing a lossy migration.
		var postCount int64
		if err := tx.QueryRow(ctx, `SELECT count(*) FROM `+spec.table).Scan(&postCount); err != nil {
			return err
		}
		if postCount != preCount {
			return fmt.Errorf("row-count mismatch converting %s: pre=%d post=%d (aborting, no rows lost)", spec.table, preCount, postCount)
		}
	}

	return tx.Commit(ctx)
}

// RotateTelemetryPartitions is the O(1)-per-partition lifecycle job: for each
// partitioned telemetry table it CREATEs the upcoming month partitions (create-ahead,
// so inserts always find a home) and DROPs any month partition whose entire range is
// older than the table's retention window (drop-expired — the O(1) metadata replacement
// for the O(rows) DELETE). Idempotent: creating an existing partition is a no-op
// (IF NOT EXISTS), and a table with nothing to drop simply drops nothing.
//
// Returns the number of partitions created and dropped across all tables, for the
// caller's log/liveness.
func (s *Store) RotateTelemetryPartitions(ctx context.Context, now time.Time) (created, dropped int, err error) {
	for _, spec := range telemetryPartitionSpecs() {
		part, err := isPartitioned(ctx, s.pool, spec.table)
		if err != nil {
			return created, dropped, err
		}
		if !part {
			// The table has not been converted (e.g. a DB that skipped the partition
			// migration). Rotation is a no-op for a plain table — the DELETE sweep still
			// bounds it — so skip rather than error.
			continue
		}
		c, d, err := s.rotateOneTelemetryPartition(ctx, spec, now)
		if err != nil {
			return created, dropped, fmt.Errorf("rotate %s: %w", spec.table, err)
		}
		created += c
		dropped += d
	}
	return created, dropped, nil
}

func (s *Store) rotateOneTelemetryPartition(ctx context.Context, spec telemetryPartitionSpec, now time.Time) (created, dropped int, err error) {
	// Create-ahead: ensure the current month through +partitionCreateAheadMonths exist.
	cur := monthStartUTC(now)
	ahead := monthStartUTC(now.AddDate(0, partitionCreateAheadMonths, 0))
	for m := cur; !m.After(ahead); m = addMonth(m) {
		name := partitionName(spec.table, m)
		before := s.partitionExists(ctx, name)
		if err := createMonthPartition(ctx, s.pool, spec, m); err != nil {
			return created, dropped, err
		}
		if !before && s.partitionExists(ctx, name) {
			created++
		}
	}

	// Drop-expired: a partition is expired when its ENTIRE range is older than the
	// cutoff, i.e. its upper bound (monthStart+1month) <= now-retention. Dropping only
	// whole-months-past-retention leaves the boundary month intact (the row-level DELETE
	// sweep trims that month's sub-cutoff tail), so no not-yet-expired row is ever
	// removed by a partition drop.
	cutoff := now.Add(-spec.retention)
	names, err := s.listMonthPartitions(ctx, spec.table)
	if err != nil {
		return created, dropped, err
	}
	for _, mn := range names {
		// upper bound is the month AFTER the partition's month
		upper := addMonth(mn.month)
		if !upper.After(cutoff) { // upper <= cutoff → the whole partition is expired
			if _, err := s.pool.Exec(ctx, fmt.Sprintf(`DROP TABLE IF EXISTS %s`, mn.name)); err != nil {
				return created, dropped, fmt.Errorf("drop expired partition %s: %w", mn.name, err)
			}
			dropped++
		}
	}
	return created, dropped, nil
}

// monthPartition is one month partition's leaf name and the UTC month-start it covers.
type monthPartition struct {
	name  string
	month time.Time
}

// listMonthPartitions returns every month partition (the "_pYYYY_MM" leaves, never the
// DEFAULT partition) of a partitioned telemetry table, each with the UTC month it
// covers parsed from its deterministic name. Parsing the NAME (not pg_get_expr bound
// text) keeps rotation robust and timezone-independent.
func (s *Store) listMonthPartitions(ctx context.Context, table string) ([]monthPartition, error) {
	rows, err := s.pool.Query(ctx,
		`SELECT c.relname
		   FROM pg_inherits i JOIN pg_class c ON c.oid = i.inhrelid
		  WHERE i.inhparent = $1::regclass
		    AND c.relname LIKE $2`,
		table, table+`_p%`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []monthPartition
	prefix := table + "_p"
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			return nil, err
		}
		// name is "<table>_pYYYY_MM"; parse the trailing YYYY_MM.
		suffix := name[len(prefix):] // "YYYY_MM"
		var y, mo int
		if _, err := fmt.Sscanf(suffix, "%04d_%02d", &y, &mo); err != nil {
			continue // not a month partition (defensive; the LIKE already scopes this)
		}
		out = append(out, monthPartition{name: name, month: time.Date(y, time.Month(mo), 1, 0, 0, 0, 0, time.UTC)})
	}
	return out, rows.Err()
}

// partitionExists reports whether a leaf partition with the given name exists (used to
// count real creates in the rotation stats).
func (s *Store) partitionExists(ctx context.Context, name string) bool {
	var n int
	if err := s.pool.QueryRow(ctx,
		`SELECT count(*) FROM pg_class WHERE relname = $1 AND relnamespace = 'public'::regnamespace`,
		name).Scan(&n); err != nil {
		return false
	}
	return n > 0
}
