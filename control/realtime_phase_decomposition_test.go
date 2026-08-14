package main

import (
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
)

func TestRealtimeTTFTPhaseCaptureObservesBoundariesAndRefusesPrefill(t *testing.T) {
	started := time.Now().Add(-50 * time.Millisecond)
	c := newRealtimeTTFTPhaseCapture(started)
	c.dialStart = started.Add(10 * time.Millisecond)
	c.dialSet = true
	c.headersAt = started.Add(25 * time.Millisecond)
	c.headersSet = true
	c.firstEventAt = started.Add(45 * time.Millisecond)
	c.firstEventSet = true

	var ev RealtimeExecutionEvidence
	c.apply(&ev)

	if ev.QueueWaitMS == nil || *ev.QueueWaitMS != 10 {
		t.Fatalf("queue_wait = %v, want 10", ev.QueueWaitMS)
	}
	if ev.ProviderStartupMS == nil || *ev.ProviderStartupMS != 15 {
		t.Fatalf("provider_startup = %v, want 15", ev.ProviderStartupMS)
	}
	if ev.EngineToFirstEventMS == nil || *ev.EngineToFirstEventMS != 20 {
		t.Fatalf("engine_to_first_event = %v, want 20", ev.EngineToFirstEventMS)
	}
	if ev.PrefillMS != nil {
		t.Fatal("prefill must stay nil — not separable on OpenAI-compatible streaming")
	}
}

func TestRealtimeTTFTPhaseCapturePartialFailureLeavesUnknown(t *testing.T) {
	started := time.Now()
	c := newRealtimeTTFTPhaseCapture(started)
	c.markDialStart()
	var ev RealtimeExecutionEvidence
	c.apply(&ev)
	if ev.QueueWaitMS == nil {
		t.Fatal("queue_wait should be known after dial start")
	}
	if ev.ProviderStartupMS != nil || ev.EngineToFirstEventMS != nil || ev.PrefillMS != nil {
		t.Fatalf("unobserved phases must be nil, got startup=%v engine=%v prefill=%v",
			ev.ProviderStartupMS, ev.EngineToFirstEventMS, ev.PrefillMS)
	}
}

func TestDecomposeRealtimePhasesReadsColumnsAndRefusesPrefill(t *testing.T) {
	installSettlementCurrencyForTest(t, "usd")
	ctx, store, pool := openIsolatedTestStore(t)
	t.Setenv("MERC_TOKEN_KEY", "rt-phase-key-at-least-32-bytes-long!!")
	resetRealtimeClearingState(t, ctx, pool)

	buyerID, err := store.CreateBuyerAccount(ctx,
		"rt-phase-"+uuid.NewString()+"@example.test", "integration-password", 5)
	must(t, err)
	profile := sortedVLLMProfiles()[0]
	worker := newRealtimeClearingOffer(t, ctx, store, pool, profile, "HOT", 0.08, 0.30, 4)

	contract, _, err := store.AuthorizeRealtimeContract(ctx, RealtimeContractAuthorization{
		RequestID: "rt-phase-" + uuid.NewString(), BuyerID: buyerID, Profile: profile,
		InputCommitment: strings.Repeat("a", 64), RequestSHA256: strings.Repeat("b", 64),
		MaximumPriceUSD: 0.001, EstimatedPriceUSD: 0.0005, DeadlineAt: time.Now().Add(time.Minute),
		MaximumPromptTokens: 8_330, MaximumCompletionTokens: 1,
		EstimatedPromptTokens: 4_163, EstimatedCompletionTokens: 1, BuyerDeclaredCeilingUSD: 0.0011,
	})
	mustf(t, err, "authorize: %v")

	qw, ps, eng := int64(12), int64(34), int64(56)
	evidence := RealtimeExecutionEvidence{
		ID: uuid.New(), HTTPStatus: 200, StreamEventCount: 1,
		StreamRootSHA256: strings.Repeat("1", 64), OutputCommitment: strings.Repeat("2", 64),
		PromptTokens: 1, CompletionTokens: 1, TotalTokens: 2,
		TimeToFirstEventMS: 102, DurationMS: 200,
		QueueWaitMS: &qw, ProviderStartupMS: &ps, EngineToFirstEventMS: &eng,
		// PrefillMS deliberately nil
	}
	if _, err := store.FinalizeRealtimeSuccess(ctx, contract.ID, evidence); err != nil {
		t.Fatalf("finalize: %v", err)
	}
	_ = worker

	got, err := DecomposeRealtimePhases(ctx, pool, evidence.ID)
	must(t, err)
	if !got.QueueWait.Known || got.QueueWait.DurationMS != 12 {
		t.Fatalf("queue_wait = %+v", got.QueueWait)
	}
	if !got.ProviderStartup.Known || got.ProviderStartup.DurationMS != 34 {
		t.Fatalf("provider_startup = %+v", got.ProviderStartup)
	}
	if !got.EngineToFirstEvent.Known || got.EngineToFirstEvent.DurationMS != 56 {
		t.Fatalf("engine_to_first_event = %+v", got.EngineToFirstEvent)
	}
	if got.Prefill.Known {
		t.Fatalf("prefill must be unknown, got %+v", got.Prefill)
	}
	if got.Prefill.Why == "" {
		t.Fatal("prefill refusal must name the protocol boundary")
	}

	if err := store.RecordRealtimePhaseCalibrations(ctx, evidence.ID); err != nil {
		t.Fatal(err)
	}
	var nPred, nAct int
	if err := pool.QueryRow(ctx, `
		SELECT COUNT(*) FILTER (WHERE predicted_ms IS NOT NULL),
		       COUNT(*) FILTER (WHERE realized_ms IS NOT NULL)
		  FROM eta_calibration
		 WHERE subject_kind='execution_contract' AND subject_id=$1`,
		contract.ID).Scan(&nPred, &nAct); err != nil {
		t.Fatal(err)
	}
	if nPred != 0 {
		t.Fatalf("phase calibrations must not invent predictions, got %d", nPred)
	}
	if nAct != 3 {
		t.Fatalf("want 3 actual-only phase rows, got %d", nAct)
	}
}

