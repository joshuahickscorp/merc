package main

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"log"
	"net/url"
	"os"
	"strings"
	"sync/atomic"
	"time"

	"github.com/minio/minio-go/v7"
	"github.com/minio/minio-go/v7/pkg/credentials"
)

type Storage struct {
	internal *minio.Client // control-side PutObject/GetObject
	public   *minio.Client // signs presigned URLs the agent reaches
	bucket   string
	breaker  *storeBreaker // fail-fast guard for a sustained store outage
}

func NewStorage(ctx context.Context) (*Storage, error) {
	endpoint := os.Getenv("S3_ENDPOINT")
	bucket := os.Getenv("S3_BUCKET")
	access := os.Getenv("S3_ACCESS_KEY")
	secret := os.Getenv("S3_SECRET_KEY")
	region := os.Getenv("S3_REGION")
	if region == "" {
		region = "us-east-1"
	}
	if endpoint == "" || bucket == "" || access == "" || secret == "" {
		return nil, fmt.Errorf("object storage is mandatory but unconfigured: " +
			"set S3_ENDPOINT, S3_BUCKET, S3_ACCESS_KEY, S3_SECRET_KEY")
	}
	publicEndpoint := os.Getenv("S3_PUBLIC_ENDPOINT")
	if publicEndpoint == "" {
		publicEndpoint = endpoint
	}

	internal, err := newMinio(endpoint, access, secret, region)
	if err != nil {
		return nil, fmt.Errorf("building internal S3 client: %w", err)
	}
	public, err := newMinio(publicEndpoint, access, secret, region)
	if err != nil {
		return nil, fmt.Errorf("building public S3 client: %w", err)
	}

	st := &Storage{internal: internal, public: public, bucket: bucket,
		breaker: newStoreBreaker(5, 10*time.Second)} // open after 5 fully-failed calls; cool down 10s

	cctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	exists, err := internal.BucketExists(cctx, bucket)
	if err != nil {
		return nil, fmt.Errorf("object store unreachable at %s: %w", endpoint, err)
	}
	if !exists {
		if err := internal.MakeBucket(cctx, bucket, minio.MakeBucketOptions{Region: region}); err != nil {
			return nil, fmt.Errorf("creating bucket %q: %w", bucket, err)
		}
		log.Printf("storage: created bucket %q at %s", bucket, endpoint)
	} else {
		log.Printf("storage: bucket %q present at %s (public presign endpoint %s)", bucket, endpoint, publicEndpoint)
	}
	return st, nil
}

func newMinio(endpoint, access, secret, region string) (*minio.Client, error) {
	secure := false
	host := endpoint
	if strings.Contains(endpoint, "://") {
		u, err := url.Parse(endpoint)
		if err != nil {
			return nil, fmt.Errorf("invalid S3 endpoint %q: %w", endpoint, err)
		}
		secure = u.Scheme == "https"
		host = u.Host
	}
	return minio.New(host, &minio.Options{
		Creds:  credentials.NewStaticV4(access, secret, ""),
		Secure: secure,
		Region: region,
	})
}

func (s *Storage) PresignGet(ctx context.Context, key string, ttl time.Duration) (string, error) {
	u, err := s.public.PresignedGetObject(ctx, s.bucket, key, ttl, url.Values{})
	if err != nil {
		return "", fmt.Errorf("presign GET %q: %w", key, err)
	}
	return u.String(), nil
}

func (s *Storage) PresignPut(ctx context.Context, key string, ttl time.Duration) (string, error) {
	u, err := s.public.PresignedPutObject(ctx, s.bucket, key, ttl)
	if err != nil {
		return "", fmt.Errorf("presign PUT %q: %w", key, err)
	}
	return u.String(), nil
}

func (s *Storage) ObjectExists(ctx context.Context, key string) (bool, error) {
	var exists bool
	err := s.withRetry(ctx, func() (bool, error) {
		_, serr := s.internal.StatObject(ctx, s.bucket, key, minio.StatObjectOptions{})
		if serr == nil {
			exists = true
			return false, nil
		}
		if minio.ToErrorResponse(serr).Code == "NoSuchKey" {
			exists = false
			return false, nil // definitive: the store answered "not there"
		}
		return true, fmt.Errorf("stat object %q: %w", key, serr)
	})
	return exists, err
}

