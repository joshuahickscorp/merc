package main

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
)

func persistHistoricalV2CatalogueSchedule(
	t *testing.T,
	ctx context.Context,
	store *Store,
	current CataloguePriceSchedule,
) CataloguePriceSchedule {
	t.Helper()
	legacy := current
	legacy.Version = 2
	legacy.BoardFreshnessPolicy = ""
	legacy.BoardValidUntil = ""
	legacy.CurrentUseFreshnessPolicy = ""
	legacy.CurrentUseValidUntil = ""
	legacy.Results = append([]RepriceResult(nil), current.Results...)
	for i := range legacy.Results {
		legacy.Results[i].PhysicalAuthority = CatalogueResultPhysicalAuthority{}
	}
	var err error
	legacy.SHA256, err = cataloguePriceScheduleDigest(legacy)
	must(t, err)
	mustf(t, validateCataloguePriceSchedule(legacy), "historical v2 schedule shape: %v")
	scheduleJSON, err := json.Marshal(legacy)
	must(t, err)
	_, err = store.pool.Exec(ctx, `
		INSERT INTO catalogue_price_schedules (
		  sha256,version,reference_currency,settlement_currency,
		  reference_to_settlement_rate,fx_revision,board_sha256,board_schema_version,
		  board_fetched_at,positioning_multiplier,supplier_share,schedule_json
		) VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,NULL,$11)`,
		legacy.SHA256, legacy.Version, legacy.ReferenceCurrency, legacy.SettlementCurrency,
		legacy.ReferenceToSettlement, legacy.FXRevision, legacy.BoardSHA256,
		legacy.BoardSchemaVersion, legacy.BoardFetchedAt, legacy.PositioningMultiplier, scheduleJSON)
	mustf(t, err, "persist historical v2 schedule: %v")
	for _, result := range legacy.Results {
		_, err = store.pool.Exec(ctx, `
			INSERT INTO model_price_history (
			  schedule_sha256,model_id,job_type,prior_price_per_1k,prior_price_source,
			  reference_price_per_1k,reference_currency,price_per_1k,
			  price_currency,price_formula,supplier_share
			) VALUES ($1,$2,$3,0,'market_board',$4,$5,$6,$7,$8,$9)`,
			legacy.SHA256, result.ModelID, result.JobType, result.ReferencePricePer1K,
			legacy.ReferenceCurrency, result.PricePer1K, legacy.SettlementCurrency,
			result.Formula, result.SupplierShare)
		mustf(t, err, "persist historical v2 result %s: %v", result.ModelID, err)
	}
	return legacy
}

