package main

import (
	"context"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"fmt"
	"math"
	"regexp"
	"sort"
	"strings"
	"sync/atomic"
	"time"

	"github.com/google/uuid"
)

// Aggregate observation counters. Never labelled by prefix_id (that would be a
// cross-tenant oracle); counts only, for the measurement arm.
var (
	prefixObsConfirmed   atomic.Int64 // believed warm, engine reported cached_tokens > 0
	prefixObsInvalidated atomic.Int64 // believed warm, engine reported cached_tokens == 0
	prefixObsNoSignal    atomic.Int64 // engine did not report the signal
	prefixObsColdServe   atomic.Int64 // believed cold (or unknown); no correction applied
)

// PrefixObservationStats returns process-local observation counters since start.
func PrefixObservationStats() (confirmed, invalidated, noSignal, coldServe int64) {
	return prefixObsConfirmed.Load(), prefixObsInvalidated.Load(),
		prefixObsNoSignal.Load(), prefixObsColdServe.Load()
}

// Prefix identity and cache-aware routing.
//
// Measured on an M3 Ultra with Llama-3.2-1B-4bit: independent prompts cap near
// 6.5k tok/s, while sharing a 160-token system prefix across a batch of 128
// reaches ~20k tok/s. The prefill is simply not recomputed per stream. That is
// the largest single throughput lever found, and it only pays when work sharing
// a prefix reaches a worker that already holds that prefix warm.
//
// prefix_id is a ROUTING HINT and nothing else. It never selects a model, a
// runtime, a price or an authorisation, so a forged or colliding value costs at
// most a cache miss. That is why it can be accepted from a buyer at all.
//
// # Belief model (observed, never assumed)
//
// Merc does not own the engine's KV cache and cannot see inside it. The table
// worker_prefix_state is therefore a *model* of residency, not a mirror of it.
//
// What the index believes
//
//	A (worker, prefix_id, depth) row with last_seen_warm within prefixWarmTTL
//	means "this worker recently served a request that materialised this prefix,
//	so its engine *probably* still holds the matching KV".
//
// How the belief is formed
//
//	On task completion Merc records every job_prefix_chain node for the serving
//	worker (markWorkerWarmForJob). That is inference from "we just ran this
//	prompt there", not a read of the engine.
//
// How the belief can be wrong
//
//   - Engine eviction under memory pressure, continuous batching, or a restart
//     that Merc has not yet heard about.
//   - Multi-cell workers: the row is keyed by worker_id (and cell_id when
//     known); a cell that did not serve the prefix may still look warm if an
//     earlier serve was attributed only to the worker.
//   - Clock skew / delayed heartbeats: TTL expiry is the floor, not a
//     guarantee that the engine still holds the block.
//
// How it learns it was wrong
//
//	When the engine reports an observed cache signal (OpenAI-shaped
//	usage.prompt_tokens_details.cached_tokens; vLLM populates this),
//	CorrectPrefixBeliefFromObservation prefers that signal over the model:
//	believed-warm + observed cached_tokens == 0 invalidates the matching
//	chain nodes immediately rather than waiting out the TTL. An index that
//	is never corrected is a guess with a data structure around it; an index
//	corrected by observation is defensible.
//
// What it does then
//
//	Invalidate drops the stale rows so the next claim ranks the worker cold
//	for that chain. After the current request finishes, markWorkerWarmForJob
//	re-records warmth from the serve that just ran (which *did* materialise
//	the prefix). Concurrent claimants therefore stop piling onto a worker
//	whose cache was already empty, while the post-serve mark keeps the model
//	honest for the next request.
//
// Engines that expose no signal (llama.cpp / MLX / Candle on Metal typically
// do not) leave the index on TTL + value-ranked eviction only. That limitation
// is labelled in the measurement artifact; it is not silently papered over.

const (
	prefixIDPrefix = "pfx_"
	// 128 bits of a SHA-256 is ample to make accidental collision irrelevant
	// while keeping the identifier short enough to log and index.
	prefixIDHexLen = 32
	// How long a worker is considered to still hold a prefix. Deliberately
	// shorter than a model warmth window: KV cache is evicted far sooner than
	// model weights.
	prefixWarmTTL = 90 * time.Second
)

