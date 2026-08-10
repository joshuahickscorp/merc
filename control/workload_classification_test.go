package main

import (
	"bytes"
	"context"
	"encoding/json"
	"math"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"

	"github.com/google/uuid"
)

func validBatchWorkloadSubmit(t *testing.T) jobSubmit {
	t.Helper()
	// Classification mechanics require one successful current admission. The
	// checked-in production authority is intentionally empty, so start from the
	// explicit in-memory TEST_ONLY combined-token lane and layer this fixture's
	// execution-shape fields onto that normalized request.
	sub := testOnlyCombinedTokenSubmit(t)
	sub.Params = json.RawMessage(`{"split_size":8}`)
	sub.Verification = VerificationPolicy{
		RedundancyFrac: 0.1,
		HoneypotFrac:   0.1,
		PayoutHoldSecs: 3600,
	}
	sub.MinReputation = 0.25
	sub.DeadlineSecs = 3600
	return sub
}

func TestWorkloadDecisionIsServerClassifiedAndRevisionPinned(t *testing.T) {
	sub := validBatchWorkloadSubmit(t)
	decision, err := buildWorkloadDecision(sub, strings.Repeat("a", 64))
	must(t, err)
	if decision.WorkloadClass != "batch_generation" ||
		decision.RuntimeJobType != "batch_infer" ||
		decision.ModelRevision != "b69aef112e9f895e6f98d7ae0949f72ff09aa401" {
		t.Fatalf("decision did not derive class/job/revision from runtime authority: %+v", decision)
	}
	if len(decision.RuntimeCandidates) != 1 ||
		decision.RuntimeCandidates[0].CellID != "candle-metal-llama1-infer" {
		t.Fatalf("decision did not resolve one exact runtime cell: %+v", decision.RuntimeCandidates)
	}
	// Batch generation is deterministic and prefix-compatible, but exact-result
	// reuse remains fail-closed until its settlement meter is governed.
	if !decision.Deterministic || decision.ExactResultCacheEligible ||
		!decision.PrefixReuseEligible || decision.InflightCoalescingEligible {
		t.Fatalf("decision over/under-stated reuse eligibility: %+v", decision)
	}
	mustf(t, ValidateWorkloadDecisionSnapshot(decision), "untouched decision rejected: %v")
}

func TestWorkloadBindingCoversEveryExecutionAssumption(t *testing.T) {
	base := validBatchWorkloadSubmit(t)
	inputSHA := strings.Repeat("b", 64)
	original, err := buildWorkloadDecision(base, inputSHA)
	must(t, err)

	mutations := map[string]func(*jobSubmit){
		"max tokens": func(s *jobSubmit) { s.JobType.MaxTokens++ },
		"params":     func(s *jobSubmit) { s.Params = json.RawMessage(`{"split_size":16}`) },
		"memory":     func(s *jobSubmit) { s.Constraints.MinMemoryGB = 12 },
		"hardware": func(s *jobSubmit) {
			s.Constraints.HWClasses = []string{"apple_silicon_ultra"}
		},
		"residency": func(s *jobSubmit) { s.Constraints.DataResidency = []string{"CA"} },
		"verification": func(s *jobSubmit) {
			s.Verification.RedundancyFrac = 0.5
		},
		"tier":           func(s *jobSubmit) { s.Tier = "priority" },
		"reputation":     func(s *jobSubmit) { s.MinReputation = 0.9 },
		"deadline":       func(s *jobSubmit) { s.DeadlineSecs = 600 },
		"max duration":   func(s *jobSubmit) { s.Constraints.MaxDurationSecs = 7200 },
		"input identity": func(s *jobSubmit) {},
	}
	for name, mutate := range mutations {
		t.Run(name, func(t *testing.T) {
			changed := base
			mutate(&changed)
			changedInputSHA := inputSHA
			if name == "input identity" {
				changedInputSHA = strings.Repeat("c", 64)
			}
			decision, err := buildWorkloadDecision(changed, changedInputSHA)
			must(t, err)
			if decision.BindingSHA256 == original.BindingSHA256 {
				t.Fatalf("%s changed but workload binding digest did not", name)
			}
		})
	}

	nonExecution := base
	nonExecution.MaxUSD = 99
	nonExecution.WebhookURL = "https://example.test/hook"
	nonExecution.IdempotencyKey = "different-idempotency-key"
	same, err := buildWorkloadDecision(nonExecution, inputSHA)
	must(t, err)
	if same.BindingSHA256 != original.BindingSHA256 {
		t.Fatal("budget/delivery/idempotency metadata changed the execution-shape binding")
	}
}

