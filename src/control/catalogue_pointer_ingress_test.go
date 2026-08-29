package main

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"testing"
	"time"

	"github.com/google/uuid"
)

// governedDifferentCatalogueSchedule builds a second, fully governed schedule,
// then restores this process's old board view. The returned object models a
// reprice committed by another control process while this one still holds a
// decision derived from the prior schedule.
func governedDifferentCatalogueSchedule(
	t *testing.T, oldScheduleSHA string,
) (CataloguePriceSchedule, func()) {
	t.Helper()
	priceBoardMu.Lock()
	previousBoard := priceBoardCached
	previousDigest := priceBoardSHA256
	previousSource := priceBoardSource
	if previousBoard == nil {
		priceBoardMu.Unlock()
		t.Fatal("current catalogue fixture has no loaded price board")
	}
	raw, err := json.Marshal(previousBoard)
	priceBoardMu.Unlock()
	must(t, err)
	var repriced priceBoard
	must(t, json.Unmarshal(raw, &repriced))
	repriced.PositioningMultiplier *= 2
	repricedRaw, err := json.Marshal(repriced)
	must(t, err)

	oldPath, hadPath := os.LookupEnv(priceBoardPathEnv)
	oldDigest, hadDigest := os.LookupEnv(priceBoardDigestEnv)
	restore := func() {
		priceBoardMu.Lock()
		priceBoardCached = previousBoard
		priceBoardSHA256 = previousDigest
		priceBoardSource = previousSource
		priceBoardMu.Unlock()
		if hadPath {
			_ = os.Setenv(priceBoardPathEnv, oldPath)
		} else {
			_ = os.Unsetenv(priceBoardPathEnv)
		}
		if hadDigest {
			_ = os.Setenv(priceBoardDigestEnv, oldDigest)
		} else {
			_ = os.Unsetenv(priceBoardDigestEnv)
		}
	}
	t.Cleanup(restore)

	path := filepath.Join(t.TempDir(), "rolling-process-repriced-board.json")
	mustf(t, os.WriteFile(path, repricedRaw, 0o600), "write repriced board: %v")
	mustf(t, os.Setenv(priceBoardPathEnv, path), "set repriced board path: %v")
	mustf(t, os.Setenv(priceBoardDigestEnv, sha256Hex(repricedRaw)), "set repriced board digest: %v")
	priceBoardMu.Lock()
	priceBoardCached = nil
	priceBoardSHA256 = ""
	priceBoardSource = ""
	priceBoardMu.Unlock()

	schedule, err := BuildCataloguePriceSchedule()
	mustf(t, err, "build governed rolling-process reprice: %v")
	if schedule.SHA256 == oldScheduleSHA {
		t.Fatal("rolling-process reprice retained the old schedule digest")
	}
	return schedule, restore
}

