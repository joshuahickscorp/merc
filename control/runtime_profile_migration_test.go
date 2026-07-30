package main

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// freshMigratedDatabase creates a brand-new database, migrates it from
// schema.sql, and returns a pool onto it.
//
// A migration tested only against the shared development database proves the
// migration is idempotent, not that it works. Restore is exactly the case where
// the tables do not exist yet, and it is the case a schema change is most likely
// to break.
func freshMigratedDatabase(t *testing.T) (*Store, *pgxpool.Pool, context.Context) {
	t.Helper()
	adminURL := requireTestDatabase(t)
	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	t.Cleanup(cancel)

	admin, err := pgxpool.New(ctx, adminURL)
	if err != nil {
		t.Fatalf("connect admin: %v", err)
	}
	defer admin.Close()

	name := "merc_migr_" + strings.ReplaceAll(uuid.NewString()[:8], "-", "")
	if _, err := admin.Exec(ctx, `CREATE DATABASE `+name); err != nil {
		t.Fatalf("create database %s: %v", name, err)
	}
	t.Cleanup(func() {
		c, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		drop, err := pgxpool.New(c, adminURL)
		if err != nil {
			return
		}
		defer drop.Close()
		_, _ = drop.Exec(c, `DROP DATABASE IF EXISTS `+name+` WITH (FORCE)`)
	})

	freshURL := swapDatabaseName(t, adminURL, name)
	pool, err := pgxpool.New(ctx, freshURL)
	if err != nil {
		t.Fatalf("connect %s: %v", name, err)
	}
	t.Cleanup(pool.Close)
	store := NewStore(pool)
	if err := store.Migrate(ctx); err != nil {
		t.Fatalf("migrate fresh database: %v", err)
	}
	return store, pool, ctx
}

func swapDatabaseName(t *testing.T, dsn, name string) string {
	t.Helper()
	slash := strings.LastIndex(dsn, "/")
	if slash < 0 {
		t.Fatalf("cannot find the database name in %q", dsn)
	}
	rest := dsn[slash+1:]
	query := ""
	if q := strings.Index(rest, "?"); q >= 0 {
		query = rest[q:]
	}
	return dsn[:slash+1] + name + query
}

// A restored database must come up with the governed registry populated. If the
// sync only worked against a database that already had the tables, restore would
// produce a control plane that admits nothing.
func TestFreshDatabaseStartsWithAGovernedRegistry(t *testing.T) {
	store, pool, ctx := freshMigratedDatabase(t)

	var engines, profiles, cells, platforms int
	if err := pool.QueryRow(ctx, `
		SELECT (SELECT COUNT(*) FROM runtime_engines),
		       (SELECT COUNT(*) FROM runtime_profiles),
		       (SELECT COUNT(*) FROM runtime_profile_models),
		       (SELECT COUNT(*) FROM runtime_profile_hardware)`).
		Scan(&engines, &profiles, &cells, &platforms); err != nil {
		t.Fatalf("count registry: %v", err)
	}
	if engines != len(runtimeAuthority.Engines) {
		t.Errorf("engines = %d, want %d", engines, len(runtimeAuthority.Engines))
	}
	if profiles != len(runtimeAuthority.Runtimes) {
		t.Errorf("profiles = %d, want %d", profiles, len(runtimeAuthority.Runtimes))
	}
	if cells == 0 || platforms == 0 {
		t.Fatalf("registry has %d cells and %d platforms; the child rows did not sync",
			cells, platforms)
	}

	// Exactly the routable profiles are marked routable, and the derived column
	// cannot disagree with the lifecycle it was derived from.
	rows, err := pool.Query(ctx,
		`SELECT runtime_profile_id, lifecycle, routable, profile_digest, revision
		   FROM runtime_profiles ORDER BY runtime_profile_id`)
	if err != nil {
		t.Fatalf("read profiles: %v", err)
	}
	defer rows.Close()
	seen := 0
	for rows.Next() {
		var id, lifecycle, digest, revision string
		var routable bool
		if err := rows.Scan(&id, &lifecycle, &routable, &digest, &revision); err != nil {
			t.Fatalf("scan profile: %v", err)
		}
		seen++
		profile, ok := runtimeProfileByID(id)
		if !ok {
			t.Errorf("database has profile %q that the authority does not", id)
			continue
		}
		if lifecycle != profile.Lifecycle || routable != runtimeLifecycleRoutable(profile.Lifecycle) {
			t.Errorf("%s: lifecycle/routable = %s/%v, want %s/%v",
				id, lifecycle, routable, profile.Lifecycle,
				runtimeLifecycleRoutable(profile.Lifecycle))
		}
		want, err := profile.ContentDigest()
		if err != nil {
			t.Fatal(err)
		}
		if digest != want || revision != profile.Revision {
			t.Errorf("%s: stored %s/%s, want %s/%s", id, revision, digest,
				profile.Revision, want)
		}
	}
	if seen != len(runtimeAuthority.Runtimes) {
		t.Fatalf("read %d profiles, want %d", seen, len(runtimeAuthority.Runtimes))
	}

	// A database with no workers is trivially reconciled, which is the correct
	// answer and the one that makes the NOT NULL step safe on a fresh install.
	rec, err := store.ReconcileWorkerRuntimeProfiles(ctx)
	if err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	if !rec.Ready {
		t.Fatalf("fresh database is not reconciled: %+v", rec)
	}

	// Migrating twice must be a no-op, not a drift refusal.
	if err := store.Migrate(ctx); err != nil {
		t.Fatalf("second migrate: %v", err)
	}
}

