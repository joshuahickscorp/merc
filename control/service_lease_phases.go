package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
)

// Lease phases for G021 — defined against the implemented state machine before
// any capture. A definition that invents states production does not have is
// wrong; a definition that ignores states production has hides cost.
//
// Implemented lease states (service_leases.state CHECK):
//
//	ACTIVE | UPGRADING | FAILOVER_REQUIRED | COMPLETED | CANCELLED
//
// Implemented event kinds (service_lease_events_kind_check):
//
//	ACTIVATED | METERED | SLO_MEASURED | ROLLING_UPDATE_STARTED |
//	ROLLING_UPDATE_COMPLETED | WORKER_LOSS | FAILOVER_COMPLETED |
//	FAILOVER_TERMINATED | EXPIRED | CANCELLED
//
// Phase set (continuous metered contract, not a one-shot job):
//
//  1. reservation — pricing acceptance + capacity hold until ACTIVATED.
//     Endpoints: lease.created_at → first ACTIVATED event.created_at.
//     Same-txn activation yields a measured zero, not unknown.
//
//  2. provisioning_to_ready — activation until the worker first attests READY
//     via SLO_MEASURED while the lease is (or becomes) ACTIVE. READY is a
//     heartbeat status, not a lease state; the first SLO_MEASURED after
//     ACTIVATED is the observable READY boundary. Without it the phase is
//     unknown — we do not treat ACTIVATED as READY.
//
//  3. steady_serving — ACTIVE windows while the worker is READY. Sum of
//     intervals from each READY-capable SLO_MEASURED (or ACTIVE entry) to the
//     next non-serving transition (UPGRADING / FAILOVER_REQUIRED / terminal).
//     When only endpoints exist without intermediate heartbeats, the span
//     from first READY to the first leave-ACTIVE event is used.
//
//  4. upgrade_drain — each UPGRADING interval:
//     ROLLING_UPDATE_STARTED → ROLLING_UPDATE_COMPLETED (or terminal).
//
//  5. failover — each FAILOVER_REQUIRED interval:
//     WORKER_LOSS (or control-plane force) → FAILOVER_COMPLETED or
//     FAILOVER_TERMINATED / CANCELLED / EXPIRED.
//
//  6. termination — last non-terminal moment → finalized_at when state is
//     COMPLETED or CANCELLED. Absent finalized_at means the phase is unknown
//     (lease still live), never zero.
//
// Metering (METERED events) is continuous across ACTIVE/UPGRADING and is NOT a
// separate phase: money accrues by replica-nanoseconds, not by phase labels.
// Phase capture is for latency/regret accounting, not a second money rail.

// LeasePhaseDecomposition is one lease's observed phase spans.
type LeasePhaseDecomposition struct {
	LeaseID uuid.UUID `json:"lease_id"`
	State   string    `json:"state"`

	Reservation         TaskPhase `json:"reservation"`
	ProvisioningToReady TaskPhase `json:"provisioning_to_ready"`
	SteadyServing       TaskPhase `json:"steady_serving"`
	UpgradeDrain        TaskPhase `json:"upgrade_drain"`
	Failover            TaskPhase `json:"failover"`
	Termination         TaskPhase `json:"termination"`

	// EventCounts makes sparse histories reviewable without a second query.
	EventCounts map[string]int `json:"event_counts,omitempty"`
}

// KnownPhases returns only measured spans.
func (d LeasePhaseDecomposition) KnownPhases() []TaskPhase {
	var out []TaskPhase
	for _, p := range []TaskPhase{
		d.Reservation, d.ProvisioningToReady, d.SteadyServing,
		d.UpgradeDrain, d.Failover, d.Termination,
	} {
		if p.Known {
			out = append(out, p)
		}
	}
	return out
}

type leaseEventRow struct {
	kind      string
	createdAt time.Time
}