func persistHistoricalV3PhysicalV1CatalogueSchedule(
	t *testing.T,
	ctx context.Context,
	store *Store,
	current CataloguePriceSchedule,
) CataloguePriceSchedule {
	t.Helper()
	legacy := current
	legacy.Results = append([]RepriceResult(nil), current.Results...)
	for i := range legacy.Results {
		physical := legacy.Results[i].PhysicalAuthority
		physical.Version = catalogueResultPhysicalAuthorityLegacyVersion
		physical.EngineBuildHash = ""
		physical.EngineBuildIdentityPolicy = ""
		physical.HardwareIdentity = ""
		physical.Throughput.EngineBuildHash = ""
		physical.Throughput.EngineBuildIdentityPolicy = ""
		physical.Throughput.HardwareIdentity = ""
		physical.Power.RuntimeCellID = ""
		physical.Power.RuntimeProfileID = ""
		physical.Power.Engine = ""
		physical.Power.EngineBuildHash = ""
		physical.Power.EngineBuildIdentityPolicy = ""
		physical.Power.HWClass = ""
		physical.Power.HardwareIdentity = ""
		physical.Power.CoveredWorkloads = append(
			[]CataloguePowerCoveredWorkload(nil), physical.Power.CoveredWorkloads...)
		for j := range physical.Power.CoveredWorkloads {
			physical.Power.CoveredWorkloads[j].RuntimeCellID = ""
			physical.Power.CoveredWorkloads[j].RuntimeProfileID = ""
			physical.Power.CoveredWorkloads[j].Engine = ""
			physical.Power.CoveredWorkloads[j].EngineBuildHash = ""
			physical.Power.CoveredWorkloads[j].EngineBuildIdentityPolicy = ""
			physical.Power.CoveredWorkloads[j].HardwareIdentity = ""
		}
		legacy.Results[i].PhysicalAuthority = physical
	}
	var err error
	legacy.SHA256, err = cataloguePriceScheduleDigest(legacy)
	must(t, err)
	mustf(t, validateCataloguePriceSchedule(legacy), "historical v3/physical-v1 schedule shape: %v")
	scheduleJSON, err := json.Marshal(legacy)
	must(t, err)
	_, err = store.pool.Exec(ctx, `
		INSERT INTO catalogue_price_schedules (
		  sha256,version,reference_currency,settlement_currency,
		  reference_to_settlement_rate,fx_revision,board_sha256,board_schema_version,
		  board_fetched_at,positioning_multiplier,supplier_share,schedule_json
		) VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,NULL,$11)`,
		legacy.SHA256, legacy.Version, legacy.ReferenceCurrency, legacy.SettlementCurrency,
		legacy.ReferenceToSettlement, legacy.FXRevision, legacy.BoardSHA256,
		legacy.BoardSchemaVersion, legacy.BoardFetchedAt, legacy.PositioningMultiplier, scheduleJSON)
	mustf(t, err, "persist historical v3/physical-v1 schedule: %v")
	for _, result := range legacy.Results {
		_, err = store.pool.Exec(ctx, `
			INSERT INTO model_price_history (
			  schedule_sha256,model_id,job_type,prior_price_per_1k,prior_price_source,
			  reference_price_per_1k,reference_currency,price_per_1k,
			  price_currency,price_formula,supplier_share
			) VALUES ($1,$2,$3,0,'market_board',$4,$5,$6,$7,$8,$9)`,
			legacy.SHA256, result.ModelID, result.JobType, result.ReferencePricePer1K,
			legacy.ReferenceCurrency, result.PricePer1K, legacy.SettlementCurrency,
			result.Formula, result.SupplierShare)
		mustf(t, err, "persist historical v3/physical-v1 result %s: %v", result.ModelID, err)
	}
	return legacy
}

func currentPhysicalCatalogueFixture(t *testing.T) (context.Context, *Store, CataloguePriceSchedule) {
	t.Helper()
	installBoundCataloguePublicationAuthorityForTest(t)
	pinBoardClockForPublication(t)
	ctx, store, _ := openAdminMutationTestStore(t)
	schedule, err := BuildCataloguePriceSchedule()
	mustf(t, err, "build physical catalogue schedule: %v")
	if _, err := store.ApplyRepricing(ctx, schedule); err != nil {
		t.Fatalf("apply physical catalogue schedule: %v", err)
	}
	return ctx, store, schedule
}

func TestApplyRepricingRejectsDigestValidHandBuiltSchedule(t *testing.T) {
	installBoundCataloguePublicationAuthorityForTest(t)
	pinBoardClockForPublication(t)
	ctx, store, pool := openIsolatedTestStore(t)
	schedule, err := BuildCataloguePriceSchedule()
	must(t, err)
	forged := schedule
	forged.Results = append([]RepriceResult(nil), schedule.Results...)
	forged.Results[0].Formula += " attacker_repriced=1"
	forged.SHA256, err = cataloguePriceScheduleDigest(forged)
	must(t, err)
	mustf(t, validateCataloguePriceSchedule(forged), "forged schedule must be shape/digest valid: %v")

	if _, err := store.ApplyRepricing(ctx, forged); err == nil ||
		!strings.Contains(err.Error(), "sole current governed derivation") {
		t.Fatalf("digest-valid hand-built schedule apply error=%v", err)
	}
	var rows int
	mustf(t, pool.QueryRow(ctx,
		`SELECT count(*) FROM catalogue_price_schedules WHERE sha256=$1`, forged.SHA256,
	).Scan(&rows), "count forged schedules: %v")
	if rows != 0 {
		t.Fatalf("forged schedule left %d durable rows", rows)
	}
}

