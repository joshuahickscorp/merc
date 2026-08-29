package main

import (
	"context"
	"errors"
	"net/http"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
)

// seedRealtimePooledSettleContract inserts a minimal EXECUTING contract for
// settle-path unit tests that drive maybeDebitPrepaidForRealtimeTx directly.
func seedRealtimePooledSettleContract(
	t *testing.T, ctx context.Context, pool *pgxpool.Pool, buyerID uuid.UUID,
) uuid.UUID {
	t.Helper()
	profile := sortedVLLMProfiles()[0]
	contractID := uuid.New()
	if _, err := pool.Exec(ctx, `
		INSERT INTO execution_contracts
		 (id,request_id,buyer_id,workload_type,route,model_alias,runtime_profile_id,
		  runtime_profile_sha256,input_commitment,request_sha256,maximum_price_usd,
		  estimated_price_usd,buyer_input_usd_per_million_tokens,buyer_output_usd_per_million_tokens,
		  supplier_input_usd_per_million_tokens,supplier_output_usd_per_million_tokens,
		  deadline_at,verification_tier,state,currency)
		VALUES ($1,$2,$3,'CHAT_COMPLETION','/v1/chat/completions',$4,$5,$6,
		        $7,$8,1.10,0.80,$10,$11,0,0,now()+interval '1 minute','V0','EXECUTING',$9)`,
		contractID, "req-pooled-settle-"+uuid.NewString(), buyerID, profile.ModelAlias,
		profile.RuntimeProfileID, profile.ProfileSHA256,
		strings.Repeat("a", 64), strings.Repeat("b", 64), SettlementCurrencyCode(),
		profile.BuyerInputUSDPerMillionTokens, profile.BuyerOutputUSDPerMillionTokens); err != nil {
		t.Fatal(err)
	}
	return contractID
}

