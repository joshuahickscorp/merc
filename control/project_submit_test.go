package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
)

func executableProjectQuoteFixture(t *testing.T, root string, now time.Time, handler func(cliJobSubmit, string)) (ProjectWorkloadIR, ProjectQuote, *httptest.Server) {
	t.Helper()
	input := []byte("{\"text\":\"hello\"}\n")
	writeProjectFixture(t, root, "input.jsonl", string(input))
	serverQuote := validProjectServerQuote(t)
	serverQuote.QuoteID = "q_" + uuid.NewString()
	serverQuote.Tier = "batch"
	serverQuote.ExpiresAt = now.Add(time.Hour)
	digest := sha256.Sum256(input)
	serverQuote.InputSHA256 = hex.EncodeToString(digest[:])
	currency, err := ParseCurrency(serverQuote.Currency)
	if err != nil {
		t.Fatal(err)
	}
	maximum, err := MoneyNanosFromUSDFloat(currency, serverQuote.Cost.MaxUSD)
	if err != nil {
		t.Fatal(err)
	}
	ir := projectQuoteIRFixture(serverQuote, maximum.Nanos+1)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v1/quote":
			writeJSON(w, http.StatusOK, serverQuote)
		case "/v1/jobs":
			var request cliJobSubmit
			if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
				t.Error(err)
			}
			if handler != nil {
				handler(request, r.Header.Get("Idempotency-Key"))
			}
			w.Header().Set("Idempotent-Replayed", "true")
			writeJSON(w, http.StatusAccepted, JobSubmitResponse{JobID: uuid.New()})
		default:
			http.NotFound(w, r)
		}
	}))
	c := &client{base: server.URL, key: "test-project-key", hc: server.Client()}
	artifact, err := quoteCompiledProject(c, root, ir)
	if err != nil {
		server.Close()
		t.Fatal(err)
	}
	return ir, artifact, server
}

func TestSubmitCompiledProjectPreservesReviewedAuthority(t *testing.T) {
	now := time.Date(2026, 8, 1, 13, 0, 0, 0, time.UTC)
	root := t.TempDir()
	var calls int
	ir, artifact, server := executableProjectQuoteFixture(t, root, now, func(request cliJobSubmit, key string) {
		calls++
		if !request.FirmQuote || request.QuoteID == "" {
			t.Error("project submit did not bind a firm quote")
		}
		if !strings.HasPrefix(key, "project:") || len(key) > 128 {
			t.Errorf("invalid deterministic idempotency key %q", key)
		}
	})
	defer server.Close()
	c := &client{base: server.URL, key: "test-project-key", hc: server.Client()}
	result, err := submitCompiledProject(c, root, ir, artifact, now)
	if err != nil {
		t.Fatal(err)
	}
	if calls != 1 || result.Status != "ACCEPTED" || result.ExecutionMode != "INDEPENDENT_FINITE_STEPS" ||
		len(result.Steps) != 1 || !result.Steps[0].IdempotentReplay ||
		result.Steps[0].QuoteID != artifact.Steps[0].QuoteID ||
		result.Steps[0].PricingDecisionSHA256 != artifact.Steps[0].PricingDecisionSHA256 {
		t.Fatalf("project submission lost reviewed authority: %+v calls=%d", result, calls)
	}
}

func TestSubmitCompiledProjectRefusesAuthorityTamperingBeforeMutation(t *testing.T) {
	now := time.Date(2026, 8, 1, 13, 0, 0, 0, time.UTC)
	root := t.TempDir()
	var calls int
	ir, artifact, server := executableProjectQuoteFixture(t, root, now, func(cliJobSubmit, string) { calls++ })
	defer server.Close()
	artifact.Steps[0].Authority.Pricing.BuyerPrice++
	_, err := submitCompiledProject(&client{base: server.URL, hc: server.Client()}, root, ir, artifact, now)
	if err == nil || !strings.Contains(err.Error(), "authority quote digest mismatch") || calls != 0 {
		t.Fatalf("tampered quote reached mutation: err=%v calls=%d", err, calls)
	}
}

