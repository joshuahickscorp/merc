package main

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"
)

// findSubMicroCeilingBounds returns token bounds whose exact settlement ceiling
// projects to fewer micros than a hold-ceil (remainder under half a micro).
// That gap is the funding-precision defect: projected need under-holds.
func findSubMicroCeilingBounds(t *testing.T, profile VLLMRuntimeProfile) (
	maxPrompt, maxCompletion, estPrompt, estCompletion, exactNanos, projectedMicros, holdMicros int64,
	maxUSD, estUSD float64,
) {
	t.Helper()
	currency := MustParseCurrency(SettlementCurrencyCode())
	fx, err := loadRealtimeFXAuthority(currency)
	must(t, err)
	// Prefer small estimated tokens; maxima must stay ordered and positive.
	// Sub-micro remainders show up at small token counts under current rates.
	for maxP := int64(1); maxP <= 5_000; maxP++ {
		for maxC := int64(1); maxC <= 500; maxC++ {
			estP, estC := int64(1), int64(1)
			if estP > maxP {
				estP = maxP
			}
			if estC > maxC {
				estC = maxC
			}
			refExp, refMax, _, settlementMax, err := realtimeBuyerPriceBounds(
				profile, maxP, maxC, estP, estC, currency, fx)
			if err != nil || settlementMax.Nanos <= 0 || refMax.Nanos < refExp.Nanos {
				continue
			}
			proj, err := LedgerMicrosFromNanos(settlementMax)
			if err != nil {
				continue
			}
			hold := ceilNanosToMicros(settlementMax.Nanos)
			if hold <= proj {
				continue
			}
			// Auth path compares caller projections against reference (USD) bounds.
			maxUSD, err = projectRealtimeNanosToMajor(refMax)
			must(t, err)
			estUSD, err = projectRealtimeNanosToMajor(refExp)
			must(t, err)
			return maxP, maxC, estP, estC, settlementMax.Nanos, proj, hold, maxUSD, estUSD
		}
	}
	t.Fatal("no token bounds produce project < hold-ceil under current profile rates")
	return 0, 0, 0, 0, 0, 0, 0, 0, 0
}

// TestCeilNanosToMicrosHoldsHigherWholeMicro pins the holding direction:
// a need with a sub-micro remainder holds the next whole micro. Ledger
// projection (to-nearest) is deliberately different and must not be reused.
func TestCeilNanosToMicrosHoldsHigherWholeMicro(t *testing.T) {
	const needNanos int64 = 1_000_050 // remainder 50 < half micro
	hold := ceilNanosToMicros(needNanos)
	if hold != 1001 {
		t.Fatalf("hold micros=%d, want 1001 (ceil)", hold)
	}
	projected, err := LedgerMicrosFromNanos(MoneyNanos{
		Currency: MustParseCurrency("usd"), Nanos: needNanos,
	})
	must(t, err)
	if projected != 1000 {
		t.Fatalf("ledger projection micros=%d, want 1000 (to-nearest); fixture drift", projected)
	}
	if hold <= projected {
		t.Fatalf("hold %d must exceed projection %d for this remainder", hold, projected)
	}
	// Exact whole micros are unchanged.
	if got := ceilNanosToMicros(2_000_000); got != 2000 {
		t.Fatalf("exact-micro ceil=%d, want 2000", got)
	}
	if got := ceilNanosToMicros(0); got != 0 {
		t.Fatalf("zero ceil=%d, want 0", got)
	}
	if got := ceilNanosToMicros(-50); got != 0 {
		t.Fatalf("negative ceil=%d, want 0", got)
	}
}

