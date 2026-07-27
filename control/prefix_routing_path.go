package main

import (
	"context"
	"fmt"

	"github.com/google/uuid"
)

// Live-path wiring for warm-prefix routing.
//
// prefix_routing.go owns identity, state and pure ranking. This file is the
// only place the request path and scheduler should touch those primitives, so
// the HTTP handlers and claim SQL stay free of token-identity details.

// prefixSampleBytes is how many leading input bytes we fingerprint for a
// job's prefix chain. 2048 chain depth × 4 bytes/surrogate-token is the
// largest window ComputePrefixChain will ever hash; reading more cannot
// deepen the chain.
const prefixSampleBytes = 2048 * 4

// prefixTokensFromBytes maps raw input bytes to a stable surrogate token
// sequence for ComputePrefixChain.
//
// The control plane has no model tokenizer. A surrogate only has to be
// stable and domain-separated: identical leading bytes produce identical
// chain nodes, and a forged or approximate tokenisation can only cost a
// cache miss (the safe direction for a routing hint). Four-byte little-endian
// windows match the existing estimateTokens ~4 bytes/token heuristic so a
// 32-token chain node covers roughly the first 128 bytes of input.
func prefixTokensFromBytes(b []byte) []int {
	if len(b) == 0 {
		return nil
	}
	if len(b) > prefixSampleBytes {
		b = b[:prefixSampleBytes]
	}
	n := (len(b) + 3) / 4
	out := make([]int, n)
	for i := 0; i < n; i++ {
		var t uint32
		for j := 0; j < 4; j++ {
			idx := i*4 + j
			if idx < len(b) {
				t |= uint32(b[idx]) << (8 * j)
			}
		}
		// Never emit 0: a run of NUL bytes would otherwise collapse every
		// depth onto the same degenerate id space.
		if t == 0 {
			t = 1
		}
		out[i] = int(t)
	}
	return out
}

// prefixChainFromInputBytes derives the nested prefix chain for a job's
// leading input. Empty or short input yields a nil chain (no routing hint).
func prefixChainFromInputBytes(b []byte) []PrefixChainEntry {
	return ComputePrefixChain(prefixTokensFromBytes(b))
}

// LoadJobPrefixChain returns the chain recorded at submit, ordered shallow
// to deep. Missing chain is not an error: cold jobs simply have none.
func (s *Store) LoadJobPrefixChain(ctx context.Context, jobID uuid.UUID) ([]PrefixChainEntry, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT depth, prefix_id FROM job_prefix_chain
		 WHERE job_id = $1
		 ORDER BY depth ASC`, jobID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []PrefixChainEntry
	for rows.Next() {
		var e PrefixChainEntry
		if err := rows.Scan(&e.Depth, &e.PrefixID); err != nil {
			return nil, err
		}
		out = append(out, e)
	}
	return out, rows.Err()
}

// markWorkerWarmForJob records every chain node this worker materialised by
// serving jobID. Failures are returned to the caller so a best-effort path
// can log and continue: warmth is advisory and must never fail a commit.
func (s *Store) markWorkerWarmForJob(ctx context.Context, workerID, jobID uuid.UUID) error {
	chain, err := s.LoadJobPrefixChain(ctx, jobID)
	if err != nil {
		return fmt.Errorf("loading prefix chain for job %s: %w", jobID, err)
	}
	if len(chain) == 0 {
		// Legacy path: a job may carry only the shallow jobs.prefix_id
		// without a trie chain. Still record that single node if present.
		var prefixID *string
		if err := s.pool.QueryRow(ctx,
			`SELECT prefix_id FROM jobs WHERE id = $1`, jobID,
		).Scan(&prefixID); err != nil {
			return err
		}
		if prefixID != nil && *prefixID != "" {
			return s.MarkPrefixWarm(ctx, workerID, *prefixID)
		}
		return nil
	}
	return s.MarkPrefixChainWarm(ctx, workerID, chain)
}
