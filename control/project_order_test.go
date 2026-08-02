package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
)

func TestProjectOrderPublicAPIIsIdempotentAndBuyerScoped(t *testing.T) {
	installSettlementCurrencyForTest(t, "cad")
	ctx, store, _ := openIsolatedTestStore(t)
	buyer, err := store.CreateBuyerAccount(ctx, "project-order-"+uuid.NewString()+"@example.test", "integration-password", 0)
	if err != nil {
		t.Fatal(err)
	}
	_, key, _, err := store.CreateAPIKey(ctx, buyer, "project order", true)
	if err != nil {
		t.Fatal(err)
	}
	other, err := store.CreateBuyerAccount(ctx, "project-order-other-"+uuid.NewString()+"@example.test", "integration-password", 0)
	if err != nil {
		t.Fatal(err)
	}
	_, otherKey, _, err := store.CreateAPIKey(ctx, other, "project order other", true)
	if err != nil {
		t.Fatal(err)
	}
	server := httptest.NewServer(NewServer(store, nil, nil, nil).Routes())
	t.Cleanup(server.Close)
	body := mustJSON(projectOrderCreateRequest{IRSHA256: strings.Repeat("a", 64), Currency: "cad", BuyerCeilingNanos: 1_000_000})
	post := func(token string) *http.Response {
		req, err := http.NewRequest(http.MethodPost, server.URL+"/v1/projects", bytes.NewReader(body))
		if err != nil {
			t.Fatal(err)
		}
		req.Header.Set("Authorization", "Bearer "+token)
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Idempotency-Key", "project-order-api-"+strings.Repeat("x", 16))
		resp, err := server.Client().Do(req)
		if err != nil {
			t.Fatal(err)
		}
		return resp
	}
	first := post(key)
	defer first.Body.Close()
	if first.StatusCode != http.StatusCreated {
		t.Fatalf("create project order: HTTP %s", first.Status)
	}
	var created ProjectOrder
	if err := json.NewDecoder(first.Body).Decode(&created); err != nil {
		t.Fatal(err)
	}
	if created.Currency != "cad" || created.RemainingNanos != created.BuyerCeilingNanos || created.ID == "" {
		t.Fatalf("create lost CAD fixed-point authority: %+v", created)
	}
	replay := post(key)
	defer replay.Body.Close()
	if replay.StatusCode != http.StatusCreated || replay.Header.Get("Idempotent-Replayed") != "true" {
		t.Fatalf("idempotent project order replay: HTTP %s replay=%q", replay.Status, replay.Header.Get("Idempotent-Replayed"))
	}
	var repeated ProjectOrder
	if err := json.NewDecoder(replay.Body).Decode(&repeated); err != nil {
		t.Fatal(err)
	}
	if repeated.ID != created.ID {
		t.Fatalf("idempotent project order changed identity: %s != %s", repeated.ID, created.ID)
	}
	get := func(token string) int {
		req, err := http.NewRequest(http.MethodGet, server.URL+"/v1/projects/"+created.ID, nil)
		if err != nil {
			t.Fatal(err)
		}
		req.Header.Set("Authorization", "Bearer "+token)
		resp, err := server.Client().Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		return resp.StatusCode
	}
	if got := get(key); got != http.StatusOK {
		t.Fatalf("owner read project order: HTTP %d", got)
	}
	if got := get(otherKey); got != http.StatusNotFound {
		t.Fatalf("other buyer read project order: HTTP %d", got)
	}
}

