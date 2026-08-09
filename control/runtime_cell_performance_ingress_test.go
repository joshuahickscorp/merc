package main

import (
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
)

func TestSubmitJobTxRefusesCurrentPerformanceUnitMismatchWithoutWriting(t *testing.T) {
	ctx, store, pool := openIsolatedTestStore(t)
	workload, compute, placement, economic, pricing := distributedPricingFixture(t)
	if placement.PerformanceAuthority == nil {
		t.Fatal("current pricing fixture lacks frozen performance authority")
	}
	if got := placement.PerformanceAuthority.Performance.UnitScope; got != performanceUnitScopeTokenLikeInputPlusOutputTokens {
		t.Fatalf("durable-ingress premise changed: TEST_ONLY batch fixture scope=%q", got)
	}

	// Model a quote whose frozen performance unit is no longer compatible with
	// the unit settled by its current job type. Submit must catch this before the
	// ordinary frozen-digest checks and, critically, before starting any write.
	frozen := *placement.PerformanceAuthority
	frozen.Performance = placement.PerformanceAuthority.Performance
	frozen.Performance.HardwareClasses = append([]string(nil),
		placement.PerformanceAuthority.Performance.HardwareClasses...)
	frozen.Performance.Unit = "embeddings"
	measurement := frozen.BenchmarkSnapshot.Throughput[frozen.Performance.RuntimeProfileID]
	measurement.Unit = "embeddings"
	frozen.BenchmarkSnapshot.Throughput[frozen.Performance.RuntimeProfileID] = measurement
	var err error
	frozen.BenchmarkSnapshotSHA256, err = benchmarkReceiptSummarySHA256(frozen.BenchmarkSnapshot)
	must(t, err)
	frozen.Digest, err = frozenRuntimeCellPerformanceDigest(frozen)
	must(t, err)
	placement.PerformanceAuthority = &frozen

	jobID := uuid.New()
	job := &jobRow{
		ID:                   jobID,
		WorkloadDecision:     workload,
		ComputePlan:          compute,
		PlacementRequirement: placement,
		EconomicPlan:         economic,
		PricingDecision:      pricing,
	}
	err = store.SubmitJobTx(ctx, job, nil)
	if err == nil || !strings.Contains(err.Error(), "current placement authority") ||
		!strings.Contains(err.Error(), `"embeddings"`) ||
		!strings.Contains(err.Error(), `"tokens"`) {
		t.Fatalf("durable ingress did not refuse the unit mismatch at its own boundary: %v", err)
	}

	var rows int
	mustf(t, pool.QueryRow(ctx, `SELECT count(*) FROM jobs WHERE id=$1`, jobID).Scan(&rows),
		"count refused job writes: %v")
	if rows != 0 {
		t.Fatalf("unit-mismatched submission wrote %d job rows", rows)
	}
}

func TestSubmitJobTxRefusesSupersededMatrixWithoutWriting(t *testing.T) {
	ctx, store, pool := openIsolatedTestStore(t)
	workload, compute, placement, economic, pricing := distributedPricingFixture(t)
	placement.RuntimeMatrixSHA256 = strings.Repeat("f", 64)
	if placement.RuntimeMatrixSHA256 == generatedRuntimeMatrixSHA256 {
		placement.RuntimeMatrixSHA256 = strings.Repeat("e", 64)
	}

	// The frozen pair remains internally readable: the value is a historical
	// capability identity, not an alias for the currently compiled matrix.
	mustf(t, validatePlacementRequirement(placement, workload),
		"historical placement inherited the current matrix: %v")

	jobID := uuid.New()
	err := store.SubmitJobTx(ctx, &jobRow{
		ID:                   jobID,
		WorkloadDecision:     workload,
		ComputePlan:          compute,
		PlacementRequirement: placement,
		EconomicPlan:         economic,
		PricingDecision:      pricing,
	}, nil)
	if err == nil || !strings.Contains(err.Error(), "current capability matrix") ||
		!strings.Contains(err.Error(), "historical-only") {
		t.Fatalf("durable ingress accepted a superseded matrix: %v", err)
	}

	var rows int
	mustf(t, pool.QueryRow(ctx, `SELECT count(*) FROM jobs WHERE id=$1`, jobID).Scan(&rows),
		"count superseded-matrix job writes: %v")
	if rows != 0 {
		t.Fatalf("superseded-matrix submission wrote %d job rows", rows)
	}
}

