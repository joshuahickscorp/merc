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
			`{"id":"pi_decline_recovery","latest_charge":{"id":"ch_decline_recovery"},"status":"succeeded","currency":%q,"amount":%d,"amount_received":%d}`,
			currency, cents, cents)
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
