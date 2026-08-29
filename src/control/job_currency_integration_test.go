package main

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
)

func TestAcceptedJobCurrencyFencesDispatchSettlementAndCollection(t *testing.T) {
	// CAD settlement plus the sole current durable-admission fixture: TEST_ONLY
	// combined-token batch authority, published catalogue schedule, and uniform
	// geometry (one primary + one exact clone). Cross-currency catalogue
	// publication requires the operator FX pair that strangerDeploymentInputs sets.
	strangerDeploymentInputs(t)
	installSettlementCurrencyForTest(t, "cad")
	ctx, store, pool, f, job, tasks, _ := currentUniformMoneyPathJob(t)
	// Free credit is USD-only and contributes nothing under CAD. A saved
	// payment method is the deferred-collection rail for non-prepaid admits;
	// without it SubmitJobTx correctly refuses (the former store path skipped
	// free-credit funding entirely and let CAD jobs enter unfunded).
	// validJobRowClasses marks non-USD jobs prepaid so money-path tests have a
	// spendable CAD balance; this test is the payment-method fence, not prepaid.
	job.PrepaidRequired = false
	must(t, store.UpsertBillingCustomer(ctx, f.BuyerID, "cus_cad_currency_"+f.BuyerID.String()))
	must(t, store.SetBillingPMByCustomer(ctx, "cus_cad_currency_"+f.BuyerID.String(), "pm_cad_currency"))
	mustf(t, store.SubmitJobTx(ctx, job, tasks), "submit CAD job: %v")

	var jobCurrency, planCurrency, jsonCurrency string
	if err := pool.QueryRow(ctx, `
		SELECT j.currency,p.currency,p.plan_json #>> '{schedule,currency}'
		  FROM jobs j JOIN job_economic_plans p ON p.job_id=j.id
		 WHERE j.id=$1`, f.JobID).Scan(&jobCurrency, &planCurrency, &jsonCurrency); err != nil {
		t.Fatal(err)
	}
	if jobCurrency != "cad" || planCurrency != "cad" || jsonCurrency != "cad" {
		t.Fatalf("accepted currency authority job=%q plan=%q json=%q",
			jobCurrency, planCurrency, jsonCurrency)
	}
	if _, err := pool.Exec(ctx, `UPDATE jobs SET currency='usd' WHERE id=$1`, f.JobID); err == nil {
		t.Fatal("database allowed accepted job currency mutation")
	}
	if _, err := pool.Exec(ctx, `UPDATE job_economic_plans SET currency='usd' WHERE job_id=$1`, f.JobID); err == nil {
		t.Fatal("database allowed accepted economic-plan currency mutation")
	}
	if _, err := pool.Exec(ctx, `
		INSERT INTO ledger_entries (kind,task_id,amount_usd,currency,payout_status)
		VALUES ('platform_take',$1,0.01,'usd','released')`, tasks[0].ID); err == nil {
		t.Fatal("database allowed task ledger currency to differ from its job")
	}

	// USD deployment must not claim or start a CAD job. Use the real claim path
	// so placement v3 execution-identity freezes are honest.
	setSettlementCurrency(MustParseCurrency("usd"))
	claimed, err := store.ClaimTasksTx(ctx, WorkerAuth{
		WorkerID: f.WorkerID, SupplierID: f.SupplierID,
	})
	mustf(t, err, "cross-currency claim should be an inert no-match, got %v")
	if claimed != nil {
		t.Fatalf("USD deployment claimed CAD job: %+v", claimed)
	}
	if err := store.StartTask(ctx, tasks[0].ID, f.WorkerID, 0); err == nil ||
		(!errors.Is(err, errCurrencyMismatch) && !errors.Is(err, errNotFound)) {
		// Unclaimed under USD is errNotFound; a partial claim would be currency mismatch.
		t.Fatalf("USD deployment started CAD job: %v", err)
	}
	if _, _, err := store.JobChargeInfo(ctx, f.JobID); !errors.Is(err, errCurrencyMismatch) {
		t.Fatalf("USD deployment exposed CAD job for collection: %v", err)
	}
	if got := taskStatus(t, ctx, pool, tasks[0].ID); got != "queued" {
		t.Fatalf("currency refusal changed task status to %q", got)
	}

	setSettlementCurrency(MustParseCurrency("cad"))
	claimed, err = store.ClaimTasksTx(ctx, WorkerAuth{
		WorkerID: f.WorkerID, SupplierID: f.SupplierID,
	})
	mustf(t, err, "CAD claim: %v")
	if claimed == nil {
		t.Fatal("CAD deployment claimed nothing for CAD job")
	}
	mustf(t, store.StartTask(ctx, claimed.TaskID, f.WorkerID, claimed.Attempt),
		"CAD deployment could not start CAD job: %v")
	// Prefer the claimed task for the rest of the fence (may be either clone).
	startedTaskID := claimed.TaskID
	if _, err := pool.Exec(ctx, `
		UPDATE tasks SET status='verifying' WHERE id=$1`, startedTaskID); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `
		UPDATE jobs SET status='verifying' WHERE id=$1`, f.JobID); err != nil {
		t.Fatal(err)
	}

	entries := splitFrozenCharge(
		f.BuyerID, f.SupplierID, startedTaskID, "cad",
		job.EconomicPlan.BuyerChargePerTaskUSD, job.EconomicPlan.SupplierPayoutPerTaskUSD,
		0, time.Now().UTC(),
	)
	info := &CommitTaskInfo{
		TaskID: startedTaskID, JobID: f.JobID, WorkerID: f.WorkerID,
		SupplierID: f.SupplierID, jobType: job.JobType, ModelRef: job.ModelRef,
		SplitSize: 1, engine: "candle", buildHash: job.PlacementRequirement.EngineBuildHash,
	}
	setSettlementCurrency(MustParseCurrency("usd"))
	if err := store.FinalizeTaskVerification(ctx, info, OutcomePass, entries); !errors.Is(err, errCurrencyMismatch) {
		t.Fatalf("USD deployment settled CAD job: %v", err)
	}
	var ledgerRows int
	if err := pool.QueryRow(ctx, `
		SELECT count(*) FROM ledger_entries WHERE task_id=$1`, startedTaskID).Scan(&ledgerRows); err != nil {
		t.Fatal(err)
	}
	if ledgerRows != 0 || taskStatus(t, ctx, pool, startedTaskID) != "verifying" {
		t.Fatalf("failed settlement was not atomic: ledger=%d status=%s",
			ledgerRows, taskStatus(t, ctx, pool, startedTaskID))
	}

	setSettlementCurrency(MustParseCurrency("cad"))
	mustf(t, store.FinalizeTaskVerification(ctx, info, OutcomePass, entries), "CAD settlement failed: %v")
	var distinctCurrencies int
	if err := pool.QueryRow(ctx, `
		SELECT count(*),count(DISTINCT currency)
		  FROM ledger_entries WHERE task_id=$1 AND currency='cad'`, startedTaskID).
		Scan(&ledgerRows, &distinctCurrencies); err != nil {
		t.Fatal(err)
	}
	if ledgerRows != 3 || distinctCurrencies != 1 {
		t.Fatalf("CAD settlement rows=%d distinct currencies=%d", ledgerRows, distinctCurrencies)
	}

	if _, err := pool.Exec(ctx, canonicalSchema); err != nil {
		t.Fatalf("canonical schema is not idempotent with a frozen CAD job: %v", err)
	}
}

