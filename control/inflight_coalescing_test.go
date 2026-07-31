package main

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
)

// identityFor builds a well-formed, tenant-scoped request identity for a test.
func identityFor(t *testing.T, tenant uuid.UUID, input string) string {
	t.Helper()
	id, err := RequestIdentity{
		TenantScope: tenant.String(),
		ModelID:     "llama-3.2-1b-instruct-q4",
		Input:       input,
		TopP:        1,
		Policy:      "coalescing-test",
	}.Compute()
	if err != nil {
		t.Fatalf("compute identity: %v", err)
	}
	return id
}

// The headline number: many eligible callers, one physical execution.
//
// 128 concurrent claims against one identity must elect exactly one leader. Not
// "usually one" — the election is a single statement precisely so that a race
// cannot produce two, and two would mean two supplier payables for work the
// buyer is charged for once.
func TestOneLeaderUnderConcurrency(t *testing.T) {
	ctx, store, _ := openIsolatedMoneyPathStore(t)
	tenant := uuid.New()
	identity := identityFor(t, tenant, "one leader")

	const callers = 128
	var leaders, followers int64
	var mu sync.Mutex
	var wg sync.WaitGroup
	start := make(chan struct{})
	for i := 0; i < callers; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			<-start
			role, err := store.ClaimInflightExecution(ctx, identity, tenant, fmt.Sprintf("caller-%d", i))
			mu.Lock()
			defer mu.Unlock()
			if err != nil {
				t.Errorf("claim %d: %v", i, err)
				return
			}
			if role.Leader {
				leaders++
			} else {
				followers++
			}
		}(i)
	}
	close(start)
	wg.Wait()

	if leaders != 1 {
		t.Fatalf("%d leaders elected among %d callers, want exactly 1", leaders, callers)
	}
	if followers != callers-1 {
		t.Fatalf("%d followers, want %d", followers, callers-1)
	}
	counted, err := store.InflightFollowers(ctx, identity)
	if err != nil {
		t.Fatal(err)
	}
	if counted != int64(callers-1) {
		t.Fatalf("the row counted %d followers, want %d", counted, callers-1)
	}
}

// Two tenants issuing byte-identical requests must never meet.
//
// The leak this closes is not the bytes — both tenants would receive the same
// answer either way — it is existence: tenant B learns that tenant A ran this
// exact request, by observing that it came back instantly at the reuse price.
// Invisible to any test that only compares outputs.
func TestIdenticalRequestsInDifferentTenantsNeverCoalesce(t *testing.T) {
	ctx, store, _ := openIsolatedMoneyPathStore(t)
	tenantA, tenantB := uuid.New(), uuid.New()
	const sameInput = "byte for byte the same request"

	identityA := identityFor(t, tenantA, sameInput)
	identityB := identityFor(t, tenantB, sameInput)
	if identityA == identityB {
		t.Fatal("two tenants issuing an identical request share one identity")
	}

	roleA, err := store.ClaimInflightExecution(ctx, identityA, tenantA, "a")
	if err != nil || !roleA.Leader {
		t.Fatalf("tenant A did not lead: %+v %v", roleA, err)
	}
	roleB, err := store.ClaimInflightExecution(ctx, identityB, tenantB, "b")
	if err != nil {
		t.Fatal(err)
	}
	if !roleB.Leader {
		t.Fatal("tenant B followed tenant A's execution across a tenant boundary")
	}

	// And B cannot read A's result even holding A's identity string, which is
	// the case that matters if an identity ever leaks.
	if _, ok, err := store.AwaitInflightResult(ctx, identityA, tenantB); err != nil || ok {
		t.Fatalf("tenant B read tenant A's in-flight row: ok=%v err=%v", ok, err)
	}
}

// A follower giving up must not disturb the leader. This is the property that
// makes collapsing 128 callers safe: the 128th disconnecting cannot cancel work
// the other 127 are waiting on.
func TestFollowerCancellationDoesNotCancelTheLeader(t *testing.T) {
	ctx, store, _ := openIsolatedMoneyPathStore(t)
	tenant := uuid.New()
	identity := identityFor(t, tenant, "follower cancels")

	if role, err := store.ClaimInflightExecution(ctx, identity, tenant, "leader"); err != nil || !role.Leader {
		t.Fatalf("leader claim: %+v %v", role, err)
	}
	if role, err := store.ClaimInflightExecution(ctx, identity, tenant, "follower"); err != nil || role.Leader {
		t.Fatalf("second caller led: %+v %v", role, err)
	}

	followerCtx, cancel := context.WithCancel(ctx)
	done := make(chan error, 1)
	go func() {
		_, _, err := store.AwaitInflightResult(followerCtx, identity, tenant)
		done <- err
	}()
	cancel()
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("a cancelled follower returned a result")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("a cancelled follower did not return")
	}

	// The leader still holds the identity and can still publish.
	if err := store.ResolveInflightSuccess(ctx, identity, "leader",
		"ref/leader", strings.Repeat("a", 64), 12); err != nil {
		t.Fatalf("leader could not publish after a follower cancelled: %v", err)
	}
}

