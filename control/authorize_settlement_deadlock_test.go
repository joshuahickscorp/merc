package main

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgconn"
)

// TestAuthorizeSettlementDeadlockRepro races single-buyer AuthorizeRealtimeContract
// against FinalizeRealtimeSuccess under concurrency. This is the production shape
// that hit 40P01 after the late funding-lock reorder (a8159ac7): authorize claimed
// the offer row before buyer funding, while settlement locks the buyer (and
// contract) before releasing offer capacity.
//
// On a broken tree this fails with 40P01 (set MERC_EXPECT_DEADLOCK=1 to require
// at least one). On the fixed hierarchy it must pass with zero deadlocks.
func TestAuthorizeSettlementDeadlockRepro(t *testing.T) {
	installSettlementCurrencyForTest(t, "usd")
	ctx, store, pool := openIsolatedTestStore(t)
	profile, _, workerID := realtimeFundingFixture(t, ctx, store, pool)

	// Faster deadlock detection so the repro is deterministic within test budget.
	// ALTER DATABASE + pool.Reset so every Store connection inherits 50ms.
	var dbName string
	if err := pool.QueryRow(ctx, `SELECT current_database()`).Scan(&dbName); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, fmt.Sprintf(`ALTER DATABASE %s SET deadlock_timeout = '50ms'`, quoteIdent(dbName))); err != nil {
		t.Fatal(err)
	}
	pool.Reset()

	// Map relation OIDs so a captured 40P01 Detail line can name the tuple lock.
	rows, err := pool.Query(ctx, `
		SELECT c.oid::bigint, c.relname
		  FROM pg_class c JOIN pg_namespace n ON n.oid=c.relnamespace
		 WHERE n.nspname='public' AND c.relkind='r'
		   AND c.relname IN ('buyers','realtime_worker_offers','execution_contracts',
		                     'ledger_entries','buyer_prepaid_balances')
		 ORDER BY c.relname`)
	if err != nil {
		t.Fatal(err)
	}
	for rows.Next() {
		var oid int64
		var name string
		if err := rows.Scan(&oid, &name); err != nil {
			rows.Close()
			t.Fatal(err)
		}
		t.Logf("relation oid %d = %s", oid, name)
	}
	rows.Close()

	buyerID, err := store.CreateBuyerAccount(ctx,
		"deadlock-repro-"+uuid.NewString()+"@example.test", "integration-password", 50_000)
	if err != nil {
		t.Fatal(err)
	}

	// Ample capacity so contention is on lock order, not no-supply.
	if _, err := pool.Exec(ctx, `
		UPDATE realtime_worker_offers
		   SET max_active_sequences=10_000, available_sequences=10_000, status='ACTIVE', last_seen_at=now()
		 WHERE worker_id=$1 AND runtime_profile_id=$2`,
		workerID, profile.RuntimeProfileID); err != nil {
		t.Fatal(err)
	}
	// Keep last_seen inside the 45s eligibility window across the race.
	refresh := time.NewTicker(5 * time.Second)
	t.Cleanup(refresh.Stop)
	go func() {
		for range refresh.C {
			c, cancel := context.WithTimeout(context.Background(), 2*time.Second)
			_, _ = pool.Exec(c, `
				UPDATE realtime_worker_offers
				   SET last_seen_at=now(), available_sequences=GREATEST(available_sequences, 100),
				       status='ACTIVE'
				 WHERE worker_id=$1 AND runtime_profile_id=$2`,
				workerID, profile.RuntimeProfileID)
			cancel()
		}
	}()

	maxUSD, estUSD, maxPrompt, maxCompletion := realtimeAuthCeiling(t, profile, 7, 2)
	const (
		concurrency = 16
		rounds      = 40
	)

	var (
		deadlocks atomic.Int64
		authOK    atomic.Int64
		settleOK  atomic.Int64
		otherErr  atomic.Int64
		firstDL   atomic.Value // string
		wg        sync.WaitGroup
	)
	start := make(chan struct{})

	authOnce := func() {
		c, _, err := store.AuthorizeRealtimeContract(context.Background(), RealtimeContractAuthorization{
			RequestID: "dl-auth-" + uuid.NewString(), BuyerID: buyerID, Profile: profile,
			InputCommitment: strings.Repeat("a", 64), RequestSHA256: strings.Repeat("b", 64),
			MaximumPriceUSD: maxUSD, EstimatedPriceUSD: estUSD, DeadlineAt: time.Now().Add(time.Minute),
			MaximumPromptTokens: maxPrompt, MaximumCompletionTokens: maxCompletion,
			EstimatedPromptTokens: 7, EstimatedCompletionTokens: 2,
			BuyerDeclaredCeilingUSD: maxUSD + 0.0001,
		})
		if err != nil {
			if isPostgresDeadlock(err) {
				deadlocks.Add(1)
				firstDL.CompareAndSwap(nil, formatDeadlockError("authorize", err))
				return
			}
			// Capacity races after aborted settles are noise for this repro.
			if !errors.Is(err, errRealtimeNoSupply) {
				otherErr.Add(1)
				t.Logf("authorize err: %v", err)
			} else {
				otherErr.Add(1)
			}
			return
		}
		authOK.Add(1)
		// Immediately settle — the other half of the lock-order cycle.
		_, serr := store.FinalizeRealtimeSuccess(context.Background(), c.ID, RealtimeExecutionEvidence{
			ID: uuid.New(), HTTPStatus: http.StatusOK,
			StreamRootSHA256: strings.Repeat("1", 64), OutputCommitment: strings.Repeat("2", 64),
			PromptTokens: 7, CompletionTokens: 2, TotalTokens: 9,
		})
		if serr != nil {
			if isPostgresDeadlock(serr) {
				deadlocks.Add(1)
				firstDL.CompareAndSwap(nil, formatDeadlockError("settle", serr))
				// Still try to free capacity so later rounds are not starved.
				_, _ = store.FinalizeRealtimeFailure(context.Background(), c.ID, uuid.New(), 500, 1,
					"deadlock_repro", "fallback after settle deadlock", false)
				return
			}
			// Already finalized races are fine under retry noise.
			if !errors.Is(serr, errRealtimeAlreadyFinalized) {
				otherErr.Add(1)
				t.Logf("settle err: %v", serr)
			}
			return
		}
		settleOK.Add(1)
	}

	for i := 0; i < concurrency; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			for r := 0; r < rounds; r++ {
				authOnce()
			}
		}()
	}
	close(start)
	wg.Wait()

	t.Logf("authOK=%d settleOK=%d deadlocks=%d otherErr=%d",
		authOK.Load(), settleOK.Load(), deadlocks.Load(), otherErr.Load())
	if v := firstDL.Load(); v != nil {
		t.Logf("first deadlock error (verbatim):\n%s", v.(string))
		// Also pull server-side DETAIL if log_line_prefix exposes it via a
		// recent auto_explain / log capture is not available — surface what we
		// have from the client error which includes Process wait graph.
	}
	if deadlocks.Load() == 0 {
		// On a fixed tree this is the pass condition. On the broken tree the
		// test must fail — caller's job is to prove broken → fixed.
		if os.Getenv("MERC_EXPECT_DEADLOCK") == "1" {
			t.Fatalf("expected at least one 40P01 under single-buyer c=%d; got none (authOK=%d settleOK=%d)",
				concurrency, authOK.Load(), settleOK.Load())
		}
		return
	}
	t.Fatalf("observed %d PostgreSQL deadlocks (40P01) under single-buyer authorize+settle concurrency=%d; first:\n%s",
		deadlocks.Load(), concurrency, firstDL.Load())
}

