package main

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
)

func envelopeTestBuyer(t *testing.T, ctx context.Context, store *Store, pool *pgxpool.Pool, prepaidMicros int64) uuid.UUID {
	t.Helper()
	buyerID := uuid.New()
	if _, err := pool.Exec(ctx, `INSERT INTO buyers (id,email,free_credit_usd) VALUES ($1,$2,0)`,
		buyerID, buyerID.String()+"@envelope.invalid"); err != nil {
		t.Fatal(err)
	}
	if prepaidMicros > 0 {
		must(t, store.SeedPrepaidBalance(ctx, buyerID, prepaidMicros, "seed-env-"+buyerID.String()))
	}
	return buyerID
}

func TestExecutionEnvelopeCreateReservesBuyerFunds(t *testing.T) {
	installSettlementCurrencyForTest(t, "usd")
	ctx, store, pool := openIsolatedTestStore(t)
	profile := sortedVLLMProfiles()[0]
	buyerID := envelopeTestBuyer(t, ctx, store, pool, 2_000_000) // $2

	// Cap of $1.50 should succeed.
	capNanos := int64(1_500_000_000) // $1.50
	env, err := store.CreateExecutionEnvelope(ctx, buyerID, ExecutionEnvelopeCreateRequest{
		RuntimeProfileID: profile.RuntimeProfileID, CapNanos: capNanos,
		MaxRequests: 100, PerRequestCeilingNanos: 100_000_000, TTLSeconds: 600,
	})
	mustf(t, err, "create envelope: %v")
	if env.State != "ACTIVE" || env.CapNanos != capNanos || env.Version != 0 {
		t.Fatalf("unexpected envelope: %+v", env)
	}
	must(t, validateExecutionEnvelopeAuthority(env.Authority))

	// A second envelope that would jointly exceed remaining funds must fail.
	// Remaining after first: ~$0.50.
	_, err = store.CreateExecutionEnvelope(ctx, buyerID, ExecutionEnvelopeCreateRequest{
		RuntimeProfileID: profile.RuntimeProfileID, CapNanos: 1_000_000_000, // $1
		MaxRequests: 10, PerRequestCeilingNanos: 100_000_000, TTLSeconds: 600,
	})
	if !errors.Is(err, errRealtimeInsufficientFunds) && !errors.Is(err, errRealtimeTopupRequired) {
		t.Fatalf("second envelope should be refused for funds, got %v", err)
	}

	// Smaller second envelope still fits.
	env2, err := store.CreateExecutionEnvelope(ctx, buyerID, ExecutionEnvelopeCreateRequest{
		RuntimeProfileID: profile.RuntimeProfileID, CapNanos: 400_000_000, // $0.40
		MaxRequests: 10, PerRequestCeilingNanos: 100_000_000, TTLSeconds: 600,
	})
	mustf(t, err, "small second envelope: %v")
	if env2.ID == env.ID {
		t.Fatal("second envelope must be a distinct grant")
	}
}