var prefixIDPattern = regexp.MustCompile(`^pfx_[0-9a-f]{32}$`)

// ComputePrefixID derives a stable identity for a shared prompt prefix.
//
// Domain-separated so a prefix id can never be confused with, or collide
// across, any other hash in the system.
func ComputePrefixID(prefix string) string {
	if strings.TrimSpace(prefix) == "" {
		return ""
	}
	sum := sha256.Sum256(append([]byte("merc-prompt-prefix-v1\x00"), []byte(prefix)...))
	return prefixIDPrefix + hex.EncodeToString(sum[:])[:prefixIDHexLen]
}

// ValidPrefixID reports whether a client-supplied value is well formed. An
// invalid value is dropped rather than rejected: routing is an optimisation and
// must never fail a job that is otherwise admissible.
func ValidPrefixID(id string) bool {
	return prefixIDPattern.MatchString(id)
}

// NormalisePrefixID accepts either a raw prefix or an already-computed id and
// returns a storable id, or "" when there is nothing usable.
func NormalisePrefixID(supplied, rawPrefix string) string {
	supplied = strings.TrimSpace(supplied)
	if ValidPrefixID(supplied) {
		return supplied
	}
	return ComputePrefixID(rawPrefix)
}

// MarkPrefixWarm records that a worker now holds a prefix. Called when a task
// carrying a prefix completes, which is the point at which the KV for that
// prefix is known to have been materialised on that worker.
func (s *Store) MarkPrefixWarm(ctx context.Context, workerID uuid.UUID, prefixID string) error {
	if !ValidPrefixID(prefixID) {
		return nil // nothing to record; never an error for the caller
	}
	_, err := s.pool.Exec(ctx, `
		INSERT INTO worker_prefix_state (worker_id, prefix_id, last_seen_warm, hits)
		VALUES ($1, $2, now(), 1)
		ON CONFLICT (worker_id, prefix_id) DO UPDATE
		   SET last_seen_warm = now(), hits = worker_prefix_state.hits + 1`,
		workerID, prefixID)
	if err != nil {
		return fmt.Errorf("marking prefix %s warm on worker %s: %w", prefixID, workerID, err)
	}
	return nil
}

// PrefixIsWarm reports whether a worker still holds a prefix inside the TTL.
func (s *Store) PrefixIsWarm(ctx context.Context, workerID uuid.UUID, prefixID string) (bool, error) {
	if !ValidPrefixID(prefixID) {
		return false, nil
	}
	var warm bool
	err := s.pool.QueryRow(ctx, `
		SELECT EXISTS(
		  SELECT 1 FROM worker_prefix_state
		   WHERE worker_id = $1 AND prefix_id = $2
		     AND last_seen_warm > now() - $3::interval)`,
		workerID, prefixID, prefixWarmTTL.String()).Scan(&warm)
	return warm, err
}

// PrefixWarmWorkers lists workers currently holding a prefix, warmest first.
// Used by operator reporting and by tests; the scheduler does the same test
// inline so routing stays a single query.
func (s *Store) PrefixWarmWorkers(ctx context.Context, prefixID string) ([]uuid.UUID, error) {
	if !ValidPrefixID(prefixID) {
		return nil, nil
	}
	rows, err := s.pool.Query(ctx, `
		SELECT worker_id FROM worker_prefix_state
		 WHERE prefix_id = $1 AND last_seen_warm > now() - $2::interval
		 ORDER BY last_seen_warm DESC`, prefixID, prefixWarmTTL.String())
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []uuid.UUID
	for rows.Next() {
		var id uuid.UUID
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		out = append(out, id)
	}
	return out, rows.Err()
}

// SweepStalePrefixState bounds the table. A prefix a worker has not served in
// well over the TTL is not coming back warm, and the row is pure noise.
func (s *Store) SweepStalePrefixState(ctx context.Context) (int64, error) {
	tag, err := s.pool.Exec(ctx, `
		DELETE FROM worker_prefix_state
		 WHERE last_seen_warm < now() - $1::interval`, (20 * prefixWarmTTL).String())
	if err != nil {
		return 0, err
	}
	return tag.RowsAffected(), nil
}

