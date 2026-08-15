package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

// realtimeOfferLivenessWindow is the eligibility freshness bound for realtime
// offers. It must match every SQL predicate of the form
// `last_seen_at > now() - interval '45 seconds'` (realtime_store authorize,
// market liquidity, service leases). Widening it to make ingest look better
// is forbidden: a dead device must leave the eligible set within this window.
const realtimeOfferLivenessWindow = 45 * time.Second

// Default coalesced-liveness knobs. 2ms is far below the 45s window and is
// safe because last_seen_at is the device's (clamped) observation time, not
// flush-time now() — so flush delay cannot extend apparent liveness.
//
// 50ms was the first-cut default. On a 1vCPU/961MB PROXY it capped ingest at
// ~conc/55ms (~260 hb/s at conc=16) — an 11× loss vs the one-statement path.
// 2ms still cannot widen the 45s contract and is what the droplet-curve
// measurement used for the coalesced win. Override with
// MERC_LIVENESS_BATCH_FLUSH_MS.
const (
	defaultLivenessBatchMaxSize       = 1000
	defaultLivenessBatchFlushInterval = 2 * time.Millisecond
)

var (
	// errStaleHeartbeatObservation is returned when a device reports an
	// observation older than the eligibility window. Writing it would either
	// be a no-op for eligibility or (if we stamped server now instead) extend
	// liveness past the contract. Reject rather than write.
	errStaleHeartbeatObservation = errors.New("heartbeat observation is older than the liveness window")
)

// resolveHeartbeatObservation converts a device-supplied observation (or the
// authenticated receipt time when the device omitted one) into a durable
// last_seen_at candidate.
//
// Rules (contract):
//  1. Never trust an unauthenticated claim — the caller must already hold a
//     verified WorkerAuth (HTTP path: authWorker + X-Worker-Token).
//  2. Clamp to serverNow: a device must never extend its own liveness by
//     reporting a future clock.
//  3. Reject observations older than the liveness window rather than writing
//     them (or floor-stamping them to now(), which would revive a dead device).
func resolveHeartbeatObservation(observedAtUnixMs *int64, serverNow time.Time) (time.Time, error) {
	serverNow = serverNow.UTC()
	var observed time.Time
	if observedAtUnixMs == nil {
		// Backward compatible: stamp the authenticated receipt time. This is
		// still earlier than (or equal to) flush time, so batching cannot
		// extend apparent liveness past today's contract.
		observed = serverNow
	} else {
		observed = time.UnixMilli(*observedAtUnixMs).UTC()
	}
	if observed.After(serverNow) {
		observed = serverNow
	}
	if serverNow.Sub(observed) > realtimeOfferLivenessWindow {
		return time.Time{}, errStaleHeartbeatObservation
	}
	return observed, nil
}

// livenessBatchConfig controls coalesced durable heartbeats. Zero values mean
// defaults. Enabled defaults to true unless MERC_LIVENESS_BATCH=0.
type livenessBatchConfig struct {
	Enabled       bool
	MaxBatch      int
	FlushInterval time.Duration
}

func livenessBatchConfigFromEnv() livenessBatchConfig {
	cfg := livenessBatchConfig{
		Enabled:       true,
		MaxBatch:      defaultLivenessBatchMaxSize,
		FlushInterval: defaultLivenessBatchFlushInterval,
	}
	switch strings.TrimSpace(os.Getenv("MERC_LIVENESS_BATCH")) {
	case "0", "false", "off", "disabled":
		cfg.Enabled = false
	case "1", "true", "on", "enabled":
		cfg.Enabled = true
	case "":
		// Production default is on. Tests default off so a flush wait does
		// not inflate every HeartbeatRealtimeOffer in the suite; opt in with
		// SetLivenessBatchConfigForTest or MERC_LIVENESS_BATCH=1.
		if testing.Testing() {
			cfg.Enabled = false
		}
	}
	if v := strings.TrimSpace(os.Getenv("MERC_LIVENESS_BATCH_MAX")); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			cfg.MaxBatch = n
		}
	}
	if v := strings.TrimSpace(os.Getenv("MERC_LIVENESS_BATCH_FLUSH_MS")); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n >= 0 {
			cfg.FlushInterval = time.Duration(n) * time.Millisecond
		}
	}
	return cfg
}

