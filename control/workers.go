package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http"
	"sync"
	"sync/atomic"
	"time"

	"github.com/google/uuid"
	"golang.org/x/sync/errgroup"
)

type Workers struct {
	store        *Store
	storage      *Storage
	payout       Payout
	client       *http.Client
	verification *VerificationProcessor
}

func NewWorkers(store *Store, storage *Storage, payout Payout) *Workers {
	return &Workers{
		store:        store,
		storage:      storage,
		payout:       payout,
		client:       newWebhookHTTPClient(),
		verification: NewVerificationProcessor(store, storage, NewVerifier(store).WithStorage(storage)),
	}
}

const (
	payoutInterval               = 60 * time.Second
	payoutSendingLease           = 5 * time.Minute
	payoutIdempotencyRetryWindow = 23 * time.Hour
	staleInterval                = 30 * time.Second
	pinnedTiebreakInterval       = 15 * time.Second
	pinnedTiebreakTimeout        = 2 * time.Minute
	finalizationInterval         = 20 * time.Second
	webhookInterval              = 20 * time.Second
	webhookDeliveryLease         = 2 * time.Minute
	webhookDeliveryConcurrency   = 8
	webhookDeliveryBatch         = 16 // 16/8 × 10s timeout stays below the 60s readiness window
	webhookDeliveryMaxAttempts   = 12
	hedgeInterval                = 30 * time.Second
	disputeInterval              = 20 * time.Second // buyer-dispute re-verification + resolution
	staleTaskTimeout             = 30 * time.Minute // claim older than this with no commit -> stale
	staleBackoff                 = 1 * time.Minute  // delay before a requeued task is visible
	maxTaskRetries               = 3                // requeue this many times before failing
	sweepBatch                   = 100              // max rows handled per tick
	budgetStopInterval           = 7 * time.Second
	hedgeAfter                   = 90 * time.Second // 2 × ~45s target per-task time
	hedgeMaxInFlight             = 4                // concurrent hedges per job
	hedgeBatch                   = 20               // max new hedges per tick
	endgameRaceInterval          = 5 * time.Second
	endgameRaceMinRun            = 10 * time.Second
	hedgeThrottledAfter          = 15 * time.Second
	stuckInterval                = 30 * time.Second
	stuckEtaFactor               = 1.5
	stuckProgressGrace           = 2 * hedgeAfter
	deadWorkerAfter              = 180 * time.Second
	telemetryRetentionInterval   = 1 * time.Hour
	workerMemorySampleRetention  = 14 * 24 * time.Hour
	taskDurationRetention        = 30 * 24 * time.Hour
	jobEventRetention            = 180 * 24 * time.Hour
)

var telemetryTables = []string{"worker_memory_samples", "task_durations", "job_events"}

const staleMultiple = 3

type tickerLiveness struct {
	mu      sync.RWMutex
	entries map[string]*tickerStat
}

type tickerStat struct {
	interval    time.Duration
	lastSuccess time.Time // zero until the first successful run
	failures    int64
}

var liveness = &tickerLiveness{entries: map[string]*tickerStat{}}

func (l *tickerLiveness) register(name string, interval time.Duration) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if _, ok := l.entries[name]; !ok {
		l.entries[name] = &tickerStat{interval: interval}
	}
}

func (l *tickerLiveness) markSuccess(name string, t time.Time) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if e, ok := l.entries[name]; ok {
		e.lastSuccess = t
	}
}

func (l *tickerLiveness) markFailure(name string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if e, ok := l.entries[name]; ok {
		e.failures++
	}
}

func (l *tickerLiveness) failureSnapshot() map[string]int64 {
	l.mu.RLock()
	defer l.mu.RUnlock()
	out := make(map[string]int64, len(l.entries))
	for name, e := range l.entries {
		out[name] = e.failures
	}
	return out
}

func (l *tickerLiveness) stale(now, since time.Time) []string {
	l.mu.RLock()
	defer l.mu.RUnlock()
	var bad []string
	for name, e := range l.entries {
		budget := time.Duration(staleMultiple) * e.interval
		ref := e.lastSuccess
		if ref.IsZero() {
			ref = since // never succeeded -> measure staleness from the loop start
		}
		if now.Sub(ref) > budget {
			bad = append(bad, name)
		}
	}
	return bad
}

func (l *tickerLiveness) snapshot(now, since time.Time) map[string]float64 {
	l.mu.RLock()
	defer l.mu.RUnlock()
	out := make(map[string]float64, len(l.entries))
	for name, e := range l.entries {
		ref := e.lastSuccess
		if ref.IsZero() {
			ref = since
		}
		out[name] = now.Sub(ref).Seconds()
	}
	return out
}

