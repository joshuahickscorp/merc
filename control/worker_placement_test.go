package main

import (
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
)

// Failing-before evidence (Step 9, locality freshness tripwire):
//
//	With workerPlacementRequireLocalityFreshness=false, a locality-driven
//	placement that omits freshness validates. With the tripwire on (production),
//	the same body is refused. This is the permanent replacement for a one-shot
//	probe against pre-repair HEAD.

func TestWorkerPlacementLocalityDrivenRequiresFreshnessFailingBefore(t *testing.T) {
	workerID, supplierID := uuid.New(), uuid.New()
	taskID, jobID := uuid.New(), uuid.New()
	// Locality drove selection but freshness of worker_prefix_state is unknown.
	in := BatchClaimPlacementInputs{
		WorkerID: workerID, SupplierID: supplierID,
		TaskID: taskID, JobID: jobID,
		ClaimedAt:       time.Date(2026, 8, 9, 12, 0, 0, 0, time.UTC),
		RuntimeCellID:   "cell-a",
		HWClass:         "nvidia_24gb",
		WarmPrefixDepth: 256,
		// PrefixLastSeen deliberately zero → FreshnessKnown=false
		LocalityDroveSelection: true,
	}

	// Failing-before: neutralise the tripwire and show the body would pass.
	prev := workerPlacementRequireLocalityFreshness
	workerPlacementRequireLocalityFreshness = false
	t.Cleanup(func() { workerPlacementRequireLocalityFreshness = prev })

	// Build without validation by constructing the body the builder would make
	// before ValidateWorkerPlacement (builder itself validates, so build via
	// open construction for the failing-before probe).
	p := WorkerPlacement{
		Version:        workerPlacementVersion,
		Status:         workerPlacementBound,
		Lane:           workerPlacementLaneBatch,
		WorkerID:       workerID,
		SupplierID:     supplierID,
		SelectionShape: workerSelectionBatchClaimPull,
		ClaimEligibility: &BatchClaimEligibility{
			ClaimingWorkerID: workerID, ClaimingSupplierID: supplierID,
			ClaimedTaskID: taskID, ClaimedJobID: jobID,
			ClaimedAt:        in.ClaimedAt.Format(time.RFC3339Nano),
			RuntimeCellID:    in.RuntimeCellID,
			HWClass:          in.HWClass,
			AvailabilityMode: marketAvailabilitySkipLocked,
			CandidateEpoch:   batchCandidateEpochNone,
			Note:             "claim-time eligibility snapshot",
		},
		Locality: &LocalityBelief{
			Source:          localitySourcePrefixState,
			WarmPrefixDepth: 256,
			FreshnessKnown:  false, // the defect
		},
		LocalityDroveSelection: true,
		Fallback:               WorkerPlacementFallback{Shape: workerFallbackBatchRequeue},
		Reason:                 "locality-driven claim without freshness",
	}
	if err := ValidateWorkerPlacement(p); err != nil {
		t.Fatalf("FAILING_BEFORE probe: neutralised tripwire must accept locality without freshness: %v", err)
	}

	// Production tripwire on: same body must fail.
	workerPlacementRequireLocalityFreshness = true
	if err := ValidateWorkerPlacement(p); err == nil {
		t.Fatal("locality-driven placement without freshness must be refused")
	} else if !strings.Contains(err.Error(), "freshness") {
		t.Fatalf("refusal must name freshness, got: %v", err)
	}

	// Builder refuses the same defect.
	if _, err := newBatchClaimWorkerPlacement(in); err == nil {
		t.Fatal("newBatchClaimWorkerPlacement must refuse locality-driven claim without last_seen")
	}

	// With freshness recorded, both pass.
	in.PrefixLastSeen = in.ClaimedAt.Add(-10 * time.Second)
	got, err := newBatchClaimWorkerPlacement(in)
	must(t, err)
	if got.Locality == nil || !got.Locality.FreshnessKnown || got.Locality.FreshUntil == "" {
		t.Fatalf("locality-driven placement must record freshness: %+v", got.Locality)
	}
	if got.Locality.FreshnessTTLSecs != workerPrefixStateTTLSecs {
		t.Fatalf("prefix TTL = %d, want %d", got.Locality.FreshnessTTLSecs, workerPrefixStateTTLSecs)
	}
}

