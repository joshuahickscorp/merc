package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/google/uuid"
)

func TestReplayShadowRegretAgainstLifecycleLadderIsObservationOnly(t *testing.T) {
	installSettlementCurrencyForTest(t, "usd")
	ctx, store, pool := openIsolatedTestStore(t)

	// Seed two shadow decisions: one where routed != ladder-best, one where equal.
	// Considered cells carry measured liability so cost/latency can score.
	candle := candleEmbedCell
	llama := llamaEmbedCell
	if candle == "" || llama == "" {
		// Fallback constants if the package uses different names.
		candle = "candle_metal/all-minilm-l6-v2"
		llama = "llama_cpp_metal/all-minilm-l6-v2"
	}

	jobA, jobB := uuid.New(), uuid.New()
	buyerID := uuid.New()
	if _, err := pool.Exec(ctx, `INSERT INTO buyers (id,email,free_credit_usd) VALUES ($1,$2,0)`,
		buyerID, buyerID.String()+"@replay.invalid"); err != nil {
		t.Fatal(err)
	}
	for _, jobID := range []uuid.UUID{jobA, jobB} {
		if _, err := pool.Exec(ctx, `
			INSERT INTO jobs (id,buyer_id,status,job_type,model_ref,input_ref,task_count,
			                  offered_rate_usd_hr,min_memory_gb,tier,currency)
			VALUES ($1,$2,'complete','embed','all-minilm-l6-v2','in',1,10.0,0,'batch','usd')`,
			jobID, buyerID); err != nil {
			t.Fatal(err)
		}
	}

	// ACTIVE outranks REAL_RUNTIME_PROVEN on the ladder. Route to the lower-rank
	// cell so lifecycle-ladder-v1 changes the selection.
	consideredHigh := []shadowCandidate{
		{CellID: candle, Lifecycle: runtimeLifecycleActive, QualityTier: "STANDARD",
			SupplierLiabilityMeasured: true, MedianMsPerUnit: 0.3,
			SupplierLiabilityUSDPerVerifiedUnit: 0.1},
		{CellID: llama, Lifecycle: runtimeLifecycleRealRuntimeProven, QualityTier: "STANDARD",
			SupplierLiabilityMeasured: true, MedianMsPerUnit: 0.5,
			SupplierLiabilityUSDPerVerifiedUnit: 0.2},
	}
	consideredTie := []shadowCandidate{
		{CellID: candle, Lifecycle: runtimeLifecycleActive, QualityTier: "STANDARD",
			SupplierLiabilityMeasured: true, MedianMsPerUnit: 0.4,
			SupplierLiabilityUSDPerVerifiedUnit: 0.15},
	}
	must(t, store.RecordShadowSelection(ctx, jobA.String(), ShadowSelection{
		RuntimeMatrixSHA: "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
		PolicyRevision:   1,
		JobType:          "embed",
		ModelRef:         "all-minilm-l6-v2",
		ModelKind:        "embedding",
		WorkloadClass:    "embed",
		LatencyClass:     "BATCH",
		RoutedCellID:     llama,
		ShadowCellID:     candle,
		Considered:       consideredHigh,
		Excluded:         []shadowExclusion{},
		SelectionPolicy:  shadowSelectionPolicy,
		SelectionBasis:   selectionBasisLadder,
		// execution_mode must satisfy runtime_shadow_selections_mode_known
		// ('' | POOL | REPLICA_SERVICE | LOCAL_CLUSTER | CLOUD_BACKSTOP).
		// Batch work reaches POOL by construction today.
		ExecutionMode:       string(ModePool),
		ExecutionModeReason: "test",
	}))
	// Second decision: singleton considered — ladder cannot change selection.
	// Directive XII first-replay: replaying the same historical policy
	// (lifecycle-ladder-v1) on a decision already ladder-optimal must
	// reproduce the historical winner. Counterfactual is PREDICTION only.
	must(t, store.RecordShadowSelection(ctx, jobB.String(), ShadowSelection{
		RuntimeMatrixSHA:    "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
		PolicyRevision:      1,
		JobType:             "embed",
		ModelRef:            "all-minilm-l6-v2",
		ModelKind:           "embedding",
		WorkloadClass:       "embed",
		LatencyClass:        "BATCH",
		RoutedCellID:        candle,
		ShadowCellID:        candle,
		Considered:          consideredTie,
		Excluded:            []shadowExclusion{},
		SelectionPolicy:     shadowSelectionPolicy,
		SelectionBasis:      selectionBasisLadder,
		ExecutionMode:       string(ModePool),
		ExecutionModeReason: "test",
	}))

	report, err := store.BuildReplayRegretArtifact(ctx, "bff5fd33b2547218eb31a953ba34ac3d58f6ece1")
	must(t, err)
	if report.SourceCommit == "" {
		t.Fatal("source_commit required")
	}
	if report.Policy != replayPolicyLifecycleLadder {
		t.Fatalf("policy = %q", report.Policy)
	}
	if report.Decisions != 2 {
		t.Fatalf("decisions = %d, want 2", report.Decisions)
	}
	if report.SelectionsChanged < 1 {
		t.Fatalf("expected at least one changed selection under ladder, got %d", report.SelectionsChanged)
	}
	// Directive XII: first proof of the replay machinery is reproduction of the
	// historical winner under the same policy — not only counterfactual regret.
	if report.UnchangedSelections < 1 {
		t.Fatalf("Directive XII first-replay: replaying lifecycle-ladder-v1 must reproduce "+
			"at least one historical winner (jobB singleton ladder-optimal), unchanged=%d",
			report.UnchangedSelections)
	}
	if report.SLARegret.Status != "REFUSED" || report.LocalityRegret.Status != "REFUSED" {
		t.Fatalf("SLA/locality must be refused, got sla=%+v locality=%+v",
			report.SLARegret, report.LocalityRegret)
	}
	if report.MoneyAuthority != "OBSERVATION_ONLY_NO_SELECTION_AUTHORITY" {
		t.Fatalf("money authority = %q", report.MoneyAuthority)
	}
	if report.LatencyRegret.Scored < 1 {
		t.Fatalf("latency regret should score the measured pair, scored=%d unmeasured=%d",
			report.LatencyRegret.Scored, report.LatencyRegret.Unmeasured)
	}
	// Positive latency regret: routed slower than ladder alternative (0.5 - 0.3).
	if report.LatencyRegret.TotalRegretMSPerUnit <= 0 {
		t.Fatalf("expected positive latency regret (routed slower), got %v",
			report.LatencyRegret.TotalRegretMSPerUnit)
	}
	if report.CostRegret.TotalLiabilityRegretPerUnit <= 0 {
		t.Fatalf("expected positive cost regret (routed more expensive), got %v",
			report.CostRegret.TotalLiabilityRegretPerUnit)
	}

	// Serialize the report to a temp path. The sealed LFS evidence body under
	// evidence/perf/g021-g053-per-phase-prediction-regret.json is ledger-bound;
	// rewriting it from the suite races TestLFSCorpusIntegrity (timestamp
	// churn → resolved-payload mismatch). Operators regenerate the sealed
	// artifact deliberately outside the suite.
	report.GeneratedAt = time.Now().UTC().Format(time.RFC3339)
	body, err := json.MarshalIndent(report, "", "  ")
	must(t, err)
	path := filepath.Join(t.TempDir(), "g021-g053-per-phase-prediction-regret.json")
	if err := os.WriteFile(path, body, 0o644); err != nil {
		t.Fatal(err)
	}
	t.Logf("wrote %s (%d bytes)", path, len(body))
}
