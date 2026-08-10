package main

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

// G061 — prepaid batch admission must share the buyer-funding advisory used by
// realtime and free-credit. Without it, prepaid only locks buyer_prepaid_balances
// while realtime locks the advisory + buyers and reads the balance as a plain
// subquery, so the two rails can check/check/commit/commit and oversubscribe.
//
// These tests fail on the unmodified tree and pass once reservePrepaidForJobTx
// takes pg_advisory_xact_lock("realtime-buyer-funding|"+buyerID) first.

// g061OversubscribeFixture picks a realtime ceiling R2 and a prepaid reserved
// R1 such that R1<=B, R2<=B, R1+R2>B for a shared prepaid balance B.
func g061OversubscribeFixture(t *testing.T, profile VLLMRuntimeProfile) (
	balanceMicros, prepaidNeedMicros, realtimeHoldMicros int64,
	maxUSD, estUSD float64, maxPrompt, maxCompletion int64,
) {
	t.Helper()
	// Large token ceiling so R2 is material (~$0.07 on current profiles).
	maxUSD, estUSD, maxPrompt, maxCompletion = realtimeAuthCeiling(t, profile, 100_000, 50_000)
	realtimeHoldMicros = usdToMicros(maxUSD)
	if realtimeHoldMicros <= 0 {
		t.Fatalf("realtime ceiling micros=%d (maxUSD=%f)", realtimeHoldMicros, maxUSD)
	}
	// Match prepaid need to the realtime hold so the oversubscribe window is simple.
	prepaidNeedMicros = realtimeHoldMicros
	// B covers each alone but not both: B = 1.5 * R - 1  ⇒  R <= B < 2R.
	balanceMicros = prepaidNeedMicros + realtimeHoldMicros/2
	if balanceMicros < prepaidNeedMicros {
		balanceMicros = prepaidNeedMicros
	}
	if prepaidNeedMicros+realtimeHoldMicros <= balanceMicros {
		balanceMicros = prepaidNeedMicros + realtimeHoldMicros - 1
	}
	if prepaidNeedMicros > balanceMicros || realtimeHoldMicros > balanceMicros {
		t.Fatalf("cannot place oversubscribe window: R1=%d R2=%d B=%d",
			prepaidNeedMicros, realtimeHoldMicros, balanceMicros)
	}
	if prepaidNeedMicros+realtimeHoldMicros <= balanceMicros {
		t.Fatalf("R1+R2 does not exceed B: R1=%d R2=%d B=%d",
			prepaidNeedMicros, realtimeHoldMicros, balanceMicros)
	}
	return balanceMicros, prepaidNeedMicros, realtimeHoldMicros, maxUSD, estUSD, maxPrompt, maxCompletion
}

func clonePrepaidJobFromTemplate(t *testing.T, f moneyPathFixture, template *jobRow) (*jobRow, []taskRow) {
	t.Helper()
	fx := f
	fx.JobID = uuid.New()
	fx.TaskIDs = []uuid.UUID{uuid.New(), uuid.New()}
	tasks := makeTasks(fx, 2)
	job := validJobRow(t, fx, tasks)
	job.EconomicPlan = template.EconomicPlan
	job.EstimatedUSD = template.EstimatedUSD
	job.SLAPremiumUSD = template.SLAPremiumUSD
	job.PlacementRequirement = template.PlacementRequirement
	job.PricingDecision = template.PricingDecision
	job.HWClasses = append([]string(nil), template.HWClasses...)
	job.MinMemoryGB = template.MinMemoryGB
	job.OfferedRateUsdHr = template.OfferedRateUsdHr
	job.WorkloadDecision = template.WorkloadDecision
	job.ComputePlan = template.ComputePlan
	job.EconomicInputSource = template.EconomicInputSource
	job.PrepaidRequired = true
	return job, tasks
}