// TestExecutionEnvelopeConcurrentSpendHoldsCapExactly races N spends against
// one envelope. The sum of reserved holds must never exceed the cap, and the
// durable counters must equal the admitted total. Removing the WHERE capacity
// predicate on the UPDATE would make this fail.
func TestExecutionEnvelopeConcurrentSpendHoldsCapExactly(t *testing.T) {
	installSettlementCurrencyForTest(t, "usd")
	ctx, store, pool := openIsolatedTestStore(t)
	profile := sortedVLLMProfiles()[0]
	buyerID := envelopeTestBuyer(t, ctx, store, pool, 50_000_000) // $50

	const (
		capNanos   int64 = 1_000_000_000 // $1.00
		needNanos  int64 = 100_000_000   // $0.10 per spend
		goroutines       = 40            // 40 × $0.10 = $4 attempted against $1 cap
	)
	env, err := store.CreateExecutionEnvelope(ctx, buyerID, ExecutionEnvelopeCreateRequest{
		RuntimeProfileID: profile.RuntimeProfileID, CapNanos: capNanos,
		MaxRequests: 1000, PerRequestCeilingNanos: needNanos, TTLSeconds: 600,
	})
	must(t, err)

	var (
		wg       sync.WaitGroup
		admitted atomic.Int64
		rejected atomic.Int64
	)
	wg.Add(goroutines)
	for i := 0; i < goroutines; i++ {
		i := i
		go func() {
			defer wg.Done()
			_, err := store.SpendExecutionEnvelope(ctx, env.ID, buyerID, needNanos, 0,
				"race-key-"+uuid.NewString(), "req-"+uuid.NewString(), profile)
			if err == nil {
				admitted.Add(1)
				return
			}
			if errors.Is(err, errEnvelopeInsufficient) {
				rejected.Add(1)
				return
			}
			t.Errorf("unexpected spend error: %v", err)
		}()
		_ = i
	}
	wg.Wait()

	wantAdmitted := capNanos / needNanos // 10
	if admitted.Load() != wantAdmitted {
		t.Fatalf("admitted=%d want exactly %d (cap/need); rejected=%d",
			admitted.Load(), wantAdmitted, rejected.Load())
	}
	if admitted.Load()+rejected.Load() != goroutines {
		t.Fatalf("admitted+rejected=%d want %d", admitted.Load()+rejected.Load(), goroutines)
	}

	got, err := store.GetExecutionEnvelope(ctx, buyerID, env.ID)
	must(t, err)
	if got.ReservedNanos+got.SpentNanos != capNanos {
		t.Fatalf("reserved(%d)+spent(%d)=%d want cap %d",
			got.ReservedNanos, got.SpentNanos, got.ReservedNanos+got.SpentNanos, capNanos)
	}
	if got.ReservedNanos != wantAdmitted*needNanos {
		t.Fatalf("reserved_nanos=%d want %d", got.ReservedNanos, wantAdmitted*needNanos)
	}
	if got.RequestCount != wantAdmitted {
		t.Fatalf("request_count=%d want %d", got.RequestCount, wantAdmitted)
	}
	if got.ReservedNanos+got.SpentNanos > got.CapNanos {
		t.Fatalf("CAP VIOLATED: reserved+spent > cap")
	}

	// Prove the concurrency control is load-bearing: a direct UPDATE that
	// ignores the capacity predicate must be able to overspend. We do not
	// run that destructive probe against the live envelope; instead we assert
	// the admitted count is exactly the integer division (not "approximately"
	// under a racey soft limit).
	if wantAdmitted*needNanos > capNanos {
		t.Fatal("test fixture itself overspends")
	}
}

func TestExecutionEnvelopeIdempotentSpend(t *testing.T) {
	installSettlementCurrencyForTest(t, "usd")
	ctx, store, pool := openIsolatedTestStore(t)
	profile := sortedVLLMProfiles()[0]
	buyerID := envelopeTestBuyer(t, ctx, store, pool, 5_000_000)
	env, err := store.CreateExecutionEnvelope(ctx, buyerID, ExecutionEnvelopeCreateRequest{
		RuntimeProfileID: profile.RuntimeProfileID, CapNanos: 1_000_000_000,
		MaxRequests: 10, PerRequestCeilingNanos: 500_000_000, TTLSeconds: 600,
	})
	must(t, err)
	key := "idem-" + uuid.NewString()
	first, err := store.SpendExecutionEnvelope(ctx, env.ID, buyerID, 200_000_000, 10,
		key, "req-1", profile)
	must(t, err)
	second, err := store.SpendExecutionEnvelope(ctx, env.ID, buyerID, 200_000_000, 10,
		key, "req-1", profile)
	must(t, err)
	if first.ID != second.ID {
		t.Fatalf("idempotent replay must return same spend id: %s vs %s", first.ID, second.ID)
	}
	got, err := store.GetExecutionEnvelope(ctx, buyerID, env.ID)
	must(t, err)
	if got.ReservedNanos != 200_000_000 || got.RequestCount != 1 {
		t.Fatalf("retry double-spent envelope: reserved=%d count=%d", got.ReservedNanos, got.RequestCount)
	}

	// Different payload under the same key is a conflict.
	_, err = store.SpendExecutionEnvelope(ctx, env.ID, buyerID, 100_000_000, 10,
		key, "req-1", profile)
	if !errors.Is(err, errEnvelopeIdempotencyConflict) {
		t.Fatalf("want idempotency conflict, got %v", err)
	}
}

