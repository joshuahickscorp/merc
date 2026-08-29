package main

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

func TestVerificationProcessCapacityPreservesDatabaseHeadroom(t *testing.T) {
	tests := []struct {
		name        string
		maxConns    int32
		leaderConns int32
		wantSlots   int
		wantReserve int32
	}{
		{"default with leader", 20, 1, 5, 5},
		{"minimum with leader", 6, 1, 1, 3},
		{"unsafe with leader", 5, 1, 0, 5},
		{"minimum without leader", 5, 0, 1, 2},
		{"odd spare remains reserved", 9, 0, 2, 3},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			budget := newVerificationResourceBudget(tc.maxConns, tc.leaderConns, 1024)
			if got := budget.processCapacity(); got != tc.wantSlots {
				t.Fatalf("process capacity=%d want=%d", got, tc.wantSlots)
			}
			if budget.reservedDB != tc.wantReserve {
				t.Fatalf("reserved DB=%d want=%d", budget.reservedDB, tc.wantReserve)
			}
			claimedDB := int32(budget.processCapacity()) * verificationDBConnectionsPerProcess
			if claimedDB+budget.reservedDB > tc.maxConns {
				t.Fatalf("capacity overcommits pool: verifier=%d reserve=%d max=%d",
					claimedDB, budget.reservedDB, tc.maxConns)
			}
			if budget.processCapacity() > 0 &&
				budget.reservedDB < tc.leaderConns+verificationDBHeadroom {
				t.Fatalf("reserve=%d cannot cover leader=%d plus headroom=%d",
					budget.reservedDB, tc.leaderConns, verificationDBHeadroom)
			}
		})
	}
}

func TestExecutionDBCapacityFailsClosedAtUnsafeWorkerSizes(t *testing.T) {
	for _, tc := range []struct {
		maxConns int32
		leader   bool
		wantErr  bool
	}{
		{5, true, true},
		{6, true, false},
		{4, false, true},
		{5, false, false},
	} {
		err := validateExecutionDBCapacity(tc.maxConns, tc.leader)
		if (err != nil) != tc.wantErr {
			t.Fatalf("max=%d leader=%t err=%v wantErr=%t",
				tc.maxConns, tc.leader, err, tc.wantErr)
		}
	}
}

func TestVerificationCapacityLeavesLiveConnectionsForAPIAndRenewal(t *testing.T) {
	databaseURL := requireTestDatabase(t)
	cfg, err := pgxpool.ParseConfig(databaseURL)
	must(t, err)
	cfg.MaxConns = 6
	pool, err := pgxpool.NewWithConfig(context.Background(), cfg)
	must(t, err)
	defer pool.Close()
	store := NewStoreWithWorkerLeader(pool)
	if got := store.verificationResources.processCapacity(); got != 1 {
		t.Fatalf("verification slots=%d want=1", got)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	leader, err := pool.Acquire(ctx)
	must(t, err)
	defer leader.Release()
	lockConn, err := pool.Acquire(ctx)
	must(t, err)
	defer lockConn.Release()
	verificationQuery, err := pool.Acquire(ctx)
	must(t, err)
	defer verificationQuery.Release()
	heartbeatConn, err := pool.Acquire(ctx)
	must(t, err)
	defer heartbeatConn.Release()
	apiConn, err := pool.Acquire(ctx)
	mustf(t, err, "API headroom unavailable at peak verification load: %v")
	defer apiConn.Release()
	backgroundConn, err := pool.Acquire(ctx)
	mustf(t, err, "background headroom unavailable at peak verification load: %v")
	defer backgroundConn.Release()

	fullCtx, fullCancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer fullCancel()
	if extra, err := pool.Acquire(fullCtx); err == nil {
		extra.Release()
		t.Fatal("pool unexpectedly exposed capacity beyond the declared six connections")
	} else if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("sixth acquisition error=%v want deadline", err)
	}
}