func TestCurrentCatalogueExpiresAtExactPhysicalBoundaryButHistoricalReplaySurvives(t *testing.T) {
	ctx, store, schedule := currentPhysicalCatalogueFixture(t)
	result := schedule.Results[0]
	powerBoundary, err := time.Parse(time.RFC3339, result.PhysicalAuthority.Power.ValidUntil)
	must(t, err)
	previousNow := cataloguePowerNow
	cataloguePowerNow = func() time.Time { return powerBoundary }
	t.Cleanup(func() { cataloguePowerNow = previousNow })

	if _, err := store.LoadCataloguePriceAuthority(ctx, result.ModelID); err != nil {
		t.Fatalf("current catalogue was refused exactly at its inclusive validity boundary: %v", err)
	}
	cataloguePowerNow = func() time.Time { return powerBoundary.Add(time.Nanosecond) }
	if _, err := store.LoadCataloguePriceAuthority(ctx, result.ModelID); err == nil ||
		!strings.Contains(err.Error(), "stale under "+cataloguePowerFreshnessPolicy) {
		t.Fatalf("current catalogue after power expiry error=%v", err)
	}

	// Accepted history is resolved solely from append-only schedule/history rows.
	if _, err := store.LoadCataloguePriceAuthorityAtSchedule(
		ctx, schedule.SHA256, schedule.Version, result.ModelID, result.JobType,
	); err != nil {
		t.Fatalf("historical catalogue replay consulted the current clock: %v", err)
	}
}

func TestWithdrawnReceiptAfterPersistenceBlocksCurrentAuthority(t *testing.T) {
	ctx, store, schedule := currentPhysicalCatalogueFixture(t)
	result := schedule.Results[0]
	path, _, _ := strings.Cut(result.PhysicalAuthority.Throughput.Citation, "#")
	raw, err := os.ReadFile(path)
	must(t, err)
	var receipt map[string]any
	must(t, json.Unmarshal(raw, &receipt))
	receipt["binding_status"] = BindingWithdrawn
	mutated, err := json.MarshalIndent(receipt, "", "  ")
	must(t, err)
	must(t, os.WriteFile(path, append(mutated, '\n'), 0o600))

	if _, err := store.LoadCataloguePriceAuthority(ctx, result.ModelID); err == nil {
		t.Fatal("current catalogue survived withdrawal of its throughput receipt")
	}
	if _, err := store.LoadCataloguePriceAuthorityAtSchedule(
		ctx, schedule.SHA256, schedule.Version, result.ModelID, result.JobType,
	); err != nil {
		t.Fatalf("accepted historical replay consulted withdrawn current evidence: %v", err)
	}
}

func TestHistoricalCatalogueReplayDoesNotJoinMutableModelMetadata(t *testing.T) {
	ctx, store, schedule := currentPhysicalCatalogueFixture(t)
	result := schedule.Results[0]
	if _, err := store.pool.Exec(ctx,
		`UPDATE models SET job_type='media_rendering' WHERE id=$1`, result.ModelID,
	); err != nil {
		t.Fatalf("mutate current model metadata: %v", err)
	}
	if _, err := store.LoadCataloguePriceAuthorityAtSchedule(
		ctx, schedule.SHA256, schedule.Version, result.ModelID, result.JobType,
	); err != nil {
		t.Fatalf("historical catalogue replay depended on mutable models.job_type: %v", err)
	}
}

