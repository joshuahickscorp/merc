package main

import (
	"context"
	"fmt"
	"log"
	"os"
	"strconv"
	"time"

	"github.com/google/uuid"
)

// Ordinary retention for job payload objects.
//
// Buyer-requested erasure already has a path (buyer_identity_tombstones ->
// pending_object_deletions), and every delete on it is authorised against the
// digests recorded in the tombstone. That is deliberately narrow: it exists to
// prove an erasure request was honoured exactly and no further.
//
// It is not a retention policy. Without one, a job's input and output objects
// stay in the bucket forever: storage cost grows without bound, and buyer
// payloads are held indefinitely for no stated reason -- which is the thing a
// retention period exists to avoid. This sweep is that policy, and it is
// separate from erasure on purpose. Routing ordinary retention through the
// tombstone queue would mean minting tombstones for buyers who never asked for
// erasure, which would make the erasure audit trail describe requests that were
// never made.
const (
	defaultJobObjectRetention = 30 * 24 * time.Hour
	jobObjectRetentionSweep   = 1 * time.Hour
	jobObjectRetentionBatch   = 100

	// jobObjectRetentionPolicyRevision is the revision emitted by current
	// admissions. FrozenCostPolicySnapshot v1 deliberately validates against its
	// own permanent revision constant; changing this value without also bumping
	// the snapshot version makes new admission fail closed rather than invalidating
	// historical v1 decisions.
	jobObjectRetentionPolicyRevision = "job-object-retention-v1"
	defaultJobObjectRetentionBasis   = "job-object-retention-v1 default: retain payload objects for 30 days after terminal_at and hold them longer while a dispute is unresolved"
)

// JobObjectRetentionPolicy is the process policy used by new admissions. A
// PricingDecision freezes its duration, revision and basis; historical replay
// never calls this loader.
type JobObjectRetentionPolicy struct {
	Duration time.Duration
	Revision string
	Basis    string
}

// jobObjectRetention is the age past terminal_at at which payload objects go.
//
// The floor is the seven-day dispute filing window (errDisputeWindowClosed): a
// buyer who can still open a dispute must still be able to fetch the output
// they are disputing, and a resolver must still be able to read it. A shorter
// setting would delete the evidence for a dispute the buyer is entitled to
// file, so it is refused rather than clamped silently.
func currentJobObjectRetentionPolicy() JobObjectRetentionPolicy {
	policy := JobObjectRetentionPolicy{
		Duration: defaultJobObjectRetention,
		Revision: jobObjectRetentionPolicyRevision,
		Basis:    defaultJobObjectRetentionBasis,
	}
	raw := os.Getenv("MERC_JOB_OBJECT_RETENTION_DAYS")
	if raw == "" {
		return policy
	}
	days, err := strconv.Atoi(raw)
	// time.Duration is int64 nanoseconds. Refuse an overflowing configured
	// period rather than wrapping it into a short or negative retention.
	maxDays := int64(^uint64(0)>>1) / int64(24*time.Hour)
	if err != nil || days < 8 || int64(days) > maxDays {
		log.Printf("retention: ignoring MERC_JOB_OBJECT_RETENTION_DAYS=%q "+
			"(must be a representable integer >= 8 so the 7-day dispute window keeps its evidence); "+
			"using %s", raw, defaultJobObjectRetention)
		return policy
	}
	policy.Duration = time.Duration(days) * 24 * time.Hour
	policy.Basis = fmt.Sprintf(
		"job-object-retention-v1 configured by MERC_JOB_OBJECT_RETENTION_DAYS=%d: retain payload objects for %d days after terminal_at and hold them longer while a dispute is unresolved",
		days, days,
	)
	return policy
}

func jobObjectRetentionPeriod() time.Duration {
	return currentJobObjectRetentionPolicy().Duration
}

// ClaimJobsForObjectRetention leases terminal jobs whose payload objects are
// past retention.
//
// A job with an unresolved dispute is never returned, at any age. The dispute
// is about what the job produced, so deleting the output while it is contested
// would destroy the evidence the resolution depends on -- and the resolution
// moves money.
func (s *Store) ClaimJobsForObjectRetention(
	ctx context.Context,
	olderThan time.Duration,
	limit int,
) ([]uuid.UUID, error) {
	if limit <= 0 || olderThan <= 0 {
		return nil, nil
	}
	rows, err := s.pool.Query(ctx, `
		WITH candidates AS (
		  SELECT j.id
		    FROM jobs j
		   WHERE j.objects_purged_at IS NULL
		     AND j.terminal_at IS NOT NULL
		     AND j.terminal_at <= now() - make_interval(secs => $1)
		     AND NOT EXISTS (
		           SELECT 1 FROM disputes d
		            WHERE d.job_id = j.id
		              AND d.status IN ('open','no_peer','reverifying','unresolvable'))
		   ORDER BY j.terminal_at
		   FOR UPDATE SKIP LOCKED
		   LIMIT $2
		)
		SELECT id FROM candidates`,
		int64(olderThan/time.Second), limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []uuid.UUID
	for rows.Next() {
		var id uuid.UUID
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		out = append(out, id)
	}
	return out, rows.Err()
}

// MarkJobObjectsPurged records that a job's payload objects are gone.
//
// Written only after the storage removal returns success, so a crash between
// the two leaves the job unmarked and the sweep simply removes an already
// absent prefix on the next pass. RemoveObjects treats missing keys as success
// precisely so that retry is safe.
func (s *Store) MarkJobObjectsPurged(ctx context.Context, jobID uuid.UUID) error {
	_, err := s.pool.Exec(ctx,
		`UPDATE jobs SET objects_purged_at = now()
		  WHERE id = $1 AND objects_purged_at IS NULL`, jobID)
	return err
}

// JobObjectsPurged reports whether a job's payload objects have been removed,
// so the API can say so rather than presigning a URL that will 404.
func (s *Store) JobObjectsPurged(ctx context.Context, jobID uuid.UUID) (bool, error) {
	var purged bool
	err := s.pool.QueryRow(ctx,
		`SELECT objects_purged_at IS NOT NULL FROM jobs WHERE id = $1`, jobID).Scan(&purged)
	return purged, err
}

func (wk *Workers) sweepJobObjectRetention(ctx context.Context) error {
	if wk.storage == nil {
		return nil
	}
	retention := jobObjectRetentionPeriod()
	ids, err := wk.store.ClaimJobsForObjectRetention(ctx, retention, jobObjectRetentionBatch)
	if err != nil {
		return err
	}
	purged := 0
	for _, id := range ids {
		// Every key merc writes for a job lives under this prefix: input,
		// merged output, per-task inputs and per-attempt results. Deleting the
		// prefix removes them all without having to enumerate a layout that
		// four separate call sites construct independently.
		prefix := fmt.Sprintf("jobs/%s/", id)
		if err := wk.storage.RemovePrefix(ctx, prefix); err != nil {
			log.Printf("workers: object-retention: removing %q: %v", prefix, err)
			return err
		}
		if err := wk.store.MarkJobObjectsPurged(ctx, id); err != nil {
			return err
		}
		purged++
	}
	if purged > 0 {
		log.Printf("workers: object-retention: purged payload objects for %d job(s) terminal for more than %s",
			purged, retention)
	}
	return nil
}
