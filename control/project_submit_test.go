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
