package main

import (
	"context"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Arrival batching: group compatible, already-admitted requests so a continuous-
// batching engine receives them as one coherent arrival rather than N staggered
// ones.
//
// This is not coalescing. Coalescing (inflight_coalescing.go) collapses identical
// requests onto one physical execution and one supplier payable. Arrival batching
// keeps every request as its own contract, its own bill, and its own delivery;
// it only changes when the upstream forward starts. Billing is therefore
// identical batched vs unbatched by construction — the money path never sees the
// batcher.
//
// This is also not a second token-budget policy. SelectBatch, EstimatedTTFT and
// ValidateBatchBudget in batch_policy.go are the packing and deadline primitives;
// the four classes in traffic_class.go are the queue policy. This file is the
// scheduler that consumes both.
//
// Cost-class discipline: the compatibility key includes the already-selected
// worker / upstream / cost class. Batching never re-routes work onto a more
// expensive worker. Warmth and join only operate inside that key — the same
// rule scheduler.go applies to warmth vs cost rank.

// ArrivalBatchConfig configures the batcher. Zero values mean "use defaults".
type ArrivalBatchConfig struct {
	// Enabled turns the join window on. When false, Admit returns immediately
	// (bypass). Measurement harnesses flip this for off vs on sweeps.
	Enabled bool

	// JoinWindow overrides the per-class default join window when > 0.
	// Named constant origins are in traffic_class.go; measurement should replace
	// them with measured engine feedback (see JoinWindowForArrival).
	JoinWindow time.Duration

	// Clock, when set, replaces time.Now for tests.
	Clock func() time.Time

	// AfterFlush is an optional test hook invoked after a lane flushes.
	AfterFlush func(laneKey string, n int)
}

// ArrivalRequest is one admitted unit offered to the batcher.
//
// Compatibility (same model, same runtime cell, same worker/cost class, same
// sampling fingerprint) is the caller's responsibility to encode into LaneKey.
// Traffic class is carried so a lower class can never force a higher class to
// wait, and so token budgets come from the promoted table.
type ArrivalRequest struct {
	ID string

	// LaneKey groups compatible work. Must encode model + runtime cell +
	// selected worker (cost class) + sampling params. Requests with different
	// keys never share a batch.
	LaneKey string

	Class        TrafficClass
	Deadline     time.Time
	PromptTokens int
	OutputTokens int

	// ReusablePrefixTokens is warm prefill the target already holds; passed
	// through to SelectBatch so cache hits pack densely.
	ReusablePrefixTokens int
}

// ArrivalBatcher holds compatible admits for a short join window, then releases
// them together.
type ArrivalBatcher struct {
	cfg ArrivalBatchConfig

	mu    sync.Mutex
	lanes map[string]*arrivalLane
	// closed is set by Close; further Admit calls bypass.
	closed bool
}

type arrivalLane struct {
	key      string
	class    TrafficClass
	pending  []*arrivalWaiter
	timer    *time.Timer
	flushing bool
}

type arrivalWaiter struct {
	req      ArrivalRequest
	admitted time.Time
	// ready is closed when this request may forward. Never sends a value —
	// close is the signal, so multiple waiters and a cancelled context cannot
	// deadlock on a buffered channel.
	ready chan struct{}
	// released is set under the batcher lock when ready is closed, so a late
	// Cancel does not double-close.
	released bool
}

// NewArrivalBatcher constructs a batcher. The zero config is disabled (Admit is
// a no-op bypass), which is the safe default until a caller opts in.
func NewArrivalBatcher(cfg ArrivalBatchConfig) *ArrivalBatcher {
	return &ArrivalBatcher{
		cfg:   cfg,
		lanes: make(map[string]*arrivalLane),
	}
}

func (b *ArrivalBatcher) now() time.Time {
	if b.cfg.Clock != nil {
		return b.cfg.Clock()
	}
	return time.Now()
}

// Admit offers a request to the batcher.
//
// The returned channel is closed when the request may forward. It is always
// non-nil. When the batcher is disabled, closed, or the request cannot safely
// wait (interactive with no window, deadline too tight, unknown class), the
// channel is already closed — bypass, not an error.
//
// ctx cancellation removes the request from the lane without flushing siblings.
func (b *ArrivalBatcher) Admit(ctx context.Context, req ArrivalRequest) <-chan struct{} {
	ready := make(chan struct{})
	if b == nil || !b.cfg.Enabled || b.isClosed() || req.LaneKey == "" {
		close(ready)
		return ready
	}
	policy, err := PolicyForTrafficClass(req.Class)
	if err != nil {
		// Unknown class: fail closed to bypass, never to a free upgrade.
		close(ready)
		return ready
	}
	now := b.now()
	hold := JoinWindowForArrival(req.Class, req.Deadline, now, req.PromptTokens, b.cfg.JoinWindow)
	if hold <= 0 || HoldWouldMissDeadline(req.Deadline, now, hold, req.PromptTokens) {
		close(ready)
		return ready
	}
	// Refuse a hold whose class token budget itself cannot meet the deadline.
	// Same primitive ValidateBatchBudget uses for a throughput budget that
	// would breach an interactive latency contract.
	if !req.Deadline.IsZero() {
		if err := ValidateBatchBudget(latencyClassForTraffic(req.Class), policy.TokenBudget, req.Deadline.Sub(now)); err != nil {
			// The class budget cannot meet the remaining deadline even with no
			// hold. Forward immediately rather than waiting.
			close(ready)
			return ready
		}
	}

	w := &arrivalWaiter{req: req, admitted: now, ready: ready}

	b.mu.Lock()
	if b.closed {
		b.mu.Unlock()
		close(ready)
		return ready
	}
	lane := b.lanes[req.LaneKey]
	if lane == nil {
		lane = &arrivalLane{key: req.LaneKey, class: req.Class}
		b.lanes[req.LaneKey] = lane
	}
	// Never mix classes on one lane. A lower class that collides on LaneKey
	// (caller bug) bypasses rather than delaying a higher class.
	if lane.class != req.Class {
		b.mu.Unlock()
		close(ready)
		return ready
	}
	lane.pending = append(lane.pending, w)
	// If packing the new arrival would push estimated TTFT past the tightest
	// interactive-or-any deadline in the lane, flush what we already had
	// without the newcomer, then start a fresh lane with only the newcomer —
	// or, if the newcomer alone is fine with the current set under budget,
	// flush everything now.
	if force, alone := b.deadlineForceFlush(lane); force {
		if alone {
			// Drop the newcomer from pending, flush the rest, re-queue newcomer.
			lane.pending = lane.pending[:len(lane.pending)-1]
			b.flushLaneLocked(lane, "deadline_protect")
			// Re-create lane for the newcomer.
			lane = &arrivalLane{key: req.LaneKey, class: req.Class, pending: []*arrivalWaiter{w}}
			b.lanes[req.LaneKey] = lane
			b.armTimerLocked(lane, hold)
		} else {
			b.flushLaneLocked(lane, "deadline_force")
		}
		b.mu.Unlock()
		return ready
	}
	// Token budget full → flush now so the engine gets a coherent full batch.
	if b.budgetFull(lane, policy.TokenBudget) {
		b.flushLaneLocked(lane, "budget_full")
		b.mu.Unlock()
		return ready
	}
	// Arm or re-arm the timer to the earliest force time across the lane.
	b.armTimerLocked(lane, b.earliestHold(lane, now))
	b.mu.Unlock()

	// Cancellation: remove from lane if still pending.
	if ctx != nil {
		go func() {
			select {
			case <-ready:
				return
			case <-ctx.Done():
				b.cancel(w)
			}
		}()
	}
	return ready
}

func (b *ArrivalBatcher) isClosed() bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.closed
}

