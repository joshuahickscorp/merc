package main

import (
	"encoding/json"
	"math"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
)

// Core regression: one record, max_tokens=256, observed 5 tokens.
// Buyer charge must drop by approximately the unused output share, and the
// three ledger rows must still sum to zero.
func TestObservedOutputSettlementOneRecordMaxTokens256Observed5(t *testing.T) {
	const (
		frozenCharge = 1.0
		frozenPayout = 0.70
		// 1 input token + 256 output tokens mirrors a one-record generative plan.
		estimatedIn  int64 = 1
		estimatedOut int64 = 256
		records      int64 = 1
		maxTokens          = uint32(256)
		observed     int64 = 5
	)

	got := settleObservedOutputTokens(
		frozenCharge, frozenPayout,
		float64(estimatedIn), estimatedOut,
		records, maxTokens,
		observed, true,
	)
	if !got.Applied {
		t.Fatal("expected observed-output rebate to apply")
	}
	if got.CeilingTokens != 256 || got.ObservedTokens != 5 {
		t.Fatalf("evidence ceiling=%d observed=%d, want 256/5", got.CeilingTokens, got.ObservedTokens)
	}

	outputUnitShare := float64(estimatedOut) / float64(estimatedIn+estimatedOut)
	unusedShare := outputUnitShare * (1.0 - float64(observed)/float64(256))
	wantCharge := roundUSD(frozenCharge * (1.0 - unusedShare))
	if wantCharge < minBillableSettlementUSD {
		wantCharge = minBillableSettlementUSD
	}
	wantPayout := roundUSD(frozenPayout * wantCharge / frozenCharge)

	if got.BilledCharge != wantCharge {
		t.Fatalf("billed charge=%.6f, want %.6f (unusedShare=%.6f)",
			got.BilledCharge, wantCharge, unusedShare)
	}
	if got.SupplierPayout != wantPayout {
		t.Fatalf("supplier payout=%.6f, want %.6f", got.SupplierPayout, wantPayout)
	}
	// Approximately the unused output share of the freeze (within one micro
	// after roundUSD on the billed charge).
	if math.Abs(got.BilledCharge-wantCharge) > 0 {
		t.Fatalf("billed charge drifted from rounded unused-share arithmetic")
	}
	rebateFrac := (frozenCharge - got.BilledCharge) / frozenCharge
	if math.Abs(rebateFrac-unusedShare) > 1e-5 {
		t.Fatalf("rebate fraction=%.9f far from unusedShare=%.9f", rebateFrac, unusedShare)
	}
	// Drop is large: ~15× overbill relative to 5/256 of the output ceiling.
	if got.BilledCharge >= frozenCharge*0.5 {
		t.Fatalf("billed charge %.6f did not drop enough from freeze %.6f",
			got.BilledCharge, frozenCharge)
	}
	if got.RebateUSD != roundUSD(frozenCharge-got.BilledCharge) {
		t.Fatalf("rebate=%.6f, want %.6f", got.RebateUSD, frozenCharge-got.BilledCharge)
	}

	// Zero-sum via splitFrozenCharge (buyer negative, supplier+platform positive).
	buyer, supplier, task := uuid.New(), uuid.New(), uuid.New()
	entries := splitFrozenCharge(buyer, supplier, task, "usd",
		got.BilledCharge, got.SupplierPayout, 90, time.Unix(100, 0))
	if len(entries) != 3 {
		t.Fatalf("entries=%d, want 3", len(entries))
	}
	var sum float64
	for _, e := range entries {
		sum += e.AmountUSD
	}
	if roundUSD(sum) != 0 {
		t.Fatalf("ledger rows sum to %.9f, want 0", sum)
	}
	if entries[0].AmountUSD != -got.BilledCharge ||
		entries[1].AmountUSD != got.SupplierPayout ||
		entries[2].AmountUSD != roundUSD(got.BilledCharge-got.SupplierPayout) {
		t.Fatalf("entries=%+v do not match settled amounts", entries)
	}
}

