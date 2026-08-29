package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
)

// The seven loop-failure classes that can be driven from the store and the
// workers without a live agent. Each test names the injected fault, asserts
// the money outcome on ledger rows, and ends with the same zero-sum /
// no-double-pay check.
//
// The five classes already independently proven (no-double-settlement,
// no-phantom-balance, duplicate submission, out-of-order webhooks, refund
// behaviour) are not re-tested here.

type loopFaultMoney struct {
	buyerChargeRows      int
	buyerChargeMicros    int64
	supplierCreditRows   int
	supplierCreditMicros int64
	platformTakeMicros   int64
	settlementNetMicros  int64
	tasksPaidTwice       int
}

func readLoopFaultMoney(t *testing.T, ctx context.Context, pool *pgxpool.Pool, jobID uuid.UUID) loopFaultMoney {
	t.Helper()
	var m loopFaultMoney
	err := pool.QueryRow(ctx, `
		SELECT
		  COUNT(*) FILTER (WHERE le.kind='buyer_charge'),
		  COALESCE(SUM((le.amount_usd * 1000000)::bigint) FILTER (WHERE le.kind='buyer_charge'),0),
		  COUNT(*) FILTER (WHERE le.kind='supplier_credit'),
		  COALESCE(SUM((le.amount_usd * 1000000)::bigint) FILTER (WHERE le.kind='supplier_credit'),0),
		  COALESCE(SUM((le.amount_usd * 1000000)::bigint) FILTER (WHERE le.kind='platform_take'),0),
		  COALESCE(SUM((le.amount_usd * 1000000)::bigint) FILTER (
		    WHERE le.kind IN ('buyer_charge','supplier_credit','platform_take',
		                      'clawback','buyer_refund','platform_refund')),0)
		  FROM ledger_entries le
		  JOIN tasks t ON t.id = le.task_id
		 WHERE t.job_id=$1`, jobID).
		Scan(&m.buyerChargeRows, &m.buyerChargeMicros,
			&m.supplierCreditRows, &m.supplierCreditMicros,
			&m.platformTakeMicros, &m.settlementNetMicros)
	mustf(t, err, "read job settlement ledger: %v")

	err = pool.QueryRow(ctx, `
		SELECT COUNT(*) FROM (
		  SELECT le.task_id
		    FROM ledger_entries le
		    JOIN tasks t ON t.id = le.task_id
		   WHERE t.job_id=$1 AND le.kind='supplier_credit'
		   GROUP BY le.task_id
		  HAVING COUNT(*) > 1
		) paid_twice`, jobID).Scan(&m.tasksPaidTwice)
	mustf(t, err, "read double-pay: %v")
	return m
}

func readLoopFaultPrepaid(t *testing.T, ctx context.Context, pool *pgxpool.Pool, buyerID uuid.UUID) (rows int, micros int64) {
	t.Helper()
	err := pool.QueryRow(ctx, `
		SELECT COUNT(*), COALESCE(SUM((amount_usd * 1000000)::bigint),0)
		  FROM ledger_entries
		 WHERE buyer_id=$1 AND kind='prepaid_topup'`, buyerID).
		Scan(&rows, &micros)
	mustf(t, err, "read prepaid top-up ledger: %v")
	return rows, micros
}

// assertLoopFaultMoneyInvariants is the global check every class ends on:
// settlement legs for this job sum to zero, and no task has two supplier_credit
// rows.
func assertLoopFaultMoneyInvariants(t *testing.T, ctx context.Context, pool *pgxpool.Pool, jobID uuid.UUID) {
	t.Helper()
	m := readLoopFaultMoney(t, ctx, pool, jobID)
	t.Logf("zero-sum: buyer_charge_rows=%d micros=%d supplier_credit_rows=%d micros=%d platform_micros=%d net=%d paid_twice=%d",
		m.buyerChargeRows, m.buyerChargeMicros, m.supplierCreditRows, m.supplierCreditMicros,
		m.platformTakeMicros, m.settlementNetMicros, m.tasksPaidTwice)
	if m.settlementNetMicros != 0 {
		t.Errorf("ledger is not zero-sum: settlement net %d micros", m.settlementNetMicros)
	}
	if m.tasksPaidTwice != 0 {
		t.Errorf("%d task(s) have more than one supplier_credit; a task was paid twice", m.tasksPaidTwice)
	}
}

