package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
)

// fabricatedNarrowPassedPromotionEvidence constructs the strongest receipt the
// current wire type can express. It is deliberately fabricated rather than
// returned by EvaluateCellPromotion: gate v4 cannot pass without durable
// matched-pair input/cohort authority, and this type has nowhere to carry it.
// Activation tests use this value to prove that even a cryptographically
// self-consistent narrow "pass" cannot be upgraded into global routability.
func fabricatedNarrowPassedPromotionEvidence(t *testing.T) CellPromotionEvidence {
	t.Helper()
	now := time.Now().UTC().Truncate(time.Microsecond)
	revision := currentActivation().PolicyRevision
	const (
		currency  = "usd"
		liability = 0.000001
	)
	geometry := strings.Repeat("1", 64)
	schedule := strings.Repeat("2", 64)
	proxy := func(cellID, runtimeID, engine string, median float64) MeasuredSupplierLiabilityProxy {
		return MeasuredSupplierLiabilityProxy{
			CellID: cellID, RuntimeID: runtimeID, Engine: engine,
			JobType: "embed", ModelRef: "all-minilm-l6-v2",
			HWClass: "apple_silicon_ultra", HardwareIdentity: "Apple M3 Ultra",
			Currency: currency,
			Samples:  minSupplierLiabilitySamples, Units: 2000,
			MedianMsPerUnit: median, SupplierUSDPerUnit: liability,
			VerificationSamples:           minSupplierLiabilitySamples,
			TerminalAttempts:              minSupplierLiabilitySamples,
			Measured:                      true,
			SourceBinding:                 BindingBound,
			ExecutionBuildHash:            "synthetic-activation-refusal-fixture",
			RuntimeMatrixSHA256:           generatedRuntimeMatrixSHA256,
			InputGeometrySHA256:           geometry,
			UnknownPlatformCostComponents: unknownPlatformCostComponents(),
		}
	}
	challenger := proxy(llamaEmbedCell, "llama_cpp_metal", "llama_cpp", 1)
	incumbent := proxy(candleEmbedCell, "candle_metal", "candle", 4)
	evidence := CellPromotionEvidence{
		GateVersion: promotionGateVersion, EvaluatedAt: now,
		Scope: CellPromotionScope{
			JobType: "embed", ModelRef: "all-minilm-l6-v2",
			ModelRevision: modelRevisionFor("all-minilm-l6-v2"), Tier: "batch",
			QualityTier: "OUTCOME_EQUIVALENT", Verification: "cosine",
			HWClass: "apple_silicon_ultra", HardwareIdentity: "Apple M3 Ultra",
			LatencyClass: "standard_batch",
			RuntimeID:    "llama_cpp_metal", CellID: llamaEmbedCell,
			Currency: currency, CatalogueScheduleSHA256: schedule,
			RuntimeMatrixSHA256: generatedRuntimeMatrixSHA256,
			SelectionPolicy:     shadowSelectionPolicy, PolicyRevision: revision,
			ObservedAfter:  now.Add(-supplierLiabilityObservationWindow),
			ObservedBefore: now,
		},
		IncumbentCell:                                 candleEmbedCell,
		ChallengerSupplierLiability:                   challenger,
		IncumbentSupplierLiability:                    incumbent,
		ChallengerSupplierLiabilityUSDPerVerifiedUnit: liability,
		IncumbentSupplierLiabilityUSDPerVerifiedUnit:  liability,
		SupplierLiabilityReductionFraction:            0,
		RequiredMarginFraction:                        promotionThroughputMarginFraction,
		Basis:                                         promotionBasisThroughput,
		ThroughputGainFraction:                        0.75,
		LatencyRatio:                                  0.25,
		LiabilityRegret: SelectorLiabilityRegret{
			JobType: "embed", ModelRef: "all-minilm-l6-v2",
			HWClass: "apple_silicon_ultra", HardwareIdentity: "Apple M3 Ultra",
			Currency:  currency,
			Decisions: 1, ScoredDecisions: 1,
			ExactPairDecisions: 1, ExactPairScoredDecisions: 1,
		},
		RollbackTargetRevision: revision,
		RuntimeMatrixSHA256:    generatedRuntimeMatrixSHA256,
		PolicyRevision:         revision,
	}
	evidence.UnknownPlatformCostComponents = unresolvedPlatformCostComponents(
		challenger, incumbent)
	return evidence
}

func recordFabricatedNarrowPassedPromotionEvidence(
	t *testing.T, ctx context.Context, store *Store,
	mutate func(*CellPromotionEvidence),
) (string, CellPromotionEvidence) {
	t.Helper()
	evidence := fabricatedNarrowPassedPromotionEvidence(t)
	if mutate != nil {
		mutate(&evidence)
	}
	inserted, err := store.RecordCellPromotionEvaluation(ctx, evidence)
	mustf(t, err, "record narrow promotion fixture: %v")
	if !inserted {
		t.Fatal("narrow promotion fixture collided with an existing receipt")
	}
	ref, err := evidence.ReceiptRef()
	mustf(t, err, "derive narrow promotion fixture ref: %v")
	return ref, evidence
}

// withActivationRestored makes the process-wide policy snapshot safe to mutate.
//
// The snapshot is global on purpose — it is what every projection reads — so a
// test that installs one has to put the previous one back or it leaks into every
// test that runs after it in the same binary.
func withActivationRestored(t *testing.T) {
	t.Helper()
	previous := activeRuntimeActivation.Load()
	t.Cleanup(func() { activeRuntimeActivation.Store(previous) })
}

func openActivationStore(t *testing.T) (context.Context, *Store, *pgxpool.Pool) {
	t.Helper()
	withActivationRestored(t)
	// Checked-in production evidence is correctly unbindable. Activation-policy
	// mechanics tests that need an initially-advertised document ACTIVE cell
	// install explicit TEST_ONLY publication authority before Migrate so the
	// registry seeds ACTIVE+routable instead of honest QUARANTINED.
	installBoundCataloguePublicationAuthorityForTest(t)
	return openIsolatedTestStore(t)
}

// openActivationStoreWithoutPublication is for the tests that resolve benchmark
// identity for two cells and require them to share hardware. The publication
// authority above pins its cell to apple_silicon_pro while the other TEST_ONLY
// identity is an M3 Ultra, so installing it would make a matched-hardware
// comparison impossible — and that guard is Step 4's rule that an Ultra
// measurement may not stand in for another class. These tests want the honest
// unpublished registry, which is also what they had before the publication seam
// existed.
func openActivationStoreWithoutPublication(t *testing.T) (context.Context, *Store, *pgxpool.Pool) {
	t.Helper()
	withActivationRestored(t)
	return openIsolatedTestStore(t)
}

// The seed must reproduce the document exactly, or the split has changed what the
// product does on day one rather than only how it is governed.
func TestActivationPolicySeedsFromTheDocumentAndProjectsBack(t *testing.T) {
	ctx, store, pool := openActivationStore(t)

	entries, err := store.CurrentActivationPolicy(ctx)
	must(t, err)
	want, err := documentActivationEntries()
	must(t, err)
	if len(entries) != len(want) {
		t.Fatalf("policy holds %d statements, document declares %d", len(entries), len(want))
	}
	byKey := map[string]ActivationPolicyEntry{}
	for _, entry := range entries {
		byKey[activationKey(entry.RuntimeProfileID, entry.CellID)] = entry
	}
	for _, expected := range want {
		key := activationKey(expected.RuntimeProfileID, expected.CellID)
		got, ok := byKey[key]
		if !ok {
			t.Fatalf("%s has no activation policy", key)
		}
		if got.Lifecycle != expected.Lifecycle {
			t.Errorf("%s is %s in policy and %s in the document",
				key, got.Lifecycle, expected.Lifecycle)
		}
		if got.Source != activationSourceDocument {
			t.Errorf("%s was seeded from %q", key, got.Source)
		}
	}

	// And the registry's denormalized columns agree with policy, because the
	// scheduler reads those and a cache that disagrees with its source is worse
	// than no cache.
	assertRegistryMatchesPolicy(t, ctx, pool)
}

// CanaryAllowlist and CanaryTrafficPct are stored evidence today; no admission
// path consumes them. A CANARY cell must therefore remain directed-only rather
// than entering the ordinary catalogue unrestricted. Use the known-bindable
// Candle embed cell so authority binding cannot accidentally be the reason for
// the demotion.
func TestCanaryCellIsDirectedButNeverOrdinarilyAdvertised(t *testing.T) {
	activation := newRuntimeActivation(7, map[string]string{
		activationKey("candle_metal", candleEmbedCell): runtimeLifecycleCanary,
	}, nil)
	for _, capability := range activation.advertised {
		if capability.ID == candleEmbedCell {
			t.Fatal("CANARY cell entered ordinary advertised routing without a cohort/% consumer")
		}
	}
	directed := false
	for _, capability := range activation.directed {
		if capability.ID == candleEmbedCell {
			directed = true
			break
		}
	}
	if !directed {
		t.Fatal("CANARY cell was removed from directed evidence collection")
	}

	entries, err := documentActivationEntries()
	must(t, err)
	for _, entry := range entries {
		if entry.CellID != "" && entry.Lifecycle == runtimeLifecycleCanary && entry.Routable {
			t.Fatalf("document-seeded CANARY cell %s is marked ordinarily routable", entry.CellID)
		}
	}
}

