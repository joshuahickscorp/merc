package main

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
)

func dependentProjectFixture(t *testing.T) (string, ProjectWorkloadIR, ProjectMaterialization, *client, func() int) {
	t.Helper()
	root := t.TempDir()
	payload := []byte("{\"text\":\"receipt-bound downstream input\"}\n")
	writeProjectFixture(t, root, "intermediate.jsonl", string(payload))
	serverQuote := validProjectServerQuote(t)
	serverQuote.QuoteID = "q_" + uuid.NewString()
	serverQuote.Tier = "batch"
	serverQuote.ExpiresAt = time.Now().Add(time.Hour)
	input := sha256.Sum256(payload)
	serverQuote.InputSHA256 = hex.EncodeToString(input[:])
	maximum, err := MoneyNanosFromUSDFloat(mustCurrency(t, serverQuote.Currency), serverQuote.Cost.MaxUSD)
	if err != nil {
		t.Fatal(err)
	}
	projectID, upstreamJobID := uuid.NewString(), uuid.New()
	pricingSHA, err := pricingDecisionDigest(serverQuote.Pricing)
	if err != nil {
		t.Fatal(err)
	}
	materialized := sha256.Sum256(payload)
	ir := projectQuoteIRFixture(serverQuote, maximum.Nanos*2)
	ir.Steps = []ProjectIRStep{
		{ID: "upstream", Kind: "batch_inference", Inputs: []string{"project://source.jsonl"}, Outputs: []string{"project://intermediate.jsonl"},
			RuntimeID: serverQuote.Workload.RuntimeCandidates[0].RuntimeID, ModelID: serverQuote.Model},
		{ID: "downstream", Kind: "embeddings", DependsOn: []string{"upstream"}, Inputs: []string{"project://intermediate.jsonl"}, Outputs: []string{"project://final.jsonl"},
			RuntimeID: serverQuote.Workload.RuntimeCandidates[0].RuntimeID, ModelID: serverQuote.Model},
	}
	materialization := ProjectMaterialization{Version: 2, IRSHA256: ir.IRSHA256, ProjectID: projectID,
		StepID: "upstream", JobID: upstreamJobID.String(), Output: "project://intermediate.jsonl",
		Bytes: int64(len(payload)), SHA256: hex.EncodeToString(materialized[:]), PricingDecisionSHA256: pricingSHA,
		AuthorityQuoteSHA256: strings.Repeat("c", 64), MaterializedAt: time.Now().UTC().Format(time.RFC3339Nano)}
	var submitCalls int
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v1/projects/" + projectID:
			writeJSON(w, http.StatusOK, ProjectOrder{ID: projectID, IRSHA256: ir.IRSHA256, Currency: ir.Economics.Currency,
				BuyerCeilingNanos: ir.Economics.MaximumBuyerPriceNanos, ReservedNanos: maximum.Nanos,
				RemainingNanos: maximum.Nanos, Status: "OPEN", Steps: []ProjectOrderStep{{StepID: "upstream",
					JobID: upstreamJobID.String(), QuoteID: "q_upstream", PricingDecisionSHA256: pricingSHA, AcceptedCeilingNanos: maximum.Nanos}}})
		case "/v1/jobs/" + upstreamJobID.String() + "/receipt":
			writeJSON(w, http.StatusOK, ClearingReceipt{JobID: upstreamJobID, Status: "complete", AuthorityStatus: "verified",
				Authority: ReceiptAuthority{PricingDecisionSHA256: pricingSHA}})
		case "/v1/quote":
			writeJSON(w, http.StatusOK, serverQuote)
		case "/v1/jobs":
			submitCalls++
			var request cliJobSubmit
			if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
				t.Error(err)
			}
			if request.ProjectID != projectID || request.ProjectStepID != "downstream" || !request.FirmQuote || request.QuoteID != serverQuote.QuoteID {
				t.Errorf("dependent firm submit lost project authority: %+v", request)
			}
			writeJSON(w, http.StatusAccepted, JobSubmitResponse{JobID: uuid.New()})
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(server.Close)
	return root, ir, materialization, &client{base: server.URL, key: "test-project-key", hc: server.Client()}, func() int { return submitCalls }
}

func mustCurrency(t *testing.T, code string) Currency {
	t.Helper()
	currency, err := ParseCurrency(code)
	if err != nil {
		t.Fatal(err)
	}
	return currency
}

