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
		digest, err := profile.ContentDigest(runtimeAuthorityModels)
		if err != nil {
			return err
		}
		if err := assertNoContentDrift(ctx, tx, profile, digest); err != nil {
			return err
		}
		routable := runtimeLifecycleRoutable(profile.Lifecycle)

		// Demote every other revision of this profile BEFORE inserting the new
		// one. Retained, never deleted: a receipt that named an older revision
		// must still resolve.
		//
		// Order is load-bearing. runtime_profiles_one_current is a partial unique
		// index enforced per statement, so inserting r5 while r4 still holds
		// is_current violates it — the whole migration aborts with a duplicate
		// key on a profile whose content is perfectly valid. The first real
		// revision bump against a populated database is what surfaced this; a
		// bump had only ever been exercised against an empty table, where the
		// demote-after ordering is indistinguishable from the correct one.
		// Routability goes with is_current: another revision is about to become
		// the current one, so this row can no longer receive work. The lifecycle
		// it was retired at is kept verbatim — that is the history.
		if _, err := tx.Exec(ctx, `
			UPDATE runtime_profiles SET is_current = false, routable = false
			 WHERE runtime_profile_id = $1 AND revision <> $2 AND is_current`,
			profile.RuntimeID, profile.Revision); err != nil {
			return fmt.Errorf("demote prior revisions of %q: %w", profile.RuntimeID, err)
		}
		// And the denormalized copy the cell-uniqueness index reads, or the
		// superseded revision keeps claiming every cell the new one declares.
		//
		// Lifecycle moves with routability, because routability is derived from
		// it and the CHECK refuses one without the other. A superseded revision's
		// cell is recorded as RETIRED rather than left claiming ACTIVE while
		// flagged unroutable — the latter is a row that contradicts itself, and
		// the constraint exists to make that unrepresentable.
		if _, err := tx.Exec(ctx, `
			UPDATE runtime_profile_models m
			   SET routable = false, lifecycle = 'RETIRED'
			  FROM runtime_profiles p
			 WHERE p.runtime_profile_id = m.runtime_profile_id
			   AND p.revision = m.revision
			   AND m.runtime_profile_id = $1 AND m.revision <> $2
			   AND m.routable AND NOT p.is_current`,
			profile.RuntimeID, profile.Revision); err != nil {
			return fmt.Errorf("demote prior revision cells of %q: %w", profile.RuntimeID, err)
		}

		// Insert the revision if absent; NEVER overwrite an existing one's
		// content. assertNoContentDrift has already refused a changed digest
		// under an existing revision, so a conflict here is a re-sync of
		// identical content and the content columns are simply re-asserted.
		//
		// Lifecycle, routability and supersession are the only things that move,
		// and they move on the row for THIS revision. Historical revisions keep
		// the lifecycle they were retired at.
		if _, err := tx.Exec(ctx, `
			INSERT INTO runtime_profiles
			  (runtime_profile_id, revision, profile_digest, engine, adapter, lifecycle,
			   routable, quality_tier, benchmark_authority, source_identity,
			   superseded_by, device_min, device_max, is_current, updated_at)
			VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,true,now())
			ON CONFLICT (runtime_profile_id, revision) DO UPDATE SET
			  lifecycle = EXCLUDED.lifecycle,
			  routable = EXCLUDED.routable,
			  superseded_by = EXCLUDED.superseded_by,
			  is_current = true,
			  updated_at = now()`,
			profile.RuntimeID, profile.Revision, digest, profile.Engine, profile.Adapter,
			profile.Lifecycle, routable, profile.QualityTier, profile.BenchmarkAuthority,
			profile.SourceIdentity, profile.SupersededBy,
			profile.Hardware.DeviceCount.Minimum, profile.Hardware.DeviceCount.Maximum,
		); err != nil {
			return fmt.Errorf("sync runtime profile %q %s: %w",
				profile.RuntimeID, profile.Revision, err)
		}
		// Child rows are replaced wholesale. They are pure projections of the
		// profile content that assertNoContentDrift just pinned, so a replace
		// cannot lose information the document does not still carry.
		for _, table := range []string{
			"runtime_profile_models", "runtime_profile_hardware", "runtime_profile_capabilities",
		} {
			// Scoped to THIS revision. A wholesale delete by profile id would
			// destroy the child rows of every historical revision.
			if _, err := tx.Exec(ctx,
				`DELETE FROM `+table+` WHERE runtime_profile_id = $1 AND revision = $2`,
				profile.RuntimeID, profile.Revision); err != nil {
				return fmt.Errorf("clear %s for %q %s: %w",
					table, profile.RuntimeID, profile.Revision, err)
			}
		}
		for _, cell := range profile.Cells {
			// The cell's EFFECTIVE lifecycle, and routability derived from it —
			// not the profile's. A profile-level ACTIVE must not carry a
			// REJECTED_FOR_CONTRACT cell into the routable set with it.
			effective := cell.EffectiveLifecycle(profile)
			if _, err := tx.Exec(ctx, `
				INSERT INTO runtime_profile_models
				  (runtime_profile_id, revision, cell_id, job_type, model_id, runner,
				   min_memory_gb, verification, routable, lifecycle, quality_tier,
				   benchmark_authority, rejection_reason, max_batch, max_concurrency)
				VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,$15)`,
				profile.RuntimeID, profile.Revision, cell.ID, cell.Job, cell.Model,
				cell.Runner, cell.MinMemoryGB, cell.Verification,
				cell.Routable(profile), effective,
				cell.qualityTierFor(profile), cell.benchmarkAuthorityFor(profile),
				cell.RejectionReason, cell.MaxBatch, cell.MaxConcurrency); err != nil {
				return fmt.Errorf("sync cell %q of %q: %w", cell.ID, profile.RuntimeID, err)
			}
		}
		for _, platform := range profile.Hardware.Platforms {
			if _, err := tx.Exec(ctx, `
				INSERT INTO runtime_profile_hardware (runtime_profile_id, revision, platform)
				VALUES ($1,$2,$3) ON CONFLICT DO NOTHING`,
				profile.RuntimeID, profile.Revision, platform); err != nil {
				return fmt.Errorf("sync platform %q of %q: %w", platform, profile.RuntimeID, err)
			}
		}
		for _, capability := range profile.declaredCapabilities() {
			if _, err := tx.Exec(ctx, `
				INSERT INTO runtime_profile_capabilities (runtime_profile_id, revision, capability)
				VALUES ($1,$2,$3) ON CONFLICT DO NOTHING`,
				profile.RuntimeID, profile.Revision, capability); err != nil {
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
	//
	// All THREE identity columns, not just the id. workers_runtime_profile_identity
	// requires 0 or 3 non-nulls, and this backfill was writing 1 — caught by that
	// constraint once it was made NULL-safe. Partial identity is not identity: a
	// row naming a profile with no revision cannot say which meaning of it.
	if _, err := tx.Exec(ctx, `
		UPDATE workers w
		   SET runtime_profile_id = p.runtime_profile_id,
		       runtime_profile_revision = p.revision,
		       runtime_profile_digest = p.profile_digest
		  FROM runtime_profiles p
		 WHERE w.runtime_profile_id IS NULL
		   AND p.engine = w.engine
		   AND p.routable
		   AND p.is_current`); err != nil {
		return fmt.Errorf("backfill worker runtime profiles: %w", err)
	}
	return tx.Commit(ctx)
}

func assertNoContentDrift(
	ctx context.Context, tx pgx.Tx, profile authorityRuntimeProfile, digest string,
) error {
	// Scoped to the exact revision. With history retained there are several rows
	// per profile, and drift is only meaningful against the row claiming the same
	// revision — an older revision having a different digest is the point.
	var existingDigest string
	err := tx.QueryRow(ctx,
		`SELECT profile_digest FROM runtime_profiles
		  WHERE runtime_profile_id = $1 AND revision = $2`,
		profile.RuntimeID, profile.Revision).Scan(&existingDigest)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil // this revision is new
	}
	if err != nil {
		return err
	}
	if existingDigest != digest {
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
		                AND p.revision = w.runtime_profile_revision
		                AND p.engine = w.engine))
		  FROM workers w`).
		Scan(&out.TotalWorkers, &out.Backfilled, &out.Unreconciled, &out.EngineMismatched)
	if err != nil {
		return out, err
	}
	out.Ready = out.Unreconciled == 0 && out.EngineMismatched == 0
	return out, nil
}