func TestSubmitJobTxRechecksPerformanceAcrossFreshnessBoundaryWithoutWriting(t *testing.T) {
	ctx, store, pool := openIsolatedMoneyPathStore(t)
	f := seedMoneyPathFixture(t, ctx, store, pool, moneyPathSeedOpts{TaskCount: 1})
	tasks := makeTasks(f, 1)
	f.TaskIDs = []uuid.UUID{tasks[0].ID}
	job := validJobRow(t, f, tasks)
	if job.PlacementRequirement.PerformanceAuthority == nil {
		t.Fatal("current job fixture lacks frozen performance authority")
	}
	frozen := job.PlacementRequirement.PerformanceAuthority.Performance
	if frozen.Status != cellThroughputMeasured || frozen.Haircut != measuredThroughputHaircut {
		t.Fatalf("clock-boundary fixture did not freeze the fresh posture: %+v", frozen)
	}
	measuredAt, err := time.Parse(time.RFC3339, frozen.BenchmarkedAt)
	mustf(t, err, "parse frozen benchmark time: %v")
	afterBoundary := measuredAt.Add(benchmarkRevalidationWindow + time.Nanosecond)

	// Prove the only changed input is the policy clock and that it changes the
	// monetary projection, not merely a diagnostic string.
	profile, cell := cellByID(t, frozen.CellID)
	current := resolveCellPerformance(profile, cell, afterBoundary)
	if current.Status != cellThroughputStale ||
		current.Haircut != staleThroughputHaircut ||
		current.ConservativeUnitsPerSec != frozen.ObservedUnitsPerSec*staleThroughputHaircut {
		t.Fatalf("post-boundary authority did not resolve the stale projection: %+v", current)
	}

	savedClock := runtimeCellPerformanceNow
	runtimeCellPerformanceNow = func() time.Time { return afterBoundary }
	t.Cleanup(func() { runtimeCellPerformanceNow = savedClock })
	err = store.SubmitJobTx(ctx, job, tasks)
	if err == nil || !strings.Contains(err.Error(), "frozen performance projection") ||
		!strings.Contains(err.Error(), cellThroughputMeasured) ||
		!strings.Contains(err.Error(), cellThroughputStale) {
		t.Fatalf("durable ingress retained the pre-boundary haircut: %v", err)
	}

	var jobs, taskRows, plans int
	mustf(t, pool.QueryRow(ctx, `SELECT count(*) FROM jobs WHERE id=$1`, f.JobID).Scan(&jobs),
		"count clock-refused job writes: %v")
	mustf(t, pool.QueryRow(ctx, `SELECT count(*) FROM tasks WHERE job_id=$1`, f.JobID).Scan(&taskRows),
		"count clock-refused task writes: %v")
	mustf(t, pool.QueryRow(ctx, `SELECT count(*) FROM job_economic_plans WHERE job_id=$1`, f.JobID).Scan(&plans),
		"count clock-refused plan writes: %v")
	if jobs != 0 || taskRows != 0 || plans != 0 {
		t.Fatalf("clock-refused submission wrote jobs/tasks/plans=%d/%d/%d", jobs, taskRows, plans)
	}
}

func TestSubmitJobTxRefusesFutureDatedCurrentPerformanceWithoutWriting(t *testing.T) {
	// Sole current durable-admission fixture, then rewind the performance clock so
	// the frozen MEASURED authority becomes future-dated relative to "now".
	ctx, store, pool, f, job, tasks, _ := currentUniformMoneyPathJob(t)
	if job.PlacementRequirement.PerformanceAuthority == nil {
		t.Fatal("current job fixture lacks frozen performance authority")
	}
	frozen := job.PlacementRequirement.PerformanceAuthority.Performance
	measuredAt, err := time.Parse(time.RFC3339, frozen.BenchmarkedAt)
	mustf(t, err, "parse frozen benchmark time: %v")

	savedClock := runtimeCellPerformanceNow
	runtimeCellPerformanceNow = func() time.Time { return measuredAt.Add(-time.Nanosecond) }
	t.Cleanup(func() { runtimeCellPerformanceNow = savedClock })
	err = store.SubmitJobTx(ctx, job, tasks)
	// Durable ingress must refuse before any write. The comparison surfaces either
	// the explicit future-dated reason or a projection mismatch against the
	// unproven current authority that future-dating produces.
	if err == nil ||
		(!strings.Contains(err.Error(), "future-dated") &&
			!strings.Contains(err.Error(), "no longer matches current authority")) {
		t.Fatalf("durable ingress accepted future-dated current performance: %v", err)
	}

	var jobs, taskRows, plans int
	mustf(t, pool.QueryRow(ctx, `SELECT count(*) FROM jobs WHERE id=$1`, f.JobID).Scan(&jobs),
		"count future-dated job writes: %v")
	mustf(t, pool.QueryRow(ctx, `SELECT count(*) FROM tasks WHERE job_id=$1`, f.JobID).Scan(&taskRows),
		"count future-dated task writes: %v")
	mustf(t, pool.QueryRow(ctx, `SELECT count(*) FROM job_economic_plans WHERE job_id=$1`, f.JobID).Scan(&plans),
		"count future-dated plan writes: %v")
	if jobs != 0 || taskRows != 0 || plans != 0 {
		t.Fatalf("future-dated submission wrote jobs/tasks/plans=%d/%d/%d", jobs, taskRows, plans)
	}
}