// TestProjectOrderReservationIsServerSideAndFixedPoint is deliberately a Store
// integration test rather than a client-ledger unit test. It proves the same
// transaction that makes a job visible also records the step's exact
// PricingDecision ceiling, serializes the ceiling through the order row, and
// leaves no job behind on either duplicate-step or budget refusal.
func TestProjectOrderReservationIsServerSideAndFixedPoint(t *testing.T) {
	installSettlementCurrencyForTest(t, "usd")
	ctx, store, pool := openIsolatedMoneyPathStore(t)
	f := seedMoneyPathFixture(t, ctx, store, pool, moneyPathSeedOpts{TaskCount: 1})
	tasks := makeTasks(f, 1)
	f.TaskIDs = []uuid.UUID{tasks[0].ID}
	schedule := testEconomicSchedule()
	schedule.MinChargeBatchUSD = 5
	schedule.ControlPlanePerTaskUSD = 0
	schedule.ControlPlanePerBatchUSD = 0.01
	schedule.ControlPlaneAllocationPolicy = controlPlaneAllocationChargeBatchV1
	f.Plan = BuildEconomicPlan(EconomicPlanInput{
		BaseComputeUSD: 0.20, InitialTaskCount: 1, ExtraTaskReserve: 1, SupplierShare: 0.97,
	}, schedule)
	if !f.Plan.Executable {
		t.Fatalf("fixed-point fixture plan refused: %s", f.Plan.BlockReason)
	}
	job := validJobRow(t, f, tasks)

	quoteID := uuid.New()
	quote := Quote{
		QuoteID: "q_" + quoteID.String(), bareID: quoteID,
		etaRawP50Secs: job.ETARawSecs,
		JobType:       job.JobType, Model: job.ModelRef, Tier: job.Tier,
		Currency: job.EconomicPlan.Schedule.Currency,
		Workload: job.WorkloadDecision, Placement: job.PlacementRequirement,
		ComputePlan: job.ComputePlan, Pricing: job.PricingDecision,
		Time: QuoteTime{P50Secs: job.ComputePlan.ETAP50Secs, P90Secs: job.ComputePlan.ETAP90Secs,
			WorstCaseSecs: job.ComputePlan.ETAWorstCaseSecs, ConfidenceBandMethod: job.ComputePlan.ETAConfidenceBandMethod},
		Economics:   job.EconomicPlan,
		InputSHA256: strings.Repeat("a", 64),
		ExpiresAt:   time.Now().Add(quoteTTL).UTC(),
	}
	if err := store.InsertQuote(ctx, f.BuyerID, quote); err != nil {
		t.Fatalf("insert firm project quote: %v", err)
	}
	pricingSHA, err := pricingDecisionDigest(job.PricingDecision)
	if err != nil {
		t.Fatal(err)
	}
	accepted := job.PricingDecision.FixedPoint.AcceptedCeilingNanos
	if accepted <= 1 {
		t.Fatalf("fixture has no positive exact project reserve: %d", accepted)
	}
	orderInput := projectOrderCreateRequest{IRSHA256: strings.Repeat("b", 64),
		Currency: job.EconomicPlan.Schedule.Currency, BuyerCeilingNanos: accepted}
	order, replay, err := store.CreateProjectOrder(ctx, f.BuyerID, orderInput,
		"project-order-test-"+uuid.NewString(), strings.Repeat("c", 64))
	if err != nil || replay {
		t.Fatalf("create project order: order=%+v replay=%t err=%v", order, replay, err)
	}

	job.QuoteID = quoteID
	job.FirmQuote = true
	job.FirmQuoteMaxUSD = job.PricingDecision.MaximumBuyerPrice
	job.ProjectOrderID, err = uuid.Parse(order.ID)
	if err != nil {
		t.Fatal(err)
	}
	job.ProjectStepID = "embed"
	if err := store.SubmitJobTx(ctx, job, tasks); err != nil {
		t.Fatalf("submit fixed-point project step: %v", err)
	}
	got, err := store.GetProjectOrder(ctx, f.BuyerID, job.ProjectOrderID)
	if err != nil {
		t.Fatalf("read project order: %v", err)
	}
	if got.ReservedNanos != accepted || got.RemainingNanos != 0 || got.Currency != orderInput.Currency {
		t.Fatalf("server project reserve = %+v, want exact accepted ceiling %d", got, accepted)
	}
	var storedSHA string
	if err := pool.QueryRow(ctx, `
		SELECT pricing_decision_sha256 FROM project_order_steps
		 WHERE project_order_id=$1 AND step_id='embed'`, job.ProjectOrderID).Scan(&storedSHA); err != nil {
		t.Fatalf("read project step reservation: %v", err)
	}
	if storedSHA != pricingSHA {
		t.Fatalf("reservation pricing SHA=%s want frozen %s", storedSHA, pricingSHA)
	}

	duplicate := *job
	duplicate.ID = uuid.New()
	duplicateTasks := makeTasks(f, 1)
	duplicateTasks[0].ID = uuid.New()
	duplicateTasks[0].JobID = duplicate.ID
	if err := store.SubmitJobTx(ctx, &duplicate, duplicateTasks); !errors.Is(err, errProjectStepReserved) {
		t.Fatalf("second accepted job for one project step: %v", err)
	}
	if countJobRows(t, ctx, pool, duplicate.ID) != 0 {
		t.Fatal("duplicate project step left a job row")
	}

	underfunded, _, err := store.CreateProjectOrder(ctx, f.BuyerID,
		projectOrderCreateRequest{IRSHA256: strings.Repeat("d", 64), Currency: orderInput.Currency, BuyerCeilingNanos: accepted - 1},
		"project-order-underfunded-"+uuid.NewString(), strings.Repeat("e", 64))
	if err != nil {
		t.Fatalf("create underfunded project order: %v", err)
	}
	underfundedID, err := uuid.Parse(underfunded.ID)
	if err != nil {
		t.Fatal(err)
	}
	over := *job
	over.ID = uuid.New()
	over.ProjectOrderID = underfundedID
	over.ProjectStepID = "embed"
	overTasks := makeTasks(f, 1)
	overTasks[0].ID = uuid.New()
	overTasks[0].JobID = over.ID
	if err := store.SubmitJobTx(ctx, &over, overTasks); !errors.Is(err, errProjectBudget) {
		t.Fatalf("over-ceiling project job was accepted: %v", err)
	}
	if countJobRows(t, ctx, pool, over.ID) != 0 {
		t.Fatal("over-ceiling project job left a row")
	}
}
