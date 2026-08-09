package main

import (
	"math"
	"strings"
	"testing"
)

func computePlanFixture(t *testing.T) (WorkloadDecision, ComputePlan, EconomicPlan) {
	t.Helper()
	// A few historical embed-reader tests intentionally install an exact legacy
	// embed authority before calling this shared fixture. Preserve that authority
	// when present; otherwise install the explicit TEST_ONLY publication seam.
	// Neither branch widens checked-in production evidence.
	if !advertisedRuntimeCell(candleEmbedCell) {
		installBoundCataloguePublicationAuthorityForTest(t)
	}
	sub, herr := normalizeAndValidateJobSubmit(jobSubmit{
		JobType: JobType{Type: "embed"},
		Model:   ModelRef{Kind: "hf", Ref: "all-minilm-l6-v2"},
		Constraints: JobConstraints{
			MaxDurationSecs: 3600,
		},
		Tier: "batch",
	})
	if herr != nil {
		t.Fatalf("normalize shared embed compute-plan fixture: %s", herr.msg)
	}
	return computePlanFixtureForSubmit(t, sub)
}

func combinedTokenComputePlanFixture(t *testing.T) (WorkloadDecision, ComputePlan, EconomicPlan) {
	t.Helper()
	// The tests in this file are lane-neutral. Prefer the narrow combined-token
	// batch seam so they do not imply that superseded embed evidence is current.
	return computePlanFixtureForSubmit(t, testOnlyCombinedTokenSubmit(t))
}

func computePlanFixtureForSubmit(
	t *testing.T, sub jobSubmit,
) (WorkloadDecision, ComputePlan, EconomicPlan) {
	t.Helper()
	decision, err := buildWorkloadDecision(sub, strings.Repeat("a", 64))
	mustf(t, err, "build compute-plan workload: %v")
	economic := BuildEconomicPlan(EconomicPlanInput{
		BaseComputeUSD:   0.40,
		InitialTaskCount: 4,
		ExtraTaskReserve: 2,
		SupplierShare:    0.97,
	}, testEconomicSchedule())
	if !economic.Executable {
		t.Fatalf("compute-plan fixture economics blocked: %s", economic.BlockReason)
	}
	plan, err := newDistributedComputePlan(
		decision,
		4,
		512,
		testInputDepthProfile(4),
		2,
		2,
		1,
		1,
		quoteTimeFromETABands(30, 50, true),
		"planner",
		0.20,
		0.20,
		QuoteConfidence{Score: 0.8, Reasons: []string{"fixture planner evidence"}},
		[]string{"fixture unknown"},
	)
	mustf(t, err, "build compute plan: %v")
	mustf(t, ValidateComputePlanEconomicSnapshot(plan, decision, economic), "valid compute/economic authority rejected: %v")
	return decision, plan, economic
}

func TestComputePlanRejectsGeometryPlacementAndEconomicTampering(t *testing.T) {
	decision, plan, economic := combinedTokenComputePlanFixture(t)

	tests := []struct {
		name   string
		mutate func(*ComputePlan)
	}{
		{"split size", func(p *ComputePlan) { p.SplitSize = p.InputRecords }},
		{"primary tasks", func(p *ComputePlan) { p.PrimaryTasks++ }},
		{"total tasks", func(p *ComputePlan) { p.TotalInitialTasks++ }},
		{"memory floor", func(p *ComputePlan) { p.MinimumMemoryGB++ }},
		{"input records", func(p *ComputePlan) { p.InputRecords++ }},
		{"input tokens", func(p *ComputePlan) { p.EstimatedInputTokens++ }},
		{"settlement input units", func(p *ComputePlan) { p.SettlementInputUnits++ }},
		{"ETA confidence-band method", func(p *ComputePlan) {
			p.ETAConfidenceBandMethod = etaBandMethodSyntheticMultiples
		}},
		{"ETA band/source provenance", func(p *ComputePlan) { p.ETASource = "static" }},
		{"base compute", func(p *ComputePlan) { p.BaseComputeUSD += 0.01 }},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			mutant := plan
			tc.mutate(&mutant)
			if err := ValidateComputePlanEconomicSnapshot(mutant, decision, economic); err == nil {
				t.Fatalf("%s mutation survived compute-plan validation", tc.name)
			}
		})
	}
}