type livenessHeartbeatItem struct {
	worker     WorkerAuth
	hb         RealtimeOfferHeartbeat
	observedAt time.Time
	done       chan error
}

// livenessCoalescer folds many authenticated heartbeats into few multi-row
// statements. Callers block until their row is durable (or the batch fails).
type livenessCoalescer struct {
	store *Store
	cfg   livenessBatchConfig

	mu      sync.Mutex
	pending []*livenessHeartbeatItem
	timer   *time.Timer
	closed  bool
	// flushGen increments on every flush so waiters can detect completion
	// without the flusher holding their done channels under the lock longer
	// than necessary.
	flushing bool

	flushCount atomic.Int64
	itemCount  atomic.Int64

	// detachedQ carries fire-and-forget batches (flag ON) to ONE serialized
	// flusher. Blocking submitters keep their original inline flush and never
	// touch it.
	detachedQ    chan []*livenessHeartbeatItem
	detachedOnce sync.Once
}

// detachedLivenessQueueDepth bounds queued detached batches. A full queue BLOCKS
// the submitter rather than dropping: the durable row carries offer state that
// authorize reads (status, warmth, capacity — see the write classification), so
// a dropped batch could strand a DRAINING offer as ACTIVE. Blocking degrades to
// the backpressure callers already had before the write was detached.
const detachedLivenessQueueDepth = 64

// One flusher, not a pool. Detaching the caller removed the merc-agent's
// implicit write serialization: it used to await each heartbeat's durable commit
// before sending the next, so an ACTIVE was always durable before the
// DRAINING/FAILED that followed it. Concurrent flushers would let that earlier
// ACTIVE commit LAST and strand a dead worker as selectable — and the terminal
// report is single-shot (agent/src/vllm.rs report_terminal_state sends one
// heartbeat, then the container dies), so nothing would ever heal the row.
//
// A single FIFO consumer restores the ordering guarantee structurally. It is not
// a throughput concern: each queue entry is a whole coalesced batch of up to
// MaxBatch rows, so one in-flight statement still absorbs a large fleet.
func newLivenessCoalescer(store *Store, cfg livenessBatchConfig) *livenessCoalescer {
	return &livenessCoalescer{
		store:     store,
		cfg:       cfg,
		detachedQ: make(chan []*livenessHeartbeatItem, detachedLivenessQueueDepth),
	}
}

// startDetachedFlusher launches the single consumer on first detached use.
func (c *livenessCoalescer) startDetachedFlusher() {
	c.detachedOnce.Do(func() {
		go func() {
			for batch := range c.detachedQ {
				c.flush(batch)
			}
		}()
	})
}

func (c *livenessCoalescer) close() {
	if c == nil {
		return
	}
	c.mu.Lock()
	c.closed = true
	if c.timer != nil {
		c.timer.Stop()
		c.timer = nil
	}
	batch := c.pending
	c.pending = nil
	c.mu.Unlock()
	// Flush outside the lock so close does not drop durability or deadlock
	// against in-flight submitters waiting on done.
	c.flush(batch)
}

// submit enqueues one heartbeat and waits until it is durable or ctx ends.
func (c *livenessCoalescer) submit(ctx context.Context, worker WorkerAuth, hb RealtimeOfferHeartbeat, observedAt time.Time) error {
	return c.enqueue(ctx, worker, hb, observedAt, true)
}

// submitDetached enqueues one heartbeat for monitoring persistence WITHOUT
// waiting for durability. Used only when the live index is the liveness
// authority (flag ON): the answer to "is this offer alive?" has already been
// recorded in-process, so the durable write is history, not the fast path.
func (c *livenessCoalescer) submitDetached(worker WorkerAuth, hb RealtimeOfferHeartbeat, observedAt time.Time) {
	_ = c.enqueue(context.Background(), worker, hb, observedAt, false)
}

