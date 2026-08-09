package main

import (
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/google/uuid"
)

// strangerBatchCorpus is a prompt corpus for the only successful-current
// admission fixture in this package: the explicit in-memory TEST_ONLY
// combined-token batch authority. It must not be reused as production evidence.
const strangerBatchCorpus = `{"id":"0","prompt":"A verifiable compute network settles every task against a receipt."}
{"id":"1","prompt":"A stranger should not have to understand GPUs."}
{"id":"2","prompt":"The cheapest verified outcome is not the cheapest attempt."}
`

func testOnlyBatchPublicRequest(input string, maxUSD float64) map[string]any {
	return map[string]any{
		"job_type": map[string]any{"type": "batch_infer", "max_tokens": 16},
		"model":    map[string]any{"kind": "gguf", "ref": "llama-3.2-1b-instruct-q4"},
		"tier":     "batch",
		"input":    input,
		"max_usd":  maxUSD,
		// Avoid claiming a measured batch honeypot answer in this mechanics-only
		// fixture. Byte-exact redundant execution is a truthful verification shape.
		"verification": map[string]any{"redundancy_frac": 1.0},
	}
}

// TestBuyerStrangerPublicAPISurface is the mechanics-only half of the buyer
// stranger loop: everything a stranger can do against the public API without a
// running agent or engine. Successful admission uses the explicit in-memory
// TEST_ONLY combined-token batch authority; the checked-in production evidence
// currently exposes no scope-compatible performance lane.
//
// Steps covered here:
//
//	signup + funded sandbox credit
//	  -> create an additional API key
//	  -> quote with a deterministic ceiling
//	  -> submit under that ceiling
//	  -> watch status / events / failures surfaces
//	  -> cancel while queued and observe reservation release
//	  -> invoice + receipt readable after cancel
//
// Not covered (needs hardware / deployed agent): verified result delivery and
// end-to-end wall-clock to first verified job. That is
// TestFirstCompleteLoopThroughThePublicAPI.
func TestBuyerStrangerPublicAPISurface(t *testing.T) {
	installBoundCataloguePublicationAuthorityForTest(t)
	installTestOnlyCombinedTokenAuthority(t)
	strangerDeploymentInputs(t)
	installSettlementCurrencyForTest(t, "usd")

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

	verifier := NewVerifier(store).WithStorage(artifacts.storage)
	srv := httptest.NewServer(NewServer(store, artifacts.storage, verifier, nil).Routes())
	t.Cleanup(srv.Close)

	// 1. sign up
	email := "stranger-surface-" + uuid.NewString() + "@example.test"
	signup := postJSON(t, srv.URL+"/v1/signup", "", map[string]any{
		"email": email, "password": "a-stranger-password-1234",
	})
	if signup.status != http.StatusCreated && signup.status != http.StatusOK {
		t.Fatalf("signup: HTTP %d: %s", signup.status, signup.body)
	}
	apiKey, _ := signup.json["sandbox_key"].(string)
	if apiKey == "" {
		t.Fatalf("signup issued no sandbox key: %s", signup.body)
	}
	credit, _ := signup.json["free_credit_usd"].(float64)
	if credit < 5.0 {
		t.Fatalf("sandbox grant was $%.2f; strangerDeploymentInputs sets $5.00", credit)
	}

	// 2. identity + remaining credit
	me := getJSON(t, srv.URL+"/v1/me", apiKey)
	if me.status != http.StatusOK {
		t.Fatalf("GET /v1/me: HTTP %d: %s", me.status, me.body)
	}
	remaining, _ := me.json["free_credit_remaining_usd"].(float64)
	if remaining < 5.0 {
		t.Fatalf("free credit remaining $%.4f right after signup", remaining)
	}

	// 3. create an API key (beyond the auto-minted sandbox key)
	keyCreate := postJSON(t, srv.URL+"/v1/keys", apiKey, map[string]any{
		"name": "stranger-cli", "test": true,
	})
	if keyCreate.status != http.StatusCreated && keyCreate.status != http.StatusOK {
		t.Fatalf("POST /v1/keys: HTTP %d: %s", keyCreate.status, keyCreate.body)
	}
	extraKey, _ := keyCreate.json["key"].(string)
	if extraKey == "" {
		t.Fatalf("create key did not reveal raw key once: %s", keyCreate.body)
	}
	// The new key must authenticate.
	me2 := getJSON(t, srv.URL+"/v1/me", extraKey)
	if me2.status != http.StatusOK {
		t.Fatalf("new key cannot call /v1/me: HTTP %d: %s", me2.status, me2.body)
	}

	// 4. deterministic quote with a ceiling the buyer can accept
	const ceiling = 1.00
	quoteBody := testOnlyBatchPublicRequest(strangerBatchCorpus, ceiling)
	quoteStart := time.Now()
	quote := postJSON(t, srv.URL+"/v1/quote", apiKey, quoteBody)
	quoteLatency := time.Since(quoteStart)
	if quote.status != http.StatusOK {
		t.Fatalf("POST /v1/quote: HTTP %d: %s", quote.status, quote.body)
	}
	quoteID, _ := quote.json["quote_id"].(string)
	if quoteID == "" {
		t.Fatalf("quote missing quote_id: %s", quote.body)
	}
	cost, _ := quote.json["cost"].(map[string]any)
	if cost == nil {
		t.Fatalf("quote missing cost object: %s", quote.body)
	}
	maxUSD, _ := cost["max_usd"].(float64)
	expectedUSD, _ := cost["expected_usd"].(float64)
	if maxUSD <= 0 || expectedUSD <= 0 {
		t.Fatalf("quote cost not positive: max=%v expected=%v body=%s", maxUSD, expectedUSD, quote.body)
	}
	if maxUSD > ceiling {
		// Quote may exceed buyer ceiling; admission at submit must still refuse.
		// For this small corpus under the first-complete schedule it should fit.
		t.Logf("note: quoted max $%.6f exceeds ceiling $%.2f (submit will enforce)", maxUSD, ceiling)
	}
	t.Logf("quote latency=%s expected=$%.9f max=$%.9f id=%s",
		quoteLatency, expectedUSD, maxUSD, quoteID)

	// 5. submit under the ceiling (bind firm quote when present)
	submitBody := testOnlyBatchPublicRequest(strangerBatchCorpus, ceiling)
	submitBody["quote_id"] = quoteID
	submitBody["firm_quote"] = true
	submit := postJSONWithHeaders(t, srv.URL+"/v1/jobs", apiKey, map[string]string{
		"Idempotency-Key": uuid.NewString(),
	}, submitBody)
	switch submit.status {
	case http.StatusOK, http.StatusCreated, http.StatusAccepted:
	default:
		t.Fatalf("submit: HTTP %d: %s", submit.status, submit.body)
	}
	jobID, _ := submit.json["job_id"].(string)
	if jobID == "" {
		jobID, _ = submit.json["id"].(string)
	}
	if jobID == "" {
		t.Fatalf("submit returned no job id: %s", submit.body)
	}
	estimate, _ := submit.json["estimated_usd"].(float64)
	if estimate <= 0 {
		t.Fatalf("submit estimate not positive: %v", estimate)
	}
	if estimate > ceiling {
		t.Fatalf("admitted estimate %.9f above ceiling %.2f", estimate, ceiling)
	}

	// Credit is held by the open estimate.
	meHeld := getJSON(t, srv.URL+"/v1/me", apiKey)
	heldRemaining, _ := meHeld.json["free_credit_remaining_usd"].(float64)
	if heldRemaining >= remaining {
		t.Fatalf("open job did not hold free credit: before=%.6f after=%.6f estimate=%.6f",
			remaining, heldRemaining, estimate)
	}

	// 6. watch progress surfaces
	status := getJSON(t, srv.URL+"/v1/jobs/"+jobID, apiKey)
	if status.status != http.StatusOK {
		t.Fatalf("GET job: HTTP %d: %s", status.status, status.body)
	}
	if st, _ := status.json["status"].(string); st != "queued" && st != "running" {
		// No agent: must stay queued. running would mean something claimed it.
		t.Logf("job status after submit: %v", status.json["status"])
	}
	events := getJSON(t, srv.URL+"/v1/jobs/"+jobID+"/events", apiKey)
	if events.status != http.StatusOK {
		t.Fatalf("GET events: HTTP %d: %s", events.status, events.body)
	}
	failures := getJSON(t, srv.URL+"/v1/jobs/"+jobID+"/failures", apiKey)
	if failures.status != http.StatusOK {
		t.Fatalf("GET failures: HTTP %d: %s", failures.status, failures.body)
	}

	// 7. cancel while queued — reservation release, no charge
	cancel := deleteJSON(t, srv.URL+"/v1/jobs/"+jobID, apiKey)
	if cancel.status != http.StatusOK {
		t.Fatalf("DELETE job: HTTP %d: %s", cancel.status, cancel.body)
	}
	if st, _ := cancel.json["status"].(string); st != "cancelled" {
		t.Fatalf("cancel status %v, want cancelled: %s", cancel.json["status"], cancel.body)
	}
	refund, _ := cancel.json["refund"].(map[string]any)
	if refund == nil {
		t.Fatalf("cancel response missing refund explanation: %s", cancel.body)
	}
	if kind, _ := refund["kind"].(string); kind != "reservation_release" {
		t.Fatalf("refund.kind=%v, want reservation_release", refund["kind"])
	}
	if charged, ok := refund["charged"].(bool); !ok || charged {
		t.Fatalf("refund.charged must be false for a queued cancel: %v", refund["charged"])
	}
	released, _ := cancel.json["free_credit_remaining_usd"].(float64)
	if released+1e-9 < credit-0.01 {
		// After cancel the full grant (minus nothing charged) should be back.
		t.Fatalf("free credit after cancel $%.6f, grant was $%.2f", released, credit)
	}

	// 8. invoice + receipt remain readable after cancel
	invoice := getJSON(t, srv.URL+"/v1/jobs/"+jobID+"/invoice", apiKey)
	if invoice.status != http.StatusOK {
		t.Fatalf("GET invoice: HTTP %d: %s", invoice.status, invoice.body)
	}
	if invStatus, _ := invoice.json["status"].(string); invStatus != "cancelled" {
		t.Fatalf("invoice status %q, want cancelled", invStatus)
	}
	// Gross platform take must never be labelled as true net on the wire.
	if _, hasTrueNetAlias := invoice.json["true_net_profit_usd"]; hasTrueNetAlias {
		t.Fatal("invoice exposes true_net_profit_usd; that alias is a defect")
	}
	if contrib, ok := invoice.json["contribution"].(map[string]any); ok {
		if gross, ok := contrib["merc_gross_spread"].(map[string]any); ok {
			if basis, _ := gross["basis"].(string); basis != "" &&
				(basis == "profit" || basis == "true_net") {
				t.Fatalf("gross spread basis claims profit: %q", basis)
			}
		}
	}
	receipt := getJSON(t, srv.URL+"/v1/jobs/"+jobID+"/receipt", apiKey)
	if receipt.status != http.StatusOK {
		t.Fatalf("GET receipt: HTTP %d: %s", receipt.status, receipt.body)
	}
	if receipt.json["invoice"] == nil {
		t.Fatalf("receipt missing nested invoice: %s", receipt.body)
	}

	// Gross ledger naming: platform_take may exist; it must coexist with the
	// honest platform_gross_spread name, never alone as "profit".
	if inv, ok := receipt.json["invoice"].(map[string]any); ok {
		if _, hasTake := inv["platform_take_usd"]; hasTake {
			if _, hasGross := inv["platform_gross_spread_usd"]; !hasGross {
				t.Fatal("receipt invoice has platform_take_usd without platform_gross_spread_usd")
			}
		}
	}

	t.Logf("stranger public-API surface closed: job=%s quote_latency=%s estimate=$%.9f released=$%.4f",
		jobID, quoteLatency, estimate, released)
}
