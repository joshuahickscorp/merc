package main

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"
)

// storeAnchoredDistributedFixture applies the governed catalogue schedule and
// builds a distributed pricing decision whose catalogue is a real append-only
// row, not the unit-test fixture digests.
func storeAnchoredDistributedFixture(t *testing.T) (
	context.Context,
	*Store,
	*pgxpool.Pool,
	WorkloadDecision,
	ComputePlan,
	PlacementRequirement,
	EconomicPlan,
	PricingDecision,
	CataloguePriceAuthority,
) {
	t.Helper()
	pinBoardClockForPublication(t)
	ctx, store, pool := openAdminMutationTestStore(t)

	schedule, err := BuildCataloguePriceSchedule()
	if err != nil {
		t.Fatalf("build catalogue schedule: %v", err)
	}
	if _, err := store.ApplyRepricing(ctx, schedule); err != nil {
		t.Fatalf("apply catalogue schedule: %v", err)
	}

	workload, compute, _ := computePlanFixture(t)
	authority, err := store.LoadCataloguePriceAuthority(ctx, workload.Binding.Model.Ref)
	if err != nil {
		t.Fatalf("load catalogue authority for %s: %v", workload.Binding.Model.Ref, err)
	}
	if authority.JobType != workload.RuntimeJobType {
		t.Fatalf("fixture model job type %q != workload %q",
			authority.JobType, workload.RuntimeJobType)
	}

	economicSchedule := testEconomicSchedule()
	economicSchedule.Currency = authority.SettlementCurrency
	economic := BuildEconomicPlan(EconomicPlanInput{
		BaseComputeUSD:   0.40,
		InitialTaskCount: compute.TotalInitialTasks,
		ExtraTaskReserve: 2,
		SupplierShare:    authority.SupplierShare,
	}, economicSchedule)
	if !economic.Executable {
		t.Fatalf("store-anchored economics blocked: %s", economic.BlockReason)
	}
	if err := ValidateComputePlanEconomicSnapshot(compute, workload, economic); err != nil {
		t.Fatalf("compute/economic authority disagree: %v", err)
	}

	placement := placementForPricingFixture(t, workload, authority)
	pricing, err := newDistributedPricingDecision(
		workload, compute, placement, economic, authority,
		workload.Binding.Tier, "",
	)
	if err != nil {
		t.Fatalf("build store-anchored pricing decision: %v", err)
	}
	return ctx, store, pool, workload, compute, placement, economic, pricing, authority
}

// forgeDistributedPricingAtCatalogue rebuilds placement and the full pricing
// decision so every pure composite check passes under the supplied catalogue.
func forgeDistributedPricingAtCatalogue(
	t *testing.T,
	workload WorkloadDecision,
	compute ComputePlan,
	base EconomicPlan,
	catalogue CataloguePriceAuthority,
	rate float64,
) (PlacementRequirement, EconomicPlan, PricingDecision) {
	t.Helper()
	economicSchedule := base.Schedule
	economicSchedule.Currency = catalogue.SettlementCurrency
	economic := BuildEconomicPlan(EconomicPlanInput{
		BaseComputeUSD:   base.Input.BaseComputeUSD,
		InitialTaskCount: base.Input.InitialTaskCount,
		ExtraTaskReserve: base.Input.ExtraTaskReserve,
		SupplierShare:    catalogue.SupplierShare,
		SLAPremiumUSD:    base.Input.SLAPremiumUSD,
		FirmQuoteMaxUSD:  base.Input.FirmQuoteMaxUSD,
	}, economicSchedule)
	if !economic.Executable {
		t.Fatalf("forged economics blocked: %s", economic.BlockReason)
	}
	placement := placementForPricingFixture(t, workload, catalogue)
	// placementForPricingFixture uses live admission rates; pin the stored rate.
	placement.OfferedRateUsdHr = float32(expectedSupplierUSDHr(
		rate, catalogue.ReferencePricePer1K, catalogue.SupplierShare, workload.Binding.Tier,
	))
	forged, err := distributedPricingDecisionAtRate(
		workload, compute, placement, economic, catalogue,
		workload.Binding.Tier, "", rate,
	)
	if err != nil {
		t.Fatalf("forged decision must be internally consistent: %v", err)
	}
	return placement, economic, forged
}

