package main

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
)

func currentIdentityWorkerCapability(
	workerID, supplierID uuid.UUID, placement PlacementRequirement, job *jobRow,
) WorkerCapability {
	rate := float32(1)
	unit, scope := "tokens", performanceUnitScopeTokenLikeInputPlusOutputTokens
	if placement.PerformanceAuthority != nil {
		rate = float32(placement.PerformanceAuthority.Performance.ConservativeUnitsPerSec)
		unit = placement.PerformanceAuthority.Performance.Unit
		scope = placement.PerformanceAuthority.Performance.UnitScope
	}
	benchmark := BenchResult{
		ModelID: job.ModelRef, JobType: job.JobType, TPS: rate, ThermalOK: true,
		Unit: unit, UnitScope: scope, MeasuredUnix: uint64(runtimeCellPerformanceNow().Unix()),
	}
	if job.JobType == "embed" {
		benchmark.EPS, benchmark.TPS = rate, 0
	}
	return WorkerCapability{
		WorkerID: workerID, SupplierID: supplierID,
		HWClass: placement.HWClasses[0], Engine: placement.Engine,
		BuildHash: placement.EngineBuildHash, BuildIdentityPolicy: placement.EngineBuildIdentityPolicy,
		HardwareIdentity: placement.HardwareIdentity,
		MemoryGB:         128, MemoryBwGbps: 800, MinPayoutUsdHr: 0,
		SupportedJobs: []string{job.JobType}, SupportedModels: []string{job.ModelRef},
		Benchmarks:   []BenchResult{benchmark},
		AgentVersion: "TEST_ONLY identity lifecycle", OSVersion: "TEST_ONLY macOS",
	}
}

// mutateWorkerToDifferentDirectedIdentity uses the real registration path to
// prove that a worker row can legitimately change after a claim. The rendering
// cell is directed-only and therefore does not borrow current advertised
// benchmark authority; it gives the mutation a valid governed destination
// without weakening ordinary batch admission.
func differentDirectedIdentity(exact WorkerCapability) WorkerCapability {
	mutated := exact
	mutated.BuildHash = "0000000000000000"
	if mutated.BuildHash == exact.BuildHash {
		mutated.BuildHash = "1111111111111111"
	}
	mutated.HardwareIdentity = "TEST_ONLY Apple M1 Ultra"
	mutated.SupportedJobs = []string{"media_rendering"}
	mutated.SupportedModels = []string{"svg-scene-render-v1"}
	mutated.Benchmarks = nil
	return mutated
}

func mutateWorkerToDifferentDirectedIdentity(
	t *testing.T, ctx context.Context, store *Store, exact WorkerCapability,
) WorkerCapability {
	t.Helper()
	mutated := differentDirectedIdentity(exact)
	mustf(t, store.UpsertWorker(ctx, mutated), "mutate claimed worker through registration: %v")
	return mutated
}