func TestDependentProjectStepRequotesAndSubmitsAgainstServerReservation(t *testing.T) {
	root, ir, materialization, c, submitCalls := dependentProjectFixture(t)
	quote, err := quoteDependentProjectStep(c, root, ir, materialization.ProjectID, "downstream", []ProjectMaterialization{materialization})
	if err != nil {
		t.Fatal(err)
	}
	if quote.ProjectID != materialization.ProjectID || quote.Step.StepID != "downstream" ||
		quote.ServerReservedNanos <= 0 || quote.ServerRemainingNanos != quote.Step.MaximumCostNanos {
		t.Fatalf("dependent quote lost current project reserve: %+v", quote)
	}
	result, err := submitDependentProjectStep(c, root, ir, quote, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if result.Status != "ACCEPTED" || result.ExecutionMode != "DEPENDENT_MATERIALIZED_STEP" ||
		len(result.Steps) != 1 || result.Steps[0].StepID != "downstream" || submitCalls() != 1 {
		t.Fatalf("dependent submit did not preserve authority: %+v calls=%d", result, submitCalls())
	}
}

func TestDependentProjectStepRefusesChangedMaterializedArtifactBeforeQuote(t *testing.T) {
	root, ir, materialization, c, submitCalls := dependentProjectFixture(t)
	writeProjectFixture(t, root, "intermediate.jsonl", "{\"text\":\"tampered after receipt\"}\n")
	_, err := quoteDependentProjectStep(c, root, ir, materialization.ProjectID, "downstream", []ProjectMaterialization{materialization})
	if err == nil || (!strings.Contains(err.Error(), "bytes changed") && !strings.Contains(err.Error(), "hash changed")) || submitCalls() != 0 {
		t.Fatalf("tampered materialization reached quote or submit: err=%v calls=%d", err, submitCalls())
	}
}

func TestDependentProjectStepRevalidatesMaterializationAtSubmit(t *testing.T) {
	root, ir, materialization, c, submitCalls := dependentProjectFixture(t)
	quote, err := quoteDependentProjectStep(c, root, ir, materialization.ProjectID, "downstream", []ProjectMaterialization{materialization})
	if err != nil {
		t.Fatal(err)
	}
	writeProjectFixture(t, root, "intermediate.jsonl", "{\"text\":\"tampered after quote\"}\n")
	_, err = submitDependentProjectStep(c, root, ir, quote, time.Now())
	if err == nil || (!strings.Contains(err.Error(), "bytes changed") && !strings.Contains(err.Error(), "hash changed")) || submitCalls() != 0 {
		t.Fatalf("tampered materialization reached dependent submit: err=%v calls=%d", err, submitCalls())
	}
}

func TestInitialProjectRootsQuoteAndSubmitWithoutPricingFutureArtifacts(t *testing.T) {
	root := t.TempDir()
	payload := []byte("{\"text\":\"root input only\"}\n")
	writeProjectFixture(t, root, "source.jsonl", string(payload))
	serverQuote := validProjectServerQuote(t)
	serverQuote.QuoteID = "q_" + uuid.NewString()
	serverQuote.Tier = "batch"
	serverQuote.ExpiresAt = time.Now().Add(time.Hour)
	digest := sha256.Sum256(payload)
	serverQuote.InputSHA256 = hex.EncodeToString(digest[:])
	maximum, err := MoneyNanosFromUSDFloat(mustCurrency(t, serverQuote.Currency), serverQuote.Cost.MaxUSD)
	if err != nil {
		t.Fatal(err)
	}
	ir := projectQuoteIRFixture(serverQuote, maximum.Nanos*2)
	ir.Steps = []ProjectIRStep{
		{ID: "root", Kind: "batch_inference", Inputs: []string{"project://source.jsonl"}, Outputs: []string{"project://vectors"},
			RuntimeID: serverQuote.Workload.RuntimeCandidates[0].RuntimeID, ModelID: serverQuote.Model},
		{ID: "downstream", Kind: "embeddings", DependsOn: []string{"root"}, Inputs: []string{"project://vectors"}, Outputs: []string{"project://final.jsonl"},
			RuntimeID: serverQuote.Workload.RuntimeCandidates[0].RuntimeID, ModelID: serverQuote.Model},
	}
	projectID, jobID := uuid.NewString(), uuid.New()
	var quoteCalls, submitCalls int
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v1/quote":
			quoteCalls++
			writeJSON(w, http.StatusOK, serverQuote)
		case "/v1/projects":
			var order projectOrderCreateRequest
			if err := json.NewDecoder(r.Body).Decode(&order); err != nil {
				t.Error(err)
			}
			if order.IRSHA256 != ir.IRSHA256 || order.BuyerCeilingNanos != ir.Economics.MaximumBuyerPriceNanos {
				t.Errorf("initial roots lost full-project buyer authority: %+v", order)
			}
			project := ProjectOrder{ID: projectID, IRSHA256: order.IRSHA256, Currency: order.Currency,
				BuyerCeilingNanos: order.BuyerCeilingNanos, RemainingNanos: order.BuyerCeilingNanos, Status: "OPEN"}
			if submitCalls > 0 {
				project.ReservedNanos, project.RemainingNanos = maximum.Nanos, order.BuyerCeilingNanos-maximum.Nanos
			}
			writeJSON(w, http.StatusCreated, project)
		case "/v1/jobs":
			submitCalls++
			var request cliJobSubmit
			if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
				t.Error(err)
			}
			if request.ProjectID != projectID || request.ProjectStepID != "root" || !request.FirmQuote || request.QuoteID != serverQuote.QuoteID {
				t.Errorf("root submission lost project authority: %+v", request)
			}
			if submitCalls > 1 {
				w.Header().Set("Idempotent-Replayed", "true")
			}
			writeJSON(w, http.StatusAccepted, JobSubmitResponse{JobID: jobID})
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()
	c := &client{base: server.URL, key: "test-project-key", hc: server.Client()}
	quote, err := quoteInitialProjectRoots(c, root, ir)
	if err != nil {
		t.Fatal(err)
	}
	if quoteCalls != 1 || len(quote.Steps) != 1 || quote.Steps[0].StepID != "root" {
		t.Fatalf("initial quote priced a future materialization: calls=%d quote=%+v", quoteCalls, quote)
	}
	result, err := submitInitialProjectRoots(c, root, ir, quote, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if result.Status != "ACCEPTED" || result.ExecutionMode != "INITIAL_MATERIALIZED_ROOTS" || result.ProjectID != projectID ||
		len(result.Steps) != 1 || result.Steps[0].StepID != "root" || submitCalls != 1 {
		t.Fatalf("initial root submission failed: result=%+v submits=%d", result, submitCalls)
	}
	replay, err := submitInitialProjectRoots(c, root, ir, quote, time.Now())
	if err != nil || replay.Status != "ACCEPTED" || len(replay.Steps) != 1 || !replay.Steps[0].IdempotentReplay || submitCalls != 2 {
		t.Fatalf("initial root replay did not reuse its server reservation: result=%+v err=%v submits=%d", replay, err, submitCalls)
	}
}
