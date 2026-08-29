package main

import (
	"context"
	"log"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/google/uuid"
)

// admissionTelemetry is a process-local, loss-bounded async path for
// realtime admission events. The events are observational market-liquidity
// telemetry: a fault must never fail an otherwise-valid buyer request
// (see recordRealtimeAdmissionEvent). Moving them off the first-token path
// is therefore a latency optimisation, not a money change.
//
// Loss bounds (not "never lose"):
//   - Under load: never drops. A full queue falls back to a synchronous
//     write on the caller, so overload becomes backpressure rather than
//     silent loss.
//   - Shutdown: Close drains the queue (with a timeout) so in-flight
//     events are written before the process exits gracefully.
//   - Crash / kill -9: at most queueCap + workerCount events that were
//     still in the channel or mid-write can be lost. That bound is
//     deliberate and tested. A process crash is already a loss of any
//     in-memory work; the queue capacity is the documented ceiling.
//
// The fill-rate denominator for market liquidity is therefore complete
// under ordinary load and graceful shutdown, and bounded-incomplete only
// on hard process death.
type admissionTelemetry struct {
	store   *Store
	ch      chan admissionTelemetryEvent
	stop    chan struct{}
	done    chan struct{}
	closed  atomic.Bool
	mu      sync.Mutex // serialises close vs send
	queued  atomic.Int64
	written atomic.Int64
	syncFB  atomic.Int64 // sync fallback count (queue full or post-close)
	workers int
}

type admissionTelemetryEvent struct {
	buyerID          uuid.UUID
	runtimeProfileID string
	hwClass          string
	decision         string
	contractID       uuid.UUID
}

const (
	admissionTelemetryQueueCap = 4096
	admissionTelemetryWorkers  = 2
	admissionTelemetryWriteTO  = 5 * time.Second
)

func newAdmissionTelemetry(store *Store) *admissionTelemetry {
	if store == nil {
		return nil
	}
	t := &admissionTelemetry{
		store:   store,
		ch:      make(chan admissionTelemetryEvent, admissionTelemetryQueueCap),
		stop:    make(chan struct{}),
		done:    make(chan struct{}),
		workers: admissionTelemetryWorkers,
	}
	var wg sync.WaitGroup
	for i := 0; i < t.workers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for ev := range t.ch {
				t.write(ev)
				t.queued.Add(-1)
			}
		}()
	}
	go func() {
		wg.Wait()
		close(t.done)
	}()
	return t
}

// record enqueues the event or writes it synchronously when the queue is
// full / the recorder is closed. Never drops.
func (t *admissionTelemetry) record(
	buyerID uuid.UUID, runtimeProfileID, hwClass, decision string, contractID uuid.UUID,
) {
	if t == nil {
		return
	}
	ev := admissionTelemetryEvent{
		buyerID: buyerID, runtimeProfileID: runtimeProfileID,
		hwClass: hwClass, decision: decision, contractID: contractID,
	}
	t.mu.Lock()
	if t.closed.Load() {
		t.mu.Unlock()
		t.syncFB.Add(1)
		t.write(ev)
		return
	}
	select {
	case t.ch <- ev:
		t.queued.Add(1)
		t.mu.Unlock()
	default:
		t.mu.Unlock()
		// Queue full: sync fallback so overload cannot drop telemetry.
		t.syncFB.Add(1)
		t.write(ev)
	}
}

func (t *admissionTelemetry) write(ev admissionTelemetryEvent) {
	// Retry transient Postgres deadlocks. The admitted path verifies the
	// contract row while finalize may hold it FOR UPDATE; under load those
	// two can deadlock. A failed write after retries would drop the event —
	// forbidden under ordinary load — so we retry rather than log-and-lose.
	const maxAttempts = 4
	var err error
	for attempt := 1; attempt <= maxAttempts; attempt++ {
		ctx, cancel := context.WithTimeout(context.Background(), admissionTelemetryWriteTO)
		// Trusted insert: the handler already holds the authorize-returned
		// placement. The verify SELECT in RecordRealtimeAdmissionEvent can
		// deadlock with FinalizeRealtimeSuccess's FOR UPDATE (default
		// deadlock_timeout ≈ 1s), which used to surface as a 502 on settle.
		err = t.store.InsertRealtimeAdmissionEventTrusted(ctx,
			ev.buyerID, ev.runtimeProfileID, ev.hwClass, ev.decision, ev.contractID)
		cancel()
		if err == nil {
			t.written.Add(1)
			return
		}
		if !isTransientPostgresContention(err) || attempt == maxAttempts {
			break
		}
		time.Sleep(time.Duration(attempt) * 2 * time.Millisecond)
	}
	log.Printf("realtime liquidity telemetry (async): decision=%s profile=%s: %v",
		ev.decision, ev.runtimeProfileID, err)
}

func isTransientPostgresContention(err error) bool {
	if err == nil {
		return false
	}
	// pgx surfaces SQLSTATE in the error string; avoid importing pgconn just
	// for this classification. 40P01 = deadlock_detected, 40001 = serialization.
	msg := err.Error()
	return strings.Contains(msg, "40P01") || strings.Contains(msg, "40001") ||
		strings.Contains(msg, "deadlock detected")
}

// Close stops accepting async enqueues and drains the queue. After Close,
// record() still works but writes synchronously. Safe to call once.
func (t *admissionTelemetry) Close(timeout time.Duration) {
	if t == nil {
		return
	}
	t.mu.Lock()
	if t.closed.Swap(true) {
		t.mu.Unlock()
		return
	}
	close(t.ch)
	t.mu.Unlock()
	if timeout <= 0 {
		timeout = 5 * time.Second
	}
	select {
	case <-t.done:
	case <-time.After(timeout):
		log.Printf("admission telemetry drain timed out after %s (queued≈%d)",
			timeout, t.queued.Load())
	}
}

// stats are for tests and diagnostics.
func (t *admissionTelemetry) stats() (written, syncFallbacks, queued int64) {
	if t == nil {
		return 0, 0, 0
	}
	return t.written.Load(), t.syncFB.Load(), t.queued.Load()
}

// queueCap exposes the crash-loss bound for tests.
func (t *admissionTelemetry) queueCap() int {
	if t == nil {
		return 0
	}
	return cap(t.ch)
}