// CellPromotionEvidence is exact to one traffic/hardware cohort, while a cell
// lifecycle changes routing for every cohort. Even a self-consistent narrow
// "pass" cannot bridge that coverage boundary (and gate v4 independently lacks
// durable matched-pair authority).
func TestNarrowPromotionReceiptCannotWriteGlobalActivationPolicy(t *testing.T) {
	// The checked-in r2 comparison remains truthful history and intentionally
	// lacks the exact build/device identity required for current authority. This
	// test is about the later coverage boundary, so install ephemeral per-cell
	// TEST_ONLY identities before the isolated registry is seeded. Nothing under
	// evidence/ is relabelled and production remains non-routable.
	installTestOnlyExactIdentityForLegacyBenchmark(t, candleEmbedCell)
	installTestOnlyExactIdentityForLegacyBenchmark(t, llamaEmbedCell)
	ctx, store, pool := openActivationStoreWithoutPublication(t)

	profile, ok := runtimeProfileByID("llama_cpp_metal")
	if !ok {
		t.Fatal("llama_cpp_metal is not registered")
	}
	before, err := profile.CapabilityDigest(runtimeAuthorityModels)
	must(t, err)
	if got := lifecycleOfCell(t, ctx, pool, "llama_cpp_metal", llamaEmbedCell); got != runtimeLifecycleRealRuntimeProven {
		t.Fatalf("the embed cell starts at %s, not REAL_RUNTIME_PROVEN", got)
	}
	if cellIsAdvertised(llamaEmbedCell) {
		t.Fatal("the llama.cpp embed cell is advertised before any promotion")
	}

	var beforeRevision int64
	must(t, pool.QueryRow(ctx,
		`SELECT MAX(policy_revision) FROM runtime_activation_policies`).Scan(&beforeRevision))
	_, _, incumbentAuthority, err := currentRuntimeCellBenchmarkIdentity(candleEmbedCell)
	mustf(t, err, "resolve TEST_ONLY incumbent identity: %v")
	_, _, challengerAuthority, err := currentRuntimeCellBenchmarkIdentity(llamaEmbedCell)
	mustf(t, err, "resolve TEST_ONLY challenger identity: %v")
	if incumbentAuthority.HWClass != challengerAuthority.HWClass ||
		incumbentAuthority.HardwareIdentity != challengerAuthority.HardwareIdentity {
		t.Fatalf("TEST_ONLY comparison identities do not share hardware: incumbent=%s/%s challenger=%s/%s",
			incumbentAuthority.HWClass, incumbentAuthority.HardwareIdentity,
			challengerAuthority.HWClass, challengerAuthority.HardwareIdentity)
	}
	receipt, _ := recordFabricatedNarrowPassedPromotionEvidence(t, ctx, store,
		func(evidence *CellPromotionEvidence) {
			evidence.Scope.HWClass = challengerAuthority.HWClass
			evidence.Scope.HardwareIdentity = challengerAuthority.HardwareIdentity
			evidence.LiabilityRegret.HWClass = challengerAuthority.HWClass
			evidence.LiabilityRegret.HardwareIdentity = challengerAuthority.HardwareIdentity

			evidence.IncumbentSupplierLiability.HWClass = incumbentAuthority.HWClass
			evidence.IncumbentSupplierLiability.HardwareIdentity = incumbentAuthority.HardwareIdentity
			evidence.IncumbentSupplierLiability.ExecutionBuildHash = incumbentAuthority.EngineBuildHash
			evidence.IncumbentSupplierLiability.ExecutionBuildIdentityPolicy = incumbentAuthority.EngineBuildIdentityPolicy

			evidence.ChallengerSupplierLiability.HWClass = challengerAuthority.HWClass
			evidence.ChallengerSupplierLiability.HardwareIdentity = challengerAuthority.HardwareIdentity
			evidence.ChallengerSupplierLiability.ExecutionBuildHash = challengerAuthority.EngineBuildHash
			evidence.ChallengerSupplierLiability.ExecutionBuildIdentityPolicy = challengerAuthority.EngineBuildIdentityPolicy
		})
	_, err = store.ApplyActivationPolicy(ctx, []ActivationPolicyEntry{{
		RuntimeProfileID: "llama_cpp_metal",
		CellID:           llamaEmbedCell,
		Lifecycle:        runtimeLifecycleCanary,
		PromotionReceipt: receipt,
		CanaryAllowlist:  []string{"proof-cohort"},
		CanaryTrafficPct: 5,
	}}, "narrow receipt must not become a global canary")
	if err == nil {
		t.Fatal("a narrow promotion receipt made a cell globally routable")
	}
	for _, want := range []string{"no durable matched", "exact scope", "global"} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("promotion refusal does not name %q: %v", want, err)
		}
	}
	var afterRevision int64
	must(t, pool.QueryRow(ctx,
		`SELECT MAX(policy_revision) FROM runtime_activation_policies`).Scan(&afterRevision))
	if afterRevision != beforeRevision {
		t.Fatalf("refused promotion wrote policy revision %d after %d", afterRevision, beforeRevision)
	}
	if got := lifecycleOfCell(t, ctx, pool, "llama_cpp_metal", llamaEmbedCell); got != runtimeLifecycleRealRuntimeProven {
		t.Fatalf("refused promotion moved the cell to %s", got)
	}
	if cellIsAdvertised(llamaEmbedCell) {
		t.Fatal("a refused narrow promotion reached the advertised projection")
	}
	assertRegistryMatchesPolicy(t, ctx, pool)

	// The identity every agent compares did not.
	after, err := profile.CapabilityDigest(runtimeAuthorityModels)
	must(t, err)
	if after != before {
		t.Fatal("a refused promotion moved the profile capability digest")
	}
	if generatedRuntimeMatrixSHA256 != pinnedCapabilityMatrixDigest {
		t.Fatal("a refused promotion moved the capability matrix digest every agent binds")
	}
}

// The last compile-time lifecycle coupling: a cell the agent's embedded document
// calls DRAFT.
//
// Enrolment used to validate a worker's declaration against the DIRECTED set, so
// a DRAFT cell could not be declared by any agent — and promoting it out of DRAFT
// by policy therefore still needed the fleet rebuilt and redeployed first. A
// worker now declares CAPABILITY and the control plane authorizes against policy,
// so the promotion is the only thing that has to happen.
func TestPromotingOutOfDraftNeedsNoNewAgentBuild(t *testing.T) {
	ctx, store, _ := openActivationStore(t)

	cuda := WorkerCapability{
		HWClass: "nvidia_24gb", Engine: "vllm", MemoryGB: 24,
		// Shape validation requires a 16-char build hash before activation is
		// considered; the refusal under test is the DRAFT lifecycle, not the
		// build-hash shape gate.
		BuildHash:           "0123456789abcdef",
		BuildIdentityPolicy: currentEngineBuildIdentityPolicy,
		HardwareIdentity:    testOnlyHardwareIdentity,
		SupportedJobs:       []string{"batch_infer"},
		SupportedModels:     []string{"llama-3.2-1b-instruct-q4"},
	}
	// Declaring a DRAFT cell is legitimate — the host really can serve it. What
	// it cannot do yet is be authorized for anything.
	if _, err := projectWorkerRuntimeCapabilities(cuda); err == nil {
		t.Fatal("a DRAFT cell was authorized before any policy promoted it")
	} else if !strings.Contains(err.Error(), "is activated") {
		t.Fatalf("the refusal was about the declaration rather than activation: %v", err)
	}

	if _, err := store.ApplyActivationPolicy(ctx, []ActivationPolicyEntry{
		{RuntimeProfileID: "vllm_cuda", Lifecycle: runtimeLifecycleValidated},
		{RuntimeProfileID: "vllm_cuda", CellID: "vllm-cuda-llama1-infer",
			Lifecycle: runtimeLifecycleValidated},
	}, "registered for directed proof runs"); err != nil {
		t.Fatalf("promote out of DRAFT: %v", err)
	}

	projected, err := projectWorkerRuntimeCapabilities(cuda)
	mustf(t, err, "after promotion the same worker still cannot enrol: %v")
	if len(projected) != 1 || projected[0].ID != "vllm-cuda-llama1-infer" {
		t.Fatalf("projected %+v", projected)
	}
	// Directed, never advertised. VALIDATED is not routable.
	if cellIsAdvertised("vllm-cuda-llama1-infer") {
		t.Fatal("a VALIDATED cell reached the buyer-visible catalogue")
	}
	if generatedRuntimeMatrixSHA256 != pinnedCapabilityMatrixDigest {
		t.Fatal("promoting out of DRAFT moved the capability matrix digest")
	}
}