// --------------------------------------------------------------------------
// Prefix trie: deepest-valid-prefix reuse.
//
// Exact-prefix matching only pays off when two requests share their entire
// prompt. Measured at the batch-256 operating point, prefill is 76% of wall
// time, so almost all of that work is re-paid per request under exact matching.
//
// A request registers one id per nested depth. Two requests sharing a system
// prompt but diverging afterwards still match at the shared depth, and routing
// prefers the worker holding the deepest match.

// prefixChainDepths are the token boundaries at which reuse is tracked.
//
// Powers of two: fine enough that a shared system prompt lands on a boundary,
// coarse enough that the state table stays small. A per-token trie would match
// marginally deeper and cost far more to store and evict.
var prefixChainDepths = []int{32, 64, 128, 256, 512, 1024, 2048}

// PrefixChainEntry is one node of a request's prefix chain.
type PrefixChainEntry struct {
	Depth    int
	PrefixID string
}

// ComputePrefixChain derives the nested prefix ids for a tokenized prompt.
//
// Identity is computed over the token sequence rather than the raw text, so
// two prompts that tokenize identically share a cache entry and two that merely
// look similar do not.
func ComputePrefixChain(tokens []int) []PrefixChainEntry {
	var out []PrefixChainEntry
	for _, d := range prefixChainDepths {
		if d > len(tokens) {
			break
		}
		out = append(out, PrefixChainEntry{Depth: d, PrefixID: prefixIDForTokens(tokens[:d])})
	}
	return out
}

// prefixIDForTokens hashes a token slice with the same domain separation used
// for text prefixes, so the two id spaces cannot collide.
func prefixIDForTokens(tokens []int) string {
	h := sha256.New()
	h.Write([]byte("merc-prompt-prefix-tokens-v1\x00"))
	var buf [8]byte
	for _, t := range tokens {
		binary.LittleEndian.PutUint64(buf[:], uint64(t))
		h.Write(buf[:])
	}
	return prefixIDPrefix + hex.EncodeToString(h.Sum(nil))[:prefixIDHexLen]
}

// RecordJobPrefixChain stores the chain a job can match at.
func (s *Store) RecordJobPrefixChain(ctx context.Context, jobID uuid.UUID, chain []PrefixChainEntry) error {
	for _, e := range chain {
		if !ValidPrefixID(e.PrefixID) || e.Depth <= 0 {
			continue
		}
		if _, err := s.pool.Exec(ctx, `
			INSERT INTO job_prefix_chain (job_id, depth, prefix_id)
			VALUES ($1,$2,$3)
			ON CONFLICT (job_id, depth) DO UPDATE SET prefix_id = EXCLUDED.prefix_id`,
			jobID, e.Depth, e.PrefixID); err != nil {
			return fmt.Errorf("recording prefix chain for job %s: %w", jobID, err)
		}
	}
	return nil
}

// MarkPrefixChainWarm records every depth a worker now holds.
func (s *Store) MarkPrefixChainWarm(ctx context.Context, workerID uuid.UUID, chain []PrefixChainEntry) error {
	for _, e := range chain {
		if !ValidPrefixID(e.PrefixID) {
			continue
		}
		if _, err := s.pool.Exec(ctx, `
			INSERT INTO worker_prefix_state (worker_id, prefix_id, depth, last_seen_warm, hits)
			VALUES ($1,$2,$3,now(),1)
			ON CONFLICT (worker_id, prefix_id) DO UPDATE
			   SET last_seen_warm = now(), hits = worker_prefix_state.hits + 1,
			       depth = GREATEST(worker_prefix_state.depth, EXCLUDED.depth)`,
			workerID, e.PrefixID, e.Depth); err != nil {
			return fmt.Errorf("marking prefix chain warm on worker %s: %w", workerID, err)
		}
	}
	return nil
}

// DeepestWarmPrefix returns how many prompt tokens a worker can skip for a job,
// i.e. the deepest chain node that worker still holds. Zero means no reuse.
func (s *Store) DeepestWarmPrefix(ctx context.Context, workerID, jobID uuid.UUID) (int, error) {
	var depth int
	err := s.pool.QueryRow(ctx, `
		SELECT COALESCE(MAX(c.depth), 0)
		  FROM job_prefix_chain c
		  JOIN worker_prefix_state w
		    ON w.prefix_id = c.prefix_id AND w.worker_id = $1
		 WHERE c.job_id = $2
		   AND w.last_seen_warm > now() - $3::interval`,
		workerID, jobID, prefixWarmTTL.String()).Scan(&depth)
	return depth, err
}

