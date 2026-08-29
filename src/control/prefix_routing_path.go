package main

import (
	"context"
	"fmt"
	"log"

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
//
// When the most recent complete on this job frozen a runtime_cell_id onto a
// task for this worker, that cell is attributed on the belief row (see
// MarkPrefixChainWarmOnCell). Absence of a cell is not an error.
func (s *Store) markWorkerWarmForJob(ctx context.Context, workerID, jobID uuid.UUID) error {
	chain, err := s.LoadJobPrefixChain(ctx, jobID)
	if err != nil {
		return fmt.Errorf("loading prefix chain for job %s: %w", jobID, err)
	}
	cellID := s.latestTaskCellForWorkerJob(ctx, workerID, jobID)
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
			if err := s.MarkPrefixWarm(ctx, workerID, *prefixID); err != nil {
				return err
			}
			if cellID != "" {
				_, _ = s.pool.Exec(ctx, `
					UPDATE worker_prefix_state SET cell_id = $3
					 WHERE worker_id = $1 AND prefix_id = $2`,
					workerID, *prefixID, cellID)
			}
			return nil
		}
		return nil
	}
	return s.MarkPrefixChainWarmOnCell(ctx, workerID, cellID, chain)
}

// latestTaskCellForWorkerJob returns the runtime_cell_id frozen onto the most
// recent task of jobID that this worker executed, or "" when unknown.
func (s *Store) latestTaskCellForWorkerJob(ctx context.Context, workerID, jobID uuid.UUID) string {
	var cell string
	err := s.pool.QueryRow(ctx, `
		SELECT COALESCE(runtime_cell_id, '')
		  FROM tasks
		 WHERE job_id = $1
		   AND (execution_worker_id = $2 OR worker_id = $2)
		   AND COALESCE(runtime_cell_id, '') <> ''
		 ORDER BY COALESCE(completed_at, claimed_at, created_at) DESC NULLS LAST
		 LIMIT 1`, jobID, workerID).Scan(&cell)
	if err != nil {
		return ""
	}
	return cell
}

// observeAndMarkPrefixForCommit applies engine-reported cache observation
// (when present) then records post-serve warmth. Order is load-bearing:
//
//  1. Correct against the *pre-serve* belief so a stale warm entry is dropped
//     before concurrent claimants re-read it.
//  2. Mark warm from this serve so the next request sees the materialisation
//     that just happened (even when the prior belief was wrong).
//
// Observation is optional: agents and engines that do not report
// cached_prompt_tokens leave the index on TTL + eviction only.
func (s *Store) observeAndMarkPrefixForCommit(ctx context.Context, workerID, jobID uuid.UUID, cached *uint64) {
	if cached != nil {
		if _, err := s.CorrectPrefixBeliefFromObservation(ctx, workerID, jobID, true, int64(*cached)); err != nil {
			log.Printf("prefix observation: worker %s job %s: %v", workerID, jobID, err)
		}
	} else {
		// Explicit no-signal accounting so the measurement arm can separate
		// "engine silent" from "engine said miss".
		if _, err := s.CorrectPrefixBeliefFromObservation(ctx, workerID, jobID, false, 0); err != nil {
			log.Printf("prefix observation (no signal): worker %s job %s: %v", workerID, jobID, err)
		}
	}
	if err := s.markWorkerWarmForJob(ctx, workerID, jobID); err != nil {
		log.Printf("prefix warmth: worker %s job %s: %v", workerID, jobID, err)
	}
}