func TestWorkloadDecisionRejectsTampering(t *testing.T) {
	decision, err := buildWorkloadDecision(validBatchWorkloadSubmit(t), strings.Repeat("d", 64))
	must(t, err)
	decision.MinimumMemoryGB = 0
	if err := ValidateWorkloadDecisionSnapshot(decision); err == nil {
		t.Fatal("tampered workload decision was accepted")
	}
}

func TestPlacementRequirementRejectsEveryClaimAuthorityMutation(t *testing.T) {
	sub := testOnlyCombinedTokenSubmit(t)
	sub.Constraints.HWClasses = []string{"apple_silicon_ultra"}
	sub.Constraints.DataResidency = []string{"CA"}
	decision, err := buildWorkloadDecision(sub, strings.Repeat("e", 64))
	must(t, err)
	base, err := placementRequirementFor(sub, decision, 1.25)
	must(t, err)

	tests := []struct {
		name   string
		mutate func(*PlacementRequirement)
	}{
		{"version", func(p *PlacementRequirement) { p.Version++ }},
		{"job type", func(p *PlacementRequirement) { p.JobType = "embed" }},
		{"model ref", func(p *PlacementRequirement) { p.ModelRef = "different-model" }},
		{"model kind", func(p *PlacementRequirement) { p.ModelKind = "different-kind" }},
		{"runtime cell", func(p *PlacementRequirement) { p.RuntimeCellID = "different-cell" }},
		{"runtime id", func(p *PlacementRequirement) { p.RuntimeID = "different-runtime" }},
		{"engine", func(p *PlacementRequirement) { p.Engine = "different-engine" }},
		{"malformed matrix identity", func(p *PlacementRequirement) { p.RuntimeMatrixSHA256 = "not-a-sha256" }},
		{"memory", func(p *PlacementRequirement) { p.MinMemoryGB++ }},
		{"hardware", func(p *PlacementRequirement) { p.HWClasses = []string{"apple_silicon_max"} }},
		{"residency", func(p *PlacementRequirement) { p.DataResidency = []string{"US"} }},
		{"reputation", func(p *PlacementRequirement) { p.MinReputation = 0.99 }},
		{"trusted tier", func(p *PlacementRequirement) { p.TrustedOnly = !p.TrustedOnly }},
		{"negative offered rate", func(p *PlacementRequirement) { p.OfferedRateUsdHr = -1 }},
		{"non-finite offered rate", func(p *PlacementRequirement) {
			p.OfferedRateUsdHr = float32(math.NaN())
		}},
		{"performance digest", func(p *PlacementRequirement) {
			p.PerformanceAuthority.Digest = strings.Repeat("0", 64)
		}},
		{"benchmark snapshot digest", func(p *PlacementRequirement) {
			p.PerformanceAuthority.BenchmarkSnapshotSHA256 = strings.Repeat("1", 64)
		}},
		{"measured hardware", func(p *PlacementRequirement) {
			p.PerformanceAuthority.Performance.MeasuredOnHWClass = "apple_silicon_max"
		}},
		{"observed throughput", func(p *PlacementRequirement) {
			p.PerformanceAuthority.Performance.ObservedUnitsPerSec *= 2
		}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			mutant := base
			mutant.HWClasses = append([]string(nil), base.HWClasses...)
			mutant.DataResidency = append([]string(nil), base.DataResidency...)
			if base.PerformanceAuthority != nil {
				frozen := *base.PerformanceAuthority
				frozen.Performance.HardwareClasses = append([]string(nil),
					base.PerformanceAuthority.Performance.HardwareClasses...)
				frozen.BenchmarkSnapshot = cloneBenchmarkReceiptSummary(
					base.PerformanceAuthority.BenchmarkSnapshot)
				mutant.PerformanceAuthority = &frozen
			}
			tc.mutate(&mutant)
			if err := validatePlacementRequirement(mutant, decision); err == nil {
				t.Fatalf("%s mutation survived placement validation", tc.name)
			}
		})
	}
}