func TestComputePlanDigestBindsEveryExecutionField(t *testing.T) {
	_, plan, _ := combinedTokenComputePlanFixture(t)
	original, err := computePlanDigest(plan)
	must(t, err)
	mutant := plan
	mutant.ETAWorstCaseSecs++
	changed, err := computePlanDigest(mutant)
	must(t, err)
	if original == changed {
		t.Fatal("compute-plan digest did not bind ETA authority")
	}
}

func TestQuoteTimeFromETABandsIsExplicitAndNeverLetsThePlannerBoundFallBelowP50(t *testing.T) {
	planner := quoteTimeFromETABands(150, 120, true)
	if planner.P50Secs != 150 || planner.P90Secs != 150 || planner.WorstCaseSecs != 600 ||
		planner.ConfidenceBandMethod != etaBandMethodPlannerConservativeBound {
		t.Fatalf("planner ETA band = %+v, want p50=150 conservative=150 worst=600 with explicit model-bound semantics", planner)
	}

	planner = quoteTimeFromETABands(100, 250, true)
	if planner.P90Secs != 250 || planner.WorstCaseSecs != 500 {
		t.Fatalf("planner ETA band = %+v, want conservative=250 and worst=500", planner)
	}

	synthetic := quoteTimeFromETABands(100, 0, false)
	if synthetic.P90Secs != 200 || synthetic.WorstCaseSecs != 400 ||
		synthetic.ConfidenceBandMethod != etaBandMethodSyntheticMultiples {
		t.Fatalf("synthetic ETA band = %+v, want 2x/4x advisory multiples", synthetic)
	}
}

func TestFrozenComputePlanRejectsTaskClassTotalTampering(t *testing.T) {
	decision, plan, _ := combinedTokenComputePlanFixture(t)
	plan.TotalInitialTasks++
	if err := ValidateFrozenComputePlanSnapshot(plan, decision); err == nil {
		t.Fatal("frozen compute-plan validator accepted a task-class total mutation")
	}
}

func TestBoundQuoteSplitNeverConsultsLivePlanner(t *testing.T) {
	_, plan, _ := combinedTokenComputePlanFixture(t)
	calls := 0
	got, err := selectSubmissionSplitSize(&boundQuote{ComputePlan: plan}, func() int {
		calls++
		return plan.SplitSize * 100
	})
	must(t, err)
	if got != plan.SplitSize {
		t.Fatalf("bound split=%d, want frozen %d", got, plan.SplitSize)
	}
	if calls != 0 {
		t.Fatalf("bound submit consulted live planner %d time(s)", calls)
	}
}