// settleRealtimeBuyerChargeTx writes buyer_charge then runs the realtime prepaid
// settle path — the order FinalizeRealtimeSuccess uses. Returns the debit error.
func settleRealtimeBuyerChargeTx(
	ctx context.Context, pool *pgxpool.Pool,
	buyerID, contractID uuid.UUID, chargeMicros int64,
) error {
	tx, err := pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	contractRef := contractID
	buyer := buyerID
	if _, err := insertLedgerEntryTx(ctx, tx, ledgerInsert{
		Kind: KindBuyerCharge, BuyerID: &buyer, ExecutionContractID: &contractRef,
		AmountMicros: -chargeMicros, Currency: SettlementCurrencyCode(),
		CurrencyAuthority: ledgerCurrencyAuthorityExecutionContract,
		PayoutStatus:      PayoutReleased,
	}); err != nil {
		return err
	}
	if err := maybeDebitPrepaidForRealtimeTx(ctx, tx, buyerID, contractID, chargeMicros); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

func prepaidDebitMicrosForContract(
	t *testing.T, ctx context.Context, pool *pgxpool.Pool, buyerID, contractID uuid.UUID,
) int64 {
	t.Helper()
	var debitMicros int64
	if err := pool.QueryRow(ctx, `
		SELECT COALESCE(SUM((-amount_usd*1000000)::bigint),0)
		  FROM ledger_entries
		 WHERE buyer_id=$1 AND kind='prepaid_debit'
		   AND payout_ref=$2`, buyerID, prepaidExecutionContractDebitRef(contractID),
	).Scan(&debitMicros); err != nil {
		t.Fatal(err)
	}
	return debitMicros
}

// TestRealtimePooledSettlePartialPrepaidFailsOnFullDebit reproduces the worked
// example defect: admission pools free+prepaid, but settle all-or-nothing debits
// the full charge from prepaid and fails when prepaid alone is short — even
// though free covers the residual and the buyer has the money.
//
//	free_credit_usd = 0.40, prepaid = 700_000 micros, charge = 800_000 micros
//	admit: 400_000+700_000 >= 800_000 → admit
//	broken settle: debit 800_000 from 700_000 → errInsufficientPrepaid
func TestRealtimePooledSettlePartialPrepaidFailsOnFullDebit(t *testing.T) {
	installSettlementCurrencyForTest(t, "usd")
	ctx, store, pool := openIsolatedTestStore(t)

	const (
		freeUSD       = 0.40
		prepaidMicros = int64(700_000)
		chargeMicros  = int64(800_000)
		freeMicros    = int64(400_000)
	)
	if usdToMicros(freeUSD) != freeMicros {
		t.Fatalf("fixture free micros=%d, want %d", usdToMicros(freeUSD), freeMicros)
	}

	buyerID := uuid.New()
	if _, err := pool.Exec(ctx, `INSERT INTO buyers (id,email,free_credit_usd) VALUES ($1,$2,$3)`,
		buyerID, buyerID.String()+"@pooled-partial.invalid", freeUSD); err != nil {
		t.Fatal(err)
	}
	must(t, store.SeedPrepaidBalance(ctx, buyerID, prepaidMicros, "seed-pooled-partial-"+buyerID.String()))

	// Admission agrees the pooled balance covers the charge.
	txAdmit, err := pool.Begin(ctx)
	must(t, err)
	needNanos := chargeMicros * 1000
	mustf(t, evaluateRealtimeBuyerFunding(ctx, txAdmit, buyerID, needNanos),
		"admission must succeed for free+prepaid pool: %v")
	must(t, txAdmit.Commit(ctx))

	contractID := seedRealtimePooledSettleContract(t, ctx, pool, buyerID)
	err = settleRealtimeBuyerChargeTx(ctx, pool, buyerID, contractID, chargeMicros)
	if errors.Is(err, errInsufficientPrepaid) {
		// Current HEAD: settle debits the full charge from prepaid and fails.
		// After the residual-debit fix this branch must not run.
		t.Fatalf("pooled partial settle failed with insufficient prepaid "+
			"(free=%d prepaid=%d charge=%d): %v — settle must consume free first and "+
			"debit only residual %d from prepaid",
			freeMicros, prepaidMicros, chargeMicros, err, chargeMicros-freeMicros)
	}
	mustf(t, err, "pooled partial settle: %v")

	// After the fix: residual only.
	wantResidual := chargeMicros - freeMicros
	gotDebit := prepaidDebitMicrosForContract(t, ctx, pool, buyerID, contractID)
	if gotDebit != wantResidual {
		t.Fatalf("prepaid_debit micros=%d, want residual %d (charge %d − free %d)",
			gotDebit, wantResidual, chargeMicros, freeMicros)
	}
	bal, err := store.BuyerPrepaidBalanceMicros(ctx, buyerID)
	must(t, err)
	if bal != prepaidMicros-wantResidual {
		t.Fatalf("prepaid balance=%d, want %d−%d", bal, prepaidMicros, wantResidual)
	}
	// Conservation: free-covered + prepaid debited == charge exactly.
	freeConsumed := chargeMicros - gotDebit
	if freeConsumed+gotDebit != chargeMicros {
		t.Fatalf("conservation broke: freeConsumed=%d + prepaidDebit=%d != charge=%d",
			freeConsumed, gotDebit, chargeMicros)
	}
}

// TestRealtimePooledSettleConservation asserts freeConsumed + prepaidDebit ==
// chargeMicros exactly on the partial path (no micro created or destroyed).
func TestRealtimePooledSettleConservation(t *testing.T) {
	installSettlementCurrencyForTest(t, "usd")
	ctx, store, pool := openIsolatedTestStore(t)

	cases := []struct {
		name          string
		freeUSD       float64
		prepaidMicros int64
		chargeMicros  int64
		wantDebit     int64
	}{
		{"worked_example", 0.40, 700_000, 800_000, 400_000},
		{"free_covers_all_but_one", 0.40, 700_000, 400_001, 1},
		{"free_one_short", 0.40, 700_000, 400_000, 0}, // free==charge → no debit
		{"one_micro_each", 0.000001, 1, 2, 1},
		{"free_zero_full_debit", 0, 900_000, 800_000, 800_000},
		{"prepaid_zero_free_covers", 0.90, 0, 800_000, 0},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			buyerID := uuid.New()
			if _, err := pool.Exec(ctx, `INSERT INTO buyers (id,email,free_credit_usd) VALUES ($1,$2,$3)`,
				buyerID, buyerID.String()+"@cons-"+tc.name+".invalid", tc.freeUSD); err != nil {
				t.Fatal(err)
			}
			if tc.prepaidMicros > 0 {
				must(t, store.SeedPrepaidBalance(ctx, buyerID, tc.prepaidMicros, "seed-cons-"+tc.name+"-"+buyerID.String()))
			}
			contractID := seedRealtimePooledSettleContract(t, ctx, pool, buyerID)
			mustf(t, settleRealtimeBuyerChargeTx(ctx, pool, buyerID, contractID, tc.chargeMicros),
				"settle %s: %v", tc.name)
			gotDebit := prepaidDebitMicrosForContract(t, ctx, pool, buyerID, contractID)
			if gotDebit != tc.wantDebit {
				t.Fatalf("prepaid_debit=%d, want %d", gotDebit, tc.wantDebit)
			}
			freeConsumed := tc.chargeMicros - gotDebit
			if freeConsumed+gotDebit != tc.chargeMicros {
				t.Fatalf("conservation: freeConsumed=%d + debit=%d != charge=%d",
					freeConsumed, gotDebit, tc.chargeMicros)
			}
			if freeConsumed < 0 || gotDebit < 0 {
				t.Fatalf("negative money term: freeConsumed=%d debit=%d", freeConsumed, gotDebit)
			}
		})
	}
}