var workersStartedAtNano atomic.Int64

func workersStarted() time.Time {
	n := workersStartedAtNano.Load()
	if n == 0 {
		return time.Time{}
	}
	return time.Unix(0, n)
}

func (wk *Workers) Run(ctx context.Context) {
	workersStartedAtNano.Store(time.Now().UnixNano())
	tickers := []struct {
		interval time.Duration
		name     string
		fn       func(context.Context) error
	}{
		{payoutInterval, "payout-release", wk.releasePayouts},
		{verificationRecoveryInterval, "verification-recovery", wk.recoverVerification},
		{pinnedTiebreakInterval, "pinned-tiebreak-recovery", wk.recoverPinnedTiebreaks},
		{staleInterval, "stale-requeue", wk.requeueStaleTasks},
		{finalizationInterval, "job-finalize", wk.finalizeJobs},
		{webhookInterval, "webhook-sweep", wk.deliverPendingWebhooks},
		{hedgeInterval, "straggler-hedge", wk.hedgeStragglers},
		{endgameRaceInterval, "endgame-race", wk.raceEndgameTails},
		{stuckInterval, "stuck-reaper", wk.reapStuckJobs},
		{stuckInterval, "dead-claim-rescue", wk.rescueDeadClaims},
		{reconcileInterval, "ledger-reconcile", wk.reconcileLedger},
		{disputeInterval, "dispute-resolve", wk.resolveDisputes},
		{chargeCollectInterval, "charge-collect", wk.collectCharges},
		{telemetryRetentionInterval, "telemetry-retention", wk.sweepTelemetryRetention},
		{budgetStopInterval, "budget-stop-sweep", wk.sweepBudgetStops},
		{noPeerWatchdogInterval, "no-peer-watchdog", wk.reapNoPeerWedged},
	}
	var loops sync.WaitGroup
	loops.Add(len(tickers))
	for _, ticker := range tickers {
		t := ticker
		liveness.register(t.name, t.interval)
		go func() {
			defer loops.Done()
			wk.tick(ctx, t.interval, t.name, t.fn)
		}()
	}
	<-ctx.Done()
	log.Print("workers: shutting down; waiting for active sweeps")
	loops.Wait()
	log.Print("workers: all sweeps stopped")
}

const verificationRecoveryInterval = 2 * time.Second

func (wk *Workers) recoverVerification(ctx context.Context) error {
	return wk.verification.Drain(ctx, sweepBatch)
}

func (wk *Workers) recoverPinnedTiebreaks(ctx context.Context) error {
	stale, err := wk.store.StalePinnedTiebreaks(ctx, pinnedTiebreakTimeout, sweepBatch)
	if err != nil {
		return err
	}
	for _, item := range stale {
		_, also, alsoSuppliers, err := wk.store.PinnedTiebreakExclusions(ctx, item)
		if err != nil {
			return err
		}
		candidates, err := wk.store.CandidateWorkers(ctx, item.JobType, item.ModelRef, item.MinMemoryGB)
		if err != nil {
			return err
		}
		excludedWorkers := make(map[uuid.UUID]bool, len(also)+1)
		for _, id := range also {
			excludedWorkers[id] = true
		}
		excludedWorkers[item.PinnedWorker] = true
		excludedSuppliers := make(map[uuid.UUID]bool, len(alsoSuppliers))
		for _, id := range alsoSuppliers {
			excludedSuppliers[id] = true
		}
		frozenCandidates := make([]MatchWorker, 0, len(candidates))
		for _, candidate := range candidates {
			if excludedWorkers[candidate.ID] || excludedSuppliers[candidate.SupplierID] ||
				candidate.HWClass != item.HWClass ||
				(item.Engine != "" && candidate.Engine != item.Engine) ||
				(item.BuildHash != "" && candidate.BuildHash != item.BuildHash) {
				continue
			}
			frozenCandidates = append(frozenCandidates, candidate)
		}
		for _, candidate := range rankPeersBySpeed(frozenCandidates, item.JobType) {
			peer := candidate.ID
			reassigned, err := wk.store.ReassignPinnedTiebreak(ctx, item, peer, pinnedTiebreakTimeout)
			if errors.Is(err, ErrNoSupply) {
				continue
			}
			if err != nil {
				return err
			}
			if reassigned {
				log.Printf("workers: reassigned stale pinned tiebreak %s for job %s from %s to %s",
					item.TaskID, item.JobID, item.PinnedWorker, peer)
			}
			break // reassigned, or another recovery/claim won the CAS
		}
	}
	return nil
}

