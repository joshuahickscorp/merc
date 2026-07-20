package main

import (
	"context"
	"os"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
)

// TestBillingCustomerCanonicalSchema guards the fresh-database billing path.
// CI and prove-local opt in with a disposable PostgreSQL URL; ordinary unit
// test runs remain hermetic.
func TestBillingCustomerCanonicalSchema(t *testing.T) {
	databaseURL := os.Getenv("CX_TEST_DATABASE_URL")
	if databaseURL == "" {
		t.Skip("CX_TEST_DATABASE_URL is not set")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	pool, err := pgxpool.New(ctx, databaseURL)
	if err != nil {
		t.Fatalf("connect disposable PostgreSQL: %v", err)
	}
	defer pool.Close()
	store := NewStore(pool)
	if err := store.Migrate(ctx); err != nil {
		t.Fatalf("apply canonical schema: %v", err)
	}

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

	if err := store.UpsertBillingCustomer(ctx, buyerID, customerID); err != nil {
		t.Fatalf("upsert billing customer: %v", err)
	}
	customer, paymentMethod, err := store.GetBillingCustomer(ctx, buyerID)
	if err != nil || customer != customerID || paymentMethod != "" {
		t.Fatalf("initial billing customer = (%q,%q,%v)", customer, paymentMethod, err)
	}
	if err := store.SetBillingPMByCustomer(ctx, customerID, "pm_schema_test"); err != nil {
		t.Fatalf("set default payment method: %v", err)
	}
	_, paymentMethod, err = store.GetBillingCustomer(ctx, buyerID)
	if err != nil || paymentMethod != "pm_schema_test" {
		t.Fatalf("saved payment method = (%q,%v)", paymentMethod, err)
	}

	if _, err := pool.Exec(ctx, `
		INSERT INTO jobs
		    (id,buyer_id,status,job_type,input_ref,actual_usd,charge_status)
		VALUES ($1,$2,'complete','embed','schema/input',1.00,'no_payment_method')`,
		jobID, buyerID); err != nil {
		t.Fatalf("insert no-card job: %v", err)
	}
	changed, err := store.ReflipNoCardJobs(ctx)
	if err != nil {
		t.Fatalf("re-enable no-card jobs: %v", err)
	}
	if changed < 1 {
		t.Fatalf("re-enabled jobs = %d, want at least 1", changed)
	}
	var chargeStatus string
	if err := pool.QueryRow(ctx, `SELECT charge_status FROM jobs WHERE id=$1`, jobID).Scan(&chargeStatus); err != nil {
		t.Fatalf("read charge status: %v", err)
	}
	if chargeStatus != "deferred" {
		t.Fatalf("charge status = %q, want deferred", chargeStatus)
	}
}
