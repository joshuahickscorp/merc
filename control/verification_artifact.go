package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/minio/minio-go/v7"
)

const verificationArtifactAbsoluteMaxBytes int64 = maxRequestBodyBytes

var (
	ErrVerificationArtifactTooLarge = errors.New("verification artifact exceeds size limit")
	ErrVerificationArtifactChanged  = errors.New("verification artifact changed while reading")
)

type VerificationArtifactTooLargeError struct {
	Key           string
	ObservedBytes int64
	MaxBytes      int64
}

func (e *VerificationArtifactTooLargeError) Error() string {
	return fmt.Sprintf("verification artifact %q is at least %d bytes; limit is %d: %v",
		e.Key, e.ObservedBytes, e.MaxBytes, ErrVerificationArtifactTooLarge)
}

func (e *VerificationArtifactTooLargeError) Unwrap() error {
	return ErrVerificationArtifactTooLarge
}

type SealedVerificationArtifact struct {
	Key    string
	SHA256 string
	Bytes  int64
	Body   []byte
}

func sealedVerificationObjectKey(taskID uuid.UUID, attempt int16, digest string) string {
	return fmt.Sprintf("verification/%s/attempt-%d/%s.result", taskID, attempt, digest)
}

func oversizedVerificationEvidenceKey(taskID uuid.UUID, attempt int16, digest string) string {
	return fmt.Sprintf("verification/%s/attempt-%d/oversize/%s.json", taskID, attempt, digest)
}

func unavailableVerificationEvidenceKey(taskID uuid.UUID, attempt int16, digest string) string {
	return fmt.Sprintf("verification/%s/attempt-%d/unavailable/%s.json", taskID, attempt, digest)
}

func isOversizedVerificationEvidenceKey(key string) bool {
	return strings.Contains(key, "/oversize/") && strings.HasSuffix(key, ".json")
}

func isUnavailableVerificationEvidenceKey(key string) bool {
	return strings.Contains(key, "/unavailable/") && strings.HasSuffix(key, ".json")
}

func readExactObjectSizeBounded(r io.Reader, key string, declaredBytes, maxBytes int64) ([]byte, error) {
	if maxBytes < 0 || declaredBytes < 0 {
		return nil, fmt.Errorf("verification artifact %q has invalid size policy %d/%d", key, declaredBytes, maxBytes)
	}
	if declaredBytes > maxBytes {
		return nil, &VerificationArtifactTooLargeError{Key: key, ObservedBytes: declaredBytes, MaxBytes: maxBytes}
	}
	if declaredBytes == int64(^uint(0)>>1) {
		return nil, &VerificationArtifactTooLargeError{Key: key, ObservedBytes: declaredBytes, MaxBytes: maxBytes}
	}
	readLimit := declaredBytes + 1
	buf := make([]byte, int(readLimit))
	n, err := io.ReadFull(io.LimitReader(r, readLimit), buf)
	switch {
	case err == nil:
		if int64(n) > maxBytes {
			return nil, &VerificationArtifactTooLargeError{Key: key, ObservedBytes: int64(n), MaxBytes: maxBytes}
		}
		return nil, fmt.Errorf("%w: object %q grew beyond HEAD size %d", ErrVerificationArtifactChanged, key, declaredBytes)
	case errors.Is(err, io.EOF), errors.Is(err, io.ErrUnexpectedEOF):
		if int64(n) != declaredBytes {
			return nil, fmt.Errorf("%w: object %q HEAD reported %d bytes but GET returned %d",
				ErrVerificationArtifactChanged, key, declaredBytes, n)
		}
		return buf[:n], nil
	default:
		return nil, err
	}
}

func (s *Storage) statVerificationObject(ctx context.Context, key string) (int64, error) {
	var size int64
	err := s.withRetry(ctx, func() (bool, error) {
		info, err := s.internal.StatObject(ctx, s.bucket, key, minio.StatObjectOptions{})
		if err == nil {
			size = info.Size
			return false, nil
		}
		if minio.ToErrorResponse(err).Code == "NoSuchKey" {
			return false, fmt.Errorf("stat verification object %q: %w", key, err)
		}
		return true, fmt.Errorf("stat verification object %q: %w", key, err)
	})
	return size, err
}

