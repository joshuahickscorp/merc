package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"sort"
	"strings"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

// Exact deterministic result reuse.
//
// Prefix reuse removes prefill. This removes everything: when the complete
// effective request identity matches, a prior result is not an approximation of
// the answer, it IS the answer, and no model needs to run.
//
// Eligibility is narrow on purpose. Anything that could make two runs differ --
// a temperature above zero, a different seed, a different model revision, a
// different tool schema -- changes the identity and produces a miss. A
// near-miss must never silently stand in for fresh inference; that is a
// different product (SEMANTIC_REUSE) requiring buyer opt-in, and it is not
// this.

var requestIdentityPattern = regexp.MustCompile(`^req_[0-9a-f]{64}$`)

// RequestIdentity is every input that can change the output.
type RequestIdentity struct {
	ModelID       string
	ModelRevision string
	Adapter       string
	Input         string
	Tools         string
	Schema        string
	// Sampling must be deterministic for the request to be cacheable at all.
	Temperature float64
	TopP        float64
	Seed        int64
	MaxTokens   int
	Policy      string
}

// errNonDeterministic marks a request that cannot be cached because two runs
// need not agree.
var errNonDeterministic = errors.New("request sampling is not deterministic; exact reuse is not eligible")

// Deterministic reports whether this request would produce the same output
// every time. Temperature and top-p are the gate: anything that samples cannot
// be replayed from cache without changing what the buyer would have received.
func (r RequestIdentity) Deterministic() bool {
	return r.Temperature == 0 && (r.TopP == 0 || r.TopP == 1)
}

// Compute derives the cache key.
//
// Field names are included in the hashed payload so that moving a value from
// one field to another cannot collide, and the encoding is canonical JSON with
// sorted keys so that two identical requests always hash identically.
func (r RequestIdentity) Compute() (string, error) {
	if !r.Deterministic() {
		return "", errNonDeterministic
	}
	if strings.TrimSpace(r.ModelID) == "" || strings.TrimSpace(r.Input) == "" {
		return "", fmt.Errorf("request identity requires at least a model and an input")
	}
	fields := map[string]any{
		"model": r.ModelID, "revision": r.ModelRevision, "adapter": r.Adapter,
		"input": r.Input, "tools": r.Tools, "schema": r.Schema,
		"temperature": r.Temperature, "top_p": r.TopP, "seed": r.Seed,
		"max_tokens": r.MaxTokens, "policy": r.Policy,
	}
	keys := make([]string, 0, len(fields))
	for k := range fields {
		keys = append(keys, k)
	}
	sort.Strings(keys)

	h := sha256.New()
	h.Write([]byte("merc-request-identity-v1\x00"))
	for _, k := range keys {
		blob, err := json.Marshal(fields[k])
		if err != nil {
			return "", err
		}
		h.Write([]byte(k))
		h.Write([]byte{0})
		h.Write(blob)
		h.Write([]byte{0})
	}
	return "req_" + hex.EncodeToString(h.Sum(nil)), nil
}

// ValidRequestIdentity checks shape before it reaches SQL.
func ValidRequestIdentity(id string) bool {
	return requestIdentityPattern.MatchString(id)
}

// ExactCacheHit is a reusable prior result.
type ExactCacheHit struct {
	ResultRef    string
	OutputTokens int64
	Hits         int64
}

// LookupExactResult returns a prior result for an identical request.
func (s *Store) LookupExactResult(ctx context.Context, identity string) (ExactCacheHit, bool, error) {
	if !ValidRequestIdentity(identity) {
		return ExactCacheHit{}, false, nil
	}
	var hit ExactCacheHit
	err := s.pool.QueryRow(ctx, `
		UPDATE exact_result_cache
		   SET hits = hits + 1, last_hit_at = now()
		 WHERE request_identity = $1
		 RETURNING result_ref, output_tokens, hits`, identity).
		Scan(&hit.ResultRef, &hit.OutputTokens, &hit.Hits)
	if errors.Is(err, pgx.ErrNoRows) {
		return ExactCacheHit{}, false, nil
	}
	if err != nil {
		return ExactCacheHit{}, false, err
	}
	return hit, true, nil
}

// tenantScopedRefPattern matches artifact paths that belong to one buyer's job.
//
// The scheduler writes results to jobs/<job_id>/tasks/<task_id>/... which is
// scoped to the buyer that submitted the job. The exact-result cache is keyed
// on request identity ONLY -- deliberately, because deterministic inference on
// identical input yields identical output regardless of who asked. That makes
// the key tenant-neutral while the path is not.
//
// Storing a tenant-scoped path under a tenant-neutral key means a later buyer
// gets a reference into someone else's job namespace: either the read is
// refused and the cache silently never works, or it succeeds and crosses a
// tenant boundary. Neither is acceptable, so the cache accepts only
// content-addressed references.
var tenantScopedRefPattern = regexp.MustCompile(`^jobs/`)

// RecordExactResult stores a completed deterministic result for reuse.
func (s *Store) RecordExactResult(ctx context.Context, identity, resultRef string, outputTokens int64) error {
	if !ValidRequestIdentity(identity) {
		return fmt.Errorf("refusing to cache under a malformed request identity")
	}
	if strings.TrimSpace(resultRef) == "" {
		return fmt.Errorf("refusing to cache an empty result reference")
	}
	if tenantScopedRefPattern.MatchString(resultRef) {
		return fmt.Errorf(
			"refusing to cache tenant-scoped reference %q: the exact-result cache is keyed on "+
				"request identity alone and is shared across buyers, so it may only hold "+
				"content-addressed artifacts", resultRef)
	}
	_, err := s.pool.Exec(ctx, `
		INSERT INTO exact_result_cache (request_identity, result_ref, output_tokens)
		VALUES ($1,$2,$3)
		ON CONFLICT (request_identity) DO NOTHING`, identity, resultRef, outputTokens)
	return err
}

// --------------------------------------------------------------------------
// In-flight coalescing

// ClaimInflightLeader tries to become the executing request for an identity.
//
// Returns true when this caller should execute. False means an identical
// deterministic request is already running and this one should wait for it: one
// execution, many receipts, and the cost allocated by policy rather than
// charged N times for work done once.
func (s *Store) ClaimInflightLeader(ctx context.Context, identity string, jobID uuid.UUID) (bool, error) {
	if !ValidRequestIdentity(identity) {
		return true, nil // uncacheable: always execute
	}
	ct, err := s.pool.Exec(ctx, `
		INSERT INTO inflight_requests (request_identity, leader_job_id)
		VALUES ($1,$2) ON CONFLICT (request_identity) DO NOTHING`, identity, jobID)
	if err != nil {
		return false, err
	}
	if ct.RowsAffected() == 1 {
		return true, nil
	}
	if _, err := s.pool.Exec(ctx, `
		UPDATE inflight_requests SET followers = followers + 1
		 WHERE request_identity = $1`, identity); err != nil {
		return false, err
	}
	return false, nil
}

// ReleaseInflight clears the in-flight marker and reports how many followers
// waited on this execution.
func (s *Store) ReleaseInflight(ctx context.Context, identity string) (int64, error) {
	if !ValidRequestIdentity(identity) {
		return 0, nil
	}
	var followers int64
	err := s.pool.QueryRow(ctx, `
		DELETE FROM inflight_requests WHERE request_identity = $1
		 RETURNING followers`, identity).Scan(&followers)
	if errors.Is(err, pgx.ErrNoRows) {
		return 0, nil
	}
	return followers, err
}