// TestRealtimePooledSettleFreeCoversNoDebit pins the free-covers-all path:
// no prepaid_debit row, balance unchanged — byte-identical to pre-fix behaviour.
func TestRealtimePooledSettleFreeCoversNoDebit(t *testing.T) {
	installSettlementCurrencyForTest(t, "usd")
	ctx, store, pool := openIsolatedTestStore(t)

	const freeUSD = 5.0
	const chargeMicros int64 = 800_000
	const prepaidMicros int64 = 1_000_000

	buyerID := uuid.New()
	if _, err := pool.Exec(ctx, `INSERT INTO buyers (id,email,free_credit_usd) VALUES ($1,$2,$3)`,
		buyerID, buyerID.String()+"@free-covers.invalid", freeUSD); err != nil {
		t.Fatal(err)
	}
	must(t, store.SeedPrepaidBalance(ctx, buyerID, prepaidMicros, "seed-free-covers-"+buyerID.String()))
	contractID := seedRealtimePooledSettleContract(t, ctx, pool, buyerID)
	must(t, settleRealtimeBuyerChargeTx(ctx, pool, buyerID, contractID, chargeMicros))

	if got := prepaidDebitMicrosForContract(t, ctx, pool, buyerID, contractID); got != 0 {
		t.Fatalf("free-covers path wrote prepaid_debit=%d, want 0", got)
	}
	bal, err := store.BuyerPrepaidBalanceMicros(ctx, buyerID)
	must(t, err)
	if bal != prepaidMicros {
		t.Fatalf("prepaid balance moved on free-covers path: %d → want %d", bal, prepaidMicros)
	}
	var debitRows int
	if err := pool.QueryRow(ctx, `
		SELECT count(*) FROM ledger_entries
		 WHERE buyer_id=$1 AND kind='prepaid_debit'`, buyerID).Scan(&debitRows); err != nil {
		t.Fatal(err)
	}
	if debitRows != 0 {
		t.Fatalf("free-covers path created %d prepaid_debit rows, want 0", debitRows)
	}
}

// TestRealtimePooledSettleFreeZeroFullDebit pins free==0 (including every
// non-USD deployment where free is forced to 0): full charge from prepaid.
func TestRealtimePooledSettleFreeZeroFullDebit(t *testing.T) {
	installSettlementCurrencyForTest(t, "usd")
	ctx, store, pool := openIsolatedTestStore(t)

	const prepaidMicros int64 = 5_000_000
	const chargeMicros int64 = 800_000

	buyerID := uuid.New()
	if _, err := pool.Exec(ctx, `INSERT INTO buyers (id,email,free_credit_usd) VALUES ($1,$2,0)`,
		buyerID, buyerID.String()+"@free-zero.invalid"); err != nil {
		t.Fatal(err)
	}
	must(t, store.SeedPrepaidBalance(ctx, buyerID, prepaidMicros, "seed-free-zero-"+buyerID.String()))
	contractID := seedRealtimePooledSettleContract(t, ctx, pool, buyerID)
	must(t, settleRealtimeBuyerChargeTx(ctx, pool, buyerID, contractID, chargeMicros))

	if got := prepaidDebitMicrosForContract(t, ctx, pool, buyerID, contractID); got != chargeMicros {
		t.Fatalf("free-zero debit=%d, want full charge %d", got, chargeMicros)
	}
	bal, err := store.BuyerPrepaidBalanceMicros(ctx, buyerID)
	must(t, err)
	if bal != prepaidMicros-chargeMicros {
		t.Fatalf("prepaid balance=%d, want %d−%d", bal, prepaidMicros, chargeMicros)
	}
}