func TestHistoricalOldMatrixJobRemainsReadableAndAccruesRiskReserve(t *testing.T) {
	ctx, store, pool, f, job, tasks, _ := currentUniformMoneyPathJob(t)
	if job.PricingDecision.RiskReserve.Status != pricingCostModeled ||
		job.PricingDecision.RiskReserve.Amount <= 0 {
		t.Fatalf("historical completion fixture lacks modeled risk reserve: %+v",
			job.PricingDecision.RiskReserve)
	}
	mustf(t, store.SubmitJobTx(ctx, job, tasks), "submit pre-revision job: %v")
	acceptedMatrix := job.PlacementRequirement.RuntimeMatrixSHA256
	if job.PricingDecision.CostPolicy == nil {
		t.Fatal("historical completion fixture lacks a frozen cost policy")
	}
	acceptedRetention := job.PricingDecision.CostPolicy.RetentionSeconds

	// Advance the binary's current matrix and cost configuration after
	// acceptance. The stored placement and pricing digests retain the accepted
	// authorities and must remain replayable without reading today's defaults.
	savedMatrix := generatedRuntimeMatrixSHA256
	generatedRuntimeMatrixSHA256 = strings.Repeat("f", 64)
	if generatedRuntimeMatrixSHA256 == acceptedMatrix {
		generatedRuntimeMatrixSHA256 = strings.Repeat("e", 64)
	}
	t.Cleanup(func() { generatedRuntimeMatrixSHA256 = savedMatrix })
	t.Setenv(costScheduleRevisionEnv, "cost-schedule-future")
	t.Setenv("MERC_JOB_OBJECT_RETENTION_DAYS", "8")

	placement, err := store.JobPlacementRequirement(ctx, f.JobID)
	mustf(t, err, "read historical placement after matrix revision: %v")
	if placement == nil || placement.RuntimeMatrixSHA256 != acceptedMatrix {
		t.Fatalf("historical placement identity changed after matrix revision: %+v", placement)
	}
	pricing, err := store.JobPricingDecision(ctx, f.JobID)
	mustf(t, err, "replay historical pricing after matrix revision: %v")
	if pricing == nil || pricing.RiskReserve.Amount != job.PricingDecision.RiskReserve.Amount ||
		pricing.CostPolicy == nil || pricing.CostPolicy.RetentionSeconds != acceptedRetention {
		t.Fatalf("historical pricing changed after matrix revision: %+v", pricing)
	}

	var settledChargeNanos int64
	currency := MustParseCurrency(job.PricingDecision.Currency)
	for _, task := range tasks {
		var taskChargeNanos int64
		mustf(t, pool.QueryRow(ctx, `
			SELECT economic_buyer_charge_nanos FROM tasks WHERE id=$1`, task.ID).
			Scan(&taskChargeNanos), "read historical task charge authority: %v")
		chargeMicros, err := LedgerMicrosFromNanos(MoneyNanos{
			Currency: currency,
			Nanos:    taskChargeNanos,
		})
		mustf(t, err, "project historical task charge into ledger micros: %v")
		if chargeMicros <= 0 {
			t.Fatalf("historical task %s has non-positive settled charge %d micros", task.ID, chargeMicros)
		}
		_, err = insertLedgerEntryOnTaskConflictDoNothingTx(ctx, pool, ledgerInsert{
			Kind: KindBuyerCharge, BuyerID: &f.BuyerID, TaskID: &task.ID,
			AmountMicros: -chargeMicros, Currency: currency.Code(), PayoutStatus: PayoutReleased,
		})
		mustf(t, err, "seed historical task buyer-charge settlement: %v")
		settledChargeNanos += chargeMicros * NanosPerMicro
	}
	for _, status := range []string{"running", "verifying", "complete"} {
		for _, task := range tasks {
			if _, err := pool.Exec(ctx,
				`UPDATE tasks SET status=$2,completed_at=CASE WHEN $2='complete' THEN now() ELSE completed_at END WHERE id=$1`,
				task.ID, status); err != nil {
				t.Fatalf("transition historical task %s to %s: %v", task.ID, status, err)
			}
		}
	}
	for _, status := range []string{"running", "verifying"} {
		if _, err := pool.Exec(ctx,
			`UPDATE jobs SET status=$2 WHERE id=$1`, f.JobID, status); err != nil {
			t.Fatalf("transition historical job to %s: %v", status, err)
		}
	}
	mustf(t, store.FinalizeJobTx(ctx, f.JobID),
		"finalize historical job after matrix revision: %v")

	var status string
	var accrualRows int
	var accrualMicros int64
	mustf(t, pool.QueryRow(ctx, `SELECT status FROM jobs WHERE id=$1`, f.JobID).Scan(&status),
		"read historical completion status: %v")
	mustf(t, pool.QueryRow(ctx, `
		SELECT count(*),COALESCE(sum((amount_usd * 1000000)::bigint),0)::bigint
		  FROM ledger_entries
		 WHERE kind=$1 AND payout_ref=$2`,
		KindRiskReserveAccrual, riskReserveAccrualRef(f.JobID)).Scan(&accrualRows, &accrualMicros),
		"read historical risk-reserve accrual: %v")
	wantRiskNanos, err := riskReserveNanos(
		job.PricingDecision.CostPolicy.Schedule, settledChargeNanos)
	mustf(t, err, "derive historical reserve from actual settlement: %v")
	wantLedgerMicros, err := riskReserveLedgerMicrosForAccrual(wantRiskNanos)
	mustf(t, err, "project historical reserve into ledger micros: %v")
	reserve, err := store.RiskReserveSnapshot(ctx, f.JobID)
	mustf(t, err, "read canonical historical risk reserve: %v")
	if status != "complete" || accrualRows != 1 || accrualMicros != wantLedgerMicros ||
		reserve == nil || reserve.SettledChargeNanos != settledChargeNanos ||
		reserve.AccruedNanos != wantRiskNanos || reserve.HeldNanos != wantRiskNanos {
		t.Fatalf("historical completion/reserve status=%q rows=%d ledger_micros=%d snapshot=%+v, want complete/1/%d settled=%d accrued=held=%d",
			status, accrualRows, accrualMicros, reserve, wantLedgerMicros,
			settledChargeNanos, wantRiskNanos)
	}
}

