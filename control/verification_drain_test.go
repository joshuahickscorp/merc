package main

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
)

func syntheticLeasedVerificationWork(n int) []LeasedVerificationWork {
	out := make([]LeasedVerificationWork, n)
	for i := range out {
		id := uuid.New()
		out[i] = LeasedVerificationWork{
			Work: VerificationWork{ID: id, CreatedAt: time.Unix(1_800_000_000+int64(i), 0)},
			Lease: VerificationLease{
				WorkID: id, Owner: "test", Token: uuid.New(),
				ExpiresAt: time.Now().Add(time.Minute),
			},
		}
	}
	return out
}

func TestVerificationDrainUsesEverySafeSlotWithoutExceedingBudget(t *testing.T) {
	const total = 100
	queue := syntheticLeasedVerificationWork(total)
	budget := newVerificationResourceBudget(20, verificationWorkerLeaderConnections, 1024)
	store := &Store{verificationResources: budget}
	processor := NewVerificationProcessor(store, nil, nil)

	var queueMu sync.Mutex
	processor.claimWork = func(_ context.Context, _ string, _ time.Duration, limit int) ([]LeasedVerificationWork, error) {
		queueMu.Lock()
		defer queueMu.Unlock()
		if len(queue) == 0 {
			return nil, nil
		}
		if limit > len(queue) {
			limit = len(queue)
		}
		claimed := append([]LeasedVerificationWork(nil), queue[:limit]...)
		queue = queue[limit:]
		return claimed, nil
	}

	var active, peak atomic.Int64
	seen := sync.Map{}
	processor.processWork = func(_ context.Context, leased LeasedVerificationWork) (VerificationProcessResult, error) {
		now := active.Add(1)
		for {
			prior := peak.Load()
			if now <= prior || peak.CompareAndSwap(prior, now) {
				break
			}
		}
		if _, duplicate := seen.LoadOrStore(leased.Work.ID, true); duplicate {
			t.Errorf("work %s processed twice", leased.Work.ID)
		}
		time.Sleep(5 * time.Millisecond)
		active.Add(-1)
		return VerificationProcessResult{Outcome: OutcomePass}, nil
	}

	if err := processor.Drain(context.Background(), total); err != nil {
		t.Fatal(err)
	}
	processed := 0
	seen.Range(func(_, _ any) bool {
		processed++
		return true
	})
	if processed != total {
		t.Fatalf("processed=%d want=%d", processed, total)
	}
	if got, want := peak.Load(), int64(budget.processCapacity()); got != want {
		t.Fatalf("peak concurrency=%d want safe capacity=%d", got, want)
	}
	if got := len(budget.processSlots); got != 0 {
		t.Fatalf("%d verification permits leaked", got)
	}
}

func TestVerificationDrainCancellationReturnsEveryClaimToRetry(t *testing.T) {
	const total = 8
	queue := syntheticLeasedVerificationWork(total)
	budget := newVerificationResourceBudget(29, verificationWorkerLeaderConnections, 1024)
	store := &Store{verificationResources: budget}
	processor := NewVerificationProcessor(store, nil, nil)

	var claimOnce sync.Once
	processor.claimWork = func(_ context.Context, _ string, _ time.Duration, _ int) ([]LeasedVerificationWork, error) {
		var claimed []LeasedVerificationWork
		claimOnce.Do(func() { claimed = append([]LeasedVerificationWork(nil), queue...) })
		return claimed, nil
	}
	started := make(chan struct{}, total)
	processor.processWork = func(ctx context.Context, _ LeasedVerificationWork) (VerificationProcessResult, error) {
		started <- struct{}{}
		<-ctx.Done()
		return VerificationProcessResult{}, ctx.Err()
	}
	var releaseMu sync.Mutex
	released := map[uuid.UUID]int{}
	processor.releaseWork = func(_ context.Context, lease VerificationLease, retryAt time.Time, cause string) error {
		if retryAt.After(time.Now().Add(2*time.Second)) || cause != context.Canceled.Error() {
			t.Errorf("release was not promptly retryable: retryAt=%s cause=%q", retryAt, cause)
		}
		releaseMu.Lock()
		released[lease.WorkID]++
		releaseMu.Unlock()
		return nil
	}

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- processor.Drain(ctx, total) }()
	for range total {
		<-started
	}
	cancel()
	if err := <-done; !errors.Is(err, context.Canceled) {
		t.Fatalf("drain error=%v want context cancellation", err)
	}
	releaseMu.Lock()
	defer releaseMu.Unlock()
	if len(released) != total {
		t.Fatalf("released=%d want=%d", len(released), total)
	}
	for id, count := range released {
		if count != 1 {
			t.Fatalf("work %s released %d times", id, count)
		}
	}
	if got := len(budget.processSlots); got != 0 {
		t.Fatalf("%d verification permits leaked", got)
	}
}

