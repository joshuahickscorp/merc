package main

import (
	"context"
	"errors"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
)

// TestOperatorResolveUnresolvableDisputeUnblocksPayout is the exit for defect 1:
// an unresolvable dispute permanently froze supplier money because no route could
// leave that status. Operator resolve records admin_actions and, on rejected,
// makes the previously blocked credit claimable again.
func TestOperatorResolveUnresolvableDisputeUnblocksPayout(t *testing.T) {
	ctx, store, pool := openIsolatedTestStore(t)
	t.Setenv("MERC_CANARY_MODE", "false")
	t.Setenv("MERC_CANARY_DISABLE_DECISION_REF", "test:operator-dispute-resolve")
	f := seedDisputePayoutFixture(t, ctx, pool, "complete")
	actor := testAdminActor(uuid.New())
	if _, err := pool.Exec(ctx, `
		INSERT INTO api_keys (id,key_hash,is_admin,revoked,name)
		VALUES ($1,$2,true,false,$3)`,
		actor.PrincipalID, "admin-test-"+actor.PrincipalID.String(), actor.Label); err != nil {
		t.Fatalf("seed operator key: %v", err)
	}

	disputeID, err := store.RecordDispute(ctx, f.jobID, f.buyerID, "output does not match the submitted input")
	mustf(t, err, "file dispute: %v")
	mustf(t, store.SetDisputeStatus(ctx, disputeID, "unresolvable"), "park as unresolvable: %v")
	due, err := store.DuePayouts(ctx, 100)
	if err != nil || dueContains(due, f.entryID) {
		t.Fatalf("unresolvable must still freeze payout: present=%v err=%v", dueContains(due, f.entryID), err)
	}

	// Failing-before evidence: setActiveDisputeStatus refuses any transition out
	// of unresolvable (the permanent park before the operator route existed).
	if err := store.SetDisputeStatus(ctx, disputeID, "no_peer"); err == nil {
		t.Fatal("expected transition out of unresolvable via SetDisputeStatus to fail")
	} else {
		t.Logf("failing-before evidence (setActiveDisputeStatus refuses exit): %s", err.Error())
	}

	corr := "dispute-resolve-" + uuid.NewString()
	if err := store.ResolveDisputeTx(ctx, actor, disputeID, "rejected",
		"independent review of artifacts; original result stands", corr); err != nil {
		t.Fatalf("operator resolve: %v", err)
	}

	var status string
	must(t, pool.QueryRow(ctx, `SELECT status FROM disputes WHERE id=$1`, disputeID).Scan(&status))
	if status != "rejected" {
		t.Fatalf("status=%q want rejected", status)
	}

	var actions int
	var moneyEffect string
	if err := pool.QueryRow(ctx, `
		SELECT count(*)::int, COALESCE(max(detail->'after'->>'money_effect'),'')
		  FROM admin_actions
		 WHERE kind='dispute_resolved' AND target_id=$1 AND correlation_ref=$2`,
		disputeID, corr).Scan(&actions, &moneyEffect); err != nil {
		t.Fatalf("admin_actions evidence: %v", err)
	}
	if actions != 1 {
		t.Fatalf("admin_actions rows=%d want 1", actions)
	}
	if moneyEffect != "held_supplier_credits_eligible_again" {
		t.Fatalf("money_effect=%q", moneyEffect)
	}

	// Claimable means DuePayouts no longer filters the credit for an active
	// dispute (same signal TestDisputeFilingAtomicallyFreezes... uses). ClaimPayout
	// may still return false without buyer_cash funding; that is a separate gate.
	due, err = store.DuePayouts(ctx, 100)
	if err != nil || !dueContains(due, f.entryID) {
		t.Fatalf("rejected operator resolve must re-enable held payout: present=%v err=%v", dueContains(due, f.entryID), err)
	}
	// ClaimPayout must not still observe an active dispute freeze.
	if _, claimed, err := store.ClaimPayout(ctx, f.entryID); err != nil {
		t.Fatalf("claim after operator reject: err=%v", err)
	} else if claimed {
		// Funding was present — cash path advanced. Fine.
	} else {
		var status string
		must(t, pool.QueryRow(ctx, `SELECT payout_status FROM ledger_entries WHERE id=$1`, f.entryID).Scan(&status))
		// awaiting_funding is the no-collection outcome; held would mean dispute still blocked.
		if status == PayoutHeld {
			// DuePayouts included it and Claim saw no dispute — re-read dispute block.
			var disputed bool
			if err := pool.QueryRow(ctx, `
				SELECT EXISTS(SELECT 1 FROM disputes
				 WHERE job_id=$1 AND status IN ('open','no_peer','reverifying','unresolvable'))`,
				f.jobID).Scan(&disputed); err != nil {
				t.Fatal(err)
			}
			if disputed {
				t.Fatal("active dispute still freezes payout after operator reject")
			}
		}
	}

	// Buyer-visible outcome on the job events timeline.
	var jobEvents int
	if err := pool.QueryRow(ctx, `
		SELECT count(*) FROM job_events
		 WHERE job_id=$1 AND event='dispute_rejected'`, f.jobID).Scan(&jobEvents); err != nil {
		t.Fatal(err)
	}
	if jobEvents < 1 {
		t.Fatal("buyer job timeline missing dispute_rejected event")
	}

	// Fail closed: open disputes are not on the operator queue.
	f2 := seedDisputePayoutFixture(t, ctx, pool, "complete")
	openID, err := store.RecordDispute(ctx, f2.jobID, f2.buyerID, "still in automatic re-verify path")
	mustf(t, err, "file open dispute: %v")
	if err := store.ResolveDisputeTx(ctx, actor, openID, "rejected", "premature", corr+"-open"); !errors.Is(err, errDisputeNotOperatorQueue) {
		t.Fatalf("open dispute operator resolve error = %v, want %v", err, errDisputeNotOperatorQueue)
	}
}