// Policy is append-only in the database, not merely by convention in Go.
func TestActivationPolicyRowsCannotBeRewrittenOrDeleted(t *testing.T) {
	ctx, _, pool := openActivationStore(t)

	for name, statement := range map[string]string{
		"update": `UPDATE runtime_activation_policies SET lifecycle='ACTIVE'
		            WHERE runtime_profile_id='llama_cpp_metal'`,
		"delete": `DELETE FROM runtime_activation_policies
		            WHERE runtime_profile_id='llama_cpp_metal'`,
	} {
		if _, err := pool.Exec(ctx, statement); err == nil {
			t.Errorf("a policy %s was accepted", name)
		}
	}
}

// A capability change must not inherit the activation decisions taken about the
// runtime it replaced.
func TestPolicyWrittenAgainstAnotherCapabilityIsRefusedAtWriteAndAtRead(t *testing.T) {
	ctx, store, _ := openActivationStore(t)

	// At write: the operator is still there to be told.
	_, err := store.ApplyActivationPolicy(ctx, []ActivationPolicyEntry{{
		RuntimeProfileID: "llama_cpp_metal",
		CellID:           llamaEmbedCell,
		CapabilityDigest: "00000000000000000000000000000000000000000000000000000000000000ff",
		Lifecycle:        runtimeLifecycleCanary,
		PromotionReceipt: "evidence/chain/two-agent-product-chain.json",
	}}, "promotion against a capability that is not current")
	if err == nil {
		t.Fatal("a promotion naming a stale capability digest was accepted")
	}

	// At read: a row that was valid when written and is not any more falls back
	// to the document rather than continuing to govern.
	stale, err := activationSnapshotFrom([]ActivationPolicyEntry{{
		PolicyRevision:   7,
		RuntimeProfileID: "llama_cpp_metal",
		ProfileRevision:  "r8",
		CellID:           llamaEmbedCell,
		CapabilityDigest: "00000000000000000000000000000000000000000000000000000000000000ff",
		Lifecycle:        runtimeLifecycleActive,
	}})
	must(t, err)
	if len(stale.Stale) != 1 {
		t.Fatalf("a stale statement produced %d warnings: %v", len(stale.Stale), stale.Stale)
	}
	for _, cap := range stale.advertised {
		if cap.ID == llamaEmbedCell {
			t.Fatal("a policy written against a different capability made a cell routable")
		}
	}

	// And a statement for a profile revision the document has moved past.
	superseded, err := activationSnapshotFrom([]ActivationPolicyEntry{{
		PolicyRevision:   7,
		RuntimeProfileID: "llama_cpp_metal",
		ProfileRevision:  "r1",
		CellID:           llamaEmbedCell,
		CapabilityDigest: "00000000000000000000000000000000000000000000000000000000000000ff",
		Lifecycle:        runtimeLifecycleActive,
	}})
	must(t, err)
	if len(superseded.Stale) != 1 {
		t.Fatalf("a superseded-revision statement produced %v", superseded.Stale)
	}
}

// Quarantining a cell must remove it from every route, not only from the
// advertised catalogue: directed routing sends real buyer work.
func TestQuarantinePolicyRemovesACellFromEveryRoute(t *testing.T) {
	ctx, store, _ := openActivationStore(t)

	if !cellIsDirected(llamaEmbedCell) {
		t.Fatal("the proven embed cell is not directed-reachable to begin with")
	}
	if _, err := store.ApplyActivationPolicy(ctx, []ActivationPolicyEntry{{
		RuntimeProfileID: "llama_cpp_metal",
		CellID:           llamaEmbedCell,
		Lifecycle:        runtimeLifecycleQuarantined,
	}}, "quarantine"); err != nil {
		t.Fatal(err)
	}
	if cellIsDirected(llamaEmbedCell) {
		t.Fatal("a QUARANTINED cell is still reachable by directed routing")
	}
	if cellIsAdvertised(llamaEmbedCell) {
		t.Fatal("a QUARANTINED cell is still advertised")
	}
	// And a worker of that engine can no longer bind the profile at all, because
	// nothing on it is reachable.
	_, err := ResolveWorkerRuntimeProfile(WorkerCapability{
		HWClass: "apple_silicon_max", Engine: "llama_cpp", MemoryGB: 64,
		SupportedJobs: []string{"embed"}, SupportedModels: []string{"all-minilm-l6-v2"},
	})
	if err == nil {
		t.Fatal("a worker enrolled against a profile with every cell quarantined")
	}
}

func TestReplicaRefreshAndCommitGuardRefuseStaleNewAdmission(t *testing.T) {
	ctx, writer, pool := openActivationStore(t)
	replicaPool, err := pgxpool.New(ctx, pool.Config().ConnString())
	mustf(t, err, "open second control replica pool: %v")
	t.Cleanup(replicaPool.Close)
	replica := NewStore(replicaPool)

	stale, err := replica.activationForNewAdmission(ctx)
	mustf(t, err, "prime replica activation cache: %v")
	if !cellIsDirected(llamaEmbedCell) {
		t.Fatal("fixture cell is not directed-reachable before containment")
	}
	containedRevision, err := writer.ApplyActivationPolicy(ctx, []ActivationPolicyEntry{{
		RuntimeProfileID: "llama_cpp_metal",
		CellID:           llamaEmbedCell,
		Lifecycle:        runtimeLifecycleQuarantined,
	}}, "cross-replica containment")
	mustf(t, err, "commit containment from writer replica: %v")
	if containedRevision <= stale.PolicyRevision {
		t.Fatalf("containment epoch=%d did not advance stale epoch=%d",
			containedRevision, stale.PolicyRevision)
	}

	// Emulate the independent process B: A's in-process adoption cannot update
	// B's atomic pointer, so put B's captured snapshot back before exercising its
	// persistence boundary. Classification succeeds against the stale directed
	// cell, but the locked transaction sees A's newer epoch and writes nothing.
	activeRuntimeActivation.Store(stale)
	workerID := uuid.New()
	capability := WorkerCapability{
		WorkerID: workerID, SupplierID: uuid.New(),
		HWClass: "apple_silicon_max", Engine: "llama_cpp", MemoryGB: 64,
		// Shape validation must pass so the admission epoch guard is the refusal.
		BuildHash: "0123456789abcdef", BuildIdentityPolicy: currentEngineBuildIdentityPolicy,
		HardwareIdentity: testOnlyHardwareIdentity,
		SupportedJobs:    []string{"embed"}, SupportedModels: []string{"all-minilm-l6-v2"},
		activationPolicyRevision: stale.PolicyRevision,
	}
	err = replica.UpsertWorker(ctx, capability)
	if !errors.Is(err, errActivationAdmissionStale) {
		t.Fatalf("stale replica persistence error=%v, want activation epoch refusal", err)
	}
	var workers int
	must(t, pool.QueryRow(ctx, `SELECT COUNT(*) FROM workers WHERE id=$1`, workerID).Scan(&workers))
	if workers != 0 {
		t.Fatal("stale replica wrote a worker before noticing containment")
	}

	// Request ingress refreshes B from the database before it reads the directed
	// projection. The now-quarantined worker is rejected and the process cache is
	// advanced to A's exact epoch.
	activeRuntimeActivation.Store(stale)
	wireCapability := capability
	wireCapability.activationPolicyRevision = 0
	body, err := json.Marshal(wireCapability)
	must(t, err)
	req := httptest.NewRequest(http.MethodPost, "/v1/worker/register", bytes.NewReader(body))
	req = req.WithContext(context.WithValue(req.Context(), ctxWorker, &WorkerAuth{
		WorkerID: workerID, SupplierID: capability.SupplierID,
	}))
	rec := httptest.NewRecorder()
	(&Server{store: replica}).handleWorkerRegister(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("refreshed replica registration status=%d body=%s, want authority refusal",
			rec.Code, rec.Body.String())
	}
	if currentActivation().PolicyRevision != containedRevision || cellIsDirected(llamaEmbedCell) {
		t.Fatalf("replica did not install containment epoch: revision=%d directed=%t",
			currentActivation().PolicyRevision, cellIsDirected(llamaEmbedCell))
	}
	must(t, pool.QueryRow(ctx, `SELECT COUNT(*) FROM workers WHERE id=$1`, workerID).Scan(&workers))
	if workers != 0 {
		t.Fatal("refreshed replica wrote the quarantined worker")
	}
}