// enqueue is the shared body. wait=true is the original blocking contract,
// byte-identical in behaviour to what callers had before; wait=false returns as
// soon as the row is queued and dispatches flushes onto bounded background
// goroutines instead of the caller's stack.
func (c *livenessCoalescer) enqueue(ctx context.Context, worker WorkerAuth, hb RealtimeOfferHeartbeat, observedAt time.Time, wait bool) error {
	item := &livenessHeartbeatItem{
		worker:     worker,
		hb:         hb,
		observedAt: observedAt,
	}
	if wait {
		item.done = make(chan error, 1)
	}
	c.mu.Lock()
	if c.closed {
		c.mu.Unlock()
		return errors.New("liveness coalescer closed")
	}
	c.pending = append(c.pending, item)
	n := len(c.pending)
	maxBatch := c.cfg.MaxBatch
	if maxBatch < 1 {
		maxBatch = defaultLivenessBatchMaxSize
	}
	if n >= maxBatch {
		batch := c.pending
		c.pending = nil
		if c.timer != nil {
			c.timer.Stop()
			c.timer = nil
		}
		c.dispatchLocked(batch, wait)
	} else {
		if c.timer == nil {
			interval := c.cfg.FlushInterval
			if interval < 0 {
				interval = defaultLivenessBatchFlushInterval
			}
			if interval == 0 {
				// Interval 0: flush as soon as the lock is released if nothing
				// else is already mid-flush. Concurrent submitters that race
				// into pending before we re-lock will ride along.
				batch := c.pending
				c.pending = nil
				c.dispatchLocked(batch, wait)
			} else {
				c.timer = time.AfterFunc(interval, c.timerFlush)
				c.mu.Unlock()
			}
		} else {
			c.mu.Unlock()
		}
	}

	if !wait {
		return nil
	}
	select {
	case err := <-item.done:
		return err
	case <-ctx.Done():
		return ctx.Err()
	}
}

// dispatch runs a batch inline for blocking submitters (unchanged behaviour), or
// hands it to the single FIFO flusher for detached ones. Batches reach the queue
// in the order they were taken from pending, and one consumer commits them in
// that order, so a later heartbeat's status can never be overwritten by an
// earlier one. No batch is ever dropped: it carries offer state (status, warmth,
// capacity) that authorize reads, not just a liveness stamp.
// It MUST be called with c.mu held, and it releases the lock. Enqueueing under
// the same lock that took the batch out of pending is what makes the order
// total: otherwise two goroutines could take batch N and N+1 and then race to
// the channel, delivering them inverted and reintroducing exactly the overwrite
// this design exists to prevent. Blocking on a full queue while holding the lock
// is deliberate backpressure and cannot deadlock — the flusher never takes c.mu.
func (c *livenessCoalescer) dispatchLocked(batch []*livenessHeartbeatItem, inline bool) {
	if len(batch) == 0 {
		c.mu.Unlock()
		return
	}
	if inline {
		c.mu.Unlock()
		c.flush(batch)
		return
	}
	c.startDetachedFlusher()
	c.detachedQ <- batch
	c.mu.Unlock()
}

func (c *livenessCoalescer) timerFlush() {
	c.mu.Lock()
	c.timer = nil
	if len(c.pending) == 0 {
		c.mu.Unlock()
		return
	}
	batch := c.pending
	c.pending = nil
	// Blocking submitters are waiting on this batch, so flush it on the timer
	// goroutine exactly as before. A batch with no waiters is detached work and
	// must not stall the runtime timer on a slow PostgreSQL.
	c.dispatchLocked(batch, batchHasWaiter(batch))
}

func batchHasWaiter(batch []*livenessHeartbeatItem) bool {
	for _, item := range batch {
		if item.done != nil {
			return true
		}
	}
	return false
}

func (c *livenessCoalescer) flush(batch []*livenessHeartbeatItem) {
	if len(batch) == 0 {
		return
	}
	c.flushCount.Add(1)
	c.itemCount.Add(int64(len(batch)))
	// Use a detached context bound by a generous timeout: waiters already hold
	// their own ctx, but a cancelled waiter must not abort siblings mid-flush.
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	errs := c.store.flushLivenessHeartbeatBatch(ctx, batch)
	for i, item := range batch {
		err := errs[i]
		select {
		case item.done <- err:
		default:
		}
	}
}