// The backfill is the whole reason the migration can be additive. A worker row
// written before the registry existed must survive and acquire its governed
// identity, not be deleted or refused.
func TestLegacyCandleWorkerSurvivesTheBackfill(t *testing.T) {
	store, pool, ctx := freshMigratedDatabase(t)

	supplierID, workerID := uuid.New(), uuid.New()
	if _, err := pool.Exec(ctx,
		`INSERT INTO suppliers (id,email,reputation,status) VALUES ($1,$2,0.5,'active')`,
		supplierID, "legacy+"+supplierID.String()+"@example.test"); err != nil {
		t.Fatalf("insert supplier: %v", err)
	}
	// A legacy row: engine set, no governed profile, exactly as it would look
	// after restoring a backup taken before this migration.
	if _, err := pool.Exec(ctx,
		`INSERT INTO workers (id,supplier_id,hw_class,engine,memory_gb)
		 VALUES ($1,$2,'apple_silicon_ultra','candle',96)`,
		workerID, supplierID); err != nil {
		t.Fatalf("insert legacy worker: %v", err)
	}

	rec, err := store.ReconcileWorkerRuntimeProfiles(ctx)
	if err != nil {
		t.Fatalf("reconcile before backfill: %v", err)
	}
	if rec.Unreconciled != 1 || rec.Ready {
		t.Fatalf("a legacy worker should be unreconciled before the backfill: %+v", rec)
	}

	// Re-running the migration runs the backfill.
	if err := store.Migrate(ctx); err != nil {
		t.Fatalf("migrate for backfill: %v", err)
	}

	var profileID *string
	if err := pool.QueryRow(ctx,
		`SELECT runtime_profile_id FROM workers WHERE id=$1`, workerID).Scan(&profileID); err != nil {
		t.Fatalf("read worker: %v", err)
	}
	if profileID == nil || *profileID != "candle_metal" {
		t.Fatalf("legacy worker profile = %v, want candle_metal", profileID)
	}

	rec, err = store.ReconcileWorkerRuntimeProfiles(ctx)
	if err != nil {
		t.Fatalf("reconcile after backfill: %v", err)
	}
	if !rec.Ready || rec.Unreconciled != 0 || rec.EngineMismatched != 0 {
		t.Fatalf("backfilled database is not ready for NOT NULL: %+v", rec)
	}
	if rec.Backfilled != 1 || rec.TotalWorkers != 1 {
		t.Fatalf("reconciliation counts = %+v, want 1 worker backfilled", rec)
	}
}