var errStoreCircuitOpen = fmt.Errorf("object store circuit open (recent sustained failures); failing fast")

type storeBreaker struct {
	failures  atomic.Int32
	openUntil atomic.Int64 // unix nanos; > now = open
	threshold int32
	cooldown  time.Duration
}

func newStoreBreaker(threshold int32, cooldown time.Duration) *storeBreaker {
	return &storeBreaker{threshold: threshold, cooldown: cooldown}
}

func (b *storeBreaker) allow(now time.Time) bool { return now.UnixNano() >= b.openUntil.Load() }

func (b *storeBreaker) record(now time.Time, healthy bool) {
	if healthy {
		b.failures.Store(0)
		b.openUntil.Store(0)
		return
	}
	if b.failures.Add(1) >= b.threshold {
		b.openUntil.Store(now.Add(b.cooldown).UnixNano())
		b.failures.Store(0)
	}
}

func (s *Storage) withRetry(ctx context.Context, op func() (retry bool, err error)) error {
	if !s.breaker.allow(time.Now()) {
		return errStoreCircuitOpen
	}
	var lastErr error
	for attempt := 0; attempt < 3; attempt++ {
		if attempt > 0 {
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-time.After(time.Duration(attempt) * 200 * time.Millisecond):
			}
		}
		retry, err := op()
		if !retry {
			s.breaker.record(time.Now(), true) // store responded (success or definitive) -> healthy
			return err
		}
		lastErr = err
	}
	s.breaker.record(time.Now(), false) // exhausted transient retries -> a real store failure
	return lastErr
}

func (s *Storage) PutObject(ctx context.Context, key string, data []byte, contentType string) error {
	if contentType == "" {
		contentType = "application/octet-stream"
	}
	return s.withRetry(ctx, func() (bool, error) {
		start := time.Now()
		if _, err := s.internal.PutObject(ctx, s.bucket, key, bytes.NewReader(data), int64(len(data)),
			minio.PutObjectOptions{ContentType: contentType}); err != nil {
			return true, fmt.Errorf("put object %q: %w", key, err)
		}
		observeTransfer("put", len(data), time.Since(start))
		return false, nil
	})
}

func (s *Storage) GetObject(ctx context.Context, key string) ([]byte, error) {
	var data []byte
	err := s.withRetry(ctx, func() (bool, error) {
		start := time.Now()
		obj, err := s.internal.GetObject(ctx, s.bucket, key, minio.GetObjectOptions{})
		if err != nil {
			return true, err
		}
		d, rerr := io.ReadAll(obj)
		obj.Close()
		if rerr == nil {
			data = d
			observeTransfer("get", len(d), time.Since(start))
			return false, nil
		}
		if minio.ToErrorResponse(rerr).Code == "NoSuchKey" {
			return false, fmt.Errorf("get object %q: %w", key, rerr)
		}
		return true, fmt.Errorf("get object %q: %w", key, rerr)
	})
	return data, err
}

// boundedStreamingUploadPartBytes caps the buffer minio-go allocates per part
// for an upload whose total size is unknown.
//
// With no PartSize set, OptimalPartInfo picks a part size from the maximum
// possible object size, which for an unknown-size stream means the control
// plane reserves a very large buffer per concurrent upload.  Job inputs arrive
// as unknown-size streams, so that is a memory ceiling set by the largest
// object anyone could theoretically send rather than by what is being sent.
const boundedStreamingUploadPartBytes uint64 = 16 << 20

func (s *Storage) PutObjectStream(ctx context.Context, key string, r io.Reader, contentType string) error {
	if contentType == "" {
		contentType = "application/octet-stream"
	}
	_, err := s.internal.PutObject(ctx, s.bucket, key, r, -1, minio.PutObjectOptions{
		ContentType: contentType,
		PartSize:    boundedStreamingUploadPartBytes,
	})
	if err != nil {
		return fmt.Errorf("put object stream %q: %w", key, err)
	}
	return nil
}