func enableLocalObjectStoreForLoopFaults(t *testing.T) {
	t.Helper()
	if strings.TrimSpace(os.Getenv("MERC_TEST_S3_ENDPOINT")) != "" {
		return
	}
	if os.Getenv(allowSkippingDBTestsEnv) == "1" {
		t.Skipf("MERC_TEST_S3_ENDPOINT is unset and %s=1; loop-fault cases require object storage", allowSkippingDBTestsEnv)
	}
	// Local compose defaults (ops/deploy/docker-compose.yml). The harness skips when these
	// stay unset; this lane has to actually take the proof.
	t.Setenv("MERC_TEST_S3_ENDPOINT", "http://127.0.0.1:9000")
	t.Setenv("MERC_TEST_S3_BUCKET", "cx-jobs")
	t.Setenv("MERC_TEST_S3_ACCESS_KEY", "minioadmin")
	t.Setenv("MERC_TEST_S3_SECRET_KEY", "minioadmin")
}

func loopFaultEmbedBody() []byte {
	vec := make([]float64, EMBED_DIM_FOR_CHAIN)
	vec[0] = 0.25
	vec[1] = -0.5
	vec[2] = 0.125
	body, err := json.Marshal(struct {
		JobType string      `json:"job_type"`
		Model   string      `json:"model"`
		Dim     int         `json:"dim"`
		Count   int         `json:"count"`
		Vectors [][]float64 `json:"vectors"`
	}{
		JobType: "embed",
		Model:   "all-minilm-l6-v2",
		Dim:     EMBED_DIM_FOR_CHAIN,
		Count:   1,
		Vectors: [][]float64{vec},
	})
	if err != nil {
		panic(err)
	}
	return body
}

type loopFaultCase struct {
	h    *verifiedHarness
	ctx  context.Context
	f    moneyPathFixture
	body []byte
}

func newLoopFaultCase(t *testing.T, taskCount int) *loopFaultCase {
	t.Helper()
	if taskCount <= 0 {
		taskCount = 1
	}
	enableLocalObjectStoreForLoopFaults(t)
	h, ctx, store := newVerifiedArtifactHarness(t)
	f := seedMoneyPathFixture(t, ctx, store, h.pool(), moneyPathSeedOpts{
		TaskCount: taskCount, TaskStatus: "running", ClaimWorker: true,
		SeedJob: true, SeedPlanRows: true,
	})
	body := loopFaultEmbedBody()
	for _, taskID := range f.TaskIDs {
		if _, err := h.pool().Exec(ctx, `
			UPDATE tasks SET runtime_cell_id=$2, runtime_id=$3,
			                 runtime_matrix_sha256=$4, model_kind=$5,
			                 claimed_at=now(), started_at=now()
			 WHERE id=$1`,
			taskID, llamaEmbedCell, "llama_cpp_metal",
			generatedRuntimeMatrixSHA256, "gguf"); err != nil {
			t.Fatalf("stamp runtime provenance: %v", err)
		}
		var inputRef string
		if err := h.pool().QueryRow(ctx,
			`SELECT input_ref FROM tasks WHERE id=$1`, taskID).Scan(&inputRef); err != nil {
			t.Fatal(err)
		}
		corpus := []byte(`{"id":"0","text":"loop fault matrix"}` + "\n")
		mustf(t, h.storage.PutObject(ctx, inputRef, corpus, "application/x-ndjson"),
			"upload input %s: %v", inputRef)
	}
	return &loopFaultCase{h: h, ctx: ctx, f: f, body: body}
}

func (c *loopFaultCase) commit(t *testing.T, taskID uuid.UUID) {
	t.Helper()
	c.h.commitThroughStorage(c.ctx, c.f, taskID, c.body)
}

func (c *loopFaultCase) processAttempt(t *testing.T, taskID uuid.UUID) VerificationProcessResult {
	t.Helper()
	var last VerificationProcessResult
	var lastErr error
	for i := 0; i < 12; i++ {
		last, lastErr = c.h.processor.ProcessAttempt(c.ctx, taskID, 0)
		if lastErr == nil && !last.Pending {
			return last
		}
		if lastErr != nil &&
			!errors.Is(lastErr, ErrVerificationWorkBusy) &&
			!errors.Is(lastErr, ErrVerificationChunkBusy) &&
			!errors.Is(lastErr, ErrVerificationResourceBusy) {
			t.Fatalf("ProcessAttempt: %v", lastErr)
		}
		time.Sleep(25 * time.Millisecond)
	}
	t.Fatalf("ProcessAttempt still pending after retries: %+v err=%v", last, lastErr)
	return last
}