// Invariant 2: settlement never increases relative to the freeze, even when
// the worker reports more tokens than the ceiling.
func TestObservedOutputSettlementNeverIncreasesAboveFreeze(t *testing.T) {
	frozenCharge, frozenPayout := 0.50, 0.30
	got := settleObservedOutputTokens(
		frozenCharge, frozenPayout,
		10, 100,
		1, 100,
		10_000, // well above ceiling
		true,
	)
	if got.BilledCharge > frozenCharge {
		t.Fatalf("billed charge %.6f > freeze %.6f", got.BilledCharge, frozenCharge)
	}
	if got.SupplierPayout > frozenPayout {
		t.Fatalf("payout %.6f > freeze payout %.6f", got.SupplierPayout, frozenPayout)
	}
	// Full use of the ceiling → freeze stands.
	if got.BilledCharge != frozenCharge || got.SupplierPayout != frozenPayout {
		t.Fatalf("over-report must clamp to freeze, got charge=%.6f payout=%.6f",
			got.BilledCharge, got.SupplierPayout)
	}
	if got.ObservedTokens != 100 {
		t.Fatalf("observed clamp=%d, want ceiling 100", got.ObservedTokens)
	}
}

// Invariant 4: 0 <= supplierPayout' <= billedCharge, platformTake' >= 0.
func TestObservedOutputSettlementSupplierWithinBilledCharge(t *testing.T) {
	cases := []struct {
		name                   string
		charge, payout         float64
		inTok, outTok, records int64
		maxTokens              uint32
		observed               int64
	}{
		{"partial", 1.0, 0.7, 1, 256, 1, 256, 5},
		{"zero observed", 2.0, 1.0, 50, 200, 2, 100, 0},
		{"full ceiling", 0.25, 0.10, 8, 32, 1, 32, 32},
		{"tiny freeze", 0.000010, 0.000005, 1, 10, 1, 10, 1},
		{"high share payout", 0.10, 0.10, 4, 100, 1, 100, 1},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := settleObservedOutputTokens(
				tc.charge, tc.payout,
				float64(tc.inTok), tc.outTok,
				tc.records, tc.maxTokens,
				tc.observed, true,
			)
			if got.SupplierPayout < 0 {
				t.Fatalf("supplier payout negative: %.9f", got.SupplierPayout)
			}
			if got.SupplierPayout > got.BilledCharge {
				t.Fatalf("supplier %.9f > billed %.9f", got.SupplierPayout, got.BilledCharge)
			}
			platform := roundUSD(got.BilledCharge - got.SupplierPayout)
			if platform < 0 {
				t.Fatalf("platform take negative: %.9f", platform)
			}
			if got.BilledCharge > tc.charge || got.SupplierPayout > tc.payout {
				t.Fatalf("increased above freeze: got=%+v freeze charge=%.6f payout=%.6f",
					got, tc.charge, tc.payout)
			}
		})
	}
}

// Invariant 6: non-generative (embed) jobs are unchanged — zero ceiling, zero rebate.
func TestObservedOutputSettlementEmbedUnchanged(t *testing.T) {
	frozenCharge, frozenPayout := 0.42, 0.21
	// Embed plans freeze EstimatedOutputTokens at 0.
	got := settleObservedOutputTokens(
		frozenCharge, frozenPayout,
		128, 0, // no generative output
		10, 0, // no max_tokens
		999, true,
	)
	if got.Applied || got.BilledCharge != frozenCharge || got.SupplierPayout != frozenPayout {
		t.Fatalf("embed settled %+v, want exact freeze", got)
	}
	if got.RebateUSD != 0 || got.CeilingTokens != 0 {
		t.Fatalf("embed must not invent ceiling/rebate: %+v", got)
	}
}

// Invariant 7: missing compute-plan token estimates or missing reported tokens
// settle at the freeze — no silent zeroing, no crash.
func TestObservedOutputSettlementMissingInputsSettleAtFreeze(t *testing.T) {
	frozenCharge, frozenPayout := 0.88, 0.44
	cases := []struct {
		name                   string
		inTok, outTok, records int64
		maxTokens              uint32
		observed               int64
		hasReported            bool
	}{
		{"no report", 10, 100, 1, 100, 0, false},
		{"zero total units", 0, 0, 1, 100, 5, true},
		{"zero ceiling records", 10, 100, 0, 100, 5, true},
		{"zero max tokens", 10, 100, 1, 0, 5, true},
		{"negative estimated out treated as missing", 10, -1, 1, 100, 5, true},
		{"negative durable report fails closed", 10, 100, 1, 100, -1, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := settleObservedOutputTokens(
				frozenCharge, frozenPayout,
				float64(tc.inTok), tc.outTok,
				tc.records, tc.maxTokens,
				tc.observed, tc.hasReported,
			)
			if got.Applied {
				t.Fatalf("missing inputs must not apply rebate: %+v", got)
			}
			if got.BilledCharge != frozenCharge || got.SupplierPayout != frozenPayout {
				t.Fatalf("got charge=%.6f payout=%.6f, want freeze", got.BilledCharge, got.SupplierPayout)
			}
			if got.BilledCharge == 0 && frozenCharge != 0 {
				t.Fatal("silent zeroing of a positive freeze")
			}
		})
	}
}

