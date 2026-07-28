package main

import (
	"context"
	"math"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

func TestRuntimeCatalogPriceIsStableAcrossMigration(t *testing.T) {
	databaseURL := requireTestDatabase(t)
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
	schedule, err := BuildCataloguePriceSchedule(0.97)
	if err != nil {
		t.Fatalf("build catalogue schedule: %v", err)
	}
	updated, err := store.ApplyRepricing(ctx, schedule)
	if err != nil {
		t.Fatalf("apply measured schedule: updated=%d err=%v", updated, err)
	}
	if updated != 0 && updated != len(schedule.Results) {
		t.Fatalf("atomic schedule updated %d/%d model rows", updated, len(schedule.Results))
	}

	type priceState struct {
		price           float64
		source          string
		formula         string
		reference       string
		referencePrice  float64
		currency        string
		scheduleSHA256  string
		scheduleVersion int
	}
	read := func() map[string]priceState {
		rows, err := pool.Query(ctx, `
			SELECT id,price_per_1k,COALESCE(price_source,''),COALESCE(price_formula,''),
			       COALESCE(price_reference_currency,''),COALESCE(price_reference_per_1k,0),
			       COALESCE(price_currency,''),
			       COALESCE(price_schedule_sha256,''),
			       COALESCE(price_schedule_version,0)
			  FROM models ORDER BY id`)
		if err != nil {
			t.Fatalf("read prices: %v", err)
		}
		defer rows.Close()
		out := make(map[string]priceState)
		for rows.Next() {
			var id string
			var state priceState
			if err := rows.Scan(
				&id, &state.price, &state.source, &state.formula,
				&state.reference, &state.referencePrice, &state.currency,
				&state.scheduleSHA256, &state.scheduleVersion,
			); err != nil {
				t.Fatalf("scan price: %v", err)
			}
			out[id] = state
		}
		return out
	}
	before := read()
	for _, result := range schedule.Results {
		state := before[result.ModelID]
		if math.Abs(state.price-result.PricePer1K) > 1e-12 ||
			state.source != "market_board" ||
			state.formula != result.Formula ||
			state.reference != schedule.ReferenceCurrency ||
			math.Abs(state.referencePrice-result.ReferencePricePer1K) > 1e-12 ||
			state.currency != schedule.SettlementCurrency ||
			state.scheduleSHA256 != schedule.SHA256 ||
			state.scheduleVersion != schedule.Version {
			t.Fatalf("%s not bound to exact catalogue schedule: %+v", result.ModelID, state)
		}
		authority, err := store.LoadCataloguePriceAuthority(ctx, result.ModelID)
		if err != nil {
			t.Fatalf("load composite price authority for %s: %v", result.ModelID, err)
		}
		if authority.ScheduleSHA256 != schedule.SHA256 ||
			authority.BoardSHA256 != schedule.BoardSHA256 ||
			authority.FXRevision != schedule.FXRevision ||
			authority.SupplierShare != schedule.SupplierShare ||
			authority.ReferencePricePer1K != result.ReferencePricePer1K ||
			authority.SettlementPricePer1K != result.PricePer1K {
			t.Fatalf("%s composite authority differs from append-only schedule: %+v",
				result.ModelID, authority)
		}
	}
	if _, err := pool.Exec(ctx, `
		UPDATE models SET price_per_1k=price_per_1k+0.00000001 WHERE id=$1`,
		schedule.Results[0].ModelID,
	); err == nil {
		t.Fatal("direct mutation of schedule-bound model price was accepted")
	}
	if err := store.Migrate(ctx); err != nil {
		t.Fatalf("repeat migration: %v", err)
	}
	after := read()
	if len(before) != len(after) {
		t.Fatalf("catalog size changed: before=%d after=%d", len(before), len(after))
	}
	for id, want := range before {
		got := after[id]
		if math.Abs(got.price-want.price) > 1e-12 ||
			got.source != want.source || got.formula != want.formula ||
			got.reference != want.reference ||
			math.Abs(got.referencePrice-want.referencePrice) > 1e-12 ||
			got.currency != want.currency ||
			got.scheduleSHA256 != want.scheduleSHA256 ||
			got.scheduleVersion != want.scheduleVersion {
			t.Fatalf("%s changed across restart: before=%+v after=%+v", id, want, got)
		}
	}
	replayed, err := store.ApplyRepricing(ctx, schedule)
	if err != nil {
		t.Fatalf("idempotent schedule replay: %v", err)
	}
	if replayed != 0 {
		t.Fatalf("idempotent schedule replay rewrote %d model rows", replayed)
	}
	var schedules, history int
	if err := pool.QueryRow(ctx,
		`SELECT count(*) FROM catalogue_price_schedules WHERE sha256=$1`,
		schedule.SHA256,
	).Scan(&schedules); err != nil {
		t.Fatal(err)
	}
	if err := pool.QueryRow(ctx,
		`SELECT count(*) FROM model_price_history WHERE schedule_sha256=$1`,
		schedule.SHA256,
	).Scan(&history); err != nil {
		t.Fatal(err)
	}
	if schedules != 1 || history != len(schedule.Results) {
		t.Fatalf("schedule audit rows schedules=%d history=%d want 1/%d",
			schedules, history, len(schedule.Results))
	}
	var referenceCurrency, settlementCurrency, fxRevision string
	var fxRate float64
	if err := pool.QueryRow(ctx, `
		SELECT reference_currency,settlement_currency,reference_to_settlement_rate,fx_revision
		  FROM catalogue_price_schedules WHERE sha256=$1`,
		schedule.SHA256,
	).Scan(&referenceCurrency, &settlementCurrency, &fxRate, &fxRevision); err != nil {
		t.Fatal(err)
	}
	if referenceCurrency != schedule.ReferenceCurrency ||
		settlementCurrency != schedule.SettlementCurrency ||
		fxRate != schedule.ReferenceToSettlement ||
		fxRevision != schedule.FXRevision {
		t.Fatalf("durable FX authority differs from schedule: %s %s %v %s",
			referenceCurrency, settlementCurrency, fxRate, fxRevision)
	}
}

func TestApplyRepricingRollsBackEveryModelWhenOneTargetUpdateFails(t *testing.T) {
	databaseURL := requireTestDatabase(t)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	pool, err := pgxpool.New(ctx, databaseURL)
	if err != nil {
		t.Fatalf("connect disposable PostgreSQL: %v", err)
	}
	defer pool.Close()
	store := NewStore(pool)
	if err := store.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	schedule, err := BuildCataloguePriceSchedule(0.97)
	if err != nil {
		t.Fatal(err)
	}
	// Use a distinct valid schedule identity so this test proves a new apply,
	// even when a prior suite run already installed the shipped schedule.
	schedule.BoardFetchedAt += "-rollback-test"
	for i := range schedule.Results {
		schedule.Results[i].Formula += " rollback_test=1"
	}
	schedule.SHA256, err = cataloguePriceScheduleDigest(schedule)
	if err != nil {
		t.Fatal(err)
	}
	if err := validateCataloguePriceSchedule(schedule); err != nil {
		t.Fatal(err)
	}

	first := schedule.Results[0].ModelID
	var beforePrice float64
	var beforeSchedule string
	if err := pool.QueryRow(ctx, `
		SELECT price_per_1k,COALESCE(price_schedule_sha256,'')
		  FROM models WHERE id=$1`, first,
	).Scan(&beforePrice, &beforeSchedule); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `
		CREATE OR REPLACE FUNCTION cx_test_reject_second_catalogue_price() RETURNS trigger AS $$
		BEGIN
		    IF NEW.id='llama-3.2-1b-instruct-q4' THEN
		        RAISE EXCEPTION 'forced second-model repricing failure';
		    END IF;
		    RETURN NEW;
		END;
		$$ LANGUAGE plpgsql;
		DROP TRIGGER IF EXISTS test_reject_second_catalogue_price ON models;
		CREATE TRIGGER test_reject_second_catalogue_price
		    BEFORE UPDATE ON models
		    FOR EACH ROW EXECUTE FUNCTION cx_test_reject_second_catalogue_price()`); err != nil {
		t.Fatal(err)
	}
	defer func() {
		cleanupCtx, cleanupCancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cleanupCancel()
		_, _ = pool.Exec(cleanupCtx, `
			DROP TRIGGER IF EXISTS test_reject_second_catalogue_price ON models;
			DROP FUNCTION IF EXISTS cx_test_reject_second_catalogue_price()`)
	}()

	if updated, err := store.ApplyRepricing(ctx, schedule); err == nil {
		t.Fatalf("forced target failure accepted after updating %d rows", updated)
	}
	var afterPrice float64
	var afterSchedule string
	if err := pool.QueryRow(ctx, `
		SELECT price_per_1k,COALESCE(price_schedule_sha256,'')
		  FROM models WHERE id=$1`, first,
	).Scan(&afterPrice, &afterSchedule); err != nil {
		t.Fatal(err)
	}
	if math.Abs(afterPrice-beforePrice) > 1e-12 || afterSchedule != beforeSchedule {
		t.Fatalf("failed schedule partially changed first model: before=%v/%q after=%v/%q",
			beforePrice, beforeSchedule, afterPrice, afterSchedule)
	}
	var scheduleRows, historyRows int
	if err := pool.QueryRow(ctx,
		`SELECT count(*) FROM catalogue_price_schedules WHERE sha256=$1`, schedule.SHA256,
	).Scan(&scheduleRows); err != nil {
		t.Fatal(err)
	}
	if err := pool.QueryRow(ctx,
		`SELECT count(*) FROM model_price_history WHERE schedule_sha256=$1`, schedule.SHA256,
	).Scan(&historyRows); err != nil {
		t.Fatal(err)
	}
	if scheduleRows != 0 || historyRows != 0 {
		t.Fatalf("failed schedule left audit rows schedules=%d history=%d", scheduleRows, historyRows)
	}
}