func TestWorkerPlacementBatchAcceptPendingDoesNotBindWorker(t *testing.T) {
	p, err := newBatchAcceptPendingWorkerPlacement()
	must(t, err)
	if p.Status != workerPlacementPendingClaim || p.Lane != workerPlacementLaneBatch {
		t.Fatalf("pending accept: %+v", p)
	}
	if p.WorkerID != uuid.Nil || p.SupplierID != uuid.Nil {
		t.Fatalf("pending accept must not bind a worker: %+v", p)
	}
	if p.SelectionShape != workerSelectionNonePendingPull {
		t.Fatalf("selection shape: %q", p.SelectionShape)
	}
	if p.ClaimEligibility != nil {
		t.Fatal("pending accept must not invent claim eligibility")
	}
	// Reason must refuse frozen-epoch fiction.
	if !strings.Contains(p.Reason, "SKIP LOCKED") || !strings.Contains(strings.ToLower(p.Reason), "claim") {
		t.Fatalf("reason must explain pull-at-claim: %q", p.Reason)
	}
	digest, err := workerPlacementDigest(p)
	must(t, err)
	if !validSHA256(digest) {
		t.Fatalf("digest: %q", digest)
	}
}

func TestWorkerPlacementBatchClaimBindsWorkerWithoutFrozenEpoch(t *testing.T) {
	workerID, supplierID := uuid.New(), uuid.New()
	taskID, jobID := uuid.New(), uuid.New()
	claimedAt := time.Date(2026, 8, 9, 12, 0, 0, 0, time.UTC)
	p, err := newBatchClaimWorkerPlacement(BatchClaimPlacementInputs{
		WorkerID: workerID, SupplierID: supplierID,
		TaskID: taskID, JobID: jobID,
		ClaimedAt: claimedAt, RuntimeCellID: "cell-1", HWClass: "nvidia_24gb",
	})
	must(t, err)
	if p.Status != workerPlacementBound || p.WorkerID != workerID || p.SupplierID != supplierID {
		t.Fatalf("bound claim: %+v", p)
	}
	if p.ClaimEligibility == nil {
		t.Fatal("batch BOUND requires claim_eligibility")
	}
	if p.ClaimEligibility.CandidateEpoch != batchCandidateEpochNone {
		t.Fatalf("batch must record NONE_PULL_MARKET, got %q", p.ClaimEligibility.CandidateEpoch)
	}
	if p.ClaimEligibility.AvailabilityMode != marketAvailabilitySkipLocked {
		t.Fatalf("availability: %q", p.ClaimEligibility.AvailabilityMode)
	}
	if p.CandidateAuthority != "" || p.CandidateCount != 0 {
		t.Fatalf("batch must not claim a frozen book: %+v", p)
	}
	// Market-plane projection stays coherent and also refuses a fake epoch.
	md, err := projectPullMarketDecision(p)
	must(t, err)
	if md.MarketShape != marketShapePullEligibilitySnapshot ||
		md.PullEligibilitySnapshot == nil ||
		md.PullEligibilitySnapshot.ClaimingWorkerID != workerID {
		t.Fatalf("pull market projection: %+v", md)
	}
	// Inventing an epoch on the eligibility body is refused.
	bad := p
	bad.ClaimEligibility = &BatchClaimEligibility{}
	*bad.ClaimEligibility = *p.ClaimEligibility
	bad.ClaimEligibility.CandidateEpoch = "epoch-1"
	if err := ValidateWorkerPlacement(bad); err == nil {
		t.Fatal("must refuse invented batch candidate epoch")
	}
}

func TestWorkerPlacementAcceptedBindsWorkerOrRecordsWhyNot(t *testing.T) {
	// Batch accept: cannot bind.
	pending, err := newBatchAcceptPendingWorkerPlacement()
	must(t, err)
	if pending.Status != workerPlacementPendingClaim || pending.WorkerID != uuid.Nil {
		t.Fatalf("accept path must record why it cannot bind: %+v", pending)
	}

	// Realtime: binds the MarketDecision winner.
	md := fixturePushMarketDecision(t, 1, marketAvailabilityBlockingForUpdate)
	digest, err := marketDecisionDigest(md)
	must(t, err)
	lastSeen := time.Date(2026, 8, 9, 12, 0, 0, 0, time.UTC)
	rt, err := newRealtimeWorkerPlacement(md, digest, "HOT", lastSeen, true)
	must(t, err)
	if rt.Status != workerPlacementBound ||
		rt.WorkerID != md.PushOrderBook.SelectedWorkerID ||
		rt.CandidateAuthority != "MarketDecision" ||
		rt.CandidateAuthorityDigest != digest {
		t.Fatalf("realtime bind: %+v", rt)
	}
	if rt.Locality == nil || !rt.Locality.FreshnessKnown || rt.Locality.OfferWarmth != "HOT" {
		t.Fatalf("realtime locality: %+v", rt.Locality)
	}

	// Service lease: binds offer-book winner.
	w, s := uuid.New(), uuid.New()
	lease, err := newServiceLeaseWorkerPlacement(w, s, 3, 1, "")
	must(t, err)
	if lease.Status != workerPlacementBound || lease.WorkerID != w ||
		lease.Fallback.Shape != workerFallbackLeaseKeepLeaseNewWorker {
		t.Fatalf("lease bind: %+v", lease)
	}
}

