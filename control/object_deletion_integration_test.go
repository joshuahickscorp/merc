package main

import (
	"context"
	"errors"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
)

func openObjectStorageForTest(t *testing.T) *Storage {
	t.Helper()
	for _, key := range []string{"S3_ENDPOINT", "S3_BUCKET", "S3_ACCESS_KEY", "S3_SECRET_KEY"} {
		if strings.TrimSpace(os.Getenv(key)) == "" {
			t.Skipf("%s is not set (need MinIO/S3 for object-storage integration test)", key)
		}
	}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	storage, err := NewStorage(ctx)
	if err != nil {
		t.Fatalf("connect object storage: %v", err)
	}
	return storage
}

// TestBuyerObjectDeletionQueueAndSweep is the DSAR object-erasure receipt.
// technical-exercises.sh binds deletable_data_removed to this test's exit status.
func TestBuyerObjectDeletionQueueAndSweep(t *testing.T) {
	prevGrace := objectDeletionGrace
	objectDeletionGrace = 0 // no presign window in this harness
	t.Cleanup(func() { objectDeletionGrace = prevGrace })

	ctx, store, pool := openAdminMutationTestStore(t)
	storage := openObjectStorageForTest(t)
	wk := NewWorkers(store, storage, nil)

	buyerID := uuid.New()
	jobID := uuid.New()
	taskID := uuid.New()
	email := "object-delete-" + buyerID.String() + "@example.invalid"
	admin := testAdminActor(uuid.New())
	admin.Label = "object-deletion-operator"

	keyA := "jobs/" + jobID.String() + "/input.jsonl"
	keyB := "jobs/" + jobID.String() + "/tasks/" + taskID.String() + "/result.jsonl"
	// Rogue key: present in the bucket but never on the tombstone digest list.
	keyRogue := "jobs/" + uuid.NewString() + "/not-authorized.jsonl"

	for _, key := range []string{keyA, keyB, keyRogue} {
		if err := storage.PutObject(ctx, key, []byte("synthetic-dsar-"+key), "application/octet-stream"); err != nil {
			t.Fatalf("seed object %q: %v", key, err)
		}
		exists, err := storage.ObjectExists(ctx, key)
		if err != nil || !exists {
			t.Fatalf("seeded object missing %q: exists=%v err=%v", key, exists, err)
		}
	}

	statements := []struct {
		query string
		args  []any
	}{
		{`INSERT INTO api_keys (id,key_hash,is_admin,revoked,name) VALUES ($1,$2,true,false,$3)`,
			[]any{admin.PrincipalID, "admin-test-" + admin.PrincipalID.String(), admin.Label}},
		{`INSERT INTO buyers (id,email,password_hash,free_credit_usd) VALUES ($1,$2,'synthetic-password-hash',0)`,
			[]any{buyerID, email}},
		{`INSERT INTO jobs (id,buyer_id,status,job_type,input_ref,output_ref)
		  VALUES ($1,$2,'complete','embed',$3,NULL)`,
			[]any{jobID, buyerID, keyA}},
		{`INSERT INTO tasks (id,job_id,status,input_ref,result_ref,result_key)
		  VALUES ($1,$2,'complete',$3,$4,$4)`,
			[]any{taskID, jobID, keyA, keyB}},
	}
	for _, statement := range statements {
		if _, err := pool.Exec(ctx, statement.query, statement.args...); err != nil {
			t.Fatalf("seed object-deletion fixture: %v\n%s", err, statement.query)
		}
	}

	correlation := "PRIV-OBJDEL-" + uuid.NewString()
	result, err := store.TombstoneBuyer(ctx, admin, buyerID, "verified object deletion request", correlation)
	if err != nil {
		t.Fatal(err)
	}
	if result.TombstoneID == uuid.Nil {
		t.Fatal("tombstone id missing")
	}
	artifactSet := map[string]bool{}
	for _, ref := range result.ArtifactRefs {
		artifactSet[ref] = true
	}
	for _, ref := range []string{keyA, keyB} {
		if !artifactSet[ref] {
			t.Fatalf("tombstone manifest omitted %q", ref)
		}
	}

	var queued int
	if err := pool.QueryRow(ctx, `
		SELECT count(*) FROM pending_object_deletions
		 WHERE tombstone_id=$1 AND object_key=ANY($2)`,
		result.TombstoneID, []string{keyA, keyB}).Scan(&queued); err != nil {
		t.Fatal(err)
	}
	if queued != 2 {
		t.Fatalf("expected 2 pending deletions, got %d", queued)
	}

	// Second tombstone with same correlation is a clean no-op (admin mutation replay).
	replay, err := store.TombstoneBuyer(ctx, admin, buyerID, "verified object deletion request", correlation)
	if err != nil || replay.TombstoneID != result.TombstoneID {
		t.Fatalf("re-tombstone was not a no-op: %+v %v", replay, err)
	}
	var queuedAfterReplay int
	if err := pool.QueryRow(ctx, `
		SELECT count(*) FROM pending_object_deletions WHERE tombstone_id=$1`,
		result.TombstoneID).Scan(&queuedAfterReplay); err != nil {
		t.Fatal(err)
	}
	if queuedAfterReplay != queued {
		t.Fatalf("re-tombstone re-queued objects: before=%d after=%d", queued, queuedAfterReplay)
	}

	// Insert an unauthorized pending row (digest not on the tombstone list).
	if _, err := pool.Exec(ctx, `
		INSERT INTO pending_object_deletions
		  (tombstone_id,buyer_id,object_key,object_key_sha256,not_before)
		VALUES ($1,$2,$3,$4,now())`,
		result.TombstoneID, buyerID, keyRogue, artifactRefDigest(keyRogue)); err != nil {
		t.Fatalf("seed unauthorized pending row: %v", err)
	}

	if err := wk.sweepPendingObjectDeletions(ctx); err != nil {
		t.Fatalf("sweeper: %v", err)
	}

	for _, key := range []string{keyA, keyB} {
		exists, err := storage.ObjectExists(ctx, key)
		if err != nil {
			t.Fatalf("ObjectExists %q: %v", key, err)
		}
		if exists {
			t.Fatalf("object %q still present after sweeper", key)
		}
	}
	rogueExists, err := storage.ObjectExists(ctx, keyRogue)
	if err != nil {
		t.Fatal(err)
	}
	if !rogueExists {
		t.Fatal("unauthorized object was deleted; tombstone digest gate failed")
	}
	var refused int
	if err := pool.QueryRow(ctx, `
		SELECT count(*) FROM pending_object_deletions
		 WHERE tombstone_id=$1 AND object_key=$2 AND refused_at IS NOT NULL`,
		result.TombstoneID, keyRogue).Scan(&refused); err != nil {
		t.Fatal(err)
	}
	if refused != 1 {
		t.Fatalf("unauthorized pending row not refused: count=%d", refused)
	}
	var remainingAuthorized int
	if err := pool.QueryRow(ctx, `
		SELECT count(*) FROM pending_object_deletions
		 WHERE tombstone_id=$1 AND object_key=ANY($2) AND refused_at IS NULL`,
		result.TombstoneID, []string{keyA, keyB}).Scan(&remainingAuthorized); err != nil {
		t.Fatal(err)
	}
	if remainingAuthorized != 0 {
		t.Fatalf("authorized pending rows not cleared: %d", remainingAuthorized)
	}

	// Explicit authorization API: digest not on list is refused.
	ok, err := store.TombstoneAuthorizesObjectKey(ctx, result.TombstoneID, keyRogue)
	if err != nil {
		t.Fatal(err)
	}
	if ok {
		t.Fatal("TombstoneAuthorizesObjectKey accepted a non-manifest key")
	}
	ok, err = store.TombstoneAuthorizesObjectKey(ctx, result.TombstoneID, keyA)
	if err != nil {
		t.Fatal(err)
	}
	if !ok {
		t.Fatal("TombstoneAuthorizesObjectKey rejected a manifest key")
	}

	// Missing-key delete is success (sweeper retry safety).
	if err := storage.RemoveObjects(ctx, []string{keyA, keyB, "jobs/never-existed/object"}); err != nil {
		t.Fatalf("RemoveObjects on missing keys must succeed: %v", err)
	}
}