func reattachRunningClaim(t *testing.T, ctx context.Context, pool *pgxpool.Pool, taskID, workerID uuid.UUID) int16 {
	t.Helper()
	tag, err := pool.Exec(ctx, `
		UPDATE tasks
		   SET status='running', claimed_by=$2, claimed_at=now(), worker_id=$2, visible_at=now()
		 WHERE id=$1 AND status IN ('retrying','queued')`, taskID, workerID)
	mustf(t, err, "reattach running claim: %v")
	if tag.RowsAffected() != 1 {
		t.Fatalf("reattach running claim: %d rows, want 1", tag.RowsAffected())
	}
	var retries int16
	mustf(t, pool.QueryRow(ctx, `SELECT retry_count FROM tasks WHERE id=$1`, taskID).
		Scan(&retries), "read retry_count: %v")
	return retries
}

// A retry, a redelivery, and a concurrent duplicate must debit the buyer once.
func TestLoopFaultNoDoubleCharge(t *testing.T) {
	c := newLoopFaultCase(t, 1)
	taskID := c.f.TaskIDs[0]
	c.commit(t, taskID)

	t.Log("FAULT: concurrent ProcessAttempt on one committed task, then serial redelivery")

	var wg sync.WaitGroup
	const racers = 3
	wg.Add(racers)
	for i := 0; i < racers; i++ {
		go func() {
			defer wg.Done()
			_, _ = c.h.processor.ProcessAttempt(c.ctx, taskID, 0)
		}()
	}
	wg.Wait()

	// Redelivery: the same attempt is presented again after the race.
	for i := 0; i < 3; i++ {
		_, err := c.h.processor.ProcessAttempt(c.ctx, taskID, 0)
		if err != nil &&
			!errors.Is(err, ErrVerificationWorkBusy) &&
			!errors.Is(err, ErrVerificationWorkTerminal) &&
			!errors.Is(err, ErrVerificationChunkBusy) &&
			!errors.Is(err, ErrVerificationResourceBusy) {
			t.Fatalf("redelivery ProcessAttempt: %v", err)
		}
	}

	money := readLoopFaultMoney(t, c.ctx, c.h.pool(), c.f.JobID)
	t.Logf("LEDGER: buyer_charge_rows=%d micros=%d supplier_credit_rows=%d",
		money.buyerChargeRows, money.buyerChargeMicros, money.supplierCreditRows)
	if money.buyerChargeRows != 1 {
		t.Errorf("buyer_charge rows=%d, want exactly 1 across retry/redelivery/concurrent duplicate",
			money.buyerChargeRows)
	}
	if money.buyerChargeMicros >= 0 {
		t.Errorf("buyer_charge micros=%d, want a debit", money.buyerChargeMicros)
	}
	if money.supplierCreditRows != 1 {
		t.Errorf("supplier_credit rows=%d, want 1", money.supplierCreditRows)
	}
	assertLoopFaultMoneyInvariants(t, c.ctx, c.h.pool(), c.f.JobID)
}

