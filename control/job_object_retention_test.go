package main

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/google/uuid"
)

// seedTerminalJob writes a job that went terminal `age` ago, plus the four
// object shapes merc actually creates under jobs/<id>/.
func seedTerminalJob(
	t *testing.T,
	ctx context.Context,
	store *Store,
	storage *Storage,
	buyerID uuid.UUID,
	age time.Duration,
) (uuid.UUID, []string) {
	t.Helper()
	jobID := uuid.New()
	taskID := uuid.New()

	keys := []string{
		fmt.Sprintf("jobs/%s/input.jsonl", jobID),
		fmt.Sprintf("jobs/%s/output.jsonl", jobID),
		fmt.Sprintf("jobs/%s/tasks/%s/input.jsonl", jobID, taskID),
		fmt.Sprintf("jobs/%s/tasks/%s/attempt-1/result.json", jobID, taskID),
	}
	for _, k := range keys {
		mustf(t, storage.PutObject(ctx, k, []byte("payload-"+k), "application/octet-stream"), "seed %q: %v", k)
	}

	if _, err := store.pool.Exec(ctx, `
		INSERT INTO jobs (id, buyer_id, status, job_type, tier, input_ref, output_ref,
		                  created_at, terminal_at)
		VALUES ($1,$2,'complete','embed','batch',$3,$4, now() - $5::interval, now() - $5::interval)`,
		jobID, buyerID, keys[0], keys[1], fmt.Sprintf("%d seconds", int(age.Seconds()))); err != nil {
		t.Fatalf("insert job: %v", err)
	}
	return jobID, keys
}

func objectsPresent(t *testing.T, ctx context.Context, storage *Storage, keys []string) int {
	t.Helper()
	n := 0
	for _, k := range keys {
		exists, err := storage.ObjectExists(ctx, k)
		mustf(t, err, "exists %q: %v", k)
		if exists {
			n++
		}
	}
	return n
}

// A job's payload objects are removed once it has been terminal past the
// retention period, and a job still inside it is untouched.
func TestJobObjectRetentionPurgesOnlyAgedTerminalJobs(t *testing.T) {
	ctx, store, _ := openAdminMutationTestStore(t)
	storage := openObjectStorageForTest(t)
	wk := NewWorkers(store, storage, nil)

	buyerID := uuid.New()
	if _, err := store.pool.Exec(ctx,
		`INSERT INTO buyers (id,email) VALUES ($1,$2) ON CONFLICT DO NOTHING`,
		buyerID, "retention-"+buyerID.String()+"@example.invalid"); err != nil {
		t.Fatalf("seed buyer: %v", err)
	}

	retention := jobObjectRetentionPeriod()
	oldID, oldKeys := seedTerminalJob(t, ctx, store, storage, buyerID, retention+48*time.Hour)
	freshID, freshKeys := seedTerminalJob(t, ctx, store, storage, buyerID, time.Hour)

	if got := objectsPresent(t, ctx, storage, oldKeys); got != len(oldKeys) {
		t.Fatalf("seeded %d/%d aged objects", got, len(oldKeys))
	}

	mustf(t, wk.sweepJobObjectRetention(ctx), "sweep: %v")

	// Every shape under the prefix goes, not just input and output: per-task
	// inputs and per-attempt results are buyer payload too.
	if got := objectsPresent(t, ctx, storage, oldKeys); got != 0 {
		t.Fatalf("aged job kept %d/%d objects after retention", got, len(oldKeys))
	}
	if got := objectsPresent(t, ctx, storage, freshKeys); got != len(freshKeys) {
		t.Fatalf("job inside retention lost objects: %d/%d remain", got, len(freshKeys))
	}

	purged, err := store.JobObjectsPurged(ctx, oldID)
	if err != nil || !purged {
		t.Fatalf("aged job not marked purged: purged=%v err=%v", purged, err)
	}
	if purged, err := store.JobObjectsPurged(ctx, freshID); err != nil || purged {
		t.Fatalf("fresh job marked purged: purged=%v err=%v", purged, err)
	}

	// Idempotent: a second pass must not re-claim what it already purged.
	ids, err := store.ClaimJobsForObjectRetention(ctx, retention, 100)
	mustf(t, err, "re-claim: %v")
	for _, id := range ids {
		if id == oldID {
			t.Fatal("already-purged job was claimed again")
		}
	}
}