// A crashed leader must not strand the identity forever, and re-election must be
// bounded so one broken request cannot become unbounded compute.
func TestExpiredLeaseIsTakenOverAndReElectionIsBounded(t *testing.T) {
	ctx, store, pool := openIsolatedMoneyPathStore(t)
	tenant := uuid.New()
	identity := identityFor(t, tenant, "leader crashes")

	if role, err := store.ClaimInflightExecution(ctx, identity, tenant, "leader-1"); err != nil || !role.Leader {
		t.Fatalf("first claim: %+v %v", role, err)
	}
	// A live lease is not taken over.
	if role, err := store.ClaimInflightExecution(ctx, identity, tenant, "leader-2"); err != nil || role.Leader {
		t.Fatalf("a live lease was stolen: %+v %v", role, err)
	}

	expire := func() {
		t.Helper()
		if _, err := pool.Exec(ctx,
			`UPDATE inflight_executions SET lease_expires_at = now() - interval '1 second'
			  WHERE request_identity=$1`, identity); err != nil {
			t.Fatal(err)
		}
	}

	// Election 2 and 3 succeed; the fourth attempt is refused and the caller is
	// told to execute alone.
	for election := 2; election <= inflightMaxElections; election++ {
		expire()
		role, err := store.ClaimInflightExecution(ctx, identity, tenant, fmt.Sprintf("leader-%d", election))
		if err != nil {
			t.Fatal(err)
		}
		if !role.Leader || role.Elections != election {
			t.Fatalf("election %d produced %+v", election, role)
		}
	}
	expire()
	role, err := store.ClaimInflightExecution(ctx, identity, tenant, "leader-last")
	if err != nil {
		t.Fatal(err)
	}
	if !role.Ineligible {
		t.Fatalf("re-election was unbounded: %+v", role)
	}
	// Ineligible still means "execute", never "hang".
	if !role.Leader {
		t.Fatal("an exhausted identity refused to let its caller execute at all")
	}
}

// A follower waiting on a leader whose lease expires must be released rather
// than waiting for a result that is never coming.
func TestFollowerIsReleasedWhenTheLeaderStopsReporting(t *testing.T) {
	ctx, store, pool := openIsolatedMoneyPathStore(t)
	tenant := uuid.New()
	identity := identityFor(t, tenant, "leader goes quiet")

	if _, err := store.ClaimInflightExecution(ctx, identity, tenant, "leader"); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx,
		`UPDATE inflight_executions SET lease_expires_at = now() - interval '1 second'
		  WHERE request_identity=$1`, identity); err != nil {
		t.Fatal(err)
	}
	waitCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	result, ok, err := store.AwaitInflightResult(waitCtx, identity, tenant)
	if err != nil {
		t.Fatalf("await: %v", err)
	}
	if ok {
		t.Fatalf("a follower got a result from a leader that stopped reporting: %+v", result)
	}
}

// An explicit leader failure releases followers immediately, and a leader that
// was taken over may not publish afterwards.
func TestPublishIsFencedOnLeadership(t *testing.T) {
	ctx, store, pool := openIsolatedMoneyPathStore(t)
	tenant := uuid.New()
	identity := identityFor(t, tenant, "fencing")

	if _, err := store.ClaimInflightExecution(ctx, identity, tenant, "leader-1"); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx,
		`UPDATE inflight_executions SET lease_expires_at = now() - interval '1 second'
		  WHERE request_identity=$1`, identity); err != nil {
		t.Fatal(err)
	}
	if role, err := store.ClaimInflightExecution(ctx, identity, tenant, "leader-2"); err != nil || !role.Leader {
		t.Fatalf("takeover: %+v %v", role, err)
	}

	// The deposed leader finishing late must not publish: two executions both
	// claiming to be the answer would hand followers whichever landed last.
	err := store.ResolveInflightSuccess(ctx, identity, "leader-1",
		"ref/stale", strings.Repeat("b", 64), 9)
	if err == nil {
		t.Fatal("a deposed leader published its result")
	}
	if !strings.Contains(err.Error(), "not held by") {
		t.Errorf("refusal said %q", err.Error())
	}

	// The current leader can.
	if err := store.ResolveInflightSuccess(ctx, identity, "leader-2",
		"ref/live", strings.Repeat("c", 64), 11); err != nil {
		t.Fatalf("current leader could not publish: %v", err)
	}
	result, ok, err := store.AwaitInflightResult(ctx, identity, tenant)
	if err != nil || !ok {
		t.Fatalf("await after publish: ok=%v err=%v", ok, err)
	}
	if result.ResultRef != "ref/live" {
		t.Fatalf("followers got %q, want the current leader's result", result.ResultRef)
	}
}