// What the buyer is charged equals what the receipt says, including a
// verification execution Merc bought (honeypot).
func TestLoopFaultExactReceipt(t *testing.T) {
	c := newLoopFaultCase(t, 2)
	primary, honeypot := c.f.TaskIDs[0], c.f.TaskIDs[1]

	var honeypotInput string
	mustf(t, c.h.pool().QueryRow(c.ctx, `SELECT input_ref FROM tasks WHERE id=$1`, honeypot).
		Scan(&honeypotInput), "honeypot input: %v")
	mustf(t, c.h.store.InsertHoneypot(c.ctx, "embed", honeypotInput, c.body, ""),
		"seed honeypot known answer: %v")
	if _, err := c.h.pool().Exec(c.ctx, `UPDATE tasks SET is_honeypot=true WHERE id=$1`, honeypot); err != nil {
		t.Fatalf("mark honeypot: %v", err)
	}

	t.Log("FAULT: settle a primary task and a Merc-bought honeypot verification execution")
	c.commit(t, primary)
	c.commit(t, honeypot)
	c.processAttempt(t, primary)
	c.processAttempt(t, honeypot)
	if err := c.h.store.FinalizeJobTx(c.ctx, c.f.JobID); err != nil {
		t.Logf("FinalizeJobTx: %v", err)
	}

	invoice, err := c.h.store.JobInvoice(c.ctx, c.f.JobID, c.f.BuyerID)
	mustf(t, err, "JobInvoice: %v")
	tasks, err := c.h.store.JobTaskReceipts(c.ctx, c.f.JobID)
	mustf(t, err, "JobTaskReceipts: %v")
	verif, err := c.h.store.JobVerification(c.ctx, c.f.JobID)
	mustf(t, err, "JobVerification: %v")
	classes, err := c.h.store.JobVerificationClasses(c.ctx, c.f.JobID)
	mustf(t, err, "JobVerificationClasses: %v")
	receipt := assembleClearingReceipt(
		c.f.JobID, invoice.Status, nil, nil, nil, nil, invoice, verif, classes, tasks)

	money := readLoopFaultMoney(t, c.ctx, c.h.pool(), c.f.JobID)
	t.Logf("LEDGER: buyer_charge_rows=%d micros=%d supplier_credit_rows=%d invoice.ChargedUSD=%.9f",
		money.buyerChargeRows, money.buyerChargeMicros, money.supplierCreditRows, invoice.ChargedUSD)

	if usdToMicros(invoice.ChargedUSD) != money.buyerChargeMicros {
		t.Errorf("receipt ChargedUSD micros=%d, ledger buyer_charge micros=%d",
			usdToMicros(invoice.ChargedUSD), money.buyerChargeMicros)
	}
	if usdToMicros(invoice.SupplierPaidUSD) != money.supplierCreditMicros {
		t.Errorf("receipt SupplierPaidUSD micros=%d, ledger supplier_credit micros=%d",
			usdToMicros(invoice.SupplierPaidUSD), money.supplierCreditMicros)
	}
	if invoice.NetChargedUSD != nil &&
		usdToMicros(*invoice.NetChargedUSD) != -money.buyerChargeMicros {
		t.Errorf("receipt NetChargedUSD micros=%d, want %d",
			usdToMicros(*invoice.NetChargedUSD), -money.buyerChargeMicros)
	}

	if len(receipt.Tasks) != 2 {
		t.Errorf("receipt tasks=%d, want 2 (primary + honeypot)", len(receipt.Tasks))
	}
	var sawHoneypot bool
	for _, tr := range receipt.Tasks {
		if tr.IsHoneypot {
			sawHoneypot = true
		}
	}
	if !sawHoneypot {
		t.Error("receipt omitted the honeypot verification execution Merc bought")
	}

	taskChargeMicros := func(taskID uuid.UUID) int64 {
		t.Helper()
		var micros int64
		mustf(t, c.h.pool().QueryRow(c.ctx, `
			SELECT COALESCE(SUM((amount_usd * 1000000)::bigint),0)
			  FROM ledger_entries WHERE task_id=$1 AND kind='buyer_charge'`, taskID).
			Scan(&micros), "task buyer_charge: %v")
		return micros
	}
	primaryCharge := taskChargeMicros(primary)
	honeypotCharge := taskChargeMicros(honeypot)
	if honeypotCharge >= 0 {
		t.Errorf("honeypot verification execution buyer_charge micros=%d, want a debit (Merc-bought check must appear in the charge)",
			honeypotCharge)
	}
	if primaryCharge+honeypotCharge != money.buyerChargeMicros {
		t.Errorf("primary %d + honeypot %d != job buyer_charge %d",
			primaryCharge, honeypotCharge, money.buyerChargeMicros)
	}
	if usdToMicros(invoice.ChargedUSD) != primaryCharge+honeypotCharge {
		t.Errorf("receipt ChargedUSD micros=%d does not include both primary %d and honeypot %d",
			usdToMicros(invoice.ChargedUSD), primaryCharge, honeypotCharge)
	}
	if money.buyerChargeRows != 2 {
		t.Errorf("buyer_charge rows=%d, want 2 (primary + Merc-bought verification)", money.buyerChargeRows)
	}
	assertLoopFaultMoneyInvariants(t, c.ctx, c.h.pool(), c.f.JobID)
}