func TestPlacementRequirementPinsTheBenchmarkHardwareClass(t *testing.T) {
	sub := testOnlyCombinedTokenSubmit(t)
	sub.Constraints.HWClasses = nil
	decision, err := buildWorkloadDecision(sub, strings.Repeat("9", 64))
	must(t, err)

	placement, err := placementRequirementFor(sub, decision, 1.25)
	must(t, err)
	if got, want := placement.HWClasses, []string{"apple_silicon_ultra"}; !sameStrings(got, want) {
		t.Fatalf("placement hardware = %v, want benchmark-bound %v", got, want)
	}
	if placement.PerformanceAuthority == nil ||
		placement.PerformanceAuthority.Performance.MeasuredOnHWClass != "apple_silicon_ultra" ||
		!validSHA256(placement.PerformanceAuthority.BenchmarkSnapshotSHA256) ||
		!validSHA256(placement.PerformanceAuthority.Digest) {
		t.Fatalf("placement did not freeze benchmark/hardware authority: %+v",
			placement.PerformanceAuthority)
	}
	if !sameStrings(decision.Binding.Constraints.HWClasses, nil) {
		t.Fatalf("buyer constraint was rewritten instead of narrowed by placement: %v",
			decision.Binding.Constraints.HWClasses)
	}
}

func TestHistoricalPlacementReplaysFrozenBenchmarkAfterCurrentManifestReplacement(t *testing.T) {
	workload, compute, placement, economic, pricing := distributedPricingFixture(t)
	if placement.PerformanceAuthority == nil {
		t.Fatal("fixture placement lacks frozen performance authority")
	}
	path := placement.PerformanceAuthority.Performance.BenchmarkAuthority
	original, ok := benchmarkAuthorityManifest[path]
	if !ok {
		t.Fatalf("fixture benchmark authority %q is not in the manifest", path)
	}
	replacement := cloneBenchmarkReceiptSummary(original)
	replacement.HWClass = "apple_silicon_max"
	throughput := replacement.Throughput[placement.RuntimeID]
	throughput.UnitsPerSecAtOperatingBatch *= 1.5
	replacement.Throughput[placement.RuntimeID] = throughput
	benchmarkAuthorityManifest[path] = replacement
	t.Cleanup(func() { benchmarkAuthorityManifest[path] = original })

	if err := ValidateDistributedPricingDecisionSnapshot(
		pricing, workload, compute, placement, economic,
	); err != nil {
		t.Fatalf("historical pricing failed after current benchmark replacement: %v", err)
	}

	if _, err := newDistributedPricingDecision(
		workload, compute, placement, economic, pricing.Catalogue,
		pricing.Tier, pricing.OriginQuotePricingDecisionSHA256,
	); err == nil {
		t.Fatal("new admission accepted a placement frozen from the replaced benchmark authority")
	}
}

func TestNewPlacementRefusesAWithdrawnCurrentBenchmarkAuthority(t *testing.T) {
	sub := testOnlyCombinedTokenSubmit(t)
	decision, err := buildWorkloadDecision(sub, strings.Repeat("7", 64))
	must(t, err)

	// Resolve the authority path before changing the current manifest. The
	// workload decision is already frozen, which exercises the precise seam that
	// matters: an old workload proposal must not launder a newly withdrawn
	// benchmark into a fresh placement/pricing acceptance.
	candidate := decision.RuntimeCandidates[0]
	var path string
	for _, profile := range runtimeAuthority.Runtimes {
		if profile.RuntimeID != candidate.RuntimeID {
			continue
		}
		for _, cell := range profile.Cells {
			if cell.ID == candidate.CellID {
				path = cell.benchmarkAuthorityFor(profile)
			}
		}
	}
	if path == "" {
		t.Fatal("fixture runtime cell names no benchmark authority")
	}
	original := benchmarkAuthorityManifest[path]
	withdrawn := cloneBenchmarkReceiptSummary(original)
	withdrawn.Validity = authorityValidityWithdrawn
	benchmarkAuthorityManifest[path] = withdrawn
	t.Cleanup(func() { benchmarkAuthorityManifest[path] = original })

	if _, err := placementRequirementFor(sub, decision, 1.25); err == nil ||
		!strings.Contains(strings.ToLower(err.Error()), "withdrawn") {
		t.Fatalf("new placement did not refuse withdrawn benchmark authority: %v", err)
	}
}

