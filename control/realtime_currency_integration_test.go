package main

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
)

func TestRealtimeCADAuthorizationAndSettlementConvertFrozenUSDAuthority(t *testing.T) {
	installSettlementCurrencyForTest(t, "cad")
	installRealtimeCADFXForTest(t)
	t.Setenv("MERC_TOKEN_KEY", "realtime-currency-integration-key-with-at-least-32-bytes")
	ctx, store, pool := openPayoutTestStore(t)
	if _, err := pool.Exec(ctx, `TRUNCATE
		realtime_admission_events, realtime_offer_samples,
		realtime_authorization_events, realtime_settlements, realtime_executions,
		realtime_refunds, execution_contracts, realtime_worker_offers,
		realtime_supplier_outcome_stats
		RESTART IDENTITY CASCADE`); err != nil {
		t.Fatalf("reset realtime currency fixture: %v", err)
	}
	if _, err := pool.Exec(ctx, `DELETE FROM ledger_entries WHERE execution_contract_id IS NOT NULL`); err != nil {
		t.Fatalf("reset realtime currency ledger: %v", err)
	}

	buyerID, err := store.CreateBuyerAccount(ctx,
		"realtime-currency-"+uuid.NewString()+"@example.test", "integration-password", 5)
	must(t, err)
	// free_credit_usd is deliberately unavailable to a CAD contract. Fund the
	// accepted settlement currency explicitly so this test exercises FX rather
	// than the cross-currency sandbox-credit refusal.
	must(t, store.SeedPrepaidBalance(ctx, buyerID, 10_000_000,
		"realtime-cad-funding-"+buyerID.String()))
	supplierID, workerID := uuid.New(), uuid.New()
	if _, err := pool.Exec(ctx, `INSERT INTO suppliers (id,email,status) VALUES ($1,$2,'active')`,
		supplierID, "realtime-currency-supplier-"+uuid.NewString()+"@example.test"); err != nil {
		t.Fatal(err)
	}
	if _, err := store.CreateWorkerToken(ctx, workerID, supplierID); err != nil {
		t.Fatal(err)
	}
	profile := sortedVLLMProfiles()[0]
	must(t, store.UpsertRealtimeOffer(ctx, WorkerAuth{WorkerID: workerID, SupplierID: supplierID},
		RealtimeOfferRegistration{
			RuntimeProfileID: profile.RuntimeProfileID, RuntimeProfileSHA256: profile.ProfileSHA256,
			HWClass: "nvidia_24gb", GPUCount: 1, MemoryGBPerGPU: 24,
			UpstreamBaseURL: "http://127.0.0.1:8899/v1",
			UpstreamToken:   "cx_vllm_realtime_currency_token_123456789",
			Warmth:          "HOT", MaxActiveSequences: 1, AvailableSequences: 1,
			SupplierInputUSDPerMillionTokens:  0.08,
			SupplierOutputUSDPerMillionTokens: 0.30,
		}))

	const maximumPrompt, maximumCompletion int64 = 100, 64
	const estimatedPrompt, estimatedCompletion int64 = 12, 64
	maximumUSD, err := tokenCharge(maximumPrompt, maximumCompletion,
		profile.BuyerInputUSDPerMillionTokens, profile.BuyerOutputUSDPerMillionTokens)
	must(t, err)
	estimatedUSD, err := tokenCharge(estimatedPrompt, estimatedCompletion,
		profile.BuyerInputUSDPerMillionTokens, profile.BuyerOutputUSDPerMillionTokens)
	must(t, err)
	fx, err := loadRealtimeFXAuthority(MustParseCurrency("cad"))
	must(t, err)
	referenceExpected, referenceMaximum, _, settlementMaximum, err := realtimeBuyerPriceBounds(
		profile, maximumPrompt, maximumCompletion, estimatedPrompt, estimatedCompletion,
		MustParseCurrency("cad"), fx)
	must(t, err)
	declaredReferenceCeiling := referenceMaximum.Nanos + 10_000

	physicalAuth := RealtimeContractAuthorization{
		RequestID: "req-realtime-currency-" + uuid.NewString(), BuyerID: buyerID, Profile: profile, FX: fx,
		InputCommitment: strings.Repeat("a", 64), RequestSHA256: strings.Repeat("b", 64),
		MaximumPriceUSD: maximumUSD, EstimatedPriceUSD: estimatedUSD,
		MaximumPriceUSDNanos: referenceMaximum.Nanos, EstimatedPriceUSDNanos: referenceExpected.Nanos,
		MaximumPromptTokens: maximumPrompt, MaximumCompletionTokens: maximumCompletion,
		EstimatedPromptTokens: estimatedPrompt, EstimatedCompletionTokens: estimatedCompletion,
		BuyerDeclaredCeilingUSD:      float64(declaredReferenceCeiling) / float64(NanosPerMajorUnit),
		BuyerDeclaredCeilingUSDNanos: declaredReferenceCeiling,
		DeadlineAt:                   time.Now().Add(time.Minute), IdempotencyKey: "physical-frozen-fx-" + uuid.NewString(),
	}
	contract, replay, err := store.AuthorizeRealtimeContract(ctx, physicalAuth)
	if err != nil || replay {
		t.Fatalf("authorize converted CAD realtime contract: replay=%v err=%v", replay, err)
	}
	wantMaximumCAD, err := projectRealtimeNanosToMajor(settlementMaximum)
	must(t, err)
	if contract.Currency != "cad" || contract.MaximumPriceUSD != wantMaximumCAD ||
		contract.MaximumPriceUSD == maximumUSD || contract.Pricing == nil || contract.Pricing.Realtime == nil ||
		contract.Pricing.Realtime.ReferenceCurrency != "usd" ||
		contract.Pricing.Realtime.SettlementCurrency != "cad" ||
		contract.Pricing.Realtime.FX.FXRevision != fx.FXRevision {
		t.Fatalf("authorization relabeled rather than converted USD/CAD authority: contract=%+v", contract)
	}
	headerMaximumUSD, err := realtimeContractMaximumReferenceUSD(contract)
	if err != nil || headerMaximumUSD != maximumUSD {
		t.Fatalf("X-Merc-Max-USD projection moved into CAD: got=%v err=%v want=%v",
			headerMaximumUSD, err, maximumUSD)
	}

	reuseMoney, err := SettleRealtimeReuseHitMoneyWithFX(
		MustParseCurrency("cad"), fx, 7,
		profile.BuyerInputUSDPerMillionTokens, profile.BuyerOutputUSDPerMillionTokens)
	must(t, err)
	reuseAuth := RealtimeContractAuthorization{
		RequestID: "req-realtime-currency-reuse-" + uuid.NewString(), BuyerID: buyerID,
		Profile: profile, FX: fx, InputCommitment: strings.Repeat("e", 64),
		RequestSHA256: strings.Repeat("f", 64), BuyerDeclaredCeilingUSD: 1,
		BuyerDeclaredCeilingUSDNanos: NanosPerMajorUnit,
		ReuseClass:                   ClassExactResultReuse, DeadlineAt: time.Now().Add(time.Minute),
		IdempotencyKey: "reuse-frozen-fx-" + uuid.NewString(),
	}
	reuseHit := ExactCacheHit{ResultRef: "cas/sha256/realtime-currency-reuse", OutputTokens: 7}
	const reuseOutputCommitment = "1111111111111111111111111111111111111111111111111111111111111111"
	reuseContract, reuseSettlement, err := store.SettleRealtimeExactReuse(
		ctx,
		reuseAuth,
		reuseHit,
		reuseMoney,
		reuseOutputCommitment,
	)
	must(t, err)
	reuseHeaderUSD, err := realtimeContractMaximumReferenceUSD(reuseContract)
	must(t, err)
	if reuseContract.Pricing == nil || reuseContract.Pricing.RealtimeReuse == nil ||
		reuseContract.Pricing.RealtimeReuse.ReferenceBuyerChargeNanos == reuseSettlement.BuyerChargeNanos ||
		reuseSettlement.Currency != "cad" || reuseSettlement.BuyerChargeNanos != reuseMoney.BuyerDebitNanos ||
		reuseHeaderUSD == reuseContract.MaximumPriceUSD {
		t.Fatalf("realtime reuse relabeled USD as CAD: contract=%+v settlement=%+v headerUSD=%v",
			reuseContract, reuseSettlement, reuseHeaderUSD)
	}

	// Current FX moves after acceptance. Settlement and read replay must remain
	// on the frozen 1.37 authority, never today's 1.99 environment.
	t.Setenv(priceFXRateEnv, "1.99")
	t.Setenv(priceFXRevisionEnv, "later-fx-must-not-reprice-realtime")
	replayedPhysical, replayed, err := store.AuthorizeRealtimeContract(ctx, physicalAuth)
	if err != nil || !replayed || replayedPhysical.ID != contract.ID {
		t.Fatalf("physical idempotent replay consulted current FX: replay=%v contract=%s/%s err=%v",
			replayed, contract.ID, replayedPhysical.ID, err)
	}
	replayedReuse, replayedReuseSettlement, err := store.SettleRealtimeExactReuse(
		ctx, reuseAuth, reuseHit, reuseMoney, reuseOutputCommitment)
	if err != nil || replayedReuse.ID != reuseContract.ID || replayedReuseSettlement.ID != reuseSettlement.ID {
		t.Fatalf("reuse idempotent replay consulted current FX: contract=%s/%s settlement=%s/%s err=%v",
			reuseContract.ID, replayedReuse.ID, reuseSettlement.ID, replayedReuseSettlement.ID, err)
	}
	settlement, err := store.FinalizeRealtimeSuccess(ctx, contract.ID, RealtimeExecutionEvidence{
		ID: uuid.New(), HTTPStatus: 200, StreamRootSHA256: strings.Repeat("c", 64),
		OutputCommitment: strings.Repeat("d", 64), PromptTokens: estimatedPrompt,
		CompletionTokens: estimatedCompletion, TotalTokens: estimatedPrompt + estimatedCompletion,
	})
	must(t, err)
	buyerReference, err := realtimeReferenceTokenCharge(
		estimatedPrompt, estimatedCompletion,
		NanoMajorPerMillionTokens(contract.Pricing.Realtime.BuyerInputReferenceNanosPerMillion),
		NanoMajorPerMillionTokens(contract.Pricing.Realtime.BuyerOutputReferenceNanosPerMillion), false)
	must(t, err)
	supplierReference, err := realtimeReferenceTokenCharge(
		estimatedPrompt, estimatedCompletion,
		NanoMajorPerMillionTokens(contract.Pricing.Realtime.SupplierInputReferenceNanosPerMillion),
		NanoMajorPerMillionTokens(contract.Pricing.Realtime.SupplierOutputReferenceNanosPerMillion), true)
	must(t, err)
	wantBuyer, err := convertRealtimeReferenceNanos(buyerReference.Nanos, MustParseCurrency("cad"), fx, true)
	must(t, err)
	wantSupplier, err := convertRealtimeReferenceNanos(supplierReference.Nanos, MustParseCurrency("cad"), fx, true)
	must(t, err)
	if settlement.Currency != "cad" || settlement.BuyerChargeNanos != wantBuyer.Nanos ||
		settlement.SupplierPayableNanos != wantSupplier.Nanos ||
		settlement.KnownCostContributionNanos != wantBuyer.Nanos-wantSupplier.Nanos {
		t.Fatalf("settlement did not use frozen USD→CAD authority: got=%+v want buyer=%d supplier=%d",
			settlement, wantBuyer.Nanos, wantSupplier.Nanos)
	}
	if contract.MarketClearing == nil || contract.MarketClearing.Version != 3 ||
		contract.MarketClearing.SupplierRateCurrency != "usd" ||
		contract.MarketClearing.BuyerMoneyCurrency != "cad" ||
		contract.MarketClearing.RankingInputs == nil ||
		contract.MarketClearing.RankingInputs.RateCurrency != "usd" {
		t.Fatalf("market receipt did not map mixed money fields explicitly: %+v", contract.MarketClearing)
	}
	receipt, err := store.RealtimeReceipt(ctx, buyerID, contract.ID)
	must(t, err)
	if receipt.Version != 2 || receipt.SettlementCurrency != "cad" ||
		receipt.BuyerChargeAmount <= 0 || receipt.SupplierPayableAmount <= 0 ||
		receipt.BuyerChargeUSD != 0 || receipt.SupplierPayableUSD != 0 {
		t.Fatalf("CAD receipt relabeled settlement amounts as USD: %+v", receipt)
	}
	receiptJSON, err := json.Marshal(receipt)
	must(t, err)
	for _, staleUSDField := range []string{
		`"authorized_usd"`, `"captured_usd"`, `"buyer_charge_usd"`,
		`"supplier_payable_usd"`, `"platform_gross_spread_usd"`,
	} {
		if strings.Contains(string(receiptJSON), staleUSDField) {
			t.Fatalf("CAD public receipt emitted mislabeled field %s: %s", staleUSDField, receiptJSON)
		}
	}
	var sawCAD bool
	snapshot, err := store.RealtimeOperationalSnapshot(ctx)
	must(t, err)
	if snapshot.OpenSupplierPayableUSD != 0 || snapshot.ReversalRequiredUSD != 0 || snapshot.InternalRefundsUSD != 0 {
		t.Fatalf("legacy USD metrics mixed CAD liabilities: %+v", snapshot)
	}
	for _, money := range snapshot.MoneyByCurrency {
		if money.Currency == "cad" {
			sawCAD = true
			if money.OpenSupplierPayable <= 0 {
				t.Fatalf("CAD supplier liability missing from currency bucket: %+v", snapshot)
			}
		}
	}
	if !sawCAD {
		t.Fatalf("CAD operational currency bucket missing: %+v", snapshot)
	}

	// Direct SQL cannot attach either a ledger fact or settlement denominated
	// in USD to a CAD contract. Use a fresh accepted contract so uniqueness and
	// append-only constraints do not mask the currency fence.
	installRealtimeCADFXForTest(t)
	probeAuth := physicalAuth
	probeAuth.RequestID = "req-realtime-currency-probe-" + uuid.NewString()
	probeAuth.IdempotencyKey = "probe-frozen-fx-" + uuid.NewString()
	probeAuth.InputCommitment = strings.Repeat("7", 64)
	probeAuth.RequestSHA256 = strings.Repeat("8", 64)
	probe, replay, err := store.AuthorizeRealtimeContract(ctx, probeAuth)
	if err != nil || replay {
		t.Fatalf("authorize currency-fence probe: replay=%v err=%v", replay, err)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO ledger_entries
		(kind,execution_contract_id,amount_usd,currency,payout_status,payout_ref)
		VALUES ('platform_take',$1,0.000001,'usd','released',$2)`,
		probe.ID, "wrong-currency-ledger-"+probe.ID.String()); err == nil {
		t.Fatal("database accepted USD ledger fact for CAD execution contract")
	}
	probeExecution := uuid.New()
	if _, err := pool.Exec(ctx, `INSERT INTO realtime_executions
		(id,contract_id,worker_id,supplier_id,http_status,stream_event_count,
		 stream_root_sha256,output_commitment,prompt_tokens,completion_tokens,total_tokens,
		 time_to_first_event_ms,duration_ms,verification_state)
		VALUES ($1,$2,$3,$4,200,1,$5,$6,1,1,2,1,1,'PASSED')`,
		probeExecution, probe.ID, probe.WorkerID, probe.SupplierID,
		strings.Repeat("9", 64), strings.Repeat("a", 64)); err != nil {
		t.Fatalf("seed currency-fence execution: %v", err)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO realtime_settlements
		(id,contract_id,authoritative_execution_id,receipt_id,buyer_charge_usd,
		 supplier_gross_usd,platform_margin_usd,verification_cost_usd,
		 currency,buyer_charge_nanos,supplier_gross_nanos,known_cost_contribution_nanos)
		VALUES ($1,$2,$3,$4,0.000001,0,0.000001,0,'usd',1000,0,1000)`,
		uuid.New(), probe.ID, probeExecution, "rcp_"+probeExecution.String()); err == nil {
		t.Fatal("database accepted USD settlement for CAD execution contract")
	}
	loaded, err := scanRealtimeContract(pool.QueryRow(ctx,
		`SELECT `+realtimeContractColumns+` FROM execution_contracts WHERE id=$1`, contract.ID))
	must(t, err)
	if loaded.Pricing == nil || loaded.PricingDecisionSHA256 != contract.PricingDecisionSHA256 ||
		loaded.Pricing.Realtime.FX.FXRevision != fx.FXRevision {
		t.Fatalf("historical replay used current FX instead of frozen bytes: %+v", loaded)
	}
	loadedReuse, err := scanRealtimeContract(pool.QueryRow(ctx,
		`SELECT `+realtimeContractColumns+` FROM execution_contracts WHERE id=$1`, reuseContract.ID))
	must(t, err)
	if loadedReuse.Pricing == nil || loadedReuse.Pricing.RealtimeReuse == nil ||
		loadedReuse.Pricing.RealtimeReuse.FX.FXRevision != fx.FXRevision {
		t.Fatalf("historical reuse replay used current FX instead of frozen bytes: %+v", loadedReuse)
	}
}
