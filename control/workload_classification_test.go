package main

import (
	"bytes"
	"context"
	"encoding/json"
	"math"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/google/uuid"
)

func validBatchWorkloadSubmit(t *testing.T) jobSubmit {
	t.Helper()
	sub, herr := normalizeAndValidateJobSubmit(jobSubmit{
		JobType: JobType{Type: "batch_infer", MaxTokens: 32},
		Model:   ModelRef{Ref: "llama-3.2-1b-instruct-q4"},
		Params:  json.RawMessage(`{"split_size":8}`),
		Constraints: JobConstraints{
			MaxDurationSecs: 3600,
		},
		Verification: VerificationPolicy{
			RedundancyFrac: 0.1,
			HoneypotFrac:   0.1,
			PayoutHoldSecs: 3600,
		},
		Tier:          "batch",
		MinReputation: 0.25,
		DeadlineSecs:  3600,
	})
	if herr != nil {
		t.Fatalf("normalize valid workload: %s", herr.msg)
	}
	return sub
}

func TestWorkloadDecisionIsServerClassifiedAndRevisionPinned(t *testing.T) {
	sub := validBatchWorkloadSubmit(t)
	decision, err := buildWorkloadDecision(sub, strings.Repeat("a", 64))
	if err != nil {
		t.Fatal(err)
	}
	if decision.WorkloadClass != "batch_generation" ||
		decision.RuntimeJobType != "batch_infer" ||
		decision.ModelRevision != "b69aef112e9f895e6f98d7ae0949f72ff09aa401" {
		t.Fatalf("decision did not derive class/job/revision from runtime authority: %+v", decision)
	}
	if len(decision.RuntimeCandidates) != 1 ||
		decision.RuntimeCandidates[0].CellID != "candle-metal-llama1-infer" {
		t.Fatalf("decision did not resolve one exact runtime cell: %+v", decision.RuntimeCandidates)
	}
	if !decision.Deterministic || decision.ExactResultCacheEligible ||
		!decision.PrefixReuseEligible || decision.InflightCoalescingEligible {
		t.Fatalf("decision over/under-stated reuse eligibility: %+v", decision)
	}
	if err := ValidateWorkloadDecisionSnapshot(decision); err != nil {
		t.Fatalf("untouched decision rejected: %v", err)
	}
}

func TestWorkloadBindingCoversEveryExecutionAssumption(t *testing.T) {
	base := validBatchWorkloadSubmit(t)
	inputSHA := strings.Repeat("b", 64)
	original, err := buildWorkloadDecision(base, inputSHA)
	if err != nil {
		t.Fatal(err)
	}

	mutations := map[string]func(*jobSubmit){
		"max tokens": func(s *jobSubmit) { s.JobType.MaxTokens = 64 },
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
			if err != nil {
				t.Fatal(err)
			}
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
	if err != nil {
		t.Fatal(err)
	}
	if same.BindingSHA256 != original.BindingSHA256 {
		t.Fatal("budget/delivery/idempotency metadata changed the execution-shape binding")
	}
}

func TestWorkloadDecisionRejectsTampering(t *testing.T) {
	decision, err := buildWorkloadDecision(validBatchWorkloadSubmit(t), strings.Repeat("d", 64))
	if err != nil {
		t.Fatal(err)
	}
	decision.MinimumMemoryGB = 0
	if err := ValidateWorkloadDecisionSnapshot(decision); err == nil {
		t.Fatal("tampered workload decision was accepted")
	}
}

func TestPlacementRequirementRejectsEveryClaimAuthorityMutation(t *testing.T) {
	sub := validBatchWorkloadSubmit(t)
	sub.Constraints.HWClasses = []string{"apple_silicon_max"}
	sub.Constraints.DataResidency = []string{"CA"}
	decision, err := buildWorkloadDecision(sub, strings.Repeat("e", 64))
	if err != nil {
		t.Fatal(err)
	}
	base, err := placementRequirementFor(sub, decision, 1.25)
	if err != nil {
		t.Fatal(err)
	}

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
		{"matrix", func(p *PlacementRequirement) { p.RuntimeMatrixSHA256 = strings.Repeat("f", 64) }},
		{"memory", func(p *PlacementRequirement) { p.MinMemoryGB++ }},
		{"hardware", func(p *PlacementRequirement) { p.HWClasses = []string{"apple_silicon_ultra"} }},
		{"residency", func(p *PlacementRequirement) { p.DataResidency = []string{"US"} }},
		{"reputation", func(p *PlacementRequirement) { p.MinReputation = 0.99 }},
		{"trusted tier", func(p *PlacementRequirement) { p.TrustedOnly = !p.TrustedOnly }},
		{"negative offered rate", func(p *PlacementRequirement) { p.OfferedRateUsdHr = -1 }},
		{"non-finite offered rate", func(p *PlacementRequirement) {
			p.OfferedRateUsdHr = float32(math.NaN())
		}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			mutant := base
			mutant.HWClasses = append([]string(nil), base.HWClasses...)
			mutant.DataResidency = append([]string(nil), base.DataResidency...)
			tc.mutate(&mutant)
			if err := validatePlacementRequirement(mutant, decision); err == nil {
				t.Fatalf("%s mutation survived placement validation", tc.name)
			}
		})
	}
}