func TestSubmitCompiledProjectRefusesChangedInputAndDependencies(t *testing.T) {
	now := time.Date(2026, 8, 1, 13, 0, 0, 0, time.UTC)
	root := t.TempDir()
	ir, artifact, server := executableProjectQuoteFixture(t, root, now, nil)
	defer server.Close()
	writeProjectFixture(t, root, "input.jsonl", "{\"text\":\"changed\"}\n")
	_, err := validateProjectQuoteForSubmit(root, ir, artifact, now)
	if err == nil || !strings.Contains(err.Error(), "input changed") {
		t.Fatalf("changed input passed: %v", err)
	}
	writeProjectFixture(t, root, "input.jsonl", "{\"text\":\"hello\"}\n")
	ir.Steps[0].DependsOn = []string{"upstream"}
	_, err = validateProjectQuoteForSubmit(root, ir, artifact, now)
	if err == nil || !strings.Contains(err.Error(), "only independent finite steps") {
		t.Fatalf("dependency graph was mislabeled executable: %v", err)
	}
}

func TestSubmitCompiledProjectRefusesExpiredQuote(t *testing.T) {
	now := time.Date(2026, 8, 1, 13, 0, 0, 0, time.UTC)
	root := t.TempDir()
	ir, artifact, server := executableProjectQuoteFixture(t, root, now, nil)
	defer server.Close()
	_, err := validateProjectQuoteForSubmit(root, ir, artifact, now.Add(2*time.Hour))
	if err == nil || !strings.Contains(err.Error(), "quote expired") {
		t.Fatalf("expired quote passed: %v", err)
	}
}