// The database must refuse governed identities the registry does not contain,
// independently of whatever the Go layer believes. A control plane bug that
// wrote a bad profile id would otherwise persist it.
func TestDatabaseRefusesUngovernedWorkerProfiles(t *testing.T) {
	_, pool, ctx := freshMigratedDatabase(t)
	supplierID := uuid.New()
	if _, err := pool.Exec(ctx,
		`INSERT INTO suppliers (id,email,reputation,status) VALUES ($1,$2,0.5,'active')`,
		supplierID, "gov+"+supplierID.String()+"@example.test"); err != nil {
		t.Fatalf("insert supplier: %v", err)
	}

	for _, tc := range []struct {
		name    string
		profile string
		engine  string
		want    string
	}{
		{"an unregistered profile", "totally_made_up", "candle", ""},
		{"a profile that disagrees with the engine", "candle_metal", "mlx", "engine"},
	} {
		_, err := pool.Exec(ctx,
			`INSERT INTO workers (id,supplier_id,hw_class,engine,runtime_profile_id)
			 VALUES ($1,$2,'apple_silicon_ultra',$3,$4)`,
			uuid.New(), supplierID, tc.engine, tc.profile)
		if err == nil {
			t.Errorf("%s was accepted by the database", tc.name)
			continue
		}
		if tc.want != "" && !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%s refused with %q, want a message mentioning %q",
				tc.name, err.Error(), tc.want)
		}
	}

	// A retired profile must not take new work. Retire mlx_metal in the database
	// only — the embedded document is untouched — and confirm the trigger, not
	// the Go layer, is what refuses.
	if _, err := pool.Exec(ctx,
		`UPDATE runtime_profiles SET lifecycle='RETIRED', routable=false
		  WHERE runtime_profile_id='mlx_metal'`); err != nil {
		t.Fatalf("retire profile: %v", err)
	}
	_, err := pool.Exec(ctx,
		`INSERT INTO workers (id,supplier_id,hw_class,engine,runtime_profile_id)
		 VALUES ($1,$2,'apple_silicon_ultra','mlx','mlx_metal')`,
		uuid.New(), supplierID)
	if err == nil {
		t.Fatal("a retired runtime profile accepted a new worker")
	}
	if !strings.Contains(err.Error(), "retired") {
		t.Errorf("retired-profile refusal said %q", err.Error())
	}
}

// Content immutability, enforced where it counts: at sync time against a
// database that already holds the old meaning. Editing a profile in place would
// silently change what every past receipt naming it meant.
func TestSyncRefusesProfileContentDriftUnderTheSameRevision(t *testing.T) {
	_, pool, ctx := freshMigratedDatabase(t)

	// Simulate a prior sync of candle_metal r1 whose content was different.
	if _, err := pool.Exec(ctx,
		`UPDATE runtime_profiles SET profile_digest = repeat('b',64)
		  WHERE runtime_profile_id='candle_metal'`); err != nil {
		t.Fatalf("plant drift: %v", err)
	}

	conn, err := pool.Acquire(ctx)
	if err != nil {
		t.Fatalf("acquire: %v", err)
	}
	defer conn.Release()

	err = syncRuntimeProfiles(ctx, conn)
	if !errors.Is(err, ErrRuntimeProfileContentDrift) {
		t.Fatalf("sync error = %v, want ErrRuntimeProfileContentDrift", err)
	}
	if !strings.Contains(err.Error(), "bump the revision") {
		t.Errorf("drift refusal does not say what to do: %v", err)
	}

	// Bumping the revision is the sanctioned escape. With history retained you do
	// not MUTATE a revision — it is part of the primary key and child rows
	// reference it — you register a new one beside it. Simulate that by removing
	// the planted row entirely and re-syncing.
	if _, err := pool.Exec(ctx,
		`DELETE FROM runtime_profile_models WHERE runtime_profile_id='candle_metal'`); err != nil {
		t.Fatalf("clear child rows: %v", err)
	}
	if _, err := pool.Exec(ctx,
		`UPDATE runtime_profiles SET profile_digest = $1
		  WHERE runtime_profile_id='candle_metal'`, currentCandleDigest(t)); err != nil {
		t.Fatalf("restore digest: %v", err)
	}
	if err := syncRuntimeProfiles(ctx, conn); err != nil {
		t.Fatalf("sync after restoring the true digest: %v", err)
	}
}

