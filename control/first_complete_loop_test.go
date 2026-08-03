package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
)

// The first complete loop, end to end, through the PUBLIC API.
//
//	a stranger signs up
//	  -> receives a funded sandbox account and an API key
//	  -> submits a project
//	  -> Merc admits, prices and plans it
//	  -> a real agent on a real runtime executes it
//	  -> the result verifies
//	  -> the buyer is charged no more than the ceiling they accepted
//	  -> the supplier is owed exactly once
//	  -> Merc keeps a positive contribution
//	  -> and every one of those is readable from a receipt afterwards
//
// Why this is not the same test as the chain proofs that already exist: every one
// of those submits a TEST-CONSTRUCTED job row through store.SubmitJobTx. That
// skips signup, skips the API key, skips admission, skips the quote, skips the
// compute plan and skips the pricing decision — which is to say it skips
// everything a stranger actually encounters. Directed routing is production via
// POST /admin/runtime/jobs/directed; this loop still uses ordinary buyer submit
// because a stranger does not name a cell.
//
// This one goes through the API. Nothing is inserted into the database by the
// test except the supplier and worker credential a real supplier would be issued,
// and every assertion below is read back out of the control plane.
//
// What it does NOT claim: it is not candidate-bound. It runs against the tree, not
// against an immutable image at an exact commit, so it cannot mint CANARY_PROVEN.
// That distinction is the whole of scripts/go-closure-canary-rehearsal.sh and it is
// not smuggled in here.
func TestFirstCompleteLoopThroughThePublicAPI(t *testing.T) {
	// No longer gated on the pricing-authority defect. It used to refuse here with
	//
	//	physical pricing decision lacks executable composite authority: modeled
	//	supplier gross 0.102978 USD/hr is below the admission ceiling 0.104733
	//	USD/hr, so a worker admitted at that ceiling could not earn it
	//
	// and under that, once the arithmetic was exact enough to see them, two further
	// defects: a sub-second task duration truncated to a whole integer second, and a
	// supplier floor derived from the USD reference price while the entitlement was
	// denominated in the settlement currency. All three are fixed; the floor and the
	// entitlement are now one expression evaluated once, and the admission half of
	// this loop is asserted without any hardware at all by
	// TestAStrangerCanBeAdmittedForASubMicroJob.
	//
	// What remains gating this test is only hardware: a built agent binary and a
	// real engine. Both skips are honest and both are named.
	agentBinaryPath(t)
	// candle is the embed runtime below, and it needs no HTTP engine — the agent
	// loads the model in-process. The llama.cpp URL is passed through to the agent
	// config for the cells that do need it and is not a precondition for this run.
	llamaURL := os.Getenv("MERC_LLAMA_EMBED_URL")

	strangerDeploymentInputs(t)

	artifacts := newArtifactHarness(t)
	ctx, store, pool := openIsolatedTestStore(t)

	// Publish the catalogue price authority the way BOOT does. Admission refuses a
	// model with no complete append-only price authority — correctly, since pricing
	// work against an unpublished price is how a buyer gets charged a number nobody
	// approved — and httptest does not run main()'s boot sequence.
	schedule, err := BuildCataloguePriceSchedule()
	if err != nil {
		t.Fatalf("build catalogue price schedule: %v", err)
	}
	if _, err := store.ApplyRepricing(ctx, schedule); err != nil {
		t.Fatalf("publish catalogue price schedule: %v", err)
	}
	verifier := NewVerifier(store).WithStorage(artifacts.storage)
	srv := httptest.NewServer(NewServer(store, artifacts.storage, verifier, nil).Routes())
	t.Cleanup(srv.Close)

	// The background sweeps, because a deployment runs them and this test claims to
	// prove a deployment.
	//
	// Without them the loop reaches `status=verifying` and stops there forever, and
	// it took driving the whole thing to see why: finalization is attempted inline
	// on the LAST task commit, but two tasks committing at once contend for the
	// verification process capacity, so one returns 202 Pending. The other then
	// finalizes, finds not-all-tasks-done, and returns — and nothing inline is ever
	// scheduled to come back for the pending one. `verification-recovery` (2s) and
	// `job-finalize` (20s) are what close it, and only main() was starting them.
	//
	// stubPayout is what main() itself defaults to when no provider is configured,
	// so this is the production wiring rather than a test-only substitute.
	workersCtx, stopWorkers := context.WithCancel(context.Background())
	t.Cleanup(stopWorkers)
	workersDone := make(chan struct{})
	go func() {
		defer close(workersDone)
		NewWorkers(store, artifacts.storage, stubPayout{}).Run(workersCtx)
	}()
	t.Cleanup(func() {
		stopWorkers()
		<-workersDone
	})

	// The verification floor. Merc refuses a job it cannot verify — "no usable
	// honeypot is seeded for this workload" — which is the right answer and was hit
	// for real during the first Metal embed run. seedDemo installs the governed
	// embed honeypot AND its input object, so the probe the verifier fetches
	// actually exists.
	if err := seedDemo(ctx, pool, artifacts.storage); err != nil {
		t.Fatalf("seed the verification floor: %v", err)
	}

	// --- the supply side: a real agent, enrolled, on a real runtime -----------
	agent := launchAgent(t, ctx, store, pool, srv.URL, "candle", "candle_metal", llamaURL)
	waitForEnrolment(t, ctx, pool, agent)

	loopCtx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	t.Cleanup(cancel)

	// --- the stranger ---------------------------------------------------------
	email := fmt.Sprintf("stranger-%s@example.test", uuid.NewString())
	signup := postJSON(t, srv.URL+"/v1/signup", "", map[string]any{
		"email": email, "password": "a-stranger-password-1234",
	})
	if signup.status != http.StatusOK && signup.status != http.StatusCreated {
		t.Fatalf("signup: HTTP %d: %s", signup.status, signup.body)
	}
	apiKey, _ := signup.json["sandbox_key"].(string)
	if apiKey == "" {
		t.Fatalf("signup issued no sandbox key, so a stranger cannot call the API: %s",
			signup.body)
	}
	credit, _ := signup.json["free_credit_usd"].(float64)
	if credit <= 0 {
		t.Fatalf("signup granted %v credit; an unfunded stranger cannot complete the loop",
			credit)
	}
	t.Logf("stranger %s funded with $%.2f and holding an API key", email, credit)

	// --- the project ----------------------------------------------------------
	corpus := strings.Join([]string{
		`{"id":"0","text":"A verifiable compute network settles every task against a receipt."}`,
		`{"id":"1","text":"A stranger should not have to understand GPUs."}`,
		`{"id":"2","text":"The cheapest verified outcome is not the cheapest attempt."}`,
	}, "\n") + "\n"

	const ceiling = 1.00
	submit := postJSONWithHeaders(t, srv.URL+"/v1/jobs", apiKey, map[string]string{
		"Idempotency-Key": uuid.NewString(),
	}, map[string]any{
		"job_type": map[string]any{"type": "embed"},
		"model":    map[string]any{"kind": "hf", "ref": "all-minilm-l6-v2"},
		"tier":     "batch",
		"input":    corpus,
		"max_usd":  ceiling,
	})
	if submit.status != http.StatusOK && submit.status != http.StatusCreated &&
		submit.status != http.StatusAccepted {
		t.Fatalf("a stranger could not submit a project: HTTP %d: %s",
			submit.status, submit.body)
	}
	jobIDText, _ := submit.json["job_id"].(string)
	if jobIDText == "" {
		jobIDText, _ = submit.json["id"].(string)
	}
	jobID, err := uuid.Parse(jobIDText)
	if err != nil {
		t.Fatalf("submit returned no usable job id: %s", submit.body)
	}
	// Merc quoted the work before running it, and the quote is bounded by what the
	// buyer accepted. A ceiling that is only checked at settlement is not a ceiling.
	estimate, _ := submit.json["estimated_usd"].(float64)
	t.Logf("job %s admitted: estimate $%.9f against a $%.2f ceiling", jobID, estimate, ceiling)
	if estimate > ceiling {
		t.Fatalf("admitted an estimate of %.9f above the accepted ceiling %.2f",
			estimate, ceiling)
	}

	// --- the network runs it --------------------------------------------------
	waitForJobSettled(t, loopCtx, pool, jobID, "stranger")

	// --- what the loop actually did, read back out of the control plane -------
	var status string
	var actualUSD float64
	var currency string
	if err := pool.QueryRow(loopCtx, `
		SELECT status, COALESCE(actual_usd,0), currency FROM jobs WHERE id=$1`, jobID).
		Scan(&status, &actualUSD, &currency); err != nil {
		t.Fatalf("read job: %v", err)
	}
	if status != "complete" {
		t.Fatalf("job status %q", status)
	}
	if actualUSD > ceiling {
		t.Fatalf("buyer charged %.9f above the ceiling %.2f they accepted", actualUSD, ceiling)
	}

	var buyerMicros, supplierMicros, platformMicros int64
	var supplierRows, creditedTasks, executedTasks int
	if err := pool.QueryRow(loopCtx, `
		SELECT COALESCE((-sum(amount_usd) FILTER (WHERE kind='buyer_charge')*1000000)::bigint,0),
		       COALESCE((sum(amount_usd) FILTER (WHERE kind='supplier_credit')*1000000)::bigint,0),
		       COALESCE((sum(amount_usd) FILTER (WHERE kind='platform_take')*1000000)::bigint,0),
		       count(*) FILTER (WHERE kind='supplier_credit'),
		       count(DISTINCT task_id) FILTER (WHERE kind='supplier_credit')
		  FROM ledger_entries
		 WHERE task_id IN (SELECT id FROM tasks WHERE job_id=$1)`, jobID).
		Scan(&buyerMicros, &supplierMicros, &platformMicros,
			&supplierRows, &creditedTasks); err != nil {
		t.Fatalf("read ledger: %v", err)
	}
	if err := pool.QueryRow(loopCtx, `
		SELECT count(*) FROM tasks WHERE job_id=$1 AND status='complete'`, jobID).
		Scan(&executedTasks); err != nil {
		t.Fatalf("read executed task count: %v", err)
	}
	// ONCE PER EXECUTED TASK, which is not the same as once per job.
	//
	// This asserted `== 1` and failed on a correctly-settled loop: Merc buys
	// verification by executing extra tasks, so a three-record embed with one
	// honeypot is TWO executions, and the supplier who performed both is owed for
	// both — economic.SupplierPayoutPerTaskUSD x (primary + redundancy + honeypot)
	// is exactly what the pricing decision quotes the buyer for.
	//
	// The invariant that actually matters is that no task is paid twice, so that is
	// what is checked: one credit row per credited task, and a credited task for
	// every task that completed.
	if executedTasks == 0 {
		t.Fatal("no task reached complete, so nothing was executed to pay for")
	}
	if supplierRows != creditedTasks {
		t.Fatalf("%d supplier credit rows across %d distinct tasks; a task was paid twice",
			supplierRows, creditedTasks)
	}
	if creditedTasks != executedTasks {
		t.Fatalf("%d tasks completed but %d were credited; the supplier performed work "+
			"nobody owes them for", executedTasks, creditedTasks)
	}
	if supplierMicros <= 0 {
		t.Fatal("the supplier executed the work and is owed nothing")
	}
	if platformMicros <= 0 {
		t.Fatalf("Merc contribution is %d micros; the platform did not keep a margin",
			platformMicros)
	}
	if buyerMicros != supplierMicros+platformMicros {
		t.Fatalf("money did not conserve: buyer %d != supplier %d + platform %d",
			buyerMicros, supplierMicros, platformMicros)
	}

	// Every supplier who is owed is one who actually ran something. A single-row
	// scan here would have passed while a second, wrong supplier was also being
	// paid, so the set is compared rather than the first row.
	paidSuppliers := map[uuid.UUID]bool{}
	rows, err := pool.Query(loopCtx, `
		SELECT DISTINCT supplier_id FROM ledger_entries
		 WHERE kind='supplier_credit'
		   AND task_id IN (SELECT id FROM tasks WHERE job_id=$1)`, jobID)
	if err != nil {
		t.Fatalf("read supplier attribution: %v", err)
	}
	for rows.Next() {
		var id uuid.UUID
		if err := rows.Scan(&id); err != nil {
			rows.Close()
			t.Fatalf("scan supplier attribution: %v", err)
		}
		paidSuppliers[id] = true
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		t.Fatalf("read supplier attribution: %v", err)
	}
	if len(paidSuppliers) != 1 || !paidSuppliers[agent.supplierID] {
		t.Fatalf("credited suppliers %v, but %s is the one that executed the work",
			paidSuppliers, agent.supplierID)
	}
	paidSupplier := agent.supplierID

	// The decision Merc took is recorded, including where it placed the work.
	var routedCell, basis, mode, modeReason string
	if err := pool.QueryRow(loopCtx, `
		SELECT routed_cell_id, selection_basis, execution_mode, execution_mode_reason
		  FROM runtime_shadow_selections WHERE job_id=$1`, jobID).
		Scan(&routedCell, &basis, &mode, &modeReason); err != nil {
		t.Fatalf("the API path recorded no runtime decision for this job: %v", err)
	}
	if mode != string(ModePool) {
		t.Fatalf("execution mode %q for independent task fan-out", mode)
	}

	// And the runtime that ran it is named on the task, not inferred.
	var cell, runtimeID, engine, hwClass string
	var verification *string
	if err := pool.QueryRow(loopCtx, `
		SELECT COALESCE(runtime_cell_id,''), COALESCE(runtime_id,''),
		       COALESCE(execution_engine,''), COALESCE(execution_hw_class,''),
		       verification_outcome
		  FROM tasks WHERE job_id=$1 LIMIT 1`, jobID).
		Scan(&cell, &runtimeID, &engine, &hwClass, &verification); err != nil {
		t.Fatalf("read task provenance: %v", err)
	}
	if cell == "" || runtimeID == "" || engine == "" {
		t.Fatalf("the completed task names no runtime: cell=%q runtime=%q engine=%q",
			cell, runtimeID, engine)
	}

	t.Logf("LOOP CLOSED: buyer %d micros = supplier %d + Merc %d, on %s/%s (%s), "+
		"mode %s, basis %s, verification %v",
		buyerMicros, supplierMicros, platformMicros, runtimeID, cell, hwClass,
		mode, basis, derefOr(verification))

	writeFirstLoopReceipt(t, firstLoopReceipt{
		JobID: jobID.String(), Email: email, Currency: currency,
		CeilingUSD: ceiling, EstimateUSD: estimate, ChargedUSD: actualUSD,
		BuyerMicros: buyerMicros, SupplierMicros: supplierMicros,
		PlatformMicros: platformMicros, SupplierID: paidSupplier.String(),
		RuntimeCell: cell, RuntimeID: runtimeID, Engine: engine, HWClass: hwClass,
		ExecutionMode: mode, ExecutionModeReason: modeReason,
		SelectionBasis: basis, RoutedCell: routedCell,
		VerificationOutcome: derefOr(verification),
	})
}

