//go:build integration

package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
)

// TestL12RoutableInferLoopThroughThePublicAPI closes buyer → claim → execute →
// verify → settle on the one cell that currently binds
// (candle-metal-llama1-infer / r6), using the real agent binary and production
// authority. It does not install TEST_ONLY publication. A second job then
// commits substituted bytes onto a seeded honeypot and must be REJECTED.
//
// Plane: local isolated database + httptest control + this host's Metal agent.
// Does not satisfy EXTERNAL_ALPHA_PROVEN.
func TestL12RoutableInferLoopThroughThePublicAPI(t *testing.T) {
	if !advertisedRuntimeCell("candle-metal-llama1-infer") {
		t.Fatal("candle-metal-llama1-infer is not advertised; r6 authority did not bind")
	}
	agentBinaryPath(t)
	strangerDeploymentInputs(t)
	installSettlementCurrencyForTest(t, "usd")
	t.Setenv("MERC_SANDBOX_PROFILE", l12SandboxProfile(t))
	t.Setenv("MERC_MODEL_CACHE", filepath.Join(os.Getenv("HOME"), ".cache", "huggingface", "hub"))

	artifacts := newArtifactHarness(t)
	ctx, store, pool := openIsolatedTestStore(t)

	schedule, err := BuildCataloguePriceSchedule()
	mustf(t, err, "build catalogue price schedule: %v")
	if _, err := store.ApplyRepricing(ctx, schedule); err != nil {
		t.Fatalf("publish catalogue price schedule: %v", err)
	}

	honeypot := l12LoadHoneypot(t)
	t.Setenv("MERC_BATCH_INFER_HONEYPOT_ANSWER", honeypot.Answer)
	t.Setenv("MERC_BATCH_INFER_HONEYPOT_ANSWER_CLASS", honeypot.Class)
	if err := seedBatchInferHoneypot(ctx, pool, artifacts.storage); err != nil {
		t.Fatalf("seed batch_infer honeypot: %v", err)
	}

	verifier := NewVerifier(store).WithStorage(artifacts.storage)
	srv := httptest.NewServer(NewServer(store, artifacts.storage, verifier, nil).Routes())
	t.Cleanup(srv.Close)

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

	llamaURL := os.Getenv("MERC_LLAMA_EMBED_URL")
	// Serialise startup benches: two Metal processes measuring at once
	// starve the infer sweep and register without a matching benchmark.
	agent := launchAgent(t, ctx, store, pool, srv.URL, "candle-a", "candle_metal", llamaURL)
	waitForEnrolment(t, ctx, pool, agent)
	peer := launchAgent(t, ctx, store, pool, srv.URL, "candle-b", "candle_metal", llamaURL)
	waitForEnrolment(t, ctx, pool, peer)
	// launchAgent issues an unbound staging token. Ordinary claim requires an
	// active device-bound credential (the same bar seed/demo uses). Bind the
	// operator-controlled tokens rather than skipping containment.
	for i, w := range []*enrolledAgent{agent, peer} {
		if _, err := pool.Exec(ctx, `
			UPDATE worker_tokens
			   SET device_key_algorithm='p256',
			       device_public_key=$1,
			       device_fingerprint=$2
			 WHERE worker_id=$3 AND revoked=false`,
			seedDevicePublicKey(),
			fmt.Sprintf("l12-operator-metal-%d", i+1),
			w.workerID); err != nil {
			t.Fatalf("bind operator worker token: %v", err)
		}
	}

	loopCtx, cancel := context.WithTimeout(context.Background(), 12*time.Minute)
	t.Cleanup(cancel)

	email := fmt.Sprintf("l12-rehearsal-%s@example.test", uuid.NewString())
	signup := postJSON(t, srv.URL+"/v1/signup", "", map[string]any{
		"email": email, "password": "l12-rehearsal-password-1234",
	})
	if signup.status != http.StatusOK && signup.status != http.StatusCreated {
		t.Fatalf("signup: HTTP %d: %s", signup.status, signup.body)
	}
	apiKey, _ := signup.json["sandbox_key"].(string)
	if apiKey == "" {
		t.Fatalf("signup issued no sandbox key: %s", signup.body)
	}

	const ceiling = 1.00
	body := map[string]any{
		"job_type": map[string]any{"type": "batch_infer", "max_tokens": 16},
		"model":    map[string]any{"kind": "gguf", "ref": "llama-3.2-1b-instruct-q4"},
		"tier":     "batch",
		"input":    `{"id":"l12-0","prompt":"operator-controlled l12 infer rehearsal"}` + "\n",
		"max_usd":  ceiling,
		"verification": map[string]any{
			"redundancy_frac": 1.0,
			"honeypot_frac":   0.1,
		},
	}
	quote := postJSON(t, srv.URL+"/v1/quote", apiKey, body)
	if quote.status != http.StatusOK {
		t.Fatalf("quote infer: HTTP %d: %s", quote.status, quote.body)
	}
	submit := postJSONWithHeaders(t, srv.URL+"/v1/jobs", apiKey, map[string]string{
		"Idempotency-Key": "l12-accept-" + uuid.NewString(),
	}, body)
	if submit.status != http.StatusOK && submit.status != http.StatusCreated &&
		submit.status != http.StatusAccepted {
		t.Fatalf("submit infer: HTTP %d: %s", submit.status, submit.body)
	}
	jobIDText, _ := submit.json["job_id"].(string)
	if jobIDText == "" {
		jobIDText, _ = submit.json["id"].(string)
	}
	jobID, err := uuid.Parse(jobIDText)
	if err != nil {
		t.Fatalf("submit returned no job id: %s", submit.body)
	}
	waitForJobSettled(t, loopCtx, pool, jobID, "l12-accept")

	var status, cell, outcome string
	var actualUSD float64
	if err := pool.QueryRow(loopCtx, `
		SELECT j.status, COALESCE(j.actual_usd,0),
		       COALESCE(t.runtime_cell_id,''), COALESCE(t.verification_outcome,'')
		  FROM jobs j JOIN tasks t ON t.job_id=j.id
		 WHERE j.id=$1
		 ORDER BY t.is_honeypot DESC
		 LIMIT 1`, jobID).Scan(&status, &actualUSD, &cell, &outcome); err != nil {
		t.Fatalf("read accept job: %v", err)
	}
	if status != "complete" {
		t.Fatalf("accept job status %q", status)
	}
	if cell != "candle-metal-llama1-infer" {
		t.Fatalf("routed cell %q, want candle-metal-llama1-infer", cell)
	}
	if outcome != "pass" && outcome != "pass_with_penalty" {
		t.Fatalf("accept verification_outcome %q", outcome)
	}

	var creditRows, creditedTasks, executedTasks int
	if err := pool.QueryRow(loopCtx, `
		SELECT count(*) FILTER (WHERE kind='supplier_credit'),
		       count(DISTINCT task_id) FILTER (WHERE kind='supplier_credit')
		  FROM ledger_entries
		 WHERE task_id IN (SELECT id FROM tasks WHERE job_id=$1)`, jobID).
		Scan(&creditRows, &creditedTasks); err != nil {
		t.Fatalf("read accept ledger: %v", err)
	}
	if err := pool.QueryRow(loopCtx, `
		SELECT count(*) FROM tasks WHERE job_id=$1 AND status='complete'`, jobID).
		Scan(&executedTasks); err != nil {
		t.Fatalf("read executed: %v", err)
	}
	if executedTasks == 0 || creditRows != creditedTasks || creditedTasks != executedTasks {
		t.Fatalf("settlement not exactly-once: credits=%d distinct=%d executed=%d",
			creditRows, creditedTasks, executedTasks)
	}

	l12WriteReceipt(t, "buyer-execution", map[string]any{
		"status": "PASS", "plane": "local", "job_id": jobID.String(),
		"quote_http": quote.status, "submit_http": submit.status,
		"cell": cell, "verification_outcome": outcome,
	})
	l12WriteReceipt(t, "supplier-execution", map[string]any{
		"status": "PASS", "plane": "local", "job_id": jobID.String(),
		"worker_id": agent.workerID.String(), "cell": cell,
	})
	l12WriteReceipt(t, "verification-accept", map[string]any{
		"status": "PASS", "plane": "local", "job_id": jobID.String(),
		"verification_outcome": outcome,
	})
	l12WriteReceipt(t, "settlement", map[string]any{
		"status": "PASS", "plane": "local", "job_id": jobID.String(),
		"executed_tasks": executedTasks, "supplier_credit_rows": creditRows,
		"actual_usd": actualUSD, "settled_exactly_once": creditRows == executedTasks,
	})

	// --- reject: substituted honeypot result on a live commit ---------------
	for _, w := range []*enrolledAgent{agent, peer} {
		if w.cmd.Process != nil {
			_ = w.cmd.Process.Kill()
			_, _ = w.cmd.Process.Wait()
		}
	}
	rejectToken, rejectWorker := l12RegisterRejectWorker(t, ctx, store, pool, srv.URL)
	rejectSubmit := postJSONWithHeaders(t, srv.URL+"/v1/jobs", apiKey, map[string]string{
		"Idempotency-Key": "l12-reject-" + uuid.NewString(),
	}, body)
	if rejectSubmit.status != http.StatusOK && rejectSubmit.status != http.StatusCreated &&
		rejectSubmit.status != http.StatusAccepted {
		t.Fatalf("reject submit: HTTP %d: %s", rejectSubmit.status, rejectSubmit.body)
	}
	rejectJobText, _ := rejectSubmit.json["job_id"].(string)
	if rejectJobText == "" {
		rejectJobText, _ = rejectSubmit.json["id"].(string)
	}
	rejectJob, err := uuid.Parse(rejectJobText)
	if err != nil {
		t.Fatalf("reject job id: %s", rejectSubmit.body)
	}
	l12CommitSubstitutedHoneypot(t, srv.URL, rejectToken, rejectJob)
	deadline := time.Now().Add(2 * time.Minute)
	var failEvents int
	for time.Now().Before(deadline) {
		if err := pool.QueryRow(ctx, `
			SELECT count(*) FROM verification_events
			 WHERE job_id=$1 AND kind IN ('honeypot_fail','honeypot_class_mismatch')`,
			rejectJob).Scan(&failEvents); err != nil {
			t.Fatalf("read reject events: %v", err)
		}
		if failEvents > 0 {
			break
		}
		time.Sleep(200 * time.Millisecond)
	}
	if failEvents == 0 {
		t.Fatal("substituted honeypot was not REJECTED")
	}
	l12WriteReceipt(t, "verification-reject", map[string]any{
		"status": "PASS", "plane": "local", "job_id": rejectJob.String(),
		"worker_id": rejectWorker.String(), "honeypot_fail_events": failEvents,
	})
}