// Tests 1 and 2: a coherently rewritten catalogue is accepted by the pure
// validator and refused by the store-backed one. Both assertions live in the
// same test so the delta is the evidence of the defect and its fix.
func TestStoreAnchoredPricingRejectsRewrittenCatalogueFields(t *testing.T) {
	ctx, store, _, workload, compute, _, economic, pricing, authority := storeAnchoredDistributedFixture(t)
	rate := pricing.ExpectedSupplierUnitsPerSec

	cases := []struct {
		name   string
		mutate func(CataloguePriceAuthority) CataloguePriceAuthority
		field  string
	}{
		{
			name: "settlement_price_per_1k",
			mutate: func(c CataloguePriceAuthority) CataloguePriceAuthority {
				c.ReferencePricePer1K *= 10
				c.SettlementPricePer1K = ceilPricePer1K(
					c.ReferencePricePer1K * c.ReferenceToSettlementRate,
				)
				return c
			},
			field: "SettlementPricePer1K",
		},
		{
			name: "supplier_share",
			mutate: func(c CataloguePriceAuthority) CataloguePriceAuthority {
				// Stay inside (0,1] and away from the governed policy value.
				if c.SupplierShare > 0.5 {
					c.SupplierShare = 0.41
				} else {
					c.SupplierShare = 0.91
				}
				return c
			},
			field: "SupplierShare",
		},
		{
			name: "reference_to_settlement_rate",
			mutate: func(c CataloguePriceAuthority) CataloguePriceAuthority {
				c.ReferenceToSettlementRate *= 1.25
				c.SettlementPricePer1K = ceilPricePer1K(
					c.ReferencePricePer1K * c.ReferenceToSettlementRate,
				)
				return c
			},
			field: "ReferenceToSettlementRate",
		},
		{
			name: "schedule_sha256",
			mutate: func(c CataloguePriceAuthority) CataloguePriceAuthority {
				// Keep numbers; only the digest the pure path trusts is forged.
				c.ScheduleSHA256 = strings.Repeat("a", 64)
				return c
			},
			field: "ScheduleSHA256",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			mutantCatalogue := tc.mutate(authority)
			if mutantCatalogue == authority {
				t.Fatal("mutation left catalogue unchanged")
			}
			forgedPlacement, forgedEconomic, forged := forgeDistributedPricingAtCatalogue(
				t, workload, compute, economic, mutantCatalogue, rate,
			)

			if err := ValidateDistributedPricingDecisionSnapshot(
				forged, workload, compute, forgedPlacement, forgedEconomic,
			); err != nil {
				t.Fatalf("pure validator refused a coherently rewritten catalogue "+
					"(%s); the defect this test pins is that pure accepts it: %v",
					tc.field, err)
			}

			err := ValidateDistributedPricingDecisionSnapshotWithStore(
				ctx, store, forged, workload, compute, forgedPlacement, forgedEconomic,
			)
			if err == nil {
				t.Fatalf("store-backed validator accepted a rewritten catalogue %s", tc.field)
			}
			// Coherent price rewrites touch more than one catalogue field
			// (settlement must match reference×FX). The store refuses on the
			// first disagreeing field; that field name is required to be a
			// catalogue mismatch or an unresolvable digest, not a silent pass.
			if !strings.Contains(err.Error(), "does not match append-only authority") &&
				!strings.Contains(err.Error(), "not resolvable") {
				t.Fatalf("store-backed error for %s was not a catalogue anchor refusal: %v",
					tc.field, err)
			}
			if tc.field == "SupplierShare" || tc.field == "ScheduleSHA256" {
				if !strings.Contains(err.Error(), tc.field) &&
					!strings.Contains(err.Error(), "not resolvable") {
					t.Fatalf("store-backed error did not name %s: %v", tc.field, err)
				}
			}

			// Honest decision still passes both paths.
			honestPlacement, honestEconomic, honest := forgeDistributedPricingAtCatalogue(
				t, workload, compute, economic, authority, rate,
			)
			if err := ValidateDistributedPricingDecisionSnapshot(
				honest, workload, compute, honestPlacement, honestEconomic,
			); err != nil {
				t.Fatalf("honest pure validation failed: %v", err)
			}
			if err := ValidateDistributedPricingDecisionSnapshotWithStore(
				ctx, store, honest, workload, compute, honestPlacement, honestEconomic,
			); err != nil {
				t.Fatalf("honest store-backed validation failed: %v", err)
			}
		})
	}
}