// DecomposeLeasePhases reads one lease's timestamps and event history. It never
// writes and never invents a span whose endpoints are not both present.
func DecomposeLeasePhases(ctx context.Context, db ledgerExec, leaseID uuid.UUID) (LeasePhaseDecomposition, error) {
	if leaseID == uuid.Nil {
		return LeasePhaseDecomposition{}, errors.New("lease phase decomposition requires a lease id")
	}
	var (
		out                  LeasePhaseDecomposition
		createdAt, startedAt time.Time
		finalizedAt          *time.Time
		state                string
	)
	if err := db.QueryRow(ctx, `
		SELECT state, created_at, started_at, finalized_at
		  FROM service_leases WHERE id = $1`, leaseID,
	).Scan(&state, &createdAt, &startedAt, &finalizedAt); err != nil {
		return LeasePhaseDecomposition{}, fmt.Errorf("read service lease %s: %w", leaseID, err)
	}
	out.LeaseID = leaseID
	out.State = state
	out.EventCounts = map[string]int{}

	// ledgerExec exposes QueryRow only; pull the ordered history as one JSON
	// aggregate rather than opening a multi-row cursor.
	var eventsJSON []byte
	if err := db.QueryRow(ctx, `
		SELECT COALESCE(jsonb_agg(jsonb_build_object(
		         'kind', kind,
		         'created_at', created_at)
		       ORDER BY created_at ASC, id ASC), '[]'::jsonb)
		  FROM service_lease_events
		 WHERE lease_id = $1`, leaseID,
	).Scan(&eventsJSON); err != nil {
		return LeasePhaseDecomposition{}, fmt.Errorf("read lease events: %w", err)
	}
	var rawEvents []struct {
		Kind      string    `json:"kind"`
		CreatedAt time.Time `json:"created_at"`
	}
	if err := json.Unmarshal(eventsJSON, &rawEvents); err != nil {
		return LeasePhaseDecomposition{}, fmt.Errorf("decode lease events: %w", err)
	}
	events := make([]leaseEventRow, 0, len(rawEvents))
	for _, e := range rawEvents {
		events = append(events, leaseEventRow{kind: e.Kind, createdAt: e.CreatedAt})
		out.EventCounts[e.Kind]++
	}

	// --- reservation: created_at → ACTIVATED ---
	activatedAt := firstEventTime(events, "ACTIVATED")
	if activatedAt != nil {
		out.Reservation = measuredPhase("reservation", &createdAt, activatedAt, "created_at", "ACTIVATED.created_at")
	} else {
		out.Reservation = TaskPhase{Name: "reservation", Why: "no ACTIVATED event"}
	}

	// --- provisioning_to_ready: ACTIVATED → first SLO_MEASURED ---
	// SLO_MEASURED is only written on READY heartbeats (service_leases.go).
	firstReady := firstEventTime(events, "SLO_MEASURED")
	if activatedAt != nil && firstReady != nil {
		out.ProvisioningToReady = measuredPhase("provisioning_to_ready", activatedAt, firstReady,
			"ACTIVATED.created_at", "first SLO_MEASURED.created_at")
	} else if activatedAt == nil {
		out.ProvisioningToReady = TaskPhase{Name: "provisioning_to_ready", Why: "no ACTIVATED event"}
	} else {
		out.ProvisioningToReady = TaskPhase{Name: "provisioning_to_ready",
			Why: "no SLO_MEASURED event — worker never attested READY data-plane"}
	}

	// --- upgrade_drain: sum of ROLLING_UPDATE_STARTED → COMPLETED (or end) ---
	out.UpgradeDrain = sumPairedSpans("upgrade_drain", events,
		"ROLLING_UPDATE_STARTED", "ROLLING_UPDATE_COMPLETED",
		[]string{"FAILOVER_COMPLETED", "FAILOVER_TERMINATED", "EXPIRED", "CANCELLED"})

	// --- failover: WORKER_LOSS → FAILOVER_COMPLETED|TERMINATED|terminal ---
	out.Failover = sumPairedSpans("failover", events,
		"WORKER_LOSS", "FAILOVER_COMPLETED",
		[]string{"FAILOVER_TERMINATED", "EXPIRED", "CANCELLED"})
	// FAILOVER_TERMINATED alone (control path without WORKER_LOSS event pair)
	// is a terminal event, not a span; leave as unknown if no WORKER_LOSS open.

	// --- steady_serving: first READY → first leave-serving transition ---
	// Leave-serving: ROLLING_UPDATE_STARTED, WORKER_LOSS, EXPIRED, CANCELLED.
	// Multiple READY windows after failover completion are summed when both
	// endpoints exist.
	out.SteadyServing = sumSteadyServing(events, firstReady)

	// --- termination: last non-terminal event (or started_at) → finalized_at ---
	if finalizedAt == nil {
		out.Termination = TaskPhase{Name: "termination",
			Why: "finalized_at unset — lease is not COMPLETED/CANCELLED"}
	} else {
		from, fromName := lastPreTerminal(events, startedAt)
		out.Termination = measuredPhase("termination", &from, finalizedAt, fromName, "finalized_at")
	}

	return out, nil
}