// TestProjectCompilerCADAdmissionThroughPublicAPI proves the production boundary
// the unit-level compiler tests deliberately cannot: a buyer-approved, probed
// project is quoted and firm-submitted through the authenticated public API.
//
// This is an admission proof, not an execution claim. There is no worker in this
// fixture, so it must not be used as evidence for topology, supplier earnings,
// outcome verification, or true net contribution. Those need the real agent
// chain. What it does bind is the exact Project IR -> quote -> firm job path to
// the same persisted PricingDecision the server will later settle against.
func TestProjectCompilerCADAdmissionThroughPublicAPI(t *testing.T) {
	strangerDeploymentInputs(t)
	installSettlementCurrencyForTest(t, "cad")

	artifacts := newArtifactHarness(t)
	ctx, store, pool := openIsolatedTestStore(t)
	schedule, err := BuildCataloguePriceSchedule()
	if err != nil {
		t.Fatalf("build catalogue price schedule: %v", err)
	}
	if _, err := store.ApplyRepricing(ctx, schedule); err != nil {
		t.Fatalf("publish catalogue price schedule: %v", err)
	}
	if err := seedDemo(ctx, pool, artifacts.storage); err != nil {
		t.Fatalf("seed verification floor: %v", err)
	}
	server := httptest.NewServer(NewServer(store, artifacts.storage, NewVerifier(store).WithStorage(artifacts.storage), nil).Routes())
	t.Cleanup(server.Close)

	signup := postJSON(t, server.URL+"/v1/signup", "", map[string]any{
		"email": "project-" + uuid.NewString() + "@example.test", "password": "a-stranger-password-1234",
	})
	if signup.status != http.StatusOK && signup.status != http.StatusCreated {
		t.Fatalf("signup: HTTP %d: %s", signup.status, signup.body)
	}
	buyerKey, _ := signup.json["sandbox_key"].(string)
	if buyerKey == "" {
		t.Fatalf("signup issued no sandbox API key: %s", signup.body)
	}

	contracts, err := advertisedProjectRuntimeContracts()
	if err != nil {
		t.Fatal(err)
	}
	var embed ProjectRuntimeContract
	for _, candidate := range contracts {
		if candidate.WorkloadKind == "embeddings" {
			embed = candidate
			break
		}
	}
	if embed.RuntimeID == "" || embed.ModelID == "" {
		t.Fatal("no advertised embeddings runtime contract")
	}

	root := t.TempDir()
	input := "{\"text\":\"the buyer-approved compiler must not derive price\"}\n" +
		"{\"text\":\"the server must freeze the same CAD authority it quoted\"}\n"
	writeProjectFixture(t, root, "input.jsonl", input)
	writeProjectFixture(t, root, "pipeline.py", "embedding = client.embeddings.create(...)\n")
	writeDeclarationFixture(t, root, ProjectDeclaration{
		Version: 1,
		Steps: []ProjectIRStep{{
			ID: "embed", Kind: "embeddings", Inputs: []string{"project://input.jsonl"}, Outputs: []string{"project://vectors"},
			RuntimeContract: embed.RuntimeContractSHA256, ModelContract: embed.ModelContractSHA256,
			ResourceEstimate: ProjectIRResourceEstimate{State: "BOUNDED_PROBE_REQUIRED"}, Parallelism: "INDEPENDENT",
			CheckpointPolicy: "NOT_APPLICABLE", Verification: embed.Verification,
		}},
		Privacy: ProjectIRPrivacy{Egress: "DENY", DataLocation: "CA"},
		Quality: ProjectIRQuality{Requirement: "project-public-admission-v1", Verification: "independent"},
		Result:  ProjectIRResult{Contract: "vectors-v1", Retention: "30d", Delivery: "object-store"},
		Economics: ProjectIREconomics{
			Currency: "cad", MaximumBuyerPriceNanos: 20_000_000_000,
			SupplierFloor: "UNRESOLVED_REFUSE", MercContribution: "UNRESOLVED_REFUSE",
		},
	})

	proposal, err := compileProject(projectCompileOptions{Root: root})
	if err != nil {
		t.Fatalf("compile unprobed project: %v", err)
	}
	ir, err := compileProject(projectCompileOptions{
		Root: root, ProbeRequested: true, BuyerApprovedIRSHA256: proposal.IRSHA256,
	})
	if err != nil {
		t.Fatalf("compile buyer-approved probe: %v", err)
	}
	if !ir.Probe.Executed || !ir.Probe.BuyerAuthorized || ir.Probe.ApprovedIRSHA256 != proposal.IRSHA256 {
		t.Fatalf("project probe was not bound to buyer approval: %+v", ir.Probe)
	}

	c := &client{base: server.URL, key: buyerKey, hc: server.Client()}
	artifact, err := quoteCompiledProject(c, root, ir)
	if err != nil {
		t.Fatalf("quote through public API: %v", err)
	}
	if artifact.Currency != "cad" || len(artifact.Steps) != 1 || artifact.Steps[0].PricingDecisionSHA256 == "" {
		t.Fatalf("public quote lost CAD PricingDecision authority: %+v", artifact)
	}
	result, err := submitCompiledProject(c, root, ir, artifact, time.Now().UTC())
	if err != nil {
		t.Fatalf("firm submit through public API: %v", err)
	}
	if result.Status != "ACCEPTED" || len(result.Steps) != 1 || !strings.HasPrefix(result.Steps[0].IdempotencyKey, "project:") {
		t.Fatalf("project public submission was not accepted with its deterministic authority: %+v", result)
	}
	jobID, err := uuid.Parse(result.Steps[0].JobID)
	if err != nil {
		t.Fatalf("submitted job id: %v", err)
	}

	var quoteID *uuid.UUID
	var firmQuote bool
	var currency string
	var pricingJSON []byte
	if err := pool.QueryRow(ctx, `
		SELECT quote_id, firm_quote, currency, pricing_decision
		  FROM jobs WHERE id=$1`, jobID).Scan(&quoteID, &firmQuote, &currency, &pricingJSON); err != nil {
		t.Fatalf("read submitted job authority: %v", err)
	}
	if quoteID == nil || "q_"+quoteID.String() != artifact.Steps[0].QuoteID || !firmQuote || currency != "cad" {
		t.Fatalf("job did not freeze the reviewed CAD firm quote: quote=%v firm=%t currency=%q", quoteID, firmQuote, currency)
	}
	var frozen PricingDecision
	if err := json.Unmarshal(pricingJSON, &frozen); err != nil {
		t.Fatalf("decode frozen PricingDecision: %v", err)
	}
	frozenSHA, err := pricingDecisionDigest(frozen)
	if err != nil || frozenSHA != artifact.Steps[0].PricingDecisionSHA256 {
		t.Fatalf("job pricing decision diverged from reviewed project quote: %s err=%v", frozenSHA, err)
	}
}

