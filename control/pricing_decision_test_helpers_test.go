package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"
)

func supplierShareForTest(t *testing.T, jobType, modelID string) float64 {
	t.Helper()
	share, err := supplierShareForWorkload(jobType, modelID)
	mustf(t, err, "supplier share policy for %s/%s: %v", jobType, modelID)
	return share
}

func catalogueAuthorityFixture(t *testing.T, workload WorkloadDecision, currency string, supplierShare float64) CataloguePriceAuthority {
	t.Helper()
	rate := 1.0
	fxRevision := "identity-usd"
	if currency != "usd" {
		rate = 1.35
		fxRevision = "test-fx-" + currency
	}
	referencePrice := 0.01
	// Digest is unique per fixture content so USD and CAD (and share variants)
	// can each exist as distinct append-only schedule rows under store-backed
	// validation. A fixed all-c digest made two different authorities claim the
	// same schedule and cannot be seeded.
	digestInput := fmt.Sprintf(
		"fixture-catalogue|%s|%s|%s|%.12g|%.12g|%s|%.12g|%s",
		workload.Binding.Model.Ref, workload.RuntimeJobType, currency,
		referencePrice, rate, fxRevision, supplierShare, supplierSharePolicyRevision,
	)
	sum := sha256.Sum256([]byte(digestInput))
	boardSum := sha256.Sum256([]byte("fixture-board|" + digestInput))
	authority := CataloguePriceAuthority{
		Version:                     cataloguePriceScheduleVersion,
		ModelID:                     workload.Binding.Model.Ref,
		JobType:                     workload.RuntimeJobType,
		PriceSource:                 "market_board",
		ScheduleSHA256:              hex.EncodeToString(sum[:]),
		ScheduleVersion:             cataloguePriceScheduleVersion,
		ReferenceCurrency:           catalogueReferenceCurrency,
		ReferencePricePer1K:         referencePrice,
		SettlementCurrency:          currency,
		SettlementPricePer1K:        ceilPricePer1K(referencePrice * rate),
		ReferenceToSettlementRate:   rate,
		FXRevision:                  fxRevision,
		BoardSHA256:                 hex.EncodeToString(boardSum[:]),
		PriceFormula:                "test market-board authority",
		SupplierShare:               supplierShare,
		SupplierSharePolicyRevision: supplierSharePolicyRevision,
	}
	mustf(t, validateCataloguePriceAuthority(authority), "catalogue authority fixture: %v")
	return authority
}

// seedCataloguePriceAuthority inserts the append-only schedule and history rows
// that store-backed pricing validation resolves. Idempotent on the fixture
// digest: a second call with the same authority is a no-op.
func seedCataloguePriceAuthority(t *testing.T, ctx context.Context, pool *pgxpool.Pool, a CataloguePriceAuthority) {
	t.Helper()
	if pool == nil {
		t.Fatal("seedCataloguePriceAuthority requires a pool")
	}
	mustf(t, validateCataloguePriceAuthority(a), "seed catalogue authority: %v")
	scheduleJSON, err := json.Marshal(map[string]any{
		"sha256":                         a.ScheduleSHA256,
		"version":                        a.ScheduleVersion,
		"reference_currency":             a.ReferenceCurrency,
		"settlement_currency":            a.SettlementCurrency,
		"fx_revision":                    a.FXRevision,
		"board_sha256":                   a.BoardSHA256,
		"supplier_share_policy_revision": a.SupplierSharePolicyRevision,
	})
	mustf(t, err, "marshal fixture schedule: %v")
	if _, err := pool.Exec(ctx, `
		INSERT INTO catalogue_price_schedules (
		  sha256,version,reference_currency,settlement_currency,
		  reference_to_settlement_rate,fx_revision,board_sha256,board_schema_version,
		  board_fetched_at,positioning_multiplier,supplier_share,schedule_json
		) VALUES ($1,$2,$3,$4,$5,$6,$7,1,'1970-01-01T00:00:00Z',1.0,NULL,$8)
		ON CONFLICT (sha256) DO NOTHING`,
		a.ScheduleSHA256, a.ScheduleVersion, a.ReferenceCurrency, a.SettlementCurrency,
		a.ReferenceToSettlementRate, a.FXRevision, a.BoardSHA256, scheduleJSON,
	); err != nil {
		t.Fatalf("seed catalogue_price_schedules: %v", err)
	}
	// Ensure the model row exists for the history FK when tests use non-seed models.
	if _, err := pool.Exec(ctx, `
		INSERT INTO models (id, job_type, kind)
		VALUES ($1,$2,'hf')
		ON CONFLICT (id) DO NOTHING`,
		a.ModelID, a.JobType,
	); err != nil {
		t.Fatalf("ensure model %s for catalogue seed: %v", a.ModelID, err)
	}
	if _, err := pool.Exec(ctx, `
		INSERT INTO model_price_history (
		  schedule_sha256,model_id,prior_price_per_1k,prior_price_source,
		  reference_price_per_1k,reference_currency,price_per_1k,
		  price_currency,price_formula,supplier_share
		) VALUES ($1,$2,0,'seed',$3,$4,$5,$6,$7,$8)
		ON CONFLICT (schedule_sha256, model_id) DO NOTHING`,
		a.ScheduleSHA256, a.ModelID, a.ReferencePricePer1K, a.ReferenceCurrency,
		a.SettlementPricePer1K, a.SettlementCurrency, a.PriceFormula, a.SupplierShare,
	); err != nil {
		t.Fatalf("seed model_price_history for %s: %v", a.ModelID, err)
	}
}

// catalogueAuthorityFixtureInStore is the store-backed counterpart of
// catalogueAuthorityFixture: same numbers, and the append-only rows exist.
func catalogueAuthorityFixtureInStore(
	t *testing.T,
	ctx context.Context,
	pool *pgxpool.Pool,
	workload WorkloadDecision,
	currency string,
	supplierShare float64,
) CataloguePriceAuthority {
	t.Helper()
	authority := catalogueAuthorityFixture(t, workload, currency, supplierShare)
	seedCataloguePriceAuthority(t, ctx, pool, authority)
	return authority
}

func placementForPricingFixture(
	t *testing.T,
	workload WorkloadDecision,
	authority CataloguePriceAuthority,
) PlacementRequirement {
	t.Helper()
	binding := workload.Binding
	ceiling, err := supplierAdmissionCeilingUSDHr(
		authority, workload.RuntimeJobType, binding.Tier,
		admissionCellsForWorkload(workload),
	)
	mustf(t, err, "derive placement ceiling fixture: %v")
	placement, err := placementRequirementFor(jobSubmit{
		JobType: binding.JobType, Model: binding.Model, Constraints: binding.Constraints,
		Tier: binding.Tier, MinReputation: binding.MinReputation,
	}, workload, float32(ceiling))
	mustf(t, err, "build placement fixture: %v")
	return placement
}
