package main

import (
	"context"
	"errors"
	"math"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// TestRealtimeClearingIdenticalDecisionLegacyVsStats is the single most
// important test for the reputation critical-section extraction: given the
// same fixture, the old full-aggregate CTE and the new stats-table path must
// pick the same worker and produce the same verified_outcome_cost to the
// exact Postgres numeric text.
func TestRealtimeClearingIdenticalDecisionLegacyVsStats(t *testing.T) {
	installSettlementCurrencyForTest(t, "usd")
	ctx, store, pool := openPayoutTestStore(t)
	t.Setenv("MERC_TOKEN_KEY", "outcome-stats-parity-key-at-least-32b!!")
	resetRealtimeClearingState(t, ctx, pool)

	buyerID, err := store.CreateBuyerAccount(ctx,
		"outcome-stats-parity-"+uuid.NewString()+"@example.test", "integration-password", 5)
	must(t, err)
	profile := sortedVLLMProfiles()[0]

	// Cheap unreliable HOT: 50% failure → verified cost doubles past the
	// reliable higher ask. Same shape as the existing cost-rank integration
	// test, so a ranking regression is visible here too.
	reliable := newRealtimeClearingOffer(t, ctx, store, pool, profile, "COLD", 0.08, 0.30, 4)
	cheapUnreliable := newRealtimeClearingOffer(t, ctx, store, pool, profile, "HOT", 0.05, 0.20, 16)
	_ = reliable
	seedSupplierFailureRate(t, ctx, store, pool, buyerID, cheapUnreliable, profile, 5, 5)

	if _, err := pool.Exec(ctx, `
		UPDATE realtime_worker_offers
		   SET status='ACTIVE', available_sequences=max_active_sequences, last_seen_at=now()
		 WHERE runtime_profile_id=$1`, profile.RuntimeProfileID); err != nil {
		t.Fatal(err)
	}

	legacy, err := probeRealtimeClearingWinnerLegacy(ctx, pool,
		profile.RuntimeProfileID, profile.ProfileSHA256,
		profile.BuyerInputUSDPerMillionTokens, profile.BuyerOutputUSDPerMillionTokens)
	mustf(t, err, "legacy probe: %v")
	stats, err := probeRealtimeClearingWinnerStats(ctx, pool,
		profile.RuntimeProfileID, profile.ProfileSHA256,
		profile.BuyerInputUSDPerMillionTokens, profile.BuyerOutputUSDPerMillionTokens)
	mustf(t, err, "stats probe: %v")
	if err := assertRealtimeClearingProbesMatch(legacy, stats); err != nil {
		t.Fatalf("clearing decision diverged: %v\nlegacy=%+v\nstats=%+v", err, legacy, stats)
	}
	if legacy.WorkerID != reliable.WorkerID {
		t.Fatalf("expected reliable winner for the fixture, got %s (reliable=%s cheap=%s)",
			legacy.WorkerID, reliable.WorkerID, cheapUnreliable.WorkerID)
	}
	// Sanity: cost adjustment actually applied for the cheap supplier's history
	// would make it lose; the winner's cost text must be a finite number.
	if legacy.VerifiedOutcomeCost == "" || strings.Contains(legacy.VerifiedOutcomeCost, "e+") {
		// 1e12 ranks last; winner should be the unadjusted reliable ask.
		t.Fatalf("winner cost looks like a last-rank sentinel: %q", legacy.VerifiedOutcomeCost)
	}
}

// TestRealtimeSupplierOutcomeStatsNoDriftAfterTerminalPaths proves the
// incremental counters still equal a fresh full aggregate after the real
// authorize / finalize / (optional) refund writers have run. A silent drift
// here would be worse than the slow query.
func TestRealtimeSupplierOutcomeStatsNoDriftAfterTerminalPaths(t *testing.T) {
	installSettlementCurrencyForTest(t, "usd")
	ctx, store, pool := openPayoutTestStore(t)
	t.Setenv("MERC_TOKEN_KEY", "outcome-stats-drift-key-at-least-32-bytes!")
	resetRealtimeClearingState(t, ctx, pool)

	buyerID, err := store.CreateBuyerAccount(ctx,
		"outcome-stats-drift-"+uuid.NewString()+"@example.test", "integration-password", 5)
	must(t, err)
	profile := sortedVLLMProfiles()[0]
	worker := newRealtimeClearingOffer(t, ctx, store, pool, profile, "WARM", 0.08, 0.30, 32)

	// Mix of verified and failed terminals through the real writers.
	seedSupplierFailureRate(t, ctx, store, pool, buyerID, worker, profile, 7, 3)

	// One cancelled terminal must NOT count (historical aggregate excludes it).
	if _, err := pool.Exec(ctx, `
		UPDATE realtime_worker_offers
		   SET available_sequences=max_active_sequences, status='ACTIVE', last_seen_at=now()
		 WHERE worker_id=$1 AND runtime_profile_id=$2`,
		worker.WorkerID, profile.RuntimeProfileID); err != nil {
		t.Fatal(err)
	}
	cancelled, _, err := store.AuthorizeRealtimeContract(ctx, RealtimeContractAuthorization{
		RequestID: "seed-cancel-" + uuid.NewString(), BuyerID: buyerID, Profile: profile,
		InputCommitment: strings.Repeat("9", 64), RequestSHA256: strings.Repeat("8", 64),
		MaximumPriceUSD: 0.001, EstimatedPriceUSD: 0.0005, DeadlineAt: time.Now().Add(time.Minute),
		MaximumPromptTokens: 8_330, MaximumCompletionTokens: 1,
		EstimatedPromptTokens: 4_163, EstimatedCompletionTokens: 1, BuyerDeclaredCeilingUSD: 0.0011,
	})
	mustf(t, err, "authorize for cancel: %v")
	ok, err := store.FinalizeRealtimeFailure(ctx, cancelled.ID, uuid.New(), 499, 1,
		"seed_cancel", "cancelled must not enter terminal_attempts", true)
	if err != nil || !ok {
		t.Fatalf("finalize cancel: ok=%v err=%v", ok, err)
	}

	agg, err := loadRealtimeSupplierOutcomeStatsFromAggregate(ctx, pool, profile.RuntimeProfileID)
	mustf(t, err, "aggregate: %v")
	tab, err := loadRealtimeSupplierOutcomeStatsFromTable(ctx, pool, profile.RuntimeProfileID)
	mustf(t, err, "table: %v")
	if diffs := diffRealtimeSupplierOutcomeStats(agg, tab); len(diffs) > 0 {
		t.Fatalf("stats drifted from fresh aggregate:\n%s", strings.Join(diffs, "\n"))
	}

	// Explicit expected shape for this fixture: 7 verified + 3 failed = 10
	// attempts, 3 fails, 7 settlements, 0 refunds. CANCELLED is excluded.
	var found bool
	for _, s := range tab {
		if s.SupplierID != worker.SupplierID {
			continue
		}
		found = true
		if s.TerminalAttempts != 10 || s.TerminalFails != 3 ||
			s.VerifiedSettlements != 7 || s.RefundCount != 0 {
			t.Fatalf("unexpected counters for seeded supplier: %+v", s)
		}
	}
	if !found {
		t.Fatal("seeded supplier missing from stats table")
	}
}

// TestRealtimeAuthorizeReadsStatsTableNotLiveAggregate proves authorize is
// wired to the stats table: a deliberately wrong counter changes the frozen
// ranking inputs even though the contract history still says otherwise. If
// this test is green after reverting authorize to the inline CTE, the change
// did not land.
func TestRealtimeAuthorizeReadsStatsTableNotLiveAggregate(t *testing.T) {
	installSettlementCurrencyForTest(t, "usd")
	ctx, store, pool := openPayoutTestStore(t)
	t.Setenv("MERC_TOKEN_KEY", "outcome-stats-wire-key-at-least-32-bytes!!")
	resetRealtimeClearingState(t, ctx, pool)

	buyerID, err := store.CreateBuyerAccount(ctx,
		"outcome-stats-wire-"+uuid.NewString()+"@example.test", "integration-password", 5)
	must(t, err)
	profile := sortedVLLMProfiles()[0]
	// Single offer, no contract history. Inject synthetic stats that would
	// only appear if authorize joins the table.
	worker := newRealtimeClearingOffer(t, ctx, store, pool, profile, "HOT", 0.08, 0.30, 4)
	const (
		injectedAttempts    = 20
		injectedFails       = 10
		injectedSettlements = 10
		injectedRefunds     = 0
	)
	if _, err := pool.Exec(ctx, `
		INSERT INTO realtime_supplier_outcome_stats
		  (runtime_profile_id, supplier_id, terminal_attempts, terminal_fails,
		   verified_settlements, refund_count)
		VALUES ($1,$2,$3,$4,$5,$6)`,
		profile.RuntimeProfileID, worker.SupplierID,
		injectedAttempts, injectedFails, injectedSettlements, injectedRefunds); err != nil {
		t.Fatal(err)
	}

	// Fresh aggregate over contracts must still be empty for this supplier.
	agg, err := loadRealtimeSupplierOutcomeStatsFromAggregate(ctx, pool, profile.RuntimeProfileID)
	must(t, err)
	for _, s := range agg {
		if s.SupplierID == worker.SupplierID && s.TerminalAttempts != 0 {
			t.Fatalf("fixture polluted: aggregate already has history %+v", s)
		}
	}

	contract := authorizeClearingContract(t, ctx, store, buyerID, profile)
	if contract.WorkerID != worker.WorkerID {
		t.Fatalf("unexpected winner %s", contract.WorkerID)
	}
	in := contract.MarketClearing.RankingInputs
	if in == nil {
		t.Fatal("missing ranking inputs")
	}
	if in.TerminalAttempts != injectedAttempts || in.TerminalFails != injectedFails {
		t.Fatalf("authorize did not read stats table: got attempts=%d fails=%d want %d/%d (aggregate is empty; if this fails after a revert to the inline CTE the wiring is gone)",
			in.TerminalAttempts, in.TerminalFails, injectedAttempts, injectedFails)
	}
	if !in.RetryCostApplied || in.ObservedFailureRate == nil ||
		math.Abs(*in.ObservedFailureRate-0.5) > 1e-9 {
		t.Fatalf("injected 50%% failure must adjust ranking: %+v", in)
	}
	// verified_outcome_cost nanos: base ask * 2 for 50% failure.
	base := in.BaseAskNanos
	if in.VerifiedOutcomeCostNanos != base*2 && in.VerifiedOutcomeCostNanos != divRoundUp(base*20, 10) {
		// divRoundUp(base * 20 / 10) == base*2 for even bases; keep both checks.
		want := divRoundUp(base*int64(injectedAttempts), int64(injectedAttempts-injectedFails))
		if in.VerifiedOutcomeCostNanos != want {
			t.Fatalf("verified cost: got %d want %d (base %d)", in.VerifiedOutcomeCostNanos, want, base)
		}
	}
}

// TestRealtimeSupplierOutcomeStatsDriftCheckFailsWhenTableWrong is the
// negative control for the drift helper: if the table is forced out of band
// from truth, diff must report it. CI uses the positive no-drift test; this
// proves the detector is not a no-op.
func TestRealtimeSupplierOutcomeStatsDriftCheckFailsWhenTableWrong(t *testing.T) {
	installSettlementCurrencyForTest(t, "usd")
	ctx, store, pool := openPayoutTestStore(t)
	t.Setenv("MERC_TOKEN_KEY", "outcome-stats-neg-key-at-least-32-bytes!!!")
	resetRealtimeClearingState(t, ctx, pool)

	buyerID, err := store.CreateBuyerAccount(ctx,
		"outcome-stats-neg-"+uuid.NewString()+"@example.test", "integration-password", 5)
	must(t, err)
	profile := sortedVLLMProfiles()[0]
	worker := newRealtimeClearingOffer(t, ctx, store, pool, profile, "WARM", 0.08, 0.30, 16)
	seedSupplierFailureRate(t, ctx, store, pool, buyerID, worker, profile, 5, 5)

	// Corrupt the table deliberately (keep CHECK terminal_fails <= terminal_attempts).
	if _, err := pool.Exec(ctx, `
		UPDATE realtime_supplier_outcome_stats
		   SET terminal_attempts = terminal_attempts + 50
		 WHERE runtime_profile_id=$1 AND supplier_id=$2`,
		profile.RuntimeProfileID, worker.SupplierID); err != nil {
		t.Fatal(err)
	}
	agg, err := loadRealtimeSupplierOutcomeStatsFromAggregate(ctx, pool, profile.RuntimeProfileID)
	must(t, err)
	tab, err := loadRealtimeSupplierOutcomeStatsFromTable(ctx, pool, profile.RuntimeProfileID)
	must(t, err)
	diffs := diffRealtimeSupplierOutcomeStats(agg, tab)
	if len(diffs) == 0 {
		t.Fatal("drift detector returned no diffs after deliberate corruption")
	}
}

// TestRealtimeReputationDoesNotEnterBuyerCharge documents the money invariant
// the staleness argument relies on: ranking inputs may change which supplier
// wins, but buyer charge is derived from the selected offer rates and metered
// tokens, not from terminal_attempts / refund_count. A stale reputation value
// therefore cannot create a money error — only a different (still correctly
// priced) winner.
func TestRealtimeReputationDoesNotEnterBuyerCharge(t *testing.T) {
	installSettlementCurrencyForTest(t, "usd")
	ctx, store, pool := openPayoutTestStore(t)
	t.Setenv("MERC_TOKEN_KEY", "outcome-stats-money-key-at-least-32-bytes!")
	resetRealtimeClearingState(t, ctx, pool)

	buyerID, err := store.CreateBuyerAccount(ctx,
		"outcome-stats-money-"+uuid.NewString()+"@example.test", "integration-password", 5)
	must(t, err)
	profile := sortedVLLMProfiles()[0]
	worker := newRealtimeClearingOffer(t, ctx, store, pool, profile, "HOT", 0.08, 0.30, 4)

	// Inflate reputation multipliers so verified-outcome cost is huge.
	if _, err := pool.Exec(ctx, `
		INSERT INTO realtime_supplier_outcome_stats
		  (runtime_profile_id, supplier_id, terminal_attempts, terminal_fails,
		   verified_settlements, refund_count)
		VALUES ($1,$2,100,50,100,20)`,
		profile.RuntimeProfileID, worker.SupplierID); err != nil {
		t.Fatal(err)
	}

	contract := authorizeClearingContract(t, ctx, store, buyerID, profile)
	if contract.Pricing == nil {
		t.Fatal("missing pricing decision")
	}
	// Pricing must use the offer rates (0.08 / 0.30), not the inflated ranking cost.
	if contract.SupplierInputUSDPerMillionTokens != 0.08 ||
		contract.SupplierOutputUSDPerMillionTokens != 0.30 {
		t.Fatalf("contract rates mutated by reputation: in=%g out=%g",
			contract.SupplierInputUSDPerMillionTokens, contract.SupplierOutputUSDPerMillionTokens)
	}
	if contract.MarketClearing == nil || contract.MarketClearing.RankingInputs == nil {
		t.Fatal("missing ranking inputs")
	}
	rankCost := contract.MarketClearing.RankingInputs.VerifiedOutcomeCostNanos
	baseAsk := contract.MarketClearing.RankingInputs.BaseAskNanos
	if rankCost <= baseAsk {
		t.Fatalf("fixture should inflate ranking cost above base ask: rank=%d base=%d", rankCost, baseAsk)
	}
	// Buyer reserve is independent of rank cost (authorizeClearingContract ceiling).
	if contract.MaximumPriceUSD != 0.001 {
		t.Fatalf("maximum price changed by reputation: %g", contract.MaximumPriceUSD)
	}

	evidence := RealtimeExecutionEvidence{
		ID: uuid.New(), HTTPStatus: 200, StreamEventCount: 1,
		StreamRootSHA256: strings.Repeat("1", 64), OutputCommitment: strings.Repeat("2", 64),
		PromptTokens: 1, CompletionTokens: 1, TotalTokens: 2,
		TimeToFirstEventMS: 1, DurationMS: 1,
	}
	settlement, err := store.FinalizeRealtimeSuccess(ctx, contract.ID, evidence)
	mustf(t, err, "settle: %v")
	// Settlement charge is metered from offer rates * tokens, not ranking cost.
	if settlement.BuyerChargeUSD <= 0 {
		t.Fatalf("buyer charge missing: %+v", settlement)
	}
	// Ranking cost is nanos-per-million with large multipliers; buyer charge is a
	// tiny metered amount. Reputation must not inflate the charge into dollars.
	if settlement.BuyerChargeUSD > 1.0 {
		t.Fatalf("buyer charge looks reputation-inflated: %g (ranking cost nanos=%d)",
			settlement.BuyerChargeUSD, rankCost)
	}
}

// TestRealtimeAuthorizeSQLUsesStatsJoin is a static guard: the production
// authorize SQL must join realtime_supplier_outcome_stats and must not embed
// the historical full-table supplier_outcomes aggregate. Reverting the SQL
// constant fails this test without needing a live database.
func TestRealtimeAuthorizeSQLUsesStatsJoin(t *testing.T) {
	for _, name := range []struct {
		label string
		sql   string
	}{
		{"blocking", realtimeAuthorizeSelectOfferSQLBlocking},
		{"skip", realtimeAuthorizeSelectOfferSQLSkip},
		{"alias", realtimeAuthorizeSelectOfferSQL},
	} {
		if !strings.Contains(name.sql, "realtime_supplier_outcome_stats") {
			t.Fatalf("%s authorize SQL lost the stats table join", name.label)
		}
		if strings.Contains(name.sql, "supplier_outcomes AS") {
			t.Fatalf("%s authorize SQL still embeds the per-request supplier_outcomes aggregate", name.label)
		}
		if strings.Contains(name.sql, "FROM execution_contracts") {
			t.Fatalf("%s authorize SQL must not scan execution_contracts for reputation", name.label)
		}
	}
	if !strings.Contains(realtimeAuthorizeSelectOfferSQLSkip, "SKIP LOCKED") {
		t.Fatal("skip-path authorize SQL must use FOR UPDATE SKIP LOCKED")
	}
	if strings.Contains(realtimeAuthorizeSelectOfferSQLBlocking, "SKIP LOCKED") {
		t.Fatal("blocking authorize SQL must not SKIP LOCKED (single-offer wait path)")
	}
	// Step 7: claim freezes the considered book with the reservation so a
	// rank>1 SKIP LOCKED win can record lock-skipped peers.
	if !strings.Contains(realtimeAuthorizeSelectOfferSQLSkip, "jsonb_agg") ||
		!strings.Contains(realtimeAuthorizeSelectOfferSQLBlocking, "jsonb_agg") {
		t.Fatal("authorize SQL must freeze the considered book with the claim")
	}
	// Legacy probe must still hold the aggregate so drift tests stay meaningful.
	if !strings.Contains(realtimeClearingProbeLegacySQL, "FROM execution_contracts") {
		t.Fatal("legacy probe lost the full aggregate; drift comparison would be vacuous")
	}
}

func TestRealtimeAuthorizeSQLReturnsSelectedOfferLastSeen(t *testing.T) {
	for _, name := range []struct {
		label string
		sql   string
	}{
		{"blocking", realtimeAuthorizeSelectOfferSQLBlocking},
		{"skip", realtimeAuthorizeSelectOfferSQLSkip},
	} {
		if !strings.Contains(name.sql, "o.last_seen_at") || !strings.Contains(name.sql, "u.last_seen_at") {
			t.Fatalf("%s authorize SQL must return the selected offer last_seen_at from the claim", name.label)
		}
	}
}

func TestDiffRealtimeSupplierOutcomeStatsEmptyWhenEqual(t *testing.T) {
	id := uuid.New()
	a := []realtimeSupplierOutcomeStats{{
		SupplierID: id, TerminalAttempts: 3, TerminalFails: 1,
		VerifiedSettlements: 2, RefundCount: 0,
	}}
	b := []realtimeSupplierOutcomeStats{{
		SupplierID: id, TerminalAttempts: 3, TerminalFails: 1,
		VerifiedSettlements: 2, RefundCount: 0,
	}}
	if diffs := diffRealtimeSupplierOutcomeStats(a, b); len(diffs) != 0 {
		t.Fatalf("equal sets reported diffs: %v", diffs)
	}
}

// Ensure pgx import used when scan hits ErrNoRows in probes under empty book.
func TestRealtimeClearingProbeNoSupply(t *testing.T) {
	ctx, _, pool := openPayoutTestStore(t)
	resetRealtimeClearingState(t, ctx, pool)
	profile := sortedVLLMProfiles()[0]
	_, err := probeRealtimeClearingWinnerStats(ctx, pool,
		profile.RuntimeProfileID, profile.ProfileSHA256,
		profile.BuyerInputUSDPerMillionTokens, profile.BuyerOutputUSDPerMillionTokens)
	if !errors.Is(err, pgx.ErrNoRows) {
		t.Fatalf("empty book: want ErrNoRows got %v", err)
	}
}

// compile-time: pool satisfies the small query interface used by probes.
var _ interface {
	QueryRow(context.Context, string, ...any) pgx.Row
} = (*pgxpool.Pool)(nil)