// TestProjectCompilerCADExecutionThroughPublicAPI closes the gap left by the
// admission-only proof above. It drives a buyer-approved Project IR through a
// public CAD quote and firm submission, then makes a real enrolled agent claim,
// execute, verify, settle, and expose a receipt for that project step. The
// ledger assertion below is intentionally platform take, not true net
// contribution; project execution must not relabel gross rows as net economics.
//
// This deliberately proves only one independent finite step. A dependent graph
// still needs durable result materialization and a new quote after each upstream
// artifact is frozen; accepting it here would turn a declaration into a false
// execution capability.
func TestProjectCompilerCADExecutionThroughPublicAPI(t *testing.T) {
	agentBinaryPath(t)
	llamaURL := os.Getenv("MERC_LLAMA_EMBED_URL")
	strangerDeploymentInputs(t)
	installSettlementCurrencyForTest(t, "cad")

	artifacts := newArtifactHarness(t)
	ctx, store, pool := openIsolatedTestStore(t)
	schedule, err := BuildCataloguePriceSchedule()
	if err != nil {
		t.Fatalf("build catalogue price schedule: %v", err)
	}
	if _, err := store.ApplyRepricing(ctx, schedule); err != nil {
		t.Fatalf("publish catalogue price schedule: %v", err)
	}
	verifier := NewVerifier(store).WithStorage(artifacts.storage)
	server := httptest.NewServer(NewServer(store, artifacts.storage, verifier, nil).Routes())
	t.Cleanup(server.Close)

	workersCtx, stopWorkers := context.WithCancel(context.Background())
	workersDone := make(chan struct{})
	go func() {
		defer close(workersDone)
		NewWorkers(store, artifacts.storage, stubPayout{}).Run(workersCtx)
	}()
	t.Cleanup(func() {
		stopWorkers()
		<-workersDone
	})
	if err := seedDemo(ctx, pool, artifacts.storage); err != nil {
		t.Fatalf("seed verification floor: %v", err)
	}
	agent := launchAgent(t, ctx, store, pool, server.URL, "candle", "candle_metal", llamaURL)
	waitForEnrolment(t, ctx, pool, agent)

	contracts, err := advertisedProjectRuntimeContracts()
	if err != nil {
		t.Fatal(err)
	}
	var embed ProjectRuntimeContract
	for _, candidate := range contracts {
		if candidate.WorkloadKind == "embeddings" {
			embed = candidate
			break
		}
	}
	if embed.RuntimeID == "" || embed.ModelID == "" {
		t.Fatal("no advertised embeddings runtime contract")
	}
	root := t.TempDir()
	writeProjectFixture(t, root, "input.jsonl", "{\"text\":\"project IR execution must retain the frozen CAD authority\"}\n")
	writeProjectFixture(t, root, "pipeline.py", "embedding = client.embeddings.create(...)\n")
	writeDeclarationFixture(t, root, ProjectDeclaration{
		Version: 1,
		Steps: []ProjectIRStep{{
			ID: "embed", Kind: "embeddings", Inputs: []string{"project://input.jsonl"}, Outputs: []string{"project://vectors"},
			RuntimeContract: embed.RuntimeContractSHA256, ModelContract: embed.ModelContractSHA256,
			ResourceEstimate: ProjectIRResourceEstimate{State: "BOUNDED_PROBE_REQUIRED"}, Parallelism: "INDEPENDENT",
			CheckpointPolicy: "NOT_APPLICABLE", Verification: embed.Verification,
		}},
		Privacy: ProjectIRPrivacy{Egress: "DENY", DataLocation: "CA"},
		Quality: ProjectIRQuality{Requirement: "project-public-execution-v1", Verification: "independent"},
		Result:  ProjectIRResult{Contract: "vectors-v1", Retention: "30d", Delivery: "object-store"},
		Economics: ProjectIREconomics{Currency: "cad", MaximumBuyerPriceNanos: 20_000_000_000,
			SupplierFloor: "UNRESOLVED_REFUSE", MercContribution: "UNRESOLVED_REFUSE"},
	})
	proposal, err := compileProject(projectCompileOptions{Root: root})
	if err != nil {
		t.Fatalf("compile unprobed project: %v", err)
	}
	ir, err := compileProject(projectCompileOptions{Root: root, ProbeRequested: true, BuyerApprovedIRSHA256: proposal.IRSHA256})
	if err != nil {
		t.Fatalf("compile buyer-approved probe: %v", err)
	}

	signup := postJSON(t, server.URL+"/v1/signup", "", map[string]any{
		"email": "project-execution-" + uuid.NewString() + "@example.test", "password": "a-stranger-password-1234",
	})
	if signup.status != http.StatusOK && signup.status != http.StatusCreated {
		t.Fatalf("signup: HTTP %d: %s", signup.status, signup.body)
	}
	buyerKey, _ := signup.json["sandbox_key"].(string)
	if buyerKey == "" {
		t.Fatalf("signup issued no sandbox API key: %s", signup.body)
	}
	c := &client{base: server.URL, key: buyerKey, hc: server.Client()}
	artifact, err := quoteCompiledProject(c, root, ir)
	if err != nil {
		t.Fatalf("quote through public API: %v", err)
	}
	submission, err := submitCompiledProject(c, root, ir, artifact, time.Now().UTC())
	if err != nil || submission.Status != "ACCEPTED" || len(submission.Steps) != 1 {
		t.Fatalf("firm project submit: result=%+v err=%v", submission, err)
	}
	jobID, err := uuid.Parse(submission.Steps[0].JobID)
	if err != nil {
		t.Fatalf("submitted job id: %v", err)
	}
	loopCtx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()
	waitForJobSettled(t, loopCtx, pool, jobID, "project-ir")

	var status, currency, verification string
	var resultCount, supplierCredits int
	var platformTakeMicros int64
	if err := pool.QueryRow(loopCtx, `SELECT status, currency FROM jobs WHERE id=$1`, jobID).Scan(&status, &currency); err != nil {
		t.Fatalf("read project job: %v", err)
	}
	if status != "complete" || currency != "cad" {
		t.Fatalf("project job status/currency = %q/%q, want complete/cad", status, currency)
	}
	if err := pool.QueryRow(loopCtx, `
		SELECT count(*), COALESCE(max(verification_outcome),''),
		       count(*) FILTER (WHERE kind='supplier_credit'),
		       COALESCE((sum(amount_usd) FILTER (WHERE kind='platform_take')*1000000)::bigint,0)
		  FROM tasks t
		  LEFT JOIN ledger_entries l ON l.task_id=t.id
		 WHERE t.job_id=$1`, jobID).Scan(&resultCount, &verification, &supplierCredits, &platformTakeMicros); err != nil {
		t.Fatalf("read project execution evidence: %v", err)
	}
	if resultCount == 0 || verification != "pass" || supplierCredits == 0 || platformTakeMicros <= 0 {
		t.Fatalf("project step lacked execution/verification/gross-platform evidence: tasks=%d verification=%q supplier_credits=%d platform_take=%d", resultCount, verification, supplierCredits, platformTakeMicros)
	}
	keys, err := store.JobResultKeys(loopCtx, jobID)
	if err != nil || len(keys) == 0 {
		t.Fatalf("project execution exposes no retained result artifact: keys=%v err=%v", keys, err)
	}
	if got := c.do("GET", "/v1/jobs/"+jobID.String()+"/receipt", nil); !strings.Contains(string(got), jobID.String()) {
		t.Fatalf("buyer receipt does not name the project job: %s", got)
	}
}