func currentCandleDigest(t *testing.T) string {
	t.Helper()
	p, ok := runtimeProfileByID("candle_metal")
	if !ok {
		t.Fatal("candle_metal is not registered")
	}
	d, err := p.ContentDigest()
	if err != nil {
		t.Fatal(err)
	}
	return d
}

// History is evidence: a revision that a receipt named must stay resolvable, and
// deleting one is refused outright.
func TestHistoricalRevisionsAreRetainedAndUndeletable(t *testing.T) {
	_, pool, ctx := freshMigratedDatabase(t)

	// Exactly one current revision per profile.
	var currents int
	if err := pool.QueryRow(ctx,
		`SELECT COUNT(*) FROM runtime_profiles WHERE is_current AND runtime_profile_id='candle_metal'`).
		Scan(&currents); err != nil {
		t.Fatal(err)
	}
	if currents != 1 {
		t.Fatalf("candle_metal has %d current revisions, want 1", currents)
	}

	// Plant an older revision the way a real upgrade would leave one behind.
	if _, err := pool.Exec(ctx, `
		INSERT INTO runtime_profiles
		  (runtime_profile_id, revision, profile_digest, engine, adapter, lifecycle,
		   routable, quality_tier, benchmark_authority, source_identity, is_current)
		VALUES ('candle_metal','r1',repeat('c',64),'candle','merc-candle','RETIRED',
		        false,'OUTCOME_EQUIVALENT','x','y',false)`); err != nil {
		t.Fatalf("plant historical revision: %v", err)
	}

	// Re-syncing must not remove it.
	conn, err := pool.Acquire(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Release()
	if err := syncRuntimeProfiles(ctx, conn); err != nil {
		t.Fatalf("sync with a historical revision present: %v", err)
	}
	var kept int
	if err := pool.QueryRow(ctx,
		`SELECT COUNT(*) FROM runtime_profiles
		  WHERE runtime_profile_id='candle_metal' AND revision='r1'`).Scan(&kept); err != nil {
		t.Fatal(err)
	}
	if kept != 1 {
		t.Fatal("a re-sync destroyed a historical revision")
	}
	// Still exactly one current, and it is not the historical one.
	var current string
	if err := pool.QueryRow(ctx,
		`SELECT revision FROM runtime_profiles
		  WHERE runtime_profile_id='candle_metal' AND is_current`).Scan(&current); err != nil {
		t.Fatal(err)
	}
	if current == "r1" {
		t.Fatal("the retired historical revision is current")
	}

	// And it cannot be deleted, because a receipt may still name it.
	if _, err := pool.Exec(ctx,
		`DELETE FROM runtime_profiles WHERE runtime_profile_id='candle_metal' AND revision='r1'`); err == nil {
		t.Fatal("a historical runtime profile revision was deleted")
	} else if !strings.Contains(err.Error(), "receipt evidence") {
		t.Errorf("refusal said %q", err.Error())
	}
}

// Lifecycle promotion is the one thing that must NOT look like replacing a
// profile. If it did, moving mlx_metal toward routability would require pretending
// its content changed.
func TestSyncAllowsLifecycleMovementWithoutARevisionBump(t *testing.T) {
	_, pool, ctx := freshMigratedDatabase(t)
	if _, err := pool.Exec(ctx,
		`UPDATE runtime_profiles SET lifecycle='REAL_RUNTIME_PROVEN'
		  WHERE runtime_profile_id='mlx_metal'`); err != nil {
		t.Fatalf("promote: %v", err)
	}
	conn, err := pool.Acquire(ctx)
	if err != nil {
		t.Fatalf("acquire: %v", err)
	}
	defer conn.Release()
	if err := syncRuntimeProfiles(ctx, conn); err != nil {
		t.Fatalf("sync after a lifecycle move: %v", err)
	}
	// The sync restores the document's lifecycle, because the document is the
	// source of truth for state as well as content.
	var lifecycle string
	if err := pool.QueryRow(ctx,
		`SELECT lifecycle FROM runtime_profiles WHERE runtime_profile_id='mlx_metal'`).
		Scan(&lifecycle); err != nil {
		t.Fatalf("read lifecycle: %v", err)
	}
	if lifecycle != runtimeLifecycleValidated {
		t.Fatalf("lifecycle = %s, want the document's %s", lifecycle, runtimeLifecycleValidated)
	}
}

// The derived routable column and the denormalized copy on the cell rows must
// never disagree with the lifecycle they came from. Two sources of truth about
// routability is the failure this whole registry exists to prevent.
func TestRoutabilityCannotBeAssertedIndependently(t *testing.T) {
	_, pool, ctx := freshMigratedDatabase(t)

	if _, err := pool.Exec(ctx,
		`UPDATE runtime_profiles SET routable=true WHERE runtime_profile_id='mlx_metal'`); err == nil {
		t.Fatal("a VALIDATED profile was marked routable")
	}
	if _, err := pool.Exec(ctx,
		`UPDATE runtime_profiles SET routable=false WHERE runtime_profile_id='candle_metal'`); err == nil {
		t.Fatal("an ACTIVE profile was marked non-routable")
	}

	var mismatched int
	if err := pool.QueryRow(ctx, `
		SELECT COUNT(*) FROM runtime_profile_models m
		  JOIN runtime_profiles p USING (runtime_profile_id)
		 WHERE m.routable <> p.routable`).Scan(&mismatched); err != nil {
		t.Fatalf("cross-check routable: %v", err)
	}
	if mismatched != 0 {
		t.Fatalf("%d cell rows disagree with their profile's routability", mismatched)
	}

	// Only one routable profile may own a cell id. Promoting a challenger onto an
	// occupied cell must collide at the database, not just in the document.
	if _, err := pool.Exec(ctx,
		`UPDATE runtime_profile_models SET routable=true
		  WHERE runtime_profile_id='mlx_metal'`); err != nil {
		t.Fatalf("mark challenger cells routable: %v", err)
	}
	_, err := pool.Exec(ctx,
		`UPDATE runtime_profile_models SET cell_id='candle-metal-llama1-infer'
		  WHERE runtime_profile_id='mlx_metal'`)
	if err == nil {
		t.Fatal("two routable profiles claimed the same cell id")
	}
}

// A worker must satisfy the profile it claims, not merely name it. Every one of
// these was previously either unchecked or checked only against the flat
// projection, which knows nothing about device counts.
func TestWorkerProfileValidationRefusesEveryUnsatisfiedClaim(t *testing.T) {
	candle, ok := runtimeProfileByID("candle_metal")
	if !ok {
		t.Fatal("candle_metal is not registered")
	}
	good := WorkerCapability{
		Engine:          "candle",
		HWClass:         "apple_silicon_ultra",
		MemoryGB:        96,
		SupportedJobs:   []string{"embed", "batch_infer"},
		SupportedModels: []string{"all-minilm-l6-v2", "llama-3.2-1b-instruct-q4"},
	}
	if err := ValidateWorkerAgainstProfile(candle, good); err != nil {
		t.Fatalf("a valid worker was refused: %v", err)
	}

	for _, tc := range []struct {
		name   string
		want   string
		mutate func(*WorkerCapability)
	}{
		{"a wrong engine", "engine", func(c *WorkerCapability) { c.Engine = "mlx" }},
		{"an unsupported platform", "hardware class",
			func(c *WorkerCapability) { c.HWClass = "nvidia_80gb" }},
		{"memory below the cell floor", "below the",
			func(c *WorkerCapability) { c.MemoryGB = 1 }},
		{"more devices than the profile admits", "device",
			func(c *WorkerCapability) { c.GPUCount = 8 }},
		{"no job/model pair the profile serves", "no job/model pair",
			func(c *WorkerCapability) { c.SupportedModels = []string{"some-other-model"} }},
	} {
		cap := good
		tc.mutate(&cap)
		err := ValidateWorkerAgainstProfile(candle, cap)
		if err == nil {
			t.Errorf("%s was accepted", tc.name)
			continue
		}
		if !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%s refused with %q, want a message containing %q",
				tc.name, err.Error(), tc.want)
		}
	}

	// A worker serving only embeddings must not be refused for failing to fit a
	// generation model it never advertised.
	embedOnly := good
	embedOnly.SupportedJobs = []string{"embed"}
	embedOnly.SupportedModels = []string{"all-minilm-l6-v2"}
	embedOnly.MemoryGB = 3 // above the 2 GB embed floor, below the 4 GB infer floor
	if err := ValidateWorkerAgainstProfile(candle, embedOnly); err != nil {
		t.Errorf("an embed-only worker was refused for a cell it never claimed: %v", err)
	}

	// A non-routable profile may not take work however well the worker fits it.
	mlx, ok := runtimeProfileByID("mlx_metal")
	if !ok {
		t.Fatal("mlx_metal is not registered")
	}
	mlxWorker := good
	mlxWorker.Engine = "mlx"
	if err := ValidateWorkerAgainstProfile(mlx, mlxWorker); err == nil {
		t.Fatal("a VALIDATED profile accepted a worker")
	} else if !strings.Contains(err.Error(), "VALIDATED") {
		t.Errorf("non-routable refusal said %q", err.Error())
	}

	// And an engine with no routable profile cannot be resolved at all.
	if _, err := ResolveWorkerRuntimeProfile(mlxWorker); err == nil {
		t.Fatal("an engine with no routable profile resolved")
	}
}