func TestEffectiveObservedOutputMaxTokensMatchesPricingDefault(t *testing.T) {
	workload := WorkloadDecision{
		Binding: WorkloadBinding{JobType: JobType{Type: "batch_infer"}},
	}
	plan := ComputePlan{EstimatedOutputTokens: defaultQuoteMaxTokens}
	if got := effectiveObservedOutputMaxTokens(workload, plan); got != defaultQuoteMaxTokens {
		t.Fatalf("effective max_tokens = %d, want pricing default %d", got, defaultQuoteMaxTokens)
	}
	workload.Binding.JobType.MaxTokens = 77
	if got := effectiveObservedOutputMaxTokens(workload, plan); got != 77 {
		t.Fatalf("explicit max_tokens = %d, want 77", got)
	}
}

func TestSettlementInputUnitsPreserveHistoryAndUseV3FrozenAuthority(t *testing.T) {
	v1 := ComputePlan{
		Version:              computePlanVersionV1,
		InputRecords:         2,
		InputBytes:           101,
		EstimatedInputTokens: 26,
	}
	v2 := v1
	v2.Version = computePlanVersionV2
	v2.EstimatedInputTokens = 3 // selected bodies exclude JSON framing/metadata
	v3 := v2
	v3.Version = computePlanVersionV3
	v3.SettlementInputUnits = 25.25 // exact max(records, input_bytes/4) price authority

	v1Units := settlementInputUnitsForComputePlan(v1)
	v2Units := settlementInputUnitsForComputePlan(v2)
	v3Units := settlementInputUnitsForComputePlan(v3)
	if v1Units != 26 || v2Units != v1Units || v3Units != 25.25 {
		t.Fatalf("settlement input units v1/v2/v3=%v/%v/%v, want 26/26/25.25", v1Units, v2Units, v3Units)
	}

	v1Settlement := settleObservedOutputTokens(1, 0.7, v1Units, 200, 2, 100, 20, true)
	v2Settlement := settleObservedOutputTokens(1, 0.7, v2Units, 200, 2, 100, 20, true)
	if v2Settlement != v1Settlement {
		t.Fatalf("v2 body-depth planning changed frozen economics: v1=%+v v2=%+v",
			v1Settlement, v2Settlement)
	}
	if changed := settleObservedOutputTokens(
		1, 0.7, float64(v2.EstimatedInputTokens), 200, 2, 100, 20, true,
	); changed == v1Settlement {
		t.Fatal("test fixture does not expose the body-token settlement regression")
	}
	v3Settlement := settleObservedOutputTokens(1, 0.7, v3Units, 200, 2, 100, 20, true)
	if v3Settlement == v1Settlement {
		t.Fatal("v3 settlement did not use its exact frozen fractional money authority")
	}
}

func TestObservedOutputSettlementDeterministic(t *testing.T) {
	a := settleObservedOutputTokens(1.23, 0.80, 40, 200, 2, 128, 17, true)
	b := settleObservedOutputTokens(1.23, 0.80, 40, 200, 2, 128, 17, true)
	if a != b {
		t.Fatalf("non-deterministic settlement: %+v vs %+v", a, b)
	}
}

func TestObservedOutputSettlementMinBillableFloor(t *testing.T) {
	// Extreme unused share on a tiny freeze still floors at one micro-USD.
	got := settleObservedOutputTokens(
		0.000002, 0.000001,
		1, 1_000_000,
		1, 1_000_000,
		0, true,
	)
	if got.BilledCharge < minBillableSettlementUSD {
		t.Fatalf("billed %.9f below minBillable", got.BilledCharge)
	}
	if got.BilledCharge > 0.000002 {
		t.Fatalf("floor raised above freeze: %.9f", got.BilledCharge)
	}
}