func TestJobClaimProjectionRejectsFrozenWorkloadMismatch(t *testing.T) {
	sub := validBatchWorkloadSubmit(t)
	sub.Constraints.HWClasses = []string{"apple_silicon_max"}
	sub.Constraints.DataResidency = []string{"CA"}
	decision, err := buildWorkloadDecision(sub, strings.Repeat("f", 64))
	if err != nil {
		t.Fatal(err)
	}
	placement, err := placementRequirementFor(sub, decision, 1.25)
	if err != nil {
		t.Fatal(err)
	}
	base := jobRow{
		JobType:              decision.RuntimeJobType,
		ModelRef:             decision.Binding.Model.Ref,
		Tier:                 decision.Binding.Tier,
		MinMemoryGB:          float32(decision.MinimumMemoryGB),
		MaxDurationSecs:      decision.Binding.Constraints.MaxDurationSecs,
		HWClasses:            append([]string(nil), decision.Binding.Constraints.HWClasses...),
		DataResidency:        append([]string(nil), decision.Binding.Constraints.DataResidency...),
		MinReputation:        decision.Binding.MinReputation,
		OfferedRateUsdHr:     1.25,
		WorkloadDecision:     decision,
		PlacementRequirement: placement,
	}
	if err := validateJobClaimAuthority(&base); err != nil {
		t.Fatalf("valid claim projection rejected: %v", err)
	}
	tests := []struct {
		name   string
		mutate func(*jobRow)
	}{
		{"job type", func(j *jobRow) { j.JobType = "embed" }},
		{"model", func(j *jobRow) { j.ModelRef = "different-model" }},
		{"tier", func(j *jobRow) { j.Tier = "priority" }},
		{"memory", func(j *jobRow) { j.MinMemoryGB++ }},
		{"duration", func(j *jobRow) { j.MaxDurationSecs++ }},
		{"hardware", func(j *jobRow) { j.HWClasses = []string{"apple_silicon_ultra"} }},
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
	if err != nil {
		t.Fatal(err)
	}
	original, err := workloadDecisionDigest(decision)
	if err != nil {
		t.Fatal(err)
	}
	decision.ModelRevision = "different-revision"
	changed, err := workloadDecisionDigest(decision)
	if err != nil {
		t.Fatal(err)
	}
	if changed == original {
		t.Fatal("derived runtime authority changed without changing the full decision digest")
	}
}

func TestBatchExactReuseIsDisabledForCurrentAndHistoricalDecisions(t *testing.T) {
	sub := validBatchWorkloadSubmit(t)
	decision, err := buildWorkloadDecision(sub, strings.Repeat("1", 64))
	if err != nil {
		t.Fatal(err)
	}
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
	if err != nil {
		t.Fatal(err)
	}
	historical := decision
	historical.ModelRevision = strings.Repeat("5", 40)
	if err := ValidateFrozenWorkloadDecisionSnapshot(historical); err != nil {
		t.Fatalf("self-contained historical decision rejected: %v", err)
	}
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
	if err := validateWorkloadJSONL("embed", valid); err != nil {
		t.Fatalf("valid input rejected: %v", err)
	}
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
			if _, herr := normalizeAndValidateJobSubmit(sub); herr == nil {
				t.Fatal("invalid workload shape accepted")
			}
		})
	}

	sub := valid()
	sub.Constraints.HWClasses = []string{"apple_silicon_ultra", "apple_silicon_ultra"}
	sub.Constraints.DataResidency = []string{"ca", " CA "}
	normalized, herr := normalizeAndValidateJobSubmit(sub)
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
	if err != nil {
		t.Fatal(err)
	}
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
	if err != nil {
		t.Fatal(err)
	}
	if constrained != 1 {
		t.Fatalf("constraint-aware eligible workers=%d, want only the XZ trusted Max worker", constrained)
	}
	reputation, err := store.EligiblePoolReputationFor(ctx, "embed", "all-minilm-l6-v2", req)
	if err != nil {
		t.Fatal(err)
	}
	if reputation != 1 {
		t.Fatalf("constraint-aware pool reputation=%v, want 1.0", reputation)
	}

	req.HWClasses = []string{"apple_silicon_base"}
	none, err := store.EligibleWorkerCountFor(ctx, "embed", "all-minilm-l6-v2", req)
	if err != nil {
		t.Fatal(err)
	}
	if none != 0 {
		t.Fatalf("incompatible residency+hardware constraints reported %d eligible workers", none)
	}
}