// The device-count range was pure documentation before this: declared in the
// authority, checked for well-formedness, never compared to a worker.
func TestDeviceCountIsAnAdmissionConstraintNotADeclaration(t *testing.T) {
	candle, _ := runtimeProfileByID("candle_metal")
	if candle.Hardware.DeviceCount.Maximum != 1 {
		t.Fatalf("candle_metal admits up to %d devices; this test assumes 1",
			candle.Hardware.DeviceCount.Maximum)
	}
	base := WorkerCapability{
		Engine: "candle", HWClass: "apple_silicon_ultra", MemoryGB: 96,
		SupportedJobs:   []string{"embed"},
		SupportedModels: []string{"all-minilm-l6-v2"},
	}
	for _, tc := range []struct {
		gpus    int
		refused bool
	}{
		{0, false}, // an agent predating the field is a single-device host
		{1, false},
		{2, true},
		{8, true},
	} {
		cap := base
		cap.GPUCount = tc.gpus
		err := ValidateWorkerAgainstProfile(candle, cap)
		if tc.refused && err == nil {
			t.Errorf("gpu_count=%d was admitted by a single-device profile", tc.gpus)
		}
		if !tc.refused && err != nil {
			t.Errorf("gpu_count=%d was refused: %v", tc.gpus, err)
		}
	}
}