func TestFinalizeJobTxRefusesUnreadableModernPricingInsteadOfSkippingRiskReserve(t *testing.T) {
	ctx, store, pool, f, job, tasks, _ := currentUniformMoneyPathJob(t)
	if job.PricingDecision.RiskReserve.Status != pricingCostModeled ||
		job.PricingDecision.RiskReserve.Amount <= 0 {
		t.Fatalf("modern pricing fixture lacks modeled risk reserve: %+v",
			job.PricingDecision.RiskReserve)
	}
	mustf(t, store.SubmitJobTx(ctx, job, tasks), "submit modern pricing job: %v")

	for _, task := range tasks {
		for _, status := range []string{"running", "verifying", "complete"} {
			_, err := pool.Exec(ctx,
				`UPDATE tasks SET status=$2,completed_at=CASE WHEN $2='complete' THEN now() ELSE completed_at END WHERE id=$1`,
				task.ID, status)
			mustf(t, err, "transition modern task to %s: %v", status)
		}
	}
	for _, status := range []string{"running", "verifying"} {
		_, err := pool.Exec(ctx, `UPDATE jobs SET status=$2 WHERE id=$1`, f.JobID, status)
		mustf(t, err, "transition modern job to %s: %v", status)
	}

	// Simulate durable corruption beneath the normal immutable-authority trigger.
	// The JSON remains present and modern; its digest no longer authenticates it.
	// Completion must roll back, not treat this read error like a legacy NULL.
	_, err := pool.Exec(ctx, `ALTER TABLE jobs DISABLE TRIGGER jobs_pricing_authority_immutable`)
	mustf(t, err, "disable pricing immutability for corruption fixture: %v")
	t.Cleanup(func() {
		_, _ = pool.Exec(ctx, `ALTER TABLE jobs ENABLE TRIGGER jobs_pricing_authority_immutable`)
	})
	acceptedDigest, err := pricingDecisionDigest(job.PricingDecision)
	mustf(t, err, "digest accepted pricing fixture: %v")
	badDigest := strings.Repeat("f", 64)
	if badDigest == acceptedDigest {
		badDigest = strings.Repeat("e", 64)
	}
	_, err = pool.Exec(ctx,
		`UPDATE jobs SET pricing_decision_sha256=$2 WHERE id=$1`, f.JobID, badDigest)
	mustf(t, err, "corrupt frozen pricing digest: %v")
	_, err = pool.Exec(ctx, `ALTER TABLE jobs ENABLE TRIGGER jobs_pricing_authority_immutable`)
	mustf(t, err, "restore pricing immutability after corruption fixture: %v")

	err = store.FinalizeJobTx(ctx, f.JobID)
	if err == nil || !strings.Contains(err.Error(), "load frozen pricing authority") ||
		!strings.Contains(err.Error(), "digest mismatch") {
		t.Fatalf("finalization swallowed modern pricing-authority failure: %v", err)
	}

	var status string
	var reserveRows int
	mustf(t, pool.QueryRow(ctx, `SELECT status FROM jobs WHERE id=$1`, f.JobID).Scan(&status),
		"read refused finalization status: %v")
	mustf(t, pool.QueryRow(ctx, `
		SELECT count(*) FROM ledger_entries
		 WHERE kind=$1 AND payout_ref=$2`,
		KindRiskReserveAccrual, riskReserveAccrualRef(f.JobID)).Scan(&reserveRows),
		"count refused risk-reserve accruals: %v")
	if status != "verifying" || reserveRows != 0 {
		t.Fatalf("unreadable pricing finalized/skipped risk authority: status=%q reserve rows=%d",
			status, reserveRows)
	}
}

