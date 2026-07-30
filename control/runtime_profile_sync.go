package main

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// Synchronizing the embedded runtime authority into PostgreSQL.
//
// The document remains the source of truth. What the database gains is the
// ability to REFUSE, at the moment a worker row is written, things the document
// forbids — instead of only at process start, where a running deployment never
// re-reads it.
//
// The load-bearing rule here is content immutability. A (runtime_profile_id,
// revision) pair that already exists with a different profile_digest is a
// refusal, not an update: every receipt, benchmark and calibration bucket that
// named that profile would otherwise silently start meaning something else.
// Lifecycle and supersession DO update, because a profile is expected to move
// VALIDATED to CANARY to ACTIVE without becoming a different profile.

// ErrRuntimeProfileContentDrift is returned when the embedded authority changed
// a profile's meaning without bumping its revision. It is deliberately fatal to
// the migration: continuing would leave the database describing one runtime and
// the process describing another.
var ErrRuntimeProfileContentDrift = errors.New("runtime profile content changed under an existing revision")

// syncRuntimeProfiles writes the embedded authority into the governed tables.
// It runs inside Migrate, under the schema migration lock, alongside
// syncRuntimeCatalog.
func syncRuntimeProfiles(ctx context.Context, conn *pgxpool.Conn) error {
	tx, err := conn.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)

	for _, engine := range runtimeAuthority.Engines {
		if _, err := tx.Exec(ctx, `
			INSERT INTO runtime_engines (engine, adapter, synced_at)
			VALUES ($1,$2,now())
			ON CONFLICT (engine) DO UPDATE
			   SET adapter = EXCLUDED.adapter, synced_at = now()`,
			engine.Engine, engine.Adapter); err != nil {
			return fmt.Errorf("sync runtime engine %q: %w", engine.Engine, err)
		}
	}

	for _, profile := range runtimeAuthority.Runtimes {
		digest, err := profile.ContentDigest()
		if err != nil {
			return err
		}
		if err := assertNoContentDrift(ctx, tx, profile, digest); err != nil {
			return err
		}
		routable := runtimeLifecycleRoutable(profile.Lifecycle)
		if _, err := tx.Exec(ctx, `
			INSERT INTO runtime_profiles
			  (runtime_profile_id, revision, profile_digest, engine, adapter, lifecycle,
			   routable, quality_tier, benchmark_authority, source_identity,
			   superseded_by, device_min, device_max, updated_at)
			VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,now())
			ON CONFLICT (runtime_profile_id) DO UPDATE SET
			  -- Content columns are re-asserted, not changed: assertNoContentDrift
			  -- has already proven they are identical for this revision.
			  revision = EXCLUDED.revision,
			  profile_digest = EXCLUDED.profile_digest,
			  engine = EXCLUDED.engine,
			  adapter = EXCLUDED.adapter,
			  quality_tier = EXCLUDED.quality_tier,
			  benchmark_authority = EXCLUDED.benchmark_authority,
			  device_min = EXCLUDED.device_min,
			  device_max = EXCLUDED.device_max,
			  -- These two are the ones that may legitimately move.
			  lifecycle = EXCLUDED.lifecycle,
			  routable = EXCLUDED.routable,
			  superseded_by = EXCLUDED.superseded_by,
			  source_identity = EXCLUDED.source_identity,
			  updated_at = now()`,
			profile.RuntimeID, profile.Revision, digest, profile.Engine, profile.Adapter,
			profile.Lifecycle, routable, profile.QualityTier, profile.BenchmarkAuthority,
			generatedRuntimeMatrixSHA256, profile.SupersededBy,
			profile.Hardware.DeviceCount.Minimum, profile.Hardware.DeviceCount.Maximum,
		); err != nil {
			return fmt.Errorf("sync runtime profile %q: %w", profile.RuntimeID, err)
		}

		// Child rows are replaced wholesale. They are pure projections of the
		// profile content that assertNoContentDrift just pinned, so a replace
		// cannot lose information the document does not still carry.
		for _, table := range []string{
			"runtime_profile_models", "runtime_profile_hardware", "runtime_profile_capabilities",
		} {
			if _, err := tx.Exec(ctx,
				`DELETE FROM `+table+` WHERE runtime_profile_id = $1`, profile.RuntimeID); err != nil {
				return fmt.Errorf("clear %s for %q: %w", table, profile.RuntimeID, err)
			}
		}
		for _, cell := range profile.Cells {
			if _, err := tx.Exec(ctx, `
				INSERT INTO runtime_profile_models
				  (runtime_profile_id, cell_id, job_type, model_id, runner,
				   min_memory_gb, verification, routable)
				VALUES ($1,$2,$3,$4,$5,$6,$7,$8)`,
				profile.RuntimeID, cell.ID, cell.Job, cell.Model, cell.Runner,
				cell.MinMemoryGB, cell.Verification, routable); err != nil {
				return fmt.Errorf("sync cell %q of %q: %w", cell.ID, profile.RuntimeID, err)
			}
		}
		for _, platform := range profile.Hardware.Platforms {
			if _, err := tx.Exec(ctx, `
				INSERT INTO runtime_profile_hardware (runtime_profile_id, platform)
				VALUES ($1,$2) ON CONFLICT DO NOTHING`,
				profile.RuntimeID, platform); err != nil {
				return fmt.Errorf("sync platform %q of %q: %w", platform, profile.RuntimeID, err)
			}
		}
		for _, capability := range profile.declaredCapabilities() {
			if _, err := tx.Exec(ctx, `
				INSERT INTO runtime_profile_capabilities (runtime_profile_id, capability)
				VALUES ($1,$2) ON CONFLICT DO NOTHING`,
				profile.RuntimeID, capability); err != nil {
				return fmt.Errorf("sync capability %q of %q: %w",
					capability, profile.RuntimeID, err)
			}
		}
	}

	// A2 step 3: backfill. Every legacy worker predates the registry and was, by
	// workers_engine_valid, a Candle worker. Naming that explicitly is what lets
	// the old string check eventually go.
	//
	// Only rows whose engine actually matches are backfilled. A row that somehow
	// carries a different engine is left NULL and surfaces in the reconciliation
	// report rather than being silently relabelled.
	if _, err := tx.Exec(ctx, `
		UPDATE workers w
		   SET runtime_profile_id = p.runtime_profile_id
		  FROM runtime_profiles p
		 WHERE w.runtime_profile_id IS NULL
		   AND p.engine = w.engine
		   AND p.routable`); err != nil {
		return fmt.Errorf("backfill worker runtime profiles: %w", err)
	}
	return tx.Commit(ctx)
}