// Registration must go through the governed gate, not around it. This is the
// end-to-end version: a worker whose capability does not satisfy its profile
// must not reach the workers table at all.
func TestUpsertWorkerEnforcesTheGovernedProfile(t *testing.T) {
	store, pool, ctx := freshMigratedDatabase(t)
	supplierID := uuid.New()
	if _, err := pool.Exec(ctx,
		`INSERT INTO suppliers (id,email,reputation,status) VALUES ($1,$2,0.5,'active')`,
		supplierID, "upsert+"+supplierID.String()+"@example.test"); err != nil {
		t.Fatalf("insert supplier: %v", err)
	}

	good := WorkerCapability{
		WorkerID: uuid.New(), SupplierID: supplierID,
		Engine: "candle", HWClass: "apple_silicon_ultra", MemoryGB: 96,
		BuildHash: "abc", AgentVersion: "1", OSVersion: "macOS",
		SupportedJobs:   []string{"embed"},
		SupportedModels: []string{"all-minilm-l6-v2"},
	}
	if err := store.UpsertWorker(ctx, good); err != nil {
		t.Fatalf("a valid worker was refused: %v", err)
	}
	var stored string
	if err := pool.QueryRow(ctx,
		`SELECT runtime_profile_id FROM workers WHERE id=$1`, good.WorkerID).Scan(&stored); err != nil {
		t.Fatalf("read worker profile: %v", err)
	}
	if stored != "candle_metal" {
		t.Fatalf("stored profile = %q, want candle_metal", stored)
	}

	tooManyDevices := good
	tooManyDevices.WorkerID = uuid.New()
	tooManyDevices.GPUCount = 4
	if err := store.UpsertWorker(ctx, tooManyDevices); err == nil {
		t.Fatal("a worker exceeding the profile's device count was registered")
	}
	var count int
	if err := pool.QueryRow(ctx,
		`SELECT COUNT(*) FROM workers WHERE id=$1`, tooManyDevices.WorkerID).Scan(&count); err != nil {
		t.Fatalf("count: %v", err)
	}
	if count != 0 {
		t.Fatal("a refused worker still reached the workers table")
	}
}