// TestAuthorizeOnlyNoSettlementDeadlock checks whether pure concurrent authorize
// (no settle) alone is enough to hit 40P01. Diagnosis expects it is NOT — the
// cycle needs the opposite-order settlement path.
func TestAuthorizeOnlyNoSettlementDeadlock(t *testing.T) {
	installSettlementCurrencyForTest(t, "usd")
	ctx, store, pool := openIsolatedTestStore(t)
	profile, _, workerID := realtimeFundingFixture(t, ctx, store, pool)

	var dbName string
	if err := pool.QueryRow(ctx, `SELECT current_database()`).Scan(&dbName); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, fmt.Sprintf(`ALTER DATABASE %s SET deadlock_timeout = '50ms'`, quoteIdent(dbName))); err != nil {
		t.Fatal(err)
	}
	pool.Reset()

	buyerID, err := store.CreateBuyerAccount(ctx,
		"deadlock-authonly-"+uuid.NewString()+"@example.test", "integration-password", 50_000)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `
		UPDATE realtime_worker_offers
		   SET max_active_sequences=10_000, available_sequences=10_000, status='ACTIVE', last_seen_at=now()
		 WHERE worker_id=$1 AND runtime_profile_id=$2`,
		workerID, profile.RuntimeProfileID); err != nil {
		t.Fatal(err)
	}
	maxUSD, estUSD, maxPrompt, maxCompletion := realtimeAuthCeiling(t, profile, 7, 2)

	var deadlocks atomic.Int64
	var wg sync.WaitGroup
	start := make(chan struct{})
	const concurrency = 16
	const rounds = 30
	for i := 0; i < concurrency; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			for r := 0; r < rounds; r++ {
				c, _, err := store.AuthorizeRealtimeContract(context.Background(), RealtimeContractAuthorization{
					RequestID: "dl-only-" + uuid.NewString(), BuyerID: buyerID, Profile: profile,
					InputCommitment: strings.Repeat("a", 64), RequestSHA256: strings.Repeat("b", 64),
					MaximumPriceUSD: maxUSD, EstimatedPriceUSD: estUSD, DeadlineAt: time.Now().Add(time.Minute),
					MaximumPromptTokens: maxPrompt, MaximumCompletionTokens: maxCompletion,
					EstimatedPromptTokens: 7, EstimatedCompletionTokens: 2,
					BuyerDeclaredCeilingUSD: maxUSD + 0.0001,
				})
				if err != nil {
					if isPostgresDeadlock(err) {
						deadlocks.Add(1)
					}
					continue
				}
				// Void without going through success settle — same as latency probe teardown.
				_, _ = store.FinalizeRealtimeFailure(context.Background(), c.ID, uuid.New(), 500, 1,
					"auth_only_probe", "teardown", false)
			}
		}()
	}
	close(start)
	wg.Wait()
	if deadlocks.Load() > 0 {
		t.Fatalf("pure authorize+failure observed %d deadlocks; cycle may not require success settle", deadlocks.Load())
	}
	t.Logf("authorize-only+failure: 0 deadlocks (as expected if cycle needs success settle buyer FOR UPDATE)")
}

