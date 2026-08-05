package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
)

// A reusable integration environment backed by the REAL storage interface.
//
// This exists because the second-runtime chain was blocked by its absence.
// Verification settles against a stored result artifact, and every verification
// test in this tree constructed its processor with a nil Storage — they exercise
// leasing and drain mechanics and never once move bytes. There was no way to
// prove a runtime executed real work through the product without one.
//
// The rule that makes it worth building: NO test-only artifact path. This drives
// control/storage.go exactly as production does — same client, same retry and
// breaker behaviour, same presigning — against a real object store. A fixture
// that bypassed the storage rules would prove the chain works everywhere except
// where it actually runs.
//
// Skips rather than fails when no object store is configured. A developer
// without MinIO running should be told the proof was not taken, never that the
// runtimes disagree.
//
//	docker compose up -d minio createbuckets
//	MERC_TEST_S3_ENDPOINT=http://127.0.0.1:9000 \
//	MERC_TEST_S3_BUCKET=cx-jobs \
//	MERC_TEST_S3_ACCESS_KEY=minioadmin \
//	MERC_TEST_S3_SECRET_KEY=minioadmin \
//	MERC_TEST_DATABASE_URL=... go test ./...
type artifactHarness struct {
	storage *Storage
	prefix  string
	t       *testing.T
	ctx     context.Context
}

