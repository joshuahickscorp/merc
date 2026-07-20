package main

import (
	"context"
	"os"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
)

func TestListWorkersToleratesLegacyNullTelemetry(t *testing.T) {
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

	supplierID, workerID := uuid.New(), uuid.New()
	if _, err := pool.Exec(ctx, `
		INSERT INTO suppliers (id,email,reputation,tier,status)
		VALUES ($1,$2,NULL,NULL,NULL)`,
		supplierID, supplierID.String()+"@legacy.invalid"); err != nil {
		t.Fatalf("insert legacy supplier: %v", err)
	}
	if _, err := pool.Exec(ctx, `
		INSERT INTO workers (id,supplier_id,hw_class,memory_gb,last_seen_at,version)
		VALUES ($1,$2,'apple_silicon_pro',NULL,now(),'legacy-seed')`,
		workerID, supplierID); err != nil {
		t.Fatalf("insert legacy worker: %v", err)
	}
	t.Cleanup(func() {
		cleanupCtx, cleanupCancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cleanupCancel()
		_, _ = pool.Exec(cleanupCtx, `DELETE FROM workers WHERE id=$1`, workerID)
		_, _ = pool.Exec(cleanupCtx, `DELETE FROM suppliers WHERE id=$1`, supplierID)
	})

	workers, err := store.ListWorkers(ctx)
	if err != nil {
		t.Fatalf("list workers with nullable legacy telemetry: %v", err)
	}
	for _, worker := range workers {
		if worker.ID == workerID {
			if worker.MemoryGB != 0 || worker.Reputation != 0 || worker.Tier != 0 || worker.Status != "pending" {
				t.Fatalf("legacy worker defaults = %+v", worker)
			}
			return
		}
	}
	t.Fatal("legacy worker was absent from the admin inventory")
}
