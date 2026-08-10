package main

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"
)

// G069 — prepaid refund must hold pure non-envelope EXECUTING ceilings the
// same way admission does. Admission refuses when
// balance - reserved - executingHold < need; refund historically computed
// available := balance - reserved and could drain cash settlement still needs.
//
// Funding source is not determinable at refund time from the contract's own
// spend rows: while EXECUTING there is no buyer_charge / prepaid_debit, and
// free-vs-prepaid is chosen only at settle. G070 re-checked this and left the
// conservative hold in place. These tests pin that behaviour: hold the full
// non-envelope EXECUTING ceiling against prepaid available.

// g069SeedAboveCeiling funds a prepaid balance B that covers one realtime
// ceiling C with material slack, rounded up to whole settlement cents so
// top-up and refund rails accept the amount. free credit stays 0; no card.
// Large token bounds make C multi-cent so B-C is a positive refundable amount
// after the hold (tiny 7/2 ceilings are sub-cent and only exercise refuse).
func g069SeedAboveCeiling(t *testing.T, profile VLLMRuntimeProfile) (
	needMicros, seedMicros int64,
	maxUSD, estUSD float64, maxPrompt, maxCompletion int64,
) {
	t.Helper()
	maxUSD, estUSD, maxPrompt, maxCompletion = realtimeAuthCeiling(t, profile, 100_000, 50_000)
	needMicros = usdToMicros(maxUSD)
	if needMicros <= 0 {
		t.Fatalf("realtime ceiling micros=%d (maxUSD=%f)", needMicros, maxUSD)
	}
	// Slack so a correct refund of B-C is still a positive minor-unit amount
	// when the hold is applied, and so a buggy refund of full B is observable.
	seedMicros = needMicros + needMicros/2
	if seedMicros%10_000 != 0 {
		seedMicros = ((seedMicros / 10_000) + 1) * 10_000
	}
	if seedMicros <= needMicros {
		seedMicros = ((needMicros / 10_000) + 2) * 10_000
	}
	// Ensure whole-cent slack remains after subtracting the ceiling.
	if (seedMicros-needMicros)/10_000 < 1 {
		seedMicros = needMicros + 20_000
		if seedMicros%10_000 != 0 {
			seedMicros = ((seedMicros / 10_000) + 1) * 10_000
		}
	}
	return needMicros, seedMicros, maxUSD, estUSD, maxPrompt, maxCompletion
}