func l12SandboxProfile(t *testing.T) string {
	t.Helper()
	if p := strings.TrimSpace(os.Getenv("MERC_SANDBOX_PROFILE")); p != "" {
		if _, err := os.Stat(p); err == nil {
			return p
		}
	}
	fallback := "/tmp/merc-l12/merc-agent.sb"
	if _, err := os.Stat(fallback); err == nil {
		return fallback
	}
	return repoSandboxProfilePath(t)
}

type l12Honeypot struct {
	Answer string
	Class  string
}

func l12LoadHoneypot(t *testing.T) l12Honeypot {
	t.Helper()
	raw, err := os.ReadFile("/tmp/merc-l12/honeypot.json")
	if err != nil {
		t.Fatalf("measure honeypot first (merc-agent honeypot-answer): %v", err)
	}
	var doc map[string]any
	mustf(t, json.Unmarshal(raw, &doc), "parse honeypot: %v")
	answer, _ := doc["known_answer_utf8"].(string)
	class, _ := doc["answer_class"].(string)
	if answer == "" || class == "" {
		t.Fatalf("honeypot file missing answer/class: %s", raw)
	}
	return l12Honeypot{Answer: answer, Class: class}
}

func l12RegisterRejectWorker(
	t *testing.T, ctx context.Context, store *Store, pool *pgxpool.Pool, base string,
) (string, uuid.UUID) {
	t.Helper()
	supplierID, workerID := uuid.New(), uuid.New()
	if _, err := pool.Exec(ctx, `
		INSERT INTO suppliers (id,email,status,reputation,completed_tasks)
		VALUES ($1,$2,'active',0.95,100)`,
		supplierID, "l12-reject-"+uuid.NewString()+"@proof.test"); err != nil {
		t.Fatalf("seed reject supplier: %v", err)
	}
	token, err := store.IssueDeviceBoundWorkerToken(ctx, workerID, supplierID, "l12-reject-device")
	mustf(t, err, "issue reject token: %v")
	_, _, bench, err := currentRuntimeCellBenchmarkIdentity("candle-metal-llama1-infer")
	mustf(t, err, "current infer identity: %v")
	now := uint64(time.Now().UTC().Unix())
	cap := map[string]any{
		"hw_class":              bench.HWClass,
		"engine":                "candle",
		"build_hash":            bench.EngineBuildHash,
		"build_identity_policy": bench.EngineBuildIdentityPolicy,
		"hardware_identity":     bench.HardwareIdentity,
		"memory_gb":             96,
		"memory_bw_gbps":        800,
		"supported_jobs":        []string{"batch_infer"},
		"supported_models":      []string{"llama-3.2-1b-instruct-q4"},
		"min_payout_usd_hr":     0.0,
		"agent_version":         "0.1.0",
		"os_version":            "macos",
		"sandboxed":             true,
		"unsandboxed_opt_in":    false,
		"agent_session_id":      uuid.NewString(),
		"benchmarks": []map[string]any{{
			"model_id":      "llama-3.2-1b-instruct-q4",
			"job_type":      "batch_infer",
			"tps":           bench.Throughput["candle_metal"].UnitsPerSecAtOperatingBatch,
			"eps":           0,
			"p99_ms":        20,
			"thermal_ok":    true,
			"unit":          "tokens",
			"unit_scope":    "token_like_input_plus_max_output_tokens",
			"measured_unix": now,
		}},
	}
	reg := postJSONWithHeaders(t, base+"/v1/worker/register", "", map[string]string{
		"X-Worker-Token": token,
	}, cap)
	if reg.status != http.StatusOK {
		t.Fatalf("reject worker register: HTTP %d: %s", reg.status, reg.body)
	}
	return token, workerID
}

