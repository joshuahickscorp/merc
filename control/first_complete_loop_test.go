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

// A complete mechanics loop, end to end, through the PUBLIC API.
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
// What it does NOT claim: production admission. The checked-in batch receipt
// measures decode-output tokens while settlement uses combined input-plus-output
// tokens, so this test installs the one explicit in-memory TEST_ONLY authority
// that matches those semantics. It is also not candidate-bound. It cannot mint
// CANARY_PROVEN or establish a currently sellable production lane.
func TestFirstCompleteLoopThroughThePublicAPI(t *testing.T) {
	installBoundCataloguePublicationAuthorityForTest(t)
	installTestOnlyCombinedTokenAuthority(t)
	// What remains gating this mechanics test is hardware: a built agent binary
	// and a real engine. Both skips are honest and named.
	agentBinaryPath(t)
	// candle is the in-process runtime below, so it needs no HTTP engine. The
	// llama.cpp URL is passed through to the agent config for cells that do need it
	// and is not a precondition for this run.
	llamaURL := os.Getenv("MERC_LLAMA_EMBED_URL")

	strangerDeploymentInputs(t)

	artifacts := newArtifactHarness(t)
	ctx, store, pool := openIsolatedTestStore(t)

	// Publish the catalogue price authority the way BOOT does. Admission refuses a
	// model with no complete append-only price authority — correctly, since pricing
	// work against an unpublished price is how a buyer gets charged a number nobody
	// approved — and httptest does not run main()'s boot sequence.
	schedule, err := BuildCataloguePriceSchedule()
	mustf(t, err, "build catalogue price schedule: %v")
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
	const ceiling = 1.00
	submitBody := testOnlyBatchPublicRequest(strangerBatchCorpus, ceiling)
	submit := postJSONWithHeaders(t, srv.URL+"/v1/jobs", apiKey, map[string]string{
		"Idempotency-Key": uuid.NewString(),
	}, submitBody)
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
	mustf(t, err, "read supplier attribution: %v")
	for rows.Next() {
		var id uuid.UUID
		if err := rows.Scan(&id); err != nil {
			rows.Close()
			t.Fatalf("scan supplier attribution: %v", err)
		}
		paidSuppliers[id] = true
	}
	rows.Close()
	mustf(t, rows.Err(), "read supplier attribution: %v")
	if len(paidSuppliers) != 1 || !paidSuppliers[agent.supplierID] {
		t.Fatalf("credited suppliers %v, but %s is the one that executed the work",
			paidSuppliers, agent.supplierID)
	}
	paidSupplier := agent.supplierID

	// The decision Merc took is recorded, including where it placed the work.
	var routedCell, basis, mode, modeReason string
	if err := pool.QueryRow(loopCtx, `
		SELECT routed_cell_id,
		       COALESCE(NULLIF(selection_basis_v3, ''), selection_basis),
		       execution_mode, execution_mode_reason
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

	// Stranger-visible surfaces: invoice and receipt must be readable on the
	// public API and must not present gross platform take as true net profit.
	invoice := getJSON(t, srv.URL+"/v1/jobs/"+jobID.String()+"/invoice", apiKey)
	if invoice.status != http.StatusOK {
		t.Fatalf("GET invoice after settle: HTTP %d: %s", invoice.status, invoice.body)
	}
	receipt := getJSON(t, srv.URL+"/v1/jobs/"+jobID.String()+"/receipt", apiKey)
	if receipt.status != http.StatusOK {
		t.Fatalf("GET receipt after settle: HTTP %d: %s", receipt.status, receipt.body)
	}
	if inv, ok := receipt.json["invoice"].(map[string]any); ok {
		if _, hasTake := inv["platform_take_usd"]; hasTake {
			if _, hasGross := inv["platform_gross_spread_usd"]; !hasGross {
				t.Fatal("settled receipt exposes platform_take without platform_gross_spread")
			}
		}
		if _, bad := inv["true_net_profit_usd"]; bad {
			t.Fatal("settled invoice labels gross as true_net_profit_usd")
		}
	} else {
		t.Fatalf("settled receipt missing invoice: %s", receipt.body)
	}

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
	// random buyer, supplier, and job IDs. An operator may choose an explicit
	// diagnostic path, but writeFirstLoopReceipt refuses evidence/ while this loop
	// uses synthetic performance authority. Ordinary CI writes beside its other
	// ignored artifacts so a passing test leaves its source tree unchanged.
	path := strings.TrimSpace(os.Getenv("MERC_FIRST_COMPLETE_LOOP_RECEIPT_PATH"))
	if path == "" {
		path = filepath.Join("..", ".artifacts", "canary", "first-complete-loop.json")
	}
	return path
}

func writeFirstLoopReceipt(t *testing.T, loop firstLoopReceipt) {
	t.Helper()
	path := firstCompleteLoopReceiptPath()
	// A mechanics run backed by synthetic performance authority must never write
	// into evidence/, even when an operator supplied the old controlled-run path.
	// Refuse before creating a directory or writing any bytes.
	if strings.Contains(filepath.ToSlash(path), "/evidence/") || strings.HasPrefix(filepath.ToSlash(path), "evidence/") {
		t.Fatalf("TEST_ONLY combined-token mechanics receipt may not be written under evidence/: %s", path)
	}
	dir := filepath.Dir(path)
	mustf(t, os.MkdirAll(dir, 0o755), "create receipt directory: %v")
	payload := map[string]any{
		"schema_version":        1,
		"kind":                  "first_complete_loop",
		"harness":               "control/first_complete_loop_test.go",
		"runtime_matrix_sha256": generatedRuntimeMatrixSHA256,
		"loop":                  loop,
		"candidate_bound":       false,
		"authority_class":       "TEST_ONLY",
		"production_admission":  false,
		"limitations": []string{
			"Admission uses an in-memory TEST_ONLY combined-token benchmark authority; " +
				"this receipt cannot establish a sellable production lane.",
			"Runs against the working tree, not an immutable image at an exact " +
				"commit, so it cannot mint CANARY_PROVEN however complete the loop is.",
			"One buyer, one supplier, one job, one hardware class. Not a fleet claim.",
			"The buyer is funded by the sandbox credit grant, so the Stripe rails are " +
				"not exercised: no payment intent, no capture, no payout.",
		},
	}
	body, err := json.MarshalIndent(payload, "", "  ")
	mustf(t, err, "render receipt: %v")
	mustf(t, os.WriteFile(path, append(body, '\n'), 0o644), "write receipt: %v")
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
	must(t, err)
	req, err := http.NewRequest(http.MethodPost, url, bytes.NewReader(body))
	must(t, err)
	req.Header.Set("Content-Type", "application/json")
	if bearer != "" {
		req.Header.Set("Authorization", "Bearer "+bearer)
	}
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	resp, err := (&http.Client{Timeout: 120 * time.Second}).Do(req)
	mustf(t, err, "POST %s: %v", url)
	defer resp.Body.Close()
	var buf bytes.Buffer
	if _, err := buf.ReadFrom(resp.Body); err != nil {
		t.Fatal(err)
	}
	out := apiResponse{status: resp.StatusCode, body: buf.String()}
	_ = json.Unmarshal(buf.Bytes(), &out.json)
	return out
}

func getJSON(t *testing.T, url, bearer string) apiResponse {
	t.Helper()
	req, err := http.NewRequest(http.MethodGet, url, nil)
	if err != nil {
		t.Fatal(err)
	}
	if bearer != "" {
		req.Header.Set("Authorization", "Bearer "+bearer)
	}
	resp, err := (&http.Client{Timeout: 30 * time.Second}).Do(req)
	if err != nil {
		t.Fatalf("GET %s: %v", url, err)
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

func deleteJSON(t *testing.T, url, bearer string) apiResponse {
	t.Helper()
	req, err := http.NewRequest(http.MethodDelete, url, nil)
	if err != nil {
		t.Fatal(err)
	}
	if bearer != "" {
		req.Header.Set("Authorization", "Bearer "+bearer)
	}
	resp, err := (&http.Client{Timeout: 30 * time.Second}).Do(req)
	if err != nil {
		t.Fatalf("DELETE %s: %v", url, err)
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