// flushLivenessHeartbeatBatch applies many heartbeats in one transaction:
// one set-based UPDATE … FROM unnest(...) and one multi-row sample INSERT.
// errs is aligned with batch; missing rows yield errNotFound for that index.
func (s *Store) flushLivenessHeartbeatBatch(ctx context.Context, batch []*livenessHeartbeatItem) []error {
	errs := make([]error, len(batch))
	if len(batch) == 0 {
		return errs
	}
	// Single-item path keeps the original statement shape for EXPLAIN parity
	// and avoids unnest planning overhead when the coalescer is disabled or
	// a lone heartbeat arrives.
	if len(batch) == 1 {
		errs[0] = s.heartbeatRealtimeOfferOne(ctx, batch[0].worker, batch[0].hb, batch[0].observedAt)
		if errs[0] == nil {
			s.recordDurableHeartbeat(batch[0].worker, batch[0].hb, time.Now())
		}
		return errs
	}

	// Collapse repeats of the same offer to the LATEST observation before they
	// reach unnest. `UPDATE … FROM unnest(...)` with two source rows for one
	// target picks an unspecified row, so without this an ACTIVE heartbeat could
	// overwrite the FAILED one that followed it — a dead worker left routable.
	// Ties fall back to arrival order, which is receipt order for a given agent.
	// The key matches the UPDATE's WHERE clause, supplier included: keying on
	// (worker, profile) alone would let a wrong-supplier item evict the
	// legitimate one for the same offer and fail them both.
	winner := make(map[string]int, len(batch))
	for i, item := range batch {
		key := updatedRowKey(item.worker.WorkerID, item.worker.SupplierID, item.hb.RuntimeProfileID)
		if prev, ok := winner[key]; ok && batch[prev].observedAt.After(item.observedAt) {
			continue
		}
		winner[key] = i
	}
	order := make([]int, 0, len(winner))
	for _, i := range winner {
		order = append(order, i)
	}
	sort.Ints(order)

	workerIDs := make([]uuid.UUID, len(order))
	supplierIDs := make([]uuid.UUID, len(order))
	profileIDs := make([]string, len(order))
	warmths := make([]string, len(order))
	availables := make([]int32, len(order))
	statuses := make([]string, len(order))
	observeds := make([]time.Time, len(order))
	for n, i := range order {
		item := batch[i]
		workerIDs[n] = item.worker.WorkerID
		supplierIDs[n] = item.worker.SupplierID
		profileIDs[n] = item.hb.RuntimeProfileID
		warmths[n] = item.hb.Warmth
		availables[n] = int32(item.hb.AvailableSequences)
		statuses[n] = item.hb.Status
		observeds[n] = item.observedAt.UTC()
	}

	tx, err := s.pool.Begin(ctx)
	if err != nil {
		for i := range errs {
			errs[i] = err
		}
		return errs
	}
	defer tx.Rollback(ctx)

	rows, err := tx.Query(ctx, `
		UPDATE realtime_worker_offers AS o
		   SET warmth = i.warmth,
		       available_sequences = GREATEST(0, LEAST(i.available_sequences::int, o.max_active_sequences - (
		           SELECT count(*)::int FROM execution_contracts c
		            WHERE c.worker_id = o.worker_id AND c.runtime_profile_id = o.runtime_profile_id
		              AND c.state = 'EXECUTING'))),
		       status = i.status,
		       last_seen_at = i.observed_at,
		       updated_at = now()
		  FROM unnest(
		         $1::uuid[], $2::uuid[], $3::text[], $4::text[],
		         $5::int[], $6::text[], $7::timestamptz[]
		       ) AS i(worker_id, supplier_id, runtime_profile_id, warmth,
		              available_sequences, status, observed_at),
		       workers AS w
		 WHERE o.worker_id = i.worker_id
		   AND o.supplier_id = i.supplier_id
		   AND o.runtime_profile_id = i.runtime_profile_id
		   AND w.id = o.worker_id
		   AND i.available_sequences BETWEEN 0 AND o.max_active_sequences
		RETURNING o.worker_id, o.supplier_id, o.runtime_profile_id,
		          o.runtime_profile_sha256,
		          COALESCE(NULLIF(o.placement_plan->>'hw_class',''), w.hw_class),
		          o.max_active_sequences, o.available_sequences,
		          o.supplier_input_usd_per_million_tokens,
		          o.supplier_output_usd_per_million_tokens,
		          o.status`,
		workerIDs, supplierIDs, profileIDs, warmths, availables, statuses, observeds)
	if err != nil {
		for i := range errs {
			errs[i] = err
		}
		return errs
	}

	type updatedRow struct {
		workerID   uuid.UUID
		supplierID uuid.UUID
		profileID  string
		profileSHA string
		hwClass    string
		maxActive  int
		available  int
		inputRate  float64
		outputRate float64
		status     string
	}
	updated := make(map[string]updatedRow, len(batch))
	for rows.Next() {
		var u updatedRow
		if err := rows.Scan(&u.workerID, &u.supplierID, &u.profileID, &u.profileSHA,
			&u.hwClass, &u.maxActive, &u.available, &u.inputRate, &u.outputRate, &u.status); err != nil {
			rows.Close()
			for i := range errs {
				errs[i] = err
			}
			return errs
		}
		updated[updatedRowKey(u.workerID, u.supplierID, u.profileID)] = u
	}
	if err := rows.Err(); err != nil {
		for i := range errs {
			errs[i] = err
		}
		return errs
	}
	rows.Close()

	if len(updated) > 0 {
		// Multi-row sample insert — same operational telemetry the single
		// path writes, without N round-trips.
		sWorker := make([]uuid.UUID, 0, len(updated))
		sSupplier := make([]uuid.UUID, 0, len(updated))
		sProfile := make([]string, 0, len(updated))
		sSHA := make([]string, 0, len(updated))
		sHW := make([]string, 0, len(updated))
		sStatus := make([]string, 0, len(updated))
		sMax := make([]int32, 0, len(updated))
		sAvail := make([]int32, 0, len(updated))
		sIn := make([]float64, 0, len(updated))
		sOut := make([]float64, 0, len(updated))
		for _, u := range updated {
			sWorker = append(sWorker, u.workerID)
			sSupplier = append(sSupplier, u.supplierID)
			sProfile = append(sProfile, u.profileID)
			sSHA = append(sSHA, u.profileSHA)
			sHW = append(sHW, u.hwClass)
			sStatus = append(sStatus, u.status)
			sMax = append(sMax, int32(u.maxActive))
			sAvail = append(sAvail, int32(u.available))
			sIn = append(sIn, u.inputRate)
			sOut = append(sOut, u.outputRate)
		}
		if _, err := tx.Exec(ctx, `
			INSERT INTO realtime_offer_samples
			  (worker_id,supplier_id,runtime_profile_id,runtime_profile_sha256,hw_class,
			   status,max_active_sequences,available_sequences,
			   supplier_input_usd_per_million_tokens,supplier_output_usd_per_million_tokens)
			SELECT * FROM unnest(
			  $1::uuid[], $2::uuid[], $3::text[], $4::text[], $5::text[],
			  $6::text[], $7::int[], $8::int[], $9::float8[], $10::float8[]
			)`,
			sWorker, sSupplier, sProfile, sSHA, sHW, sStatus, sMax, sAvail, sIn, sOut); err != nil {
			for i := range errs {
				errs[i] = err
			}
			return errs
		}
	}

	if err := tx.Commit(ctx); err != nil {
		for i := range errs {
			errs[i] = err
		}
		return errs
	}

	for i, item := range batch {
		if _, ok := updated[updatedRowKey(item.worker.WorkerID, item.worker.SupplierID, item.hb.RuntimeProfileID)]; !ok {
			errs[i] = errNotFound
		}
	}
	// Record only the rows that actually reached the statement. A superseded
	// repeat did not persist its own payload, and claiming it did would let the
	// change-gate skip the write that carries the newer state.
	persistedAt := time.Now()
	for _, i := range order {
		if errs[i] == nil {
			s.recordDurableHeartbeat(batch[i].worker, batch[i].hb, persistedAt)
		}
	}
	return errs
}

