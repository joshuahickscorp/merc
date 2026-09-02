package main

import (
	"errors"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"testing"

	"github.com/google/uuid"
)

func TestBuyerChargeExplicitDeclineRearmsWithFreshProviderKey(t *testing.T) {
	ctx, store, pool := openIsolatedTestStore(t)
	buyerID, jobID := uuid.New(), uuid.New()
	operationKey := "job-" + jobID.String()
	currency := SettlementCurrencyCode()
	settlement, err := SettlementCurrency()
	mustf(t, err, "settlement currency: %v")
	cents, err := settlement.MajorToMinor(5)
	mustf(t, err, "charge amount: %v")

	_, err = pool.Exec(ctx, `INSERT INTO buyers (id,email) VALUES ($1,$2)`,
		buyerID, buyerID.String()+"@charge.invalid")
	mustf(t, err, "insert buyer: %v")
	_, err = pool.Exec(ctx, `
		INSERT INTO jobs (id,buyer_id,status,job_type,input_ref,actual_usd,currency,charge_status)
		VALUES ($1,$2,'complete','embed','charge/input',5.00,$3,'not_attempted')`,
		jobID, buyerID, currency)
	mustf(t, err, "insert job: %v")
	mustf(t, store.UpsertBillingCustomer(ctx, buyerID, "cus_decline_recovery"),
		"upsert billing customer: %v")
	mustf(t, store.SetBillingPMByCustomer(ctx, "cus_decline_recovery", "pm_decline_recovery"),
		"set payment method: %v")

	var (
		mu             sync.Mutex
		providerKeys   []string
		operationKeys  []string
		paymentMethods []string
		paymentAttempt int
	)
	withStripeTestServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/payment_intents" {
			t.Errorf("Stripe request = %s %s, want POST /payment_intents", r.Method, r.URL.Path)
		}
		if err := r.ParseForm(); err != nil {
			t.Errorf("ParseForm: %v", err)
		}
		mu.Lock()
		providerKeys = append(providerKeys, r.Header.Get("Idempotency-Key"))
		operationKeys = append(operationKeys, r.Form.Get("metadata[cx_operation_key]"))
		paymentMethods = append(paymentMethods, r.Form.Get("payment_method"))
		paymentAttempt++
		attempt := paymentAttempt
		mu.Unlock()

		w.Header().Set("Content-Type", "application/json")
		if attempt == 1 {
			w.WriteHeader(http.StatusPaymentRequired)
			_, _ = fmt.Fprint(w, `{"error":{"type":"card_error","code":"card_declined","message":"declined"}}`)
			return
		}
		_, _ = fmt.Fprintf(w,
			`{"object":"payment_intent","id":"pi_decline_recovery","customer":%q,"payment_method":%q,"latest_charge":{"object":"charge","id":"ch_decline_recovery"},"status":"succeeded","currency":%q,"amount":%d,"amount_received":%d}`,
			r.Form.Get("customer"), r.Form.Get("payment_method"), currency, cents, cents)
	}))

	if _, err := chargeBuyer(ctx, store, buyerID, 5, operationKey, "job", jobID); !errors.Is(err, errBuyerChargeDefinitelyFailed) {
		t.Fatalf("first charge error = %v, want definite failure", err)
	}
	var operationStatus, jobStatus string
	var providerAttempt int64
	mustf(t, pool.QueryRow(ctx, `
		SELECT status,provider_attempt FROM buyer_charge_operations WHERE operation_key=$1`, operationKey).
		Scan(&operationStatus, &providerAttempt), "read failed operation: %v")
	mustf(t, pool.QueryRow(ctx, `SELECT charge_status FROM jobs WHERE id=$1`, jobID).
		Scan(&jobStatus), "read failed job: %v")
	if operationStatus != "failed" || providerAttempt != 1 || jobStatus != "failed" {
		t.Fatalf("after decline operation=(%s,%d) job=%s, want failed/1/failed", operationStatus, providerAttempt, jobStatus)
	}

	mustf(t, store.SetBillingPMByCustomer(ctx, "cus_decline_recovery", "pm_decline_recovery_replacement"),
		"replace declined payment method: %v")
	charge, err := chargeBuyer(ctx, store, buyerID, 5, operationKey, "job", jobID)
	mustf(t, err, "retry charge: %v")
	if charge.PaymentIntentID != "pi_decline_recovery" || charge.ReceivedCents != cents {
		t.Fatalf("retry charge = %+v", charge)
	}
	mustf(t, pool.QueryRow(ctx, `
		SELECT status,provider_attempt FROM buyer_charge_operations WHERE operation_key=$1`, operationKey).
		Scan(&operationStatus, &providerAttempt), "read rearmed operation: %v")
	mustf(t, pool.QueryRow(ctx, `SELECT charge_status FROM jobs WHERE id=$1`, jobID).
		Scan(&jobStatus), "read rearmed job: %v")
	if operationStatus != "outcome_unknown" || providerAttempt != 2 || jobStatus != "outcome_unknown" {
		t.Fatalf("after rearm operation=(%s,%d) job=%s, want outcome_unknown/2/outcome_unknown", operationStatus, providerAttempt, jobStatus)
	}
	mustf(t, store.SetJobCharged(ctx, jobID, charge), "confirm retry charge: %v")

	mu.Lock()
	defer mu.Unlock()
	if len(providerKeys) != 2 || providerKeys[0] != operationKey ||
		providerKeys[1] != operationKey+"-retry-2" {
		t.Fatalf("provider idempotency keys = %q, want stable then retry key", providerKeys)
	}
	if len(operationKeys) != 2 || strings.TrimSpace(operationKeys[0]) != operationKey ||
		strings.TrimSpace(operationKeys[1]) != operationKey {
		t.Fatalf("Stripe operation metadata = %q, want stable operation key", operationKeys)
	}
	if len(paymentMethods) != 2 || paymentMethods[0] != "pm_decline_recovery" ||
		paymentMethods[1] != "pm_decline_recovery_replacement" {
		t.Fatalf("Stripe payment methods = %q, want original then replacement", paymentMethods)
	}
	mustf(t, pool.QueryRow(ctx, `
		SELECT status,provider_attempt FROM buyer_charge_operations WHERE operation_key=$1`, operationKey).
		Scan(&operationStatus, &providerAttempt), "read completed operation: %v")
	if operationStatus != "succeeded" || providerAttempt != 2 {
		t.Fatalf("completed operation=(%s,%d), want succeeded/2", operationStatus, providerAttempt)
	}
}