func TestAdmissionEpochGuardReadsClockAfterWaitingForContainment(t *testing.T) {
	ctx, _, pool := openActivationStore(t)
	staleRevision := currentActivation().PolicyRevision

	// Start the admission transaction first and force PostgreSQL's transaction
	// timestamp to predate containment. This is the exact shape in which now()
	// was unsafe after an advisory-lock wait.
	admissionTx, err := pool.Begin(ctx)
	must(t, err)
	defer admissionTx.Rollback(ctx)
	var admissionStarted time.Time
	var admissionPID int
	must(t, admissionTx.QueryRow(ctx,
		`SELECT now(),pg_backend_pid()`).Scan(&admissionStarted, &admissionPID))

	writerTx, err := pool.Begin(ctx)
	must(t, err)
	defer writerTx.Rollback(ctx)
	must(t, lockActivationPolicy(ctx, writerTx))
	profile := mustRuntimeProfile(t, "llama_cpp_metal")
	digest, err := profile.CapabilityDigest(runtimeAuthorityModels)
	must(t, err)
	revision, err := insertActivationPolicyLocked(ctx, writerTx, []ActivationPolicyEntry{{
		RuntimeProfileID: "llama_cpp_metal", ProfileRevision: profile.Revision,
		CellID: llamaEmbedCell, CapabilityDigest: digest,
		Lifecycle: runtimeLifecycleQuarantined,
	}}, activationSourceOperator, nil, "containment while stale admission waits")
	mustf(t, err, "write containment under held epoch lock: %v")
	must(t, projectActivationPolicyIntoRegistry(ctx, writerTx))
	var containmentEffectiveAt time.Time
	must(t, writerTx.QueryRow(ctx, `
		SELECT effective_at FROM runtime_activation_policies
		 WHERE policy_revision=$1 LIMIT 1`, revision).Scan(&containmentEffectiveAt))
	if !containmentEffectiveAt.After(admissionStarted) {
		t.Fatalf("fixture did not put containment after stale tx clock: admission=%s containment=%s",
			admissionStarted, containmentEffectiveAt)
	}

	guardResult := make(chan error, 1)
	go func() {
		guardResult <- guardActivationAdmissionTx(ctx, admissionTx, staleRevision)
	}()
	deadline := time.Now().Add(3 * time.Second)
	for {
		var waiting bool
		must(t, pool.QueryRow(ctx, `
			SELECT EXISTS (
			  SELECT 1 FROM pg_locks
			   WHERE pid=$1 AND locktype='advisory' AND NOT granted
			)`, admissionPID).Scan(&waiting))
		if waiting {
			break
		}
		select {
		case early := <-guardResult:
			t.Fatalf("admission guard returned before containment released the lock: %v", early)
		default:
		}
		if time.Now().After(deadline) {
			t.Fatal("admission guard never blocked on containment epoch lock")
		}
		time.Sleep(10 * time.Millisecond)
	}

	must(t, writerTx.Commit(ctx))
	err = <-guardResult
	if !errors.Is(err, errActivationAdmissionStale) {
		t.Fatalf("post-wait guard error=%v, want stale-epoch refusal", err)
	}
}

func TestActivationEpochEffectiveTimesCannotHideSemanticChanges(t *testing.T) {
	ctx, store, pool := openActivationStore(t)
	profile := mustRuntimeProfile(t, "llama_cpp_metal")
	digest, err := profile.CapabilityDigest(runtimeAuthorityModels)
	must(t, err)
	var baseline int64
	must(t, pool.QueryRow(ctx,
		`SELECT MAX(policy_revision) FROM runtime_activation_policies`).Scan(&baseline))
	stale := currentActivation()

	// The governed writer cannot schedule a future epoch. Such an epoch would
	// force all later revisions to be equally late under the monotonic rule and
	// could block an emergency containment action.
	future := time.Now().UTC().Add(time.Hour).Truncate(time.Microsecond)
	_, err = store.ApplyActivationPolicy(ctx, []ActivationPolicyEntry{{
		RuntimeProfileID: "llama_cpp_metal", CellID: llamaEmbedCell,
		Lifecycle: runtimeLifecycleQuarantined, EffectiveAt: future,
	}}, "future policy must not block containment")
	if err == nil || !strings.Contains(err.Error(), "cannot be in the future") {
		t.Fatalf("governed writer accepted future activation: %v", err)
	}
	requestBody, err := json.Marshal(selectorActivationRequest{
		Entries: []ActivationPolicyEntry{{
			RuntimeProfileID: "llama_cpp_metal", CellID: llamaEmbedCell,
			Lifecycle: runtimeLifecycleQuarantined, EffectiveAt: future,
		}},
		Note: "operator API must refuse future activation",
	})
	must(t, err)
	req := httptest.NewRequest(http.MethodPost, "/admin/runtime/selector/activation",
		bytes.NewReader(requestBody))
	rec := httptest.NewRecorder()
	(&Server{store: store}).handleAdminSelectorActivation(rec, req)
	if rec.Code != http.StatusConflict ||
		!strings.Contains(rec.Body.String(), "cannot be in the future") {
		t.Fatalf("operator API future activation status=%d body=%s",
			rec.Code, rec.Body.String())
	}

	// The database boundary independently refuses direct SQL, while taking the
	// same epoch lock as governed writers.
	_, err = pool.Exec(ctx, `
		INSERT INTO runtime_activation_policies
		  (policy_revision,runtime_profile_id,profile_revision,cell_id,
		   capability_digest,lifecycle,routable,directed_eligible,
		   effective_at,source,note)
		VALUES ($1,'llama_cpp_metal',$2,$3,$4,'QUARANTINED',false,false,$5,'operator','future direct SQL')`,
		baseline+1, profile.Revision, llamaEmbedCell, digest, future)
	if err == nil || !strings.Contains(err.Error(), "cannot be in the future") {
		t.Fatalf("database accepted future activation epoch: %v", err)
	}
	var afterRefusals int64
	must(t, pool.QueryRow(ctx,
		`SELECT MAX(policy_revision) FROM runtime_activation_policies`).Scan(&afterRefusals))
	if afterRefusals != baseline {
		t.Fatalf("future refusals wrote revision %d after baseline %d", afterRefusals, baseline)
	}

	// Immediate emergency containment remains available after both refusals and
	// advances another replica when it next admits new authority.
	containedRevision, err := store.ApplyActivationPolicy(ctx, []ActivationPolicyEntry{{
		RuntimeProfileID: "llama_cpp_metal", CellID: llamaEmbedCell,
		Lifecycle: runtimeLifecycleQuarantined,
	}}, "immediate containment after rejected scheduling")
	mustf(t, err, "immediate containment was blocked by rejected future policy: %v")
	if containedRevision != baseline+1 {
		t.Fatalf("immediate containment revision=%d, want %d", containedRevision, baseline+1)
	}
	activeRuntimeActivation.Store(stale)
	refreshed, err := NewStore(pool).activationForNewAdmission(ctx)
	mustf(t, err, "refresh immediate containment: %v")
	if refreshed.PolicyRevision != containedRevision || cellIsDirected(llamaEmbedCell) {
		t.Fatalf("immediate containment did not reach stale replica: revision=%d directed=%t",
			refreshed.PolicyRevision, cellIsDirected(llamaEmbedCell))
	}

	var epochAt time.Time
	must(t, pool.QueryRow(ctx, `
		SELECT effective_at FROM runtime_activation_policies
		 WHERE policy_revision=$1 LIMIT 1`, containedRevision).Scan(&epochAt))
	// A committed revision is sealed as a complete row set: even an append that
	// copies the exact effective_at cannot change semantics without moving the
	// cache identity.
	_, err = pool.Exec(ctx, `
		INSERT INTO runtime_activation_policies
		  (policy_revision,runtime_profile_id,profile_revision,cell_id,
		   capability_digest,lifecycle,routable,directed_eligible,
		   effective_at,source,note)
		VALUES ($1,'llama_cpp_metal',$2,'',$3,'QUARANTINED',false,false,$4,'operator','split epoch')`,
		containedRevision, profile.Revision, digest, epochAt)
	if err == nil || !strings.Contains(err.Error(), "is sealed") {
		t.Fatalf("database appended to a committed activation revision: %v", err)
	}
	// A later revision also cannot backdate itself before the current global
	// epoch, which preserves revision as a monotonic freshness token.
	_, err = pool.Exec(ctx, `
		INSERT INTO runtime_activation_policies
		  (policy_revision,runtime_profile_id,profile_revision,cell_id,
		   capability_digest,lifecycle,routable,directed_eligible,
		   effective_at,source,note)
		VALUES ($1,'llama_cpp_metal',$2,'',$3,'QUARANTINED',false,false,$4,'operator','nonmonotonic epoch')`,
		containedRevision+1, profile.Revision, digest, epochAt.Add(-time.Microsecond))
	if err == nil || !strings.Contains(err.Error(), "precedes an earlier revision epoch") {
		t.Fatalf("database accepted non-monotonic activation epoch: %v", err)
	}

	// A gap cannot later be backfilled. Otherwise MAX(policy_revision) would
	// stay fixed while the newly inserted lower revision changed a disjoint
	// profile/cell's effective statement underneath equal-epoch replicas.
	_, err = pool.Exec(ctx, `
		INSERT INTO runtime_activation_policies
		  (policy_revision,runtime_profile_id,profile_revision,cell_id,
		   capability_digest,lifecycle,routable,directed_eligible,
		   effective_at,source,note)
		VALUES ($1,'llama_cpp_metal',$2,$3,$4,'QUARANTINED',false,false,
		        clock_timestamp(),'operator','advance across deliberate gap')`,
		containedRevision+2, profile.Revision, llamaEmbedCell, digest)
	mustf(t, err, "write higher activation epoch for backfill regression: %v")
	_, err = pool.Exec(ctx, `
		INSERT INTO runtime_activation_policies
		  (policy_revision,runtime_profile_id,profile_revision,cell_id,
		   capability_digest,lifecycle,routable,directed_eligible,
		   effective_at,source,note)
		VALUES ($1,'llama_cpp_metal',$2,'',$3,'QUARANTINED',false,false,
		        clock_timestamp(),'operator','late lower-revision backfill')`,
		containedRevision+1, profile.Revision, digest)
	if err == nil || !strings.Contains(err.Error(), "must advance beyond latest existing epoch") {
		t.Fatalf("database backfilled an activation revision below the high-water mark: %v", err)
	}

	// Re-running the schema must preserve already sealed revisions. In
	// particular, idempotent seed statements may not be mistaken for attempts
	// to append children to a committed epoch.
	mustf(t, NewStore(pool).Migrate(ctx), "repeat migration with sealed activation epochs: %v")
}