// updatedRowKey identifies a successfully updated offer. supplier_id is part of
// the key because the UPDATE matches on it: without it, a batch containing a
// correct-supplier and a wrong-supplier heartbeat for the same
// (worker, profile) would report success to both.
func updatedRowKey(workerID, supplierID uuid.UUID, profileID string) string {
	return workerID.String() + "|" + supplierID.String() + "|" + profileID
}

// heartbeatRealtimeOfferOne is the single-device durable write. last_seen_at is
// the clamped observation time — never flush-time now().
func (s *Store) heartbeatRealtimeOfferOne(ctx context.Context, worker WorkerAuth, hb RealtimeOfferHeartbeat, observedAt time.Time) error {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	var (
		profileSHA string
		hwClass    string
		maxActive  int
		available  int
		inputRate  float64
		outputRate float64
	)
	err = tx.QueryRow(ctx, `
		UPDATE realtime_worker_offers AS o
		   SET warmth=$4,
		       available_sequences=GREATEST(0,LEAST($5,o.max_active_sequences-(
		           SELECT count(*)::int FROM execution_contracts c
		            WHERE c.worker_id=$1 AND c.runtime_profile_id=$3
		              AND c.state='EXECUTING'))),
		       status=$6,last_seen_at=$7,updated_at=now()
		  FROM workers AS w
		 WHERE o.worker_id=$1 AND o.supplier_id=$2 AND o.runtime_profile_id=$3
		   AND w.id=o.worker_id
		   AND $5 BETWEEN 0 AND o.max_active_sequences
		RETURNING o.runtime_profile_sha256,
	          COALESCE(NULLIF(o.placement_plan->>'hw_class',''),w.hw_class),o.max_active_sequences,
	          o.available_sequences,o.supplier_input_usd_per_million_tokens,
	          o.supplier_output_usd_per_million_tokens`,
		worker.WorkerID, worker.SupplierID, hb.RuntimeProfileID, hb.Warmth,
		hb.AvailableSequences, hb.Status, observedAt.UTC()).Scan(&profileSHA, &hwClass, &maxActive,
		&available, &inputRate, &outputRate)
	if errors.Is(err, pgx.ErrNoRows) {
		return errNotFound
	}
	if err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, `
		INSERT INTO realtime_offer_samples
		  (worker_id,supplier_id,runtime_profile_id,runtime_profile_sha256,hw_class,
		   status,max_active_sequences,available_sequences,
		   supplier_input_usd_per_million_tokens,supplier_output_usd_per_million_tokens)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10)`,
		worker.WorkerID, worker.SupplierID, hb.RuntimeProfileID, profileSHA, hwClass,
		hb.Status, maxActive, available, inputRate, outputRate); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

