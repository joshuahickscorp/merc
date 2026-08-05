package main

import (
	"context"
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