// TestPrepaidRefundHoldsNonEnvelopeExecutingCeiling is sequential case 1:
// prepaid B, free credit 0, authorize without envelope for C <= B, then
// BeginPrepaidRefund must not return more than B-C. Today it returns up to B.
func TestPrepaidRefundHoldsNonEnvelopeExecutingCeiling(t *testing.T) {
	installSettlementCurrencyForTest(t, "usd")
	t.Setenv("MERC_TOKEN_KEY", "prepaid-refund-exec-hold-key-with-at-least-32-bytes!")
	ctx, store, pool := openIsolatedTestStore(t)
	profile, _, _ := realtimeFundingFixture(t, ctx, store, pool)

	needMicros, seedMicros, maxUSD, estUSD, maxPrompt, maxCompletion := g069SeedAboveCeiling(t, profile)
	buyerID := uuid.New()
	if _, err := pool.Exec(ctx, `INSERT INTO buyers (id,email,free_credit_usd) VALUES ($1,$2,0)`,
		buyerID, buyerID.String()+"@g069-seq.invalid"); err != nil {
		t.Fatal(err)
	}
	fundPrepaidViaTopup(t, ctx, store, buyerID, seedMicros/10_000)

	contract, replay, err := store.AuthorizeRealtimeContract(ctx, RealtimeContractAuthorization{
		RequestID: "req-g069-seq-" + uuid.NewString(), BuyerID: buyerID, Profile: profile,
		InputCommitment: strings.Repeat("a", 64), RequestSHA256: strings.Repeat("b", 64),
		MaximumPriceUSD: maxUSD, EstimatedPriceUSD: estUSD,
		DeadlineAt:              time.Now().Add(time.Minute),
		IdempotencyKey:          "g069-seq-" + uuid.NewString(),
		MaximumPromptTokens:     maxPrompt,
		MaximumCompletionTokens: maxCompletion,
		EstimatedPromptTokens:   maxPrompt / 2, EstimatedCompletionTokens: maxCompletion / 2,
		// No EnvelopeID — pure non-envelope EXECUTING.
	})
	if err != nil || replay {
		t.Fatalf("authorize: err=%v replay=%v", err, replay)
	}
	if contract.State != "EXECUTING" {
		t.Fatalf("contract state=%s, want EXECUTING", contract.State)
	}
	// Sanity: no envelope spend row, so the hold is the pure EXECUTING sibling.
	var spendRows int
	if err := pool.QueryRow(ctx, `
		SELECT count(*) FROM execution_envelope_spends WHERE contract_id=$1`, contract.ID).
		Scan(&spendRows); err != nil {
		t.Fatal(err)
	}
	if spendRows != 0 {
		t.Fatalf("expected no envelope spend for pure realtime, got %d", spendRows)
	}

	actor := insertTestAdminActor(t, pool, ctx)
	plan, err := store.BeginPrepaidRefund(ctx, actor, buyerID,
		"refund while pure non-envelope EXECUTING is open",
		adminTestRef("INC-g069-seq"))
	bal, berr := store.BuyerPrepaidBalanceMicros(ctx, buyerID)
	if berr != nil {
		t.Fatal(berr)
	}

	maxRefundableMicros := seedMicros - needMicros
	if maxRefundableMicros < 0 {
		maxRefundableMicros = 0
	}
	// Balance after any refund must still cover the admitted ceiling.
	if bal < needMicros {
		t.Fatalf("refund drained in-flight prepaid: balance=%d need ceiling=%d seed=%d plan_cents=%d err=%v — "+
			"BeginPrepaidRefund ignored non-envelope EXECUTING hold (available used balance-reserved only)",
			bal, needMicros, seedMicros, plan.Cents, err)
	}
	if err != nil {
		// Refusal or sub-cent dust is fine if the ceiling cash stayed.
		if !errors.Is(err, errInsufficientPrepaid) &&
			!strings.Contains(err.Error(), "below one") {
			t.Fatalf("BeginPrepaidRefund unexpected error: %v", err)
		}
		return
	}
	settlement, serr := SettlementCurrency()
	if serr != nil {
		t.Fatal(serr)
	}
	microsPerMinor, merr := settlement.MicrosPerMinorUnit()
	if merr != nil {
		t.Fatal(merr)
	}
	refundedMicros := plan.Cents * microsPerMinor
	if refundedMicros > maxRefundableMicros {
		t.Fatalf("refund returned %d micros (cents=%d), want <= B-C=%d (B=%d C=%d) — "+
			"BeginPrepaidRefund treated pure EXECUTING as unreserved prepaid cash",
			refundedMicros, plan.Cents, maxRefundableMicros, seedMicros, needMicros)
	}
}