func (wk *Workers) tick(ctx context.Context, d time.Duration, name string, fn func(context.Context) error) {
	t := time.NewTicker(d)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			if err := fn(ctx); err != nil {
				log.Printf("workers: %s: %v", name, err)
				liveness.markFailure(name)
				continue
			}
			liveness.markSuccess(name, time.Now())
		}
	}
}

func (wk *Workers) releasePayouts(ctx context.Context) error {
	finishAttempt := func(claimed DueHeldEntry, resolvingUnknown bool) error {
		result, sendErr := wk.payout.Send(ctx, claimed.SupplierID, claimed.RequestedCents,
			claimed.Currency, claimed.ID.String())
		if sendErr != nil {
			var state string
			var stateErr error
			if resolvingUnknown || !errors.Is(sendErr, errPayoutDefinitelyNotSent) {
				state, stateErr = wk.store.MarkPayoutOutcomeUnknown(ctx, claimed.ID, sendErr)
			} else {
				state, stateErr = wk.store.DeferPayout(ctx, claimed.ID, sendErr)
			}
			if stateErr != nil {
				return stateErr
			}
			log.Printf("workers: payout %s unresolved in state %s ($%.6f to %s): %v",
				claimed.ID, state, claimed.AmountUSD, claimed.SupplierID, sendErr)
			return nil
		}
		state, err := wk.store.FinalizePayout(ctx, claimed.ID, result)
		if err != nil {
			return err
		}
		switch state {
		case PayoutReleased:
			metrics.payoutsReleased.Add(1)
		case PayoutReversalRequired:
			log.Printf("workers: payout %s crossed the provider boundary during clawback; external reversal required (not implemented)", claimed.ID)
		case PayoutExported:
			log.Printf("workers: payout %s exported for manual settlement; no supplier cash is reported moved", claimed.ID)
		}
		return nil
	}

	if _, err := wk.store.RecoverStalePayoutOperations(ctx, payoutSendingLease, sweepBatch); err != nil {
		return err
	}
	unknown, err := wk.store.ClaimOutcomeUnknownPayouts(
		ctx, payoutSendingLease, payoutIdempotencyRetryWindow, sweepBatch)
	if err != nil {
		return err
	}
	for _, claimed := range unknown {
		if err := finishAttempt(claimed, true); err != nil {
			return err
		}
	}
	due, err := wk.store.DuePayouts(ctx, sweepBatch)
	if err != nil {
		return err
	}
	for _, e := range due {
		claimed, ok, cerr := wk.store.ClaimPayout(ctx, e.ID)
		if cerr != nil {
			return cerr
		}
		if !ok {
			continue // clawback or another release worker won the CAS
		}
		if err := finishAttempt(claimed, false); err != nil {
			return err
		}
	}
	return nil
}

func (wk *Workers) requeueStaleTasks(ctx context.Context) error {
	stale, err := wk.store.StaleRunningTasks(ctx, staleTaskTimeout, sweepBatch)
	if err != nil {
		return err
	}
	for _, t := range stale {
		if int(t.RetryCount) >= maxTaskRetries {
			checkpointBeforeFail(ctx, wk.store, wk.storage, t.JobID)
			if ferr := wk.store.FailTaskAndSettleJob(ctx, t.ID, t.JobID); ferr != nil {
				return ferr
			}
			log.Printf("workers: task %s failed after %d retries (job %s settled at completed work)", t.ID, t.RetryCount, t.JobID)
			continue
		}
		shift := t.RetryCount
		if shift > 4 {
			shift = 4
		}
		backoff := staleBackoff << uint(shift)
		if rerr := wk.store.RequeueStaleTask(ctx, t.ID, backoff); rerr != nil {
			return rerr
		}
		log.Printf("workers: requeued stale task %s (retry %d, backoff %s)", t.ID, t.RetryCount+1, backoff)
	}
	return nil
}