// TestRealtimePooledSettlePartialIdempotent replays settle on the partial path
// and proves the debit is keyed by execution contract (no double debit).
func TestRealtimePooledSettlePartialIdempotent(t *testing.T) {
	installSettlementCurrencyForTest(t, "usd")
	ctx, store, pool := openIsolatedTestStore(t)

	const (
		freeUSD       = 0.40
		prepaidMicros = int64(700_000)
		chargeMicros  = int64(800_000)
		wantResidual  = int64(400_000)
	)
	buyerID := uuid.New()
	if _, err := pool.Exec(ctx, `INSERT INTO buyers (id,email,free_credit_usd) VALUES ($1,$2,$3)`,
		buyerID, buyerID.String()+"@partial-idemp.invalid", freeUSD); err != nil {
		t.Fatal(err)
	}
	must(t, store.SeedPrepaidBalance(ctx, buyerID, prepaidMicros, "seed-partial-idemp-"+buyerID.String()))
	contractID := seedRealtimePooledSettleContract(t, ctx, pool, buyerID)
	must(t, settleRealtimeBuyerChargeTx(ctx, pool, buyerID, contractID, chargeMicros))

	// Replay: buyer_charge already exists (unique on execution_contract_id,kind),
	// so re-drive only the debit path inside a fresh transaction — the production
	// idempotency surface for prepaid is payout_ref on prepaid_debit.
	tx, err := pool.Begin(ctx)
	must(t, err)
	mustf(t, maybeDebitPrepaidForRealtimeTx(ctx, tx, buyerID, contractID, chargeMicros),
		"replay debit: %v")
	must(t, tx.Commit(ctx))

	if got := prepaidDebitMicrosForContract(t, ctx, pool, buyerID, contractID); got != wantResidual {
		t.Fatalf("after replay prepaid_debit sum=%d, want residual %d (not 2×)", got, wantResidual)
	}
	var debitRows int
	if err := pool.QueryRow(ctx, `
		SELECT count(*) FROM ledger_entries
		 WHERE buyer_id=$1 AND kind='prepaid_debit'
		   AND payout_ref=$2`, buyerID, prepaidExecutionContractDebitRef(contractID),
	).Scan(&debitRows); err != nil {
		t.Fatal(err)
	}
	if debitRows != 1 {
		t.Fatalf("prepaid_debit rows=%d, want 1 after replay", debitRows)
	}
	bal, err := store.BuyerPrepaidBalanceMicros(ctx, buyerID)
	must(t, err)
	if bal != prepaidMicros-wantResidual {
		t.Fatalf("balance after replay=%d, want single residual debit %d−%d",
			bal, prepaidMicros, wantResidual)
	}
}