// The same Stripe event delivered N times moves money once. The Nth delivery is
// a no-op (HTTP 200 / Duplicate), not an error that would hide a partial write.
func TestLoopFaultWebhookRetries(t *testing.T) {
	ctx, store, pool := openIsolatedMoneyPathStore(t)
	suffix := uuid.NewString()
	buyerID, err := store.CreateBuyerAccount(ctx, "loop-fault-wh-"+suffix+"@example.test", "integration-password", 0)
	mustf(t, err, "create buyer: %v")

	opKey := "prepaid-topup-" + suffix
	charge := ChargeResult{
		PaymentIntentID: "pi_loop_fault_" + suffix,
		ChargeID:        "ch_loop_fault_" + suffix,
		RequestedCents:  1500,
		ReceivedCents:   1500,
		Currency:        SettlementCurrencyCode(),
	}
	if _, err := store.BeginPrepaidTopup(ctx, opKey, buyerID, charge.RequestedCents); err != nil {
		t.Fatalf("BeginPrepaidTopup: %v", err)
	}

	secret := "whsec_loop_fault_retry"
	payload := []byte(fmt.Sprintf(
		`{"id":"evt_loop_fault_%s","type":"payment_intent.succeeded","api_version":"%s","livemode":false,"created":1700000100,"data":{"object":{"id":"%s","latest_charge":{"id":"%s"},"status":"succeeded","amount":%d,"amount_received":%d,"currency":"%s","metadata":{"cx_operation_key":"%s"}}}}`,
		suffix, stripeAPIVersion, charge.PaymentIntentID, charge.ChargeID,
		charge.RequestedCents, charge.ReceivedCents, charge.Currency, opKey))

	const deliveries = 5
	t.Logf("FAULT: deliver the same payment_intent.succeeded %d times", deliveries)
	for i := 0; i < deliveries; i++ {
		req := signedStripeCashRequest(t, payload, secret)
		rec := httptest.NewRecorder()
		handleStripeWebhookWithAllHandlersAtMode(
			rec, req, secret, nil, store.ApplyPaymentEventTx,
			store.ReconcileBuyerChargeOperation, false,
		)
		if rec.Code != 200 {
			t.Fatalf("delivery %d/%d returned %d %s (want 200 no-op-or-apply)",
				i+1, deliveries, rec.Code, rec.Body.String())
		}
	}

	topupRows, topupMicros := readLoopFaultPrepaid(t, ctx, pool, buyerID)
	t.Logf("LEDGER: prepaid_topup rows=%d micros=%d after %d deliveries",
		topupRows, topupMicros, deliveries)
	if topupRows != 1 {
		t.Errorf("prepaid_topup rows=%d, want 1", topupRows)
	}
	wantMicros := int64(charge.ReceivedCents) * (microUSDPerUSD / 100)
	if topupMicros != wantMicros {
		t.Errorf("prepaid_topup micros=%d, want %d", topupMicros, wantMicros)
	}
	bal, err := store.BuyerPrepaidBalanceMicros(ctx, buyerID)
	mustf(t, err, "BuyerPrepaidBalanceMicros: %v")
	if bal != wantMicros {
		t.Errorf("prepaid balance micros=%d, want %d", bal, wantMicros)
	}

	// Same Stripe cash event, N times, through the store applier. The Nth is
	// Duplicate (a no-op), never a conflict error.
	eventID := "evt_loop_fault_cash_" + suffix
	object := []byte(fmt.Sprintf(
		`{"id":%q,"charge":%q,"payment_intent":%q,"amount":1500,"currency":%q,"status":"needs_response"}`,
		"dp_loop_fault_"+suffix, charge.ChargeID, charge.PaymentIntentID, charge.Currency))
	cashPayload := []byte(fmt.Sprintf(
		`{"id":%q,"type":%q,"created":1700000101,"data":{"object":%s}}`,
		eventID, stripeEventDisputeCreated, object))
	cashEvent, err := parseStripeCashEvent(eventID, stripeEventDisputeCreated, 1_700_000_101, object, cashPayload)
	mustf(t, err, "parse cash event: %v")

	var applied, duplicates int
	for i := 0; i < deliveries; i++ {
		result, err := store.ApplyPaymentEventTx(ctx, cashEvent)
		mustf(t, err, "ApplyPaymentEventTx delivery %d: %v", i)
		switch {
		case result.Duplicate:
			duplicates++
		case result.CashEffectApplied:
			applied++
		}
	}
	t.Logf("cash event deliveries: applied=%d duplicate=%d", applied, duplicates)
	if applied != 1 {
		t.Errorf("cash effect applied %d times, want 1", applied)
	}
	if duplicates != deliveries-1 {
		t.Errorf("duplicate no-ops=%d, want %d", duplicates, deliveries-1)
	}
	var storedEvents int
	mustf(t, pool.QueryRow(ctx,
		`SELECT COUNT(*) FROM stripe_webhook_events WHERE event_id=$1`, eventID).
		Scan(&storedEvents), "count webhook events: %v")
	if storedEvents != 1 {
		t.Errorf("stripe_webhook_events rows=%d, want 1", storedEvents)
	}

	assertLoopFaultMoneyInvariants(t, ctx, pool, uuid.Nil)
}