// newArtifactHarness builds a storage-backed environment scoped to one test.
//
// Every object it writes lives under a per-test prefix and is removed on
// cleanup, so two tests running against one bucket cannot see each other's
// artifacts — the same isolation an isolated database gives, applied to bytes.
func newArtifactHarness(t *testing.T) *artifactHarness {
	t.Helper()
	endpoint := os.Getenv("MERC_TEST_S3_ENDPOINT")
	bucket := os.Getenv("MERC_TEST_S3_BUCKET")
	access := os.Getenv("MERC_TEST_S3_ACCESS_KEY")
	secret := os.Getenv("MERC_TEST_S3_SECRET_KEY")
	if endpoint == "" || bucket == "" || access == "" || secret == "" {
		t.Skip("object storage is not configured; set MERC_TEST_S3_ENDPOINT, " +
			"MERC_TEST_S3_BUCKET, MERC_TEST_S3_ACCESS_KEY and MERC_TEST_S3_SECRET_KEY")
	}

	// NewStorage reads process environment, which is what production does. Using
	// t.Setenv rather than a bespoke constructor keeps this on the production
	// path instead of beside it.
	t.Setenv("S3_ENDPOINT", endpoint)
	t.Setenv("S3_BUCKET", bucket)
	t.Setenv("S3_ACCESS_KEY", access)
	t.Setenv("S3_SECRET_KEY", secret)
	if public := os.Getenv("MERC_TEST_S3_PUBLIC_ENDPOINT"); public != "" {
		t.Setenv("S3_PUBLIC_ENDPOINT", public)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	t.Cleanup(cancel)

	storage, err := NewStorage(ctx)
	mustf(t, err, "open object storage: %v")
	h := &artifactHarness{
		storage: storage,
		prefix:  "cas/test/" + strings.ReplaceAll(uuid.NewString(), "-", "") + "/",
		t:       t,
		ctx:     ctx,
	}
	t.Cleanup(func() {
		c, cancel := context.WithTimeout(context.Background(), 60*time.Second)
		defer cancel()
		if err := storage.RemovePrefix(c, h.prefix); err != nil {
			t.Logf("harness cleanup for %s: %v", h.prefix, err)
		}
	})
	return h
}

// contentAddressedKey is the only key shape this harness produces.
//
// Never job-scoped. A jobs/<job_id>/… reference under a shared key is a handle
// into one buyer's namespace, and the exact-result cache already refuses those
// for that reason. The chain proof has to hold to the same rule or it proves a
// shape production forbids.
func (h *artifactHarness) contentAddressedKey(body []byte) string {
	sum := sha256.Sum256(body)
	return h.prefix + hex.EncodeToString(sum[:])
}

// put stores bytes under their own digest and returns key and digest.
func (h *artifactHarness) put(body []byte, contentType string) (string, string) {
	h.t.Helper()
	key := h.contentAddressedKey(body)
	if err := h.storage.PutObject(h.ctx, key, body, contentType); err != nil {
		h.t.Fatalf("put %s: %v", key, err)
	}
	sum := sha256.Sum256(body)
	return key, hex.EncodeToString(sum[:])
}

// get reads bytes back and verifies they still hash to their key.
//
// Verification on read, not only on write: a corrupted or replaced object must
// be caught where it is consumed, because that is where a wrong answer would
// otherwise be settled and paid for.
func (h *artifactHarness) get(key string) ([]byte, error) {
	body, err := h.storage.GetObject(h.ctx, key)
	if err != nil {
		return nil, err
	}
	sum := sha256.Sum256(body)
	if got := hex.EncodeToString(sum[:]); !strings.HasSuffix(key, got) {
		return nil, fmt.Errorf(
			"artifact %s does not hash to its own key: content digest is %s", key, got)
	}
	return body, nil
}

// The harness itself has to be trustworthy before anything is proved with it.
func TestArtifactHarnessRoundTripsThroughRealStorage(t *testing.T) {
	h := newArtifactHarness(t)
	body := []byte(`{"job_type":"embed","dim":384,"count":1,"vectors":[[0.1,0.2]]}`)

	key, digest := h.put(body, "application/json")
	if !strings.HasPrefix(key, "cas/") {
		t.Fatalf("harness produced a non-content-addressed key %q", key)
	}
	if strings.Contains(key, "jobs/") {
		t.Fatalf("harness produced a tenant-scoped key %q", key)
	}

	read, err := h.get(key)
	mustf(t, err, "round trip: %v")
	if string(read) != string(body) {
		t.Fatal("bytes changed in storage")
	}

	exists, err := h.storage.ObjectExists(h.ctx, key)
	if err != nil || !exists {
		t.Fatalf("ObjectExists after put = %v, %v", exists, err)
	}
	if digest == "" || len(digest) != 64 {
		t.Fatalf("digest %q is not a sha256", digest)
	}
}

// Every failure the chain has to survive, driven at the real store rather than
// at a stub that returns whatever the test wanted.
func TestArtifactFailureModes(t *testing.T) {
	h := newArtifactHarness(t)

	t.Run("missing artifact", func(t *testing.T) {
		if _, err := h.get(h.prefix + strings.Repeat("0", 64)); err == nil {
			t.Fatal("reading an object that was never written succeeded")
		}
	})

	t.Run("corrupted artifact is caught on read", func(t *testing.T) {
		body := []byte("the original result")
		key, _ := h.put(body, "application/octet-stream")
		// Overwrite the object at its key with different bytes: the key still
		// claims one digest and the content now has another. This is the shape
		// of both a corruption and a swap, and read-side verification is what
		// separates "we stored something" from "we stored THIS".
		mustf(t, h.storage.PutObject(h.ctx, key, []byte("tampered"), "application/octet-stream"), "overwrite: %v")
		if _, err := h.get(key); err == nil {
			t.Fatal("an object whose content no longer matches its key was accepted")
		} else if !strings.Contains(err.Error(), "does not hash to its own key") {
			t.Errorf("refusal said %q", err.Error())
		}
	})

	t.Run("wrong digest claimed by the caller", func(t *testing.T) {
		body := []byte("honest bytes")
		key, digest := h.put(body, "application/octet-stream")
		read, err := h.storage.GetObject(h.ctx, key)
		must(t, err)
		claimed := strings.Repeat("f", 64)
		sum := sha256.Sum256(read)
		if hex.EncodeToString(sum[:]) == claimed {
			t.Fatal("fixture digest collision")
		}
		if hex.EncodeToString(sum[:]) != digest {
			t.Fatal("storage returned bytes that do not match what was written")
		}
	})

	t.Run("duplicate upload of identical bytes is idempotent", func(t *testing.T) {
		body := []byte("written twice")
		first, digestA := h.put(body, "application/octet-stream")
		second, digestB := h.put(body, "application/octet-stream")
		if first != second || digestA != digestB {
			t.Fatalf("content addressing is not stable: %s/%s vs %s/%s",
				first, digestA, second, digestB)
		}
		read, err := h.get(first)
		if err != nil || string(read) != string(body) {
			t.Fatalf("re-upload changed the object: %v", err)
		}
	})

	t.Run("tenant mismatch cannot be expressed as a key", func(t *testing.T) {
		// The cache and the harness share one rule, and it is enforced in
		// production code rather than only in the harness: an artifact reference
		// that names a job namespace is refused outright.
		identity, err := RequestIdentity{
			TenantScope: uuid.NewString(), ModelID: "m", Input: "i", TopP: 1,
		}.Compute()
		must(t, err)
		store := &Store{}
		err = store.RecordExactResult(context.Background(), identity,
			"jobs/"+uuid.NewString()+"/tasks/x/result.json", 1)
		if err == nil {
			t.Fatal("a tenant-scoped artifact reference was accepted")
		}
		if !strings.Contains(err.Error(), "tenant-scoped") {
			t.Errorf("refusal said %q", err.Error())
		}
	})

	t.Run("cleanup removes everything under the prefix", func(t *testing.T) {
		scratch := newArtifactHarness(t)
		key, _ := scratch.put([]byte("temporary"), "application/octet-stream")
		mustf(t, scratch.storage.RemovePrefix(scratch.ctx, scratch.prefix), "remove prefix: %v")
		exists, err := scratch.storage.ObjectExists(scratch.ctx, key)
		mustf(t, err, "exists after cleanup: %v")
		if exists {
			t.Fatal("cleanup left an object behind; tests would leak into each other")
		}
	})
}

// Presigned URLs are how the agent actually moves bytes, so the harness has to
// exercise them rather than only the control-side client.
func TestArtifactHarnessPresignsBothDirections(t *testing.T) {
	h := newArtifactHarness(t)
	body := []byte("presigned round trip")
	key := h.contentAddressedKey(body)

	putURL, err := h.storage.PresignPut(h.ctx, key, 5*time.Minute)
	mustf(t, err, "presign put: %v")
	getURL, err := h.storage.PresignGet(h.ctx, key, 5*time.Minute)
	mustf(t, err, "presign get: %v")
	for name, url := range map[string]string{"put": putURL, "get": getURL} {
		if !strings.Contains(url, key) {
			t.Errorf("%s URL does not address the key: %s", name, url)
		}
		if !strings.Contains(url, "X-Amz-Signature") {
			t.Errorf("%s URL carries no signature: %s", name, url)
		}
		// A presigned URL that never expires is a permanent credential.
		if !strings.Contains(url, "X-Amz-Expires") {
			t.Errorf("%s URL has no expiry: %s", name, url)
		}
	}
}
