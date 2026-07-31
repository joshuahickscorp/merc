package main

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

// In-flight coalescing, wired.
//
// The shape the milestone asks for: 128 eligible callers, one physical
// execution, 128 valid delivery authorities, one supplier payable, discounted
// buyer prices, positive Merc contribution, no tenant leak.
//
// The previous implementation was two functions with no caller — an INSERT
// ... ON CONFLICT that incremented a counter, and a DELETE that returned it.
// Nothing could have called them: a follower that waits must know what it is
// waiting for and when to give up, and there was no state, no lease, no result
// and no expiry to tell it. A leader that crashed held the identity forever.
//
// What is deliberately NOT here:
//
//   - Cross-tenant sharing. RequestIdentity is tenant-scoped, so two tenants
//     issuing byte-identical requests never meet. Global sharing is a different
//     product with a privacy argument this does not make.
//   - A second cache. A COMPLETE row is a short-lived hand-off with its own
//     expiry; the exact-result cache is the durable store. Two stores with
//     different eviction rules is how a cache becomes a correctness surface.

// inflightLeaseTTL bounds how long a follower waits on a leader that has stopped
// reporting. Long enough that an ordinary slow request is not stolen, short
// enough that a crashed leader does not strand its followers for a human
// timescale.
const inflightLeaseTTL = 30 * time.Second

// inflightResultTTL is how long a COMPLETE row stays available for followers
// that have not polled yet. Short: this is a hand-off window, not storage.
const inflightResultTTL = 60 * time.Second

// inflightMaxElections bounds re-election. An identity whose leader keeps dying
// is a failing request, and retrying it forever would turn one broken input into
// unbounded compute. After this many attempts the identity fails closed and the
// callers execute independently, which is the correct degradation: slower, still
// correct, never a hang.
const inflightMaxElections = 3

// InflightRole is what a caller learned by asking to execute.
type InflightRole struct {
	// Leader means this caller must execute and then resolve the identity.
	Leader bool
	// Elections is how many leaders this identity has had, including this one.
	Elections int
	// Ineligible means coalescing does not apply — an uncacheable request, or an
	// identity that has exhausted re-election. The caller executes alone and
	// must NOT call Resolve.
	Ineligible bool
}

// InflightResult is what a follower waited for.
type InflightResult struct {
	State        string
	ResultRef    string
	ResultSHA256 string
	Tokens       int64
	Failure      string
}