func TestWorkerPlacementFallbackIsLaneDiscriminated(t *testing.T) {
	workerID, supplierID := uuid.New(), uuid.New()
	taskID, jobID := uuid.New(), uuid.New()
	base, err := newBatchClaimWorkerPlacement(BatchClaimPlacementInputs{
		WorkerID: workerID, SupplierID: supplierID,
		TaskID: taskID, JobID: jobID,
		ClaimedAt: time.Now().UTC(),
	})
	must(t, err)

	// Batch cannot record realtime immutable-worker fallback.
	cross := base
	cross.Fallback = WorkerPlacementFallback{Shape: workerFallbackRealtimeImmutableWorker}
	if err := ValidateWorkerPlacement(cross); err == nil {
		t.Fatal("batch must refuse realtime fallback shape")
	}

	// Batch cannot record lease keep-lease-new-worker.
	cross.Fallback = WorkerPlacementFallback{Shape: workerFallbackLeaseKeepLeaseNewWorker}
	if err := ValidateWorkerPlacement(cross); err == nil {
		t.Fatal("batch must refuse lease fallback shape")
	}

	// Realtime cannot record batch requeue.
	md := fixturePushMarketDecision(t, 1, marketAvailabilityBlockingForUpdate)
	digest, err := marketDecisionDigest(md)
	must(t, err)
	rt, err := newRealtimeWorkerPlacement(md, digest, "COLD", time.Time{}, false)
	must(t, err)
	rt.Fallback = WorkerPlacementFallback{Shape: workerFallbackBatchRequeue}
	if err := ValidateWorkerPlacement(rt); err == nil {
		t.Fatal("realtime must refuse batch fallback shape")
	}

	// Lease cannot record realtime immutable worker.
	lease, err := newServiceLeaseWorkerPlacement(workerID, supplierID, 2, 1, "")
	must(t, err)
	lease.Fallback = WorkerPlacementFallback{Shape: workerFallbackRealtimeImmutableWorker}
	if err := ValidateWorkerPlacement(lease); err == nil {
		t.Fatal("lease must refuse realtime fallback shape")
	}

	// Hedge is legal on batch.
	hedge, err := newBatchClaimWorkerPlacement(BatchClaimPlacementInputs{
		WorkerID: workerID, SupplierID: supplierID,
		TaskID: taskID, JobID: jobID,
		ClaimedAt: time.Now().UTC(), Hedge: true,
	})
	must(t, err)
	if hedge.Fallback.Shape != workerFallbackBatchHedge {
		t.Fatalf("hedge fallback: %q", hedge.Fallback.Shape)
	}
}

func TestWorkerPlacementRealtimeRefusesPullBodyCrossWiring(t *testing.T) {
	// A batch claim placement must not be passable as a realtime citation.
	p, err := newBatchClaimWorkerPlacement(BatchClaimPlacementInputs{
		WorkerID: uuid.New(), SupplierID: uuid.New(),
		TaskID: uuid.New(), JobID: uuid.New(),
		ClaimedAt: time.Now().UTC(),
	})
	must(t, err)
	// Force realtime lane while keeping batch claim body.
	p.Lane = workerPlacementLaneRealtime
	p.SelectionShape = workerSelectionRealtimePushBook
	p.Fallback = WorkerPlacementFallback{Shape: workerFallbackRealtimeImmutableWorker}
	if err := ValidateWorkerPlacement(p); err == nil {
		t.Fatal("must refuse realtime lane carrying batch claim_eligibility")
	}
}

func TestWorkerPlacementFourMeaningsRemainDistinct(t *testing.T) {
	// Guard: Step 9 binds the fourth meaning without renaming the first three.
	// PlacementDecision remains supply mode.
	mode, err := ChooseExecutionMode(PlacementRequest{
		WorkloadClass: "batch_embedding", Coupling: CouplingIndependent, Degree: 1,
		Fabric: FabricWAN, CommunityCapacityAvailable: true,
	})
	must(t, err)
	if mode.Mode != ModePool {
		t.Fatalf("PlacementDecision still means mode, got %+v", mode)
	}
	// PlacementRequirement remains eligibility (versioned struct).
	if placementRequirementVersion < 1 {
		t.Fatal("PlacementRequirement version missing")
	}
	// RealtimePlacementPlan remains host topology type name.
	var plan RealtimePlacementPlan
	if plan.Version != 0 {
		t.Fatal("unexpected default plan version")
	}
	// WorkerPlacement is the worker-choice type.
	pending, err := newBatchAcceptPendingWorkerPlacement()
	must(t, err)
	if pending.Status != workerPlacementPendingClaim {
		t.Fatalf("worker placement pending: %+v", pending)
	}
}