// The migration's end state, corrected by evidence.
//
// The directive's step 6 was "make the profile reference NOT NULL". Applied
// literally it broke a freshly migrated database: enrollment creates a worker
// row BEFORE the agent registers any capability, so at that moment there is
// legitimately no profile to bind, and the test suite caught it immediately.
//
// The invariant that actually matters is narrower and stronger — a worker may
// not be DISPATCHABLE without a complete governed identity — so it is enforced
// where dispatch authority is granted rather than where a row is created.
func TestDispatchRequiresGovernedProfileIdentityNotRowCreation(t *testing.T) {
	_, pool, ctx := freshMigratedDatabase(t)

	// The placeholder path must still work: an enrolling worker has no profile.
	var nullable string
	if err := pool.QueryRow(ctx, `
		SELECT is_nullable FROM information_schema.columns
		 WHERE table_name='workers' AND column_name='runtime_profile_id'`).Scan(&nullable); err != nil {
		t.Fatalf("read column: %v", err)
	}
	if nullable != "YES" {
		t.Error("runtime_profile_id is NOT NULL; enrollment creates a worker row " +
			"before any profile exists and would fail")
	}

	supplierID, workerID := uuid.New(), uuid.New()
	if _, err := pool.Exec(ctx,
		`INSERT INTO suppliers (id,email,reputation,status) VALUES ($1,$2,0.5,'active')`,
		supplierID, "dispatch+"+supplierID.String()+"@example.test"); err != nil {
		t.Fatalf("insert supplier: %v", err)
	}
	if _, err := pool.Exec(ctx,
		`INSERT INTO workers (id,supplier_id,hw_class) VALUES ($1,$2,'apple_silicon_ultra')`,
		workerID, supplierID); err != nil {
		t.Fatalf("an enrollment placeholder was refused: %v", err)
	}

	// But it may not hold dispatch capability without governed identity.
	_, err := pool.Exec(ctx, `
		INSERT INTO worker_authorized_capabilities
		  (worker_id,cell_id,runtime_id,job_type,model_ref,model_kind,matrix_sha256)
		VALUES ($1,'candle-metal-minilm-embed','candle_metal','embed',
		        'all-minilm-l6-v2','hf',$2)`, workerID, generatedRuntimeMatrixSHA256)
	if err == nil {
		t.Fatal("a worker with no governed profile was granted dispatch capability")
	}
	if !strings.Contains(err.Error(), "governed runtime profile identity") {
		t.Errorf("refusal said %q", err.Error())
	}

	// With the full identity — id, revision AND digest — it is allowed.
	id, revision, digest, err := governedProfileIdentity("candle")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx,
		`UPDATE workers SET engine='candle', runtime_profile_id=$2,
		                    runtime_profile_revision=$3, runtime_profile_digest=$4
		  WHERE id=$1`, workerID, id, revision, digest); err != nil {
		t.Fatalf("bind governed identity: %v", err)
	}
	if _, err := pool.Exec(ctx, `
		INSERT INTO worker_authorized_capabilities
		  (worker_id,cell_id,runtime_id,job_type,model_ref,model_kind,matrix_sha256)
		VALUES ($1,'candle-metal-minilm-embed','candle_metal','embed',
		        'all-minilm-l6-v2','hf',$2)`, workerID, generatedRuntimeMatrixSHA256); err != nil {
		t.Fatalf("a fully bound worker was refused dispatch capability: %v", err)
	}

	// Partial identity is not identity: the CHECK refuses id without revision.
	if _, err := pool.Exec(ctx,
		`UPDATE workers SET runtime_profile_revision=NULL WHERE id=$1`, workerID); err == nil {
		t.Error("a worker kept a profile id with no revision")
	}

	// Step 8: on a reconciled database the legacy engine string check is gone,
	// superseded by the governed foreign key and the agreement trigger.
	var legacy int
	if err := pool.QueryRow(ctx,
		`SELECT COUNT(*) FROM pg_constraint WHERE conname='workers_engine_valid'`).
		Scan(&legacy); err != nil {
		t.Fatalf("read constraint: %v", err)
	}
	if legacy != 0 {
		t.Error("workers_engine_valid survived on a fully reconciled database")
	}
	// `engine` itself stays as a read-compatibility column until step 10.
	var engineCol int
	if err := pool.QueryRow(ctx, `
		SELECT COUNT(*) FROM information_schema.columns
		 WHERE table_name='workers' AND column_name='engine'`).Scan(&engineCol); err != nil {
		t.Fatalf("read engine column: %v", err)
	}
	if engineCol != 1 {
		t.Error("the engine compatibility column was removed before its readers migrated")
	}
}