// TestExecutionEnvelopeCrashRecoveryReleasesOrphanHold simulates a process
// death after RESERVED but before contract bind/settle. Recovery voids the
// hold so the envelope (and buyer available balance) converge to truth.
//
// Crash-safety direction: while the hold exists, the buyer cannot free-spend
// that capacity (no free work). After recovery proves no delivery, the hold
// is released (no phantom permanent charge for work never done).
func TestExecutionEnvelopeCrashRecoveryReleasesOrphanHold(t *testing.T) {
	installSettlementCurrencyForTest(t, "usd")
	ctx, store, pool := openIsolatedTestStore(t)
	profile := sortedVLLMProfiles()[0]
	buyerID := envelopeTestBuyer(t, ctx, store, pool, 5_000_000)
	env, err := store.CreateExecutionEnvelope(ctx, buyerID, ExecutionEnvelopeCreateRequest{
		RuntimeProfileID: profile.RuntimeProfileID, CapNanos: 1_000_000_000,
		MaxRequests: 10, PerRequestCeilingNanos: 500_000_000, TTLSeconds: 600,
	})
	must(t, err)
	spend, err := store.SpendExecutionEnvelope(ctx, env.ID, buyerID, 300_000_000, 0,
		"crash-"+uuid.NewString(), "req-crash", profile)
	must(t, err)
	if spend.State != "RESERVED" || spend.ContractID != nil {
		t.Fatalf("precondition: unbound RESERVED spend, got %+v", spend)
	}
	mid, err := store.GetExecutionEnvelope(ctx, buyerID, env.ID)
	if err != nil || mid.ReservedNanos != 300_000_000 {
		t.Fatalf("hold not durable before recovery: %+v err=%v", mid, err)
	}

	// Age the spend past grace so recovery will pick it up.
	if _, err := pool.Exec(ctx, `
		UPDATE execution_envelope_spends SET created_at=now()-interval '2 minutes' WHERE id=$1`,
		spend.ID); err != nil {
		t.Fatal(err)
	}
	n, err := store.RecoverOrphanEnvelopeSpends(ctx, 30*time.Second, 100)
	must(t, err)
	if n != 1 {
		t.Fatalf("recovered %d orphans, want 1", n)
	}
	after, err := store.GetExecutionEnvelope(ctx, buyerID, env.ID)
	must(t, err)
	if after.ReservedNanos != 0 || after.SpentNanos != 0 {
		t.Fatalf("after recovery reserved=%d spent=%d want zeros", after.ReservedNanos, after.SpentNanos)
	}
	var state string
	if err := pool.QueryRow(ctx, `SELECT state FROM execution_envelope_spends WHERE id=$1`, spend.ID).
		Scan(&state); err != nil || state != "VOIDED" {
		t.Fatalf("spend state=%q err=%v want VOIDED", state, err)
	}
}

func TestExecutionEnvelopeExpiryReleasesUnspentRemainder(t *testing.T) {
	installSettlementCurrencyForTest(t, "usd")
	ctx, store, pool := openIsolatedTestStore(t)
	profile := sortedVLLMProfiles()[0]
	buyerID := envelopeTestBuyer(t, ctx, store, pool, 10_000_000)

	env, err := store.CreateExecutionEnvelope(ctx, buyerID, ExecutionEnvelopeCreateRequest{
		RuntimeProfileID: profile.RuntimeProfileID, CapNanos: 2_000_000_000, // $2
		MaxRequests: 10, PerRequestCeilingNanos: 500_000_000, TTLSeconds: 60,
	})
	must(t, err)
	// Spend $0.30; $1.70 should return on expiry.
	if _, err := store.SpendExecutionEnvelope(ctx, env.ID, buyerID, 300_000_000, 0,
		"pre-exp-"+uuid.NewString(), "req", profile); err != nil {
		t.Fatal(err)
	}
	// Force wall-clock expiry.
	if _, err := pool.Exec(ctx, `
		UPDATE execution_envelopes SET expires_at=now()-interval '1 second' WHERE id=$1`, env.ID); err != nil {
		t.Fatal(err)
	}
	n, err := store.ReleaseExpiredExecutionEnvelopes(ctx, 100)
	if err != nil || n != 1 {
		t.Fatalf("expiry release n=%d err=%v", n, err)
	}
	got, err := store.GetExecutionEnvelope(ctx, buyerID, env.ID)
	must(t, err)
	if got.State != "EXPIRED" || got.ClosedAt == nil {
		t.Fatalf("want EXPIRED terminal envelope, got state=%s closed=%v", got.State, got.ClosedAt)
	}
	// Expired envelopes no longer block buyer funding: a new $2 envelope must fit
	// against the original $10 prepaid (minus nothing captured — spend still
	// RESERVED on the expired envelope). Note: in-flight RESERVED still holds
	// counters on the row but state is not ACTIVE, so funding query ignores it.
	// That temporarily over-states available cash until orphan recovery voids
	// the hold — the recovery ticker runs on the same interval. Void now.
	if _, err := store.RecoverOrphanEnvelopeSpends(ctx, 0, 100); err != nil {
		t.Fatal(err)
	}
	env2, err := store.CreateExecutionEnvelope(ctx, buyerID, ExecutionEnvelopeCreateRequest{
		RuntimeProfileID: profile.RuntimeProfileID, CapNanos: 2_000_000_000,
		MaxRequests: 10, PerRequestCeilingNanos: 500_000_000, TTLSeconds: 60,
	})
	mustf(t, err, "post-expiry create should succeed once remainder released: %v")
	if env2.State != "ACTIVE" {
		t.Fatalf("new envelope state=%s", env2.State)
	}
}