func (wk *Workers) resolveDisputes(ctx context.Context) error {
	active, err := wk.store.ActiveDisputes(ctx, sweepBatch)
	if err != nil {
		return err
	}
	for _, d := range active {
		switch d.Status {
		case "open", "no_peer":
			target, ok, terr := wk.store.ReverifyTarget(ctx, d.JobID)
			if terr != nil {
				return terr
			}
			if !ok {
				if serr := wk.store.SetDisputeStatus(ctx, d.ID, "unresolvable"); serr != nil {
					return serr
				}
				continue
			}
			peer, perr := wk.store.SelectRedundancyPeerExcluding(ctx, target.JobType, target.ModelRef, target.MinMemGB, target.AnchorWorker, nil, nil)
			if perr != nil {
				if serr := wk.store.SetDisputeStatus(ctx, d.ID, "no_peer"); serr != nil {
					return serr
				}
				continue
			}
			reverifyID, ierr := wk.store.InsertTiebreakTask(ctx, d.JobID, target.TaskID, peer, target.InputRef, target.ChunkIndex)
			if ierr != nil {
				return ierr
			}
			if serr := wk.store.SetDisputeReverifying(ctx, d.ID, reverifyID); serr != nil {
				return serr
			}
			_ = wk.store.InsertJobEvent(ctx, d.JobID, nil, "dispute_reverifying", "Dispute: independent re-verification dispatched", nil)
		case "reverifying":
			pending, perr := wk.store.JobHasPendingTasks(ctx, d.JobID)
			if perr != nil {
				return perr
			}
			if pending {
				continue // wait until the re-verify (and any cascaded tiebreak) settles
			}
			verdict, text := "rejected", "Dispute rejected: independent re-verification agreed with the original result"
			if target, ok, terr := wk.store.ReverifyTarget(ctx, d.JobID); terr != nil {
				return terr
			} else if ok {
				clawed, cerr := wk.store.TaskHasClawback(ctx, target.TaskID)
				if cerr != nil {
					return cerr
				}
				if clawed {
					verdict, text = "resolved", "Dispute upheld: independent re-verification found the original result was wrong (clawed back)"
				}
			}
			if serr := wk.store.SetDisputeStatus(ctx, d.ID, verdict); serr != nil {
				return serr
			}
			_ = wk.store.InsertJobEvent(ctx, d.JobID, nil, "dispute_"+verdict, text, nil)
		}
	}
	return nil
}

func (wk *Workers) hedgeStragglers(ctx context.Context) error {
	stragglers, err := wk.store.StragglerTasks(ctx, hedgeAfter, hedgeThrottledAfter, hedgeMaxInFlight, hedgeBatch)
	if err != nil {
		return err
	}
	for _, s := range stragglers {
		if !s.ThrottledHedge {
			cold, cerr := wk.store.isColdModelStraggler(ctx, s.TaskID)
			if cerr != nil {
				return cerr
			}
			if cold {
				log.Printf("workers: cold-model straggler task %s (chunk %d of job %s, model %s)  -  suppressing spurious hedge (worker %s still loading an uncached model, not wedged)", s.TaskID, s.ChunkIndex, s.JobID, s.ModelRef, s.WorkerID)
				metrics.coldModelHedgesSuppressed.Add(1)
				continue
			}
		}
		representedSuppliers, rerr := wk.representedChunkSuppliers(ctx, s.JobID, s.ChunkIndex)
		if rerr != nil {
			return rerr
		}
		peer, perr := wk.store.SelectRedundancyPeerExcluding(ctx, s.JobType, s.ModelRef, s.MinMemGB,
			s.WorkerID, nil, representedSuppliers)
		if errors.Is(perr, ErrNoSupply) {
			continue // no distinct same-class worker free  -  leave it; the stale reaper still guards it
		}
		if perr != nil {
			return perr
		}
		if _, ierr := wk.store.InsertHedgeTask(ctx, s.JobID, s.TaskID, peer, s.InputRef, s.ChunkIndex); ierr != nil {
			return ierr
		}
		metrics.hedges.Add(1)
		if s.ThrottledHedge {
			metrics.throttledHedges.Add(1)
			log.Printf("workers: hedged THROTTLED-worker straggler task %s (chunk %d of job %s) to peer %s (worker %s reporting throttled=true)", s.TaskID, s.ChunkIndex, s.JobID, peer, s.WorkerID)
		} else {
			log.Printf("workers: hedged straggler task %s (chunk %d of job %s) to peer %s", s.TaskID, s.ChunkIndex, s.JobID, peer)
		}
	}
	return nil
}