func seedCurrentIdentityLifecycleJob(t *testing.T) (
	context.Context, *Store, *pgxpool.Pool, moneyPathFixture, *jobRow, []taskRow,
) {
	t.Helper()
	ctx, store, pool := openMoneyPathStore(t)
	// Current ingress deliberately re-opens the sole governed catalogue
	// derivation. Install synthetic receipt authority only after store startup,
	// then publish its complete schedule through the production constructor;
	// hand-built one-result schedules are historical fixtures, not current
	// authority.
	installBoundCataloguePublicationAuthorityForTest(t)
	installTestOnlyCombinedTokenAuthority(t)
	// Store startup persisted the deliberately empty production projection. The
	// scoped synthetic receipts establish a new test epoch, so do not inherit
	// production stale-cell lifecycle markers into that projection.
	installed := currentActivation()
	activeRuntimeActivation.Store(newRuntimeActivation(
		installed.PolicyRevision, map[string]string{}, nil))
	pinBoardClockForPublication(t)
	schedule, err := BuildCataloguePriceSchedule()
	mustf(t, err, "build current TEST_ONLY catalogue schedule: %v")
	if _, err := store.ApplyRepricing(ctx, schedule); err != nil {
		t.Fatalf("apply current TEST_ONLY catalogue schedule: %v", err)
	}
	fixture := seedMoneyPathFixture(t, ctx, store, pool, moneyPathSeedOpts{TaskCount: 2})
	tasks := makeTasks(fixture, 2)
	fixture.TaskIDs = []uuid.UUID{tasks[0].ID, tasks[1].ID}
	// validJobRow's generic one-cent authority is useful to pure and historical
	// tests. Build the geometry without seeding that hand-built schedule, then
	// replace its catalogue-dependent blocks with the exact applied authority.
	unseeded := fixture
	unseeded.ctx, unseeded.pool = nil, nil
	job := validJobRow(t, unseeded, tasks)
	authority, err := store.LoadCataloguePriceAuthority(ctx, job.ModelRef)
	mustf(t, err, "load current TEST_ONLY catalogue authority: %v")
	economicInput := job.EconomicPlan.Input
	economicInput.SupplierShare = authority.SupplierShare
	economic := BuildEconomicPlan(economicInput, job.EconomicPlan.Schedule)
	if !economic.Executable {
		t.Fatalf("rebuild current lifecycle economics: %s", economic.BlockReason)
	}
	mustf(t, ValidateComputePlanEconomicSnapshot(
		job.ComputePlan, job.WorkloadDecision, economic,
	), "rebuild current lifecycle compute/economic authority: %v")
	placement := placementForPricingFixture(t, job.WorkloadDecision, authority)
	pricing, err := newDistributedPricingDecision(
		job.WorkloadDecision, job.ComputePlan, placement, economic,
		authority, job.WorkloadDecision.Binding.Tier, "",
	)
	mustf(t, err, "rebuild current lifecycle pricing decision: %v")
	job.EconomicPlan = economic
	job.EstimatedUSD = economic.InitialBuyerChargeUSD
	job.SLAPremiumUSD = economic.Input.SLAPremiumUSD
	job.PlacementRequirement = placement
	job.PricingDecision = pricing
	job.HWClasses = append([]string(nil), placement.HWClasses...)
	job.MinMemoryGB = placement.MinMemoryGB
	job.OfferedRateUsdHr = placement.OfferedRateUsdHr
	job.EconomicInputSource = economicInputSourceSubmitStream
	if len(job.PlacementRequirement.HWClasses) != 1 {
		t.Fatalf("current placement hardware classes=%v, want one exact class",
			job.PlacementRequirement.HWClasses)
	}
	return ctx, store, pool, fixture, job, tasks
}

func TestCurrentTaskStartAndCommitRefuseWorkerIdentityMutationAfterClaim(t *testing.T) {
	ctx, store, _, fixture, job, tasks := seedCurrentIdentityLifecycleJob(t)
	exact := currentIdentityWorkerCapability(
		fixture.WorkerID, fixture.SupplierID, job.PlacementRequirement, job)
	mustf(t, store.UpsertWorker(ctx, exact), "register exact current worker: %v")
	mustf(t, store.SubmitJobTx(ctx, job, tasks), "submit current identity job: %v")

	claimed, err := store.ClaimTasksTx(ctx, WorkerAuth{
		WorkerID: fixture.WorkerID, SupplierID: fixture.SupplierID,
	})
	mustf(t, err, "claim with exact current identity: %v")
	if claimed == nil || claimed.TaskID != tasks[0].ID {
		t.Fatalf("exact current claim=%+v, want task %s", claimed, tasks[0].ID)
	}
	mustf(t, store.StartTask(ctx, tasks[0].ID, fixture.WorkerID, 0),
		"exact post-claim start acknowledgement: %v")

	mutateWorkerToDifferentDirectedIdentity(t, ctx, store, exact)
	if err := store.StartTask(ctx, tasks[0].ID, fixture.WorkerID, 0); !errors.Is(err, errNotFound) {
		t.Fatalf("StartTask after build/device mutation error=%v, want errNotFound", err)
	}
	if _, err := store.CompleteTaskTx(
		ctx, tasks[0].ID, fixture.WorkerID, commitFor(fixture, tasks[0].ID, 0),
	); !errors.Is(err, errNotFound) {
		t.Fatalf("CompleteTaskTx after build/device mutation error=%v, want errNotFound", err)
	}

	// A registration replay restoring the exact frozen execution identity is
	// admissible and proves the fence is equality-based rather than a permanent
	// lockout caused by registration itself.
	mustf(t, store.UpsertWorker(ctx, exact), "restore exact claimed worker identity: %v")
	mustf(t, store.StartTask(ctx, tasks[0].ID, fixture.WorkerID, 0),
		"exact restored start acknowledgement: %v")
	if _, err := store.CompleteTaskTx(
		ctx, tasks[0].ID, fixture.WorkerID, commitFor(fixture, tasks[0].ID, 0),
	); err != nil {
		t.Fatalf("exact restored build/device could not commit: %v", err)
	}
}