// Close flushes every lane and rejects further holds.
func (b *ArrivalBatcher) Close() {
	if b == nil {
		return
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	b.closed = true
	for _, lane := range b.lanes {
		b.flushLaneLocked(lane, "close")
	}
}

func (b *ArrivalBatcher) cancel(w *arrivalWaiter) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if w.released {
		return
	}
	lane := b.lanes[w.req.LaneKey]
	if lane == nil {
		w.released = true
		close(w.ready)
		return
	}
	for i, p := range lane.pending {
		if p == w {
			lane.pending = append(lane.pending[:i], lane.pending[i+1:]...)
			break
		}
	}
	w.released = true
	close(w.ready)
	if len(lane.pending) == 0 {
		if lane.timer != nil {
			lane.timer.Stop()
			lane.timer = nil
		}
		delete(b.lanes, lane.key)
	}
}

func (b *ArrivalBatcher) budgetFull(lane *arrivalLane, budget int) bool {
	if len(lane.pending) == 0 || budget <= 0 {
		return false
	}
	candidates := make([]BatchCandidate, len(lane.pending))
	for i, p := range lane.pending {
		candidates[i] = BatchCandidate{
			ID: p.req.ID, PromptTokens: p.req.PromptTokens,
			OutputTokens: p.req.OutputTokens, ReusablePrefixTokens: p.req.ReusablePrefixTokens,
		}
	}
	chosen, used := SelectBatch(candidates, budget)
	// Full when every pending member was packed and the budget is exhausted,
	// or when at least one pending member was left out (no room).
	if len(chosen) < len(candidates) {
		return true
	}
	return used >= budget
}