func (wk *Workers) raceEndgameTails(ctx context.Context) error {
	if !fanoutPlannerEnabled.Load() {
		return nil
	}
	tails, err := wk.store.EndgameTailTasks(ctx, endgameRaceMinRun, hedgeMaxInFlight, hedgeBatch)
	if err != nil {
		return err
	}
	for _, s := range tails {
		cold, cerr := wk.store.isColdModelStraggler(ctx, s.TaskID)
		if cerr != nil {
			return cerr
		}
		if cold {
			log.Printf("workers: endgame race: task %s (chunk %d of job %s, model %s) is a cold-model straggler  -  not raced (duplicate would pay the same cold load)", s.TaskID, s.ChunkIndex, s.JobID, s.ModelRef)
			continue
		}
		representedSuppliers, rerr := wk.representedChunkSuppliers(ctx, s.JobID, s.ChunkIndex)
		if rerr != nil {
			return rerr
		}
		peer, perr := wk.selectEndgameRacePeerExcluding(ctx, s.JobType, s.ModelRef, s.MinMemGB,
			s.WorkerID, representedSuppliers)
		if errors.Is(perr, ErrNoSupply) {
			continue // no idle warm same-class peer  -  the ordinary hedge path still guards this chunk
		}
		if perr != nil {
			return perr
		}
		if _, ierr := wk.store.InsertHedgeTask(ctx, s.JobID, s.TaskID, peer, s.InputRef, s.ChunkIndex); ierr != nil {
			return ierr
		}
		metrics.hedges.Add(1) // an endgame race IS a hedge dispatch (same machinery, same cap)
		metrics.endgameRaces.Add(1)
		log.Printf("workers: endgame race: duplicated slowest running task %s (chunk %d of job %s, type %s) onto idle fastest same-class peer %s  -  zero unclaimed tasks left, worker %s is the job's wall-clock (fan-out planner wave 1B)",
			s.TaskID, s.ChunkIndex, s.JobID, s.JobType, peer, s.WorkerID)
	}
	return nil
}

func (wk *Workers) representedChunkSuppliers(ctx context.Context, jobID uuid.UUID, chunkIndex int) ([]uuid.UUID, error) {
	rows, err := wk.store.pool.Query(ctx, `
		SELECT DISTINCT supplier_id
		  FROM (
		    SELECT t.execution_supplier_id AS supplier_id
		      FROM tasks t
		     WHERE t.job_id=$1 AND COALESCE(t.chunk_index,0)=$2
		       AND NOT COALESCE(t.is_honeypot,false)
		       AND t.execution_supplier_id IS NOT NULL
		    UNION ALL
		    -- An unstarted pin is live routing state rather than execution history;
		    -- reserve its currently assigned supplier until claim-time revalidation.
		    SELECT w.supplier_id
		      FROM tasks t JOIN workers w ON w.id=t.claimed_by
		     WHERE t.job_id=$1 AND COALESCE(t.chunk_index,0)=$2
		       AND NOT COALESCE(t.is_honeypot,false)
		       AND t.status IN ('queued','retrying') AND t.started_at IS NULL
		    UNION ALL
		    SELECT vw.supplier_id
		      FROM verification_work vw JOIN tasks t ON t.id=vw.task_id
		     WHERE t.job_id=$1 AND COALESCE(t.chunk_index,0)=$2
		       AND NOT COALESCE(t.is_honeypot,false)
		    UNION ALL
		    SELECT history.supplier_id
		      FROM task_execution_history history JOIN tasks t ON t.id=history.task_id
		     WHERE t.job_id=$1 AND COALESCE(t.chunk_index,0)=$2
		       AND NOT COALESCE(t.is_honeypot,false)
		  ) represented
		 ORDER BY supplier_id`, jobID, chunkIndex)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var suppliers []uuid.UUID
	for rows.Next() {
		var supplierID uuid.UUID
		if err := rows.Scan(&supplierID); err != nil {
			return nil, err
		}
		suppliers = append(suppliers, supplierID)
	}
	return suppliers, rows.Err()
}

