package main

import (
	"context"
	"fmt"
	"log"
	"os"
	"strconv"
	"time"
)

// Retention for the durable exact-result cache.
//
// Every distinct deterministic request body from every tenant used to be a
// permanent row, and the content-addressed object behind it permanent storage.
// There was no TTL, no size bound, and no worker caller. This reuses the
// job-object retention pattern: age-based claim, size-based trim, sweep from
// Workers.Run, delete the backing object with the row (when no other row still
// references it).

const (
	defaultExactResultRetention    = 7 * 24 * time.Hour
	defaultExactResultMaxBytes     = int64(512 << 20) // 512 MiB across all tenants
	exactResultRetentionSweep      = 1 * time.Hour
	exactResultRetentionBatch      = 200
	exactResultRetentionTickerName = "exact-result-retention"
)

// exactResultRetentionPeriod is how long a cache row may live after create
// (or last hit, if any). Shorter than job payload retention on purpose: a miss
// re-runs inference, it does not destroy dispute evidence.
func exactResultRetentionPeriod() time.Duration {
	raw := os.Getenv("MERC_EXACT_RESULT_RETENTION_HOURS")
	if raw == "" {
		return defaultExactResultRetention
	}
	hours, err := strconv.Atoi(raw)
	if err != nil || hours < 1 {
		log.Printf("retention: ignoring MERC_EXACT_RESULT_RETENTION_HOURS=%q "+
			"(must be an integer >= 1); using %s", raw, defaultExactResultRetention)
		return defaultExactResultRetention
	}
	return time.Duration(hours) * time.Hour
}

// exactResultCacheMaxBytes is the soft total-size bound. When exceeded the
// sweep trims least-recently-hit (then oldest) rows until under budget.
func exactResultCacheMaxBytes() int64 {
	raw := os.Getenv("MERC_EXACT_RESULT_MAX_BYTES")
	if raw == "" {
		return defaultExactResultMaxBytes
	}
	n, err := strconv.ParseInt(raw, 10, 64)
	if err != nil || n < 1 {
		log.Printf("retention: ignoring MERC_EXACT_RESULT_MAX_BYTES=%q "+
			"(must be a positive integer); using %d", raw, defaultExactResultMaxBytes)
		return defaultExactResultMaxBytes
	}
	return n
}

// exactCacheEviction is one row the sweep has decided to drop.
type exactCacheEviction struct {
	Identity  string
	ResultRef string
}