// Small multi-task workloads (the canary shape: many primary chunks + a floor
// honeypot against a micro-USD catalogue price) freeze an unfloored compute
// estimate while BuildEconomicPlan raises Input.BaseComputeUSD to the
// min-billable settlement floor. The authorities must still agree: the
// economic base is exactly the floored form of the compute sum. Rewriting the
// compute plan upward to match the floor would collapse every micro-job into
// the same estimate and erase cost discrimination among exactly the jobs the
// floor touches — a settlement concern must not rewrite estimation.
func TestComputePlanKeepsUnflooredEstimateUnderMinBillableSettlementFloor(t *testing.T) {
	decision, _, _ := combinedTokenComputePlanFixture(t)
	const cataloguePrimary = 0.000001 // one micro-USD: estimate floor for a tiny workload
	const primaryTasks = 24
	const honeypotTasks = 1
	totalTasks := primaryTasks + honeypotTasks
	// Unfloored expansion: round(1µ * 25/24) stays 1µ, so verif rounds to $0 —
	// the canary disagreement shape before the floor-aware binding.
	unfloored := roundEconomicUSD(cataloguePrimary * float64(totalTasks) / float64(primaryTasks))
	if unfloored != cataloguePrimary {
		t.Fatalf("fixture no longer reproduces zero-expansion rounding: unfloored=%v primary=%v",
			unfloored, cataloguePrimary)
	}
	economic := BuildEconomicPlan(EconomicPlanInput{
		BaseComputeUSD:   unfloored,
		InitialTaskCount: totalTasks,
		ExtraTaskReserve: economicExtraTaskReserve(primaryTasks),
		SupplierShare:    0.97,
	}, testEconomicSchedule())
	if !economic.Executable {
		t.Fatalf("economics blocked: %s", economic.BlockReason)
	}
	if economic.Input.BaseComputeUSD <= unfloored {
		t.Fatalf("expected min-billable floor to raise base above unfloored %v, got %v",
			unfloored, economic.Input.BaseComputeUSD)
	}

	// Estimation freeze: unfloored primary + (expansion − primary). This is
	// what submit/quote already freeze; it must validate against floored economics.
	plan, err := newDistributedComputePlan(
		decision, primaryTasks, 512, testInputDepthProfile(primaryTasks), 1, primaryTasks, 0, honeypotTasks,
		quoteTimeFromETABands(30, 0, false), "static",
		cataloguePrimary, math.Max(0, unfloored-cataloguePrimary),
		QuoteConfidence{Score: 0.8, Reasons: []string{"unfloored estimate freeze"}},
		nil,
	)
	mustf(t, err, "unfloored compute plan: %v")
	mustf(t, ValidateComputePlanEconomicSnapshot(plan, decision, economic), "unfloored compute must bind floored settlement economics: %v")
	computeSum := roundEconomicUSD(plan.BaseComputeUSD + plan.VerificationOverheadUSD)
	if computeSum != unfloored {
		t.Fatalf("compute sum %v drifted from unfloored estimate %v", computeSum, unfloored)
	}
	if computeSum >= economic.Input.BaseComputeUSD {
		t.Fatalf("fixture lost the floor gap: compute %v economic %v",
			computeSum, economic.Input.BaseComputeUSD)
	}
	// Settlement form of the estimate must be exactly the frozen economic base.
	if got := settlementBaseFromComputeEstimate(computeSum, economic.Input.SupplierShare, totalTasks); got != economic.Input.BaseComputeUSD {
		t.Fatalf("settlement form %v != economic base %v", got, economic.Input.BaseComputeUSD)
	}
}

// True money disagreement (not the min-billable floor relationship) still fails.
// The floor-aware binding must not accept an arbitrary economic base above or
// below the floored form of the compute sum.
func TestComputePlanStillRejectsTrueMoneyDisagreement(t *testing.T) {
	decision, plan, economic := combinedTokenComputePlanFixture(t)

	// Inflate economic base far above floor(compute) without changing compute.
	inflated := economic
	inflated.Input.BaseComputeUSD = economic.Input.BaseComputeUSD + 1.0
	// Rebuild so ValidateEconomicPlanSnapshot round-trips; we then force the
	// input base past the floor relationship while keeping a valid plan shape.
	inflated = BuildEconomicPlan(EconomicPlanInput{
		BaseComputeUSD:   economic.Input.BaseComputeUSD + 1.0,
		InitialTaskCount: economic.Input.InitialTaskCount,
		ExtraTaskReserve: economic.Input.ExtraTaskReserve,
		SupplierShare:    economic.Input.SupplierShare,
		SLAPremiumUSD:    economic.Input.SLAPremiumUSD,
		FirmQuoteMaxUSD:  economic.Input.FirmQuoteMaxUSD,
	}, economic.Schedule)
	if !inflated.Executable {
		t.Fatalf("inflated economics blocked: %s", inflated.BlockReason)
	}
	if err := ValidateComputePlanEconomicSnapshot(plan, decision, inflated); err == nil {
		t.Fatal("compute plan accepted an economic base above floor(compute sum)")
	}

	// Deflate compute below the economic base without the floor covering the gap:
	// set compute sum to something whose floor is still below the fixture economic
	// base (fixture economic is $0.40 — well above min-billable).
	deflated := plan
	deflated.BaseComputeUSD = 0.01
	deflated.VerificationOverheadUSD = 0.01
	if err := ValidateComputePlanEconomicSnapshot(deflated, decision, economic); err == nil {
		t.Fatal("economic base above floor(deflated compute) was accepted")
	}
}