func (wk *Workers) selectEndgameRacePeerExcluding(ctx context.Context, jobType, modelRef string,
	minMemGB float32, anchor uuid.UUID, representedSuppliers []uuid.UUID) (uuid.UUID, error) {
	primary, err := wk.store.GetWorkerProfile(ctx, anchor)
	if err != nil {
		return uuid.Nil, err
	}
	candidates, err := wk.store.CandidateWorkers(ctx, jobType, modelRef, minMemGB)
	if err != nil {
		return uuid.Nil, err
	}
	pruned := prunePeers(candidates, anchor, primary.SupplierID, nil, representedSuppliers)
	ranked, err := Match(MatchTask{
		JobType: jobType, MinMemoryGB: minMemGB,
		HWClasses: []string{primary.HWClass}, PinEngine: primary.Engine,
		PinBuildHash: primary.BuildHash, Tier: "batch",
	}, pruned)
	if err != nil {
		return uuid.Nil, err
	}
	ids := make([]uuid.UUID, len(ranked))
	for i, worker := range ranked {
		ids[i] = worker.ID
	}
	busy, err := wk.store.BusyWorkerIDs(ctx, ids)
	if err != nil {
		return uuid.Nil, err
	}
	idle := make([]MatchWorker, 0, len(ranked))
	for _, worker := range ranked {
		if busy[worker.ID] || (modelRef != "" && !worker.Warm) {
			continue
		}
		idle = append(idle, worker)
	}
	if len(idle) == 0 {
		return uuid.Nil, ErrNoSupply
	}
	return rankPeersBySpeed(idle, jobType)[0].ID, nil
}

func (wk *Workers) reapStuckJobs(ctx context.Context) error {
	if ws := workersStarted(); !ws.IsZero() && time.Since(ws) < stuckProgressGrace {
		return nil
	}
	stuck, err := wk.store.StuckRunningJobs(ctx, stuckEtaFactor, stuckProgressGrace, deadWorkerAfter, sweepBatch)
	if err != nil {
		return err
	}
	for _, j := range stuck {
		cause := "the workload made no progress"
		if j.DeadClaim {
			cause = "the machine running it went unresponsive"
		}

		if j.Strikes == 0 {
			flipped, rerr := wk.store.RescueStuckJob(ctx, j.ID, staleBackoff)
			if rerr != nil {
				log.Printf("workers: rescuing stuck job %s: %v", j.ID, rerr)
				continue
			}
			if !flipped {
				continue // progressed, went terminal, or a concurrent sweep already rescued
			}
			msg := fmt.Sprintf(
				"Run stalled past its deadline (%s); unfinished work was moved to a different machine. "+
					"If it stalls again it will be cancelled with completed work checkpointed.", cause)
			_ = wk.store.InsertJobEvent(ctx, j.ID, nil, "job_stuck_rescued", msg, nil)
			metrics.stuckRescues.Add(1)
			log.Printf("workers: stuck job %s rescued (strike 1 · %d/%d tasks done · dead_claim=%v)",
				j.ID, j.TasksDone, j.TaskCount, j.DeadClaim)
			continue
		}

		if j.OutputRef != "" && j.TasksDone > 0 {
			if _, merr := mergeJobResults(ctx, wk.store, wk.storage, j.ID); merr != nil {
				log.Printf("workers: stuck job %s: checkpoint merge failed: %v (left running for retry)", j.ID, merr)
				continue // do NOT cancel work we could not checkpoint
			}
		}
		flipped, cerr := wk.store.CancelStuckJob(ctx, j.ID)
		if cerr != nil {
			log.Printf("workers: cancelling stuck job %s: %v", j.ID, cerr)
			continue
		}
		if !flipped {
			continue // progressed or went terminal since selection  -  leave it alone
		}
		if serr := wk.store.SetJobActualUSD(ctx, j.ID); serr != nil {
			log.Printf("workers: settling stuck job %s actual_usd: %v", j.ID, serr)
		}
		msg := fmt.Sprintf(
			"Run stuck · stalled past its deadline with no progress (%s) and was cancelled automatically. "+
				"You are charged only for the %d of %d tasks that completed; the rest was never charged. "+
				"Completed results are checkpointed and downloadable.",
			cause, j.TasksDone, j.TaskCount)
		_ = wk.store.InsertJobEvent(ctx, j.ID, nil, "job_stuck_cancelled", msg, wk.stuckPartialDetail(ctx, j.ID))
		metrics.stuckCancels.Add(1)
		log.Printf("workers: stuck job %s cancelled (strike %d · %d/%d tasks done · eta %ds · dead_claim=%v · grace %s)",
			j.ID, j.Strikes, j.TasksDone, j.TaskCount, j.EtaSecs, j.DeadClaim, stuckProgressGrace)
	}
	return nil
}

