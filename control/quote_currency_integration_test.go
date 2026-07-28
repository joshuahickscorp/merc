package main

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
)

func TestQuoteCurrencyIsVisibleImmutableAndBoundAtSubmission(t *testing.T) {
	installSettlementCurrencyForTest(t, "cad")
	ctx, store, pool := openIsolatedTestStore(t)
	buyerID := uuid.New()
	if _, err := pool.Exec(ctx, `INSERT INTO buyers (id,email,password_hash)
		VALUES ($1,$2,'x')`, buyerID, "quote-currency-"+buyerID.String()+"@test"); err != nil {
		t.Fatal(err)
	}

	workload, compute, economic := computePlanFixture(t)
	schedule := economic.Schedule
	schedule.Currency = "cad"
	economic = BuildEconomicPlan(economic.Input, schedule)
	if err := ValidateComputePlanEconomicSnapshot(compute, workload, economic); err != nil {
		t.Fatalf("CAD economic fixture: %v", err)
	}

	quoteID := uuid.New()
	quote := Quote{
		QuoteID:     "q_" + quoteID.String(),
		bareID:      quoteID,
		JobType:     workload.RuntimeJobType,
		Model:       workload.Binding.Model.Ref,
		Tier:        workload.Binding.Tier,
		Currency:    "cad",
		Workload:    workload,
		ComputePlan: compute,
		Economics:   economic,
		InputSHA256: strings.Repeat("a", 64),
		ExpiresAt:   time.Now().Add(quoteTTL).UTC(),
	}
	if err := store.InsertQuote(ctx, buyerID, quote); err != nil {
		t.Fatal(err)
	}

	encoded, err := json.Marshal(quote)
	if err != nil {
		t.Fatal(err)
	}
	var wire struct {
		Currency  string       `json:"currency"`
		Economics EconomicPlan `json:"economics"`
	}
	if err := json.Unmarshal(encoded, &wire); err != nil {
		t.Fatal(err)
	}
	if wire.Currency != "cad" || wire.Economics.Schedule.Currency != "cad" {
		t.Fatalf("buyer quote omits CAD authority: %s", encoded)
	}

	bound, err := store.GetBindableQuote(ctx, quoteID, buyerID)
	if err != nil {
		t.Fatal(err)
	}
	if bound.Currency != "cad" || bound.EconomicPlan.Schedule.Currency != "cad" {
		t.Fatalf("bound quote currency=%q schedule=%q",
			bound.Currency, bound.EconomicPlan.Schedule.Currency)
	}
	if err := validateBoundQuoteCurrency(bound); err != nil {
		t.Fatalf("CAD quote rejected under CAD settlement: %v", err)
	}

	setSettlementCurrency(MustParseCurrency("usd"))
	if err := validateBoundQuoteCurrency(bound); !errors.Is(err, errCurrencyMismatch) {
		t.Fatalf("CAD quote was accepted after USD cutover: %v", err)
	}

	if _, err := pool.Exec(ctx, `UPDATE quotes SET currency='usd' WHERE id=$1`, quoteID); err == nil {
		t.Fatal("database allowed frozen quote currency to change")
	}
	if _, err := pool.Exec(ctx, `UPDATE quotes
		SET economic_plan=jsonb_set(economic_plan,'{schedule,currency}','"usd"')
		WHERE id=$1`, quoteID); err == nil {
		t.Fatal("database allowed frozen quote economic authority to change")
	}
	if _, err := pool.Exec(ctx, `INSERT INTO quotes
		(job_type,currency,economic_plan,quote_json)
		VALUES ('embed','cad',
			'{"schedule":{"currency":"usd"}}',
			'{"currency":"cad","economics":{"schedule":{"currency":"usd"}}}')`,
	); err == nil {
		t.Fatal("database allowed a new quote whose JSON economic currency differs")
	}
	if _, err := pool.Exec(ctx, canonicalSchema); err != nil {
		t.Fatalf("canonical schema is not idempotent with frozen quotes: %v", err)
	}
}

func TestInsertQuoteRejectsCurrencyMismatchBeforeDatabaseWrite(t *testing.T) {
	installSettlementCurrencyForTest(t, "cad")
	ctx, store, _ := openIsolatedTestStore(t)
	workload, compute, economic := computePlanFixture(t)
	schedule := economic.Schedule
	schedule.Currency = "cad"
	economic = BuildEconomicPlan(economic.Input, schedule)
	quote := Quote{
		bareID: uuid.New(), Currency: "usd",
		Workload: workload, ComputePlan: compute, Economics: economic,
	}
	err := store.InsertQuote(ctx, uuid.New(), quote)
	if !errors.Is(err, errCurrencyMismatch) {
		t.Fatalf("mismatched quote currency was not rejected: %v", err)
	}
}
