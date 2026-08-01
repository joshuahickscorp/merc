package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func projectQuoteIRFixture(quote Quote, ceiling int64) ProjectWorkloadIR {
	return ProjectWorkloadIR{
		Version: 1, IRSHA256: strings.Repeat("a", 64),
		Probe:     ProjectIRProbe{Executed: true, BuyerAuthorized: true, ApprovedIRSHA256: strings.Repeat("b", 64)},
		Economics: ProjectIREconomics{Currency: quote.Currency, MaximumBuyerPriceNanos: ceiling},
		Steps: []ProjectIRStep{{
			ID: "embed", Kind: "embeddings", Inputs: []string{"project://input.jsonl"},
			RuntimeID: quote.Workload.RuntimeCandidates[0].RuntimeID, ModelID: quote.Model,
		}},
	}
}

func validProjectServerQuote(t *testing.T) Quote {
	t.Helper()
	workload, compute, _ := computePlanFixture(t)
	schedule := testEconomicSchedule()
	schedule.ControlPlanePerTaskUSD = 0
	schedule.ControlPlanePerBatchUSD = .005
	schedule.MinChargeBatchUSD = 5
	schedule.MinimumContributionUSD = .000001
	schedule.ControlPlaneAllocationPolicy = controlPlaneAllocationChargeBatchV1
	economic := BuildEconomicPlan(EconomicPlanInput{
		BaseComputeUSD: .40, InitialTaskCount: 4, ExtraTaskReserve: 2, SupplierShare: .97,
	}, schedule)
	if err := ValidateComputePlanEconomicSnapshot(compute, workload, economic); err != nil {
		t.Fatal(err)
	}
	authority := catalogueAuthorityFixture(t, workload, economic.Schedule.Currency, economic.Input.SupplierShare)
	placement := placementForPricingFixture(t, workload, authority)
	pricing, err := newDistributedPricingDecision(workload, compute, placement, economic, authority, workload.Binding.Tier, "")
	if err != nil {
		t.Fatal(err)
	}
	return Quote{
		QuoteID: "qte_test", JobType: workload.RuntimeJobType,
		Model: workload.Binding.Model.Ref, Currency: pricing.Currency,
		Workload: workload, ComputePlan: compute, Placement: placement, Economics: economic,
		Pricing:    pricing,
		Cost:       QuoteCost{ExpectedUSD: pricing.BuyerPrice, MaxUSD: pricing.MaximumBuyerPrice},
		Time:       QuoteTime{P50Secs: 3, P90Secs: 5, ConfidenceBandMethod: etaBandMethodPlannerConservativeBound},
		Confidence: QuoteConfidence{Score: .8, Reasons: []string{"fixture measured supply"}},
	}
}

func TestQuoteCompiledProjectUsesLivePricingDecisionAndNanosCeiling(t *testing.T) {
	root := t.TempDir()
	writeProjectFixture(t, root, "input.jsonl", "{\"text\":\"hello\"}\n")
	serverQuote := validProjectServerQuote(t)
	currency, err := ParseCurrency(serverQuote.Currency)
	if err != nil {
		t.Fatal(err)
	}
	expected, err := MoneyNanosFromUSDFloat(currency, serverQuote.Cost.ExpectedUSD)
	if err != nil {
		t.Fatal(err)
	}
	maximum, err := MoneyNanosFromUSDFloat(currency, serverQuote.Cost.MaxUSD)
	if err != nil {
		t.Fatal(err)
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/quote" || r.Header.Get("Authorization") != "Bearer test-project-key" {
			t.Fatalf("wrong project quote request: %s auth=%q", r.URL.Path, r.Header.Get("Authorization"))
		}
		var request cliJobSubmit
		if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
			t.Fatal(err)
		}
		if request.Model.Ref != serverQuote.Model || request.JobType.Type != serverQuote.JobType {
			t.Fatalf("project compiler bypassed resolved model authority: %+v", request)
		}
		writeJSON(w, http.StatusOK, serverQuote)
	}))
	defer server.Close()
	c := &client{base: server.URL, key: "test-project-key", hc: server.Client()}
	quote, err := quoteCompiledProject(c, root, projectQuoteIRFixture(serverQuote, maximum.Nanos+1))
	if err != nil {
		t.Fatal(err)
	}
	if quote.ExpectedCostNanos != expected.Nanos || quote.MaximumCostNanos != maximum.Nanos ||
		quote.CriticalPathP50Secs != 3 || quote.CriticalPathP90Secs != 5 || quote.MinimumConfidence != .8 {
		t.Fatalf("project quote did not preserve fixed-point/time authority: %+v", quote)
	}
	if len(quote.Steps) != 1 || len(quote.Steps[0].PricingDecisionSHA256) != 64 {
		t.Fatalf("step quote lost PricingDecision identity: %+v", quote.Steps)
	}
}

func TestQuoteCompiledProjectRefusesAggregateBuyerCeiling(t *testing.T) {
	root := t.TempDir()
	writeProjectFixture(t, root, "input.jsonl", "{\"text\":\"hello\"}\n")
	serverQuote := validProjectServerQuote(t)
	currency, err := ParseCurrency(serverQuote.Currency)
	if err != nil {
		t.Fatal(err)
	}
	maximum, err := MoneyNanosFromUSDFloat(currency, serverQuote.Cost.MaxUSD)
	if err != nil {
		t.Fatal(err)
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, http.StatusOK, serverQuote)
	}))
	defer server.Close()
	ir := projectQuoteIRFixture(serverQuote, maximum.Nanos-1)
	_, err = quoteCompiledProject(&client{base: server.URL, hc: server.Client()}, root, ir)
	if err == nil || !strings.Contains(err.Error(), "exceeds buyer ceiling") {
		t.Fatalf("aggregate ceiling breach passed: %v", err)
	}
}

func TestProjectCriticalPathUsesDependenciesNotStepOrder(t *testing.T) {
	steps := []ProjectIRStep{
		{ID: "join", DependsOn: []string{"left", "right"}},
		{ID: "right"}, {ID: "left"},
	}
	got, err := projectCriticalPath(steps, map[string]int{"left": 5, "right": 7, "join": 3})
	if err != nil {
		t.Fatal(err)
	}
	if got != 10 {
		t.Fatalf("critical path=%d, want max(5,7)+3=10", got)
	}
}

func TestQuoteCompiledProjectRefusesTamperedPricingDecision(t *testing.T) {
	root := t.TempDir()
	writeProjectFixture(t, root, "input.jsonl", "{\"text\":\"hello\"}\n")
	serverQuote := validProjectServerQuote(t)
	currency, err := ParseCurrency(serverQuote.Currency)
	if err != nil {
		t.Fatal(err)
	}
	maximum, err := MoneyNanosFromUSDFloat(currency, serverQuote.Cost.MaxUSD)
	if err != nil {
		t.Fatal(err)
	}
	serverQuote.Pricing.PolicyRevision += "-tampered"
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, http.StatusOK, serverQuote)
	}))
	defer server.Close()
	_, err = quoteCompiledProject(
		&client{base: server.URL, hc: server.Client()}, root,
		projectQuoteIRFixture(serverQuote, maximum.Nanos+1),
	)
	if err == nil || !strings.Contains(err.Error(), "PricingDecision is invalid") {
		t.Fatalf("tampered PricingDecision was aggregated: %v", err)
	}
}