// Guard against a silent partial sync: every routable cell in the document must
// have a database row, and vice versa.
func TestRegistryAndDocumentAgreeOnEveryRoutableCell(t *testing.T) {
	_, pool, ctx := freshMigratedDatabase(t)
	rows, err := pool.Query(ctx,
		`SELECT runtime_profile_id, cell_id FROM runtime_profile_models WHERE routable`)
	if err != nil {
		t.Fatalf("read cells: %v", err)
	}
	defer rows.Close()
	inDB := map[string]bool{}
	for rows.Next() {
		var profileID, cellID string
		if err := rows.Scan(&profileID, &cellID); err != nil {
			t.Fatalf("scan: %v", err)
		}
		inDB[fmt.Sprintf("%s/%s", profileID, cellID)] = true
	}
	inDoc := map[string]bool{}
	for _, profile := range runtimeAuthority.RoutableRuntimes() {
		for _, cell := range profile.Cells {
			inDoc[fmt.Sprintf("%s/%s", profile.RuntimeID, cell.ID)] = true
		}
	}
	for key := range inDoc {
		if !inDB[key] {
			t.Errorf("routable cell %s is in the document but not the database", key)
		}
	}
	for key := range inDB {
		if !inDoc[key] {
			t.Errorf("routable cell %s is in the database but not the document", key)
		}
	}
	if len(inDoc) != len(generatedAdvertisedRuntimeCapabilities) {
		t.Errorf("%d routable cells in the document, %d in the advertised projection",
			len(inDoc), len(generatedAdvertisedRuntimeCapabilities))
	}
}

var _ = pgx.ErrNoRows