// TestPrepaidRefundLeavesSettlementDebitable is sequential case 2: after a
// hold-aware refund of true slack, FinalizeRealtimeSuccess must still debit.
// Today refund drains B and settle fails with errInsufficientPrepaid.
func TestPrepaidRefundLeavesSettlementDebitable(t *testing.T) {
	installSettlementCurrencyForTest(t, "usd")
	t.Setenv("MERC_TOKEN_KEY", "prepaid-refund-settle-key-with-at-least-32-bytes!!")
	ctx, store, pool := openIsolatedTestStore(t)
	profile, _, _ := realtimeFundingFixture(t, ctx, store, pool)

	needMicros, seedMicros, maxUSD, estUSD, maxPrompt, maxCompletion := g069SeedAboveCeiling(t, profile)
	buyerID := uuid.New()
	if _, err := pool.Exec(ctx, `INSERT INTO buyers (id,email,free_credit_usd) VALUES ($1,$2,0)`,
		buyerID, buyerID.String()+"@g069-settle.invalid"); err != nil {
		t.Fatal(err)
	}
	fundPrepaidViaTopup(t, ctx, store, buyerID, seedMicros/10_000)

	contract, replay, err := store.AuthorizeRealtimeContract(ctx, RealtimeContractAuthorization{
		RequestID: "req-g069-settle-" + uuid.NewString(), BuyerID: buyerID, Profile: profile,
		InputCommitment: strings.Repeat("c", 64), RequestSHA256: strings.Repeat("d", 64),
		MaximumPriceUSD: maxUSD, EstimatedPriceUSD: estUSD,
		DeadlineAt:              time.Now().Add(time.Minute),
		IdempotencyKey:          "g069-settle-" + uuid.NewString(),
		MaximumPromptTokens:     maxPrompt,
		MaximumCompletionTokens: maxCompletion,
		EstimatedPromptTokens:   maxPrompt / 2, EstimatedCompletionTokens: maxCompletion / 2,
	})
	if err != nil || replay {
		t.Fatalf("authorize: err=%v replay=%v", err, replay)
	}

	actor := insertTestAdminActor(t, pool, ctx)
	_, refundErr := store.BeginPrepaidRefund(ctx, actor, buyerID,
		"refund slack while EXECUTING; settlement must still debit",
		adminTestRef("INC-g069-settle"))
	// Refund may refuse (no whole-cent slack) or take only B-C; either way
	// settle must succeed. A successful full-B refund is the bug under test.
	bal, berr := store.BuyerPrepaidBalanceMicros(ctx, buyerID)
	if berr != nil {
		t.Fatal(berr)
	}
	if bal < needMicros {
		t.Fatalf("precondition for settle broken: balance=%d < ceiling=%d after refund err=%v — "+
			"refund returned cash settlement still needs", bal, needMicros, refundErr)
	}

	// Settle a small usage within the frozen token bounds — charge << ceiling
	// so a hold-aware refund of B-C still leaves enough to debit.
	promptTok, completionTok := int64(7), int64(2)
	settlement, err := store.FinalizeRealtimeSuccess(ctx, contract.ID, RealtimeExecutionEvidence{
		ID: uuid.New(), HTTPStatus: http.StatusOK, StreamRootSHA256: strings.Repeat("1", 64),
		OutputCommitment: strings.Repeat("2", 64),
		PromptTokens:     promptTok, CompletionTokens: completionTok, TotalTokens: promptTok + completionTok,
	})
	if err != nil {
		t.Fatalf("FinalizeRealtimeSuccess after refund: %v (balance=%d ceiling=%d refund_err=%v) — "+
			"want debit success; today fails with errInsufficientPrepaid after delivery",
			err, bal, needMicros, refundErr)
	}
	if settlement.BuyerChargeUSD <= 0 || settlement.BuyerChargeUSD > maxUSD {
		t.Fatalf("unexpected buyer charge: %+v max=%f", settlement, maxUSD)
	}
	chargeMicros := usdToMicros(settlement.BuyerChargeUSD)
	// Settled amount is the physical token charge — hold fix must not change it.
	balAfter, err := store.BuyerPrepaidBalanceMicros(ctx, buyerID)
	must(t, err)
	// balance_after = bal_before_settle - charge (free credit is 0).
	if balAfter != bal-chargeMicros {
		t.Fatalf("settled prepaid balance=%d, want %d (pre-settle %d - charge %d); settled amount moved?",
			balAfter, bal-chargeMicros, bal, chargeMicros)
	}
}