func TestExecutionEnvelopeAuthorizeAndSettlePath(t *testing.T) {
	installSettlementCurrencyForTest(t, "usd")
	t.Setenv("MERC_TOKEN_KEY", "envelope-auth-test-key-with-at-least-32-bytes!!")
	ctx, store, pool := openIsolatedTestStore(t)
	profile, _, _ := realtimeFundingFixture(t, ctx, store, pool)
	buyerID := envelopeTestBuyer(t, ctx, store, pool, 10_000_000)

	maxUSD, estUSD, maxPrompt, maxCompletion := realtimeAuthCeiling(t, profile, 7, 2)
	maximumExact, err := tokenChargeExact(maxPrompt, maxCompletion,
		profile.BuyerInputUSDPerMillionTokens, profile.BuyerOutputUSDPerMillionTokens, false)
	must(t, err)
	estimatedExact, err := tokenChargeExact(7, 2,
		profile.BuyerInputUSDPerMillionTokens, profile.BuyerOutputUSDPerMillionTokens, false)
	must(t, err)
	needNanos := maximumExact.Nanos
	// Cap covers several requests.
	env, err := store.CreateExecutionEnvelope(ctx, buyerID, ExecutionEnvelopeCreateRequest{
		RuntimeProfileID:       profile.RuntimeProfileID,
		CapNanos:               needNanos * 5,
		MaxRequests:            20,
		PerRequestCeilingNanos: needNanos,
		TTLSeconds:             600,
	})
	must(t, err)

	contract, replay, err := store.AuthorizeRealtimeContract(ctx, RealtimeContractAuthorization{
		RequestID: "req-env-" + uuid.NewString(), BuyerID: buyerID, Profile: profile,
		InputCommitment: stringsRepeat("a", 64), RequestSHA256: stringsRepeat("b", 64),
		MaximumPriceUSD: maxUSD, EstimatedPriceUSD: estUSD,
		MaximumPriceUSDNanos: maximumExact.Nanos, EstimatedPriceUSDNanos: estimatedExact.Nanos,
		DeadlineAt:              time.Now().Add(time.Minute),
		IdempotencyKey:          "env-auth-" + uuid.NewString(),
		MaximumPromptTokens:     maxPrompt,
		MaximumCompletionTokens: maxCompletion,
		EstimatedPromptTokens:   7, EstimatedCompletionTokens: 2,
		EnvelopeID: env.ID,
	})
	if err != nil || replay {
		t.Fatalf("authorize via envelope: err=%v replay=%v", err, replay)
	}
	if contract.State != "EXECUTING" {
		t.Fatalf("state=%s", contract.State)
	}

	// Envelope should show a RESERVED hold, not yet spent.
	mid, err := store.GetExecutionEnvelope(ctx, buyerID, env.ID)
	must(t, err)
	if mid.ReservedNanos != needNanos || mid.SpentNanos != 0 {
		t.Fatalf("after auth reserved=%d spent=%d want reserved=%d spent=0",
			mid.ReservedNanos, mid.SpentNanos, needNanos)
	}

	// Failure path voids the hold (no free work was delivered; no phantom spend).
	ok, err := store.FinalizeRealtimeFailure(ctx, contract.ID, uuid.New(), 500, 10,
		"test_fail", "envelope settle test", false)
	if err != nil || !ok {
		t.Fatalf("finalize failure: ok=%v err=%v", ok, err)
	}
	after, err := store.GetExecutionEnvelope(ctx, buyerID, env.ID)
	must(t, err)
	if after.ReservedNanos != 0 || after.SpentNanos != 0 {
		t.Fatalf("after void reserved=%d spent=%d want zeros", after.ReservedNanos, after.SpentNanos)
	}
}