func TestFrozenPerformanceCanonicalizesSourceCommitForRepositorylessReplay(t *testing.T) {
	_, _, placement, _, _ := distributedPricingFixture(t)
	if placement.PerformanceAuthority == nil {
		t.Fatal("fixture placement lacks frozen performance authority")
	}
	frozen := *placement.PerformanceAuthority
	if got := frozen.BenchmarkSnapshot.MercSourceCommit; len(got) != 40 ||
		!hexObjectName.MatchString(got) {
		t.Fatalf("frozen benchmark source commit is not canonical: %q", got)
	}
	// Rehash a syntactically short historical snapshot. The validator must reject
	// it by frozen-contract shape, without relying on whether the current process
	// happens to have a .git object database.
	frozen.BenchmarkSnapshot.MercSourceCommit = frozen.BenchmarkSnapshot.MercSourceCommit[:12]
	var err error
	frozen.BenchmarkSnapshotSHA256, err = benchmarkReceiptSummarySHA256(frozen.BenchmarkSnapshot)
	must(t, err)
	frozen.Digest, err = frozenRuntimeCellPerformanceDigest(frozen)
	must(t, err)
	if err := validateFrozenRuntimeCellPerformance(&frozen); err == nil ||
		!strings.Contains(err.Error(), "40-character") {
		t.Fatalf("rehashable short source commit survived frozen replay: %v", err)
	}
}

func TestPlacementRequirementRefusesHardwareWithoutMatchingThroughputEvidence(t *testing.T) {
	sub := testOnlyCombinedTokenSubmit(t)
	sub.Constraints.HWClasses = []string{"apple_silicon_max"}
	decision, err := buildWorkloadDecision(sub, strings.Repeat("8", 64))
	must(t, err)

	if _, err := placementRequirementFor(sub, decision, 1.25); err == nil ||
		!strings.Contains(err.Error(), "outside the buyer's allowed set") {
		t.Fatalf("unmeasured hardware placement was not refused: %v", err)
	}
}

func TestJobClaimProjectionRejectsFrozenWorkloadMismatch(t *testing.T) {
	sub := testOnlyCombinedTokenSubmit(t)
	sub.Constraints.HWClasses = []string{"apple_silicon_ultra"}
	sub.Constraints.DataResidency = []string{"CA"}
	decision, err := buildWorkloadDecision(sub, strings.Repeat("f", 64))
	must(t, err)
	placement, err := placementRequirementFor(sub, decision, 1.25)
	must(t, err)
	base := jobRow{
		JobType:              decision.RuntimeJobType,
		ModelRef:             decision.Binding.Model.Ref,
		Tier:                 decision.Binding.Tier,
		MinMemoryGB:          float32(decision.MinimumMemoryGB),
		MaxDurationSecs:      decision.Binding.Constraints.MaxDurationSecs,
		HWClasses:            append([]string(nil), placement.HWClasses...),
		DataResidency:        append([]string(nil), decision.Binding.Constraints.DataResidency...),
		MinReputation:        decision.Binding.MinReputation,
		OfferedRateUsdHr:     1.25,
		WorkloadDecision:     decision,
		PlacementRequirement: placement,
	}
	mustf(t, validateJobClaimAuthority(&base), "valid claim projection rejected: %v")
	tests := []struct {
		name   string
		mutate func(*jobRow)
	}{
		{"job type", func(j *jobRow) { j.JobType = "embed" }},
		{"model", func(j *jobRow) { j.ModelRef = "different-model" }},
		{"tier", func(j *jobRow) { j.Tier = "priority" }},
		{"memory", func(j *jobRow) { j.MinMemoryGB++ }},
		{"duration", func(j *jobRow) { j.MaxDurationSecs++ }},
		{"hardware", func(j *jobRow) { j.HWClasses = []string{"apple_silicon_max"} }},
		{"residency", func(j *jobRow) { j.DataResidency = []string{"US"} }},
		{"reputation", func(j *jobRow) { j.MinReputation = 0.99 }},
		{"offered rate", func(j *jobRow) { j.OfferedRateUsdHr = -1 }},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			mutant := base
			mutant.HWClasses = append([]string(nil), base.HWClasses...)
			mutant.DataResidency = append([]string(nil), base.DataResidency...)
			tc.mutate(&mutant)
			if err := validateJobClaimAuthority(&mutant); err == nil {
				t.Fatalf("%s mutation survived job claim projection validation", tc.name)
			}
		})
	}
}