func TestSettlementBaseFromComputeEstimateMatchesBuildEconomicPlanFloor(t *testing.T) {
	for _, tc := range []struct {
		raw   float64
		tasks int
		share float64
	}{
		{0.000001, 25, 0.97}, // canary micro-job
		{0.000001, 1, 0.8},
		{0.40, 4, 0.97}, // already above floor
		{1.0, 1, 1.0},
	} {
		economic := BuildEconomicPlan(EconomicPlanInput{
			BaseComputeUSD:   tc.raw,
			InitialTaskCount: tc.tasks,
			SupplierShare:    tc.share,
		}, testEconomicSchedule())
		if !economic.Executable {
			continue
		}
		got := settlementBaseFromComputeEstimate(tc.raw, tc.share, tc.tasks)
		if math.Abs(got-economic.Input.BaseComputeUSD) > 0.000001 {
			t.Fatalf("raw=%v tasks=%d share=%v: settlement form %v != BuildEconomicPlan %v",
				tc.raw, tc.tasks, tc.share, got, economic.Input.BaseComputeUSD)
		}
	}
}

func TestExactReusePlanBindsOriginWithoutInventingPhysicalWork(t *testing.T) {
	decision, origin, _ := combinedTokenComputePlanFixture(t)
	reuse, err := newExactReuseComputePlan(decision, 4, 512, testInputDepthProfile(4), 0.05, &origin)
	must(t, err)
	if reuse.TotalInitialTasks != 0 || reuse.SplitSize != 0 || reuse.ETAP50Secs != 0 {
		t.Fatalf("exact reuse invented physical work: %+v", reuse)
	}
	wantOrigin, err := computePlanDigest(origin)
	must(t, err)
	if reuse.OriginComputePlanSHA256 != wantOrigin {
		t.Fatal("exact reuse did not bind its originating distributed plan")
	}
	mustf(t, ValidateFrozenComputePlanSnapshot(reuse, decision), "exact reuse plan rejected: %v")
	if reuse.InputDepthProfile == nil || reuse.Version != computePlanVersion {
		t.Fatal("exact reuse plan must carry current input depth profile authority")
	}
}

func TestV4ComputePlanSeparatesPlanningDepthFromSettlementUnits(t *testing.T) {
	_, plan, _ := combinedTokenComputePlanFixture(t)
	if plan.Version != computePlanVersion || plan.InputDepthProfile == nil {
		t.Fatalf("fixture is not a current v4 plan: version=%d profile=%v", plan.Version, plan.InputDepthProfile)
	}
	if plan.EstimatedInputTokens != 4 || plan.SettlementInputUnits != 128 {
		t.Fatalf("fixture failed to preserve distinct planning/money units: body=%d settlement=%v",
			plan.EstimatedInputTokens, plan.SettlementInputUnits)
	}
}