// TestRealtimeFundingNeedCeilsSubMicroRemainder funds a buyer with exactly the
// projected micros for one ceiling and proves the gate refuses when the true
// nano need ceils one micro higher.
func TestRealtimeFundingNeedCeilsSubMicroRemainder(t *testing.T) {
	installSettlementCurrencyForTest(t, "usd")
	ctx, store, pool := openIsolatedTestStore(t)
	profile, _, _ := realtimeFundingFixture(t, ctx, store, pool)
	maxP, maxC, estP, estC, exactNanos, projected, hold, maxUSD, estUSD :=
		findSubMicroCeilingBounds(t, profile)
	if hold != projected+1 {
		t.Fatalf("expected one-micro gap, got hold=%d projected=%d exact=%d", hold, projected, exactNanos)
	}

	buyerID := uuid.New()
	if _, err := pool.Exec(ctx, `INSERT INTO buyers (id,email,free_credit_usd) VALUES ($1,$2,0)`,
		buyerID, buyerID.String()+"@hold-ceil.invalid"); err != nil {
		t.Fatal(err)
	}
	// Fund only the projected micros — enough under the old float path, short under ceil.
	must(t, store.SeedPrepaidBalance(ctx, buyerID, projected, "seed-hold-ceil-"+buyerID.String()))

	tx, err := pool.Begin(ctx)
	must(t, err)
	err = evaluateRealtimeBuyerFunding(ctx, tx, buyerID, exactNanos)
	_ = tx.Rollback(ctx)
	if !errors.Is(err, errRealtimeInsufficientFunds) && !errors.Is(err, errRealtimeTopupRequired) {
		t.Fatalf("projected-only balance admitted need=%d (hold=%d projected=%d): %v",
			exactNanos, hold, projected, err)
	}

	// Funding the true hold micros must pass.
	must(t, store.SeedPrepaidBalance(ctx, buyerID, hold-projected, "seed-hold-ceil-topup-"+buyerID.String()))
	tx, err = pool.Begin(ctx)
	must(t, err)
	err = evaluateRealtimeBuyerFunding(ctx, tx, buyerID, exactNanos)
	_ = tx.Rollback(ctx)
	if err != nil {
		t.Fatalf("true-ceiling funding refused: %v (hold=%d)", err, hold)
	}

	// Non-envelope authorize must also refuse the under-funded buyer after
	// draining back to projected-only balance.
	// Balance is now `hold`; debit conceptually by re-seeding a fresh buyer.
	buyer2 := uuid.New()
	if _, err := pool.Exec(ctx, `INSERT INTO buyers (id,email,free_credit_usd) VALUES ($1,$2,0)`,
		buyer2, buyer2.String()+"@hold-ceil-auth.invalid"); err != nil {
		t.Fatal(err)
	}
	must(t, store.SeedPrepaidBalance(ctx, buyer2, projected, "seed-hold-ceil-auth-"+buyer2.String()))
	_, _, err = store.AuthorizeRealtimeContract(ctx, RealtimeContractAuthorization{
		RequestID: "req-hold-ceil-" + uuid.NewString(), BuyerID: buyer2, Profile: profile,
		InputCommitment: strings.Repeat("a", 64), RequestSHA256: strings.Repeat("b", 64),
		MaximumPriceUSD: maxUSD, EstimatedPriceUSD: estUSD, DeadlineAt: time.Now().Add(time.Minute),
		MaximumPromptTokens: maxP, MaximumCompletionTokens: maxC,
		EstimatedPromptTokens: estP, EstimatedCompletionTokens: estC,
	})
	if !errors.Is(err, errRealtimeInsufficientFunds) && !errors.Is(err, errRealtimeTopupRequired) {
		t.Fatalf("authorize with projected-only balance err=%v, want insufficient (tokens %d/%d exact=%d)",
			err, maxP, maxC, exactNanos)
	}
}