func TestVersionTwoAcceptedPricingReplayRemainsSelfContained(t *testing.T) {
	ctx, store, _, workload, compute, _, economic, pricing, _ := storeAnchoredDistributedFixture(t)
	current, err := BuildCataloguePriceSchedule()
	must(t, err)
	legacy := persistHistoricalV2CatalogueSchedule(t, ctx, store, current)
	var legacyResult RepriceResult
	for _, result := range legacy.Results {
		if result.ModelID == workload.Binding.Model.Ref && result.JobType == workload.RuntimeJobType {
			legacyResult = result
			break
		}
	}
	legacyAuthority, err := store.LoadCataloguePriceAuthorityAtSchedule(
		ctx, legacy.SHA256, legacy.Version, legacyResult.ModelID, legacyResult.JobType,
	)
	mustf(t, err, "load v2 catalogue authority: %v")
	legacySchedule := economic.Schedule
	legacySchedule.Currency = legacyAuthority.SettlementCurrency
	legacyEconomic := BuildEconomicPlan(EconomicPlanInput{
		BaseComputeUSD:   economic.Input.BaseComputeUSD,
		InitialTaskCount: economic.Input.InitialTaskCount,
		ExtraTaskReserve: economic.Input.ExtraTaskReserve,
		SupplierShare:    legacyAuthority.SupplierShare,
		SLAPremiumUSD:    economic.Input.SLAPremiumUSD,
		FirmQuoteMaxUSD:  economic.Input.FirmQuoteMaxUSD,
	}, legacySchedule)
	if !legacyEconomic.Executable {
		t.Fatalf("historical v2 economics blocked: %s", legacyEconomic.BlockReason)
	}
	placement := placementForPricingFixture(t, workload, legacyAuthority)
	// Catalogue v2 predates retained physical power authority. Pair it with the
	// placement shape that could actually have been accepted in that epoch,
	// rather than asking a modern v3 placement to mint new energy facts from a
	// historical schedule that never carried them.
	placement.Version = 1
	placement.EngineBuildHash = ""
	placement.EngineBuildIdentityPolicy = ""
	placement.HardwareIdentity = ""
	placement.PerformanceAuthority = nil
	placement.HWClasses = append(
		[]string(nil), workload.Binding.Constraints.HWClasses...)
	placement.OfferedRateUsdHr = float32(expectedSupplierUSDHr(
		pricing.ExpectedSupplierUnitsPerSec, legacyAuthority.ReferencePricePer1K,
		legacyAuthority.SupplierShare, workload.Binding.Tier,
	))
	legacyPricing, err := distributedPricingDecisionAtRate(
		workload, compute, placement, legacyEconomic, legacyAuthority,
		workload.Binding.Tier, "", pricing.ExpectedSupplierUnitsPerSec,
	)
	mustf(t, err, "build historical v2 pricing decision: %v")
	if err := ValidateDistributedPricingDecisionSnapshotWithStore(
		ctx, store, legacyPricing, workload, compute, placement, legacyEconomic,
	); err != nil {
		t.Fatalf("accepted v2 pricing replay was rejected: %v", err)
	}
	if _, err := store.LoadCataloguePriceAuthority(ctx, legacyResult.ModelID); err != nil {
		// The current pointer remains v3; this assertion only ensures installing
		// historical v2 rows did not disturb current authority.
		t.Fatalf("historical v2 insert disturbed current v3 authority: %v", err)
	}
}

func TestVersionThreePhysicalV1CatalogueReplayRemainsHistoricalOnly(t *testing.T) {
	ctx, store, current := currentPhysicalCatalogueFixture(t)
	legacy := persistHistoricalV3PhysicalV1CatalogueSchedule(t, ctx, store, current)
	result := legacy.Results[0]
	authority, err := store.LoadCataloguePriceAuthorityAtSchedule(
		ctx, legacy.SHA256, legacy.Version, result.ModelID, result.JobType)
	mustf(t, err, "load historical v3/physical-v1 authority: %v")
	if authority.PhysicalAuthority.Version != catalogueResultPhysicalAuthorityLegacyVersion {
		t.Fatalf("historical physical version=%d, want %d",
			authority.PhysicalAuthority.Version, catalogueResultPhysicalAuthorityLegacyVersion)
	}
	if err := validateCurrentCataloguePriceAuthorityFrom(ctx, store.pool, authority); err == nil {
		t.Fatal("historical v3/physical-v1 authority became current-admissible")
	}
}