// A later reprice moves models.price_* and the current LoadCatalogue pointer.
// An already-accepted decision still validates against its own schedule digest.
func TestStoreAnchoredPricingSurvivesLegitimateReprice(t *testing.T) {
	ctx, store, pool, workload, compute, placement, economic, pricing, authority := storeAnchoredDistributedFixture(t)

	// Insert a second append-only schedule with different prices and repoint models.
	newSHA := strings.Repeat("b", 64)
	newBoard := strings.Repeat("e", 64)
	newReference := authority.ReferencePricePer1K * 3
	newSettlement := ceilPricePer1K(newReference * authority.ReferenceToSettlementRate)
	scheduleJSON, err := json.Marshal(map[string]any{
		"sha256":                         newSHA,
		"version":                        authority.ScheduleVersion,
		"reference_currency":             authority.ReferenceCurrency,
		"settlement_currency":            authority.SettlementCurrency,
		"fx_revision":                    authority.FXRevision + "-reprice",
		"board_sha256":                   newBoard,
		"supplier_share_policy_revision": authority.SupplierSharePolicyRevision,
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `
		INSERT INTO catalogue_price_schedules (
		  sha256,version,reference_currency,settlement_currency,
		  reference_to_settlement_rate,fx_revision,board_sha256,board_schema_version,
		  board_fetched_at,positioning_multiplier,supplier_share,schedule_json
		) VALUES ($1,$2,$3,$4,$5,$6,$7,1,'1970-01-01T00:00:00Z',1.0,NULL,$8)`,
		newSHA, authority.ScheduleVersion, authority.ReferenceCurrency,
		authority.SettlementCurrency, authority.ReferenceToSettlementRate,
		authority.FXRevision+"-reprice", newBoard, scheduleJSON,
	); err != nil {
		t.Fatalf("insert reprice schedule: %v", err)
	}
	if _, err := pool.Exec(ctx, `
		INSERT INTO model_price_history (
		  schedule_sha256,model_id,prior_price_per_1k,prior_price_source,
		  reference_price_per_1k,reference_currency,price_per_1k,
		  price_currency,price_formula,supplier_share
		) VALUES ($1,$2,$3,'market_board',$4,$5,$6,$7,$8,$9)`,
		newSHA, authority.ModelID, authority.SettlementPricePer1K,
		newReference, authority.ReferenceCurrency, newSettlement,
		authority.SettlementCurrency, "reprice test formula", authority.SupplierShare,
	); err != nil {
		t.Fatalf("insert reprice history: %v", err)
	}
	if _, err := pool.Exec(ctx, `
		UPDATE models
		   SET price_per_1k=$2, price_reference_per_1k=$3,
		       price_schedule_sha256=$4, price_formula=$5,
		       price_source='market_board', price_currency=$6,
		       price_reference_currency=$7, price_schedule_version=$8
		 WHERE id=$1`,
		authority.ModelID, newSettlement, newReference, newSHA,
		"reprice test formula", authority.SettlementCurrency,
		authority.ReferenceCurrency, authority.ScheduleVersion,
	); err != nil {
		t.Fatalf("repoint model to reprice schedule: %v", err)
	}

	current, err := store.LoadCataloguePriceAuthority(ctx, authority.ModelID)
	if err != nil {
		t.Fatalf("load current catalogue after reprice: %v", err)
	}
	if current.ScheduleSHA256 == authority.ScheduleSHA256 ||
		current.SettlementPricePer1K == authority.SettlementPricePer1K {
		t.Fatalf("reprice did not move current catalogue: %+v", current)
	}

	// The accepted decision still anchors to the old digest.
	if err := ValidateDistributedPricingDecisionSnapshotWithStore(
		ctx, store, pricing, workload, compute, placement, economic,
	); err != nil {
		t.Fatalf("legitimate reprice invalidated an already-accepted decision: %v", err)
	}
	// And the pure path still accepts it for offline contexts.
	if err := ValidateDistributedPricingDecisionSnapshot(
		pricing, workload, compute, placement, economic,
	); err != nil {
		t.Fatalf("pure validation of accepted decision failed after reprice: %v", err)
	}
}

// An unknown schedule digest fails closed with a distinct error.
func TestStoreAnchoredPricingFailsClosedOnUnresolvableSchedule(t *testing.T) {
	ctx, store, _, workload, compute, _, economic, pricing, _ := storeAnchoredDistributedFixture(t)

	missing := pricing.Catalogue
	missing.ScheduleSHA256 = strings.Repeat("f", 64)
	// Keep composite self-consistent under pure rebuild so the only failure is
	// the store-backed schedule resolution.
	placement, economic, forged := forgeDistributedPricingAtCatalogue(
		t, workload, compute, economic, missing, pricing.ExpectedSupplierUnitsPerSec,
	)
	if err := ValidateDistributedPricingDecisionSnapshot(
		forged, workload, compute, placement, economic,
	); err != nil {
		t.Fatalf("pure validator refused unresolvable-digest forgery: %v", err)
	}
	err := ValidateDistributedPricingDecisionSnapshotWithStore(
		ctx, store, forged, workload, compute, placement, economic,
	)
	if err == nil {
		t.Fatal("store-backed validator accepted an unresolvable schedule digest")
	}
	if !strings.Contains(err.Error(), "not resolvable") {
		t.Fatalf("expected unresolvable schedule error, got: %v", err)
	}
}

func TestCatalogueAnchoredValidationRequiresStore(t *testing.T) {
	workload, compute, placement, economic, pricing := distributedPricingFixture(t)
	err := ValidateDistributedPricingDecisionSnapshotWithStore(
		context.Background(), nil, pricing, workload, compute, placement, economic,
	)
	if err == nil || !strings.Contains(err.Error(), "requires a store") {
		t.Fatalf("nil store must refuse rather than fall back to pure check: %v", err)
	}
}