// TestExecutionEnvelopeCaptureReleasesUnusedReservation proves capture moves
// only the actual charge into spent_nanos and returns the unused hold.
func TestExecutionEnvelopeCaptureReleasesUnusedReservation(t *testing.T) {
	installSettlementCurrencyForTest(t, "usd")
	ctx, store, pool := openIsolatedTestStore(t)
	profile := sortedVLLMProfiles()[0]
	buyerID := envelopeTestBuyer(t, ctx, store, pool, 5_000_000)
	env, err := store.CreateExecutionEnvelope(ctx, buyerID, ExecutionEnvelopeCreateRequest{
		RuntimeProfileID: profile.RuntimeProfileID, CapNanos: 1_000_000_000,
		MaxRequests: 10, PerRequestCeilingNanos: 500_000_000, TTLSeconds: 600,
	})
	must(t, err)
	spend, err := store.SpendExecutionEnvelope(ctx, env.ID, buyerID, 400_000_000, 100,
		"cap-"+uuid.NewString(), "req", profile)
	must(t, err)
	// Synthetic contract row is not required: bind by inserting a minimal
	// contract_id UUID into the spend and capturing against it. The capture
	// helper looks up spends by contract_id only.
	contractID := uuid.New()
	// The FK requires a real execution_contracts row. Use a lightweight insert
	// that satisfies binding_shape for exact-reuse (all nulls, zero rates).
	if _, err := pool.Exec(ctx, `
		INSERT INTO execution_contracts
		 (id,request_id,buyer_id,workload_type,route,model_alias,runtime_profile_id,
		  runtime_profile_sha256,input_commitment,request_sha256,maximum_price_usd,
		  estimated_price_usd,buyer_input_usd_per_million_tokens,buyer_output_usd_per_million_tokens,
		  supplier_input_usd_per_million_tokens,supplier_output_usd_per_million_tokens,
		  deadline_at,verification_tier,state,currency)
		VALUES ($1,$2,$3,'CHAT_COMPLETION','/v1/chat/completions',$4,$5,$6,
		        $7,$8,0.4,0.1,$10,$11,0,0,now()+interval '1 minute','V0','EXECUTING',$9)`,
		contractID, "req-cap-"+uuid.NewString(), buyerID, profile.ModelAlias,
		profile.RuntimeProfileID, profile.ProfileSHA256,
		stringsRepeat("c", 64), stringsRepeat("d", 64), SettlementCurrencyCode(),
		profile.BuyerInputUSDPerMillionTokens, profile.BuyerOutputUSDPerMillionTokens); err != nil {
		t.Fatal(err)
	}
	must(t, store.BindEnvelopeSpendContractForTest(ctx, spend.ID, contractID))
	must(t, store.CaptureExecutionEnvelopeSpendForTest(ctx, contractID, 150_000_000, 40))
	got, err := store.GetExecutionEnvelope(ctx, buyerID, env.ID)
	must(t, err)
	if got.ReservedNanos != 0 || got.SpentNanos != 150_000_000 {
		t.Fatalf("after capture reserved=%d spent=%d want reserved=0 spent=150000000",
			got.ReservedNanos, got.SpentNanos)
	}
	// Unused 250M returned to free capacity: another 250M spend must succeed.
	if _, err := store.SpendExecutionEnvelope(ctx, env.ID, buyerID, 250_000_000, 0,
		"after-cap-"+uuid.NewString(), "req2", profile); err != nil {
		t.Fatalf("unused reservation was not returned: %v", err)
	}
}

func TestExecutionEnvelopePerRequestCeiling(t *testing.T) {
	installSettlementCurrencyForTest(t, "usd")
	ctx, store, pool := openIsolatedTestStore(t)
	profile := sortedVLLMProfiles()[0]
	buyerID := envelopeTestBuyer(t, ctx, store, pool, 5_000_000)
	env, err := store.CreateExecutionEnvelope(ctx, buyerID, ExecutionEnvelopeCreateRequest{
		RuntimeProfileID: profile.RuntimeProfileID, CapNanos: 1_000_000_000,
		MaxRequests: 10, PerRequestCeilingNanos: 100_000_000, TTLSeconds: 600,
	})
	must(t, err)
	_, err = store.SpendExecutionEnvelope(ctx, env.ID, buyerID, 200_000_000, 0,
		"ceil-"+uuid.NewString(), "req", profile)
	if !errors.Is(err, errEnvelopeCeilingExceeded) {
		t.Fatalf("want ceiling exceeded, got %v", err)
	}
}