// ClaimInflightExecution makes this caller the leader for an identity, or tells
// it to follow.
//
// Leadership is taken in one statement so concurrency cannot produce two
// leaders. The ON CONFLICT arm takes leadership only when the existing row is a
// RUNNING one whose lease has expired — a live leader keeps it, and a resolved
// identity is not re-elected at all.
func (s *Store) ClaimInflightExecution(
	ctx context.Context, identity string, tenant uuid.UUID, leaderRef string,
) (InflightRole, error) {
	if !ValidRequestIdentity(identity) || tenant == uuid.Nil || leaderRef == "" {
		// Uncacheable, unscoped, or unidentified: execute alone rather than
		// coalescing on a key that does not mean what it should.
		return InflightRole{Leader: true, Ineligible: true}, nil
	}

	var leader bool
	var elections int
	err := s.pool.QueryRow(ctx, `
		INSERT INTO inflight_executions
		  (request_identity, tenant_scope, leader_ref, state,
		   lease_expires_at, elections, expires_at)
		VALUES ($1,$2,$3,'RUNNING',
		        now() + make_interval(secs => $4), 1,
		        now() + make_interval(secs => $5))
		ON CONFLICT (request_identity) DO UPDATE
		   SET leader_ref       = EXCLUDED.leader_ref,
		       state            = 'RUNNING',
		       lease_expires_at = EXCLUDED.lease_expires_at,
		       elections        = inflight_executions.elections + 1,
		       result_ref       = NULL,
		       result_sha256    = NULL,
		       result_tokens    = NULL,
		       failure          = NULL,
		       resolved_at      = NULL,
		       started_at       = now(),
		       expires_at       = EXCLUDED.expires_at
		 WHERE inflight_executions.state = 'RUNNING'
		   AND inflight_executions.lease_expires_at <= now()
		   AND inflight_executions.elections < $6
		   -- Tenant equality is asserted, not assumed. The identity already
		   -- encodes the tenant, so a mismatch here means the hash was built
		   -- wrong, and taking over anyway would hand one tenant another's
		   -- execution.
		   AND inflight_executions.tenant_scope = EXCLUDED.tenant_scope
		RETURNING true, elections`,
		identity, tenant, leaderRef,
		inflightLeaseTTL.Seconds(), inflightResultTTL.Seconds(), inflightMaxElections,
	).Scan(&leader, &elections)

	if errors.Is(err, pgx.ErrNoRows) {
		// A row exists and this caller did not take it: either a live leader is
		// running, or the identity is already resolved, or re-election is
		// exhausted. Followers count themselves so the saving is measurable.
		var state string
		var exhausted bool
		if err := s.pool.QueryRow(ctx, `
			UPDATE inflight_executions
			   SET followers = followers + 1
			 WHERE request_identity = $1 AND tenant_scope = $2
			RETURNING state, elections >= $3`,
			identity, tenant, inflightMaxElections).Scan(&state, &exhausted); err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				// Raced with expiry between the insert and here. Execute alone.
				return InflightRole{Leader: true, Ineligible: true}, nil
			}
			return InflightRole{}, err
		}
		if exhausted && state != "COMPLETE" {
			return InflightRole{Leader: true, Ineligible: true}, nil
		}
		return InflightRole{Leader: false}, nil
	}
	if err != nil {
		return InflightRole{}, err
	}
	return InflightRole{Leader: true, Elections: elections}, nil
}

// RenewInflightLease extends a leader's claim. A leader running longer than the
// TTL must renew or be taken over; without this, any request slower than the TTL
// would be executed twice by design.
func (s *Store) RenewInflightLease(ctx context.Context, identity, leaderRef string) error {
	ct, err := s.pool.Exec(ctx, `
		UPDATE inflight_executions
		   SET lease_expires_at = now() + make_interval(secs => $3)
		 WHERE request_identity = $1 AND leader_ref = $2 AND state = 'RUNNING'`,
		identity, leaderRef, inflightLeaseTTL.Seconds())
	if err != nil {
		return err
	}
	if ct.RowsAffected() == 0 {
		return fmt.Errorf("inflight lease for %s is no longer held by %s", identity, leaderRef)
	}
	return nil
}

// ResolveInflightSuccess publishes the leader's result to its followers.
//
// Fenced on leader_ref: a leader that was taken over while it was still running
// must not publish, or two executions would both claim to be the answer and the
// followers would get whichever landed last.
func (s *Store) ResolveInflightSuccess(
	ctx context.Context, identity, leaderRef, resultRef, resultSHA256 string, tokens int64,
) error {
	ct, err := s.pool.Exec(ctx, `
		UPDATE inflight_executions
		   SET state = 'COMPLETE', result_ref = $3, result_sha256 = $4,
		       result_tokens = $5, resolved_at = now(),
		       expires_at = now() + make_interval(secs => $6)
		 WHERE request_identity = $1 AND leader_ref = $2 AND state = 'RUNNING'`,
		identity, leaderRef, resultRef, resultSHA256, tokens, inflightResultTTL.Seconds())
	if err != nil {
		return err
	}
	if ct.RowsAffected() == 0 {
		return fmt.Errorf("inflight %s is not held by %s; result not published", identity, leaderRef)
	}
	return nil
}

// ResolveInflightFailure releases followers immediately rather than making them
// wait out the lease for an answer that is never coming.
func (s *Store) ResolveInflightFailure(ctx context.Context, identity, leaderRef, failure string) error {
	if failure == "" {
		failure = "leader failed without a reported reason"
	}
	_, err := s.pool.Exec(ctx, `
		UPDATE inflight_executions
		   SET state = 'FAILED', failure = $3, resolved_at = now(),
		       expires_at = now() + make_interval(secs => $4)
		 WHERE request_identity = $1 AND leader_ref = $2 AND state = 'RUNNING'`,
		identity, leaderRef, failure, inflightResultTTL.Seconds())
	return err
}