func (s *Storage) readVerificationObjectBounded(ctx context.Context, key string, maxBytes int64, expectedBytes *int64) ([]byte, error) {
	if strings.TrimSpace(key) == "" {
		return nil, errors.New("verification artifact key is required")
	}
	if maxBytes <= 0 || maxBytes > verificationArtifactAbsoluteMaxBytes {
		return nil, fmt.Errorf("verification artifact %q has invalid limit %d", key, maxBytes)
	}
	started := time.Now()
	declared, err := s.statVerificationObject(ctx, key)
	if err != nil {
		return nil, err
	}
	if declared > maxBytes {
		return nil, &VerificationArtifactTooLargeError{Key: key, ObservedBytes: declared, MaxBytes: maxBytes}
	}
	if expectedBytes != nil && declared != *expectedBytes {
		return nil, fmt.Errorf("%w: object %q is %d bytes, pinned authority says %d",
			ErrVerificationArtifactChanged, key, declared, *expectedBytes)
	}
	if cached, ok := cachedVerificationBody(ctx, key); ok {
		if int64(len(cached)) != declared {
			return nil, fmt.Errorf("%w: cached object %q is %d bytes, HEAD now says %d",
				ErrVerificationArtifactChanged, key, len(cached), declared)
		}
		return cached, nil
	}
	if err := reserveVerificationMemory(ctx, declared+1); err != nil {
		return nil, fmt.Errorf("read verification object %q (%d bytes): %w", key, declared, err)
	}
	r, err := s.GetObjectReader(ctx, key)
	if err != nil {
		return nil, err
	}
	defer r.Close()
	body, err := readExactObjectSizeBounded(r, key, declared, maxBytes)
	if err != nil {
		return nil, err
	}
	observeTransfer("get", len(body), time.Since(started))
	cacheVerificationBody(ctx, key, body)
	return body, nil
}

const verificationDigestReadBufferBytes int64 = 32 << 10

func (s *Storage) verifyVerificationObjectDigest(ctx context.Context, key string, expectedBytes, maxBytes int64, expectedSHA string) error {
	if expectedBytes < 0 || expectedBytes > maxBytes {
		return &VerificationArtifactTooLargeError{Key: key, ObservedBytes: expectedBytes, MaxBytes: maxBytes}
	}
	declared, err := s.statVerificationObject(ctx, key)
	if err != nil {
		return err
	}
	if declared != expectedBytes {
		return fmt.Errorf("%w: object %q is %d bytes after PUT, expected %d",
			ErrVerificationArtifactChanged, key, declared, expectedBytes)
	}
	if err := reserveVerificationMemory(ctx, verificationDigestReadBufferBytes); err != nil {
		return fmt.Errorf("verify sealed object %q: %w", key, err)
	}
	r, err := s.GetObjectReader(ctx, key)
	if err != nil {
		return err
	}
	defer r.Close()
	started := time.Now()
	h := sha256.New()
	buf := make([]byte, verificationDigestReadBufferBytes)
	n, err := io.CopyBuffer(h, io.LimitReader(r, expectedBytes+1), buf)
	if err != nil {
		return err
	}
	if n != expectedBytes {
		return fmt.Errorf("%w: object %q returned %d bytes after PUT, expected %d",
			ErrVerificationArtifactChanged, key, n, expectedBytes)
	}
	if hex.EncodeToString(h.Sum(nil)) != expectedSHA {
		return fmt.Errorf("%w: object %q digest disagrees after PUT", ErrVerificationArtifactChanged, key)
	}
	observeTransfer("get", int(n), time.Since(started))
	return nil
}

func (s *Storage) sealVerificationArtifactWithLimit(ctx context.Context, taskID uuid.UUID, attempt int16, stagingKey string, maxBytes int64, probe recoveryBoundaryProbe) (SealedVerificationArtifact, error) {
	if taskID == uuid.Nil {
		return SealedVerificationArtifact{}, fmt.Errorf("sealing verification artifact: task id is required")
	}
	if attempt < 0 {
		return SealedVerificationArtifact{}, fmt.Errorf("sealing verification artifact: invalid attempt %d", attempt)
	}
	if stagingKey == "" {
		return SealedVerificationArtifact{}, fmt.Errorf("sealing verification artifact: staging key is required")
	}

	body, err := s.readVerificationObjectBounded(ctx, stagingKey, maxBytes, nil)
	if err != nil {
		return SealedVerificationArtifact{}, fmt.Errorf("reading verification staging object: %w", err)
	}
	reachRecoveryBoundary(ctx, probe, BoundaryVerifyAfterStagingRead)
	sum := sha256.Sum256(body)
	digest := hex.EncodeToString(sum[:])
	key := sealedVerificationObjectKey(taskID, attempt, digest)
	if err := s.PutObject(ctx, key, body, "application/octet-stream"); err != nil {
		return SealedVerificationArtifact{}, fmt.Errorf("writing sealed verification object: %w", err)
	}
	reachRecoveryBoundary(ctx, probe, BoundaryVerifyAfterSealedPut)
	expected := int64(len(body))
	if err := s.verifyVerificationObjectDigest(ctx, key, expected, maxBytes, digest); err != nil {
		return SealedVerificationArtifact{}, fmt.Errorf("reading sealed verification object: %w", err)
	}
	reachRecoveryBoundary(ctx, probe, BoundaryVerifyAfterSealedReadback)

	return SealedVerificationArtifact{
		Key: key, SHA256: digest, Bytes: int64(len(body)), Body: body,
	}, nil
}