func TestPolicylessPhysicalV2CatalogueSnapshotRemainsHistoricalOnly(t *testing.T) {
	sub := testOnlyCombinedTokenSubmit(t)
	workload, err := buildWorkloadDecision(sub, strings.Repeat("8", 64))
	must(t, err)
	placement, err := placementRequirementFor(sub, workload, 1)
	must(t, err)
	physical, ok := testOnlyCataloguePhysicalAuthority(workload)
	if !ok {
		t.Fatal("TEST_ONLY current physical authority is unavailable")
	}
	physical.EngineBuildIdentityPolicy = ""
	physical.Throughput.EngineBuildIdentityPolicy = ""
	physical.Power.EngineBuildIdentityPolicy = ""
	for i := range physical.Power.CoveredWorkloads {
		physical.Power.CoveredWorkloads[i].EngineBuildIdentityPolicy = ""
	}

	mustf(t, func() error {
		_, err := validateCatalogueResultPhysicalAuthority(RepriceResult{
			ModelID: physical.ModelID, JobType: physical.JobType,
			PhysicalAuthority: physical,
		})
		return err
	}(), "policy-less physical-v2 snapshot was not historically readable: %v")
	catalogue := catalogueAuthorityFixture(
		t, workload, SettlementCurrencyCode(),
		supplierShareForTest(t, workload.RuntimeJobType, workload.Binding.Model.Ref),
	)
	catalogue.PhysicalAuthority = physical
	if err := validateCurrentPlacementCataloguePhysicalAuthority(placement, catalogue); err == nil {
		t.Fatal("policy-less historical physical-v2 snapshot became current-admissible")
	}
}

func expireCataloguePowerJustPastBoundary(t *testing.T, schedule CataloguePriceSchedule) {
	t.Helper()
	boundary, err := time.Parse(time.RFC3339, schedule.Results[0].PhysicalAuthority.Power.ValidUntil)
	must(t, err)
	previous := cataloguePowerNow
	cataloguePowerNow = func() time.Time { return boundary.Add(time.Nanosecond) }
	t.Cleanup(func() { cataloguePowerNow = previous })
}

// currentUniformCatalogueIngressFixture builds the only currently priced
// distributed geometry: one primary task plus one exact redundancy clone. The
// synthetic throughput and power receipts are installed only after database
// startup, then published through the governed schedule constructor so these
// durable-ingress tests exercise current authority rather than a hand-built or
// pre-uniform historical snapshot.
func currentUniformCatalogueIngressFixture(t *testing.T) (
	context.Context, *Store, *jobRow, []taskRow, CataloguePriceSchedule,
) {
	t.Helper()
	ctx, store, _, _, job, tasks, schedule := currentUniformMoneyPathJob(t)
	return ctx, store, job, tasks, schedule
}