func (s *Store) ensureLivenessCoalescer() *livenessCoalescer {
	s.livenessOnce.Do(func() {
		cfg := livenessBatchConfigFromEnv()
		if s.livenessCfgOverride != nil {
			cfg = *s.livenessCfgOverride
		}
		if cfg.Enabled {
			s.livenessCoalescer = newLivenessCoalescer(s, cfg)
		}
		s.livenessCfg = cfg
	})
	return s.livenessCoalescer
}

// SetLivenessBatchConfigForTest overrides batching for a store. Must be called
// before any HeartbeatRealtimeOffer on that store.
func (s *Store) SetLivenessBatchConfigForTest(cfg livenessBatchConfig) {
	s.livenessCfgOverride = &cfg
}

// LivenessBatchEnabled reports whether coalesced writes are active on this store.
func (s *Store) LivenessBatchEnabled() bool {
	s.ensureLivenessCoalescer()
	return s.livenessCfg.Enabled
}

// LivenessBatchConfig returns the active coalescer knobs (after ensure).
func (s *Store) LivenessBatchConfig() livenessBatchConfig {
	s.ensureLivenessCoalescer()
	return s.livenessCfg
}

// LivenessFlushStats returns coalescer flush and item counts since start.
// Baseline (batch off) returns zeros — each heartbeat is its own statement.
func (s *Store) LivenessFlushStats() (flushes, items int64) {
	if s == nil || s.livenessCoalescer == nil {
		return 0, 0
	}
	return s.livenessCoalescer.flushCount.Load(), s.livenessCoalescer.itemCount.Load()
}

// formatLivenessWindowSQL returns the interval literal used in production SQL
// so tests can assert the Go constant still matches the predicates.
func formatLivenessWindowSQL() string {
	return fmt.Sprintf("%d seconds", int(realtimeOfferLivenessWindow/time.Second))
}
