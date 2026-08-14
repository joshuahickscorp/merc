package main

import (
	"context"
	"errors"
	"fmt"

	"github.com/google/uuid"
)

// RealtimePhaseDecomposition is the G021 actuals half for one verified (or
// failed-with-partial) realtime execution. It reads only what
// realtime_executions stores — it never invents a duration.
//
// Prefill is always reported unknown under the OpenAI-compatible streaming
// protocol: the first SSE event is already post-prefill first token, so there
// is no wire boundary between prefill and first decode. That refusal is the
// answer, not a missing feature.
type RealtimePhaseDecomposition struct {
	ContractID  uuid.UUID `json:"contract_id"`
	ExecutionID uuid.UUID `json:"execution_id"`

	// TimeToFirstEventMS is the legacy wall (unchanged). Kept beside the split
	// so a reader can check the split against the total without re-querying.
	TimeToFirstEventMS int64 `json:"time_to_first_event_ms"`
	DurationMS         int64 `json:"duration_ms"`

	QueueWait          TaskPhase `json:"queue_wait"`
	ProviderStartup    TaskPhase `json:"provider_startup"`
	Prefill            TaskPhase `json:"prefill"`
	EngineToFirstEvent TaskPhase `json:"engine_to_first_event"`
}

// KnownPhases returns only measured spans. Prefill is never among them under
// the current protocol.
func (d RealtimePhaseDecomposition) KnownPhases() []TaskPhase {
	var out []TaskPhase
	for _, p := range []TaskPhase{d.QueueWait, d.ProviderStartup, d.Prefill, d.EngineToFirstEvent} {
		if p.Known {
			out = append(out, p)
		}
	}
	return out
}

func phaseFromNullableMS(name string, ms *int64, whyIfNil string) TaskPhase {
	if ms == nil {
		return TaskPhase{Name: name, Why: whyIfNil}
	}
	if *ms < 0 {
		return TaskPhase{Name: name, Why: fmt.Sprintf("%s is negative (%d); span is not measurable", name, *ms)}
	}
	return TaskPhase{
		Name: name, Known: true,
		DurationMS: float64(*ms),
	}
}

// DecomposeRealtimePhases reads one execution's observed TTFT split. It never
// writes, and never fabricates a prefill duration from time_to_first_event_ms.
func DecomposeRealtimePhases(ctx context.Context, db ledgerExec, executionID uuid.UUID) (RealtimePhaseDecomposition, error) {
	if executionID == uuid.Nil {
		return RealtimePhaseDecomposition{}, errors.New("realtime phase decomposition requires an execution id")
	}
	var (
		out                             RealtimePhaseDecomposition
		queue, startup, prefill, engine *int64
		ttfe, duration                  int64
		contractID                      uuid.UUID
	)
	err := db.QueryRow(ctx, `
		SELECT contract_id, COALESCE(time_to_first_event_ms, 0), duration_ms,
		       queue_wait_ms, provider_startup_ms, prefill_ms, engine_to_first_event_ms
		  FROM realtime_executions
		 WHERE id = $1`, executionID,
	).Scan(&contractID, &ttfe, &duration, &queue, &startup, &prefill, &engine)
	if err != nil {
		return RealtimePhaseDecomposition{}, fmt.Errorf("read realtime execution %s phases: %w", executionID, err)
	}
	out.ContractID = contractID
	out.ExecutionID = executionID
	out.TimeToFirstEventMS = ttfe
	out.DurationMS = duration
	out.QueueWait = phaseFromNullableMS("queue_wait", queue,
		"queue_wait_ms unset — dial start was not observed (failure before upstream, exact-reuse, or pre-G021 row)")
	out.ProviderStartup = phaseFromNullableMS("provider_startup", startup,
		"provider_startup_ms unset — response headers were not observed")
	// Prefill: even if a future protocol writes the column, a NULL still means
	// unobserved. Today production never writes it.
	out.Prefill = phaseFromNullableMS("prefill", prefill,
		"prefill is not separable on the OpenAI-compatible streaming protocol: first SSE event is post-prefill first token; no wire boundary exists")
	out.EngineToFirstEvent = phaseFromNullableMS("engine_to_first_event", engine,
		"engine_to_first_event_ms unset — first upstream event was not observed after headers")
	return out, nil
}
