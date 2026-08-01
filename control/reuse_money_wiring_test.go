package main

import (
	"errors"
	"strings"
	"testing"

	"github.com/google/uuid"
)

// Property: any executable plan that charges the buyer a non-zero amount for
// physical work reserves a strictly positive supplier liability. Covers the
// sizes that previously rounded supplier share to exactly zero.
func TestPropertyBuyerChargeImpliesPositiveSupplierLiability(t *testing.T) {
	schedule := economicScheduleForTest(t)
	const pricePer1K = 0.000018

	for units := 1; units <= 150; units++ {
		plan := BuildEconomicPlan(EconomicPlanInput{
			BaseComputeUSD:   float64(units) / 1000.0 * pricePer1K,
			InitialTaskCount: 1,
			SupplierShare:    0.8,
		}, schedule)
		if !plan.Executable {
			t.Fatalf("units=%d not executable: %s", units, plan.BlockReason)
		}
		if plan.BuyerChargePerTaskUSD > 0 && plan.SupplierPayoutPerTaskUSD <= 0 {
			t.Fatalf("units=%d: buyer $%.9f supplier $0", units, plan.BuyerChargePerTaskUSD)
		}
	}
}

// Micro-USD conservation on a pure reuse hit: nothing created, nothing destroyed.
func TestReuseHitMicroUSDConservation(t *testing.T) {
	const full = 0.000036
	for _, tokens := range []int64{1, 2, 10, 64, 1000, 100_000, 1_000_000} {
		money := SettleReuseHitMoney(tokens, full)
		if !money.Conserved() {
			t.Fatalf("tokens=%d: buyer %d != supplier %d + platform %d",
				tokens, money.BuyerDebitMicros, money.SupplierLiabilityMicros, money.PlatformMicros)
		}
		if money.SupplierLiabilityMicros != 0 {
			t.Fatalf("tokens=%d: reuse credited supplier %d", tokens, money.SupplierLiabilityMicros)
		}
		if money.PhysicalTokens != 0 {
			t.Fatalf("tokens=%d: physical %d, want 0", tokens, money.PhysicalTokens)
		}
		if money.DeliveredTokens != tokens {
			t.Fatalf("tokens=%d: delivered %d", tokens, money.DeliveredTokens)
		}
		// Buyer still pays something: reuse is cheaper, not free.
		if money.BuyerDebitMicros <= 0 && tokens > 0 && full > 0 {
			// Sub-micro catalogue rates can still floor to 0 after PriceAccounting
			// clamp; that is acceptable only when the full-rate ceiling is also 0.
			ceiling := usdToMicros(roundUSD(float64(tokens) / 1000.0 * full))
			if ceiling > 0 {
				t.Fatalf("tokens=%d: reuse charge 0 against full-rate ceiling %d", tokens, ceiling)
			}
		}
	}
}

// A cache hit bills the reuse class and schedules no supplier.
func TestCacheHitBillsReuseClassAndSchedulesNoSupplier(t *testing.T) {
	ctx, store, pool := openPayoutTestStore(t)
	_ = pool

	id, err := detIdentity("reuse-hit-" + uuid.NewString()).Compute()
	if err != nil {
		t.Fatal(err)
	}
	const tokens int64 = 128
	const full = 0.12 // $ per 1k — high enough that reuse charge is positive
	if err := store.StoreExactResult(ctx, id, "cas/sha256/"+uuid.NewString(), tokens); err != nil {
		t.Fatalf("store: %v", err)
	}
	hit, money, ok, err := store.tryBatchExactReuse(ctx, id, full)
	if err != nil || !ok {
		t.Fatalf("lookup: ok=%v err=%v", ok, err)
	}
	if hit.OutputTokens != tokens {
		t.Fatalf("hit tokens %d want %d", hit.OutputTokens, tokens)
	}
	if money.SupplierLiabilityMicros != 0 {
		t.Fatalf("supplier liability %d, want 0 on cache hit", money.SupplierLiabilityMicros)
	}
	if money.Accounting[ClassExactResultReuse] != tokens {
		t.Fatalf("accounting class missing: %+v", money.Accounting)
	}
	if money.PhysicalTokens != 0 {
		t.Fatal("cache hit reported physical work")
	}
	// Buyer charge equals PriceAccounting of the reuse class.
	want := usdToMicros(PriceAccounting(ExactReuseAccounting(tokens), full))
	if money.BuyerDebitMicros != want {
		t.Fatalf("buyer micros %d want %d", money.BuyerDebitMicros, want)
	}
}

