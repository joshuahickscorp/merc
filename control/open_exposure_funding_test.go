package main

import (
	"context"
	"errors"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"
)

// Open-exposure singularity tests. Three rails (prepaid open reservation,
// realtime funding, free-credit remaining) must hold the same open quantity.
// These tests fail on the unmodified tree where realtime omits service leases
// and under-counts prepaid batch residual, and free credit under-counts and
// does not serialise.

// pickRealtimeCeilingIn returns a realtime auth ceiling in (lo, hi]. Fails the
// test when no token scale lands in that window.
func pickRealtimeCeilingIn(t *testing.T, profile VLLMRuntimeProfile, lo, hi float64) (
	maxUSD, estUSD float64, maxPrompt, maxCompletion int64,
) {
	t.Helper()
	if !(lo < hi) {
		t.Fatalf("empty ceiling window (%f,%f]", lo, hi)
	}
	maxUSD, estUSD, maxPrompt, maxCompletion = realtimeAuthCeiling(t, profile, 7, 2)
	if maxUSD > lo && maxUSD <= hi {
		return maxUSD, estUSD, maxPrompt, maxCompletion
	}
	for tokens := int64(8); tokens <= 200_000; tokens += 4 {
		comp := tokens / 4
		if comp < 1 {
			comp = 1
		}
		candMax, candEst, candP, candC := realtimeAuthCeiling(t, profile, tokens, comp)
		if candMax > lo && candMax <= hi {
			return candMax, candEst, candP, candC
		}
		// Also try asymmetric completion growth.
		candMax, candEst, candP, candC = realtimeAuthCeiling(t, profile, tokens, tokens)
		if candMax > lo && candMax <= hi {
			return candMax, candEst, candP, candC
		}
	}
	t.Fatalf("could not find realtime ceiling in (%f,%f]; last maxUSD=%f", lo, hi, maxUSD)
	return 0, 0, 0, 0
}

// TestOpenExposureRealtimeRefusesWhileServiceLeaseHolds is the cleanest P0
// proof: prepaid B, ACTIVE lease reserving ~0.9B, realtime need in (B-lease, B]
// must refuse. Today evaluateRealtimeBuyerFunding has no service_leases term
// and authorizes.
func TestOpenExposureRealtimeRefusesWhileServiceLeaseHolds(t *testing.T) {
	installSettlementCurrencyForTest(t, "usd")
	ctx, store, pool := openIsolatedTestStore(t)
	profile, _, _ := realtimeFundingFixture(t, ctx, store, pool)

	// Seed B just above a ~$0.90 lease so residual after lease is small
	// enough that a typical realtime ceiling sits in (B-lease, B].
	const seedMicros int64 = 1_000_000 // B = $1.00
	buyerID := uuid.New()
	if _, err := pool.Exec(ctx, `INSERT INTO buyers (id,email,free_credit_usd) VALUES ($1,$2,0)`,
		buyerID, buyerID.String()+"@open-exp-lease.invalid"); err != nil {
		t.Fatal(err)
	}
	must(t, store.SeedPrepaidBalance(ctx, buyerID, seedMicros, "seed-open-exp-lease-"+buyerID.String()))

	worker, _ := newFabricMeasurementWorker(t, ctx, store)
	seedMeasuredWarmResidency(t, ctx, pool, worker.WorkerID, profile.ModelAlias)
	offer := serviceLeaseOffer(profile)
	offer.Region = "ca-open-exp-" + uuid.NewString()
	must(t, store.UpsertServiceLeaseOffer(ctx, worker, offer))

	// 900_000_000 nanos = $0.90 declared ceiling → ~0.9B of seed.
	lease, err := store.CreateServiceLease(ctx, buyerID, ServiceLeaseRequest{
		RuntimeProfileID: profile.RuntimeProfileID, Region: offer.Region, Currency: "usd",
		MinimumReplicas: 1, MaximumReplicas: 1, TermSeconds: 120,
		MaximumP95LatencyMilliseconds: 500, BuyerDeclaredCeilingNanos: 900_000_000,
	})
	mustf(t, err, "create service lease: %v")
	if lease.ReservedBuyerMicros <= 0 || lease.ReservedBuyerMicros >= seedMicros {
		t.Fatalf("lease reserved %d micros, want 0 < reserved < seed %d", lease.ReservedBuyerMicros, seedMicros)
	}

	// Ceiling must fit in full prepaid (today authorizes) but exceed residual
	// after the lease hold (must refuse once leases are counted).
	lo := microsToUSD(seedMicros - lease.ReservedBuyerMicros)
	hi := microsToUSD(seedMicros)
	maxUSD, estUSD, maxPrompt, maxCompletion := pickRealtimeCeilingIn(t, profile, lo, hi)

	_, _, err = store.AuthorizeRealtimeContract(ctx, RealtimeContractAuthorization{
		RequestID: "req-open-exp-lease-" + uuid.NewString(), BuyerID: buyerID, Profile: profile,
		InputCommitment: strings.Repeat("a", 64), RequestSHA256: strings.Repeat("b", 64),
		MaximumPriceUSD: maxUSD, EstimatedPriceUSD: estUSD, DeadlineAt: time.Now().Add(time.Minute),
		MaximumPromptTokens: maxPrompt, MaximumCompletionTokens: maxCompletion,
		EstimatedPromptTokens: maxPrompt / 2, EstimatedCompletionTokens: maxCompletion / 2,
	})
	if err == nil {
		t.Fatalf("AuthorizeRealtimeContract succeeded while ACTIVE service lease holds %d micros of prepaid %d; "+
			"evaluateRealtimeBuyerFunding under-holds by omitting service_leases (ceiling=%f residual_after_lease=%f)",
			lease.ReservedBuyerMicros, seedMicros, maxUSD, lo)
	}
	if !errors.Is(err, errRealtimeInsufficientFunds) && !errors.Is(err, errRealtimeTopupRequired) {
		t.Fatalf("want insufficient funds / topup required, got %v", err)
	}
}

