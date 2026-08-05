package main

import (
	"context"
	"fmt"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
)

// A paired cohort: the same workload driven through BOTH embed cells by two real
// agents, enough times that the cost of each is measured rather than sampled.
//
// This is the evidence the promotion gate was built to consume, and until it ran
// the gate could only be tested against seeded rows. The difference matters: a
// seeded row proves the arithmetic, and a cohort proves the arithmetic against
// what the engines actually do — cold loads, thermal state, queue depth and all.
//
// Opt-in. Forty real executions plus two agent enrolments is minutes of wall
// clock and both retained models are cold-loaded twice, so it is not something to
// put in front of every `go test`. MERC_PAIRED_COHORT=1 runs it; the skip is
// registered in scripts/allowed-test-skips.txt so the absence stays visible.
//
// What it does NOT prove: that the CHALLENGER is safe to route. That is the
// promotion gate's decision, and this test asserts the gate reaches a decision
// with reasons — not that the decision is yes.
// cohortRecordsPerTask is the batch size each cohort task embeds. Batch 32 is on
// the measured curve in evidence/perf/runtime-benchmarks/embed-cell-candle-vs-llama-cpp-r1.json,
// where the two engines are 2.7x apart — far enough that a real difference cannot
// be mistaken for timer noise.
const cohortRecordsPerTask = 32