// Failure before any worker executes: no charge, no supplier credit, terminal
// job, nothing stranded.
func TestLoopFaultFailureBeforeExecution(t *testing.T) {
	ctx, store, pool := openIsolatedMoneyPathStore(t)
	f := seedMoneyPathFixture(t, ctx, store, pool, moneyPathSeedOpts{TaskCount: 1})
	taskID := f.TaskIDs[0]
	currency := f.Plan.Schedule.Currency
	if currency == "" {
		currency = SettlementCurrencyCode()
	}
	if _, err := pool.Exec(ctx, `
		INSERT INTO jobs (
		  id,buyer_id,status,job_type,model_ref,input_ref,task_count,tasks_done,
		  offered_rate_usd_hr,min_memory_gb,tier,estimated_usd,actual_usd,
		  firm_quote,sla_premium_usd,currency)
		VALUES ($1,$2,'queued','embed','all-minilm-l6-v2','money/input',1,0,
		        0,0,'batch',$3,0,false,0,$4)`,
		f.JobID, f.BuyerID, f.Plan.InitialBuyerChargeUSD, currency); err != nil {
		t.Fatalf("insert queued job: %v", err)
	}
	if _, err := pool.Exec(ctx, `
		INSERT INTO tasks (id,job_id,status,input_ref,result_key,chunk_index,retry_count)
		VALUES ($1,$2,'queued','money/input',$3,0,0)`,
		taskID, f.JobID, taskAttemptResultKey(f.JobID, taskID, 0)); err != nil {
		t.Fatalf("insert queued task: %v", err)
	}

	t.Log("FAULT: CancelJob on a queued job that has never been claimed")
	mustf(t, store.CancelJob(ctx, f.JobID, f.BuyerID), "CancelJob: %v")

	var jobStatus, taskStatus string
	var claimed *uuid.UUID
	mustf(t, pool.QueryRow(ctx, `SELECT status FROM jobs WHERE id=$1`, f.JobID).
		Scan(&jobStatus), "job status: %v")
	mustf(t, pool.QueryRow(ctx, `SELECT status, claimed_by FROM tasks WHERE id=$1`, taskID).
		Scan(&taskStatus, &claimed), "task status: %v")
	money := readLoopFaultMoney(t, ctx, pool, f.JobID)
	t.Logf("LEDGER: buyer_charge_rows=%d supplier_credit_rows=%d job=%s task=%s claimed=%v",
		money.buyerChargeRows, money.supplierCreditRows, jobStatus, taskStatus, claimed != nil)

	if jobStatus != "cancelled" {
		t.Errorf("job status=%q, want cancelled", jobStatus)
	}
	if taskStatus != "failed" && taskStatus != "cancelled" {
		t.Errorf("task status=%q, want a terminal failed/cancelled", taskStatus)
	}
	if claimed != nil {
		t.Error("cancelled-before-execution task still holds a claim")
	}
	if money.buyerChargeRows != 0 || money.buyerChargeMicros != 0 {
		t.Errorf("buyer was charged %d micros before any execution", money.buyerChargeMicros)
	}
	if money.supplierCreditRows != 0 || money.supplierCreditMicros != 0 {
		t.Errorf("supplier was credited %d micros for work that never ran", money.supplierCreditMicros)
	}
	assertLoopFaultMoneyInvariants(t, ctx, pool, f.JobID)
}