func TestCurrentPinnedTiebreakDirectStartRequiresExactBuildAndDevice(t *testing.T) {
	ctx, store, pool, fixture, job, tasks := seedCurrentIdentityLifecycleJob(t)
	primary := currentIdentityWorkerCapability(
		fixture.WorkerID, fixture.SupplierID, job.PlacementRequirement, job)
	peer := currentIdentityWorkerCapability(
		fixture.OtherWorkerID, fixture.OtherSupplierID, job.PlacementRequirement, job)
	mustf(t, store.UpsertWorker(ctx, primary), "register exact primary: %v")
	mustf(t, store.UpsertWorker(ctx, peer), "register exact tiebreak peer: %v")
	mustf(t, store.SubmitJobTx(ctx, job, tasks), "submit current tiebreak job: %v")

	claimed, err := store.ClaimTasksTx(ctx, WorkerAuth{
		WorkerID: fixture.WorkerID, SupplierID: fixture.SupplierID,
	})
	mustf(t, err, "claim primary with exact identity: %v")
	if claimed == nil || claimed.TaskID != tasks[0].ID {
		t.Fatalf("primary claim=%+v, want task %s", claimed, tasks[0].ID)
	}
	before := readDynamicObligationSnapshot(
		t, ctx, pool, fixture.JobID, tasks[0].ID, tasks[0].ChunkIndex)
	unsafe := peer
	unsafe.UnsandboxedOptIn = true
	mustf(t, store.UpsertWorker(ctx, unsafe), "register unsandboxed tiebreak peer: %v")
	if _, err := store.InsertTiebreakTask(
		ctx, fixture.JobID, tasks[0].ID, fixture.OtherWorkerID,
		tasks[0].InputRef, tasks[0].ChunkIndex,
	); !errors.Is(err, ErrNoSupply) {
		t.Fatalf("unsandboxed tiebreak insertion error=%v, want ErrNoSupply", err)
	}
	if got := readDynamicObligationSnapshot(
		t, ctx, pool, fixture.JobID, tasks[0].ID, tasks[0].ChunkIndex,
	); got != before {
		t.Fatalf("unsandboxed tiebreak changed reserve/task projection: before=%+v after=%+v", before, got)
	}
	mustf(t, store.UpsertWorker(ctx, peer), "restore sandboxed tiebreak peer: %v")
	if _, err := pool.Exec(ctx,
		`UPDATE suppliers SET owner_buyer_id=$2 WHERE id=$1`,
		fixture.OtherSupplierID, fixture.BuyerID,
	); err != nil {
		t.Fatalf("link tiebreak supplier to buyer: %v", err)
	}
	if _, err := store.InsertTiebreakTask(
		ctx, fixture.JobID, tasks[0].ID, fixture.OtherWorkerID,
		tasks[0].InputRef, tasks[0].ChunkIndex,
	); !errors.Is(err, ErrNoSupply) {
		t.Fatalf("buyer-linked tiebreak insertion error=%v, want ErrNoSupply", err)
	}
	if got := readDynamicObligationSnapshot(
		t, ctx, pool, fixture.JobID, tasks[0].ID, tasks[0].ChunkIndex,
	); got != before {
		t.Fatalf("buyer-linked tiebreak changed reserve/task projection: before=%+v after=%+v", before, got)
	}
	if _, err := pool.Exec(ctx,
		`UPDATE suppliers SET owner_buyer_id=NULL WHERE id=$1`, fixture.OtherSupplierID,
	); err != nil {
		t.Fatalf("unlink tiebreak supplier from buyer: %v", err)
	}
	tiebreakID, err := store.InsertTiebreakTask(
		ctx, fixture.JobID, tasks[0].ID, fixture.OtherWorkerID, tasks[0].InputRef, tasks[0].ChunkIndex)
	mustf(t, err, "insert current pinned tiebreak: %v")

	if err := store.UpsertWorker(ctx, differentDirectedIdentity(peer)); !errors.Is(err, errWorkerIdentityPinned) {
		t.Fatalf("live pinned tiebreak identity mutation error=%v, want errWorkerIdentityPinned", err)
	}
	mustf(t, store.StartTask(ctx, tiebreakID, fixture.OtherWorkerID, 0),
		"direct tiebreak start after refused identity mutation: %v")
	var frozenBuild, frozenHardware string
	mustf(t, pool.QueryRow(ctx, `
		SELECT COALESCE(execution_build_hash,''),COALESCE(execution_hardware_identity,'')
		  FROM tasks WHERE id=$1`, tiebreakID).Scan(&frozenBuild, &frozenHardware),
		"read tiebreak execution identity: %v")
	if frozenBuild != peer.BuildHash || frozenHardware != peer.HardwareIdentity {
		t.Fatalf("tiebreak froze build/device %q/%q, want %q/%q",
			frozenBuild, frozenHardware, peer.BuildHash, peer.HardwareIdentity)
	}
}