func TestVerificationChunkLockSerializesSameChunkOnly(t *testing.T) {
	databaseURL := requireTestDatabase(t)
	pool, err := pgxpool.New(context.Background(), databaseURL)
	if err != nil {
		t.Fatal(err)
	}
	defer pool.Close()
	processor := NewVerificationProcessor(NewStore(pool), nil, nil)
	jobID := uuid.New()

	unlockFirst, err := processor.lockChunk(context.Background(), jobID, 7)
	if err != nil {
		t.Fatal(err)
	}
	defer unlockFirst()
	if unlock, err := processor.lockChunk(context.Background(), jobID, 7); !errors.Is(err, ErrVerificationChunkBusy) {
		if err == nil {
			unlock()
		}
		t.Fatalf("same chunk lock error=%v want busy", err)
	}
	unlockOther, err := processor.lockChunk(context.Background(), jobID, 8)
	if err != nil {
		t.Fatalf("different chunk was unnecessarily serialized: %v", err)
	}
	unlockOther()
}

func TestCancelledRecoveryMakesEveryDatabaseLeasePromptlyRetryable(t *testing.T) {
	ctx, store, pool := openMoneyPathStore(t)
	fixture := seedMoneyPathFixture(t, ctx, store, pool, moneyPathSeedOpts{
		TaskCount: 8, TaskStatus: "running", ClaimWorker: true, SeedJob: true, SeedPlanRows: true,
	})
	for _, taskID := range fixture.TaskIDs {
		if _, err := store.CompleteTaskTx(ctx, taskID, fixture.WorkerID, commitFor(fixture, taskID, 0)); err != nil {
			t.Fatalf("complete task %s: %v", taskID, err)
		}
	}
	store.verificationResources = newVerificationResourceBudget(
		29, verificationWorkerLeaderConnections, verificationArtifactMemoryCeiling,
	)
	processor := NewVerificationProcessor(store, nil, nil)
	started := make(chan struct{}, len(fixture.TaskIDs))
	processor.processWork = func(ctx context.Context, _ LeasedVerificationWork) (VerificationProcessResult, error) {
		started <- struct{}{}
		<-ctx.Done()
		return VerificationProcessResult{}, ctx.Err()
	}

	drainCtx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- processor.Drain(drainCtx, len(fixture.TaskIDs)) }()
	for range fixture.TaskIDs {
		<-started
	}
	cancel()
	if err := <-done; !errors.Is(err, context.Canceled) {
		t.Fatalf("drain error=%v want cancellation", err)
	}

	var pending, leased int
	if err := pool.QueryRow(ctx, `
		SELECT count(*) FILTER (WHERE status='pending' AND lease_token IS NULL AND lease_expires_at IS NULL),
		       count(*) FILTER (WHERE status='leased')
		  FROM verification_work WHERE job_id=$1`,
		fixture.JobID,
	).Scan(&pending, &leased); err != nil {
		t.Fatal(err)
	}
	if pending != len(fixture.TaskIDs) || leased != 0 {
		t.Fatalf("post-cancel verification rows pending=%d leased=%d want=%d/0",
			pending, leased, len(fixture.TaskIDs))
	}
}
