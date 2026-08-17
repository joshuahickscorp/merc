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
	"os/exec"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
)

// TestL12RoutableInferLoopThroughThePublicAPI closes buyer → claim → execute →
// verify → settle on the one cell that currently binds
// (candle-metal-llama1-infer / r6), using production authority. It does not
// install TEST_ONLY publication.
//
// A live merc-agent `run` process cannot enrol: projectWorkerRuntimeCapabilities
// requires the sealed r6 engine_build_hash (7cc01c442c7f6dbe) and the binary
// that produced it is no longer on disk. Workers therefore register with that
// sealed identity (the only credential the advertised cell accepts) and execute
// via merc-agent emit-infer-artifact — the production BatchInferRunner — so
// commit bytes are real Metal output, not a fixture.
//
// Plane: local isolated database + httptest control + this host's Metal agent.
// Does not satisfy EXTERNAL_ALPHA_PROVEN.
func TestL12RoutableInferLoopThroughThePublicAPI(t *testing.T) {
	if !advertisedRuntimeCell("candle-metal-llama1-infer") {
		t.Fatal("candle-metal-llama1-infer is not advertised; r6 authority did not bind")
	}
	l12EnsurePriceBoard(t)
	agentBin := l12AgentBinary(t)
	strangerDeploymentInputs(t)
	installSettlementCurrencyForTest(t, "usd")

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

	driveCtx, stopDrive := context.WithCancel(context.Background())
	t.Cleanup(stopDrive)
	var emitMu sync.Mutex
	var acceptWorkers []uuid.UUID
	for i := 0; i < 2; i++ {
		token, workerID := l12RegisterAdvertisedInferWorker(t, ctx, store, pool, srv.URL, i)
		acceptWorkers = append(acceptWorkers, workerID)
		go l12DriveAdvertisedInferWorker(driveCtx, t, srv.URL, token, agentBin, &emitMu)
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
	body := l12InferJobBody(ceiling)
	quote := postJSON(t, srv.URL+"/v1/quote", apiKey, body)
	if quote.status != http.StatusOK {
		t.Fatalf("quote infer: HTTP %d: %s", quote.status, quote.body)
	}
	idem := "l12-accept-" + uuid.NewString()
	submit := postJSONWithHeaders(t, srv.URL+"/v1/jobs", apiKey, map[string]string{
		"Idempotency-Key": idem,
	}, body)
	if submit.status != http.StatusOK && submit.status != http.StatusCreated &&
		submit.status != http.StatusAccepted {
		t.Fatalf("submit infer: HTTP %d: %s", submit.status, submit.body)
	}
	replay := postJSONWithHeaders(t, srv.URL+"/v1/jobs", apiKey, map[string]string{
		"Idempotency-Key": idem,
	}, body)
	if replay.status != submit.status {
		t.Fatalf("idempotent replay HTTP %d, first submit HTTP %d: %s",
			replay.status, submit.status, replay.body)
	}
	jobIDText, _ := submit.json["job_id"].(string)
	if jobIDText == "" {
		jobIDText, _ = submit.json["id"].(string)
	}
	replayID, _ := replay.json["job_id"].(string)
	if replayID == "" {
		replayID, _ = replay.json["id"].(string)
	}
	if jobIDText == "" || replayID != jobIDText {
		t.Fatalf("idempotent replay minted a second job: first=%q replay=%q bodies %s / %s",
			jobIDText, replayID, submit.body, replay.body)
	}
	jobID, err := uuid.Parse(jobIDText)
	if err != nil {
		t.Fatalf("submit returned no job id: %s", submit.body)
	}
	l12WaitForJobSettled(t, loopCtx, pool, jobID, "l12-accept")

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
		"replay_http": replay.status, "idempotent_one_job": replayID == jobIDText,
		"cell": cell, "verification_outcome": outcome,
		"advertisement": "documentActivation advertised candle-metal-llama1-infer; quote used normalizeAdvertisedRuntimeModelRef",
	})
	l12WriteReceipt(t, "supplier-execution", map[string]any{
		"status": "PASS", "plane": "local", "job_id": jobID.String(),
		"worker_ids": []string{acceptWorkers[0].String(), acceptWorkers[1].String()},
		"cell":       cell,
		"executor":   "merc-agent emit-infer-artifact (production BatchInferRunner)",
		"enrolment":  "sealed r6 identity via POST /v1/worker/register; live merc-agent run cannot match 7cc01c442c7f6dbe",
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

	stopDrive()

	// --- reject: substituted clone on a live commit --------------------------
	// Honeypots are refused by uniform-task economics. A substituted
	// redundancy clone is the current reject proof: one honest emit and one
	// garbage commit must produce redundancy_mismatch.
	rejectCtx, stopReject := context.WithCancel(context.Background())
	t.Cleanup(stopReject)
	honestToken, _ := l12RegisterAdvertisedInferWorker(t, ctx, store, pool, srv.URL, 8)
	go l12DriveAdvertisedInferWorker(rejectCtx, t, srv.URL, honestToken, agentBin, &emitMu)
	rejectToken, rejectWorker := l12RegisterAdvertisedInferWorker(t, ctx, store, pool, srv.URL, 9)
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
	deadline := time.Now().Add(3 * time.Minute)
	var failEvents int
	for time.Now().Before(deadline) {
		if err := pool.QueryRow(loopCtx, `
			SELECT count(*) FROM verification_events
			 WHERE job_id=$1 AND kind IN (
			   'honeypot_fail','honeypot_class_mismatch','redundancy_mismatch')`,
			rejectJob).Scan(&failEvents); err != nil {
			t.Fatalf("read reject events: %v", err)
		}
		if failEvents > 0 {
			break
		}
		time.Sleep(200 * time.Millisecond)
	}
	if failEvents == 0 {
		t.Fatal("substituted redundancy clone was not REJECTED")
	}
	stopReject()
	l12WriteReceipt(t, "verification-reject", map[string]any{
		"status": "PASS", "plane": "local", "job_id": rejectJob.String(),
		"worker_id": rejectWorker.String(), "reject_events": failEvents,
		"reject_kind": "redundancy_mismatch_or_honeypot_fail",
	})
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

func l12RegisterAdvertisedInferWorker(
	t *testing.T, ctx context.Context, store *Store, pool *pgxpool.Pool, base string, n int,
) (string, uuid.UUID) {
	t.Helper()
	supplierID, workerID := uuid.New(), uuid.New()
	if _, err := pool.Exec(ctx, `
		INSERT INTO suppliers (id,email,status,reputation,completed_tasks)
		VALUES ($1,$2,'active',0.95,100)`,
		supplierID, fmt.Sprintf("l12-infer-%d-%s@proof.test", n, uuid.NewString())); err != nil {
		t.Fatalf("seed infer supplier: %v", err)
	}
	token, err := store.IssueDeviceBoundWorkerToken(ctx, workerID, supplierID,
		fmt.Sprintf("l12-operator-metal-%d", n+1))
	mustf(t, err, "issue infer token: %v")
	cap, _ := l12SealedInferCapability(t)
	reg := postJSONWithHeaders(t, base+"/v1/worker/register", "", map[string]string{
		"X-Worker-Token": token,
	}, map[string]any{
		"hw_class":              cap.HWClass,
		"engine":                cap.Engine,
		"build_hash":            cap.BuildHash,
		"build_identity_policy": cap.BuildIdentityPolicy,
		"hardware_identity":     cap.HardwareIdentity,
		"memory_gb":             cap.MemoryGB,
		"memory_bw_gbps":        cap.MemoryBwGbps,
		"supported_jobs":        cap.SupportedJobs,
		"supported_models":      cap.SupportedModels,
		"min_payout_usd_hr":     0.0,
		"agent_version":         cap.AgentVersion,
		"os_version":            cap.OSVersion,
		"sandboxed":             true,
		"unsandboxed_opt_in":    false,
		"agent_session_id":      uuid.NewString(),
		"benchmarks": []map[string]any{{
			"model_id":      cap.Benchmarks[0].ModelID,
			"job_type":      cap.Benchmarks[0].JobType,
			"tps":           cap.Benchmarks[0].TPS,
			"eps":           0,
			"p99_ms":        20,
			"thermal_ok":    true,
			"unit":          cap.Benchmarks[0].Unit,
			"unit_scope":    cap.Benchmarks[0].UnitScope,
			"measured_unix": cap.Benchmarks[0].MeasuredUnix,
		}},
	})
	if reg.status != http.StatusOK {
		t.Fatalf("advertised infer worker register: HTTP %d: %s", reg.status, reg.body)
	}
	return token, workerID
}

func l12DriveAdvertisedInferWorker(
	ctx context.Context, t *testing.T, base, token, agentBin string, emitMu *sync.Mutex,
) {
	t.Helper()
	lastBeat := time.Time{}
	for {
		select {
		case <-ctx.Done():
			return
		default:
		}
		if time.Since(lastBeat) > 2*time.Second {
			l12Heartbeat(t, base, token)
			lastBeat = time.Now()
		}
		if !l12PollAndExecute(ctx, t, base, token, agentBin, emitMu) {
			select {
			case <-ctx.Done():
				return
			case <-time.After(200 * time.Millisecond):
			}
		}
	}
}

func l12WaitForJobSettled(
	t *testing.T, ctx context.Context, pool *pgxpool.Pool, jobID uuid.UUID, name string,
) {
	t.Helper()
	deadline := time.Now().Add(8 * time.Minute)
	var status string
	var actual float64
	for time.Now().Before(deadline) {
		if err := ctx.Err(); err != nil {
			t.Fatalf("%s: wait cancelled: %v (status=%s actual=%.9f)", name, err, status, actual)
		}
		if err := pool.QueryRow(ctx, `
			SELECT status, COALESCE(actual_usd,0) FROM jobs WHERE id=$1`, jobID).
			Scan(&status, &actual); err != nil {
			t.Fatalf("%s: read job settlement state: %v", name, err)
		}
		if status == "complete" && actual > 0 {
			return
		}
		if status == "failed" || status == "cancelled" {
			t.Fatalf("%s: job reached %s instead of settling", name, status)
		}
		time.Sleep(500 * time.Millisecond)
	}
	t.Fatalf("%s: job did not settle inside 8m (status=%s actual=%.9f)", name, status, actual)
}

func l12Heartbeat(t *testing.T, base, token string) {
	t.Helper()
	hb := postJSONWithHeaders(t, base+"/v1/worker/heartbeat", "", map[string]string{
		"X-Worker-Token": token,
	}, map[string]any{
		"available_memory_gb": 64,
		"effective_memory_gb": 64,
		"loaded_models":       []string{"llama-3.2-1b-instruct-q4"},
	})
	if hb.status != http.StatusOK {
		t.Logf("heartbeat HTTP %d: %s", hb.status, hb.body)
	}
}

func l12PollAndExecute(
	ctx context.Context, t *testing.T, base, token, agentBin string, emitMu *sync.Mutex,
) bool {
	t.Helper()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, base+"/v1/worker/poll?wait_ms=500", nil)
	if err != nil {
		return false
	}
	req.Header.Set("X-Worker-Token", token)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return false
	}
	raw, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode == http.StatusNoContent || len(raw) == 0 {
		return false
	}
	if resp.StatusCode != http.StatusOK {
		t.Logf("poll HTTP %d: %s", resp.StatusCode, raw)
		return false
	}
	var dispatch map[string]any
	if json.Unmarshal(raw, &dispatch) != nil {
		return false
	}
	taskID, _ := dispatch["task_id"].(string)
	resultKey, _ := dispatch["result_key"].(string)
	outputURL, _ := dispatch["output_url"].(string)
	inputURL, _ := dispatch["input_url"].(string)
	if taskID == "" || outputURL == "" || inputURL == "" {
		t.Logf("dispatch missing fields: %v", dispatch)
		return false
	}
	maxTokens := uint32(16)
	if manifest, ok := dispatch["manifest"].(map[string]any); ok {
		if jt, ok := manifest["job_type"].(map[string]any); ok {
			if v, ok := jt["max_tokens"].(float64); ok && v > 0 {
				maxTokens = uint32(v)
			}
		}
	}
	inReq, err := http.NewRequestWithContext(ctx, http.MethodGet, inputURL, nil)
	if err != nil {
		return false
	}
	inResp, err := http.DefaultClient.Do(inReq)
	if err != nil {
		t.Logf("download input: %v", err)
		return false
	}
	input, _ := io.ReadAll(inResp.Body)
	inResp.Body.Close()

	dir := t.TempDir()
	inPath := filepath.Join(dir, "input.jsonl")
	outPath := filepath.Join(dir, "result.json")
	if err := os.WriteFile(inPath, input, 0o644); err != nil {
		t.Logf("write input: %v", err)
		return false
	}
	emitMu.Lock()
	cmd := exec.CommandContext(ctx, agentBin, "emit-infer-artifact",
		"--model", "llama-3.2-1b-instruct-q4",
		"--max-tokens", fmt.Sprintf("%d", maxTokens),
		"--input", inPath,
		"--out", outPath)
	cmd.Env = append(os.Environ(),
		"MERC_MODEL_CACHE="+filepath.Join(os.Getenv("HOME"), ".cache", "huggingface", "hub"))
	emitted, err := cmd.CombinedOutput()
	emitMu.Unlock()
	if err != nil {
		t.Logf("emit-infer-artifact: %v\n%s", err, emitted)
		return false
	}
	result, err := os.ReadFile(outPath)
	if err != nil {
		t.Logf("read result: %v", err)
		return false
	}
	put, err := http.NewRequestWithContext(ctx, http.MethodPut, outputURL, bytes.NewReader(result))
	if err != nil {
		return false
	}
	put.Header.Set("Content-Type", "application/json")
	putResp, err := http.DefaultClient.Do(put)
	if err != nil {
		t.Logf("put result: %v", err)
		return false
	}
	_, _ = io.Copy(io.Discard, putResp.Body)
	putResp.Body.Close()
	if putResp.StatusCode/100 != 2 {
		t.Logf("put result HTTP %d", putResp.StatusCode)
		return false
	}
	sum := sha256.Sum256(result)
	start, err := http.NewRequestWithContext(ctx, http.MethodPost, base+"/v1/worker/task/"+taskID+"/start", nil)
	if err != nil {
		return false
	}
	start.Header.Set("X-Worker-Token", token)
	start.Header.Set("X-Task-Attempt", "0")
	startResp, err := http.DefaultClient.Do(start)
	if err == nil {
		startResp.Body.Close()
	}
	commit := postJSONWithHeaders(t, base+"/v1/worker/task/"+taskID+"/commit", "", map[string]string{
		"X-Worker-Token": token,
	}, map[string]any{
		"attempt":           0,
		"result_key":        resultKey,
		"duration_ms":       20,
		"tokens_used":       maxTokens,
		"result_sha256":     hex.EncodeToString(sum[:]),
		"inference_backend": "candle",
	})
	if commit.status != http.StatusNoContent && commit.status != http.StatusAccepted &&
		commit.status != http.StatusOK {
		t.Logf("commit HTTP %d: %s", commit.status, commit.body)
		return false
	}
	return true
}

func l12CommitSubstitutedHoneypot(t *testing.T, base, token string, jobID uuid.UUID) {
	t.Helper()
	deadline := time.Now().Add(90 * time.Second)
	var dispatch map[string]any
	for time.Now().Before(deadline) {
		l12Heartbeat(t, base, token)
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