func TestSubmitJobTxRefusesProductionBatchDecodeScopeWithoutWriting(t *testing.T) {
	// Keep the legacy production decode-only unit/scope while minting a synthetic
	// exact identity so the cell is reachable. The refusal under test is still
	// the unit/scope mismatch against combined-token settlement.
	installTestOnlyExactIdentityForLegacyBenchmark(t, "candle-metal-llama1-infer")
	ctx, store, pool := openIsolatedTestStore(t)
	// Refresh empty overlay after Migrate so the exact-identity cell stays
	// advertised for normalize/build.
	installed := currentActivation()
	activeRuntimeActivation.Store(newRuntimeActivation(
		installed.PolicyRevision, map[string]string{}, nil))
	sub, herr := normalizeAndValidateJobSubmit(jobSubmit{
		JobType: JobType{Type: "batch_infer", MaxTokens: 16},
		Model:   ModelRef{Kind: "gguf", Ref: "llama-3.2-1b-instruct-q4"},
		Constraints: JobConstraints{
			MaxDurationSecs: 3600,
		},
		Tier: "batch",
	})
	if herr != nil {
		t.Fatalf("normalize production batch ingress fixture: %s", herr.msg)
	}
	workload, err := buildWorkloadDecision(sub, strings.Repeat("b", 64))
	must(t, err)
	profile, cell := cellByID(t, "candle-metal-llama1-infer")
	performance := resolveCellPerformance(profile, cell, benchmarkNow)
	if performance.Unit != "tokens" || performance.UnitScope != performanceUnitScopeDecodeOutputTokens {
		t.Fatalf("production batch receipt authority=%q/%q, want tokens/decode-only",
			performance.Unit, performance.UnitScope)
	}
	// Prove the current settlement gate itself before durable ingress.
	if err := validateCurrentPerformanceSettlementAuthority(performance); err == nil ||
		!strings.Contains(err.Error(), performanceUnitScopeDecodeOutputTokens) ||
		!strings.Contains(err.Error(), performanceUnitScopeTokenLikeInputPlusOutputTokens) ||
		!strings.Contains(err.Error(), "unit/scope mismatch") {
		t.Fatalf("decode-only performance was accepted by settlement gate: %v", err)
	}
	// Durable ingress with only a workload (no current placement authority)
	// must also refuse without writing.
	jobID := uuid.New()
	err = store.SubmitJobTx(ctx, &jobRow{
		ID: jobID, WorkloadDecision: workload,
	}, nil)
	if err == nil {
		t.Fatal("durable ingress accepted production decode-only batch without placement authority")
	}
	if !strings.Contains(err.Error(), performanceUnitScopeDecodeOutputTokens) &&
		!strings.Contains(err.Error(), "unit/scope mismatch") &&
		!strings.Contains(err.Error(), "placement") &&
		!strings.Contains(err.Error(), "physical authority") {
		t.Fatalf("decode-scope refusal was opaque: %v", err)
	}

	var rows int
	mustf(t, pool.QueryRow(ctx, `SELECT count(*) FROM jobs WHERE id=$1`, jobID).Scan(&rows),
		"count decode-scope-refused job writes: %v")
	if rows != 0 {
		t.Fatalf("decode-scope-refused submission wrote %d job rows", rows)
	}
}