func TestWorkloadDecisionDigestBindsDerivedAuthority(t *testing.T) {
	decision, err := buildWorkloadDecision(validBatchWorkloadSubmit(t), strings.Repeat("d", 64))
	must(t, err)
	original, err := workloadDecisionDigest(decision)
	must(t, err)
	decision.ModelRevision = "different-revision"
	changed, err := workloadDecisionDigest(decision)
	must(t, err)
	if changed == original {
		t.Fatal("derived runtime authority changed without changing the full decision digest")
	}
}

func TestBatchExactReuseIsDisabledForCurrentAndHistoricalDecisions(t *testing.T) {
	sub := validBatchWorkloadSubmit(t)
	decision, err := buildWorkloadDecision(sub, strings.Repeat("1", 64))
	must(t, err)
	if decision.ExactResultCacheEligible {
		t.Fatal("new workload decision re-enabled the unsafe batch exact-result cache")
	}
	if got := batchRequestIdentity(decision); got != "" {
		t.Fatalf("disabled batch exact reuse produced identity %q", got)
	}
	// A frozen pre-disable decision must also be unable to read or populate the
	// cache. Otherwise an old job completing after this fix could retain the
	// record-count-as-token meter for a future release.
	historical := decision
	historical.ExactResultCacheEligible = true
	if got := batchRequestIdentity(historical); got != "" {
		t.Fatalf("historical batch decision bypassed the exact-reuse kill switch: %q", got)
	}
}

func TestFrozenWorkloadDecisionSurvivesAuthorityHistoryWithoutAcceptingBindingTamper(t *testing.T) {
	decision, err := buildWorkloadDecision(validBatchWorkloadSubmit(t), strings.Repeat("4", 64))
	must(t, err)
	historical := decision
	historical.ModelRevision = strings.Repeat("5", 40)
	mustf(t, ValidateFrozenWorkloadDecisionSnapshot(historical), "self-contained historical decision rejected: %v")
	if err := ValidateWorkloadDecisionSnapshot(historical); err == nil {
		t.Fatal("historical decision unexpectedly matched current runtime authority")
	}

	tampered := decision
	tampered.Binding.InputSHA256 = strings.Repeat("6", 64)
	if err := ValidateFrozenWorkloadDecisionSnapshot(tampered); err == nil {
		t.Fatal("frozen validator accepted a binding changed after admission")
	}
}

func TestWorkloadInputShapeFailsBeforeExecution(t *testing.T) {
	valid := []byte("{\"id\":\"1\",\"text\":\"hello\"}\n{\"prompt\":\"world\"}\n")
	mustf(t, validateWorkloadJSONL("embed", valid), "valid input rejected: %v")
	for name, input := range map[string][]byte{
		"malformed":       []byte("{not-json}\n"),
		"array":           []byte("[\"not\",\"an\",\"object\"]\n"),
		"missing body":    []byte("{\"id\":\"1\"}\n"),
		"non-string body": []byte("{\"text\":7}\n"),
		"duplicate body":  []byte("{\"text\":\"first\",\"text\":\"second\"}\n"),
		"invalid utf8":    append([]byte("{\"text\":\""), 0xff, '"', '}', '\n'),
		"empty":           []byte("\n \n"),
	} {
		t.Run(name, func(t *testing.T) {
			if err := validateWorkloadJSONL("batch_infer", input); err == nil {
				t.Fatal("invalid workload input accepted")
			}
		})
	}
}

