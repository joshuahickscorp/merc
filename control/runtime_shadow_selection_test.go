package main

import (
	"strings"
	"testing"

	"github.com/google/uuid"
)

// The shadow selector must see BOTH embed cells.
//
// This is the whole reason the selection is scored over the directed set rather
// than the routable one. Ordinary admission has already collapsed the routable
// set to a singleton before this runs — runtimeCapabilityForBindingDirected
// refuses unless exactly one cell matches — so a selector over that set would
// record the answer it was handed and call it a decision.
func TestShadowSelectionConsidersTheProvenCellAdmissionCannotRoute(t *testing.T) {
	withActivationRestored(t)

	decision, err := buildWorkloadDecision(jobSubmit{
		JobType:     JobType{Type: "embed"},
		Model:       ModelRef{Kind: "hf", Ref: "all-minilm-l6-v2"},
		Tier:        "batch",
		Constraints: JobConstraints{MaxDurationSecs: 3600},
	}, strings.Repeat("a", 64))
	if err != nil {
		t.Fatal(err)
	}
	if len(decision.RuntimeCandidates) != 1 {
		t.Fatalf("admission froze %d candidates; it is documented to freeze exactly one",
			len(decision.RuntimeCandidates))
	}
	if decision.RuntimeCandidates[0].CellID != candleEmbedCell {
		t.Fatalf("admission froze %q, want the routable candle cell",
			decision.RuntimeCandidates[0].CellID)
	}

	shadow, err := planShadowSelection(decision)
	if err != nil {
		t.Fatal(err)
	}

	considered := map[string]shadowCandidate{}
	for _, candidate := range shadow.Considered {
		considered[candidate.CellID] = candidate
	}
	t.Logf("considered=%v excluded=%v shadow=%s routed=%s",
		keysOf(considered), shadow.Excluded, shadow.ShadowCellID, shadow.RoutedCellID)

	// Both embed cells, and the difference between them is the point: one is
	// routable and one is proven-but-directed-only.
	if _, ok := considered[candleEmbedCell]; !ok {
		t.Error("the routable candle embed cell was not considered")
	}
	llama, ok := considered[llamaEmbedCell]
	if !ok {
		t.Fatal("the llama.cpp embed cell was not considered; the shadow selection " +
			"is scoring the routable set, which admission has already collapsed to one")
	}
	if llama.Routable {
		t.Error("the llama.cpp embed cell is reported routable; ordinary buyer work " +
			"must not be able to reach it")
	}
	if considered[candleEmbedCell].Routable != true {
		t.Error("the candle embed cell is reported non-routable")
	}

	// And the selection changes nothing about the job.
	if shadow.RoutedCellID != decision.RuntimeCandidates[0].CellID {
		t.Error("the shadow recorded a routed cell that is not the frozen one")
	}
}

// A rejected cell is excluded WITH the measurement that rejected it.
//
// "It chose the candle cell" is not reviewable. "It excluded the llama.cpp
// generation cell because that cell sells byte_exact and its determinism sweep
// found divergence from its own serial output in every batched configuration" is
// the only form of exclusion anyone can argue with.
func TestShadowSelectionRecordsWhyACellWasExcluded(t *testing.T) {
	withActivationRestored(t)

	// Ordinary admission freezes the bindable embed singleton. Shadow selection
	// then scores the directed set, where llama.cpp's embed cell is reachable
	// and its rejected generation cell must be excluded with the measurement.
	decision, err := buildWorkloadDecision(jobSubmit{
		JobType:     JobType{Type: "embed"},
		Model:       ModelRef{Kind: "hf", Ref: "all-minilm-l6-v2"},
		Tier:        "batch",
		Constraints: JobConstraints{MaxDurationSecs: 3600},
	}, strings.Repeat("b", 64))
	if err != nil {
		t.Fatal(err)
	}
	shadow, err := planShadowSelection(decision)
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("excluded: %+v", shadow.Excluded)

	// The rejected generation cell is not in the embed directed set; exclusion
	// of a peer that sells a different job is not required. What is required:
	// any cell shadow considers and rejects must name why. For embed the
	// interesting exclusion is the generation cell if it appears, otherwise we
	// assert the rejected cell is not silently considered as a candidate.
	for _, c := range shadow.Considered {
		if c.CellID == "llama-cpp-metal-llama1-infer" {
			t.Fatal("the REJECTED_FOR_CONTRACT generation cell was considered for embed work")
		}
	}
	// Directed embed peers may include llama.cpp embed; it must not be excluded
	// for a fabricated reason.
	for _, exclusion := range shadow.Excluded {
		if exclusion.CellID == "llama-cpp-metal-minilm-embed" && exclusion.Reason == "" {
			t.Fatal("llama embed excluded with an empty reason")
		}
	}
	// Re-check the generation cell on a directed batch_infer decision so the
	// REJECTED_FOR_CONTRACT exclusion remains covered.
	infer, err := buildWorkloadDecisionDirected(jobSubmit{
		JobType:     JobType{Type: "batch_infer", MaxTokens: 128},
		Model:       ModelRef{Kind: "gguf", Ref: "llama-3.2-1b-instruct-q4"},
		Tier:        "batch",
		Constraints: JobConstraints{MaxDurationSecs: 3600},
	}, strings.Repeat("b", 64), "candle-metal-llama1-infer")
	if err != nil {
		t.Fatal(err)
	}
	inferShadow, err := planShadowSelection(infer)
	if err != nil {
		t.Fatal(err)
	}
	var reason string
	for _, exclusion := range inferShadow.Excluded {
		if exclusion.CellID == "llama-cpp-metal-llama1-infer" {
			reason = exclusion.Reason
		}
	}
	if reason == "" {
		t.Fatal("the REJECTED_FOR_CONTRACT generation cell was not recorded as excluded")
	}
	if !strings.Contains(reason, "REJECTED_FOR_CONTRACT") {
		t.Errorf("exclusion reason %q does not name the decision", reason)
	}
	// The stated measurement, not a restatement of the state name.
	if !strings.Contains(reason, "byte_exact") && !strings.Contains(reason, "determinism") {
		t.Errorf("exclusion reason %q does not carry the measurement that decided it", reason)
	}
	if shadow.ShadowCellID != shadow.RoutedCellID {
		t.Errorf("the shadow diverged on a workload with one eligible cell: %s vs %s",
			shadow.ShadowCellID, shadow.RoutedCellID)
	}
}