func TestBuyerChargeExplicitDeclineRearmsBatchBoundary(t *testing.T) {
	ctx, store, pool := openIsolatedTestStore(t)
	buyerID, batchID := uuid.New(), uuid.New()
	operationKey := "cxbatch-" + batchID.String()
	currency := SettlementCurrencyCode()

	_, err := pool.Exec(ctx, `INSERT INTO buyers (id,email) VALUES ($1,$2)`,
		buyerID, buyerID.String()+"@batch-charge.invalid")
	mustf(t, err, "insert buyer: %v")
	_, err = pool.Exec(ctx, `
		INSERT INTO charge_batches (id,buyer_id,amount_usd,currency,status)
		VALUES ($1,$2,5.00,$3,'attempting')`, batchID, buyerID, currency)
	mustf(t, err, "insert charge batch: %v")

	armed, providerKey, err := store.BeginBuyerChargeOperation(
		ctx, operationKey, "batch", batchID, buyerID,
		"cus_batch_charge", "pm_batch_charge", 500, currency,
	)
	mustf(t, err, "begin batch charge: %v")
	if !armed || providerKey != operationKey {
		t.Fatalf("first batch arm=(%v,%q), want true and stable key", armed, providerKey)
	}
	var batchStatus string
	mustf(t, pool.QueryRow(ctx, `SELECT status FROM charge_batches WHERE id=$1`, batchID).
		Scan(&batchStatus), "read armed batch: %v")
	if batchStatus != "outcome_unknown" {
		t.Fatalf("armed batch status=%q, want outcome_unknown", batchStatus)
	}

	mustf(t, store.MarkBuyerChargeDefinitelyFailed(ctx, operationKey, errors.New("card declined")),
		"mark batch decline: %v")
	mustf(t, pool.QueryRow(ctx, `SELECT status FROM charge_batches WHERE id=$1`, batchID).
		Scan(&batchStatus), "read failed batch: %v")
	if batchStatus != "attempting" {
		t.Fatalf("failed batch status=%q, want attempting for backed-off retry", batchStatus)
	}

	armed, providerKey, err = store.BeginBuyerChargeOperation(
		ctx, operationKey, "batch", batchID, buyerID,
		"cus_batch_charge", "pm_batch_charge", 500, currency,
	)
	mustf(t, err, "rearm batch charge: %v")
	if !armed || providerKey != operationKey+"-retry-2" {
		t.Fatalf("rearmed batch=(%v,%q), want true and retry-2 key", armed, providerKey)
	}
}