func TestQuoteUsesStrictSharedNormalizerBeforeStoreAccess(t *testing.T) {
	server := &Server{}
	auth := &AuthResult{BuyerID: uuid.New()}

	tests := map[string]string{
		"unknown field": `{
			"job_type":{"type":"embed"},
			"model":{"ref":"all-minilm-l6-v2"},
			"input":"{\"text\":\"hello\"}\n",
			"not_a_field":true
		}`,
		"submit-invalid temperature": `{
			"job_type":{"type":"batch_infer","max_tokens":32,"temperature":0.5},
			"model":{"ref":"llama-3.2-1b-instruct-q4"},
			"input":"{\"prompt\":\"hello\"}\n"
		}`,
		"submit-invalid hardware": `{
			"job_type":{"type":"embed"},
			"model":{"ref":"all-minilm-l6-v2"},
			"constraints":{"hw_classes":["imaginary_gpu"]},
			"input":"{\"text\":\"hello\"}\n"
		}`,
	}
	for name, body := range tests {
		t.Run(name, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodPost, "/v1/quote", bytes.NewBufferString(body))
			req = req.WithContext(context.WithValue(req.Context(), ctxBuyer, auth))
			rec := httptest.NewRecorder()
			server.handleQuote(rec, req)
			if rec.Code != http.StatusBadRequest {
				t.Fatalf("status=%d body=%s, want 400 before any store access", rec.Code, rec.Body.String())
			}
		})
	}
}

func TestNormalizeWorkloadRejectsCrossLaneAndInvalidPolicyFields(t *testing.T) {
	// These are request-shape assertions. Exercise the same structural preflight
	// production uses before loading durable activation, so zero current lanes do
	// not turn every case into the same authority error.
	normalize := func(sub jobSubmit) (jobSubmit, *httpError) {
		return normalizeAndValidateJobSubmit(sub, false)
	}
	valid := func() jobSubmit {
		return jobSubmit{
			JobType: JobType{Type: "embed"},
			Model:   ModelRef{Ref: "all-minilm-l6-v2"},
			Tier:    "batch",
		}
	}
	tests := map[string]func(*jobSubmit){
		"generation field on embed": func(s *jobSubmit) { s.JobType.MaxTokens = 1 },
		"negative embed batch":      func(s *jobSubmit) { s.JobType.BatchSize = -1 },
		"invalid residency": func(s *jobSubmit) {
			s.Constraints.DataResidency = []string{"Canada"}
		},
		"verification above one": func(s *jobSubmit) {
			s.Verification.RedundancyFrac = 1.1
		},
		"reputation above one": func(s *jobSubmit) { s.MinReputation = 1.1 },
		"scalar params":        func(s *jobSubmit) { s.Params = json.RawMessage(`7`) },
	}
	for name, mutate := range tests {
		t.Run(name, func(t *testing.T) {
			sub := valid()
			mutate(&sub)
			if _, herr := normalize(sub); herr == nil {
				t.Fatal("invalid workload shape accepted")
			}
		})
	}

	sub := valid()
	sub.Constraints.HWClasses = []string{"apple_silicon_ultra", "apple_silicon_ultra"}
	sub.Constraints.DataResidency = []string{"ca", " CA "}
	normalized, herr := normalize(sub)
	if herr != nil {
		t.Fatal(herr.msg)
	}
	if len(normalized.Constraints.HWClasses) != 1 ||
		len(normalized.Constraints.DataResidency) != 1 ||
		normalized.Constraints.DataResidency[0] != "CA" {
		t.Fatalf("semantic sets were not canonicalized: %+v", normalized.Constraints)
	}
}