func TestClaimUsesFrozenRuntimeCandidate(t *testing.T) {
	// Isolated: a shared queue hands unrelated jobs to both workers and masks
	// the frozen-candidate filter under test.
	ctx, store, pool := openIsolatedTestStore(t)
	fixture := seedMoneyPathFixture(t, ctx, store, pool, moneyPathSeedOpts{TaskCount: 1})

	// Only the primary worker is registered for the cell/runtime frozen by the
	// server decision. The second worker still advertises the same model and
	// current matrix digest, so a model-only claim filter would incorrectly
	// accept it.
	if _, err := pool.Exec(ctx, `
		UPDATE worker_authorized_capabilities
		   SET cell_id='candle-metal-minilm-embed', runtime_id='candle_metal'
		 WHERE worker_id=$1`, fixture.WorkerID); err != nil {
		t.Fatal(err)
	}

	tasks := makeTasks(fixture, 1)
	job := validJobRow(t, fixture, tasks)
	if err := store.SubmitJobTx(ctx, job, tasks); err != nil {
		t.Fatalf("submit frozen-runtime job: %v", err)
	}

	wrong, err := store.ClaimTasksTx(ctx, WorkerAuth{
		WorkerID: fixture.OtherWorkerID, SupplierID: fixture.OtherSupplierID,
	})
	if err != nil {
		t.Fatalf("wrong-cell claim: %v", err)
	}
	// Shared DBs may have other claimable jobs; only THIS fixture job must be
	// refused to the wrong-cell worker.
	if wrong != nil && wrong.JobID == fixture.JobID {
		t.Fatalf("worker outside frozen runtime candidate claimed fixture task %s", wrong.TaskID)
	}

	right, err := store.ClaimTasksTx(ctx, WorkerAuth{
		WorkerID: fixture.WorkerID, SupplierID: fixture.SupplierID,
	})
	if err != nil {
		t.Fatalf("frozen-cell claim: %v", err)
	}
	if right == nil || right.JobID != fixture.JobID {
		t.Fatalf("worker on frozen runtime candidate did not receive job: %+v", right)
	}
}