// A task that dies mid-flight leaves no partial charge. The retry path
// terminates it and the ledger still balances.
func TestLoopFaultFailureDuringExecution(t *testing.T) {
	ctx, store, pool := openIsolatedMoneyPathStore(t)
	f := seedMoneyPathFixture(t, ctx, store, pool, moneyPathSeedOpts{
		TaskCount: 1, TaskStatus: "running", ClaimWorker: true, SeedJob: true, SeedPlanRows: true,
	})
	taskID := f.TaskIDs[0]
	if _, err := pool.Exec(ctx,
		`UPDATE tasks SET claimed_at=now(), started_at=now() WHERE id=$1`, taskID); err != nil {
		t.Fatal(err)
	}

	t.Log("FAULT: FailTaskTx(timeout) mid-flight, then retry until terminal")
	report := FailureReport{
		Class: "timeout", Message: "worker died mid-embedding",
		Backend: "embed", Model: "all-minilm-l6-v2", DurationMS: 2500,
	}
	outcome, err := store.FailTaskTx(ctx, taskID, f.WorkerID, 0, report)
	if err != nil || outcome != FailRequeued {
		t.Fatalf("first mid-flight failure = (%q,%v), want requeued", outcome, err)
	}
	moneyAfterRetry := readLoopFaultMoney(t, ctx, pool, f.JobID)
	if moneyAfterRetry.buyerChargeRows != 0 || moneyAfterRetry.supplierCreditRows != 0 {
		t.Errorf("retryable mid-flight failure wrote money: buyer_charge=%d supplier_credit=%d",
			moneyAfterRetry.buyerChargeRows, moneyAfterRetry.supplierCreditRows)
	}

	var last FailOutcome
	for i := 0; i < maxTaskRetries+2; i++ {
		attempt := reattachRunningClaim(t, ctx, pool, taskID, f.WorkerID)
		last, err = store.FailTaskTx(ctx, taskID, f.WorkerID, attempt, report)
		mustf(t, err, "FailTaskTx attempt %d: %v", attempt)
		if last == FailTerminal {
			break
		}
		if last != FailRequeued {
			t.Fatalf("FailTaskTx outcome=%q, want requeued or terminal", last)
		}
	}
	if last != FailTerminal {
		t.Fatalf("retry path never terminated (last=%q)", last)
	}

	var jobStatus, taskStatus string
	var claimed *uuid.UUID
	mustf(t, pool.QueryRow(ctx, `SELECT status FROM jobs WHERE id=$1`, f.JobID).
		Scan(&jobStatus), "job status: %v")
	mustf(t, pool.QueryRow(ctx, `SELECT status, claimed_by FROM tasks WHERE id=$1`, taskID).
		Scan(&taskStatus, &claimed), "task status: %v")
	money := readLoopFaultMoney(t, ctx, pool, f.JobID)
	t.Logf("LEDGER: buyer_charge_rows=%d supplier_credit_rows=%d job=%s task=%s claimed=%v",
		money.buyerChargeRows, money.supplierCreditRows, jobStatus, taskStatus, claimed != nil)

	if jobStatus != "failed" {
		t.Errorf("job status=%q, want failed", jobStatus)
	}
	if taskStatus != "failed" {
		t.Errorf("task status=%q, want failed", taskStatus)
	}
	if claimed != nil {
		t.Error("terminated task still holds a claim")
	}
	if money.buyerChargeRows != 0 || money.buyerChargeMicros != 0 {
		t.Errorf("partial buyer_charge of %d micros after mid-flight death", money.buyerChargeMicros)
	}
	if money.supplierCreditRows != 0 || money.supplierCreditMicros != 0 {
		t.Errorf("supplier credited %d micros for a task that never committed", money.supplierCreditMicros)
	}
	assertLoopFaultMoneyInvariants(t, ctx, pool, f.JobID)
}

// Crash between verification passing and settlement committing must not
// double-charge on recovery and must not drop the supplier's entitlement.
func TestLoopFaultFailureAfterVerification(t *testing.T) {
	c := newLoopFaultCase(t, 1)
	taskID := c.f.TaskIDs[0]
	c.commit(t, taskID)

	t.Log("FAULT: panic at BoundaryAcceptedAfterLedger (after ledger inserts, before commit)")
	crash := &crashAtBoundary{at: BoundaryAcceptedAfterLedger}
	c.h.processor.probe = crash
	panicked := false
	func() {
		defer func() {
			if r := recover(); r != nil {
				panicked = true
				t.Logf("simulated crash: %v", r)
			}
		}()
		_, _ = c.h.processor.ProcessAttempt(c.ctx, taskID, 0)
	}()
	if !panicked {
		t.Fatal("expected simulated crash at BoundaryAcceptedAfterLedger")
	}
	if !crash.hit {
		t.Fatal("crash probe never reached BoundaryAcceptedAfterLedger")
	}

	rolled := readLoopFaultMoney(t, c.ctx, c.h.pool(), c.f.JobID)
	t.Logf("LEDGER after crash (before recovery): buyer_charge_rows=%d supplier_credit_rows=%d",
		rolled.buyerChargeRows, rolled.supplierCreditRows)
	if rolled.buyerChargeRows != 0 || rolled.supplierCreditRows != 0 {
		t.Fatalf("crash left durable money: buyer_charge=%d supplier_credit=%d",
			rolled.buyerChargeRows, rolled.supplierCreditRows)
	}

	if _, err := c.h.pool().Exec(c.ctx, `
		UPDATE verification_work
		   SET lease_expires_at=now()-interval '1 second'
		 WHERE task_id=$1 AND status='leased'`, taskID); err != nil {
		t.Fatalf("expire abandoned verification lease: %v", err)
	}

	c.h.processor.probe = nil
	result := c.processAttempt(t, taskID)
	if result.Outcome == OutcomeFail {
		t.Fatalf("recovery verification failed: %+v", result)
	}

	money := readLoopFaultMoney(t, c.ctx, c.h.pool(), c.f.JobID)
	t.Logf("LEDGER after recovery: buyer_charge_rows=%d micros=%d supplier_credit_rows=%d micros=%d",
		money.buyerChargeRows, money.buyerChargeMicros, money.supplierCreditRows, money.supplierCreditMicros)
	if money.buyerChargeRows != 1 {
		t.Errorf("after recovery: %d buyer_charge rows, want 1 (no double charge)", money.buyerChargeRows)
	}
	if money.supplierCreditRows != 1 {
		t.Errorf("after recovery: %d supplier_credit rows, want 1 (supplier entitlement not dropped)",
			money.supplierCreditRows)
	}
	if money.supplierCreditMicros <= 0 {
		t.Errorf("supplier entitlement micros=%d, want a credit", money.supplierCreditMicros)
	}
	assertLoopFaultMoneyInvariants(t, c.ctx, c.h.pool(), c.f.JobID)
}