// TestRealtimeConcurrentAuthRefusesProjectedMicroOverAdmit is the load-bearing
// defect proof: N concurrent non-envelope admits where sum(projected micros)
// fits the balance but sum(exact nano ceilings ceiled to micros) does not.
// Before the fix every admit passed; after, at least one refuses and open
// holds never exceed the prepaid balance.
func TestRealtimeConcurrentAuthRefusesProjectedMicroOverAdmit(t *testing.T) {
	installSettlementCurrencyForTest(t, "usd")
	ctx, store, pool := openIsolatedTestStore(t)
	profile, _, _ := realtimeFundingFixture(t, ctx, store, pool)
	maxP, maxC, estP, estC, exactNanos, projected, hold, maxUSD, estUSD :=
		findSubMicroCeilingBounds(t, profile)

	const n = 8
	// Balance covers every projected micro and is short of every true hold.
	balance := projected * n
	if hold*n <= balance {
		t.Fatalf("fixture gap collapsed: hold=%d projected=%d n=%d", hold, projected, n)
	}
	// Old path would admit all n because each need was `projected` and
	// n*projected == balance. New path holds `hold` each → at most balance/hold.
	maxAdmissible := balance / hold
	if maxAdmissible >= n {
		t.Fatalf("maxAdmissible=%d >= n=%d; cannot prove over-admit refusal", maxAdmissible, n)
	}

	buyerID := uuid.New()
	if _, err := pool.Exec(ctx, `INSERT INTO buyers (id,email,free_credit_usd) VALUES ($1,$2,0)`,
		buyerID, buyerID.String()+"@proj-overadmit.invalid"); err != nil {
		t.Fatal(err)
	}
	must(t, store.SeedPrepaidBalance(ctx, buyerID, balance, "seed-proj-overadmit-"+buyerID.String()))

	var (
		wg        sync.WaitGroup
		start     = make(chan struct{})
		okCount   atomic.Int64
		failCount atomic.Int64
	)
	wg.Add(n)
	for i := 0; i < n; i++ {
		go func(i int) {
			defer wg.Done()
			<-start
			_, _, err := store.AuthorizeRealtimeContract(context.Background(), RealtimeContractAuthorization{
				RequestID: fmt.Sprintf("req-proj-overadmit-%d-%s", i, uuid.NewString()),
				BuyerID:   buyerID, Profile: profile,
				InputCommitment: strings.Repeat(fmt.Sprintf("%x", i%16), 64)[:64],
				RequestSHA256:   strings.Repeat(fmt.Sprintf("%x", (i+5)%16), 64)[:64],
				MaximumPriceUSD: maxUSD, EstimatedPriceUSD: estUSD, DeadlineAt: time.Now().Add(time.Minute),
				MaximumPromptTokens: maxP, MaximumCompletionTokens: maxC,
				EstimatedPromptTokens: estP, EstimatedCompletionTokens: estC,
			})
			if err == nil {
				okCount.Add(1)
				return
			}
			if errors.Is(err, errRealtimeInsufficientFunds) || errors.Is(err, errRealtimeTopupRequired) ||
				errors.Is(err, errRealtimeNoSupply) {
				failCount.Add(1)
				return
			}
			t.Errorf("unexpected auth error: %v", err)
			failCount.Add(1)
		}(i)
	}
	close(start)
	wg.Wait()

	ok := okCount.Load()
	if ok > maxAdmissible {
		t.Fatalf("over-admitted: ok=%d fail=%d maxAdmissible=%d (balance=%d hold=%d projected=%d exactNanos=%d)",
			ok, failCount.Load(), maxAdmissible, balance, hold, projected, exactNanos)
	}
	if failCount.Load() < 1 {
		t.Fatalf("expected at least one funding refusal; ok=%d fail=%d", ok, failCount.Load())
	}
	// Open EXECUTING holds (ceiled from accepted_ceiling_nanos) must not exceed balance.
	var reservedHold int64
	if err := pool.QueryRow(ctx, `
		SELECT COALESCE(SUM(
		  CASE
		    WHEN COALESCE((pricing_decision #>> '{fixed_point,accepted_ceiling_nanos}')::bigint, 0) > 0
		    THEN (( (pricing_decision #>> '{fixed_point,accepted_ceiling_nanos}')::bigint + 999) / 1000)
		    ELSE ROUND(maximum_price_usd * 1000000)::bigint
		  END), 0)::bigint
		  FROM execution_contracts WHERE buyer_id=$1 AND state='EXECUTING'`, buyerID).Scan(&reservedHold); err != nil {
		t.Fatal(err)
	}
	if reservedHold > balance {
		t.Fatalf("open hold micros %d exceed prepaid balance %d (ok=%d)", reservedHold, balance, ok)
	}
	t.Logf("concurrent projected-over-admit: ok=%d fail=%d maxAdmissible=%d hold=%d projected=%d balance=%d reservedHold=%d",
		ok, failCount.Load(), maxAdmissible, hold, projected, balance, reservedHold)
}