func TestQuoteSupplyRequirementsMatchClaimTimeHardFilters(t *testing.T) {
	ctx, store, pool := openMoneyPathStore(t)
	fixture := seedMoneyPathFixture(t, ctx, store, pool, moneyPathSeedOpts{})

	for _, update := range []struct {
		sql string
		id  uuid.UUID
	}{
		{`UPDATE suppliers SET data_country='XZ', tier=2, reputation=1.00, completed_tasks=500 WHERE id=$1`, fixture.SupplierID},
		{`UPDATE suppliers SET data_country='US', tier=1, reputation=0.40 WHERE id=$1`, fixture.OtherSupplierID},
		{`UPDATE workers SET hw_class='apple_silicon_max' WHERE id=$1`, fixture.WorkerID},
		{`UPDATE workers SET hw_class='apple_silicon_base' WHERE id=$1`, fixture.OtherWorkerID},
	} {
		if _, err := pool.Exec(ctx, update.sql, update.id); err != nil {
			t.Fatal(err)
		}
	}

	all, err := store.EligibleWorkerCount(ctx, "embed", "all-minilm-l6-v2", 0)
	must(t, err)
	if all < 2 {
		t.Fatalf("unconstrained eligible workers=%d, want at least the two fixture workers", all)
	}

	req := QuoteSupplyRequirements{
		HWClasses:     []string{"apple_silicon_max"},
		DataResidency: []string{"XZ"},
		MinReputation: 0.999,
		TrustedOnly:   true,
	}
	constrained, err := store.EligibleWorkerCountFor(ctx, "embed", "all-minilm-l6-v2", req)
	must(t, err)
	if constrained != 1 {
		t.Fatalf("constraint-aware eligible workers=%d, want only the XZ trusted Max worker", constrained)
	}
	reputation, err := store.EligiblePoolReputationFor(ctx, "embed", "all-minilm-l6-v2", req)
	must(t, err)
	if reputation != 1 {
		t.Fatalf("constraint-aware pool reputation=%v, want 1.0", reputation)
	}

	req.HWClasses = []string{"apple_silicon_base"}
	none, err := store.EligibleWorkerCountFor(ctx, "embed", "all-minilm-l6-v2", req)
	must(t, err)
	if none != 0 {
		t.Fatalf("incompatible residency+hardware constraints reported %d eligible workers", none)
	}
}

func TestClaimUsesFrozenRuntimeCandidate(t *testing.T) {
	// Isolated: a shared queue hands unrelated jobs to both workers and masks
	// the frozen-candidate filter under test.
	// The checked-in batch receipt is intentionally SUPERSEDED. Build this
	// scheduler-mechanics fixture through the scoped TEST_ONLY current
	// catalogue/placement authority instead of reviving that receipt.
	ctx, store, job, tasks, _ := currentUniformCatalogueIngressFixture(t)
	pool := store.pool
	var workerID, supplierID, otherWorkerID, otherSupplierID uuid.UUID
	must(t, pool.QueryRow(ctx, `
		SELECT w.id,s.id FROM workers w JOIN suppliers s ON s.id=w.supplier_id
		 WHERE s.email LIKE 'money-sup-%' LIMIT 1`).Scan(&workerID, &supplierID))
	must(t, pool.QueryRow(ctx, `
		SELECT w.id,s.id FROM workers w JOIN suppliers s ON s.id=w.supplier_id
		 WHERE s.email LIKE 'money-other-%' LIMIT 1`).Scan(&otherWorkerID, &otherSupplierID))
	mustf(t, store.UpsertWorker(ctx,
		currentIdentityWorkerCapability(workerID, supplierID, job.PlacementRequirement, job)),
		"register frozen-cell worker: %v")
	mustf(t, store.UpsertWorker(ctx,
		currentIdentityWorkerCapability(otherWorkerID, otherSupplierID, job.PlacementRequirement, job)),
		"register comparison worker: %v")
	// Only the primary worker is registered for the exact cell/runtime frozen by
	// the server decision. The second worker advertises the same job, model and
	// current matrix digest but a different cell/runtime, so a model-only claim
	// filter would incorrectly accept it. Derive the fixture from the placement
	// instead of hard-coding the old embed lane: successful new-path mechanics
	// currently use the explicit TEST_ONLY combined-token batch authority.
	for _, update := range []struct {
		workerID uuid.UUID
		cellID   string
		runtime  string
	}{
		{workerID, job.PlacementRequirement.RuntimeCellID,
			job.PlacementRequirement.RuntimeID},
		{otherWorkerID, "test-only-wrong-cell", "test-only-wrong-runtime"},
	} {
		if _, err := pool.Exec(ctx, `
			UPDATE worker_authorized_capabilities
			   SET cell_id=$2, runtime_id=$3, job_type=$4, model_ref=$5, model_kind=$6
			 WHERE worker_id=$1`, update.workerID, update.cellID, update.runtime,
			job.JobType, job.ModelRef, job.PlacementRequirement.ModelKind); err != nil {
			t.Fatal(err)
		}
		if _, err := pool.Exec(ctx, `
			UPDATE workers
			   SET hw_class=$2, min_payout_usd_hr=0
			 WHERE id=$1`, update.workerID, job.PlacementRequirement.HWClasses[0]); err != nil {
			t.Fatal(err)
		}
	}
	mustf(t, store.SubmitJobTx(ctx, job, tasks), "submit frozen-runtime job: %v")

	wrong, err := store.ClaimTasksTx(ctx, WorkerAuth{
		WorkerID: otherWorkerID, SupplierID: otherSupplierID,
	})
	mustf(t, err, "wrong-cell claim: %v")
	// Shared DBs may have other claimable jobs; only THIS fixture job must be
	// refused to the wrong-cell worker.
	if wrong != nil && wrong.JobID == job.ID {
		t.Fatalf("worker outside frozen runtime candidate claimed fixture task %s", wrong.TaskID)
	}

	right, err := store.ClaimTasksTx(ctx, WorkerAuth{
		WorkerID: workerID, SupplierID: supplierID,
	})
	mustf(t, err, "frozen-cell claim: %v")
	if right == nil || right.JobID != job.ID {
		t.Fatalf("worker on frozen runtime candidate did not receive job: %+v", right)
	}
}