func TestPairedCohortMeasuresCostAndRegret(t *testing.T) {
	if os.Getenv("MERC_PAIRED_COHORT") != "1" {
		t.Skip("MERC_PAIRED_COHORT is not 1; the paired cohort is an opt-in " +
			"multi-minute run against two real agents")
	}
	agentBinaryPath(t)
	llamaURL := os.Getenv("MERC_LLAMA_EMBED_URL")
	if llamaURL == "" {
		t.Skip("MERC_LLAMA_EMBED_URL is not set; the llama.cpp agent has no engine to reach")
	}
	t.Setenv("MERC_VERIFICATION_SAMPLE_SECRET",
		"paired-cohort-verification-sampling-secret-0123456789")
	artifacts := newArtifactHarness(t)

	ctx, store, pool := openIsolatedMoneyPathStore(t)
	// A real verifier, for the reason TestBothAgentsSettleThroughTheProductionPath
	// gives: with a nil one the commit path leases the verification work and then
	// cannot act on it, and a task that never reaches a terminal outcome never
	// reaches `complete` — so the cost query would find nothing and report a clean
	// zero-sample cohort.
	verifier := NewVerifier(store).WithStorage(artifacts.storage)
	srv := httptest.NewServer(NewServer(store, artifacts.storage, verifier, nil).Routes())
	t.Cleanup(srv.Close)

	candle := launchAgent(t, ctx, store, pool, srv.URL, "candle", "candle_metal", llamaURL)
	waitForEnrolment(t, ctx, pool, candle)
	llama := launchAgent(t, ctx, store, pool, srv.URL, "llama_cpp", "llama_cpp_metal", llamaURL)
	waitForEnrolment(t, ctx, pool, llama)

	cohortCtx, cancel := context.WithTimeout(context.Background(), 20*time.Minute)
	t.Cleanup(cancel)

	// A BATCH of records, not one.
	//
	// The first run of this cohort used a single record and measured 3.0000 ms per
	// unit on both cells — identical to four decimals, which is not two engines
	// agreeing, it is a task too small to separate them. The retained bench receipt
	// puts llama.cpp at 2,179 texts/sec against candle's 326 at batch 8, and that
	// difference only appears above batch 1. Identical input on both cells is what
	// makes the costs comparable; a batch is what makes them different.
	var corpusLines []byte
	for i := 0; i < cohortRecordsPerTask; i++ {
		corpusLines = append(corpusLines, []byte(fmt.Sprintf(
			`{"id":"%d","text":"Merc settles every task against a receipt, record %d."}`+"\n",
			i, i))...)
	}
	corpus := corpusLines

	type cohortCell struct {
		name, cell, runtimeID string
		agent                 *enrolledAgent
	}
	cells := []cohortCell{
		{"candle", candleEmbedCell, "candle_metal", candle},
		{"llama_cpp", llamaEmbedCell, "llama_cpp_metal", llama},
	}

	// Submit the whole cohort before waiting on any of it. Submitting and waiting
	// per job would serialise 40 executions behind the agents' poll interval and
	// turn a two-minute cohort into a twenty-minute one.
	var all []cohortSubmission
	for _, c := range cells {
		for i := 0; i < minCellCostSamples; i++ {
			f := seedMoneyPathFixture(t, cohortCtx, store, pool, moneyPathSeedOpts{TaskCount: 1})
			tasks := makeTasks(f, 1)
			// Units, so the cost query has a denominator. Without this the task
			// persists NULL and is excluded from every measurement.
			//
			// The fixture's job row declares EconomicInputRecords from its PRIMARY
			// task count rather than from the corpus, so the job says one record while
			// the task expects cohortRecordsPerTask. No constraint fires — the
			// cross-check only applies to the streamed-submit path — and the number
			// under test is the task's, which is what the cost query divides by.
			tasks[0].ExpectedOutputRecords = cohortRecordsPerTask
			f.TaskIDs = []uuid.UUID{tasks[0].ID}
			job := validJobRowDirected(t, f, tasks, c.cell)
			for _, key := range []string{job.InputRef, tasks[0].InputRef} {
				if err := artifacts.storage.PutObject(
					cohortCtx, key, corpus, "application/x-ndjson"); err != nil {
					t.Fatalf("%s[%d]: upload %s: %v", c.name, i, key, err)
				}
			}
			mustf(t, store.SubmitJobTx(cohortCtx, job, tasks), "%s[%d]: submit: %v", c.name, i)
			// Recorded the way the API records it, so the regret read has decisions
			// to pair costs with. createJob does exactly this after the submit
			// transaction commits.
			shadow, err := planShadowSelection(job.WorkloadDecision)
			mustf(t, err, "%s[%d]: shadow selection: %v", c.name, i)
			if len(shadow.Considered) > 1 {
				byHW, costErr := store.MeasuredCellCostsByHardware(
					cohortCtx, shadow.JobType, shadow.ModelRef)
				if costErr != nil {
					t.Fatalf("%s[%d]: measured cost: %v", c.name, i, costErr)
				}
				shadow = shadow.rankedByMeasuredCost(byHW)
			}
			mustf(t, store.RecordShadowSelection(cohortCtx, f.JobID.String(), shadow), "%s[%d]: record shadow selection: %v", c.name, i)
			all = append(all, cohortSubmission{cell: c.cell, jobID: f.JobID, taskID: tasks[0].ID})
		}
	}
	t.Logf("submitted %d jobs across %d cells", len(all), len(cells))

	terminal := waitForCohortTerminal(t, cohortCtx, pool, cohortTaskIDs(all))
	t.Logf("terminal after wait: %v", terminal)

	byHW, err := store.MeasuredCellCostsByHardware(cohortCtx, "embed", "all-minilm-l6-v2")
	mustf(t, err, "measured cell costs: %v")
	hw := comparableHardwareFor(byHW, []string{candleEmbedCell, llamaEmbedCell})
	if hw == "" {
		t.Fatalf("no hardware class carries a measured cost for both cells: %s",
			string(mustJSON(byHW)))
	}
	costs := byHW[hw]
	for _, c := range cells {
		got := costs[c.cell]
		if !got.Measured {
			t.Fatalf("%s has %d of %d samples needed on %s",
				c.cell, got.Samples, minCellCostSamples, hw)
		}
		if got.TerminalFails > 0 {
			t.Errorf("%s failed %d of %d terminal attempts during the cohort",
				c.cell, got.TerminalFails, got.TerminalAttempts)
		}
		cost, ok := got.ExpectedVerifiedOutcomeUSDPerUnit()
		if !ok {
			t.Fatalf("%s has no verified-outcome cost: %+v", c.cell, got)
		}
		t.Logf("%s on %s: %d samples, %.4f ms/unit, %.9f USD/unit verified, "+
			"verification %d/%d failed, terminal %d/%d failed",
			c.cell, hw, got.Samples, got.MedianMsPerUnit, cost,
			got.VerificationFails, got.VerificationSamples,
			got.TerminalFails, got.TerminalAttempts)
	}

	regret, _, err := store.SelectorRegretForScope(cohortCtx, cellCostScope{
		JobType: "embed", ModelRef: "all-minilm-l6-v2", HWClass: hw,
	})
	mustf(t, err, "selector regret: %v")
	if regret.Decisions != len(all) {
		t.Errorf("regret saw %d decisions, %d were recorded", regret.Decisions, len(all))
	}
	if regret.ScoredDecisions == 0 {
		t.Fatalf("no decision was scored, so the cohort produced no regret: %+v", regret)
	}
	if regret.TotalRegretPerUnit < 0 {
		t.Errorf("negative total regret is arithmetically impossible: %+v", regret)
	}
	t.Logf("regret: %d decisions, %d scored, %d unmeasured, %d diverged, "+
		"mean %.9f USD/unit, max %.9f, cheapest %q",
		regret.Decisions, regret.ScoredDecisions, regret.UnmeasuredDecisions,
		regret.DivergedDecisions, regret.MeanRegretPerUnit, regret.MaxRegretPerUnit,
		regret.CheapestCell)

	// The gate, on real evidence. Either verdict is a pass for this test; a gate
	// that could only ever say yes would not be a gate.
	evidence, err := store.EvaluateCellPromotion(cohortCtx, CellPromotionScope{
		JobType: "embed", ModelRef: "all-minilm-l6-v2", HWClass: hw,
		LatencyClass: "standard_batch", RuntimeID: "llama_cpp_metal",
		CellID: llamaEmbedCell, QualityTier: "OUTCOME_EQUIVALENT",
		Verification: "cosine",
	}, candleEmbedCell, time.Now())
	mustf(t, err, "evaluate promotion: %v")
	ref, err := evidence.ReceiptRef()
	mustf(t, err, "receipt ref: %v")
	t.Logf("promotion gate: passed=%v saving=%.4f latency_ratio=%.3f refusals=%v receipt=%s",
		evidence.Passed(), evidence.SavingFraction, evidence.LatencyRatio,
		evidence.Refusals, ref)

	writeCohortReceipt(t, hw, costs, regret, evidence)
}