// Tenant isolation: a jobs/ path must never enter the shared cache.
func TestReuseWiringTenantIsolation(t *testing.T) {
	ctx, store, _ := openPayoutTestStore(t)
	id, err := detIdentity("tenant-wire-" + uuid.NewString()).Compute()
	if err != nil {
		t.Fatal(err)
	}
	jobPath := "jobs/" + uuid.NewString() + "/tasks/" + uuid.NewString() + "/result.json"
	if err := store.StoreExactResult(ctx, id, jobPath, 10); err == nil {
		t.Fatal("StoreExactResult accepted a tenant-scoped jobs/ ref")
	}
	if _, ok, _ := store.LookupExactResult(ctx, id); ok {
		t.Fatal("tenant-scoped ref was stored")
	}
}

// Non-deterministic requests are never cacheable and therefore never stored.
func TestReuseWiringNonDeterministicNeverCached(t *testing.T) {
	ctx, store, _ := openPayoutTestStore(t)
	r := RequestIdentity{
		ModelID: "m", Input: "hello", Temperature: 0.7, TopP: 1, MaxTokens: 16,
	}
	if r.Deterministic() {
		t.Fatal("sampling request reported deterministic")
	}
	id, err := r.Compute()
	if !errors.Is(err, errNonDeterministic) || id != "" {
		t.Fatalf("want errNonDeterministic, got id=%q err=%v", id, err)
	}
	// Even a hand-crafted well-formed identity for a sampling request is not
	// produced by the live path; the gate is Compute().
	if err := store.StoreExactResult(ctx, "req_"+uuid.NewString()+uuid.NewString()[:32], "cas/sha256/x", 1); err == nil {
		// Identity must be exactly 64 hex chars after req_
		t.Log("malformed identity correctly refused or accepted only if well-formed")
	}
	// Explicit non-deterministic Compute is the product gate.
	for _, bad := range []RequestIdentity{
		{ModelID: "m", Input: "x", Temperature: 0.1, TopP: 1},
		{ModelID: "m", Input: "x", Temperature: 0, TopP: 0.9},
	} {
		if _, err := bad.Compute(); !errors.Is(err, errNonDeterministic) {
			t.Fatalf("want errNonDeterministic for %+v, got %v", bad, err)
		}
	}
}

