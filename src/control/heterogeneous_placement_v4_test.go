package main

import (
	"strings"
	"testing"

	"github.com/google/uuid"
)

const (
	embedCosineQualityContractID = "embed-cosine-v2-all-minilm-l6-v2"
	cudaEmbedPeerCellID          = "vllm-cuda-minilm-embed"
)

// advertiseTestOnlyCUDAEmbedPeer adds the DRAFT CUDA embed identity to the
// process advertised projection so a test can freeze a multi-family embed
// workload. It does not change document lifecycle, does not mark the cell
// routable, and does not promote. Production advertisement stays Metal-only.
func advertiseTestOnlyCUDAEmbedPeer(t *testing.T) {
	t.Helper()
	var peer generatedRuntimeCapability
	found := false
	for _, cell := range generatedCapabilityRuntimeCells {
		if cell.ID == cudaEmbedPeerCellID {
			peer = cell
			found = true
			break
		}
	}
	if !found {
		t.Fatal("capability projection is missing vllm-cuda-minilm-embed")
	}
	if peer.Job != "embed" || peer.Model != "all-minilm-l6-v2" || peer.Device != "cuda" {
		t.Fatalf("CUDA embed peer drifted: %+v", peer)
	}
	current := currentActivation()
	for _, advertised := range current.advertised {
		if advertised.ID == cudaEmbedPeerCellID {
			return
		}
	}
	next := *current
	next.advertised = append(append([]generatedRuntimeCapability(nil), current.advertised...), peer)
	previous := activeRuntimeActivation.Load()
	activeRuntimeActivation.Store(&next)
	t.Cleanup(func() { activeRuntimeActivation.Store(previous) })
}

func multiFamilyEmbedSubmit(t *testing.T) jobSubmit {
	t.Helper()
	sub, herr := normalizeAndValidateJobSubmit(jobSubmit{
		JobType: JobType{Type: "embed"},
		Model:   ModelRef{Kind: "hf", Ref: "all-minilm-l6-v2"},
		Constraints: JobConstraints{
			MaxDurationSecs: 3600,
		},
		Tier: "batch",
	})
	if herr != nil {
		t.Fatalf("normalize multi-family embed submit: %s", herr.msg)
	}
	return sub
}

func buildQuotedMultiFamilyEmbed(t *testing.T) (jobSubmit, WorkloadDecision, PlacementRequirement) {
	t.Helper()
	installBoundCataloguePublicationAuthorityForTest(t)
	advertiseTestOnlyCUDAEmbedPeer(t)

	candleProfile, candle := cellByID(t, candleEmbedCell)
	cudaProfile, cuda := cellByID(t, cudaEmbedPeerCellID)
	if candle.EffectiveLifecycle(candleProfile) != runtimeLifecycleActive {
		t.Fatal("candle embed must remain the advertised pricing primary")
	}
	if cuda.EffectiveLifecycle(cudaProfile) != runtimeLifecycleDraft {
		t.Fatalf("CUDA embed lifecycle = %s; test must not promote",
			cuda.EffectiveLifecycle(cudaProfile))
	}
	sub := multiFamilyEmbedSubmit(t)
	workload, err := buildWorkloadDecision(sub, strings.Repeat("a", 64))
	mustf(t, err, "build multi-family embed workload: %v")
	if len(workload.RuntimeCandidates) < 2 {
		t.Fatalf("multi-family embed workload froze %d candidates: %+v",
			len(workload.RuntimeCandidates), workload.RuntimeCandidates)
	}
	if workload.QualityContractID != embedCosineQualityContractID {
		t.Fatalf("quality_contract_id=%q want %s",
			workload.QualityContractID, embedCosineQualityContractID)
	}
	ids := make([]string, 0, len(workload.RuntimeCandidates))
	devices := map[string]bool{}
	for _, c := range workload.RuntimeCandidates {
		ids = append(ids, c.CellID)
		devices[c.Device] = true
	}
	if !devices["metal"] || !devices["cuda"] {
		t.Fatalf("eligible set is not Metal+CUDA: %+v", workload.RuntimeCandidates)
	}
	if _, err := qualityContractAuthorizingMultiFamily(workload.QualityContractID, ids); err != nil {
		t.Fatalf("embed cosine contract must authorize the freeze: %v", err)
	}

	placement, err := placementRequirementFor(sub, workload, 1)
	mustf(t, err, "quote placementRequirementFor: %v")
	if placement.Version != placementRequirementVersionMultiFamily {
		t.Fatalf("quoted placement version=%d want %d",
			placement.Version, placementRequirementVersionMultiFamily)
	}
	return sub, workload, placement
}

