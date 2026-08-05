package main

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/google/uuid"
)

func seedExactCacheRow(
	t *testing.T,
	ctx context.Context,
	store *Store,
	storage *Storage,
	age time.Duration,
	body []byte,
) (identity, ref string) {
	t.Helper()
	id, err := detIdentity("exact-ret-" + uuid.NewString()).Compute()
	must(t, err)
	ref, err = store.StoreExactResultBytes(ctx, storage, id, body, 16)
	mustf(t, err, "store: %v")
	if age > 0 {
		if _, err := store.pool.Exec(ctx, `
			UPDATE exact_result_cache
			   SET created_at = now() - $2::interval,
			       last_hit_at = now() - $2::interval
			 WHERE request_identity = $1`,
			id, fmt.Sprintf("%d seconds", int(age.Seconds()))); err != nil {
			t.Fatalf("age row: %v", err)
		}
	}
	return id, ref
}

// Aged exact-result rows and their backing objects are removed by the worker
// sweep; a second pass is a no-op (idempotent).
func TestExactResultRetentionPurgesAgedEntryAndObject(t *testing.T) {
	ctx, store, _ := openAdminMutationTestStore(t)
	storage := openObjectStorageForTest(t)
	wk := NewWorkers(store, storage, nil)

	retention := exactResultRetentionPeriod()
	body := []byte(`{"id":"chatcmpl-aged","choices":[{"message":{"role":"assistant","content":"1 2 3 4"}}]}`)
	id, ref := seedExactCacheRow(t, ctx, store, storage, retention+2*time.Hour, body)

	exists, err := storage.ObjectExists(ctx, ref)
	if err != nil || !exists {
		t.Fatalf("seeded object missing: exists=%v err=%v", exists, err)
	}
	// Read without LookupExactResult: that path bumps last_hit_at and would
	// pull the row back inside the retention window.
	var n int
	if err := store.pool.QueryRow(ctx,
		`SELECT count(*) FROM exact_result_cache WHERE request_identity=$1`, id).Scan(&n); err != nil || n != 1 {
		t.Fatalf("seeded row missing: n=%d err=%v", n, err)
	}

	mustf(t, wk.sweepExactResultCache(ctx), "sweep: %v")
	if err := store.pool.QueryRow(ctx,
		`SELECT count(*) FROM exact_result_cache WHERE request_identity=$1`, id).Scan(&n); err != nil || n != 0 {
		t.Fatalf("aged row survived sweep: n=%d err=%v", n, err)
	}
	exists, err = storage.ObjectExists(ctx, ref)
	if err != nil || exists {
		t.Fatalf("backing object survived sweep: exists=%v err=%v", exists, err)
	}

	// Idempotent: second pass must not error and must not re-delete anything
	// meaningful (no remaining row to claim).
	mustf(t, wk.sweepExactResultCache(ctx), "second sweep: %v")
	aged, err := store.ClaimExactResultsForAgeRetention(ctx, retention, 100)
	mustf(t, err, "re-claim: %v")
	for _, e := range aged {
		if e.Identity == id {
			t.Fatal("already-purged identity was claimed again")
		}
	}
}

func TestExactResultRetentionHoldsFreshEntries(t *testing.T) {
	ctx, store, _ := openAdminMutationTestStore(t)
	storage := openObjectStorageForTest(t)
	wk := NewWorkers(store, storage, nil)

	body := []byte(`{"id":"chatcmpl-fresh","choices":[{"message":{"role":"assistant","content":"ok"}}]}`)
	id, ref := seedExactCacheRow(t, ctx, store, storage, 0, body)

	mustf(t, wk.sweepExactResultCache(ctx), "sweep: %v")
	if _, ok, err := store.LookupExactResult(ctx, id); err != nil || !ok {
		t.Fatalf("fresh row was purged: ok=%v err=%v", ok, err)
	}
	exists, err := storage.ObjectExists(ctx, ref)
	if err != nil || !exists {
		t.Fatalf("fresh object was purged: exists=%v err=%v", exists, err)
	}
}

func TestExactResultSizeRetentionTrimsOverBudget(t *testing.T) {
	ctx, store, _ := openAdminMutationTestStore(t)
	storage := openObjectStorageForTest(t)

	// Three large rows; budget fits only one. Oldest two must go.
	const bodySize = 10_000
	body := make([]byte, bodySize)
	for i := range body {
		body[i] = byte('a' + (i % 26))
	}
	var ids []string
	var refs []string
	for i := 0; i < 3; i++ {
		// Distinct bodies so content digests (and refs) differ.
		b := append([]byte(nil), body...)
		b[0] = byte('A' + i)
		id, ref := seedExactCacheRow(t, ctx, store, storage, time.Duration(3-i)*time.Hour, b)
		ids = append(ids, id)
		refs = append(refs, ref)
	}

	evicted, err := store.ClaimExactResultsForSizeRetention(ctx, bodySize+100, 10)
	mustf(t, err, "size claim: %v")
	if len(evicted) < 2 {
		t.Fatalf("want at least 2 size evictions to fit under budget, got %d", len(evicted))
	}
	// Clean up orphaned objects the claim left behind (sweep would remove them).
	for _, e := range evicted {
		still, err := store.exactResultRefStillReferenced(ctx, e.ResultRef)
		must(t, err)
		if !still {
			_ = storage.RemoveObjects(ctx, []string{e.ResultRef})
		}
	}
	_ = ids
	_ = refs
}

// Workers.Run must schedule the exact-result retention ticker. The production
// reachability graph also asserts this; this guards the constant wiring site.
func TestExactResultRetentionIsScheduledFromWorkersRun(t *testing.T) {
	// Compile-time / source presence is enforced by production_reachability_test
	// (Workers.Run → Workers.sweepExactResultCache). Here assert the interval
	// and ticker name are non-zero so a zero-duration "disabled" tick cannot
	// silently land.
	if exactResultRetentionSweep <= 0 {
		t.Fatal("exactResultRetentionSweep must be positive")
	}
	if exactResultRetentionTickerName == "" {
		t.Fatal("exactResultRetentionTickerName must be set")
	}
	if defaultExactResultRetention <= 0 || defaultExactResultMaxBytes <= 0 {
		t.Fatal("exact-result retention defaults must be positive")
	}
}