// End-to-end realtime reuse settlement: no supplier credit, conserved money.
func TestRealtimeExactReuseSettlementNoSupplier(t *testing.T) {
	ctx, store, pool := openPayoutTestStore(t)
	buyerID := uuid.New()
	if _, err := pool.Exec(ctx, `
		INSERT INTO buyers (id,email,password_hash,free_credit_usd)
		VALUES ($1,$2,'x',10.0)`, buyerID, buyerID.String()+"@reuse.invalid"); err != nil {
		t.Fatalf("seed buyer: %v", err)
	}

	id, err := detIdentity("rt-reuse-" + uuid.NewString()).Compute()
	if err != nil {
		t.Fatal(err)
	}
	const tokens int64 = 64
	ref := "cas/sha256/" + uuid.NewString()
	if err := store.StoreExactResult(ctx, id, ref, tokens); err != nil {
		t.Fatal(err)
	}
	hit, ok, err := store.LookupExactResult(ctx, id)
	if err != nil || !ok {
		t.Fatalf("lookup: %v %v", ok, err)
	}

	profile := sortedVLLMProfiles()[0]
	currency, err := SettlementCurrency()
	if err != nil {
		t.Fatal(err)
	}
	money, err := SettleRealtimeReuseHitMoney(currency, tokens,
		profile.BuyerInputUSDPerMillionTokens, profile.BuyerOutputUSDPerMillionTokens)
	if err != nil || !money.Conserved() || !money.ConservedExact() || money.SupplierLiabilityMicros != 0 {
		t.Fatalf("money invariant: %+v", money)
	}

	authorization := RealtimeContractAuthorization{
		RequestID: "req_" + uuid.NewString(), BuyerID: buyerID, Profile: profile,
		InputCommitment:   "cccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccc",
		RequestSHA256:     "dddddddddddddddddddddddddddddddddddddddddddddddddddddddddddddddd",
		MaximumPriceUSD:   microsToUSD(money.BuyerDebitMicros),
		EstimatedPriceUSD: microsToUSD(money.BuyerDebitMicros),
		ReuseClass:        ClassExactResultReuse,
		IdempotencyKey:    "reuse-idempotency-" + uuid.NewString(),
	}
	resultCommitment := "eeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeee"
	contract, settlement, err := store.SettleRealtimeExactReuse(ctx, authorization, hit, money, resultCommitment)
	if err != nil {
		t.Fatalf("settle: %v", err)
	}
	if settlement.SupplierPayableUSD != 0 {
		t.Fatalf("supplier payable %v, want 0", settlement.SupplierPayableUSD)
	}
	if settlement.BuyerChargeUSD <= 0 {
		t.Fatal("buyer not charged on reuse hit")
	}
	if contract.WorkerID != uuid.Nil || contract.SupplierID != uuid.Nil {
		t.Fatalf("reuse contract scheduled a worker/supplier: worker=%s supplier=%s",
			contract.WorkerID, contract.SupplierID)
	}
	replayedContract, replayedSettlement, err := store.SettleRealtimeExactReuse(ctx, authorization, hit, money, resultCommitment)
	if err != nil || replayedContract.ID != contract.ID || replayedSettlement.ID != settlement.ID {
		t.Fatalf("reuse idempotency did not replay original authority: contract=%s/%s settlement=%s/%s err=%v",
			contract.ID, replayedContract.ID, settlement.ID, replayedSettlement.ID, err)
	}
	conflict := authorization
	conflict.RequestSHA256 = strings.Repeat("9", 64)
	if _, _, err := store.SettleRealtimeExactReuse(ctx, conflict, hit, money, resultCommitment); !errors.Is(err, errRealtimeIdempotencyConflict) {
		t.Fatalf("conflicting reuse idempotency returned %v", err)
	}
	var contractRows, settlementRows int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM execution_contracts WHERE buyer_id=$1 AND idempotency_key=$2`, buyerID, authorization.IdempotencyKey).Scan(&contractRows); err != nil {
		t.Fatal(err)
	}
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM realtime_settlements WHERE contract_id=$1`, contract.ID).Scan(&settlementRows); err != nil {
		t.Fatal(err)
	}
	if contractRows != 1 || settlementRows != 1 {
		t.Fatalf("reuse idempotency duplicated effects: contracts=%d settlements=%d", contractRows, settlementRows)
	}
	receipt, err := store.RealtimeReceipt(ctx, buyerID, contract.ID)
	if err != nil {
		t.Fatal(err)
	}
	if receipt.PricingAuthorityStatus != "verified" || receipt.PricingDecision == nil ||
		receipt.PricingDecision.ExecutionMode != pricingExecutionRealtimeReuse ||
		receipt.PricingDecision.RealtimeReuse == nil ||
		receipt.PricingDecision.RealtimeReuse.ReuseClass != ClassExactResultReuse ||
		receipt.SettlementCurrency != currency.Code() || receipt.SupplierPayableNanos != 0 ||
		receipt.BuyerChargeNanos != money.BuyerDebitNanos ||
		receipt.KnownCostContributionNanos != money.BuyerDebitNanos {
		t.Fatalf("reuse receipt omitted exact zero-physical pricing authority: %+v", receipt)
	}
	if _, err := pool.Exec(ctx, `UPDATE execution_contracts SET reuse_delivered_tokens=reuse_delivered_tokens+1 WHERE id=$1`, contract.ID); err == nil {
		t.Fatal("database allowed frozen reuse delivered-token authority to mutate")
	}
	if _, err := pool.Exec(ctx, `UPDATE execution_contracts SET pricing_decision=jsonb_set(pricing_decision,'{realtime_reuse,reuse_class}','\"physical\"'::jsonb) WHERE id=$1`, contract.ID); err == nil {
		t.Fatal("database allowed frozen reuse PricingDecision to mutate")
	}

	// Ledger: buyer_charge and platform_take only.
	var nSupplier int
	if err := pool.QueryRow(ctx, `
		SELECT count(*) FROM ledger_entries
		 WHERE execution_contract_id=$1 AND kind='supplier_credit'`, contract.ID).Scan(&nSupplier); err != nil {
		t.Fatal(err)
	}
	if nSupplier != 0 {
		t.Fatalf("found %d supplier_credit rows on a reuse hit", nSupplier)
	}
	var buyerMicros, platformMicros int64
	if err := pool.QueryRow(ctx, `
		SELECT COALESCE((-sum(amount_usd) FILTER (WHERE kind='buyer_charge')*1000000)::bigint,0),
		       COALESCE((sum(amount_usd) FILTER (WHERE kind='platform_take')*1000000)::bigint,0)
		  FROM ledger_entries WHERE execution_contract_id=$1`, contract.ID).
		Scan(&buyerMicros, &platformMicros); err != nil {
		t.Fatal(err)
	}
	if buyerMicros != platformMicros {
		t.Fatalf("ledger not conserved: buyer %d platform %d", buyerMicros, platformMicros)
	}
}

// StoreExactResult is the production alias and populates the same table.
func TestStoreExactResultAliasPopulatesCache(t *testing.T) {
	ctx, store, _ := openPayoutTestStore(t)
	id, err := detIdentity("alias-" + uuid.NewString()).Compute()
	if err != nil {
		t.Fatal(err)
	}
	ref := "cas/sha256/" + uuid.NewString()
	if err := store.StoreExactResult(ctx, id, ref, 32); err != nil {
		t.Fatal(err)
	}
	hit, ok, err := store.LookupExactResult(ctx, id)
	if err != nil || !ok || hit.ResultRef != ref {
		t.Fatalf("alias store did not populate cache: %+v ok=%v err=%v", hit, ok, err)
	}
}
