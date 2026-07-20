package main

import (
	"context"
	"fmt"
	"log"
	"sort"
	"sync/atomic"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

const workerLeaderAdvisoryLock = "computeexchange-background-workers-v1"

const (
	workerLeaderPollInterval      = 5 * time.Second
	workerLeaderHealthInterval    = 5 * time.Second
	workerLeaderStopTimeout       = 45 * time.Second
	workerLeaderStaleSamples      = 2
	workerLeaderObservationMaxAge = 3 * workerLeaderPollInterval
)

var (
	workerElectionReadinessEnabled atomic.Bool
	workerElectionObservedAtNano   atomic.Int64
)

func setWorkerElectionReadinessEnabled(enabled bool) {
	workerElectionObservedAtNano.Store(0)
	workerElectionReadinessEnabled.Store(enabled)
}

func markWorkerElectionObserved(now time.Time) {
	workerElectionObservedAtNano.Store(now.UnixNano())
}

func workerElectionRecentlyObserved(now time.Time) bool {
	if !workerElectionReadinessEnabled.Load() {
		return true
	}
	n := workerElectionObservedAtNano.Load()
	if n == 0 {
		return false
	}
	age := now.Sub(time.Unix(0, n))
	return age >= 0 && age <= workerLeaderObservationMaxAge
}

func tryAcquireWorkerLeadership(ctx context.Context, pool *pgxpool.Pool) (*pgxpool.Conn, bool, error) {
	conn, err := pool.Acquire(ctx)
	if err != nil {
		return nil, false, err
	}
	var acquired bool
	if err := conn.QueryRow(ctx,
		`SELECT pg_try_advisory_lock(hashtextextended($1, 0))`, workerLeaderAdvisoryLock).Scan(&acquired); err != nil {
		conn.Release()
		return nil, false, err
	}
	if !acquired {
		conn.Release()
		return nil, false, nil
	}
	return conn, true, nil
}

func releaseWorkerLeadership(conn *pgxpool.Conn) {
	cleanupCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	var unlocked bool
	err := conn.QueryRow(cleanupCtx,
		`SELECT pg_advisory_unlock(hashtextextended($1, 0))`, workerLeaderAdvisoryLock).Scan(&unlocked)
	if err == nil && unlocked {
		conn.Release()
		return
	}
	raw := conn.Hijack()
	if closeErr := raw.Close(cleanupCtx); closeErr != nil {
		log.Printf("workers: leader connection close failed: %v", closeErr)
	}
	if err != nil {
		log.Printf("workers: leader unlock failed; session discarded: %v", err)
	} else {
		log.Print("workers: leader lock was not held; session discarded")
	}
}

func resetWorkerLiveness() {
	liveness.mu.Lock()
	liveness.entries = map[string]*tickerStat{}
	liveness.mu.Unlock()
	workersStartedAtNano.Store(0)
}

func waitWorkerLeaderRetry(ctx context.Context) bool {
	t := time.NewTimer(workerLeaderPollInterval)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-t.C:
		return true
	}
}

func runWorkerLeader(ctx context.Context, pool *pgxpool.Pool, workers *Workers) {
	followerLogged := false
	for ctx.Err() == nil {
		conn, acquired, err := tryAcquireWorkerLeadership(ctx, pool)
		if err != nil {
			if ctx.Err() == nil {
				log.Printf("workers: leader election: %v", err)
			}
			if !waitWorkerLeaderRetry(ctx) {
				return
			}
			continue
		}
		markWorkerElectionObserved(time.Now())
		if !acquired {
			if !followerLogged {
				log.Print("workers: standby replica; another control holds sweep leadership")
				followerLogged = true
			}
			if !waitWorkerLeaderRetry(ctx) {
				return
			}
			continue
		}

		followerLogged = false
		resetWorkerLiveness()
		log.Print("workers: acquired sweep leadership")
		leaderCtx, cancel := context.WithCancel(ctx)
		done := make(chan struct{})
		go func() {
			defer close(done)
			workers.Run(leaderCtx)
		}()

		reason := monitorWorkerLeadership(leaderCtx, conn)
		cancel()
		select {
		case <-done:
		case <-time.After(workerLeaderStopTimeout):
			log.Fatalf("workers: failed to stop within %s after losing leadership", workerLeaderStopTimeout)
		}
		releaseWorkerLeadership(conn)
		resetWorkerLiveness()
		log.Printf("workers: released sweep leadership: %s", reason)
		if ctx.Err() != nil {
			return
		}
		if !waitWorkerLeaderRetry(ctx) {
			return
		}
	}
}

func monitorWorkerLeadership(ctx context.Context, conn *pgxpool.Conn) string {
	ticker := time.NewTicker(workerLeaderHealthInterval)
	defer ticker.Stop()
	staleSamples := 0
	for {
		select {
		case <-ctx.Done():
			return "shutdown"
		case now := <-ticker.C:
			pingCtx, cancel := context.WithTimeout(ctx, 3*time.Second)
			err := conn.Ping(pingCtx)
			cancel()
			if err != nil {
				return fmt.Sprintf("leader database session failed: %v", err)
			}
			markWorkerElectionObserved(now)

			started := workersStarted()
			if started.IsZero() {
				continue
			}
			bad := liveness.stale(now, started)
			if len(bad) == 0 {
				staleSamples = 0
				continue
			}
			staleSamples++
			if staleSamples >= workerLeaderStaleSamples {
				sort.Strings(bad)
				return "stale sweeps: " + fmt.Sprint(bad)
			}
		}
	}
}