// TestOpenExposureRealtimeRefusesWhilePrepaidBatchReserves is the batch arm:
// prepaid B, open prepaid job with Reserved=0.9B and Estimated=0.45B, realtime
// need in (B-reserved, B-estimated] must refuse. Today realtime subtracts only
// estimated and authorizes.
func TestOpenExposureRealtimeRefusesWhilePrepaidBatchReserves(t *testing.T) {
	installSettlementCurrencyForTest(t, "usd")
	ctx, store, pool := openIsolatedTestStore(t)
	profile, _, _ := realtimeFundingFixture(t, ctx, store, pool)

	// Absolute dollars sized so typical realtime ceilings (~$0.05–$0.40) land
	// in (B-reserved, B-estimated] = ($0.10, $0.55].
	const seedMicros int64 = 1_000_000 // B = $1.00
	const reservedUSD = 0.90           // 0.9B
	const estimatedUSD = 0.45          // 0.45B
	buyerID := uuid.New()
	if _, err := pool.Exec(ctx, `INSERT INTO buyers (id,email,free_credit_usd) VALUES ($1,$2,0)`,
		buyerID, buyerID.String()+"@open-exp-batch.invalid"); err != nil {
		t.Fatal(err)
	}
	must(t, store.SeedPrepaidBalance(ctx, buyerID, seedMicros, "seed-open-exp-batch-"+buyerID.String()))

	jobID := uuid.New()
	if _, err := pool.Exec(ctx, `
		INSERT INTO jobs (id,buyer_id,status,job_type,input_ref,task_count,prepaid_required,estimated_usd,currency)
		VALUES ($1,$2,'queued','embed','open-exp-batch',1,true,$3,'usd')`,
		jobID, buyerID, estimatedUSD); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `
		INSERT INTO job_economic_plans
		  (job_id,plan_version,schedule_version,plan_json,initial_task_count,
		   buyer_charge_per_task_usd,supplier_payout_per_task_usd,
		   initial_buyer_charge_usd,reserved_buyer_charge_usd,currency)
		VALUES ($1,1,'test','{"schedule":{"currency":"usd"}}',1,$2,$3,$2,$4,'usd')`,
		jobID, estimatedUSD, estimatedUSD*0.7, reservedUSD); err != nil {
		t.Fatal(err)
	}

	lo := microsToUSD(seedMicros) - reservedUSD  // $0.10
	hi := microsToUSD(seedMicros) - estimatedUSD // $0.55
	maxUSD, estUSD, maxPrompt, maxCompletion := pickRealtimeCeilingIn(t, profile, lo, hi)

	_, _, err := store.AuthorizeRealtimeContract(ctx, RealtimeContractAuthorization{
		RequestID: "req-open-exp-batch-" + uuid.NewString(), BuyerID: buyerID, Profile: profile,
		InputCommitment: strings.Repeat("c", 64), RequestSHA256: strings.Repeat("d", 64),
		MaximumPriceUSD: maxUSD, EstimatedPriceUSD: estUSD, DeadlineAt: time.Now().Add(time.Minute),
		MaximumPromptTokens: maxPrompt, MaximumCompletionTokens: maxCompletion,
		EstimatedPromptTokens: maxPrompt / 2, EstimatedCompletionTokens: maxCompletion / 2,
	})
	if err == nil {
		t.Fatalf("AuthorizeRealtimeContract succeeded with prepaid batch residual $%.2f still open "+
			"(estimated only $%.2f, ceiling=%f); realtime under-holds reserved residual",
			reservedUSD, estimatedUSD, maxUSD)
	}
	if !errors.Is(err, errRealtimeInsufficientFunds) && !errors.Is(err, errRealtimeTopupRequired) {
		t.Fatalf("want insufficient funds / topup required, got %v", err)
	}
}

