package main

import (
	"context"
	"math"
	"os"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

func TestRuntimeCatalogPriceIsStableAcrossMigration(t *testing.T) {
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
		t.Fatalf("first migration: %v", err)
	}
	results := RepriceCatalogueFromSupplierEconomics(0.97)
	updated, err := store.ApplyRepricing(ctx, results)
	if err != nil {
		t.Fatalf("apply measured schedule: updated=%d err=%v", updated, err)
	}
	// The disposable URL may be reused by a preceding race suite. In that case
	// measured prices are deliberately immutable and ApplyRepricing is a no-op;
	// require the complete measured state rather than resetting provenance.
	if updated == 0 {
		var measured int
		if err := pool.QueryRow(ctx, `
			SELECT COUNT(*) FROM models
			 WHERE id = ANY($1::text[])
			   AND price_source = 'measured_supplier_economics'
			   AND COALESCE(price_formula,'') <> ''`,
			[]string{results[0].ModelID, results[1].ModelID}).Scan(&measured); err != nil {
			t.Fatalf("read existing measured schedule: %v", err)
		}
		if measured != len(results) {
			t.Fatalf("apply measured schedule updated no rows and found only %d/%d measured models", measured, len(results))
		}
	}

	type priceState struct {
		price   float64
		source  string
		formula string
	}
	read := func() map[string]priceState {
		rows, err := pool.Query(ctx, `SELECT id,price_per_1k,COALESCE(price_source,''),COALESCE(price_formula,'') FROM models ORDER BY id`)
		if err != nil {
			t.Fatalf("read prices: %v", err)
		}
		defer rows.Close()
		out := make(map[string]priceState)
		for rows.Next() {
			var id string
			var state priceState
			if err := rows.Scan(&id, &state.price, &state.source, &state.formula); err != nil {
				t.Fatalf("scan price: %v", err)
			}
			out[id] = state
		}
		return out
	}
	before := read()
	if err := store.Migrate(ctx); err != nil {
		t.Fatalf("repeat migration: %v", err)
	}
	after := read()
	if len(before) != len(after) {
		t.Fatalf("catalog size changed: before=%d after=%d", len(before), len(after))
	}
	for id, want := range before {
		got := after[id]
		if math.Abs(got.price-want.price) > 1e-12 || got.source != want.source || got.formula != want.formula {
			t.Fatalf("%s changed across restart: before=%+v after=%+v", id, want, got)
		}
	}
}