func TestMultiFamilyEmbedPlacementV4BindsHeterogeneousEligibleSet(t *testing.T) {
	// Store startup reloads activation from the document. Open it first, then
	// install the TEST_ONLY publication + CUDA overlay so persist reconstructs
	// the same multi-family workload.
	previousActivation := activeRuntimeActivation.Load()
	t.Cleanup(func() { activeRuntimeActivation.Store(previousActivation) })
	ctx, store, pool := openIsolatedMoneyPathStore(t)
	pinBoardClockForPublication(t)
	sub, workload, placement := buildQuotedMultiFamilyEmbed(t)

	// Fail-before this change: digest and catalogue/placement binding refuse
	// version 4 even though quote.go already writes the multi-family shape.
	digest, digestErr := placementRequirementDigest(placement)
	if digestErr != nil {
		t.Fatalf("placementRequirementDigest refused v4 (fail-before shape): %v", digestErr)
	}
	if !validSHA256(digest) {
		t.Fatalf("placement v4 digest is not a sha256: %q", digest)
	}

	schedule, err := BuildCataloguePriceSchedule()
	mustf(t, err, "build embed catalogue schedule: %v")
	if _, err := store.ApplyRepricing(ctx, schedule); err != nil {
		t.Fatalf("apply embed catalogue schedule: %v", err)
	}
	// Re-apply the overlay if schedule publish refreshed activation.
	advertiseTestOnlyCUDAEmbedPeer(t)
	catalogue, err := store.LoadCataloguePriceAuthority(ctx, workload.Binding.Model.Ref)
	mustf(t, err, "load embed catalogue authority: %v")

	if err := validateCurrentPlacementCataloguePhysicalAuthority(placement, catalogue); err != nil {
		t.Fatalf("catalogue/placement binding refused v4 (fail-before shape): %v", err)
	}

	ceiling, err := supplierAdmissionCeilingUSDHr(
		catalogue, workload.RuntimeJobType, workload.Binding.Tier,
		[]string{workload.RuntimeCandidates[0].CellID},
		sub.Constraints.HWClasses,
	)
	mustf(t, err, "preferred-cell supplier ceiling: %v")
	priced, err := placementRequirementFor(sub, workload, float32(ceiling))
	mustf(t, err, "re-quote placement at catalogue ceiling: %v")
	if priced.Version != placementRequirementVersionMultiFamily {
		t.Fatalf("priced placement version=%d want %d",
			priced.Version, placementRequirementVersionMultiFamily)
	}

	fixture := seedMoneyPathFixture(t, ctx, store, pool, moneyPathSeedOpts{TaskCount: 2})
	if code := SettlementCurrencyCode(); code != "" && fixture.Plan.Schedule.Currency != code {
		aligned := fixture.Plan.Schedule
		aligned.Currency = code
		plan := BuildEconomicPlan(fixture.Plan.Input, aligned)
		if !plan.Executable {
			t.Fatalf("align embed economics to %q: %s", code, plan.BlockReason)
		}
		fixture.Plan = plan
	}
	tasks := makeTasks(fixture, 2)
	tasks[0].IsRedundancy = false
	tasks[1].IsRedundancy = true
	tasks[1].InputRef = tasks[0].InputRef
	tasks[1].InputDepthBand = tasks[0].InputDepthBand
	tasks[1].ChunkIndex = tasks[0].ChunkIndex
	for i := range tasks {
		tasks[i].InputSHA256 = workload.Binding.InputSHA256
		if tasks[i].ExpectedOutputRecords <= 0 {
			tasks[i].ExpectedOutputRecords = 1
		}
	}

	economicInput := fixture.Plan.Input
	economicInput.SupplierShare = catalogue.SupplierShare
	economicInput.ExtraTaskReserve = economicExtraTaskReserve(1)
	economicSchedule := fixture.Plan.Schedule
	economicSchedule.Currency = catalogue.SettlementCurrency
	economic := BuildEconomicPlan(economicInput, economicSchedule)
	if !economic.Executable {
		t.Fatalf("embed multi-family economics blocked: %s", economic.BlockReason)
	}
	compute, err := newDistributedComputePlan(
		workload,
		1,
		128,
		testInputDepthProfile(1),
		1,
		1,
		1,
		0,
		quoteTimeFromETABands(60, 0, false),
		"static",
		economic.Input.BaseComputeUSD,
		0,
		QuoteConfidence{Score: 0.9, Reasons: []string{"multi-family embed v4 fixture"}},
		[]string{"TEST_ONLY multi-family embed placement; CUDA peer is not promoted"},
	)
	mustf(t, err, "build embed compute plan: %v")
	mustf(t, ValidateComputePlanEconomicSnapshot(compute, workload, economic),
		"embed compute/economic authority: %v")

	pricing, err := newDistributedPricingDecision(
		workload, compute, priced, economic, catalogue, workload.Binding.Tier, "",
	)
	mustf(t, err, "quote newDistributedPricingDecision for v4: %v")

	job := &jobRow{
		ID:                   fixture.JobID,
		BuyerID:              fixture.BuyerID,
		JobType:              workload.RuntimeJobType,
		ModelRef:             workload.Binding.Model.Ref,
		InputRef:             "hetero-v4/input-" + fixture.JobID.String(),
		OutputRef:            "hetero-v4/output-" + fixture.JobID.String(),
		Tier:                 "batch",
		EstimatedUSD:         economic.InitialBuyerChargeUSD,
		TaskCount:            2,
		MinMemoryGB:          priced.MinMemoryGB,
		HWClasses:            append([]string(nil), priced.HWClasses...),
		MaxDurationSecs:      3600,
		SplitSize:            1,
		OfferedRateUsdHr:     priced.OfferedRateUsdHr,
		ETASecs:              60,
		ETARawSecs:           60,
		EconomicInputRecords: 1,
		EconomicInputBytes:   128,
		EconomicInputSource:  economicInputSourceSubmitStream,
		EconomicPlan:         economic,
		WorkloadDecision:     workload,
		ComputePlan:          compute,
		PlacementRequirement: priced,
		PricingDecision:      pricing,
	}
	for _, identity := range [][2]uuid.UUID{
		{fixture.WorkerID, fixture.SupplierID},
		{fixture.OtherWorkerID, fixture.OtherSupplierID},
	} {
		capability := currentIdentityWorkerCapability(
			identity[0], identity[1], priced, job,
		)
		// Union HW lists the eligible claim set. Worker identity still binds
		// the preferred measured class — the pricing pin — not HWClasses[0].
		if priced.PerformanceAuthority != nil {
			capability.HWClass = priced.PerformanceAuthority.Performance.MeasuredOnHWClass
		}
		mustf(t, store.UpsertWorker(ctx, capability), "register v4 fixture worker: %v")
	}

	mustf(t, store.SubmitJobTx(ctx, job, tasks), "persist multi-family embed job: %v")

	activationRev := activationAdmissionRevision(job.activationPolicyRevision)
	if activationRev <= 0 {
		activationRev = 1
	}
	decision, err := buildBatchRuntimeDecision(workload, priced, activationRev)
	mustf(t, err, "buildBatchRuntimeDecision: %v")
	if decision.SelectionBasis != runtimeSelectionBasisHeterogeneousEligibleSet {
		t.Fatalf("selection_basis=%q want %s",
			decision.SelectionBasis, runtimeSelectionBasisHeterogeneousEligibleSet)
	}
	if decision.QualityContractID != embedCosineQualityContractID {
		t.Fatalf("runtime quality_contract_id=%q want %s",
			decision.QualityContractID, embedCosineQualityContractID)
	}
	if len(decision.EligibleCellIDs) < 2 {
		t.Fatalf("eligible_cell_ids=%v want Metal+CUDA set", decision.EligibleCellIDs)
	}

	// Production document is unchanged: CUDA embed remains DRAFT identity.
	cudaProfile, cuda := cellByID(t, cudaEmbedPeerCellID)
	if cuda.EffectiveLifecycle(cudaProfile) != runtimeLifecycleDraft {
		t.Fatal("CUDA embed was promoted; this seam must not promote")
	}
}