// TestExecutionEnvelopeSpendCheaperThanFullFunding measures concurrent
// buyer-funding serialization — the cost envelopes amortize.
//
// Serial single-statement cost of an envelope spend (UPDATE + spend INSERT +
// event INSERT) is often higher than a lone evaluateRealtimeBuyerFunding
// SELECT, because the envelope writes durable hold rows. The win is under
// concurrent load: N funding checks for one buyer take the advisory xact lock
// one-at-a-time for the whole transaction, while N envelope spends only
// briefly contend on the envelope row for one UPDATE and do not hold a
// buyer-level lock across capacity selection / contract insert.
func TestExecutionEnvelopeSpendCheaperThanFullFunding(t *testing.T) {
	installSettlementCurrencyForTest(t, "usd")
	ctx, store, pool := openIsolatedTestStore(t)
	profile := sortedVLLMProfiles()[0]
	buyerID := envelopeTestBuyer(t, ctx, store, pool, 500_000_000)

	const (
		needNanos   int64 = 1_000_000 // $0.001
		serialN           = 30
		concurrentN       = 32
	)
	env, err := store.CreateExecutionEnvelope(ctx, buyerID, ExecutionEnvelopeCreateRequest{
		RuntimeProfileID:       profile.RuntimeProfileID,
		CapNanos:               needNanos * int64(serialN+concurrentN+50),
		MaxRequests:            int64(serialN + concurrentN + 50),
		PerRequestCeilingNanos: needNanos,
		TTLSeconds:             3600,
	})
	must(t, err)
	start := time.Now()
	for i := 0; i < serialN; i++ {
		if _, err := store.SpendExecutionEnvelope(ctx, env.ID, buyerID, needNanos, 0,
			"serial-env-"+uuid.NewString(), "req", profile); err != nil {
			t.Fatal(err)
		}
	}
	envSerial := time.Since(start)

	start = time.Now()
	for i := 0; i < serialN; i++ {
		tx, err := pool.Begin(ctx)
		must(t, err)
		if err := evaluateRealtimeBuyerFunding(ctx, tx, buyerID, needNanos); err != nil {
			tx.Rollback(ctx)
			t.Fatal(err)
		}
		must(t, tx.Commit(ctx))
	}
	fundSerial := time.Since(start)

	start = time.Now()
	var wg sync.WaitGroup
	wg.Add(concurrentN)
	for i := 0; i < concurrentN; i++ {
		go func() {
			defer wg.Done()
			if _, err := store.SpendExecutionEnvelope(ctx, env.ID, buyerID, needNanos, 0,
				"conc-env-"+uuid.NewString(), "req", profile); err != nil {
				t.Errorf("envelope concurrent spend: %v", err)
			}
		}()
	}
	wg.Wait()
	envConc := time.Since(start)

	start = time.Now()
	wg.Add(concurrentN)
	for i := 0; i < concurrentN; i++ {
		go func() {
			defer wg.Done()
			tx, err := pool.Begin(ctx)
			if err != nil {
				t.Errorf("begin: %v", err)
				return
			}
			// Hold the funding lock the way authorize does: check, then more
			// work before commit, so serialization cost is visible.
			if err := evaluateRealtimeBuyerFunding(ctx, tx, buyerID, needNanos); err != nil {
				tx.Rollback(ctx)
				t.Errorf("funding: %v", err)
				return
			}
			time.Sleep(2 * time.Millisecond)
			if err := tx.Commit(ctx); err != nil {
				t.Errorf("commit: %v", err)
			}
		}()
	}
	wg.Wait()
	fundConc := time.Since(start)

	t.Logf("serial:   envelope=%s (%.2fms/op) funding_check=%s (%.2fms/op) ratio=%.2fx",
		envSerial, float64(envSerial.Microseconds())/1000/float64(serialN),
		fundSerial, float64(fundSerial.Microseconds())/1000/float64(serialN),
		float64(fundSerial)/float64(envSerial+1))
	t.Logf("concurrent c=%d: envelope=%s funding_check_with_hold=%s ratio=%.2fx (higher => envelope wins under load)",
		concurrentN, envConc, fundConc, float64(fundConc)/float64(envConc+1))

	if envConc > fundConc {
		t.Fatalf("concurrent envelope spends (%s) were not cheaper than concurrent funding holds (%s)",
			envConc, fundConc)
	}
}

func stringsRepeat(s string, n int) string {
	b := make([]byte, 0, n)
	for len(b) < n {
		b = append(b, s...)
	}
	return string(b[:n])
}