// A workload class is promoted by adding an arm to the classifier, and Step 26
// forbids exactly that being done ahead of the proof: "each promoted class
// reaches its declared proof rung end to end; prerequisites absent means
// BLOCKED_BY_PREREQUISITE, not scaffolding."
//
// The classifier already fails closed — an unknown runner/job pair is refused
// rather than defaulted — so the enforcement exists. What did not exist is
// anything noticing that the admitted SET grew. A new `case` arm is three lines
// and turns a refusal into a promotion silently: the class starts flowing
// through pricing, settlement and evidence with no canary, no quality contract
// and no declared rung behind it.
//
// So the set is pinned. Adding a fifth class means editing this list, and that
// edit is the moment to ask whether the class reaches its rung end to end.
// Merc's other non-inference work is deliberately NOT here — render assembly,
// LoRA evaluation and project decomposition all carry explicit
// *_NOT_EXECUTABLE refusals, which is the BLOCKED_BY_PREREQUISITE shape the
// step asks for, and image generation refuses with "no image runtime in
// service".
func TestOnlyProvenWorkloadClassesAreAdmittedByTheClassifier(t *testing.T) {
	raw, err := os.ReadFile("workload_classification.go")
	if err != nil {
		t.Fatalf("read classifier: %v", err)
	}
	body := string(raw)

	promoted := []string{
		`workloadClass = "embeddings"`,
		`workloadClass = "batch_generation"`,
		`workloadClass = "media_transcode"`,
		`workloadClass = "media_rendering"`,
	}
	for _, arm := range promoted {
		if !strings.Contains(body, arm) {
			t.Fatalf("classifier no longer promotes %q. A class leaving the admitted set is "+
				"a withdrawal and must be deliberate — Step 26's rollback clause, not a rename", arm)
		}
	}
	if got := strings.Count(body, "workloadClass = \""); got != len(promoted) {
		t.Fatalf("classifier admits %d workload classes, expected %d. A new arm promotes a class "+
			"into pricing, settlement and evidence; Step 26 requires it to reach its declared "+
			"proof rung end to end first, and forbids scaffolding it ahead of the proof. "+
			"If the class is genuinely proven, add it here with its rung named", got, len(promoted))
	}

	// The fail-closed default is the enforcement; without it an unrecognised
	// runner/job pair would fall through to an empty class rather than refuse.
	if !strings.Contains(body, "has no admitted workload classifier for runner=") {
		t.Fatal("the classifier no longer refuses an unrecognised runner/job pair by name; " +
			"an unadmitted workload must be BLOCKED_BY_PREREQUISITE, never a silent default")
	}
}