// cohortSubmission is one submitted job in the cohort.
type cohortSubmission struct {
	cell   string
	jobID  uuid.UUID
	taskID uuid.UUID
}

func cohortTaskIDs(in []cohortSubmission) []uuid.UUID {
	out := make([]uuid.UUID, 0, len(in))
	for _, s := range in {
		out = append(out, s.taskID)
	}
	return out
}

// waitForCohortTerminal blocks until every task is terminal, reporting the last
// census it saw. It does NOT fail on a timeout: a cohort that is 38 of 40 done is
// still evidence, and the sample-count assertions above decide whether it is
// enough. Failing here would replace a specific "candle has 19 of 20 samples"
// with a generic "timed out".
func waitForCohortTerminal(
	t *testing.T, ctx context.Context, pool *pgxpool.Pool, taskIDs []uuid.UUID,
) map[string]int {
	t.Helper()
	deadline := time.Now().Add(12 * time.Minute)
	census := map[string]int{}
	for time.Now().Before(deadline) {
		rows, err := pool.Query(ctx, `
			SELECT status, COUNT(*) FROM tasks WHERE id = ANY($1) GROUP BY status`, taskIDs)
		mustf(t, err, "cohort census: %v")
		census = map[string]int{}
		for rows.Next() {
			var status string
			var n int
			if err := rows.Scan(&status, &n); err != nil {
				rows.Close()
				t.Fatalf("cohort census scan: %v", err)
			}
			census[status] = n
		}
		rows.Close()
		if census["complete"]+census["failed"]+census["cancelled"] == len(taskIDs) {
			return census
		}
		time.Sleep(2 * time.Second)
	}
	t.Logf("cohort did not fully terminate inside the deadline: %v", census)
	return census
}

// writeCohortReceipt persists what the cohort measured, so a promotion argued
// from it later can be checked against the run rather than against a memory of it.
func writeCohortReceipt(
	t *testing.T, hwClass string, costs map[string]MeasuredCellCost,
	regret SelectorRegret, evidence CellPromotionEvidence,
) {
	t.Helper()
	dir := filepath.Join("..", "evidence", "perf", "selector")
	mustf(t, os.MkdirAll(dir, 0o755), "create receipt directory: %v")
	receipt := map[string]any{
		"schema_version":        1,
		"kind":                  "paired_cell_cohort",
		"harness":               "control/paired_cohort_test.go",
		"runtime_matrix_sha256": generatedRuntimeMatrixSHA256,
		"hardware_class":        hwClass,
		"samples_required":      minCellCostSamples,
		"measured_cost":         costs,
		"selector_regret":       regret,
		"promotion_evidence":    evidence,
		"limitations": []string{
			"One host, one hardware class; not a fleet claim.",
			"Verification is Merc's ordinary sampled path, not a governed-reference " +
				"comparison against an approved output.",
			"Storage, egress, energy, depreciation and refund risk are not measured " +
				"by this cohort and are reported as unknown by the cost model.",
		},
	}
	path := filepath.Join(dir, "paired-cohort-embed.json")
	id, bin, err := DefaultBoundIdentity("..", "control/paired_cohort_test.go",
		"embedded measured_cost + promotion_evidence", "embedded cohort samples")
	mustf(t, err, "identity: %v")
	if err := WriteBoundEvidenceJSON(EvidenceWriteRequest{
		RepoRoot: "..", Path: path, Payload: receipt,
		Identity: id, BuildBinaryPath: bin,
	}); err != nil {
		t.Fatalf("write receipt: %v", err)
	}
	t.Logf("cohort receipt written to %s", path)
}