func TestBatchChargeConfirmationIsAtomicAndReplaySafe(t *testing.T) {
	ctx, store, pool := openIsolatedTestStore(t)
	buyerID, supplierID, batchID, jobID, taskID := uuid.New(), uuid.New(), uuid.New(), uuid.New(), uuid.New()
	currency := SettlementCurrencyCode()
	settlement, err := SettlementCurrency()
	mustf(t, err, "settlement currency: %v")
	cents, err := settlement.MajorToMinor(5)
	mustf(t, err, "charge amount: %v")
	charge := ChargeResult{
		PaymentIntentID: "pi_batch_confirmation_" + uuid.NewString(),
		ChargeID:        "ch_batch_confirmation_" + uuid.NewString(),
		RequestedCents:  cents,
		ReceivedCents:   cents,
		Currency:        currency,
	}

	for _, statement := range []struct {
		query string
		args  []any
	}{
		{`INSERT INTO buyers (id,email) VALUES ($1,$2)`,
			[]any{buyerID, buyerID.String() + "@batch-confirm.invalid"}},
		{`INSERT INTO suppliers (id,email,reputation,status) VALUES ($1,$2,0.5,'active')`,
			[]any{supplierID, supplierID.String() + "@batch-confirm.invalid"}},
		{`INSERT INTO charge_batches (id,buyer_id,amount_usd,currency,status)
		  VALUES ($1,$2,5.00,$3,'attempting')`, []any{batchID, buyerID, currency}},
		{`INSERT INTO jobs
		    (id,buyer_id,status,job_type,input_ref,actual_usd,currency,charge_status,charge_batch_id)
		  VALUES ($1,$2,'complete','embed','batch-confirm/input',5.00,$3,'not_attempted',$4)`,
			[]any{jobID, buyerID, currency, batchID}},
		{`INSERT INTO tasks (id,job_id,status,verification_outcome,completed_at)
		  VALUES ($1,$2,'complete','pass',now())`, []any{taskID, jobID}},
		{`INSERT INTO ledger_entries
		    (kind,supplier_id,task_id,amount_usd,currency,payout_status,release_at)
		  VALUES ('supplier_credit',$1,$2,1.00,$3,'awaiting_funding',now()+interval '1 hour')`,
			[]any{supplierID, taskID, currency}},
	} {
		if _, err := pool.Exec(ctx, statement.query, statement.args...); err != nil {
			t.Fatalf("seed batch confirmation: %v\nSQL: %s", err, statement.query)
		}
	}

	armed, providerKey, err := store.BeginBuyerChargeOperation(
		ctx, "cxbatch-"+batchID.String(), "batch", batchID, buyerID,
		"cus_batch_confirmation", "pm_batch_confirmation", cents, currency,
	)
	mustf(t, err, "arm batch charge: %v")
	if !armed || providerKey != "cxbatch-"+batchID.String() {
		t.Fatalf("armed batch=(%v,%q), want true and stable provider key", armed, providerKey)
	}

	mustf(t, store.MarkChargeBatchCharged(ctx, batchID, charge), "confirm batch charge: %v")
	var batchStatus, jobStatus, operationStatus, payoutStatus string
	var collectionCount int
	mustf(t, pool.QueryRow(ctx, `
		SELECT cb.status,j.charge_status,bco.status,le.payout_status,
		       (SELECT count(*) FROM buyer_cash_collections WHERE payment_intent=$1)::int
		  FROM charge_batches cb
		  JOIN jobs j ON j.charge_batch_id=cb.id
		  JOIN buyer_charge_operations bco ON bco.charge_batch_id=cb.id
		  JOIN tasks t ON t.job_id=j.id
		  JOIN ledger_entries le ON le.task_id=t.id AND le.kind='supplier_credit'
		 WHERE cb.id=$2`, charge.PaymentIntentID, batchID).
		Scan(&batchStatus, &jobStatus, &operationStatus, &payoutStatus, &collectionCount),
		"read confirmed batch: %v")
	if batchStatus != "charged" || jobStatus != "charged" || operationStatus != "succeeded" ||
		payoutStatus != PayoutHeld || collectionCount != 1 {
		t.Fatalf("confirmed batch state=(batch=%s,job=%s,operation=%s,payout=%s,collections=%d), want charged/charged/succeeded/held/1",
			batchStatus, jobStatus, operationStatus, payoutStatus, collectionCount)
	}

	// A provider replay must not create a second cash fact or re-run the
	// dependent transitions, but it must remain an accepted no-op.
	mustf(t, store.MarkChargeBatchCharged(ctx, batchID, charge), "replay batch charge: %v")
	mustf(t, pool.QueryRow(ctx, `
		SELECT count(*) FROM buyer_cash_collections WHERE payment_intent=$1`, charge.PaymentIntentID).
		Scan(&collectionCount), "count replayed batch cash: %v")
	if collectionCount != 1 {
		t.Fatalf("replayed batch cash rows=%d, want exactly one", collectionCount)
	}

	// A different provider identity must roll back the attempted canonical
	// cash insert as well as refusing the already-succeeded durable operation.
	badCharge := charge
	badCharge.PaymentIntentID = "pi_batch_confirmation_conflict_" + uuid.NewString()
	badCharge.ChargeID = "ch_batch_confirmation_conflict_" + uuid.NewString()
	if err := store.MarkChargeBatchCharged(ctx, batchID, badCharge); err == nil {
		t.Fatal("conflicting replay unexpectedly succeeded")
	}
	var storedPI string
	mustf(t, pool.QueryRow(ctx, `
		SELECT stripe_pi FROM charge_batches WHERE id=$1`, batchID).
		Scan(&storedPI), "read batch after conflicting replay: %v")
	if storedPI != charge.PaymentIntentID {
		t.Fatalf("conflicting replay changed stripe_pi=%q, want %q", storedPI, charge.PaymentIntentID)
	}
	mustf(t, pool.QueryRow(ctx, `
		SELECT count(*) FROM buyer_cash_collections WHERE payment_intent=$1`, badCharge.PaymentIntentID).
		Scan(&collectionCount), "count rolled-back conflicting cash: %v")
	if collectionCount != 0 {
		t.Fatalf("conflicting replay left %d canonical cash rows", collectionCount)
	}
}