func TestMigrationRefusesLegacyFutureActivationEpoch(t *testing.T) {
	ctx, _, pool := openActivationStore(t)
	profile := mustRuntimeProfile(t, "llama_cpp_metal")
	digest, err := profile.CapabilityDigest(runtimeAuthorityModels)
	must(t, err)
	var baseline int64
	must(t, pool.QueryRow(ctx,
		`SELECT MAX(policy_revision) FROM runtime_activation_policies`).Scan(&baseline))

	// Emulate a row created by a pre-invariant binary. Current writes cannot get
	// past this trigger; startup must still audit historical storage rather than
	// assuming the trigger has always existed.
	_, err = pool.Exec(ctx,
		`DROP TRIGGER runtime_activation_policies_epoch_valid ON runtime_activation_policies`)
	must(t, err)
	_, err = pool.Exec(ctx,
		`DROP TRIGGER runtime_activation_policies_epoch_seal ON runtime_activation_policies`)
	must(t, err)
	_, err = pool.Exec(ctx, `
		INSERT INTO runtime_activation_policies
		  (policy_revision,runtime_profile_id,profile_revision,cell_id,
		   capability_digest,lifecycle,routable,directed_eligible,
		   effective_at,source,note)
		VALUES ($1,'llama_cpp_metal',$2,$3,$4,'QUARANTINED',false,false,$5,'operator','legacy future epoch')`,
		baseline+1, profile.Revision, llamaEmbedCell, digest,
		time.Now().UTC().Add(time.Hour))
	mustf(t, err, "plant legacy future activation row: %v")

	err = NewStore(pool).Migrate(ctx)
	if err == nil || !strings.Contains(err.Error(), "future, split, or non-monotonic") {
		t.Fatalf("migration accepted legacy future activation epoch: %v", err)
	}
}

func TestNewAdmissionReturns503WhenActivationDatabaseIsUnavailable(t *testing.T) {
	ctx, _, pool := openActivationStore(t)
	replicaPool, err := pgxpool.New(ctx, pool.Config().ConnString())
	mustf(t, err, "open failing replica pool: %v")
	replica := NewStore(replicaPool)
	replicaPool.Close()
	server := &Server{store: replica}

	var quotesBefore, jobsBefore, workersBefore int
	must(t, pool.QueryRow(ctx, `SELECT COUNT(*) FROM quotes`).Scan(&quotesBefore))
	must(t, pool.QueryRow(ctx, `SELECT COUNT(*) FROM jobs`).Scan(&jobsBefore))
	must(t, pool.QueryRow(ctx, `SELECT COUNT(*) FROM workers`).Scan(&workersBefore))

	buyer := &AuthResult{BuyerID: uuid.New()}
	quoteBody := `{"job_type":{"type":"embed"},"model":{"ref":"all-minilm-l6-v2"},"input":{"inline":"{\"text\":\"hello\"}\\n"}}`
	quoteReq := httptest.NewRequest(http.MethodPost, "/v1/quote", strings.NewReader(quoteBody))
	quoteReq = quoteReq.WithContext(context.WithValue(quoteReq.Context(), ctxBuyer, buyer))
	quoteRec := httptest.NewRecorder()
	server.handleQuote(quoteRec, quoteReq)
	if quoteRec.Code != http.StatusServiceUnavailable {
		t.Fatalf("quote activation DB failure status=%d body=%s", quoteRec.Code, quoteRec.Body.String())
	}
	// Runtime advertisement is authority, not request shape. Even a model the
	// stale cache does not advertise must reach the database freshness boundary
	// before that cache is consulted.
	staleAuthorityBody := `{"job_type":{"type":"embed"},"model":{"ref":"stale-cache-must-not-decide"},"input":{"inline":"{\"text\":\"hello\"}\\n"}}`
	staleAuthorityReq := httptest.NewRequest(http.MethodPost, "/v1/quote",
		strings.NewReader(staleAuthorityBody))
	staleAuthorityReq = staleAuthorityReq.WithContext(context.WithValue(
		staleAuthorityReq.Context(), ctxBuyer, buyer))
	staleAuthorityRec := httptest.NewRecorder()
	server.handleQuote(staleAuthorityRec, staleAuthorityReq)
	if staleAuthorityRec.Code != http.StatusServiceUnavailable {
		t.Fatalf("quote consulted stale runtime authority before DB refresh: status=%d body=%s",
			staleAuthorityRec.Code, staleAuthorityRec.Body.String())
	}

	workerCap := WorkerCapability{
		HWClass: "apple_silicon_max", Engine: "llama_cpp", MemoryGB: 64,
		SupportedJobs: []string{"embed"}, SupportedModels: []string{"all-minilm-l6-v2"},
	}
	workerBody, err := json.Marshal(workerCap)
	must(t, err)
	workerReq := httptest.NewRequest(http.MethodPost, "/v1/worker/register", bytes.NewReader(workerBody))
	workerReq = workerReq.WithContext(context.WithValue(workerReq.Context(), ctxWorker, &WorkerAuth{
		WorkerID: uuid.New(), SupplierID: uuid.New(),
	}))
	workerRec := httptest.NewRecorder()
	server.handleWorkerRegister(workerRec, workerReq)
	if workerRec.Code != http.StatusServiceUnavailable {
		t.Fatalf("worker activation DB failure status=%d body=%s", workerRec.Code, workerRec.Body.String())
	}

	_, submitErr := server.createJob(ctx, buyer.BuyerID, jobSubmit{
		JobType: JobType{Type: "embed"}, Model: ModelRef{Ref: "all-minilm-l6-v2"},
		FirmQuote: true, QuoteID: "q_" + uuid.NewString(),
	})
	if submitErr == nil || submitErr.status != http.StatusServiceUnavailable {
		t.Fatalf("firm-quote submit activation DB failure=%v, want 503", submitErr)
	}

	var quotesAfter, jobsAfter, workersAfter int
	must(t, pool.QueryRow(ctx, `SELECT COUNT(*) FROM quotes`).Scan(&quotesAfter))
	must(t, pool.QueryRow(ctx, `SELECT COUNT(*) FROM jobs`).Scan(&jobsAfter))
	must(t, pool.QueryRow(ctx, `SELECT COUNT(*) FROM workers`).Scan(&workersAfter))
	if quotesAfter != quotesBefore || jobsAfter != jobsBefore || workersAfter != workersBefore {
		t.Fatalf("activation DB failure wrote durable rows: quotes %d->%d jobs %d->%d workers %d->%d",
			quotesBefore, quotesAfter, jobsBefore, jobsAfter, workersBefore, workersAfter)
	}
}

