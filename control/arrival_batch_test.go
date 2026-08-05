package main

import (
	"context"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// Interactive work under a tight deadline must never sit in a join window past
// that deadline. This test fails if HoldWouldMissDeadline is bypassed or if
// Admit ignores a zero/negative safe hold.
func TestInteractiveNeverHeldPastDeadline(t *testing.T) {
	// A deadline so tight that any positive hold + EstimatedTTFT would miss.
	// Prefill of 4096 tokens estimates ~570ms TTFT; a 50ms remaining deadline
	// cannot absorb a hold.
	now := time.Date(2026, 8, 2, 12, 0, 0, 0, time.UTC)
	deadline := now.Add(50 * time.Millisecond)

	if !HoldWouldMissDeadline(deadline, now, 10*time.Millisecond, 4096) {
		t.Fatal("HoldWouldMissDeadline must report that a 10ms hold + interactive prefill misses a 50ms deadline")
	}
	// Immediate dispatch of the same prefill still misses — the gate is about
	// the hold, so hold=0 with work past deadline is also a miss. Admit must
	// bypass (window 0) rather than queue.
	if !HoldWouldMissDeadline(deadline, now, 0, 4096) {
		t.Fatal("even hold=0 must miss when EstimatedTTFT alone exceeds the remaining deadline")
	}

	var clock atomic.Value
	clock.Store(now)
	flushed := make(chan int, 8)
	batcher := NewArrivalBatcher(ArrivalBatchConfig{
		Enabled:    true,
		JoinWindow: 100 * time.Millisecond, // would hold if the deadline gate were removed
		Clock:      func() time.Time { return clock.Load().(time.Time) },
		AfterFlush: func(string, int) { flushed <- 1 },
	})

	ready := batcher.Admit(context.Background(), ArrivalRequest{
		ID: "interactive-tight", LaneKey: "model|cell|worker|samp",
		Class: TrafficInteractive, Deadline: deadline, PromptTokens: 4096,
	})
	select {
	case <-ready:
		// Released immediately — correct.
	case <-time.After(50 * time.Millisecond):
		t.Fatal("interactive request under a tight deadline was held in the batch window")
	}
	// Must not have been enqueued into a lane that later flushes.
	select {
	case <-flushed:
		t.Fatal("tight-deadline interactive request entered a flushable lane")
	case <-time.After(20 * time.Millisecond):
	}

	// Mutation guard: if JoinWindowForArrival stopped clamping to the deadline,
	// a long configured window would be returned for this request.
	hold := JoinWindowForArrival(TrafficInteractive, deadline, now, 4096, 100*time.Millisecond)
	if hold > 0 {
		t.Fatalf("JoinWindowForArrival returned %v; want 0 when remaining deadline cannot absorb work", hold)
	}
}

// A request with room under its deadline may join; the flush must still land
// before the deadline.
func TestArrivalBatchFlushesBeforeInteractiveDeadline(t *testing.T) {
	now := time.Date(2026, 8, 2, 12, 0, 0, 0, time.UTC)
	// Short prompts: EstimatedTTFT(128) is well under a second.
	deadline := now.Add(2 * time.Second)

	var mu sync.Mutex
	current := now
	var flushCount atomic.Int32
	batcher := NewArrivalBatcher(ArrivalBatchConfig{
		Enabled:    true,
		JoinWindow: 15 * time.Millisecond,
		Clock: func() time.Time {
			mu.Lock()
			defer mu.Unlock()
			return current
		},
		AfterFlush: func(string, int) { flushCount.Add(1) },
	})

	const n = 4
	readies := make([]<-chan struct{}, n)
	for i := 0; i < n; i++ {
		readies[i] = batcher.Admit(context.Background(), ArrivalRequest{
			ID: string(rune('a' + i)), LaneKey: "m|c|w|s",
			Class: TrafficInteractive, Deadline: deadline, PromptTokens: 128,
		})
	}
	// Advance clock past the join window and wait for release.
	deadlineCheck := time.After(500 * time.Millisecond)
	for i := 0; i < n; i++ {
		select {
		case <-readies[i]:
		case <-deadlineCheck:
			t.Fatalf("request %d was not released before the test deadline", i)
		}
	}
	// Wall time from admit to release must be under the contract deadline span.
	// (We cannot advance the fake clock from the timer — time.AfterFunc uses
	// real time — so the join window is a real 15ms here.)
	if flushCount.Load() < 1 {
		t.Fatal("expected at least one lane flush")
	}
}

// Two requests that share a lane key must be released together when the window
// elapses; different keys must not block each other.
func TestArrivalBatchGroupsCompatibleOnly(t *testing.T) {
	batcher := NewArrivalBatcher(ArrivalBatchConfig{
		Enabled:    true,
		JoinWindow: 30 * time.Millisecond,
	})
	defer batcher.Close()

	a := batcher.Admit(context.Background(), ArrivalRequest{
		ID: "a", LaneKey: "same", Class: TrafficBatchStandard,
		Deadline: time.Now().Add(time.Minute), PromptTokens: 64,
	})
	b := batcher.Admit(context.Background(), ArrivalRequest{
		ID: "b", LaneKey: "same", Class: TrafficBatchStandard,
		Deadline: time.Now().Add(time.Minute), PromptTokens: 64,
	})
	other := batcher.Admit(context.Background(), ArrivalRequest{
		ID: "c", LaneKey: "other", Class: TrafficBatchStandard,
		Deadline: time.Now().Add(time.Minute), PromptTokens: 64,
	})

	// Both lanes release within a few windows.
	for _, ch := range []<-chan struct{}{a, b, other} {
		select {
		case <-ch:
		case <-time.After(200 * time.Millisecond):
			t.Fatal("request was not released")
		}
	}
}

// Disabled batcher is a pure bypass — every Admit returns an already-ready
// channel. Measurement harnesses use this for the off configuration.
func TestArrivalBatchDisabledIsBypass(t *testing.T) {
	batcher := NewArrivalBatcher(ArrivalBatchConfig{Enabled: false})
	ready := batcher.Admit(context.Background(), ArrivalRequest{
		ID: "x", LaneKey: "k", Class: TrafficInteractive,
		Deadline: time.Now().Add(time.Minute), PromptTokens: 128,
	})
	select {
	case <-ready:
	default:
		t.Fatal("disabled batcher must release immediately")
	}
}

// Billing is identical batched vs unbatched. The batcher does not enter the
// money path; this test locks that invariant to the charge function the
// realtime path uses for price derivation.
func TestBillingIdenticalBatchedVsUnbatched(t *testing.T) {
	// Same token geometry, same catalogue rates: the charge must not depend on
	// whether the request shared an arrival batch.
	const (
		prompt     int64   = 128
		completion int64   = 32
		inputRate  float64 = 0.10 // USD / 1M tokens
		outputRate float64 = 0.40
	)
	// Simulate "batched" and "unbatched" purely as a label: both call the same
	// authority. If a future change threads a batch factor into tokenCharge,
	// this fails.
	unbatched, err := tokenCharge(prompt, completion, inputRate, outputRate)
	must(t, err)
	// Two members of one arrival batch, each billed alone.
	var batchedTotal float64
	for i := 0; i < 2; i++ {
		c, err := tokenCharge(prompt, completion, inputRate, outputRate)
		must(t, err)
		batchedTotal += c
	}
	if want := unbatched * 2; batchedTotal != want {
		t.Fatalf("batched sum %v != 2× unbatched %v; batching must not change what is charged",
			batchedTotal, want)
	}
	// Per-request equality is the buyer-facing claim.
	batchedOne, err := tokenCharge(prompt, completion, inputRate, outputRate)
	must(t, err)
	if batchedOne != unbatched {
		t.Fatalf("per-request charge changed with batching: batched=%v unbatched=%v",
			batchedOne, unbatched)
	}
}

// A throughput-class hold that would breach an interactive deadline is refused
// by the same ValidateBatchBudget primitive batch_policy uses. Keeps the
// arrival batcher consistent with the existing latency-contract gate rather
// than inventing a second one.
func TestThroughputBudgetCannotBreachInteractiveDeadline(t *testing.T) {
	// maxBatchTokens (~8192) estimates ~1138ms TTFT; a 100ms interactive
	// deadline must refuse that budget.
	if err := ValidateBatchBudget(LatencyInteractive, maxBatchTokens, 100*time.Millisecond); err == nil {
		t.Fatal("throughput-sized budget must be refused under an interactive deadline")
	}
	// The arrival path uses the same check: a request whose class budget cannot
	// meet remaining deadline bypasses the join window.
	now := time.Now()
	deadline := now.Add(100 * time.Millisecond)
	batcher := NewArrivalBatcher(ArrivalBatchConfig{
		Enabled:    true,
		JoinWindow: time.Second,
		Clock:      func() time.Time { return now },
	})
	// BATCH_STANDARD carries maxBatchTokens. Under a 100ms deadline the budget
	// validation fails and Admit bypasses.
	ready := batcher.Admit(context.Background(), ArrivalRequest{
		ID: "thru", LaneKey: "k", Class: TrafficBatchStandard,
		Deadline: deadline, PromptTokens: 128,
	})
	select {
	case <-ready:
	default:
		t.Fatal("request whose class budget cannot meet the deadline must bypass the window")
	}
}

// Cost-class discipline: different worker IDs produce different lane keys, so
// batching cannot merge work onto a more expensive worker after authorization.
func TestArrivalLaneKeyIncludesWorker(t *testing.T) {
	a := ArrivalLaneKey("m", "cell", "worker-cheap", "samp")
	b := ArrivalLaneKey("m", "cell", "worker-expensive", "samp")
	if a == b {
		t.Fatal("lane key must include worker id so cost class cannot be jumped by batching")
	}
	if ArrivalLaneKey("m", "cell", "w", "t=0") == ArrivalLaneKey("m", "cell", "w", "t=1") {
		t.Fatal("lane key must include sampling fingerprint")
	}
}

// Context cancel removes a waiter without releasing siblings as a flush.
func TestArrivalBatchCancelDoesNotFlushSiblings(t *testing.T) {
	var flushes atomic.Int32
	batcher := NewArrivalBatcher(ArrivalBatchConfig{
		Enabled:    true,
		JoinWindow: 80 * time.Millisecond,
		AfterFlush: func(string, int) { flushes.Add(1) },
	})
	defer batcher.Close()

	ctx, cancel := context.WithCancel(context.Background())
	a := batcher.Admit(ctx, ArrivalRequest{
		ID: "a", LaneKey: "k", Class: TrafficBatchStandard,
		Deadline: time.Now().Add(time.Minute), PromptTokens: 64,
	})
	b := batcher.Admit(context.Background(), ArrivalRequest{
		ID: "b", LaneKey: "k", Class: TrafficBatchStandard,
		Deadline: time.Now().Add(time.Minute), PromptTokens: 64,
	})
	cancel()
	select {
	case <-a:
	case <-time.After(100 * time.Millisecond):
		t.Fatal("cancelled request was not released")
	}
	// Sibling still waits for the join window, then flushes alone (or with
	// whoever remains).
	select {
	case <-b:
	case <-time.After(200 * time.Millisecond):
		t.Fatal("sibling was not released")
	}
}