// TestRealtimeEnvelopeAndNonEnvelopeHoldSameNeed proves both admission paths
// derive the same exact nano need and therefore the same micro hold for one
// request shape.
func TestRealtimeEnvelopeAndNonEnvelopeHoldSameNeed(t *testing.T) {
	installSettlementCurrencyForTest(t, "usd")
	ctx, store, pool := openIsolatedTestStore(t)
	profile, _, _ := realtimeFundingFixture(t, ctx, store, pool)
	maxP, maxC, estP, estC, exactNanos, _, hold, maxUSD, estUSD :=
		findSubMicroCeilingBounds(t, profile)

	auth := RealtimeContractAuthorization{
		RequestID: "req-path-parity-" + uuid.NewString(), BuyerID: uuid.New(), Profile: profile,
		InputCommitment: strings.Repeat("e", 64), RequestSHA256: strings.Repeat("f", 64),
		MaximumPriceUSD: maxUSD, EstimatedPriceUSD: estUSD, DeadlineAt: time.Now().Add(time.Minute),
		MaximumPromptTokens: maxP, MaximumCompletionTokens: maxC,
		EstimatedPromptTokens: estP, EstimatedCompletionTokens: estC,
	}
	currency := MustParseCurrency(SettlementCurrencyCode())
	fx, err := loadRealtimeFXAuthority(currency)
	must(t, err)
	auth.FX = fx

	needFromAuth, err := realtimeAuthNeedNanos(auth)
	must(t, err)
	if needFromAuth != exactNanos {
		t.Fatalf("realtimeAuthNeedNanos=%d, bounds exact=%d", needFromAuth, exactNanos)
	}
	if ceilNanosToMicros(needFromAuth) != hold {
		t.Fatalf("hold from auth need=%d, want %d", ceilNanosToMicros(needFromAuth), hold)
	}

	// Envelope create holds CapNanos via the same evaluateRealtimeBuyerFunding
	// ceiling. Seed two buyers with exactly `hold` micros.
	nonEnvBuyer := uuid.New()
	envBuyer := uuid.New()
	for _, id := range []uuid.UUID{nonEnvBuyer, envBuyer} {
		if _, err := pool.Exec(ctx, `INSERT INTO buyers (id,email,free_credit_usd) VALUES ($1,$2,0)`,
			id, id.String()+"@path-parity.invalid"); err != nil {
			t.Fatal(err)
		}
		must(t, store.SeedPrepaidBalance(ctx, id, hold, "seed-parity-"+id.String()))
	}

	// Non-envelope: one successful authorize consumes the full hold.
	auth.BuyerID = nonEnvBuyer
	auth.RequestID = "req-parity-nonenv-" + uuid.NewString()
	_, _, err = store.AuthorizeRealtimeContract(ctx, auth)
	mustf(t, err, "non-envelope authorize: %v")

	// A second non-envelope authorize against the same hold budget must refuse
	// (first contract still EXECUTING and reserved at the ceiled amount).
	auth.RequestID = "req-parity-nonenv-2-" + uuid.NewString()
	auth.InputCommitment = strings.Repeat("1", 64)
	auth.RequestSHA256 = strings.Repeat("2", 64)
	_, _, err = store.AuthorizeRealtimeContract(ctx, auth)
	if !errors.Is(err, errRealtimeInsufficientFunds) && !errors.Is(err, errRealtimeTopupRequired) {
		t.Fatalf("second non-envelope authorize err=%v, want insufficient after full hold", err)
	}

	// Envelope path: create with CapNanos == exact need holds the same micros.
	env, err := store.CreateExecutionEnvelope(ctx, envBuyer, ExecutionEnvelopeCreateRequest{
		RuntimeProfileID:       profile.RuntimeProfileID,
		CapNanos:               exactNanos,
		MaxRequests:            1,
		MaxTokens:              maxP + maxC,
		PerRequestCeilingNanos: exactNanos,
		TTLSeconds:             3600,
	})
	mustf(t, err, "envelope create: %v")
	if env.CapNanos != exactNanos {
		t.Fatalf("envelope cap=%d, want exact need %d", env.CapNanos, exactNanos)
	}

	// Same-sized second envelope must refuse — hold already equals prepaid.
	_, err = store.CreateExecutionEnvelope(ctx, envBuyer, ExecutionEnvelopeCreateRequest{
		RuntimeProfileID:       profile.RuntimeProfileID,
		CapNanos:               exactNanos,
		MaxRequests:            1,
		MaxTokens:              maxP + maxC,
		PerRequestCeilingNanos: exactNanos,
		TTLSeconds:             3600,
	})
	if !errors.Is(err, errRealtimeInsufficientFunds) && !errors.Is(err, errRealtimeTopupRequired) {
		t.Fatalf("second envelope create err=%v, want insufficient (same hold as non-envelope)", err)
	}

	// Direct funding check: both paths feed the same needNanos into evaluate.
	tx, err := pool.Begin(ctx)
	must(t, err)
	// Fresh buyer with hold-1 micros: both need shapes refuse.
	shortBuyer := uuid.New()
	if _, err := pool.Exec(ctx, `INSERT INTO buyers (id,email,free_credit_usd) VALUES ($1,$2,0)`,
		shortBuyer, shortBuyer.String()+"@path-parity-short.invalid"); err != nil {
		t.Fatal(err)
	}
	must(t, store.SeedPrepaidBalance(ctx, shortBuyer, hold-1, "seed-parity-short-"+shortBuyer.String()))
	_ = tx.Rollback(ctx)

	tx, err = pool.Begin(ctx)
	must(t, err)
	errNonEnv := evaluateRealtimeBuyerFunding(ctx, tx, shortBuyer, needFromAuth)
	_ = tx.Rollback(ctx)
	tx, err = pool.Begin(ctx)
	must(t, err)
	errEnv := evaluateRealtimeBuyerFunding(ctx, tx, shortBuyer, exactNanos)
	_ = tx.Rollback(ctx)
	if !errors.Is(errNonEnv, errRealtimeInsufficientFunds) && !errors.Is(errNonEnv, errRealtimeTopupRequired) {
		t.Fatalf("non-envelope need funding err=%v", errNonEnv)
	}
	if !errors.Is(errEnv, errRealtimeInsufficientFunds) && !errors.Is(errEnv, errRealtimeTopupRequired) {
		t.Fatalf("envelope need funding err=%v", errEnv)
	}
	if !errors.Is(errNonEnv, errEnv) && errNonEnv.Error() != errEnv.Error() {
		// Same sentinel preferred; if wrapped, both must still be funding refusals.
		t.Logf("funding errors differ only by wrap: nonenv=%v env=%v", errNonEnv, errEnv)
	}
}

