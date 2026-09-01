package main

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
)

// TestBillingCustomerCanonicalSchema guards the fresh-database billing path.
// CI and prove-local opt in with a disposable PostgreSQL URL; ordinary unit
// test runs remain hermetic.
func TestBillingCustomerCanonicalSchema(t *testing.T) {
	databaseURL := requireTestDatabase(t)

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	pool, err := pgxpool.New(ctx, databaseURL)
	mustf(t, err, "connect disposable PostgreSQL: %v")
	defer pool.Close()
	store := NewStore(pool)
	mustf(t, store.Migrate(ctx), "apply canonical schema: %v")

	buyerID := uuid.New()
	jobID := uuid.New()
	customerID := "cus_schema_" + buyerID.String()
	if _, err := pool.Exec(ctx,
		`INSERT INTO buyers (id,email) VALUES ($1,$2)`,
		buyerID, buyerID.String()+"@schema.invalid"); err != nil {
		t.Fatalf("insert buyer: %v", err)
	}
	t.Cleanup(func() {
		cleanupCtx, cleanupCancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cleanupCancel()
		_, _ = pool.Exec(cleanupCtx, `DELETE FROM jobs WHERE id=$1`, jobID)
		_, _ = pool.Exec(cleanupCtx, `DELETE FROM buyers WHERE id=$1`, buyerID)
	})

	mustf(t, store.UpsertBillingCustomer(ctx, buyerID, customerID), "upsert billing customer: %v")
	if err := store.UpsertBillingCustomer(ctx, buyerID, "cus_card_replacement"); !errors.Is(err, errBillingCustomerIdentityMismatch) {
		t.Fatalf("replacement billing customer err=%v, want %v", err, errBillingCustomerIdentityMismatch)
	}
	customer, paymentMethod, err := store.GetBillingCustomer(ctx, buyerID)
	if err != nil || customer != customerID || paymentMethod != "" {
		t.Fatalf("initial billing customer = (%q,%q,%v)", customer, paymentMethod, err)
	}
	mustf(t, store.SetBillingPMByCustomer(ctx, customerID, "pm_schema_test"), "set default payment method: %v")
	_, paymentMethod, err = store.GetBillingCustomer(ctx, buyerID)
	if err != nil || paymentMethod != "pm_schema_test" {
		t.Fatalf("saved payment method = (%q,%v)", paymentMethod, err)
	}

	if _, err := pool.Exec(ctx, `
		INSERT INTO jobs
		    (id,buyer_id,status,job_type,input_ref,actual_usd,currency,charge_status)
		VALUES ($1,$2,'complete','embed','schema/input',1.00,$3,'no_payment_method')`,
		jobID, buyerID, SettlementCurrencyCode()); err != nil {
		t.Fatalf("insert no-card job: %v", err)
	}
	changed, err := store.ReflipNoCardJobs(ctx)
	mustf(t, err, "re-enable no-card jobs: %v")
	if changed < 1 {
		t.Fatalf("re-enabled jobs = %d, want at least 1", changed)
	}
	var chargeStatus string
	mustf(t, pool.QueryRow(ctx, `SELECT charge_status FROM jobs WHERE id=$1`, jobID).Scan(&chargeStatus), "read charge status: %v")
	if chargeStatus != "deferred" {
		t.Fatalf("charge status = %q, want deferred", chargeStatus)
	}
}

func TestSavedCardWebhookOrderingAndExpandableReferences(t *testing.T) {
	ctx, store, pool := openIsolatedTestStore(t)
	defer pool.Close()
	buyerID := insertTestBuyer(t, pool, ctx)
	customerID := "cus_card_order_" + buyerID.String()[:8]
	mustf(t, store.UpsertBillingCustomer(ctx, buyerID, customerID), "upsert billing customer: %v")

	const secret = "whsec_card_ordering"
	handler := func(payload []byte) *httptest.ResponseRecorder {
		req := signedStripeCashRequest(t, payload, secret)
		rec := httptest.NewRecorder()
		handleStripeWebhookWithAllHandlersAtModeAndRiskAndPaymentFailureAndPMEvent(
			rec, req, secret, nil, nil, nil, false, nil, nil,
			store.SetBillingPMByCustomerEvent,
		)
		return rec
	}

	newPayload, err := json.Marshal(map[string]any{
		"id": "evt_card_new", "type": "setup_intent.succeeded",
		"api_version": stripeAPIVersion, "livemode": false, "created": int64(1_700_000_002),
		"data": map[string]any{"object": map[string]any{
			"object":         "setup_intent",
			"id":             "seti_card_new",
			"customer":       map[string]any{"object": "customer", "id": customerID},
			"payment_method": map[string]any{"object": "payment_method", "id": "pm_card_new"},
		}},
	})
	mustf(t, err, "marshal new card event: %v")
	if response := handler(newPayload); response.Code != http.StatusOK {
		t.Fatalf("new card webhook status=%d, want 200", response.Code)
	}

	oldPayload, err := json.Marshal(map[string]any{
		"id": "evt_card_old", "type": "payment_method.attached",
		"api_version": stripeAPIVersion, "livemode": false, "created": int64(1_700_000_001),
		"data": map[string]any{"object": map[string]any{
			"object": "payment_method", "id": "pm_card_old",
			"customer": map[string]any{"object": "customer", "id": customerID},
		}},
	})
	mustf(t, err, "marshal old card event: %v")
	if response := handler(oldPayload); response.Code != http.StatusOK {
		t.Fatalf("old card webhook status=%d, want 200", response.Code)
	}
	conflictPayload, err := json.Marshal(map[string]any{
		"id": "evt_card_old", "type": "payment_method.attached",
		"api_version": stripeAPIVersion, "livemode": false, "created": int64(1_700_000_001),
		"data": map[string]any{"object": map[string]any{
			"object": "payment_method", "id": "pm_card_conflict",
			"customer": map[string]any{"object": "customer", "id": customerID},
		}},
	})
	mustf(t, err, "marshal conflicting card event: %v")
	if response := handler(conflictPayload); response.Code != http.StatusInternalServerError {
		t.Fatalf("conflicting card webhook status=%d, want 500", response.Code)
	}
	_, paymentMethod, err := store.GetBillingCustomer(ctx, buyerID)
	mustf(t, err, "read ordered payment method: %v")
	if paymentMethod != "pm_card_new" {
		t.Fatalf("ordered payment method=%q, want pm_card_new", paymentMethod)
	}
	var eventRows int
	mustf(t, pool.QueryRow(ctx, `SELECT count(*) FROM stripe_payment_method_events`).Scan(&eventRows),
		"count payment-method events: %v")
	if eventRows != 2 {
		t.Fatalf("payment-method event rows=%d, want 2", eventRows)
	}
}