// ClaimExactResultsForAgeRetention leases aged exact-result rows for deletion.
// Age is measured from coalesce(last_hit_at, created_at) so a frequently reused
// entry is not purged while cold ones of the same create age are.
func (s *Store) ClaimExactResultsForAgeRetention(
	ctx context.Context,
	olderThan time.Duration,
	limit int,
) ([]exactCacheEviction, error) {
	if limit <= 0 || olderThan <= 0 {
		return nil, nil
	}
	rows, err := s.pool.Query(ctx, `
		WITH candidates AS (
		  SELECT request_identity, result_ref
		    FROM exact_result_cache
		   WHERE coalesce(last_hit_at, created_at) <= now() - make_interval(secs => $1)
		   ORDER BY coalesce(last_hit_at, created_at)
		   FOR UPDATE SKIP LOCKED
		   LIMIT $2
		)
		DELETE FROM exact_result_cache e
		  USING candidates c
		 WHERE e.request_identity = c.request_identity
		RETURNING e.request_identity, e.result_ref`,
		int64(olderThan/time.Second), limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return scanExactCacheEvictions(rows)
}

// ClaimExactResultsForSizeRetention deletes least-valuable rows while total
// result_bytes exceeds the budget. Value is "recently hit, then recently
// created"; cold bulk is first out. One batch per call; the next tick continues
// if still over budget.
func (s *Store) ClaimExactResultsForSizeRetention(
	ctx context.Context,
	maxBytes int64,
	limit int,
) ([]exactCacheEviction, error) {
	if maxBytes <= 0 || limit <= 0 {
		return nil, nil
	}
	var total int64
	if err := s.pool.QueryRow(ctx,
		`SELECT coalesce(sum(result_bytes), 0) FROM exact_result_cache`).Scan(&total); err != nil {
		return nil, err
	}
	if total <= maxBytes {
		return nil, nil
	}
	needFree := total - maxBytes
	// Walk oldest-first. Include every row whose bytes still contribute to the
	// gap (cum - result_bytes < needFree), so a single large row that straddles
	// the cut is still removed.
	rows, err := s.pool.Query(ctx, `
		WITH ranked AS (
		  SELECT request_identity, result_ref, result_bytes,
		         sum(result_bytes) OVER (
		           ORDER BY coalesce(last_hit_at, created_at) ASC,
		                    created_at ASC
		           ROWS BETWEEN UNBOUNDED PRECEDING AND CURRENT ROW
		         ) AS cum
		    FROM exact_result_cache
		),
		victims AS (
		  SELECT request_identity, result_ref
		    FROM ranked
		   WHERE cum - result_bytes < $1
		   ORDER BY cum
		   LIMIT $2
		)
		DELETE FROM exact_result_cache e
		  USING victims v
		 WHERE e.request_identity = v.request_identity
		RETURNING e.request_identity, e.result_ref`,
		needFree, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return scanExactCacheEvictions(rows)
}

func scanExactCacheEvictions(rows interface {
	Next() bool
	Scan(dest ...any) error
	Err() error
}) ([]exactCacheEviction, error) {
	var out []exactCacheEviction
	for rows.Next() {
		var e exactCacheEviction
		if err := rows.Scan(&e.Identity, &e.ResultRef); err != nil {
			return nil, err
		}
		out = append(out, e)
	}
	return out, rows.Err()
}

// exactResultRefStillReferenced reports whether any remaining cache row points
// at ref. Content-addressed objects may be shared across identities when two
// distinct requests produce identical completion bytes; only the last reference
// may delete the object.
func (s *Store) exactResultRefStillReferenced(ctx context.Context, ref string) (bool, error) {
	var n int
	err := s.pool.QueryRow(ctx,
		`SELECT count(*) FROM exact_result_cache WHERE result_ref = $1`, ref).Scan(&n)
	return n > 0, err
}

func (wk *Workers) sweepExactResultCache(ctx context.Context) error {
	if wk.store == nil {
		return nil
	}
	retention := exactResultRetentionPeriod()
	aged, err := wk.store.ClaimExactResultsForAgeRetention(ctx, retention, exactResultRetentionBatch)
	if err != nil {
		return fmt.Errorf("exact-result age retention: %w", err)
	}
	sized, err := wk.store.ClaimExactResultsForSizeRetention(ctx, exactResultCacheMaxBytes(), exactResultRetentionBatch)
	if err != nil {
		return fmt.Errorf("exact-result size retention: %w", err)
	}
	evicted := append(aged, sized...)
	if len(evicted) == 0 {
		return nil
	}
	deletedObjects := 0
	for _, e := range evicted {
		if wk.storage == nil || e.ResultRef == "" {
			continue
		}
		still, err := wk.store.exactResultRefStillReferenced(ctx, e.ResultRef)
		if err != nil {
			return err
		}
		if still {
			continue
		}
		if err := wk.storage.RemoveObjects(ctx, []string{e.ResultRef}); err != nil {
			log.Printf("workers: exact-result-retention: removing %q: %v", e.ResultRef, err)
			return err
		}
		deletedObjects++
	}
	log.Printf("workers: exact-result-retention: evicted %d row(s) (age=%d size=%d), deleted %d object(s); retention=%s max_bytes=%d",
		len(evicted), len(aged), len(sized), deletedObjects, retention, exactResultCacheMaxBytes())
	return nil
}