func TestInsertQuoteRechecksCurrentCatalogueAtDurableIngressWithZeroWrites(t *testing.T) {
	ctx, store, job, _, schedule := currentUniformCatalogueIngressFixture(t)
	expireCataloguePowerJustPastBoundary(t, schedule)
	quoteID := uuid.New()
	quote := Quote{
		QuoteID: "q_" + quoteID.String(), bareID: quoteID,
		JobType: job.JobType, Model: job.ModelRef,
		Tier: job.Tier, Currency: job.PricingDecision.Currency,
		Workload: job.WorkloadDecision, ComputePlan: job.ComputePlan,
		Placement: job.PlacementRequirement,
		Economics: job.EconomicPlan, Pricing: job.PricingDecision,
		Time: QuoteTime{
			P50Secs: job.ComputePlan.ETAP50Secs, P90Secs: job.ComputePlan.ETAP90Secs,
			WorstCaseSecs:        job.ComputePlan.ETAWorstCaseSecs,
			ConfidenceBandMethod: job.ComputePlan.ETAConfidenceBandMethod,
		},
		etaRawP50Secs: job.ComputePlan.ETAP50Secs,
		ExpiresAt:     time.Now().Add(time.Hour).UTC(),
		InputSHA256:   job.WorkloadDecision.Binding.InputSHA256,
	}
	if err := store.InsertQuote(ctx, job.BuyerID, quote); err == nil ||
		!strings.Contains(err.Error(), "durable ingress") {
		t.Fatalf("expired catalogue quote insert error=%v", err)
	}
	var rows int
	must(t, store.pool.QueryRow(ctx, `SELECT count(*) FROM quotes WHERE id=$1`, quoteID).Scan(&rows))
	if rows != 0 {
		t.Fatalf("expired catalogue wrote %d quote rows", rows)
	}
}

func TestSubmitJobTxRechecksCurrentCatalogueAtDurableIngressWithZeroWrites(t *testing.T) {
	ctx, store, job, tasks, schedule := currentUniformCatalogueIngressFixture(t)
	expireCataloguePowerJustPastBoundary(t, schedule)
	if err := store.SubmitJobTx(ctx, job, tasks); err == nil ||
		!strings.Contains(err.Error(), "durable ingress") {
		t.Fatalf("expired catalogue job submit error=%v", err)
	}
	var rows int
	must(t, store.pool.QueryRow(ctx, `SELECT count(*) FROM jobs WHERE id=$1`, job.ID).Scan(&rows))
	if rows != 0 {
		t.Fatalf("expired catalogue wrote %d job rows", rows)
	}
}

func TestWithdrawnPersistedCatalogueReturnsQuoteServiceUnavailableWithZeroWrites(t *testing.T) {
	strangerDeploymentInputs(t)
	installSettlementCurrencyForTest(t, "usd")
	ctx, store, schedule := currentPhysicalCatalogueFixture(t)
	result := schedule.Results[0]
	path, _, _ := strings.Cut(result.PhysicalAuthority.Throughput.Citation, "#")
	raw, err := os.ReadFile(path)
	must(t, err)
	var receipt map[string]any
	must(t, json.Unmarshal(raw, &receipt))
	receipt["binding_status"] = BindingWithdrawn
	mutated, err := json.MarshalIndent(receipt, "", "  ")
	must(t, err)
	must(t, os.WriteFile(path, append(mutated, '\n'), 0o600))

	body, err := json.Marshal(testOnlyBatchPublicRequest(strangerBatchCorpus, 1))
	must(t, err)
	req := httptest.NewRequest(http.MethodPost, "/v1/quote", bytes.NewReader(body))
	req = req.WithContext(context.WithValue(req.Context(), ctxBuyer, &AuthResult{BuyerID: uuid.New()}))
	recorder := httptest.NewRecorder()
	NewServer(store, nil, nil, nil).handleQuote(recorder, req)
	if recorder.Code != http.StatusServiceUnavailable {
		t.Fatalf("withdrawn current catalogue quote status=%d body=%s",
			recorder.Code, recorder.Body.String())
	}
	var rows int
	must(t, store.pool.QueryRow(ctx, `SELECT count(*) FROM quotes`).Scan(&rows))
	if rows != 0 {
		t.Fatalf("withdrawn current catalogue wrote %d quotes", rows)
	}
}