// TestRealtimeSettlementConservationExactNanos keeps the Step 5 conservation
// gate: buyer charge nanos equal entitlements + known costs (spread lives in
// the known-cost contribution on the realtime settlement row).
func TestRealtimeSettlementConservationExactNanos(t *testing.T) {
	installSettlementCurrencyForTest(t, "usd")
	ctx, store, pool := openIsolatedTestStore(t)
	profile, _, _ := realtimeFundingFixture(t, ctx, store, pool)

	buyerID := uuid.New()
	if _, err := pool.Exec(ctx, `INSERT INTO buyers (id,email,free_credit_usd) VALUES ($1,$2,0)`,
		buyerID, buyerID.String()+"@conserve.invalid"); err != nil {
		t.Fatal(err)
	}
	// Generous prepaid so funding is not the subject under test.
	must(t, store.SeedPrepaidBalance(ctx, buyerID, 50_000_000, "seed-conserve-"+buyerID.String()))
	maxUSD, estUSD, maxPrompt, maxCompletion := realtimeAuthCeiling(t, profile, 7, 2)
	contract, _, err := store.AuthorizeRealtimeContract(ctx, RealtimeContractAuthorization{
		RequestID: "req-conserve-" + uuid.NewString(), BuyerID: buyerID, Profile: profile,
		InputCommitment: strings.Repeat("c", 64), RequestSHA256: strings.Repeat("d", 64),
		MaximumPriceUSD: maxUSD, EstimatedPriceUSD: estUSD, DeadlineAt: time.Now().Add(time.Minute),
		MaximumPromptTokens: maxPrompt, MaximumCompletionTokens: maxCompletion,
		EstimatedPromptTokens: 7, EstimatedCompletionTokens: 2,
	})
	mustf(t, err, "authorize: %v")

	settlement, err := store.FinalizeRealtimeSuccess(ctx, contract.ID, RealtimeExecutionEvidence{
		ID: uuid.New(), HTTPStatus: 200, StreamRootSHA256: strings.Repeat("1", 64),
		OutputCommitment: strings.Repeat("2", 64), PromptTokens: 7, CompletionTokens: 2, TotalTokens: 9,
	})
	mustf(t, err, "finalize: %v")
	if settlement.BuyerChargeNanos <= 0 {
		t.Fatalf("buyer charge nanos not recorded: %+v", settlement)
	}
	// realtime_settlements constraint: buyer = supplier + known_cost_contribution.
	if settlement.BuyerChargeNanos != settlement.SupplierPayableNanos+settlement.KnownCostContributionNanos {
		t.Fatalf("conservation broken: buyer=%d supplier=%d known_cost=%d",
			settlement.BuyerChargeNanos, settlement.SupplierPayableNanos, settlement.KnownCostContributionNanos)
	}
	if settlement.BuyerChargeNanos < settlement.SupplierPayableNanos {
		t.Fatalf("buyer charge %d below supplier entitlement %d",
			settlement.BuyerChargeNanos, settlement.SupplierPayableNanos)
	}
}