func TestNoPeerDisputePromotesToOperatorQueueAfterBound(t *testing.T) {
	ctx, store, pool := openIsolatedTestStore(t)
	t.Setenv("MERC_CANARY_MODE", "false")
	t.Setenv("MERC_CANARY_DISABLE_DECISION_REF", "test:no-peer-bound")
	f := seedDisputePayoutFixture(t, ctx, pool, "complete")

	disputeID, err := store.RecordDispute(ctx, f.jobID, f.buyerID, "need independent re-verify")
	mustf(t, err, "file dispute: %v")

	// Drive the attempt bound: each call is one failed peer search.
	var last string
	for i := 0; i < noPeerDisputeMaxAttempts; i++ {
		last, err = store.NoteDisputeNoPeer(ctx, disputeID)
		mustf(t, err, "note no_peer attempt %d: %v", i+1)
		if i+1 < noPeerDisputeMaxAttempts && last != "no_peer" {
			t.Fatalf("attempt %d status=%q want no_peer", i+1, last)
		}
	}
	if last != "unresolvable" {
		t.Fatalf("after %d attempts status=%q want unresolvable", noPeerDisputeMaxAttempts, last)
	}

	var attempts int
	var status string
	if err := pool.QueryRow(ctx, `
		SELECT status, no_peer_attempts FROM disputes WHERE id=$1`, disputeID).Scan(&status, &attempts); err != nil {
		t.Fatal(err)
	}
	if status != "unresolvable" || attempts != noPeerDisputeMaxAttempts {
		t.Fatalf("status=%q attempts=%d want unresolvable/%d", status, attempts, noPeerDisputeMaxAttempts)
	}

	// ActiveDisputes must no longer return it (operator queue, not sweep).
	active, err := store.ActiveDisputes(ctx, 100)
	must(t, err)
	for _, d := range active {
		if d.ID == disputeID {
			t.Fatalf("unresolvable dispute %s still in ActiveDisputes sweep set", disputeID)
		}
	}

	// Evidence on dispute_events includes the attempt count and reason.
	var detailReason string
	var detailAttempts int
	if err := pool.QueryRow(ctx, `
		SELECT detail->>'reason', (detail->>'no_peer_attempts')::int
		  FROM dispute_events
		 WHERE dispute_id=$1 AND event='unresolvable'
		 ORDER BY id DESC LIMIT 1`, disputeID).Scan(&detailReason, &detailAttempts); err != nil {
		t.Fatalf("unresolvable event: %v", err)
	}
	if detailReason != "no_peer_attempt_bound_exhausted" || detailAttempts != noPeerDisputeMaxAttempts {
		t.Fatalf("event reason=%q attempts=%d", detailReason, detailAttempts)
	}

	// Age bound: first_no_peer_at in the past promotes on the next note even
	// with a low attempt count.
	f2 := seedDisputePayoutFixture(t, ctx, pool, "complete")
	d2, err := store.RecordDispute(ctx, f2.jobID, f2.buyerID, "age-bound peer starvation")
	mustf(t, err, "file age-bound dispute: %v")
	if _, err := store.NoteDisputeNoPeer(ctx, d2); err != nil {
		t.Fatalf("first no_peer note: %v", err)
	}
	if _, err := pool.Exec(ctx, `
		UPDATE disputes
		   SET first_no_peer_at=now()-($2 * interval '1 second'),
		       no_peer_attempts=1
		 WHERE id=$1`, d2, int(noPeerDisputeMaxAge.Seconds())+60); err != nil {
		t.Fatal(err)
	}
	status2, err := store.NoteDisputeNoPeer(ctx, d2)
	mustf(t, err, "age-bound note: %v")
	if status2 != "unresolvable" {
		t.Fatalf("age-bound status=%q want unresolvable", status2)
	}
	var ageReason string
	if err := pool.QueryRow(ctx, `
		SELECT detail->>'reason' FROM dispute_events
		 WHERE dispute_id=$1 AND event='unresolvable'
		 ORDER BY id DESC LIMIT 1`, d2).Scan(&ageReason); err != nil {
		t.Fatal(err)
	}
	if ageReason != "no_peer_age_bound_exhausted" {
		t.Fatalf("age reason=%q", ageReason)
	}
}

// Ensure the helper types from the dispute payout suite stay linked.
var (
	_ = context.Background
	_ = pgxpool.Pool{}
)