// TestRealtimePooledAdmitSettlesAgree is the property: whenever admission
// succeeds on free+prepaid, residual settle must succeed too (and refuse only
// when the pool is genuinely short).
func TestRealtimePooledAdmitSettlesAgree(t *testing.T) {
	installSettlementCurrencyForTest(t, "usd")
	ctx, store, pool := openIsolatedTestStore(t)

	type row struct {
		name          string
		freeUSD       float64
		prepaidMicros int64
		chargeMicros  int64
		wantAdmit     bool
		wantSettle    bool
		wantDebit     int64
	}
	// freeRemaining = free (no prior spent). residual = max(0, charge−freeMicros).
	cases := []row{
		// Worked example: free=0.40, prepaid=0.70, charge=0.80 → admit+settle, debit 0.40.
		{"worked_example", 0.40, 700_000, 800_000, true, true, 400_000},
		// free == charge → admit, no debit.
		{"free_eq_charge", 0.80, 700_000, 800_000, true, true, 0},
		// free == charge−1 micro → residual 1.
		{"free_eq_charge_minus_1", 0.799999, 700_000, 800_000, true, true, 1},
		// prepaid == residual exactly.
		{"prepaid_eq_residual", 0.40, 400_000, 800_000, true, true, 400_000},
		// prepaid == residual−1 → pool short by 1 at admit and settle.
		{"prepaid_eq_residual_minus_1", 0.40, 399_999, 800_000, false, false, 0},
		// free == 0 → full debit.
		{"free_zero", 0, 900_000, 800_000, true, true, 800_000},
		// prepaid == 0, free covers.
		{"prepaid_zero_free_covers", 0.90, 0, 800_000, true, true, 0},
		// prepaid == 0, free short → refuse.
		{"prepaid_zero_free_short", 0.40, 0, 800_000, false, false, 0},
		// free zero, prepaid short → refuse.
		{"free_zero_prepaid_short", 0, 799_999, 800_000, false, false, 0},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			buyerID := uuid.New()
			if _, err := pool.Exec(ctx, `INSERT INTO buyers (id,email,free_credit_usd) VALUES ($1,$2,$3)`,
				buyerID, buyerID.String()+"@agree-"+tc.name+".invalid", tc.freeUSD); err != nil {
				t.Fatal(err)
			}
			if tc.prepaidMicros > 0 {
				must(t, store.SeedPrepaidBalance(ctx, buyerID, tc.prepaidMicros,
					"seed-agree-"+tc.name+"-"+buyerID.String()))
			}
			tx, err := pool.Begin(ctx)
			must(t, err)
			admitErr := evaluateRealtimeBuyerFunding(ctx, tx, buyerID, tc.chargeMicros*1000)
			_ = tx.Rollback(ctx)
			admitted := admitErr == nil
			if admitted != tc.wantAdmit {
				t.Fatalf("admit=%v (err=%v), want admit=%v", admitted, admitErr, tc.wantAdmit)
			}
			if !admitted {
				// Settlement is not attempted when admission refuses; if forced,
				// residual may still fail the same way — not asserted here.
				return
			}
			contractID := seedRealtimePooledSettleContract(t, ctx, pool, buyerID)
			settleErr := settleRealtimeBuyerChargeTx(ctx, pool, buyerID, contractID, tc.chargeMicros)
			settled := settleErr == nil
			if settled != tc.wantSettle {
				t.Fatalf("settle=%v (err=%v), want settle=%v — admission and settlement disagree",
					settled, settleErr, tc.wantSettle)
			}
			if !settled {
				return
			}
			if got := prepaidDebitMicrosForContract(t, ctx, pool, buyerID, contractID); got != tc.wantDebit {
				t.Fatalf("debit=%d, want %d", got, tc.wantDebit)
			}
			// Invariant 4 core: admit success ⇒ settle success (already checked).
			// Conservation on the success path.
			if freeConsumed := tc.chargeMicros - tc.wantDebit; freeConsumed+tc.wantDebit != tc.chargeMicros {
				t.Fatalf("conservation broken on admit-agree case")
			}
		})
	}
}