func TestAlphaRequestRetentionSweep(t *testing.T) {
	ctx, store, pool := openAdminMutationTestStore(t)

	stalePending := uuid.New()
	staleRejected := uuid.New()
	acceptedOld := uuid.New()
	freshPending := uuid.New()

	statements := []struct {
		query string
		args  []any
	}{
		{`INSERT INTO alpha_requests (id,email,role,note,source_ip,consent_at,status,created_at)
		  VALUES ($1,'stale-pending@example.invalid','buyer','n','127.0.0.1',now()-interval '100 days','pending',now()-interval '100 days')`,
			[]any{stalePending}},
		{`INSERT INTO alpha_requests (id,email,role,note,source_ip,consent_at,status,created_at)
		  VALUES ($1,'stale-rejected@example.invalid','supplier','n','127.0.0.1',now()-interval '100 days','rejected',now()-interval '100 days')`,
			[]any{staleRejected}},
		{`INSERT INTO alpha_requests (id,email,role,note,source_ip,consent_at,status,created_at)
		  VALUES ($1,'accepted-old@example.invalid','buyer','n','127.0.0.1',now()-interval '100 days','accepted',now()-interval '100 days')`,
			[]any{acceptedOld}},
		{`INSERT INTO alpha_requests (id,email,role,note,source_ip,consent_at,status,created_at)
		  VALUES ($1,'fresh-pending@example.invalid','buyer','n','127.0.0.1',now(),'pending',now())`,
			[]any{freshPending}},
	}
	for _, statement := range statements {
		if _, err := pool.Exec(ctx, statement.query, statement.args...); err != nil {
			t.Fatalf("seed alpha retention fixture: %v\n%s", err, statement.query)
		}
	}

	// Cutoff: 90 days ago — matches default retention.
	cutoff := time.Now().UTC().Add(-90 * 24 * time.Hour)
	n, err := store.DeleteOldAlphaRequests(ctx, cutoff)
	if err != nil {
		t.Fatal(err)
	}
	if n < 2 {
		t.Fatalf("expected at least 2 non-accepted stale rows pruned, got %d", n)
	}

	var stillPending, stillAccepted, stillFresh int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM alpha_requests WHERE id=$1`, stalePending).Scan(&stillPending); err != nil {
		t.Fatal(err)
	}
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM alpha_requests WHERE id=$1`, acceptedOld).Scan(&stillAccepted); err != nil {
		t.Fatal(err)
	}
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM alpha_requests WHERE id=$1`, freshPending).Scan(&stillFresh); err != nil {
		t.Fatal(err)
	}
	if stillPending != 0 {
		t.Fatal("stale pending alpha_request survived retention sweep")
	}
	if stillAccepted != 1 {
		t.Fatal("accepted alpha_request was deleted by retention sweep")
	}
	if stillFresh != 1 {
		t.Fatal("fresh pending alpha_request was deleted by retention sweep")
	}
}

func TestAlphaRequestRejectsWithoutConsent(t *testing.T) {
	// Pure unit check: consent gate runs before any database call.
	store := &Store{}
	if err := store.CreateAlphaRequest(context.Background(), "noconsent@example.invalid", "buyer", "", "127.0.0.1", time.Time{}); !errors.Is(err, errAlphaConsentRequired) {
		t.Fatalf("CreateAlphaRequest without consent: %v", err)
	}
}