// A leader that fails releases its followers with a reason.
func TestLeaderFailureReleasesFollowersPromptly(t *testing.T) {
	ctx, store, _ := openIsolatedMoneyPathStore(t)
	tenant := uuid.New()
	identity := identityFor(t, tenant, "leader fails")

	if _, err := store.ClaimInflightExecution(ctx, identity, tenant, "leader"); err != nil {
		t.Fatal(err)
	}
	if err := store.ResolveInflightFailure(ctx, identity, "leader", "upstream returned 503"); err != nil {
		t.Fatal(err)
	}
	waitCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	result, ok, err := store.AwaitInflightResult(waitCtx, identity, tenant)
	if err != nil {
		t.Fatal(err)
	}
	if ok {
		t.Fatal("a failed execution was delivered as a result")
	}
	if result.State != "FAILED" || !strings.Contains(result.Failure, "503") {
		t.Fatalf("failure not surfaced to the follower: %+v", result)
	}
}

// Expiry is a sweep, not a lease side effect, and it must actually remove rows.
func TestExpiredInflightRowsAreSwept(t *testing.T) {
	ctx, store, pool := openIsolatedMoneyPathStore(t)
	tenant := uuid.New()
	identity := identityFor(t, tenant, "expires")

	if _, err := store.ClaimInflightExecution(ctx, identity, tenant, "leader"); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx,
		`UPDATE inflight_executions SET expires_at = now() - interval '1 second'
		  WHERE request_identity=$1`, identity); err != nil {
		t.Fatal(err)
	}
	removed, err := store.sweepExpiredInflight(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if removed < 1 {
		t.Fatal("the sweep removed nothing")
	}
	var remaining int
	if err := pool.QueryRow(ctx,
		`SELECT count(*) FROM inflight_executions WHERE request_identity=$1`, identity).
		Scan(&remaining); err != nil {
		t.Fatal(err)
	}
	if remaining != 0 {
		t.Fatal("an expired row survived the sweep")
	}
}

// The money shape: one physical execution, N discounted deliveries, one supplier
// payable, positive Merc contribution, and no token inflation.
func TestCoalescedDeliveryMoneyIsConservedAndDiscounted(t *testing.T) {
	const fullPricePer1K = 0.002
	const deliveredTokens = 400
	const followers = 127

	fresh := PriceAccounting(TokenAccounting{
		ClassUncachedInput:   100,
		ClassGeneratedOutput: deliveredTokens,
	}, fullPricePer1K)

	coalesced := SettleReuseHitMoney(deliveredTokens, fullPricePer1K)
	if !coalesced.Conserved() {
		t.Fatalf("buyer != supplier + platform: %+v", coalesced)
	}
	// The whole point: the follower's supplier liability is zero, because the
	// supplier is paid once by the leader for the one execution.
	if coalesced.SupplierLiabilityMicros != 0 {
		t.Fatalf("a coalesced follower minted a second supplier liability: %+v", coalesced)
	}
	// Merc keeps a positive contribution — storage, lookup, delivery and
	// verification are real costs, so a follower must not be free.
	if coalesced.PlatformMicros <= 0 {
		t.Fatalf("coalesced delivery has no Merc contribution: %+v", coalesced)
	}
	// And the buyer pays less than a fresh execution would have cost.
	followerUSD := microsToUSD(coalesced.BuyerDebitMicros)
	if followerUSD >= fresh {
		t.Fatalf("a coalesced follower paid %.9f, not less than a fresh execution's %.9f",
			followerUSD, fresh)
	}

	// No physical-token inflation. 128 callers, one execution: the physical
	// tokens are the leader's alone, however many followers rode along.
	leader := TokenAccounting{
		ClassUncachedInput:   100,
		ClassGeneratedOutput: deliveredTokens,
	}
	fleet := TokenAccounting{}
	for class, tokens := range leader {
		fleet[class] = tokens
	}
	fleet[ClassCoalescedDelivery] = deliveredTokens * followers
	if fleet.PhysicalTokens() != leader.PhysicalTokens() {
		t.Fatalf("physical tokens grew from %d to %d because followers were counted as work",
			leader.PhysicalTokens(), fleet.PhysicalTokens())
	}
	if fleet.DeliveredTokens() <= leader.DeliveredTokens() {
		t.Fatal("delivered tokens did not grow with the followers")
	}
}