type firstLoopReceipt struct {
	JobID               string  `json:"job_id"`
	Email               string  `json:"buyer_email"`
	Currency            string  `json:"currency"`
	CeilingUSD          float64 `json:"accepted_ceiling_usd"`
	EstimateUSD         float64 `json:"quoted_estimate_usd"`
	ChargedUSD          float64 `json:"buyer_charged_usd"`
	BuyerMicros         int64   `json:"buyer_debit_micros"`
	SupplierMicros      int64   `json:"supplier_credit_micros"`
	PlatformMicros      int64   `json:"merc_contribution_micros"`
	SupplierID          string  `json:"paid_supplier_id"`
	RuntimeCell         string  `json:"runtime_cell_id"`
	RuntimeID           string  `json:"runtime_id"`
	Engine              string  `json:"engine"`
	HWClass             string  `json:"hw_class"`
	ExecutionMode       string  `json:"execution_mode"`
	ExecutionModeReason string  `json:"execution_mode_reason"`
	SelectionBasis      string  `json:"selection_basis"`
	RoutedCell          string  `json:"routed_cell_id"`
	VerificationOutcome string  `json:"verification_outcome"`
}

func firstCompleteLoopReceiptPath() string {
	// A repeatable test must not rewrite a tracked historical receipt with fresh
	// random buyer, supplier, and job IDs. An operator who wants to preserve an
	// output supplies an explicit path in the controlled evidence run; ordinary CI
	// writes beside its other ignored artifacts so a passing test leaves its source
	// tree exactly as it found it.
	path := strings.TrimSpace(os.Getenv("MERC_FIRST_COMPLETE_LOOP_RECEIPT_PATH"))
	if path == "" {
		path = filepath.Join("..", ".artifacts", "canary", "first-complete-loop.json")
	}
	return path
}