func (wk *Workers) stuckPartialDetail(ctx context.Context, jobID uuid.UUID) []byte {
	keys, err := wk.store.CancelledTaskResultKeys(ctx, jobID)
	if err != nil {
		log.Printf("workers: stuck job %s: listing cancelled result keys: %v (no partial_urls detail)", jobID, err)
		return nil
	}
	var urls []string
	for _, k := range keys {
		pk := k + ".partial"
		exists, serr := wk.storage.ObjectExists(ctx, pk)
		if serr != nil {
			log.Printf("workers: stuck job %s: checking partial %q: %v", jobID, pk, serr)
			continue
		}
		if !exists {
			continue
		}
		u, perr := wk.storage.PresignGet(ctx, pk, time.Hour)
		if perr != nil {
			log.Printf("workers: stuck job %s: presigning partial %q: %v", jobID, pk, perr)
			continue
		}
		urls = append(urls, u)
	}
	if len(urls) == 0 {
		return nil
	}
	detail, _ := json.Marshal(map[string]any{"partial_urls": urls})
	return detail
}

func (wk *Workers) rescueDeadClaims(ctx context.Context) error {
	if ws := workersStarted(); !ws.IsZero() && time.Since(ws) < stuckProgressGrace {
		return nil
	}
	dead, err := wk.store.DeadClaimedTasks(ctx, deadWorkerAfter, sweepBatch)
	if err != nil {
		return err
	}
	for _, d := range dead {
		rescued, rerr := wk.store.RescueRunningTask(ctx, d.TaskID, staleBackoff)
		if rerr != nil {
			return rerr
		}
		if !rescued {
			continue // committed/failed between selection and here  -  leave it alone
		}
		taskID := d.TaskID
		_ = wk.store.InsertJobEvent(ctx, d.JobID, &taskID, "task_rescued_dead_worker",
			"Chunk moved to a different machine: the machine running it stopped responding. "+
				"No retry was counted against the task.", nil)
		if d.JobType != "custom" && d.SupplierID != uuid.Nil {
			if derr := wk.store.DockReputationMild(ctx, d.SupplierID, EventThermalThrottle); derr != nil {
				log.Printf("workers: docking supplier %s for dead claim on task %s: %v", d.SupplierID, d.TaskID, derr)
			}
		}
		log.Printf("workers: rescued task %s (job %s) from dead worker %s", d.TaskID, d.JobID, d.WorkerID)
	}
	return nil
}

func checkpointBeforeFail(ctx context.Context, store *Store, storage *Storage, jobID uuid.UUID) {
	outputRef, done, err := store.JobCheckpointInfo(ctx, jobID)
	if err != nil {
		log.Printf("fail-path checkpoint: job %s lookup: %v (proceeding with the fail)", jobID, err)
		return
	}
	if outputRef == "" || done == 0 {
		return // nowhere to write, or nothing completed to checkpoint
	}
	if _, err := mergeJobResults(ctx, store, storage, jobID); err != nil {
		log.Printf("fail-path checkpoint: job %s merge: %v (proceeding with the fail  -  completed chunks remain per-task objects)", jobID, err)
	}
}

func recordEtaCalibration(ctx context.Context, store *Store, jobID uuid.UUID) {
	predicted, realized, err := store.RecordEtaCalibration(ctx, jobID)
	if err != nil {
		log.Printf("eta calibration for job %s: %v (finalize unaffected)", jobID, err)
		return
	}
	if predicted > 0 && float64(realized) > 1.2*float64(predicted) {
		metrics.watchdogNearMiss.Add(1)
	}
}

type webhookPayload struct {
	DeliveryID string `json:"delivery_id"`
	Event      string `json:"event"`
	JobID      string `json:"job_id"`
	Status     string `json:"status"`
	ResultsURL string `json:"results_url"`
	TS         string `json:"ts"`
}

func (wk *Workers) finalizeJobs(ctx context.Context) error {
	finalizable, err := wk.store.FinalizableJobs(ctx, sweepBatch)
	if err != nil {
		return err
	}
	for _, j := range finalizable {
		if j.OutputRef != "" {
			if _, merr := mergeJobResults(ctx, wk.store, wk.storage, j.ID); merr != nil {
				log.Printf("workers: merging job %s results: %v (left non-complete for retry)", j.ID, merr)
				continue // do NOT mark complete on a failed merge
			}
		}
		if cerr := wk.store.FinalizeJobTx(ctx, j.ID); cerr != nil {
			log.Printf("workers: completing/settling job %s: %v", j.ID, cerr)
			continue
		}
		_ = wk.store.InsertJobEvent(ctx, j.ID, nil, "job_completed", "Job completed; results ready", nil)
		recordEtaCalibration(ctx, wk.store, j.ID)
	}
	return nil
}