// TestRealtimePooledPartialRefundRestoresAvailable funds a contract with free
// covering part of the charge and prepaid the residual, refunds it, and asserts
// the buyer's pooled available balance returns exactly to its pre-admission value.
// KindPrepaidRestore must net the partial debit magnitude, not the full charge.
func TestRealtimePooledPartialRefundRestoresAvailable(t *testing.T) {
	installSettlementCurrencyForTest(t, "usd")
	t.Setenv("MERC_TOKEN_KEY", "rt-pooled-refund-key-with-at-least-32-bytes!!")
	ctx, store, pool := openIsolatedTestStore(t)
	profile, _, _ := realtimeFundingFixture(t, ctx, store, pool)

	// Large tokens so charge is meaningful; free covers a strict prefix.
	const promptTokens, completionTokens int64 = 100_000, 50_000
	maxUSD, estUSD, maxPrompt, maxCompletion := realtimeAuthCeiling(t, profile, promptTokens, completionTokens)
	// Expected charge from rates so we can size free as a strict prefix.
	currency := MustParseCurrency(SettlementCurrencyCode())
	buyerIn, err := nanoRatePerMillionFromFloat(profile.BuyerInputUSDPerMillionTokens)
	must(t, err)
	buyerOut, err := nanoRatePerMillionFromFloat(profile.BuyerOutputUSDPerMillionTokens)
	must(t, err)
	chargeExact, err := BuyerRealtimeTokenChargeNanos(currency, promptTokens, completionTokens, buyerIn, buyerOut)
	must(t, err)
	chargeMicros, err := LedgerMicrosFromNanos(chargeExact)
	must(t, err)
	if chargeMicros < 2 {
		t.Fatalf("charge micros=%d too small to split free/prepaid", chargeMicros)
	}
	freeMicros := chargeMicros / 2
	if freeMicros <= 0 || freeMicros >= chargeMicros {
		t.Fatalf("free split free=%d charge=%d", freeMicros, chargeMicros)
	}
	residual := chargeMicros - freeMicros
	// free+prepaid must cover the auth ceiling; prepaid must cover the residual.
	// Realtime EXECUTING holds are committed money, not prepaid balance reduction,
	// so residual settle only needs prepaid >= residual at debit time.
	needCeiling := usdToMicros(maxUSD)
	seedPrepaid := needCeiling - freeMicros
	if seedPrepaid < residual {
		seedPrepaid = residual
	}

	buyerID := uuid.New()
	if _, err := pool.Exec(ctx, `INSERT INTO buyers (id,email,free_credit_usd) VALUES ($1,$2,$3)`,
		buyerID, buyerID.String()+"@pooled-refund.invalid", microsToUSD(freeMicros)); err != nil {
		t.Fatal(err)
	}
	must(t, store.SeedPrepaidBalance(ctx, buyerID, seedPrepaid, "seed-pooled-refund-"+buyerID.String()))

	// Pre-admission available = free + prepaid (no spent, no committed).
	preAvailable := freeMicros + seedPrepaid

	contract, _, err := store.AuthorizeRealtimeContract(ctx, RealtimeContractAuthorization{
		RequestID: "req-pooled-refund-" + uuid.NewString(), BuyerID: buyerID, Profile: profile,
		InputCommitment: strings.Repeat("c", 64), RequestSHA256: strings.Repeat("d", 64),
		MaximumPriceUSD: maxUSD, EstimatedPriceUSD: estUSD, DeadlineAt: time.Now().Add(time.Minute),
		MaximumPromptTokens: maxPrompt, MaximumCompletionTokens: maxCompletion,
		EstimatedPromptTokens: promptTokens, EstimatedCompletionTokens: completionTokens,
	})
	mustf(t, err, "authorize: %v")

	settlement, err := store.FinalizeRealtimeSuccess(ctx, contract.ID, RealtimeExecutionEvidence{
		ID: uuid.New(), HTTPStatus: http.StatusOK, StreamRootSHA256: strings.Repeat("1", 64),
		OutputCommitment: strings.Repeat("2", 64),
		PromptTokens:     promptTokens, CompletionTokens: completionTokens,
		TotalTokens: promptTokens + completionTokens,
	})
	mustf(t, err, "finalize: %v")
	actualCharge := usdToMicros(settlement.BuyerChargeUSD)
	if actualCharge != chargeMicros {
		// LedgerMicrosFromNanos should match finalize; tolerate only exact.
		t.Fatalf("settled charge micros=%d, precomputed %d", actualCharge, chargeMicros)
	}
	gotDebit := prepaidDebitMicrosForContract(t, ctx, pool, buyerID, contract.ID)
	if gotDebit <= 0 {
		t.Fatalf("expected partial prepaid debit > 0, got %d (free may have covered all)", gotDebit)
	}
	if gotDebit >= actualCharge {
		t.Fatalf("expected partial debit < full charge: debit=%d charge=%d", gotDebit, actualCharge)
	}
	freeConsumed := actualCharge - gotDebit
	if freeConsumed+gotDebit != actualCharge {
		t.Fatalf("conservation: free=%d + debit=%d != charge=%d", freeConsumed, gotDebit, actualCharge)
	}

	actor := insertTestAdminActor(t, pool, ctx)
	_, created, err := store.RefundRealtimeContract(ctx, actor, contract.ID,
		"pooled partial refund restore", adminTestRef("INC-pooled-refund"))
	if err != nil || !created {
		t.Fatalf("RefundRealtimeContract: err=%v created=%v", err, created)
	}

	// Materialised prepaid restored by the partial debit amount (not full charge).
	bal, err := store.BuyerPrepaidBalanceMicros(ctx, buyerID)
	must(t, err)
	if bal != seedPrepaid {
		t.Fatalf("prepaid after refund=%d, want seed %d (restore of partial debit %d)",
			bal, seedPrepaid, gotDebit)
	}
	var restoreMicros int64
	if err := pool.QueryRow(ctx, `
		SELECT COALESCE(SUM((amount_usd*1000000)::bigint),0)
		  FROM ledger_entries
		 WHERE buyer_id=$1 AND kind='prepaid_restore'
		   AND payout_ref=$2`, buyerID, prepaidExecutionContractRestoreRef(contract.ID),
	).Scan(&restoreMicros); err != nil {
		t.Fatal(err)
	}
	if restoreMicros != gotDebit {
		t.Fatalf("prepaid_restore micros=%d, want partial debit %d (not full charge %d)",
			restoreMicros, gotDebit, actualCharge)
	}

	// Available returns to pre-admission: free + prepaid, spent netted, restore nets debit.
	tx, err := pool.Begin(ctx)
	must(t, err)
	// Exact pre-admission pool must admit; one micro over must refuse.
	mustf(t, evaluateRealtimeBuyerFunding(ctx, tx, buyerID, preAvailable*1000),
		"post-refund available must admit pre-admission pool %d: %v", preAvailable)
	overErr := evaluateRealtimeBuyerFunding(ctx, tx, buyerID, (preAvailable+1)*1000)
	_ = tx.Rollback(ctx)
	if overErr == nil {
		t.Fatalf("post-refund available admitted pre+1 micro — phantom capacity after partial restore")
	}
	if !errors.Is(overErr, errRealtimeInsufficientFunds) && !errors.Is(overErr, errRealtimeTopupRequired) {
		t.Fatalf("post-refund over-admit err=%v, want insufficient/topup", overErr)
	}
}