func isPostgresDeadlock(err error) bool {
	if err == nil {
		return false
	}
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) && pgErr.Code == "40P01" {
		return true
	}
	// Some paths wrap without preserving the code on the outer error.
	msg := err.Error()
	return strings.Contains(msg, "40P01") || strings.Contains(msg, "deadlock detected")
}

// formatDeadlockError expands the Postgres 40P01 payload the client receives.
// Postgres puts the wait graph in Detail; the statement that lost is in
// Message/Where. Server logs mirror this when log_destination includes stderr.
func formatDeadlockError(path string, err error) string {
	var pgErr *pgconn.PgError
	if !errors.As(err, &pgErr) {
		return fmt.Sprintf("path=%s err=%v", path, err)
	}
	return fmt.Sprintf(
		"path=%s\nSeverity=%s\nCode=%s\nMessage=%s\nDetail=%s\nHint=%s\nWhere=%s\nSchema=%s\nTable=%s\nColumn=%s\nDataType=%s\nConstraint=%s\nFile=%s\nLine=%d\nRoutine=%s\nFullError=%v",
		path, pgErr.Severity, pgErr.Code, pgErr.Message, pgErr.Detail, pgErr.Hint, pgErr.Where,
		pgErr.SchemaName, pgErr.TableName, pgErr.ColumnName, pgErr.DataTypeName, pgErr.ConstraintName,
		pgErr.File, pgErr.Line, pgErr.Routine, err,
	)
}

func quoteIdent(name string) string {
	return `"` + strings.ReplaceAll(name, `"`, `""`) + `"`
}