// TestPrepaidRealtimeFundingAdvisory_PrepaidFirst is the load-bearing defect:
// open a prepaid funding check (job hold not yet committed), then run a
// realtime authorize for the same buyer. Today both succeed; after the shared
// advisory, exactly one does.
func TestPrepaidRealtimeFundingAdvisory_PrepaidFirst(t *testing.T) {
	installSettlementCurrencyForTest(t, "usd")
	ctx, store, pool := openIsolatedTestStore(t)
	profile, _, _ := realtimeFundingFixture(t, ctx, store, pool)

	balanceMicros, prepaidNeed, _, maxUSD, estUSD, maxPrompt, maxCompletion :=
		g061OversubscribeFixture(t, profile)

	buyerID := uuid.New()
	if _, err := pool.Exec(ctx, `INSERT INTO buyers (id,email,free_credit_usd) VALUES ($1,$2,0)`,
		buyerID, buyerID.String()+"@g061-pp-first.invalid"); err != nil {
		t.Fatal(err)
	}
	must(t, store.SeedPrepaidBalance(ctx, buyerID, balanceMicros, "seed-g061-pp-first-"+buyerID.String()))

	jobID := uuid.New()
	reservedUSD := microsToUSD(prepaidNeed)
	initialUSD := reservedUSD // residual math uses reserved; initial <= reserved

	txPrepaid, err := pool.Begin(ctx)
	must(t, err)
	defer txPrepaid.Rollback(ctx)

	mustf(t, reservePrepaidForJobTx(ctx, txPrepaid, buyerID, prepaidNeed), "prepaid funding check: %v")
	if err := insertPrepaidJobHoldTx(ctx, txPrepaid, jobID, buyerID, initialUSD, reservedUSD); err != nil {
		t.Fatalf("insert prepaid hold rows: %v", err)
	}

	type authResult struct{ err error }
	authDone := make(chan authResult, 1)
	go func() {
		_, _, err := store.AuthorizeRealtimeContract(context.Background(), RealtimeContractAuthorization{
			RequestID: "req-g061-pp-first-" + uuid.NewString(), BuyerID: buyerID, Profile: profile,
			InputCommitment: strings.Repeat("a", 64), RequestSHA256: strings.Repeat("b", 64),
			MaximumPriceUSD: maxUSD, EstimatedPriceUSD: estUSD, DeadlineAt: time.Now().Add(time.Minute),
			MaximumPromptTokens: maxPrompt, MaximumCompletionTokens: maxCompletion,
			EstimatedPromptTokens: maxPrompt / 2, EstimatedCompletionTokens: maxCompletion / 2,
		})
		authDone <- authResult{err: err}
	}()

	var auth authResult
	authReturnedEarly := false
	select {
	case auth = <-authDone:
		authReturnedEarly = true
	case <-time.After(300 * time.Millisecond):
		// Still blocked — expected after the advisory fix.
	}

	mustf(t, txPrepaid.Commit(ctx), "commit prepaid: %v")

	if !authReturnedEarly {
		select {
		case auth = <-authDone:
		case <-time.After(15 * time.Second):
			t.Fatal("AuthorizeRealtimeContract did not return after prepaid commit")
		}
	}

	if auth.err == nil {
		t.Fatalf("prepaid-first interleave: realtime authorize also succeeded "+
			"(R1=%d micros, ceiling=%f, B=%d); both rails admitted — shared buyer-funding advisory missing or not held through prepaid commit",
			prepaidNeed, maxUSD, balanceMicros)
	}
	if !errors.Is(auth.err, errRealtimeInsufficientFunds) && !errors.Is(auth.err, errRealtimeTopupRequired) {
		t.Fatalf("prepaid-first interleave: realtime err=%v, want insufficient/topup", auth.err)
	}
}