func TestV4ComputePlanRejectsDepthProfileAndSettlementAuthorityTampering(t *testing.T) {
	decision, plan, _ := combinedTokenComputePlanFixture(t)
	if plan.Version != computePlanVersion || plan.InputDepthProfile == nil {
		t.Fatalf("fixture is not a current v4 plan: version=%d profile=%v",
			plan.Version, plan.InputDepthProfile)
	}
	// Band tampering
	mutant := plan
	depth := *plan.InputDepthProfile
	depth.P90DepthBand = inputDepthBandLong
	mutant.InputDepthProfile = &depth
	if err := ValidateFrozenComputePlanSnapshot(mutant, decision); err == nil {
		t.Fatal("accepted tampered p90 depth band")
	}
	// Token tampering on plan while profile stays
	mutant = plan
	mutant.EstimatedInputTokens++
	if err := ValidateFrozenComputePlanSnapshot(mutant, decision); err == nil {
		t.Fatal("accepted estimated_input_tokens disagreeing with profile")
	}
	// Settlement units use the full frozen JSONL geometry, not the selected
	// body-depth profile. Neither authority may be substituted for the other.
	mutant = plan
	mutant.SettlementInputUnits = float64(plan.EstimatedInputTokens)
	if err := ValidateFrozenComputePlanSnapshot(mutant, decision); err == nil {
		t.Fatal("accepted settlement units rewritten to selected-body tokens")
	}
	// Record count vs profile
	mutant = plan
	depth = *plan.InputDepthProfile
	depth.ShortRecords++
	// re-derive tokens/p90 so only the count mismatch vs InputRecords remains
	// (validateInputDepthProfile may still pass if p90/tokens recomputed inconsistently)
	depth.EstimatedTokens = estimateTokensFromCounts(int(depth.BodyRunes), int(depth.BodyASCIIBytes), int(depth.BodyBytes))
	depth.P90DepthBand = deriveP90DepthBand(depth.ShortRecords, depth.MediumRecords, depth.LongRecords)
	mutant.InputDepthProfile = &depth
	if err := ValidateFrozenComputePlanSnapshot(mutant, decision); err == nil {
		t.Fatal("accepted depth profile counts that disagree with input_records")
	}
	// Dropping the profile entirely
	mutant = plan
	mutant.InputDepthProfile = nil
	if err := ValidateFrozenComputePlanSnapshot(mutant, decision); err == nil {
		t.Fatal("accepted v4 plan without depth profile")
	}
	// Profile version tampering
	mutant = plan
	depth = *plan.InputDepthProfile
	depth.Version = 99
	mutant.InputDepthProfile = &depth
	if err := ValidateFrozenComputePlanSnapshot(mutant, decision); err == nil {
		t.Fatal("accepted unsupported depth profile version")
	}
	// The selected bodies must fit inside the exact frozen input byte count.
	mutant = plan
	depth = *plan.InputDepthProfile
	depth.BodyBytes = plan.InputBytes + 1
	depth.BodyRunes = depth.BodyBytes
	depth.BodyASCIIBytes = depth.BodyBytes
	depth.EstimatedTokens = estimateTokensFromCounts(
		int(depth.BodyRunes), int(depth.BodyASCIIBytes), int(depth.BodyBytes),
	)
	mutant.EstimatedInputTokens = depth.EstimatedTokens
	mutant.InputDepthProfile = &depth
	if err := ValidateFrozenComputePlanSnapshot(mutant, decision); err == nil {
		t.Fatal("accepted selected-body bytes greater than exact input bytes")
	}
}