func firstEventTime(events []leaseEventRow, kind string) *time.Time {
	for i := range events {
		if events[i].kind == kind {
			t := events[i].createdAt
			return &t
		}
	}
	return nil
}

func lastPreTerminal(events []leaseEventRow, fallback time.Time) (time.Time, string) {
	terminal := map[string]bool{
		"EXPIRED": true, "CANCELLED": true,
		"FAILOVER_TERMINATED": true,
	}
	for i := len(events) - 1; i >= 0; i-- {
		if !terminal[events[i].kind] {
			return events[i].createdAt, "last non-terminal event"
		}
	}
	return fallback, "started_at"
}

// sumPairedSpans pairs each openKind with the next closeKind or any of
// altCloses. Unclosed opens are omitted (unknown), not clamped to now — a live
// upgrade must not mint a duration that has not ended.
func sumPairedSpans(name string, events []leaseEventRow, openKind, closeKind string, altCloses []string) TaskPhase {
	closeSet := map[string]bool{closeKind: true}
	for _, k := range altCloses {
		closeSet[k] = true
	}
	var total time.Duration
	var pairs int
	var open *time.Time
	for i := range events {
		e := events[i]
		if e.kind == openKind {
			if open == nil {
				t := e.createdAt
				open = &t
			}
			continue
		}
		if open != nil && closeSet[e.kind] {
			d := e.createdAt.Sub(*open)
			if d >= 0 {
				total += d
				pairs++
			}
			open = nil
		}
	}
	if pairs == 0 {
		if open != nil {
			return TaskPhase{Name: name, Why: fmt.Sprintf(
				"%s opened at %s but never closed; live span is not measured as complete",
				openKind, open.UTC().Format(time.RFC3339Nano))}
		}
		return TaskPhase{Name: name, Why: "no " + openKind + " events"}
	}
	return TaskPhase{
		Name: name, Known: true, Duration: total,
		DurationMS: float64(total) / float64(time.Millisecond),
	}
}

// sumSteadyServing accumulates READY→leave intervals. A lease that only has
// SLO_MEASURED and then EXPIRED/CANCELLED without upgrade/failover still gets
// one steady span.
func sumSteadyServing(events []leaseEventRow, firstReady *time.Time) TaskPhase {
	if firstReady == nil {
		return TaskPhase{Name: "steady_serving", Why: "no SLO_MEASURED (READY) event"}
	}
	leaveKinds := map[string]bool{
		"ROLLING_UPDATE_STARTED": true,
		"WORKER_LOSS":            true,
		"EXPIRED":                true,
		"CANCELLED":              true,
		"FAILOVER_TERMINATED":    true,
	}
	// Resume serving after FAILOVER_COMPLETED or ROLLING_UPDATE_COMPLETED:
	// next SLO_MEASURED re-opens.
	var total time.Duration
	var pairs int
	open := firstReady
	for i := range events {
		e := events[i]
		if open != nil && leaveKinds[e.kind] {
			d := e.createdAt.Sub(*open)
			if d >= 0 {
				total += d
				pairs++
			}
			open = nil
			continue
		}
		if open == nil && (e.kind == "SLO_MEASURED" || e.kind == "FAILOVER_COMPLETED" || e.kind == "ROLLING_UPDATE_COMPLETED") {
			// After failover/upgrade, wait for a READY attestation before
			// counting serving again — FAILOVER_COMPLETED alone is not READY.
			if e.kind == "SLO_MEASURED" {
				t := e.createdAt
				open = &t
			}
		}
	}
	if pairs == 0 {
		if open != nil {
			return TaskPhase{Name: "steady_serving", Why: "READY observed but lease has not left serving; live span is not measured as complete"}
		}
		return TaskPhase{Name: "steady_serving", Why: "no closed READY→leave interval"}
	}
	return TaskPhase{
		Name: "steady_serving", Known: true, Duration: total,
		DurationMS: float64(total) / float64(time.Millisecond),
	}
}