// TestPrepaidRefundRealtimeAuthorizeContestedCash races refund against
// realtime authorize for one buyer: exactly one may consume the contested
// cash (a single ceiling-sized pool with no slack).
func TestPrepaidRefundRealtimeAuthorizeContestedCash(t *testing.T) {
	installSettlementCurrencyForTest(t, "usd")
	t.Setenv("MERC_TOKEN_KEY", "prepaid-refund-race-key-with-at-least-32-bytes!!!!")
	ctx, store, pool := openIsolatedTestStore(t)
	profile, _, _ := realtimeFundingFixture(t, ctx, store, pool)

	maxUSD, estUSD, maxPrompt, maxCompletion := realtimeAuthCeiling(t, profile, 100_000, 50_000)
	needMicros := usdToMicros(maxUSD)
	// Fund exactly one ceiling, rounded up to a whole cent so refund can take
	// the full balance when it wins the race (no residual hold required).
	// Contested window: seed covers one ceiling, not two.
	seedMicros := needMicros
	if seedMicros%10_000 != 0 {
		seedMicros = ((seedMicros / 10_000) + 1) * 10_000
	}
	if seedMicros < needMicros {
		t.Fatalf("seed %d < need %d", seedMicros, needMicros)
	}

	buyerID := uuid.New()
	if _, err := pool.Exec(ctx, `INSERT INTO buyers (id,email,free_credit_usd) VALUES ($1,$2,0)`,
		buyerID, buyerID.String()+"@g069-race.invalid"); err != nil {
		t.Fatal(err)
	}
	fundPrepaidViaTopup(t, ctx, store, buyerID, seedMicros/10_000)
	actor := insertTestAdminActor(t, pool, ctx)

	var (
		wg         sync.WaitGroup
		start      = make(chan struct{})
		authOK     atomic.Bool
		refundOK   atomic.Bool
		authErr    atomic.Value
		refundErr  atomic.Value
		refundCent atomic.Int64
	)
	wg.Add(2)
	go func() {
		defer wg.Done()
		<-start
		_, _, err := store.AuthorizeRealtimeContract(context.Background(), RealtimeContractAuthorization{
			RequestID: "req-g069-race-" + uuid.NewString(), BuyerID: buyerID, Profile: profile,
			InputCommitment: strings.Repeat("e", 64), RequestSHA256: strings.Repeat("f", 64),
			MaximumPriceUSD: maxUSD, EstimatedPriceUSD: estUSD,
			DeadlineAt:              time.Now().Add(time.Minute),
			IdempotencyKey:          "g069-race-" + uuid.NewString(),
			MaximumPromptTokens:     maxPrompt,
			MaximumCompletionTokens: maxCompletion,
			EstimatedPromptTokens:   maxPrompt / 2, EstimatedCompletionTokens: maxCompletion / 2,
		})
		if err == nil {
			authOK.Store(true)
			return
		}
		authErr.Store(err)
	}()
	go func() {
		defer wg.Done()
		<-start
		plan, err := store.BeginPrepaidRefund(context.Background(), actor, buyerID,
			"race refund against realtime authorize",
			adminTestRef("INC-g069-race"))
		if err == nil {
			refundOK.Store(true)
			refundCent.Store(plan.Cents)
			return
		}
		refundErr.Store(err)
	}()
	close(start)
	wg.Wait()

	bal, err := store.BuyerPrepaidBalanceMicros(ctx, buyerID)
	must(t, err)
	var executing int
	if err := pool.QueryRow(ctx, `
		SELECT count(*) FROM execution_contracts
		 WHERE buyer_id=$1 AND state='EXECUTING'`, buyerID).Scan(&executing); err != nil {
		t.Fatal(err)
	}

	// Contested cash is the single ceiling. Exactly one consumer may take it:
	// either EXECUTING is open (authorize won) or a full refund emptied the
	// pool (refund won). Both succeeding over-subscribes.
	if authOK.Load() && refundOK.Load() {
		// Partial refund of true post-auth slack is only possible when
		// seedMicros > needMicros by at least one cent. With seed ≈ need, a
		// successful refund of any whole-cent amount while EXECUTING means
		// refund ignored the hold.
		if executing > 0 && bal < needMicros {
			t.Fatalf("both rails consumed contested cash: EXECUTING=%d balance=%d need=%d "+
				"refund_cents=%d — shared advisory or EXECUTING hold missing on refund",
				executing, bal, needMicros, refundCent.Load())
		}
		if executing > 0 {
			// Refund of residual dust below the ceiling is OK; refund of
			// more than seed-need is not.
			maxSlack := seedMicros - needMicros
			if maxSlack < 0 {
				maxSlack = 0
			}
			if refundCent.Load()*10_000 > maxSlack {
				t.Fatalf("refund+authorize both took contested cash: refund_cents=%d "+
					"max_slack_micros=%d balance=%d need=%d",
					refundCent.Load(), maxSlack, bal, needMicros)
			}
		}
	}
	if authOK.Load() && executing > 0 && bal < needMicros {
		t.Fatalf("authorize won but refund drained ceiling cash: balance=%d need=%d", bal, needMicros)
	}
	if !authOK.Load() && !refundOK.Load() {
		// Both refusing is surprising with seed covering one ceiling, but
		// sub-cent seed quirks can refuse refund; authorize should still
		// have a path. Surface both errors.
		t.Fatalf("neither rail consumed cash: auth_err=%v refund_err=%v balance=%d seed=%d need=%d",
			authErr.Load(), refundErr.Load(), bal, seedMicros, needMicros)
	}
}