// TestOpenExposureFreeCreditSequentialHoldsReserved is the P1 sequential arm:
// grant G with I+I <= G < R+I — second admit must refuse once the first holds R.
func TestOpenExposureFreeCreditSequentialHoldsReserved(t *testing.T) {
	installSettlementCurrencyForTest(t, "usd")
	ctx, store, pool, f, first, firstTasks, _ := currentUniformMoneyPathJob(t)

	I := first.EconomicPlan.InitialBuyerChargeUSD
	R := first.EconomicPlan.ReservedBuyerChargeUSD
	if !(I > 0 && R > I) {
		t.Fatalf("fixture needs R > I > 0; got I=%f R=%f", I, R)
	}
	// G satisfies I+I <= G < R+I
	G := (2*I + (R + I)) / 2
	if G < 2*I {
		G = 2 * I
	}
	if G >= R+I {
		G = R + I - 0.000001
	}
	if G < 2*I || G >= R+I {
		t.Fatalf("cannot place G in [%f, %f); got G=%f", 2*I, R+I, G)
	}
	if _, err := pool.Exec(ctx, `UPDATE buyers SET free_credit_usd=$1 WHERE id=$2`, G, f.BuyerID); err != nil {
		t.Fatal(err)
	}
	first.PrepaidRequired = false

	mustf(t, store.SubmitJobTx(ctx, first, firstTasks), "first free-credit admit: %v")

	remaining, err := store.BuyerFreeCreditRemaining(ctx, f.BuyerID)
	must(t, err)
	// After first job, hold must be R (reserved), not I (estimated).
	wantRemaining := G - R
	if wantRemaining < 0 {
		wantRemaining = 0
	}
	if remaining > wantRemaining+1e-6 {
		t.Fatalf("free credit remaining after first job=%f, want <= %f (G-R): still holding estimated I=%f not reserved R=%f",
			remaining, wantRemaining, I, R)
	}

	// Second job identical economics, new ids.
	secondFixture := f
	secondFixture.JobID = uuid.New()
	secondFixture.TaskIDs = []uuid.UUID{uuid.New(), uuid.New()}
	secondTasks := makeTasks(secondFixture, 2)
	second := validJobRow(t, secondFixture, secondTasks)
	authority, err := store.LoadCataloguePriceAuthority(ctx, second.ModelRef)
	mustf(t, err, "load second catalogue authority: %v")
	economicInput := second.EconomicPlan.Input
	economicInput.SupplierShare = authority.SupplierShare
	economicSchedule := second.EconomicPlan.Schedule
	economicSchedule.Currency = authority.SettlementCurrency
	economic := BuildEconomicPlan(economicInput, economicSchedule)
	if !economic.Executable {
		t.Fatalf("second economic plan blocked: %s", economic.BlockReason)
	}
	placement := placementForPricingFixture(t, second.WorkloadDecision, authority)
	pricing, err := newDistributedPricingDecision(
		second.WorkloadDecision, second.ComputePlan, placement, economic,
		authority, second.WorkloadDecision.Binding.Tier, "",
	)
	mustf(t, err, "rebuild second pricing: %v")
	second.EconomicPlan = economic
	second.EstimatedUSD = economic.InitialBuyerChargeUSD
	second.PlacementRequirement = placement
	second.PricingDecision = pricing
	second.HWClasses = append([]string(nil), placement.HWClasses...)
	second.MinMemoryGB = placement.MinMemoryGB
	second.OfferedRateUsdHr = placement.OfferedRateUsdHr
	second.PrepaidRequired = false

	err = store.SubmitJobTx(ctx, second, secondTasks)
	if err == nil {
		t.Fatalf("second free-credit admit succeeded; G=%f I=%f R=%f so I+I<=G < R+I requires refusal", G, I, R)
	}
}