func TestQuoteExpiryIsCappedAtCataloguePhysicalAuthorityBoundary(t *testing.T) {
	strangerDeploymentInputs(t)
	installSettlementCurrencyForTest(t, "usd")
	nearBoundary := time.Now().UTC().Add(2 * time.Minute).Truncate(time.Second)
	installBoundCataloguePowerAuthorityWithMutationForTest(t, func(receipt map[string]any) {
		receipt[testOnlyPowerReceiptFragment].(map[string]any)["measured_at"] =
			nearBoundary.Add(-cataloguePowerMaxAge).Format(time.RFC3339)
	})
	installTestOnlyCombinedTokenAuthority(t)
	pinBoardClockForPublication(t)
	ctx, store, pool := openIsolatedTestStore(t)
	schedule, err := BuildCataloguePriceSchedule()
	must(t, err)
	_, err = store.ApplyRepricing(ctx, schedule)
	must(t, err)
	buyerID := uuid.New()
	_, err = pool.Exec(ctx, `INSERT INTO buyers (id,email,password_hash) VALUES ($1,$2,'x')`,
		buyerID, "catalogue-expiry-cap-"+buyerID.String()+"@test")
	must(t, err)
	body, err := json.Marshal(testOnlyBatchPublicRequest(strangerBatchCorpus, 1))
	must(t, err)
	req := httptest.NewRequest(http.MethodPost, "/v1/quote", bytes.NewReader(body))
	req = req.WithContext(context.WithValue(req.Context(), ctxBuyer, &AuthResult{BuyerID: buyerID}))
	recorder := httptest.NewRecorder()
	NewServer(store, nil, nil, nil).handleQuote(recorder, req)
	if recorder.Code != http.StatusOK {
		t.Fatalf("near-boundary quote status=%d body=%s", recorder.Code, recorder.Body.String())
	}
	var quote Quote
	must(t, json.Unmarshal(recorder.Body.Bytes(), &quote))
	if !quote.ExpiresAt.Equal(nearBoundary) ||
		quote.ExpiresAt.After(time.Now().UTC().Add(quoteTTL)) {
		t.Fatalf("quote expiry=%s want physical boundary=%s", quote.ExpiresAt, nearBoundary)
	}
}

func TestQuoteExpiryIsCappedAtMeasuredPlacementRevalidationBoundary(t *testing.T) {
	strangerDeploymentInputs(t)
	installSettlementCurrencyForTest(t, "usd")
	previousActivation := activeRuntimeActivation.Load()
	t.Cleanup(func() { activeRuntimeActivation.Store(previousActivation) })
	ctx, store, pool := openIsolatedTestStore(t)
	boundary := time.Now().UTC().Add(2 * time.Minute).Truncate(time.Second)
	measuredAt := boundary.Add(-benchmarkRevalidationWindow)
	previousPerformanceNow := runtimeCellPerformanceNow
	runtimeCellPerformanceNow = func() time.Time { return measuredAt }
	t.Cleanup(func() { runtimeCellPerformanceNow = previousPerformanceNow })
	installBoundCataloguePublicationAuthorityForTest(t)
	installTestOnlyCombinedTokenAuthority(t)
	installed := currentActivation()
	activeRuntimeActivation.Store(newRuntimeActivation(
		installed.PolicyRevision, map[string]string{}, nil))
	pinBoardClockForPublication(t)
	schedule, err := BuildCataloguePriceSchedule()
	must(t, err)
	_, err = store.ApplyRepricing(ctx, schedule)
	must(t, err)
	buyerID := uuid.New()
	_, err = pool.Exec(ctx, `INSERT INTO buyers (id,email,password_hash) VALUES ($1,$2,'x')`,
		buyerID, "placement-expiry-cap-"+buyerID.String()+"@test")
	must(t, err)
	body, err := json.Marshal(testOnlyBatchPublicRequest(strangerBatchCorpus, 1))
	must(t, err)
	req := httptest.NewRequest(http.MethodPost, "/v1/quote", bytes.NewReader(body))
	req = req.WithContext(context.WithValue(req.Context(), ctxBuyer, &AuthResult{BuyerID: buyerID}))
	recorder := httptest.NewRecorder()
	NewServer(store, nil, nil, nil).handleQuote(recorder, req)
	if recorder.Code != http.StatusOK {
		t.Fatalf("near-placement-boundary quote status=%d body=%s", recorder.Code, recorder.Body.String())
	}
	var quote Quote
	must(t, json.Unmarshal(recorder.Body.Bytes(), &quote))
	if !quote.ExpiresAt.Equal(boundary) {
		t.Fatalf("quote expiry=%s want placement revalidation boundary=%s",
			quote.ExpiresAt, boundary)
	}
	if err := validateCurrentPlacementPerformanceAuthorityAt(quote.Placement, boundary); err != nil {
		t.Fatalf("placement must remain current at its exact advertised boundary: %v", err)
	}
	if err := validateCurrentPlacementPerformanceAuthorityAt(
		quote.Placement, boundary.Add(time.Nanosecond),
	); err == nil {
		t.Fatal("placement remained current after its advertised revalidation boundary")
	}
}

