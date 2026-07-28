package main

import (
	"strings"
	"testing"
)

func computePlanFixture(t *testing.T) (WorkloadDecision, ComputePlan, EconomicPlan) {
	t.Helper()
	sub, herr := normalizeAndValidateJobSubmit(jobSubmit{
		JobType: JobType{Type: "embed"},
		Model:   ModelRef{Kind: "hf", Ref: "all-minilm-l6-v2"},
		Constraints: JobConstraints{
			MaxDurationSecs: 3600,
		},
		Tier: "batch",
	})
	if herr != nil {
		t.Fatalf("normalize compute-plan fixture: %s", herr.msg)
	}
	decision, err := buildWorkloadDecision(sub, strings.Repeat("a", 64))
	if err != nil {
		t.Fatalf("build compute-plan workload: %v", err)
	}
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
		2,
		2,
		1,
		1,
		QuoteTime{P50Secs: 30, P90Secs: 60, WorstCaseSecs: 120},
		"planner",
		0.20,
		0.20,
		QuoteConfidence{Score: 0.8, Reasons: []string{"fixture planner evidence"}},
		[]string{"fixture unknown"},
	)
	if err != nil {
		t.Fatalf("build compute plan: %v", err)
	}
	if err := ValidateComputePlanEconomicSnapshot(plan, decision, economic); err != nil {
		t.Fatalf("valid compute/economic authority rejected: %v", err)
	}
	return decision, plan, economic
}

func TestComputePlanRejectsGeometryPlacementAndEconomicTampering(t *testing.T) {
	decision, plan, economic := computePlanFixture(t)

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
	_, plan, _ := computePlanFixture(t)
	original, err := computePlanDigest(plan)
	if err != nil {
		t.Fatal(err)
	}
	mutant := plan
	mutant.ETAWorstCaseSecs++
	changed, err := computePlanDigest(mutant)
	if err != nil {
		t.Fatal(err)
	}
	if original == changed {
		t.Fatal("compute-plan digest did not bind ETA authority")
	}
}

func TestFrozenComputePlanRejectsTaskClassTotalTampering(t *testing.T) {
	decision, plan, _ := computePlanFixture(t)
	plan.TotalInitialTasks++
	if err := ValidateFrozenComputePlanSnapshot(plan, decision); err == nil {
		t.Fatal("frozen compute-plan validator accepted a task-class total mutation")
	}
}

func TestBoundQuoteSplitNeverConsultsLivePlanner(t *testing.T) {
	_, plan, _ := computePlanFixture(t)
	calls := 0
	got, err := selectSubmissionSplitSize(&boundQuote{ComputePlan: plan}, func() int {
		calls++
		return plan.SplitSize * 100
	})
	if err != nil {
		t.Fatal(err)
	}
	if got != plan.SplitSize {
		t.Fatalf("bound split=%d, want frozen %d", got, plan.SplitSize)
	}
	if calls != 0 {
		t.Fatalf("bound submit consulted live planner %d time(s)", calls)
	}
}

func TestExactReusePlanBindsOriginWithoutInventingPhysicalWork(t *testing.T) {
	decision, origin, _ := computePlanFixture(t)
	reuse, err := newExactReuseComputePlan(decision, 4, 512, 0.05, &origin)
	if err != nil {
		t.Fatal(err)
	}
	if reuse.TotalInitialTasks != 0 || reuse.SplitSize != 0 || reuse.ETAP50Secs != 0 {
		t.Fatalf("exact reuse invented physical work: %+v", reuse)
	}
	wantOrigin, err := computePlanDigest(origin)
	if err != nil {
		t.Fatal(err)
	}
	if reuse.OriginComputePlanSHA256 != wantOrigin {
		t.Fatal("exact reuse did not bind its originating distributed plan")
	}
	if err := ValidateFrozenComputePlanSnapshot(reuse, decision); err != nil {
		t.Fatalf("exact reuse plan rejected: %v", err)
	}
}