func TestDurableCurrentCataloguePointerRejectsRollingProcessStaleAuthority(t *testing.T) {
	t.Run("new quote", func(t *testing.T) {
		ctx, store, pool, fixture, job, _ := seedCurrentIdentityLifecycleJob(t)
		next, restoreOldView := governedDifferentCatalogueSchedule(
			t, job.PricingDecision.Catalogue.ScheduleSHA256)
		var hookErr, oldDigestErr, pointerErr error
		previousHook := durableAdmissionPhysicalRecheckHook
		durableAdmissionPhysicalRecheckHook = func() {
			durableAdmissionPhysicalRecheckHook = nil
			_, hookErr = store.ApplyRepricing(ctx, next)
			restoreOldView()
			oldDigestErr = validateCurrentCataloguePriceAuthorityFrom(
				ctx, store.pool, job.PricingDecision.Catalogue)
			pointerErr = validateCurrentCatalogueModelPointerFrom(
				ctx, store.pool, job.PricingDecision.Catalogue)
		}
		t.Cleanup(func() { durableAdmissionPhysicalRecheckHook = previousHook })

		quoteID := uuid.New()
		quote := currentQuoteForJob(job, quoteID, time.Now().UTC().Add(time.Minute))
		err := store.InsertQuote(ctx, fixture.BuyerID, quote)
		mustf(t, hookErr, "apply concurrent catalogue pointer: %v")
		mustf(t, oldDigestErr, "old by-digest validator should still accept stale-process authority: %v")
		if pointerErr == nil {
			t.Fatal("current-pointer validator accepted the rolling process's stale schedule")
		}
		if !errors.Is(err, errCataloguePhysicalAuthorityUnavailable) {
			t.Fatalf("stale rolling-process quote error=%v, want catalogue physical sentinel", err)
		}
		var rows int
		mustf(t, pool.QueryRow(ctx, `SELECT count(*) FROM quotes WHERE id=$1`, quoteID).Scan(&rows),
			"count refused quotes: %v")
		if rows != 0 {
			t.Fatalf("stale rolling-process quote wrote %d rows", rows)
		}
	})

	t.Run("unquoted job", func(t *testing.T) {
		ctx, store, pool, fixture, job, tasks := seedCurrentIdentityLifecycleJob(t)
		next, restoreOldView := governedDifferentCatalogueSchedule(
			t, job.PricingDecision.Catalogue.ScheduleSHA256)
		var hookErr, oldDigestErr, pointerErr error
		previousHook := durableAdmissionPhysicalRecheckHook
		durableAdmissionPhysicalRecheckHook = func() {
			durableAdmissionPhysicalRecheckHook = nil
			_, hookErr = store.ApplyRepricing(ctx, next)
			restoreOldView()
			oldDigestErr = validateCurrentCataloguePriceAuthorityFrom(
				ctx, store.pool, job.PricingDecision.Catalogue)
			pointerErr = validateCurrentCatalogueModelPointerFrom(
				ctx, store.pool, job.PricingDecision.Catalogue)
		}
		t.Cleanup(func() { durableAdmissionPhysicalRecheckHook = previousHook })

		err := store.SubmitJobTx(ctx, job, tasks)
		mustf(t, hookErr, "apply concurrent catalogue pointer: %v")
		mustf(t, oldDigestErr, "old by-digest validator should still accept stale-process authority: %v")
		if pointerErr == nil {
			t.Fatal("current-pointer validator accepted the rolling process's stale schedule")
		}
		if !errors.Is(err, errCataloguePhysicalAuthorityUnavailable) {
			t.Fatalf("stale rolling-process unquoted job error=%v, want catalogue physical sentinel", err)
		}
		for name, query := range map[string]string{
			"jobs":     `SELECT count(*) FROM jobs WHERE id=$1`,
			"tasks":    `SELECT count(*) FROM tasks WHERE job_id=$1`,
			"plans":    `SELECT count(*) FROM job_economic_plans WHERE job_id=$1`,
			"reserves": `SELECT count(*) FROM job_economic_reserves WHERE job_id=$1`,
		} {
			var rows int
			mustf(t, pool.QueryRow(ctx, query, fixture.JobID).Scan(&rows), "count %s: %v", name)
			if rows != 0 {
				t.Fatalf("stale rolling-process job left %d %s rows", rows, name)
			}
		}
		if rows := countBuyerLedger(t, ctx, pool, fixture.BuyerID); rows != 0 {
			t.Fatalf("stale rolling-process job left %d buyer ledger rows", rows)
		}
	})
}

func TestBoundQuoteKeepsFrozenPriceAcrossPhysicalEquivalentReprice(t *testing.T) {
	ctx, store, pool, fixture, job, tasks := seedCurrentIdentityLifecycleJob(t)
	quoteID := uuid.New()
	quote := currentQuoteForJob(job, quoteID, time.Now().UTC().Add(time.Minute))
	mustf(t, store.InsertQuote(ctx, fixture.BuyerID, quote), "insert bound price promise: %v")
	t.Cleanup(func() {
		cleanupCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		_, _ = pool.Exec(cleanupCtx, `DELETE FROM quotes WHERE id=$1`, quoteID)
	})

	next, restoreOldView := governedDifferentCatalogueSchedule(
		t, job.PricingDecision.Catalogue.ScheduleSHA256)
	if _, err := store.ApplyRepricing(ctx, next); err != nil {
		t.Fatalf("apply physical-equivalent reprice: %v", err)
	}
	restoreOldView()
	current, err := store.LoadCataloguePriceAuthorityAtSchedule(
		ctx, next.SHA256, next.Version, job.ModelRef, job.JobType,
	)
	mustf(t, err, "load repriced append-only authority: %v")
	if current.ScheduleSHA256 == job.PricingDecision.Catalogue.ScheduleSHA256 ||
		current.SettlementPricePer1K == job.PricingDecision.Catalogue.SettlementPricePer1K {
		t.Fatalf("test reprice did not move current money: old=%+v current=%+v",
			job.PricingDecision.Catalogue, current)
	}
	if !reflect.DeepEqual(current.PhysicalAuthority, job.PricingDecision.Catalogue.PhysicalAuthority) {
		t.Fatal("price-only reprice unexpectedly changed the physical authority premise")
	}

	job.QuoteID = quoteID
	mustf(t, store.SubmitJobTx(ctx, job, tasks),
		"unexpired bound quote lost its frozen price across physical-equivalent reprice: %v")
	if rows := countJobRows(t, ctx, pool, fixture.JobID); rows != 1 {
		t.Fatalf("bound price promise wrote %d jobs, want one", rows)
	}
}