func TestJobEconomicCurrencyConstraintsRejectDirectBypasses(t *testing.T) {
	installSettlementCurrencyForTest(t, "cad")
	// computePlanFixture needs an advertised embed cell; install TEST_ONLY
	// publication authority before any store open that would quarantine it.
	installBoundCataloguePublicationAuthorityForTest(t)
	ctx, _, pool := openMoneyPathStore(t)
	buyerID := uuid.New()
	jobID := uuid.New()
	if _, err := pool.Exec(ctx, `
		INSERT INTO buyers (id,email,password_hash) VALUES ($1,$2,'x')`,
		buyerID, "job-currency-"+buyerID.String()+"@test"); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `
		INSERT INTO jobs (id,buyer_id,job_type,input_ref,currency)
		VALUES ($1,$2,'embed','currency-bypass','cad')`,
		jobID, buyerID); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_, _ = pool.Exec(ctx, `DELETE FROM job_economic_plans WHERE job_id=$1`, jobID)
		_, _ = pool.Exec(ctx, `DELETE FROM jobs WHERE id=$1`, jobID)
		_, _ = pool.Exec(ctx, `DELETE FROM buyers WHERE id=$1`, buyerID)
	})

	_, _, plan := computePlanFixture(t)
	schedule := plan.Schedule
	schedule.Currency = "cad"
	plan = BuildEconomicPlan(plan.Input, schedule)
	badJSONPlan := plan
	badJSONPlan.Schedule.Currency = "usd"
	badJSON, err := json.Marshal(badJSONPlan)
	must(t, err)
	insertPlan := `
		INSERT INTO job_economic_plans (
		  job_id,plan_version,schedule_version,currency,plan_json,initial_task_count,
		  buyer_charge_per_task_usd,supplier_payout_per_task_usd,
		  initial_buyer_charge_usd,reserved_buyer_charge_usd,sla_premium_usd
		) VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11)`
	args := []any{
		jobID, plan.Version, plan.Schedule.Version, "cad", badJSON,
		plan.Input.InitialTaskCount, plan.BuyerChargePerTaskUSD,
		plan.SupplierPayoutPerTaskUSD, plan.InitialBuyerChargeUSD,
		plan.ReservedBuyerChargeUSD, plan.Input.SLAPremiumUSD,
	}
	if _, err := pool.Exec(ctx, insertPlan, args...); err == nil ||
		!strings.Contains(err.Error(), "currency") {
		t.Fatalf("database accepted mismatched plan JSON currency: %v", err)
	}

	goodJSON, err := json.Marshal(badJSONPlan)
	must(t, err)
	args[3], args[4] = "usd", goodJSON
	if _, err := pool.Exec(ctx, insertPlan, args...); err == nil ||
		!strings.Contains(err.Error(), "currency") {
		t.Fatalf("database accepted plan currency differing from job: %v", err)
	}
}