func TestPlacementRequirementDigestRefusesUnknownVersion(t *testing.T) {
	_, _, placement := buildQuotedMultiFamilyEmbed(t)
	placement.Version = 5
	if _, err := placementRequirementDigest(placement); err == nil ||
		!strings.Contains(err.Error(), "unsupported placement requirement version 5") {
		t.Fatalf("digest must refuse unknown version 5: %v", err)
	}
}

func TestMultiFamilyPlacementV4RefusesGenerationQ4VsBF16(t *testing.T) {
	installBoundCataloguePublicationAuthorityForTest(t)
	sub := testOnlyCombinedTokenSubmit(t)
	workload, err := buildWorkloadDecision(sub, strings.Repeat("c", 64))
	mustf(t, err, "generation workload: %v")
	if workload.RuntimeJobType != "batch_infer" {
		t.Fatalf("job=%q want batch_infer", workload.RuntimeJobType)
	}
	placement, err := placementRequirementFor(sub, workload, 1)
	mustf(t, err, "generation placement: %v")
	if placement.Version == placementRequirementVersionMultiFamily {
		t.Fatal("generation quote wrote multi-family v4; q4 and bf16 are not interchangeable")
	}

	forged := placement
	forged.Version = placementRequirementVersionMultiFamily
	forged.HWClasses = []string{"apple_silicon_ultra", "nvidia_48gb"}
	if err := validatePlacementRequirement(forged, workload); err == nil {
		t.Fatal("forged generation v4 placement must not validate")
	}
	if _, err := qualityContractAuthorizingMultiFamily(
		generationQ4VsBF16Refused().ID,
		[]string{"candle-metal-llama1-infer", "vllm-cuda-llama1-infer"},
	); err == nil {
		t.Fatal("REFUSED q4-vs-bf16 contract must not authorize a v4 bind")
	}
}

func TestMultiFamilyPlacementV4RefusesContractAndHWMutations(t *testing.T) {
	_, workload, placement := buildQuotedMultiFamilyEmbed(t)
	mustf(t, validatePlacementRequirement(placement, workload), "valid v4 placement refused: %v")

	missingContract := workload
	missingContract.QualityContractID = ""
	if err := validatePlacementRequirement(placement, missingContract); err == nil {
		t.Fatal("v4 placement without a quality contract must not bind")
	}

	refused := workload
	refused.QualityContractID = generationQ4VsBF16Refused().ID
	if err := validatePlacementRequirement(placement, refused); err == nil {
		t.Fatal("v4 placement under the REFUSED q4-vs-bf16 contract must not bind")
	}

	stripped := placement
	stripped.HWClasses = []string{placement.PerformanceAuthority.Performance.MeasuredOnHWClass}
	if err := validatePlacementRequirement(stripped, workload); err == nil {
		t.Fatal("v4 placement that drops the eligible HW union must not bind")
	}
}