// Nothing becomes routable without naming the receipt that authorised it. The
// rule is in the database, so a caller that forgets cannot make it optional.
// Operator promotions further require the gate's verdict, not a free string.
func TestRoutablePolicyRequiresAPromotionReceipt(t *testing.T) {
	ctx, store, _ := openActivationStore(t)

	_, err := store.ApplyActivationPolicy(ctx, []ActivationPolicyEntry{{
		RuntimeProfileID: "llama_cpp_metal",
		CellID:           llamaEmbedCell,
		Lifecycle:        runtimeLifecycleCanary,
	}}, "promotion with no evidence")
	if err == nil {
		t.Fatal("a cell was promoted to CANARY with no promotion receipt")
	}

	// A non-empty path that is not a gate verdict is also refused.
	_, err = store.ApplyActivationPolicy(ctx, []ActivationPolicyEntry{{
		RuntimeProfileID: "llama_cpp_metal",
		CellID:           llamaEmbedCell,
		Lifecycle:        runtimeLifecycleCanary,
		PromotionReceipt: "evidence/chain/two-agent-product-chain.json",
	}}, "promotion with a loose path")
	if err == nil {
		t.Fatal("a cell was promoted with a loose receipt path, not a gate verdict")
	}
	if !strings.Contains(err.Error(), promotionGateVersion) {
		t.Fatalf("refusal did not name the gate: %v", err)
	}
}

func TestOperatorCannotPromoteAProfileGlobally(t *testing.T) {
	ctx, store, pool := openActivationStore(t)
	var before int64
	must(t, pool.QueryRow(ctx,
		`SELECT MAX(policy_revision) FROM runtime_activation_policies`).Scan(&before))
	_, err := store.ApplyActivationPolicy(ctx, []ActivationPolicyEntry{{
		RuntimeProfileID: "llama_cpp_metal",
		Lifecycle:        runtimeLifecycleCanary,
		PromotionReceipt: "operator-note-is-not-global-authority",
	}}, "profile-global promotion has no representable evidence")
	if err == nil || !strings.Contains(err.Error(), "profile-global lifecycle") {
		t.Fatalf("profile-global promotion was not explicitly refused: %v", err)
	}
	var after int64
	must(t, pool.QueryRow(ctx,
		`SELECT MAX(policy_revision) FROM runtime_activation_policies`).Scan(&after))
	if after != before || cellIsAdvertised(llamaEmbedCell) {
		t.Fatalf("refused profile promotion changed revision/routing: before=%d after=%d advertised=%t",
			before, after, cellIsAdvertised(llamaEmbedCell))
	}
}

// A receipt row is not trusted merely because its passed column is true. Every
// durable projection, epoch and runtime identity has to agree with the strict
// evidence JSON before the independent coverage refusal is reached.
func TestPromotionReceiptRefusesStaleAndFabricatedAuthority(t *testing.T) {
	apply := func(
		t *testing.T, mutate func(*CellPromotionEvidence), want string,
	) {
		t.Helper()
		ctx, store, pool := openActivationStore(t)
		ref, _ := recordFabricatedNarrowPassedPromotionEvidence(t, ctx, store, mutate)
		_, err := store.ApplyActivationPolicy(ctx, []ActivationPolicyEntry{{
			RuntimeProfileID: "llama_cpp_metal", CellID: llamaEmbedCell,
			Lifecycle: runtimeLifecycleCanary, PromotionReceipt: ref,
		}}, "adversarial receipt")
		if err == nil || !strings.Contains(err.Error(), want) {
			t.Fatalf("receipt was not refused for %q: %v", want, err)
		}
		if cellIsAdvertised(llamaEmbedCell) {
			t.Fatal("a stale/fabricated receipt reached routing")
		}
		var operators int
		must(t, pool.QueryRow(ctx,
			`SELECT COUNT(*) FROM runtime_activation_policies WHERE source='operator'`).Scan(&operators))
		if operators != 0 {
			t.Fatalf("refused receipt wrote %d operator policy rows", operators)
		}
	}

	t.Run("stale runtime matrix", func(t *testing.T) {
		stale := strings.Repeat("a", 64)
		apply(t, func(e *CellPromotionEvidence) {
			e.RuntimeMatrixSHA256 = stale
			e.Scope.RuntimeMatrixSHA256 = stale
			e.ChallengerSupplierLiability.RuntimeMatrixSHA256 = stale
			e.IncumbentSupplierLiability.RuntimeMatrixSHA256 = stale
		}, "runtime matrix is stale")
	})
	t.Run("stale activation epoch", func(t *testing.T) {
		apply(t, func(e *CellPromotionEvidence) {
			e.PolicyRevision++
			e.Scope.PolicyRevision = e.PolicyRevision
			e.RollbackTargetRevision = e.PolicyRevision
		}, "policy epoch is stale")
	})
	t.Run("wrong runtime identity", func(t *testing.T) {
		apply(t, func(e *CellPromotionEvidence) {
			e.Scope.RuntimeID = "candle_metal"
			e.ChallengerSupplierLiability.RuntimeID = "candle_metal"
		}, "does not match policy runtime")
	})

	t.Run("cryptographic identity disagrees", func(t *testing.T) {
		ctx, store, pool := openActivationStore(t)
		evidence := fabricatedNarrowPassedPromotionEvidence(t)
		evidenceJSON, err := json.Marshal(evidence)
		must(t, err)
		scopeJSON, err := json.Marshal(evidence.Scope)
		must(t, err)
		forgedDigest := strings.Repeat("f", 64)
		forgedRef := promotionGateVersion + ":" + llamaEmbedCell + ":" + forgedDigest
		_, err = pool.Exec(ctx, `
			INSERT INTO runtime_cell_promotion_evaluations
			  (evidence_sha256,promotion_receipt_ref,gate_version,scope_json,
			   incumbent_cell,challenger_cell,passed,policy_revision,
			   runtime_matrix_sha256,evaluated_at,evidence_json)
			VALUES ($1,$2,$3,$4::jsonb,$5,$6,true,$7,$8,$9,$10::jsonb)`,
			forgedDigest, forgedRef, promotionGateVersion, scopeJSON,
			evidence.IncumbentCell, evidence.Scope.CellID, evidence.PolicyRevision,
			evidence.RuntimeMatrixSHA256, evidence.EvaluatedAt, evidenceJSON)
		mustf(t, err, "insert forged promotion receipt: %v")
		_, err = store.ApplyActivationPolicy(ctx, []ActivationPolicyEntry{{
			RuntimeProfileID: "llama_cpp_metal", CellID: llamaEmbedCell,
			Lifecycle: runtimeLifecycleCanary, PromotionReceipt: forgedRef,
		}}, "forged digest")
		if err == nil || !strings.Contains(err.Error(), "cryptographic identity disagrees") {
			t.Fatalf("forged digest was not refused cryptographically: %v", err)
		}
		if cellIsAdvertised(llamaEmbedCell) {
			t.Fatal("a forged digest reached routing")
		}
	})

	t.Run("pair projection disagrees", func(t *testing.T) {
		ctx, store, pool := openActivationStore(t)
		evidence := fabricatedNarrowPassedPromotionEvidence(t)
		digest, err := evidence.Digest()
		must(t, err)
		ref, err := evidence.ReceiptRef()
		must(t, err)
		evidenceJSON, err := json.Marshal(evidence)
		must(t, err)
		scopeJSON, err := json.Marshal(evidence.Scope)
		must(t, err)
		_, err = pool.Exec(ctx, `
			INSERT INTO runtime_cell_promotion_evaluations
			  (evidence_sha256,promotion_receipt_ref,gate_version,scope_json,
			   incumbent_cell,challenger_cell,passed,policy_revision,
			   runtime_matrix_sha256,evaluated_at,evidence_json)
			VALUES ($1,$2,$3,$4::jsonb,'fabricated-incumbent',$5,true,$6,$7,$8,$9::jsonb)`,
			digest, ref, promotionGateVersion, scopeJSON, evidence.Scope.CellID,
			evidence.PolicyRevision, evidence.RuntimeMatrixSHA256,
			evidence.EvaluatedAt, evidenceJSON)
		mustf(t, err, "insert pair-projection receipt: %v")
		_, err = store.ApplyActivationPolicy(ctx, []ActivationPolicyEntry{{
			RuntimeProfileID: "llama_cpp_metal", CellID: llamaEmbedCell,
			Lifecycle: runtimeLifecycleCanary, PromotionReceipt: ref,
		}}, "fabricated pair projection")
		if err == nil || !strings.Contains(err.Error(), "pair projection disagrees") {
			t.Fatalf("fabricated pair projection was not refused: %v", err)
		}
		if cellIsAdvertised(llamaEmbedCell) {
			t.Fatal("a fabricated pair projection reached routing")
		}
	})
}