func writeFirstLoopReceipt(t *testing.T, loop firstLoopReceipt) {
	t.Helper()
	path := firstCompleteLoopReceiptPath()
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("create receipt directory: %v", err)
	}
	payload := map[string]any{
		"schema_version":        1,
		"kind":                  "first_complete_loop",
		"harness":               "control/first_complete_loop_test.go",
		"runtime_matrix_sha256": generatedRuntimeMatrixSHA256,
		"loop":                  loop,
		"candidate_bound":       false,
		"limitations": []string{
			"Runs against the working tree, not an immutable image at an exact " +
				"commit, so it cannot mint CANARY_PROVEN however complete the loop is.",
			"One buyer, one supplier, one job, one hardware class. Not a fleet claim.",
			"The buyer is funded by the sandbox credit grant, so the Stripe rails are " +
				"not exercised: no payment intent, no capture, no payout.",
		},
	}
	// Default path is outside evidence/; only enforce the bound writer when the
	// destination is under evidence/ (operator-run path).
	if strings.Contains(filepath.ToSlash(path), "/evidence/") || strings.HasPrefix(filepath.ToSlash(path), "evidence/") {
		id, bin, err := DefaultBoundIdentity("..", "control/first_complete_loop_test.go",
			"embedded loop receipt", "embedded loop events")
		if err != nil {
			t.Fatalf("identity: %v", err)
		}
		if err := WriteBoundEvidenceJSON(EvidenceWriteRequest{
			RepoRoot: "..", Path: path, Payload: payload,
			Identity: id, BuildBinaryPath: bin,
		}); err != nil {
			t.Fatalf("write receipt: %v", err)
		}
	} else {
		body, err := json.MarshalIndent(payload, "", "  ")
		if err != nil {
			t.Fatalf("render receipt: %v", err)
		}
		if err := os.WriteFile(path, append(body, '\n'), 0o644); err != nil {
			t.Fatalf("write receipt: %v", err)
		}
	}
	t.Logf("first-complete-loop receipt written to %s", path)
}