// A worker that vanishes mid-task is reclaimed by the workers' stale path and
// is not credited for work it did not commit.
func TestLoopFaultSupplierDisappearance(t *testing.T) {
	ctx, store, pool := openIsolatedMoneyPathStore(t)
	f := seedMoneyPathFixture(t, ctx, store, pool, moneyPathSeedOpts{
		TaskCount: 1, TaskStatus: "running", ClaimWorker: true, SeedJob: true, SeedPlanRows: true,
	})
	taskID := f.TaskIDs[0]
	if _, err := pool.Exec(ctx, `
		UPDATE tasks SET claimed_at=now()-make_interval(secs => $2), started_at=now()-make_interval(secs => $2)
		 WHERE id=$1`,
		taskID, (staleTaskTimeout + time.Minute).Seconds()); err != nil {
		t.Fatalf("age claim: %v", err)
	}
	if _, err := pool.Exec(ctx, `
		UPDATE workers SET last_seen_at=now()-make_interval(secs => $2) WHERE id=$1`,
		f.WorkerID, (staleTaskTimeout + time.Minute).Seconds()); err != nil {
		t.Fatalf("age vanished worker: %v", err)
	}

	t.Log("FAULT: worker last_seen and claim aged past staleTaskTimeout; Workers.requeueStaleTasks")
	wk := NewWorkers(store, nil, nil)
	mustf(t, wk.requeueStaleTasks(ctx), "requeueStaleTasks: %v")

	var status string
	var retries int
	var claimed, execSupplier *uuid.UUID
	mustf(t, pool.QueryRow(ctx, `
		SELECT status, COALESCE(retry_count,0), claimed_by, execution_supplier_id
		  FROM tasks WHERE id=$1`, taskID).
		Scan(&status, &retries, &claimed, &execSupplier), "task after stale reclaim: %v")
	money := readLoopFaultMoney(t, ctx, pool, f.JobID)
	t.Logf("LEDGER: buyer_charge_rows=%d supplier_credit_rows=%d status=%s retries=%d claimed=%v",
		money.buyerChargeRows, money.supplierCreditRows, status, retries, claimed != nil)

	if status != "queued" {
		t.Errorf("task status=%q after stale reclaim, want queued", status)
	}
	if claimed != nil {
		t.Error("vanished worker still holds the claim")
	}
	if retries != 1 {
		t.Errorf("retry_count=%d, want 1 (one reclaim per disappearance)", retries)
	}
	if money.buyerChargeRows != 0 || money.buyerChargeMicros != 0 {
		t.Errorf("buyer charged %d micros for uncommitted work", money.buyerChargeMicros)
	}
	if money.supplierCreditRows != 0 || money.supplierCreditMicros != 0 {
		t.Errorf("vanished supplier credited %d micros without a commit", money.supplierCreditMicros)
	}
	if execSupplier != nil && *execSupplier == f.SupplierID && money.supplierCreditMicros != 0 {
		t.Error("vanished supplier kept a standing credit")
	}

	// A second sweep against the now-unclaimed task must not keep incrementing
	// retries, and must not mint money.
	mustf(t, wk.requeueStaleTasks(ctx), "second requeueStaleTasks: %v")
	var retriesAfter int
	mustf(t, pool.QueryRow(ctx, `SELECT retry_count FROM tasks WHERE id=$1`, taskID).
		Scan(&retriesAfter), "retry after second sweep: %v")
	if retriesAfter != 1 {
		t.Errorf("second stale sweep pushed retry_count to %d", retriesAfter)
	}
	assertLoopFaultMoneyInvariants(t, ctx, pool, f.JobID)
}