// This is the exact recovery regression that blocked every generative finalize:
// verification work snapshots deliberately do not duplicate job max_tokens, so
// rebuilt CommitTaskInfo has jobMaxTokens=0. Planning and apply must both load
// the same frozen workload/compute/economic authority and agree on the rebate.
//
// Settlement recovery after a generative commit: verification snapshots omit
// job max_tokens, so rebuilt CommitTaskInfo has jobMaxTokens=0. Planning and
// apply must both load the same frozen workload/compute/economic authority and
// agree on the rebate. Directed routing freezes the generation cell so the
// max_tokens path stays covered even if ordinary admission later changes.
func TestObservedOutputSettlementRecoverySnapshotPlannerAndApplyAgree(t *testing.T) {
	installSettlementCurrencyForTest(t, "usd")
	ctx, store, pool := openMoneyPathStore(t)
	f := seedMoneyPathFixture(t, ctx, store, pool, moneyPathSeedOpts{TaskCount: 1})

	const directedBatchCell = "candle-metal-llama1-infer"
	sub := jobSubmit{
		JobType: JobType{Type: "batch_infer", MaxTokens: 100},
		Model:   ModelRef{Kind: "gguf", Ref: "llama-3.2-1b-instruct-q4"},
		Constraints: JobConstraints{
			MaxDurationSecs: 3600,
		},
		Tier: "batch",
	}
	workload, err := buildWorkloadDecisionDirected(sub, strings.Repeat("c", 64), directedBatchCell)
	mustf(t, err, "build directed batch workload: %v")
	if workload.Binding.JobType.MaxTokens != 100 {
		t.Fatalf("directed batch lost max_tokens: %+v", workload.Binding.JobType)
	}
	jobDepth := testInputDepthProfile(1)
	jobDepthAccumulator := newInputDepthAccumulator()
	jobDepthAccumulator.addBody(strings.Repeat("x", 3000))
	if jobDepth, err = jobDepthAccumulator.profile(); err != nil {
		t.Fatalf("build long job depth fixture: %v", err)
	}
	compute, err := newDistributedComputePlan(
		workload,
		1,
		4096,
		jobDepth,
		1,
		1,
		0,
		0,
		quoteTimeFromETABands(60, 0, false),
		"static",
		f.Plan.Input.BaseComputeUSD,
		0,
		QuoteConfidence{Score: 0.9, Reasons: []string{"snapshot recovery regression fixture"}},
		[]string{"single local regression fixture"},
	)
	mustf(t, err, "build compute plan: %v")
	authority := catalogueAuthorityFixtureInStore(
		t, ctx, pool, workload, f.Plan.Schedule.Currency, f.Plan.Input.SupplierShare,
	)
	placement := placementForPricingFixture(t, workload, authority)
	pricing, err := newDistributedPricingDecision(
		workload, compute, placement, f.Plan, authority, sub.Tier, "",
	)
	mustf(t, err, "build pricing decision: %v")
	jobTypeSpec, err := json.Marshal(sub.JobType)
	must(t, err)
	verificationPolicy, err := json.Marshal(sub.Verification)
	must(t, err)
	tasks := makeTasks(f, 1)
	tasks[0].ExpectedOutputRecords = 1
	f.TaskIDs = []uuid.UUID{tasks[0].ID}
	job := &jobRow{
		ID: f.JobID, BuyerID: f.BuyerID,
		JobType: "batch_infer", ModelRef: "llama-3.2-1b-instruct-q4",
		InputRef: "money/batch-input-" + f.JobID.String(), OutputRef: "money/batch-output-" + f.JobID.String(),
		Tier: sub.Tier, VerificationPolicy: verificationPolicy,
		EstimatedUSD: f.Plan.InitialBuyerChargeUSD, TaskCount: 1,
		MinMemoryGB: float32(workload.MinimumMemoryGB), MaxDurationSecs: 3600,
		JobTypeSpec: jobTypeSpec, SplitSize: 1,
		OfferedRateUsdHr: placement.OfferedRateUsdHr, ETASecs: compute.ETAP50Secs,
		ETARawSecs:           compute.ETAP50Secs,
		EconomicInputRecords: 1, EconomicInputBytes: 4096,
		EconomicInputSource: economicInputSourceSubmitStream,
		EconomicPlan:        f.Plan, WorkloadDecision: workload, ComputePlan: compute,
		PlacementRequirement: placement, PricingDecision: pricing,
	}
	mustf(t, store.SubmitJobTx(ctx, job, tasks), "submit batch job: %v")
	if _, err := pool.Exec(ctx, `UPDATE jobs SET status='running' WHERE id=$1`, f.JobID); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `
		UPDATE tasks
		   SET status='running', claimed_by=$2, claimed_at=now(), worker_id=$2,
		       execution_worker_id=$2, execution_supplier_id=$3,
		       execution_hw_class='apple_silicon_max',
		       execution_engine='candle', execution_build_hash='deadbeefdeadbeef'
		 WHERE id=$1`, tasks[0].ID, f.WorkerID, f.SupplierID); err != nil {
		t.Fatal(err)
	}

	commit := commitFor(f, tasks[0].ID, 0)
	commit.TokensUsed = 5
	if _, err := store.CompleteTaskTx(ctx, tasks[0].ID, f.WorkerID, commit); err != nil {
		t.Fatalf("stage committed result: %v", err)
	}
	work, err := store.VerificationWorkForAttempt(ctx, tasks[0].ID, 0)
	mustf(t, err, "load verification work: %v")
	recovered, _, err := commitInfoFromVerificationWork(work)
	mustf(t, err, "recover commit info: %v")
	if recovered.jobMaxTokens != 0 {
		t.Fatalf("snapshot unexpectedly duplicated max_tokens: %d", recovered.jobMaxTokens)
	}
	settled, err := store.observedOutputSettlementForTask(ctx, recovered)
	mustf(t, err, "plan recovered settlement: %v")
	// Ceiling/observed must still surface so the buyer can audit the observation.
	// The token-proportional rebate may be clamped by the contribution floor
	// (processor fixed fee + control on this conservative schedule leave little
	// headroom); when clamped, Applied stays true and FloorClamped records why
	// the full unused-output rebate was not granted.
	if settled.CeilingTokens != 100 || settled.ObservedTokens != 5 {
		t.Fatalf("recovered settlement lost observation evidence: %+v", settled)
	}
	if !settled.Applied && !settled.FloorClamped {
		t.Fatalf("recovered settlement neither rebated nor floor-clamped: %+v", settled)
	}
	if !settled.FloorClamped && settled.BilledCharge >= f.Plan.BuyerChargePerTaskUSD {
		t.Fatalf("recovered settlement did not apply bounded rebate: %+v", settled)
	}
	// Before verification writes the ledger, both presentation fallbacks must
	// preserve the frozen whole-input economic composition. Compute-plan v3
	// carries selected-body depth separately from the exact catalogue units, so
	// neither presentation path may change the buyer charge split.
	var preLedgerEntries int
	if err := pool.QueryRow(ctx,
		`SELECT count(*) FROM ledger_entries WHERE task_id=$1`,
		tasks[0].ID,
	).Scan(&preLedgerEntries); err != nil {
		t.Fatal(err)
	}
	if preLedgerEntries != 0 {
		t.Fatalf("fixture already has %d ledger entries before fallback checks", preLedgerEntries)
	}
	wantRebate := settled.RebateUSD
	preLedgerReceipts, err := store.JobTaskReceipts(ctx, f.JobID)
	mustf(t, err, "pre-ledger task receipts: %v")
	if len(preLedgerReceipts) != 1 ||
		preLedgerReceipts[0].BilledBuyerChargeUSD == nil ||
		*preLedgerReceipts[0].BilledBuyerChargeUSD != settled.BilledCharge {
		t.Fatalf("pre-ledger receipt billed diverged from settlement: got=%+v want billed=%.6f",
			preLedgerReceipts, settled.BilledCharge)
	}
	if wantRebate > 0 {
		if preLedgerReceipts[0].OutputTokenRebateUSD == nil ||
			*preLedgerReceipts[0].OutputTokenRebateUSD != wantRebate {
			t.Fatalf("pre-ledger receipt rebate diverged: got=%v want %.6f",
				preLedgerReceipts[0].OutputTokenRebateUSD, wantRebate)
		}
	}
	preLedgerInvoice := InvoiceView{JobID: f.JobID}
	mustf(t, store.attachObservedOutputInvoiceEvidence(ctx, &preLedgerInvoice), "pre-ledger invoice evidence: %v")
	if wantRebate > 0 {
		if preLedgerInvoice.OutputTokenRebateUSD == nil ||
			*preLedgerInvoice.OutputTokenRebateUSD != wantRebate {
			t.Fatalf("pre-ledger invoice fallback rebate=%v, want %.6f",
				preLedgerInvoice.OutputTokenRebateUSD, wantRebate)
		}
	}
	var entries []LedgerEntry
	if settled.HasNanos {
		var serr error
		entries, serr = splitFrozenChargeNanos(
			f.BuyerID, f.SupplierID, tasks[0].ID, "usd",
			settled.BilledChargeNanos, settled.SupplierPayoutNanos, 0, time.Now().UTC(),
		)
		if serr != nil {
			t.Fatalf("split frozen charge nanos: %v", serr)
		}
	} else {
		entries = splitFrozenCharge(
			f.BuyerID, f.SupplierID, tasks[0].ID, "usd",
			settled.BilledCharge, settled.SupplierPayout, 0, time.Now().UTC(),
		)
	}
	mustf(t, store.FinalizeTaskVerification(ctx, recovered, OutcomePass, entries), "apply rejected planner's recovered settlement: %v")
	var durationDepthBand *string
	if err := pool.QueryRow(ctx, `
		SELECT input_depth_band
		  FROM task_durations
		 WHERE task_id=$1
		 ORDER BY created_at DESC
		 LIMIT 1`,
		tasks[0].ID,
	).Scan(&durationDepthBand); err != nil {
		t.Fatalf("read finalized task duration depth band: %v", err)
	}
	// The fixture deliberately makes the job p90 long while the original task
	// chunk is short. A duration must follow immutable task geometry, not job
	// aggregate authority.
	if compute.InputDepthProfile.P90DepthBand != inputDepthBandLong {
		t.Fatalf("fixture job p90=%q, want long", compute.InputDepthProfile.P90DepthBand)
	}
	if durationDepthBand == nil || *durationDepthBand != inputDepthBandShort {
		t.Fatalf("finalized task duration depth band=%v, want short", durationDepthBand)
	}
	var ledgerNet float64
	if err := pool.QueryRow(ctx,
		`SELECT COALESCE(sum(amount_usd),0)::float8 FROM ledger_entries WHERE task_id=$1`,
		tasks[0].ID,
	).Scan(&ledgerNet); err != nil {
		t.Fatal(err)
	}
	if roundUSD(ledgerNet) != 0 {
		t.Fatalf("recovered settlement ledger net %.9f, want zero", ledgerNet)
	}

	// Presentation must use the same frozen workload authority as settlement,
	// even if the older denormalized submission JSON drifts.
	if _, err := pool.Exec(ctx,
		`UPDATE jobs
		    SET job_type_spec=jsonb_set(job_type_spec,'{max_tokens}','999'::jsonb)
		  WHERE id=$1`, f.JobID); err != nil {
		t.Fatal(err)
	}
	receipts, err := store.JobTaskReceipts(ctx, f.JobID)
	mustf(t, err, "task receipts: %v")
	if len(receipts) != 1 || receipts[0].OutputTokenCeiling == nil ||
		*receipts[0].OutputTokenCeiling != 100 ||
		receipts[0].ObservedOutputTokens == nil || *receipts[0].ObservedOutputTokens != 5 {
		t.Fatalf("receipt did not preserve frozen 100/5 evidence: %+v", receipts)
	}
	invoice := InvoiceView{JobID: f.JobID}
	mustf(t, store.attachObservedOutputInvoiceEvidence(ctx, &invoice), "invoice evidence: %v")
	if invoice.OutputTokenCeiling == nil || *invoice.OutputTokenCeiling != 100 ||
		invoice.ObservedOutputTokens == nil || *invoice.ObservedOutputTokens != 5 {
		t.Fatalf("invoice did not preserve frozen 100/5 evidence: %+v", invoice)
	}

	// A negative durable observation is corrupt. Money holds its existing
	// fail-closed settlement and presentation omits the observation instead of
	// falsely advertising zero tokens.
	if _, err := pool.Exec(ctx,
		`UPDATE tasks SET reported_tokens_used=-1 WHERE id=$1`, tasks[0].ID); err != nil {
		t.Fatal(err)
	}
	receipts, err = store.JobTaskReceipts(ctx, f.JobID)
	mustf(t, err, "task receipts after corrupt observation: %v")
	if len(receipts) != 1 || receipts[0].ObservedOutputTokens != nil {
		t.Fatalf("receipt advertised corrupt observation: %+v", receipts)
	}
	invoice = InvoiceView{JobID: f.JobID}
	mustf(t, store.attachObservedOutputInvoiceEvidence(ctx, &invoice), "invoice corrupt evidence: %v")
	if invoice.ObservedOutputTokens != nil {
		t.Fatalf("invoice advertised corrupt observation: %+v", invoice)
	}
}