func TestFirstCompleteLoopReceiptPathDefaultsOutsideTrackedEvidence(t *testing.T) {
	t.Setenv("MERC_FIRST_COMPLETE_LOOP_RECEIPT_PATH", "")
	if got, want := firstCompleteLoopReceiptPath(), filepath.Join("..", ".artifacts", "canary", "first-complete-loop.json"); got != want {
		t.Fatalf("default first-loop receipt path = %q, want %q", got, want)
	}
	t.Setenv("MERC_FIRST_COMPLETE_LOOP_RECEIPT_PATH", "evidence/canary/operator-run.json")
	if got, want := firstCompleteLoopReceiptPath(), "evidence/canary/operator-run.json"; got != want {
		t.Fatalf("explicit first-loop receipt path = %q, want %q", got, want)
	}
}

type apiResponse struct {
	status int
	body   string
	json   map[string]any
}

func postJSON(t *testing.T, url, bearer string, payload any) apiResponse {
	t.Helper()
	return postJSONWithHeaders(t, url, bearer, nil, payload)
}

func postJSONWithHeaders(
	t *testing.T, url, bearer string, headers map[string]string, payload any,
) apiResponse {
	t.Helper()
	body, err := json.Marshal(payload)
	if err != nil {
		t.Fatal(err)
	}
	req, err := http.NewRequest(http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	if bearer != "" {
		req.Header.Set("Authorization", "Bearer "+bearer)
	}
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	resp, err := (&http.Client{Timeout: 120 * time.Second}).Do(req)
	if err != nil {
		t.Fatalf("POST %s: %v", url, err)
	}
	defer resp.Body.Close()
	var buf bytes.Buffer
	if _, err := buf.ReadFrom(resp.Body); err != nil {
		t.Fatal(err)
	}
	out := apiResponse{status: resp.StatusCode, body: buf.String()}
	_ = json.Unmarshal(buf.Bytes(), &out.json)
	return out
}