// AwaitInflightResult polls until the leader resolves, the lease expires, or the
// caller's context ends.
//
// A follower's context ending cancels only the follower. The leader's execution
// is not tied to any follower's request — that is the property that makes 128
// callers safe to collapse into one run, and losing it would mean the last
// caller to disconnect could cancel work 127 others are waiting on.
func (s *Store) AwaitInflightResult(
	ctx context.Context, identity string, tenant uuid.UUID,
) (InflightResult, bool, error) {
	const poll = 25 * time.Millisecond
	for {
		var out InflightResult
		var leaseExpired bool
		err := s.pool.QueryRow(ctx, `
			SELECT state, COALESCE(result_ref,''), COALESCE(result_sha256,''),
			       COALESCE(result_tokens,0), COALESCE(failure,''),
			       lease_expires_at <= now()
			  FROM inflight_executions
			 WHERE request_identity = $1 AND tenant_scope = $2`,
			identity, tenant).
			Scan(&out.State, &out.ResultRef, &out.ResultSHA256, &out.Tokens,
				&out.Failure, &leaseExpired)
		if errors.Is(err, pgx.ErrNoRows) {
			return InflightResult{}, false, nil // expired out from under us
		}
		if err != nil {
			return InflightResult{}, false, err
		}
		switch out.State {
		case "COMPLETE":
			return out, true, nil
		case "FAILED":
			return out, false, nil
		}
		if leaseExpired {
			// The leader stopped reporting. Returning "no result" sends this
			// caller back to ClaimInflightExecution, where it either takes over
			// or is told to execute alone.
			return InflightResult{}, false, nil
		}
		select {
		case <-ctx.Done():
			return InflightResult{}, false, ctx.Err()
		case <-time.After(poll):
		}
	}
}

// InflightFollowers reports how many callers waited on one identity.
//
// NOT read by the overhead recorder, whatever this comment claimed for as long as
// it existed: a caller census found its only callers are tests. COALESCING_AVOIDED
// overhead is not recorded from it, so the saving coalescing produces is real and
// unmeasured. Recording it needs a writer, not a corrected comment; the comment is
// corrected here so the next reader does not go looking for one.
func (s *Store) InflightFollowers(ctx context.Context, identity string) (int64, error) {
	var followers int64
	err := s.pool.QueryRow(ctx,
		`SELECT followers FROM inflight_executions WHERE request_identity=$1`, identity).
		Scan(&followers)
	if errors.Is(err, pgx.ErrNoRows) {
		return 0, nil
	}
	return followers, err
}

// inflightTickerName registers the sweep ADVISORY, for the reason the sweep's own
// comment gives: a backlog costs a cache miss and a duplicated execution, never a
// wrong answer or a wrong charge. A hard liveness failure on an analytics-grade
// cleanup would page someone for a table that is merely growing.
const inflightTickerName = "inflight-sweep"

// sweepInflightExecutions is the ticker adapter.
//
// The ticker table takes func(context.Context) error and the sweep returns the
// row count as well, so an adapter is unavoidable. The count is discarded rather
// than logged: a DELETE that removes nothing is the steady state, and logging it
// every interval would bury the case that matters.
func (wk *Workers) sweepInflightExecutions(ctx context.Context) error {
	_, err := wk.store.sweepExpiredInflight(ctx)
	return err
}

// sweepExpiredInflight removes rows past their expiry.
//
// Non-authoritative, like the overhead ticker: a backlog here costs a cache miss
// and a duplicated execution, never a wrong answer or a wrong charge.
func (s *Store) sweepExpiredInflight(ctx context.Context) (int64, error) {
	ct, err := s.pool.Exec(ctx,
		`DELETE FROM inflight_executions WHERE expires_at <= now()`)
	if err != nil {
		return 0, err
	}
	return ct.RowsAffected(), nil
}