func (s *Storage) sealOversizedVerificationEvidenceWithProbe(ctx context.Context, taskID uuid.UUID, attempt int16, sizeErr *VerificationArtifactTooLargeError, probe recoveryBoundaryProbe) (SealedVerificationArtifact, error) {
	if taskID == uuid.Nil || attempt < 0 || sizeErr == nil || sizeErr.MaxBytes <= 0 || sizeErr.ObservedBytes <= sizeErr.MaxBytes {
		return SealedVerificationArtifact{}, errors.New("invalid oversized verification evidence")
	}
	keySum := sha256.Sum256([]byte(sizeErr.Key))
	evidence, err := json.Marshal(struct {
		Version              int    `json:"version"`
		Reason               string `json:"reason"`
		StagingKeySHA256     string `json:"staging_key_sha256"`
		ObservedBytesAtLeast int64  `json:"observed_bytes_at_least"`
		MaxBytes             int64  `json:"max_bytes"`
	}{
		Version: 1, Reason: "verification_artifact_too_large",
		StagingKeySHA256:     hex.EncodeToString(keySum[:]),
		ObservedBytesAtLeast: sizeErr.ObservedBytes, MaxBytes: sizeErr.MaxBytes,
	})
	if err != nil {
		return SealedVerificationArtifact{}, err
	}
	sum := sha256.Sum256(evidence)
	digest := hex.EncodeToString(sum[:])
	key := oversizedVerificationEvidenceKey(taskID, attempt, digest)
	if err := s.PutObject(ctx, key, evidence, "application/json"); err != nil {
		return SealedVerificationArtifact{}, fmt.Errorf("writing oversized verification evidence: %w", err)
	}
	reachRecoveryBoundary(ctx, probe, BoundaryVerifyAfterSealedPut)
	expected := int64(len(evidence))
	if err := s.verifyVerificationObjectDigest(ctx, key, expected, verificationArtifactAbsoluteMaxBytes, digest); err != nil {
		return SealedVerificationArtifact{}, fmt.Errorf("reading oversized verification evidence: %w", err)
	}
	reachRecoveryBoundary(ctx, probe, BoundaryVerifyAfterSealedReadback)
	return SealedVerificationArtifact{Key: key, SHA256: digest, Bytes: expected, Body: evidence}, nil
}

func (s *Storage) sealUnavailableVerificationEvidenceWithProbe(
	ctx context.Context,
	taskID uuid.UUID,
	attempt int16,
	stagingKey, reason string,
	leaseAttempts int,
	probe recoveryBoundaryProbe,
) (SealedVerificationArtifact, error) {
	if taskID == uuid.Nil || attempt < 0 || strings.TrimSpace(stagingKey) == "" ||
		(reason != "missing" && reason != "changed") || leaseAttempts <= 0 {
		return SealedVerificationArtifact{}, errors.New("invalid unavailable verification evidence")
	}
	keySum := sha256.Sum256([]byte(stagingKey))
	evidence, err := json.Marshal(struct {
		Version          int    `json:"version"`
		Reason           string `json:"reason"`
		StagingKeySHA256 string `json:"staging_key_sha256"`
		LeaseAttempts    int    `json:"lease_attempts"`
	}{
		Version: 1, Reason: "verification_artifact_" + reason,
		StagingKeySHA256: hex.EncodeToString(keySum[:]), LeaseAttempts: leaseAttempts,
	})
	if err != nil {
		return SealedVerificationArtifact{}, err
	}
	sum := sha256.Sum256(evidence)
	digest := hex.EncodeToString(sum[:])
	key := unavailableVerificationEvidenceKey(taskID, attempt, digest)
	if err := s.PutObject(ctx, key, evidence, "application/json"); err != nil {
		return SealedVerificationArtifact{}, fmt.Errorf("writing unavailable verification evidence: %w", err)
	}
	reachRecoveryBoundary(ctx, probe, BoundaryVerifyAfterSealedPut)
	expected := int64(len(evidence))
	if err := s.verifyVerificationObjectDigest(ctx, key, expected, verificationArtifactAbsoluteMaxBytes, digest); err != nil {
		return SealedVerificationArtifact{}, fmt.Errorf("reading unavailable verification evidence: %w", err)
	}
	reachRecoveryBoundary(ctx, probe, BoundaryVerifyAfterSealedReadback)
	return SealedVerificationArtifact{Key: key, SHA256: digest, Bytes: expected, Body: evidence}, nil
}

func (s *Storage) ReadSealedVerificationArtifact(ctx context.Context, artifact VerificationArtifact) ([]byte, error) {
	return s.readSealedVerificationArtifactWithLimit(ctx, artifact, verificationArtifactAbsoluteMaxBytes)
}

func (s *Storage) readSealedVerificationArtifactWithLimit(ctx context.Context, artifact VerificationArtifact, maxBytes int64) ([]byte, error) {
	if artifact.Bytes < 0 || artifact.Bytes > maxBytes {
		return nil, &VerificationArtifactTooLargeError{Key: artifact.Key, ObservedBytes: artifact.Bytes, MaxBytes: maxBytes}
	}
	body, err := s.readVerificationObjectBounded(ctx, artifact.Key, maxBytes, &artifact.Bytes)
	if err != nil {
		return nil, err
	}
	sum := sha256.Sum256(body)
	if int64(len(body)) != artifact.Bytes || hex.EncodeToString(sum[:]) != artifact.SHA256 {
		return nil, fmt.Errorf("%w: sealed verification artifact %q no longer matches pinned authority", ErrVerificationArtifactChanged, artifact.Key)
	}
	return body, nil
}