// deadlineForceFlush reports whether the current pending set would miss a
// deadline. force=true means flush now. alone=true means the newest arrival is
// the one that tips the budget past a deadline and should not join the others.
func (b *ArrivalBatcher) deadlineForceFlush(lane *arrivalLane) (force, alone bool) {
	if len(lane.pending) == 0 {
		return false, false
	}
	now := b.now()
	candidates := make([]BatchCandidate, len(lane.pending))
	for i, p := range lane.pending {
		candidates[i] = BatchCandidate{
			ID: p.req.ID, PromptTokens: p.req.PromptTokens,
			OutputTokens: p.req.OutputTokens, ReusablePrefixTokens: p.req.ReusablePrefixTokens,
		}
	}
	// Select under the class budget so we evaluate the batch the engine would
	// actually run, not an unbounded pack.
	policy, err := PolicyForTrafficClass(lane.class)
	if err != nil {
		return true, false
	}
	chosen, used := SelectBatch(candidates, policy.TokenBudget)
	if len(chosen) == 0 {
		return false, false
	}
	// Tightest deadline among the chosen set.
	var tightest time.Time
	for _, p := range lane.pending {
		in := false
		for _, c := range chosen {
			if c.ID == p.req.ID {
				in = true
				break
			}
		}
		if !in || p.req.Deadline.IsZero() {
			continue
		}
		if tightest.IsZero() || p.req.Deadline.Before(tightest) {
			tightest = p.req.Deadline
		}
	}
	if tightest.IsZero() {
		return false, false
	}
	// Would running this batch now miss? If even immediate dispatch misses,
	// still flush — holding cannot help.
	if HoldWouldMissDeadline(tightest, now, 0, used) {
		// If dropping the newest makes the rest safe, isolate it.
		if len(lane.pending) > 1 {
			without := candidates[:len(candidates)-1]
			_, usedWithout := SelectBatch(without, policy.TokenBudget)
			var tightWithout time.Time
			for _, p := range lane.pending[:len(lane.pending)-1] {
				if p.req.Deadline.IsZero() {
					continue
				}
				if tightWithout.IsZero() || p.req.Deadline.Before(tightWithout) {
					tightWithout = p.req.Deadline
				}
			}
			if !tightWithout.IsZero() && !HoldWouldMissDeadline(tightWithout, now, 0, usedWithout) {
				return true, true
			}
		}
		return true, false
	}
	// Would continuing to hold until the lane's current earliest hold miss?
	hold := b.earliestHold(lane, now)
	if HoldWouldMissDeadline(tightest, now, hold, used) {
		return true, false
	}
	return false, false
}