// TestPrepaidRefundConservativeWhenExecutingFundedByFreeCredit pins case 4 /
// G070 P2. Funding source is not knowable from spend rows while EXECUTING
// (none exist yet; free-first is deferred to settle). Refund therefore keeps
// the full non-envelope EXECUTING ceiling hold against prepaid even when free
// credit alone covered admission — conservative over-hold, not a silent
// cash-out. If a funding-source column or settle-time spend row were present
// on EXECUTING, the exact pocket would be preferred; they are not.
func TestPrepaidRefundConservativeWhenExecutingFundedByFreeCredit(t *testing.T) {
	installSettlementCurrencyForTest(t, "usd")
	t.Setenv("MERC_TOKEN_KEY", "prepaid-refund-fc-key-with-at-least-32-bytes!!!!!!")
	ctx, store, pool := openIsolatedTestStore(t)
	profile, _, _ := realtimeFundingFixture(t, ctx, store, pool)

	needMicros, _, maxUSD, estUSD, maxPrompt, maxCompletion := g069SeedAboveCeiling(t, profile)
	// Free credit alone covers the ceiling; prepaid is a separate pocket.
	prepaidMicros := int64(2_000_000) // $2, whole cents
	if prepaidMicros%10_000 != 0 {
		t.Fatalf("prepaid seed must be whole cents, got %d", prepaidMicros)
	}
	// free_credit_usd is float major units; grant well above the ceiling.
	freeCreditUSD := microsToUSD(needMicros) + 1.0

	buyerID := uuid.New()
	if _, err := pool.Exec(ctx, `INSERT INTO buyers (id,email,free_credit_usd) VALUES ($1,$2,$3)`,
		buyerID, buyerID.String()+"@g069-fc.invalid", freeCreditUSD); err != nil {
		t.Fatal(err)
	}
	fundPrepaidViaTopup(t, ctx, store, buyerID, prepaidMicros/10_000)

	contract, replay, err := store.AuthorizeRealtimeContract(ctx, RealtimeContractAuthorization{
		RequestID: "req-g069-fc-" + uuid.NewString(), BuyerID: buyerID, Profile: profile,
		InputCommitment: strings.Repeat("a", 64), RequestSHA256: strings.Repeat("b", 64),
		MaximumPriceUSD: maxUSD, EstimatedPriceUSD: estUSD,
		DeadlineAt:              time.Now().Add(time.Minute),
		IdempotencyKey:          "g069-fc-" + uuid.NewString(),
		MaximumPromptTokens:     maxPrompt,
		MaximumCompletionTokens: maxCompletion,
		EstimatedPromptTokens:   maxPrompt / 2, EstimatedCompletionTokens: maxCompletion / 2,
	})
	if err != nil || replay {
		t.Fatalf("authorize via free credit: err=%v replay=%v", err, replay)
	}
	if contract.State != "EXECUTING" {
		t.Fatalf("state=%s, want EXECUTING", contract.State)
	}
	// Prepaid balance must be untouched at authorize (no debit until settle).
	bal, err := store.BuyerPrepaidBalanceMicros(ctx, buyerID)
	must(t, err)
	if bal != prepaidMicros {
		t.Fatalf("prepaid balance after free-credit-funded auth=%d, want untouched %d", bal, prepaidMicros)
	}

	actor := insertTestAdminActor(t, pool, ctx)
	plan, refundErr := store.BeginPrepaidRefund(ctx, actor, buyerID,
		"conservative hold: free-credit EXECUTING still binds prepaid available",
		adminTestRef("INC-g069-fc"))
	balAfter, berr := store.BuyerPrepaidBalanceMicros(ctx, buyerID)
	if berr != nil {
		t.Fatal(berr)
	}

	// Conservative: available = prepaid - executingHold. When C >= prepaid,
	// refund must refuse (or refund 0). When C < prepaid, refund <= prepaid-C.
	// With prepaid=$2 and typical ceilings of a few cents, C may be < or >.
	// Either way balance after must keep max(0, min(prepaid, need)) for settle
	// if free credit were exhausted — pin that refund never returns more than
	// max(0, prepaid - need).
	maxRefundable := prepaidMicros - needMicros
	if maxRefundable < 0 {
		maxRefundable = 0
	}
	if refundErr != nil {
		if balAfter != prepaidMicros {
			t.Fatalf("refused refund still moved balance: %d -> %d err=%v", prepaidMicros, balAfter, refundErr)
		}
		if !errors.Is(refundErr, errInsufficientPrepaid) &&
			!strings.Contains(refundErr.Error(), "below one") {
			t.Fatalf("unexpected refund error under conservative hold: %v", refundErr)
		}
		return
	}
	refunded := plan.Cents * 10_000
	if refunded > maxRefundable {
		t.Fatalf("conservative free-credit case: refunded %d micros > max(0,prepaid-C)=%d "+
			"(prepaid=%d C=%d) — refund must hold indistinguishable EXECUTING against prepaid",
			refunded, maxRefundable, prepaidMicros, needMicros)
	}
	if balAfter < prepaidMicros-maxRefundable {
		t.Fatalf("balance after conservative refund=%d, want >= %d", balAfter, prepaidMicros-maxRefundable)
	}
}