// TestOpenExposureFreeCreditConcurrentCeiling mirrors
// TestAuthorizeLateLockConcurrentFreeCreditCeiling for the batch path: grant
// covers exactly one reserved ceiling, N concurrent admits, exactly one succeeds.
func TestOpenExposureFreeCreditConcurrentCeiling(t *testing.T) {
	installSettlementCurrencyForTest(t, "usd")
	ctx, store, pool, f, template, templateTasks, _ := currentUniformMoneyPathJob(t)
	R := template.EconomicPlan.ReservedBuyerChargeUSD
	if R <= 0 {
		t.Fatalf("fixture reserved=%f", R)
	}
	// Grant covers exactly one reserved ceiling (micro slack for float noise).
	G := R + 0.000001
	if _, err := pool.Exec(ctx, `UPDATE buyers SET free_credit_usd=$1 WHERE id=$2`, G, f.BuyerID); err != nil {
		t.Fatal(err)
	}

	const n = 8
	var (
		wg        sync.WaitGroup
		start     = make(chan struct{})
		okCount   atomic.Int64
		failCount atomic.Int64
	)
	type submission struct {
		job   *jobRow
		tasks []taskRow
	}
	subs := make([]submission, n)
	for i := 0; i < n; i++ {
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
		job.PrepaidRequired = false
		_ = templateTasks
		subs[i] = submission{job: job, tasks: tasks}
	}

	wg.Add(n)
	for i := 0; i < n; i++ {
		go func(i int) {
			defer wg.Done()
			<-start
			err := store.SubmitJobTx(context.Background(), subs[i].job, subs[i].tasks)
			if err == nil {
				okCount.Add(1)
				return
			}
			failCount.Add(1)
		}(i)
	}
	close(start)
	wg.Wait()
	if okCount.Load() != 1 {
		t.Fatalf("free-credit concurrent batch admit succeeded=%d fail=%d, want exactly 1 success (grant covers one reserved ceiling R=%f)",
			okCount.Load(), failCount.Load(), R)
	}
	var jobs int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM jobs WHERE buyer_id=$1 AND status IN ('queued','running','verifying')`,
		f.BuyerID).Scan(&jobs); err != nil {
		t.Fatal(err)
	}
	if jobs != 1 {
		t.Fatalf("open jobs=%d, want exactly 1", jobs)
	}
}

// TestOpenExposureDebitedReservationFreesCashForRealtime is the no-regression
// arm: after prepaid residual is consumed by prepaid_debit, realtime must still
// admit against the freed cash. Proves the fix does not permanently over-hold
// reserved without netting debits.
func TestOpenExposureDebitedReservationFreesCashForRealtime(t *testing.T) {
	installSettlementCurrencyForTest(t, "usd")
	ctx, store, pool := openIsolatedTestStore(t)
	profile, _, _ := realtimeFundingFixture(t, ctx, store, pool)

	const seedMicros int64 = 1_000_000 // $1.00
	const reservedUSD = 0.90
	const estimatedUSD = 0.45
	buyerID := uuid.New()
	if _, err := pool.Exec(ctx, `INSERT INTO buyers (id,email,free_credit_usd) VALUES ($1,$2,0)`,
		buyerID, buyerID.String()+"@open-exp-debit.invalid"); err != nil {
		t.Fatal(err)
	}
	must(t, store.SeedPrepaidBalance(ctx, buyerID, seedMicros, "seed-open-exp-debit-"+buyerID.String()))

	jobID := uuid.New()
	taskID := uuid.New()
	if _, err := pool.Exec(ctx, `
		INSERT INTO jobs (id,buyer_id,status,job_type,input_ref,task_count,prepaid_required,estimated_usd,currency)
		VALUES ($1,$2,'running','embed','open-exp-debit',1,true,$3,'usd')`,
		jobID, buyerID, estimatedUSD); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `
		INSERT INTO job_economic_plans
		  (job_id,plan_version,schedule_version,plan_json,initial_task_count,
		   buyer_charge_per_task_usd,supplier_payout_per_task_usd,
		   initial_buyer_charge_usd,reserved_buyer_charge_usd,currency)
		VALUES ($1,1,'test','{"schedule":{"currency":"usd"}}',1,$2,$3,$2,$4,'usd')`,
		jobID, estimatedUSD, estimatedUSD*0.7, reservedUSD); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `
		INSERT INTO tasks (id,job_id,status,input_ref,chunk_index)
		VALUES ($1,$2,'complete','open-exp-debit',0)`, taskID, jobID); err != nil {
		t.Fatal(err)
	}
	// Consume the full reserved residual via prepaid_debit. Residual → 0 while
	// the job is still open. An over-hold that ignores debits leaves residual
	// at $0.90 and available ≈ $0.10; a ceiling in ($0.10, $1.00] would then
	// refuse incorrectly.
	debitMicros := usdToMicros(reservedUSD)
	if err := store.DebitPrepaidForTask(ctx, buyerID, taskID, debitMicros); err != nil {
		t.Fatalf("debit reserved residual: %v", err)
	}

	// Ceiling larger than the wrong residual residual (seed-reserved=$0.10)
	// but within full prepaid top-up authority.
	lo := microsToUSD(seedMicros) - reservedUSD // $0.10 if residual not netted
	hi := microsToUSD(seedMicros)               // $1.00
	maxUSD, estUSD, maxPrompt, maxCompletion := pickRealtimeCeilingIn(t, profile, lo, hi)

	_, _, err := store.AuthorizeRealtimeContract(ctx, RealtimeContractAuthorization{
		RequestID: "req-open-exp-debit-" + uuid.NewString(), BuyerID: buyerID, Profile: profile,
		InputCommitment: strings.Repeat("e", 64), RequestSHA256: strings.Repeat("f", 64),
		MaximumPriceUSD: maxUSD, EstimatedPriceUSD: estUSD, DeadlineAt: time.Now().Add(time.Minute),
		MaximumPromptTokens: maxPrompt, MaximumCompletionTokens: maxCompletion,
		EstimatedPromptTokens: maxPrompt / 2, EstimatedCompletionTokens: maxCompletion / 2,
	})
	if err != nil {
		t.Fatalf("realtime admit after reserved residual was fully debited failed: %v "+
			"(open-exposure over-holds freed cash; ceiling=%f)", err, maxUSD)
	}
}