func TestCurrentHedgePeerReregistrationCannotConsumeReserveOrCreateTask(t *testing.T) {
	ctx, store, pool, fixture, job, tasks := seedCurrentIdentityLifecycleJob(t)
	primary := currentIdentityWorkerCapability(
		fixture.WorkerID, fixture.SupplierID, job.PlacementRequirement, job)
	peer := currentIdentityWorkerCapability(
		fixture.OtherWorkerID, fixture.OtherSupplierID, job.PlacementRequirement, job)
	mustf(t, store.UpsertWorker(ctx, primary), "register exact hedge primary: %v")
	mustf(t, store.UpsertWorker(ctx, peer), "register exact hedge peer: %v")
	mustf(t, store.SubmitJobTx(ctx, job, tasks), "submit current hedge job: %v")

	claimed, err := store.ClaimTasksTx(ctx, WorkerAuth{
		WorkerID: fixture.WorkerID, SupplierID: fixture.SupplierID,
	})
	mustf(t, err, "claim current hedge anchor: %v")
	if claimed == nil || claimed.TaskID != tasks[0].ID {
		t.Fatalf("hedge anchor claim=%+v, want task %s", claimed, tasks[0].ID)
	}
	mustf(t, store.StartTask(ctx, tasks[0].ID, fixture.WorkerID, 0),
		"start current hedge anchor: %v")

	before := readDynamicObligationSnapshot(
		t, ctx, pool, fixture.JobID, tasks[0].ID, tasks[0].ChunkIndex)
	// Simulate the selection/insertion race: this peer was exact when selected,
	// then legitimately re-registered to another directed execution identity.
	mutateWorkerToDifferentDirectedIdentity(t, ctx, store, peer)
	if _, err := store.InsertHedgeTask(ctx, fixture.JobID, tasks[0].ID,
		fixture.OtherWorkerID, tasks[0].InputRef, tasks[0].ChunkIndex); !errors.Is(err, ErrNoSupply) {
		t.Fatalf("hedge insertion after peer identity mutation error=%v, want ErrNoSupply", err)
	}
	after := readDynamicObligationSnapshot(
		t, ctx, pool, fixture.JobID, tasks[0].ID, tasks[0].ChunkIndex)
	if after != before {
		t.Fatalf("refused peer mutation changed reserve/task projection: before=%+v after=%+v", before, after)
	}

	// The opposite race ordering is equally closed. Restore the peer, insert a
	// valid hedge (consuming its reserve), then prove registration cannot mutate
	// the live pinned worker into a different execution identity.
	mustf(t, store.UpsertWorker(ctx, peer), "restore exact peer after pre-insert refusal: %v")
	hedgeID, err := store.InsertHedgeTask(ctx, fixture.JobID, tasks[0].ID,
		fixture.OtherWorkerID, tasks[0].InputRef, tasks[0].ChunkIndex)
	mustf(t, err, "insert exact current hedge: %v")
	if hedgeID == uuid.Nil {
		t.Fatal("exact current hedge returned nil task")
	}
	inserted := readDynamicObligationSnapshot(
		t, ctx, pool, fixture.JobID, tasks[0].ID, tasks[0].ChunkIndex)
	if err := store.UpsertWorker(ctx, differentDirectedIdentity(peer)); !errors.Is(err, errWorkerIdentityPinned) {
		t.Fatalf("post-insert pinned hedge mutation error=%v, want errWorkerIdentityPinned", err)
	}
	var authorizedBefore time.Time
	mustf(t, pool.QueryRow(ctx, `
		SELECT authorized_at FROM worker_authorized_capabilities
		 WHERE worker_id=$1 AND cell_id=$2 AND runtime_id=$3`,
		fixture.OtherWorkerID, job.PlacementRequirement.RuntimeCellID,
		job.PlacementRequirement.RuntimeID,
	).Scan(&authorizedBefore), "read live hedge WAC source time: %v")
	cached := peer
	cached.Benchmarks = append([]BenchResult(nil), peer.Benchmarks...)
	cached.Benchmarks[0].MeasuredUnix = uint64(runtimeCellPerformanceNow().
		Add(-workerBenchmarkMaxAge + time.Hour).Unix())
	mustf(t, store.UpsertWorker(ctx, cached),
		"re-register live hedge from still-fresh cached benchmark: %v")
	var authorizedAfter time.Time
	mustf(t, pool.QueryRow(ctx, `
		SELECT authorized_at FROM worker_authorized_capabilities
		 WHERE worker_id=$1 AND cell_id=$2 AND runtime_id=$3`,
		fixture.OtherWorkerID, job.PlacementRequirement.RuntimeCellID,
		job.PlacementRequirement.RuntimeID,
	).Scan(&authorizedAfter), "read post-cache live hedge WAC source time: %v")
	if !authorizedAfter.Equal(authorizedBefore) {
		t.Fatalf("near-expiry cache regressed live hedge WAC: before=%s after=%s",
			authorizedBefore, authorizedAfter)
	}
	raisedAsk := peer
	raisedAsk.MinPayoutUsdHr = job.OfferedRateUsdHr + 100
	if err := store.UpsertWorker(ctx, raisedAsk); !errors.Is(err, errWorkerIdentityPinned) {
		t.Fatalf("same-identity live hedge ask mutation error=%v, want errWorkerIdentityPinned", err)
	}
	removedWAC := peer
	removedWAC.SupportedJobs = []string{"media_rendering"}
	removedWAC.SupportedModels = []string{"svg-scene-render-v1"}
	removedWAC.Benchmarks = nil
	if err := store.UpsertWorker(ctx, removedWAC); !errors.Is(err, errWorkerIdentityPinned) {
		t.Fatalf("same-identity live hedge WAC removal error=%v, want errWorkerIdentityPinned", err)
	}
	if got := readDynamicObligationSnapshot(
		t, ctx, pool, fixture.JobID, tasks[0].ID, tasks[0].ChunkIndex); got != inserted {
		t.Fatalf("refused post-insert mutation changed reserve/task projection: before=%+v after=%+v", inserted, got)
	}
	// Hedges re-enter the ordinary claimant boundary, which freezes their exact
	// execution tuple. Retire the already-priced initial redundancy so this
	// claim deterministically addresses the newly inserted hedge.
	if _, err := pool.Exec(ctx,
		`UPDATE tasks SET status='cancelled' WHERE id=$1 AND status='queued'`, tasks[1].ID,
	); err != nil {
		t.Fatalf("retire initial redundancy before hedge claim: %v", err)
	}
	claimedHedge, err := store.ClaimTasksTx(ctx, WorkerAuth{
		WorkerID: fixture.OtherWorkerID, SupplierID: fixture.OtherSupplierID,
	})
	mustf(t, err, "claim live hedge after refused registration drift: %v")
	if claimedHedge == nil || claimedHedge.TaskID != hedgeID {
		t.Fatalf("post-drift hedge claim=%+v, want task %s", claimedHedge, hedgeID)
	}
	mustf(t, store.StartTask(ctx, hedgeID, fixture.OtherWorkerID, 0),
		"live hedge became unclaimable after refused registration drift: %v")
}