func TestHistoricalV1ComputePlanRemainsValidUnderOldTokenRule(t *testing.T) {
	decision, modern, _ := combinedTokenComputePlanFixture(t)
	// Exact-result reuse was eligible when v1/v2 plans were emitted. Rebuild an
	// accepted-era shape from the explicit TEST_ONLY batch authority so the
	// compatibility fixture remains byte-stable without claiming that a
	// superseded production receipt is current.
	historicalDecision := decision
	historicalDecision.ExactResultCacheEligible = true
	// Runtime candidates gained model_kind after this fixture was published.
	// A decision stored before that keeps its bytes, so the fixture must too,
	// or it stops being the historical record it exists to be.
	historicalDecision.RuntimeCandidates = append(
		[]WorkloadRuntimeCandidate(nil), decision.RuntimeCandidates...)
	for i := range historicalDecision.RuntimeCandidates {
		historicalDecision.RuntimeCandidates[i].ModelKind = ""
	}
	historicalDecisionSHA, err := workloadDecisionDigest(historicalDecision)
	mustf(t, err, "hash historical workload decision: %v")
	// Reconstruct a version-1 plan shape with the historical bytes/4 token rule.
	v1 := modern
	v1.Version = computePlanVersionV1
	v1.WorkloadDecisionSHA256 = historicalDecisionSHA
	v1.InputDepthProfile = nil
	v1.EstimatedInputTokens = estimatedInputTokensForComputePlanV1(v1.InputRecords, v1.InputBytes)
	v1.SettlementInputUnits = 0
	v1.ETAConfidenceBandMethod = ""
	// This synthetic accepted-era fixture predates v4 ETA-band semantics and must
	// preserve its pinned digest exactly.
	v1.ETAP90Secs = 60
	mustf(t, ValidateFrozenComputePlanSnapshot(v1, historicalDecision), "historical v1 plan rejected: %v")
	digest, err := computePlanDigest(v1)
	mustf(t, err, "historical v1 plan not hashable: %v")
	if digest == "" || len(digest) != 64 {
		t.Fatalf("unexpected v1 digest %q", digest)
	}
	const historicalV1FixtureDigest = "957fc906575edac0b3bc3b6d0f7b48907e38370dabba46503f379e41b5630f71"
	if digest != historicalV1FixtureDigest {
		t.Fatalf("historical v1 digest changed: got %s want %s",
			digest, historicalV1FixtureDigest)
	}
	// V1 still rejects token tampering under the old rule.
	tampered := v1
	tampered.EstimatedInputTokens++
	if err := ValidateFrozenComputePlanSnapshot(tampered, historicalDecision); err == nil {
		t.Fatal("v1 plan accepted token tampering")
	}
	// V1 rejects a smuggled depth profile.
	tampered = v1
	depth := testInputDepthProfile(v1.InputRecords)
	tampered.InputDepthProfile = &depth
	if err := ValidateFrozenComputePlanSnapshot(tampered, historicalDecision); err == nil {
		t.Fatal("v1 plan accepted an input depth profile")
	}
	// Version 2 is retained exactly as written before v3 introduced explicit
	// settlement units. Historical pricing/receipt validation must not invent a
	// field and thereby change an accepted plan's digest.
	v2 := modern
	v2.Version = computePlanVersionV2
	v2.WorkloadDecisionSHA256 = historicalDecisionSHA
	v2.SettlementInputUnits = 0
	v2.ETAConfidenceBandMethod = ""
	mustf(t, ValidateFrozenComputePlanSnapshot(v2, historicalDecision), "historical v2 plan rejected: %v")
	tampered = v2
	tampered.SettlementInputUnits = settlementInputUnitsForGeometry(v2.InputRecords, v2.InputBytes)
	if err := ValidateFrozenComputePlanSnapshot(tampered, historicalDecision); err == nil {
		t.Fatal("v2 plan accepted a post-hoc settlement authority field")
	}
	// Unsupported version still fails.
	bad := v1
	bad.Version = 99
	if _, err := computePlanDigest(bad); err == nil {
		t.Fatal("unsupported version was hashable")
	}
	if err := ValidateFrozenComputePlanSnapshot(bad, historicalDecision); err == nil {
		t.Fatal("unsupported version passed validation")
	}
}

func TestBoundQuoteRefusesDifferentMeasuredDepthProfile(t *testing.T) {
	_, plan, _ := combinedTokenComputePlanFixture(t)
	if plan.InputDepthProfile == nil {
		t.Fatal("fixture missing depth profile")
	}
	measured := *plan.InputDepthProfile
	if !inputDepthProfilesEqual(*plan.InputDepthProfile, measured) {
		t.Fatal("identical profiles should equal")
	}
	measured.LongRecords++
	measured.ShortRecords--
	measured.P90DepthBand = deriveP90DepthBand(measured.ShortRecords, measured.MediumRecords, measured.LongRecords)
	if inputDepthProfilesEqual(*plan.InputDepthProfile, measured) {
		t.Fatal("distinct profiles should not equal — bound submit must refuse")
	}
}