func (b *ArrivalBatcher) earliestHold(lane *arrivalLane, now time.Time) time.Duration {
	var earliest time.Duration
	set := false
	for _, p := range lane.pending {
		hold := JoinWindowForArrival(p.req.Class, p.req.Deadline, p.admitted, p.req.PromptTokens, b.cfg.JoinWindow)
		// Remaining hold from now, not from admit time.
		elapsed := now.Sub(p.admitted)
		remaining := hold - elapsed
		if remaining < 0 {
			remaining = 0
		}
		if !set || remaining < earliest {
			earliest = remaining
			set = true
		}
	}
	if !set {
		return 0
	}
	return earliest
}

func (b *ArrivalBatcher) armTimerLocked(lane *arrivalLane, hold time.Duration) {
	if lane.timer != nil {
		lane.timer.Stop()
		lane.timer = nil
	}
	if hold <= 0 {
		b.flushLaneLocked(lane, "hold_elapsed")
		return
	}
	key := lane.key
	lane.timer = time.AfterFunc(hold, func() {
		b.mu.Lock()
		defer b.mu.Unlock()
		l := b.lanes[key]
		if l == nil {
			return
		}
		b.flushLaneLocked(l, "timer")
	})
}

func (b *ArrivalBatcher) flushLaneLocked(lane *arrivalLane, reason string) {
	if lane == nil || lane.flushing {
		return
	}
	lane.flushing = true
	if lane.timer != nil {
		lane.timer.Stop()
		lane.timer = nil
	}
	pending := lane.pending
	lane.pending = nil
	delete(b.lanes, lane.key)
	n := 0
	for _, w := range pending {
		if w.released {
			continue
		}
		w.released = true
		close(w.ready)
		n++
	}
	if b.cfg.AfterFlush != nil && n > 0 {
		// Hook outside the critical path intent; still under lock so tests see
		// deterministic ordering. Production leaves AfterFlush nil.
		b.cfg.AfterFlush(lane.key, n)
	}
	_ = reason
}

// latencyClassForTraffic maps a traffic class onto the two-way latency class
// ValidateBatchBudget understands. INTERACTIVE keeps the tight budget; every
// throughput-shaped class uses the batch budget.
func latencyClassForTraffic(class TrafficClass) LatencyClass {
	if class == TrafficInteractive {
		return LatencyInteractive
	}
	return LatencyBatch
}

// ArrivalLaneKey builds the compatibility key. WorkerID is required so batching
// cannot move work onto a different (possibly more expensive) cost class —
// the worker was already chosen by AuthorizeRealtimeContract under the
// scheduler's cost-class discipline.
func ArrivalLaneKey(model, runtimeCell, workerID, samplingFingerprint string) string {
	var key strings.Builder
	key.Grow(len(model) + len(runtimeCell) + len(workerID) + len(samplingFingerprint) + 3)
	key.WriteString(model)
	key.WriteByte('|')
	key.WriteString(runtimeCell)
	key.WriteByte('|')
	key.WriteString(workerID)
	key.WriteByte('|')
	key.WriteString(samplingFingerprint)
	return key.String()
}

// SamplingFingerprint extracts the sampling params that must match for two
// requests to share a continuous-batching arrival. Distinct from request
// identity: prompts differ inside a batch; temperature/top_p/seed/max_tokens
// must not, or the engine's batch is heterogeneous for no gain.
func SamplingFingerprint(temperature, topP float64, seed *int64, maxTokens int64) string {
	var storage [96]byte
	encoded := append(storage[:0], "t="...)
	encoded = strconv.AppendFloat(encoded, temperature, 'g', -1, 64)
	encoded = append(encoded, "|p="...)
	encoded = strconv.AppendFloat(encoded, topP, 'g', -1, 64)
	encoded = append(encoded, "|s="...)
	if seed == nil {
		encoded = append(encoded, "none"...)
	} else {
		encoded = strconv.AppendInt(encoded, *seed, 10)
	}
	encoded = append(encoded, "|m="...)
	encoded = strconv.AppendInt(encoded, maxTokens, 10)
	return string(encoded)
}

// WaitArrival blocks until the batcher releases the request or ctx ends.
// Returns true if released by the batcher, false if ctx ended first.
func WaitArrival(ctx context.Context, ready <-chan struct{}) bool {
	if ready == nil {
		return true
	}
	select {
	case <-ready:
		return true
	case <-ctx.Done():
		return false
	}
}