func TestSubmitJobTxRefusesWithdrawnOrReboundCurrentAuthorityWithoutWriting(t *testing.T) {
	ctx, store, pool := openIsolatedTestStore(t)
	workload, compute, placement, economic, pricing := distributedPricingFixture(t)
	if placement.PerformanceAuthority == nil {
		t.Fatal("TEST_ONLY current pricing fixture lacks frozen performance authority")
	}
	path := placement.PerformanceAuthority.Performance.BenchmarkAuthority
	original, ok := benchmarkAuthorityManifest[path]
	if !ok {
		t.Fatalf("TEST_ONLY authority %q is absent", path)
	}

	tests := []struct {
		name       string
		mutate     func(benchmarkReceiptSummary) benchmarkReceiptSummary
		wantPhrase string
	}{
		{
			name: "withdrawn",
			mutate: func(summary benchmarkReceiptSummary) benchmarkReceiptSummary {
				summary.Validity = authorityValidityWithdrawn
				return summary
			},
			wantPhrase: "WITHDRAWN",
		},
		{
			name: "rebound at same path",
			mutate: func(summary benchmarkReceiptSummary) benchmarkReceiptSummary {
				summary.Harness = "TEST_ONLY rebound producer identity"
				return summary
			},
			wantPhrase: "no longer matches the frozen admission snapshot",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			benchmarkAuthorityManifest[path] = tc.mutate(cloneBenchmarkReceiptSummary(original))
			defer func() { benchmarkAuthorityManifest[path] = original }()

			// Frozen replay is self-contained even after today's authority changes.
			mustf(t, validateFrozenRuntimeCellPerformance(placement.PerformanceAuthority),
				"historical replay consulted current %s authority: %v", tc.name)

			jobID := uuid.New()
			err := store.SubmitJobTx(ctx, &jobRow{
				ID:                   jobID,
				WorkloadDecision:     workload,
				ComputePlan:          compute,
				PlacementRequirement: placement,
				EconomicPlan:         economic,
				PricingDecision:      pricing,
			}, nil)
			if err == nil || !strings.Contains(err.Error(), tc.wantPhrase) {
				t.Fatalf("durable ingress accepted %s current authority: %v", tc.name, err)
			}
			var rows int
			mustf(t, pool.QueryRow(ctx, `SELECT count(*) FROM jobs WHERE id=$1`, jobID).Scan(&rows),
				"count %s-authority-refused job writes: %v", tc.name)
			if rows != 0 {
				t.Fatalf("%s-authority-refused submission wrote %d job rows", tc.name, rows)
			}
		})
	}
}

func TestSubmitJobTxTreatsPlacementV1AsHistoricalReadOnlyWithoutWriting(t *testing.T) {
	ctx, store, pool := openIsolatedTestStore(t)
	workload, compute, placement, economic, pricing := distributedPricingFixture(t)
	placement.Version = 1
	placement.PerformanceAuthority = nil
	placement.HWClasses = append([]string(nil), workload.Binding.Constraints.HWClasses...)

	jobID := uuid.New()
	job := &jobRow{
		ID:                   jobID,
		WorkloadDecision:     workload,
		ComputePlan:          compute,
		PlacementRequirement: placement,
		EconomicPlan:         economic,
		PricingDecision:      pricing,
	}
	err := store.SubmitJobTx(ctx, job, nil)
	// Placement v1 remains historical-read-only; current durable admission is v3.
	if err == nil || !strings.Contains(err.Error(), "historical-read-only") ||
		!strings.Contains(err.Error(), "version 1") {
		t.Fatalf("new durable ingress accepted legacy placement v1: %v", err)
	}

	var rows int
	mustf(t, pool.QueryRow(ctx, `SELECT count(*) FROM jobs WHERE id=$1`, jobID).Scan(&rows),
		"count refused legacy-placement writes: %v")
	if rows != 0 {
		t.Fatalf("legacy-placement submission wrote %d job rows", rows)
	}
}