func TestActivationPolicyRevisionAllocationIsSerialized(t *testing.T) {
	ctx, store, pool := openActivationStore(t)
	const writers = 8
	start := make(chan struct{})
	type result struct {
		revision int64
		err      error
	}
	results := make(chan result, writers)
	var wg sync.WaitGroup
	for i := 0; i < writers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			revision, err := store.ApplyActivationPolicy(ctx, []ActivationPolicyEntry{{
				RuntimeProfileID: "llama_cpp_metal", CellID: llamaEmbedCell,
				Lifecycle: runtimeLifecycleRealRuntimeProven,
			}}, "concurrent non-routable policy restatement")
			results <- result{revision: revision, err: err}
		}()
	}
	close(start)
	wg.Wait()
	close(results)
	seen := map[int64]bool{}
	for result := range results {
		if result.err != nil {
			t.Fatalf("concurrent policy write failed: %v", result.err)
		}
		if seen[result.revision] {
			t.Fatalf("two independent writes shared policy revision %d", result.revision)
		}
		seen[result.revision] = true
	}
	if len(seen) != writers {
		t.Fatalf("got %d unique revisions from %d writers", len(seen), writers)
	}
	var operatorRevisions int
	must(t, pool.QueryRow(ctx, `
		SELECT COUNT(DISTINCT policy_revision)
		  FROM runtime_activation_policies WHERE source='operator'`).Scan(&operatorRevisions))
	if operatorRevisions != writers {
		t.Fatalf("database holds %d operator epochs for %d independent writes",
			operatorRevisions, writers)
	}
}

// Rows accepted by the pre-v4 control plane are immutable historical evidence,
// but they are not current authority. Startup must retain them while projecting
// an effective quarantine into both process memory and the registry.
func TestStartupQuarantinesLegacyRoutableOperatorEpochs(t *testing.T) {
	tests := []struct {
		name      string
		cellID    string
		lifecycle string
		receipt   string
	}{
		{
			name: "gate v2 active cell", cellID: candleEmbedCell,
			lifecycle: runtimeLifecycleActive,
			receipt:   "cell-promotion-gate-v2:" + candleEmbedCell + ":" + strings.Repeat("1", 64),
		},
		{
			name: "gate v2 canary cell", cellID: candleEmbedCell,
			lifecycle: runtimeLifecycleCanary,
			receipt:   "cell-promotion-gate-v2:" + candleEmbedCell + ":" + strings.Repeat("2", 64),
		},
		{
			name:      "free form active profile",
			lifecycle: runtimeLifecycleActive,
			receipt:   "legacy-operator-profile-note",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			ctx, _, pool := openActivationStore(t)
			revision := insertLegacyRoutableActivation(
				t, ctx, pool, "candle_metal", tc.cellID, tc.lifecycle, tc.receipt)

			// A restart/redeploy runs migration projection before installing the
			// in-memory snapshot. Neither path may trust the legacy row.
			mustf(t, NewStore(pool).Migrate(ctx), "reload legacy activation epoch: %v")
			if currentActivation().PolicyRevision != revision {
				t.Fatalf("loaded policy revision=%d, want legacy epoch %d",
					currentActivation().PolicyRevision, revision)
			}
			if got := currentActivation().cellLifecycle(
				mustRuntimeProfile(t, "candle_metal"), mustRuntimeCell(t, "candle_metal", candleEmbedCell)); got != runtimeLifecycleQuarantined {
				t.Fatalf("legacy %s became effective lifecycle %s", tc.lifecycle, got)
			}
			if cellIsAdvertised(candleEmbedCell) || cellIsDirected(candleEmbedCell) {
				t.Fatal("legacy routable operator epoch remained reachable after startup")
			}
			if got := lifecycleOfCell(t, ctx, pool, "candle_metal", candleEmbedCell); got != runtimeLifecycleQuarantined {
				t.Fatalf("registry projected legacy lifecycle %s, want QUARANTINED", got)
			}
			var retained int
			must(t, pool.QueryRow(ctx, `
				SELECT COUNT(*) FROM runtime_activation_policies
				 WHERE policy_revision=$1 AND source='operator'`, revision).Scan(&retained))
			if retained != 1 {
				t.Fatalf("legacy evidence was not retained exactly once: %d rows", retained)
			}
		})
	}
}

func TestRollbackOfLegacyRoutableEpochWritesQuarantineForward(t *testing.T) {
	ctx, store, pool := openActivationStore(t)
	target := insertLegacyRoutableActivation(t, ctx, pool,
		"candle_metal", candleEmbedCell, runtimeLifecycleActive,
		"cell-promotion-gate-v2:"+candleEmbedCell+":"+strings.Repeat("3", 64))

	revision, err := store.RollbackActivationPolicy(ctx, target,
		"attempt to restore a legacy promotion must contain it")
	mustf(t, err, "rollback legacy routable epoch: %v")
	if revision <= target {
		t.Fatalf("rollback revision=%d did not write forward from %d", revision, target)
	}
	var lifecycle, source string
	var rollbackTarget int64
	must(t, pool.QueryRow(ctx, `
		SELECT lifecycle,source,rollback_target
		  FROM runtime_activation_policies
		 WHERE policy_revision=$1 AND runtime_profile_id='candle_metal' AND cell_id=$2`,
		revision, candleEmbedCell).Scan(&lifecycle, &source, &rollbackTarget))
	if lifecycle != runtimeLifecycleQuarantined || source != activationSourceRollback ||
		rollbackTarget != target {
		t.Fatalf("legacy rollback row lifecycle=%s source=%s target=%d",
			lifecycle, source, rollbackTarget)
	}
	if cellIsAdvertised(candleEmbedCell) || cellIsDirected(candleEmbedCell) {
		t.Fatal("rollback laundered a legacy operator receipt into reachability")
	}
	if got := lifecycleOfCell(t, ctx, pool, "candle_metal", candleEmbedCell); got != runtimeLifecycleQuarantined {
		t.Fatalf("legacy rollback registry lifecycle=%s, want QUARANTINED", got)
	}
}

func TestRollbackCanRestoreExactCurrentDocumentActiveAuthority(t *testing.T) {
	ctx, store, pool := openActivationStore(t)
	var documentRevision int64
	must(t, pool.QueryRow(ctx, `SELECT MIN(policy_revision) FROM runtime_activation_policies`).Scan(&documentRevision))
	_, err := store.ApplyActivationPolicy(ctx, []ActivationPolicyEntry{{
		RuntimeProfileID: "candle_metal", CellID: candleEmbedCell,
		Lifecycle: runtimeLifecycleQuarantined,
	}}, "temporary containment")
	must(t, err)
	if cellIsAdvertised(candleEmbedCell) {
		t.Fatal("containment fixture left document ACTIVE cell advertised")
	}

	_, err = store.RollbackActivationPolicy(ctx, documentRevision,
		"restore the exact current document statement")
	mustf(t, err, "restore document activation: %v")
	if !cellIsAdvertised(candleEmbedCell) {
		t.Fatal("rollback quarantined exact current document ACTIVE authority")
	}
	if got := lifecycleOfCell(t, ctx, pool, "candle_metal", candleEmbedCell); got != runtimeLifecycleActive {
		t.Fatalf("document rollback registry lifecycle=%s, want ACTIVE", got)
	}
}

func TestCommittedContainmentSurvivesPostCommitRefreshFailure(t *testing.T) {
	ctx, store, pool := openActivationStore(t)
	if !cellIsAdvertised(candleEmbedCell) {
		t.Fatal("document ACTIVE fixture cell is not initially advertised")
	}
	priorRevision := currentActivation().PolicyRevision
	previousRefresh := activationPolicyBestEffortRefresh
	refreshCalls := 0
	activationPolicyBestEffortRefresh = func(
		context.Context, *pgxpool.Pool,
	) (*runtimeActivation, error) {
		refreshCalls++
		return nil, errors.New("injected post-commit read failure")
	}
	t.Cleanup(func() { activationPolicyBestEffortRefresh = previousRefresh })

	revision, err := store.ApplyActivationPolicy(ctx, []ActivationPolicyEntry{{
		RuntimeProfileID: "candle_metal", CellID: candleEmbedCell,
		Lifecycle: runtimeLifecycleQuarantined,
	}}, "containment must close routing before any refresh")
	mustf(t, err, "committed containment returned refresh failure: %v")
	if revision <= priorRevision {
		t.Fatalf("committed revision=%d did not advance prior ACTIVE revision=%d",
			revision, priorRevision)
	}
	if refreshCalls != 1 {
		t.Fatalf("best-effort post-commit read called %d times, want 1", refreshCalls)
	}
	if currentActivation().PolicyRevision != revision {
		t.Fatalf("cache revision=%d, committed=%d",
			currentActivation().PolicyRevision, revision)
	}
	if cellIsAdvertised(candleEmbedCell) || cellIsDirected(candleEmbedCell) {
		t.Fatal("post-commit read failure retained stale ACTIVE reachability")
	}
	if got := lifecycleOfCell(t, ctx, pool, "candle_metal", candleEmbedCell); got != runtimeLifecycleQuarantined {
		t.Fatalf("committed registry lifecycle=%s, want QUARANTINED", got)
	}
}