// --------------------------------------------------------------------------
// Value-aware cache eviction.
//
// The intuitive policy -- keep the deepest prefixes because they cost most to
// recompute -- is wrong here, and measurement says so.
//
// Prefill cost was measured across 64/256/1024-token prompts at batch 64 and
// scales as O(n^1.01): essentially LINEAR. At 1B scale the per-layer MLP work
// dominates quadratic attention over this range. KV bytes are also linear in
// depth. So in
//
//	value = reuse_probability x recompute_cost_avoided / bytes
//
// the depth term cancels, and ranking collapses to reuse probability. A deep
// node is not intrinsically more valuable per byte than a shallow one; it is
// only more valuable if it is actually reused more often.
//
// prefillCostExponent is kept explicit so that if a larger model or longer
// contexts push prefill superlinear, re-measuring and changing this constant is
// all that is required.
const prefillCostExponent = 1.01

// kvBytesPerToken for Llama-3.2-1B: 16 layers x 2 (K+V) x 8 KV heads x 64 dim
// x 2 bytes.
const kvBytesPerToken = 16 * 2 * 8 * 64 * 2

// prefixRoutingStateBudgetBytes bounds Merc's advisory belief about one
// worker's KV residency. It is not a claim about physical VRAM and never
// causes a worker-side eviction; crossing it only removes low-value routing
// hints, whose safe failure mode is a cold recompute.
const prefixRoutingStateBudgetBytes int64 = 64 << 20

// PrefixCacheValue scores a cache node for eviction. Higher survives.
//
// hits is observed reuse; age is time since last use. Reuse probability is
// estimated as hits decayed by age -- a node used twenty times a minute ago is
// a better bet than one used twice an hour ago, and neither plain LRU (recency
// only) nor plain LFU (frequency only) expresses that.
func PrefixCacheValue(hits int64, age time.Duration, depth int) float64 {
	if depth <= 0 || hits < 0 {
		return 0
	}
	// Decay with a half-life of the warmth TTL: beyond it, a node is unlikely
	// to still be resident anyway.
	halfLives := age.Seconds() / prefixWarmTTL.Seconds()
	reuseProbability := float64(hits) * math.Pow(0.5, halfLives)

	recompute := math.Pow(float64(depth), prefillCostExponent)
	bytes := float64(depth) * kvBytesPerToken
	return reuseProbability * recompute / bytes
}