func TestBuyerChargeFailureTransitionDoesNotCommitAfterSourceCASLoss(t *testing.T) {
	ctx, store, pool := openIsolatedTestStore(t)
	buyerID, jobID := uuid.New(), uuid.New()
	operationKey := "job-cas-" + jobID.String()
	currency := SettlementCurrencyCode()

	_, err := pool.Exec(ctx, `INSERT INTO buyers (id,email) VALUES ($1,$2)`,
		buyerID, buyerID.String()+"@charge-cas.invalid")
	mustf(t, err, "insert buyer: %v")
	_, err = pool.Exec(ctx, `
		INSERT INTO jobs (id,buyer_id,status,job_type,input_ref,actual_usd,currency,charge_status)
		VALUES ($1,$2,'complete','embed','charge-cas/input',5.00,$3,'not_attempted')`,
		jobID, buyerID, currency)
	mustf(t, err, "insert job: %v")

	armed, providerKey, err := store.BeginBuyerChargeOperation(
		ctx, operationKey, "job", jobID, buyerID,
		"cus_charge_cas", "pm_charge_cas", 500, currency,
	)
	mustf(t, err, "begin charge: %v")
	if !armed || providerKey != operationKey {
		t.Fatalf("charge arm=(%v,%q), want true and stable key", armed, providerKey)
	}
	_, err = pool.Exec(ctx, `UPDATE jobs SET charge_status='charged' WHERE id=$1`, jobID)
	mustf(t, err, "cross source charge boundary: %v")

	err = store.MarkBuyerChargeDefinitelyFailed(ctx, operationKey, errors.New("late decline"))
	if err == nil || !strings.Contains(err.Error(), "source job lost its failure-state CAS") {
		t.Fatalf("failure transition error=%v, want source CAS error", err)
	}
	var operationStatus, jobStatus string
	mustf(t, pool.QueryRow(ctx, `
		SELECT status FROM buyer_charge_operations WHERE operation_key=$1`, operationKey).
		Scan(&operationStatus), "read operation after rejected transition: %v")
	mustf(t, pool.QueryRow(ctx, `SELECT charge_status FROM jobs WHERE id=$1`, jobID).
		Scan(&jobStatus), "read job after rejected transition: %v")
	if operationStatus != "outcome_unknown" || jobStatus != "charged" {
		t.Fatalf("after rejected transition operation=%s job=%s, want outcome_unknown/charged",
			operationStatus, jobStatus)
	}
}