// TestPrepaidRealtimeFundingAdvisory_RealtimeFirst is the reverse interleave:
// hold realtime funding open (with an uncommitted EXECUTING hold), then run
// prepaid admission. Today both succeed; after the shared advisory, exactly one does.
func TestPrepaidRealtimeFundingAdvisory_RealtimeFirst(t *testing.T) {
	installSettlementCurrencyForTest(t, "usd")
	ctx, store, pool := openIsolatedTestStore(t)
	profile, _, _ := realtimeFundingFixture(t, ctx, store, pool)

	balanceMicros, prepaidNeed, realtimeHold, maxUSD, estUSD, maxPrompt, maxCompletion :=
		g061OversubscribeFixture(t, profile)

	buyerID := uuid.New()
	if _, err := pool.Exec(ctx, `INSERT INTO buyers (id,email,free_credit_usd) VALUES ($1,$2,0)`,
		buyerID, buyerID.String()+"@g061-rt-first.invalid"); err != nil {
		t.Fatal(err)
	}
	must(t, store.SeedPrepaidBalance(ctx, buyerID, balanceMicros, "seed-g061-rt-first-"+buyerID.String()))

	// Hold realtime funding the same way AuthorizeRealtimeContract does: advisory
	// + buyers FOR UPDATE via evaluateRealtimeBuyerFunding. needNanos is the
	// integer-micro hold expressed as nanos (hold term ceils nanos→micros).
	needNanos := realtimeHold * 1000
	txRT, err := pool.Begin(ctx)
	must(t, err)
	defer txRT.Rollback(ctx)
	mustf(t, evaluateRealtimeBuyerFunding(ctx, txRT, buyerID, needNanos), "realtime funding check: %v")

	contractID := uuid.New()
	if err := insertExecutingRealtimeHoldTx(ctx, txRT, contractID, buyerID, profile, maxUSD, estUSD, maxPrompt, maxCompletion, needNanos); err != nil {
		t.Fatalf("insert EXECUTING hold: %v", err)
	}

	type subResult struct{ err error }
	subDone := make(chan subResult, 1)
	go func() {
		// Production admission gate for prepaid batch (SubmitJobTx calls this).
		tx, err := store.pool.Begin(context.Background())
		if err != nil {
			subDone <- subResult{err: err}
			return
		}
		defer tx.Rollback(context.Background())
		err = reservePrepaidForJobTx(context.Background(), tx, buyerID, prepaidNeed)
		if err != nil {
			subDone <- subResult{err: err}
			return
		}
		// Materialise hold only on success so a refused admit leaves no residual.
		if err := insertPrepaidJobHoldTx(context.Background(), tx, uuid.New(), buyerID,
			microsToUSD(prepaidNeed), microsToUSD(prepaidNeed)); err != nil {
			subDone <- subResult{err: err}
			return
		}
		subDone <- subResult{err: tx.Commit(context.Background())}
	}()

	var sub subResult
	subReturnedEarly := false
	select {
	case sub = <-subDone:
		subReturnedEarly = true
	case <-time.After(300 * time.Millisecond):
		// Still blocked — expected after the advisory fix.
	}

	mustf(t, txRT.Commit(ctx), "commit realtime: %v")

	if !subReturnedEarly {
		select {
		case sub = <-subDone:
		case <-time.After(15 * time.Second):
			t.Fatal("prepaid admission did not return after realtime commit")
		}
	}

	if sub.err == nil {
		t.Fatalf("realtime-first interleave: prepaid admission also succeeded "+
			"(R1=%d micros, R2=%d micros, B=%d); both rails admitted — shared buyer-funding advisory missing, or prepaid admission does not observe the EXECUTING hold after serialization",
			prepaidNeed, realtimeHold, balanceMicros)
	}
	if !errors.Is(sub.err, errInsufficientPrepaid) {
		t.Fatalf("realtime-first interleave: prepaid err=%v, want errInsufficientPrepaid", sub.err)
	}
}

// TestPrepaidRealtimeFundingAdvisory_DifferentBuyersConcurrent proves the lock
// is per-buyer: two different buyers admit concurrently and both succeed.
func TestPrepaidRealtimeFundingAdvisory_DifferentBuyersConcurrent(t *testing.T) {
	installSettlementCurrencyForTest(t, "usd")
	ctx, store, pool := openIsolatedTestStore(t)
	profile, _, _ := realtimeFundingFixture(t, ctx, store, pool)

	balanceMicros, prepaidNeed, _, maxUSD, estUSD, maxPrompt, maxCompletion :=
		g061OversubscribeFixture(t, profile)

	buyerA := uuid.New()
	buyerB := uuid.New()
	for _, id := range []uuid.UUID{buyerA, buyerB} {
		if _, err := pool.Exec(ctx, `INSERT INTO buyers (id,email,free_credit_usd) VALUES ($1,$2,0)`,
			id, id.String()+"@g061-diff.invalid"); err != nil {
			t.Fatal(err)
		}
		must(t, store.SeedPrepaidBalance(ctx, id, balanceMicros, "seed-g061-diff-"+id.String()))
	}

	var (
		wg           sync.WaitGroup
		start        = make(chan struct{})
		ppErr, rtErr error
	)
	wg.Add(2)
	go func() {
		defer wg.Done()
		<-start
		tx, err := store.pool.Begin(context.Background())
		if err != nil {
			ppErr = err
			return
		}
		defer tx.Rollback(context.Background())
		if err := reservePrepaidForJobTx(context.Background(), tx, buyerA, prepaidNeed); err != nil {
			ppErr = err
			return
		}
		if err := insertPrepaidJobHoldTx(context.Background(), tx, uuid.New(), buyerA,
			microsToUSD(prepaidNeed), microsToUSD(prepaidNeed)); err != nil {
			ppErr = err
			return
		}
		ppErr = tx.Commit(context.Background())
	}()
	go func() {
		defer wg.Done()
		<-start
		_, _, rtErr = store.AuthorizeRealtimeContract(context.Background(), RealtimeContractAuthorization{
			RequestID: "req-g061-diff-" + uuid.NewString(), BuyerID: buyerB, Profile: profile,
			InputCommitment: strings.Repeat("c", 64), RequestSHA256: strings.Repeat("d", 64),
			MaximumPriceUSD: maxUSD, EstimatedPriceUSD: estUSD, DeadlineAt: time.Now().Add(time.Minute),
			MaximumPromptTokens: maxPrompt, MaximumCompletionTokens: maxCompletion,
			EstimatedPromptTokens: maxPrompt / 2, EstimatedCompletionTokens: maxCompletion / 2,
		})
	}()
	close(start)
	wg.Wait()
	if ppErr != nil {
		t.Fatalf("buyer A prepaid admit failed: %v", ppErr)
	}
	if rtErr != nil {
		t.Fatalf("buyer B realtime admit failed: %v", rtErr)
	}
}