// EvictPrefixCacheToBudget removes the lowest-value nodes until the worker's
// cache fits budgetBytes. Returns how many rows were evicted.
//
// Eviction is advisory bookkeeping: it records what merc believes a worker
// holds. It never deletes anything a worker needs to serve correctly, because a
// stale-miss simply costs a recompute.
func (s *Store) EvictPrefixCacheToBudget(ctx context.Context, workerID uuid.UUID, budgetBytes int64) (int64, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT prefix_id, depth, hits,
		       EXTRACT(EPOCH FROM (now() - last_seen_warm))::float8
		  FROM worker_prefix_state WHERE worker_id = $1`, workerID)
	if err != nil {
		return 0, err
	}
	type node struct {
		id    string
		bytes int64
		value float64
	}
	var nodes []node
	for rows.Next() {
		var id string
		var depth int
		var hits int64
		var ageSec float64
		if err := rows.Scan(&id, &depth, &hits, &ageSec); err != nil {
			rows.Close()
			return 0, err
		}
		if depth <= 0 {
			depth = 1
		}
		nodes = append(nodes, node{
			id:    id,
			bytes: int64(depth) * kvBytesPerToken,
			value: PrefixCacheValue(hits, time.Duration(ageSec*float64(time.Second)), depth),
		})
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return 0, err
	}

	var total int64
	for _, n := range nodes {
		total += n.bytes
	}
	if total <= budgetBytes {
		return 0, nil
	}

	// Lowest value first.
	sort.Slice(nodes, func(i, j int) bool { return nodes[i].value < nodes[j].value })

	var evicted int64
	for _, n := range nodes {
		if total <= budgetBytes {
			break
		}
		ct, err := s.pool.Exec(ctx,
			`DELETE FROM worker_prefix_state WHERE worker_id=$1 AND prefix_id=$2`,
			workerID, n.id)
		if err != nil {
			return evicted, err
		}
		if ct.RowsAffected() > 0 {
			evicted++
			total -= n.bytes
		}
	}
	return evicted, nil
}

// EnforcePrefixRoutingStateBudgets applies the fixed advisory budget to every
// worker with recorded warm-prefix state. It is called by Workers rather than
// a request path so an attacker cannot turn cache-hint eviction into a
// per-request database amplification vector.
func (s *Store) EnforcePrefixRoutingStateBudgets(ctx context.Context) (int64, error) {
	rows, err := s.pool.Query(ctx, `SELECT DISTINCT worker_id FROM worker_prefix_state ORDER BY worker_id`)
	if err != nil {
		return 0, err
	}
	defer rows.Close()
	var workers []uuid.UUID
	for rows.Next() {
		var workerID uuid.UUID
		if err := rows.Scan(&workerID); err != nil {
			return 0, err
		}
		workers = append(workers, workerID)
	}
	if err := rows.Err(); err != nil {
		return 0, err
	}
	var evicted int64
	for _, workerID := range workers {
		n, err := s.EvictPrefixCacheToBudget(ctx, workerID, prefixRoutingStateBudgetBytes)
		if err != nil {
			return evicted, err
		}
		evicted += n
	}
	return evicted, nil
}

// --------------------------------------------------------------------------
// Observation correction.
//
// Prefers the engine's reported cache-hit signal over Merc's belief. See the
// package comment above for the full belief model.

// PrefixObservationAction names what CorrectPrefixBeliefFromObservation did.
type PrefixObservationAction string

const (
	// PrefixObsConfirmed: index said warm and the engine reported cached tokens.
	PrefixObsConfirmed PrefixObservationAction = "confirmed"
	// PrefixObsInvalidated: index said warm but the engine reported a miss;
	// matching chain nodes were dropped so the next claim ranks cold.
	PrefixObsInvalidated PrefixObservationAction = "invalidated"
	// PrefixObsNoSignal: caller had no engine signal; belief left untouched.
	PrefixObsNoSignal PrefixObservationAction = "no_signal"
	// PrefixObsColdServe: index already said cold; nothing to correct.
	PrefixObsColdServe PrefixObservationAction = "cold_serve"
)

// PrefixObservationOutcome is the durable result of one correction attempt.
type PrefixObservationOutcome struct {
	Action          PrefixObservationAction `json:"action"`
	BelievedDepth   int                     `json:"believed_depth"`
	CachedTokens    int64                   `json:"cached_tokens"`
	InvalidatedRows int64                   `json:"invalidated_rows,omitempty"`
}

// CorrectPrefixBeliefFromObservation reconciles worker_prefix_state for jobID
// on workerID against an engine-reported cached token count.
//
// hasSignal must be true only when the engine actually exposed the field
// (vLLM's usage.prompt_tokens_details.cached_tokens). A missing signal must
// not be treated as a miss — that would thrash the index on every local-engine
// completion.
//
// cachedTokens is the observed hit size. Zero with hasSignal means a true miss
// against whatever the index believed; any positive value confirms.
func (s *Store) CorrectPrefixBeliefFromObservation(
	ctx context.Context, workerID, jobID uuid.UUID, hasSignal bool, cachedTokens int64,
) (PrefixObservationOutcome, error) {
	believed, err := s.DeepestWarmPrefix(ctx, workerID, jobID)
	if err != nil {
		return PrefixObservationOutcome{}, err
	}
	out := PrefixObservationOutcome{BelievedDepth: believed, CachedTokens: cachedTokens}

	if !hasSignal {
		out.Action = PrefixObsNoSignal
		prefixObsNoSignal.Add(1)
		return out, nil
	}
	if believed <= 0 {
		out.Action = PrefixObsColdServe
		prefixObsColdServe.Add(1)
		return out, nil
	}
	if cachedTokens > 0 {
		// Confirm: refresh last_seen_warm on matching nodes so a real hit
		// extends the TTL rather than only the next mark-warm path doing so.
		if err := s.confirmPrefixBelief(ctx, workerID, jobID); err != nil {
			return out, err
		}
		out.Action = PrefixObsConfirmed
		prefixObsConfirmed.Add(1)
		return out, nil
	}

	// Observed miss against a warm belief: drop the chain nodes. Leaving them
	// would keep routing later requests onto a worker that already proved empty.
	n, err := s.invalidatePrefixBelief(ctx, workerID, jobID)
	if err != nil {
		return out, err
	}
	out.Action = PrefixObsInvalidated
	out.InvalidatedRows = n
	prefixObsInvalidated.Add(1)
	return out, nil
}

// confirmPrefixBelief refreshes last_seen_warm for every chain node this worker
// still holds for the job, without inventing nodes it never had.
func (s *Store) confirmPrefixBelief(ctx context.Context, workerID, jobID uuid.UUID) error {
	_, err := s.pool.Exec(ctx, `
		UPDATE worker_prefix_state w
		   SET last_seen_warm = now(),
		       hits = hits + 1
		  FROM job_prefix_chain c
		 WHERE w.worker_id = $1
		   AND w.prefix_id = c.prefix_id
		   AND c.job_id = $2
		   AND w.last_seen_warm > now() - $3::interval`,
		workerID, jobID, prefixWarmTTL.String())
	return err
}

// invalidatePrefixBelief deletes every chain node for jobID on workerID.
// Returns how many rows were removed.
func (s *Store) invalidatePrefixBelief(ctx context.Context, workerID, jobID uuid.UUID) (int64, error) {
	tag, err := s.pool.Exec(ctx, `
		DELETE FROM worker_prefix_state w
		 WHERE w.worker_id = $1
		   AND w.prefix_id IN (
		     SELECT c.prefix_id FROM job_prefix_chain c WHERE c.job_id = $2
		   )`, workerID, jobID)
	if err != nil {
		return 0, err
	}
	// Also drop a legacy shallow jobs.prefix_id row if present and not already
	// covered by the chain delete.
	var legacy *string
	if err := s.pool.QueryRow(ctx,
		`SELECT prefix_id FROM jobs WHERE id = $1`, jobID,
	).Scan(&legacy); err != nil {
		return tag.RowsAffected(), err
	}
	if legacy != nil && ValidPrefixID(*legacy) {
		extra, err := s.pool.Exec(ctx,
			`DELETE FROM worker_prefix_state WHERE worker_id=$1 AND prefix_id=$2`,
			workerID, *legacy)
		if err != nil {
			return tag.RowsAffected(), err
		}
		return tag.RowsAffected() + extra.RowsAffected(), nil
	}
	return tag.RowsAffected(), nil
}

// MarkPrefixChainWarmOnCell records warmth attributed to a specific runtime
// cell. cellID may be empty when the completing task predates cell freeze; the
// row remains worker-scoped in that case. cell_id is advisory metadata on the
// belief row — claim ranking still keys by worker_id because the claiming
// worker is the unit of placement, and a multi-cell worker that served on any
// admitted cell is a better bet than a cold peer of the same cost class.
func (s *Store) MarkPrefixChainWarmOnCell(ctx context.Context, workerID uuid.UUID, cellID string, chain []PrefixChainEntry) error {
	if err := s.MarkPrefixChainWarm(ctx, workerID, chain); err != nil {
		return err
	}
	if strings.TrimSpace(cellID) == "" {
		return nil
	}
	for _, e := range chain {
		if !ValidPrefixID(e.PrefixID) {
			continue
		}
		if _, err := s.pool.Exec(ctx, `
			UPDATE worker_prefix_state
			   SET cell_id = $3
			 WHERE worker_id = $1 AND prefix_id = $2`,
			workerID, e.PrefixID, cellID); err != nil {
			// cell_id column may not yet exist on a mid-migration store; the
			// warmth row itself is already durable. Surface the error so a
			// misapplied schema is visible in tests.
			return fmt.Errorf("recording cell_id on prefix belief: %w", err)
		}
	}
	return nil
}