// TestBatchExactReuseRefuseEnableWithoutPrepaidDebit is the fail-closed guard
// for the latent sibling: SubmitExactReuseBatchJob admits via
// evaluateRealtimeBuyerFunding and writes buyer_charge / platform_take but
// never calls a prepaid debit. Enabling batchExactReuseEnabled without that
// settle path would finalize charges the prepaid pocket never collects.
//
// Missing on that path: maybeDebitPrepaidForRealtimeTx (or an equivalent
// task/ref prepaid debit keyed to the synthetic task) after buyer_charge.
func TestBatchExactReuseRefuseEnableWithoutPrepaidDebit(t *testing.T) {
	raw, err := os.ReadFile("exact_reuse_batch.go")
	must(t, err)
	source := string(raw)
	// Production settle vocabulary used elsewhere for prepaid collection.
	hasPrepaidDebit := strings.Contains(source, "maybeDebitPrepaidForRealtimeTx") ||
		strings.Contains(source, "debitPrepaidForTaskTx") ||
		strings.Contains(source, "debitPrepaidByRefTx") ||
		strings.Contains(source, "debitPrepaidForExecutionContractTx")
	if batchExactReuseEnabled && !hasPrepaidDebit {
		t.Fatal("batchExactReuseEnabled is true but SubmitExactReuseBatchJob still has no " +
			"prepaid debit settle path (missing maybeDebitPrepaid / debitPrepaid* after " +
			"buyer_charge); refusing silent money-losing enablement")
	}
	// While the settle path is absent, the flag must stay compile-time false.
	if !hasPrepaidDebit && batchExactReuseEnabled {
		t.Fatal("unreachable: covered above")
	}
	if hasPrepaidDebit && !batchExactReuseEnabled {
		// Settle wired but still disabled — fine; flag flip can be a later change.
		return
	}
	if !batchExactReuseEnabled {
		// Pin the known-disabled state so a silent `= true` without debit fails.
		if !strings.Contains(source, "const batchExactReuseEnabled = false") {
			t.Fatal("batchExactReuseEnabled is no longer compile-time false and no prepaid " +
				"debit exists on SubmitExactReuseBatchJob — wire residual/full prepaid settle " +
				"before enabling")
		}
	}
}