// TestPrepaidRealtimeFundingAdvisory_UncontendedPrepaid still admits when the
// prepaid path is alone and funds cover the reserved ceiling.
func TestPrepaidRealtimeFundingAdvisory_UncontendedPrepaid(t *testing.T) {
	installSettlementCurrencyForTest(t, "usd")
	ctx, store, pool, f, template, templateTasks, _ := currentUniformMoneyPathJob(t)
	_ = pool
	template.PrepaidRequired = true
	need := usdToMicros(template.EconomicPlan.ReservedBuyerChargeUSD)
	must(t, store.SeedPrepaidBalance(ctx, f.BuyerID, need, "seed-g061-uncontended-"+f.BuyerID.String()))
	mustf(t, store.SubmitJobTx(ctx, template, templateTasks), "uncontended prepaid admit: %v")
}

// insertPrepaidJobHoldTx writes the job + economic plan that prepaidOpenReservation
// counts, inside an open transaction (the durable hold side of admission).
func insertPrepaidJobHoldTx(ctx context.Context, tx pgx.Tx, jobID, buyerID uuid.UUID, initialUSD, reservedUSD float64) error {
	currency := SettlementCurrencyCode()
	if _, err := tx.Exec(ctx, `
		INSERT INTO jobs (id,buyer_id,status,job_type,input_ref,task_count,prepaid_required,estimated_usd,currency)
		VALUES ($1,$2,'queued','embed','g061-pp-hold',1,true,$3,$4)`,
		jobID, buyerID, initialUSD, currency); err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, `
		INSERT INTO job_economic_plans
		  (job_id,plan_version,schedule_version,plan_json,initial_task_count,
		   buyer_charge_per_task_usd,supplier_payout_per_task_usd,
		   initial_buyer_charge_usd,reserved_buyer_charge_usd,currency)
		VALUES ($1,1,'g061',$2::jsonb,1,$3,$4,$3,$5,$6)`,
		jobID,
		fmt.Sprintf(`{"schedule":{"currency":%q}}`, currency),
		initialUSD,
		initialUSD*0.7,
		reservedUSD,
		currency,
	); err != nil {
		return err
	}
	return nil
}

// insertExecutingRealtimeHoldTx materialises a pure non-envelope EXECUTING row
// so sqlOpenNonEnvelopeExecutingCeilingMicros sees the realtime ceiling after
// commit. Omits pricing_decision so the hold term falls back to
// ROUND(maximum_price_usd * 1e6) and skips the pricing-authority trigger path.
// Unbound shape (no worker) keeps supplier rates at 0.
func insertExecutingRealtimeHoldTx(
	ctx context.Context, tx pgx.Tx, contractID, buyerID uuid.UUID,
	profile VLLMRuntimeProfile, maxUSD, estUSD float64, maxPrompt, maxCompletion, needNanos int64,
) error {
	_ = needNanos
	currency := SettlementCurrencyCode()
	_, err := tx.Exec(ctx, `
		INSERT INTO execution_contracts (
			id, request_id, buyer_id, workload_type, route,
			model_alias, runtime_profile_id, runtime_profile_sha256,
			input_commitment, request_sha256,
			maximum_price_usd, estimated_price_usd,
			buyer_input_usd_per_million_tokens, buyer_output_usd_per_million_tokens,
			supplier_input_usd_per_million_tokens, supplier_output_usd_per_million_tokens,
			deadline_at, verification_tier, state, currency,
			maximum_prompt_tokens, maximum_completion_tokens,
			estimated_prompt_tokens, estimated_completion_tokens
		) VALUES (
			$1,$2,$3,'CHAT_COMPLETION','/v1/chat/completions',
			$4,$5,$6,
			$7,$8,
			$9,$10,
			0.1,0.4,
			0,0,
			now() + interval '1 minute','V0','EXECUTING',$11,
			$12,$13,$14,$15
		)`,
		contractID, "req-g061-hold-"+contractID.String(), buyerID,
		profile.ModelAlias, profile.RuntimeProfileID, profile.ProfileSHA256,
		strings.Repeat("e", 64), strings.Repeat("f", 64),
		maxUSD, estUSD, currency,
		maxPrompt, maxCompletion, maxPrompt/2, maxCompletion/2,
	)
	return err
}