// coalesced_delivery must be a registered, non-physical billing class. A class
// the accounting layer does not know is refused at write time, which would make
// every coalesced follower fail to record what it was.
func TestCoalescedDeliveryIsARegisteredLogicalClass(t *testing.T) {
	physical, known := physicalClasses[ClassCoalescedDelivery]
	if !known {
		t.Fatal("coalesced_delivery is not a registered billing class")
	}
	if physical {
		t.Fatal("coalesced_delivery is marked as physical work; the follower ran no model")
	}
}

// An unscoped identity must be refused rather than hashed as the empty string,
// which would produce one shared key for every tenant.
func TestRequestIdentityRefusesAnUnscopedTenant(t *testing.T) {
	_, err := RequestIdentity{
		ModelID: "m", Input: "i", TopP: 1,
	}.Compute()
	if err == nil {
		t.Fatal("an identity with no tenant scope was accepted")
	}
	if !strings.Contains(err.Error(), "tenant scope") {
		t.Errorf("refusal said %q", err.Error())
	}
}

// A renewed lease survives its own TTL, and the leader keeps the right to publish.
//
// The failure this prevents is not a stall. inflightLeaseTTL is 30 seconds and a
// realtime contract has two minutes, so a leader slower than the TTL was taken
// over by the next arrival — and the callers already waiting behind it were NOT
// re-collapsed onto the new leader. AwaitInflightResult returns no-result on
// lease expiry and sends each of them off to execute alone, so one slow execution
// fanned out into one supplier execution per waiting buyer, which is the opposite
// of what coalescing is for.
func TestARenewedLeaseSurvivesItsTTLAndKeepsThePublishRight(t *testing.T) {
	ctx, store, pool := openIsolatedMoneyPathStore(t)
	tenant := uuid.New()
	identity := identityFor(t, tenant, "slow-leader")

	role, err := store.ClaimInflightExecution(ctx, identity, tenant, "leader-1")
	if err != nil {
		t.Fatal(err)
	}
	if !role.Leader {
		t.Fatal("the first caller did not lead")
	}

	// The lease as it would be after the leader has been working longer than the
	// TTL. Set directly rather than slept, because the point is the renewal and
	// not the clock.
	expire := func() {
		if _, err := pool.Exec(ctx, `
			UPDATE inflight_executions SET lease_expires_at = now() - interval '1 second'
			 WHERE request_identity=$1`, identity); err != nil {
			t.Fatal(err)
		}
	}

	// Without a renewal the next arrival takes the row over. This is the
	// behaviour that was shipping.
	expire()
	stolen, err := store.ClaimInflightExecution(ctx, identity, tenant, "leader-2")
	if err != nil {
		t.Fatal(err)
	}
	if !stolen.Leader {
		t.Fatal("an expired lease was not taken over; this test no longer " +
			"reproduces the condition the renewal exists for")
	}
	if err := store.ResolveInflightSuccess(
		ctx, identity, "leader-1", "ref", strings.Repeat("a", 64), 1); err == nil {
		t.Fatal("the original leader could still publish after being taken over")
	}

	// And with one. Same expiry, renewal in between, and the row stays this
	// leader's: the next arrival joins as a follower instead of electing itself.
	fresh := identityFor(t, tenant, "renewed-leader")
	if role, err := store.ClaimInflightExecution(ctx, fresh, tenant, "leader-3"); err != nil {
		t.Fatal(err)
	} else if !role.Leader {
		t.Fatal("the renewed-lease fixture did not lead")
	}
	if _, err := pool.Exec(ctx, `
		UPDATE inflight_executions SET lease_expires_at = now() - interval '1 second'
		 WHERE request_identity=$1`, fresh); err != nil {
		t.Fatal(err)
	}
	if err := store.RenewInflightLease(ctx, fresh, "leader-3"); err != nil {
		t.Fatalf("renew: %v", err)
	}
	joined, err := store.ClaimInflightExecution(ctx, fresh, tenant, "leader-4")
	if err != nil {
		t.Fatal(err)
	}
	if joined.Leader {
		t.Fatal("a renewed lease was still taken over; the renewal did not extend it")
	}
	if err := store.ResolveInflightSuccess(
		ctx, fresh, "leader-3", "ref", strings.Repeat("b", 64), 1); err != nil {
		t.Fatalf("the renewing leader lost its right to publish: %v", err)
	}

	// A renewal by someone who is not the leader must do nothing.
	if err := store.RenewInflightLease(ctx, fresh, "not-the-leader"); err == nil {
		t.Fatal("a non-leader renewed a lease it does not hold")
	}
}