// An unresolved dispute holds the objects regardless of age: the dispute is
// about what the job produced and resolving it moves money.
func TestJobObjectRetentionHoldsDisputedJobs(t *testing.T) {
	ctx, store, _ := openAdminMutationTestStore(t)
	storage := openObjectStorageForTest(t)
	wk := NewWorkers(store, storage, nil)

	buyerID := uuid.New()
	if _, err := store.pool.Exec(ctx,
		`INSERT INTO buyers (id,email) VALUES ($1,$2) ON CONFLICT DO NOTHING`,
		buyerID, "disputed-"+buyerID.String()+"@example.invalid"); err != nil {
		t.Fatalf("seed buyer: %v", err)
	}

	retention := jobObjectRetentionPeriod()
	jobID, keys := seedTerminalJob(t, ctx, store, storage, buyerID, retention+90*24*time.Hour)

	for _, status := range []string{"open", "no_peer", "reverifying", "unresolvable"} {
		// No t.Skip here on purpose. This is the assertion that protects the
		// evidence a money-moving dispute resolution reads; a schema change
		// that breaks the insert must fail the suite, not excuse it.
		if _, err := store.pool.Exec(ctx, `
			INSERT INTO disputes (id,job_id,buyer_id,status,reason,filing_deadline)
			VALUES ($1,$2,$3,$4,'retention hold test', now() + interval '7 days')`,
			uuid.New(), jobID, buyerID, status); err != nil {
			t.Fatalf("insert %s dispute: %v", status, err)
		}

		mustf(t, wk.sweepJobObjectRetention(ctx), "sweep with %s dispute: %v", status)
		if got := objectsPresent(t, ctx, storage, keys); got != len(keys) {
			t.Fatalf("dispute status %q did not hold objects: %d/%d remain",
				status, got, len(keys))
		}

		if _, err := store.pool.Exec(ctx, `DELETE FROM disputes WHERE job_id=$1`, jobID); err != nil {
			t.Fatalf("clear dispute: %v", err)
		}
	}

	// With no unresolved dispute the same job is purged, proving the hold was
	// the dispute and not the age.
	mustf(t, wk.sweepJobObjectRetention(ctx), "sweep after dispute cleared: %v")
	if got := objectsPresent(t, ctx, storage, keys); got != 0 {
		t.Fatalf("undisputed aged job kept %d/%d objects", got, len(keys))
	}
}

// A retention shorter than the dispute filing window would delete the evidence
// for a dispute the buyer is still entitled to file.
func TestJobObjectRetentionRefusesPeriodInsideDisputeWindow(t *testing.T) {
	for _, raw := range []string{"1", "7", "0", "-3", "not-a-number"} {
		t.Setenv("MERC_JOB_OBJECT_RETENTION_DAYS", raw)
		if got := jobObjectRetentionPeriod(); got != defaultJobObjectRetention {
			t.Fatalf("MERC_JOB_OBJECT_RETENTION_DAYS=%q yielded %s, want the %s default",
				raw, got, defaultJobObjectRetention)
		}
	}
	t.Setenv("MERC_JOB_OBJECT_RETENTION_DAYS", "8")
	if got := jobObjectRetentionPeriod(); got != 8*24*time.Hour {
		t.Fatalf("8 days rejected: got %s", got)
	}
}

// RemovePrefix with an empty prefix would match every object merc holds.
func TestRemovePrefixRefusesEmptyPrefix(t *testing.T) {
	ctx, store, _ := openAdminMutationTestStore(t)
	_ = store
	storage := openObjectStorageForTest(t)

	canary := "jobs/" + uuid.NewString() + "/input.jsonl"
	mustf(t, storage.PutObject(ctx, canary, []byte("canary"), "application/octet-stream"), "seed canary: %v")
	t.Cleanup(func() { _ = storage.RemoveObjects(context.Background(), []string{canary}) })

	for _, prefix := range []string{"", "   ", "\t"} {
		if err := storage.RemovePrefix(ctx, prefix); err == nil {
			t.Fatalf("RemovePrefix(%q) was accepted", prefix)
		}
	}
	exists, err := storage.ObjectExists(ctx, canary)
	if err != nil || !exists {
		t.Fatalf("canary object removed by a refused prefix: exists=%v err=%v", exists, err)
	}
}