func l12CommitSubstitutedHoneypot(t *testing.T, base, token string, jobID uuid.UUID) {
	t.Helper()
	deadline := time.Now().Add(90 * time.Second)
	var dispatch map[string]any
	for time.Now().Before(deadline) {
		req, err := http.NewRequest(http.MethodGet, base+"/v1/worker/poll?wait_ms=1000", nil)
		must(t, err)
		req.Header.Set("X-Worker-Token", token)
		resp, err := http.DefaultClient.Do(req)
		mustf(t, err, "poll: %v")
		raw, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		if resp.StatusCode == http.StatusNoContent || len(raw) == 0 {
			continue
		}
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("poll HTTP %d: %s", resp.StatusCode, raw)
		}
		var doc map[string]any
		if json.Unmarshal(raw, &doc) != nil {
			continue
		}
		dispatch = doc
		break
	}
	if dispatch == nil {
		t.Fatal("reject worker never received a task")
	}
	taskID, _ := dispatch["task_id"].(string)
	resultKey, _ := dispatch["result_key"].(string)
	outputURL, _ := dispatch["output_url"].(string)
	if taskID == "" || outputURL == "" {
		t.Fatalf("dispatch missing task/output: %v", dispatch)
	}
	garbage := []byte(`{"job_type":"batch_infer","model":"llama-3.2-1b-instruct-q4","inference_backend":"candle","completions":[{"index":0,"text":"SUBSTITUTED-L12-RESULT","tokens":4}]}`)
	put, err := http.NewRequest(http.MethodPut, outputURL, bytes.NewReader(garbage))
	must(t, err)
	put.Header.Set("Content-Type", "application/json")
	putResp, err := http.DefaultClient.Do(put)
	mustf(t, err, "put result: %v")
	_, _ = io.Copy(io.Discard, putResp.Body)
	putResp.Body.Close()
	if putResp.StatusCode/100 != 2 {
		t.Fatalf("put result HTTP %d", putResp.StatusCode)
	}
	sum := sha256.Sum256(garbage)
	start, err := http.NewRequest(http.MethodPost, base+"/v1/worker/task/"+taskID+"/start", nil)
	must(t, err)
	start.Header.Set("X-Worker-Token", token)
	start.Header.Set("X-Task-Attempt", "0")
	startResp, err := http.DefaultClient.Do(start)
	mustf(t, err, "start: %v")
	startResp.Body.Close()
	commit := postJSONWithHeaders(t, base+"/v1/worker/task/"+taskID+"/commit", "", map[string]string{
		"X-Worker-Token": token,
	}, map[string]any{
		"attempt":           0,
		"result_key":        resultKey,
		"duration_ms":       10,
		"tokens_used":       4,
		"result_sha256":     hex.EncodeToString(sum[:]),
		"inference_backend": "candle",
	})
	if commit.status != http.StatusNoContent && commit.status != http.StatusAccepted &&
		commit.status != http.StatusOK {
		t.Fatalf("commit substituted result: HTTP %d: %s", commit.status, commit.body)
	}
	_ = jobID
}