func TestPhaseRegretMSRefusesMissingPrediction(t *testing.T) {
	if _, ok := PhaseRegretMS(0, 10, false, true); ok {
		t.Fatal("regret against missing prediction must be refused")
	}
	if _, ok := PhaseRegretMS(5, 0, true, false); ok {
		t.Fatal("regret against missing actual must be refused")
	}
	r, ok := PhaseRegretMS(5, 12, true, true)
	if !ok || r != 7 {
		t.Fatalf("got regret=%v ok=%v, want 7 true", r, ok)
	}
}

func TestETABiasFactorIgnoresNonTotalPhases(t *testing.T) {
	installSettlementCurrencyForTest(t, "usd")
	ctx, store, pool := openIsolatedTestStore(t)

	// Seed a total-duration calibration that would stretch (realized >> predicted).
	jobType, tier, model := "embed", "batch", "phase-bias-model"
	for i := 0; i < driftMinSamples; i++ {
		if _, err := pool.Exec(ctx, `
			INSERT INTO eta_calibration
			  (job_id, job_type, tier, model_ref, phase, predicted_secs, realized_secs)
			VALUES ($1,$2,$3,$4,'total',10,30)`,
			uuid.New(), jobType, tier, model); err != nil {
			t.Fatal(err)
		}
	}
	// Pathological phase rows that would destroy the factor if mis-read.
	for i := 0; i < driftMinSamples; i++ {
		if _, err := pool.Exec(ctx, `
			INSERT INTO eta_calibration
			  (job_type, tier, model_ref, phase, subject_kind, subject_id,
			   predicted_ms, realized_ms)
			VALUES ($1,$2,$3,'queue','task',$4,1,1e9)`,
			jobType, tier, model, uuid.New()); err != nil {
			t.Fatal(err)
		}
	}
	factor, samples, err := store.ETABiasFactor(ctx, jobType, tier, model, "")
	must(t, err)
	if samples < driftMinSamples {
		t.Fatalf("samples=%d, want at least %d from total rows only", samples, driftMinSamples)
	}
	// 30/10 = 3.0, clamped to etaBiasFactorMax.
	if factor < 2.9 || factor > etaBiasFactorMax {
		t.Fatalf("factor=%v from total rows; phase rows must not participate", factor)
	}
}