// A tie must not manufacture a divergence.
//
// With no cost model there is nothing to rank two equally-proven cells by, and a
// tie broken alphabetically would record a divergence on roughly half the rows
// that reflects sort order rather than a judgement — a number that looks exactly
// like evidence and is not.
func TestShadowSelectionBreaksTiesTowardWhatAdmissionChose(t *testing.T) {
	tied := []shadowCandidate{
		{CellID: "aaa-cell", Lifecycle: runtimeLifecycleActive, QualityTier: "OUTCOME_EQUIVALENT"},
		{CellID: "zzz-cell", Lifecycle: runtimeLifecycleActive, QualityTier: "OUTCOME_EQUIVALENT"},
	}
	if got := chooseShadowCell(tied, "zzz-cell"); got != "zzz-cell" {
		t.Errorf("a tie chose %q over the routed cell; sort order became a divergence", got)
	}
	if got := chooseShadowCell(tied, "aaa-cell"); got != "aaa-cell" {
		t.Errorf("a tie chose %q over the routed cell", got)
	}

	// A genuinely more proven cell still wins over the routed one. Otherwise the
	// tie-break would swallow the only signal the selection can produce.
	better := []shadowCandidate{
		{CellID: "proven", Lifecycle: runtimeLifecycleActive, QualityTier: "OUTCOME_EQUIVALENT"},
		{CellID: "draft", Lifecycle: runtimeLifecycleDraft, QualityTier: "UNPROVEN"},
	}
	if got := chooseShadowCell(better, "draft"); got != "proven" {
		t.Errorf("the shadow kept the routed cell %q over a strictly more proven one", got)
	}
}

// The selection is recorded, immutable, and cannot refuse a submit.
func TestShadowSelectionIsRecordedAndImmutable(t *testing.T) {
	ctx, store, pool := openActivationStore(t)

	decision, err := buildWorkloadDecision(jobSubmit{
		JobType:     JobType{Type: "embed"},
		Model:       ModelRef{Kind: "hf", Ref: "all-minilm-l6-v2"},
		Tier:        "batch",
		Constraints: JobConstraints{MaxDurationSecs: 3600},
	}, strings.Repeat("c", 64))
	if err != nil {
		t.Fatal(err)
	}
	shadow, err := planShadowSelection(decision)
	if err != nil {
		t.Fatal(err)
	}
	jobID := uuid.NewString()
	if err := store.RecordShadowSelection(ctx, jobID, shadow); err != nil {
		t.Fatalf("record: %v", err)
	}
	// Idempotent: a retried submit must not produce a second opinion.
	if err := store.RecordShadowSelection(ctx, jobID, shadow); err != nil {
		t.Fatalf("second record: %v", err)
	}

	var rows int
	var considered, excluded string
	if err := pool.QueryRow(ctx, `
		SELECT COUNT(*) OVER (), considered_cells::text, excluded_cells::text
		  FROM runtime_shadow_selections WHERE job_id=$1`, jobID).
		Scan(&rows, &considered, &excluded); err != nil {
		t.Fatal(err)
	}
	if rows != 1 {
		t.Fatalf("%d rows for one job", rows)
	}
	if !strings.Contains(considered, llamaEmbedCell) {
		t.Errorf("the stored decision does not name the proven cell: %s", considered)
	}

	// Immutable. A decision is what was believed at a moment; rewriting one makes
	// every accuracy measurement taken from it meaningless.
	if _, err := pool.Exec(ctx,
		`UPDATE runtime_shadow_selections SET shadow_cell_id='rewritten' WHERE job_id=$1`,
		jobID); err == nil {
		t.Error("a recorded shadow selection was rewritten")
	}
	if _, err := pool.Exec(ctx,
		`DELETE FROM runtime_shadow_selections WHERE job_id=$1`, jobID); err == nil {
		t.Error("a recorded shadow selection was deleted")
	}

	divergence, err := store.ShadowSelectionDivergence(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(divergence) != 1 || divergence[0].Decisions != 1 {
		t.Fatalf("divergence report = %+v", divergence)
	}
	t.Logf("divergence: %+v", divergence[0])
}

func keysOf(m map[string]shadowCandidate) []string {
	out := make([]string, 0, len(m))
	for key := range m {
		out = append(out, key)
	}
	return out
}