func l12WriteReceipt(t *testing.T, name string, doc map[string]any) {
	t.Helper()
	root, err := filepath.Abs("..")
	mustf(t, err, "repo root: %v")
	path := filepath.Join(root, "evidence", "canary", "l12-p1-canary-rehearsal-"+name+".json")
	stamped := map[string]any{
		"schema_version":         1,
		"kind":                   "p1_canary_rehearsal_" + name,
		"gate":                   "P1-CANARY-REHEARSAL",
		"classification":         "ALPHA_CONTROL",
		"does_not_satisfy":       "EXTERNAL_ALPHA_PROVEN",
		"participant_class":      "operator_controlled",
		"synthetic":              true,
		"controlled_by_operator": true,
		"operator_owned":         true,
		"external_alpha_proven":  false,
		"observed_at":            time.Now().UTC().Format(time.RFC3339),
		"rehearsal":              true,
	}
	for k, v := range doc {
		stamped[k] = v
	}
	body, err := json.MarshalIndent(stamped, "", "  ")
	mustf(t, err, "render receipt: %v")
	mustf(t, os.MkdirAll(filepath.Dir(path), 0o755), "mkdir: %v")
	mustf(t, os.WriteFile(path, append(body, '\n'), 0o644), "write %s: %v", path)
	t.Logf("wrote %s", path)
}