func insertLegacyRoutableActivation(
	t *testing.T, ctx context.Context, pool *pgxpool.Pool,
	runtimeID, cellID, lifecycle, receipt string,
) int64 {
	t.Helper()
	profile := mustRuntimeProfile(t, runtimeID)
	digest, err := profile.CapabilityDigest(runtimeAuthorityModels)
	must(t, err)
	var revision int64
	must(t, pool.QueryRow(ctx,
		`SELECT COALESCE(MAX(policy_revision),0)+1 FROM runtime_activation_policies`).Scan(&revision))
	_, err = pool.Exec(ctx, `
		INSERT INTO runtime_activation_policies
		  (policy_revision,runtime_profile_id,profile_revision,cell_id,
		   capability_digest,lifecycle,routable,directed_eligible,
		   promotion_receipt,source,note)
		VALUES ($1,$2,$3,$4,$5,$6,true,true,$7,'operator','legacy activation fixture')`,
		revision, runtimeID, profile.Revision, cellID, digest, lifecycle, receipt)
	mustf(t, err, "insert legacy %s activation: %v", lifecycle)
	return revision
}

func mustRuntimeProfile(t *testing.T, runtimeID string) authorityRuntimeProfile {
	t.Helper()
	for _, profile := range runtimeAuthority.Runtimes {
		if profile.RuntimeID == runtimeID {
			return profile
		}
	}
	t.Fatalf("runtime profile %q is not registered", runtimeID)
	return authorityRuntimeProfile{}
}

func mustRuntimeCell(t *testing.T, runtimeID, cellID string) authorityCell {
	t.Helper()
	profile := mustRuntimeProfile(t, runtimeID)
	for _, cell := range profile.Cells {
		if cell.ID == cellID {
			return cell
		}
	}
	t.Fatalf("runtime cell %q/%q is not registered", runtimeID, cellID)
	return authorityCell{}
}

// The digest changed DEFINITION, so anything that recorded the old value must
// still be able to resolve the revision it named.
func TestPriorCapabilityDigestsAreRetained(t *testing.T) {
	ctx, _, pool := openActivationStore(t)

	// Simulate a database written under manifest v1: an older digest under the
	// same revision, at the older version.
	const legacy = "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"
	if _, err := pool.Exec(ctx, `
		UPDATE runtime_profiles SET profile_digest=$1, capability_manifest_version=1
		 WHERE runtime_profile_id='llama_cpp_metal' AND is_current`, legacy); err != nil {
		t.Fatal(err)
	}
	mustf(t, NewStore(pool).Migrate(ctx), "re-migration over a v1 registry: %v")

	var current string
	var version int
	if err := pool.QueryRow(ctx, `
		SELECT profile_digest, capability_manifest_version FROM runtime_profiles
		 WHERE runtime_profile_id='llama_cpp_metal' AND is_current`).
		Scan(&current, &version); err != nil {
		t.Fatal(err)
	}
	if version != capabilityManifestVersion {
		t.Fatalf("the row is still at manifest version %d", version)
	}
	if current == legacy {
		t.Fatal("the v1 digest was not upgraded")
	}
	var retained bool
	if err := pool.QueryRow(ctx, `
		SELECT EXISTS (SELECT 1 FROM runtime_profile_digest_history
		                WHERE runtime_profile_id='llama_cpp_metal' AND profile_digest=$1)`,
		legacy).Scan(&retained); err != nil {
		t.Fatal(err)
	}
	if !retained {
		t.Fatal("the prior digest was overwritten without being retained; " +
			"anything that recorded it can no longer resolve the revision it named")
	}
}

func assertRegistryMatchesPolicy(t *testing.T, ctx context.Context, pool *pgxpool.Pool) {
	t.Helper()
	rows, err := pool.Query(ctx, `
		SELECT m.runtime_profile_id, m.cell_id, m.lifecycle, m.routable
		  FROM runtime_profile_models m
		  JOIN runtime_profiles p
		    ON p.runtime_profile_id = m.runtime_profile_id AND p.revision = m.revision
		 WHERE p.is_current`)
	must(t, err)
	defer rows.Close()
	snapshot := currentActivation()
	seen := 0
	for rows.Next() {
		var runtimeID, cellID, lifecycle string
		var routable bool
		must(t, rows.Scan(&runtimeID, &cellID, &lifecycle, &routable))
		seen++
		profile, ok := runtimeProfileByID(runtimeID)
		if !ok {
			t.Fatalf("registry holds unregistered profile %q", runtimeID)
		}
		for _, cell := range profile.Cells {
			if cell.ID != cellID {
				continue
			}
			if want := snapshot.cellLifecycle(profile, cell); want != lifecycle {
				t.Errorf("%s/%s: registry says %s, policy says %s",
					runtimeID, cellID, lifecycle, want)
			}
			// Registry routable tracks lifecycle AND bindable authority.
			wantRoutable := snapshot.cellRoutable(profile, cell)
			if routable != wantRoutable {
				t.Errorf("%s/%s: routable=%v, want %v at lifecycle %s",
					runtimeID, cellID, routable, wantRoutable, lifecycle)
			}
		}
	}
	must(t, rows.Err())
	if seen == 0 {
		t.Fatal("the registry holds no current cells")
	}
}

func lifecycleOfCell(t *testing.T, ctx context.Context, pool *pgxpool.Pool, runtimeID, cellID string) string {
	t.Helper()
	var lifecycle string
	if err := pool.QueryRow(ctx, `
		SELECT m.lifecycle FROM runtime_profile_models m
		  JOIN runtime_profiles p
		    ON p.runtime_profile_id = m.runtime_profile_id AND p.revision = m.revision
		 WHERE p.is_current AND m.runtime_profile_id=$1 AND m.cell_id=$2`,
		runtimeID, cellID).Scan(&lifecycle); err != nil {
		t.Fatalf("read lifecycle of %s/%s: %v", runtimeID, cellID, err)
	}
	return lifecycle
}

func cellIsAdvertised(cellID string) bool {
	for _, cap := range advertisedRuntimeCapabilities() {
		if cap.ID == cellID {
			return true
		}
	}
	return false
}

func cellIsDirected(cellID string) bool {
	for _, cap := range directedRuntimeCapabilities() {
		if cap.ID == cellID {
			return true
		}
	}
	return false
}

// A rejection is a measurement, and policy does not reverse measurements.
//
// This is the hole the capability/activation split opens if nothing closes it:
// the determinism rule that refuses a byte_exact cell on a non-deterministic
// engine runs when the DOCUMENT loads, and activation policy is a second door
// into the same decision. Without this guard an operator could promote
// llama.cpp's byte_exact generation cell straight to CANARY by writing a row —
// the cell whose sweep found divergence from its own serial output in every
// batched configuration tested.
func TestPolicyCannotReverseAMeasuredRejection(t *testing.T) {
	ctx, store, _ := openActivationStore(t)

	for _, lifecycle := range []string{
		runtimeLifecycleValidated,
		runtimeLifecycleRealRuntimeProven,
		runtimeLifecycleCanary,
		runtimeLifecycleActive,
	} {
		_, err := store.ApplyActivationPolicy(ctx, []ActivationPolicyEntry{{
			RuntimeProfileID: "llama_cpp_metal",
			CellID:           "llama-cpp-metal-llama1-infer",
			Lifecycle:        lifecycle,
			PromotionReceipt: "evidence/perf/runtime-benchmarks/llama-cpp-metal-llama1-q4-r3.json",
		}}, "reversing a measured rejection")
		if err == nil {
			t.Fatalf("policy promoted a REJECTED_FOR_CONTRACT cell to %s", lifecycle)
		}
		if !strings.Contains(err.Error(), "REJECTED_FOR_CONTRACT") {
			t.Errorf("refused for the wrong reason at %s: %v", lifecycle, err)
		}
	}

	// Restating the rejection is fine — that is not a reversal.
	if _, err := store.ApplyActivationPolicy(ctx, []ActivationPolicyEntry{{
		RuntimeProfileID: "llama_cpp_metal",
		CellID:           "llama-cpp-metal-llama1-infer",
		Lifecycle:        runtimeLifecycleRejectedForContract,
	}}, "restating the rejection"); err != nil {
		t.Fatalf("policy could not restate an existing rejection: %v", err)
	}
}