func assertNoContentDrift(
	ctx context.Context, tx pgx.Tx, profile authorityRuntimeProfile, digest string,
) error {
	var existingRevision, existingDigest string
	err := tx.QueryRow(ctx,
		`SELECT revision, profile_digest FROM runtime_profiles WHERE runtime_profile_id = $1`,
		profile.RuntimeID).Scan(&existingRevision, &existingDigest)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil // first registration
	}
	if err != nil {
		return err
	}
	if existingRevision == profile.Revision && existingDigest != digest {
		return fmt.Errorf(
			"%w: %s is still revision %s but its content digest moved from %s to %s; "+
				"bump the revision instead of editing the profile in place",
			ErrRuntimeProfileContentDrift, profile.RuntimeID, profile.Revision,
			existingDigest, digest)
	}
	return nil
}

// WorkerRuntimeProfileReconciliation is the A2 step-5 proof. It is the evidence
// that must be clean before workers.runtime_profile_id becomes NOT NULL and
// workers_engine_valid is dropped. Until then, both are reported rather than
// assumed.
type WorkerRuntimeProfileReconciliation struct {
	TotalWorkers int `json:"total_workers"`
	// Backfilled carry a governed profile.
	Backfilled int `json:"backfilled"`
	// Unreconciled predate the registry and could not be matched to a routable
	// profile by engine. Non-zero means NOT NULL would fail.
	Unreconciled int `json:"unreconciled"`
	// EngineMismatched carry a profile whose engine disagrees with the legacy
	// string. The trigger refuses to create these; a non-zero count means one
	// arrived before the trigger existed.
	EngineMismatched int `json:"engine_mismatched"`
	// Ready reports whether step 6 (NOT NULL) and step 7 (drop the old check)
	// are safe to apply.
	Ready bool `json:"ready"`
}

// ReconcileWorkerRuntimeProfiles reports whether every worker row now carries a
// governed profile identity that agrees with its legacy engine string.
func (s *Store) ReconcileWorkerRuntimeProfiles(ctx context.Context) (WorkerRuntimeProfileReconciliation, error) {
	var out WorkerRuntimeProfileReconciliation
	err := s.pool.QueryRow(ctx, `
		SELECT COUNT(*),
		       COUNT(*) FILTER (WHERE w.runtime_profile_id IS NOT NULL),
		       COUNT(*) FILTER (WHERE w.runtime_profile_id IS NULL),
		       COUNT(*) FILTER (
		         WHERE w.runtime_profile_id IS NOT NULL
		           AND COALESCE(w.engine,'') <> ''
		           AND NOT EXISTS (
		             SELECT 1 FROM runtime_profiles p
		              WHERE p.runtime_profile_id = w.runtime_profile_id
		                AND p.engine = w.engine))
		  FROM workers w`).
		Scan(&out.TotalWorkers, &out.Backfilled, &out.Unreconciled, &out.EngineMismatched)
	if err != nil {
		return out, err
	}
	out.Ready = out.Unreconciled == 0 && out.EngineMismatched == 0
	return out, nil
}