func (wk *Workers) deliverPendingWebhooks(ctx context.Context) error {
	pending, err := wk.store.ClaimPendingWebhooks(ctx, webhookDeliveryBatch, webhookDeliveryLease)
	if err != nil {
		return err
	}
	var deliveries errgroup.Group
	deliveries.SetLimit(webhookDeliveryConcurrency)
	for _, pendingWebhook := range pending {
		p := pendingWebhook
		deliveries.Go(func() error {
			derr := wk.deliverWebhook(ctx, p)
			if derr == nil {
				if err := wk.store.DeliverWebhookTx(ctx, p.ID, p.LeaseToken); err != nil {
					return fmt.Errorf("marking webhook %s delivered: %w", p.ID, err)
				}
				return nil
			}

			nextAttempt := p.Attempts + 1
			backoff := webhookRetryBackoff(nextAttempt)
			attempts, dead, err := wk.store.MarkWebhookFailed(
				ctx, p.ID, p.LeaseToken, derr, webhookFailureIsPermanent(derr),
				backoff, webhookDeliveryMaxAttempts,
			)
			if err != nil {
				return fmt.Errorf("recording webhook %s failure: %w", p.ID, err)
			}
			if dead {
				log.Printf("workers: webhook %s for job %s dead-lettered after %d failed attempt(s): %v",
					p.ID, p.JobID, attempts, derr)
			} else {
				log.Printf("workers: webhook %s for job %s attempt %d failed; retry in %s: %v",
					p.ID, p.JobID, attempts, backoff, derr)
			}
			return nil
		})
	}
	return deliveries.Wait()
}

func (wk *Workers) deliverWebhook(ctx context.Context, p PendingWebhook) error {
	secret, err := openWebhookSigningSecret(p.SigningSecretSealed)
	if err != nil {
		return permanentWebhookFailure(fmt.Errorf("webhook signing secret unavailable: %w", err))
	}
	var resultsURL string
	if keys, err := wk.store.JobResultKeys(ctx, p.JobID); err == nil && len(keys) > 0 {
		if u, perr := wk.storage.PresignGet(ctx, keys[0], time.Hour); perr == nil {
			resultsURL = u
		}
	}
	event := "job." + p.Status
	if p.Status == "complete" {
		event = "job.completed"
	}
	body, _ := json.Marshal(webhookPayload{
		DeliveryID: p.ID.String(),
		Event:      event,
		JobID:      p.JobID.String(),
		Status:     p.Status,
		ResultsURL: resultsURL,
		TS:         time.Now().UTC().Format(time.RFC3339),
	})
	sig := signWebhook(secret, body)

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, p.URL, bytes.NewReader(body))
	if err != nil {
		return permanentWebhookFailure(err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-CX-Delivery-ID", p.ID.String())
	req.Header.Set("X-CX-Signature", sig)
	resp, err := wk.client.Do(req)
	if err != nil {
		if resp != nil && resp.Body != nil {
			resp.Body.Close()
		}
		return err
	}
	resp.Body.Close()
	if resp.StatusCode >= 200 && resp.StatusCode < 300 {
		return nil
	}
	statusErr := fmt.Errorf("webhook endpoint returned %s", resp.Status)
	if webhookHTTPStatusIsRetryable(resp.StatusCode) {
		return statusErr
	}
	return permanentWebhookFailure(statusErr)
}

func (wk *Workers) sweepTelemetryRetention(ctx context.Context) error {
	n, err := wk.store.DeleteOldWorkerMemorySamples(ctx, time.Now().Add(-workerMemorySampleRetention))
	if err != nil {
		return err
	}
	if n > 0 {
		log.Printf("workers: telemetry-retention: pruned %d worker_memory_samples row(s) older than %s", n, workerMemorySampleRetention)
	}
	td, err := wk.store.DeleteOldTaskDurations(ctx, time.Now().Add(-taskDurationRetention))
	if err != nil {
		return err
	}
	if td > 0 {
		log.Printf("workers: telemetry-retention: pruned %d task_durations row(s) older than %s", td, taskDurationRetention)
	}
	je, err := wk.store.DeleteOldJobEvents(ctx, time.Now().Add(-jobEventRetention))
	if err != nil {
		return err
	}
	if je > 0 {
		log.Printf("workers: telemetry-retention: pruned %d job_events row(s) older than %s", je, jobEventRetention)
	}
	return nil
}

func (wk *Workers) sweepBudgetStops(ctx context.Context) error {
	stopped, err := wk.store.SweepBudgetStops(ctx)
	if err != nil {
		return err
	}
	if stopped > 0 {
		metrics.budgetStops.Add(int64(stopped))
	}
	return nil
}