func TestQuoteDurablePlacementRecheckClosesClockRaceWith503AndZeroWrites(t *testing.T) {
	strangerDeploymentInputs(t)
	installSettlementCurrencyForTest(t, "usd")
	installBoundCataloguePublicationAuthorityForTest(t)
	installTestOnlyCombinedTokenAuthority(t)
	pinBoardClockForPublication(t)
	ctx, store, pool := openIsolatedTestStore(t)
	schedule, err := BuildCataloguePriceSchedule()
	must(t, err)
	_, err = store.ApplyRepricing(ctx, schedule)
	must(t, err)

	sub := testOnlyCombinedTokenSubmit(t)
	workload, err := buildWorkloadDecision(sub, strings.Repeat("a", 64))
	must(t, err)
	_, performance, err := admissionUnitsPerSec(
		workload.RuntimeJobType, workload.Binding.Model.Ref,
		admissionCellsForWorkload(workload), time.Now(),
	)
	must(t, err)
	measuredAt, err := time.Parse(time.RFC3339, performance.BenchmarkedAt)
	must(t, err)
	beforeBoundary := measuredAt.Add(time.Hour)
	afterBoundary := measuredAt.Add(benchmarkRevalidationWindow + time.Nanosecond)
	previousClock := runtimeCellPerformanceNow
	previousHook := durableAdmissionPhysicalRecheckHook
	runtimeCellPerformanceNow = func() time.Time { return beforeBoundary }
	durableAdmissionPhysicalRecheckHook = func() {
		runtimeCellPerformanceNow = func() time.Time { return afterBoundary }
		durableAdmissionPhysicalRecheckHook = nil
	}
	t.Cleanup(func() {
		runtimeCellPerformanceNow = previousClock
		durableAdmissionPhysicalRecheckHook = previousHook
	})

	buyerID := uuid.New()
	_, err = pool.Exec(ctx, `INSERT INTO buyers (id,email,password_hash) VALUES ($1,$2,'x')`,
		buyerID, "placement-race-quote-"+buyerID.String()+"@test")
	must(t, err)
	body, err := json.Marshal(testOnlyBatchPublicRequest(strangerBatchCorpus, 1))
	must(t, err)
	req := httptest.NewRequest(http.MethodPost, "/v1/quote", bytes.NewReader(body))
	req = req.WithContext(context.WithValue(req.Context(), ctxBuyer, &AuthResult{BuyerID: buyerID}))
	recorder := httptest.NewRecorder()
	NewServer(store, nil, nil, nil).handleQuote(recorder, req)
	if recorder.Code != http.StatusServiceUnavailable {
		t.Fatalf("placement clock race status=%d body=%s", recorder.Code, recorder.Body.String())
	}
	var rows int
	must(t, pool.QueryRow(ctx, `SELECT count(*) FROM quotes`).Scan(&rows))
	if rows != 0 {
		t.Fatalf("placement clock race wrote %d quote rows", rows)
	}
}