func (s *Storage) PutObjectReadSeeker(ctx context.Context, key string, r io.ReadSeeker, size int64, contentType string) error {
	if size < 0 {
		return fmt.Errorf("put object %q: negative size %d", key, size)
	}
	if contentType == "" {
		contentType = "application/octet-stream"
	}
	return s.withRetry(ctx, func() (bool, error) {
		if _, err := r.Seek(0, io.SeekStart); err != nil {
			return false, fmt.Errorf("rewind object %q: %w", key, err)
		}
		started := time.Now()
		if _, err := s.internal.PutObject(ctx, s.bucket, key, r, size,
			minio.PutObjectOptions{ContentType: contentType}); err != nil {
			return true, fmt.Errorf("put object %q: %w", key, err)
		}
		observeTransfer("put", int(size), time.Since(started))
		return false, nil
	})
}

func (s *Storage) GetObjectReader(ctx context.Context, key string) (io.ReadCloser, error) {
	if !s.breaker.allow(time.Now()) {
		return nil, errStoreCircuitOpen
	}
	var lastErr error
	for attempt := 0; attempt < 3; attempt++ {
		if attempt > 0 {
			select {
			case <-ctx.Done():
				return nil, ctx.Err()
			case <-time.After(time.Duration(attempt) * 200 * time.Millisecond):
			}
		}
		obj, err := s.internal.GetObject(ctx, s.bucket, key, minio.GetObjectOptions{})
		if err != nil {
			lastErr = fmt.Errorf("get object reader %q: %w", key, err)
			continue
		}
		if _, serr := obj.Stat(); serr != nil {
			obj.Close()
			if minio.ToErrorResponse(serr).Code == "NoSuchKey" {
				return nil, fmt.Errorf("get object reader %q: %w", key, serr)
			}
			lastErr = fmt.Errorf("get object reader %q: %w", key, serr)
			continue
		}
		s.breaker.record(time.Now(), true)
		return obj, nil
	}
	s.breaker.record(time.Now(), false)
	return nil, lastErr
}

// objectDeleteMissingCodes are treated as success: a crash-safe deletion
// sweeper retries, so a key that is already gone must not fail the batch.
var objectDeleteMissingCodes = map[string]struct{}{
	"NoSuchKey":     {},
	"NoSuchVersion": {},
	"NotFound":      {},
}

// RemoveObjects bulk-deletes object keys from the configured bucket.
// Missing keys succeed so the pending-deletion sweeper can retry safely.
// Uses the same circuit breaker and withRetry discipline as the other
// mutating Storage methods.
func (s *Storage) RemoveObjects(ctx context.Context, keys []string) error {
	unique := uniqueNonEmptyKeys(keys)
	if len(unique) == 0 {
		return nil
	}
	return s.withRetry(ctx, func() (bool, error) {
		objectsCh := make(chan minio.ObjectInfo, len(unique))
		for _, key := range unique {
			objectsCh <- minio.ObjectInfo{Key: key}
		}
		close(objectsCh)

		var first error
		errorCh := s.internal.RemoveObjects(ctx, s.bucket, objectsCh, minio.RemoveObjectsOptions{})
		for remErr := range errorCh {
			if remErr.Err == nil {
				continue
			}
			code := minio.ToErrorResponse(remErr.Err).Code
			if _, missing := objectDeleteMissingCodes[code]; missing {
				continue
			}
			if first == nil {
				first = fmt.Errorf("remove object %q: %w", remErr.ObjectName, remErr.Err)
			}
		}
		if first != nil {
			return true, first
		}
		return false, nil
	})
}

func uniqueNonEmptyKeys(keys []string) []string {
	if len(keys) == 0 {
		return nil
	}
	seen := make(map[string]struct{}, len(keys))
	out := make([]string, 0, len(keys))
	for _, key := range keys {
		key = strings.TrimSpace(key)
		if key == "" {
			continue
		}
		if _, ok := seen[key]; ok {
			continue
		}
		seen[key] = struct{}{}
		out = append(out, key)
	}
	return out
}
